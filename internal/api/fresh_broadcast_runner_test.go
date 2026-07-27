package api

// Fresh Broadcast Runner tests — pin the PARITY CONTRACT with the Python
// reference implementation (agents/scheduling/stream_router.py +
// board_generator.build_fresh_bcast_briefs). Golden values in this file were
// computed by running the Python side; if a golden fails, the Go side has
// drifted from the authoritative implementation — fix Go, not the golden.

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// ── uuid5 parity (the double-stage guard) ────────────────────────────────────

// Goldens from: python3 -c "import uuid;
// ns=uuid.UUID('c0f5ee21-0726-4b3a-9e17-000000000000');
// print(uuid.uuid5(ns,'CONSUMER-J28-DB-GMAIL'))" etc.
func TestFreshSegIDPythonParity(t *testing.T) {
	cases := map[string]string{
		"CONSUMER-J28-DB-GMAIL": "4e0bc7f6-002c-59b2-91a7-4f6b610b4dbc",
		"MORTGAGE-J28-RR-APPLE": "6ef8cbc5-4dbb-5d95-af20-6b6ea8236a5a",
		"WCM-J28-WCL-GMAIL":     "8242023a-05bf-534c-b7e1-fbb02ab4f90d",
		"CONSUMER-J5-TT-OTHER":  "28bfa9cd-e9e1-543d-add5-fbb164967441",
	}
	for name, want := range cases {
		if got := freshSegID(name); got != want {
			t.Errorf("freshSegID(%q) = %s, python uuid5 = %s — namespace/name drift breaks the cross-implementation double-stage guard", name, got, want)
		}
	}
	// The id must be an RFC-4122 version-5 UUID (SHA-1 name-based).
	u := uuid.MustParse(freshSegID("CONSUMER-J28-DB-GMAIL"))
	if u.Version() != 5 {
		t.Errorf("freshSegID emits version %d, want version 5", u.Version())
	}
}

func TestFreshTokensPythonParity(t *testing.T) {
	jul28 := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	jan5 := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	// registry.fresh_token: month initial + UNPADDED day.
	if got := freshBatchToken(jul28); got != "J28" {
		t.Errorf("freshBatchToken(jul28) = %q, want J28", got)
	}
	if got := freshBatchToken(jan5); got != "J5" {
		t.Errorf("freshBatchToken(jan5) = %q, want J5 (unpadded)", got)
	}
	// registry.date_token: %b%d lowercased, PADDED day.
	if got := freshDateToken(jul28); got != "jul28" {
		t.Errorf("freshDateToken(jul28) = %q, want jul28", got)
	}
	if got := freshDateToken(jan5); got != "jan05" {
		t.Errorf("freshDateToken(jan5) = %q, want jan05 (zero-padded)", got)
	}
	if got := freshSegName("CONSUMER", "J28", "DB", "GMAIL"); got != "CONSUMER-J28-DB-GMAIL" {
		t.Errorf("freshSegName = %q", got)
	}
	if got := freshBatchMarker("CONSUMER", "J28", jul28); got != "consumer-j28-20260728" {
		t.Errorf("freshBatchMarker = %q, want consumer-j28-20260728", got)
	}
}

// ── stable md5→site hash parity ──────────────────────────────────────────────

// Goldens from python assign_site (int(md5[:8],16) % len(sites)) over the
// consumer pool and the pool+WCL variant.
func TestFreshAssignSitePythonParity(t *testing.T) {
	pool := []string{"DB", "CP", "QF", "HT", "TT"}
	poolWCL := append(append([]string{}, pool...), "WCL")
	cases := []struct{ md5, want5, want6 string }{
		{"9e107d9d372bb6826bd81d3542a419d6", "CP", "HT"},
		{"e4d909c290d0fb1ca068ffaddf22cbd0", "HT", "DB"},
		{"00000000ffffffffffffffffffffffff", "DB", "DB"},
		{"ffffffff00000000ffffffffffffffff", "DB", "HT"},
	}
	for _, c := range cases {
		got, err := freshAssignSite(c.md5, pool)
		if err != nil || got != c.want5 {
			t.Errorf("assign(%s, pool5) = %q,%v want %q", c.md5[:8], got, err, c.want5)
		}
		got, err = freshAssignSite(c.md5, poolWCL)
		if err != nil || got != c.want6 {
			t.Errorf("assign(%s, pool6) = %q,%v want %q", c.md5[:8], got, err, c.want6)
		}
	}
	if _, err := freshAssignSite("9e107d9d372bb6826bd81d3542a419d6", nil); err == nil {
		t.Error("empty site list must REFUSE")
	}
	if _, err := freshAssignSite("", pool); err == nil {
		t.Error("empty md5 must REFUSE")
	}
}

