package api

// segmentation_overview.go — AUDIENCE UNIFICATION Phase 2 backend
// (docs/AUDIENCE_UNIFICATION.md): GET /api/mailing/v2/segments/overview.
//
// One org-scoped, paginated catalog read for the "Audiences" screen. Per
// segment it joins, in FOUR batched point-reads over the returned page (no
// unbounded aggregates, each side-source fails open and is flagged):
//
//   - mailing_segment_build_ledger    honest member count + build clock
//     (fetchSegmentLedgerRows — the same read ListSegments uses)
//   - mailing_segments (+ LATERAL mailing_segment_registry match)
//     conditions blob (for the definition summary), prune inputs
//     (keep_active / last_used_at / archived_at) and the best-matching
//     active registry row (protect-first) for the family verdict
//   - mailing_segment_perf_daily      7d/30d delivered / unique opens /
//     unique action-clicks (nightly member-scoped rollup — NOT
//     send-attributed; unique = per-day distinct members summed over days)
//   - mailing_campaign_audiences ⋈ mailing_campaigns (role='include')
//     last_mailed_at + campaigns_30d (Phase-1 links table)
//
// Verdict reuses familyVerdict (segmentation_health.go) per segment:
// registered? sla? newest ledger build within SLA? → LIVE / STALE /
// STATIC-DECLARED / UNREGISTERED.
//
// prune_at mirrors the SegmentCleanupWorker auto-archive phase
// (internal/worker/segment_cleanup.go autoArchiveUnreferencedSegments):
// null when protected (non-active status, keep_active, or an active
// registry keep_policy='protect' family match); otherwise
// GREATEST(last include-campaign ref, last_used_at, created_at) + 30d.

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/segmentation"
	"github.com/lib/pq"
)

// segOverviewDefaultLimit pages the catalog when the caller sends no limit —
// the overview must never stream all ~25k rows with per-row joins.
const segOverviewDefaultLimit = 500

// segOverviewPruneWindow is the cleanup worker's no-reference archive window.
const segOverviewPruneWindow = 30 * 24 * time.Hour

// Side-source query budgets (each fails open independently).
const (
	segOverviewMetaBudget = 10 * time.Second
	segOverviewPerfBudget = 8 * time.Second
	segOverviewRefsBudget = 8 * time.Second
)

type segOverviewPerf struct {
	Delivered    int64   `json:"delivered"`
	UniqueOpens  int64   `json:"unique_opens"`
	UniqueClicks int64   `json:"unique_clicks"`
	OpenRate     float64 `json:"open_rate"`
	ClickRate    float64 `json:"click_rate"`
}

type segOverviewRow struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Category          string     `json:"category"`
	SegmentType       string     `json:"segment_type"`
	Status            string     `json:"status"`
	Members           int64      `json:"members"`
	AudienceSource    string     `json:"audience_source"` // materialized | cached
	LastBuiltAt       *time.Time `json:"last_built_at"`
	LastBuildStatus   *string    `json:"last_build_status"`
	ConditionsSummary string     `json:"conditions_summary"`
	Perf7d            *segOverviewPerf `json:"perf_7d"`
	Perf30d           *segOverviewPerf `json:"perf_30d"`
	LastMailedAt      *time.Time `json:"last_mailed_at"`
	Campaigns30d      int64      `json:"campaigns_30d"`
	Verdict           string     `json:"verdict"` // LIVE|STALE|STATIC-DECLARED|UNREGISTERED|UNKNOWN
	PruneAt           *time.Time `json:"prune_at"`
}

type segOverviewSummary struct {
	// Scope is always "page": totals cover the returned rows, not the whole
	// estate (an estate-wide verdict scan would join ledger+registry over
	// ~25k rows on every load).
	Scope     string         `json:"scope"`
	Total     int            `json:"total"`
	ByVerdict map[string]int `json:"by_verdict"`
	Prunable  int            `json:"prunable"`
}

type segOverviewResponse struct {
	APIVersion  string               `json:"api_version"`
	GeneratedAt time.Time            `json:"generated_at"`
	Segments    []segOverviewRow     `json:"segments"`
	Summary     segOverviewSummary   `json:"summary"`
	// Total matches ALL filters ignoring limit/offset (CountSegments).
	Total int64 `json:"total"`
	// Degradation flags — each side-source fails open independently.
	MetaAvailable bool `json:"meta_available"` // registry/conditions/prune inputs
	PerfAvailable bool `json:"perf_available"` // mailing_segment_perf_daily rollup
	RefsAvailable bool `json:"refs_available"` // mailing_campaign_audiences links
	// PerfMethod labels the attribution honesty of perf_7d/perf_30d.
	PerfMethod string `json:"perf_method"` // member_scoped_rollup | unavailable
}

