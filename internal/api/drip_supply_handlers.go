package api

// drip_supply_handlers.go — REQ-118 WP9: the Supply API.
//
// This is the read/write surface the portal's Supply tab (WP10) renders and the
// ONLY way a contract changes from the portal. It is registered under the
// /mailing group as `/supply`, so the final URLs are /api/mailing/supply/*
// (docs/DRIP_SUPPLY_CHAIN_DESIGN.md §3).
//
// ── Five rules this file is built around ────────────────────────────────────
//
//  1. ORG SCOPING. The drip supply chain's tables are NOT org-scoped: WP1
//     verified against prod (2026-09-03) that neither partner_clean_queue nor
//     partner_datasets carries an organization_id, so §1 omitted it from every
//     drip_* table too — lane and sending domain ARE the keys. Handlers here
//     therefore scope by SUBJECT (lane / sending domain / source slug) and
//     resolve the org on every request for auth consistency. The org is a real
//     filter on exactly one query in this file: `sent today`, which reads the
//     org-scoped mailing_campaigns. Everything else is subject-scoped, and a
//     caller who passes a different X-Organization-ID sees the same drip rows —
//     which is correct, because there is one estate.
//
//  2. UNKNOWN IS null, NEVER 0 (§6, "unknown rendered as such"). Every number
//     that can be absent is a pointer. A day with no balance rows is not a day
//     with zero capacity, and a lane with no economics row is not a lane worth
//     $0.00 — both are `null`, and the UI must render them as unknown.
//
//  3. EVERY RESPONSE CARRIES `as_of` AND `labels` (§3). `labels` names each
//     number's kind from the fixed vocabulary
//     contracted | effective | planned | reserved | actual | forecast,
//     so a screen can never present a plan as a measurement. `contract_versions`
//     is present wherever the answer depends on a contract.
//
//  4. READ-ONLY, PARAMETERISED, TIME-BOUNDED. Every read runs inside a
//     READ ONLY transaction carrying `SET LOCAL statement_timeout = '20s'`
//     (the sibling idiom — cpm_planner_handlers.go:670, eo_clean_handlers.go:196).
//     SET LOCAL rather than a bare SET on a pooled connection: session state
//     survives a *sql.Conn returning to the pool, a transaction's does not.
//     partner_clean_queue is never scanned without a predicate an index covers
//     (the fresh/remail/pending-EO/follow-up shapes are the planner's measured
//     ones, planner.go readFresh/readRemail/readPendingEO/readFollowups; the
//     stranded-claim count uses the Reap predicate verbatim so it rides
//     idx_pcq_reap_orphans).
//
//  5. FAIL CLOSED ON THE CONTRACT KEY. §1.5 rule 2: a contract is honoured only
//     when its integrity token verifies, and CONTRACT_TOKEN_KEY has no
//     compiled-in fallback ("unset = the supply chain stops, by design").
//     Endpoints that PROJECT CONTRACT POLICY (/ecosystem, /lanes/{lane}) and the
//     one that ISSUES a token (/contracts/../approve) therefore return 503
//     naming the variable when the key is unset. Endpoints that project the
//     LEDGERS (/health, /domains, /ledger/*, /plan) do not need it and keep
//     working: an operator must still be able to see the estate when the key is
//     missing, which is exactly when they most need to.
//
// Nothing in this file writes to the send path. The only writes are contract
// drafts, the approve→schedule transition, and audited manual revenue rows.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/pkg/contractmeta"
	"github.com/ignite/sparkpost-monitor/internal/worker/dripsupply"
)

// ─────────────────────────────────────────────────────────────────────────────
// constants
// ─────────────────────────────────────────────────────────────────────────────

const (
	// dripSupplyStmtTimeout bounds every statement this file issues. 20s is the
	// brief's budget and sits under prod's 30s pool default, so a slow read
	// fails as a clean 504-shaped error instead of pinning a backend.
	dripSupplyStmtTimeout = "20s"

	// dripSupplyStrandedAge mirrors dripsupply.DefaultReapAge (48h): a claim
	// older than this with no campaign is the orphan shape the reaper releases.
	dripSupplyStrandedAge = 48 * time.Hour

	// Ledger page sizes. The ledgers are append-only and grow all day; an
	// unbounded projection is how a screen becomes an outage.
	dripSupplyDefaultLimit = 500
	dripSupplyMaxLimit     = 5000

	// dripSupplyTickWindow is the lookback for the health colour's "two
	// consecutive tick outcomes" rule (§6) and the lane pane's outcome list.
	dripSupplyTickWindow = 24 * time.Hour

	// dripSupplyKeyMissingMsg is the single fail-closed message. It names the
	// variable because the operator's fix is one move.
	dripSupplyKeyMissingMsg = "contract integrity key unavailable: " + contractmeta.KeyEnvVar +
		" is unset or shorter than 32 bytes. Contracts cannot be verified or issued, so this endpoint " +
		"fails closed (REQ-118 §1.5). Ledger endpoints (/supply/health, /supply/domains, /supply/ledger/*, /supply/plan) still work."
)

// Label vocabulary (§3). A number is exactly one of these.
const (
	dripLabelContracted = "contracted"
	dripLabelEffective  = "effective"
	dripLabelPlanned    = "planned"
	dripLabelReserved   = "reserved"
	dripLabelActual     = "actual"
	dripLabelForecast   = "forecast"
)

// dripLabelVocab is the closed set. A test asserts every labels map this file
// builds draws only from it — a label the UI does not understand is worse than
// no label, because it reads as authoritative.
var dripLabelVocab = map[string]bool{
	dripLabelContracted: true,
	dripLabelEffective:  true,
	dripLabelPlanned:    true,
	dripLabelReserved:   true,
	dripLabelActual:     true,
	dripLabelForecast:   true,
}

// Health colours (§6).
const (
	dripHealthGreen  = "green"
	dripHealthAmber  = "amber"
	dripHealthRed    = "red"
	dripHealthGrey   = "grey"
	dripFillAmberPct = 0.80
)

// ─────────────────────────────────────────────────────────────────────────────
// service
// ─────────────────────────────────────────────────────────────────────────────

// DripSupplyService serves the REQ-118 supply chain's projection and contract
// surface. It owns no state beyond the pool.
type DripSupplyService struct{ db *sql.DB }

// NewDripSupplyService builds the service.
func NewDripSupplyService(db *sql.DB) *DripSupplyService { return &DripSupplyService{db: db} }

// RegisterRoutes mounts the service under the /api/mailing group, so the final
// URLs are /api/mailing/supply/*. Route table (§3):
//
//	GET  /supply/health
//	GET  /supply/ecosystem
//	GET  /supply/lanes/{lane}
//	GET  /supply/domains
//	GET  /supply/domains/{domain}
//	GET  /supply/ledger/capacity
//	GET  /supply/ledger/supply
//	GET  /supply/plan
//	GET  /supply/contracts/{kind}/{subject}
//	POST /supply/contracts/{kind}/{subject}
//	POST /supply/contracts/{kind}/{subject}/{version}/approve
//	POST /supply/manual-revenue
func (s *DripSupplyService) RegisterRoutes(r chi.Router) {
	r.Route("/supply", func(sr chi.Router) {
		sr.Get("/health", s.HandleHealth)
		sr.Get("/ecosystem", s.HandleEcosystem)
		sr.Get("/lanes/{lane}", s.HandleLane)
		sr.Get("/domains", s.HandleDomains)
		sr.Get("/domains/{domain}", s.HandleDomain)
		sr.Route("/ledger", func(lr chi.Router) {
			lr.Get("/capacity", s.HandleCapacityLedger)
			lr.Get("/supply", s.HandleSupplyLedger)
		})
		sr.Get("/plan", s.HandlePlan)
		sr.Route("/contracts", func(cr chi.Router) {
			cr.Get("/{kind}/{subject}", s.HandleContractVersions)
			cr.Post("/{kind}/{subject}", s.HandleContractDraft)
			cr.Post("/{kind}/{subject}/{version}/approve", s.HandleContractApprove)
		})
		sr.Post("/manual-revenue", s.HandleManualRevenue)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// envelope + small helpers
// ─────────────────────────────────────────────────────────────────────────────

// dripSupplyMeta is the header every response carries (§3).
type dripSupplyMeta struct {
	AsOf             time.Time         `json:"as_of"`
	Day              string            `json:"day"`
	Labels           map[string]string `json:"labels"`
	ContractVersions map[string]int    `json:"contract_versions,omitempty"`
	// Degraded names anything the response could NOT compute, so a null is
	// never mistaken for "measured and empty".
	Degraded []string `json:"degraded,omitempty"`
}

func dripMeta(day time.Time, labels map[string]string) dripSupplyMeta {
	return dripSupplyMeta{
		AsOf:   time.Now().UTC(),
		Day:    day.Format("2006-01-02"),
		Labels: labels,
	}
}

// dripReadBody reads a request body under a hard cap. Contract bodies are a few
// kilobytes; a cap keeps a malformed or hostile POST from becoming a memory
// event, and reading the bytes once lets the envelope and the contract body be
// decoded from the same JSON.
func dripReadBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, errors.New("empty body")
	}
	return io.ReadAll(io.LimitReader(r.Body, dripSupplyMaxBodyBytes))
}

// dripSupplyMaxBodyBytes caps a contract or manual-revenue POST body.
const dripSupplyMaxBodyBytes = 1 << 20

func dsupInt(v int) *int              { return &v }
func dsupFloat(v float64) *float64    { return &v }
func dsupTime(v time.Time) *time.Time { return &v }

func dsupNullInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

func dsupNullFloat(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}

func dsupNullTime(v sql.NullTime) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time.UTC()
	return &t
}

func dsupNullString(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

// dripSupplyDenverLoc resolves America/Denver, falling back to UTC. Every day
// boundary in this file is Denver's, matching the ledgers and the planner
// (dripsupply.DenverDay).
func dripSupplyDenverLoc() *time.Location {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		return time.UTC
	}
	return loc
}

// dripSupplyDay parses ?day=YYYY-MM-DD as a Denver day, defaulting to today in
// Denver. An unparseable value is an error, never a silent fallback to today:
// answering for the wrong day is worse than refusing.
func dripSupplyDay(r *http.Request) (time.Time, error) {
	loc := dripSupplyDenverLoc()
	raw := strings.TrimSpace(r.URL.Query().Get("day"))
	if raw == "" {
		return dripsupply.DenverDay(time.Now()), nil
	}
	d, err := time.ParseInLocation("2006-01-02", raw, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("day must be YYYY-MM-DD, got %q", raw)
	}
	return d, nil
}

// dripSupplyDayBounds returns [start, end) of the Denver day.
func dripSupplyDayBounds(day time.Time) (time.Time, time.Time) {
	loc := day.Location()
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	return start, start.AddDate(0, 0, 1)
}

// dripNextDenverMidnight is the default effective_at for a new contract: a
// contract takes effect at the NEXT Denver midnight (§0), never immediately.
func dripNextDenverMidnight(now time.Time) time.Time {
	loc := dripSupplyDenverLoc()
	d := now.In(loc)
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
}

// dripSupplyOrg resolves and validates the organization. The drip tables are
// not org-scoped (see the file header), but every request still resolves an org
// so this surface behaves like every other authenticated handler and so the one
// org-scoped read (mailing_campaigns) is correctly bounded.
func (s *DripSupplyService) org(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil || orgID == uuid.Nil {
		respondError(w, http.StatusUnauthorized, "organization context required")
		return uuid.Nil, false
	}
	return orgID, true
}

// readTx opens the READ ONLY, time-bounded transaction every read uses.
func (s *DripSupplyService) readTx(ctx context.Context) (*sql.Tx, error) {
	if s.db == nil {
		return nil, errors.New("drip supply: no database")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '`+dripSupplyStmtTimeout+`'`); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// writeTx opens the time-bounded transaction the three write endpoints use.
func (s *DripSupplyService) writeTx(ctx context.Context) (*sql.Tx, error) {
	if s.db == nil {
		return nil, errors.New("drip supply: no database")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '`+dripSupplyStmtTimeout+`'`); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// dripContractKey returns the HMAC key or reports the fail-closed 503. It is
// the FIRST thing an endpoint that needs it does, so no work and no write
// happens before the refusal.
func dripContractKey(w http.ResponseWriter) ([]byte, bool) {
	key, err := contractmeta.KeyFromEnv()
	if err != nil {
		respondError(w, http.StatusServiceUnavailable, dripSupplyKeyMissingMsg)
		return nil, false
	}
	return key, true
}

// dripSubject normalises a lane / sending domain / source slug from the URL.
// Lowercased and trimmed, because every writer of these tables lowercases.
func dripSubject(raw string) string { return strings.ToLower(strings.TrimSpace(raw)) }

// dripLimit parses ?limit= with a default and a hard ceiling.
func dripLimit(r *http.Request) int {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return dripSupplyDefaultLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return dripSupplyDefaultLimit
	}
	if n > dripSupplyMaxLimit {
		return dripSupplyMaxLimit
	}
	return n
}

