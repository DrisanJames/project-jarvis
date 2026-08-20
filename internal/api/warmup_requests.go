package api

// Campaign Manager — Kumo warm-up REQUEST side (records intent only).
//
// WHY A TABLE AND NOT A CALL: the whole Kumo warm-up build pipeline is
// Python on the operator's machine, driven by local JSON registries
// (agents/scheduling/data/kumo_estate.json, yahoo_ramp_schedule.json). The
// portal runs in ECS and cannot invoke it. The DATABASE is therefore the
// contract: the wizard writes a request row here, the local Python builder
// polls, builds the wave, and stamps campaign_id / build_note / status back.
//
// HARD BOUNDARY — this file NEVER creates a campaign, never enqueues, never
// sends. It only records what the operator asked for. A 'requested' row with
// no consumer simply sits there, which is the designed steady state until
// the Python side is wired. Nothing is ever DELETEd: cancel is
// status='cancelled'.
//
// STATE OWNERSHIP (enforced in HandleWarmupRequestUpsert):
//
//	API-settable : draft | requested | cancelled
//	BUILDER-only : building | built | failed
//
// The API refuses to write a builder-owned state, and refuses to mutate a
// row that is currently building/built — otherwise the portal could race the
// builder and silently un-stamp a campaign_id.
//
// Routes are registered by the operator in handlers_pmta_campaign.go
// (RegisterRoutes); this file deliberately touches neither that file nor
// cmd/server/main.go.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/lib/pq"
)

// =============================================================================
// Schema (wired into runStartupMigrations by the operator)
// =============================================================================
//
// REGISTERED AS THREE SEPARATE MIGRATION ENTRIES — NOT one string.
// migrationSkipProbe classifies an entry by its LEADING keywords
// (reMigCreateTable, cmd/server/migration_skip.go:41 — the regex is anchored
// `^\s*CREATE\s+TABLE\s+IF\s+NOT\s+EXISTS`). A single string holding CREATE
// TABLE then CREATE INDEX is probed as CREATE TABLE, so the moment the table
// exists the probe skips the WHOLE entry and the index never lands — silently,
// forever. Same rationale, same shape as worker.LaneStatsRollupDDL /
// worker.LaneStatsRollupIndexDDL (cmd/server/main.go, aug19 lane-stats entries).
//
// Budget discipline: runStartupMigrations gives each statement 5s; a timeout
// logs "skipped, will retry next boot" and is then absent forever. Everything
// below is small DDL on a brand-new (empty) table — no backfill, no rewrite,
// no heavy index build. All three are idempotent (IF NOT EXISTS) and carry no
// trailing semicolon (the runner executes one statement per entry).

// KumoWarmupRequestsDDL creates the request table. New table, no dependency
// on any hot relation, instant.
const KumoWarmupRequestsDDL = `
CREATE TABLE IF NOT EXISTS mailing_kumo_warmup_requests (
	id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	organization_id      UUID        NOT NULL,
	sending_domain       TEXT        NOT NULL,
	brand_slug           TEXT        NOT NULL DEFAULT '',
	creative_id          UUID,
	subject              TEXT,
	preheader            TEXT,
	audience_segment_ids UUID[],
	cold_source          TEXT,
	cold_quota           INTEGER,
	isp_quotas           JSONB,
	scheduled_at         TIMESTAMPTZ NOT NULL,
	status               TEXT        NOT NULL DEFAULT 'draft',
	requested_by         TEXT,
	build_note           TEXT,
	campaign_id          UUID,
	created_at           TIMESTAMPTZ DEFAULT NOW(),
	updated_at           TIMESTAMPTZ DEFAULT NOW()
)`

// KumoWarmupRequestsIndexDDL — the list endpoint's access path
// (organization_id = $1 AND scheduled_at ∈ [day)). Instant on the empty table.
const KumoWarmupRequestsIndexDDL = `
CREATE INDEX IF NOT EXISTS idx_kumo_warmup_requests_org_sched
	ON mailing_kumo_warmup_requests (organization_id, scheduled_at)`

