package worker

// PartnerSlicer drains the partner_inbound_batches FIFO, streams the S3
// NDJSON for each batch in slice-sized chunks, applies global-suppression
// dropping, and inserts survivors into partner_clean_queue with status
// 'pending_eo' so the validator worker can pick them up.
//
// Key design rules (per plan + sending-throttle.mdc):
//   1. FIFO at the BATCH boundary — oldest received_at first.
//   2. Pointer-based offset so a crashed slicer resumes where it left off.
//   3. Emergency-stop respected at the SLICE boundary, never mid-slice.
//   4. Partner sourcing — never reach into mailing_subscribers here. We
//      only populate partner_clean_queue. The drip orchestrator promotes
//      survivors to subscribers at wave-creation time.

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// batchLeaseTTL is how long a claimed batch stays off-limits to other workers.
// A worker renews it on every slice, so this bounds how long an ABANDONED
// batch (crashed or hung worker) waits before another worker recovers it —
// not how long a batch may legitimately take.
const batchLeaseTTL = "15 minutes"

// SuppressionHub is the minimal interface PartnerSlicer needs from the
// engine.GlobalSuppressionHub. Pulled out so the worker is unit-testable
// without spinning up the real hub.
type SuppressionHub interface {
	IsSuppressed(email string) bool
	IsSuppressedByHash(md5Hash string) bool
}

// PartnerSlicerConfig knobs.
type PartnerSlicerConfig struct {
	SliceSize    int           // records per slice (default 1000)
	PollInterval time.Duration // how often to look for new batches (default 30s)
	Concurrency  int           // batches processed concurrently (default 1)
	S3Region     string
}

// PartnerSlicer drains S3 partner batches into partner_clean_queue.
type PartnerSlicer struct {
	db       *sql.DB
	s3Client *s3.Client
	bucket   string
	hub      SuppressionHub
	cfg      PartnerSlicerConfig

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	startOnce sync.Once
	stopOnce  sync.Once
}

// NewPartnerSlicer constructs the worker. db, s3Client, and hub must be
// non-nil. Pass an explicit cfg or zero-values to take defaults.
func NewPartnerSlicer(db *sql.DB, s3Client *s3.Client, bucket string, hub SuppressionHub, cfg PartnerSlicerConfig) *PartnerSlicer {
	if cfg.SliceSize <= 0 {
		cfg.SliceSize = 1000
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &PartnerSlicer{
		db:       db,
		s3Client: s3Client,
		bucket:   bucket,
		hub:      hub,
		cfg:      cfg,
		ctx:      ctx,
		cancel:   cancel,
	}
}

func (ps *PartnerSlicer) Start() {
	ps.startOnce.Do(func() {
		// Start cfg.Concurrency workers. Cost is per BATCH (one S3 GetObject
		// plus a claim/complete transaction), and partners may post a single
		// record per call, so throughput is bound by API-call count rather
		// than data volume — one worker cannot keep up with a real-time feed.
		// Workers claim disjoint rows via SKIP LOCKED and hold a renewed lease
		// (batchLeaseTTL) for the duration of processing.
		for i := 0; i < ps.cfg.Concurrency; i++ {
			ps.wg.Add(1)
			go func(worker int) {
				// Stagger so N workers don't hit S3/PG in lockstep on boot.
				select {
				case <-ps.ctx.Done():
					ps.wg.Done()
					return
				case <-time.After(time.Duration(worker) * 250 * time.Millisecond):
				}
				ps.run()
			}(i)
		}
		log.Printf("[PartnerSlicer] started — slice_size=%d poll_interval=%s workers=%d",
			ps.cfg.SliceSize, ps.cfg.PollInterval, ps.cfg.Concurrency)
	})
}

func (ps *PartnerSlicer) Stop() {
	ps.stopOnce.Do(func() {
		ps.cancel()
		ps.wg.Wait()
		log.Println("[PartnerSlicer] stopped")
	})
}

func (ps *PartnerSlicer) run() {
	defer ps.wg.Done()
	t := time.NewTicker(ps.cfg.PollInterval)
	defer t.Stop()

	// Try once on boot so an existing backlog isn't blocked behind the first tick.
	ps.processOnce()
	for {
		select {
		case <-ps.ctx.Done():
			return
		case <-t.C:
			ps.processOnce()
		}
	}
}

// processOnce claims the next FIFO batch (skipping emergency-stopped ones)
// and runs slices until the batch is fully drained or interrupted.
func (ps *PartnerSlicer) processOnce() {
	for {
		if ps.ctx.Err() != nil {
			return
		}
		batch, err := ps.claimNextBatch(ps.ctx)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				log.Printf("[PartnerSlicer] claim_next_batch: %v", err)
			}
			return
		}
		if batch == nil {
			return
		}
		if err := ps.processBatch(ps.ctx, batch); err != nil {
			log.Printf("[PartnerSlicer] batch %s: %v", batch.id, err)
			// Mark batch in-error but recoverable on next tick.
			ps.markBatchError(batch.id, err.Error())
		}
	}
}

