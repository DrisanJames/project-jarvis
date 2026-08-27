package worker

// Click-funnel snapshot PAYLOAD — the read model the Click Funnels screen
// serves from. See docs/METRIC_CONTRACT.md §10, which is binding on every
// field here.
//
// SHAPE: two objects, not one board.
//
//	catalog/current.json      — every lane, small: identity, state, health,
//	                            Studio coverage, alert counts. What the lane
//	                            navigator and filters need, nothing more.
//	lanes/<offer>/current.json — ONE lane: graph, per-touch copy + proof, and
//	                            DAY-GRAIN engagement rows.
//
// Loading one funnel must never require loading every funnel, so the selected
// lane is its own object. Day-grain rows are what make a window change free:
// 7/14/30d is a re-aggregation of cached days, not an Athena round trip.
//
// EVERY payload carries watermarks. A configuration read at 14:00 presented
// beside metrics complete only through 13:30 is a lie unless the gap is stated,
// so freshness travels with the data instead of being inferred from file mtime.

import "time"

// ClickFunnelSchemaVersion is bumped when a consumer-visible field changes
// meaning. The API refuses a snapshot from a FUTURE version rather than
// silently misreading it.
const ClickFunnelSchemaVersion = 1

// ── freshness ───────────────────────────────────────────────────────────────

// ClickFunnelWatermarks records how complete each source is. MetricsThrough is
// the newest lake partition the pass actually read; JourneyThrough is when the
// Postgres side was captured.
//
// LakeLagNote exists because click-drip engagement has a HARD freshness floor:
// open/click ride source='app', whose ingest lag was measured 2026-08-25 at
// 27.7% <1h, 60.8% at 6-24h, 8.6% at 1-2d. No snapshot cadence can beat that,
// and a "live" badge on this screen would be false.
type ClickFunnelWatermarks struct {
	MetricsThrough string    `json:"metrics_through"` // newest dt read from the lake
	MetricsFrom    string    `json:"metrics_from"`    // oldest dt in this pass
	JourneyThrough time.Time `json:"journey_through"` // PG capture time
	LakeRowCount   int       `json:"lake_row_count"`  //
	LakeError      string    `json:"lake_error"`      // non-empty => engagement is STALE, not zero
	LakeLagNote    string    `json:"lake_lag_note"`   //
	Reconciled     bool      `json:"reconciled"`      // true when this pass ran the 7d full window
	ReconciledAt   time.Time `json:"reconciled_at"`   //
}

// ── the catalog (all lanes, small) ──────────────────────────────────────────

// ClickFunnelCatalog is the lane navigator's payload.
type ClickFunnelCatalog struct {
	SnapshotID    string                  `json:"snapshot_id"`
	SchemaVersion int                     `json:"schema_version"`
	GeneratedAt   time.Time               `json:"generated_at"`
	Watermarks    ClickFunnelWatermarks   `json:"watermarks"`
	DataQuality   string                  `json:"data_quality"` // ok | degraded | stale
	Lanes         []ClickFunnelCatalogRow `json:"lanes"`
	OrphanInlets  []string                `json:"unmapped_slug_offers"`
}

// ClickFunnelCatalogRow is one lane, reduced to what a navigator needs. No
// graph, no per-touch rows, no day series.
type ClickFunnelCatalogRow struct {
	OfferID     string `json:"offer_id"`
	OfferName   string `json:"offer_name"`
	JourneyID   string `json:"journey_id"`
	JourneyName string `json:"journey_name"`
	Enabled     bool   `json:"enabled"`
	PayoutType  string `json:"payout_type"`

	RoutingState   string `json:"routing_state"`
	RedirectOffer  string `json:"redirect_offer_id"`
	Recommendation string `json:"routing_recommendation"`
	SlugInlets     int    `json:"slug_inlets"`

	// STATE family — point in time, no window.
	ActiveNow  int `json:"active_now"`
	WaitingNow int `json:"waiting_now"`

	// COHORT family — mature enrollments only, administrative exits excluded.
	MatureEnrolled  int     `json:"mature_enrolled"`
	MatureCompleted int     `json:"mature_completed"`
	CompletionRate  float64 `json:"completion_rate"`

	// Conversions, three figures (METRIC_CONTRACT §10.5). Never one.
	ConversionsPostEnrollment int `json:"conversions_post_enrollment"`
	ConversionsPreTouch       int `json:"conversions_pre_touch"`
	ConversionsDripAttributed int `json:"conversions_drip_attributed"`

	// Creative Studio coverage — the platform invariant, surfaced as a count so
	// a lane mailing un-approved inherited creative cannot hide.
	TouchesEnabled   int `json:"touches_enabled"`
	TouchesWithProof int `json:"touches_with_proof"`
	TouchesSendable  int `json:"touches_sendable"`

	AlertCount int      `json:"alert_count"`
	Alerts     []string `json:"alerts"` // severity-prefixed codes, detail lives on the lane
}

// ── one lane (graph + touches + day series) ─────────────────────────────────

