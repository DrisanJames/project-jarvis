package worker

// LaneSnapshotWorker fixtures. Permanent fixtures:
//
//  1. SILENT-ZERO GUARD: the lake query uses PRESENT-tense 'open'/'click' and
//     never the Postgres PAST-tense 'opened'/'clicked'. The wrong convention
//     returns an empty engagement column with no error at all, so this is
//     pinned against the rendered SQL text, not a comment.
//  2. COST GUARD: the query filters on the dt PARTITION column. Dropping it
//     scans the whole table.
//  3. AGGREGATION: campaign→lane mapping and per-(vertical, brand, isp, source)
//     sums.
//  4. UNMAPPED CAMPAIGNS ARE DROPPED AND COUNTED — never attributed to a lane.
//  5. (in internal/api) no snapshot ⇒ explicit state, not empty rows.
//  6. RE-RUN SAFETY: this ticks every 5 minutes and re-fires on every ECS
//     bounce. Two passes must produce ONE object with identical numbers.
//
// No test here touches AWS or a real database: the Athena and S3 calls go
// through the SetLakeReader/SetStore seams, and the PG query through sqlmock.

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ignite/sparkpost-monitor/internal/analytics"
)

// ── 1. the silent-zero guard ────────────────────────────────────────────────

func TestLaneSnapshotSQLUsesPresentTenseEventTypes(t *testing.T) {
	sql := analytics.BuildLaneSnapshotSQL("2026-08-19")

	for _, want := range []string{"'open'", "'click'"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("lake query must use PRESENT-tense %s (the lake's convention); got:\n%s", want, sql)
		}
	}
	// The Postgres spelling would return a silent zero against the lake.
	for _, bad := range []string{"'opened'", "'clicked'"} {
		if strings.Contains(sql, bad) {
			t.Fatalf("lake query must NEVER use PAST-tense %s — mailing_tracking_events uses that, the lake does not, and the mismatch is a SILENT ZERO; got:\n%s", bad, sql)
		}
	}
	// And the exported list itself, which the builder renders from.
	for _, et := range analytics.LaneSnapshotEventTypes {
		if et == "opened" || et == "clicked" {
			t.Fatalf("LaneSnapshotEventTypes must not contain the past-tense %q", et)
		}
	}
	for _, want := range []string{"attempted", "delivered", "open", "click", "bounced"} {
		found := false
		for _, et := range analytics.LaneSnapshotEventTypes {
			if et == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("LaneSnapshotEventTypes is missing %q", want)
		}
	}
}

// ── 2. the cost guard ───────────────────────────────────────────────────────

func TestLaneSnapshotSQLFiltersOnPartitionColumn(t *testing.T) {
	sql := analytics.BuildLaneSnapshotSQL("2026-08-19")

	// dt is the partition column. Without a predicate on it the scan is the
	// whole table.
	if !regexp.MustCompile(`WHERE\s+dt\s+IN\s*\(`).MatchString(sql) {
		t.Fatalf("lake query must filter on the dt PARTITION column or the scan cost explodes; got:\n%s", sql)
	}
	// A Denver day spans exactly two UTC dt partitions: D and D+1.
	if !strings.Contains(sql, "'2026-08-19'") || !strings.Contains(sql, "'2026-08-20'") {
		t.Fatalf("a Denver day spans dt partitions D and D+1; got:\n%s", sql)
	}
	// …and localDtExpr narrows back to the Denver day, so D+1's UTC morning
	// (which is still Denver D) is included and its afternoon is not.
	if !strings.Contains(sql, "America/Denver") {
		t.Fatalf("lake query must narrow the two dt partitions to the Denver day; got:\n%s", sql)
	}
	if !strings.Contains(sql, "GROUP BY campaign_id, isp_group, source, event_type") {
		t.Fatalf("lake query must group by campaign/isp/source/event_type; got:\n%s", sql)
	}
	// The lake has no vertical column — a join attempt here is a bug.
	if strings.Contains(sql, "vertical") {
		t.Fatalf("the lake has NO vertical column; resolution happens in Postgres. got:\n%s", sql)
	}
}

// ── 3. aggregation ──────────────────────────────────────────────────────────

