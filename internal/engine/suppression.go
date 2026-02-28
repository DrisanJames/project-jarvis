package engine

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// GlobalSuppressionCallback is called whenever an email is suppressed,
// propagating the event to the GlobalSuppressionHub.
type GlobalSuppressionCallback func(ctx context.Context, email, reason, source, isp, dsnCode, dsnDiag, sourceIP, campaign string)

// SuppressionStore manages ISP-scoped email suppression lists in PostgreSQL
// and syncs them to PMTA suppression files on disk.
type SuppressionStore struct {
	repo           SuppressionRepository
	orgID          string
	suppressionDir string // e.g. /etc/pmta/suppressions
	mu             sync.RWMutex
	inMemory       map[ISP]map[string]bool // fast in-memory lookup
	stopCh         chan struct{}

	onGlobalSuppress GlobalSuppressionCallback
}

// NewSuppressionStore creates a new suppression store.
func NewSuppressionStore(repo SuppressionRepository, orgID, suppressionDir string) *SuppressionStore {
	ss := &SuppressionStore{
		repo:           repo,
		orgID:          orgID,
		suppressionDir: suppressionDir,
		inMemory:       make(map[ISP]map[string]bool),
		stopCh:         make(chan struct{}),
	}
	for _, isp := range AllISPs() {
		ss.inMemory[isp] = make(map[string]bool)
	}
	return ss
}

// SetGlobalSuppressionCallback registers a callback that fires on every
// new suppression, forwarding the event to the GlobalSuppressionHub.
func (ss *SuppressionStore) SetGlobalSuppressionCallback(cb GlobalSuppressionCallback) {
	ss.onGlobalSuppress = cb
}

// LoadFromDB populates the in-memory cache from the database.
func (ss *SuppressionStore) LoadFromDB(ctx context.Context) error {
	data, err := ss.repo.LoadAll(ctx, ss.orgID)
	if err != nil {
		return fmt.Errorf("load suppressions: %w", err)
	}

	ss.mu.Lock()
	defer ss.mu.Unlock()

	for _, isp := range AllISPs() {
		ss.inMemory[isp] = make(map[string]bool)
	}

	count := 0
	for isp, emails := range data {
		if _, ok := ss.inMemory[isp]; !ok {
			continue
		}
		for _, email := range emails {
			ss.inMemory[isp][strings.ToLower(email)] = true
			count++
		}
	}
	log.Printf("[suppression] loaded %d suppressions from DB", count)
	return nil
}

// IsSuppressed checks if an email is suppressed for a given ISP (in-memory).
func (ss *SuppressionStore) IsSuppressed(isp ISP, email string) bool {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	m, ok := ss.inMemory[isp]
	if !ok {
		return false
	}
	return m[strings.ToLower(email)]
}

// IsSuppressedAnyISP checks if an email is suppressed at any ISP.
func (ss *SuppressionStore) IsSuppressedAnyISP(email string) map[ISP]bool {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	result := make(map[ISP]bool)
	emailLower := strings.ToLower(email)
	for isp, m := range ss.inMemory {
		if m[emailLower] {
			result[isp] = true
		}
	}
	return result
}

// Suppress adds an email to the suppression list for an ISP.
// Returns true if the email was newly suppressed, false if already suppressed.
func (ss *SuppressionStore) Suppress(ctx context.Context, s Suppression) (bool, error) {
	s.Email = strings.ToLower(s.Email)
	s.OrganizationID = ss.orgID

	// Check in-memory first
	if ss.IsSuppressed(s.ISP, s.Email) {
		return false, nil
	}

	if err := ss.repo.Add(ctx, s); err != nil {
		return false, fmt.Errorf("insert suppression: %w", err)
	}

	// Update in-memory
	ss.mu.Lock()
	if _, ok := ss.inMemory[s.ISP]; !ok {
		ss.inMemory[s.ISP] = make(map[string]bool)
	}
	ss.inMemory[s.ISP][s.Email] = true
	ss.mu.Unlock()

	// Append to ISP suppression file incrementally
	ss.appendToFile(s.ISP, s.Email)

	// Propagate to global suppression hub (single source of truth)
	if ss.onGlobalSuppress != nil {
		ss.onGlobalSuppress(ctx, s.Email, s.Reason, "agent_"+string(s.ISP), string(s.ISP), s.DSNCode, s.DSNDiagnostic, s.SourceIP, s.CampaignID)
	}

	return true, nil
}

// Remove deletes a suppression (admin override).
func (ss *SuppressionStore) Remove(ctx context.Context, isp ISP, email string) error {
	email = strings.ToLower(email)
	if err := ss.repo.Remove(ctx, ss.orgID, isp, email); err != nil {
		return err
	}
	ss.mu.Lock()
	delete(ss.inMemory[isp], email)
	ss.mu.Unlock()
	return nil
}

// CheckDB looks up suppression details from the database.
func (ss *SuppressionStore) CheckDB(ctx context.Context, isp ISP, email string) (*Suppression, error) {
	email = strings.ToLower(email)
	return ss.repo.Get(ctx, ss.orgID, isp, email)
}

// ListByISP returns a paginated list of suppressions for an ISP.
func (ss *SuppressionStore) ListByISP(ctx context.Context, isp ISP, search string, limit, offset int) ([]Suppression, int64, error) {
	list, total, err := ss.repo.List(ctx, ss.orgID, isp, search, limit, offset)
	return list, int64(total), err
}

// GetStats returns aggregated suppression statistics for an ISP.
func (ss *SuppressionStore) GetStats(ctx context.Context, isp ISP) (*SuppressionStats, error) {
	return ss.repo.Stats(ctx, ss.orgID, isp)
}

// StartFileSync begins the background goroutine that rebuilds ISP suppression
// files from the database every 5 minutes.
func (ss *SuppressionStore) StartFileSync(ctx context.Context) {
	if ss.suppressionDir == "" {
		return
	}
	os.MkdirAll(ss.suppressionDir, 0755)
	go func() {
		ss.rebuildAllFiles()
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ss.stopCh:
				return
			case <-ticker.C:
				ss.rebuildAllFiles()
			}
		}
	}()
}

// Stop terminates background goroutines.
func (ss *SuppressionStore) Stop() {
	close(ss.stopCh)
}

func (ss *SuppressionStore) rebuildAllFiles() {
	for _, isp := range AllISPs() {
		if err := ss.rebuildFile(isp); err != nil {
			log.Printf("[suppression] rebuild file error isp=%s: %v", isp, err)
		}
	}
}

func (ss *SuppressionStore) rebuildFile(isp ISP) error {
	ctx := context.Background()
	emails, err := ss.repo.ListEmails(ctx, ss.orgID, isp)
	if err != nil {
		return err
	}

	filePath := filepath.Join(ss.suppressionDir, string(isp)+".txt")
	tmpPath := filePath + ".tmp"

	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	for _, email := range emails {
		fmt.Fprintln(f, email)
	}
	f.Close()

	if err := os.Rename(tmpPath, filePath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	log.Printf("[suppression] rebuilt %s: %d emails", filePath, len(emails))
	return nil
}

func (ss *SuppressionStore) appendToFile(isp ISP, email string) {
	if ss.suppressionDir == "" {
		return
	}
	filePath := filepath.Join(ss.suppressionDir, string(isp)+".txt")
	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, email)
}

// SuppressionFilePathForISP returns the path to an ISP's suppression file.
func (ss *SuppressionStore) SuppressionFilePathForISP(isp ISP) string {
	return filepath.Join(ss.suppressionDir, string(isp)+".txt")
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
