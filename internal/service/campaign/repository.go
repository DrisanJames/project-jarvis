package campaign

import (
	"context"

	"github.com/ignite/sparkpost-monitor/internal/domain"
)

// Repository defines the data access contract for campaigns.
// Implementations must be safe for concurrent use.
type Repository interface {
	// Get returns a single campaign. Returns ErrNotFound if it doesn't exist.
	Get(ctx context.Context, orgID, id string) (*domain.Campaign, error)

	// List returns campaigns matching the given filter, ordered by created_at DESC.
	List(ctx context.Context, orgID string, filter ListFilter) ([]domain.Campaign, int, error)

	// Create inserts a new campaign and returns its ID.
	Create(ctx context.Context, c *domain.Campaign) (string, error)

	// Update modifies a campaign. Only non-zero fields in the update are applied.
	Update(ctx context.Context, orgID, id string, u UpdateFields) error

	// Delete removes a campaign. Only draft/cancelled campaigns can be deleted.
	Delete(ctx context.Context, orgID, id string) error

	// UpdateStatus transitions a campaign's status. Returns ErrInvalidTransition
	// if the transition is not allowed.
	UpdateStatus(ctx context.Context, orgID, id string, status domain.CampaignStatus) error

	// EnqueueSubscribers resolves the campaign's segment/list and inserts
	// queue items. Returns the number of subscribers enqueued.
	EnqueueSubscribers(ctx context.Context, orgID, campaignID string, batchSize int) (int, error)
}

// ListFilter controls pagination and filtering for campaign lists.
type ListFilter struct {
	Status string
	Search string
	Limit  int
	Offset int
}

// UpdateFields holds the mutable fields for a campaign update.
// Zero-value fields are not applied.
type UpdateFields struct {
	Name        *string
	Subject     *string
	FromName    *string
	FromEmail   *string
	HTMLContent *string
	PreviewText *string
	ProfileID   *string
	ScheduledAt *string
}
