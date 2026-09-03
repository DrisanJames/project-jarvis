package dripsupply

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// -----------------------------------------------------------------------------
// Harness
// -----------------------------------------------------------------------------
//
// Same reasoning as reservation_test.go: the contract under test is CHECK
// enforcement, FOR UPDATE SKIP LOCKED, a window function and Postgres interval
// arithmetic. sqlmock evaluates none of that and would happily pass a claim SQL
// with the cap CTE deleted.
//
// The helpers below are deliberately NOT the ones in reservation_test.go: that
// file is owned by another work package and is being edited concurrently, and
// these tests need a different schema (partner_clean_queue + its REQ-118
// constraint) and a per-test choice of fence literal. Same pattern, own names,
// no shared symbol to collide on.

const (
	pcqTestAdminDSNEnv = "DRIPSUPPLY_TEST_ADMIN_DSN"
	pcqDefaultAdminDSN = "postgres://apex_user:apex_password@localhost:5432/apex_db?sslmode=disable"
	pcqScratchDBName   = "req118_txn"
)

// pastFence is the fence literal rewritten into the TEST schema only, so
// enforcement is actually exercised. Production ships PCQAllocationFence
// (2099) — see TestPCQFenceIsFarFuture for why that is not an oversight.
const pastFence = "2020-01-01 00:00:00+00"

func pcqAdminDSN() string {
	if v := strings.TrimSpace(os.Getenv(pcqTestAdminDSNEnv)); v != "" {
		return v
	}
	return pcqDefaultAdminDSN
}

func pcqScratchDSN(t *testing.T) string {
	t.Helper()
	dsn := pcqAdminDSN()
	i := strings.LastIndex(dsn, "/")
	if i < 0 {
		t.Skipf("cannot derive a scratch DSN from %q", dsn)
	}
	tail := dsn[i+1:]
	q := ""
	if j := strings.Index(tail, "?"); j >= 0 {
		q = tail[j:]
	}
	return dsn[:i+1] + pcqScratchDBName + q
}

func ensurePCQScratchDB(t *testing.T) {
	t.Helper()
	admin, err := sql.Open("postgres", pcqAdminDSN())
	if err != nil {
		t.Skipf("cannot open admin DSN: %v", err)
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Skipf("local postgres unreachable (%v) — set %s to run the dripsupply integration tests", err, pcqTestAdminDSNEnv)
	}
	var exists bool
	if err := admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, pcqScratchDBName).Scan(&exists); err != nil {
		t.Skipf("cannot list databases: %v", err)
	}
	if exists {
		return
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+pcqScratchDBName); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		t.Skipf("cannot create scratch database %s: %v", pcqScratchDBName, err)
	}
}