// dripIsTimeout reports whether an error is Postgres cancelling a statement
// that blew the 20s budget (SQLSTATE 57014). Those degrade to `null` + a
// `degraded` note rather than a 500: a slow orphan scan must not take the whole
// estate strip down.
func dripIsTimeout(err error) bool {
	if err == nil {
		return false
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "57014"
	}
	return errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(strings.ToLower(err.Error()), "statement timeout") ||
		strings.Contains(strings.ToLower(err.Error()), "canceling statement")
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /supply/health — the estate strip
// ─────────────────────────────────────────────────────────────────────────────

type dripEstateStrip struct {
	Contracted      *int     `json:"contracted"`
	Effective       *int     `json:"effective"`
	Reserved        *int     `json:"reserved"`
	Committed       *int     `json:"committed"`
	Desired         *int     `json:"desired"`
	Unfilled        *int     `json:"unfilled"`
	EOSpendTodayUSD *float64 `json:"eo_spend_today_usd"`
	StrandedClaims  *int     `json:"stranded_claims"`
	DomainISPCells  int      `json:"domain_isp_cells"`
	LaneISPCells    int      `json:"lane_isp_cells"`
}

type dripFreshness struct {
	Balances *time.Time `json:"balances"`
	Ledger   *time.Time `json:"ledger"`
	Max      *time.Time `json:"max"`
}

type dripHealthResponse struct {
	dripSupplyMeta
	Estate    dripEstateStrip `json:"estate"`
	Freshness dripFreshness   `json:"freshness"`
}

func dripHealthLabels() map[string]string {
	return map[string]string{
		"estate.contracted":      dripLabelContracted,
		"estate.effective":       dripLabelEffective,
		"estate.reserved":        dripLabelReserved,
		"estate.committed":       dripLabelActual,
		"estate.desired":         dripLabelPlanned,
		"estate.unfilled":        dripLabelPlanned,
		"estate.eo_spend_today":  dripLabelActual,
		"estate.stranded_claims": dripLabelActual,
	}
}

// dripBalanceSumSQL sums the day's domain×ISP balances. COUNT(*) rides along so
// "no rows" (unknown) is distinguishable from "rows summing to zero".
const dripBalanceSumSQL = `
	SELECT COUNT(*)::bigint,
	       COALESCE(SUM(contracted), 0)::bigint,
	       COALESCE(SUM(effective), 0)::bigint,
	       COALESCE(SUM(reserved), 0)::bigint,
	       COALESCE(SUM(committed), 0)::bigint,
	       MAX(last_refill_tick)
	  FROM drip_capacity_balance
	 WHERE day = $1`

const dripLaneBalanceSumSQL = `
	SELECT COUNT(*)::bigint,
	       COALESCE(SUM(desired), 0)::bigint,
	       COALESCE(SUM(unfilled), 0)::bigint
	  FROM drip_lane_balance
	 WHERE day = $1`

const dripEOSpendSQL = `
	SELECT COUNT(*)::bigint, COALESCE(SUM(total_cost), 0)::float8
	  FROM drip_supply_ledger
	 WHERE event = 'VALIDATION_ORDERED'
	   AND occurred_at >= $1 AND occurred_at < $2`

const dripCapacityLedgerFreshSQL = `
	SELECT MAX(updated_at) FROM drip_capacity_ledger WHERE day = $1`

// dripStrandedClaimsSQL is the Reap predicate VERBATIM
// (dripsupply.reapOrphanClaimsSQL): status='claimed' AND
// last_touch_campaign_id IS NULL AND mailed_campaign_id IS NULL, ranged on
// claimed_at. That triple is exactly idx_pcq_reap_orphans' WHERE clause
// (cmd/server/main.go concurrentIndexSpecs), so this is an index range scan and
// not the parallel seq scan the same count costs without it (EXPLAIN on prod
// 2026-09-03: cost 959,408 unindexed). The cutoff is computed IN POSTGRES, like
// the reaper's, so this number and the reaper's never disagree over clock skew.
const dripStrandedClaimsSQL = `
	SELECT COUNT(*)::bigint
	  FROM partner_clean_queue
	 WHERE status = 'claimed'
	   AND last_touch_campaign_id IS NULL
	   AND mailed_campaign_id IS NULL
	   AND claimed_at < NOW() - make_interval(secs => $1::double precision)`

// HandleHealth GET /api/mailing/supply/health?day=
//
// The estate strip (§6): contracted / effective / reserved / committed across
// every domain×ISP, the lane side's desired + unfilled, today's EO spend from
// the Supply Ledger, the stranded-claim count, and the freshness of both
// ledgers. Needs no contract key — this is the view that must survive the key
// being missing.
func (s *DripSupplyService) HandleHealth(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.org(w, r); !ok {
		return
	}
	day, err := dripSupplyDay(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	tx, err := s.readTx(ctx)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply health: "+err.Error())
		return
	}
	defer func() { _ = tx.Rollback() }()

	out := dripHealthResponse{dripSupplyMeta: dripMeta(day, dripHealthLabels())}
	dayStart, dayEnd := dripSupplyDayBounds(day)

	var cells, contracted, effective, reserved, committed int64
	var refill sql.NullTime
	if err := tx.QueryRowContext(ctx, dripBalanceSumSQL, day).
		Scan(&cells, &contracted, &effective, &reserved, &committed, &refill); err != nil {
		respondError(w, http.StatusInternalServerError, "supply health: capacity balances: "+err.Error())
		return
	}
	out.Estate.DomainISPCells = int(cells)
	if cells > 0 {
		out.Estate.Contracted = dsupInt(int(contracted))
		out.Estate.Effective = dsupInt(int(effective))
		out.Estate.Reserved = dsupInt(int(reserved))
		out.Estate.Committed = dsupInt(int(committed))
	} else {
		out.Degraded = append(out.Degraded, "no drip_capacity_balance rows for this day — capacity is unknown, not zero")
	}
	out.Freshness.Balances = dsupNullTime(refill)

	var laneCells, desired, unfilled int64
	if err := tx.QueryRowContext(ctx, dripLaneBalanceSumSQL, day).Scan(&laneCells, &desired, &unfilled); err != nil {
		respondError(w, http.StatusInternalServerError, "supply health: lane balances: "+err.Error())
		return
	}
	out.Estate.LaneISPCells = int(laneCells)
	if laneCells > 0 {
		out.Estate.Desired = dsupInt(int(desired))
		out.Estate.Unfilled = dsupInt(int(unfilled))
	} else {
		out.Degraded = append(out.Degraded, "no drip_lane_balance rows for this day — demand is unknown, not zero")
	}

	var eoRows int64
	var eoSpend float64
	if err := tx.QueryRowContext(ctx, dripEOSpendSQL, dayStart, dayEnd).Scan(&eoRows, &eoSpend); err != nil {
		respondError(w, http.StatusInternalServerError, "supply health: eo spend: "+err.Error())
		return
	}
	// Zero EO orders IS a measurement here: the Supply Ledger is append-only and
	// an absent row means nothing was ordered, which is a real $0.00.
	out.Estate.EOSpendTodayUSD = dsupFloat(eoSpend)

	var ledgerFresh sql.NullTime
	if err := tx.QueryRowContext(ctx, dripCapacityLedgerFreshSQL, day).Scan(&ledgerFresh); err != nil {
		respondError(w, http.StatusInternalServerError, "supply health: ledger freshness: "+err.Error())
		return
	}
	out.Freshness.Ledger = dsupNullTime(ledgerFresh)
	out.Freshness.Max = dripMaxTime(out.Freshness.Balances, out.Freshness.Ledger)

	var stranded int64
	switch err := tx.QueryRowContext(ctx, dripStrandedClaimsSQL, dripSupplyStrandedAge.Seconds()).Scan(&stranded); {
	case err == nil:
		out.Estate.StrandedClaims = dsupInt(int(stranded))
	case dripIsTimeout(err):
		// Left null on purpose. This is the one query whose index
		// (idx_pcq_reap_orphans) is built CONCURRENTLY at boot; until it exists
		// the count is a seq scan over 13.7M rows. Unknown, never zero.
		out.Degraded = append(out.Degraded,
			"stranded_claims timed out at "+dripSupplyStmtTimeout+" — idx_pcq_reap_orphans may not be built yet")
	default:
		respondError(w, http.StatusInternalServerError, "supply health: stranded claims: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, out)
}

func dripMaxTime(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case b.After(*a):
		return b
	default:
		return a
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /supply/ecosystem — the lane queue
// ─────────────────────────────────────────────────────────────────────────────

type dripDispatchValue struct {
	ContributionECPM *float64 `json:"contribution_ecpm"`
	ObservedECPM     *float64 `json:"observed_ecpm"`
	Maturity         string   `json:"maturity"` // mature | incomplete | unknown
	Messages         *int     `json:"messages"`
	Conversions      *int     `json:"conversions"`
	Inherited        bool     `json:"inherited"` // true = estate median, not this lane's own number
}

type dripDemand struct {
	Desired            *int   `json:"desired"`
	AwardedFirm        *int   `json:"awarded_firm"`
	AwardedProvisional *int   `json:"awarded_provisional"`
	SupplyBacked       *int   `json:"supply_backed"`
	Unserved           *int   `json:"unserved"`
	UnservedReason     string `json:"unserved_reason"`
	FollowupsReserved  *int   `json:"followups_reserved"`
	SupplyReleased     *int   `json:"supply_released"`
}

type dripLaneRow struct {
	Lane        string `json:"lane"`
	Rank        *int   `json:"rank"`
	RankReason  string `json:"rank_reason"`
	Tier        int    `json:"tier"`
	Exploration bool   `json:"exploration"`
	Paused      bool   `json:"paused"`

	DispatchValue dripDispatchValue `json:"dispatch_value"`
	Demand        dripDemand        `json:"demand"`

	FollowupsDue      *int     `json:"followups_due"`
	FreshMailable     *int     `json:"fresh_mailable"`
	PendingEO         *int     `json:"pending_eo"`
	RemailEligible    *int     `json:"remail_eligible"`
	CleanOrderedToday *int     `json:"clean_ordered_today"`
	CleanCostTodayUSD *float64 `json:"clean_cost_today_usd"`
	SentToday         *int     `json:"sent_today"`

	Reserved          *int     `json:"reserved"`
	Committed         *int     `json:"committed"`
	FillRate          *float64 `json:"fill_rate"`
	BindingConstraint string   `json:"binding_constraint"`

	Health       string `json:"health"`
	HealthReason string `json:"health_reason"`

	DispatchVersion  int `json:"dispatch_contract_version"`
	InventoryVersion int `json:"inventory_contract_version,omitempty"`
}

type dripEcosystemResponse struct {
	dripSupplyMeta
	Lanes    []dripLaneRow `json:"lanes"`
	FrozenAt *time.Time    `json:"plan_frozen_at"`
}

func dripEcosystemLabels() map[string]string {
	return map[string]string{
		"rank":                       dripLabelPlanned,
		"tier":                       dripLabelContracted,
		"dispatch_value":             dripLabelActual,
		"demand.desired":             dripLabelContracted,
		"demand.awarded_firm":        dripLabelPlanned,
		"demand.awarded_provisional": dripLabelPlanned,
		"demand.supply_backed":       dripLabelPlanned,
		"demand.unserved":            dripLabelPlanned,
		"demand.followups_reserved":  dripLabelPlanned,
		"demand.supply_released":     dripLabelPlanned,
		"followups_due":              dripLabelActual,
		"fresh_mailable":             dripLabelActual,
		"pending_eo":                 dripLabelForecast,
		"remail_eligible":            dripLabelActual,
		"clean_ordered_today":        dripLabelActual,
		"sent_today":                 dripLabelActual,
		"reserved":                   dripLabelReserved,
		"committed":                  dripLabelActual,
		"fill_rate":                  dripLabelActual,
	}
}

// HandleEcosystem GET /api/mailing/supply/ecosystem?day=
//
// One row per lane with an active dispatch contract (§6 pane 1). Requires the
// contract key: rank, tier and desired are CONTRACT policy, and §1.5 rule 2
// says an unverified contract is not honoured — showing one anyway would put a
// hand-edited number on the operator's screen.
func (s *DripSupplyService) HandleEcosystem(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.org(w, r)
	if !ok {
		return
	}
	day, err := dripSupplyDay(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	key, ok := dripContractKey(w)
	if !ok {
		return
	}
	ctx := r.Context()
	tx, err := s.readTx(ctx)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply ecosystem: "+err.Error())
		return
	}
	defer func() { _ = tx.Rollback() }()

	contracts, err := dripsupply.LoadActiveWithKey(ctx, tx, day, key)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply ecosystem: load contracts: "+err.Error())
		return
	}
	out, err := s.ecosystem(ctx, tx, day, orgID, contracts)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply ecosystem: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, out)
}

// ecosystem is the seam the handler and its tests share: it takes an already
// loaded (and token-verified) contract set so a test can exercise the whole
// projection without minting HMACs.
func (s *DripSupplyService) ecosystem(ctx context.Context, tx dripQueryer, day time.Time, orgID uuid.UUID, contracts *dripsupply.ActiveSet) (*dripEcosystemResponse, error) {
	out := &dripEcosystemResponse{dripSupplyMeta: dripMeta(day, dripEcosystemLabels())}

	lanes := make([]string, 0, len(contracts.Dispatches))
	for l := range contracts.Dispatches {
		lanes = append(lanes, l)
	}
	sort.Strings(lanes)
	if len(lanes) == 0 {
		out.Degraded = append(out.Degraded, "no active dispatch contracts for this day — no lane is governed")
		out.Lanes = []dripLaneRow{}
		return out, nil
	}

	plan, frozenAt, err := dripPlanByLane(ctx, tx, day, lanes)
	if err != nil {
		return nil, err
	}
	out.FrozenAt = frozenAt
	balances, err := dripLaneBalanceByLane(ctx, tx, day, lanes)
	if err != nil {
		return nil, err
	}
	binding, err := dripBindingByLane(ctx, tx, day, lanes)
	if err != nil {
		return nil, err
	}
	ticks, err := dripTickHealthByLane(ctx, tx, lanes, time.Now().Add(-dripSupplyTickWindow))
	if err != nil {
		return nil, err
	}
	fresh, err := dripFreshMailableByLane(ctx, tx, day, contracts, lanes)
	if err != nil {
		return nil, err
	}
	remail, err := dripRemailEligibleByLane(ctx, tx, day, contracts, lanes)
	if err != nil {
		return nil, err
	}
	pendingEO, err := dripPendingEOByLane(ctx, tx, lanes)
	if err != nil {
		return nil, err
	}
	followups, err := dripFollowupsDueByLane(ctx, tx, day, lanes)
	if err != nil {
		return nil, err
	}
	ordered, err := dripCleanOrderedByLane(ctx, tx, day, lanes)
	if err != nil {
		return nil, err
	}
	sent, err := dripSentTodayByLane(ctx, tx, day, orgID, lanes)
	if err != nil {
		return nil, err
	}
	paused, err := dripPausedByLane(ctx, tx, lanes)
	if err != nil {
		return nil, err
	}
	ranks, err := dripsupply.RankInputs(ctx, tx, day)
	if err != nil {
		return nil, fmt.Errorf("rank inputs: %w", err)
	}

	rows := make([]dripLaneRow, 0, len(lanes))
	for _, lane := range lanes {
		dc := contracts.Dispatches[lane]
		row := dripLaneRow{
			Lane:            lane,
			Tier:            dc.OperatorPriorityTier,
			Exploration:     dc.OperatorPriorityTier == dripsupply.ExplorationTier,
			Paused:          paused[lane],
			DispatchVersion: dc.Meta.Version,
		}
		if inv := contracts.Inventories[lane]; inv != nil {
			row.InventoryVersion = inv.Meta.Version
		}

		if p, ok := plan[lane]; ok {
			row.Rank = dsupInt(p.Rank)
			row.RankReason = p.RankReason
			row.Demand.AwardedFirm = dsupInt(p.AwardFirm)
			row.Demand.AwardedProvisional = dsupInt(p.AwardProvisional)
			row.Demand.SupplyBacked = dsupInt(p.AwardFirm + p.AwardProvisional)
			row.Demand.Unserved = dsupInt(p.Unserved)
			row.Demand.UnservedReason = p.UnservedReason
			row.Demand.FollowupsReserved = dsupInt(p.FollowupsReserved)
			row.Demand.SupplyReleased = dsupInt(p.SupplyReleased)
		}
		if b, ok := balances[lane]; ok {
			row.Demand.Desired = dsupInt(b.Desired)
			row.Reserved = dsupInt(b.Reserved)
			row.Committed = dsupInt(b.Committed)
			row.FillRate = dripFillRate(b.Committed, b.Desired)
			if row.Demand.AwardedFirm == nil {
				row.Demand.AwardedFirm = dsupInt(b.AwardedFirm)
				row.Demand.AwardedProvisional = dsupInt(b.AwardedProvisional)
				row.Demand.SupplyBacked = dsupInt(b.AwardedFirm + b.AwardedProvisional)
			}
		} else {
			// The contract states the demand even when the planner has not run;
			// showing it as null would read as "this lane wants nothing".
			if n, ok := dripDesiredTotal(dc); ok {
				row.Demand.Desired = dsupInt(n)
			}
		}

		row.BindingConstraint = binding[lane]
		row.FollowupsDue = dripLookup(followups, lane)
		row.FreshMailable = dripLookup(fresh, lane)
		row.PendingEO = dripLookup(pendingEO, lane)
		row.RemailEligible = dripLookup(remail, lane)
		if o, ok := ordered[lane]; ok {
			row.CleanOrderedToday = dsupInt(o.Quantity)
			row.CleanCostTodayUSD = dsupFloat(o.Cost)
		}
		row.SentToday = dripLookup(sent, lane)
		row.DispatchValue = dripDispatchValueFor(ranks, lane)

		desired := 0
		if row.Demand.Desired != nil {
			desired = *row.Demand.Desired
		}
		row.Health, row.HealthReason = dripHealthColour(row.Paused, desired, row.FillRate, ticks[lane])
		rows = append(rows, row)
	}
	out.Lanes = rows
	return out, nil
}

// dripFillRate is committed ÷ desired. Nil when desired is 0 or unknown — a
// fill rate against no demand is not 100%, it is undefined.
func dripFillRate(committed, desired int) *float64 {
	if desired <= 0 {
		return nil
	}
	return dsupFloat(float64(committed) / float64(desired))
}

// dripDesiredTotal sums a dispatch contract's per-ISP desired intros. Reports
// false for 'consume_available' lanes, whose demand is not a number the
// contract states.
func dripDesiredTotal(dc *dripsupply.DispatchContract) (int, bool) {
	if dc == nil || dc.DemandMode != "target" {
		return 0, false
	}
	total := 0
	for _, v := range dc.DesiredDailyIntros {
		if v > 0 {
			total += v
		}
	}
	return total, true
}

func dripLookup(m map[string]int, lane string) *int {
	if v, ok := m[lane]; ok {
		return dsupInt(v)
	}
	return nil
}

func dripDispatchValueFor(ranks map[string]dripsupply.RankInput, lane string) dripDispatchValue {
	ri, ok := ranks[lane]
	if !ok {
		return dripDispatchValue{Maturity: "unknown"}
	}
	v := dripDispatchValue{
		ContributionECPM: dsupFloat(ri.ContributionECPM),
		ObservedECPM:     dsupFloat(ri.Observed),
		Messages:         dsupInt(ri.Messages),
		Conversions:      dsupInt(ri.Conversions),
		Inherited:        ri.Fallback,
	}
	if ri.SampleOK {
		v.Maturity = "mature"
	} else {
		v.Maturity = "incomplete"
	}
	return v
}

// dripHealthColour implements §6's rule exactly:
//
//	grey  — paused
//	red   — two consecutive zero/failed tick outcomes while desired > 0
//	amber — fill rate below 80%
//	green — otherwise
//
// It is a pure function so the rule is unit-testable without a database, and so
// the UI never re-derives it (two implementations of a colour is two colours).
func dripHealthColour(paused bool, desired int, fill *float64, tick dripTickHealth) (string, string) {
	if paused {
		return dripHealthGrey, "lane paused: no active, non-emergency-paused dataset or no active roster brand"
	}
	if desired > 0 && tick.ConsecutiveBad >= 2 {
		reason := "two consecutive tick outcomes were zero/failed while desired > 0"
		if tick.LastReason != "" {
			reason += ": " + tick.LastReason
		}
		return dripHealthRed, reason
	}
	if fill != nil && *fill < dripFillAmberPct {
		return dripHealthAmber, fmt.Sprintf("fill rate %.0f%% is below %.0f%%", *fill*100, dripFillAmberPct*100)
	}
	if fill == nil && desired > 0 {
		return dripHealthAmber, "no committed capacity recorded for a lane with demand"
	}
	return dripHealthGreen, ""
}

// ─────────────────────────────────────────────────────────────────────────────
// shared readers (used by /ecosystem and /lanes/{lane})
// ─────────────────────────────────────────────────────────────────────────────

// dripQueryer is the read surface every reader below needs. *sql.DB, *sql.Tx
// and *sql.Conn satisfy it; so does a sqlmock-backed DB in the tests.
type dripQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type dripPlanAgg struct {
	Rank              int
	RankReason        string
	AwardFirm         int
	AwardProvisional  int
	FollowupsReserved int
	Unserved          int
	UnservedReason    string
	SupplyReleased    int
}

const dripPlanByLaneSQL = `
	SELECT lane,
	       MIN(rank)::bigint,
	       COALESCE(MIN(rank_reason) FILTER (WHERE rank_reason <> ''), ''),
	       COALESCE(SUM(award_firm), 0)::bigint,
	       COALESCE(SUM(award_provisional), 0)::bigint,
	       COALESCE(SUM(followups_reserved), 0)::bigint,
	       COALESCE(SUM(unserved), 0)::bigint,
	       COALESCE(mode() WITHIN GROUP (ORDER BY unserved_reason) FILTER (WHERE unserved_reason <> ''), ''),
	       COALESCE(SUM(supply_released), 0)::bigint,
	       MAX(frozen_at)
	  FROM drip_daily_plan
	 WHERE day = $1 AND lane = ANY($2)
	 GROUP BY 1`

func dripPlanByLane(ctx context.Context, q dripQueryer, day time.Time, lanes []string) (map[string]dripPlanAgg, *time.Time, error) {
	rows, err := q.QueryContext(ctx, dripPlanByLaneSQL, day, pq.Array(lanes))
	if err != nil {
		return nil, nil, fmt.Errorf("daily plan: %w", err)
	}
	defer rows.Close()
	out := map[string]dripPlanAgg{}
	var frozen *time.Time
	for rows.Next() {
		var lane string
		var rank, firm, prov, fups, unserved, released int64
		var reason, unservedReason string
		var frozenAt sql.NullTime
		if err := rows.Scan(&lane, &rank, &reason, &firm, &prov, &fups, &unserved, &unservedReason, &released, &frozenAt); err != nil {
			return nil, nil, fmt.Errorf("daily plan scan: %w", err)
		}
		out[lane] = dripPlanAgg{
			Rank: int(rank), RankReason: reason,
			AwardFirm: int(firm), AwardProvisional: int(prov),
			FollowupsReserved: int(fups), Unserved: int(unserved),
			UnservedReason: unservedReason, SupplyReleased: int(released),
		}
		if t := dsupNullTime(frozenAt); t != nil {
			frozen = dripMaxTime(frozen, t)
		}
	}
	return out, frozen, rows.Err()
}

type dripLaneBalanceAgg struct {
	Desired            int
	AwardedFirm        int
	AwardedProvisional int
	Reserved           int
	Committed          int
	Unfilled           int
}

const dripLaneBalanceByLaneSQL = `
	SELECT lane,
	       COALESCE(SUM(desired), 0)::bigint,
	       COALESCE(SUM(awarded_firm), 0)::bigint,
	       COALESCE(SUM(awarded_provisional), 0)::bigint,
	       COALESCE(SUM(reserved), 0)::bigint,
	       COALESCE(SUM(committed), 0)::bigint,
	       COALESCE(SUM(unfilled), 0)::bigint
	  FROM drip_lane_balance
	 WHERE day = $1 AND lane = ANY($2)
	 GROUP BY 1`

func dripLaneBalanceByLane(ctx context.Context, q dripQueryer, day time.Time, lanes []string) (map[string]dripLaneBalanceAgg, error) {
	rows, err := q.QueryContext(ctx, dripLaneBalanceByLaneSQL, day, pq.Array(lanes))
	if err != nil {
		return nil, fmt.Errorf("lane balances: %w", err)
	}
	defer rows.Close()
	out := map[string]dripLaneBalanceAgg{}
	for rows.Next() {
		var lane string
		var desired, firm, prov, reserved, committed, unfilled int64
		if err := rows.Scan(&lane, &desired, &firm, &prov, &reserved, &committed, &unfilled); err != nil {
			return nil, fmt.Errorf("lane balances scan: %w", err)
		}
		out[lane] = dripLaneBalanceAgg{
			Desired: int(desired), AwardedFirm: int(firm), AwardedProvisional: int(prov),
			Reserved: int(reserved), Committed: int(committed), Unfilled: int(unfilled),
		}
	}
	return out, rows.Err()
}

// dripBindingByLaneSQL is the modal binding_reason among the day's grants that
// were actually bound below demand. 'requested' means nothing bound, so it is
// excluded — otherwise a healthy lane would report "requested" as its
// constraint. Rides idx_drip_capacity_ledger_day_lane_isp.
const dripBindingByLaneSQL = `
	SELECT lane, mode() WITHIN GROUP (ORDER BY binding_reason)
	  FROM drip_capacity_ledger
	 WHERE day = $1 AND lane = ANY($2) AND binding_reason <> 'requested'
	 GROUP BY 1`

func dripBindingByLane(ctx context.Context, q dripQueryer, day time.Time, lanes []string) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, dripBindingByLaneSQL, day, pq.Array(lanes))
	if err != nil {
		return nil, fmt.Errorf("binding constraint: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var lane string
		var reason sql.NullString
		if err := rows.Scan(&lane, &reason); err != nil {
			return nil, fmt.Errorf("binding constraint scan: %w", err)
		}
		out[lane] = dsupNullString(reason)
	}
	return out, rows.Err()
}

// dripTickHealth is what the health colour needs from drip_tick_outcomes.
type dripTickHealth struct {
	ConsecutiveBad int
	LastReason     string
	LastTick       *time.Time
}

// dripTickHealthSQL rolls the passes of one tick into a single verdict (a lane
// whose intro pass fired and whose follow-up pass failed did NOT have a clean
// tick), then takes the two most recent. Window functions run after GROUP BY,
// so the ROW_NUMBER is over the rolled-up ticks.
const dripTickHealthSQL = `
	SELECT lane, tick, bad, reason
	  FROM (
	    SELECT lane,
	           tick,
	           bool_or(outcome IN ('zero','failed')) AS bad,
	           COALESCE(MIN(reason) FILTER (WHERE outcome IN ('zero','failed')), '') AS reason,
	           ROW_NUMBER() OVER (PARTITION BY lane ORDER BY tick DESC) AS rn
	      FROM drip_tick_outcomes
	     WHERE lane = ANY($1) AND tick >= $2
	     GROUP BY lane, tick
	  ) t
	 WHERE rn <= 2
	 ORDER BY lane, tick DESC`

func dripTickHealthByLane(ctx context.Context, q dripQueryer, lanes []string, since time.Time) (map[string]dripTickHealth, error) {
	rows, err := q.QueryContext(ctx, dripTickHealthSQL, pq.Array(lanes), since)
	if err != nil {
		return nil, fmt.Errorf("tick outcomes: %w", err)
	}
	defer rows.Close()
	out := map[string]dripTickHealth{}
	seen := map[string]int{}
	for rows.Next() {
		var lane, reason string
		var tick time.Time
		var bad bool
		if err := rows.Scan(&lane, &tick, &bad, &reason); err != nil {
			return nil, fmt.Errorf("tick outcomes scan: %w", err)
		}
		h := out[lane]
		idx := seen[lane]
		seen[lane] = idx + 1
		if idx == 0 {
			h.LastTick = dsupTime(tick)
			h.LastReason = reason
		}
		// Rows arrive newest first; count the leading run of bad ticks so a
		// lane that failed twice and then recovered is NOT red.
		if bad && h.ConsecutiveBad == idx {
			h.ConsecutiveBad = idx + 1
		}
		out[lane] = h
	}
	return out, rows.Err()
}

// The four inventory readers below reuse the planner's MEASURED query shapes
// (planner.go readFresh/readRemail/readPendingEO/readFollowups) so the screen
// and the planner can never disagree about what is mailable. Fresh and remail
// group lanes by their inventory contract's window because a per-lane
// correlated cutoff measured 8.9s on prod against 4.1s for this shape
// (planner.go readFresh, verified 2026-09-03).

const dripFreshMailableSQL = `
	SELECT vertical, COUNT(*)::bigint
	  FROM partner_clean_queue
	 WHERE status = 'ready'
	   AND vertical = ANY($1)
	   AND mailed_at IS NULL
	   AND touch_count = 0
	   AND validated_at >= $2
	 GROUP BY 1`

func dripFreshMailableByLane(ctx context.Context, q dripQueryer, day time.Time, c *dripsupply.ActiveSet, lanes []string) (map[string]int, error) {
	byDays := map[int][]string{}
	for _, l := range lanes {
		days := 60
		if inv := c.Inventories[l]; inv != nil && inv.VerdictValidDays > 0 {
			days = inv.VerdictValidDays
		}
		byDays[days] = append(byDays[days], l)
	}
	out := map[string]int{}
	for _, days := range dripSortedInts(byDays) {
		group := byDays[days]
		sort.Strings(group)
		cutoff := day.AddDate(0, 0, -days)
		if err := dripScanLaneCounts(ctx, q, dripFreshMailableSQL, out, pq.Array(group), cutoff); err != nil {
			return nil, fmt.Errorf("fresh mailable (%dd): %w", days, err)
		}
	}
	return out, nil
}

const dripRemailEligibleSQL = `
	SELECT vertical, COUNT(*)::bigint
	  FROM partner_clean_queue
	 WHERE status = 'mailed'
	   AND vertical = ANY($1)
	   AND mailed_at < $2
	   AND engaged_at IS NULL
	   AND terminal_reason IS NULL
	 GROUP BY 1`

func dripRemailEligibleByLane(ctx context.Context, q dripQueryer, day time.Time, c *dripsupply.ActiveSet, lanes []string) (map[string]int, error) {
	byDays := map[int][]string{}
	for _, l := range lanes {
		inv := c.Inventories[l]
		// A lane whose inventory contract disables remail is not queried at
		// all: remail credit must never appear by accident (planner.go
		// readRemail).
		if inv == nil || !inv.RemailEnabled {
			continue
		}
		d := inv.RemailAfterDays
		if d < 0 {
			d = 0
		}
		byDays[d] = append(byDays[d], l)
	}
	out := map[string]int{}
	for _, days := range dripSortedInts(byDays) {
		group := byDays[days]
		sort.Strings(group)
		cutoff := day.AddDate(0, 0, -days)
		if err := dripScanLaneCounts(ctx, q, dripRemailEligibleSQL, out, pq.Array(group), cutoff); err != nil {
			return nil, fmt.Errorf("remail eligible (%dd): %w", days, err)
		}
	}
	return out, nil
}

const dripPendingEOSQL = `
	SELECT vertical, COUNT(*)::bigint
	  FROM partner_clean_queue
	 WHERE status IN ('pending_eo', 'eo_in_flight')
	   AND vertical = ANY($1)
	 GROUP BY 1`

func dripPendingEOByLane(ctx context.Context, q dripQueryer, lanes []string) (map[string]int, error) {
	out := map[string]int{}
	if err := dripScanLaneCounts(ctx, q, dripPendingEOSQL, out, pq.Array(lanes)); err != nil {
		return nil, fmt.Errorf("pending eo: %w", err)
	}
	return out, nil
}

// dripFollowupsDueSQL deliberately includes OVERDUE touches (`next_touch_at <
// day_end`, not a range): a follow-up whose deadline passed yesterday is more
// due, not less (planner.go readFollowups).
const dripFollowupsDueSQL = `
	SELECT vertical, COUNT(*)::bigint
	  FROM partner_clean_queue
	 WHERE status = 'mailed'
	   AND vertical = ANY($1)
	   AND next_touch_at < $2
	   AND engaged_at IS NULL
	   AND terminal_reason IS NULL
	 GROUP BY 1`

func dripFollowupsDueByLane(ctx context.Context, q dripQueryer, day time.Time, lanes []string) (map[string]int, error) {
	_, dayEnd := dripSupplyDayBounds(day)
	out := map[string]int{}
	if err := dripScanLaneCounts(ctx, q, dripFollowupsDueSQL, out, pq.Array(lanes), dayEnd); err != nil {
		return nil, fmt.Errorf("follow-ups due: %w", err)
	}
	return out, nil
}

type dripOrderAgg struct {
	Quantity int
	Cost     float64
}

const dripCleanOrderedSQL = `
	SELECT lane, COALESCE(SUM(quantity), 0)::bigint, COALESCE(SUM(total_cost), 0)::float8
	  FROM drip_supply_ledger
	 WHERE event = 'VALIDATION_ORDERED'
	   AND lane = ANY($1)
	   AND occurred_at >= $2 AND occurred_at < $3
	 GROUP BY 1`

func dripCleanOrderedByLane(ctx context.Context, q dripQueryer, day time.Time, lanes []string) (map[string]dripOrderAgg, error) {
	start, end := dripSupplyDayBounds(day)
	rows, err := q.QueryContext(ctx, dripCleanOrderedSQL, pq.Array(lanes), start, end)
	if err != nil {
		return nil, fmt.Errorf("clean ordered: %w", err)
	}
	defer rows.Close()
	out := map[string]dripOrderAgg{}
	for rows.Next() {
		var lane string
		var qty int64
		var cost float64
		if err := rows.Scan(&lane, &qty, &cost); err != nil {
			return nil, fmt.Errorf("clean ordered scan: %w", err)
		}
		out[lane] = dripOrderAgg{Quantity: int(qty), Cost: cost}
	}
	return out, rows.Err()
}

// dripSentTodaySQL is the ONE org-scoped read in this file (mailing_campaigns
// carries organization_id; the drip tables do not). Lane comes from the drip
// campaign naming convention, byte-identical to economics.go's econCampaignCTE
// so the two never disagree about which campaign belongs to which lane.
const dripSentTodaySQL = `
	SELECT split_part(name, ' ', 2) AS lane, COALESCE(SUM(sent_count), 0)::bigint
	  FROM mailing_campaigns
	 WHERE organization_id = $1
	   AND name LIKE '[partner-drip] %'
	   AND scheduled_at >= $2 AND scheduled_at < $3
	 GROUP BY 1`

func dripSentTodayByLane(ctx context.Context, q dripQueryer, day time.Time, orgID uuid.UUID, lanes []string) (map[string]int, error) {
	start, end := dripSupplyDayBounds(day)
	rows, err := q.QueryContext(ctx, dripSentTodaySQL, orgID, start, end)
	if err != nil {
		return nil, fmt.Errorf("sent today: %w", err)
	}
	defer rows.Close()
	want := map[string]bool{}
	for _, l := range lanes {
		want[l] = true
	}
	out := map[string]int{}
	for rows.Next() {
		var lane string
		var n int64
		if err := rows.Scan(&lane, &n); err != nil {
			return nil, fmt.Errorf("sent today scan: %w", err)
		}
		lane = strings.ToLower(strings.TrimSpace(lane))
		if want[lane] {
			out[lane] += int(n)
		}
	}
	return out, rows.Err()
}

// dripPausedSQL answers "is this lane stopped" from the two places the operator
// can stop one: an emergency-paused / inactive dataset, and an emptied brand
// roster. A lane with no live dataset OR no active roster brand cannot mail, so
// it renders grey rather than red — grey is "off on purpose", red is "broken".
const dripPausedSQL = `
	SELECT l.lane,
	       COALESCE(bool_or(NOT d.paused_emergency AND d.status = 'active'), FALSE) AS live_dataset,
	       COALESCE(bool_or(rr.active), FALSE)                                      AS live_roster
	  FROM unnest($1::text[]) AS l(lane)
	  LEFT JOIN partner_datasets d ON d.vertical = l.lane
	  LEFT JOIN partner_drip_vertical_roster rr ON rr.vertical = l.lane
	 GROUP BY 1`

func dripPausedByLane(ctx context.Context, q dripQueryer, lanes []string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, dripPausedSQL, pq.Array(lanes))
	if err != nil {
		return nil, fmt.Errorf("lane pause state: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var lane string
		var dataset, roster bool
		if err := rows.Scan(&lane, &dataset, &roster); err != nil {
			return nil, fmt.Errorf("lane pause state scan: %w", err)
		}
		out[lane] = !dataset || !roster
	}
	return out, rows.Err()
}

func dripScanLaneCounts(ctx context.Context, q dripQueryer, query string, into map[string]int, args ...any) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var lane string
		var n int64
		if err := rows.Scan(&lane, &n); err != nil {
			return err
		}
		into[strings.ToLower(strings.TrimSpace(lane))] += int(n)
	}
	return rows.Err()
}

