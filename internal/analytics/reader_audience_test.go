package analytics

import (
	"context"
	"strings"
	"testing"
)

func TestBuildAudienceLatestDtSQL(t *testing.T) {
	got, err := buildAudienceLatestDtSQL("2026-06-05")
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	want := "SELECT dt, COUNT(*) c FROM audience WHERE dt >= '2026-06-05' GROUP BY dt ORDER BY dt DESC"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if _, err := buildAudienceLatestDtSQL("2026'; DROP"); err == nil {
		t.Errorf("expected rejection of injection-shaped min dt")
	}
	if _, err := buildAudienceLatestDtSQL(""); err == nil {
		t.Errorf("expected rejection of empty min dt")
	}
}

func TestBuildAudienceBreakdownSQL(t *testing.T) {
	cases := []struct {
		name string
		f    AudienceBreakdownFilter
		want string
	}{
		{
			"one-dim-defaults",
			AudienceBreakdownFilter{Dt: "2026-06-08", GroupBy: []string{"status"}},
			"SELECT status, COUNT(*) c, ROUND(AVG(engagement_score), 2) FROM audience" +
				" WHERE dt = '2026-06-08'" +
				" GROUP BY status ORDER BY c DESC LIMIT 1000",
		},
		{
			// Eq predicates render in sorted column order; a churned range
			// implies churned_dt <> '' so the '' sentinel can't slip under
			// the lower bound. COUNT(*) (not DISTINCT) is deliberate: one
			// snapshot row per subscriber-list row by construction.
			"full-filters",
			AudienceBreakdownFilter{
				Dt:      "2026-06-08",
				GroupBy: []string{"data_source", "engagement_band"},
				Eq: map[string]string{
					"status":       "active",
					"email_domain": "gmail.com",
					"isp":          "", // empty value: skipped, not a predicate
				},
				AcquiredFrom: "2026-05-01",
				AcquiredTo:   "2026-05-31",
				ChurnedFrom:  "2026-05-01",
				ChurnedTo:    "2026-06-08",
				Limit:        50,
			},
			"SELECT data_source, engagement_band, COUNT(*) c, ROUND(AVG(engagement_score), 2) FROM audience" +
				" WHERE dt = '2026-06-08'" +
				" AND email_domain = 'gmail.com'" +
				" AND status = 'active'" +
				" AND acquired_dt >= '2026-05-01'" +
				" AND acquired_dt <= '2026-05-31'" +
				" AND churned_dt <> ''" +
				" AND churned_dt >= '2026-05-01'" +
				" AND churned_dt <= '2026-06-08'" +
				" GROUP BY data_source, engagement_band ORDER BY c DESC LIMIT 50",
		},
		{
			// free-token values: colons, slashes, dots and spaces are legal
			// in source/data_source provenance strings.
			"free-token-eq",
			AudienceBreakdownFilter{
				Dt:      "2026-06-08",
				GroupBy: []string{"engagement_band"},
				Eq: map[string]string{
					"source":      "warmup-data/2026/3M_Free_090123.csv",
					"data_source": "attribits:finance:attribits_finance_001",
				},
				Limit: 10,
			},
			"SELECT engagement_band, COUNT(*) c, ROUND(AVG(engagement_score), 2) FROM audience" +
				" WHERE dt = '2026-06-08'" +
				" AND data_source = 'attribits:finance:attribits_finance_001'" +
				" AND source = 'warmup-data/2026/3M_Free_090123.csv'" +
				" GROUP BY engagement_band ORDER BY c DESC LIMIT 10",
		},
	}
	for _, tc := range cases {
		got, err := buildAudienceBreakdownSQL(tc.f)
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s:\n got  %s\n want %s", tc.name, got, tc.want)
		}
	}
}

