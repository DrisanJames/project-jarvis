package analytics

import (
	"strings"
	"testing"
)

// --- spec validation ---

func TestValidateSegmentSpecAcceptsCanonicalSpecs(t *testing.T) {
	good := []SegmentSpec{
		{Event: "open", WindowDays: 7, Scope: "global"},
		{Event: "click", WindowDays: 30, Scope: "brand", BrandApex: "discountblog.com"},
		{Event: "open", WindowDays: 14, Scope: "isp", ISP: "gmail"},
		{Event: "open", WindowDays: 14, Scope: "global", ExcludeSeeds: true,
			SeedListIDs: []string{"11111111-2222-3333-4444-555555555555"}},
	}
	for _, spec := range good {
		if err := ValidateSegmentSpec(spec); err != nil {
			t.Errorf("spec %+v: unexpected error %v", spec, err)
		}
	}
}

func TestValidateSegmentSpecRejections(t *testing.T) {
	cases := []struct {
		name string
		spec SegmentSpec
	}{
		{"unknown event", SegmentSpec{Event: "delivered", Scope: "global"}},
		{"empty event", SegmentSpec{Event: "", Scope: "global"}},
		{"injection-shaped event", SegmentSpec{Event: "open' OR '1'='1", Scope: "global"}},
		{"unknown scope", SegmentSpec{Event: "open", Scope: "tenant"}},
		{"empty scope", SegmentSpec{Event: "open", Scope: ""}},
		{"brand scope without apex", SegmentSpec{Event: "open", Scope: "brand"}},
		{"injection-shaped brand", SegmentSpec{Event: "open", Scope: "brand", BrandApex: "x.com' OR dt='"}},
		{"brand with quote", SegmentSpec{Event: "open", Scope: "brand", BrandApex: "foo'bar.com"}},
		{"isp scope without isp", SegmentSpec{Event: "open", Scope: "isp"}},
		{"injection-shaped isp", SegmentSpec{Event: "open", Scope: "isp", ISP: "gmail'; DROP TABLE audience;--"}},
		{"isp with dot (tokenRe, not dottedRe)", SegmentSpec{Event: "open", Scope: "isp", ISP: "gmail.com"}},
		{"non-uuid seed list id", SegmentSpec{Event: "open", Scope: "global",
			SeedListIDs: []string{"not-a-uuid"}}},
		{"injection-shaped seed list id", SegmentSpec{Event: "open", Scope: "global",
			SeedListIDs: []string{"') OR ('1'='1"}}},
	}
	for _, tc := range cases {
		if err := ValidateSegmentSpec(tc.spec); err == nil {
			t.Errorf("%s: expected rejection, got nil", tc.name)
		}
	}
}

