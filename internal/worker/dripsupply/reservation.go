package dripsupply

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Ledger statuses (§1.2 drip_capacity_ledger.status).
const (
	StatusReserved  = "reserved"
	StatusCommitted = "committed"
	StatusReleased  = "released"
	StatusExpired   = "expired"
)

// Binding reasons. The first five are the §1.2 vocabulary; the rest are the
// reasons §2.2 and §2.3 require but §1.2's illustrative list does not enumerate.
// binding_reason is TEXT with no CHECK, so this is an extension, not a conflict.
const (
	ReasonDomainTokens = "domain_tokens"
	ReasonLaneDemand   = "lane_demand"
	ReasonSupply       = "supply"
	ReasonPlanShare    = "plan_share"
	ReasonGovernor     = "governor" // emitted as "governor:<name>"

	ReasonRequested      = "requested"       // nothing constrained below demand
	ReasonOutsideWindow  = "outside_window"  // §2.3
	ReasonReserveTimeout = "reserve_timeout" // §2.2
	ReasonNoBalance      = "no_balance"      // fail closed: no domain balance row
	ReasonNoLaneBalance  = "no_lane_balance" // fail closed: no lane balance row
)

// ErrAllocationNotReserved is returned by Commit/Release when the allocation is
// no longer holding capacity — it was released or expired out from under the
// caller. This is NOT a benign retry: mail may have shipped against capacity the
// ledger has already handed back, so WP5 must alert on it rather than swallow it.
var ErrAllocationNotReserved = errors.New("dripsupply: allocation is not in reserved status")

// ErrAllocationNotFound is returned when the allocation id does not exist.
var ErrAllocationNotFound = errors.New("dripsupply: allocation not found")

// ReserveReq is §2.2's request, plus three fields the doc's algorithm needs but
// its struct literal omits:
//
//	DomainVersion / DispatchVersion — the idempotency_key is
//	  domain|isp|lane|wave_key|domain_ver|dispatch_ver (§1.2), so the versions
//	  must arrive with the request; the executor has already loaded them for the tick.
//	Win — §2.3's outside-window rule needs the domain contract's active window.
//	  A zero Window disables the window check (and is logged), because a silent
//	  "always closed" would wedge the estate.
type ReserveReq struct {
	Day        time.Time
	Domain     string
	ISP        string
	Lane       string
	TouchClass string // 'intro' | 'followup' | 'remail'
	WaveKey    string
	Requested  int
	// MailableSupply is the §2.2 supply term. A NEGATIVE value means "unknown /
	// not supply-bound"; 0 means there is genuinely nothing mailable and the
	// grant is 0 with binding_reason='supply'.
	MailableSupply  int
	DomainVersion   int
	DispatchVersion int
	Win             Window
}

// ReserveRes is §2.2's response.
type ReserveRes struct {
	AllocationID  uuid.UUID
	Granted       int
	BindingReason string
	Existing      bool
}

// IdempotencyKey is §1.2's key: domain|isp|lane|wave_key|domain_ver|dispatch_ver.
func (r ReserveReq) IdempotencyKey() string {
	return strings.Join([]string{
		strings.TrimSpace(r.Domain),
		normISP(r.ISP),
		strings.TrimSpace(r.Lane),
		strings.TrimSpace(r.WaveKey),
		fmt.Sprintf("%d", r.DomainVersion),
		fmt.Sprintf("%d", r.DispatchVersion),
	}, "|")
}

