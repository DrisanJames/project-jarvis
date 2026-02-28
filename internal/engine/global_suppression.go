package engine

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// GlobalSuppressionHub is the single source of truth for all suppressed emails.
// Every negative signal — hard bounce, soft bounce, complaint, unsubscribe,
// FBL, inactive, manual — flows through this hub. Emails are stored with their
// MD5 hash for efficient comparison against active mailing lists.
//
// The hub aggregates from:
//   - PMTA agent SuppressionStore (per-ISP bounces/complaints/transients)
//   - Tracking pixel unsubscribes
//   - ESP bounce/complaint webhooks
//   - Campaign event tracker (inactive emails)
//   - Manual admin additions
type GlobalSuppressionHub struct {
	db    *sql.DB
	orgID string

	mu       sync.RWMutex
	hashSet  map[string]bool // MD5 hash -> suppressed (hot cache)
	emailSet map[string]bool // lowercase email -> suppressed (hot cache)

	suppressionDir string
	globalFilePath string
	remoteDir      string // Path on the PMTA server where suppression files live

	executor *Executor // Optional: if set, SCP files to PMTA after rebuild

	subMu       sync.RWMutex
	subscribers map[string]chan SuppressionEvent

	stopCh chan struct{}
}

// SetExecutor connects the hub to the PMTA executor for remote file sync.
func (h *GlobalSuppressionHub) SetExecutor(e *Executor, remoteDir string) {
	h.executor = e
	h.remoteDir = remoteDir
}

// SuppressionEvent is emitted whenever an email is added to or removed from
// the global suppression list.
type SuppressionEvent struct {
	Email     string    `json:"email"`
	MD5Hash   string    `json:"md5_hash"`
	Reason    string    `json:"reason"`
	Source    string    `json:"source"`
	ISP       string    `json:"isp,omitempty"`
	Action    string    `json:"action"` // "suppressed" or "removed"
	Timestamp time.Time `json:"timestamp"`
}

