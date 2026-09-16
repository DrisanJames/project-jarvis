package api

// Reservoir totals — the "what do we actually hold" surface for the Data
// Partners screen.
//
//   GET /api/mailing/data-partners/reservoir        (60s cached)
//   GET /api/mailing/data-partners/reservoir?refresh=1
//
// ONE query does the work: partner_clean_queue grouped by
// (vertical, status, isp_family). Measured 2026-09-15 against prod: 1,257
// groups over 15.0M rows in 8.7s. Everything the response carries — grand
// total, by-status, by-vertical, and the per-ISP cells — is derived from that
// single result set in Go, so the endpoint never issues a second scan.
//
// 8.7s is too slow to serve on every poll, and these handlers had no caching
// pattern to inherit, so this file owns a small process-local TTL cache. A
// concurrent caller that arrives while a refresh is in flight waits on the
// same refresh rather than starting its own (single-flight), because two
// overlapping full scans of a 15M-row table is exactly the kind of load the
// portal should never generate.
//
// Org note: the partner tables carry no organization_id column (same scope
// note as partner_csv_ingest.go) — getOrgID(r) is echoed back, not filtered on.

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"sync"
	"time"
)

const (
	reservoirCacheTTL     = 60 * time.Second
	reservoirQueryTimeout = 90 * time.Second
)

// PartnerReservoirHandler serves the reservoir rollup.
type PartnerReservoirHandler struct {
	db *sql.DB

	mu       sync.Mutex
	cached   *reservoirResponse
	cachedAt time.Time
	inFlight *reservoirFlight
}

// reservoirFlight lets concurrent callers share one refresh.
type reservoirFlight struct {
	done chan struct{}
	resp *reservoirResponse
	err  error
}

func NewPartnerReservoirHandler(db *sql.DB) *PartnerReservoirHandler {
	return &PartnerReservoirHandler{db: db}
}

type reservoirCell struct {
	Vertical string `json:"vertical"`
	Status   string `json:"status"`
	ISP      string `json:"isp"`
	Count    int64  `json:"count"`
}

type reservoirStatusTotal struct {
	Status string `json:"status"`
	Count  int64  `json:"count"`
}

type reservoirVerticalTotal struct {
	Vertical  string           `json:"vertical"`
	Total     int64            `json:"total"`
	ByStatus  map[string]int64 `json:"by_status"`
	ByISP     map[string]int64 `json:"by_isp"`
	Mailable  int64            `json:"mailable"`  // ready
	Reserve   int64            `json:"reserve"`   // held + pending_eo: needs work before it can mail
	Exhausted int64            `json:"exhausted"` // mailed + suppressed_* + dead_letter
}

type reservoirResponse struct {
	GeneratedAt   string                   `json:"generated_at"`
	OrgID         string                   `json:"organization_id"`
	Cached        bool                     `json:"cached"`
	AgeSeconds    int                      `json:"age_seconds"`
	QueryMillis   int64                    `json:"query_ms"`
	Total         int64                    `json:"total"`
	MailableTotal int64                    `json:"mailable_total"`
	ByStatus      []reservoirStatusTotal   `json:"by_status"`
	ByVertical    []reservoirVerticalTotal `json:"by_vertical"`
	Cells         []reservoirCell          `json:"cells"`
}

// statusBucket groups the queue's statuses into the three things an operator
// actually decides on. Unknown statuses count toward the total and appear in
// by_status, but are never silently folded into a bucket.
func reservoirBucket(status string) string {
	switch status {
	case "ready":
		return "mailable"
	case "held", "pending_eo":
		return "reserve"
	case "mailed", "dead_letter", "suppressed", "suppressed_eo", "suppressed_global", "engaged", "claimed":
		return "exhausted"
	default:
		return ""
	}
}