func (r ReserveReq) validate() error {
	switch {
	case strings.TrimSpace(r.Domain) == "":
		return errors.New("dripsupply: ReserveReq.Domain is required")
	case strings.TrimSpace(r.ISP) == "":
		return errors.New("dripsupply: ReserveReq.ISP is required")
	case strings.TrimSpace(r.Lane) == "":
		return errors.New("dripsupply: ReserveReq.Lane is required")
	case strings.TrimSpace(r.WaveKey) == "":
		// Without a wave key every reservation for the domain×isp×lane×versions
		// collapses onto one idempotency key and the second wave of the day
		// silently returns Existing with a stale grant.
		return errors.New("dripsupply: ReserveReq.WaveKey is required (it is part of the idempotency key)")
	case strings.TrimSpace(r.TouchClass) == "":
		return errors.New("dripsupply: ReserveReq.TouchClass is required")
	case !validTouchClass(r.TouchClass):
		// The ledger carries CHECK (touch_class IN ('intro','followup','remail'))
		// (WP1, cmd/server/main.go). Catching it here gives the caller a usable
		// message instead of a 23514 from deep inside the reservation.
		return fmt.Errorf("dripsupply: ReserveReq.TouchClass %q must be one of %v", r.TouchClass, TouchClasses)
	case r.Requested < 0:
		return fmt.Errorf("dripsupply: ReserveReq.Requested must be >= 0, got %d", r.Requested)
	}
	return nil
}

// TouchClasses are the §1.2 touch_class values; the ledger has a CHECK on them.
var TouchClasses = []string{"intro", "followup", "remail"}

func validTouchClass(s string) bool {
	for _, t := range TouchClasses {
		if strings.TrimSpace(s) == t {
			return true
		}
	}
	return false
}

// PlanReader supplies the §2.2 `plan_remaining` term. WP6 owns drip_daily_plan;
// this interface keeps WP3 off that table's (not-yet-pinned) key columns.
// It is called INSIDE the reservation transaction so the plan read and the
// balance decrement are one consistent snapshot.
//
// bounded=false means the plan does not constrain this cell.
type PlanReader interface {
	PlanRemaining(ctx context.Context, q Queryer, req ReserveReq) (limit int, bounded bool, err error)
}

// Service is the only way capacity is consumed (§2.2).
type Service struct {
	db   *sql.DB
	gov  GovernorReader
	plan PlanReader

	clock          func() time.Time
	stmtTimeout    string
	maxExpireBatch int

	// timeouts counts reservations abandoned to a statement timeout. Exposed
	// because a wedged reserve path must not be invisible — it returns
	// granted=0 like any other bound and writes no ledger row.
	timeouts atomic.Int64

	planWarnOnce sync.Once
}

// Option configures a Service.
type Option func(*Service)

// WithGovernors injects the governor stack RefillDomain applies.
func WithGovernors(g GovernorReader) Option { return func(s *Service) { s.gov = g } }

// WithPlanReader injects the plan_remaining term. Without it the plan term is
// unbounded and binding_reason can never be 'plan_share' — WP5 must wire WP6's
// reader before the planner is authoritative.
func WithPlanReader(p PlanReader) Option { return func(s *Service) { s.plan = p } }

// WithClock injects a clock (tests, and any deterministic replay).
func WithClock(f func() time.Time) Option { return func(s *Service) { s.clock = f } }

// WithStatementTimeout overrides the §2.2 5 s budget. Prod's default
// statement_timeout is 30 s; reserve deliberately runs far tighter because it
// holds a row lock every other wave on that domain is queued behind.
func WithStatementTimeout(d string) Option { return func(s *Service) { s.stmtTimeout = d } }

// WithMaxExpireBatch bounds one ExpireStale sweep.
func WithMaxExpireBatch(n int) Option { return func(s *Service) { s.maxExpireBatch = n } }

// NewService builds the reservation service.
func NewService(db *sql.DB, opts ...Option) *Service {
	s := &Service{
		db:             db,
		clock:          time.Now,
		stmtTimeout:    "5s",
		maxExpireBatch: 500,
	}
	for _, o := range opts {
		if o != nil {
			o(s)
		}
	}
	return s
}

// TimeoutCount is the number of reservations abandoned to a statement timeout
// since boot. Non-zero and rising means the balance rows are contended or the
// database is sick, and waves are being skipped without a ledger row.
func (s *Service) TimeoutCount() int64 { return s.timeouts.Load() }

func (s *Service) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

func (s *Service) governorCeilings(ctx context.Context, day time.Time, domain, isp string, w Window) ([]GovernorCeiling, error) {
	if s.gov == nil {
		return nil, nil
	}
	return s.gov.Ceilings(ctx, day, domain, normISP(isp), w)
}

