package api

// Wave-1 tests: the smart-link `hash` dictionary key and the tracking-layer
// offer-link rewrite (/o/<sub>/<hash>/<campaign>).
//
// Coverage:
//   - RewriteMoneyLinksToTracking: single/multiple cratoolpro hrefs rewritten
//     to the exact /o/ URL, non-cratoolpro untouched, /integration/ excluded,
//     idempotent, and the empty-segment safe-no-op guard.
//   - SmartLinkTrackingURL: exact shape + scheme normalization (bare host,
//     https-prefixed host, trailing slash).
//   - resolveByHash (sqlmock): active row scanned incl. hash; ErrNoRows.
//   - HandleAdminCreate: mints a valid non-empty hash server-side when none is
//     supplied, and the create response carries it.
//
// Reuses newSmartLinkService / smartLinkCols (smartlink_handlers_test.go) and
// the proof consts (offer_proof_send.go).

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// RewriteMoneyLinksToTracking
// ---------------------------------------------------------------------------

func TestRewriteMoneyLinksToTracking_ExactShapeSingle(t *testing.T) {
	in := `<a href="https://www.cratoolpro.com/A/B/?source_id=email&sub1=x">go</a>`
	out, n := RewriteMoneyLinksToTracking(in, "t.em.discountblog.com", "sub-1", "hsh12", "camp-9")
	if n != 1 {
		t.Fatalf("expected 1 rewrite, got %d", n)
	}
	want := `href="https://t.em.discountblog.com/o/sub-1/hsh12/camp-9"`
	if !strings.Contains(out, want) {
		t.Errorf("missing exact /o/ URL %q in: %s", want, out)
	}
	if strings.Contains(out, "cratoolpro.com") {
		t.Errorf("cratoolpro should be gone: %s", out)
	}
}

func TestRewriteMoneyLinksToTracking_Multiple(t *testing.T) {
	in := `<a href="http://cratoolpro.com/A/B/">one</a> mid ` +
		`<a href="https://www.cratoolpro.com/C/D/?q=1">two</a>`
	out, n := RewriteMoneyLinksToTracking(in, "t.em.quizfiesta.com", "s2", "h2", "c2")
	if n != 2 {
		t.Fatalf("expected 2 rewrites, got %d", n)
	}
	want := `href="https://t.em.quizfiesta.com/o/s2/h2/c2"`
	if strings.Count(out, want) != 2 {
		t.Errorf("expected 2 /o/ hrefs %q: %s", want, out)
	}
	if strings.Contains(out, "cratoolpro") {
		t.Errorf("cratoolpro should be gone: %s", out)
	}
}

func TestRewriteMoneyLinksToTracking_NonCratoolproUntouched(t *testing.T) {
	in := `<a href="https://discountblog.com/deal">house</a> ` +
		`<a href="https://example.com/x">other</a> ` +
		`<a href="mailto:hi@example.com">mail</a>`
	out, n := RewriteMoneyLinksToTracking(in, "t.em.discountblog.com", "s", "h", "c")
	if n != 0 {
		t.Fatalf("expected 0 rewrites, got %d", n)
	}
	if out != in {
		t.Errorf("non-cratoolpro hrefs must be untouched:\n in=%s\nout=%s", in, out)
	}
}

func TestRewriteMoneyLinksToTracking_IntegrationExcluded(t *testing.T) {
	in := `<a href="https://www.cratoolpro.com/integration/postback?x=1">pixel</a>`
	out, n := RewriteMoneyLinksToTracking(in, "t.em.discountblog.com", "s", "h", "c")
	if n != 0 || out != in {
		t.Fatalf("integration path must NOT be rewritten: n=%d out=%s", n, out)
	}
	// Case-insensitive too.
	in2 := `<a href="https://cratoolpro.com/Integration/x">y</a>`
	out2, n2 := RewriteMoneyLinksToTracking(in2, "t.em.discountblog.com", "s", "h", "c")
	if n2 != 0 || out2 != in2 {
		t.Errorf("case-insensitive integration exclusion failed: n=%d out=%s", n2, out2)
	}
}

func TestRewriteMoneyLinksToTracking_MixedIntegrationAndOffer(t *testing.T) {
	in := `<a href="https://cratoolpro.com/integration/pb">pixel</a> ` +
		`<a href="https://www.cratoolpro.com/REAL/OFFER/?source_id=email">buy</a>`
	out, n := RewriteMoneyLinksToTracking(in, "t.em.discountblog.com", "s", "h", "c")
	if n != 1 {
		t.Fatalf("expected exactly 1 rewrite (offer only), got %d", n)
	}
	if !strings.Contains(out, "cratoolpro.com/integration/pb") {
		t.Errorf("integration link should survive: %s", out)
	}
	if strings.Contains(out, "cratoolpro.com/REAL") {
		t.Errorf("offer link should be rewritten away: %s", out)
	}
	if !strings.Contains(out, `href="https://t.em.discountblog.com/o/s/h/c"`) {
		t.Errorf("/o/ href missing: %s", out)
	}
}

func TestRewriteMoneyLinksToTracking_Idempotent(t *testing.T) {
	in := `<a href="https://www.cratoolpro.com/A/B/?source_id=email">go</a>`
	once, n1 := RewriteMoneyLinksToTracking(in, "t.em.discountblog.com", "s", "h", "c")
	twice, n2 := RewriteMoneyLinksToTracking(once, "t.em.discountblog.com", "s", "h", "c")
	if n1 != 1 {
		t.Fatalf("first pass expected 1, got %d", n1)
	}
	if n2 != 0 {
		t.Fatalf("second pass must be a no-op, got %d", n2)
	}
	if once != twice {
		t.Errorf("rewrite not idempotent:\nonce =%s\ntwice=%s", once, twice)
	}
}

