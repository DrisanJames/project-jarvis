package api

// campaign_audience_links.go — audience unification Phase 1, W1.
//
// mailing_campaign_audiences is the app-owned companion table that makes a
// campaign's audience identity SURVIVE: which segments it includes/excludes,
// today recoverable only from the pmta_config->'campaign_input' JSON blob
// (mailing_campaigns.segment_id is never written by the PMTA path, and the
// apex_admin-owned mailing_campaigns table can never gain a column from the
// app user — companion table is the established pattern).
//
// Writes are NON-FATAL by contract (log + continue — a companion-table
// failure must never fail a stage or deploy) and idempotent
// (ON CONFLICT DO NOTHING on the (campaign_id, segment_id, role) PK).
// Kill switch: DISABLE_CAMPAIGN_AUDIENCE_LINKS=1 skips all writes.
//
// Call sites (all three campaign-writing paths):
//   - reserveCampaignForDeploy   (handlers_pmta_campaign.go, deploy reserve)
//   - stagePMTADraftCampaign     (pmta_campaign_persistence.go, Draft Board stage)
//   - createPMTAWaveCampaign     (pmta_campaign_persistence.go, finalize)
//
// Backfill for pre-existing campaigns: POST /api/admin/audience-links/backfill
// (X-Admin-Key, closed by default) parses pmta_config in 500-id batches.

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// audienceLinksKillSwitch disables all mailing_campaign_audiences writes when
// set to "1" — the one-move rollback for the new stage/deploy-path writer.
const audienceLinksKillSwitch = "DISABLE_CAMPAIGN_AUDIENCE_LINKS"

// campaignAudienceUUIDPattern is the case-insensitive UUID shape gate the SQL
// backfill applies to pmta_config segment ids before ::uuid casting — kept as
// a named constant so the Go writer (uuid.Parse) and the SQL backfill can
// never diverge on what counts as a valid segment id.
const campaignAudienceUUIDPattern = `^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`

// parseSegmentUUIDs returns the deduplicated valid-UUID subset of raw,
// normalized to canonical lowercase form, preserving first-seen order.
// Non-UUID entries (segment names, garbage) are silently dropped — the links
// table records identity, not intent.
func parseSegmentUUIDs(raw []string) []string {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		id, err := uuid.Parse(s)
		if err != nil {
			continue
		}
		canonical := id.String()
		if _, dup := seen[canonical]; dup {
			continue
		}
		seen[canonical] = struct{}{}
		out = append(out, canonical)
	}
	return out
}

// writeCampaignAudienceLinks records campaign→segment links for one campaign
// from a deploy/stage payload's segment lists. Returns the number of link rows
// written (0 when the kill switch is on, nothing parses as a UUID, or every
// row already existed). Never returns an error: failures are logged and
// swallowed so the caller's stage/deploy can never be affected.
type segmentLinkInput struct {
	role string
	ids  []string
}

func writeCampaignAudienceLinks(ctx context.Context, q dbQuerier, campaignID string, inclusionSegments, exclusionSegments []string) int {
	if os.Getenv(audienceLinksKillSwitch) == "1" {
		return 0
	}
	if q == nil || campaignID == "" {
		return 0
	}
	if _, err := uuid.Parse(campaignID); err != nil {
		return 0
	}
	written := 0
	for _, in := range []segmentLinkInput{
		{role: "include", ids: parseSegmentUUIDs(inclusionSegments)},
		{role: "exclude", ids: parseSegmentUUIDs(exclusionSegments)},
	} {
		if len(in.ids) == 0 {
			continue
		}
		res, err := q.ExecContext(ctx, `
			INSERT INTO mailing_campaign_audiences (campaign_id, segment_id, role)
			SELECT $1::uuid, s, $3
			FROM unnest($2::uuid[]) AS s
			ON CONFLICT DO NOTHING
		`, campaignID, pq.Array(in.ids), in.role)
		if err != nil {
			log.Printf("[AudienceLinks] campaign %s: %s link insert failed (continuing — deploy unaffected): %v", campaignID, in.role, err)
			continue
		}
		if n, err := res.RowsAffected(); err == nil {
			written += int(n)
		}
	}
	return written
}

// audienceLinksBackfillResult is the admin backfill response body.
type audienceLinksBackfillResult struct {
	WindowDays       int      `json:"window_days"`
	CampaignsScanned int64    `json:"campaigns_scanned"`
	IncludeLinks     int64    `json:"include_links"`
	ExcludeLinks     int64    `json:"exclude_links"`
	Batches          int      `json:"batches"`
	Errors           []string `json:"errors"`
	TookMs           int64    `json:"took_ms"`
}

// HandleAudienceLinksBackfill POST /api/admin/audience-links/backfill?days=N
// parses pmta_config->'campaign_input'->'inclusion_segments'/'exclusion_segments'
// for campaigns created in the window and inserts links, batched 500 ids per
// loop with an id cursor (never one statement over the whole 73k-campaign
// table — see AUDIENCE_UNIFICATION.md pre-mortem). Idempotent; safe to re-run.
func HandleAudienceLinksBackfill(db *sql.DB) http.HandlerFunc {
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

		res := audienceLinksBackfillResult{WindowDays: days, Errors: []string{}}
		cursor := "00000000-0000-0000-0000-000000000000"

	scan:
		for ctx.Err() == nil {
			rows, err := db.QueryContext(ctx, `
				SELECT id::text FROM mailing_campaigns
				WHERE organization_id = $1 AND created_at >= $2 AND id > $3::uuid
				  AND pmta_config IS NOT NULL
				ORDER BY id
				LIMIT $4`, orgID, windowStart, cursor, batchSize)
			if err != nil {
				res.Errors = append(res.Errors, "id scan: "+err.Error())
				break
			}
			ids := make([]string, 0, batchSize)
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
			if len(ids) == 0 {
				break
			}
			res.Batches++
			res.CampaignsScanned += int64(len(ids))
			cursor = ids[len(ids)-1]

			for _, in := range []struct {
				role string
				key  string
			}{
				{"include", "inclusion_segments"},
				{"exclude", "exclusion_segments"},
			} {
				out, err := db.ExecContext(ctx, `
					INSERT INTO mailing_campaign_audiences (campaign_id, segment_id, role)
					SELECT c.id, seg.val::uuid, $2
					FROM mailing_campaigns c
					CROSS JOIN LATERAL jsonb_array_elements_text(
						CASE WHEN jsonb_typeof(c.pmta_config->'campaign_input'->$3::text) = 'array'
						     THEN c.pmta_config->'campaign_input'->$3::text
						     ELSE '[]'::jsonb END
					) AS seg(val)
					WHERE c.id = ANY($1::uuid[])
					  AND seg.val ~ $4
					ON CONFLICT DO NOTHING
				`, pq.Array(ids), in.role, in.key, campaignAudienceUUIDPattern)
				if err != nil {
					res.Errors = append(res.Errors, in.role+" batch "+strconv.Itoa(res.Batches)+": "+err.Error())
					break scan
				}
				n, _ := out.RowsAffected()
				if in.role == "include" {
					res.IncludeLinks += n
				} else {
					res.ExcludeLinks += n
				}
			}
		}
		if ctx.Err() != nil {
			res.Errors = append(res.Errors, "window: "+ctx.Err().Error())
		}

		res.TookMs = time.Since(started).Milliseconds()
		respondJSON(w, http.StatusOK, res)
	}
}
