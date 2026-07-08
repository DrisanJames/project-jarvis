package api

// TEMPORARY local-PG integration check for the Offer Alignment SQL — driven
// only when OFFER_ALIGNMENT_LOCALPG_DSN is set (a scratch stub-schema DB).
// Exercises every composed query end-to-end so Postgres itself validates
// syntax, aliases, GROUP BY shapes, and parameter binding. Skipped everywhere
// else (CI, normal test runs).

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"
)

func TestOfferAlignmentSQLAgainstLocalPG(t *testing.T) {
	dsn := os.Getenv("OFFER_ALIGNMENT_LOCALPG_DSN")
	if dsn == "" {
		t.Skip("OFFER_ALIGNMENT_LOCALPG_DSN not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	org := "00000000-0000-0000-0000-000000000001"
	from := time.Now().Add(-30 * 24 * time.Hour)
	to := time.Now().Add(time.Hour)

	slugs, efids, offerName, err := resolveOfferSlugsForKey(ctx, db, "fidelity")
	if err != nil {
		t.Fatalf("resolveOfferSlugsForKey: %v", err)
	}
	if len(slugs) != 1 || slugs[0] != "FIDELITY" || len(efids) != 1 || offerName != "Fidelity Term Life" {
		t.Fatalf("slug resolution: slugs=%v efids=%v name=%q", slugs, efids, offerName)
	}
	patterns := []string{"%/" + slugs[0] + "/%"}

	set, err := resolveOfferCampaignSet(ctx, db, org, "fidelity", offerName, patterns, from, to, "")
	if err != nil {
		t.Fatalf("resolveOfferCampaignSet: %v", err)
	}
	if set.Stamped != 1 || set.Inferred != 1 {
		t.Fatalf("set stamped=%d inferred=%d ids=%v, want 1/1", set.Stamped, set.Inferred, set.IDs)
	}
	// sending_domain diagnostic filter (matches campaign from_email host).
	setDom, err := resolveOfferCampaignSet(ctx, db, org, "fidelity", offerName, patterns, from, to, "em.discountblog.com")
	if err != nil {
		t.Fatalf("resolveOfferCampaignSet(domain): %v", err)
	}
	if len(setDom.IDs) != 2 {
		t.Fatalf("domain-filtered set = %v, want both campaigns", setDom.IDs)
	}
	setMiss, err := resolveOfferCampaignSet(ctx, db, org, "fidelity", offerName, patterns, from, to, "em.quizfiesta.com")
	if err != nil {
		t.Fatalf("resolveOfferCampaignSet(domain-miss): %v", err)
	}
	if len(setMiss.IDs) != 0 {
		t.Fatalf("domain-miss set = %v, want empty", setMiss.IDs)
	}

	eng, err := fetchAlignmentEngagement(ctx, db, org, set.IDs, from, to)
	if err != nil {
		t.Fatalf("fetchAlignmentEngagement: %v", err)
	}
	if e := eng["apple"]; e == nil || e.PGSent != 1 || e.HumanClicks != 2 || e.Clickers != 1 {
		t.Fatalf("engagement apple = %+v", eng["apple"])
	}

	conv, err := fetchAlignmentConversionsByISP(ctx, db, org, efids, from, to)
	if err != nil {
		t.Fatalf("fetchAlignmentConversionsByISP: %v", err)
	}
	if c := conv["apple"]; c == nil || c.Conversions != 1 {
		t.Fatalf("conversions apple = %+v", conv["apple"])
	}

	s := &Server{mailingDB: db}
	// Scoped variant (conv CTE + LATERAL) and unscoped variant both compile+run.
	creatives, err := s.fetchAlignmentCreatives(ctx, db, org, set.IDs, patterns, efids, from, to)
	if err != nil {
		t.Fatalf("fetchAlignmentCreatives(scoped): %v", err)
	}
	if len(creatives) == 0 {
		t.Fatalf("expected creative rows, got none")
	}
	foundStamped := false
	for _, c := range creatives {
		if c.ISP != "apple" {
			t.Fatalf("creative isp = %q, want apple", c.ISP)
		}
		if !c.Inferred {
			foundStamped = true
		}
	}
	if !foundStamped {
		t.Fatalf("stamped campaign's creative should not be flagged inferred: %+v", creatives)
	}
	if _, err := s.fetchAlignmentCreatives(ctx, db, org, set.IDs, nil, nil, from, to); err != nil {
		t.Fatalf("fetchAlignmentCreatives(unscoped): %v", err)
	}

	dataSources, err := s.fetchAlignmentDataSources(ctx, db, org, set.IDs, efids, from, to)
	if err != nil {
		t.Fatalf("fetchAlignmentDataSources: %v", err)
	}
	if len(dataSources) != 1 || dataSources[0].DataSource == nil || *dataSources[0].DataSource != "partnerX" ||
		dataSources[0].Delivered != 1 || dataSources[0].Clicks != 2 || dataSources[0].Conversions != 1 {
		t.Fatalf("data sources = %+v", dataSources)
	}

	// Snapshot table round trip: worker INSERT literal + matrix read literal.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO mailing_offer_alignment_snapshot
		(organization_id, window_days, offer_key, offer_name, isp,
		 delivered, hard, soft, reputation_block, deferred,
		 human_clicks, clickers, pg_sent, conversions, revenue,
		 stamped_campaigns, inferred_campaigns,
		 badge, badge_reason, action, sample_ok, refreshed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,NOW())
	`, org, 7, "fidelity", "Fidelity Term Life", "apple",
		1000, 10, 5, 50, 20, 2, 1, 1, 1, 12.5, 1, 1,
		"HEALTHY", "ok", "", true); err != nil {
		t.Fatalf("snapshot insert: %v", err)
	}
	rows, refreshedAt, built, err := s.readOfferAlignmentSnapshot(ctx, org, 7)
	if err != nil {
		t.Fatalf("readOfferAlignmentSnapshot: %v", err)
	}
	if !built {
		t.Fatalf("built=false with a data row present")
	}
	if len(rows) != 1 || rows[0].OfferKey != "fidelity" || rows[0].Delivered != 1000 {
		t.Fatalf("snapshot rows = %+v", rows)
	}
	if rows[0].BlockRate == 0 || rows[0].RPM == 0 || rows[0].AttributionCoverage != 0.5 {
		t.Fatalf("derived rates wrong: %+v", rows[0])
	}
	if refreshedAt.IsZero() {
		t.Fatalf("refreshed_at not read")
	}

	// Worker enumeration queries.
	orgs, err := s.offerAlignmentOrgs(ctx, from)
	if err != nil || len(orgs) != 1 {
		t.Fatalf("offerAlignmentOrgs: %v %v", orgs, err)
	}
	offers, err := s.offerAlignmentOffers(ctx, db, org, from, to)
	if err != nil {
		t.Fatalf("offerAlignmentOffers: %v", err)
	}
	if len(offers) != 1 || offers[0].key != "fidelity" {
		t.Fatalf("offers = %+v", offers)
	}
}
