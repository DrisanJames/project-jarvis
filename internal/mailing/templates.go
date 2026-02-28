package mailing

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// EmailTemplate represents an email template with folder support
type EmailTemplate struct {
	ID             uuid.UUID  `json:"id" db:"id"`
	OrganizationID uuid.UUID  `json:"organization_id" db:"organization_id"`
	FolderID       *uuid.UUID `json:"folder_id,omitempty" db:"folder_id"`
	CategoryID     *uuid.UUID `json:"category_id,omitempty" db:"category_id"`
	Name           string     `json:"name" db:"name"`
	Description    string     `json:"description,omitempty" db:"description"`
	Subject        string     `json:"subject,omitempty" db:"subject"`
	FromName       string     `json:"from_name,omitempty" db:"from_name"`
	FromEmail      string     `json:"from_email,omitempty" db:"from_email"`
	ReplyTo        string     `json:"reply_to,omitempty" db:"reply_to"`
	HTMLContent    string     `json:"html_content" db:"html_content"`
	PlainContent   string     `json:"plain_content,omitempty" db:"plain_content"`
	PreviewText    string     `json:"preview_text,omitempty" db:"preview_text"`
	ThumbnailURL   string     `json:"thumbnail_url,omitempty" db:"thumbnail_url"`
	Status         string     `json:"status" db:"status"`
	IsActive       bool       `json:"is_active" db:"is_active"`
	CreatedAt      time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at" db:"updated_at"`
	// Computed fields
	FolderPath     string     `json:"folder_path,omitempty"`
	FolderName     string     `json:"folder_name,omitempty"`
}

// CreateTemplateRequest represents the request to create a template
type CreateTemplateRequest struct {
	FolderID     *uuid.UUID `json:"folder_id,omitempty"`
	Name         string     `json:"name"`
	Description  string     `json:"description,omitempty"`
	Subject      string     `json:"subject,omitempty"`
	FromName     string     `json:"from_name,omitempty"`
	FromEmail    string     `json:"from_email,omitempty"`
	ReplyTo      string     `json:"reply_to,omitempty"`
	HTMLContent  string     `json:"html_content"`
	PlainContent string     `json:"plain_content,omitempty"`
	PreviewText  string     `json:"preview_text,omitempty"`
	ThumbnailURL string     `json:"thumbnail_url,omitempty"`
}

// UpdateTemplateRequest represents the request to update a template
type UpdateTemplateRequest struct {
	FolderID     *uuid.UUID `json:"folder_id,omitempty"`
	Name         *string    `json:"name,omitempty"`
	Description  *string    `json:"description,omitempty"`
	Subject      *string    `json:"subject,omitempty"`
	FromName     *string    `json:"from_name,omitempty"`
	FromEmail    *string    `json:"from_email,omitempty"`
	ReplyTo      *string    `json:"reply_to,omitempty"`
	HTMLContent  *string    `json:"html_content,omitempty"`
	PlainContent *string    `json:"plain_content,omitempty"`
	PreviewText  *string    `json:"preview_text,omitempty"`
	ThumbnailURL *string    `json:"thumbnail_url,omitempty"`
	Status       *string    `json:"status,omitempty"`
}

// EmailTemplateStore provides database operations for email templates
// Note: This is distinct from TemplateService in template_engine.go which handles Liquid rendering
type EmailTemplateStore struct {
	db *sql.DB
}

// NewEmailTemplateStore creates a new EmailTemplateStore
func NewEmailTemplateStore(db *sql.DB) *EmailTemplateStore {
	return &EmailTemplateStore{db: db}
}

// CreateTemplate creates a new email template
func (s *EmailTemplateStore) CreateTemplate(ctx context.Context, orgID uuid.UUID, req *CreateTemplateRequest) (*EmailTemplate, error) {
	template := &EmailTemplate{
		ID:             uuid.New(),
		OrganizationID: orgID,
		FolderID:       req.FolderID,
		Name:           req.Name,
		Description:    req.Description,
		Subject:        req.Subject,
		FromName:       req.FromName,
		FromEmail:      req.FromEmail,
		ReplyTo:        req.ReplyTo,
		HTMLContent:    req.HTMLContent,
		PlainContent:   req.PlainContent,
		PreviewText:    req.PreviewText,
		ThumbnailURL:   req.ThumbnailURL,
		Status:         "active",
		IsActive:       true,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}

	query := `INSERT INTO mailing_templates (
		id, organization_id, folder_id, name, description, subject, from_name, from_email, 
		reply_to, html_content, plain_content, preview_text, thumbnail_url, status, created_at, updated_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
	RETURNING id, created_at, updated_at`

	err := s.db.QueryRowContext(ctx, query,
		template.ID, template.OrganizationID, template.FolderID, template.Name, template.Description,
		template.Subject, template.FromName, template.FromEmail, template.ReplyTo,
		template.HTMLContent, template.PlainContent, template.PreviewText, template.ThumbnailURL,
		template.Status, template.CreatedAt, template.UpdatedAt,
	).Scan(&template.ID, &template.CreatedAt, &template.UpdatedAt)

	if err != nil {
		return nil, fmt.Errorf("failed to create template: %w", err)
	}

	// Get folder path if folder_id is set
	if template.FolderID != nil {
		s.db.QueryRowContext(ctx, `SELECT name, path FROM mailing_template_folders WHERE id = $1`, template.FolderID).Scan(&template.FolderName, &template.FolderPath)
	}

	return template, nil
}

