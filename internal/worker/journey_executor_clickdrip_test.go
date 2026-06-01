package worker

// Phase 3 (Click-Drip Journey) — unit tests for the metadata override
// branches in executeEmailNode and the supporting pure-function helper
// readReminderSeqIndex.
//
// The override fan-in lives entirely in executeEmailNode:
//
//   - enrollment.Metadata["sending_profile_id"] (UUID string)
//     replaces any node.Config sending_profile_id so the reminder
//     ships from the same brand domain the subscriber clicked.
//
//   - enrollment.Metadata["body_html"] replaces node.Config htmlContent
//     so the reminder reuses the *exact* creative the subscriber
//     clicked (operator policy 2026-06-01: continuation, not re-pitch).
//
//   - enrollment.Metadata["original_from_name"] / ["original_from_email"]
//     fill fromName / fromEmail when node.Config left them blank, so
//     the reminder reads as a follow-up from the same friendly-from
//     used on the original send.
//
//   - node.Config["reminder_sequence_index"] keys into
//     mailing_offer_reminder_subjects by (everflow_offer_id,
//     sequence_index). When the row exists AND enabled=true, its
//     subject / preheader / from_name_override apply. When
//     enabled=false the override is silently skipped — operator
//     uses the enabled flag as a per-offer kill-switch.
//
// Test conventions mirror journey_event_enroller_test.go and
// journey_email_activator_test.go: sqlmock with the default
// substring-via-regexp matcher, regexp.QuoteMeta on a distinctive
// SQL substring per expectation, sql.ErrNoRows on the
// loadSubscriberByEmail call so the personalization branch is a
// no-op (Render() pass-through preserves the literal subject /
// htmlContent we want to assert on).

import (
	"context"
	"database/sql"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// TestReadReminderSeqIndex is a pure-function table test for the
// click-drip sequence-index extractor. The executor depends on it to
// key into mailing_offer_reminder_subjects per step; misclassifying
// the JSON-decoded float64 / string / int / int64 forms would cause
// silent off-by-one or skipped-overrides on every reminder send.
func TestReadReminderSeqIndex(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]interface{}
		wantI  int
		wantOK bool
	}{
		{
			name:   "nil config",
			config: nil,
			wantOK: false,
		},
		{
			name:   "empty config",
			config: map[string]interface{}{},
			wantOK: false,
		},
		{
			// JSON numbers decode to float64 — the dominant production
			// path. Zero must still be reported as ok=true so step 0
			// (the +1h reminder) gets its override.
			name:   "snake_case float64 zero is present",
			config: map[string]interface{}{"reminder_sequence_index": 0.0},
			wantI:  0,
			wantOK: true,
		},
		{
			name:   "snake_case float64 three",
			config: map[string]interface{}{"reminder_sequence_index": 3.0},
			wantI:  3,
			wantOK: true,
		},
		{
			// Operators sometimes hand-edit journey JSON; the camelCase
			// key is the second-priority lookup so old payloads still work.
			name:   "camelCase fallback",
			config: map[string]interface{}{"reminderSequenceIndex": 2.0},
			wantI:  2,
			wantOK: true,
		},
		{
			// String form covers config produced by the journey builder
			// UI when a user types into a number-bound input.
			name:   "string numeric",
			config: map[string]interface{}{"reminder_sequence_index": "2"},
			wantI:  2,
			wantOK: true,
		},
		{
			// Empty string is the "no value" case — must NOT be
			// interpreted as 0 because that would trigger the wrong
			// reminder row.
			name:   "string empty skips",
			config: map[string]interface{}{"reminder_sequence_index": ""},
			wantOK: false,
		},
		{
			// Non-numeric string is operator garbage; better to skip
			// the override than silently ship reminder 0.
			name:   "string non-numeric skips",
			config: map[string]interface{}{"reminder_sequence_index": "abc"},
			wantOK: false,
		},
		{
			// In-process callers (tests, future programmatic graph
			// builders) may pass int / int64 directly without a JSON
			// roundtrip.
			name:   "int direct",
			config: map[string]interface{}{"reminder_sequence_index": int(5)},
			wantI:  5,
			wantOK: true,
		},
		{
			name:   "int64 direct",
			config: map[string]interface{}{"reminder_sequence_index": int64(7)},
			wantI:  7,
			wantOK: true,
		},
		{
			name:   "unrelated key is ignored",
			config: map[string]interface{}{"other_key": 0.0},
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotI, gotOK := readReminderSeqIndex(tc.config)
			require.Equal(t, tc.wantOK, gotOK, "ok flag")
			if tc.wantOK {
				require.Equal(t, tc.wantI, gotI, "extracted index")
			}
		})
	}
}