// segOverviewMeta is the per-segment slice of the meta batch read.
type segOverviewMeta struct {
	conditionsRaw string
	keepActive    bool
	lastUsedAt    sql.NullTime
	archivedAt    sql.NullTime
	registered    bool
	slaHours      int
	keepPolicy    string
}

// segOverviewRefs is the per-segment slice of the campaign-links batch read.
type segOverviewRefs struct {
	lastMailedAt sql.NullTime
	campaigns30d int64
	lastRefAt    sql.NullTime // newest include-ref campaign created_at (prune clock)
}

// HandleSegmentsOverview serves GET /v2/segments/overview. Read-only.
// Filters: q, status, categories, exclude_categories, limit (default 500),
// offset — identical semantics to ListSegments (parseSegmentListFilter).
func (api *SegmentationAPI) HandleSegmentsOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := segmentGetOrgIDFromRequest(r)

	filter, _, errMsg := parseSegmentListFilter(r.URL.Query())
	if errMsg != "" {
		segmentRespondJSONStatus(w, http.StatusBadRequest, map[string]interface{}{
			"error":   "invalid_filter",
			"details": errMsg,
		})
		return
	}
	if filter.Limit <= 0 {
		filter.Limit = segOverviewDefaultLimit
	}

	store := api.engine.Store()
	segments, err := store.ListSegmentsFiltered(ctx, orgID, nil, filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	total, err := store.CountSegments(ctx, orgID, nil, filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	ids := make([]string, 0, len(segments))
	for _, s := range segments {
		ids = append(ids, s.ID.String())
	}

	ledger := api.fetchSegmentLedgerRows(ctx, segments)
	meta, metaOK := api.fetchSegmentOverviewMeta(ctx, orgID, ids)
	perf7, perf30, perfOK := api.fetchSegmentOverviewPerf(ctx, ids)
	refs, refsOK := api.fetchSegmentOverviewRefs(ctx, ids)

	now := time.Now().UTC()
	rows := make([]segOverviewRow, 0, len(segments))
	summary := segOverviewSummary{Scope: "page", ByVerdict: map[string]int{}}

	for _, seg := range segments {
		sid := seg.ID.String()
		row := segOverviewRow{
			ID:          sid,
			Name:        seg.Name,
			Category:    seg.Category,
			SegmentType: seg.SegmentType,
			Status:      seg.Status,
		}

		// Members: ledger count when a build completed, else cached
		// subscriber_count — byte-for-byte the ListSegments resolution.
		led, hasLedger := ledger[sid]
		if hasLedger && led.lastBuiltAt.Valid {
			row.Members = led.subscriberCount
			row.AudienceSource = "materialized"
			t := led.lastBuiltAt.Time
			row.LastBuiltAt = &t
		} else {
			row.Members = int64(seg.SubscriberCount)
			row.AudienceSource = "cached"
		}
		var buildSource string
		if hasLedger {
			status := coerceLedgerStatus(led.status, led.updatedAt, now)
			row.LastBuildStatus = &status
			if led.buildSource.Valid {
				buildSource = led.buildSource.String
			}
		}

		m, hasMeta := meta[sid]
		if hasMeta {
			row.ConditionsSummary = renderSegmentConditionsSummary(m.conditionsRaw, seg.SegmentType, buildSource)
		} else {
			row.ConditionsSummary = "unavailable"
		}

		if p, ok := perf7[sid]; ok {
			row.Perf7d = p
		}
		if p, ok := perf30[sid]; ok {
			row.Perf30d = p
		}

		rf, hasRefs := refs[sid]
		if hasRefs {
			if rf.lastMailedAt.Valid {
				t := rf.lastMailedAt.Time
				row.LastMailedAt = &t
			}
			row.Campaigns30d = rf.campaigns30d
		}

		// Verdict: reuse the Segmentation Command derivation, per segment.
		if metaOK && hasMeta {
			row.Verdict = familyVerdict(m.registered, m.slaHours, row.LastBuiltAt, now)
		} else if metaOK {
			// Meta read succeeded but this id had no row (deleted mid-read).
			row.Verdict = segVerdictUnregistered
		} else {
			row.Verdict = "UNKNOWN" // registry read failed — never fake a verdict
		}

		if metaOK && hasMeta {
			row.PruneAt = derivePruneAt(seg.Status, m.keepActive, m.keepPolicy,
				m.archivedAt, rf.lastRefAt, m.lastUsedAt, seg.CreatedAt)
		}

		summary.ByVerdict[row.Verdict]++
		if row.PruneAt != nil {
			summary.Prunable++
		}
		rows = append(rows, row)
	}
	summary.Total = len(rows)

	perfMethod := "member_scoped_rollup"
	if !perfOK {
		perfMethod = "unavailable"
	}
	segmentRespondJSON(w, segOverviewResponse{
		APIVersion:    VersionSegmentationAPI,
		GeneratedAt:   now,
		Segments:      rows,
		Summary:       summary,
		Total:         total,
		MetaAvailable: metaOK,
		PerfAvailable: perfOK,
		RefsAvailable: refsOK,
		PerfMethod:    perfMethod,
	})
}

// renderSegmentConditionsSummary is the pure definition-line renderer:
// criteria-v2 rendered; lake_spec rendered; otherwise the segment's declared
// type + build_source ("static — build_source: partner_wave" per the Phase-2
// contract; dynamic legacy rows label themselves honestly).
func renderSegmentConditionsSummary(conditionsRaw, segmentType, buildSource string) string {
	if v2, err := segmentation.ParseV2Criteria(conditionsRaw); err != nil {
		return "criteria-v2 (invalid)"
	} else if v2 != nil {
		return segmentation.SummarizeV2(v2)
	}
	if spec := parseLakeSpec(conditionsRaw); spec != nil {
		s := "lake: " + spec.Event + " within " + strconv.Itoa(spec.WindowDays) + "d"
		switch spec.Scope {
		case "brand":
			s += " (brand " + spec.BrandApex + ")"
		case "isp":
			s += " (isp " + spec.ISP + ")"
		}
		return s
	}
	src := buildSource
	if src == "" {
		src = "unknown"
	}
	st := segmentType
	if st == "" {
		st = "static"
	}
	return st + " — build_source: " + src
}

// derivePruneAt computes the archive countdown per the cleanup worker's
// auto-archive rules. Nil = protected (never auto-archived).
func derivePruneAt(status string, keepActive bool, keepPolicy string,
	archivedAt, lastRefAt, lastUsedAt sql.NullTime, createdAt time.Time) *time.Time {
	if status != "active" || keepActive || archivedAt.Valid {
		return nil
	}
	if keepPolicy == "protect" {
		return nil // active registry protect-family match
	}
	clock := createdAt
	if lastUsedAt.Valid && lastUsedAt.Time.After(clock) {
		clock = lastUsedAt.Time
	}
	if lastRefAt.Valid && lastRefAt.Time.After(clock) {
		clock = lastRefAt.Time
	}
	t := clock.Add(segOverviewPruneWindow)
	return &t
}

// fetchSegmentOverviewMeta batch-reads conditions + prune inputs + the best
// active registry match (protect-first, then highest SLA) per segment.
// ok=false → the caller renders verdict UNKNOWN and no prune countdown.
func (api *SegmentationAPI) fetchSegmentOverviewMeta(ctx context.Context, orgID uuid.UUID, ids []string) (map[string]segOverviewMeta, bool) {
	out := make(map[string]segOverviewMeta, len(ids))
	if len(ids) == 0 || api.db == nil {
		return out, true
	}
	qctx, cancel := context.WithTimeout(ctx, segOverviewMetaBudget)
	defer cancel()

	rows, err := api.db.QueryContext(qctx, `
		SELECT s.id::text,
		       COALESCE(s.conditions::text, ''),
		       COALESCE(s.keep_active, FALSE),
		       s.last_used_at,
		       s.archived_at,
		       (reg.keep_policy IS NOT NULL) AS registered,
		       COALESCE(reg.sla_hours, 0),
		       COALESCE(reg.keep_policy, '')
		  FROM mailing_segments s
		  LEFT JOIN LATERAL (
		      SELECT r.sla_hours, r.keep_policy
		        FROM mailing_segment_registry r
		       WHERE r.organization_id = s.organization_id
		         AND r.active = TRUE
		         AND s.name LIKE r.family_pattern
		       ORDER BY (r.keep_policy = 'protect') DESC, r.sla_hours DESC
		       LIMIT 1
		  ) reg ON TRUE
		 WHERE s.id = ANY($1::uuid[]) AND s.organization_id = $2
	`, pq.Array(ids), orgID)
	if err != nil {
		log.Printf("[SegmentOverview] meta query error (verdicts degrade to UNKNOWN): %v", err)
		return out, false
	}
	defer rows.Close()
	for rows.Next() {
		var sid string
		var m segOverviewMeta
		if err := rows.Scan(&sid, &m.conditionsRaw, &m.keepActive, &m.lastUsedAt,
			&m.archivedAt, &m.registered, &m.slaHours, &m.keepPolicy); err != nil {
			continue
		}
		out[sid] = m
	}
	if err := rows.Err(); err != nil {
		log.Printf("[SegmentOverview] meta rows error: %v", err)
		return out, false
	}
	return out, true
}

// fetchSegmentOverviewPerf batch-sums the nightly rollup over trailing 7/30
// Denver days in one grouped read. Rates are opens/clicks over delivered.
func (api *SegmentationAPI) fetchSegmentOverviewPerf(ctx context.Context, ids []string) (map[string]*segOverviewPerf, map[string]*segOverviewPerf, bool) {
	p7 := make(map[string]*segOverviewPerf, len(ids))
	p30 := make(map[string]*segOverviewPerf, len(ids))
	if len(ids) == 0 || api.db == nil {
		return p7, p30, true
	}

	// Denver day floors, matching the rollup's day bucketing.
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		loc = time.UTC
	}
	lt := time.Now().In(loc)
	today := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
	floor7 := today.AddDate(0, 0, -7).Format("2006-01-02")
	floor30 := today.AddDate(0, 0, -30).Format("2006-01-02")

	qctx, cancel := context.WithTimeout(ctx, segOverviewPerfBudget)
	defer cancel()
	rows, err := api.db.QueryContext(qctx, `
		SELECT segment_id::text,
		       COALESCE(SUM(delivered)     FILTER (WHERE day >= $2::date), 0),
		       COALESCE(SUM(opens)         FILTER (WHERE day >= $2::date), 0),
		       COALESCE(SUM(clicks_action) FILTER (WHERE day >= $2::date), 0),
		       COALESCE(SUM(delivered), 0),
		       COALESCE(SUM(opens), 0),
		       COALESCE(SUM(clicks_action), 0)
		  FROM mailing_segment_perf_daily
		 WHERE segment_id = ANY($1::uuid[])
		   AND day >= $3::date
		 GROUP BY segment_id
	`, pq.Array(ids), floor7, floor30)
	if err != nil {
		log.Printf("[SegmentOverview] perf rollup query error (perf columns degrade): %v", err)
		return p7, p30, false
	}
	defer rows.Close()
	for rows.Next() {
		var sid string
		var d7, o7, c7, d30, o30, c30 int64
		if err := rows.Scan(&sid, &d7, &o7, &c7, &d30, &o30, &c30); err != nil {
			continue
		}
		p7[sid] = newSegOverviewPerf(d7, o7, c7)
		p30[sid] = newSegOverviewPerf(d30, o30, c30)
	}
	if err := rows.Err(); err != nil {
		log.Printf("[SegmentOverview] perf rows error: %v", err)
		return p7, p30, false
	}
	return p7, p30, true
}

