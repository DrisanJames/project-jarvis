package api

// GET /api/mailing/pmta-campaign/property-ledger/snapshot
//
// Serves the timed Athena→JSON→S3 lane snapshot written by
// worker.LaneSnapshotWorker (internal/worker/lane_snapshot.go). It reads a
// file. That is the entire design.
//
// It is a SEPARATE endpoint from HandleLaneStats (property_lane_stats.go),
// which is untouched: that one is the live Postgres path over
// mailing_tracking_events (PAST-tense event types, PG delivery-ingestion
// counts). This one is the lake path (PRESENT-tense event types, lake delivery
// truth, day-so-far only). The two do NOT arbitrate, cascade, or fall back to
// each other — the previous attempt at that apparatus is what had to be
// switched off (commit fc7fec3).
//
// HONESTY CONTRACT (each of these is asserted by a test):
//   - No snapshot yet ⇒ available:false, state:"no_snapshot_yet", captured_at
//     "", rows NULL. Never an empty array, which a screen would render as
//     "zero activity today".
//   - captured_at + day always ride in the payload so the UI can show how stale
//     the snapshot is. There is no server-side staleness verdict.
//   - Per-row `source` survives untouched. A source that reports no engagement
//     into the lake (kumo) carries null engagement with
//     engagement_available:false — absent, never zero.
//   - Totals are computed per source as well as overall, and the overall totals
//     carry the double-count warning, because source='app' mirrors ses/pmta
//     deliveries.

import (
	"net/http"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/worker"
)

// laneSnapshotStateReady / laneSnapshotStateMissing are the two states the
// endpoint can be in. There is no third.
const (
	laneSnapshotStateReady   = "ready"
	laneSnapshotStateMissing = "no_snapshot_yet"

	laneSnapshotMissingMsg = "No lane snapshot has been captured yet. The snapshot worker runs every 5 minutes; if this persists, check the lane_snapshot heartbeat and that ANALYTICS_ATHENA_OUTPUT and LANE_SNAPSHOT_BUCKET are set."
)

// laneSnapshotSourceTotal is one transport's totals inside a lane selection.
type laneSnapshotSourceTotal struct {
	Source    string `json:"source"`
	Attempted int64  `json:"attempted"`
	Delivered int64  `json:"delivered"`
	Bounced   int64  `json:"bounced"`

	OpenUniq    *int64 `json:"open_uniq"`
	ClickUniq   *int64 `json:"click_uniq"`
	OpenEvents  *int64 `json:"open_events"`
	ClickEvents *int64 `json:"click_events"`

	EngagementAvailable bool `json:"engagement_available"`
}

// laneSnapshotResponse is the wire shape.
type laneSnapshotResponse struct {
	Available bool   `json:"available"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`

	// Day is the Denver calendar day the snapshot covers; CapturedAt is when
	// the worker finished aggregating it (RFC3339 UTC, "" when absent).
	Day        string `json:"day"`
	CapturedAt string `json:"captured_at"`
	AgeSeconds *int64 `json:"age_seconds"`

	Vertical string `json:"vertical"`
	Brand    string `json:"brand,omitempty"`

	// Rows is NULL (not []) when there is no snapshot. A lane that genuinely
	// had no activity in an existing snapshot gets an EMPTY array plus
	// available:true — that is a real "no activity", and the two are
	// deliberately distinguishable.
	Rows []worker.LaneSnapshotRow `json:"rows"`

	SourceTotals []laneSnapshotSourceTotal `json:"source_totals"`

	Unmapped worker.LaneSnapshotUnmapped `json:"unmapped"`

	Source      string   `json:"source"`
	Storage     string   `json:"storage,omitempty"`
	Notes       []string `json:"notes"`
	GeneratedAt string   `json:"generated_at"`
}

// HandleLaneSnapshotStats serves the lane snapshot for one lane.
//
// Query: vertical=<v> (required) · brand=<b> (optional; omitted = every brand
// in the vertical).
func (s *PMTACampaignService) HandleLaneSnapshotStats(w http.ResponseWriter, r *http.Request) {
	// Reuses property_lane_stats.go's slug validator so the two endpoints
	// accept exactly the same lane identifiers (that file is not modified).
	vertical, ok := laneStatsSlug(r.URL.Query().Get("vertical"))
	if !ok {
		respondError(w, http.StatusBadRequest,
			"vertical is required (lowercase [a-z0-9_-], e.g. internal_auto_insurance)")
		return
	}
	brand := ""
	if raw := strings.TrimSpace(r.URL.Query().Get("brand")); raw != "" {
		b, bok := laneStatsSlug(raw)
		if !bok {
			respondError(w, http.StatusBadRequest, "brand must be a lowercase [a-z0-9_-] brand code")
			return
		}
		brand = b
	}
	orgID := getOrgID(r)

	snap, storage := worker.LoadLaneSnapshot(r.Context())
	respondJSON(w, http.StatusOK, buildLaneSnapshotResponse(snap, storage, orgID, vertical, brand, time.Now()))
}

