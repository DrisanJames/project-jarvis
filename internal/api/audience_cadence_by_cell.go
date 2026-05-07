package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/lib/pq"
)

// VersionAudienceCadenceByISP is the response version for
// /api/mailing/analytics/audience-cadence-by-isp.
//
//	2.0 (2026-05-05): GLOBAL per-ISP refactor. Replaces the prior per-(brand × ISP)
//	view (1.0). Reason: the welcome pool is a single shared bucket — the deploy
//	scripts hand the same six inclusion segments to every brand and the planner
//	shuffles by ISP across brands as it picks recipients. So the meaningful
//	exhaustion question is global per ISP, not per (sending_domain × ISP). The
//	1.0 view double-counted subscribers touched by multiple brands (one row per
//	subscriber × sending_domain in mailing_subscriber_domain_state) and gave a
//	misleadingly inflated picture of pool size. 2.0 fixes that by grouping on
//	(subscriber → ISP) directly, deduped, using mailing_message_log for prior
//	send count exactly the way welcome_audience_health does.
//
//	Pool definition:
//	  - Source: the six welcomePoolSegmentIDs (Mailgun-Engaged-Master + 4
//	    Attribits brand feeds + Welcome-Yahoo-Priority-Engaged) — same as the
//	    deploy scripts.
//	  - Subscriber set: status='confirmed', deduped across segments.
//	  - prior_sends: COUNT(*) from mailing_message_log per subscriber. (Same
//	    metric the planner uses to enforce welcomeSaturationThreshold.)
//	  - last_open_at IS NOT NULL → exempt (handed to Engager flow).
//	  - prior_sends > welcomeSaturationThreshold AND no open → retired.
//	  - 0..welcomeSaturationThreshold AND no open → still in welcome pool,
//	    bucketed by sends-remaining-until-retirement.
//
//	Burn definition:
//	  - Welcome 'sent' events from mailing_tracking_events in the last
//	    audienceCadenceWelcomeBurnWindowDays days, classified by ISP from
//	    mailing_subscribers.email (NOT t.recipient_domain — see churn rationale
//	    below).
//	  - daily_unique_burn = distinct (subscriber, day) pairs / window_days.
//
//	Churn definition:
//	  - mailing_tracking_events for unsubscribed / hard bounce / complaint in
//	    the last audienceCadenceChurnWindowDays days. ISP classified from
//	    mailing_subscribers.email because t.recipient_domain is NULL for
//	    unsubscribe events.
//
// Surfaces: SCHEDULING_INTEGRITY_PLAYBOOK §15 — the single live source of
// truth for "when do I need to upload more data per ISP?".
//
// Architecture: heavy queries (~95s combined) run inside a 15-minute background
// worker that materialises mailing_audience_cadence_isp_snapshot. The handler
// reads from that table and serves in <300ms.
const VersionAudienceCadenceByISP = "2.0"

const (
	// activationTargetPct is the operator-locked threshold for "is this ISP
	// engagement-warm enough that uploading raw data will land?". ISPs below
	// 1% opener share are flagged AMPLE — uploading raw data here only inflates
	// the saturation tier (see §15.3 of SCHEDULING_INTEGRITY_PLAYBOOK).
	activationTargetPct = 1.0

	// audienceCadenceWelcomeBurnWindowDays controls how many trailing days of
	// Welcome sends we average to compute the daily burn rate.
	audienceCadenceWelcomeBurnWindowDays = 7

	// audienceCadenceChurnWindowDays controls how many trailing days of churn
	// we report.
	audienceCadenceChurnWindowDays = 30

	// audienceCadenceSaturationThreshold mirrors welcomeSaturationThreshold (8).
	audienceCadenceSaturationThreshold = 8

	// audienceCadenceRefreshInterval is how often the background worker
	// rebuilds the snapshot table.
	audienceCadenceRefreshInterval = 15 * time.Minute

	// audienceCadenceStalenessThreshold — if the snapshot is older than this
	// when the handler is called, kick off an async refresh.
	audienceCadenceStalenessThreshold = 30 * time.Minute

	// audienceCadenceRefreshTimeout — max time the snapshot refresh queries
	// can run before being cancelled. Sized to comfortably cover the three
	// queries (pool ~70s, burn ~30s, churn ~3s) plus headroom for production
	// load (we've observed 5x slowdowns under contention).
	audienceCadenceRefreshTimeout = 15 * time.Minute
)