func testLaneMapping() map[string]LaneSnapshotCampaign {
	return map[string]LaneSnapshotCampaign{
		"c-auto-1": {OrganizationID: "org-1", Vertical: "internal_auto_insurance", Brand: "yi"},
		"c-auto-2": {OrganizationID: "org-1", Vertical: "internal_auto_insurance", Brand: "yi"},
		"c-life-1": {OrganizationID: "org-1", Vertical: "term_life", Brand: "rru"},
	}
}

func findRow(t *testing.T, s *LaneSnapshot, vertical, brand, isp, source string) LaneSnapshotRow {
	t.Helper()
	for _, r := range s.Rows {
		if r.Vertical == vertical && r.Brand == brand && r.ISP == isp && r.Source == source {
			return r
		}
	}
	t.Fatalf("no row for (%s, %s, %s, %s) in %+v", vertical, brand, isp, source, s.Rows)
	return LaneSnapshotRow{}
}

func TestBuildLaneSnapshotAggregatesByLaneISPSource(t *testing.T) {
	lake := []analytics.LaneSnapshotLakeRow{
		// Two campaigns, same lane + isp + source: must SUM into one cell.
		{CampaignID: "c-auto-1", ISPGroup: "gmail", Source: "ses", EventType: "attempted", Events: 100},
		{CampaignID: "c-auto-2", ISPGroup: "gmail", Source: "ses", EventType: "attempted", Events: 40},
		{CampaignID: "c-auto-1", ISPGroup: "gmail", Source: "ses", EventType: "delivered", Events: 90},
		{CampaignID: "c-auto-2", ISPGroup: "gmail", Source: "ses", EventType: "delivered", Events: 35},
		{CampaignID: "c-auto-1", ISPGroup: "gmail", Source: "ses", EventType: "open", Events: 30, Uniques: 20},
		{CampaignID: "c-auto-2", ISPGroup: "gmail", Source: "ses", EventType: "open", Events: 9, Uniques: 7},
		{CampaignID: "c-auto-1", ISPGroup: "gmail", Source: "ses", EventType: "click", Events: 5, Uniques: 4},
		{CampaignID: "c-auto-1", ISPGroup: "gmail", Source: "ses", EventType: "bounced", Events: 3},
		// Different ISP — its own cell.
		{CampaignID: "c-auto-1", ISPGroup: "yahoo", Source: "ses", EventType: "delivered", Events: 11},
		// Different source, same lane+isp — MUST NOT be merged into the ses cell.
		{CampaignID: "c-auto-1", ISPGroup: "gmail", Source: "app", EventType: "delivered", Events: 90},
		// Different vertical entirely.
		{CampaignID: "c-life-1", ISPGroup: "gmail", Source: "ses", EventType: "delivered", Events: 7},
	}

	snap := BuildLaneSnapshot("2026-08-19", time.Date(2026, 8, 19, 18, 30, 0, 0, time.UTC), lake, testLaneMapping())

	if snap.Day != "2026-08-19" {
		t.Fatalf("day = %q", snap.Day)
	}
	if snap.CapturedAt != "2026-08-19T18:30:00Z" {
		t.Fatalf("captured_at = %q", snap.CapturedAt)
	}

	gm := findRow(t, snap, "internal_auto_insurance", "yi", "gmail", "ses")
	if gm.Attempted != 140 || gm.Delivered != 125 || gm.Bounced != 3 {
		t.Fatalf("gmail/ses counts wrong: %+v", gm)
	}
	if gm.Campaigns != 2 {
		t.Fatalf("gmail/ses should have folded 2 campaigns, got %d", gm.Campaigns)
	}
	if gm.OpenEvents == nil || *gm.OpenEvents != 39 || gm.OpenUniq == nil || *gm.OpenUniq != 27 {
		t.Fatalf("gmail/ses opens wrong: %+v", gm)
	}
	if gm.ClickEvents == nil || *gm.ClickEvents != 5 || gm.ClickUniq == nil || *gm.ClickUniq != 4 {
		t.Fatalf("gmail/ses clicks wrong: %+v", gm)
	}

	// The 'app' mirror stays its OWN row — never summed into ses, because it
	// double-counts the same deliveries.
	app := findRow(t, snap, "internal_auto_insurance", "yi", "gmail", "app")
	if app.Delivered != 90 {
		t.Fatalf("app row delivered = %d, want 90", app.Delivered)
	}

	yh := findRow(t, snap, "internal_auto_insurance", "yi", "yahoo", "ses")
	if yh.Delivered != 11 {
		t.Fatalf("yahoo delivered = %d", yh.Delivered)
	}
	tl := findRow(t, snap, "term_life", "rru", "gmail", "ses")
	if tl.Delivered != 7 {
		t.Fatalf("term_life delivered = %d", tl.Delivered)
	}

	if snap.MappedCampaigns != 3 {
		t.Fatalf("mapped campaigns = %d, want 3", snap.MappedCampaigns)
	}
	if snap.Unmapped.Campaigns != 0 || snap.Unmapped.Events != 0 {
		t.Fatalf("nothing should be unmapped here: %+v", snap.Unmapped)
	}
}

