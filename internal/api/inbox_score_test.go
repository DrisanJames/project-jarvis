package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/worker"
)

const inboxScoreTestOrg = "00000000-0000-0000-0000-000000000001"

func newInboxScoreTestServer(t *testing.T) (sqlmock.Sqlmock, *chi.Mux) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	svc := &InboxScoreService{
		db:   db,
		pool: worker.NewRenderPool(db, "https://t.projectjarvis.io", "test-secret", inboxScoreTestOrg),
		now:  func() time.Time { return time.Date(2026, 10, 21, 18, 0, 0, 0, time.UTC) },
	}
	root := chi.NewRouter()
	svc.RegisterAdminRoutes(root)
	root.Route("/api/mailing", func(r chi.Router) { svc.RegisterRoutes(r) })
	return mock, root
}

func inboxScoreReq(method, path string, body interface{}, admin string) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("X-Organization-ID", inboxScoreTestOrg)
	req.Header.Set("Content-Type", "application/json")
	if admin != "" {
		req.Header.Set("X-Admin-Key", admin)
	}
	return req
}

// TestInboxScore_AdminClosedByDefault: no ADMIN_API_KEY → every admin route
// refuses; a wrong key refuses. The scanner can never write without the key.
func TestInboxScore_AdminClosedByDefault(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "")
	_, mux := newInboxScoreTestServer(t)
	for _, p := range []string{"/scores", "/profiles", "/seed-placements", "/render"} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, inboxScoreReq("POST", inboxScoreAdminPrefix+p, map[string]string{}, "anything"))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s with unset ADMIN_API_KEY: code=%d want 401", p, rr.Code)
		}
	}
	t.Setenv("ADMIN_API_KEY", "right")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, inboxScoreReq("POST", inboxScoreAdminPrefix+"/scores", map[string]string{}, "wrong"))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("wrong key: code=%d want 401", rr.Code)
	}
}

// TestInboxScore_PostScores_ValidatesAndUpserts: unknown ISP and out-of-range
// probability are rejected before any write; a valid row is upserted on the
// (org, campaign, isp, source) key.
func TestInboxScore_PostScores_ValidatesAndUpserts(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "k")
	mock, mux := newInboxScoreTestServer(t)

	bad := map[string]interface{}{"scores": []map[string]interface{}{{"campaign_id": "dce16d95-df2c-456d-a129-f3e80f938f0d", "isp": "hotmail", "p_inbox": 0.5}}}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, inboxScoreReq("POST", inboxScoreAdminPrefix+"/scores", bad, "k"))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "hotmail") {
		t.Fatalf("unknown isp: code=%d body=%s", rr.Code, rr.Body.String())
	}
	bad2 := map[string]interface{}{"scores": []map[string]interface{}{{"campaign_id": "dce16d95-df2c-456d-a129-f3e80f938f0d", "isp": "gmail", "p_inbox": 1.5}}}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, inboxScoreReq("POST", inboxScoreAdminPrefix+"/scores", bad2, "k"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("p_inbox 1.5: code=%d body=%s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("rejected rows must not touch the DB: %v", err)
	}

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO mailing_inbox_scores`).
		WithArgs(inboxScoreTestOrg, "dce16d95-df2c-456d-a129-f3e80f938f0d", "abc", "gmail", 3, 2.5, 0.82, `[{"rule":"HTML_MESSAGE","score":0.001}]`, `["Looks like a normal newsletter."]`, "render").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	good := map[string]interface{}{"scores": []map[string]interface{}{{
		"campaign_id": "dce16d95-df2c-456d-a129-f3e80f938f0d", "content_hash": "abc", "isp": "gmail", "profile_version": 3,
		"sa_score": 2.5, "p_inbox": 0.82,
		"rules":   json.RawMessage(`[{"rule":"HTML_MESSAGE","score":0.001}]`),
		"reasons": json.RawMessage(`["Looks like a normal newsletter."]`),
	}}}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, inboxScoreReq("POST", inboxScoreAdminPrefix+"/scores", good, "k"))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"upserted":1`) {
		t.Fatalf("valid upsert: code=%d body=%s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("upsert SQL: %v", err)
	}
}

