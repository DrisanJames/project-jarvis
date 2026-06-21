package consumers

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/eventbus"
)

// Shadow mode (default) writes the shadow ledger and does NOT call add.
func TestSuppressionProjector_ShadowMode_WritesShadowOnly(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	var addCalls int
	add := func(email, reason, brand, source string) { addCalls++ }

	payload := mustJSON(t, suppressionPayload{
		Email: "bounce@example.com", Reason: "hard_bounce", BrandRoot: "", Source: "pmta",
	})

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO suppression_shadow")).
		WithArgs("bounce@example.com", "hard_bounce", "" /*brand_root*/, "pmta", "bounce@example.com").
		WillReturnResult(sqlmock.NewResult(0, 1))

	p := NewSuppressionProjector(db, eventbus.Config{}, "", add) // "" => shadow default
	if p.mode != SuppressionModeShadow {
		t.Fatalf("empty mode should default to shadow, got %q", p.mode)
	}
	if err := p.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if addCalls != 0 {
		t.Fatalf("shadow mode must NOT call add, got %d calls", addCalls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Authoritative mode writes the shadow ledger AND calls add (live hub).
func TestSuppressionProjector_AuthoritativeMode_CallsAdd(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	var gotEmail, gotReason, gotBrand, gotSource string
	var addCalls int
	add := func(email, reason, brand, source string) {
		addCalls++
		gotEmail, gotReason, gotBrand, gotSource = email, reason, brand, source
	}

	payload := mustJSON(t, suppressionPayload{
		Email: "fbl@example.com", Reason: "complaint", BrandRoot: "discountblog.com", Source: "ses",
	})

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO suppression_shadow")).
		WithArgs("fbl@example.com", "complaint", "discountblog.com", "ses", "fbl@example.com").
		WillReturnResult(sqlmock.NewResult(0, 1))

	p := NewSuppressionProjector(db, eventbus.Config{}, SuppressionModeAuthoritative, add)
	if err := p.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if addCalls != 1 {
		t.Fatalf("authoritative mode must call add once, got %d", addCalls)
	}
	if gotEmail != "fbl@example.com" || gotReason != "complaint" ||
		gotBrand != "discountblog.com" || gotSource != "ses" {
		t.Fatalf("add got (%q,%q,%q,%q)", gotEmail, gotReason, gotBrand, gotSource)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Authoritative mode with a nil add func errors (fail loud, don't skip the hub).
func TestSuppressionProjector_AuthoritativeNilAddErrors(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO suppression_shadow")).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	p := NewSuppressionProjector(db, eventbus.Config{}, SuppressionModeAuthoritative, nil)
	payload := mustJSON(t, suppressionPayload{Email: "x@y.com", Reason: "hard_bounce"})
	if err := p.Handle(context.Background(), nil, payload); err == nil {
		t.Fatal("expected error when authoritative mode has nil add")
	}
}

// Replay collapses on (email, brand_root) — the shadow ON CONFLICT handles it;
// the handler returns nil whether 1 or 0 rows are affected.
func TestSuppressionProjector_IdempotentOnReplay(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	payload := mustJSON(t, suppressionPayload{Email: "dup@example.com", Reason: "hard_bounce"})

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO suppression_shadow")).
		WithArgs("dup@example.com", "hard_bounce", "", nil, "dup@example.com").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO suppression_shadow")).
		WithArgs("dup@example.com", "hard_bounce", "", nil, "dup@example.com").
		WillReturnResult(sqlmock.NewResult(0, 0)) // conflict no-op

	p := NewSuppressionProjector(db, eventbus.Config{}, SuppressionModeShadow, nil)
	for i := 0; i < 2; i++ {
		if err := p.Handle(context.Background(), nil, payload); err != nil {
			t.Fatalf("Handle replay %d: %v", i, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestSuppressionProjector_RejectsEmptyEmail(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	p := NewSuppressionProjector(db, eventbus.Config{}, SuppressionModeShadow, nil)
	payload := mustJSON(t, suppressionPayload{Reason: "hard_bounce"})
	if err := p.Handle(context.Background(), nil, payload); err == nil {
		t.Fatal("expected error on empty email")
	}
}

func TestSuppressionGroupID_FanOutSuffix(t *testing.T) {
	if got := SuppressionGroupID("base", "task-abc"); got != "base.task-abc" {
		t.Fatalf("GroupID = %q", got)
	}
	if got := SuppressionGroupID("", "task-abc"); got != "ignite-suppression-projector.task-abc" {
		t.Fatalf("GroupID default base = %q", got)
	}
	if got := SuppressionGroupID("base", ""); got != "base" {
		t.Fatalf("GroupID no instance = %q", got)
	}
}

func TestSuppressionStart_NoopWhenDisabled(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	p := NewSuppressionProjector(db, eventbus.Config{}, SuppressionModeShadow, nil)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start should no-op when disabled, got %v", err)
	}
	p.Stop()
}
