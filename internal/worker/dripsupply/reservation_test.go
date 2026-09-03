package dripsupply

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// -----------------------------------------------------------------------------
// Harness
// -----------------------------------------------------------------------------
//
// These tests need REAL Postgres. The contract under test is row locking
// (SELECT … FOR UPDATE), ON CONFLICT semantics and NUMERIC arithmetic —
// sqlmock returns canned rows without evaluating any of it and cannot tell a
// correct guard from a missing one.
//
// They run against the LOCAL apex-postgres container (localhost:5432), in a
// scratch database `req118_res`, each test in its own schema which is dropped at
// the end. Nothing here can reach production: the DSN is hard-defaulted to
// localhost and the tests skip (never fail, never fall back) when it is
// unreachable.

const (
	testAdminDSNEnv = "DRIPSUPPLY_TEST_ADMIN_DSN"
	defaultAdminDSN = "postgres://apex_user:apex_password@localhost:5432/apex_db?sslmode=disable"
	scratchDBName   = "req118_res"
)

func adminDSN() string {
	if v := strings.TrimSpace(os.Getenv(testAdminDSNEnv)); v != "" {
		return v
	}
	return defaultAdminDSN
}

// scratchDSN points the admin DSN at the req118_res database.
func scratchDSN(t *testing.T) string {
	t.Helper()
	dsn := adminDSN()
	i := strings.LastIndex(dsn, "/")
	if i < 0 {
		t.Skipf("cannot derive a scratch DSN from %q", dsn)
	}
	tail := dsn[i+1:]
	q := ""
	if j := strings.Index(tail, "?"); j >= 0 {
		q = tail[j:]
	}
	return dsn[:i+1] + scratchDBName + q
}

// ensureScratchDB creates req118_res if it is not there yet.
func ensureScratchDB(t *testing.T) {
	t.Helper()
	admin, err := sql.Open("postgres", adminDSN())
	if err != nil {
		t.Skipf("cannot open admin DSN: %v", err)
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Skipf("local postgres unreachable (%v) — set %s to run the dripsupply integration tests", err, testAdminDSNEnv)
	}
	var exists bool
	if err := admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, scratchDBName).Scan(&exists); err != nil {
		t.Skipf("cannot list databases: %v", err)
	}
	if exists {
		return
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+scratchDBName); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		t.Skipf("cannot create scratch database %s: %v", scratchDBName, err)
	}
}

