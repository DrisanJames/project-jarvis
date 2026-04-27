package api

// HandleGetJourneyNodeStats serves
//   GET /api/mailing/journeys/{journeyId}/node-stats
//
// Returns a map keyed by journey_node_id with lifetime delivery
// metrics rolled up from every shadow campaign that was created for
// that node, plus the live "audience awaiting injection" count read
// from the enrollment table.
//
// Why this endpoint exists:
//
//   - The Welcome Series canvas tile shows Delivered / Opens / Clicks /
//     Hard Bounce / Soft Bounce / Awaiting per node. Without this
//     endpoint the tile is decorative.
//   - The shadow campaign approach means every journey email is a real
//     mailing_campaigns row tagged with journey_id + journey_node_id.
//     Aggregating tracking events over those rows matches every other
//     surface in the system (campaign list, dashboard, ISP perf).
//   - Hard / soft bounce split uses HardBounceSQL from
//     internal/api/metrics.go (the canonical fragment), per the
//     bounce-metrics rule in .cursor/rules/bounce-metrics.mdc.
//
// Response shape:
//
//   {
//     "api_version": "1.0",
//     "journey_id": "...",
//     "nodes": {
//       "node_email_1": {
//         "audience_awaiting": 12,
//         "sent": 1500,
//         "delivered": 1480,
//         "opens": 320,
//         "clicks": 45,
//         "hard_bounce": 12,
//         "soft_bounce": 8,
//         "shadow_campaigns": 3
//       }
//     }
//   }

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// VersionJourneyNodeStats is bumped on shape changes to the response
// body so the page footer round-trip catches stale deploys.
const VersionJourneyNodeStats = "1.0"

// JourneyNodeStat is one row in the response map.
type JourneyNodeStat struct {
	AudienceAwaiting int `json:"audience_awaiting"`
	Sent             int `json:"sent"`
	Delivered        int `json:"delivered"`
	Opens            int `json:"opens"`
	Clicks           int `json:"clicks"`
	HardBounce       int `json:"hard_bounce"`
	SoftBounce       int `json:"soft_bounce"`
	ShadowCampaigns  int `json:"shadow_campaigns"`
}

// JourneyNodeStatsResponse is the full response envelope.
type JourneyNodeStatsResponse struct {
	APIVersion string                     `json:"api_version"`
	JourneyID  string                     `json:"journey_id"`
	Nodes      map[string]JourneyNodeStat `json:"nodes"`
}

// HandleGetJourneyNodeStats is the chi handler.
func (jb *JourneyBuilder) HandleGetJourneyNodeStats(w http.ResponseWriter, r *http.Request) {
	journeyID := chi.URLParam(r, "journeyId")
	if journeyID == "" {
		writeJourneyNodeStatsError(w, http.StatusBadRequest, "journeyId required")
		return
	}

	stats, err := loadJourneyNodeStats(r.Context(), jb.db, journeyID)
	if err != nil {
		writeJourneyNodeStatsError(w, http.StatusInternalServerError, "failed to load node stats: "+err.Error())
		return
	}

	resp := JourneyNodeStatsResponse{
		APIVersion: VersionJourneyNodeStats,
		JourneyID:  journeyID,
		Nodes:      stats,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// loadJourneyNodeStats does two queries:
//
//  1. Aggregate mailing_tracking_events joined to mailing_campaigns
//     filtered by journey_id, grouped by journey_node_id. This gives us
//     send / delivered / open / click / bounce counts per node from the
//     canonical event log, using the same hard/soft split as every
//     other metrics surface (HardBounceSQL).
//  2. Count enrollments still waiting at each node from
//     mailing_journey_enrollments. The "audience_awaiting" metric is
//     the source of truth that the canvas tile surfaces.
//
// We merge the two result sets in Go so missing rows in either side
// don't blow up the response (a node with no shadow campaigns yet but
// 50 awaiting enrollments still shows up).
func loadJourneyNodeStats(ctx context.Context, db *sql.DB, journeyID string) (map[string]JourneyNodeStat, error) {
	out := make(map[string]JourneyNodeStat)

	hardSQL := HardBounceSQL("t")

	// Lifetime metrics from shadow campaigns.
	rows, err := db.QueryContext(ctx, `
		SELECT
			c.journey_node_id,
			COUNT(DISTINCT c.id)                                                                             AS shadow_campaigns,
			COALESCE(SUM(CASE WHEN t.event_type = 'sent'      THEN 1 ELSE 0 END), 0)                         AS sent,
			COALESCE(SUM(CASE WHEN t.event_type = 'delivered' THEN 1 ELSE 0 END), 0)                         AS delivered,
			COALESCE(SUM(CASE WHEN t.event_type = 'opened'    THEN 1 ELSE 0 END), 0)                         AS opens,
			COALESCE(SUM(CASE WHEN t.event_type = 'clicked'   THEN 1 ELSE 0 END), 0)                         AS clicks,
			COALESCE(SUM(CASE WHEN t.event_type = 'bounced' AND `+hardSQL+`        THEN 1 ELSE 0 END), 0) AS hard_bounce,
			COALESCE(SUM(CASE WHEN t.event_type = 'bounced' AND NOT (`+hardSQL+`)  THEN 1 ELSE 0 END), 0) AS soft_bounce
		FROM mailing_campaigns c
		LEFT JOIN mailing_tracking_events t ON t.campaign_id = c.id
		WHERE c.journey_id = $1
		  AND c.journey_node_id IS NOT NULL
		GROUP BY c.journey_node_id
	`, journeyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var nodeID sql.NullString
		var stat JourneyNodeStat
		if err := rows.Scan(
			&nodeID,
			&stat.ShadowCampaigns,
			&stat.Sent, &stat.Delivered, &stat.Opens, &stat.Clicks,
			&stat.HardBounce, &stat.SoftBounce,
		); err != nil {
			continue
		}
		if !nodeID.Valid || nodeID.String == "" {
			continue
		}
		out[nodeID.String] = stat
	}

	// Audience awaiting injection per current node.
	awaitingRows, err := db.QueryContext(ctx, `
		SELECT current_node_id, COUNT(*)
		FROM mailing_journey_enrollments
		WHERE journey_id = $1
		  AND status IN ('active', 'wait_for_send', 'processing', 'waiting', 'pending')
		GROUP BY current_node_id
	`, journeyID)
	if err != nil {
		return out, nil
	}
	defer awaitingRows.Close()

	for awaitingRows.Next() {
		var nodeID sql.NullString
		var count int
		if err := awaitingRows.Scan(&nodeID, &count); err != nil {
			continue
		}
		if !nodeID.Valid || nodeID.String == "" {
			continue
		}
		stat := out[nodeID.String]
		stat.AudienceAwaiting = count
		out[nodeID.String] = stat
	}

	return out, nil
}

// writeJourneyNodeStatsError mirrors the error envelope used by the
// rest of the journey handlers so the frontend's existing error
// handling keeps working without special cases.
func writeJourneyNodeStatsError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"api_version": VersionJourneyNodeStats,
		"error":       msg,
	})
}
