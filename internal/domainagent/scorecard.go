// Package domainagent implements the Domain Agent: a per-sending-domain
// daily delivery scorecard (rolled up from mailing_message_log +
// mailing_tracking_events), a deterministic posture/briefing generator, and
// a plan store that carries a briefing through slot editing, payload
// compilation, and in-process deploy via PMTACampaignService.DeployFromInput.
package domainagent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// scorecardCell accumulates one (sending_domain, isp) row for a single day.
type scorecardCell struct {
	sends         int
	delivered     int
	humanOpens    int
	machineOpens  int
	humanClicks   int
	machineClicks int
	hardBounces   int
	softBounces   int
	otherBounces  int
	complaints    int
	unsubscribes  int
}

type cellKey struct {
	domain string
	isp    string
}

// utcDate truncates t to a UTC calendar date.
func utcDate(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// RollupDay rebuilds mailing_domain_agent_scorecard for a single (org, day).
// Recipient-domain → ISP normalization happens in Go via the canonical
// isp.GroupFromDomain classifier (same one the audience planner uses), so
// raw domains sharing an ISP family merge into one row.
func RollupDay(ctx context.Context, db *sql.DB, orgID string, day time.Time) error {
	dayStart := utcDate(day)
	dayEnd := dayStart.AddDate(0, 0, 1)

	cells := map[cellKey]*scorecardCell{}
	cell := func(sendingDomain, recipientDomain string) *scorecardCell {
		k := cellKey{
			domain: strings.ToLower(strings.TrimSpace(sendingDomain)),
			isp:    isp.GroupFromDomain(recipientDomain),
		}
		c, ok := cells[k]
		if !ok {
			c = &scorecardCell{}
			cells[k] = c
		}
		return c
	}

	// One tx for the whole rollup: SET LOCAL only binds to a transaction, and
	// the day-slice aggregates need more than the pool's default
	// statement_timeout (30s in prod — the first backfill died on it).
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("scorecard begin tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '300s'`); err != nil {
		return fmt.Errorf("scorecard set timeout: %w", err)
	}

	// (a) sends + delivered from the message log, joined to the sending
	// profile for the sending domain.
	sendRows, err := tx.QueryContext(ctx, `
		SELECT sp.sending_domain,
		       LOWER(SPLIT_PART(m.email, '@', 2)) AS rdom,
		       COUNT(*),
		       COUNT(*) FILTER (WHERE m.status = 'delivered' OR m.delivered_at IS NOT NULL)
		FROM mailing_message_log m
		JOIN mailing_sending_profiles sp ON sp.id = m.sending_profile_id
		WHERE m.organization_id = $1 AND m.sent_at >= $2 AND m.sent_at < $3
		GROUP BY 1, 2
	`, orgID, dayStart, dayEnd)
	if err != nil {
		return fmt.Errorf("scorecard sends query: %w", err)
	}
	for sendRows.Next() {
		var sendingDomain, rdom string
		var sends, delivered int
		if err := sendRows.Scan(&sendingDomain, &rdom, &sends, &delivered); err != nil {
			sendRows.Close()
			return fmt.Errorf("scorecard sends scan: %w", err)
		}
		c := cell(sendingDomain, rdom)
		c.sends += sends
		c.delivered += delivered
	}
	if err := sendRows.Err(); err != nil {
		sendRows.Close()
		return fmt.Errorf("scorecard sends rows: %w", err)
	}
	sendRows.Close()

	// (b) engagement + negative events from the tracking table.
	evtRows, err := tx.QueryContext(ctx, `
		SELECT sending_domain, COALESCE(recipient_domain, ''), event_type,
		       COALESCE(is_machine_open, FALSE), COALESCE(is_machine_click, FALSE),
		       COALESCE(bounce_type, ''), COUNT(*)
		FROM mailing_tracking_events
		WHERE organization_id = $1 AND event_at >= $2 AND event_at < $3
		  AND sending_domain <> ''
		GROUP BY 1, 2, 3, 4, 5, 6
	`, orgID, dayStart, dayEnd)
	if err != nil {
		return fmt.Errorf("scorecard events query: %w", err)
	}
	for evtRows.Next() {
		var sendingDomain, recipientDomain, eventType, bounceType string
		var machineOpen, machineClick bool
		var n int
		if err := evtRows.Scan(&sendingDomain, &recipientDomain, &eventType, &machineOpen, &machineClick, &bounceType, &n); err != nil {
			evtRows.Close()
			return fmt.Errorf("scorecard events scan: %w", err)
		}
		c := cell(sendingDomain, recipientDomain)
		switch eventType {
		case "opened":
			if machineOpen {
				c.machineOpens += n
			} else {
				c.humanOpens += n
			}
		case "clicked":
			if machineClick {
				c.machineClicks += n
			} else {
				c.humanClicks += n
			}
		case "bounced":
			switch bounceType {
			case "hard":
				c.hardBounces += n
			case "soft":
				c.softBounces += n
			default:
				c.otherBounces += n
			}
		case "complained":
			c.complaints += n
		case "unsubscribed":
			c.unsubscribes += n
		}
	}
	if err := evtRows.Err(); err != nil {
		evtRows.Close()
		return fmt.Errorf("scorecard events rows: %w", err)
	}
	evtRows.Close()

	// Rebuild the day atomically: DELETE + INSERT, still inside the same tx.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM mailing_domain_agent_scorecard
		WHERE organization_id = $1 AND day = $2
	`, orgID, dayStart); err != nil {
		return fmt.Errorf("scorecard delete day: %w", err)
	}
	for k, c := range cells {
		if k.domain == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mailing_domain_agent_scorecard
				(organization_id, day, sending_domain, isp,
				 sends, delivered, human_opens, machine_opens,
				 human_clicks, machine_clicks, hard_bounces, soft_bounces,
				 other_bounces, complaints, unsubscribes, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, NOW())
		`, orgID, dayStart, k.domain, k.isp,
			c.sends, c.delivered, c.humanOpens, c.machineOpens,
			c.humanClicks, c.machineClicks, c.hardBounces, c.softBounces,
			c.otherBounces, c.complaints, c.unsubscribes); err != nil {
			return fmt.Errorf("scorecard insert %s/%s: %w", k.domain, k.isp, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("scorecard commit: %w", err)
	}
	return nil
}

// RollupRange rolls up every day in [from, to] inclusive. Per-day failures
// are logged and skipped; a joined error is returned if any day failed.
func RollupRange(ctx context.Context, db *sql.DB, orgID string, from, to time.Time) error {
	var errs []string
	for d := utcDate(from); !d.After(utcDate(to)); d = d.AddDate(0, 0, 1) {
		if err := RollupDay(ctx, db, orgID, d); err != nil {
			log.Printf("[DomainAgent] rollup org=%s day=%s FAILED: %v", orgID, d.Format("2006-01-02"), err)
			errs = append(errs, fmt.Sprintf("%s: %v", d.Format("2006-01-02"), err))
			continue
		}
		log.Printf("[DomainAgent] rollup org=%s day=%s ok", orgID, d.Format("2006-01-02"))
	}
	if len(errs) > 0 {
		return errors.New("scorecard rollup: " + strings.Join(errs, "; "))
	}
	return nil
}
