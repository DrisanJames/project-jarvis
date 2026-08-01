package api

// Click Funnels — the operator surface for the click-drip offer lanes.
//
// A "click funnel" is one offer lane: a row in mailing_offer_journey_map binding
// an Everflow offer to a journey graph, fed by money-slug inlets in
// mailing_offer_slug_map, with per-touch copy in mailing_offer_reminder_subjects.
// All lanes currently share ONE journey graph (click-drip-4touch-72h), so the
// unit the operator actually reasons about is the LANE (offer), not the journey —
// every scope key here is the everflow offer id.
//
// What this file adds over the pre-existing pieces:
//
//   - Per-NODE engagement. Until 2026-08-01 click-drip wrote one shadow campaign
//     per OFFER, so all four touches shared a campaign_id and their opens/clicks
//     were inseparable. The sender now stamps journey_offer_id + journey_node_id
//     per (offer, node) (journey_clickdrip_sender.go), which is what makes the
//     per-node columns below computable at all. Nodes that only ever sent under
//     the old scheme report zeros for engagement and a non-zero `reached` — that
//     is honest, not a bug; see AttributionNote in the response.
//
//   - A bulk clicker inlet. The operator supplies sub1 values (= subscriber ids)
//     from an offer network report. Preview validates and buckets them; enroll
//     writes mailing_journey_event_triggers rows with source='manual_upload'.
//     Writing to the TRIGGER QUEUE rather than straight to enrollments is
//     deliberate: JourneyEventEnroller then applies every existing guarantee
//     unchanged (journey-map gating, already-converted suppression, 24h dedup,
//     sending-profile resolution, ON CONFLICT enrollment dedup). A second
//     enrollment path would have to re-implement all of it and would drift.
//
// CONVERSION SEMANTICS (do not "simplify" this): enrollments.status='converted'
// is NOT a conversion — the journey's terminal `goal` node sets it on anyone who
// completes all four touches (4,635 rows in 60d). The real signal is
// converted_at, written by the Everflow postback (57 rows in the same window).
// Reporting the former as conversions overstates the rate ~81x.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// VersionClickFunnels is bumped on response-shape changes.
const VersionClickFunnels = "1.0"

// maxUploadIDs bounds one paste/upload. Enrolling starts a 4-touch sequence per
// person, so an unbounded paste is an unbounded send.
const maxUploadIDs = 200000

// ClickFunnelsService serves the Click Funnels screen.
type ClickFunnelsService struct {
	db *sql.DB
}

// NewClickFunnelsService wires the service.
func NewClickFunnelsService(db *sql.DB) *ClickFunnelsService {
	return &ClickFunnelsService{db: db}
}

// RegisterRoutes mounts the service under the authenticated /api/mailing prefix.
func (s *ClickFunnelsService) RegisterRoutes(r chi.Router) {
	r.Route("/click-funnels", func(r chi.Router) {
		r.Get("/", s.HandleListFunnels)
		r.Post("/upload/preview", s.HandleUploadPreview)
		r.Post("/upload/enroll", s.HandleUploadEnroll)
		r.Get("/{offerID}/nodes", s.HandleFunnelNodes)
	})
}

// ---------------------------------------------------------------- lane list

// ClickFunnelLane is one offer lane in the list view.
type ClickFunnelLane struct {
	OfferID        string `json:"offer_id"`
	JourneyID      string `json:"journey_id"`
	JourneyName    string `json:"journey_name"`
	Enabled        bool   `json:"enabled"`
	PayoutType     string `json:"payout_type"`
	RoutingState   string `json:"routing_state"`
	RedirectOffer  string `json:"redirect_offer_id"`
	Recommendation string `json:"routing_recommendation"`

	SlugInlets   int `json:"slug_inlets"`
	Active       int `json:"active_enrollments"`
	Enrolled30d  int `json:"enrolled_30d"`
	Conversions  int `json:"conversions_30d"`
	TouchesSent  int `json:"touches_30d"`
	ConfiguredTo int `json:"configured_touches"`
}

