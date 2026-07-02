package api

// dashboard_console_click_funnels.go — Dashboard Console: click-funnels card.
//
// GET /api/mailing/dashboard/click-funnels
//
// Read-only. REPLACES the dead journey-center overview data source for the
// dashboard's click-drip funnel card. It reports the LIVE click-drip lanes.
//
// LIVE click-drip data model (verified against runtime, 2026-07-02):
//   - mailing_offer_journey_map — the lane REGISTRY. One row per Everflow offer
//     wired for click-drip: everflow_offer_id (PK), click_journey_id, payout_type,
//     enabled. This is the inlet the enroller re-validates on every enrollment
//     (journey_event_enroller.go: "SELECT ... FROM mailing_offer_journey_map").
//   - mailing_journey_enrollments — the ENROLLMENT table the enroller INSERTs
//     into (status='active', enrolled_via='click_postback', enrollment_offer_id =
//     the Everflow offer id). Status lifecycle: active → completed/converted/exited.
//     There is a partial index idx_journey_enrollments_active_by_offer on
//     (enrollment_offer_id, status) WHERE status='active', which the active count
//     below uses.
//   - mailing_offers — display name, joined by everflow_offer_id.
//
// WHAT EACH NUMBER MEANS (no fabrication — every count is a real column read):
//   - lanes[].offer_id             = everflow_offer_id (the lane key; a string
//                                    like "1090", NOT the mailing_offers UUID).
//   - lanes[].name                 = mailing_offers.name for that everflow id,
//                                    "" if the offer isn't in the offers catalog.
//   - lanes[].enabled              = mailing_offer_journey_map.enabled (operator
//                                    toggle; a disabled lane still shows history).
//   - lanes[].payout_type          = CPM/CPA/CPL/CPC/… (CPC lanes are enrolled-skip).
//   - lanes[].click_journey_id     = the journey graph this lane feeds.
//   - lanes[].active_enrolled      = CURRENT in-journey count = COUNT(*) of
//                                    mailing_journey_enrollments WHERE
//                                    enrollment_offer_id = <lane> AND status='active'.
//   - lanes[].completed_or_converted = LIFETIME (all-time, NOT windowed) count of
//                                    enrollments for the lane with status IN
//                                    ('completed','converted'). Documented as
//                                    lifetime because enrollments carry no cheap
//                                    per-window completion timestamp to bound on.
//   - total_active_lanes           = number of lanes with enabled=true.
//   - total_enrolled               = SUM of active_enrolled across ALL lanes.
//
// KNOWN GAPS (surfaced rather than faked):
//   - completed_or_converted is lifetime, not window-scoped (see above).
//   - "converted" here means the enrollment reached a converted STATUS; the
//     authoritative revenue-conversion ledger is mailing_offer_suppressions
//     (reason='converted') — used by the trending-offers card. These can differ:
//     an enrollment may convert via postback before its status is advanced.
//   - enrollment_offer_id is only populated for click_postback enrollments;
//     segment/other enrollment paths are not attributed to a lane here.
import (
	"context"
	"database/sql"
	"net/http"
	"time"
)

// VersionDashboardClickFunnels tracks this endpoint's response contract.
//
// 1.0 (2026-07-02): initial — live click-drip lanes from mailing_offer_journey_map
// joined to mailing_journey_enrollments active/terminal counts.
const VersionDashboardClickFunnels = "1.0"

type clickFunnelLane struct {
	OfferID              string `json:"offer_id"` // everflow_offer_id (lane key)
	Name                 string `json:"name"`
	Enabled              bool   `json:"enabled"`
	PayoutType           string `json:"payout_type"`
	ClickJourneyID       string `json:"click_journey_id"`
	ActiveEnrolled       int    `json:"active_enrolled"`        // current in-journey
	CompletedOrConverted int    `json:"completed_or_converted"` // LIFETIME
}

// HandleDashboardClickFunnels serves GET /api/mailing/dashboard/click-funnels.
func (s *Server) HandleDashboardClickFunnels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	// The lane registry is tiny (one row per live offer). Correlated
	// subqueries against the (enrollment_offer_id, status) partial index are
	// cheap at this cardinality and keep the shape one-row-per-lane.
	query := `
		SELECT
			jm.everflow_offer_id,
			COALESCE(
				(SELECT o.name FROM mailing_offers o
				  WHERE o.everflow_offer_id = jm.everflow_offer_id
				  ORDER BY o.created_at ASC LIMIT 1), '') AS name,
			jm.enabled,
			COALESCE(jm.payout_type, 'UNKNOWN') AS payout_type,
			COALESCE(jm.click_journey_id, '') AS click_journey_id,
			COALESCE((SELECT COUNT(*) FROM mailing_journey_enrollments e
				WHERE e.enrollment_offer_id = jm.everflow_offer_id
				  AND e.status = 'active'), 0) AS active_enrolled,
			COALESCE((SELECT COUNT(*) FROM mailing_journey_enrollments e
				WHERE e.enrollment_offer_id = jm.everflow_offer_id
				  AND e.status IN ('completed', 'converted')), 0) AS completed_or_converted
		FROM mailing_offer_journey_map jm
		ORDER BY active_enrolled DESC, jm.everflow_offer_id ASC`

	rows, err := s.mailingDB.QueryContext(ctx, query)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "database query failed")
		return
	}
	defer rows.Close()

	lanes := make([]clickFunnelLane, 0)
	totalActiveLanes := 0
	totalEnrolled := 0
	for rows.Next() {
		var lane clickFunnelLane
		var name sql.NullString
		if err := rows.Scan(
			&lane.OfferID, &name, &lane.Enabled, &lane.PayoutType,
			&lane.ClickJourneyID, &lane.ActiveEnrolled, &lane.CompletedOrConverted,
		); err != nil {
			continue
		}
		lane.Name = name.String
		if lane.Enabled {
			totalActiveLanes++
		}
		totalEnrolled += lane.ActiveEnrolled
		lanes = append(lanes, lane)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"lanes":              lanes,
		"total_active_lanes": totalActiveLanes,
		"total_enrolled":     totalEnrolled,
		"api_version":        VersionDashboardClickFunnels,
		"generated_at":       time.Now().UTC(),
	})
}