// GlobalSuppressionEntry is a single record in the global suppression table.
type GlobalSuppressionEntry struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	MD5Hash   string    `json:"md5_hash"`
	Reason    string    `json:"reason"`
	Source    string    `json:"source"`
	ISP       string    `json:"isp,omitempty"`
	DSNCode   string    `json:"dsn_code,omitempty"`
	DSNDiag   string    `json:"dsn_diag,omitempty"`
	SourceIP  string    `json:"source_ip,omitempty"`
	Campaign  string    `json:"campaign_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// GlobalSuppressionStats holds summary statistics.
type GlobalSuppressionStats struct {
	TotalSuppressed int64                      `json:"total_suppressed"`
	TodayAdded      int64                      `json:"today_added"`
	Last24hAdded    int64                      `json:"last_24h_added"`
	Last1hAdded     int64                      `json:"last_1h_added"`
	ByReason        map[string]int64           `json:"by_reason"`
	BySource        map[string]int64           `json:"by_source"`
	ByISP           map[string]int64           `json:"by_isp"`
	VelocityPerMin  float64                    `json:"velocity_per_min"`
}

// MD5Hash computes the MD5 hash of a lowercase email address.
func MD5Hash(email string) string {
	h := md5.Sum([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(h[:])
}

// NewGlobalSuppressionHub creates the hub.
func NewGlobalSuppressionHub(db *sql.DB, orgID, suppressionDir string) *GlobalSuppressionHub {
	globalFile := ""
	if suppressionDir != "" {
		globalFile = filepath.Join(suppressionDir, "global_suppression.txt")
	}
	return &GlobalSuppressionHub{
		db:             db,
		orgID:          orgID,
		hashSet:        make(map[string]bool),
		emailSet:       make(map[string]bool),
		suppressionDir: suppressionDir,
		globalFilePath: globalFile,
		subscribers:    make(map[string]chan SuppressionEvent),
		stopCh:         make(chan struct{}),
	}
}

// LoadFromDB populates the in-memory MD5 cache from the database.
func (h *GlobalSuppressionHub) LoadFromDB(ctx context.Context) error {
	rows, err := h.db.QueryContext(ctx,
		`SELECT email, md5_hash FROM mailing_global_suppressions WHERE organization_id = $1`,
		h.orgID)
	if err != nil {
		return fmt.Errorf("load global suppressions: %w", err)
	}
	defer rows.Close()

	h.mu.Lock()
	defer h.mu.Unlock()

	count := 0
	for rows.Next() {
		var email, hash string
		if err := rows.Scan(&email, &hash); err != nil {
			continue
		}
		h.emailSet[email] = true
		h.hashSet[hash] = true
		count++
	}
	log.Printf("[global-suppression] loaded %d entries from DB", count)
	return rows.Err()
}

// IsSuppressed checks if an email is on the global suppression list (O(1) in-memory).
func (h *GlobalSuppressionHub) IsSuppressed(email string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.emailSet[strings.ToLower(strings.TrimSpace(email))]
}

// IsSuppressedByHash checks if an MD5 hash is on the global suppression list.
func (h *GlobalSuppressionHub) IsSuppressedByHash(md5Hash string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.hashSet[strings.ToLower(md5Hash)]
}

// CheckBatch checks a batch of emails against the suppression list.
// Returns a map of email -> suppressed (true/false).
func (h *GlobalSuppressionHub) CheckBatch(emails []string) map[string]bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := make(map[string]bool, len(emails))
	for _, email := range emails {
		result[email] = h.emailSet[strings.ToLower(strings.TrimSpace(email))]
	}
	return result
}

// CheckBatchMD5 checks a batch of MD5 hashes against the suppression list.
// Returns a map of hash -> suppressed (true/false).
func (h *GlobalSuppressionHub) CheckBatchMD5(hashes []string) map[string]bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := make(map[string]bool, len(hashes))
	for _, hash := range hashes {
		result[hash] = h.hashSet[strings.ToLower(hash)]
	}
	return result
}

// Suppress adds an email to the global suppression list.
// This is the SINGLE entry point — all negative signals must flow through here.
func (h *GlobalSuppressionHub) Suppress(ctx context.Context, email, reason, source, isp, dsnCode, dsnDiag, sourceIP, campaign string) (bool, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return false, nil
	}

	hash := MD5Hash(email)

	// Fast check: already suppressed?
	if h.IsSuppressed(email) {
		return false, nil
	}

	_, err := h.db.ExecContext(ctx,
		`INSERT INTO mailing_global_suppressions
		(organization_id, email, md5_hash, reason, source, isp, dsn_code, dsn_diag, source_ip, campaign_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW())
		ON CONFLICT (organization_id, md5_hash) DO UPDATE SET
			reason = EXCLUDED.reason,
			source = EXCLUDED.source,
			updated_at = NOW()`,
		h.orgID, email, hash, reason, source,
		nullStr(isp), nullStr(dsnCode), nullStr(dsnDiag),
		nullStr(sourceIP), nullStr(campaign),
	)
	if err != nil {
		return false, fmt.Errorf("global suppress %s: %w", email, err)
	}

	// Update hot cache
	h.mu.Lock()
	h.emailSet[email] = true
	h.hashSet[hash] = true
	h.mu.Unlock()

	// Append to global suppression file for PMTA
	h.appendToGlobalFile(email)

	// Emit event
	event := SuppressionEvent{
		Email:     email,
		MD5Hash:   hash,
		Reason:    reason,
		Source:    source,
		ISP:       isp,
		Action:    "suppressed",
		Timestamp: time.Now(),
	}
	h.fanOut(event)

	log.Printf("[global-suppression] SUPPRESSED %s reason=%s source=%s isp=%s md5=%s", email, reason, source, isp, hash)
	return true, nil
}

// Remove removes an email from the global suppression list (admin override).
func (h *GlobalSuppressionHub) Remove(ctx context.Context, email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	hash := MD5Hash(email)

	_, err := h.db.ExecContext(ctx,
		`DELETE FROM mailing_global_suppressions WHERE organization_id = $1 AND md5_hash = $2`,
		h.orgID, hash)
	if err != nil {
		return err
	}

	h.mu.Lock()
	delete(h.emailSet, email)
	delete(h.hashSet, hash)
	h.mu.Unlock()

	h.fanOut(SuppressionEvent{
		Email:     email,
		MD5Hash:   hash,
		Action:    "removed",
		Timestamp: time.Now(),
	})

	return nil
}

// ExportMD5List returns all suppressed MD5 hashes (for external comparison).
func (h *GlobalSuppressionHub) ExportMD5List() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	hashes := make([]string, 0, len(h.hashSet))
	for hash := range h.hashSet {
		hashes = append(hashes, hash)
	}
	return hashes
}

// Count returns the total number of suppressed emails.
func (h *GlobalSuppressionHub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.emailSet)
}

// GetStats returns suppression statistics.
func (h *GlobalSuppressionHub) GetStats(ctx context.Context) (*GlobalSuppressionStats, error) {
	stats := &GlobalSuppressionStats{
		ByReason: make(map[string]int64),
		BySource: make(map[string]int64),
		ByISP:    make(map[string]int64),
	}

	h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_global_suppressions WHERE organization_id = $1`, h.orgID).Scan(&stats.TotalSuppressed)
	h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_global_suppressions WHERE organization_id = $1 AND created_at >= CURRENT_DATE`, h.orgID).Scan(&stats.TodayAdded)
	h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_global_suppressions WHERE organization_id = $1 AND created_at >= NOW() - INTERVAL '24 hours'`, h.orgID).Scan(&stats.Last24hAdded)
	h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailing_global_suppressions WHERE organization_id = $1 AND created_at >= NOW() - INTERVAL '1 hour'`, h.orgID).Scan(&stats.Last1hAdded)

	if stats.Last1hAdded > 0 {
		stats.VelocityPerMin = float64(stats.Last1hAdded) / 60.0
	}

	rows, _ := h.db.QueryContext(ctx,
		`SELECT reason, COUNT(*) FROM mailing_global_suppressions WHERE organization_id = $1 GROUP BY reason`, h.orgID)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var r string
			var c int64
			rows.Scan(&r, &c)
			stats.ByReason[r] = c
		}
	}

	rows2, _ := h.db.QueryContext(ctx,
		`SELECT source, COUNT(*) FROM mailing_global_suppressions WHERE organization_id = $1 GROUP BY source`, h.orgID)
	if rows2 != nil {
		defer rows2.Close()
		for rows2.Next() {
			var s string
			var c int64
			rows2.Scan(&s, &c)
			stats.BySource[s] = c
		}
	}

	rows3, _ := h.db.QueryContext(ctx,
		`SELECT COALESCE(isp,'unknown'), COUNT(*) FROM mailing_global_suppressions WHERE organization_id = $1 GROUP BY isp`, h.orgID)
	if rows3 != nil {
		defer rows3.Close()
		for rows3.Next() {
			var i string
			var c int64
			rows3.Scan(&i, &c)
			stats.ByISP[i] = c
		}
	}

	return stats, nil
}

// Search returns paginated suppression entries matching a query.
func (h *GlobalSuppressionHub) Search(ctx context.Context, query string, limit, offset int) ([]GlobalSuppressionEntry, int64, error) {
	var total int64
	countQ := `SELECT COUNT(*) FROM mailing_global_suppressions WHERE organization_id = $1`
	listQ := `SELECT id, email, md5_hash, reason, source, COALESCE(isp,''), COALESCE(dsn_code,''), COALESCE(dsn_diag,''), COALESCE(source_ip,''), COALESCE(campaign_id,''), created_at
		FROM mailing_global_suppressions WHERE organization_id = $1`

	args := []interface{}{h.orgID}
	if query != "" {
		isHash := len(query) == 32
		if isHash {
			countQ += ` AND md5_hash = $2`
			listQ += ` AND md5_hash = $2`
		} else {
			countQ += ` AND email ILIKE $2`
			listQ += ` AND email ILIKE $2`
			query = "%" + query + "%"
		}
		args = append(args, query)
	}

	h.db.QueryRowContext(ctx, countQ, args...).Scan(&total)

	listQ += fmt.Sprintf(` ORDER BY created_at DESC LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
	args = append(args, limit, offset)

	rows, err := h.db.QueryContext(ctx, listQ, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var entries []GlobalSuppressionEntry
	for rows.Next() {
		var e GlobalSuppressionEntry
		rows.Scan(&e.ID, &e.Email, &e.MD5Hash, &e.Reason, &e.Source, &e.ISP, &e.DSNCode, &e.DSNDiag, &e.SourceIP, &e.Campaign, &e.CreatedAt)
		entries = append(entries, e)
	}
	return entries, total, nil
}

