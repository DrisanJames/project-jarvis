package worker

// PartnerValidator polls partner_clean_queue for status='pending_eo' rows
// in FIFO chunks and runs EmailOversight validation. Verified + Complainer
// records become 'ready' (mailable). Other results get suppressed and
// added to the global suppression list.
//
// This worker fixes the bug pattern observed 2026-04-08:
//   - Inserts use `md5_hash` (not `email_hash`)
//   - Inserts always include `organization_id`
//   - Idempotent INSERT ... ON CONFLICT (organization_id, md5_hash) DO UPDATE.

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/emailoversight"
)

// EOValidator is the minimum interface PartnerValidator needs from the
// EmailOversight client.
type EOValidator interface {
	Validate(ctx context.Context, email string) (*emailoversight.ValidationResponse, error)
}

// PartnerValidatorConfig knobs.
type PartnerValidatorConfig struct {
	BatchSize     int           // records per cycle (default 500)
	PollInterval  time.Duration // default 30s
	Concurrency   int           // EO calls in parallel (default 10)
	MaxRetries    int           // before giving up (default 3)
	OrganizationID string       // used for global suppression inserts (must match GlobalSuppressionHub orgID)
}

// PartnerValidator drains pending_eo rows.
type PartnerValidator struct {
	db       *sql.DB
	eoClient EOValidator
	cfg      PartnerValidatorConfig

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	startOnce sync.Once
	stopOnce  sync.Once
}

func NewPartnerValidator(db *sql.DB, eoClient EOValidator, cfg PartnerValidatorConfig) *PartnerValidator {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 500
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 10
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.OrganizationID == "" {
		cfg.OrganizationID = "00000000-0000-0000-0000-000000000001"
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &PartnerValidator{
		db:       db,
		eoClient: eoClient,
		cfg:      cfg,
		ctx:      ctx,
		cancel:   cancel,
	}
}

func (pv *PartnerValidator) Start() {
	pv.startOnce.Do(func() {
		pv.wg.Add(1)
		go pv.run()
		log.Printf("[PartnerValidator] started — batch_size=%d concurrency=%d poll_interval=%s",
			pv.cfg.BatchSize, pv.cfg.Concurrency, pv.cfg.PollInterval)
	})
}

func (pv *PartnerValidator) Stop() {
	pv.stopOnce.Do(func() {
		pv.cancel()
		pv.wg.Wait()
		log.Println("[PartnerValidator] stopped")
	})
}

func (pv *PartnerValidator) run() {
	defer pv.wg.Done()
	t := time.NewTicker(pv.cfg.PollInterval)
	defer t.Stop()

	pv.processOnce()
	for {
		select {
		case <-pv.ctx.Done():
			return
		case <-t.C:
			pv.processOnce()
		}
	}
}

// processOnce drains one batch of pending_eo rows. Returns silently when
// queue is empty.
func (pv *PartnerValidator) processOnce() {
	if pv.eoClient == nil {
		return
	}
	// Reap orphaned in-flight rows first. A row goes 'eo_in_flight' at claim
	// time and should return to a terminal state within seconds; if the process
	// dies (deploy/restart) between claim and apply, the row would otherwise be
	// stuck forever because the claimer only selects 'pending_eo'. Any row still
	// in-flight long past a batch's lifetime is an orphan — return it to the
	// queue. The 15-minute floor is well above real batch latency, so it never
	// races a sibling validator that legitimately holds a fresh claim.
	pv.reapStaleInFlight(pv.ctx)
	for {
		if pv.ctx.Err() != nil {
			return
		}
		claimed, err := pv.claimPendingEO(pv.ctx)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				log.Printf("[PartnerValidator] claim_pending_eo: %v", err)
			}
			return
		}
		if len(claimed) == 0 {
			return
		}
		pv.validateAndApply(pv.ctx, claimed)
	}
}

type pendingRecord struct {
	id        string
	email     string
	emailMD5  string
	attempts  int
	datasetID string
	partnerID string
	vertical  string
}

