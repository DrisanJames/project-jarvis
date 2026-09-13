package api

// campaign_performance — the platform's first MAINTAINED analytics capability
// (2026-09-12). One read-only surface that computes per-campaign performance
// exactly the way docs/METRIC_CONTRACT.md §1, §2, §2.1, §3, §4, §12 say to, so
// no screen has to hand-roll the rules again.
//
//	GET /api/mailing/campaign-performance?ids=<uuid,...>            (≤50)
//	GET /api/mailing/campaign-performance?name=<substr>&from=&to=   (Denver days)
//	    [&breakdown=isp]
//
// Where each number comes from (§1):
//   - DELIVERY  → the Athena lake via analytics.Breakdown, source IN
//     (pmta, ses, kumo), COUNT(DISTINCT event_uid) (§2), campaign-scoped.
//     Grouping by the reader's `event_type` dimension applies eventTypeExpr
//     (reader.go), which folds BOTH bounce spellings — PMTA/kumo
//     hard_bounce/soft_bounce and SES raw `bounced`+bounce_cat — into the §3
//     taxonomy at read time. reputation_block / administrative /
//     preflight_validation are counted in NEITHER hard nor soft nor attempted.
//     `relayed_to_ses` is a hop, never delivered. Attempted is DERIVED (§2).
//   - ENGAGEMENT → PG mailing_tracking_events, one grouped scan per id list,
//     partition-pruned on raw event_at (§4). Two bases side by side (§12):
//     INCLUSIVE (every event, no filter — the operating basis, §12.1) and
//     HUMAN (ignite_verdict_is_human(ignite_event_verdict(user_agent,
//     ip_address)) on opened/clicked rows only, §6). is_machine_open /
//     is_machine_click are LABELS, never filters (§12.1/§12.5) — not used.
//     `sent` is the worker row only (substring(id::text,15,1)='4', §2.1).
//   - CONVERSIONS → PG mailing_everflow_conversions by campaign_id (the
//     postback's sub3), counted UNJOINED (CLAUDE.md §11).
//
// Fail-soft: when the lake reader is disabled (or a lake query fails) the PG
// half is still returned and `delivery` carries an `unavailable` reason; every
// rate whose denominator is delivered/attempted is then null with a reason
// (§2: never divide metrics of different scope or availability).
//
// No writes anywhere. Routes are registered by the caller (server wiring).

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/analytics"
	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// VersionCampaignPerformance is bumped on every behaviour change (testing.mdc).
//
//	1.0 (2026-09-12): initial — contract-compliant per-campaign delivery (lake),
//	    engagement (PG, inclusive + human), conversions (unjoined), disclosed
//	    rates, maturity, optional ISP breakdown.
const VersionCampaignPerformance = "1.0"

// campaignPerformanceContractVersion is stamped on every response so a consumer
// can tell which revision of the contract produced the numbers.
const campaignPerformanceContractVersion = "METRIC_CONTRACT 2026-09-12"

const (
	// maxCampaignPerformanceIDs bounds both input modes (ids list and name
	// resolution) so one request is one bounded PG scan + one lake query.
	maxCampaignPerformanceIDs = 50
	// campaignPerformanceLakeChunk mirrors analytics.maxBreakdownCampaignIDs
	// (reader.go:148, unexported) — the reader rejects a longer IN-list.
	campaignPerformanceLakeChunk = 2000
	// campaignPerformanceTimeout bounds the whole request (apex-alb idle
	// timeout is 300s, verified 2026-09-13). The engagement half is loaded ONE
	// CAMPAIGN PER STATEMENT, campaignPerformanceEngagementWorkers at a time,
	// each under campaignPerformanceStmtTimeout: measured on prod 2026-09-13,
	// one settled board campaign (32k sent / 54k opens / 661 clicks) takes
	// ~26s on the (campaign_id, event_at) index — five campaigns in one
	// statement blew the pool's 30s statement_timeout (main.go DSN options).
	// SET LOCAL overrides that per transaction; splitting bounds each statement
	// by one campaign's volume and parallelises the heap fetches.
	campaignPerformanceTimeout           = 240 * time.Second
	campaignPerformanceStmtTimeout       = "120s"
	campaignPerformanceEngagementWorkers = 4
	// campaignPerformanceMaturityDays — §12.3: a cohort number read with less
	// than ~3 days of tail is an undercount, not a discrepancy.
	campaignPerformanceMaturityDays = 3.0
	// campaignPerformanceISPUnresolved labels PG engagement rows whose
	// recipient_domain is NULL (subscriber row missing at ingest —
	// internal/tracking/consumer.go). Never folded into 'other'.
	campaignPerformanceISPUnresolved = "unresolved_subscriber"
)

// ── click-quality predicate (mirrors agents/dbknowledge/_db.py byte-for-byte) ──
//
// PG_CLICK_ASSET / PG_CLICK_NONNAV / PG_CLICK_ACTION. An asset/resource fetch
// is not a click; unsub/pref/compliance and synthetic markers are not clicks;
// an unresolved t.em tracker self-reference is not a click UNLESS it is the
// /o/ scanner-safe money-link redirect (proven navigation, 2026-09-01).
const pgClickAssetSQL = `(link_url ~* '\.(css|js|woff2?|ttf|otf|eot|png|jpe?g|gif|svg|ico|webp|map)([?#]|$)'` +
	` OR link_url ~* '(fonts\.g|cdn\.|cloudfront|akamai|fastly|jsdelivr|unpkg|gstatic)')`

