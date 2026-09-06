package main

// =============================================================================
// ignite_ip_classification / ignite_ip_class() regression suite (2026-09-05).
//
// WHAT THIS PROTECTS. The whole unit exists because ignite_datacenter_ranges
// carries blanket ownership /16s (135.232.0.0/16, 74.179.0.0/16) and the
// verdict path reads "owned by a cloud provider" as "machine". The behaviour
// table lifts observed /32s out of those /16s by NARROWEST MATCH WINS. If that
// resolution regresses, the 18 'unresolved' /32s in the seed collapse back into
// their hosting /16 and we are once again classifying human-carrying addresses
// as datacenter. That single property is the reason this file exists.
//
// FOUR VALUES, PROVEN ON PROD INSIDE BEGIN/ROLLBACK, PINNED HERE:
//     ignite_ip_class('135.232.20.148') = 'scanner'      /32 inside a hosting /16
//     ignite_ip_class('135.232.20.64')  = 'unresolved'   /32 inside a hosting /16
//     ignite_ip_class('135.232.99.99')  = 'hosting'      no /32; falls back to /16
//     ignite_ip_class('8.8.8.8')        = NULL           no row at all
//
// HOW IT TESTS WITHOUT A DATABASE. The resolution semantics live in SQL, so the
// tests cannot call the function. Instead they PARSE the committed accessor
// body (igniteIPClassFnDDL — the exact string runStartupMigrations installs)
// into an ipClassResolution, and evaluate the fixture through whatever that
// parse says. The ORDER BY direction the resolver uses is READ OUT OF THE SQL,
// never hardcoded — so editing DESC to ASC in ip_classification.go, or deleting
// the ORDER BY, changes what these tests compute and the four pins above break.
// TestIPClassResolutionNegativeControl proves exactly that on every run by
// applying those two mutations itself and asserting the suite goes red.
//
// This is the same idiom as datacenter_classifier_test.go (structural pins on
// the committed body + a transcription evaluated against scenarios), extended
// so the transcription is DERIVED from the body rather than written alongside
// it. The authority on real Postgres semantics is the sibling integration test
// (ip_classification_integration_test.go, -tags integration), which executes
// these same constants against a live PG 16 inside BEGIN/ROLLBACK. Neither file
// skips: this one always runs, that one is opted into by build tag.
//
// SCOPE. ignite_ip_is_datacenter() is NOT retested here — it is untouched by
// this unit and feeds the live click_verdict trigger. It appears only in
// TestIgniteIPIsDatacenterUnchangedByThisUnit, which asserts it stayed put.
// =============================================================================

