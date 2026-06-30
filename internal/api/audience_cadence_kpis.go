package api

// Audience Cadence KPIs + ISP Doctrines — backend for the repurposed
// Audience Cadence screen (web/src/components/mailing/components/AudienceCadenceByCell.tsx).
//
// Operator ask (2026-06-12): "dive into the KPIs of an audience member for a
// given ISP as we know it: establish evolving statistics about the amount of
// messages needed to convert in this ISP environment. The doctrine should
// live here."
//
// Two halves:
//
//  1. GET /api/mailing/audience-cadence/kpis?days=60
//     Per ISP family, the evolving "messages-to-X" statistics:
//       - messages-to-first-engagement: subscribers whose FIRST
//         opened/clicked tracking event falls inside the window (event_at
//         bounded — engagement history before the window is not scanned, so
//         this is an approximation stated in method_note), capped at
//         cadenceEngagedSampleLimit subscribers. Prior sends counted from
//         mailing_message_log via the LOWER(email) index
//         (idx_message_log_lower_email_sent).
//       - messages-to-conversion: terminal event = mailing_offer_suppressions
//         reason='converted' (subscriber_id, suppressed_at). Small cohort,
//         exact (no sampling).
//       - conversion_per_10k_sends: window converters / window sends × 10000.
//       - 4-week weekly trend of msgs_to_convert_p50 (week of conversion).
//     Percentiles via percentile_cont in Postgres.
//
//  2. mailing_isp_doctrines — the operator's ISP doctrine canon, seeded from
//     docs/{APPLE,MICROSOFT,YAHOO,COMCAST,AOL}_SENDING_DOCTRINE.md +
//     docs/DOCTRINE_KPI_SCORECARD_JUN12.md (gmail/att lanes), operator-
//     editable via PUT.
//
// ISP families: gmail, microsoft, yahoo, aol, comcast, apple, att, charter,
// other. NOTE: aol is split out of the yahoo family deliberately — operator
// directive 2026-06-11 (AOL doctrine, Law header): AOL is operated as ITS OWN
// LANE, and its doctrine row needs a matching KPI row.

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	// VersionAudienceCadenceKPIs is the response version for
	// /api/mailing/audience-cadence/kpis.
	VersionAudienceCadenceKPIs = "1.0"

	// cadenceEngagedSampleLimit caps the messages-to-first-engagement cohort.
	// The per-subscriber LEFT JOIN to mailing_message_log (LOWER(email)) scales
	// super-linearly as the table grows: measured 2026-06-23 on prod — 5k≈9s,
	// 10k≈38s, 50k TIMED OUT past the 60s budget (the "messages-to-engage query
	// failed" screen-blocker). 5k samples is statistically ample for the per-ISP
	// percentiles and leaves headroom under load.
	cadenceEngagedSampleLimit = 5000

	// cadenceMaxDays bounds the ?days= window.
	cadenceMaxDays     = 90
	cadenceDefaultDays = 14

	// cadenceTrendDays bounds the weekly msgs-to-convert trend (4 weeks).
	cadenceTrendDays = 28

	// cadenceEngageBudget bounds the heavy messages-to-first-engagement cohort
	// scan independently of the overall 60s budget. The per-subscriber join to
	// mailing_message_log (LOWER(email)) is the only section that can blow the
	// budget — and does, until idx_message_log_lower_email_sent is built (see
	// migrations/051_message_log_lower_email_index.sql). Capping it here leaves
	// headroom for the cheap convert/trend/sends queries, so a slow engage join
	// degrades to a "computing" partial response instead of 500ing the screen.
	cadenceEngageBudget = 40 * time.Second
)

// cadenceISPOrder is the canonical display order for ISP families.
var cadenceISPOrder = []string{
	"apple", "microsoft", "gmail", "yahoo", "aol", "comcast", "att", "charter", "other",
}

// ispFamilyCase builds the SQL CASE expression mapping an email-domain
// expression to an ISP family. domExpr is repeated verbatim (keep it cheap,
// e.g. SPLIT_PART(LOWER(s.email),'@',2)).
func ispFamilyCase(domExpr string) string {
	const tpl = `CASE
		WHEN {d} IN ('gmail.com','googlemail.com') THEN 'gmail'
		WHEN {d} IN ('outlook.com','hotmail.com','live.com','msn.com')
			OR {d} LIKE 'outlook.%' OR {d} LIKE 'hotmail.%' OR {d} LIKE 'live.%' THEN 'microsoft'
		WHEN {d} IN ('aol.com','aim.com') OR {d} LIKE 'aol.%' THEN 'aol'
		WHEN {d} IN ('yahoo.com','ymail.com','rocketmail.com','verizon.net','frontier.com','frontiernet.net')
			OR {d} LIKE 'yahoo.%' THEN 'yahoo'
		WHEN {d} = 'comcast.net' OR {d} LIKE 'comcast.%' THEN 'comcast'
		WHEN {d} IN ('icloud.com','me.com','mac.com') THEN 'apple'
		WHEN {d} IN ('att.net','sbcglobal.net','bellsouth.net','ameritech.net','pacbell.net','swbell.net','flash.net','prodigy.net','snet.net') THEN 'att'
		WHEN {d} IN ('charter.net','spectrum.net','rr.com','roadrunner.com','twc.com','brighthouse.com')
			OR {d} LIKE '%.rr.com' THEN 'charter'
		ELSE 'other'
	END`
	return strings.ReplaceAll(tpl, "{d}", domExpr)
}

