package worker

// Unit tests for the click-drip routing decision (isClickDripEnrollment)
// and the delay-acceleration test knob (scaleJourneyDelay).
//
// These are the two pure-ish branches added when the JourneyExecutor and
// JourneyEventEnroller were wired in-process (2026-06-01). The send itself
// (JourneyClickDripSender.Send → PMTA) is verified end-to-end in production
// per the quality-gate rule; here we lock down the decision logic that
// decides WHETHER to take the PMTA path and HOW LONG to wait.

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestIsClickDripEnrollment(t *testing.T) {
	tests := []struct {
		name string
		meta map[string]interface{}
		want bool
	}{
		{
			name: "nil metadata is not click-drip",
			meta: nil,
			want: false,
		},
		{
			name: "empty metadata is not click-drip",
			meta: map[string]interface{}{},
			want: false,
		},
		{
			name: "enrolled_via=click_postback wins",
			meta: map[string]interface{}{"enrolled_via": "click_postback"},
			want: true,
		},
		{
			name: "source=click_postback wins",
			meta: map[string]interface{}{"source": "click_postback"},
			want: true,
		},
		{
			name: "sending_profile_id presence is sufficient",
			meta: map[string]interface{}{"sending_profile_id": "eeeeeeee-1111-2222-3333-444444444444"},
			want: true,
		},
		{
			name: "empty sending_profile_id does not count",
			meta: map[string]interface{}{"sending_profile_id": ""},
			want: false,
		},
		{
			name: "segment-triggered (Welcome) enrollment is NOT click-drip",
			meta: map[string]interface{}{"enrolled_via": "segment", "segment_id": "abc"},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isClickDripEnrollment(Enrollment{Metadata: tc.meta})
			require.Equal(t, tc.want, got)
		})
	}
}

func TestScaleJourneyDelay(t *testing.T) {
	const oneHour = time.Hour

	t.Run("unset env returns delay unchanged", func(t *testing.T) {
		os.Unsetenv("JOURNEY_DELAY_SCALE")
		require.Equal(t, oneHour, scaleJourneyDelay(oneHour))
	})

	t.Run("invalid env returns delay unchanged", func(t *testing.T) {
		t.Setenv("JOURNEY_DELAY_SCALE", "not-a-number")
		require.Equal(t, oneHour, scaleJourneyDelay(oneHour))
	})

	t.Run("zero or negative scale returns delay unchanged", func(t *testing.T) {
		t.Setenv("JOURNEY_DELAY_SCALE", "0")
		require.Equal(t, oneHour, scaleJourneyDelay(oneHour))
		t.Setenv("JOURNEY_DELAY_SCALE", "-1")
		require.Equal(t, oneHour, scaleJourneyDelay(oneHour))
	})

	t.Run("scale shrinks the delay", func(t *testing.T) {
		// 0.5 of 1h = 30m, comfortably above the 5s floor.
		t.Setenv("JOURNEY_DELAY_SCALE", "0.5")
		require.Equal(t, 30*time.Minute, scaleJourneyDelay(oneHour))
	})

	t.Run("aggressive scale is floored at 5s", func(t *testing.T) {
		// 0.000001 of 1h = 3.6ms → clamped to the 5s floor so the
		// executor's wait scheduling never collapses to zero.
		t.Setenv("JOURNEY_DELAY_SCALE", "0.000001")
		require.Equal(t, 5*time.Second, scaleJourneyDelay(oneHour))
	})

	t.Run("scale up is honored", func(t *testing.T) {
		t.Setenv("JOURNEY_DELAY_SCALE", "2")
		require.Equal(t, 2*oneHour, scaleJourneyDelay(oneHour))
	})
}

