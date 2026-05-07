package api

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"time"

	"github.com/lib/pq"
)

// VersionWelcomeAudienceHealth is the response version for /api/mailing/analytics/welcome-audience-health.
//
//	1.0 (2026-05-04): initial version. Surfaces the welcome eligibility pool, the
//	saturation exclusion segment, sends-remaining and days-remaining sunset
//	buckets, daily burn rate, and a projected next-list-upload date so the
//	operator can see audience exhaustion at a glance.
//	1.1 (2026-05-07): saturation threshold lowered 8 → 4 to redirect daily
//	Welcome volume from low-yield late-stage non-openers (~54% of May 7 picks
//	were prior=5–8 cohort with ~2% activation) toward never-mailed and warm-1-4
//	subscribers. Added Vertical-Finance-Engaged to the pool list (186k net-new
//	subs, 99% never-mailed) to absorb the supply contraction. Bucket key names
//	preserved for frontend-compat; ranges reinterpret against the new threshold.
const VersionWelcomeAudienceHealth = "1.1"

// welcomePoolSegmentIDs are the source segments that feed the Welcome slots.
// Must mirror WELCOME_POOL_SEGMENT_IDS in refresh_welcome_saturation_segment.py
// — if either side adds/removes a segment, update both files together.
var welcomePoolSegmentIDs = []string{
	"a03b2527-040a-49ae-9bdb-4c8fdab6e96e", // Mailgun-Engaged-Master           (welcome-newsletter / welcome-att / welcome-offer-gmail / welcome-offer-yahoo-aol)
	"9743a42c-9e85-4415-bbad-f2ed1498f40f", // TruGreen-Attribits-Apr2026-DB    (brand-scoped)
	"e18fa7db-a45f-4c18-baf5-466c3d891d61", // TruGreen-Attribits-Apr2026-HT    (brand-scoped)
	"d71c1802-13e8-43b4-ad82-a156f0de9310", // TruGreen-Attribits-Apr2026-MH    (brand-scoped)
	"0f1784d3-d2d5-4891-8b5f-9e2e396cdbba", // TruGreen-Attribits-Apr2026-QF    (brand-scoped)
	"2ed35605-5428-43da-ade7-891777649f88", // Welcome-Yahoo-Priority-Engaged   (yahoo, dormant in rotation; included for retirement coverage)
	"31e4f5d7-0afb-4f9f-a962-e8f197e9c383", // Vertical-Finance-Engaged         (added 2026-05-07 to absorb threshold-4 supply contraction)
}

// welcomeSaturationSegmentID is the standing exclusion segment id (operator
// rule 2026-05-04). Wired into every Welcome campaign's exclusion_segments.
const welcomeSaturationSegmentID = "00000000-0000-0000-0000-0000005a7ed8"

// welcomeSaturationThreshold is the operator-locked threshold. Subscribers
// with prior_sends > THRESHOLD AND last_open_at IS NULL are retired into the
// saturation segment. Lowered 8 → 4 on 2026-05-07.
const welcomeSaturationThreshold = 4