// ─── Service ────────────────────────────────────────────────────────────────

// AudienceCadenceKPIService serves the messages-to-engage/convert KPIs and
// the ISP doctrine registry.
type AudienceCadenceKPIService struct {
	db *sql.DB
}

// NewAudienceCadenceKPIService constructs the service and ensures the
// doctrine table exists with its seeds (idempotent — CREATE IF NOT EXISTS +
// INSERT ... ON CONFLICT DO NOTHING).
func NewAudienceCadenceKPIService(db *sql.DB) *AudienceCadenceKPIService {
	s := &AudienceCadenceKPIService{db: db}
	if err := s.ensureTables(); err != nil {
		// Non-fatal: the KPI endpoints don't depend on the doctrine table,
		// and boot must not die on a DDL race.
		log.Printf("[audience-cadence-kpis] ensureTables: %v", err)
	}
	return s
}

func (s *AudienceCadenceKPIService) ensureTables() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS mailing_isp_doctrines (
		isp          TEXT PRIMARY KEY,
		title        TEXT NOT NULL DEFAULT '',
		doctrine_md  TEXT NOT NULL DEFAULT '',
		north_stars  TEXT NOT NULL DEFAULT '',
		health_bands TEXT NOT NULL DEFAULT '',
		updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return err
	}
	for _, d := range ispDoctrineSeeds {
		if _, err := s.db.Exec(
			`INSERT INTO mailing_isp_doctrines (isp, title, doctrine_md, north_stars, health_bands)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (isp) DO NOTHING`,
			d.ISP, d.Title, d.DoctrineMD, d.NorthStars, d.HealthBands,
		); err != nil {
			return err
		}
	}
	return nil
}

// ─── KPI types ──────────────────────────────────────────────────────────────

type cadenceWeekPoint struct {
	WeekStart        string  `json:"week_start"`
	Converters       int     `json:"converters"`
	MsgsToConvertP50 float64 `json:"msgs_to_convert_p50"`
}

type cadenceISPKPI struct {
	ISP string `json:"isp"`

	EngagedSample   int     `json:"engaged_sample"`
	MsgsToEngageP25 float64 `json:"msgs_to_engage_p25"`
	MsgsToEngageP50 float64 `json:"msgs_to_engage_p50"`
	MsgsToEngageP75 float64 `json:"msgs_to_engage_p75"`
	MsgsToEngageAvg float64 `json:"msgs_to_engage_avg"`

	Converters       int     `json:"converters"`
	MsgsToConvertP25 float64 `json:"msgs_to_convert_p25"`
	MsgsToConvertP50 float64 `json:"msgs_to_convert_p50"`
	MsgsToConvertP75 float64 `json:"msgs_to_convert_p75"`
	MsgsToConvertAvg float64 `json:"msgs_to_convert_avg"`

	WindowSends           int64   `json:"window_sends"`
	ConversionPer10kSends float64 `json:"conversion_per_10k_sends"`

	WeeklyTrend []cadenceWeekPoint `json:"weekly_trend"`
}

type cadenceKPIResponse struct {
	APIVersion         string          `json:"api_version"`
	AsOf               time.Time       `json:"as_of"`
	Days               int             `json:"days"`
	WindowStart        time.Time       `json:"window_start"`
	EngagedSampleLimit int             `json:"engaged_sample_limit"`
	EngagedSampleTotal int             `json:"engaged_sample_total"`
	MethodNote         string          `json:"method_note"`
	ISPs               []cadenceISPKPI `json:"isps"`

	// Computing is true when one or more heavy sections degraded (timed out or
	// errored) and the payload is partial — the client should show a friendly
	// "still computing — large dataset" state, not an error.
	Computing bool `json:"computing"`
	// EngageAvailable is false when the messages-to-first-engagement cohort scan
	// did not complete (the section that overruns the budget until
	// idx_message_log_lower_email_sent is built). When false, every row's
	// msgs_to_engage_* / engaged_sample fields are not meaningful.
	EngageAvailable bool `json:"engage_available"`
	// PartialErrors carries the per-section degradation reasons (diagnostic).
	PartialErrors []string `json:"partial_errors,omitempty"`
}

// ─── KPI handler ────────────────────────────────────────────────────────────

// HandleAudienceCadenceKPIs — GET /api/mailing/audience-cadence/kpis?days=60
// cadenceKPICache memoizes the heavy KPI aggregation per (org, days) — the
// first-engagement cohort scan is expensive and the answer evolves daily, not
// per page load (QA finding C2, 2026-06-12).
var cadenceKPICache = struct {
	sync.Mutex
	entries map[string]cadenceKPICacheEntry
}{entries: map[string]cadenceKPICacheEntry{}}

type cadenceKPICacheEntry struct {
	body    []byte
	expires time.Time
}

const cadenceKPICacheTTL = 15 * time.Minute

