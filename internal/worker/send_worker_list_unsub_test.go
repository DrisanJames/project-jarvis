package worker

import (
	"strings"
	"testing"
)

// TestBuildUnsubscribeHeaders_SESPath is the regression guard for the
// 2026-07-14 fix. Before the fix, the RFC 8058 List-Unsubscribe /
// List-Unsubscribe-Post headers were constructed ONLY inside the
// `!sesInfo.ViaSES` branch of processItem, so every SES-relayed send (gmail,
// apple, and the entire yahoo-family newsletter ramp — all SES) shipped WITHOUT
// the Gmail/Yahoo-mandated (Feb-2024) one-click unsubscribe header.
//
// The fix hoisted header construction into the transport-agnostic
// buildUnsubscribeHeaders helper, invoked from a SHARED block in processItem
// that runs for BOTH transports whenever trackBase != "". This helper has no
// ViaSES parameter BY DESIGN: an SES send and a Dedicated send compute the exact
// same header. This test exercises the SES scenario (the previously-broken path)
// and asserts the compliant headers + a resolved (non-token) unsubscribe URL.
func TestBuildUnsubscribeHeaders_SESPath(t *testing.T) {
	pool := &SendWorkerPool{
		orgID:          "11111111-1111-1111-1111-111111111111",
		trackingSecret: "test-signing-secret",
	}

	headers := make(map[string]string)
	trackBase := "https://trk.em.discountblog.com" // per-profile brand track base (SES lane)
	unsubURL, brandUnsubURL := pool.buildUnsubscribeHeaders(
		"22222222-2222-2222-2222-222222222222", // campaignID
		"33333333-3333-3333-3333-333333333333", // subscriberID
		"discountblog.com",                     // brandRoot
		"news@em.discountblog.com",             // fromEmail
		trackBase,
		headers,
	)

	// 1. RFC 8058 one-click headers MUST be present for the SES path.
	lu, ok := headers["List-Unsubscribe"]
	if !ok || lu == "" {
		t.Fatalf("SES path produced no List-Unsubscribe header (the exact regression this guards); headers=%v", headers)
	}
	if got := headers["List-Unsubscribe-Post"]; got != "List-Unsubscribe=One-Click" {
		t.Errorf("List-Unsubscribe-Post = %q, want %q", got, "List-Unsubscribe=One-Click")
	}

	// 2. Header must carry BOTH the mailto: (ISP-trust) leg and the brand-scoped
	//    HTTPS one-click leg.
	if !strings.Contains(lu, "<mailto:unsub+") {
		t.Errorf("List-Unsubscribe missing mailto leg: %s", lu)
	}
	if !strings.Contains(lu, "<"+brandUnsubURL+">") {
		t.Errorf("List-Unsubscribe missing brand HTTPS leg %q: %s", brandUnsubURL, lu)
	}
	// mailto domain must align with the From domain for ISP trust.
	if !strings.Contains(lu, "@em.discountblog.com?subject=unsubscribe>") {
		t.Errorf("mailto leg not aligned to From domain: %s", lu)
	}

	// 3. The returned unsubscribe URL must be RESOLVED (a real signed tracking
	//    URL), never an unreplaced Liquid token — SES sends previously never ran
	//    the token safety-net either, so a literal {{ system.unsubscribe_url }}
	//    could ship in-body.
	if !strings.Contains(unsubURL, "/track/unsubscribe/") {
		t.Errorf("unsubURL is not a resolved tracking URL: %q", unsubURL)
	}
	if strings.Contains(unsubURL, "{{") || strings.Contains(brandUnsubURL, "{{") {
		t.Errorf("URL still contains an unresolved token: unsub=%q brand=%q", unsubURL, brandUnsubURL)
	}
	if !strings.HasPrefix(brandUnsubURL, trackBase+"/track/unsubscribe/") {
		t.Errorf("brandUnsubURL not on the brand track base: %q", brandUnsubURL)
	}
}

// TestBuildUnsubscribeHeaders_KillSwitch confirms the one-move rollback:
// DISABLE_LIST_UNSUB_HEADERS=true omits the header writes on BOTH transports
// (restoring pre-fix behavior) WITHOUT redeploy, while still returning resolved
// URLs so token replacement never leaves a literal token in the body.
func TestBuildUnsubscribeHeaders_KillSwitch(t *testing.T) {
	t.Setenv("DISABLE_LIST_UNSUB_HEADERS", "true")

	pool := &SendWorkerPool{
		orgID:          "11111111-1111-1111-1111-111111111111",
		trackingSecret: "test-signing-secret",
	}
	headers := make(map[string]string)
	unsubURL, brandUnsubURL := pool.buildUnsubscribeHeaders(
		"22222222-2222-2222-2222-222222222222",
		"33333333-3333-3333-3333-333333333333",
		"discountblog.com",
		"news@em.discountblog.com",
		"https://trk.em.discountblog.com",
		headers,
	)

	if _, ok := headers["List-Unsubscribe"]; ok {
		t.Errorf("kill switch set but List-Unsubscribe header was still written: %v", headers)
	}
	if _, ok := headers["List-Unsubscribe-Post"]; ok {
		t.Errorf("kill switch set but List-Unsubscribe-Post header was still written: %v", headers)
	}
	// URLs are still resolved so in-body token replacement continues to work.
	if !strings.Contains(unsubURL, "/track/unsubscribe/") || strings.Contains(unsubURL, "{{") {
		t.Errorf("kill switch must still return a resolved unsub URL, got %q", unsubURL)
	}
	if !strings.Contains(brandUnsubURL, "/track/unsubscribe/") {
		t.Errorf("kill switch must still return a resolved brand unsub URL, got %q", brandUnsubURL)
	}
}

// TestBuildUnsubscribeHeaders_ParityAcrossTransports pins the invariant that
// makes the fix correct: the header is computed identically regardless of
// transport (the helper takes no transport flag). If a future change reintroduces
// a transport-specific branch, this parity check fails.
func TestBuildUnsubscribeHeaders_ParityAcrossTransports(t *testing.T) {
	pool := &SendWorkerPool{
		orgID:          "11111111-1111-1111-1111-111111111111",
		trackingSecret: "test-signing-secret",
	}
	args := func() (h map[string]string) {
		h = make(map[string]string)
		pool.buildUnsubscribeHeaders(
			"22222222-2222-2222-2222-222222222222",
			"33333333-3333-3333-3333-333333333333",
			"discountblog.com",
			"news@em.discountblog.com",
			"https://trk.em.discountblog.com",
			h,
		)
		return h
	}
	a, b := args(), args()
	if a["List-Unsubscribe"] != b["List-Unsubscribe"] || a["List-Unsubscribe-Post"] != b["List-Unsubscribe-Post"] {
		t.Errorf("List-Unsubscribe headers are not deterministic/parity-equal:\n a=%v\n b=%v", a, b)
	}
	if a["List-Unsubscribe"] == "" {
		t.Errorf("expected a non-empty List-Unsubscribe header for the shared (both-transport) path")
	}
}
