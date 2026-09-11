package tracking

import (
	"bytes"
	"context"
	"encoding/base64"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const (
	sigSub  = "33333333-SUBSCRIBER-SECRET-3333"
	sigDest = "https://evil-dest.example/SECRETPATH?x=1"
)

// liveClickToken mints a click link the way the LIVE generator does
// (worker.RewriteClickLinks: TrackSign over the base64url-ENCODED string).
func liveClickToken(key, dest string) (encoded, sig string) {
	raw := "00000000-0000-0000-0000-000000000001|" + campUUID + "|" + sigSub + "|44444444-4444-4444-4444-444444444444|" + dest
	encoded = base64.URLEncoding.EncodeToString([]byte(raw))
	return encoded, SignTrackingMsg([]byte(key), []byte(encoded))
}

func resetSigCounters(t *testing.T) {
	t.Helper()
	sigCounters = newSigStats()
}

func sigHandler(pub eventPublisher, keys string) *Handler {
	h := NewHandler(pub, nil)
	h.sigKeys = ParseSigKeys(keys)
	return h
}

func doClick(h *Handler, encoded, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/track/click/"+encoded+"/"+sig, nil)
	rr := httptest.NewRecorder()
	h.Routes().ServeHTTP(rr, req)
	return rr
}

// --- pure verifier ---------------------------------------------------------

func TestVerify_ValidEncodedScheme(t *testing.T) {
	enc, sig := liveClickToken("k1", sigDest)
	if r := VerifyTrackingToken(enc, sig, ParseSigKeys("k1"), sigFormatsEncOnly); r.Class != SigValid || r.Format != SigFormatEncoded {
		t.Fatalf("got %+v", r)
	}
}

func TestVerify_TamperedDataFails(t *testing.T) {
	enc, sig := liveClickToken("k1", sigDest)
	forged, _ := liveClickToken("k1", "https://attacker.example/")
	if forged == enc {
		t.Fatal("test setup")
	}
	if r := VerifyTrackingToken(forged, sig, ParseSigKeys("k1"), sigFormatsEncOnly); r.Class != SigInvalid {
		t.Fatalf("tampered data: got %+v", r)
	}
}

func TestVerify_TamperedSigFails(t *testing.T) {
	enc, sig := liveClickToken("k1", sigDest)
	bad := []byte(sig)
	if bad[0] == 'a' {
		bad[0] = 'b'
	} else {
		bad[0] = 'a'
	}
	if r := VerifyTrackingToken(enc, string(bad), ParseSigKeys("k1"), sigFormatsEncOnly); r.Class != SigInvalid {
		t.Fatalf("tampered sig: got %+v", r)
	}
	if r := VerifyTrackingToken(enc, sig+"00", ParseSigKeys("k1"), sigFormatsEncOnly); r.Class != SigInvalid {
		t.Fatalf("over-long sig: got %+v", r)
	}
}

func TestVerify_WrongKeyFails(t *testing.T) {
	enc, sig := liveClickToken("k1", sigDest)
	if r := VerifyTrackingToken(enc, sig, ParseSigKeys("k2"), sigFormatsEncOnly); r.Class != SigInvalid {
		t.Fatalf("wrong key: got %+v", r)
	}
}

func TestVerify_RotationListAcceptsOldAndNew(t *testing.T) {
	keys := ParseSigKeys("newkey, oldkey")
	for _, k := range []string{"newkey", "oldkey"} {
		enc, sig := liveClickToken(k, sigDest)
		if r := VerifyTrackingToken(enc, sig, keys, sigFormatsEncOnly); r.Class != SigValid {
			t.Fatalf("key %s: got %+v", k, r)
		}
	}
	enc, sig := liveClickToken("retired", sigDest)
	if r := VerifyTrackingToken(enc, sig, keys, sigFormatsEncOnly); r.Class != SigInvalid {
		t.Fatalf("retired key accepted: %+v", r)
	}
	// A key that itself contains a comma still verifies (raw value tried first).
	enc, sig = liveClickToken("a,b", sigDest)
	if r := VerifyTrackingToken(enc, sig, ParseSigKeys("a,b"), sigFormatsEncOnly); r.Class != SigValid {
		t.Fatalf("comma key: got %+v", r)
	}
}

func TestVerify_RawSchemeOnlyForOpen(t *testing.T) {
	raw := "o|c|s|e"
	enc := base64.URLEncoding.EncodeToString([]byte(raw))
	sig := SignTrackingMsg([]byte("k1"), []byte(raw)) // API transactional-pixel shape
	if r := VerifyTrackingToken(enc, sig, ParseSigKeys("k1"), sigFormatsEncOnly); r.Class != SigInvalid {
		t.Fatalf("raw must not verify on click/unsub: %+v", r)
	}
	if r := VerifyTrackingToken(enc, sig, ParseSigKeys("k1"), sigFormatsOpen); r.Class != SigValid || r.Format != SigFormatRaw {
		t.Fatalf("raw must verify on open: %+v", r)
	}
}

func TestVerify_MissingKeyUndecodableNoSig(t *testing.T) {
	enc, sig := liveClickToken("k1", sigDest)
	if r := VerifyTrackingToken(enc, sig, nil, sigFormatsEncOnly); r.Class != SigMissingKey {
		t.Fatalf("got %+v", r)
	}
	if r := VerifyTrackingToken("!!notb64", sig, ParseSigKeys("k1"), sigFormatsEncOnly); r.Class != SigUndecodable {
		t.Fatalf("got %+v", r)
	}
	if r := VerifyTrackingToken(enc, "", ParseSigKeys("k1"), sigFormatsEncOnly); r.Class != SigNoSig {
		t.Fatalf("got %+v", r)
	}
}

// --- handler: shadow ------------------------------------------------------

func TestClick_ShadowNeverAltersRedirect(t *testing.T) {
	resetSigCounters(t)
	t.Setenv(SigModeEnv, "") // default = shadow
	pub := &capturePublisher{}
	h := sigHandler(pub, "k1")

	enc, sig := liveClickToken("k1", sigDest)
	forged, _ := liveClickToken("k1", "https://attacker.example/")
	for _, c := range []struct{ enc, sig string }{{enc, sig}, {forged, sig}, {enc, "0000000000000000"}} {
		rr := doClick(h, c.enc, c.sig)
		if rr.Code != http.StatusTemporaryRedirect || rr.Header().Get("Location") == "" {
			t.Fatalf("shadow changed response: %d loc=%q", rr.Code, rr.Header().Get("Location"))
		}
	}
	if len(pub.events) != 3 {
		t.Fatalf("shadow must publish every click, got %d", len(pub.events))
	}
	c := sigCounters.snapshot()
	if c["click.sig.valid"] != 1 || c["click.sig.invalid"] != 2 || c["click.fmt.enc"] != 1 {
		t.Fatalf("counters: %v", c)
	}
	if c["click.dest.unknown"] != 3 {
		t.Fatalf("dest counters: %v", c)
	}
}

func TestClick_ShadowMissingKeyCounted(t *testing.T) {
	resetSigCounters(t)
	h := sigHandler(&capturePublisher{}, "")
	enc, sig := liveClickToken("k1", sigDest)
	if rr := doClick(h, enc, sig); rr.Code != http.StatusTemporaryRedirect {
		t.Fatalf("got %d", rr.Code)
	}
	if sigCounters.snapshot()["click.sig.missing_key"] != 1 {
		t.Fatalf("counters: %v", sigCounters.snapshot())
	}
}

func TestClick_OffModeNoCounters(t *testing.T) {
	resetSigCounters(t)
	t.Setenv(SigModeEnv, "off")
	h := sigHandler(&capturePublisher{}, "k1")
	enc, _ := liveClickToken("k1", sigDest)
	if rr := doClick(h, enc, "0000000000000000"); rr.Code != http.StatusTemporaryRedirect {
		t.Fatalf("got %d", rr.Code)
	}
	if n := len(sigCounters.snapshot()); n != 0 {
		t.Fatalf("off mode counted: %v", sigCounters.snapshot())
	}
}

// --- handler: enforce -----------------------------------------------------

func TestClick_EnforceBlocksWithoutEvent(t *testing.T) {
	resetSigCounters(t)
	t.Setenv(SigModeEnv, "enforce")
	pub := &capturePublisher{}
	h := sigHandler(pub, "k1")

	enc, sig := liveClickToken("k1", sigDest)
	forged, _ := liveClickToken("k1", "https://attacker.example/")
	for _, c := range []struct{ enc, sig string }{{forged, sig}, {enc, "0000000000000000"}} {
		rr := doClick(h, c.enc, c.sig)
		if rr.Code != http.StatusBadRequest || rr.Header().Get("Location") != "" {
			t.Fatalf("enforce: got %d loc=%q", rr.Code, rr.Header().Get("Location"))
		}
	}
	if len(pub.events) != 0 {
		t.Fatalf("enforce published %d events for invalid links", len(pub.events))
	}
	if sigCounters.snapshot()["click.enforce.blocked"] != 2 {
		t.Fatalf("counters: %v", sigCounters.snapshot())
	}
	// Valid link still redirects and publishes.
	rr := doClick(h, enc, sig)
	if rr.Code != http.StatusTemporaryRedirect || rr.Header().Get("Location") != sigDest || len(pub.events) != 1 {
		t.Fatalf("valid under enforce: %d loc=%q events=%d", rr.Code, rr.Header().Get("Location"), len(pub.events))
	}
}

func TestClick_EnforceMissingKeyFailsOpen(t *testing.T) {
	resetSigCounters(t)
	t.Setenv(SigModeEnv, "enforce")
	pub := &capturePublisher{}
	h := sigHandler(pub, "")
	enc, sig := liveClickToken("k1", sigDest)
	if rr := doClick(h, enc, sig); rr.Code != http.StatusTemporaryRedirect || len(pub.events) != 1 {
		t.Fatalf("missing key must fail open: %d events=%d", rr.Code, len(pub.events))
	}
}

func TestOpenUnsub_EnforceNeverBlocks(t *testing.T) {
	resetSigCounters(t)
	t.Setenv(SigModeEnv, "enforce")
	pub := &capturePublisher{}
	h := sigHandler(pub, "k1")
	enc := base64.URLEncoding.EncodeToString([]byte("o|" + campUUID + "|" + subUUID + "|e"))
	for _, p := range []string{"/track/open/" + enc + "/0000000000000000", "/track/unsubscribe/" + enc + "/0000000000000000"} {
		rr := httptest.NewRecorder()
		h.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: got %d", p, rr.Code)
		}
	}
	if len(pub.events) != 2 {
		t.Fatalf("open/unsub events: %d", len(pub.events))
	}
	c := sigCounters.snapshot()
	if c["open.sig.invalid"] != 1 || c["unsub.sig.invalid"] != 1 {
		t.Fatalf("counters: %v", c)
	}
}