type partnerBatch struct {
	id          string
	datasetID   string
	partnerID   string
	vertical    string
	s3Bucket    string
	s3Key       string
	recordCount int
	nextOffset  int
}

// claimNextBatch takes the next eligible batch — express datasets first, then
// oldest received_at — and atomically flips it to 'slicing' under a lease, so
// concurrent workers never process the same batch. Returns nil when no batch
// is eligible.
func (ps *PartnerSlicer) claimNextBatch(ctx context.Context) (*partnerBatch, error) {
	tx, err := ps.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Express datasets are claimed ahead of bulk intake so a record that must
	// mail on arrival is not queued behind unrelated backlog. Ordering only:
	// nothing is skipped, and non-express batches keep their received_at FIFO
	// order relative to each other.
	//
	// A 'slicing' row is claimable only once its LEASE has expired. The row
	// lock taken here is released when this short transaction commits — long
	// before the S3 fetch and insert work finishes — so with more than one
	// worker, matching 'slicing' unconditionally would let a second worker
	// claim a batch the first is still actively processing, racing its
	// next_record_offset writes. The lease keeps crash recovery (a worker that
	// dies stops renewing, and the batch is picked up after the timeout)
	// without allowing concurrent processing of the same batch.
	row := tx.QueryRowContext(ctx, `
		SELECT b.id, b.dataset_id, b.partner_id, d.vertical,
		       b.s3_bucket, b.s3_key, b.record_count, b.next_record_offset
		FROM partner_inbound_batches b
		JOIN partner_datasets d ON d.id = b.dataset_id
		WHERE b.emergency_stopped = false
		  AND (
		        b.status = 'received'
		     OR (b.status = 'slicing'
		         AND COALESCE(b.slicing_started_at, 'epoch'::timestamptz) < NOW() - $1::interval)
		      )
		  AND d.paused_emergency = false
		  AND d.status = 'active'
		ORDER BY COALESCE(d.express_dispatch, false) DESC, b.received_at ASC
		FOR UPDATE OF b SKIP LOCKED
		LIMIT 1
	`, batchLeaseTTL)
	var b partnerBatch
	if err := row.Scan(&b.id, &b.datasetID, &b.partnerID, &b.vertical,
		&b.s3Bucket, &b.s3Key, &b.recordCount, &b.nextOffset); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	// NOW(), not COALESCE(slicing_started_at, NOW()): this column is the lease
	// clock. Preserving the first-claim time would leave a stale-recovered
	// batch permanently past its lease, so every worker would consider it
	// claimable and the guard above would protect nothing.
	if _, err := tx.ExecContext(ctx, `
		UPDATE partner_inbound_batches
		SET status = 'slicing',
		    slicing_started_at = NOW()
		WHERE id = $1
	`, b.id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &b, nil
}

// processBatch streams the S3 NDJSON, skipping the first nextOffset records,
// processing in slice-sized chunks. After each chunk we update next_record_offset
// and check ctx + emergency-stopped so a Stop() or operator pause is respected
// at the slice boundary (per sending-throttle.mdc).
func (ps *PartnerSlicer) processBatch(ctx context.Context, b *partnerBatch) error {
	out, err := ps.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.s3Bucket),
		Key:    aws.String(b.s3Key),
	})
	if err != nil {
		return fmt.Errorf("s3 GetObject %s/%s: %w", b.s3Bucket, b.s3Key, err)
	}
	defer out.Body.Close()

	gzr, err := gzipReaderFromS3(out.Body)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gzr.Close()

	scanner := bufio.NewScanner(gzr)
	scanner.Buffer(make([]byte, 256*1024), 1024*1024)

	// Skip records we've already processed (resume after crash).
	for i := 0; i < b.nextOffset; i++ {
		if !scanner.Scan() {
			break
		}
	}

	totalSliced := 0
	totalSurvived := 0
	totalGlobalDropped := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Re-check emergency stop at every slice boundary so the operator's
		// pause is felt within at most slice_size records.
		if paused, _ := ps.isDatasetPaused(ctx, b.datasetID); paused {
			log.Printf("[PartnerSlicer] dataset %s paused mid-batch %s — leaving for next cycle", b.datasetID, b.id)
			return nil
		}

		records, sliceCount := ps.readSlice(scanner, ps.cfg.SliceSize)
		if sliceCount == 0 {
			break
		}

		survivors, dropped := ps.classifyAndFilter(records)
		totalGlobalDropped += dropped

		if len(survivors) > 0 {
			if err := ps.bulkInsertSurvivors(ctx, b, survivors); err != nil {
				return fmt.Errorf("bulk insert: %w", err)
			}
		}
		// Always record dropped records so operators can audit the gap
		// between recordCount and ready_total.
		if dropped > 0 {
			ps.bulkInsertSuppressed(ctx, b, records, survivors)
		}

		totalSliced += sliceCount
		totalSurvived += len(survivors)
		// Renew the lease with each slice. A 50k-record batch can legitimately
		// run longer than batchLeaseTTL; without renewal another worker would
		// treat it as abandoned and process it concurrently. Progress itself is
		// the liveness signal, so a worker that hangs or dies stops renewing and
		// the batch is correctly recovered.
		newOffset := b.nextOffset + totalSliced
		if _, err := ps.db.ExecContext(ctx, `
			UPDATE partner_inbound_batches
			SET next_record_offset = $1,
			    slicing_started_at = NOW()
			WHERE id = $2
		`, newOffset, b.id); err != nil {
			return fmt.Errorf("update offset: %w", err)
		}
	}

	if scanErr := scanner.Err(); scanErr != nil {
		return fmt.Errorf("scan: %w", scanErr)
	}

	finalOffset := b.nextOffset + totalSliced
	_, _ = ps.db.ExecContext(ctx, `
		UPDATE partner_inbound_batches
		SET status = 'slicing_complete',
		    slicing_completed_at = NOW(),
		    next_record_offset = $1
		WHERE id = $2
	`, finalOffset, b.id)
	log.Printf("[PartnerSlicer] batch %s done — sliced=%d survived=%d global_dropped=%d", b.id, totalSliced, totalSurvived, totalGlobalDropped)
	return nil
}

