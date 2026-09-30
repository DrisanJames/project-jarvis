package tracking

// Open-pixel position marker tests (2026-09-29): ?p=t|b is read, carried on
// the event, counted, logged and persisted as the FIRST counted open's
// metadata — and nothing about signature checks or dedupe changes.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func signedOpenToken(key string) (enc, sig string) {
	enc = base64.URLEncoding.EncodeToString([]byte(testOrgID + "|" + testCampaignID + "|" + testSubscriberID + "|" + testEmailID))
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(enc))
	return enc, hex.EncodeToString(m.Sum(nil))[:16]
}

func TestHandleOpen_PixelPos(t *testing.T) {
	cases := []struct{ query, want, counter string }{
		{"?p=t", "t", "open.pos.t"},
		{"?p=b", "b", "open.pos.b"},
		{"?p=zzz", "", "open.pos.none"},
		{"?p=T", "", "open.pos.none"},
		{"?p=", "", "open.pos.none"},
		{"", "", "open.pos.none"},
	}
	for _, tc := range cases {
		t.Run("q="+tc.query, func(t *testing.T) {
			resetSigCounters(t)
			t.Setenv(SigModeEnv, "shadow")
			pub := &capturePublisher{}
			h := sigHandler(pub, "k1")
			enc, sig := signedOpenToken("k1")

			var logs bytes.Buffer
			prev := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(prev) })

			rr := httptest.NewRecorder()
			h.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/track/open/"+enc+"/"+sig+tc.query, nil))
			require.Equal(t, http.StatusOK, rr.Code)
			require.Equal(t, "image/gif", rr.Header().Get("Content-Type"))

			evt, ok := pub.last()
			require.True(t, ok, "open must publish")
			require.Equal(t, tc.want, evt.PixelPos)
			require.Equal(t, testCampaignID, evt.CampaignID)

			c := sigCounters.snapshot()
			// Signature still verifies with the query string present.
			require.EqualValues(t, 1, c["open.sig.valid"], "counters: %v", c)
			require.EqualValues(t, 1, c[tc.counter], "counters: %v", c)
			require.Contains(t, logs.String(), "OPEN campaign="+testCampaignID+" subscriber="+testSubscriberID+" pos="+tc.want+"\n")
		})
	}
}

func TestTrackingEvent_PixelPosOmittedWhenEmpty(t *testing.T) {
	b, err := json.Marshal(TrackingEvent{EventType: EventOpen})
	require.NoError(t, err)
	require.NotContains(t, string(b), "pixel_pos", "unmarked opens must be byte-identical on the wire")
	b, err = json.Marshal(TrackingEvent{EventType: EventOpen, PixelPos: "t"})
	require.NoError(t, err)
	require.Contains(t, string(b), `"pixel_pos":"t"`)
}

func TestOpenMetadataJSON(t *testing.T) {
	require.Equal(t, `{"pixel_pos":"t"}`, OpenMetadataJSON("t").String)
	require.True(t, OpenMetadataJSON("b").Valid)
	for _, p := range []string{"", "x", "tb", "B"} {
		require.False(t, OpenMetadataJSON(p).Valid, "pos %q must write NULL metadata", p)
	}
}

// expectFirstOpenUpToInsert queues the processOpen calls that precede the
// events INSERT for an admitted (first) open.
func expectFirstOpenUpToInsert(mock sqlmock.Sqlmock, now time.Time) {
	mock.ExpectQuery(`(?is)SELECT\s+EXISTS.*event_type\s*=\s*'opened'`).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`(?is)INSERT\s+INTO\s+mailing_open_dedupe`).
		WillReturnRows(sqlmock.NewRows([]string{"true"}).AddRow(true))
	mock.ExpectQuery(`(?is)SELECT\s+email\s+FROM\s+mailing_subscribers`).
		WillReturnRows(sqlmock.NewRows([]string{"email"}))
	mock.ExpectQuery(`(?is)SELECT\s+event_at.*event_type\s*=\s*'sent'`).
		WillReturnRows(sqlmock.NewRows([]string{"event_at"}))
}

func openEvt(now time.Time, pos string) TrackingEvent {
	return TrackingEvent{
		EventType: EventOpen, OrgID: testOrgID, CampaignID: testCampaignID,
		SubscriberID: testSubscriberID, EmailID: testEmailID,
		IPAddress: "1.2.3.4", UserAgent: "UA", PixelPos: pos, Timestamp: now,
	}
}

// A marked first open writes metadata {"pixel_pos":...}; a SECOND fetch of the
// other pixel is refused by the dedupe gate and writes nothing — so the row
// records the position that fired FIRST, whichever arrives first.
func TestProcessOpen_PixelPos_FirstCountedWinsMetadata(t *testing.T) {
	for _, first := range []string{"t", "b"} {
		second := map[string]string{"t": "b", "b": "t"}[first]
		t.Run("first="+first, func(t *testing.T) {
			c, mock := newConsumer(t)
			now := time.Now().UTC()

			expectFirstOpenUpToInsert(mock, now)
			mock.ExpectExec(`(?is)INSERT\s+INTO\s+mailing_tracking_events\s*\(.*is_machine_open,\s*metadata\).*\$9,\s*\$10::jsonb.*LEFT\s+JOIN\s+mailing_subscribers`).
				WithArgs(
					uuid.MustParse(testEmailID), uuid.MustParse(testOrgID),
					uuid.MustParse(testCampaignID), uuid.MustParse(testSubscriberID),
					now, "1.2.3.4", "UA", "desktop", false,
					OpenMetadataJSON(first),
				).
				WillReturnResult(sqlmock.NewResult(0, 0)) // 0 rows: stop before aggregates
			require.NoError(t, c.processOpen(context.Background(), openEvt(now, first)))

			// Second pixel: dedupe refuses → no INSERT, no side effects.
			later := now.Add(time.Second)
			mock.ExpectQuery(`(?is)SELECT\s+EXISTS.*event_type\s*=\s*'opened'`).
				WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
			mock.ExpectQuery(`(?is)INSERT\s+INTO\s+mailing_open_dedupe`).
				WillReturnRows(sqlmock.NewRows([]string{"true"})) // ErrNoRows
			require.NoError(t, c.processOpen(context.Background(), openEvt(later, second)))

			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

// Unknown marker → the exact pre-existing 9-arg statement (no metadata column).
func TestProcessOpen_PixelPos_UnknownWritesNoMetadata(t *testing.T) {
	c, mock := newConsumer(t)
	now := time.Now().UTC()
	expectFirstOpenUpToInsert(mock, now)
	mock.ExpectExec(openInsertRegex.String()).
		WithArgs(
			uuid.MustParse(testEmailID), uuid.MustParse(testOrgID),
			uuid.MustParse(testCampaignID), uuid.MustParse(testSubscriberID),
			now, "1.2.3.4", "UA", "desktop", false,
		).
		WillReturnResult(sqlmock.NewResult(0, 0))
	require.NoError(t, c.processOpen(context.Background(), openEvt(now, "zzz")))
	require.NoError(t, mock.ExpectationsWereMet())
}
