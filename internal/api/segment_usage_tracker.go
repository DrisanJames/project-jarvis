package api

import (
	"context"
	"database/sql"
	"log"
	"strings"

	"github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// bumpSegmentUsage records that a campaign just used a set of segments. It
// updates `mailing_segments.last_used_at`, increments `usage_count`, bumps
// `updated_at` (so the segment cycles to the back of the materialize/refresh
// worker queues — both of which select by oldest `updated_at`), and clears
// any pending cleanup-warning flags so an actively-used segment doesn't get
// archived by SegmentCleanupWorker.
//
// This function is the canonical replacement for the dead
// `update_segment_usage()` trigger from migration 006, which was wired to
// `mailing_campaigns.segment_ids` — a column that does NOT exist on PMTA
// campaigns. PMTA campaigns store segment references inside `pmta_config`
// JSONB, so the trigger never fires. Calling this helper from
// `finalizeAudience` after a successful Phase 1 plan repairs that gap.
//
// Best-effort semantics: failures are logged but never propagated. The
// caller MUST NOT block the campaign-finalize critical path on segment
// metadata bookkeeping.
//
// Deduplicates input IDs and silently skips empty / malformed UUIDs.
func bumpSegmentUsage(ctx context.Context, db *sql.DB, segmentIDs []string, campaignID string) {
	uniq := dedupSegmentIDs(segmentIDs)
	if len(uniq) == 0 {
		return
	}

	res, err := db.ExecContext(ctx, `
		UPDATE mailing_segments SET
			last_used_at         = NOW(),
			usage_count          = COALESCE(usage_count, 0) + 1,
			updated_at           = NOW(),
			cleanup_warning_sent = FALSE,
			cleanup_warned_at    = NULL
		WHERE id = ANY($1::uuid[])
	`, pq.Array(uniq))

	if err != nil {
		log.Printf("[bumpSegmentUsage] campaign=%s segments=%d failed: %v",
			campaignID, len(uniq), err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[bumpSegmentUsage] campaign=%s bumped %d/%d segment(s)",
			campaignID, n, len(uniq))
	}
}

// collectSegmentIDsForUsage extracts every segment ID a campaign references
// for usage-tracking purposes: inclusion segments, exclusion segments, and
// any send-priority items whose type is "segment". Lists (type="list") are
// intentionally excluded — list usage tracking would belong on
// `mailing_lists`, not `mailing_segments`, and is out of scope for this
// helper.
func collectSegmentIDsForUsage(input engine.PMTACampaignInput) []string {
	out := make([]string, 0, len(input.InclusionSegments)+len(input.ExclusionSegments)+len(input.SendPriority))
	out = append(out, input.InclusionSegments...)
	out = append(out, input.ExclusionSegments...)
	for _, item := range input.SendPriority {
		if strings.EqualFold(item.Type, "segment") && item.ID != "" {
			out = append(out, item.ID)
		}
	}
	return out
}

// dedupSegmentIDs returns the unique, syntactically-valid UUID subset of ids
// in input order. Empty strings and malformed UUIDs are dropped silently —
// bumpSegmentUsage is best-effort and should never crash the finalize path
// on garbage input.
func dedupSegmentIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		if !isLikelyUUID(id) {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// isLikelyUUID does a cheap syntactic check (8-4-4-4-12 hex with dashes)
// before we feed an id to a parameterized SQL query. We don't use a real
// UUID parser because:
//
//   - This is a hot path called from every campaign finalize
//   - PostgreSQL's ::uuid cast will reject malformed values authoritatively
//   - We just want to avoid sending obviously-wrong shapes through the
//     network round-trip
func isLikelyUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}
