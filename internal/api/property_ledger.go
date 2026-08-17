package api

// Property Ledger API (Vector A plan rev4, Steps 17–18) — the operator
// control surface for per-(sending domain × ISP) drip-introduction budgets.
//
// State semantics (internal/worker/property_ledger_doc.go):
//   - I-2  budget edits are NEXT-DAY: they stage in pending_budget /
//     pending_effective_day (tomorrow, Denver) plus a day-keyed
//     property_budget_versions row; the counter worker promotes at the
//     boundary. The live daily_budget is NEVER written here.
//   - I-3  holds are IMMEDIATE and interval-tracked ([held_from, held_to)).
//   - I-10 every version/interval/proposal writer locks the live ledger row
//     (SELECT … FOR UPDATE); CAS is the integer lock_version.
//   - Edits ARE approvals: budget edits stamp approved_by/approved_at.
//   - Any budget/hold change supersedes the cell's unresolved proposals in
//     the SAME tx.
//   - The list endpoint reads TODAY's introductions from
//     property_intro_counters ONLY — never live partner_clean_queue
//     aggregation (regex-pinned in property_ledger_test.go).
//
// Actor identity comes from the Step-13 middleware stamp (spoof-proof on
// session paths) via actorFromRequest. Routes are registered under
// /api/mailing/pmta-campaign/property-ledger* (handlers_pmta_campaign.go);
// the /api middleware already enforces session-or-admin-key — no per-route
// admin gate.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	isppkg "github.com/ignite/sparkpost-monitor/internal/pkg/isp"
	"github.com/ignite/sparkpost-monitor/internal/worker"
)

// propertyLedgerLoc is the operational-day timezone (I-1).
var propertyLedgerLoc = func() *time.Location {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		return time.UTC
	}
	return loc
}()

func propertyLedgerToday() string {
	return time.Now().In(propertyLedgerLoc).Format("2006-01-02")
}

func propertyLedgerTomorrow() string {
	return time.Now().In(propertyLedgerLoc).AddDate(0, 0, 1).Format("2006-01-02")
}

func propertyLedgerValidBrand(b string) bool {
	for _, code := range worker.DripIntroBrands() {
		if b == code {
			return true
		}
	}
	return false
}

func propertyLedgerValidISP(isp string) bool {
	for _, g := range isppkg.LedgerGroups() {
		if isp == g {
			return true
		}
	}
	return false
}

// ── Step 17: reads ──────────────────────────────────────────────────────────

// propertyLedgerListSQL — counters only (never partner_clean_queue), both
// counter-join sides normalized. $1 = today (Denver date).
const propertyLedgerListSQL = `
	SELECT b.brand, b.isp, b.daily_budget, b.hold,
	       COALESCE(b.notes, ''), COALESCE(b.updated_by, ''), b.updated_at,
	       COALESCE(b.approved_by, ''), b.approved_at,
	       b.min_budget, b.max_budget, b.lock_version,
	       b.pending_budget, b.pending_effective_day,
	       h.reason, h.held_from,
	       p.id::text, p.proposed_budget, p.base_budget, p.basis, p.created_at, p.expires_at,
	       c.introduced, c.updated_at
	FROM partner_drip_brand_budgets b
	LEFT JOIN property_hold_intervals h
	  ON lower(btrim(h.brand)) = lower(btrim(b.brand))
	 AND lower(btrim(h.isp))   = lower(btrim(b.isp))
	 AND h.held_to IS NULL
	LEFT JOIN property_budget_proposals p
	  ON lower(btrim(p.brand)) = lower(btrim(b.brand))
	 AND lower(btrim(p.isp))   = lower(btrim(b.isp))
	 AND p.resolved_at IS NULL
	LEFT JOIN property_intro_counters c
	  ON lower(btrim(c.brand)) = lower(btrim(b.brand))
	 AND lower(btrim(c.isp))   = lower(btrim(b.isp))
	 AND c.day = $1::date
	ORDER BY b.brand, b.isp`

