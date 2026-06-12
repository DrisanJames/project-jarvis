package engine

// Hourly assessment cadence for the consciousness layer (Portal Round-3 §3/§4).
//
// Instead of being a per-event firehose, the consciousness accumulates signals
// in memory and, once per interval (CONSCIOUSNESS_ASSESSMENT_INTERVAL_MINUTES,
// default 60), writes ONE assessment row per scope ("platform" + each
// recipient-ISP family with traffic or signals) to
// mailing_consciousness_assessments. Each assessment carries a posture, the
// deduped notable events observed during the window, per-pipe (ISP × VMTA)
// delivery stats, and pool-aware routing/throttle/exclude/warmup
// RECOMMENDATIONS. Raw per-event thoughts stay in the in-memory ring buffer
// (Consciousness.thoughts, 500 entries) served by the live endpoints — they
// are never persisted to the DB.
//
// All recommendations are ADVISORY ONLY — nothing here mutates sending state.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	isppkg "github.com/ignite/sparkpost-monitor/internal/pkg/isp"
	"github.com/lib/pq"
)

// AssessmentScopePlatform is the cross-ISP rollup scope.
const AssessmentScopePlatform = "platform"

// Recommendation types (v1) — §4.2.
const (
	RecommendationReroute  = "reroute"
	RecommendationThrottle = "throttle"
	RecommendationExclude  = "exclude"
	RecommendationWarmup   = "warmup"
)

// Record-type classification — mirrors internal/api/handlers_deliverability.go
// exactly. Keep both in sync if the taxonomy ever changes.
const (
	assessAttemptedSQL  = `record_type IN ('d','b','t','tq','f')`
	assessDeliveredSQL  = `record_type = 'd'`
	assessDeferredSQL   = `record_type IN ('t','tq')`
	assessComplaintSQL  = `record_type = 'f'`
	assessHardBounceSQL = `record_type = 'b' AND bounce_cat IN ('bad-mailbox','bad-domain','inactive-mailbox','no-answer-from-host','quota-issues','routing-errors')`
	assessRepBlockSQL   = `record_type = 'b' AND bounce_cat IN ('spam-related','policy-related')`
)

