package api

import (
	"context"
	"database/sql"
	"log"
	"regexp"
	"strings"
)

// Hot-table guard for CampaignBuilder's self-healing schema (ensureSchema at
// boot, and the create path's retry after an insert error). Every statement
// takes ACCESS EXCLUSIVE on mailing_campaigns, and a QUEUED request blocks
// every later reader and writer on the send path: 2026-09-11 a no-op DROP
// CONSTRAINT waited ~20s behind a 570s CPM read with ~95 sessions queued
// behind it. So: probe the catalog, run only what is missing, and run each
// statement under SET LOCAL lock_timeout so a busy table means "retry next
// boot", never a barricade.

var campaignStatusValues = []string{"draft", "scheduled", "preparing", "finalizing_audience", "sending", "paused",
	"completed", "completed_with_errors", "cancelled", "failed", "deleted", "sent"}

const (
	campaignStatusCheck    = "mailing_campaigns_status_check"
	campaignDDLLockTimeout = "2s"
)

var (
	reCampaignDropConstraint = regexp.MustCompile(`(?i)DROP CONSTRAINT IF EXISTS\s+(\w+)`)
	reCampaignAddColumn      = regexp.MustCompile(`(?i)ADD COLUMN IF NOT EXISTS\s+(\w+)`)
)

func campaignStatusCheckDDL() string {
	return `ALTER TABLE mailing_campaigns ADD CONSTRAINT ` + campaignStatusCheck + ` CHECK (status IN ('` +
		strings.Join(campaignStatusValues, "','") + `'))`
}

// statusCheckCovers: the existing CHECK already admits every status the app
// writes. A superset is fine — replacing it with a narrower list could fail.
func statusCheckCovers(def string) bool {
	for _, v := range campaignStatusValues {
		if !strings.Contains(def, "'"+v+"'") {
			return false
		}
	}
	return true
}

// campaignSchemaPlan returns only the DDL whose effect is missing. cols holds
// existing column names (lower case); cons maps existing constraint names to
// their definitions. Pure.
func campaignSchemaPlan(drops, adds []string, cols map[string]bool, cons map[string]string) []string {
	var out []string
	for _, ddl := range drops {
		m := reCampaignDropConstraint.FindStringSubmatch(ddl)
		if m == nil {
			out = append(out, ddl)
			continue
		}
		if m[1] == campaignStatusCheck {
			continue // replaced below only when it no longer covers the statuses
		}
		if _, ok := cons[m[1]]; ok {
			out = append(out, ddl)
		}
	}
	for _, ddl := range adds {
		if m := reCampaignAddColumn.FindStringSubmatch(ddl); m == nil || !cols[strings.ToLower(m[1])] {
			out = append(out, ddl)
		}
	}
	// The old code re-added the status CHECK on every call; prod has had none
	// (verified 2026-09-11), so each boot paid an ACCESS EXCLUSIVE + full-table
	// validation that failed. Absent stays absent — adding one now would start
	// rejecting statuses prod accepts. Only a present-but-narrow CHECK is widened.
	if def, has := cons[campaignStatusCheck]; has && !statusCheckCovers(def) {
		out = append(out, `ALTER TABLE mailing_campaigns DROP CONSTRAINT IF EXISTS `+campaignStatusCheck, campaignStatusCheckDDL())
	}
	return out
}

func probeCampaignSchema(ctx context.Context, db *sql.DB) (map[string]bool, map[string]string, error) {
	cols := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT column_name FROM information_schema.columns WHERE table_name = 'mailing_campaigns'`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return nil, nil, err
		}
		cols[strings.ToLower(c)] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	cons := map[string]string{}
	rows, err = db.QueryContext(ctx, `SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = to_regclass('mailing_campaigns')`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			return nil, nil, err
		}
		cons[name] = def
	}
	return cols, cons, rows.Err()
}

func execCampaignDDL(ctx context.Context, db *sql.DB, ddl string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '`+campaignDDLLockTimeout+`'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		return err
	}
	return tx.Commit()
}

// applyCampaignSchema probes, plans and applies. A failed probe falls back to
// every statement — still one at a time under the lock timeout.
func applyCampaignSchema(ctx context.Context, db *sql.DB, drops, adds []string) {
	var plan []string
	cols, cons, err := probeCampaignSchema(ctx, db)
	if err != nil {
		log.Printf("[CampaignBuilder] schema probe failed (%v) — applying all under lock_timeout", err)
		plan = append(append(append(plan, drops...), adds...), campaignStatusCheckDDL())
	} else {
		plan = campaignSchemaPlan(drops, adds, cols, cons)
	}
	failed := 0
	for _, ddl := range plan {
		if err := execCampaignDDL(ctx, db, ddl); err != nil {
			failed++
			log.Printf("[CampaignBuilder] schema: %s: %v (retried next boot)", safePrefix(ddl, 70), err)
		}
	}
	if len(plan) > 0 {
		log.Printf("[CampaignBuilder] schema: %d statement(s) needed, %d failed", len(plan), failed)
	}
}
