package worker

// =============================================================================
// PARTNER-DRIP DIGEST — periodic Slack progress digest for one drip vertical
// =============================================================================
// Posts a recurring snapshot of a single partner-drip vertical (queue depth by
// ISP, sent in the last 24h, observed drain ETA, and whether the vertical is
// live) to a dedicated Slack channel. Built for the internal Sam's Club drip
// (#yahoo-aol-sams-drip) so the operator can watch the 1.4M Yahoo/AOL drain
// without running ad-hoc SQL. Best-effort: every query is time-boxed and
// failures are logged, never fatal.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/notify"
	"github.com/lib/pq"
)

// PartnerDripDigestMonitor posts a periodic digest for one vertical.
type PartnerDripDigestMonitor struct {
	db       *sql.DB
	notifier notify.Notifier
	vertical string
	label    string
	interval time.Duration
}

// NewPartnerDripDigestMonitor builds the monitor. notifier may be nil (Noop).
// Posts every 6h by default.
func NewPartnerDripDigestMonitor(db *sql.DB, notifier notify.Notifier, vertical, label string) *PartnerDripDigestMonitor {
	if notifier == nil {
		notifier = notify.NoopNotifier{}
	}
	if label == "" {
		label = vertical
	}
	return &PartnerDripDigestMonitor{
		db:       db,
		notifier: notifier,
		vertical: vertical,
		label:    label,
		interval: 6 * time.Hour,
	}
}

// Start runs the digest loop until ctx is cancelled.
func (m *PartnerDripDigestMonitor) Start(ctx context.Context) {
	if m.db == nil || m.vertical == "" {
		log.Printf("[PartnerDripDigest] disabled (db or vertical missing)")
		return
	}
	if _, noop := m.notifier.(notify.NoopNotifier); noop {
		log.Printf("[PartnerDripDigest] %s: no Slack transport — digests log only", m.vertical)
	}
	log.Printf("[PartnerDripDigest] started vertical=%s interval=%s transport=%s",
		m.vertical, m.interval, m.notifier.Name())

	// Brief startup delay so migrations/boot settle before first query.
	select {
	case <-ctx.Done():
		return
	case <-time.After(3 * time.Minute):
	}
	m.digestOnce(ctx)

	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[PartnerDripDigest] stopping vertical=%s", m.vertical)
			return
		case <-t.C:
			m.digestOnce(ctx)
		}
	}
}

type ispDigestRow struct {
	isp       string
	remaining int
	ready     int
	mailed    int
	sent24h   int
}

