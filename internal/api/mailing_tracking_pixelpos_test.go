package api

// Open-pixel position marker (2026-09-29) on the API's own /track/open route
// and on the Campaign Center / transactional pixel builders.

import (
	"database/sql/driver"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/tracking"
	"github.com/ignite/sparkpost-monitor/internal/worker"
	"github.com/stretchr/testify/require"
)

const pixelPosTestKey = "pixelpos-test-key"

func pixelPosService(t *testing.T) (*MailingService, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return &MailingService{db: db, throttler: NewMailingThrottler(), signingKey: pixelPosTestKey}, mock
}

func servePixelPosOpen(svc *MailingService, path string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Get("/track/open/{data}/{sig}", svc.HandleTrackOpen)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func validOpenPath() string {
	data := encodeTrackingPayload(validOrgUUID, validCampUUID, validSubUUID, validMailUUID)
	return "/track/open/" + data + "/" + signData(data, pixelPosTestKey)[:16]
}

// expectAdmittedOpenUpToInsert: dedupe admits, email lookup empty, no 'sent'.
func expectAdmittedOpenUpToInsert(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`INSERT INTO mailing_open_dedupe`).
		WillReturnRows(sqlmock.NewRows([]string{"true"}).AddRow(true))
	mock.ExpectQuery(`SELECT email FROM mailing_subscribers`).
		WillReturnRows(sqlmock.NewRows([]string{"email"}))
	mock.ExpectQuery(`(?s)SELECT event_at FROM mailing_tracking_events.*'sent'`).
		WillReturnRows(sqlmock.NewRows([]string{"event_at"}))
}

func baseOpenInsertArgs() []driver.Value {
	return []driver.Value{
		uuid.MustParse(validMailUUID), uuid.MustParse(validOrgUUID),
		uuid.MustParse(validCampUUID), uuid.MustParse(validSubUUID),
		sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), false,
	}
}

