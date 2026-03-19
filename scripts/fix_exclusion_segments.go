// +build ignore

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://ignite:ignite_dev_password@127.0.0.1:5432/ignite?sslmode=disable"
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("DB connect: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		log.Fatalf("DB ping: %v", err)
	}

	ctx := context.Background()

	orgID := os.Getenv("ORG_ID")
	if orgID == "" {
		db.QueryRowContext(ctx, "SELECT id::text FROM organizations LIMIT 1").Scan(&orgID)
	}
	log.Printf("Org: %s", orgID)

	// Step 1: Delete old per-brand exclusion segments
	oldNames := []string{
		"HT Hard Bounced", "HT Complained", "HT 60D Inactive",
		"MH Hard Bounced", "MH Complained", "MH 60D Inactive",
	}
	for _, name := range oldNames {
		res, err := db.ExecContext(ctx,
			"DELETE FROM mailing_segments WHERE organization_id = $1 AND name = $2", orgID, name)
		if err != nil {
			log.Printf("WARN: delete %s: %v", name, err)
		} else {
			n, _ := res.RowsAffected()
			log.Printf("Deleted segment %q (%d rows)", name, n)
		}
	}

	// Step 2: Create 8 new domain-scoped exclusion segments (4 per brand)
	type segDef struct {
		Name        string
		Description string
		Domain      string
		Conditions  interface{}
	}

	brands := []struct {
		Prefix string
		Domain string
	}{
		{"HT", "em.historythinking.com"},
		{"MH", "em.myownhealth.net"},
	}

	for _, brand := range brands {
		segs := []segDef{
			{
				Name:        fmt.Sprintf("%s Sent 7D No Open", brand.Prefix),
				Description: fmt.Sprintf("Received email from %s in last 7 days but never opened", brand.Domain),
				Conditions: cg("AND", false,
					[]cond{
						{Type: "event", Field: "sent", Op: "event_in_last_days", Val: "7", Domain: brand.Domain},
						{Type: "event", Field: "opened", Op: "event_not_in_last_days", Val: "7", Domain: brand.Domain},
					}, nil),
			},
			{
				Name:        fmt.Sprintf("%s Sent 14D No Open", brand.Prefix),
				Description: fmt.Sprintf("Received email from %s in last 14 days but never opened", brand.Domain),
				Conditions: cg("AND", false,
					[]cond{
						{Type: "event", Field: "sent", Op: "event_in_last_days", Val: "14", Domain: brand.Domain},
						{Type: "event", Field: "opened", Op: "event_not_in_last_days", Val: "14", Domain: brand.Domain},
					}, nil),
			},
			{
				Name:        fmt.Sprintf("%s Sent 30D No Open", brand.Prefix),
				Description: fmt.Sprintf("Received email from %s in last 30 days but never opened", brand.Domain),
				Conditions: cg("AND", false,
					[]cond{
						{Type: "event", Field: "sent", Op: "event_in_last_days", Val: "30", Domain: brand.Domain},
						{Type: "event", Field: "opened", Op: "event_not_in_last_days", Val: "30", Domain: brand.Domain},
					}, nil),
			},
			{
				Name:        fmt.Sprintf("%s Sent 60D No Engagement", brand.Prefix),
				Description: fmt.Sprintf("Received email from %s in last 60 days, no opens or clicks", brand.Domain),
				Conditions: cg("AND", false,
					[]cond{
						{Type: "event", Field: "sent", Op: "event_in_last_days", Val: "60", Domain: brand.Domain},
						{Type: "event", Field: "opened", Op: "event_not_in_last_days", Val: "60", Domain: brand.Domain},
						{Type: "event", Field: "clicked", Op: "event_not_in_last_days", Val: "60", Domain: brand.Domain},
					}, nil),
			},
		}

		for _, s := range segs {
			condJSON, _ := json.Marshal(s.Conditions)
			segID, err := ensureSeg(ctx, db, orgID, s.Name, s.Description, string(condJSON))
			if err != nil {
				log.Printf("ERROR creating %s: %v", s.Name, err)
				continue
			}
			log.Printf("Segment: %s -> %s", s.Name, segID)
		}
	}

	// Step 3: Sync bounced/complained subscribers to global suppressions
	log.Println("Syncing bounced subscribers to global suppressions...")
	syncResult, err := db.ExecContext(ctx, `
		INSERT INTO mailing_global_suppressions (id, organization_id, email, md5_hash, reason, source, created_at)
		SELECT gen_random_uuid(), s.organization_id, s.email, MD5(LOWER(TRIM(s.email))), 'hard_bounce', 'status_sync', NOW()
		FROM mailing_subscribers s
		WHERE s.status = 'bounced'
		AND NOT EXISTS (
			SELECT 1 FROM mailing_global_suppressions g
			WHERE g.organization_id = s.organization_id AND g.md5_hash = MD5(LOWER(TRIM(s.email)))
		)
		ON CONFLICT (organization_id, md5_hash) DO NOTHING`)
	if err != nil {
		log.Printf("WARN: sync bounced: %v", err)
	} else {
		n, _ := syncResult.RowsAffected()
		log.Printf("Synced %d bounced subscribers to global suppressions", n)
	}

	log.Println("Syncing complained subscribers to global suppressions...")
	syncResult, err = db.ExecContext(ctx, `
		INSERT INTO mailing_global_suppressions (id, organization_id, email, md5_hash, reason, source, created_at)
		SELECT gen_random_uuid(), s.organization_id, s.email, MD5(LOWER(TRIM(s.email))), 'complaint', 'status_sync', NOW()
		FROM mailing_subscribers s
		WHERE s.status = 'complained'
		AND NOT EXISTS (
			SELECT 1 FROM mailing_global_suppressions g
			WHERE g.organization_id = s.organization_id AND g.md5_hash = MD5(LOWER(TRIM(s.email)))
		)
		ON CONFLICT (organization_id, md5_hash) DO NOTHING`)
	if err != nil {
		log.Printf("WARN: sync complained: %v", err)
	} else {
		n, _ := syncResult.RowsAffected()
		log.Printf("Synced %d complained subscribers to global suppressions", n)
	}

	// Step 4: Sync bot clicker emails to global suppressions
	// Bot clickers are identified by opens within 2 seconds of send
	log.Println("Syncing bot clicker emails to global suppressions...")
	syncResult, err = db.ExecContext(ctx, `
		INSERT INTO mailing_global_suppressions (id, organization_id, email, md5_hash, reason, source, created_at)
		SELECT DISTINCT gen_random_uuid(), s.organization_id, s.email, MD5(LOWER(TRIM(s.email))), 'bot_clicker', 'status_sync', NOW()
		FROM mailing_subscribers s
		WHERE s.status = 'confirmed'
		AND EXISTS (
			SELECT 1 FROM mailing_tracking_events e
			WHERE e.subscriber_id = s.id
			AND e.event_type = 'opened'
			AND e.is_machine_open = TRUE
		)
		AND NOT EXISTS (
			SELECT 1 FROM mailing_tracking_events e2
			WHERE e2.subscriber_id = s.id
			AND e2.event_type = 'opened'
			AND (e2.is_machine_open = FALSE OR e2.is_machine_open IS NULL)
		)
		AND NOT EXISTS (
			SELECT 1 FROM mailing_global_suppressions g
			WHERE g.organization_id = s.organization_id AND g.md5_hash = MD5(LOWER(TRIM(s.email)))
		)
		ON CONFLICT (organization_id, md5_hash) DO NOTHING`)
	if err != nil {
		log.Printf("WARN: sync bot clickers: %v", err)
	} else {
		n, _ := syncResult.RowsAffected()
		log.Printf("Synced %d bot clicker emails to global suppressions", n)
	}

	log.Println("Done.")
}

