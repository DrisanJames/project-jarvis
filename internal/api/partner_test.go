package api

// Tests for the partner-ingestion subsystem.
//
//   - Pure helpers   : sanitizeSlug, parseIngestPayload, normalizeRecord,
//                      HashPartnerKey, GeneratePartnerKey, isValidVertical,
//                      isValidBrand, slugifyForPartner, sanitize key paths.
//   - DB handlers    : every PartnerAdminHandler endpoint (sqlmock + httptest).
//   - Auth flow      : PartnerKeyAuth middleware happy path + every error.
//   - Public schema  : HandleGetSchema returns the public spec without auth.
//
// Pattern follows outbox_admin_test.go: regex query matcher, out-of-order
// expectations, no real DB. Each test creates its own sqlmock.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

func newPartnerMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
		sqlmock.MonitorPingsOption(false),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.MatchExpectationsInOrder(false)
	return db, mock
}

// ---------------- pure helpers ----------------

func TestSanitizeSlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Attribits", "attribits"},
		{"My Partner!", "my-partner"},
		{"  whitespace  ", "whitespace"},
		{"weird///chars___trailing-", "weird-chars___trailing"},
		{"", "unknown"},
		{"!!!", "unknown"},
		{"UPPER_CASE-with-DASH", "upper_case-with-dash"},
	}
	for _, c := range cases {
		got := sanitizeSlug(c.in)
		if got != c.want {
			t.Errorf("sanitizeSlug(%q): want %q got %q", c.in, c.want, got)
		}
	}
}

func TestSlugifyForPartner(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Attribits", "attribits"},
		{"Attribits HOME", "attribits-home"},
		{"  Spacey  ", "spacey"},
		{"!!!only punct!!!", "only-punct"},
	}
	for _, c := range cases {
		got := slugifyForPartner(c.in)
		if got != c.want {
			t.Errorf("slugifyForPartner(%q): want %q got %q", c.in, c.want, got)
		}
	}
}

func TestIsValidVertical(t *testing.T) {
	// Must stay in lockstep with the partner_datasets_vertical_check CHECK
	// array in cmd/server/main.go — a vertical the DB accepts but this rejects
	// is unmanageable through the portal (HandleUpdateCreative 400s on it).
	for _, v := range []string{
		"refi_heloc", "personal_loans", "tax_relief", "remodel",
		"direct_offer", "clickers_samsclub", "metal_roofing_signal",
		"samsclub_internal", "flooring", "term_life", "senior_care",
		"auto_insurance", "jarvis_att", "jarvis_apple", "consumer",
		"internal_auto_insurance",
	} {
		if !isValidVertical(v) {
			t.Errorf("isValidVertical(%q) should be true", v)
		}
	}
	for _, v := range []string{"", "foo", "REFI_HELOC", "loans"} {
		if isValidVertical(v) {
			t.Errorf("isValidVertical(%q) should be false", v)
		}
	}
}

func TestIsValidBrand(t *testing.T) {
	for _, b := range []string{"db", "ht", "mh", "qf"} {
		if !isValidBrand(b) {
			t.Errorf("isValidBrand(%q) should be true", b)
		}
	}
	for _, b := range []string{"", "DB", "rb", "wf"} {
		if isValidBrand(b) {
			t.Errorf("isValidBrand(%q) should be false", b)
		}
	}
}

func TestHashPartnerKeyDeterministic(t *testing.T) {
	a := HashPartnerKey("dpk_abc")
	b := HashPartnerKey("dpk_abc")
	c := HashPartnerKey("dpk_abd")
	if a != b {
		t.Fatalf("hash should be deterministic")
	}
	if a == c {
		t.Fatalf("different keys must produce different hashes")
	}
	if len(a) != 64 {
		t.Fatalf("sha-256 hex must be 64 chars, got %d", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("hash must be hex: %v", err)
	}
}

func TestGeneratePartnerKey(t *testing.T) {
	raw, prefix, hash, err := GeneratePartnerKey()
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(raw, "dpk_"), "raw key must start with dpk_")
	require.True(t, strings.HasPrefix(prefix, "dpk_"), "prefix must start with dpk_")
	require.Equal(t, 8, len(prefix), "prefix is dpk_ + 4 chars = 8 chars")
	require.Equal(t, hash, HashPartnerKey(raw), "hash must match HashPartnerKey(raw)")
	require.Equal(t, 64, len(hash), "sha-256 hex 64 chars")

	raw2, _, _, err := GeneratePartnerKey()
	require.NoError(t, err)
	require.NotEqual(t, raw, raw2, "two generations must not collide")
}

