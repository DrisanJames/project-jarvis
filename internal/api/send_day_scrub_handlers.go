package api

// =============================================================================
// SEND-DAY AUDIENCE EXPORT + SUPPRESSION SCRUB LOOP (/api/mailing/send-day/*)
// =============================================================================
// Operator ask (2026-07-27): before each send day the day's ACTIVE AUDIENCE is
// exported as MD5s, run through Optizmo (opt-out compliance scrub), and the
// returned suppression MD5s must land back as suppressions BEFORE the send.
// Tonight that was a hand-run SQL export (1.52M md5s from the jul28 manifest's
// 542 inclusion segments) — "This should become software."
//
// Endpoints:
//   GET  /api/mailing/send-day/{date}/audience-md5          — streamed CSV of
//        DISTINCT LOWER(mailing_subscribers.email_hash) across the UNION of
//        inclusion segments of the date's staged/scheduled campaigns (name
//        prefix "<tok> - ", tok = jul28-style date token). Row streaming —
//        never buffered (1.5M rows).
//   GET  /api/mailing/send-day/{date}/audience-md5/summary  — counts only.
//   POST /api/mailing/send-day/{date}/scrub-suppressions    — the Optizmo
//        return: JSON {"md5s": [...]} (multipart CSV is a noted follow-up,
//        same convention as the EO cleaning service). Fail-closed: any
//        non-MD5 entry refuses the WHOLE request.
//
// Audience truth is the DB, not yaml briefs: campaigns'
// pmta_config->'campaign_input'->'inclusion_segments' (the same path
// resolveCampaignSegmentNames and segmentation_health read), materialized
// membership via mailing_segment_members → mailing_subscribers.email_hash
// (the exact column the operator's hand-run export used, so the Optizmo
// round-trip joins back losslessly).
//
// SUPPRESSION STORE (deliberate choice): returned MD5s are resolved back to
// subscriber emails and written to mailing_global_suppressions — NOT
// mailing_offer_suppressions — because a send-day compliance opt-out must
// block the address for EVERY campaign, not one offer. The send worker's
// global check (SendWorkerPool.processQueueItem →
// globalHub.IsSuppressedForBrand, internal/worker/send_worker.go:1723) reads
// the hub's in-memory emailSet, which is loaded from
// mailing_global_suppressions (GlobalSuppressionHub.LoadFromDB,
// internal/engine/global_suppression.go:141). Because that hot-path check is
// EMAIL-keyed, hash-only rows would be INERT — hence the resolve-to-email
// join. Unmatched hashes are reported loudly, never written silently (an
// address we don't hold cannot be queued by the send path). After the bulk
// insert the hub cache is reloaded — the exact bulk-import idiom of
// worker.SuppressionImportService (suppression_import.go:595-600); the
// 15-minute hub reconcile is the standing safety net if the reload fails.
//
// No new tables: the existing global-suppression store is reused end-to-end.

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// VersionSendDayScrubAPI is bumped on every response-shape change.
const VersionSendDayScrubAPI = "1.0"

const (
	sendDayScrubMaxMD5s      = 1000000 // per POST (JSON array); CSV streaming is the follow-up for larger
	sendDayScrubHashChunk    = 10000   // unnest batch size (eoCleanUploadChunk idiom)
	sendDayScrubExportBudget = "300s"  // SET LOCAL statement_timeout for the 1.5M-row export/count
	sendDayScrubImportBudget = "120s"  // SET LOCAL statement_timeout for the resolve+insert tx
	sendDayScrubBadSampleMax = 20      // rejected-entry sample size (eoCleanFailSampleLimit idiom)
	// sendDayScrubStatuses — the pre-send lifecycle: campaigns whose planned
	// audience is still (or currently) in play for the date. Terminal/negative
	// statuses (cancelled, deleted, failed, sent, completed) are excluded.
	sendDayScrubStatuses = "'draft','scheduled','finalizing_audience','preparing','sending','paused'"
)

