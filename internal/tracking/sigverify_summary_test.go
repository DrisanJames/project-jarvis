package tracking

import (
	"strings"
	"testing"
)

func TestSigSummary_TopUnknownHosts_HostsOnly(t *testing.T) {
	resetSigCounters(t)
	t.Setenv(SigModeEnv, "")
	h := NewHandler(&capturePublisher{}, realShapeDict(t))
	h.sigKeys = ParseSigKeys("k1")
	dests := []string{
		"https://fonts.googleapis.com/css2?family=Inter", "https://fonts.googleapis.com/css2?family=Roboto", "https://fonts.googleapis.com/x",
		"https://engine076.com/c/SECRETPATH?sub1=" + sigSub, "https://ENGINE076.com/c/2",
		"https://a.example/p", "https://b.example/p", "https://c.example/p", "https://d.example/p",
		"https://www.discountblog.com/blog/x", // owned — must not appear
	}
	for _, d := range dests {
		enc, sig := liveClickToken("k1", d)
		doClick(h, enc, sig)
	}
	line := sigCounters.flush(sigSummaryEvery, sigModeShadow, 1)
	want := "unknown_top[fonts.googleapis.com=3 engine076.com=2 a.example=1 b.example=1 c.example=1]"
	if !strings.Contains(line, want) {
		t.Fatalf("summary missing %q:\n%s", want, line)
	}
	for _, leak := range []string{"/css2", "family=", "SECRETPATH", "sub1", sigSub, "?", "discountblog", "d.example"} {
		if strings.Contains(line, leak) {
			t.Fatalf("summary leaked %q:\n%s", leak, line)
		}
	}
	// Window resets.
	if next := sigCounters.flush(sigSummaryEvery, sigModeShadow, 1); !strings.Contains(next, "unknown_top[]") {
		t.Fatalf("unknown hosts not reset: %s", next)
	}
}

func TestSigSummary_UnknownHostsBounded(t *testing.T) {
	resetSigCounters(t)
	sigCounters.incUnknownHost("evil host/../x")
	for i := 0; i < unknownHostCap+50; i++ {
		sigCounters.incUnknownHost("h" + strings.Repeat("x", i%7) + "-" + string(rune('a'+i%26)) + ".example" + strings.Repeat("z", i/26))
	}
	sigCounters.mu.Lock()
	n := len(sigCounters.unknownHosts)
	inv := sigCounters.unknownHosts["_invalid"]
	ovf := sigCounters.unknownHosts["_overflow"]
	sigCounters.mu.Unlock()
	if n > unknownHostCap+1 || inv != 1 || ovf == 0 {
		t.Fatalf("bounds: distinct=%d invalid=%d overflow=%d", n, inv, ovf)
	}
}
