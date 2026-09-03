package dripsupply

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// -----------------------------------------------------------------------------
// Transitions — the single partner_clean_queue state-change path (REQ-118 §2.4)
// -----------------------------------------------------------------------------
//
// Non-negotiable 1 of the design: "Nothing moves partner_clean_queue.status to
// 'claimed' without a capacity_allocation_id (DB constraint)." The database
// half of that is `pcq_claim_requires_allocation` (WP1, cmd/server/main.go
// criticalSendPathDDL). This file is the Go half: every claim in the new chain
// goes through Claim or ClaimByISPCaps, both of which REFUSE a zero allocation
// id before they touch the database, so the constraint is the backstop and not
// the first line of defence.
//
// Nothing here starts a transaction. Claim / ClaimByISPCaps / Release take the
// caller's handle so a claim and the ledger write that authorised it can share
// ONE transaction: if the executor dies between them, neither happened. Reap is
// the exception — it is a janitor over rows nobody holds, so it runs as its own
// statement.
//
// IMPORT DIRECTION: internal/worker imports THIS package (WP5 injects the
// service into PartnerDripOrchestrator), so nothing here may import
// internal/worker. The SQL fragments ported from the orchestrator are therefore
// DUPLICATED below with their source line, and transition_parity_test.go reads
// the orchestrator source off disk and fails when a copy drifts.

// Transitions is stateless: every call carries its own database handle, so one
// value is safe for any number of concurrent ticks.
type Transitions struct{}

// NewTransitions returns the transition service.
func NewTransitions() *Transitions { return &Transitions{} }

// ErrNoAllocation is returned by Claim and ClaimByISPCaps when the caller
// passes uuid.Nil. This is a REFUSAL, not a fallback: a claim with no
// allocation is exactly the invariant violation REQ-118 exists to end, and the
// caller has skipped Reserve if it gets here. No statement is executed.
var ErrNoAllocation = errors.New("dripsupply: claim refused: capacity_allocation_id is required (REQ-118 §2.4 non-negotiable 1)")

// ErrNoPositiveGrant is returned by ClaimByISPCaps when every per-ISP grant is
// zero. Reserve granting nothing is a normal outcome (governor at 0, lane
// filled, supply empty) and the caller must write a `zero` tick outcome rather
// than issue a claim that can only return no rows.
var ErrNoPositiveGrant = errors.New("dripsupply: claim refused: no positive per-ISP grant")

// -----------------------------------------------------------------------------
// The constraint this package is the Go half of
// -----------------------------------------------------------------------------

// PCQAllocationFence is a VERBATIM copy of the `pcqAllocationFence` constant in
// cmd/server/main.go:2453 (WP1). It ships FAR in the future on purpose: the
// CHECK is NOT VALID, which still applies to every new and updated row, so a
// fence in the past would reject the legacy claim path the moment the binary
// boots with DRIP_SUPPLY_CHAIN_MODE=off. The §7 step-3 canary deploy moves the
// main.go constant to the cutover day's Denver midnight in UTC.
//
// It is restated here so the integration tests can build the PRODUCTION
// constraint and then rewrite the literal to a PAST date in the test schema
// only — which is the only way to exercise enforcement without shipping a
// fence that would break production. transition_parity_test.go pins this copy
// against main.go.
const PCQAllocationFence = "2099-01-01 00:00:00+00"

// PCQClaimConstraintDDL renders the `pcq_claim_requires_allocation` CHECK for a
// given fence literal. The expression is byte-identical to WP1's
// criticalSendPathDDL entry `req118_pcq_claim_requires_allocation`
// (cmd/server/main.go:2433-2442); only the DO-guard wrapper (which exists to
// make the production ADD idempotent and re-issuable) is omitted, because a
// scratch schema creates the table once.
//
// Pass PCQAllocationFence for the shipped shape. Tests pass a past date.
func PCQClaimConstraintDDL(fence string) string {
	return `ALTER TABLE partner_clean_queue ADD CONSTRAINT pcq_claim_requires_allocation
				CHECK (status <> 'claimed' OR capacity_allocation_id IS NOT NULL OR claimed_at < '` + fence + `') NOT VALID`
}

// -----------------------------------------------------------------------------
// Ported SQL fragments (duplicated — see IMPORT DIRECTION above)
// -----------------------------------------------------------------------------

