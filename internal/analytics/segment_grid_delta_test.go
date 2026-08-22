package analytics

import (
	"strings"
	"testing"
)

func TestParseS3Bucket(t *testing.T) {
	cases := []struct{ in, want string }{
		{"s3://ignite-analytics-lake/athena-results/", "ignite-analytics-lake"},
		{"s3://bucket", "bucket"},
		{"s3://bucket/", "bucket"},
		{"https://example.com/x", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := parseS3Bucket(c.in); got != c.want {
			t.Errorf("parseS3Bucket(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildGridSnapshotTableDDL_Shape(t *testing.T) {
	ddl := buildGridSnapshotTableDDL("ignite-analytics-lake")
	for _, want := range []string{
		"CREATE EXTERNAL TABLE IF NOT EXISTS segment_grid_snapshots",
		"PARTITIONED BY (event string, window_days string, dt string)",
		"STORED AS PARQUET",
		"LOCATION 's3://ignite-analytics-lake/segment_snapshots/'",
		"'projection.enabled'='true'",
		"'projection.event.values'='open,click'",
		"'projection.window_days.values'='7,14,30,60'",
		"'projection.dt.type'='date'",
		"'storage.location.template'='s3://ignite-analytics-lake/segment_snapshots/event=${event}/window_days=${window_days}/dt=${dt}/'",
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("DDL missing %q:\n%s", want, ddl)
		}
	}
}

func TestBuildGridSnapshotUnloadSQL_Shape(t *testing.T) {
	sql, err := buildGridSnapshotUnloadSQL("lake", SegmentEventOpen, 30, "2026-08-21", "2026-07-22", 1753164000000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(sql, "UNLOAD (SELECT subscriber_id, brand FROM email_events") {
		t.Fatalf("bad UNLOAD prefix: %s", sql)
	}
	if !strings.Contains(sql, "TO 's3://lake/segment_snapshots/event=open/window_days=30/dt=2026-08-21/'") {
		t.Fatalf("bad UNLOAD target: %s", sql)
	}
	if !strings.Contains(sql, "WITH (format = 'PARQUET')") {
		t.Fatalf("bad UNLOAD format: %s", sql)
	}
	if strings.Contains(sql, "LIMIT") {
		t.Fatalf("UNLOAD must not truncate the snapshot with a LIMIT: %s", sql)
	}
}

func TestBuildGridSnapshotUnloadSQL_RejectsBadInput(t *testing.T) {
	cases := []struct {
		name   string
		event  string
		window int
		dt     string
	}{
		{"bad event", "delivered", 30, "2026-08-21"},
		{"injection event", "open' OR '1'='1", 30, "2026-08-21"},
		{"unprojected window", SegmentEventOpen, 45, "2026-08-21"},
		{"bad dt", SegmentEventOpen, 30, "not-a-date"},
	}
	for _, c := range cases {
		if _, err := buildGridSnapshotUnloadSQL("lake", c.event, c.window, c.dt, "2026-07-22", 1); err == nil {
			t.Errorf("%s: expected error, got none", c.name)
		}
	}
}

func TestBuildGridSnapshotDiffSQL_Shape(t *testing.T) {
	sql, err := buildGridSnapshotDiffSQL(SegmentEventClick, 7, "2026-08-20", "2026-08-21")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"SELECT 'add' AS op, subscriber_id, brand FROM (",
		"SELECT 'del' AS op, subscriber_id, brand FROM (",
		" EXCEPT ",
		"UNION ALL",
		"window_days = '7'",
		"dt = '2026-08-20'",
		"dt = '2026-08-21'",
		"event = 'click'",
		"LIMIT 2000001",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("diff SQL missing %q:\n%s", want, sql)
		}
	}
}

func TestBuildGridSnapshotDiffSQL_RejectsInvertedOrEqualRange(t *testing.T) {
	if _, err := buildGridSnapshotDiffSQL(SegmentEventClick, 7, "2026-08-21", "2026-08-21"); err == nil {
		t.Error("base == today must be rejected (nothing to diff)")
	}
	if _, err := buildGridSnapshotDiffSQL(SegmentEventClick, 7, "2026-08-22", "2026-08-21"); err == nil {
		t.Error("base after today must be rejected")
	}
	if _, err := buildGridSnapshotDiffSQL(SegmentEventClick, 9, "2026-08-20", "2026-08-21"); err == nil {
		t.Error("unprojected window must be rejected")
	}
}

func TestGridSnapshotWindowOK(t *testing.T) {
	for _, w := range []int{7, 14, 30, 60} {
		if !GridSnapshotWindowOK(w) {
			t.Errorf("window %d must be snapshot-projected", w)
		}
	}
	for _, w := range []int{0, 1, 45, 90, 120} {
		if GridSnapshotWindowOK(w) {
			t.Errorf("window %d must NOT be snapshot-projected", w)
		}
	}
}
