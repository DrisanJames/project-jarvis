package suppression

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/domain"
)

// mockRepo is an in-memory repository for testing.
type mockRepo struct {
	mu    sync.RWMutex
	store map[string]*domain.Suppression // keyed by "orgID:email"
}

func newMockRepo() *mockRepo {
	return &mockRepo{store: make(map[string]*domain.Suppression)}
}

func (m *mockRepo) key(orgID, email string) string {
	return orgID + ":" + strings.ToLower(email)
}

func (m *mockRepo) IsSuppressed(_ context.Context, orgID, email string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.store[m.key(orgID, email)]
	return ok, nil
}

func (m *mockRepo) Suppress(_ context.Context, s *domain.Suppression) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.key(s.OrganizationID, s.Email)
	if _, exists := m.store[k]; exists {
		return nil
	}
	m.store[k] = s
	return nil
}

func (m *mockRepo) Remove(_ context.Context, orgID, email string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.key(orgID, email)
	if _, ok := m.store[k]; !ok {
		return fmt.Errorf("not found")
	}
	delete(m.store, k)
	return nil
}

func (m *mockRepo) List(_ context.Context, orgID string, f ListFilter) ([]domain.Suppression, int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []domain.Suppression
	for _, s := range m.store {
		if s.OrganizationID != orgID {
			continue
		}
		if f.Reason != "" && string(s.Reason) != f.Reason {
			continue
		}
		result = append(result, *s)
	}
	return result, len(result), nil
}

func (m *mockRepo) Count(_ context.Context, orgID string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, s := range m.store {
		if s.OrganizationID == orgID {
			count++
		}
	}
	return count, nil
}

func (m *mockRepo) AllEmails(_ context.Context, orgID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var emails []string
	for _, s := range m.store {
		if s.OrganizationID == orgID {
			emails = append(emails, s.Email)
		}
	}
	return emails, nil
}

const testOrgID = "org-001"

func TestSuppress_AddsEmailToList(t *testing.T) {
	svc := NewService(newMockRepo())
	ctx := context.Background()

	err := svc.Suppress(ctx, testOrgID, "BOUNCE@example.com",
		domain.ReasonHardBounce, domain.SourcePMTABounce,
		"gmail", "550", "user unknown", "1.2.3.4", "camp-001")
	if err != nil {
		t.Fatalf("Suppress: %v", err)
	}

	ok, err := svc.IsSuppressed(ctx, testOrgID, "bounce@example.com")
	if err != nil {
		t.Fatalf("IsSuppressed: %v", err)
	}
	if !ok {
		t.Error("expected email to be suppressed after Suppress()")
	}
}

func TestSuppress_Idempotent(t *testing.T) {
	svc := NewService(newMockRepo())
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		err := svc.Suppress(ctx, testOrgID, "dup@example.com",
			domain.ReasonComplaint, domain.SourceFBLReport, "", "", "", "", "")
		if err != nil {
			t.Fatalf("Suppress #%d: %v", i, err)
		}
	}

	count, _ := svc.Count(ctx, testOrgID)
	if count != 1 {
		t.Errorf("expected 1 suppression, got %d", count)
	}
}

func TestSuppress_EmptyEmail_Fails(t *testing.T) {
	svc := NewService(newMockRepo())
	ctx := context.Background()

	err := svc.Suppress(ctx, testOrgID, "",
		domain.ReasonHardBounce, domain.SourcePMTABounce, "", "", "", "", "")
	if err == nil {
		t.Error("expected error for empty email")
	}
}

func TestRemove_DeletesSuppression(t *testing.T) {
	svc := NewService(newMockRepo())
	ctx := context.Background()

	_ = svc.Suppress(ctx, testOrgID, "remove@example.com",
		domain.ReasonManual, domain.SourceManual, "", "", "", "", "")

	err := svc.Remove(ctx, testOrgID, "remove@example.com")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}

	ok, _ := svc.IsSuppressed(ctx, testOrgID, "remove@example.com")
	if ok {
		t.Error("expected email to no longer be suppressed after Remove()")
	}
}

func TestRemove_NotFound_ReturnsError(t *testing.T) {
	svc := NewService(newMockRepo())
	ctx := context.Background()

	err := svc.Remove(ctx, testOrgID, "ghost@example.com")
	if err == nil {
		t.Error("expected error when removing non-existent suppression")
	}
}

func TestList_FiltersByReason(t *testing.T) {
	repo := newMockRepo()
	svc := NewService(repo)
	ctx := context.Background()

	_ = svc.Suppress(ctx, testOrgID, "bounce1@example.com",
		domain.ReasonHardBounce, domain.SourcePMTABounce, "", "", "", "", "")
	_ = svc.Suppress(ctx, testOrgID, "complaint1@example.com",
		domain.ReasonComplaint, domain.SourceFBLReport, "", "", "", "", "")
	_ = svc.Suppress(ctx, testOrgID, "bounce2@example.com",
		domain.ReasonHardBounce, domain.SourcePMTABounce, "", "", "", "", "")

	results, total, err := svc.List(ctx, testOrgID, ListFilter{Reason: "hard_bounce"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 2 {
		t.Errorf("expected 2 hard bounces, got %d", total)
	}
	for _, r := range results {
		if r.Reason != domain.ReasonHardBounce {
			t.Errorf("unexpected reason: %s", r.Reason)
		}
	}
}

func TestGetStats_AggregatesByReasonAndSource(t *testing.T) {
	repo := newMockRepo()
	svc := NewService(repo)
	ctx := context.Background()

	_ = svc.Suppress(ctx, testOrgID, "a@example.com",
		domain.ReasonHardBounce, domain.SourcePMTABounce, "", "", "", "", "")
	_ = svc.Suppress(ctx, testOrgID, "b@example.com",
		domain.ReasonComplaint, domain.SourceFBLReport, "", "", "", "", "")
	_ = svc.Suppress(ctx, testOrgID, "c@example.com",
		domain.ReasonHardBounce, domain.SourceESPWebhook, "", "", "", "", "")

	stats, err := svc.GetStats(ctx, testOrgID)
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.Total != 3 {
		t.Errorf("expected total=3, got %d", stats.Total)
	}
	if stats.ByReason["hard_bounce"] != 2 {
		t.Errorf("expected 2 hard bounces, got %d", stats.ByReason["hard_bounce"])
	}
	if stats.BySource["fbl_report"] != 1 {
		t.Errorf("expected 1 fbl_report, got %d", stats.BySource["fbl_report"])
	}
}
