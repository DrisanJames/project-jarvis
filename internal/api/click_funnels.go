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
	"github.com/ignite/sparkpost-monitor/internal/analytics"
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

// hasAttributionCols reports whether the node-attribution columns exist. Cheap
// catalog lookup, cached; on any error it answers false (degrade, never 500).
func (s *ClickFunnelsService) hasAttributionCols(ctx context.Context) bool {
	s.attrMu.Lock()
	defer s.attrMu.Unlock()
	if !s.attrProbed.IsZero() && time.Since(s.attrProbed) < attrProbeTTL {
		return s.attrPresent
	}
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name='mailing_campaigns'
		  AND column_name IN ('journey_key','journey_offer_id')
	`).Scan(&n)
	s.attrProbed = time.Now()
	s.attrPresent = err == nil && n >= 2
	return s.attrPresent
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

// HandleListFunnels serves GET /api/mailing/click-funnels.
//
// Includes `unmapped_slug_offers`: offers that have an ENABLED money-slug inlet
// but no journey-map row. Those clicks reach the trigger queue and are then
// dropped with skip_reason='offer_unmapped_at_processing', which is invisible
// unless something surfaces it — offer 1054 sat in exactly that state.
func (s *ClickFunnelsService) HandleListFunnels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// touches_30d reads journey_offer_id; substitute a constant when the
	// attribution DDL has not landed so the list still renders.
	touchesExpr := `(SELECT COUNT(*) FROM mailing_message_log ml
		          JOIN mailing_campaigns c ON c.id = ml.campaign_id
		         WHERE c.journey_offer_id = m.everflow_offer_id
		           AND ml.sent_at > NOW() - INTERVAL '30 days')`
	if !s.hasAttributionCols(ctx) {
		touchesExpr = `0`
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT m.everflow_offer_id,
		       `+offerNameSubquery("m.everflow_offer_id")+`,
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
		       `+touchesExpr+`                                                              AS touches_30d,
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
			&l.OfferID, &l.OfferName, &l.JourneyID, &l.JourneyName, &l.Enabled, &l.PayoutType,
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

// HandleFunnelNodes serves GET /api/mailing/click-funnels/{offerID}/nodes.
func (s *ClickFunnelsService) HandleFunnelNodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	offerID := strings.TrimSpace(chi.URLParam(r, "offerID"))
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offerID required")
		return
	}

	var journeyID, offerName string
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(click_journey_id,''), `+offerNameSubquery("$1")+
			` FROM mailing_offer_journey_map WHERE everflow_offer_id=$1`,
		offerID).Scan(&journeyID, &offerName); err != nil {
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
	// Lake reads are partition-pruned by dt, so the window bounds both cost and
	// latency. Default 90d; ?from=&to= override for a narrower look.
	fromDt, toDt := clickFunnelWindow(r)
	engagement, engSource, err := s.loadNodeEngagement(ctx, offerID, fromDt, toDt)
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
	// Creative history per node, with each version's LIFETIME metrics. Non-fatal:
	// a lane that has not sent since versioning shipped simply has none.
	versionsByNode, vErr := s.loadTouchVersions(ctx, offerID, fromDt, toDt)
	if vErr != nil {
		versionsByNode = map[string][]TouchVersion{}
	}

	// Lane outcome split, mirroring HubSpot's enrolled / completed / met-goal /
	// lost model so the funnel is readable as an outcome and not just a stack of
	// step counts. `completed` is sequence completion (the terminal goal node,
	// status='converted'); `converted` is the Everflow postback (converted_at) —
	// they differ by ~81x and must never be conflated. `exited` is HubSpot's
	// "exits before completion": left the funnel early (engagement watcher,
	// postback exit, suppression) without finishing.
	var totalEnrolled, totalActive, totalConverted, totalCompleted, totalExited int
	var medianHours sql.NullFloat64
	_ = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE status='active'),
		       COUNT(converted_at),
		       COUNT(*) FILTER (WHERE status IN ('converted','completed')),
		       COUNT(*) FILTER (WHERE status='exited'),
		       percentile_cont(0.5) WITHIN GROUP (
		           ORDER BY EXTRACT(EPOCH FROM (converted_at - enrolled_at)) / 3600.0
		       ) FILTER (WHERE converted_at IS NOT NULL)
		FROM mailing_journey_enrollments WHERE enrollment_offer_id=$1
	`, offerID).Scan(&totalEnrolled, &totalActive, &totalConverted, &totalCompleted, &totalExited, &medianHours)

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
		n.DelayMs = g.delayMillis()

		if g.Type == "email" {
			if g.Config.ReminderIndex != nil {
				n.Sequence = *g.Config.ReminderIndex
			} else {
				n.Sequence = seq
			}
			seq++

			if c, ok := copyBySeq[n.Sequence]; ok {
				n.Subject, n.Preheader, n.FromOverride, n.CopyEnabled = c.subject, c.preheader, c.fromOverride, c.enabled
				n.BodyHTML = c.bodyHTML
				n.ProofID, n.ProofName = c.proofID, c.proofName
				n.ProofOfferKey, n.ProofApproval, n.ProofActive = c.proofOfferKey, c.proofApproval, c.proofActive
				// Mirrors the sender's gate exactly: approved AND active.
				n.ProofSendable = c.proofActive && strings.EqualFold(c.proofApproval, "approved")
				// Inherited only when the touch has neither a proof nor a
				// snapshot — that is when the body is whatever each subscriber
				// happened to click.
				n.BodyInherited = c.proofID == "" && strings.TrimSpace(c.bodyHTML) == ""
				if !c.updatedAt.IsZero() {
					n.CopyUpdatedAt = c.updatedAt.UTC().Format(time.RFC3339)
					n.CopyAgeDays = int(time.Since(c.updatedAt).Hours() / 24)
				}
				n.Versions = versionsByNode[g.ID]
			} else {
				n.BodyInherited = true
				// No row means the sender falls back to the clicked campaign's
				// own subject — worth flagging rather than rendering blank.
				n.CopyMissing = true
			}
			if e, ok := engagement[g.ID]; ok {
				n.Sent, n.Delivered, n.Opens, n.Clicks = e.sent, e.delivered, e.opens, e.clicks
				n.HumanClicks, n.Deferred = e.humanClicks, e.deferred
				n.Relayed = e.relayed
				n.Unsubs, n.HardBounce, n.SoftBounce = e.unsubs, e.hard, e.soft
				n.Attributed = true
				anyAttributed = true
			}
			n.Conversions = conversions[g.ID]

			// Denominator = ACCEPTED volume. 'delivered' alone undercounts any
			// node whose mail was SES-relayed (booked as relayed_to_ses), which
			// is what produced >100% rates. Fall back to sent when the lake has
			// neither.
			base := float64(n.Delivered + n.Relayed)
			if base == 0 {
				base = float64(n.Sent)
			}
			if base > 0 {
				n.OpenRate = clampRate(float64(n.Opens) / base * 100)
				n.ClickRate = clampRate(float64(n.Clicks) / base * 100)
				n.HumanClickRate = clampRate(float64(n.HumanClicks) / base * 100)
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

	var medianPtr *float64
	if medianHours.Valid {
		v := round2(medianHours.Float64)
		medianPtr = &v
	}

	note := "Per-node engagement is attributed through each node's own shadow campaign. " +
		"Touches sent before node-level attribution shipped (2026-08-01) counted toward Reached but " +
		"cannot be split by node, so their opens/clicks are not shown here."
	if !anyAttributed {
		note = "No node-attributed sends yet. Reached counts are live, but per-node opens/clicks stay " +
			"at zero until this lane sends its next touch after the 2026-08-01 attribution change."
	}
	if !s.hasAttributionCols(ctx) {
		note = "Node attribution is not active yet: the journey_key / journey_offer_id columns " +
			"have not landed (their ALTER is waiting on an ACCESS EXCLUSIVE lock on mailing_campaigns). " +
			"Flow counts below are live; per-node opens/clicks appear once the DDL applies. Sending is unaffected."
	}
	switch engSource {
	case "lake":
		note += " Delivery and engagement come from the analytics lake (deduped by event_uid, real " +
			"transports only). Clicks split human vs machine; lake opens carry no machine flag, so " +
			"open counts are raw."
	case "pg-fallback":
		note += " LAKE UNAVAILABLE — these engagement numbers came from Postgres, which is " +
			"PMTA-route-complete and under-reports SES-routed delivery and engagement."
	}

	writeJSON(w, http.StatusOK, ClickFunnelNodesResponse{
		APIVersion:           VersionClickFunnels,
		OfferID:              offerID,
		OfferName:            offerName,
		JourneyID:            journeyID,
		Nodes:                nodes,
		TotalEnrolled:        totalEnrolled,
		TotalActive:          totalActive,
		TotalConverted:       totalConverted,
		TotalCompleted:       totalCompleted,
		TotalExited:          totalExited,
		MedianHoursToConvert: medianPtr,
		CompletionRate:       laneRate(totalCompleted, totalEnrolled),
		ConversionRate:       laneRate(totalConverted, totalEnrolled),
		AttributionNote:      note,
		EngagementSource:     engSource,
		WindowFrom:           fromDt,
		WindowTo:             toDt,
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
	sent, delivered, opens, clicks, humanClicks, unsubs, hard, soft, deferred int
	// relayed counts 'relayed_to_ses' — click-drip mail handed to SES books
	// delivery under that event, NOT 'delivered'. Excluding it collapsed the
	// rate denominator to ~11% of real volume (193 sends -> 21 "delivered").
	relayed int
}

// nodeCampaigns maps each of the lane's per-node shadow campaign ids to its
// node id — the join key that lets the LAKE attribute engagement per node.
// clampRate bounds a percentage at 100. A rate above 100 means numerator and
// denominator came from different scopes — the funnel showed 533% for exactly
// that reason. Clamping keeps the surface honest while the underlying counts
// (opens/clicks/delivered/relayed) stay visible for diagnosis.
func clampRate(v float64) float64 {
	if v > 100 {
		return 100
	}
	return round2(v)
}

func (s *ClickFunnelsService) nodeCampaigns(ctx context.Context, offerID string) (map[string]string, error) {
	if !s.hasAttributionCols(ctx) {
		// No attribution columns yet: no campaign is node-scoped, so there is
		// nothing to attribute. Empty map (not an error) → nodes render with
		// live flow counts and an explicit "not node-attributed" marker.
		return map[string]string{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, journey_node_id
		FROM mailing_campaigns
		WHERE journey_offer_id = $1 AND journey_node_id IS NOT NULL
	`, offerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, node string
		if err := rows.Scan(&id, &node); err != nil {
			return nil, err
		}
		out[id] = node
	}
	return out, rows.Err()
}

