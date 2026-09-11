package api

// TRACKING_SIG_MODE on the API server's own /track/* routes. Payloads use nil
// UUIDs + a destination so an ACCEPTED click exits via the pre-existing
// nil-UUID guard (302, no DB) and a BLOCKED one exits before it — strict
// sqlmock then proves "no event": any DB write fails the test.

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/tracking"
)

const sigTestDest = "https://offer-dest.example/SECRETPATH?sub1=SUBID-SECRET"

func sigClickData() string {
	return encodeTrackingPayload(nilUUID, nilUUID, nilUUID, nilUUID, sigTestDest)
}

func signEnc(key, data string) string { return tracking.SignTrackingMsg([]byte(key), []byte(data)) }

// apiClick drives HandleTrackClick; sig=="" models the sig-less route.
func apiClick(t *testing.T, signingKey, data, sig string) *httptest.ResponseRecorder {
	t.Helper()
	svc, mock, _ := newTrackingTestService(t)
	svc.signingKey = signingKey
	params := map[string]string{"data": data}
	path := "/track/click/" + data
	if sig != "" {
		params["sig"] = sig
		path += "/" + sig
	}
	req := withChiURLParam(httptest.NewRequest(http.MethodGet, path, nil), params)
	rec := httptest.NewRecorder()
	svc.HandleTrackClick(rec, req)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected DB calls (event written?): %v", err)
	}
	return rec
}

func delta(before map[string]int64, key string) int64 {
	return tracking.SigCountersSnapshot()[key] - before[key]
}