func newSegOverviewPerf(delivered, opens, clicks int64) *segOverviewPerf {
	p := &segOverviewPerf{Delivered: delivered, UniqueOpens: opens, UniqueClicks: clicks}
	if delivered > 0 {
		p.OpenRate = float64(opens) / float64(delivered)
		p.ClickRate = float64(clicks) / float64(delivered)
	}
	return p
}

// fetchSegmentOverviewRefs batch-reads the Phase-1 campaign→segment links:
// newest include-campaign clock + 30d count (indexed by
// idx_campaign_audiences_segment).
func (api *SegmentationAPI) fetchSegmentOverviewRefs(ctx context.Context, ids []string) (map[string]segOverviewRefs, bool) {
	out := make(map[string]segOverviewRefs, len(ids))
	if len(ids) == 0 || api.db == nil {
		return out, true
	}
	qctx, cancel := context.WithTimeout(ctx, segOverviewRefsBudget)
	defer cancel()
	rows, err := api.db.QueryContext(qctx, `
		SELECT ca.segment_id::text,
		       MAX(COALESCE(c.started_at, c.scheduled_at, c.created_at)) AS last_mailed_at,
		       COUNT(*) FILTER (WHERE c.created_at > NOW() - INTERVAL '30 days') AS campaigns_30d,
		       MAX(c.created_at) AS last_ref_at
		  FROM mailing_campaign_audiences ca
		  JOIN mailing_campaigns c ON c.id = ca.campaign_id
		 WHERE ca.segment_id = ANY($1::uuid[])
		   AND ca.role = 'include'
		 GROUP BY ca.segment_id
	`, pq.Array(ids))
	if err != nil {
		log.Printf("[SegmentOverview] campaign refs query error (last_mailed degrades): %v", err)
		return out, false
	}
	defer rows.Close()
	for rows.Next() {
		var sid string
		var rf segOverviewRefs
		if err := rows.Scan(&sid, &rf.lastMailedAt, &rf.campaigns30d, &rf.lastRefAt); err != nil {
			continue
		}
		out[sid] = rf
	}
	if err := rows.Err(); err != nil {
		log.Printf("[SegmentOverview] campaign refs rows error: %v", err)
		return out, false
	}
	return out, true
}
