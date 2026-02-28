package mailing

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// Worker handles background email sending
type Worker struct {
	store      *Store
	sender     *Sender
	campSender *CampaignSender
	batchSize  int
	interval   time.Duration
	running    bool
	mu         sync.Mutex
	stopCh     chan struct{}
	wg         sync.WaitGroup
}

// NewWorker creates a new send worker
func NewWorker(store *Store, sender *Sender, batchSize int, interval time.Duration) *Worker {
	return &Worker{
		store:      store,
		sender:     sender,
		campSender: NewCampaignSender(store, sender),
		batchSize:  batchSize,
		interval:   interval,
		stopCh:     make(chan struct{}),
	}
}

// Start starts the worker
func (w *Worker) Start(ctx context.Context) error {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return fmt.Errorf("worker already running")
	}
	w.running = true
	w.mu.Unlock()

	log.Println("Starting send worker...")

	w.wg.Add(1)
	go w.run(ctx)

	return nil
}

// Stop stops the worker
func (w *Worker) Stop() {
	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()

	log.Println("Stopping send worker...")
	close(w.stopCh)
	w.wg.Wait()

	w.mu.Lock()
	w.running = false
	w.mu.Unlock()
}

func (w *Worker) run(ctx context.Context) {
	defer w.wg.Done()

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.processBatch(ctx)
		}
	}
}

func (w *Worker) processBatch(ctx context.Context) {
	sent, err := w.campSender.ProcessQueue(ctx, w.batchSize)
	if err != nil {
		log.Printf("Error processing queue: %v", err)
		return
	}
	if sent > 0 {
		log.Printf("Processed %d emails", sent)
	}
}

// QuotaResetWorker resets daily/hourly quotas
type QuotaResetWorker struct {
	store    *Store
	interval time.Duration
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewQuotaResetWorker creates a new quota reset worker
func NewQuotaResetWorker(store *Store) *QuotaResetWorker {
	return &QuotaResetWorker{
		store:    store,
		interval: 1 * time.Minute,
		stopCh:   make(chan struct{}),
	}
}

// Start starts the quota reset worker
func (qr *QuotaResetWorker) Start(ctx context.Context) {
	qr.wg.Add(1)
	go qr.run(ctx)
}

// Stop stops the quota reset worker
func (qr *QuotaResetWorker) Stop() {
	close(qr.stopCh)
	qr.wg.Wait()
}

func (qr *QuotaResetWorker) run(ctx context.Context) {
	defer qr.wg.Done()

	var lastHourlyReset, lastDailyReset time.Time

	ticker := time.NewTicker(qr.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-qr.stopCh:
			return
		case now := <-ticker.C:
			// Reset hourly at the start of each hour
			if now.Hour() != lastHourlyReset.Hour() {
				qr.resetHourlyQuotas(ctx)
				lastHourlyReset = now
			}

			// Reset daily at midnight UTC
			if now.Day() != lastDailyReset.Day() {
				qr.resetDailyQuotas(ctx)
				lastDailyReset = now
			}

			// Reset monthly on the first of each month
			if now.Day() == 1 && now.Hour() == 0 && now.Minute() < 5 {
				qr.resetMonthlyQuotas(ctx)
			}
		}
	}
}

func (qr *QuotaResetWorker) resetHourlyQuotas(ctx context.Context) {
	// In production, execute: UPDATE mailing_delivery_servers SET used_hourly = 0
	log.Println("Resetting hourly quotas")
}

func (qr *QuotaResetWorker) resetDailyQuotas(ctx context.Context) {
	// In production, execute: UPDATE mailing_delivery_servers SET used_daily = 0, used_hourly = 0
	log.Println("Resetting daily quotas")
}

func (qr *QuotaResetWorker) resetMonthlyQuotas(ctx context.Context) {
	// In production, execute: UPDATE mailing_delivery_servers SET used_monthly = 0
	log.Println("Resetting monthly quotas")
}

// EngagementUpdateWorker updates subscriber engagement scores
type EngagementUpdateWorker struct {
	store    *Store
	scorer   *EngagementScorer
	interval time.Duration
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewEngagementUpdateWorker creates a new engagement update worker
func NewEngagementUpdateWorker(store *Store) *EngagementUpdateWorker {
	return &EngagementUpdateWorker{
		store:    store,
		scorer:   NewEngagementScorer(store),
		interval: 1 * time.Hour,
		stopCh:   make(chan struct{}),
	}
}

// Start starts the engagement update worker
func (eu *EngagementUpdateWorker) Start(ctx context.Context) {
	eu.wg.Add(1)
	go eu.run(ctx)
}

// Stop stops the engagement update worker
func (eu *EngagementUpdateWorker) Stop() {
	close(eu.stopCh)
	eu.wg.Wait()
}

func (eu *EngagementUpdateWorker) run(ctx context.Context) {
	defer eu.wg.Done()

	ticker := time.NewTicker(eu.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-eu.stopCh:
			return
		case <-ticker.C:
			eu.updateEngagementScores(ctx)
		}
	}
}