type propertyLedgerProposal struct {
	ID             string    `json:"id"`
	ProposedBudget int       `json:"proposed_budget"`
	BaseBudget     int       `json:"base_budget"`
	Basis          string    `json:"basis"`
	CreatedAt      time.Time `json:"created_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

type propertyLedgerRow struct {
	Brand              string                  `json:"brand"`
	ISP                string                  `json:"isp"`
	SendingDomain      string                  `json:"sending_domain"`
	DailyBudget        int                     `json:"daily_budget"`
	Hold               bool                    `json:"hold"`
	HoldReason         string                  `json:"hold_reason,omitempty"`
	HeldSince          *time.Time              `json:"held_since,omitempty"`
	Notes              string                  `json:"notes,omitempty"`
	UpdatedBy          string                  `json:"updated_by,omitempty"`
	UpdatedAt          time.Time               `json:"updated_at"`
	ApprovedBy         string                  `json:"approved_by,omitempty"`
	ApprovedAt         *time.Time              `json:"approved_at,omitempty"`
	MinBudget          *int                    `json:"min_budget,omitempty"`
	MaxBudget          *int                    `json:"max_budget,omitempty"`
	LockVersion        int64                   `json:"lock_version"`
	PendingBudget      *int                    `json:"pending_budget,omitempty"`
	PendingEffectiveDay string                 `json:"pending_effective_day,omitempty"`
	Proposal           *propertyLedgerProposal `json:"proposal,omitempty"`
	IntroducedToday    *int                    `json:"introduced_today"`
	IntroducedAsOf     *time.Time              `json:"introduced_as_of,omitempty"`
}

type propertyLedgerRunInfo struct {
	Day        string     `json:"day"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// HandleListPropertyLedger GET /api/mailing/pmta-campaign/property-ledger
func (s *PMTACampaignService) HandleListPropertyLedger(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	today := propertyLedgerToday()

	rows, err := s.db.QueryContext(ctx, propertyLedgerListSQL, today)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "property-ledger query failed")
		return
	}
	defer rows.Close()

	out := []propertyLedgerRow{}
	for rows.Next() {
		var row propertyLedgerRow
		var notes, updatedBy, approvedBy string
		var approvedAt, heldFrom, propCreated, propExpires, introAsOf sql.NullTime
		var minB, maxB, pendB, propProposed, propBase, introduced sql.NullInt64
		var pendDay sql.NullTime
		var holdReason, propID, propBasis sql.NullString
		if err := rows.Scan(&row.Brand, &row.ISP, &row.DailyBudget, &row.Hold,
			&notes, &updatedBy, &row.UpdatedAt,
			&approvedBy, &approvedAt,
			&minB, &maxB, &row.LockVersion,
			&pendB, &pendDay,
			&holdReason, &heldFrom,
			&propID, &propProposed, &propBase, &propBasis, &propCreated, &propExpires,
			&introduced, &introAsOf); err != nil {
			respondError(w, http.StatusInternalServerError, "property-ledger scan failed")
			return
		}
		row.Notes, row.UpdatedBy, row.ApprovedBy = notes, updatedBy, approvedBy
		if approvedAt.Valid {
			row.ApprovedAt = &approvedAt.Time
		}
		if minB.Valid {
			v := int(minB.Int64)
			row.MinBudget = &v
		}
		if maxB.Valid {
			v := int(maxB.Int64)
			row.MaxBudget = &v
		}
		if pendB.Valid {
			v := int(pendB.Int64)
			row.PendingBudget = &v
		}
		if pendDay.Valid {
			row.PendingEffectiveDay = pendDay.Time.Format("2006-01-02")
		}
		if holdReason.Valid {
			row.HoldReason = holdReason.String
		}
		if heldFrom.Valid {
			row.HeldSince = &heldFrom.Time
		}
		if propID.Valid {
			row.Proposal = &propertyLedgerProposal{
				ID:             propID.String,
				ProposedBudget: int(propProposed.Int64),
				BaseBudget:     int(propBase.Int64),
				Basis:          propBasis.String,
				CreatedAt:      propCreated.Time,
				ExpiresAt:      propExpires.Time,
			}
		}
		if introduced.Valid {
			v := int(introduced.Int64)
			row.IntroducedToday = &v
		}
		if introAsOf.Valid {
			row.IntroducedAsOf = &introAsOf.Time
		}
		if d, ok := worker.BrandSendingDomain(row.Brand); ok {
			row.SendingDomain = d
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "property-ledger rows failed")
		return
	}

	// Global hold flag.
	var ghValue bool
	var ghReason string
	var ghSince time.Time
	var ghLockVersion int64
	_ = s.db.QueryRowContext(ctx, `
		SELECT value, COALESCE(reason, ''), updated_at, lock_version
		FROM property_ledger_flags WHERE flag = 'global_hold'`).
		Scan(&ghValue, &ghReason, &ghSince, &ghLockVersion)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"day":  today,
		"rows": out,
		"global_hold": map[string]interface{}{
			"value":        ghValue,
			"reason":       ghReason,
			"since":        ghSince,
			"lock_version": ghLockVersion,
		},
		"runs": map[string]interface{}{
			"counter":        s.latestRun(ctx, "property_counter_runs", false),
			"vdm":            s.latestRun(ctx, "ses_vdm_snapshot_runs", false),
			"reconciliation": s.latestRun(ctx, "property_reconciliation_runs", false),
		},
		"alerts_enabled": propertyAlertsEnabled(),
	})
}

func propertyAlertsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PROPERTY_RECON_ALERTS_ENABLED"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// latestRun returns the freshest run row of a run table (I-8 freshness strip).
// completedOnly narrows to status='completed'.
func (s *PMTACampaignService) latestRun(ctx context.Context, table string, completedOnly bool) *propertyLedgerRunInfo {
	// table is a compile-time constant at every call site — never user input.
	q := `SELECT day, status, started_at, finished_at FROM ` + table
	if completedOnly {
		q += ` WHERE status = 'completed'`
	}
	q += ` ORDER BY started_at DESC LIMIT 1`
	var info propertyLedgerRunInfo
	var day time.Time
	var finished sql.NullTime
	err := s.db.QueryRowContext(ctx, q).Scan(&day, &info.Status, &info.StartedAt, &finished)
	if err != nil {
		return nil
	}
	info.Day = day.Format("2006-01-02")
	if finished.Valid {
		info.FinishedAt = &finished.Time
	}
	return &info
}

// HandleListReconciliation GET …/property-ledger/reconciliation?day=YYYY-MM-DD
// Serves ONLY days whose reconciliation run is COMPLETED (I-8). Before P5
// ships (no completed runs exist) this returns the empty shape without
// touching property_reconciliation_daily.
func (s *PMTACampaignService) HandleListReconciliation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	day := strings.TrimSpace(r.URL.Query().Get("day"))
	if day == "" {
		day = propertyLedgerToday()
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		respondError(w, http.StatusBadRequest, "day must be YYYY-MM-DD")
		return
	}

	var runID string
	var startedAt time.Time
	var finishedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT run_id::text, started_at, finished_at
		FROM property_reconciliation_runs
		WHERE day = $1::date AND status = 'completed'
		ORDER BY finished_at DESC NULLS LAST LIMIT 1`, day).Scan(&runID, &startedAt, &finishedAt)
	if err == sql.ErrNoRows {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"day": day, "run": nil, "rows": []interface{}{},
			"note": "no completed reconciliation run for this day (reconciler ships in P5; days are served only from COMPLETED runs)",
		})
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "reconciliation run lookup failed")
		return
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT brand, isp, budget_version, intended, drip_dispatched, held_dispatched,
		       board_dispatched, budget_status, telemetry_status,
		       delivery_rate, complaint_rate, evidence, created_at
		FROM property_reconciliation_daily
		WHERE day = $1::date AND run_id = $2::uuid
		ORDER BY brand, isp`, day, runID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "reconciliation query failed")
		return
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var brand, isp, budgetStatus, telemetryStatus string
		var budgetVersion sql.NullInt64
		var intended, drip, held, board int
		var deliveryRate, complaintRate sql.NullFloat64
		var evidence json.RawMessage
		var createdAt time.Time
		if err := rows.Scan(&brand, &isp, &budgetVersion, &intended, &drip, &held, &board,
			&budgetStatus, &telemetryStatus, &deliveryRate, &complaintRate, &evidence, &createdAt); err != nil {
			respondError(w, http.StatusInternalServerError, "reconciliation scan failed")
			return
		}
		row := map[string]interface{}{
			"brand": brand, "isp": isp,
			"intended": intended, "drip_dispatched": drip,
			"held_dispatched": held, "board_dispatched": board,
			"budget_status": budgetStatus, "telemetry_status": telemetryStatus,
			"evidence": evidence, "created_at": createdAt,
		}
		if budgetVersion.Valid {
			row["budget_version"] = budgetVersion.Int64
		}
		if deliveryRate.Valid {
			row["delivery_rate"] = deliveryRate.Float64
		}
		if complaintRate.Valid {
			row["complaint_rate"] = complaintRate.Float64
		}
		out = append(out, row)
	}
	var fin interface{}
	if finishedAt.Valid {
		fin = finishedAt.Time
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"day": day, "rows": out,
		"run": map[string]interface{}{"run_id": runID, "status": "completed", "started_at": startedAt, "finished_at": fin},
	})
}

