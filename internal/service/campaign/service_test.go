package campaign_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/domain"
	"github.com/ignite/sparkpost-monitor/internal/service/campaign"
)

// memRepo is an in-memory campaign repository for unit testing.
type memRepo struct {
	mu        sync.Mutex
	campaigns map[string]*domain.Campaign // keyed by id
}

func newMemRepo() *memRepo {
	return &memRepo{campaigns: make(map[string]*domain.Campaign)}
}

func (m *memRepo) Get(_ context.Context, orgID, id string) (*domain.Campaign, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.campaigns[id]
	if !ok || c.OrganizationID != orgID {
		return nil, campaign.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (m *memRepo) List(_ context.Context, orgID string, f campaign.ListFilter) ([]domain.Campaign, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Campaign
	for _, c := range m.campaigns {
		if c.OrganizationID != orgID {
			continue
		}
		if f.Status != "" && string(c.Status) != f.Status {
			continue
		}
		out = append(out, *c)
	}
	total := len(out)
	if f.Offset >= len(out) {
		return nil, total, nil
	}
	end := f.Offset + f.Limit
	if end > len(out) || f.Limit <= 0 {
		end = len(out)
	}
	return out[f.Offset:end], total, nil
}

func (m *memRepo) Create(_ context.Context, c *domain.Campaign) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.ID == "" {
		return "", fmt.Errorf("id required")
	}
	cp := *c
	m.campaigns[cp.ID] = &cp
	return cp.ID, nil
}

func (m *memRepo) Update(_ context.Context, orgID, id string, u campaign.UpdateFields) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.campaigns[id]
	if !ok || c.OrganizationID != orgID {
		return campaign.ErrNotFound
	}
	if u.Name != nil {
		c.Name = *u.Name
	}
	if u.Subject != nil {
		c.Subject = *u.Subject
	}
	return nil
}

func (m *memRepo) Delete(_ context.Context, orgID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.campaigns[id]
	if !ok || c.OrganizationID != orgID {
		return campaign.ErrNotFound
	}
	if c.Status != domain.CampaignDraft && c.Status != domain.CampaignCancelled {
		return fmt.Errorf("can only delete draft/cancelled")
	}
	delete(m.campaigns, id)
	return nil
}

func (m *memRepo) UpdateStatus(_ context.Context, orgID, id string, status domain.CampaignStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.campaigns[id]
	if !ok || c.OrganizationID != orgID {
		return campaign.ErrNotFound
	}
	c.Status = status
	return nil
}

func (m *memRepo) EnqueueSubscribers(_ context.Context, _, _ string, _ int) (int, error) {
	return 42, nil // stub: always enqueue 42
}

const testOrg = "org-1"

func TestCreate(t *testing.T) {
	svc := campaign.NewService(newMemRepo())
	c, err := svc.Create(context.Background(), testOrg, campaign.CreateInput{
		Name: "Test", Subject: "Hello", FromName: "Me", FromEmail: "me@test.com",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if c.Status != domain.CampaignDraft {
		t.Fatalf("expected draft, got %s", c.Status)
	}
}

func TestCreateValidation(t *testing.T) {
	svc := campaign.NewService(newMemRepo())
	_, err := svc.Create(context.Background(), testOrg, campaign.CreateInput{})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestGetNotFound(t *testing.T) {
	svc := campaign.NewService(newMemRepo())
	_, err := svc.Get(context.Background(), testOrg, "nonexistent")
	if err != campaign.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSendTransition(t *testing.T) {
	repo := newMemRepo()
	svc := campaign.NewService(repo)

	c, _ := svc.Create(context.Background(), testOrg, campaign.CreateInput{
		Name: "Camp", Subject: "Sub", FromName: "X", FromEmail: "x@test.com",
	})

	n, err := svc.Send(context.Background(), testOrg, c.ID)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if n != 42 {
		t.Fatalf("expected 42 enqueued, got %d", n)
	}

	got, _ := svc.Get(context.Background(), testOrg, c.ID)
	if got.Status != domain.CampaignSending {
		t.Fatalf("expected sending, got %s", got.Status)
	}
}

func TestSendAlreadySending(t *testing.T) {
	repo := newMemRepo()
	svc := campaign.NewService(repo)
	c, _ := svc.Create(context.Background(), testOrg, campaign.CreateInput{
		Name: "Camp", Subject: "Sub", FromName: "X", FromEmail: "x@test.com",
	})

	svc.Send(context.Background(), testOrg, c.ID)

	_, err := svc.Send(context.Background(), testOrg, c.ID)
	if err != campaign.ErrAlreadySending {
		t.Fatalf("expected ErrAlreadySending, got %v", err)
	}
}

func TestDelete(t *testing.T) {
	repo := newMemRepo()
	svc := campaign.NewService(repo)
	c, _ := svc.Create(context.Background(), testOrg, campaign.CreateInput{
		Name: "Camp", Subject: "Sub", FromName: "X", FromEmail: "x@test.com",
	})

	if err := svc.Delete(context.Background(), testOrg, c.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, err := svc.Get(context.Background(), testOrg, c.ID)
	if err != campaign.ErrNotFound {
		t.Fatalf("expected not found after delete, got %v", err)
	}
}

func TestListWithFilter(t *testing.T) {
	repo := newMemRepo()
	svc := campaign.NewService(repo)

	svc.Create(context.Background(), testOrg, campaign.CreateInput{
		Name: "A", Subject: "Sub", FromName: "X", FromEmail: "x@test.com",
	})
	svc.Create(context.Background(), testOrg, campaign.CreateInput{
		Name: "B", Subject: "Sub", FromName: "X", FromEmail: "x@test.com",
	})

	list, total, err := svc.List(context.Background(), testOrg, campaign.ListFilter{Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 2 || len(list) != 2 {
		t.Fatalf("expected 2 campaigns, got %d (total %d)", len(list), total)
	}
}