func TestFreshExplicitSiteCode(t *testing.T) {
	if got := freshExplicitSiteCode("m.wcl-heloc.com"); got != "WCL" {
		t.Errorf("freshExplicitSiteCode(m.wcl-heloc.com) = %q, want WCL (stream_routing.json site_code)", got)
	}
	if got := freshExplicitSiteCode("em.discountblog.com"); got != "DISCOUNTBLOG" {
		t.Errorf("freshExplicitSiteCode(em.discountblog.com) = %q", got)
	}
}

// ── caps math (stream_router.apply_caps semantics) ───────────────────────────

func TestFreshApplyCaps(t *testing.T) {
	rows := []freshDrawRow{
		{Email: "a", ISPFamily: "yahoo"},
		{Email: "b", ISPFamily: "yahoo"},
		{Email: "c", ISPFamily: "aol"},
		{Email: "d", ISPFamily: ""}, // folds to other
		{Email: "e", ISPFamily: "Yahoo"},
	}
	kept, perISP, trimmed := freshApplyCaps(rows, 10, map[string]int{"yahoo": 2})
	if len(kept) != 4 {
		t.Fatalf("kept %d, want 4 (yahoo capped at 2)", len(kept))
	}
	if perISP["yahoo"] != 2 || perISP["aol"] != 1 || perISP["other"] != 1 {
		t.Errorf("perISP = %v", perISP)
	}
	if trimmed["yahoo"] != 1 {
		t.Errorf("trimmed = %v, want yahoo:1", trimmed)
	}
	// Order preserved: freshest rows win the cap.
	if kept[0].Email != "a" || kept[1].Email != "b" || kept[2].Email != "c" || kept[3].Email != "d" {
		t.Errorf("order not preserved: %+v", kept)
	}
	// Daily cap bounds the total.
	kept, _, _ = freshApplyCaps(rows, 3, nil)
	if len(kept) != 3 {
		t.Errorf("daily cap 3 → kept %d", len(kept))
	}
	// Cap 0 = no draw (guarded upstream, but the pure fn must hold too).
	kept, _, _ = freshApplyCaps(rows, 0, nil)
	if len(kept) != 0 {
		t.Errorf("daily cap 0 → kept %d, want 0", len(kept))
	}
}

// ── gmail-doctrine masking ───────────────────────────────────────────────────

func TestFreshPlanAssignmentGmailMask(t *testing.T) {
	rows := []freshDrawRow{
		{Email: "g@x", EmailMD5: "9e107d9d372bb6826bd81d3542a419d6", ISPFamily: "gmail"},
		{Email: "y@x", EmailMD5: "e4d909c290d0fb1ca068ffaddf22cbd0", ISPFamily: "yahoo"},
	}
	primaries := []string{"RR", "RB", "FC"}

	// With an explicit lane: gmail assigns ONLY there; others hash the union.
	got, masked, err := freshPlanAssignment(rows, primaries, "WCL")
	if err != nil {
		t.Fatal(err)
	}
	if masked != 0 {
		t.Errorf("masked = %d, want 0 with explicit lane", masked)
	}
	if n := len(got[freshCell{Site: "WCL", Lane: "GMAIL"}]); n != 1 {
		t.Errorf("gmail row must land on the explicit lane; WCL-GMAIL has %d", n)
	}
	for cell := range got {
		if cell.Lane == "GMAIL" && cell.Site != "WCL" {
			t.Errorf("gmail volume assigned to brand site %s — doctrine violation", cell.Site)
		}
	}

	// Without an explicit lane: gmail is masked, never staged.
	got, masked, err = freshPlanAssignment(rows, primaries, "")
	if err != nil {
		t.Fatal(err)
	}
	if masked != 1 {
		t.Errorf("masked = %d, want 1 (gmail with no explicit lane)", masked)
	}
	for cell := range got {
		if cell.Lane == "GMAIL" {
			t.Errorf("gmail cell %v staged despite no explicit lane", cell)
		}
	}
}

func TestFreshLaneOf(t *testing.T) {
	if freshLaneOf("sbcglobal") != "SBCGLOBAL" || freshLaneOf("Comcast") != "COMCAST" {
		t.Error("known families must map to their lane tokens")
	}
	for _, unknown := range []string{"charter", "verizon", "", "other"} {
		if got := freshLaneOf(unknown); got != "OTHER" {
			t.Errorf("laneOf(%q) = %q, want OTHER (never dropped)", unknown, got)
		}
	}
}

// ── SES friendly-from transform (jul21 doctrine) ─────────────────────────────

func TestFreshFromNameTransform(t *testing.T) {
	// Brand SES cell: "[Publication] featuring [Partner]".
	if got := freshFromName("Refinance Rates USA", "West Capital Lending", false); got != "Refinance Rates USA featuring West Capital Lending" {
		t.Errorf("brand-cell FF = %q", got)
	}
	// Explicit first-party lane: proof FF verbatim — it IS the brand.
	if got := freshFromName("WCL", "West Capital Lending", true); got != "West Capital Lending" {
		t.Errorf("explicit-lane FF = %q, want verbatim", got)
	}
}

