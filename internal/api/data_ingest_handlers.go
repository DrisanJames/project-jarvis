package api

// data_ingest_handlers.go serves the Data Ingest dashboard (REQ 2026-09-20, U4)
// at /api/mailing/data-ingest/*.
//
// THE ONE RULE THIS FILE EXISTS TO ENFORCE: a number is either MEASURED or it
// says it is not. Every response carries {as_of, source, fields{...}} where each
// fields KEY is a payload value key and its value is "measured" (read from the
// thing that counted it), "derived" (computed from measured inputs) or
// "not_measured". When Redis is nil, the flag is off, or the day has no
// counters, the value is null and the field is not_measured — NEVER 0. A
// confident zero on an ingest dashboard reads as "nothing arrived", which is
// the exact wrong answer when the truth is "we did not look".
//
// The field-key vocabulary is shared with the portal (builder B5): the keys of
// `fields` are the SAME strings as the value keys, flat, so the UI can look up
// a tile's status by its own name with no mapping table.
//
// LOAD BUDGET (the operator's words: "doesn't load the database"): the live
// numbers come from Redis hashes. Exactly ONE heavy query exists here — the
// grouped scan of partner_clean_queue by (dataset_id, status) behind /feeds,
// /day and /state — and it is cached 60s with single-flight, the same pattern
// and for the same reason as partner_reservoir_handlers.go (8.7s over ~15M
// rows). Per-feed and per-load counts ride covering indexes (idx_pcq_isp_family,
// idx_pcq_batch) bounded by one dataset / one day's batch ids.
//
// ORG SCOPE: data_partners carries organization_id; every feed/load query joins
// through it. partner_clean_queue does NOT carry one (same scope note as
// partner_csv_ingest.go / partner_reservoir_handlers.go), so its counts are
// echoed with the resolved org id rather than filtered by it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"

	"github.com/ignite/sparkpost-monitor/internal/dataingest"
)

// Field measurement markers — the vocabulary the portal switches on.
const (
	fieldMeasured    = "measured"
	fieldDerived     = "derived"
	fieldNotMeasured = "not_measured"
)

// Response `source` values.
const (
	sourceCounters = "counters"
	sourceRollup   = "rollup"
	sourceTable    = "table"
	sourceMixed    = "mixed"
)

const (
	dataIngestCacheTTL     = 60 * time.Second
	dataIngestQueryTimeout = 90 * time.Second
	// dataIngestRefreshEvery is the cadence of the BACKGROUND reservoir scan
	// once StartQueueStateRefresher has been called (prod). Measured cold on
	// 2026-09-20: 30–70s per scan under send load, so a request must never
	// wait on it — the refresher owns the scan and handlers read the last
	// snapshot (its age is on the response as cache_age_seconds).
	dataIngestRefreshEvery = 10 * time.Minute
	// dataIngestStreamCoalesce bounds how often the SSE stream flushes; deltas
	// arriving inside the window are merged into one frame.
	dataIngestStreamCoalesce = 2 * time.Second
	// dataIngestHeartbeat keeps proxies from closing an idle SSE connection.
	dataIngestHeartbeat  = 15 * time.Second
	dataIngestSeriesDays = 30
	dataIngestLoadsLimit = 500
)

// DataIngestService owns the dashboard's read + emit surface.
type DataIngestService struct {
	db     *sql.DB
	rdb    *redis.Client
	reader *dataingest.Reader
	hub    *dataingest.Hub
	now    func() time.Time

	// queue-state cache: ONE grouped scan of partner_clean_queue shared by
	// /feeds, /day and /state, single-flight so two portal polls never start
	// two 15M-row scans.
	mu       sync.Mutex
	cached   *queueStateSnapshot
	cachedAt time.Time
	inFlight *queueStateFlight
	// refresherOn flips when StartQueueStateRefresher runs (prod boot). From
	// then on NO request path scans partner_clean_queue: a handler reads the
	// last snapshot, or reports not_measured while the first scan is warming.
	// Off (tests, or a service mounted without the refresher) keeps the
	// inline single-flight scan so the contract is still served.
	refresherOn bool
}

// errQueueStateWarming is returned by queueState when the refresher owns the
// scan and it has not completed once yet; callers mark the reservoir fields
// not_measured rather than blocking a request behind a 15M-row scan.
var errQueueStateWarming = errors.New("queue state warming: first background scan not complete")

// NewDataIngestService builds the service. rdb MAY be nil — every live-counter
// field then renders not_measured and the dashboard falls back to the rollup
// table, which is the documented behaviour with the bus dark.
func NewDataIngestService(db *sql.DB, rdb *redis.Client) *DataIngestService {
	return &DataIngestService{
		db:     db,
		rdb:    rdb,
		reader: dataingest.NewReader(rdb),
		hub:    dataingest.DefaultHub(),
		now:    time.Now,
	}
}

// RegisterRoutes mounts the dashboard under the caller's router. In production
// that is the /api/mailing group (final URLs /api/mailing/data-ingest/*), which
// already carries session/X-Admin-Key auth; the group below re-asserts it so
// the service is closed by default wherever it is mounted.
func (s *DataIngestService) RegisterRoutes(r chi.Router) {
	r.Use(dataIngestAuth)
	r.Get("/day", s.HandleDay)
	r.Get("/hours", s.HandleHours)
	r.Get("/feeds", s.HandleFeeds)
	r.Get("/feeds/{dataset_id}", s.HandleFeed)
	r.Get("/loads", s.HandleLoads)
	r.Get("/state", s.HandleState)
	r.Get("/stream", s.HandleStream)
	r.Post("/events", s.HandleEvents)
	r.Post("/rollup", s.HandleRollup)
}

// dataIngestAuth is the closed-by-default gate, mirroring the X-Admin-Key check
// used on the root-router admin endpoints (server_routes_mailing.go:83-84):
// an UNSET ADMIN_API_KEY authorizes nobody.
//
// A session-authenticated request is recognised by X-User-Email, which
// apiAuthMiddleware (routes.go:46-49) deletes from the inbound request and
// re-sets from the validated session — so it cannot be spoofed past that
// middleware — or by an org already resolved into the request context.
func dataIngestAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if adminKey := os.Getenv("ADMIN_API_KEY"); adminKey != "" && r.Header.Get("X-Admin-Key") == adminKey {
			next.ServeHTTP(w, r)
			return
		}
		if strings.TrimSpace(r.Header.Get("X-User-Email")) != "" {
			next.ServeHTTP(w, r)
			return
		}
		if org := GetOrgFromContext(r.Context()); org != nil {
			next.ServeHTTP(w, r)
			return
		}
		respondError(w, http.StatusUnauthorized, "unauthorized")
	})
}

// ── response envelope ───────────────────────────────────────────────────────

// diMeta is on EVERY response. Fields maps each payload VALUE KEY to its
// measurement status so the portal can render "not yet measured" instead of a
// zero it would otherwise present as fact.
type diMeta struct {
	AsOf   string            `json:"as_of"`
	Source string            `json:"source"`
	Fields map[string]string `json:"fields"`
	OrgID  string            `json:"organization_id"`
}

func (s *DataIngestService) meta(r *http.Request, source string) diMeta {
	return diMeta{
		AsOf:   s.now().UTC().Format(time.RFC3339),
		Source: source,
		Fields: map[string]string{},
		OrgID:  dataIngestOrgID(r),
	}
}

// mark records one field's measurement status.
func (m *diMeta) mark(field, status string) { m.Fields[field] = status }

// markAll records the same status for several fields (the not_measured sweep).
func (m *diMeta) markAll(status string, fields ...string) {
	for _, f := range fields {
		m.Fields[f] = status
	}
}

// i64 returns a pointer so an UNMEASURED number serializes as null, not 0.
func i64(v int64) *int64 { return &v }

// dataIngestOrgID resolves the caller's organization through the canonical
// 6-step chain (org_context.go:300). Its last step is the single-tenant
// fallback, so an error here means the chain itself failed — we still answer
// with the fallback rather than 500ing a read-only dashboard, exactly as the
// other mailing handlers do.
func dataIngestOrgID(r *http.Request) string {
	if id, err := GetOrgIDFromRequest(r); err == nil && id != uuid.Nil {
		return id.String()
	}
	return SingleTenantFallbackOrgID
}

// dataIngestRealBucket is the ONE repository for at-rest objects
// (brain #3589: the Mac is not a repository). A batch whose s3_bucket is this
// one has a real object behind it; anything else is a DB-only load.
func dataIngestRealBucket() string {
	if b := strings.TrimSpace(os.Getenv("PARTNER_INGEST_S3_BUCKET")); b != "" {
		return b
	}
	return defaultPartnerIngestS3Bucket
}

