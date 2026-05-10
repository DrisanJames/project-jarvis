package mailing

// Tests for RecomputeSDSScoreLocal — the per-(subscriber, sending_domain)
// engagement score used by the PMTA planner's ORDER BY sds.score_local DESC
// selection on the primary master-selection path.
//
// The critical property under test is the bot guard added in response to the
// Apr 2026 bot-send audit: every open and click on the tracking hot path
// calls RecomputeSDSScoreLocal (see internal/api/mailing_tracking.go). Without
// the guard, a honeypot-flagged scanner that fires opens and clicks on every
// send would monotonically push score_local toward 1.0, and because the
// planner orders candidates by score_local DESC, the bot would be selected
// first on every subsequent campaign — a self-reinforcing feedback loop.
//
// We verify here that the UPDATE statement short-circuits bots via the
// correlated subquery against mailing_subscribers.is_bot.

import (
	"context"
	"database/sql"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func newMailingDBMock(t *testing.T) (sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return mock, db
}

func TestRecomputeSDSScoreLocal_EmitsUpdateWithBotGuard(t *testing.T) {
	mock, db := newMailingDBMock(t)
	subID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	domain := "em.example.com"

	// Match the query emitted by the production path. The must-have
	// invariant is that the UPDATE carries the correlated subquery
	// against mailing_subscribers.is_bot so bot rows collapse to 0.
	pattern := regexp.MustCompile(`(?s)UPDATE mailing_subscriber_domain_state sds.*is_bot.*FROM mailing_subscribers s WHERE s\.id = sds\.subscriber_id.*THEN 0::numeric`)
	mock.ExpectExec(pattern.String()).
		WithArgs(subID, domain).
		WillReturnResult(sqlmock.NewResult(0, 1))

	RecomputeSDSScoreLocal(context.Background(), db, subID, domain)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations not met: %v", err)
	}
}

func TestRecomputeSDSScoreLocal_NoopOnEmptyDomain(t *testing.T) {
	mock, db := newMailingDBMock(t)
	subID := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	// Empty/whitespace domain must short-circuit before any DB call.
	// Any ExecContext here would fail ExpectationsWereMet below.
	RecomputeSDSScoreLocal(context.Background(), db, subID, "")
	RecomputeSDSScoreLocal(context.Background(), db, subID, "   ")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations not met (should have been zero calls): %v", err)
	}
}

func TestRecomputeSDSScoreLocal_NoopOnNilUUID(t *testing.T) {
	mock, db := newMailingDBMock(t)

	// Nil subscriber ID is a programming error upstream but must not
	// panic or hit the DB. Same contract as empty domain.
	RecomputeSDSScoreLocal(context.Background(), db, uuid.Nil, "em.example.com")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations not met (should have been zero calls): %v", err)
	}
}

func TestRecomputeSDSScoreLocal_NormalizesSendingDomain(t *testing.T) {
	mock, db := newMailingDBMock(t)
	subID := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	// NormalizeSendingDomain lowercases and trims. Verify the UPDATE
	// is bound with the normalized value so SDS lookups hit the same
	// key the planner/worker writes under.
	pattern := regexp.MustCompile(`UPDATE mailing_subscriber_domain_state sds`)
	mock.ExpectExec(pattern.String()).
		WithArgs(subID, "em.example.com").
		WillReturnResult(sqlmock.NewResult(0, 1))

	RecomputeSDSScoreLocal(context.Background(), db, subID, "  EM.Example.COM  ")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations not met: %v", err)
	}
}

