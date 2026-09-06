package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// The FamilyGovernor hook inside enqueuePMTAWave (pmta_wave_dispatcher.go),
// driven through the real dispatcher against sqlmock on the direct (non-Kafka,
// set-based) path:
//
//   - OFF / nil / non-family ISP / no contract / governor error → the enqueue is
//     byte-identical to today (claim LIMIT == remaining, wave credited in full)
//     and, for OFF, NOT ONE governor query is issued (negative control).
//   - SHADOW → governor queries + ledger row, `remaining` untouched.
//   - ON → claim LIMIT == Allowed; Allowed == 0 completes the wave with a
//     last_error note and never claims a recipient.

func fgDispatcherEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DISABLE_WAVE_AB_SPLIT", "true")
	t.Setenv("KAFKA_SEND_QUEUE_ALL", "")
	t.Setenv("KAFKA_SEND_QUEUE_WAVES", "")
	t.Setenv("KAFKA_SEND_QUEUE_CAMPAIGNS", "")
	t.Setenv("DISABLE_SETBASED_ENQUEUE", "")
}

// expectGovernedWavePrelude is expectRoutedWavePrelude with the plan's isp and
// sending_domain parametrised (they are what the governor keys on).
func expectGovernedWavePrelude(mock sqlmock.Sqlmock, waveID, campaignID, planID, orgID uuid.UUID, planned int, planISP, planDomain string) {
	mock.ExpectBegin()
	mock.ExpectQuery(`w.campaign_id, w.isp_plan_id`).
		WithArgs(waveID.String()).
		WillReturnRows(sqlmock.NewRows(waveMetaColumns).AddRow(
			campaignID, planID, orgID,
			"planned", "scheduled", "ready",
			testScheduledAt, testScheduledAt, planned, 0, nil, planISP, planDomain))
	mock.ExpectQuery(`COALESCE\(sp.sending_domain`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"sending_domain"}).AddRow(planDomain))
	mock.ExpectExec(`UPDATE mailing_campaign_waves\s+SET status = 'enqueuing'`).
		WithArgs(waveID.String()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaigns\s+SET status = 'sending'`).
		WithArgs(campaignID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaign_isp_plans\s+SET status = 'running'`).
		WithArgs(planID).WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectContentAndClaim registers the content read, the snapshot ensure, and
// the claim of `claim` recipients (LIMIT arg == claim), then the direct INSERT
// + plan_recipient transition and the three counter writes for `claim` rows.
func expectContentAndClaim(mock sqlmock.Sqlmock, waveID, campaignID, planID uuid.UUID, claim int) {
	mock.ExpectQuery(`COALESCE\(from_name`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{
			"from_name", "from_email", "subject", "html_content",
			"name", "plain_content", "content_locked",
		}).AddRow("Sender", "hello@m.test.com", "Subj", "<html>body</html>",
			"Test Campaign", "plain", false))
	expectSnapshotEnsured(mock, uuid.New())
	rows := sqlmock.NewRows(claimColumns)
	for i := 0; i < claim; i++ {
		rows.AddRow(uuid.New().String(), uuid.New().String(), "a@yahoo.com", "yahoo", i+1, "segment", nil, "selected", "active", false)
	}
	mock.ExpectQuery(`FROM mailing_campaign_plan_recipients pr`).
		WithArgs(planID, claim).
		WillReturnRows(rows)
	mock.ExpectExec(`(?s)INSERT INTO mailing_campaign_queue`).
		WillReturnResult(sqlmock.NewResult(0, int64(claim)))
	mock.ExpectExec(`SET status = 'queued', queued_at = NOW\(\)`).
		WillReturnResult(sqlmock.NewResult(0, int64(claim)))
	mock.ExpectExec(`UPDATE mailing_campaign_waves\s+SET enqueued_recipients`).
		WithArgs(waveID.String(), claim, 0, 0, 0, "completed").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaign_isp_plans\s+SET enqueued_count`).
		WithArgs(planID, claim).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaigns\s+SET queued_count`).
		WithArgs(campaignID, claim).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