func (m *PartnerDripDigestMonitor) digestOnce(ctx context.Context) {
	qctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	// Run inside a read-only tx with a relaxed SET LOCAL statement_timeout. The
	// app pool's default statement_timeout is tighter than this aggregate needs
	// under production load, and the server (not the client context) was
	// cancelling the digest. SET LOCAL auto-resets at tx end, so it never leaks
	// to other pooled users. The query itself is scoped by dataset_id (the only
	// indexed lead column on this multi-million-row table; a vertical filter has
	// no supporting index) and typically returns in well under a second.
	tx, err := m.db.BeginTx(qctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		log.Printf("[PartnerDripDigest] %s: begin tx failed: %v", m.vertical, err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(qctx, `SET LOCAL statement_timeout = '150s'`); err != nil {
		log.Printf("[PartnerDripDigest] %s: set timeout failed: %v", m.vertical, err)
		return
	}

	var dsIDs []string
	if drows, derr := tx.QueryContext(qctx, `SELECT id::text FROM partner_datasets WHERE vertical = $1`, m.vertical); derr == nil {
		for drows.Next() {
			var id string
			if drows.Scan(&id) == nil {
				dsIDs = append(dsIDs, id)
			}
		}
		drows.Close()
	}

	const aggCols = `
		       COUNT(*) FILTER (WHERE status IN ('hold','ready'))                                  AS remaining,
		       COUNT(*) FILTER (WHERE status = 'ready')                                            AS ready,
		       COUNT(*) FILTER (WHERE status = 'mailed')                                           AS mailed,
		       COUNT(*) FILTER (WHERE status = 'mailed' AND mailed_at > NOW() - INTERVAL '24 hours') AS sent24h`
	var rows *sql.Rows
	if len(dsIDs) > 0 {
		rows, err = tx.QueryContext(qctx, `
			SELECT COALESCE(NULLIF(isp_family, ''), 'other') AS isp,`+aggCols+`
			FROM partner_clean_queue
			WHERE dataset_id = ANY($1::uuid[])
			GROUP BY 1
		`, pq.Array(dsIDs))
	} else {
		rows, err = tx.QueryContext(qctx, `
			SELECT COALESCE(NULLIF(isp_family, ''), 'other') AS isp,`+aggCols+`
			FROM partner_clean_queue
			WHERE vertical = $1
			GROUP BY 1
		`, m.vertical)
	}
	if err != nil {
		log.Printf("[PartnerDripDigest] %s: query failed: %v", m.vertical, err)
		return
	}
	var data []ispDigestRow
	var totRemaining, totReady, totMailed, totSent24 int
	for rows.Next() {
		var r ispDigestRow
		if err := rows.Scan(&r.isp, &r.remaining, &r.ready, &r.mailed, &r.sent24h); err != nil {
			continue
		}
		data = append(data, r)
		totRemaining += r.remaining
		totReady += r.ready
		totMailed += r.mailed
		totSent24 += r.sent24h
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("[PartnerDripDigest] %s: scan error: %v", m.vertical, err)
		return
	}
	if len(data) == 0 {
		// Nothing loaded for this vertical — stay quiet (avoids noise before load).
		return
	}

	// Is the vertical live? (drip_state row present = orchestrator processes it)
	var live bool
	_ = m.db.QueryRowContext(qctx,
		`SELECT EXISTS(SELECT 1 FROM partner_drip_state WHERE vertical = $1)`, m.vertical).Scan(&live)

	sort.Slice(data, func(i, j int) bool { return data[i].remaining > data[j].remaining })

	var b strings.Builder
	status := "PAUSED (no drip_state row)"
	if live {
		status = "LIVE"
	}
	fmt.Fprintf(&b, "status: %s\n", status)
	fmt.Fprintf(&b, "remaining: %s  (ready %s · hold %s)\n",
		comma(totRemaining), comma(totReady), comma(totRemaining-totReady))
	fmt.Fprintf(&b, "mailed total: %s  ·  sent last 24h: %s\n", comma(totMailed), comma(totSent24))
	if totSent24 > 0 {
		etaDays := (totRemaining + totSent24 - 1) / totSent24
		fmt.Fprintf(&b, "drain ETA at last-24h rate: ~%d days\n", etaDays)
	}
	b.WriteString("\nby ISP (remaining · sent 24h):\n")
	for _, r := range data {
		if r.remaining == 0 && r.sent24h == 0 {
			continue
		}
		fmt.Fprintf(&b, "  %-10s %10s   (24h: %s)\n", r.isp, comma(r.remaining), comma(r.sent24h))
	}

	title := fmt.Sprintf("Sam's Club drip — %s digest", m.label)
	if err := m.notifier.Notify(title, b.String()); err != nil {
		log.Printf("[PartnerDripDigest] %s: Slack post failed: %v", m.vertical, err)
		return
	}
	log.Printf("[PartnerDripDigest] %s: posted digest (remaining=%d sent24h=%d live=%v)",
		m.vertical, totRemaining, totSent24, live)
}

// comma formats an int with thousands separators (e.g. 1401095 -> "1,401,095").
func comma(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var out []byte
	pre := len(s) % 3
	if pre > 0 {
		out = append(out, s[:pre]...)
	}
	for i := pre; i < len(s); i += 3 {
		if len(out) > 0 {
			out = append(out, ',')
		}
		out = append(out, s[i:i+3]...)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}