// inTx runs fn in a transaction with the service's statement timeout applied via
// SET LOCAL, so the budget dies with the transaction and never leaks onto a
// pooled connection the next caller picks up.
func (s *Service) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if s.stmtTimeout != "" {
		if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '`+s.stmtTimeout+`'`); err != nil {
			return fmt.Errorf("set statement_timeout=%s: %w", s.stmtTimeout, err)
		}
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Reserve
// -----------------------------------------------------------------------------

// term is one input to the §2.2 min(). Order in the slice is the tie-break, so
// the binding reason is deterministic when two terms are equally small.
type term struct {
	name  string
	value int
}

// errDuplicateKey unwinds the reservation transaction when the idempotency key
// already exists. Rolling back is what makes "consumes nothing" true: the
// balance decrements made earlier in the transaction are undone with it.
var errDuplicateKey = errors.New("dripsupply: duplicate idempotency key")

// Reserve is the only way capacity is consumed. ONE transaction, in §2.2's order:
//
//	(1) SELECT … FOR UPDATE the domain balance row
//	(2) SELECT … FOR UPDATE the lane balance row      ← always domain THEN lane
//	(3) granted = min(requested, floor(tokens), effective-reserved-committed,
//	                  lane_unfilled, plan_remaining, mailable_supply)
//	(4) granted <= 0 → ledger row with reserved=0, status='released' and the
//	    binding reason naming the zero term; return
//	(5) decrement tokens, increment reserved on BOTH balances
//	(6) insert the ledger row
//	(7) return the id
//
// Double-fire safety: the ledger insert is ON CONFLICT (idempotency_key) DO
// NOTHING and the whole transaction is rolled back when it conflicts, so a
// scheduler re-fire, a retry, or the second ECS instance re-running the same
// wave gets the FIRST allocation back with Existing=true and consumes nothing.
//
// Timeout: on a statement/lock timeout the caller gets granted=0,
// binding_reason='reserve_timeout' and NO error (§2.2). No ledger row is written
// for a timeout — writing one would burn the idempotency key and make a
// transient database stall permanently unretryable for that wave.
func (s *Service) Reserve(ctx context.Context, req ReserveReq) (ReserveRes, error) {
	if s == nil || s.db == nil {
		return ReserveRes{}, errors.New("dripsupply: Reserve called on a nil service")
	}
	if err := req.validate(); err != nil {
		return ReserveRes{}, err
	}
	req.ISP = normISP(req.ISP)
	key := req.IdempotencyKey()

	// Fast path for the expected duplicate (scheduler re-fire / ECS bounce):
	// answer from the committed ledger row without queueing behind the domain
	// balance's row lock. Correctness does not depend on this — the in-transaction
	// ON CONFLICT below is the real guard.
	if res, found, err := s.lookupExisting(ctx, s.db, key); err != nil {
		return ReserveRes{}, err
	} else if found {
		return res, nil
	}

	if (req.Win != Window{}) && !req.Win.Contains(req.Day, s.now()) {
		return s.recordZeroGrant(ctx, req, key, ReasonOutsideWindow)
	}
	if (req.Win == Window{}) {
		log.Printf("[DripSupply] reserve %s/%s/%s: no active window on the request — the outside_window guard is DISABLED for this call", req.Domain, req.ISP, req.Lane)
	}

	var out ReserveRes
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// (1) domain balance, locked.
		var bal Balance
		err := tx.QueryRowContext(ctx, `
			SELECT contracted, effective, effective_reason, tokens, reserved, committed
			FROM drip_capacity_balance
			WHERE day = $1::date AND sending_domain = $2 AND isp = $3
			FOR UPDATE
		`, dayKey(req.Day), req.Domain, req.ISP).Scan(&bal.Contracted, &bal.Effective, &bal.EffectiveReason, &bal.Tokens, &bal.Reserved, &bal.Committed)
		if errors.Is(err, sql.ErrNoRows) {
			return s.insertZeroLedgerRow(ctx, tx, req, key, ReasonNoBalance, 0, 0, &out)
		}
		if err != nil {
			return fmt.Errorf("lock domain balance %s/%s: %w", req.Domain, req.ISP, err)
		}

		// (2) lane balance, locked — ALWAYS after the domain row. Two orchestrator
		// instances taking the same two rows in the same order cannot deadlock.
		var lane LaneBalance
		err = tx.QueryRowContext(ctx, `
			SELECT desired, unfilled, reserved, committed
			FROM drip_lane_balance
			WHERE day = $1::date AND lane = $2 AND isp = $3
			FOR UPDATE
		`, dayKey(req.Day), req.Lane, req.ISP).Scan(&lane.Desired, &lane.Unfilled, &lane.Reserved, &lane.Committed)
		if errors.Is(err, sql.ErrNoRows) {
			return s.insertZeroLedgerRow(ctx, tx, req, key, ReasonNoLaneBalance, bal.Headroom(), 0, &out)
		}
		if err != nil {
			return fmt.Errorf("lock lane balance %s/%s: %w", req.Lane, req.ISP, err)
		}

		// (3) the min().
		// The governor label comes off the ROW (effective_reason, written by
		// RefillDomain), not from process memory: both orchestrator instances and
		// the §3 API must report the same reason for the same cell.
		domainReason := ReasonDomainTokens
		if bal.Effective < bal.Contracted {
			name := strings.TrimSpace(bal.EffectiveReason)
			if name == "" {
				// effective was reduced but nothing claimed it — a refill that
				// predates the column, or a hand-edited row. Say so rather than
				// naming a governor that may not have done it.
				name = "reduced"
			}
			domainReason = ReasonGovernor + ":" + name
		}
		// Order is the tie-break (see bindingMin). The domain HEADROOM term sits
		// ahead of the token term on purpose: when a governor zeroes `effective`
		// the refill also zeroes `tokens`, and both terms are 0 — reporting
		// 'domain_tokens' there would blame pacing for a governor stop and send
		// the operator looking at the wrong thing.
		terms := []term{
			{ReasonRequested, req.Requested},
			{domainReason, bal.Headroom()},
			{ReasonDomainTokens, int(math.Floor(bal.Tokens))},
			{ReasonLaneDemand, lane.Unfilled},
		}
		if s.plan != nil {
			limit, bounded, err := s.plan.PlanRemaining(ctx, tx, req)
			if err != nil {
				return fmt.Errorf("plan_remaining %s/%s/%s: %w", req.Domain, req.ISP, req.Lane, err)
			}
			if bounded {
				terms = append(terms, term{ReasonPlanShare, limit})
			}
		} else {
			s.planWarnOnce.Do(func() {
				log.Printf("[DripSupply] no PlanReader wired — the plan_share term is UNBOUNDED and the daily plan does not constrain reservations")
			})
		}
		if req.MailableSupply >= 0 {
			terms = append(terms, term{ReasonSupply, req.MailableSupply})
		}

		granted, reason := bindingMin(terms)

		// (4) zero grant still records why.
		if granted <= 0 {
			return s.insertZeroLedgerRow(ctx, tx, req, key, reason, bal.Headroom(), lane.Unfilled, &out)
		}

		// (5) decrement tokens, increment reserved on BOTH balances.
		if _, err := tx.ExecContext(ctx, `
			UPDATE drip_capacity_balance
			SET tokens = GREATEST(tokens - $4, 0), reserved = reserved + $4
			WHERE day = $1::date AND sending_domain = $2 AND isp = $3
		`, dayKey(req.Day), req.Domain, req.ISP, granted); err != nil {
			return fmt.Errorf("decrement domain balance %s/%s: %w", req.Domain, req.ISP, err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE drip_lane_balance
			SET reserved = reserved + $4, unfilled = GREATEST(unfilled - $4, 0)
			WHERE day = $1::date AND lane = $2 AND isp = $3
		`, dayKey(req.Day), req.Lane, req.ISP, granted); err != nil {
			return fmt.Errorf("decrement lane balance %s/%s: %w", req.Lane, req.ISP, err)
		}

		// (6) the ledger row.
		id := uuid.New()
		inserted, err := s.insertLedgerRow(ctx, tx, ledgerRow{
			AllocationID:  id,
			Key:           key,
			Req:           req,
			Reserved:      granted,
			Status:        StatusReserved,
			BindingReason: reason,
			DomainAfter:   bal.Headroom() - granted,
			LaneUnfilled:  lane.Unfilled - granted,
			Tick:          s.now(),
		})
		if err != nil {
			return err
		}
		if !inserted {
			// Someone else committed this exact key while we held the locks.
			// Unwind everything (that is what makes "consumes nothing" true).
			return errDuplicateKey
		}
		out = ReserveRes{AllocationID: id, Granted: granted, BindingReason: reason}
		return nil
	})

	switch {
	case errors.Is(err, errDuplicateKey):
		res, found, lerr := s.lookupExisting(ctx, s.db, key)
		if lerr != nil {
			return ReserveRes{}, lerr
		}
		if !found {
			return ReserveRes{}, fmt.Errorf("dripsupply: idempotency key %q conflicted but no row is visible", key)
		}
		return res, nil
	case err != nil && isStatementTimeout(err):
		n := s.timeouts.Add(1)
		log.Printf("[DripSupply] reserve TIMED OUT (%s/%s/%s wave=%s, budget=%s, total=%d) — granting 0 with reason=%s; no ledger row is written so the wave key stays retryable: %v",
			req.Domain, req.ISP, req.Lane, req.WaveKey, s.stmtTimeout, n, ReasonReserveTimeout, err)
		return ReserveRes{Granted: 0, BindingReason: ReasonReserveTimeout}, nil
	case err != nil:
		return ReserveRes{}, fmt.Errorf("dripsupply: reserve %s/%s/%s wave=%s: %w", req.Domain, req.ISP, req.Lane, req.WaveKey, err)
	}
	return out, nil
}

