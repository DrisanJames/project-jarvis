package api

// campaign_attribution.go — deploy-time attribution stamping (Offer Alignment
// PART A). Stamps the freshly-deployed mailing_campaigns row with its offer
// identity (offer_id + offer_key), creative identity (md5-keyed dim row) and
// subject identity, plus attribution_source ('payload' | 'name_inferred').
// The wave dispatcher's enqueue INSERTs and the send worker's sent-event
// INSERT then inherit these columns onto mailing_campaign_queue and
// mailing_tracking_events, and the open/click handlers' existing
// `q.offer_id IS NOT NULL` copy guard starts firing for stamped campaigns —
// unstamped campaigns flow NULLs end-to-end, zero behavior change.
//
// Every step is log-and-continue: stamping must NEVER fail a deploy. Kill
// switch: DISABLE_ATTRIBUTION_STAMPING=1 skips everything.

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// attributionStampKillSwitch disables the deploy-time attribution stamp when
// set to "1". Queue/event inheritance naturally no-ops on the NULLs.
const attributionStampKillSwitch = "DISABLE_ATTRIBUTION_STAMPING"

// waveNameTokenRe is the board wave-lane convention gate (e.g. "W1-CLK1-MSFT"
// inside "jul07 - Warranty For You - W1-CLK1-MSFT - fidelity"). Offer-key
// inference from the campaign-name last token runs ONLY when the name carries
// a wave token — otherwise arbitrary last tokens on proof/test/drip campaign
// names would be stamped as offer keys.
var waveNameTokenRe = regexp.MustCompile(`\bW\d+-`)

// parseOfferTokenFromCampaignName extracts the offer slug from a
// board-convention campaign name: the last " - "-separated token, lowercased.
// Returns ok=false when the name does not match the wave convention, has no
// " - " separator, or the last token is itself the wave lane (a name that
// ends at the lane carries no offer token).
func parseOfferTokenFromCampaignName(name string) (string, bool) {
	if !waveNameTokenRe.MatchString(name) {
		return "", false
	}
	idx := strings.LastIndex(name, " - ")
	if idx < 0 {
		return "", false
	}
	token := strings.TrimSpace(name[idx+3:])
	if token == "" || waveNameTokenRe.MatchString(token) {
		return "", false
	}
	return strings.ToLower(token), true
}

// moneyLinkSlugPattern matches an Everflow money link and captures the offer
// slug — the 2nd path segment, the same rule moneySlugExpr
// (handlers_analytics_creatives.go) applies to clicked link_urls. The
// source_id=email marker restricts it to money links (unsubscribe/asset/bare
// scanner URLs never carry it), and it matches both raw click URLs and hrefs
// inside compiled HTML (the URL token ends at a quote/whitespace/angle
// bracket). POSIX-ERE/RE2 common subset ONLY: the backfill passes this exact
// string to Postgres regexp_matches()/substring() as a bind argument, so Go
// stamping and SQL backfill can never diverge.
const moneyLinkSlugPattern = `https?://[^/"'\s<>]+/[A-Za-z0-9]+/([A-Za-z0-9]+)/[^"'\s<>]*source_id=email`

var moneyLinkSlugRe = regexp.MustCompile(moneyLinkSlugPattern)

