package tracking

// Offline signature-compatibility tally for real /track/* links (used by
// cmd/tracking-sigcheck). Output is COUNTS ONLY, keyed by (host, kind, scheme,
// result) — never a token, subscriber id or destination.

import (
	"bufio"
	"io"
	"net/url"
	"strings"
)

// SigTallyKey is one output bucket.
type SigTallyKey struct {
	Host   string // lowercased tracking host ("" if none)
	Kind   string // click | open | unsubscribe | other
	Scheme string // enc | raw | - (line not a /track/ link)
	Result string // valid | invalid | no_sig | undecodable | missing_key | unparseable
}

// TallyTrackingURLs reads newline-delimited URLs and, for each /track/ link,
// evaluates BOTH signing schemes independently (so per-scheme compatibility is
// visible even though the service only accepts raw on /track/open). The path
// segments are taken from the ESCAPED path — the same bytes chi hands the
// service — with no re-encoding or padding normalization.
func TallyTrackingURLs(r io.Reader, keys [][]byte) (map[SigTallyKey]int, int, error) {
	out := map[SigTallyKey]int{}
	lines := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		lines++
		host, kind, data, sig, ok := splitTrackURL(line)
		if !ok {
			out[SigTallyKey{Host: host, Kind: "other", Scheme: "-", Result: "unparseable"}]++
			continue
		}
		for _, scheme := range []string{SigFormatEncoded, SigFormatRaw} {
			res := VerifyTrackingToken(data, sig, keys, []string{scheme})
			out[SigTallyKey{Host: host, Kind: kind, Scheme: scheme, Result: res.Class}]++
		}
	}
	return out, lines, sc.Err()
}

// splitTrackURL extracts host, kind, data and sig from
// <scheme>://<host>/track/<kind>/<data>[/<sig>].
func splitTrackURL(raw string) (host, kind, data, sig string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", "", false
	}
	host = strings.ToLower(u.Hostname())
	segs := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	for i, s := range segs {
		if s != "track" {
			continue
		}
		if i+2 >= len(segs) { // need kind and data
			return host, "", "", "", false
		}
		kind = segs[i+1]
		if kind != "click" && kind != "open" && kind != "unsubscribe" {
			return host, "", "", "", false
		}
		data = segs[i+2]
		if i+3 < len(segs) {
			sig = segs[i+3]
		}
		return host, kind, data, sig, data != ""
	}
	return host, "", "", "", false
}
