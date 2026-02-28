package suppression

import (
	"context"

	"github.com/ignite/sparkpost-monitor/internal/domain"
)

// Repository defines the data access contract for the suppression list.
type Repository interface {
	// IsSuppressed returns true if the email is on the global suppression list.
	IsSuppressed(ctx context.Context, orgID, email string) (bool, error)

	// Suppress adds an email to the suppression list. If it already exists,
	// the existing record is preserved (idempotent).
	Suppress(ctx context.Context, s *domain.Suppression) error

	// Remove deletes a suppression entry. Returns ErrNotFound if it doesn't exist.
	Remove(ctx context.Context, orgID, email string) error

	// List returns suppression entries matching the filter.
	List(ctx context.Context, orgID string, filter ListFilter) ([]domain.Suppression, int, error)

	// Count returns the total number of suppressed emails for an org.
	Count(ctx context.Context, orgID string) (int, error)

	// AllEmails returns every suppressed email address for an org (for file sync).
	AllEmails(ctx context.Context, orgID string) ([]string, error)
}

// ListFilter controls pagination and filtering for suppression lists.
type ListFilter struct {
	Reason string
	Source string
	ISP    string
	Search string
	Limit  int
	Offset int
}