// bindingMin returns the smallest term and the name of the term that bound it.
// Ties resolve to the EARLIEST term in the slice, which is why 'requested' is
// first: when demand is fully satisfied the reason reads 'requested', not a
// constraint that happened to equal it.
func bindingMin(terms []term) (int, string) {
	best, reason := math.MaxInt, ReasonRequested
	for _, t := range terms {
		v := t.value
		if v < 0 {
			v = 0
		}
		if v < best {
			best, reason = v, t.name
		}
	}
	if best == math.MaxInt {
		return 0, ReasonRequested
	}
	return best, reason
}

// recordZeroGrant writes a standalone zero-grant ledger row (used for the
// outside-window short circuit, which needs no balance locks).
func (s *Service) recordZeroGrant(ctx context.Context, req ReserveReq, key, reason string) (ReserveRes, error) {
	var out ReserveRes
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		return s.insertZeroLedgerRow(ctx, tx, req, key, reason, 0, 0, &out)
	})
	if err != nil {
		if errors.Is(err, errDuplicateKey) {
			res, found, lerr := s.lookupExisting(ctx, s.db, key)
			if lerr != nil {
				return ReserveRes{}, lerr
			}
			if found {
				return res, nil
			}
		}
		if isStatementTimeout(err) {
			s.timeouts.Add(1)
			return ReserveRes{Granted: 0, BindingReason: ReasonReserveTimeout}, nil
		}
		return ReserveRes{}, fmt.Errorf("dripsupply: record zero grant %s/%s/%s: %w", req.Domain, req.ISP, req.Lane, err)
	}
	return out, nil
}

