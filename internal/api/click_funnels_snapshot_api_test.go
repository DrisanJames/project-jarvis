package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/worker"
)

// The snapshot-backed read API. Every test here pins a rule from
// docs/METRIC_CONTRACT.md §10 that production got wrong at least once.

func newCFRouter(svc *ClickFunnelsService) chi.Router {
	r := chi.NewRouter()
	svc.RegisterRoutes(r)
	return r
}

// TestCatalog_NoSnapshotIs503NotEmptyList — an operator must be able to tell
// "the worker has not run" from "there are no funnels". An empty list for an
// outage is the silent-zero failure this codebase keeps repeating.
func TestCatalog_NoSnapshotIs503NotEmptyList(t *testing.T) {
	undo := worker.SetClickFunnelSnapshotForTest(nil, nil, nil)
	defer undo()

	rec := httptest.NewRecorder()
	newCFRouter(NewClickFunnelsService(nil)).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/click-funnels/catalog", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when no snapshot exists, got %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"lanes":[]`) {
		t.Fatal("an outage must never be rendered as an empty lane list")
	}
}

// TestCatalog_ServesSnapshotWithFreshness — the payload must carry provenance,
// not just numbers.
func TestCatalog_ServesSnapshotWithFreshness(t *testing.T) {
	gen := time.Now().Add(-9 * time.Minute)
	cat := &worker.ClickFunnelCatalog{
		SnapshotID: "20260825T120000Z-abcd1234", SchemaVersion: worker.ClickFunnelSchemaVersion,
		GeneratedAt: gen, DataQuality: "ok",
		Watermarks: worker.ClickFunnelWatermarks{MetricsThrough: "2026-08-25", MetricsFrom: "2026-08-23"},
		Lanes: []worker.ClickFunnelCatalogRow{{OfferID: "420", OfferName: "Sam's Club", Enabled: true}},
		OrphanInlets: []string{"1054"},
	}
	undo := worker.SetClickFunnelSnapshotForTest(cat, nil, nil)
	defer undo()

	rec := httptest.NewRecorder()
	newCFRouter(NewClickFunnelsService(nil)).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/click-funnels/catalog", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var got cfCatalogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Lanes) != 1 || got.Lanes[0].OfferID != "420" {
		t.Fatalf("lanes wrong: %+v", got.Lanes)
	}
	if got.Snapshot.AgeSeconds < 500 {
		t.Fatalf("age must reflect the snapshot's real age, got %ds", got.Snapshot.AgeSeconds)
	}
	if got.Snapshot.Watermarks.MetricsThrough != "2026-08-25" {
		t.Fatal("watermarks must travel with the payload")
	}
	if len(got.OrphanInlets) != 1 {
		t.Fatal("orphan inlets must survive: an enabled slug with no lane drops every click it receives")
	}
}

