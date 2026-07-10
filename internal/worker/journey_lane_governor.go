package worker

// JourneyLaneGovernor is the dynamic concentration/routing layer of the
// click funnel (Unit D, 2026-07-10).
//
// Each tick (hourly) it walks every offer lane in mailing_offer_journey_map
// and:
//  1. Computes 30-day lane stats — enrollments / active passes / conversions
//     from mailing_journey_enrollments (keyed by enrollment_offer_id) plus
//     reminder touches from mailing_message_log via the offer's deterministic
//     shadow campaign (shadowCampaignID, journey_clickdrip_sender.go) — and
//     writes them to lane_stats / lane_stats_updated_at.
//  2. Computes a routing recommendation: a CPA/CPL lane with a real sample
//     (>= CLICKDRIP_GOVERNOR_MIN_SAMPLE enrollments in 30d) and ZERO
//     conversions is burning funnel passes on an offer that doesn't convert —
//     recommend redirecting its clicks into the proven target offer
//     (CLICKDRIP_REDIRECT_TARGET_OFFER, default Sam's Club 420). CPM lanes are
//     always 'active' (CPM pays on delivered volume; conversions are not the
//     objective). The target offer itself is never redirected.
//  3. Enforcement is ADVISE-ONLY by default: the recommendation lands in
//     routing_recommendation for the operator to review. Only with
//     CLICKDRIP_DYNAMIC_ROUTING=enforce does the governor also write
//     routing_state / redirect_offer_id, which the click-tracking scanner
//     (new clicks) and the event enroller (pending triggers) honor. A lane an
//     operator manually set to 'paused_auto' is never overwritten — manual
//     pause wins over the governor even in enforce mode.
//
// Multi-instance safety: the server runs on multiple ECS tasks. The journey
// workers' cursor-row pattern doesn't fit here (no cursor to advance), so the
// tick takes a pg advisory lock. It is taken on a DEDICATED connection —
// acquire and release on the same session — because pg advisory locks are
// session-scoped: the pool-level pattern (internal/pkg/distlock PGAdvisoryLock)
// can release on a different connection than it acquired on, permanently
// wedging an interval worker like this one. The tick is also idempotent
// (recomputes and rewrites the same rows), so a double-fire is harmless.

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultLaneGovernorInterval — lane stats move on click/conversion time
// scales; hourly is plenty.
const DefaultLaneGovernorInterval = 1 * time.Hour

// laneGovernorLockKey is the advisory-lock key serializing ticks across ECS
// tasks (hashed via hashtext, same derivation style as distlock).
const laneGovernorLockKey = "journey-lane-governor"

// JourneyLaneGovernor is the worker handle. Construct via
// NewJourneyLaneGovernor, then Start(ctx) once at boot. Stop() is safe to
// call multiple times.
type JourneyLaneGovernor struct {
	db       *sql.DB
	interval time.Duration
	stopChan chan struct{}
	stopOnce sync.Once

	totalLanes           int64
	totalRecommendations int64
	totalEnforced        int64
	totalErrors          int64
}

// NewJourneyLaneGovernor wires the worker.
func NewJourneyLaneGovernor(db *sql.DB) *JourneyLaneGovernor {
	return &JourneyLaneGovernor{
		db:       db,
		interval: DefaultLaneGovernorInterval,
		stopChan: make(chan struct{}),
	}
}

// WithInterval overrides the tick cadence (tests / incidents).
func (w *JourneyLaneGovernor) WithInterval(d time.Duration) *JourneyLaneGovernor {
	if d > 0 {
		w.interval = d
	}
	return w
}

// Start runs the tick loop until ctx is cancelled or Stop() is called.
func (w *JourneyLaneGovernor) Start(ctx context.Context) {
	go func() {
		log.Printf("JourneyLaneGovernor: started (interval=%s, mode=%s)", w.interval, laneGovernorMode())
		// Run once immediately so lane stats populate right after boot.
		w.tick(ctx)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.tick(ctx)
			case <-w.stopChan:
				log.Println("JourneyLaneGovernor: stopped")
				return
			case <-ctx.Done():
				log.Println("JourneyLaneGovernor: context cancelled, stopping")
				return
			}
		}
	}()
}

// Stop terminates the tick loop. Idempotent.
func (w *JourneyLaneGovernor) Stop() {
	w.stopOnce.Do(func() { close(w.stopChan) })
}

// Stats returns lifetime counters for observability.
func (w *JourneyLaneGovernor) Stats() (lanes, recommendations, enforced, errors int64) {
	return atomic.LoadInt64(&w.totalLanes),
		atomic.LoadInt64(&w.totalRecommendations),
		atomic.LoadInt64(&w.totalEnforced),
		atomic.LoadInt64(&w.totalErrors)
}