// --- SSE Subscriptions ---

func (h *GlobalSuppressionHub) Subscribe(id string) <-chan SuppressionEvent {
	ch := make(chan SuppressionEvent, 100)
	h.subMu.Lock()
	h.subscribers[id] = ch
	h.subMu.Unlock()
	return ch
}

func (h *GlobalSuppressionHub) Unsubscribe(id string) {
	h.subMu.Lock()
	if ch, ok := h.subscribers[id]; ok {
		close(ch)
		delete(h.subscribers, id)
	}
	h.subMu.Unlock()
}

func (h *GlobalSuppressionHub) fanOut(e SuppressionEvent) {
	h.subMu.RLock()
	defer h.subMu.RUnlock()
	for _, ch := range h.subscribers {
		select {
		case ch <- e:
		default:
		}
	}
}

// --- File Sync ---

func (h *GlobalSuppressionHub) appendToGlobalFile(email string) {
	if h.globalFilePath == "" {
		return
	}
	f, err := os.OpenFile(h.globalFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, email)
}

// StartFileSync periodically rebuilds the global PMTA suppression file.
func (h *GlobalSuppressionHub) StartFileSync(ctx context.Context) {
	if h.suppressionDir == "" {
		return
	}
	os.MkdirAll(h.suppressionDir, 0755)
	go func() {
		h.rebuildGlobalFile()
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-h.stopCh:
				return
			case <-ticker.C:
				h.rebuildGlobalFile()
			}
		}
	}()
}