func TestParseLaneCampaignName(t *testing.T) {
	// The live shape, verified against prod 2026-08-19.
	v, b, ok := ParseLaneCampaignName("[partner-drip] term_life rru 20260820T050519 8deb78a3 95b0af78 [ses:9a66ef56]")
	if !ok || v != "term_life" || b != "rru" {
		t.Fatalf("got (%q,%q,%v)", v, b, ok)
	}
	for _, bad := range []string{
		"",
		"OFR-CLK db 20260819",             // a board campaign, not a drip lane
		"[partner-drip] refi_heloc",       // no brand token
		"[partner-drip]",                  // prefix only
		"partner-drip term_life rru 2026", // missing the bracketed prefix
	} {
		if _, _, ok := ParseLaneCampaignName(bad); ok {
			t.Fatalf("%q must not parse as a lane", bad)
		}
	}
}

// ── 4. unmapped campaigns are dropped AND counted ───────────────────────────

func TestBuildLaneSnapshotDropsAndCountsUnmappedCampaigns(t *testing.T) {
	lake := []analytics.LaneSnapshotLakeRow{
		{CampaignID: "c-auto-1", ISPGroup: "gmail", Source: "ses", EventType: "delivered", Events: 10},
		// A broadcast campaign that is in the lake but not in the drip mapping.
		{CampaignID: "c-broadcast-x", ISPGroup: "gmail", Source: "ses", EventType: "delivered", Events: 5000},
		{CampaignID: "c-broadcast-x", ISPGroup: "gmail", Source: "ses", EventType: "open", Events: 900, Uniques: 700},
		{CampaignID: "c-broadcast-y", ISPGroup: "yahoo", Source: "ses", EventType: "delivered", Events: 25},
	}

	snap := BuildLaneSnapshot("2026-08-19", time.Now(), lake, testLaneMapping())

	if len(snap.Rows) != 1 {
		t.Fatalf("only the mapped campaign may produce a row, got %d: %+v", len(snap.Rows), snap.Rows)
	}
	if snap.Rows[0].Delivered != 10 {
		t.Fatalf("unmapped volume leaked into a lane: %+v", snap.Rows[0])
	}
	// Dropped, but VISIBLE — this is the difference between honest and silent.
	if snap.Unmapped.Campaigns != 2 {
		t.Fatalf("unmapped campaigns = %d, want 2", snap.Unmapped.Campaigns)
	}
	if snap.Unmapped.Events != 5925 {
		t.Fatalf("unmapped events = %d, want 5925", snap.Unmapped.Events)
	}
	// And no bucket named for the absence of a mapping.
	for _, r := range snap.Rows {
		if r.Vertical == "" || r.Vertical == "unknown" || r.Vertical == "other" {
			t.Fatalf("unmapped activity must never become a pseudo-lane: %+v", r)
		}
	}
}

// ── engagement ABSENT vs ZERO (the kumo rule) ───────────────────────────────