// KumoWarmupRequestsLiveSlotDDL — the requested "one live request per
// (domain, Denver day)" guard. IT IS EXPRESSIBLE CHEAPLY, and here is why the
// obvious form does NOT work:
//
//	(scheduled_at::date)                     -- timestamptz->date is STABLE,
//	                                         -- not IMMUTABLE: rejected as an
//	                                         -- index expression (it depends on
//	                                         -- the session TimeZone GUC).
//	((scheduled_at AT TIME ZONE 'America/Denver')::date)
//	                                         -- timezone(text, timestamptz) IS
//	                                         -- IMMUTABLE — verified on the prod
//	                                         -- primary (PG 16.13):
//	                                         --   pg_proc.provolatile = 'i'
//	                                         -- so this one is indexable.
//
// The build is a plain (non-CONCURRENT) unique index on a table that is empty
// at creation time — microseconds, well inside the 5s budget. Denver is the
// operational day everywhere else in this codebase (propertyLedgerLoc), so the
// uniqueness slot matches what an operator means by "the same day".
//
// Partial predicate = the LIVE states only: cancelled and failed rows are
// excluded so an operator can cancel-and-retry the same domain+day.
//
// NOTE FOR THE WIRER: this index can fail to create if pre-existing duplicate
// live rows exist. On a brand-new table that cannot happen; if the table is
// ever backfilled before this lands, the migration will log a failure and
// retry next boot (fail-safe, never fatal). Register it as its own entry.
const KumoWarmupRequestsLiveSlotDDL = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_kumo_warmup_requests_live_slot
	ON mailing_kumo_warmup_requests (
		organization_id,
		sending_domain,
		((scheduled_at AT TIME ZONE 'America/Denver')::date)
	)
	WHERE status IN ('draft','requested','building','built')`

// =============================================================================
// Status vocabulary
// =============================================================================

// warmupAPIStatuses are the ONLY status values a portal caller may write.
var warmupAPIStatuses = map[string]bool{
	"draft":     true,
	"requested": true,
	"cancelled": true,
}

// warmupBuilderStatuses are owned by the Python builder. The API refuses to
// set them, and refuses to mutate a row sitting in an in-flight one.
var warmupBuilderStatuses = map[string]bool{
	"building": true,
	"built":    true,
	"failed":   true,
}

// warmupFrozenStatuses: the builder is working on (or has finished) this row.
// The API must not touch it — a portal write here could clobber campaign_id.
var warmupFrozenStatuses = map[string]bool{
	"building": true,
	"built":    true,
}

// =============================================================================
// Small helpers
// =============================================================================

var (
	reWarmupUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	// Brand slugs are the estate's short codes (aad, bcc, fth, hfc, hlj, htm,
	// mpf, pmd, trb, usf, yfb). Constrained because the slug is interpolated
	// into a POSIX regex for the filename / segment-name lookups.
	reWarmupSlug = regexp.MustCompile(`^[a-z0-9]{2,12}$`)
)

// kumoApexFromSendingDomain maps a sending domain to its apex, which is the
// key mailing_creatives.brand_code actually carries.
//
// VERIFIED on the prod primary 2026-08-20: all 11 rows of
// `mailing_sending_profiles WHERE routing_mode='kumo'` are of the form
// em.<apex>, and every one of the 11 `filename ~ 'kumo-digest'` creatives
// carries the APEX in brand_code — including the two Yahoo ramp pilots
// (`aadwd.com`, `hfcl.net`), NOT the uppercase AAD/HFC codes. Do not
// "fix" this to a 2–3 letter code.
func kumoApexFromSendingDomain(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	labels := strings.Split(d, ".")
	if len(labels) >= 3 {
		return strings.Join(labels[1:], ".")
	}
	return d
}

// warmupKumoDigestSlugRe builds the filename regex for one brand slug. The
// estate's daily-fresh creatives are registered as nl-<slug>-kumo-digest.html
// (agents/jobs/kumo_newsletter_stage.py).
func warmupKumoDigestSlugRe(slug string) string {
	return `^nl-` + slug + `-kumo-digest`
}

// warmupSegmentNameRe matches segment names that carry this brand's slug as a
// whole token. VERIFIED against the live names on prod 2026-08-20:
//
//	"AAD 30D Openers"       (space-bounded)
//	"KUMO-ALLTIME-BCC-ENG"  (hyphen-bounded)
//	"RAMP-YHOO-A12-YFB"     (hyphen, end-anchored)
//	"FRESH-KUMO-A1-BCC"     (hyphen, end-anchored)
func warmupSegmentNameRe(slug string) string {
	return `(^|[ -])` + strings.ToUpper(slug) + `([ -]|$)`
}

func warmupBadUUID(v string) bool { return !reWarmupUUID.MatchString(v) }

// =============================================================================
// GET  /…/warmup/domains
// =============================================================================

type warmupDomain struct {
	SendingDomain string `json:"sending_domain"`
	Apex          string `json:"apex"`
	BrandSlug     string `json:"brand_slug"`
	// BrandSlugSource: "creative-filename" when the slug was read out of the
	// brand's registered nl-<slug>-kumo-digest.html row (authoritative), or
	// "apex-fallback" when no such creative exists yet. There is NO mechanical
	// rule from domain to slug (bestcreditcare.com→bcc,
	// firsttimebuyerhomeloan.com→fth), so the filename is the only DB-resident
	// mapping and the fallback is explicitly labelled rather than guessed.
	BrandSlugSource string `json:"brand_slug_source"`
	FromName        string `json:"from_name"`
	FromEmail       string `json:"from_email"`
}

// HandleWarmupDomains lists the live Kumo warm-up sending domains, sourced
// from mailing_sending_profiles (routing_mode='kumo') — the authoritative
// estate. Never a hardcoded list: the estate moved from 9 to 11 domains
// without any doc noticing.
func (s *PMTACampaignService) HandleWarmupDomains(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)

	rows, err := s.db.QueryContext(ctx, `
		SELECT sending_domain, COALESCE(from_name,''), COALESCE(from_email,'')
		FROM mailing_sending_profiles
		WHERE organization_id = $1 AND routing_mode = 'kumo' AND status = 'active'
		ORDER BY sending_domain`, orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "kumo profile query failed")
		return
	}
	defer rows.Close()

	domains := []warmupDomain{}
	for rows.Next() {
		var d warmupDomain
		if err := rows.Scan(&d.SendingDomain, &d.FromName, &d.FromEmail); err != nil {
			respondError(w, http.StatusInternalServerError, "kumo profile scan failed")
			return
		}
		d.Apex = kumoApexFromSendingDomain(d.SendingDomain)
		domains = append(domains, d)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "kumo profile query failed")
		return
	}

	// Resolve slugs from the registered digest creatives (apex → slug).
	slugByApex := map[string]string{}
	cRows, err := s.db.QueryContext(ctx, `
		SELECT brand_code, filename
		FROM mailing_creatives
		WHERE organization_id = $1 AND filename ~ '^nl-[a-z0-9]+-kumo-digest'`, orgID)
	if err == nil {
		defer cRows.Close()
		for cRows.Next() {
			var apex, filename string
			if err := cRows.Scan(&apex, &filename); err != nil {
				continue
			}
			if parts := strings.Split(filename, "-"); len(parts) >= 2 {
				slugByApex[strings.ToLower(apex)] = parts[1]
			}
		}
	}

	for i := range domains {
		if slug, ok := slugByApex[domains[i].Apex]; ok && reWarmupSlug.MatchString(slug) {
			domains[i].BrandSlug = slug
			domains[i].BrandSlugSource = "creative-filename"
			continue
		}
		// Fallback: the apex's first label. Honest, stable, and labelled — it
		// is NOT the estate short code.
		domains[i].BrandSlug = strings.Split(domains[i].Apex, ".")[0]
		domains[i].BrandSlugSource = "apex-fallback"
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"domains": domains,
		"count":   len(domains),
	})
}

// =============================================================================
// GET  /…/warmup/creative?brand_slug=|sending_domain=
// =============================================================================

type warmupCreativeResp struct {
	CreativeID string `json:"creative_id"`
	BrandCode  string `json:"brand_code"` // the APEX domain, e.g. "bestcreditcare.com"
	BrandSlug  string `json:"brand_slug"`
	Filename   string `json:"filename"`
	Subject    string `json:"subject"`
	Preheader  string `json:"preheader"`
	// UpdatedAt IS THE FRESHNESS FIELD. The daily refresher
	// (agents/content/refresh.py) rewrites the creative in place and touches
	// updated_at; generated_at is frozen at FIRST INSERT and never moves. On
	// prod 2026-08-20 the estate's rows read generated_at = 2026-08-11 (nine
	// days stale) while updated_at = 2026-08-20 16:33Z — the same morning.
	// Reading generated_at as "how fresh is this creative" is a documented
	// hour-burning trap; the two are labelled distinctly here on purpose and
	// freshness_field names the winner explicitly for the UI.
	UpdatedAt      time.Time `json:"updated_at"`
	GeneratedAt    time.Time `json:"generated_at"`
	FreshnessField string    `json:"freshness_field"`
	HTMLLength     int64     `json:"html_length"`
	// SHA256 is computed LIVE from html_content, not read from the stored
	// mailing_creatives.sha256 column, so it can never disagree with the bytes
	// that would actually mail.
	SHA256         string `json:"sha256"`
	ApprovalStatus string `json:"approval_status"`
}

// HandleWarmupCreative returns the newest APPROVED kumo-digest creative for a
// brand. Accepts sending_domain (preferred — resolves via apex) or brand_slug.
func (s *PMTACampaignService) HandleWarmupCreative(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)

	sendingDomain := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sending_domain")))
	brandSlug := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("brand_slug")))
	if sendingDomain == "" && brandSlug == "" {
		respondError(w, http.StatusBadRequest, "brand_slug or sending_domain required")
		return
	}

	var (
		where string
		key   string
	)
	if sendingDomain != "" {
		// brand_code on mailing_creatives is the APEX domain (verified prod).
		where = "brand_code = $2"
		key = kumoApexFromSendingDomain(sendingDomain)
	} else {
		if !reWarmupSlug.MatchString(brandSlug) {
			respondError(w, http.StatusBadRequest, "brand_slug must be 2-12 lowercase alphanumerics")
			return
		}
		where = "filename ~ $2"
		key = warmupKumoDigestSlugRe(brandSlug)
	}

	var (
		resp     warmupCreativeResp
		filename string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, brand_code, filename, COALESCE(subject,''), COALESCE(preheader,''),
		       updated_at, generated_at,
		       length(html_content)::bigint,
		       encode(sha256(convert_to(html_content, 'UTF8')), 'hex'),
		       approval_status
		FROM mailing_creatives
		WHERE organization_id = $1
		  AND `+where+`
		  AND filename ~ 'kumo-digest'
		  AND approval_status = 'approved'
		ORDER BY updated_at DESC
		LIMIT 1`, orgID, key).
		Scan(&resp.CreativeID, &resp.BrandCode, &filename, &resp.Subject, &resp.Preheader,
			&resp.UpdatedAt, &resp.GeneratedAt, &resp.HTMLLength, &resp.SHA256, &resp.ApprovalStatus)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "no approved kumo-digest creative for that brand")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "creative lookup failed")
		return
	}

	resp.Filename = filename
	resp.FreshnessField = "updated_at"
	if brandSlug != "" {
		resp.BrandSlug = brandSlug
	} else if parts := strings.Split(filename, "-"); len(parts) >= 2 {
		resp.BrandSlug = parts[1]
	}
	respondJSON(w, http.StatusOK, resp)
}

