package api

// =============================================================================
// SEGMENTATION COMMAND — GET /api/mailing/segmentation/health
// =============================================================================
// One aggregation endpoint behind the Segmentation Command screen. Its job is
// to make segment-membership STALENESS impossible to hide: the frozen-segment
// defect class (band counts frozen since jul19, a gmail recalc cron that never
// ran, a rollup that ran once) survived for weeks because every existing
// surface showed counts-refresh ("segment_refresh OK") while MEMBERSHIP
// rebuilt on a 28-day rotation. This endpoint separates the two truths and
// verdicts every segment FAMILY:
//
//   LIVE            registered, SLA declared, newest membership build in SLA
//   STALE           registered, SLA declared, newest membership build past SLA
//                   (or never built) — membership truth, from the build ledger
//   STATIC-DECLARED registered with sla_hours=0 (family declares no cadence)
//   UNREGISTERED    segments exist but match NO active registry pattern —
//                   discovered by name-pattern grouping so unowned families
//                   are loud, not invisible (the alarm-blindness root cause)
//
// Sources (read-only; no writes anywhere on this path):
//   - mailing_segment_registry      ownership truth (family, SLA, cadence)
//   - mailing_segment_build_ledger  MEMBERSHIP build truth (last_built_at)
//   - mailing_segments              declared type, counts, count-refresh times
//   - mailing_worker_heartbeats /   segment-worker liveness (platform-global
//     mailing_worker_runs           tables, same as /api/worker-health)
//   - mailing_segment_members       per-family churn (bounded aggregate — the
//     table is ~47M rows and materialized_at is unindexed, so this query runs
//     under its own short timeout and DEGRADES to churn_method='unavailable'
//     rather than stalling the screen; when it succeeds the method is
//     'materialized_window' because DELETE+INSERT rebuilds re-stamp
//     materialized_at for every row — it counts rows (re)materialized in the
//     window, not net-new members)
//   - mailing_campaigns             campaign_refs_3d via the same JSONB path
//     main.go's segment-cleanup guard uses
//     (pmta_config->'campaign_input'->'inclusion_segments'); degrades to
//     refs_method='unavailable'
//
// Resilience contract (mirrors segment_workers_handlers.go): the backbone
// segments query is the only hard dependency; registry/churn/refs/worker
// sources each degrade independently and the endpoint never 500s on them.

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// VersionSegmentationHealthAPI is bumped on every response-shape change.
const VersionSegmentationHealthAPI = "1.0"

// Family verdicts (JSON contract — the frontend switch matches these exactly).
const (
	segVerdictLive         = "LIVE"
	segVerdictStale        = "STALE"
	segVerdictStatic       = "STATIC-DECLARED"
	segVerdictUnregistered = "UNREGISTERED"
)

// Worker lights (JSON contract).
const (
	workerLightOK      = "ok"
	workerLightStale   = "stale"
	workerLightError   = "error"
	workerLightStalled = "stalled"
	workerLightUnknown = "unknown" // no heartbeat row — NEVER rendered as OK
)

// segmentationExpectedWorkers always appear in the workers array, with light
// 'unknown' when mailing_worker_heartbeats has no row for them — a worker that
// never beat must not be invisible (that was the gmail-recalc-cron blindness).
var segmentationExpectedWorkers = []string{
	"segment_refresh",
	"segment_cleanup",
	"verified_humans_ledger",
	"partner_human_rollup",
}

// segHealthMaxSegments bounds the backbone query; segHealthMaxDrilldown caps
// the per-family drilldown list (truncation is flagged, never silent).
const (
	segHealthMaxSegments  = 20000
	segHealthMaxDrilldown = 40
)

// segHealthDivergenceGap: counts refreshed this much later than the last
// membership build flag the segment as divergent — the "counts fresh,
// membership stale" trap gets its own explicit indicator.
const segHealthDivergenceGap = 24 * time.Hour

// Per-source query budgets. Churn/refs are optional and short; the backbone
// gets most of the request budget (well inside the prod 30s statement_timeout).
const (
	segHealthRequestBudget = 25 * time.Second
	segHealthChurnBudget   = 10 * time.Second
	segHealthRefsBudget    = 8 * time.Second
)

// ── Pure derivations (unit-tested in segmentation_health_test.go) ───────────