// GetTemplate retrieves a template by ID
func (s *EmailTemplateStore) GetTemplate(ctx context.Context, id uuid.UUID) (*EmailTemplate, error) {
	query := `SELECT t.id, t.organization_id, t.folder_id, t.category_id, t.name, 
		COALESCE(t.description, '') as description, COALESCE(t.subject, '') as subject,
		COALESCE(t.from_name, '') as from_name, COALESCE(t.from_email, '') as from_email,
		COALESCE(t.reply_to, '') as reply_to, COALESCE(t.html_content, '') as html_content,
		COALESCE(t.plain_content, '') as plain_content, COALESCE(t.preview_text, '') as preview_text,
		COALESCE(t.thumbnail_url, '') as thumbnail_url, t.status, t.created_at, t.updated_at,
		COALESCE(f.name, '') as folder_name, COALESCE(f.path, '') as folder_path
		FROM mailing_templates t
		LEFT JOIN mailing_template_folders f ON t.folder_id = f.id
		WHERE t.id = $1`

	template := &EmailTemplate{}
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&template.ID, &template.OrganizationID, &template.FolderID, &template.CategoryID,
		&template.Name, &template.Description, &template.Subject, &template.FromName, &template.FromEmail,
		&template.ReplyTo, &template.HTMLContent, &template.PlainContent, &template.PreviewText,
		&template.ThumbnailURL, &template.Status, &template.CreatedAt, &template.UpdatedAt,
		&template.FolderName, &template.FolderPath,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get template: %w", err)
	}

	template.IsActive = template.Status == "active"
	return template, nil
}

// ListTemplates lists templates for an organization, optionally filtered by folder
func (s *EmailTemplateStore) ListTemplates(ctx context.Context, orgID uuid.UUID, folderID *uuid.UUID) ([]*EmailTemplate, error) {
	var query string
	var args []interface{}

	if folderID == nil {
		// List all templates (optionally only root level)
		query = `SELECT t.id, t.organization_id, t.folder_id, t.category_id, t.name, 
			COALESCE(t.description, '') as description, COALESCE(t.subject, '') as subject,
			COALESCE(t.from_name, '') as from_name, COALESCE(t.from_email, '') as from_email,
			COALESCE(t.reply_to, '') as reply_to, COALESCE(t.html_content, '') as html_content,
			COALESCE(t.plain_content, '') as plain_content, COALESCE(t.preview_text, '') as preview_text,
			COALESCE(t.thumbnail_url, '') as thumbnail_url, t.status, t.created_at, t.updated_at,
			COALESCE(f.name, '') as folder_name, COALESCE(f.path, '') as folder_path
			FROM mailing_templates t
			LEFT JOIN mailing_template_folders f ON t.folder_id = f.id
			WHERE t.organization_id = $1
			ORDER BY f.path NULLS FIRST, t.name`
		args = []interface{}{orgID}
	} else {
		// List templates in specific folder
		query = `SELECT t.id, t.organization_id, t.folder_id, t.category_id, t.name, 
			COALESCE(t.description, '') as description, COALESCE(t.subject, '') as subject,
			COALESCE(t.from_name, '') as from_name, COALESCE(t.from_email, '') as from_email,
			COALESCE(t.reply_to, '') as reply_to, COALESCE(t.html_content, '') as html_content,
			COALESCE(t.plain_content, '') as plain_content, COALESCE(t.preview_text, '') as preview_text,
			COALESCE(t.thumbnail_url, '') as thumbnail_url, t.status, t.created_at, t.updated_at,
			COALESCE(f.name, '') as folder_name, COALESCE(f.path, '') as folder_path
			FROM mailing_templates t
			LEFT JOIN mailing_template_folders f ON t.folder_id = f.id
			WHERE t.organization_id = $1 AND t.folder_id = $2
			ORDER BY t.name`
		args = []interface{}{orgID, folderID}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list templates: %w", err)
	}
	defer rows.Close()

	var templates []*EmailTemplate
	for rows.Next() {
		template := &EmailTemplate{}
		err := rows.Scan(
			&template.ID, &template.OrganizationID, &template.FolderID, &template.CategoryID,
			&template.Name, &template.Description, &template.Subject, &template.FromName, &template.FromEmail,
			&template.ReplyTo, &template.HTMLContent, &template.PlainContent, &template.PreviewText,
			&template.ThumbnailURL, &template.Status, &template.CreatedAt, &template.UpdatedAt,
			&template.FolderName, &template.FolderPath,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan template: %w", err)
		}
		template.IsActive = template.Status == "active"
		templates = append(templates, template)
	}

	return templates, nil
}