// pcqSchemaDDL is the production partner_clean_queue shape, trimmed to the
// columns the transition path reads or writes, plus partner_datasets (the
// datasetNotEmergencyPausedSQL predicate's correlated table). Column names,
// types, nullability and defaults are taken from cmd/server/main.go:8945
// (dp_create_partner_clean_queue) and the jun11c_pcq_* / req118_pcq_* ALTERs at
// main.go:9899-9905 and main.go:2414-2415. The foreign keys are dropped so a
// fixture does not need a partner/batch graph; nothing under test reads them.
func pcqSchemaDDL(fence string) []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS partner_datasets (
			id               UUID PRIMARY KEY,
			slug             TEXT,
			vertical         TEXT,
			paused_emergency BOOLEAN NOT NULL DEFAULT FALSE
		)`,
		`CREATE TABLE IF NOT EXISTS partner_clean_queue (
			id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			batch_id               UUID,
			dataset_id             UUID NOT NULL,
			partner_id             UUID,
			vertical               TEXT NOT NULL,
			email                  TEXT NOT NULL,
			email_md5              VARCHAR(32) NOT NULL,
			isp_family             TEXT NOT NULL DEFAULT 'other',
			status                 TEXT NOT NULL DEFAULT 'pending_eo',
			mailed_campaign_id     UUID,
			ingested_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			claimed_at             TIMESTAMPTZ,
			mailed_at              TIMESTAMPTZ,
			extra_metadata         JSONB NOT NULL DEFAULT '{}'::jsonb,
			touch_count            INTEGER NOT NULL DEFAULT 0,
			last_touch_campaign_id UUID,
			subscriber_id          UUID,
			capacity_allocation_id UUID,
			supply_reservation_id  UUID
		)`,
		PCQClaimConstraintDDL(fence),
	}
}

// newPCQTestDB builds a per-test schema carrying partner_clean_queue with the
// REQ-118 constraint at the given fence. The schema is selected with a
// CONNECTION parameter, not `SET search_path`, for the reason reservation_test
// documents: database/sql hands out arbitrary pooled connections.
func newPCQTestDB(t *testing.T, fence string) *sql.DB {
	t.Helper()
	ensurePCQScratchDB(t)
	schema := "t" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")

	bootstrap, err := sql.Open("postgres", pcqScratchDSN(t))
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

	dsn := pcqScratchDSN(t)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := sql.Open("postgres", dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatalf("open scratch: %v", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(10)
	t.Cleanup(func() {
		db.Close()
		clean, err := sql.Open("postgres", pcqScratchDSN(t))
		if err == nil {
			_, _ = clean.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
			clean.Close()
		}
	})
	for _, stmt := range pcqSchemaDDL(fence) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("ddl %.60s: %v", strings.ReplaceAll(stmt, "\n", " "), err)
		}
	}
	return db
}

// -----------------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------------

type pcqRow struct {
	ISP          string
	Status       string
	IngestedAgo  time.Duration
	ClaimedAgo   time.Duration // 0 = NULL
	Subscriber   bool
	LastTouch    bool
	MailedCampID bool
	TouchCount   int
	Allocation   uuid.UUID
	HomeBrand    string
}

func mkDataset(t *testing.T, db *sql.DB, paused bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := db.Exec(
		`INSERT INTO partner_datasets (id, slug, vertical, paused_emergency) VALUES ($1,$2,$3,$4)`,
		id, "src-"+id.String()[:8], "wcl_remail", paused); err != nil {
		t.Fatalf("insert dataset: %v", err)
	}
	return id
}

func insertRow(t *testing.T, db *sql.DB, dataset uuid.UUID, vertical string, r pcqRow) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var claimedAt, lastTouch, mailedCamp, subID, alloc any
	if r.ClaimedAgo > 0 {
		claimedAt = time.Now().UTC().Add(-r.ClaimedAgo)
	}
	if r.LastTouch {
		lastTouch = uuid.New()
	}
	if r.MailedCampID {
		mailedCamp = uuid.New()
	}
	if r.Subscriber {
		subID = uuid.New()
	}
	if r.Allocation != uuid.Nil {
		alloc = r.Allocation
	}
	extra := `{}`
	if r.HomeBrand != "" {
		extra = fmt.Sprintf(`{"home_brand":%q}`, r.HomeBrand)
	}
	status := r.Status
	if status == "" {
		status = "ready"
	}
	isp := r.ISP
	if isp == "" {
		isp = "other"
	}
	_, err := db.Exec(`
		INSERT INTO partner_clean_queue
		  (id, batch_id, dataset_id, partner_id, vertical, email, email_md5, isp_family,
		   status, ingested_at, claimed_at, last_touch_campaign_id, mailed_campaign_id,
		   subscriber_id, touch_count, capacity_allocation_id, extra_metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9, NOW() - make_interval(secs => $10::double precision),
		        $11,$12,$13,$14,$15,$16,$17::jsonb)`,
		id, uuid.New(), dataset, uuid.New(), vertical,
		id.String()[:8]+"@example.com", strings.ReplaceAll(id.String(), "-", "")[:32], isp,
		status, r.IngestedAgo.Seconds(), claimedAt, lastTouch, mailedCamp, subID,
		r.TouchCount, alloc, extra)
	if err != nil {
		t.Fatalf("insert pcq row: %v", err)
	}
	return id
}

func rowState(t *testing.T, db *sql.DB, id uuid.UUID) (status string, claimedAt sql.NullTime, alloc sql.NullString) {
	t.Helper()
	if err := db.QueryRow(
		`SELECT status, claimed_at, capacity_allocation_id::text FROM partner_clean_queue WHERE id = $1`,
		id).Scan(&status, &claimedAt, &alloc); err != nil {
		t.Fatalf("read row %s: %v", id, err)
	}
	return
}

func countStatus(t *testing.T, db *sql.DB, status string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM partner_clean_queue WHERE status = $1`, status).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", status, err)
	}
	return n
}