// TestLane_WindowAggregationIsFree exercises the core of the redesign: a window
// change must be a re-sum of day-grain rows, never a query. It also pins the
// ACCEPTED rate base (§10.6) — the defect that produced a 22.88% click rate
// over a denominator of 38 "delivered" when 2,894 messages were relayed.
func TestLane_WindowAggregationIsFree(t *testing.T) {
	lane := &worker.ClickFunnelLane{
		SnapshotID: "s1", SchemaVersion: worker.ClickFunnelSchemaVersion,
		GeneratedAt: time.Now(), DataQuality: "ok",
		ClickFunnelCatalogRow: worker.ClickFunnelCatalogRow{OfferID: "420", Enabled: true},
		Nodes: []worker.ClickFunnelNode{
			{NodeID: "delay-1h", Type: "delay", Reached: 129001},
			{NodeID: "email-0", Type: "email", Sequence: 0, Reached: 48262, Attributed: true,
				Days: []worker.ClickFunnelNodeDay{
					{Dt: "2026-08-23", Delivered: 20, Relayed: 1400, Opens: 400, ClicksRaw: 300, ClicksClassified: 300, Deferred: 200},
					{Dt: "2026-08-24", Delivered: 18, Relayed: 1494, Opens: 536, ClicksRaw: 371, ClicksClassified: 371, Deferred: 339},
					{Dt: "2026-08-25", Delivered: 99, Relayed: 9999, Opens: 999, ClicksRaw: 999, ClicksClassified: 999},
				}},
		},
	}
	undo := worker.SetClickFunnelSnapshotForTest(
		&worker.ClickFunnelCatalog{SchemaVersion: worker.ClickFunnelSchemaVersion},
		map[string]*worker.ClickFunnelLane{"420": lane}, nil)
	defer undo()

	rec := httptest.NewRecorder()
	newCFRouter(NewClickFunnelsService(nil)).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/click-funnels/420?from=2026-08-23&to=2026-08-24", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var got cfLaneResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var email0 *cfNodeView
	for i := range got.Nodes {
		if got.Nodes[i].NodeID == "email-0" {
			email0 = &got.Nodes[i]
		}
	}
	if email0 == nil {
		t.Fatal("email-0 missing")
	}
	m := email0.Metrics

	// The 08-25 row is OUTSIDE the window and must not leak in.
	if m.Delivered != 38 || m.Relayed != 2894 {
		t.Fatalf("window filter failed: delivered=%d relayed=%d (want 38/2894)", m.Delivered, m.Relayed)
	}
	if m.Accepted != 2932 {
		t.Fatalf("accepted = %d, want 2932 — the rate base is delivered+relayed, not delivered", m.Accepted)
	}
	if m.Opens != 936 || m.ClicksRaw != 671 {
		t.Fatalf("engagement aggregation wrong: opens=%d clicks=%d", m.Opens, m.ClicksRaw)
	}
	if m.Deferred != 539 {
		t.Fatalf("deferred = %d, want 539", m.Deferred)
	}
	// 936/2932 = 31.9236% ; 671/2932 = 22.8854%. The old truncating path gave
	// 22.88 for the second.
	if m.OpenRate != 31.92 || m.ClickRate != 22.89 {
		t.Fatalf("rates wrong: open=%v click=%v (want 31.92 / 22.89)", m.OpenRate, m.ClickRate)
	}
	if !strings.Contains(m.RateBaseLabel, "accepted") {
		t.Fatalf("the denominator must be labelled accepted, got %q", m.RateBaseLabel)
	}
	if m.ClassificationUsable {
		t.Fatal("with zero machine clicks the verdict is INERT — classification must not be reported usable")
	}
	// Step-through denominator is the first node that logs execution, not
	// total_enrolled (§10.8).
	if email0.StepThroughOf != 129001 || email0.StepThroughRate != 37.41 {
		t.Fatalf("step-through wrong: %v of %d", email0.StepThroughRate, email0.StepThroughOf)
	}
	if !strings.Contains(email0.StepThroughLabel, "entered the ladder") {
		t.Fatalf("step-through must name its denominator, got %q", email0.StepThroughLabel)
	}
	// Day rows must not be shipped raw — that would rebuild the payload problem.
	if len(email0.Days) != 0 {
		t.Fatalf("day rows must be aggregated away, got %d", len(email0.Days))
	}
}

// TestLane_ConversionSuppressedWhenNotMeasurable pins §10.5: a per-touch
// conversion rate of 0.00% where nothing is attributable reads as "this touch
// converts nobody", which is a different claim from "we cannot attribute".
func TestLane_ConversionSuppressedWhenNotMeasurable(t *testing.T) {
	lane := &worker.ClickFunnelLane{
		SchemaVersion: worker.ClickFunnelSchemaVersion, GeneratedAt: time.Now(),
		ClickFunnelCatalogRow: worker.ClickFunnelCatalogRow{OfferID: "420"},
		Nodes: []worker.ClickFunnelNode{
			{NodeID: "email-3", Type: "email", Sequence: 3, Reached: 4330, Conversions: 0},
			{NodeID: "email-0", Type: "email", Sequence: 0, Reached: 48262, Conversions: 9},
		},
	}
	undo := worker.SetClickFunnelSnapshotForTest(
		&worker.ClickFunnelCatalog{SchemaVersion: worker.ClickFunnelSchemaVersion},
		map[string]*worker.ClickFunnelLane{"420": lane}, nil)
	defer undo()

	rec := httptest.NewRecorder()
	newCFRouter(NewClickFunnelsService(nil)).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/click-funnels/420", nil))
	var got cfLaneResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)

	for _, n := range got.Nodes {
		switch n.NodeID {
		case "email-3":
			if n.ConversionMeasurable {
				t.Fatal("email-3 has no attributable conversions — must not be reported as measurable")
			}
		case "email-0":
			if !n.ConversionMeasurable {
				t.Fatal("email-0 has 9 attributed conversions and must be measurable")
			}
			// 9/48262 = 0.018648% -> 0.02 (the truncating path gave 0.01)
			if n.ConversionRate != 0.02 {
				t.Fatalf("conversion rate = %v, want 0.02", n.ConversionRate)
			}
		}
	}
}