func TestBuildAudienceBreakdownSQLRejections(t *testing.T) {
	base := func() AudienceBreakdownFilter {
		return AudienceBreakdownFilter{Dt: "2026-06-08", GroupBy: []string{"status"}}
	}
	bad := []struct {
		name string
		f    AudienceBreakdownFilter
	}{
		{"missing-dt", AudienceBreakdownFilter{GroupBy: []string{"status"}}},
		{"bad-dt", AudienceBreakdownFilter{Dt: "2026'; DROP", GroupBy: []string{"status"}}},
		{"unknown-dim", AudienceBreakdownFilter{Dt: "2026-06-08", GroupBy: []string{"email"}}},
		{"too-many-dims", AudienceBreakdownFilter{Dt: "2026-06-08", GroupBy: []string{"status", "isp", "source", "data_source"}}},
		{"empty-group-by", AudienceBreakdownFilter{Dt: "2026-06-08"}},
		{"unknown-eq-column", func() AudienceBreakdownFilter { f := base(); f.Eq = map[string]string{"email": "a@b.com"}; return f }()},
		// freeTokenRe excludes quotes and semicolons — injection-shaped
		// provenance values must be rejected.
		{"source-quote", func() AudienceBreakdownFilter { f := base(); f.Eq = map[string]string{"source": "x' OR 1=1"}; return f }()},
		{"data-source-semicolon", func() AudienceBreakdownFilter { f := base(); f.Eq = map[string]string{"data_source": "x;DROP TABLE audience"}; return f }()},
		{"status-quote", func() AudienceBreakdownFilter { f := base(); f.Eq = map[string]string{"status": "active'"}; return f }()},
		{"list-id-not-uuid", func() AudienceBreakdownFilter { f := base(); f.Eq = map[string]string{"list_id": "not-a-uuid"}; return f }()},
		{"acquired-dt-eq-malformed", func() AudienceBreakdownFilter { f := base(); f.Eq = map[string]string{"acquired_dt": "2026-6-8"}; return f }()},
		{"bad-acquired-from", func() AudienceBreakdownFilter { f := base(); f.AcquiredFrom = "2026-6-1"; return f }()},
		{"bad-churned-to", func() AudienceBreakdownFilter { f := base(); f.ChurnedTo = "bad"; return f }()},
		// Inverted ranges would silently match zero rows — reject explicitly.
		{"acquired-from-after-to", func() AudienceBreakdownFilter {
			f := base()
			f.AcquiredFrom, f.AcquiredTo = "2026-06-09", "2026-06-01"
			return f
		}()},
		{"churned-from-after-to", func() AudienceBreakdownFilter {
			f := base()
			f.ChurnedFrom, f.ChurnedTo = "2026-06-09", "2026-06-01"
			return f
		}()},
	}
	for _, tc := range bad {
		if _, err := buildAudienceBreakdownSQL(tc.f); err == nil {
			t.Errorf("%s: expected validation error, got nil", tc.name)
		}
	}

	// freeTokenRe ACCEPTS the real provenance charset: spaces, dots, slashes,
	// colons.
	f := base()
	f.Eq = map[string]string{
		"source":        "warmup-data/2026/3M Free 090123.csv",
		"data_source":   "attribits:finance:attribits_finance_001",
		"source_system": "jvc-warmup",
	}
	if _, err := buildAudienceBreakdownSQL(f); err != nil {
		t.Errorf("free-token values with space/dot/slash/colon should be accepted, got %v", err)
	}
}

func TestBuildAudienceSourcePerfSQL(t *testing.T) {
	cases := []struct {
		name string
		f    SourcePerfFilter
		want string
	}{
		{
			"plain",
			SourcePerfFilter{AudienceDt: "2026-06-08", From: "2026-06-02", To: "2026-06-08", Dim: "data_source"},
			"WITH aud AS (SELECT id, email, data_source AS dimv FROM audience WHERE dt = '2026-06-08')," +
				" keyed AS (SELECT join_key, max(dimv) AS dimv FROM (" +
				"SELECT id AS join_key, dimv FROM aud UNION ALL SELECT email AS join_key, dimv FROM aud" +
				") GROUP BY join_key)" +
				" SELECT k.dimv, e.event_type, COUNT(DISTINCT e.event_uid) c FROM email_events e" +
				" JOIN keyed k ON (CASE WHEN e.subscriber_id <> '' THEN e.subscriber_id ELSE lower(e.email) END) = k.join_key" +
				" WHERE e.dt BETWEEN '2026-06-02' AND '2026-06-08'" +
				" GROUP BY 1, 2 ORDER BY c DESC LIMIT 2000",
		},
		{
			"with-eq",
			SourcePerfFilter{
				AudienceDt: "2026-06-08", From: "2026-06-02", To: "2026-06-08", Dim: "source",
				Eq:    map[string]string{"engagement_band": "hot", "isp": "gmail"},
				Limit: 100,
			},
			"WITH aud AS (SELECT id, email, source AS dimv FROM audience WHERE dt = '2026-06-08'" +
				" AND engagement_band = 'hot' AND isp = 'gmail')," +
				" keyed AS (SELECT join_key, max(dimv) AS dimv FROM (" +
				"SELECT id AS join_key, dimv FROM aud UNION ALL SELECT email AS join_key, dimv FROM aud" +
				") GROUP BY join_key)" +
				" SELECT k.dimv, e.event_type, COUNT(DISTINCT e.event_uid) c FROM email_events e" +
				" JOIN keyed k ON (CASE WHEN e.subscriber_id <> '' THEN e.subscriber_id ELSE lower(e.email) END) = k.join_key" +
				" WHERE e.dt BETWEEN '2026-06-02' AND '2026-06-08'" +
				" GROUP BY 1, 2 ORDER BY c DESC LIMIT 100",
		},
	}
	for _, tc := range cases {
		got, err := buildAudienceSourcePerfSQL(tc.f)
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s:\n got  %s\n want %s", tc.name, got, tc.want)
		}
	}
}

