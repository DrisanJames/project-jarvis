package pmta

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

const (
	defaultSummaryBatchSize    = 10000
	defaultSummaryTickInterval = 15 * time.Second
	summaryQueryTimeout        = 30 * time.Second
)

// AcctSummaryBuilder periodically processes unprocessed rows from pmta_acct_raw,
// resolves campaign IDs and ISP groups, and upserts rollups into
// pmta_acct_daily_summary. This makes PMTA accounting data the authoritative
// source for delivery/bounce/complaint metrics.
type AcctSummaryBuilder struct {
	db        *sql.DB
	batchSize int
	tick      time.Duration
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// NewAcctSummaryBuilder creates a new summary builder with default throughput.
func NewAcctSummaryBuilder(db *sql.DB) *AcctSummaryBuilder {
	return NewAcctSummaryBuilderWithConfig(db, defaultSummaryBatchSize, defaultSummaryTickInterval)
}

// NewAcctSummaryBuilderWithConfig creates a builder with explicit batch/tick.
func NewAcctSummaryBuilderWithConfig(db *sql.DB, batchSize int, tick time.Duration) *AcctSummaryBuilder {
	if batchSize <= 0 {
		batchSize = defaultSummaryBatchSize
	}
	if tick <= 0 {
		tick = defaultSummaryTickInterval
	}
	return &AcctSummaryBuilder{db: db, batchSize: batchSize, tick: tick}
}

// Start begins the periodic processing loop.
func (b *AcctSummaryBuilder) Start() {
	b.ctx, b.cancel = context.WithCancel(context.Background())
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		log.Println("[AcctSummary] Builder started")

		time.Sleep(10 * time.Second)
		b.processBatch()

		ticker := time.NewTicker(b.tick)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				b.processBatch()
			case <-b.ctx.Done():
				log.Println("[AcctSummary] Builder stopped")
				return
			}
		}
	}()
}

// Stop halts the builder and waits for the current cycle to finish.
func (b *AcctSummaryBuilder) Stop() {
	if b.cancel != nil {
		b.cancel()
	}
	b.wg.Wait()
}

type rawAcctRow struct {
	id              int64
	recordType      string
	recipient       string
	recipientDomain string
	bounceCat       string
	jobID           string
	vmta            string
	pool            string
	receivedAt      time.Time
}

// isSESRelayRow mirrors engine.IsPMTARelayedToSES: a "d" record on an SES
// relay VMTA/pool is a handoff to SES, not a recipient delivery, so it must not
// inflate the daily delivered rollup.
func isSESRelayRow(r rawAcctRow) bool {
	return strings.Contains(strings.ToLower(r.pool), "ses") ||
		strings.Contains(strings.ToLower(r.vmta), "ses")
}