// HandleListCoverage GET …/property-ledger/coverage?day=YYYY-MM-DD
func (s *PMTACampaignService) HandleListCoverage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	day := strings.TrimSpace(r.URL.Query().Get("day"))
	if day == "" {
		day = propertyLedgerToday()
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		respondError(w, http.StatusBadRequest, "day must be YYYY-MM-DD")
		return
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT brand, isp, status, observed_sends
		FROM property_coverage_daily WHERE day = $1::date
		ORDER BY brand, isp`, day)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "coverage query failed")
		return
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	warnings := 0
	for rows.Next() {
		var brand, isp, status string
		var observed int
		if err := rows.Scan(&brand, &isp, &status, &observed); err != nil {
			respondError(w, http.StatusInternalServerError, "coverage scan failed")
			return
		}
		if status == "EXTRA_CELL" || strings.HasPrefix(status, "UNKNOWN_") {
			warnings++
		}
		out = append(out, map[string]interface{}{
			"brand": brand, "isp": isp, "status": status, "observed_sends": observed,
		})
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"day": day, "rows": out, "warning_cells": warnings,
	})
}

// ── Step 18: writes ─────────────────────────────────────────────────────────

type propertyLedgerUpdateReq struct {
	Brand       string  `json:"brand"`
	ISP         string  `json:"isp"`
	DailyBudget *int    `json:"daily_budget"`
	Hold        *bool   `json:"hold"`
	Notes       *string `json:"notes"`
	Reason      string  `json:"reason"`
	LockVersion *int64  `json:"lock_version"`
}

// propertyLedgerCeilingCheck enforces the transactional global ceiling inside
// the caller's tx (Step 18 / Step 27 rule): serialize on an advisory xact
// lock, then assert Σ(effective-tomorrow budgets) ≤ PROPERTY_LEDGER_TOTAL_MAX.
// Env unset/0 = any INCREASE is refused (never unlimited). Returns an HTTP
// status + message on refusal; (0, "") on pass.
func propertyLedgerCeilingCheck(ctx context.Context, tx *sql.Tx, brand, isp string, newBudget, currentEffectiveTomorrow int, tomorrow string) (int, string) {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('property_ledger_total'))`); err != nil {
		return http.StatusInternalServerError, "ceiling lock failed"
	}
	ceiling := 0
	if v := strings.TrimSpace(os.Getenv("PROPERTY_LEDGER_TOTAL_MAX")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			ceiling = n
		}
	}
	if ceiling <= 0 {
		if newBudget > currentEffectiveTomorrow {
			return http.StatusUnprocessableEntity, "PROPERTY_LEDGER_TOTAL_MAX ceiling unset — budget increases are disabled (decreases and holds still apply)"
		}
		return 0, ""
	}
	var othersTotal int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE
			WHEN pending_effective_day IS NOT NULL AND pending_effective_day <= $3::date THEN pending_budget
			ELSE daily_budget END), 0)
		FROM partner_drip_brand_budgets
		WHERE NOT (brand = $1 AND isp = $2)`, brand, isp, tomorrow).Scan(&othersTotal); err != nil {
		return http.StatusInternalServerError, "ceiling sum failed"
	}
	if othersTotal+newBudget > ceiling {
		return http.StatusUnprocessableEntity,
			fmt.Sprintf("global ceiling exceeded: effective-tomorrow total %d > PROPERTY_LEDGER_TOTAL_MAX %d", othersTotal+newBudget, ceiling)
	}
	return 0, ""
}

// stageBudgetEdit stages a next-day budget (I-2) + version row + approval
// stamps inside the caller's tx. The live ledger row MUST already be locked
// (I-10). Does NOT touch daily_budget.
func stageBudgetEdit(ctx context.Context, tx *sql.Tx, brand, isp string, budget int, tomorrow, actor, source string) error {
	sendingDomain, _ := worker.BrandSendingDomain(brand)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO property_budget_versions
			(brand, isp, version, budget, sending_domain, effective_day, approved_by, source)
		SELECT $1, $2, COALESCE(MAX(version), 0) + 1, $3, $4, $5::date, $6, $7
		FROM property_budget_versions WHERE brand = $1 AND isp = $2`,
		brand, isp, budget, sendingDomain, tomorrow, actor, source)
	return err
}