func TestBuildAudienceSourcePerfSQLRejections(t *testing.T) {
	base := func() SourcePerfFilter {
		return SourcePerfFilter{AudienceDt: "2026-06-08", From: "2026-06-02", To: "2026-06-08", Dim: "source"}
	}
	bad := []struct {
		name string
		f    SourcePerfFilter
	}{
		{"missing-audience-dt", SourcePerfFilter{From: "2026-06-02", To: "2026-06-08", Dim: "source"}},
		{"bad-audience-dt", SourcePerfFilter{AudienceDt: "2026'; DROP", From: "2026-06-02", To: "2026-06-08", Dim: "source"}},
		{"missing-from", SourcePerfFilter{AudienceDt: "2026-06-08", To: "2026-06-08", Dim: "source"}},
		{"bad-to", SourcePerfFilter{AudienceDt: "2026-06-08", From: "2026-06-02", To: "2026-6-8", Dim: "source"}},
		// Inverted range would silently match zero rows.
		{"from-after-to", SourcePerfFilter{AudienceDt: "2026-06-08", From: "2026-06-09", To: "2026-06-02", Dim: "source"}},
		{"missing-dim", SourcePerfFilter{AudienceDt: "2026-06-08", From: "2026-06-02", To: "2026-06-08"}},
		// email_domain is a valid audience dim but NOT a source-perf dim.
		{"non-perf-dim", SourcePerfFilter{AudienceDt: "2026-06-08", From: "2026-06-02", To: "2026-06-08", Dim: "email_domain"}},
		{"injection-dim", SourcePerfFilter{AudienceDt: "2026-06-08", From: "2026-06-02", To: "2026-06-08", Dim: "source; DROP"}},
		{"eq-source-quote", func() SourcePerfFilter { f := base(); f.Eq = map[string]string{"source": "x'--"}; return f }()},
		{"eq-unknown-column", func() SourcePerfFilter { f := base(); f.Eq = map[string]string{"email": "a@b.com"}; return f }()},
	}
	for _, tc := range bad {
		if _, err := buildAudienceSourcePerfSQL(tc.f); err == nil {
			t.Errorf("%s: expected validation error, got nil", tc.name)
		}
	}
}

func TestBuildAudienceFirstTouchSQL(t *testing.T) {
	got, err := buildAudienceFirstTouchSQL("2026-05-26", "2026-06-08")
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	want := "SELECT first_dt, COUNT(*) c FROM (" +
		"SELECT (CASE WHEN subscriber_id <> '' THEN subscriber_id ELSE lower(email) END) k, MIN(dt) AS first_dt" +
		" FROM email_events WHERE event_type IN ('attempted','delivered','relayed_to_ses') GROUP BY 1" +
		") WHERE first_dt BETWEEN '2026-05-26' AND '2026-06-08' GROUP BY 1 ORDER BY 1"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}

	if _, err := buildAudienceFirstTouchSQL("", "2026-06-08"); err == nil {
		t.Errorf("expected rejection of missing from")
	}
	if _, err := buildAudienceFirstTouchSQL("2026-05-26", "2026'; DROP"); err == nil {
		t.Errorf("expected rejection of injection-shaped to")
	}
	if _, err := buildAudienceFirstTouchSQL("2026-06-09", "2026-06-01"); err == nil {
		t.Errorf("expected rejection of inverted range")
	}
}