import (
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// Parsing the committed accessor into executable resolution semantics
// -----------------------------------------------------------------------------

// ipClassResolution is what the accessor body says about how a match is
// chosen. Everything the resolver below does is driven by these fields, so a
// change to the SQL changes the computed answers rather than merely failing a
// string match.
type ipClassResolution struct {
	hasOrderBy    bool // ORDER BY masklen(cidr) present at all
	orderDesc     bool // ...DESC. Absent direction means ASC (Postgres default)
	hasLimit1     bool
	filtersActive bool // WHERE ... is_active
	filtersMaxAge bool // WHERE ... max_age IS NULL OR last_confirmed_at > now() - max_age
}

var (
	// Direction group is optional on purpose: `ORDER BY masklen(c.cidr)` with
	// the DESC deleted is valid SQL that silently resolves BROADEST-first.
	// That regression must be representable, not a parse error.
	reIPClassOrderBy = regexp.MustCompile(`(?is)ORDER\s+BY\s+masklen\s*\(\s*(?:[a-z0-9_]+\.)?cidr\s*\)\s*(DESC|ASC)?`)
	reIPClassLimit1  = regexp.MustCompile(`(?is)\bLIMIT\s+1\b`)
	reIPClassMaxAge  = regexp.MustCompile(`(?is)max_age\s+IS\s+NULL\s+OR\s+[a-z0-9_.]*last_confirmed_at\s*>`)
	reIPClassActive  = regexp.MustCompile(`(?is)\b(?:[a-z0-9_]+\.)?is_active\b`)
)

func parseIPClassResolution(ddl string) ipClassResolution {
	var r ipClassResolution
	if m := reIPClassOrderBy.FindStringSubmatch(ddl); m != nil {
		r.hasOrderBy = true
		r.orderDesc = strings.EqualFold(m[1], "DESC")
	}
	r.hasLimit1 = reIPClassLimit1.MatchString(ddl)
	r.filtersActive = reIPClassActive.MatchString(ddl)
	r.filtersMaxAge = reIPClassMaxAge.MatchString(ddl)
	return r
}

// -----------------------------------------------------------------------------
// The fixture table + the resolver
// -----------------------------------------------------------------------------

// ipClassRow mirrors one ignite_ip_classification row. Rows are held in SEED
// INSERT ORDER, which matters: with no ORDER BY, an unordered scan of a small
// table returns physical order, so the hosting /16 (seeded first, from
// ignite_datacenter_ranges) wins — the exact shape of the bug being guarded.
type ipClassRow struct {
	cidr            string
	class           string
	isActive        bool
	lastConfirmedAt time.Time
}

// ipClassFixture mirrors the committed seed's ORDER and content for the two
// /16s the four pinned prod values live inside:
//   - statement 1 seeds every ignite_datacenter_ranges row as 'hosting'
//   - statement 2 seeds the measured scanner /32s
//   - statement 3 seeds the mixed-traffic 'unresolved' /32s
func ipClassFixture(now time.Time) []ipClassRow {
	return []ipClassRow{
		{"135.232.0.0/16", "hosting", true, now.Add(-1 * time.Hour)},
		{"74.179.0.0/16", "hosting", true, now.Add(-1 * time.Hour)},
		{"135.232.20.148/32", "scanner", true, now.Add(-1 * time.Hour)},
		{"135.232.20.64/32", "unresolved", true, now.Add(-1 * time.Hour)},
	}
}

// resolveIPClass evaluates the fixture the way the parsed accessor says to.
// Returns ("", false) for the SQL NULL case (no row matched).
//
// maxAge <= 0 models a NULL max_age argument.
func resolveIPClass(res ipClassResolution, rows []ipClassRow, ip string, maxAge time.Duration, now time.Time) (string, bool) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		panic("test fixture: bad ip " + ip)
	}

	type cand struct {
		row     ipClassRow
		masklen int
		seq     int
	}
	var matched []cand
	for i, row := range rows {
		p := netip.MustParsePrefix(row.cidr)
		if !p.Contains(addr) {
			continue
		}
		if res.filtersActive && !row.isActive {
			continue
		}
		if res.filtersMaxAge && maxAge > 0 && !row.lastConfirmedAt.After(now.Add(-maxAge)) {
			continue
		}
		matched = append(matched, cand{row, p.Bits(), i})
	}
	if len(matched) == 0 {
		return "", false
	}

	if res.hasOrderBy {
		sort.SliceStable(matched, func(a, b int) bool {
			if res.orderDesc {
				return matched[a].masklen > matched[b].masklen
			}
			return matched[a].masklen < matched[b].masklen
		})
	}
	// No ORDER BY: leave in physical/seed order — what an unordered scan of a
	// small table actually returns. LIMIT 1 (or a scalar SQL function's first
	// row) then takes whichever row that is.
	return matched[0].row.class, true
}

// -----------------------------------------------------------------------------
// The behaviour pins
// -----------------------------------------------------------------------------

type ipClassCase struct {
	name   string
	ip     string
	maxAge time.Duration
	want   string // "" means SQL NULL
	why    string
}