// insertZeroLedgerRow is §2.2 step (4): a zero grant is still a decision and
// still gets a row, with reserved=0, status='released' and the reason naming the
// term that was zero. This is the whole answer to "is this lane wedged or just
// out of demand" — without it, a starved lane is indistinguishable from silence.
func (s *Service) insertZeroLedgerRow(ctx context.Context, tx *sql.Tx, req ReserveReq, key, reason string, domainAfter, laneAfter int, out *ReserveRes) error {
	id := uuid.New()
	inserted, err := s.insertLedgerRow(ctx, tx, ledgerRow{
		AllocationID:  id,
		Key:           key,
		Req:           req,
		Reserved:      0,
		Status:        StatusReleased,
		BindingReason: reason,
		DomainAfter:   domainAfter,
		LaneUnfilled:  laneAfter,
		Tick:          s.now(),
	})
	if err != nil {
		return err
	}
	if !inserted {
		return errDuplicateKey
	}
	*out = ReserveRes{AllocationID: id, Granted: 0, BindingReason: reason}
	return nil
}

type ledgerRow struct {
	AllocationID  uuid.UUID
	Key           string
	Req           ReserveReq
	Reserved      int
	Status        string
	BindingReason string
	DomainAfter   int
	LaneUnfilled  int
	Tick          time.Time
}

