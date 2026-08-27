package worker

// =============================================================================
// STORAGE GUARD — State-aware replication slot / WAL / queue / acct monitoring
// =============================================================================
// Post Sev-1 (May 2026): an inactive physical replication slot pinned ~3.7 TB
// WAL on the primary. This worker watches for recurrence and for application-
// layer bloat signals (queue HTML retention, acct_raw backlog).

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	DefaultStorageGuardInterval = 5 * time.Minute
	DefaultStorageGuardReAlert  = 30 * time.Minute

	walRetainedWarnBytes  = int64(10 * 1024 * 1024 * 1024)  // 10 GB
	walRetainedSev1Bytes  = int64(100 * 1024 * 1024 * 1024) // 100 GB
	queueAcceptedHTMLWarn = int64(1_000_000)
	queueTerminalAgedWarn = int64(100_000)
	acctRawBacklogWarn    = int64(5_000_000)
)

type storageGuardInvariantKey string

const (
	sgInvUnexpectedSlot    storageGuardInvariantKey = "unexpected_slot"
	sgInvInactiveSlot      storageGuardInvariantKey = "inactive_slot"
	sgInvWALRetainedWarn   storageGuardInvariantKey = "wal_retained_warn"
	sgInvWALRetainedSev1   storageGuardInvariantKey = "wal_retained_sev1"
	sgInvQueueAcceptedHTML storageGuardInvariantKey = "queue_accepted_html"
	sgInvQueueTerminalAged storageGuardInvariantKey = "queue_terminal_aged"
	sgInvAcctRawBacklog    storageGuardInvariantKey = "acct_raw_backlog"
)

// StorageSnapshot is the last evaluated storage guard state, exposed on
// GET /health/storage for operator dashboards.
type StorageSnapshot struct {
	CheckedAt            time.Time `json:"checked_at"`
	ReplicaConfigured    bool      `json:"replica_configured"`
	ReplicationSlotCount int64     `json:"replication_slot_count"`
	MaxRetainedWALBytes  int64     `json:"max_retained_wal_bytes"`
	InactiveSlotCount    int64     `json:"inactive_slot_count"`
	QueueAcceptedHTML    int64     `json:"queue_accepted_with_html"`
	QueueTerminalAged    int64     `json:"queue_terminal_aged_14d"`
	AcctRawUnprocessed   int64     `json:"acct_raw_unprocessed"`
	Alerts               []string  `json:"alerts,omitempty"`
}

// StorageGuard evaluates storage and replication invariants on a ticker.
type StorageGuard struct {
	db                *sql.DB
	interval          time.Duration
	reAlert           time.Duration
	replicaConfigured bool

	alerter    SMSAlerter
	recipients []string

	mu         sync.Mutex
	lastAlerts map[storageGuardInvariantKey]time.Time
	snapshot   StorageSnapshot
}

// NewStorageGuard constructs a storage guard with default timings.
func NewStorageGuard(db *sql.DB, replicaConfigured bool) *StorageGuard {
	return &StorageGuard{
		db:                db,
		interval:          DefaultStorageGuardInterval,
		reAlert:           DefaultStorageGuardReAlert,
		replicaConfigured: replicaConfigured,
		lastAlerts:        make(map[storageGuardInvariantKey]time.Time),
	}
}

// SetAlerter wires Twilio SMS paging (optional).
func (g *StorageGuard) SetAlerter(alerter SMSAlerter, recipients []string) {
	if alerter == nil || len(recipients) == 0 {
		g.alerter = nil
		g.recipients = nil
		return
	}
	g.alerter = alerter
	g.recipients = append([]string(nil), recipients...)
}

// Snapshot returns the most recent evaluation (thread-safe copy).
func (g *StorageGuard) Snapshot() StorageSnapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.snapshot
}

// Start blocks until ctx is cancelled.
func (g *StorageGuard) Start(ctx context.Context) {
	log.Printf("[StorageGuard] starting (interval=%s, replica_configured=%v, recipients=%d)",
		g.interval, g.replicaConfigured, len(g.recipients))
	g.runOnce(ctx)

	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("[StorageGuard] stopping")
			return
		case <-ticker.C:
			g.runOnce(ctx)
		}
	}
}

func (g *StorageGuard) runOnce(ctx context.Context) {
	queryCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	snap := StorageSnapshot{
		CheckedAt:         time.Now().UTC(),
		ReplicaConfigured: g.replicaConfigured,
	}

	g.checkReplicationSlots(queryCtx, &snap)
	g.checkQueueHTML(queryCtx, &snap)
	g.checkQueueTerminalAged(queryCtx, &snap)
	g.checkAcctRawBacklog(queryCtx, &snap)

	g.mu.Lock()
	g.snapshot = snap
	g.mu.Unlock()
}