func (b *AcctSummaryBuilder) processBatch() {
	ctx, cancel := context.WithTimeout(b.ctx, summaryQueryTimeout)
	defer cancel()

	rows, err := b.db.QueryContext(ctx, `
		SELECT id, record_type, recipient, COALESCE(recipient_domain, ''),
		       COALESCE(bounce_cat, ''), COALESCE(job_id, ''),
		       COALESCE(vmta, ''), COALESCE(pool, ''), received_at
		FROM pmta_acct_raw
		WHERE processed = FALSE
		ORDER BY id ASC
		LIMIT $1
	`, b.batchSize)
	if err != nil {
		log.Printf("[AcctSummary] query error: %v", err)
		return
	}
	defer rows.Close()

	type summaryKey struct {
		date        string
		campaignID  string // UUID string or empty
		recipientISP string
	}
	type summaryDelta struct {
		delivered          int
		relayedToSES       int
		hardBounce         int
		softBounce         int
		reputationBlocked  int
		complained         int
		deferred           int
		total              int
	}

	deltas := make(map[summaryKey]*summaryDelta)
	var processedIDs []int64
	var resolvedUpdates []struct {
		id          int64
		campaignID  string
		recipientISP string
	}

	for rows.Next() {
		var r rawAcctRow
		if err := rows.Scan(&r.id, &r.recordType, &r.recipient, &r.recipientDomain,
			&r.bounceCat, &r.jobID, &r.vmta, &r.pool, &r.receivedAt); err != nil {
			log.Printf("[AcctSummary] scan error: %v", err)
			continue
		}

		campaignID := b.resolveCampaign(ctx, r.jobID, r.recipient)
		recipientISP := isp.GroupFromDomain(r.recipientDomain)
		dateStr := r.receivedAt.UTC().Format("2006-01-02")

		key := summaryKey{date: dateStr, campaignID: campaignID, recipientISP: recipientISP}
		d, ok := deltas[key]
		if !ok {
			d = &summaryDelta{}
			deltas[key] = d
		}
		d.total++

		switch r.recordType {
		case "d":
			if isSESRelayRow(r) {
				d.relayedToSES++
			} else {
				d.delivered++
			}
		case "b":
			switch classifyBounce(r.bounceCat) {
			case bounceHard:
				d.hardBounce++
			case bounceReputation:
				d.reputationBlocked++
			default:
				d.softBounce++
			}
		case "f":
			d.complained++
		case "t", "tq":
			d.deferred++
		}

		processedIDs = append(processedIDs, r.id)
		resolvedUpdates = append(resolvedUpdates, struct {
			id           int64
			campaignID   string
			recipientISP string
		}{r.id, campaignID, recipientISP})
	}

	if len(processedIDs) == 0 {
		return
	}

	// Upsert summary deltas
	for key, d := range deltas {
		var campIDParam interface{}
		if key.campaignID != "" {
			campIDParam = key.campaignID
		}

		_, err := b.db.ExecContext(ctx, `
			INSERT INTO pmta_acct_daily_summary
				(id, summary_date, campaign_id, recipient_isp, delivered, relayed_to_ses, hard_bounced,
				 soft_bounced, reputation_blocked, complained, deferred, total_records, last_updated_at)
			VALUES (gen_random_uuid(), $1::date, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW())
			ON CONFLICT (summary_date, COALESCE(campaign_id, '00000000-0000-0000-0000-000000000000'::uuid), recipient_isp)
			DO UPDATE SET
				delivered = pmta_acct_daily_summary.delivered + EXCLUDED.delivered,
				relayed_to_ses = pmta_acct_daily_summary.relayed_to_ses + EXCLUDED.relayed_to_ses,
				hard_bounced = pmta_acct_daily_summary.hard_bounced + EXCLUDED.hard_bounced,
				soft_bounced = pmta_acct_daily_summary.soft_bounced + EXCLUDED.soft_bounced,
				reputation_blocked = pmta_acct_daily_summary.reputation_blocked + EXCLUDED.reputation_blocked,
				complained = pmta_acct_daily_summary.complained + EXCLUDED.complained,
				deferred = pmta_acct_daily_summary.deferred + EXCLUDED.deferred,
				total_records = pmta_acct_daily_summary.total_records + EXCLUDED.total_records,
				last_updated_at = NOW()
		`, key.date, campIDParam, key.recipientISP, d.delivered, d.relayedToSES, d.hardBounce,
			d.softBounce, d.reputationBlocked, d.complained, d.deferred, d.total)
		if err != nil {
			log.Printf("[AcctSummary] upsert error for %s/%s/%s: %v",
				key.date, key.campaignID, key.recipientISP, err)
		}
	}

	// Mark raw rows as processed and stamp resolved fields
	for _, u := range resolvedUpdates {
		var campPtr interface{}
		if u.campaignID != "" {
			campPtr = u.campaignID
		}
		b.db.ExecContext(ctx, `
			UPDATE pmta_acct_raw SET processed = TRUE, campaign_id = $2::uuid, recipient_isp = $3
			WHERE id = $1
		`, u.id, campPtr, u.recipientISP)
	}

	log.Printf("[AcctSummary] Processed %d raw records into %d summary buckets", len(processedIDs), len(deltas))
}

// resolveCampaign attempts to resolve a campaign UUID from the job_id field.
// Falls back to mailing_message_log lookup by recipient email.
func (b *AcctSummaryBuilder) resolveCampaign(ctx context.Context, jobID, recipient string) string {
	if _, err := uuid.Parse(jobID); err == nil {
		return jobID
	}

	recipientEmail := strings.ToLower(strings.TrimSpace(recipient))
	if recipientEmail == "" {
		return ""
	}

	var resolved sql.NullString
	b.db.QueryRowContext(ctx, `
		SELECT campaign_id::text FROM mailing_message_log
		WHERE LOWER(email) = $1
		ORDER BY sent_at DESC LIMIT 1
	`, recipientEmail).Scan(&resolved)
	if resolved.Valid {
		return resolved.String
	}
	return ""
}

// bounceClass represents the three-way bounce classification.
type bounceClass int

const (
	bounceHard       bounceClass = iota // list hygiene: invalid address, dead domain
	bounceReputation                    // provider blocks: spam-related, policy rejections
	bounceSoft                          // transient: quota, rate-limit, temp failures
)

// classifyBounce separates true hard bounces (list hygiene) from reputation
// blocks (provider rejections) and transient soft bounces.
func classifyBounce(cat string) bounceClass {
	switch cat {
	case "hard", "bad-mailbox", "bad-domain", "inactive-mailbox", "no-answer-from-host":
		return bounceHard
	case "spam-related", "policy-related", "routing-errors":
		return bounceReputation
	default:
		return bounceSoft
	}
}

// isHardBounceCat is kept for backward compatibility with existing callers.
func isHardBounceCat(cat string) bool {
	return classifyBounce(cat) == bounceHard
}
