package api

// Drip Lane Onboarding — turn an INGESTED lead file into a lane that actually
// mails, and answer "why is this lane not mailing?" in one read-only pass.
//
//	GET  /api/mailing/drip-lane/options                 pickers (verticals, brands+transport, ISPs, offers)
//	GET  /api/mailing/drip-lane/verify?vertical=&brand= the seven gates
//	POST /api/mailing/drip-lane/onboard                 wire roster + welcome + ladder + budget cells
//
// ─────────────────────────────────────────────────────────────────────────────
// THE LAW THIS OBEYS (Claude memory `cold-data-drip-pipeline-only-LAW`,
// operator 2026-08-17, after the INTRO-sidecar burn: 88,459 cold sends,
// 16,018 bounces / 18.1%):
//
//	cold recipients reach production ONLY as
//	dataset → partner ingest (EO validation) → partner_clean_queue (held)
//	        → drip orchestrator under its per-wave / daily / allow-list caps
//
// This file therefore configures the ORCHESTRATOR'S OWN TABLES and NOTHING
// else. It MUST NOT, now or ever:
//   - create, deploy, stage or cancel a campaign,
//   - INSERT/UPDATE/DELETE partner_clean_queue,
//   - touch a segment, a list, or mailing_campaign_queue.
//
// There is no sidecar send path in here and none may be added. The negative
// assertion is executable, not a comment: TestDripLaneOnboarding_NoSidecarSend
// (drip_lane_onboarding_test.go) greps every SQL constant in this file for the
// forbidden tables and fails the build's test run if one appears.
//
// ─────────────────────────────────────────────────────────────────────────────
// THE SEVEN GATES a lane must pass before it can mail. Each is verified against
// the SEND-TIME query that actually runs, not a plausible approximation:
//
//	1 DATA      partner_clean_queue rows exist for the vertical (supplier ingest's
//	            job — this service never writes them)
//	2 ROSTER    an ACTIVE (vertical, brand) row in partner_drip_vertical_roster
//	            → worker.PartnerDripOrchestrator loadVerticalRosters
//	3 PROFILE   brand → sending domain → an ACTIVE mailing_sending_profiles row,
//	            and we report TRANSPORT + TRACKING DOMAIN because a lane pointed
//	            at a tracking host that does not serve TLS ships dead links
//	4 WELCOME   an ACTIVE partner_drip_creatives row for (vertical, brand)
//	            → resolveCreative (partner_drip_orchestrator.go:1654)
//	5 FOLLOWUP  ACTIVE partner_drip_followup_creatives rows for touches 2..N
//	            → resolveFollowupCreative (partner_drip_orchestrator.go:4900).
//	            NOTE the touch numbering: the follow-up pass computes
//	            touchNum = MAX(touch_count)+1 and REJECTS anything outside
//	            2..MaxTouchCount (:4666-4669). A follow-up row written at
//	            touch_number = 1 is dead configuration — the welcome IS touch 1
//	            and lives in partner_drip_creatives.
//	6 OFFER     for any offer-center touch (creative_filename = '' AND offer_id
//	            set) the offer must carry a SERVING row in all three of
//	            mailing_offer_creatives / _subject_lines / _from_names, using the
//	            orchestrator's own status predicates — an offer whose only
//	            creative is 'archived' passes a naive COUNT(*) and then hard-fails
//	            at send time (resolveOfferCreative :1694, :1712, :1681)
//	7 CAPS      partner_drip_brand_budgets. NO ROWS = UNCONSTRAINED (the ledger's
//	            documented semantics, cmd/server/main.go:3922-3926) — for a cold
//	            lane that is a WARNING, not a pass. Every cell 0-or-hold = the
//	            lane is configured to send nothing.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHAT THIS FILE DELIBERATELY DOES NOT REIMPLEMENT (no second writer):
//
//	roster  → reuses laneRosterAssignPreflightSQL / laneRosterUpsertSQL and the
//	          existing PROPERTY_LEDGER_ROSTER_WRITE_ENABLED gate verbatim
//	          (property_lane_roster.go). A roster row changes live sending on the
//	          orchestrator's NEXT TICK, so bypassing that gate is not an option.
//	budgets → the Property Ledger owns budget EDITS, and on this platform a budget
//	          edit is never an immediate daily_budget write: it stages into
//	          pending_budget + pending_effective_day = tomorrow (Denver), behind
//	          an advisory-lock global ceiling and a lock_version CAS
//	          (property_ledger.go:700-713, :752-761). This service can only SEED a
//	          cell that does not exist yet — HandleUpdatePropertyLedger 404s on a
//	          missing cell ("ledger cell not found (seed pending?)",
//	          property_ledger.go:665), which is the exact gap a new lane falls
//	          into. The seed lands as daily_budget = 0 with the requested figure
//	          STAGED for tomorrow through the same propertyLedgerCeilingCheck +
//	          stageBudgetEdit the ledger itself uses. A brand-new cold lane
//	          therefore mails 0 on the day it is wired. That is the point.
//	          An EXISTING cell is never touched here — it is reported with its
//	          lock_version so the operator edits it on the Property Ledger.
//	brand   → brand code → sending domain goes through worker.BrandSendingDomain
//	 codes    (compiled map UNION the mailing_brand_metadata overlay,
//	          partner_drip_orchestrator.go:299/:332). The orchestrator's roster
//	          speaks lowercase codes (rru, wfy, mrd, hws, tot, yih) while
//	          mailing_brand_metadata.brand_code speaks the Python registry's
//	          scheme (RR, WF, MR, HW, TT, YI). Resolving one against the other
//	          returns nothing and reads as "no sending profile" for a lane that is
//	          demonstrably sending. Going through the domain sidesteps the whole
//	          vocabulary problem — see property_lane_brands.go:29-34.
//
// Org-scoped via GetOrgIDFromRequest on every handler. partner_drip_* carry no
// organization column (single-tenant partner-drip estate — property_lane_roster.go:32-37),
// so the org is echoed rather than used as a phantom SQL predicate that would
// silently return zero rows; mailing_sending_profiles and mailing_offers DO
// carry organization_id and ARE filtered by it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	isppkg "github.com/ignite/sparkpost-monitor/internal/pkg/isp"
	"github.com/ignite/sparkpost-monitor/internal/worker"
)

// ── write gate ──────────────────────────────────────────────────────────────

// dripLaneOnboardFlagEnv gates the ONBOARD (write) endpoint, mirroring
// laneRosterWriteFlagEnv (property_lane_roster.go:53) and
// laneThrottleWriteFlagEnv exactly: ships UNSET, so the endpoint is inert on
// deploy and the screen renders read-only with an honest banner naming the var.
// Setting it to "1" is the one-move enable; unsetting it is the one-move
// rollback. verify/options are ALWAYS live — a diagnostic must never be gated.
const dripLaneOnboardFlagEnv = "DRIP_LANE_ONBOARD_ENABLED"

func dripLaneOnboardEnabled() bool { return os.Getenv(dripLaneOnboardFlagEnv) == "1" }

