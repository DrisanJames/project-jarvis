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
	// title/body path: title = "<scope> — <headline>", body holds the rest
	// (empty here). The scope is the title passed to the constructor and the
	// headline is the monitor's body string.
	if fn.lastTitle != "Campaign lateness — campaign X is late" {
		t.Fatalf("expected title %q, got %q", "Campaign lateness — campaign X is late", fn.lastTitle)
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
