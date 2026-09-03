package api

// =============================================================================
// FRESH BROADCAST RUNNER — the fresh-data introduction loop as native software
// =============================================================================
// Operator 2026-07-27: "This must become software, it must become repetitive,
// maybe it is an API endpoint you can invoke and/or have the system invoke as
// well." ONE runner, three invocation paths:
//
//   1. API      POST /api/mailing/fresh-broadcast/runs   (fresh_broadcast_handlers.go)
//   2. Worker   FreshBroadcastWorker nightly 09:00 UTC   (internal/worker/fresh_broadcast_worker.go)
//   3. Screen   "Run DRY now" on FreshBroadcastConfig.tsx
//
// ⚠ PARITY CONTRACT — the Python side is AUTHORITATIVE until this deploys and
// proves parity: agents/scheduling/stream_router.py (draw predicate, claim
// semantics, dated-segment naming <PREFIX>-<TOK>-<SITE>-<LANE>, stable md5→site
// hash, EO gate, prior-batch exclusion) + board_generator.build_fresh_bcast_briefs
// (one campaign per stream×destination, offer-binds-to-stream, SES FF
// transform). Segment ids are uuid5 in the SAME namespace
// (c0f5ee21-0726-4b3a-9e17-000000000000) so the two implementations can never
// double-stage a batch: whichever side stages first, the other side's
// idempotency check sees the identically-named/ID'd segments and skips.
//
// Config source: mailing_stream_broadcast_config (operator-editable, portal
// screen; cross-team table contract — see stream_broadcast_config.go). ALL
// fields — knobs AND structural (dataset_ids / primary_sites / sending_domain /
// sending_profile_id / source_tags) — are read from the table.
//
// What a LIVE (non-dry) run does per ENABLED stream:
//   DRAW      queue-source: partner_clean_queue status='ready' ∩ eo_result IN
//             eo_mailable ∩ dataset_id ANY(dataset_ids) ∩ NOT paused_emergency,
//             freshest-first (COALESCE(last_pushed_at, ingested_at) DESC),
//             LIMIT daily_cap, isp_caps 0 = excluded from the draw.
//             tag-source (wcm shape; dataset_ids empty + source_tags set):
//             mailing_subscribers carrying any source_tag ∩ EO-Verified ∩
//             never-mailed ∩ unsuppressed ∩ NOT already in a prior <PREFIX>-*
//             segment (the dated membership IS the claim).
//   STAGE     dated segments (uuid5 ids), hydrate queue rows to subscribers
//             (python's exact insert shape incl. vertical_tag in tags), insert
//             members, claim queue rows (status='claimed' + claim_note).
//   CAMPAIGNS one DRAFT per (stream × destination site) via the same internal
//             path POST /pmta-campaign/stage uses (stagePMTADraftCampaign —
//             called directly, not over HTTP). Copy comes from the most recent
//             APPROVED mailing_offer_proofs row for the stream's offer;
//             SES-routed brand cells apply the "[Publication] featuring
//             [Partner]" FF transform (jul21 SES doctrine); the explicit
//             first-party lane (WCL) keeps the proof FF verbatim — it IS the
//             brand. Gmail doctrine: gmail-eligible volume assigns ONLY to an
//             explicit-lane destination when the stream has one, else gmail is
//             excluded from the draw entirely.
//
// Everything lands as status='draft' — the Draft Board remains the operator
// approval gate. NO auto-promote in v1 (auto_stage only lets the nightly
// worker run the STAGE step; promotion stays manual).
//
// Idempotency / resume safety: a non-dry run for a (date, stream) whose dated
// segments already exist SKIPS that stream with an explicit reason.
//
// Critical-pipeline note: this file only WRITES drafts + segments + queue
// claims. It never deploys, never touches waves/queue enqueue. HOLD-CRITICAL
// discipline still applies to its deploy (segment claims feed the send path).

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// freshBroadcastNS is the uuid5 namespace shared with the Python router
// (agents/scheduling/stream_router.py CONSUMER_NS). NEVER change it — parity
// of segment ids across the two implementations is the double-stage guard.
var freshBroadcastNS = uuid.MustParse("c0f5ee21-0726-4b3a-9e17-000000000000")

const (
	// freshBroadcastStartUTCHour — staged drafts carry an 11:00 UTC (05:00
	// Denver in summer) start: ~one hour after the engager board's 04:01
	// Denver start, the Morning Framework's cold-sidecar slot. The operator
	// adjusts timing on the Draft Board before promoting.
	freshBroadcastStartUTCHour = 11

	freshBroadcastTimezone = "America/Denver"

	// freshBroadcastExclusionList is the standing global suppression list
	// every board carries (resolved by NAME at stage time, same as the
	// Python compiler's brief.exclusion_lists).
	freshBroadcastExclusionList = "global-suppression-list"
)

// ── Registry mirrors (kept in lock-step with agents/registry/brand_metadata.py;
//    same precedent as internal/worker/engagement_family_builder.go) ──────────

// freshBrandApex maps a canonical brand code to its apex domain
// (BRAND_DOMAIN). Broadcast brands only — the destinations a stream's
// primary_sites may name.
var freshBrandApex = map[string]string{
	"DB": "discountblog.com", "HT": "historythinking.com", "MH": "myownhealth.net",
	"QF": "quizfiesta.com", "BW": "businessweeklypro.com", "FC": "financialcalculate.com",
	"CP": "consumerpro.net", "HW": "homewarrantyservices.org", "RR": "refinanceratesusa.com",
	"TT": "thingoftheday.org", "YI": "yourinsurancehub.com", "MR": "myrepairdiy.com",
	"CI": "casainsure.com", "LP": "learnpersonalloans.com", "RB": "ratesbazar.com",
	"WF": "warrantyforyou.com",
}

// freshBrandLabel maps a brand code to its publication label (BRAND_LABEL) —
// the "[Publication]" half of the SES friendly-from transform.
var freshBrandLabel = map[string]string{
	"DB": "Discount Blog", "HT": "History Thinking", "MH": "My Own Health",
	"QF": "Quiz Fiesta", "BW": "Business Weekly Pro", "FC": "Financial Calculate",
	"CP": "Consumer Pro", "HW": "Home Warranty Services", "RR": "Refinance Rates USA",
	"TT": "Thing of the Day", "YI": "Your Insurance Hub", "MR": "My Repair DIY",
	"CI": "Casa Insure", "LP": "Learn Personal Loans", "RB": "Rates Bazar",
	"WF": "Warranty For You",
}

// freshLaneTokens mirrors agents/scheduling/data/isp_roster.json lane_tokens.
// Any other isp_family folds into OTHER (a mailable record is never dropped
// by this layer — stream_router.lane_of semantics).
var freshLaneTokens = map[string]string{
	"microsoft": "MICROSOFT", "apple": "APPLE", "gmail": "GMAIL",
	"yahoo": "YAHOO", "aol": "AOL", "att": "ATT",
	"sbcglobal": "SBCGLOBAL", "cox": "COX", "comcast": "COMCAST",
}

// ── Pure derivations (unit-tested; parity goldens vs the Python side) ────────