// dripLaneDataGateTimeout bounds the two partner_clean_queue counts. That table
// is the largest in the estate and prod runs a 30s statement_timeout; a count
// that does not finish is reported as UNKNOWN, never as zero. A silent zero here
// would read as "nothing ingested" for a lane holding millions of records.
const dripLaneDataGateTimeout = 8 * time.Second

// ── gate model ──────────────────────────────────────────────────────────────

const (
	dripGatePass    = "pass"
	dripGateFail    = "fail"
	dripGateWarn    = "warn"
	dripGateUnknown = "unknown"
)

type dripLaneGate struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	// Fatal marks a gate whose failure means the lane CANNOT mail (verdict FAIL)
	// as opposed to one that means it mails in a degraded shape (verdict WARN).
	Fatal bool `json:"fatal"`
}

type dripLaneProfile struct {
	Brand          string `json:"brand"`
	SendingDomain  string `json:"sending_domain"`
	Transport      string `json:"transport"` // SES | KUMO | PMTA
	TrackingDomain string `json:"tracking_domain"`
	ProfileID      string `json:"profile_id"`
	Found          bool   `json:"found"`
}

type dripLaneOfferReadiness struct {
	OfferID       string   `json:"offer_id"`
	Name          string   `json:"name"`
	Status        string   `json:"status"`
	HasCreative   bool     `json:"has_creative"`
	HasSubjects   bool     `json:"has_subjects"`
	HasFromNames  bool     `json:"has_from_names"`
	Complete      bool     `json:"complete"`
	Missing       []string `json:"missing"`
	UsedByTouches []int    `json:"used_by_touches,omitempty"`
}

type dripLaneTouch struct {
	Touch            int    `json:"touch"`
	Source           string `json:"source"` // partner_drip_creatives | partner_drip_followup_creatives
	Scope            string `json:"scope"`  // vertical | global
	CreativeFilename string `json:"creative_filename"`
	OfferID          string `json:"offer_id,omitempty"`
	Active           bool   `json:"active"`
}

type dripLaneBudgetCell struct {
	ISP           string `json:"isp"`
	DailyBudget   int    `json:"daily_budget"`
	PendingBudget *int   `json:"pending_budget,omitempty"`
	PendingDay    string `json:"pending_effective_day,omitempty"`
	Hold          bool   `json:"hold"`
	LockVersion   int64  `json:"lock_version"`
}

type dripLaneVerifyResult struct {
	Vertical       string                   `json:"vertical"`
	Brand          string                   `json:"brand,omitempty"`
	Brands         []string                 `json:"brands"`
	Verdict        string                   `json:"verdict"` // PASS | WARN | FAIL
	Gates          []dripLaneGate           `json:"gates"`
	Profiles       []dripLaneProfile        `json:"profiles"`
	Touches        []dripLaneTouch          `json:"touches"`
	Offers         []dripLaneOfferReadiness `json:"offers"`
	Budgets        []dripLaneBudgetCell     `json:"budgets"`
	MaxTouch       int                      `json:"max_touch"`
	OrganizationID string                   `json:"organization_id"`
	WriteEnabled   bool                     `json:"write_enabled"`
	WriteFlagEnv   string                   `json:"write_flag_env"`
	RosterWrite    bool                     `json:"roster_write_enabled"`
	RosterFlagEnv  string                   `json:"roster_write_flag_env"`
}

// ── SQL (every constant here is asserted sidecar-free by the test) ──────────

// dripLaneDataTotalSQL / dripLaneDataReadySQL — both index-led on purpose.
// total uses uq_pcq_vertical_email_md5 (vertical, email_md5); ready uses the
// partial idx_pcq_status_ready (vertical, status, ingested_at) WHERE status='ready'
// (cmd/server/main.go:8316, :10627). A GROUP BY status over the whole vertical
// has no supporting index and seq-scans the biggest table in the estate.
const dripLaneDataTotalSQL = `SELECT count(*) FROM partner_clean_queue WHERE vertical = $1`
const dripLaneDataReadySQL = `SELECT count(*) FROM partner_clean_queue WHERE vertical = $1 AND status = 'ready'`

// dripLaneRosterSQL mirrors the orchestrator's normalization (lower(btrim(...)),
// partner_drip_orchestrator.go loadVerticalRosters) so a row written as
// "Refi_Heloc " is found here exactly as the orchestrator finds it.
const dripLaneRosterSQL = `
	SELECT lower(btrim(brand)), active
	FROM partner_drip_vertical_roster
	WHERE lower(btrim(vertical)) = $1
	ORDER BY active DESC, brand`

// dripLaneProfileSQL resolves the profile the way the send path does: by
// SENDING DOMAIN, never by brand code. via_ses DESC first mirrors the SES-relay
// preference; org-scoped because mailing_sending_profiles carries organization_id.
const dripLaneProfileSQL = `
	SELECT id::text, COALESCE(via_ses, FALSE), COALESCE(routing_mode, ''),
	       COALESCE(tracking_domain, '')
	FROM mailing_sending_profiles
	WHERE sending_domain = $1 AND organization_id = $2 AND status = 'active'
	ORDER BY COALESCE(via_ses, FALSE) DESC, created_at DESC
	LIMIT 1`

// dripLaneWelcomeSQL mirrors resolveCreative's predicate EXACTLY
// (partner_drip_orchestrator.go:1657-1661): plain equality, no lower/btrim. A
// row stored with different casing is invisible to the orchestrator, so it must
// be invisible here too — otherwise verify reports PASS on a lane that cannot
// resolve its own welcome creative.
const dripLaneWelcomeSQL = `
	SELECT COALESCE(creative_filename, ''), COALESCE(offer_id::text, ''), active
	FROM partner_drip_creatives
	WHERE vertical = $1 AND brand = $2`

// dripLaneFollowupSQL mirrors resolveFollowupCreative
// (partner_drip_orchestrator.go:4903-4909): vertical-specific rows win, rows
// with vertical IS NULL are the shared/global fallback chain.
const dripLaneFollowupSQL = `
	SELECT touch_number, COALESCE(creative_filename, ''), COALESCE(offer_id::text, ''),
	       active, (vertical IS NOT NULL AND vertical = $2) AS scoped
	FROM partner_drip_followup_creatives
	WHERE brand = $1 AND (vertical = $2 OR vertical IS NULL)
	ORDER BY touch_number, scoped DESC`

// dripLaneOfferPoolSQL is the three-table serving check. It reuses
// laneServingSubjectPredicate / laneServingFromNamePredicate
// (property_lane_content.go:66-67) and repeats the creative predicate that
// laneServingCreativeSQL pins to resolveOfferCreative — a naive COUNT(*)
// without these status filters is a FALSE PASS that hard-fails at send time.
const dripLaneOfferPoolSQL = `
	SELECT COALESCE(o.name, ''), COALESCE(o.status, ''),
	  EXISTS (SELECT 1 FROM mailing_offer_creatives
	          WHERE offer_id = o.id
	            AND COALESCE(status, '') NOT IN ('archived','rejected')
	            AND COALESCE(html_content, '') <> ''),
	  EXISTS (SELECT 1 FROM mailing_offer_subject_lines
	          WHERE offer_id = o.id AND ` + laneServingSubjectPredicate + `),
	  EXISTS (SELECT 1 FROM mailing_offer_from_names
	          WHERE offer_id = o.id AND ` + laneServingFromNamePredicate + `)
	FROM mailing_offers o
	WHERE o.id = $1::uuid AND o.organization_id = $2`

