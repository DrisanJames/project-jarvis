package api

// Click Funnels READ API — snapshot-backed, request path free of both stores.
//
// GET /api/mailing/click-funnels/catalog        — every lane, SMALL
// GET /api/mailing/click-funnels/{offerID}      — ONE lane, full detail
//
// WHY TWO OBJECTS: returning every lane's graph, touches and day series in one
// payload is the lane table with the chrome taken off — it still makes the
// whole estate a prerequisite for reading one funnel. The catalog carries only
// what a navigator needs (identity, state, health, Studio coverage, alerts);
// the selected lane is fetched on its own.
//
// NEITHER handler touches Postgres or Athena. Both read the snapshot published
// by worker.ClickFunnelSnapshotWorker (memory, then S3). The windowing that
// used to cost a live Athena pass is a re-aggregation of the snapshot's
// day-grain rows here.
//
// Every metric below is docs/METRIC_CONTRACT.md §10. Rates are computed HERE,
// once, and shipped with their numerator and denominator so the browser never
// re-derives one of them differently.

import (
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/worker"
)

// VersionClickFunnelsSnapshot tracks this endpoint's response contract.
const VersionClickFunnelsSnapshot = "2.0"

// clickFunnelMaxWindowDays bounds the requested window to what the snapshot
// retains.
const clickFunnelMaxWindowDays = 90

// ── response shapes ─────────────────────────────────────────────────────────

type cfSnapshotMeta struct {
	SnapshotID  string                       `json:"snapshot_id"`
	GeneratedAt time.Time                    `json:"generated_at"`
	AgeSeconds  int                          `json:"age_seconds"`
	Storage     string                       `json:"storage"` // memory | s3
	DataQuality string                       `json:"data_quality"`
	Watermarks  worker.ClickFunnelWatermarks `json:"watermarks"`
}

type cfCatalogResponse struct {
	APIVersion   string                         `json:"api_version"`
	Snapshot     cfSnapshotMeta                 `json:"snapshot"`
	Lanes        []worker.ClickFunnelCatalogRow `json:"lanes"`
	OrphanInlets []string                       `json:"unmapped_slug_offers"`
}

// cfNodeMetrics is one node's engagement over the requested window.
//
// Counts AND rates ship together on purpose: a rate with no numerator and
// denominator is unsafe on this screen — "22.88%" hid the fact that the base
// was 2,894 messages ACCEPTED by SES, not delivered to anyone.
type cfNodeMetrics struct {
	HasData bool `json:"has_data"`

	Delivered  int `json:"delivered"`
	Relayed    int `json:"relayed"`
	Accepted   int `json:"accepted"`
	HardBounce int `json:"hard_bounce"`
	SoftBounce int `json:"soft_bounce"`
	Deferred   int `json:"deferred"`

	Opens int `json:"opens"`

	ClicksRaw        int `json:"clicks_raw"`
	ClicksClassified int `json:"clicks_classified"`
	ClicksQualified  int `json:"clicks_qualified"`
	ClicksMachine    int `json:"clicks_machine"`

	Unsubs     int `json:"unsubs"`
	Complaints int `json:"complaints"`

	// Rates, all over ACCEPTED (METRIC_CONTRACT §10.6). RateBaseLabel names the
	// denominator so the UI cannot imply inbox placement.
	OpenRate           float64 `json:"open_rate"`
	ClickRate          float64 `json:"click_rate"`
	QualifiedClickRate float64 `json:"qualified_click_rate"`
	UnsubRate          float64 `json:"unsub_rate"`
	RateBaseLabel      string  `json:"rate_base_label"`

	// ClassificationCoverage is what makes qualified clicks readable at all:
	// is_machine_click is INERT in production, so coverage is usually 100% with
	// zero machine clicks and "qualified" means nothing yet (§10.7).
	ClassificationCoverage float64 `json:"classification_coverage"`
	ClassificationUsable   bool    `json:"classification_usable"`
}

type cfNodeView struct {
	worker.ClickFunnelNode

	// Step-through with its parts exposed (§10.8).
	StepThroughRate  float64 `json:"step_through_rate"`
	StepThroughOf    int     `json:"step_through_of"`
	StepThroughLabel string  `json:"step_through_label"`

	// Per-touch conversion is suppressed unless drip-attributed conversions
	// exist for this node (§10.5) — a 0.00% that is really "not measurable"
	// is worse than an em dash.
	ConversionRate      float64 `json:"conversion_rate"`
	ConversionMeasurable bool   `json:"conversion_measurable"`

	StuckRetryRatio int           `json:"stuck_retry_ratio"`
	Metrics         cfNodeMetrics `json:"metrics"`

	// Days is dropped from the response: the API already aggregated them, and
	// shipping 90 days x N nodes would put the payload back where it started.
	Days []worker.ClickFunnelNodeDay `json:"days,omitempty"`
}

