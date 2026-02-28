// Suppression service core: struct, constructor, bootstrap, and global suppression data.
package api

import (
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"strings"

	"github.com/google/uuid"
)

// SuppressionService handles all suppression-related operations
type SuppressionService struct {
	db         *sql.DB
	optizmoKey string
	matcher    *SuppressionMatcher
}

// NewSuppressionService creates a new suppression service
func NewSuppressionService(db *sql.DB, optizmoKey string) *SuppressionService {
	svc := &SuppressionService{
		db:         db,
		optizmoKey: optizmoKey,
		matcher:    NewSuppressionMatcher(),
	}

	// Ensure tables exist
	svc.ensureTables()

	// Load existing suppression lists into memory
	go svc.loadSuppressionLists()

	return svc
}

// loadSuppressionLists loads all suppression entries into bloom filters
func (s *SuppressionService) loadSuppressionLists() {
	if s.db == nil {
		return
	}

	rows, err := s.db.Query(`
		SELECT l.id, e.md5_hash
		FROM mailing_suppression_lists l
		JOIN mailing_suppression_entries e ON l.id = e.list_id
		WHERE e.md5_hash IS NOT NULL
	`)
	if err != nil {
		log.Printf("Warning: Could not load suppression lists: %v", err)
		return
	}
	defer rows.Close()

	listHashes := make(map[string][]string)
	for rows.Next() {
		var listID, md5Hash string
		if err := rows.Scan(&listID, &md5Hash); err == nil {
			listHashes[listID] = append(listHashes[listID], md5Hash)
		}
	}

	for listID, hashes := range listHashes {
		s.matcher.LoadList(listID, hashes)
		log.Printf("Loaded suppression list %s: %d entries", listID, len(hashes))
	}
}

// ensureTables creates necessary database tables
func (s *SuppressionService) ensureTables() {
	if s.db == nil {
		return
	}

	s.db.Exec(`
		CREATE TABLE IF NOT EXISTS mailing_suppression_lists (
			id VARCHAR(100) PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			description TEXT,
			source VARCHAR(50) DEFAULT 'internal',
			optizmo_list_id VARCHAR(100),
			entry_count INTEGER DEFAULT 0,
			created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
			updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
		)
	`)

	// Add missing columns to existing tables (migrations)
	s.db.Exec(`ALTER TABLE mailing_suppression_lists ADD COLUMN IF NOT EXISTS is_global BOOLEAN DEFAULT FALSE`)
	s.db.Exec(`ALTER TABLE mailing_suppression_lists ADD COLUMN IF NOT EXISTS source VARCHAR(50) DEFAULT 'internal'`)

	s.db.Exec(`
		CREATE TABLE IF NOT EXISTS mailing_suppression_entries (
			id VARCHAR(100) PRIMARY KEY,
			list_id VARCHAR(100) REFERENCES mailing_suppression_lists(id),
			email VARCHAR(255),
			md5_hash VARCHAR(64),
			reason VARCHAR(100),
			source VARCHAR(100),
			created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
			UNIQUE(list_id, md5_hash)
		)
	`)

	// Add missing columns to existing entries table (migrations)
	s.db.Exec(`ALTER TABLE mailing_suppression_entries ADD COLUMN IF NOT EXISTS category VARCHAR(50) DEFAULT 'manual'`)
	s.db.Exec(`ALTER TABLE mailing_suppression_entries ADD COLUMN IF NOT EXISTS is_global BOOLEAN DEFAULT FALSE`)

	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_suppression_entries_hash ON mailing_suppression_entries(md5_hash)`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_suppression_entries_list ON mailing_suppression_entries(list_id)`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_suppression_entries_email ON mailing_suppression_entries(email)`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_suppression_entries_global ON mailing_suppression_entries(is_global)`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_suppression_entries_category ON mailing_suppression_entries(category)`)

	// Ensure Global Suppression List exists (industry standard)
	s.ensureGlobalSuppressionList()
}

// GlobalSuppressionCategories represents industry-standard suppression reasons
var GlobalSuppressionCategories = map[string]string{
	"hard_bounce":         "Hard Bounce - Permanent delivery failure",
	"soft_bounce_promoted": "Soft Bounce Promoted - Exceeded retry threshold",
	"spam_complaint":      "Spam Complaint - FBL/ISP complaint",
	"unsubscribe":         "Unsubscribe - User opt-out request",
	"spam_trap":           "Spam Trap - Known honeypot address",
	"role_based":          "Role-Based - Generic address (abuse@, postmaster@)",
	"known_litigator":     "Known Litigator - Legal risk address",
	"disposable":          "Disposable Email - Temporary/throwaway domain",
	"invalid":             "Invalid - Malformed or syntactically incorrect",
	"manual":              "Manual - Manually suppressed",
}

// RoleBasedPrefixes contains role-based email prefixes (industry standard)
var RoleBasedPrefixes = []string{
	"abuse", "admin", "billing", "compliance", "contact", "ftp", "help",
	"hostmaster", "info", "legal", "marketing", "mailer-daemon", "no-reply",
	"noreply", "null", "office", "postmaster", "privacy", "registrar",
	"root", "sales", "security", "spam", "support", "sysadmin",
	"tech", "unsubscribe", "usenet", "uucp", "webmaster", "www",
}

// ensureGlobalSuppressionList creates the Global Suppression List if it doesn't exist
func (s *SuppressionService) ensureGlobalSuppressionList() {
	// Check if global list exists
	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM mailing_suppression_lists WHERE is_global = TRUE`).Scan(&count)
	
	if count == 0 {
		// Create the Global Suppression List (using org_id from existing list)
		s.db.Exec(`
			INSERT INTO mailing_suppression_lists (id, organization_id, name, description, source, is_global)
			SELECT 'global-suppression-list', organization_id,
				'Global Suppression List', 
				'Industry-standard global suppression list containing hard bounces, complaints, unsubscribes, spam traps, and role-based addresses. Applied to all campaigns automatically.',
				'system', TRUE
			FROM mailing_suppression_lists 
			WHERE id != 'global-suppression-list'
			AND NOT EXISTS (SELECT 1 FROM mailing_suppression_lists WHERE id = 'global-suppression-list')
			LIMIT 1
		`)
	}
}

