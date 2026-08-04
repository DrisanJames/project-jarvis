package api

// campaign_tags.go — audience unification Phase 1, W4a.
//
// mailing_campaign_tags is the app-owned companion table that gives campaigns
// family rollups without name-string parsing at read time (there is no tags
// column on apex_admin-owned mailing_campaigns, and grouping today is name
// regexes in 6+ places). The auto-tagger derives tags from the same identity
// facts the attribution stamp already resolves:
//
//	offer:<offer_key>       when an offer key resolved (or is already stamped)
//	brand:<apex>            from the sending domain (or from_email host),
//	                        stripping the em./m. sending-subdomain prefix
//	slot:<token>            the board wave-lane / stream token in the name
//	                        (W1-CLK1-MSFT, OFR-…, NL-…, FRESH-BCAST-…, KUMO-WARM…)
//	stream:partner-drip     for '[partner-drip] …' campaign names
//
// Invoked from stampCampaignAttribution after a successful stamp AND on the
// skipped-as-already-stamped path (re-deploys/promotions of a stage-stamped
// draft still tag). All writes are ON CONFLICT DO NOTHING and log-and-continue.
// Kill switch: DISABLE_CAMPAIGN_AUTO_TAGS=1.
//
// Backfill for pre-existing campaigns: POST /api/admin/campaign-tags/backfill
// (X-Admin-Key, closed by default) runs the same derivation over the
// name/offer_key/from_email/pmta_config columns in 500-id batches.

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// autoTagKillSwitch disables all mailing_campaign_tags auto-writes when set
// to "1" — the one-move rollback for the new tag writer.
const autoTagKillSwitch = "DISABLE_CAMPAIGN_AUTO_TAGS"

// slotTokenRe extracts the board slot/lane token from a campaign name:
// the wave-lane convention (W1-CLK1-MSFT — waveNameTokenRe's sibling,
// campaign_attribution.go:41) plus the named stream conventions
// (OFR-…, NL-…, FRESH-BCAST-…, KUMO-WARM…). First match wins.
var slotTokenRe = regexp.MustCompile(`\b(W\d+-\S+|OFR-\S+|NL-\S+|FRESH-BCAST-\S+|KUMO-WARM\S*)`)

// partnerDripNameRe classifies the partner-drip stream by its standing name
// convention (same literal prefix the drip orchestrator mints).
var partnerDripNameRe = regexp.MustCompile(`^\[partner-drip\]`)

// brandApexFromDomains derives the brand apex for a brand:<apex> tag from the
// campaign's sending domain, falling back to the from_email host. The
// em./m. sending-subdomain prefixes are stripped (em.discountblog.com →
// discountblog.com), matching the SENDING_DOMAIN convention (CLAUDE.md §7).
func brandApexFromDomains(sendingDomain, fromEmail string) string {
	domain := strings.ToLower(strings.TrimSpace(sendingDomain))
	if domain == "" {
		if at := strings.LastIndex(fromEmail, "@"); at >= 0 {
			domain = strings.ToLower(strings.TrimSpace(fromEmail[at+1:]))
		}
	}
	domain = strings.TrimSuffix(domain, ".")
	if domain == "" || !strings.Contains(domain, ".") {
		return ""
	}
	for _, prefix := range []string{"em.", "m."} {
		if strings.HasPrefix(domain, prefix) && strings.Contains(domain[len(prefix):], ".") {
			domain = domain[len(prefix):]
			break
		}
	}
	return domain
}

// deriveCampaignTags is the pure tag derivation — everything the auto-tagger
// and the admin backfill write flows through here so the two can never
// diverge. All tags are lowercased so rollup grouping is case-stable.
func deriveCampaignTags(name, offerKey, sendingDomain, fromEmail string) []string {
	tags := make([]string, 0, 4)
	if key := strings.ToLower(strings.TrimSpace(offerKey)); key != "" {
		tags = append(tags, "offer:"+key)
	}
	if apex := brandApexFromDomains(sendingDomain, fromEmail); apex != "" {
		tags = append(tags, "brand:"+apex)
	}
	if m := slotTokenRe.FindString(name); m != "" {
		tags = append(tags, "slot:"+strings.ToLower(m))
	}
	if partnerDripNameRe.MatchString(strings.TrimSpace(name)) {
		tags = append(tags, "stream:partner-drip")
	}
	return tags
}

// firstVariantFromEmail returns the deploy payload's variant-A from_email for
// the brand-apex fallback (the stamp call sites don't carry the resolved
// profile from_email).
func firstVariantFromEmail(input engine.PMTACampaignInput) string {
	if len(input.Variants) > 0 {
		return input.Variants[0].FromEmail
	}
	return ""
}