// legacySupplyClass derives the class of a batch that predates the
// supply_class column. 11,165,296 of the 11,165,349 unstamped batches on
// 2026-09-20 were partner API posts landed in the repository bucket — one
// class by construction — so they are read as dynamic/partner_api here rather
// than rewritten by an 11M-row UPDATE on a hot 7.75 GB table. Every other
// legacy load (42 rows) was stamped by agents/jobs/data_ingest_backfill.py
// --stamp-batches --non-api-only; anything still blank stays blank (honest).
func legacySupplyClass(supplyClass, sourcePath, bucket string) (string, string) {
	if supplyClass != "" {
		return supplyClass, sourcePath
	}
	if bucket != "" && bucket == dataIngestRealBucket() {
		if sourcePath == "" {
			sourcePath = "partner_api"
		}
		return "dynamic", sourcePath
	}
	return supplyClass, sourcePath
}

// denverDay resolves ?date= (YYYY-MM-DD) or defaults to today in Denver.
// Rejects a malformed date rather than silently answering for today.
func denverDay(r *http.Request) (string, error) {
	q := strings.TrimSpace(r.URL.Query().Get("date"))
	if q == "" {
		return dataingest.Today(), nil
	}
	t, err := time.ParseInLocation(dataingest.DayLayout, q, dataingest.Denver)
	if err != nil {
		return "", fmt.Errorf("date must be YYYY-MM-DD")
	}
	return t.Format(dataingest.DayLayout), nil
}

// dayBounds returns the [start, end) UTC instants of a Denver calendar day.
func dayBounds(day string) (time.Time, time.Time, error) {
	t, err := time.ParseInLocation(dataingest.DayLayout, day, dataingest.Denver)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return t.UTC(), t.AddDate(0, 0, 1).UTC(), nil
}

func previousDay(day string) string {
	t, err := time.ParseInLocation(dataingest.DayLayout, day, dataingest.Denver)
	if err != nil {
		return day
	}
	return t.AddDate(0, 0, -1).Format(dataingest.DayLayout)
}

// ispCount is the portal's composition row shape: an ARRAY of {isp, n}, not a
// map, so the order the API chose (descending n) survives JSON.
type ispCount struct {
	ISP    string `json:"isp"`
	N      int64  `json:"n"`
	Mailed *int64 `json:"mailed,omitempty"`
}

func sortISPCounts(in []ispCount) []ispCount {
	sort.Slice(in, func(i, j int) bool {
		if in[i].N != in[j].N {
			return in[i].N > in[j].N
		}
		return in[i].ISP < in[j].ISP
	})
	return in
}

// ── the ONE heavy query: partner_clean_queue by (dataset, status) ───────────

type queueStateSnapshot struct {
	GeneratedAt time.Time
	QueryMillis int64
	// ByDataset[datasetID][status] = n
	ByDataset map[string]map[string]int64
	ByStatus  map[string]int64
	Total     int64
	// Objects[orgID] = the two at-rest object tiles, computed in the same
	// background scan: the batch-side count is a full pass over 10.6M
	// partner_inbound_batches rows (30s statement timeout inline on
	// 2026-09-20), so it lives here and never on a request.
	Objects         map[string]objectCounts
	ObjectsMeasured bool
}

type objectCounts struct {
	StaticObjects      int64
	LoadsWithoutObject int64
}

type queueStateFlight struct {
	done chan struct{}
	snap *queueStateSnapshot
	err  error
}