// governedLane is one mailing_offer_journey_map row's governor-relevant slice.
type governedLane struct {
	offerID      string
	payoutType   string
	routingState string
	createdAt time.Time
}

// tick recomputes lane stats + recommendations for every offer lane.
func (w *JourneyLaneGovernor) tick(ctx context.Context) {
	tickCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// Single-writer guard: pg advisory lock on a dedicated connection so
	// acquire and release land on the same session (see file header).
	conn, err := w.db.Conn(tickCtx)
	if err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyLaneGovernor: acquire conn: %v", err)
		return
	}
	defer conn.Close()
	var locked bool
	if err := conn.QueryRowContext(tickCtx,
		`SELECT pg_try_advisory_lock(hashtext($1)::bigint)`, laneGovernorLockKey).Scan(&locked); err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyLaneGovernor: advisory lock: %v", err)
		return
	}
	if !locked {
		return // another instance is governing this tick
	}
	defer func() {
		// Background context: release even when tickCtx already expired.
		_, _ = conn.ExecContext(context.Background(),
			`SELECT pg_advisory_unlock(hashtext($1)::bigint)`, laneGovernorLockKey)
	}()

	lanes, err := w.loadLanes(tickCtx)
	if err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyLaneGovernor: load lanes: %v", err)
		return
	}

	minSample := laneGovernorMinSample()
	targetOffer := laneGovernorRedirectTarget()
	enforce := laneGovernorEnforceEnabled()

	recommendations := 0
	enforced := 0
	for _, lane := range lanes {
		rec, changed, err := w.governLane(tickCtx, lane, minSample, targetOffer, enforce)
		if err != nil {
			atomic.AddInt64(&w.totalErrors, 1)
			log.Printf("JourneyLaneGovernor: lane %s: %v", lane.offerID, err)
			continue
		}
		if strings.HasPrefix(rec, "redirect:") {
			recommendations++
		}
		if changed {
			enforced++
		}
	}

	atomic.AddInt64(&w.totalLanes, int64(len(lanes)))
	atomic.AddInt64(&w.totalRecommendations, int64(recommendations))
	atomic.AddInt64(&w.totalEnforced, int64(enforced))
	log.Printf("JourneyLaneGovernor: tick lanes=%d recommendations=%d enforced=%d (mode=%s)",
		len(lanes), recommendations, enforced, laneGovernorMode())
}