func (s *Service) insertLedgerRow(ctx context.Context, tx *sql.Tx, r ledgerRow) (bool, error) {
	if r.DomainAfter < 0 {
		r.DomainAfter = 0
	}
	if r.LaneUnfilled < 0 {
		r.LaneUnfilled = 0
	}
	var got uuid.UUID
	err := tx.QueryRowContext(ctx, `
		INSERT INTO drip_capacity_ledger (
			allocation_id, idempotency_key, day, tick, sending_domain, isp, lane, touch_class,
			domain_contract_version, dispatch_contract_version,
			requested, reserved, committed, released, status, campaign_id, binding_reason,
			domain_balance_after, lane_unfilled_after, created_at, updated_at
		) VALUES ($1, $2, $3::date, $4, $5, $6, $7, $8, $9, $10, $11, $12, 0, 0, $13, NULL, $14, $15, $16, NOW(), NOW())
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING allocation_id
	`,
		r.AllocationID, r.Key, dayKey(r.Req.Day), r.Tick, r.Req.Domain, normISP(r.Req.ISP), r.Req.Lane, r.Req.TouchClass,
		r.Req.DomainVersion, r.Req.DispatchVersion, r.Req.Requested, r.Reserved, r.Status, r.BindingReason,
		r.DomainAfter, r.LaneUnfilled,
	).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("insert ledger row (key=%s): %w", r.Key, err)
	}
	return true, nil
}

// lookupExisting resolves an idempotency key to the allocation that already
// owns it. A row still in 'reserved' hands back its OUTSTANDING capacity; a row
// that has already been committed, released or expired hands back 0 — returning
// its original `reserved` would let a retry ship the same wave twice against
// capacity that is no longer held.
func (s *Service) lookupExisting(ctx context.Context, q Queryer, key string) (ReserveRes, bool, error) {
	var (
		id                            uuid.UUID
		reserved, committed, released int
		status, reason                string
	)
	err := q.QueryRowContext(ctx, `
		SELECT allocation_id, reserved, committed, released, status, binding_reason
		FROM drip_capacity_ledger
		WHERE idempotency_key = $1
	`, key).Scan(&id, &reserved, &committed, &released, &status, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return ReserveRes{}, false, nil
	}
	if err != nil {
		return ReserveRes{}, false, fmt.Errorf("dripsupply: lookup idempotency key %q: %w", key, err)
	}
	res := ReserveRes{AllocationID: id, BindingReason: reason, Existing: true}
	if status == StatusReserved {
		if out := reserved - committed - released; out > 0 {
			res.Granted = out
		}
	} else {
		res.BindingReason = "already_" + status
	}
	return res, true, nil
}

// -----------------------------------------------------------------------------
// Commit / Release / ExpireStale
// -----------------------------------------------------------------------------