// ispCadenceRow is one ISP slice of the global welcome pool. ISP is the only
// dimension — there is intentionally no brand or sending_domain field. See
// VersionAudienceCadenceByISP comment for rationale.
type ispCadenceRow struct {
	ISP                   string  `json:"isp"`
	PoolTotal             int     `json:"pool_total"`             // total unique subs in pool with this ISP
	ExemptOpened          int     `json:"exempt_opened"`          // last_open_at IS NOT NULL → handed to Engager
	RetiredSaturated      int     `json:"retired_saturated"`      // prior_sends > 8, no open → already excluded
	NeverMailed           int     `json:"never_mailed"`           // prior_sends = 0
	OneSendAway           int     `json:"one_send_away"`          // prior_sends = 8 (next send retires)
	TwoToThreeAway        int     `json:"two_to_three_away"`      // prior_sends 6..7
	FourToFiveAway        int     `json:"four_to_five_away"`      // prior_sends 4..5
	SixToEightAway        int     `json:"six_to_eight_away"`      // prior_sends 1..3 (mislabeled but kept stable)
	WelcomePool           int     `json:"welcome_pool"`           // sum of the four mailable buckets
	DailyUniqueBurn7d     int     `json:"daily_unique_burn_7d"`   // global Welcome unique-subs/day
	Unsubscribed30d       int     `json:"unsubscribed_30d"`
	HardBounced30d        int     `json:"hard_bounced_30d"`
	Complained30d         int     `json:"complained_30d"`
	CycleDays             float64 `json:"cycle_days"`
	SaturationHorizonDays float64 `json:"saturation_horizon_days"`
	ActivationPct         float64 `json:"activation_pct"`
	ActivationTargetMet   bool    `json:"activation_target_met"`
	UnsubRate30dPct       float64 `json:"unsub_rate_30d_pct"`
	HardBounceRate30dPct  float64 `json:"hard_bounce_rate_30d_pct"`
	RefreshTrigger        string  `json:"refresh_trigger"`
	RefreshReason         string  `json:"refresh_reason"`
}

// HandleAudienceCadenceByISP answers GET /api/mailing/analytics/audience-cadence-by-isp.
//
// Reads from `mailing_audience_cadence_isp_snapshot` — refreshed every
// audienceCadenceRefreshInterval by StartAudienceCadenceWorker. If the snapshot
// is empty (first boot) we run a synchronous refresh; if stale we kick off an
// async refresh while serving the current rows.
func (s *AdvancedMailingService) HandleAudienceCadenceByISP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	rows, refreshedAt, err := s.readISPCadenceSnapshot(ctx)
	if err != nil {
		log.Printf("[audience-cadence-by-isp] snapshot read error: %v", err)
		respondError(w, http.StatusInternalServerError, "audience_cadence_snapshot_unavailable")
		return
	}

	if len(rows) == 0 {
		// Snapshot is empty (first boot). Kick off async build and tell the
		// frontend to retry — synchronous wait would exceed the ALB timeout.
		log.Printf("[audience-cadence-by-isp] snapshot empty — kicking off async build")
		go func() {
			ctxBg, cancelBg := context.WithTimeout(context.Background(), audienceCadenceRefreshTimeout)
			defer cancelBg()
			if err := s.RefreshAudienceCadenceISPSnapshot(ctxBg); err != nil {
				log.Printf("[audience-cadence-by-isp] async build error: %v", err)
			}
		}()
		respondJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":   "audience_cadence_snapshot_building",
			"message": "Snapshot is being built (first boot or table truncation). Retry in 1-3 minutes.",
		})
		return
	} else if refreshedAt.Before(time.Now().Add(-audienceCadenceStalenessThreshold)) {
		log.Printf("[audience-cadence-by-isp] snapshot stale (%s) — kicking off async refresh", refreshedAt.Format(time.RFC3339))
		go func() {
			ctxBg, cancelBg := context.WithTimeout(context.Background(), audienceCadenceRefreshTimeout)
			defer cancelBg()
			if err := s.RefreshAudienceCadenceISPSnapshot(ctxBg); err != nil {
				log.Printf("[audience-cadence-by-isp] async refresh error: %v", err)
			}
		}()
	}

	resp := s.buildISPCadenceResponse(rows, refreshedAt)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("[audience-cadence-by-isp] encode error: %v", err)
	}
}

