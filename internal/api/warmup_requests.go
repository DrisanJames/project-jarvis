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
	"fmt"
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
//
// ⚠️ RE-CUT 2026-08-20 — `kind` IS PART OF THE KEY NOW.
// The original slot was (organization_id, sending_domain, denver_day) with NO
// discriminator. The 11 kumo domains are simultaneously warm-up domains AND
// newsletter domains, so the SECOND request for a domain+day — whichever mode
// wrote first — failed the INSERT with a unique violation that surfaced as a
// bare 500 with no explanation. The index NAME changes with the definition
// (…_live_slot -> idx_campaign_requests_live_slot_v2) because CREATE UNIQUE
// INDEX IF NOT EXISTS is a no-op when the OLD name already exists: reusing the
// name would silently keep the kind-blind key forever. The old index is
// retired by api.CampaignRequestDropKindBlindSlotDDL, which is registered
// BEFORE this entry (see cmd/server/main.go); `kind` is added before that.
//
// Re-cut now because the table is EMPTY (0 rows / 32 kB, verified on the prod
// primary 2026-08-20). That makes the build microseconds and the lock
// uncontended — the only free moment this will ever have.
const KumoWarmupRequestsLiveSlotDDL = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_campaign_requests_live_slot_v2
	ON mailing_kumo_warmup_requests (
		organization_id,
		kind,
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
	// Domain is a TRANSITIONAL ALIAS of sending_domain. The wizard's
	// WarmupDomainRow interface keys on `domain`, so `warmupDomains.find(d =>
	// d.domain === selectedDomain)` matched NOTHING against the documented
	// response and the warm-up branch never activated. sending_domain is the
	// canonical name (it is what the request row, the newsletter contract and
	// every other handler use); this alias keeps the current client working
	// while the frontend converges, and should be deleted once it has.
	Domain    string `json:"domain"`
	Apex      string `json:"apex"`
	BrandSlug string `json:"brand_slug"`
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
		d.Domain = d.SendingDomain
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
	// ID is a TRANSITIONAL ALIAS of creative_id. The wizard's WarmupCreative
	// interface reads `id` (PMTACampaignWizard.tsx), the server has always
	// emitted `creative_id`, and neither side noticed because the value is
	// only ever posted straight back. creative_id is the canonical name (it is
	// what the newsletter contract and the request row use); this alias exists
	// so the client keeps working while the frontend converges, and should be
	// deleted once it has.
	ID        string `json:"id"`
	BrandCode string `json:"brand_code"` // the APEX domain, e.g. "bestcreditcare.com"
	BrandSlug string `json:"brand_slug"`
	Filename  string `json:"filename"`
	Subject   string `json:"subject"`
	Preheader string `json:"preheader"`
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
	// HTMLBytes is a TRANSITIONAL ALIAS of html_length — same story as ID.
	HTMLBytes int64 `json:"html_bytes"`
	// SHA256 is computed LIVE from html_content, not read from the stored
	// mailing_creatives.sha256 column, so it can never disagree with the bytes
	// that would actually mail.
	SHA256 string `json:"sha256"`
	// CreativeSHA256 is the same value under the name the request contract
	// uses, so the client can post back exactly the field it approved.
	CreativeSHA256 string `json:"creative_sha256"`
	ApprovalStatus string `json:"approval_status"`
	// ApprovedBy is the PRODUCER STAMP that selected this row. Surfaced so an
	// operator can see WHY this creative and not another one.
	ApprovedBy string `json:"approved_by"`
	// HTML is populated only when include_html=1. Without it the wizard's
	// preview iframe rendered an empty white pane with no error — the operator
	// approved blind, which removed the only human check on what ships.
	HTML string `json:"html,omitempty"`
}

