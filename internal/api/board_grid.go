package api

// Board Grid — the send-day board as a PROPERTY × SLOT grid you edit by
// exception, that arrives already knowing what is wrong with it.
//
//	GET  /api/mailing/board-grid?date=YYYY-MM-DD     the day, as a grid, gated
//	GET  /api/mailing/board-grid/clone?from=&to=     yesterday's grid re-dated (NO WRITE)
//	GET  /api/mailing/board-grid/creative?campaign_id= what a live cell will SAY
//	                                                 (board_grid_content.go)
//	POST /api/mailing/board-grid/gates               run the gates over a proposed grid
//	POST /api/mailing/board-grid/stage               stage a proposal as campaigns
//	                                                 (lives in board_grid_stage.go —
//	                                                 THIS file stays read-only)
//
// Clone does NOT retime anything: the proposal keeps the source day's slots —
// the stable structure — and only rewrites the date token inside each name
// (both the 8-digit '08222026' form and the month-name 'aug22' form) to the
// target date. Each proposal cell KEEPS the source campaign's id as
// source_campaign_id: that id is how /stage later loads the source's
// byte-faithful deploy blob (pmta_config->'campaign_input').
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY THIS EXISTS
//
// The structure of a send-day (which properties mail, in which slots) is stable
// day over day. The only daily decision is WHICH OFFER sits in each cell.
// Everything else — sending domain, audience, timing, creative, copy, name — is
// derivable. Typing it by hand 48 times a day is what produced every defect on
// the 08/19–08/22 boards:
//
//	'MF' for MH, '080202026', '08062026'          → NAME
//	a DB-named campaign sending from consumerpro  → NAME_PROPERTY_MISMATCH
//	RR-Globe and RB-ADR finalizing with NULL offer→ MISSING_OFFER
//	RR-HELOC shipping a Liquid {{custom.*}} subject→ LIQUID_SUBJECT
//	WF and BWP double-booked at one anchor        → SLOT_COLLISION
//
// Each of those is a rule, not a judgement, so each is a gate below. The offer
// assignment stays the operator's — this file never picks one.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHAT COUNTS AS "THE BOARD"
//
// 2026-08-22 carried 692 campaigns, of which only 50 are the board. They are NOT
// separable by execution_mode — all 692 are 'pmta_isp_wave'. The discriminator
// is partner_drip_tag IS NULL AND journey_id IS NULL: the ~644 others are
// orchestrator-created '[partner-drip] <vertical> <brand> <ts>' sends firing at
// scattered evening minutes. A name regex ('^[0-9]{8} - ') returns 48 and
// silently DROPS the two kumo warm-ramp campaigns ('aug22 - aad - KUMO-WARM'),
// which are operator-scheduled broadcasts and belong on the grid. Do not
// "simplify" this predicate back to a name match.
//
// ─────────────────────────────────────────────────────────────────────────────
// READ-ONLY BY CONSTRUCTION
//
// Every statement in THIS file is a SELECT. Cloning returns a PROPOSAL — the
// source day's slots kept, names re-dated — and does not create campaigns.
// The write path is POST /board-grid/stage (board_grid_stage.go), which is
// CREATE-ONLY and deploys each cell id-less through the FULL existing gated
// deploy path (PMTACampaignService.deployFromInput — audience planning, the
// wave sanity check, the offer gate, the time_spans[*].source contract).
// There is no second send path and none may be added —
// TestBoardGrid_ReadOnly asserts this file stays read-only.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// BoardGridService serves the property × slot view of a send-day.
type BoardGridService struct {
	db *sql.DB
	// campaigns is the deploy dependency for POST /board-grid/stage
	// (board_grid_stage.go): staging deploys each cell id-less through
	// campaigns.deployFromInput — the FULL gated path. nil until
	// SetCampaignService is called at route wiring; the stage handler
	// answers 503 without it. Everything in THIS file ignores it.
	campaigns *PMTACampaignService
}

func NewBoardGridService(db *sql.DB) *BoardGridService {
	return &BoardGridService{db: db}
}

func (s *BoardGridService) RegisterRoutes(r chi.Router) {
	r.Route("/board-grid", func(cr chi.Router) {
		cr.Get("/", s.HandleGetGrid)
		cr.Get("/clone", s.HandleCloneGrid)
		cr.Get("/creative", s.HandleGetCellCreative) // board_grid_content.go — read-only
		cr.Post("/gates", s.HandleRunGates)
		cr.Post("/stage", s.HandleStageGrid) // board_grid_stage.go — CREATE-ONLY
	})
}

// boardCampaignPredicate isolates the operator-scheduled broadcast board from
// the orchestrator's partner-drip sends. See the WHAT COUNTS AS "THE BOARD"
// note above before changing it.
const boardCampaignPredicate = `
    c.partner_drip_tag IS NULL
AND c.journey_id IS NULL
AND c.name NOT LIKE '[partner-drip]%'
AND c.status NOT IN ('cancelled','deleted')`