// === Per-domain engagement engine state-machine tests (SA-1) ===
//
// These verify the inline state transitions added to the existing
// UPSERT helpers. Because sqlmock asserts at the SQL-text level (not
// row-outcome), the tests pin the CASE expressions that drive each
// transition. Any future refactor that drops the CASE for a different
// construct must update both the implementation and these regex
// patterns together — the coupling is intentional.
//
// Critical invariants under test:
//
//   1. UpsertSDSSend's threshold check uses the INCREMENTED total_sent
//      value (mailing_subscriber_domain_state.total_sent + 1), not the
//      pre-write value, so the 4th send actually flips the row to cold.
//   2. UpsertSDSOpen / UpsertSDSClick are IDEMPOTENT for engaged rows
//      (no state_updated_at re-stamp). The CASE arm `WHEN
//      mailing_subscriber_domain_state.state IN ('probe','cold') THEN
//      NOW() ELSE mailing_subscriber_domain_state.state_updated_at END`
//      enforces this in a single statement.
//   3. UpsertSDSHardBounce / UpsertSDSComplaint / UpsertSDSUnsub all
//      land state='suppressed' unconditionally (terminal transition).

func TestUpsertSDSSend_CrossesColdThreshold(t *testing.T) {
	mock, db := newMailingDBMock(t)
	subID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	domain := "em.example.com"

	// Assert the UPSERT contains the cold-threshold CASE that uses the
	// post-increment total_sent (the +1 is the critical bit) and only
	// flips state when current state is NOT engaged/suppressed.
	pattern := regexp.MustCompile(`(?s)INSERT INTO mailing_subscriber_domain_state.*ON CONFLICT.*state\s*=\s*CASE.*state IN \('engaged','suppressed'\).*total_sent \+ 1\) >= 4.*total_opens = 0.*total_clicks = 0.*THEN 'cold'`)
	mock.ExpectExec(pattern.String()).
		WithArgs(subID, domain).
		WillReturnResult(sqlmock.NewResult(0, 1))

	UpsertSDSSend(context.Background(), db, subID, domain)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations not met: %v", err)
	}
}

func TestUpsertSDSOpen_PromotesProbeToEngaged(t *testing.T) {
	mock, db := newMailingDBMock(t)
	subID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	domain := "em.example.com"

	// Pin the promotion CASE that flips probe (or cold) -> engaged.
	pattern := regexp.MustCompile(`(?s)INSERT INTO mailing_subscriber_domain_state.*total_opens.*ON CONFLICT.*state IN \('probe','cold'\).*THEN 'engaged'`)
	mock.ExpectExec(pattern.String()).
		WithArgs(subID, domain).
		WillReturnResult(sqlmock.NewResult(0, 1))

	UpsertSDSOpen(context.Background(), db, subID, domain)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations not met: %v", err)
	}
}

func TestUpsertSDSOpen_PromotesColdToEngaged(t *testing.T) {
	mock, db := newMailingDBMock(t)
	subID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	domain := "em.example.com"

	// Same UPSERT; the CASE arm covers both probe and cold via the
	// `state IN ('probe','cold')` predicate. Verifying the predicate
	// here proves cold rows get promoted alongside probe rows by the
	// same single round-trip.
	pattern := regexp.MustCompile(`(?s)ON CONFLICT.*state IN \('probe','cold'\).*THEN 'engaged'`)
	mock.ExpectExec(pattern.String()).
		WithArgs(subID, domain).
		WillReturnResult(sqlmock.NewResult(0, 1))

	UpsertSDSOpen(context.Background(), db, subID, domain)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations not met: %v", err)
	}
}

func TestUpsertSDSOpen_IdempotentForEngaged(t *testing.T) {
	mock, db := newMailingDBMock(t)
	subID := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	domain := "em.example.com"

	// The state_updated_at CASE must include an ELSE arm that
	// preserves the existing value. Without it, every open would
	// re-stamp state_updated_at on already-engaged rows and we'd lose
	// the "first engagement timestamp" semantic.
	pattern := regexp.MustCompile(`(?s)state_updated_at\s*=\s*CASE.*state IN \('probe','cold'\).*THEN NOW\(\).*ELSE mailing_subscriber_domain_state\.state_updated_at\s+END`)
	mock.ExpectExec(pattern.String()).
		WithArgs(subID, domain).
		WillReturnResult(sqlmock.NewResult(0, 1))

	UpsertSDSOpen(context.Background(), db, subID, domain)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations not met: %v", err)
	}
}

