package api

import (
	"reflect"
	"testing"
)

// TestBuildLaneScopes — profiles → lanes: box = injector host; the shared
// 'ses-relay' VMTA follows the ses-relay-a pool's box; unknown hosts fall in no
// box; colo1 is the rest of kumo (NOT IN colo2's codes).
func TestBuildLaneScopes(t *testing.T) {
	sc := buildLaneScopes([]laneProfileRow{
		{host: "15.204.101.125", poolPrefix: "db", ipPool: ""},
		{host: "15.204.101.125", poolPrefix: "qfsi", ipPool: ""},
		{host: "15.204.101.125", poolPrefix: "", ipPool: "ses-relay-a"},
		{host: "15.204.107.107", poolPrefix: "ht", ipPool: ""},
		{host: "40.160.129.116", poolPrefix: "bcc", ipPool: ""},
		{host: "40.160.129.116", poolPrefix: "Bad Prefix", ipPool: ""},
		{host: "15.204.105.55", poolPrefix: "ufo", ipPool: ""},
		{host: "10.0.0.9", poolPrefix: "zz", ipPool: ""},
	})
	if got := sc["pmta_a"].Prefixes; !reflect.DeepEqual(got, []string{"db", "qfsi", "relay"}) {
		t.Fatalf("pmta_a prefixes = %v", got)
	}
	if got := sc["pmta_b"].Prefixes; !reflect.DeepEqual(got, []string{"ht"}) {
		t.Fatalf("pmta_b prefixes = %v", got)
	}
	// colo1 is the REST of kumo (laneRest): no own prefixes, NOT IN colo2's codes.
	if sc["colo1"].Prefixes != nil || !reflect.DeepEqual(sc["colo1"].Exclude, []string{"ufo"}) {
		t.Fatalf("colo1 = kumo minus colo2, got prefixes %v exclude %v", sc["colo1"].Prefixes, sc["colo1"].Exclude)
	}
	if got := sc["colo2"].Prefixes; !reflect.DeepEqual(got, []string{"ufo"}) {
		t.Fatalf("colo2 prefixes = %v", got)
	}
	if got := sc["pmta_b"].Exclude; got != nil {
		t.Fatalf("pmta_b is profile-defined, got exclude %v", got)
	}
	for _, k := range []string{"ses", "mta", "kumo"} {
		if sc[k].Prefixes != nil {
			t.Fatalf("%s is a whole-source lane, got prefixes %v", k, sc[k].Prefixes)
		}
	}
	if sc["pmta_a"].Source != "pmta" || sc["colo1"].Source != "kumo" || sc["ses"].Source != "ses" {
		t.Fatalf("lane sources wrong: %+v", sc)
	}
}

func TestLaneHostOf(t *testing.T) {
	if h := laneHostOf("http://15.204.105.55:19099", ""); h != "15.204.105.55" {
		t.Fatalf("api_endpoint host = %q", h)
	}
	if h := laneHostOf("", "40.160.129.116"); h != "40.160.129.116" {
		t.Fatalf("smtp_host fallback = %q", h)
	}
}