// ListTemplatesInFolderRecursive lists all templates in a folder and its subfolders
func (s *EmailTemplateStore) ListTemplatesInFolderRecursive(ctx context.Context, orgID uuid.UUID, folderID uuid.UUID) ([]*EmailTemplate, error) {
	query := `WITH RECURSIVE folder_tree AS (
			SELECT id FROM mailing_template_folders WHERE id = $2
			UNION ALL
			SELECT f.id FROM mailing_template_folders f
			INNER JOIN folder_tree ft ON f.parent_id = ft.id
		)
		SELECT t.id, t.organization_id, t.folder_id, t.category_id, t.name, 
			COALESCE(t.description, '') as description, COALESCE(t.subject, '') as subject,
			COALESCE(t.from_name, '') as from_name, COALESCE(t.from_email, '') as from_email,
			COALESCE(t.reply_to, '') as reply_to, COALESCE(t.html_content, '') as html_content,
			COALESCE(t.plain_content, '') as plain_content, COALESCE(t.preview_text, '') as preview_text,
			COALESCE(t.thumbnail_url, '') as thumbnail_url, t.status, t.created_at, t.updated_at,
			COALESCE(f.name, '') as folder_name, COALESCE(f.path, '') as folder_path
		FROM mailing_templates t
		LEFT JOIN mailing_template_folders f ON t.folder_id = f.id
		WHERE t.organization_id = $1 AND t.folder_id IN (SELECT id FROM folder_tree)
		ORDER BY f.path, t.name`

	rows, err := s.db.QueryContext(ctx, query, orgID, folderID)
	if err != nil {
		return nil, fmt.Errorf("failed to list templates recursively: %w", err)
	}
	defer rows.Close()

	var templates []*EmailTemplate
	for rows.Next() {
		template := &EmailTemplate{}
		err := rows.Scan(
			&template.ID, &template.OrganizationID, &template.FolderID, &template.CategoryID,
			&template.Name, &template.Description, &template.Subject, &template.FromName, &template.FromEmail,
			&template.ReplyTo, &template.HTMLContent, &template.PlainContent, &template.PreviewText,
			&template.ThumbnailURL, &template.Status, &template.CreatedAt, &template.UpdatedAt,
			&template.FolderName, &template.FolderPath,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan template: %w", err)
		}
		template.IsActive = template.Status == "active"
		templates = append(templates, template)
	}

	return templates, nil
}

// UpdateTemplate updates an existing template
func (s *EmailTemplateStore) UpdateTemplate(ctx context.Context, id uuid.UUID, req *UpdateTemplateRequest) (*EmailTemplate, error) {
	// Build dynamic update query
	setClause := "updated_at = NOW()"
	args := []interface{}{}
	argIndex := 1

	if req.Name != nil {
		setClause += fmt.Sprintf(", name = $%d", argIndex)
		args = append(args, *req.Name)
		argIndex++
	}
	if req.Description != nil {
		setClause += fmt.Sprintf(", description = $%d", argIndex)
		args = append(args, *req.Description)
		argIndex++
	}
	if req.Subject != nil {
		setClause += fmt.Sprintf(", subject = $%d", argIndex)
		args = append(args, *req.Subject)
		argIndex++
	}
	if req.FromName != nil {
		setClause += fmt.Sprintf(", from_name = $%d", argIndex)
		args = append(args, *req.FromName)
		argIndex++
	}
	if req.FromEmail != nil {
		setClause += fmt.Sprintf(", from_email = $%d", argIndex)
		args = append(args, *req.FromEmail)
		argIndex++
	}
	if req.ReplyTo != nil {
		setClause += fmt.Sprintf(", reply_to = $%d", argIndex)
		args = append(args, *req.ReplyTo)
		argIndex++
	}
	if req.HTMLContent != nil {
		setClause += fmt.Sprintf(", html_content = $%d", argIndex)
		args = append(args, *req.HTMLContent)
		argIndex++
	}
	if req.PlainContent != nil {
		setClause += fmt.Sprintf(", plain_content = $%d", argIndex)
		args = append(args, *req.PlainContent)
		argIndex++
	}
	if req.PreviewText != nil {
		setClause += fmt.Sprintf(", preview_text = $%d", argIndex)
		args = append(args, *req.PreviewText)
		argIndex++
	}
	if req.ThumbnailURL != nil {
		setClause += fmt.Sprintf(", thumbnail_url = $%d", argIndex)
		args = append(args, *req.ThumbnailURL)
		argIndex++
	}
	if req.Status != nil {
		setClause += fmt.Sprintf(", status = $%d", argIndex)
		args = append(args, *req.Status)
		argIndex++
	}
	if req.FolderID != nil {
		setClause += fmt.Sprintf(", folder_id = $%d", argIndex)
		args = append(args, *req.FolderID)
		argIndex++
	}

	args = append(args, id)
	query := fmt.Sprintf("UPDATE mailing_templates SET %s WHERE id = $%d", setClause, argIndex)

	_, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to update template: %w", err)
	}

	return s.GetTemplate(ctx, id)
}