// TestInboxScore_SeedPlacements_FolderVocabulary: the reader may only report
// the fixed folder set, and a valid row upserts on (org, seed_email, message_id).
func TestInboxScore_SeedPlacements_FolderVocabulary(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "k")
	mock, mux := newInboxScoreTestServer(t)
	bad := map[string]interface{}{"placements": []map[string]interface{}{{"seed_email": "a@gmail.com", "isp": "gmail", "folder": "junkish", "message_id": "<m1>"}}}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, inboxScoreReq("POST", inboxScoreAdminPrefix+"/seed-placements", bad, "k"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad folder: code=%d body=%s", rr.Code, rr.Body.String())
	}
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO mailing_seed_placements`).
		WithArgs(inboxScoreTestOrg, "dce16d95-df2c-456d-a129-f3e80f938f0d", "drisan@gmail.com", "gmail", "spam", "<m1@em.discountblog.com>", "x1", `{"dkim":"pass"}`, `{}`, nil, "", `[]`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	good := map[string]interface{}{"placements": []map[string]interface{}{{
		"campaign_id": "dce16d95-df2c-456d-a129-f3e80f938f0d", "seed_email": " Drisan@gmail.com ", "isp": "gmail", "folder": "spam",
		"message_id": "<m1@em.discountblog.com>", "x_message_id": "x1", "auth_results": json.RawMessage(`{"dkim":"pass"}`),
	}}}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, inboxScoreReq("POST", inboxScoreAdminPrefix+"/seed-placements", good, "k"))
	if rr.Code != http.StatusOK {
		t.Fatalf("valid placement: code=%d body=%s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("placement SQL: %v", err)
	}
}

// TestInboxScore_Summary_RectangularGrid: one campaign with one gmail score
// and one apple seed observation comes back with all five ISP cells in the
// fixed order, the score on gmail, the seed folder on apple, and the
// plain-words how_to_read block.
func TestInboxScore_Summary_RectangularGrid(t *testing.T) {
	mock, mux := newInboxScoreTestServer(t)
	camp := "dce16d95-df2c-456d-a129-f3e80f938f0d"
	sched := time.Date(2026, 10, 21, 10, 1, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM mailing_campaigns c`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "sending_domain", "status", "scheduled_at"}).
			AddRow(camp, "10212026 - DB - Liberty", "m.discountblog.com", "scheduled", sched))
	mock.ExpectQuery(`FROM mailing_inbox_scores sc`).
		WillReturnRows(sqlmock.NewRows([]string{"campaign_id", "isp", "sa_score", "p_inbox", "profile_version", "reasons", "scored_at", "auc"}).
			AddRow(camp, "gmail", 2.5, 0.82, 3, `["Looks fine."]`, sched, 0.91))
	mock.ExpectQuery(`FROM mailing_seed_placements`).
		WillReturnRows(sqlmock.NewRows([]string{"campaign_id", "isp", "folder", "seed_email", "observed_at", "defender_score", "defender_action"}).
			AddRow(camp, "apple", "inbox", "x@icloud.com", sched, 2.3, "no action"))
	// Latest profile per ISP rides along so the portal can say WHY a provider has no guess.
	mock.ExpectQuery(`SELECT DISTINCT ON \(isp\) isp, version, status, auc, finding`).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "version", "status", "auc", "finding"}).
			AddRow("gmail", 3, "active", 0.91, "Between 2026-08-22 and 2026-10-20 this provider rejected 40 campaigns for content and took 174 with no content rejection. The filter learned from those.").
			AddRow("microsoft", 2, "candidate", nil, "The server logs show no content rejections at this provider between 2026-08-22 and 2026-10-20 (3,926 campaigns with 2,000+ deliveries, none rejected for content). Content is not what stops mail here."))

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, inboxScoreReq("GET", "/api/mailing/inbox-score/summary?from=2026-10-21&to=2026-10-21", nil, ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("summary: code=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp inboxScoreSummaryResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Campaigns) != 1 || len(resp.Campaigns[0].Cells) != 5 {
		t.Fatalf("want 1 campaign × 5 cells, got %+v", resp.Campaigns)
	}
	cells := resp.Campaigns[0].Cells
	for i, isp := range InboxScoreISPs {
		if cells[i].ISP != isp {
			t.Errorf("cell %d isp=%s want %s", i, cells[i].ISP, isp)
		}
	}
	if cells[0].PInbox == nil || *cells[0].PInbox != 0.82 || cells[0].ProfileAUC == nil || *cells[0].ProfileAUC != 0.91 {
		t.Errorf("gmail cell lacks score/auc: %+v", cells[0])
	}
	if cells[3].SeedFolder != "inbox" || cells[3].ISP != "apple" {
		t.Errorf("apple cell lacks seed folder: %+v", cells[3])
	}
	if cells[1].PInbox != nil || cells[1].SeedFolder != "" {
		t.Errorf("yahoo cell must be empty: %+v", cells[1])
	}
	if len(resp.HowToRead) < 3 {
		t.Error("how_to_read block missing")
	}
	// The provider finding is a plain sentence from the trainer, not a number.
	if b, ok := resp.Profiles["microsoft"]; !ok || b.Status != "candidate" || b.AUC != nil || !strings.Contains(b.Finding, "no content rejections") {
		t.Errorf("microsoft profile brief wrong: %+v", resp.Profiles["microsoft"])
	}
	if b := resp.Profiles["gmail"]; b.Status != "active" || b.AUC == nil || *b.AUC != 0.91 {
		t.Errorf("gmail profile brief wrong: %+v", b)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestInboxScore_Render_LiveSendPath_Deterministic: /render loads the
// campaign the way the claim SELECT does, composes through the worker's own
// path (List-Unsubscribe, click wrap, pixel, text part, X-Job all present),
// reports what a pre-dispatch render cannot know, and returns identical bytes
// on a second call for the same (campaign, email).
func TestInboxScore_Render_LiveSendPath_Deterministic(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "k")
	t.Setenv("DISABLE_BRAND_IMAGE_HOST_SWAP", "1")
	mock, mux := newInboxScoreTestServer(t)
	camp := "dce16d95-df2c-456d-a129-f3e80f938f0d"
	html := `<html><body><p>Hi {{ first_name }}</p><a href="https://www.codefortwo.com/x?sub1=1">Offer</a></body></html>`

	expectLoad := func() {
		mock.ExpectQuery(`FROM mailing_campaigns camp`).
			WithArgs(sqlmock.AnyArg(), inboxScoreTestOrg).
			WillReturnRows(sqlmock.NewRows([]string{"subject", "html", "plain", "preview", "from_name", "from_email", "reply_to", "profile", "vendor", "name", "locked", "offer", "creative", "subject_line", "org"}).
				AddRow("$700 in savings", html, "", "", "Liberty Mutual Insurance", "hello@em.discountblog.com", "reply@em.discountblog.com", "93938919-2df7-40c4-ba68-4f3e301e5b05", "pmta", "10212026 - DB - Liberty", true,
					"00000000-0000-0000-0000-000000000000", "00000000-0000-0000-0000-000000000000", "00000000-0000-0000-0000-000000000000", inboxScoreTestOrg))
		mock.ExpectQuery(`FROM mailing_subscribers s`).WillReturnError(sql.ErrNoRows)
	}
	// First call also warms the pool's per-profile caches (two lookups).
	expectLoad()
	mock.ExpectQuery(`COALESCE\(tracking_domain,''\)`).
		WillReturnRows(sqlmock.NewRows([]string{"td", "sd"}).AddRow("t.m.discountblog.com", "m.discountblog.com"))
	mock.ExpectQuery(`COALESCE\(via_ses, FALSE\)`).
		WillReturnRows(sqlmock.NewRows([]string{"via", "cs", "tenant", "raw"}).AddRow(true, "discountblog", "discountblog", false))

	body := map[string]string{"campaign_id": camp, "email": "drisan@gmail.com"}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, inboxScoreReq("POST", inboxScoreAdminPrefix+"/render", body, "k"))
	if rr.Code != http.StatusOK {
		t.Fatalf("render: code=%d body=%s", rr.Code, rr.Body.String())
	}
	var r1 inboxScoreRenderResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &r1); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, want := range []string{"X-Campaign-ID: " + camp, "List-Unsubscribe:", "List-Unsubscribe-Post: List-Unsubscribe=One-Click", "X-Job: " + camp, "Feedback-ID:", "X-SES-CONFIGURATION-SET: discountblog", "Content-Type: text/plain", "Content-Type: text/html", "Subject: "} {
		if !strings.Contains(r1.RFC822, want) {
			t.Errorf("rfc822 lacks %q", want)
		}
	}
	// Bodies are quoted-printable: long URLs are soft-wrapped with "=\r\n".
	// Remove the soft breaks before substring checks (QP also escapes "=" in
	// the original as "=3D", so a raw money link with "?sub1=" cannot appear
	// verbatim; check its host+path instead).
	flat := strings.ReplaceAll(r1.RFC822, "=\r\n", "")
	if !strings.Contains(flat, "/track/click/") || !strings.Contains(flat, "/track/open/") {
		t.Error("rendered body is not click-wrapped / pixelled like a live send")
	}
	if strings.Contains(flat, `href=3D"https://www.codefortwo.com/x`) || strings.Contains(flat, `href="https://www.codefortwo.com/x`) {
		t.Error("raw money link survived; the live send wraps it")
	}
	if r1.RecipientISP != "gmail" || !r1.ContentLocked || r1.ContentHash == "" || len(r1.Deltas) < 2 {
		t.Errorf("metadata: isp=%s locked=%v hash=%q deltas=%d", r1.RecipientISP, r1.ContentLocked, r1.ContentHash, len(r1.Deltas))
	}
	joined := strings.Join(r1.Deltas, " | ")
	if !strings.Contains(joined, "not a subscriber on file") || !strings.Contains(joined, "X-Message-ID") {
		t.Errorf("deltas must name the known differences: %s", joined)
	}

	// Second call: same bytes (caches warm, so only the two load queries).
	expectLoad()
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, inboxScoreReq("POST", inboxScoreAdminPrefix+"/render", body, "k"))
	var r2 inboxScoreRenderResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &r2); err != nil {
		t.Fatalf("decode 2: %v", err)
	}
	if r1.RFC822SHA256 != r2.RFC822SHA256 || r1.RFC822 != r2.RFC822 {
		t.Fatal("render is not deterministic for the same (campaign, email)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestInboxScore_Render_UnknownCampaignIs404 and bad input is 400, nothing composed.
func TestInboxScore_Render_UnknownCampaignIs404(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "k")
	mock, mux := newInboxScoreTestServer(t)
	mock.ExpectQuery(`FROM mailing_campaigns camp`).WillReturnError(sql.ErrNoRows)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, inboxScoreReq("POST", inboxScoreAdminPrefix+"/render", map[string]string{"campaign_id": "dce16d95-df2c-456d-a129-f3e80f938f0d", "email": "a@b.c"}, "k"))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, inboxScoreReq("POST", inboxScoreAdminPrefix+"/render", map[string]string{"campaign_id": "nope", "email": "a@b.c"}, "k"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad uuid: code=%d", rr.Code)
	}
}
