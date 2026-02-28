package mailing

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// TemplateFolder represents a hierarchical folder for organizing templates
type TemplateFolder struct {
	ID             uuid.UUID        `json:"id" db:"id"`
	OrganizationID uuid.UUID        `json:"organization_id" db:"organization_id"`
	ParentID       *uuid.UUID       `json:"parent_id,omitempty" db:"parent_id"`
	Name           string           `json:"name" db:"name"`
	Path           string           `json:"path" db:"path"`
	CreatedAt      time.Time        `json:"created_at" db:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at" db:"updated_at"`
	Children       []*TemplateFolder `json:"children,omitempty"`
	TemplateCount  int              `json:"template_count,omitempty"`
}

// TemplateFolderService provides operations for template folders
type TemplateFolderService struct {
	db *sql.DB
}

// NewTemplateFolderService creates a new TemplateFolderService
func NewTemplateFolderService(db *sql.DB) *TemplateFolderService {
	return &TemplateFolderService{db: db}
}

// CreateFolder creates a new template folder
func (s *TemplateFolderService) CreateFolder(ctx context.Context, orgID uuid.UUID, parentID *uuid.UUID, name string) (*TemplateFolder, error) {
	folder := &TemplateFolder{
		ID:             uuid.New(),
		OrganizationID: orgID,
		ParentID:       parentID,
		Name:           name,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}

	// Calculate path (trigger will also do this, but we need it for return)
	if parentID == nil {
		folder.Path = name
	} else {
		var parentPath string
		err := s.db.QueryRowContext(ctx, `SELECT path FROM mailing_template_folders WHERE id = $1`, parentID).Scan(&parentPath)
		if err != nil {
			return nil, fmt.Errorf("parent folder not found: %w", err)
		}
		folder.Path = parentPath + "/" + name
	}

	query := `INSERT INTO mailing_template_folders (id, organization_id, parent_id, name, path, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, organization_id, parent_id, name, path, created_at, updated_at`

	err := s.db.QueryRowContext(ctx, query,
		folder.ID, folder.OrganizationID, folder.ParentID, folder.Name, folder.Path, folder.CreatedAt, folder.UpdatedAt,
	).Scan(&folder.ID, &folder.OrganizationID, &folder.ParentID, &folder.Name, &folder.Path, &folder.CreatedAt, &folder.UpdatedAt)

	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique constraint") {
			return nil, fmt.Errorf("folder with name '%s' already exists at this level", name)
		}
		return nil, fmt.Errorf("failed to create folder: %w", err)
	}

	return folder, nil
}

// GetFolder retrieves a folder by ID
func (s *TemplateFolderService) GetFolder(ctx context.Context, id uuid.UUID) (*TemplateFolder, error) {
	query := `SELECT id, organization_id, parent_id, name, path, created_at, updated_at
		FROM mailing_template_folders WHERE id = $1`

	folder := &TemplateFolder{}
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&folder.ID, &folder.OrganizationID, &folder.ParentID, &folder.Name, &folder.Path, &folder.CreatedAt, &folder.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get folder: %w", err)
	}

	return folder, nil
}

// ListFolders returns all folders for an organization as a flat list
func (s *TemplateFolderService) ListFolders(ctx context.Context, orgID uuid.UUID) ([]*TemplateFolder, error) {
	query := `SELECT f.id, f.organization_id, f.parent_id, f.name, f.path, f.created_at, f.updated_at,
		(SELECT COUNT(*) FROM mailing_templates t WHERE t.folder_id = f.id) as template_count
		FROM mailing_template_folders f
		WHERE f.organization_id = $1
		ORDER BY f.path`

	rows, err := s.db.QueryContext(ctx, query, orgID)
	if err != nil {
		return nil, fmt.Errorf("failed to list folders: %w", err)
	}
	defer rows.Close()

	var folders []*TemplateFolder
	for rows.Next() {
		folder := &TemplateFolder{}
		err := rows.Scan(
			&folder.ID, &folder.OrganizationID, &folder.ParentID, &folder.Name, &folder.Path, 
			&folder.CreatedAt, &folder.UpdatedAt, &folder.TemplateCount,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan folder: %w", err)
		}
		folders = append(folders, folder)
	}

	return folders, nil
}

// ListFoldersTree returns folders organized in a tree structure
func (s *TemplateFolderService) ListFoldersTree(ctx context.Context, orgID uuid.UUID) ([]*TemplateFolder, error) {
	folders, err := s.ListFolders(ctx, orgID)
	if err != nil {
		return nil, err
	}

	// Build a map for quick lookup
	folderMap := make(map[uuid.UUID]*TemplateFolder)
	for _, f := range folders {
		f.Children = []*TemplateFolder{} // Initialize children slice
		folderMap[f.ID] = f
	}

	// Build tree structure
	var rootFolders []*TemplateFolder
	for _, f := range folders {
		if f.ParentID == nil {
			rootFolders = append(rootFolders, f)
		} else {
			parent, ok := folderMap[*f.ParentID]
			if ok {
				parent.Children = append(parent.Children, f)
			}
		}
	}

	return rootFolders, nil
}

