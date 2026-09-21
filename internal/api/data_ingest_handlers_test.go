package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/dataingest"
)

const diOrg = "00000000-0000-0000-0000-000000000001"

// newDIRouter mirrors production: the service mounted at /api/mailing/data-ingest.
// rdb is nil here on purpose — that is the state the "not_measured" contract
// exists for, and it is the state on any task booted without Redis.
func newDIRouter(t *testing.T) (http.Handler, sqlmock.Sqlmock, *DataIngestService) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc := NewDataIngestService(db, nil)
	svc.now = func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) }
	r := chi.NewRouter()
	r.Route("/api/mailing", func(mr chi.Router) {
		mr.Route("/data-ingest", svc.RegisterRoutes)
	})
	return r, mock, svc
}

func diDo(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Organization-ID", diOrg)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// sessionHdr is what apiAuthMiddleware stamps on a session-authenticated
// request (routes.go:46-49) after DELETING any inbound copy.
func sessionHdr() map[string]string { return map[string]string{"X-User-Email": "ops@example.com"} }

// Closed by default: no session header, no admin key => 401, with the JSON
// error envelope every handler in this package uses.
func TestDataIngestAuth_401WithoutKeyOrSession(t *testing.T) {
	h, _, _ := newDIRouter(t)
	for _, path := range []string{
		"/api/mailing/data-ingest/day",
		"/api/mailing/data-ingest/state",
		"/api/mailing/data-ingest/feeds",
	} {
		rec := diDo(h, http.MethodGet, path, "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: want 401, got %d %s", path, rec.Code, rec.Body.String())
		}
		var e map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e["error"] == "" {
			t.Fatalf("%s: want {\"error\":...}, got %s", path, rec.Body.String())
		}
	}
	// An ADMIN_API_KEY that is UNSET must not authorize a blank header.
	rec := diDo(h, http.MethodGet, "/api/mailing/data-ingest/state", "", map[string]string{"X-Admin-Key": ""})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("blank admin key: want 401, got %d", rec.Code)
	}
}

func TestDataIngestAuth_AdminKeyOpens(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "k")
	h, mock, _ := newDIRouter(t)
	mock.MatchExpectationsInOrder(false)
	expectDayQueries(mock)
	rec := diDo(h, http.MethodGet, "/api/mailing/data-ingest/day", "", map[string]string{"X-Admin-Key": "k"})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
}

// expectDayQueries stubs the three table reads /day makes with Redis dark:
// the cached reservoir scan, the object accounting, and the 30-day rollup.
func expectDayQueries(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SET LOCAL statement_timeout")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM partner_clean_queue")).
		WillReturnRows(sqlmock.NewRows([]string{"dataset_id", "status", "n"}))
	mock.ExpectQuery(regexp.QuoteMeta("FROM data_ingest_static_objects")).
		WillReturnRows(sqlmock.NewRows([]string{"org", "n"}))
	mock.ExpectQuery(regexp.QuoteMeta("FROM partner_inbound_batches b")).
		WillReturnRows(sqlmock.NewRows([]string{"org", "with_object", "without"}))
	mock.ExpectRollback()
	mock.ExpectQuery(regexp.QuoteMeta("FROM data_ingest_rollup")).
		WillReturnRows(sqlmock.NewRows([]string{"day", "supply_class", "n"}))
}