// HandleWelcomeAudienceHealth answers the operator's "when do I need to upload
// more data?" question with one self-contained call.
//
// Computes (single pass over the welcome eligibility pool plus a window scan
// of mailing_message_log):
//
//  1. Pool composition — how many subscribers in the pool, how many already
//     retired (in the saturation segment), how many exempt (opened ≥1), how
//     many remain mailable.
//  2. Sends-remaining-until-sunset buckets — concrete, no cadence assumption.
//  3. Days-remaining-until-sunset buckets — uses average sends/active-sub/day
//     over the last 14 days as the cadence estimate.
//  4. Per-segment breakdown — per source segment: total members, mailable,
//     retired, never-mailed.
//  5. Daily burn — rows added to the saturation segment via daily refreshes
//     over the last 14 days, plus the daily count of unique never-mailed
//     subscribers picked by Welcome campaigns over the same window.
//  6. Projected next-list-upload-needed date — never_mailed_remaining ÷
//     daily_burn (with a soft floor on burn so we don't divide by zero on a
//     quiet day). Tells the operator when the never-mailed pool will fall
//     below a working threshold (~50k).
//
// All counts are anchored to confirmed subscribers only.
//
// Response shape (high level, full schema visible in the JSON):
//
//	{
//	  "api_version": "1.1",
//	  "as_of": "2026-05-07T15:30:00Z",
//	  "saturation_segment": { "id": "00000000-...-5a7ed8", "name": "Welcome-Saturated-MailedOver4-NeverOpened", "threshold": 4, "members": 68896 },
//	  "pool":           { "total": 503600, "exempt_opened": 8460, "retired_saturated": 68896, "mailable": 426244 },
//	  "sends_remaining": [ { "bucket":"never_mailed", "label":"...", "subscribers": 381461 }, ... ],
//	  "days_remaining":  [ { "bucket":"this_week",   "label":"...", "subscribers": 3833  }, ... ],
//	  "by_segment":      [ { "segment_id":"...", "name":"Mailgun-...","total":104079,"never_mailed":73564,"retired":7102,"mailable":97000 }, ... ],
//	  "daily_burn":      { "avg_never_mailed_picked_per_day": 6650, "avg_retirements_per_day": 412, "by_day": [...] },
//	  "projection":      { "never_mailed_remaining": 381461, "days_until_upload_needed": 49, "upload_recommended_date": "2026-06-25", "trigger_threshold": 50000 }
//	}
func (s *AdvancedMailingService) HandleWelcomeAudienceHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	asOf := time.Now().UTC()

	type bucket struct {
		Bucket      string `json:"bucket"`
		Label       string `json:"label"`
		Subscribers int    `json:"subscribers"`
		PriorMin    int    `json:"prior_sends_min,omitempty"`
		PriorMax    int    `json:"prior_sends_max,omitempty"`
	}

	type segmentRow struct {
		SegmentID    string `json:"segment_id"`
		Name         string `json:"name"`
		Total        int    `json:"total"`
		NeverMailed  int    `json:"never_mailed"`
		Retired      int    `json:"retired"`
		Exempt       int    `json:"exempt_opened"`
		Mailable     int    `json:"mailable"`
		ShareOfPool  float64 `json:"share_of_pool_pct"`
	}

	type dayBurn struct {
		Date          string `json:"date"`
		NeverMailed   int    `json:"never_mailed_picked"`
		Retirements   int    `json:"retirements"`
	}

	// ─── 1. Saturation segment metadata ────────────────────────────────────
	var satMembers int
	var satName string
	var satLastCalc *time.Time
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(name,'') , COALESCE(subscriber_count,0), last_calculated_at
		FROM mailing_segments WHERE id = $1::uuid
	`, welcomeSaturationSegmentID).Scan(&satName, &satMembers, &satLastCalc); err != nil {
		log.Printf("[welcome-audience-health] saturation segment lookup error: %v", err)
		// Don't fail the whole request — just leave members=0
	}

	// ─── 2. Build the per-subscriber send-count snapshot for the pool ──────
	// Single CTE computes prior_sends + opened-flag for every distinct
	// confirmed subscriber across all 6 inclusion segments. Indexed columns
	// (subscriber_id, segment_id, last_open_at) — runs in ~10s on production.
	rows, err := s.db.QueryContext(ctx, `
		WITH eligible AS (
			SELECT DISTINCT s.id, s.last_open_at
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
			COALESCE(s.prior_sends, 0) AS prior_sends,
			(e.last_open_at IS NOT NULL) AS opened
		FROM eligible e
		LEFT JOIN sends s ON s.subscriber_id = e.id
	`, pq.Array(welcomePoolSegmentIDs))
	if err != nil {
		log.Printf("[welcome-audience-health] pool snapshot error: %v", err)
		respondError(w, http.StatusInternalServerError, "welcome_audience_health_snapshot_failed")
		return
	}
	defer rows.Close()

	// Tally everything in one pass.
	//
	// Bucket boundaries are derived from welcomeSaturationThreshold so a future
	// adjustment of the threshold continues to bucket cleanly without needing
	// to retune the cases. Frontend-facing keys (1_send_away, 2_to_3_away,
	// 4_to_5_away, 6_to_8_away, never_mailed) are preserved across threshold
	// changes; their semantic ranges shift with the threshold.
	//
	// At threshold = 4 (current):
	//   1_send_away  = prior == 4 (next send retires)
	//   2_to_3_away  = prior in {2, 3}
	//   4_to_5_away  = prior == 1 (single-touch newcomers; 4 sends remain)
	//   6_to_8_away  = empty (no priors fall here)
	//   never_mailed = prior == 0 (5 sends remain to retirement)
	var (
		poolTotal       int
		exemptOpened    int
		retiredOverThr  int
		neverMailed     int
		oneSendAway     int // prior == threshold
		twoToThreeAway  int // prior in [threshold-2, threshold-1]
		fourToFiveAway  int // prior in [threshold-4, threshold-3] (clamped ≥ 1)
		sixToEightAway  int // prior in [1, threshold-5] (empty when threshold ≤ 5)
	)
	for rows.Next() {
		var prior int
		var opened bool
		if err := rows.Scan(&prior, &opened); err != nil {
			log.Printf("[welcome-audience-health] scan error: %v", err)
			continue
		}
		poolTotal++
		switch {
		case opened:
			exemptOpened++
		case prior > welcomeSaturationThreshold:
			retiredOverThr++
		case prior == 0:
			neverMailed++
		case prior == welcomeSaturationThreshold:
			oneSendAway++
		case prior >= welcomeSaturationThreshold-2:
			twoToThreeAway++
		case prior >= welcomeSaturationThreshold-4:
			fourToFiveAway++
		default:
			sixToEightAway++
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("[welcome-audience-health] rows.Err: %v", err)
	}
	mailable := poolTotal - exemptOpened - retiredOverThr

	maxRemaining := welcomeSaturationThreshold + 1
	twoThreeMin := welcomeSaturationThreshold - 2
	if twoThreeMin < 1 {
		twoThreeMin = 1
	}
	fourFiveMin := welcomeSaturationThreshold - 4
	if fourFiveMin < 1 {
		fourFiveMin = 1
	}
	fourFiveMax := welcomeSaturationThreshold - 3
	if fourFiveMax < fourFiveMin {
		fourFiveMax = fourFiveMin
	}
	sendsRemaining := []bucket{
		{Bucket: "retired",        Label: "Already retired (in saturation segment)",                                 Subscribers: retiredOverThr, PriorMin: welcomeSaturationThreshold + 1, PriorMax: -1},
		{Bucket: "exempt_opened",  Label: "Exempt — opened at least once (not subject)",                             Subscribers: exemptOpened},
		{Bucket: "1_send_away",    Label: "1 send remaining — next send retires",                                    Subscribers: oneSendAway,    PriorMin: welcomeSaturationThreshold, PriorMax: welcomeSaturationThreshold},
		{Bucket: "2_to_3_away",    Label: "2–3 sends remaining",                                                     Subscribers: twoToThreeAway, PriorMin: twoThreeMin, PriorMax: welcomeSaturationThreshold - 1},
		{Bucket: "4_to_5_away",    Label: "4–5 sends remaining",                                                     Subscribers: fourToFiveAway, PriorMin: fourFiveMin, PriorMax: fourFiveMax},
		{Bucket: "6_to_8_away",    Label: "6+ sends remaining (low usage)",                                          Subscribers: sixToEightAway, PriorMin: 1, PriorMax: welcomeSaturationThreshold - 5},
		{Bucket: "never_mailed",   Label:  fmt.Sprintf("Never mailed — max runway (%d sends)", maxRemaining),       Subscribers: neverMailed,    PriorMin: 0, PriorMax: 0},
	}

	// ─── 3. Cadence over last 14 days (sends per active subscriber per day) ─
	// Bounded by the pool to keep the calc representative.
	var sends14d, activeSubs14d int
	if err := s.db.QueryRowContext(ctx, `
		WITH pool AS (
			SELECT DISTINCT subscriber_id FROM mailing_segment_members
			WHERE segment_id = ANY($1::uuid[])
		)
		SELECT COUNT(*), COUNT(DISTINCT subscriber_id)
		FROM mailing_message_log
		WHERE created_at >= NOW() - INTERVAL '14 days'
		  AND subscriber_id IN (SELECT subscriber_id FROM pool)
	`, pq.Array(welcomePoolSegmentIDs)).Scan(&sends14d, &activeSubs14d); err != nil {
		log.Printf("[welcome-audience-health] cadence query error: %v", err)
	}
	cadenceRate := 0.0 // sends per active subscriber per day
	if activeSubs14d > 0 {
		cadenceRate = float64(sends14d) / float64(activeSubs14d) / 14.0
	}

	// ─── 4. Days-remaining buckets ────────────────────────────────────────
	// Translate the sends-remaining buckets to calendar days using cadenceRate.
	daysFor := func(sendsRemaining float64) float64 {
		if cadenceRate <= 0 {
			return math.Inf(1)
		}
		return sendsRemaining / cadenceRate
	}
	classifyDays := func(d float64) string {
		switch {
		case d <= 1.5:
			return "0_to_1_days"
		case d <= 3.5:
			return "2_to_3_days"
		case d <= 7.5:
			return "this_week"
		case d <= 14.5:
			return "next_week"
		case d <= 30.5:
			return "this_month"
		default:
			return "long_runway"
		}
	}
	dayBucketCounts := map[string]int{}
	addToDayBucket := func(subs, sendsRemain int) {
		if subs == 0 {
			return
		}
		dayBucketCounts[classifyDays(daysFor(float64(sendsRemain)))] += subs
	}
	// Conservative midpoints — use the lower bound so day-bucket projections
	// don't over-promise runway. neverMailed gets the full max-remaining since
	// it's the only bucket whose membership is unambiguous (prior == 0).
	addToDayBucket(oneSendAway, 1)
	addToDayBucket(twoToThreeAway, 2)
	addToDayBucket(fourToFiveAway, fourFiveMin)
	if welcomeSaturationThreshold > 5 {
		addToDayBucket(sixToEightAway, 6)
	}
	addToDayBucket(neverMailed, maxRemaining)

	dayBucketLabel := map[string]string{
		"0_to_1_days":  "Retires today or tomorrow",
		"2_to_3_days":  "Retires in 2–3 days",
		"this_week":    "Retires this week (4–7 days)",
		"next_week":    "Retires next week (8–14 days)",
		"this_month":   "Retires this month (15–30 days)",
		"long_runway":  "Long runway (30+ days)",
	}
	daysRemaining := []bucket{}
	for _, key := range []string{"0_to_1_days", "2_to_3_days", "this_week", "next_week", "this_month", "long_runway"} {
		daysRemaining = append(daysRemaining, bucket{
			Bucket: key, Label: dayBucketLabel[key], Subscribers: dayBucketCounts[key],
		})
	}

	// ─── 5. Per-segment breakdown ─────────────────────────────────────────
	segRows, err := s.db.QueryContext(ctx, `
		WITH per_seg AS (
			SELECT m.segment_id,
				COUNT(DISTINCT s.id) AS total,
				COUNT(DISTINCT s.id) FILTER (WHERE s.last_open_at IS NOT NULL) AS exempt
			FROM mailing_segment_members m
			JOIN mailing_subscribers s ON s.id = m.subscriber_id
			WHERE m.segment_id = ANY($1::uuid[]) AND s.status = 'confirmed'
			GROUP BY m.segment_id
		),
		retired_per_seg AS (
			SELECT m.segment_id, COUNT(DISTINCT m.subscriber_id) AS retired
			FROM mailing_segment_members m
			WHERE m.segment_id = ANY($1::uuid[])
			  AND m.subscriber_id IN (SELECT subscriber_id FROM mailing_segment_members WHERE segment_id = $2::uuid)
			GROUP BY m.segment_id
		),
		never_mailed_per_seg AS (
			SELECT m.segment_id, COUNT(DISTINCT m.subscriber_id) AS never_mailed
			FROM mailing_segment_members m
			JOIN mailing_subscribers s ON s.id = m.subscriber_id
			WHERE m.segment_id = ANY($1::uuid[])
			  AND s.status = 'confirmed'
			  AND s.last_open_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM mailing_message_log ml WHERE ml.subscriber_id = m.subscriber_id)
			GROUP BY m.segment_id
		)
		SELECT ps.segment_id, COALESCE(seg.name,'?') AS name, ps.total,
			COALESCE(nm.never_mailed,0) AS never_mailed,
			COALESCE(rs.retired,0)      AS retired,
			ps.exempt
		FROM per_seg ps
		LEFT JOIN mailing_segments    seg ON seg.id = ps.segment_id
		LEFT JOIN retired_per_seg     rs  ON rs.segment_id = ps.segment_id
		LEFT JOIN never_mailed_per_seg nm ON nm.segment_id = ps.segment_id
		ORDER BY ps.total DESC
	`, pq.Array(welcomePoolSegmentIDs), welcomeSaturationSegmentID)
	if err != nil {
		log.Printf("[welcome-audience-health] per-segment query error: %v", err)
	}
	bySegment := []segmentRow{}
	if segRows != nil {
		defer segRows.Close()
		for segRows.Next() {
			var sr segmentRow
			if err := segRows.Scan(&sr.SegmentID, &sr.Name, &sr.Total, &sr.NeverMailed, &sr.Retired, &sr.Exempt); err != nil {
				log.Printf("[welcome-audience-health] segment scan: %v", err)
				continue
			}
			sr.Mailable = sr.Total - sr.Exempt - sr.Retired
			if poolTotal > 0 {
				sr.ShareOfPool = math.Round(float64(sr.Total)/float64(poolTotal)*1000) / 10
			}
			bySegment = append(bySegment, sr)
		}
	}

	// ─── 6. Daily burn (last 14 days) ─────────────────────────────────────
	// Two parts: (a) unique never-mailed-at-time-of-send subscribers picked by
	// Welcome campaigns per day, and (b) approximate daily retirements
	// derived as count of subscribers whose 9th send fell on each calendar day.
	burnRows, err := s.db.QueryContext(ctx, `
		WITH welcome_picks AS (
			SELECT DATE_TRUNC('day', c.scheduled_at)::date AS day,
				COUNT(DISTINCT r.subscriber_id) FILTER (
					WHERE NOT EXISTS (
						SELECT 1 FROM mailing_message_log ml
						WHERE ml.subscriber_id = r.subscriber_id
						  AND ml.created_at < c.scheduled_at
					)
				) AS never_mailed_picked
			FROM mailing_campaigns c
			JOIN mailing_campaign_plan_recipients r ON r.campaign_id = c.id
			WHERE c.scheduled_at >= NOW() - INTERVAL '14 days'
			  AND c.scheduled_at < NOW() + INTERVAL '1 day'
			  AND c.name ILIKE '%Welcome%'
			  AND c.status NOT IN ('cancelled')
			GROUP BY 1
		),
		retirements AS (
			SELECT DATE_TRUNC('day', threshold_send.created_at)::date AS day,
				COUNT(*) AS retirements
			FROM (
				SELECT subscriber_id, created_at,
					ROW_NUMBER() OVER (PARTITION BY subscriber_id ORDER BY created_at) AS rn
				FROM mailing_message_log
				WHERE created_at >= NOW() - INTERVAL '21 days'
			) threshold_send
			JOIN mailing_subscribers s ON s.id = threshold_send.subscriber_id
			WHERE threshold_send.rn = $2
			  AND s.last_open_at IS NULL
			  AND threshold_send.created_at >= NOW() - INTERVAL '14 days'
			GROUP BY 1
		)
		SELECT COALESCE(p.day, r.day)::text AS day,
			COALESCE(p.never_mailed_picked, 0),
			COALESCE(r.retirements, 0)
		FROM welcome_picks p FULL OUTER JOIN retirements r USING(day)
		ORDER BY 1
	`, pq.Array(welcomePoolSegmentIDs), welcomeSaturationThreshold+1)
	if err != nil {
		log.Printf("[welcome-audience-health] burn query error: %v", err)
	}
	dailyBurn := []dayBurn{}
	var sumNeverMailed, sumRetirements int
	if burnRows != nil {
		defer burnRows.Close()
		for burnRows.Next() {
			var db dayBurn
			if err := burnRows.Scan(&db.Date, &db.NeverMailed, &db.Retirements); err != nil {
				log.Printf("[welcome-audience-health] burn scan: %v", err)
				continue
			}
			dailyBurn = append(dailyBurn, db)
			sumNeverMailed += db.NeverMailed
			sumRetirements += db.Retirements
		}
	}
	avgNeverMailedPerDay := 0
	avgRetirementsPerDay := 0
	if n := len(dailyBurn); n > 0 {
		avgNeverMailedPerDay = sumNeverMailed / n
		avgRetirementsPerDay = sumRetirements / n
	}

	// ─── 7. Projection — when does the never-mailed pool fall below threshold? ─
	const uploadTriggerThreshold = 50000
	reachableNeverMailed := neverMailed // shorthand
	burnRate := math.Max(float64(avgNeverMailedPerDay), 200.0) // soft floor: don't divide by a quiet 0
	daysUntilUploadNeeded := math.MaxInt32
	uploadRecommendedDate := ""
	if reachableNeverMailed > uploadTriggerThreshold {
		daysUntilUploadNeeded = int(math.Ceil(float64(reachableNeverMailed-uploadTriggerThreshold) / burnRate))
		uploadRecommendedDate = asOf.AddDate(0, 0, daysUntilUploadNeeded).Format("2006-01-02")
	} else {
		// Already at or below threshold — upload now.
		daysUntilUploadNeeded = 0
		uploadRecommendedDate = asOf.Format("2006-01-02")
	}

	// ─── Response envelope ────────────────────────────────────────────────
	resp := map[string]interface{}{
		"api_version": VersionWelcomeAudienceHealth,
		"as_of":       asOf.Format(time.RFC3339),
		"saturation_segment": map[string]interface{}{
			"id":                  welcomeSaturationSegmentID,
			"name":                satName,
			"threshold":           welcomeSaturationThreshold,
			"members":             satMembers,
			"last_calculated_at":  satLastCalc,
		},
		"pool": map[string]int{
			"total":             poolTotal,
			"exempt_opened":     exemptOpened,
			"retired_saturated": retiredOverThr,
			"mailable":          mailable,
			"never_mailed":      neverMailed,
		},
		"sends_remaining": sendsRemaining,
		"days_remaining":  daysRemaining,
		"by_segment":      bySegment,
		"cadence": map[string]interface{}{
			"window_days":                       14,
			"sends_total":                       sends14d,
			"active_subscribers":                activeSubs14d,
			"sends_per_active_sub_per_day":      math.Round(cadenceRate*1000) / 1000,
		},
		"daily_burn": map[string]interface{}{
			"avg_never_mailed_picked_per_day": avgNeverMailedPerDay,
			"avg_retirements_per_day":         avgRetirementsPerDay,
			"by_day":                          dailyBurn,
		},
		"projection": map[string]interface{}{
			"never_mailed_remaining":     reachableNeverMailed,
			"trigger_threshold":          uploadTriggerThreshold,
			"burn_rate_per_day":          int(math.Round(burnRate)),
			"days_until_upload_needed":   daysUntilUploadNeeded,
			"upload_recommended_date":    uploadRecommendedDate,
		},
		"pool_segment_ids": welcomePoolSegmentIDs,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("[welcome-audience-health] encode error: %v", err)
	}
}
