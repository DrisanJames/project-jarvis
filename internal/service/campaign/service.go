package campaign

import (
	"context"
	"fmt"
	"log"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/domain"
)

// Service implements campaign business logic. It coordinates between the
// repository layer and any cross-cutting concerns (suppression, throttling).
// All public methods are safe for concurrent use if the underlying
// repository is concurrency-safe.
type Service struct {
	repo Repository
}

// NewService creates a campaign service backed by the given repository.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// Get returns a single campaign.
func (s *Service) Get(ctx context.Context, orgID, id string) (*domain.Campaign, error) {
	return s.repo.Get(ctx, orgID, id)
}

// List returns campaigns matching the filter.
func (s *Service) List(ctx context.Context, orgID string, f ListFilter) ([]domain.Campaign, int, error) {
	return s.repo.List(ctx, orgID, f)
}

// Create validates and persists a new campaign in draft status.
func (s *Service) Create(ctx context.Context, orgID string, input CreateInput) (*domain.Campaign, error) {
	if input.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if input.Subject == "" {
		return nil, fmt.Errorf("subject is required")
	}

	c := &domain.Campaign{
		ID:             uuid.New().String(),
		OrganizationID: orgID,
		Name:           input.Name,
		Subject:        input.Subject,
		FromName:       input.FromName,
		FromEmail:      input.FromEmail,
		HTMLContent:    input.HTMLContent,
		Status:         domain.CampaignDraft,
	}
	if input.ListID != "" {
		c.ListID = &input.ListID
	}

	id, err := s.repo.Create(ctx, c)
	if err != nil {
		return nil, err
	}
	c.ID = id
	return c, nil
}

// Update modifies mutable campaign fields. Only draft campaigns should be
// edited, but we leave that enforcement to the repository/database.
func (s *Service) Update(ctx context.Context, orgID, id string, u UpdateFields) error {
	return s.repo.Update(ctx, orgID, id, u)
}

// Delete removes a campaign (only draft/cancelled).
func (s *Service) Delete(ctx context.Context, orgID, id string) error {
	return s.repo.Delete(ctx, orgID, id)
}

// Send transitions a campaign to sending state and enqueues subscribers.
// Returns the number of recipients enqueued.
func (s *Service) Send(ctx context.Context, orgID, campaignID string) (int, error) {
	c, err := s.repo.Get(ctx, orgID, campaignID)
	if err != nil {
		return 0, err
	}

	if c.Status != domain.CampaignDraft && c.Status != domain.CampaignScheduled {
		return 0, ErrAlreadySending
	}

	if err := s.repo.UpdateStatus(ctx, orgID, campaignID, domain.CampaignSending); err != nil {
		return 0, fmt.Errorf("transition to sending: %w", err)
	}

	n, err := s.repo.EnqueueSubscribers(ctx, orgID, campaignID, 1000)
	if err != nil {
		// Roll back status on enqueue failure
		if rbErr := s.repo.UpdateStatus(ctx, orgID, campaignID, domain.CampaignFailed); rbErr != nil {
			log.Printf("[campaign.Service] rollback failed: %v", rbErr)
		}
		return 0, fmt.Errorf("enqueue subscribers: %w", err)
	}

	log.Printf("[campaign.Service] Campaign %s: enqueued %d recipients", campaignID, n)
	return n, nil
}

// CreateInput holds the fields for creating a new campaign.
type CreateInput struct {
	Name        string `json:"name"`
	Subject     string `json:"subject"`
	FromName    string `json:"from_name"`
	FromEmail   string `json:"from_email"`
	HTMLContent string `json:"html_content"`
	ListID      string `json:"list_id"`
}