func (g *StorageGuard) checkReplicationSlots(ctx context.Context, snap *StorageSnapshot) {
	rows, err := g.db.QueryContext(ctx, `
		SELECT slot_name,
		       active,
		       COALESCE(
		         pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn),
		         0
		       )::bigint AS retained_bytes
		FROM pg_replication_slots
	`)
	if err != nil {
		log.Printf("[StorageGuard] replication_slots query failed: %v", err)
		return
	}
	defer rows.Close()

	var slotCount int64
	var inactiveCount int64
	var maxRetained int64

	for rows.Next() {
		var name string
		var active bool
		var retained int64
		if err := rows.Scan(&name, &active, &retained); err != nil {
			continue
		}
		slotCount++
		if !active {
			inactiveCount++
			msg := fmt.Sprintf(
				"[Project Jarvis] StorageGuard: replication slot %q is INACTIVE (retained WAL ~%.1f GB). Investigate replica health immediately.",
				name, float64(retained)/(1024*1024*1024),
			)
			snap.Alerts = append(snap.Alerts, msg)
			g.maybeAlert(ctx, sgInvInactiveSlot, msg)
		}
		if retained > maxRetained {
			maxRetained = retained
		}
	}

	snap.ReplicationSlotCount = slotCount
	snap.InactiveSlotCount = inactiveCount
	snap.MaxRetainedWALBytes = maxRetained

	if !g.replicaConfigured && slotCount > 0 {
		msg := fmt.Sprintf(
			"[Project Jarvis] StorageGuard: %d unexpected replication slot(s) with no READ_REPLICA_URL configured (max retained WAL ~%.1f GB).",
			slotCount, float64(maxRetained)/(1024*1024*1024),
		)
		snap.Alerts = append(snap.Alerts, msg)
		g.maybeAlert(ctx, sgInvUnexpectedSlot, msg)
	}

	if maxRetained >= walRetainedSev1Bytes {
		msg := fmt.Sprintf(
			"[Project Jarvis] StorageGuard SEV-1: retained WAL ~%.1f GB (threshold 100 GB). Pause heavy writes until resolved.",
			float64(maxRetained)/(1024*1024*1024),
		)
		snap.Alerts = append(snap.Alerts, msg)
		g.maybeAlert(ctx, sgInvWALRetainedSev1, msg)
		if os.Getenv("PAUSE_ENQUEUE_ON_STORAGE_GUARD") == "true" {
			log.Println("[StorageGuard] PAUSE_ENQUEUE_ON_STORAGE_GUARD=true — operator should halt purge/VACUUM FULL")
		}
	} else if maxRetained >= walRetainedWarnBytes {
		msg := fmt.Sprintf(
			"[Project Jarvis] StorageGuard: retained WAL ~%.1f GB (warning threshold 10 GB).",
			float64(maxRetained)/(1024*1024*1024),
		)
		snap.Alerts = append(snap.Alerts, msg)
		g.maybeAlert(ctx, sgInvWALRetainedWarn, msg)
	}
}

func (g *StorageGuard) checkQueueHTML(ctx context.Context, snap *StorageSnapshot) {
	var count int64
	err := g.db.QueryRowContext(ctx, `
		SELECT COUNT(*)::bigint
		FROM mailing_campaign_queue
		WHERE status = 'accepted'
		  AND html_content IS NOT NULL
	`).Scan(&count)
	if err != nil {
		log.Printf("[StorageGuard] queue accepted+html query failed: %v", err)
		return
	}
	snap.QueueAcceptedHTML = count
	if count <= queueAcceptedHTMLWarn {
		return
	}
	msg := fmt.Sprintf(
		"[Project Jarvis] StorageGuard: %d accepted queue rows still carry html_content (threshold %d). Deploy queue slimming or investigate.",
		count, queueAcceptedHTMLWarn,
	)
	snap.Alerts = append(snap.Alerts, msg)
	g.maybeAlert(ctx, sgInvQueueAcceptedHTML, msg)
}

func (g *StorageGuard) checkQueueTerminalAged(ctx context.Context, snap *StorageSnapshot) {
	var count int64
	err := g.db.QueryRowContext(ctx, `
		SELECT COUNT(*)::bigint
		FROM mailing_campaign_queue
		WHERE status IN ('accepted','cancelled','failed','dead_letter','dead_letter_strict')
		  AND COALESCE(updated_at, created_at) < NOW() - INTERVAL '14 days'
	`).Scan(&count)
	if err != nil {
		log.Printf("[StorageGuard] queue terminal aged query failed: %v", err)
		return
	}
	snap.QueueTerminalAged = count
	if count <= queueTerminalAgedWarn {
		return
	}
	msg := fmt.Sprintf(
		"[Project Jarvis] StorageGuard: %d operationally-terminal queue rows older than 14d (threshold %d). Run terminal purge.",
		count, queueTerminalAgedWarn,
	)
	snap.Alerts = append(snap.Alerts, msg)
	g.maybeAlert(ctx, sgInvQueueTerminalAged, msg)
}

func (g *StorageGuard) checkAcctRawBacklog(ctx context.Context, snap *StorageSnapshot) {
	var count int64
	err := g.db.QueryRowContext(ctx, `
		SELECT COUNT(*)::bigint FROM pmta_acct_raw WHERE processed = FALSE
	`).Scan(&count)
	if err != nil {
		if isTableNotExistsError(err) {
			return
		}
		log.Printf("[StorageGuard] acct_raw backlog query failed: %v", err)
		return
	}
	snap.AcctRawUnprocessed = count
	if count <= acctRawBacklogWarn {
		return
	}
	msg := fmt.Sprintf(
		"[Project Jarvis] StorageGuard: %d unprocessed pmta_acct_raw rows (threshold %d). Increase AcctSummaryBuilder throughput.",
		count, acctRawBacklogWarn,
	)
	snap.Alerts = append(snap.Alerts, msg)
	g.maybeAlert(ctx, sgInvAcctRawBacklog, msg)
}

func (g *StorageGuard) maybeAlert(ctx context.Context, key storageGuardInvariantKey, msg string) {
	log.Println(msg)
	if g.alerter == nil || len(g.recipients) == 0 {
		return
	}
	g.mu.Lock()
	last, seen := g.lastAlerts[key]
	if seen && time.Since(last) < g.reAlert {
		g.mu.Unlock()
		return
	}
	g.lastAlerts[key] = time.Now()
	g.mu.Unlock()

	for _, to := range g.recipients {
		to = strings.TrimSpace(to)
		if to == "" {
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if _, err := g.alerter.SendSMS(sendCtx, to, msg); err != nil {
			log.Printf("[StorageGuard] SMS send to %s failed: %v", to, err)
		}
		cancel()
	}
}