// datasetNotEmergencyPausedSQL is a VERBATIM copy of the identically named
// constant at internal/worker/partner_drip_orchestrator.go:2194.
//
// It excludes queue rows whose dataset the operator has emergency-stopped
// (REQ-004). Deliberately a correlated NOT EXISTS rather than a JOIN:
// partner_datasets never appears in the claim CTEs' FROM clause, so the
// FOR UPDATE SKIP LOCKED row locks stay scoped to partner_clean_queue only.
const datasetNotEmergencyPausedSQL = `
			  AND NOT EXISTS (
			      SELECT 1 FROM partner_datasets d
			      WHERE d.id = partner_clean_queue.dataset_id
			        AND d.paused_emergency
			  )`

// convertersPrefix is a VERBATIM copy of
// internal/worker/partner_drip_converters.go:33.
const convertersPrefix = "converters_"

// convertersPinDisabled is a VERBATIM copy of
// internal/worker/partner_drip_converters.go:63. Read at call time so the
// operator's kill switch does not need a restart.
func convertersPinDisabled() bool {
	v := os.Getenv("PARTNER_DRIP_CONVERTERS_PIN_DISABLED")
	return v == "1" || v == "true"
}

// quoteSQLLiteral is a VERBATIM copy of
// internal/worker/partner_drip_engagement_gate.go:307.
func quoteSQLLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// homeBrandPinSQL is a VERBATIM copy of
// internal/worker/partner_drip_converters.go:71.
//
// It returns the claim predicate binding pinned records to their converting
// domain, or "" for non-converter verticals (byte-identical legacy SQL).
// `alias` qualifies the column; brand is inlined as a quoted literal
// (orchestrator brand codes, never user input).
func homeBrandPinSQL(vertical, brand, alias string) string {
	lv := strings.ToLower(strings.TrimSpace(vertical))
	if convertersPinDisabled() || !strings.HasPrefix(lv, convertersPrefix) {
		return ""
	}
	q := ""
	if alias != "" {
		q = alias + "."
	}
	b := quoteSQLLiteral(strings.ToLower(strings.TrimSpace(brand)))
	return "\n\t\t\t  AND (COALESCE(" + q + "extra_metadata->>'home_brand','') = '' OR lower(" + q + "extra_metadata->>'home_brand') = " + b + ")"
}

// capsValuesClauses is a VERBATIM port of
// internal/worker/partner_drip_orchestrator.go:2940, with `startIdx` kept as a
// parameter because ClaimByISPCaps binds the allocation id at $3 and so starts
// the caps pairs one placeholder later than the orchestrator's $3.
//
// A cap of 0 STAYS in the VALUES list. That is load-bearing: the bucket CASE
// asks whether the ISP appears in `caps` at all, so a zero-grant ISP buckets to
// ITSELF and is then excluded by `rn <= 0` — it does NOT spill into 'other'.
// Dropping zero entries here would re-open the 2026-08-27 'other'-bucket leak,
// where records for a lane that was deliberately given nothing were claimed
// under another ISP's allowance.
func capsValuesClauses(perISPCaps map[string]int, startIdx int) ([]string, []interface{}, int) {
	clauses := make([]string, 0, len(perISPCaps))
	args := make([]interface{}, 0, 2*len(perISPCaps))
	positive := 0
	idx := startIdx
	for ispName, capValue := range perISPCaps {
		if capValue < 0 {
			capValue = 0
		}
		if capValue > 0 {
			positive++
		}
		clauses = append(clauses, fmt.Sprintf("($%d::text, $%d::int)", idx, idx+1))
		args = append(args, ispName, capValue)
		idx += 2
	}
	return clauses, args, positive
}

// -----------------------------------------------------------------------------
// ClaimedRecord
// -----------------------------------------------------------------------------

// ClaimedRecord is the exported mirror of worker.claimedRecord
// (internal/worker/partner_drip_orchestrator.go:2171). The ids stay TEXT, as
// they are in the orchestrator's scan, so WP5's adapter is a field copy and a
// malformed uuid in one row cannot fail the whole wave's scan.
type ClaimedRecord struct {
	ID        string
	Email     string
	EmailMD5  string
	ISPFamily string
	DatasetID string
	PartnerID string
	BatchID   string
	Extra     []byte
}

