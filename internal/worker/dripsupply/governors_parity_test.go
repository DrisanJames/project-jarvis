package dripsupply_test

// Parity gates for the two orchestrator behaviours bucket.go DUPLICATES.
//
// The duplication is forced: internal/worker imports this package (WP5), so a
// production import in the other direction is a cycle. An EXTERNAL test package
// can import both sides without creating one — the same device WP2 used for
// TestISPClassesMatchOrchestratorDefaults. If either of these fails, the
// orchestrator moved and bucket.go must follow it.

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/worker"
	"github.com/ignite/sparkpost-monitor/internal/worker/dripsupply"
)

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestGmailAllowListMatchesOrchestrator pins dripsupply.GmailAllowedBrands to
// worker.DefaultNewRecordISPBrandAllow()["gmail"] — the same default AND the
// same env parse, including the whitespace/case handling.
func TestGmailAllowListMatchesOrchestrator(t *testing.T) {
	cases := []struct {
		name string
		env  string
	}{
		{"default (mature-4)", ""},
		{"operator override", "db,ht,mh,qf,bw"},
		{"messy whitespace and case", " DB , ht ,, MH ,qf, "},
		{"single brand", "ht"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PARTNER_DRIP_GMAIL_NEW_BRANDS", tc.env)
			want := keys(worker.DefaultNewRecordISPBrandAllow()["gmail"])
			got := keys(dripsupply.GmailAllowedBrands())
			if !eq(got, want) {
				t.Fatalf("GmailAllowedBrands() = %v, orchestrator = %v — the duplicated env parse has drifted", got, want)
			}
			if len(want) == 0 {
				t.Fatal("the orchestrator's gmail allow-list is empty; the parity assertion above proves nothing")
			}
		})
	}
}

// TestGmailAllowListVocabularyIsUnambiguous is the trap guard.
//
// The platform carries TWO brand-code vocabularies: brandident (bw, hw, rr, tt,
// yi, mr, lp, wf — what mailing_isp_bans is keyed by, and what
// DefaultBrandForDomain returns) and the orchestrator's longer codes (bwp, hws,
// rru, tot, yih, mrd, lpl, wfy). The gmail allow-list happens to contain only
// db/ht/mh/qf, which are IDENTICAL in both — which is the only reason a single
// resolver can serve the ban check and the allow-list check.
//
// If the default ever grows a code that differs between the vocabularies, this
// fails: the resolver would then have to be vocabulary-aware, and silently is not.
func TestGmailAllowListVocabularyIsUnambiguous(t *testing.T) {
	t.Setenv("PARTNER_DRIP_GMAIL_NEW_BRANDS", "")
	divergent := map[string]string{
		"bwp": "bw", "hws": "hw", "rru": "rr", "tot": "tt",
		"yih": "yi", "mrd": "mr", "lpl": "lp", "wfy": "wf",
	}
	for code := range dripsupply.GmailAllowedBrands() {
		if short, bad := divergent[code]; bad {
			t.Fatalf("the gmail allow-list default now contains %q, which brandident calls %q — DefaultBrandForDomain returns the brandident code, so this brand would be silently held at 0. Make the resolver vocabulary-aware before shipping this list.", code, short)
		}
	}
}

// TestSESRouteAllMatchesOrchestrator pins dripsupply.SESRouteAll to
// worker.dripRouteAllSES (partner_drip_orchestrator.go:1323).
//
// That function is UNEXPORTED and has no test hook, and adding one would mean
// editing a file another builder is in — so the parity is pinned against its
// SOURCE: the env key and the exact set of off-values. A behavioural assertion
// would be better; this is the strongest guard available without touching the
// orchestrator, and it fails loudly the moment either side moves.
func TestSESRouteAllMatchesOrchestrator(t *testing.T) {
	raw, err := os.ReadFile("../partner_drip_orchestrator.go")
	if err != nil {
		t.Skipf("cannot read partner_drip_orchestrator.go: %v", err)
	}
	src := string(raw)
	i := strings.Index(src, "func dripRouteAllSES() bool {")
	if i < 0 {
		t.Fatal("dripRouteAllSES is gone from the orchestrator — SESRouteAll is mirroring a function that no longer exists")
	}
	body := src[i:]
	if j := strings.Index(body, "\n}"); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, dripsupply.SESRouteAllEnv) {
		t.Fatalf("the orchestrator no longer reads %s:\n%s", dripsupply.SESRouteAllEnv, body)
	}
	// The off-values my copy honours must be exactly the ones it honours.
	for _, off := range []string{`"false"`, `"0"`, `"off"`, `"no"`} {
		if !strings.Contains(body, off) {
			t.Fatalf("the orchestrator no longer treats %s as OFF:\n%s", off, body)
		}
	}
	if !strings.Contains(body, "return true") {
		t.Fatalf("the orchestrator's route-all default is no longer ON:\n%s", body)
	}

	// And my copy behaves the way that source says.
	for _, v := range []string{"false", "0", "off", "no", "FALSE", " Off "} {
		t.Setenv(dripsupply.SESRouteAllEnv, v)
		if dripsupply.SESRouteAll() {
			t.Fatalf("SESRouteAll() is true for the off-value %q", v)
		}
	}
	for _, v := range []string{"", "true", "1", "anything"} {
		t.Setenv(dripsupply.SESRouteAllEnv, v)
		if !dripsupply.SESRouteAll() {
			t.Fatalf("SESRouteAll() is false for %q, but the default is ON", v)
		}
	}
}

// TestSESPinsAreNotTheRoutedSet records WHY SESRoutedISP does not mirror
// `sesPins`. dripBrandISPSESProfiles is a brand×ISP → tenant-profile MAP whose
// default is the single entry ht=microsoft; it is layered on top of the
// route-all default, not a set of SES-routed ISPs. Mirroring it would have
// governed exactly one cell.
func TestSESPinsAreNotTheRoutedSet(t *testing.T) {
	raw, err := os.ReadFile("../partner_drip_orchestrator.go")
	if err != nil {
		t.Skipf("cannot read partner_drip_orchestrator.go: %v", err)
	}
	src := string(raw)
	i := strings.Index(src, "func dripBrandISPSESProfiles() map[string]map[string]string {")
	if i < 0 {
		t.Skip("dripBrandISPSESProfiles has moved; re-derive the SES-routed set")
	}
	if !strings.Contains(src[i:i+400], `"ht=microsoft=`) {
		t.Fatalf("the sesPins default is no longer the single ht=microsoft entry — re-check whether SESRoutedISP should now mirror it")
	}
}