// AssessmentRecommendation is one advisory action attached to an hourly
// assessment. The same objects are exposed via
// GET /api/mailing/consciousness/recommendations for Domain Agent reuse.
type AssessmentRecommendation struct {
	Severity   string    `json:"severity"` // info | warn | critical
	Scope      string    `json:"scope"`    // "isp:gmail" / "domain:em.x.com" / "pool:db-gmail-pool" composite display scope
	ISP        string    `json:"isp,omitempty"`
	Domain     string    `json:"domain,omitempty"` // sending domain, when domain-scoped
	Pool       string    `json:"pool,omitempty"`   // pool of the problem source
	Type       string    `json:"type"`             // reroute | throttle | exclude | warmup
	Evidence   string    `json:"evidence"`         // counts + DSN class
	Action     string    `json:"action"`           // names BOTH the bad source and the recommended target pool
	SourceVMTA string    `json:"source_vmta,omitempty"`
	TargetVMTA string    `json:"target_vmta,omitempty"`
	TargetPool string    `json:"target_pool,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// AssessmentPipe is the per-(ISP, VMTA) delivery snapshot inside an assessment.
type AssessmentPipe struct {
	ISP          string  `json:"isp"`
	VMTA         string  `json:"vmta"`
	Pool         string  `json:"pool"`
	Attempted    int64   `json:"attempted"`
	Delivered    int64   `json:"delivered"`
	Deferred     int64   `json:"deferred"`
	HardBounced  int64   `json:"hard_bounced"`
	SoftBounced  int64   `json:"soft_bounced"`
	RepBlocked   int64   `json:"rep_blocked"`
	Complaints   int64   `json:"complaints"`
	DeliveredPct float64 `json:"delivered_pct"`
	DeferralPct  float64 `json:"deferral_pct"`
	HardPct      float64 `json:"hard_pct"`
}

// AssessmentTotals aggregates pipe counts for one scope.
type AssessmentTotals struct {
	Attempted    int64   `json:"attempted"`
	Delivered    int64   `json:"delivered"`
	Deferred     int64   `json:"deferred"`
	HardBounced  int64   `json:"hard_bounced"`
	SoftBounced  int64   `json:"soft_bounced"`
	RepBlocked   int64   `json:"rep_blocked"`
	Complaints   int64   `json:"complaints"`
	DeliveredPct float64 `json:"delivered_pct"`
	DeferralPct  float64 `json:"deferral_pct"`
	HardPct      float64 `json:"hard_pct"`
	RepBlockPct  float64 `json:"rep_block_pct"`
}

// AssessmentSignalCounts summarizes the in-memory thought stream accumulated
// since the previous assessment for one scope.
type AssessmentSignalCounts struct {
	BySeverity map[string]int `json:"by_severity,omitempty"`
	ByType     map[string]int `json:"by_type,omitempty"`
	Total      int            `json:"total"`
}

// AssessmentBody is the JSONB payload of one assessment row.
type AssessmentBody struct {
	WindowMinutes   int                        `json:"window_minutes"`
	Totals          AssessmentTotals           `json:"totals"`
	Signals         AssessmentSignalCounts     `json:"signals"`
	NotableEvents   []string                   `json:"notable_events,omitempty"`
	Pipes           []AssessmentPipe           `json:"pipes,omitempty"`
	Recommendations []AssessmentRecommendation `json:"recommendations"`
}

// ConsciousnessAssessment is one persisted hourly assessment for one scope.
type ConsciousnessAssessment struct {
	ID             string         `json:"id"`
	OrganizationID string         `json:"organization_id"`
	Hour           time.Time      `json:"hour"`
	Scope          string         `json:"scope"`
	Posture        string         `json:"posture"`
	Body           AssessmentBody `json:"body"`
	CreatedAt      time.Time      `json:"created_at"`
}

// ----------------------------------------------------------------------------
// In-memory signal accumulator (drained on every assessment cycle)
// ----------------------------------------------------------------------------

const maxNotableEvents = 40

type assessmentAccumulator struct {
	bySeverity map[string]int
	byType     map[string]int
	total      int
	notableSet map[string]struct{}
	notable    []string
}

func newAssessmentAccumulator() *assessmentAccumulator {
	return &assessmentAccumulator{
		bySeverity: make(map[string]int),
		byType:     make(map[string]int),
		notableSet: make(map[string]struct{}),
	}
}

// accumulateForAssessment folds a thought into the per-scope staging buffers.
// Called from think() — must stay cheap and allocation-light.
func (c *Consciousness) accumulateForAssessment(t Thought) {
	c.assessMu.Lock()
	defer c.assessMu.Unlock()
	if c.assessAcc == nil {
		c.assessAcc = make(map[string]*assessmentAccumulator)
	}
	scopes := [2]string{AssessmentScopePlatform, ""}
	n := 1
	if t.ISP != "" {
		scopes[1] = strings.ToLower(string(t.ISP))
		n = 2
	}
	sev := t.Severity
	if sev == "" {
		sev = "info"
	}
	for i := 0; i < n; i++ {
		acc := c.assessAcc[scopes[i]]
		if acc == nil {
			acc = newAssessmentAccumulator()
			c.assessAcc[scopes[i]] = acc
		}
		acc.bySeverity[sev]++
		acc.byType[t.Type]++
		acc.total++
		// Notable events: warnings/criticals, deduped by content (repeated
		// WONTs / deferral storms / blocks collapse to one line).
		if sev == "warning" || sev == "critical" || t.Type == "warning" {
			if _, dup := acc.notableSet[t.Content]; !dup && len(acc.notable) < maxNotableEvents {
				acc.notableSet[t.Content] = struct{}{}
				acc.notable = append(acc.notable, t.Content)
			}
		}
	}
}

// SetAssessmentStore wires the DB + org used for hourly assessment writes.
// Without it the accumulator still runs (bounded) but nothing is persisted.
func (c *Consciousness) SetAssessmentStore(db *sql.DB, orgID string) {
	c.assessMu.Lock()
	c.assessmentDB = db
	c.assessmentOrgID = orgID
	c.assessMu.Unlock()
}

// assessmentIntervalFromEnv reads CONSCIOUSNESS_ASSESSMENT_INTERVAL_MINUTES
// (default 60) so the cadence can be tightened during incidents.
func assessmentIntervalFromEnv() time.Duration {
	if v := os.Getenv("CONSCIOUSNESS_ASSESSMENT_INTERVAL_MINUTES"); v != "" {
		if mins, err := strconv.Atoi(v); err == nil && mins > 0 {
			return time.Duration(mins) * time.Minute
		}
	}
	return 60 * time.Minute
}

// assessmentLoop writes one assessment per scope every interval. A short
// initial delay produces a first assessment soon after boot so the screen is
// never empty for a full hour after a deploy.
func (c *Consciousness) assessmentLoop(ctx context.Context) {
	interval := c.assessmentInterval
	if interval <= 0 {
		interval = 60 * time.Minute
	}
	initial := 5 * time.Minute
	if initial > interval {
		initial = interval
	}
	timer := time.NewTimer(initial)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-c.stopCh:
		return
	case <-timer.C:
		c.runAssessmentCycle(ctx)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.runAssessmentCycle(ctx)
		}
	}
}

// runAssessmentCycle drains the signal accumulator, reads the last window of
// PMTA accounting, builds recommendations, and persists one row per scope.
func (c *Consciousness) runAssessmentCycle(ctx context.Context) {
	c.assessMu.Lock()
	db := c.assessmentDB
	orgID := c.assessmentOrgID
	drained := c.assessAcc
	c.assessAcc = make(map[string]*assessmentAccumulator)
	c.assessMu.Unlock()

	if db == nil || orgID == "" {
		return
	}

	windowMin := int(c.assessmentInterval.Minutes())
	if windowMin < 5 {
		windowMin = 5
	}

	qctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	pipes, err := queryAssessmentPipes(qctx, db, windowMin)
	if err != nil {
		log.Printf("[consciousness] assessment pipe query failed: %v", err)
		return
	}
	dsnClass := queryTopDSNClass(qctx, db, windowMin)
	domainStats := queryDomainDeferrals(qctx, db, windowMin)
	baseline := queryBaselineAttempted(qctx, db, windowMin)
	famCtx := queryFamilyVolumeContexts(qctx, db, orgID)

	now := time.Now().UTC()
	recs := buildAssessmentRecommendations(now, pipes, dsnClass, domainStats, baseline, famCtx)

	// Group pipes + recommendations by ISP scope.
	pipesByScope := make(map[string][]AssessmentPipe)
	for _, p := range pipes {
		pipesByScope[p.ISP] = append(pipesByScope[p.ISP], p)
	}
	recsByScope := make(map[string][]AssessmentRecommendation)
	for _, r := range recs {
		recsByScope[r.ISP] = append(recsByScope[r.ISP], r)
	}

	// Scope set: platform + every ISP family with traffic or signals.
	scopeSet := map[string]struct{}{AssessmentScopePlatform: {}}
	for s := range pipesByScope {
		scopeSet[s] = struct{}{}
	}
	for s := range drained {
		scopeSet[s] = struct{}{}
	}

	hour := now.Truncate(time.Hour)
	written := 0
	for scope := range scopeSet {
		var body AssessmentBody
		body.WindowMinutes = windowMin
		if scope == AssessmentScopePlatform {
			body.Pipes = pipes
			body.Recommendations = recs
		} else {
			body.Pipes = pipesByScope[scope]
			body.Recommendations = recsByScope[scope]
		}
		if body.Recommendations == nil {
			body.Recommendations = []AssessmentRecommendation{}
		}
		body.Totals = sumPipeTotals(body.Pipes)
		if acc := drained[scope]; acc != nil {
			body.Signals = AssessmentSignalCounts{
				BySeverity: acc.bySeverity,
				ByType:     acc.byType,
				Total:      acc.total,
			}
			body.NotableEvents = acc.notable
		}
		posture := computeScopePosture(body)

		raw, mErr := json.Marshal(body)
		if mErr != nil {
			log.Printf("[consciousness] assessment marshal failed for %s: %v", scope, mErr)
			continue
		}
		if _, iErr := db.ExecContext(qctx, `
			INSERT INTO mailing_consciousness_assessments (organization_id, hour, scope, posture, body)
			VALUES ($1, $2, $3, $4, $5)`,
			orgID, hour, scope, posture, raw); iErr != nil {
			log.Printf("[consciousness] assessment insert failed for %s: %v", scope, iErr)
			continue
		}
		written++
	}

	// Narrate into the in-memory stream only (never the DB).
	c.think(Thought{
		Type: "reflection",
		Content: fmt.Sprintf("Hourly assessment written: %d scopes, %d recommendations across the last %d minutes.",
			written, len(recs), windowMin),
		Severity: "info",
	})
	log.Printf("[consciousness] hourly assessment: %d scopes, %d recommendations (window %dm)", written, len(recs), windowMin)
}

func sumPipeTotals(pipes []AssessmentPipe) AssessmentTotals {
	var t AssessmentTotals
	for _, p := range pipes {
		t.Attempted += p.Attempted
		t.Delivered += p.Delivered
		t.Deferred += p.Deferred
		t.HardBounced += p.HardBounced
		t.SoftBounced += p.SoftBounced
		t.RepBlocked += p.RepBlocked
		t.Complaints += p.Complaints
	}
	if t.Attempted > 0 {
		t.DeliveredPct = round1(100 * float64(t.Delivered) / float64(t.Attempted))
		t.DeferralPct = round1(100 * float64(t.Deferred) / float64(t.Attempted))
		t.HardPct = round1(100 * float64(t.HardBounced) / float64(t.Attempted))
		t.RepBlockPct = round1(100 * float64(t.RepBlocked) / float64(t.Attempted))
	}
	return t
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

// computeScopePosture derives the posture label from delivery totals,
// recommendations, and accumulated signals. Deterministic ladder.
func computeScopePosture(body AssessmentBody) string {
	criticalRecs, warnRecs := 0, 0
	for _, r := range body.Recommendations {
		switch r.Severity {
		case "critical":
			criticalRecs++
		case "warn":
			warnRecs++
		}
	}
	t := body.Totals
	if t.Attempted == 0 && body.Signals.Total == 0 {
		return "idle"
	}
	if criticalRecs > 0 || t.RepBlockPct > 10 || t.HardPct > 5 {
		return "critical"
	}
	if warnRecs > 0 || t.DeferralPct > 25 || body.Signals.BySeverity["critical"] > 0 {
		return "degraded"
	}
	if t.DeferralPct > 10 || body.Signals.BySeverity["warning"] > 0 {
		return "watch"
	}
	return "healthy"
}

// ----------------------------------------------------------------------------
// PMTA accounting reads (once per cycle — NOT the hot path)
// ----------------------------------------------------------------------------

func queryAssessmentPipes(ctx context.Context, db *sql.DB, windowMin int) ([]AssessmentPipe, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(NULLIF(recipient_isp, ''), 'other'),
		       COALESCE(NULLIF(vmta, ''), '(none)'),
		       COALESCE(NULLIF(pool, ''), ''),
		       COUNT(*) FILTER (WHERE %s),
		       COUNT(*) FILTER (WHERE %s),
		       COUNT(*) FILTER (WHERE %s),
		       COUNT(*) FILTER (WHERE %s),
		       COUNT(*) FILTER (WHERE record_type = 'b'),
		       COUNT(*) FILTER (WHERE %s),
		       COUNT(*) FILTER (WHERE %s)
		FROM pmta_acct_raw
		WHERE received_at > NOW() - ($1 || ' minutes')::interval
		GROUP BY 1, 2, 3
		ORDER BY 1, 4 DESC`,
		assessAttemptedSQL, assessDeliveredSQL, assessDeferredSQL,
		assessHardBounceSQL, assessRepBlockSQL, assessComplaintSQL),
		strconv.Itoa(windowMin))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pipes []AssessmentPipe
	var unresolved []string
	for rows.Next() {
		var p AssessmentPipe
		var allBounce int64
		if err := rows.Scan(&p.ISP, &p.VMTA, &p.Pool, &p.Attempted, &p.Delivered,
			&p.Deferred, &p.HardBounced, &allBounce, &p.RepBlocked, &p.Complaints); err != nil {
			continue
		}
		p.SoftBounced = allBounce - p.HardBounced - p.RepBlocked
		if p.SoftBounced < 0 {
			p.SoftBounced = 0
		}
		if p.Attempted > 0 {
			p.DeliveredPct = round1(100 * float64(p.Delivered) / float64(p.Attempted))
			p.DeferralPct = round1(100 * float64(p.Deferred) / float64(p.Attempted))
			p.HardPct = round1(100 * float64(p.HardBounced) / float64(p.Attempted))
		}
		if p.Pool == "" {
			p.Pool = poolFromVMTAName(p.VMTA)
		}
		if p.Pool == "" && p.VMTA != "(none)" {
			unresolved = append(unresolved, p.VMTA)
		}
		pipes = append(pipes, p)
	}

	// Last-resort pool resolution from the IP registry (VMTA names derive from
	// the IP hostname's first label — see pmta_campaign_planner.go).
	if len(unresolved) > 0 {
		if found := lookupPoolsForVMTAs(ctx, db, unresolved); len(found) > 0 {
			for i := range pipes {
				if pipes[i].Pool == "" {
					pipes[i].Pool = found[pipes[i].VMTA]
				}
			}
		}
	}
	return pipes, nil
}

