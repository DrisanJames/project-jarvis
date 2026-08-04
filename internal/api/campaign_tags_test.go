package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

// TestDeriveCampaignTags pins the auto-tagger derivation — the pure function
// both the deploy-time tagger and the admin backfill flow through.
func TestDeriveCampaignTags(t *testing.T) {
	cases := []struct {
		name          string
		campaignName  string
		offerKey      string
		sendingDomain string
		fromEmail     string
		want          []string
	}{
		{
			name:          "board wave-convention name",
			campaignName:  "jul07 - Warranty For You - W1-CLK1-MSFT - fidelity",
			offerKey:      "fidelity",
			sendingDomain: "em.warrantyforyou.com",
			want:          []string{"offer:fidelity", "brand:warrantyforyou.com", "slot:w1-clk1-msft"},
		},
		{
			name:         "partner drip stream, brand from from_email host",
			campaignName: "[partner-drip] term_life mh 2026-07-07 08:00",
			offerKey:     "term-life",
			fromEmail:    "diane@em.myownhealth.net",
			want:         []string{"offer:term-life", "brand:myownhealth.net", "stream:partner-drip"},
		},
		{
			name:          "kumo warm slot with m. prefix domain",
			campaignName:  "KUMO-WARM aadwd aug04",
			sendingDomain: "m.aadwd.com",
			want:          []string{"brand:aadwd.com", "slot:kumo-warm"},
		},
		{
			name:         "fresh broadcast slot token",
			campaignName: "aug03 - FRESH-BCAST-CONSUMER-GMAIL - db",
			want:         []string{"slot:fresh-bcast-consumer-gmail"},
		},
		{
			name:          "newsletter slot, apex domain kept as-is",
			campaignName:  "NL-DB aug02 morning",
			sendingDomain: "discountblog.com",
			want:          []string{"brand:discountblog.com", "slot:nl-db"},
		},
		{
			name:         "nothing derivable",
			campaignName: "PROOF - operator eyeball",
			want:         []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveCampaignTags(tc.campaignName, tc.offerKey, tc.sendingDomain, tc.fromEmail)
			if len(got) != len(tc.want) {
				t.Fatalf("deriveCampaignTags() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("deriveCampaignTags() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestAutoTagCampaign_KillSwitchWritesNothing: DISABLE_CAMPAIGN_AUTO_TAGS=1
// must skip the insert entirely — armed-expectation negative control.
func TestAutoTagCampaign_KillSwitchWritesNothing(t *testing.T) {
	t.Setenv(autoTagKillSwitch, "1")

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO mailing_campaign_tags`).
		WillReturnResult(sqlmock.NewResult(0, 2))

	written := autoTagCampaign(context.Background(), db, uuid.New().String(),
		[]string{"offer:fidelity", "brand:discountblog.com"})

	if written != 0 {
		t.Fatalf("kill switch on but %d tags were written", written)
	}
	if err := mock.ExpectationsWereMet(); err == nil {
		t.Fatal("kill switch must skip ALL DB access, but the tag insert ran")
	}
}

// TestHandleCampaignTagsReport_PerTagAggregates: tag param empty → one
// aggregate row per tag, summed from the denormalized campaign counters,
// org-scoped, with the to date inclusive (+1d exclusive upper bound).
func TestHandleCampaignTagsReport_PerTagAggregates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	orgID := uuid.New().String()
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	toExclusive := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`FROM mailing_campaign_tags`).
		WithArgs(orgID, from, toExclusive).
		WillReturnRows(sqlmock.NewRows([]string{
			"tag", "campaigns", "total_recipients", "sent_count", "delivered_count",
			"unique_open_count", "unique_click_count", "hard_bounce_count", "soft_bounce_count",
		}).
			AddRow("brand:discountblog.com", 5, 50000, 48000, 47000, 1500, 200, 80, 90).
			AddRow("offer:fidelity", 3, 30000, 29000, 28500, 900, 120, 40, 60))

	req := httptest.NewRequest("GET", "/api/mailing/campaign-tags/report?from=2026-07-01&to=2026-07-31", nil)
	req.Header.Set("X-Organization-ID", orgID)
	rec := httptest.NewRecorder()

	HandleCampaignTagsReport(db)(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Rows []campaignTagAggregate `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(body.Rows))
	}
	first := body.Rows[0]
	if first.Tag != "brand:discountblog.com" || first.Campaigns != 5 ||
		first.SentCount != 48000 || first.DeliveredCount != 47000 ||
		first.UniqueOpenCount != 1500 || first.UniqueClickCnt != 200 ||
		first.HardBounceCount != 80 || first.SoftBounceCount != 90 {
		t.Fatalf("unexpected first row: %+v", first)
	}
	if body.Rows[1].Tag != "offer:fidelity" || body.Rows[1].TotalRecipients != 30000 {
		t.Fatalf("unexpected second row: %+v", body.Rows[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// TestHandleCampaignTagsReport_PerDenverDayForTag: tag given → the same
// aggregate keyed per Denver day for that tag.
func TestHandleCampaignTagsReport_PerDenverDayForTag(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	orgID := uuid.New().String()
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	toExclusive := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`America/Denver`).
		WithArgs(orgID, from, toExclusive, "offer:fidelity").
		WillReturnRows(sqlmock.NewRows([]string{
			"day", "campaigns", "total_recipients", "sent_count", "delivered_count",
			"unique_open_count", "unique_click_count", "hard_bounce_count", "soft_bounce_count",
		}).
			AddRow("2026-07-01", 2, 20000, 19000, 18800, 600, 80, 20, 30).
			AddRow("2026-07-02", 1, 10000, 9800, 9700, 300, 40, 10, 15))

	req := httptest.NewRequest("GET", "/api/mailing/campaign-tags/report?tag=offer:fidelity&from=2026-07-01&to=2026-07-02", nil)
	req.Header.Set("X-Organization-ID", orgID)
	rec := httptest.NewRecorder()

	HandleCampaignTagsReport(db)(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Rows []campaignTagAggregate `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(body.Rows))
	}
	if body.Rows[0].Day != "2026-07-01" || body.Rows[0].Campaigns != 2 || body.Rows[0].UniqueClickCnt != 80 {
		t.Fatalf("unexpected first row: %+v", body.Rows[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}