func dripSortedInts[V any](m map[int]V) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /supply/lanes/{lane} — the lane pane
// ─────────────────────────────────────────────────────────────────────────────

type dripLaneISPDemand struct {
	ISP                string `json:"isp"`
	Desired            *int   `json:"desired"`
	AwardedFirm        *int   `json:"awarded_firm"`
	AwardedProvisional *int   `json:"awarded_provisional"`
	SupplyBacked       *int   `json:"supply_backed"`
	Unserved           *int   `json:"unserved"`
	UnservedReason     string `json:"unserved_reason"`
	FollowupsReserved  *int   `json:"followups_reserved"`
	Reserved           *int   `json:"reserved"`
	Committed          *int   `json:"committed"`
	FreshMailable      *int   `json:"fresh_mailable"`
	FollowupsDue       *int   `json:"followups_due"`
	PendingEO          *int   `json:"pending_eo"`
	Excluded           bool   `json:"excluded"`
}

type dripLaneCapacityCell struct {
	SendingDomain   string   `json:"sending_domain"`
	ISP             string   `json:"isp"`
	Contracted      *int     `json:"contracted"`
	Effective       *int     `json:"effective"`
	EffectiveReason string   `json:"effective_reason"`
	Planned         *int     `json:"planned"`
	Reserved        *int     `json:"reserved"`
	Submitted       *int     `json:"submitted"`
	Remaining       *int     `json:"remaining"`
	BlockedReason   string   `json:"blocked_reason"`
	Tokens          *float64 `json:"tokens"`
}