const pgClickNonNavSQL = `(COALESCE(link_url,'') = ''` +
	` OR link_url ~* 'unsub|optout|opt-out|preference|/privacy'` +
	` OR link_url ~* '^everflow-import:'` +
	` OR (link_url ~* '^https?://t\.em\.' AND link_url !~* '^https?://(t|trk)\.e?m\.[^/]+/o/'))`

const pgClickActionSQL = "(event_type = 'clicked' AND NOT " + pgClickNonNavSQL + " AND NOT " + pgClickAssetSQL + ")"

// ── SQL ──────────────────────────────────────────────────────────────────────

// campaignPerformanceEngagementSQL renders the ONE grouped PG scan. The verdict
// is evaluated on opened/clicked rows only: 'sent' rows carry no UA/IP and
// would classify 'machine-bare' (agents/dbknowledge/_db.py HUMAN_VERDICT_PG).
// The raw event_at >= / < bounds are what prune the monthly partitions (§4).
// $1 org, $2 uuid[] campaign ids, $3/$4 event_at bounds.
func campaignPerformanceEngagementSQL(byISP bool) string {
	key := "campaign_id::text"
	group := "campaign_id"
	if byISP {
		key = "campaign_id::text, COALESCE(recipient_domain,'')"
		group = "campaign_id, COALESCE(recipient_domain,'')"
	}
	return `
		WITH ev AS (
			SELECT campaign_id, id, event_type, subscriber_id, link_url, recipient_domain,
			       CASE WHEN event_type IN ('opened','clicked')
			            THEN ignite_verdict_is_human(ignite_event_verdict(user_agent, ip_address))
			            ELSE false END AS is_human
			FROM mailing_tracking_events
			WHERE organization_id = $1
			  AND campaign_id = ANY($2::uuid[])
			  AND event_at >= $3 AND event_at < $4
			  AND event_type IN ('sent','opened','clicked')
		)
		SELECT ` + key + `,
			COUNT(*) FILTER (WHERE event_type = 'sent' AND substring(id::text,15,1) = '4'),
			COUNT(*) FILTER (WHERE event_type = 'sent'),
			COUNT(*) FILTER (WHERE event_type = 'opened'),
			COUNT(*) FILTER (WHERE event_type = 'clicked'),
			COUNT(DISTINCT subscriber_id) FILTER (WHERE event_type = 'opened'),
			COUNT(DISTINCT subscriber_id) FILTER (WHERE event_type = 'clicked'),
			COUNT(*) FILTER (WHERE ` + pgClickActionSQL + `),
			COUNT(*) FILTER (WHERE event_type = 'opened' AND is_human),
			COUNT(*) FILTER (WHERE event_type = 'clicked' AND is_human),
			COUNT(DISTINCT subscriber_id) FILTER (WHERE event_type = 'opened' AND is_human),
			COUNT(DISTINCT subscriber_id) FILTER (WHERE event_type = 'clicked' AND is_human),
			COUNT(*) FILTER (WHERE ` + pgClickActionSQL + ` AND is_human)
		FROM ev
		GROUP BY ` + group
}

// campaignPerformanceConversionsSQL counts conversions UNJOINED (CLAUDE.md
// §11: joining mailing_offers fans one conversion ×N). Only rows whose
// postback carried sub3 (→ campaign_id) are campaign-attributed.
const campaignPerformanceConversionsSQL = `
		SELECT campaign_id::text, COUNT(*), COALESCE(SUM(payout),0)::float8
		FROM mailing_everflow_conversions
		WHERE organization_id = $1
		  AND campaign_id = ANY($2::uuid[])
		GROUP BY campaign_id`

// campaignPerformanceCampaignSQL resolves the campaign set. Sending domain and
// transport live on the PROFILE (day_cards.go); the campaign row supplies
// name/status/scheduled_at. `where` is one of the two mode clauses below.
const campaignPerformanceCampaignSQL = `
		SELECT c.id::text, COALESCE(c.name,''), COALESCE(c.status,''), c.scheduled_at, c.created_at,
		       COALESCE(sp.sending_domain,''),
		       CASE WHEN LOWER(COALESCE(sp.routing_mode,'')) = 'kumo' THEN 'kumo'
		            WHEN COALESCE(sp.via_ses, FALSE) THEN 'ses'
		            ELSE 'pmta' END AS transport
		  FROM mailing_campaigns c
		  LEFT JOIN mailing_sending_profiles sp ON sp.id = c.sending_profile_id
		 WHERE c.organization_id = $1
		   AND `

const (
	campaignPerformanceWhereIDs  = `c.id = ANY($2::uuid[])`
	campaignPerformanceWhereName = `c.name ILIKE '%' || $2 || '%' AND c.scheduled_at >= $3 AND c.scheduled_at < $4`
	campaignPerformanceOrder     = ` ORDER BY c.scheduled_at DESC NULLS LAST, c.name ASC LIMIT `
)

// ── service ──────────────────────────────────────────────────────────────────

// lakeBreakdownFn is the lake seam: production binds analytics.Breakdown, tests
// inject a fake through SetLakeBreakdown.
type lakeBreakdownFn func(ctx context.Context, f analytics.BreakdownFilter) ([]analytics.BreakdownRow, error)