var sendDayMD5Re = regexp.MustCompile(`^[0-9a-f]{32}$`)
var sendDayUUIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// sendDayDateToken converts an ISO date to the campaign-name token the
// scheduler stamps as the "<tok> - " name prefix: "2026-07-28" → "jul28"
// (agents/scheduling/registry.py date_token, strftime %b%d lowercased —
// zero-padded day, so July 5 is "jul05").
func sendDayDateToken(date string) (string, error) {
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		return "", fmt.Errorf("date must be YYYY-MM-DD: %w", err)
	}
	return strings.ToLower(d.Format("Jan02")), nil
}

// normalizeScrubMD5s lowercases/trims and dedupes (order-preserving) the
// Optizmo return. Any entry that is not a 32-hex MD5 lands in bad — the
// HANDLER REFUSES THE WHOLE REQUEST on any bad entry (the eo-clean
// fail-closed idiom: a silently skipped opt-out is a compliance miss).
func normalizeScrubMD5s(raw []string) (clean []string, bad []string) {
	seen := make(map[string]bool, len(raw))
	for i, r := range raw {
		h := strings.ToLower(strings.TrimSpace(r))
		if h == "" {
			continue // blank lines in a paste/file are noise, not errors
		}
		if !sendDayMD5Re.MatchString(h) {
			bad = append(bad, fmt.Sprintf("entry %d: %q", i+1, r))
			continue
		}
		if !seen[h] {
			seen[h] = true
			clean = append(clean, h)
		}
	}
	return clean, bad
}