// schemaDDL is the §1.2 shape plus the one production table the real governor
// reads. The balance/ledger DDL comes from the package constants, so a WP1/WP3
// drift breaks these tests instead of production.
func schemaDDL() []string {
	out := []string{CapacityLedgerDDL, CapacityBalanceDDL, LaneBalanceDDL}
	out = append(out, CapacityLedgerIndexDDL...)
	// cmd/server/main.go:5322
	out = append(out, `CREATE TABLE IF NOT EXISTS mailing_isp_throttle_state (
		isp           TEXT PRIMARY KEY,
		msgs_per_hour DOUBLE PRECISION NOT NULL,
		updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)
	return out
}

// newTestDB returns a pool pinned to a per-test schema via a CONNECTION
// parameter, not a `SET search_path` statement: database/sql hands each
// goroutine an arbitrary pooled connection, so a SET would apply to exactly one
// of them and the other 99 would resolve the table names elsewhere — which reads
// as "mutual exclusion passed" while nothing was tested.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	ensureScratchDB(t)
	schema := "t" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")

	bootstrap, err := sql.Open("postgres", scratchDSN(t))
	if err != nil {
		t.Skipf("cannot open scratch DSN: %v", err)
	}
	defer bootstrap.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := bootstrap.PingContext(ctx); err != nil {
		t.Skipf("scratch database unreachable: %v", err)
	}
	if _, err := bootstrap.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}

	dsn := scratchDSN(t)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := sql.Open("postgres", dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatalf("open scratch: %v", err)
	}
	// Enough connections that every worker can be in flight at once; a smaller
	// pool serializes them and masks a missing row lock.
	db.SetMaxOpenConns(30)
	db.SetMaxIdleConns(30)
	t.Cleanup(func() {
		db.Close()
		clean, err := sql.Open("postgres", scratchDSN(t))
		if err == nil {
			_, _ = clean.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
			clean.Close()
		}
	})
	for _, stmt := range schemaDDL() {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("ddl %.60s: %v", strings.ReplaceAll(stmt, "\n", " "), err)
		}
	}
	return db
}

func testLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		return time.UTC
	}
	return loc
}

// testDay is a fixed non-DST-boundary Denver day, so every assertion here is
// reproducible regardless of when the suite runs.
func testDay(t *testing.T) time.Time {
	t.Helper()
	return time.Date(2026, 9, 10, 0, 0, 0, 0, testLoc(t))
}

func domainContract(domain string, version int, maxByISP map[string]int) *DomainContract {
	return &DomainContract{
		Meta:              Meta{Version: version},
		SendingDomain:     domain,
		BrandCode:         "ht",
		DailyMaxByISP:     maxByISP,
		ActiveWindowStart: "01:00",
		ActiveWindowEnd:   "20:00",
		IntervalMinutes:   15,
		MaxBurstIntervals: 2,
	}
}

func dispatchContract(lane string, version int, desired map[string]int, exclusions ...string) *DispatchContract {
	return &DispatchContract{
		Meta:               Meta{Version: version},
		Lane:               lane,
		DesiredDailyIntros: desired,
		ISPExclusions:      exclusions,
		AllowedDomains:     []string{"ht"},
	}
}

func activeSet(day time.Time, doms []*DomainContract, disp []*DispatchContract) *ActiveSet {
	set := &ActiveSet{Day: day, Domains: map[string]*DomainContract{}, Dispatches: map[string]*DispatchContract{}}
	for _, d := range doms {
		set.Domains[d.SendingDomain] = d
	}
	for _, d := range disp {
		set.Dispatches[d.Lane] = d
	}
	return set
}

// setBalance forces a domain balance row into an exact state so a test can pin
// ONE term of the min() and leave the others slack.
func setBalance(t *testing.T, db *sql.DB, day time.Time, domain, isp string, effective int, tokens float64) {
	t.Helper()
	if _, err := db.Exec(`
		UPDATE drip_capacity_balance SET effective = $4, tokens = $5
		WHERE day = $1::date AND sending_domain = $2 AND isp = $3
	`, dayKey(day), domain, isp, effective, tokens); err != nil {
		t.Fatalf("set balance: %v", err)
	}
}

func setLaneUnfilled(t *testing.T, db *sql.DB, day time.Time, lane, isp string, unfilled int) {
	t.Helper()
	if _, err := db.Exec(`
		UPDATE drip_lane_balance SET unfilled = $4
		WHERE day = $1::date AND lane = $2 AND isp = $3
	`, dayKey(day), lane, isp, unfilled); err != nil {
		t.Fatalf("set lane unfilled: %v", err)
	}
}

type balanceRow struct {
	Contracted, Effective, Reserved, Committed, Released int
	Tokens                                               float64
}

func readBalance(t *testing.T, db *sql.DB, day time.Time, domain, isp string) balanceRow {
	t.Helper()
	var b balanceRow
	err := db.QueryRow(`
		SELECT contracted, effective, tokens, reserved, committed, released
		FROM drip_capacity_balance WHERE day = $1::date AND sending_domain = $2 AND isp = $3
	`, dayKey(day), domain, isp).Scan(&b.Contracted, &b.Effective, &b.Tokens, &b.Reserved, &b.Committed, &b.Released)
	if err != nil {
		t.Fatalf("read balance %s/%s: %v", domain, isp, err)
	}
	return b
}

type laneRow struct{ Desired, Reserved, Committed, Unfilled int }

func readLane(t *testing.T, db *sql.DB, day time.Time, lane, isp string) laneRow {
	t.Helper()
	var l laneRow
	err := db.QueryRow(`
		SELECT desired, reserved, committed, unfilled
		FROM drip_lane_balance WHERE day = $1::date AND lane = $2 AND isp = $3
	`, dayKey(day), lane, isp).Scan(&l.Desired, &l.Reserved, &l.Committed, &l.Unfilled)
	if err != nil {
		t.Fatalf("read lane %s/%s: %v", lane, isp, err)
	}
	return l
}

type ledgerRowRead struct {
	Requested, Reserved, Committed, Released int
	Status, Reason                           string
	ReleaseReason                            sql.NullString
	CampaignID                               *uuid.UUID
}

func readLedger(t *testing.T, db *sql.DB, id uuid.UUID) ledgerRowRead {
	t.Helper()
	var r ledgerRowRead
	err := db.QueryRow(`
		SELECT requested, reserved, committed, released, status, binding_reason, release_reason, campaign_id
		FROM drip_capacity_ledger WHERE allocation_id = $1
	`, id).Scan(&r.Requested, &r.Reserved, &r.Committed, &r.Released, &r.Status, &r.Reason, &r.ReleaseReason, &r.CampaignID)
	if err != nil {
		t.Fatalf("read ledger %s: %v", id, err)
	}
	return r
}

// readEffectiveReason reads the persisted governor label off the balance row.
func readEffectiveReason(t *testing.T, db *sql.DB, day time.Time, domain, isp string) string {
	t.Helper()
	var reason string
	if err := db.QueryRow(`
		SELECT effective_reason FROM drip_capacity_balance
		WHERE day = $1::date AND sending_domain = $2 AND isp = $3
	`, dayKey(day), domain, isp).Scan(&reason); err != nil {
		t.Fatalf("read effective_reason %s/%s: %v", domain, isp, err)
	}
	return reason
}

// seedDay is the common fixture: one domain, one lane, one ISP, day open.
func seedDay(t *testing.T, db *sql.DB, day time.Time, domainMax, laneDesired int) (*DomainContract, *DispatchContract) {
	t.Helper()
	dc := domainContract("em.historythinking.com", 1, map[string]int{"aol": domainMax})
	lc := dispatchContract("wcl_remail", 1, map[string]int{"aol": laneDesired})
	if _, err := EnsureDayBalances(context.Background(), db, day, activeSet(day, []*DomainContract{dc}, []*DispatchContract{lc})); err != nil {
		t.Fatalf("EnsureDayBalances: %v", err)
	}
	return dc, lc
}

func baseReq(day time.Time, wave string, requested int) ReserveReq {
	return ReserveReq{
		Day:             day,
		Domain:          "em.historythinking.com",
		ISP:             "aol",
		Lane:            "wcl_remail",
		TouchClass:      "intro",
		WaveKey:         wave,
		Requested:       requested,
		MailableSupply:  NoLimit,
		DomainVersion:   1,
		DispatchVersion: 1,
		Win:             DefaultWindow(),
	}
}

// midWindow is a clock inside the active window, so the outside_window guard
// never fires in tests that are about something else.
func midWindow(day time.Time) func() time.Time {
	return func() time.Time { return dayOf(day).Add(10 * time.Hour) }
}

// -----------------------------------------------------------------------------
// §8.2 test 1 — 100 concurrent reservations cannot exceed a domain×ISP balance
// -----------------------------------------------------------------------------

func TestReserve_ConcurrentCannotExceedDomainBalance(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	seedDay(t, db, day, 1000, 1_000_000)

	const (
		goroutines = 100
		perReq     = 25
		effective  = 1000
	)
	// Tokens and lane demand are deliberately slack: this test pins the
	// effective-reserved-committed term and nothing else.
	setBalance(t, db, day, "em.historythinking.com", "aol", effective, 1_000_000)
	setLaneUnfilled(t, db, day, "wcl_remail", "aol", 1_000_000)

	svc := NewService(db, WithClock(midWindow(day)))

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		total   int
		errs    []error
		start   = make(chan struct{})
		reasons = map[string]int{}
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := svc.Reserve(ctx, baseReq(day, fmt.Sprintf("w%03d", i), perReq))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			total += res.Granted
			reasons[res.BindingReason]++
		}(i)
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d reservations errored, first: %v", len(errs), errs[0])
	}
	if total != effective {
		t.Fatalf("granted total = %d, want exactly %d (the whole balance, no more and no less); reasons=%v", total, effective, reasons)
	}

	bal := readBalance(t, db, day, "em.historythinking.com", "aol")
	if bal.Reserved != effective {
		t.Fatalf("balance.reserved = %d, want %d", bal.Reserved, effective)
	}
	if bal.Reserved > bal.Effective {
		t.Fatalf("OVER-GRANT: reserved %d > effective %d", bal.Reserved, bal.Effective)
	}

	var ledgerReserved, ledgerRows int
	if err := db.QueryRow(`SELECT COALESCE(SUM(reserved),0), COUNT(*) FROM drip_capacity_ledger WHERE day = $1::date`, dayKey(day)).
		Scan(&ledgerReserved, &ledgerRows); err != nil {
		t.Fatalf("ledger sum: %v", err)
	}
	if ledgerReserved != effective {
		t.Fatalf("ledger SUM(reserved) = %d, want %d", ledgerReserved, effective)
	}
	// Every attempt is recorded, including the ones that got nothing: a starved
	// lane must be distinguishable from silence (§2.2 step 4).
	if ledgerRows != goroutines {
		t.Fatalf("ledger rows = %d, want %d (one per attempt, zero grants included)", ledgerRows, goroutines)
	}
	var zeroRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM drip_capacity_ledger WHERE reserved = 0 AND status = 'released' AND binding_reason <> ''`).Scan(&zeroRows); err != nil {
		t.Fatalf("zero rows: %v", err)
	}
	if zeroRows == 0 {
		t.Fatal("no zero-grant ledger rows: the losers of the race recorded nothing")
	}
	t.Logf("100 goroutines × %d requested: granted %d, %d ledger rows (%d zero-grant), reasons=%v",
		perReq, total, ledgerRows, zeroRows, reasons)
}