func TestBuildLaneSnapshotKumoEngagementIsAbsentNotZero(t *testing.T) {
	mapping := map[string]LaneSnapshotCampaign{
		"c-k": {OrganizationID: "org-1", Vertical: "warmup", Brand: "bcc"},
		"c-s": {OrganizationID: "org-1", Vertical: "warmup", Brand: "bcc"},
	}
	lake := []analytics.LaneSnapshotLakeRow{
		// kumo carries delivery/bounce but NO open/click rows in the lake.
		{CampaignID: "c-k", ISPGroup: "yahoo", Source: "kumo", EventType: "delivered", Events: 500},
		{CampaignID: "c-k", ISPGroup: "yahoo", Source: "kumo", EventType: "bounced", Events: 12},
		// A ses row in the same lane that genuinely saw zero clicks.
		{CampaignID: "c-s", ISPGroup: "yahoo", Source: "ses", EventType: "delivered", Events: 100},
		{CampaignID: "c-s", ISPGroup: "yahoo", Source: "ses", EventType: "open", Events: 4, Uniques: 3},
	}

	snap := BuildLaneSnapshot("2026-08-19", time.Now(), lake, mapping)

	k := findRow(t, snap, "warmup", "bcc", "yahoo", "kumo")
	if k.EngagementAvailable {
		t.Fatal("kumo reports no open/click into the lake — engagement_available must be false")
	}
	if k.OpenUniq != nil || k.ClickUniq != nil || k.OpenEvents != nil || k.ClickEvents != nil {
		t.Fatalf("kumo engagement must be ABSENT (null), not zero: %+v", k)
	}
	if k.Delivered != 500 || k.Bounced != 12 {
		t.Fatalf("kumo delivery/bounce must still be counted: %+v", k)
	}
	// It must marshal to JSON null, not 0 — this is what the screen reads.
	b, err := json.Marshal(k)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"click_uniq":null`) {
		t.Fatalf("kumo click_uniq must serialize as null; got %s", b)
	}

	// The ses row DID report engagement, and a real zero stays a zero.
	s := findRow(t, snap, "warmup", "bcc", "yahoo", "ses")
	if !s.EngagementAvailable {
		t.Fatal("ses reports engagement — engagement_available must be true")
	}
	if s.ClickUniq == nil || *s.ClickUniq != 0 {
		t.Fatalf("a genuine zero must be 0, not null: %+v", s)
	}
}

func TestBuildLaneSnapshotKumoEngagementPopulatesIfObserved(t *testing.T) {
	// If kumo ever does emit engagement into the lake, the observation must win
	// over the doctrine list — this file must not hard-suppress real numbers.
	mapping := map[string]LaneSnapshotCampaign{"c-k": {OrganizationID: "o", Vertical: "warmup", Brand: "bcc"}}
	lake := []analytics.LaneSnapshotLakeRow{
		{CampaignID: "c-k", ISPGroup: "yahoo", Source: "kumo", EventType: "delivered", Events: 5},
		{CampaignID: "c-k", ISPGroup: "yahoo", Source: "kumo", EventType: "click", Events: 2, Uniques: 2},
	}
	snap := BuildLaneSnapshot("2026-08-19", time.Now(), lake, mapping)
	k := findRow(t, snap, "warmup", "bcc", "yahoo", "kumo")
	if !k.EngagementAvailable || k.ClickUniq == nil || *k.ClickUniq != 2 {
		t.Fatalf("observed kumo engagement must be reported: %+v", k)
	}
}

// ── 6. re-run safety ────────────────────────────────────────────────────────

type stubLaneLake struct {
	rows  []analytics.LaneSnapshotLakeRow
	err   error
	calls int
}

func (s *stubLaneLake) LaneSnapshot(ctx context.Context, day string) ([]analytics.LaneSnapshotLakeRow, error) {
	s.calls++
	return s.rows, s.err
}

type stubLaneStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
}

func newStubLaneStore() *stubLaneStore {
	return &stubLaneStore{objects: map[string][]byte{}}
}

func (s *stubLaneStore) Put(ctx context.Context, key string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	cp := append([]byte(nil), body...)
	s.objects[key] = cp
	return nil
}

func (s *stubLaneStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[key]
	if !ok {
		return nil, errors.New("not found")
	}
	return b, nil
}

func newLaneSnapshotWorkerWithMock(t *testing.T) (*LaneSnapshotWorker, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	t.Cleanup(SetLaneSnapshotForTest(nil, nil))
	w := NewLaneSnapshotWorker(db, nil)
	return w, mock
}

// expectLaneSnapshotPass declares one pass's expectations: the ONE campaign
// query, then the heartbeat write.
func expectLaneSnapshotPass(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT id::text, organization_id::text, name\s+FROM mailing_campaigns`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "name"}).
			AddRow("c-auto-1", "org-1", "[partner-drip] internal_auto_insurance yi 20260819T010101 abc").
			AddRow("c-auto-2", "org-1", "[partner-drip] internal_auto_insurance yi 20260819T020202 def"))
	mock.ExpectExec(`(?i)worker_heartbeats`).WillReturnResult(sqlmock.NewResult(1, 1))
}