// TestShadowCampaignID locks the deterministic shadow-campaign id contract:
// the same offer always maps to the same id (so ensureShadowCampaign can do a
// primary-key lookup instead of a name seq scan), different offers map to
// different ids, and the value is a valid UUID. Changing the namespace or the
// derivation string would orphan every existing shadow campaign, so this test
// guards against an accidental change.
func TestShadowCampaignID(t *testing.T) {
	a1 := shadowCampaignID("9539", "", "")
	a2 := shadowCampaignID("9539", "", "")
	b := shadowCampaignID("7667", "", "")

	require.Equal(t, a1, a2, "same offer must yield a stable id")
	require.NotEqual(t, a1, b, "different offers must yield different ids")
	require.Len(t, a1, 36, "must be a canonical UUID string")
	// Pin the exact value so a namespace/derivation change is caught. This is
	// ALSO the back-compat guarantee for the 2026-08-01 per-node split: an empty
	// node must still reproduce the original per-offer id byte-for-byte, or every
	// unsubscribe token minted before the split stops resolving.
	require.Equal(t, "55f62e3e-dccc-5181-812c-c5459661d5ef", a1)

	// Per-node contract: each (offer, node) gets its own stable id, distinct
	// from every other node and from the legacy per-offer id. This is what makes
	// per-node opens/clicks attributable at all — collapsing them back onto one
	// campaign is the bug this guards.
	n0a := shadowCampaignID("9539", "email-0", "")
	n0b := shadowCampaignID("9539", "email-0", "")
	n1 := shadowCampaignID("9539", "email-1", "")
	require.Equal(t, n0a, n0b, "same (offer,node) must yield a stable id")
	require.NotEqual(t, n0a, n1, "different nodes of one offer must yield different ids")
	require.NotEqual(t, n0a, a1, "node-scoped id must differ from the legacy per-offer id")
	require.NotEqual(t, n0a, shadowCampaignID("7667", "email-0", ""), "same node under different offers must differ")
	require.Len(t, n0a, 36, "must be a canonical UUID string")
}

func TestReplaceMoneyMergeTags(t *testing.T) {
	html := `<a href="https://www.eos57ytf.com/K4C5ZLC/PS8241/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}">x</a>` +
		`<a href="https://x.com/?s=%7B%7Bsubscriber.id%7D%7D">y</a>` +
		`<span>{{ subscriber.id }}</span>`
	out := replaceMoneyMergeTags(html, "abc-123", "deals@em.myownhealth.net")
	for _, bad := range []string{"{{subscriber.id}}", "{{ subscriber.id }}", "{{brand.domain}}", "%7B%7Bsubscriber.id%7D%7D"} {
		if strings.Contains(out, bad) {
			t.Fatalf("unrendered tag %q survived: %s", bad, out)
		}
	}
	if !strings.Contains(out, "sub1=abc-123") || !strings.Contains(out, "sub2=myownhealth.net") {
		t.Fatalf("substitution wrong: %s", out)
	}
	// em. prefix stripping must not mangle apexes that merely start with 'm'
	out2 := replaceMoneyMergeTags(`{{brand.domain}}`, "x", "a@em.myrepairdiy.com")
	if out2 != "myrepairdiy.com" {
		t.Fatalf("brand derivation wrong: %s", out2)
	}
}

