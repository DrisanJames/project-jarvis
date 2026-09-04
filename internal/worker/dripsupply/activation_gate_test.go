package dripsupply

// activation_gate_test.go — regression guard for the prod defect found
// 2026-09-04: contract activation was gated on a UTC day key, so a contract
// whose effective_at falls mid-day was skipped for that whole day.
//
// Observed in prod: 113 REQ-118 contracts with effective_at = 2026-09-04
// 06:00Z were still `scheduled` at 06:21Z with both shadow ledgers empty. The
// mediator had marked day "2026-09-04" activated at ~00:0xZ — when nothing was
// due — so the 06:00Z boundary never re-ran ActivateScheduled. They would have
// activated only at the next UTC midnight, ~18h late.
//
// These use sqlmock rather than the scratch-schema harness because what is
// under test is WHICH statements the gate issues, not row arithmetic.

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// errStopLoad ends TickStart immediately after the activation step: the
// contract load is the next thing it does, and an error there makes it log and
// return. That keeps the mock's expectations scoped to the gate.
var errStopLoad = errors.New("stop after activation")

func gateMediator(t *testing.T) (*Mediator, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	med := NewMediator(db, NewService(db), MediatorConfig{
		Mode:           ModeShadow,
		AlertsDisabled: true,
		ContractSource: func(context.Context, time.Time) (*ActiveSet, error) {
			return nil, errStopLoad
		},
	})
	return med, mock, func() { db.Close() }
}

// expectActivation queues the statements ActivateScheduled issues: one
// transaction, three UPDATEs per kind, commit.
func expectActivation(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	for range AllKinds() {
		for i := 0; i < 3; i++ {
			mock.ExpectExec(`UPDATE drip_\w+_contracts`).
				WillReturnResult(sqlmock.NewResult(0, 1))
		}
	}
	mock.ExpectCommit()
}

const dueProbeFragment = `WHERE status = 'scheduled' AND effective_at <= $1`

// THE regression: two ticks on the SAME UTC day, one before the contracts come
// due and one after. The second must activate.
func TestActivationFiresWhenContractsComeDueMidDay(t *testing.T) {
	med, mock, done := gateMediator(t)
	defer done()

	day := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	before := day.Add(5 * time.Minute)            // 00:05Z — nothing due yet
	after := day.Add(6*time.Hour + 5*time.Minute) // 06:05Z — same UTC day, now due

	// Tick 1: probe says nothing is due, so NO transaction may be opened.
	mock.ExpectQuery(regexp.QuoteMeta(dueProbeFragment)).
		WithArgs(before).
		WillReturnRows(sqlmock.NewRows([]string{"due"}).AddRow(false))
	med.TickStart(context.Background(), before)

	// Tick 2: same UTC day, the contracts are now due — activation MUST run.
	mock.ExpectQuery(regexp.QuoteMeta(dueProbeFragment)).
		WithArgs(after).
		WillReturnRows(sqlmock.NewRows([]string{"due"}).AddRow(true))
	expectActivation(mock)
	med.TickStart(context.Background(), after)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a contract that came due mid-day was not activated: %v", err)
	}
}

// The cheap half of the gate: nothing due means no activation transaction, so
// the 15-minute tick does not write WAL for a no-op 96 times a day.
func TestNothingDueOpensNoTransaction(t *testing.T) {
	med, mock, done := gateMediator(t)
	defer done()

	now := time.Date(2026, 9, 4, 3, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(dueProbeFragment)).
		WithArgs(now).
		WillReturnRows(sqlmock.NewRows([]string{"due"}).AddRow(false))

	med.TickStart(context.Background(), now)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expected only the due probe: %v", err)
	}
}

// A failing probe must not take the tick down, and must not activate blind.
func TestDueProbeFailureIsNonFatal(t *testing.T) {
	med, mock, done := gateMediator(t)
	defer done()

	now := time.Date(2026, 9, 4, 6, 5, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(dueProbeFragment)).
		WithArgs(now).
		WillReturnError(errors.New("probe boom"))

	med.TickStart(context.Background(), now) // must not panic

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a failed probe must not open a transaction: %v", err)
	}
}

// The probe is built from kindSpecs so a fifth contract kind cannot be added
// without being covered here.
func TestDueProbeCoversEveryContractKind(t *testing.T) {
	q := dueProbeSQL()
	for _, kind := range AllKinds() {
		table := kindSpecs[kind].Table
		if !strings.Contains(q, table) {
			t.Errorf("due probe does not read %s (kind %s)", table, kind)
		}
	}
	if got := strings.Count(q, "EXISTS"); got != len(AllKinds()) {
		t.Errorf("due probe has %d EXISTS clauses, want %d", got, len(AllKinds()))
	}
}