// unionInclusionSegments takes each campaign's inclusion_segments JSON array
// (as raw text, one per campaign) and returns the order-preserving deduped
// UNION of uuid-shaped segment ids, plus any non-uuid refs (reported, never
// silently dropped). Pure — unit-tested over fake campaign configs.
func unionInclusionSegments(configArrays []string) (ids []string, skipped []string) {
	seen := make(map[string]bool)
	skippedSeen := make(map[string]bool)
	for _, raw := range configArrays {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var arr []string
		if err := json.Unmarshal([]byte(raw), &arr); err != nil {
			skipped = appendOnce(skipped, skippedSeen, fmt.Sprintf("unparseable:%s", truncateForLog(raw, 40)))
			continue
		}
		for _, id := range arr {
			id = strings.ToLower(strings.TrimSpace(id))
			if id == "" {
				continue
			}
			if !sendDayUUIDRe.MatchString(id) {
				skipped = appendOnce(skipped, skippedSeen, id)
				continue
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids, skipped
}

func appendOnce(dst []string, seen map[string]bool, v string) []string {
	if seen[v] {
		return dst
	}
	seen[v] = true
	return append(dst, v)
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// sendDayScrubHubReloader is the slice of *engine.GlobalSuppressionHub this
// service needs (identical to worker.GlobalSuppressionReloader): refresh the
// in-memory cache the send worker's IsSuppressedForBrand check reads.
type sendDayScrubHubReloader interface {
	LoadFromDB(ctx context.Context) error
	Count() int
}

// ── Store ───────────────────────────────────────────────────────────────────

// SendDayScrubStore is the data-access layer (no raw SQL in handlers).
type SendDayScrubStore struct {
	db *sql.DB
}

func NewSendDayScrubStore(db *sql.DB) *SendDayScrubStore {
	return &SendDayScrubStore{db: db}
}

// CampaignSegmentConfigs returns, for every pre-send-lifecycle campaign of
// the org whose name starts with "<tok> - ", the raw JSON text of
// pmta_config->'campaign_input'->'inclusion_segments' (guarded to arrays —
// the segmentation_health jsonb_typeof idiom), plus the campaign count.
func (st *SendDayScrubStore) CampaignSegmentConfigs(ctx context.Context, orgID uuid.UUID, tok string) ([]string, int, error) {
	rows, err := st.db.QueryContext(ctx, `
		SELECT CASE WHEN jsonb_typeof(c.pmta_config->'campaign_input'->'inclusion_segments') = 'array'
		            THEN (c.pmta_config->'campaign_input'->'inclusion_segments')::text
		            ELSE '[]' END
		FROM mailing_campaigns c
		WHERE c.organization_id = $1::uuid
		  AND c.name LIKE $2
		  AND c.status IN (`+sendDayScrubStatuses+`)`,
		orgID, tok+" - %")
	if err != nil {
		return nil, 0, fmt.Errorf("load campaign configs: %w", err)
	}
	defer rows.Close()
	var configs []string
	for rows.Next() {
		var cfg string
		if err := rows.Scan(&cfg); err != nil {
			return nil, 0, fmt.Errorf("scan campaign config: %w", err)
		}
		configs = append(configs, cfg)
	}
	return configs, len(configs), rows.Err()
}

// VerifySegments filters the unioned segment ids to those that exist
// org-scoped in mailing_segments (the resolveCampaignSegmentNames ownership
// idiom — a foreign-org or stale ref never reaches the members query).
func (st *SendDayScrubStore) VerifySegments(ctx context.Context, orgID uuid.UUID, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := st.db.QueryContext(ctx, `
		SELECT id::text FROM mailing_segments
		WHERE organization_id = $1::uuid AND id = ANY($2::uuid[])`,
		orgID, pq.Array(ids))
	if err != nil {
		return nil, fmt.Errorf("verify segments: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan segment id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// audienceHashSQL is the one audience-resolution statement (export streams
// it, summary COUNTs it): DISTINCT lowercased subscriber email_hash across
// the materialized members of the verified inclusion segments. Joins the
// planner's own membership table (pmta_campaign_planner.go:1126) to
// mailing_subscribers — the same email_hash column the hand-run export used.
const audienceHashSQL = `
	SELECT DISTINCT LOWER(s.email_hash)
	FROM mailing_segment_members m
	JOIN mailing_subscribers s ON s.id = m.subscriber_id
	WHERE m.segment_id = ANY($1::uuid[])
	  AND s.organization_id = $2::uuid
	  AND s.email_hash IS NOT NULL AND s.email_hash <> ''`

// QueryAudienceHashes opens a tx with a raised statement budget (the 1.5M-row
// DISTINCT cannot live inside the prod default statement_timeout) and returns
// the streaming rows. Caller MUST rows.Close() then tx.Rollback() (read-only).
func (st *SendDayScrubStore) QueryAudienceHashes(ctx context.Context, orgID uuid.UUID, segIDs []string) (*sql.Tx, *sql.Rows, error) {
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin export tx: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '`+sendDayScrubExportBudget+`'`); err != nil {
		tx.Rollback()
		return nil, nil, fmt.Errorf("set export budget: %w", err)
	}
	rows, err := tx.QueryContext(ctx, audienceHashSQL, pq.Array(segIDs), orgID)
	if err != nil {
		tx.Rollback()
		return nil, nil, fmt.Errorf("audience query: %w", err)
	}
	return tx, rows, nil
}

// CountAudienceHashes is the summary variant — COUNT(DISTINCT …) under the
// same raised budget.
func (st *SendDayScrubStore) CountAudienceHashes(ctx context.Context, orgID uuid.UUID, segIDs []string) (int64, error) {
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin summary tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '`+sendDayScrubExportBudget+`'`); err != nil {
		return 0, fmt.Errorf("set summary budget: %w", err)
	}
	var n int64
	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM (`+audienceHashSQL+`) u`,
		pq.Array(segIDs), orgID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("audience count: %w", err)
	}
	return n, nil
}

// scrubImportCounts is the outcome of one scrub-return import.
type scrubImportCounts struct {
	UniqueMD5s        int   `json:"unique_md5s"`
	MatchedMD5s       int64 `json:"matched_md5s"`
	SuppressedNew     int64 `json:"suppressed_new"`
	AlreadySuppressed int64 `json:"already_suppressed"`
	UnmatchedMD5s     int64 `json:"unmatched_md5s"`
}

// ImportScrubReturn lands the Optizmo suppression MD5s in
// mailing_global_suppressions, org-scoped, source='optizmo-scrub/<date>'.
// One tx: temp hash table (unnest chunks) → resolve against
// mailing_subscribers.email_hash (idx_subscribers_email_hash) → single
// INSERT…SELECT with ON CONFLICT (organization_id, md5_hash) DO NOTHING —
// the matchAndSuppressSubscribers DB-side idiom, retargeted at the GLOBAL
// store. Idempotent: a re-POST of the same file suppresses 0 new rows.
func (st *SendDayScrubStore) ImportScrubReturn(ctx context.Context, orgID uuid.UUID, date string, md5s []string) (scrubImportCounts, error) {
	counts := scrubImportCounts{UniqueMD5s: len(md5s)}

	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return counts, fmt.Errorf("begin import tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '`+sendDayScrubImportBudget+`'`); err != nil {
		return counts, fmt.Errorf("set import budget: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`CREATE TEMP TABLE _sendday_scrub_hashes (hash VARCHAR(32) NOT NULL) ON COMMIT DROP`); err != nil {
		return counts, fmt.Errorf("create temp table: %w", err)
	}
	for start := 0; start < len(md5s); start += sendDayScrubHashChunk {
		end := start + sendDayScrubHashChunk
		if end > len(md5s) {
			end = len(md5s)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO _sendday_scrub_hashes (hash) SELECT unnest($1::varchar[])`,
			pq.Array(md5s[start:end])); err != nil {
			return counts, fmt.Errorf("load hashes (%d..%d): %w", start, end, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX ON _sendday_scrub_hashes (hash)`); err != nil {
		log.Printf("[send-day-scrub] warning: could not index temp table: %v", err)
	}

	// How many returned hashes exist in our subscriber base at all.
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT h.hash)
		FROM _sendday_scrub_hashes h
		JOIN mailing_subscribers s ON s.email_hash = h.hash
		WHERE s.organization_id = $1::uuid`,
		orgID).Scan(&counts.MatchedMD5s); err != nil {
		return counts, fmt.Errorf("count matches: %w", err)
	}

	// Resolve hash → email and land in the GLOBAL store. DISTINCT ON keeps
	// one row per hash so RowsAffected = newly suppressed distinct md5s.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO mailing_global_suppressions
			(organization_id, email, md5_hash, reason, source, created_at)
		SELECT DISTINCT ON (h.hash) $1, LOWER(TRIM(s.email)), h.hash, 'optizmo', $2, NOW()
		FROM _sendday_scrub_hashes h
		JOIN mailing_subscribers s ON s.email_hash = h.hash
		WHERE s.organization_id = $1::uuid
		ORDER BY h.hash
		ON CONFLICT (organization_id, md5_hash) DO NOTHING`,
		orgID, "optizmo-scrub/"+date)
	if err != nil {
		return counts, fmt.Errorf("bulk suppress: %w", err)
	}
	counts.SuppressedNew, _ = res.RowsAffected()
	counts.AlreadySuppressed = counts.MatchedMD5s - counts.SuppressedNew
	counts.UnmatchedMD5s = int64(counts.UniqueMD5s) - counts.MatchedMD5s

	if err := tx.Commit(); err != nil {
		return counts, fmt.Errorf("commit import tx: %w", err)
	}
	return counts, nil
}

// ── Service ─────────────────────────────────────────────────────────────────

// SendDayScrubService exposes the export/summary/import trio.
type SendDayScrubService struct {
	store *SendDayScrubStore
	hub   sendDayScrubHubReloader // nil until SetGlobalSuppressionHub (engine block)
}

func NewSendDayScrubService(db *sql.DB) *SendDayScrubService {
	return &SendDayScrubService{store: NewSendDayScrubStore(db)}
}

// SetGlobalSuppressionHub wires the hub whose in-memory cache must be
// reloaded after a bulk import (the SuppressionImportService idiom).
func (s *SendDayScrubService) SetGlobalSuppressionHub(h sendDayScrubHubReloader) {
	s.hub = h
}

// RegisterRoutes mounts the service under the /api/mailing group. Flat
// patterns so the existing static /send-day/* routes (volume-reconciliation,
// preflight-batch, …) keep chi static-segment precedence.
func (s *SendDayScrubService) RegisterRoutes(r chi.Router) {
	r.Get("/send-day/{date}/audience-md5", s.HandleExportAudienceMD5)
	r.Get("/send-day/{date}/audience-md5/summary", s.HandleAudienceSummary)
	r.Post("/send-day/{date}/scrub-suppressions", s.HandleImportScrubSuppressions)
}

// resolveDayAudience is the shared date → campaigns → verified-segment-ids
// walk. Writes the error response itself and returns ok=false on any refusal.
func (s *SendDayScrubService) resolveDayAudience(w http.ResponseWriter, r *http.Request) (orgID uuid.UUID, date, tok string, campaigns int, segIDs []string, skipped []string, ok bool) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization not resolved")
		return orgID, "", "", 0, nil, nil, false
	}
	date = chi.URLParam(r, "date")
	tok, err = sendDayDateToken(date)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return orgID, date, "", 0, nil, nil, false
	}
	configs, campaigns, err := s.store.CampaignSegmentConfigs(r.Context(), orgID, tok)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return orgID, date, tok, 0, nil, nil, false
	}
	if campaigns == 0 {
		respondError(w, http.StatusNotFound,
			fmt.Sprintf("no staged/scheduled campaigns named %q for %s — stage the board first", tok+" - …", date))
		return orgID, date, tok, 0, nil, nil, false
	}
	ids, skipped := unionInclusionSegments(configs)
	segIDs, err = s.store.VerifySegments(r.Context(), orgID, ids)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return orgID, date, tok, campaigns, nil, skipped, false
	}
	if len(segIDs) == 0 {
		respondError(w, http.StatusNotFound,
			fmt.Sprintf("%d campaign(s) matched %q but no inclusion segments resolved org-scoped", campaigns, tok+" - …"))
		return orgID, date, tok, campaigns, nil, skipped, false
	}
	return orgID, date, tok, campaigns, segIDs, skipped, true
}