// CampaignPerformanceService serves GET /campaign-performance. Stateless
// beyond its DB handle and the two injectable seams.
type CampaignPerformanceService struct {
	db          *sql.DB
	lake        lakeBreakdownFn
	lakeEnabled func() bool
	now         func() time.Time
	// engWorkers bounds concurrent per-campaign engagement statements (tests
	// set 1 so sqlmock expectations stay ordered).
	engWorkers int
}

// NewCampaignPerformanceService builds the service bound to the global lake
// reader (analytics.Breakdown / analytics.ReaderEnabled).
func NewCampaignPerformanceService(db *sql.DB) *CampaignPerformanceService {
	return &CampaignPerformanceService{
		db:          db,
		lake:        analytics.Breakdown,
		lakeEnabled: analytics.ReaderEnabled,
		now:         time.Now,
		engWorkers:  campaignPerformanceEngagementWorkers,
	}
}

// SetLakeBreakdown replaces the lake seam. A nil fn models "lake reader
// disabled" (the fail-soft path); a non-nil fn is treated as enabled.
func (s *CampaignPerformanceService) SetLakeBreakdown(fn lakeBreakdownFn) {
	s.lake = fn
	enabled := fn != nil
	s.lakeEnabled = func() bool { return enabled }
}

// RegisterRoutes mounts the service under the /api/mailing group (pattern:
// drip_supply_handlers.go), so the final URL is /api/mailing/campaign-performance.
func (s *CampaignPerformanceService) RegisterRoutes(r chi.Router) {
	r.Route("/campaign-performance", func(cr chi.Router) {
		cr.Get("/", s.HandleGet)
	})
}

// ── wire types ───────────────────────────────────────────────────────────────

type cpCampaignMeta struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	SendingDomain string  `json:"sending_domain"`
	Brand         string  `json:"brand"`
	Status        string  `json:"status"`
	ScheduledAt   *string `json:"scheduled_at"` // Denver RFC3339
	Transport     string  `json:"transport"`    // pmta | ses | kumo (routing_mode / via_ses)
}

type cpDelivery struct {
	Delivered        int64            `json:"delivered"`
	Hard             int64            `json:"hard"`
	Soft             int64            `json:"soft"`
	Complaint        int64            `json:"complaint"`
	AttemptedDerived int64            `json:"attempted_derived"`
	Source           string           `json:"source"`
	SourcesSeen      []string         `json:"sources_seen"`
	Excluded         map[string]int64 `json:"excluded"` // hops / non-attempt classes, disclosed not summed
}

type cpEngagementBase struct {
	Opens          int64 `json:"opens"`
	Clicks         int64 `json:"clicks"`
	UniqueOpeners  int64 `json:"unique_openers"`
	UniqueClickers int64 `json:"unique_clickers"`
	ActionClicks   int64 `json:"action_clicks"`
}

type cpEngagement struct {
	SentPG           int64            `json:"sent_pg"`
	SentPGAllWriters int64            `json:"sent_pg_all_writers"`
	Inclusive        cpEngagementBase `json:"inclusive"`
	Human            cpEngagementBase `json:"human"`
	Source           string           `json:"source"`
}

type cpConversions struct {
	Conversions int64   `json:"conversions"`
	Payout      float64 `json:"payout"`
	Source      string  `json:"source"`
	Counting    string  `json:"counting"`
}

type cpRate struct {
	Value           *float64 `json:"value"` // fraction 0..1, rounded to 6 places; null when not computable
	Numerator       int64    `json:"numerator"`
	NumeratorName   string   `json:"numerator_name"`
	DenominatorName string   `json:"denominator_name"`
	Denominator     *int64   `json:"denominator"`
	Reason          string   `json:"reason,omitempty"`
}

type cpMaturity struct {
	DaysSinceScheduled *float64 `json:"days_since_scheduled"`
	Immature           bool     `json:"immature"`
	ThresholdDays      float64  `json:"threshold_days"`
}

type cpISPRow struct {
	ISP        string            `json:"isp"`
	Delivery   interface{}       `json:"delivery"`
	Engagement cpEngagement      `json:"engagement"`
	Rates      map[string]cpRate `json:"rates"`
}

type cpEntry struct {
	Campaign    cpCampaignMeta    `json:"campaign"`
	Delivery    interface{}       `json:"delivery"` // *cpDelivery or {"unavailable": reason}
	Engagement  cpEngagement      `json:"engagement"`
	Conversions *cpConversions    `json:"conversions,omitempty"`
	Rates       map[string]cpRate `json:"rates"`
	Maturity    cpMaturity        `json:"maturity"`
	ISP         []cpISPRow        `json:"isp,omitempty"`
	Notes       []string          `json:"notes,omitempty"`
}

type cpWindow struct {
	From          string `json:"from"` // Denver YYYY-MM-DD
	To            string `json:"to"`
	Timezone      string `json:"timezone"`
	Basis         string `json:"basis"`
	EventScanFrom string `json:"event_scan_from"` // Denver RFC3339 — the PG/lake bounds actually used
	EventScanTo   string `json:"event_scan_to"`
}

