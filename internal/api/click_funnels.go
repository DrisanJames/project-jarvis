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
	"strconv"
	"strings"
	"sync"
	"time"

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

	// attrMu/attrPresent/attrProbed cache whether mailing_campaigns has the
	// node-attribution columns.
	//
	// WHY (2026-08-02): those columns arrive via DDL that can lose a long race
	// for an ACCESS EXCLUSIVE lock on a continuously-read mailing_campaigns
	// (measured: four overlapping CPM attribution queries, the oldest 9+ minutes,
	// starve it indefinitely). The SEND path already degrades when they are
	// absent — but every READ here referenced them too, so the whole screen 500'd
	// with `column c.journey_offer_id does not exist` while sending was perfectly
	// healthy. A reporting screen must degrade to "no attribution yet", never to
	// an error page.
	attrMu      sync.Mutex
	attrPresent bool
	attrProbed  time.Time
}

// attrProbeTTL re-checks for the columns periodically so the screen lights up on
// its own once the DDL finally lands — no redeploy.
const attrProbeTTL = 2 * time.Minute

// NewClickFunnelsService wires the service.
func NewClickFunnelsService(db *sql.DB) *ClickFunnelsService {
	return &ClickFunnelsService{db: db}
}

// RegisterRoutes mounts the service under the authenticated /api/mailing prefix.
func (s *ClickFunnelsService) RegisterRoutes(r chi.Router) {
	r.Route("/click-funnels", func(r chi.Router) {
		// SNAPSHOT-BACKED READS (2026-08-25). Neither handler touches Postgres
		// or Athena; both serve worker.ClickFunnelSnapshotWorker's output.
		// Static segments are matched before the {offerID} wildcard by chi, so
		// /catalog and /upload/* are unambiguous.
		r.Get("/catalog", s.HandleFunnelCatalog)
		r.Post("/upload/preview", s.HandleUploadPreview)
		r.Post("/upload/enroll", s.HandleUploadEnroll)
		r.Get("/{offerID}", s.HandleFunnelLane)
		// HubSpot's "Matching enrollments": from any step, see the actual
		// records that reached it. Without this the funnel is a wall of
		// aggregates you cannot audit or act on.
		r.Get("/{offerID}/nodes/{nodeID}/enrollments", s.HandleNodeEnrollments)
	})
}

// ---------------------------------------------------------------- lane list

// ClickFunnelLane is one offer lane in the list view.
type ClickFunnelLane struct {
	OfferID        string `json:"offer_id"`
	OfferName      string `json:"offer_name"`
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
	// BodyHTML is the per-touch body override. Empty means the touch still
	// inherits whatever creative the subscriber originally clicked — which is
	// how an August body ended up under a June subject on offer 420.
	BodyHTML      string `json:"body_html"`
	BodyInherited bool   `json:"body_inherited"`
	// The Creative Studio creative this touch points at, with the registry
	// metadata that decides whether it is safe to send: approval state and the
	// money-link check. A touch pointing at an unapproved creative, or one whose
	// money links are dead, is exactly what an operator needs to see BEFORE it
	// mails — and none of that exists for a pasted HTML blob.
	ProofID       string `json:"proof_id"`
	ProofName     string `json:"proof_name"`
	ProofOfferKey string `json:"proof_offer_key"`
	ProofApproval string `json:"proof_approval_status"`
	ProofActive   bool   `json:"proof_active"`
	// ProofSendable is the gate the sender actually applies: only an APPROVED
	// and ACTIVE proof may mail. Surfaced so a touch pointing at a withdrawn
	// proof is visible here rather than discovered when it silently stops
	// using it.
	ProofSendable bool `json:"proof_sendable"`
	// CopyUpdatedAt / CopyAgeDays surface staleness. Offer 420 shipped copy with
	// a hard "Ends 7/5" deadline for four weeks after it expired because nothing
	// showed how old the words were.
	CopyUpdatedAt string `json:"copy_updated_at"`
	CopyAgeDays   int    `json:"copy_age_days"`
	// Versions is the creative history: the live version first, then superseded
	// ones whose metrics are frozen sunset aggregates.
	Versions []TouchVersion `json:"versions"`

	// Flow
	Reached  int `json:"reached"`
	Awaiting int `json:"awaiting"`
	Errors   int `json:"errors"`

	// Engagement (per-node, from this node's own shadow campaign)
	Sent        int `json:"sent"`
	Delivered   int `json:"delivered"`
	Opens       int `json:"opens"`
	Clicks      int `json:"clicks"`
	HumanClicks int `json:"human_clicks"`
	Relayed     int `json:"relayed"`
	Deferred    int `json:"deferred"`
	Unsubs      int `json:"unsubscribes"`
	HardBounce  int `json:"hard_bounce"`
	SoftBounce  int `json:"soft_bounce"`
	Conversions int `json:"conversions"`

	// Rates, 0-100, computed server-side so every surface agrees.
	OpenRate       float64 `json:"open_rate"`
	ClickRate      float64 `json:"click_rate"`
	HumanClickRate float64 `json:"human_click_rate"`
	ConversionRate float64 `json:"conversion_rate"`
	StepThrough    float64 `json:"step_through_rate"`

	Attributed bool `json:"attributed"`
}