func TestBrandRootFromEmail(t *testing.T) {
	tests := []struct{ in, want string }{
		{"deals@em.discountblog.com", "discountblog.com"},
		{"a@em.myrepairdiy.com", "myrepairdiy.com"}, // em.-strip must not eat a leading 'm'
		{"x@discountblog.com", "discountblog.com"},  // no em. prefix
		{"not-an-email", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := brandRootFromEmail(tc.in); got != tc.want {
			t.Fatalf("brandRootFromEmail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRenderSystemURLTokens_SafetyNet locks the send-time half of the
// {{ system.* }} fix: any literal system URL token still in the creative
// (the executor's Liquid render is skipped when the subscriber row fails to
// load) must be replaced with a real signed URL BEFORE the HTML reaches the
// PMTA API sender's quoted-printable encoder — QP soft line-breaks split a
// token mid-name ('{{ system.preferences_ur=') and PMTA's template parse of
// 'content' then 422s the whole touch. Also locks the CAN-SPAM fallback: a
// body with no unsubscribe link gets one injected, and a body that already
// has one is not double-injected.
func TestRenderSystemURLTokens_SafetyNet(t *testing.T) {
	s := NewJourneyClickDripSender(nil, nil, "https://t.em.discountblog.com", "test-secret")
	const (
		orgID     = "00000000-0000-0000-0000-000000000001"
		campID    = "55f62e3e-dccc-5181-812c-c5459661d5ef"
		subID     = "abc-123"
		brandRoot = "discountblog.com"
		trackBase = "https://t.em.discountblog.com"
	)

	t.Run("all token variants replaced with signed URLs", func(t *testing.T) {
		html := `<body><p><a href="{{ system.brand_unsubscribe_url }}">brand</a>` +
			` <a href="{{system.brand_unsubscribe_url}}">brand2</a>` +
			` <a href="{{ system.unsubscribe_url }}">unsubscribe globally here</a>` +
			` <a href="{{system.unsubscribe_url}}">global2</a>` +
			` or <a href="{{ system.preferences_url }}">preferences</a>` +
			` <a href="{{system.preferences_url}}">prefs2</a></p></body>`
		out := s.renderSystemURLTokens(html, orgID, campID, subID, brandRoot, trackBase)

		if strings.Contains(out, "{{") {
			t.Fatalf("raw template token survived (would 422 at PMTA after QP encoding): %s", out)
		}
		if !strings.Contains(out, trackBase+"/track/unsubscribe/") {
			t.Fatalf("unsubscribe URL missing — the reminder must ship a working unsub link: %s", out)
		}
		if !strings.Contains(out, trackBase+"/preferences?sid="+subID) {
			t.Fatalf("preferences URL missing: %s", out)
		}
		// Parity check: the global unsub token must render to the exact URL
		// the broadcast send worker's safety net would produce.
		want := GenerateUnsubscribeURL(orgID, campID, subID, trackBase, "test-secret")
		if !strings.Contains(out, want) {
			t.Fatalf("global unsub URL diverges from broadcast generator:\nwant substr %s\nin %s", want, out)
		}
		// No unsub links were missing, so the fallback block must NOT appear:
		// exactly the 4 replaced tokens (2 brand + 2 global), no 5th injected.
		if strings.Count(out, "/track/unsubscribe/") != 4 {
			t.Fatalf("expected exactly the 4 replaced unsub links (no fallback injection), got: %s", out)
		}
	})

	// Regression (2026-08-04, ccba9ec): the enumerated URL map above let any
	// OTHER token — the footer's {{ system.current_year }} — reach the QP
	// encoder, whose soft line-breaks split it mid-name ("&copy; {{ sys=") and
	// PMTA's template parse 422'd the whole message (~1-2% of touches lost).
	// Known scalars must render; anything else must be stripped outright.
	t.Run("current_year renders and residual mustache is swept", func(t *testing.T) {
		html := `<body><p>&copy; {{ system.current_year }} and {{system.current_year}}</p>` +
			`<p>unknown {{ some.unknown_token }} here</p>` +
			`<a href="{{ system.unsubscribe_url }}">unsubscribe</a></body>`
		out := s.renderSystemURLTokens(html, orgID, campID, subID, brandRoot, trackBase)

		year := strconv.Itoa(time.Now().Year())
		if strings.Count(out, "&copy; "+year+" and "+year) != 1 {
			t.Fatalf("current_year token variants not rendered to %s: %s", year, out)
		}
		if strings.Contains(out, "{{") || strings.Contains(out, "}}") {
			t.Fatalf("residual mustache survived the sweep (would 422 at PMTA after QP encoding): %s", out)
		}
		if !strings.Contains(out, "unknown  here") {
			t.Fatalf("unknown token should be stripped, leaving surrounding text intact: %s", out)
		}
	})

	t.Run("body without unsub link gets CAN-SPAM fallback injected", func(t *testing.T) {
		out := s.renderSystemURLTokens(`<body><p>deal reminder</p></body>`, orgID, campID, subID, brandRoot, trackBase)
		if !strings.Contains(out, "/track/unsubscribe/") {
			t.Fatalf("fallback unsubscribe block not injected: %s", out)
		}
		if !strings.Contains(out, "</div></body>") {
			t.Fatalf("fallback block should be injected before </body>: %s", out)
		}
	})
}

func TestApplyISPBrandRouting(t *testing.T) {
	po := &PartnerDripOrchestrator{cfg: PartnerDripOrchestratorConfig{
		NewRecordISPBrandAllow: map[string]map[string]bool{
			"gmail": {"db": true, "ht": true, "mh": true, "qf": true},
			"yahoo": {"db": true, "ht": true, "mh": true, "qf": true},
			"apple": {"db": true, "ht": true, "mh": true, "qf": true},
			"att":   {"db": true, "ht": true, "mh": true, "qf": true},
		},
	}}
	caps := map[string]int{"gmail": 500, "yahoo": 300, "apple": 1000, "att": 200, "microsoft": 800, "comcast": 100}
	// non-mature brand: hard ISPs zeroed, others untouched
	got := po.applyISPBrandRouting("lpl", caps)
	for _, isp := range []string{"gmail", "yahoo", "apple", "att"} {
		if got[isp] != 0 {
			t.Fatalf("lpl: %s should be 0, got %d", isp, got[isp])
		}
	}
	if got["microsoft"] != 800 || got["comcast"] != 100 {
		t.Fatalf("lpl: non-routed ISPs altered: %v", got)
	}
	// mature brand: all caps preserved
	got2 := po.applyISPBrandRouting("qf", caps)
	for isp, want := range caps {
		if got2[isp] != want {
			t.Fatalf("qf: %s want %d got %d", isp, want, got2[isp])
		}
	}
}

// TestEnsureShadowCampaign_StampsNodeAttribution proves the 2026-08-01 node
// attribution actually reaches the INSERT. Before this, click-drip created ONE
// campaign per offer and stamped no node identity at all: NodeID was a declared
// field on ClickDripSendParams that nothing ever wrote, mailing_campaigns'
// journey_node_id was NULL on all 228k rows, and /journeys/{id}/node-stats had
// nothing to group by. A column that is written but never read (or declared but
// never written) is this codebase's most common defect, so assert the exact
// values bound to the INSERT rather than merely that it ran.
func TestEnsureShadowCampaign_StampsNodeAttribution(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	const (
		orgID     = "00000000-0000-0000-0000-000000000001"
		offerID   = "6137"
		nodeID    = "email-2"
		profileID = "44444444-4444-4444-4444-444444444444"
	)

	s := NewJourneyClickDripSender(db, nil, "", "")

	// Miss the fast path so the INSERT branch runs.
	wantID := shadowCampaignID(offerID, nodeID, "")
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id::text FROM mailing_campaigns WHERE id=$1`)).
		WithArgs(wantID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM mailing_offers WHERE everflow_offer_id=$1`)).
		WithArgs(offerID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	// The four attribution stamps, in INSERT order: journey_key (the VARCHAR
	// journey id the UUID journey_id column cannot hold), journey_node_id,
	// journey_offer_id (the lane scope the screen groups by — all lanes share
	// one journey), journey_wave_index (reminder sequence).
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_campaigns`)).
		WithArgs(
			wantID, orgID, "Click-Drip Reminder · offer 6137 · email-2",
			"Still interested?", "Diane", "deals@em.discountblog.com",
			sqlmock.AnyArg(), sqlmock.AnyArg(),
			"click-drip-4touch-72h", nodeID, offerID, int32(2),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	got, err := s.ensureShadowCampaign(context.Background(), orgID, ClickDripSendParams{
		JourneyID:       "click-drip-4touch-72h",
		NodeID:          nodeID,
		ReminderSeq:     2,
		EverflowOfferID: offerID,
		Subject:         "Still interested?",
		FromName:        "Diane",
		FromEmail:       "deals@em.discountblog.com",
		ProfileID:       profileID,
	})
	require.NoError(t, err)
	require.Equal(t, wantID, got, "must return the (offer,node)-scoped id, not the per-offer one")
	require.NotEqual(t, shadowCampaignID(offerID, "", ""), got)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestEnsureShadowCampaign_NoNodeKeepsLegacyShape guards the back-compat half:
// a send with no node context must still resolve the ORIGINAL per-offer id and
// stamp no node columns, so pre-split rows and their unsubscribe tokens are
// untouched.
func TestEnsureShadowCampaign_NoNodeKeepsLegacyShape(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	const (
		orgID   = "00000000-0000-0000-0000-000000000001"
		offerID = "6137"
	)

	s := NewJourneyClickDripSender(db, nil, "", "")
	wantID := shadowCampaignID(offerID, "", "")

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id::text FROM mailing_campaigns WHERE id=$1`)).
		WithArgs(wantID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM mailing_offers WHERE everflow_offer_id=$1`)).
		WithArgs(offerID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_campaigns`)).
		WithArgs(
			wantID, orgID, "Click-Drip Reminder · offer 6137",
			"", "", "",
			sqlmock.AnyArg(), sqlmock.AnyArg(),
			nil, nil, offerID, nil,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	got, err := s.ensureShadowCampaign(context.Background(), orgID, ClickDripSendParams{
		EverflowOfferID: offerID,
	})
	require.NoError(t, err)
	require.Equal(t, wantID, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestEnsureShadowCampaign_DegradesWhenAttributionColumnsMissing is the
// regression guard for the 2026-08-02 incident: the node-attribution columns
// ship via DDL that can lag the binary (the migration runner's 5s budget
// includes an ACCESS EXCLUSIVE lock wait on a hot mailing_campaigns), and when
// they were absent EVERY click-drip reminder failed with
// `column "journey_key" ... does not exist`.
//
// Attribution is reporting metadata. A missing column must degrade to the
// un-stamped INSERT and still SEND — never fail the touch.
func TestEnsureShadowCampaign_DegradesWhenAttributionColumnsMissing(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	const (
		orgID   = "00000000-0000-0000-0000-000000000001"
		offerID = "6137"
		nodeID  = "email-2"
	)
	s := NewJourneyClickDripSender(db, nil, "", "")
	wantID := shadowCampaignID(offerID, nodeID, "")

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id::text FROM mailing_campaigns WHERE id=$1`)).
		WithArgs(wantID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM mailing_offers WHERE everflow_offer_id=$1`)).
		WithArgs(offerID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	// The stamped INSERT fails exactly as production did...
	mock.ExpectExec(regexp.QuoteMeta(`journey_key, journey_node_id, journey_offer_id, journey_wave_index`)).
		WillReturnError(errors.New(`pq: column "journey_key" of relation "mailing_campaigns" does not exist`))

	// ...and the sender must immediately retry WITHOUT those columns.
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_campaigns`)).
		WithArgs(wantID, orgID, "Click-Drip Reminder · offer 6137 · email-2",
			"Still interested?", "Diane", "deals@em.discountblog.com",
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	params := ClickDripSendParams{
		JourneyID:       "click-drip-4touch-72h",
		NodeID:          nodeID,
		ReminderSeq:     2,
		EverflowOfferID: offerID,
		Subject:         "Still interested?",
		FromName:        "Diane",
		FromEmail:       "deals@em.discountblog.com",
	}
	got, err := s.ensureShadowCampaign(context.Background(), orgID, params)
	require.NoError(t, err, "a missing attribution column must NOT fail the send")
	require.Equal(t, wantID, got)
	require.NoError(t, mock.ExpectationsWereMet())

	// The fallback latches: the next send skips the stamped attempt entirely
	// rather than paying a failed INSERT every time.
	require.True(t, s.stampsDisabled(), "fallback must latch after a missing-column error")

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id::text FROM mailing_campaigns WHERE id=$1`)).
		WithArgs(wantID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM mailing_offers WHERE everflow_offer_id=$1`)).
		WithArgs(offerID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO mailing_campaigns`)).
		WithArgs(wantID, orgID, "Click-Drip Reminder · offer 6137 · email-2",
			"Still interested?", "Diane", "deals@em.discountblog.com",
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	_, err = s.ensureShadowCampaign(context.Background(), orgID, params)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	// And it SELF-HEALS: once the recheck window elapses the stamped path is
	// tried again, so attribution resumes without a restart.
	s.mu.Lock()
	s.stampLastProbe = time.Now().Add(-2 * stampRecheckAfter)
	s.mu.Unlock()
	require.False(t, s.stampsDisabled(), "must re-probe after the recheck window")
}

// TestIsMissingColumnErr keeps the detector from swallowing unrelated failures —
// a broad match here would hide real send errors behind a silent fallback.
func TestIsMissingColumnErr(t *testing.T) {
	require.True(t, isMissingColumnErr(errors.New(`pq: column "journey_key" of relation "mailing_campaigns" does not exist`)))
	require.True(t, isMissingColumnErr(errors.New(`pq: column "journey_offer_id" does not exist`)))
	require.False(t, isMissingColumnErr(nil))
	require.False(t, isMissingColumnErr(errors.New("pq: canceling statement due to statement timeout")))
	require.False(t, isMissingColumnErr(errors.New("connection refused")))
	require.False(t, isMissingColumnErr(errors.New(`pq: relation "mailing_offers" does not exist`)),
		"a missing TABLE is a real failure, not an attribution degrade")
}

// TestTouchContentHash_VersionsOnAnyCopyChange pins the operator's versioning
// rule: a touch's metrics belong to the exact creative + subject that earned
// them, so changing ANY part must mint a new version (which sunsets the old
// one's numbers), while a pure reformat must NOT.
func TestTouchContentHash_VersionsOnAnyCopyChange(t *testing.T) {
	base := touchContentHash("Subject A", "Pre A", "From A", "<p>Body A</p>")

	require.Equal(t, base, touchContentHash("Subject A", "Pre A", "From A", "<p>Body A</p>"),
		"identical copy must be the same version")

	// Every field independently versions.
	require.NotEqual(t, base, touchContentHash("Subject B", "Pre A", "From A", "<p>Body A</p>"), "subject change")
	require.NotEqual(t, base, touchContentHash("Subject A", "Pre B", "From A", "<p>Body A</p>"), "preheader change")
	require.NotEqual(t, base, touchContentHash("Subject A", "Pre A", "From B", "<p>Body A</p>"), "from-name change")
	require.NotEqual(t, base, touchContentHash("Subject A", "Pre A", "From A", "<p>Body B</p>"), "body change")

	// Whitespace/reformatting is NOT a copy change — otherwise every save would
	// pointlessly sunset the metrics.
	require.Equal(t, base, touchContentHash("Subject A ", " Pre A", "From A", "<p>Body A</p>\n"),
		"whitespace-only differences must not mint a version")
	require.Equal(t, base, touchContentHash("Subject   A", "Pre A", "From A", "<p>Body  A</p>"),
		"collapsed internal whitespace must not mint a version")

	// Field boundaries must not be ambiguous: moving text across fields changes
	// the version rather than colliding.
	require.NotEqual(t,
		touchContentHash("AB", "", "", ""),
		touchContentHash("A", "B", "", ""),
		"field boundaries must be unambiguous in the hash")

	// And the hash must actually key a distinct campaign.
	require.NotEqual(t,
		shadowCampaignID("420", "email-0", base),
		shadowCampaignID("420", "email-0", touchContentHash("Subject B", "Pre A", "From A", "<p>Body A</p>")),
		"a new version must land on its own campaign so its metrics separate")
	require.NotEqual(t, shadowCampaignID("420", "email-0", base), shadowCampaignID("420", "email-0", ""),
		"versioned id must differ from the unversioned per-node id")
}
