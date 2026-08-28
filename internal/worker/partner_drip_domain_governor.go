package worker

// Per-SENDING-DOMAIN × ISP global governor (operator 2026-08-27):
// "a global cap for gmail for those 4 domains. That way we make sure they
// recover ... only insurance internal is running gmail, everything else is
// not allowed ... throttle this lane x amount every 15 minutes max, or 4k per
// lane."
//
// Ledger: partner_drip_domain_isp_governor, one row per (brand, isp):
//   daily_cap        the domain's TOTAL daily ceiling for that ISP across every
//                    stream (board + drips + remail). Board sends are never cut
//                    by this overlay — they are the engaged/repair traffic —
//                    but they SPEND the cap, so drips only get what is left.
//   cold_cap         how much of daily_cap cold/drip data may take.
//   allowed_pattern  regex; drip verticals NOT matching get cap 0 for this ISP.
//   lane_daily_cap   per-vertical daily ceiling for this ISP.
//   lane_window_cap  per-vertical ceiling per window_minutes (the 15-min throttle).
//   mode             'shadow' (compute + log, caps unchanged) | 'enforce'.
//
// Spend sources (all existing tables, no hot-path writes):
//   board   = mailing_campaign_plan_recipients rows for campaigns ANCHORED
//             today (Denver scheduled_at — boards are promoted days ahead, so
//             created_at is wrong), non-drip, non-journey, recipient_isp = isp,
//             status IN ('queued','sent')  (~110 ms, idx by campaign_id)
//   drips   = partner_clean_queue first touches (mailed_brand, isp_family,
//             mailed_at today — idx_pcq_governed_daily_count) PLUS, for
//             gmail-only lanes, today's [partner-drip] campaigns' recipients
//             (their follow-ups are all gmail and never stamp mailed_at)
//   lane    = today's [partner-drip] <vertical> <brand> campaigns, and the
//             ones created inside the window.
//
// Applied in BOTH the welcome and the follow-up loops, after the existing
// brand routing / intro budgets. Fail direction for governed cells is CLOSED
// (a count error zeroes the drip cap — a cold wave lost is cheap; a domain
// over its recovery cap is not). Kill switch PARTNER_DRIP_DOMAIN_GOVERNOR_DISABLED.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const PartnerDripDomainGovernorDDL = `
CREATE TABLE IF NOT EXISTS partner_drip_domain_isp_governor (
    brand            TEXT NOT NULL,
    isp              TEXT NOT NULL,
    daily_cap        INTEGER NOT NULL DEFAULT 0,
    cold_cap         INTEGER NOT NULL DEFAULT 0,
    allowed_pattern  TEXT NOT NULL DEFAULT '^internal_auto_insurance',
    lane_daily_cap   INTEGER NOT NULL DEFAULT 4000,
    lane_window_cap  INTEGER NOT NULL DEFAULT 250,
    window_minutes   INTEGER NOT NULL DEFAULT 15,
    mode             TEXT NOT NULL DEFAULT 'shadow',
    updated_by       TEXT,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (brand, isp)
)`

// PartnerDripDomainGovernorSeedDDL seeds the operator's 2026-08-27 numbers:
// engaged gmail/day (reported volume − 5k seeds) + 5k, because the board
// spend counted from plan recipients INCLUDES the seed list and the operator
// ruled seeds are not to be charged — so the engaged allowance stays exactly
// DB 34,913 / HT 26,455 / QF 27,444 / MH 27,642 (+~3% headroom). Cold
// allowance only where an internal-insurance gmail lane lives. Idempotent;
// mode 'shadow' first.
const PartnerDripDomainGovernorSeedDDL = `
INSERT INTO partner_drip_domain_isp_governor (brand, isp, daily_cap, cold_cap, updated_by)
SELECT v.brand, 'gmail', v.daily, v.cold, 'operator-2026-08-27-gmail-recovery'
FROM (VALUES ('db',41000,0), ('ht',36000,4000), ('qf',37000,4000), ('mh',34000,0)) AS v(brand,daily,cold)
WHERE NOT EXISTS (SELECT 1 FROM partner_drip_domain_isp_governor g WHERE g.brand=v.brand AND g.isp='gmail')`

type domainGovernorRow struct {
	brand, isp    string
	dailyCap      int
	coldCap       int
	allowed       *regexp.Regexp
	laneDailyCap  int
	laneWindowCap int
	windowMinutes int
	enforce       bool
}

type domainGovernorState struct {
	mu    sync.RWMutex
	rows  map[string]map[string]domainGovernorRow // brand -> isp -> row
	ready bool
}

func domainGovernorDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PARTNER_DRIP_DOMAIN_GOVERNOR_DISABLED"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// loadDomainGovernor refreshes the cache once per orchestrator tick. A read
// error keeps the previous cache (operator edits land next tick, no deploy).
func (po *PartnerDripOrchestrator) loadDomainGovernor(ctx context.Context) {
	rows := map[string]map[string]domainGovernorRow{}
	err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		rs, err := tx.QueryContext(ctx, `
			SELECT LOWER(brand), LOWER(isp), daily_cap, cold_cap, COALESCE(allowed_pattern,''),
			       lane_daily_cap, lane_window_cap, window_minutes, LOWER(COALESCE(mode,'shadow'))
			FROM partner_drip_domain_isp_governor`)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var r domainGovernorRow
			var pat, mode string
			if err := rs.Scan(&r.brand, &r.isp, &r.dailyCap, &r.coldCap, &pat, &r.laneDailyCap, &r.laneWindowCap, &r.windowMinutes, &mode); err != nil {
				return err
			}
			if pat != "" {
				re, perr := regexp.Compile(pat)
				if perr != nil {
					log.Printf("[DomainGovernor] %s/%s bad allowed_pattern %q — treating as match-nothing", r.brand, r.isp, pat)
					re = regexp.MustCompile(`^$`)
				}
				r.allowed = re
			}
			if r.windowMinutes <= 0 {
				r.windowMinutes = 15
			}
			r.enforce = mode == "enforce"
			if rows[r.brand] == nil {
				rows[r.brand] = map[string]domainGovernorRow{}
			}
			rows[r.brand][r.isp] = r
		}
		return rs.Err()
	})
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return // table not migrated yet — overlay absent, behavior unchanged
		}
		log.Printf("[DomainGovernor] load failed (%v) — keeping previous cache", err)
		return
	}
	po.domainGov.mu.Lock()
	po.domainGov.rows, po.domainGov.ready = rows, true
	po.domainGov.mu.Unlock()
}

// domainGovernorSpend is everything the decision needs for one (brand, isp, vertical).
type domainGovernorSpend struct {
	board, drips, laneToday, laneWindow int
}

// domainGovernorDecide is the pure decision: returns the cap the drip may use
// for this ISP and the binding reason. Unit-tested without a DB.
func domainGovernorDecide(row domainGovernorRow, vertical string, capIn int, sp domainGovernorSpend) (int, string) {
	if capIn <= 0 {
		return 0, "cap already 0"
	}
	if row.allowed != nil && !row.allowed.MatchString(vertical) {
		return 0, "vertical not allowed on this domain/isp"
	}
	best, why := capIn, "lane cap"
	take := func(v int, reason string) {
		if v < 0 {
			v = 0
		}
		if v < best {
			best, why = v, reason
		}
	}
	take(row.coldCap-sp.drips, fmt.Sprintf("cold_cap %d − drips today %d", row.coldCap, sp.drips))
	take(row.dailyCap-sp.board-sp.drips, fmt.Sprintf("daily_cap %d − board %d − drips %d", row.dailyCap, sp.board, sp.drips))
	if row.laneDailyCap > 0 {
		take(row.laneDailyCap-sp.laneToday, fmt.Sprintf("lane_daily_cap %d − lane today %d", row.laneDailyCap, sp.laneToday))
	}
	if row.laneWindowCap > 0 {
		take(row.laneWindowCap-sp.laneWindow, fmt.Sprintf("lane_window_cap %d − last %dm %d", row.laneWindowCap, row.windowMinutes, sp.laneWindow))
	}
	return best, why
}

// domainGovernorSpendToday reads the three spend figures for (brand, isp, vertical).
func (po *PartnerDripOrchestrator) domainGovernorSpendToday(ctx context.Context, row domainGovernorRow, vertical string) (domainGovernorSpend, error) {
	var sp domainGovernorSpend
	pattern := ""
	if row.allowed != nil {
		pattern = row.allowed.String()
	}
	err := po.withDBTimeout(ctx, func(tx *sql.Tx) error {
		// Board spend: today's non-drip campaigns of the brand, this ISP.
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM mailing_campaign_plan_recipients r
			WHERE r.recipient_isp = $2 AND r.status IN ('queued','sent')
			  AND r.campaign_id IN (
			    SELECT id FROM mailing_campaigns
			    WHERE partner_drip_tag IS NULL AND journey_id IS NULL
			      AND status NOT IN ('cancelled','deleted','failed','draft')
			      AND scheduled_at >= date_trunc('day', NOW() AT TIME ZONE 'America/Denver') AT TIME ZONE 'America/Denver'
			      AND scheduled_at <  (date_trunc('day', NOW() AT TIME ZONE 'America/Denver') + INTERVAL '1 day') AT TIME ZONE 'America/Denver'
			      AND name ~ (' - ' || UPPER($1) || ' - '))
		`, row.brand, row.isp).Scan(&sp.board); err != nil {
			return fmt.Errorf("board spend: %w", err)
		}
		// Drip first touches on this brand/ISP today, allowed verticals only.
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM partner_clean_queue
			WHERE mailed_brand = $1 AND LOWER(COALESCE(isp_family,'')) = $2
			  AND mailed_at >= date_trunc('day', NOW() AT TIME ZONE 'America/Denver') AT TIME ZONE 'America/Denver'
			  AND ($3 = '' OR vertical ~ $3)
		`, row.brand, row.isp, pattern).Scan(&sp.drips); err != nil {
			return fmt.Errorf("drip spend: %w", err)
		}
		// Follow-ups on ISP-only lanes never stamp mailed_at: add today's
		// campaign recipients for lanes named for this ISP, minus their first
		// touches (already counted above).
		var laneAll, laneFirst int
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(COALESCE(total_recipients, sent_count, 0)),0) FROM mailing_campaigns
			WHERE name ~ ('^\[partner-drip\] \S*' || $2 || '\S* ' || $1 || ' ')
			  AND created_at >= date_trunc('day', NOW() AT TIME ZONE 'America/Denver') AT TIME ZONE 'America/Denver'
		`, row.brand, "_"+row.isp+"_").Scan(&laneAll); err != nil {
			return fmt.Errorf("isp-lane campaigns: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM partner_clean_queue
			WHERE mailed_brand = $1 AND vertical ~ $2
			  AND mailed_at >= date_trunc('day', NOW() AT TIME ZONE 'America/Denver') AT TIME ZONE 'America/Denver'
		`, row.brand, "_"+row.isp+"_").Scan(&laneFirst); err != nil {
			return fmt.Errorf("isp-lane first touches: %w", err)
		}
		if laneAll > laneFirst {
			sp.drips += laneAll - laneFirst
		}
		// This vertical's sends today and inside the window (all brands — the
		// lane throttle is per lane, not per brand).
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(COALESCE(total_recipients, sent_count, 0)),0),
			       COALESCE(SUM(CASE WHEN created_at >= NOW() - ($2 * INTERVAL '1 minute') THEN COALESCE(total_recipients, sent_count, 0) ELSE 0 END),0)
			FROM mailing_campaigns
			WHERE name ~ ('^\[partner-drip\] ' || $1 || ' ')
			  AND created_at >= date_trunc('day', NOW() AT TIME ZONE 'America/Denver') AT TIME ZONE 'America/Denver'
		`, vertical, row.windowMinutes).Scan(&sp.laneToday, &sp.laneWindow); err != nil {
			return fmt.Errorf("lane spend: %w", err)
		}
		return nil
	})
	return sp, err
}

