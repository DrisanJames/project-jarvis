package main

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/tracking"
)

func TestRun_PrintsCountsOnly(t *testing.T) {
	key := []byte("k1")
	raw := "org|camp|SUB-SECRET-ID|email|https://dest.example/SECRETPATH"
	enc := base64.URLEncoding.EncodeToString([]byte(raw))
	sig := tracking.SignTrackingMsg(key, []byte(enc))
	in := "https://t.em.discountblog.com/track/click/" + enc + "/" + sig + "\n"

	var out bytes.Buffer
	if err := run(strings.NewReader(in), &out, [][]byte{key}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"lines=1", "t.em.discountblog.com\tclick\tenc\tvalid\t1", "t.em.discountblog.com\tclick\traw\tinvalid\t1"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
	for _, leak := range []string{enc, sig, "SUB-SECRET-ID", "SECRETPATH", "dest.example", "k1"} {
		if strings.Contains(s, leak) {
			t.Fatalf("output leaked %q:\n%s", leak, s)
		}
	}
}