// HandleWarmupCreative returns the newest APPROVED newsletter creative for a
// brand, through THE CANONICAL SELECTION (newsletter_requests.go) — the same
// predicate and the same total order the newsletter preview and (once it
// converges) the Python sender use. It no longer discriminates on
// `filename ~ 'kumo-digest'`: a filename convention with no constraint behind
// it is what let the preview and the sender resolve different rows.
//
// Accepts sending_domain (preferred — resolves via apex) or brand_slug, and
// include_html=1 to carry the body for the preview iframe.
func (s *PMTACampaignService) HandleWarmupCreative(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgID(r)

	sendingDomain := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sending_domain")))
	brandSlug := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("brand_slug")))
	if sendingDomain == "" && brandSlug == "" {
		respondError(w, http.StatusBadRequest, "brand_slug or sending_domain required")
		return
	}

	// brandKey is the apex (what mailing_creatives.brand_code carries for
	// newsletter rows). filenameRe is the LEGACY brand_slug path only: there is
	// no apex for a bare slug, so the filename is the sole DB-resident mapping.
	// It narrows WITHIN the producer-stamped set — it never replaces the
	// producer gate the way the old query's filename filter did.
	var brandKey, filenameRe string
	if sendingDomain != "" {
		brandKey = kumoApexFromSendingDomain(sendingDomain)
	} else {
		if !reWarmupSlug.MatchString(brandSlug) {
			respondError(w, http.StatusBadRequest, "brand_slug must be 2-12 lowercase alphanumerics")
			return
		}
		filenameRe = `^nl-` + brandSlug + `-`
	}

	c, err := SelectNewsletterCreative(ctx, s.db, orgID, brandKey, filenameRe)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "creative lookup failed")
		return
	}
	if c == nil {
		respondError(w, http.StatusNotFound,
			"no approved newsletter creative for that brand (needs approval_status='approved' and a "+
				"producer stamp in approved_by: "+strings.Join(NewsletterProducerStamps, ", ")+")")
		return
	}

	resp := warmupCreativeResp{
		CreativeID:     c.CreativeID,
		ID:             c.CreativeID,
		BrandCode:      c.BrandCode,
		Filename:       c.Filename,
		Subject:        c.Subject,
		Preheader:      c.Preheader,
		UpdatedAt:      c.UpdatedAt,
		GeneratedAt:    c.GeneratedAt,
		FreshnessField: "updated_at",
		HTMLLength:     c.HTMLLength,
		HTMLBytes:      c.HTMLLength,
		SHA256:         c.SHA256,
		CreativeSHA256: c.SHA256,
		ApprovalStatus: c.ApprovalStatus,
		ApprovedBy:     c.ApprovedBy,
	}
	if newsletterTruthy(r.URL.Query().Get("include_html")) {
		resp.HTML = c.HTML
	}
	if brandSlug != "" {
		resp.BrandSlug = brandSlug
	} else {
		resp.BrandSlug = newsletterBrandSlug(c.Filename, c.BrandCode)
	}
	respondJSON(w, http.StatusOK, resp)
}

// =============================================================================
// GET  /…/warmup/segments?brand_slug=
// =============================================================================

type warmupSegment struct {
	SegmentID string `json:"segment_id"`
	// ID is a TRANSITIONAL ALIAS of segment_id — same class as warmupDomain.Domain
	// and warmupCreativeResp.ID. The wizard's WarmupSegment interface keys on
	// `id`, so selection (`warmupSelectedSegmentIds.includes(sg.id)`) compared
	// undefined against every row and nothing could be selected.
	ID   string `json:"id"`
	Name string `json:"name"`
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
		seg.ID = seg.SegmentID
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
	ID            string `json:"id"` // empty = create
	SendingDomain string `json:"sending_domain"`
	// Kind selects the programme: kumo_warmup (default, back-compatible with
	// every caller that predates newsletters) or newsletter.
	Kind               string   `json:"kind"`
	BrandSlug          string   `json:"brand_slug"`
	CreativeID         *string  `json:"creative_id"`
	Subject            *string  `json:"subject"`
	Preheader          *string  `json:"preheader"`
	AudienceSegmentIDs []string `json:"audience_segment_ids"`
	ColdSource         *string  `json:"cold_source"`
	ColdQuota          *int     `json:"cold_quota"`
	// ISPQuotas accepts BOTH wire shapes. The handler declared
	// map[string]int; the wizard sends [{"isp":"yahoo","volume":0}] (the same
	// array the offer payload builds). The array decoded into a nil map, so
	// every per-ISP quota an operator set was silently dropped — including the
	// volume:0-per-ISP marker that means "audience-bound, uncapped". Decoded
	// by warmupParseISPQuotas and STORED as an object either way.
	ISPQuotas   json.RawMessage `json:"isp_quotas"`
	ScheduledAt string          `json:"scheduled_at"` // RFC3339
	Status      string          `json:"status"`       // draft|requested|cancelled
	// CreativeSHA256 is the sha of the bytes the operator actually previewed.
	// creative_id does NOT pin the bytes: the daily stage UPDATEs html_content
	// on the same row id, so a preview at 09:00 and a build at 11:00 ship
	// different articles under the approved subject. When this is supplied and
	// no longer matches the live creative, the request is REFUSED with
	// NewsletterShaMismatchReason — never accepted quietly.
	CreativeSHA256 string `json:"creative_sha256"`
}