// loadNodeEngagement returns per-node engagement from the ANALYTICS LAKE.
//
// The lake is the authoritative recipient-side record: it is route-correct and
// deduped by event_uid, whereas the PG counters this originally read are
// PMTA-route-complete only and miss SES-routed delivery entirely. It reaches the
// lake through internal/analytics.Breakdown — the existing reader — rather than
// a second Athena client, so this inherits every canonical rule already encoded
// there and cannot drift from the other lake surfaces:
//
//   - COUNT(DISTINCT event_uid) — SNS/accounting retries duplicate rows.
//   - source IN (pmta, ses, kumo): the REAL transports. The `app` source emits a
//     duplicate 'delivered' with no distinct delivery key, so including it
//     double-counts delivery; `relayed_to_ses` is a hop, never a delivery.
//     It IS counted toward the rate DENOMINATOR as accepted volume (2026-08-05):
//     SES-relayed click-drip mail books relayed_to_ses (source=pmta) and its
//     final SES delivery is not attributed back to the node campaign, so
//     'delivered' alone read 21 against 193 real sends and rendered >100% rates.
//   - eventTypeExpr normalizes the two bounce spellings (PMTA puts the class in
//     event_type, SES puts it in bounce_cat) — keying off event_type alone makes
//     every SES bounce read as zero.
//   - delivery_delay is deduped by recipient, not per retry (2.6x inflation).
//
// Lake event names differ from PG's ('open'/'click' vs 'opened'/'clicked') —
// mapping them by hand is exactly how a surface silently reports zeros.
//
// is_machine_click splits human clicks from scanner traffic. Opens carry NO
// machine flag in the lake, so opens here are RAW and must be presented as such;
// per platform doctrine a money-link click is the trustworthy signal anyway.
//
// Returns the engagement map plus the source label actually used, so the screen
// can state its provenance instead of implying lake numbers when it fell back.
func (s *ClickFunnelsService) loadNodeEngagement(ctx context.Context, offerID, fromDt, toDt string) (map[string]nodeEngagement, string, error) {
	byCampaign, err := s.nodeCampaigns(ctx, offerID)
	if err != nil {
		return nil, "", err
	}
	out := map[string]nodeEngagement{}
	if len(byCampaign) == 0 {
		return out, "lake", nil
	}

	if !analytics.ReaderEnabled() {
		// Reader not initialized (no Athena config in this environment). Fall
		// back to PG rather than render an empty funnel, and SAY so.
		pg, perr := s.loadNodeEngagementPG(ctx, offerID)
		return pg, "pg-fallback", perr
	}

	ids := make([]string, 0, len(byCampaign))
	for id := range byCampaign {
		ids = append(ids, id)
	}

	// TWO passes, because delivery and engagement live on DIFFERENT lake sources
	// and a single filter cannot serve both (verified 2026-08-01 against prod):
	//
	//   * Delivery / bounce / deferral ride the real transports (pmta/ses/kumo).
	//     The source filter is mandatory there — the `app` source emits a
	//     duplicate 'delivered' with no distinct key and double-counts delivery.
	//   * open / click exist ONLY on source='app' (the pixel/redirect layer).
	//     Applying the transport filter to them returns ZERO rows — a 30-day
	//     probe over the click-drip campaigns returned 7,102 clicks and 2,350
	//     opens with no source filter, and none at all with it.
	//
	// srcAllow rejects "app", so the engagement pass omits SourceIn entirely and
	// selects on event_type instead; open/click occur on no other source.
	delivery, err := analytics.Breakdown(ctx, analytics.BreakdownFilter{
		From:              fromDt,
		To:                toDt,
		GroupBy:           []string{"campaign_id", "event_type"},
		SourceIn:          []string{"pmta", "ses", "kumo"},
		CampaignIDs:       ids,
		DedupDelayByEmail: true,
		Limit:             5000,
	})
	if err != nil {
		pg, perr := s.loadNodeEngagementPG(ctx, offerID)
		if perr != nil {
			return nil, "", fmt.Errorf("lake delivery breakdown failed (%v) and PG fallback failed: %w", err, perr)
		}
		return pg, "pg-fallback", nil
	}

	engagement, err := analytics.Breakdown(ctx, analytics.BreakdownFilter{
		From:    fromDt,
		To:      toDt,
		GroupBy: []string{"campaign_id", "event_type", "is_machine_click"},
		// UNIQUE opens/clicks (2026-08-05). Raw event counts are per-fetch:
		// one node showed 244 click events from 27 recipients and rendered a
		// 533% click rate. Keyed on subscriber_id — email is NULL on ~87% of
		// app-source engagement rows.
		DedupEngagementByRecipient: true,
		CampaignIDs:                ids,
		Limit:                      5000,
	})
	if err != nil {
		pg, perr := s.loadNodeEngagementPG(ctx, offerID)
		if perr != nil {
			return nil, "", fmt.Errorf("lake engagement breakdown failed (%v) and PG fallback failed: %w", err, perr)
		}
		return pg, "pg-fallback", nil
	}

	for _, r := range delivery {
		node, ok := byCampaign[r.Keys["campaign_id"]]
		if !ok {
			continue
		}
		e := out[node]
		c := int(r.Count)
		switch r.Keys["event_type"] {
		case "delivered":
			e.delivered += c
		case "hard_bounce":
			e.hard += c
		case "soft_bounce":
			e.soft += c
		case "delivery_delay":
			e.deferred += c
		case "relayed_to_ses":
			e.relayed += c
		}
		out[node] = e
	}

	for _, r := range engagement {
		node, ok := byCampaign[r.Keys["campaign_id"]]
		if !ok {
			continue
		}
		e := out[node]
		c := int(r.Count)
		switch r.Keys["event_type"] {
		case "open":
			e.opens += c
		case "click":
			e.clicks += c
			// is_machine_click is a real boolean column; only an explicit
			// "false" is a human click. Absent/unknown is NOT counted human —
			// absence of a machine signal is not evidence of humanity.
			if strings.EqualFold(r.Keys["is_machine_click"], "false") {
				e.humanClicks += c
			}
		case "unsubscribe":
			e.unsubs += c
		}
		out[node] = e
	}

	// "Sent" is not a lake event — attempted volume is delivered + bounced +
	// deferred, and the honest denominator for rates is delivered.
	for node, e := range out {
		e.sent = e.delivered + e.hard + e.soft
		out[node] = e
	}
	return out, "lake", nil
}