const dripLaneBudgetSQL = `
	SELECT isp, daily_budget, hold, pending_budget, pending_effective_day, lock_version
	FROM partner_drip_brand_budgets
	WHERE lower(btrim(brand)) = $1
	ORDER BY isp`

// dripLaneWelcomeUpsertSQL — conflict target is the table's PRIMARY KEY
// (vertical, brand), verified against the DDL at cmd/server/main.go:8332-8345.
// creative_filename = '' is the OFFER-CENTER path: resolveCreative branches to
// resolveOfferCreative only when the filename is empty AND offer_id is set
// (partner_drip_orchestrator.go:1671-1678).
const dripLaneWelcomeUpsertSQL = `
	INSERT INTO partner_drip_creatives
	       (vertical, brand, creative_filename, subject_line, preheader, from_name,
	        active, offer_id, updated_by, updated_at)
	VALUES ($1, $2, '', '(offer-center)', '', '(offer-center)', TRUE, $3::uuid, $4, NOW())
	ON CONFLICT (vertical, brand) DO UPDATE SET
	       active            = TRUE,
	       offer_id          = EXCLUDED.offer_id,
	       creative_filename = '',
	       subject_line      = '(offer-center)',
	       from_name         = '(offer-center)',
	       updated_by        = EXCLUDED.updated_by,
	       updated_at        = NOW()`

// dripLaneFollowupUpsertSQL — the conflict target is the EXPRESSION unique index
// partner_drip_followup_creatives_uq (brand, touch_number, COALESCE(vertical,''))
// (cmd/server/main.go:2178). The original (brand, touch_number) PK was DROPPED
// at :2177, so ON CONFLICT (vertical, brand, touch_number) — the shape a
// per-vertical table invites — does not resolve to any index and errors.
const dripLaneFollowupUpsertSQL = `
	INSERT INTO partner_drip_followup_creatives
	       (vertical, brand, touch_number, creative_filename, subject_line,
	        preheader, from_name, active, offer_id, updated_at)
	VALUES ($1, $2, $3, '', '(offer-center)', '', '(offer-center)', TRUE, $4::uuid, NOW())
	ON CONFLICT (brand, touch_number, COALESCE(vertical, '')) DO UPDATE SET
	       active            = TRUE,
	       offer_id          = EXCLUDED.offer_id,
	       creative_filename = '',
	       updated_at        = NOW()`

// dripLaneBudgetSeedSQL creates a ledger cell that does NOT exist yet, at
// daily_budget = 0 / hold = FALSE / lock_version = 0. The requested figure is
// then STAGED for tomorrow by the same stageBudgetEdit + pending_* columns the
// Property Ledger uses. ON CONFLICT DO NOTHING makes a concurrent seed a no-op
// rather than a silent overwrite of someone else's cell.
const dripLaneBudgetSeedSQL = `
	INSERT INTO partner_drip_brand_budgets (brand, isp, daily_budget, hold, updated_by, updated_at)
	VALUES ($1, $2, 0, FALSE, $3, NOW())
	ON CONFLICT (brand, isp) DO NOTHING`

const dripLaneBudgetStageSQL = `
	UPDATE partner_drip_brand_budgets
	SET pending_budget = $3, pending_effective_day = $4::date,
	    approved_by = $5, approved_at = NOW(),
	    updated_by = $5, updated_at = NOW(),
	    lock_version = lock_version + 1
	WHERE brand = $1 AND isp = $2`

// dripLaneSQLConstants is the list the no-sidecar test walks. Every SQL constant
// in this file MUST be registered here — the test also asserts the count, so a
// new statement cannot be added without being screened.
func dripLaneSQLConstants() map[string]string {
	return map[string]string{
		"dripLaneDataTotalSQL":       dripLaneDataTotalSQL,
		"dripLaneDataReadySQL":       dripLaneDataReadySQL,
		"dripLaneRosterSQL":          dripLaneRosterSQL,
		"dripLaneProfileSQL":         dripLaneProfileSQL,
		"dripLaneWelcomeSQL":         dripLaneWelcomeSQL,
		"dripLaneFollowupSQL":        dripLaneFollowupSQL,
		"dripLaneOfferPoolSQL":       dripLaneOfferPoolSQL,
		"dripLaneBudgetSQL":          dripLaneBudgetSQL,
		"dripLaneWelcomeUpsertSQL":   dripLaneWelcomeUpsertSQL,
		"dripLaneFollowupUpsertSQL":  dripLaneFollowupUpsertSQL,
		"dripLaneBudgetSeedSQL":      dripLaneBudgetSeedSQL,
		"dripLaneBudgetStageSQL":     dripLaneBudgetStageSQL,
		"dripLaneOptionsVerticalSQL": dripLaneOptionsVerticalSQL,
		"dripLaneOptionsOfferSQL":    dripLaneOptionsOfferSQL,
	}
}

// ── service ─────────────────────────────────────────────────────────────────

// DripLaneOnboardingService serves the lane readiness + onboarding surface.
type DripLaneOnboardingService struct{ db *sql.DB }

// NewDripLaneOnboardingService builds the service.
func NewDripLaneOnboardingService(db *sql.DB) *DripLaneOnboardingService {
	return &DripLaneOnboardingService{db: db}
}

// RegisterRoutes mounts the service under the /api/mailing group, so the final
// URLs are /api/mailing/drip-lane/{options,verify,onboard}.
func (s *DripLaneOnboardingService) RegisterRoutes(r chi.Router) {
	r.Route("/drip-lane", func(dr chi.Router) {
		dr.Get("/options", s.HandleOptions)
		dr.Get("/verify", s.HandleVerify)
		dr.Post("/onboard", s.HandleOnboard)
	})
}

// ── options ─────────────────────────────────────────────────────────────────

const dripLaneOptionsVerticalSQL = `
	SELECT v, bool_or(rostered) AS rostered, bool_or(has_dataset) AS has_dataset
	FROM (
		SELECT lower(btrim(vertical)) v, TRUE AS rostered, FALSE AS has_dataset
		FROM partner_drip_vertical_roster
		WHERE COALESCE(btrim(vertical), '') <> ''
		UNION ALL
		SELECT lower(btrim(vertical)) v, FALSE, TRUE
		FROM partner_datasets
		WHERE COALESCE(btrim(vertical), '') <> '' AND status = 'active'
	) u
	GROUP BY v
	ORDER BY v`