func TestUpsertSDSClick_PromotesProbeToEngaged(t *testing.T) {
	mock, db := newMailingDBMock(t)
	subID := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	domain := "em.example.com"

	// Click path mirrors open: probe/cold -> engaged via the same
	// CASE arm. Different counter column (total_clicks) anchors the
	// regex so a swap with the open helper is caught.
	pattern := regexp.MustCompile(`(?s)INSERT INTO mailing_subscriber_domain_state.*total_clicks.*ON CONFLICT.*state IN \('probe','cold'\).*THEN 'engaged'`)
	mock.ExpectExec(pattern.String()).
		WithArgs(subID, domain).
		WillReturnResult(sqlmock.NewResult(0, 1))

	UpsertSDSClick(context.Background(), db, subID, domain)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations not met: %v", err)
	}
}

func TestUpsertSDSHardBounce_SetsSuppressed(t *testing.T) {
	mock, db := newMailingDBMock(t)
	subID := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	domain := "em.example.com"

	// Hard bounce has TWO writes: the SDS UPSERT (must carry
	// state='suppressed') and the global mailing_subscribers UPDATE.
	sdsPattern := regexp.MustCompile(`(?s)INSERT INTO mailing_subscriber_domain_state.*hard_bounced_at.*ON CONFLICT.*state\s*=\s*'suppressed'`)
	mock.ExpectExec(sdsPattern.String()).
		WithArgs(subID, domain).
		WillReturnResult(sqlmock.NewResult(0, 1))

	globalPattern := regexp.MustCompile(`UPDATE mailing_subscribers\s+SET hard_bounced_at`)
	mock.ExpectExec(globalPattern.String()).
		WithArgs(subID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	UpsertSDSHardBounce(context.Background(), db, subID, domain)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations not met: %v", err)
	}
}

func TestUpsertSDSComplaint_SetsSuppressed(t *testing.T) {
	mock, db := newMailingDBMock(t)
	subID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	domain := "em.example.com"

	// Mirror of hard-bounce: SDS suppressed flip + global complained_at.
	sdsPattern := regexp.MustCompile(`(?s)INSERT INTO mailing_subscriber_domain_state.*complained_at.*ON CONFLICT.*state\s*=\s*'suppressed'`)
	mock.ExpectExec(sdsPattern.String()).
		WithArgs(subID, domain).
		WillReturnResult(sqlmock.NewResult(0, 1))

	globalPattern := regexp.MustCompile(`UPDATE mailing_subscribers\s+SET complained_at`)
	mock.ExpectExec(globalPattern.String()).
		WithArgs(subID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	UpsertSDSComplaint(context.Background(), db, subID, domain)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations not met: %v", err)
	}
}

func TestUpsertSDSUnsub_SetsSuppressed(t *testing.T) {
	mock, db := newMailingDBMock(t)
	subID := uuid.MustParse("aaaa1111-bbbb-cccc-dddd-eeeeffffeeee")
	domain := "em.example.com"

	// Unsubscribe is per-domain only — no global UPDATE pair like
	// hard-bounce / complaint. Per design rule in this file's header
	// docstring: callers separately decide whether to write a global
	// suppression row depending on token shape.
	pattern := regexp.MustCompile(`(?s)INSERT INTO mailing_subscriber_domain_state.*unsubscribed_at.*ON CONFLICT.*state\s*=\s*'suppressed'`)
	mock.ExpectExec(pattern.String()).
		WithArgs(subID, domain).
		WillReturnResult(sqlmock.NewResult(0, 1))

	UpsertSDSUnsub(context.Background(), db, subID, domain)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations not met: %v", err)
	}
}