// -----------------------------------------------------------------------------
// §8.2 test 8 — no row enters 'claimed' without an allocation
// -----------------------------------------------------------------------------

// TestClaimRefusesZeroAllocation is the Go half: the refusal happens BEFORE any
// statement, so the constraint never has to catch it.
func TestClaimRefusesZeroAllocation(t *testing.T) {
	db := newPCQTestDB(t, pastFence)
	ds := mkDataset(t, db, false)
	id := insertRow(t, db, ds, "wcl_remail", pcqRow{ISP: "aol"})

	tr := NewTransitions()
	n, err := tr.Claim(context.Background(), db, []uuid.UUID{id}, uuid.Nil)
	if !errors.Is(err, ErrNoAllocation) {
		t.Fatalf("Claim(uuid.Nil) error = %v, want ErrNoAllocation", err)
	}
	if n != 0 {
		t.Errorf("Claim(uuid.Nil) claimed %d rows, want 0", n)
	}
	if status, _, _ := rowState(t, db, id); status != "ready" {
		t.Errorf("row status = %q after a refused claim, want ready", status)
	}

	// Same refusal on the caps path, and again with no statement issued.
	if _, err := tr.ClaimByISPCaps(context.Background(), db, "wcl_remail", "ht",
		map[string]int{"aol": 10}, 100, uuid.Nil); !errors.Is(err, ErrNoAllocation) {
		t.Fatalf("ClaimByISPCaps(uuid.Nil) error = %v, want ErrNoAllocation", err)
	}
	if status, _, _ := rowState(t, db, id); status != "ready" {
		t.Errorf("row status = %q after a refused caps claim, want ready", status)
	}
}

// TestClaimFenceEnforcedWhenFencePassed is §8.2 test 8. With the fence in the
// past (the post-cutover state), a direct UPDATE to 'claimed' without an
// allocation is REJECTED by the database, and the same claim through
// Transitions.Claim — which carries an allocation — succeeds.
func TestClaimFenceEnforcedWhenFencePassed(t *testing.T) {
	db := newPCQTestDB(t, pastFence)
	ds := mkDataset(t, db, false)
	bare := insertRow(t, db, ds, "wcl_remail", pcqRow{ISP: "aol"})
	good := insertRow(t, db, ds, "wcl_remail", pcqRow{ISP: "aol"})

	// The bypass every old job used to do.
	_, err := db.Exec(
		`UPDATE partner_clean_queue SET status='claimed', claimed_at=NOW() WHERE id = $1`, bare)
	if err == nil {
		t.Fatal("unallocated claim was ACCEPTED with the fence in the past — pcq_claim_requires_allocation is not enforcing")
	}
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) || string(pqErr.Code) != "23514" {
		t.Fatalf("unallocated claim failed with %v, want a 23514 check_violation", err)
	}
	if pqErr.Constraint != "pcq_claim_requires_allocation" {
		t.Errorf("violated constraint = %q, want pcq_claim_requires_allocation", pqErr.Constraint)
	}
	if status, _, _ := rowState(t, db, bare); status != "ready" {
		t.Errorf("row status = %q after the rejected UPDATE, want ready", status)
	}

	// The sanctioned path.
	alloc := uuid.New()
	n, err := NewTransitions().Claim(context.Background(), db, []uuid.UUID{good}, alloc)
	if err != nil {
		t.Fatalf("Claim with allocation: %v", err)
	}
	if n != 1 {
		t.Fatalf("Claim moved %d rows, want 1", n)
	}
	status, claimedAt, gotAlloc := rowState(t, db, good)
	if status != "claimed" {
		t.Errorf("status = %q, want claimed", status)
	}
	if !claimedAt.Valid {
		t.Error("claimed_at is NULL after a claim")
	}
	if !gotAlloc.Valid || gotAlloc.String != alloc.String() {
		t.Errorf("capacity_allocation_id = %v, want %s", gotAlloc, alloc)
	}
}

