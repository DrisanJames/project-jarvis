package api

// Newsletters mode fixtures (newsletter_requests.go + the warm-up handlers it
// re-cuts). These are NEGATIVE-PATH tests on purpose: this repo has shipped
// inert gates before, so each one proves a specific refusal actually fires.
//
// What they pin, in the order the audit ranked the defects:
//
//  1. Preview and sender resolve the SAME row. They issue the SAME statement
//     with the SAME parameters, so a tie in updated_at cannot break them apart
//     -- and the shared ORDER BY is a TOTAL order.
//  2. A domain with no approved creative is status "missing" with a reason:
//     not a 500, not a silently-omitted row, not a fabricated one.
//  3. A creative whose bytes changed after approval is REFUSED with an
//     operator-facing sentence.
//  4. The kind discriminator is actually in the live-slot key, so a warm-up
//     request and a newsletter request for the same kumo domain on the same
//     Denver day no longer collide.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

// newsletterCreativeRows mirrors newsletterCreativeCols, in order.
func newsletterCreativeRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "brand_code", "filename", "subject", "preheader", "html",
		"updated_at", "generated_at", "approval_status", "approved_by",
		"html_length", "live_sha256"})
}

func getNewsletter(t *testing.T, s *PMTACampaignService, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET",
		"/api/mailing/pmta-campaign/newsletter/preview"+query, nil)
	req.Header.Set("X-Organization-ID", warmupTestOrg)
	rec := httptest.NewRecorder()
	s.HandleNewsletterPreview(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// 1. PREVIEW AND SENDER SELECT THE SAME ROW GIVEN TIES
// ---------------------------------------------------------------------------

// TestCanonicalCreativeQueryIsOneDefinition — the single-row and the
// all-domains queries are COMPOSED from the same predicate and the same order,
// so they cannot drift the way the Go preview and the Python sender did.
func TestCanonicalCreativeQueryIsOneDefinition(t *testing.T) {
	for name, q := range map[string]string{
		"single": CanonicalNewsletterCreativeSQL,
		"batch":  CanonicalNewsletterCreativeBatchSQL,
	} {
		if !strings.Contains(q, newsletterCreativeWhere) {
			t.Fatalf("%s: must embed the shared WHERE verbatim, not a copy", name)
		}
		if !strings.Contains(q, newsletterCreativeOrder) {
			t.Fatalf("%s: must embed the shared ORDER BY verbatim, not a copy", name)
		}
		// The producer gate is the discriminator. Without it the sender
		// resolves an auto-insurance CPC offer creative for em.discountblog.com
		// and a NEWSLETTER SEND SHIPS AN OFFER.
		if !strings.Contains(q, "approved_by = ANY($3::text[])") {
			t.Fatalf("%s: must gate on the approved_by producer stamps", name)
		}
		if !strings.Contains(q, "approval_status = 'approved'") {
			t.Fatalf("%s: must require approval_status='approved'", name)
		}
		// filename is NOT the discriminator -- it is a naming convention with
		// no constraint behind it, and relying on it is what broke this.
		if strings.Contains(q, "kumo-digest") {
			t.Fatalf("%s: filename must not be the discriminator", name)
		}
	}

	// TOTAL order. All 11 kumo digest rows share one updated_at, so without a
	// unique final key the tie is resolved arbitrarily and two readers of the
	// same data can disagree.
	if !strings.HasSuffix(strings.TrimSpace(newsletterCreativeOrder), "id DESC") {
		t.Fatalf("ORDER BY must end in a unique key (id DESC), got %q", newsletterCreativeOrder)
	}

	// The stamp the Python builder writes is a cross-language contract.
	want := map[string]bool{"kumo_newsletter_stage": true, "legacy_newsletter_stage": true}
	if len(NewsletterProducerStamps) != len(want) {
		t.Fatalf("producer stamps changed: %v", NewsletterProducerStamps)
	}
	for _, s := range NewsletterProducerStamps {
		if !want[s] {
			t.Fatalf("unexpected producer stamp %q -- this string is the contract with the Python builder", s)
		}
	}
}

// TestPreviewAndSenderIssueTheSameStatement — the wizard preview
// (HandleWarmupCreative) and the exported selector the Python sender must
// converge on (SelectNewsletterCreative) run byte-identical SQL with identical
// parameters. Given a TIE (two approved rows sharing updated_at AND
// generated_at) they therefore resolve to the same row by construction, not by
// two queries happening to agree.
func TestPreviewAndSenderIssueTheSameStatement(t *testing.T) {
	shared := time.Date(2026, 8, 20, 16, 33, 0, 0, time.UTC)
	gen := time.Date(2026, 8, 11, 18, 13, 0, 0, time.UTC)

	// The tie: identical updated_at and generated_at. `id DESC` is the only
	// thing that decides, and both callers ship it.
	tieRows := func() *sqlmock.Rows {
		return newsletterCreativeRows().
			AddRow("ffffffff-0000-0000-0000-000000000002", "aadwd.com",
				"nl-aad-kumo-digest.html", "Winner", "pre", "<b>winner</b>",
				shared, gen, "approved", "kumo_newsletter_stage", int64(10), "sha-winner")
	}

	exact := regexp.QuoteMeta(CanonicalNewsletterCreativeSQL)

	// (a) preview
	sPrev, mPrev := newLedgerServiceWithMock(t)
	mPrev.ExpectQuery(exact).
		WithArgs(warmupTestOrg, pq.Array([]string{"aadwd.com"}),
			pq.Array(NewsletterProducerStamps), "").
		WillReturnRows(tieRows())
	req := httptest.NewRequest("GET",
		"/api/mailing/pmta-campaign/warmup/creative?sending_domain=em.aadwd.com", nil)
	req.Header.Set("X-Organization-ID", warmupTestOrg)
	rec := httptest.NewRecorder()
	sPrev.HandleWarmupCreative(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: got %d: %s", rec.Code, rec.Body.String())
	}
	var prev struct {
		CreativeID string `json:"creative_id"`
		SHA256     string `json:"sha256"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &prev); err != nil {
		t.Fatal(err)
	}
	if err := mPrev.ExpectationsWereMet(); err != nil {
		t.Fatalf("preview must run the canonical statement verbatim: %v", err)
	}

	// (b) the selector the sender converges on -- same SQL, same args.
	sSend, mSend := newLedgerServiceWithMock(t)
	mSend.ExpectQuery(exact).
		WithArgs(warmupTestOrg, pq.Array([]string{"aadwd.com"}),
			pq.Array(NewsletterProducerStamps), "").
		WillReturnRows(tieRows())
	got, err := SelectNewsletterCreative(context.Background(), sSend.db, warmupTestOrg, "aadwd.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := mSend.ExpectationsWereMet(); err != nil {
		t.Fatalf("sender selector must run the canonical statement verbatim: %v", err)
	}

	if got == nil || got.CreativeID != prev.CreativeID || got.SHA256 != prev.SHA256 {
		t.Fatalf("preview and sender disagreed on a tie: preview=%+v sender=%+v", prev, got)
	}
}

// TestSelectNewsletterCreativeReturnsAbsenceNotError — no approved creative is
// an ABSENCE, not an error and not a zero-valued struct the caller might
// render as a real row.
func TestSelectNewsletterCreativeReturnsAbsenceNotError(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	mock.ExpectQuery(`FROM mailing_creatives`).
		WillReturnRows(newsletterCreativeRows()) // zero rows
	c, err := SelectNewsletterCreative(context.Background(), s.db, warmupTestOrg, "discountblog.com", "")
	if err != nil {
		t.Fatalf("absence must not be an error: %v", err)
	}
	if c != nil {
		t.Fatalf("absence must be nil, got %+v", c)
	}
}

// ---------------------------------------------------------------------------
// 2. A DOMAIN WITH NO APPROVED CREATIVE IS "missing" -- NOT A 500, NOT OMITTED
// ---------------------------------------------------------------------------

func TestNewsletterPreviewReportsMissingCreativeVisibly(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)

	mock.ExpectQuery(`FROM mailing_sending_profiles`).
		WithArgs(warmupTestOrg, "").
		WillReturnRows(sqlmock.NewRows([]string{"sending_domain", "from_name", "from_email"}).
			AddRow("em.aadwd.com", "Jamie @ AAD", "hello@em.aadwd.com").
			AddRow("em.discountblog.com", "Jamie @ Discount Blog", "hello@em.discountblog.com"))
	// Only ONE of the two properties has an approved newsletter.
	mock.ExpectQuery(`FROM mailing_creatives`).
		WillReturnRows(newsletterCreativeRows().
			AddRow("11111111-0000-0000-0000-000000000001", "aadwd.com",
				"nl-aad-kumo-digest.html", "Today's read", "Three minutes",
				"<html>aad</html>", time.Now().Add(-2*time.Hour),
				time.Now().Add(-9*24*time.Hour), "approved", "kumo_newsletter_stage",
				int64(6744), "sha-aad"))

	rec := getNewsletter(t, s, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("a missing creative must NOT be an error: got %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Day     string `json:"day"`
		Ready   int    `json:"ready"`
		Missing int    `json:"missing"`
		Domains []struct {
			SendingDomain  string `json:"sending_domain"`
			Apex           string `json:"apex"`
			FromName       string `json:"from_name"`
			FromEmail      string `json:"from_email"`
			CreativeID     string `json:"creative_id"`
			CreativeSHA256 string `json:"creative_sha256"`
			Status         string `json:"status"`
			Reason         string `json:"reason"`
			HTML           string `json:"html"`
		} `json:"domains"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// The domain without a creative is PRESENT. Omitting it is the defect --
	// the operator is auditing what will send.
	if len(resp.Domains) != 2 {
		t.Fatalf("every eligible domain must be returned, got %d: %s", len(resp.Domains), rec.Body.String())
	}
	if resp.Ready != 1 || resp.Missing != 1 {
		t.Fatalf("counts wrong: ready=%d missing=%d", resp.Ready, resp.Missing)
	}
	if resp.Day == "" {
		t.Fatal("response must carry the Denver day it is describing")
	}

	byDomain := map[string]int{}
	for i, d := range resp.Domains {
		byDomain[d.SendingDomain] = i
	}
	ok := resp.Domains[byDomain["em.aadwd.com"]]
	if ok.Status != newsletterStatusReady || ok.CreativeID == "" || ok.CreativeSHA256 == "" {
		t.Fatalf("ready domain wrong: %+v", ok)
	}
	// from_name/from_email come from the DOMAIN's sending profile, never the
	// creative row -- DKIM alignment.
	if ok.FromName != "Jamie @ AAD" || ok.FromEmail != "hello@em.aadwd.com" {
		t.Fatalf("from_name/from_email must come from the sending profile: %+v", ok)
	}
	if ok.HTML != "" {
		t.Fatal("html must be omitted unless include_html=1")
	}

	miss := resp.Domains[byDomain["em.discountblog.com"]]
	if miss.Status != newsletterStatusMissing {
		t.Fatalf("domain with no approved creative must be %q, got %q", newsletterStatusMissing, miss.Status)
	}
	// NOT FABRICATED: no id, no sha, no borrowed creative from another brand.
	if miss.CreativeID != "" || miss.CreativeSHA256 != "" {
		t.Fatalf("missing domain must carry NO creative fields: %+v", miss)
	}
	if miss.Reason == "" {
		t.Fatal("a non-ready domain must explain itself to the operator")
	}
	if !strings.Contains(miss.Reason, "discountblog.com") {
		t.Fatalf("the reason must name the property: %q", miss.Reason)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestNewsletterPreviewStaleAndIncludeHTML — a creative that today's stage did
// not re-register is "stale" with a reason (it would re-send yesterday's
// articles), and include_html=1 actually returns a body. The blank preview
// iframe is what made the operator approve blind.
func TestNewsletterPreviewStaleAndIncludeHTML(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	mock.ExpectQuery(`FROM mailing_sending_profiles`).
		WithArgs(warmupTestOrg, "em.hfcl.net").
		WillReturnRows(sqlmock.NewRows([]string{"sending_domain", "from_name", "from_email"}).
			AddRow("em.hfcl.net", "Walter @ HFC", "hello@em.hfcl.net"))
	mock.ExpectQuery(`FROM mailing_creatives`).
		WillReturnRows(newsletterCreativeRows().
			AddRow("22222222-0000-0000-0000-000000000002", "hfcl.net",
				"nl-hfc-kumo-digest.html", "Old read", "pre", "<html>hfc body</html>",
				time.Now().Add(-72*time.Hour), time.Now().Add(-16*24*time.Hour),
				"approved", "kumo_newsletter_stage", int64(4200), "sha-hfc"))

	rec := getNewsletter(t, s, "?include_html=1&sending_domain=em.hfcl.net")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		IncludeHTML bool `json:"include_html"`
		Stale       int  `json:"stale"`
		Domains     []struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
			HTML   string `json:"html"`
		} `json:"domains"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Domains) != 1 {
		t.Fatalf("want 1 domain, got %d", len(resp.Domains))
	}
	d := resp.Domains[0]
	if d.Status != newsletterStatusStale {
		t.Fatalf("a 72h-old creative must be %q, got %q", newsletterStatusStale, d.Status)
	}
	if d.Reason == "" {
		t.Fatal("stale must explain itself")
	}
	if !resp.IncludeHTML || d.HTML != "<html>hfc body</html>" {
		t.Fatalf("include_html=1 must return the body, got %q", d.HTML)
	}
	if resp.Stale != 1 {
		t.Fatalf("stale count wrong: %d", resp.Stale)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// 3. A SHA MISMATCH IS REFUSED WITH A CLEAR REASON
// ---------------------------------------------------------------------------

func TestNewsletterRequestRefusesDriftedCreative(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	creativeID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	mock.ExpectBegin()
	mock.ExpectQuery(`FROM mailing_sending_profiles`).
		WithArgs(warmupTestOrg, "em.discountblog.com", ""). // newsletters span all 27
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`FROM mailing_creatives`).
		WithArgs(creativeID, warmupTestOrg).
		WillReturnRows(sqlmock.NewRows([]string{"sha", "approval_status", "updated_at"}).
			AddRow("bbbbbbbbbbbbbbbb", "approved",
				time.Date(2026, 8, 20, 16, 33, 0, 0, time.UTC)))
	// DECISIVE: no INSERT expectation. sqlmock (ordered) fails if the handler
	// writes anyway.
	mock.ExpectRollback()

	body := fmt.Sprintf(`{
		"sending_domain":"em.discountblog.com",
		"kind":"newsletter",
		"creative_id":%q,
		"creative_sha256":"aaaaaaaaaaaaaaaa",
		"scheduled_at":%q,
		"status":"requested"
	}`, creativeID, warmupFutureRFC3339())

	rec := postWarmup(t, s, body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("drifted creative: got %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	msg := rec.Body.String()
	// The refusal has to be an OPERATOR sentence, not "insert failed".
	for _, want := range []string{
		"em.discountblog.com", // which property
		"aaaaaaaaaaaa",        // what was approved
		"bbbbbbbbbbbb",        // what is live now
		"2026-08-20",          // when it changed
		"refused",             // that nothing was sent
		"resubmit",            // what to do about it
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal must contain %q: %s", want, msg)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestNewsletterRequestRequiresAPinnedSha — handing an UNPINNED newsletter to
// the builder is refused, because creative_id alone does not pin the bytes:
// kumo_newsletter_stage.py UPDATEs html_content on the same row id every
// morning. Refused before any DB access.
func TestNewsletterRequestRequiresAPinnedSha(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`FROM mailing_sending_profiles`).
		WithArgs(warmupTestOrg, "em.discountblog.com", "").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectRollback()

	rec := postWarmup(t, s, fmt.Sprintf(`{
		"sending_domain":"em.discountblog.com","kind":"newsletter",
		"creative_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"scheduled_at":%q,"status":"requested"}`, warmupFutureRFC3339()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unpinned newsletter: got %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "creative_sha256") {
		t.Fatalf("400 must name the missing pin: %s", rec.Body.String())
	}

	// And an unapproved creative is refused even when the sha matches -- only
	// approved copy mails.
	s2, mock2 := newLedgerServiceWithMock(t)
	mock2.ExpectBegin()
	mock2.ExpectQuery(`FROM mailing_sending_profiles`).
		WithArgs(warmupTestOrg, "em.discountblog.com", "").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock2.ExpectQuery(`FROM mailing_creatives`).
		WillReturnRows(sqlmock.NewRows([]string{"sha", "approval_status", "updated_at"}).
			AddRow("aaaaaaaaaaaaaaaa", "pending", time.Now()))
	mock2.ExpectRollback()
	rec2 := postWarmup(t, s2, fmt.Sprintf(`{
		"sending_domain":"em.discountblog.com","kind":"newsletter",
		"creative_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"creative_sha256":"aaaaaaaaaaaaaaaa",
		"scheduled_at":%q,"status":"requested"}`, warmupFutureRFC3339()))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("unapproved creative: got %d, want 409 (%s)", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "not approved") {
		t.Fatalf("409 must say the creative is not approved: %s", rec2.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 4. THE KIND DISCRIMINATOR PREVENTS THE LIVE-SLOT COLLISION
// ---------------------------------------------------------------------------

// TestLiveSlotIsKindAware — the index key must carry `kind`. Without it the 11
// kumo domains (which are BOTH warm-up and newsletter properties) can hold
// only ONE live request per Denver day, and the second raises a unique
// violation the operator sees as a bare 500.
func TestLiveSlotIsKindAware(t *testing.T) {
	ddl := KumoWarmupRequestsLiveSlotDDL
	if !strings.Contains(ddl, "kind") {
		t.Fatal("live-slot index must include `kind` or warm-up and newsletter collide on domain+day")
	}
	// The NAME must change with the definition: CREATE UNIQUE INDEX IF NOT
	// EXISTS is a no-op when the old name already exists, so reusing it would
	// leave the kind-blind key in place forever on any DB that already has it.
	if strings.Contains(ddl, "idx_kumo_warmup_requests_live_slot") {
		t.Fatal("the re-cut index must use a NEW name, or IF NOT EXISTS silently keeps the old key")
	}
	if !strings.Contains(ddl, "idx_campaign_requests_live_slot_v2") {
		t.Fatalf("unexpected live-slot index name: %s", ddl)
	}
	// The old index must actually be retired, or both exist and the kind-blind
	// one still rejects the second request.
	if !strings.Contains(CampaignRequestDropKindBlindSlotDDL, "DROP INDEX IF EXISTS idx_kumo_warmup_requests_live_slot") {
		t.Fatalf("the kind-blind index must be dropped: %s", CampaignRequestDropKindBlindSlotDDL)
	}
	// Key ORDER pins the semantics: org, kind, domain, Denver day.
	body := ddl[strings.Index(ddl, "("):]
	for i, col := range []string{"organization_id", "kind", "sending_domain", "America/Denver"} {
		idx := strings.Index(body, col)
		if idx < 0 {
			t.Fatalf("live-slot key missing %q", col)
		}
		if i > 0 && idx < strings.Index(body, []string{"organization_id", "kind", "sending_domain", "America/Denver"}[i-1]) {
			t.Fatalf("live-slot key columns out of order at %q", col)
		}
	}
}

// TestKindDiscriminatorIsBoundAndClosed — `kind` is written as a bound
// parameter (so the DB key really differs between the two modes), defaults to
// the pre-newsletter behaviour, and rejects anything outside the allow-list.
func TestKindDiscriminatorIsBoundAndClosed(t *testing.T) {
	// A newsletter for a NON-kumo domain must be accepted: newsletters span
	// all 27 sending domains, so the routing_mode gate is '' for this kind.
	s, mock := newLedgerServiceWithMock(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`FROM mailing_sending_profiles`).
		WithArgs(warmupTestOrg, "em.discountblog.com", "").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`INSERT INTO mailing_kumo_warmup_requests`).
		WithArgs(warmupTestOrg, "em.discountblog.com", "newsletter", sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("33333333-0000-0000-0000-000000000003"))
	mock.ExpectCommit()
	rec := postWarmup(t, s, fmt.Sprintf(`{
		"sending_domain":"em.discountblog.com","kind":"newsletter",
		"scheduled_at":%q,"status":"draft"}`, warmupFutureRFC3339()))
	if rec.Code != http.StatusOK {
		t.Fatalf("newsletter on a PMTA/SES domain must be allowed: got %d %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("kind must be a bound INSERT parameter: %v", err)
	}

	// The same domain is STILL refused for a warm-up request -- broadening the
	// gate must not broaden the kumo programme.
	s2, mock2 := newLedgerServiceWithMock(t)
	mock2.ExpectBegin()
	mock2.ExpectQuery(`FROM mailing_sending_profiles`).
		WithArgs(warmupTestOrg, "em.discountblog.com", "kumo").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock2.ExpectRollback()
	rec2 := postWarmup(t, s2, fmt.Sprintf(`{
		"sending_domain":"em.discountblog.com","scheduled_at":%q}`, warmupFutureRFC3339()))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("warm-up on a non-kumo domain must still be 400: got %d", rec2.Code)
	}

	// Closed allow-list, refused before any DB access.
	s3, mock3 := newLedgerServiceWithMock(t)
	rec3 := postWarmup(t, s3, fmt.Sprintf(`{
		"sending_domain":"em.aadwd.com","kind":"broadcast","scheduled_at":%q}`, warmupFutureRFC3339()))
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("unknown kind: got %d, want 400", rec3.Code)
	}
	if err := mock3.ExpectationsWereMet(); err != nil {
		t.Fatalf("unknown kind must be refused before any DB access: %v", err)
	}
}

// TestLiveSlotViolationBecomesA409 — if the slot IS hit (same kind,
// same domain, same day) the operator gets a 409 that explains it, not the
// bare 500 the audit found.
func TestLiveSlotViolationBecomesA409(t *testing.T) {
	s, mock := newLedgerServiceWithMock(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`FROM mailing_sending_profiles`).
		WithArgs(warmupTestOrg, "em.aadwd.com", "kumo").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`INSERT INTO mailing_kumo_warmup_requests`).
		WillReturnError(&pq.Error{
			Code:       "23505",
			Constraint: "idx_campaign_requests_live_slot_v2",
			Message:    "duplicate key value violates unique constraint",
		})
	mock.ExpectRollback()

	rec := postWarmup(t, s, fmt.Sprintf(`{
		"sending_domain":"em.aadwd.com","scheduled_at":%q,"status":"requested"}`,
		warmupFutureRFC3339()))
	if rec.Code != http.StatusConflict {
		t.Fatalf("live-slot violation: got %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	msg := rec.Body.String()
	for _, want := range []string{"em.aadwd.com", "kumo_warmup", "Cancel it"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("409 must explain the slot: want %q in %s", want, msg)
		}
	}
	// A NON-slot unique violation must NOT be dressed up as a slot conflict.
	if _, ok := warmupLiveSlotConflict(&pq.Error{Code: "23505", Constraint: "mailing_creatives_pkey"},
		"newsletter", "em.aadwd.com", time.Now()); ok {
		t.Fatal("only the live-slot constraint may produce the slot message")
	}
	if _, ok := warmupLiveSlotConflict(fmt.Errorf("connection reset"),
		"newsletter", "em.aadwd.com", time.Now()); ok {
		t.Fatal("a non-pq error must not be reported as a slot conflict")
	}
}

// ---------------------------------------------------------------------------
// 5. isp_quotas accepts BOTH wire shapes, and volume:0 SURVIVES
// ---------------------------------------------------------------------------

func TestISPQuotasAcceptsBothShapes(t *testing.T) {
	// The wizard sends the array; the handler declared the object. The array
	// used to decode to a nil map and every quota was silently dropped --
	// including the volume:0-per-ISP marker that MEANS "audience-bound,
	// uncapped". A zero must survive.
	arr, err := warmupParseISPQuotas([]byte(`[{"isp":"yahoo","volume":0},{"isp":"aol","volume":327}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(arr) != 2 {
		t.Fatalf("array shape dropped entries: %+v", arr)
	}
	if v, ok := arr["yahoo"]; !ok || v != 0 {
		t.Fatalf("volume:0 must survive (it is the audience-bound marker): %+v", arr)
	}
	if arr["aol"] != 327 {
		t.Fatalf("array shape wrong: %+v", arr)
	}

	obj, err := warmupParseISPQuotas([]byte(`{"yahoo":0,"aol":327}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(obj) != 2 || obj["yahoo"] != 0 || obj["aol"] != 327 {
		t.Fatalf("object shape wrong: %+v", obj)
	}

	for _, empty := range []string{"", "null"} {
		m, err := warmupParseISPQuotas([]byte(empty))
		if err != nil || m != nil {
			t.Fatalf("%q must decode to a nil map with no error, got %+v / %v", empty, m, err)
		}
	}
	if _, err := warmupParseISPQuotas([]byte(`"gentle"`)); err == nil {
		t.Fatal("a scalar isp_quotas must be a 400, not silently ignored")
	}
}

// ---------------------------------------------------------------------------
// 6. DDL: skip-probe classification and the 5s startup-migration budget
// ---------------------------------------------------------------------------

func TestNewsletterDDLFitsMigrationBudget(t *testing.T) {
	// Single-action ADD COLUMN IF NOT EXISTS: recognized by reMigAddColumn
	// (cmd/server/migration_skip.go:46), so the probe skips it on every boot
	// after the first and it takes a lock exactly once. A top-level comma
	// would disqualify it and it would re-execute forever.
	for name, ddl := range map[string]string{
		"kind":         CampaignRequestKindDDL,
		"creative_sha": CampaignRequestCreativeShaDDL,
	} {
		trimmed := strings.TrimSpace(ddl)
		if !strings.HasPrefix(trimmed, "ALTER TABLE") {
			t.Fatalf("%s: must LEAD with ALTER TABLE for skip-probe classification", name)
		}
		if !strings.Contains(ddl, "ADD COLUMN IF NOT EXISTS") {
			t.Fatalf("%s: must be idempotent", name)
		}
		if strings.Count(ddl, "ADD COLUMN") != 1 {
			t.Fatalf("%s: must be a SINGLE action -- a multi-action ALTER is never skipped", name)
		}
		if strings.Contains(ddl, ",") {
			t.Fatalf("%s: a top-level comma disqualifies it from reMigAddColumn", name)
		}
		if strings.Contains(ddl, ";") {
			t.Fatalf("%s: one statement per entry, no semicolon", name)
		}
		// The runner executes one statement per entry; a probe-eligible ALTER
		// must actually be probe-eligible.
		if !regexp.MustCompile(`(?is)^\s*ALTER\s+TABLE\s+(?:ONLY\s+)?([a-z0-9_.]+)\s+ADD\s+COLUMN\s+IF\s+NOT\s+EXISTS\s+([a-z0-9_]+)(?:[^,()]|\([^)]*\))*$`).
			MatchString(ddl) {
			t.Fatalf("%s: does not match reMigAddColumn -- it would execute on EVERY boot", name)
		}
	}

	// DROP INDEX is NOT a shape the probe recognizes, so it re-runs each boot.
	// That is intentional and free (catalog lookup, no table lock) -- but it
	// MUST be IF EXISTS or every boot after the first logs an ERROR.
	if !strings.Contains(CampaignRequestDropKindBlindSlotDDL, "IF EXISTS") {
		t.Fatal("the DROP re-runs on every boot; it must be IF EXISTS")
	}
	if strings.Contains(CampaignRequestDropKindBlindSlotDDL, ";") {
		t.Fatal("one statement per entry, no semicolon")
	}

	// lock_timeout is scoped: runStartupMigrations holds ONE dedicated
	// connection for the whole slice, so a SET without a matching RESET would
	// silently re-scope every LATER migration in the slice.
	if !strings.Contains(CampaignRequestLockTimeoutDDL, "SET lock_timeout") {
		t.Fatalf("expected a lock_timeout guard, got %q", CampaignRequestLockTimeoutDDL)
	}
	if !strings.Contains(CampaignRequestLockTimeoutResetDDL, "RESET lock_timeout") {
		t.Fatal("the lock_timeout SET must be paired with a RESET")
	}

	// No backfill anywhere: a timed-out DML entry is logged "skipped, will
	// retry next boot" and is then silently absent forever.
	for name, ddl := range map[string]string{
		"kind":         CampaignRequestKindDDL,
		"creative_sha": CampaignRequestCreativeShaDDL,
		"drop_slot":    CampaignRequestDropKindBlindSlotDDL,
		"live_slot_v2": KumoWarmupRequestsLiveSlotDDL,
	} {
		for _, dml := range []string{"INSERT ", "UPDATE ", "DELETE ", "SELECT "} {
			if strings.Contains(strings.ToUpper(ddl), dml) {
				t.Fatalf("%s: no %s in the 5s slice", name, strings.TrimSpace(dml))
			}
		}
		if strings.Contains(strings.ToUpper(ddl), "CONCURRENTLY") {
			t.Fatalf("%s: CONCURRENTLY belongs in concurrentIndexSpecs", name)
		}
	}

	// The table is NOT renamed: ALTER TABLE ... RENAME has no IF EXISTS form
	// and is not a probe-recognized shape, so it would ERROR on every boot
	// after the first.
	for _, ddl := range []string{CampaignRequestKindDDL, CampaignRequestCreativeShaDDL,
		CampaignRequestDropKindBlindSlotDDL, KumoWarmupRequestsLiveSlotDDL} {
		if strings.Contains(strings.ToUpper(ddl), "RENAME") {
			t.Fatal("never RENAME this table -- it would ERROR on every boot after the first")
		}
	}
}