// settlement is the shared body of Commit, Release and ExpireStale: it moves
// `submit` of an allocation's outstanding reservation to committed, hands
// `giveBack` back to both balances, and writes the ledger row's new status.
//
// Lock order is ledger row → domain balance → lane balance. Reserve never locks
// a ledger row, so it cannot be on the other side of a cycle; every settlement
// path takes the same three in the same order.
func (s *Service) settle(ctx context.Context, allocationID uuid.UUID, submit int, campaignID uuid.UUID, releaseAll bool, finalStatus, releaseReason, why string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var (
			day                           time.Time
			domain, isp, lane, status     string
			reserved, committed, released int
		)
		err := tx.QueryRowContext(ctx, `
			SELECT day, sending_domain, isp, lane, reserved, committed, released, status
			FROM drip_capacity_ledger
			WHERE allocation_id = $1
			FOR UPDATE
		`, allocationID).Scan(&day, &domain, &isp, &lane, &reserved, &committed, &released, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAllocationNotFound
		}
		if err != nil {
			return fmt.Errorf("lock ledger row %s: %w", allocationID, err)
		}

		if status != StatusReserved {
			// Already settled. A repeat Commit is the expected shape of a retry
			// or a double deploy and must be a no-op, not a double count.
			if status == StatusCommitted {
				log.Printf("[DripSupply] settle %s: already %s — no-op (%s)", allocationID, status, why)
				return nil
			}
			return fmt.Errorf("%w: allocation %s is %s", ErrAllocationNotReserved, allocationID, status)
		}

		outstanding := reserved - committed - released
		if outstanding < 0 {
			outstanding = 0
		}
		if submit > outstanding {
			log.Printf("[DripSupply] settle %s: submitted %d > outstanding %d — clamping (%s)", allocationID, submit, outstanding, why)
			submit = outstanding
		}
		if submit < 0 {
			submit = 0
		}
		giveBack := 0
		if releaseAll {
			giveBack = outstanding - submit
		} else {
			// Release(n): give back exactly n of the outstanding reservation.
			giveBack = submit
			submit = 0
			if giveBack > outstanding {
				giveBack = outstanding
			}
		}
		consumed := submit + giveBack
		if consumed == 0 {
			return nil
		}

		newStatus := status
		if releaseAll {
			newStatus = finalStatus
			if submit == 0 && finalStatus == StatusCommitted {
				newStatus = StatusReleased
			}
		} else if committed+submit == 0 && reserved-released-giveBack == 0 {
			newStatus = StatusReleased
		}

		var campaignArg any
		if campaignID != uuid.Nil {
			campaignArg = campaignID
		}
		// release_reason records WHY capacity was handed back. binding_reason is
		// left untouched — it is the record of why the GRANT was the size it was,
		// and overwriting it would destroy the only evidence of the constraint.
		// Successive partial releases append rather than clobber.
		reasonArg := ""
		if giveBack > 0 {
			reasonArg = strings.TrimSpace(releaseReason)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE drip_capacity_ledger
			SET committed = committed + $2,
			    released  = released  + $3,
			    status    = $4,
			    campaign_id = COALESCE($5, campaign_id),
			    release_reason = CASE
			        WHEN $6::text = '' THEN release_reason
			        WHEN release_reason IS NULL OR release_reason = '' THEN $6::text
			        ELSE release_reason || '; ' || $6::text
			    END,
			    updated_at = NOW()
			WHERE allocation_id = $1
		`, allocationID, submit, giveBack, newStatus, campaignArg, reasonArg); err != nil {
			return fmt.Errorf("update ledger row %s: %w", allocationID, err)
		}

		// Returned tokens are capped at `effective` so a release cannot mint a
		// bucket bigger than the day's whole allowance; the burst ceiling itself
		// is re-applied by the next Refill.
		if _, err := tx.ExecContext(ctx, `
			UPDATE drip_capacity_balance
			SET reserved  = GREATEST(reserved - $4, 0),
			    committed = committed + $5,
			    released  = released  + $6,
			    tokens    = LEAST(tokens + $6, effective)
			WHERE day = $1::date AND sending_domain = $2 AND isp = $3
		`, day.Format("2006-01-02"), domain, isp, consumed, submit, giveBack); err != nil {
			return fmt.Errorf("settle domain balance %s/%s: %w", domain, isp, err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE drip_lane_balance
			SET reserved  = GREATEST(reserved - $4, 0),
			    committed = committed + $5,
			    unfilled  = unfilled + $6
			WHERE day = $1::date AND lane = $2 AND isp = $3
		`, day.Format("2006-01-02"), lane, isp, consumed, submit, giveBack); err != nil {
			return fmt.Errorf("settle lane balance %s/%s: %w", lane, isp, err)
		}
		return nil
	})
}