type dripLaneSupplyCell struct {
	SourceSlug string         `json:"source_slug"`
	ISP        string         `json:"isp"`
	Events     map[string]int `json:"events"`
	CostUSD    float64        `json:"cost_usd"`
}

type dripLaneEconomicsRow struct {
	ISP              string   `json:"isp"`
	Messages         int      `json:"messages"`
	Conversions      int      `json:"conversions"`
	RevenueUSD       float64  `json:"revenue_usd"`
	GrossECPM        *float64 `json:"gross_ecpm"`
	ContributionECPM *float64 `json:"contribution_ecpm"`
	FullyLoadedECPM  *float64 `json:"fully_loaded_ecpm"`
	CleaningValue    *float64 `json:"cleaning_value"`
	Maturity         string   `json:"maturity"`
	SampleOK         bool     `json:"sample_ok"`
}

type dripTickOutcomeRow struct {
	Tick       time.Time       `json:"tick"`
	Pass       string          `json:"pass"`
	Outcome    string          `json:"outcome"`
	Reason     string          `json:"reason"`
	Claimed    int             `json:"claimed"`
	CampaignID *string         `json:"campaign_id"`
	CapsSeen   json.RawMessage `json:"caps_seen"`
}

type dripContractSummary struct {
	Kind         string             `json:"kind"`
	Subject      string             `json:"subject"`
	Version      int                `json:"version"`
	Status       string             `json:"status"`
	EffectiveAt  time.Time          `json:"effective_at"`
	TokenPresent bool               `json:"token_present"`
	Metadata     contractmeta.Block `json:"metadata"`
}

type dripLaneResponse struct {
	dripSupplyMeta
	Lane          string                 `json:"lane"`
	Tier          int                    `json:"tier"`
	Paused        bool                   `json:"paused"`
	Demand        []dripLaneISPDemand    `json:"demand_by_isp"`
	Capacity      []dripLaneCapacityCell `json:"capacity_by_domain_isp"`
	Supply        []dripLaneSupplyCell   `json:"supply_by_source_isp"`
	Economics     []dripLaneEconomicsRow `json:"economics_by_isp"`
	TickOutcomes  []dripTickOutcomeRow   `json:"tick_outcomes_24h"`
	Contracts     []dripContractSummary  `json:"contracts"`
	DispatchValue dripDispatchValue      `json:"dispatch_value"`
}

func dripLaneLabels() map[string]string {
	return map[string]string{
		"demand_by_isp.desired":             dripLabelContracted,
		"demand_by_isp.awarded_firm":        dripLabelPlanned,
		"demand_by_isp.awarded_provisional": dripLabelPlanned,
		"demand_by_isp.supply_backed":       dripLabelPlanned,
		"demand_by_isp.unserved":            dripLabelPlanned,
		"demand_by_isp.followups_reserved":  dripLabelPlanned,
		"demand_by_isp.reserved":            dripLabelReserved,
		"demand_by_isp.committed":           dripLabelActual,
		"demand_by_isp.fresh_mailable":      dripLabelActual,
		"demand_by_isp.followups_due":       dripLabelActual,
		"demand_by_isp.pending_eo":          dripLabelForecast,
		"capacity.contracted":               dripLabelContracted,
		"capacity.effective":                dripLabelEffective,
		"capacity.planned":                  dripLabelPlanned,
		"capacity.reserved":                 dripLabelReserved,
		"capacity.submitted":                dripLabelActual,
		"capacity.remaining":                dripLabelEffective,
		"capacity.tokens":                   dripLabelEffective,
		"supply_by_source_isp.events":       dripLabelActual,
		"economics_by_isp":                  dripLabelActual,
		"tick_outcomes_24h":                 dripLabelActual,
	}
}

// HandleLane GET /api/mailing/supply/lanes/{lane}?day=
func (s *DripSupplyService) HandleLane(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.org(w, r); !ok {
		return
	}
	lane := dripSubject(chi.URLParam(r, "lane"))
	if lane == "" {
		respondError(w, http.StatusBadRequest, "lane is required")
		return
	}
	day, err := dripSupplyDay(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	key, ok := dripContractKey(w)
	if !ok {
		return
	}
	ctx := r.Context()
	tx, err := s.readTx(ctx)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply lane: "+err.Error())
		return
	}
	defer func() { _ = tx.Rollback() }()

	contracts, err := dripsupply.LoadActiveWithKey(ctx, tx, day, key)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply lane: load contracts: "+err.Error())
		return
	}
	out, err := s.laneDetail(ctx, tx, day, lane, contracts)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply lane: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, out)
}

// laneDetail is the tested seam (see ecosystem's note).
func (s *DripSupplyService) laneDetail(ctx context.Context, tx dripQueryer, day time.Time, lane string, contracts *dripsupply.ActiveSet) (*dripLaneResponse, error) {
	out := &dripLaneResponse{dripSupplyMeta: dripMeta(day, dripLaneLabels()), Lane: lane}
	out.ContractVersions = map[string]int{}

	dc := contracts.Dispatches[lane]
	if dc == nil {
		// Fail closed and SAY SO (§2.1): a lane with no active dispatch
		// contract is skipped by the executor, and the pane must show that
		// rather than an empty lane that looks merely quiet.
		out.Degraded = append(out.Degraded, "no active dispatch contract for this lane — the executor skips it (skipped:no_contract)")
	} else {
		out.Tier = dc.OperatorPriorityTier
		out.ContractVersions["dispatch"] = dc.Meta.Version
	}
	inv := contracts.Inventories[lane]
	if inv == nil {
		out.Degraded = append(out.Degraded, "no active inventory contract for this lane — no cleaning, remail or verdict window is defined")
	} else {
		out.ContractVersions["inventory"] = inv.Meta.Version
	}

	lanes := []string{lane}
	paused, err := dripPausedByLane(ctx, tx, lanes)
	if err != nil {
		return nil, err
	}
	out.Paused = paused[lane]

	if err := s.laneDemand(ctx, tx, day, lane, dc, contracts, out); err != nil {
		return nil, err
	}
	if err := s.laneCapacity(ctx, tx, day, lane, out); err != nil {
		return nil, err
	}
	if err := s.laneSupplyLedger(ctx, tx, day, lane, out); err != nil {
		return nil, err
	}
	if err := s.laneEconomics(ctx, tx, day, lane, out); err != nil {
		return nil, err
	}
	if err := s.laneTicks(ctx, tx, lane, out); err != nil {
		return nil, err
	}
	if err := s.laneContracts(ctx, tx, lane, out); err != nil {
		return nil, err
	}
	ranks, err := dripsupply.RankInputs(ctx, tx, day)
	if err != nil {
		return nil, fmt.Errorf("rank inputs: %w", err)
	}
	out.DispatchValue = dripDispatchValueFor(ranks, lane)
	return out, nil
}