// DeleteTemplate deletes a template by ID
func (s *EmailTemplateStore) DeleteTemplate(ctx context.Context, id uuid.UUID) error {
	query := `DELETE FROM mailing_templates WHERE id = $1`
	result, err := s.db.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("failed to delete template: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("template not found")
	}

	return nil
}

// MoveTemplate moves a template to a different folder
func (s *EmailTemplateStore) MoveTemplate(ctx context.Context, id uuid.UUID, folderID *uuid.UUID) error {
	query := `UPDATE mailing_templates SET folder_id = $1, updated_at = NOW() WHERE id = $2`
	result, err := s.db.ExecContext(ctx, query, folderID, id)
	if err != nil {
		return fmt.Errorf("failed to move template: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("template not found")
	}

	return nil
}

// CloneTemplate creates a copy of a template
func (s *EmailTemplateStore) CloneTemplate(ctx context.Context, id uuid.UUID, newName string, newFolderID *uuid.UUID) (*EmailTemplate, error) {
	original, err := s.GetTemplate(ctx, id)
	if err != nil {
		return nil, err
	}
	if original == nil {
		return nil, fmt.Errorf("template not found")
	}

	folderID := newFolderID
	if folderID == nil {
		folderID = original.FolderID
	}

	req := &CreateTemplateRequest{
		FolderID:     folderID,
		Name:         newName,
		Description:  original.Description,
		Subject:      original.Subject,
		FromName:     original.FromName,
		FromEmail:    original.FromEmail,
		ReplyTo:      original.ReplyTo,
		HTMLContent:  original.HTMLContent,
		PlainContent: original.PlainContent,
		PreviewText:  original.PreviewText,
		ThumbnailURL: original.ThumbnailURL,
	}

	return s.CreateTemplate(ctx, original.OrganizationID, req)
}

// SearchTemplates searches templates by name or content
func (s *EmailTemplateStore) SearchTemplates(ctx context.Context, orgID uuid.UUID, searchTerm string) ([]*EmailTemplate, error) {
	query := `SELECT t.id, t.organization_id, t.folder_id, t.category_id, t.name, 
		COALESCE(t.description, '') as description, COALESCE(t.subject, '') as subject,
		COALESCE(t.from_name, '') as from_name, COALESCE(t.from_email, '') as from_email,
		COALESCE(t.reply_to, '') as reply_to, COALESCE(t.html_content, '') as html_content,
		COALESCE(t.plain_content, '') as plain_content, COALESCE(t.preview_text, '') as preview_text,
		COALESCE(t.thumbnail_url, '') as thumbnail_url, t.status, t.created_at, t.updated_at,
		COALESCE(f.name, '') as folder_name, COALESCE(f.path, '') as folder_path
		FROM mailing_templates t
		LEFT JOIN mailing_template_folders f ON t.folder_id = f.id
		WHERE t.organization_id = $1 AND (
			t.name ILIKE $2 OR 
			t.subject ILIKE $2 OR 
			t.description ILIKE $2
		)
		ORDER BY t.name`

	searchPattern := "%" + searchTerm + "%"
	rows, err := s.db.QueryContext(ctx, query, orgID, searchPattern)
	if err != nil {
		return nil, fmt.Errorf("failed to search templates: %w", err)
	}
	defer rows.Close()

	var templates []*EmailTemplate
	for rows.Next() {
		template := &EmailTemplate{}
		err := rows.Scan(
			&template.ID, &template.OrganizationID, &template.FolderID, &template.CategoryID,
			&template.Name, &template.Description, &template.Subject, &template.FromName, &template.FromEmail,
			&template.ReplyTo, &template.HTMLContent, &template.PlainContent, &template.PreviewText,
			&template.ThumbnailURL, &template.Status, &template.CreatedAt, &template.UpdatedAt,
			&template.FolderName, &template.FolderPath,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan template: %w", err)
		}
		template.IsActive = template.Status == "active"
		templates = append(templates, template)
	}

	return templates, nil
}