// TouchVersion is one creative+subject combination of a touch, with the
// LIFETIME metrics earned by exactly that combination.
//
// Operator rule (2026-08-02): a touch's numbers belong to the specific creative
// and subject that earned them. Changing any part mints a new version; the
// previous one is superseded and its metrics stop moving — a historical
// aggregate, never blended into the new copy's stats. The split is structural,
// not cosmetic: each version has its own shadow campaign, so engagement lands in
// a different bucket at send time.
type TouchVersion struct {
	ContentHash  string `json:"content_hash"`
	Subject      string `json:"subject"`
	Preheader    string `json:"preheader"`
	FromOverride string `json:"from_name_override"`
	BodyHTML     string `json:"body_html"`
	IsLive       bool   `json:"is_live"`
	FirstSeenAt  string `json:"first_seen_at"`
	LastSeenAt   string `json:"last_seen_at"`
	SupersededAt string `json:"superseded_at"`

	// Lifetime metrics for THIS version only.
	Sent        int     `json:"sent"`
	Delivered   int     `json:"delivered"`
	Opens       int     `json:"opens"`
	Clicks      int     `json:"clicks"`
	HumanClicks int     `json:"human_clicks"`
	OpenRate    float64 `json:"open_rate"`
	ClickRate   float64 `json:"click_rate"`
	Attributed  bool    `json:"attributed"`
}

// ClickFunnelNodesResponse envelopes the node view.
type ClickFunnelNodesResponse struct {
	APIVersion string `json:"api_version"`
	OfferID    string `json:"offer_id"`
	// OfferName is the human label the screen leads with; the id stays in the
	// payload because it is the key every other surface is scoped by.
	OfferName      string            `json:"offer_name"`
	JourneyID      string            `json:"journey_id"`
	Nodes          []ClickFunnelNode `json:"nodes"`
	TotalEnrolled  int               `json:"total_enrolled"`
	TotalActive    int               `json:"total_active"`
	TotalConverted int               `json:"total_converted"`
	// HubSpot parity: outcome split + time-to-goal.
	// TotalCompleted = finished the sequence (terminal goal node).
	// TotalExited    = left early ("exits before completion").
	// MedianHoursToConvert = time-to-goal, median enrolled_at -> converted_at.
	TotalCompleted       int      `json:"total_completed"`
	TotalExited          int      `json:"total_exited"`
	MedianHoursToConvert *float64 `json:"median_hours_to_convert"`
	CompletionRate       float64  `json:"completion_rate"`
	ConversionRate       float64  `json:"conversion_rate"`
	AttributionNote      string   `json:"attribution_note"`
	// EngagementSource is "lake" (Athena, authoritative) or "pg-fallback".
	// Surfaced so the screen never implies lake provenance it does not have.
	EngagementSource string `json:"engagement_source"`
	WindowFrom       string `json:"window_from"`
	WindowTo         string `json:"window_to"`
}

// journeyGraphNode mirrors the persisted node JSON.
type journeyGraphNode struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Name   string `json:"name"`
	Config struct {
		Subject string `json:"subject"`
		// The persisted click-drip graph expresses a wait as
		// {"delayUnit":"hours","delayValue":1} — NOT delay_hours/delay_minutes.
		// Reading only the snake_case pair made every delay node render with no
		// duration (caught by TestFunnelNodes_ReportsPGFallbackProvenance).
		// Both spellings are honoured so a graph authored by JourneyBuilder and
		// one authored by the seed migration both display.
		DelayUnit     string  `json:"delayUnit"`
		DelayValue    float64 `json:"delayValue"`
		DelayHours    float64 `json:"delay_hours"`
		DelayMinutes  float64 `json:"delay_minutes"`
		ReminderIndex *int    `json:"reminder_sequence_index"`
	} `json:"config"`
}