// queryTopDSNClass returns the dominant bounce/deferral classification per
// recipient ISP over the window — the "DSN class" in evidence lines.
func queryTopDSNClass(ctx context.Context, db *sql.DB, windowMin int) map[string]string {
	out := make(map[string]string)
	rows, err := db.QueryContext(ctx, `
		SELECT isp, klass FROM (
			SELECT COALESCE(NULLIF(recipient_isp, ''), 'other') AS isp,
			       COALESCE(NULLIF(bounce_cat, ''), NULLIF(dsn_status, ''), 'unknown') AS klass,
			       ROW_NUMBER() OVER (PARTITION BY COALESCE(NULLIF(recipient_isp, ''), 'other') ORDER BY COUNT(*) DESC) AS rn
			FROM pmta_acct_raw
			WHERE received_at > NOW() - ($1 || ' minutes')::interval
			  AND record_type IN ('b', 't', 'tq')
			GROUP BY 1, 2
		) ranked WHERE rn = 1`,
		strconv.Itoa(windowMin))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var isp, klass string
		if rows.Scan(&isp, &klass) == nil {
			out[isp] = klass
		}
	}
	return out
}

type domainDeferralStat struct {
	ISP         string
	Domain      string
	Attempted   int64
	Deferred    int64
	DeferralPct float64
}

