package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/domain"
	"github.com/ignite/sparkpost-monitor/internal/service/suppression"
)

// SuppressionRepo implements suppression.Repository against PostgreSQL.
type SuppressionRepo struct{ db *sql.DB }

// NewSuppressionRepo creates a Postgres-backed suppression repository.
func NewSuppressionRepo(db *sql.DB) *SuppressionRepo { return &SuppressionRepo{db: db} }

func (r *SuppressionRepo) IsSuppressed(ctx context.Context, orgID, email string) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM mailing_suppressions WHERE email = $1 AND active = true)`,
		email,
	).Scan(&exists)
	return exists, err
}

func (r *SuppressionRepo) Suppress(ctx context.Context, s *domain.Suppression) error {
	if s.ID == "" {
		s.ID = uuid.New().String()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO mailing_suppressions (id, email, reason, source, active, created_at)
		VALUES ($1, $2, $3, $4, true, NOW())
		ON CONFLICT (email) DO UPDATE SET reason = $3, active = true, updated_at = NOW()
	`, s.ID, s.Email, s.Reason, s.Source)
	if err != nil {
		return fmt.Errorf("suppress: %w", err)
	}
	return nil
}

func (r *SuppressionRepo) Remove(ctx context.Context, orgID, email string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE mailing_suppressions SET active = false, updated_at = NOW() WHERE email = $1`,
		email,
	)
	if err != nil {
		return fmt.Errorf("remove suppression: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return suppression.ErrNotFound
	}
	return nil
}

func (r *SuppressionRepo) List(ctx context.Context, orgID string, f suppression.ListFilter) ([]domain.Suppression, int, error) {
	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_suppressions WHERE active = true`,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count suppressions: %w", err)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = total
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT id, email, reason, source, created_at
		FROM mailing_suppressions
		WHERE active = true
		ORDER BY created_at DESC
		LIMIT $1 OFFSET $2
	`, limit, f.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list suppressions: %w", err)
	}
	defer rows.Close()

	var out []domain.Suppression
	for rows.Next() {
		var s domain.Suppression
		if err := rows.Scan(&s.ID, &s.Email, &s.Reason, &s.Source, &s.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan suppression: %w", err)
		}
		out = append(out, s)
	}
	return out, total, nil
}

func (r *SuppressionRepo) Count(ctx context.Context, orgID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_suppressions WHERE active = true`,
	).Scan(&n)
	return n, err
}

func (r *SuppressionRepo) AllEmails(ctx context.Context, orgID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT email FROM mailing_suppressions WHERE active = true ORDER BY email`,
	)
	if err != nil {
		return nil, fmt.Errorf("all suppressed emails: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, err
		}
		out = append(out, email)
	}
	return out, nil
}
