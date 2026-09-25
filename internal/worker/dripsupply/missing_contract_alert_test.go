package dripsupply

// missing_contract_alert_test.go — ops-triage AP-5057C887.
//
// Grant paged "no active dispatch contract" every hour for lanes the canary
// never enforces (term_life, auto_insurance, flooring, metal_roofing_signal:
// parked, contract-less by design). The alert must fire only when the cell
// would actually be enforced — the same test failClosed uses — and the
// fail-closed Skip semantics must not move.

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newMissingContractMediator(t *testing.T, mode Mode, canary []CanaryCell, set *ActiveSet) (*Mediator, *captureNotifier) {
	t.Helper()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Date(2026, 9, 25, 16, 0, 0, 0, time.UTC)
	note := &captureNotifier{}
	med := NewMediator(db, NewService(db, WithClock(func() time.Time { return now })), MediatorConfig{
		Mode:           mode,
		Canary:         canary,
		Notifier:       note,
		AlertEvery:     time.Hour,
		Clock:          func() time.Time { return now },
		ContractSource: func(context.Context, time.Time) (*ActiveSet, error) { return set, nil },
	})
	// Inject the tick state directly: nothing under test reaches the DB.
	med.mu.Lock()
	med.day = now.Truncate(24 * time.Hour)
	med.contracts = set
	med.refilled = map[string]bool{}
	med.mu.Unlock()
	return med, note
}

func TestMissingContractAlertGatedOnEnforcement(t *testing.T) {
	const domain = "em.discountblog.com"
	day := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	withDomain := activeSet(day, []*DomainContract{domainContract(domain, 1, map[string]int{"aol": 100})}, nil)
	noDomain := activeSet(day, nil, nil)
	yf := []CanaryCell{{Domain: "*", ISP: "*", Lane: "yahoo_family"}}

	cases := []struct {
		name      string
		mode      Mode
		canary    []CanaryCell
		set       *ActiveSet
		lane      string
		headline  string
		wantAlert bool
		wantSkip  bool
	}{
		{"canary, parked lane, no dispatch contract", ModeCanary, yf, withDomain, "flooring", "no active dispatch contract", false, false},
		{"canary, parked lane, no domain contract", ModeCanary, yf, noDomain, "flooring", "no active domain contract", false, false},
		{"canary, enforced lane, no dispatch contract", ModeCanary, yf, withDomain, "yahoo_family", "no active dispatch contract", true, true},
		{"canary, enforced lane, no domain contract", ModeCanary, yf, noDomain, "yahoo_family", "no active domain contract", true, true},
		{"on, no dispatch contract", ModeOn, nil, withDomain, "flooring", "no active dispatch contract", true, true},
		{"on, no domain contract", ModeOn, nil, noDomain, "flooring", "no active domain contract", true, true},
		{"shadow, no dispatch contract", ModeShadow, nil, withDomain, "flooring", "no active dispatch contract", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			med, note := newMissingContractMediator(t, tc.mode, tc.canary, tc.set)
			a, err := med.Grant(context.Background(), GrantReq{
				Day: day, Lane: tc.lane, Domain: domain, WaveKey: "w1",
				ISPs: []string{"aol", "yahoo"}, Requested: 10,
			})
			if err != nil {
				t.Fatalf("Grant: %v", err)
			}
			if a == nil || a.Reason != SkipNoContract {
				t.Fatalf("want fail-closed allocation with %q, got %+v", SkipNoContract, a)
			}
			if a.Skip != tc.wantSkip {
				t.Errorf("Skip = %v, want %v (failClosed semantics must not change)", a.Skip, tc.wantSkip)
			}
			got := len(note.matching(tc.headline))
			if tc.wantAlert && got != 1 {
				t.Errorf("want 1 %q alert, got %d: %v", tc.headline, got, note.all())
			}
			if !tc.wantAlert && len(note.all()) != 0 {
				t.Errorf("want no alert, got %v", note.all())
			}
		})
	}
}
