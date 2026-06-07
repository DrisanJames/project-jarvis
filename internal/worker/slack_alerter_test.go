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
	if fn.lastTitle != "Campaign lateness" {
		t.Fatalf("expected title %q, got %q", "Campaign lateness", fn.lastTitle)
	}
	if fn.lastBody != "campaign X is late" {
		t.Fatalf("expected body passthrough, got %q", fn.lastBody)
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