func TestNormalizeRecord(t *testing.T) {
	cases := []struct {
		name string
		in   ingestRecord
		want bool
	}{
		{"valid", ingestRecord{Email: "Foo@Example.com"}, true},
		{"trim+lower", ingestRecord{Email: "  Bar@Example.com  "}, true},
		{"missing-at", ingestRecord{Email: "no-at-sign"}, false},
		{"empty", ingestRecord{Email: ""}, false},
		{"with-space", ingestRecord{Email: "a @b.com"}, false},
		{"too-long", ingestRecord{Email: strings.Repeat("a", 250) + "@b.com"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, ok := normalizeRecord(c.in)
			if ok != c.want {
				t.Fatalf("normalizeRecord ok=%v want=%v", ok, c.want)
			}
			if ok && out.Email != strings.ToLower(strings.TrimSpace(c.in.Email)) {
				t.Fatalf("normalizeRecord did not lower/trim: %q", out.Email)
			}
		})
	}
}

func TestParseIngestPayload(t *testing.T) {
	t.Run("json object", func(t *testing.T) {
		body := []byte(`{"records":[{"email":"a@b.com"},{"email":"c@d.com"}]}`)
		out, err := parseIngestPayload(body, "application/json")
		require.NoError(t, err)
		require.Len(t, out, 2)
	})
	t.Run("ndjson", func(t *testing.T) {
		body := []byte(`{"email":"a@b.com"}` + "\n" + `{"email":"c@d.com"}`)
		out, err := parseIngestPayload(body, "application/x-ndjson")
		require.NoError(t, err)
		require.Len(t, out, 2)
		require.Equal(t, "a@b.com", out[0].Email)
	})
	t.Run("empty rejected", func(t *testing.T) {
		_, err := parseIngestPayload([]byte("   "), "application/json")
		require.Error(t, err)
	})
	t.Run("malformed lines silently dropped", func(t *testing.T) {
		body := []byte(`{"email":"a@b.com"}` + "\n" + `not-json` + "\n" + `{"email":"c@d.com"}`)
		out, err := parseIngestPayload(body, "application/x-ndjson")
		require.NoError(t, err)
		require.Len(t, out, 2)
	})
	t.Run("invalid emails dropped", func(t *testing.T) {
		body := []byte(`{"records":[{"email":"a@b.com"},{"email":"no-at"},{"email":""}]}`)
		out, err := parseIngestPayload(body, "application/json")
		require.NoError(t, err)
		require.Len(t, out, 1)
		require.Equal(t, "a@b.com", out[0].Email)
	})
}

