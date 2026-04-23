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
