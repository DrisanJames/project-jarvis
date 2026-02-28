package suppression

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ignite/sparkpost-monitor/internal/domain"
)

// Service implements suppression business logic. It is safe for concurrent use.
// All methods are pure: they take typed inputs and return typed outputs.
type Service struct {
	repo Repository
}

// NewService creates a suppression service backed by the given repository.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// IsSuppressed checks whether an email address should be blocked from sending.
func (s *Service) IsSuppressed(ctx context.Context, orgID, email string) (bool, error) {
	return s.repo.IsSuppressed(ctx, orgID, strings.ToLower(strings.TrimSpace(email)))
}

// Suppress adds an email to the global suppression list. Idempotent — if the
// email is already suppressed, the existing record is preserved.
func (s *Service) Suppress(ctx context.Context, orgID, email string, reason domain.SuppressionReason, source domain.SuppressionSource, isp, dsnCode, dsnDiag, sourceIP, campaignID string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return fmt.Errorf("email is required")
	}

	hash := md5.Sum([]byte(email))
	entry := &domain.Suppression{
		OrganizationID: orgID,
		Email:          email,
		MD5Hash:        hex.EncodeToString(hash[:]),
		Reason:         reason,
		Source:         source,
		ISP:            isp,
		DSNCode:        dsnCode,
		DSNDiag:        dsnDiag,
		SourceIP:       sourceIP,
		CampaignID:     campaignID,
	}

	return s.repo.Suppress(ctx, entry)
}

// Remove deletes a suppression entry. Returns an error if the email is not suppressed.
func (s *Service) Remove(ctx context.Context, orgID, email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return fmt.Errorf("email is required")
	}
	return s.repo.Remove(ctx, orgID, email)
}

// List returns suppression entries matching the given filter.
func (s *Service) List(ctx context.Context, orgID string, filter ListFilter) ([]domain.Suppression, int, error) {
	return s.repo.List(ctx, orgID, filter)
}

// Count returns the total number of suppressed emails for an organization.
func (s *Service) Count(ctx context.Context, orgID string) (int, error) {
	return s.repo.Count(ctx, orgID)
}

// Stats returns aggregate counts grouped by reason and source.
type Stats struct {
	Total       int            `json:"total"`
	ByReason    map[string]int `json:"by_reason"`
	BySource    map[string]int `json:"by_source"`
	Last24Hours int            `json:"last_24_hours"`
}

// GetStats computes suppression statistics for the dashboard.
func (s *Service) GetStats(ctx context.Context, orgID string) (*Stats, error) {
	entries, total, err := s.repo.List(ctx, orgID, ListFilter{Limit: 0})
	if err != nil {
		return nil, err
	}

	stats := &Stats{
		Total:    total,
		ByReason: make(map[string]int),
		BySource: make(map[string]int),
	}
	for _, e := range entries {
		stats.ByReason[string(e.Reason)]++
		stats.BySource[string(e.Source)]++
	}
	return stats, nil
}

// AllEmails returns every suppressed email for the org. Used by the file sync
// worker to rebuild the PMTA suppression file.
func (s *Service) AllEmails(ctx context.Context, orgID string) ([]string, error) {
	return s.repo.AllEmails(ctx, orgID)
}
