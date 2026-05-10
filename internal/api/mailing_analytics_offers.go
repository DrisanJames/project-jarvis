package api

// Phase E — Interim slug-based offer attribution.
//
// Until INTENT Phase 3 ships (the Go fix that propagates offer_id
// through queue → send-worker → tracking event), the Analytics Center
// derives offers by scanning mailing_tracking_events.link_url for the
// canonical cratoolpro slug pattern `/{SLUG}/`. Catalog lives in
// internal/pkg/offers/catalog.go.
//
// Three handlers:
//
//   * HandleOfferPerformance — per-offer click counts, unique clickers,
//     campaign reach.
//   * HandleOfferOverlap     — pairwise (subscriber clicked offer A AND
//     offer B) for the audience overlap matrix.
//   * HandleSlugCoverage     — scans recent campaigns' creative_html for
//     a known offer slug + the cratoolpro tracking suffix; flags broken
//     creatives.
//
// All three are read-only and lean on shared helpers from metrics.go
// (HardBounceSQL, isppkg.GroupFromDomain).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/offers"
)

const (
	VersionOfferPerformance = "1.0"
	VersionOfferOverlap     = "1.0"
	VersionSlugCoverage     = "1.0"
)

// ─── 1. HandleOfferPerformance ──────────────────────────────────────────────
//
// For each known slug in offers.SlugCatalog, runs a single COUNT query
// over mailing_tracking_events.link_url ILIKE '%/{SLUG}/%' clipped to
// the requested date range. Returns clicks, unique_clickers (distinct
// subscriber_id), and campaigns_with_slug (distinct campaign_id) per
// offer.
//
// One query per slug keeps the per-offer index hits small. The catalog
// is currently 7 slugs so the cost is bounded; if it grows past ~30,
// switch to a single query with a CASE-by-pattern aggregator.

func (s *AdvancedMailingService) HandleOfferPerformance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start, end := parseAnalyticsRange(r)

	type row struct {
		Slug              string `json:"slug"`
		OfferName         string `json:"offer_name"`
		Vertical          string `json:"vertical"`
		Clicks            int    `json:"clicks"`
		UniqueClickers    int    `json:"unique_clickers"`
		CampaignsWithSlug int    `json:"campaigns_with_slug"`
	}
	rows := []row{}

	for _, slug := range offers.AllSlugs() {
		offer, _ := offers.Lookup(slug)
		var clicks, unique, campaigns int
		err := s.db.QueryRowContext(ctx, `
			SELECT
				COUNT(*) FILTER (WHERE t.event_type = 'clicked')                     AS clicks,
				COUNT(DISTINCT t.subscriber_id) FILTER (WHERE t.event_type = 'clicked') AS unique_clickers,
				COUNT(DISTINCT t.campaign_id)                                       AS campaigns_with_slug
			FROM mailing_tracking_events t
			WHERE t.event_at >= $1 AND t.event_at < $2
			  AND t.link_url ILIKE $3
		`, start, end, offers.LinkURLPattern(slug)).Scan(&clicks, &unique, &campaigns)
		if err != nil {
			// Don't abort the whole panel — record zero and continue.
			rows = append(rows, row{
				Slug: slug, OfferName: offer.Name, Vertical: offer.Vertical,
			})
			continue
		}
		rows = append(rows, row{
			Slug:              slug,
			OfferName:         offer.Name,
			Vertical:          offer.Vertical,
			Clicks:            clicks,
			UniqueClickers:    unique,
			CampaignsWithSlug: campaigns,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"api_version": VersionOfferPerformance,
		"range":       map[string]string{"start": start.UTC().Format(time.RFC3339), "end": end.UTC().Format(time.RFC3339)},
		"rows":        rows,
	})
}

// ─── 2. HandleOfferOverlap ──────────────────────────────────────────────────
//
// Pairwise overlap of unique clickers across the offer catalog. For each
// (offerA, offerB) pair, returns the count of distinct subscribers who
// clicked at least one link matching BOTH slugs in the window.
//
// Implementation: build a CTE per slug with the distinct clickers, then
// INTERSECT pairs. This is O(N^2) in the catalog size — fine for the
// ~7 slugs we have today; revisit if the catalog grows past ~20.

