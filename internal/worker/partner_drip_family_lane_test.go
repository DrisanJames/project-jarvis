package worker

// yahoo_family lane pins (2026-09-07). Every test here is a NEGATIVE control on
// the promise that matters: this lane can never ship an offer, and an open or
// a click takes a record out of the ladder.

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

func TestFamilyLane_NewsletterGuardRefusesEveryOfferShape(t *testing.T) {
	clean := `<html><body><a href="https://www.discountblog.com/blog/how-to-save">Read</a><img src="https://www.discountblog.com/i.png"></body></html>`
	if err := newsletterGuard(clean); err != nil {
		t.Fatalf("clean newsletter refused: %v", err)
	}
	bad := map[string]string{
		"money host cratoolpro": `<a href="https://www.cratoolpro.com/K4C5/PS82/?source_id=email">go</a>`,
		"money host codefortwo": `<a href="https://www.codefortwo.com/K4C5ZLC/PS8241/">go</a>`,
		"offer gateway /o/":     `<a href="https://t.m.discountblog.com/o/t.m.discountblog.com/470d0831-8b2d-4b8a-8753-2515c23c9f1e/w6ex7q8qzy/3abcaa96-94b7-4451-bf82-250506186d14">go</a>`,
		"everflow hop":          `<a href="https://tracking.everflow.io/x">go</a>`,
		"affiliate marker":      `<a href="https://site.com/aff/123">go</a>`,
		"case-insensitive":      `<a href="https://WWW.CRATOOLPRO.COM/x">go</a>`,
		"empty":                 "   ",
	}
	for name, html := range bad {
		if err := newsletterGuard(html); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	if !errors.Is(newsletterGuard(bad["money host cratoolpro"]), ErrNewsletterOffer) {
		t.Fatal("offer refusal must carry ErrNewsletterOffer")
	}
}

func TestFamilyLane_IsFamilyLaneAndHomeBrandPin(t *testing.T) {
	if !IsFamilyLane("yahoo_family") || !IsFamilyLane(" YAHOO_FAMILY ") || IsFamilyLane("wcl_remail") {
		t.Fatal("IsFamilyLane")
	}
	t.Setenv("PARTNER_DRIP_CONVERTERS_PIN_DISABLED", "")
	if homeBrandPinSQL("yahoo_family", "db", "q") == "" {
		t.Fatal("the family lane must pin to home_brand like the converters lanes")
	}
	if homeBrandPinSQL("wcl_remail", "db", "q") != "" {
		t.Fatal("non-pinned lanes must stay unpinned")
	}
}

func TestFamilyLane_LoadStudioCreative_RequiresApprovedNewsletterStamp(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	po := &PartnerDripOrchestrator{db: db}
	id := "11111111-1111-1111-1111-111111111111"

	// 1) approved newsletter → html + subject/preheader fill only when blank
	mock.ExpectQuery(`FROM mailing_creatives\s+WHERE id = \$1 AND approval_status = 'approved' AND approved_by = ANY\(\$2::text\[\]\)`).
		WithArgs(id, pq.Array(FamilyLaneProducerStamps)).
		WillReturnRows(sqlmock.NewRows([]string{"html", "subject", "preheader", "filename", "brand_code"}).
			AddRow(`<a href="https://www.discountblog.com/blog/x">x</a>`, "Studio subject", "Studio pre", "nl-db-family-s2.html", "discountblog.com"))
	c := creativeRec{subject: "Row subject"}
	if err := po.loadStudioCreative(context.Background(), id, &c); err != nil {
		t.Fatalf("approved newsletter must load: %v", err)
	}
	if c.subject != "Row subject" || c.preheader != "Studio pre" || c.filename != "nl-db-family-s2.html" || !strings.Contains(c.htmlBody, "discountblog") {
		t.Fatalf("fields: %+v", c)
	}

	// 2) not approved / wrong stamp → no rows → refused (fail closed)
	mock.ExpectQuery(`FROM mailing_creatives`).WithArgs(id, pq.Array(FamilyLaneProducerStamps)).
		WillReturnRows(sqlmock.NewRows([]string{"html", "subject", "preheader", "filename", "brand_code"}))
	if err := po.loadStudioCreative(context.Background(), id, &creativeRec{}); err == nil {
		t.Fatal("an unapproved or non-newsletter creative must be refused")
	}

	// 3) approved but carries an offer → guard refuses
	mock.ExpectQuery(`FROM mailing_creatives`).WithArgs(id, pq.Array(FamilyLaneProducerStamps)).
		WillReturnRows(sqlmock.NewRows([]string{"html", "subject", "preheader", "filename", "brand_code"}).
			AddRow(`<a href="https://www.cratoolpro.com/x/y/">buy</a>`, "s", "p", "nl-db-family-s1.html", "discountblog.com"))
	err = po.loadStudioCreative(context.Background(), id, &creativeRec{})
	if !errors.Is(err, ErrNewsletterOffer) {
		t.Fatalf("offer inside an approved row must be refused with ErrNewsletterOffer, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFamilyLane_EngagedExitSQLShape(t *testing.T) {
	markOpens, closeLadder := familyEngagedExitSQL(true)
	for _, want := range []string{"te.event_type = 'opened'", "d.vertical = $1", humanVerdictSQL, "q.engaged_at IS NULL", "q.vertical = $1"} {
		if !strings.Contains(markOpens, want) {
			t.Fatalf("markOpens missing %q", want)
		}
	}
	for _, want := range []string{"terminal_reason = 'engaged_exit'", "next_touch_at = NULL", "engaged_at IS NOT NULL", "terminal_reason IS NULL", "vertical = $1"} {
		if !strings.Contains(closeLadder, want) {
			t.Fatalf("closeLadder missing %q", want)
		}
	}
	unfiltered, _ := familyEngagedExitSQL(false)
	if strings.Contains(unfiltered, humanVerdictSQL) {
		t.Fatal("verdict filter must be droppable by the kill switch")
	}
	// The lane's follow-up claim already excludes engaged_at IS NOT NULL rows
	// (engagedExitSQL); closing the ladder is belt and braces plus the audit reason.
	if !regexp.MustCompile(`engaged_at IS NULL`).MatchString(engagedExitSQL("yahoo_family", "")) {
		t.Fatal("follow-up claim for the family lane must exclude engaged records")
	}
}