// With no Redis, /day must say the live classes are NOT MEASURED. A zero here
// would read as "nothing arrived today", which is the exact lie the field
// markers exist to prevent.
func TestDataIngestDay_NotMeasuredWhenRedisNil(t *testing.T) {
	h, mock, _ := newDIRouter(t)
	mock.MatchExpectationsInOrder(false)
	expectDayQueries(mock)

	rec := diDo(h, http.MethodGet, "/api/mailing/data-ingest/day?date=2026-09-20", "", sessionHdr())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		AsOf   string            `json:"as_of"`
		Source string            `json:"source"`
		Fields map[string]string `json:"fields"`
		Date   string            `json:"date"`
		AtRest map[string]*int64 `json:"at_rest"`
		Dyn    map[string]any    `json:"dynamic"`
		Xfer   map[string]*int64 `json:"internal_transfer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	if got.AsOf == "" || got.Source == "" {
		t.Fatalf("envelope missing as_of/source: %s", rec.Body.String())
	}
	if got.Date != "2026-09-20" {
		t.Fatalf("date %q", got.Date)
	}
	// Every counter-sourced field is not_measured AND null — never 0.
	for _, f := range []string{"landed", "arrived", "n", "yesterday", "feeds_live", "composition", "series"} {
		if got.Fields[f] != fieldNotMeasured {
			t.Fatalf("%s should be %s, got %q", f, fieldNotMeasured, got.Fields[f])
		}
	}
	if got.AtRest["landed"] != nil {
		t.Fatalf("at_rest.landed must be null when unmeasured, got %d", *got.AtRest["landed"])
	}
	if got.Xfer["n"] != nil {
		t.Fatalf("internal_transfer.n must be null when unmeasured")
	}
	if v, ok := got.Dyn["arrived"]; !ok || v != nil {
		t.Fatalf("dynamic.arrived must be present and null, got %v", got.Dyn["arrived"])
	}
	// The fields map keys must be the VALUE keys, flat — the portal looks a
	// tile up by its own name.
	for _, k := range []string{"raw", "staged", "inflight", "mailed", "removed",
		"static_objects", "loads_without_object", "parked_in_db", "arrival_rate"} {
		if _, ok := got.Fields[k]; !ok {
			t.Fatalf("fields is missing the key %q: %v", k, got.Fields)
		}
	}
}

// A bad ?date= is a 400, never a silent answer for today.
func TestDataIngestDay_RejectsBadDate(t *testing.T) {
	h, _, _ := newDIRouter(t)
	rec := diDo(h, http.MethodGet, "/api/mailing/data-ingest/day?date=20260920", "", sessionHdr())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d %s", rec.Code, rec.Body.String())
	}
}

// /feeds composes the three switches that can each independently close a feed:
// dataset status, paused_emergency, and the partner's own status. The drip-state
// row presence becomes send_row.
func TestDataIngestFeeds_StatusComposite(t *testing.T) {
	h, mock, _ := newDIRouter(t)
	ds1, ds2 := uuid.NewString(), uuid.NewString()
	p1 := uuid.NewString()

	cols := []string{"id", "name", "slug", "vertical", "status", "paused_emergency",
		"express", "pid", "pname", "pstatus", "has_drip_state", "has_contract",
		"supply_class", "source_path", "received_at"}
	recv := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("FROM partner_datasets d")).WithArgs(diOrg).
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow(ds1, "Feed One", "feed-one", "refi_heloc", "active", false, true,
				p1, "Partner A", "active", true, true, "dynamic", "partner_api", recv).
			AddRow(ds2, "Feed Two", "feed-two", "remodel", "active", true, false,
				p1, "Partner A", "active", false, false, "at_rest", "csv_upload", nil))

	// The ONE heavy query, behind the 60s cache.
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SET LOCAL statement_timeout")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM partner_clean_queue")).
		WillReturnRows(sqlmock.NewRows([]string{"dataset_id", "status", "n"}).
			AddRow(ds1, "ready", int64(120)).
			AddRow(ds1, "pending_eo", int64(30)).
			AddRow(ds1, "mailed", int64(900)).
			AddRow(ds2, "held", int64(7)))
	mock.ExpectQuery(regexp.QuoteMeta("FROM data_ingest_static_objects")).
		WillReturnRows(sqlmock.NewRows([]string{"org", "n"}))
	mock.ExpectQuery(regexp.QuoteMeta("FROM partner_inbound_batches b")).
		WillReturnRows(sqlmock.NewRows([]string{"org", "with_object", "without"}))
	mock.ExpectRollback()

	rec := diDo(h, http.MethodGet, "/api/mailing/data-ingest/feeds", "", sessionHdr())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Fields map[string]string `json:"fields"`
		Feeds  []struct {
			DatasetID     string `json:"dataset_id"`
			PartnerID     string `json:"partner_id"`
			Name          string `json:"name"`
			Lane          string `json:"lane"`
			SupplyClass   string `json:"supply_class"`
			SourceChannel string `json:"source_channel"`
			Status        struct {
				IngestOpen bool `json:"ingest_open"`
				SendRow    bool `json:"send_row"`
				Express    bool `json:"express"`
				Contract   bool `json:"contract"`
			} `json:"status"`
			Raw       *int64 `json:"raw"`
			Staged    *int64 `json:"staged"`
			InFlight  *int64 `json:"inflight"`
			Mailed    *int64 `json:"mailed"`
			Records   *int64 `json:"records"`
			Consumed  *int64 `json:"consumed"`
			Remaining *int64 `json:"remaining"`
			Today     *int64 `json:"today"`
			Yesterday *int64 `json:"yesterday"`
			LastEvent string `json:"last_event"`
		} `json:"feeds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	if len(got.Feeds) != 2 {
		t.Fatalf("want 2 feeds, got %d", len(got.Feeds))
	}
	f1, f2 := got.Feeds[0], got.Feeds[1]
	if !f1.Status.IngestOpen || !f1.Status.SendRow || !f1.Status.Express || !f1.Status.Contract {
		t.Fatalf("feed one status wrong: %+v", f1.Status)
	}
	if f1.SupplyClass != "dynamic" || f1.SourceChannel != "partner_api" || f1.PartnerID != p1 {
		t.Fatalf("feed one identity wrong: %+v", f1)
	}
	if *f1.Staged != 120 || *f1.Raw != 30 || *f1.Mailed != 900 || *f1.Records != 1050 {
		t.Fatalf("feed one counts wrong: raw=%d staged=%d mailed=%d records=%d",
			*f1.Raw, *f1.Staged, *f1.Mailed, *f1.Records)
	}
	// paused_emergency closes the door even though the dataset row says active.
	if f2.Status.IngestOpen {
		t.Fatalf("paused_emergency feed must NOT be ingest_open: %+v", f2.Status)
	}
	if f2.Status.SendRow || f2.Status.Contract {
		t.Fatalf("no drip-state / no active contract => both false: %+v", f2.Status)
	}
	if *f2.Raw != 7 {
		t.Fatalf("held must fold into raw, got %d", *f2.Raw)
	}
	// Redis is nil, so the live columns must be not_measured and null — never 0.
	if got.Fields["today"] != fieldNotMeasured || got.Fields["last_event"] != fieldNotMeasured {
		t.Fatalf("live fields should be not_measured: %v", got.Fields)
	}
	if f1.Today != nil || f1.Yesterday != nil {
		t.Fatalf("today/yesterday must be null when unmeasured")
	}
	// consumed/remaining have no measurement path yet and must say so.
	if got.Fields["consumed"] != fieldNotMeasured || got.Fields["remaining"] != fieldNotMeasured {
		t.Fatalf("consumed/remaining must be not_measured: %v", got.Fields)
	}
	if f1.Consumed != nil || f1.Remaining != nil {
		t.Fatalf("consumed/remaining must be null")
	}
	if got.Fields["staged"] != fieldDerived || got.Fields["records"] != fieldMeasured {
		t.Fatalf("count flags wrong: %v", got.Fields)
	}
}