type cond struct {
	Type, Field, Op, Val, Domain string
}

func cg(logic string, negated bool, conds []cond, subGroups []interface{}) map[string]interface{} {
	condList := make([]map[string]interface{}, 0, len(conds))
	for _, c := range conds {
		m := map[string]interface{}{
			"id":             uuid.New().String(),
			"condition_type": c.Type,
			"field":          c.Field,
			"operator":       c.Op,
		}
		if c.Val != "" {
			m["value"] = c.Val
		}
		if c.Domain != "" {
			m["event_sending_domain"] = c.Domain
		}
		condList = append(condList, m)
	}
	groups := make([]interface{}, 0)
	if subGroups != nil {
		groups = subGroups
	}
	return map[string]interface{}{
		"id":             uuid.New().String(),
		"logic_operator": logic,
		"is_negated":     negated,
		"conditions":     condList,
		"groups":         groups,
	}
}

func ensureSeg(ctx context.Context, db *sql.DB, orgID, name, desc, condJSON string) (uuid.UUID, error) {
	var segID uuid.UUID
	err := db.QueryRowContext(ctx,
		"SELECT id FROM mailing_segments WHERE organization_id = $1 AND name = $2", orgID, name).Scan(&segID)
	if err == sql.ErrNoRows {
		segID = uuid.New()
		_, err = db.ExecContext(ctx,
			`INSERT INTO mailing_segments
			 (id, organization_id, name, description, segment_type, conditions, subscriber_count, status, calculation_mode, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, 'dynamic', $5::jsonb, 0, 'active', 'batch', NOW(), NOW())`,
			segID, orgID, name, desc, condJSON)
		if err != nil {
			return uuid.Nil, err
		}
	} else if err != nil {
		return uuid.Nil, err
	} else {
		// Update existing with new conditions
		_, err = db.ExecContext(ctx,
			"UPDATE mailing_segments SET conditions = $1::jsonb, description = $2, updated_at = NOW() WHERE id = $3",
			condJSON, desc, segID)
		if err != nil {
			return uuid.Nil, err
		}
	}
	return segID, nil
}