func TestRewriteMoneyLinksToTracking_EmptySegmentIsSafeNoOp(t *testing.T) {
	in := `<a href="https://www.cratoolpro.com/A/B/?source_id=email">go</a>`
	cases := []struct {
		name                    string
		td, sub, hash, campaign string
	}{
		{"empty domain", "", "s", "h", "c"},
		{"empty subscriber", "t.em.x.com", "", "h", "c"},
		{"empty hash", "t.em.x.com", "s", "", "c"},
		{"empty campaign", "t.em.x.com", "s", "h", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, n := RewriteMoneyLinksToTracking(in, tc.td, tc.sub, tc.hash, tc.campaign)
			if n != 0 {
				t.Errorf("expected no rewrite on %s, got n=%d", tc.name, n)
			}
			if out != in {
				t.Errorf("html must be unchanged on %s:\n in=%s\nout=%s", tc.name, in, out)
			}
			if strings.Contains(out, "/o//") || strings.Contains(out, "https:///o/") {
				t.Errorf("must not emit a malformed /o/ URL on %s: %s", tc.name, out)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// SmartLinkTrackingURL
// ---------------------------------------------------------------------------

func TestSmartLinkTrackingURL_ShapeAndSchemeNormalization(t *testing.T) {
	want := "https://t.em.discountblog.com/o/sub-1/hsh12/camp-9"
	cases := []struct {
		name, domain string
	}{
		{"bare host", "t.em.discountblog.com"},
		{"https prefixed", "https://t.em.discountblog.com"},
		{"trailing slash", "https://t.em.discountblog.com/"},
		{"padded", "  t.em.discountblog.com  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SmartLinkTrackingURL(tc.domain, "sub-1", "hsh12", "camp-9")
			if got != want {
				t.Errorf("%s: got %q want %q", tc.name, got, want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// resolveByHash
// ---------------------------------------------------------------------------

func TestResolveByHash_ScansHash(t *testing.T) {
	svc, mock := newSmartLinkService(t)
	now := time.Now().UTC().Truncate(time.Second)

	mock.ExpectQuery(`(?s)SELECT id, brand_root, slug, review_slug, offer_url_template, status, COALESCE\(risk_profile,'low'\).*WHERE hash = \$1 AND status = 'active'`).
		WithArgs("abc1234567").
		WillReturnRows(sqlmock.NewRows(smartLinkCols).AddRow(
			"11111111-2222-3333-4444-555555555555",
			"discountblog.com", "auto-refi", "best-auto-refi",
			"https://www.cratoolpro.com/A/B/?source_id=email",
			"active", "high", "", "abc1234567", now, now,
		))

	sl, err := svc.resolveByHash(context.Background(), "abc1234567")
	require.NoError(t, err)
	assert.Equal(t, "abc1234567", sl.Hash)
	assert.Equal(t, "active", sl.Status)
	assert.Equal(t, "auto-refi", sl.Slug)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestResolveByHash_ErrNoRows(t *testing.T) {
	svc, mock := newSmartLinkService(t)

	mock.ExpectQuery(`WHERE hash = \$1 AND status = 'active'`).
		WithArgs("missing").
		WillReturnRows(sqlmock.NewRows(smartLinkCols)) // zero rows

	_, err := svc.resolveByHash(context.Background(), "missing")
	require.Error(t, err)
	assert.Equal(t, "sql: no rows in result set", err.Error())
	assert.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// HandleAdminCreate — hash minting
// ---------------------------------------------------------------------------

// nonEmptyHashArg asserts the value passed to the INSERT is a valid, minted
// smart-link hash (matches smartLinkHashPattern) — proving the server minted a
// real token rather than inserting NULL/empty.
type nonEmptyHashArg struct{}

func (nonEmptyHashArg) Match(v driver.Value) bool {
	s, ok := v.(string)
	return ok && smartLinkHashPattern.MatchString(s)
}

func adminCreatePOST(t *testing.T, svc *SmartLinkService, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/smartlinks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	svc.HandleAdminCreate(rec, req)
	return rec
}

func TestHandleAdminCreate_MintsHash(t *testing.T) {
	svc, mock := newSmartLinkService(t)
	now := time.Now().UTC().Truncate(time.Second)

	// The INSERT's 8th arg (hash) must be a minted, pattern-valid token.
	mock.ExpectQuery(`INSERT INTO mailing_smart_links`).
		WithArgs(
			"discountblog.com", "auto-refi", "best-auto-refi",
			"https://cratoolpro.example/x?source_id=email",
			"active", "low", "",
			nonEmptyHashArg{},
		).
		WillReturnRows(sqlmock.NewRows(smartLinkCols).AddRow(
			"id-9", "discountblog.com", "auto-refi", "best-auto-refi",
			"https://cratoolpro.example/x?source_id=email",
			"active", "low", "", "mint000123", now, now,
		))

	rec := adminCreatePOST(t, svc,
		`{"brand_root":"discountblog.com","slug":"auto-refi","review_slug":"best-auto-refi","offer_url_template":"https://cratoolpro.example/x?source_id=email"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	var sl SmartLink
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sl))
	assert.NotEmpty(t, sl.Hash, "create response must carry the hash")
	assert.Equal(t, "mint000123", sl.Hash)
	// Guards against a future json:"-"/omitempty regression hiding the field.
	assert.Contains(t, rec.Body.String(), `"hash":"mint000123"`)
	assert.NoError(t, mock.ExpectationsWereMet())
}