// ipClassPinnedCases — the four prod-proven values plus the max_age contract.
// runIPClassCases is shared with the negative control, which is the point:
// the mutation test re-runs THESE cases, so it cannot drift from them.
func ipClassPinnedCases() []ipClassCase {
	return []ipClassCase{
		{
			name: "narrow /32 scanner beats the hosting /16 that contains it",
			ip:   "135.232.20.148", want: "scanner",
			why: "THE point of the unit: a broad ownership row and a narrow behaviour row coexist and the narrow one wins",
		},
		{
			name: "narrow /32 unresolved beats the hosting /16 that contains it",
			ip:   "135.232.20.64", want: "unresolved",
			why: "'unresolved' is a real answer for mixed traffic, not a placeholder — it must survive resolution",
		},
		{
			name: "no /32 row falls back to the containing /16",
			ip:   "135.232.99.99", want: "hosting",
			why: "narrowest-match must still MATCH when only the broad row exists",
		},
		{
			name: "unclassified address returns NULL",
			ip:   "8.8.8.8", want: "",
			why: "NULL is 'no row', distinct from the 'unknown' class which asserts we looked",
		},
		{
			name: "NULL max_age matches regardless of age",
			ip:   "135.232.20.148", maxAge: 0, want: "scanner",
			why: "max_age IS NULL short-circuits the freshness predicate",
		},
	}
}

func runIPClassCases(t *testing.T, res ipClassResolution, cases []ipClassCase) []string {
	t.Helper()
	now := time.Now()
	var failures []string
	for _, c := range cases {
		got, ok := resolveIPClass(res, ipClassFixture(now), c.ip, c.maxAge, now)
		gotStr := "NULL"
		if ok {
			gotStr = got
		}
		wantStr := "NULL"
		if c.want != "" {
			wantStr = c.want
		}
		if gotStr != wantStr {
			failures = append(failures, fmt.Sprintf("ignite_ip_class(%s) = %s, want %s — %s", c.ip, gotStr, wantStr, c.why))
		}
	}
	return failures
}

// TestIPClassNarrowestMatchWins is the load-bearing test. It evaluates the
// committed accessor body against the four prod-proven values.
func TestIPClassNarrowestMatchWins(t *testing.T) {
	res := parseIPClassResolution(igniteIPClassFnDDL)
	if !res.hasOrderBy {
		t.Fatalf("igniteIPClassFnDDL has no ORDER BY masklen(cidr): resolution is non-deterministic and a /32 no longer reliably beats its /16")
	}
	if !res.orderDesc {
		t.Fatalf("igniteIPClassFnDDL orders masklen ASCENDING: the BROADEST prefix wins, which is the datacenter-misclassification bug this table exists to fix")
	}
	if !res.hasLimit1 {
		t.Fatalf("igniteIPClassFnDDL has no LIMIT 1: the accessor returns text, so a multi-row result errors at runtime")
	}

	for _, c := range ipClassPinnedCases() {
		t.Run(c.name, func(t *testing.T) {
			if fails := runIPClassCases(t, res, []ipClassCase{c}); len(fails) > 0 {
				t.Fatal(fails[0])
			}
		})
	}
}

// TestIPClassResolutionNegativeControl is the proof that the test above is
// sensitive to the bug it guards. It applies the two realistic regressions to
// the committed body and asserts the pinned cases GO RED under each. A test
// that survives its own bug is worthless; this one refuses to.
func TestIPClassResolutionNegativeControl(t *testing.T) {
	mutations := []struct {
		name     string
		mutate   func(string) string
		wantFail string // substring that must appear in the induced failure
	}{
		{
			name:     "DESC flipped to ASC (broadest prefix wins)",
			mutate:   func(s string) string { return strings.Replace(s, "masklen(c.cidr) DESC", "masklen(c.cidr) ASC", 1) },
			wantFail: "ignite_ip_class(135.232.20.148) = hosting, want scanner",
		},
		{
			name:     "ORDER BY dropped entirely (unordered scan)",
			mutate:   func(s string) string { return strings.Replace(s, "ORDER BY masklen(c.cidr) DESC ", "", 1) },
			wantFail: "ignite_ip_class(135.232.20.148) = hosting, want scanner",
		},
		{
			name:     "DESC keyword deleted (SQL default ASC)",
			mutate:   func(s string) string { return strings.Replace(s, "masklen(c.cidr) DESC", "masklen(c.cidr)", 1) },
			wantFail: "ignite_ip_class(135.232.20.148) = hosting, want scanner",
		},
	}

	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			broken := m.mutate(igniteIPClassFnDDL)
			if broken == igniteIPClassFnDDL {
				t.Fatalf("mutation %q did not change the body — the accessor's ORDER BY was rewritten; update this negative control so it keeps biting", m.name)
			}
			failures := runIPClassCases(t, parseIPClassResolution(broken), ipClassPinnedCases())
			if len(failures) == 0 {
				t.Fatalf("NEGATIVE CONTROL FAILED: the pinned cases still PASS against a broken resolution (%s). These tests do not guard narrowest-match.", m.name)
			}
			found := false
			for _, f := range failures {
				if strings.Contains(f, m.wantFail) {
					found = true
				}
			}
			if !found {
				t.Fatalf("negative control tripped, but not on the narrowest-match case.\nwant a failure containing: %s\ngot: %v", m.wantFail, failures)
			}
			t.Logf("negative control OK — broken resolution produces: %v", failures)
		})
	}
}

