package api

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// TestStagePMTADraftCampaign_WritesAudienceLinks pins the W1 stage path: after
// the draft row commits and persists, the include/exclude segment UUIDs from
// the payload land in mailing_campaign_audiences (deduped, non-UUID entries
// dropped) — and a link failure could never have failed the stage, because the
// writes run after the durable-commit verification.
func TestStagePMTADraftCampaign_WritesAudienceLinks(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	orgID := uuid.New().String()
	segA := uuid.New().String()
	segB := uuid.New().String()
	segC := uuid.New().String()

	mock.ExpectBegin()
	// resolvePMTASendingProfile by-domain lookup — no profile is tolerated.
	mock.ExpectQuery(`FROM mailing_sending_profiles`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "from_email", "from_name", "reply_email"}))
	mock.ExpectExec(`INSERT INTO mailing_campaigns`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	// verifyCampaignPersisted durable-readback.
	mock.ExpectQuery(`SELECT id::text FROM mailing_campaigns`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uuid.New().String()))
	// W1: include links (segA deduped, the non-UUID name dropped → 2 rows)…
	mock.ExpectExec(`INSERT INTO mailing_campaign_audiences`).
		WillReturnResult(sqlmock.NewResult(0, 2))
	// …then exclude links (segC → 1 row).
	mock.ExpectExec(`INSERT INTO mailing_campaign_audiences`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	draft := engine.PMTACampaignDraftInput{
		CampaignInput: engine.PMTACampaignInput{
			Name:              "aug04 - Discount Blog - W1-CLK1-MSFT - fidelity",
			SendingDomain:     "em.discountblog.com",
			InclusionSegments: []string{segA, segB, "DB 30D Openers (name, not a uuid)", segA},
			ExclusionSegments: []string{segC},
			Variants: []engine.ContentVariant{{
				VariantName: "A", Subject: "s", HTMLContent: "<html>x</html>", SplitPercent: 100,
			}},
		},
	}
	cc := &campaignColumnCache{cols: map[string]bool{}}

	result, err := stagePMTADraftCampaign(context.Background(), db, orgID, draft, cc)
	if err != nil {
		t.Fatalf("stagePMTADraftCampaign: %v", err)
	}
	if result.Status != "draft" {
		t.Fatalf("status = %q, want draft", result.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// TestWriteCampaignAudienceLinks_KillSwitchWritesNothing is the negative
// control for the DISABLE_CAMPAIGN_AUDIENCE_LINKS gate: the insert expectation
// below is ARMED — if the gate ever stops firing, the insert matches, written
// becomes non-zero, and this test goes red.
func TestWriteCampaignAudienceLinks_KillSwitchWritesNothing(t *testing.T) {
	t.Setenv(audienceLinksKillSwitch, "1")

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO mailing_campaign_audiences`).
		WillReturnResult(sqlmock.NewResult(0, 2))

	written := writeCampaignAudienceLinks(context.Background(), db, uuid.New().String(),
		[]string{uuid.New().String(), uuid.New().String()},
		[]string{uuid.New().String()})

	if written != 0 {
		t.Fatalf("kill switch on but %d links were written", written)
	}
	if err := mock.ExpectationsWereMet(); err == nil {
		t.Fatal("kill switch must skip ALL DB access, but the link insert ran")
	}
}

// TestWriteCampaignAudienceLinks_NoValidUUIDsIsNoOp: segment NAMES (the legacy
// payload shape) never reach the DB — no valid UUID, no statement.
func TestWriteCampaignAudienceLinks_NoValidUUIDsIsNoOp(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO mailing_campaign_audiences`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	written := writeCampaignAudienceLinks(context.Background(), db, uuid.New().String(),
		[]string{"DB 30D Openers", "HT 30D Clickers"}, nil)

	if written != 0 {
		t.Fatalf("no valid UUIDs but %d links were written", written)
	}
	if err := mock.ExpectationsWereMet(); err == nil {
		t.Fatal("name-only segment lists must not produce a DB write")
	}
}