// ClickFunnelListResponse envelopes the lane list.
type ClickFunnelListResponse struct {
	APIVersion string            `json:"api_version"`
	Lanes      []ClickFunnelLane `json:"lanes"`
	Orphans    []string          `json:"unmapped_slug_offers"`
}

// HandleListFunnels serves GET /api/mailing/click-funnels.
//
// Includes `unmapped_slug_offers`: offers that have an ENABLED money-slug inlet
// but no journey-map row. Those clicks reach the trigger queue and are then
// dropped with skip_reason='offer_unmapped_at_processing', which is invisible
// unless something surfaces it — offer 1054 sat in exactly that state.
func (s *ClickFunnelsService) HandleListFunnels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := s.db.QueryContext(ctx, `
		SELECT m.everflow_offer_id,
		       COALESCE(m.click_journey_id, ''),
		       COALESCE(j.name, ''),
		       m.enabled,
		       COALESCE(m.payout_type, ''),
		       COALESCE(m.routing_state, 'active'),
		       COALESCE(m.redirect_offer_id, ''),
		       COALESCE(m.routing_recommendation, ''),
		       (SELECT COUNT(*) FROM mailing_offer_slug_map s
		         WHERE s.everflow_offer_id = m.everflow_offer_id AND s.enabled)                    AS slug_inlets,
		       (SELECT COUNT(*) FROM mailing_journey_enrollments e
		         WHERE e.enrollment_offer_id = m.everflow_offer_id AND e.status = 'active')        AS active_enrollments,
		       (SELECT COUNT(*) FROM mailing_journey_enrollments e
		         WHERE e.enrollment_offer_id = m.everflow_offer_id
		           AND e.enrolled_at > NOW() - INTERVAL '30 days')                                 AS enrolled_30d,
		       (SELECT COUNT(*) FROM mailing_journey_enrollments e
		         WHERE e.enrollment_offer_id = m.everflow_offer_id
		           AND e.converted_at > NOW() - INTERVAL '30 days')                                AS conversions_30d,
		       (SELECT COUNT(*) FROM mailing_message_log ml
		          JOIN mailing_campaigns c ON c.id = ml.campaign_id
		         WHERE c.journey_offer_id = m.everflow_offer_id
		           AND ml.sent_at > NOW() - INTERVAL '30 days')                                    AS touches_30d,
		       (SELECT COUNT(*) FROM mailing_offer_reminder_subjects rs
		         WHERE rs.everflow_offer_id = m.everflow_offer_id AND rs.enabled)                  AS configured_touches
		FROM mailing_offer_journey_map m
		LEFT JOIN mailing_journeys j ON j.id = m.click_journey_id
		ORDER BY m.enabled DESC, active_enrollments DESC, m.everflow_offer_id
	`)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load funnels: "+err.Error())
		return
	}
	defer rows.Close()

	lanes := make([]ClickFunnelLane, 0, 32)
	for rows.Next() {
		var l ClickFunnelLane
		if err := rows.Scan(
			&l.OfferID, &l.JourneyID, &l.JourneyName, &l.Enabled, &l.PayoutType,
			&l.RoutingState, &l.RedirectOffer, &l.Recommendation,
			&l.SlugInlets, &l.Active, &l.Enrolled30d, &l.Conversions,
			&l.TouchesSent, &l.ConfiguredTo,
		); err != nil {
			respondError(w, http.StatusInternalServerError, "scan funnel: "+err.Error())
			return
		}
		lanes = append(lanes, l)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "iterate funnels: "+err.Error())
		return
	}

	orphans := []string{}
	orows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT s.everflow_offer_id
		FROM mailing_offer_slug_map s
		LEFT JOIN mailing_offer_journey_map m ON m.everflow_offer_id = s.everflow_offer_id
		WHERE s.enabled AND m.everflow_offer_id IS NULL
		ORDER BY 1
	`)
	if err == nil {
		defer orows.Close()
		for orows.Next() {
			var o string
			if err := orows.Scan(&o); err == nil {
				orphans = append(orphans, o)
			}
		}
	}

	writeJSON(w, http.StatusOK, ClickFunnelListResponse{
		APIVersion: VersionClickFunnels,
		Lanes:      lanes,
		Orphans:    orphans,
	})
}

// --------------------------------------------------------------- node detail

// ClickFunnelNode is one node of a lane's graph with its copy and its metrics.
type ClickFunnelNode struct {
	NodeID   string `json:"node_id"`
	Type     string `json:"type"`
	Label    string `json:"label"`
	Sequence int    `json:"sequence_index"`
	DelayMs  int64  `json:"delay_ms"`

	// Creative — for email nodes, the per-touch copy the sender applies.
	// Body HTML is intentionally absent: click-drip reuses the creative of the
	// campaign the subscriber originally clicked, so the body is PER-SUBSCRIBER
	// and there is no single node-level body to show.
	Subject      string `json:"subject"`
	Preheader    string `json:"preheader"`
	FromOverride string `json:"from_name_override"`
	CopyEnabled  bool   `json:"copy_enabled"`
	CopyMissing  bool   `json:"copy_missing"`

	// Flow
	Reached  int `json:"reached"`
	Awaiting int `json:"awaiting"`
	Errors   int `json:"errors"`

	// Engagement (per-node, from this node's own shadow campaign)
	Sent        int `json:"sent"`
	Delivered   int `json:"delivered"`
	Opens       int `json:"opens"`
	Clicks      int `json:"clicks"`
	Unsubs      int `json:"unsubscribes"`
	HardBounce  int `json:"hard_bounce"`
	SoftBounce  int `json:"soft_bounce"`
	Conversions int `json:"conversions"`

	// Rates, 0-100, computed server-side so every surface agrees.
	OpenRate       float64 `json:"open_rate"`
	ClickRate      float64 `json:"click_rate"`
	ConversionRate float64 `json:"conversion_rate"`
	StepThrough    float64 `json:"step_through_rate"`

	Attributed bool `json:"attributed"`
}

// ClickFunnelNodesResponse envelopes the node view.
type ClickFunnelNodesResponse struct {
	APIVersion      string            `json:"api_version"`
	OfferID         string            `json:"offer_id"`
	JourneyID       string            `json:"journey_id"`
	Nodes           []ClickFunnelNode `json:"nodes"`
	TotalEnrolled   int               `json:"total_enrolled"`
	TotalActive     int               `json:"total_active"`
	TotalConverted  int               `json:"total_converted"`
	AttributionNote string            `json:"attribution_note"`
}

// journeyGraphNode mirrors the persisted node JSON.
type journeyGraphNode struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Name   string `json:"name"`
	Config struct {
		Subject       string  `json:"subject"`
		DelayHours    float64 `json:"delay_hours"`
		DelayMinutes  float64 `json:"delay_minutes"`
		ReminderIndex *int    `json:"reminder_sequence_index"`
	} `json:"config"`
}

// HandleFunnelNodes serves GET /api/mailing/click-funnels/{offerID}/nodes.
func (s *ClickFunnelsService) HandleFunnelNodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	offerID := strings.TrimSpace(chi.URLParam(r, "offerID"))
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offerID required")
		return
	}

	var journeyID string
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(click_journey_id,'') FROM mailing_offer_journey_map WHERE everflow_offer_id=$1`,
		offerID).Scan(&journeyID); err != nil {
		if err == sql.ErrNoRows {
			respondError(w, http.StatusNotFound, "no funnel configured for offer "+offerID)
			return
		}
		respondError(w, http.StatusInternalServerError, "resolve funnel: "+err.Error())
		return
	}

	graph, err := s.loadGraphNodes(ctx, journeyID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "load journey graph: "+err.Error())
		return
	}

	reached, errorsByNode, err := s.loadNodeFlow(ctx, offerID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "load node flow: "+err.Error())
		return
	}
	awaiting, err := s.loadAwaiting(ctx, offerID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "load awaiting: "+err.Error())
		return
	}
	engagement, err := s.loadNodeEngagement(ctx, offerID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "load node engagement: "+err.Error())
		return
	}
	conversions, err := s.loadNodeConversions(ctx, offerID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "load node conversions: "+err.Error())
		return
	}
	copyBySeq, err := s.loadReminderCopy(ctx, offerID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "load reminder copy: "+err.Error())
		return
	}

	var totalEnrolled, totalActive, totalConverted int
	_ = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE status='active'),
		       COUNT(converted_at)
		FROM mailing_journey_enrollments WHERE enrollment_offer_id=$1
	`, offerID).Scan(&totalEnrolled, &totalActive, &totalConverted)

	// Step-through denominator: the first node's reach, so each node reads as
	// "% of everyone who entered the funnel that got this far".
	entry := 0
	for _, g := range graph {
		if r, ok := reached[g.ID]; ok && r > entry {
			entry = r
		}
	}

	seq := 0
	nodes := make([]ClickFunnelNode, 0, len(graph))
	anyAttributed := false
	for _, g := range graph {
		n := ClickFunnelNode{
			NodeID:   g.ID,
			Type:     g.Type,
			Label:    g.Name,
			Sequence: -1,
			Reached:  reached[g.ID],
			Awaiting: awaiting[g.ID],
			Errors:   errorsByNode[g.ID],
		}
		if n.Label == "" {
			n.Label = g.ID
		}
		n.DelayMs = int64(g.Config.DelayHours*3600000 + g.Config.DelayMinutes*60000)

		if g.Type == "email" {
			if g.Config.ReminderIndex != nil {
				n.Sequence = *g.Config.ReminderIndex
			} else {
				n.Sequence = seq
			}
			seq++

			if c, ok := copyBySeq[n.Sequence]; ok {
				n.Subject, n.Preheader, n.FromOverride, n.CopyEnabled = c.subject, c.preheader, c.fromOverride, c.enabled
			} else {
				// No row means the sender falls back to the clicked campaign's
				// own subject — worth flagging rather than rendering blank.
				n.CopyMissing = true
			}
			if e, ok := engagement[g.ID]; ok {
				n.Sent, n.Delivered, n.Opens, n.Clicks = e.sent, e.delivered, e.opens, e.clicks
				n.Unsubs, n.HardBounce, n.SoftBounce = e.unsubs, e.hard, e.soft
				n.Attributed = true
				anyAttributed = true
			}
			n.Conversions = conversions[g.ID]

			base := float64(n.Delivered)
			if base == 0 {
				base = float64(n.Sent)
			}
			if base > 0 {
				n.OpenRate = round2(float64(n.Opens) / base * 100)
				n.ClickRate = round2(float64(n.Clicks) / base * 100)
			}
			if n.Reached > 0 {
				n.ConversionRate = round2(float64(n.Conversions) / float64(n.Reached) * 100)
			}
		}

		if entry > 0 {
			n.StepThrough = round2(float64(n.Reached) / float64(entry) * 100)
		}
		nodes = append(nodes, n)
	}

	note := "Per-node engagement is attributed through each node's own shadow campaign. " +
		"Touches sent before node-level attribution shipped (2026-08-01) counted toward Reached but " +
		"cannot be split by node, so their opens/clicks are not shown here."
	if !anyAttributed {
		note = "No node-attributed sends yet. Reached counts are live, but per-node opens/clicks stay " +
			"at zero until this lane sends its next touch after the 2026-08-01 attribution change."
	}

	writeJSON(w, http.StatusOK, ClickFunnelNodesResponse{
		APIVersion:      VersionClickFunnels,
		OfferID:         offerID,
		JourneyID:       journeyID,
		Nodes:           nodes,
		TotalEnrolled:   totalEnrolled,
		TotalActive:     totalActive,
		TotalConverted:  totalConverted,
		AttributionNote: note,
	})
}

func (s *ClickFunnelsService) loadGraphNodes(ctx context.Context, journeyID string) ([]journeyGraphNode, error) {
	var raw []byte
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(nodes::text, '[]') FROM mailing_journeys WHERE id=$1`, journeyID).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("journey %s not found", journeyID)
		}
		return nil, err
	}
	var out []journeyGraphNode
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// loadNodeFlow returns per-node distinct enrollments that executed the node
// (Reached) and per-node error counts, scoped to the lane.
func (s *ClickFunnelsService) loadNodeFlow(ctx context.Context, offerID string) (map[string]int, map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT l.node_id,
		       COUNT(DISTINCT l.enrollment_id) FILTER (WHERE l.action <> 'error') AS reached,
		       COUNT(*) FILTER (WHERE l.action = 'error')                          AS errs
		FROM mailing_journey_execution_log l
		JOIN mailing_journey_enrollments e ON e.id = l.enrollment_id
		WHERE e.enrollment_offer_id = $1
		GROUP BY l.node_id
	`, offerID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	reached, errs := map[string]int{}, map[string]int{}
	for rows.Next() {
		var node string
		var rc, ec int
		if err := rows.Scan(&node, &rc, &ec); err != nil {
			return nil, nil, err
		}
		reached[node], errs[node] = rc, ec
	}
	return reached, errs, rows.Err()
}

func (s *ClickFunnelsService) loadAwaiting(ctx context.Context, offerID string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(current_node_id,''), COUNT(*)
		FROM mailing_journey_enrollments
		WHERE enrollment_offer_id=$1 AND status='active'
		GROUP BY 1
	`, offerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var node string
		var n int
		if err := rows.Scan(&node, &n); err != nil {
			return nil, err
		}
		out[node] = n
	}
	return out, rows.Err()
}