// HandleAudienceCadenceRefresh allows operators to force a refresh of the
// snapshot. POST /api/mailing/analytics/audience-cadence-by-isp/refresh
// Useful right after a list upload to see the new pool counts immediately.
//
// Fire-and-forget: returns 202 Accepted immediately and runs the refresh in
// a background goroutine with a fresh context (so the ALB's 5-min timeout
// doesn't cancel the queries mid-flight). The mutex inside
// RefreshAudienceCadenceISPSnapshot prevents concurrent runs — if a refresh
// is already in flight, this one waits its turn (no spam-rebuild risk).
//
// The frontend should re-fetch the snapshot endpoint a minute or two later
// to see the new data — or simply wait for the next 15-min auto refresh.
func (s *AdvancedMailingService) HandleAudienceCadenceRefresh(w http.ResponseWriter, r *http.Request) {
	go func() {
		ctxBg, cancelBg := context.WithTimeout(context.Background(), audienceCadenceRefreshTimeout)
		defer cancelBg()
		start := time.Now()
		if err := s.RefreshAudienceCadenceISPSnapshot(ctxBg); err != nil {
			log.Printf("[audience-cadence-by-isp] manual refresh error: %v", err)
			return
		}
		log.Printf("[audience-cadence-by-isp] manual refresh ok in %s", time.Since(start))
	}()
	respondJSON(w, http.StatusAccepted, map[string]any{
		"status":   "accepted",
		"message":  "Snapshot rebuild queued — re-fetch the cadence endpoint in 1-3 minutes to see updated data.",
		"queued_at": time.Now().UTC().Format(time.RFC3339),
	})
}

// audienceCadenceRefreshMu serialises snapshot rebuilds across goroutines so
// the heavy queries never run concurrently.
var audienceCadenceRefreshMu sync.Mutex

// RefreshAudienceCadenceISPSnapshot rebuilds `mailing_audience_cadence_isp_snapshot`
// from the live tables. Runs the three heavy queries in sequence under per-
// statement timeouts, then writes the rows into the snapshot table inside a
// transaction (DELETE + INSERT for atomicity).
//
// Safe to call concurrently; only one rebuild runs at a time. Honours ctx
// cancellation between queries so a stuck refresh can be aborted.
func (s *AdvancedMailingService) RefreshAudienceCadenceISPSnapshot(ctx context.Context) error {
	audienceCadenceRefreshMu.Lock()
	defer audienceCadenceRefreshMu.Unlock()

	start := time.Now()
	pool, err := s.queryISPCadencePool(ctx)
	if err != nil {
		return fmt.Errorf("pool query: %w", err)
	}
	if err := s.queryISPCadenceBurn(ctx, pool); err != nil {
		// Burn errors are non-fatal — we still serve pool counts.
		log.Printf("[audience-cadence-refresh] burn query (non-fatal): %v", err)
	}
	if err := s.queryISPCadenceChurn(ctx, pool); err != nil {
		log.Printf("[audience-cadence-refresh] churn query (non-fatal): %v", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mailing_audience_cadence_isp_snapshot`); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete: %w", err)
	}
	rowCount := 0
	for _, c := range pool {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mailing_audience_cadence_isp_snapshot
			(isp, pool_total, exempt_opened, retired_saturated,
			 never_mailed, one_send_away, two_to_three_away,
			 four_to_five_away, six_to_eight_away,
			 daily_unique_burn_7d, unsubscribed_30d, hard_bounced_30d, complained_30d,
			 refreshed_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NOW())
		`, c.ISP,
			c.PoolTotal, c.ExemptOpened, c.RetiredSaturated,
			c.NeverMailed, c.OneSendAway, c.TwoToThreeAway,
			c.FourToFiveAway, c.SixToEightAway,
			c.DailyUniqueBurn7d, c.Unsubscribed30d, c.HardBounced30d, c.Complained30d); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("insert (%s): %w", c.ISP, err)
		}
		rowCount++
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	log.Printf("[audience-cadence-refresh] wrote %d rows in %s", rowCount, time.Since(start))
	return nil
}