func TestNormalizeEmail(t *testing.T) {
	// Valid emails are accepted and lowercased.
	got, err := normalizeEmail("First.Last+Tag@Example.COM")
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if got != "first.last+tag@example.com" {
		t.Errorf("normalizeEmail lowercasing: got %q", got)
	}

	bad := []string{
		"",
		"a'b@c.com",            // quote
		"a@b.com' OR '1'='1",   // injection
		"a@b.com; DROP TABLE",  // semicolon/space
		"no-at-sign",           // not an address
		strings.Repeat("a", 320) + "@b.com", // over 320 chars
	}
	for _, e := range bad {
		if _, err := normalizeEmail(e); err == nil {
			t.Errorf("normalizeEmail(%q): expected error, got nil", e)
		}
	}
}

func TestBuildAudienceMemberProfileSQL(t *testing.T) {
	got, err := buildAudienceMemberProfileSQL("2026-06-08", "A@B.com")
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	want := "SELECT " + audienceColumns + " FROM audience" +
		" WHERE dt = '2026-06-08' AND email = 'a@b.com' LIMIT 20"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}

	if _, err := buildAudienceMemberProfileSQL("", "a@b.com"); err == nil {
		t.Errorf("expected rejection of missing dt")
	}
	if _, err := buildAudienceMemberProfileSQL("2026-06-08", "a'b@c.com"); err == nil {
		t.Errorf("expected rejection of email with quote")
	}
}

func TestBuildAudienceMemberEventsSQL(t *testing.T) {
	const eventCols = "event_uid, recipient_send_id, campaign_id, subscriber_id, email, " +
		"email_domain, brand, isp_group, route_type, event_type, suppression_reason, " +
		"vmta, pool, bounce_cat, dsn_code, dsn_diag, link_url, source_ip, variant, " +
		"event_at, event_epoch_ms, ingested_at, source, dt"

	// No ids: email-only predicate.
	got, err := buildAudienceMemberEventsSQL("A@B.com", nil, "2026-03-11", 0)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	want := "SELECT " + eventCols + " FROM email_events" +
		" WHERE (lower(email) = 'a@b.com') AND dt >= '2026-03-11'" +
		" ORDER BY event_epoch_ms DESC LIMIT 200"
	if got != want {
		t.Errorf("no-ids:\n got  %s\n want %s", got, want)
	}

	// With ids: only well-formed UUIDs reach the IN list (re-validated for
	// defense in depth — the ids are lake data, not caller input), dupes
	// collapse, and the limit clamps to 500.
	ids := []string{
		"550e8400-e29b-41d4-a716-446655440000",
		"550e8400-e29b-41d4-a716-446655440000", // duplicate
		"not-a-uuid'; DROP TABLE email_events;--",
		"",
		"6ba7b810-9dad-11d1-80b4-00c04fd430c8",
	}
	got, err = buildAudienceMemberEventsSQL("a@b.com", ids, "2026-03-11", 9999)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	want = "SELECT " + eventCols + " FROM email_events" +
		" WHERE (lower(email) = 'a@b.com' OR subscriber_id IN" +
		" ('550e8400-e29b-41d4-a716-446655440000', '6ba7b810-9dad-11d1-80b4-00c04fd430c8'))" +
		" AND dt >= '2026-03-11' ORDER BY event_epoch_ms DESC LIMIT 500"
	if got != want {
		t.Errorf("with-ids:\n got  %s\n want %s", got, want)
	}
	if strings.Contains(got, "DROP") {
		t.Errorf("malformed id leaked into SQL: %s", got)
	}

	if _, err := buildAudienceMemberEventsSQL("a'b@c.com", nil, "2026-03-11", 0); err == nil {
		t.Errorf("expected rejection of email with quote")
	}
	if _, err := buildAudienceMemberEventsSQL("a@b.com", nil, "bad", 0); err == nil {
		t.Errorf("expected rejection of malformed min dt")
	}
}

