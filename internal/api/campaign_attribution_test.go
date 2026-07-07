package api

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// TestParseOfferTokenFromCampaignName pins the name-inference gate: the offer
// token is the last " - " token, extracted ONLY when the name carries the
// board wave-lane convention (W\d+-). Drip/proof/test names never yield a token.
func TestParseOfferTokenFromCampaignName(t *testing.T) {
	cases := []struct {
		name      string
		campaign  string
		wantToken string
		wantOK    bool
	}{
		{
			name:      "board name with offer slug",
			campaign:  "jul07 - Warranty For You - W1-CLK1-MSFT - fidelity",
			wantToken: "fidelity",
			wantOK:    true,
		},
		{
			name:     "partner drip name has no wave token",
			campaign: "[partner-drip] term_life db 2026-07-07 08:00",
			wantOK:   false,
		},
		{
			name:     "proof/test name without wave token",
			campaign: "PROOF - metal sectional - operator eyeball",
			wantOK:   false,
		},
		{
			name:      "wave token with unmapped slug still yields the token",
			campaign:  "jul07 - Discount Blog - W2-ENG1-APPLE - Sams-Club",
			wantToken: "sams-club",
			wantOK:    true,
		},
		{
			name:     "name ending at the wave lane carries no offer token",
			campaign: "jul07 - Discount Blog - W1-CLK1-MSFT",
			wantOK:   false,
		},
		{
			name:     "empty name",
			campaign: "",
			wantOK:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token, ok := parseOfferTokenFromCampaignName(tc.campaign)
			if ok != tc.wantOK {
				t.Fatalf("parseOfferTokenFromCampaignName(%q) ok = %v, want %v", tc.campaign, ok, tc.wantOK)
			}
			if ok && token != tc.wantToken {
				t.Fatalf("parseOfferTokenFromCampaignName(%q) = %q, want %q", tc.campaign, token, tc.wantToken)
			}
		})
	}
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestStampCampaignAttribution_NameInferredMapped is the happy path: a board
// name resolves through the slug map to a mailing_offers row; creative and
// subject identities upsert; a single COALESCE-guarded UPDATE stamps the
// campaign with attribution_source='name_inferred'.
func TestStampCampaignAttribution_NameInferredMapped(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	orgID := uuid.New().String()
	campaignID := uuid.New().String()
	offerID := uuid.New().String()
	creativeID := uuid.New().String()
	subjectID := uuid.New().String()
	html := "<html><body>deal</body></html>"
	subject := "Your quote is waiting"

	mock.ExpectQuery(`SELECT attribution_source IS NOT NULL FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"stamped"}).AddRow(false))
	mock.ExpectQuery(`FROM mailing_offer_slug_map`).
		WithArgs("fidelity").
		WillReturnRows(sqlmock.NewRows([]string{"everflow_offer_id"}).AddRow("9539"))
	mock.ExpectQuery(`SELECT id::text FROM mailing_offers`).
		WithArgs(orgID, "9539").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(offerID))
	mock.ExpectQuery(`INSERT INTO mailing_creative_identities`).
		WithArgs(orgID, md5hex(html), "fidelity", subject, "Warranty For You", campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(creativeID))
	mock.ExpectQuery(`INSERT INTO mailing_subject_identities`).
		WithArgs(orgID, md5hex(subject), subject).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(subjectID))
	mock.ExpectExec(`UPDATE mailing_campaigns`).
		WithArgs(campaignID, offerID, "fidelity", creativeID, subjectID, "name_inferred").
		WillReturnResult(sqlmock.NewResult(0, 1))

	stampCampaignAttribution(context.Background(), db, orgID, campaignID,
		engine.PMTACampaignInput{},
		"jul07 - Warranty For You - W1-CLK1-MSFT - fidelity",
		subject, html, "Warranty For You")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// TestStampCampaignAttribution_UnmappedSlugStampsKeyOnly: a wave-convention
// name whose token the slug map does not know still stamps offer_key (the
// operational key) with offer_id NULL.
func TestStampCampaignAttribution_UnmappedSlugStampsKeyOnly(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	orgID := uuid.New().String()
	campaignID := uuid.New().String()
	creativeID := uuid.New().String()
	subjectID := uuid.New().String()
	html := "<html><body>club</body></html>"
	subject := "Members save more"

	mock.ExpectQuery(`SELECT attribution_source IS NOT NULL FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"stamped"}).AddRow(false))
	mock.ExpectQuery(`FROM mailing_offer_slug_map`).
		WithArgs("sams-club").
		WillReturnRows(sqlmock.NewRows([]string{"everflow_offer_id"})) // no rows → unmapped
	mock.ExpectQuery(`INSERT INTO mailing_creative_identities`).
		WithArgs(orgID, md5hex(html), "sams-club", subject, "Discount Blog", campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(creativeID))
	mock.ExpectQuery(`INSERT INTO mailing_subject_identities`).
		WithArgs(orgID, md5hex(subject), subject).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(subjectID))
	mock.ExpectExec(`UPDATE mailing_campaigns`).
		WithArgs(campaignID, nil, "sams-club", creativeID, subjectID, "name_inferred").
		WillReturnResult(sqlmock.NewResult(0, 1))

	stampCampaignAttribution(context.Background(), db, orgID, campaignID,
		engine.PMTACampaignInput{},
		"jul07 - Discount Blog - W2-ENG1-APPLE - Sams-Club",
		subject, html, "Discount Blog")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// TestStampCampaignAttribution_PayloadOfferID: a payload UUID (drip/agent