// ispCadenceISPCase is the inline ISP classifier used in every cadence query.
// Mirrors isp.GroupFromDomain in Go — kept inline so the snapshot worker
// matches what the planner sees. Email column is parameterised.
//
// Use as: fmt.Sprintf(query, ispCaseFromEmail("s.email")).
func ispCaseFromEmail(emailCol string) string {
	return fmt.Sprintf(`
		CASE
			WHEN lower(split_part(%[1]s,'@',2)) IN ('gmail.com','googlemail.com') THEN 'gmail'
			WHEN lower(split_part(%[1]s,'@',2)) IN ('yahoo.com','ymail.com','rocketmail.com','myyahoo.com','yahoo.ca','yahoo.co.uk','yahoo.com.au','yahoo.co.in') THEN 'yahoo'
			WHEN lower(split_part(%[1]s,'@',2)) IN ('outlook.com','outlook.co.uk','hotmail.com','hotmail.co.uk','hotmail.fr','live.com','live.co.uk','msn.com') THEN 'microsoft'
			WHEN lower(split_part(%[1]s,'@',2)) IN ('icloud.com','me.com','mac.com') THEN 'apple'
			WHEN lower(split_part(%[1]s,'@',2)) IN ('aol.com','aim.com') THEN 'aol'
			WHEN lower(split_part(%[1]s,'@',2)) IN ('comcast.net','xfinity.com') THEN 'comcast'
			WHEN lower(split_part(%[1]s,'@',2)) IN ('charter.net','spectrum.net','rr.com','roadrunner.com','twc.com','brighthouse.com') THEN 'charter'
			WHEN lower(split_part(%[1]s,'@',2)) = 'att.net' THEN 'att'
			WHEN lower(split_part(%[1]s,'@',2)) IN ('sbcglobal.net','bellsouth.net','pacbell.net','swbell.net','ameritech.net') THEN 'sbcglobal'
			WHEN lower(split_part(%[1]s,'@',2)) = 'cox.net' THEN 'cox'
			ELSE 'other'
		END
	`, emailCol)
}

// queryISPCadencePool runs the global pool query: distinct confirmed
// subscribers across the six welcomePoolSegmentIDs, classified by ISP via
// mailing_subscribers.email, with prior_sends pulled from mailing_message_log
// (same source the planner enforces saturation against).
func (s *AdvancedMailingService) queryISPCadencePool(ctx context.Context) (map[string]*ispCadenceRow, error) {
	pool := map[string]*ispCadenceRow{}
	q := fmt.Sprintf(`
		WITH eligible AS (
			SELECT DISTINCT s.id, s.email, s.last_open_at
			FROM mailing_segment_members m
			JOIN mailing_subscribers s ON s.id = m.subscriber_id
			WHERE m.segment_id = ANY($1::uuid[])
			  AND s.status = 'confirmed'
		),
		sends AS (
			SELECT subscriber_id, COUNT(*) AS prior_sends
			FROM mailing_message_log
			WHERE subscriber_id IN (SELECT id FROM eligible)
			GROUP BY subscriber_id
		)
		SELECT
			(%s) AS isp,
			COUNT(*)                                                                         AS pool_total,
			COUNT(*) FILTER (WHERE e.last_open_at IS NOT NULL)                               AS exempt_opened,
			COUNT(*) FILTER (WHERE e.last_open_at IS NULL AND COALESCE(s.prior_sends,0) > $2) AS retired,
			COUNT(*) FILTER (WHERE e.last_open_at IS NULL AND COALESCE(s.prior_sends,0) = 0) AS never_mailed,
			COUNT(*) FILTER (WHERE e.last_open_at IS NULL AND s.prior_sends = $2)             AS one_send_away,
			COUNT(*) FILTER (WHERE e.last_open_at IS NULL AND s.prior_sends BETWEEN 6 AND 7) AS two_to_three_away,
			COUNT(*) FILTER (WHERE e.last_open_at IS NULL AND s.prior_sends BETWEEN 4 AND 5) AS four_to_five_away,
			COUNT(*) FILTER (WHERE e.last_open_at IS NULL AND s.prior_sends BETWEEN 1 AND 3) AS six_to_eight_away
		FROM eligible e
		LEFT JOIN sends s ON s.subscriber_id = e.id
		GROUP BY isp
	`, ispCaseFromEmail("e.email"))
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin pool tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = '600s'"); err != nil {
		return nil, fmt.Errorf("set timeout: %w", err)
	}
	rows, err := tx.QueryContext(ctx, q, pq.Array(welcomePoolSegmentIDs), audienceCadenceSaturationThreshold)
	if err != nil {
		return nil, fmt.Errorf("pool query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ispKey string
		var poolTotal, exempt, retired, never, one, twoThree, fourFive, sixEight int
		if err := rows.Scan(&ispKey, &poolTotal, &exempt, &retired, &never, &one, &twoThree, &fourFive, &sixEight); err != nil {
			log.Printf("[audience-cadence-refresh] pool scan: %v", err)
			continue
		}
		pool[ispKey] = &ispCadenceRow{
			ISP:              ispKey,
			PoolTotal:        poolTotal,
			ExemptOpened:     exempt,
			RetiredSaturated: retired,
			NeverMailed:      never,
			OneSendAway:      one,
			TwoToThreeAway:   twoThree,
			FourToFiveAway:   fourFive,
			SixToEightAway:   sixEight,
			WelcomePool:      never + one + twoThree + fourFive + sixEight,
		}
	}
	return pool, rows.Err()
}