type nodeEngagement struct {
	sent, delivered, opens, clicks, unsubs, hard, soft int
}

// loadNodeEngagement aggregates each node's own shadow campaign. Uses the
// canonical HardBounceSQL fragment so the hard/soft split matches every other
// metrics surface (.cursor/rules/bounce-metrics.mdc).
func (s *ClickFunnelsService) loadNodeEngagement(ctx context.Context, offerID string) (map[string]nodeEngagement, error) {
	hardSQL := HardBounceSQL("t")
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.journey_node_id,
		       COALESCE(SUM(CASE WHEN t.event_type='sent'         THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN t.event_type='delivered'    THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN t.event_type='opened'       THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN t.event_type='clicked'      THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN t.event_type='unsubscribed' THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN t.event_type='bounced' AND `+hardSQL+`       THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN t.event_type='bounced' AND NOT (`+hardSQL+`) THEN 1 ELSE 0 END),0)
		FROM mailing_campaigns c
		LEFT JOIN mailing_tracking_events t ON t.campaign_id = c.id
		WHERE c.journey_offer_id = $1 AND c.journey_node_id IS NOT NULL
		GROUP BY c.journey_node_id
	`, offerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]nodeEngagement{}
	for rows.Next() {
		var node string
		var e nodeEngagement
		if err := rows.Scan(&node, &e.sent, &e.delivered, &e.opens, &e.clicks, &e.unsubs, &e.hard, &e.soft); err != nil {
			return nil, err
		}
		out[node] = e
	}
	return out, rows.Err()
}

// loadNodeConversions does LAST-TOUCH attribution: a lane conversion is credited
// to the last email node executed at or before converted_at. Uses converted_at
// (the Everflow postback), never status='converted' — that is the terminal goal
// node firing on sequence completion and would overstate conversions ~81x.
func (s *ClickFunnelsService) loadNodeConversions(ctx context.Context, offerID string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH conv AS (
		    SELECT id, converted_at
		    FROM mailing_journey_enrollments
		    WHERE enrollment_offer_id = $1 AND converted_at IS NOT NULL
		),
		last_touch AS (
		    SELECT (
		        SELECT l.node_id
		        FROM mailing_journey_execution_log l
		        WHERE l.enrollment_id = conv.id
		          AND l.node_type = 'email'
		          AND l.action <> 'error'
		          AND l.executed_at <= conv.converted_at
		        ORDER BY l.executed_at DESC
		        LIMIT 1
		    ) AS node_id
		    FROM conv
		)
		SELECT node_id, COUNT(*) FROM last_touch WHERE node_id IS NOT NULL GROUP BY node_id
	`, offerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var node string
		var n int
		if err := rows.Scan(&node, &n); err != nil {
			return nil, err
		}
		out[node] = n
	}
	return out, rows.Err()
}