func (s *AudienceCadenceKPIService) HandleAudienceCadenceKPIs(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)

	days := cadenceDefaultDays
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	if days > cadenceMaxDays {
		days = cadenceMaxDays
	}

	cacheKey := orgID + "|" + strconv.Itoa(days)
	cadenceKPICache.Lock()
	if e, ok := cadenceKPICache.entries[cacheKey]; ok && time.Now().Before(e.expires) {
		cadenceKPICache.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(e.body)
		return
	}
	cadenceKPICache.Unlock()

	// Hard budget for the whole aggregation — never let cohort scans pile up.
	ctxBudget, cancelBudget := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancelBudget()
	r = r.WithContext(ctxBudget)

	now := time.Now().UTC()
	since := now.AddDate(0, 0, -days)
	trendSince := now.AddDate(0, 0, -cadenceTrendDays)
	if trendSince.Before(since) {
		trendSince = since
	}

	rows := map[string]*cadenceISPKPI{}
	getRow := func(isp string) *cadenceISPKPI {
		if row, ok := rows[isp]; ok {
			return row
		}
		row := &cadenceISPKPI{ISP: isp, WeeklyTrend: []cadenceWeekPoint{}}
		rows[isp] = row
		return row
	}

	ctx := r.Context()
	subISP := ispFamilyCase(`SPLIT_PART(LOWER(s.email), '@', 2)`)

	// Partial-failure accounting: each heavy section degrades to a "computing"
	// partial response (HTTP 200) instead of a hard 500, so one slow/failed
	// query never nukes the whole screen. engageAvailable specifically gates the
	// messages-to-first-engagement section (the one that overruns the budget
	// until idx_message_log_lower_email_sent exists).
	var computing bool
	var partialErrors []string
	engageAvailable := true

	// 1) messages-to-first-engagement (sampled cohort).
	engagedTotal := 0
	qEngage := `
		WITH first_eng AS (
			SELECT e.subscriber_id, MIN(e.event_at) AS first_at
			FROM mailing_tracking_events e
			WHERE e.organization_id = $1
			  AND e.event_type IN ('opened','clicked')
			  AND e.subscriber_id IS NOT NULL
			  AND e.event_at >= $2
			GROUP BY e.subscriber_id
			LIMIT $3
		), cohort AS (
			SELECT f.subscriber_id, f.first_at, LOWER(s.email) AS email, ` + subISP + ` AS isp
			FROM first_eng f
			JOIN mailing_subscribers s ON s.id = f.subscriber_id
		), per_sub AS (
			SELECT c.isp, c.subscriber_id, COUNT(m.id)::float8 AS msgs
			FROM cohort c
			LEFT JOIN mailing_message_log m
			  ON LOWER(m.email) = c.email
			 AND m.organization_id = $1
			 AND m.sent_at <= c.first_at
			GROUP BY c.isp, c.subscriber_id
		)
		SELECT isp,
		       COUNT(*)::int,
		       COALESCE(percentile_cont(0.25) WITHIN GROUP (ORDER BY msgs), 0),
		       COALESCE(percentile_cont(0.50) WITHIN GROUP (ORDER BY msgs), 0),
		       COALESCE(percentile_cont(0.75) WITHIN GROUP (ORDER BY msgs), 0),
		       COALESCE(AVG(msgs), 0)
		FROM per_sub
		GROUP BY isp`
	engageCtx, engageCancel := context.WithTimeout(ctx, cadenceEngageBudget)
	eng, err := s.db.QueryContext(engageCtx, qEngage, orgID, since, cadenceEngagedSampleLimit)
	if err != nil {
		// DEGRADE, do not 500: without idx_message_log_lower_email_sent this
		// per-subscriber join becomes a seq scan that overruns the budget and
		// returns context-deadline-exceeded. Report the engage section as
		// "computing" and keep the remaining KPIs (which run independently
		// below in the budget this section did not consume).
		log.Printf("[audience-cadence-kpis] engage query (degrading to computing): %v", err)
		engageAvailable = false
		computing = true
		partialErrors = append(partialErrors, "messages-to-engage: "+err.Error())
	} else {
		for eng.Next() {
			var isp string
			var n int
			var p25, p50, p75, avg float64
			if err := eng.Scan(&isp, &n, &p25, &p50, &p75, &avg); err != nil {
				log.Printf("[audience-cadence-kpis] engage scan (degrading): %v", err)
				engageAvailable = false
				computing = true
				partialErrors = append(partialErrors, "messages-to-engage scan: "+err.Error())
				break
			}
			row := getRow(isp)
			row.EngagedSample = n
			row.MsgsToEngageP25 = p25
			row.MsgsToEngageP50 = p50
			row.MsgsToEngageP75 = p75
			row.MsgsToEngageAvg = avg
			engagedTotal += n
		}
		if rerr := eng.Err(); rerr != nil {
			// Deadline tripped mid-stream — same degradation path.
			log.Printf("[audience-cadence-kpis] engage rows (degrading): %v", rerr)
			engageAvailable = false
			computing = true
			partialErrors = append(partialErrors, "messages-to-engage: "+rerr.Error())
		}
		eng.Close()
	}
	engageCancel()

	// 2) messages-to-conversion (exact, terminal event = offer suppression
	//    reason='converted').
	convCTE := `
		WITH conv AS (
			SELECT os.subscriber_id, MIN(os.suppressed_at) AS conv_at
			FROM mailing_offer_suppressions os
			WHERE os.organization_id = $1
			  AND os.reason = 'converted'
			  AND os.suppressed_at >= $2
			GROUP BY os.subscriber_id
		), cohort AS (
			SELECT v.subscriber_id, v.conv_at, LOWER(s.email) AS email, ` + subISP + ` AS isp
			FROM conv v
			JOIN mailing_subscribers s ON s.id = v.subscriber_id
		), per_sub AS (
			SELECT c.isp, c.subscriber_id, date_trunc('week', c.conv_at) AS wk, COUNT(m.id)::float8 AS msgs
			FROM cohort c
			LEFT JOIN mailing_message_log m
			  ON LOWER(m.email) = c.email
			 AND m.organization_id = $1
			 AND m.sent_at <= c.conv_at
			GROUP BY c.isp, c.subscriber_id, date_trunc('week', c.conv_at)
		)`

	qConvert := convCTE + `
		SELECT isp,
		       COUNT(*)::int,
		       COALESCE(percentile_cont(0.25) WITHIN GROUP (ORDER BY msgs), 0),
		       COALESCE(percentile_cont(0.50) WITHIN GROUP (ORDER BY msgs), 0),
		       COALESCE(percentile_cont(0.75) WITHIN GROUP (ORDER BY msgs), 0),
		       COALESCE(AVG(msgs), 0)
		FROM per_sub
		GROUP BY isp`
	conv, err := s.db.QueryContext(ctx, qConvert, orgID, since)
	if err != nil {
		// Degrade independently — an engage timeout (or any single section
		// failing) must not nuke the rest of the response.
		log.Printf("[audience-cadence-kpis] convert query (degrading): %v", err)
		computing = true
		partialErrors = append(partialErrors, "messages-to-convert: "+err.Error())
	} else {
		for conv.Next() {
			var isp string
			var n int
			var p25, p50, p75, avg float64
			if err := conv.Scan(&isp, &n, &p25, &p50, &p75, &avg); err != nil {
				log.Printf("[audience-cadence-kpis] convert scan (degrading): %v", err)
				computing = true
				partialErrors = append(partialErrors, "messages-to-convert scan: "+err.Error())
				break
			}
			row := getRow(isp)
			row.Converters = n
			row.MsgsToConvertP25 = p25
			row.MsgsToConvertP50 = p50
			row.MsgsToConvertP75 = p75
			row.MsgsToConvertAvg = avg
		}
		conv.Close()
	}

	// 3) 4-week weekly trend of msgs_to_convert_p50 (cheap — conversion
	//    cohort is small; reruns the same CTEs over the trend window).
	qTrend := convCTE + `
		SELECT isp, wk,
		       COUNT(*)::int,
		       COALESCE(percentile_cont(0.50) WITHIN GROUP (ORDER BY msgs), 0)
		FROM per_sub
		GROUP BY isp, wk
		ORDER BY isp, wk`
	trend, err := s.db.QueryContext(ctx, qTrend, orgID, trendSince)
	if err != nil {
		log.Printf("[audience-cadence-kpis] trend query (degrading): %v", err)
		computing = true
		partialErrors = append(partialErrors, "weekly-trend: "+err.Error())
	} else {
		for trend.Next() {
			var isp string
			var wk time.Time
			var n int
			var p50 float64
			if err := trend.Scan(&isp, &wk, &n, &p50); err != nil {
				log.Printf("[audience-cadence-kpis] trend scan (degrading): %v", err)
				computing = true
				partialErrors = append(partialErrors, "weekly-trend scan: "+err.Error())
				break
			}
			row := getRow(isp)
			row.WeeklyTrend = append(row.WeeklyTrend, cadenceWeekPoint{
				WeekStart:        wk.Format("2006-01-02"),
				Converters:       n,
				MsgsToConvertP50: p50,
			})
		}
		trend.Close()
	}

	// 4) window sends per ISP → conversion_per_10k_sends. Sourced from the
	// daily domain-agent scorecard rollup (already ISP-family-keyed), NOT raw
	// mailing_message_log — a windowed GROUP BY over 60-90 days of message
	// log was a minutes-long scan (QA finding C2, 2026-06-12).
	qSends := `
		SELECT LOWER(isp) AS isp, COALESCE(SUM(sends), 0)::bigint
		FROM mailing_domain_agent_scorecard
		WHERE organization_id = $1
		  AND day >= $2::date
		GROUP BY 1`
	sends, err := s.db.QueryContext(ctx, qSends, orgID, since)
	if err != nil {
		log.Printf("[audience-cadence-kpis] sends query (degrading): %v", err)
		computing = true
		partialErrors = append(partialErrors, "window-sends: "+err.Error())
	} else {
		for sends.Next() {
			var isp string
			var n int64
			if err := sends.Scan(&isp, &n); err != nil {
				log.Printf("[audience-cadence-kpis] sends scan (degrading): %v", err)
				computing = true
				partialErrors = append(partialErrors, "window-sends scan: "+err.Error())
				break
			}
			getRow(isp).WindowSends = n
		}
		sends.Close()
	}

	out := make([]cadenceISPKPI, 0, len(rows))
	for _, isp := range cadenceISPOrder {
		if row, ok := rows[isp]; ok {
			if row.WindowSends > 0 {
				row.ConversionPer10kSends = float64(row.Converters) / float64(row.WindowSends) * 10000
			}
			out = append(out, *row)
		}
	}

	resp := cadenceKPIResponse{
		APIVersion:         VersionAudienceCadenceKPIs,
		AsOf:               now,
		Days:               days,
		WindowStart:        since,
		EngagedSampleLimit: cadenceEngagedSampleLimit,
		EngagedSampleTotal: engagedTotal,
		MethodNote: "Engaged cohort = subscribers whose first opened/clicked tracking event falls inside the window " +
			"(event-bounded approximation — engagement history before the window is not scanned), sampled to at most " +
			strconv.Itoa(cadenceEngagedSampleLimit) + " subscribers. Prior messages counted from mailing_message_log up to the terminal event. " +
			"Conversions = mailing_offer_suppressions reason='converted' (exact, no sampling). " +
			"SES-routed traffic (all of gmail + the SES-tenant brands) is pixel-blind in Postgres, so engaged samples undercount on those routes; " +
			"AOL is reported as its own family per operator doctrine (not folded into yahoo).",
		ISPs:            out,
		Computing:       computing,
		EngageAvailable: engageAvailable,
		PartialErrors:   partialErrors,
	}

	if body, err := json.Marshal(resp); err == nil {
		// Only cache complete responses — a partial "computing" payload must not
		// stick for the full TTL, or the screen would keep reporting "computing"
		// long after the index lands and the query recovers.
		if !computing {
			cadenceKPICache.Lock()
			cadenceKPICache.entries[cacheKey] = cadenceKPICacheEntry{body: body, expires: time.Now().Add(cadenceKPICacheTTL)}
			cadenceKPICache.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
		return
	}
	respondJSON(w, http.StatusOK, resp)
}