// queryISPCadenceBurn fills the 7-day Welcome burn rate per ISP. Counts
// distinct (subscriber, day) pairs to compute average unique subscribers
// mailed per day across all sending domains. ISP classified from
// mailing_subscribers.email.
func (s *AdvancedMailingService) queryISPCadenceBurn(ctx context.Context, pool map[string]*ispCadenceRow) error {
	q := fmt.Sprintf(`
		WITH events AS (
			SELECT
				(%s) AS isp,
				t.subscriber_id,
				date_trunc('day', t.created_at) AS day
			FROM mailing_tracking_events t
			JOIN mailing_subscribers s ON s.id = t.subscriber_id
			JOIN mailing_campaigns c ON c.id = t.campaign_id
			WHERE t.event_type = 'sent'
			  AND t.created_at >= NOW() - ($1 || ' days')::interval
			  AND c.name ILIKE '%%Welcome%%'
		)
		SELECT isp,
		       (COUNT(DISTINCT (subscriber_id::text || '|' || day::text))::numeric / $1::numeric)::int AS avg_unique_per_day
		FROM events
		GROUP BY isp
	`, ispCaseFromEmail("s.email"))
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin burn tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = '600s'"); err != nil {
		return fmt.Errorf("set timeout: %w", err)
	}
	rows, err := tx.QueryContext(ctx, q, audienceCadenceWelcomeBurnWindowDays)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ispKey string
		var avg int
		if err := rows.Scan(&ispKey, &avg); err != nil {
			log.Printf("[audience-cadence-refresh] burn scan: %v", err)
			continue
		}
		c := pool[ispKey]
		if c == nil {
			c = &ispCadenceRow{ISP: ispKey}
			pool[ispKey] = c
		}
		c.DailyUniqueBurn7d = avg
	}
	return rows.Err()
}