// deploys) wins over name inference — offer_id is the payload value,
// attribution_source='payload', offer_key resolved best-effort from
// mailing_offers.landing_page_slug.
func TestStampCampaignAttribution_PayloadOfferID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	orgID := uuid.New().String()
	campaignID := uuid.New().String()
	payloadOfferID := uuid.New()
	creativeID := uuid.New().String()
	subjectID := uuid.New().String()
	html := "<html><body>term life</body></html>"
	subject := "A term life check-in"

	mock.ExpectQuery(`SELECT attribution_source IS NOT NULL FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"stamped"}).AddRow(false))
	mock.ExpectQuery(`FROM mailing_offers`).
		WithArgs(payloadOfferID.String(), orgID).
		WillReturnRows(sqlmock.NewRows([]string{"slug", "ef"}).AddRow("term-life", "8511"))
	mock.ExpectQuery(`INSERT INTO mailing_creative_identities`).
		WithArgs(orgID, md5hex(html), "term-life", subject, "My Own Health", campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(creativeID))
	mock.ExpectQuery(`INSERT INTO mailing_subject_identities`).
		WithArgs(orgID, md5hex(subject), subject).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(subjectID))
	mock.ExpectExec(`UPDATE mailing_campaigns`).
		WithArgs(campaignID, payloadOfferID.String(), "term-life", creativeID, subjectID, "payload").
		WillReturnResult(sqlmock.NewResult(0, 1))

	stampCampaignAttribution(context.Background(), db, orgID, campaignID,
		engine.PMTACampaignInput{OfferID: payloadOfferID.String()},
		"[partner-drip] term_life mh 2026-07-07",
		subject, html, "My Own Health")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// TestStampCampaignAttribution_KillSwitchSkipsEverything: with
// DISABLE_ATTRIBUTION_STAMPING=1 no DB statement may run. The slug-map
// expectation below must therefore go UNMET — ExpectationsWereMet returning
// nil would mean the stamp executed despite the kill switch.
func TestStampCampaignAttribution_KillSwitchSkipsEverything(t *testing.T) {
	t.Setenv(attributionStampKillSwitch, "1")

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM mailing_offer_slug_map`).
		WillReturnRows(sqlmock.NewRows([]string{"everflow_offer_id"}).AddRow("9539"))

	stampCampaignAttribution(context.Background(), db, uuid.New().String(), uuid.New().String(),
		engine.PMTACampaignInput{},
		"jul07 - Warranty For You - W1-CLK1-MSFT - fidelity",
		"subject", "<html>x</html>", "Warranty For You")

	if err := mock.ExpectationsWereMet(); err == nil {
		t.Fatal("kill switch must skip ALL DB access, but the slug-map lookup ran")
	}
}