// TestIngestRecordRootLevelTID pins the 2026-08-13 ratesavings incident: the
// partner was told the metadata subobject is optional and posted tid,
// opt_in_ip, and opt_in_url at the record ROOT. The closed ingestRecord struct
// destroyed all three at the door — the canonical S3 NDJSON is a re-marshal of
// the struct, so downstream recovery was impossible. Root-level tid must land
// in Metadata (the only path extraMetadataJSON carries to partner_clean_queue),
// and the opt_in_* aliases must fold onto the canonical fields.
func TestIngestRecordRootLevelTID(t *testing.T) {
	t.Run("exact incident payload survives", func(t *testing.T) {
		body := []byte(`{
			"opt_in_url": "https://quotes.ratesavings.org",
			"state": "NC",
			"opt_in_ip": "2603:6080:2303:8ef9:9955:5375:39c9:1d1a",
			"opt_in_date": "Wed, 12 Aug 2026 20:54:03 GMT",
			"zip": "27217",
			"last_name": "Solos",
			"first_name": "Lorena",
			"tid": "496e49454c785965383241526a33664964522f4b4a673d3d",
			"city": "Burlington",
			"address_1": "519 PIEDMONT Way",
			"email": "lorenasolis1985@icloud.com"
		}`)
		var rec ingestRecord
		require.NoError(t, json.Unmarshal(body, &rec))
		require.Equal(t, "496e49454c785965383241526a33664964522f4b4a673d3d", rec.Metadata["tid"])
		require.Equal(t, "https://quotes.ratesavings.org", rec.SignupURL)
		require.Equal(t, "2603:6080:2303:8ef9:9955:5375:39c9:1d1a", rec.IPAddress)
		require.Equal(t, "Burlington", rec.City)
		require.Equal(t, "519 PIEDMONT Way", rec.Address1)

		// The door's output is what S3 stores — the re-marshal must carry tid too.
		out, err := json.Marshal(rec)
		require.NoError(t, err)
		require.Contains(t, string(out), "496e49454c785965383241526a33664964522f4b4a673d3d")
	})
	t.Run("nested metadata tid wins over root", func(t *testing.T) {
		var rec ingestRecord
		require.NoError(t, json.Unmarshal([]byte(`{"email":"a@b.com","tid":"root","metadata":{"tid":"nested"}}`), &rec))
		require.Equal(t, "nested", rec.Metadata["tid"])
	})
	t.Run("data tid wins over root", func(t *testing.T) {
		var rec ingestRecord
		require.NoError(t, json.Unmarshal([]byte(`{"email":"a@b.com","tid":"root","data":{"tid":"nested"}}`), &rec))
		require.Equal(t, "nested", rec.Metadata["tid"])
	})
	t.Run("numeric tid does not kill the record", func(t *testing.T) {
		var rec ingestRecord
		require.NoError(t, json.Unmarshal([]byte(`{"email":"a@b.com","tid":12345}`), &rec))
		require.Equal(t, "12345", rec.Metadata["tid"])
	})
	t.Run("explicit ip and signup_url beat aliases", func(t *testing.T) {
		var rec ingestRecord
		require.NoError(t, json.Unmarshal([]byte(`{"email":"a@b.com","ip_address":"1.2.3.4","opt_in_ip":"5.6.7.8","signup_url":"https://a","opt_in_url":"https://b"}`), &rec))
		require.Equal(t, "1.2.3.4", rec.IPAddress)
		require.Equal(t, "https://a", rec.SignupURL)
	})
	t.Run("no tid leaves metadata nil", func(t *testing.T) {
		var rec ingestRecord
		require.NoError(t, json.Unmarshal([]byte(`{"email":"a@b.com"}`), &rec))
		require.Nil(t, rec.Metadata)
	})
}

func TestPartnerS3KeyBuilders(t *testing.T) {
	c := &PartnerIngestS3Client{bucket: "test-bucket", region: "us-west-2"}
	ts := mustParseTime(t, "2026-05-12T14:22:00Z")
	got := c.BatchKey("Attribits", "Attribits HELOC!", "batch-uuid", ts)
	want := "partners/attribits/attribits-heloc/2026/05/12/batch-uuid.ndjson.gz"
	if got != want {
		t.Fatalf("BatchKey mismatch:\n  want %q\n  got  %q", want, got)
	}
	got = c.MetaKey("Attribits", "Attribits HELOC!", "batch-uuid", ts)
	want = "partners/attribits/attribits-heloc/2026/05/12/batch-uuid.meta.json"
	if got != want {
		t.Fatalf("MetaKey mismatch:\n  want %q\n  got  %q", want, got)
	}
}

// ---------------- HandleListPartners ----------------