// -----------------------------------------------------------------------------
// Claim
// -----------------------------------------------------------------------------

// claimByIDsSQL is §2.4's `ready -> claimed`. `AND status = 'ready'` is the
// idempotency guard: a re-fired tick, a retry, or the second of two orchestrator
// instances re-issuing the same id list claims 0 rows the second time instead of
// re-stamping claimed_at and detaching the row from the allocation that already
// owns it. The returned count is therefore how many rows THIS call moved, which
// is what the caller must reconcile against its grant.
const claimByIDsSQL = `
		UPDATE partner_clean_queue
		SET status = 'claimed', claimed_at = NOW(), capacity_allocation_id = $2
		WHERE id = ANY($1::uuid[]) AND status = 'ready'`

// Claim moves the given rows from 'ready' to 'claimed' and stamps the
// allocation that paid for them. It returns the number of rows actually moved.
//
// Refuses with ErrNoAllocation, WITHOUT executing anything, when allocationID
// is the zero uuid.
func (t *Transitions) Claim(ctx context.Context, tx Queryer, ids []uuid.UUID, allocationID uuid.UUID) (int, error) {
	if allocationID == uuid.Nil {
		return 0, ErrNoAllocation
	}
	if len(ids) == 0 {
		return 0, nil
	}
	strIDs := make([]string, len(ids))
	for i, id := range ids {
		strIDs[i] = id.String()
	}
	res, err := tx.ExecContext(ctx, claimByIDsSQL, pq.Array(strIDs), allocationID.String())
	if err != nil {
		return 0, fmt.Errorf("dripsupply: claim %d rows: %w", len(ids), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return int(n), nil
}

// ClaimByISPCaps is the port of
// (*PartnerDripOrchestrator).claimRecordsByISPCaps
// (internal/worker/partner_drip_orchestrator.go:2959-3051), with two changes and
// nothing else:
//
//  1. the caps CTE is fed by `grants` — the per-ISP RESERVATION grants — instead
//     of resolvePerISPCaps' typed cap stack (§2.7);
//  2. the UPDATE stamps capacity_allocation_id.
//
// Everything the original guaranteed is preserved deliberately:
//
//   - the 'other' bucket fallback, so a record whose isp_family is outside the
//     12 canonical classes (protonmail, and whatever the next one is) is
//     claimable under 'other' instead of sitting in 'ready' forever;
//   - a grant of 0 excludes that ISP even so — see capsValuesClauses;
//   - oldest-ingest-first within each ISP (ROW_NUMBER … ORDER BY ingested_at),
//     and again in `eligible`, so the hardCap trims the newest;
//   - datasetNotEmergencyPausedSQL on BOTH the ranking scan and the re-check
//     inside `picked`, so an emergency stop that lands mid-statement still wins;
//   - homeBrandPinSQL for converter verticals;
//   - FOR UPDATE SKIP LOCKED inside `picked`, so two instances racing the same
//     ISP queue take disjoint rows rather than blocking.
//
// The caller supplies the transaction (and its statement timeout). Prod runs a
// 30 s statement_timeout on worker queries; this statement is bounded by
// hardCap, not by the 13.5M-row table, and the orchestrator has run this plan
// shape in production since REQ-004.
func (t *Transitions) ClaimByISPCaps(
	ctx context.Context,
	tx Queryer,
	vertical, forBrand string,
	grants map[string]int,
	hardCap int,
	allocationID uuid.UUID,
) ([]ClaimedRecord, error) {
	if allocationID == uuid.Nil {
		return nil, ErrNoAllocation
	}
	if len(grants) == 0 {
		return nil, fmt.Errorf("dripsupply: claim by isp caps: grants is empty")
	}
	if hardCap <= 0 {
		return nil, fmt.Errorf("dripsupply: claim by isp caps: hardCap must be > 0")
	}

	pin := homeBrandPinSQL(vertical, forBrand, "")

	// $1 = vertical, $2 = hardCap, $3 = allocation id, then $4.. = isp, cap, …
	args := []interface{}{vertical, hardCap, allocationID.String()}
	valueClauses, capArgs, positive := capsValuesClauses(grants, 4)
	args = append(args, capArgs...)
	if positive == 0 {
		return nil, ErrNoPositiveGrant
	}

	query := fmt.Sprintf(`
		WITH caps(isp, cap) AS (
			VALUES %s
		),
		ranked AS (
			SELECT id, isp_family, ingested_at,
			       ROW_NUMBER() OVER (
			           PARTITION BY CASE
			               WHEN COALESCE(NULLIF(isp_family, ''), 'other') IN (SELECT isp FROM caps)
			               THEN COALESCE(NULLIF(isp_family, ''), 'other') ELSE 'other' END
			           ORDER BY ingested_at ASC
			       ) AS rn,
			       CASE
			           WHEN COALESCE(NULLIF(isp_family, ''), 'other') IN (SELECT isp FROM caps)
			           THEN COALESCE(NULLIF(isp_family, ''), 'other') ELSE 'other' END AS isp_bucket
			FROM partner_clean_queue
			WHERE status = 'ready' AND vertical = $1`+datasetNotEmergencyPausedSQL+pin+`
		),
		eligible AS (
			SELECT r.id
			FROM ranked r
			JOIN caps c ON c.isp = r.isp_bucket
			WHERE r.rn <= c.cap
			ORDER BY r.ingested_at ASC
			LIMIT $2
		),
		picked AS (
			SELECT id FROM partner_clean_queue
			WHERE id IN (SELECT id FROM eligible)
			  AND status = 'ready'`+datasetNotEmergencyPausedSQL+pin+`
			FOR UPDATE SKIP LOCKED
		)
		UPDATE partner_clean_queue q
		SET status = 'claimed', claimed_at = NOW(), capacity_allocation_id = $3
		FROM picked
		WHERE q.id = picked.id
		RETURNING q.id, q.email, q.email_md5, q.isp_family, q.dataset_id, q.partner_id, q.batch_id, q.extra_metadata
	`, strings.Join(valueClauses, ", "))

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("dripsupply: claim by isp caps (%s): %w", vertical, err)
	}
	defer rows.Close()

	out := make([]ClaimedRecord, 0, hardCap)
	for rows.Next() {
		var r ClaimedRecord
		if err := rows.Scan(&r.ID, &r.Email, &r.EmailMD5, &r.ISPFamily, &r.DatasetID, &r.PartnerID, &r.BatchID, &r.Extra); err != nil {
			// Same as the orchestrator: one unscannable row does not fail the
			// wave. The row stays claimed and the reaper (below) returns it.
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dripsupply: claim by isp caps (%s): iterate: %w", vertical, err)
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// Release
// -----------------------------------------------------------------------------

// releaseClaimSQL is a VERBATIM copy of the UPDATE in
// (*PartnerDripOrchestrator).releaseClaim
// (internal/worker/partner_drip_orchestrator.go:3190-3215), which flips claimed
// records back so the next tick can retry them after a deploy / promote /
// segment-creation failure.
//
// The CASE is the part that must not be "simplified": a released FOLLOW-UP
// claim (touch_count >= 1, already t1-mailed) goes back to 'mailed', which is
// the pool the t2..tN pass claims from. Releasing it to 'ready' would eject it
// from the reminder ladder AND expose it to the first-touch pass, re-sending t1
// to someone mid-sequence (2026-08-05: 30k records ejected, WCL follow-ups
// collapsed 9,338 -> 122/day while first-touches inflated with duplicates).
const releaseClaimSQL = `
		UPDATE partner_clean_queue
		SET status = CASE
		        WHEN COALESCE(touch_count, 0) >= 1 AND mailed_campaign_id IS NOT NULL
		        THEN 'mailed'
		        ELSE 'ready'
		    END,
		    claimed_at = NULL
		WHERE id = ANY($1::uuid[])`

// Release returns claimed rows to their pre-claim pool, unchanged from
// releaseClaim's semantics.
//
// `reason` is NOT written to partner_clean_queue. That is deliberate: the table
// has no release-reason column, adding one would change the ported semantics,
// and the reason already has two homes that the operator reads —
// drip_capacity_ledger.release_reason (Service.Release, §2.2) and
// drip_tick_outcomes.reason (WP5, §2.7). It is carried here so this call and
// those rows cannot disagree, and so the error names why the release was
// attempted when the statement itself fails.
//
// capacity_allocation_id is left in place: the row keeps the fingerprint of the
// allocation that claimed it, which is what makes "committed vs mailed"
// reconcilable (§8.3) after a release. Nothing reads it for eligibility — the
// claim predicates key on status.
func (t *Transitions) Release(ctx context.Context, tx Queryer, ids []uuid.UUID, reason string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	strIDs := make([]string, len(ids))
	for i, id := range ids {
		strIDs[i] = id.String()
	}
	res, err := tx.ExecContext(ctx, releaseClaimSQL, pq.Array(strIDs))
	if err != nil {
		return 0, fmt.Errorf("dripsupply: release %d rows (%s): %w", len(ids), reason, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return int(n), nil
}

// -----------------------------------------------------------------------------
// Reap
// -----------------------------------------------------------------------------

// DefaultReapAge is §2.4's 48 h orphan cutoff.
const DefaultReapAge = 48 * time.Hour

// DefaultReapBatch mirrors worker.claimSweepBatchSize
// (internal/worker/partner_drip_orchestrator.go:3078) so the two janitors put
// the same bound on one statement's lock footprint.
const DefaultReapBatch = 5000

// maxReapBatch is the hard ceiling on one call, whatever the caller asks for.
// The reaper competes with live claim traffic on the 13.5M-row queue under a
// 30 s prod statement_timeout; an unbounded LIMIT is how a janitor becomes an
// outage.
const maxReapBatch = 50000

// reapOrphanClaimsSQL is the §2.4 reaper.
//
// It covers the orphan shape releaseStaleClaims
// (internal/worker/partner_drip_orchestrator.go:3066-3081) CANNOT: that
// statement requires `subscriber_id IS NULL`, so a row that got as far as
// having a subscriber hydrated — which is most of the way through staging, and
// exactly where a mid-finalization ECS bounce strands it — is invisible to it
// and sits 'claimed' forever. The predicate here keys on the two columns that
// actually prove mail was attempted:
//
//	last_touch_campaign_id IS NULL  -- never entered a wave
//	mailed_campaign_id     IS NULL  -- never mailed
//
// which also means a legitimately mid-ladder or mailed row can never be swept
// back to 'ready' and re-sent t1 (the 2026-08-05 failure mode).
//
// The cutoff is computed IN POSTGRES (NOW() - make_interval), never from the Go
// clock: two orchestrator instances with clock skew must not be able to reap
// each other's live claims.
//
// $1 = age in seconds, $2 = batch size.
const reapOrphanClaimsSQL = `
		UPDATE partner_clean_queue
		SET status = 'ready',
		    claimed_at = NULL
		WHERE id IN (
		    SELECT id
		    FROM partner_clean_queue
		    WHERE status = 'claimed'
		      AND claimed_at IS NOT NULL
		      AND claimed_at < NOW() - make_interval(secs => $1::double precision)
		      AND last_touch_campaign_id IS NULL
		      AND mailed_campaign_id IS NULL
		    LIMIT $2
		    FOR UPDATE SKIP LOCKED
		)`

// Reap returns orphaned 'claimed' rows to 'ready' and reports how many it
// moved. ONE statement, at most `batch` rows — it does not loop. The caller
// ticks it, so a large backlog drains over several ticks instead of one
// long-running statement holding row locks against live claims.
//
// olderThan <= 0 is an error, never a default: a zero cutoff would reap the
// claims the current tick is still working with.
func (t *Transitions) Reap(ctx context.Context, db Queryer, olderThan time.Duration, batch int) (int, error) {
	if olderThan <= 0 {
		return 0, fmt.Errorf("dripsupply: reap: olderThan must be > 0, got %s", olderThan)
	}
	if batch <= 0 {
		batch = DefaultReapBatch
	}
	if batch > maxReapBatch {
		batch = maxReapBatch
	}
	res, err := db.ExecContext(ctx, reapOrphanClaimsSQL, olderThan.Seconds(), batch)
	if err != nil {
		return 0, fmt.Errorf("dripsupply: reap orphan claims (older than %s): %w", olderThan, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return int(n), nil
}

// -----------------------------------------------------------------------------
// small helpers
// -----------------------------------------------------------------------------

// compile-time proof that the interfaces used above are the package's, not
// ad-hoc ones.
var (
	_ Queryer = (*sql.DB)(nil)
	_ Queryer = (*sql.Tx)(nil)
	_ Queryer = (*sql.Conn)(nil)
)