// familyVerdict derives the family verdict from registration + SLA math.
// lastBuild is the newest MEMBERSHIP build (ledger last_built_at max across
// the family) — never a count-refresh timestamp.
func familyVerdict(registered bool, slaHours int, lastBuild *time.Time, now time.Time) string {
	if !registered {
		return segVerdictUnregistered
	}
	if slaHours <= 0 {
		return segVerdictStatic
	}
	if lastBuild == nil || now.Sub(*lastBuild) > time.Duration(slaHours)*time.Hour {
		return segVerdictStale
	}
	return segVerdictLive
}

// deriveFamilyKey groups an UNREGISTERED segment into a discovered family by
// its name prefix (token before the first '-' or ' '). Deterministic so the
// same estate always groups the same way.
func deriveFamilyKey(name string) (key, pattern string) {
	n := strings.TrimSpace(name)
	if n == "" {
		return "(unnamed)", "(unnamed)"
	}
	cut := strings.IndexAny(n, "- ")
	if cut < 2 { // no separator, or a degenerate 1-char prefix — own family
		return n, n
	}
	prefix := n[:cut]
	return prefix, prefix + string(n[cut]) + "%"
}

// segmentWorkerLight classifies one worker's liveness. A missing heartbeat
// row is UNKNOWN, never OK. Thresholds mirror segmentWorkerOnline
// (segment_workers_handlers.go): stale past 2× cadence with a 5-minute floor.
func segmentWorkerLight(hasBeat, stalled bool, lastStatus string, secondsSinceBeat int64, expectedIntervalSeconds int) string {
	if !hasBeat {
		return workerLightUnknown
	}
	if stalled {
		return workerLightStalled
	}
	if lastStatus != "" && lastStatus != "ok" {
		return workerLightError
	}
	threshold := int64(expectedIntervalSeconds) * 2
	if min := int64(segmentWorkerOnlineGrace.Seconds()); threshold < min {
		threshold = min
	}
	if secondsSinceBeat > threshold {
		return workerLightStale
	}
	return workerLightOK
}

// segmentCountsDiverged flags the counts-fresh/membership-stale trap: the
// count-refresh clock advanced while the membership build clock did not.
func segmentCountsDiverged(countRefreshedAt, membershipBuiltAt *time.Time) bool {
	if countRefreshedAt == nil {
		return false
	}
	if membershipBuiltAt == nil {
		return true // counts exist for a membership that was never built
	}
	return countRefreshedAt.Sub(*membershipBuiltAt) > segHealthDivergenceGap
}

// segFamilyVerdictRank orders families worst-first for display and alert
// ranking. (Named to avoid the unrelated verdictRank in send_baselines.go.)
func segFamilyVerdictRank(verdict string) int {
	switch verdict {
	case segVerdictStale:
		return 0
	case segVerdictUnregistered:
		return 1
	case segVerdictLive:
		return 2
	default: // STATIC-DECLARED
		return 3
	}
}

// ── JSON shapes ─────────────────────────────────────────────────────────────

type segHealthSegment struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	SegmentType     string     `json:"segment_type"`
	SubscriberCount int64      `json:"subscriber_count"`
	// LastCountAt is the COUNT-refresh clock (GREATEST of last_calculated_at /
	// last_count_at on mailing_segments) — deliberately separate from…
	LastCountAt *time.Time `json:"last_count_at"`
	// …LastBuiltAt, the MEMBERSHIP build clock (ledger last_built_at).
	LastBuiltAt     *time.Time `json:"last_built_at"`
	LastBuildStatus string     `json:"last_build_status"` // ok|failed|running|blocked_delta|unknown|none
	LedgerCount     int64      `json:"ledger_count"`
	BuildSource     string     `json:"build_source"`
	MemberInserts24 *int64     `json:"member_inserts_24h"` // null when churn unavailable
	CampaignRefs3d  int        `json:"campaign_refs_3d"`
	Diverged        bool       `json:"count_membership_diverged"`
}

