package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/domain"
	"github.com/ignite/sparkpost-monitor/internal/service/campaign"
)

// CampaignRepo implements campaign.Repository against PostgreSQL.
type CampaignRepo struct{ db *sql.DB }

// NewCampaignRepo creates a Postgres-backed campaign repository.
func NewCampaignRepo(db *sql.DB) *CampaignRepo { return &CampaignRepo{db: db} }

func (r *CampaignRepo) Get(ctx context.Context, orgID, id string) (*domain.Campaign, error) {
	c := &domain.Campaign{}
	err := r.db.QueryRowContext(ctx, `
		SELECT id, organization_id, name, subject, from_name, from_email,
		       COALESCE(reply_to,''), COALESCE(html_content,''), COALESCE(plain_content,''),
		       status, sent_count, open_count, click_count, bounce_count,
		       complaint_count, unsubscribe_count, revenue, created_at, updated_at
		FROM mailing_campaigns
		WHERE id = $1 AND organization_id = $2
	`, id, orgID).Scan(
		&c.ID, &c.OrganizationID, &c.Name, &c.Subject, &c.FromName, &c.FromEmail,
		&c.ReplyTo, &c.HTMLContent, &c.PlainContent,
		&c.Status, &c.SentCount, &c.OpenCount, &c.ClickCount, &c.BounceCount,
		&c.ComplaintCount, &c.UnsubscribeCount, &c.Revenue, &c.CreatedAt, &c.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, campaign.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get campaign: %w", err)
	}
	return c, nil
}

func (r *CampaignRepo) List(ctx context.Context, orgID string, f campaign.ListFilter) ([]domain.Campaign, int, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}

	countQ := `SELECT COUNT(*) FROM mailing_campaigns WHERE organization_id = $1`
	args := []interface{}{orgID}
	idx := 2

	if f.Status != "" {
		countQ += fmt.Sprintf(" AND status = $%d", idx)
		args = append(args, f.Status)
		idx++
	}

	var total int
	if err := r.db.QueryRowContext(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count campaigns: %w", err)
	}

	q := `
		SELECT id, name, subject, from_name, from_email, status,
		       sent_count, open_count, click_count, revenue, created_at
		FROM mailing_campaigns
		WHERE organization_id = $1`

	qArgs := []interface{}{orgID}
	qIdx := 2
	if f.Status != "" {
		q += fmt.Sprintf(" AND status = $%d", qIdx)
		qArgs = append(qArgs, f.Status)
		qIdx++
	}
	q += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d OFFSET $%d", qIdx, qIdx+1)
	qArgs = append(qArgs, limit, f.Offset)

	rows, err := r.db.QueryContext(ctx, q, qArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("list campaigns: %w", err)
	}
	defer rows.Close()

	var out []domain.Campaign
	for rows.Next() {
		var c domain.Campaign
		if err := rows.Scan(
			&c.ID, &c.Name, &c.Subject, &c.FromName, &c.FromEmail, &c.Status,
			&c.SentCount, &c.OpenCount, &c.ClickCount, &c.Revenue, &c.CreatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("scan campaign: %w", err)
		}
		c.OrganizationID = orgID
		out = append(out, c)
	}
	return out, total, nil
}

func (r *CampaignRepo) Create(ctx context.Context, c *domain.Campaign) (string, error) {
	if c.ID == "" {
		c.ID = uuid.New().String()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO mailing_campaigns
			(id, organization_id, list_id, name, subject, from_name, from_email,
			 html_content, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW(), NOW())
	`, c.ID, c.OrganizationID, c.ListID, c.Name, c.Subject,
		c.FromName, c.FromEmail, c.HTMLContent, c.Status)
	if err != nil {
		return "", fmt.Errorf("create campaign: %w", err)
	}
	return c.ID, nil
}

func (r *CampaignRepo) Update(ctx context.Context, orgID, id string, u campaign.UpdateFields) error {
	sets := []string{}
	args := []interface{}{}
	idx := 1
	add := func(col string, val interface{}) {
		sets = append(sets, fmt.Sprintf("%s = $%d", col, idx))
		args = append(args, val)
		idx++
	}

	if u.Name != nil {
		add("name", *u.Name)
	}
	if u.Subject != nil {
		add("subject", *u.Subject)
	}
	if u.FromName != nil {
		add("from_name", *u.FromName)
	}
	if u.FromEmail != nil {
		add("from_email", *u.FromEmail)
	}
	if u.HTMLContent != nil {
		add("html_content", *u.HTMLContent)
	}
	if u.PreviewText != nil {
		add("preview_text", *u.PreviewText)
	}
	if u.ProfileID != nil {
		add("sending_profile_id", *u.ProfileID)
	}

	if len(sets) == 0 {
		return nil
	}

	add("updated_at", "NOW()")
	q := fmt.Sprintf("UPDATE mailing_campaigns SET %s WHERE id = $%d AND organization_id = $%d",
		joinComma(sets), idx, idx+1)
	args = append(args, id, orgID)

	res, err := r.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("update campaign: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return campaign.ErrNotFound
	}
	return nil
}

func (r *CampaignRepo) Delete(ctx context.Context, orgID, id string) error {
	res, err := r.db.ExecContext(ctx, `
		DELETE FROM mailing_campaigns
		WHERE id = $1 AND organization_id = $2 AND status IN ('draft','cancelled')
	`, id, orgID)
	if err != nil {
		return fmt.Errorf("delete campaign: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return campaign.ErrNotFound
	}
	return nil
}

func (r *CampaignRepo) UpdateStatus(ctx context.Context, orgID, id string, status domain.CampaignStatus) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE mailing_campaigns SET status = $1, updated_at = NOW()
		WHERE id = $2 AND organization_id = $3
	`, status, id, orgID)
	if err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return campaign.ErrNotFound
	}
	return nil
}

func (r *CampaignRepo) EnqueueSubscribers(ctx context.Context, orgID, campaignID string, batchSize int) (int, error) {
	// Resolve list_id from campaign
	var listID sql.NullString
	if err := r.db.QueryRowContext(ctx, `
		SELECT list_id FROM mailing_campaigns WHERE id = $1 AND organization_id = $2
	`, campaignID, orgID).Scan(&listID); err != nil {
		if err == sql.ErrNoRows {
			return 0, campaign.ErrNotFound
		}
		return 0, fmt.Errorf("resolve list: %w", err)
	}
	if !listID.Valid || listID.String == "" {
		return 0, fmt.Errorf("campaign has no list_id")
	}

	res, err := r.db.ExecContext(ctx, `
		INSERT INTO mailing_campaign_queue (id, campaign_id, subscriber_id, email, status, created_at)
		SELECT gen_random_uuid(), $1, s.id, s.email, 'queued', NOW()
		FROM mailing_subscribers s
		WHERE s.list_id = $2 AND s.status = 'active'
		  AND NOT EXISTS (
		      SELECT 1 FROM mailing_suppressions sup
		      WHERE sup.email = s.email AND sup.active = true
		  )
		ON CONFLICT DO NOTHING
	`, campaignID, listID.String)
	if err != nil {
		return 0, fmt.Errorf("enqueue: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