// =============================================================================
// GET  /…/warmup/segments?brand_slug=
// =============================================================================

type warmupSegment struct {
	SegmentID string `json:"segment_id"`
	Name      string `json:"name"`
	// SubscriberCount is the BUILD-LEDGER count when a ledger row exists.
	// mailing_segments.subscriber_count is a cached tally that
	// SegmentRefreshWorker writes as 0 whenever its count query times out
	// (routine under overnight load) — 46 of 108 active engagement segments
	// disagreed on 2026-08-18. Reading the cached column alone shows a healthy
	// segment as empty and the operator picks nothing.
	SubscriberCount int64 `json:"subscriber_count"`
	// CountSource: "build_ledger" (trustworthy) or "cached_counter" (no ledger
	// row exists for this segment — true for the FRESH-KUMO-*/RAMP-YHOO-*
	// families on prod today, so the cached value is all there is).
	CountSource string `json:"count_source"`
	// CounterCount is the cached mailing_segments value, always exposed so the
	// UI can show the disagreement rather than hide it.
	CounterCount    int64      `json:"counter_count"`
	CounterMismatch bool       `json:"counter_mismatch"`
	BuildStatus     string     `json:"build_status"`
	LastBuiltAt     *time.Time `json:"last_built_at"`
}

// HandleWarmupSegments lists candidate engaged-anchor segments for a brand.
func (s *PMTACampaignService) HandleWarmupSegments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)

	brandSlug := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("brand_slug")))
	if brandSlug == "" {
		respondError(w, http.StatusBadRequest, "brand_slug required")
		return
	}
	if !reWarmupSlug.MatchString(brandSlug) {
		respondError(w, http.StatusBadRequest, "brand_slug must be 2-12 lowercase alphanumerics")
		return
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.name, COALESCE(s.subscriber_count, 0)::bigint,
		       l.subscriber_count, l.last_build_status, l.last_built_at
		FROM mailing_segments s
		LEFT JOIN mailing_segment_build_ledger l ON l.segment_id = s.id
		WHERE s.organization_id = $1
		  AND s.status = 'active'
		  AND s.name ~ $2
		ORDER BY s.name
		LIMIT 250`, orgID, warmupSegmentNameRe(brandSlug))
	if err != nil {
		respondError(w, http.StatusInternalServerError, "segment query failed")
		return
	}
	defer rows.Close()

	segments := []warmupSegment{}
	for rows.Next() {
		var (
			seg         warmupSegment
			ledgerCount sql.NullInt64
			buildStatus sql.NullString
			builtAt     sql.NullTime
		)
		if err := rows.Scan(&seg.SegmentID, &seg.Name, &seg.CounterCount,
			&ledgerCount, &buildStatus, &builtAt); err != nil {
			respondError(w, http.StatusInternalServerError, "segment scan failed")
			return
		}
		if ledgerCount.Valid {
			seg.SubscriberCount = ledgerCount.Int64
			seg.CountSource = "build_ledger"
			seg.CounterMismatch = ledgerCount.Int64 != seg.CounterCount
		} else {
			seg.SubscriberCount = seg.CounterCount
			seg.CountSource = "cached_counter"
		}
		if buildStatus.Valid {
			seg.BuildStatus = buildStatus.String
		} else {
			seg.BuildStatus = "unknown"
		}
		if builtAt.Valid {
			t := builtAt.Time
			seg.LastBuiltAt = &t
		}
		segments = append(segments, seg)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "segment query failed")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"brand_slug": brandSlug,
		"segments":   segments,
		"count":      len(segments),
	})
}

// =============================================================================
// POST /…/warmup/requests
// =============================================================================

type warmupRequestUpsertReq struct {
	ID                 string         `json:"id"` // empty = create
	SendingDomain      string         `json:"sending_domain"`
	BrandSlug          string         `json:"brand_slug"`
	CreativeID         *string        `json:"creative_id"`
	Subject            *string        `json:"subject"`
	Preheader          *string        `json:"preheader"`
	AudienceSegmentIDs []string       `json:"audience_segment_ids"`
	ColdSource         *string        `json:"cold_source"`
	ColdQuota          *int           `json:"cold_quota"`
	ISPQuotas          map[string]int `json:"isp_quotas"`
	ScheduledAt        string         `json:"scheduled_at"` // RFC3339
	Status             string         `json:"status"`       // draft|requested|cancelled
}

// HandleWarmupRequestUpsert creates or updates a warm-up request. It records
// intent ONLY — no campaign is created here, ever.
func (s *PMTACampaignService) HandleWarmupRequestUpsert(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)

	var req warmupRequestUpsertReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	sendingDomain := strings.ToLower(strings.TrimSpace(req.SendingDomain))
	if sendingDomain == "" {
		respondError(w, http.StatusBadRequest, "sending_domain required")
		return
	}
	if req.ID != "" && warmupBadUUID(req.ID) {
		respondError(w, http.StatusBadRequest, "id must be a uuid")
		return
	}

	// --- status: builder-owned values are REFUSED outright ---------------
	status := strings.ToLower(strings.TrimSpace(req.Status))
	if status == "" {
		status = "draft"
	}
	if warmupBuilderStatuses[status] {
		respondError(w, http.StatusBadRequest,
			"status '"+status+"' is owned by the build job — the API may only set draft, requested or cancelled")
		return
	}
	if !warmupAPIStatuses[status] {
		respondError(w, http.StatusBadRequest,
			"unknown status '"+status+"' (draft|requested|cancelled)")
		return
	}

	// --- scheduled_at: required, parseable, in the future ------------------
	scheduledRaw := strings.TrimSpace(req.ScheduledAt)
	if scheduledRaw == "" {
		respondError(w, http.StatusBadRequest, "scheduled_at required (RFC3339)")
		return
	}
	scheduledAt, err := time.Parse(time.RFC3339, scheduledRaw)
	if err != nil {
		respondError(w, http.StatusBadRequest, "scheduled_at must be RFC3339 (e.g. 2026-08-21T01:01:00-06:00)")
		return
	}
	// A cancel does not need a future time — the row is being retired.
	if status != "cancelled" && !scheduledAt.After(time.Now()) {
		respondError(w, http.StatusBadRequest, "scheduled_at must be in the future")
		return
	}

	if req.ColdQuota != nil && *req.ColdQuota < 0 {
		respondError(w, http.StatusBadRequest, "cold_quota must be >= 0")
		return
	}
	for isp, q := range req.ISPQuotas {
		if q < 0 {
			respondError(w, http.StatusBadRequest, "isp_quotas."+isp+" must be >= 0")
			return
		}
	}
	if req.CreativeID != nil && strings.TrimSpace(*req.CreativeID) != "" && warmupBadUUID(strings.TrimSpace(*req.CreativeID)) {
		respondError(w, http.StatusBadRequest, "creative_id must be a uuid")
		return
	}
	segIDs := make([]string, 0, len(req.AudienceSegmentIDs))
	for _, id := range req.AudienceSegmentIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if warmupBadUUID(id) {
			respondError(w, http.StatusBadRequest, "audience_segment_ids must all be uuids")
			return
		}
		segIDs = append(segIDs, id)
	}
	brandSlug := strings.ToLower(strings.TrimSpace(req.BrandSlug))
	if brandSlug != "" && !reWarmupSlug.MatchString(brandSlug) {
		respondError(w, http.StatusBadRequest, "brand_slug must be 2-12 lowercase alphanumerics")
		return
	}

	var ispQuotasJSON interface{}
	if req.ISPQuotas != nil {
		b, mErr := json.Marshal(req.ISPQuotas)
		if mErr != nil {
			respondError(w, http.StatusBadRequest, "isp_quotas not serializable")
			return
		}
		ispQuotasJSON = string(b)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "tx begin failed")
		return
	}
	defer tx.Rollback()

	// --- the sending domain must be a LIVE kumo profile -------------------
	var profileExists bool
	err = tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM mailing_sending_profiles
			WHERE organization_id = $1 AND sending_domain = $2
			  AND routing_mode = 'kumo' AND status = 'active'
		)`, orgID, sendingDomain).Scan(&profileExists)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "kumo profile check failed")
		return
	}
	if !profileExists {
		respondError(w, http.StatusBadRequest,
			"sending_domain '"+sendingDomain+"' is not a live KumoMTA warm-up profile (routing_mode='kumo')")
		return
	}

	actor := actorFromRequest(r)
	var id string

	if req.ID == "" {
		// ---------------- CREATE ----------------
		err = tx.QueryRowContext(ctx, `
			INSERT INTO mailing_kumo_warmup_requests
				(organization_id, sending_domain, brand_slug, creative_id, subject, preheader,
				 audience_segment_ids, cold_source, cold_quota, isp_quotas, scheduled_at,
				 status, requested_by, created_at, updated_at)
			VALUES ($1, $2, $3, $4::uuid, $5, $6, $7::uuid[], $8, $9, $10::jsonb, $11, $12, $13, NOW(), NOW())
			RETURNING id`,
			orgID, sendingDomain, brandSlug, warmupNullableUUID(req.CreativeID),
			warmupNullableText(req.Subject), warmupNullableText(req.Preheader),
			pq.Array(segIDs), warmupNullableText(req.ColdSource), warmupNullableInt(req.ColdQuota),
			ispQuotasJSON, scheduledAt, status, actor).Scan(&id)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "warm-up request insert failed")
			return
		}
	} else {
		// ---------------- UPDATE ----------------
		id = req.ID
		var current string
		err = tx.QueryRowContext(ctx, `
			SELECT status FROM mailing_kumo_warmup_requests
			WHERE id = $1 AND organization_id = $2
			FOR UPDATE`, id, orgID).Scan(&current)
		if err == sql.ErrNoRows {
			respondError(w, http.StatusNotFound, "warm-up request not found")
			return
		}
		if err != nil {
			respondError(w, http.StatusInternalServerError, "warm-up request lock failed")
			return
		}
		if msg, ok := warmupTransitionAllowed(current, status); !ok {
			respondError(w, http.StatusConflict, msg)
			return
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE mailing_kumo_warmup_requests
			SET sending_domain       = $3,
			    brand_slug           = $4,
			    creative_id          = $5::uuid,
			    subject              = $6,
			    preheader            = $7,
			    audience_segment_ids = $8::uuid[],
			    cold_source          = $9,
			    cold_quota           = $10,
			    isp_quotas           = $11::jsonb,
			    scheduled_at         = $12,
			    status               = $13,
			    requested_by         = $14,
			    updated_at           = NOW()
			WHERE id = $1 AND organization_id = $2`,
			id, orgID, sendingDomain, brandSlug, warmupNullableUUID(req.CreativeID),
			warmupNullableText(req.Subject), warmupNullableText(req.Preheader),
			pq.Array(segIDs), warmupNullableText(req.ColdSource), warmupNullableInt(req.ColdQuota),
			ispQuotasJSON, scheduledAt, status, actor)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "warm-up request update failed")
			return
		}
	}

	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "commit failed")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"id":             id,
		"sending_domain": sendingDomain,
		"brand_slug":     brandSlug,
		"status":         status,
		"scheduled_at":   scheduledAt.UTC().Format(time.RFC3339),
		"requested_by":   actor,
	})
}

// warmupTransitionAllowed encodes who owns which state.
//
//	draft     → draft | requested | cancelled
//	requested → requested | cancelled          (no walk-back once handed over)
//	failed    → draft | requested | cancelled  (operator retry — builder is done)
//	building  → nothing                        (the builder is mid-flight)
//	built     → nothing                        (campaign_id is stamped)
//	cancelled → cancelled                      (idempotent no-op)
func warmupTransitionAllowed(current, next string) (string, bool) {
	if warmupFrozenStatuses[current] {
		return "request is '" + current + "' — owned by the build job and not editable from the API", false
	}
	switch current {
	case "draft", "failed":
		return "", true
	case "requested":
		if next == "requested" || next == "cancelled" {
			return "", true
		}
		return "request is already 'requested' — it can only be cancelled from the API", false
	case "cancelled":
		if next == "cancelled" {
			return "", true
		}
		return "request is cancelled — create a new request instead of reviving it", false
	}
	return "unknown current status '" + current + "'", false
}

func warmupNullableText(v *string) interface{} {
	if v == nil {
		return nil
	}
	if strings.TrimSpace(*v) == "" {
		return nil
	}
	return *v
}

func warmupNullableUUID(v *string) interface{} {
	if v == nil || strings.TrimSpace(*v) == "" {
		return nil
	}
	return strings.TrimSpace(*v)
}

func warmupNullableInt(v *int) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

// =============================================================================
// GET  /…/warmup/requests?date=YYYY-MM-DD
// =============================================================================

type warmupRequestRow struct {
	ID                 string     `json:"id"`
	SendingDomain      string     `json:"sending_domain"`
	BrandSlug          string     `json:"brand_slug"`
	CreativeID         *string    `json:"creative_id"`
	CreativeSubject    *string    `json:"creative_subject"`
	CreativeFilename   *string    `json:"creative_filename"`
	Subject            *string    `json:"subject"`
	Preheader          *string    `json:"preheader"`
	AudienceSegmentIDs []string   `json:"audience_segment_ids"`
	ColdSource         *string    `json:"cold_source"`
	ColdQuota          *int       `json:"cold_quota"`
	ISPQuotas          *string    `json:"isp_quotas"`
	ScheduledAt        time.Time  `json:"scheduled_at"`
	Status             string     `json:"status"`
	RequestedBy        *string    `json:"requested_by"`
	BuildNote          *string    `json:"build_note"`
	CampaignID         *string    `json:"campaign_id"`
	CreatedAt          *time.Time `json:"created_at"`
	UpdatedAt          *time.Time `json:"updated_at"`
}

// HandleWarmupRequestList lists requests, optionally narrowed to one Denver
// day. Denver bounds are computed in Go and param-injected — the WHERE clause
// never tz-casts scheduled_at (that is the documented seq-scan footgun, and it
// would also defeat idx_kumo_warmup_requests_org_sched).
func (s *PMTACampaignService) HandleWarmupRequestList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)

	var (
		dayStart, dayEnd interface{}
		dateParam        = strings.TrimSpace(r.URL.Query().Get("date"))
	)
	if dateParam != "" {
		start, err := time.ParseInLocation("2006-01-02", dateParam, propertyLedgerLoc)
		if err != nil {
			respondError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
			return
		}
		dayStart = start.UTC()
		dayEnd = start.AddDate(0, 0, 1).UTC()
	}

	rows, err := s.db.QueryContext(ctx, warmupRequestListSQL, orgID, dayStart, dayEnd)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "warm-up request list failed")
		return
	}
	defer rows.Close()

	out := []warmupRequestRow{}
	for rows.Next() {
		var (
			row      warmupRequestRow
			segIDs   pq.StringArray
			creative sql.NullString
			cSubject sql.NullString
			cFile    sql.NullString
			subject  sql.NullString
			pre      sql.NullString
			coldSrc  sql.NullString
			coldQ    sql.NullInt64
			ispQ     sql.NullString
			reqBy    sql.NullString
			note     sql.NullString
			campaign sql.NullString
			created  sql.NullTime
			updated  sql.NullTime
		)
		if err := rows.Scan(&row.ID, &row.SendingDomain, &row.BrandSlug, &creative,
			&subject, &pre, &segIDs, &coldSrc, &coldQ, &ispQ, &row.ScheduledAt,
			&row.Status, &reqBy, &note, &campaign, &created, &updated,
			&cSubject, &cFile); err != nil {
			respondError(w, http.StatusInternalServerError, "warm-up request scan failed")
			return
		}
		row.AudienceSegmentIDs = []string(segIDs)
		if row.AudienceSegmentIDs == nil {
			row.AudienceSegmentIDs = []string{}
		}
		row.CreativeID = warmupStrPtr(creative)
		row.CreativeSubject = warmupStrPtr(cSubject)
		row.CreativeFilename = warmupStrPtr(cFile)
		row.Subject = warmupStrPtr(subject)
		row.Preheader = warmupStrPtr(pre)
		row.ColdSource = warmupStrPtr(coldSrc)
		row.ISPQuotas = warmupStrPtr(ispQ)
		row.RequestedBy = warmupStrPtr(reqBy)
		row.BuildNote = warmupStrPtr(note)
		row.CampaignID = warmupStrPtr(campaign)
		if coldQ.Valid {
			v := int(coldQ.Int64)
			row.ColdQuota = &v
		}
		if created.Valid {
			t := created.Time
			row.CreatedAt = &t
		}
		if updated.Valid {
			t := updated.Time
			row.UpdatedAt = &t
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "warm-up request list failed")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"date":     dateParam,
		"requests": out,
		"count":    len(out),
	})
}

// warmupRequestListSQL — org-scoped, optionally narrowed to a Go-computed
// Denver-day UTC window ($2/$3, NULL = all). Joined to the creative for its
// subject/filename, and carrying the builder's campaign_id stamp.
const warmupRequestListSQL = `
	SELECT r.id, r.sending_domain, r.brand_slug, r.creative_id,
	       r.subject, r.preheader, r.audience_segment_ids, r.cold_source,
	       r.cold_quota, r.isp_quotas::text, r.scheduled_at, r.status,
	       r.requested_by, r.build_note, r.campaign_id, r.created_at, r.updated_at,
	       c.subject, c.filename
	FROM mailing_kumo_warmup_requests r
	LEFT JOIN mailing_creatives c ON c.id = r.creative_id
	WHERE r.organization_id = $1
	  AND ($2::timestamptz IS NULL OR (r.scheduled_at >= $2 AND r.scheduled_at < $3))
	ORDER BY r.scheduled_at DESC, r.created_at DESC
	LIMIT 500`

func warmupStrPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}