// delayMillis normalizes the two graph spellings into milliseconds.
func (g journeyGraphNode) delayMillis() int64 {
	if g.Config.DelayValue > 0 {
		switch strings.ToLower(g.Config.DelayUnit) {
		case "minutes", "minute", "min":
			return int64(g.Config.DelayValue * 60000)
		case "days", "day":
			return int64(g.Config.DelayValue * 86400000)
		case "seconds", "second", "sec":
			return int64(g.Config.DelayValue * 1000)
		default: // hours is the click-drip default
			return int64(g.Config.DelayValue * 3600000)
		}
	}
	return int64(g.Config.DelayHours*3600000 + g.Config.DelayMinutes*60000)
}

type nodeEngagement struct {
	sent, delivered, opens, clicks, humanClicks, unsubs, hard, soft, deferred int
	// relayed counts 'relayed_to_ses' — click-drip mail handed to SES books
	// delivery under that event, NOT 'delivered'. Excluding it collapsed the
	// rate denominator to ~11% of real volume (193 sends -> 21 "delivered").
	relayed int
}

type reminderCopy struct {
	subject, preheader, fromOverride, bodyHTML       string
	proofID, proofName, proofOfferKey, proofApproval string
	proofActive                                      bool
	enabled                                          bool
	updatedAt                                        time.Time
}

// ------------------------------------------------- matching enrollments

// NodeEnrollment is one record that reached a node.
type NodeEnrollment struct {
	EnrollmentID string  `json:"enrollment_id"`
	Email        string  `json:"email"`
	Status       string  `json:"status"`
	CurrentNode  string  `json:"current_node_id"`
	EnrolledAt   string  `json:"enrolled_at"`
	ExecutedAt   string  `json:"executed_at"`
	Action       string  `json:"action"`
	ErrorMessage string  `json:"error_message"`
	ConvertedAt  *string `json:"converted_at"`
	ExitReason   string  `json:"exit_reason"`
}

// NodeEnrollmentsResponse envelopes the drill-down.
type NodeEnrollmentsResponse struct {
	APIVersion  string           `json:"api_version"`
	OfferID     string           `json:"offer_id"`
	NodeID      string           `json:"node_id"`
	Total       int              `json:"total"`
	Enrollments []NodeEnrollment `json:"enrollments"`
}