// freshBatchToken renders the dated batch token: 2026-07-28 → "J28"
// (registry.fresh_token — month initial + unpadded day).
func freshBatchToken(date time.Time) string {
	return strings.ToUpper(date.Format("Jan")[:1]) + strconv.Itoa(date.Day())
}

// freshDateToken renders the campaign-name date token: 2026-07-28 → "jul28"
// (registry.date_token — %b%d lowercased, day zero-padded).
func freshDateToken(date time.Time) string {
	return strings.ToLower(date.Format("Jan")) + date.Format("02")
}

// freshSegName builds the dated segment name: <PREFIX>-<TOK>-<SITE>-<LANE>.
func freshSegName(prefix, token, site, lane string) string {
	return prefix + "-" + token + "-" + site + "-" + lane
}

// freshSegID derives the deterministic uuid5 segment id in the shared
// namespace (uuid.NewSHA1 IS RFC-4122 version 5 — SHA-1 name-based).
func freshSegID(name string) string {
	return uuid.NewSHA1(freshBroadcastNS, []byte(name)).String()
}

// freshBatchMarker: python's marker literal, e.g. "consumer-j28-20260728".
func freshBatchMarker(prefix, token string, date time.Time) string {
	return strings.ToLower(prefix) + "-" + strings.ToLower(token) + "-" + date.Format("20060102")
}

// freshLaneOf folds an isp_family into its board lane token.
func freshLaneOf(ispFamily string) string {
	if t, ok := freshLaneTokens[strings.ToLower(strings.TrimSpace(ispFamily))]; ok {
		return t
	}
	return "OTHER"
}

// freshExplicitSiteCode derives an explicit lane's pseudo-site code from its
// sending domain, matching stream_routing.json's site_code convention
// (m.wcl-heloc.com → "WCL"). First label token up to '-' or '.', upper-cased,
// after stripping the m./em. mailing prefix.
func freshExplicitSiteCode(sendingDomain string) string {
	d := strings.ToLower(strings.TrimSpace(sendingDomain))
	d = strings.TrimPrefix(d, "em.")
	d = strings.TrimPrefix(d, "m.")
	for i, r := range d {
		if r == '-' || r == '.' {
			return strings.ToUpper(d[:i])
		}
	}
	return strings.ToUpper(d)
}

// freshAssignSite is the stable site assignment: same md5 → same site,
// forever (stream_router.assign_site — int(md5[:8],16) % len(sites)).
func freshAssignSite(emailMD5 string, sites []string) (string, error) {
	if len(sites) == 0 {
		return "", fmt.Errorf("assign_site: empty site list — REFUSE")
	}
	if len(emailMD5) < 8 {
		return "", fmt.Errorf("assign_site: empty/short email_md5 — REFUSE")
	}
	v, err := strconv.ParseUint(emailMD5[:8], 16, 64)
	if err != nil {
		return "", fmt.Errorf("assign_site: bad md5 prefix %q: %w", emailMD5[:8], err)
	}
	return sites[int(v%uint64(len(sites)))], nil
}

// freshFromName applies the jul21 SES friendly-from doctrine: partner-only
// FFs are banned on the brand SES lanes → "[Publication] featuring [Partner]".
// The explicit first-party lane is exempt (it IS the brand) — the approved
// proof FF rides verbatim.
func freshFromName(brandLabel, proofFromName string, explicitLane bool) string {
	if explicitLane {
		return proofFromName
	}
	return brandLabel + " featuring " + proofFromName
}

// freshDrawRow is one drawn record (queue-row shape; QueueID=="" marks a
// tag-source row with no queue row to claim).
type freshDrawRow struct {
	QueueID      string
	Email        string
	EmailMD5     string
	ISPFamily    string
	SubscriberID string
	PushCount    int
}

// freshApplyCaps trims a freshest-first draw under daily_cap + finite
// isp_caps, preserving order so the freshest rows win the cap
// (stream_router.apply_caps). isp_caps==0 entries never reach here — they are
// excluded SQL-side.
func freshApplyCaps(rows []freshDrawRow, dailyCap int, ispCaps map[string]int) (kept []freshDrawRow, perISP, trimmed map[string]int) {
	caps := make(map[string]int, len(ispCaps))
	for k, v := range ispCaps {
		caps[strings.ToLower(k)] = v
	}
	perISP, trimmed = map[string]int{}, map[string]int{}
	for _, r := range rows {
		if dailyCap >= 0 && len(kept) >= dailyCap {
			break
		}
		fam := strings.ToLower(r.ISPFamily)
		if fam == "" {
			fam = "other"
		}
		if cap, ok := caps[fam]; ok && perISP[fam] >= cap {
			trimmed[fam]++
			continue
		}
		kept = append(kept, r)
		perISP[fam]++
	}
	return kept, perISP, trimmed
}

// freshCell is one (site, lane) staging cell.
type freshCell struct {
	Site string
	Lane string
}

// freshPlanAssignment maps drawn rows to (site, lane) cells via the stable
// hash. Gmail doctrine: gmail rows assign ONLY to the explicit lane when one
// exists; with no explicit lane they are masked (counted, never staged —
// belt-and-braces: the draw already excludes gmail for such streams). The
// pool order is primary_sites then the explicit site — the SAME order the
// Python sites() builds, which the hash parity depends on.
func freshPlanAssignment(rows []freshDrawRow, primarySites []string, explicitSite string) (map[freshCell][]freshDrawRow, int, error) {
	pool := append(append([]string{}, primarySites...), func() []string {
		if explicitSite != "" {
			return []string{explicitSite}
		}
		return nil
	}()...)
	out := map[freshCell][]freshDrawRow{}
	masked := 0
	for _, r := range rows {
		fam := strings.ToLower(r.ISPFamily)
		rowPool := pool
		if fam == "gmail" {
			if explicitSite == "" {
				masked++
				continue
			}
			rowPool = []string{explicitSite}
		}
		md5hex := r.EmailMD5
		if len(md5hex) < 8 {
			sum := md5.Sum([]byte(strings.ToLower(r.Email)))
			md5hex = hex.EncodeToString(sum[:])
		}
		site, err := freshAssignSite(md5hex, rowPool)
		if err != nil {
			return nil, masked, err
		}
		cell := freshCell{Site: site, Lane: freshLaneOf(r.ISPFamily)}
		out[cell] = append(out[cell], r)
	}
	return out, masked, nil
}

// ── Draw SQL builders (shape unit-tested against the Python reference) ───────

// freshQueueDrawSQL mirrors stream_router.draw_stageable: ready + EO-mailable
// + not emergency-paused, freshest-first by re-push signal. $1 dataset uuids,
// $2 eo statuses, then optionally excluded isps, then optionally LIMIT.
func freshQueueDrawSQL(excluded, limited bool) string {
	sqlStr := `SELECT q.id::text, q.email, COALESCE(q.email_md5, ''), COALESCE(q.isp_family, ''),
	       COALESCE(q.subscriber_id::text, ''), COALESCE(q.push_count, 1)
	FROM partner_clean_queue q
	WHERE q.dataset_id = ANY($1::uuid[])
	  AND q.status = 'ready'
	  AND q.eo_result = ANY($2)
	  AND NOT EXISTS (SELECT 1 FROM partner_datasets d
	                  WHERE d.id = q.dataset_id AND d.paused_emergency)`
	n := 3
	if excluded {
		sqlStr += fmt.Sprintf("\n\t  AND lower(COALESCE(q.isp_family, 'other')) <> ALL($%d::text[])", n)
		n++
	}
	sqlStr += "\n\tORDER BY COALESCE(q.last_pushed_at, q.ingested_at) DESC"
	if limited {
		sqlStr += fmt.Sprintf("\n\tLIMIT $%d", n)
	}
	return sqlStr
}