// POST /events validates the CLOSED vocabulary at the door: a bad transition is
// a 400 at the Python writer's call site, not a DLQ record hours later.
func TestDataIngestEvents_RejectsBadTransition(t *testing.T) {
	h, _, _ := newDIRouter(t)
	body := `{"v":1,"op_id":"` + uuid.NewString() + `","at":"2026-09-20T10:00:00Z",` +
		`"supply_class":"dynamic","source_path":"partner_api","transition":"teleported","n":5}`
	rec := diDo(h, http.MethodPost, "/api/mailing/data-ingest/events", body, sessionHdr())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "transition") {
		t.Fatalf("error should name the bad field: %s", rec.Body.String())
	}

	// by_isp that does not sum to n is the silent under-count guard.
	bad := `{"v":1,"op_id":"` + uuid.NewString() + `","at":"2026-09-20T10:00:00Z",` +
		`"supply_class":"dynamic","source_path":"partner_api","transition":"landed",` +
		`"by_isp":{"gmail":3},"n":9}`
	if rec := diDo(h, http.MethodPost, "/api/mailing/data-ingest/events", bad, sessionHdr()); rec.Code != http.StatusBadRequest {
		t.Fatalf("by_isp mismatch: want 400, got %d %s", rec.Code, rec.Body.String())
	}

	// An unknown supply_class is rejected too (the CHECK constraint's twin).
	bad2 := `{"v":1,"op_id":"` + uuid.NewString() + `","at":"2026-09-20T10:00:00Z",` +
		`"supply_class":"borrowed","source_path":"partner_api","transition":"landed","n":1}`
	if rec := diDo(h, http.MethodPost, "/api/mailing/data-ingest/events", bad2, sessionHdr()); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad supply_class: want 400, got %d", rec.Code)
	}
}

