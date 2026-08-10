package worker

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// rawLiquidCreative is the exact shape of the creative that shipped as raw
// source on 2026-08-10: an unterminated Liquid if-tag inside an HTML comment.
// Liquid tokenizes tags even inside comments, so the whole parse aborts.
const rawLiquidCreative = `<html><body>
<!-- editor note: {% if subscriber.first_name %} greet by name -->
<p>Hello {{ first_name }}</p>
<a href="https://trk.em.discountblog.com/track/unsubscribe/abc/sig">Unsubscribe</a>
</body></html>`

// TestRender_ReturnsRawTemplateOnParseError pins the fail-open CONTRACT of
// TemplateService.Render that this whole guard exists to contain. If this ever
// starts returning "" instead of the original template, the guards below are
// still correct but the blast radius of missing one changes — so pin it.
func TestRender_ReturnsRawTemplateOnParseError(t *testing.T) {
	ts := mailing.NewTemplateService()
	out, err := ts.Render("k", rawLiquidCreative, map[string]interface{}{"first_name": "Drisan"})
	if err == nil {
		t.Fatal("expected a Liquid parse error for an unterminated if-block")
	}
	if out != rawLiquidCreative {
		t.Fatalf("Render no longer returns the original template on parse error; got %q", out)
	}
	if !strings.Contains(out, "{{ first_name }}") || !strings.Contains(out, "{% if") {
		t.Fatal("expected raw Liquid tokens in the returned body")
	}
}

// TestValidateRenderedMessage_RawLiquidSourceIsFatal is the regression guard for
// the INERT GATE half of the incident: the validator did fire and did enumerate
// the tokens, but the html finding was warning-severity and the mode was audit,
// so nothing blocked. Raw source must now block in AUDIT mode — the default and
// the mode production actually runs in.
func TestValidateRenderedMessage_RawLiquidSourceIsFatal(t *testing.T) {
	msg := &EmailMessage{
		Subject:     "Your weekly deals",
		HTMLContent: rawLiquidCreative,
		TextContent: "Text content that is long enough to not be trivial for the validator.",
		Headers: map[string]string{
			"List-Unsubscribe":      "<https://example.com>",
			"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
		},
	}

	issues := ValidateRenderedMessage(msg, ValidatorAudit, "promotional")

	var fatal *ValidationIssue
	for i := range issues {
		if issues[i].Code == "raw_liquid_source" {
			fatal = &issues[i]
		}
	}
	if fatal == nil {
		t.Fatal("raw Liquid control tag in HTML was not flagged as raw_liquid_source")
	}
	if fatal.Severity != SeverityFatal {
		t.Errorf("severity = %q, want %q — a warning here is what let the incident ship", fatal.Severity, SeverityFatal)
	}
	if !HasFatalIssues(issues) {
		t.Error("HasFatalIssues = false in AUDIT mode; the gate is still inert")
	}
	if !HasBlockingIssues(issues) {
		t.Error("HasBlockingIssues = false; fatal must also block in enforce mode")
	}
}

// TestValidateRenderedMessage_CleanBodyNotFatal is the false-positive guard: a
// normally rendered body has no surviving control tags and must not be blocked.
func TestValidateRenderedMessage_CleanBodyNotFatal(t *testing.T) {
	msg := &EmailMessage{
		Subject:     "Your weekly deals",
		HTMLContent: `<html><body><p>Hello Drisan</p><a href="https://x.com/track/unsubscribe/a/b">Unsubscribe</a></body></html>`,
		TextContent: "Text content that is long enough to not be trivial for the validator.",
		Headers: map[string]string{
			"List-Unsubscribe":      "<https://example.com>",
			"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
		},
	}
	if HasFatalIssues(ValidateRenderedMessage(msg, ValidatorAudit, "promotional")) {
		t.Fatal("a cleanly rendered body must never be fatal")
	}
}