// supersedeProposals resolves every unresolved proposal for the cell (same-tx
// contract, Step 6).
func supersedeProposals(ctx context.Context, tx *sql.Tx, brand, isp string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE property_budget_proposals
		SET resolved_at = NOW(), resolution = 'superseded'
		WHERE brand = $1 AND isp = $2 AND resolved_at IS NULL`, brand, isp)
	return err
}

// HandleUpdatePropertyLedger POST …/property-ledger/update
func (s *PMTACampaignService) HandleUpdatePropertyLedger(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req propertyLedgerUpdateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	brand := strings.ToLower(strings.TrimSpace(req.Brand))
	isp := strings.ToLower(strings.TrimSpace(req.ISP))
	if !propertyLedgerValidBrand(brand) {
		respondError(w, http.StatusBadRequest, "unknown brand (must be one of the 16 drip roster codes)")
		return
	}
	if !propertyLedgerValidISP(isp) {
		respondError(w, http.StatusBadRequest, "unknown isp (must be in the ledger ISP authority)")
		return
	}
	if req.DailyBudget == nil && req.Hold == nil && req.Notes == nil {
		respondError(w, http.StatusBadRequest, "nothing to update: provide daily_budget, hold, or notes")
		return
	}
	if req.LockVersion == nil {
		respondError(w, http.StatusBadRequest, "lock_version required")
		return
	}
	if req.DailyBudget != nil && *req.DailyBudget < 0 {
		respondError(w, http.StatusBadRequest, "daily_budget must be >= 0")
		return
	}
	if req.Hold != nil && strings.TrimSpace(req.Reason) == "" {
		respondError(w, http.StatusBadRequest, "reason required when changing hold")
		return
	}
	actor := actorFromRequest(r)
	tomorrow := propertyLedgerTomorrow()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "tx begin failed")
		return
	}
	defer tx.Rollback()

	// I-10: lock the live row first.
	var cur struct {
		daily       int
		hold        bool
		lockVersion int64
		pendB       sql.NullInt64
		pendDay     sql.NullTime
		minB, maxB  sql.NullInt64
	}
	err = tx.QueryRowContext(ctx, `
		SELECT daily_budget, hold, lock_version, pending_budget, pending_effective_day, min_budget, max_budget
		FROM partner_drip_brand_budgets
		WHERE brand = $1 AND isp = $2 FOR UPDATE`, brand, isp).
		Scan(&cur.daily, &cur.hold, &cur.lockVersion, &cur.pendB, &cur.pendDay, &cur.minB, &cur.maxB)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "ledger cell not found (seed pending?)")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "ledger row lock failed")
		return
	}
	if cur.lockVersion != *req.LockVersion {
		respondJSON(w, http.StatusConflict, map[string]interface{}{
			"error":        "lock_version conflict — the row changed since you loaded it",
			"lock_version": cur.lockVersion,
		})
		return
	}

	setFragments := []string{}
	args := []interface{}{}
	arg := func(v interface{}) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	if req.DailyBudget != nil {
		v := *req.DailyBudget
		if cur.minB.Valid && int64(v) < cur.minB.Int64 {
			respondError(w, http.StatusUnprocessableEntity, fmt.Sprintf("daily_budget %d below min_budget %d", v, cur.minB.Int64))
			return
		}
		if cur.maxB.Valid && int64(v) > cur.maxB.Int64 {
			respondError(w, http.StatusUnprocessableEntity, fmt.Sprintf("daily_budget %d above max_budget %d", v, cur.maxB.Int64))
			return
		}
		curEffTomorrow := cur.daily
		if cur.pendB.Valid {
			curEffTomorrow = int(cur.pendB.Int64)
		}
		if status, msg := propertyLedgerCeilingCheck(ctx, tx, brand, isp, v, curEffTomorrow, tomorrow); status != 0 {
			respondError(w, status, msg)
			return
		}
		if err := stageBudgetEdit(ctx, tx, brand, isp, v, tomorrow, actor, "portal-edit"); err != nil {
			respondError(w, http.StatusInternalServerError, "version write failed")
			return
		}
		setFragments = append(setFragments,
			"pending_budget = "+arg(v),
			"pending_effective_day = "+arg(tomorrow)+"::date",
			"approved_by = "+arg(actor),
			"approved_at = NOW()")
	}

	if req.Hold != nil {
		h := *req.Hold
		reason := strings.TrimSpace(req.Reason)
		if h {
			// Open an interval iff none is open (uq_phi_one_open).
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO property_hold_intervals (brand, isp, held_from, reason, changed_by)
				SELECT $1, $2, NOW(), $3, $4
				WHERE NOT EXISTS (
					SELECT 1 FROM property_hold_intervals
					WHERE brand = $1 AND isp = $2 AND held_to IS NULL)`,
				brand, isp, reason, actor); err != nil {
				respondError(w, http.StatusInternalServerError, "hold interval open failed")
				return
			}
		} else {
			if _, err := tx.ExecContext(ctx, `
				UPDATE property_hold_intervals SET held_to = NOW()
				WHERE brand = $1 AND isp = $2 AND held_to IS NULL`, brand, isp); err != nil {
				respondError(w, http.StatusInternalServerError, "hold interval close failed")
				return
			}
		}
		setFragments = append(setFragments, "hold = "+arg(h))
	}

	if req.Notes != nil {
		setFragments = append(setFragments, "notes = "+arg(*req.Notes))
	}

	// Any budget/hold change supersedes the cell's unresolved proposals.
	if req.DailyBudget != nil || req.Hold != nil {
		if err := supersedeProposals(ctx, tx, brand, isp); err != nil {
			respondError(w, http.StatusInternalServerError, "proposal supersede failed")
			return
		}
	}

	setFragments = append(setFragments,
		"updated_by = "+arg(actor),
		"updated_at = NOW()",
		"lock_version = lock_version + 1")
	q := "UPDATE partner_drip_brand_budgets SET " + strings.Join(setFragments, ", ") +
		" WHERE brand = " + arg(brand) + " AND isp = " + arg(isp) +
		" AND lock_version = " + arg(*req.LockVersion)
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "ledger update failed")
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		respondError(w, http.StatusConflict, "lock_version conflict during update")
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "commit failed")
		return
	}

	after := map[string]interface{}{"lock_version": cur.lockVersion + 1}
	if req.DailyBudget != nil {
		after["pending_budget"] = *req.DailyBudget
		after["pending_effective_day"] = tomorrow
	}
	if req.Hold != nil {
		after["hold"] = *req.Hold
		after["reason"] = req.Reason
	}
	if req.Notes != nil {
		after["notes"] = *req.Notes
	}
	writeAuditLog(ctx, s.db, actor, "property_ledger_update", "property_ledger_cell",
		brand+"/"+isp,
		map[string]interface{}{"daily_budget": cur.daily, "hold": cur.hold, "lock_version": cur.lockVersion},
		after)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok":           true,
		"brand":        brand,
		"isp":          isp,
		"lock_version": cur.lockVersion + 1,
		"pending_note": "Budget edits apply from tomorrow (Denver). Hold is immediate.",
	})
}