// queryISPCadenceChurn fills the 30-day churn fields per ISP. ISP is
// classified from mailing_subscribers.email (joined via t.subscriber_id)
// because t.recipient_domain is NULL for unsubscribe events.
func (s *AdvancedMailingService) queryISPCadenceChurn(ctx context.Context, pool map[string]*ispCadenceRow) error {
	q := fmt.Sprintf(`
		WITH events AS (
			SELECT
				(%s) AS isp,
				t.subscriber_id,
				t.event_type,
				t.bounce_type
			FROM mailing_tracking_events t
			JOIN mailing_subscribers s ON s.id = t.subscriber_id
			WHERE t.created_at >= NOW() - ($1 || ' days')::interval
			  AND t.event_type IN ('unsubscribed','bounced','complaint','complained')
		)
		SELECT
			isp,
			COUNT(DISTINCT subscriber_id) FILTER (WHERE event_type='unsubscribed')                       AS unsubs,
			COUNT(DISTINCT subscriber_id) FILTER (WHERE event_type='bounced' AND bounce_type='hard')     AS hard_bounces,
			COUNT(DISTINCT subscriber_id) FILTER (WHERE event_type IN ('complaint','complained'))        AS complaints
		FROM events
		GROUP BY isp
	`, ispCaseFromEmail("s.email"))
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin churn tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = '600s'"); err != nil {
		return fmt.Errorf("set timeout: %w", err)
	}
	rows, err := tx.QueryContext(ctx, q, audienceCadenceChurnWindowDays)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ispKey string
		var unsub, hb, comp int
		if err := rows.Scan(&ispKey, &unsub, &hb, &comp); err != nil {
			log.Printf("[audience-cadence-refresh] churn scan: %v", err)
			continue
		}
		c := pool[ispKey]
		if c == nil {
			c = &ispCadenceRow{ISP: ispKey}
			pool[ispKey] = c
		}
		c.Unsubscribed30d = unsub
		c.HardBounced30d = hb
		c.Complained30d = comp
	}
	return rows.Err()
}