type cpResponse struct {
	AsOf            string            `json:"as_of"`
	Version         string            `json:"version"`
	ContractVersion string            `json:"contract_version"`
	Window          cpWindow          `json:"window"`
	Breakdown       string            `json:"breakdown,omitempty"`
	Truncated       bool              `json:"truncated,omitempty"`
	Campaigns       []cpEntry         `json:"campaigns"`
	Definitions     map[string]string `json:"definitions"`
	Notes           []string          `json:"notes,omitempty"`
}

// campaignPerformanceDefinitions — one line per metric naming its § in the
// contract. Emitted verbatim on every response.
var campaignPerformanceDefinitions = map[string]string{
	"delivery.delivered":             "lake COUNT(DISTINCT event_uid) event_type='delivered', source IN (pmta,ses,kumo), campaign-scoped — §1, §2",
	"delivery.hard":                  "lake reclassified hard_bounce = bounce_cat IN (hard,bad-mailbox,bad-domain,inactive-mailbox); PMTA/kumo hard_bounce and SES bounced+bounce_cat both fold here — §3",
	"delivery.soft":                  "lake reclassified soft_bounce (ELSE arm); reputation_block / administrative / preflight_validation are NOT soft — §3",
	"delivery.complaint":             "lake event_type='complaint' — §1; read at T-14 for a final figure — §12.3",
	"delivery.attempted_derived":     "delivered + hard + soft + untyped bounced (DERIVED; raw attempted exists on SES only) — §2",
	"delivery.excluded":              "relayed_to_ses is a PMTA→SES hop, not delivered; reputation_block / administrative / preflight_validation are counted in neither hard nor soft nor attempted — §3",
	"engagement.sent_pg":             "PG event_type='sent' worker rows only: substring(id::text,15,1)='4' — §2.1 read-around for the 2026-06-05→2026-09-01 double-count",
	"engagement.sent_pg_all_writers": "PG event_type='sent' all writers (inflated up to 2× on SES lanes inside the §2.1 window) — disclosure only",
	"engagement.inclusive":           "every open/click EVENT (opens, clicks) and DISTINCT subscriber_id (unique_openers, unique_clickers), no filter — the operating basis — §12, §12.1",
	"engagement.human":               "same counts restricted to ignite_verdict_is_human(ignite_event_verdict(user_agent, ip_address)) on opened/clicked rows only — a reporting lens, never an audience filter — §1, §6, §12.1",
	"engagement.action_clicks":       "PG_CLICK_ACTION (agents/dbknowledge/_db.py): clicked AND NOT nonnav AND NOT asset; /o/ redirect exempt — §6 machine-click URL rule",
	"conversions":                    "mailing_everflow_conversions rows with campaign_id (postback sub3), COUNT(*) + SUM(payout), UNJOINED — CLAUDE.md §11",
	"rates":                          "every rate discloses numerator and denominator; delivery/hard/soft/complaint ÷ attempted_derived; open/click ÷ delivered; ctor ÷ unique_openers; null with reason when scope or availability differs — §2",
	"maturity":                       "days since scheduled_at; immature when < 3 days (send-cohort tail settles over ~3 days, ISP-shaped) — §12.3",
	"isp":                            "delivery ISP = lake clean classifier (ispExpr, real recipient domain); engagement ISP = isp.GroupFromDomain(recipient_domain); NULL recipient_domain → unresolved_subscriber — §5",
}

// ── handler ──────────────────────────────────────────────────────────────────

type cpCampaignRow struct {
	ID          string
	Name        string
	Status      string
	ScheduledAt sql.NullTime
	CreatedAt   sql.NullTime
	Domain      string
	Transport   string
}