// warmupParseISPQuotas accepts {"yahoo":100} or [{"isp":"yahoo","volume":100}]
// and normalizes to a map. A zero volume is MEANINGFUL (volume:0 per ISP is
// how an audience-bound/uncapped send is expressed) and is preserved, not
// filtered out.
func warmupParseISPQuotas(raw json.RawMessage) (map[string]int, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	switch trimmed[0] {
	case '{':
		var m map[string]int
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return m, nil
	case '[':
		var arr []struct {
			ISP    string `json:"isp"`
			Volume int    `json:"volume"`
		}
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, err
		}
		m := make(map[string]int, len(arr))
		for _, e := range arr {
			isp := strings.ToLower(strings.TrimSpace(e.ISP))
			if isp == "" {
				continue
			}
			m[isp] = e.Volume
		}
		return m, nil
	}
	return nil, fmt.Errorf("isp_quotas must be an object or an array of {isp,volume}")
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

	// --- kind: closed allow-list, defaults to the pre-newsletter behaviour --
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	if kind == "" {
		kind = RequestKindKumoWarmup
	}
	if !campaignRequestKinds[kind] {
		respondError(w, http.StatusBadRequest,
			"unknown kind '"+kind+"' ("+RequestKindKumoWarmup+"|"+RequestKindNewsletter+")")
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
	ispQuotas, qErr := warmupParseISPQuotas(req.ISPQuotas)
	if qErr != nil {
		respondError(w, http.StatusBadRequest,
			"isp_quotas must be {\"isp\":volume} or [{\"isp\":...,\"volume\":...}]: "+qErr.Error())
		return
	}
	for isp, q := range ispQuotas {
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
	if ispQuotas != nil {
		b, mErr := json.Marshal(ispQuotas)
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

	// --- the sending domain must be a LIVE profile ------------------------
	// kumo_warmup is restricted to routing_mode='kumo' (the 11 warm-up
	// properties). NEWSLETTERS span all 27 sending domains, so the mode gate
	// is the kind, not the transport — a required routing_mode there would
	// exclude the 16 PMTA/SES brands the operator asked for.
	requiredRouting := ""
	if kind == RequestKindKumoWarmup {
		requiredRouting = "kumo"
	}
	var profileExists bool
	err = tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM mailing_sending_profiles
			WHERE organization_id = $1 AND sending_domain = $2
			  AND status = 'active'
			  AND ($3 = '' OR routing_mode = $3)
		)`, orgID, sendingDomain, requiredRouting).Scan(&profileExists)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "sending profile check failed")
		return
	}
	if !profileExists {
		if kind == RequestKindKumoWarmup {
			respondError(w, http.StatusBadRequest,
				"sending_domain '"+sendingDomain+"' is not a live KumoMTA warm-up profile (routing_mode='kumo')")
		} else {
			respondError(w, http.StatusBadRequest,
				"sending_domain '"+sendingDomain+"' has no active sending profile")
		}
		return
	}

	// --- the creative pin: verify the BYTES, not just the id ---------------
	// This is the enforcement point the API owns. The builder re-checks
	// creative_sha256 at build time (it can drift again between submit and
	// build); both sides speak through NewsletterShaMismatchReason so a drift
	// always reads the same way to the operator.
	creativeID := ""
	if req.CreativeID != nil {
		creativeID = strings.TrimSpace(*req.CreativeID)
	}
	// A newsletter handed to the builder MUST be pinned. A draft may still be
	// mid-composition, and a cancel is retiring the row.
	if kind == RequestKindNewsletter && status == "requested" {
		if creativeID == "" {
			respondError(w, http.StatusBadRequest,
				"a newsletter request needs creative_id — the creative is what is being approved")
			return
		}
		if strings.TrimSpace(req.CreativeSHA256) == "" {
			respondError(w, http.StatusBadRequest,
				"a newsletter request needs creative_sha256 (the sha of the bytes shown in the preview); "+
					"creative_id alone does not pin what ships — the daily stage rewrites html_content on the same row")
			return
		}
	}

	var liveSHA interface{}
	if creativeID != "" {
		var (
			sha        string
			approval   string
			cUpdatedAt time.Time
		)
		err = tx.QueryRowContext(ctx, `
			SELECT encode(sha256(convert_to(COALESCE(html_content,''), 'UTF8')), 'hex'),
			       COALESCE(approval_status, ''), updated_at
			FROM mailing_creatives
			WHERE id = $1::uuid AND organization_id = $2`, creativeID, orgID).
			Scan(&sha, &approval, &cUpdatedAt)
		if err == sql.ErrNoRows {
			respondError(w, http.StatusBadRequest,
				"creative_id "+creativeID+" does not exist in this organization")
			return
		}
		if err != nil {
			respondError(w, http.StatusInternalServerError, "creative pin check failed")
			return
		}
		if approval != "approved" {
			respondError(w, http.StatusConflict,
				"creative "+creativeID+" is '"+approval+"', not approved — only approved copy mails")
			return
		}
		if want := strings.TrimSpace(req.CreativeSHA256); want != "" && !strings.EqualFold(want, sha) {
			respondError(w, http.StatusConflict,
				NewsletterShaMismatchReason(sendingDomain, want, sha, cUpdatedAt))
			return
		}
		liveSHA = sha
	}

	actor := actorFromRequest(r)
	var id string

	if req.ID == "" {
		// ---------------- CREATE ----------------
		err = tx.QueryRowContext(ctx, `
			INSERT INTO mailing_kumo_warmup_requests
				(organization_id, sending_domain, kind, brand_slug, creative_id, creative_sha256,
				 subject, preheader, audience_segment_ids, cold_source, cold_quota, isp_quotas,
				 scheduled_at, status, requested_by, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5::uuid, $6, $7, $8, $9::uuid[], $10, $11, $12::jsonb, $13, $14, $15, NOW(), NOW())
			RETURNING id`,
			orgID, sendingDomain, kind, brandSlug, warmupNullableUUID(req.CreativeID), liveSHA,
			warmupNullableText(req.Subject), warmupNullableText(req.Preheader),
			pq.Array(segIDs), warmupNullableText(req.ColdSource), warmupNullableInt(req.ColdQuota),
			ispQuotasJSON, scheduledAt, status, actor).Scan(&id)
		if err != nil {
			if msg, ok := warmupLiveSlotConflict(err, kind, sendingDomain, scheduledAt); ok {
				respondError(w, http.StatusConflict, msg)
				return
			}
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
			    kind                 = $4,
			    brand_slug           = $5,
			    creative_id          = $6::uuid,
			    creative_sha256      = $7,
			    subject              = $8,
			    preheader            = $9,
			    audience_segment_ids = $10::uuid[],
			    cold_source          = $11,
			    cold_quota           = $12,
			    isp_quotas           = $13::jsonb,
			    scheduled_at         = $14,
			    status               = $15,
			    requested_by         = $16,
			    updated_at           = NOW()
			WHERE id = $1 AND organization_id = $2`,
			id, orgID, sendingDomain, kind, brandSlug, warmupNullableUUID(req.CreativeID), liveSHA,
			warmupNullableText(req.Subject), warmupNullableText(req.Preheader),
			pq.Array(segIDs), warmupNullableText(req.ColdSource), warmupNullableInt(req.ColdQuota),
			ispQuotasJSON, scheduledAt, status, actor)
		if err != nil {
			if msg, ok := warmupLiveSlotConflict(err, kind, sendingDomain, scheduledAt); ok {
				respondError(w, http.StatusConflict, msg)
				return
			}
			respondError(w, http.StatusInternalServerError, "warm-up request update failed")
			return
		}
	}

	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "commit failed")
		return
	}

	shaOut := ""
	if s, ok := liveSHA.(string); ok {
		shaOut = s
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"id":             id,
		"sending_domain": sendingDomain,
		"kind":           kind,
		"brand_slug":     brandSlug,
		"status":         status,
		"scheduled_at":   scheduledAt.UTC().Format(time.RFC3339),
		"requested_by":   actor,
		// The sha the row now pins. Echoed so the caller can prove the bytes
		// it approved are the bytes recorded.
		"creative_sha256": shaOut,
	})
}