// The name exclusion is BELT AND SUSPENDERS on top of the tag discriminator,
// not a replacement for it (see the warning above): the drip orchestrator's
// attribution stamp is logged-non-fatal, so a stamp failure leaves
// partner_drip_tag NULL and the drip campaign leaked onto the grid (found
// live 2026-08-22: '[partner-drip] consumer tot ...' produced a phantom
// REPEAT_OFFER blocker on TT). Named drip campaigns are never the board.

// BoardCell is one (property, slot) assignment.
type BoardCell struct {
	Property      string `json:"property"`       // brand code, e.g. "DB"
	PropertyLabel string `json:"property_label"` // e.g. "Discount Blog"
	SendingDomain string `json:"sending_domain"`
	BrandRoot     string `json:"brand_root,omitempty"`
	Slot          string `json:"slot"` // Denver HH:MM
	CampaignID    string `json:"campaign_id,omitempty"`
	// SourceCampaignID is set ONLY on clone-proposal cells: the id of the
	// source day's campaign this cell was cloned from. POST /board-grid/stage
	// loads that campaign's pmta_config->'campaign_input' blob as the deploy
	// payload — without it a proposal cell cannot be staged.
	SourceCampaignID string `json:"source_campaign_id,omitempty"`
	Name             string `json:"name"`
	OfferID          string `json:"offer_id,omitempty"`
	OfferName        string `json:"offer_name,omitempty"`
	Subject          string `json:"subject,omitempty"`
	// WHAT WILL ACTUALLY SHIP. The grid used to carry the subject alone, so
	// the preheader, the friendly-from and the creative were invisible until
	// the mail was already out (operator 2026-08-23: "I do not know the
	// subject lines, preheaders and content it is going to use"). These are
	// the stored columns the send worker reads — not the offer's proof, which
	// is only what a REBUILD would install.
	Preheader   string `json:"preheader,omitempty"`
	FromName    string `json:"from_name,omitempty"`
	FromEmail   string `json:"from_email,omitempty"`
	CreativeLen int    `json:"creative_len,omitempty"`
	// SubjectRendered is Subject put through the same Liquid engine the send
	// worker uses, against a recipient with NO personalization data — the
	// ~90% case. Equal to Subject when the subject carries no tokens.
	SubjectRendered   string `json:"subject_rendered,omitempty"`
	PreheaderRendered string `json:"preheader_rendered,omitempty"`
	Status            string `json:"status,omitempty"`
	Recipients        int    `json:"recipients"`
	Proposed          bool   `json:"proposed,omitempty"`
	// PendingFinalize marks a row the AudienceFinalizationWorker has not
	// finished yet (status finalizing_audience/preparing). offer_id and
	// sending_profile_id are both written at the END of finalization
	// (pmta_campaign_persistence.go:186), so a pending cell's NULL offer is
	// "not yet", not "missing" — Gate 3 warns instead of blocking.
	PendingFinalize bool `json:"pending_finalize,omitempty"`
	// FailureReason is the recorded cause of a status='failed' row —
	// markCampaignFailed writes it into pmta_config->>'failure_reason'.
	// Empty on rows that failed before the reason was recorded (pre-fix
	// deploys), which Gate 7 says explicitly instead of showing nothing.
	FailureReason string `json:"failure_reason,omitempty"`
	// StuckFinalize marks a row that has sat in finalizing_audience/preparing
	// beyond the finalizer's plausible runtime (15 min) — the worker pool may
	// be stalled, and OFFER_PENDING's "re-check shortly" no longer applies.
	StuckFinalize bool `json:"stuck_finalize,omitempty"`
}

// BoardFinding is one gate result against one cell.
type BoardFinding struct {
	Level    string `json:"level"` // "blocker" | "warn"
	Code     string `json:"code"`
	Property string `json:"property"`
	Slot     string `json:"slot"`
	Message  string `json:"message"`
}

type BoardGrid struct {
	Date       string         `json:"date"`
	Slots      []string       `json:"slots"`
	Properties []string       `json:"properties"`
	Cells      []BoardCell    `json:"cells"`
	Findings   []BoardFinding `json:"findings"`
	Summary    map[string]int `json:"summary"`
	SourceDate string         `json:"source_date,omitempty"`
	// StaleAsOf is set ONLY when the live load failed and this payload was
	// served from the success cache instead (up to boardGridCacheStaleTTL
	// old). Absent on a live or fresh-cache read.
	StaleAsOf *time.Time `json:"stale_as_of,omitempty"`
}

// ---------------------------------------------------------------------------
// Per-(org, date) success cache.
//
// 2026-08-22: with RDS IO-starved (EBS burst exhausted) the loadCells
// statement was hitting its 15s deadline and the screen 500/503'd with
// nothing to show. The board is a slow-moving fact intra-minute, so a
// successful assembly is cached 60s and served before querying; when a live
// load FAILS, the last good grid (up to 10 min old) is served with an
// explicit stale_as_of stamp instead of an error — respondLoadError only
// fires when there is no cache at all.