// emailSendCapture records the args the executor's emailSender was
// invoked with so the test can assert exactly which value won the
// override fan-in. Tests run single-threaded so no synchronization is
// needed.
type emailSendCapture struct {
	called      bool
	email       string
	subject     string
	htmlContent string
	fromName    string
	fromEmail   string
}

func (c *emailSendCapture) sender(_ context.Context, email, subject, htmlContent, fromName, fromEmail string) error {
	c.called = true
	c.email = email
	c.subject = subject
	c.htmlContent = htmlContent
	c.fromName = fromName
	c.fromEmail = fromEmail
	return nil
}

// TestEmailNode_ClickDripMetadataOverrides_BodyAndProfileAndIdentity is
// the green-light Phase 3 path: an enrollment created by the
// click-postback pipeline carries everything needed to make the
// reminder feel like a continuation of the original click. The test
// proves all four override branches fired:
//
//  1. metadata.sending_profile_id triggered the mailing_sending_profiles
//     lookup (we mock it); profile from_name does NOT win because
//     metadata.original_from_name already populated fromName.
//  2. metadata.body_html replaced node.Config htmlContent.
//  3. node.Config.reminder_sequence_index + metadata.everflow_offer_id
//     keyed into mailing_offer_reminder_subjects, and the enabled=true
//     row's subject came through.
//  4. The captured emailSender args are exactly the post-override values.
func TestEmailNode_ClickDripMetadataOverrides_BodyAndProfileAndIdentity(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	je := NewJourneyExecutor(db)
	cap := &emailSendCapture{}
	je.SetEmailSender(cap.sender)

	const profileUUID = "00000000-0000-0000-0000-000000000001"

	// 1) sending profile lookup — metadata.sending_profile_id wins over
	//    node.Config (which doesn't set it). Profile values would only
	//    backfill from_name / from_email when those are still empty;
	//    here metadata.original_from_name beats us to it so the
	//    profile's "BrandProfile" / "noreply@brandprofile.com" do NOT
	//    end up in the captured args.
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_sending_profiles WHERE id = $1`)).
		WithArgs(profileUUID).
		WillReturnRows(sqlmock.NewRows([]string{"from_name", "from_email"}).
			AddRow("BrandProfile", "noreply@brandprofile.com"))

	// 2) reminder-subject lookup keyed on (offer, seq=0). Returning
	//    enabled=true with a non-empty subject is what proves the
	//    override fired.
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_reminder_subjects`)).
		WithArgs("9539", 0).
		WillReturnRows(sqlmock.NewRows([]string{"subject", "preheader", "from_name_override", "enabled"}).
			AddRow("REM SUBJECT", "REM PRE", "", true))

	// 3) subscriber load — ErrNoRows so the templating branch is a
	//    no-op and the literal subject / htmlContent reach the sender
	//    untouched. We don't care about personalization in this test.
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_subscribers`)).
		WithArgs("user@example.com").
		WillReturnError(sql.ErrNoRows)

	enrollment := Enrollment{
		ID:              "e1",
		JourneyID:       "j1",
		SubscriberEmail: "user@example.com",
		CurrentNodeID:   "n1",
		Status:          "active",
		Metadata: map[string]interface{}{
			"sending_profile_id":  profileUUID,
			"body_html":           "<p>OVERRIDE</p>",
			"original_from_name":  "BrandX",
			"original_from_email": "noreply@brandx.com",
			"everflow_offer_id":   "9539",
		},
	}
	node := &JourneyNodeExec{
		ID:   "n1",
		Type: "email",
		Config: map[string]interface{}{
			// JSON unmarshalling produces float64 for numbers — match
			// the production path.
			"reminder_sequence_index": 0.0,
		},
	}

	result, err := je.executeEmailNode(context.Background(), enrollment, node)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "continue", result.Action)

	require.True(t, cap.called, "emailSender should have been invoked")
	require.Equal(t, "user@example.com", cap.email)
	require.Equal(t, "REM SUBJECT", cap.subject, "subject should come from mailing_offer_reminder_subjects row")
	require.Equal(t, "<p>OVERRIDE</p>", cap.htmlContent, "htmlContent should come from metadata.body_html")
	require.Equal(t, "BrandX", cap.fromName, "fromName should come from metadata.original_from_name (set before profile lookup)")
	require.Equal(t, "noreply@brandx.com", cap.fromEmail, "fromEmail should come from metadata.original_from_email")

	// Side-effect assertion: the reminder preheader is mirrored back
	// into node.Config so downstream renderers (and the activator's
	// shadow-campaign metadata) see it.
	require.Equal(t, "REM PRE", node.Config["preheader"])

	require.NoError(t, mock.ExpectationsWereMet())
}

// TestEmailNode_NoMetadataOverrides_PreservesNodeConfig is the
// regression-guard for the legacy (non-click-drip) journey path: an
// enrollment with empty metadata + a fully populated node.Config
// must round-trip the config values to emailSender unchanged. Any of
// the Phase 3 metadata branches reaching for an unset key would
// either crash (nil map index) or silently zero one of these
// assertions.
func TestEmailNode_NoMetadataOverrides_PreservesNodeConfig(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	je := NewJourneyExecutor(db)
	cap := &emailSendCapture{}
	je.SetEmailSender(cap.sender)

	// Only DB call expected: subscriber load. No sending profile, no
	// reminder subjects, no template lookup — config doesn't reference
	// any of them and metadata is empty.
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_subscribers`)).
		WithArgs("user@example.com").
		WillReturnError(sql.ErrNoRows)

	enrollment := Enrollment{
		ID:              "e1",
		JourneyID:       "j1",
		SubscriberEmail: "user@example.com",
		CurrentNodeID:   "n1",
		Status:          "active",
		Metadata:        map[string]interface{}{},
	}
	node := &JourneyNodeExec{
		ID:   "n1",
		Type: "email",
		Config: map[string]interface{}{
			"subject":      "orig",
			"html_content": "<p>orig</p>", // snake_case fallback path
			"fromName":     "orig",
			"fromEmail":    "o@o.com",
		},
	}

	result, err := je.executeEmailNode(context.Background(), enrollment, node)
	require.NoError(t, err)
	require.Equal(t, "continue", result.Action)

	require.True(t, cap.called)
	require.Equal(t, "orig", cap.subject)
	require.Equal(t, "<p>orig</p>", cap.htmlContent)
	require.Equal(t, "orig", cap.fromName)
	require.Equal(t, "o@o.com", cap.fromEmail)

	require.NoError(t, mock.ExpectationsWereMet())
}