func (eu *EngagementUpdateWorker) updateEngagementScores(ctx context.Context) {
	// In production, batch update engagement scores
	// UPDATE mailing_subscribers SET engagement_score = calculated_score WHERE ...
	log.Println("Updating engagement scores")
}

// CampaignCompletionWorker marks campaigns as complete
type CampaignCompletionWorker struct {
	store    *Store
	interval time.Duration
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewCampaignCompletionWorker creates a new campaign completion worker
func NewCampaignCompletionWorker(store *Store) *CampaignCompletionWorker {
	return &CampaignCompletionWorker{
		store:    store,
		interval: 1 * time.Minute,
		stopCh:   make(chan struct{}),
	}
}

// Start starts the campaign completion worker
func (cc *CampaignCompletionWorker) Start(ctx context.Context) {
	cc.wg.Add(1)
	go cc.run(ctx)
}

// Stop stops the campaign completion worker
func (cc *CampaignCompletionWorker) Stop() {
	close(cc.stopCh)
	cc.wg.Wait()
}

func (cc *CampaignCompletionWorker) run(ctx context.Context) {
	defer cc.wg.Done()

	ticker := time.NewTicker(cc.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-cc.stopCh:
			return
		case <-ticker.C:
			cc.checkCampaignCompletion(ctx)
		}
	}
}

func (cc *CampaignCompletionWorker) checkCampaignCompletion(ctx context.Context) {
	// Check for campaigns where all queued emails have been sent
	// UPDATE mailing_campaigns SET status = 'sent', completed_at = NOW()
	// WHERE status = 'sending' AND NOT EXISTS (
	//   SELECT 1 FROM mailing_campaign_queue WHERE campaign_id = id AND status = 'queued'
	// )
	log.Println("Checking campaign completion")
}

// WarmupWorker manages domain/IP warmup
type WarmupWorker struct {
	store    *Store
	interval time.Duration
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewWarmupWorker creates a new warmup worker
func NewWarmupWorker(store *Store) *WarmupWorker {
	return &WarmupWorker{
		store:    store,
		interval: 24 * time.Hour,
		stopCh:   make(chan struct{}),
	}
}

// Start starts the warmup worker
func (ww *WarmupWorker) Start(ctx context.Context) {
	ww.wg.Add(1)
	go ww.run(ctx)
}

// Stop stops the warmup worker
func (ww *WarmupWorker) Stop() {
	close(ww.stopCh)
	ww.wg.Wait()
}

func (ww *WarmupWorker) run(ctx context.Context) {
	defer ww.wg.Done()

	ticker := time.NewTicker(ww.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ww.stopCh:
			return
		case <-ticker.C:
			ww.processWarmup(ctx)
		}
	}
}

func (ww *WarmupWorker) processWarmup(ctx context.Context) {
	// Warmup stages (example):
	// Stage 1: 100 emails/day
	// Stage 2: 250 emails/day
	// Stage 3: 500 emails/day
	// Stage 4: 1000 emails/day
	// Stage 5: 2500 emails/day
	// Stage 6: 5000 emails/day
	// Stage 7: 10000 emails/day
	// Stage 8: Full capacity

	// Advance warmup stage based on performance
	log.Println("Processing warmup advancement")
}

// WorkerManager manages all background workers
type WorkerManager struct {
	sendWorker       *Worker
	quotaWorker      *QuotaResetWorker
	engagementWorker *EngagementUpdateWorker
	completionWorker *CampaignCompletionWorker
	warmupWorker     *WarmupWorker
}

// NewWorkerManager creates a new worker manager
func NewWorkerManager(store *Store, sender *Sender) *WorkerManager {
	return &WorkerManager{
		sendWorker:       NewWorker(store, sender, 100, 1*time.Second),
		quotaWorker:      NewQuotaResetWorker(store),
		engagementWorker: NewEngagementUpdateWorker(store),
		completionWorker: NewCampaignCompletionWorker(store),
		warmupWorker:     NewWarmupWorker(store),
	}
}

// Start starts all workers
func (wm *WorkerManager) Start(ctx context.Context) error {
	if err := wm.sendWorker.Start(ctx); err != nil {
		return err
	}
	wm.quotaWorker.Start(ctx)
	wm.engagementWorker.Start(ctx)
	wm.completionWorker.Start(ctx)
	wm.warmupWorker.Start(ctx)
	return nil
}

// Stop stops all workers
func (wm *WorkerManager) Stop() {
	wm.sendWorker.Stop()
	wm.quotaWorker.Stop()
	wm.engagementWorker.Stop()
	wm.completionWorker.Stop()
	wm.warmupWorker.Stop()
}