// TestIPClassIsActiveExcludesRow: is_active=false removes a row from
// resolution. Asserted through the FALLBACK, not merely "not scanner": with
// the /32 retired, the address must resolve to the /16 that still contains it.
func TestIPClassIsActiveExcludesRow(t *testing.T) {
	res := parseIPClassResolution(igniteIPClassFnDDL)
	if !res.filtersActive {
		t.Fatalf("igniteIPClassFnDDL does not filter on is_active: retiring a row would have no effect, and is_active-instead-of-DELETE is the whole retirement mechanism")
	}
	now := time.Now()
	rows := ipClassFixture(now)
	for i := range rows {
		if rows[i].cidr == "135.232.20.148/32" {
			rows[i].isActive = false
		}
	}
	got, ok := resolveIPClass(res, rows, "135.232.20.148", 0, now)
	if !ok || got != "hosting" {
		t.Fatalf("with the scanner /32 retired, ignite_ip_class('135.232.20.148') = %q (matched=%v), want \"hosting\" — an inactive row must be skipped and resolution must fall through to the /16", got, ok)
	}
}

// TestIPClassMaxAgeExcludesStaleRow: max_age drops a row whose
// last_confirmed_at is older than the interval; NULL max_age ignores age.
func TestIPClassMaxAgeExcludesStaleRow(t *testing.T) {
	res := parseIPClassResolution(igniteIPClassFnDDL)
	if !res.filtersMaxAge {
		t.Fatalf("igniteIPClassFnDDL has no `max_age IS NULL OR last_confirmed_at > now() - max_age` predicate: the freshness argument is inert")
	}
	now := time.Now()
	rows := ipClassFixture(now)
	for i := range rows {
		if rows[i].cidr == "135.232.20.148/32" {
			rows[i].lastConfirmedAt = now.Add(-40 * 24 * time.Hour) // stale
		}
	}

	got, ok := resolveIPClass(res, rows, "135.232.20.148", 30*24*time.Hour, now)
	if !ok || got != "hosting" {
		t.Fatalf("max_age=30d against a 40d-old /32: got %q (matched=%v), want \"hosting\" — the stale narrow row must drop out and the fresh /16 must answer", got, ok)
	}

	got, ok = resolveIPClass(res, rows, "135.232.20.148", 0, now)
	if !ok || got != "scanner" {
		t.Fatalf("NULL max_age against the same 40d-old /32: got %q (matched=%v), want \"scanner\" — NULL max_age must match regardless of age", got, ok)
	}

	got, ok = resolveIPClass(res, rows, "135.232.20.148", 60*24*time.Hour, now)
	if !ok || got != "scanner" {
		t.Fatalf("max_age=60d against a 40d-old /32: got %q (matched=%v), want \"scanner\" — a row inside the window must still win", got, ok)
	}
}

// -----------------------------------------------------------------------------
// Schema contract
// -----------------------------------------------------------------------------