// Commit moves `submitted` of the allocation from reserved to committed and
// RELEASES THE REMAINDER back to both balances (§2.2). Call it after
// deployWaveGroups with the number the transport actually accepted — a wave that
// reserved 1,000 and submitted 600 must hand 400 back to the day, not strand it
// until ExpireStale notices 45 minutes later.
//
// Idempotent: a second Commit on an already-committed allocation is a no-op.
func (s *Service) Commit(ctx context.Context, allocationID uuid.UUID, submitted int, campaignID uuid.UUID) error {
	if allocationID == uuid.Nil {
		return errors.New("dripsupply: Commit called with a nil allocation id")
	}
	if submitted < 0 {
		return fmt.Errorf("dripsupply: Commit submitted must be >= 0, got %d", submitted)
	}
	if err := s.settle(ctx, allocationID, submitted, campaignID, true, StatusCommitted, "partial_commit", "commit"); err != nil {
		return fmt.Errorf("dripsupply: commit %s (submitted=%d): %w", allocationID, submitted, err)
	}
	return nil
}

// Release hands `n` of an allocation's outstanding reservation back without
// committing it (a failed wave group, a cancelled deploy).
//
// `reason` is written to drip_capacity_ledger.release_reason (WP1 follow-through)
// and logged; binding_reason — the record of why the grant was the size it was —
// is left intact. Repeated partial releases append to release_reason.
func (s *Service) Release(ctx context.Context, allocationID uuid.UUID, n int, reason string) error {
	if allocationID == uuid.Nil {
		return errors.New("dripsupply: Release called with a nil allocation id")
	}
	if n <= 0 {
		return nil
	}
	log.Printf("[DripSupply] release %s n=%d reason=%s", allocationID, n, reason)
	if err := s.settle(ctx, allocationID, n, uuid.Nil, false, StatusReleased, reason, "release:"+reason); err != nil {
		return fmt.Errorf("dripsupply: release %s (n=%d, reason=%s): %w", allocationID, n, reason, err)
	}
	return nil
}

// ExpireStale releases reservations still in 'reserved' after olderThan, returns
// their tokens to the balance, and stamps release_reason='expire_stale'
// (§2.2: reserved > 45 min with no commit).
// This is the crash-window recovery: an ECS bounce between Reserve and Commit
// strands the grant on the balance forever otherwise, and the domain silently
// mails less every day until the day rolls.
//
// One allocation per transaction, bounded by MaxExpireBatch, honouring context
// cancellation between rows — a sweep that took every stale row in one
// transaction would hold locks across the whole estate's balances.
func (s *Service) ExpireStale(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan <= 0 {
		return 0, fmt.Errorf("dripsupply: ExpireStale needs a positive age, got %s", olderThan)
	}
	// The cutoff is computed by POSTGRES, not by s.now(): created_at was stamped
	// by the database clock, and two ECS tasks comparing it against their own
	// wall clocks would expire each other's live reservations on any skew.
	rows, err := s.db.QueryContext(ctx, `
		SELECT allocation_id
		FROM drip_capacity_ledger
		WHERE status = 'reserved' AND created_at < NOW() - make_interval(secs => $1)
		ORDER BY created_at
		LIMIT $2
	`, olderThan.Seconds(), s.maxExpireBatch)
	if err != nil {
		return 0, fmt.Errorf("dripsupply: ExpireStale scan (older than %s): %w", olderThan, err)
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("dripsupply: ExpireStale scan row: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("dripsupply: ExpireStale rows: %w", err)
	}
	rows.Close()

	expired := 0
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return expired, fmt.Errorf("dripsupply: ExpireStale cancelled after %d: %w", expired, err)
		}
		if err := s.settle(ctx, id, 0, uuid.Nil, true, StatusExpired, "expire_stale", "expire_stale"); err != nil {
			if errors.Is(err, ErrAllocationNotFound) || errors.Is(err, ErrAllocationNotReserved) {
				continue // settled by someone else between the scan and now
			}
			return expired, fmt.Errorf("dripsupply: ExpireStale %s: %w", id, err)
		}
		expired++
	}
	if expired > 0 {
		log.Printf("[DripSupply] ExpireStale released %d reservation(s) older than %s", expired, olderThan)
	}
	if len(ids) == s.maxExpireBatch {
		log.Printf("[DripSupply] ExpireStale hit its batch bound (%d) — more stale reservations remain; the next sweep will take them", s.maxExpireBatch)
	}
	return expired, nil
}
