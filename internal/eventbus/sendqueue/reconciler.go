package sendqueue

import (
	"context"
	"crypto/sha1" // nolint:gosec // uuidv5 per RFC 4122 uses SHA-1 by design
	"database/sql"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// =============================================================================
// UNLANDED-WAVE SWEEPER (REQ-089) — the reconciler the SK-4 path actually needs.
// =============================================================================
// What it replaces: LedgerReconciler.reconcilePass, which ticked every 60s over
// mailing_send_ledger — a table NOTHING on the live path wrote (0 rows after 18h
// of routing on 2026-09-01) while 219,237 produced-but-not-landed recipients sat
// invisible to it by construction (send-transport SEV-1 #4). That reconciler now
// lives in ledger_reconciler.go, unwired, as the partner of the equally-unwired
// SK-2 KafkaSendConsumer.
//
// What this one does instead — it reconciles against the DB, not a ledger:
//
//	a routed wave is 'produced' with produced_recipients
//	its plan_recipients are 'queued'
//	=> every one of them MUST have a mailing_campaign_queue row
//
// Any that does not (a record parked in the DLQ, a produce that failed after
// COMMIT, a consumer that never came back) is REBUILT by running the same
// queueInsertSQL the dispatcher and the consumer run, under the SAME
// deterministic uuidv5(campaign, subscriber, wave) idempotency key. That makes
// the rebuild idempotent twice over: the pre-check skips keys that already
// exist, and the INSERT's ON CONFLICT makes a racing Kafka record a no-op. A
// second pass over the same wave inserts 0.
//
// It needs no Kafka, no ledger, and no producer: everything it needs to rebuild
// a row is already durable in mailing_campaign_plan_recipients.
//
// It is also the promoter of last resort: a wave whose records are all present
// but whose landed_recipients never caught up (a failed write-back, a record
// that landed before this column existed) is promoted 'produced' -> 'completed'
// here, so a wave can never wedge a campaign's completion forever.

const (
	// DefaultSweepInterval is how often a pass runs.
	DefaultSweepInterval = 60 * time.Second
	// DefaultSweepGrace is how long a 'produced' wave is left alone before its
	// missing rows are treated as lost rather than in flight. 10 minutes is
	// well past a healthy consumer's drain (measured 20-25 rows/s aggregate on
	// 2026-09-01) and well inside the send window.
	DefaultSweepGrace = 10 * time.Minute
	// DefaultSweepWaveLimit bounds waves examined per pass.
	DefaultSweepWaveLimit = 25
	// DefaultSweepRowLimit bounds plan_recipients examined per wave per pass,
	// so one enormous wave can neither blow the 30s pass budget nor starve the
	// others. A wave with more missing rows than this is finished on the next
	// pass (the sweep is incremental by construction — every pass that inserts
	// rows moves landed_recipients closer to produced_recipients).
	DefaultSweepRowLimit = 5000
)

// sweepNamespace MUST equal worker.outboxNamespace: the rebuilt row has to carry
// the SAME uuidv5 key the dispatcher put on the Kafka command, or the rebuild
// would create a SECOND queue row for a recipient whose command is still parked
// in the broker — a double send. Duplicated here rather than imported because
// internal/worker imports this package (importing back is a cycle); the equality
// is pinned by TestSweepIdempotencyKeyMatchesWorker in internal/worker, which
// can see both.
var sweepNamespace = uuid.MustParse("2f9c14c6-7c4f-4d3a-93ef-8e7d4ce3a2d1")

// SweepIdempotencyKey mirrors worker.OutboxIdempotencyKey byte for byte.
func SweepIdempotencyKey(campaignID, subscriberID, waveID uuid.UUID) uuid.UUID {
	data := make([]byte, 0, 48)
	data = append(data, campaignID[:]...)
	data = append(data, subscriberID[:]...)
	data = append(data, waveID[:]...)
	h := sha1.New() // nolint:gosec // RFC 4122 v5
	h.Write(sweepNamespace[:])
	h.Write(data)
	sum := h.Sum(nil)
	var k uuid.UUID
	copy(k[:], sum[:16])
	k[6] = (k[6] & 0x0f) | 0x50 // version 5
	k[8] = (k[8] & 0x3f) | 0x80 // variant RFC 4122
	return k
}

// selectUnlandedWavesSQL finds routed waves past grace that still owe rows.
// Plan (EXPLAIN, prod 2026-09-01): Index Scan using idx_campaign_waves_due
// (status), Filter on the counters + completed_at — cost 8.59.
const selectUnlandedWavesSQL = `
SELECT w.id, w.campaign_id, w.isp_plan_id, w.scheduled_at,
       w.produced_recipients, w.landed_recipients
FROM mailing_campaign_waves w
WHERE w.status = 'produced'
  AND w.completed_at < NOW() - $1::interval
ORDER BY w.completed_at ASC
LIMIT $2`

// selectWaveContentSQL resolves what a rebuilt row needs for CONTENT. Priority:
//
//  1. a sibling row of the SAME wave that already landed — byte-identical
//     content reference (snapshot id) and priority to what the wave produced;
//  2. the content snapshot this wave itself created.
//
// If neither resolves, the wave is SKIPPED, loudly. A queue row with neither a
// content_snapshot_id nor inline html sends an EMPTY email; rebuilding from the
// campaign's current html_content is not a safe substitute either (the
// dispatcher sanitizes tracking URLs at dispatch, and the campaign row may have
// been edited since), so "no content" is a refusal, never a guess.
const selectWaveSnapshotFromQueueSQL = `
SELECT content_snapshot_id, priority
FROM mailing_campaign_queue
WHERE wave_id = $1 AND content_snapshot_id IS NOT NULL
LIMIT 1`

const selectWaveSnapshotFromSnapshotsSQL = `
SELECT id
FROM mailing_content_snapshots
WHERE wave_id = $1
ORDER BY created_at DESC
LIMIT 1`

// selectWaveSubjectSQL reads the campaign's base subject for rebuilt rows.
//
// NOTE (deliberate, documented deviation): the dispatcher applies a
// deterministic per-recipient subject mutation (mutateSubjectLine — synonym
// swap / punctuation rotation, for hash-fingerprint diversification) that lives
// in internal/worker and cannot be reached from here without an import cycle. A
// rebuilt row therefore carries the campaign's BASE subject — the
// operator-approved copy — instead of that recipient's variant. This affects
// only the trailing remainder a healthy path never produces, and the failure it
// replaces is the recipient getting no email at all.
const selectWaveSubjectSQL = `
SELECT COALESCE(c.subject, '') FROM mailing_campaigns c WHERE c.id = $1`

// selectUnlandedPlanRecipientsSQL claims the wave's accounted-for recipients.
// Plan (EXPLAIN, prod 2026-09-01): Bitmap Index Scan on
// idx_campaign_plan_recipients_wave + status filter — cost 4756 for a 767-row
// wave. No FOR UPDATE: the sweep never mutates plan_recipients, and the INSERT
// it drives is idempotent, so two ECS tasks running the same pass simply race to
// a no-op.
const selectUnlandedPlanRecipientsSQL = `
SELECT pr.subscriber_id, pr.recipient_isp, pr.selection_rank,
       pr.audience_source_type, COALESCE(pr.audience_source_id::text, '')
FROM mailing_campaign_plan_recipients pr
WHERE pr.wave_id = $1
  AND pr.status = 'queued'
ORDER BY pr.selection_rank ASC
LIMIT $2`

// selectExistingKeysSQL is the "did it already land" probe. Index Only Scan on
// uq_mcq_idempotency_key (EXPLAIN, prod) — the same unique index the INSERT's
// ON CONFLICT uses, so the pre-check and the write agree by construction.
const selectExistingKeysSQL = `
SELECT idempotency_key FROM mailing_campaign_queue
WHERE idempotency_key = ANY($1::uuid[])`

// sweepLandedSQL credits a rebuild batch and promotes the wave when the rebuild
// closed the gap. Same CASE-guarded shape as the consumer's markLandedSQL, so
// promotion is decided under the row lock no matter which writer gets there
// first (two ECS tasks, or a task and the consumer).
const sweepLandedSQL = `
UPDATE mailing_campaign_waves
SET landed_recipients = landed_recipients + $2,
    status = CASE WHEN status = 'produced' AND landed_recipients + $2 >= produced_recipients
                  THEN 'completed' ELSE status END,
    enqueued_recipients = CASE WHEN status = 'produced' AND landed_recipients + $2 >= produced_recipients
                               THEN landed_recipients + $2 ELSE enqueued_recipients END,
    updated_at = NOW()
WHERE id = $1`

// sweepPromoteSQL is the backstop: nothing is missing, so whatever
// landed_recipients says, this wave is done. It sets the counter to the truth
// (the actual queue-row count for the wave) rather than incrementing blindly.
const sweepPromoteSQL = `
UPDATE mailing_campaign_waves w
SET landed_recipients = GREATEST(w.landed_recipients,
        (SELECT COUNT(*) FROM mailing_campaign_queue q WHERE q.wave_id = w.id)),
    enqueued_recipients = GREATEST(w.enqueued_recipients,
        (SELECT COUNT(*) FROM mailing_campaign_queue q WHERE q.wave_id = w.id)),
    status = 'completed',
    updated_at = NOW()
WHERE w.id = $1 AND w.status = 'produced'`

// sweeperDB is the seam the sweep runs through so the whole pass is testable
// with sqlmock. *sql.DB satisfies it.
type sweeperDB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// SweepStats is the running total of what the sweeper has done, published for
// /health so "reconciler_running: true" finally means something measurable.
type SweepStats struct {
	Passes      uint64 `json:"passes"`
	WavesSwept  uint64 `json:"waves_swept"`
	RowsRebuilt uint64 `json:"rows_rebuilt"`
	WavesDone   uint64 `json:"waves_promoted"`
	Unresolved  uint64 `json:"waves_unresolved_content"`
	Errors      uint64 `json:"errors"`
}

// UnlandedWaveSweeper periodically rebuilds queue rows that a routed wave
// produced but that never landed, and promotes waves whose landing is complete.
// Safe under two ECS tasks: every write is idempotent or status-guarded.
type UnlandedWaveSweeper struct {
	db        *sql.DB
	interval  time.Duration
	grace     time.Duration
	waveLimit int
	rowLimit  int

	passes      atomic.Uint64
	wavesSwept  atomic.Uint64
	rowsRebuilt atomic.Uint64
	wavesDone   atomic.Uint64
	unresolved  atomic.Uint64
	errors      atomic.Uint64
}

// NewUnlandedWaveSweeper constructs a sweeper with default timings. Non-positive
// overrides fall back to the defaults so a caller cannot accidentally disable it.
func NewUnlandedWaveSweeper(db *sql.DB, interval, grace time.Duration) *UnlandedWaveSweeper {
	s := &UnlandedWaveSweeper{
		db:        db,
		interval:  DefaultSweepInterval,
		grace:     DefaultSweepGrace,
		waveLimit: DefaultSweepWaveLimit,
		rowLimit:  DefaultSweepRowLimit,
	}
	if interval > 0 {
		s.interval = interval
	}
	if grace > 0 {
		s.grace = grace
	}
	return s
}

// WithLimits overrides the per-pass bounds (tests, and an operator env if one is
// ever added). Non-positive values are ignored.
func (s *UnlandedWaveSweeper) WithLimits(waves, rows int) *UnlandedWaveSweeper {
	if waves > 0 {
		s.waveLimit = waves
	}
	if rows > 0 {
		s.rowLimit = rows
	}
	return s
}

// Stats returns the running totals.
func (s *UnlandedWaveSweeper) Stats() SweepStats {
	return SweepStats{
		Passes:      s.passes.Load(),
		WavesSwept:  s.wavesSwept.Load(),
		RowsRebuilt: s.rowsRebuilt.Load(),
		WavesDone:   s.wavesDone.Load(),
		Unresolved:  s.unresolved.Load(),
		Errors:      s.errors.Load(),
	}
}

// Start blocks until ctx is cancelled, running one pass immediately so a restart
// that coincides with a stranded wave recovers within milliseconds rather than
// one interval. Every ECS bounce re-enters here; a pass is idempotent, so a
// double start (two tasks) only means the work is found by whichever gets there
// first.
func (s *UnlandedWaveSweeper) Start(ctx context.Context) {
	log.Printf("[UnlandedWaveSweeper] Starting (interval=%s, grace=%s, waves/pass=%d, rows/wave/pass=%d)",
		s.interval, s.grace, s.waveLimit, s.rowLimit)
	s.sweepOnce(ctx)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("[UnlandedWaveSweeper] Stopping")
			return
		case <-ticker.C:
			s.sweepOnce(ctx)
		}
	}
}