// queryDomainDeferrals aggregates ISP × sending-domain deferral rates for the
// throttle (deferral storm) recommendation.
func queryDomainDeferrals(ctx context.Context, db *sql.DB, windowMin int) []domainDeferralStat {
	var out []domainDeferralStat
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(NULLIF(recipient_isp, ''), 'other'),
		       COALESCE(NULLIF(split_part(sender, '@', 2), ''), '(unknown)'),
		       COUNT(*) FILTER (WHERE %s),
		       COUNT(*) FILTER (WHERE %s)
		FROM pmta_acct_raw
		WHERE received_at > NOW() - ($1 || ' minutes')::interval
		GROUP BY 1, 2`,
		assessAttemptedSQL, assessDeferredSQL),
		strconv.Itoa(windowMin))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s domainDeferralStat
		if rows.Scan(&s.ISP, &s.Domain, &s.Attempted, &s.Deferred) != nil {
			continue
		}
		if s.Attempted > 0 {
			s.DeferralPct = round1(100 * float64(s.Deferred) / float64(s.Attempted))
		}
		out = append(out, s)
	}
	return out
}

// ----------------------------------------------------------------------------
// Established daily-send baselines per ISP family (send-baselines semantics)
// ----------------------------------------------------------------------------
//
// PROVENANCE: these queries are a compact, family-rolled-up copy of the
// send-baselines engine in internal/api/send_baselines.go (computeBaselines +
// HandleSendBaselinesToday). The engine package cannot import internal/api, so
// the minimal shared semantics are reproduced here — keep them aligned:
//   - baseline_daily_sends = trimmed mean (drop single min+max at ≥5 samples)
//     of daily sends over days WITH sends in the trailing 28 full days from
//     mailing_domain_agent_scorecard, TODAY EXCLUDED (partial day skews it).
//   - today_sends = MAX(scorecard today row, live count of today's 'sent'
//     mailing_tracking_events) — the scorecard worker rebuilds every 6h and
//     can lag; live events cover the gap.
//   - open quality = recent-7d human-open rate vs prior-window rate;
//     "collapsed" when recent < 60% of prior (openCollapseFraction).
// Both mailing_domain_agent_scorecard.isp and pmta_acct_raw.recipient_isp are
// stamped via isp.GroupFromDomain, so family keys join directly.

const (
	recPacingOverBaselinePct = 150.0 // today > 150% of baseline ⇒ pacing problem, not reputation
	recOpenCollapseFraction  = 0.60  // mirrors openCollapseFraction in internal/api/send_baselines.go
	recBaselineWindowDays    = 28    // mirrors the send-baselines default window
	recRecentWindowDays      = 7     // mirrors recentWindowDays in internal/api/send_baselines.go
)

// Open-quality states carried on deferral-storm evidence lines.
const (
	openQualityHeld      = "held"
	openQualityCollapsed = "collapsed"
	openQualityUnknown   = "unknown"
)

// familyVolumeContext is the per-ISP-family volume + engagement context used
// to attribute a deferral storm to pacing vs ISP rate limiting vs reputation.
type familyVolumeContext struct {
	BaselineDailySends float64 // trimmed-mean daily sends, today excluded
	HasBaseline        bool
	TodaySends         int64
	RecentOpenRate     float64 // recent-7d human_opens / sends
	PriorOpenRate      float64 // prior-window human_opens / sends
	OpenQuality        string  // held | collapsed | unknown
}

// trimmedMeanDaily mirrors trimmedMean in internal/api/send_baselines.go:
// mean of days-with-sends, dropping the single min and max when there are at
// least 5 samples (robust against one blowout / one trickle day).
func trimmedMeanDaily(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	if len(sorted) >= 5 {
		sorted = sorted[1 : len(sorted)-1]
	}
	var sum float64
	for _, v := range sorted {
		sum += v
	}
	return round1(sum / float64(len(sorted)))
}

// queryFamilyVolumeContexts builds the baseline/today/open-quality context for
// every ISP family with scorecard history or sends today. One bulk pass per
// assessment cycle (NOT the hot path). Errors degrade to an empty map — the
// recommendation builder then falls back to the pre-baseline behavior.
func queryFamilyVolumeContexts(ctx context.Context, db *sql.DB, orgID string) map[string]familyVolumeContext {
	out := make(map[string]familyVolumeContext)
	if orgID == "" {
		return out
	}

	// 1) Trailing 28 full days (today excluded): daily sends + human opens per
	//    family, split recent-7d vs prior for the open-quality comparison.
	type dailyAgg struct {
		sends, humanOpens int64
		recent            bool
	}
	perFamily := make(map[string][]dailyAgg)
	rows, err := db.QueryContext(ctx, `
		SELECT LOWER(isp),
		       SUM(sends), SUM(human_opens),
		       (day >= CURRENT_DATE - $2::int) AS recent
		FROM mailing_domain_agent_scorecard
		WHERE organization_id = $1
		  AND day >= CURRENT_DATE - $3::int
		  AND day <  CURRENT_DATE
		GROUP BY LOWER(isp), day`,
		orgID, recRecentWindowDays, recBaselineWindowDays)
	if err != nil {
		log.Printf("[consciousness] family baseline query failed: %v", err)
		return out
	}
	for rows.Next() {
		var fam string
		var d dailyAgg
		if rows.Scan(&fam, &d.sends, &d.humanOpens, &d.recent) == nil {
			perFamily[fam] = append(perFamily[fam], d)
		}
	}
	rows.Close()

	for fam, days := range perFamily {
		var fc familyVolumeContext
		var active []float64
		var recentSends, recentOpens, priorSends, priorOpens int64
		for _, d := range days {
			if d.sends > 0 {
				active = append(active, float64(d.sends))
			}
			if d.recent {
				recentSends += d.sends
				recentOpens += d.humanOpens
			} else {
				priorSends += d.sends
				priorOpens += d.humanOpens
			}
		}
		fc.BaselineDailySends = trimmedMeanDaily(active)
		fc.HasBaseline = fc.BaselineDailySends > 0
		if recentSends > 0 {
			fc.RecentOpenRate = float64(recentOpens) / float64(recentSends)
		}
		if priorSends > 0 {
			fc.PriorOpenRate = float64(priorOpens) / float64(priorSends)
		}
		switch {
		case recentSends == 0 || priorSends == 0 || fc.PriorOpenRate == 0:
			fc.OpenQuality = openQualityUnknown
		case fc.RecentOpenRate >= recOpenCollapseFraction*fc.PriorOpenRate:
			fc.OpenQuality = openQualityHeld
		default:
			fc.OpenQuality = openQualityCollapsed
		}
		out[fam] = fc
	}

	// 2) Today's sends per family: MAX(scorecard today, live tracking events).
	todaySC := make(map[string]int64)
	scRows, err := db.QueryContext(ctx, `
		SELECT LOWER(isp), SUM(sends)
		FROM mailing_domain_agent_scorecard
		WHERE organization_id = $1 AND day = CURRENT_DATE
		GROUP BY 1`, orgID)
	if err == nil {
		for scRows.Next() {
			var fam string
			var n int64
			if scRows.Scan(&fam, &n) == nil {
				todaySC[fam] += n
			}
		}
		scRows.Close()
	} else {
		log.Printf("[consciousness] family today-scorecard query failed: %v", err)
	}

	todayLive := make(map[string]int64)
	liveRows, err := db.QueryContext(ctx, `
		SELECT COALESCE(LOWER(recipient_domain), ''), COUNT(*)
		FROM mailing_tracking_events
		WHERE organization_id = $1
		  AND event_at >= CURRENT_DATE
		  AND event_type = 'sent'
		GROUP BY 1`, orgID)
	if err == nil {
		for liveRows.Next() {
			var rdom string
			var n int64
			if liveRows.Scan(&rdom, &n) == nil {
				todayLive[isppkg.GroupFromDomain(rdom)] += n
			}
		}
		liveRows.Close()
	} else {
		log.Printf("[consciousness] family today-live query failed: %v", err)
	}

	for fam, n := range todaySC {
		fc := out[fam]
		if fc.OpenQuality == "" {
			fc.OpenQuality = openQualityUnknown
		}
		if n > fc.TodaySends {
			fc.TodaySends = n
		}
		out[fam] = fc
	}
	for fam, n := range todayLive {
		fc := out[fam]
		if fc.OpenQuality == "" {
			fc.OpenQuality = openQualityUnknown
		}
		if n > fc.TodaySends {
			fc.TodaySends = n
		}
		out[fam] = fc
	}
	return out
}

// queryBaselineAttempted returns the trailing-7-day per-window average
// attempted count per ISP (excluding the current window) for the warmup
// pacing recommendation.
func queryBaselineAttempted(ctx context.Context, db *sql.DB, windowMin int) map[string]float64 {
	out := make(map[string]float64)
	totalMin := 7 * 24 * 60
	windows := float64(totalMin-windowMin) / float64(windowMin)
	if windows < 1 {
		windows = 1
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(NULLIF(recipient_isp, ''), 'other'), COUNT(*) FILTER (WHERE %s)
		FROM pmta_acct_raw
		WHERE received_at > NOW() - INTERVAL '7 days'
		  AND received_at <= NOW() - ($1 || ' minutes')::interval
		GROUP BY 1`, assessAttemptedSQL),
		strconv.Itoa(windowMin))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var isp string
		var n int64
		if rows.Scan(&isp, &n) == nil {
			out[isp] = float64(n) / windows
		}
	}
	return out
}

