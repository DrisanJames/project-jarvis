package api

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Offer-suppression visibility for the Campaign Manager.
//
// WHY THIS EXISTS (operator 2026-08-18): "when I get to choosing my audience, I
// don't see all of the suppressions that have been built in the past… I just
// wanna make sure that we are correctly populating the offer suppressions."
//
// The wizard's Suppression panel lists `mailing_suppression_lists` rows only.
// That is NOT where an offer's suppression actually lives. There are two
// mechanisms and the panel showed neither of them honestly:
//
//  1. OFFER-LEVEL (automatic). planPMTAAudience loads
//     `mailing_offer_suppressions WHERE offer_id = <the campaign's offer>`
//     (pmta_campaign_planner.go:668) — converted subscribers, advertiser
//     scrubs, Optizmo deltas. Nothing in the UI ever said how many rows that
//     is, so "0" and "949,785" looked identical to the operator.
//
//  2. ADVERTISER LISTS (manual). Some offers additionally have a curated
//     `mailing_suppression_lists` row (id shaped `offer-<slug>-<date>`). It only
//     applies if the operator ticks it — nothing links list → offer in the DB
//     (`suppression_refresh_sources` carries that FK but has 0 rows in prod).
//
// The DUPLICATE-OFFER TRAP is the reason this is a correctness surface and not
// a nicety. `mailing_offers` carries near-duplicate rows for the same
// advertiser, and the suppressions hang off ONE of them. Measured in prod
// 2026-08-18: "Sam's Club Membership" (active) has 0 offer suppressions while
// "Sam's Club Membership - Partner Drip (4989)" (sunset) has 949,785. Picking
// the active row mails a list that should have been suppressed. Same shape for
// Get Metal Roofing (0 vs 203,538), CarShield (0 vs 103,722), Fidelity Life
// (0 vs 354,171), 3 Day Blinds (0 vs 6,915), West Capital HELOC (0 vs 32).
//
// This endpoint is READ-ONLY and advisory. It reports what WILL fire, suggests
// the advertiser list that looks like it belongs to this offer, and names any
// sibling offer row that is holding the suppressions instead. It never mutates
// and never blocks a deploy — remediation is the operator's call.

// offerSuppressionReason is one reason bucket of the offer-level ledger.
type offerSuppressionReason struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// offerSuppressionListRef is a curated advertiser suppression list that looks
// like it belongs to this offer. SUGGESTION ONLY — matched on name, because no
// DB link exists.
type offerSuppressionListRef struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	EntryCount int    `json:"entry_count"`
}

// offerSuppressionSibling is another mailing_offers row for what looks like the
// same advertiser that IS carrying offer-level suppressions.
type offerSuppressionSibling struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Status           string `json:"status"`
	SuppressionCount int    `json:"suppression_count"`
}

type offerSuppressionSummary struct {
	OfferID   string `json:"offer_id"`
	OfferName string `json:"offer_name"`
	// Status of the mailing_offers row ('active','draft','sunset').
	OfferStatus string `json:"offer_status"`
	// Rows in mailing_offer_suppressions for this offer — what the planner
	// will actually subtract.
	SuppressionCount int                       `json:"suppression_count"`
	Reasons          []offerSuppressionReason  `json:"reasons"`
	SuggestedLists   []offerSuppressionListRef `json:"suggested_lists"`
	Siblings         []offerSuppressionSibling `json:"siblings"`
	// Non-empty when the operator should look before deploying.
	Warning string `json:"warning,omitempty"`
}

// offerNameNoise are tokens carried by suppression-LIST names that describe the
// list rather than the advertiser, so they must not defeat a prefix match
// ("SBLI Quick Quote Offer Suppression" -> "sbli quick quote").
var offerNameNoise = map[string]bool{
	"offer": true, "suppression": true, "suppressions": true, "email": true,
	"only": true, "ask": true, "for": true, "cap": true, "shared": true,
	"cpa": true, "cpl": true, "cpm": true, "req": true, "list": true,
	"sensitive": true, "io": true, "mon": true, "sat": true, "proof": true,
	"converters": true,
}

var offerNameNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// normalizeOfferWords lowercases, splits on any non-alphanumeric run and drops
// empties. "Sam's Club Membership - Partner Drip (4989)" ->
// [sams club membership partner drip 4989].
func normalizeOfferWords(s string) []string {
	lowered := strings.ToLower(strings.TrimSpace(s))
	parts := offerNameNonAlnum.Split(lowered, -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// normalizeSuppressionListWords is normalizeOfferWords plus: drop bare numeric
// tokens (dates like 2026-05-04 become 2026 05 04) and strip trailing
// list-descriptive noise, so a list name reduces to the advertiser it belongs
// to. "Jacuzzi Bath Remodel (Zip-Targeted CPL) — 2026-05-04" -> [jacuzzi bath
// remodel zip targeted].
func normalizeSuppressionListWords(s string) []string {
	words := normalizeOfferWords(s)
	kept := make([]string, 0, len(words))
	for _, w := range words {
		if isAllDigits(w) {
			continue
		}
		kept = append(kept, w)
	}
	for len(kept) > 0 && offerNameNoise[kept[len(kept)-1]] {
		kept = kept[:len(kept)-1]
	}
	return kept
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// sharesOfferPrefix reports whether two normalized name token slices agree on
// every token up to the length of the shorter one, with at least two tokens in
// common. Two tokens is the floor that keeps "West Capital HELOC" from matching
// "West Shore Home Bath Remodel" while still linking "Fidelity Life" to
// "Fidelity Life Insurance".
//
// Pure — unit-tested against the full prod offer/list catalog.
func sharesOfferPrefix(a, b []string) bool {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n < 2 {
		return false
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// HandleOfferSuppression GET /api/mailing/pmta-campaign/offer-suppression?offer_id=<uuid>
//
// Read-only. Org-scoped. Reports the offer-level suppression that WILL fire for
// this offer, plus advisory pointers at a curated advertiser list and at any
// sibling offer row holding the suppressions instead.
func (s *PMTACampaignService) HandleOfferSuppression(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "organization not resolved")
		return
	}
	offerID := strings.TrimSpace(r.URL.Query().Get("offer_id"))
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offer_id is required")
		return
	}

	// The largest offer ledger in prod is ~950k rows; a per-offer count rides
	// idx_offer_suppressions_offer_sub and measures ~1.6s. Budget generously
	// but never unbounded — this is an advisory panel, not the send path.
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	out := offerSuppressionSummary{
		OfferID:        offerID,
		Reasons:        []offerSuppressionReason{},
		SuggestedLists: []offerSuppressionListRef{},
		Siblings:       []offerSuppressionSibling{},
	}

	var status sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT name, COALESCE(status, 'draft') FROM mailing_offers WHERE id = $1 AND organization_id = $2`,
		offerID, orgID.String()).Scan(&out.OfferName, &status)
	if err == sql.ErrNoRows {
		respondError(w, http.StatusNotFound, "offer not found")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "offer lookup failed: "+err.Error())
		return
	}
	out.OfferStatus = status.String

	// 1. What the planner will actually subtract.
	reasonRows, err := s.db.QueryContext(ctx,
		`SELECT COALESCE(reason, 'unspecified') AS reason, COUNT(*) AS n
		   FROM mailing_offer_suppressions
		  WHERE offer_id = $1
		  GROUP BY 1 ORDER BY 2 DESC`, offerID)
	if err != nil {
		// A slow/failed count must not blank the whole panel — the list and
		// sibling advice below is still worth showing.
		log.Printf("[OfferSuppression] reason breakdown failed for offer %s: %v", offerID, err)
	} else {
		for reasonRows.Next() {
			var rr offerSuppressionReason
			if reasonRows.Scan(&rr.Reason, &rr.Count) == nil {
				out.Reasons = append(out.Reasons, rr)
				out.SuppressionCount += rr.Count
			}
		}
		reasonRows.Close()
	}

	offerWords := normalizeOfferWords(out.OfferName)

	// 2. Curated advertiser list that LOOKS like this offer's. Suggestion only
	//    — the operator still has to tick it; nothing here auto-attaches.
	listRows, err := s.db.QueryContext(ctx,
		`SELECT id, name, COALESCE(entry_count, 0)
		   FROM mailing_suppression_lists
		  WHERE id <> 'global-suppression-list'`)
	if err != nil {
		log.Printf("[OfferSuppression] suppression-list scan failed: %v", err)
	} else {
		for listRows.Next() {
			var ref offerSuppressionListRef
			if listRows.Scan(&ref.ID, &ref.Name, &ref.EntryCount) != nil {
				continue
			}
			if sharesOfferPrefix(offerWords, normalizeSuppressionListWords(ref.Name)) {
				out.SuggestedLists = append(out.SuggestedLists, ref)
			}
		}
		listRows.Close()
	}

	// 3. Sibling offer rows. Name-match FIRST (64 rows, free), then count only
	//    the handful of candidates — a GROUP BY over the whole 3M-row ledger on
	//    every panel open would be a self-inflicted IO event.
	siblingRows, err := s.db.QueryContext(ctx,
		`SELECT id::text, name, COALESCE(status, 'draft')
		   FROM mailing_offers
		  WHERE organization_id = $1 AND id <> $2`, orgID.String(), offerID)
	if err != nil {
		log.Printf("[OfferSuppression] sibling scan failed: %v", err)
	} else {
		type cand struct{ id, name, status string }
		candidates := []cand{}
		for siblingRows.Next() {
			var c cand
			if siblingRows.Scan(&c.id, &c.name, &c.status) != nil {
				continue
			}
			if sharesOfferPrefix(offerWords, normalizeOfferWords(c.name)) {
				candidates = append(candidates, c)
			}
		}
		siblingRows.Close()
		// Bound the fan-out: at most 5 counted candidates per request.
		if len(candidates) > 5 {
			candidates = candidates[:5]
		}
		for _, c := range candidates {
			var n int
			if s.db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM mailing_offer_suppressions WHERE offer_id = $1`, c.id).Scan(&n) != nil {
				continue
			}
			if n > 0 {
				out.Siblings = append(out.Siblings, offerSuppressionSibling{
					ID: c.id, Name: c.name, Status: c.status, SuppressionCount: n,
				})
			}
		}
		sort.Slice(out.Siblings, func(i, j int) bool {
			return out.Siblings[i].SuppressionCount > out.Siblings[j].SuppressionCount
		})
	}

	out.Warning = offerSuppressionWarning(out)
	respondJSON(w, http.StatusOK, out)
}

