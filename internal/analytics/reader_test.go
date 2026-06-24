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

func TestSqlStr(t *testing.T) {
	// Date-shaped values MUST become quoted string literals (the fix for the
	// Athena "varchar BETWEEN integer and integer" type-mismatch). The '' escape
	// is defense-in-depth; validation prevents quotes from ever reaching here.
	cases := map[string]string{
		"2026-06-03": "'2026-06-03'",
		"gmail":      "'gmail'",
		"a'b":        "'a''b'",
	}
	for in, want := range cases {
		if got := sqlStr(in); got != want {
			t.Errorf("sqlStr(%q)=%q want %q", in, got, want)
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

func TestBuildBreakdownSQL(t *testing.T) {
	// Eq predicates render in sorted column order, so the expected strings are
	// deterministic. COUNT(DISTINCT event_uid) is load-bearing (bridge
	// redelivery dedup) — these assertions guard against a COUNT(*) regression.
	cases := []struct {
		name string
		f    BreakdownFilter
		want string
	}{
		{
			"one-dim",
			BreakdownFilter{From: "2026-06-01", To: "2026-06-08", GroupBy: []string{"event_type"}},
			"SELECT event_type, COUNT(DISTINCT event_uid) c FROM email_events" +
				" WHERE dt BETWEEN '2026-06-01' AND '2026-06-08'" +
				" GROUP BY event_type ORDER BY c DESC LIMIT 1000",
		},
		{
			"two-dims",
			BreakdownFilter{From: "2026-06-01", To: "2026-06-08", GroupBy: []string{"isp_group", "event_type"}, Limit: 50},
			"SELECT isp_group, event_type, COUNT(DISTINCT event_uid) c FROM email_events" +
				" WHERE dt BETWEEN '2026-06-01' AND '2026-06-08'" +
				" GROUP BY isp_group, event_type ORDER BY c DESC LIMIT 50",
		},
		{
			"with-eq-filters",
			BreakdownFilter{
				From: "2026-06-01", To: "2026-06-08",
				GroupBy: []string{"event_type"},
				Eq: map[string]string{
					"isp_group":   "gmail",
					"brand":       "discountblog.com",
					"campaign_id": "550e8400-e29b-41d4-a716-446655440000",
					"vmta":        "", // empty value: skipped, not a predicate
				},
				Limit: 10,
			},
			"SELECT event_type, COUNT(DISTINCT event_uid) c FROM email_events" +
				" WHERE dt BETWEEN '2026-06-01' AND '2026-06-08'" +
				" AND brand = 'discountblog.com'" +
				" AND campaign_id = '550e8400-e29b-41d4-a716-446655440000'" +
				" AND isp_group = 'gmail'" +
				" GROUP BY event_type ORDER BY c DESC LIMIT 10",
		},
		{
			// (a) Combined transport: source IN (...) present, COUNT(DISTINCT) intact.
			"source-in-combined",
			BreakdownFilter{From: "2026-06-01", To: "2026-06-08", GroupBy: []string{"event_type"}, SourceIn: []string{"pmta", "ses"}},
			"SELECT event_type, COUNT(DISTINCT event_uid) c FROM email_events" +
				" WHERE dt BETWEEN '2026-06-01' AND '2026-06-08'" +
				" AND source IN ('pmta', 'ses')" +
				" GROUP BY event_type ORDER BY c DESC LIMIT 1000",
		},
		{
			// (b) SourceIn + an Eq filter: the IN clause precedes the Eq predicate.
			"source-in-precedes-eq",
			BreakdownFilter{
				From: "2026-06-01", To: "2026-06-08",
				GroupBy:  []string{"event_type"},
				Eq:       map[string]string{"isp_group": "gmail"},
				SourceIn: []string{"pmta", "ses"},
			},
			"SELECT event_type, COUNT(DISTINCT event_uid) c FROM email_events" +
				" WHERE dt BETWEEN '2026-06-01' AND '2026-06-08'" +
				" AND source IN ('pmta', 'ses')" +
				" AND isp_group = 'gmail'" +
				" GROUP BY event_type ORDER BY c DESC LIMIT 1000",
		},
		{
			// (c) SourceIn + local_dt in GroupBy: BOTH the widened dt BETWEEN and
			// the source IN clause are present (widening gates only on local_dt).
			"source-in-with-local-dt",
			BreakdownFilter{From: "2026-06-02", To: "2026-06-07", GroupBy: []string{"local_dt"}, SourceIn: []string{"pmta", "ses"}},
			"SELECT " + localDtExpr + " AS local_dt, COUNT(DISTINCT event_uid) c FROM email_events" +
				" WHERE dt BETWEEN '2026-06-01' AND '2026-06-08'" +
				" AND " + localDtExpr + " BETWEEN '2026-06-02' AND '2026-06-07'" +
				" AND source IN ('pmta', 'ses')" +
				" GROUP BY " + localDtExpr + " ORDER BY c DESC LIMIT 1000",
		},
		{
			// (e) Dedupe: duplicated SourceIn values render each once.
			"source-in-dedupe",
			BreakdownFilter{From: "2026-06-01", To: "2026-06-08", GroupBy: []string{"event_type"}, SourceIn: []string{"pmta", "pmta", "ses"}},
			"SELECT event_type, COUNT(DISTINCT event_uid) c FROM email_events" +
				" WHERE dt BETWEEN '2026-06-01' AND '2026-06-08'" +
				" AND source IN ('pmta', 'ses')" +
				" GROUP BY event_type ORDER BY c DESC LIMIT 1000",
		},
	}
	for _, tc := range cases {
		got, err := buildBreakdownSQL(tc.f)
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s:\n got  %s\n want %s", tc.name, got, tc.want)
		}
	}
}

func TestBuildBreakdownSQLLocalHour(t *testing.T) {
	// local_hour mirrors local_dt: Denver-derived bucket, so it must (a) render
	// the computed localHourExpr in SELECT and GROUP BY, and (b) trigger the
	// ±1-day dt partition widening (hour buckets straddle the UTC day boundary).
	cases := []struct {
		name string
		f    BreakdownFilter
		want string
	}{
		{
			// local_hour in GroupBy: widened dt BETWEEN + precise local-day predicate.
			"local-hour-group-by",
			BreakdownFilter{From: "2026-06-02", To: "2026-06-07", GroupBy: []string{"local_hour"}},
			"SELECT " + localHourExpr + " AS local_hour, COUNT(DISTINCT event_uid) c FROM email_events" +
				" WHERE dt BETWEEN '2026-06-01' AND '2026-06-08'" +
				" AND " + localDtExpr + " BETWEEN '2026-06-02' AND '2026-06-07'" +
				" GROUP BY " + localHourExpr + " ORDER BY c DESC LIMIT 1000",
		},
		{
			// local_hour + event_type: the frontend's groupBy=local_hour,event_type
			// hourly-trend contract. Widening still fires off local_hour.
			"local-hour-plus-event-type",
			BreakdownFilter{From: "2026-06-02", To: "2026-06-07", GroupBy: []string{"local_hour", "event_type"}},
			"SELECT " + localHourExpr + " AS local_hour, event_type, COUNT(DISTINCT event_uid) c FROM email_events" +
				" WHERE dt BETWEEN '2026-06-01' AND '2026-06-08'" +
				" AND " + localDtExpr + " BETWEEN '2026-06-02' AND '2026-06-07'" +
				" GROUP BY " + localHourExpr + ", event_type ORDER BY c DESC LIMIT 1000",
		},
		{
			// local_hour in GroupBy coexisting with an Eq predicate on isp_group.
			"local-hour-with-eq-isp",
			BreakdownFilter{
				From: "2026-06-02", To: "2026-06-07",
				GroupBy: []string{"local_hour"},
				Eq:      map[string]string{"isp_group": "gmail"},
			},
			"SELECT " + localHourExpr + " AS local_hour, COUNT(DISTINCT event_uid) c FROM email_events" +
				" WHERE dt BETWEEN '2026-06-01' AND '2026-06-08'" +
				" AND " + localDtExpr + " BETWEEN '2026-06-02' AND '2026-06-07'" +
				" AND isp_group = 'gmail'" +
				" GROUP BY " + localHourExpr + " ORDER BY c DESC LIMIT 1000",
		},
		{
			// local_hour as an Eq value: rendered through the computed expression,
			// NOT a raw column; widening fires off the Eq too (no GroupBy local_hour).
			"local-hour-eq-value",
			BreakdownFilter{
				From: "2026-06-02", To: "2026-06-07",
				GroupBy: []string{"event_type"},
				Eq:      map[string]string{"local_hour": "2026-06-04 09:00"},
			},
			"SELECT event_type, COUNT(DISTINCT event_uid) c FROM email_events" +
				" WHERE dt BETWEEN '2026-06-01' AND '2026-06-08'" +
				" AND " + localDtExpr + " BETWEEN '2026-06-02' AND '2026-06-07'" +
				" AND " + localHourExpr + " = '2026-06-04 09:00'" +
				" GROUP BY event_type ORDER BY c DESC LIMIT 1000",
		},
	}
	for _, tc := range cases {
		got, err := buildBreakdownSQL(tc.f)
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s:\n got  %s\n want %s", tc.name, got, tc.want)
		}
	}
}

func TestBuildBreakdownSQLLocalHourValidation(t *testing.T) {
	base := func() BreakdownFilter {
		return BreakdownFilter{From: "2026-06-01", To: "2026-06-08", GroupBy: []string{"event_type"}}
	}
	// Valid "YYYY-MM-DD HH:00" passes the localHourRe wired into breakdownDims.
	f := base()
	f.Eq = map[string]string{"local_hour": "2026-06-24 09:00"}
	if _, err := buildBreakdownSQL(f); err != nil {
		t.Errorf("valid local_hour Eq value should be accepted, got %v", err)
	}

	// Invalid local_hour Eq values must be rejected by validation (proves the
	// regex is wired): a non-:00 minute and a bogus string.
	bad := []struct {
		name string
		f    BreakdownFilter
	}{
		{"local-hour-bad-minute", func() BreakdownFilter {
			f := base()
			f.Eq = map[string]string{"local_hour": "2026-06-24 09:30"}
			return f
		}()},
		{"local-hour-bogus", func() BreakdownFilter { f := base(); f.Eq = map[string]string{"local_hour": "bogus"}; return f }()},
		{"local-hour-day-only", func() BreakdownFilter { f := base(); f.Eq = map[string]string{"local_hour": "2026-06-24"}; return f }()},
		{"local-hour-injection", func() BreakdownFilter {
			f := base()
			f.Eq = map[string]string{"local_hour": "2026-06-24 09:00' OR 1=1"}
			return f
		}()},
	}
	for _, tc := range bad {
		if _, err := buildBreakdownSQL(tc.f); err == nil {
			t.Errorf("%s: expected validation error, got nil", tc.name)
		}
	}
}

func TestBuildBreakdownSQLRejections(t *testing.T) {
	base := func() BreakdownFilter {
		return BreakdownFilter{From: "2026-06-01", To: "2026-06-08", GroupBy: []string{"event_type"}}
	}
	bad := []struct {
		name string
		f    BreakdownFilter
	}{
		{"unknown-dim", BreakdownFilter{From: "2026-06-01", To: "2026-06-08", GroupBy: []string{"email"}}},
		{"too-many-dims", BreakdownFilter{From: "2026-06-01", To: "2026-06-08", GroupBy: []string{"dt", "brand", "isp_group", "event_type"}}},
		{"empty-group-by", BreakdownFilter{From: "2026-06-01", To: "2026-06-08"}},
		{"missing-from", BreakdownFilter{To: "2026-06-08", GroupBy: []string{"event_type"}}},
		// An inverted range would make BETWEEN silently return zero rows.
		{"from-after-to", BreakdownFilter{From: "2026-06-09", To: "2026-06-01", GroupBy: []string{"event_type"}}},
		{"bad-from-dt", BreakdownFilter{From: "2026'; DROP", To: "2026-06-08", GroupBy: []string{"event_type"}}},
		{"bad-to-dt", BreakdownFilter{From: "2026-06-01", To: "2026-6-8", GroupBy: []string{"event_type"}}},
		{"unknown-eq-column", func() BreakdownFilter { f := base(); f.Eq = map[string]string{"email": "a@b.com"}; return f }()},
		// One injection-shaped value per validation class.
		{"eq-dt-quote", func() BreakdownFilter { f := base(); f.Eq = map[string]string{"dt": "2026-06-08' OR 1=1"}; return f }()},
		{"eq-campaign-not-uuid", func() BreakdownFilter { f := base(); f.Eq = map[string]string{"campaign_id": "not-a-uuid"}; return f }()},
		{"eq-campaign-quote", func() BreakdownFilter { f := base(); f.Eq = map[string]string{"campaign_id": "1' OR '1'='1"}; return f }()},
		{"eq-token-semicolon", func() BreakdownFilter { f := base(); f.Eq = map[string]string{"event_type": "click;DROP"}; return f }()},
		{"eq-token-quote", func() BreakdownFilter { f := base(); f.Eq = map[string]string{"isp_group": "gmail'"}; return f }()},
		{"eq-dotted-quote", func() BreakdownFilter { f := base(); f.Eq = map[string]string{"brand": "x.com'"}; return f }()},
		{"eq-dotted-semicolon", func() BreakdownFilter { f := base(); f.Eq = map[string]string{"email_domain": "b.com;DROP"}; return f }()},
		// isp_group is token-validated: dots are NOT allowed there.
		{"eq-isp-dot-rejected", func() BreakdownFilter { f := base(); f.Eq = map[string]string{"isp_group": "gmail.com"}; return f }()},
		// (d) source_in is allowlist-validated: only pmta|ses; anything else rejects.
		{"source-in-bogus", func() BreakdownFilter { f := base(); f.SourceIn = []string{"pmta", "bogus"}; return f }()},
		{"source-in-injection", func() BreakdownFilter { f := base(); f.SourceIn = []string{"'; DROP"}; return f }()},
	}
	for _, tc := range bad {
		if _, err := buildBreakdownSQL(tc.f); err == nil {
			t.Errorf("%s: expected validation error, got nil", tc.name)
		}
	}

	// Unknown dims must be named in the error so the caller can fix the request.
	if _, err := buildBreakdownSQL(BreakdownFilter{From: "2026-06-01", To: "2026-06-08", GroupBy: []string{"bogus"}}); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("unknown dim error should name the dim, got %v", err)
	}

	// dotted-pattern columns ACCEPT dots (domains, vmta/pool names).
	f := base()
	f.Eq = map[string]string{"brand": "discountblog.com", "vmta": "db-gmail-pool.1", "email_domain": "yahoo.co.uk"}
	if _, err := buildBreakdownSQL(f); err != nil {
		t.Errorf("dotted values on dotted columns should be accepted, got %v", err)
	}

	// Duplicate dims dedupe rather than error (and 4-with-dup is 3 unique = OK).
	if _, err := buildBreakdownSQL(BreakdownFilter{From: "2026-06-01", To: "2026-06-08", GroupBy: []string{"event_type", "event_type", "brand", "isp_group"}}); err != nil {
		t.Errorf("deduped group_by should be accepted, got %v", err)
	}
}

func TestClampBreakdownLimit(t *testing.T) {
	cases := map[int]int{-1: 1000, 0: 1000, 1: 1, 1000: 1000, 5000: 5000, 99999: 5000}
	for in, want := range cases {
		if got := clampBreakdownLimit(in); got != want {
			t.Errorf("clampBreakdownLimit(%d)=%d want %d", in, got, want)
		}
		if got := ClampBreakdownLimit(in); got != want {
			t.Errorf("ClampBreakdownLimit(%d)=%d want %d", in, got, want)
		}
	}
}

func TestBreakdownDisabled(t *testing.T) {
	resetReader()
	if _, err := Breakdown(context.Background(), BreakdownFilter{From: "2026-06-01", To: "2026-06-08", GroupBy: []string{"event_type"}}); err == nil {
		t.Fatalf("Breakdown should error when reader disabled")
	} else if !IsDisabledErr(err) {
		t.Fatalf("expected disabled sentinel, got %v", err)
	}
}