const dripLaneDemandBalanceSQL = `
	SELECT isp, desired, awarded_firm, awarded_provisional, reserved, committed
	  FROM drip_lane_balance WHERE day = $1 AND lane = $2`

const dripLaneDemandPlanSQL = `
	SELECT isp,
	       COALESCE(SUM(award_firm), 0)::bigint,
	       COALESCE(SUM(award_provisional), 0)::bigint,
	       COALESCE(SUM(followups_reserved), 0)::bigint,
	       COALESCE(SUM(unserved), 0)::bigint,
	       COALESCE(mode() WITHIN GROUP (ORDER BY unserved_reason) FILTER (WHERE unserved_reason <> ''), '')
	  FROM drip_daily_plan WHERE day = $1 AND lane = $2 GROUP BY 1`

const dripLaneInventoryByISPSQL = `
	SELECT isp_family,
	       COUNT(*) FILTER (WHERE status = 'ready' AND mailed_at IS NULL AND touch_count = 0 AND validated_at >= $2)::bigint AS fresh,
	       COUNT(*) FILTER (WHERE status = 'mailed' AND next_touch_at < $3 AND engaged_at IS NULL AND terminal_reason IS NULL)::bigint AS followups,
	       COUNT(*) FILTER (WHERE status IN ('pending_eo','eo_in_flight'))::bigint AS pending_eo
	  FROM partner_clean_queue
	 WHERE vertical = $1
	   AND (
	         (status = 'ready' AND mailed_at IS NULL AND touch_count = 0 AND validated_at >= $2)
	      OR (status = 'mailed' AND next_touch_at < $3 AND engaged_at IS NULL AND terminal_reason IS NULL)
	      OR status IN ('pending_eo','eo_in_flight')
	   )
	 GROUP BY 1`