// ── draw SQL shape (mirrors the python reference predicates) ─────────────────

func TestFreshQueueDrawSQLShape(t *testing.T) {
	q := freshQueueDrawSQL(true, true)
	for _, want := range []string{
		"FROM partner_clean_queue q",
		"q.status = 'ready'",
		"q.eo_result = ANY($2)",
		"q.dataset_id = ANY($1::uuid[])",
		"d.paused_emergency", // emergency-pause anti-join
		"COALESCE(q.last_pushed_at, q.ingested_at) DESC", // freshest-first by re-push
		"lower(COALESCE(q.isp_family, 'other')) <> ALL($3::text[])",
		"LIMIT $4",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("queue draw SQL missing %q:\n%s", want, q)
		}
	}
	// Without exclusions the LIMIT takes the next placeholder.
	q = freshQueueDrawSQL(false, true)
	if !strings.Contains(q, "LIMIT $3") || strings.Contains(q, "<> ALL") {
		t.Errorf("unexcluded queue draw SQL placeholders wrong:\n%s", q)
	}
}

func TestFreshTagDrawSQLShape(t *testing.T) {
	ispCase := isp.SQLCaseFromEmail("s.email")
	q := freshTagDrawSQL(ispCase, true, true)
	for _, want := range []string{
		"FROM mailing_subscribers s",
		"s.tags && $1::text[]",
		"s.status IN ('pending','confirmed')",
		"COALESCE(s.total_emails_received, 0) = 0", // never-mailed
		"s.last_email_at IS NULL",
		"FROM mailing_eo_validation v",  // EO gate governs supply
		"v.status = ANY($2)",
		"mailing_global_suppressions",   // unsuppressed
		"seg.name LIKE $3",              // prior-batch exclusion = the claim
		"ORDER BY s.created_at DESC",
		"LIMIT $5",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("tag draw SQL missing %q:\n%s", want, q)
		}
	}
	if !strings.Contains(q, "CASE") {
		t.Errorf("tag draw SQL must embed the canonical ISP CASE")
	}
}

// ── draft campaign input invariants ──────────────────────────────────────────

func testStreamCfg() freshStreamConfig {
	return freshStreamConfig{
		StreamKey: "wcm", DailyCap: 15000, Offer: "west-capital-heloc",
		ThrottleHours: 12, Label: "West Capital (WCM homeowners)",
		SegPrefix: "WCM", VerticalTag: "vertical:mortgage",
		PrimarySites: []string{"RR", "RB", "FC"},
		EOMailable:   []string{"Verified"},
		SendingDomain: "m.wcl-heloc.com", SendingProfileID: "4df6545a-d623-4c18-abe1-195b7b26e463",
		SourceTags: []string{"batch:wcm_heloc_001"},
	}
}