// TestClaimFenceInFutureAcceptsLegacyClaim is test 8's NEGATIVE CONTROL and the
// reason PCQAllocationFence ships at 2099: a NOT VALID CHECK still applies to
// every new and updated row, so with the fence in the past the LEGACY claim path
// would break the moment the binary boots with DRIP_SUPPLY_CHAIN_MODE=off. With
// the shipped fence the same unallocated UPDATE is accepted.
func TestClaimFenceInFutureAcceptsLegacyClaim(t *testing.T) {
	db := newPCQTestDB(t, PCQAllocationFence)
	ds := mkDataset(t, db, false)
	legacy := insertRow(t, db, ds, "wcl_remail", pcqRow{ISP: "aol"})

	if _, err := db.Exec(
		`UPDATE partner_clean_queue SET status='claimed', claimed_at=NOW() WHERE id = $1`, legacy); err != nil {
		t.Fatalf("legacy unallocated claim was REJECTED under the shipped far-future fence: %v — "+
			"this would take the old drip chain down on the first boot", err)
	}
	if status, _, alloc := rowState(t, db, legacy); status != "claimed" || alloc.Valid {
		t.Errorf("legacy claim state = (%q, alloc %v), want (claimed, NULL)", status, alloc)
	}
}

// TestPCQFenceIsFarFuture pins the shipped literal so a canary-day edit to the
// package copy cannot land without someone reading the comment above it.
func TestPCQFenceIsFarFuture(t *testing.T) {
	fence, err := time.Parse("2006-01-02 15:04:05-07", PCQAllocationFence)
	if err != nil {
		t.Fatalf("PCQAllocationFence %q is not a parseable timestamptz literal: %v", PCQAllocationFence, err)
	}
	if !fence.After(time.Now().AddDate(1, 0, 0)) {
		t.Errorf("PCQAllocationFence = %s is within a year; the shipped fence must be far-future until §7 step 3", PCQAllocationFence)
	}
}

// TestClaimIsIdempotentOnRefire pins the `AND status='ready'` guard: the second
// fire of the same id list (scheduler re-fire, retry, second instance) moves
// nothing and does not re-stamp the row onto a new allocation.
func TestClaimIsIdempotentOnRefire(t *testing.T) {
	db := newPCQTestDB(t, pastFence)
	ds := mkDataset(t, db, false)
	id := insertRow(t, db, ds, "wcl_remail", pcqRow{ISP: "aol"})

	tr := NewTransitions()
	first := uuid.New()
	second := uuid.New()
	if n, err := tr.Claim(context.Background(), db, []uuid.UUID{id}, first); err != nil || n != 1 {
		t.Fatalf("first Claim = (%d, %v), want (1, nil)", n, err)
	}
	_, firstAt, _ := rowState(t, db, id)

	n, err := tr.Claim(context.Background(), db, []uuid.UUID{id}, second)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if n != 0 {
		t.Errorf("second Claim moved %d rows, want 0", n)
	}
	_, secondAt, alloc := rowState(t, db, id)
	if alloc.String != first.String() {
		t.Errorf("allocation after re-fire = %s, want the original %s", alloc.String, first)
	}
	if !firstAt.Time.Equal(secondAt.Time) {
		t.Errorf("claimed_at was re-stamped by a re-fire: %s -> %s", firstAt.Time, secondAt.Time)
	}
}