// HandleExportAudienceMD5 streams the day's audience as CSV (header "md5",
// one lowercased email_hash per row, flushed every 5k — never buffered).
func (s *SendDayScrubService) HandleExportAudienceMD5(w http.ResponseWriter, r *http.Request) {
	orgID, date, _, _, segIDs, _, ok := s.resolveDayAudience(w, r)
	if !ok {
		return
	}

	// 10-minute stream budget (ExportSegmentMembersCSV idiom); the DB-side
	// statement budget is raised inside QueryAudienceHashes.
	streamCtx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	tx, rows, err := s.store.QueryAudienceHashes(streamCtx, orgID, segIDs)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()
	defer rows.Close()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-audience-md5.csv"`, date))
	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{"md5"})

	written := 0
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			continue
		}
		_ = cw.Write([]string{hash})
		written++
		if written%5000 == 0 {
			cw.Flush() // progress to the client; no ballooning response buffer
		}
	}
	if err := rows.Err(); err != nil {
		// Headers are gone — log loudly; the truncated CSV is detectable by
		// the summary count the operator sees next to the download.
		log.Printf("[send-day-scrub] export stream error for %s after %d rows: %v", date, written, err)
	}
	log.Printf("[send-day-scrub] exported %d distinct md5s for %s (%d segments)", written, date, len(segIDs))
}