type reminderCopy struct {
	subject, preheader, fromOverride string
	enabled                          bool
}

func (s *ClickFunnelsService) loadReminderCopy(ctx context.Context, offerID string) (map[int]reminderCopy, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sequence_index, COALESCE(subject,''), COALESCE(preheader,''),
		       COALESCE(from_name_override,''), COALESCE(enabled,false)
		FROM mailing_offer_reminder_subjects
		WHERE everflow_offer_id = $1
	`, offerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]reminderCopy{}
	for rows.Next() {
		var idx int
		var c reminderCopy
		if err := rows.Scan(&idx, &c.subject, &c.preheader, &c.fromOverride, &c.enabled); err != nil {
			return nil, err
		}
		out[idx] = c
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------- upload

// UploadRequest is the shared body for preview and enroll.
type UploadRequest struct {
	OfferID string `json:"offer_id"`
	// Sub1s accepts raw pasted text or a parsed list. Whitespace, commas,
	// quotes and a leading header line are tolerated — operators paste straight
	// out of an offer-network CSV.
	Raw   string   `json:"raw"`
	Sub1s []string `json:"sub1s"`
	// Confirm must be true on enroll. A preview response carries the token the
	// operator saw; requiring it back makes "enroll" a deliberate second act.
	Confirm bool `json:"confirm"`
}

// UploadPreview is the bucketed result the operator approves.
type UploadPreview struct {
	APIVersion string `json:"api_version"`
	OfferID    string `json:"offer_id"`
	JourneyID  string `json:"journey_id"`
	LaneEnabled bool  `json:"lane_enabled"`

	Submitted        int `json:"submitted"`
	Malformed        int `json:"malformed"`
	Duplicates       int `json:"duplicates_in_file"`
	Unknown          int `json:"unknown_subscriber"`
	AlreadyActive    int `json:"already_active"`
	AlreadyConverted int `json:"already_converted"`
	RecentlyTriggered int `json:"recently_triggered"`
	Ready            int `json:"ready"`

	SampleReady   []string `json:"sample_ready"`
	SampleUnknown []string `json:"sample_unknown"`
	Warnings      []string `json:"warnings"`
}

// HandleUploadPreview serves POST /api/mailing/click-funnels/upload/preview.
// Read-only: it never writes a trigger row.
func (s *ClickFunnelsService) HandleUploadPreview(w http.ResponseWriter, r *http.Request) {
	req, err := decodeUploadRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	pv, _, err := s.buildPreview(r.Context(), req)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pv)
}

// UploadResult reports what enroll actually wrote.
type UploadResult struct {
	APIVersion string        `json:"api_version"`
	OfferID    string        `json:"offer_id"`
	Enqueued   int           `json:"enqueued"`
	Skipped    int           `json:"skipped"`
	Preview    UploadPreview `json:"preview"`
	Note       string        `json:"note"`
}

// HandleUploadEnroll serves POST /api/mailing/click-funnels/upload/enroll.
//
// Writes mailing_journey_event_triggers rows with source='manual_upload'. It
// does NOT create enrollments directly — JourneyEventEnroller drains the queue
// within ~5s and applies the same gating every organic click gets. Requires
// confirm=true so a preview cannot become a send by accident.
func (s *ClickFunnelsService) HandleUploadEnroll(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, err := decodeUploadRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !req.Confirm {
		respondError(w, http.StatusBadRequest, "confirm=true required: enrolling starts a live reminder sequence")
		return
	}

	orgID := getOrgID(r)

	pv, ready, err := s.buildPreview(ctx, req)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(ready) == 0 {
		writeJSON(w, http.StatusOK, UploadResult{
			APIVersion: VersionClickFunnels, OfferID: req.OfferID,
			Enqueued: 0, Skipped: pv.Submitted, Preview: pv,
			Note: "nothing enrolled — no rows passed validation",
		})
		return
	}

	// One statement, ON CONFLICT-free but idempotent in effect: the 24h
	// per-(subscriber, offer) trigger guard is re-applied inside the INSERT so a
	// double-submit cannot double-enroll even between preview and enroll.
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO mailing_journey_event_triggers (
			id, source, everflow_offer_id, subscriber_id, subscriber_email,
			sub2_brand, sub3_campaign_id, click_id, sending_profile_id,
			sending_domain, click_url, status, received_at
		)
		SELECT gen_random_uuid(), 'manual_upload', $1, s.id, NULL,
		       '', '', '', NULL,
		       '', '', 'pending', NOW()
		FROM mailing_subscribers s
		WHERE s.id = ANY($2::uuid[])
		  AND ($3::uuid IS NULL OR s.organization_id = $3::uuid)
		  AND NOT EXISTS (
		        SELECT 1 FROM mailing_journey_event_triggers t
		        WHERE t.subscriber_id = s.id AND t.everflow_offer_id = $1
		          AND t.received_at > NOW() - INTERVAL '24 hours'
		  )
	`, req.OfferID, pq.Array(ready), clickFunnelOrgFilter(orgID))
	if err != nil {
		respondError(w, http.StatusInternalServerError, "enqueue triggers: "+err.Error())
		return
	}
	n, _ := res.RowsAffected()

	writeJSON(w, http.StatusOK, UploadResult{
		APIVersion: VersionClickFunnels,
		OfferID:    req.OfferID,
		Enqueued:   int(n),
		Skipped:    pv.Submitted - int(n),
		Preview:    pv,
		Note: "queued to mailing_journey_event_triggers (source=manual_upload). " +
			"JourneyEventEnroller drains within ~5s and applies journey gating, " +
			"converted-suppression and enrollment dedup; the first touch fires on the node's delay.",
	})
}