// ClickFunnelLane is the selected-lane payload.
type ClickFunnelLane struct {
	SnapshotID    string                `json:"snapshot_id"`
	SchemaVersion int                   `json:"schema_version"`
	GeneratedAt   time.Time             `json:"generated_at"`
	Watermarks    ClickFunnelWatermarks `json:"watermarks"`
	DataQuality   string                `json:"data_quality"`

	ClickFunnelCatalogRow `json:"lane"`

	LadderHours   float64 `json:"ladder_hours"`   // Σ delay nodes; the maturity threshold
	MaturityHours float64 `json:"maturity_hours"` // ladder + grace

	// COHORT / STATE detail
	TotalEnrolled   int `json:"total_enrolled"`
	InFlight        int `json:"in_flight"` // immature — NOT part of any rate
	ExitsBehavioral int `json:"exits_behavioral"`
	ExitsAdmin      int `json:"exits_administrative"`
	ExitsConverted  int `json:"exits_converted"`

	// Time-to-goal, split three ways (METRIC_CONTRACT §10.5).
	MedianHoursEnrollToConv    *float64 `json:"median_hours_enroll_to_conversion"`
	MedianHoursFirstSendToConv *float64 `json:"median_hours_first_send_to_conversion"`

	// GoalNodeReached is the flow diagnostic that disagrees with
	// MatureCompleted by design; enrollment status is canonical (§10.4).
	GoalNodeReached int `json:"goal_node_reached"`

	Nodes  []ClickFunnelNode  `json:"nodes"`
	Alerts []ClickFunnelAlert `json:"alerts"`
	Notes  []string           `json:"notes"`
}

// ClickFunnelNode is one node of the journey graph with its flow, copy and
// day-grain engagement.
type ClickFunnelNode struct {
	NodeID   string `json:"node_id"`
	Type     string `json:"type"` // trigger | delay | email | goal
	Label    string `json:"label"`
	Sequence int    `json:"sequence_index"` // -1 for non-email
	DelayMs  int64  `json:"delay_ms"`

	// FLOW — ACTIVITY family (lifetime execution log) and STATE (awaiting).
	Reached  int `json:"reached"`
	Awaiting int `json:"awaiting"`

	// ERRORS — enrollments is primary, attempts secondary (§10.9).
	ErrorEnrollments int `json:"error_enrollments"`
	ErrorAttempts    int `json:"error_attempts"`

	// COPY / CREATIVE STUDIO
	Subject       string `json:"subject"`
	Preheader     string `json:"preheader"`
	FromOverride  string `json:"from_name_override"`
	CopyEnabled   bool   `json:"copy_enabled"`
	CopyMissing   bool   `json:"copy_missing"`
	CopyUpdatedAt string `json:"copy_updated_at"`

	ProofID       string `json:"proof_id"`
	ProofName     string `json:"proof_name"`
	ProofApproval string `json:"proof_approval"`
	ProofActive   bool   `json:"proof_active"`
	// ProofSendable mirrors the sender's gate EXACTLY (approved AND active) —
	// journey_executor.go refuses anything else and falls through, so a touch
	// that is not sendable is mailing inherited creative, not this proof.
	ProofSendable bool `json:"proof_sendable"`
	BodyInherited bool `json:"body_inherited"`

	// ATTRIBUTION — a node with no shadow campaign cannot be measured at all.
	ShadowCampaignID string `json:"shadow_campaign_id"`
	Attributed       bool   `json:"attributed"`

	// CONVERSIONS — drip-attributed, last touch within the declared lookback.
	Conversions             int     `json:"conversions"`
	ConversionLookbackHours float64 `json:"conversion_lookback_hours"`

	// DAY-GRAIN engagement. The API aggregates these for whatever window the
	// operator picked; no Athena on the request path, ever.
	Days []ClickFunnelNodeDay `json:"days"`
}

// ClickFunnelNodeDay is one UTC lake day for one node.
//
// Accepted = Delivered + Relayed (METRIC_CONTRACT §10.6). Click-drip mail is
// handed to SES and books relayed_to_ses, not delivered; using delivered alone
// collapses the base to ~1% of real volume. Accepted is NOT inbox placement and
// must be labelled as accepted wherever shown.
type ClickFunnelNodeDay struct {
	Dt string `json:"dt"`

	Delivered  int `json:"delivered"`
	Relayed    int `json:"relayed"`
	HardBounce int `json:"hard_bounce"`
	SoftBounce int `json:"soft_bounce"`
	Deferred   int `json:"deferred"` // DISTINCT mailboxes, not events

	Opens int `json:"opens"` // DISTINCT recipients

	// Four click values, never one "human click" (§10.7). is_machine_click is
	// INERT in production, so qualified is meaningless without coverage.
	ClicksRaw        int `json:"clicks_raw"`
	ClicksClassified int `json:"clicks_classified"`
	ClicksMachine    int `json:"clicks_machine"`

	Unsubs     int `json:"unsubs"`
	Complaints int `json:"complaints"`
}

// Accepted is the contract's rate base.
func (d ClickFunnelNodeDay) Accepted() int { return d.Delivered + d.Relayed }

// ClicksQualified is classified AND not machine. Presented ONLY alongside
// coverage — see §10.7.
func (d ClickFunnelNodeDay) ClicksQualified() int {
	q := d.ClicksClassified - d.ClicksMachine
	if q < 0 {
		return 0
	}
	return q
}

// ── alerts ──────────────────────────────────────────────────────────────────

// ClickFunnelAlert is an operational condition, not a metric. These are what
// must reach an operator fast; engagement can lag six hours, a retry loop
// cannot.
type ClickFunnelAlert struct {
	Code     string `json:"code"`
	Severity string `json:"severity"` // critical | warning | info
	NodeID   string `json:"node_id"`
	Message  string `json:"message"`
	Count    int    `json:"count"`
}

// Alert codes. Stable strings — the UI filters on them.
const (
	ClickFunnelAlertStuckRetry      = "stuck_retry"
	ClickFunnelAlertNoProof         = "no_studio_proof"
	ClickFunnelAlertProofUnsendable = "proof_not_sendable"
	ClickFunnelAlertUnattributed    = "node_unattributed"
	ClickFunnelAlertNoInlet         = "no_slug_inlet"
	ClickFunnelAlertAdminExits      = "administrative_exits"
	ClickFunnelAlertCopyMissing     = "copy_missing"
)