type cfLaneResponse struct {
	APIVersion string         `json:"api_version"`
	Snapshot   cfSnapshotMeta `json:"snapshot"`

	Lane worker.ClickFunnelCatalogRow `json:"lane"`

	WindowFrom string `json:"window_from"`
	WindowTo   string `json:"window_to"`

	LadderHours   float64 `json:"ladder_hours"`
	MaturityHours float64 `json:"maturity_hours"`

	TotalEnrolled   int `json:"total_enrolled"`
	InFlight        int `json:"in_flight"`
	ExitsBehavioral int `json:"exits_behavioral"`
	ExitsAdmin      int `json:"exits_administrative"`
	ExitsConverted  int `json:"exits_converted"`
	GoalNodeReached int `json:"goal_node_reached"`

	MedianHoursEnrollToConv    *float64 `json:"median_hours_enroll_to_conversion"`
	MedianHoursFirstSendToConv *float64 `json:"median_hours_first_send_to_conversion"`

	Nodes  []cfNodeView              `json:"nodes"`
	Alerts []worker.ClickFunnelAlert `json:"alerts"`
	Notes  []string                  `json:"notes"`
}

// ── handlers ────────────────────────────────────────────────────────────────

// HandleFunnelCatalog serves GET /api/mailing/click-funnels/catalog.
func (s *ClickFunnelsService) HandleFunnelCatalog(w http.ResponseWriter, r *http.Request) {
	cat, storage := worker.LoadClickFunnelCatalog(r.Context())
	if cat == nil {
		// NEVER an empty lane list — an operator must be able to tell "no
		// snapshot yet" from "no funnels configured".
		respondError(w, http.StatusServiceUnavailable,
			"no click-funnel snapshot available yet — the snapshot worker has not completed a pass (check CLICK_FUNNEL_SNAPSHOT_DISABLED and the worker heartbeat)")
		return
	}
	writeJSON(w, http.StatusOK, cfCatalogResponse{
		APIVersion:   VersionClickFunnelsSnapshot,
		Snapshot:     cfMeta(cat.SnapshotID, cat.GeneratedAt, storage, cat.DataQuality, cat.Watermarks),
		Lanes:        cat.Lanes,
		OrphanInlets: cat.OrphanInlets,
	})
}

// HandleFunnelLane serves GET /api/mailing/click-funnels/{offerID}.
//
// ?from=YYYY-MM-DD&to=YYYY-MM-DD selects the ACTIVITY window. Changing it costs
// nothing: the snapshot carries day-grain rows and this re-sums them.
func (s *ClickFunnelsService) HandleFunnelLane(w http.ResponseWriter, r *http.Request) {
	offerID := strings.TrimSpace(chi.URLParam(r, "offerID"))
	if offerID == "" {
		respondError(w, http.StatusBadRequest, "offerID required")
		return
	}
	lane, storage := worker.LoadClickFunnelLane(r.Context(), offerID)
	if lane == nil {
		if cat, _ := worker.LoadClickFunnelCatalog(r.Context()); cat == nil {
			respondError(w, http.StatusServiceUnavailable,
				"no click-funnel snapshot available yet — the snapshot worker has not completed a pass")
			return
		}
		respondError(w, http.StatusNotFound, "no funnel configured for offer "+offerID)
		return
	}

	from, to := clickFunnelWindow(r)

	out := cfLaneResponse{
		APIVersion: VersionClickFunnelsSnapshot,
		Snapshot:   cfMeta(lane.SnapshotID, lane.GeneratedAt, storage, lane.DataQuality, lane.Watermarks),
		Lane:       lane.ClickFunnelCatalogRow,
		WindowFrom: from,
		WindowTo:   to,

		LadderHours:   lane.LadderHours,
		MaturityHours: lane.MaturityHours,

		TotalEnrolled:   lane.TotalEnrolled,
		InFlight:        lane.InFlight,
		ExitsBehavioral: lane.ExitsBehavioral,
		ExitsAdmin:      lane.ExitsAdmin,
		ExitsConverted:  lane.ExitsConverted,
		GoalNodeReached: lane.GoalNodeReached,

		MedianHoursEnrollToConv:    lane.MedianHoursEnrollToConv,
		MedianHoursFirstSendToConv: lane.MedianHoursFirstSendToConv,

		Alerts: lane.Alerts,
		Notes:  lane.Notes,
	}

	// Step-through denominator: the first node in the graph that logs any
	// execution — in practice the first DELAY node, NOT total_enrolled
	// (§10.8). Naming it in the response keeps the label honest.
	entry, entryLabel := 0, ""
	for _, n := range lane.Nodes {
		if n.Reached > entry {
			entry, entryLabel = n.Reached, n.NodeID
		}
	}

	out.Nodes = make([]cfNodeView, 0, len(lane.Nodes))
	for _, n := range lane.Nodes {
		v := cfNodeView{ClickFunnelNode: n}
		v.Days = nil // aggregated below; never shipped raw

		if entry > 0 {
			v.StepThroughRate = cfPct(n.Reached, entry)
			v.StepThroughOf = entry
			v.StepThroughLabel = "of " + itoa(entry) + " who entered the ladder at " + entryLabel
		}

		// Per-touch conversion only when there is something to attribute.
		v.ConversionMeasurable = n.Conversions > 0 && n.Reached > 0
		if v.ConversionMeasurable {
			v.ConversionRate = cfPct(n.Conversions, n.Reached)
		}

		if n.ErrorEnrollments > 0 {
			v.StuckRetryRatio = n.ErrorAttempts / n.ErrorEnrollments
		}

		v.Metrics = aggregateNodeWindow(n.Days, from, to)
		out.Nodes = append(out.Nodes, v)
	}

	writeJSON(w, http.StatusOK, out)
}