// extractMoneySlugsFromHTML returns the DISTINCT money-link offer slugs in a
// compiled creative, lowercased and sorted. One distinct slug = an unambiguous
// single-offer creative; multiple = a multi-offer newsletter (don't guess).
func extractMoneySlugsFromHTML(html string) []string {
	if !strings.Contains(html, "source_id=email") {
		return nil
	}
	seen := map[string]struct{}{}
	for _, m := range moneyLinkSlugRe.FindAllStringSubmatch(html, -1) {
		seen[strings.ToLower(m[1])] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// attributionQuerier is the narrow DB seam stampCampaignAttribution runs
// through (*sql.DB satisfies it; sqlmock in tests).
type attributionQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

// stampCampaignAttribution writes offer/creative/subject attribution onto a
// freshly-deployed campaign row. Called POST-COMMIT (never inside the deploy
// TX) so a stamping failure can never roll back or fail a deploy — mirrors
// stampPartnerAttributionOnCampaign's post-deploy patch pattern.
//
// Offer resolution order:
//  1. payload — input.OfferID parses as a UUID (drip/agent deploys) →
//     offer_id = that UUID, offer_key best-effort from
//     mailing_offers.landing_page_slug or the slug-map reverse lookup,
//     attribution_source='payload'.
//  2. name_inferred — the campaign name matches the board wave convention →
//     offer_key = lowercased last name token; the slug map validates it and,
//     when mapped, resolves offer_id via mailing_offers.everflow_offer_id
//     (offer_id may stay NULL — the token is still the operational key),
//     attribution_source='name_inferred'.
//  3. html_inferred — the compiled creative carries exactly ONE distinct
//     money-link slug (source_id=email marker): the money link is ground
//     truth of what the campaign mailed, independent of naming convention or
//     payload — it is what catches one-off broadcasts (operator 2026-07-07).
//     Multi-offer creatives (2+ distinct slugs) are logged and NOT guessed.
//  4. none of the above — offer columns stay NULL (historical semantics).
//
// Creative/subject identities are md5-keyed UPSERTs into the dim tables;
// re-deploys hit ON CONFLICT and are idempotent.
func stampCampaignAttribution(ctx context.Context, db attributionQuerier, orgID, campaignID string, input engine.PMTACampaignInput, name, subject, html, fromName string) {
	if os.Getenv(attributionStampKillSwitch) == "1" {
		return
	}
	if db == nil || campaignID == "" {
		return
	}
	// Outermost guard: a panic here must never escape into the finalizer
	// goroutine (whose own recover would mark an already-committed campaign
	// failed). Same posture as writeDeploymentAudit.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[attribution] recovered panic stamping campaign %s: %v", campaignID, r)
		}
	}()

	// Idempotency gate: attribution_source is written by the same final UPDATE
	// as every other stamp column, so a non-NULL source proves a coherent stamp
	// already landed — re-runs (finalizer retry, re-deploy, draft re-promotion)
	// are no-ops. This keeps dim campaign_count honest (one increment per
	// campaign) and prevents a later name-inferred run from partially
	// overwriting an earlier payload stamp. A partial run that timed out before
	// the final UPDATE left source NULL, so it retries fully and heals.
	var alreadyStamped bool
	if err := db.QueryRowContext(ctx, `
		SELECT attribution_source IS NOT NULL FROM mailing_campaigns WHERE id = $1
	`, campaignID).Scan(&alreadyStamped); err != nil {
		if err != sql.ErrNoRows {
			log.Printf("[attribution] campaign %s: stamped-check failed (continuing): %v", campaignID, err)
		}
	} else if alreadyStamped {
		// Auto-tagging (audience unification W4a) still applies when the stamp
		// is skipped as already-stamped — a stage-time stamp followed by the
		// deploy-time re-run must still land the tags. The stored offer_key is
		// re-read because this run never computed one. ON CONFLICT DO NOTHING
		// keeps the re-run idempotent; kill switch DISABLE_CAMPAIGN_AUTO_TAGS=1.
		if os.Getenv(autoTagKillSwitch) != "1" {
			var storedKey sql.NullString
			if err := db.QueryRowContext(ctx, `
				SELECT offer_key FROM mailing_campaigns WHERE id = $1
			`, campaignID).Scan(&storedKey); err != nil && err != sql.ErrNoRows {
				log.Printf("[AutoTag] campaign %s: offer_key read failed (continuing): %v", campaignID, err)
			}
			autoTagCampaign(ctx, db, campaignID,
				deriveCampaignTags(name, storedKey.String, input.SendingDomain, firstVariantFromEmail(input)))
		}
		return
	}

	var offerID, offerKey, attributionSource interface{}

	if parsed, err := uuid.Parse(strings.TrimSpace(input.OfferID)); err == nil {
		// (1) payload path.
		offerID = parsed.String()
		attributionSource = "payload"
		var slug, everflowID string
		err := db.QueryRowContext(ctx, `
			SELECT COALESCE(NULLIF(lower(landing_page_slug), ''), ''), COALESCE(everflow_offer_id, '')
			FROM mailing_offers
			WHERE id = $1 AND organization_id = $2
		`, parsed.String(), orgID).Scan(&slug, &everflowID)
		if err != nil && err != sql.ErrNoRows {
			log.Printf("[attribution] campaign %s: offer_key lookup for payload offer %s failed (continuing): %v", campaignID, parsed, err)
		}
		if slug == "" && everflowID != "" {
			// Best-effort reverse lookup through the slug map.
			if err := db.QueryRowContext(ctx, `
				SELECT lower(cratoolpro_slug) FROM mailing_offer_slug_map
				WHERE everflow_offer_id = $1
				ORDER BY cratoolpro_slug ASC LIMIT 1
			`, everflowID).Scan(&slug); err != nil && err != sql.ErrNoRows {
				log.Printf("[attribution] campaign %s: slug-map reverse lookup ef=%s failed (continuing): %v", campaignID, everflowID, err)
			}
		}
		if slug != "" {
			offerKey = slug
		}
	} else if token, ok := parseOfferTokenFromCampaignName(name); ok {
		// (2) name-inferred path. The token is the operational offer key even
		// when the slug map doesn't know it; offer_id fills in only when the
		// map + mailing_offers agree.
		offerKey = token
		attributionSource = "name_inferred"
		offerID = resolveOfferIDViaSlugMap(ctx, db, orgID, campaignID, token)
	} else if slugs := extractMoneySlugsFromHTML(html); len(slugs) == 1 {
		// (3) html-inferred path — the compiled creative's money link is what
		// the campaign actually mailed. Only unambiguous (exactly one distinct
		// slug); the slug is the operational key even when the map doesn't
		// know it, same semantics as the name path.
		offerKey = slugs[0]
		attributionSource = "html_inferred"
		offerID = resolveOfferIDViaSlugMap(ctx, db, orgID, campaignID, slugs[0])
	} else if len(slugs) > 1 {
		log.Printf("[attribution] campaign %s: %d distinct money slugs in creative (%s…) — multi-offer, not stamping an offer", campaignID, len(slugs), strings.Join(slugs[:2], ","))
	}

	var creativeID interface{}
	if strings.TrimSpace(html) != "" {
		sum := md5.Sum([]byte(html))
		var id string
		if err := db.QueryRowContext(ctx, `
			INSERT INTO mailing_creative_identities
				(organization_id, content_md5, offer_key, subject, from_name, sample_campaign_id)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (organization_id, content_md5) DO UPDATE
			SET last_used_at = NOW(),
			    campaign_count = mailing_creative_identities.campaign_count + 1
			RETURNING id::text
		`, orgID, hex.EncodeToString(sum[:]), offerKey, strings.TrimSpace(subject), fromName, campaignID).Scan(&id); err != nil {
			log.Printf("[attribution] campaign %s: creative identity upsert failed (continuing): %v", campaignID, err)
		} else {
			creativeID = id
		}
	}

	var subjectLineID interface{}
	if trimmed := strings.TrimSpace(subject); trimmed != "" {
		sum := md5.Sum([]byte(trimmed))
		var id string
		if err := db.QueryRowContext(ctx, `
			INSERT INTO mailing_subject_identities
				(organization_id, subject_md5, subject)
			VALUES ($1, $2, $3)
			ON CONFLICT (organization_id, subject_md5) DO UPDATE
			SET last_used_at = NOW(),
			    campaign_count = mailing_subject_identities.campaign_count + 1
			RETURNING id::text
		`, orgID, hex.EncodeToString(sum[:]), trimmed).Scan(&id); err != nil {
			log.Printf("[attribution] campaign %s: subject identity upsert failed (continuing): %v", campaignID, err)
		} else {
			subjectLineID = id
		}
	}

	if offerID == nil && offerKey == nil && creativeID == nil && subjectLineID == nil {
		return // nothing resolved — leave the row's NULL/historical semantics alone
	}

	stampAttributionUpdate(ctx, db, campaignID, offerID, offerKey, creativeID, subjectLineID, attributionSource)

	// Auto-tagger (audience unification W4a) — after a successful stamp, derive
	// the campaign's rollup tags from the same identity facts. Log-and-continue
	// inside; kill switch DISABLE_CAMPAIGN_AUTO_TAGS=1.
	keyForTags := ""
	if s, ok := offerKey.(string); ok {
		keyForTags = s
	}
	autoTagCampaign(ctx, db, campaignID,
		deriveCampaignTags(name, keyForTags, input.SendingDomain, firstVariantFromEmail(input)))
}