// HandleNodeEnrollments serves
//
//	GET /api/mailing/click-funnels/{offerID}/nodes/{nodeID}/enrollments
//
// HubSpot's "Matching enrollments" affordance: the records that actually
// reached this step, so an aggregate can be audited instead of trusted. Also
// accepts ?action=error to list only the touches that FAILED at this node —
// click-drip send errors block and retry rather than advancing, so this is how
// an operator sees who is stuck and why.
func (s *ClickFunnelsService) HandleNodeEnrollments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	offerID := strings.TrimSpace(chi.URLParam(r, "offerID"))
	nodeID := strings.TrimSpace(chi.URLParam(r, "nodeID"))
	if offerID == "" || nodeID == "" {
		respondError(w, http.StatusBadRequest, "offerID and nodeID are required")
		return
	}
	onlyErrors := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("action")), "error")

	limit := 200
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 && parsed <= 1000 {
			limit = parsed
		}
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id,
		       COALESCE(e.subscriber_email,''),
		       COALESCE(e.status,''),
		       COALESCE(e.current_node_id,''),
		       to_char(e.enrolled_at,  'YYYY-MM-DD"T"HH24:MI:SSZ'),
		       to_char(max(l.executed_at), 'YYYY-MM-DD"T"HH24:MI:SSZ'),
		       COALESCE((array_agg(l.action ORDER BY l.executed_at DESC))[1], ''),
		       COALESCE((array_agg(COALESCE(l.error_message,'') ORDER BY l.executed_at DESC))[1], ''),
		       to_char(e.converted_at, 'YYYY-MM-DD"T"HH24:MI:SSZ'),
		       COALESCE(e.exit_reason,'')
		FROM mailing_journey_execution_log l
		JOIN mailing_journey_enrollments e ON e.id = l.enrollment_id
		WHERE e.enrollment_offer_id = $1
		  AND l.node_id = $2
		  AND ($3::bool IS NOT TRUE OR l.action = 'error')
		GROUP BY e.id, e.subscriber_email, e.status, e.current_node_id,
		         e.enrolled_at, e.converted_at, e.exit_reason
		ORDER BY max(l.executed_at) DESC
		LIMIT $4
	`, offerID, nodeID, onlyErrors, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "load node enrollments: "+err.Error())
		return
	}
	defer rows.Close()

	out := make([]NodeEnrollment, 0, limit)
	for rows.Next() {
		var e NodeEnrollment
		var converted sql.NullString
		if err := rows.Scan(&e.EnrollmentID, &e.Email, &e.Status, &e.CurrentNode,
			&e.EnrolledAt, &e.ExecutedAt, &e.Action, &e.ErrorMessage, &converted, &e.ExitReason); err != nil {
			respondError(w, http.StatusInternalServerError, "scan node enrollment: "+err.Error())
			return
		}
		if converted.Valid {
			v := converted.String
			e.ConvertedAt = &v
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, "iterate node enrollments: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, NodeEnrollmentsResponse{
		APIVersion:  VersionClickFunnels,
		OfferID:     offerID,
		NodeID:      nodeID,
		Total:       len(out),
		Enrollments: out,
	})
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
	APIVersion  string `json:"api_version"`
	OfferID     string `json:"offer_id"`
	JourneyID   string `json:"journey_id"`
	LaneEnabled bool   `json:"lane_enabled"`

	Submitted         int `json:"submitted"`
	Malformed         int `json:"malformed"`
	Duplicates        int `json:"duplicates_in_file"`
	Unknown           int `json:"unknown_subscriber"`
	AlreadyActive     int `json:"already_active"`
	AlreadyConverted  int `json:"already_converted"`
	RecentlyTriggered int `json:"recently_triggered"`
	Ready             int `json:"ready"`

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
// clickFunnelWindow resolves the lake dt window. Athena bills per byte scanned
// and email_events is partitioned by dt, so an unbounded window is both slow and
// expensive (measured: a 30-day engagement pass over the click-drip campaigns
// scanned ~3.1 GB). Default 30 days;
// ?from=/?to= narrow it. Values are passed to analytics.Breakdown, which
// validates them against its dt pattern before they reach SQL.
// offerNameSubquery renders a scalar subquery resolving ONE display name for an
// everflow offer id, given the SQL expression holding that id.
//
// Two production realities it has to survive:
//   - DUPLICATES. mailing_offers can hold several rows per everflow id (offer
//     5990 has three: "CarShield Auto Warranty", "CarShield Auto Warranty -
//     IceT2000 (545801)", "QF - Carshield Auto Warranty"). A plain LEFT JOIN
//     would multiply the lane row and show the funnel three times. A scalar
//     subquery returns exactly one, always.
//   - GAPS. 8 of 22 enabled lanes have no mailing_offers row at all, so the
//     result is COALESCEd to "Offer <id>" rather than rendering blank.
//
// Ordering picks the least-decorated name (shortest, alphabetical tie-break),
// which is the canonical offer rather than a per-brand or per-creative variant.
func offerNameSubquery(idExpr string) string {
	return `COALESCE((SELECT o.name FROM mailing_offers o
	                   WHERE o.everflow_offer_id = ` + idExpr + `
	                     AND COALESCE(o.name,'') <> ''
	                   ORDER BY length(o.name), o.name
	                   LIMIT 1), 'Offer ' || ` + idExpr + `)`
}

func clickFunnelWindow(r *http.Request) (string, string) {
	const layout = "2006-01-02"
	to := time.Now().UTC()
	from := to.AddDate(0, 0, -30)
	if v := strings.TrimSpace(r.URL.Query().Get("from")); v != "" {
		if t, err := time.Parse(layout, v); err == nil {
			from = t
		}
	}
	if v := strings.TrimSpace(r.URL.Query().Get("to")); v != "" {
		if t, err := time.Parse(layout, v); err == nil {
			to = t
		}
	}
	if to.Before(from) {
		from, to = to, from
	}
	return from.Format(layout), to.Format(layout)
}

func clickFunnelOrgFilter(s string) interface{} {
	if _, err := uuid.Parse(s); err != nil {
		return nil
	}
	return s
}
