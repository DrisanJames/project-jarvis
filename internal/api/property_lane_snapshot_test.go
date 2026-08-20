package api

// Lane-snapshot endpoint fixtures. The permanent one:
//
//	NO SNAPSHOT ⇒ an EXPLICIT state with an empty captured_at and a NULL row
//	set. Never `"rows": []`, which every screen renders as "we measured today
//	and there was zero activity". Those are different facts and the payload
//	must keep them different.
//
// Plus: org scoping, brand filtering, and per-source totals that never fold a
// no-engagement transport's absence into a zero.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/worker"
)

func testSnapshot() *worker.LaneSnapshot {
	return &worker.LaneSnapshot{
		Day:        "2026-08-19",
		CapturedAt: "2026-08-19T18:00:00Z",
		Source:     "athena_lake_snapshot",
		Notes:      []string{"note"},
		Unmapped:   worker.LaneSnapshotUnmapped{Campaigns: 2, Events: 5925},
		Rows: []worker.LaneSnapshotRow{
			{OrganizationID: "org-1", Vertical: "internal_auto_insurance", Brand: "yi", ISP: "gmail", Source: "ses",
				Attempted: 140, Delivered: 125, Bounced: 3,
				OpenUniq: i64p(27), ClickUniq: i64p(4), OpenEvents: i64p(39), ClickEvents: i64p(5),
				EngagementAvailable: true, Campaigns: 2},
			{OrganizationID: "org-1", Vertical: "internal_auto_insurance", Brand: "yi", ISP: "gmail", Source: "app",
				Attempted: 0, Delivered: 125, EngagementAvailable: true,
				OpenUniq: i64p(0), ClickUniq: i64p(0), OpenEvents: i64p(0), ClickEvents: i64p(0), Campaigns: 2},
			{OrganizationID: "org-1", Vertical: "internal_auto_insurance", Brand: "cp", ISP: "yahoo", Source: "kumo",
				Delivered: 500, Bounced: 12, EngagementAvailable: false, Campaigns: 1},
			{OrganizationID: "org-1", Vertical: "term_life", Brand: "rru", ISP: "gmail", Source: "ses",
				Delivered: 7, EngagementAvailable: true,
				OpenUniq: i64p(1), ClickUniq: i64p(0), OpenEvents: i64p(1), ClickEvents: i64p(0), Campaigns: 1},
			// Another tenant's row — must never reach an org-1 payload.
			{OrganizationID: "org-2", Vertical: "internal_auto_insurance", Brand: "yi", ISP: "gmail", Source: "ses",
				Delivered: 999999, EngagementAvailable: true,
				OpenUniq: i64p(1), ClickUniq: i64p(1), OpenEvents: i64p(1), ClickEvents: i64p(1)},
		},
	}
}

// ── 5. the no-snapshot state ────────────────────────────────────────────────