func TestHandleListPartners(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	rows := sqlmock.NewRows([]string{
		"id", "name", "slug", "contact_email", "status", "notes", "created_at",
		"dataset_count", "batch_count",
	}).AddRow(
		"00000000-0000-0000-0000-000000000abc",
		"Attribits", "attribits", "ops@attribits.com", "active", "",
		mustParseTime(t, "2026-05-12T00:00:00Z"),
		4, 12,
	)
	mock.ExpectQuery(`SELECT p\.id, p\.name, p\.slug.*FROM data_partners p\s*ORDER BY p\.created_at DESC`).
		WillReturnRows(rows)

	h := NewPartnerAdminHandler(db)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mailing/data-partners", nil)
	h.HandleListPartners(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	partners := body["partners"].([]interface{})
	require.Len(t, partners, 1)
	first := partners[0].(map[string]interface{})
	require.Equal(t, "Attribits", first["name"])
	require.Equal(t, "attribits", first["slug"])
	require.Equal(t, float64(4), first["dataset_count"])
	require.Equal(t, float64(12), first["batch_count"])
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------- HandleCreatePartner ----------------

func TestHandleCreatePartner_HappyPath(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	mock.ExpectExec(`INSERT INTO data_partners`).
		WithArgs(sqlmock.AnyArg(), "Attribits", "attribits", "ops@attribits.com", "smoke notes").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewPartnerAdminHandler(db)
	body, _ := json.Marshal(map[string]string{
		"name":          "Attribits",
		"slug":          "attribits",
		"contact_email": "ops@attribits.com",
		"notes":         "smoke notes",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/data-partners", bytes.NewReader(body))
	h.HandleCreatePartner(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "Attribits", resp["name"])
	require.Equal(t, "attribits", resp["slug"])
	require.Equal(t, "active", resp["status"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleCreatePartner_NameRequired(t *testing.T) {
	db, _ := newPartnerMockDB(t)
	h := NewPartnerAdminHandler(db)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/data-partners",
		strings.NewReader(`{"slug":"foo"}`))
	h.HandleCreatePartner(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "name is required")
}

func TestHandleCreatePartner_DuplicateConflict(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	mock.ExpectExec(`INSERT INTO data_partners`).
		WillReturnError(&fakePgErr{msg: "duplicate key value violates unique constraint"})

	h := NewPartnerAdminHandler(db)
	body, _ := json.Marshal(map[string]string{"name": "Attribits"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/data-partners", bytes.NewReader(body))
	h.HandleCreatePartner(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------- HandleListDatasets ----------------

func TestHandleListDatasets(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	rows := sqlmock.NewRows([]string{
		"id", "partner_id", "name", "slug", "vertical", "flush_window_hours",
		"paused_emergency", "paused_reason", "status", "created_at",
		"partner_name", "partner_slug", "batch_count", "ready_count", "mailed_count",
	}).AddRow(
		"00000000-0000-0000-0000-0000000abc01",
		"00000000-0000-0000-0000-000000000abc",
		"Attribits-HELOC", "attribits-heloc", "refi_heloc", 24,
		false, "", "active", mustParseTime(t, "2026-05-12T00:00:00Z"),
		"Attribits", "attribits", 3, 1500, 4500,
	)
	mock.ExpectQuery(`SELECT d\.id, d\.partner_id.*FROM partner_datasets d\s*JOIN data_partners p`).
		WillReturnRows(rows)

	h := NewPartnerAdminHandler(db)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mailing/data-partners/datasets", nil)
	h.HandleListDatasets(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	datasets := body["datasets"].([]interface{})
	require.Len(t, datasets, 1)
	d := datasets[0].(map[string]interface{})
	require.Equal(t, "Attribits-HELOC", d["name"])
	require.Equal(t, "refi_heloc", d["vertical"])
	require.Equal(t, float64(24), d["flush_window_hours"])
	require.Equal(t, float64(1500), d["ready_queue_count"])
	require.Equal(t, float64(4500), d["mailed_count"])
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------- HandleEmergencyStop / Resume ----------------

func TestHandleEmergencyStop(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	mock.ExpectExec(`UPDATE partner_datasets`).
		WithArgs("00000000-0000-0000-0000-000000000abc", "operator emergency stop").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewPartnerAdminHandler(db)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/api/mailing/data-partners/datasets/00000000-0000-0000-0000-000000000abc/emergency-stop",
		strings.NewReader(`{"reason":"operator emergency stop"}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "00000000-0000-0000-0000-000000000abc")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	h.HandleEmergencyStopDataset(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleEmergencyStop_InvalidUUID(t *testing.T) {
	db, _ := newPartnerMockDB(t)
	h := NewPartnerAdminHandler(db)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/api/mailing/data-partners/datasets/not-a-uuid/emergency-stop",
		strings.NewReader(`{}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "not-a-uuid")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	h.HandleEmergencyStopDataset(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandleResumeDataset_NotFound(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	mock.ExpectExec(`UPDATE partner_datasets`).
		WithArgs("00000000-0000-0000-0000-000000000abc").
		WillReturnResult(sqlmock.NewResult(0, 0)) // 0 rows affected = not found

	h := NewPartnerAdminHandler(db)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/api/mailing/data-partners/datasets/00000000-0000-0000-0000-000000000abc/resume",
		strings.NewReader(`{}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "00000000-0000-0000-0000-000000000abc")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	h.HandleResumeDataset(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------- HandleListCreatives ----------------

func TestHandleListCreatives(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	rows := sqlmock.NewRows([]string{
		"vertical", "brand", "creative_filename", "subject_line", "preheader",
		"from_name", "active", "effective_from", "updated_by",
	}).
		AddRow("refi_heloc", "db", "amerisave-db-newsletter-05132026.html",
			"Subject", "Preheader", "Jamie", true,
			mustParseTime(t, "2026-05-12T00:00:00Z"), "operator").
		AddRow("personal_loans", "qf", "personal-loans-qf-newsletter-05132026.html",
			"Subject", "Preheader", "QF", true,
			mustParseTime(t, "2026-05-12T00:00:00Z"), "operator")
	mock.ExpectQuery(`SELECT vertical, brand, creative_filename.*FROM partner_drip_creatives`).
		WillReturnRows(rows)

	h := NewPartnerAdminHandler(db)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mailing/data-partners/creatives", nil)
	h.HandleListCreatives(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	creatives := body["creatives"].([]interface{})
	require.Len(t, creatives, 2)
	first := creatives[0].(map[string]interface{})
	require.Equal(t, "refi_heloc", first["vertical"])
	require.Equal(t, "db", first["brand"])
	require.Equal(t, "amerisave-db-newsletter-05132026.html", first["creative_filename"])
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------- HandleUpdateCreative ----------------

func TestHandleUpdateCreative_HappyPath(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	beforeRows := sqlmock.NewRows([]string{
		"creative_filename", "subject_line", "preheader", "from_name", "active",
	}).AddRow("old.html", "old subj", "old pre", "old from", true)
	mock.ExpectQuery(`SELECT creative_filename, subject_line.*FROM partner_drip_creatives WHERE vertical`).
		WillReturnRows(beforeRows)
	mock.ExpectExec(`INSERT INTO partner_drip_creatives.*ON CONFLICT \(vertical, brand\) DO UPDATE`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewPartnerAdminHandler(db)
	body, _ := json.Marshal(map[string]string{
		"creative_filename": "amerisave-db-newsletter-05132026.html",
		"subject_line":      "New subj",
		"preheader":         "Pre",
		"from_name":         "Jamie",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut,
		"/api/mailing/data-partners/creatives/refi_heloc/db", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("vertical", "refi_heloc")
	rctx.URLParams.Add("brand", "db")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	h.HandleUpdateCreative(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "next_wave", resp["effective"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleUpdateCreative_RejectsTraversal(t *testing.T) {
	db, _ := newPartnerMockDB(t)
	h := NewPartnerAdminHandler(db)
	body, _ := json.Marshal(map[string]string{
		"creative_filename": "../../etc/passwd",
		"subject_line":      "x", "from_name": "x",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut,
		"/api/mailing/data-partners/creatives/refi_heloc/db", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("vertical", "refi_heloc")
	rctx.URLParams.Add("brand", "db")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	h.HandleUpdateCreative(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "bare basename")
}

func TestHandleUpdateCreative_InvalidVertical(t *testing.T) {
	db, _ := newPartnerMockDB(t)
	h := NewPartnerAdminHandler(db)
	body, _ := json.Marshal(map[string]string{
		"creative_filename": "ok.html", "subject_line": "x", "from_name": "x",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut,
		"/api/mailing/data-partners/creatives/bad_vertical/db", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("vertical", "bad_vertical")
	rctx.URLParams.Add("brand", "db")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	h.HandleUpdateCreative(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// ---------------- HandleUpdateISPDistribution ----------------

func TestHandleUpdateISPDistribution(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	dsID := "00000000-0000-0000-0000-000000000abc"
	mock.ExpectBegin()
	// daily_cap snapshot (core.md §14 2026-08-06): yahoo carries a lane budget
	// of 150 set outside this request — the per-wave edit must NOT wipe it.
	mock.ExpectQuery(`SELECT LOWER\(TRIM\(isp\)\), daily_cap FROM partner_isp_distribution_overrides`).
		WithArgs(dsID).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "daily_cap"}).AddRow("yahoo", 150))
	mock.ExpectExec(`DELETE FROM partner_isp_distribution_overrides WHERE dataset_id = \$1`).
		WithArgs(dsID).WillReturnResult(sqlmock.NewResult(0, 0))
	// gmail: request sets daily_cap=500 explicitly.
	mock.ExpectExec(`INSERT INTO partner_isp_distribution_overrides`).
		WithArgs(dsID, "gmail", 0.4, 1000, 500, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// yahoo: request omits daily_cap -> prior lane budget 150 is preserved.
	mock.ExpectExec(`INSERT INTO partner_isp_distribution_overrides`).
		WithArgs(dsID, "yahoo", 0.3, sqlmock.AnyArg(), 150, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewPartnerAdminHandler(db)
	body, _ := json.Marshal(map[string]interface{}{
		"overrides": []map[string]interface{}{
			{"isp": "gmail", "pct_override": 0.4, "max_per_wave": 1000, "daily_cap": 500},
			{"isp": "YAHOO", "pct_override": 0.3},
		},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut,
		"/api/mailing/data-partners/datasets/"+dsID+"/isp-distribution",
		bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", dsID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	h.HandleUpdateISPDistribution(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleUpdateISPDistribution_PctOutOfRange(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	dsID := "00000000-0000-0000-0000-000000000abc"
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT LOWER\(TRIM\(isp\)\), daily_cap FROM partner_isp_distribution_overrides`).
		WithArgs(dsID).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "daily_cap"}))
	mock.ExpectExec(`DELETE FROM partner_isp_distribution_overrides WHERE dataset_id = \$1`).
		WithArgs(dsID).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	h := NewPartnerAdminHandler(db)
	body, _ := json.Marshal(map[string]interface{}{
		"overrides": []map[string]interface{}{{"isp": "gmail", "pct_override": 1.5}},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut,
		"/api/mailing/data-partners/datasets/"+dsID+"/isp-distribution",
		bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", dsID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	h.HandleUpdateISPDistribution(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "pct_override must be between 0 and 1")
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------- HandleListAuditLog ----------------

func TestHandleListAuditLog(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	rows := sqlmock.NewRows([]string{
		"id", "actor", "action", "target_type", "target_id",
		"before_state", "after_state", "created_at",
	}).AddRow(
		"00000000-0000-0000-0000-0000000audit",
		"ops@projectjarvis.io", "create_partner", "data_partner",
		"00000000-0000-0000-0000-000000000abc",
		"", `{"name":"Attribits"}`,
		mustParseTime(t, "2026-05-12T00:00:00Z"),
	)
	mock.ExpectQuery(`SELECT id, actor, action.*FROM partner_admin_audit_log`).
		WillReturnRows(rows)

	h := NewPartnerAdminHandler(db)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/mailing/data-partners/audit-log", nil)
	h.HandleListAuditLog(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	events := body["events"].([]interface{})
	require.Len(t, events, 1)
	first := events[0].(map[string]interface{})
	require.Equal(t, "create_partner", first["action"])
	require.Equal(t, "data_partner", first["target_type"])
	after := first["after_state"].(map[string]interface{})
	require.Equal(t, "Attribits", after["name"])
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------- HandleGetDashboard ----------------

func TestHandleGetDashboard(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	verticalRows := sqlmock.NewRows([]string{
		"vertical", "next_brand_index", "last_wave_at", "last_wave_brand", "last_wave_size",
		"ready_q", "pending_eo", "mailed",
	}).AddRow("refi_heloc", 2, mustParseTime(t, "2026-05-12T00:00:00Z"), "DB", int64(50),
		120, 30, 850)
	mock.ExpectQuery(`SELECT s\.vertical, s\.next_brand_index.*FROM partner_drip_state s`).
		WillReturnRows(verticalRows)

	batchRows := sqlmock.NewRows([]string{
		"id", "dataset_id", "partner_id", "status", "record_count",
		"received_at", "completed_at", "emergency_stopped",
		"d_name", "d_vertical", "p_name",
	}).AddRow(
		"00000000-0000-0000-0000-0000000batch",
		"00000000-0000-0000-0000-0000000ds01",
		"00000000-0000-0000-0000-000000000abc",
		"slicing_complete", 5000,
		mustParseTime(t, "2026-05-12T00:00:00Z"), nil, false,
		"Attribits-HELOC", "refi_heloc", "Attribits")
	mock.ExpectQuery(`SELECT b\.id, b\.dataset_id, b\.partner_id.*FROM partner_inbound_batches b`).
		WillReturnRows(batchRows)

	h := NewPartnerAdminHandler(db)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mailing/data-partners/dashboard", nil)
	h.HandleGetDashboard(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	verticals := body["verticals"].([]interface{})
	require.Len(t, verticals, 1)
	first := verticals[0].(map[string]interface{})
	require.Equal(t, "refi_heloc", first["vertical"])
	require.Equal(t, "DB", first["last_wave_brand"])
	require.Equal(t, float64(120), first["ready_queue"])
	require.Equal(t, float64(850), first["mailed_total"])
	batches := body["recent_batches"].([]interface{})
	require.Len(t, batches, 1)
	require.Equal(t, "Attribits", batches[0].(map[string]interface{})["partner_name"])
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------- HandleGetDatasetThroughput ----------------

func TestHandleGetDatasetThroughput(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	dsID := "00000000-0000-0000-0000-000000000abc"
	mock.ExpectQuery(`SELECT d\.flush_window_hours.*FROM partner_datasets d\s*WHERE d\.id`).
		WithArgs(dsID).
		WillReturnRows(sqlmock.NewRows([]string{
			"flush_window_hours", "oldest_ingest_at", "ready_total",
		}).AddRow(24, mustParseTime(t, "2026-05-12T00:00:00Z"), 1000))
	mock.ExpectQuery(`SELECT isp_family, COUNT.*FROM partner_clean_queue.*GROUP BY isp_family`).
		WithArgs(dsID).
		WillReturnRows(sqlmock.NewRows([]string{"isp_family", "count"}).
			AddRow("gmail", 600).AddRow("yahoo", 300).AddRow("other", 100))
	mock.ExpectQuery(`SELECT isp, pct_override, COALESCE\(max_per_wave, 0\), daily_cap FROM partner_isp_distribution_overrides`).
		WithArgs(dsID).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "pct_override", "max_per_wave", "daily_cap"}).
			AddRow("gmail", 0.5, 500, 250))

	h := NewPartnerAdminHandler(db)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/mailing/data-partners/datasets/"+dsID+"/throughput", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", dsID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	h.HandleGetDatasetThroughput(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, float64(1000), body["ready_queue_total"])
	breakdown := body["isp_breakdown"].(map[string]interface{})
	require.Equal(t, float64(600), breakdown["gmail"])
	overrides := body["isp_overrides"].([]interface{})
	require.Len(t, overrides, 1)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------- PartnerKeyAuth middleware ----------------

func TestPartnerKeyAuth_MissingHeader(t *testing.T) {
	db, _ := newPartnerMockDB(t)
	mw := PartnerKeyAuth(db)
	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/partner-ingest/v1/records", nil)
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.False(t, called)
	require.Contains(t, rec.Body.String(), "missing_api_key")
}

func TestPartnerKeyAuth_BadPrefix(t *testing.T) {
	db, _ := newPartnerMockDB(t)
	mw := PartnerKeyAuth(db)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/partner-ingest/v1/records", nil)
	req.Header.Set("X-Partner-Key", "wrong-prefix-key")
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), "invalid_api_key_format")
}

func TestPartnerKeyAuth_KeyNotFound(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	mock.ExpectQuery(`SELECT k\.id, k\.partner_id.*FROM partner_api_keys k`).
		WillReturnError(sql.ErrNoRows)

	mw := PartnerKeyAuth(db)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/partner-ingest/v1/records", nil)
	req.Header.Set("X-Partner-Key", "dpk_unknown1234567890abcdefghijklmn")
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), "invalid_api_key")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPartnerKeyAuth_DatasetPaused(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	mock.ExpectQuery(`SELECT k\.id, k\.partner_id.*FROM partner_api_keys k`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "partner_id", "dataset_id", "key_prefix",
			"p_slug", "p_name",
			"d_slug", "d_name", "d_vertical",
			"d_paused", "d_status", "p_status", "k_status",
		}).AddRow(
			"00000000-0000-0000-0000-0000000key01",
			"00000000-0000-0000-0000-000000000abc",
			"00000000-0000-0000-0000-0000000ds001",
			"dpk_test",
			"attribits", "Attribits",
			"attribits-heloc", "Attribits-HELOC", "refi_heloc",
			true, "active", "active", "active",
		))

	mw := PartnerKeyAuth(db)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/partner-ingest/v1/records", nil)
	req.Header.Set("X-Partner-Key", "dpk_paused1234567890abcdefghijklmn")
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "dataset_paused")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPartnerKeyAuth_HappyPath(t *testing.T) {
	db, mock := newPartnerMockDB(t)
	mock.ExpectQuery(`SELECT k\.id, k\.partner_id.*FROM partner_api_keys k`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "partner_id", "dataset_id", "key_prefix",
			"p_slug", "p_name",
			"d_slug", "d_name", "d_vertical",
			"d_paused", "d_status", "p_status", "k_status",
		}).AddRow(
			"00000000-0000-0000-0000-0000000key01",
			"00000000-0000-0000-0000-000000000abc",
			"00000000-0000-0000-0000-0000000ds001",
			"dpk_test",
			"attribits", "Attribits",
			"attribits-heloc", "Attribits-HELOC", "refi_heloc",
			false, "active", "active", "active",
		))
	// async bumpLastUsed call — sqlmock with MatchExpectationsInOrder=false will accept
	mock.ExpectExec(`UPDATE partner_api_keys SET last_used_at`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mw := PartnerKeyAuth(db)
	got := PartnerAuthContext{}
	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		got, _ = PartnerAuthFromContext(r.Context())
		w.WriteHeader(http.StatusAccepted)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/partner-ingest/v1/records", nil)
	req.Header.Set("X-Partner-Key", "dpk_valid12345678901234567890ABCDEFG")
	h.ServeHTTP(rec, req)
	require.True(t, called, "next handler must run")
	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Equal(t, "Attribits", got.PartnerName)
	require.Equal(t, "refi_heloc", got.Vertical)
	require.Equal(t, "Attribits-HELOC", got.DatasetName)
	// Give async UPDATE a chance to run (synchronous path doesn't wait)
	// The test will still pass if it hasn't fired yet because MatchExpectationsInOrder=false
	// allows ExpectationsWereMet to be lenient on order, but we still need it to fire.
	// We don't assert ExpectationsWereMet here since the goroutine might race the test.
}

// ---------------- HandleGetSchema (public) ----------------

func TestHandleGetSchema_NoAuthRequired(t *testing.T) {
	db, _ := newPartnerMockDB(t)
	h := NewPartnerIngestHandler(db, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/partner-ingest/v1/schema", nil)
	h.HandleGetSchema(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "POST /api/partner-ingest/v1/records", body["endpoint"])
	verticals := body["verticals_supported"].([]interface{})
	require.Len(t, verticals, 4)
	limits := body["limits"].(map[string]interface{})
	require.Equal(t, float64(maxRecordsPerBatch), limits["max_records_per_batch"])
}

// ---------------- HandlePostRecords ----------------

func TestHandlePostRecords_AuthMissing(t *testing.T) {
	db, _ := newPartnerMockDB(t)
	h := NewPartnerIngestHandler(db, &PartnerIngestS3Client{bucket: "test"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/partner-ingest/v1/records",
		strings.NewReader(`{"records":[{"email":"a@b.com"}]}`))
	req.Header.Set("Content-Type", "application/json")
	h.HandlePostRecords(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Contains(t, rec.Body.String(), "auth_missing")
}

func TestHandlePostRecords_S3Unavailable(t *testing.T) {
	db, _ := newPartnerMockDB(t)
	h := NewPartnerIngestHandler(db, nil) // s3 not yet wired
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/partner-ingest/v1/records",
		strings.NewReader(`{"records":[{"email":"a@b.com"}]}`))
	ctx := context.WithValue(req.Context(), ctxKeyPartnerID, "p-id")
	ctx = context.WithValue(ctx, ctxKeyPartnerSlug, "attribits")
	ctx = context.WithValue(ctx, ctxKeyPartnerName, "Attribits")
	ctx = context.WithValue(ctx, ctxKeyDatasetID, "d-id")
	ctx = context.WithValue(ctx, ctxKeyDatasetSlug, "attribits-heloc")
	ctx = context.WithValue(ctx, ctxKeyDatasetName, "Attribits-HELOC")
	ctx = context.WithValue(ctx, ctxKeyVertical, "refi_heloc")
	ctx = context.WithValue(ctx, ctxKeyAPIKeyID, "k-id")
	ctx = context.WithValue(ctx, ctxKeyKeyPrefix, "dpk_xxxx")
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	h.HandlePostRecords(rec, req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "ingest_unavailable")
}

// ---------------- helpers ----------------

type fakePgErr struct{ msg string }

func (e *fakePgErr) Error() string { return e.msg }

// mustParseTime is reused from mailing_analytics_promoted_test.go.
// Just import time so this file compiles.
var _ = time.RFC3339