// offerSuppressionWarning renders the operator-facing verdict. Pure, so the
// wording is unit-tested rather than eyeballed.
func offerSuppressionWarning(sum offerSuppressionSummary) string {
	if sum.SuppressionCount == 0 {
		if len(sum.Siblings) > 0 {
			top := sum.Siblings[0]
			return "This offer has NO offer-level suppression. A sibling offer row (" +
				top.Name + ", " + top.Status + ") carries " + formatThousands(top.SuppressionCount) +
				" suppressed subscribers — confirm you picked the right offer row before deploying."
		}
		if len(sum.SuggestedLists) > 0 {
			return "This offer has NO offer-level suppression. A matching advertiser suppression list " +
				"exists but is not applied automatically — tick it in the audience step."
		}
		return "This offer has NO suppression of any kind on file — no offer-level ledger and no " +
			"advertiser list. Nothing will be subtracted for this advertiser."
	}
	if len(sum.SuggestedLists) > 0 {
		return "Offer-level suppression will fire. A matching advertiser suppression list also exists " +
			"and is NOT applied automatically — tick it in the audience step if the advertiser requires it."
	}
	return ""
}

// formatThousands renders an int with comma separators (no locale package for
// one call site).
func formatThousands(n int) string {
	s := ""
	if n < 0 {
		s = "-"
		n = -n
	}
	digits := ""
	for {
		part := n % 1000
		n /= 1000
		if n == 0 {
			digits = itoaPlain(part) + digits
			break
		}
		digits = "," + pad3(part) + digits
	}
	return s + digits
}

func itoaPlain(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [4]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func pad3(n int) string {
	s := itoaPlain(n)
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}
