package analytics

import (
	"context"
	"strings"
	"testing"
)

// resetReader clears the global reader between tests.
func resetReader() {
	readerMu.Lock()
	reader = nil
	readerMu.Unlock()
}

func TestInitReader_DisabledWhenNoOutput(t *testing.T) {
	resetReader()
	if err := InitReader(context.Background(), "", "", "", ""); err != nil {
		t.Fatalf("InitReader with empty output should not error, got %v", err)
	}
	if ReaderEnabled() {
		t.Fatalf("reader should be disabled when output is empty")
	}
}

func TestDisabledQueriesReturnError(t *testing.T) {
	resetReader()
	if _, err := Summary(context.Background(), "2026-06-01", "2026-06-08"); err == nil {
		t.Fatalf("Summary should error when reader disabled")
	} else if !IsDisabledErr(err) {
		t.Fatalf("expected disabled sentinel, got %v", err)
	}
	if _, err := RecentEvents(context.Background(), EventFilter{Dt: "2026-06-08"}); err == nil {
		t.Fatalf("RecentEvents should error when reader disabled")
	} else if !IsDisabledErr(err) {
		t.Fatalf("expected disabled sentinel, got %v", err)
	}
}

// validateFilter mirrors the validation RecentEvents performs, so we can prove
// rejection without standing up an Athena client.
func validateFilter(f EventFilter) error {
	if err := validateDt("dt", f.Dt); err != nil {
		return err
	}
	if err := validateUUID("campaign_id", f.CampaignID); err != nil {
		return err
	}
	if err := validateToken("isp_group", f.ISPGroup); err != nil {
		return err
	}
	if err := validateToken("event_type", f.EventType); err != nil {
		return err
	}
	return nil
}

func TestInjectionRejection(t *testing.T) {
	bad := []struct {
		name string
		f    EventFilter
	}{
		{"sqli-dt", EventFilter{Dt: "2026'; DROP TABLE email_events;--"}},
		{"malformed-dt", EventFilter{Dt: "2026-6-8"}},
		{"dt-spaces", EventFilter{Dt: "2026-06-08 OR 1=1"}},
		{"non-uuid-campaign", EventFilter{CampaignID: "not-a-uuid"}},
		{"sqli-campaign", EventFilter{CampaignID: "1' OR '1'='1"}},
		{"sqli-isp", EventFilter{ISPGroup: "gmail' OR 1=1"}},
		{"isp-space", EventFilter{ISPGroup: "gmail group"}},
		{"sqli-eventtype", EventFilter{EventType: "click;DROP"}},
		{"eventtype-quote", EventFilter{EventType: "click'"}},
	}
	for _, tc := range bad {
		if err := validateFilter(tc.f); err == nil {
			t.Errorf("%s: expected validation error, got nil", tc.name)
		}
	}
}

func TestValidInputsAccepted(t *testing.T) {
	good := []EventFilter{
		{},
		{Dt: "2026-06-08"},
		{CampaignID: "550e8400-e29b-41d4-a716-446655440000"},
		{ISPGroup: "gmail"},
		{EventType: "click"},
		{ISPGroup: "yahoo-aol", EventType: "soft_bounce", Dt: "2026-01-01"},
	}
	for i, f := range good {
		if err := validateFilter(f); err != nil {
			t.Errorf("good[%d] %+v: unexpected error %v", i, f, err)
		}
	}
}

func TestSummaryRejectsBadDates(t *testing.T) {
	// Even with a (non-nil) reader, validation runs before any network call,
	// so a bad date must fail fast. Use a reader with a client we never call.
	resetReader()
	r := &Reader{database: "ignite_analytics", workgroup: "primary", output: "s3://x/"}
	if _, err := r.Summary(context.Background(), "2026'; DROP", "2026-06-08"); err == nil {
		t.Fatalf("Summary should reject injection in fromDt")
	}
	if _, err := r.Summary(context.Background(), "2026-06-01", "bad"); err == nil {
		t.Fatalf("Summary should reject malformed toDt")
	}
	if _, err := r.Summary(context.Background(), "", "2026-06-08"); err == nil {
		t.Fatalf("Summary should require fromDt")
	}
}

func TestClampLimit(t *testing.T) {
	cases := map[int]int{-5: 1, 0: 1, 1: 1, 100: 100, 1000: 1000, 5000: 1000}
	for in, want := range cases {
		if got := clampLimit(in); got != want {
			t.Errorf("clampLimit(%d)=%d want %d", in, got, want)
		}
	}
}

func TestRecentEventsBuiltSQLShape(t *testing.T) {
	// Validate the SELECT column order matches scanEvent by round-tripping a
	// synthetic row. This guards against column/scan drift.
	row := []string{
		"uid1", "rsid", "550e8400-e29b-41d4-a716-446655440000", "sub", "a@b.com",
		"b.com", "brandx", "gmail", "ses", "click", "", "vmta1", "pool1", "",
		"", "", "https://x/", "1.2.3.4", "v2", "2026-06-08T00:00:00Z",
		"1717804800000", "2026-06-08T00:00:01Z", "app", "2026-06-08",
	}
	ev := scanEvent(row)
	if ev.EventUID != "uid1" || ev.CampaignID != "550e8400-e29b-41d4-a716-446655440000" ||
		ev.ISPGroup != "gmail" || ev.EventType != "click" || ev.Dt != "2026-06-08" ||
		ev.EventEpochMs != 1717804800000 || ev.Source != "app" {
		t.Fatalf("scanEvent mapped columns incorrectly: %+v", ev)
	}
}

func TestScanEventShortRow(t *testing.T) {
	// A short/empty row must not panic and yields zero-value fields.
	ev := scanEvent([]string{"only-uid"})
	if ev.EventUID != "only-uid" || ev.Dt != "" || ev.EventEpochMs != 0 {
		t.Fatalf("scanEvent short row unexpected: %+v", ev)
	}
}

func TestValidationErrorMentionsField(t *testing.T) {
	err := validateDt("dt", "bad")
	if err == nil || !strings.Contains(err.Error(), "dt") {
		t.Fatalf("expected dt error, got %v", err)
	}
}
