-- Migration: Template Folder System
-- Date: 2026-02-05
-- Description: Hierarchical template folder system with folder-based organization

-- ============================================
-- TEMPLATE FOLDERS (Hierarchical)
-- ============================================

CREATE TABLE IF NOT EXISTS mailing_template_folders (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    parent_id UUID REFERENCES mailing_template_folders(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    path TEXT,  -- Full path like "This Day In History/Welcome Series"
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    UNIQUE(organization_id, parent_id, name)
);

-- Handle NULL parent_id uniqueness (root level folders)
CREATE UNIQUE INDEX idx_template_folders_root_unique 
    ON mailing_template_folders(organization_id, name) 
    WHERE parent_id IS NULL;

CREATE INDEX idx_template_folders_org ON mailing_template_folders(organization_id);
CREATE INDEX idx_template_folders_parent ON mailing_template_folders(parent_id);
CREATE INDEX idx_template_folders_path ON mailing_template_folders(path);

-- ============================================
-- ADD FOLDER SUPPORT TO TEMPLATES
-- ============================================

-- Add folder_id column to existing mailing_templates table
ALTER TABLE mailing_templates 
    ADD COLUMN IF NOT EXISTS folder_id UUID REFERENCES mailing_template_folders(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_templates_folder ON mailing_templates(folder_id);

-- ============================================
-- HELPER FUNCTION: Update folder path
-- ============================================

CREATE OR REPLACE FUNCTION update_folder_path() RETURNS TRIGGER AS $$
DECLARE
    parent_path TEXT;
BEGIN
    IF NEW.parent_id IS NULL THEN
        NEW.path := NEW.name;
    ELSE
        SELECT path INTO parent_path FROM mailing_template_folders WHERE id = NEW.parent_id;
        NEW.path := parent_path || '/' || NEW.name;
    END IF;
    NEW.updated_at := NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_update_folder_path
    BEFORE INSERT OR UPDATE ON mailing_template_folders
    FOR EACH ROW
    EXECUTE FUNCTION update_folder_path();

-- ============================================
-- HELPER FUNCTION: Update child paths recursively
-- ============================================

CREATE OR REPLACE FUNCTION update_child_folder_paths() RETURNS TRIGGER AS $$
BEGIN
    -- When a folder's path changes, update all children
    IF OLD.path IS DISTINCT FROM NEW.path THEN
        UPDATE mailing_template_folders
        SET path = NEW.path || '/' || name
        WHERE parent_id = NEW.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_update_child_paths
    AFTER UPDATE ON mailing_template_folders
    FOR EACH ROW
    WHEN (OLD.path IS DISTINCT FROM NEW.path)
    EXECUTE FUNCTION update_child_folder_paths();

COMMIT;