// --- destination classifier -----------------------------------------------

func TestClassifyDestination(t *testing.T) {
	d := &SmartLinkDictionary{}
	d.loadFn = func(context.Context) (map[string]smartLinkEntry, error) {
		return map[string]smartLinkEntry{"abc123": {Destination: "https://www.cratoolpro.com/X/Y?sub1={{subscriber.id}}"}}, nil
	}
	if err := d.reloadOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"https://www.discountblog.com/article":   DestOwned,
		"https://t.em.quizfiesta.com/x":          DestOwned,
		"https://cratoolpro.com/A/B?source_id=1": DestMoney,
		"https://www.eos57ytf.com/K4C5ZLC/":      DestMoney, // dead-link remap target
		"https://evil.example/":                  DestUnknown,
		"ftp://discountblog.com/":                DestBadScheme,
		"javascript:alert(1)":                    DestUnparseable,
	}
	for in, want := range cases {
		if got := classifyDestination(in, d); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}

// --- logs + health --------------------------------------------------------

func TestSigLogs_NoTokenContents(t *testing.T) {
	resetSigCounters(t)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	t.Setenv(SigModeEnv, "enforce")
	h := sigHandler(&capturePublisher{}, "k1")
	enc, _ := liveClickToken("k1", sigDest)
	doClick(h, enc, "0000000000000000")
	log.Print(sigCounters.flush(sigSummaryEvery, currentSigMode(), len(h.sigKeys)))

	out := buf.String()
	for _, leak := range []string{enc, enc[:24], sigSub, sigDest, "evil-dest.example", "SECRETPATH", "k1,", "0000000000000000"} {
		if strings.Contains(out, leak) {
			t.Fatalf("log leaked %q:\n%s", leak, out)
		}
	}
	for _, want := range []string{"TRACKSIG reject endpoint=click class=invalid tok=" + tokenHashPrefix(enc), "TRACKSIG SUMMARY", "click.sig.invalid=1", "mode=enforce"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q:\n%s", want, out)
		}
	}
}