type segHealthFamily struct {
	FamilyKey       string     `json:"family_key"`
	FamilyPattern   string     `json:"family_pattern"`
	Registered      bool       `json:"registered"`
	Owner           string     `json:"owner"`
	Cadence         string     `json:"cadence"`
	SLAHours        int        `json:"sla_hours"`
	KeepPolicy      string     `json:"keep_policy"`
	Verdict         string     `json:"verdict"`
	SegmentsCount   int        `json:"segments_count"`
	DynamicCount    int        `json:"dynamic_count"`
	StaticCount     int        `json:"static_count"`
	SubscriberTotal int64      `json:"subscriber_total"`
	LastBuildMin    *time.Time `json:"last_membership_build_min"`
	LastBuildMax    *time.Time `json:"last_membership_build_max"`
	Builds24h       int        `json:"builds_last_24h"`
	FailedNow       int        `json:"failed_now"`
	MemberInserts24 int64      `json:"member_inserts_24h"`
	MemberInserts7d int64      `json:"member_inserts_7d"`
	CampaignRefs3d  int        `json:"campaign_refs_3d"`
	DivergedCount   int        `json:"diverged_count"`
	Segments        []segHealthSegment `json:"segments"`
	SegmentsTrunc   bool               `json:"segments_truncated"`
}

type segHealthWorker struct {
	Name             string  `json:"name"`
	Kind             string  `json:"kind"` // platform | external
	CadenceSeconds   int     `json:"cadence_seconds"`
	LastBeatAt       *string `json:"last_beat_at"`
	SecondsSinceBeat *int64  `json:"seconds_since_beat"`
	LastStatus       string  `json:"last_status"`
	LastError        string  `json:"last_error"`
	CycleCount       int64   `json:"cycle_count"`
	Stalled          bool    `json:"stalled"`
	LastRunStatus    string  `json:"last_run_status"`
	LastRunAt        *string `json:"last_run_at"`
	Light            string  `json:"light"` // ok|stale|error|stalled|unknown
}

type segHealthAlert struct {
	Severity     string  `json:"severity"` // red | amber
	Verdict      string  `json:"verdict"`
	FamilyKey    string  `json:"family_key"`
	Message      string  `json:"message"`
	BlastRadius  int64   `json:"blast_radius"` // subscriber_total of the family
	HoursOverdue float64 `json:"hours_overdue"`
}

type segHealthSummary struct {
	FamiliesLive         int   `json:"families_live"`
	FamiliesStale        int   `json:"families_stale"`
	FamiliesStatic       int   `json:"families_static"`
	FamiliesUnregistered int   `json:"families_unregistered"`
	SegmentsTotal        int   `json:"segments_total"`
	SubscriberTotal      int64 `json:"subscriber_total"`
}

type segmentationHealthResponse struct {
	APIVersion  string            `json:"api_version"`
	GeneratedAt string            `json:"generated_at"`
	Summary     segHealthSummary  `json:"summary"`
	Families    []segHealthFamily `json:"families"`
	Workers     []segHealthWorker `json:"workers"`
	Alerts      []segHealthAlert  `json:"alerts"`
	// ChurnMethod: 'materialized_window' (rows (re)materialized in window —
	// see header comment) or 'unavailable' (query errored / timed out).
	ChurnMethod string `json:"churn_method"`
	// RefsMethod: 'inclusion_segments_3d' or 'unavailable'.
	RefsMethod string `json:"refs_method"`
	// SegmentsTruncated: the backbone hit its hard row bound.
	SegmentsTruncated bool `json:"segments_truncated"`
	// RegistryAvailable=false: the registry read failed and every family below
	// is therefore UNREGISTERED by construction, not by truth.
	RegistryAvailable bool `json:"registry_available"`
}

// ── Store ───────────────────────────────────────────────────────────────────

// SegmentationHealthStore is the read-only data access layer (no raw SQL in
// the handler). All org-scoped queries take orgID explicitly.
type SegmentationHealthStore struct {
	db *sql.DB
}

func NewSegmentationHealthStore(db *sql.DB) *SegmentationHealthStore {
	return &SegmentationHealthStore{db: db}
}

type segHealthRegistryRow struct {
	FamilyKey     string
	FamilyPattern string
	Owner         string
	Cadence       string
	SLAHours      int
	KeepPolicy    string
	Active        bool
}