const (
	boardGridCacheFreshTTL   = 60 * time.Second
	boardGridCacheStaleTTL   = 10 * time.Minute
	boardGridCacheMaxEntries = 64
)

type boardGridCacheEntry struct {
	grid       BoardGrid
	computedAt time.Time
}

var boardGridCache = struct {
	sync.Mutex
	entries map[string]boardGridCacheEntry
}{entries: map[string]boardGridCacheEntry{}}

// boardGridCacheGet returns the cached grid for key when it is younger than
// maxAge. The returned BoardGrid is a value copy — callers may set StaleAsOf
// without mutating the cache.
func boardGridCacheGet(key string, maxAge time.Duration) (BoardGrid, time.Time, bool) {
	boardGridCache.Lock()
	defer boardGridCache.Unlock()
	e, ok := boardGridCache.entries[key]
	if !ok || time.Since(e.computedAt) >= maxAge {
		return BoardGrid{}, time.Time{}, false
	}
	return e.grid, e.computedAt, true
}

// boardGridCachePut stores a successful assembly, evicting the oldest entry
// when the map exceeds boardGridCacheMaxEntries (a simple bound — org×date
// keys churn daily, this just stops unbounded growth).
func boardGridCachePut(key string, grid BoardGrid) {
	boardGridCache.Lock()
	defer boardGridCache.Unlock()
	boardGridCache.entries[key] = boardGridCacheEntry{grid: grid, computedAt: time.Now()}
	if len(boardGridCache.entries) <= boardGridCacheMaxEntries {
		return
	}
	oldestKey := ""
	var oldestAt time.Time
	for k, e := range boardGridCache.entries {
		if oldestKey == "" || e.computedAt.Before(oldestAt) {
			oldestKey, oldestAt = k, e.computedAt
		}
	}
	delete(boardGridCache.entries, oldestKey)
}

// resetBoardGridCacheForTest drops every cached grid (the cache is package
// level and outlives per-test service instances).
func resetBoardGridCacheForTest() {
	boardGridCache.Lock()
	boardGridCache.entries = map[string]boardGridCacheEntry{}
	boardGridCache.Unlock()
}

// ---------------------------------------------------------------------------
// Handlers

func (s *BoardGridService) HandleGetGrid(w http.ResponseWriter, r *http.Request) {
	date := strings.TrimSpace(r.URL.Query().Get("date"))
	if date == "" {
		// The board day is a DENVER day (§6 throttle defaults) — server-local
		// "today" is a different date for a few hours around midnight.
		// denverToday lives in segmentation_health.go.
		date = denverToday(time.Now()).Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		respondError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
		return
	}
	cacheKey := getOrgID(r) + "|" + date
	// Fresh success cache (60s): served before querying at all.
	if grid, _, ok := boardGridCacheGet(cacheKey, boardGridCacheFreshTTL); ok {
		respondJSON(w, http.StatusOK, grid)
		return
	}
	cells, err := s.loadCells(r, date)
	if err != nil {
		// Live load failed: serve the last GOOD grid (≤10 min) with an
		// explicit staleness stamp; error only with no cache at all.
		if grid, at, ok := boardGridCacheGet(cacheKey, boardGridCacheStaleTTL); ok {
			stale := at.UTC()
			grid.StaleAsOf = &stale
			log.Printf("[board-grid] live load failed (%v) — serving cached grid from %s", err, stale.Format(time.RFC3339))
			respondJSON(w, http.StatusOK, grid)
			return
		}
		respondLoadError(w, err)
		return
	}
	grid := s.assemble(r, date, "", cells)
	boardGridCachePut(cacheKey, grid)
	respondJSON(w, http.StatusOK, grid)
}

// HandleCloneGrid returns `from`'s grid re-dated onto `to`. It does NOT retime:
// the proposal keeps `from`'s slots (the stable structure of a day) and only
// rewrites the date token in each name to `to`. It writes nothing — the
// response is a PROPOSAL the operator edits; staging still happens via
// /stage-board or the Campaign Manager.
func (s *BoardGridService) HandleCloneGrid(w http.ResponseWriter, r *http.Request) {
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	to := strings.TrimSpace(r.URL.Query().Get("to"))
	if from == "" || to == "" {
		respondError(w, http.StatusBadRequest, "from and to are required (YYYY-MM-DD)")
		return
	}
	for _, d := range []string{from, to} {
		if _, err := time.Parse("2006-01-02", d); err != nil {
			respondError(w, http.StatusBadRequest, "dates must be YYYY-MM-DD")
			return
		}
	}
	src, err := s.loadCells(r, from)
	if err != nil {
		respondLoadError(w, err)
		return
	}
	toks, err := time.Parse("2006-01-02", to)
	if err != nil {
		respondError(w, http.StatusBadRequest, "bad to date")
		return
	}
	proposed := make([]BoardCell, 0, len(src))
	for _, c := range src {
		// KEEP the source id as source_campaign_id: /stage loads the source's
		// pmta_config->'campaign_input' blob through it. Discarding it (the
		// pre-2026-08-23 behavior) made a proposal un-stageable.
		c.SourceCampaignID = c.CampaignID
		c.CampaignID = "" // a proposal is not a campaign
		c.Status = ""
		c.Recipients = 0
		c.Proposed = true
		c.Name = rewriteNameDate(c.Name, toks)
		proposed = append(proposed, c)
	}
	respondJSON(w, http.StatusOK, s.assemble(r, to, from, proposed))
}