func TestAPITrackSig_ShadowNeverChangesBehavior(t *testing.T) {
	t.Setenv(tracking.SigModeEnv, "")
	t.Setenv(tracking.SigKeyEnv, "k1")
	data := sigClickData()
	before := tracking.SigCountersSnapshot()

	if rec := apiClick(t, "k1", data, signEnc("k1", data)); rec.Code != http.StatusFound || rec.Header().Get("Location") != sigTestDest {
		t.Fatalf("valid: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// Sig-less link: pre-existing behavior (accepted) unchanged in shadow.
	if rec := apiClick(t, "k1", data, ""); rec.Code != http.StatusFound {
		t.Fatalf("no_sig shadow: %d", rec.Code)
	}
	// Present-but-bad: pre-existing 403 unchanged in shadow.
	if rec := apiClick(t, "k1", data, "0000000000000000"); rec.Code != http.StatusForbidden {
		t.Fatalf("invalid shadow: %d", rec.Code)
	}
	if delta(before, "click.sig.valid") != 1 || delta(before, "click.sig.no_sig") != 1 || delta(before, "click.sig.invalid") != 1 {
		t.Fatalf("counters: %v", tracking.SigCountersSnapshot())
	}
}

func TestAPITrackSig_EnforceBlocksMatrix(t *testing.T) {
	t.Setenv(tracking.SigModeEnv, "enforce")
	t.Setenv(tracking.SigKeyEnv, "k1")
	data := sigClickData()
	sig := signEnc("k1", data)
	forged := encodeTrackingPayload(nilUUID, nilUUID, nilUUID, nilUUID, "https://attacker.example/")
	badSig := "f" + sig[1:]
	if badSig == sig {
		badSig = "e" + sig[1:]
	}
	before := tracking.SigCountersSnapshot()

	cases := map[string][2]string{
		"tampered data": {forged, sig},
		"tampered sig":  {data, badSig},
		"wrong key":     {data, signEnc("k2", data)},
		"sig-less":      {data, ""},
	}
	for name, c := range cases {
		rec := apiClick(t, "k1", c[0], c[1])
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Fatalf("%s: got %d loc=%q", name, rec.Code, rec.Header().Get("Location"))
		}
	}
	if delta(before, "click.enforce.blocked") != 4 || delta(before, "click.sig.no_sig") != 1 || delta(before, "click.sig.invalid") != 3 {
		t.Fatalf("counters: %v", tracking.SigCountersSnapshot())
	}
	if rec := apiClick(t, "k1", data, sig); rec.Code != http.StatusFound || rec.Header().Get("Location") != sigTestDest {
		t.Fatalf("valid under enforce: %d", rec.Code)
	}
}

func TestAPITrackSig_EnforceRotationAcceptsOldKey(t *testing.T) {
	t.Setenv(tracking.SigModeEnv, "enforce")
	t.Setenv(tracking.SigKeyEnv, "newkey,oldkey")
	data := sigClickData()
	for _, k := range []string{"newkey", "oldkey"} {
		// svc.signingKey (legacy single-key verifySig) is "newkey": the old-key
		// link passes only because enforce makes the shared verifier authoritative.
		if rec := apiClick(t, "newkey", data, signEnc(k, data)); rec.Code != http.StatusFound {
			t.Fatalf("key %s: %d", k, rec.Code)
		}
	}
}

func TestAPITrackSig_EnforceMissingKeyFailsOpen(t *testing.T) {
	t.Setenv(tracking.SigModeEnv, "enforce")
	t.Setenv(tracking.SigKeyEnv, "")
	before := tracking.SigCountersSnapshot()
	if rec := apiClick(t, "", sigClickData(), ""); rec.Code != http.StatusFound {
		t.Fatalf("missing key must fail open: %d", rec.Code)
	}
	if delta(before, "click.sig.missing_key") != 1 {
		t.Fatalf("counters: %v", tracking.SigCountersSnapshot())
	}
}

func TestAPITrackSig_OffModeNoCounting(t *testing.T) {
	t.Setenv(tracking.SigModeEnv, "off")
	t.Setenv(tracking.SigKeyEnv, "k1")
	before := tracking.SigCountersSnapshot()
	if rec := apiClick(t, "k1", sigClickData(), ""); rec.Code != http.StatusFound {
		t.Fatalf("off: %d", rec.Code)
	}
	if delta(before, "click.sig.no_sig") != 0 {
		t.Fatal("off mode counted")
	}
}

func TestAPITrackSig_OpenUnsubCountOnlyUnderEnforce(t *testing.T) {
	t.Setenv(tracking.SigModeEnv, "enforce")
	t.Setenv(tracking.SigKeyEnv, "k1")
	before := tracking.SigCountersSnapshot()

	// Open, sig-less + nil ids: pixel as before (no block, no DB).
	svc, mock, _ := newTrackingTestService(t)
	svc.signingKey = "k1"
	od := encodeTrackingPayload(nilUUID, nilUUID, nilUUID, nilUUID)
	rec := httptest.NewRecorder()
	svc.HandleTrackOpen(rec, withChiURLParam(httptest.NewRequest(http.MethodGet, "/track/open/"+od, nil), map[string]string{"data": od}))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/gif" {
		t.Fatalf("open: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// Unsub with a bad sig: the pre-existing 403 — enforce adds nothing.
	svc2, mock2, _ := newTrackingTestService(t)
	svc2.signingKey = "k1"
	ud := encodeTrackingPayload(nilUUID, nilUUID, nilUUID)
	rec2 := httptest.NewRecorder()
	svc2.HandleTrackUnsubscribe(rec2, withChiURLParam(httptest.NewRequest(http.MethodGet, "/track/unsubscribe/"+ud+"/0000000000000000", nil), map[string]string{"data": ud, "sig": "0000000000000000"}))
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("unsub: %d", rec2.Code)
	}
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if delta(before, "open.sig.no_sig") != 1 || delta(before, "unsub.sig.invalid") != 1 || delta(before, "click.enforce.blocked") != 0 {
		t.Fatalf("counters: %v", tracking.SigCountersSnapshot())
	}
}

func TestAPITrackSig_RejectLogHasNoTokenContents(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	t.Setenv(tracking.SigModeEnv, "enforce")
	t.Setenv(tracking.SigKeyEnv, "k1")
	data := sigClickData()
	apiClick(t, "k1", data, "0000000000000000")
	out := buf.String()
	for _, leak := range []string{data, data[:24], sigTestDest, "offer-dest.example", "SUBID-SECRET", "0000000000000000"} {
		if strings.Contains(out, leak) {
			t.Fatalf("log leaked %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, "TRACKSIG reject endpoint=click class=invalid tok="+tracking.TokenHashPrefix(data)) {
		t.Fatalf("missing reject line: %s", out)
	}
}

func TestAPIHealth_ExposesTrackSig(t *testing.T) {
	t.Setenv(tracking.SigKeyEnv, "k1")
	t.Setenv(tracking.SigModeEnv, "")
	apiClick(t, "k1", sigClickData(), "")
	rec := httptest.NewRecorder()
	NewHealthChecker(nil, nil, nil, "").HandleHealth(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `"track_sig"`) || !strings.Contains(body, `"click.sig.no_sig"`) || !strings.Contains(body, `"mode":"shadow"`) {
		t.Fatalf("health: %d %s", rec.Code, body)
	}
	if strings.Contains(body, `"k1"`) {
		t.Fatalf("health leaked key: %s", body)
	}
}
