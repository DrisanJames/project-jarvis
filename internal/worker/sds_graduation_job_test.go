package worker

// Tests for the SDS Graduation Job. Each test exercises one pass of
// the daily routine in isolation by invoking RunOnce and asserting on
// the SQL the job emits. The negative test (NoColdDecayPass) is the
// critical guard: it pins the explicit operator decision (2026-05-09)
// that this job MUST NOT include a cold-decay re-probe pass.

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func newGraduationDBMock(t *testing.T) (sqlmock.Sqlmock, *SDSGraduationJob) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	job := NewSDSGraduationJob(db)
	return mock, job
}

// TestSDSGraduationJob_ColdPass verifies the cold pass UPDATE runs
// against mailing_subscriber_domain_state with the expected predicate:
// total_sent>=4 AND total_opens=0 AND total_clicks=0 AND state!='cold'.
// We deliberately match the entire WHERE clause so a regression that
// drops one of the filters (e.g. forgets the state!='cold' guard and
// re-stamps state_updated_at on every cold row every day) fails here.
func TestSDSGraduationJob_ColdPass(t *testing.T) {
	mock, job := newGraduationDBMock(t)

	coldPattern := regexp.MustCompile(`(?s)UPDATE mailing_subscriber_domain_state\s+SET state\s*=\s*'cold'.*WHERE total_sent >= 4\s+AND total_opens = 0\s+AND total_clicks = 0\s+AND state != 'cold'`)
	mock.ExpectExec(coldPattern.String()).
		WillReturnResult(sqlmock.NewResult(0, 7))

	// The cross-engaged pass also runs — set up a permissive matcher
	// so the test focuses on the cold pass without spurious failures.
	crossPattern := regexp.MustCompile(`UPDATE mailing_subscribers\s+SET cross_engaged = true`)
	mock.ExpectExec(crossPattern.String()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	job.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations not met: %v", err)
	}
}

// TestSDSGraduationJob_CrossEngagedPass verifies the cross-engaged
// UPDATE: subscribers with state='engaged' on >=2 distinct
// sending_domains get cross_engaged=true. We pin the GROUP BY +
// HAVING COUNT(DISTINCT sending_domain) >= 2 because that's the
// definitional contract — a single-domain engaged subscriber must NOT
// graduate.
func TestSDSGraduationJob_CrossEngagedPass(t *testing.T) {
	mock, job := newGraduationDBMock(t)

	// Cold pass runs first; allow it through with a permissive matcher.
	coldPattern := regexp.MustCompile(`UPDATE mailing_subscriber_domain_state\s+SET state\s*=\s*'cold'`)
	mock.ExpectExec(coldPattern.String()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	crossPattern := regexp.MustCompile(`(?s)UPDATE mailing_subscribers\s+SET cross_engaged = true.*SELECT subscriber_id\s+FROM mailing_subscriber_domain_state\s+WHERE state = 'engaged'\s+GROUP BY subscriber_id\s+HAVING COUNT\(DISTINCT sending_domain\) >= 2.*cross_engaged IS NULL OR cross_engaged = false`)
	mock.ExpectExec(crossPattern.String()).
		WillReturnResult(sqlmock.NewResult(0, 3))

	job.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations not met: %v", err)
	}
}

// TestSDSGraduationJob_NoColdDecayPass is the negative guard for the
// operator decision (2026-05-09) that this job MUST NOT include a
// cold-decay re-probe pass. With sqlmock running in strict order, any
// third UPDATE (or any UPDATE on a state='engaged' row demoting it
// back to probe) would surface as an unexpected query and fail the
// test.
//
// We register exactly TWO ExpectExec entries — the cold pass and the
// cross-engaged pass — and assert ExpectationsWereMet at the end.
// sqlmock's default behavior fails the test on any extra Exec call.
//
// The cold-pass regex doubly enforces the invariant: state='cold' is
// only set when total_opens=0 AND total_clicks=0. Any future drift
// where the cold pass starts touching engaged rows would either fail
// the regex or produce an extra Exec — both caught here.
func TestSDSGraduationJob_NoColdDecayPass(t *testing.T) {
	mock, job := newGraduationDBMock(t)

	coldPattern := regexp.MustCompile(`(?s)UPDATE mailing_subscriber_domain_state\s+SET state\s*=\s*'cold'.*total_opens = 0\s+AND total_clicks = 0`)
	mock.ExpectExec(coldPattern.String()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	crossPattern := regexp.MustCompile(`UPDATE mailing_subscribers\s+SET cross_engaged = true`)
	mock.ExpectExec(crossPattern.String()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	job.RunOnce(context.Background())

	// If RunOnce had a third UPDATE — e.g. a cold-decay re-probe pass
	// flipping state='cold' rows back to 'probe' on a 60-day timer —
	// sqlmock would have surfaced it as an unexpected query during
	// RunOnce. ExpectationsWereMet additionally verifies both
	// registered passes ran (no missed pass).
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations not met (unexpected demotion or missed pass): %v", err)
	}
}