// resolveOfferIDViaSlugMap resolves an offer token/slug to a mailing_offers id
// through the slug map (token matches cratoolpro_slug or offer_name), returning
// nil when unmapped — shared by the name-inferred and html-inferred paths.
func resolveOfferIDViaSlugMap(ctx context.Context, db attributionQuerier, orgID, campaignID, token string) interface{} {
	var everflowID string
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(everflow_offer_id, '') FROM mailing_offer_slug_map
		WHERE upper(cratoolpro_slug) = upper($1) OR upper(offer_name) = upper($1)
		LIMIT 1
	`, token).Scan(&everflowID)
	if err != nil && err != sql.ErrNoRows {
		log.Printf("[attribution] campaign %s: slug-map lookup token=%q failed (continuing): %v", campaignID, token, err)
	}
	if err == nil && everflowID != "" {
		var resolved string
		err := db.QueryRowContext(ctx, `
			SELECT id::text FROM mailing_offers
			WHERE organization_id = $1 AND everflow_offer_id = $2
			LIMIT 1
		`, orgID, everflowID).Scan(&resolved)
		if err == nil {
			return resolved
		} else if err != sql.ErrNoRows {
			log.Printf("[attribution] campaign %s: mailing_offers lookup ef=%s failed (continuing): %v", campaignID, everflowID, err)
		}
	}
	return nil
}

// stampAttributionUpdate is the single COALESCE-guarded UPDATE that lands the
// stamp (split out of stampCampaignAttribution for readability only).
func stampAttributionUpdate(ctx context.Context, db attributionQuerier, campaignID string, offerID, offerKey, creativeID, subjectLineID, attributionSource interface{}) {
	// Single UPDATE, COALESCE-guarded so a re-run (re-deploy, draft re-promote)
	// never clobbers an earlier stamp with NULLs.
	if _, err := db.ExecContext(ctx, `
		UPDATE mailing_campaigns
		SET offer_id = COALESCE($2::uuid, offer_id),
		    offer_key = COALESCE($3, offer_key),
		    creative_id = COALESCE($4::uuid, creative_id),
		    subject_line_id = COALESCE($5::uuid, subject_line_id),
		    attribution_source = COALESCE($6, attribution_source),
		    updated_at = NOW()
		WHERE id = $1
	`, campaignID, offerID, offerKey, creativeID, subjectLineID, attributionSource); err != nil {
		log.Printf("[attribution] campaign %s: attribution UPDATE failed (swallowed — deploy unaffected): %v", campaignID, err)
	}
}