// TestLane_StuckRetryRatioSurfaces — 26,908 attempts from 4 enrollments is an
// operational alert, not a metric (§10.9).
func TestLane_StuckRetryRatioSurfaces(t *testing.T) {
	lane := &worker.ClickFunnelLane{
		SchemaVersion: worker.ClickFunnelSchemaVersion, GeneratedAt: time.Now(),
		ClickFunnelCatalogRow: worker.ClickFunnelCatalogRow{OfferID: "420"},
		Nodes: []worker.ClickFunnelNode{
			{NodeID: "email-3", Type: "email", Sequence: 3, Reached: 4330,
				ErrorEnrollments: 4, ErrorAttempts: 26908},
		},
	}
	undo := worker.SetClickFunnelSnapshotForTest(
		&worker.ClickFunnelCatalog{SchemaVersion: worker.ClickFunnelSchemaVersion},
		map[string]*worker.ClickFunnelLane{"420": lane}, nil)
	defer undo()

	rec := httptest.NewRecorder()
	newCFRouter(NewClickFunnelsService(nil)).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/click-funnels/420", nil))
	var got cfLaneResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)

	if len(got.Nodes) != 1 {
		t.Fatalf("want 1 node, got %d", len(got.Nodes))
	}
	n := got.Nodes[0]
	if n.ErrorEnrollments != 4 || n.ErrorAttempts != 26908 {
		t.Fatalf("both figures must survive: enrollments=%d attempts=%d", n.ErrorEnrollments, n.ErrorAttempts)
	}
	if n.StuckRetryRatio != 6727 {
		t.Fatalf("stuck ratio = %d, want 6727", n.StuckRetryRatio)
	}
}

// TestLane_UnknownOffer404sWhenSnapshotExists distinguishes a missing lane from
// a missing snapshot.
func TestLane_UnknownOffer404sWhenSnapshotExists(t *testing.T) {
	undo := worker.SetClickFunnelSnapshotForTest(
		&worker.ClickFunnelCatalog{SchemaVersion: worker.ClickFunnelSchemaVersion}, nil, nil)
	defer undo()

	rec := httptest.NewRecorder()
	newCFRouter(NewClickFunnelsService(nil)).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/click-funnels/99999", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestReadPath_TouchesNoDatabase is the acceptance criterion in test form: the
// service is constructed with a NIL *sql.DB, so any request-time Postgres read
// would panic.
func TestReadPath_TouchesNoDatabase(t *testing.T) {
	lane := &worker.ClickFunnelLane{
		SchemaVersion: worker.ClickFunnelSchemaVersion, GeneratedAt: time.Now(),
		ClickFunnelCatalogRow: worker.ClickFunnelCatalogRow{OfferID: "420"},
		Nodes:                 []worker.ClickFunnelNode{{NodeID: "email-0", Type: "email"}},
	}
	undo := worker.SetClickFunnelSnapshotForTest(
		&worker.ClickFunnelCatalog{SchemaVersion: worker.ClickFunnelSchemaVersion,
			Lanes: []worker.ClickFunnelCatalogRow{{OfferID: "420"}}},
		map[string]*worker.ClickFunnelLane{"420": lane}, nil)
	defer undo()

	router := newCFRouter(NewClickFunnelsService(nil)) // nil DB on purpose
	for _, path := range []string{"/click-funnels/catalog", "/click-funnels/420"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: want 200 with a nil DB, got %d (%s)", path, rec.Code, rec.Body.String())
		}
	}
}