// -----------------------------------------------------------------------------
// ClaimByISPCaps
// -----------------------------------------------------------------------------

// TestClaimByISPCapsZeroGrantExcludesISP is the regression guard for the
// 2026-08-27 'other'-bucket leak: an ISP granted 0 must claim nothing, and must
// NOT fall through to the 'other' allowance.
func TestClaimByISPCapsZeroGrantExcludesISP(t *testing.T) {
	db := newPCQTestDB(t, pastFence)
	ds := mkDataset(t, db, false)

	var aolIDs, otherIDs, protonIDs []uuid.UUID
	for i := 0; i < 3; i++ {
		aolIDs = append(aolIDs, insertRow(t, db, ds, "wcl_remail", pcqRow{ISP: "aol", IngestedAgo: time.Duration(30-i) * time.Minute}))
		otherIDs = append(otherIDs, insertRow(t, db, ds, "wcl_remail", pcqRow{ISP: "other", IngestedAgo: time.Duration(30-i) * time.Minute}))
		// protonmail is outside the 12 canonical classes: it must bucket to
		// 'other' (the fallback the orchestrator added 2026-08-11).
		protonIDs = append(protonIDs, insertRow(t, db, ds, "wcl_remail", pcqRow{ISP: "protonmail", IngestedAgo: time.Duration(30-i) * time.Minute}))
	}

	alloc := uuid.New()
	got, err := NewTransitions().ClaimByISPCaps(context.Background(), db, "wcl_remail", "ht",
		map[string]int{"aol": 0, "other": 4}, 100, alloc)
	if err != nil {
		t.Fatalf("ClaimByISPCaps: %v", err)
	}

	byISP := map[string]int{}
	claimed := map[string]bool{}
	for _, r := range got {
		byISP[r.ISPFamily]++
		claimed[r.ID] = true
	}
	if byISP["aol"] != 0 {
		t.Errorf("aol granted 0 but %d aol rows were claimed — the 'other'-bucket leak is back", byISP["aol"])
	}
	if n := byISP["other"] + byISP["protonmail"]; n != 4 {
		t.Errorf("other-bucket claimed %d rows, want 4 (3 'other' + 3 protonmail, capped at 4): %v", n, byISP)
	}
	if byISP["protonmail"] == 0 {
		t.Error("no protonmail row claimed — the unrecognized-ISP fallback to 'other' regressed and those rows starve in 'ready'")
	}
	for _, id := range aolIDs {
		if claimed[id.String()] {
			t.Fatalf("aol row %s claimed under a 0 grant", id)
		}
		if status, _, _ := rowState(t, db, id); status != "ready" {
			t.Errorf("aol row status = %q, want ready", status)
		}
	}
	_ = otherIDs
	_ = protonIDs

	// Every claimed row carries the allocation.
	for _, r := range got {
		id := uuid.MustParse(r.ID)
		status, _, gotAlloc := rowState(t, db, id)
		if status != "claimed" {
			t.Errorf("row %s status = %q, want claimed", r.ID, status)
		}
		if !gotAlloc.Valid || gotAlloc.String != alloc.String() {
			t.Errorf("row %s allocation = %v, want %s", r.ID, gotAlloc, alloc)
		}
	}
}