// TestIPClassificationClassCheckConstraint pins the CHECK membership EXACTLY.
// A missing member silently rejects a legitimate curation write at runtime; an
// extra member lets an unhandled class into every consumer.
func TestIPClassificationClassCheckConstraint(t *testing.T) {
	wantClasses := []string{"scanner", "hosting", "vpn-or-proxy", "residential-or-mobile", "unresolved", "unknown"}
	wantConfidence := []string{"confirmed", "probable", "assumed"}

	classIn := extractCheckINList(t, igniteIPClassificationDDL, "class IN (")
	if !equalStringSets(classIn, wantClasses) {
		t.Fatalf("class CHECK membership = %v, want exactly %v", classIn, wantClasses)
	}
	confIn := extractCheckINList(t, igniteIPClassificationDDL, "confidence IN (")
	if !equalStringSets(confIn, wantConfidence) {
		t.Fatalf("confidence CHECK membership = %v, want exactly %v", confIn, wantConfidence)
	}

	// The constraint must be a CHECK — a comment mentioning the classes is not
	// enforcement (this repo's inert-Gate-F precedent).
	if !strings.Contains(igniteIPClassificationDDL, "CHECK (class IN (") {
		t.Fatalf("class membership is not enforced by a CHECK constraint")
	}
	if !strings.Contains(igniteIPClassificationDDL, "CHECK (confidence IN (") {
		t.Fatalf("confidence membership is not enforced by a CHECK constraint")
	}

	// A bogus class must not be a member. This is the unit-side half of the
	// rejection proof; the integration test makes Postgres actually refuse it.
	for _, bogus := range []string{"datacenter", "human", "bot", "machine"} {
		for _, c := range classIn {
			if c == bogus {
				t.Fatalf("class CHECK admits %q — the verdict vocabulary must not leak into the behaviour vocabulary", bogus)
			}
		}
	}
}

// TestIPClassificationTableShape pins the columns and the two index/key
// structures resolution depends on.
func TestIPClassificationTableShape(t *testing.T) {
	for _, col := range []string{
		"cidr", "class", "attributes", "confidence", "evidence_source",
		"evidence", "last_confirmed_at", "first_seen", "updated_at",
		"is_active", "note",
	} {
		if !strings.Contains(igniteIPClassificationDDL, col) {
			t.Errorf("ignite_ip_classification is missing column %q", col)
		}
	}
	if !strings.Contains(igniteIPClassificationDDL, "cidr              cidr PRIMARY KEY") &&
		!regexp.MustCompile(`(?is)\bcidr\s+cidr\s+PRIMARY\s+KEY`).MatchString(igniteIPClassificationDDL) {
		t.Errorf("cidr must be `cidr PRIMARY KEY` — the seed's ON CONFLICT (cidr) DO NOTHING has no arbiter without it, and every boot would error instead of no-opping")
	}
	if !regexp.MustCompile(`(?is)attributes\s+text\[\]`).MatchString(igniteIPClassificationDDL) {
		t.Errorf("attributes must be text[]")
	}
	if !regexp.MustCompile(`(?is)evidence\s+jsonb`).MatchString(igniteIPClassificationDDL) {
		t.Errorf("evidence must be jsonb")
	}
	if !strings.Contains(igniteIPClassificationDDL, "CREATE TABLE IF NOT EXISTS") {
		t.Errorf("CREATE TABLE without IF NOT EXISTS — the second boot errors and the entry is lost")
	}

	// GiST over inet_ops is what drives the <<= containment scan; the cidr
	// PRIMARY KEY's btree cannot serve it.
	if !regexp.MustCompile(`(?is)USING\s+gist\s*\(\s*cidr\s+inet_ops\s*\)`).MatchString(igniteIPClassificationGistDDL) {
		t.Errorf("GiST index must be USING gist (cidr inet_ops); got %q", igniteIPClassificationGistDDL)
	}
	if !strings.Contains(igniteIPClassificationGistDDL, "CREATE INDEX IF NOT EXISTS") {
		t.Errorf("GiST index DDL missing IF NOT EXISTS")
	}
	if !strings.Contains(igniteIPClassificationGistDDL, "ignite_ip_classification") {
		t.Errorf("GiST index is not on ignite_ip_classification")
	}
}