// approveOneProposal applies one proposal in its own tx. Returns HTTP status +
// payload for the per-id report.
func (s *PMTACampaignService) approveOneProposal(ctx context.Context, proposalID, actor string) (int, map[string]interface{}) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return http.StatusInternalServerError, map[string]interface{}{"error": "tx begin failed"}
	}
	defer tx.Rollback()

	var brand, isp string
	var proposed int
	var baseLockVersion int64
	var expiresAt time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT brand, isp, proposed_budget, base_lock_version, expires_at
		FROM property_budget_proposals
		WHERE id = $1::uuid AND resolved_at IS NULL
		FOR UPDATE`, proposalID).Scan(&brand, &isp, &proposed, &baseLockVersion, &expiresAt)
	if err == sql.ErrNoRows {
		return http.StatusNotFound, map[string]interface{}{"error": "proposal not found or already resolved"}
	}
	if err != nil {
		return http.StatusInternalServerError, map[string]interface{}{"error": "proposal lock failed"}
	}
	if !expiresAt.After(time.Now()) {
		if _, err := tx.ExecContext(ctx, `
			UPDATE property_budget_proposals SET resolved_at = NOW(), resolution = 'expired'
			WHERE id = $1::uuid`, proposalID); err == nil {
			_ = tx.Commit()
		}
		return http.StatusConflict, map[string]interface{}{"error": "proposal expired"}
	}

	// I-10: lock the ledger row.
	var cur struct {
		daily       int
		lockVersion int64
		pendB       sql.NullInt64
		minB, maxB  sql.NullInt64
	}
	err = tx.QueryRowContext(ctx, `
		SELECT daily_budget, lock_version, pending_budget, min_budget, max_budget
		FROM partner_drip_brand_budgets
		WHERE brand = $1 AND isp = $2 FOR UPDATE`, brand, isp).
		Scan(&cur.daily, &cur.lockVersion, &cur.pendB, &cur.minB, &cur.maxB)
	if err == sql.ErrNoRows {
		return http.StatusNotFound, map[string]interface{}{"error": "ledger cell not found"}
	}
	if err != nil {
		return http.StatusInternalServerError, map[string]interface{}{"error": "ledger row lock failed"}
	}

	// Base-version staleness (Step 6): mismatch → supersede + 409.
	if cur.lockVersion != baseLockVersion {
		if _, err := tx.ExecContext(ctx, `
			UPDATE property_budget_proposals SET resolved_at = NOW(), resolution = 'superseded'
			WHERE id = $1::uuid`, proposalID); err == nil {
			_ = tx.Commit()
		}
		return http.StatusConflict, map[string]interface{}{
			"error":        "stale proposal — the ledger changed since it was proposed (superseded)",
			"lock_version": cur.lockVersion,
		}
	}

	if cur.minB.Valid && int64(proposed) < cur.minB.Int64 {
		return http.StatusUnprocessableEntity, map[string]interface{}{"error": fmt.Sprintf("proposed %d below min_budget %d", proposed, cur.minB.Int64)}
	}
	if cur.maxB.Valid && int64(proposed) > cur.maxB.Int64 {
		return http.StatusUnprocessableEntity, map[string]interface{}{"error": fmt.Sprintf("proposed %d above max_budget %d", proposed, cur.maxB.Int64)}
	}

	tomorrow := propertyLedgerTomorrow()
	curEffTomorrow := cur.daily
	if cur.pendB.Valid {
		curEffTomorrow = int(cur.pendB.Int64)
	}
	if status, msg := propertyLedgerCeilingCheck(ctx, tx, brand, isp, proposed, curEffTomorrow, tomorrow); status != 0 {
		return status, map[string]interface{}{"error": msg}
	}

	if err := stageBudgetEdit(ctx, tx, brand, isp, proposed, tomorrow, actor, "proposal-approve"); err != nil {
		return http.StatusInternalServerError, map[string]interface{}{"error": "version write failed"}
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE partner_drip_brand_budgets
		SET pending_budget = $3, pending_effective_day = $4::date,
		    approved_by = $5, approved_at = NOW(),
		    updated_by = $5, updated_at = NOW(),
		    lock_version = lock_version + 1
		WHERE brand = $1 AND isp = $2 AND lock_version = $6`,
		brand, isp, proposed, tomorrow, actor, baseLockVersion)
	if err != nil {
		return http.StatusInternalServerError, map[string]interface{}{"error": "ledger update failed"}
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return http.StatusConflict, map[string]interface{}{"error": "lock_version conflict during approve"}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE property_budget_proposals SET resolved_at = NOW(), resolution = 'approved'
		WHERE id = $1::uuid`, proposalID); err != nil {
		return http.StatusInternalServerError, map[string]interface{}{"error": "proposal resolve failed"}
	}
	if err := tx.Commit(); err != nil {
		return http.StatusInternalServerError, map[string]interface{}{"error": "commit failed"}
	}

	writeAuditLog(ctx, s.db, actor, "property_ledger_approve_proposal", "property_budget_proposal", proposalID,
		map[string]interface{}{"daily_budget": cur.daily, "lock_version": cur.lockVersion},
		map[string]interface{}{"pending_budget": proposed, "pending_effective_day": tomorrow})

	return http.StatusOK, map[string]interface{}{
		"ok": true, "brand": brand, "isp": isp,
		"pending_budget": proposed, "pending_effective_day": tomorrow,
		"lock_version": cur.lockVersion + 1,
	}
}

// HandleApproveProposal POST …/property-ledger/approve-proposal
func (s *PMTACampaignService) HandleApproveProposal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProposalID string `json:"proposal_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ProposalID) == "" {
		respondError(w, http.StatusBadRequest, "proposal_id required")
		return
	}
	status, payload := s.approveOneProposal(r.Context(), strings.TrimSpace(req.ProposalID), actorFromRequest(r))
	respondJSON(w, status, payload)
}