// readSlice pulls up to sliceSize records from the scanner. Returns the
// parsed records plus the count actually read (so the caller can detect EOF).
func (ps *PartnerSlicer) readSlice(scanner *bufio.Scanner, sliceSize int) ([]partnerRawRecord, int) {
	out := make([]partnerRawRecord, 0, sliceSize)
	count := 0
	for count < sliceSize && scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec partnerRawRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			count++
			continue
		}
		rec.Email = strings.ToLower(strings.TrimSpace(rec.Email))
		if rec.Email == "" || !strings.Contains(rec.Email, "@") {
			count++
			continue
		}
		rec.MD5 = md5HashLower(rec.Email)
		domain := domainOf(rec.Email)
		rec.ISPFamily = isp.GroupFromDomain(domain)
		out = append(out, rec)
		count++
	}
	return out, count
}

// extraMetadataJSON builds the partner_clean_queue.extra_metadata payload.
//
// THIRD and final place a partner field can be destroyed on this path. It is a
// hand-written field list, NOT a marshal of partnerRawRecord — so adding a
// struct field does NOTHING unless it is also named here. That is exactly how
// `city` survived two consecutive fixes (the api door, then partnerRawRecord)
// and still arrived empty across three live sends: door -> struct -> THIS
// FUNCTION, and only the first two were extended (2026-08-10).
//
// Extracted from bulkInsertSurvivors so a test can bind to the REAL field list.
// Inline, the only way to test it was to copy the map into the test, which
// yields a test that passes even when production drops a field.
func extraMetadataJSON(rec partnerRawRecord) []byte {
	out, _ := json.Marshal(map[string]interface{}{
		"first_name":  rec.FirstName,
		"last_name":   rec.LastName,
		"city":        rec.City,
		"zip":         rec.Zip,
		"state":       rec.State,
		"address_1":   rec.Address1,
		"ip_address":  rec.IPAddress,
		"opt_in_date": rec.OptInDate,
		"signup_url":  rec.SignupURL,
		"signup_date": rec.SignupAt,
		"source":      rec.Source,
		"metadata":    rec.Metadata,
	})
	return out
}