// AddToGlobalSuppression adds an email to the global suppression list
func (s *SuppressionService) AddToGlobalSuppression(email, category, source string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return fmt.Errorf("email cannot be empty")
	}

	// Validate category
	if _, ok := GlobalSuppressionCategories[category]; !ok {
		category = "manual"
	}

	// Compute MD5 hash
	hash := md5.Sum([]byte(email))
	md5Hash := hex.EncodeToString(hash[:])

	id := uuid.New().String()
	_, err := s.db.Exec(`
		INSERT INTO mailing_suppression_entries (id, list_id, email, md5_hash, reason, source, category, is_global)
		VALUES ($1, 'global-suppression-list', $2, $3, $4, $5, $6, TRUE)
		ON CONFLICT (list_id, md5_hash) DO UPDATE SET 
			reason = EXCLUDED.reason,
			source = EXCLUDED.source,
			category = EXCLUDED.category
	`, id, email, md5Hash, GlobalSuppressionCategories[category], source, category)

	if err != nil {
		return err
	}

	// Update entry count
	s.db.Exec(`
		UPDATE mailing_suppression_lists 
		SET entry_count = (SELECT COUNT(*) FROM mailing_suppression_entries WHERE list_id = 'global-suppression-list'),
		    updated_at = NOW()
		WHERE id = 'global-suppression-list'
	`)

	return nil
}

// IsRoleBasedEmail checks if an email is a role-based address
func IsRoleBasedEmail(email string) bool {
	parts := strings.Split(strings.ToLower(email), "@")
	if len(parts) != 2 {
		return false
	}
	localPart := parts[0]
	for _, prefix := range RoleBasedPrefixes {
		if localPart == prefix || strings.HasPrefix(localPart, prefix+"+") || strings.HasPrefix(localPart, prefix+".") {
			return true
		}
	}
	return false
}

// nullString helper for optional strings
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