// TestReserve_NegativeControl_NoRowLockOverGrants is the negative control for
// test 1: the SAME race, run against a reservation that reads the balance
// WITHOUT `FOR UPDATE`. It must over-grant. If this passes (i.e. does not
// over-grant) the harness cannot detect a missing row lock and test 1 above
// proves nothing.
func TestReserve_NegativeControl_NoRowLockOverGrants(t *testing.T) {
	db := newTestDB(t)
	day := testDay(t)
	seedDay(t, db, day, 1000, 1_000_000)
	setBalance(t, db, day, "em.historythinking.com", "aol", 1000, 1_000_000)

	const (
		goroutines = 100
		perReq     = 25
		effective  = 1000
	)

	// naiveReserve is what Reserve would be without step (1)'s FOR UPDATE: read
	// the headroom, then write. The sleep makes the interleaving deterministic
	// instead of hoping the scheduler produces it.
	naiveReserve := func(ctx context.Context) (int, error) {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return 0, err
		}
		defer func() { _ = tx.Rollback() }()
		var effectiveCol, reserved, committed int
		if err := tx.QueryRowContext(ctx, `
			SELECT effective, reserved, committed FROM drip_capacity_balance
			WHERE day = $1::date AND sending_domain = $2 AND isp = $3
		`, dayKey(day), "em.historythinking.com", "aol").Scan(&effectiveCol, &reserved, &committed); err != nil {
			return 0, err
		}
		headroom := effectiveCol - reserved - committed
		granted := perReq
		if headroom < granted {
			granted = headroom
		}
		if granted <= 0 {
			return 0, tx.Commit()
		}
		time.Sleep(5 * time.Millisecond)
		if _, err := tx.ExecContext(ctx, `
			UPDATE drip_capacity_balance SET reserved = reserved + $4
			WHERE day = $1::date AND sending_domain = $2 AND isp = $3
		`, dayKey(day), "em.historythinking.com", "aol", granted); err != nil {
			return 0, err
		}
		return granted, tx.Commit()
	}

	var wg sync.WaitGroup
	startCh := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startCh
			_, _ = naiveReserve(context.Background())
		}()
	}
	close(startCh)
	wg.Wait()

	bal := readBalance(t, db, day, "em.historythinking.com", "aol")
	if bal.Reserved <= effective {
		t.Fatalf("negative control did NOT over-grant (reserved=%d, effective=%d): the 100-goroutine harness cannot detect a missing FOR UPDATE, so test 1 proves nothing", bal.Reserved, effective)
	}
	t.Logf("negative control over-granted as required: reserved=%d vs effective=%d", bal.Reserved, effective)
}

// -----------------------------------------------------------------------------
// §8.2 test 2 — a duplicate idempotency key returns the existing allocation and
// consumes nothing
// -----------------------------------------------------------------------------