// readISPCadenceSnapshot reads the materialised rows from the snapshot table.
// Returns the rows and the most recent refreshed_at across them.
func (s *AdvancedMailingService) readISPCadenceSnapshot(ctx context.Context) ([]ispCadenceRow, time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT isp, pool_total, exempt_opened, retired_saturated,
		       never_mailed, one_send_away, two_to_three_away,
		       four_to_five_away, six_to_eight_away,
		       daily_unique_burn_7d, unsubscribed_30d, hard_bounced_30d, complained_30d,
		       refreshed_at
		FROM mailing_audience_cadence_isp_snapshot
	`)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	out := []ispCadenceRow{}
	var maxRefresh time.Time
	for rows.Next() {
		var c ispCadenceRow
		var refreshed time.Time
		if err := rows.Scan(
			&c.ISP, &c.PoolTotal, &c.ExemptOpened, &c.RetiredSaturated,
			&c.NeverMailed, &c.OneSendAway, &c.TwoToThreeAway,
			&c.FourToFiveAway, &c.SixToEightAway,
			&c.DailyUniqueBurn7d, &c.Unsubscribed30d, &c.HardBounced30d, &c.Complained30d,
			&refreshed,
		); err != nil {
			return nil, time.Time{}, err
		}
		c.WelcomePool = c.NeverMailed + c.OneSendAway + c.TwoToThreeAway + c.FourToFiveAway + c.SixToEightAway
		finalizeISPCadence(&c)
		out = append(out, c)
		if refreshed.After(maxRefresh) {
			maxRefresh = refreshed
		}
	}
	return out, maxRefresh, rows.Err()
}

// buildISPCadenceResponse assembles the final response envelope from the
// snapshot rows. All derived metrics (cycle, activation, refresh trigger) have
// already been filled by finalizeISPCadence.
func (s *AdvancedMailingService) buildISPCadenceResponse(rows []ispCadenceRow, refreshedAt time.Time) map[string]any {
	asOf := time.Now().UTC()

	// Sort by trigger urgency, then by burn rate (fastest-burning first).
	sort.SliceStable(rows, func(i, j int) bool {
		ti, tj := triggerOrder(rows[i].RefreshTrigger), triggerOrder(rows[j].RefreshTrigger)
		if ti != tj {
			return ti < tj
		}
		return rows[i].DailyUniqueBurn7d > rows[j].DailyUniqueBurn7d
	})

	// Aggregate totals across all ISPs (the global view of the global view).
	totals := ispCadenceRow{ISP: "ALL"}
	nowCount, weekCount, ampleCount := 0, 0, 0
	for _, r := range rows {
		totals.PoolTotal += r.PoolTotal
		totals.ExemptOpened += r.ExemptOpened
		totals.RetiredSaturated += r.RetiredSaturated
		totals.NeverMailed += r.NeverMailed
		totals.OneSendAway += r.OneSendAway
		totals.TwoToThreeAway += r.TwoToThreeAway
		totals.FourToFiveAway += r.FourToFiveAway
		totals.SixToEightAway += r.SixToEightAway
		totals.WelcomePool += r.WelcomePool
		totals.DailyUniqueBurn7d += r.DailyUniqueBurn7d
		totals.Unsubscribed30d += r.Unsubscribed30d
		totals.HardBounced30d += r.HardBounced30d
		totals.Complained30d += r.Complained30d
		switch r.RefreshTrigger {
		case "NOW":
			nowCount++
		case "THIS_WEEK":
			weekCount++
		case "AMPLE":
			ampleCount++
		}
	}
	finalizeISPCadence(&totals)

	hottest := []ispCadenceRow{}
	for _, r := range rows {
		if r.RefreshTrigger == "NOW" {
			hottest = append(hottest, r)
		}
	}

	return map[string]any{
		"api_version":           VersionAudienceCadenceByISP,
		"as_of":                 asOf.Format(time.RFC3339),
		"snapshot_refreshed_at": refreshedAt.UTC().Format(time.RFC3339),
		"snapshot_age_seconds":  int(asOf.Sub(refreshedAt).Seconds()),
		"saturation_threshold":  audienceCadenceSaturationThreshold,
		"activation_target_pct": activationTargetPct,
		"burn_window_days":      audienceCadenceWelcomeBurnWindowDays,
		"churn_window_days":     audienceCadenceChurnWindowDays,
		"pool_segment_ids":      welcomePoolSegmentIDs,
		"isp_rows":              rows,
		"top_now_isps":          hottest,
		"totals":                totals,
		"trigger_counts": map[string]int{
			"now":       nowCount,
			"this_week": weekCount,
			"ample":     ampleCount,
		},
		"trigger_definitions": []map[string]string{
			{"trigger": "NOW", "rule": "cycle_days <= 10 AND activation >= 1%", "action": "Upload today or tomorrow — new data will land"},
			{"trigger": "THIS_WEEK", "rule": "cycle_days 10-20 AND activation >= 1%", "action": "Upload within 7 days"},
			{"trigger": "TWO_WEEKS", "rule": "cycle_days 20-40 AND activation >= 1%", "action": "Upload within 14 days"},
			{"trigger": "ONE_MONTH_PLUS", "rule": "cycle_days 40-90 AND activation >= 1%", "action": "Upload within 30 days"},
			{"trigger": "AMPLE", "rule": "activation < 1% OR cycle_days > 90", "action": "Do NOT upload raw — engagement-cold or pool not the bottleneck. Source engaged-only data."},
			{"trigger": "UNKNOWN", "rule": "no Welcome burn observed in 7d", "action": "ISP not currently being mailed — burn unknown"},
		},
		"explainer": map[string]string{
			"pool_definition":  "Distinct confirmed subscribers in the 6 welcomePoolSegmentIDs (Mailgun-Engaged-Master + 4 Attribits feeds + Welcome-Yahoo-Priority-Engaged) — the unified bucket that feeds every brand's Welcome campaigns.",
			"burn_definition":  fmt.Sprintf("Average unique subscribers mailed per day from any Welcome campaign over the last %d days (across all sending domains).", audienceCadenceWelcomeBurnWindowDays),
			"churn_definition": fmt.Sprintf("Distinct subscribers who unsubscribed / hard bounced / complained in the last %d days.", audienceCadenceChurnWindowDays),
			"isp_classification": "ISP derived from mailing_subscribers.email (NOT t.recipient_domain — it is NULL for unsubscribe events).",
			"why_global_not_brand": "The 6 inclusion segments form a single shared bucket. The deploy scripts hand the same pool to every brand and the planner shuffles by ISP across brands as it picks recipients. Per-(brand × ISP) counts double-count subscribers touched by multiple brands. The exhaustion question is global per ISP.",
		},
	}
}

// StartAudienceCadenceWorker launches a goroutine that refreshes the snapshot
// every audienceCadenceRefreshInterval. Call once from cmd/server/main.go after
// the AdvancedMailingService is constructed.
//
// On the first tick (after a 60-second warmup) it runs synchronously; subsequent
// ticks honour the same singleflight mutex used by the manual refresh handler.
func (s *AdvancedMailingService) StartAudienceCadenceWorker(ctx context.Context) {
	go func() {
		select {
		case <-time.After(60 * time.Second):
		case <-ctx.Done():
			return
		}
		runCtx, runCancel := context.WithTimeout(ctx, audienceCadenceRefreshTimeout)
		if err := s.RefreshAudienceCadenceISPSnapshot(runCtx); err != nil {
			log.Printf("[audience-cadence-worker] initial refresh: %v", err)
		}
		runCancel()
		ticker := time.NewTicker(audienceCadenceRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rc, rcCancel := context.WithTimeout(ctx, audienceCadenceRefreshTimeout)
				if err := s.RefreshAudienceCadenceISPSnapshot(rc); err != nil {
					log.Printf("[audience-cadence-worker] periodic refresh: %v", err)
				}
				rcCancel()
			}
		}
	}()
}

// finalizeISPCadence computes derived metrics + classification for one ISP row.
// Called both after read (snapshot rows are stored without derived fields) and
// for the totals roll-up in the response builder.
func finalizeISPCadence(c *ispCadenceRow) {
	denom := c.PoolTotal
	if denom > 0 {
		c.ActivationPct = acRound2(float64(c.ExemptOpened) / float64(denom) * 100.0)
		c.UnsubRate30dPct = acRound2(float64(c.Unsubscribed30d) / float64(denom) * 100.0)
		c.HardBounceRate30dPct = acRound2(float64(c.HardBounced30d) / float64(denom) * 100.0)
	}
	c.ActivationTargetMet = c.ActivationPct >= activationTargetPct
	if c.DailyUniqueBurn7d > 0 {
		c.CycleDays = acRound1(float64(c.WelcomePool) / float64(c.DailyUniqueBurn7d))
		c.SaturationHorizonDays = acRound1(c.CycleDays * 4.0)
	}
	c.RefreshTrigger, c.RefreshReason = classifyTrigger(c.CycleDays, c.ActivationPct, c.DailyUniqueBurn7d)
}

// classifyTrigger mirrors the Python logic in .scratch/may05_upload_cadence.py.
// Engagement gate fires first: ISPs below the activation target are flagged
// AMPLE regardless of cycle (uploading raw to a cold ISP only inflates the
// saturation tier — must source engaged-only data instead).
func classifyTrigger(cycleDays, activationPct float64, dailyBurn int) (string, string) {
	if dailyBurn == 0 || cycleDays == 0 {
		return "UNKNOWN", "no Welcome burn observed in last 7 days"
	}
	if activationPct < activationTargetPct {
		return "AMPLE", fmt.Sprintf("opener share %.2f%% — engagement cold, do NOT upload raw (cycle %.1fd)", activationPct, cycleDays)
	}
	switch {
	case cycleDays <= 10:
		return "NOW", fmt.Sprintf("cycle %.1fd → ~%.0fd to saturation", cycleDays, cycleDays*4)
	case cycleDays <= 20:
		return "THIS_WEEK", fmt.Sprintf("cycle %.1fd → ~%.0fd to saturation", cycleDays, cycleDays*4)
	case cycleDays <= 40:
		return "TWO_WEEKS", fmt.Sprintf("cycle %.1fd → ~%.0fd to saturation", cycleDays, cycleDays*4)
	case cycleDays <= 90:
		return "ONE_MONTH_PLUS", fmt.Sprintf("cycle %.1fd — comfortable runway", cycleDays)
	default:
		return "AMPLE", fmt.Sprintf("cycle %.0fd — pool not the bottleneck", cycleDays)
	}
}

func triggerOrder(t string) int {
	switch t {
	case "NOW":
		return 0
	case "THIS_WEEK":
		return 1
	case "TWO_WEEKS":
		return 2
	case "ONE_MONTH_PLUS":
		return 3
	case "AMPLE":
		return 4
	default:
		return 5
	}
}

func acRound1(v float64) float64 { return math.Round(v*10) / 10 }
func acRound2(v float64) float64 { return math.Round(v*100) / 100 }