func fgRun(t *testing.T, mock sqlmock.Sqlmock, gov *FamilyGovernor, waveID uuid.UUID, wantN int) {
	t.Helper()
	n, err := enqueuePMTAWave(context.Background(), gov.db, waveID.String(), nil, gov)
	if err != nil {
		t.Fatalf("enqueuePMTAWave: %v", err)
	}
	if n != wantN {
		t.Fatalf("want %d enqueued, got %d", wantN, n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet/unexpected: %v", err)
	}
}

// NEGATIVE CONTROL — mode off on a family wave: zero governor statements.
func TestFamilyGovernorHook_OffIssuesNoQueries(t *testing.T) {
	fgDispatcherEnv(t)
	db, mock := fgNewMock(t)
	mock.MatchExpectationsInOrder(false)
	waveID, campaignID, planID, orgID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	expectGovernedWavePrelude(mock, waveID, campaignID, planID, orgID, 2, "yahoo", fgTestDomain)
	expectContentAndClaim(mock, waveID, campaignID, planID, 2)
	gov := newFamilyGovernorWithMode(db, FamilyGovernorOff)
	fgRun(t, mock, gov, waveID, 2)
}

// NEGATIVE CONTROL — the public EnqueuePMTAWave (nil governor) is unchanged.
func TestFamilyGovernorHook_NilGovernorUnchanged(t *testing.T) {
	fgDispatcherEnv(t)
	db, mock := fgNewMock(t)
	mock.MatchExpectationsInOrder(false)
	waveID, campaignID, planID, orgID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	expectGovernedWavePrelude(mock, waveID, campaignID, planID, orgID, 2, "yahoo", fgTestDomain)
	expectContentAndClaim(mock, waveID, campaignID, planID, 2)
	n, err := EnqueuePMTAWave(context.Background(), db, waveID.String(), nil)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// SHADOW: contract says trim to 1, the wave still claims 2 and credits 2; the
// ledger row records requested=2 allowed=1 reason=trim.
func TestFamilyGovernorHook_ShadowNeverTrims(t *testing.T) {
	fgDispatcherEnv(t)
	db, mock := fgNewMock(t)
	mock.MatchExpectationsInOrder(false)
	waveID, campaignID, planID, orgID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	expectGovernedWavePrelude(mock, waveID, campaignID, planID, orgID, 2, "yahoo", fgTestDomain)
	fgExpectContract(mock, fgTestLane, 10)
	fgExpectSpend(mock, fgTestDomain, 9)
	fgExpectLedger(mock, waveID.String(), fgTestDomain, "yahoo", FamilyGovernorShadow, 2, 10, 9, 1, "trim")
	expectContentAndClaim(mock, waveID, campaignID, planID, 2) // LIMIT 2 — untouched
	gov := newFamilyGovernorWithMode(db, FamilyGovernorShadow)
	fgRun(t, mock, gov, waveID, 2)
}

// SHADOW deny: allowed=0 is logged + recorded, the wave still enqueues in full.
func TestFamilyGovernorHook_ShadowDenyStillSends(t *testing.T) {
	fgDispatcherEnv(t)
	db, mock := fgNewMock(t)
	mock.MatchExpectationsInOrder(false)
	waveID, campaignID, planID, orgID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	expectGovernedWavePrelude(mock, waveID, campaignID, planID, orgID, 2, "aol", fgTestDomain)
	fgExpectContract(mock, fgTestLane, 10)
	fgExpectSpend(mock, fgTestDomain, 10)
	fgExpectLedger(mock, waveID.String(), fgTestDomain, "aol", FamilyGovernorShadow, 2, 10, 10, 0, "deny")
	expectContentAndClaim(mock, waveID, campaignID, planID, 2)
	gov := newFamilyGovernorWithMode(db, FamilyGovernorShadow)
	fgRun(t, mock, gov, waveID, 2)
}

// ON trim: claim LIMIT becomes Allowed (1 of 2); the wave credits 1 and
// completes; the leftover plan_recipient stays 'selected' for the next wave.
func TestFamilyGovernorHook_OnTrimsToAllowed(t *testing.T) {
	fgDispatcherEnv(t)
	db, mock := fgNewMock(t)
	mock.MatchExpectationsInOrder(false)
	waveID, campaignID, planID, orgID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	expectGovernedWavePrelude(mock, waveID, campaignID, planID, orgID, 2, "yahoo", fgTestDomain)
	fgExpectContract(mock, fgTestLane, 10)
	fgExpectSpend(mock, fgTestDomain, 9)
	fgExpectLedger(mock, waveID.String(), fgTestDomain, "yahoo", FamilyGovernorOn, 2, 10, 9, 1, "trim")
	expectContentAndClaim(mock, waveID, campaignID, planID, 1) // LIMIT 1
	gov := newFamilyGovernorWithMode(db, FamilyGovernorOn)
	fgRun(t, mock, gov, waveID, 1)
}

// ON deny: no content read, no claim, no INSERT — the wave is completed with a
// family_governor note in last_error and the transaction commits.
func TestFamilyGovernorHook_OnDenyCompletesWave(t *testing.T) {
	fgDispatcherEnv(t)
	db, mock := fgNewMock(t)
	mock.MatchExpectationsInOrder(false)
	waveID, campaignID, planID, orgID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	expectGovernedWavePrelude(mock, waveID, campaignID, planID, orgID, 500, "sbcglobal", fgTestDomain)
	fgExpectContract(mock, fgTestLane, 10000)
	fgExpectSpend(mock, fgTestDomain, 12000)
	fgExpectLedger(mock, waveID.String(), fgTestDomain, "sbcglobal", FamilyGovernorOn, 500, 10000, 12000, 0, "deny")
	mock.ExpectExec(`(?s)UPDATE mailing_campaign_waves\s+SET status = 'completed'.*last_error = COALESCE\(last_error, ''\) \|\| \$2`).
		WithArgs(waveID.String(), " [family_governor: deny ceiling=10000 spent=12000]").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	gov := newFamilyGovernorWithMode(db, FamilyGovernorOn)
	fgRun(t, mock, gov, waveID, 0)
}

// ON within: allowed == remaining → nothing changes.
func TestFamilyGovernorHook_OnWithinUntouched(t *testing.T) {
	fgDispatcherEnv(t)
	db, mock := fgNewMock(t)
	mock.MatchExpectationsInOrder(false)
	waveID, campaignID, planID, orgID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	expectGovernedWavePrelude(mock, waveID, campaignID, planID, orgID, 2, "cox", fgTestDomain)
	fgExpectContract(mock, fgTestLane, 10)
	fgExpectSpend(mock, fgTestDomain, 3)
	fgExpectLedger(mock, waveID.String(), fgTestDomain, "cox", FamilyGovernorOn, 2, 10, 3, 2, "within")
	expectContentAndClaim(mock, waveID, campaignID, planID, 2)
	gov := newFamilyGovernorWithMode(db, FamilyGovernorOn)
	fgRun(t, mock, gov, waveID, 2)
}

// ON, non-family plan (gmail): zero governor statements, full enqueue.
func TestFamilyGovernorHook_OnNonFamilyUntouched(t *testing.T) {
	fgDispatcherEnv(t)
	db, mock := fgNewMock(t)
	mock.MatchExpectationsInOrder(false)
	waveID, campaignID, planID, orgID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	expectGovernedWavePrelude(mock, waveID, campaignID, planID, orgID, 2, "gmail", fgTestDomain)
	expectContentAndClaim(mock, waveID, campaignID, planID, 2)
	gov := newFamilyGovernorWithMode(db, FamilyGovernorOn)
	fgRun(t, mock, gov, waveID, 2)
}

// ON, no active contract for the lane: contract read only, full enqueue.
func TestFamilyGovernorHook_OnNoContractUntouched(t *testing.T) {
	fgDispatcherEnv(t)
	db, mock := fgNewMock(t)
	mock.MatchExpectationsInOrder(false)
	waveID, campaignID, planID, orgID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	expectGovernedWavePrelude(mock, waveID, campaignID, planID, orgID, 2, "att", fgTestDomain)
	fgExpectContract(mock, fgTestLane, nil)
	expectContentAndClaim(mock, waveID, campaignID, planID, 2)
	gov := newFamilyGovernorWithMode(db, FamilyGovernorOn)
	fgRun(t, mock, gov, waveID, 2)
}

// ON, governor DB error: fail OPEN — the wave enqueues in full and the wave
// transaction is NOT poisoned (the governor ran on db, not tx).
func TestFamilyGovernorHook_OnErrorFailsOpen(t *testing.T) {
	fgDispatcherEnv(t)
	db, mock := fgNewMock(t)
	mock.MatchExpectationsInOrder(false)
	waveID, campaignID, planID, orgID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	expectGovernedWavePrelude(mock, waveID, campaignID, planID, orgID, 2, "yahoo", fgTestDomain)
	mock.ExpectQuery(`FROM drip_dispatch_contracts`).WithArgs(fgTestLane).
		WillReturnError(errors.New("canceling statement due to statement timeout"))
	expectContentAndClaim(mock, waveID, campaignID, planID, 2)
	gov := newFamilyGovernorWithMode(db, FamilyGovernorOn)
	fgRun(t, mock, gov, waveID, 2)
}

// Plan sending_domain is the lane key even when the profile lookup differs;
// an empty plan domain falls back to the profile's sending_domain.
func TestFamilyGovernorHook_EmptyPlanDomainFallsBackToProfile(t *testing.T) {
	fgDispatcherEnv(t)
	db, mock := fgNewMock(t)
	mock.MatchExpectationsInOrder(false)
	waveID, campaignID, planID, orgID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mock.ExpectBegin()
	mock.ExpectQuery(`w.campaign_id, w.isp_plan_id`).
		WithArgs(waveID.String()).
		WillReturnRows(sqlmock.NewRows(waveMetaColumns).AddRow(
			campaignID, planID, orgID, "planned", "scheduled", "ready",
			testScheduledAt, testScheduledAt, 2, 0, nil, "yahoo", ""))
	mock.ExpectQuery(`COALESCE\(sp.sending_domain`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"sending_domain"}).AddRow("m.profile.com"))
	mock.ExpectExec(`UPDATE mailing_campaign_waves\s+SET status = 'enqueuing'`).
		WithArgs(waveID.String()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaigns\s+SET status = 'sending'`).
		WithArgs(campaignID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE mailing_campaign_isp_plans\s+SET status = 'running'`).
		WithArgs(planID).WillReturnResult(sqlmock.NewResult(0, 1))
	fgExpectContract(mock, "broadcast-family.m.profile.com", 100)
	fgExpectSpend(mock, "m.profile.com", 0)
	fgExpectLedger(mock, waveID.String(), "m.profile.com", "yahoo", FamilyGovernorShadow, 2, 100, 0, 2, "within")
	expectContentAndClaim(mock, waveID, campaignID, planID, 2)
	gov := newFamilyGovernorWithMode(db, FamilyGovernorShadow)
	fgRun(t, mock, gov, waveID, 2)
}

// Scheduler / consumer plumbing: the setter lands the governor where the two
// dispatch entrypoints read it.
func TestFamilyGovernorSetters(t *testing.T) {
	gov := newFamilyGovernorWithMode(nil, FamilyGovernorShadow)
	s := NewPMTAWaveScheduler(nil, nil, "")
	if s.familyGovernor != nil {
		t.Fatal("scheduler must start without a governor")
	}
	s.SetFamilyGovernor(gov)
	if s.familyGovernor != gov {
		t.Fatal("SetFamilyGovernor did not land on the scheduler")
	}
	c := NewPMTAWaveConsumer(nil, "", nil)
	c.SetFamilyGovernor(gov)
	if c.familyGovernor != gov {
		t.Fatal("SetFamilyGovernor did not land on the consumer")
	}
}