// autoTagCampaign writes derived tags for one campaign. Returns rows written
// (0 when the kill switch is on or nothing derived). Never returns an error:
// log-and-continue, same posture as the attribution stamp it rides on.
func autoTagCampaign(ctx context.Context, db attributionQuerier, campaignID string, tags []string) int {
	if os.Getenv(autoTagKillSwitch) == "1" {
		return 0
	}
	if db == nil || campaignID == "" || len(tags) == 0 {
		return 0
	}
	res, err := db.ExecContext(ctx, `
		INSERT INTO mailing_campaign_tags (campaign_id, tag, source)
		SELECT $1::uuid, t, 'auto'
		FROM unnest($2::text[]) AS t
		ON CONFLICT DO NOTHING
	`, campaignID, pq.Array(tags))
	if err != nil {
		log.Printf("[AutoTag] campaign %s: tag insert failed (continuing — stamp/deploy unaffected): %v", campaignID, err)
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}

// campaignTagsBackfillResult is the admin backfill response body.
type campaignTagsBackfillResult struct {
	WindowDays       int      `json:"window_days"`
	CampaignsScanned int64    `json:"campaigns_scanned"`
	CampaignsTagged  int64    `json:"campaigns_tagged"`
	TagsWritten      int64    `json:"tags_written"`
	Batches          int      `json:"batches"`
	Errors           []string `json:"errors"`
	TookMs           int64    `json:"took_ms"`
}

// HandleCampaignTagsBackfill POST /api/admin/campaign-tags/backfill?days=N
// runs the auto-tagger derivation over campaigns created in the window from
// the name/offer_key/from_email/pmta_config columns, batched 500 ids per loop
// with an id cursor. Idempotent; safe to re-run.
func HandleCampaignTagsBackfill(db *sql.DB) http.HandlerFunc {
	const batchSize = 500
	return func(w http.ResponseWriter, r *http.Request) {
		orgID := getOrgID(r)
		started := time.Now()

		days := 30
		if n, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && n >= 1 && n <= 365 {
			days = n
		}
		windowStart := time.Now().AddDate(0, 0, -days)

		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
		defer cancel()

		res := campaignTagsBackfillResult{WindowDays: days, Errors: []string{}}
		cursor := "00000000-0000-0000-0000-000000000000"

		for ctx.Err() == nil {
			rows, err := db.QueryContext(ctx, `
				SELECT id::text, COALESCE(name, ''), COALESCE(offer_key, ''),
				       COALESCE(from_email, ''),
				       COALESCE(pmta_config->'campaign_input'->>'sending_domain', '')
				FROM mailing_campaigns
				WHERE organization_id = $1 AND created_at >= $2 AND id > $3::uuid
				ORDER BY id
				LIMIT $4`, orgID, windowStart, cursor, batchSize)
			if err != nil {
				res.Errors = append(res.Errors, "scan: "+err.Error())
				break
			}
			type cand struct {
				id, name, offerKey, fromEmail, sendingDomain string
			}
			batch := make([]cand, 0, batchSize)
			for rows.Next() {
				var c cand
				if err := rows.Scan(&c.id, &c.name, &c.offerKey, &c.fromEmail, &c.sendingDomain); err == nil {
					batch = append(batch, c)
				}
			}
			rows.Close()
			if len(batch) == 0 {
				break
			}
			res.Batches++
			res.CampaignsScanned += int64(len(batch))
			cursor = batch[len(batch)-1].id

			// Aligned pair arrays → one INSERT per batch.
			ids := make([]string, 0, len(batch)*2)
			tags := make([]string, 0, len(batch)*2)
			for _, c := range batch {
				derived := deriveCampaignTags(c.name, c.offerKey, c.sendingDomain, c.fromEmail)
				if len(derived) == 0 {
					continue
				}
				res.CampaignsTagged++
				for _, t := range derived {
					ids = append(ids, c.id)
					tags = append(tags, t)
				}
			}
			if len(ids) == 0 {
				continue
			}
			out, err := db.ExecContext(ctx, `
				INSERT INTO mailing_campaign_tags (campaign_id, tag, source)
				SELECT x.cid, x.tag, 'auto'
				FROM unnest($1::uuid[], $2::text[]) AS x(cid, tag)
				ON CONFLICT DO NOTHING
			`, pq.Array(ids), pq.Array(tags))
			if err != nil {
				res.Errors = append(res.Errors, "insert batch "+strconv.Itoa(res.Batches)+": "+err.Error())
				break
			}
			n, _ := out.RowsAffected()
			res.TagsWritten += n
		}
		if ctx.Err() != nil {
			res.Errors = append(res.Errors, "window: "+ctx.Err().Error())
		}

		res.TookMs = time.Since(started).Milliseconds()
		respondJSON(w, http.StatusOK, res)
	}
}