// dripLaneOptionsOfferSQL lists offers with their three-pool completeness
// inline, so the operator sees at SELECTION time that an offer would hard-fail
// at send time. Bounded by org + non-archived status; mailing_offers is a small
// table (tens of rows), so the three EXISTS per row are cheap.
const dripLaneOptionsOfferSQL = `
	SELECT o.id::text, COALESCE(o.name, ''), COALESCE(o.status, ''),
	  EXISTS (SELECT 1 FROM mailing_offer_creatives
	          WHERE offer_id = o.id
	            AND COALESCE(status, '') NOT IN ('archived','rejected')
	            AND COALESCE(html_content, '') <> ''),
	  EXISTS (SELECT 1 FROM mailing_offer_subject_lines
	          WHERE offer_id = o.id AND ` + laneServingSubjectPredicate + `),
	  EXISTS (SELECT 1 FROM mailing_offer_from_names
	          WHERE offer_id = o.id AND ` + laneServingFromNamePredicate + `)
	FROM mailing_offers o
	WHERE o.organization_id = $1 AND COALESCE(o.status, '') NOT IN ('archived','deleted')
	ORDER BY lower(o.name)`

type dripLaneVerticalOption struct {
	Vertical   string `json:"vertical"`
	Rostered   bool   `json:"rostered"`
	HasDataset bool   `json:"has_dataset"`
}

// HandleOptions GET /api/mailing/drip-lane/options
//
// Everything the screen's pickers need in ONE call: the verticals the write
// path will accept, every lane brand with its TRANSPORT and TRACKING DOMAIN
// (so a misroute is visible at selection, not after the first wave), the ISP
// authority the ledger accepts, and the offers with inline three-pool
// completeness.
func (s *DripLaneOnboardingService) HandleOptions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "organization could not be resolved")
		return
	}

	verticals := []dripLaneVerticalOption{}
	rows, err := s.db.QueryContext(ctx, dripLaneOptionsVerticalSQL)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "vertical options query failed")
		return
	}
	for rows.Next() {
		var v dripLaneVerticalOption
		if err := rows.Scan(&v.Vertical, &v.Rostered, &v.HasDataset); err != nil {
			rows.Close()
			respondError(w, http.StatusInternalServerError, "vertical options scan failed")
			return
		}
		verticals = append(verticals, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "vertical options query failed")
		return
	}

	brands := s.brandOptions(ctx, orgID)

	offers := []dripLaneOfferReadiness{}
	orows, err := s.db.QueryContext(ctx, dripLaneOptionsOfferSQL, orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "offer options query failed")
		return
	}
	for orows.Next() {
		var o dripLaneOfferReadiness
		if err := orows.Scan(&o.OfferID, &o.Name, &o.Status,
			&o.HasCreative, &o.HasSubjects, &o.HasFromNames); err != nil {
			orows.Close()
			respondError(w, http.StatusInternalServerError, "offer options scan failed")
			return
		}
		o.Missing = dripLaneMissingPools(o.HasCreative, o.HasSubjects, o.HasFromNames)
		o.Complete = len(o.Missing) == 0
		offers = append(offers, o)
	}
	orows.Close()
	if err := orows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "offer options query failed")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"verticals":                verticals,
		"brands":                   brands,
		"isps":                     isppkg.LedgerGroups(),
		"offers":                   offers,
		"max_touch":                worker.MaxTouchCount,
		"organization_id":          orgID.String(),
		"write_enabled":            dripLaneOnboardEnabled(),
		"write_flag_env":           dripLaneOnboardFlagEnv,
		"roster_write_enabled":     laneRosterWriteEnabled(),
		"roster_write_flag_env":    laneRosterWriteFlagEnv,
		"budget_effective_note":    "Budgets seeded here take effect TOMORROW (Denver) — the cell is created at 0/day today. Existing cells are never modified by this screen; edit them on the Property Ledger.",
		"cold_data_law":            "Cold data reaches production ONLY via dataset → partner ingest (EO) → partner_clean_queue → drip orchestrator. This screen configures orchestrator tables only; it never creates a campaign, writes partner_clean_queue, or touches a segment.",
		"budget_ceiling_env":       "PROPERTY_LEDGER_TOTAL_MAX",
	})
}

