package api

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func seedsFixture(t *testing.T) (*pmtaAudiencePlan, pmtaNormalizedCampaign) {
	t.Helper()
	aud := &pmtaAudiencePlan{
		RecipientsByISP: map[string][]pmtaSelectedRecipient{
			"gmail":     {{SubscriberID: "s1", Email: "a@gmail.com", ISP: "gmail", SelectionRank: 1}},
			"microsoft": {{SubscriberID: "s2", Email: "b@outlook.com", ISP: "microsoft", SelectionRank: 1}},
			"apple":     {},
		},
		CountsByISP:   map[string]int{"gmail": 1, "microsoft": 1, "apple": 0},
		SelectedTotal: 2,
	}
	norm := pmtaNormalizedCampaign{Plans: []pmtaNormalizedPlan{{ISP: "gmail"}, {ISP: "microsoft"}, {ISP: "apple"}}}
	return aud, norm
}

func seedRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"email", "isp", "subscriber_id"}).
		AddRow("inbox@seed-gmail.projectjarvis.io", "gmail", "11111111-1111-1111-1111-111111111111").
		AddRow("inbox@seed-microsoft.projectjarvis.io", "microsoft", "22222222-2222-2222-2222-222222222222").
		AddRow("inbox@seed-apple.projectjarvis.io", "apple", "33333333-3333-3333-3333-333333333333")
}

// A seed joins every lane the campaign really mails, after planning, and never
// a lane with no real recipients.
func TestAppendInboxScoreSeeds_JoinsMailedLanesOnly(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`FROM mailing_inbox_score_seeds`).WithArgs("org1").WillReturnRows(seedRows())
	aud, norm := seedsFixture(t)
	seeded := appendInboxScoreSeeds(context.Background(), db, "org1", norm, aud)
	if len(seeded) != 2 || seeded[0] != "gmail" || seeded[1] != "microsoft" {
		t.Fatalf("seeded = %v, want [gmail microsoft]", seeded)
	}
	if len(aud.RecipientsByISP["gmail"]) != 2 || aud.RecipientsByISP["gmail"][1].Email != "inbox@seed-gmail.projectjarvis.io" {
		t.Fatalf("gmail seed not appended: %+v", aud.RecipientsByISP["gmail"])
	}
	r := aud.RecipientsByISP["gmail"][1]
	if r.SourceType != "inbox_score_seed" || r.SelectionRank != 2 || r.SubscriberID != "11111111-1111-1111-1111-111111111111" || r.ISP != "gmail" {
		t.Fatalf("seed recipient fields wrong: %+v", r)
	}
	if len(aud.RecipientsByISP["apple"]) != 0 || aud.CountsByISP["apple"] != 0 {
		t.Fatalf("apple lane had no real recipients and must stay empty: %+v", aud.RecipientsByISP["apple"])
	}
	if aud.CountsByISP["gmail"] != 2 || aud.CountsByISP["microsoft"] != 2 || aud.SelectedTotal != 4 {
		t.Fatalf("counts not bumped: %+v total=%d", aud.CountsByISP, aud.SelectedTotal)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Re-finalization must not double-seed a lane.
func TestAppendInboxScoreSeeds_Idempotent(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	mock.ExpectQuery(`FROM mailing_inbox_score_seeds`).WillReturnRows(seedRows())
	aud, norm := seedsFixture(t)
	aud.RecipientsByISP["gmail"] = append(aud.RecipientsByISP["gmail"], pmtaSelectedRecipient{Email: "INBOX@seed-gmail.projectjarvis.io", ISP: "gmail"})
	aud.CountsByISP["gmail"] = 2
	seeded := appendInboxScoreSeeds(context.Background(), db, "org1", norm, aud)
	if len(seeded) != 1 || seeded[0] != "microsoft" {
		t.Fatalf("seeded = %v, want [microsoft]", seeded)
	}
	if len(aud.RecipientsByISP["gmail"]) != 2 {
		t.Fatalf("gmail seed duplicated: %+v", aud.RecipientsByISP["gmail"])
	}
}

// The kill switch and a broken table both leave the audience untouched and
// never fail the campaign.
func TestAppendInboxScoreSeeds_KillSwitchAndErrors(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	aud, norm := seedsFixture(t)

	os.Setenv("INBOX_SCORE_SEEDS_DISABLED", "1")
	if seeded := appendInboxScoreSeeds(context.Background(), db, "org1", norm, aud); seeded != nil {
		t.Fatalf("kill switch ignored: %v", seeded)
	}
	os.Unsetenv("INBOX_SCORE_SEEDS_DISABLED")

	mock.ExpectQuery(`FROM mailing_inbox_score_seeds`).WillReturnError(errors.New(`relation "mailing_inbox_score_seeds" does not exist`))
	if seeded := appendInboxScoreSeeds(context.Background(), db, "org1", norm, aud); seeded != nil {
		t.Fatalf("query error must skip seeds, got %v", seeded)
	}
	if aud.SelectedTotal != 2 || len(aud.RecipientsByISP["gmail"]) != 1 {
		t.Fatalf("audience changed on error: total=%d gmail=%d", aud.SelectedTotal, len(aud.RecipientsByISP["gmail"]))
	}
}

// A seed whose domain does not classify as its lane is refused (misrouting
// guard: the seed would otherwise ride the wrong transport and lie about it).
func TestAppendInboxScoreSeeds_RefusesMisclassifiedSeed(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	mock.ExpectQuery(`FROM mailing_inbox_score_seeds`).WillReturnRows(
		sqlmock.NewRows([]string{"email", "isp", "subscriber_id"}).AddRow("nxproof@mail.projectjarvis.io", "gmail", "44444444-4444-4444-4444-444444444444"))
	aud, norm := seedsFixture(t)
	if seeded := appendInboxScoreSeeds(context.Background(), db, "org1", norm, aud); seeded != nil {
		t.Fatalf("misclassified seed must be skipped, got %v", seeded)
	}
}