// applyDomainGovernor clamps a wave's per-ISP caps to the domain's recovery
// ceiling. Shadow mode logs the decision and returns caps unchanged.
func (po *PartnerDripOrchestrator) applyDomainGovernor(ctx context.Context, brand, vertical, pass string, caps map[string]int) map[string]int {
	if domainGovernorDisabled() {
		return caps
	}
	po.domainGov.mu.RLock()
	ready := po.domainGov.ready
	rows := po.domainGov.rows[strings.ToLower(strings.TrimSpace(brand))]
	po.domainGov.mu.RUnlock()
	if !ready || len(rows) == 0 {
		return caps
	}
	out := cloneISPCapMap(caps)
	for isp, capIn := range caps {
		row, ok := rows[strings.ToLower(strings.TrimSpace(isp))]
		if !ok || capIn <= 0 {
			continue
		}
		sp, err := po.domainGovernorSpendToday(ctx, row, vertical)
		if err != nil {
			log.Printf("[DomainGovernor] %s/%s %s spend read failed (%v) — %s", brand, isp, vertical, err,
				map[bool]string{true: "FAIL-CLOSED cap=0", false: "shadow: cap unchanged"}[row.enforce])
			if row.enforce {
				out[isp] = 0
			}
			continue
		}
		allowed, why := domainGovernorDecide(row, vertical, capIn, sp)
		if sp.board > row.dailyCap {
			log.Printf("[DomainGovernor] WARN %s/%s board alone is over daily_cap (%d > %d) — board is never cut here", brand, isp, sp.board, row.dailyCap)
		}
		if allowed != capIn {
			log.Printf("[DomainGovernor] %s %s/%s %s pass=%s cap %d → %d (%s)",
				map[bool]string{true: "ENFORCE", false: "SHADOW"}[row.enforce], brand, isp, vertical, pass, capIn, allowed, why)
		}
		if row.enforce {
			out[isp] = allowed
		}
	}
	return out
}

// domainGovernorWindow is exported for tests/diagnostics: the Denver day start.
func domainGovernorDayStart(now time.Time) time.Time {
	loc, _ := time.LoadLocation("America/Denver")
	d := now.In(loc)
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
}
