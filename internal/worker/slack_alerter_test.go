package worker

import (
	"context"
	"errors"
	"testing"
)

// fakeNotifier records the last Notify call and can simulate a transport error.
type fakeNotifier struct {
	calls     int
	lastTitle string
	lastBody  string
	err       error
}

func (f *fakeNotifier) Notify(title, body string) error {
	f.calls++
	f.lastTitle = title
	f.lastBody = body
	return f.err
}
func (f *fakeNotifier) Name() string { return "fake" }

func TestSlackAlerter_SendSMS_PostsAndReturnsSentinel(t *testing.T) {
	fn := &fakeNotifier{}
	a := NewSlackAlerter(fn, "Campaign lateness")

	id, err := a.SendSMS(context.Background(), "ignored-recipient", "campaign X is late")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "slack" {
		t.Fatalf("expected sentinel id %q, got %q", "slack", id)
	}
	if fn.calls != 1 {
		t.Fatalf("expected exactly 1 Notify call, got %d", fn.calls)
	}
	// fakeNotifier implements only Notify, so Deliver takes the legacy
	// title/body path. Title is the standard headline
	// "<tier mark> · <Scope> · <what happened>" (docs/SLACK_MESSAGE_STANDARD.md);
	// the constructor's label is normalised onto the scope vocabulary
	// ("Campaign lateness" -> "Send"), and the monitor's first line is the
	// headline. There is no second line here, so the legacy body is empty.
	const wantTitle = "\U0001F6A8 ALERT · Send · campaign X is late"
	if fn.lastTitle != wantTitle {
		t.Fatalf("expected title %q, got %q", wantTitle, fn.lastTitle)
	}
	if fn.lastBody != "" {
		t.Fatalf("expected empty legacy body, got %q", fn.lastBody)
	}
}

func TestSlackAlerter_SendSMS_PropagatesError(t *testing.T) {
	fn := &fakeNotifier{err: errors.New("slack 500")}
	a := NewSlackAlerter(fn, "Storage guard")

	id, err := a.SendSMS(context.Background(), "", "disk full")
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	if id != "" {
		t.Fatalf("expected empty id on failure, got %q", id)
	}
}

func TestSlackAlerter_NilNotifier_Errors(t *testing.T) {
	a := NewSlackAlerter(nil, "")
	if _, err := a.SendSMS(context.Background(), "", "x"); err == nil {
		t.Fatal("expected error for nil notifier")
	}
}

// TestSlackAlerter_SplitsHeadlineFromBody — the pagers compose
// "<headline>\n<Label: value>…"; the first line must become the headline and the
// remainder the body (docs/SLACK_MESSAGE_STANDARD.md).
func TestSlackAlerter_SplitsHeadlineFromBody(t *testing.T) {
	fn := &fakeNotifier{}
	a := NewSlackAlerterTiered(fn, "DB", "WARN")

	if _, err := a.SendSMS(context.Background(), "", "retained WAL · *12.4 GB*\nThreshold: 10 GB"); err != nil {
		t.Fatal(err)
	}
	if want := "\u26A0\uFE0F WARN · DB · retained WAL · *12.4 GB*"; fn.lastTitle != want {
		t.Fatalf("title = %q, want %q", fn.lastTitle, want)
	}
	if want := "Threshold: 10 GB"; fn.lastBody != want {
		t.Fatalf("body = %q, want %q", fn.lastBody, want)
	}
}