// HandleRunGates gates a proposed grid the operator has edited client-side.
// The date is parsed exactly like HandleGetGrid's: a malformed date would make
// dateTok empty in runGates and silently disable the NAME_DATE gate.
func (s *BoardGridService) HandleRunGates(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Date  string      `json:"date"`
		Cells []BoardCell `json:"cells"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Date == "" {
		respondError(w, http.StatusBadRequest, "date is required")
		return
	}
	if _, err := time.Parse("2006-01-02", body.Date); err != nil {
		respondError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
		return
	}
	respondJSON(w, http.StatusOK, s.assemble(r, body.Date, "", body.Cells))
}

// respondLoadError maps a loadCells failure onto the wire. A deadline is a 503
// naming the query; anything else is logged in full and answered generically —
// the raw driver string is not an operator-facing message.
func respondLoadError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		respondError(w, http.StatusServiceUnavailable,
			"the board-grid query timed out after 15s — the database is busy, retry shortly")
		return
	}
	log.Printf("[board-grid] load failed: %v", err)
	respondError(w, http.StatusInternalServerError, "board-grid query failed")
}

// ---------------------------------------------------------------------------
// Loading

func (s *BoardGridService) loadCells(r *http.Request, date string) ([]BoardCell, error) {
	orgID := getOrgID(r)
	// The board day is a DENVER day. Computing the bounds here (two timestamptz
	// instants) keeps the WHERE sargable — an AT TIME ZONE on the column would
	// defeat the scheduled_at index; a bare `>= $1::date` compares in the
	// session's TZ and is wrong at the day boundary.
	dayStart, dayEnd, err := denverDayBounds(date)
	if err != nil {
		return nil, fmt.Errorf("board grid day bounds: %w", err)
	}
	// THE DEPLOY BLOB IS THE PROPERTY SOURCE, NOT THE PROFILE JOIN.
	// reserveCampaignForDeploy (handlers_pmta_campaign.go:1586) inserts the
	// row with status='finalizing_audience' and sending_profile_id / offer_id
	// still NULL — both land LATER, when the AudienceFinalizationWorker runs
	// createPMTAWaveCampaign (pmta_campaign_persistence.go:186). Reading the
	// grid inside that window joined nothing, dropped every in-flight
	// campaign into one '(unmapped)' row, and fired MISSING_OFFER +
	// NAME_PROPERTY + UNMAPPED_COLLISION on cells that were already correct
	// (operator 2026-08-23: 11 blockers, every one of which cleared by
	// itself once the finalizer completed). pmta_config->'campaign_input' is
	// written IN THAT SAME RESERVATION TX, so it carries sending_domain from
	// the first instant the row exists — resolve the domain from the profile
	// when there is one and from the blob otherwise.
	//
	// offer_id gets NO such fallback: the column is the only thing the
	// planner and stampCampaignAttribution actually read, so a blob-only
	// offer is exactly the RR-Globe defect Gate 3 exists to catch. The
	// pending_finalize flag below is what keeps Gate 3 quiet while the
	// column is legitimately still on its way.
	const q = `
WITH b AS (
  SELECT c.id, c.name, c.subject, c.status, c.total_recipients, c.scheduled_at,
         COALESCE(c.preview_text,'')            AS preheader,
         COALESCE(c.from_name,'')               AS from_name,
         COALESCE(c.from_email,'')              AS from_email,
         length(COALESCE(c.html_content,''))    AS creative_len,
         COALESCE(NULLIF(sp.sending_domain,''),
                  NULLIF(c.pmta_config->'campaign_input'->>'sending_domain',''), '') AS sending_domain,
         COALESCE(c.offer_id::text,'')                                               AS offer_id,
         (c.status IN ('finalizing_audience','preparing'))                           AS pending_finalize,
         COALESCE(c.pmta_config->>'failure_reason','')                               AS failure_reason,
         (c.status IN ('finalizing_audience','preparing')
          AND c.updated_at < NOW() - INTERVAL '15 minutes')                          AS stuck_finalize
    FROM mailing_campaigns c
    LEFT JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
   WHERE c.scheduled_at >= $1
     AND c.scheduled_at <  $2
     AND ($3 = '' OR c.organization_id::text = $3)
     AND ` + boardCampaignPredicate + `
)
SELECT COALESCE(bm.brand_code, '')                                   AS brand_code,
       COALESCE(bm.brand_label, '')                                  AS brand_label,
       b.sending_domain                                              AS sending_domain,
       COALESCE(bm.brand_root, '')                                   AS brand_root,
       to_char(b.scheduled_at AT TIME ZONE 'America/Denver','HH24:MI') AS slot,
       b.id::text                                                    AS campaign_id,
       COALESCE(b.name,'')                                           AS name,
       b.offer_id                                                    AS offer_id,
       COALESCE(o.name,'')                                           AS offer_name,
       COALESCE(b.subject,'')                                        AS subject,
       COALESCE(b.status,'')                                         AS status,
       COALESCE(b.total_recipients,0)                                AS recipients,
       b.pending_finalize                                            AS pending_finalize,
       b.failure_reason                                              AS failure_reason,
       b.stuck_finalize                                              AS stuck_finalize,
       b.preheader                                                   AS preheader,
       b.from_name                                                   AS from_name,
       b.from_email                                                  AS from_email,
       b.creative_len                                                AS creative_len
  FROM b
  LEFT JOIN mailing_offers o ON o.id::text = b.offer_id
  -- Normalize BOTH sides. The old one-sided form forced the campaign's domain
  -- to 'em.' and compared it against the metadata column verbatim, so a brand
  -- whose canonical sending domain really is 'm.<apex>' (WCL: m.wcl-heloc.com)
  -- could never join its own metadata row and fell through to the apex
  -- fallback below.
  LEFT JOIN mailing_brand_metadata bm
         ON b.sending_domain <> ''
        AND regexp_replace(lower(bm.sending_domain), '^(e?m)\.', 'em.')
          = regexp_replace(lower(b.sending_domain),  '^(e?m)\.', 'em.')
 ORDER BY brand_code, slot`

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, q, dayStart, dayEnd, orgID)
	if err != nil {
		return nil, gridQueryErr(ctx, err)
	}
	defer rows.Close()

	var out []BoardCell
	for rows.Next() {
		var c BoardCell
		if err := rows.Scan(&c.Property, &c.PropertyLabel, &c.SendingDomain, &c.BrandRoot, &c.Slot,
			&c.CampaignID, &c.Name, &c.OfferID, &c.OfferName, &c.Subject,
			&c.Status, &c.Recipients, &c.PendingFinalize, &c.FailureReason, &c.StuckFinalize,
			&c.Preheader, &c.FromName, &c.FromEmail, &c.CreativeLen); err != nil {
			return nil, fmt.Errorf("board grid scan: %w", err)
		}
		c.SubjectRendered = renderForOperator(c.Subject)
		c.PreheaderRendered = renderForOperator(c.Preheader)
		if c.Property == "" {
			// No brand-metadata row for this sending domain. Fall back to the
			// apex so the cell is still visible and still gated — dropping it
			// would hide a real campaign from the board.
			c.Property = apexFromSendingDomain(c.SendingDomain)
			c.PropertyLabel = c.Property
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, gridQueryErr(ctx, err)
	}
	return out, nil
}

// gridQueryErr keeps context.DeadlineExceeded reachable through errors.Is even
// when the driver reports the cancellation with its own error string.
func gridQueryErr(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("board grid query: %w", context.DeadlineExceeded)
	}
	return fmt.Errorf("board grid query: %w", err)
}

// ---------------------------------------------------------------------------
// Assembly + gates

func (s *BoardGridService) assemble(r *http.Request, date, sourceDate string, cells []BoardCell) BoardGrid {
	slotSet := map[string]bool{}
	propSet := map[string]bool{}
	for _, c := range cells {
		slotSet[c.Slot] = true
		propSet[c.Property] = true
	}
	slots := keysSorted(slotSet)
	props := keysSorted(propSet)

	findings := s.runGates(r, date, cells)
	summary := map[string]int{"cells": len(cells), "blocker": 0, "warn": 0}
	for _, f := range findings {
		summary[f.Level]++
	}
	summary["clean"] = len(cells) - distinctCellsWithFindings(findings, cells)

	return BoardGrid{
		Date:       date,
		SourceDate: sourceDate,
		Slots:      slots,
		Properties: props,
		Cells:      cells,
		Findings:   findings,
		Summary:    summary,
	}
}

var (
	reDateToken = regexp.MustCompile(`\b\d{8}\b`)
	// KUMO-WARM and FRESH-BCAST names date themselves 'aug22' / 'jul28', not
	// '08222026'. Both schemes are live; both must be rewritten and gated.
	reMonthDateToken = regexp.MustCompile(`(?i)\b(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)\d{1,2}\b`)
	reLiquid         = regexp.MustCompile(`\{\{|\{%`)
)

// rewriteNameDate rewrites a campaign name's date token to `to`, whichever
// scheme the name uses: the 8-digit '08222026' form or the month-name 'aug22'
// form (rewritten in the same style, preserving the token's letter case).
func rewriteNameDate(name string, to time.Time) string {
	name = reDateToken.ReplaceAllString(name, to.Format("01022006"))
	return reMonthDateToken.ReplaceAllStringFunc(name, func(tok string) string {
		repl := strings.ToLower(to.Format("Jan")) + strconv.Itoa(to.Day())
		switch {
		case tok == strings.ToUpper(tok):
			return strings.ToUpper(repl)
		case tok[0] >= 'A' && tok[0] <= 'Z':
			return strings.ToUpper(repl[:1]) + repl[1:]
		default:
			return repl
		}
	})
}

// monthTokenRegexForDate matches the month-name token for one specific date,
// zero-padded or not ('aug8' / 'aug08'), any case.
// advertiserToken is the ADVERTISER_REPEAT proxy: the first word of the offer
// name, lowercased and stripped of non-alphanumerics. "Optima Tax Relief v2"
// and "Optima - Fresh Start" both yield "optima". Names shorter than 3 chars
// yield "" (too ambiguous to warn on).
func advertiserToken(offerName string) string {
	first := strings.Fields(strings.TrimSpace(offerName))
	if len(first) == 0 {
		return ""
	}
	var b strings.Builder
	for _, r := range strings.ToLower(first[0]) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	tok := b.String()
	if len(tok) < 3 {
		return ""
	}
	return tok
}

// isOfferExemptName moved to offer_gate.go — it is now shared between this
// audit gate (Gate 3 MISSING_OFFER) and the deploy-time offer gate.

func monthTokenRegexForDate(t time.Time) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf(`(?i)\b%s0?%d\b`,
		strings.ToLower(t.Format("Jan")), t.Day()))
}

func (s *BoardGridService) runGates(r *http.Request, date string, cells []BoardCell) []BoardFinding {
	var f []BoardFinding

	// Gate 1 SLOT_COLLISION — two campaigns on one property at one anchor.
	// This is the WF/BWP double-booking, and it is only ever a mistake.
	seen := map[string][]BoardCell{}
	for _, c := range cells {
		k := c.Property + "|" + c.Slot
		seen[k] = append(seen[k], c)
	}
	for k, group := range seen {
		if len(group) > 1 {
			parts := strings.SplitN(k, "|", 2)
			names := make([]string, 0, len(group))
			for _, g := range group {
				names = append(names, g.Name)
			}
			sort.Strings(names)
			// When every colliding cell is '(unmapped)' — no brand-metadata
			// row resolved a property — two unknowns at one slot is a metadata
			// gap, not a proven double-book on one property. Warn, don't block.
			if parts[0] == "(unmapped)" {
				f = append(f, BoardFinding{"warn", "UNMAPPED_COLLISION", parts[0], parts[1],
					fmt.Sprintf("%d unmapped campaigns share this anchor (no brand metadata — cannot prove a double-book): %s",
						len(group), strings.Join(names, " | "))})
				continue
			}
			f = append(f, BoardFinding{"blocker", "SLOT_COLLISION", parts[0], parts[1],
				fmt.Sprintf("%d campaigns share this anchor: %s", len(group), strings.Join(names, " | "))})
		}
	}

	// Gate 2 REPEAT_OFFER — the same offer twice on one property in one day.
	// Operator rule when the third slot was added: "no repetitive campaigns for
	// that sending domain."
	byProp := map[string]map[string][]BoardCell{}
	for _, c := range cells {
		if c.OfferID == "" {
			continue
		}
		if byProp[c.Property] == nil {
			byProp[c.Property] = map[string][]BoardCell{}
		}
		byProp[c.Property][c.OfferID] = append(byProp[c.Property][c.OfferID], c)
	}
	for prop, offers := range byProp {
		for _, group := range offers {
			if len(group) > 1 {
				slots := make([]string, 0, len(group))
				for _, g := range group {
					slots = append(slots, g.Slot)
				}
				sort.Strings(slots)
				f = append(f, BoardFinding{"blocker", "REPEAT_OFFER", prop, slots[0],
					fmt.Sprintf("%q runs %d times on this property (%s)",
						group[0].OfferName, len(group), strings.Join(slots, ", "))})
			}
		}
	}

	// Gate 2b ADVERTISER_REPEAT (warn) — REPEAT_OFFER keys on offer_id, so two
	// DIFFERENT offer rows for the same advertiser slip through (2026-08-22:
	// FC and QF each carried two Optima campaigns under distinct offer ids).
	// Advertiser identity is not modelled anywhere, so the proxy is the
	// normalized first word of the offer name; a proxy match is a WARN for a
	// human to judge, never a blocker.
	byAdv := map[string]map[string][]BoardCell{}
	for _, c := range cells {
		adv := advertiserToken(c.OfferName)
		if adv == "" {
			continue
		}
		if byAdv[c.Property] == nil {
			byAdv[c.Property] = map[string][]BoardCell{}
		}
		byAdv[c.Property][adv] = append(byAdv[c.Property][adv], c)
	}
	for prop, advs := range byAdv {
		for adv, group := range advs {
			distinct := map[string]bool{}
			for _, g := range group {
				distinct[g.OfferID] = true
			}
			if len(group) > 1 && len(distinct) > 1 {
				slots := make([]string, 0, len(group))
				for _, g := range group {
					slots = append(slots, g.Slot)
				}
				sort.Strings(slots)
				f = append(f, BoardFinding{"warn", "ADVERTISER_REPEAT", prop, slots[0],
					fmt.Sprintf("advertiser %q appears under %d distinct offer rows on this property (%s) — same-advertiser repeat slips the offer_id gate",
						adv, len(distinct), strings.Join(slots, ", "))})
			}
		}
	}

	// Per-cell gates.
	dateTok, monthTok := "", ""
	var reMonthTok *regexp.Regexp
	if t, err := time.Parse("2006-01-02", date); err == nil {
		dateTok = t.Format("01022006")
		monthTok = strings.ToLower(t.Format("Jan")) + strconv.Itoa(t.Day())
		reMonthTok = monthTokenRegexForDate(t)
	}
	for _, c := range cells {
		// Gate 7 FAILED_CAMPAIGN — a 'failed' row will NEVER send and sits on
		// the board looking like inventory. The recorded reason (persisted by
		// markCampaignFailed into pmta_config->>'failure_reason') is the whole
		// point: "failed" without a why is what cost the 08/24 board its
		// morning. A failed row is reported ONCE — the remaining per-cell
		// gates are skipped for it (a failed row with a NULL offer firing
		// MISSING_OFFER on top is noise, not signal).
		if c.Status == "failed" {
			reason := c.FailureReason
			if reason == "" {
				reason = "reason not recorded (row failed before failure-reason persistence shipped) — check server logs for [Deploy/BG]"
			}
			f = append(f, BoardFinding{"blocker", "FAILED_CAMPAIGN", c.Property, c.Slot,
				fmt.Sprintf("campaign FAILED and will never send: %s — re-stage this slot; the failed row is inert", reason)})
			continue
		}
		// Gate 8 SILENT_ZERO — scheduled with 0 recipients is the classic
		// silent-failure signature (§6): the row LOOKS staged, dispatches
		// nothing, and nobody finds out until the send window has passed.
		// Proposal cells legitimately carry 0 (audience is planned at deploy).
		if c.Status == "scheduled" && c.Recipients == 0 && !c.Proposed {
			f = append(f, BoardFinding{"blocker", "SILENT_ZERO", c.Property, c.Slot,
				"scheduled with 0 recipients — the silent-failure signature: this row will dispatch nothing. Cancel it from its cell shelf and re-stage the slot"})
		}
		// Gate 9 STUCK_FINALIZE — finalizing/preparing beyond 15 min. The
		// OFFER_PENDING "re-check shortly" story stops applying; say so
		// instead of letting the operator keep waiting on a stalled worker.
		if c.StuckFinalize {
			f = append(f, BoardFinding{"warn", "STUCK_FINALIZE", c.Property, c.Slot,
				fmt.Sprintf("still %s after 15+ minutes — the audience finalizer may be stalled; if this persists, cancel the row from its shelf and re-stage", c.Status)})
		}
		// Gate 3 MISSING_OFFER — RR-Globe / RB-ADR finalized with a NULL offer_id,
		// which silently breaks conversion attribution and offer suppression.
		// Exemption: KUMO-WARM / newsletter cells carry NO offer BY DOCTRINE —
		// warm-up content is editorial and offers are banned in it (CLAUDE.md
		// §13.1), so a NULL offer there is correct, not a defect.
		if c.OfferID == "" && !isOfferExemptName(c.Name) {
			if c.PendingFinalize {
				// The audience finalizer writes offer_id at the very end of
				// its run; blocking here is what produced 6 phantom blockers
				// on 2026-08-23, all of which cleared unaided.
				f = append(f, BoardFinding{"warn", "OFFER_PENDING", c.Property, c.Slot,
					fmt.Sprintf("offer_id not written yet — campaign is still %s; the audience finalizer writes offer_id last. Re-check once it reaches 'scheduled'", c.Status)})
			} else {
				f = append(f, BoardFinding{"blocker", "MISSING_OFFER", c.Property, c.Slot,
					"no offer_id — conversions will not attribute and offer suppression cannot apply"})
			}
		}
		// Gate 4 LIQUID_SUBJECT — RR-HELOC shipped '{{custom.equity_estimate}}'
		// in a subject line to an audience that had no such field. Gated on
		// what the subject RENDERS to, not on whether it contains braces —
		// see subjectRenderProblem.
		if msg := subjectRenderProblem(c.Subject); msg != "" {
			f = append(f, BoardFinding{"blocker", "LIQUID_SUBJECT", c.Property, c.Slot, msg})
		}
		// Gate 5 NAME_DATE — '08062026' and '080202026' both shipped. Either
		// live scheme satisfies the gate: the 8-digit token or the month-name
		// token ('aug22') that KUMO-WARM and FRESH-BCAST names use. A wrong or
		// absent date in both schemes still flags.
		if dateTok != "" && !strings.Contains(c.Name, dateTok) && !reMonthTok.MatchString(c.Name) {
			f = append(f, BoardFinding{"warn", "NAME_DATE", c.Property, c.Slot,
				fmt.Sprintf("name does not carry this board's date token (%s or %s): %q",
					dateTok, monthTok, c.Name)})
		}
		// Gate 6 NAME_PROPERTY — a 'DB'-named campaign that sent from consumerpro.
		if c.Property != "" && !nameMentionsPropertyRoot(c.Name, c.Property, c.BrandRoot) {
			f = append(f, BoardFinding{"warn", "NAME_PROPERTY", c.Property, c.Slot,
				fmt.Sprintf("name does not name property %s (domain is %s): %q",
					c.Property, c.SendingDomain, c.Name)})
		}
	}

	sort.SliceStable(f, func(i, j int) bool {
		if f[i].Level != f[j].Level {
			return f[i].Level == "blocker"
		}
		if f[i].Property != f[j].Property {
			return f[i].Property < f[j].Property
		}
		return f[i].Slot < f[j].Slot
	})
	return f
}

// ---------------------------------------------------------------------------
// helpers

// denverDayBounds returns the [start, end) UTC instants of one Denver calendar
// day, so the query keeps a sargable range on scheduled_at with no AT TIME
// ZONE in the WHERE.
func denverDayBounds(date string) (time.Time, time.Time, error) {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("load America/Denver: %w", err)
	}
	start, err := time.ParseInLocation("2006-01-02", date, loc)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("parse date %q: %w", date, err)
	}
	// AddDate, not Add(24h): DST transition days are 23h or 25h long.
	return start, start.AddDate(0, 0, 1), nil
}

// apexFromSendingDomain turns m.foo.com / em.foo.com into FOO for display.
func apexFromSendingDomain(sd string) string {
	sd = strings.TrimSpace(strings.ToLower(sd))
	if sd == "" {
		return "(unmapped)"
	}
	sd = strings.TrimPrefix(strings.TrimPrefix(sd, "em."), "m.")
	if i := strings.IndexByte(sd, '.'); i > 0 {
		sd = sd[:i]
	}
	return strings.ToUpper(sd)
}

// nameMentionsProperty decides whether a campaign name names its property.
//
// THE TWO CODE SYSTEMS. There are two live brand-code schemes and a correct
// board uses BOTH: mailing_brand_metadata.brand_code is BW / HW / LP / TT / YI,
// while the board's own names (and the drip orchestrator's roster) use
// BWP / HWS / LPL / TOT / YIH. Matching only brand_code raises a false warning
// on 15 of the 50 cells of a CORRECT board — verified against 2026-08-22 — and
// a gate that cries wolf 15 times is worse than no gate at all.
//
// So the rule is: a name token satisfies the property when it is the brand code
// OR an acronym of the brand root, i.e. its letters appear IN ORDER inside the
// root. HWS fits homewarrantyservices, LPL fits learnpersonalloans, TOT fits
// thingoftheday, YIH fits yourinsurancehub — without hardcoding an alias map,
// which is exactly the shortcut that was wrong for 5 of 8 brands last time it
// was guessed.
//
// Deliberately lenient: for a WARNING gate a false accept costs nothing, while
// a false reject trains the operator to ignore the column. It still rejects the
// defects that actually shipped — 'MF' does not fit myownhealth (no 'f'), and a
// 'DB'-named campaign does not fit consumerpro (no 'd').
//
// Substring matching is NOT enough on its own: 'RR' is inside 'CURRENT' and
// 'MR' is inside 'MRD', so tokens are compared whole.
func nameMentionsProperty(name, code string) bool {
	return nameMentionsPropertyRoot(name, code, "")
}

func nameMentionsPropertyRoot(name, code, brandRoot string) bool {
	if code == "" {
		return true
	}
	up := strings.ToUpper(name)
	code = strings.ToUpper(code)
	root := lettersOnly(strings.ToUpper(brandRoot))
	for _, tok := range strings.FieldsFunc(up, func(r rune) bool {
		return r < 'A' || r > 'Z'
	}) {
		if tok == code {
			return true
		}
		// Accept the other code system: an acronym of the brand root.
		if root != "" && len(tok) >= 2 && isSubsequence(tok, root) {
			return true
		}
		// No brand-metadata row (root unmapped — e.g. the kumo warmup
		// domains): fall back to the property code itself as the acronym
		// target, so 'aad' satisfies AADWD instead of warning on every kumo
		// cell every day (found live 2026-08-22).
		if root == "" && len(tok) >= 3 && isSubsequence(tok, code) {
			return true
		}
	}
	return false
}

func lettersOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isSubsequence reports whether every rune of need appears in hay in order.
func isSubsequence(need, hay string) bool {
	i := 0
	for _, h := range hay {
		if i < len(need) && rune(need[i]) == h {
			i++
		}
	}
	return i == len(need)
}

func keysSorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func distinctCellsWithFindings(f []BoardFinding, cells []BoardCell) int {
	hit := map[string]bool{}
	for _, x := range f {
		hit[x.Property+"|"+x.Slot] = true
	}
	n := 0
	for _, c := range cells {
		if hit[c.Property+"|"+c.Slot] {
			n++
		}
	}
	return n
}