// A well-formed event is ACCEPTED (202) with the bus dark — and the response
// says counted:not_measured, because Emit is fire-and-forget through a
// flag-gated tap and acceptance is not a promise that anything counted.
func TestDataIngestEvents_AcceptsValidAndAdmitsItIsNotCounted(t *testing.T) {
	h, _, _ := newDIRouter(t)
	op := uuid.NewString()
	body := `{"v":1,"op_id":"` + op + `","at":"2026-09-20T10:00:00Z",` +
		`"supply_class":"internal_transfer","source_path":"yahoo_family_inject",` +
		`"transition":"transfer","n":1200}`
	rec := diDo(h, http.MethodPost, "/api/mailing/data-ingest/events", body, sessionHdr())
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Accepted int               `json:"accepted"`
		OpIDs    []string          `json:"op_ids"`
		Fields   map[string]string `json:"fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Accepted != 1 || len(got.OpIDs) != 1 || got.OpIDs[0] != op {
		t.Fatalf("unexpected: %+v", got)
	}
	if got.Fields["counted"] != fieldNotMeasured {
		t.Fatalf("counted must be %s, got %q", fieldNotMeasured, got.Fields["counted"])
	}
}

// /rollup refuses rather than writing an empty day when there are no counters
// to roll up — a 200 with 0 rows would look like "the day was empty".
func TestDataIngestRollup_503WithoutCounters(t *testing.T) {
	h, _, _ := newDIRouter(t)
	rec := diDo(h, http.MethodPost, "/api/mailing/data-ingest/rollup?date=2026-09-19", "", sessionHdr())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d %s", rec.Code, rec.Body.String())
	}
}

// /state answers even when the heavy query fails, and says which tiles are not
// measured instead of rendering zeros.
func TestDataIngestState_NotMeasuredOnQueryFailure(t *testing.T) {
	h, mock, _ := newDIRouter(t)
	mock.MatchExpectationsInOrder(false)
	mock.ExpectBegin().WillReturnError(errTestNoDB)
	rec := diDo(h, http.MethodGet, "/api/mailing/data-ingest/state", "", sessionHdr())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Fields   map[string]string `json:"fields"`
		Tiles    map[string]*int64 `json:"tiles"`
		Counters struct {
			Running        bool   `json:"running"`
			LastHandledAt  string `json:"last_handled_at"`
			Applied        uint64 `json:"applied"`
			Duplicates     uint64 `json:"duplicates"`
			LagMax         int64  `json:"lag_max"`
			RedisAvailable bool   `json:"redis_available"`
			MeasuredToday  bool   `json:"measured_today"`
		} `json:"counters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"static_objects", "loads_without_object", "parked_in_db",
		"staged", "inflight", "mailed", "removed"} {
		if got.Fields[k] != fieldNotMeasured {
			t.Fatalf("%s should be %s, got %q", k, fieldNotMeasured, got.Fields[k])
		}
		if v, ok := got.Tiles[k]; !ok {
			t.Fatalf("tiles is missing %q", k)
		} else if v != nil {
			t.Fatalf("tile %s must be null when unmeasured, got %d", k, *v)
		}
	}
	if got.Counters.RedisAvailable || got.Counters.Running {
		t.Fatal("counters must report not-available / not-running with a nil client and a dark bus")
	}
}

var errTestNoDB = &diTestErr{"db down"}

type diTestErr struct{ s string }

func (e *diTestErr) Error() string { return e.s }

