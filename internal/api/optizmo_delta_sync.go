package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// OptizmoDeltaSyncWorker downloads Optizmo suppression files nightly for all
// offers with suppression_sync_enabled=TRUE and upserts new hashes into
// mailing_offer_suppressions. Runs within a 10PM-2AM MST window.
type OptizmoDeltaSyncWorker struct {
	db           *sql.DB
	optizmoToken string
	mstLoc       *time.Location
	stopCh       chan struct{}
	cancelFn     context.CancelFunc
	mu           sync.Mutex
	running      bool
	activeSyncs  sync.Map
	s3Client     *SuppressionS3Client
	suppMgr      *OfferSuppressionManager
}

// NewOptizmoDeltaSyncWorker creates a new worker, loading the Optizmo API
// token from the environment.
func NewOptizmoDeltaSyncWorker(db *sql.DB) *OptizmoDeltaSyncWorker {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		log.Printf("[DeltaSync] WARNING: Could not load America/Denver timezone, falling back to UTC-7: %v", err)
		loc = time.FixedZone("MST", -7*60*60)
	}

	token := os.Getenv("OPTIZMO_API_TOKEN")
	if token == "" {
		token = "nOC0do1yMRfevcVXdikjTQOhOpyGPlx5"
	}

	return &OptizmoDeltaSyncWorker{
		db:           db,
		optizmoToken: token,
		mstLoc:       loc,
		stopCh:       make(chan struct{}),
	}
}

// SetS3Client sets the S3 client for suppression file storage.
func (w *OptizmoDeltaSyncWorker) SetS3Client(s3Client *SuppressionS3Client) {
	w.s3Client = s3Client
}

// SetSuppressionManager sets the offer suppression manager for Bloom updates.
func (w *OptizmoDeltaSyncWorker) SetSuppressionManager(mgr *OfferSuppressionManager) {
	w.suppMgr = mgr
}

// Start launches the background scheduler goroutine.
func (w *OptizmoDeltaSyncWorker) Start() {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return
	}
	w.running = true
	w.stopCh = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	w.cancelFn = cancel
	w.mu.Unlock()

	log.Printf("[DeltaSync] Optizmo Delta Sync Worker started")

	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				w.checkAndRun(ctx)
			case <-w.stopCh:
				return
			}
		}
	}()
}

// Stop gracefully shuts down the worker. Active sync cycles are cancelled
// via the lifecycle context.
func (w *OptizmoDeltaSyncWorker) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.running {
		return
	}
	w.running = false
	if w.cancelFn != nil {
		w.cancelFn()
	}
	close(w.stopCh)
	log.Printf("[DeltaSync] Optizmo Delta Sync Worker stopped")
}

// checkAndRun determines whether a sync cycle should start.
// Conditions: within 10PM-2AM MST, no sync already ran today.
func (w *OptizmoDeltaSyncWorker) checkAndRun(lifecycleCtx context.Context) {
	now := time.Now().In(w.mstLoc)
	hour := now.Hour()

	// Only run between 10 PM (22) and 2 AM MST
	if hour < 22 && hour >= 2 {
		return
	}

	checkCtx, cancel := context.WithTimeout(lifecycleCtx, 30*time.Second)
	defer cancel()

	// Check if any eligible offer was synced in the last 20 hours
	var lastSyncAt sql.NullTime
	w.db.QueryRowContext(checkCtx,
		`SELECT MAX(last_suppression_sync_at) FROM mailing_offers
		 WHERE suppression_sync_enabled = TRUE AND status = 'active'`,
	).Scan(&lastSyncAt)

	if lastSyncAt.Valid && time.Since(lastSyncAt.Time) < 20*time.Hour {
		return
	}

	log.Printf("[DeltaSync] Scheduler triggering nightly sync cycle")
	if err := w.runSyncCycle(lifecycleCtx); err != nil {
		log.Printf("[DeltaSync] Cycle error: %v", err)
	}
}

type syncOfferRow struct {
	ID          string
	Name        string
	OptizmoLink string
}