// brandOptions resolves every lane brand to its sending domain, transport and
// tracking domain. Never fails the request: a brand whose profile is missing is
// returned with Found=false so the picker can SHOW the gap rather than omit the
// brand and leave the operator wondering where it went.
func (s *DripLaneOnboardingService) brandOptions(ctx context.Context, orgID uuid.UUID) []dripLaneProfile {
	codes := map[string]bool{}
	for _, c := range worker.DripIntroBrands() {
		codes[strings.ToLower(strings.TrimSpace(c))] = true
	}
	// Union the ACTIVE roster codes — 'wcl' mails today and is not in the
	// compiled slice (property_lane_brands.go:20-24).
	if rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT lower(btrim(brand)) FROM partner_drip_vertical_roster
		WHERE active AND COALESCE(btrim(brand), '') <> ''`); err == nil {
		for rows.Next() {
			var b string
			if rows.Scan(&b) == nil && b != "" {
				codes[b] = true
			}
		}
		rows.Close()
	}
	out := make([]dripLaneProfile, 0, len(codes))
	for code := range codes {
		out = append(out, s.profileFor(ctx, code, orgID))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Brand < out[j].Brand })
	return out
}

// profileFor: brand code → sending domain (worker.BrandSendingDomain) → active
// sending profile. This is the send path's own chain; it never consults
// mailing_brand_metadata.brand_code, which speaks a different code scheme.
func (s *DripLaneOnboardingService) profileFor(ctx context.Context, brand string, orgID uuid.UUID) dripLaneProfile {
	p := dripLaneProfile{Brand: brand}
	domain, ok := worker.BrandSendingDomain(brand)
	if !ok || strings.TrimSpace(domain) == "" {
		return p
	}
	p.SendingDomain = domain
	var viaSES bool
	var routingMode string
	if err := s.db.QueryRowContext(ctx, dripLaneProfileSQL, domain, orgID).
		Scan(&p.ProfileID, &viaSES, &routingMode, &p.TrackingDomain); err != nil {
		return p
	}
	p.Found = true
	switch {
	case viaSES:
		p.Transport = "SES"
	case strings.EqualFold(strings.TrimSpace(routingMode), "kumo"):
		p.Transport = "KUMO"
	default:
		p.Transport = "PMTA"
	}
	return p
}

func dripLaneMissingPools(creative, subjects, fromNames bool) []string {
	missing := []string{}
	if !creative {
		missing = append(missing, "creatives")
	}
	if !subjects {
		missing = append(missing, "subject_lines")
	}
	if !fromNames {
		missing = append(missing, "from_names")
	}
	return missing
}

// ── verify ──────────────────────────────────────────────────────────────────

// HandleVerify GET /api/mailing/drip-lane/verify?vertical=X[&brand=Y]
//
// Read-only. Answers "why is this lane not mailing?" across all seven gates.
func (s *DripLaneOnboardingService) HandleVerify(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "organization could not be resolved")
		return
	}
	vertical := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("vertical")))
	if vertical == "" {
		respondError(w, http.StatusBadRequest, "vertical required")
		return
	}
	brand := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("brand")))
	if brand != "" && !propertyLedgerValidLaneBrand(ctx, s.db, brand) {
		respondError(w, http.StatusBadRequest, "unknown brand (not a compiled drip roster code and not actively rostered)")
		return
	}

	res, status, msg := s.verify(ctx, vertical, brand, orgID)
	if status != 0 {
		respondError(w, status, msg)
		return
	}
	respondJSON(w, http.StatusOK, res)
}

func (s *DripLaneOnboardingService) verify(ctx context.Context, vertical, brand string, orgID uuid.UUID) (*dripLaneVerifyResult, int, string) {
	out := &dripLaneVerifyResult{
		Vertical:       vertical,
		Brand:          brand,
		Brands:         []string{},
		Verdict:        "PASS",
		Gates:          []dripLaneGate{},
		Profiles:       []dripLaneProfile{},
		Touches:        []dripLaneTouch{},
		Offers:         []dripLaneOfferReadiness{},
		Budgets:        []dripLaneBudgetCell{},
		MaxTouch:       worker.MaxTouchCount,
		OrganizationID: orgID.String(),
		WriteEnabled:   dripLaneOnboardEnabled(),
		WriteFlagEnv:   dripLaneOnboardFlagEnv,
		RosterWrite:    laneRosterWriteEnabled(),
		RosterFlagEnv:  laneRosterWriteFlagEnv,
	}
	gate := func(name, status, detail string, fatal bool) {
		out.Gates = append(out.Gates, dripLaneGate{Name: name, Status: status, Detail: detail, Fatal: fatal})
		switch status {
		case dripGateFail:
			if fatal {
				out.Verdict = "FAIL"
			} else if out.Verdict == "PASS" {
				out.Verdict = "WARN"
			}
		case dripGateWarn, dripGateUnknown:
			if out.Verdict == "PASS" {
				out.Verdict = "WARN"
			}
		}
	}

	// ── 1 DATA ───────────────────────────────────────────────────────────────
	// Bounded and honest: a count that does not finish is UNKNOWN, never zero.
	dctx, cancel := context.WithTimeout(ctx, dripLaneDataGateTimeout)
	var total, ready int64
	errTotal := s.db.QueryRowContext(dctx, dripLaneDataTotalSQL, vertical).Scan(&total)
	var errReady error
	if errTotal == nil {
		errReady = s.db.QueryRowContext(dctx, dripLaneDataReadySQL, vertical).Scan(&ready)
	}
	cancel()
	switch {
	case errTotal != nil || errReady != nil:
		gate("DATA", dripGateUnknown,
			fmt.Sprintf("partner_clean_queue count did not complete within %s — supply is UNKNOWN, not zero. Re-run, or read the Data Partners lane health screen.", dripLaneDataGateTimeout),
			true)
	case total == 0:
		gate("DATA", dripGateFail,
			"0 records in partner_clean_queue for this vertical — nothing has been ingested. Cold data enters ONLY through partner ingest (EO-validated); this screen cannot and must not create records.",
			true)
	default:
		gate("DATA", dripGatePass,
			fmt.Sprintf("%d records · %d ready to claim", total, ready), true)
	}

	// ── 2 ROSTER ─────────────────────────────────────────────────────────────
	activeBrands, inactiveBrands, err := s.roster(ctx, vertical)
	if err != nil {
		return nil, http.StatusInternalServerError, "roster query failed"
	}
	out.Brands = activeBrands
	switch {
	case len(activeBrands) == 0 && len(inactiveBrands) == 0:
		gate("ROSTER", dripGateFail,
			"no partner_drip_vertical_roster row for this vertical — it routes to no sending domain and the orchestrator will never pick it up", true)
	case len(activeBrands) == 0:
		gate("ROSTER", dripGateFail,
			fmt.Sprintf("every roster row is INACTIVE (soft-disabled): %s", strings.Join(inactiveBrands, ", ")), true)
	default:
		detail := "active brands: " + strings.Join(activeBrands, ", ")
		if len(inactiveBrands) > 0 {
			detail += " · inactive: " + strings.Join(inactiveBrands, ", ")
		}
		gate("ROSTER", dripGatePass, detail, true)
	}

	// The brands the remaining gates are evaluated against.
	brands := activeBrands
	if brand != "" {
		brands = []string{brand}
	}

	// ── 3 PROFILE ────────────────────────────────────────────────────────────
	profOK := len(brands) > 0
	profDetail := []string{}
	for _, b := range brands {
		p := s.profileFor(ctx, b, orgID)
		out.Profiles = append(out.Profiles, p)
		if !p.Found {
			profOK = false
			if p.SendingDomain == "" {
				profDetail = append(profDetail, b+": no sending domain (brand unknown to the orchestrator)")
			} else {
				profDetail = append(profDetail, b+": "+p.SendingDomain+" has NO ACTIVE SENDING PROFILE")
			}
			continue
		}
		td := p.TrackingDomain
		if td == "" {
			td = "(global fallback)"
		}
		profDetail = append(profDetail, fmt.Sprintf("%s: %s [%s] track=%s", b, p.SendingDomain, p.Transport, td))
	}
	if len(brands) == 0 {
		gate("PROFILE", dripGateFail, "no brand to check (roster is empty)", true)
	} else if profOK {
		gate("PROFILE", dripGatePass, strings.Join(profDetail, " · "), true)
	} else {
		gate("PROFILE", dripGateFail, strings.Join(profDetail, " · "), true)
	}

	// ── 4 WELCOME / 5 FOLLOWUP ───────────────────────────────────────────────
	offerUse := map[string][]int{}
	welOK, fuOK := len(brands) > 0, len(brands) > 0
	welDetail, fuDetail := []string{}, []string{}
	for _, b := range brands {
		wActive, wRows, err := s.welcome(ctx, vertical, b)
		if err != nil {
			return nil, http.StatusInternalServerError, "welcome creative query failed"
		}
		out.Touches = append(out.Touches, wRows...)
		if !wActive {
			welOK = false
			welDetail = append(welDetail, b+": no ACTIVE partner_drip_creatives row — resolveCreative returns no rows and the welcome wave cannot deploy")
		} else {
			welDetail = append(welDetail, b+": configured")
		}
		for _, t := range wRows {
			if t.Active && t.OfferID != "" && t.CreativeFilename == "" {
				offerUse[t.OfferID] = append(offerUse[t.OfferID], t.Touch)
			}
		}

		fRows, err := s.followups(ctx, vertical, b)
		if err != nil {
			return nil, http.StatusInternalServerError, "follow-up creative query failed"
		}
		out.Touches = append(out.Touches, fRows...)
		serving := []int{}
		dead := []int{}
		for _, t := range fRows {
			if !t.Active {
				continue
			}
			// Touch 1 in the follow-up table is dead configuration: the
			// follow-up pass only ever asks for 2..MaxTouchCount.
			if t.Touch < 2 || t.Touch > worker.MaxTouchCount {
				dead = append(dead, t.Touch)
				continue
			}
			serving = append(serving, t.Touch)
			if t.OfferID != "" && t.CreativeFilename == "" {
				offerUse[t.OfferID] = append(offerUse[t.OfferID], t.Touch)
			}
		}
		switch {
		case len(serving) == 0:
			fuOK = false
			fuDetail = append(fuDetail, b+": no ACTIVE follow-up rows in touches 2.."+fmt.Sprint(worker.MaxTouchCount)+" — the ladder is welcome-only")
		default:
			d := fmt.Sprintf("%s: touches %v", b, dedupeInts(serving))
			if len(dead) > 0 {
				d += fmt.Sprintf(" · %v INERT (outside 2..%d — never resolved by the follow-up pass)", dedupeInts(dead), worker.MaxTouchCount)
			}
			fuDetail = append(fuDetail, d)
		}
	}
	if welOK {
		gate("WELCOME", dripGatePass, strings.Join(welDetail, " · "), true)
	} else {
		gate("WELCOME", dripGateFail, joinOrDash(welDetail), true)
	}
	// FOLLOWUP is non-fatal: a welcome-only lane mails, it just never ladders.
	if fuOK {
		gate("FOLLOWUP", dripGatePass, strings.Join(fuDetail, " · "), false)
	} else {
		gate("FOLLOWUP", dripGateFail, joinOrDash(fuDetail), false)
	}

	// ── 6 OFFER ──────────────────────────────────────────────────────────────
	offerIDs := make([]string, 0, len(offerUse))
	for id := range offerUse {
		offerIDs = append(offerIDs, id)
	}
	sort.Strings(offerIDs)
	offOK := true
	offDetail := []string{}
	for _, id := range offerIDs {
		o, err := s.offerReadiness(ctx, id, orgID)
		if err != nil {
			return nil, http.StatusInternalServerError, "offer pool query failed"
		}
		o.UsedByTouches = dedupeInts(offerUse[id])
		out.Offers = append(out.Offers, o)
		if !o.Complete {
			offOK = false
			name := o.Name
			if name == "" {
				name = id
			}
			offDetail = append(offDetail,
				fmt.Sprintf("%s: MISSING %s — send-time resolve hard-fails", name, strings.Join(o.Missing, "+")))
			continue
		}
		offDetail = append(offDetail, o.Name+": complete")
	}
	switch {
	case len(offerIDs) == 0:
		gate("OFFER", dripGatePass, "no offer-center touch in use (creatives resolve from disk)", true)
	case offOK:
		gate("OFFER", dripGatePass, strings.Join(offDetail, " · "), true)
	default:
		gate("OFFER", dripGateFail, strings.Join(offDetail, " · "), true)
	}

	// ── 7 CAPS ───────────────────────────────────────────────────────────────
	capOK, capWarn := len(brands) > 0, false
	capDetail := []string{}
	for _, b := range brands {
		cells, err := s.budgets(ctx, b)
		if err != nil {
			return nil, http.StatusInternalServerError, "budget query failed"
		}
		out.Budgets = append(out.Budgets, cells...)
		live := 0
		held := 0
		totalBudget := 0
		for _, c := range cells {
			if c.Hold {
				held++
				continue
			}
			if c.DailyBudget > 0 {
				live++
				totalBudget += c.DailyBudget
			}
		}
		switch {
		case len(cells) == 0:
			capWarn = true
			capDetail = append(capDetail, b+": NO ledger rows — an empty ledger is UNCONSTRAINED, not zero. A cold lane should carry explicit per-ISP budgets.")
		case live == 0:
			capOK = false
			capDetail = append(capDetail, fmt.Sprintf("%s: every cell is 0 or held (%d held) — the lane is configured to send nothing", b, held))
		default:
			d := fmt.Sprintf("%s: %d live lanes, %d/day", b, live, totalBudget)
			if held > 0 {
				d += fmt.Sprintf(" · %d HELD", held)
			}
			capDetail = append(capDetail, d)
		}
	}
	switch {
	case len(brands) == 0:
		gate("CAPS", dripGateFail, "no brand to check (roster is empty)", true)
	case !capOK:
		gate("CAPS", dripGateFail, strings.Join(capDetail, " · "), true)
	case capWarn:
		gate("CAPS", dripGateWarn, strings.Join(capDetail, " · "), false)
	default:
		gate("CAPS", dripGatePass, strings.Join(capDetail, " · "), true)
	}

	return out, 0, ""
}

func joinOrDash(v []string) string {
	if len(v) == 0 {
		return "-"
	}
	return strings.Join(v, " · ")
}

func dedupeInts(in []int) []int {
	seen := map[int]bool{}
	out := []int{}
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out
}

func (s *DripLaneOnboardingService) roster(ctx context.Context, vertical string) (active, inactive []string, err error) {
	rows, err := s.db.QueryContext(ctx, dripLaneRosterSQL, vertical)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	active, inactive = []string{}, []string{}
	for rows.Next() {
		var b string
		var isActive bool
		if err := rows.Scan(&b, &isActive); err != nil {
			return nil, nil, err
		}
		if isActive {
			active = append(active, b)
		} else {
			inactive = append(inactive, b)
		}
	}
	return active, inactive, rows.Err()
}

func (s *DripLaneOnboardingService) welcome(ctx context.Context, vertical, brand string) (bool, []dripLaneTouch, error) {
	rows, err := s.db.QueryContext(ctx, dripLaneWelcomeSQL, vertical, brand)
	if err != nil {
		return false, nil, err
	}
	defer rows.Close()
	out := []dripLaneTouch{}
	anyActive := false
	for rows.Next() {
		t := dripLaneTouch{Touch: 1, Source: "partner_drip_creatives", Scope: "vertical"}
		if err := rows.Scan(&t.CreativeFilename, &t.OfferID, &t.Active); err != nil {
			return false, nil, err
		}
		if t.Active {
			anyActive = true
		}
		out = append(out, t)
	}
	return anyActive, out, rows.Err()
}

func (s *DripLaneOnboardingService) followups(ctx context.Context, vertical, brand string) ([]dripLaneTouch, error) {
	rows, err := s.db.QueryContext(ctx, dripLaneFollowupSQL, brand, vertical)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []dripLaneTouch{}
	for rows.Next() {
		t := dripLaneTouch{Source: "partner_drip_followup_creatives"}
		var scoped bool
		if err := rows.Scan(&t.Touch, &t.CreativeFilename, &t.OfferID, &t.Active, &scoped); err != nil {
			return nil, err
		}
		t.Scope = "global"
		if scoped {
			t.Scope = "vertical"
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *DripLaneOnboardingService) offerReadiness(ctx context.Context, offerID string, orgID uuid.UUID) (dripLaneOfferReadiness, error) {
	o := dripLaneOfferReadiness{OfferID: offerID}
	err := s.db.QueryRowContext(ctx, dripLaneOfferPoolSQL, offerID, orgID).
		Scan(&o.Name, &o.Status, &o.HasCreative, &o.HasSubjects, &o.HasFromNames)
	if err == sql.ErrNoRows {
		// A touch bound to an offer this org cannot see is a real defect, not an
		// empty result — surface it as incomplete rather than skipping it.
		o.Missing = []string{"offer row (not found in this organization)"}
		return o, nil
	}
	if err != nil {
		return o, err
	}
	o.Missing = dripLaneMissingPools(o.HasCreative, o.HasSubjects, o.HasFromNames)
	o.Complete = len(o.Missing) == 0
	return o, nil
}

func (s *DripLaneOnboardingService) budgets(ctx context.Context, brand string) ([]dripLaneBudgetCell, error) {
	rows, err := s.db.QueryContext(ctx, dripLaneBudgetSQL, brand)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []dripLaneBudgetCell{}
	for rows.Next() {
		var c dripLaneBudgetCell
		var pend sql.NullInt64
		var pendDay sql.NullTime
		if err := rows.Scan(&c.ISP, &c.DailyBudget, &c.Hold, &pend, &pendDay, &c.LockVersion); err != nil {
			return nil, err
		}
		if pend.Valid {
			v := int(pend.Int64)
			c.PendingBudget = &v
		}
		if pendDay.Valid {
			c.PendingDay = pendDay.Time.Format("2006-01-02")
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ── onboard ─────────────────────────────────────────────────────────────────

type dripLaneBudgetReq struct {
	ISP         string `json:"isp"`
	DailyBudget int    `json:"daily_budget"`
}

type dripLaneOnboardReq struct {
	Vertical string `json:"vertical"`
	Brand    string `json:"brand"`
	OfferID  string `json:"offer_id"`
	// Touches is the TOTAL ladder length INCLUDING the welcome (touch 1).
	// Follow-up rows are written for 2..Touches, because the follow-up pass
	// computes touchNum = MAX(touch_count)+1 and rejects anything outside
	// 2..MaxTouchCount (partner_drip_orchestrator.go:4666-4669) — a follow-up
	// row at touch_number 1 is never resolved.
	Touches   int                 `json:"touches"`
	Weight    int                 `json:"weight"`
	SortOrder int                 `json:"sort_order"`
	Budgets   []dripLaneBudgetReq `json:"budgets"`
	Confirm   bool                `json:"confirm"`
}

type dripLaneBudgetOutcome struct {
	ISP     string `json:"isp"`
	Action  string `json:"action"` // seeded | existing | refused
	Staged  int    `json:"staged_budget,omitempty"`
	Day     string `json:"effective_day,omitempty"`
	Message string `json:"message,omitempty"`
}

// HandleOnboard POST /api/mailing/drip-lane/onboard
//
// Wires gates 2 (roster), 4 (welcome), 5 (follow-up ladder) and 7 (budget
// cells). Gates 1 (DATA), 3 (PROFILE) and 6 (OFFER) are ASSERTED, never
// invented: a missing sending profile or an offer without all three serving
// pools stops the run with a 422 naming exactly what is absent, because
// guessing either is how mail goes out misrouted or unattributed.
func (s *DripLaneOnboardingService) HandleOnboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Gate FIRST, so a gated request executes ZERO SQL.
	if !dripLaneOnboardEnabled() {
		respondError(w, http.StatusForbidden,
			"drip-lane onboarding writes are disabled: set the server env "+dripLaneOnboardFlagEnv+
				"=1 to enable (unsetting it is the one-move rollback)")
		return
	}
	// The roster row is a LIVE sending change on the orchestrator's next tick.
	// It has its own operator gate and this endpoint does not get to bypass it.
	if !laneRosterWriteEnabled() {
		respondError(w, http.StatusForbidden,
			"roster writes are disabled: set the server env "+laneRosterWriteFlagEnv+
				"=1 to enable (this endpoint writes partner_drip_vertical_roster and will not bypass that gate)")
		return
	}

	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "organization could not be resolved")
		return
	}

	var req dripLaneOnboardReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !req.Confirm {
		respondError(w, http.StatusBadRequest,
			"confirm:true required — this writes live orchestrator configuration")
		return
	}
	vertical := strings.ToLower(strings.TrimSpace(req.Vertical))
	brand := strings.ToLower(strings.TrimSpace(req.Brand))
	offerID := strings.TrimSpace(req.OfferID)
	if vertical == "" {
		respondError(w, http.StatusBadRequest, "vertical required")
		return
	}
	if !propertyLedgerValidLaneBrand(ctx, s.db, brand) {
		respondError(w, http.StatusBadRequest, "unknown brand (not a compiled drip roster code and not actively rostered)")
		return
	}
	if _, err := uuid.Parse(offerID); err != nil {
		respondError(w, http.StatusBadRequest, "offer_id must be a UUID")
		return
	}
	if req.Touches < 1 || req.Touches > worker.MaxTouchCount {
		respondError(w, http.StatusBadRequest,
			fmt.Sprintf("touches must be 1..%d (the total ladder INCLUDING the welcome; the orchestrator rejects follow-up touch numbers outside 2..%d)",
				worker.MaxTouchCount, worker.MaxTouchCount))
		return
	}
	if req.SortOrder < 0 {
		respondError(w, http.StatusBadRequest, "sort_order must be >= 0")
		return
	}
	actor := actorFromRequest(r)

	// ── ASSERT gate 3 PROFILE ────────────────────────────────────────────────
	prof := s.profileFor(ctx, brand, orgID)
	if !prof.Found {
		if prof.SendingDomain == "" {
			respondError(w, http.StatusUnprocessableEntity,
				"brand "+brand+" resolves to no sending domain — onboard the domain first; this endpoint will not guess one")
			return
		}
		respondError(w, http.StatusUnprocessableEntity,
			"brand "+brand+" ("+prof.SendingDomain+") has no ACTIVE sending profile in this organization — onboard the domain first; this endpoint will not guess one")
		return
	}

	// ── ASSERT gate 6 OFFER ──────────────────────────────────────────────────
	offer, err := s.offerReadiness(ctx, offerID, orgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "offer pool preflight failed")
		return
	}
	if !offer.Complete {
		respondError(w, http.StatusUnprocessableEntity,
			"offer "+offerID+" is missing "+strings.Join(offer.Missing, " + ")+
				" — all three of mailing_offer_creatives / _subject_lines / _from_names must hold a SERVING (non-archived, non-empty) row or the send-time resolve hard-fails. Wire the offer in Creative Studio first.")
		return
	}

	// ── validate budgets before opening the tx ───────────────────────────────
	seen := map[string]bool{}
	for _, b := range req.Budgets {
		isp := strings.ToLower(strings.TrimSpace(b.ISP))
		if !propertyLedgerValidISP(isp) {
			respondError(w, http.StatusBadRequest, "unknown isp "+b.ISP+" (must be in the ledger ISP authority)")
			return
		}
		if b.DailyBudget < 0 {
			respondError(w, http.StatusBadRequest, "daily_budget must be >= 0 for isp "+isp)
			return
		}
		if seen[isp] {
			respondError(w, http.StatusBadRequest, "duplicate isp "+isp+" in budgets")
			return
		}
		seen[isp] = true
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "tx begin failed")
		return
	}
	defer tx.Rollback()

	// ── gate 2 ROSTER — reuses the Property Ledger's own statements ──────────
	var (
		verticalKnown bool
		curWeight     sql.NullInt64
		curActive     sql.NullBool
		curSortOrder  sql.NullInt64
	)
	if err := tx.QueryRowContext(ctx, laneRosterAssignPreflightSQL, vertical, brand).
		Scan(&verticalKnown, &curWeight, &curActive, &curSortOrder); err != nil {
		respondError(w, http.StatusInternalServerError, "roster preflight failed")
		return
	}
	if !verticalKnown {
		respondError(w, http.StatusBadRequest,
			"unknown vertical: no partner_drip_vertical_roster row and no partner_datasets feed uses it — "+
				"a typo here would create a phantom lane the orchestrator loads every tick. Ingest the file first.")
		return
	}
	weight := clampLaneRosterWeight(req.Weight)
	var rosterRow laneRosterRow
	rosterRow.Vertical, rosterRow.Brand = vertical, brand
	if err := tx.QueryRowContext(ctx, laneRosterUpsertSQL,
		vertical, brand, weight, req.SortOrder, actor).
		Scan(&rosterRow.Weight, &rosterRow.Active, &rosterRow.SortOrder,
			&rosterRow.UpdatedAt, &rosterRow.UpdatedBy); err != nil {
		respondError(w, http.StatusInternalServerError, "roster upsert failed")
		return
	}

	// ── gate 4 WELCOME (touch 1) ─────────────────────────────────────────────
	if _, err := tx.ExecContext(ctx, dripLaneWelcomeUpsertSQL, vertical, brand, offerID, actor); err != nil {
		respondError(w, http.StatusInternalServerError, "welcome creative upsert failed")
		return
	}

	// ── gate 5 FOLLOWUP (touches 2..Touches) ────────────────────────────────
	ladder := []int{}
	for t := 2; t <= req.Touches; t++ {
		if _, err := tx.ExecContext(ctx, dripLaneFollowupUpsertSQL, vertical, brand, t, offerID); err != nil {
			respondError(w, http.StatusInternalServerError,
				fmt.Sprintf("follow-up creative upsert failed at touch %d", t))
			return
		}
		ladder = append(ladder, t)
	}

	// ── gate 7 CAPS — SEED-ONLY, staged for tomorrow (Denver) ───────────────
	tomorrow := propertyLedgerTomorrow()
	budgetOutcomes := []dripLaneBudgetOutcome{}
	for _, b := range req.Budgets {
		isp := strings.ToLower(strings.TrimSpace(b.ISP))
		res, err := tx.ExecContext(ctx, dripLaneBudgetSeedSQL, brand, isp, actor)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "budget cell seed failed for isp "+isp)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// The cell already exists. The Property Ledger owns edits — it holds
			// the lock_version CAS, the min/max clamp and the hold-interval
			// bookkeeping. Silently overwriting it from here would be a second
			// writer to a live cap.
			budgetOutcomes = append(budgetOutcomes, dripLaneBudgetOutcome{
				ISP: isp, Action: "existing",
				Message: "cell already exists and was NOT modified — edit it on the Property Ledger (POST /api/mailing/pmta-campaign/property-ledger/update), which owns budget edits",
			})
			continue
		}
		if b.DailyBudget == 0 {
			budgetOutcomes = append(budgetOutcomes, dripLaneBudgetOutcome{
				ISP: isp, Action: "seeded", Staged: 0,
				Message: "cell created at 0/day; nothing staged",
			})
			continue
		}
		// Same ceiling the Property Ledger enforces. A freshly seeded cell's
		// current effective-tomorrow budget is 0, so this IS an increase and is
		// refused outright when PROPERTY_LEDGER_TOTAL_MAX is unset — deliberately.
		if status, msg := propertyLedgerCeilingCheck(ctx, tx, brand, isp, b.DailyBudget, 0, tomorrow); status != 0 {
			respondError(w, status, "isp "+isp+": "+msg)
			return
		}
		if err := stageBudgetEdit(ctx, tx, brand, isp, b.DailyBudget, tomorrow, actor, "lane-onboard"); err != nil {
			respondError(w, http.StatusInternalServerError, "budget version write failed for isp "+isp)
			return
		}
		if _, err := tx.ExecContext(ctx, dripLaneBudgetStageSQL,
			brand, isp, b.DailyBudget, tomorrow, actor); err != nil {
			respondError(w, http.StatusInternalServerError, "budget stage failed for isp "+isp)
			return
		}
		budgetOutcomes = append(budgetOutcomes, dripLaneBudgetOutcome{
			ISP: isp, Action: "seeded", Staged: b.DailyBudget, Day: tomorrow,
			Message: "cell created at 0/day today; " + fmt.Sprint(b.DailyBudget) + "/day effective " + tomorrow + " (Denver)",
		})
	}

	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "commit failed")
		return
	}

	// A roster write changes what propertyLedgerValidLaneBrand accepts.
	invalidateLaneBrandCache()

	before := map[string]interface{}{"roster_exists": curWeight.Valid}
	if curWeight.Valid {
		before["weight"] = curWeight.Int64
		before["active"] = curActive.Bool
		before["sort_order"] = curSortOrder.Int64
	}
	writeAuditLog(ctx, s.db, actor, "drip_lane_onboard", "drip_lane", vertical+"/"+brand,
		before,
		map[string]interface{}{
			"vertical": vertical, "brand": brand, "offer_id": offerID,
			"sending_domain": prof.SendingDomain, "transport": prof.Transport,
			"tracking_domain": prof.TrackingDomain,
			"weight":          rosterRow.Weight, "weight_requested": req.Weight,
			"sort_order": rosterRow.SortOrder,
			"touches":    req.Touches, "followup_touches": ladder,
			"budgets": budgetOutcomes, "organization_id": orgID.String(),
		})

	// Re-verify on the live tree so the response is measured, not asserted.
	after, status, msg := s.verify(ctx, vertical, brand, orgID)
	if status != 0 {
		respondError(w, status, msg)
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok":               true,
		"vertical":         vertical,
		"brand":            brand,
		"offer_id":         offerID,
		"sending_domain":   prof.SendingDomain,
		"transport":        prof.Transport,
		"tracking_domain":  prof.TrackingDomain,
		"roster":           rosterRow,
		"weight_requested": req.Weight,
		"weight_clamped":   rosterRow.Weight != req.Weight,
		"followup_touches": ladder,
		"budgets":          budgetOutcomes,
		"budget_note":      "Seeded cells are 0/day today and take effect tomorrow (Denver), exactly like every other budget change on this platform. Existing cells were left untouched.",
		"verify":           after,
		"organization_id":  orgID.String(),
	})
}