// SupplyClassFor is the DEFAULT for a source path, and it deliberately returns
// "" for the transition paths whose class belongs to the batch — guessing there
// would relabel a partner feed as at_rest the moment it hit the slicer.
func TestDataIngestSupplyClassFor(t *testing.T) {
	cases := map[string]string{
		dataingest.SourcePartnerAPI:        dataingest.ClassDynamic,
		dataingest.SourceSiteEvent:         dataingest.ClassDynamic,
		dataingest.SourceCSVUpload:         dataingest.ClassAtRest,
		dataingest.SourceStaticUpload:      dataingest.ClassAtRest,
		dataingest.SourceDrive:             dataingest.ClassAtRest,
		dataingest.SourceYahooFamilyInject: dataingest.ClassInternalTransfer,
		dataingest.SourceDripSupply:        dataingest.ClassInternalTransfer,
		dataingest.SourceHydration:         dataingest.ClassInternalTransfer,
		dataingest.SourceSlicer:            "",
		dataingest.SourceOrchestrator:      "",
	}
	for path, want := range cases {
		if got := dataingest.SupplyClassFor(path); got != want {
			t.Errorf("SupplyClassFor(%q) = %q, want %q", path, got, want)
		}
	}
}

// /loads carries the portal's exact keys: totals{landed,mailed,not_mailed,
// removed,duplicates}, per-load rows, ISP composition and the source breakdown.
// duplicates = what the batches DECLARED minus what actually landed as rows.
func TestDataIngestLoads_TotalsCompositionSources(t *testing.T) {
	h, mock, _ := newDIRouter(t)
	mock.MatchExpectationsInOrder(false)
	b1, b2 := uuid.NewString(), uuid.NewString()
	ds := uuid.NewString()
	recv := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("FROM partner_inbound_batches b")).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "dataset_id", "name", "supply_class", "source_path",
			"s3_bucket", "s3_key", "has_object", "record_count", "received_at"}).
			AddRow(b1, ds, "Feed One", "at_rest", "static_upload",
				"jarvis-partner-ingest", "static/ops/2026-09-20/a.csv", true, int64(110), recv).
			AddRow(b2, ds, "Feed One", "dynamic", "partner_api",
				"", "", false, int64(40), recv))

	mock.ExpectQuery(regexp.QuoteMeta("FROM partner_clean_queue")).
		WillReturnRows(sqlmock.NewRows([]string{"batch_id", "status", "n"}).
			AddRow(b1, "ready", int64(60)).
			AddRow(b1, "mailed", int64(30)).
			AddRow(b1, "suppressed_eo", int64(10)).
			AddRow(b2, "pending_eo", int64(40)))

	mock.ExpectQuery(regexp.QuoteMeta("COUNT(*) FILTER (WHERE status IN ('mailed','engaged'))")).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "n", "mailed"}).
			AddRow("gmail", int64(90), int64(20)).
			AddRow("microsoft", int64(50), int64(10)))

	rec := diDo(h, http.MethodGet, "/api/mailing/data-ingest/loads?date=2026-09-20", "", sessionHdr())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Fields map[string]string `json:"fields"`
		Totals map[string]*int64 `json:"totals"`
		Loads  []struct {
			BatchID string `json:"batch_id"`
			Object  bool   `json:"object"`
			Records int64  `json:"records"`
			Mailed  int64  `json:"mailed"`
			Staged  int64  `json:"staged"`
			Raw     int64  `json:"raw"`
			Removed int64  `json:"removed"`
		} `json:"loads"`
		Composition []struct {
			ISP    string `json:"isp"`
			N      int64  `json:"n"`
			Mailed *int64 `json:"mailed"`
		} `json:"composition"`
		Sources []struct {
			Name string `json:"name"`
			N    int64  `json:"n"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	if len(got.Loads) != 2 {
		t.Fatalf("want 2 loads, got %d", len(got.Loads))
	}
	// landed = rows that persisted (100 + 40); declared = 150 => 10 duplicates.
	if *got.Totals["landed"] != 140 {
		t.Fatalf("landed = %d, want 140", *got.Totals["landed"])
	}
	if *got.Totals["mailed"] != 30 || *got.Totals["removed"] != 10 {
		t.Fatalf("mailed/removed wrong: %d/%d", *got.Totals["mailed"], *got.Totals["removed"])
	}
	if *got.Totals["not_mailed"] != 100 {
		t.Fatalf("not_mailed = %d, want 100", *got.Totals["not_mailed"])
	}
	if *got.Totals["duplicates"] != 10 {
		t.Fatalf("duplicates = %d, want 10 (declared 150 - landed 140)", *got.Totals["duplicates"])
	}
	for _, k := range []string{"landed", "mailed", "not_mailed", "removed", "duplicates",
		"loads", "composition", "sources"} {
		if _, ok := got.Fields[k]; !ok {
			t.Fatalf("fields missing %q: %v", k, got.Fields)
		}
	}
	if len(got.Composition) != 2 || got.Composition[0].ISP != "gmail" || got.Composition[0].N != 90 {
		t.Fatalf("composition wrong: %+v", got.Composition)
	}
	if got.Composition[0].Mailed == nil || *got.Composition[0].Mailed != 20 {
		t.Fatalf("composition mailed wrong: %+v", got.Composition[0])
	}
	if len(got.Sources) != 2 || got.Sources[0].Name != "static_upload" || got.Sources[0].N != 100 {
		t.Fatalf("sources wrong: %+v", got.Sources)
	}
	// A load in the repository bucket has a real object; one with no bucket at
	// all is the DB-only case the red tile counts.
	if !got.Loads[0].Object || got.Loads[1].Object {
		t.Fatalf("object flags wrong: %+v", got.Loads)
	}
}

// /hours emits exactly 24 {hour, n} points and says not_measured with no Redis.
func TestDataIngestHours_ShapeAndNotMeasured(t *testing.T) {
	h, _, _ := newDIRouter(t)
	rec := diDo(h, http.MethodGet, "/api/mailing/data-ingest/hours?date=2026-09-20", "", sessionHdr())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Fields map[string]string `json:"fields"`
		Class  string            `json:"class"`
		Hours  []struct {
			Hour int   `json:"hour"`
			N    int64 `json:"n"`
		} `json:"hours"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Class != "dynamic" {
		t.Fatalf("default class should be dynamic, got %q", got.Class)
	}
	if got.Fields["hours"] != fieldNotMeasured {
		t.Fatalf("hours should be %s, got %q", fieldNotMeasured, got.Fields["hours"])
	}
	if len(got.Hours) != 0 {
		t.Fatalf("unmeasured hours must be empty, not 24 zeros: %d points", len(got.Hours))
	}
	if rec := diDo(h, http.MethodGet, "/api/mailing/data-ingest/hours?class=borrowed", "", sessionHdr()); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid class: want 400, got %d", rec.Code)
	}
}