// buildPreview validates and buckets the submitted ids. Returns the preview and
// the ids that are actually enrollable.
func (s *ClickFunnelsService) buildPreview(ctx context.Context, req UploadRequest) (UploadPreview, []string, error) {
	pv := UploadPreview{APIVersion: VersionClickFunnels, OfferID: req.OfferID, Warnings: []string{}}

	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(click_journey_id,''), enabled FROM mailing_offer_journey_map WHERE everflow_offer_id=$1`,
		req.OfferID).Scan(&pv.JourneyID, &pv.LaneEnabled); err != nil {
		if err == sql.ErrNoRows {
			return pv, nil, fmt.Errorf("offer %s has no journey lane — configure the funnel before uploading", req.OfferID)
		}
		return pv, nil, err
	}
	if !pv.LaneEnabled {
		pv.Warnings = append(pv.Warnings, "This lane is DISABLED. Uploaded rows will queue but the enroller will skip them until it is enabled.")
	}

	raw := append([]string{}, req.Sub1s...)
	raw = append(raw, splitUploadText(req.Raw)...)

	seen := map[string]bool{}
	var ids []string
	for _, tok := range raw {
		tok = strings.Trim(strings.TrimSpace(tok), `"',`)
		if tok == "" {
			continue
		}
		pv.Submitted++
		parsed, err := uuid.Parse(tok)
		if err != nil {
			pv.Malformed++
			if len(pv.SampleUnknown) < 5 {
				pv.SampleUnknown = append(pv.SampleUnknown, tok)
			}
			continue
		}
		id := parsed.String()
		if seen[id] {
			pv.Duplicates++
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) > maxUploadIDs {
		return pv, nil, fmt.Errorf("upload too large: %d ids (max %d)", len(ids), maxUploadIDs)
	}
	if len(ids) == 0 {
		return pv, nil, nil
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id,
		       EXISTS (SELECT 1 FROM mailing_journey_enrollments e
		                WHERE e.enrollment_offer_id = $2 AND e.status = 'active'
		                  AND e.subscriber_email = s.email)                              AS active,
		       EXISTS (SELECT 1 FROM mailing_journey_enrollments e
		                WHERE e.enrollment_offer_id = $2 AND e.converted_at IS NOT NULL
		                  AND e.subscriber_email = s.email)                              AS converted,
		       EXISTS (SELECT 1 FROM mailing_journey_event_triggers t
		                WHERE t.subscriber_id = s.id AND t.everflow_offer_id = $2
		                  AND t.received_at > NOW() - INTERVAL '24 hours')               AS triggered
		FROM mailing_subscribers s
		WHERE s.id = ANY($1::uuid[])
	`, pq.Array(ids), req.OfferID)
	if err != nil {
		return pv, nil, err
	}
	defer rows.Close()

	known := map[string]bool{}
	ready := make([]string, 0, len(ids))
	for rows.Next() {
		var id string
		var active, converted, triggered bool
		if err := rows.Scan(&id, &active, &converted, &triggered); err != nil {
			return pv, nil, err
		}
		known[id] = true
		switch {
		case converted:
			pv.AlreadyConverted++
		case active:
			pv.AlreadyActive++
		case triggered:
			pv.RecentlyTriggered++
		default:
			ready = append(ready, id)
			if len(pv.SampleReady) < 5 {
				pv.SampleReady = append(pv.SampleReady, id)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return pv, nil, err
	}

	for _, id := range ids {
		if !known[id] {
			pv.Unknown++
			if len(pv.SampleUnknown) < 5 {
				pv.SampleUnknown = append(pv.SampleUnknown, id)
			}
		}
	}
	pv.Ready = len(ready)

	if pv.Ready == 0 && pv.Submitted > 0 {
		pv.Warnings = append(pv.Warnings, "No rows are enrollable — check that these sub1 values are subscriber ids for this platform.")
	}
	return pv, ready, nil
}

func decodeUploadRequest(r *http.Request) (UploadRequest, error) {
	var req UploadRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 32<<20)).Decode(&req); err != nil {
		return req, fmt.Errorf("invalid JSON body: %w", err)
	}
	req.OfferID = strings.TrimSpace(req.OfferID)
	if req.OfferID == "" {
		return req, fmt.Errorf("offer_id required")
	}
	return req, nil
}

// splitUploadText tolerates newline / comma / tab / space separated pastes and
// drops an obvious header token.
func splitUploadText(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == '\t' || r == ' ' || r == ';'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		lf := strings.ToLower(strings.Trim(f, `"' `))
		if lf == "sub1" || lf == "subscriber_id" || lf == "subscriberid" || lf == "id" {
			continue
		}
		out = append(out, f)
	}
	return out
}

// clickFunnelOrgFilter yields the org id only when it is a real UUID, so the
// $3::uuid cast in the enroll INSERT degrades to "no org filter" instead of
// erroring when org resolution returns a non-UUID (dev single-tenant fallback).
// The package-level nullableUUID passes non-UUID strings straight through,
// which would blow up that cast.
func clickFunnelOrgFilter(s string) interface{} {
	if _, err := uuid.Parse(s); err != nil {
		return nil
	}
	return s
}