// TestClaimByISPCapsOldestFirstAndHardCap pins the two ordering guarantees: the
// per-ISP window takes the oldest ingest first, and hardCap trims the newest.
func TestClaimByISPCapsOldestFirstAndHardCap(t *testing.T) {
	db := newPCQTestDB(t, pastFence)
	ds := mkDataset(t, db, false)

	// ages descending: [0] is the oldest.
	ages := []time.Duration{5 * time.Hour, 4 * time.Hour, 3 * time.Hour, 2 * time.Hour, time.Hour}
	ids := make([]uuid.UUID, len(ages))
	for i, a := range ages {
		ids[i] = insertRow(t, db, ds, "wcl_remail", pcqRow{ISP: "aol", IngestedAgo: a})
	}

	got, err := NewTransitions().ClaimByISPCaps(context.Background(), db, "wcl_remail", "ht",
		map[string]int{"aol": 5}, 3, uuid.New())
	if err != nil {
		t.Fatalf("ClaimByISPCaps: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("claimed %d rows, want hardCap=3", len(got))
	}
	claimed := map[string]bool{}
	for _, r := range got {
		claimed[r.ID] = true
	}
	for i, id := range ids {
		want := i < 3 // the three oldest
		if claimed[id.String()] != want {
			t.Errorf("row aged %s claimed=%v, want %v — claim is not oldest-ingest-first", ages[i], claimed[id.String()], want)
		}
	}

	// The per-ISP cap binds below hardCap too.
	got2, err := NewTransitions().ClaimByISPCaps(context.Background(), db, "wcl_remail", "ht",
		map[string]int{"aol": 1}, 100, uuid.New())
	if err != nil {
		t.Fatalf("ClaimByISPCaps (cap 1): %v", err)
	}
	if len(got2) != 1 {
		t.Errorf("cap 1 claimed %d rows, want 1", len(got2))
	}
}

// TestClaimByISPCapsRespectsEmergencyStop pins the ported
// datasetNotEmergencyPausedSQL predicate — on BOTH the ranking scan and the
// re-check inside `picked`.
func TestClaimByISPCapsRespectsEmergencyStop(t *testing.T) {
	db := newPCQTestDB(t, pastFence)
	live := mkDataset(t, db, false)
	stopped := mkDataset(t, db, true)

	liveID := insertRow(t, db, live, "wcl_remail", pcqRow{ISP: "aol", IngestedAgo: 2 * time.Hour})
	stoppedID := insertRow(t, db, stopped, "wcl_remail", pcqRow{ISP: "aol", IngestedAgo: 3 * time.Hour})

	got, err := NewTransitions().ClaimByISPCaps(context.Background(), db, "wcl_remail", "ht",
		map[string]int{"aol": 10}, 100, uuid.New())
	if err != nil {
		t.Fatalf("ClaimByISPCaps: %v", err)
	}
	if len(got) != 1 || got[0].ID != liveID.String() {
		t.Fatalf("claimed %d rows (%v), want only the live dataset's row %s", len(got), got, liveID)
	}
	if status, _, _ := rowState(t, db, stoppedID); status != "ready" {
		t.Errorf("emergency-stopped dataset row status = %q, want ready", status)
	}
}

// TestClaimByISPCapsRefusesAllZeroGrants: an all-zero grant map is a `zero` tick
// outcome, not a claim that can only return nothing.
func TestClaimByISPCapsRefusesAllZeroGrants(t *testing.T) {
	db := newPCQTestDB(t, pastFence)
	ds := mkDataset(t, db, false)
	insertRow(t, db, ds, "wcl_remail", pcqRow{ISP: "aol"})

	_, err := NewTransitions().ClaimByISPCaps(context.Background(), db, "wcl_remail", "ht",
		map[string]int{"aol": 0, "other": 0}, 100, uuid.New())
	if !errors.Is(err, ErrNoPositiveGrant) {
		t.Fatalf("error = %v, want ErrNoPositiveGrant", err)
	}
	if n := countStatus(t, db, "claimed"); n != 0 {
		t.Errorf("%d rows claimed on an all-zero grant", n)
	}
}

// -----------------------------------------------------------------------------
// Release
// -----------------------------------------------------------------------------

// TestReleaseSendsFollowupsBackToMailed is the 2026-08-05 regression guard: a
// released follow-up must return to 'mailed' (the t2..tN pool), never to
// 'ready' (which re-sends t1 to someone mid-sequence).
func TestReleaseSendsFollowupsBackToMailed(t *testing.T) {
	db := newPCQTestDB(t, PCQAllocationFence)
	ds := mkDataset(t, db, false)

	intro := insertRow(t, db, ds, "wcl_remail", pcqRow{
		ISP: "aol", Status: "claimed", ClaimedAgo: time.Minute, TouchCount: 0})
	followup := insertRow(t, db, ds, "wcl_remail", pcqRow{
		ISP: "aol", Status: "claimed", ClaimedAgo: time.Minute, TouchCount: 2, MailedCampID: true})

	n, err := NewTransitions().Release(context.Background(), db,
		[]uuid.UUID{intro, followup}, "deploy failed")
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if n != 2 {
		t.Fatalf("Release moved %d rows, want 2", n)
	}
	if status, at, _ := rowState(t, db, intro); status != "ready" || at.Valid {
		t.Errorf("intro released to (%q, claimed_at valid=%v), want (ready, false)", status, at.Valid)
	}
	if status, at, _ := rowState(t, db, followup); status != "mailed" || at.Valid {
		t.Errorf("follow-up released to (%q, claimed_at valid=%v), want (mailed, false) — "+
			"releasing it to 'ready' ejects it from the ladder and re-sends t1", status, at.Valid)
	}
}

// -----------------------------------------------------------------------------
// Reap
// -----------------------------------------------------------------------------

// TestReapReleasesOrphansAndNothingElse is the §8.2 test-8 sibling the WP asks
// for: the reaper releases the orphan shape the existing releaseStaleClaims
// cannot see (subscriber_id SET) and touches nothing that is legitimately in
// flight.
func TestReapReleasesOrphansAndNothingElse(t *testing.T) {
	db := newPCQTestDB(t, PCQAllocationFence)
	ds := mkDataset(t, db, false)

	// The target: claimed 72h ago, subscriber hydrated, never entered a wave.
	// releaseStaleClaims (orchestrator :3066) requires subscriber_id IS NULL and
	// so leaves this row claimed forever.
	orphan := insertRow(t, db, ds, "wcl_remail", pcqRow{
		ISP: "aol", Status: "claimed", ClaimedAgo: 72 * time.Hour, Subscriber: true})

	// Must NOT move:
	fresh := insertRow(t, db, ds, "wcl_remail", pcqRow{
		ISP: "aol", Status: "claimed", ClaimedAgo: time.Hour, Subscriber: true})
	inWave := insertRow(t, db, ds, "wcl_remail", pcqRow{
		ISP: "aol", Status: "claimed", ClaimedAgo: 72 * time.Hour, Subscriber: true, LastTouch: true})
	mailedClaim := insertRow(t, db, ds, "wcl_remail", pcqRow{
		ISP: "aol", Status: "claimed", ClaimedAgo: 72 * time.Hour, Subscriber: true, MailedCampID: true})
	mailedRow := insertRow(t, db, ds, "wcl_remail", pcqRow{
		ISP: "aol", Status: "mailed", ClaimedAgo: 72 * time.Hour, Subscriber: true, TouchCount: 1, MailedCampID: true})
	readyRow := insertRow(t, db, ds, "wcl_remail", pcqRow{ISP: "aol"})

	n, err := NewTransitions().Reap(context.Background(), db, DefaultReapAge, DefaultReapBatch)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if n != 1 {
		t.Fatalf("Reap released %d rows, want exactly 1 (the orphan)", n)
	}
	if status, at, _ := rowState(t, db, orphan); status != "ready" || at.Valid {
		t.Errorf("orphan = (%q, claimed_at valid=%v), want (ready, false)", status, at.Valid)
	}
	for name, id := range map[string]uuid.UUID{
		"fresh claim inside the cutoff": fresh,
		"claimed row already in a wave": inWave,
		"claimed row already mailed":    mailedClaim,
	} {
		if status, at, _ := rowState(t, db, id); status != "claimed" || !at.Valid {
			t.Errorf("%s = (%q, claimed_at valid=%v), want (claimed, true)", name, status, at.Valid)
		}
	}
	if status, _, _ := rowState(t, db, mailedRow); status != "mailed" {
		t.Errorf("mailed row status = %q, want mailed", status)
	}
	if status, _, _ := rowState(t, db, readyRow); status != "ready" {
		t.Errorf("ready row status = %q, want ready", status)
	}
}

// TestReapRespectsBatchBound: one call moves at most `batch` rows. The reaper
// competes with live claim traffic under a 30s prod statement_timeout; an
// unbounded sweep is how a janitor becomes an outage.
func TestReapRespectsBatchBound(t *testing.T) {
	db := newPCQTestDB(t, PCQAllocationFence)
	ds := mkDataset(t, db, false)
	for i := 0; i < 5; i++ {
		insertRow(t, db, ds, "wcl_remail", pcqRow{
			ISP: "aol", Status: "claimed", ClaimedAgo: 72 * time.Hour, Subscriber: true})
	}

	tr := NewTransitions()
	n, err := tr.Reap(context.Background(), db, DefaultReapAge, 2)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if n != 2 {
		t.Fatalf("Reap(batch=2) released %d rows, want 2", n)
	}
	if got := countStatus(t, db, "claimed"); got != 3 {
		t.Errorf("%d rows still claimed after a bounded reap, want 3", got)
	}
	// Draining takes more ticks, not a longer statement.
	total := n
	for i := 0; i < 5 && total < 5; i++ {
		more, err := tr.Reap(context.Background(), db, DefaultReapAge, 2)
		if err != nil {
			t.Fatalf("Reap: %v", err)
		}
		total += more
	}
	if total != 5 || countStatus(t, db, "claimed") != 0 {
		t.Errorf("drained %d of 5, %d still claimed", total, countStatus(t, db, "claimed"))
	}
}

// TestReapRefusesZeroAge: a zero cutoff would reap the claims the current tick
// is still working with, so it is an error and not a default.
func TestReapRefusesZeroAge(t *testing.T) {
	db := newPCQTestDB(t, PCQAllocationFence)
	ds := mkDataset(t, db, false)
	id := insertRow(t, db, ds, "wcl_remail", pcqRow{
		ISP: "aol", Status: "claimed", ClaimedAgo: time.Second, Subscriber: true})

	for _, age := range []time.Duration{0, -time.Hour} {
		if _, err := NewTransitions().Reap(context.Background(), db, age, 100); err == nil {
			t.Fatalf("Reap(olderThan=%s) returned nil error, want a refusal", age)
		}
	}
	if status, _, _ := rowState(t, db, id); status != "claimed" {
		t.Errorf("row status = %q after a refused reap, want claimed", status)
	}
}

// TestReapCutoffIsComputedInPostgres pins the wording of the statement, because
// the property it protects (two instances with clock skew cannot expire each
// other's live claims) is invisible in a single-process test.
func TestReapCutoffIsComputedInPostgres(t *testing.T) {
	if !strings.Contains(reapOrphanClaimsSQL, "NOW() - make_interval") {
		t.Error("reapOrphanClaimsSQL no longer computes its cutoff in Postgres; a Go-clock cutoff lets a skewed instance reap live claims")
	}
	if !strings.Contains(reapOrphanClaimsSQL, "FOR UPDATE SKIP LOCKED") {
		t.Error("reapOrphanClaimsSQL lost FOR UPDATE SKIP LOCKED; the janitor will block on live claim traffic")
	}
	if !strings.Contains(reapOrphanClaimsSQL, "LIMIT $2") {
		t.Error("reapOrphanClaimsSQL lost its LIMIT; the sweep is unbounded")
	}
	if strings.Contains(reapOrphanClaimsSQL, "subscriber_id") {
		t.Error("reapOrphanClaimsSQL grew a subscriber_id predicate — that is exactly the blind spot it exists to cover")
	}
}