func (s *DripSupplyService) laneDemand(ctx context.Context, tx dripQueryer, day time.Time, lane string,
	dc *dripsupply.DispatchContract, contracts *dripsupply.ActiveSet, out *dripLaneResponse) error {

	byISP := map[string]*dripLaneISPDemand{}
	get := func(isp string) *dripLaneISPDemand {
		isp = strings.ToLower(strings.TrimSpace(isp))
		if c, ok := byISP[isp]; ok {
			return c
		}
		c := &dripLaneISPDemand{ISP: isp}
		byISP[isp] = c
		return c
	}

	// The contract states the demand; the balance and plan state what became of
	// it. Seeding from the contract means an ISP the planner never reached
	// still shows its desired number instead of vanishing.
	if dc != nil {
		excluded := map[string]bool{}
		for _, e := range dc.ISPExclusions {
			excluded[strings.ToLower(strings.TrimSpace(e))] = true
		}
		for isp, n := range dc.DesiredDailyIntros {
			c := get(isp)
			c.Desired = dsupInt(n)
			c.Excluded = excluded[c.ISP]
		}
		for isp := range excluded {
			get(isp).Excluded = true
		}
	}

	rows, err := tx.QueryContext(ctx, dripLaneDemandBalanceSQL, day, lane)
	if err != nil {
		return fmt.Errorf("lane demand balances: %w", err)
	}
	for rows.Next() {
		var isp string
		var desired, firm, prov, reserved, committed int64
		if err := rows.Scan(&isp, &desired, &firm, &prov, &reserved, &committed); err != nil {
			rows.Close()
			return fmt.Errorf("lane demand balances scan: %w", err)
		}
		c := get(isp)
		c.Desired = dsupInt(int(desired))
		c.AwardedFirm = dsupInt(int(firm))
		c.AwardedProvisional = dsupInt(int(prov))
		c.SupplyBacked = dsupInt(int(firm + prov))
		c.Reserved = dsupInt(int(reserved))
		c.Committed = dsupInt(int(committed))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("lane demand balances: %w", err)
	}

	prows, err := tx.QueryContext(ctx, dripLaneDemandPlanSQL, day, lane)
	if err != nil {
		return fmt.Errorf("lane demand plan: %w", err)
	}
	for prows.Next() {
		var isp, reason string
		var firm, prov, fups, unserved int64
		if err := prows.Scan(&isp, &firm, &prov, &fups, &unserved, &reason); err != nil {
			prows.Close()
			return fmt.Errorf("lane demand plan scan: %w", err)
		}
		c := get(isp)
		c.AwardedFirm = dsupInt(int(firm))
		c.AwardedProvisional = dsupInt(int(prov))
		c.SupplyBacked = dsupInt(int(firm + prov))
		c.FollowupsReserved = dsupInt(int(fups))
		c.Unserved = dsupInt(int(unserved))
		c.UnservedReason = reason
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return fmt.Errorf("lane demand plan: %w", err)
	}

	validDays := 60
	if inv := contracts.Inventories[lane]; inv != nil && inv.VerdictValidDays > 0 {
		validDays = inv.VerdictValidDays
	}
	_, dayEnd := dripSupplyDayBounds(day)
	irows, err := tx.QueryContext(ctx, dripLaneInventoryByISPSQL, lane, day.AddDate(0, 0, -validDays), dayEnd)
	if err != nil {
		return fmt.Errorf("lane inventory by isp: %w", err)
	}
	for irows.Next() {
		var isp string
		var fresh, fups, pending int64
		if err := irows.Scan(&isp, &fresh, &fups, &pending); err != nil {
			irows.Close()
			return fmt.Errorf("lane inventory by isp scan: %w", err)
		}
		c := get(isp)
		c.FreshMailable = dsupInt(int(fresh))
		c.FollowupsDue = dsupInt(int(fups))
		c.PendingEO = dsupInt(int(pending))
	}
	irows.Close()
	if err := irows.Err(); err != nil {
		return fmt.Errorf("lane inventory by isp: %w", err)
	}

	keys := make([]string, 0, len(byISP))
	for k := range byISP {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out.Demand = make([]dripLaneISPDemand, 0, len(keys))
	for _, k := range keys {
		out.Demand = append(out.Demand, *byISP[k])
	}
	return nil
}

// dripLaneCapacitySQL joins the day's domain×ISP balance (contracted /
// effective / tokens — estate-wide, shared by every lane on that domain) to
// THIS lane's own plan and ledger totals. The two are deliberately different
// grains and are labelled as such: `contracted`/`effective` are the domain's,
// `planned`/`reserved`/`submitted` are the lane's share.
const dripLaneCapacitySQL = `
	WITH pl AS (
	    SELECT sending_domain, isp,
	           SUM(award_firm + award_provisional + followups_reserved)::bigint AS planned
	      FROM drip_daily_plan WHERE day = $1 AND lane = $2 GROUP BY 1, 2
	), lg AS (
	    SELECT sending_domain, isp,
	           SUM(reserved)::bigint  AS reserved,
	           SUM(committed)::bigint AS committed,
	           mode() WITHIN GROUP (ORDER BY binding_reason) FILTER (WHERE binding_reason <> 'requested') AS binding
	      FROM drip_capacity_ledger WHERE day = $1 AND lane = $2 GROUP BY 1, 2
	)
	SELECT COALESCE(b.sending_domain, pl.sending_domain, lg.sending_domain) AS sending_domain,
	       COALESCE(b.isp, pl.isp, lg.isp)                                  AS isp,
	       b.contracted, b.effective, COALESCE(b.effective_reason, ''), b.tokens,
	       pl.planned, lg.reserved, lg.committed, COALESCE(lg.binding, '')
	  FROM pl
	  FULL OUTER JOIN lg ON lg.sending_domain = pl.sending_domain AND lg.isp = pl.isp
	  LEFT JOIN drip_capacity_balance b
	         ON b.day = $1
	        AND b.sending_domain = COALESCE(pl.sending_domain, lg.sending_domain)
	        AND b.isp = COALESCE(pl.isp, lg.isp)
	 ORDER BY 1, 2`

func (s *DripSupplyService) laneCapacity(ctx context.Context, tx dripQueryer, day time.Time, lane string, out *dripLaneResponse) error {
	rows, err := tx.QueryContext(ctx, dripLaneCapacitySQL, day, lane)
	if err != nil {
		return fmt.Errorf("lane capacity: %w", err)
	}
	defer rows.Close()
	out.Capacity = []dripLaneCapacityCell{}
	for rows.Next() {
		var domain, isp, effReason, binding string
		var contracted, effective, planned, reserved, committed sql.NullInt64
		var tokens sql.NullFloat64
		if err := rows.Scan(&domain, &isp, &contracted, &effective, &effReason, &tokens,
			&planned, &reserved, &committed, &binding); err != nil {
			return fmt.Errorf("lane capacity scan: %w", err)
		}
		cell := dripLaneCapacityCell{
			SendingDomain: domain, ISP: isp,
			Contracted: dsupNullInt(contracted), Effective: dsupNullInt(effective),
			EffectiveReason: effReason, Tokens: dsupNullFloat(tokens),
			Planned: dsupNullInt(planned), Reserved: dsupNullInt(reserved),
			Submitted: dsupNullInt(committed), BlockedReason: binding,
		}
		// Remaining is the DOMAIN's headroom, and it is only knowable when the
		// balance row exists; without it the cell is unknown, not full.
		if effective.Valid {
			var used int64
			if err := s.domainUsage(ctx, tx, day, domain, isp, &used); err != nil {
				return err
			}
			rem := effective.Int64 - used
			if rem < 0 {
				rem = 0
			}
			cell.Remaining = dsupInt(int(rem))
		}
		if cell.BlockedReason == "" && effReason != "" {
			cell.BlockedReason = "governor:" + effReason
		}
		out.Capacity = append(out.Capacity, cell)
	}
	return rows.Err()
}

const dripDomainUsageSQL = `
	SELECT COALESCE(reserved, 0)::bigint + COALESCE(committed, 0)::bigint
	  FROM drip_capacity_balance WHERE day = $1 AND sending_domain = $2 AND isp = $3`

func (s *DripSupplyService) domainUsage(ctx context.Context, tx dripQueryer, day time.Time, domain, isp string, out *int64) error {
	switch err := tx.QueryRowContext(ctx, dripDomainUsageSQL, day, domain, isp).Scan(out); {
	case err == nil, errors.Is(err, sql.ErrNoRows):
		return nil
	default:
		return fmt.Errorf("domain usage %s/%s: %w", domain, isp, err)
	}
}

const dripLaneSupplyLedgerSQL = `
	SELECT source_slug, isp, event,
	       COALESCE(SUM(quantity), 0)::bigint,
	       COALESCE(SUM(total_cost), 0)::float8
	  FROM drip_supply_ledger
	 WHERE lane = $1 AND occurred_at >= $2 AND occurred_at < $3
	 GROUP BY 1, 2, 3
	 ORDER BY 1, 2, 3`

func (s *DripSupplyService) laneSupplyLedger(ctx context.Context, tx dripQueryer, day time.Time, lane string, out *dripLaneResponse) error {
	start, end := dripSupplyDayBounds(day)
	rows, err := tx.QueryContext(ctx, dripLaneSupplyLedgerSQL, lane, start, end)
	if err != nil {
		return fmt.Errorf("lane supply ledger: %w", err)
	}
	defer rows.Close()
	idx := map[string]int{}
	out.Supply = []dripLaneSupplyCell{}
	for rows.Next() {
		var source, isp, event string
		var qty int64
		var cost float64
		if err := rows.Scan(&source, &isp, &event, &qty, &cost); err != nil {
			return fmt.Errorf("lane supply ledger scan: %w", err)
		}
		k := source + "|" + isp
		i, ok := idx[k]
		if !ok {
			out.Supply = append(out.Supply, dripLaneSupplyCell{SourceSlug: source, ISP: isp, Events: map[string]int{}})
			i = len(out.Supply) - 1
			idx[k] = i
		}
		out.Supply[i].Events[event] += int(qty)
		out.Supply[i].CostUSD += cost
	}
	return rows.Err()
}

const dripLaneEconomicsSQL = `
	SELECT isp, messages, conversions,
	       (revenue_everflow + revenue_manual)::float8,
	       gross_ecpm::float8, contribution_ecpm::float8,
	       fully_loaded_ecpm::float8, cleaning_value::float8,
	       maturity, sample_ok
	  FROM drip_lane_economics
	 WHERE lane = $1 AND day = $2
	 ORDER BY isp`

func (s *DripSupplyService) laneEconomics(ctx context.Context, tx dripQueryer, day time.Time, lane string, out *dripLaneResponse) error {
	rows, err := tx.QueryContext(ctx, dripLaneEconomicsSQL, lane, day)
	if err != nil {
		return fmt.Errorf("lane economics: %w", err)
	}
	defer rows.Close()
	out.Economics = []dripLaneEconomicsRow{}
	for rows.Next() {
		var e dripLaneEconomicsRow
		var gross, contrib, loaded, cleaning sql.NullFloat64
		if err := rows.Scan(&e.ISP, &e.Messages, &e.Conversions, &e.RevenueUSD,
			&gross, &contrib, &loaded, &cleaning, &e.Maturity, &e.SampleOK); err != nil {
			return fmt.Errorf("lane economics scan: %w", err)
		}
		e.GrossECPM = dsupNullFloat(gross)
		e.ContributionECPM = dsupNullFloat(contrib)
		// fully_loaded_ecpm and cleaning_value are NULL ON PURPOSE (WP1 §4):
		// infra is unallocated and a sub-threshold sample has no cleaning
		// value. They stay null here — never coerced to 0.
		e.FullyLoadedECPM = dsupNullFloat(loaded)
		e.CleaningValue = dsupNullFloat(cleaning)
		out.Economics = append(out.Economics, e)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(out.Economics) == 0 {
		out.Degraded = append(out.Degraded, "no drip_lane_economics rows for this lane and day — economics are unknown, not zero")
	}
	return nil
}

const dripLaneTicksSQL = `
	SELECT tick, pass, outcome, reason, claimed, campaign_id::text, caps_seen
	  FROM drip_tick_outcomes
	 WHERE lane = $1 AND tick >= $2
	 ORDER BY tick DESC, pass
	 LIMIT $3`

func (s *DripSupplyService) laneTicks(ctx context.Context, tx dripQueryer, lane string, out *dripLaneResponse) error {
	rows, err := tx.QueryContext(ctx, dripLaneTicksSQL, lane, time.Now().Add(-dripSupplyTickWindow), dripSupplyDefaultLimit)
	if err != nil {
		return fmt.Errorf("lane tick outcomes: %w", err)
	}
	defer rows.Close()
	out.TickOutcomes = []dripTickOutcomeRow{}
	for rows.Next() {
		var t dripTickOutcomeRow
		var campaign sql.NullString
		var caps []byte
		if err := rows.Scan(&t.Tick, &t.Pass, &t.Outcome, &t.Reason, &t.Claimed, &campaign, &caps); err != nil {
			return fmt.Errorf("lane tick outcomes scan: %w", err)
		}
		if campaign.Valid {
			v := campaign.String
			t.CampaignID = &v
		}
		if len(caps) > 0 {
			t.CapsSeen = json.RawMessage(caps)
		}
		out.TickOutcomes = append(out.TickOutcomes, t)
	}
	return rows.Err()
}

func (s *DripSupplyService) laneContracts(ctx context.Context, tx dripQueryer, lane string, out *dripLaneResponse) error {
	out.Contracts = []dripContractSummary{}
	for _, kind := range []dripsupply.ContractKind{dripsupply.KindDispatch, dripsupply.KindInventory} {
		got, err := dripContractRows(ctx, tx, kind, lane, "active", "scheduled")
		if err != nil {
			return err
		}
		out.Contracts = append(out.Contracts, got...)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /supply/domains and /supply/domains/{domain}
// ─────────────────────────────────────────────────────────────────────────────

type dripDomainRow struct {
	SendingDomain   string     `json:"sending_domain"`
	Contracted      *int       `json:"contracted"`
	Effective       *int       `json:"effective"`
	EffectiveReason string     `json:"effective_reason"`
	Reserved        *int       `json:"reserved"`
	Committed       *int       `json:"committed"`
	Released        *int       `json:"released"`
	Remaining       *int       `json:"remaining"`
	Status          string     `json:"status"` // open | met
	ISPCells        int        `json:"isp_cells"`
	LastRefillTick  *time.Time `json:"last_refill_tick"`
}

type dripDomainsResponse struct {
	dripSupplyMeta
	Domains []dripDomainRow `json:"domains"`
}

type dripDomainISPRow struct {
	ISP             string     `json:"isp"`
	Contracted      *int       `json:"contracted"`
	Effective       *int       `json:"effective"`
	EffectiveReason string     `json:"effective_reason"`
	Tokens          *float64   `json:"tokens"`
	Reserved        *int       `json:"reserved"`
	Committed       *int       `json:"committed"`
	Released        *int       `json:"released"`
	Remaining       *int       `json:"remaining"`
	LastRefillTick  *time.Time `json:"last_refill_tick"`
}

type dripDomainResponse struct {
	dripSupplyMeta
	SendingDomain string                  `json:"sending_domain"`
	Buckets       []dripDomainISPRow      `json:"buckets_by_isp"`
	Ledger        []dripCapacityLedgerRow `json:"ledger"`
	Contracts     []dripContractSummary   `json:"contracts"`
}

func dripDomainLabels() map[string]string {
	return map[string]string{
		"contracted":       dripLabelContracted,
		"effective":        dripLabelEffective,
		"tokens":           dripLabelEffective,
		"reserved":         dripLabelReserved,
		"committed":        dripLabelActual,
		"released":         dripLabelActual,
		"remaining":        dripLabelEffective,
		"ledger.requested": dripLabelPlanned,
		"ledger.reserved":  dripLabelReserved,
		"ledger.committed": dripLabelActual,
	}
}

const dripDomainsSQL = `
	SELECT sending_domain,
	       COUNT(*)::bigint,
	       COALESCE(SUM(contracted), 0)::bigint,
	       COALESCE(SUM(effective), 0)::bigint,
	       COALESCE(SUM(reserved), 0)::bigint,
	       COALESCE(SUM(committed), 0)::bigint,
	       COALESCE(SUM(released), 0)::bigint,
	       COALESCE(mode() WITHIN GROUP (ORDER BY effective_reason) FILTER (WHERE effective_reason <> ''), ''),
	       MAX(last_refill_tick)
	  FROM drip_capacity_balance
	 WHERE day = $1
	 GROUP BY 1
	 ORDER BY 1`

// HandleDomains GET /api/mailing/supply/domains?day=
func (s *DripSupplyService) HandleDomains(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.org(w, r); !ok {
		return
	}
	day, err := dripSupplyDay(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	tx, err := s.readTx(ctx)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply domains: "+err.Error())
		return
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, dripDomainsSQL, day)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply domains: "+err.Error())
		return
	}
	defer rows.Close()
	out := dripDomainsResponse{dripSupplyMeta: dripMeta(day, dripDomainLabels()), Domains: []dripDomainRow{}}
	for rows.Next() {
		var d dripDomainRow
		var cells, contracted, effective, reserved, committed, released int64
		var refill sql.NullTime
		if err := rows.Scan(&d.SendingDomain, &cells, &contracted, &effective, &reserved,
			&committed, &released, &d.EffectiveReason, &refill); err != nil {
			respondError(w, http.StatusInternalServerError, "supply domains scan: "+err.Error())
			return
		}
		d.ISPCells = int(cells)
		d.Contracted = dsupInt(int(contracted))
		d.Effective = dsupInt(int(effective))
		d.Reserved = dsupInt(int(reserved))
		d.Committed = dsupInt(int(committed))
		d.Released = dsupInt(int(released))
		rem := effective - reserved - committed
		if rem < 0 {
			rem = 0
		}
		d.Remaining = dsupInt(int(rem))
		if rem == 0 {
			d.Status = "met"
		} else {
			d.Status = "open"
		}
		d.LastRefillTick = dsupNullTime(refill)
		out.Domains = append(out.Domains, d)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "supply domains: "+err.Error())
		return
	}
	if len(out.Domains) == 0 {
		out.Degraded = append(out.Degraded, "no drip_capacity_balance rows for this day — the estate's capacity is unknown, not zero")
	}
	respondJSON(w, http.StatusOK, out)
}

const dripDomainBucketsSQL = `
	SELECT isp, contracted, effective, COALESCE(effective_reason, ''), tokens,
	       reserved, committed, released, last_refill_tick
	  FROM drip_capacity_balance
	 WHERE day = $1 AND sending_domain = $2
	 ORDER BY isp`

// HandleDomain GET /api/mailing/supply/domains/{domain}?day=
func (s *DripSupplyService) HandleDomain(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.org(w, r); !ok {
		return
	}
	domain := dripSubject(chi.URLParam(r, "domain"))
	if domain == "" {
		respondError(w, http.StatusBadRequest, "domain is required")
		return
	}
	day, err := dripSupplyDay(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	tx, err := s.readTx(ctx)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply domain: "+err.Error())
		return
	}
	defer func() { _ = tx.Rollback() }()

	out := dripDomainResponse{
		dripSupplyMeta: dripMeta(day, dripDomainLabels()),
		SendingDomain:  domain,
		Buckets:        []dripDomainISPRow{},
	}
	rows, err := tx.QueryContext(ctx, dripDomainBucketsSQL, day, domain)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply domain buckets: "+err.Error())
		return
	}
	for rows.Next() {
		var b dripDomainISPRow
		var contracted, effective, reserved, committed, released sql.NullInt64
		var tokens sql.NullFloat64
		var refill sql.NullTime
		if err := rows.Scan(&b.ISP, &contracted, &effective, &b.EffectiveReason, &tokens,
			&reserved, &committed, &released, &refill); err != nil {
			rows.Close()
			respondError(w, http.StatusInternalServerError, "supply domain buckets scan: "+err.Error())
			return
		}
		b.Contracted = dsupNullInt(contracted)
		b.Effective = dsupNullInt(effective)
		b.Tokens = dsupNullFloat(tokens)
		b.Reserved = dsupNullInt(reserved)
		b.Committed = dsupNullInt(committed)
		b.Released = dsupNullInt(released)
		if effective.Valid {
			rem := effective.Int64 - reserved.Int64 - committed.Int64
			if rem < 0 {
				rem = 0
			}
			b.Remaining = dsupInt(int(rem))
		}
		b.LastRefillTick = dsupNullTime(refill)
		out.Buckets = append(out.Buckets, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "supply domain buckets: "+err.Error())
		return
	}

	ledger, err := dripReadCapacityLedger(ctx, tx, dripCapacityLedgerFilter{
		Day: day, Domain: domain, Limit: dripSupplyDefaultLimit,
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply domain ledger: "+err.Error())
		return
	}
	out.Ledger = ledger

	contracts, err := dripContractRows(ctx, tx, dripsupply.KindDomain, domain, "active", "scheduled")
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply domain contracts: "+err.Error())
		return
	}
	out.Contracts = contracts
	for _, c := range contracts {
		if c.Status == "active" {
			out.ContractVersions = map[string]int{"domain": c.Version}
		}
	}
	if len(out.Buckets) == 0 {
		out.Degraded = append(out.Degraded, "no drip_capacity_balance rows for this domain and day — capacity is unknown, not zero")
	}
	respondJSON(w, http.StatusOK, out)
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /supply/ledger/capacity, /supply/ledger/supply, /supply/plan
// ─────────────────────────────────────────────────────────────────────────────

type dripCapacityLedgerRow struct {
	AllocationID    string     `json:"allocation_id"`
	IdempotencyKey  string     `json:"idempotency_key"`
	Tick            time.Time  `json:"tick"`
	SendingDomain   string     `json:"sending_domain"`
	ISP             string     `json:"isp"`
	Lane            string     `json:"lane"`
	TouchClass      string     `json:"touch_class"`
	DomainVersion   int        `json:"domain_contract_version"`
	DispatchVersion int        `json:"dispatch_contract_version"`
	Requested       int        `json:"requested"`
	Reserved        int        `json:"reserved"`
	Committed       int        `json:"committed"`
	Released        int        `json:"released"`
	Status          string     `json:"status"`
	CampaignID      *string    `json:"campaign_id"`
	BindingReason   string     `json:"binding_reason"`
	ReleaseReason   string     `json:"release_reason"`
	DomainAfter     int        `json:"domain_balance_after"`
	LaneUnfilled    int        `json:"lane_unfilled_after"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       *time.Time `json:"updated_at"`
}

type dripCapacityLedgerFilter struct {
	Day    time.Time
	Domain string
	ISP    string
	Lane   string
	Limit  int
}

// dripCapacityLedgerSQL uses NULL-tolerant equality on every optional filter so
// one prepared shape serves all four combinations — and every value is a bound
// parameter, never interpolated.
const dripCapacityLedgerSQL = `
	SELECT allocation_id::text, idempotency_key, tick, sending_domain, isp, lane, touch_class,
	       domain_contract_version, dispatch_contract_version,
	       requested, reserved, committed, released, status, campaign_id::text,
	       binding_reason, COALESCE(release_reason, ''),
	       domain_balance_after, lane_unfilled_after, created_at, updated_at
	  FROM drip_capacity_ledger
	 WHERE day = $1
	   AND ($2::text IS NULL OR sending_domain = $2)
	   AND ($3::text IS NULL OR isp = $3)
	   AND ($4::text IS NULL OR lane = $4)
	 ORDER BY tick DESC, allocation_id
	 LIMIT $5`

func dripNullableParam(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.ToLower(strings.TrimSpace(s))
}

func dripReadCapacityLedger(ctx context.Context, q dripQueryer, f dripCapacityLedgerFilter) ([]dripCapacityLedgerRow, error) {
	if f.Limit <= 0 {
		f.Limit = dripSupplyDefaultLimit
	}
	rows, err := q.QueryContext(ctx, dripCapacityLedgerSQL, f.Day,
		dripNullableParam(f.Domain), dripNullableParam(f.ISP), dripNullableParam(f.Lane), f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []dripCapacityLedgerRow{}
	for rows.Next() {
		var e dripCapacityLedgerRow
		var campaign sql.NullString
		var updated sql.NullTime
		if err := rows.Scan(&e.AllocationID, &e.IdempotencyKey, &e.Tick, &e.SendingDomain, &e.ISP, &e.Lane,
			&e.TouchClass, &e.DomainVersion, &e.DispatchVersion, &e.Requested, &e.Reserved, &e.Committed,
			&e.Released, &e.Status, &campaign, &e.BindingReason, &e.ReleaseReason,
			&e.DomainAfter, &e.LaneUnfilled, &e.CreatedAt, &updated); err != nil {
			return nil, err
		}
		if campaign.Valid {
			v := campaign.String
			e.CampaignID = &v
		}
		e.UpdatedAt = dsupNullTime(updated)
		out = append(out, e)
	}
	return out, rows.Err()
}

type dripCapacityLedgerResponse struct {
	dripSupplyMeta
	Filters map[string]string       `json:"filters"`
	Limit   int                     `json:"limit"`
	Entries []dripCapacityLedgerRow `json:"entries"`
}

// HandleCapacityLedger GET /api/mailing/supply/ledger/capacity?day=&domain=&isp=&lane=
func (s *DripSupplyService) HandleCapacityLedger(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.org(w, r); !ok {
		return
	}
	day, err := dripSupplyDay(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	f := dripCapacityLedgerFilter{
		Day:    day,
		Domain: dripSubject(r.URL.Query().Get("domain")),
		ISP:    dripSubject(r.URL.Query().Get("isp")),
		Lane:   dripSubject(r.URL.Query().Get("lane")),
		Limit:  dripLimit(r),
	}
	ctx := r.Context()
	tx, err := s.readTx(ctx)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply capacity ledger: "+err.Error())
		return
	}
	defer func() { _ = tx.Rollback() }()

	entries, err := dripReadCapacityLedger(ctx, tx, f)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply capacity ledger: "+err.Error())
		return
	}
	out := dripCapacityLedgerResponse{
		dripSupplyMeta: dripMeta(day, map[string]string{
			"entries.requested": dripLabelPlanned,
			"entries.reserved":  dripLabelReserved,
			"entries.committed": dripLabelActual,
			"entries.released":  dripLabelActual,
		}),
		Filters: map[string]string{"domain": f.Domain, "isp": f.ISP, "lane": f.Lane},
		Limit:   f.Limit,
		Entries: entries,
	}
	if len(entries) == f.Limit {
		out.Degraded = append(out.Degraded, fmt.Sprintf("truncated at limit=%d — narrow the filters or raise ?limit= (max %d)", f.Limit, dripSupplyMaxLimit))
	}
	respondJSON(w, http.StatusOK, out)
}

type dripSupplyLedgerRow struct {
	EntryID          string    `json:"entry_id"`
	OccurredAt       time.Time `json:"occurred_at"`
	Lane             string    `json:"lane"`
	SourceSlug       string    `json:"source_slug"`
	ISP              string    `json:"isp"`
	Event            string    `json:"event"`
	Quantity         int       `json:"quantity"`
	UnitCost         float64   `json:"unit_cost"`
	TotalCost        float64   `json:"total_cost"`
	BatchID          *string   `json:"batch_id"`
	ReservationID    *string   `json:"reservation_id"`
	Reason           string    `json:"reason"`
	SourceVersion    *int      `json:"source_contract_version"`
	InventoryVersion *int      `json:"inventory_contract_version"`
}

const dripSupplyLedgerListSQL = `
	SELECT entry_id::text, occurred_at, lane, source_slug, isp, event, quantity,
	       unit_cost::float8, total_cost::float8, batch_id::text, reservation_id::text,
	       COALESCE(reason, ''), source_contract_version, inventory_contract_version
	  FROM drip_supply_ledger
	 WHERE occurred_at >= $1 AND occurred_at < $2
	   AND ($3::text IS NULL OR lane = $3)
	   AND ($4::text IS NULL OR source_slug = $4)
	 ORDER BY occurred_at DESC, entry_id
	 LIMIT $5`

type dripSupplyLedgerResponse struct {
	dripSupplyMeta
	Filters map[string]string     `json:"filters"`
	Limit   int                   `json:"limit"`
	Entries []dripSupplyLedgerRow `json:"entries"`
}

// HandleSupplyLedger GET /api/mailing/supply/ledger/supply?day=&lane=&source=
func (s *DripSupplyService) HandleSupplyLedger(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.org(w, r); !ok {
		return
	}
	day, err := dripSupplyDay(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	lane := dripSubject(r.URL.Query().Get("lane"))
	source := dripSubject(r.URL.Query().Get("source"))
	limit := dripLimit(r)
	start, end := dripSupplyDayBounds(day)

	ctx := r.Context()
	tx, err := s.readTx(ctx)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply ledger: "+err.Error())
		return
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, dripSupplyLedgerListSQL, start, end,
		dripNullableParam(lane), dripNullableParam(source), limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply ledger: "+err.Error())
		return
	}
	defer rows.Close()
	out := dripSupplyLedgerResponse{
		dripSupplyMeta: dripMeta(day, map[string]string{
			"entries.quantity":   dripLabelActual,
			"entries.unit_cost":  dripLabelActual,
			"entries.total_cost": dripLabelActual,
		}),
		Filters: map[string]string{"lane": lane, "source": source},
		Limit:   limit,
		Entries: []dripSupplyLedgerRow{},
	}
	for rows.Next() {
		var e dripSupplyLedgerRow
		var batch, reservation sql.NullString
		var srcVer, invVer sql.NullInt64
		if err := rows.Scan(&e.EntryID, &e.OccurredAt, &e.Lane, &e.SourceSlug, &e.ISP, &e.Event,
			&e.Quantity, &e.UnitCost, &e.TotalCost, &batch, &reservation, &e.Reason, &srcVer, &invVer); err != nil {
			respondError(w, http.StatusInternalServerError, "supply ledger scan: "+err.Error())
			return
		}
		if batch.Valid {
			v := batch.String
			e.BatchID = &v
		}
		if reservation.Valid {
			v := reservation.String
			e.ReservationID = &v
		}
		e.SourceVersion = dsupNullInt(srcVer)
		e.InventoryVersion = dsupNullInt(invVer)
		out.Entries = append(out.Entries, e)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "supply ledger: "+err.Error())
		return
	}
	if len(out.Entries) == limit {
		out.Degraded = append(out.Degraded, fmt.Sprintf("truncated at limit=%d — narrow the filters or raise ?limit= (max %d)", limit, dripSupplyMaxLimit))
	}
	respondJSON(w, http.StatusOK, out)
}

type dripPlanRow struct {
	Lane              string     `json:"lane"`
	ISP               string     `json:"isp"`
	SendingDomain     string     `json:"sending_domain"`
	AwardFirm         int        `json:"award_firm"`
	AwardProvisional  int        `json:"award_provisional"`
	FollowupsReserved int        `json:"followups_reserved"`
	PlanShare         float64    `json:"plan_share"`
	Rank              int        `json:"rank"`
	RankReason        string     `json:"rank_reason"`
	Unserved          int        `json:"unserved"`
	UnservedReason    string     `json:"unserved_reason"`
	SupplyReleased    int        `json:"supply_released"`
	FrozenAt          *time.Time `json:"frozen_at"`
}

type dripPlanResponse struct {
	dripSupplyMeta
	FrozenAt *time.Time    `json:"plan_frozen_at"`
	Rows     []dripPlanRow `json:"rows"`
}

const dripPlanListSQL = `
	SELECT lane, isp, sending_domain, award_firm, award_provisional, followups_reserved,
	       plan_share::float8, rank, rank_reason, unserved, unserved_reason, supply_released, frozen_at
	  FROM drip_daily_plan
	 WHERE day = $1
	 ORDER BY rank, lane, isp, sending_domain
	 LIMIT $2`

// HandlePlan GET /api/mailing/supply/plan?day=
func (s *DripSupplyService) HandlePlan(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.org(w, r); !ok {
		return
	}
	day, err := dripSupplyDay(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := dripLimit(r)
	ctx := r.Context()
	tx, err := s.readTx(ctx)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply plan: "+err.Error())
		return
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, dripPlanListSQL, day, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply plan: "+err.Error())
		return
	}
	defer rows.Close()
	out := dripPlanResponse{
		dripSupplyMeta: dripMeta(day, map[string]string{
			"rows.award_firm":         dripLabelPlanned,
			"rows.award_provisional":  dripLabelPlanned,
			"rows.followups_reserved": dripLabelPlanned,
			"rows.plan_share":         dripLabelPlanned,
			"rows.unserved":           dripLabelPlanned,
			"rows.supply_released":    dripLabelPlanned,
		}),
		Rows: []dripPlanRow{},
	}
	for rows.Next() {
		var p dripPlanRow
		var frozen sql.NullTime
		if err := rows.Scan(&p.Lane, &p.ISP, &p.SendingDomain, &p.AwardFirm, &p.AwardProvisional,
			&p.FollowupsReserved, &p.PlanShare, &p.Rank, &p.RankReason, &p.Unserved,
			&p.UnservedReason, &p.SupplyReleased, &frozen); err != nil {
			respondError(w, http.StatusInternalServerError, "supply plan scan: "+err.Error())
			return
		}
		p.FrozenAt = dsupNullTime(frozen)
		out.FrozenAt = dripMaxTime(out.FrozenAt, p.FrozenAt)
		out.Rows = append(out.Rows, p)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "supply plan: "+err.Error())
		return
	}
	if len(out.Rows) == 0 {
		out.Degraded = append(out.Degraded, "no drip_daily_plan rows for this day — the planner has not frozen a plan; the day is unplanned, not empty")
	}
	respondJSON(w, http.StatusOK, out)
}

// ─────────────────────────────────────────────────────────────────────────────
// Contracts
// ─────────────────────────────────────────────────────────────────────────────

type dripContractVersionRow struct {
	ID             string             `json:"id"`
	Version        int                `json:"version"`
	Status         string             `json:"status"`
	EffectiveAt    time.Time          `json:"effective_at"`
	SupersededAt   *time.Time         `json:"superseded_at"`
	CreatedBy      string             `json:"created_by"`
	CreatedAt      time.Time          `json:"created_at"`
	ApprovedBy     string             `json:"approved_by"`
	ApprovedAt     *time.Time         `json:"approved_at"`
	ChangeLedgerID string             `json:"change_ledger_id"`
	Notes          string             `json:"notes"`
	TokenPresent   bool               `json:"token_present"`
	TokenIssuedAt  *time.Time         `json:"token_issued_at"`
	Metadata       contractmeta.Block `json:"metadata"`
}

type dripContractsResponse struct {
	dripSupplyMeta
	Kind      string                   `json:"kind"`
	Subject   string                   `json:"subject"`
	Active    *int                     `json:"active_version"`
	Scheduled *int                     `json:"scheduled_version"`
	Versions  []dripContractVersionRow `json:"versions"`
}

// dripContractSelectSQL builds the version listing for a kind. The table and
// subject column come from dripsupply.TableFor / SubjectColumnFor — the
// package's own map — so a kind this file does not know about is an error, not
// an injected identifier.
func dripContractSelectSQL(kind dripsupply.ContractKind, statuses []string) (string, error) {
	table, err := dripsupply.TableFor(kind)
	if err != nil {
		return "", err
	}
	col, err := dripsupply.SubjectColumnFor(kind)
	if err != nil {
		return "", err
	}
	q := `SELECT id::text, version, status, effective_at, superseded_at, created_by, created_at,
	             COALESCE(approved_by, ''), approved_at, change_ledger_id, notes, metadata, token
	        FROM ` + table + ` WHERE ` + col + ` = $1`
	if len(statuses) > 0 {
		q += ` AND status = ANY($2)`
	}
	q += ` ORDER BY version DESC`
	return q, nil
}

func dripScanContractVersions(ctx context.Context, q dripQueryer, kind dripsupply.ContractKind, subject string, statuses []string) ([]dripContractVersionRow, error) {
	query, err := dripContractSelectSQL(kind, statuses)
	if err != nil {
		return nil, err
	}
	var rows *sql.Rows
	if len(statuses) > 0 {
		rows, err = q.QueryContext(ctx, query, subject, pq.Array(statuses))
	} else {
		rows, err = q.QueryContext(ctx, query, subject)
	}
	if err != nil {
		return nil, fmt.Errorf("%s contract versions for %q: %w", kind, subject, err)
	}
	defer rows.Close()
	out := []dripContractVersionRow{}
	for rows.Next() {
		var v dripContractVersionRow
		var superseded, approvedAt sql.NullTime
		var token string
		if err := rows.Scan(&v.ID, &v.Version, &v.Status, &v.EffectiveAt, &superseded, &v.CreatedBy,
			&v.CreatedAt, &v.ApprovedBy, &approvedAt, &v.ChangeLedgerID, &v.Notes, &v.Metadata, &token); err != nil {
			return nil, fmt.Errorf("%s contract versions scan: %w", kind, err)
		}
		v.SupersededAt = dsupNullTime(superseded)
		v.ApprovedAt = dsupNullTime(approvedAt)
		// PRESENCE ONLY. The token is an HMAC over the contract body; the KEY
		// never leaves the process and the value itself is redacted here so a
		// screenshot of this screen cannot become an oracle.
		v.TokenPresent = strings.TrimSpace(token) != "" && v.Metadata.Token.Issued()
		if !v.Metadata.Token.IssuedAt.IsZero() {
			v.TokenIssuedAt = dsupTime(v.Metadata.Token.IssuedAt)
		}
		v.Metadata.Token = contractmeta.Token{
			Alg: v.Metadata.Token.Alg, IssuedAt: v.Metadata.Token.IssuedAt, IssuedBy: v.Metadata.Token.IssuedBy,
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func dripContractRows(ctx context.Context, q dripQueryer, kind dripsupply.ContractKind, subject string, statuses ...string) ([]dripContractSummary, error) {
	vs, err := dripScanContractVersions(ctx, q, kind, subject, statuses)
	if err != nil {
		return nil, err
	}
	out := make([]dripContractSummary, 0, len(vs))
	for _, v := range vs {
		out = append(out, dripContractSummary{
			Kind: string(kind), Subject: subject, Version: v.Version, Status: v.Status,
			EffectiveAt: v.EffectiveAt, TokenPresent: v.TokenPresent, Metadata: v.Metadata,
		})
	}
	return out, nil
}

// dripParseKind maps the URL segment to a contract kind, refusing anything else.
func dripParseKind(raw string) (dripsupply.ContractKind, error) {
	k := dripsupply.ContractKind(strings.ToLower(strings.TrimSpace(raw)))
	for _, known := range dripsupply.AllKinds() {
		if k == known {
			return k, nil
		}
	}
	return "", fmt.Errorf("unknown contract kind %q: must be one of domain, dispatch, inventory, source", raw)
}

// HandleContractVersions GET /api/mailing/supply/contracts/{kind}/{subject}
func (s *DripSupplyService) HandleContractVersions(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.org(w, r); !ok {
		return
	}
	kind, err := dripParseKind(chi.URLParam(r, "kind"))
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	subject := dripSubject(chi.URLParam(r, "subject"))
	if subject == "" {
		respondError(w, http.StatusBadRequest, "subject is required")
		return
	}
	day, err := dripSupplyDay(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	tx, err := s.readTx(ctx)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply contracts: "+err.Error())
		return
	}
	defer func() { _ = tx.Rollback() }()

	versions, err := dripScanContractVersions(ctx, tx, kind, subject, nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply contracts: "+err.Error())
		return
	}
	out := dripContractsResponse{
		dripSupplyMeta: dripMeta(day, map[string]string{"versions": dripLabelContracted}),
		Kind:           string(kind),
		Subject:        subject,
		Versions:       versions,
	}
	for i := range versions {
		switch versions[i].Status {
		case string(dripsupply.StatusActive):
			out.Active = dsupInt(versions[i].Version)
		case string(dripsupply.StatusScheduled):
			if out.Scheduled == nil {
				out.Scheduled = dsupInt(versions[i].Version)
			}
		}
	}
	if out.Active != nil {
		out.ContractVersions = map[string]int{string(kind): *out.Active}
	}
	if len(versions) == 0 {
		out.Degraded = append(out.Degraded, "no contract rows for this subject — the mediators fail closed on it (skipped:no_contract)")
	}
	respondJSON(w, http.StatusOK, out)
}

// dripDraftEnvelope is the POST body: the contract's policy body inline, plus
// the two audit fields and an optional segment ref list.
type dripDraftEnvelope struct {
	Notes       string   `json:"notes"`
	EffectiveAt string   `json:"effective_at"`
	SegmentIDs  []string `json:"segment_ids"`
	CreatedBy   string   `json:"created_by"`
}

// HandleContractDraft POST /api/mailing/supply/contracts/{kind}/{subject}
//
// Body is the contract's policy fields (the JSON tags on dripsupply's contract
// structs) plus optional `notes`, `effective_at` and `segment_ids`. The subject
// comes from the URL and OVERRIDES any subject in the body, so a draft can
// never be filed against a different lane than the one being edited.
//
// The write is dripsupply.InsertDraft, which validates first and refuses
// without created_by + change_ledger_id — so this handler cannot persist
// something the mediators would then have to guess about.
func (s *DripSupplyService) HandleContractDraft(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.org(w, r); !ok {
		return
	}
	kind, err := dripParseKind(chi.URLParam(r, "kind"))
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	subject := dripSubject(chi.URLParam(r, "subject"))
	if subject == "" {
		respondError(w, http.StatusBadRequest, "subject is required")
		return
	}
	raw, err := dripReadBody(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var env dripDraftEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	effectiveAt := dripNextDenverMidnight(time.Now())
	if strings.TrimSpace(env.EffectiveAt) != "" {
		t, perr := time.Parse(time.RFC3339, strings.TrimSpace(env.EffectiveAt))
		if perr != nil {
			respondError(w, http.StatusBadRequest, "effective_at must be RFC3339, got "+strconv.Quote(env.EffectiveAt))
			return
		}
		effectiveAt = t
	}

	actor := actorFromRequest(r)
	if strings.TrimSpace(env.CreatedBy) != "" && strings.TrimSpace(actor) == "unknown" {
		actor = strings.TrimSpace(env.CreatedBy)
	}
	// The portal audit id. §1.1 requires change_ledger_id on every version; a
	// portal write has no agents/jobs/change_ledger.py id, so it carries its
	// own namespaced one.
	changeLedgerID := "portal:" + uuid.NewString()

	contract, err := dripDecodeContract(kind, subject, raw)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	meta := dripContractMeta(contract)
	meta.CreatedBy = actor
	meta.ChangeLedgerID = changeLedgerID
	meta.EffectiveAt = effectiveAt
	meta.Notes = strings.TrimSpace(env.Notes)

	// Validate BEFORE opening a transaction so a bad body never takes a
	// connection, and so the 400 carries the full field list (§WP9).
	if verr := contract.Validate(); verr != nil {
		dripRespondValidation(w, verr)
		return
	}

	ctx := r.Context()
	tx, err := s.writeTx(ctx)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply contract draft: "+err.Error())
		return
	}
	defer func() { _ = tx.Rollback() }()

	// §1.5 rule 3: refs are resolved from live rows and fail closed on a miss.
	// Resolved at draft time so the operator learns about a missing sending
	// profile or dataset now, not at activation.
	refs, err := contractmeta.ResolveRefs(ctx, tx, dripRefSpec(kind, subject, contract, env.SegmentIDs))
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]any{
			"error":  "contract refs could not be resolved: " + err.Error(),
			"fields": []string{"refs"},
		})
		return
	}
	meta.Metadata.Refs = refs

	id, version, err := dripsupply.InsertDraft(ctx, tx, contract)
	if err != nil {
		var verrs dripsupply.ValidationErrors
		if errors.As(err, &verrs) {
			dripRespondValidation(w, verrs)
			return
		}
		respondError(w, http.StatusInternalServerError, "supply contract draft: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "supply contract draft commit: "+err.Error())
		return
	}
	respondJSON(w, http.StatusCreated, map[string]any{
		"id":               id.String(),
		"kind":             string(kind),
		"subject":          subject,
		"version":          version,
		"status":           string(dripsupply.StatusDraft),
		"effective_at":     effectiveAt,
		"created_by":       actor,
		"change_ledger_id": changeLedgerID,
		"as_of":            time.Now().UTC(),
		"labels":           map[string]string{"version": dripLabelContracted},
	})
}

// dripDecodeContract unmarshals the body into the right contract struct and
// stamps the URL's subject onto it.
func dripDecodeContract(kind dripsupply.ContractKind, subject string, raw []byte) (dripsupply.Contract, error) {
	switch kind {
	case dripsupply.KindDomain:
		var c dripsupply.DomainContract
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("invalid domain contract body: %w", err)
		}
		c.SendingDomain = subject
		return &c, nil
	case dripsupply.KindDispatch:
		var c dripsupply.DispatchContract
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("invalid dispatch contract body: %w", err)
		}
		c.Lane = subject
		return &c, nil
	case dripsupply.KindInventory:
		var c dripsupply.InventoryContract
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("invalid inventory contract body: %w", err)
		}
		c.Lane = subject
		return &c, nil
	case dripsupply.KindSource:
		var c dripsupply.SourceContract
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("invalid source contract body: %w", err)
		}
		c.SourceSlug = subject
		return &c, nil
	}
	return nil, fmt.Errorf("unknown contract kind %q", kind)
}

// dripContractMeta returns the mutable Meta of a decoded contract. The struct's
// Meta is embedded and exported, so this is a type switch rather than
// reflection.
func dripContractMeta(c dripsupply.Contract) *dripsupply.Meta {
	switch v := c.(type) {
	case *dripsupply.DomainContract:
		return &v.Meta
	case *dripsupply.DispatchContract:
		return &v.Meta
	case *dripsupply.InventoryContract:
		return &v.Meta
	case *dripsupply.SourceContract:
		return &v.Meta
	}
	return nil
}

// dripRefSpec says which refs a kind resolves (§1.5). Dispatch contracts carry
// brand codes rather than dataset slugs, so they resolve only the caller's
// segment ids — inventing a slug lookup for them would be a new pattern.
func dripRefSpec(kind dripsupply.ContractKind, subject string, c dripsupply.Contract, segmentIDs []string) contractmeta.RefSpec {
	spec := contractmeta.RefSpec{SegmentIDs: segmentIDs}
	switch kind {
	case dripsupply.KindDomain:
		spec.SendingDomain = subject
	case dripsupply.KindInventory:
		if inv, ok := c.(*dripsupply.InventoryContract); ok {
			spec.DatasetSlugs = inv.AcceptedSources
		}
	case dripsupply.KindSource:
		spec.DatasetSlugs = []string{subject}
	}
	return spec
}

// dripRespondValidation turns dripsupply's ValidationErrors into the 400 shape
// the portal form binds to: a message plus the exact field list.
func dripRespondValidation(w http.ResponseWriter, err error) {
	var verrs dripsupply.ValidationErrors
	if errors.As(err, &verrs) {
		respondJSON(w, http.StatusBadRequest, map[string]any{
			"error":  verrs.Error(),
			"fields": verrs.Fields(),
		})
		return
	}
	respondJSON(w, http.StatusBadRequest, map[string]any{
		"error":  err.Error(),
		"fields": []string{},
	})
}

// dripApproveSQL is guarded on `status = 'draft'`, so a second approve of the
// same version affects zero rows and the handler reports the real state rather
// than re-stamping approved_at.
func dripApproveSQL(kind dripsupply.ContractKind) (string, error) {
	table, err := dripsupply.TableFor(kind)
	if err != nil {
		return "", err
	}
	col, err := dripsupply.SubjectColumnFor(kind)
	if err != nil {
		return "", err
	}
	return `UPDATE ` + table + ` SET status = 'approved', approved_by = $1, approved_at = NOW()
	         WHERE ` + col + ` = $2 AND version = $3 AND status = 'draft'`, nil
}

func dripContractStatusSQL(kind dripsupply.ContractKind) (string, error) {
	table, err := dripsupply.TableFor(kind)
	if err != nil {
		return "", err
	}
	col, err := dripsupply.SubjectColumnFor(kind)
	if err != nil {
		return "", err
	}
	return `SELECT status FROM ` + table + ` WHERE ` + col + ` = $1 AND version = $2`, nil
}

// HandleContractApprove POST /api/mailing/supply/contracts/{kind}/{subject}/{version}/approve
//
// draft → approved → scheduled in ONE transaction. The token is issued by
// dripsupply.Schedule on the approved→scheduled edge (§1.5 rule 1), so this is
// the only place in the portal that mints one — and it refuses with 503 before
// touching the database when CONTRACT_TOKEN_KEY is unset, because a contract
// scheduled without a token can never be honoured by LoadActive.
func (s *DripSupplyService) HandleContractApprove(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.org(w, r); !ok {
		return
	}
	kind, err := dripParseKind(chi.URLParam(r, "kind"))
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	subject := dripSubject(chi.URLParam(r, "subject"))
	if subject == "" {
		respondError(w, http.StatusBadRequest, "subject is required")
		return
	}
	version, err := strconv.Atoi(strings.TrimSpace(chi.URLParam(r, "version")))
	if err != nil || version <= 0 {
		respondError(w, http.StatusBadRequest, "version must be a positive integer")
		return
	}
	// FAIL CLOSED FIRST: no key, no write, no partial state.
	key, ok := dripContractKey(w)
	if !ok {
		return
	}
	actor := actorFromRequest(r)

	ctx := r.Context()
	tx, err := s.writeTx(ctx)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply contract approve: "+err.Error())
		return
	}
	defer func() { _ = tx.Rollback() }()

	approve, err := dripApproveSQL(kind)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := tx.ExecContext(ctx, approve, actor, subject, version)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "supply contract approve: "+err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		statusQ, qerr := dripContractStatusSQL(kind)
		if qerr != nil {
			respondError(w, http.StatusBadRequest, qerr.Error())
			return
		}
		var status string
		switch err := tx.QueryRowContext(ctx, statusQ, subject, version).Scan(&status); {
		case errors.Is(err, sql.ErrNoRows):
			respondError(w, http.StatusNotFound,
				fmt.Sprintf("no %s contract %q at version %d", kind, subject, version))
		case err != nil:
			respondError(w, http.StatusInternalServerError, "supply contract approve: "+err.Error())
		default:
			respondError(w, http.StatusConflict,
				fmt.Sprintf("%s contract %q v%d is %q, not draft — only a draft can be approved", kind, subject, version, status))
		}
		return
	}

	tok, err := dripsupply.Schedule(ctx, tx, kind, subject, version, key, time.Now().UTC())
	if err != nil {
		var notApproved *dripsupply.ErrNotApproved
		var notFound *dripsupply.ErrContractNotFound
		switch {
		case errors.As(err, &notFound):
			respondError(w, http.StatusNotFound, err.Error())
		case errors.As(err, &notApproved):
			respondError(w, http.StatusConflict, err.Error())
		case errors.Is(err, contractmeta.ErrNoKey):
			respondError(w, http.StatusServiceUnavailable, dripSupplyKeyMissingMsg)
		default:
			respondError(w, http.StatusInternalServerError, "supply contract schedule: "+err.Error())
		}
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "supply contract approve commit: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"kind":            string(kind),
		"subject":         subject,
		"version":         version,
		"status":          string(dripsupply.StatusScheduled),
		"approved_by":     actor,
		"token_present":   tok.Issued(),
		"token_issued_at": tok.IssuedAt,
		"as_of":           time.Now().UTC(),
		"labels":          map[string]string{"version": dripLabelContracted},
		"note":            "scheduled: it becomes active at the next Denver midnight (ActivateScheduled), never immediately",
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /supply/manual-revenue
// ─────────────────────────────────────────────────────────────────────────────

type dripManualRevenueRequest struct {
	Lane             string  `json:"lane"`
	RevenueDate      string  `json:"revenue_date"`
	AttributionStart string  `json:"attribution_start"`
	AttributionEnd   string  `json:"attribution_end"`
	Amount           float64 `json:"amount"`
	Source           string  `json:"source"`
	Reference        string  `json:"reference"`
	EnteredBy        string  `json:"entered_by"`
	RevisionOf       string  `json:"revision_of"`
}

// HandleManualRevenue POST /api/mailing/supply/manual-revenue
//
// The audited entry for lanes whose revenue lives outside Everflow (§1.2).
// Validation lives in dripsupply.InsertManualRevenue so it holds for every
// caller; this handler only parses and reports.
func (s *DripSupplyService) HandleManualRevenue(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.org(w, r); !ok {
		return
	}
	raw, err := dripReadBody(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req dripManualRevenueRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	loc := dripSupplyDenverLoc()
	parseDay := func(field, v string) (time.Time, error) {
		v = strings.TrimSpace(v)
		if v == "" {
			return time.Time{}, fmt.Errorf("%s is required (YYYY-MM-DD)", field)
		}
		t, err := time.ParseInLocation("2006-01-02", v, loc)
		if err != nil {
			return time.Time{}, fmt.Errorf("%s must be YYYY-MM-DD, got %q", field, v)
		}
		return t, nil
	}
	entry := dripsupply.ManualRevenueEntry{
		Lane:      dripSubject(req.Lane),
		Amount:    req.Amount,
		Source:    strings.TrimSpace(req.Source),
		Reference: strings.TrimSpace(req.Reference),
		EnteredBy: actorFromRequest(r),
	}
	if strings.TrimSpace(req.EnteredBy) != "" && entry.EnteredBy == "unknown" {
		entry.EnteredBy = strings.TrimSpace(req.EnteredBy)
	}
	var fields []string
	if t, err := parseDay("revenue_date", req.RevenueDate); err != nil {
		fields = append(fields, "revenue_date")
	} else {
		entry.RevenueDate = t
	}
	if t, err := parseDay("attribution_start", req.AttributionStart); err != nil {
		fields = append(fields, "attribution_start")
	} else {
		entry.AttributionStart = t
	}
	if t, err := parseDay("attribution_end", req.AttributionEnd); err != nil {
		fields = append(fields, "attribution_end")
	} else {
		entry.AttributionEnd = t
	}
	if s := strings.TrimSpace(req.RevisionOf); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			fields = append(fields, "revision_of")
		} else {
			entry.RevisionOf = &id
		}
	}
	if len(fields) > 0 {
		respondJSON(w, http.StatusBadRequest, map[string]any{
			"error":  "manual revenue entry rejected: malformed date or id fields",
			"fields": fields,
		})
		return
	}

	ctx := r.Context()
	tx, err := s.writeTx(ctx)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "manual revenue: "+err.Error())
		return
	}
	defer func() { _ = tx.Rollback() }()

	id, err := dripsupply.InsertManualRevenue(ctx, tx, entry)
	if err != nil {
		var verrs dripsupply.ValidationErrors
		if errors.As(err, &verrs) {
			dripRespondValidation(w, verrs)
			return
		}
		respondError(w, http.StatusInternalServerError, "manual revenue: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "manual revenue commit: "+err.Error())
		return
	}
	respondJSON(w, http.StatusCreated, map[string]any{
		"id":         id.String(),
		"lane":       entry.Lane,
		"amount":     entry.Amount,
		"entered_by": entry.EnteredBy,
		"as_of":      time.Now().UTC(),
		"labels":     map[string]string{"amount": dripLabelActual},
	})
}