// loadLanes returns every offer lane in the journey map.
func (w *JourneyLaneGovernor) loadLanes(ctx context.Context) ([]governedLane, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT everflow_offer_id, COALESCE(payout_type,'UNKNOWN'), COALESCE(routing_state,'active'),
		       COALESCE(created_at, NOW())
		FROM mailing_offer_journey_map
		ORDER BY everflow_offer_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []governedLane
	for rows.Next() {
		var l governedLane
		if err := rows.Scan(&l.offerID, &l.payoutType, &l.routingState, &l.createdAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// governLane computes one lane's 30d stats + recommendation and writes them.
// Returns the recommendation and whether an enforced routing_state change was
// written.
func (w *JourneyLaneGovernor) governLane(ctx context.Context, lane governedLane, minSample int, targetOffer string, enforce bool) (string, bool, error) {
	// Conversions are counted over a WIDER window than enrollments: CPA/CPL
	// conversions routinely lag the click by weeks, so a 30d-vs-30d
	// comparison would flag young/slow lanes that genuinely convert
	// (reviewer finding D, 2026-07-10). Window: CLICKDRIP_GOVERNOR_CONV_WINDOW_DAYS.
	convWindowDays := laneGovernorConvWindowDays()
	var enrollments30, active, conversionsWin int
	if err := w.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE enrolled_at > NOW() - INTERVAL '30 days'),
			COUNT(*) FILTER (WHERE status = 'active'),
			COUNT(*) FILTER (WHERE converted_at > NOW() - ($2 || ' days')::interval)
		FROM mailing_journey_enrollments
		WHERE enrollment_offer_id = $1
	`, lane.offerID, convWindowDays).Scan(&enrollments30, &active, &conversionsWin); err != nil {
		return "", false, err
	}

	// Reminder touches ride the offer's deterministic shadow campaign
	// (shadowCampaignID, journey_clickdrip_sender.go — UUID v5 on the shared
	// clickDripShadowNamespace), so a primary-key-friendly campaign_id filter
	// counts them without any name scan.
	var touches30 int
	if err := w.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_message_log
		WHERE campaign_id = $1 AND sent_at > NOW() - INTERVAL '30 days'
	`, shadowCampaignID(lane.offerID)).Scan(&touches30); err != nil {
		return "", false, err
	}

	// A lane younger than the conversion window cannot have proven itself
	// either way — never recommend redirecting it.
	laneMature := time.Since(lane.createdAt) >= time.Duration(convWindowDays)*24*time.Hour
	rec := laneRecommendation(lane.offerID, lane.payoutType, enrollments30, conversionsWin, minSample, targetOffer, laneMature)

	statsJSON, _ := json.Marshal(map[string]interface{}{
		"enrollments_30d": enrollments30,
		"active":          active,
		"conversions_window_d":  convWindowDays,
		"conversions_in_window": conversionsWin,
		"lane_mature":           laneMature,
		"touches_30d":     touches30,
		"payout_type":     lane.payoutType,
		"computed_at":     time.Now().UTC().Format(time.RFC3339),
	})

	// Manual pause always wins: never enforce over an operator-set
	// paused_auto lane (stats + recommendation still refresh).
	if enforce && lane.routingState != "paused_auto" {
		newState, newRedirect := "active", ""
		if strings.HasPrefix(rec, "redirect:") {
			newState = "redirect"
			newRedirect = strings.TrimPrefix(rec, "redirect:")
		}
		if _, err := w.db.ExecContext(ctx, `
			UPDATE mailing_offer_journey_map
			SET lane_stats=$2::jsonb, lane_stats_updated_at=NOW(),
			    routing_recommendation=$3, routing_state=$4, redirect_offer_id=$5,
			    updated_at=NOW()
			WHERE everflow_offer_id=$1
		`, lane.offerID, string(statsJSON), rec, newState, newRedirect); err != nil {
			return rec, false, err
		}
		return rec, newState != lane.routingState, nil
	}

	if _, err := w.db.ExecContext(ctx, `
		UPDATE mailing_offer_journey_map
		SET lane_stats=$2::jsonb, lane_stats_updated_at=NOW(),
		    routing_recommendation=$3, updated_at=NOW()
		WHERE everflow_offer_id=$1
	`, lane.offerID, string(statsJSON), rec); err != nil {
		return rec, false, err
	}
	return rec, false, nil
}

// laneRecommendation is the pure recommendation rule. Only conversion-paid
// lanes (CPA/CPL) with a statistically meaningful 30d sample and ZERO
// conversions get a redirect recommendation; CPM lanes pay on delivered
// volume so they are always 'active'; the redirect target itself is never
// redirected (no cycles).
func laneRecommendation(offerID, payoutType string, enrollments30, conversionsWin, minSample int, targetOffer string, laneMature bool) string {
	if (payoutType == "CPA" || payoutType == "CPL") &&
		offerID != targetOffer &&
		laneMature &&
		enrollments30 >= minSample &&
		conversionsWin == 0 {
		return "redirect:" + targetOffer
	}
	return "active"
}

// laneGovernorConvWindowDays is the conversion look-back window (days) for
// the dead-lane rule, and the minimum lane age before a redirect can be
// recommended. Default 60 (2x the enrollment window — CPA/CPL conversions
// lag clicks by weeks). Unset/garbage/<=0 -> 60.
func laneGovernorConvWindowDays() int {
	v := strings.TrimSpace(os.Getenv("CLICKDRIP_GOVERNOR_CONV_WINDOW_DAYS"))
	if v == "" {
		return 60
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 60
	}
	return n
}

// laneGovernorMinSample is the minimum 30d enrollment sample before a
// zero-conversion CPA/CPL lane earns a redirect recommendation. Unset,
// empty, non-numeric, or <= 0 → default 50.
func laneGovernorMinSample() int {
	v := strings.TrimSpace(os.Getenv("CLICKDRIP_GOVERNOR_MIN_SAMPLE"))
	if v == "" {
		return 50
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 50
	}
	return n
}

// laneGovernorRedirectTarget is the everflow offer id dead lanes redirect
// into. Default "420" (Sam's Club — the proven consumer-signal target).
func laneGovernorRedirectTarget() string {
	if v := strings.TrimSpace(os.Getenv("CLICKDRIP_REDIRECT_TARGET_OFFER")); v != "" {
		return v
	}
	return "420"
}

// laneGovernorEnforceEnabled: CLICKDRIP_DYNAMIC_ROUTING=enforce makes the
// governor write routing_state / redirect_offer_id from its recommendation;
// any other value (default) is advise-only.
func laneGovernorEnforceEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("CLICKDRIP_DYNAMIC_ROUTING")), "enforce")
}

// laneGovernorMode is the human-readable mode for log lines.
func laneGovernorMode() string {
	if laneGovernorEnforceEnabled() {
		return "enforce"
	}
	return "advise"
}