// With the refresher owning the scan (prod), a request must NEVER run the
// partner_clean_queue GROUP BY: before the first background scan completes the
// reservoir tiles are not_measured, and sqlmock sees no query at all.
func TestDataIngestState_RefresherOnNeverScansInline(t *testing.T) {
	h, mock, svc := newDIRouter(t)
	svc.refresherOn = true // the flag StartQueueStateRefresher sets; no goroutine in tests

	rec := diDo(h, http.MethodGet, "/api/mailing/data-ingest/state", "", sessionHdr())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Fields map[string]string `json:"fields"`
		Tiles  map[string]*int64 `json:"tiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"parked_in_db", "staged", "inflight", "mailed", "removed", "static_objects", "loads_without_object"} {
		if got.Fields[f] != fieldNotMeasured {
			t.Errorf("field %s = %q, want not_measured while warming", f, got.Fields[f])
		}
		if got.Tiles[f] != nil {
			t.Errorf("tile %s = %d, want null while warming", f, *got.Tiles[f])
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected DB traffic with refresher on: %v", err)
	}

	// Once a snapshot exists it is served as-is, again with no scan.
	svc.mu.Lock()
	svc.cached = &queueStateSnapshot{GeneratedAt: time.Now(), ByStatus: map[string]int64{"held": 5, "ready": 7}, ByDataset: map[string]map[string]int64{}}
	svc.cachedAt = time.Now()
	svc.mu.Unlock()
	rec = diDo(h, http.MethodGet, "/api/mailing/data-ingest/state", "", sessionHdr())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Tiles["parked_in_db"] == nil || *got.Tiles["parked_in_db"] != 5 || got.Tiles["staged"] == nil || *got.Tiles["staged"] != 7 {
		t.Fatalf("snapshot not served: %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected DB traffic with snapshot cached: %v", err)
	}
}

// When the last-batch LATERAL times out, /feeds serves the wiring from the
// small tables and marks the three batch-derived columns not_measured — a 500
// would blank the whole feed list for one slow lookup.
func TestDataIngestFeeds_FallsBackWithoutLastBatch(t *testing.T) {
	h, mock, _ := newDIRouter(t)
	ds1 := uuid.NewString()
	p1 := uuid.NewString()
	cols := []string{"id", "name", "slug", "vertical", "status", "paused_emergency",
		"express", "pid", "pname", "pstatus", "has_drip_state", "has_contract",
		"supply_class", "source_path", "received_at"}
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY received_at DESC")).WithArgs(diOrg).
		WillReturnError(errors.New("pq: canceling statement due to statement timeout"))
	mock.ExpectQuery(regexp.QuoteMeta("''::text, ''::text, NULL::timestamptz")).WithArgs(diOrg).
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow(ds1, "Feed One", "feed-one", "refi_heloc", "active", false, true,
				p1, "Partner A", "active", true, true, "", "", nil))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SET LOCAL statement_timeout")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM partner_clean_queue")).
		WillReturnRows(sqlmock.NewRows([]string{"dataset_id", "status", "n"}).AddRow(ds1, "ready", int64(120)))
	mock.ExpectQuery(regexp.QuoteMeta("FROM data_ingest_static_objects")).
		WillReturnRows(sqlmock.NewRows([]string{"org", "n"}))
	mock.ExpectQuery(regexp.QuoteMeta("FROM partner_inbound_batches b")).
		WillReturnRows(sqlmock.NewRows([]string{"org", "with_object", "without"}))
	mock.ExpectRollback()

	rec := diDo(h, http.MethodGet, "/api/mailing/data-ingest/feeds", "", sessionHdr())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Fields map[string]string `json:"fields"`
		Note   string            `json:"note"`
		Feeds  []struct {
			DatasetID  string `json:"dataset_id"`
			LastLoaded string `json:"last_loaded"`
			Staged     *int64 `json:"staged"`
		} `json:"feeds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Feeds) != 1 || got.Feeds[0].DatasetID != ds1 {
		t.Fatalf("feeds = %s", rec.Body.String())
	}
	if got.Fields["last_loaded"] != fieldNotMeasured || got.Fields["supply_class"] != fieldNotMeasured {
		t.Errorf("batch-derived fields should be not_measured: %v", got.Fields)
	}
	if got.Note == "" || got.Feeds[0].LastLoaded != "" {
		t.Errorf("expected a note and no last_loaded, got note=%q last=%q", got.Note, got.Feeds[0].LastLoaded)
	}
	if got.Feeds[0].Staged == nil || *got.Feeds[0].Staged != 120 {
		t.Errorf("reservoir counts must still be served from the snapshot: %s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A timed-out day range on partner_inbound_batches degrades /loads to an
// honest empty answer (every derived total not_measured, note set), not a 500.
func TestDataIngestLoads_DegradesOnQueryError(t *testing.T) {
	h, mock, _ := newDIRouter(t)
	mock.ExpectQuery(regexp.QuoteMeta("FROM partner_inbound_batches b")).
		WillReturnError(errors.New("pq: canceling statement due to statement timeout"))
	rec := diDo(h, http.MethodGet, "/api/mailing/data-ingest/loads?date=2026-09-20", "", sessionHdr())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Fields map[string]string `json:"fields"`
		Note   string            `json:"note"`
		Loads  []any             `json:"loads"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Loads) != 0 || got.Note == "" || got.Fields["loads"] != fieldNotMeasured || got.Fields["landed"] != fieldNotMeasured {
		t.Fatalf("bad degrade: %s", rec.Body.String())
	}
}