func TestReserve_DuplicateIdempotencyKeyConsumesNothing(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	seedDay(t, db, day, 1000, 1000)
	setBalance(t, db, day, "em.historythinking.com", "aol", 1000, 1000)
	svc := NewService(db, WithClock(midWindow(day)))

	first, err := svc.Reserve(ctx, baseReq(day, "wave-1", 25))
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if first.Granted != 25 || first.Existing {
		t.Fatalf("first reserve = %+v, want granted 25 and Existing=false", first)
	}
	afterFirst := readBalance(t, db, day, "em.historythinking.com", "aol")
	laneAfterFirst := readLane(t, db, day, "wcl_remail", "aol")

	// The re-fire: same domain|isp|lane|wave_key|versions.
	second, err := svc.Reserve(ctx, baseReq(day, "wave-1", 25))
	if err != nil {
		t.Fatalf("second reserve: %v", err)
	}
	if !second.Existing {
		t.Fatal("second reserve did not report Existing=true")
	}
	if second.AllocationID != first.AllocationID {
		t.Fatalf("second reserve returned allocation %s, want the first one %s", second.AllocationID, first.AllocationID)
	}
	if second.Granted != first.Granted {
		t.Fatalf("second reserve granted %d, want the existing allocation's outstanding %d", second.Granted, first.Granted)
	}

	afterSecond := readBalance(t, db, day, "em.historythinking.com", "aol")
	if afterSecond != afterFirst {
		t.Fatalf("duplicate CONSUMED capacity: balance %+v -> %+v", afterFirst, afterSecond)
	}
	if got := readLane(t, db, day, "wcl_remail", "aol"); got != laneAfterFirst {
		t.Fatalf("duplicate CONSUMED lane demand: %+v -> %+v", laneAfterFirst, got)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM drip_capacity_ledger`).Scan(&rows); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if rows != 1 {
		t.Fatalf("ledger has %d rows after a duplicate, want 1", rows)
	}
}

// TestReserve_NegativeControl_DifferentKeyConsumesAgain proves the assertions
// above are actually sensitive: change ONE component of the idempotency key
// (the wave key, then the contract version) and capacity IS consumed again.
// Without this, a Reserve that always returned Existing would pass test 2.
func TestReserve_NegativeControl_DifferentKeyConsumesAgain(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	seedDay(t, db, day, 1000, 1000)
	setBalance(t, db, day, "em.historythinking.com", "aol", 1000, 1000)
	svc := NewService(db, WithClock(midWindow(day)))

	if _, err := svc.Reserve(ctx, baseReq(day, "wave-1", 25)); err != nil {
		t.Fatalf("reserve 1: %v", err)
	}
	res2, err := svc.Reserve(ctx, baseReq(day, "wave-2", 25))
	if err != nil {
		t.Fatalf("reserve 2: %v", err)
	}
	if res2.Existing {
		t.Fatal("a different wave key was treated as a duplicate")
	}

	req3 := baseReq(day, "wave-1", 25)
	req3.DomainVersion = 2 // a contract change re-opens the key by design (§1.2)
	res3, err := svc.Reserve(ctx, req3)
	if err != nil {
		t.Fatalf("reserve 3: %v", err)
	}
	if res3.Existing {
		t.Fatal("a bumped domain_contract_version was treated as a duplicate")
	}

	bal := readBalance(t, db, day, "em.historythinking.com", "aol")
	if bal.Reserved != 75 {
		t.Fatalf("reserved = %d, want 75 (three distinct keys each consumed 25)", bal.Reserved)
	}
}

// -----------------------------------------------------------------------------
// §8.2 test 5 — a governor of 0 blocks reservations without changing the contract
// -----------------------------------------------------------------------------

func TestReserve_GovernorZeroBlocksWithoutTouchingContract(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	dc, _ := seedDay(t, db, day, 5000, 5000)

	if _, err := db.Exec(`INSERT INTO mailing_isp_throttle_state (isp, msgs_per_hour) VALUES ('aol', 0)`); err != nil {
		t.Fatalf("seed throttle: %v", err)
	}

	clock := dayOf(day).Add(2 * time.Hour)
	svc := NewService(db,
		WithGovernors(Governors{ThrottleGovernor{DB: db}}),
		WithClock(func() time.Time { return clock }))

	if _, err := svc.RefillDomain(ctx, day, dc); err != nil {
		t.Fatalf("refill: %v", err)
	}
	bal := readBalance(t, db, day, "em.historythinking.com", "aol")
	if bal.Effective != 0 {
		t.Fatalf("effective = %d, want 0 (throttle rate 0)", bal.Effective)
	}
	if bal.Contracted != 5000 {
		t.Fatalf("contracted = %d, want 5000 — the governor MUTATED the contract", bal.Contracted)
	}
	if dc.DailyMaxByISP["aol"] != 5000 {
		t.Fatalf("contract struct daily max = %d, want 5000 — the governor mutated the contract in memory", dc.DailyMaxByISP["aol"])
	}

	res, err := svc.Reserve(ctx, baseReq(day, "wave-1", 100))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if res.Granted != 0 {
		t.Fatalf("granted %d under a governor of 0, want 0", res.Granted)
	}
	if !strings.HasPrefix(res.BindingReason, ReasonGovernor+":") {
		t.Fatalf("binding_reason = %q, want a %q:<name> reason", res.BindingReason, ReasonGovernor)
	}
	row := readLedger(t, db, res.AllocationID)
	if row.Reserved != 0 || row.Status != StatusReleased {
		t.Fatalf("zero-grant ledger row = %+v, want reserved=0 status=released", row)
	}
	if row.Requested != 100 {
		t.Fatalf("zero-grant row lost the requested amount: %d", row.Requested)
	}
	if after := readBalance(t, db, day, "em.historythinking.com", "aol"); after.Contracted != 5000 {
		t.Fatalf("contracted changed to %d after a blocked reserve", after.Contracted)
	}
}

// TestReserve_NegativeControl_GovernorAboveZeroAllows proves the block in test 5
// came from the governor and not from a Reserve that grants 0 unconditionally.
// A governor whose ceiling is ABOVE the contract must be ignored entirely (§2.3).
func TestReserve_NegativeControl_GovernorAboveZeroAllows(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	dc, _ := seedDay(t, db, day, 5000, 5000)

	// 500 msgs/hr × 19 h window = 9,500 — above the 5,000 contract, so ignored.
	if _, err := db.Exec(`INSERT INTO mailing_isp_throttle_state (isp, msgs_per_hour) VALUES ('aol', 500)`); err != nil {
		t.Fatalf("seed throttle: %v", err)
	}
	clock := dayOf(day).Add(2 * time.Hour)
	svc := NewService(db,
		WithGovernors(Governors{ThrottleGovernor{DB: db}}),
		WithClock(func() time.Time { return clock }))

	if _, err := svc.RefillDomain(ctx, day, dc); err != nil {
		t.Fatalf("refill: %v", err)
	}
	bal := readBalance(t, db, day, "em.historythinking.com", "aol")
	if bal.Effective != 5000 {
		t.Fatalf("effective = %d, want 5000 — a governor above the contract must be ignored, never raise it", bal.Effective)
	}

	res, err := svc.Reserve(ctx, baseReq(day, "wave-1", 50))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if res.Granted <= 0 {
		t.Fatalf("granted %d with a non-blocking governor (reason=%q) — test 5's zero was not caused by the governor", res.Granted, res.BindingReason)
	}
}

// -----------------------------------------------------------------------------
// §8.2 test 6 — partial submission releases the remainder
// -----------------------------------------------------------------------------

func TestCommit_PartialSubmissionReleasesRemainder(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	seedDay(t, db, day, 1000, 1000)
	setBalance(t, db, day, "em.historythinking.com", "aol", 1000, 500)
	svc := NewService(db, WithClock(midWindow(day)))

	res, err := svc.Reserve(ctx, baseReq(day, "wave-1", 100))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if res.Granted != 100 {
		t.Fatalf("granted %d, want 100 (reason=%s)", res.Granted, res.BindingReason)
	}
	beforeCommit := readBalance(t, db, day, "em.historythinking.com", "aol")
	laneBefore := readLane(t, db, day, "wcl_remail", "aol")

	campaign := uuid.New()
	if err := svc.Commit(ctx, res.AllocationID, 60, campaign); err != nil {
		t.Fatalf("commit: %v", err)
	}

	row := readLedger(t, db, res.AllocationID)
	if row.Committed != 60 || row.Released != 40 || row.Status != StatusCommitted {
		t.Fatalf("ledger after partial commit = %+v, want committed=60 released=40 status=committed", row)
	}
	if row.CampaignID == nil || *row.CampaignID != campaign {
		t.Fatalf("ledger campaign_id = %v, want %s", row.CampaignID, campaign)
	}

	bal := readBalance(t, db, day, "em.historythinking.com", "aol")
	if bal.Reserved != 0 {
		t.Fatalf("balance.reserved = %d after commit, want 0 (the whole reservation settled)", bal.Reserved)
	}
	if bal.Committed != 60 || bal.Released != 40 {
		t.Fatalf("balance = %+v, want committed=60 released=40", bal)
	}
	if wantTokens := beforeCommit.Tokens + 40; bal.Tokens != wantTokens {
		t.Fatalf("tokens = %v after releasing 40, want %v — unsent capacity must go back to the bucket, not evaporate", bal.Tokens, wantTokens)
	}

	lane := readLane(t, db, day, "wcl_remail", "aol")
	if lane.Reserved != 0 || lane.Committed != 60 {
		t.Fatalf("lane = %+v, want reserved=0 committed=60", lane)
	}
	if want := laneBefore.Unfilled + 40; lane.Unfilled != want {
		t.Fatalf("lane.unfilled = %d, want %d — the un-submitted 40 must return to lane demand", lane.Unfilled, want)
	}

	// Double-fire: the same commit again (retry, redeploy) must change nothing.
	if err := svc.Commit(ctx, res.AllocationID, 60, campaign); err != nil {
		t.Fatalf("second commit: %v", err)
	}
	if got := readBalance(t, db, day, "em.historythinking.com", "aol"); got != bal {
		t.Fatalf("a repeated Commit double-counted: %+v -> %+v", bal, got)
	}
}

// TestCommit_NegativeControl_FullSubmissionReleasesNothing proves test 6's
// numbers come from the partial-release path: commit everything and nothing is
// handed back. A Commit that always released the full reservation would pass
// test 6's "reserved == 0" assertion but fail here.
func TestCommit_NegativeControl_FullSubmissionReleasesNothing(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	seedDay(t, db, day, 1000, 1000)
	setBalance(t, db, day, "em.historythinking.com", "aol", 1000, 500)
	svc := NewService(db, WithClock(midWindow(day)))

	res, err := svc.Reserve(ctx, baseReq(day, "wave-1", 100))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	laneBefore := readLane(t, db, day, "wcl_remail", "aol")
	balBefore := readBalance(t, db, day, "em.historythinking.com", "aol")

	if err := svc.Commit(ctx, res.AllocationID, 100, uuid.New()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	row := readLedger(t, db, res.AllocationID)
	if row.Released != 0 || row.Committed != 100 {
		t.Fatalf("full commit released %d / committed %d, want 0 / 100", row.Released, row.Committed)
	}
	bal := readBalance(t, db, day, "em.historythinking.com", "aol")
	if bal.Released != 0 {
		t.Fatalf("balance.released = %d after a full commit, want 0", bal.Released)
	}
	if bal.Tokens != balBefore.Tokens {
		t.Fatalf("tokens moved from %v to %v on a full commit — nothing should be handed back", balBefore.Tokens, bal.Tokens)
	}
	if lane := readLane(t, db, day, "wcl_remail", "aol"); lane.Unfilled != laneBefore.Unfilled {
		t.Fatalf("lane.unfilled moved from %d to %d on a full commit", laneBefore.Unfilled, lane.Unfilled)
	}
}

// -----------------------------------------------------------------------------
// §8.2 test 7 — the day boundary is deterministic
// -----------------------------------------------------------------------------

func TestDayBoundary_BalancesAreDeterministicAcrossContractVersions(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	next := day.AddDate(0, 0, 1)

	v1 := domainContract("em.historythinking.com", 1, map[string]int{"aol": 1000})
	lane1 := dispatchContract("wcl_remail", 1, map[string]int{"aol": 1000})
	if _, err := EnsureDayBalances(ctx, db, day, activeSet(day, []*DomainContract{v1}, []*DispatchContract{lane1})); err != nil {
		t.Fatalf("ensure day D: %v", err)
	}
	setBalance(t, db, day, "em.historythinking.com", "aol", 1000, 1000)

	svc := NewService(db, WithClock(midWindow(day)))
	if _, err := svc.Reserve(ctx, baseReq(day, "wave-1", 40)); err != nil {
		t.Fatalf("reserve on D: %v", err)
	}
	mid := readBalance(t, db, day, "em.historythinking.com", "aol")
	if mid.Reserved != 40 {
		t.Fatalf("reserved on D = %d, want 40", mid.Reserved)
	}

	// Re-running the seeder mid-day is safe: this is the DO NOTHING guard, and
	// it is what stops an ECS bounce at 14:00 from handing the day back.
	if _, err := EnsureDayBalances(ctx, db, day, activeSet(day, []*DomainContract{v1}, []*DispatchContract{lane1})); err != nil {
		t.Fatalf("re-ensure day D: %v", err)
	}
	if got := readBalance(t, db, day, "em.historythinking.com", "aol"); got.Reserved != 40 {
		t.Fatalf("re-seeding day D RESET reserved from 40 to %d — the ON CONFLICT DO NOTHING guard is gone", got.Reserved)
	}

	// Tomorrow's contract takes effect on tomorrow's rows only.
	v2 := domainContract("em.historythinking.com", 2, map[string]int{"aol": 2000})
	lane2 := dispatchContract("wcl_remail", 2, map[string]int{"aol": 2000})
	if _, err := EnsureDayBalances(ctx, db, next, activeSet(next, []*DomainContract{v2}, []*DispatchContract{lane2})); err != nil {
		t.Fatalf("ensure day D+1: %v", err)
	}
	if got := readBalance(t, db, next, "em.historythinking.com", "aol"); got.Contracted != 2000 {
		t.Fatalf("D+1 contracted = %d, want 2000", got.Contracted)
	}
	if got := readBalance(t, db, day, "em.historythinking.com", "aol"); got.Contracted != 1000 || got.Reserved != 40 {
		t.Fatalf("seeding D+1 mutated D: %+v, want contracted=1000 reserved=40", got)
	}

	// The same wave key on the new day and version is a NEW allocation: the day
	// and the versions are both in the idempotency key.
	req := baseReq(next, "wave-1", 40)
	req.DomainVersion, req.DispatchVersion = 2, 2
	svcNext := NewService(db, WithClock(midWindow(next)))
	res, err := svcNext.Reserve(ctx, req)
	if err != nil {
		t.Fatalf("reserve on D+1: %v", err)
	}
	if res.Existing {
		t.Fatal("D+1's wave-1 was treated as a duplicate of D's wave-1")
	}
	if got := readBalance(t, db, day, "em.historythinking.com", "aol"); got.Reserved != 40 {
		t.Fatalf("a D+1 reservation consumed D's balance: reserved = %d", got.Reserved)
	}
}

// -----------------------------------------------------------------------------
// ExpireStale — the crash-window recovery
// -----------------------------------------------------------------------------

func TestExpireStale_ReleasesStaleReservationsAndReturnsTokens(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	seedDay(t, db, day, 1000, 1000)
	setBalance(t, db, day, "em.historythinking.com", "aol", 1000, 500)
	svc := NewService(db, WithClock(midWindow(day)))

	stale, err := svc.Reserve(ctx, baseReq(day, "stale", 100))
	if err != nil {
		t.Fatalf("reserve stale: %v", err)
	}
	fresh, err := svc.Reserve(ctx, baseReq(day, "fresh", 100))
	if err != nil {
		t.Fatalf("reserve fresh: %v", err)
	}
	// Age only the first one — this is the ECS bounce between Reserve and Commit.
	if _, err := db.Exec(`UPDATE drip_capacity_ledger SET created_at = NOW() - INTERVAL '90 minutes' WHERE allocation_id = $1`, stale.AllocationID); err != nil {
		t.Fatalf("age row: %v", err)
	}
	beforeTokens := readBalance(t, db, day, "em.historythinking.com", "aol").Tokens

	n, err := svc.ExpireStale(ctx, 45*time.Minute)
	if err != nil {
		t.Fatalf("ExpireStale: %v", err)
	}
	if n != 1 {
		t.Fatalf("ExpireStale released %d, want exactly 1", n)
	}
	if row := readLedger(t, db, stale.AllocationID); row.Status != StatusExpired || row.Released != 100 {
		t.Fatalf("stale row = %+v, want status=expired released=100", row)
	}
	// The negative control lives inside this test: a reservation younger than the
	// cutoff must be untouched, or ExpireStale is just cancelling live work —
	// the janitor incident this package exists to not repeat.
	if row := readLedger(t, db, fresh.AllocationID); row.Status != StatusReserved || row.Released != 0 {
		t.Fatalf("FRESH reservation was expired: %+v", row)
	}
	bal := readBalance(t, db, day, "em.historythinking.com", "aol")
	if bal.Reserved != 100 {
		t.Fatalf("balance.reserved = %d, want 100 (only the fresh reservation)", bal.Reserved)
	}
	if bal.Tokens != beforeTokens+100 {
		t.Fatalf("tokens = %v, want %v — an expired reservation must return its tokens", bal.Tokens, beforeTokens+100)
	}
}

// -----------------------------------------------------------------------------
// Timeout — a wedged reserve grants 0, never an error, and never burns the key
// -----------------------------------------------------------------------------

func TestReserve_StatementTimeoutGrantsZeroAndStaysRetryable(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	seedDay(t, db, day, 1000, 1000)
	setBalance(t, db, day, "em.historythinking.com", "aol", 1000, 1000)

	// Hold the domain balance row from another connection so the reservation's
	// FOR UPDATE has to wait past its budget.
	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	if _, err := blocker.ExecContext(ctx, `
		SELECT 1 FROM drip_capacity_balance
		WHERE day = $1::date AND sending_domain = $2 AND isp = $3 FOR UPDATE
	`, dayKey(day), "em.historythinking.com", "aol"); err != nil {
		t.Fatalf("blocker lock: %v", err)
	}

	svc := NewService(db, WithClock(midWindow(day)), WithStatementTimeout("300ms"))
	res, err := svc.Reserve(ctx, baseReq(day, "wave-1", 25))
	if err != nil {
		t.Fatalf("a timed-out reserve returned an error to the caller: %v", err)
	}
	if res.Granted != 0 || res.BindingReason != ReasonReserveTimeout {
		t.Fatalf("timed-out reserve = %+v, want granted=0 reason=%s", res, ReasonReserveTimeout)
	}
	if svc.TimeoutCount() != 1 {
		t.Fatalf("TimeoutCount = %d, want 1 — a wedged reserve must be countable", svc.TimeoutCount())
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM drip_capacity_ledger`).Scan(&rows); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if rows != 0 {
		t.Fatalf("a timeout wrote %d ledger row(s): the idempotency key is now burned and this wave can never be retried", rows)
	}

	_ = blocker.Rollback()

	// Negative control: with the lock released the SAME key succeeds, proving
	// the timeout left it retryable.
	retry, err := svc.Reserve(ctx, baseReq(day, "wave-1", 25))
	if err != nil {
		t.Fatalf("retry after timeout: %v", err)
	}
	if retry.Granted != 25 || retry.Existing {
		t.Fatalf("retry after timeout = %+v, want a fresh grant of 25", retry)
	}
}

