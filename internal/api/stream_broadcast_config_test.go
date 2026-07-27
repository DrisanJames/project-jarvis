package api

// Fresh Broadcast Config tests — PUT validation paths (unknown offer refuses,
// draft offer warns, negative cap refuses, throttle bounds, stale updated_at
// 409) and bench readiness derivation. sqlmock only, no DB.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
)

const streamCfgTestOrg = "11111111-2222-3333-4444-555555555555"

func streamCfgRouter(svc *StreamBroadcastService) *chi.Mux {
	r := chi.NewRouter()
	svc.RegisterRoutes(r)
	return r
}

func offerRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "name", "landing_page_slug", "status"})
}

func streamCfgCols() []string {
	return []string{"stream_key", "enabled", "daily_cap", "isp_caps", "offer", "throttle_hours",
		"label", "seg_prefix", "vertical_tag", "dataset_ids", "primary_sites",
		"secondary_sites", "eo_mailable", "sending_domain", "sending_profile_id",
		"updated_at", "updated_by"}
}

// ── Pure derivations ────────────────────────────────────────────────────────

func TestSlugifyOfferName(t *testing.T) {
	cases := map[string]string{
		"3 Day Blinds":             "3-day-blinds",
		"Liberty Mutual Insurance": "liberty-mutual-insurance",
		"Tahiti Village Resort":    "tahiti-village-resort",
		"  Fidelity Life ":         "fidelity-life",
		"Mutual of Omaha":          "mutual-of-omaha",
	}
	for in, want := range cases {
		if got := slugifyOfferName(in); got != want {
			t.Errorf("slugifyOfferName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBenchReadinessLevel(t *testing.T) {
	cases := []struct {
		name                       string
		exists                     bool
		status                     string
		proofs, smartlinks         int
		want                       string
	}{
		{"no offer row = NOT ONBOARDED", false, "", 0, 0, benchNotOnboarded},
		{"active + proofs + smartlink = READY", true, "active", 2, 1, benchReady},
		{"draft status = ATTENTION (fidelity today)", true, "draft", 2, 1, benchAttention},
		{"active but no proofs = ATTENTION", true, "active", 0, 1, benchAttention},
		{"active but no smartlink = ATTENTION", true, "active", 3, 0, benchAttention},
	}
	for _, c := range cases {
		if got := benchReadinessLevel(c.exists, c.status, c.proofs, c.smartlinks); got != c.want {
			t.Errorf("%s: readiness = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestValidateStreamConfigWrite(t *testing.T) {
	if msg := validateStreamConfigWrite(100, 8, map[string]int{"apple": 0}, "tahiti-village"); msg != "" {
		t.Errorf("valid write rejected: %s", msg)
	}
	if msg := validateStreamConfigWrite(-1, 8, nil, "x"); !strings.Contains(msg, "daily_cap") {
		t.Errorf("negative cap not refused: %q", msg)
	}
	if msg := validateStreamConfigWrite(0, 0, nil, "x"); !strings.Contains(msg, "throttle_hours") {
		t.Errorf("throttle 0 not refused: %q", msg)
	}
	if msg := validateStreamConfigWrite(0, 25, nil, "x"); !strings.Contains(msg, "throttle_hours") {
		t.Errorf("throttle 25 not refused: %q", msg)
	}
	if msg := validateStreamConfigWrite(0, 8, map[string]int{"apple": -5}, "x"); !strings.Contains(msg, "isp_caps[apple]") {
		t.Errorf("negative isp cap not refused: %q", msg)
	}
	if msg := validateStreamConfigWrite(0, 8, nil, "  "); !strings.Contains(msg, "offer") {
		t.Errorf("blank offer not refused: %q", msg)
	}
}

// ── PUT handler paths ───────────────────────────────────────────────────────

func putStreamCfg(t *testing.T, svc *StreamBroadcastService, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/stream-broadcast/config", strings.NewReader(body))
	req.Header.Set("X-Organization-ID", streamCfgTestOrg)
	rec := httptest.NewRecorder()
	streamCfgRouter(svc).ServeHTTP(rec, req)
	return rec
}

func TestStreamConfigPut_NegativeCapRefusedBeforeDB(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	// No expectations: validation must refuse before any query runs.
	svc := NewStreamBroadcastService(db)
	rec := putStreamCfg(t, svc, `{"stream_key":"consumer","daily_cap":-5,"throttle_hours":8,
		"offer":"tahiti-village","expected_updated_at":"2026-07-26T10:00:00Z"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("DB was touched on a validation failure: %v", err)
	}
}

func TestStreamConfigPut_ThrottleBoundsRefused(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewStreamBroadcastService(db)
	rec := putStreamCfg(t, svc, `{"stream_key":"consumer","daily_cap":10,"throttle_hours":25,
		"offer":"tahiti-village","expected_updated_at":"2026-07-26T10:00:00Z"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "throttle_hours") {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestStreamConfigPut_UnknownOfferRefused(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	// Offer resolution scans mailing_offers; no row matches liz-buys-homes.
	mock.ExpectQuery(`FROM mailing_offers`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(offerRows().
			AddRow("o1", "Tahiti Village Resort", "tahiti-village", "active"))

	svc := NewStreamBroadcastService(db)
	rec := putStreamCfg(t, svc, `{"stream_key":"consumer","daily_cap":100,"throttle_hours":8,
		"offer":"liz-buys-homes","expected_updated_at":"2026-07-26T10:00:00Z"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not onboarded") {
		t.Errorf("body should say not onboarded: %s", rec.Body.String())
	}
}

func TestStreamConfigPut_DraftOfferWarnsButSaves(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	now := time.Now()
	mock.ExpectQuery(`FROM mailing_offers`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(offerRows().
			AddRow("o-fid", "Fidelity Life", "", "draft"))
	mock.ExpectQuery(`UPDATE mailing_stream_broadcast_config`).
		WithArgs(sqlmock.AnyArg(), "term_life", sqlmock.AnyArg(), true, 10000,
			`{"apple":0}`, "fidelity", 8, "op@x.com").
		WillReturnRows(sqlmock.NewRows(streamCfgCols()).
			AddRow("term_life", true, 10000, `{"apple":0}`, "fidelity", 8,
				"Term Life", "TERMLIFE", nil, `[]`, `["YI"]`, `["CI"]`,
				`["Verified","Complainer"]`, nil, nil, now, "op@x.com"))

	svc := NewStreamBroadcastService(db)
	rec := putStreamCfg(t, svc, `{"stream_key":"term_life","enabled":true,"daily_cap":10000,
		"throttle_hours":8,"isp_caps":{"apple":0},"offer":"fidelity",
		"expected_updated_at":"2026-07-26T10:00:00Z","updated_by":"op@x.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out streamConfigPutResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "'draft'") {
		t.Errorf("expected one draft warning, got %+v", out.Warnings)
	}
	if out.Stream.StreamKey != "term_life" || out.Stream.ISPCaps["apple"] != 0 {
		t.Errorf("stream row wrong: %+v", out.Stream)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestStreamConfigPut_StaleUpdatedAt409(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(`FROM mailing_offers`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(offerRows().
			AddRow("o1", "Tahiti Village Resort", "tahiti-village", "active"))
	// Optimistic-lock UPDATE matches no row (stale updated_at)…
	mock.ExpectQuery(`UPDATE mailing_stream_broadcast_config`).
		WillReturnRows(sqlmock.NewRows(streamCfgCols())) // zero rows → ErrNoRows
	// …but the stream row itself exists → 409, not 404.
	mock.ExpectQuery(`SELECT 1 FROM mailing_stream_broadcast_config`).
		WithArgs(sqlmock.AnyArg(), "consumer").
		WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))

	svc := NewStreamBroadcastService(db)
	rec := putStreamCfg(t, svc, `{"stream_key":"consumer","enabled":true,"daily_cap":60000,
		"throttle_hours":8,"offer":"tahiti-village","expected_updated_at":"2026-07-20T00:00:00Z"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s (want 409)", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestStreamConfigPut_UnknownStream404(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery(`FROM mailing_offers`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(offerRows().
			AddRow("o1", "Tahiti Village Resort", "tahiti-village", "active"))
	mock.ExpectQuery(`UPDATE mailing_stream_broadcast_config`).
		WillReturnRows(sqlmock.NewRows(streamCfgCols()))
	mock.ExpectQuery(`SELECT 1 FROM mailing_stream_broadcast_config`).
		WithArgs(sqlmock.AnyArg(), "nope").
		WillReturnRows(sqlmock.NewRows([]string{"one"})) // no row

	svc := NewStreamBroadcastService(db)
	rec := putStreamCfg(t, svc, `{"stream_key":"nope","enabled":true,"daily_cap":1,
		"throttle_hours":8,"offer":"tahiti-village","expected_updated_at":"2026-07-20T00:00:00Z"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s (want 404)", rec.Code, rec.Body.String())
	}
}

// ── GET + bench readiness over sqlmock ──────────────────────────────────────

func TestStreamConfigGet_BenchLights(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	now := time.Now()

	mock.ExpectQuery(`FROM mailing_stream_broadcast_config`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(streamCfgCols()).
			AddRow("consumer", true, 60000, `{}`, "tahiti-village", 8,
				"Consumer (Attribits Spicy Clickers)", "CONSUMER", nil,
				`["9502c7c4"]`, `["DB","CP"]`, `["BW"]`, `["Verified","Complainer"]`,
				nil, nil, now, "seed-migration").
			AddRow("wcm", true, 15000, `{}`, "west-capital-heloc", 12,
				"West Capital (WCM homeowners)", "WCM", "vertical:mortgage",
				`[]`, `[]`, `[]`, `["Verified","Complainer"]`,
				"m.wcl-heloc.com", nil, now, "seed-migration"))

	// Bench: 6 offers, each = ResolveOffer scan (+ counts when resolved).
	// The offers-table scan is identical per key; liz-buys-homes resolves to
	// nothing so it has no counts query.
	offerSet := func() *sqlmock.Rows {
		return offerRows().
			AddRow("o-fid", "Fidelity Life", "", "draft").
			AddRow("o-lib", "Liberty Mutual Insurance", "", "active").
			AddRow("o-tah", "Tahiti Village Resort", "tahiti-village", "active").
			AddRow("o-3db", "3 Day Blinds", "", "active").
			AddRow("o-moo", "Mutual of Omaha", "", "active").
			AddRow("o-wch", "West Capital HELOC", "", "active")
	}
	countsRow := func(proofs, links int) *sqlmock.Rows {
		return sqlmock.NewRows([]string{"proofs", "links"}).AddRow(proofs, links)
	}
	// fidelity (draft, has proofs+link → still ATTENTION because draft)
	mock.ExpectQuery(`FROM mailing_offers`).WithArgs(sqlmock.AnyArg()).WillReturnRows(offerSet())
	mock.ExpectQuery(`FROM mailing_offer_proofs`).WithArgs(sqlmock.AnyArg(), "fidelity").WillReturnRows(countsRow(2, 1))
	// liberty-mutual (active, ready)
	mock.ExpectQuery(`FROM mailing_offers`).WithArgs(sqlmock.AnyArg()).WillReturnRows(offerSet())
	mock.ExpectQuery(`FROM mailing_offer_proofs`).WithArgs(sqlmock.AnyArg(), "liberty-mutual").WillReturnRows(countsRow(3, 2))
	// tahiti-village (active, ready via slug match)
	mock.ExpectQuery(`FROM mailing_offers`).WithArgs(sqlmock.AnyArg()).WillReturnRows(offerSet())
	mock.ExpectQuery(`FROM mailing_offer_proofs`).WithArgs(sqlmock.AnyArg(), "tahiti-village").WillReturnRows(countsRow(1, 1))
	// 3-day-blinds (active but no smartlink → attention)
	mock.ExpectQuery(`FROM mailing_offers`).WithArgs(sqlmock.AnyArg()).WillReturnRows(offerSet())
	mock.ExpectQuery(`FROM mailing_offer_proofs`).WithArgs(sqlmock.AnyArg(), "3-day-blinds").WillReturnRows(countsRow(1, 0))
	// mutual-of-omaha (active, ready)
	mock.ExpectQuery(`FROM mailing_offers`).WithArgs(sqlmock.AnyArg()).WillReturnRows(offerSet())
	mock.ExpectQuery(`FROM mailing_offer_proofs`).WithArgs(sqlmock.AnyArg(), "mutual-of-omaha").WillReturnRows(countsRow(1, 1))
	// liz-buys-homes: no row → NOT ONBOARDED, no counts query.
	mock.ExpectQuery(`FROM mailing_offers`).WithArgs(sqlmock.AnyArg()).WillReturnRows(offerSet())
	// west-capital-heloc (active internal offer, exact slugified-name match)
	mock.ExpectQuery(`FROM mailing_offers`).WithArgs(sqlmock.AnyArg()).WillReturnRows(offerSet())
	mock.ExpectQuery(`FROM mailing_offer_proofs`).WithArgs(sqlmock.AnyArg(), "west-capital-heloc").WillReturnRows(countsRow(1, 1))

	svc := NewStreamBroadcastService(db)
	req := httptest.NewRequest(http.MethodGet, "/stream-broadcast/config", nil)
	req.Header.Set("X-Organization-ID", streamCfgTestOrg)
	rec := httptest.NewRecorder()
	streamCfgRouter(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out streamConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.BenchAvailable || len(out.Bench) != 7 || len(out.Streams) != 2 {
		t.Fatalf("shape wrong: bench_available=%v bench=%d streams=%d",
			out.BenchAvailable, len(out.Bench), len(out.Streams))
	}
	byKey := map[string]benchOfferStatus{}
	for _, b := range out.Bench {
		byKey[b.Key] = b
	}
	if b := byKey["fidelity"]; b.Readiness != benchAttention || b.Status != "draft" || b.MatchedBy != "name_prefix" {
		t.Errorf("fidelity = %+v (want attention/draft/name_prefix)", b)
	}
	if b := byKey["tahiti-village"]; b.Readiness != benchReady || b.MatchedBy != "slug" {
		t.Errorf("tahiti-village = %+v (want ready/slug)", b)
	}
	if b := byKey["3-day-blinds"]; b.Readiness != benchAttention || b.MatchedBy != "name" {
		t.Errorf("3-day-blinds = %+v (want attention/name)", b)
	}
	if b := byKey["liz-buys-homes"]; b.Readiness != benchNotOnboarded || b.Exists {
		t.Errorf("liz-buys-homes = %+v (want not_onboarded)", b)
	}
	if b := byKey["west-capital-heloc"]; b.Readiness != benchReady || b.MatchedBy != "name" {
		t.Errorf("west-capital-heloc = %+v (want ready/name)", b)
	}
	// wcm stream row carries its explicit sending lane, read-only.
	wcm := out.Streams[1]
	if wcm.StreamKey != "wcm" || wcm.SendingDomain == nil || *wcm.SendingDomain != "m.wcl-heloc.com" || wcm.SendingProfileID != nil {
		t.Errorf("wcm stream = %+v (want sending_domain m.wcl-heloc.com, nil profile)", wcm)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