func (h *GlobalSuppressionHub) rebuildGlobalFile() {
	if h.globalFilePath == "" {
		return
	}
	ctx := context.Background()
	rows, err := h.db.QueryContext(ctx,
		`SELECT email FROM mailing_global_suppressions WHERE organization_id = $1 ORDER BY email`,
		h.orgID)
	if err != nil {
		log.Printf("[global-suppression] rebuild file error: %v", err)
		return
	}
	defer rows.Close()

	tmpPath := h.globalFilePath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		log.Printf("[global-suppression] create tmp file error: %v", err)
		return
	}

	count := 0
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			continue
		}
		fmt.Fprintln(f, email)
		count++
	}
	f.Close()

	if err := os.Rename(tmpPath, h.globalFilePath); err != nil {
		os.Remove(tmpPath)
		log.Printf("[global-suppression] rename error: %v", err)
		return
	}
	log.Printf("[global-suppression] rebuilt %s: %d emails", h.globalFilePath, count)

	if h.executor != nil && h.remoteDir != "" {
		remotePath := h.remoteDir + "/global_suppression.txt"
		if err := h.executor.SCPFile(h.globalFilePath, remotePath); err != nil {
			log.Printf("[global-suppression] SCP to PMTA failed: %v", err)
		} else {
			log.Printf("[global-suppression] synced %d emails to %s", count, remotePath)
		}
	}
}

// Stop terminates background goroutines.
func (h *GlobalSuppressionHub) Stop() {
	close(h.stopCh)
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