// claimPendingEO atomically marks up to BatchSize rows as 'eo_in_flight' so
// concurrent validators don't process the same rows. FIFO ordered.
func (pv *PartnerValidator) claimPendingEO(ctx context.Context) ([]pendingRecord, error) {
	rows, err := pv.db.QueryContext(ctx, `
		WITH claimed AS (
			SELECT id
			FROM partner_clean_queue
			WHERE status = 'pending_eo'
			  AND eo_attempts < $1
			ORDER BY ingested_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
		UPDATE partner_clean_queue q
		SET status = 'eo_in_flight', claimed_at = NOW()
		FROM claimed
		WHERE q.id = claimed.id
		RETURNING q.id, q.email, q.email_md5, q.eo_attempts, q.dataset_id, q.partner_id, q.vertical
	`, pv.cfg.MaxRetries, pv.cfg.BatchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]pendingRecord, 0, pv.cfg.BatchSize)
	for rows.Next() {
		var p pendingRecord
		if err := rows.Scan(&p.id, &p.email, &p.emailMD5, &p.attempts, &p.datasetID, &p.partnerID, &p.vertical); err != nil {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// reapStaleInFlight returns orphaned 'eo_in_flight' rows to 'pending_eo' so a
// later cycle can re-validate them. Orphans appear when the process dies
// between claim and apply (the claimer only sees 'pending_eo', so they would
// otherwise wedge permanently — observed 2026-06-08..09, 831 rows stuck across
// verticals until a manual re-queue). claimed_at IS NULL covers rows claimed
// before this column was populated. eo_attempts is left untouched: orphaning is
// the platform's fault, not a validation failure, so it must not burn retries.
func (pv *PartnerValidator) reapStaleInFlight(ctx context.Context) {
	res, err := pv.db.ExecContext(ctx, `
		UPDATE partner_clean_queue
		SET status = 'pending_eo'
		WHERE status = 'eo_in_flight'
		  AND (claimed_at IS NULL OR claimed_at < NOW() - INTERVAL '15 minutes')
	`)
	if err != nil {
		log.Printf("[PartnerValidator] reap_stale_in_flight: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[PartnerValidator] reaped %d orphaned eo_in_flight -> pending_eo", n)
	}
}

// validateAndApply runs EO concurrently then applies status updates per
// record based on EO's ResultId.
func (pv *PartnerValidator) validateAndApply(ctx context.Context, batch []pendingRecord) {
	results := make([]eoOutcome, len(batch))
	sem := make(chan struct{}, pv.cfg.Concurrency)
	var wg sync.WaitGroup
	for i, rec := range batch {
		wg.Add(1)
		go func(idx int, rec pendingRecord) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[idx] = pv.callEO(ctx, rec)
			time.Sleep(150 * time.Millisecond) // gentle pacing for the EO endpoint
		}(i, rec)
	}
	wg.Wait()

	var ready, suppressed, retry, errored int
	for i, outcome := range results {
		rec := batch[i]
		switch outcome.kind {
		case outcomeReady:
			ready++
			if err := pv.markReady(ctx, rec, outcome); err != nil {
				log.Printf("[PartnerValidator] mark_ready %s: %v", rec.id, err)
			}
		case outcomeSuppress:
			suppressed++
			if err := pv.markSuppressed(ctx, rec, outcome); err != nil {
				log.Printf("[PartnerValidator] mark_suppressed %s: %v", rec.id, err)
			}
			if err := pv.addToGlobalSuppression(ctx, rec.email, outcome.result); err != nil {
				log.Printf("[PartnerValidator] add_suppression %s: %v", rec.email, err)
			}
		case outcomeRetry:
			retry++
			if err := pv.markRetry(ctx, rec); err != nil {
				log.Printf("[PartnerValidator] mark_retry %s: %v", rec.id, err)
			}
		default:
			errored++
			if err := pv.markRetry(ctx, rec); err != nil {
				log.Printf("[PartnerValidator] mark_retry_after_error %s: %v", rec.id, err)
			}
		}
	}
	log.Printf("[PartnerValidator] batch=%d ready=%d suppressed=%d retry=%d errored=%d",
		len(batch), ready, suppressed, retry, errored)
}

type eoOutcomeKind int

const (
	outcomeError eoOutcomeKind = iota
	outcomeReady
	outcomeSuppress
	outcomeRetry
)

type eoOutcome struct {
	kind     eoOutcomeKind
	resultID int
	result   string
}

// callEO classifies a single EO call result. The mapping mirrors the spec
// in the plan:
//   REJECT shape (account suppression) → suppress (terminal, same class as Undeliverable)
//   1 (Verified), 7 (Complainer) → ready (mailable)
//   0 (Retry), 11 (Unknown)      → leave pending_eo, retry next cycle
//   any other ResultID           → suppress
func (pv *PartnerValidator) callEO(ctx context.Context, rec pendingRecord) eoOutcome {
	resp, err := pv.eoClient.Validate(ctx, rec.email)
	if err != nil {
		return eoOutcome{kind: outcomeRetry, result: "error: " + err.Error()}
	}
	if resp.IsReject() {
		// EO account-suppression ({"result":"REJECT","reason":"Suppressed
		// Email Address"}) is a final verdict. Before this check, its missing
		// ResultId decoded to 0 → the retry arm below → 3 wasted attempts →
		// dead_letter. Record the human-readable reason as eo_result.
		result := strings.TrimSpace(resp.Reason)
		if result == "" {
			result = resp.Result
		}
		return eoOutcome{kind: outcomeSuppress, resultID: resp.ResultID, result: result}
	}
	switch resp.ResultID {
	case 1, 7: // Verified, Complainer (deliverable)
		return eoOutcome{kind: outcomeReady, resultID: resp.ResultID, result: resp.Result}
	case 0, 11: // Retry, Unknown
		return eoOutcome{kind: outcomeRetry, resultID: resp.ResultID, result: resp.Result}
	default:
		return eoOutcome{kind: outcomeSuppress, resultID: resp.ResultID, result: resp.Result}
	}
}

func (pv *PartnerValidator) markReady(ctx context.Context, rec pendingRecord, outcome eoOutcome) error {
	_, err := pv.db.ExecContext(ctx, `
		UPDATE partner_clean_queue
		SET status = 'ready',
		    eo_result_code = $2,
		    eo_result = $3,
		    eo_attempts = eo_attempts + 1,
		    validated_at = NOW()
		WHERE id = $1
	`, rec.id, outcome.resultID, outcome.result)
	return err
}

func (pv *PartnerValidator) markSuppressed(ctx context.Context, rec pendingRecord, outcome eoOutcome) error {
	_, err := pv.db.ExecContext(ctx, `
		UPDATE partner_clean_queue
		SET status = 'suppressed_eo',
		    eo_result_code = $2,
		    eo_result = $3,
		    eo_attempts = eo_attempts + 1,
		    validated_at = NOW()
		WHERE id = $1
	`, rec.id, outcome.resultID, outcome.result)
	return err
}

// markRetry returns the row to pending_eo for the next cycle. If we've hit
// the retry cap, it falls into 'dead_letter' so the validator stops trying.
func (pv *PartnerValidator) markRetry(ctx context.Context, rec pendingRecord) error {
	if rec.attempts+1 >= pv.cfg.MaxRetries {
		_, err := pv.db.ExecContext(ctx, `
			UPDATE partner_clean_queue
			SET status = 'dead_letter',
			    eo_attempts = eo_attempts + 1,
			    validated_at = NOW()
			WHERE id = $1
		`, rec.id)
		return err
	}
	_, err := pv.db.ExecContext(ctx, `
		UPDATE partner_clean_queue
		SET status = 'pending_eo',
		    eo_attempts = eo_attempts + 1
		WHERE id = $1
	`, rec.id)
	return err
}

// addToGlobalSuppression adds a record to mailing_global_suppressions with
// the SAME shape used by the rest of the platform — md5_hash + organization_id.
// Fixes the 2026-04-08 bug where the legacy data_pipeline used email_hash
// (no such column) and missed organization_id, leaking suppression entries
// out of the LoadFromDB scan.
func (pv *PartnerValidator) addToGlobalSuppression(ctx context.Context, email, eoResult string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil
	}
	hash := md5HashLowerForValidator(email)
	_, err := pv.db.ExecContext(ctx, `
		INSERT INTO mailing_global_suppressions
		    (organization_id, email, md5_hash, reason, source, created_at)
		VALUES ($1, $2, $3, 'eo_invalid', $4, NOW())
		ON CONFLICT (organization_id, md5_hash) DO UPDATE SET
		    reason = EXCLUDED.reason,
		    source = EXCLUDED.source,
		    updated_at = NOW()
	`, pv.cfg.OrganizationID, email, hash, "partner_validator:"+eoResult)
	return err
}

func md5HashLowerForValidator(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}