// ----------------------------------------------------------------------------
// VMTA → pool resolution
// ----------------------------------------------------------------------------

// vmtaCodeToISP maps the short ISP code embedded in VMTA names
// ((v)mta-{prefix}-{code}{n}, e.g. mta-qf-gn6, vmta-db-gm1) to the engine ISP
// group, which then maps to the {prefix}-{suffix}-pool convention via
// isppkg.PoolSuffix. "gn"/"gen" routes through the general pool.
var vmtaCodeToISP = map[string]string{
	"gm":   "gmail",
	"yh":   "yahoo",
	"ya":   "yahoo",
	"ao":   "aol",
	"ms":   "microsoft",
	"msft": "microsoft",
	"ol":   "microsoft",
	"ap":   "apple",
	"ic":   "apple",
	"cm":   "comcast",
	"cc":   "comcast",
	"at":   "att",
	"sb":   "sbcglobal",
	"cx":   "cox",
	"ch":   "charter",
}

// poolFromVMTAName derives the pool name from the VMTA naming convention.
// Returns "" when the name doesn't match — callers fall back to the observed
// accounting pool or an IP-registry lookup.
func poolFromVMTAName(vmta string) string {
	name := strings.ToLower(strings.TrimSpace(vmta))
	name = strings.TrimPrefix(name, "vmta-")
	name = strings.TrimPrefix(name, "mta-")
	parts := strings.Split(name, "-")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	prefix := parts[0]
	code := strings.TrimRight(parts[1], "0123456789")
	switch code {
	case "ses":
		return prefix + "-ses-pool"
	case "gn", "gen", "general":
		return prefix + "-general-pool"
	}
	if ispName, ok := vmtaCodeToISP[code]; ok {
		return fmt.Sprintf("%s-%s-pool", prefix, isppkg.PoolSuffix(ispName))
	}
	return ""
}

