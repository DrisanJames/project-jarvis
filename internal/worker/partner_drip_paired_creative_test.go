package worker

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// Paired creative+subject rotation (operator 2026-08-19: "I want to rotate the
// creative and subject line pairs").
//
// Before this, resolveOfferCreative took ONE creative (LIMIT 1) and rotated the
// subject independently off the offer's pool — so a creative could never ship
// with the subject written for it, and adding a second creative silently
// REPLACED the first rather than joining a rotation.
//
// The opt-in is mailing_offer_creatives.subject_line_id. These tests pin both
// sides, and the negative path is the important one: 13 live offers already
// hold multiple creatives with zero pairing (Fidelity 35, CarShield 11, Metal
// Roofing 5 …) where an operator deliberately chose one canonical row. Those
// must not start rotating.

func newPairedRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"html_content", "subject_line", "from_name"})
}

// Two declared pairs => the pair rotates as a unit: the subject that comes back
// is always the one linked to the creative that came back.
func TestResolveOfferCreative_PairedRotationKeepsPairsTogether(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM mailing_offer_creatives`).
		WillReturnRows(newPairedRows().
			AddRow("<html>A</html>", "SUBJECT-A", "From A").
			AddRow("<html>B</html>", "SUBJECT-B", "From B"))
	mock.ExpectQuery(`FROM mailing_offer_subject_lines`).
		WillReturnRows(sqlmock.NewRows([]string{"subject_line"}).
			AddRow("SUBJECT-A").AddRow("SUBJECT-B"))
	mock.ExpectQuery(`FROM mailing_offer_from_names`).
		WillReturnRows(sqlmock.NewRows([]string{"from_name"}).
			AddRow("From A").AddRow("From B"))

	po := &PartnerDripOrchestrator{db: db}
	got, err := po.resolveOfferCreative(context.Background(), "offer-1", "wcl")
	if err != nil {
		t.Fatalf("resolveOfferCreative: %v", err)
	}

	// Whichever bucket the clock lands in, the creative and subject must be
	// the SAME pair — that is the whole point of the change.
	switch got.htmlBody {
	case "<html>A</html>":
		if got.subject != "SUBJECT-A" || got.fromName != "From A" {
			t.Fatalf("pair A broken: subject=%q from=%q", got.subject, got.fromName)
		}
	case "<html>B</html>":
		if got.subject != "SUBJECT-B" || got.fromName != "From B" {
			t.Fatalf("pair B broken: subject=%q from=%q", got.subject, got.fromName)
		}
	default:
		t.Fatalf("unexpected creative: %q", got.htmlBody)
	}
}

// NEGATIVE PATH — the blast-radius guard. An offer with several creatives but
// NO subject_line_id must keep the historical behaviour: one canonical creative
// chosen by status/updated_at, subject rotated independently. If this test ever
// starts failing, 13 live offers have quietly begun rotating creatives.
func TestResolveOfferCreative_UnpairedOfferDoesNotRotateCreative(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// No pairs declared.
	mock.ExpectQuery(`FROM mailing_offer_creatives`).WillReturnRows(newPairedRows())
	// Historical single-creative pick.
	mock.ExpectQuery(`FROM mailing_offer_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"html_content"}).AddRow("<html>CANONICAL</html>"))
	mock.ExpectQuery(`FROM mailing_offer_subject_lines`).
		WillReturnRows(sqlmock.NewRows([]string{"subject_line"}).AddRow("S1").AddRow("S2"))
	mock.ExpectQuery(`FROM mailing_offer_from_names`).
		WillReturnRows(sqlmock.NewRows([]string{"from_name"}).AddRow("F1"))

	po := &PartnerDripOrchestrator{db: db}
	got, err := po.resolveOfferCreative(context.Background(), "offer-legacy", "hws")
	if err != nil {
		t.Fatalf("resolveOfferCreative: %v", err)
	}
	if got.htmlBody != "<html>CANONICAL</html>" {
		t.Fatalf("unpaired offer must serve the canonical creative, got %q", got.htmlBody)
	}
	if got.subject != "S1" && got.subject != "S2" {
		t.Fatalf("subject must still rotate off the pool, got %q", got.subject)
	}
}

// A SINGLE declared pair is not a rotation — it must fall through to the
// historical path so pairing one creative mid-migration cannot pin the lane to
// a half-configured pool.
func TestResolveOfferCreative_SinglePairFallsThrough(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM mailing_offer_creatives`).
		WillReturnRows(newPairedRows().AddRow("<html>ONLY</html>", "SUBJECT-ONLY", ""))
	mock.ExpectQuery(`FROM mailing_offer_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"html_content"}).AddRow("<html>CANONICAL</html>"))
	mock.ExpectQuery(`FROM mailing_offer_subject_lines`).
		WillReturnRows(sqlmock.NewRows([]string{"subject_line"}).AddRow("S1"))
	mock.ExpectQuery(`FROM mailing_offer_from_names`).
		WillReturnRows(sqlmock.NewRows([]string{"from_name"}).AddRow("F1"))

	po := &PartnerDripOrchestrator{db: db}
	got, err := po.resolveOfferCreative(context.Background(), "offer-half", "wcl")
	if err != nil {
		t.Fatalf("resolveOfferCreative: %v", err)
	}
	if got.htmlBody != "<html>CANONICAL</html>" {
		t.Fatalf("single pair must fall through, got %q", got.htmlBody)
	}
}