func TestHealth_ExposesSigCounters(t *testing.T) {
	resetSigCounters(t)
	h := sigHandler(&capturePublisher{}, "k1")
	enc, sig := liveClickToken("k1", sigDest)
	doClick(h, enc, sig)
	rr := httptest.NewRecorder()
	h.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	body := rr.Body.String()
	if rr.Code != 200 || !strings.Contains(body, `"status":"ok"`) || !strings.Contains(body, `"click.sig.valid":1`) || !strings.Contains(body, `"keys":1`) {
		t.Fatalf("health: %d %s", rr.Code, body)
	}
	if strings.Contains(body, "k1") {
		t.Fatalf("health leaked key: %s", body)
	}
}

// --- offline tally --------------------------------------------------------

func TestTallyTrackingURLs_BothSchemes(t *testing.T) {
	enc, sig := liveClickToken("k1", sigDest)
	raw := "o|c|s|e"
	rawEnc := base64.URLEncoding.EncodeToString([]byte(raw))
	rawSig := SignTrackingMsg([]byte("k1"), []byte(raw))
	in := strings.Join([]string{
		"https://t.em.discountblog.com/track/click/" + enc + "/" + sig,
		"https://T.EM.discountblog.com/track/click/" + enc + "/ffffffffffffffff",
		"https://trk.em.quizfiesta.com/track/open/" + rawEnc + "/" + rawSig,
		"https://t.em.discountblog.com/o/sub/hash/camp",
		"",
	}, "\n")
	got, lines, err := TallyTrackingURLs(strings.NewReader(in), ParseSigKeys("k1"))
	if err != nil || lines != 4 {
		t.Fatalf("lines=%d err=%v", lines, err)
	}
	want := map[SigTallyKey]int{
		{"t.em.discountblog.com", "click", "enc", "valid"}:     1,
		{"t.em.discountblog.com", "click", "enc", "invalid"}:   1,
		{"t.em.discountblog.com", "click", "raw", "invalid"}:   2,
		{"trk.em.quizfiesta.com", "open", "raw", "valid"}:      1,
		{"trk.em.quizfiesta.com", "open", "enc", "invalid"}:    1,
		{"t.em.discountblog.com", "other", "-", "unparseable"}: 1,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%+v: got %d want %d (all=%v)", k, got[k], v, got)
		}
	}
}
