package contentdesk

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// UpsertOpsReport stores the LATEST report per (org, source). An older
// generated_at never overwrites a newer one (out-of-order delivery).
func (s *Store) UpsertOpsReport(ctx context.Context, org string, r OpsReport) (OpsReport, error) {
	if err := r.Validate(); err != nil {
		return r, err
	}
	checks := r.Checks
	if checks == nil {
		checks = []OpsCheck{}
	}
	var payload any
	if len(r.Payload) > 0 {
		payload = []byte(r.Payload)
	}
	var received time.Time
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO content_ops_reports (org_id, source, generated_at, checks, payload, received_at)
		VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, NOW())
		ON CONFLICT (org_id, source) DO UPDATE SET
			generated_at = EXCLUDED.generated_at, checks = EXCLUDED.checks,
			payload = EXCLUDED.payload, received_at = EXCLUDED.received_at
		WHERE content_ops_reports.generated_at <= EXCLUDED.generated_at
		RETURNING received_at`, org, r.Source, r.GeneratedAt, mustJSON(checks), payload).Scan(&received)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil // the stored report is newer — keep it, not an error
	}
	if err != nil {
		return r, err
	}
	r.ReceivedAt = received
	return r, nil
}

// ListOpsReports returns the org's latest report per source.
func (s *Store) ListOpsReports(ctx context.Context, org string) ([]OpsReport, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source, generated_at, checks, COALESCE(payload, 'null'::jsonb), received_at
		FROM content_ops_reports WHERE org_id = $1 ORDER BY source`, org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OpsReport{}
	for rows.Next() {
		var r OpsReport
		var checks, payload []byte
		if err := rows.Scan(&r.Source, &r.GeneratedAt, &checks, &payload, &r.ReceivedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(checks, &r.Checks); err != nil {
			r.Checks = []OpsCheck{{Name: "report", Status: CheckUnknown, Detail: "stored checks unreadable"}}
		}
		if string(payload) != "null" {
			r.Payload = payload
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