// TestEmailNode_OfferReminderDisabled_DoesNotOverrideSubject covers the
// per-offer kill switch: even when reminder_sequence_index is set AND
// metadata.everflow_offer_id is present AND the row exists,
// enabled=false must stop the override. The original node.Config
// subject must reach the sender unchanged so an operator can park a
// reminder step without redeploying the journey graph.
func TestEmailNode_OfferReminderDisabled_DoesNotOverrideSubject(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	je := NewJourneyExecutor(db)
	cap := &emailSendCapture{}
	je.SetEmailSender(cap.sender)

	// reminder-subject row exists but enabled=false. Subject /
	// preheader / from_name_override are populated to prove the worker
	// is reading the enabled flag and NOT just "any non-null subject
	// wins".
	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_offer_reminder_subjects`)).
		WithArgs("9539", 0).
		WillReturnRows(sqlmock.NewRows([]string{"subject", "preheader", "from_name_override", "enabled"}).
			AddRow("DISABLED SUBJECT", "DISABLED PRE", "DISABLED FROM", false))

	mock.ExpectQuery(regexp.QuoteMeta(`FROM mailing_subscribers`)).
		WithArgs("user@example.com").
		WillReturnError(sql.ErrNoRows)

	enrollment := Enrollment{
		ID:              "e1",
		JourneyID:       "j1",
		SubscriberEmail: "user@example.com",
		CurrentNodeID:   "n1",
		Status:          "active",
		Metadata: map[string]interface{}{
			// only everflow_offer_id — no profile, no body, no identity
			// overrides — so the only Phase 3 lookup that fires is the
			// reminder-subject one.
			"everflow_offer_id": "9539",
		},
	}
	node := &JourneyNodeExec{
		ID:   "n1",
		Type: "email",
		Config: map[string]interface{}{
			"subject":                 "orig",
			"html_content":            "<p>body</p>",
			"from_name":               "orig",
			"from_email":              "o@o.com",
			"reminder_sequence_index": 0.0,
		},
	}

	result, err := je.executeEmailNode(context.Background(), enrollment, node)
	require.NoError(t, err)
	require.Equal(t, "continue", result.Action)

	require.True(t, cap.called)
	require.Equal(t, "orig", cap.subject, "disabled reminder row must NOT override subject")
	require.Equal(t, "<p>body</p>", cap.htmlContent, "htmlContent should be untouched (no body_html metadata)")
	require.Equal(t, "orig", cap.fromName, "from_name_override on a disabled row must NOT win")
	require.Equal(t, "o@o.com", cap.fromEmail)

	// preheader must NOT be back-filled into node.Config when the row
	// is disabled.
	_, hasPreheader := node.Config["preheader"]
	require.False(t, hasPreheader, "preheader should not be set on disabled reminder row")

	require.NoError(t, mock.ExpectationsWereMet())
}
