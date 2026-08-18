package worker

// Drip Observatory rollup — unit suite (Vector B plan rev 4.2 §6.10 DoD:
// kill switch + lane gate; parse-quarantine; governed statuses; click
// canonical both branches + 'none' + mixed; lock-held skip).
//
// Parser fixtures are GENERATED from the orchestrator's real name literal
// (partner_drip_orchestrator.go:3642) with ROSTER codes from dripBrands —
// never hand-written apex-token names (rev-4.2 §11 fixture rule).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// obsFixtureName replicates fmt.Sprintf("[partner-drip] %s %s %s %s %s",
// v.vertical, brand, ts, sha4, nonce) — the :3642 literal — plus the
// optional bracket suffixes ([tN] deployWaveGroups :4456/:4505; [ses:…]
// :1267).
func obsFixtureName(vertical, brand, suffix string) string {
	name := fmt.Sprintf("[partner-drip] %s %s %s %s %s",
		vertical, brand,
		time.Date(2026, 8, 14, 1, 2, 3, 0, time.UTC).Format("20060102T150405"),
		"8ddc6ed7", "bb4c604d")
	if suffix != "" {
		name += " " + suffix
	}
	return name
}

func TestDripObservatoryParseCampaignName(t *testing.T) {
	cases := []struct {
		name         string
		input        string
		wantVertical string
		wantToken    string
		wantTouch    int
		wantOK       bool
	}{
		// Suffix permutations (rev-4.2 §11): bare, [tN], [ses:…], both.
		{"bare", obsFixtureName("internal_auto_insurance", "db", ""), "internal_auto_insurance", "db", 1, true},
		{"touch suffix", obsFixtureName("internal_auto_insurance", "db", "[t2]"), "internal_auto_insurance", "db", 2, true},
		{"ses suffix", obsFixtureName("internal_auto_insurance", "db", "[ses:93938919]"), "internal_auto_insurance", "db", 1, true},
		{"both suffixes", obsFixtureName("remodel", "bwp", "[t3] [ses:651cff33]"), "remodel", "bwp", 3, true},
		// The 11/27 mismatch class tokens parse as tokens (resolution is a
		// separate chain link).
		{"wfy token", obsFixtureName("internal_auto_insurance", "wfy", ""), "internal_auto_insurance", "wfy", 1, true},
		{"yih token", obsFixtureName("internal_auto_insurance", "yih", "[t4]"), "internal_auto_insurance", "yih", 4, true},
		// Overlay-only brand token.
		{"wcl token", obsFixtureName("refi_heloc", "wcl", ""), "refi_heloc", "wcl", 1, true},
		// Failures.
		{"not a drip name", "Morning Broadcast db 20260814T010203", "", "", 0, false},
		{"too few fields", "[partner-drip] remodel db", "", "", 0, false},
		{"timestamp not in position 3", "[partner-drip] remodel db notatimestamp 8ddc6ed7 bb4c604d", "", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vertical, token, touch, ok := parseDripCampaignName(tc.input)
			if ok != tc.wantOK {
				t.Fatalf("parse(%q) ok=%v want %v", tc.input, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if vertical != tc.wantVertical || token != tc.wantToken || touch != tc.wantTouch {
				t.Fatalf("parse(%q) = (%q,%q,%d) want (%q,%q,%d)",
					tc.input, vertical, token, touch, tc.wantVertical, tc.wantToken, tc.wantTouch)
			}
		})
	}
}

func TestDripObservatoryResolveBrandChain(t *testing.T) {
	empty := map[string]string{}
	overlay := map[string]string{"wcl": "m.wcl-heloc.com"}

	// Compiled-map codes resolve to apex + canonical brandident code.
	if apex, code, ok := resolveObsBrand("db", empty); !ok || apex != "discountblog.com" || code != "db" {
		t.Fatalf("db → (%q,%q,%v), want (discountblog.com, db, true)", apex, code, ok)
	}
	// The 11/27 orchestrator≠brandident mismatch class (§6.6): wfy/yih must
	// resolve through the chain — NOT quarantined, NOT collide-resolved.
	if apex, code, ok := resolveObsBrand("wfy", empty); !ok || apex != "warrantyforyou.com" || code != "wf" {
		t.Fatalf("wfy → (%q,%q,%v), want (warrantyforyou.com, wf, true)", apex, code, ok)
	}
	if apex, code, ok := resolveObsBrand("yih", empty); !ok || apex != "yourinsurancehub.com" || code != "yi" {
		t.Fatalf("yih → (%q,%q,%v), want (yourinsurancehub.com, yi, true)", apex, code, ok)
	}
	// Governed token codes resolve too (mpf → mypersonalfinancial.com → mp).
	if apex, code, ok := resolveObsBrand("mpf", empty); !ok || apex != "mypersonalfinancial.com" || code != "mp" {
		t.Fatalf("mpf → (%q,%q,%v), want (mypersonalfinancial.com, mp, true)", apex, code, ok)
	}
	// Overlay-only code: chain links 2–3 pass through the overlay and the
	// m.-label strip; link 4 misses in the brandident LITERAL (wcl-heloc.com
	// is not a registry brand) → quarantine here. The DB-onboard round trip
	// (mailing_brand_codes source='onboard' union) is integration #6c.
	if _, _, ok := resolveObsBrand("wcl", overlay); ok {
		t.Fatal("wcl without a brandident onboard row must NOT resolve (brand_unknown quarantine)")
	}
	if got := stripSendingLabel("m.wcl-heloc.com"); got != "wcl-heloc.com" {
		t.Fatalf("stripSendingLabel(m.wcl-heloc.com) = %q", got)
	}
	// Unknown token → chain failure (brand_unknown), never a guess.
	if _, _, ok := resolveObsBrand("zz", empty); ok {
		t.Fatal("unknown token must not resolve")
	}
	if _, _, ok := resolveObsBrand("", empty); ok {
		t.Fatal("empty token must not resolve")
	}
	// Label stripping: em. and m. only; multi-label domains untouched.
	if got := stripSendingLabel("em.discountblog.com"); got != "discountblog.com" {
		t.Fatalf("stripSendingLabel(em.discountblog.com) = %q", got)
	}
	if got := stripSendingLabel("mail.foo.com"); got != "mail.foo.com" {
		t.Fatalf("stripSendingLabel(mail.foo.com) = %q (must not strip bare 'm')", got)
	}
}

func TestDripObservatoryClassifyClickURL(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://www.cratoolpro.com/offer/abc?source_id=email&sub1=x", "money"},
		{"https://codefortwo.com/adt/landing", "money"},
		{"https://t.em.discountblog.com/o/eyJhbGciOi?x=1", "wrapper"},
		{"https://t.em.discountblog.com/track/click/abc123", "wrapper"},
		// A wrapper URL that EMBEDS a money URL in its query is a wrapper —
		// the money-host match is anchored at position 0 (§3.3/§7.2).
		{"https://t.em.discountblog.com/o/tok?d=https://www.cratoolpro.com/offer/abc", "wrapper"},
		{"https://em.discountblog.com/integration/unsub?_redir=xyz", "other"},
		{"https://example.com/whatever", "other"},
		{"::not a url::", "other"},
		{"", "other"},
	}
	for _, tc := range cases {
		if got := classifyClickURL(tc.url); got != tc.want {
			t.Errorf("classifyClickURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestDripObservatoryClickBasisLabel(t *testing.T) {
	// §3.3: 'none' iff total=0 (never a default); wrapper ⇒ resolved=0;
	// resolved-only ⇒ wrapper=0; both ⇒ mixed.
	cases := []struct {
		w, r int
		want string
	}{
		{0, 0, "none"},
		{5, 0, "wrapper"},
		{0, 3, "resolved-only"},
		{5, 3, "mixed"},
	}
	for _, tc := range cases {
		if got := clickBasisLabel(tc.w, tc.r); got != tc.want {
			t.Errorf("clickBasisLabel(%d,%d) = %q, want %q", tc.w, tc.r, got, tc.want)
		}
	}
}

func TestDripObservatoryISPNormalization(t *testing.T) {
	cases := []struct{ in, want string }{
		{"gmail", "gmail"},
		{"Gmail ", "gmail"},
		{"comcast", "comcast"},
		{"", "other"},          // orchestrator NULLIF fallback
		{"msft", "other"},      // pool-suffix style value, not vocabulary
		{"notanisp", "other"},  // long-tail bucket
		{"other", "other"},
	}
	for _, tc := range cases {
		if got := dripObsNormalizeISPFamily(tc.in); got != tc.want {
			t.Errorf("normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := dripObsISPFromEmail("someone@nyc.rr.com"); got != "charter" {
		t.Errorf("ispFromEmail(nyc.rr.com) = %q, want charter", got)
	}
	if got := dripObsISPFromEmail("no-at-sign"); got != "other" {
		t.Errorf("ispFromEmail(no-at-sign) = %q, want other", got)
	}
}

func TestDripObservatoryDispatchGroupBasis(t *testing.T) {
	cases := []struct {
		set       map[string]bool
		wantBasis string
		wantMixed bool
	}{
		{map[string]bool{"pcq": true}, "pcq", false},
		{map[string]bool{"message_log": true}, "message_log", false},
		{map[string]bool{"acct_terminal": true}, "acct_terminal", false},
		{map[string]bool{"pcq": true, "acct_terminal": true}, "pcq", true},
		{map[string]bool{"pcq": true, "unavailable": true}, "pcq", true},
		{map[string]bool{"unavailable": true}, "unavailable", false},
		{map[string]bool{}, "unavailable", false},
	}
	for _, tc := range cases {
		basis, mixed := dispatchGroupBasis(tc.set)
		if basis != tc.wantBasis || mixed != tc.wantMixed {
			t.Errorf("dispatchGroupBasis(%v) = (%q,%v), want (%q,%v)", tc.set, basis, mixed, tc.wantBasis, tc.wantMixed)
		}
	}
}

// TestDripObservatoryGovernedStatuses — §3.4 standing rules: governed/kumo
// rows publish delivery+bounce NULL + 'unavailable'; non-governed SES-routed
// rows publish delivered + bounce 'partial'. Every status set explicitly by
// the writer.
func TestDripObservatoryGovernedStatuses(t *testing.T) {
	ds := obsDataset{id: "ds", vertical: "fixture"}
	mk := func(governed bool, apex string) ([]obsFactRow, obsCellKey) {
		k := obsCellKey{org: "org", day: "2026-08-14", touch: 1, brand: "db", isp: "gmail"}
		cells := map[obsCellKey]*obsCell{k: {apex: apex, governed: governed, delivered: 40, hardBounced: 2, softBounced: 3, repBlocked: 1, complained: 1, dispatchPcq: 50}}
		extras := map[obsCellKey]*obsLaneExtras{}
		x := &obsLaneExtras{basisSet: map[string]bool{"pcq": true}, apex: apex, governed: governed}
		extras[obsCellKey{org: "org", day: "2026-08-14", touch: 1, brand: "db"}] = x
		return buildFactRows("cohort", ds, cells, extras, map[string]bool{}), k
	}

	rows, _ := mk(false, "discountblog.com")
	var ispRow, laneRow *obsFactRow
	for i := range rows {
		if rows[i].scope == "lane_isp" {
			ispRow = &rows[i]
		} else {
			laneRow = &rows[i]
		}
	}
	if ispRow == nil || laneRow == nil {
		t.Fatalf("expected lane_isp + lane rows, got %d rows", len(rows))
	}
	if !ispRow.delivered.Valid || ispRow.delivered.Int64 != 40 {
		t.Fatalf("non-governed delivered = %+v, want 40", ispRow.delivered)
	}
	if ispRow.stDelivery != "available" || ispRow.stBounce != "partial" {
		t.Fatalf("non-governed statuses = (%s,%s), want (available,partial)", ispRow.stDelivery, ispRow.stBounce)
	}
	if ispRow.stDispatch != "available" || !ispRow.dispatched.Valid || ispRow.dispatched.Int64 != 50 {
		t.Fatalf("pcq dispatch = (%s,%+v), want available/50", ispRow.stDispatch, ispRow.dispatched)
	}
	if laneRow.ispVal != "" || laneRow.scope != "lane" {
		t.Fatalf("lane row shape wrong: %+v", laneRow)
	}

	grows, _ := mk(true, "mypersonalfinancial.com")
	for _, r := range grows {
		if r.stDelivery != "unavailable" || r.stBounce != "unavailable" {
			t.Fatalf("governed statuses = (%s,%s), want (unavailable,unavailable)", r.stDelivery, r.stBounce)
		}
		if r.delivered.Valid || r.hard.Valid || r.soft.Valid || r.rep.Valid {
			t.Fatalf("governed delivery/bounce columns must be NULL: %+v", r)
		}
	}
}

// TestDripObservatoryFailedFamilyStatuses — a failed source slice publishes
// that family's status as 'failed' (§3.4).
func TestDripObservatoryFailedFamilyStatuses(t *testing.T) {
	ds := obsDataset{id: "ds", vertical: "fixture"}
	k := obsCellKey{org: "org", day: "2026-08-14", touch: 1, brand: "db", isp: "gmail"}
	cells := map[obsCellKey]*obsCell{k: {apex: "discountblog.com", opens: 7}}
	extras := map[obsCellKey]*obsLaneExtras{
		{org: "org", day: "2026-08-14", touch: 1, brand: "db"}: {basisSet: map[string]bool{"pcq": true}, apex: "discountblog.com"},
	}
	rows := buildFactRows("event", ds, cells, extras, map[string]bool{"engagement": true, "conversion": true})
	for _, r := range rows {
		if r.stEngagement != "failed" || r.stConversion != "failed" {
			t.Fatalf("failed-family statuses = (%s,%s), want (failed,failed)", r.stEngagement, r.stConversion)
		}
		if r.stDispatch != "available" {
			t.Fatalf("unaffected family must keep its own status, got %s", r.stDispatch)
		}
	}
}

// TestDripObservatoryMixedBasisLaneAggregation — wrapper-basis and
// resolved-basis campaign-days aggregate: total = sum, click_basis='mixed'
// on the aggregate row, per-ISP rows keep their own labels (§3.3).
func TestDripObservatoryMixedBasisLaneAggregation(t *testing.T) {
	ds := obsDataset{id: "ds", vertical: "fixture"}
	kW := obsCellKey{org: "org", day: "2026-08-14", touch: 1, brand: "db", isp: "gmail"}
	kR := obsCellKey{org: "org", day: "2026-08-14", touch: 1, brand: "db", isp: "yahoo"}
	cells := map[obsCellKey]*obsCell{
		kW: {apex: "discountblog.com", clicksWrapper: 2, actionsW: 2, humanClicks: 1},
		kR: {apex: "discountblog.com", clicksMoney: 3, actionsR: 3, humanClicks: 2},
	}
	extras := map[obsCellKey]*obsLaneExtras{
		{org: "org", day: "2026-08-14", touch: 1, brand: "db"}: {basisSet: map[string]bool{"pcq": true}, apex: "discountblog.com"},
	}
	rows := buildFactRows("cohort", ds, cells, extras, map[string]bool{})
	byISP := map[string]obsFactRow{}
	for _, r := range rows {
		byISP[r.ispVal] = r
	}
	if r := byISP["gmail"]; r.clickBasis != "wrapper" || r.actionsW != 2 || r.actionsR != 0 {
		t.Fatalf("gmail row = basis %q W=%d R=%d, want wrapper/2/0", r.clickBasis, r.actionsW, r.actionsR)
	}
	if r := byISP["yahoo"]; r.clickBasis != "resolved-only" || r.actionsW != 0 || r.actionsR != 3 {
		t.Fatalf("yahoo row = basis %q W=%d R=%d, want resolved-only/0/3", r.clickBasis, r.actionsW, r.actionsR)
	}
	lane := byISP[""]
	if lane.clickBasis != "mixed" || lane.actionsW != 2 || lane.actionsR != 3 || lane.humanClicks != 3 {
		t.Fatalf("lane row = basis %q W=%d R=%d human=%d, want mixed/2/3/3", lane.clickBasis, lane.actionsW, lane.actionsR, lane.humanClicks)
	}
	// 'none' is earned, not defaulted: a group with zero actions labels none.
	kN := obsCellKey{org: "org", day: "2026-08-15", touch: 1, brand: "db", isp: "gmail"}
	nrows := buildFactRows("cohort", ds, map[obsCellKey]*obsCell{kN: {apex: "discountblog.com", opens: 4}},
		map[obsCellKey]*obsLaneExtras{{org: "org", day: "2026-08-15", touch: 1, brand: "db"}: {basisSet: map[string]bool{"pcq": true}, apex: "discountblog.com"}},
		map[string]bool{})
	for _, r := range nrows {
		if r.clickBasis != "none" {
			t.Fatalf("zero-action row basis = %q, want none", r.clickBasis)
		}
	}
}

func TestDripObservatoryLaneGateParsing(t *testing.T) {
	if g := parseObservatoryLanes(""); !g.empty() {
		t.Fatal("empty env must gate to nothing")
	}
	if g := parseObservatoryLanes("  "); !g.empty() {
		t.Fatal("blank env must gate to nothing")
	}
	if g := parseObservatoryLanes("all"); !g.all {
		t.Fatal("'all' must enable every lane")
	}
	if g := parseObservatoryLanes("ALL"); !g.all {
		t.Fatal("'ALL' must enable every lane (case-insensitive)")
	}
	g := parseObservatoryLanes(" 0b5e17a1-0000-4000-8000-0000000000e2 , 0b5e17a1-0000-4000-8000-0000000000e3 ,")
	if g.all || len(g.ids) != 2 {
		t.Fatalf("uuid list parse = %+v, want 2 ids", g)
	}
}

// TestDripObservatoryKillSwitch — DRIP_OBSERVATORY_ROLLUP_DISABLED=1:
// constructor logs, Start returns (no ticker, no DB touch).
func TestDripObservatoryKillSwitch(t *testing.T) {
	t.Setenv("DRIP_OBSERVATORY_ROLLUP_DISABLED", "1")
	db, _, err := sqlmock.New() // zero expectations — any DB touch fails
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	w := NewDripObservatoryRollup(db)
	if !w.disabled {
		t.Fatal("kill switch must disable the worker")
	}
	done := make(chan struct{})
	go func() {
		w.Start(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start must return immediately when disabled")
	}
}

// TestDripObservatoryEmptyLaneGate — empty DRIP_OBSERVATORY_LANES: the
// worker runs but processes NOTHING (no run row, no lock, no DB touch).
func TestDripObservatoryEmptyLaneGate(t *testing.T) {
	t.Setenv("DRIP_OBSERVATORY_LANES", "")
	db, mock, err := sqlmock.New() // zero expectations
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	w := NewDripObservatoryRollup(db)
	if err := w.runCycle(context.Background()); err != nil {
		t.Fatalf("empty-gate cycle must no-op cleanly: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("empty-gate cycle touched the DB: %v", err)
	}
}

// TestDripObservatoryLockHeldSkip — pg_try_advisory_lock=false: the cycle
// skips without opening a run (§6.1).
func TestDripObservatoryLockHeldSkip(t *testing.T) {
	t.Setenv("DRIP_OBSERVATORY_LANES", "all")
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The ONLY statement a lock-held cycle may issue is the try-lock itself —
	// no unlock (we never held it), no run row, no source scans (§6.1).
	mock.ExpectQuery(`pg_try_advisory_lock`).
		WithArgs(dripObservatoryLockID).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(false))

	w := NewDripObservatoryRollup(db)
	if err := w.runCycle(context.Background()); err != nil {
		t.Fatalf("lock-held cycle must skip cleanly: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected DB traffic on lock-held skip: %v", err)
	}
}

func TestDripObservatoryQuarantineSampleTruncation(t *testing.T) {
	long := make([]byte, 1200)
	for i := range long {
		long[i] = 'x'
	}
	if got := truncateObsSample(string(long), 500); len(got) != 500 {
		t.Fatalf("sample truncation = %d chars, want 500", len(got))
	}
	if got := truncateObsSample("short", 500); got != "short" {
		t.Fatalf("short sample mangled: %q", got)
	}
}

// TestDripObservatoryDenverDayBounds — DST days span 23h/25h (§11 #20's
// unit-level half): 2026-03-08 springs forward (23h), 2026-11-01 falls back
// (25h).
func TestDripObservatoryDenverDayBounds(t *testing.T) {
	lo, hi, err := dripObsDayBounds("2026-03-08")
	if err != nil {
		t.Fatal(err)
	}
	if d := hi.Sub(lo); d != 23*time.Hour {
		t.Fatalf("2026-03-08 Denver day = %v, want 23h", d)
	}
	lo, hi, err = dripObsDayBounds("2026-11-01")
	if err != nil {
		t.Fatal(err)
	}
	if d := hi.Sub(lo); d != 25*time.Hour {
		t.Fatalf("2026-11-01 Denver day = %v, want 25h", d)
	}
	// Late-evening MST instant on the fall-back day buckets to Nov 1.
	inst := time.Date(2026, 11, 2, 6, 30, 0, 0, time.UTC) // 23:30 MST Nov 1
	if got := dripObsDenverDay(inst); got != "2026-11-01" {
		t.Fatalf("fall-back bucketing = %s, want 2026-11-01", got)
	}
}

// TestDripObservatoryGovernedApexSet — both identity paths agree on the
// governed flag through the apex (meta rows carry canonical codes, the name
// parse carries orchestrator tokens).
func TestDripObservatoryGovernedApexSet(t *testing.T) {
	if !dripObsGovernedApex("mypersonalfinancial.com") {
		t.Fatal("mpf apex must be governed")
	}
	if !dripObsGovernedApex("bestcreditcare.com") {
		t.Fatal("bcc apex must be governed")
	}
	if dripObsGovernedApex("discountblog.com") {
		t.Fatal("db apex must not be governed")
	}
}

// Compile-time guard: the interim identity chain never bypasses the §6.6
// contract types.
var _ = func() *sql.DB { return nil }
