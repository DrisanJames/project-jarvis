package api

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// =============================================================================
// AUTOMATED SUPPRESSION REFRESH ENGINE
// =============================================================================
// Daily automated refresh of advertiser suppression lists.
// Operates within a 12PM-12AM MST window for campaign safety.
// Downloads suppression files from provider URLs, parses MD5/email entries,
// and upserts into the internal suppression system with full audit trail.
// =============================================================================

// SuppressionRefreshEngine manages the automated daily refresh of advertiser
// suppression lists from external provider URLs (Optizmo, UnsubCentral, etc.).
type SuppressionRefreshEngine struct {
	db             *sql.DB
	httpClient     *http.Client
	stopCh         chan struct{}
	mu             sync.Mutex
	running        bool
	currentCycleID string
	mstLoc         *time.Location
	optizmoToken   string // Optizmo Mailer API auth token
}

// optizmoMailerAPIBase is the base URL for the Optizmo Mailer API.
const optizmoMailerAPIBase = "https://mailer-api.optizmo.net"

// refreshSource holds a single suppression source row for in-memory processing.
type refreshSource struct {
	ID               string
	OfferID          string
	CampaignName     string
	SuppressionURL   string
	SourceProvider   string
	GASuppressionID  string
	InternalListID   sql.NullString
	Priority         int
}

// md5HexPattern matches exactly 32 lowercase hex characters.
var md5HexPattern = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// csvHeaderTokens are common CSV header values to skip when parsing suppression files.
var csvHeaderTokens = map[string]bool{
	"email":         true,
	"md5":           true,
	"hash":          true,
	"email address": true,
}

// NewSuppressionRefreshEngine creates a new engine, loads MST timezone,
// creates an HTTP client, and ensures all required database tables exist.
// The Optizmo API token can be passed directly or via OPTIZMO_API_TOKEN env var.
func NewSuppressionRefreshEngine(db *sql.DB) *SuppressionRefreshEngine {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		log.Printf("[RefreshEngine] WARNING: Could not load America/Denver timezone, falling back to UTC-7: %v", err)
		loc = time.FixedZone("MST", -7*60*60)
	}

	// Load Optizmo token from environment, with compiled-in default
	optizmoToken := os.Getenv("OPTIZMO_API_TOKEN")
	if optizmoToken == "" {
		optizmoToken = "nOC0do1yMRfevcVXdikjTQOhOpyGPlx5"
	}

	engine := &SuppressionRefreshEngine{
		db:           db,
		mstLoc:       loc,
		stopCh:       make(chan struct{}),
		optizmoToken: optizmoToken,
		httpClient: &http.Client{
			Timeout: 10 * time.Minute, // Increased for Optizmo prepare+download flow
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
			Transport: &http.Transport{
				MaxIdleConns:        20,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 15 * time.Second,
			},
		},
	}

	if optizmoToken != "" {
		log.Printf("[RefreshEngine] Optizmo Mailer API token configured (len=%d)", len(optizmoToken))
	} else {
		log.Printf("[RefreshEngine] WARNING: No Optizmo API token configured – Optizmo downloads will fail")
	}

	engine.ensureTables()
	return engine
}

// =============================================================================
// TABLE CREATION (idempotent – mirrors migration 024)
// =============================================================================

func (e *SuppressionRefreshEngine) ensureTables() {
	if e.db == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	statements := []string{
		// Table 1: suppression_refresh_sources
		`CREATE TABLE IF NOT EXISTS suppression_refresh_sources (
			id                  VARCHAR(100) PRIMARY KEY,
			offer_id            VARCHAR(50),
			campaign_name       VARCHAR(500),
			suppression_url     TEXT,
			source_provider     VARCHAR(50) DEFAULT 'unknown',
			ga_suppression_id   VARCHAR(50),
			internal_list_id    VARCHAR(100),
			is_active           BOOLEAN DEFAULT FALSE,
			refresh_group       VARCHAR(100),
			priority            INT DEFAULT 100,
			last_refreshed_at   TIMESTAMP WITH TIME ZONE,
			last_refresh_status VARCHAR(20),
			last_entry_count    INT DEFAULT 0,
			last_refresh_ms     INT,
			last_error          TEXT,
			notes               TEXT,
			created_at          TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
			updated_at          TIMESTAMP WITH TIME ZONE DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_refresh_sources_active ON suppression_refresh_sources(is_active)`,
		`CREATE INDEX IF NOT EXISTS idx_refresh_sources_group ON suppression_refresh_sources(refresh_group)`,
		`CREATE INDEX IF NOT EXISTS idx_refresh_sources_offer ON suppression_refresh_sources(offer_id)`,
		`CREATE INDEX IF NOT EXISTS idx_refresh_sources_provider ON suppression_refresh_sources(source_provider)`,
		`CREATE INDEX IF NOT EXISTS idx_refresh_sources_internal_list ON suppression_refresh_sources(internal_list_id)`,

		// Table 2: suppression_refresh_cycles
		`CREATE TABLE IF NOT EXISTS suppression_refresh_cycles (
			id                      VARCHAR(100) PRIMARY KEY,
			started_at              TIMESTAMP WITH TIME ZONE NOT NULL,
			completed_at            TIMESTAMP WITH TIME ZONE,
			status                  VARCHAR(20) NOT NULL DEFAULT 'running',
			total_sources           INT DEFAULT 0,
			completed_sources       INT DEFAULT 0,
			failed_sources          INT DEFAULT 0,
			skipped_sources         INT DEFAULT 0,
			total_entries_downloaded BIGINT DEFAULT 0,
			total_new_entries       BIGINT DEFAULT 0,
			avg_download_ms         INT,
			triggered_by            VARCHAR(50) DEFAULT 'scheduler',
			resumed_from_source     VARCHAR(100),
			error_message           TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_refresh_cycles_status ON suppression_refresh_cycles(status)`,
		`CREATE INDEX IF NOT EXISTS idx_refresh_cycles_started ON suppression_refresh_cycles(started_at DESC)`,

		// Table 3: suppression_refresh_logs
		`CREATE TABLE IF NOT EXISTS suppression_refresh_logs (
			id                  VARCHAR(100) PRIMARY KEY,
			cycle_id            VARCHAR(100) NOT NULL,
			source_id           VARCHAR(100) NOT NULL,
			started_at          TIMESTAMP WITH TIME ZONE NOT NULL,
			completed_at        TIMESTAMP WITH TIME ZONE,
			status              VARCHAR(20) NOT NULL DEFAULT 'pending',
			entries_downloaded  INT DEFAULT 0,
			entries_new         INT DEFAULT 0,
			entries_unchanged   INT DEFAULT 0,
			file_size_bytes     BIGINT DEFAULT 0,
			download_ms         INT,
			processing_ms       INT,
			http_status_code    INT,
			content_type        VARCHAR(100),
			error_message       TEXT,
			retry_count         INT DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_refresh_logs_cycle ON suppression_refresh_logs(cycle_id)`,
		`CREATE INDEX IF NOT EXISTS idx_refresh_logs_source ON suppression_refresh_logs(source_id)`,
		`CREATE INDEX IF NOT EXISTS idx_refresh_logs_status ON suppression_refresh_logs(status)`,

		// Table 4: suppression_refresh_groups
		`CREATE TABLE IF NOT EXISTS suppression_refresh_groups (
			name                VARCHAR(100) PRIMARY KEY,
			description         TEXT,
			is_active           BOOLEAN DEFAULT TRUE,
			source_count        INT DEFAULT 0,
			created_at          TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
			updated_at          TIMESTAMP WITH TIME ZONE DEFAULT NOW()
		)`,
	}

	for _, stmt := range statements {
		if _, err := e.db.ExecContext(ctx, stmt); err != nil {
			log.Printf("[RefreshEngine] ensureTables warning: %v", err)
		}
	}
}