func TestBuildFreshCampaignInputDraftOnlyInvariants(t *testing.T) {
	cfg := testStreamCfg()
	date := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	cells := map[freshCell][]freshDrawRow{
		{Site: "WCL", Lane: "GMAIL"}: {{Email: "a"}, {Email: "b"}},
		{Site: "WCL", Lane: "YAHOO"}: {{Email: "c"}},
		{Site: "RR", Lane: "APPLE"}:  {{Email: "d"}}, // other site — must not leak in
	}
	dest := freshDestination{Site: "WCL", Label: "WCL", BrandLabel: "WCL",
		Domain: "m.wcl-heloc.com", ProfileID: "4df6545a-d623-4c18-abe1-195b7b26e463", ExplicitLane: true}
	copySrc := &freshApprovedCopy{Subject: "s", Preheader: "p", FromName: "West Capital Lending", HTML: "<html>x</html>"}

	input, total := buildFreshCampaignInput(cfg, dest, "offer-uuid", copySrc, cells, date)

	if total != 3 {
		t.Errorf("total = %d, want 3 (only WCL cells)", total)
	}
	if input.Name != "jul28 - WCL - FRESH-BCAST-WCM - west-capital-heloc" {
		t.Errorf("name = %q", input.Name)
	}
	if input.OfferID != "offer-uuid" {
		t.Error("offer_id must be stamped — NULL offer_id means suppression never fires")
	}
	if input.SendMode != "scheduled" || input.ScheduledAt == nil {
		t.Error("drafts carry send_mode=scheduled + scheduled_at")
	}
	if input.ContentLocked == nil || !*input.ContentLocked {
		t.Error("content_locked must be true (approved creative byte-faithful)")
	}
	if input.UseMasterSelection == nil || *input.UseMasterSelection {
		t.Error("use_master_selection must be false (segment-driven audience)")
	}
	// Footgun #1: any time-span source other than duration-calc/manual
	// silently fails the campaign at planning.
	if len(input.ISPPlans) != 2 {
		t.Fatalf("isp_plans = %d, want 2", len(input.ISPPlans))
	}
	for _, p := range input.ISPPlans {
		if len(p.TimeSpans) != 1 || p.TimeSpans[0].Source != "duration-calc" {
			t.Errorf("isp %s time_spans source = %+v, want duration-calc", p.ISP, p.TimeSpans)
		}
		if p.TimeSpans[0].EndAt.Sub(*p.TimeSpans[0].StartAt) != 12*time.Hour {
			t.Errorf("window must equal throttle_hours (12h)")
		}
	}
	// Inclusion segments are the deterministic uuid5 ids.
	wantSeg := freshSegID("WCM-J28-WCL-GMAIL")
	found := false
	for _, s := range input.InclusionSegments {
		if s == wantSeg {
			found = true
		}
	}
	if !found || len(input.InclusionSegments) != 2 {
		t.Errorf("inclusion segments %v must be the uuid5 ids of THIS site's cells (want %s among 2)", input.InclusionSegments, wantSeg)
	}
	// Quotas mirror per-lane counts.
	q := map[string]int{}
	for _, iq := range input.ISPQuotas {
		q[iq.ISP] = iq.Volume
	}
	if q["gmail"] != 2 || q["yahoo"] != 1 {
		t.Errorf("isp_quotas = %v", q)
	}
	if len(input.ExclusionLists) != 1 || input.ExclusionLists[0] != "global-suppression-list" {
		t.Errorf("exclusion_lists = %v", input.ExclusionLists)
	}
}

// ── idempotent (resume-safe) skip ────────────────────────────────────────────

func TestFreshDatedSegmentsExistSkip(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := &FreshBroadcastRunner{db: db}
	org := uuid.New()

	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs(org, "CONSUMER-J28-%").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	exists, err := r.datedSegmentsExist(t.Context(), org, "CONSUMER", "J28")
	if err != nil || !exists {
		t.Fatalf("exists=%v err=%v, want true,nil", exists, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// ── copy-source query shape (the operator-approved pool, most recent first) ──

func TestFreshApprovedCopySQLShape(t *testing.T) {
	for _, want := range []string{
		"FROM mailing_offer_proofs p",
		"p.approval_status = 'approved'",
		"p.is_active",
		"p.offer_key = $2",
		"ORDER BY COALESCE(p.approved_at, p.updated_at) DESC",
		"LIMIT 1",
	} {
		if !strings.Contains(freshApprovedCopySQL, want) {
			t.Errorf("approved-copy SQL missing %q", want)
		}
	}
}

// ── config source-type derivation ────────────────────────────────────────────

func TestFreshStreamTagSourced(t *testing.T) {
	c := testStreamCfg()
	if !c.tagSourced() {
		t.Error("wcm shape (no datasets + source_tags) must be tag-sourced")
	}
	c.DatasetIDs = []string{"9502c7c4-68e7-4dcf-91f5-103a1480fe68"}
	if c.tagSourced() {
		t.Error("queue streams (dataset_ids present) are never tag-sourced")
	}
}

// ── route registration (wiring proof: registered ≠ dead code) ────────────────

func TestFreshBroadcastRoutesRegistered(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The service constructor probes the campaign column cache once.
	mock.ExpectQuery(`information_schema\.columns`).
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("id"))
	// Status handler queries: stream config, then recent runs.
	org := uuid.New()
	mock.ExpectQuery(`FROM mailing_stream_broadcast_config`).
		WithArgs(org).
		WillReturnRows(sqlmock.NewRows([]string{"stream_key", "enabled", "auto_stage"}).
			AddRow("consumer", true, false))
	mock.ExpectQuery(`FROM mailing_fresh_broadcast_runs`).
		WithArgs(org).
		WillReturnRows(sqlmock.NewRows([]string{"id", "run_date", "dry", "trigger_source", "status", "results", "created_at"}))

	r := chi.NewRouter()
	svc := NewFreshBroadcastRunService(db)
	svc.RegisterRoutes(r)

	// Registered route + org header → 200 with the status payload.
	req := httptest.NewRequest("GET", "/fresh-broadcast/status", nil)
	req.Header.Set("X-Organization-ID", org.String())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET /fresh-broadcast/status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"consumer"`) {
		t.Errorf("status payload missing stream row: %s", rec.Body.String())
	}

	// Unregistered sibling path must 404 (proves the mux differentiates).
	req = httptest.NewRequest("GET", "/fresh-broadcast/bogus", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Errorf("bogus route = %d, want 404", rec.Code)
	}
}