func TestLaneSnapshotWorkerIsSafeToRunTwice(t *testing.T) {
	w, mock := newLaneSnapshotWorkerWithMock(t)
	lake := &stubLaneLake{rows: []analytics.LaneSnapshotLakeRow{
		{CampaignID: "c-auto-1", ISPGroup: "gmail", Source: "ses", EventType: "delivered", Events: 90},
		{CampaignID: "c-auto-2", ISPGroup: "gmail", Source: "ses", EventType: "delivered", Events: 35},
		{CampaignID: "c-auto-1", ISPGroup: "gmail", Source: "ses", EventType: "click", Events: 5, Uniques: 4},
	}}
	store := newStubLaneStore()
	w.SetLakeReader(lake).SetStore(store)

	expectLaneSnapshotPass(mock)
	expectLaneSnapshotPass(mock)

	w.RunOnce(context.Background())
	first, firstStorage := LoadLaneSnapshot(context.Background())
	if first == nil {
		t.Fatal("first pass produced no snapshot")
	}
	if firstStorage != "memory" {
		t.Fatalf("storage = %q, want memory", firstStorage)
	}
	firstJSON, _ := json.Marshal(first)

	w.RunOnce(context.Background())
	second, _ := LoadLaneSnapshot(context.Background())
	if second == nil {
		t.Fatal("second pass produced no snapshot")
	}

	// ONE object, overwritten — not two, and not appended to.
	if len(store.objects) != 1 {
		t.Fatalf("re-running must overwrite ONE key, got %d objects: %v", len(store.objects), store.objects)
	}
	if store.puts != 2 {
		t.Fatalf("expected 2 puts to the same key, got %d", store.puts)
	}
	wantKey := LaneSnapshotObjectKey("lane-snapshots/", LaneSnapshotDenverDay(time.Now()))
	if _, ok := store.objects[wantKey]; !ok {
		t.Fatalf("snapshot key = %v, want %q", store.objects, wantKey)
	}

	// Numbers must be IDENTICAL, never doubled — the pass carries no
	// accumulating state.
	if len(second.Rows) != len(first.Rows) {
		t.Fatalf("row count changed on re-run: %d → %d", len(first.Rows), len(second.Rows))
	}
	for i := range second.Rows {
		a, b := first.Rows[i], second.Rows[i]
		if a.Delivered != b.Delivered || a.Attempted != b.Attempted || a.Campaigns != b.Campaigns {
			t.Fatalf("re-run changed counts at row %d: %+v vs %+v", i, a, b)
		}
	}
	// captured_at is the only field allowed to move; everything else is stable.
	second.CapturedAt = first.CapturedAt
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("re-run produced a different file:\n%s\n%s", firstJSON, secondJSON)
	}

	if lake.calls != 2 {
		t.Fatalf("expected 2 lake queries, got %d", lake.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet/unexpected DB expectations — the tick must issue exactly ONE query against Postgres: %v", err)
	}
}

// The whole point of the replacement: exactly ONE Postgres query per tick.
// sqlmock fails the pass on any unexpected statement, so declaring only the
// campaign query plus the heartbeat is the assertion.
func TestLaneSnapshotWorkerIssuesOnlyOnePostgresQueryPerTick(t *testing.T) {
	w, mock := newLaneSnapshotWorkerWithMock(t)
	w.SetLakeReader(&stubLaneLake{rows: []analytics.LaneSnapshotLakeRow{
		{CampaignID: "c-auto-1", ISPGroup: "gmail", Source: "ses", EventType: "delivered", Events: 1},
	}}).SetStore(newStubLaneStore())

	expectLaneSnapshotPass(mock)
	w.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the tick must touch Postgres exactly once (PG contention is what killed the design this replaces): %v", err)
	}
}