// -----------------------------------------------------------------------------
// Binding reasons and the other min() terms
// -----------------------------------------------------------------------------

func TestReserve_BindingReasonNamesTheZeroTerm(t *testing.T) {
	day := testDay(t)
	cases := []struct {
		name       string
		setup      func(t *testing.T, db *sql.DB)
		mutate     func(r *ReserveReq)
		wantReason string
	}{
		{
			name:       "lane demand exhausted",
			setup:      func(t *testing.T, db *sql.DB) { setLaneUnfilled(t, db, day, "wcl_remail", "aol", 0) },
			wantReason: ReasonLaneDemand,
		},
		{
			name:       "no mailable supply",
			mutate:     func(r *ReserveReq) { r.MailableSupply = 0 },
			wantReason: ReasonSupply,
		},
		{
			name:       "tokens exhausted",
			setup:      func(t *testing.T, db *sql.DB) { setBalance(t, db, day, "em.historythinking.com", "aol", 1000, 0) },
			wantReason: ReasonDomainTokens,
		},
		{
			name: "outside the active window",
			mutate: func(r *ReserveReq) {
				r.Win = Window{Start: time.Hour, End: 2 * time.Hour, Interval: 15 * time.Minute, MaxBurstIntervals: 2}
			},
			wantReason: ReasonOutsideWindow,
		},
		{
			name:       "demand satisfied",
			wantReason: ReasonRequested,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			ctx := context.Background()
			seedDay(t, db, day, 1000, 1000)
			setBalance(t, db, day, "em.historythinking.com", "aol", 1000, 1000)
			if tc.setup != nil {
				tc.setup(t, db)
			}
			svc := NewService(db, WithClock(midWindow(day)))
			req := baseReq(day, "wave-1", 25)
			if tc.mutate != nil {
				tc.mutate(&req)
			}
			res, err := svc.Reserve(ctx, req)
			if err != nil {
				t.Fatalf("reserve: %v", err)
			}
			if res.BindingReason != tc.wantReason {
				t.Fatalf("binding_reason = %q, want %q (granted=%d)", res.BindingReason, tc.wantReason, res.Granted)
			}
			row := readLedger(t, db, res.AllocationID)
			if row.Reason != tc.wantReason {
				t.Fatalf("ledger binding_reason = %q, want %q", row.Reason, tc.wantReason)
			}
			if tc.wantReason != ReasonRequested {
				if res.Granted != 0 || row.Status != StatusReleased || row.Reserved != 0 {
					t.Fatalf("a bound-to-zero reserve wrote %+v (granted=%d), want reserved=0 status=released", row, res.Granted)
				}
			}
		})
	}
}