// HandleGet serves GET /campaign-performance.
func (s *CampaignPerformanceService) HandleGet(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	q := r.URL.Query()
	breakdown := strings.ToLower(strings.TrimSpace(q.Get("breakdown")))
	if breakdown != "" && breakdown != "isp" {
		respondError(w, http.StatusBadRequest, "breakdown must be 'isp' when set")
		return
	}
	byISP := breakdown == "isp"

	ids, err := parseCampaignPerformanceIDs(q.Get("ids"))
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(q.Get("name"))
	if len(ids) == 0 && name == "" {
		respondError(w, http.StatusBadRequest, "ids (comma-separated UUIDs, max 50) or name (+ from/to) is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), campaignPerformanceTimeout)
	defer cancel()
	now := s.now()

	// 1) Resolve the campaign set (org-scoped).
	var (
		rows      []cpCampaignRow
		truncated bool
		window    cpWindow
	)
	if len(ids) > 0 {
		rows, truncated, err = s.loadCampaigns(ctx, orgID, campaignPerformanceWhereIDs, pq.Array(ids))
		window.Basis = "ids: event scan bounded by min(scheduled_at)-1d → now (campaign life)"
	} else {
		from, to, fromDt, toDt, perr := parseAlignmentRange(r)
		if perr != nil {
			respondError(w, http.StatusBadRequest, perr.Error())
			return
		}
		rows, truncated, err = s.loadCampaigns(ctx, orgID, campaignPerformanceWhereName,
			escapeLikeLiteral(name), from, to.Add(time.Second))
		window.From, window.To = fromDt, toDt
		window.Basis = "name: campaigns with scheduled_at inside the Denver day range; event scan bounded by min(scheduled_at)-1d → now"
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("campaign resolution: %v", err))
		return
	}
	window.Timezone = "America/Denver"

	resp := cpResponse{
		AsOf:            now.UTC().Format(time.RFC3339),
		Version:         VersionCampaignPerformance,
		ContractVersion: campaignPerformanceContractVersion,
		Breakdown:       breakdown,
		Truncated:       truncated,
		Campaigns:       []cpEntry{},
		Definitions:     campaignPerformanceDefinitions,
	}
	if truncated {
		resp.Notes = append(resp.Notes, fmt.Sprintf("more than %d campaigns matched; only the newest %d are returned", maxCampaignPerformanceIDs, maxCampaignPerformanceIDs))
	}
	if len(rows) == 0 {
		resp.Window = window
		respondJSON(w, http.StatusOK, resp)
		return
	}

	// 2) Event-scan bounds (§4: raw event_at bounds so partitions prune).
	scanFrom, scanTo := campaignPerformanceScanBounds(rows, now)
	window.EventScanFrom = scanFrom.In(mstLoc).Format(time.RFC3339)
	window.EventScanTo = scanTo.In(mstLoc).Format(time.RFC3339)
	if window.From == "" {
		window.From = scanFrom.In(mstLoc).Format("2006-01-02")
		window.To = scanTo.In(mstLoc).Format("2006-01-02")
	}
	resp.Window = window

	campaignIDs := make([]string, 0, len(rows))
	for _, c := range rows {
		campaignIDs = append(campaignIDs, c.ID)
	}

	// 3) PG engagement — one grouped scan.
	eng, err := s.loadEngagement(ctx, orgID, campaignIDs, scanFrom, scanTo, byISP)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("engagement query: %v", err))
		return
	}

	// 4) PG conversions — unjoined.
	conv, err := s.loadConversions(ctx, orgID, campaignIDs)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("conversions query: %v", err))
		return
	}

	// 5) Lake delivery — fail-soft.
	lakeDt := func(t time.Time) string { return t.UTC().Format("2006-01-02") }
	del, delISP, lakeReason := s.loadDelivery(ctx, campaignIDs, lakeDt(scanFrom), lakeDt(scanTo), byISP)
	if lakeReason != "" {
		resp.Notes = append(resp.Notes, "delivery unavailable: "+lakeReason)
	}

	// 6) Assemble.
	for _, c := range rows {
		entry := cpEntry{
			Campaign: cpCampaignMeta{
				ID: c.ID, Name: c.Name, SendingDomain: c.Domain,
				Brand: brand.Root(c.Domain), Status: c.Status, Transport: c.Transport,
			},
			Maturity: campaignPerformanceMaturity(c.ScheduledAt, now),
		}
		if c.ScheduledAt.Valid {
			sa := c.ScheduledAt.Time.In(mstLoc).Format(time.RFC3339)
			entry.Campaign.ScheduledAt = &sa
		}
		e := eng[c.ID]
		e.Source = "pg"
		entry.Engagement = e

		var d *cpDelivery
		if lakeReason != "" {
			entry.Delivery = map[string]string{"unavailable": lakeReason}
		} else {
			d = del[c.ID]
			if d == nil {
				d = newCPDelivery()
			}
			entry.Delivery = d
		}
		entry.Rates = campaignPerformanceRates(d, e, lakeReason)

		if cv, ok := conv[c.ID]; ok {
			entry.Conversions = &cpConversions{
				Conversions: cv.n, Payout: math.Round(cv.payout*100) / 100,
				Source: "pg:mailing_everflow_conversions", Counting: "unjoined",
			}
		}
		entry.Notes = append(entry.Notes, "conversions: counted UNJOINED from mailing_everflow_conversions by campaign_id (postback sub3); a postback without sub3 is not campaign-attributed and is not counted here")
		if c.Transport == "kumo" {
			entry.Notes = append(entry.Notes, "kumo: the lake carries no sent/open/click rows for source='kumo' (CLAUDE.md §5.4) — engagement here is PG mailing_tracking_events; delivery is lake terminal outcomes (delivered + hard + soft), delivery_delay excluded")
		}
		if lakeReason != "" {
			entry.Notes = append(entry.Notes, "delivery unavailable ("+lakeReason+"): rates over delivered/attempted are null")
		}

		if byISP {
			entry.ISP = campaignPerformanceISPRows(delISP[c.ID], eng, c.ID, lakeReason)
		}
		resp.Campaigns = append(resp.Campaigns, entry)
	}
	respondJSON(w, http.StatusOK, resp)
}

// ── input helpers ────────────────────────────────────────────────────────────

func parseCampaignPerformanceIDs(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	seen := make(map[string]bool, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, err := uuid.Parse(p)
		if err != nil {
			return nil, fmt.Errorf("ids: %q is not a UUID", p)
		}
		s := id.String()
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) > maxCampaignPerformanceIDs {
		return nil, fmt.Errorf("ids: at most %d campaigns per request, got %d", maxCampaignPerformanceIDs, len(out))
	}
	return out, nil
}