// warmupLiveSlotConflict turns the live-slot unique violation into an
// operator-facing 409 instead of a bare "insert failed" 500.
//
// The slot is (organization_id, kind, sending_domain, Denver day) over the
// LIVE statuses only (idx_campaign_requests_live_slot_v2). Before `kind` was
// part of the key, a newsletter request and a warm-up request for the same
// kumo domain on the same day collided and the operator saw a 500 with no
// explanation — that is the defect this message closes out.
func warmupLiveSlotConflict(err error, kind, sendingDomain string, scheduledAt time.Time) (string, bool) {
	pqErr, ok := err.(*pq.Error)
	if !ok || pqErr.Code != "23505" {
		return "", false
	}
	if !strings.Contains(pqErr.Constraint, "live_slot") {
		return "", false
	}
	return "a live '" + kind + "' request already exists for " + sendingDomain + " on " +
		scheduledAt.In(propertyLedgerLoc).Format("2006-01-02") + " (Denver). " +
		"Cancel it before requesting another, or edit the existing request by id. " +
		"Warm-up and newsletter requests are separate slots — this conflict is within '" + kind + "'.", true
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
	ID            string  `json:"id"`
	SendingDomain string  `json:"sending_domain"`
	Kind          string  `json:"kind"`
	BrandSlug     string  `json:"brand_slug"`
	CreativeID    *string `json:"creative_id"`
	// CreativeSHA256 is the sha PINNED on the request; CreativeSHA256Live is
	// recomputed from the creative's CURRENT bytes on every read. When they
	// differ the creative was refreshed after approval — CreativeDrifted is
	// true and DriftReason carries the operator-facing sentence. The builder
	// refuses such a row; surfacing it here means the operator sees the
	// refusal coming in the portal instead of finding a `failed` row later.
	CreativeSHA256     *string    `json:"creative_sha256"`
	CreativeSHA256Live *string    `json:"creative_sha256_live"`
	CreativeDrifted    bool       `json:"creative_drifted"`
	DriftReason        string     `json:"drift_reason,omitempty"`
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
			pinSHA   sql.NullString
			cSubject sql.NullString
			cFile    sql.NullString
			cUpdated sql.NullTime
			liveSHA  sql.NullString
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
		if err := rows.Scan(&row.ID, &row.SendingDomain, &row.Kind, &row.BrandSlug, &creative, &pinSHA,
			&subject, &pre, &segIDs, &coldSrc, &coldQ, &ispQ, &row.ScheduledAt,
			&row.Status, &reqBy, &note, &campaign, &created, &updated,
			&cSubject, &cFile, &cUpdated, &liveSHA); err != nil {
			respondError(w, http.StatusInternalServerError, "warm-up request scan failed")
			return
		}
		row.AudienceSegmentIDs = []string(segIDs)
		if row.AudienceSegmentIDs == nil {
			row.AudienceSegmentIDs = []string{}
		}
		row.CreativeID = warmupStrPtr(creative)
		row.CreativeSHA256 = warmupStrPtr(pinSHA)
		row.CreativeSHA256Live = warmupStrPtr(liveSHA)
		// Drift is only meaningful once a sha was pinned AND the creative still
		// exists. An unpinned legacy row is not "drifted" — it is unpinned, and
		// saying otherwise would cry wolf on every warm-up row.
		if pinSHA.Valid && liveSHA.Valid && !strings.EqualFold(pinSHA.String, liveSHA.String) {
			row.CreativeDrifted = true
			row.DriftReason = NewsletterShaMismatchReason(
				row.SendingDomain, pinSHA.String, liveSHA.String, cUpdated.Time)
		}
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
	SELECT r.id, r.sending_domain, r.kind, r.brand_slug, r.creative_id, r.creative_sha256,
	       r.subject, r.preheader, r.audience_segment_ids, r.cold_source,
	       r.cold_quota, r.isp_quotas::text, r.scheduled_at, r.status,
	       r.requested_by, r.build_note, r.campaign_id, r.created_at, r.updated_at,
	       c.subject, c.filename, c.updated_at,
	       encode(sha256(convert_to(COALESCE(c.html_content,''), 'UTF8')), 'hex')
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