// TestReserve_MissingBalanceFailsClosed pins the fail-closed behaviour: a
// domain×ISP with no balance row grants nothing and says so, rather than
// defaulting to the contract.
func TestReserve_MissingBalanceFailsClosed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	seedDay(t, db, day, 1000, 1000)
	svc := NewService(db, WithClock(midWindow(day)))

	req := baseReq(day, "wave-1", 25)
	req.ISP = "yahoo" // never seeded
	res, err := svc.Reserve(ctx, req)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if res.Granted != 0 || res.BindingReason != ReasonNoBalance {
		t.Fatalf("reserve on an unseeded ISP = %+v, want granted=0 reason=%s", res, ReasonNoBalance)
	}

	req2 := baseReq(day, "wave-2", 25)
	req2.Lane = "not_a_lane"
	res2, err := svc.Reserve(ctx, req2)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if res2.Granted != 0 || res2.BindingReason != ReasonNoLaneBalance {
		t.Fatalf("reserve on an unseeded lane = %+v, want granted=0 reason=%s", res2, ReasonNoLaneBalance)
	}
}

// TestRebuildFromLedger_RestoresBalancesFromTheLedger covers the midnight/
// on-demand rebuild: corrupt the balance counters, rebuild, and the ledger wins.
func TestRebuildFromLedger_RestoresBalancesFromTheLedger(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	seedDay(t, db, day, 1000, 1000)
	setBalance(t, db, day, "em.historythinking.com", "aol", 1000, 500)
	svc := NewService(db, WithClock(midWindow(day)))

	a, err := svc.Reserve(ctx, baseReq(day, "w1", 100))
	if err != nil {
		t.Fatalf("reserve a: %v", err)
	}
	b, err := svc.Reserve(ctx, baseReq(day, "w2", 100))
	if err != nil {
		t.Fatalf("reserve b: %v", err)
	}
	if err := svc.Commit(ctx, a.AllocationID, 80, uuid.New()); err != nil {
		t.Fatalf("commit a: %v", err)
	}
	want := readBalance(t, db, day, "em.historythinking.com", "aol")

	// Corrupt the derived counters the way a lost update or a bad manual fix would.
	if _, err := db.Exec(`UPDATE drip_capacity_balance SET reserved = 999, committed = 0, released = 0`); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if _, err := RebuildFromLedger(ctx, db, day); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	got := readBalance(t, db, day, "em.historythinking.com", "aol")
	if got.Reserved != want.Reserved || got.Committed != want.Committed || got.Released != want.Released {
		t.Fatalf("rebuild gave %+v, want reserved=%d committed=%d released=%d", got, want.Reserved, want.Committed, want.Released)
	}
	if got.Reserved != 100 {
		t.Fatalf("rebuilt reserved = %d, want 100 (allocation %s is still outstanding)", got.Reserved, b.AllocationID)
	}
}

