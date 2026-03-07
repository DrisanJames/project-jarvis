// Suppression service core: struct, constructor, bootstrap, and global suppression data.
package api

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"strings"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// SuppressionService handles all suppression-related operations.
// When globalHub is set, all global suppression operations delegate to it
// (single source of truth backed by mailing_global_suppressions).
// The mailing_suppression_entries table is retained only for per-list
// suppression (Optizmo, imported lists) — NOT for global suppression.
type SuppressionService struct {
	db         *sql.DB
	optizmoKey string
	matcher    *SuppressionMatcher
	globalHub  *engine.GlobalSuppressionHub
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

// SetGlobalSuppressionHub wires the centralized hub so all global
// suppression operations use the single in-memory + DB system.
func (s *SuppressionService) SetGlobalSuppressionHub(hub *engine.GlobalSuppressionHub) {
	s.globalHub = hub
	if hub != nil {
		log.Printf("[SuppressionService] GlobalSuppressionHub wired (%d entries)", hub.Count())
	} else {
		log.Printf("[SuppressionService] WARNING: SetGlobalSuppressionHub called with nil hub")
	}
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

	// DDL migrations for suppression tables moved to SQL migration files — skip at runtime

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

// AddToGlobalSuppression adds an email to the global suppression list.
// Delegates to GlobalSuppressionHub when available (single source of truth).
func (s *SuppressionService) AddToGlobalSuppression(email, category, source string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return fmt.Errorf("email cannot be empty")
	}

	if _, ok := GlobalSuppressionCategories[category]; !ok {
		category = "manual"
	}

	if s.globalHub != nil {
		reason := category
		_, err := s.globalHub.Suppress(context.Background(), email, reason, source, "", "", "", "", "")
		return err
	}

	// Fallback: direct DB write (should not happen once hub is wired)
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
	return err
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