// TestStampCampaignAttribution_NothingResolvedIsNoOp: a non-board name with no
// payload offer and empty content stamps nothing — zero DB statements (the
// historical-NULL semantics stay untouched).
func TestStampCampaignAttribution_NothingResolvedIsNoOp(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	campaignID := uuid.New().String()
	mock.ExpectQuery(`SELECT attribution_source IS NOT NULL FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"stamped"}).AddRow(false))
	mock.ExpectExec(`UPDATE mailing_campaigns`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	stampCampaignAttribution(context.Background(), db, uuid.New().String(), campaignID,
		engine.PMTACampaignInput{},
		"PROOF - operator eyeball", "", "", "")

	if err := mock.ExpectationsWereMet(); err == nil {
		t.Fatal("nothing resolved must be a no-op, but the campaign UPDATE ran")
	}
}

// TestExtractMoneySlugsFromHTML pins the html-inference extractor: distinct
// lowercased 2nd-path-segment slugs of source_id=email money links only.
func TestExtractMoneySlugsFromHTML(t *testing.T) {
	cases := []struct {
		name string
		html string
		want []string
	}{
		{
			name: "single offer, repeated links",
			html: `<a href="https://www.k8k0hfdt.com/3QJ6DW/3QQG71/?source_id=email&sub1=x">Go</a>
			       <a href="https://www.k8k0hfdt.com/3QJ6DW/3QQG71/?source_id=email">Go again</a>`,
			want: []string{"3qqg71"},
		},
		{
			name: "escaped ampersands and params before the marker still match",
			html: `<a href='https://trk.example.com/AB12/CD34/?sub2=db&amp;source_id=email&amp;sub1=y'>x</a>`,
			want: []string{"cd34"},
		},
		{
			name: "multi-offer newsletter yields both slugs",
			html: `<a href="https://h.com/AAAA/BBBB/?source_id=email">1</a><a href="https://h.com/AAAA/CCCC/?source_id=email">2</a>`,
			want: []string{"bbbb", "cccc"},
		},
		{
			name: "non-money links are ignored",
			html: `<a href="https://discountblog.com/unsubscribe/xyz/">bye</a><img src="https://img.h.com/a/b/c.png">`,
			want: nil,
		},
		{
			name: "bare-URL scanner shapes without the marker are ignored",
			html: `<a href="https://www.k8k0hfdt.com/3QJ6DW/3QQG71/">no marker</a>`,
			want: nil,
		},
		{
			name: "empty html",
			html: "",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractMoneySlugsFromHTML(tc.html)
			if len(got) != len(tc.want) {
				t.Fatalf("extractMoneySlugsFromHTML() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("extractMoneySlugsFromHTML() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestStampCampaignAttribution_HTMLInferred: a one-off broadcast (no payload
// offer, no wave token in the name) whose creative carries exactly one money
// slug stamps offer_key + offer_id via the slug map with
// attribution_source='html_inferred' — the catchall the name gate refuses.
func TestStampCampaignAttribution_HTMLInferred(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	orgID := uuid.New().String()
	campaignID := uuid.New().String()
	offerID := uuid.New().String()
	creativeID := uuid.New().String()
	subjectID := uuid.New().String()
	html := `<html><a href="https://www.k8k0hfdt.com/3QJ6DW/3QQG71/?source_id=email&sub1=a&sub2=b">Save now</a></html>`
	subject := "One-off resend"

	mock.ExpectQuery(`SELECT attribution_source IS NOT NULL FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"stamped"}).AddRow(false))
	mock.ExpectQuery(`FROM mailing_offer_slug_map`).
		WithArgs("3qqg71").
		WillReturnRows(sqlmock.NewRows([]string{"everflow_offer_id"}).AddRow("2891"))
	mock.ExpectQuery(`SELECT id::text FROM mailing_offers`).
		WithArgs(orgID, "2891").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(offerID))
	mock.ExpectQuery(`INSERT INTO mailing_creative_identities`).
		WithArgs(orgID, md5hex(html), "3qqg71", subject, "Discount Blog", campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(creativeID))
	mock.ExpectQuery(`INSERT INTO mailing_subject_identities`).
		WithArgs(orgID, md5hex(subject), subject).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(subjectID))
	mock.ExpectExec(`UPDATE mailing_campaigns`).
		WithArgs(campaignID, offerID, "3qqg71", creativeID, subjectID, "html_inferred").
		WillReturnResult(sqlmock.NewResult(0, 1))

	stampCampaignAttribution(context.Background(), db, orgID, campaignID,
		engine.PMTACampaignInput{},
		"metal roofing one-off resend jul07", // no wave token — name gate refuses
		subject, html, "Discount Blog")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// TestStampCampaignAttribution_HTMLAmbiguousSkipsOffer: a multi-offer creative