// -----------------------------------------------------------------------------
// Seed contract — the two properties an operator's curation depends on
// -----------------------------------------------------------------------------

// TestIPClassificationSeedNeverDerivesScannerFromOwnership: every row migrated
// from ignite_datacenter_ranges lands as 'hosting'. That table proves who OWNS
// a range, never what it DOES; deriving 'scanner' from it would reinstate the
// exact misclassification the unit removes.
func TestIPClassificationSeedNeverDerivesScannerFromOwnership(t *testing.T) {
	stmts := splitSQLStatements(igniteIPClassificationSeedDDL)
	var fromRanges []string
	for _, s := range stmts {
		if regexp.MustCompile(`(?is)FROM\s+ignite_datacenter_ranges`).MatchString(s) {
			fromRanges = append(fromRanges, s)
		}
	}
	if len(fromRanges) == 0 {
		t.Fatalf("no seed statement migrates ignite_datacenter_ranges into ignite_ip_classification")
	}
	for _, s := range fromRanges {
		body := stripSQLLineComments(s)
		if !strings.Contains(body, "'hosting'") {
			t.Errorf("the ignite_datacenter_ranges migration does not project class 'hosting':\n%s", body)
		}
		if strings.Contains(body, "'scanner'") {
			t.Fatalf("the ignite_datacenter_ranges migration can emit 'scanner'. Ownership is never behaviour — this is the regression that reinstates classifying human-carrying addresses as datacenter:\n%s", body)
		}
		for _, forbidden := range []string{"'residential-or-mobile'", "'vpn-or-proxy'"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("the ignite_datacenter_ranges migration emits %s — ownership establishes only 'hosting':\n%s", forbidden, body)
			}
		}
	}
}

// TestIPClassificationSeedIsConflictDoNothing: the seed re-executes on EVERY
// boot (leading keyword INSERT is not recognized by migrationSkipProbe, so it
// is never skipped). ON CONFLICT DO UPDATE would make each boot revert operator
// curation back to the seed values. This is the shape test; the integration
// test proves the surviving-row behaviour against real Postgres.
func TestIPClassificationSeedIsConflictDoNothing(t *testing.T) {
	stmts := splitSQLStatements(igniteIPClassificationSeedDDL)
	if len(stmts) == 0 {
		t.Fatal("seed DDL parsed to zero statements")
	}
	for i, s := range stmts {
		body := stripSQLLineComments(s)
		if !regexp.MustCompile(`(?is)^\s*INSERT\s+INTO\s+ignite_ip_classification`).MatchString(body) {
			t.Errorf("seed statement %d is not an INSERT INTO ignite_ip_classification:\n%s", i+1, body)
			continue
		}
		if !regexp.MustCompile(`(?is)ON\s+CONFLICT\s*\(\s*cidr\s*\)\s*DO\s+NOTHING`).MatchString(body) {
			t.Errorf("seed statement %d lacks ON CONFLICT (cidr) DO NOTHING — a re-boot would error on the existing rows:\n%s", i+1, body)
		}
		if regexp.MustCompile(`(?is)DO\s+UPDATE`).MatchString(body) {
			t.Fatalf("seed statement %d uses ON CONFLICT DO UPDATE. The seed runs on every boot, so this reverts operator curation on every restart:\n%s", i+1, body)
		}
	}
}

// TestIPClassificationSeedCarriesTheProvenRows: the two prod-proven /32s are
// actually in the seed with the classes the pins expect. Without this, the
// resolution tests above would be pinning a fixture nobody seeds.
func TestIPClassificationSeedCarriesTheProvenRows(t *testing.T) {
	seed := igniteIPClassificationSeedDDL
	for _, want := range []struct{ cidr, class string }{
		{"135.232.20.148/32", "scanner"},
		{"135.232.20.64/32", "unresolved"},
	} {
		re := regexp.MustCompile(`(?is)\('` + regexp.QuoteMeta(want.cidr) + `'\s*,\s*'` + regexp.QuoteMeta(want.class) + `'`)
		if !re.MatchString(seed) {
			t.Errorf("seed does not carry %s as class %q — the prod-proven value cannot be produced", want.cidr, want.class)
		}
	}
	// 135.232.99.99 must NOT have its own row, or the "falls back to the /16"
	// pin stops testing fallback.
	if strings.Contains(seed, "135.232.99.99") {
		t.Errorf("seed carries a row for 135.232.99.99; that address is the fallback-to-/16 fixture and must have no narrow row")
	}
	if strings.Contains(seed, "8.8.8.8") {
		t.Errorf("seed carries a row for 8.8.8.8; that address is the returns-NULL fixture and must have no row")
	}
}