// HandleAudienceSummary returns {campaigns, segments, unique_md5s} without
// the body — the operator's pre-flight numbers.
func (s *SendDayScrubService) HandleAudienceSummary(w http.ResponseWriter, r *http.Request) {
	orgID, date, tok, campaigns, segIDs, skipped, ok := s.resolveDayAudience(w, r)
	if !ok {
		return
	}
	n, err := s.store.CountAudienceHashes(r.Context(), orgID, segIDs)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"api_version":          VersionSendDayScrubAPI,
		"date":                 date,
		"campaign_name_prefix": tok + " - ",
		"campaigns":            campaigns,
		"segments":             len(segIDs),
		"skipped_segment_refs": skipped,
		"unique_md5s":          n,
	})
}

// HandleImportScrubSuppressions lands the Optizmo return. Fail-closed on any
// non-MD5 row; 503 when the suppression hub isn't wired (an import whose
// cache reload cannot run would be send-path-inert until reboot).
func (s *SendDayScrubService) HandleImportScrubSuppressions(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "organization not resolved")
		return
	}
	date := chi.URLParam(r, "date")
	if _, err := sendDayDateToken(date); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.hub == nil {
		respondError(w, http.StatusServiceUnavailable,
			"global suppression hub not wired on this build — import refused (rows would not reach the send-path cache)")
		return
	}

	var req struct {
		MD5s []string `json:"md5s"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if len(req.MD5s) == 0 {
		respondError(w, http.StatusBadRequest, "md5s array is required (CSV multipart upload is a noted follow-up)")
		return
	}
	if len(req.MD5s) > sendDayScrubMaxMD5s {
		respondError(w, http.StatusBadRequest,
			fmt.Sprintf("md5s array exceeds %d entries — split the file", sendDayScrubMaxMD5s))
		return
	}
	clean, bad := normalizeScrubMD5s(req.MD5s)
	if len(bad) > 0 {
		sample := bad
		if len(sample) > sendDayScrubBadSampleMax {
			sample = sample[:sendDayScrubBadSampleMax]
		}
		respondError(w, http.StatusBadRequest, fmt.Sprintf(
			"%d entries are not 32-hex MD5s — whole request refused (fix the file): %s",
			len(bad), strings.Join(sample, "; ")))
		return
	}
	if len(clean) == 0 {
		respondError(w, http.StatusBadRequest, "md5s array normalized to 0 hashes")
		return
	}

	counts, err := s.store.ImportScrubReturn(r.Context(), orgID, date, clean)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Reload the hub cache so the send worker's IsSuppressedForBrand sees the
	// new rows NOW, not at next boot (suppression_import.go:595 idiom). On
	// failure the DB rows are already durable and the 15-min reconcile loop
	// repairs the cache — surfaced, never silent.
	hubReloaded := false
	hubCount := 0
	var hubErr string
	if counts.SuppressedNew > 0 {
		reloadCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.hub.LoadFromDB(reloadCtx); err != nil {
			hubErr = err.Error()
			log.Printf("[send-day-scrub] ERROR: hub cache reload failed after %s import (%d new): %v — reconcile loop will repair",
				date, counts.SuppressedNew, err)
		} else {
			hubReloaded = true
			hubCount = s.hub.Count()
			log.Printf("[send-day-scrub] %s: %d new global suppressions live in hub (%d entries)",
				date, counts.SuppressedNew, hubCount)
		}
	} else {
		hubReloaded = true // nothing new; cache already coherent
		hubCount = s.hub.Count()
	}

	resp := map[string]interface{}{
		"api_version":        VersionSendDayScrubAPI,
		"date":               date,
		"source":             "optizmo-scrub/" + date,
		"received":           len(req.MD5s),
		"unique_md5s":        counts.UniqueMD5s,
		"matched_md5s":       counts.MatchedMD5s,
		"suppressed_new":     counts.SuppressedNew,
		"already_suppressed": counts.AlreadySuppressed,
		"unmatched_md5s":     counts.UnmatchedMD5s,
		"hub_reloaded":       hubReloaded,
		"hub_count":          hubCount,
	}
	if hubErr != "" {
		resp["hub_reload_error"] = hubErr + " — rows are durable in mailing_global_suppressions; the 15-minute reconcile repairs the cache"
	}
	respondJSON(w, http.StatusOK, resp)
}
