package consumers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/ignite/sparkpost-monitor/internal/dataingest"
	"github.com/ignite/sparkpost-monitor/internal/eventbus"
)

func newDIConsumer(t *testing.T) (*DataIngestCountersConsumer, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewDataIngestCountersConsumer(rdb, eventbus.Config{}), mr, rdb
}

func diEvent(t *testing.T, ev dataingest.Event) []byte {
	t.Helper()
	ev.Normalize()
	b, err := json.Marshal(&ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// Applying the SAME op_id twice counts ONCE. This is the replay guarantee the
// whole counter design rests on: a consumer-group reset must not double the day.
func TestDataIngestHandler_IdempotentOnReplay(t *testing.T) {
	c, _, rdb := newDIConsumer(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 20, 10, 30, 0, 0, time.UTC)
	ds := uuid.NewString()

	payload := diEvent(t, dataingest.Event{
		OpID: uuid.NewString(), At: at,
		Class: dataingest.ClassDynamic, Source: dataingest.SourcePartnerAPI,
		DatasetID: ds, Lane: "refi_heloc", Transition: dataingest.TransitionLanded,
		ByISP: map[string]int64{"gmail": 70, "microsoft": 30}, N: 100,
	})

	if err := c.Handle(ctx, nil, payload); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	if err := c.Handle(ctx, nil, payload); err != nil {
		t.Fatalf("replay Handle: %v", err)
	}

	applied, dup, failed := c.Stats()
	if applied != 1 || dup != 1 || failed != 0 {
		t.Fatalf("stats: applied=%d duplicates=%d failed=%d; want 1/1/0", applied, dup, failed)
	}

	day := dataingest.DayOf(at)
	r := dataingest.NewReader(rdb)
	got, err := r.GetDay(ctx, day)
	if err != nil {
		t.Fatalf("GetDay: %v", err)
	}
	if n := got[dataingest.ClassDynamic][dataingest.TransitionLanded]["gmail"]; n != 70 {
		t.Fatalf("gmail counted %d, want 70 (replay must not double)", n)
	}
	if n := got[dataingest.ClassDynamic][dataingest.TransitionLanded]["microsoft"]; n != 30 {
		t.Fatalf("microsoft counted %d, want 30", n)
	}

	dsCounts, err := r.GetDataset(ctx, day, ds)
	if err != nil {
		t.Fatalf("GetDataset: %v", err)
	}
	if n := dsCounts[dataingest.TransitionLanded]["gmail"]; n != 70 {
		t.Fatalf("dataset gmail %d, want 70", n)
	}
	last, err := r.LastEvent(ctx, []string{ds})
	if err != nil {
		t.Fatalf("LastEvent: %v", err)
	}
	if last[ds] == "" {
		t.Fatal("LastEvent empty for a dataset that just emitted")
	}
}

// A payload that is not JSON, or whose transition is outside the closed
// vocabulary, is a PERMANENT failure: Handle errors so the framework DLQs it
// instead of counting garbage.
func TestDataIngestHandler_BadPayloadErrors(t *testing.T) {
	c, _, _ := newDIConsumer(t)
	ctx := context.Background()

	if err := c.Handle(ctx, nil, []byte("{not json")); err == nil {
		t.Fatal("want error on malformed JSON")
	}
	bad := diEvent(t, dataingest.Event{
		OpID: uuid.NewString(), At: time.Now(),
		Class: dataingest.ClassDynamic, Source: dataingest.SourcePartnerAPI,
		Transition: "teleported", N: 5,
	})
	if err := c.Handle(ctx, nil, bad); err == nil {
		t.Fatal("want error on an invalid transition")
	}
	// by_isp that does not sum to n is the silent under-count this check exists
	// to prevent.
	mismatched := []byte(`{"v":1,"op_id":"` + uuid.NewString() + `","at":"2026-09-20T10:00:00Z",` +
		`"supply_class":"dynamic","source_path":"partner_api","transition":"landed",` +
		`"by_isp":{"gmail":5},"n":9}`)
	if err := c.Handle(ctx, nil, mismatched); err == nil {
		t.Fatal("want error when by_isp does not sum to n")
	}
	if _, _, failed := c.Stats(); failed != 3 {
		t.Fatalf("failed=%d, want 3", failed)
	}
}

// The hour bucket is the DENVER hour of `at`, not the UTC hour. 2026-09-20
// 02:30 UTC is 2026-09-19 20:30 in Denver (MDT, UTC-6) — a different DAY as
// well as a different hour, which is exactly the off-by-one that makes a
// midnight ingest land on the wrong dashboard column.
func TestDataIngestHandler_HourBucketIsDenver(t *testing.T) {
	c, _, rdb := newDIConsumer(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 20, 2, 30, 0, 0, time.UTC)

	if err := c.Handle(ctx, nil, diEvent(t, dataingest.Event{
		OpID: uuid.NewString(), At: at,
		Class: dataingest.ClassAtRest, Source: dataingest.SourceCSVUpload,
		Transition: dataingest.TransitionLanded, N: 42,
	})); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	wantDay := at.In(dataingest.Denver).Format(dataingest.DayLayout)
	wantHour := at.In(dataingest.Denver).Hour()
	if dataingest.Denver != time.UTC && wantDay == "2026-09-20" {
		t.Fatalf("tz fixture wrong: Denver day for %s should be 2026-09-19, got %s", at, wantDay)
	}

	r := dataingest.NewReader(rdb)
	hours, err := r.GetHours(ctx, wantDay, dataingest.ClassAtRest)
	if err != nil {
		t.Fatalf("GetHours: %v", err)
	}
	if len(hours) != 24 {
		t.Fatalf("got %d hour buckets, want 24", len(hours))
	}
	for _, h := range hours {
		if h.Hour == wantHour {
			if h.N != 42 {
				t.Fatalf("Denver hour %02d has n=%d, want 42", h.Hour, h.N)
			}
			continue
		}
		if h.N != 0 {
			t.Fatalf("hour %02d should be empty, has %d", h.Hour, h.N)
		}
	}
}

// Rollup: per-dataset rows plus ONE residual row (dataset_id "") for the events
// that carried no dataset, so SUM(n) over the day is exact and the per-feed
// breakdown never double-counts.
func TestDataIngestRollup_DatasetRowsPlusResidual(t *testing.T) {
	c, _, rdb := newDIConsumer(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)
	day := dataingest.DayOf(at)
	ds := uuid.NewString()

	for _, ev := range []dataingest.Event{
		{OpID: uuid.NewString(), At: at, Class: dataingest.ClassDynamic,
			Source: dataingest.SourcePartnerAPI, DatasetID: ds, Lane: "remodel",
			Transition: dataingest.TransitionLanded, ByISP: map[string]int64{"gmail": 10}, N: 10},
		{OpID: uuid.NewString(), At: at, Class: dataingest.ClassDynamic,
			Source:     dataingest.SourceSiteEvent,
			Transition: dataingest.TransitionLanded, ByISP: map[string]int64{"gmail": 4}, N: 4},
	} {
		if err := c.Handle(ctx, nil, diEvent(t, ev)); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}

	rows, err := dataingest.NewReader(rdb).Rollup(ctx, day)
	if err != nil {
		t.Fatalf("Rollup: %v", err)
	}
	var total int64
	var dsRow, residual *dataingest.RollupRow
	for i := range rows {
		total += rows[i].N
		if rows[i].DatasetID == ds {
			dsRow = &rows[i]
		}
		if rows[i].DatasetID == "" {
			residual = &rows[i]
		}
	}
	if total != 14 {
		t.Fatalf("rollup sums to %d, want 14", total)
	}
	if dsRow == nil || dsRow.N != 10 || dsRow.Lane != "remodel" {
		t.Fatalf("dataset row wrong: %+v", dsRow)
	}
	if residual == nil || residual.N != 4 {
		t.Fatalf("residual row wrong: %+v", residual)
	}
}

// With no Redis the consumer must FAIL LOUD, never count nothing quietly: the
// dashboard's whole contract is that an unmeasured field says so.
func TestDataIngestHandler_NilRedisErrors(t *testing.T) {
	c := NewDataIngestCountersConsumer(nil, eventbus.Config{})
	err := c.Handle(context.Background(), nil, diEvent(t, dataingest.Event{
		OpID: uuid.NewString(), At: time.Now(),
		Class: dataingest.ClassAtRest, Source: dataingest.SourceCSVUpload,
		Transition: dataingest.TransitionLanded, N: 1,
	}))
	if err == nil {
		t.Fatal("want an error when Redis is nil")
	}
}

// The DEFAULT hub receives one delta per APPLIED event (and none for a
// duplicate) — this is what feeds GET /data-ingest/stream.
func TestDataIngestHandler_PublishesDeltaOncePerOp(t *testing.T) {
	c, _, _ := newDIConsumer(t)
	ctx := context.Background()
	ch, cancel := dataingest.DefaultHub().Subscribe()
	defer cancel()

	payload := diEvent(t, dataingest.Event{
		OpID: uuid.NewString(), At: time.Now(),
		Class: dataingest.ClassInternalTransfer, Source: dataingest.SourceYahooFamilyInject,
		Transition: dataingest.TransitionTransfer, N: 7,
	})
	if err := c.Handle(ctx, nil, payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if err := c.Handle(ctx, nil, payload); err != nil {
		t.Fatalf("replay: %v", err)
	}

	select {
	case d := <-ch:
		if d.N != 7 || d.SupplyClass != dataingest.ClassInternalTransfer {
			t.Fatalf("delta %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no delta published")
	}
	select {
	case d := <-ch:
		t.Fatalf("duplicate published a second delta: %+v", d)
	case <-time.After(100 * time.Millisecond):
	}
}
