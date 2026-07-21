package worker

import (
	"strings"
	"testing"
)

// Pins the 2026-07-21 fix: click-drip journey reminders shipped with ONLY the
// X-Journey-* diagnostic headers — no RFC 8058 List-Unsubscribe at all —
// while Google Postmaster flagged the sending domains "Not compliant" for
// missing one-click unsubscribe. buildClickDripHeaders is the exact function
// Send() uses to assemble msg.Headers, so this test pins the production path.
func TestBuildClickDripHeaders_RFC8058(t *testing.T) {
	p := ClickDripSendParams{
		JourneyID:       "click-drip-4touch-72h",
		NodeID:          "email-2",
		EverflowOfferID: "1234",
		ReminderSeq:     2,
		SubscriberID:    "33333333-3333-3333-3333-333333333333",
		SubscriberEmail: "human@yahoo.com",
		FromName:        "Diane @ Consumer Pro",
		FromEmail:       "diane@em.consumerpro.net",
		ProfileID:       "profile-1",
	}

	headers := buildClickDripHeaders(
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		p,
		"https://trk.em.consumerpro.net",
		"test-secret",
	)

	// Diagnostics preserved.
	if headers["X-Journey-ID"] != "click-drip-4touch-72h" ||
		headers["X-Click-Drip-Offer"] != "1234" ||
		headers["X-Click-Drip-Step"] != "2" {
		t.Errorf("X-Journey diagnostic headers wrong: %v", headers)
	}

	lu := headers["List-Unsubscribe"]
	if lu == "" {
		t.Fatalf("click-drip reminder missing List-Unsubscribe (the exact 2026-07-21 regression); headers=%v", headers)
	}
	if !strings.Contains(lu, "<mailto:unsub+") || !strings.Contains(lu, "@em.consumerpro.net?subject=unsubscribe>") {
		t.Errorf("List-Unsubscribe missing From-domain-aligned mailto leg: %s", lu)
	}
	if !strings.Contains(lu, "https://trk.em.consumerpro.net/track/unsubscribe/") {
		t.Errorf("List-Unsubscribe missing brand https one-click leg: %s", lu)
	}
	if headers["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
		t.Errorf("List-Unsubscribe-Post = %q", headers["List-Unsubscribe-Post"])
	}
}

// Without a tracking base there is no https one-click URL to advertise, so the
// pair is (correctly) absent — parity with send_worker's trackBase guard.
func TestBuildClickDripHeaders_NoTrackBase(t *testing.T) {
	headers := buildClickDripHeaders("org", "camp", ClickDripSendParams{
		JourneyID: "j", EverflowOfferID: "o", ReminderSeq: 1,
		SubscriberID: "s", FromEmail: "a@em.x.com",
	}, "", "secret")
	if _, ok := headers["List-Unsubscribe"]; ok {
		t.Error("List-Unsubscribe should not be set without a tracking base URL")
	}
}