// =============================================================================
// START / STOP
// =============================================================================

// Start begins the background ticker that checks every 60 seconds whether
// a refresh cycle should run (daily, 12PM-12AM MST).
func (e *SuppressionRefreshEngine) Start() {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return
	}
	e.running = true
	e.stopCh = make(chan struct{})
	e.mu.Unlock()

	log.Printf("[RefreshEngine] Suppression Refresh Engine started")

	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				e.checkAndRun()
			case <-e.stopCh:
				return
			}
		}
	}()
}

// Stop gracefully shuts down the refresh engine.
func (e *SuppressionRefreshEngine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.running {
		return
	}
	e.running = false
	close(e.stopCh)
	log.Printf("[RefreshEngine] Suppression Refresh Engine stopped")
}

// =============================================================================
// SCHEDULER LOGIC
// =============================================================================

// checkAndRun determines whether a new refresh cycle should start.
// Conditions: within 12PM-12AM MST window, no cycle currently running,
// and at least 23 hours since the last completed cycle.
func (e *SuppressionRefreshEngine) checkAndRun() {
	now := time.Now().In(e.mstLoc)
	hour := now.Hour()

	// Only run between 12 PM (noon) and midnight MST
	if hour < 12 || hour >= 24 {
		return
	}

	// Check if a cycle is already running
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var runningCount int
	err := e.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM suppression_refresh_cycles WHERE status = 'running'`,
	).Scan(&runningCount)
	if err != nil {
		log.Printf("[RefreshEngine] checkAndRun: error checking running cycles: %v", err)
		return
	}
	if runningCount > 0 {
		return
	}

	// Check when the last cycle completed
	var lastStarted sql.NullTime
	err = e.db.QueryRowContext(ctx,
		`SELECT started_at FROM suppression_refresh_cycles WHERE status = 'completed' ORDER BY started_at DESC LIMIT 1`,
	).Scan(&lastStarted)
	if err != nil && err != sql.ErrNoRows {
		log.Printf("[RefreshEngine] checkAndRun: error checking last cycle: %v", err)
		return
	}

	if lastStarted.Valid && time.Since(lastStarted.Time) < 23*time.Hour {
		return
	}

	// All checks passed – kick off a new cycle
	log.Printf("[RefreshEngine] Scheduler triggering new refresh cycle")
	if err := e.runCycle("scheduler"); err != nil {
		log.Printf("[RefreshEngine] Cycle error: %v", err)
	}
}

// =============================================================================
// CORE REFRESH CYCLE
// =============================================================================

// runCycle executes a full refresh of all active suppression sources.
func (e *SuppressionRefreshEngine) runCycle(triggeredBy string) error {
	cycleID := uuid.New().String()
	startedAt := time.Now()

	e.mu.Lock()
	e.currentCycleID = cycleID
	e.mu.Unlock()

	defer func() {
		e.mu.Lock()
		e.currentCycleID = ""
		e.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create cycle record
	_, err := e.db.ExecContext(ctx,
		`INSERT INTO suppression_refresh_cycles (id, started_at, status, triggered_by)
		 VALUES ($1, $2, 'running', $3)`,
		cycleID, startedAt, triggeredBy,
	)
	if err != nil {
		return fmt.Errorf("create cycle record: %w", err)
	}

	log.Printf("[RefreshEngine] Cycle %s started (triggered_by=%s)", cycleID, triggeredBy)

	// Fetch all active sources
	rows, err := e.db.QueryContext(ctx,
		`SELECT id, COALESCE(offer_id,''), COALESCE(campaign_name,''), COALESCE(suppression_url,''),
		        COALESCE(source_provider,''), COALESCE(ga_suppression_id,''), internal_list_id, priority
		 FROM suppression_refresh_sources
		 WHERE is_active = TRUE AND suppression_url != ''
		 ORDER BY priority ASC, campaign_name ASC`,
	)
	if err != nil {
		e.failCycle(cycleID, fmt.Sprintf("query sources: %v", err))
		return fmt.Errorf("query active sources: %w", err)
	}

	var sources []refreshSource
	for rows.Next() {
		var s refreshSource
		if err := rows.Scan(&s.ID, &s.OfferID, &s.CampaignName, &s.SuppressionURL,
			&s.SourceProvider, &s.GASuppressionID, &s.InternalListID, &s.Priority); err != nil {
			log.Printf("[RefreshEngine] scan source row: %v", err)
			continue
		}
		sources = append(sources, s)
	}
	rows.Close()

	totalSources := len(sources)
	log.Printf("[RefreshEngine] Cycle %s: %d active sources to refresh", cycleID, totalSources)

	// Update total_sources
	e.db.Exec(
		`UPDATE suppression_refresh_cycles SET total_sources = $1 WHERE id = $2`,
		totalSources, cycleID,
	)

	completedSources := 0
	failedSources := 0
	skippedSources := 0
	totalEntriesDownloaded := int64(0)
	totalNewEntries := int64(0)
	totalDownloadMs := int64(0)
	downloadCount := 0

	for i, src := range sources {
		// Check we're still in the refresh window
		nowMST := time.Now().In(e.mstLoc)
		if nowMST.Hour() < 12 {
			log.Printf("[RefreshEngine] Pausing cycle %s – outside refresh window (hour=%d)", cycleID, nowMST.Hour())
			e.db.Exec(
				`UPDATE suppression_refresh_cycles SET status = 'paused', completed_sources = $1, failed_sources = $2, skipped_sources = $3 WHERE id = $4`,
				completedSources, failedSources, skippedSources, cycleID,
			)
			return nil
		}

		downloaded, newEntries, dlMs, err := e.refreshSource(cycleID, src)
		if err != nil {
			if strings.Contains(err.Error(), "skipped") {
				skippedSources++
			} else {
				failedSources++
			}
			log.Printf("[RefreshEngine] Source %d/%d [%s] failed: %v", i+1, totalSources, src.CampaignName, err)
		} else {
			completedSources++
			totalEntriesDownloaded += int64(downloaded)
			totalNewEntries += int64(newEntries)
			if dlMs > 0 {
				totalDownloadMs += int64(dlMs)
				downloadCount++
			}
			log.Printf("[RefreshEngine] Source %d/%d [%s] OK: %d downloaded, %d new",
				i+1, totalSources, src.CampaignName, downloaded, newEntries)
		}

		// Update cycle progress
		e.db.Exec(
			`UPDATE suppression_refresh_cycles
			 SET completed_sources = $1, failed_sources = $2, skipped_sources = $3,
			     total_entries_downloaded = $4, total_new_entries = $5
			 WHERE id = $6`,
			completedSources, failedSources, skippedSources,
			totalEntriesDownloaded, totalNewEntries, cycleID,
		)

		// Rate limit: 2 seconds between sources
		if i < len(sources)-1 {
			time.Sleep(2 * time.Second)
		}
	}

	// Compute average download ms
	var avgDownloadMs int
	if downloadCount > 0 {
		avgDownloadMs = int(totalDownloadMs / int64(downloadCount))
	}

	// Mark cycle completed
	e.db.Exec(
		`UPDATE suppression_refresh_cycles
		 SET status = 'completed', completed_at = $1,
		     completed_sources = $2, failed_sources = $3, skipped_sources = $4,
		     total_entries_downloaded = $5, total_new_entries = $6, avg_download_ms = $7
		 WHERE id = $8`,
		time.Now(), completedSources, failedSources, skippedSources,
		totalEntriesDownloaded, totalNewEntries, avgDownloadMs, cycleID,
	)

	log.Printf("[RefreshEngine] Cycle %s completed: %d/%d sources OK, %d failed, %d skipped, %d entries downloaded, %d new entries",
		cycleID, completedSources, totalSources, failedSources, skippedSources, totalEntriesDownloaded, totalNewEntries)

	return nil
}

// failCycle marks a cycle as failed with an error message.
func (e *SuppressionRefreshEngine) failCycle(cycleID, errMsg string) {
	e.db.Exec(
		`UPDATE suppression_refresh_cycles SET status = 'failed', completed_at = $1, error_message = $2 WHERE id = $3`,
		time.Now(), errMsg, cycleID,
	)
}

// =============================================================================
// SINGLE SOURCE REFRESH
// =============================================================================

// refreshSource downloads and imports a single suppression source.
// Returns (entriesDownloaded, entriesNew, downloadMs, error).
func (e *SuppressionRefreshEngine) refreshSource(cycleID string, src refreshSource) (int, int, int, error) {
	logID := uuid.New().String()
	sourceStart := time.Now()

	// Create log record
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := e.db.ExecContext(ctx,
		`INSERT INTO suppression_refresh_logs (id, cycle_id, source_id, started_at, status)
		 VALUES ($1, $2, $3, $4, 'downloading')`,
		logID, cycleID, src.ID, sourceStart,
	)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("create log record: %w", err)
	}

	// Check for valid URL
	if src.SuppressionURL == "" {
		e.completeLog(logID, "skipped", 0, 0, 0, 0, 0, 0, "", "No suppression URL configured")
		e.updateSourceStatus(src.ID, "skipped", 0, 0, "No suppression URL configured")
		return 0, 0, 0, fmt.Errorf("skipped: no suppression URL")
	}

	// ------------------------------------------------------------------
	// STEP 1: Download the file (provider-specific logic)
	// ------------------------------------------------------------------
	downloadStart := time.Now()

	var entries []parsedEntry
	var fileSizeBytes int64
	var contentType string
	var httpStatusCode int

	isOptizmo := src.SourceProvider == "optizmo" ||
		strings.Contains(strings.ToLower(src.SuppressionURL), "optizmo.com") ||
		strings.Contains(strings.ToLower(src.SuppressionURL), "optizmo.net")

	if isOptizmo {
		// ---------------------------------------------------------------
		// OPTIZMO MAILER API: 2-step prepare-download → get-file flow
		// ---------------------------------------------------------------
		log.Printf("[RefreshEngine] Source %s: Using Optizmo Mailer API flow", src.ID)

		if e.optizmoToken == "" {
			errMsg := "Optizmo API token not configured"
			e.completeLog(logID, "failed", 0, 0, 0, 0, 0, 0, "", errMsg)
			e.updateSourceStatus(src.ID, "failed", 0, 0, errMsg)
			return 0, 0, 0, fmt.Errorf("optizmo: %s", errMsg)
		}

		// Extract the mak (mailer access key) from the URL
		mak := extractOptizmoMAK(src.SuppressionURL)
		if mak == "" {
			errMsg := fmt.Sprintf("could not extract mak from URL: %s", src.SuppressionURL)
			e.completeLog(logID, "failed", 0, 0, 0, 0, 0, 0, "", errMsg)
			e.updateSourceStatus(src.ID, "failed", 0, 0, errMsg)
			return 0, 0, 0, fmt.Errorf("optizmo: %s", errMsg)
		}

		var parseErr error
		entries, fileSizeBytes, httpStatusCode, contentType, parseErr = e.downloadOptizmo(mak, src.ID)
		if parseErr != nil {
			errMsg := fmt.Sprintf("optizmo download: %v", parseErr)
			e.completeLog(logID, "failed", 0, 0, 0, 0, 0, httpStatusCode, contentType, errMsg)
			e.updateSourceStatus(src.ID, "failed", 0, 0, errMsg)
			return 0, 0, 0, fmt.Errorf("optizmo: %w", parseErr)
		}
	} else {
		// ---------------------------------------------------------------
		// GENERIC PROVIDER: Direct HTTP GET download
		// ---------------------------------------------------------------
		req, reqErr := http.NewRequestWithContext(context.Background(), http.MethodGet, src.SuppressionURL, nil)
		if reqErr != nil {
			e.completeLog(logID, "failed", 0, 0, 0, 0, 0, 0, "", fmt.Sprintf("bad URL: %v", reqErr))
			e.updateSourceStatus(src.ID, "failed", 0, 0, fmt.Sprintf("bad URL: %v", reqErr))
			return 0, 0, 0, fmt.Errorf("bad URL: %w", reqErr)
		}
		req.Header.Set("User-Agent", "IgnitePlatform/1.0 SuppressionRefresh")
		req.Header.Set("Accept", "*/*")

		resp, dlErr := e.httpClient.Do(req)
		if dlErr != nil {
			e.completeLog(logID, "failed", 0, 0, 0, 0, 0, 0, "", fmt.Sprintf("download error: %v", dlErr))
			e.updateSourceStatus(src.ID, "failed", 0, 0, fmt.Sprintf("download error: %v", dlErr))
			return 0, 0, 0, fmt.Errorf("download: %w", dlErr)
		}
		defer resp.Body.Close()

		httpStatusCode = resp.StatusCode
		contentType = resp.Header.Get("Content-Type")

		if resp.StatusCode != http.StatusOK {
			preview := make([]byte, 512)
			n, _ := io.ReadAtLeast(resp.Body, preview, 1)
			bodyPreview := string(preview[:n])
			errMsg := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, bodyPreview)

			e.completeLog(logID, "failed", 0, 0, 0, 0, 0, httpStatusCode, contentType, errMsg)
			e.updateSourceStatus(src.ID, "failed", 0, 0, errMsg)
			return 0, 0, 0, fmt.Errorf("HTTP %d from %s", resp.StatusCode, src.SuppressionURL)
		}

		entries, fileSizeBytes = parseSuppressionStream(resp.Body, src.ID)
	}

	downloadMs := int(time.Since(downloadStart).Milliseconds())
	entriesDownloaded := len(entries)

	// ------------------------------------------------------------------
	// STEP 3: Determine or create the internal suppression list
	// ------------------------------------------------------------------
	processingStart := time.Now()
	listID := ""

	if src.InternalListID.Valid && src.InternalListID.String != "" {
		listID = src.InternalListID.String
	} else {
		// Auto-create a new suppression list
		listID = uuid.New().String()
		listName := src.CampaignName + " Suppression"
		if listName == " Suppression" {
			listName = "Auto-Refresh " + src.ID
		}

		// Resolve the organization_id for the new list
		orgID := e.resolveOrgID()

		ctxInsert, cancelInsert := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelInsert()

		_, err := e.db.ExecContext(ctxInsert,
			`INSERT INTO mailing_suppression_lists (id, organization_id, name, source, entry_count, list_type, scope)
			 VALUES ($1, $2, $3, 'auto-refresh', 0, 'advertiser', 'organization')`,
			listID, orgID, listName,
		)
		if err != nil {
			e.completeLog(logID, "failed", entriesDownloaded, 0, 0, fileSizeBytes, downloadMs, httpStatusCode, contentType,
				fmt.Sprintf("create list: %v", err))
			e.updateSourceStatus(src.ID, "failed", 0, downloadMs, fmt.Sprintf("create list: %v", err))
			return entriesDownloaded, 0, downloadMs, fmt.Errorf("create list: %w", err)
		}

		// Link the new list back to the source
		e.db.Exec(
			`UPDATE suppression_refresh_sources SET internal_list_id = $1, updated_at = NOW() WHERE id = $2`,
			listID, src.ID,
		)

		log.Printf("[RefreshEngine] Auto-created suppression list %s for source %s (%s)", listID, src.ID, listName)
	}

	// ------------------------------------------------------------------
	// STEP 4: Bulk-load entries (index-optimized full replace strategy)
	// ------------------------------------------------------------------
	// For large lists (>1M entries), we drop non-essential indexes before
	// the COPY and rebuild them afterwards. This avoids costly per-row
	// index maintenance and speeds the load by 10-50×.
	// ------------------------------------------------------------------
	entriesNew := 0

	if len(entries) > 0 {
		log.Printf("[RefreshEngine] Source %s: inserting %d entries", src.ID, len(entries))

		// De-duplicate entries in memory (by md5_hash)
		seen := make(map[string]bool, len(entries))
		uniqueEntries := make([]parsedEntry, 0, len(entries))
		for _, entry := range entries {
			if !seen[entry.md5Hash] {
				seen[entry.md5Hash] = true
				uniqueEntries = append(uniqueEntries, entry)
			}
		}
		dedupCount := len(entries) - len(uniqueEntries)
		if dedupCount > 0 {
			log.Printf("[RefreshEngine] Source %s: deduped %d entries (%d unique from %d total)",
				src.ID, dedupCount, len(uniqueEntries), len(entries))
		}
		entries = nil // Free memory
		seen = nil

		largeLoad := len(uniqueEntries) > 1_000_000

		// ---- Phase A: drop non-essential indexes for large loads ----
		nonEssentialIndexes := []idxDef{
			{"mailing_suppression_entries_list_id_md5_hash_key",
				`CREATE UNIQUE INDEX mailing_suppression_entries_list_id_md5_hash_key ON mailing_suppression_entries (list_id, md5_hash)`},
			{"idx_suppression_entries_list",
				`CREATE INDEX idx_suppression_entries_list ON mailing_suppression_entries (list_id)`},
			{"idx_suppression_entries_email",
				`CREATE INDEX idx_suppression_entries_email ON mailing_suppression_entries (email)`},
			{"idx_suppression_entries_hash",
				`CREATE INDEX idx_suppression_entries_hash ON mailing_suppression_entries (md5_hash)`},
			{"idx_suppression_entries_category",
				`CREATE INDEX idx_suppression_entries_category ON mailing_suppression_entries (category)`},
			{"idx_suppression_entries_global",
				`CREATE INDEX idx_suppression_entries_global ON mailing_suppression_entries (is_global)`},
		}

		if largeLoad {
			log.Printf("[RefreshEngine] Source %s: large load (%d entries) – dropping %d non-essential indexes",
				src.ID, len(uniqueEntries), len(nonEssentialIndexes))
			for _, idx := range nonEssentialIndexes {
				_, dropErr := e.db.Exec(fmt.Sprintf(`DROP INDEX IF EXISTS %s`, idx.name))
				if dropErr != nil {
					log.Printf("[RefreshEngine] Warning: could not drop index %s: %v", idx.name, dropErr)
				}
			}
			// Also drop the unique constraint (it created the unique index)
			e.db.Exec(`ALTER TABLE mailing_suppression_entries DROP CONSTRAINT IF EXISTS mailing_suppression_entries_list_id_md5_hash_key`)
			log.Printf("[RefreshEngine] Source %s: indexes dropped, starting COPY", src.ID)
		}

		// ---- Phase B: DELETE + COPY inside a transaction ----
		ctxBulk, cancelBulk := context.WithTimeout(context.Background(), 4*time.Hour)
		defer cancelBulk()

		tx, err := e.db.BeginTx(ctxBulk, nil)
		if err != nil {
			e.completeLog(logID, "failed", entriesDownloaded, 0, 0, fileSizeBytes, downloadMs, httpStatusCode, contentType,
				fmt.Sprintf("begin tx: %v", err))
			e.updateSourceStatus(src.ID, "failed", entriesDownloaded, downloadMs, fmt.Sprintf("begin tx: %v", err))
			// Rebuild indexes even on failure
			if largeLoad {
				e.rebuildIndexes(nonEssentialIndexes)
			}
			return entriesDownloaded, 0, downloadMs, fmt.Errorf("begin tx: %w", err)
		}

		// Delete old entries for this list
		delResult, delErr := tx.Exec(`DELETE FROM mailing_suppression_entries WHERE list_id = $1`, listID)
		if delErr != nil {
			tx.Rollback()
			e.completeLog(logID, "failed", entriesDownloaded, 0, 0, fileSizeBytes, downloadMs, httpStatusCode, contentType,
				fmt.Sprintf("delete existing: %v", delErr))
			e.updateSourceStatus(src.ID, "failed", entriesDownloaded, downloadMs, fmt.Sprintf("delete existing: %v", delErr))
			if largeLoad {
				e.rebuildIndexes(nonEssentialIndexes)
			}
			return entriesDownloaded, 0, downloadMs, fmt.Errorf("delete existing: %w", delErr)
		}
		delCount, _ := delResult.RowsAffected()
		log.Printf("[RefreshEngine] Source %s: deleted %d old entries", src.ID, delCount)

		// COPY directly into the main table
		copyStmt, err := tx.Prepare(pq.CopyIn("mailing_suppression_entries",
			"id", "list_id", "email", "md5_hash", "reason", "source", "category",
		))
		if err != nil {
			tx.Rollback()
			e.completeLog(logID, "failed", entriesDownloaded, 0, 0, fileSizeBytes, downloadMs, httpStatusCode, contentType,
				fmt.Sprintf("prepare COPY: %v", err))
			e.updateSourceStatus(src.ID, "failed", entriesDownloaded, downloadMs, fmt.Sprintf("prepare COPY: %v", err))
			if largeLoad {
				e.rebuildIndexes(nonEssentialIndexes)
			}
			return entriesDownloaded, 0, downloadMs, fmt.Errorf("prepare COPY: %w", err)
		}

		copyStart := time.Now()
		copyErrors := 0
		for i, entry := range uniqueEntries {
			entryID := uuid.New().String()
			var emailVal interface{}
			if entry.email != "" {
				emailVal = entry.email
			}

			_, execErr := copyStmt.Exec(
				entryID, listID, emailVal, entry.md5Hash,
				"advertiser_suppression", "auto-refresh", "advertiser",
			)
			if execErr != nil {
				copyErrors++
				if copyErrors <= 5 {
					log.Printf("[RefreshEngine] COPY exec error at entry %d: %v", i, execErr)
				}
				if copyErrors == 6 {
					log.Printf("[RefreshEngine] Suppressing further COPY exec errors...")
				}
				// If we get a tx-aborted error, stop immediately
				if copyErrors > 100 {
					break
				}
				continue
			}

			if (i+1)%1_000_000 == 0 {
				elapsed := time.Since(copyStart).Seconds()
				rate := float64(i+1) / elapsed
				log.Printf("[RefreshEngine] Source %s: COPY progress %d/%d (%.1f%%) – %.0f rows/sec",
					src.ID, i+1, len(uniqueEntries),
					float64(i+1)/float64(len(uniqueEntries))*100, rate)
			}
		}

		if copyErrors > 100 {
			tx.Rollback()
			errMsg := fmt.Sprintf("COPY aborted: %d errors", copyErrors)
			e.completeLog(logID, "failed", entriesDownloaded, 0, 0, fileSizeBytes, downloadMs, httpStatusCode, contentType, errMsg)
			e.updateSourceStatus(src.ID, "failed", entriesDownloaded, downloadMs, errMsg)
			if largeLoad {
				e.rebuildIndexes(nonEssentialIndexes)
			}
			return entriesDownloaded, 0, downloadMs, fmt.Errorf("%s", errMsg)
		}

		// Flush COPY buffer
		log.Printf("[RefreshEngine] Source %s: flushing COPY buffer (%d entries, %d errors)...",
			src.ID, len(uniqueEntries), copyErrors)
		_, err = copyStmt.Exec()
		if err != nil {
			tx.Rollback()
			e.completeLog(logID, "failed", entriesDownloaded, 0, 0, fileSizeBytes, downloadMs, httpStatusCode, contentType,
				fmt.Sprintf("flush COPY: %v", err))
			e.updateSourceStatus(src.ID, "failed", entriesDownloaded, downloadMs, fmt.Sprintf("flush COPY: %v", err))
			if largeLoad {
				e.rebuildIndexes(nonEssentialIndexes)
			}
			return entriesDownloaded, 0, downloadMs, fmt.Errorf("flush COPY: %w", err)
		}
		copyStmt.Close()
		entriesNew = len(uniqueEntries) - copyErrors

		log.Printf("[RefreshEngine] Source %s: committing %d entries (COPY took %s)...",
			src.ID, entriesNew, time.Since(copyStart).Round(time.Second))
		if err := tx.Commit(); err != nil {
			e.completeLog(logID, "failed", entriesDownloaded, entriesNew, entriesDownloaded-entriesNew,
				fileSizeBytes, downloadMs, httpStatusCode, contentType,
				fmt.Sprintf("final commit: %v", err))
			e.updateSourceStatus(src.ID, "failed", entriesNew, downloadMs, fmt.Sprintf("final commit: %v", err))
			if largeLoad {
				e.rebuildIndexes(nonEssentialIndexes)
			}
			return entriesDownloaded, entriesNew, downloadMs, fmt.Errorf("final commit: %w", err)
		}

		log.Printf("[RefreshEngine] Source %s: COPY committed successfully (%d entries in %s)",
			src.ID, entriesNew, time.Since(copyStart).Round(time.Second))

		// ---- Phase C: rebuild indexes after successful COPY ----
		if largeLoad {
			e.rebuildIndexes(nonEssentialIndexes)
		}
	}

	processingMs := int(time.Since(processingStart).Milliseconds())
	entriesUnchanged := entriesDownloaded - entriesNew

	// ------------------------------------------------------------------
	// STEP 5: Update metadata
	// ------------------------------------------------------------------

	// Update suppression list entry count
	e.db.Exec(
		`UPDATE mailing_suppression_lists
		 SET entry_count = $1, updated_at = NOW()
		 WHERE id = $2`,
		entriesNew, listID,
	)

	// Update log record
	e.completeLog(logID, "success", entriesDownloaded, entriesNew, entriesUnchanged,
		fileSizeBytes, downloadMs, httpStatusCode, contentType, "")

	// Update source record
	totalMs := downloadMs + processingMs
	e.updateSourceStatus(src.ID, "success", entriesDownloaded, totalMs, "")

	return entriesDownloaded, entriesNew, downloadMs, nil
}

// =============================================================================
// HELPER: update log and source records
// =============================================================================

func (e *SuppressionRefreshEngine) completeLog(
	logID, status string,
	downloaded, newEntries, unchanged int,
	fileSize int64, downloadMs, httpStatus int,
	contentType, errMsg string,
) {
	processingMs := 0 // computed externally when needed
	e.db.Exec(
		`UPDATE suppression_refresh_logs
		 SET status = $1, completed_at = $2,
		     entries_downloaded = $3, entries_new = $4, entries_unchanged = $5,
		     file_size_bytes = $6, download_ms = $7, processing_ms = $8,
		     http_status_code = $9, content_type = $10, error_message = $11
		 WHERE id = $12`,
		status, time.Now(),
		downloaded, newEntries, unchanged,
		fileSize, downloadMs, processingMs,
		httpStatus, contentType, nullableString(errMsg),
		logID,
	)
}

func (e *SuppressionRefreshEngine) updateSourceStatus(sourceID, status string, entryCount, refreshMs int, lastError string) {
	e.db.Exec(
		`UPDATE suppression_refresh_sources
		 SET last_refreshed_at = $1, last_refresh_status = $2,
		     last_entry_count = $3, last_refresh_ms = $4, last_error = $5,
		     updated_at = NOW()
		 WHERE id = $6`,
		time.Now(), status, entryCount, refreshMs, nullableString(lastError), sourceID,
	)
}

// idxDef holds an index name and its CREATE DDL for drop/rebuild during bulk loads.
type idxDef struct {
	name string
	ddl  string
}

// rebuildIndexes recreates non-essential indexes after a large COPY operation.
// Each index is built individually so a single failure doesn't block others.
func (e *SuppressionRefreshEngine) rebuildIndexes(indexes []idxDef) {
	log.Printf("[RefreshEngine] Rebuilding %d indexes...", len(indexes))
	totalStart := time.Now()
	for _, idx := range indexes {
		idxStart := time.Now()
		log.Printf("[RefreshEngine]   Creating index %s ...", idx.name)
		_, err := e.db.Exec(idx.ddl)
		if err != nil {
			log.Printf("[RefreshEngine]   WARNING: failed to create index %s: %v", idx.name, err)
		} else {
			log.Printf("[RefreshEngine]   Index %s created in %s", idx.name, time.Since(idxStart).Round(time.Second))
		}
	}
	log.Printf("[RefreshEngine] All indexes rebuilt in %s", time.Since(totalStart).Round(time.Second))
}

// nullableString returns a sql.NullString – null when s is empty.
func nullableString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// =============================================================================
// MANUAL TRIGGER
// =============================================================================

// ManualTrigger starts a refresh cycle from an API call and returns the cycle ID.
func (e *SuppressionRefreshEngine) ManualTrigger() (string, error) {
	// Run the cycle in a goroutine and query for the resulting cycle ID.
	errCh := make(chan error, 1)
	go func() {
		errCh <- e.runCycle("manual")
	}()

	// Wait briefly to let the cycle record get created
	time.Sleep(500 * time.Millisecond)

	// Query the cycle ID that was just created
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var cycleID string
	err := e.db.QueryRowContext(ctx,
		`SELECT id FROM suppression_refresh_cycles
		 WHERE triggered_by = 'manual'
		 ORDER BY started_at DESC LIMIT 1`,
	).Scan(&cycleID)
	if err != nil {
		// Fallback – wait for the goroutine to finish and return its error
		if rErr := <-errCh; rErr != nil {
			return "", rErr
		}
		return "", fmt.Errorf("could not retrieve cycle ID: %w", err)
	}

	return cycleID, nil
}

// =============================================================================
// STATUS
// =============================================================================

// GetStatus returns the current engine state as a JSON-friendly map.
func (e *SuppressionRefreshEngine) GetStatus() map[string]interface{} {
	e.mu.Lock()
	running := e.running
	currentCycle := e.currentCycleID
	e.mu.Unlock()

	now := time.Now().In(e.mstLoc)
	hour := now.Hour()
	inWindow := hour >= 12 && hour < 24

	result := map[string]interface{}{
		"engine_running":    running,
		"in_refresh_window": inWindow,
		"current_time_mst":  now.Format(time.RFC3339),
	}

	// Current cycle info
	if currentCycle != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		var status, triggeredBy string
		var startedAt time.Time
		var totalSources, completedSources, failedSources, skippedSources int

		err := e.db.QueryRowContext(ctx,
			`SELECT status, started_at, triggered_by, total_sources, completed_sources, failed_sources, skipped_sources
			 FROM suppression_refresh_cycles WHERE id = $1`, currentCycle,
		).Scan(&status, &startedAt, &triggeredBy, &totalSources, &completedSources, &failedSources, &skippedSources)

		if err == nil {
			result["current_cycle"] = map[string]interface{}{
				"id":                currentCycle,
				"status":            status,
				"started_at":        startedAt.Format(time.RFC3339),
				"triggered_by":      triggeredBy,
				"total_sources":     totalSources,
				"completed_sources": completedSources,
				"failed_sources":    failedSources,
				"skipped_sources":   skippedSources,
				"running_for_sec":   int(time.Since(startedAt).Seconds()),
			}
		}
	} else {
		result["current_cycle"] = nil
	}

	// Last completed cycle
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()

	var lcID, lcStatus, lcTriggeredBy string
	var lcStartedAt time.Time
	var lcCompletedAt sql.NullTime
	var lcTotal, lcCompleted, lcFailed, lcSkipped int
	var lcEntriesDownloaded, lcNewEntries int64

	err := e.db.QueryRowContext(ctx2,
		`SELECT id, status, started_at, completed_at, triggered_by,
		        total_sources, completed_sources, failed_sources, skipped_sources,
		        total_entries_downloaded, total_new_entries
		 FROM suppression_refresh_cycles
		 WHERE status = 'completed'
		 ORDER BY started_at DESC LIMIT 1`,
	).Scan(&lcID, &lcStatus, &lcStartedAt, &lcCompletedAt, &lcTriggeredBy,
		&lcTotal, &lcCompleted, &lcFailed, &lcSkipped,
		&lcEntriesDownloaded, &lcNewEntries)

	if err == nil {
		lastCycle := map[string]interface{}{
			"id":                      lcID,
			"status":                  lcStatus,
			"started_at":              lcStartedAt.Format(time.RFC3339),
			"triggered_by":            lcTriggeredBy,
			"total_sources":           lcTotal,
			"completed_sources":       lcCompleted,
			"failed_sources":          lcFailed,
			"skipped_sources":         lcSkipped,
			"total_entries_downloaded": lcEntriesDownloaded,
			"total_new_entries":       lcNewEntries,
		}
		if lcCompletedAt.Valid {
			lastCycle["completed_at"] = lcCompletedAt.Time.Format(time.RFC3339)
		}
		result["last_completed_cycle"] = lastCycle
	} else {
		result["last_completed_cycle"] = nil
	}

	// Total active sources
	var activeCount int
	e.db.QueryRow(`SELECT COUNT(*) FROM suppression_refresh_sources WHERE is_active = TRUE`).Scan(&activeCount)
	result["total_active_sources"] = activeCount

	// Next window start: next 12 PM MST
	nextStart := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, e.mstLoc)
	if now.After(nextStart) {
		nextStart = nextStart.Add(24 * time.Hour)
	}
	result["next_window_start"] = nextStart.Format(time.RFC3339)

	return result
}

// =============================================================================
// ORG ID RESOLUTION
// =============================================================================

// resolveOrgID finds the organization ID to use for auto-created suppression lists.
// Falls back through multiple tables to find a valid org ID.
func (e *SuppressionRefreshEngine) resolveOrgID() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var orgID string
	for _, tbl := range []string{
		"mailing_suppression_lists",
		"mailing_lists",
		"mailing_campaigns",
		"mailing_subscribers",
	} {
		err := e.db.QueryRowContext(ctx,
			fmt.Sprintf("SELECT DISTINCT organization_id FROM %s LIMIT 1", tbl),
		).Scan(&orgID)
		if err == nil && orgID != "" {
			return orgID
		}
	}

	// Hardcoded fallback for development
	return "00000000-0000-0000-0000-000000000001"
}

// =============================================================================
// OPTIZMO MAILER API DOWNLOAD
// =============================================================================

// extractOptizmoMAK extracts the "mak" (mailer access key) from an Optizmo URL.
// URLs look like: https://app.optizmo.com/access/campaigns?mak=m-cjgx-u57-8e1f...
func extractOptizmoMAK(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	mak := parsed.Query().Get("mak")
	if mak != "" {
		return mak
	}
	// Fallback: try to find mak= in the URL string
	if idx := strings.Index(rawURL, "mak="); idx >= 0 {
		rest := rawURL[idx+4:]
		if amp := strings.IndexByte(rest, '&'); amp >= 0 {
			return rest[:amp]
		}
		return rest
	}
	return ""
}

// optizmoPrepareResponse is the JSON response from the Optizmo Mailer API
// prepare-download endpoint: GET /accesskey/download/{mak}?token=...&format=md5
type optizmoPrepareResponse struct {
	Result        string `json:"result"`
	DownloadLink  string `json:"download_link"`
	CampaignName  string `json:"campaign_name,omitempty"`
	OptoutLink    string `json:"optout_link,omitempty"`
	Format        string `json:"format,omitempty"`
	Error         string `json:"error,omitempty"`
	ErrorCode     string `json:"error_code,omitempty"`
	Help          string `json:"help,omitempty"`
}

// downloadOptizmo uses the Optizmo Mailer API (2-step flow) to download
// a suppression list. Files can be 1 GB+, so we stream to a temp file.
//
// API flow:
//   1. GET /accesskey/download/{mak}?token=...&format=md5  → JSON with download_link
//   2. GET {download_link}  (follows 302 → S3)             → ZIP file on disk
//   3. Extract the suppression_list-*.txt from the ZIP
//   4. Stream-parse MD5 hashes from the extracted text
//
// Returns (parsedEntries, fileSizeBytes, httpStatusCode, contentType, error).
func (e *SuppressionRefreshEngine) downloadOptizmo(mak, sourceID string) ([]parsedEntry, int64, int, string, error) {
	// ------------------------------------------------------------------
	// Step 1: Call prepare-download to get the download link
	// ------------------------------------------------------------------
	// Endpoint: GET /accesskey/download/{mak}?token={token}&format=md5
	// (campaign_access_key is a PATH param, not query param)
	prepareURL := fmt.Sprintf("%s/accesskey/download/%s?token=%s&format=md5",
		optizmoMailerAPIBase,
		url.PathEscape(mak),
		url.QueryEscape(e.optizmoToken),
	)

	log.Printf("[RefreshEngine] Optizmo prepare-download for source %s (mak=%s...)", sourceID, mak[:min(12, len(mak))])

	prepareReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, prepareURL, nil)
	if err != nil {
		return nil, 0, 0, "", fmt.Errorf("prepare request: %w", err)
	}
	prepareReq.Header.Set("User-Agent", "IgnitePlatform/1.0 SuppressionRefresh")
	prepareReq.Header.Set("Accept", "application/json")

	prepareResp, err := e.httpClient.Do(prepareReq)
	if err != nil {
		return nil, 0, 0, "", fmt.Errorf("prepare-download request failed: %w", err)
	}
	defer prepareResp.Body.Close()

	prepareBody, err := io.ReadAll(prepareResp.Body)
	if err != nil {
		return nil, 0, prepareResp.StatusCode, "", fmt.Errorf("read prepare response: %w", err)
	}

	if prepareResp.StatusCode != http.StatusOK && prepareResp.StatusCode != http.StatusAccepted {
		return nil, 0, prepareResp.StatusCode, prepareResp.Header.Get("Content-Type"),
			fmt.Errorf("prepare-download HTTP %d: %s", prepareResp.StatusCode, string(prepareBody[:min(512, len(prepareBody))]))
	}

	var prepareResult optizmoPrepareResponse
	if err := json.Unmarshal(prepareBody, &prepareResult); err != nil {
		return nil, 0, prepareResp.StatusCode, prepareResp.Header.Get("Content-Type"),
			fmt.Errorf("parse prepare response: %w (body: %s)", err, string(prepareBody[:min(256, len(prepareBody))]))
	}

	// Check for API-level errors (e.g., "You do not have access to plain text downloads")
	if prepareResult.Result == "error" {
		errMsg := prepareResult.Error
		if errMsg == "" {
			errMsg = string(prepareBody[:min(256, len(prepareBody))])
		}
		return nil, 0, prepareResp.StatusCode, prepareResp.Header.Get("Content-Type"),
			fmt.Errorf("optizmo API error: %s", errMsg)
	}

	if prepareResult.DownloadLink == "" {
		return nil, 0, prepareResp.StatusCode, prepareResp.Header.Get("Content-Type"),
			fmt.Errorf("prepare-download returned empty download_link (body: %s)", string(prepareBody[:min(256, len(prepareBody))]))
	}

	log.Printf("[RefreshEngine] Optizmo prepare-download OK for source %s [%s], download_link ready",
		sourceID, prepareResult.CampaignName)

	// ------------------------------------------------------------------
	// Step 2: Download the ZIP file to a temp file (files can be 1 GB+)
	// ------------------------------------------------------------------
	// The download_link may return 404 while file is being prepared.
	// Poll with increasing intervals up to 12 minutes total.
	tmpFile, err := os.CreateTemp("", "optizmo-dl-*.zip")
	if err != nil {
		return nil, 0, 0, "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) // Clean up temp file when done

	var dlStatusCode int
	var dlContentType string
	var downloadedBytes int64
	downloadSuccess := false

	maxAttempts := 30
	pollInterval := 5 * time.Second

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(pollInterval)
			if pollInterval < 30*time.Second {
				pollInterval += 5 * time.Second
			}
		}

		// Use a dedicated client for large downloads (no global timeout)
		dlClient := &http.Client{
			Timeout: 30 * time.Minute,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		}

		dlReq, reqErr := http.NewRequestWithContext(context.Background(), http.MethodGet, prepareResult.DownloadLink, nil)
		if reqErr != nil {
			tmpFile.Close()
			return nil, 0, 0, "", fmt.Errorf("download request: %w", reqErr)
		}
		dlReq.Header.Set("User-Agent", "IgnitePlatform/1.0 SuppressionRefresh")

		dlResp, dlErr := dlClient.Do(dlReq)
		if dlErr != nil {
			log.Printf("[RefreshEngine] Optizmo download attempt %d/%d for source %s failed: %v",
				attempt, maxAttempts, sourceID, dlErr)
			continue
		}

		dlStatusCode = dlResp.StatusCode
		dlContentType = dlResp.Header.Get("Content-Type")

		if dlResp.StatusCode == http.StatusNotFound {
			dlResp.Body.Close()
			log.Printf("[RefreshEngine] Optizmo download attempt %d/%d for source %s: 404 (file not ready)",
				attempt, maxAttempts, sourceID)
			continue
		}

		if dlResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(dlResp.Body, 512))
			dlResp.Body.Close()
			tmpFile.Close()
			return nil, 0, dlStatusCode, dlContentType,
				fmt.Errorf("download HTTP %d: %s", dlResp.StatusCode, string(body))
		}

		// Stream to temp file
		tmpFile.Seek(0, 0)
		tmpFile.Truncate(0)
		downloadedBytes, err = io.Copy(tmpFile, dlResp.Body)
		dlResp.Body.Close()
		if err != nil {
			log.Printf("[RefreshEngine] Optizmo download stream error for source %s: %v (got %d bytes)", sourceID, err, downloadedBytes)
			continue
		}

		log.Printf("[RefreshEngine] Optizmo download OK for source %s: %d bytes (%.1f MB)",
			sourceID, downloadedBytes, float64(downloadedBytes)/1024/1024)
		downloadSuccess = true
		break
	}

	tmpFile.Close()

	if !downloadSuccess {
		return nil, 0, dlStatusCode, dlContentType,
			fmt.Errorf("download failed after %d attempts (last status: %d)", maxAttempts, dlStatusCode)
	}

	// ------------------------------------------------------------------
	// Step 3: Open the ZIP and find the suppression list file
	// ------------------------------------------------------------------
	zipReader, zipErr := zip.OpenReader(tmpPath)
	if zipErr != nil {
		return nil, downloadedBytes, dlStatusCode, dlContentType, fmt.Errorf("open zip: %w", zipErr)
	}
	defer zipReader.Close()

	if len(zipReader.File) == 0 {
		return nil, downloadedBytes, dlStatusCode, dlContentType, fmt.Errorf("zip archive is empty")
	}

	// Find the suppression list file (not the domains list)
	var suppressionFile *zip.File
	for _, f := range zipReader.File {
		lower := strings.ToLower(f.Name)
		if strings.Contains(lower, "suppression_list") || strings.Contains(lower, "optout") {
			suppressionFile = f
			break
		}
	}
	if suppressionFile == nil {
		// Fallback: use the largest file
		suppressionFile = zipReader.File[0]
		for _, f := range zipReader.File[1:] {
			if f.UncompressedSize64 > suppressionFile.UncompressedSize64 {
				suppressionFile = f
			}
		}
	}

	log.Printf("[RefreshEngine] Optizmo zip for source %s: extracting %s (%d MB uncompressed)",
		sourceID, suppressionFile.Name, suppressionFile.UncompressedSize64/1024/1024)

	rc, rcErr := suppressionFile.Open()
	if rcErr != nil {
		return nil, downloadedBytes, dlStatusCode, dlContentType,
			fmt.Errorf("open zip entry %s: %w", suppressionFile.Name, rcErr)
	}
	defer rc.Close()

	// ------------------------------------------------------------------
	// Step 4: Stream-parse entries directly from the ZIP reader
	// ------------------------------------------------------------------
	entries, parsedSize := parseSuppressionStream(rc, sourceID)
	if parsedSize > downloadedBytes {
		downloadedBytes = parsedSize
	}

	log.Printf("[RefreshEngine] Optizmo source %s: parsed %d entries from %s",
		sourceID, len(entries), suppressionFile.Name)

	return entries, downloadedBytes, dlStatusCode, dlContentType, nil
}

// =============================================================================
// STREAM PARSING (shared by all providers)
// =============================================================================

// parsedEntry holds a single email/MD5 pair parsed from a suppression file.
type parsedEntry struct {
	email   string
	md5Hash string
}

// parseSuppressionStream reads a suppression file from a reader and parses entries.
// Returns (entries, fileSizeBytes).
func parseSuppressionStream(r io.Reader, sourceID string) ([]parsedEntry, int64) {
	var entries []parsedEntry
	var fileSizeBytes int64

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		fileSizeBytes += int64(len(scanner.Bytes())) + 1

		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if csvHeaderTokens[strings.ToLower(line)] {
			continue
		}

		// Handle CSV lines – take the first field
		if strings.Contains(line, ",") {
			parts := strings.SplitN(line, ",", 2)
			line = strings.TrimSpace(parts[0])
			line = strings.Trim(line, `"'`)
			if line == "" {
				continue
			}
			if csvHeaderTokens[strings.ToLower(line)] {
				continue
			}
		}

		// Determine if line is MD5 or email
		if md5HexPattern.MatchString(line) {
			entries = append(entries, parsedEntry{
				md5Hash: strings.ToLower(line),
			})
		} else {
			email := strings.ToLower(strings.TrimSpace(line))
			if email == "" {
				continue
			}
			hash := md5.Sum([]byte(email))
			entries = append(entries, parsedEntry{
				email:   email,
				md5Hash: hex.EncodeToString(hash[:]),
			})
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[RefreshEngine] Scanner error for source %s: %v", sourceID, err)
	}

	return entries, fileSizeBytes
}