// ── aggregation ─────────────────────────────────────────────────────────────

// aggregateNodeWindow re-sums day-grain rows into one window. This is the whole
// reason a window change is free.
func aggregateNodeWindow(days []worker.ClickFunnelNodeDay, from, to string) cfNodeMetrics {
	m := cfNodeMetrics{RateBaseLabel: "accepted (delivered + relayed to SES)"}
	for _, d := range days {
		if d.Dt < from || d.Dt > to {
			continue
		}
		m.HasData = true
		m.Delivered += d.Delivered
		m.Relayed += d.Relayed
		m.HardBounce += d.HardBounce
		m.SoftBounce += d.SoftBounce
		m.Deferred += d.Deferred
		m.Opens += d.Opens
		m.ClicksRaw += d.ClicksRaw
		m.ClicksClassified += d.ClicksClassified
		m.ClicksMachine += d.ClicksMachine
		m.Unsubs += d.Unsubs
		m.Complaints += d.Complaints
	}
	m.Accepted = m.Delivered + m.Relayed
	m.ClicksQualified = m.ClicksClassified - m.ClicksMachine
	if m.ClicksQualified < 0 {
		m.ClicksQualified = 0
	}

	m.OpenRate = cfPct(m.Opens, m.Accepted)
	m.ClickRate = cfPct(m.ClicksRaw, m.Accepted)
	m.QualifiedClickRate = cfPct(m.ClicksQualified, m.Accepted)
	m.UnsubRate = cfPct(m.Unsubs, m.Accepted)

	m.ClassificationCoverage = cfPct(m.ClicksClassified, m.ClicksRaw)
	// Coverage alone does not make the verdict usable: production classifies
	// ~100% of clicks as not-machine because the column is INERT (zero `true`
	// rows estate-wide). A qualified figure is only meaningful once SOME click
	// is actually classified as machine.
	m.ClassificationUsable = m.ClicksRaw > 0 && m.ClicksMachine > 0
	return m
}

// cfPct rounds half-up to 2dp and guards the denominator. Deliberately NOT
// round2 (which truncates) — see METRIC_CONTRACT §10.10.
func cfPct(num, den int) float64 {
	if den <= 0 {
		return 0
	}
	return math.Round(float64(num)/float64(den)*100*100) / 100
}

func cfMeta(id string, gen time.Time, storage, quality string, m worker.ClickFunnelWatermarks) cfSnapshotMeta {
	age := 0
	if !gen.IsZero() {
		age = int(time.Since(gen).Seconds())
		if age < 0 {
			age = 0
		}
	}
	return cfSnapshotMeta{
		SnapshotID:  id,
		GeneratedAt: gen,
		AgeSeconds:  age,
		Storage:     storage,
		DataQuality: quality,
		Watermarks:  m,
	}
}