// lookupPoolsForVMTAs resolves VMTA → pool via the IP registry: VMTA names
// derive from the IP hostname's first label, and mailing_ip_addresses rows
// carry pool_id → mailing_ip_pools.name.
func lookupPoolsForVMTAs(ctx context.Context, db *sql.DB, vmtas []string) map[string]string {
	out := make(map[string]string)
	rows, err := db.QueryContext(ctx, `
		SELECT split_part(ip.hostname, '.', 1), COALESCE(p.name, '')
		FROM mailing_ip_addresses ip
		LEFT JOIN mailing_ip_pools p ON p.id = ip.pool_id
		WHERE split_part(ip.hostname, '.', 1) = ANY($1)`,
		pq.Array(vmtas))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var vmta, pool string
		if rows.Scan(&vmta, &pool) == nil && pool != "" {
			out[vmta] = pool
		}
	}
	return out
}

// ----------------------------------------------------------------------------
// Recommendation engine (§4.2: reroute / throttle / exclude / warmup)
// ----------------------------------------------------------------------------

const (
	recMinAttempts        = 50   // pipe must carry real traffic to be judged
	recStormDeferralPct   = 50.0 // deferral storm threshold
	recExcludeRepBlockPct = 25.0 // hard-block pattern (e.g. charter)
	recExcludeHardPct     = 20.0
	recWarmupSpikeFactor  = 3.0 // window volume vs trailing-7d baseline
	recWarmupMinVolume    = 1000
)

func displayPool(vmta, pool string) string {
	if pool != "" {
		return pool
	}
	return "pool unknown"
}