// Registry returns every active registry row for the org. Errors degrade to
// (nil, false) — the caller renders everything UNREGISTERED and says so.
func (s *SegmentationHealthStore) Registry(ctx context.Context, orgID uuid.UUID) ([]segHealthRegistryRow, bool) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT family_key, family_pattern, owner, cadence, sla_hours, keep_policy, active
		FROM mailing_segment_registry
		WHERE organization_id = $1 AND active
		ORDER BY family_key, family_pattern
	`, orgID)
	if err != nil {
		log.Printf("[segmentation-health] registry query: %v", err)
		return nil, false
	}
	defer rows.Close()
	out := make([]segHealthRegistryRow, 0, 16)
	for rows.Next() {
		var r segHealthRegistryRow
		if err := rows.Scan(&r.FamilyKey, &r.FamilyPattern, &r.Owner, &r.Cadence,
			&r.SLAHours, &r.KeepPolicy, &r.Active); err != nil {
			log.Printf("[segmentation-health] registry scan: %v", err)
			return nil, false
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		log.Printf("[segmentation-health] registry rows: %v", err)
		return nil, false
	}
	return out, true
}

type segHealthSegmentRow struct {
	ID              string
	Name            string
	SegmentType     string
	SubscriberCount int64
	LastCountAt     *time.Time
	LastBuiltAt     *time.Time
	LastBuildStatus string
	LedgerCount     int64
	BuildSource     string
	LedgerUpdatedAt *time.Time
	// MatchedPattern is the most specific active registry pattern the segment
	// name matches (SQL LIKE), or empty when unregistered.
	MatchedPattern string
}

// Segments is the backbone: every non-archived segment for the org joined to
// its build-ledger row and its most specific matching registry pattern. The
// LATERAL LIKE match is bounded by registry size (tens of rows) × segment
// count (bounded below) — the same shape SegmentRegistryStore.Freshness uses.
func (s *SegmentationHealthStore) Segments(ctx context.Context, orgID uuid.UUID) ([]segHealthSegmentRow, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.name,
		       COALESCE(s.segment_type, 'dynamic'),
		       COALESCE(s.subscriber_count, 0)::bigint,
		       GREATEST(s.last_calculated_at, s.last_count_at),
		       l.last_built_at,
		       COALESCE(l.last_build_status, 'none'),
		       COALESCE(l.subscriber_count, 0)::bigint,
		       COALESCE(l.build_source, ''),
		       l.updated_at,
		       COALESCE(reg.family_pattern, '')
		FROM mailing_segments s
		LEFT JOIN mailing_segment_build_ledger l ON l.segment_id = s.id
		LEFT JOIN LATERAL (
			SELECT r.family_pattern
			FROM mailing_segment_registry r
			WHERE r.organization_id = s.organization_id
			  AND r.active
			  AND s.name LIKE r.family_pattern
			ORDER BY length(r.family_pattern) DESC, r.family_pattern
			LIMIT 1
		) reg ON TRUE
		WHERE s.organization_id = $1 AND s.archived_at IS NULL
		ORDER BY s.name
		LIMIT $2
	`, orgID, segHealthMaxSegments+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := make([]segHealthSegmentRow, 0, 256)
	for rows.Next() {
		var r segHealthSegmentRow
		var countAt, builtAt, ledgerUpd sql.NullTime
		if err := rows.Scan(&r.ID, &r.Name, &r.SegmentType, &r.SubscriberCount,
			&countAt, &builtAt, &r.LastBuildStatus, &r.LedgerCount, &r.BuildSource,
			&ledgerUpd, &r.MatchedPattern); err != nil {
			return nil, false, err
		}
		if countAt.Valid {
			t := countAt.Time
			r.LastCountAt = &t
		}
		if builtAt.Valid {
			t := builtAt.Time
			r.LastBuiltAt = &t
		}
		if ledgerUpd.Valid {
			t := ledgerUpd.Time
			r.LedgerUpdatedAt = &t
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := false
	if len(out) > segHealthMaxSegments {
		out = out[:segHealthMaxSegments]
		truncated = true
	}
	return out, truncated, nil
}

type segHealthChurnRow struct {
	Inserts24h int64
	Inserts7d  int64
}

// Churn aggregates mailing_segment_members rows (re)materialized in the last
// 24h/7d per segment. materialized_at is UNINDEXED on a ~47M-row table, so
// this runs under segHealthChurnBudget and any error/timeout degrades to
// (nil, false) → churn_method='unavailable'. The screen still renders.
func (s *SegmentationHealthStore) Churn(ctx context.Context, orgID uuid.UUID) (map[string]segHealthChurnRow, bool) {
	cctx, cancel := context.WithTimeout(ctx, segHealthChurnBudget)
	defer cancel()
	rows, err := s.db.QueryContext(cctx, `
		SELECT m.segment_id::text,
		       COUNT(*) FILTER (WHERE m.materialized_at > NOW() - INTERVAL '24 hours')::bigint,
		       COUNT(*)::bigint
		FROM mailing_segment_members m
		WHERE m.materialized_at > NOW() - INTERVAL '7 days'
		  AND m.segment_id IN (
			SELECT id FROM mailing_segments
			WHERE organization_id = $1 AND archived_at IS NULL
		  )
		GROUP BY m.segment_id
	`, orgID)
	if err != nil {
		log.Printf("[segmentation-health] churn degraded (query bounded at %s): %v", segHealthChurnBudget, err)
		return nil, false
	}
	defer rows.Close()
	out := make(map[string]segHealthChurnRow, 256)
	for rows.Next() {
		var id string
		var c segHealthChurnRow
		if err := rows.Scan(&id, &c.Inserts24h, &c.Inserts7d); err != nil {
			log.Printf("[segmentation-health] churn scan: %v", err)
			return nil, false
		}
		out[id] = c
	}
	if err := rows.Err(); err != nil {
		log.Printf("[segmentation-health] churn degraded mid-read: %v", err)
		return nil, false
	}
	return out, true
}

// CampaignRefs counts, per segment id, the campaigns created in the last 3
// days whose deploy payload includes the segment in inclusion_segments — the
// exact JSONB path the segment-cleanup in-use guard reads
// (cmd/server/main.go:7473). Degrades to (nil, false).
func (s *SegmentationHealthStore) CampaignRefs(ctx context.Context, orgID uuid.UUID) (map[string]int, bool) {
	rctx, cancel := context.WithTimeout(ctx, segHealthRefsBudget)
	defer cancel()
	rows, err := s.db.QueryContext(rctx, `
		SELECT seg.val, COUNT(DISTINCT c.id)::int
		FROM mailing_campaigns c
		CROSS JOIN LATERAL jsonb_array_elements_text(
			CASE WHEN jsonb_typeof(c.pmta_config->'campaign_input'->'inclusion_segments') = 'array'
			     THEN c.pmta_config->'campaign_input'->'inclusion_segments'
			     ELSE '[]'::jsonb END
		) AS seg(val)
		WHERE c.organization_id = $1
		  AND c.created_at > NOW() - INTERVAL '3 days'
		GROUP BY seg.val
	`, orgID)
	if err != nil {
		log.Printf("[segmentation-health] campaign refs degraded: %v", err)
		return nil, false
	}
	defer rows.Close()
	out := make(map[string]int, 64)
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			log.Printf("[segmentation-health] campaign refs scan: %v", err)
			return nil, false
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		log.Printf("[segmentation-health] campaign refs mid-read: %v", err)
		return nil, false
	}
	return out, true
}