func TestLaneSnapshotNoSnapshotIsExplicitNotEmptyRows(t *testing.T) {
	now := time.Date(2026, 8, 19, 18, 5, 0, 0, time.UTC)
	resp := buildLaneSnapshotResponse(nil, "", "org-1", "internal_auto_insurance", "", now)

	if resp.Available {
		t.Fatal("available must be false when no snapshot exists")
	}
	if resp.State != laneSnapshotStateMissing {
		t.Fatalf("state = %q, want %q", resp.State, laneSnapshotStateMissing)
	}
	if resp.CapturedAt != "" {
		t.Fatalf("captured_at must be empty when nothing was captured, got %q", resp.CapturedAt)
	}
	if resp.AgeSeconds != nil {
		t.Fatalf("age_seconds must be null when nothing was captured, got %v", *resp.AgeSeconds)
	}
	if resp.Rows != nil {
		t.Fatalf("rows must be NULL, not %v — an empty array reads as 'zero activity today'", resp.Rows)
	}
	if resp.Message == "" {
		t.Fatal("the no-snapshot state must explain itself")
	}
	// The day is still named, so the UI can say WHICH day has no snapshot.
	if resp.Day == "" {
		t.Fatal("day must be reported even with no snapshot")
	}

	// And it must SERIALIZE as null, which is what the screen actually reads.
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"rows":null`) {
		t.Fatalf(`payload must carry "rows":null; got %s`, b)
	}
	if strings.Contains(string(b), `"rows":[]`) {
		t.Fatalf(`payload must NEVER carry "rows":[] in the no-snapshot state; got %s`, b)
	}
}

// A lane with a real snapshot but genuinely no activity is a DIFFERENT fact and
// gets an empty array plus available:true.
func TestLaneSnapshotEmptyLaneIsDistinctFromNoSnapshot(t *testing.T) {
	now := time.Date(2026, 8, 19, 18, 5, 0, 0, time.UTC)
	resp := buildLaneSnapshotResponse(testSnapshot(), "memory", "org-1", "solar", "", now)

	if !resp.Available || resp.State != laneSnapshotStateReady {
		t.Fatalf("an existing snapshot must report available/ready: %+v", resp)
	}
	if resp.Rows == nil {
		t.Fatal("a measured-but-empty lane must be [] (measured, no activity), not null (not measured)")
	}
	if len(resp.Rows) != 0 {
		t.Fatalf("rows = %v", resp.Rows)
	}
	if resp.CapturedAt == "" {
		t.Fatal("captured_at must be populated when a snapshot exists")
	}
}

// ── org scoping + filtering ─────────────────────────────────────────────────

func TestLaneSnapshotIsOrgScopedAndLaneFiltered(t *testing.T) {
	now := time.Date(2026, 8, 19, 18, 5, 0, 0, time.UTC)
	resp := buildLaneSnapshotResponse(testSnapshot(), "memory", "org-1", "internal_auto_insurance", "", now)

	if len(resp.Rows) != 3 {
		t.Fatalf("expected the 3 org-1 auto rows, got %d: %+v", len(resp.Rows), resp.Rows)
	}
	for _, r := range resp.Rows {
		if r.OrganizationID != "org-1" {
			t.Fatalf("cross-tenant row leaked: %+v", r)
		}
		if r.Vertical != "internal_auto_insurance" {
			t.Fatalf("wrong vertical leaked: %+v", r)
		}
	}

	// captured_at drives the UI's staleness display; the server computes the
	// age but takes no staleness VERDICT (no stale-serve logic here).
	if resp.AgeSeconds == nil || *resp.AgeSeconds != 300 {
		t.Fatalf("age_seconds = %v, want 300", resp.AgeSeconds)
	}
	if resp.Storage != "memory" {
		t.Fatalf("storage = %q", resp.Storage)
	}
	if resp.Unmapped.Campaigns != 2 || resp.Unmapped.Events != 5925 {
		t.Fatalf("unmapped must survive into the payload: %+v", resp.Unmapped)
	}

	// brand filter narrows further.
	branded := buildLaneSnapshotResponse(testSnapshot(), "memory", "org-1", "internal_auto_insurance", "yi", now)
	if len(branded.Rows) != 2 {
		t.Fatalf("brand=yi should yield 2 rows, got %d: %+v", len(branded.Rows), branded.Rows)
	}
	for _, r := range branded.Rows {
		if r.Brand != "yi" {
			t.Fatalf("brand filter leaked %+v", r)
		}
	}
}

// ── per-source totals ───────────────────────────────────────────────────────

func TestLaneSnapshotSourceTotalsKeepSourcesSeparate(t *testing.T) {
	now := time.Date(2026, 8, 19, 18, 5, 0, 0, time.UTC)
	resp := buildLaneSnapshotResponse(testSnapshot(), "memory", "org-1", "internal_auto_insurance", "", now)

	got := map[string]laneSnapshotSourceTotal{}
	for _, tt := range resp.SourceTotals {
		got[tt.Source] = tt
	}
	if len(got) != 3 {
		t.Fatalf("expected ses/app/kumo totals, got %+v", resp.SourceTotals)
	}
	// ses and app must NOT be merged — app mirrors ses deliveries and summing
	// them double-counts.
	if got["ses"].Delivered != 125 || got["app"].Delivered != 125 {
		t.Fatalf("ses/app must stay separate: %+v", resp.SourceTotals)
	}
	// kumo's engagement stays ABSENT in the totals, never rolled to 0.
	k := got["kumo"]
	if k.EngagementAvailable {
		t.Fatalf("kumo total must report engagement unavailable: %+v", k)
	}
	if k.OpenUniq != nil || k.ClickUniq != nil {
		t.Fatalf("kumo totals must be null, not zero: %+v", k)
	}
	if k.Delivered != 500 || k.Bounced != 12 {
		t.Fatalf("kumo delivery/bounce must still total: %+v", k)
	}

	b, _ := json.Marshal(k)
	if !strings.Contains(string(b), `"open_uniq":null`) {
		t.Fatalf("kumo total open_uniq must serialize null; got %s", b)
	}
}

// ── handler-level ───────────────────────────────────────────────────────────

func TestHandleLaneSnapshotStatsRejectsMissingVertical(t *testing.T) {
	s := &PMTACampaignService{}
	rec := httptest.NewRecorder()
	s.HandleLaneSnapshotStats(rec, httptest.NewRequest(http.MethodGet, "/property-ledger/snapshot", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleLaneSnapshotStatsServesTheSnapshot(t *testing.T) {
	s := &PMTACampaignService{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/property-ledger/snapshot?vertical=internal_auto_insurance&brand=yi", nil)
	req.Header.Set("X-Organization-ID", "org-1")

	// getOrgID runs the platform's 6-step resolution chain (org_context.go), so
	// the org the handler will scope to is whatever THAT returns for this
	// request — not necessarily the header. Stamp the fixture with it so this
	// test exercises the filter rather than a guess about the chain.
	snap := testSnapshot()
	org := getOrgID(req)
	for i := range snap.Rows {
		if snap.Rows[i].OrganizationID == "org-1" {
			snap.Rows[i].OrganizationID = org
		}
	}
	defer worker.SetLaneSnapshotForTest(snap, nil)()

	s.HandleLaneSnapshotStats(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out laneSnapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v — body %s", err, rec.Body.String())
	}
	if !out.Available || out.Day != "2026-08-19" || out.CapturedAt != "2026-08-19T18:00:00Z" {
		t.Fatalf("payload = %+v", out)
	}
	if len(out.Rows) != 2 {
		t.Fatalf("rows = %+v", out.Rows)
	}
}

func TestHandleLaneSnapshotStatsNoSnapshot(t *testing.T) {
	defer worker.SetLaneSnapshotForTest(nil, nil)()

	s := &PMTACampaignService{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/property-ledger/snapshot?vertical=internal_auto_insurance", nil)
	req.Header.Set("X-Organization-ID", "org-1")
	s.HandleLaneSnapshotStats(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"state":"no_snapshot_yet"`) {
		t.Fatalf("body must carry the explicit no-snapshot state: %s", body)
	}
	if !strings.Contains(body, `"rows":null`) {
		t.Fatalf(`body must carry "rows":null: %s`, body)
	}
	if !strings.Contains(body, `"captured_at":""`) {
		t.Fatalf(`body must carry an empty captured_at: %s`, body)
	}
}