// TestReserve_RejectsAnInvalidTouchClass keeps the ledger's
// CHECK (touch_class IN ('intro','followup','remail')) from surfacing as a raw
// 23514 from inside the reservation transaction.
func TestReserve_RejectsAnInvalidTouchClass(t *testing.T) {
	db := newTestDB(t)
	day := testDay(t)
	seedDay(t, db, day, 1000, 1000)
	setBalance(t, db, day, "em.historythinking.com", "aol", 1000, 1000)
	svc := NewService(db, WithClock(midWindow(day)))

	req := baseReq(day, "wave-1", 25)
	req.TouchClass = "welcome"
	if _, err := svc.Reserve(context.Background(), req); err == nil {
		t.Fatal("Reserve accepted touch_class 'welcome'")
	} else if !strings.Contains(err.Error(), "TouchClass") {
		t.Fatalf("error %q does not name the offending field", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM drip_capacity_ledger`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("a rejected request still wrote %d ledger row(s)", n)
	}
	// Negative control: the three legal classes are accepted and all draw on the
	// SAME domain×ISP balance (§8.2 test 10's precondition).
	for i, tc := range TouchClasses {
		ok := baseReq(day, fmt.Sprintf("wave-ok-%d", i), 10)
		ok.TouchClass = tc
		if res, err := svc.Reserve(context.Background(), ok); err != nil {
			t.Fatalf("touch_class %q rejected: %v", tc, err)
		} else if res.Granted != 10 {
			t.Fatalf("touch_class %q granted %d, want 10", tc, res.Granted)
		}
	}
	if bal := readBalance(t, db, day, "em.historythinking.com", "aol"); bal.Reserved != 30 {
		t.Fatalf("reserved = %d, want 30 — intros, follow-ups and remails must share one balance", bal.Reserved)
	}
}

// -----------------------------------------------------------------------------
// WP1 follow-through (1): effective_reason is PERSISTED, and Reserve reads the
// label off the row rather than out of process memory.
// -----------------------------------------------------------------------------

func TestEffectiveReason_PersistedAndReadByAnyInstance(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	dc, _ := seedDay(t, db, day, 5000, 5000)

	// A freshly seeded day has no governor opinion yet.
	if got := readEffectiveReason(t, db, day, "em.historythinking.com", "aol"); got != "" {
		t.Fatalf("seeded effective_reason = %q, want empty", got)
	}

	if _, err := db.Exec(`INSERT INTO mailing_isp_throttle_state (isp, msgs_per_hour) VALUES ('aol', 0)`); err != nil {
		t.Fatalf("seed throttle: %v", err)
	}
	clock := func() time.Time { return dayOf(day).Add(2 * time.Hour) }
	refiller := NewService(db, WithGovernors(Governors{ThrottleGovernor{DB: db}}), WithClock(clock))
	if _, err := refiller.RefillDomain(ctx, day, dc); err != nil {
		t.Fatalf("refill: %v", err)
	}
	if got := readEffectiveReason(t, db, day, "em.historythinking.com", "aol"); got != "throttle" {
		t.Fatalf("effective_reason = %q after a governor bound, want %q", got, "throttle")
	}

	// THE POINT: a DIFFERENT Service — a second ECS instance, or the §3 API —
	// that never ran RefillDomain and holds no cache must report the same label.
	// Before the column existed this instance said "governor:reduced".
	other := NewService(db, WithClock(clock))
	res, err := other.Reserve(ctx, baseReq(day, "wave-1", 100))
	if err != nil {
		t.Fatalf("reserve from the second instance: %v", err)
	}
	if res.Granted != 0 || res.BindingReason != ReasonGovernor+":throttle" {
		t.Fatalf("second instance reported %+v, want granted=0 reason=%s:throttle — the label is not coming off the row", res, ReasonGovernor)
	}
	if row := readLedger(t, db, res.AllocationID); row.Reason != ReasonGovernor+":throttle" {
		t.Fatalf("ledger binding_reason = %q, want %s:throttle", row.Reason, ReasonGovernor)
	}
	if bal := readBalance(t, db, day, "em.historythinking.com", "aol"); bal.Contracted != 5000 {
		t.Fatalf("contracted = %d — the governor mutated the contract", bal.Contracted)
	}
}

// TestEffectiveReason_NegativeControl_ClearsWhenNoGovernorBinds proves the label
// is written per refill and not sticky: lift the throttle, refill again, and
// effective_reason must go back to empty with the reason no longer a governor.
// A RefillDomain that only ever SET the reason would pass the test above.
func TestEffectiveReason_NegativeControl_ClearsWhenNoGovernorBinds(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	dc, _ := seedDay(t, db, day, 5000, 5000)

	if _, err := db.Exec(`INSERT INTO mailing_isp_throttle_state (isp, msgs_per_hour) VALUES ('aol', 0)`); err != nil {
		t.Fatalf("seed throttle: %v", err)
	}
	at := dayOf(day).Add(2 * time.Hour)
	svc := NewService(db, WithGovernors(Governors{ThrottleGovernor{DB: db}}), WithClock(func() time.Time { return at }))
	if _, err := svc.RefillDomain(ctx, day, dc); err != nil {
		t.Fatalf("refill 1: %v", err)
	}
	if got := readEffectiveReason(t, db, day, "em.historythinking.com", "aol"); got != "throttle" {
		t.Fatalf("effective_reason = %q, want throttle", got)
	}

	// The pipe recovers.
	if _, err := db.Exec(`DELETE FROM mailing_isp_throttle_state WHERE isp = 'aol'`); err != nil {
		t.Fatalf("clear throttle: %v", err)
	}
	at = dayOf(day).Add(4 * time.Hour)
	if _, err := svc.RefillDomain(ctx, day, dc); err != nil {
		t.Fatalf("refill 2: %v", err)
	}
	if got := readEffectiveReason(t, db, day, "em.historythinking.com", "aol"); got != "" {
		t.Fatalf("effective_reason = %q after the governor lifted, want empty — the label is sticky", got)
	}
	res, err := svc.Reserve(ctx, baseReq(day, "wave-1", 50))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if strings.HasPrefix(res.BindingReason, ReasonGovernor+":") {
		t.Fatalf("binding_reason = %q with no governor in force", res.BindingReason)
	}
	if res.Granted <= 0 {
		t.Fatalf("granted %d after the governor lifted (reason=%q)", res.Granted, res.BindingReason)
	}
}

// -----------------------------------------------------------------------------
// WP1 follow-through (2): release_reason is written by Release() and
// ExpireStale(); binding_reason is never touched.
// -----------------------------------------------------------------------------

func TestReleaseReason_WrittenWithoutTouchingBindingReason(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	seedDay(t, db, day, 1000, 1000)
	setBalance(t, db, day, "em.historythinking.com", "aol", 1000, 500)
	svc := NewService(db, WithClock(midWindow(day)))

	res, err := svc.Reserve(ctx, baseReq(day, "wave-1", 100))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	granted := readLedger(t, db, res.AllocationID)
	if granted.ReleaseReason.Valid && granted.ReleaseReason.String != "" {
		t.Fatalf("a fresh reservation already carries release_reason %q", granted.ReleaseReason.String)
	}

	if err := svc.Release(ctx, res.AllocationID, 40, "wave_group_failed"); err != nil {
		t.Fatalf("release: %v", err)
	}
	row := readLedger(t, db, res.AllocationID)
	if !row.ReleaseReason.Valid || row.ReleaseReason.String != "wave_group_failed" {
		t.Fatalf("release_reason = %v, want %q", row.ReleaseReason, "wave_group_failed")
	}
	if row.Reason != granted.Reason {
		t.Fatalf("binding_reason changed from %q to %q — the grant's record was overwritten", granted.Reason, row.Reason)
	}
	if row.Released != 40 || row.Status != StatusReserved {
		t.Fatalf("partial release gave %+v, want released=40 still reserved", row)
	}

	// Successive partial releases append rather than clobber, so the ledger keeps
	// the whole story of a wave that failed in pieces.
	if err := svc.Release(ctx, res.AllocationID, 60, "deploy_cancelled"); err != nil {
		t.Fatalf("second release: %v", err)
	}
	row2 := readLedger(t, db, res.AllocationID)
	if !strings.Contains(row2.ReleaseReason.String, "wave_group_failed") || !strings.Contains(row2.ReleaseReason.String, "deploy_cancelled") {
		t.Fatalf("release_reason = %q, want both reasons retained", row2.ReleaseReason.String)
	}
	if row2.Reason != granted.Reason {
		t.Fatalf("binding_reason changed to %q after the second release", row2.Reason)
	}
	if row2.Status != StatusReleased {
		t.Fatalf("status = %q after the whole reservation was released, want %q", row2.Status, StatusReleased)
	}
}

// TestReleaseReason_NegativeControl_NotWrittenWhenNothingIsReleased proves the
// column is driven by an actual give-back and is not stamped on every settle:
// a full commit releases nothing and must leave release_reason NULL.
func TestReleaseReason_NegativeControl_NotWrittenWhenNothingIsReleased(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	seedDay(t, db, day, 1000, 1000)
	setBalance(t, db, day, "em.historythinking.com", "aol", 1000, 500)
	svc := NewService(db, WithClock(midWindow(day)))

	full, err := svc.Reserve(ctx, baseReq(day, "full", 100))
	if err != nil {
		t.Fatalf("reserve full: %v", err)
	}
	if err := svc.Commit(ctx, full.AllocationID, 100, uuid.New()); err != nil {
		t.Fatalf("commit full: %v", err)
	}
	if row := readLedger(t, db, full.AllocationID); row.ReleaseReason.Valid && row.ReleaseReason.String != "" {
		t.Fatalf("a full commit stamped release_reason %q — the column is being written unconditionally", row.ReleaseReason.String)
	}

	// A PARTIAL commit does hand capacity back, and says so.
	part, err := svc.Reserve(ctx, baseReq(day, "part", 100))
	if err != nil {
		t.Fatalf("reserve part: %v", err)
	}
	before := readLedger(t, db, part.AllocationID)
	if err := svc.Commit(ctx, part.AllocationID, 60, uuid.New()); err != nil {
		t.Fatalf("commit part: %v", err)
	}
	row := readLedger(t, db, part.AllocationID)
	if !row.ReleaseReason.Valid || row.ReleaseReason.String != "partial_commit" {
		t.Fatalf("partial commit release_reason = %v, want %q", row.ReleaseReason, "partial_commit")
	}
	if row.Reason != before.Reason {
		t.Fatalf("binding_reason changed from %q to %q on a partial commit", before.Reason, row.Reason)
	}
}

func TestExpireStale_StampsExpireStaleAsTheReleaseReason(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	day := testDay(t)
	seedDay(t, db, day, 1000, 1000)
	setBalance(t, db, day, "em.historythinking.com", "aol", 1000, 500)
	svc := NewService(db, WithClock(midWindow(day)))

	stale, err := svc.Reserve(ctx, baseReq(day, "stale", 100))
	if err != nil {
		t.Fatalf("reserve stale: %v", err)
	}
	fresh, err := svc.Reserve(ctx, baseReq(day, "fresh", 100))
	if err != nil {
		t.Fatalf("reserve fresh: %v", err)
	}
	beforeStale := readLedger(t, db, stale.AllocationID)
	if _, err := db.Exec(`UPDATE drip_capacity_ledger SET created_at = NOW() - INTERVAL '90 minutes' WHERE allocation_id = $1`, stale.AllocationID); err != nil {
		t.Fatalf("age row: %v", err)
	}
	if _, err := svc.ExpireStale(ctx, 45*time.Minute); err != nil {
		t.Fatalf("ExpireStale: %v", err)
	}

	row := readLedger(t, db, stale.AllocationID)
	if !row.ReleaseReason.Valid || row.ReleaseReason.String != "expire_stale" {
		t.Fatalf("expired row release_reason = %v, want %q", row.ReleaseReason, "expire_stale")
	}
	if row.Reason != beforeStale.Reason {
		t.Fatalf("binding_reason changed from %q to %q on expiry", beforeStale.Reason, row.Reason)
	}
	// Negative control: the reservation inside the cutoff is untouched — no
	// status change and, critically, no release_reason. A sweep that stamped
	// every reserved row would look identical on the stale row alone.
	if freshRow := readLedger(t, db, fresh.AllocationID); freshRow.ReleaseReason.Valid && freshRow.ReleaseReason.String != "" {
		t.Fatalf("the FRESH reservation was stamped release_reason %q", freshRow.ReleaseReason.String)
	} else if freshRow.Status != StatusReserved {
		t.Fatalf("the FRESH reservation moved to %q", freshRow.Status)
	}
}