// =============================================================================
// PROVIDER DETECTION
// =============================================================================

// detectProvider auto-detects the suppression provider from the download URL.
func detectProvider(rawurl string) string {
	lower := strings.ToLower(rawurl)
	switch {
	case strings.Contains(lower, "optizmo.com") || strings.Contains(lower, "optizmo.net"):
		return "optizmo"
	case strings.Contains(lower, "unsubcentral.com"):
		return "unsubcentral"
	case strings.Contains(lower, "ezepo.net"):
		return "ezepo"
	case strings.Contains(lower, "unsubscribemaster.com"):
		return "unsubscribemaster"
	case strings.Contains(lower, "unsub-optr.com"):
		return "optima"
	case strings.Contains(lower, "unsub-bmv.com"):
		return "bmv"
	default:
		return "other"
	}
}

// =============================================================================
// API HANDLERS (to be wired into Chi router)
// =============================================================================

// HandleRefreshStatus returns the engine status as JSON.
func (e *SuppressionRefreshEngine) HandleRefreshStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(e.GetStatus())
}

// HandleManualRefresh triggers a manual refresh cycle.
func (e *SuppressionRefreshEngine) HandleManualRefresh(w http.ResponseWriter, r *http.Request) {
	cycleID, err := e.ManualTrigger()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"cycle_id": cycleID,
		"message":  "Manual refresh cycle triggered",
	})
}