// loadNodeEngagementPG is the degraded path used only when the lake reader is
// unavailable. PG is PMTA-route-complete but misses SES-routed engagement until
// the 10-minute ingester backfills it, so its numbers can run low — the response
// labels the source so a viewer is never told these are lake numbers.
func (s *ClickFunnelsService) loadNodeEngagementPG(ctx context.Context, offerID string) (map[string]nodeEngagement, error) {
	if !s.hasAttributionCols(ctx) {
		return map[string]nodeEngagement{}, nil
	}
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
	subject, preheader, fromOverride, bodyHTML       string
	proofID, proofName, proofOfferKey, proofApproval string
	proofActive                                      bool
	enabled                                          bool
	updatedAt                                        time.Time
}

func (s *ClickFunnelsService) loadReminderCopy(ctx context.Context, offerID string) (map[int]reminderCopy, error) {
	// body_html arrives with a migration that can lag this binary. Reading it
	// unconditionally would 500 the whole node view in that window — the exact
	// schema-coupling that took the screen down on 2026-08-02. Try the full
	// shape, fall back to the pre-migration one.
	rows, err := s.db.QueryContext(ctx, `
		SELECT rs.sequence_index, COALESCE(rs.subject,''), COALESCE(rs.preheader,''),
		       COALESCE(rs.from_name_override,''), COALESCE(rs.enabled,false),
		       COALESCE(rs.body_html,''), COALESCE(rs.updated_at, NOW()),
		       COALESCE(rs.proof_id::text,''),
		       COALESCE(pf.name,''), COALESCE(pf.offer_key,''),
		       COALESCE(pf.approval_status,''), COALESCE(pf.is_active,false)
		FROM mailing_offer_reminder_subjects rs
		LEFT JOIN mailing_offer_proofs pf ON pf.id = rs.proof_id
		WHERE rs.everflow_offer_id = $1
	`, offerID)
	if err != nil && strings.Contains(err.Error(), "does not exist") {
		legacy, lerr := s.db.QueryContext(ctx, `
			SELECT sequence_index, COALESCE(subject,''), COALESCE(preheader,''),
			       COALESCE(from_name_override,''), COALESCE(enabled,false),
			       '' AS body_html, COALESCE(updated_at, NOW()),
			       '' AS proof_id, '' AS proof_name, '' AS proof_offer_key,
			       '' AS proof_approval, false AS proof_active
			FROM mailing_offer_reminder_subjects
			WHERE everflow_offer_id = $1
		`, offerID)
		if lerr != nil {
			return nil, lerr
		}
		rows, err = legacy, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]reminderCopy{}
	for rows.Next() {
		var idx int
		var c reminderCopy
		if err := rows.Scan(&idx, &c.subject, &c.preheader, &c.fromOverride, &c.enabled,
			&c.bodyHTML, &c.updatedAt, &c.proofID, &c.proofName,
			&c.proofOfferKey, &c.proofApproval, &c.proofActive); err != nil {
			return nil, err
		}
		out[idx] = c
	}
	return out, rows.Err()
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

// laneRate is the lane-level percentage helper; a zero denominator yields 0
// rather than NaN (which serializes as invalid JSON and blanks the tile).
func laneRate(num, den int) float64 {
	if den <= 0 {
		return 0
	}
	return round2(float64(num) / float64(den) * 100)
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

// loadTouchVersions returns each node's creative history, newest first, with the
// LIFETIME metrics earned by each individual version.
//
// This is the operator's rule made real: a touch's numbers belong to the exact
// creative + subject that earned them, and changing anything sunsets the old
// numbers rather than blending them into the new copy's stats. The split is
// structural — the sender folds a content hash into the shadow-campaign id, so
// each version's sends land on their own campaign_id — which means the metrics
// here come from the SAME lake path as everything else, keyed per version. A
// superseded version's numbers simply stop moving once nothing sends against it.
//
// Versions predating the 2026-08-02 versioning have no shadow_campaign_id and
// report Attributed=false rather than a misleading zero.
func (s *ClickFunnelsService) loadTouchVersions(ctx context.Context, offerID, fromDt, toDt string) (map[string][]TouchVersion, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT node_id, content_hash,
		       COALESCE(subject,''), COALESCE(preheader,''), COALESCE(from_name_override,''),
		       COALESCE(body_html,''), COALESCE(shadow_campaign_id::text,''),
		       first_seen_at, last_seen_at, superseded_at
		FROM mailing_clickdrip_touch_versions
		WHERE everflow_offer_id = $1
		ORDER BY node_id, (superseded_at IS NOT NULL), last_seen_at DESC
	`, offerID)
	if err != nil {
		// Table absent (pre-migration) is not an error for the screen.
		if strings.Contains(err.Error(), "does not exist") {
			return map[string][]TouchVersion{}, nil
		}
		return nil, err
	}
	defer rows.Close()

	out := map[string][]TouchVersion{}
	campaignToKey := map[string][2]string{} // campaign id -> (node, hash)
	campaignIDs := []string{}
	for rows.Next() {
		var node, campaignID string
		var v TouchVersion
		var first, last time.Time
		var superseded sql.NullTime
		if err := rows.Scan(&node, &v.ContentHash, &v.Subject, &v.Preheader, &v.FromOverride,
			&v.BodyHTML, &campaignID, &first, &last, &superseded); err != nil {
			return nil, err
		}
		v.FirstSeenAt = first.UTC().Format(time.RFC3339)
		v.LastSeenAt = last.UTC().Format(time.RFC3339)
		if superseded.Valid {
			v.SupersededAt = superseded.Time.UTC().Format(time.RFC3339)
		} else {
			v.IsLive = true
		}
		if campaignID != "" {
			campaignToKey[campaignID] = [2]string{node, v.ContentHash}
			campaignIDs = append(campaignIDs, campaignID)
		}
		out[node] = append(out[node], v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(campaignIDs) == 0 || !analytics.ReaderEnabled() {
		return out, nil
	}

	// One lake pass over every version's campaign. Delivery and engagement live
	// on different sources (transport vs app), same as loadNodeEngagement.
	apply := func(brs []analytics.BreakdownRow) {
		for _, r := range brs {
			key, ok := campaignToKey[r.Keys["campaign_id"]]
			if !ok {
				continue
			}
			list := out[key[0]]
			for i := range list {
				if list[i].ContentHash != key[1] {
					continue
				}
				c := int(r.Count)
				switch r.Keys["event_type"] {
				case "delivered":
					list[i].Delivered += c
				case "hard_bounce", "soft_bounce":
					list[i].Sent += c
				case "open":
					list[i].Opens += c
				case "click":
					list[i].Clicks += c
					if strings.EqualFold(r.Keys["is_machine_click"], "false") {
						list[i].HumanClicks += c
					}
				}
				list[i].Attributed = true
			}
			out[key[0]] = list
		}
	}

	if d, err := analytics.Breakdown(ctx, analytics.BreakdownFilter{
		From: fromDt, To: toDt,
		GroupBy: []string{"campaign_id", "event_type"}, SourceIn: []string{"pmta", "ses", "kumo"},
		CampaignIDs: campaignIDs, DedupDelayByEmail: true, Limit: 5000,
	}); err == nil {
		apply(d)
	}
	if e, err := analytics.Breakdown(ctx, analytics.BreakdownFilter{
		From: fromDt, To: toDt,
		GroupBy:     []string{"campaign_id", "event_type", "is_machine_click"},
		CampaignIDs: campaignIDs, Limit: 5000,
	}); err == nil {
		apply(e)
	}

	for node, list := range out {
		for i := range list {
			list[i].Sent += list[i].Delivered
			base := float64(list[i].Delivered)
			if base == 0 {
				base = float64(list[i].Sent)
			}
			if base > 0 {
				list[i].OpenRate = round2(float64(list[i].Opens) / base * 100)
				list[i].ClickRate = round2(float64(list[i].Clicks) / base * 100)
			}
		}
		out[node] = list
	}
	return out, nil
}