// queueState returns the cached rollup or refreshes it under single-flight.
// Copied in shape from PartnerReservoirHandler.snapshot for the same reason:
// two overlapping full scans of a 15M-row table is exactly the load the portal
// must never generate.
func (s *DataIngestService) queueState(ctx context.Context, force bool) (*queueStateSnapshot, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	s.mu.Lock()
	if s.refresherOn {
		// Prod: the refresher owns the scan. Serve the last snapshot whatever
		// its age (the response carries cache_age_seconds); ?refresh=1 kicks
		// an EXTRA scan in the background and still returns immediately.
		c := s.cached
		s.mu.Unlock()
		if force {
			go s.refreshQueueState()
		}
		if c == nil {
			return nil, errQueueStateWarming
		}
		return c, nil
	}
	if !force && s.cached != nil && time.Since(s.cachedAt) < dataIngestCacheTTL {
		c := s.cached
		s.mu.Unlock()
		return c, nil
	}
	if f := s.inFlight; f != nil {
		s.mu.Unlock()
		select {
		case <-f.done:
			return f.snap, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	flight := &queueStateFlight{done: make(chan struct{})}
	s.inFlight = flight
	s.mu.Unlock()

	// Detached, like the reservoir handler: a cancelled HTTP request must not
	// leave every other waiter with a half-finished scan.
	qctx, cancel := context.WithTimeout(context.Background(), dataIngestQueryTimeout)
	defer cancel()
	snap, err := s.queryQueueState(qctx)

	s.mu.Lock()
	if err == nil {
		s.cached, s.cachedAt = snap, time.Now()
	}
	s.inFlight = nil
	s.mu.Unlock()

	flight.snap, flight.err = snap, err
	close(flight.done)
	return snap, err
}

// StartQueueStateRefresher moves the reservoir scan off the request path for
// the life of the process: one scan now, then one every dataIngestRefreshEvery,
// single-flighted with any ?refresh=1 kick. Called once from the route
// registration in prod (server_routes_mailing.go); never from tests, whose
// sqlmock expectations pin the inline path. ctx cancellation stops the ticker.
func (s *DataIngestService) StartQueueStateRefresher(ctx context.Context) {
	s.mu.Lock()
	if s.refresherOn {
		s.mu.Unlock()
		return
	}
	s.refresherOn = true
	s.mu.Unlock()
	go func() {
		s.refreshQueueState()
		t := time.NewTicker(dataIngestRefreshEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refreshQueueState()
			}
		}
	}()
}

// refreshQueueState runs ONE scan under the same single-flight as the inline
// path (a ticker firing while a ?refresh=1 kick is still scanning joins it
// instead of starting a second full scan). Errors are logged, never fatal:
// the previous snapshot stays served and its age tells the reader.
func (s *DataIngestService) refreshQueueState() {
	if s.db == nil {
		return
	}
	s.mu.Lock()
	if f := s.inFlight; f != nil {
		s.mu.Unlock()
		<-f.done
		return
	}
	flight := &queueStateFlight{done: make(chan struct{})}
	s.inFlight = flight
	s.mu.Unlock()

	qctx, cancel := context.WithTimeout(context.Background(), dataIngestQueryTimeout)
	defer cancel()
	snap, err := s.queryQueueState(qctx)

	s.mu.Lock()
	if err == nil {
		s.cached, s.cachedAt = snap, time.Now()
	}
	s.inFlight = nil
	s.mu.Unlock()
	if err != nil {
		log.Printf("[data-ingest] reservoir scan failed (previous snapshot stays served): %v", err)
	} else {
		log.Printf("[data-ingest] reservoir snapshot refreshed: %d rows in %dms", snap.Total, snap.QueryMillis)
	}
	flight.snap, flight.err = snap, err
	close(flight.done)
}

func (s *DataIngestService) queryQueueState(ctx context.Context) (*queueStateSnapshot, error) {
	started := time.Now()
	// The primary pool pins statement_timeout=30s in its DSN; this scan
	// measures ~9s against prod, so it runs inside a read-only tx with a
	// SET LOCAL raise — the reservoir handler's 2026-09-16 lesson.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only scan
	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '90s'`); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT dataset_id::text, COALESCE(status, '(none)') AS status, COUNT(*) AS n
		FROM partner_clean_queue
		GROUP BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	snap := &queueStateSnapshot{
		GeneratedAt: time.Now().UTC(),
		ByDataset:   map[string]map[string]int64{},
		ByStatus:    map[string]int64{},
	}
	for rows.Next() {
		var ds, status string
		var n int64
		if err := rows.Scan(&ds, &status, &n); err != nil {
			return nil, err
		}
		if snap.ByDataset[ds] == nil {
			snap.ByDataset[ds] = map[string]int64{}
		}
		snap.ByDataset[ds][status] += n
		snap.ByStatus[status] += n
		snap.Total += n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if objs, err := queryObjectAccounting(ctx, tx); err != nil {
		log.Printf("[data-ingest] object accounting failed (tiles not_measured this cycle): %v", err)
	} else {
		snap.Objects, snap.ObjectsMeasured = objs, true
	}
	snap.QueryMillis = time.Since(started).Milliseconds()
	return snap, nil
}

// stateCounts is the operator's vocabulary over partner_clean_queue statuses.
// raw = arrived but not yet mailable; staged = ready to mail; inflight =
// claimed by a wave; mailed = sent; removed = suppressed or dead-lettered;
// parked = held specifically (the "sitting in the DB" tile).
type stateCounts struct {
	Raw      int64
	Staged   int64
	InFlight int64
	Mailed   int64
	Removed  int64
	Engaged  int64
	Parked   int64
	Cleaned  int64
	Total    int64
}

func foldStates(byStatus map[string]int64) stateCounts {
	var c stateCounts
	for status, n := range byStatus {
		c.Total += n
		switch status {
		case "pending_eo":
			c.Raw += n
		case "held":
			c.Raw += n
			c.Parked += n
		case "ready":
			c.Staged += n
			c.Cleaned += n
		case "claimed":
			c.InFlight += n
			c.Cleaned += n
		case "mailed":
			c.Mailed += n
			c.Cleaned += n
		case "engaged":
			c.Mailed += n
			c.Engaged += n
			c.Cleaned += n
		case "dead_letter", "suppressed", "suppressed_eo", "suppressed_global":
			c.Removed += n
		}
	}
	return c
}

// ── GET /day ────────────────────────────────────────────────────────────────

type dayAtRest struct {
	Landed             *int64 `json:"landed"`
	Raw                *int64 `json:"raw"`
	Staged             *int64 `json:"staged"`
	InFlight           *int64 `json:"inflight"`
	Mailed             *int64 `json:"mailed"`
	Removed            *int64 `json:"removed"`
	StaticObjects      *int64 `json:"static_objects"`
	LoadsWithoutObject *int64 `json:"loads_without_object"`
	ParkedInDB         *int64 `json:"parked_in_db"`
}

type dayDynamic struct {
	Arrived     *int64   `json:"arrived"`
	Yesterday   *int64   `json:"yesterday"`
	ArrivalRate *float64 `json:"arrival_rate"`
	FeedsLive   *int64   `json:"feeds_live"`
}

type dayTransfer struct {
	N *int64 `json:"n"`
}

type daySeriesPoint struct {
	Day      string `json:"day"`
	AtRest   int64  `json:"at_rest"`
	Dynamic  int64  `json:"dynamic"`
	Transfer int64  `json:"transfer"`
}

type dayResponse struct {
	diMeta
	Date             string           `json:"date"`
	AtRest           dayAtRest        `json:"at_rest"`
	Dynamic          dayDynamic       `json:"dynamic"`
	InternalTransfer dayTransfer      `json:"internal_transfer"`
	Series           []daySeriesPoint `json:"series"`
	Composition      []ispCount       `json:"composition"`
}

// HandleDay: the three supply-class blocks for one Denver day, the 30-day
// series from the rollup, and the day's ISP composition.
func (s *DataIngestService) HandleDay(w http.ResponseWriter, r *http.Request) {
	day, err := denverDay(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	out := dayResponse{
		diMeta:      s.meta(r, sourceMixed),
		Date:        day,
		Series:      []daySeriesPoint{},
		Composition: []ispCount{},
	}

	// ── live counters (Redis) ──
	counts := map[string]map[string]map[string]int64{}
	if s.reader.Available() {
		if c, err := s.reader.GetDay(r.Context(), day); err == nil {
			counts = c
		}
	}
	measuredCounters := len(counts) > 0

	if measuredCounters {
		out.AtRest.Landed = i64(sumTransition(counts[dataingest.ClassAtRest], dataingest.TransitionLanded))
		out.mark("landed", fieldMeasured)

		arrived := sumTransition(counts[dataingest.ClassDynamic], dataingest.TransitionLanded) +
			sumTransition(counts[dataingest.ClassDynamic], dataingest.TransitionSiteEvent)
		out.Dynamic.Arrived = i64(arrived)
		out.mark("arrived", fieldMeasured)

		out.InternalTransfer.N = i64(sumTransition(counts[dataingest.ClassInternalTransfer], dataingest.TransitionTransfer))
		out.mark("n", fieldMeasured)

		// Arrival rate = arrived per elapsed Denver hour. On a PAST day the
		// divisor is the full 24h; on TODAY it is the hours so far, or the rate
		// would read low all morning.
		if hours := elapsedDenverHours(day, s.now()); hours > 0 {
			rate := float64(arrived) / hours
			out.Dynamic.ArrivalRate = &rate
			out.mark("arrival_rate", fieldDerived)
		} else {
			out.mark("arrival_rate", fieldNotMeasured)
		}

		if ids, err := s.reader.Datasets(r.Context(), day); err == nil {
			out.Dynamic.FeedsLive = i64(int64(len(ids)))
			out.mark("feeds_live", fieldMeasured)
		} else {
			out.mark("feeds_live", fieldNotMeasured)
		}

		byISP := map[string]int64{}
		for _, byTransition := range counts {
			for _, t := range []string{
				dataingest.TransitionLanded, dataingest.TransitionSiteEvent,
				dataingest.TransitionLoadRegistered, dataingest.TransitionTransfer,
			} {
				for isp, n := range byTransition[t] {
					byISP[isp] += n
				}
			}
		}
		for isp, n := range byISP {
			out.Composition = append(out.Composition, ispCount{ISP: isp, N: n})
		}
		out.Composition = sortISPCounts(out.Composition)
		out.mark("composition", fieldMeasured)
	} else {
		out.markAll(fieldNotMeasured,
			"landed", "arrived", "n", "arrival_rate", "feeds_live", "composition")
	}

	// Yesterday's dynamic arrivals.
	if measuredCounters {
		prev := previousDay(day)
		if pc, err := s.reader.GetDay(r.Context(), prev); err == nil && len(pc) > 0 {
			y := sumTransition(pc[dataingest.ClassDynamic], dataingest.TransitionLanded) +
				sumTransition(pc[dataingest.ClassDynamic], dataingest.TransitionSiteEvent)
			out.Dynamic.Yesterday = i64(y)
			out.mark("yesterday", fieldMeasured)
		} else {
			out.mark("yesterday", fieldNotMeasured)
		}
	} else {
		out.mark("yesterday", fieldNotMeasured)
	}

	// ── reservoir state (the ONE heavy query, refreshed in the background) ──
	snap, snapErr := s.queueState(r.Context(), r.URL.Query().Get("refresh") == "1")
	if snapErr == nil {
		f := foldStates(snap.ByStatus)
		out.AtRest.Raw = i64(f.Raw)
		out.AtRest.Staged = i64(f.Staged)
		out.AtRest.InFlight = i64(f.InFlight)
		out.AtRest.Mailed = i64(f.Mailed)
		out.AtRest.Removed = i64(f.Removed)
		out.AtRest.ParkedInDB = i64(f.Parked)
		out.markAll(fieldDerived, "raw", "staged", "inflight", "mailed", "removed", "parked_in_db")
	} else {
		out.markAll(fieldNotMeasured, "raw", "staged", "inflight", "mailed", "removed", "parked_in_db")
	}

	// ── object accounting (same snapshot, per org) ──
	if snapErr == nil && snap.ObjectsMeasured {
		oc := snap.Objects[dataIngestOrgID(r)]
		out.AtRest.StaticObjects = i64(oc.StaticObjects)
		out.AtRest.LoadsWithoutObject = i64(oc.LoadsWithoutObject)
		out.markAll(fieldDerived, "static_objects", "loads_without_object")
	} else {
		out.markAll(fieldNotMeasured, "static_objects", "loads_without_object")
	}

	// ── 30-day series (rollup) ──
	if series, err := s.daySeries(r.Context(), day); err == nil && len(series) > 0 {
		out.Series = series
		out.mark("series", fieldMeasured)
	} else {
		out.mark("series", fieldNotMeasured)
	}

	respondJSON(w, http.StatusOK, out)
}

// sumTransition totals one transition's per-ISP hash.
func sumTransition(byTransition map[string]map[string]int64, transition string) int64 {
	var n int64
	for _, v := range byTransition[transition] {
		n += v
	}
	return n
}

// elapsedDenverHours returns how much of `day` has elapsed, capped at 24 and
// floored at 1 so the first minute of a day cannot divide by ~0.
func elapsedDenverHours(day string, now time.Time) float64 {
	start, err := time.ParseInLocation(dataingest.DayLayout, day, dataingest.Denver)
	if err != nil {
		return 0
	}
	elapsed := now.Sub(start).Hours()
	if elapsed >= 24 {
		return 24
	}
	if elapsed < 1 {
		return 1
	}
	return elapsed
}

// queryObjectAccounting answers the two at-rest tiles for EVERY org in one
// pass, inside the refresher's transaction (90s budget), never per request:
//
//	static_objects       — objects we actually hold: data_ingest_static_objects
//	                       past the upload stage, plus AT-REST batches whose
//	                       s3_bucket IS the real repository bucket (a partner
//	                       API post also lands in that bucket but is dynamic
//	                       supply, not a static object — 2026-09-20 review:
//	                       counting every bucket batch rendered 11,165,307).
//	loads_without_object  — the RED tile: batches that exist only as DB rows
//	                       (bucket is not the repository AND no object_sha256).
//	                       brain #3589: the Mac is not a repository.
func queryObjectAccounting(ctx context.Context, tx *sql.Tx) (map[string]objectCounts, error) {
	bucket := dataIngestRealBucket()
	out := map[string]objectCounts{}

	srows, err := tx.QueryContext(ctx, `
		SELECT organization_id::text, COUNT(*)
		FROM data_ingest_static_objects
		WHERE status IN ('object','registered','loaded')
		GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	for srows.Next() {
		var org string
		var n int64
		if err := srows.Scan(&org, &n); err != nil {
			srows.Close()
			return nil, err
		}
		c := out[org]
		c.StaticObjects += n
		out[org] = c
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return nil, err
	}

	brows, err := tx.QueryContext(ctx, `
		SELECT p.organization_id::text,
		       COUNT(*) FILTER (WHERE b.s3_bucket = $1 AND b.supply_class = 'at_rest'),
		       COUNT(*) FILTER (WHERE b.s3_bucket IS DISTINCT FROM $1
		                          AND (b.object_sha256 IS NULL OR b.object_sha256 = ''))
		FROM partner_inbound_batches b
		JOIN partner_datasets d ON d.id = b.dataset_id
		JOIN data_partners p ON p.id = d.partner_id
		GROUP BY 1`, bucket)
	if err != nil {
		return nil, err
	}
	defer brows.Close()
	for brows.Next() {
		var org string
		var withObject, withoutObject int64
		if err := brows.Scan(&org, &withObject, &withoutObject); err != nil {
			return nil, err
		}
		c := out[org]
		c.StaticObjects += withObject
		c.LoadsWithoutObject += withoutObject
		out[org] = c
	}
	return out, brows.Err()
}

// daySeries reads the last 30 days of ARRIVAL volume per class from
// data_ingest_rollup, preferring reconcile > counters > backfill per
// (day, class): three sources can coexist for one day and the settled one wins.
func (s *DataIngestService) daySeries(ctx context.Context, day string) ([]daySeriesPoint, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	end, err := time.ParseInLocation(dataingest.DayLayout, day, dataingest.Denver)
	if err != nil {
		return nil, err
	}
	start := end.AddDate(0, 0, -(dataIngestSeriesDays - 1))

	const q = `
		WITH ranked AS (
			SELECT day, supply_class, source, SUM(n) AS n,
			       ROW_NUMBER() OVER (
			           PARTITION BY day, supply_class
			           ORDER BY CASE source
			                      WHEN 'reconcile' THEN 1
			                      WHEN 'counters'  THEN 2
			                      ELSE 3 END
			       ) AS rk
			FROM data_ingest_rollup
			WHERE day BETWEEN $1 AND $2
			  AND transition = ANY($3)
			GROUP BY day, supply_class, source
		)
		SELECT day, supply_class, n FROM ranked WHERE rk = 1 ORDER BY day`

	// The series answers "what ARRIVED", so it sums the arrival transitions
	// only. Counting verdicts/claims/mails here would multiply one record by
	// the number of states it has passed through.
	arrivals := pq.Array([]string{
		dataingest.TransitionLanded,
		dataingest.TransitionSiteEvent,
		dataingest.TransitionLoadRegistered,
		dataingest.TransitionTransfer,
	})

	qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(qctx, q,
		start.Format(dataingest.DayLayout), end.Format(dataingest.DayLayout), arrivals)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSeries(rows)
}

// scanSeries folds (day, supply_class, n) rows into the portal's series shape,
// where internal_transfer is rendered as "transfer".
func scanSeries(rows *sql.Rows) ([]daySeriesPoint, error) {
	byDay := map[string]*daySeriesPoint{}
	for rows.Next() {
		var d time.Time
		var cls string
		var n int64
		if err := rows.Scan(&d, &cls, &n); err != nil {
			return nil, err
		}
		key := d.Format(dataingest.DayLayout)
		p := byDay[key]
		if p == nil {
			p = &daySeriesPoint{Day: key}
			byDay[key] = p
		}
		switch cls {
		case dataingest.ClassAtRest:
			p.AtRest += n
		case dataingest.ClassDynamic:
			p.Dynamic += n
		case dataingest.ClassInternalTransfer:
			p.Transfer += n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]daySeriesPoint, 0, len(byDay))
	for _, p := range byDay {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out, nil
}

// ── GET /hours ──────────────────────────────────────────────────────────────

type hourPoint struct {
	Hour int   `json:"hour"`
	N    int64 `json:"n"`
}

type hoursResponse struct {
	diMeta
	Date  string      `json:"date"`
	Class string      `json:"class"`
	Hours []hourPoint `json:"hours"`
}

// HandleHours: 24 Denver hour buckets. Defaults to the dynamic class (the one
// that actually varies by hour) unless ?class= says otherwise.
func (s *DataIngestService) HandleHours(w http.ResponseWriter, r *http.Request) {
	day, err := denverDay(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	class := strings.TrimSpace(r.URL.Query().Get("class"))
	if class == "" {
		class = dataingest.ClassDynamic
	}
	if !dataingest.ValidSupplyClass(class) {
		respondError(w, http.StatusBadRequest, "invalid class (want at_rest|dynamic|internal_transfer)")
		return
	}
	out := hoursResponse{diMeta: s.meta(r, sourceCounters), Date: day, Class: class, Hours: []hourPoint{}}

	measured := false
	if s.reader.Available() {
		if ok, err := s.reader.Measured(r.Context(), day); err == nil && ok {
			if hrs, err := s.reader.GetHours(r.Context(), day, class); err == nil {
				for _, h := range hrs {
					out.Hours = append(out.Hours, hourPoint{Hour: h.Hour, N: h.N})
				}
				measured = true
			}
		}
	}
	if measured {
		out.mark("hours", fieldMeasured)
	} else {
		out.mark("hours", fieldNotMeasured)
	}
	respondJSON(w, http.StatusOK, out)
}

// ── GET /feeds ──────────────────────────────────────────────────────────────

type feedStatus struct {
	IngestOpen bool `json:"ingest_open"`
	SendRow    bool `json:"send_row"`
	Express    bool `json:"express"`
	Contract   bool `json:"contract"`
}

type feedRow struct {
	DatasetID     string     `json:"dataset_id"`
	PartnerID     string     `json:"partner_id"`
	Name          string     `json:"name"`
	Partner       string     `json:"partner"`
	Lane          string     `json:"lane"`
	SupplyClass   string     `json:"supply_class"`
	SourceChannel string     `json:"source_channel"`
	Status        feedStatus `json:"status"`

	Today     *int64 `json:"today"`
	Yesterday *int64 `json:"yesterday"`
	Raw       *int64 `json:"raw"`
	Staged    *int64 `json:"staged"`
	InFlight  *int64 `json:"inflight"`
	Mailed    *int64 `json:"mailed"`
	Records   *int64 `json:"records"`
	Consumed  *int64 `json:"consumed"`
	Remaining *int64 `json:"remaining"`

	LastEvent  string `json:"last_event"`
	LastLoaded string `json:"last_loaded"`
}

type feedsResponse struct {
	diMeta
	Date        string    `json:"date"`
	Feeds       []feedRow `json:"feeds"`
	CacheAgeSec int       `json:"cache_age_seconds"`
	QueryMillis int64     `json:"query_ms"`
	Note        string    `json:"note,omitempty"`
}

// HandleFeeds: one row per (partner, dataset) — wiring status from the tables,
// live volume from Redis, reservoir counts from the ONE cached grouped query.
//
// consumed / remaining are ALWAYS not_measured today: "how much of this feed
// have we used up" needs the per-feed award ledger, and inventing it from the
// reservoir would produce a confident wrong number on the operator's screen.
func (s *DataIngestService) HandleFeeds(w http.ResponseWriter, r *http.Request) {
	day, err := denverDay(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	out := feedsResponse{diMeta: s.meta(r, sourceMixed), Date: day, Feeds: []feedRow{}}

	feeds, err := s.queryFeeds(r.Context(), dataIngestOrgID(r), true)
	feedsDegraded := false
	if err != nil {
		// The wiring columns come from four small tables; only the LATERAL
		// last-batch lookup on partner_inbound_batches (10.6M rows) can time
		// out — measured 2026-09-20 before idx_pib_dataset_received existed.
		// Serve the wiring without it rather than 500 the whole feed list.
		log.Printf("[data-ingest] feeds with last-batch lookup failed (%v) — serving wiring only", err)
		feeds, err = s.queryFeeds(r.Context(), dataIngestOrgID(r), false)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "feeds_query_failed: "+err.Error())
			return
		}
		feedsDegraded = true
	}
	out.Feeds = feeds
	out.mark("feeds", fieldMeasured)
	out.markAll(fieldNotMeasured, "consumed", "remaining")
	if feedsDegraded {
		out.Note = "last_loaded/supply_class/source_channel not measured: the per-dataset last-batch lookup timed out (index idx_pib_dataset_received pending — built by the concurrent index builder after boot)"
		out.markAll(fieldNotMeasured, "last_loaded", "supply_class", "source_channel")
	} else {
		out.markAll(fieldMeasured, "last_loaded", "supply_class", "source_channel")
	}

	if snap, err := s.queueState(r.Context(), r.URL.Query().Get("refresh") == "1"); err == nil {
		for i := range out.Feeds {
			f := foldStates(snap.ByDataset[out.Feeds[i].DatasetID])
			out.Feeds[i].Raw = i64(f.Raw)
			out.Feeds[i].Staged = i64(f.Staged)
			out.Feeds[i].InFlight = i64(f.InFlight)
			out.Feeds[i].Mailed = i64(f.Mailed)
			out.Feeds[i].Records = i64(f.Total)
		}
		out.CacheAgeSec = int(time.Since(snap.GeneratedAt).Seconds())
		out.QueryMillis = snap.QueryMillis
		out.markAll(fieldDerived, "raw", "staged", "inflight", "mailed")
		out.mark("records", fieldMeasured)
	} else {
		out.markAll(fieldNotMeasured, "raw", "staged", "inflight", "mailed", "records")
	}

	if !s.reader.Available() {
		out.markAll(fieldNotMeasured, "today", "yesterday", "last_event")
	} else {
		ids := make([]string, 0, len(out.Feeds))
		for _, f := range out.Feeds {
			ids = append(ids, f.DatasetID)
		}
		todayCounts := s.datasetArrivals(r.Context(), day, ids)
		yestCounts := s.datasetArrivals(r.Context(), previousDay(day), ids)
		last, _ := s.reader.LastEvent(r.Context(), ids)
		for i := range out.Feeds {
			id := out.Feeds[i].DatasetID
			if v, ok := todayCounts[id]; ok {
				out.Feeds[i].Today = i64(v)
			}
			if v, ok := yestCounts[id]; ok {
				out.Feeds[i].Yesterday = i64(v)
			}
			out.Feeds[i].LastEvent = last[id]
		}
		out.markAll(fieldMeasured, "today", "yesterday", "last_event")
	}
	respondJSON(w, http.StatusOK, out)
}

// datasetArrivals returns dataset -> landed count for one day. Only ids that
// HAVE a counter appear; a missing id means not measured, not zero.
func (s *DataIngestService) datasetArrivals(ctx context.Context, day string, ids []string) map[string]int64 {
	out := map[string]int64{}
	if !s.reader.Available() {
		return out
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		counts, err := s.reader.GetDataset(ctx, day, id)
		if err != nil || len(counts) == 0 {
			continue
		}
		var n int64
		for _, v := range counts[dataingest.TransitionLanded] {
			n += v
		}
		out[id] = n
	}
	return out
}

func (s *DataIngestService) queryFeeds(ctx context.Context, orgID string, withLastBatch bool) ([]feedRow, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	// status.contract = the lane has an ACTIVE REQ-118 dispatch contract. A
	// feed with data, an open door and no contract mails nothing — that is the
	// state this column exists to make visible.
	const q = `
		SELECT d.id::text, d.name, d.slug, d.vertical, d.status,
		       d.paused_emergency, COALESCE(d.express_dispatch, FALSE),
		       p.id::text, p.name, p.status,
		       (ds.vertical IS NOT NULL) AS has_drip_state,
		       (dc.lane IS NOT NULL) AS has_contract,
		       COALESCE(b.supply_class, ''), COALESCE(b.source_path, ''), b.received_at,
		       COALESCE(b.s3_bucket, '')
		FROM partner_datasets d
		JOIN data_partners p ON p.id = d.partner_id
		LEFT JOIN partner_drip_state ds ON ds.vertical = d.vertical
		LEFT JOIN LATERAL (
			SELECT lane FROM drip_dispatch_contracts
			WHERE lane = d.vertical AND status = 'active'
			LIMIT 1
		) dc ON TRUE
		LEFT JOIN LATERAL (
			SELECT supply_class, source_path, received_at, s3_bucket
			FROM partner_inbound_batches
			WHERE dataset_id = d.id
			ORDER BY received_at DESC
			LIMIT 1
		) b ON TRUE
		WHERE p.organization_id = $1
		ORDER BY p.name, d.name`
	// qWiringOnly is q without the last-batch LATERAL: the same columns, the
	// three batch-derived ones NULL, so the scan below is shared.
	const qWiringOnly = `
		SELECT d.id::text, d.name, d.slug, d.vertical, d.status,
		       d.paused_emergency, COALESCE(d.express_dispatch, FALSE),
		       p.id::text, p.name, p.status,
		       (ds.vertical IS NOT NULL) AS has_drip_state,
		       (dc.lane IS NOT NULL) AS has_contract,
		       ''::text, ''::text, NULL::timestamptz, ''::text
		FROM partner_datasets d
		JOIN data_partners p ON p.id = d.partner_id
		LEFT JOIN partner_drip_state ds ON ds.vertical = d.vertical
		LEFT JOIN LATERAL (
			SELECT lane FROM drip_dispatch_contracts
			WHERE lane = d.vertical AND status = 'active'
			LIMIT 1
		) dc ON TRUE
		WHERE p.organization_id = $1
		ORDER BY p.name, d.name`

	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	sqlText := q
	if !withLastBatch {
		sqlText = qWiringOnly
	}
	rows, err := s.db.QueryContext(qctx, sqlText, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []feedRow{}
	for rows.Next() {
		var f feedRow
		var slug, datasetStatus, partnerStatus string
		var pausedEmergency bool
		var lastLoaded sql.NullTime
		var lastBucket string
		if err := rows.Scan(
			&f.DatasetID, &f.Name, &slug, &f.Lane, &datasetStatus,
			&pausedEmergency, &f.Status.Express,
			&f.PartnerID, &f.Partner, &partnerStatus,
			&f.Status.SendRow, &f.Status.Contract,
			&f.SupplyClass, &f.SourceChannel, &lastLoaded, &lastBucket,
		); err != nil {
			return nil, err
		}
		f.SupplyClass, f.SourceChannel = legacySupplyClass(f.SupplyClass, f.SourceChannel, lastBucket)
		if lastLoaded.Valid {
			f.LastLoaded = lastLoaded.Time.UTC().Format(time.RFC3339)
		}
		// ingest_open = the door accepts records: the dataset is active, not
		// emergency-paused, and its partner is active. Three separate switches,
		// each of which has silently closed a feed before.
		f.Status.IngestOpen = datasetStatus == "active" && !pausedEmergency && partnerStatus == "active"
		out = append(out, f)
	}
	return out, rows.Err()
}

// ── GET /feeds/{dataset_id} ─────────────────────────────────────────────────

type feedLoad struct {
	BatchID    string `json:"batch_id"`
	ReceivedAt string `json:"received_at"`
	Name       string `json:"name"`
	Ref        string `json:"ref"`
	Records    int64  `json:"records"`
	Raw        int64  `json:"raw"`
	Staged     int64  `json:"staged"`
	Mailed     int64  `json:"mailed"`
}

type feedFunnel struct {
	Landed  *int64 `json:"landed"`
	Raw     *int64 `json:"raw"`
	Cleaned *int64 `json:"cleaned"`
	Staged  *int64 `json:"staged"`
	Mailed  *int64 `json:"mailed"`
	Engaged *int64 `json:"engaged"`
}

type feedComposition struct {
	Raw    []ispCount `json:"raw"`
	Staged []ispCount `json:"staged"`
}

type feedDetailResponse struct {
	diMeta
	Date      string `json:"date"`
	DatasetID string `json:"dataset_id"`
	// Header: the same wiring row /feeds lists for this dataset, so the feed
	// page needs no second call (status = the four switches, never a summary).
	Name          string           `json:"name"`
	Partner       string           `json:"partner"`
	PartnerID     string           `json:"partner_id"`
	Lane          string           `json:"lane"`
	SupplyClass   string           `json:"supply_class"`
	SourceChannel string           `json:"source_channel"`
	Status        feedStatus       `json:"status"`
	LastLoaded    string           `json:"last_loaded"`
	Note          string           `json:"note,omitempty"`
	Funnel        feedFunnel       `json:"funnel"`
	Series        []daySeriesPoint `json:"series"`
	Composition   feedComposition  `json:"composition"`
	Loads         []feedLoad       `json:"loads"`
}

// HandleFeed is the per-feed page: funnel + composition from the tables
// (dataset-scoped, index-covered), 30-day series from the rollup, and the day's
// loads with per-batch state counts.
func (s *DataIngestService) HandleFeed(w http.ResponseWriter, r *http.Request) {
	datasetID := strings.TrimSpace(chi.URLParam(r, "dataset_id"))
	if datasetID == "" {
		respondError(w, http.StatusBadRequest, "dataset_id is required")
		return
	}
	if _, err := uuid.Parse(datasetID); err != nil {
		respondError(w, http.StatusBadRequest, "dataset_id must be a uuid")
		return
	}
	day, err := denverDay(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	out := feedDetailResponse{
		diMeta:    s.meta(r, sourceMixed),
		Date:      day,
		DatasetID: datasetID,
		Series:    []daySeriesPoint{},
		Composition: feedComposition{
			Raw:    []ispCount{},
			Staged: []ispCount{},
		},
		Loads: []feedLoad{},
	}

	// Funnel + composition: ONE dataset-scoped grouped query on
	// idx_pcq_isp_family (dataset_id, isp_family, status).
	// ── header (wiring row) ──
	if feeds, ferr := s.queryFeeds(r.Context(), dataIngestOrgID(r), true); ferr == nil {
		found := false
		for _, f := range feeds {
			if f.DatasetID == datasetID {
				out.Name, out.Partner, out.PartnerID, out.Lane = f.Name, f.Partner, f.PartnerID, f.Lane
				out.SupplyClass, out.SourceChannel, out.Status, out.LastLoaded = f.SupplyClass, f.SourceChannel, f.Status, f.LastLoaded
				found = true
				break
			}
		}
		if !found {
			// Org isolation: the composition/series/loads below are keyed by
			// dataset_id alone, so a dataset outside this org must stop here.
			respondError(w, http.StatusNotFound, "dataset not found in this organization")
			return
		}
		out.markAll(fieldMeasured, "name", "status", "supply_class", "source_channel", "last_loaded")
	} else {
		out.Note = "feed header not measured: " + ferr.Error()
		out.markAll(fieldNotMeasured, "name", "status", "supply_class", "source_channel", "last_loaded")
	}

	byStatus, byStatusISP, err := s.queryFeedComposition(r.Context(), datasetID)
	if err != nil {
		out.markAll(fieldNotMeasured, "raw", "cleaned", "staged", "mailed", "engaged",
			"composition_raw", "composition_staged")
	} else {
		f := foldStates(byStatus)
		out.Funnel.Raw = i64(f.Raw)
		out.Funnel.Cleaned = i64(f.Cleaned)
		out.Funnel.Staged = i64(f.Staged)
		out.Funnel.Mailed = i64(f.Mailed)
		out.Funnel.Engaged = i64(f.Engaged)
		out.markAll(fieldDerived, "raw", "cleaned", "staged", "mailed", "engaged")

		rawISP := map[string]int64{}
		for _, status := range []string{"pending_eo", "held"} {
			for isp, n := range byStatusISP[status] {
				rawISP[isp] += n
			}
		}
		for isp, n := range rawISP {
			out.Composition.Raw = append(out.Composition.Raw, ispCount{ISP: isp, N: n})
		}
		for isp, n := range byStatusISP["ready"] {
			out.Composition.Staged = append(out.Composition.Staged, ispCount{ISP: isp, N: n})
		}
		out.Composition.Raw = sortISPCounts(out.Composition.Raw)
		out.Composition.Staged = sortISPCounts(out.Composition.Staged)
		out.markAll(fieldMeasured, "composition_raw", "composition_staged")
	}

	// landed is a COUNTER fact (how many arrived today), not a table fact.
	if s.reader.Available() {
		if counts, err := s.reader.GetDataset(r.Context(), day, datasetID); err == nil && len(counts) > 0 {
			var n int64
			for _, v := range counts[dataingest.TransitionLanded] {
				n += v
			}
			out.Funnel.Landed = i64(n)
			out.mark("landed", fieldMeasured)
		} else {
			out.mark("landed", fieldNotMeasured)
		}
	} else {
		out.mark("landed", fieldNotMeasured)
	}

	if series, err := s.feedSeries(r.Context(), datasetID, day); err == nil && len(series) > 0 {
		out.Series = series
		out.mark("series", fieldMeasured)
	} else {
		out.mark("series", fieldNotMeasured)
	}

	loads, err := s.queryLoads(r.Context(), dataIngestOrgID(r), day, datasetID)
	if err != nil {
		out.mark("loads", fieldNotMeasured)
	} else {
		for _, l := range loads {
			out.Loads = append(out.Loads, feedLoad{
				BatchID: l.BatchID, ReceivedAt: l.ReceivedAt, Name: l.Dataset,
				Ref: l.ref(), Records: l.Records,
				Raw: l.Raw, Staged: l.Staged, Mailed: l.Mailed,
			})
		}
		out.mark("loads", fieldMeasured)
	}
	respondJSON(w, http.StatusOK, out)
}

func (s *DataIngestService) queryFeedComposition(ctx context.Context, datasetID string) (map[string]int64, map[string]map[string]int64, error) {
	if s.db == nil {
		return nil, nil, sql.ErrConnDone
	}
	qctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(qctx, `
		SELECT COALESCE(status, '(none)'), COALESCE(isp_family, '(none)'), COUNT(*)
		FROM partner_clean_queue
		WHERE dataset_id = $1
		GROUP BY 1, 2`, datasetID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	byStatus := map[string]int64{}
	byStatusISP := map[string]map[string]int64{}
	for rows.Next() {
		var status, isp string
		var n int64
		if err := rows.Scan(&status, &isp, &n); err != nil {
			return nil, nil, err
		}
		byStatus[status] += n
		if byStatusISP[status] == nil {
			byStatusISP[status] = map[string]int64{}
		}
		byStatusISP[status][isp] += n
	}
	return byStatus, byStatusISP, rows.Err()
}

func (s *DataIngestService) feedSeries(ctx context.Context, datasetID, day string) ([]daySeriesPoint, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	end, err := time.ParseInLocation(dataingest.DayLayout, day, dataingest.Denver)
	if err != nil {
		return nil, err
	}
	start := end.AddDate(0, 0, -(dataIngestSeriesDays - 1))
	qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(qctx, `
		SELECT day, supply_class, SUM(n)
		FROM data_ingest_rollup
		WHERE dataset_id = $1 AND day BETWEEN $2 AND $3 AND transition = $4
		GROUP BY day, supply_class
		ORDER BY day`,
		datasetID, start.Format(dataingest.DayLayout), end.Format(dataingest.DayLayout),
		dataingest.TransitionLanded)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSeries(rows)
}

// ── GET /loads ──────────────────────────────────────────────────────────────

type loadRow struct {
	BatchID     string `json:"batch_id"`
	Dataset     string `json:"dataset"`
	DatasetID   string `json:"dataset_id"`
	SourcePath  string `json:"source_path"`
	SupplyClass string `json:"supply_class"`
	S3Bucket    string `json:"s3_bucket"`
	S3Key       string `json:"s3_key"`
	Object      bool   `json:"object"`
	Records     int64  `json:"records"`
	Mailed      int64  `json:"mailed"`
	Staged      int64  `json:"staged"`
	Raw         int64  `json:"raw"`
	Removed     int64  `json:"removed"`
	ReceivedAt  string `json:"received_at"`
	// declared is the batch's own record_count; duplicates are the difference
	// between what a load DECLARED and what actually landed as rows.
	declared int64
}

// ref is the operator-readable pointer to where a load came from.
func (l loadRow) ref() string {
	if l.S3Key != "" {
		return l.S3Bucket + "/" + l.S3Key
	}
	if l.SourcePath != "" {
		return l.SourcePath
	}
	return l.BatchID
}

type loadTotals struct {
	Landed     *int64 `json:"landed"`
	Mailed     *int64 `json:"mailed"`
	NotMailed  *int64 `json:"not_mailed"`
	Removed    *int64 `json:"removed"`
	Duplicates *int64 `json:"duplicates"`
}

type loadSource struct {
	Name string `json:"name"`
	N    int64  `json:"n"`
}

type loadsResponse struct {
	diMeta
	Date        string       `json:"date"`
	Totals      loadTotals   `json:"totals"`
	Loads       []loadRow    `json:"loads"`
	Composition []ispCount   `json:"composition"`
	Sources     []loadSource `json:"sources"`
	Note        string       `json:"note,omitempty"`
}

// HandleLoads lists the batches RECEIVED on one Denver day with their per-batch
// state counts, the day's ISP composition, and where the loads came from.
// Every per-batch count rides idx_pcq_batch bounded by that day's batch ids —
// never a full scan.
func (s *DataIngestService) HandleLoads(w http.ResponseWriter, r *http.Request) {
	day, err := denverDay(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	out := loadsResponse{
		diMeta:      s.meta(r, sourceTable),
		Date:        day,
		Loads:       []loadRow{},
		Composition: []ispCount{},
		Sources:     []loadSource{},
	}

	loads, err := s.queryLoads(r.Context(), dataIngestOrgID(r), day, "")
	if err != nil {
		// The day range on partner_inbound_batches.received_at has no usable
		// index until idx_pib_dataset_received lands (concurrent builder);
		// measured 2026-09-20 as a 30s statement timeout. An empty list marked
		// not_measured is honest; a 500 hides the rest of the page.
		log.Printf("[data-ingest] loads query failed for %s: %v", day, err)
		out.Note = "loads_query_failed: " + err.Error()
		out.markAll(fieldNotMeasured, "loads", "landed", "mailed", "not_mailed", "removed", "duplicates", "sources", "composition")
		respondJSON(w, http.StatusOK, out)
		return
	}
	out.Loads = loads
	out.mark("loads", fieldMeasured)

	var landed, mailed, removed, declared int64
	bySource := map[string]int64{}
	ids := make([]string, 0, len(loads))
	for _, l := range loads {
		landed += l.Records
		mailed += l.Mailed
		removed += l.Removed
		declared += l.declared
		name := l.SourcePath
		if name == "" {
			name = "unknown"
		}
		bySource[name] += l.Records
		ids = append(ids, l.BatchID)
	}
	out.Totals.Landed = i64(landed)
	out.Totals.Mailed = i64(mailed)
	out.Totals.Removed = i64(removed)
	out.Totals.NotMailed = i64(landed - mailed - removed)
	// duplicates = declared minus landed: what a load said it carried, less the
	// rows that actually persisted (the dedup drop). Negative is impossible in
	// principle and is clamped to 0 rather than shown as a negative tile.
	dupes := declared - landed
	if dupes < 0 {
		dupes = 0
	}
	out.Totals.Duplicates = i64(dupes)
	out.markAll(fieldDerived, "landed", "mailed", "not_mailed", "removed", "duplicates")

	for name, n := range bySource {
		out.Sources = append(out.Sources, loadSource{Name: name, N: n})
	}
	sort.Slice(out.Sources, func(i, j int) bool {
		if out.Sources[i].N != out.Sources[j].N {
			return out.Sources[i].N > out.Sources[j].N
		}
		return out.Sources[i].Name < out.Sources[j].Name
	})
	out.mark("sources", fieldDerived)

	if comp, err := s.loadComposition(r.Context(), ids); err != nil {
		out.mark("composition", fieldNotMeasured)
	} else {
		out.Composition = comp
		out.mark("composition", fieldMeasured)
	}
	respondJSON(w, http.StatusOK, out)
}

// loadComposition returns per-ISP totals (and how many of them mailed) for the
// day's batch ids. Empty ids is a legitimate empty answer, not an error.
func (s *DataIngestService) loadComposition(ctx context.Context, batchIDs []string) ([]ispCount, error) {
	if len(batchIDs) == 0 {
		return []ispCount{}, nil
	}
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	qctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(qctx, `
		SELECT COALESCE(isp_family, '(none)'),
		       COUNT(*),
		       COUNT(*) FILTER (WHERE status IN ('mailed','engaged'))
		FROM partner_clean_queue
		WHERE batch_id = ANY($1)
		GROUP BY 1`, pq.Array(batchIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ispCount{}
	for rows.Next() {
		var c ispCount
		var m int64
		if err := rows.Scan(&c.ISP, &c.N, &m); err != nil {
			return nil, err
		}
		c.Mailed = i64(m)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return sortISPCounts(out), nil
}

// queryLoads returns the day's batches (optionally one dataset's) plus their
// per-batch state counts. datasetID == "" means every dataset in the org.
func (s *DataIngestService) queryLoads(ctx context.Context, orgID, day, datasetID string) ([]loadRow, error) {
	if s.db == nil {
		return nil, sql.ErrConnDone
	}
	start, end, err := dayBounds(day)
	if err != nil {
		return nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	q := `
		SELECT b.id::text, b.dataset_id::text, d.name,
		       COALESCE(b.supply_class, ''), COALESCE(b.source_path, ''),
		       COALESCE(b.s3_bucket, ''), COALESCE(b.s3_key, ''),
		       (b.object_sha256 IS NOT NULL AND b.object_sha256 <> '') AS has_object,
		       COALESCE(b.record_count, 0), b.received_at
		FROM partner_inbound_batches b
		JOIN partner_datasets d ON d.id = b.dataset_id
		JOIN data_partners p ON p.id = d.partner_id
		WHERE p.organization_id = $1
		  AND b.received_at >= $2 AND b.received_at < $3`
	args := []any{orgID, start, end}
	if datasetID != "" {
		q += ` AND b.dataset_id = $4`
		args = append(args, datasetID)
	}
	q += fmt.Sprintf(` ORDER BY b.received_at DESC LIMIT %d`, dataIngestLoadsLimit)

	rows, err := s.db.QueryContext(qctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []loadRow{}
	ids := []string{}
	for rows.Next() {
		var l loadRow
		var received time.Time
		if err := rows.Scan(&l.BatchID, &l.DatasetID, &l.Dataset, &l.SupplyClass,
			&l.SourcePath, &l.S3Bucket, &l.S3Key, &l.Object, &l.declared, &received); err != nil {
			return nil, err
		}
		l.ReceivedAt = received.UTC().Format(time.RFC3339)
		l.SupplyClass, l.SourcePath = legacySupplyClass(l.SupplyClass, l.SourcePath, l.S3Bucket)
		// A batch whose bucket IS the repository has a real object even when
		// object_sha256 predates the column.
		if !l.Object && l.S3Bucket != "" && l.S3Bucket == dataIngestRealBucket() {
			l.Object = true
		}
		out = append(out, l)
		ids = append(ids, l.BatchID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return out, nil
	}

	cRows, err := s.db.QueryContext(qctx, `
		SELECT batch_id::text, COALESCE(status, '(none)'), COUNT(*)
		FROM partner_clean_queue
		WHERE batch_id = ANY($1)
		GROUP BY 1, 2`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer cRows.Close()

	byBatch := map[string]map[string]int64{}
	for cRows.Next() {
		var b, status string
		var n int64
		if err := cRows.Scan(&b, &status, &n); err != nil {
			return nil, err
		}
		if byBatch[b] == nil {
			byBatch[b] = map[string]int64{}
		}
		byBatch[b][status] += n
	}
	if err := cRows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		f := foldStates(byBatch[out[i].BatchID])
		out[i].Raw = f.Raw
		out[i].Staged = f.Staged
		out[i].Mailed = f.Mailed
		out[i].Removed = f.Removed
		out[i].Records = f.Total
	}
	return out, nil
}

// ── GET /state ──────────────────────────────────────────────────────────────

type stateCounters struct {
	Running       bool   `json:"running"`
	LastHandledAt string `json:"last_handled_at"`
	Applied       uint64 `json:"applied"`
	Duplicates    uint64 `json:"duplicates"`
	LagMax        int64  `json:"lag_max"`
	// RedisAvailable / MeasuredToday explain WHY a tile is blank — a consumer
	// can be running while Redis is gone, and vice versa.
	RedisAvailable bool `json:"redis_available"`
	MeasuredToday  bool `json:"measured_today"`
	StreamClients  int  `json:"stream_clients"`
}

type stateTiles struct {
	StaticObjects      *int64 `json:"static_objects"`
	LoadsWithoutObject *int64 `json:"loads_without_object"`
	ParkedInDB         *int64 `json:"parked_in_db"`
	Staged             *int64 `json:"staged"`
	InFlight           *int64 `json:"inflight"`
	Mailed             *int64 `json:"mailed"`
	Removed            *int64 `json:"removed"`
}

type stateResponse struct {
	diMeta
	Tiles       stateTiles       `json:"tiles"`
	ByStatus    map[string]int64 `json:"by_status"`
	Counters    stateCounters    `json:"counters"`
	CacheAgeSec int              `json:"cache_age_seconds"`
	QueryMillis int64            `json:"query_ms"`
}

// HandleState: the status tiles (reservoir-style totals from the tables, 60s
// cached) plus the counters consumer's liveness, read from the SAME /health
// snapshot cmd/server publishes — one source, so the tab and /health can never
// disagree about whether counting is happening.
func (s *DataIngestService) HandleState(w http.ResponseWriter, r *http.Request) {
	day := dataingest.Today()
	out := stateResponse{
		diMeta:   s.meta(r, sourceTable),
		ByStatus: map[string]int64{},
	}

	di := CurrentEventBusStatus().Consumers.DataIngest
	out.Counters = stateCounters{
		Running:        di.Running,
		LastHandledAt:  di.LastHandledAt,
		Applied:        di.Applied,
		Duplicates:     di.Duplicates,
		LagMax:         di.LagMax,
		RedisAvailable: s.reader.Available(),
	}
	if s.hub != nil {
		out.Counters.StreamClients = s.hub.Subscribers()
	}
	if s.reader.Available() {
		if ok, err := s.reader.Measured(r.Context(), day); err == nil {
			out.Counters.MeasuredToday = ok
		}
	}

	snap, err := s.queueState(r.Context(), r.URL.Query().Get("refresh") == "1")
	if err != nil {
		out.markAll(fieldNotMeasured, "parked_in_db", "staged", "inflight", "mailed", "removed", "static_objects", "loads_without_object")
		respondJSON(w, http.StatusOK, out)
		return
	}
	if snap.ObjectsMeasured {
		oc := snap.Objects[dataIngestOrgID(r)]
		out.Tiles.StaticObjects = i64(oc.StaticObjects)
		out.Tiles.LoadsWithoutObject = i64(oc.LoadsWithoutObject)
		out.markAll(fieldDerived, "static_objects", "loads_without_object")
	} else {
		out.markAll(fieldNotMeasured, "static_objects", "loads_without_object")
	}
	f := foldStates(snap.ByStatus)
	out.Tiles.ParkedInDB = i64(f.Parked)
	out.Tiles.Staged = i64(f.Staged)
	out.Tiles.InFlight = i64(f.InFlight)
	out.Tiles.Mailed = i64(f.Mailed)
	out.Tiles.Removed = i64(f.Removed)
	out.ByStatus = snap.ByStatus
	out.CacheAgeSec = int(time.Since(snap.GeneratedAt).Seconds())
	out.QueryMillis = snap.QueryMillis
	out.markAll(fieldDerived, "parked_in_db", "staged", "inflight", "mailed", "removed")
	respondJSON(w, http.StatusOK, out)
}

// ── GET /stream (SSE) ───────────────────────────────────────────────────────

// HandleStream pushes counter deltas as Server-Sent Events. Modeled on
// WebSocketHub.HandleSSE but mounted INSIDE the authenticated group (never
// /ws/events, which is a different auth surface) and fed by the in-process hub
// the counters consumer publishes to.
//
// Coalesced: deltas arriving inside a 2s window are merged into ONE frame keyed
// by (class, transition, dataset), so a burst of 500 operations is one paint,
// not 500. Heartbeat comment every 15s. Drop-on-slow is in the hub itself.
func (s *DataIngestService) HandleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		respondError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := s.hub.Subscribe()
	defer cancel()

	ticker := time.NewTicker(dataIngestStreamCoalesce)
	defer ticker.Stop()
	beat := time.NewTicker(dataIngestHeartbeat)
	defer beat.Stop()

	type aggKey struct{ class, transition, dataset string }
	pending := map[aggKey]*dataingest.Delta{}

	flush := func() {
		if len(pending) == 0 {
			return
		}
		for _, d := range pending {
			b, err := json.Marshal(d)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: delta\ndata: %s\n\n", b)
		}
		pending = map[aggKey]*dataingest.Delta{}
		flusher.Flush()
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case d, open := <-ch:
			if !open {
				return
			}
			k := aggKey{d.SupplyClass, d.Transition, d.DatasetID}
			cur := pending[k]
			if cur == nil {
				cp := d
				cp.ByISP = map[string]int64{}
				for isp, n := range d.ByISP {
					cp.ByISP[isp] = n
				}
				pending[k] = &cp
				continue
			}
			cur.N += d.N
			for isp, n := range d.ByISP {
				cur.ByISP[isp] += n
			}
		case <-ticker.C:
			flush()
		case <-beat.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

// ── POST /events ────────────────────────────────────────────────────────────

type eventsRequest struct {
	Events []dataingest.Event `json:"events"`
}

type eventsResponse struct {
	diMeta
	Accepted int      `json:"accepted"`
	OpIDs    []string `json:"op_ids"`
}

// HandleEvents is the door the PYTHON writers use (yahoo_family inject,
// drip_supply valve, drivesource): the same JSON the Go Tap produces, POSTed
// with X-Admin-Key. Accepts one event object or {"events":[...]}.
//
// Validation is STRICT and returns 400: a writer that emits a bad transition
// must find out at the call site, not silently in a DLQ hours later.
func (s *DataIngestService) HandleEvents(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	raw, err := io.ReadAll(body)
	if err != nil {
		respondError(w, http.StatusBadRequest, "unreadable body: "+err.Error())
		return
	}
	var batch eventsRequest
	if err := json.Unmarshal(raw, &batch); err != nil || len(batch.Events) == 0 {
		var single dataingest.Event
		if err := json.Unmarshal(raw, &single); err != nil {
			respondError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		batch.Events = []dataingest.Event{single}
	}
	if len(batch.Events) == 0 {
		respondError(w, http.StatusBadRequest, "no events")
		return
	}
	if len(batch.Events) > 500 {
		respondError(w, http.StatusBadRequest, "at most 500 events per request")
		return
	}

	out := eventsResponse{diMeta: s.meta(r, sourceCounters)}
	for i := range batch.Events {
		batch.Events[i].Normalize()
		if err := batch.Events[i].Validate(); err != nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("events[%d]: %s", i, err.Error()))
			return
		}
	}
	for i := range batch.Events {
		dataingest.Emit(r.Context(), batch.Events[i])
		out.OpIDs = append(out.OpIDs, batch.Events[i].OpID)
	}
	out.Accepted = len(batch.Events)
	// Emit is fire-and-forget through a flag-gated tap: accepting the event is
	// NOT a promise that it was counted. Say so rather than implying it.
	out.mark("accepted", fieldMeasured)
	out.mark("counted", fieldNotMeasured)
	respondJSON(w, http.StatusAccepted, out)
}

// ── POST /rollup ────────────────────────────────────────────────────────────

type rollupResponse struct {
	diMeta
	Date     string `json:"date"`
	Rows     int    `json:"rows"`
	Upserted int    `json:"upserted"`
}

// dataIngestRollupUpsert targets the EXPRESSION UNIQUE INDEX
// uq_data_ingest_rollup (cmd/server/main.go runStartupMigrations). Postgres
// does NOT allow an expression in a PRIMARY KEY, so the uniqueness is an index
// and the conflict target must repeat the expression verbatim for inference to
// match it.
const dataIngestRollupUpsert = `
	INSERT INTO data_ingest_rollup (day, supply_class, dataset_id, lane, transition, isp, n, source, computed_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, 'counters', NOW())
	ON CONFLICT (day, supply_class, (COALESCE(dataset_id, '00000000-0000-0000-0000-000000000000'::uuid)), transition, isp, source)
	DO UPDATE SET n = EXCLUDED.n, lane = EXCLUDED.lane, computed_at = NOW()`

// HandleRollup freezes one Denver day's Redis counters into data_ingest_rollup
// with source='counters'. Idempotent: ON CONFLICT DO UPDATE, so a re-run of the
// nightly job (or a manual repeat) converges rather than doubling.
func (s *DataIngestService) HandleRollup(w http.ResponseWriter, r *http.Request) {
	day, err := denverDay(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.reader.Available() {
		respondError(w, http.StatusServiceUnavailable, "counters unavailable (no redis) — nothing to roll up")
		return
	}
	if s.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	rows, err := s.reader.Rollup(r.Context(), day)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "rollup_read_failed: "+err.Error())
		return
	}
	out := rollupResponse{diMeta: s.meta(r, sourceCounters), Date: day, Rows: len(rows)}
	if len(rows) == 0 {
		out.mark("upserted", fieldNotMeasured)
		respondJSON(w, http.StatusOK, out)
		return
	}

	qctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(qctx, nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "rollup_tx_failed: "+err.Error())
		return
	}
	defer tx.Rollback() //nolint:errcheck // committed below on success
	for _, row := range rows {
		var ds any
		if row.DatasetID != "" {
			ds = row.DatasetID
		}
		var lane any
		if row.Lane != "" {
			lane = row.Lane
		}
		if _, err := tx.ExecContext(qctx, dataIngestRollupUpsert,
			row.Day, row.SupplyClass, ds, lane, row.Transition, row.ISP, row.N); err != nil {
			respondError(w, http.StatusInternalServerError, "rollup_write_failed: "+err.Error())
			return
		}
		out.Upserted++
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "rollup_commit_failed: "+err.Error())
		return
	}
	out.mark("upserted", fieldMeasured)
	respondJSON(w, http.StatusOK, out)
}