// TestQuarantineUnrenderable_TerminalDeadLetter proves the send-path disposition:
// the row goes to dead_letter (terminal, non-retrying), NOT to failed_retryable
// (which would respin a deterministic creative defect forever), and the status
// guard mirrors markFailed's so a recovery-requeued row is never clobbered.
func TestQuarantineUnrenderable_TerminalDeadLetter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	pool := &SendWorkerPool{db: db}
	item := QueueItem{
		ID:           uuid.New(),
		CampaignID:   uuid.New(),
		SubscriberID: uuid.New(),
		Email:        "recipient@example.com",
	}

	mock.ExpectExec("UPDATE mailing_campaign_queue").
		WithArgs(item.ID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	cause := errors.New(`Liquid error (line 17): unterminated "if" block in {% if %}`)
	if err := pool.quarantineUnrenderable(context.Background(), item, "html_content", cause); err != nil {
		t.Fatalf("quarantineUnrenderable: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
	if got := pool.totalFailed; got != 1 {
		t.Errorf("totalFailed = %d, want 1 — a blocked message must be counted, not silently dropped", got)
	}
}

// TestQuarantineUnrenderable_KillSwitch proves DISABLE_RENDER_FAILCLOSED=1
// restores the pre-fix ship-anyway behavior with no DB write — the one-move
// rollback for a send-day wedged on this guard.
func TestQuarantineUnrenderable_KillSwitch(t *testing.T) {
	t.Setenv("DISABLE_RENDER_FAILCLOSED", "1")

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	pool := &SendWorkerPool{db: db}
	item := QueueItem{ID: uuid.New(), CampaignID: uuid.New(), Email: "recipient@example.com"}

	// No ExpectExec registered: any DB write here fails the mock.
	if err := pool.quarantineUnrenderable(context.Background(), item, "html_content", errors.New("boom")); err != nil {
		t.Fatalf("kill switch path must be a no-op, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestFirstRenderFailure_PicksFailingField pins the field attribution that the
// RENDER_BLOCKED operator log depends on.
func TestFirstRenderFailure_PicksFailingField(t *testing.T) {
	boom := errors.New("boom")
	if rf := firstRenderFailure(
		renderFailure{"subject", nil},
		renderFailure{"preview_text", nil},
		renderFailure{"html_content", boom},
	); rf == nil || rf.field != "html_content" {
		t.Fatalf("expected html_content, got %+v", rf)
	}
	if rf := firstRenderFailure(
		renderFailure{"subject", nil},
		renderFailure{"html_content", nil},
	); rf != nil {
		t.Fatalf("all-clean must return nil, got %+v", rf)
	}
}

// TestProcessItem_BrokenCreativeNeverReachesTransport is the WIRING guard —
// the one that would have caught the 2026-08-10 incident.
//
// The helper tests above prove the pieces work; this proves processItem
// actually CALLS them. It drives the real send path with the incident creative
// and asserts the row is quarantined with a render_blocked reason.
//
// The pool is deliberately built with NO senders configured. If the guard were
// ever removed, execution would fall through to the `sender == nil` branch and
// markFailed would write "no sender configured" instead — a different
// error_message, which the argument matcher below rejects. So this test fails
// both when the guard stops blocking AND when it blocks for the wrong reason.
func TestProcessItem_BrokenCreativeNeverReachesTransport(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	pool := &SendWorkerPool{
		db:                         db,
		ctx:                        context.Background(),
		profileTrackingDomainCache: make(map[string]string),
		profileImageHostCache:      make(map[string]string),
		profileSESCache:            make(map[string]profileSESInfo),
		subjectZWCache:             make(map[string]subjectZWConfig),
	}

	item := QueueItem{
		ID:           uuid.New(),
		CampaignID:   uuid.New(),
		SubscriberID: uuid.New(),
		Email:        "recipient@example.com",
		FromName:     "Diane @ Consumer Pro",
		FromEmail:    "diane@em.consumerpro.net",
		Subject:      "Your weekly deals",
		HTMLContent:  rawLiquidCreative, // the incident creative
		ESPType:      "pmta",
	}

	// The ONLY write this test permits: the terminal quarantine, carrying a
	// render_blocked reason. Every other query the path makes is unregistered
	// and therefore errors — which processItem's pre-render steps tolerate.
	mock.ExpectExec("UPDATE mailing_campaign_queue").
		WithArgs(item.ID, renderBlockedReason{}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := pool.processItem(item); err != nil {
		t.Fatalf("processItem returned %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the broken creative did NOT take the quarantine path: %v", err)
	}
}

// renderBlockedReason is a sqlmock argument matcher asserting the quarantine
// reason names the render failure (and not, say, a transport error).
type renderBlockedReason struct{}

func (renderBlockedReason) Match(v driver.Value) bool {
	s, ok := v.(string)
	return ok && strings.Contains(s, "render_blocked") && strings.Contains(s, "html_content")
}