// (2+ distinct money slugs) must NOT guess an offer — the dims still stamp,
// but offer_key/offer_id/attribution_source stay NULL so a later, better
// resolution (or the backfill's click phase) can still claim the campaign.
func TestStampCampaignAttribution_HTMLAmbiguousSkipsOffer(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	orgID := uuid.New().String()
	campaignID := uuid.New().String()
	creativeID := uuid.New().String()
	subjectID := uuid.New().String()
	html := `<a href="https://h.com/AAAA/BBBB/?source_id=email">1</a><a href="https://h.com/AAAA/CCCC/?source_id=email">2</a>`
	subject := "Weekly digest"

	mock.ExpectQuery(`SELECT attribution_source IS NOT NULL FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"stamped"}).AddRow(false))
	mock.ExpectQuery(`INSERT INTO mailing_creative_identities`).
		WithArgs(orgID, md5hex(html), nil, subject, "History Thinking", campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(creativeID))
	mock.ExpectQuery(`INSERT INTO mailing_subject_identities`).
		WithArgs(orgID, md5hex(subject), subject).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(subjectID))
	mock.ExpectExec(`UPDATE mailing_campaigns`).
		WithArgs(campaignID, nil, nil, creativeID, subjectID, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))

	stampCampaignAttribution(context.Background(), db, orgID, campaignID,
		engine.PMTACampaignInput{},
		"ht weekly newsletter jul07",
		subject, html, "History Thinking")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// TestStampCampaignAttribution_AlreadyStampedSkips: a campaign whose
// attribution_source is already non-NULL (finalizer retry, re-deploy, draft
// re-promotion) is left untouched — no dim UPSERTs (campaign_count stays
// honest), no UPDATE (no payload/name-inferred mixed stamps). The creative
// UPSERT expectation below must go UNMET.
func TestStampCampaignAttribution_AlreadyStampedSkips(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	campaignID := uuid.New().String()
	mock.ExpectQuery(`SELECT attribution_source IS NOT NULL FROM mailing_campaigns`).
		WithArgs(campaignID).
		WillReturnRows(sqlmock.NewRows([]string{"stamped"}).AddRow(true))
	mock.ExpectQuery(`INSERT INTO mailing_creative_identities`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(uuid.New().String()))

	stampCampaignAttribution(context.Background(), db, uuid.New().String(), campaignID,
		engine.PMTACampaignInput{},
		"jul07 - Warranty For You - W1-CLK1-MSFT - fidelity",
		"subject", "<html>x</html>", "Warranty For You")

	if err := mock.ExpectationsWereMet(); err == nil {
		t.Fatal("already-stamped campaign must skip the dim UPSERTs and UPDATE, but they ran")
	}
}