// runSyncCycle iterates all eligible offers and syncs each one.
func (w *OptizmoDeltaSyncWorker) runSyncCycle(parentCtx context.Context) error {
	ctx, cancel := context.WithTimeout(parentCtx, 2*time.Hour)
	defer cancel()

	rows, err := w.db.QueryContext(ctx,
		`SELECT id, COALESCE(name,''), COALESCE(optizmo_link,'')
		 FROM mailing_offers
		 WHERE suppression_sync_enabled = TRUE
		   AND status = 'active'
		   AND optizmo_link IS NOT NULL AND optizmo_link != ''
		 ORDER BY name`)
	if err != nil {
		return fmt.Errorf("query eligible offers: %w", err)
	}

	var offers []syncOfferRow
	for rows.Next() {
		var o syncOfferRow
		if err := rows.Scan(&o.ID, &o.Name, &o.OptizmoLink); err != nil {
			log.Printf("[DeltaSync] scan offer row: %v", err)
			continue
		}
		offers = append(offers, o)
	}
	rows.Close()

	if len(offers) == 0 {
		log.Printf("[DeltaSync] No eligible offers for nightly sync")
		return nil
	}

	log.Printf("[DeltaSync] Starting nightly sync for %d offers", len(offers))

	for i, offer := range offers {
		select {
		case <-ctx.Done():
			log.Printf("[DeltaSync] Context cancelled, stopping after %d/%d offers", i, len(offers))
			return ctx.Err()
		default:
		}

		if _, busy := w.activeSyncs.LoadOrStore(offer.ID, struct{}{}); busy {
			log.Printf("[DeltaSync] Skipping offer %s (%s) — sync already in progress", offer.ID, offer.Name)
			continue
		}
		w.syncOffer(ctx, offer)
		w.activeSyncs.Delete(offer.ID)

		if i < len(offers)-1 {
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	log.Printf("[DeltaSync] Nightly sync cycle completed for %d offers", len(offers))
	return nil
}

// syncOffer downloads and processes the suppression list for a single offer.
// Caller must manage the activeSyncs guard for manual triggers; the nightly
// cycle also checks to skip already-active offers.
func (w *OptizmoDeltaSyncWorker) syncOffer(ctx context.Context, offer syncOfferRow) {
	jobID := uuid.New().String()
	now := time.Now()

	_, err := w.db.ExecContext(ctx,
		`INSERT INTO mailing_optizmo_scrub_jobs
			(id, offer_id, audience_file_path, audience_count, status, requested_at, started_at, scrub_type)
		 VALUES ($1, $2, '', 0, 'processing', $3, $3, 'nightly_sync')`,
		jobID, offer.ID, now)
	if err != nil {
		log.Printf("[DeltaSync] offer %s (%s): failed to create job record: %v", offer.ID, offer.Name, err)
		w.setOfferSyncError(ctx, offer.ID, fmt.Sprintf("create job: %v", err))
		return
	}

	logPrefix := fmt.Sprintf("sync %s [%s]", offer.Name, jobID[:8])
	log.Printf("[DeltaSync] %s: starting download from %s", logPrefix, offer.OptizmoLink)

	dlResult, dlErr := downloadOptizmoHashesPkg(ctx, w.optizmoToken, logPrefix, offer.OptizmoLink)
	if dlErr != nil {
		errMsg := fmt.Sprintf("download failed: %v", dlErr)
		log.Printf("[DeltaSync] %s: %s", logPrefix, errMsg)
		dbCtx, dbCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer dbCancel()
		var flc, vmd5, nmd5 int
		if dlResult != nil {
			flc, vmd5, nmd5 = dlResult.FileLineCount, dlResult.ValidMD5Count, dlResult.NonMD5Count
			os.Remove(dlResult.HashFilePath)
		}
		w.db.ExecContext(dbCtx,
			`UPDATE mailing_optizmo_scrub_jobs
			 SET status = 'failed', error_message = $1, completed_at = NOW(),
			     file_count = $2, valid_md5_count = $3, non_md5_count = $4
			 WHERE id = $5`,
			errMsg, flc, vmd5, nmd5, jobID)
		w.setOfferSyncError(dbCtx, offer.ID, errMsg)
		return
	}
	defer os.Remove(dlResult.HashFilePath)

	w.db.ExecContext(ctx,
		`UPDATE mailing_optizmo_scrub_jobs SET file_count = $1, valid_md5_count = $2, non_md5_count = $3, progress_pct = 30, progress_message = 'Downloaded, uploading to S3…' WHERE id = $4`,
		dlResult.FileLineCount, dlResult.ValidMD5Count, dlResult.NonMD5Count, jobID)

	// Upload hashes to S3
	if w.s3Client != nil {
		hashFile, openErr := os.Open(dlResult.HashFilePath)
		if openErr == nil {
			_, _, uploadErr := w.s3Client.UploadHashFile(ctx, offer.ID, hashFile)
			hashFile.Close()
			if uploadErr != nil {
				log.Printf("[DeltaSync] %s: S3 hash upload failed (non-fatal): %v", logPrefix, uploadErr)
			}
		}
	}

	if dlResult.FileLineCount == 0 {
		w.db.ExecContext(ctx,
			`UPDATE mailing_optizmo_scrub_jobs
			 SET status = 'completed', audience_count = 0, suppressed_count = 0, completed_at = NOW(), progress_pct = 100, progress_message = 'Complete — empty list'
			 WHERE id = $1`, jobID)
		w.updateOfferSyncSuccess(ctx, offer.ID)
		log.Printf("[DeltaSync] %s: empty list — marked synced", logPrefix)
		return
	}

	w.db.ExecContext(ctx,
		`UPDATE mailing_optizmo_scrub_jobs SET progress_pct = 50, progress_message = 'Matching subscribers…' WHERE id = $1`, jobID)

	audienceCount, suppressedCount, _, matchErr := matchAndSuppressFromHashFile(ctx, w.db, offer.ID, dlResult.HashFilePath)
	if matchErr != nil {
		errMsg := fmt.Sprintf("subscriber matching failed: %v", matchErr)
		log.Printf("[DeltaSync] %s: %s", logPrefix, errMsg)
		dbCtx2, dbCancel2 := context.WithTimeout(context.Background(), 15*time.Second)
		defer dbCancel2()
		w.db.ExecContext(dbCtx2,
			`UPDATE mailing_optizmo_scrub_jobs
			 SET status = 'failed', error_message = $1, audience_count = $2, completed_at = NOW()
			 WHERE id = $3`, errMsg, audienceCount, jobID)
		w.setOfferSyncError(dbCtx2, offer.ID, errMsg)
		return
	}

	w.db.ExecContext(ctx,
		`UPDATE mailing_optizmo_scrub_jobs SET progress_pct = 80, progress_message = 'Building Bloom filter…' WHERE id = $1`, jobID)

	// Rebuild Bloom — prefer local file, fall back to S3
	if w.suppMgr != nil {
		bloomErr := w.suppMgr.RebuildBloomFromLocalFile(ctx, offer.ID, dlResult.HashFilePath)
		if bloomErr != nil {
			log.Printf("[DeltaSync] %s: local Bloom build failed, trying S3: %v", logPrefix, bloomErr)
			bloomErr = w.suppMgr.RebuildBloomFromS3Hashes(ctx, offer.ID)
		}
		if bloomErr != nil {
			log.Printf("[DeltaSync] %s: Bloom rebuild failed (non-fatal): %v", logPrefix, bloomErr)
		}
	}

	completedAt := time.Now()
	w.db.ExecContext(ctx,
		`UPDATE mailing_optizmo_scrub_jobs
		 SET status = 'completed', audience_count = $1, suppressed_count = $2, completed_at = $3,
		     progress_pct = 100, progress_message = 'Complete'
		 WHERE id = $4`, audienceCount, suppressedCount, completedAt, jobID)

	w.updateOfferSyncSuccess(ctx, offer.ID)

	log.Printf("[DeltaSync] %s: COMPLETED — %d file entries, %d audience, %d suppressed",
		logPrefix, dlResult.FileLineCount, audienceCount, suppressedCount)
}

func (w *OptizmoDeltaSyncWorker) setOfferSyncError(ctx context.Context, offerID, errMsg string) {
	w.db.ExecContext(ctx,
		`UPDATE mailing_offers
		 SET last_suppression_sync_error = $1, updated_at = NOW()
		 WHERE id = $2`, errMsg, offerID)
}

func (w *OptizmoDeltaSyncWorker) updateOfferSyncSuccess(ctx context.Context, offerID string) {
	now := time.Now()
	w.db.ExecContext(ctx,
		`UPDATE mailing_offers
		 SET optizmo_status = 'scrubbed',
		     optizmo_last_scrubbed_at = $1,
		     last_suppression_sync_at = $1,
		     last_suppression_sync_error = '',
		     updated_at = NOW()
		 WHERE id = $2`, now, offerID)
}

// ---------------------------------------------------------------------------
// HTTP Handlers
// ---------------------------------------------------------------------------

// HandleToggleSync toggles suppression_sync_enabled for an offer.
// POST /offer-center/offers/{id}/optizmo/toggle-sync
// Body: { "enabled": true|false }
func (w *OptizmoDeltaSyncWorker) HandleToggleSync(wr http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(wr, http.StatusBadRequest, "offer id is required")
		return
	}

	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(wr, http.StatusBadRequest, "invalid JSON body")
		return
	}

	ctx := r.Context()

	if req.Enabled {
		var optizmoLink sql.NullString
		err := w.db.QueryRowContext(ctx,
			`SELECT optizmo_link FROM mailing_offers WHERE id = $1`, offerID,
		).Scan(&optizmoLink)
		if err == sql.ErrNoRows {
			respondError(wr, http.StatusNotFound, "offer not found")
			return
		}
		if err != nil {
			respondError(wr, http.StatusInternalServerError, "failed to query offer")
			return
		}
		if !optizmoLink.Valid || optizmoLink.String == "" {
			respondError(wr, http.StatusBadRequest, "cannot enable sync — offer has no Optizmo link configured")
			return
		}
	}

	result, err := w.db.ExecContext(ctx,
		`UPDATE mailing_offers SET suppression_sync_enabled = $1, updated_at = NOW() WHERE id = $2`,
		req.Enabled, offerID)
	if err != nil {
		respondError(wr, http.StatusInternalServerError, "failed to update offer")
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		respondError(wr, http.StatusNotFound, "offer not found")
		return
	}

	action := "disabled"
	if req.Enabled {
		action = "enabled"
	}
	log.Printf("[DeltaSync] Nightly sync %s for offer %s", action, offerID)

	respondJSON(wr, http.StatusOK, map[string]interface{}{
		"offer_id":                  offerID,
		"suppression_sync_enabled":  req.Enabled,
	})
}

