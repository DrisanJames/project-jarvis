package engine

import (
	"context"
	"testing"
)

// fakeSuppressionRepo records Add calls; every other method is unused by
// SuppressionAgent.ProcessRecord.
type fakeSuppressionRepo struct{ added []Suppression }

func (f *fakeSuppressionRepo) LoadAll(context.Context, string) (map[ISP][]string, error) {
	return nil, nil
}
func (f *fakeSuppressionRepo) Add(_ context.Context, s Suppression) error {
	f.added = append(f.added, s)
	return nil
}
func (f *fakeSuppressionRepo) Remove(context.Context, string, ISP, string) error { return nil }
func (f *fakeSuppressionRepo) Get(context.Context, string, ISP, string) (*Suppression, error) {
	return nil, nil
}
func (f *fakeSuppressionRepo) List(context.Context, string, ISP, string, int, int) ([]Suppression, int, error) {
	return nil, 0, nil
}
func (f *fakeSuppressionRepo) Stats(context.Context, string, ISP) (*SuppressionStats, error) {
	return nil, nil
}
func (f *fakeSuppressionRepo) ListEmails(context.Context, string, ISP) ([]string, error) {
	return nil, nil
}

func newTestSuppressionAgent(isp ISP) (*SuppressionAgent, *fakeSuppressionRepo) {
	repo := &fakeSuppressionRepo{}
	store := NewSuppressionStore(repo, "org", "")
	a := NewSuppressionAgent(AgentID{ISP: isp, AgentType: AgentSuppression}, ISPConfig{}, store, nil, nil, nil)
	return a, repo
}

// Apple policy/capacity codes describe the SENDER, never the mailbox. 2026-10-21:
// 1,140 Apple subscribers were permanently suppressed with two 421 [HCM1]
// deferrals in 24h as their only reason (released by ledger cl-20261022-032710-41d0).
func TestSuppressionAgent_AppleHCM1DeferralsDoNotSuppress(t *testing.T) {
	a, repo := newTestSuppressionAgent(ISPApple)
	rec := AccountingRecord{
		Type:      "t",
		Recipient: "someone@icloud.com",
		DSNStatus: "4.7.0",
		DSNDiag:   "smtp;421 4.7.0 [HCM1] Messages temporarily deferred due to unexpected volume",
	}
	for i := 0; i < 3; i++ {
		a.ProcessRecord(context.Background(), rec)
	}
	if len(repo.added) != 0 {
		t.Fatalf("Apple 421 HCM1 deferrals must not suppress; got %d suppression(s): %+v", len(repo.added), repo.added)
	}
}

// Apple 554 5.7.1 [HM08] is a per-message content block — even when the MTA
// labels the bounce bad-mailbox it must never suppress the address.
func TestSuppressionAgent_AppleHM08BounceDoesNotSuppress(t *testing.T) {
	for _, cat := range []string{"policy-related", "spam-related", "bad-mailbox"} {
		a, repo := newTestSuppressionAgent(ISPApple)
		a.ProcessRecord(context.Background(), AccountingRecord{
			Type:      "b",
			Recipient: "someone@icloud.com",
			BounceCat: cat,
			DSNStatus: "5.7.1",
			DSNDiag:   "smtp;554 5.7.1 [HM08] Message rejected due to local policy",
		})
		if len(repo.added) != 0 {
			t.Errorf("bounceCat=%s: Apple HM08 must not suppress; got %+v", cat, repo.added)
		}
	}
}

// Every evidenced Apple bracket code is a sender-side policy/capacity signal.
func TestIsApplePolicySignal(t *testing.T) {
	for _, d := range []string{
		"smtp;421 4.7.0 [HCM1] try again later",
		"smtp;550 5.7.1 [HCM2] Message rejected due to local policy",
		"smtp;554 5.7.1 [HM08] Message rejected due to local policy",
		"smtp;554 5.7.1 [CS01] Message rejected due to local policy",
		"smtp;554 5.7.1 [CS02] Message rejected due to local policy",
		"smtp;554 5.7.1 [PH01] Message rejected due to local policy",
	} {
		if !isApplePolicySignal(d) {
			t.Errorf("expected Apple policy signal: %q", d)
		}
	}
	for _, d := range []string{"smtp;550 5.1.1 <x@icloud.com>: user does not exist", "smtp;452 4.2.2 Mailbox full", ""} {
		if isApplePolicySignal(d) {
			t.Errorf("must NOT be Apple policy signal: %q", d)
		}
	}
}

// Negative controls: genuine address-level failures still suppress.
func TestSuppressionAgent_GenuineFailuresStillSuppress(t *testing.T) {
	t.Run("550 5.1.1 user unknown", func(t *testing.T) {
		a, repo := newTestSuppressionAgent(ISPApple)
		a.ProcessRecord(context.Background(), AccountingRecord{
			Type:      "b",
			Recipient: "gone@icloud.com",
			BounceCat: "bad-mailbox",
			DSNStatus: "5.1.1",
			DSNDiag:   "smtp;550 5.1.1 <gone@icloud.com>: user does not exist",
		})
		if len(repo.added) != 1 || repo.added[0].Reason != "bad-mailbox" {
			t.Fatalf("550 5.1.1 must suppress as bad-mailbox; got %+v", repo.added)
		}
	})
	t.Run("repeated mailbox-full transient", func(t *testing.T) {
		a, repo := newTestSuppressionAgent(ISPApple)
		rec := AccountingRecord{
			Type:      "t",
			Recipient: "full@icloud.com",
			DSNStatus: "4.2.2",
			DSNDiag:   "smtp;452 4.2.2 Mailbox full",
		}
		a.ProcessRecord(context.Background(), rec)
		if len(repo.added) != 0 {
			t.Fatalf("one transient must not suppress; got %+v", repo.added)
		}
		a.ProcessRecord(context.Background(), rec)
		if len(repo.added) != 1 || repo.added[0].Reason != "repeated-transient" {
			t.Fatalf("2 mailbox-full transients in 24h must suppress; got %+v", repo.added)
		}
	})
}