// ─── Doctrine endpoints ─────────────────────────────────────────────────────

type ispDoctrine struct {
	ISP         string    `json:"isp"`
	Title       string    `json:"title"`
	DoctrineMD  string    `json:"doctrine_md"`
	NorthStars  string    `json:"north_stars"`
	HealthBands string    `json:"health_bands"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// HandleListISPDoctrines — GET /api/mailing/audience-cadence/doctrines
func (s *AudienceCadenceKPIService) HandleListISPDoctrines(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT isp, title, doctrine_md, north_stars, health_bands, updated_at
		 FROM mailing_isp_doctrines ORDER BY isp`)
	if err != nil {
		log.Printf("[audience-cadence-kpis] list doctrines: %v", err)
		respondError(w, http.StatusInternalServerError, "doctrine list failed")
		return
	}
	defer rows.Close()
	out := []ispDoctrine{}
	for rows.Next() {
		var d ispDoctrine
		if err := rows.Scan(&d.ISP, &d.Title, &d.DoctrineMD, &d.NorthStars, &d.HealthBands, &d.UpdatedAt); err != nil {
			respondError(w, http.StatusInternalServerError, "doctrine scan failed")
			return
		}
		out = append(out, d)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"doctrines": out})
}

var ispKeyPattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// HandleUpsertISPDoctrine — PUT /api/mailing/audience-cadence/doctrines/{isp}
func (s *AudienceCadenceKPIService) HandleUpsertISPDoctrine(w http.ResponseWriter, r *http.Request) {
	isp := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "isp")))
	if !ispKeyPattern.MatchString(isp) {
		respondError(w, http.StatusBadRequest, "invalid isp key (want lowercase [a-z0-9_-], max 32 chars)")
		return
	}
	var body struct {
		Title       string `json:"title"`
		DoctrineMD  string `json:"doctrine_md"`
		NorthStars  string `json:"north_stars"`
		HealthBands string `json:"health_bands"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var d ispDoctrine
	err := s.db.QueryRowContext(r.Context(),
		`INSERT INTO mailing_isp_doctrines (isp, title, doctrine_md, north_stars, health_bands, updated_at)
		 VALUES ($1, $2, $3, $4, $5, NOW())
		 ON CONFLICT (isp) DO UPDATE SET
		   title = EXCLUDED.title,
		   doctrine_md = EXCLUDED.doctrine_md,
		   north_stars = EXCLUDED.north_stars,
		   health_bands = EXCLUDED.health_bands,
		   updated_at = NOW()
		 RETURNING isp, title, doctrine_md, north_stars, health_bands, updated_at`,
		isp, body.Title, body.DoctrineMD, body.NorthStars, body.HealthBands,
	).Scan(&d.ISP, &d.Title, &d.DoctrineMD, &d.NorthStars, &d.HealthBands, &d.UpdatedAt)
	if err != nil {
		log.Printf("[audience-cadence-kpis] upsert doctrine %s: %v", isp, err)
		respondError(w, http.StatusInternalServerError, "doctrine save failed")
		return
	}
	respondJSON(w, http.StatusOK, d)
}

// ─── Doctrine seeds ─────────────────────────────────────────────────────────
//
// Distilled from the operator's ratified doctrine canon (2026-06-11):
// docs/APPLE_SENDING_DOCTRINE.md, MICROSOFT_SENDING_DOCTRINE.md,
// YAHOO_SENDING_DOCTRINE.md, COMCAST_SENDING_DOCTRINE.md,
// AOL_SENDING_DOCTRINE.md, DOCTRINE_KPI_SCORECARD_JUN12.md (gmail/att lanes
// + north-star bands). Seeds apply once (ON CONFLICT DO NOTHING); the
// operator edits live via PUT.

type ispDoctrineSeed struct {
	ISP         string
	Title       string
	DoctrineMD  string
	NorthStars  string
	HealthBands string
}

var ispDoctrineSeeds = []ispDoctrineSeed{
	{
		ISP:         "apple",
		Title:       "Apple — The Conversion Engine",
		DoctrineMD:  "Apple is the platform's conversion engine: **6.2 true conversions per million delivered** vs Microsoft 1.3 and Gmail ~0.7 — every retail (purchase-class) conversion ever tracked is Apple (50+ homeowners on iPhones, image-rich creative, converting 16–35 min after a real click). **MPP is an instrument, not noise**: a fetch is liveness (45% of converters were MPP-LIVE pre-click), fetches ÷ delivered is an inbox-placement meter (junked mail is not prefetched), and no fetch across ~5 sends means nobody is home. For the engaged tier the single MPP fetch lands at the real open (±15 min) — open timing is learnable for clickers/MPP-LIVE, meaningless for the passive mass. The click is the only universal signal; hierarchy: **click > MPP-liveness > nothing > human open > machine open** (37% of converters never fired an open pixel). Reputation is per-domain and fast-moving; 5.3.2 is tempo pushback, not bad addresses — evenings clear (3–5%) vs mornings (8–11%). Deliverable ≠ alive: Apple data validates itself on the first drip touch.\n\nStrategy: CLICKER + MPP-LIVE rings anchor every Apple lane; purchase-class offers route Apple-first; weight lanes to afternoon/evening; MPP-silence ×5 → accelerated exit; grow via fresh-intent Apple acquisition (KPI: cost per MPP-LIVE record).",
		NorthStars:  "- Same-day MPP fetch rate (fetches ÷ delivered): green ≥50%, yellow 30–50%, red <30%\n- Conversions per million delivered: hold ≥6/M while volume grows\n- MPP-LIVE pool size per domain (the asset) + CLICKER pool growth\n- Converter-tier mix vs the 45/37/8 (MPP-LIVE / click-only / human-UA) baseline\n- 5.3.2 rate per domain (reputation)",
		HealthBands: "Daily auto-tier on Apple bounce rate per domain: <2% uncapped · 5–10% push-through w/ watch · 10–20% capped at recent delivered · >20% clickers-only trickle. Intraday >8% → drop a band. 5.3.2 rate bands A+B: green <2%, red >5%. Deferrals: green <500 msgs, red >2,000. Evening bounce 3–5% vs morning 8–11% — weight PM. MPP silence ×5 consecutive sends → exit the lane.",
	},
	{
		ISP:         "microsoft",
		Title:       "Microsoft — Two Fleets: Verified-Human Core + CPM Reach",
		DoctrineMD:  "Microsoft's visible engagement is ~99% synthetic — human share of the click stream is **0.4%** (Apple link-preview machinery, SafeLinks/datacenter scanners); scanner engagement is anti-targeting here more than anywhere else. Yet Microsoft holds the platform's largest verified-human reservoir: **162.7k MPP-LIVE** subscribers (hotmail/outlook/live/msn synced to real iPhones) + ~4.7k verified clickers — human-DILUTED, not human-poor (1 in 50 sends touches a verified human vs Apple ~1 in 1). An MPP fetch is per-recipient inbox proof; seeds only certify gateway acceptance. Microsoft humans convert on considered-decision lead-gen (Metal Roofing, NDR, CarShield, heloc; 8/11 converters were MPP-LIVE; conversion-per-human-click ≈ Apple) — the deficit is reach-to-humans, not persuasion. Complaint rate is the only reputation lever Microsoft documents.\n\n**Run two fleets**: Core lane = MSFT-MPP-LIVE + MSFT-CLICKERS rings (~167k), Apple-style morning cadence, lead-gen led, one offer touch/day, goal 1.3 → 4+ conv/M. Reach lane = broad opener bands under the 20k/day/domain caps, priced and reported as CPM reach — never as 'engagement' — with complaints ≈ 0.",
		NorthStars:  "- Same-day MPP fetch rate on the core: green ≥40%, yellow 25–40%, red <25%\n- Click-quality flip: ≥30% of core-lane clicks device-human (raw-pool baseline 0.4%)\n- Human click rate: ≥0.3% of delivered (red <0.05%)\n- Core conversions/M: 1.3 → 4+ within two weeks of daily core lane\n- Reach lane: complaint rate (the only number Microsoft cares about) + CPM revenue/M\n- Honesty metric: % of MSFT audience definitions still using unverdicted engagement → 0",
		HealthBands: "Acceptance: green ≥97%, red <93% (baseline ~96%). Complaints: green ≤2/day, red >10. Bounce: green <2%, red >5%. Reach lane hygiene: 20k/day/domain caps, complaints ≈ 0, bounce <2%, 14/21-day clocks retire non-responders. Tenure: clickers lifetime; MPP-LIVE while fetches continue; MPP-silence ×5 sends → exit.",
	},
	{
		ISP:         "yahoo",
		Title:       "Yahoo — Ratio Rebuild (small instrumented garden)",
		DoctrineMD:  "Yahoo's visible engagement is ~99% manufactured by our own seed vendor: 10,884 of 10,889 'human Windows' clicks/30d came from one automation farm (egress **75.98.0.0/16**), which also contaminates regular campaigns via 71 accounts in the normal base — quarantine it from every measurement (the seedlist lane keeps running as reputation scaffolding). The true organic asset is **~600 people**: 524 MPP-LIVE + 74 proxy-viewers + ~15 verified clickers; ~1 in 129 sends touches a verified-alive account; conversions to date: 0. Acceptance ≠ placement — clean infrastructure is accepted and silently junked, and low complaints from the junk folder are the absence of signal, not health. Daily volume does not drive placement; **composition does** (ratio × consistency × time, at week scale). Two honest instruments only: the MPP panel meter (farm-immune, per-recipient inbox proof) and seed-excluded yahoo-proxy opens. All Yahoo lanes **PMTA-pinned** — SES strips pixels and a ratio rebuild cannot be flown blind. Fresh records are worthless without an expectation moment at capture ('your quote is in your email — check spam').\n\nStrategy: engaged-core rings on the best-meter mature domains, caps from meter bands not volume fear, ring-by-ring expansion gated on the meter. Falsification: 6 weeks of core concentration with zero conversions and no meter lift → demote to maintenance-only.",
		NorthStars:  "- MPP panel meter per sending domain (fetch rate on panel deliveries) — THE placement instrument\n- Seed-excluded proxy-open rate (true view-time reads)\n- Verified clicker count (~15/30d baseline)\n- Panel size per brand (the asset being grown)\n- First Yahoo conversion\n- Quarantine discipline: seed farm 75.98.0.0/16 excluded from every engagement read",
		HealthBands: "Meter bands per sending domain: ≥20% grow audience rings +10–20%/week · 10–20% hold and build · <10% engaged-core only until recovery. Baselines (clean PMTA days): HT 28.6% / DB 26.6% / MH 22.6% / QF 22.4% / BWP 24.2% / FC 20.3%; warming nine ~14–16%. One offer touch/day max; drip records terminal at touch 4 with no signal, never re-enrolled.",
	},
	{
		ISP:         "comcast",
		Title:       "Comcast — Pace, Don't Prove",
		DoctrineMD:  "Comcast is traditional IP reputation, and it telegraphs: **deferrals climb before blocks land**; blocks are temporal and forgiving (~7 days once behavior changes). Reputation is scoped **per IP, not per domain** — within our own /24 on the same days/content/audiences, the clean cohort held 0% block share while the burned cohort sat at 34–60%. When accepted, Comcast inboxes generously: **66.9% panel inbox rate — the highest placement measured of any ISP**; there is no placement or engagement-personalization wall, the constraint is tempo at the gate, full stop. The population is disproportionately valuable: 7.5% alive density (~10× Yahoo), primary household addresses, heavily iPhone-hosted (~47% of comcast opens are Apple MPP) — an **Apple-doctrine satellite** for content, timing, and signal handling. The May block was bought by dormant-data volume at spike tempo; dormant data is permanently barred. PMTA-direct, pinned profiles, permanently (a pacing-governed lane cannot fly pixel-blind on SES).\n\nStrategy: COMCAST-MPP-LIVE + COMCAST-CLICKERS rings, clicker-first drain, clean-IP cohort only, the deferral governor as the single feedback loop, then a smooth flat ladder — same number every day. Cheapest incremental conversion lane after Apple; the only cost is patience.",
		NorthStars:  "- Acceptance rate (hold ≥99%)\n- Deferral rate — the governor and early-warning instrument\n- 5.3.2 spam-related count (hold 0)\n- Panel inbox rate vs the 66.9% baseline\n- MPP-LIVE pool growth (1,665 baseline) + clicker pool (68)\n- Conversions/M — 6 weeks of ring concentration at zero → demote to maintenance reach",
		HealthBands: "Deferral rate >25–30% of attempts in a day → freeze the volume ladder. ANY 5.3.2 reappearance → halve volume, hold until zero for 3 consecutive days. Acceptance ≥99%. Placement baseline 66.9% (regression alarm). Growth: +10–15%/week from the engaged-core baseline, FLAT daily volume — no spike days, no double-days. Burned IPs rest ≥1 week, reintroduced one at a time at trickle.",
	},
	{
		ISP:         "aol",
		Title:       "AOL — Consistency Repair (own lane, unseeded control)",
		DoctrineMD:  "AOL throttles; it does not block: acceptance never collapsed (worst day 47%), but **715k messages were deferred in 30 days against ~230k deliveries (3:1)** — a pacing-debt regime triggered by the May volume spike. Most AOL 'bounces' are queue-expiry casualties — **read bounce spikes as tempo signals, not list hygiene**. Consistency is the currency (the differentiator from Yahoo, despite shared Oath filtering): daily volume swung 19× in a month, and erratic volume reads as snowshoe — AOL has never seen a stable daily number from us, so it has never granted one. Where Yahoo's lever is WHO, **AOL's lever is HOW WE ARRIVE: same number, same hour, every day**. The live population is ~450 people (366 MPP-LIVE + 86 proxy-viewers + 15 clickers) and is the purest Apple-satellite demographic in the portfolio — 90% of AOL opens are Apple MPP. AOL receives zero manufactured engagement (the seed network mails yahoo-domain only), so its readings are honest: the clean control lane for the whole Yahoo stack. PMTA-direct only.\n\nStrategy: engaged-core rings (CLICKERS > MPP-LIVE > VIEWERS), ONE anchor (6:00 AM Denver), flat daily number for 2–3 weeks, dormant data barred permanently; earn the ladder only after the deferral debt clears.",
		NorthStars:  "- Deferral rate per sending domain — THE number; the repair works when it trends down week-over-week\n- Acceptance rate + queue-expiry bounce count\n- Panel inbox rate vs 21.9% baseline (expect rise as the throttle unwinds)\n- MPP-LIVE pool (366 baseline) · proxy-viewer pool (86 — the honest read signal)\n- First AOL conversion",
		HealthBands: "Target deferral rate <10% before any growth; any day >40% → hold the number and investigate. <10% sustained one full week → +10%/week, flat at the new number, expansion ring = recent engaged tiers only; any deferral-rate reversal → drop back one rung, hold 2 weeks. Falsification: 3 weeks of flat cadence with no material deferral improvement → demote to drip-intake-only.",
	},
	{
		ISP:         "gmail",
		Title:       "Gmail — Viewer Cascade (SES-routed, lake-measured)",
		DoctrineMD:  "Gmail routes **SES-always**, and SES strips the open/click pixels — Gmail engagement records ~nothing in Postgres. Judge every Gmail lane from the event lake (`source='ses'`) Google-proxy opens only; PG-derived Gmail engagement segments are invalid. Trust per-recipient instruments (Google-proxy view, DSN class), never raw opens/clicks. The play is the **viewer cascade**: step-1 newsletter to the broad band → harvest same-day Google-proxy viewers into a TODAY-VIEWERS segment → step-2 offer send to viewers while the read is hours old → step-3 evening incremental to the day's clickers. Placement = performance; conversions are lagging bonus signal only (Gmail baseline ~0.7 conv/M). Routing audit must hold: zero non-SES gmail sends, zero seed-list recipients in lanes.",
		NorthStars:  "- Step-1 NL viewer rate (Google-proxy opens ÷ delivered, same-day): green ≥15%, yellow 8–15%, red <8%\n- Step-2 audience yield (TODAY-VIEWERS total): green ≥12,000, red <5,000\n- Step-2 offer view rate: green ≥25%, red <10%\n- View→click (device clicks ÷ step-2 delivered): green ≥0.5%, red <0.1%\n- Step-3 incremental device clicks: any >0 meaningful\n- Routing audit: non-SES gmail sends = 0 · seeds in lanes = 0",
		HealthBands: "Viewer-rate bands: ≥15% green / 8–15% yellow / <8% red. KPI data source is the lake (source='ses') ONLY — PG tracking is blind for Gmail, and the MPP panel meter does not apply. Day-level volume showed no placement effect (Pearson +0.13) — composition over volume here too.",
	},
	{
		ISP:         "att",
		Title:       "AT&T — Ring Discovery (yahoo-stack tempo, Apple-hosted)",
		DoctrineMD:  "The ATT family (att.net / sbcglobal / bellsouth / pacbell / ameritech…) rides the Yahoo filtering stack at the gateway — tempo and deferral discipline inherit from the Yahoo/AOL doctrines — but the population itself is heavily iPhone-hosted: **~50% of ATT-domain engagement is Apple MPP**, so liveness, placement, and timing read through the Apple instrument set (MPP fetch = per-recipient inbox proof). The engaged universe is small (~2.4k), so the lane's scaling job is **discovery**: surface NEW first-ever MPP fetchers and new device clickers and grow the rings, rather than mailing the legacy mass — legacy-address bounce risk is real. Content and timing inherit from the Apple doctrine (image-rich, over-50, morning-read anchor); measurement is per-recipient, never raw opens.",
		NorthStars:  "- New MPP-LIVE discoveries (first-ever fetchers surfaced by the lane): green ≥25, yellow 10–25, red <10\n- Same-day MPP fetch rate: green ≥30%, red <15%\n- Ring growth: new fetchers + new device clickers (scaling KPI)\n- Acceptance at yahoo-stack tempo (deferral count watched)",
		HealthBands: "Acceptance: green ≥93% with deferral msgs <300, red <85%. Bounce: green <5%, red >10% (legacy-address risk). Same-day MPP fetch: green ≥30%, red <15%. Engaged universe baseline: ~2.4k.",
	},
}