func (h *PartnerReservoirHandler) HandleGetReservoir(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	force := r.URL.Query().Get("refresh") == "1"

	resp, err := h.snapshot(r.Context(), force)
	if err != nil {
		writeJSONError(w, "reservoir_query_failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Copy before stamping per-request fields: the cached value is shared.
	out := *resp
	out.OrgID = orgID
	out.AgeSeconds = int(time.Since(h.cachedTime()).Seconds())
	out.Cached = out.AgeSeconds > 0
	writeJSON(w, http.StatusOK, out)
}

func (h *PartnerReservoirHandler) cachedTime() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cachedAt
}

// snapshot returns the cached rollup when it is fresh, otherwise refreshes it.
// Only one refresh runs at a time; everyone else waits for it.
func (h *PartnerReservoirHandler) snapshot(ctx context.Context, force bool) (*reservoirResponse, error) {
	h.mu.Lock()
	if !force && h.cached != nil && time.Since(h.cachedAt) < reservoirCacheTTL {
		c := h.cached
		h.mu.Unlock()
		return c, nil
	}
	if f := h.inFlight; f != nil {
		h.mu.Unlock()
		select {
		case <-f.done:
			return f.resp, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	flight := &reservoirFlight{done: make(chan struct{})}
	h.inFlight = flight
	h.mu.Unlock()

	// Detach from the caller's context: a refresh that survives one cancelled
	// request still fills the cache for the next one, and a cancelled HTTP
	// request must not leave every other waiter with a half-finished scan.
	qctx, cancel := context.WithTimeout(context.Background(), reservoirQueryTimeout)
	defer cancel()
	resp, err := h.query(qctx)

	h.mu.Lock()
	if err == nil {
		h.cached, h.cachedAt = resp, time.Now()
	}
	h.inFlight = nil
	h.mu.Unlock()

	flight.resp, flight.err = resp, err
	close(flight.done)
	return resp, err
}

func (h *PartnerReservoirHandler) query(ctx context.Context) (*reservoirResponse, error) {
	started := time.Now()

	// The primary pool pins statement_timeout=30000 in its DSN
	// (cmd/server/main.go:226), and this scan measures 8.4-9.2s against prod —
	// only ~3x of headroom. On 2026-09-16 the first prod call landed during the
	// deploy's boot storm, ran past 30s, and the endpoint answered 500 instead
	// of returning totals. SET LOCAL binds only inside a transaction, which is
	// the same reason internal/domainagent/scorecard.go:76 opens one for its
	// rollup. Read-only tx, rolled back — nothing here writes.
	tx, err := h.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only scan; nothing to commit
	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '90s'`); err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT COALESCE(vertical, '(none)')   AS vertical,
		       COALESCE(status, '(none)')     AS status,
		       COALESCE(isp_family, '(none)') AS isp_family,
		       COUNT(*)                       AS n
		FROM partner_clean_queue
		GROUP BY 1, 2, 3
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	resp := &reservoirResponse{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Cells:       make([]reservoirCell, 0, 1400),
	}
	statusTotals := map[string]int64{}
	verticals := map[string]*reservoirVerticalTotal{}

	for rows.Next() {
		var c reservoirCell
		if err := rows.Scan(&c.Vertical, &c.Status, &c.ISP, &c.Count); err != nil {
			return nil, err
		}
		resp.Cells = append(resp.Cells, c)
		resp.Total += c.Count
		statusTotals[c.Status] += c.Count

		v, ok := verticals[c.Vertical]
		if !ok {
			v = &reservoirVerticalTotal{
				Vertical: c.Vertical,
				ByStatus: map[string]int64{},
				ByISP:    map[string]int64{},
			}
			verticals[c.Vertical] = v
		}
		v.Total += c.Count
		v.ByStatus[c.Status] += c.Count
		v.ByISP[c.ISP] += c.Count
		switch reservoirBucket(c.Status) {
		case "mailable":
			v.Mailable += c.Count
			resp.MailableTotal += c.Count
		case "reserve":
			v.Reserve += c.Count
		case "exhausted":
			v.Exhausted += c.Count
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for s, n := range statusTotals {
		resp.ByStatus = append(resp.ByStatus, reservoirStatusTotal{Status: s, Count: n})
	}
	sort.Slice(resp.ByStatus, func(i, j int) bool { return resp.ByStatus[i].Count > resp.ByStatus[j].Count })

	for _, v := range verticals {
		resp.ByVertical = append(resp.ByVertical, *v)
	}
	// Mailable first: the operator's question is "what can I send tomorrow".
	sort.Slice(resp.ByVertical, func(i, j int) bool {
		if resp.ByVertical[i].Mailable != resp.ByVertical[j].Mailable {
			return resp.ByVertical[i].Mailable > resp.ByVertical[j].Mailable
		}
		return resp.ByVertical[i].Total > resp.ByVertical[j].Total
	})

	resp.QueryMillis = time.Since(started).Milliseconds()
	return resp, nil
}
