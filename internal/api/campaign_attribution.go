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
//  3. neither — offer columns stay NULL (historical semantics).
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
				offerID = resolved
			} else if err != sql.ErrNoRows {
				log.Printf("[attribution] campaign %s: mailing_offers lookup ef=%s failed (continuing): %v", campaignID, everflowID, err)
			}
		}
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