// sweepOnce runs one pass under a bounded timeout (the prod statement_timeout is
// 30s; the pass must not outlive it).
func (s *UnlandedWaveSweeper) sweepOnce(ctx context.Context) {
	passCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s.passes.Add(1)
	sweepUnlandedWaves(passCtx, s.db, s)
}

// unlandedWave is one candidate row from selectUnlandedWavesSQL.
type unlandedWave struct {
	id          uuid.UUID
	campaignID  uuid.UUID
	ispPlanID   uuid.UUID
	scheduledAt time.Time
	produced    int
	landed      int
}

// sweepUnlandedWaves is the broker-free, unit-tested body of one pass. It is the
// direct replacement for LedgerReconciler.reconcilePass.
func sweepUnlandedWaves(ctx context.Context, db sweeperDB, s *UnlandedWaveSweeper) {
	waves, err := selectUnlandedWaves(ctx, db, s.grace, s.waveLimit)
	if err != nil {
		s.errors.Add(1)
		log.Printf("[UnlandedWaveSweeper] candidate query failed: %v", err)
		return
	}
	if len(waves) > 0 {
		// One line per pass that FOUND something: an operator grepping
		// CloudWatch for this worker sees either nothing (healthy) or the
		// backlog it is working through. Totals since boot ride along so the
		// line is self-contained without a /health round trip.
		st := s.Stats()
		log.Printf("[UnlandedWaveSweeper] pass: %d wave(s) in 'produced' past %s grace (totals since boot: swept=%d rebuilt=%d promoted=%d unresolved=%d errors=%d)",
			len(waves), s.grace, st.WavesSwept, st.RowsRebuilt, st.WavesDone, st.Unresolved, st.Errors)
	}
	for _, w := range waves {
		if ctx.Err() != nil {
			return // shutdown / pass budget exhausted: stop cleanly, resume next pass
		}
		s.wavesSwept.Add(1)
		rebuilt, missing, err := sweepOneWave(ctx, db, s, w)
		if err != nil {
			s.errors.Add(1)
			log.Printf("[UnlandedWaveSweeper] wave %s sweep failed: %v", w.id, err)
			continue
		}
		if rebuilt > 0 {
			s.rowsRebuilt.Add(uint64(rebuilt))
			log.Printf("[UnlandedWaveSweeper] wave %s: REBUILT %d queue row(s) that were produced but never landed (produced=%d landed=%d)",
				w.id, rebuilt, w.produced, w.landed)
			if _, err := db.ExecContext(ctx, sweepLandedSQL, w.id, rebuilt); err != nil {
				s.errors.Add(1)
				log.Printf("[UnlandedWaveSweeper] wave %s landed write-back failed: %v", w.id, err)
			}
			continue
		}
		if missing == 0 {
			// Nothing is owed: every accounted-for recipient has a queue row.
			// The wave's counter simply never caught up (a failed write-back,
			// or rows that landed before this column existed). Promote it so it
			// cannot hold a campaign in 'sending' forever.
			res, err := db.ExecContext(ctx, sweepPromoteSQL, w.id)
			if err != nil {
				s.errors.Add(1)
				log.Printf("[UnlandedWaveSweeper] wave %s promote failed: %v", w.id, err)
				continue
			}
			if n, _ := res.RowsAffected(); n > 0 {
				s.wavesDone.Add(1)
				log.Printf("[UnlandedWaveSweeper] wave %s: all produced rows are present — promoted 'produced' -> 'completed' (produced=%d, counter was %d)",
					w.id, w.produced, w.landed)
			}
		}
	}
}