// A failing Athena query must leave the previous snapshot alone rather than
// publishing an empty one — an empty snapshot reads as "no activity today".
func TestLaneSnapshotWorkerLakeFailureDoesNotPublishEmptySnapshot(t *testing.T) {
	w, mock := newLaneSnapshotWorkerWithMock(t)
	good := &LaneSnapshot{Day: "2026-08-19", CapturedAt: "2026-08-19T18:00:00Z",
		Rows: []LaneSnapshotRow{{Vertical: "internal_auto_insurance", Delivered: 42}}}
	publishLaneSnapshot(good)

	w.SetLakeReader(&stubLaneLake{err: errors.New("athena boom")}).SetStore(newStubLaneStore())
	mock.ExpectExec(`(?i)worker_heartbeats`).WillReturnResult(sqlmock.NewResult(1, 1))

	w.RunOnce(context.Background())

	got, _ := LoadLaneSnapshot(context.Background())
	if got == nil || len(got.Rows) != 1 || got.Rows[0].Delivered != 42 {
		t.Fatalf("a failed lake query must not replace the last good snapshot; got %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a failed lake query must not run the PG query at all: %v", err)
	}
}

// The kill switch must actually fire.
func TestLaneSnapshotWorkerKillSwitch(t *testing.T) {
	w, mock := newLaneSnapshotWorkerWithMock(t)
	w.SetLakeReader(&stubLaneLake{}).SetStore(newStubLaneStore())

	t.Setenv("LANE_SNAPSHOT_DISABLED", "1")
	mock.ExpectExec(`(?i)worker_heartbeats`).WillReturnResult(sqlmock.NewResult(1, 1))

	w.tick(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a disabled tick must emit ONE heartbeat and run nothing else: %v", err)
	}
}

func TestLaneSnapshotWorkerStartNoOpsWithoutDB(t *testing.T) {
	w := NewLaneSnapshotWorker(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx) // must return immediately and start no goroutine loop
}

func TestLaneSnapshotObjectKey(t *testing.T) {
	if got := LaneSnapshotObjectKey("lane-snapshots/", "2026-08-19"); got != "lane-snapshots/2026-08-19.json" {
		t.Fatalf("key = %q", got)
	}
	// A prefix without a trailing slash must not produce a mangled key.
	if got := LaneSnapshotObjectKey("lane-snapshots", "2026-08-19"); got != "lane-snapshots/2026-08-19.json" {
		t.Fatalf("key = %q", got)
	}
	if got := LaneSnapshotObjectKey("", "2026-08-19"); got != "lane-snapshots/2026-08-19.json" {
		t.Fatalf("key = %q", got)
	}
}

// LoadLaneSnapshot must fall back to S3 when the process has no in-memory
// snapshot (a task that booted but has not ticked yet).
func TestLoadLaneSnapshotFallsBackToS3(t *testing.T) {
	store := newStubLaneStore()
	day := LaneSnapshotDenverDay(time.Now())
	body, _ := json.Marshal(&LaneSnapshot{Day: day, CapturedAt: "2026-08-19T18:00:00Z",
		Rows: []LaneSnapshotRow{{Vertical: "term_life", Delivered: 7}}})
	store.objects[LaneSnapshotObjectKey("lane-snapshots/", day)] = body

	defer SetLaneSnapshotForTest(nil, store)()

	got, storage := LoadLaneSnapshot(context.Background())
	if got == nil || storage != "s3" {
		t.Fatalf("expected an s3-sourced snapshot, got %+v / %q", got, storage)
	}
	if len(got.Rows) != 1 || got.Rows[0].Delivered != 7 {
		t.Fatalf("s3 snapshot decoded wrong: %+v", got)
	}
}

// And when neither memory nor S3 has anything, it says so — it does not
// fabricate an empty snapshot.
func TestLoadLaneSnapshotReturnsNilWhenNothingExists(t *testing.T) {
	defer SetLaneSnapshotForTest(nil, newStubLaneStore())()
	got, storage := LoadLaneSnapshot(context.Background())
	if got != nil || storage != "" {
		t.Fatalf("expected (nil, \"\"), got %+v / %q", got, storage)
	}
}