func (s *AdvancedMailingService) HandleOfferOverlap(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start, end := parseAnalyticsRange(r)

	// Pull distinct (slug, subscriber_id) pairs once, then pivot in Go.
	// One query — Postgres can use the link_url + subscriber_id indexes.
	patterns := offers.AllSlugs()
	if len(patterns) == 0 {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"api_version": VersionOfferOverlap,
			"pairs":       []interface{}{},
		})
		return
	}

	// Build a UNION ALL of per-slug projections.
	parts := make([]string, 0, len(patterns))
	args := []interface{}{start, end}
	for i, slug := range patterns {
		parts = append(parts, fmt.Sprintf(
			"SELECT $%d::text AS slug, t.subscriber_id FROM mailing_tracking_events t WHERE t.event_at >= $1 AND t.event_at < $2 AND t.event_type = 'clicked' AND t.link_url ILIKE $%d",
			len(args)+1, len(args)+2))
		args = append(args, slug, offers.LinkURLPattern(slug))
		_ = i
	}
	q := "WITH per_slug AS (" + strings.Join(parts, " UNION ALL ") + ") " +
		"SELECT slug, subscriber_id FROM per_slug WHERE subscriber_id IS NOT NULL"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		http.Error(w, fmt.Sprintf("offer-overlap query: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	bySlug := map[string]map[string]struct{}{}
	for rows.Next() {
		var slug, sub string
		if err := rows.Scan(&slug, &sub); err != nil {
			continue
		}
		if _, ok := bySlug[slug]; !ok {
			bySlug[slug] = map[string]struct{}{}
		}
		bySlug[slug][sub] = struct{}{}
	}

	type pair struct {
		A    string `json:"a"`
		B    string `json:"b"`
		Both int    `json:"both"`
	}
	var pairs []pair
	slugs := make([]string, 0, len(bySlug))
	for slug := range bySlug {
		slugs = append(slugs, slug)
	}
	for i := 0; i < len(slugs); i++ {
		for j := i + 1; j < len(slugs); j++ {
			a, b := slugs[i], slugs[j]
			both := 0
			setA, setB := bySlug[a], bySlug[b]
			// Iterate the smaller set
			if len(setB) < len(setA) {
				setA, setB = setB, setA
			}
			for sub := range setA {
				if _, ok := setB[sub]; ok {
					both++
				}
			}
			if both > 0 {
				offA, _ := offers.Lookup(a)
				offB, _ := offers.Lookup(b)
				labelA := offA.Name
				labelB := offB.Name
				if labelA == "" {
					labelA = a
				}
				if labelB == "" {
					labelB = b
				}
				pairs = append(pairs, pair{A: labelA, B: labelB, Both: both})
			}
		}
	}

	if pairs == nil {
		pairs = []pair{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"api_version": VersionOfferOverlap,
		"range":       map[string]string{"start": start.UTC().Format(time.RFC3339), "end": end.UTC().Format(time.RFC3339)},
		"pairs":       pairs,
	})
}

// ─── 3. HandleSlugCoverage ──────────────────────────────────────────────────
//
// Scans every campaign that started in the window and asserts that the
// creative HTML contains AT LEAST ONE catalog slug AND the canonical
// cratoolpro tracking suffix substrings (source_id=email + sub1= + sub2=).
//
// Surfaces:
//   * detected_slugs:    slugs found in the creative
//   * has_tracking_suffix: true iff ALL three suffix substrings are present
//
// Use case: catch the regression where a deploy script forgets the
// tracking-link suffix and the campaign sends but Everflow can't
// attribute conversions.

func (s *AdvancedMailingService) HandleSlugCoverage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start, end := parseAnalyticsRange(r)

	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, name, COALESCE(creative_html, '') AS html
		FROM mailing_campaigns
		WHERE COALESCE(started_at, scheduled_at, created_at) >= $1
		  AND COALESCE(started_at, scheduled_at, created_at) < $2
		ORDER BY COALESCE(started_at, scheduled_at, created_at) DESC
		LIMIT 200
	`, start, end)
	if err != nil {
		http.Error(w, fmt.Sprintf("slug-coverage query: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type campaignRow struct {
		CampaignID         string   `json:"campaign_id"`
		Name               string   `json:"name"`
		DetectedSlugs      []string `json:"detected_slugs"`
		ExpectedSlug       *string  `json:"expected_slug"`
		HasTrackingSuffix  bool     `json:"has_tracking_suffix"`
	}
	out := []campaignRow{}
	suffixes := offers.TrackingSuffixSubstrings()

	for rows.Next() {
		var id, name, html string
		if err := rows.Scan(&id, &name, &html); err != nil {
			continue
		}
		htmlUpper := strings.ToUpper(html)
		var detected []string
		for _, slug := range offers.AllSlugs() {
			needle := "/" + strings.ToUpper(slug) + "/"
			if strings.Contains(htmlUpper, needle) {
				detected = append(detected, slug)
			}
		}
		hasAllSuffix := true
		for _, suf := range suffixes {
			if !strings.Contains(html, suf) {
				hasAllSuffix = false
				break
			}
		}
		out = append(out, campaignRow{
			CampaignID:        id,
			Name:              name,
			DetectedSlugs:     detected,
			ExpectedSlug:      nil,
			HasTrackingSuffix: hasAllSuffix,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"api_version": VersionSlugCoverage,
		"range":       map[string]string{"start": start.UTC().Format(time.RFC3339), "end": end.UTC().Format(time.RFC3339)},
		"campaigns":   out,
	})
}