// HandleManualSync triggers an immediate sync for a single offer.
// POST /offer-center/offers/{id}/optizmo/trigger-sync
func (w *OptizmoDeltaSyncWorker) HandleManualSync(wr http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(wr, http.StatusBadRequest, "offer id is required")
		return
	}

	ctx := r.Context()

	var name, optizmoLink string
	err := w.db.QueryRowContext(ctx,
		`SELECT COALESCE(name,''), COALESCE(optizmo_link,'')
		 FROM mailing_offers WHERE id = $1`, offerID,
	).Scan(&name, &optizmoLink)
	if err == sql.ErrNoRows {
		respondError(wr, http.StatusNotFound, "offer not found")
		return
	}
	if err != nil {
		respondError(wr, http.StatusInternalServerError, "failed to query offer")
		return
	}
	if optizmoLink == "" {
		respondError(wr, http.StatusBadRequest, "offer has no Optizmo link configured")
		return
	}

	if _, loaded := w.activeSyncs.LoadOrStore(offerID, struct{}{}); loaded {
		respondError(wr, http.StatusConflict, "a sync is already in progress for this offer")
		return
	}

	go func() {
		defer w.activeSyncs.Delete(offerID)
		w.syncOffer(context.Background(), syncOfferRow{
			ID:          offerID,
			Name:        name,
			OptizmoLink: optizmoLink,
		})
	}()

	respondJSON(wr, http.StatusAccepted, map[string]interface{}{
		"offer_id": offerID,
		"status":   "sync_triggered",
		"message":  "Nightly sync triggered for this offer — check scrub history for results",
	})
}