func buildAssessmentRecommendations(
	now time.Time,
	pipes []AssessmentPipe,
	dsnClass map[string]string,
	domainStats []domainDeferralStat,
	baseline map[string]float64,
	famCtx map[string]familyVolumeContext,
) []AssessmentRecommendation {
	recs := []AssessmentRecommendation{}

	// Families whose family-wide deferral storm already got an attribution rec
	// (pacing / rate-limit / reputation) — domain-scoped storm recs for the
	// same family are suppressed so one condition yields one attribution.
	stormHandled := map[string]bool{}

	byISP := make(map[string][]AssessmentPipe)
	totalsByISP := make(map[string]AssessmentTotals)
	for _, p := range pipes {
		byISP[p.ISP] = append(byISP[p.ISP], p)
	}
	for isp, ps := range byISP {
		totalsByISP[isp] = sumPipeTotals(ps)
	}

	isps := make([]string, 0, len(byISP))
	for isp := range byISP {
		isps = append(isps, isp)
	}
	sort.Strings(isps)

	for _, ispName := range isps {
		ispPipes := byISP[ispName]
		klass := dsnClass[ispName]
		if klass == "" {
			klass = "unknown"
		}

		qualified := ispPipes[:0:0]
		for _, p := range ispPipes {
			if p.Attempted >= recMinAttempts {
				qualified = append(qualified, p)
			}
		}

		// (c) exclude/suppress family — hard-block pattern (e.g. charter).
		t := totalsByISP[ispName]
		if t.Attempted >= recMinAttempts && (t.RepBlockPct > recExcludeRepBlockPct || t.HardPct > recExcludeHardPct) {
			recs = append(recs, AssessmentRecommendation{
				Severity: "critical",
				Scope:    "isp:" + ispName,
				ISP:      ispName,
				Type:     RecommendationExclude,
				Evidence: fmt.Sprintf("%d attempted in window: %d reputation-blocked (%.1f%%), %d hard (%.1f%%); dominant DSN class: %s",
					t.Attempted, t.RepBlocked, t.RepBlockPct, t.HardBounced, t.HardPct, klass),
				Action: fmt.Sprintf("%s-family showing a hard-block pattern — exclude/suppress the %s family from upcoming waves until the block clears (advisory).",
					ispName, ispName),
				CreatedAt: now,
			})
		}

		if len(qualified) == 0 {
			continue
		}
		sort.Slice(qualified, func(i, j int) bool { return qualified[i].DeferralPct < qualified[j].DeferralPct })
		best := qualified[0]

		// (b) family-wide deferral storm — every qualified pipe storming.
		// Attribution consults the ESTABLISHED daily baseline + open quality
		// (send-baselines semantics, see queryFamilyVolumeContexts) before
		// blaming reputation:
		//   (2a) today > 150% of baseline           ⇒ PACING (warmup), not reputation
		//   (2b) within baseline + opens held        ⇒ ISP-side rate limiting (soft throttle)
		//   (2c) within baseline + opens collapsed,
		//        unknown opens, or no baseline       ⇒ reputation throttle (legacy wording)
		// Per-pipe asymmetry (one bad pipe, healthy siblings) never reaches
		// here — that stays the reroute path below.
		allStorming := true
		for _, p := range qualified {
			if p.DeferralPct <= recStormDeferralPct {
				allStorming = false
				break
			}
		}
		if allStorming {
			stormHandled[ispName] = true
			fv, hasCtx := famCtx[ispName]

			// Evidence always carries: today-vs-baseline volume, deferral %,
			// open-quality status — so the operator can audit the attribution.
			deferralEv := fmt.Sprintf("every pipe deferring >%.0f%% (%d attempted, %d deferred, %.1f%%)",
				recStormDeferralPct, t.Attempted, t.Deferred, t.DeferralPct)
			volumeEv := "volume today vs baseline unavailable (no scorecard history)"
			openEv := "open quality: " + openQualityUnknown
			pacePct := 0.0
			if hasCtx {
				openEv = fmt.Sprintf("open quality: %s (recent human-open rate %.2f%% vs prior %.2f%%)",
					fv.OpenQuality, 100*fv.RecentOpenRate, 100*fv.PriorOpenRate)
				if fv.HasBaseline {
					pacePct = 100 * float64(fv.TodaySends) / fv.BaselineDailySends
					volumeEv = fmt.Sprintf("volume today %d vs established baseline %.0f/day (%.0f%% of baseline)",
						fv.TodaySends, fv.BaselineDailySends, pacePct)
				}
			}
			evidence := fmt.Sprintf("%s; %s; %s; dominant DSN class: %s", volumeEv, deferralEv, openEv, klass)

			switch {
			case hasCtx && fv.HasBaseline && pacePct > recPacingOverBaselinePct:
				// (2a) Pacing: the storm coincides with a volume spike well over
				// the established baseline. Severity "high" per spec maps to
				// "critical" in the info|warn|critical taxonomy. The generic
				// window-spike warmup rec (d) is skipped via `continue` below —
				// one family never gets both for the same condition.
				recs = append(recs, AssessmentRecommendation{
					Severity: "critical",
					Scope:    "isp:" + ispName,
					ISP:      ispName,
					Type:     RecommendationWarmup,
					Evidence: evidence,
					Action: fmt.Sprintf("%s-family deferral storm driven by volume %.0f%% over baseline (%d vs %.0f/day) — return to baseline pace; deferrals should clear as volume normalizes.",
						ispName, pacePct, fv.TodaySends, fv.BaselineDailySends),
					CreatedAt: now,
				})
			case hasCtx && fv.HasBaseline && fv.OpenQuality == openQualityHeld:
				// (2b) Normal volume, healthy engagement ⇒ ISP-side rate
				// limiting, not reputation damage. Soft hold, not a cut.
				recs = append(recs, AssessmentRecommendation{
					Severity: "warn",
					Scope:    "isp:" + ispName,
					ISP:      ispName,
					Type:     RecommendationThrottle,
					Evidence: evidence,
					Action: fmt.Sprintf("%s-family deferral storm at normal volume with healthy engagement — likely ISP-side rate limiting; hold %s volume flat, do not grow.",
						ispName, ispName),
					CreatedAt: now,
				})
			default:
				// (2c) Normal volume + collapsed (or unknowable) open quality ⇒
				// the existing reputation throttle.
				recs = append(recs, AssessmentRecommendation{
					Severity: "critical",
					Scope:    "isp:" + ispName,
					ISP:      ispName,
					Type:     RecommendationThrottle,
					Evidence: evidence,
					Action: fmt.Sprintf("%s-family deferral storm across all pipes — throttle down %s volume; this is reputation, not routing.",
						ispName, ispName),
					CreatedAt: now,
				})
			}
			continue
		}

		// (a) pool reroute per recipient family — name BOTH the bad source and
		// the recommended target pool.
		for _, p := range qualified[1:] {
			if p.DeferralPct > 2*best.DeferralPct && p.DeferralPct-best.DeferralPct > 5 {
				sev := "warn"
				if p.DeferralPct > recStormDeferralPct {
					sev = "critical"
				}
				recs = append(recs, AssessmentRecommendation{
					Severity:   sev,
					Scope:      fmt.Sprintf("isp:%s pool:%s", ispName, displayPool(p.VMTA, p.Pool)),
					ISP:        ispName,
					Pool:       p.Pool,
					Type:       RecommendationReroute,
					SourceVMTA: p.VMTA,
					TargetVMTA: best.VMTA,
					TargetPool: best.Pool,
					Evidence: fmt.Sprintf("%d attempted via %s (%s): %d deferred (%.1f%%) vs %.1f%% on %s; dominant DSN class: %s",
						p.Attempted, p.VMTA, displayPool(p.VMTA, p.Pool), p.Deferred, p.DeferralPct, best.DeferralPct, best.VMTA, klass),
					Action: fmt.Sprintf("%s-family failing from %s (%s, %.1f%% deferral) — route %s-family via %s (%s, %.1f%%).",
						ispName, p.VMTA, displayPool(p.VMTA, p.Pool), p.DeferralPct,
						ispName, best.VMTA, displayPool(best.VMTA, best.Pool), best.DeferralPct),
					CreatedAt: now,
				})
			}
		}

		// (d) warm-up pacing note — volume spike vs trailing baseline.
		if base, ok := baseline[ispName]; ok && base > 0 &&
			float64(t.Attempted) > recWarmupSpikeFactor*base && t.Attempted >= recWarmupMinVolume {
			recs = append(recs, AssessmentRecommendation{
				Severity: "info",
				Scope:    "isp:" + ispName,
				ISP:      ispName,
				Type:     RecommendationWarmup,
				Evidence: fmt.Sprintf("%d attempted this window vs trailing-7d baseline of %.0f per window (%.1fx)",
					t.Attempted, base, float64(t.Attempted)/base),
				Action: fmt.Sprintf("%s volume is spiking well above baseline — hold warm-up pacing discipline; ISPs punish step-function ramps.",
					ispName),
				CreatedAt: now,
			})
		}
	}

	// (b) throttle — ISP × sending-domain deferral storms (domain-scoped).
	for _, ds := range domainStats {
		if ds.Attempted < recMinAttempts || ds.DeferralPct <= recStormDeferralPct {
			continue
		}
		// Skip if the whole family already got a storm attribution above
		// (pacing warmup, rate-limit hold, or reputation throttle) — one
		// condition, one attribution.
		if stormHandled[ds.ISP] {
			continue
		}
		klass := dsnClass[ds.ISP]
		if klass == "" {
			klass = "unknown"
		}
		recs = append(recs, AssessmentRecommendation{
			Severity: "warn",
			Scope:    fmt.Sprintf("isp:%s domain:%s", ds.ISP, ds.Domain),
			ISP:      ds.ISP,
			Domain:   ds.Domain,
			Type:     RecommendationThrottle,
			Evidence: fmt.Sprintf("%s → %s: %d attempted, %d deferred (%.1f%%); dominant DSN class: %s",
				ds.Domain, ds.ISP, ds.Attempted, ds.Deferred, ds.DeferralPct, klass),
			Action: fmt.Sprintf("Deferral storm on %s × %s — throttle down this lane for the rest of the window.",
				ds.ISP, ds.Domain),
			CreatedAt: now,
		})
	}

	// Critical first, then warn, then info; stable within severity.
	sevRank := map[string]int{"critical": 0, "warn": 1, "info": 2}
	sort.SliceStable(recs, func(i, j int) bool {
		return sevRank[recs[i].Severity] < sevRank[recs[j].Severity]
	})
	return recs
}