// freshTagDrawSQL mirrors stream_router.tag_draw_sql (the WCM shape):
// subscribers carrying any source tag ∩ EO-validated ∩ NEVER-MAILED ∩
// unsuppressed, excluding members of any prior <PREFIX>-* batch segment —
// the dated segment membership IS the claim. $1 tags, $2 eo statuses,
// $3 prefix LIKE pattern, then optionals.
func freshTagDrawSQL(ispCase string, excluded, limited bool) string {
	sqlStr := `SELECT s.id::text, lower(s.email), COALESCE(s.email_hash, ''), ` + ispCase + `
	FROM mailing_subscribers s
	WHERE s.tags && $1::text[]
	  AND s.status IN ('pending','confirmed')
	  AND COALESCE(s.total_emails_received, 0) = 0
	  AND s.last_email_at IS NULL
	  AND EXISTS (SELECT 1 FROM mailing_eo_validation v
	              WHERE v.email_lower = lower(s.email)
	                AND v.status = ANY($2))
	  AND NOT EXISTS (SELECT 1 FROM mailing_global_suppressions g
	                  WHERE lower(g.email) = lower(s.email))
	  AND NOT EXISTS (SELECT 1 FROM mailing_segment_members m
	                  JOIN mailing_segments seg ON seg.id = m.segment_id
	                  WHERE m.subscriber_id = s.id
	                    AND seg.name LIKE $3)`
	n := 4
	if excluded {
		sqlStr += fmt.Sprintf("\n\t  AND %s <> ALL($%d::text[])", ispCase, n)
		n++
	}
	sqlStr += "\n\tORDER BY s.created_at DESC"
	if limited {
		sqlStr += fmt.Sprintf("\n\tLIMIT $%d", n)
	}
	return sqlStr
}

// ── Config / result types ────────────────────────────────────────────────────

// freshStreamConfig is one enabled stream's full row (knobs + structural).
type freshStreamConfig struct {
	StreamKey        string
	DailyCap         int
	ISPCaps          map[string]int
	Offer            string
	ThrottleHours    int
	Label            string
	SegPrefix        string
	VerticalTag      string
	DatasetIDs       []string
	PrimarySites     []string
	EOMailable       []string
	SendingDomain    string
	SendingProfileID string
	SourceTags       []string
	AutoStage        bool
}

// tagSourced reports the wcm shape: no queue datasets, batch tags declared.
func (c freshStreamConfig) tagSourced() bool {
	return len(c.DatasetIDs) == 0 && len(c.SourceTags) > 0
}

// FreshCampaignResult is one staged (or planned/refused) draft campaign.
type FreshCampaignResult struct {
	CampaignID    string `json:"campaign_id,omitempty"`
	Name          string `json:"name"`
	Site          string `json:"site"`
	SendingDomain string `json:"sending_domain"`
	Planned       int    `json:"planned"`
	Status        string `json:"status"` // draft | planned (dry) | refused
	Reason        string `json:"reason,omitempty"`
}

// FreshStreamResult is one stream's per-run outcome (the runs-table payload).
type FreshStreamResult struct {
	Stream          string                `json:"stream"`
	Status          string                `json:"status"` // ok | skipped | refused | error
	Reason          string                `json:"reason,omitempty"`
	Drawn           int                   `json:"drawn"`
	Fill            int                   `json:"fill"`
	Cap             int                   `json:"cap"`
	Short           int                   `json:"short"`
	PerISP          map[string]int        `json:"per_isp,omitempty"`
	Masked          int                   `json:"masked,omitempty"`
	SegmentsCreated int                   `json:"segments_created"`
	MembersAdded    int                   `json:"members_added"`
	Claimed         int                   `json:"claimed"`
	Campaigns       []FreshCampaignResult `json:"campaigns"`
}

// FreshRunOptions parameterizes one run.
type FreshRunOptions struct {
	Date          string // YYYY-MM-DD; "" = today in America/Denver
	Dry           bool
	Trigger       string // api | worker | screen
	AutoStageOnly bool   // worker LIVE pass: only streams with auto_stage
}

// FreshRunResult is the run record returned to callers.
type FreshRunResult struct {
	RunID   string              `json:"run_id"`
	Date    string              `json:"date"`
	Dry     bool                `json:"dry"`
	Trigger string              `json:"trigger"`
	Status  string              `json:"status"` // ok | partial | failed
	Streams []FreshStreamResult `json:"streams"`
}

// ── Runner ───────────────────────────────────────────────────────────────────

// FreshBroadcastRunner owns the draw→stage→draft loop.
type FreshBroadcastRunner struct {
	db       *sql.DB
	store    *StreamBroadcastStore
	colCache *campaignColumnCache
}

// NewFreshBroadcastRunner constructs the runner (probes the campaign column
// cache once, like NewPMTACampaignService).
func NewFreshBroadcastRunner(db *sql.DB) *FreshBroadcastRunner {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return &FreshBroadcastRunner{
		db:       db,
		store:    NewStreamBroadcastStore(db),
		colCache: probeCampaignColumns(ctx, db),
	}
}

// resolveRunDate parses opts.Date or defaults to today in America/Denver
// (the platform's operating day).
func resolveRunDate(raw string, now time.Time) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		loc, err := time.LoadLocation(freshBroadcastTimezone)
		if err != nil {
			return time.Time{}, err
		}
		n := now.In(loc)
		return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC), nil
	}
	return time.Parse("2006-01-02", raw)
}

// Run executes one full pass for one org and records it in
// mailing_fresh_broadcast_runs. LIVE staging uses a context detached from any
// HTTP request so a client disconnect cannot abandon a half-staged stream.
func (r *FreshBroadcastRunner) Run(ctx context.Context, orgID uuid.UUID, opts FreshRunOptions) (FreshRunResult, error) {
	date, err := resolveRunDate(opts.Date, time.Now())
	if err != nil {
		return FreshRunResult{}, fmt.Errorf("bad date %q: %w", opts.Date, err)
	}
	trigger := strings.TrimSpace(opts.Trigger)
	if trigger == "" {
		trigger = "api"
	}
	res := FreshRunResult{
		Date: date.Format("2006-01-02"), Dry: opts.Dry, Trigger: trigger, Status: "ok",
	}

	runID, err := r.insertRun(ctx, orgID, res)
	if err != nil {
		return FreshRunResult{}, fmt.Errorf("record run: %w", err)
	}
	res.RunID = runID

	streams, err := r.loadEnabledStreams(ctx, orgID, opts.AutoStageOnly)
	if err != nil {
		res.Status = "failed"
		r.finishRun(ctx, runID, res)
		return res, fmt.Errorf("load stream config: %w", err)
	}

	for _, cfg := range streams {
		sr := r.processStream(ctx, orgID, cfg, date, opts.Dry)
		res.Streams = append(res.Streams, sr)
		if sr.Status == "error" {
			res.Status = "partial"
		}
	}
	r.finishRun(ctx, runID, res)
	return res, nil
}

