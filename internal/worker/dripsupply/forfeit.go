package dripsupply

import (
	"context"
	"fmt"
	"time"
)

// -----------------------------------------------------------------------------
// Durable record of allowance this package DROPS (R1, R4)
// -----------------------------------------------------------------------------
//
// Two things in the refill path used to reduce a domain's day and leave nothing
// behind:
//
//   1. the burst clamp in refill() (bucket.go). It sets RefillResult.Capped and
//      throws the excess away. Nothing read Capped — RefillDomain's only caller
//      discards the whole map (executor.go:799) — so an ISP could forfeit an
//      interval of allowance every tick for a whole day and the only evidence
//      was that the numbers did not add up.
//   2. RefillDomain's governor-read error (bucket.go:799), which `continue`s
//      past the ISP: no accrual, no row touched, one log line and nothing an
//      audit can query a week later.
//
// These two tables are that record. They are NOT capacity: nothing reads them
// back into a decision. They exist so the question "why did 343,829 contracted
// become 113,764 committed" has an answer in SQL.
//
// The DDL lives here and is applied on first use rather than in
// cmd/server/main.go: runStartupMigrations has a 5 s per-statement budget and a
// timeout there is logged as "skipped, will retry next boot" and is then
// silently absent forever. A CREATE TABLE IF NOT EXISTS run by the one package
// that writes the table cannot drift from the code that needs it.

// RefillForfeitDDL is `drip_refill_forfeit`: per day×domain×ISP, the token
// allowance the burst clamp discarded.
//
// `forfeited` is in TOKENS (messages), NUMERIC because the bucket is. The three
// shape columns are the refill parameters in force when the clamp fired, so an
// operator can tell an under-paced contract (interval too coarse for the daily
// max) from a scheduler that stopped ticking.
const RefillForfeitDDL = `CREATE TABLE IF NOT EXISTS drip_refill_forfeit (
		day                 DATE NOT NULL,
		sending_domain      TEXT NOT NULL,
		isp                 TEXT NOT NULL,
		forfeited           NUMERIC NOT NULL DEFAULT 0,
		refill_per_interval NUMERIC NOT NULL DEFAULT 0,
		burst_intervals     INT NOT NULL DEFAULT 0,
		active_intervals    INT NOT NULL DEFAULT 0,
		updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (day, sending_domain, isp)
	)`

// RefillSkipDDL is `drip_refill_skip`: per day×domain×ISP, how many times the
// refill declined to accrue at all, and why.
//
// It is a SECOND table rather than a column on drip_refill_forfeit because the
// two events are not the same event and must not be summed. A forfeit is
// allowance that is GONE — last_refill_tick moved past it. A skip defers:
// last_refill_tick did not move, so the intervals are still owed and the next
// successful tick will accrue them. Recording a skip as a forfeit would
// overstate the loss; recording it nowhere is what R4 exists to fix.
const RefillSkipDDL = `CREATE TABLE IF NOT EXISTS drip_refill_skip (
		day            DATE NOT NULL,
		sending_domain TEXT NOT NULL,
		isp            TEXT NOT NULL,
		skips          INT  NOT NULL DEFAULT 0,
		reason         TEXT NOT NULL DEFAULT '',
		updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (day, sending_domain, isp)
	)`

// maxSkipReason bounds what a driver error can put in the reason column: the
// row is an operator signal, not a log sink, and an unbounded error string from
// a sick database is how a diagnostic table becomes the outage.
const maxSkipReason = 240

// ensureForfeitSchema applies both DDL statements once per process.
//
// It is an atomic flag rather than a sync.Once because a sync.Once that fails
// never runs again: a first call during a database blip would leave the process
// permanently unable to record a forfeit. This retries until it succeeds, and
// costs one atomic load after that.
func (s *Service) ensureForfeitSchema(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	if s.forfeitSchemaReady.Load() {
		return nil
	}
	for _, stmt := range []string{RefillForfeitDDL, RefillSkipDDL} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("dripsupply: ensure refill forfeit schema: %w", err)
		}
	}
	s.forfeitSchemaReady.Store(true)
	return nil
}

// recordForfeit adds `res.Forfeited` to the cell's running total.
//
// It takes a Queryer so refillOne can pass its *sql.Tx: the forfeit row and the
// balance row it describes are written in ONE transaction, so a crash between
// them cannot leave a bucket that was clamped with no record of the clamp.
//
// The upsert is ADDITIVE on forfeited (a cell can be clamped on many ticks) and
// last-writer on the three shape columns, which are the parameters in force at
// the most recent clamp.
func recordForfeit(ctx context.Context, q Queryer, day time.Time, domain, isp string, res RefillResult, w Window) error {
	if res.Forfeited <= 0 {
		return nil
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO drip_refill_forfeit (
			day, sending_domain, isp, forfeited, refill_per_interval,
			burst_intervals, active_intervals, updated_at
		) VALUES ($1::date, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (day, sending_domain, isp) DO UPDATE SET
			forfeited           = drip_refill_forfeit.forfeited + EXCLUDED.forfeited,
			refill_per_interval = EXCLUDED.refill_per_interval,
			burst_intervals     = EXCLUDED.burst_intervals,
			active_intervals    = EXCLUDED.active_intervals,
			updated_at          = NOW()
	`, dayKey(day), domain, normISP(isp), res.Forfeited, res.RefillPerInterval, w.BurstIntervals(), w.ActiveIntervals())
	if err != nil {
		return fmt.Errorf("record refill forfeit %s/%s: %w", domain, isp, err)
	}
	return nil
}

// recordRefillSkip increments the cell's skip counter and stores the latest
// cause. Never returns an error to its caller in RefillDomain: a refill that
// already could not read its governors must not also fail the whole domain
// because the diagnostic write failed. The caller logs.
func recordRefillSkip(ctx context.Context, q Queryer, day time.Time, domain, isp, reason string) error {
	if len(reason) > maxSkipReason {
		reason = reason[:maxSkipReason]
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO drip_refill_skip (day, sending_domain, isp, skips, reason, updated_at)
		VALUES ($1::date, $2, $3, 1, $4, NOW())
		ON CONFLICT (day, sending_domain, isp) DO UPDATE SET
			skips      = drip_refill_skip.skips + 1,
			reason     = EXCLUDED.reason,
			updated_at = NOW()
	`, dayKey(day), domain, normISP(isp), reason)
	if err != nil {
		return fmt.Errorf("record refill skip %s/%s: %w", domain, isp, err)
	}
	return nil
}