// -----------------------------------------------------------------------------
// Wiring — an unregistered DDL constant is dead code
// -----------------------------------------------------------------------------

// TestIPClassificationMigrationWiring reads main.go and pins that all four
// entries are registered, as FOUR SEPARATE entries, in dependency order, and
// AFTER create_ignite_ip_is_datacenter_fn. The migration slice is an anonymous
// inline literal, so the source text is the only handle on it — same reason
// this repo's inert columns went unnoticed: the constant existed, the
// registration did not.
func TestIPClassificationMigrationWiring(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)

	ordered := []struct{ name, constant string }{
		{"create_ignite_ip_classification", "igniteIPClassificationDDL"},
		{"idx_ignite_ip_classification_gist", "igniteIPClassificationGistDDL"},
		{"seed_ignite_ip_classification_from_dc_ranges", "igniteIPClassificationSeedDDL"},
		{"create_ignite_ip_class_fn", "igniteIPClassFnDDL"},
	}

	prev := strings.Index(text, `{"create_ignite_ip_is_datacenter_fn"`)
	if prev < 0 {
		t.Fatal("create_ignite_ip_is_datacenter_fn is not registered in main.go — the anchor this block must follow is gone")
	}
	for _, e := range ordered {
		entry := `{"` + e.name + `", ` + e.constant + `}`
		idx := strings.Index(text, entry)
		if idx < 0 {
			t.Fatalf("runStartupMigrations has no entry %s — the DDL constant is dead code and the object never lands", entry)
		}
		if idx < prev {
			t.Fatalf("entry %q appears BEFORE the object it depends on; dependency order in the slice is execution order", e.name)
		}
		prev = idx
	}
}

// TestIgniteIPIsDatacenterUnchangedByThisUnit: the ownership helper feeds
// igniteEventVerdictBody, which trg_set_click_verdict materializes into
// mailing_tracking_events.click_verdict BEFORE INSERT. This unit is explicitly
// out of scope for it. Pin the exact body so a later "while I'm here" edit
// cannot silently rewrite classification for every future click row.
func TestIgniteIPIsDatacenterUnchangedByThisUnit(t *testing.T) {
	const want = `CREATE OR REPLACE FUNCTION ignite_ip_is_datacenter(ip inet) RETURNS boolean
	LANGUAGE sql STABLE PARALLEL SAFE AS
$dcfn$ SELECT EXISTS (SELECT 1 FROM ignite_datacenter_ranges r WHERE ip <<= r.cidr) $dcfn$`
	if igniteIPIsDatacenterDDL != want {
		t.Fatalf("ignite_ip_is_datacenter changed. It is out of scope for the IP-classification unit and feeds the live click_verdict trigger.\n got: %q\nwant: %q", igniteIPIsDatacenterDDL, want)
	}
	// And the classification accessor must not have been fused into it.
	if strings.Contains(igniteIPIsDatacenterDDL, "ignite_ip_classification") {
		t.Fatalf("ignite_ip_is_datacenter now reads ignite_ip_classification — repointing the verdict path is a separate, operator-gated unit")
	}
	if !strings.Contains(igniteEventVerdictBody, "ignite_ip_is_datacenter(ip)") {
		t.Fatalf("the verdict body no longer calls ignite_ip_is_datacenter(ip)")
	}
	if strings.Contains(igniteEventVerdictBody, "ignite_ip_class(") {
		t.Fatalf("the verdict body now calls ignite_ip_class(): the classification table is documented as INERT on the verdict path, and repointing it is operator-gated")
	}
}