// UpdateFolder updates a folder's name
func (s *TemplateFolderService) UpdateFolder(ctx context.Context, id uuid.UUID, name string) (*TemplateFolder, error) {
	query := `UPDATE mailing_template_folders SET name = $1, updated_at = NOW()
		WHERE id = $2
		RETURNING id, organization_id, parent_id, name, path, created_at, updated_at`

	folder := &TemplateFolder{}
	err := s.db.QueryRowContext(ctx, query, name, id).Scan(
		&folder.ID, &folder.OrganizationID, &folder.ParentID, &folder.Name, &folder.Path, &folder.CreatedAt, &folder.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("folder not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to update folder: %w", err)
	}

	return folder, nil
}

// MoveFolder moves a folder to a new parent
func (s *TemplateFolderService) MoveFolder(ctx context.Context, id uuid.UUID, newParentID *uuid.UUID) error {
	// Prevent moving a folder into itself or its descendants
	if newParentID != nil {
		// Check if newParentID is a descendant of id
		var isDescendant bool
		err := s.db.QueryRowContext(ctx, `
			WITH RECURSIVE descendants AS (
				SELECT id FROM mailing_template_folders WHERE id = $1
				UNION ALL
				SELECT f.id FROM mailing_template_folders f
				INNER JOIN descendants d ON f.parent_id = d.id
			)
			SELECT EXISTS(SELECT 1 FROM descendants WHERE id = $2)
		`, id, newParentID).Scan(&isDescendant)
		if err != nil {
			return fmt.Errorf("failed to check descendants: %w", err)
		}
		if isDescendant {
			return fmt.Errorf("cannot move folder into its own descendant")
		}
	}

	query := `UPDATE mailing_template_folders SET parent_id = $1, updated_at = NOW() WHERE id = $2`
	result, err := s.db.ExecContext(ctx, query, newParentID, id)
	if err != nil {
		return fmt.Errorf("failed to move folder: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("folder not found")
	}

	return nil
}

// DeleteFolder deletes a folder and all its contents (cascades to children and updates templates)
func (s *TemplateFolderService) DeleteFolder(ctx context.Context, id uuid.UUID) error {
	// The ON DELETE CASCADE handles children, and ON DELETE SET NULL handles templates
	query := `DELETE FROM mailing_template_folders WHERE id = $1`
	result, err := s.db.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("failed to delete folder: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("folder not found")
	}

	return nil
}

// GetFolderPath returns the full path string for a folder
func (s *TemplateFolderService) GetFolderPath(ctx context.Context, id uuid.UUID) (string, error) {
	var path string
	err := s.db.QueryRowContext(ctx, `SELECT path FROM mailing_template_folders WHERE id = $1`, id).Scan(&path)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("folder not found")
	}
	if err != nil {
		return "", fmt.Errorf("failed to get folder path: %w", err)
	}
	return path, nil
}

// GetFolderByPath retrieves a folder by its path
func (s *TemplateFolderService) GetFolderByPath(ctx context.Context, orgID uuid.UUID, path string) (*TemplateFolder, error) {
	query := `SELECT id, organization_id, parent_id, name, path, created_at, updated_at
		FROM mailing_template_folders WHERE organization_id = $1 AND path = $2`

	folder := &TemplateFolder{}
	err := s.db.QueryRowContext(ctx, query, orgID, path).Scan(
		&folder.ID, &folder.OrganizationID, &folder.ParentID, &folder.Name, &folder.Path, &folder.CreatedAt, &folder.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get folder by path: %w", err)
	}

	return folder, nil
}

// GetOrCreateFolderPath creates a folder path if it doesn't exist and returns the leaf folder
func (s *TemplateFolderService) GetOrCreateFolderPath(ctx context.Context, orgID uuid.UUID, path string) (*TemplateFolder, error) {
	parts := strings.Split(path, "/")
	var currentParentID *uuid.UUID
	var currentFolder *TemplateFolder

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// Try to find existing folder at this level
		var query string
		var args []interface{}
		if currentParentID == nil {
			query = `SELECT id, organization_id, parent_id, name, path, created_at, updated_at
				FROM mailing_template_folders WHERE organization_id = $1 AND name = $2 AND parent_id IS NULL`
			args = []interface{}{orgID, part}
		} else {
			query = `SELECT id, organization_id, parent_id, name, path, created_at, updated_at
				FROM mailing_template_folders WHERE organization_id = $1 AND name = $2 AND parent_id = $3`
			args = []interface{}{orgID, part, currentParentID}
		}

		folder := &TemplateFolder{}
		err := s.db.QueryRowContext(ctx, query, args...).Scan(
			&folder.ID, &folder.OrganizationID, &folder.ParentID, &folder.Name, &folder.Path, &folder.CreatedAt, &folder.UpdatedAt,
		)

		if err == sql.ErrNoRows {
			// Create the folder
			folder, err = s.CreateFolder(ctx, orgID, currentParentID, part)
			if err != nil {
				return nil, fmt.Errorf("failed to create folder '%s': %w", part, err)
			}
		} else if err != nil {
			return nil, fmt.Errorf("failed to query folder '%s': %w", part, err)
		}

		currentParentID = &folder.ID
		currentFolder = folder
	}

	return currentFolder, nil
}