// HandleApproveProposals POST …/property-ledger/approve-proposals (bulk):
// per-proposal txs, per-id report.
func (s *PMTACampaignService) HandleApproveProposals(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProposalIDs []string `json:"proposal_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.ProposalIDs) == 0 {
		respondError(w, http.StatusBadRequest, "proposal_ids required")
		return
	}
	actor := actorFromRequest(r)
	results := make([]map[string]interface{}, 0, len(req.ProposalIDs))
	for _, id := range req.ProposalIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		status, payload := s.approveOneProposal(r.Context(), id, actor)
		payload["proposal_id"] = id
		payload["status_code"] = status
		results = append(results, payload)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"results": results})
}

// HandleGlobalHold POST …/property-ledger/global-hold — flag CAS + flag
// version interval row + audit (I-3/I-5).
func (s *PMTACampaignService) HandleGlobalHold(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		Value       *bool  `json:"value"`
		Reason      string `json:"reason"`
		LockVersion *int64 `json:"lock_version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Value == nil {
		respondError(w, http.StatusBadRequest, "value required")
		return
	}
	if req.LockVersion == nil {
		respondError(w, http.StatusBadRequest, "lock_version required")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		respondError(w, http.StatusBadRequest, "reason required")
		return
	}
	actor := actorFromRequest(r)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "tx begin failed")
		return
	}
	defer tx.Rollback()

	var curValue bool
	var curLock int64
	err = tx.QueryRowContext(ctx, `
		SELECT value, lock_version FROM property_ledger_flags
		WHERE flag = 'global_hold' FOR UPDATE`).Scan(&curValue, &curLock)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "global_hold flag read failed (seed missing?)")
		return
	}
	if curLock != *req.LockVersion {
		respondJSON(w, http.StatusConflict, map[string]interface{}{
			"error":        "lock_version conflict — the flag changed since you loaded it",
			"lock_version": curLock,
		})
		return
	}
	if curValue == *req.Value {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"ok": true, "unchanged": true, "value": curValue, "lock_version": curLock,
		})
		return
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE property_ledger_flags
		SET value = $1, reason = $2, updated_by = $3, updated_at = NOW(),
		    lock_version = lock_version + 1
		WHERE flag = 'global_hold' AND lock_version = $4`,
		*req.Value, req.Reason, actor, curLock); err != nil {
		respondError(w, http.StatusInternalServerError, "flag update failed")
		return
	}
	// Interval history (I-4): close the open version, open the new one.
	if _, err := tx.ExecContext(ctx, `
		UPDATE property_ledger_flag_versions SET effective_to = NOW()
		WHERE flag = 'global_hold' AND effective_to IS NULL`); err != nil {
		respondError(w, http.StatusInternalServerError, "flag version close failed")
		return
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO property_ledger_flag_versions
			(flag, version, value, reason, changed_by, effective_from)
		SELECT 'global_hold', COALESCE(MAX(version), 0) + 1, $1, $2, $3, NOW()
		FROM property_ledger_flag_versions WHERE flag = 'global_hold'`,
		*req.Value, req.Reason, actor); err != nil {
		respondError(w, http.StatusInternalServerError, "flag version write failed")
		return
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "commit failed")
		return
	}

	writeAuditLog(ctx, s.db, actor, "property_ledger_global_hold", "property_ledger_flag", "global_hold",
		map[string]interface{}{"value": curValue, "lock_version": curLock},
		map[string]interface{}{"value": *req.Value, "reason": req.Reason})

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true, "value": *req.Value, "lock_version": curLock + 1,
		"note": "Global hold propagates within one orchestrator tick (the enforcement cache reload cadence).",
	})
}