// TestIPClassAccessorSignatureIsPinned. PostgreSQL identifies a function by
// (name, argument types), so a CREATE OR REPLACE that CHANGES the parameter
// list does not replace ignite_ip_class — it creates an OVERLOAD beside it.
// Two overloads that can both accept one argument make every bare
// ignite_ip_class(ip) call ambiguous, and it starts erroring at runtime with no
// migration failure to point at. Changing the signature requires a
// DROP FUNCTION ignite_ip_class(inet, interval) in the SAME migration entry,
// before the CREATE; this pin makes that a deliberate act.
func TestIPClassAccessorSignatureIsPinned(t *testing.T) {
	const wantSig = "CREATE OR REPLACE FUNCTION ignite_ip_class(ip inet, max_age interval DEFAULT NULL)"
	if !strings.Contains(igniteIPClassFnDDL, wantSig) {
		t.Fatalf("ignite_ip_class signature changed.\nwant: %s\ngot DDL:\n%s\n"+
			"A changed parameter list creates an OVERLOAD, not a replacement, and the bare "+
			"ignite_ip_class(ip) call becomes ambiguous at runtime. Add "+
			"`DROP FUNCTION IF EXISTS ignite_ip_class(inet, interval);` ahead of the CREATE "+
			"in the same migration entry, then update this pin.", wantSig, igniteIPClassFnDDL)
	}
	if !strings.Contains(igniteIPClassFnDDL, "RETURNS text") {
		t.Errorf("ignite_ip_class must RETURN text (NULL = no row, distinct from the 'unknown' class)")
	}
	// STABLE, not IMMUTABLE: it reads a table. PARALLEL SAFE mirrors
	// ignite_ip_is_datacenter so a parallel plan is still allowed.
	if !strings.Contains(igniteIPClassFnDDL, "STABLE PARALLEL SAFE") {
		t.Errorf("ignite_ip_class must be STABLE PARALLEL SAFE — it reads a table, so IMMUTABLE would let the planner cache a stale answer")
	}
	// A signature change without the DROP is the failure mode above.
	if strings.Contains(igniteIPClassFnDDL, "DROP FUNCTION") &&
		!strings.Contains(igniteIPClassFnDDL, "ignite_ip_class(inet, interval)") {
		t.Errorf("the entry drops a function, but not the (inet, interval) overload it is replacing")
	}
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

// splitSQLStatements splits on top-level semicolons, ignoring those inside
// single quotes or -- comments. The seed is three statements in one entry
// (lib/pq sends an argument-less Exec over the simple query protocol, so
// multi-statement is intentional and lands all-or-nothing).
func splitSQLStatements(sql string) []string {
	var out []string
	var cur strings.Builder
	inStr := false
	lines := strings.Split(sql, "\n")
	for _, line := range lines {
		trimmed := line
		if !inStr {
			if i := strings.Index(line, "--"); i >= 0 && !strings.Contains(line[:i], "'") {
				trimmed = line[:i]
			}
		}
		for _, r := range trimmed {
			if r == '\'' {
				inStr = !inStr
			}
			if r == ';' && !inStr {
				if s := strings.TrimSpace(cur.String()); s != "" {
					out = append(out, s)
				}
				cur.Reset()
				continue
			}
			cur.WriteRune(r)
		}
		cur.WriteRune('\n')
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// extractCheckINList pulls the quoted literals out of `<col> IN ( 'a','b' )`.
func extractCheckINList(t *testing.T, ddl, marker string) []string {
	t.Helper()
	i := strings.Index(ddl, marker)
	if i < 0 {
		t.Fatalf("DDL has no %q", marker)
	}
	rest := ddl[i+len(marker):]
	j := strings.Index(rest, ")")
	if j < 0 {
		t.Fatalf("unterminated IN list after %q", marker)
	}
	var out []string
	for _, m := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(rest[:j], -1) {
		out = append(out, m[1])
	}
	return out
}

func equalStringSets(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}