// HandleSyncStatus returns sync status for a specific offer.
// GET /offer-center/offers/{id}/optizmo/sync-status
func (w *OptizmoDeltaSyncWorker) HandleSyncStatus(wr http.ResponseWriter, r *http.Request) {
	offerID := chi.URLParam(r, "id")
	if offerID == "" {
		respondError(wr, http.StatusBadRequest, "offer id is required")
		return
	}

	ctx := r.Context()

	var syncEnabled bool
	var lastSyncAt sql.NullTime
	var lastSyncError sql.NullString
	err := w.db.QueryRowContext(ctx,
		`SELECT COALESCE(suppression_sync_enabled, FALSE),
		        last_suppression_sync_at,
		        COALESCE(last_suppression_sync_error, '')
		 FROM mailing_offers WHERE id = $1`, offerID,
	).Scan(&syncEnabled, &lastSyncAt, &lastSyncError)
	if err == sql.ErrNoRows {
		respondError(wr, http.StatusNotFound, "offer not found")
		return
	}
	if err != nil {
		respondError(wr, http.StatusInternalServerError, "failed to query offer")
		return
	}

	now := time.Now().In(w.mstLoc)
	nextWindow := time.Date(now.Year(), now.Month(), now.Day(), 22, 0, 0, 0, w.mstLoc)
	if now.After(nextWindow) {
		nextWindow = nextWindow.Add(24 * time.Hour)
	}

	resp := map[string]interface{}{
		"sync_enabled":     syncEnabled,
		"next_sync_window": nextWindow.Format(time.RFC3339),
	}

	if lastSyncAt.Valid {
		resp["last_sync_at"] = lastSyncAt.Time.Format(time.RFC3339)
	}
	if lastSyncError.Valid && lastSyncError.String != "" {
		resp["last_sync_error"] = lastSyncError.String
	}

	// Recent nightly sync jobs
	rows, err := w.db.QueryContext(ctx,
		`SELECT id, status, COALESCE(file_count,0), COALESCE(audience_count,0),
		        COALESCE(suppressed_count,0), COALESCE(error_message,''),
		        requested_at, completed_at
		 FROM mailing_optizmo_scrub_jobs
		 WHERE offer_id = $1 AND scrub_type = 'nightly_sync'
		 ORDER BY requested_at DESC LIMIT 10`, offerID)
	if err == nil {
		defer rows.Close()
		var jobs []map[string]interface{}
		for rows.Next() {
			var id, status, errMsg string
			var fileCount, audienceCount, suppressedCount int
			var requestedAt time.Time
			var completedAt sql.NullTime
			if err := rows.Scan(&id, &status, &fileCount, &audienceCount, &suppressedCount, &errMsg, &requestedAt, &completedAt); err != nil {
				continue
			}
			job := map[string]interface{}{
				"id":               id,
				"status":           status,
				"file_count":       fileCount,
				"audience_count":   audienceCount,
				"suppressed_count": suppressedCount,
				"requested_at":     requestedAt.Format(time.RFC3339),
			}
			if errMsg != "" {
				job["error_message"] = errMsg
			}
			if completedAt.Valid {
				job["completed_at"] = completedAt.Time.Format(time.RFC3339)
			}
			jobs = append(jobs, job)
		}
		if jobs == nil {
			jobs = []map[string]interface{}{}
		}
		resp["recent_sync_jobs"] = jobs
	}

	respondJSON(wr, http.StatusOK, resp)
}