type segHealthBeatRow struct {
	LastBeatAt              time.Time
	SecondsSinceBeat        int64
	LastStatus              string
	LastError               string
	CycleCount              int64
	ExpectedIntervalSeconds int
	Stalled                 bool
}

// Heartbeats returns every heartbeat row (the table is one row per worker —
// platform-global, no org column, same as /api/worker-health). Degrades to
// empty: every expected worker then renders UNKNOWN, never OK.
func (s *SegmentationHealthStore) Heartbeats(ctx context.Context) map[string]segHealthBeatRow {
	out := make(map[string]segHealthBeatRow, 16)
	rows, err := s.db.QueryContext(ctx, `
		SELECT worker_name, last_beat_at,
		       EXTRACT(EPOCH FROM (NOW() - last_beat_at))::bigint,
		       last_status, COALESCE(last_error, ''), cycle_count,
		       expected_interval_seconds, stalled
		FROM mailing_worker_heartbeats
		ORDER BY worker_name
	`)
	if err != nil {
		log.Printf("[segmentation-health] heartbeats degraded: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var b segHealthBeatRow
		if err := rows.Scan(&name, &b.LastBeatAt, &b.SecondsSinceBeat, &b.LastStatus,
			&b.LastError, &b.CycleCount, &b.ExpectedIntervalSeconds, &b.Stalled); err != nil {
			continue
		}
		out[name] = b
	}
	return out
}

type segHealthRunRow struct {
	Status    string
	StartedAt time.Time
}

// LastRuns returns the most recent mailing_worker_runs row per worker.
// Degrades to empty (including 42P01 when the table hasn't migrated).
func (s *SegmentationHealthStore) LastRuns(ctx context.Context) map[string]segHealthRunRow {
	out := make(map[string]segHealthRunRow, 16)
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT ON (worker_name) worker_name, status, started_at
		FROM mailing_worker_runs
		ORDER BY worker_name, started_at DESC
	`)
	if err != nil {
		log.Printf("[segmentation-health] worker runs degraded: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var r segHealthRunRow
		if err := rows.Scan(&name, &r.Status, &r.StartedAt); err != nil {
			continue
		}
		out[name] = r
	}
	return out
}

// ── Assembly ────────────────────────────────────────────────────────────────

// buildSegmentationFamilies groups segments into families (registered by
// matched pattern, unregistered by derived name prefix), derives per-family
// aggregates and verdicts, and returns families sorted worst-first. Pure Go
// over already-fetched rows so it is unit-testable without a DB.
func buildSegmentationFamilies(
	registry []segHealthRegistryRow,
	segments []segHealthSegmentRow,
	churn map[string]segHealthChurnRow, churnOK bool,
	refs map[string]int,
	now time.Time,
) []segHealthFamily {
	byPattern := make(map[string]*segHealthFamily, len(registry)+8)
	order := make([]string, 0, len(registry)+8)

	// Registered families exist even with zero segments — a purged family
	// with a live SLA must show up STALE, not vanish.
	for _, r := range registry {
		f := &segHealthFamily{
			FamilyKey:     r.FamilyKey,
			FamilyPattern: r.FamilyPattern,
			Registered:    true,
			Owner:         r.Owner,
			Cadence:       r.Cadence,
			SLAHours:      r.SLAHours,
			KeepPolicy:    r.KeepPolicy,
			Segments:      []segHealthSegment{},
		}
		byPattern[r.FamilyPattern] = f
		order = append(order, r.FamilyPattern)
	}

	for _, s := range segments {
		pattern := s.MatchedPattern
		if pattern == "" || byPattern[pattern] == nil {
			// Unregistered — discover the family from the name.
			key, derived := deriveFamilyKey(s.Name)
			pattern = "unreg:" + derived
			if byPattern[pattern] == nil {
				byPattern[pattern] = &segHealthFamily{
					FamilyKey:     key,
					FamilyPattern: derived,
					Registered:    false,
					Segments:      []segHealthSegment{},
				}
				order = append(order, pattern)
			}
		}
		f := byPattern[pattern]

		displayStatus := s.LastBuildStatus
		if s.LedgerUpdatedAt != nil {
			// A 'running' row stale past 30m is a crashed build (segment_ledger.go).
			displayStatus = coerceLedgerStatus(displayStatus, *s.LedgerUpdatedAt, now)
		}

		seg := segHealthSegment{
			ID:              s.ID,
			Name:            s.Name,
			SegmentType:     s.SegmentType,
			SubscriberCount: s.SubscriberCount,
			LastCountAt:     s.LastCountAt,
			LastBuiltAt:     s.LastBuiltAt,
			LastBuildStatus: displayStatus,
			LedgerCount:     s.LedgerCount,
			BuildSource:     s.BuildSource,
			CampaignRefs3d:  refs[s.ID],
			Diverged:        segmentCountsDiverged(s.LastCountAt, s.LastBuiltAt),
		}
		if churnOK {
			if c, ok := churn[s.ID]; ok {
				v := c.Inserts24h
				seg.MemberInserts24 = &v
				f.MemberInserts24 += c.Inserts24h
				f.MemberInserts7d += c.Inserts7d
			} else {
				zero := int64(0)
				seg.MemberInserts24 = &zero
			}
		}

		f.SegmentsCount++
		if s.SegmentType == "static" {
			f.StaticCount++
		} else {
			f.DynamicCount++
		}
		f.SubscriberTotal += s.SubscriberCount
		f.CampaignRefs3d += refs[s.ID]
		if seg.Diverged {
			f.DivergedCount++
		}
		if displayStatus == "failed" || displayStatus == "blocked_delta" {
			f.FailedNow++
		}
		if s.LastBuiltAt != nil {
			if f.LastBuildMax == nil || s.LastBuiltAt.After(*f.LastBuildMax) {
				t := *s.LastBuiltAt
				f.LastBuildMax = &t
			}
			if f.LastBuildMin == nil || s.LastBuiltAt.Before(*f.LastBuildMin) {
				t := *s.LastBuiltAt
				f.LastBuildMin = &t
			}
			if now.Sub(*s.LastBuiltAt) <= 24*time.Hour {
				f.Builds24h++
			}
		}
		f.Segments = append(f.Segments, seg)
	}

	out := make([]segHealthFamily, 0, len(order))
	for _, p := range order {
		f := byPattern[p]
		f.Verdict = familyVerdict(f.Registered, f.SLAHours, f.LastBuildMax, now)
		// Drilldown: newest membership build first, never-built last; cap with
		// an explicit truncation flag.
		sort.SliceStable(f.Segments, func(i, j int) bool {
			a, b := f.Segments[i].LastBuiltAt, f.Segments[j].LastBuiltAt
			if a == nil && b == nil {
				return f.Segments[i].Name < f.Segments[j].Name
			}
			if a == nil {
				return false
			}
			if b == nil {
				return true
			}
			return a.After(*b)
		})
		if len(f.Segments) > segHealthMaxDrilldown {
			f.Segments = f.Segments[:segHealthMaxDrilldown]
			f.SegmentsTrunc = true
		}
		out = append(out, *f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := segFamilyVerdictRank(out[i].Verdict), segFamilyVerdictRank(out[j].Verdict)
		if ri != rj {
			return ri < rj
		}
		return out[i].SubscriberTotal > out[j].SubscriberTotal
	})
	return out
}

// buildSegmentationAlerts ranks everything STALE (red) or UNREGISTERED
// (amber) by blast radius. Pure.
func buildSegmentationAlerts(families []segHealthFamily, now time.Time) []segHealthAlert {
	alerts := make([]segHealthAlert, 0, 8)
	for _, f := range families {
		switch f.Verdict {
		case segVerdictStale:
			overdue := 0.0
			msg := "membership NEVER BUILT (no ledger row) with a declared " +
				"SLA — either the builder never ran or it is not instrumented"
			if f.LastBuildMax != nil {
				overdue = now.Sub(*f.LastBuildMax).Hours() - float64(f.SLAHours)
				msg = "membership build past SLA"
			}
			alerts = append(alerts, segHealthAlert{
				Severity:     "red",
				Verdict:      f.Verdict,
				FamilyKey:    f.FamilyKey,
				Message:      msg,
				BlastRadius:  f.SubscriberTotal,
				HoursOverdue: overdue,
			})
		case segVerdictUnregistered:
			alerts = append(alerts, segHealthAlert{
				Severity:    "amber",
				Verdict:     f.Verdict,
				FamilyKey:   f.FamilyKey,
				Message:     "family has no registry owner — invisible to SLA alarms and the cleanup consent gate",
				BlastRadius: f.SubscriberTotal,
			})
		}
	}
	sort.SliceStable(alerts, func(i, j int) bool {
		if alerts[i].Severity != alerts[j].Severity {
			return alerts[i].Severity == "red"
		}
		return alerts[i].BlastRadius > alerts[j].BlastRadius
	})
	return alerts
}

// buildSegmentationWorkers merges heartbeats + last runs into the workers
// array: every expected worker always present (UNKNOWN when it never beat),
// plus any other segment-relevant heartbeat row. Pure.
func buildSegmentationWorkers(beats map[string]segHealthBeatRow, runs map[string]segHealthRunRow) []segHealthWorker {
	names := make([]string, 0, len(segmentationExpectedWorkers)+4)
	seen := make(map[string]bool, len(segmentationExpectedWorkers)+4)
	for _, n := range segmentationExpectedWorkers {
		names = append(names, n)
		seen[n] = true
	}
	extra := make([]string, 0, 4)
	for n := range beats {
		if !seen[n] && strings.Contains(n, "segment") {
			extra = append(extra, n)
			seen[n] = true
		}
	}
	sort.Strings(extra)
	names = append(names, extra...)

	out := make([]segHealthWorker, 0, len(names))
	for _, n := range names {
		w := segHealthWorker{Name: n, Kind: "platform", LastStatus: "unknown", LastRunStatus: "none"}
		if strings.HasPrefix(n, "job:") {
			w.Kind = "external"
		}
		b, hasBeat := beats[n]
		if hasBeat {
			beatAt := b.LastBeatAt.UTC().Format(time.RFC3339)
			secs := b.SecondsSinceBeat
			w.LastBeatAt = &beatAt
			w.SecondsSinceBeat = &secs
			w.LastStatus = b.LastStatus
			w.LastError = b.LastError
			w.CycleCount = b.CycleCount
			w.CadenceSeconds = b.ExpectedIntervalSeconds
			w.Stalled = b.Stalled
		}
		w.Light = segmentWorkerLight(hasBeat, b.Stalled, b.LastStatus, b.SecondsSinceBeat, b.ExpectedIntervalSeconds)
		if r, ok := runs[n]; ok {
			w.LastRunStatus = r.Status
			at := r.StartedAt.UTC().Format(time.RFC3339)
			w.LastRunAt = &at
		}
		out = append(out, w)
	}
	return out
}

// ── Service / handler ───────────────────────────────────────────────────────

// SegmentationHealthService exposes the aggregation over HTTP (read-only).
type SegmentationHealthService struct {
	store *SegmentationHealthStore
}

func NewSegmentationHealthService(db *sql.DB) *SegmentationHealthService {
	return &SegmentationHealthService{store: NewSegmentationHealthStore(db)}
}

// RegisterRoutes mounts the service under the /api/mailing group.
func (s *SegmentationHealthService) RegisterRoutes(r chi.Router) {
	r.Get("/segmentation/health", s.HandleHealth)
}

// HandleHealth handles GET /api/mailing/segmentation/health.
func (s *SegmentationHealthService) HandleHealth(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization not resolved")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), segHealthRequestBudget)
	defer cancel()
	now := time.Now()

	registry, registryOK := s.store.Registry(ctx, orgID)

	segments, truncated, err := s.store.Segments(ctx, orgID)
	if err != nil {
		// The backbone is the one hard dependency — without segments there is
		// nothing truthful to render.
		respondError(w, http.StatusInternalServerError, "segmentation health: "+err.Error())
		return
	}

	churn, churnOK := s.store.Churn(ctx, orgID)
	refs, refsOK := s.store.CampaignRefs(ctx, orgID)
	beats := s.store.Heartbeats(ctx)
	runs := s.store.LastRuns(ctx)

	families := buildSegmentationFamilies(registry, segments, churn, churnOK, refs, now)

	resp := segmentationHealthResponse{
		APIVersion:        VersionSegmentationHealthAPI,
		GeneratedAt:       now.UTC().Format(time.RFC3339),
		Families:          families,
		Workers:           buildSegmentationWorkers(beats, runs),
		Alerts:            buildSegmentationAlerts(families, now),
		ChurnMethod:       "unavailable",
		RefsMethod:        "unavailable",
		SegmentsTruncated: truncated,
		RegistryAvailable: registryOK,
	}
	if churnOK {
		resp.ChurnMethod = "materialized_window"
	}
	if refsOK {
		resp.RefsMethod = "inclusion_segments_3d"
	}
	for _, f := range families {
		switch f.Verdict {
		case segVerdictLive:
			resp.Summary.FamiliesLive++
		case segVerdictStale:
			resp.Summary.FamiliesStale++
		case segVerdictStatic:
			resp.Summary.FamiliesStatic++
		case segVerdictUnregistered:
			resp.Summary.FamiliesUnregistered++
		}
		resp.Summary.SegmentsTotal += f.SegmentsCount
		resp.Summary.SubscriberTotal += f.SubscriberTotal
	}
	respondJSON(w, http.StatusOK, resp)
}