// A marked first open: the sig still verifies with ?p= present (the INSERT is
// reached at all only past verifySig) and metadata carries the position.
func TestHandleTrackOpen_PixelPos_WritesMetadata(t *testing.T) {
	for _, pos := range []string{"t", "b"} {
		t.Run(pos, func(t *testing.T) {
			svc, mock := pixelPosService(t)
			expectAdmittedOpenUpToInsert(mock)
			mock.ExpectExec(`(?s)INSERT INTO mailing_tracking_events \(.*recipient_domain, metadata\).*\$9::jsonb`).
				WithArgs(append(baseOpenInsertArgs(), tracking.OpenMetadataJSON(pos))...).
				WillReturnResult(sqlmock.NewResult(0, 1))

			rec := servePixelPosOpen(svc, validOpenPath()+"?p="+pos)
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, "image/gif", rec.Header().Get("Content-Type"))
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

// Unknown or absent marker → the pre-existing 8-arg statement, no metadata.
func TestHandleTrackOpen_PixelPos_UnknownOrAbsentNoMetadata(t *testing.T) {
	for _, q := range []string{"", "?p=zzz", "?p="} {
		t.Run("q="+q, func(t *testing.T) {
			svc, mock := pixelPosService(t)
			expectAdmittedOpenUpToInsert(mock)
			mock.ExpectExec(`(?s)INSERT INTO mailing_tracking_events \(.*is_machine_open, recipient_domain\)\s+SELECT`).
				WithArgs(baseOpenInsertArgs()...).
				WillReturnResult(sqlmock.NewResult(0, 1))
			rec := servePixelPosOpen(svc, validOpenPath()+q)
			require.Equal(t, http.StatusOK, rec.Code)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

// Second pixel of the pair (either position) hits the dedupe gate and moves
// nothing: exactly one counted open per (campaign, subscriber).
func TestHandleTrackOpen_PixelPos_SecondPixelDeduped(t *testing.T) {
	for _, pos := range []string{"t", "b"} {
		svc, mock := pixelPosService(t)
		mock.ExpectQuery(`INSERT INTO mailing_open_dedupe`).
			WillReturnRows(sqlmock.NewRows([]string{"true"})) // ErrNoRows = already counted
		rec := servePixelPosOpen(svc, validOpenPath()+"?p="+pos)
		require.Equal(t, http.StatusOK, rec.Code)
		require.NoError(t, mock.ExpectationsWereMet(), "pos=%s", pos)
	}
}

// Negative control: a bad signature with a marker still short-circuits before
// any DB work — the marker does not bypass the check.
func TestHandleTrackOpen_PixelPos_BadSigStillRejected(t *testing.T) {
	svc, mock := pixelPosService(t)
	data := encodeTrackingPayload(validOrgUUID, validCampUUID, validSubUUID, validMailUUID)
	rec := servePixelPosOpen(svc, "/track/open/"+data+"/0000000000000000?p=t")
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

var apiPixelSrcRe = regexp.MustCompile(`<img src="([^"]*/track/open/[^"]+)"`)

// assertPixelPair checks top ?p=t + bottom ?p=b and that each URL's path
// verifies under svc.verifySig AND the tracking service's enc verifier.
func assertPixelPair(t *testing.T, svc *MailingService, out, base string) {
	t.Helper()
	require.Equal(t, 2, strings.Count(out, "/track/open/"), out)
	srcs := apiPixelSrcRe.FindAllStringSubmatch(out, -1)
	require.Len(t, srcs, 2)
	require.True(t, strings.HasSuffix(srcs[0][1], "?p=t"), srcs[0][1])
	require.True(t, strings.HasSuffix(srcs[1][1], "?p=b"), srcs[1][1])
	for _, m := range srcs {
		path := strings.SplitN(m[1], "?", 2)[0]
		parts := strings.Split(strings.TrimPrefix(path, base+"/track/open/"), "/")
		require.Len(t, parts, 2, m[1])
		require.True(t, svc.verifySig(parts[0], parts[1]), "api verifySig must accept %s", m[1])
		res := tracking.VerifyTrackingToken(parts[0], parts[1], tracking.ParseSigKeys(pixelPosTestKey),
			[]string{tracking.SigFormatEncoded})
		require.Equal(t, tracking.SigValid, res.Class, "tracking service enc verifier must accept %s", m[1])
	}
	// Top pixel immediately after <body>, bottom immediately before </body>.
	lower := strings.ToLower(out)
	bo := strings.Index(lower, "<body")
	afterBody := bo + strings.Index(out[bo:], ">") + 1
	require.True(t, strings.HasPrefix(out[afterBody:], `<div style="display:none;`), out)
	topEnd := afterBody + strings.Index(out[afterBody:], "</div>")
	require.Contains(t, out[afterBody:topEnd], "?p=t")
	bc := strings.LastIndex(lower, "</body>")
	require.True(t, strings.HasSuffix(out[:bc], `?p=b" width="1" height="1" border="0" alt="" /></div>`), out)
}

func TestInjectTrackingWithURL_PixelSigVerifies(t *testing.T) {
	svc, _ := pixelPosService(t)
	base := "https://t.em.example.com"
	html := `<html><body><p>x</p><a href="https://example.com/offer">go</a></body></html>`
	out := svc.injectTrackingWithURL(html, uuid.MustParse(validOrgUUID), uuid.MustParse(validCampUUID),
		uuid.MustParse(validSubUUID), uuid.MustParse(validMailUUID), base)
	assertPixelPair(t, svc, out, base)
	// Click rewrite unchanged and never touches the pixels.
	require.Contains(t, out, base+"/track/click/")
	require.NotContains(t, out, `href="https://example.com/offer"`)
	for _, m := range apiPixelSrcRe.FindAllStringSubmatch(out, -1) {
		require.NotContains(t, m[1], "/track/click/")
	}
	// No bare display:none on the <img> any more.
	require.NotRegexp(t, `<img[^>]*style=`, out)
}

// The transactional path now calls worker.InjectOpenPixel with its
// (org, txn, sub=txn, txn) token; the emitted pixels verify under the API's
// verifySig (the old raw-signed pixel did not).
func TestTransactionalPixel_SigVerifies(t *testing.T) {
	svc, _ := pixelPosService(t)
	base := "https://trk.em.example.com"
	txnID := uuid.New().String()
	out := worker.InjectOpenPixel(`<html><body>receipt</body></html>`, txnID, txnID, txnID, base, validOrgUUID, svc.signingKey)
	assertPixelPair(t, svc, out, base)
}