// escapeLikeLiteral makes a user substring safe as a LITERAL inside ILIKE
// (PG default escape is backslash): \ % _ are escaped so a name filter can
// never widen into a wildcard (§5 substring-ILIKE hazard).
func escapeLikeLiteral(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// campaignPerformanceScanBounds: lower = min(COALESCE(scheduled_at, created_at))
// - 24h (a Denver-scheduled send never produces events before that), upper =
// now + 1h. Both are RAW timestamptz bounds so the planner prunes partitions.
func campaignPerformanceScanBounds(rows []cpCampaignRow, now time.Time) (time.Time, time.Time) {
	var minT time.Time
	for _, c := range rows {
		t := now
		switch {
		case c.ScheduledAt.Valid:
			t = c.ScheduledAt.Time
		case c.CreatedAt.Valid:
			t = c.CreatedAt.Time
		}
		if minT.IsZero() || t.Before(minT) {
			minT = t
		}
	}
	if minT.IsZero() {
		minT = now
	}
	return minT.Add(-24 * time.Hour).UTC(), now.Add(time.Hour).UTC()
}

func campaignPerformanceMaturity(sched sql.NullTime, now time.Time) cpMaturity {
	m := cpMaturity{ThresholdDays: campaignPerformanceMaturityDays, Immature: true}
	if !sched.Valid {
		return m // no scheduled_at → cannot claim maturity; immature is the safe label
	}
	days := now.Sub(sched.Time).Hours() / 24
	days = math.Round(days*10) / 10
	m.DaysSinceScheduled = &days
	m.Immature = days < campaignPerformanceMaturityDays
	return m
}

// ── loaders ──────────────────────────────────────────────────────────────────

func (s *CampaignPerformanceService) loadCampaigns(ctx context.Context, orgID uuid.UUID, where string, args ...interface{}) ([]cpCampaignRow, bool, error) {
	q := campaignPerformanceCampaignSQL + where + campaignPerformanceOrder + fmt.Sprint(maxCampaignPerformanceIDs+1)
	all := append([]interface{}{orgID.String()}, args...)
	rs, err := s.db.QueryContext(ctx, q, all...)
	if err != nil {
		return nil, false, err
	}
	defer rs.Close()
	var out []cpCampaignRow
	for rs.Next() {
		var c cpCampaignRow
		if err := rs.Scan(&c.ID, &c.Name, &c.Status, &c.ScheduledAt, &c.CreatedAt, &c.Domain, &c.Transport); err != nil {
			return nil, false, err
		}
		out = append(out, c)
	}
	if err := rs.Err(); err != nil {
		return nil, false, err
	}
	truncated := false
	if len(out) > maxCampaignPerformanceIDs {
		out = out[:maxCampaignPerformanceIDs]
		truncated = true
	}
	return out, truncated, nil
}

// cpEngKey is "<campaign_id>" or "<campaign_id>|<isp>" for the ISP breakdown;
// the per-campaign total is always present under the bare id.
func cpEngKey(campaignID, ispName string) string {
	if ispName == "" {
		return campaignID
	}
	return campaignID + "|" + ispName
}

// loadEngagement runs one statement per campaign, engWorkers at a time, and
// merges the rows. Every statement runs in its own READ ONLY transaction with
// SET LOCAL statement_timeout so one long campaign cannot be cut by the pool
// default and cannot hold a write lock. Rows are filtered to the requested
// campaign so a merge can never double-count.
func (s *CampaignPerformanceService) loadEngagement(ctx context.Context, orgID uuid.UUID, ids []string, from, to time.Time, byISP bool) (map[string]cpEngagement, error) {
	workers := s.engWorkers
	if workers <= 0 {
		workers = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := map[string]cpEngagement{}
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		sem   = make(chan struct{}, workers)
		errCh = make(chan error, len(ids))
	)
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{} // acquire BEFORE go so workers==1 stays strictly sequential
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			part, err := s.loadEngagementOne(ctx, orgID, id, from, to, byISP)
			if err != nil {
				errCh <- fmt.Errorf("campaign %s: %w", id, err)
				cancel()
				return
			}
			mu.Lock()
			for k, v := range part {
				out[k] = v
			}
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	close(errCh)
	if err, ok := <-errCh; ok {
		return nil, err
	}
	return out, nil
}

func (s *CampaignPerformanceService) loadEngagementOne(ctx context.Context, orgID uuid.UUID, id string, from, to time.Time, byISP bool) (map[string]cpEngagement, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '`+campaignPerformanceStmtTimeout+`'`); err != nil {
		return nil, err
	}
	rs, err := tx.QueryContext(ctx, campaignPerformanceEngagementSQL(byISP), orgID.String(), pq.Array([]string{id}), from, to)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[string]cpEngagement{}
	for rs.Next() {
		var (
			cid, dom string
			e        cpEngagement
		)
		dest := []interface{}{&cid}
		if byISP {
			dest = append(dest, &dom)
		}
		dest = append(dest,
			&e.SentPG, &e.SentPGAllWriters,
			&e.Inclusive.Opens, &e.Inclusive.Clicks, &e.Inclusive.UniqueOpeners, &e.Inclusive.UniqueClickers, &e.Inclusive.ActionClicks,
			&e.Human.Opens, &e.Human.Clicks, &e.Human.UniqueOpeners, &e.Human.UniqueClickers, &e.Human.ActionClicks,
		)
		if err := rs.Scan(dest...); err != nil {
			return nil, err
		}
		if cid != id {
			continue
		}
		e.Source = "pg"
		if !byISP {
			out[cid] = e
			continue
		}
		ispName := campaignPerformanceISPUnresolved
		if strings.TrimSpace(dom) != "" {
			ispName = isp.GroupFromDomain(dom)
		}
		out[cpEngKey(cid, ispName)] = addEngagement(out[cpEngKey(cid, ispName)], e)
		out[cid] = addEngagement(out[cid], e)
	}
	return out, rs.Err()
}

// addEngagement sums event counts across recipient_domain buckets that fold to
// the same ISP. DISTINCT-subscriber counts are also summed: a subscriber has
// exactly one recipient_domain, so the buckets are disjoint by subscriber and
// the sum equals the distinct count over the union.
func addEngagement(a, b cpEngagement) cpEngagement {
	a.SentPG += b.SentPG
	a.SentPGAllWriters += b.SentPGAllWriters
	a.Inclusive.Opens += b.Inclusive.Opens
	a.Inclusive.Clicks += b.Inclusive.Clicks
	a.Inclusive.UniqueOpeners += b.Inclusive.UniqueOpeners
	a.Inclusive.UniqueClickers += b.Inclusive.UniqueClickers
	a.Inclusive.ActionClicks += b.Inclusive.ActionClicks
	a.Human.Opens += b.Human.Opens
	a.Human.Clicks += b.Human.Clicks
	a.Human.UniqueOpeners += b.Human.UniqueOpeners
	a.Human.UniqueClickers += b.Human.UniqueClickers
	a.Human.ActionClicks += b.Human.ActionClicks
	a.Source = "pg"
	return a
}

type cpConvRow struct {
	n      int64
	payout float64
}

func (s *CampaignPerformanceService) loadConversions(ctx context.Context, orgID uuid.UUID, ids []string) (map[string]cpConvRow, error) {
	rs, err := s.db.QueryContext(ctx, campaignPerformanceConversionsSQL, orgID.String(), pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[string]cpConvRow{}
	for rs.Next() {
		var (
			cid string
			c   cpConvRow
		)
		if err := rs.Scan(&cid, &c.n, &c.payout); err != nil {
			return nil, err
		}
		out[cid] = c
	}
	return out, rs.Err()
}

func newCPDelivery() *cpDelivery {
	return &cpDelivery{Source: "lake", SourcesSeen: []string{}, Excluded: map[string]int64{}}
}

// loadDelivery runs the lake breakdown(s). Returns (per-campaign, per-campaign
// per-ISP, reason) — a non-empty reason means the lake half is unavailable and
// the caller fails soft.
func (s *CampaignPerformanceService) loadDelivery(ctx context.Context, ids []string, fromDt, toDt string, byISP bool) (map[string]*cpDelivery, map[string]map[string]*cpDelivery, string) {
	if s.lakeEnabled == nil || !s.lakeEnabled() || s.lake == nil {
		return nil, nil, "lake reader disabled"
	}
	total := map[string]*cpDelivery{}
	perISP := map[string]map[string]*cpDelivery{}
	sources := map[string]map[string]bool{}

	for start := 0; start < len(ids); start += campaignPerformanceLakeChunk {
		end := start + campaignPerformanceLakeChunk
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]

		// Pass 1: campaign × event_type × source — the totals + sources_seen.
		rows, err := s.lake(ctx, analytics.BreakdownFilter{
			From: fromDt, To: toDt,
			GroupBy:     []string{"campaign_id", "event_type", "source"},
			SourceIn:    []string{"pmta", "ses", "kumo"},
			CampaignIDs: chunk,
			Limit:       5000,
		})
		if err != nil {
			log.Printf("[campaign-performance] lake breakdown failed: %v", err)
			return nil, nil, "lake breakdown failed: " + err.Error()
		}
		for _, r := range rows {
			cid := r.Keys["campaign_id"]
			d := total[cid]
			if d == nil {
				d = newCPDelivery()
				total[cid] = d
			}
			foldDelivery(d, r.Keys["event_type"], r.Count)
			if src := r.Keys["source"]; src != "" {
				if sources[cid] == nil {
					sources[cid] = map[string]bool{}
				}
				sources[cid][src] = true
			}
		}

		if !byISP {
			continue
		}
		// Pass 2: campaign × isp × event_type (3 dims is the reader's cap).
		rows, err = s.lake(ctx, analytics.BreakdownFilter{
			From: fromDt, To: toDt,
			GroupBy:     []string{"campaign_id", "isp", "event_type"},
			SourceIn:    []string{"pmta", "ses", "kumo"},
			CampaignIDs: chunk,
			Limit:       5000,
		})
		if err != nil {
			log.Printf("[campaign-performance] lake isp breakdown failed: %v", err)
			return nil, nil, "lake isp breakdown failed: " + err.Error()
		}
		for _, r := range rows {
			cid := r.Keys["campaign_id"]
			ispName := r.Keys["isp"]
			if perISP[cid] == nil {
				perISP[cid] = map[string]*cpDelivery{}
			}
			d := perISP[cid][ispName]
			if d == nil {
				d = newCPDelivery()
				perISP[cid][ispName] = d
			}
			foldDelivery(d, r.Keys["event_type"], r.Count)
		}
	}
	for cid, d := range total {
		for src := range sources[cid] {
			d.SourcesSeen = append(d.SourcesSeen, src)
		}
		sort.Strings(d.SourcesSeen)
		finalizeDelivery(d)
	}
	for _, m := range perISP {
		for _, d := range m {
			finalizeDelivery(d)
		}
	}
	return total, perISP, ""
}

// foldDelivery maps one reclassified lake event_type bucket onto the §3 classes.
// Anything not delivered/hard/soft/complaint is disclosed under Excluded —
// never silently summed.
func foldDelivery(d *cpDelivery, eventType string, n int64) {
	switch eventType {
	case "delivered":
		d.Delivered += n
	case "hard_bounce":
		d.Hard += n
	case "soft_bounce":
		d.Soft += n
	case "complaint":
		d.Complaint += n
	case "bounced":
		// Untyped bounce (no bounce_cat reclassification possible) — part of
		// derived attempted per §2, disclosed separately.
		d.Excluded["untyped_bounced"] += n
	case "":
		// no bucket
	default:
		d.Excluded[eventType] += n
	}
}

// finalizeDelivery derives attempted (§2): delivered + hard + soft + untyped
// bounced. Hops (relayed_to_ses), reputation_block, administrative,
// preflight_validation and delivery_delay stay out.
func finalizeDelivery(d *cpDelivery) {
	d.AttemptedDerived = d.Delivered + d.Hard + d.Soft + d.Excluded["untyped_bounced"]
	if d.SourcesSeen == nil {
		d.SourcesSeen = []string{}
	}
}

// ── rates ────────────────────────────────────────────────────────────────────

func cpMakeRate(num int64, numName string, den *int64, denName, unavailable string) cpRate {
	r := cpRate{Numerator: num, NumeratorName: numName, DenominatorName: denName, Denominator: den}
	switch {
	case unavailable != "":
		r.Reason = "denominator unavailable: " + unavailable
	case den == nil:
		r.Reason = "denominator unavailable"
	case *den == 0:
		r.Reason = "denominator is zero"
	default:
		v := math.Round(float64(num)/float64(*den)*1e6) / 1e6
		r.Value = &v
	}
	return r
}

// campaignPerformanceRates builds the disclosed rate set (§2). d may be nil
// when the lake half is unavailable — every delivered/attempted-based rate is
// then null with the reason; CTOR (PG over PG) still computes.
func campaignPerformanceRates(d *cpDelivery, e cpEngagement, lakeReason string) map[string]cpRate {
	var attempted, delivered *int64
	if d != nil && lakeReason == "" {
		a, dl := d.AttemptedDerived, d.Delivered
		attempted, delivered = &a, &dl
	}
	var hard, soft, complaint, dl int64
	if d != nil {
		hard, soft, complaint, dl = d.Hard, d.Soft, d.Complaint, d.Delivered
	}
	openers := e.Inclusive.UniqueOpeners
	hOpeners := e.Human.UniqueOpeners
	return map[string]cpRate{
		"delivery_rate":    cpMakeRate(dl, "delivered", attempted, "attempted_derived", lakeReason),
		"hard_rate":        cpMakeRate(hard, "hard", attempted, "attempted_derived", lakeReason),
		"soft_rate":        cpMakeRate(soft, "soft", attempted, "attempted_derived", lakeReason),
		"complaint_rate":   cpMakeRate(complaint, "complaint", attempted, "attempted_derived", lakeReason),
		"open_rate":        cpMakeRate(openers, "inclusive.unique_openers", delivered, "delivered", lakeReason),
		"click_rate":       cpMakeRate(e.Inclusive.UniqueClickers, "inclusive.unique_clickers", delivered, "delivered", lakeReason),
		"human_open_rate":  cpMakeRate(hOpeners, "human.unique_openers", delivered, "delivered", lakeReason),
		"human_click_rate": cpMakeRate(e.Human.UniqueClickers, "human.unique_clickers", delivered, "delivered", lakeReason),
		"ctor":             cpMakeRate(e.Inclusive.UniqueClickers, "inclusive.unique_clickers", &openers, "inclusive.unique_openers", ""),
		"human_ctor":       cpMakeRate(e.Human.UniqueClickers, "human.unique_clickers", &hOpeners, "human.unique_openers", ""),
	}
}

// campaignPerformanceISPRows joins the lake's per-ISP delivery with PG's
// per-ISP engagement on the ISP label. An ISP present on one side only still
// gets a row; its cross-source rates are null with a reason.
func campaignPerformanceISPRows(del map[string]*cpDelivery, eng map[string]cpEngagement, cid, lakeReason string) []cpISPRow {
	names := map[string]bool{}
	for n := range del {
		names[n] = true
	}
	prefix := cid + "|"
	for k := range eng {
		if strings.HasPrefix(k, prefix) {
			names[strings.TrimPrefix(k, prefix)] = true
		}
	}
	keys := make([]string, 0, len(names))
	for n := range names {
		keys = append(keys, n)
	}
	sort.Strings(keys)
	out := make([]cpISPRow, 0, len(keys))
	for _, n := range keys {
		row := cpISPRow{ISP: n}
		e := eng[cpEngKey(cid, n)]
		e.Source = "pg"
		row.Engagement = e
		reason := lakeReason
		d := del[n]
		switch {
		case lakeReason != "":
			row.Delivery = map[string]string{"unavailable": lakeReason}
		case d == nil:
			reason = "no lake delivery rows for this ISP label"
			row.Delivery = map[string]string{"unavailable": reason}
		default:
			row.Delivery = d
		}
		row.Rates = campaignPerformanceRates(d, e, reason)
		out = append(out, row)
	}
	return out
}