// selectUnlandedWaves reads the candidate waves for this pass.
func selectUnlandedWaves(ctx context.Context, db sweeperDB, grace time.Duration, limit int) ([]unlandedWave, error) {
	rows, err := db.QueryContext(ctx, selectUnlandedWavesSQL, grace.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []unlandedWave
	for rows.Next() {
		var w unlandedWave
		if err := rows.Scan(&w.id, &w.campaignID, &w.ispPlanID, &w.scheduledAt, &w.produced, &w.landed); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// sweepOneWave rebuilds the missing queue rows of ONE wave. It returns
// (rowsInserted, rowsMissing, error): rowsMissing is how many accounted-for
// recipients had no queue row, rowsInserted how many the sweep actually created
// (they differ only when a racing Kafka record won the ON CONFLICT — which is
// exactly why a second pass over the same wave inserts 0).
func sweepOneWave(ctx context.Context, db sweeperDB, s *UnlandedWaveSweeper, w unlandedWave) (inserted, missing int, err error) {
	recips, err := selectWavePlanRecipients(ctx, db, w.id, s.rowLimit)
	if err != nil {
		return 0, 0, fmt.Errorf("plan recipients: %w", err)
	}
	if len(recips) == 0 {
		return 0, 0, nil
	}

	keys := make([]string, 0, len(recips))
	keyByIndex := make([]uuid.UUID, len(recips))
	for i, r := range recips {
		k := SweepIdempotencyKey(w.campaignID, r.subscriberID, w.id)
		keyByIndex[i] = k
		keys = append(keys, k.String())
	}
	present, err := selectExistingKeys(ctx, db, keys)
	if err != nil {
		return 0, 0, fmt.Errorf("existing keys: %w", err)
	}

	var todo []int
	for i := range recips {
		if _, ok := present[keyByIndex[i]]; !ok {
			todo = append(todo, i)
		}
	}
	missing = len(todo)
	if missing == 0 {
		return 0, 0, nil
	}

	snapshotID, priority, subject, ok, err := resolveWaveRowContext(ctx, db, w)
	if err != nil {
		return 0, missing, fmt.Errorf("row context: %w", err)
	}
	if !ok {
		s.unresolved.Add(1)
		log.Printf("[UnlandedWaveSweeper] wave %s: %d row(s) missing but NO content reference could be resolved (no landed sibling row, no snapshot for the wave) — REFUSING to rebuild rather than send an empty body. Wave left 'produced'; it will show on send_liveness until an operator resolves it.",
			w.id, missing)
		return 0, missing, nil
	}

	for _, i := range todo {
		if ctx.Err() != nil {
			return inserted, missing, ctx.Err()
		}
		r := recips[i]
		cmd := SendCommand{
			IdempotencyKey:     keyByIndex[i],
			QueueID:            uuid.New(),
			CampaignID:         w.campaignID,
			SubscriberID:       r.subscriberID,
			WaveID:             w.id,
			ISPPlanID:          w.ispPlanID,
			ISP:                r.recipientISP,
			RecipientISP:       r.recipientISP,
			Subject:            subject,
			SelectionRank:      r.selectionRank,
			AudienceSourceType: r.audienceSourceType,
			AudienceSourceID:   r.audienceSourceID,
			ContentSnapshotID:  snapshotID,
			ScheduledAtUnix:    w.scheduledAt.Unix(),
			Priority:           priority,
		}
		// The SAME queueInsertSQL the dispatcher and the QueueWriterConsumer
		// run — one INSERT statement in this codebase, so a rebuilt row can
		// never drift from a normally-enqueued one.
		didInsert, ierr := InsertQueueRow(ctx, db, cmd)
		if ierr != nil {
			return inserted, missing, fmt.Errorf("rebuild insert key=%s: %w", cmd.IdempotencyKey, ierr)
		}
		if didInsert {
			inserted++
		}
	}
	return inserted, missing, nil
}

// wavePlanRecipient is one accounted-for recipient of a produced wave.
type wavePlanRecipient struct {
	subscriberID       uuid.UUID
	recipientISP       string
	selectionRank      int
	audienceSourceType string
	audienceSourceID   string
}

func selectWavePlanRecipients(ctx context.Context, db sweeperDB, waveID uuid.UUID, limit int) ([]wavePlanRecipient, error) {
	rows, err := db.QueryContext(ctx, selectUnlandedPlanRecipientsSQL, waveID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []wavePlanRecipient
	for rows.Next() {
		var r wavePlanRecipient
		if err := rows.Scan(&r.subscriberID, &r.recipientISP, &r.selectionRank, &r.audienceSourceType, &r.audienceSourceID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func selectExistingKeys(ctx context.Context, db sweeperDB, keys []string) (map[uuid.UUID]struct{}, error) {
	present := make(map[uuid.UUID]struct{}, len(keys))
	rows, err := db.QueryContext(ctx, selectExistingKeysSQL, pq.Array(keys))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k uuid.UUID
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		present[k] = struct{}{}
	}
	return present, rows.Err()
}

// resolveWaveRowContext resolves the content reference + priority + subject a
// rebuilt row needs. ok=false means "refuse to rebuild this wave" (see
// selectWaveSnapshotFromQueueSQL's comment).
func resolveWaveRowContext(ctx context.Context, db sweeperDB, w unlandedWave) (snapshotID uuid.UUID, priority int, subject string, ok bool, err error) {
	priority = 5
	var snap uuid.NullUUID
	var prio sql.NullInt64
	scanErr := db.QueryRowContext(ctx, selectWaveSnapshotFromQueueSQL, w.id).Scan(&snap, &prio)
	switch {
	case scanErr == nil && snap.Valid:
		snapshotID = snap.UUID
		if prio.Valid && prio.Int64 > 0 {
			priority = int(prio.Int64)
		}
	case scanErr != nil && scanErr != sql.ErrNoRows:
		return uuid.Nil, 0, "", false, scanErr
	default:
		// No landed sibling: fall back to the snapshot this wave created.
		var sid uuid.UUID
		if e := db.QueryRowContext(ctx, selectWaveSnapshotFromSnapshotsSQL, w.id).Scan(&sid); e != nil {
			if e == sql.ErrNoRows {
				return uuid.Nil, 0, "", false, nil
			}
			return uuid.Nil, 0, "", false, e
		}
		snapshotID = sid
	}
	if snapshotID == uuid.Nil {
		return uuid.Nil, 0, "", false, nil
	}
	if e := db.QueryRowContext(ctx, selectWaveSubjectSQL, w.campaignID).Scan(&subject); e != nil && e != sql.ErrNoRows {
		return uuid.Nil, 0, "", false, e
	}
	return snapshotID, priority, subject, true, nil
}