// loadEnabledStreams reads every ENABLED stream row (knobs + structural),
// optionally filtered to auto_stage streams (the worker's LIVE pass).
func (r *FreshBroadcastRunner) loadEnabledStreams(ctx context.Context, orgID uuid.UUID, autoStageOnly bool) ([]freshStreamConfig, error) {
	q := `
		SELECT stream_key, daily_cap, isp_caps::text, offer, throttle_hours, label,
		       seg_prefix, COALESCE(vertical_tag, ''), dataset_ids::text,
		       primary_sites::text, eo_mailable::text,
		       COALESCE(sending_domain, ''), COALESCE(sending_profile_id, ''),
		       COALESCE(source_tags::text, '[]'), COALESCE(auto_stage, FALSE)
		FROM mailing_stream_broadcast_config
		WHERE organization_id = $1 AND enabled`
	if autoStageOnly {
		q += ` AND auto_stage`
	}
	q += ` ORDER BY stream_key`
	rows, err := r.db.QueryContext(ctx, q, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []freshStreamConfig
	for rows.Next() {
		var c freshStreamConfig
		var ispCaps, datasets, primary, eo, tags string
		if err := rows.Scan(&c.StreamKey, &c.DailyCap, &ispCaps, &c.Offer,
			&c.ThrottleHours, &c.Label, &c.SegPrefix, &c.VerticalTag,
			&datasets, &primary, &eo, &c.SendingDomain, &c.SendingProfileID,
			&tags, &c.AutoStage); err != nil {
			return nil, err
		}
		c.ISPCaps = map[string]int{}
		_ = json.Unmarshal([]byte(ispCaps), &c.ISPCaps)
		_ = json.Unmarshal([]byte(datasets), &c.DatasetIDs)
		_ = json.Unmarshal([]byte(primary), &c.PrimarySites)
		_ = json.Unmarshal([]byte(eo), &c.EOMailable)
		_ = json.Unmarshal([]byte(tags), &c.SourceTags)
		out = append(out, c)
	}
	return out, rows.Err()
}

// refuse builds a refused-stream result (offer/copy/config problems).
func refuse(stream, reason string, cap int) FreshStreamResult {
	return FreshStreamResult{Stream: stream, Status: "refused", Reason: reason,
		Cap: cap, Campaigns: []FreshCampaignResult{}}
}

// processStream runs draw→(stage→campaigns) for one stream. Never panics the
// pass — every failure lands as a structured per-stream result.
func (r *FreshBroadcastRunner) processStream(ctx context.Context, orgID uuid.UUID, cfg freshStreamConfig, date time.Time, dry bool) FreshStreamResult {
	token := freshBatchToken(date)

	if cfg.SegPrefix == "" {
		return refuse(cfg.StreamKey, "stream has no seg_prefix — REFUSE", cfg.DailyCap)
	}
	if cfg.DailyCap <= 0 {
		return FreshStreamResult{Stream: cfg.StreamKey, Status: "skipped",
			Reason: "daily_cap 0 — draws 0 by rule", Cap: cfg.DailyCap,
			Campaigns: []FreshCampaignResult{}}
	}
	if !cfg.tagSourced() && len(cfg.DatasetIDs) == 0 {
		return refuse(cfg.StreamKey, "no dataset_ids and no source_tags — no draw source", cfg.DailyCap)
	}

	// Offer must resolve to a mailing_offers row — REFUSE otherwise (the
	// board_offer_id NULL suppression gap: NULL offer_id means offer/converted
	// suppression never fires).
	offer, err := r.store.ResolveOffer(ctx, orgID, cfg.Offer)
	if err != nil {
		return FreshStreamResult{Stream: cfg.StreamKey, Status: "error",
			Reason: "resolve offer: " + err.Error(), Cap: cfg.DailyCap,
			Campaigns: []FreshCampaignResult{}}
	}
	if offer == nil {
		return refuse(cfg.StreamKey,
			fmt.Sprintf("offer %q does not resolve to a mailing_offers row — REFUSE", cfg.Offer), cfg.DailyCap)
	}

	// Approved copy: most recent APPROVED proofs-registry row for the offer.
	copySrc, err := r.resolveApprovedCopy(ctx, orgID, cfg.Offer)
	if err != nil {
		return FreshStreamResult{Stream: cfg.StreamKey, Status: "error",
			Reason: "approved copy: " + err.Error(), Cap: cfg.DailyCap,
			Campaigns: []FreshCampaignResult{}}
	}
	if copySrc == nil {
		return refuse(cfg.StreamKey,
			fmt.Sprintf("no APPROVED active mailing_offer_proofs row for offer %q — copy is operator-approved pool only, REFUSE", cfg.Offer), cfg.DailyCap)
	}

	// Idempotency: a non-dry run whose dated segments already exist SKIPS the
	// stream (resume-safe; also the cross-implementation double-stage guard).
	if !dry {
		exists, err := r.datedSegmentsExist(ctx, orgID, cfg.SegPrefix, token)
		if err != nil {
			return FreshStreamResult{Stream: cfg.StreamKey, Status: "error",
				Reason: "idempotency check: " + err.Error(), Cap: cfg.DailyCap,
				Campaigns: []FreshCampaignResult{}}
		}
		if exists {
			return FreshStreamResult{Stream: cfg.StreamKey, Status: "skipped",
				Reason: fmt.Sprintf("dated segments %s-%s-* already exist — batch already staged (resume-safe skip)", cfg.SegPrefix, token),
				Cap: cfg.DailyCap, Campaigns: []FreshCampaignResult{}}
		}
	}

	// Excluded ISPs: isp_caps 0 = excluded from the draw entirely, plus the
	// gmail doctrine — no explicit lane means gmail never draws.
	excludedSet := map[string]bool{}
	finiteCaps := map[string]int{}
	for k, v := range cfg.ISPCaps {
		if v == 0 {
			excludedSet[strings.ToLower(k)] = true
		} else {
			finiteCaps[strings.ToLower(k)] = v
		}
	}
	if cfg.SendingDomain == "" {
		excludedSet["gmail"] = true
	}
	var excluded []string
	for k := range excludedSet {
		excluded = append(excluded, k)
	}

	rows, err := r.draw(ctx, cfg, token, excluded)
	if err != nil {
		return FreshStreamResult{Stream: cfg.StreamKey, Status: "error",
			Reason: "draw: " + err.Error(), Cap: cfg.DailyCap,
			Campaigns: []FreshCampaignResult{}}
	}
	drawn := len(rows)
	rows, perISPCounts, _ := freshApplyCaps(rows, cfg.DailyCap, finiteCaps)

	fill := len(rows)
	short := cfg.DailyCap - fill
	if short < 0 {
		short = 0
	}

	explicitSite := ""
	if cfg.SendingDomain != "" {
		explicitSite = freshExplicitSiteCode(cfg.SendingDomain)
	}
	assignment, masked, err := freshPlanAssignment(rows, cfg.PrimarySites, explicitSite)
	if err != nil {
		return FreshStreamResult{Stream: cfg.StreamKey, Status: "error",
			Reason: "assignment: " + err.Error(), Cap: cfg.DailyCap,
			Campaigns: []FreshCampaignResult{}}
	}

	result := FreshStreamResult{
		Stream: cfg.StreamKey, Status: "ok", Drawn: drawn, Fill: fill,
		Cap: cfg.DailyCap, Short: short, PerISP: perISPCounts, Masked: masked,
		Campaigns: []FreshCampaignResult{},
	}

	if dry {
		result.Campaigns = r.buildCampaigns(ctx, orgID, cfg, offer, copySrc, assignment, date, true)
		return result
	}

	staged, err := r.stage(ctx, orgID, cfg, assignment, date, token)
	if err != nil {
		result.Status = "error"
		result.Reason = "stage: " + err.Error()
		return result
	}
	result.SegmentsCreated = staged.segmentsCreated
	result.MembersAdded = staged.membersAdded
	result.Claimed = staged.claimed

	result.Campaigns = r.buildCampaigns(ctx, orgID, cfg, offer, copySrc, assignment, date, false)
	for _, c := range result.Campaigns {
		if c.Status == "refused" {
			result.Status = "error"
			if result.Reason == "" {
				result.Reason = "one or more destinations refused: " + c.Reason
			}
		}
	}
	return result
}

// draw executes the source-appropriate draw.
func (r *FreshBroadcastRunner) draw(ctx context.Context, cfg freshStreamConfig, token string, excluded []string) ([]freshDrawRow, error) {
	dctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if cfg.tagSourced() {
		ispCase := isp.SQLCaseFromEmail("s.email")
		q := freshTagDrawSQL(ispCase, len(excluded) > 0, true)
		args := []any{pq.Array(cfg.SourceTags), pq.Array(cfg.EOMailable), cfg.SegPrefix + "-%"}
		if len(excluded) > 0 {
			args = append(args, pq.Array(excluded))
		}
		args = append(args, cfg.DailyCap)
		rows, err := r.db.QueryContext(dctx, q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []freshDrawRow
		for rows.Next() {
			var d freshDrawRow
			if err := rows.Scan(&d.SubscriberID, &d.Email, &d.EmailMD5, &d.ISPFamily); err != nil {
				return nil, err
			}
			d.PushCount = 1
			out = append(out, d)
		}
		return out, rows.Err()
	}

	q := freshQueueDrawSQL(len(excluded) > 0, true)
	args := []any{pq.Array(cfg.DatasetIDs), pq.Array(cfg.EOMailable)}
	if len(excluded) > 0 {
		args = append(args, pq.Array(excluded))
	}
	args = append(args, cfg.DailyCap)
	rows, err := r.db.QueryContext(dctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []freshDrawRow
	for rows.Next() {
		var d freshDrawRow
		if err := rows.Scan(&d.QueueID, &d.Email, &d.EmailMD5, &d.ISPFamily,
			&d.SubscriberID, &d.PushCount); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// datedSegmentsExist is the resume-safe idempotency probe.
func (r *FreshBroadcastRunner) datedSegmentsExist(ctx context.Context, orgID uuid.UUID, prefix, token string) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM mailing_segments
			WHERE organization_id = $1 AND name LIKE $2
		)`, orgID, prefix+"-"+token+"-%").Scan(&exists)
	return exists, err
}

// freshApprovedCopy is the resolved copy for a stream's offer.
type freshApprovedCopy struct {
	ProofID   string
	ProofName string
	Subject   string
	Preheader string
	FromName  string
	HTML      string
}

// freshApprovedCopySQL — THE copy-source query: the most recent APPROVED,
// active proofs-registry row for the offer. approved_at wins; updated_at is
// the tiebreaker for legacy rows approved before approved_at existed.
const freshApprovedCopySQL = `
	SELECT p.id::text, p.name, p.html_content, p.variants::text, p.from_names::text
	FROM mailing_offer_proofs p
	WHERE p.organization_id = $1
	  AND p.offer_key = $2
	  AND p.approval_status = 'approved'
	  AND p.is_active
	ORDER BY COALESCE(p.approved_at, p.updated_at) DESC, p.created_at DESC
	LIMIT 1`

// resolveApprovedCopy returns nil (no error) when no approved row exists.
func (r *FreshBroadcastRunner) resolveApprovedCopy(ctx context.Context, orgID uuid.UUID, offerKey string) (*freshApprovedCopy, error) {
	var c freshApprovedCopy
	var variantsB, fromNamesB string
	err := r.db.QueryRowContext(ctx, freshApprovedCopySQL, orgID, offerKey).
		Scan(&c.ProofID, &c.ProofName, &c.HTML, &variantsB, &fromNamesB)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var variants []proofVariant
	_ = json.Unmarshal([]byte(variantsB), &variants)
	var fromNames []string
	_ = json.Unmarshal([]byte(fromNamesB), &fromNames)
	if len(variants) > 0 {
		c.Subject = strings.TrimSpace(variants[0].Subject)
		c.Preheader = strings.TrimSpace(variants[0].Preheader)
	}
	if len(fromNames) > 0 {
		c.FromName = strings.TrimSpace(fromNames[0])
	}
	if c.Subject == "" || c.FromName == "" || strings.TrimSpace(c.HTML) == "" {
		return nil, fmt.Errorf("approved proof %s (%s) is incomplete (subject/from_name/html required)", c.ProofID, c.ProofName)
	}
	return &c, nil
}

// ── LIVE staging (mirrors stream_router.stage mechanics) ─────────────────────

type freshStageStats struct {
	segmentsCreated int
	membersAdded    int
	claimed         int
}

// ── REQ-118 fresh-broadcast fence ────────────────────────────────────────────

// errBroadcastFenced is returned by stage when the fence is armed and the batch
// would claim queue rows. The message names the section so an operator reading
// it in a log knows which switch produced it and what the replacement path is.
var errBroadcastFenced = errors.New("fresh-broadcast claims are fenced (REQ-118 §2.4): reserve through dripsupply")

// broadcastFenceEnabled reads DRIP_SUPPLY_BROADCAST_FENCE at CALL time, not at
// process start: the fence is an operator switch during the §7 cutover and must
// take effect on a task restart without a code change, and a cached value would
// make "did the fence apply?" depend on when the runner was constructed.
func broadcastFenceEnabled() bool {
	return strings.TrimSpace(os.Getenv("DRIP_SUPPLY_BROADCAST_FENCE")) == "1"
}

// assignmentClaimsQueueRows reports whether this batch would issue the direct
// partner_clean_queue claim below. Tag-sourced rows carry no QueueID and never
// reach that UPDATE, so fencing them would be a behaviour change the design
// does not ask for.
func assignmentClaimsQueueRows(assignment map[freshCell][]freshDrawRow) bool {
	for _, rows := range assignment {
		for _, row := range rows {
			if row.QueueID != "" {
				return true
			}
		}
	}
	return false
}

// stage creates the dated batch segments, hydrates queue rows to subscribers
// (python's exact insert shape incl. vertical_tag in tags), inserts members,
// and claims the queue rows. Uses a context detached from any HTTP request so
// a client disconnect cannot abandon a half-staged batch.
func (r *FreshBroadcastRunner) stage(reqCtx context.Context, orgID uuid.UUID, cfg freshStreamConfig, assignment map[freshCell][]freshDrawRow, date time.Time, token string) (freshStageStats, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	_ = reqCtx // deliberately not used for the write path (detached)

	var stats freshStageStats

	// REQ-118 §2.4 / §9.4: fresh broadcasts are FENCED — they may not claim
	// partner_clean_queue rows outside the reservation path. The check is here,
	// before the first write, so a fenced run leaves no half-staged segment
	// behind; and it fires only when this batch actually carries queue rows
	// (QueueID != ""), so the tag-sourced shape, which never claims, is
	// unaffected. Unset flag = old behaviour, byte for byte.
	if broadcastFenceEnabled() && assignmentClaimsQueueRows(assignment) {
		return stats, errBroadcastFenced
	}

	marker := freshBatchMarker(cfg.SegPrefix, token, date)

	// 1. segments (idempotent per batch token; deterministic uuid5 ids).
	segIDs := map[freshCell]string{}
	for cell, rows := range assignment {
		name := freshSegName(cfg.SegPrefix, token, cell.Site, cell.Lane)
		sid := freshSegID(name)
		var existing string
		err := r.db.QueryRowContext(ctx,
			`SELECT id::text FROM mailing_segments WHERE organization_id = $1 AND name = $2`,
			orgID, name).Scan(&existing)
		if err == nil {
			segIDs[cell] = existing
			continue
		}
		if err != sql.ErrNoRows {
			return stats, fmt.Errorf("segment lookup %s: %w", name, err)
		}
		repush := 0
		for _, row := range rows {
			if row.PushCount > 1 {
				repush++
			}
		}
		desc := fmt.Sprintf(
			"%s %s staged batch (%s) — fresh broadcast runner (Go, parity with agents/scheduling/stream_router.py). %d mailable records staged for %s/%s; %d of them re-pushed by the partner (fresh signal).",
			token, cfg.Label, marker, len(rows), cell.Site, cell.Lane, repush)
		if _, err := r.db.ExecContext(ctx, `
			INSERT INTO mailing_segments
				(id, organization_id, name, description, segment_type, status, subscriber_count)
			VALUES ($1, $2, $3, $4, 'static', 'active', 0)`,
			sid, orgID, name, desc); err != nil {
			return stats, fmt.Errorf("segment insert %s: %w", name, err)
		}
		segIDs[cell] = sid
		stats.segmentsCreated++
	}

	// 2. resolve/insert subscribers for rows not already linked (queue-source
	// rows can lack subscriber_id; tag-source rows always carry one).
	needEmails := map[string]bool{}
	for _, rows := range assignment {
		for _, row := range rows {
			if row.SubscriberID == "" && row.Email != "" {
				needEmails[strings.ToLower(row.Email)] = true
			}
		}
	}
	subIDForEmail := map[string]string{}
	if len(needEmails) > 0 {
		listID, err := r.verifiedImportsListID(ctx)
		if err != nil {
			return stats, err
		}
		all := make([]string, 0, len(needEmails))
		for e := range needEmails {
			all = append(all, e)
		}
		if err := r.lookupSubscriberIDs(ctx, all, subIDForEmail); err != nil {
			return stats, fmt.Errorf("subscriber lookup: %w", err)
		}
		// insert the missing ones in the python shape
		var newIDs, newEmails, newHashes []string
		for _, e := range all {
			if _, ok := subIDForEmail[e]; ok {
				continue
			}
			sum := md5.Sum([]byte(e))
			newIDs = append(newIDs, uuid.New().String())
			newEmails = append(newEmails, e)
			newHashes = append(newHashes, hex.EncodeToString(sum[:]))
		}
		tags := []string{strings.ToLower(cfg.SegPrefix) + "-" + strings.ToLower(token), marker}
		if cfg.VerticalTag != "" {
			tags = append(tags, cfg.VerticalTag)
		}
		for i := 0; i < len(newIDs); i += 1000 {
			end := i + 1000
			if end > len(newIDs) {
				end = len(newIDs)
			}
			if _, err := r.db.ExecContext(ctx, `
				INSERT INTO mailing_subscribers
					(id, organization_id, list_id, email, email_hash, status, source,
					 data_source, verification_status, eo_validated_at, tags)
				SELECT g.id::uuid, $1, $2::uuid, g.email, g.hash, 'confirmed', 'data_partner',
				       $3, 'verified', NOW(), $4::text[]
				FROM (SELECT unnest($5::text[]) AS id, unnest($6::text[]) AS email,
				             unnest($7::text[]) AS hash) g
				ON CONFLICT (list_id, email) DO NOTHING`,
				orgID, listID, "partner:"+marker, pq.Array(tags),
				pq.Array(newIDs[i:end]), pq.Array(newEmails[i:end]), pq.Array(newHashes[i:end])); err != nil {
				return stats, fmt.Errorf("subscriber hydrate: %w", err)
			}
		}
		if err := r.lookupSubscriberIDs(ctx, newEmails, subIDForEmail); err != nil {
			return stats, fmt.Errorf("subscriber re-lookup: %w", err)
		}
	}

	// 2b. vertical provenance on ALREADY-EXISTING subscribers being staged
	// (idempotent via the @> guard; COALESCE because tags can be NULL).
	if cfg.VerticalTag != "" {
		existingIDs := map[string]bool{}
		for _, rows := range assignment {
			for _, row := range rows {
				if row.SubscriberID != "" {
					existingIDs[row.SubscriberID] = true
				}
			}
		}
		for e, id := range subIDForEmail {
			_ = e
			existingIDs[id] = true
		}
		ids := make([]string, 0, len(existingIDs))
		for id := range existingIDs {
			ids = append(ids, id)
		}
		for i := 0; i < len(ids); i += 5000 {
			end := i + 5000
			if end > len(ids) {
				end = len(ids)
			}
			if _, err := r.db.ExecContext(ctx, `
				UPDATE mailing_subscribers
				SET tags = array_append(COALESCE(tags, '{}'), $1)
				WHERE id = ANY($2::uuid[])
				  AND NOT (COALESCE(tags, '{}') @> ARRAY[$1]::text[])`,
				cfg.VerticalTag, pq.Array(ids[i:end])); err != nil {
				return stats, fmt.Errorf("vertical tag append: %w", err)
			}
		}
	}

	// 3. members + queue claims (per cell, chunked).
	for cell, rows := range assignment {
		sid := segIDs[cell]
		var memIDs, memEmails []string
		var clmQIDs, clmSIDs, clmNotes []string
		for _, row := range rows {
			subID := row.SubscriberID
			if subID == "" {
				subID = subIDForEmail[strings.ToLower(row.Email)]
			}
			if subID == "" {
				continue // unresolvable — surfaced by member count vs fill
			}
			memIDs = append(memIDs, subID)
			memEmails = append(memEmails, row.Email)
			if row.QueueID != "" {
				clmQIDs = append(clmQIDs, row.QueueID)
				clmSIDs = append(clmSIDs, subID)
				clmNotes = append(clmNotes, marker+"-"+cell.Site)
			}
		}
		for i := 0; i < len(memIDs); i += 1000 {
			end := i + 1000
			if end > len(memIDs) {
				end = len(memIDs)
			}
			res, err := r.db.ExecContext(ctx, `
				INSERT INTO mailing_segment_members (segment_id, subscriber_id, email, materialized_at)
				SELECT $1::uuid, d.sid::uuid, d.email, NOW()
				FROM (SELECT unnest($2::text[]) AS sid, unnest($3::text[]) AS email) d
				ON CONFLICT (segment_id, subscriber_id) DO NOTHING`,
				sid, pq.Array(memIDs[i:end]), pq.Array(memEmails[i:end]))
			if err != nil {
				return stats, fmt.Errorf("members insert %s-%s: %w", cell.Site, cell.Lane, err)
			}
			n, _ := res.RowsAffected()
			stats.membersAdded += int(n)
		}
		for i := 0; i < len(clmQIDs); i += 1000 {
			end := i + 1000
			if end > len(clmQIDs) {
				end = len(clmQIDs)
			}
			if _, err := r.db.ExecContext(ctx, `
				UPDATE partner_clean_queue q
				SET status = 'claimed', claimed_at = NOW(), subscriber_id = d.sid::uuid,
				    extra_metadata = COALESCE(q.extra_metadata, '{}'::jsonb)
				                     || jsonb_build_object('claim_note', d.note)
				FROM (SELECT unnest($1::text[]) AS qid, unnest($2::text[]) AS sid,
				             unnest($3::text[]) AS note) d
				WHERE q.id = d.qid::uuid`,
				pq.Array(clmQIDs[i:end]), pq.Array(clmSIDs[i:end]), pq.Array(clmNotes[i:end])); err != nil {
				return stats, fmt.Errorf("queue claim %s-%s: %w", cell.Site, cell.Lane, err)
			}
			stats.claimed += end - i
		}
	}

	// 4. refresh cached counts.
	for _, sid := range segIDs {
		if _, err := r.db.ExecContext(ctx, `
			UPDATE mailing_segments SET subscriber_count =
				(SELECT count(*) FROM mailing_segment_members WHERE segment_id = $1),
				last_count_at = NOW()
			WHERE id = $1`, sid); err != nil {
			log.Printf("[FreshBroadcast] segment count refresh %s failed (non-fatal): %v", sid, err)
		}
	}
	return stats, nil
}

// verifiedImportsListID resolves the standing hydration list (python's
// `mailing_lists.name ~* 'Verified External Imports'` lookup).
func (r *FreshBroadcastRunner) verifiedImportsListID(ctx context.Context) (string, error) {
	var id string
	err := r.db.QueryRowContext(ctx, `
		SELECT id::text FROM mailing_lists
		WHERE name ~* 'Verified External Imports'
		ORDER BY name LIMIT 1`).Scan(&id)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("no 'Verified External Imports' list — hydration target missing, REFUSE")
	}
	return id, err
}

// lookupSubscriberIDs batch-resolves lower(email) → id into out.
func (r *FreshBroadcastRunner) lookupSubscriberIDs(ctx context.Context, emails []string, out map[string]string) error {
	for i := 0; i < len(emails); i += 5000 {
		end := i + 5000
		if end > len(emails) {
			end = len(emails)
		}
		rows, err := r.db.QueryContext(ctx, `
			SELECT lower(email), id::text FROM mailing_subscribers
			WHERE lower(email) = ANY($1::text[])`, pq.Array(emails[i:end]))
		if err != nil {
			return err
		}
		for rows.Next() {
			var e, id string
			if err := rows.Scan(&e, &id); err != nil {
				rows.Close()
				return err
			}
			out[e] = id
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return nil
}

// ── Draft campaigns (one per stream × destination, via the stage flow) ───────

// freshDestination is one resolved destination lane.
type freshDestination struct {
	Site         string
	Label        string // campaign-name label (brand code / explicit site code)
	BrandLabel   string // publication label for the FF transform
	Domain       string
	ProfileID    string
	ExplicitLane bool
}

// buildFreshCampaignInput assembles the draft's PMTACampaignInput. Pure —
// unit-tested for the draft-only invariants (send_mode scheduled, time-span
// source "duration-calc" — footgun #1, content locked, master selection off).
func buildFreshCampaignInput(cfg freshStreamConfig, dest freshDestination, offerID string,
	copySrc *freshApprovedCopy, cells map[freshCell][]freshDrawRow, date time.Time) (engine.PMTACampaignInput, int) {

	start := time.Date(date.Year(), date.Month(), date.Day(), freshBroadcastStartUTCHour, 0, 0, 0, time.UTC)
	throttle := cfg.ThrottleHours
	if throttle <= 0 {
		throttle = 12
	}
	end := start.Add(time.Duration(throttle) * time.Hour)

	// Per-lane quotas + inclusion segments for THIS site's cells.
	laneCount := map[string]int{}
	var segIDsList []string
	token := freshBatchToken(date)
	for cell, rows := range cells {
		if cell.Site != dest.Site || len(rows) == 0 {
			continue
		}
		laneCount[strings.ToLower(cell.Lane)] += len(rows)
		segIDsList = append(segIDsList, freshSegID(freshSegName(cfg.SegPrefix, token, cell.Site, cell.Lane)))
	}
	total := 0
	var quotas []engine.ISPQuota
	var plans []engine.PMTAISPScheduleInput
	var targets []engine.ISP
	for lane, n := range laneCount {
		total += n
		quotas = append(quotas, engine.ISPQuota{ISP: lane, Volume: n})
		targets = append(targets, engine.ISP(lane))
		st, en := start, end
		plans = append(plans, engine.PMTAISPScheduleInput{
			ISP: lane, Quota: n,
			ThrottleStrategy: "gentle",
			Timezone:         freshBroadcastTimezone,
			Cadence:          engine.PMTACadenceInput{Mode: "interval", EveryMinutes: 15, BatchSize: 0},
			TimeSpans: []engine.PMTATimeSpanInput{{
				Type: "absolute", StartAt: &st, EndAt: &en,
				Timezone: freshBroadcastTimezone,
				// "duration-calc" — any other literal trips the server's
				// volume-based wave-sanity check and silently fails the
				// campaign (CLAUDE.md §6 footgun #1).
				Source: "duration-calc",
			}},
		})
	}

	locked := true
	masterOff := false
	name := freshDateToken(date) + " - " + dest.Label + " - FRESH-BCAST-" + cfg.SegPrefix + " - " + cfg.Offer
	input := engine.PMTACampaignInput{
		Name:             name,
		OfferID:          offerID,
		SendingDomain:    dest.Domain,
		SendingProfileID: dest.ProfileID,
		Variants: []engine.ContentVariant{{
			VariantName:  "A",
			FromName:     freshFromName(dest.BrandLabel, copySrc.FromName, dest.ExplicitLane),
			Subject:      copySrc.Subject,
			PreviewText:  copySrc.Preheader,
			HTMLContent:  copySrc.HTML,
			PlainContent: "",
			SplitPercent: 100,
		}},
		TargetISPs:         targets,
		InclusionSegments:  segIDsList,
		ExclusionLists:     []string{freshBroadcastExclusionList},
		SendPriority:       []engine.PriorityItem{},
		Timezone:           freshBroadcastTimezone,
		ThrottleStrategy:   "gentle",
		ISPQuotas:          quotas,
		ISPPlans:           plans,
		SendMode:           "scheduled",
		ScheduledAt:        &start,
		MinRemailHours:     0,
		UseMasterSelection: &masterOff,
		ContentLocked:      &locked,
	}
	return input, total
}

// resolveBrandProfile pins the m.<apex> SES tenant profile for a brand cell
// (SES is never a default route — the profile must resolve or the destination
// is refused; the python compiler's esp=ses rule).
func (r *FreshBroadcastRunner) resolveBrandProfile(ctx context.Context, orgID uuid.UUID, domain string) (string, error) {
	var id string
	err := r.db.QueryRowContext(ctx, `
		SELECT id::text FROM mailing_sending_profiles
		WHERE organization_id = $1 AND vendor_type = 'pmta' AND status = 'active'
		  AND (sending_domain = $2 OR from_email LIKE '%@' || $2)
		ORDER BY is_default DESC, created_at DESC LIMIT 1`, orgID, domain).Scan(&id)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("no active sending profile for %s", domain)
	}
	return id, err
}

// buildCampaigns emits one draft per (stream × destination site) with rows.
// dry=true plans (names + volumes) without writing anything.
func (r *FreshBroadcastRunner) buildCampaigns(ctx context.Context, orgID uuid.UUID, cfg freshStreamConfig,
	offer *resolvedOffer, copySrc *freshApprovedCopy, assignment map[freshCell][]freshDrawRow,
	date time.Time, dry bool) []FreshCampaignResult {

	// Distinct sites with volume, stable order: primaries first, explicit last.
	explicitSite := ""
	if cfg.SendingDomain != "" {
		explicitSite = freshExplicitSiteCode(cfg.SendingDomain)
	}
	siteHas := map[string]bool{}
	for cell, rows := range assignment {
		if len(rows) > 0 {
			siteHas[cell.Site] = true
		}
	}
	var order []string
	for _, s := range cfg.PrimarySites {
		if siteHas[s] {
			order = append(order, s)
		}
	}
	if explicitSite != "" && siteHas[explicitSite] {
		order = append(order, explicitSite)
	}

	out := make([]FreshCampaignResult, 0, len(order))
	for _, site := range order {
		var dest freshDestination
		if site == explicitSite && explicitSite != "" {
			if cfg.SendingProfileID == "" {
				out = append(out, FreshCampaignResult{Site: site, Status: "refused",
					Reason: "explicit lane requires sending_profile_id — REFUSE"})
				continue
			}
			dest = freshDestination{Site: site, Label: site, BrandLabel: site,
				Domain: cfg.SendingDomain, ProfileID: cfg.SendingProfileID, ExplicitLane: true}
		} else {
			apex, ok := freshBrandApex[site]
			if !ok {
				out = append(out, FreshCampaignResult{Site: site, Status: "refused",
					Reason: fmt.Sprintf("site %q not in the brand registry — REFUSE", site)})
				continue
			}
			domain := "m." + apex
			profileID, err := r.resolveBrandProfile(ctx, orgID, domain)
			if err != nil {
				out = append(out, FreshCampaignResult{Site: site, SendingDomain: domain,
					Status: "refused", Reason: "resolve profile: " + err.Error()})
				continue
			}
			dest = freshDestination{Site: site, Label: site, BrandLabel: freshBrandLabel[site],
				Domain: domain, ProfileID: profileID, ExplicitLane: false}
		}

		input, total := buildFreshCampaignInput(cfg, dest, offer.ID, copySrc, assignment, date)
		cr := FreshCampaignResult{
			Name: input.Name, Site: site, SendingDomain: dest.Domain, Planned: total,
		}
		if dry {
			cr.Status = "planned"
			out = append(out, cr)
			continue
		}
		res, err := stagePMTADraftCampaign(ctx, r.db, orgID.String(),
			engine.PMTACampaignDraftInput{CampaignInput: input, ScheduleMode: "quick"}, r.colCache)
		if err != nil {
			cr.Status = "refused"
			cr.Reason = "stage draft: " + err.Error()
			out = append(out, cr)
			continue
		}
		cr.CampaignID = res.CampaignID
		cr.Status = res.Status // 'draft' — the stage flow never deploys
		out = append(out, cr)
	}
	return out
}

// ── Runs table (mailing_fresh_broadcast_runs) ────────────────────────────────

func (r *FreshBroadcastRunner) insertRun(ctx context.Context, orgID uuid.UUID, res FreshRunResult) (string, error) {
	id := uuid.New().String()
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO mailing_fresh_broadcast_runs
			(id, organization_id, run_date, dry, trigger_source, results, status)
		VALUES ($1, $2, $3, $4, $5, '{}'::jsonb, 'running')`,
		id, orgID, res.Date, res.Dry, res.Trigger)
	return id, err
}

func (r *FreshBroadcastRunner) finishRun(ctx context.Context, runID string, res FreshRunResult) {
	payload, err := json.Marshal(res)
	if err != nil {
		payload = []byte(`{}`)
	}
	uctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := r.db.ExecContext(uctx, `
		UPDATE mailing_fresh_broadcast_runs
		SET results = $2::jsonb, status = $3
		WHERE id = $1`, runID, string(payload), res.Status); err != nil {
		log.Printf("[FreshBroadcast] run %s result write failed: %v", runID, err)
	}
}

// RunForWorker is the injection point for worker.FreshBroadcastWorker (the
// worker package cannot import internal/api — same inversion as
// WrapPMTACampaignDeploy). It runs every org that has stream config rows.
func (r *FreshBroadcastRunner) RunForWorker(ctx context.Context, date string, dry, autoStageOnly bool, trigger string) error {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT organization_id FROM mailing_stream_broadcast_config`)
	if err != nil {
		return err
	}
	var orgs []uuid.UUID
	for rows.Next() {
		var o uuid.UUID
		if err := rows.Scan(&o); err != nil {
			rows.Close()
			return err
		}
		orgs = append(orgs, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var firstErr error
	for _, org := range orgs {
		res, err := r.Run(ctx, org, FreshRunOptions{
			Date: date, Dry: dry, Trigger: trigger, AutoStageOnly: autoStageOnly})
		if err != nil && firstErr == nil {
			firstErr = err
		}
		log.Printf("[FreshBroadcast] worker run org=%s dry=%v auto_stage_only=%v status=%s streams=%d",
			org, dry, autoStageOnly, res.Status, len(res.Streams))
	}
	return firstErr
}