// partnerRawRecord is the SECOND closed struct on the ingest path, and it has
// the same destroy-on-omission property as api.ingestRecord: the S3 NDJSON is
// decoded into this and re-marshaled into partner_clean_queue.extra_metadata,
// so a field missing here is dropped even when the door preserved it.
//
// 2026-08-10: the door was fixed to keep `city`, and city STILL vanished —
// because this struct had no City field. Both structs must be extended
// together; fixing one and testing the other's output looks like the fix
// failed. Keep the field list in lockstep with api.ingestRecord.
type partnerRawRecord struct {
	Email     string                 `json:"email"`
	FirstName string                 `json:"first_name,omitempty"`
	LastName  string                 `json:"last_name,omitempty"`
	City      string                 `json:"city,omitempty"`
	Zip       string                 `json:"zip,omitempty"`
	State     string                 `json:"state,omitempty"`
	Address1  string                 `json:"address_1,omitempty"`
	IPAddress string                 `json:"ip_address,omitempty"`
	OptInDate string                 `json:"opt_in_date,omitempty"`
	SignupURL string                 `json:"signup_url,omitempty"`
	SignupAt  string                 `json:"signup_date,omitempty"`
	Source    string                 `json:"source,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
	MD5       string                 `json:"-"`
	ISPFamily string                 `json:"-"`
}

// classifyAndFilter splits records into survivors and globally-suppressed.
// Returns (survivors, dropped_count).
func (ps *PartnerSlicer) classifyAndFilter(records []partnerRawRecord) ([]partnerRawRecord, int) {
	if ps.hub == nil {
		return records, 0
	}
	survivors := make([]partnerRawRecord, 0, len(records))
	dropped := 0
	for _, rec := range records {
		if rec.Email == "" {
			dropped++
			continue
		}
		if ps.hub.IsSuppressedByHash(rec.MD5) || ps.hub.IsSuppressed(rec.Email) {
			dropped++
			continue
		}
		survivors = append(survivors, rec)
	}
	return survivors, dropped
}

// bulkInsertSurvivors inserts records into partner_clean_queue with
// status='pending_eo'. Uses a single multi-row INSERT for efficiency.
//
// A record we have already seen is NOT a no-op: the partner re-pushes an email
// when it goes active in their network, so the repeat IS the freshness signal
// (operator 2026-07-26). We record it on the existing row (last_pushed_at /
// push_count) and, when the row is an EO-clean record we have already mailed,
// return it to 'ready' so the router stages it again as new data. Rows that are
// suppressed, dead-lettered, in-flight ('claimed') or still awaiting EO are
// left exactly as they are — a re-push never revives an unmailable record.
func (ps *PartnerSlicer) bulkInsertSurvivors(ctx context.Context, b *partnerBatch, recs []partnerRawRecord) error {
	if len(recs) == 0 {
		return nil
	}
	// ON CONFLICT DO UPDATE cannot touch the same row twice in one statement,
	// and a single slice can legitimately carry the same email more than once.
	// Collapse duplicates here (first wins) or the whole slice errors out.
	recs = dedupeByMD5(recs)

	const cols = 9
	args := make([]interface{}, 0, len(recs)*cols)
	placeholders := make([]string, 0, len(recs))
	for i, rec := range recs {
		offset := i * cols
		placeholders = append(placeholders, fmt.Sprintf(
			"($%d::uuid, $%d::uuid, $%d::uuid, $%d::uuid, $%d, $%d, $%d, $%d, $%d::jsonb)",
			offset+1, offset+2, offset+3, offset+4, offset+5, offset+6, offset+7, offset+8, offset+9,
		))
		extra := extraMetadataJSON(rec)
		args = append(args,
			uuid.New().String(),
			b.id, b.datasetID, b.partnerID,
			b.vertical,
			rec.Email, rec.MD5, rec.ISPFamily,
			string(extra),
		)
	}

	q := fmt.Sprintf(`
		INSERT INTO partner_clean_queue
		    (id, batch_id, dataset_id, partner_id, vertical, email, email_md5, isp_family, extra_metadata)
		VALUES %s`+pcqRepushUpsertClause, strings.Join(placeholders, ","))
	_, err := ps.db.ExecContext(ctx, q, args...)
	return err
}

// pcqRepushUpsertClause is the re-push signal capture (operator 2026-07-26):
// a repeat push stamps last_pushed_at/push_count on the existing row, and
// revives ONLY a previously-mailed, EO-clean record back to 'ready' so the
// stream router re-stages it as fresh signal. Every other status — suppressed_*,
// dead_letter, claimed, pending_eo, eo_in_flight — falls through the CASE's
// ELSE untouched: a re-push must never resurrect an unmailable record.
// Package-level const so the semantics are pinned by tests (QA SEV-3).
const pcqRepushUpsertClause = `
		ON CONFLICT (vertical, email_md5) DO UPDATE
		SET last_pushed_at = now(),
		    push_count     = partner_clean_queue.push_count + 1,
		    status         = CASE
		        WHEN partner_clean_queue.status = 'mailed'
		         AND partner_clean_queue.eo_result IN ('Verified','Complainer')
		        THEN 'ready'
		        ELSE partner_clean_queue.status
		    END
	`

// dedupeByMD5 collapses repeat emails inside a single slice, keeping the first
// occurrence. Required by the ON CONFLICT DO UPDATE in bulkInsertSurvivors:
// Postgres rejects a statement that would update the same conflicting row more
// than once ("cannot affect row a second time").
func dedupeByMD5(recs []partnerRawRecord) []partnerRawRecord {
	seen := make(map[string]bool, len(recs))
	out := recs[:0:0]
	for _, rec := range recs {
		if seen[rec.MD5] {
			continue
		}
		seen[rec.MD5] = true
		out = append(out, rec)
	}
	return out
}

// bulkInsertSuppressed records the dropped (globally-suppressed) entries so
// the audit / dashboard can show them. Status='suppressed_global'.
func (ps *PartnerSlicer) bulkInsertSuppressed(ctx context.Context, b *partnerBatch, all []partnerRawRecord, survivors []partnerRawRecord) {
	survivorSet := make(map[string]bool, len(survivors))
	for _, s := range survivors {
		survivorSet[s.MD5] = true
	}
	dropped := make([]partnerRawRecord, 0, len(all)-len(survivors))
	for _, rec := range all {
		if !survivorSet[rec.MD5] {
			dropped = append(dropped, rec)
		}
	}
	if len(dropped) == 0 {
		return
	}
	const cols = 9
	args := make([]interface{}, 0, len(dropped)*cols)
	placeholders := make([]string, 0, len(dropped))
	for i, rec := range dropped {
		offset := i * cols
		placeholders = append(placeholders, fmt.Sprintf(
			"($%d::uuid, $%d::uuid, $%d::uuid, $%d::uuid, $%d, $%d, $%d, $%d, 'suppressed_global')",
			offset+1, offset+2, offset+3, offset+4, offset+5, offset+6, offset+7, offset+8,
		))
		args = append(args,
			uuid.New().String(),
			b.id, b.datasetID, b.partnerID,
			b.vertical,
			rec.Email, rec.MD5, rec.ISPFamily,
		)
	}
	q := fmt.Sprintf(`
		INSERT INTO partner_clean_queue
		    (id, batch_id, dataset_id, partner_id, vertical, email, email_md5, isp_family, status)
		VALUES %s
		ON CONFLICT DO NOTHING
	`, strings.Join(placeholders, ","))
	if _, err := ps.db.ExecContext(ctx, q, args...); err != nil {
		log.Printf("[PartnerSlicer] insert suppressed: %v", err)
	}
}

func (ps *PartnerSlicer) isDatasetPaused(ctx context.Context, datasetID string) (bool, error) {
	var paused bool
	err := ps.db.QueryRowContext(ctx,
		`SELECT paused_emergency FROM partner_datasets WHERE id = $1`, datasetID).Scan(&paused)
	if err != nil {
		return false, err
	}
	return paused, nil
}

func (ps *PartnerSlicer) markBatchError(batchID, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = ps.db.ExecContext(ctx, `
		UPDATE partner_inbound_batches
		SET last_error = $2,
		    status = 'received'
		WHERE id = $1
	`, batchID, truncateError(msg))
}

func truncateError(s string) string {
	const max = 1000
	if len(s) > max {
		return s[:max]
	}
	return s
}

// gzipReaderFromS3 wraps the S3 body in a gzip reader. Caller is responsible
// for closing the returned reader (which transitively closes nothing — the
// caller separately closes out.Body).
func gzipReaderFromS3(body io.Reader) (io.ReadCloser, error) {
	gzr, err := gzip.NewReader(body)
	if err != nil {
		return nil, fmt.Errorf("gzip.NewReader: %w", err)
	}
	return gzr, nil
}

// md5HashLower returns the lowercase hex MD5 of the input.
func md5HashLower(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// domainOf returns the substring after the last '@' of an email, lowercased.
func domainOf(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(email[at+1:])
}

// AdaptGlobalHub turns *engine.GlobalSuppressionHub into the SuppressionHub
// interface, so wiring code in main.go can pass the hub directly without
// importing engine into the worker package.
func AdaptGlobalHub(hub *engine.GlobalSuppressionHub) SuppressionHub {
	if hub == nil {
		return nil
	}
	return &globalHubAdapter{hub: hub}
}

type globalHubAdapter struct {
	hub *engine.GlobalSuppressionHub
}

func (g *globalHubAdapter) IsSuppressed(email string) bool {
	return g.hub.IsSuppressed(email)
}

func (g *globalHubAdapter) IsSuppressedByHash(md5Hash string) bool {
	return g.hub.IsSuppressedByHash(md5Hash)
}