func TestClampSegmentWindowDays(t *testing.T) {
	cases := []struct{ in, want int }{
		{-5, 1}, {0, 1}, {1, 1}, {7, 7}, {120, 120}, {121, 120}, {9999, 120},
	}
	for _, tc := range cases {
		if got := ClampSegmentWindowDays(tc.in); got != tc.want {
			t.Errorf("ClampSegmentWindowDays(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// --- exact SQL shape ---

func TestBuildSegmentMembersSQLGlobalOpen(t *testing.T) {
	spec := SegmentSpec{Event: "open", WindowDays: 14, Scope: "global"}
	got, err := buildSegmentMembersSQL(spec, "2026-06-08", "2026-05-26", "2026-06-09")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "WITH aud AS (SELECT id, email FROM audience WHERE dt = '2026-06-08' AND status = 'confirmed')" +
		", keyed AS (SELECT join_key, max(id) AS sub_id, max(email) AS email FROM (" +
		"SELECT id AS join_key, id, email FROM aud UNION ALL SELECT email AS join_key, id, email FROM aud" +
		") GROUP BY join_key)" +
		" SELECT DISTINCT k.sub_id, k.email FROM email_events e" +
		" JOIN keyed k ON (CASE WHEN e.subscriber_id <> '' THEN e.subscriber_id ELSE lower(e.email) END) = k.join_key" +
		" WHERE e.dt BETWEEN '2026-05-26' AND '2026-06-09'" +
		" AND e.event_type = 'open'" +
		" AND e.source IN ('app','ses')" +
		" LIMIT 1000001"
	if got != want {
		t.Errorf("SQL mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildSegmentMembersSQLBrandClick(t *testing.T) {
	spec := SegmentSpec{Event: "click", WindowDays: 7, Scope: "brand", BrandApex: "discountblog.com"}
	got, err := buildSegmentMembersSQL(spec, "2026-06-08", "2026-06-02", "2026-06-09")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "WITH aud AS (SELECT id, email FROM audience WHERE dt = '2026-06-08' AND status = 'confirmed')" +
		", keyed AS (SELECT join_key, max(id) AS sub_id, max(email) AS email FROM (" +
		"SELECT id AS join_key, id, email FROM aud UNION ALL SELECT email AS join_key, id, email FROM aud" +
		") GROUP BY join_key)" +
		" SELECT DISTINCT k.sub_id, k.email FROM email_events e" +
		" JOIN keyed k ON (CASE WHEN e.subscriber_id <> '' THEN e.subscriber_id ELSE lower(e.email) END) = k.join_key" +
		" WHERE e.dt BETWEEN '2026-06-02' AND '2026-06-09'" +
		" AND e.event_type = 'click'" +
		" AND e.source IN ('app','ses')" +
		" AND e.brand = 'discountblog.com'" +
		" LIMIT 1000001"
	if got != want {
		t.Errorf("SQL mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildSegmentMembersSQLISPWithSeedExclusion(t *testing.T) {
	spec := SegmentSpec{
		Event: "open", WindowDays: 30, Scope: "isp", ISP: "gmail",
		ExcludeSeeds: true,
		// Deliberately unsorted — the builder must render them sorted.
		SeedListIDs: []string{
			"bbbbbbbb-2222-3333-4444-555555555555",
			"aaaaaaaa-2222-3333-4444-555555555555",
		},
	}
	got, err := buildSegmentMembersSQL(spec, "2026-06-08", "2026-05-10", "2026-06-09")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "WITH aud AS (SELECT id, email FROM audience WHERE dt = '2026-06-08' AND status = 'confirmed'" +
		" AND isp = 'gmail'" +
		" AND list_id NOT IN ('aaaaaaaa-2222-3333-4444-555555555555', 'bbbbbbbb-2222-3333-4444-555555555555'))" +
		", keyed AS (SELECT join_key, max(id) AS sub_id, max(email) AS email FROM (" +
		"SELECT id AS join_key, id, email FROM aud UNION ALL SELECT email AS join_key, id, email FROM aud" +
		") GROUP BY join_key)" +
		" SELECT DISTINCT k.sub_id, k.email FROM email_events e" +
		" JOIN keyed k ON (CASE WHEN e.subscriber_id <> '' THEN e.subscriber_id ELSE lower(e.email) END) = k.join_key" +
		" WHERE e.dt BETWEEN '2026-05-10' AND '2026-06-09'" +
		" AND e.event_type = 'open'" +
		" AND e.source IN ('app','ses')" +
		" LIMIT 1000001"
	if got != want {
		t.Errorf("SQL mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildSegmentMembersSQLExcludeSeedsWithoutIDsRendersNoPredicate(t *testing.T) {
	spec := SegmentSpec{Event: "open", WindowDays: 7, Scope: "global", ExcludeSeeds: true}
	got, err := buildSegmentMembersSQL(spec, "2026-06-08", "2026-06-02", "2026-06-09")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(got, "list_id") {
		t.Errorf("expected no list_id predicate when SeedListIDs is empty, got: %s", got)
	}
}

func TestBuildSegmentMembersSQLDateValidation(t *testing.T) {
	spec := SegmentSpec{Event: "open", WindowDays: 7, Scope: "global"}
	cases := []struct {
		name          string
		adt, from, to string
	}{
		{"bad audience dt", "06/08/2026", "2026-06-02", "2026-06-09"},
		{"empty audience dt", "", "2026-06-02", "2026-06-09"},
		{"injection-shaped from", "2026-06-08", "2026-06-02' OR '1'='1", "2026-06-09"},
		{"bad to", "2026-06-08", "2026-06-02", "tomorrow"},
		{"inverted range", "2026-06-08", "2026-06-09", "2026-06-02"},
	}
	for _, tc := range cases {
		if _, err := buildSegmentMembersSQL(spec, tc.adt, tc.from, tc.to); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}
}

func TestBuildSegmentMembersSQLRejectsInvalidSpec(t *testing.T) {
	if _, err := buildSegmentMembersSQL(
		SegmentSpec{Event: "bounce", WindowDays: 7, Scope: "global"},
		"2026-06-08", "2026-06-02", "2026-06-09"); err == nil {
		t.Error("expected invalid event to be rejected before SQL construction")
	}
}

func TestBuildSegmentMembersDisabledReader(t *testing.T) {
	// The global reader is not configured in unit tests; the package-level
	// wrapper must return the errDisabled sentinel, same as every other lake
	// read entrypoint.
	if ReaderEnabled() {
		t.Skip("reader unexpectedly enabled in test environment")
	}
	_, err := BuildSegmentMembers(t.Context(), SegmentSpec{Event: "open", WindowDays: 7, Scope: "global"})
	if !IsDisabledErr(err) {
		t.Errorf("expected errDisabled, got %v", err)
	}
}