// buildLaneSnapshotResponse is the whole response logic, pure so it is testable
// without a server, AWS, or a database.
func buildLaneSnapshotResponse(snap *worker.LaneSnapshot, storage, orgID, vertical, brand string, now time.Time) laneSnapshotResponse {
	resp := laneSnapshotResponse{
		Vertical:    vertical,
		Brand:       brand,
		Source:      "athena_lake_snapshot",
		GeneratedAt: now.UTC().Format(time.RFC3339),
	}

	// NO SNAPSHOT: an explicit state, an empty captured_at, and a NULL row set.
	// Never [] — an empty array reads as "we looked and there was no activity".
	if snap == nil {
		resp.Available = false
		resp.State = laneSnapshotStateMissing
		resp.Message = laneSnapshotMissingMsg
		resp.Day = worker.LaneSnapshotDenverDay(now)
		resp.CapturedAt = ""
		resp.Rows = nil
		resp.Notes = nil
		return resp
	}

	resp.Available = true
	resp.State = laneSnapshotStateReady
	resp.Day = snap.Day
	resp.CapturedAt = snap.CapturedAt
	resp.Storage = storage
	resp.Notes = snap.Notes
	resp.Unmapped = snap.Unmapped
	if t, err := time.Parse(time.RFC3339, snap.CapturedAt); err == nil {
		age := int64(now.UTC().Sub(t.UTC()).Seconds())
		if age < 0 {
			age = 0
		}
		resp.AgeSeconds = &age
	}

	// Org-scoped filter. Rows carry the org they were computed for; a row from
	// another tenant can never reach this payload.
	rows := make([]worker.LaneSnapshotRow, 0, 16)
	for _, row := range snap.Rows {
		if row.OrganizationID != orgID {
			continue
		}
		if row.Vertical != vertical {
			continue
		}
		if brand != "" && row.Brand != brand {
			continue
		}
		rows = append(rows, row)
	}
	resp.Rows = rows
	resp.SourceTotals = laneSnapshotSourceTotals(rows)
	return resp
}

// laneSnapshotSourceTotals rolls the selection up PER SOURCE.
//
// There is deliberately no single cross-source total: source='app' is the
// PG→lake mirror and double-counts ses/pmta deliveries, so one summed number
// would be wrong by construction. Callers that want a total pick the sources
// they mean.
func laneSnapshotSourceTotals(rows []worker.LaneSnapshotRow) []laneSnapshotSourceTotal {
	order := make([]string, 0, 4)
	byS := make(map[string]*laneSnapshotSourceTotal, 4)
	for _, row := range rows {
		t := byS[row.Source]
		if t == nil {
			t = &laneSnapshotSourceTotal{Source: row.Source}
			byS[row.Source] = t
			order = append(order, row.Source)
		}
		t.Attempted += row.Attempted
		t.Delivered += row.Delivered
		t.Bounced += row.Bounced
		if !row.EngagementAvailable {
			continue
		}
		t.EngagementAvailable = true
		addLaneSnapshotPtr(&t.OpenUniq, row.OpenUniq)
		addLaneSnapshotPtr(&t.ClickUniq, row.ClickUniq)
		addLaneSnapshotPtr(&t.OpenEvents, row.OpenEvents)
		addLaneSnapshotPtr(&t.ClickEvents, row.ClickEvents)
	}
	out := make([]laneSnapshotSourceTotal, 0, len(order))
	for _, s := range order {
		out = append(out, *byS[s])
	}
	return out
}

// addLaneSnapshotPtr accumulates into a nullable total. The total stays nil
// until at least one contributing row actually reported the metric, so an
// all-kumo selection totals to null engagement rather than 0.
func addLaneSnapshotPtr(dst **int64, src *int64) {
	if src == nil {
		return
	}
	if *dst == nil {
		v := *src
		*dst = &v
		return
	}
	**dst += *src
}