func TestScanAudienceProfile(t *testing.T) {
	row := []string{
		"550e8400-e29b-41d4-a716-446655440000", // id
		"6ba7b810-9dad-11d1-80b4-00c04fd430c8", // organization_id
		"6ba7b811-9dad-11d1-80b4-00c04fd430c8", // list_id
		"a@b.com", "b.com", "gmail", "active",
		"warmup-data/2026/3M_Free_090123.csv", "jvc-warmup", "csv-import", "detail",
		"2026-01-01T00:00:00Z", "2026-01-01", "2026-01-02T00:00:00Z", "2026-01-02T00:00:01Z",
		"2026-01-02T00:00:02Z", "", "", "",
		"", "", // churned_dt, churn_reason
		"72.50", "hot",
		"12", "3", "40",
		"2026-06-01T00:00:00Z", "2026-06-02T00:00:00Z", "2026-06-03T00:00:00Z",
		"0.91", "verified", "false", "0.12",
	}
	p := scanAudienceProfile(row)
	if p.ID != "550e8400-e29b-41d4-a716-446655440000" || p.Email != "a@b.com" ||
		p.ISP != "gmail" || p.Source != "warmup-data/2026/3M_Free_090123.csv" ||
		p.EngagementScore != 72.50 || p.EngagementBand != "hot" ||
		p.TotalOpens != 12 || p.TotalClicks != 3 || p.TotalEmailsReceived != 40 ||
		p.DataQualityScore != 0.91 || p.IsBot != false || p.ChurnRiskScore != 0.12 ||
		p.VerificationStatus != "verified" || p.ChurnedDt != "" {
		t.Fatalf("scanAudienceProfile mapped columns incorrectly: %+v", p)
	}

	// Short row must not panic and yields zero values.
	p = scanAudienceProfile([]string{"only-id"})
	if p.ID != "only-id" || p.EngagementScore != 0 || p.IsBot {
		t.Fatalf("scanAudienceProfile short row unexpected: %+v", p)
	}
}

func TestClampSourcePerfLimit(t *testing.T) {
	cases := map[int]int{-1: 2000, 0: 2000, 1: 1, 2000: 2000, 5000: 5000, 99999: 5000}
	for in, want := range cases {
		if got := clampSourcePerfLimit(in); got != want {
			t.Errorf("clampSourcePerfLimit(%d)=%d want %d", in, got, want)
		}
		if got := ClampSourcePerfLimit(in); got != want {
			t.Errorf("ClampSourcePerfLimit(%d)=%d want %d", in, got, want)
		}
	}
}

func TestClampMemberEventsLimit(t *testing.T) {
	cases := map[int]int{-1: 200, 0: 200, 1: 1, 200: 200, 500: 500, 9999: 500}
	for in, want := range cases {
		if got := clampMemberEventsLimit(in); got != want {
			t.Errorf("clampMemberEventsLimit(%d)=%d want %d", in, got, want)
		}
	}
}

func TestAudienceWrappersDisabled(t *testing.T) {
	resetReader()
	ctx := context.Background()

	if _, _, err := AudienceLatestDt(ctx); !IsDisabledErr(err) {
		t.Errorf("AudienceLatestDt: expected disabled sentinel, got %v", err)
	}
	if _, err := AudienceBreakdown(ctx, AudienceBreakdownFilter{Dt: "2026-06-08", GroupBy: []string{"status"}}); !IsDisabledErr(err) {
		t.Errorf("AudienceBreakdown: expected disabled sentinel, got %v", err)
	}
	if _, err := AudienceSourcePerformance(ctx, SourcePerfFilter{AudienceDt: "2026-06-08", From: "2026-06-02", To: "2026-06-08", Dim: "source"}); !IsDisabledErr(err) {
		t.Errorf("AudienceSourcePerformance: expected disabled sentinel, got %v", err)
	}
	if _, err := AudienceFirstTouch(ctx, "2026-05-26", "2026-06-08"); !IsDisabledErr(err) {
		t.Errorf("AudienceFirstTouch: expected disabled sentinel, got %v", err)
	}
	if _, _, err := AudienceMember(ctx, "2026-06-08", "a@b.com", 0); !IsDisabledErr(err) {
		t.Errorf("AudienceMember: expected disabled sentinel, got %v", err)
	}
}
