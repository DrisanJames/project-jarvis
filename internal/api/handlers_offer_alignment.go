package api

// Offer Alignment — READ ONLY analytics keyed on the decision object
// offer × ISP × data-source × creative × subject (design doc 2026-07-07 PART B).
//
// Two independent channels, never blended:
//   - PERFORMANCE: rpm / human click rate / clicker rate.
//   - DELIVERY HEALTH: badge HEALTHY / BLOCKING / THROTTLED / LIST_QUALITY
//     (+ LOW_VOLUME under the sample floor), classified by
//     classifyAlignmentCell — the single source for thresholds.
//
// METRIC_CONTRACT.md is binding (see its "Offer Alignment" section):
//   - Delivery cells come from the Athena lake, source IN ('pmta','ses'),
//     COUNT(DISTINCT event_uid), scoped by the offer's campaign-ID set.
//     NEVER PG campaign counters.
//   - Engagement comes from PG mailing_tracking_events with the canonical
//     human verdict ignite_verdict_is_human(ignite_event_verdict(...)) and
//     money-link clicks only (link_url ILIKE '%source_id=email%').
//   - Engagement rates are PG-scoped (clicker_rate divides by the PG
//     sent-event count); delivery rates are lake-scoped. The two stores are
//     never divided into each other.
//   - Conversions reuse the conv UNION shape from
//     handlers_analytics_creatives.go (offer-suppression 'converted' rows +
//     CPM manual conversions, offer-scoped via everflow ids).
//
// Offer → campaign-set attribution (PART A stamped + historical inference):
// stamped campaigns carry offer_key (attribution_source 'payload' |
// 'name_inferred'); historical campaigns (offer_key NULL) are inferred via
// the campaign-name " - <offer>" suffix and via slug-anchored money clicks.
// Every response carries {stamped_campaigns, inferred_campaigns} so the UI
// can label inferred data.
//
// Routes (server_routes_mailing.go, next to the /analytics lake block):
//
//	GET  /api/mailing/offer-alignment/matrix    — snapshot-backed offer × ISP matrix
//	GET  /api/mailing/offer-alignment/offer     — live per-offer profile
//	GET  /api/mailing/offer-alignment/evidence  — dsn_family evidence for one offer × ISP
//	POST /api/mailing/offer-alignment/refresh   — force snapshot rebuild (202)

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/analytics"
)

// ─── thresholds (design doc B5 — single source, unit-tested) ────────────────

const (
	// alignmentMinSample: cells under this many attempted are LOW_VOLUME (no
	// badge verdict, no action, sample_ok=false).
	alignmentMinSample = 500
	// BLOCKING: reputation_block/attempted ≥ 10% OR blocking-family DSN count
	// (HM08+HCM2+S3140) ≥ 100.
	alignmentBlockRateThreshold   = 0.10
	alignmentBlockingDSNThreshold = 100
	// THROTTLED: deferred/attempted ≥ 20% while block rate stays under the
	// BLOCKING bar — capacity language, never reputation.
	alignmentDeferredRateThreshold = 0.20
	// LIST_QUALITY: hard/attempted ≥ 3% AND the hard count is dominated
	// (≥50%) by the 5.1.1 invalid-mailbox family.
	alignmentHardRateThreshold = 0.03
	// Action heuristics on top of the badge (operator rule table).
	alignmentScaleRPMThreshold      = 25.0  // HEALTHY + rpm ≥ this ⇒ "scale"
	alignmentDemandClickerRate      = 0.005 // BLOCKING + clicker rate ≥ this ⇒ demand-but-blocked
	alignmentMismatchClickerRate    = 0.002 // HEALTHY + clicker rate < this ⇒ audience mismatch
	alignmentMismatchMinPGSent      = 1000  // ...but only with a real engagement sample
	alignmentAppleHM08ActionMinimum = 100   // HM08 count that triggers the apple-pull action
)

// offerAlignmentQueryTimeout bounds the live profile/evidence handlers
// (2 Athena calls + several PG aggregations over a bounded campaign set).
const offerAlignmentQueryTimeout = 180 * time.Second

// alignmentISPBucketRe pre-validates the evidence endpoint's isp param so a
// malformed value 400s here instead of 500ing inside the lake validator
// (which applies the same token rule to the isp Eq filter).
var alignmentISPBucketRe = regexp.MustCompile(`^[a-z0-9_\-]+$`)

// alignmentFailureEventTypes are the (read-time reclassified) lake event
// types the evidence endpoint keeps — bounce + deferral rows only.
// 'administrative' (operator flush) and 'delivered' are excluded.
var alignmentFailureEventTypes = map[string]bool{
	"hard_bounce":      true,
	"soft_bounce":      true,
	"reputation_block": true,
	"delivery_delay":   true,
}

// ─── ISP classification (PG side) ────────────────────────────────────────────
//
// alignmentISPCase renders the canonical clean-ISP CASE over a recipient-domain
// SQL expression, mirroring ISP_CASE_PG in agents/dbknowledge/_db.py and
// ispExpr in internal/analytics/reader.go EXACTLY (buckets: microsoft, gmail,
// yahoo, apple, comcast, aol, att, sbcglobal, cox, charter, verizon, other) so
// PG engagement rows land in the same buckets as lake delivery rows.
func alignmentISPCase(domainExpr string) string {
	d := "lower(" + domainExpr + ")"
	return "CASE" +
		" WHEN " + d + " IN ('outlook.com','hotmail.com','live.com','msn.com','hotmail.co.uk','windowslive.com','passport.com','outlook.co.uk') THEN 'microsoft'" +
		" WHEN " + d + " IN ('gmail.com','googlemail.com') THEN 'gmail'" +
		" WHEN " + d + " IN ('yahoo.com','ymail.com','rocketmail.com','yahoo.co.uk','yahoo.ca') THEN 'yahoo'" +
		" WHEN " + d + " IN ('icloud.com','me.com','mac.com') THEN 'apple'" +
		" WHEN " + d + " = 'comcast.net' THEN 'comcast'" +
		" WHEN " + d + " = 'aol.com' THEN 'aol'" +
		" WHEN " + d + " = 'att.net' THEN 'att'" +
		" WHEN " + d + " = 'sbcglobal.net' THEN 'sbcglobal'" +
		" WHEN " + d + " = 'cox.net' THEN 'cox'" +
		" WHEN " + d + " IN ('charter.net','spectrum.net') THEN 'charter'" +
		" WHEN " + d + " = 'verizon.net' THEN 'verizon'" +
		" ELSE 'other' END"
}

// ─── offer resolution ────────────────────────────────────────────────────────

// resolveOfferSlugsForKey is the ctx-based sibling of resolveOfferSlugs
// (handlers_analytics_creatives.go) so the snapshot worker (no *http.Request)
// can share the exact same slug-map resolution semantics: raw slug,
// everflow id, or offer-name fragment → (slugs, everflow ids, offer name).
// Uncatalogued bare slugs self-resolve with no everflow scoping.
// alignmentDB is the DB seam for the alignment queries — satisfied by both
// *sql.DB (pool) and *sql.Conn (dedicated session with an elevated
// statement_timeout for the heavy scans).
type alignmentDB interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

// alignmentSessionConn returns a dedicated connection whose
// statement_timeout is raised above the pool default (30s in prod — which
// cold/under-send-load drip-scale scans exceed even chunked; observed on
// ps8241 2026-07-08). Callers MUST invoke release (RESET ALL + Close, the
// finalizeAudience idiom) so the elevated timeout never leaks back into the
// pool. On any failure it returns (nil, no-op, err) and callers fall back to
// the pool.
func alignmentSessionConn(ctx context.Context, db *sql.DB, timeoutMs int) (*sql.Conn, func(), error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, func() {}, err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("SET statement_timeout = '%d'", timeoutMs)); err != nil {
		conn.Close()
		return nil, func() {}, err
	}
	release := func() {
		resetCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn.ExecContext(resetCtx, "RESET ALL")
		cancel()
		conn.Close()
	}
	return conn, release, nil
}

func resolveOfferSlugsForKey(ctx context.Context, db alignmentDB, offer string) (slugs []string, efids []string, offerName string, err error) {
	const q = `
		SELECT cratoolpro_slug, COALESCE(offer_name, ''), COALESCE(everflow_offer_id, '')
		FROM mailing_offer_slug_map
		WHERE enabled = TRUE
		  AND (
		        everflow_offer_id = $1
		     OR upper(cratoolpro_slug) = upper($1)
		     OR ($1 <> '' AND offer_name ILIKE '%' || $1 || '%')
		  )
	`
	rows, qerr := db.QueryContext(ctx, q, offer)
	if qerr != nil {
		return nil, nil, "", qerr
	}
	defer rows.Close()
	efSeen := map[string]bool{}
	for rows.Next() {
		var slug, name, efid string
		if scanErr := rows.Scan(&slug, &name, &efid); scanErr != nil {
			return nil, nil, "", scanErr
		}
		slugs = append(slugs, slug)
		if offerName == "" && name != "" {
			offerName = name
		}
		if efid != "" && !efSeen[efid] {
			efSeen[efid] = true
			efids = append(efids, efid)
		}
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, nil, "", rerr
	}
	if len(slugs) == 0 && isBareSlug(offer) {
		slugs = []string{strings.ToUpper(offer)}
	}
	return slugs, efids, offerName, nil
}

// offerCampaignSet is the resolved campaign universe for one offer + window.
// Stamped campaigns carry offer_key (PART A deploy-time stamping); Inferred
// are historical campaigns matched by name-suffix or slug-anchored clicks.
type offerCampaignSet struct {
	IDs      []string // stamped first, then inferred (deduped)
	Stamped  int
	Inferred int
}

// alignmentLikeEscape escapes ILIKE metacharacters in an offer key / name so
// it can be embedded in a '% - <value>' suffix pattern as a literal.
func alignmentLikeEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// resolveOfferCampaignSet unions the stamped campaign set
// (mailing_campaigns.offer_key = key) with the inferred historical set:
// (a) name-suffix match — the operator " - <offer>" naming convention
// (nameSuffixOffer, performance_snapshot.go) expressed as a SQL suffix ILIKE
// against the offer key and the slug-map offer name; (b) slug-anchored money
// clicks — campaigns without offer_key whose recipients clicked this offer's
// money slugs in the window (campaignToOffer cascade, performance_snapshot.go).
// sendingDomain ("" = no filter) is a diagnostic-only filter matched against
// the campaign from_email host.
func resolveOfferCampaignSet(
	ctx context.Context, db alignmentDB, orgID interface{},
	offerKey, offerName string, patterns []string,
	from, to time.Time, sendingDomain string,
) (offerCampaignSet, error) {
	set := offerCampaignSet{}
	seen := map[string]bool{}

	// (0) stamped: forward data, exact by construction.
	// $1 org, $2 key, $3 from, $4 to, $5 sending-domain ('' = all).
	const stampedQ = `
		SELECT id::text FROM mailing_campaigns
		WHERE organization_id = $1
		  AND lower(COALESCE(offer_key,'')) = lower($2)
		  AND scheduled_at >= $3 AND scheduled_at <= $4
		  AND ($5 = '' OR lower(split_part(from_email,'@',2)) = lower($5))
		ORDER BY scheduled_at DESC
	`
	rows, err := db.QueryContext(ctx, stampedQ, orgID, offerKey, from, to, sendingDomain)
	if err != nil {
		return set, fmt.Errorf("stamped campaign set: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return set, err
		}
		if !seen[id] {
			seen[id] = true
			set.IDs = append(set.IDs, id)
			set.Stamped++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return set, err
	}

	// (1) inferred by name suffix (historical, offer_key unset). The suffix
	// pattern is anchored to the " - " separator so a mid-name mention never
	// matches. offerName may be '' — the predicate skips it then.
	// $1 org, $2 from, $3 to, $4 key pattern, $5 name pattern (''=skip), $6 domain.
	const nameQ = `
		SELECT id::text FROM mailing_campaigns
		WHERE organization_id = $1
		  AND COALESCE(offer_key,'') = ''
		  AND scheduled_at >= $2 AND scheduled_at <= $3
		  AND (name ILIKE $4 OR ($5 <> '' AND name ILIKE $5))
		  AND ($6 = '' OR lower(split_part(from_email,'@',2)) = lower($6))
		ORDER BY scheduled_at DESC
	`
	keyPattern := "% - " + alignmentLikeEscape(offerKey)
	namePattern := ""
	if offerName != "" {
		namePattern = "% - " + alignmentLikeEscape(offerName)
	}
	rows, err = db.QueryContext(ctx, nameQ, orgID, from, to, keyPattern, namePattern, sendingDomain)
	if err != nil {
		return set, fmt.Errorf("name-inferred campaign set: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return set, err
		}
		if !seen[id] {
			seen[id] = true
			set.IDs = append(set.IDs, id)
			set.Inferred++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return set, err
	}

	// (2) inferred by slug-anchored money clicks (historical). Skipped when
	// the offer has no slug patterns.
	if len(patterns) > 0 {
		// $1 org, $2 from, $3 to, $4 slug patterns, $5 domain.
		const clickQ = `
			SELECT DISTINCT t.campaign_id::text
			FROM mailing_tracking_events t
			JOIN mailing_campaigns c
			  ON c.id = t.campaign_id AND c.organization_id = $1
			WHERE t.organization_id = $1
			  AND COALESCE(c.offer_key,'') = ''
			  AND t.event_at >= $2 AND t.event_at <= $3
			  AND t.event_type = 'clicked'
			  AND t.link_url ILIKE '%source_id=email%'
			  AND t.link_url ILIKE ANY($4)
			  AND ($5 = '' OR lower(split_part(c.from_email,'@',2)) = lower($5))
		`
		rows, err = db.QueryContext(ctx, clickQ, orgID, from, to, pq.Array(patterns), sendingDomain)
		if err != nil {
			return set, fmt.Errorf("click-inferred campaign set: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return set, err
			}
			if !seen[id] {
				seen[id] = true
				set.IDs = append(set.IDs, id)
				set.Inferred++
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return set, err
		}
	}
	return set, nil
}

// alignmentLakeChunk bounds the campaign IN-list per Athena call (the reader
// caps at 2000; Athena's statement ceiling is the constraint). Drip-scale
// offers (ps8241: 20k+ campaigns) are fetched in FULL via chunked calls —
// the previous truncate-to-2000 approach read ~10% of the offer and reported
// 100k delivered on 2M+ mailed (operator, 2026-07-08). Athena cost is per
// bytes scanned; chunked calls re-scan the same partitions, but the events
// table slice is small (~0.6MB scanned per query measured), so the added
// spend is cents. Chunks are disjoint campaign sets → per-chunk DISTINCT
// counts sum exactly.

// ─── lake delivery + dsn aggregation ─────────────────────────────────────────

// alignmentDelivery is one offer × ISP delivery cell (lake truth,
// COUNT(DISTINCT event_uid), source IN ('pmta','ses')).
type alignmentDelivery struct {
	Delivered       int64
	Hard            int64
	Soft            int64
	ReputationBlock int64
	Deferred        int64 // delivery_delay events (per-RETRY, see contract)
}

func (d *alignmentDelivery) attempted() int64 {
	return d.Delivered + d.Hard + d.Soft + d.ReputationBlock
}

// fetchAlignmentLakeRows runs the two lake Breakdowns for one campaign set:
// delivery (local_dt × isp × event_type) and dsn families (local_dt × isp ×
// dsn_family). local_dt rides along so a 30-day fetch can be sliced into the
// 7-day window without a second Athena round trip.
const alignmentLakeChunk = 2000

// alignmentLakeConcurrency bounds parallel Athena calls per fetch. Serial
// chunking put drip-scale profiles (10 chunks x 2 calls x ~5s) past the
// handler budget; Athena executes concurrent queries happily. 4 keeps a
// single fetch from monopolizing the workgroup while refresh + profile runs
// overlap.
const alignmentLakeConcurrency = 4

func fetchAlignmentLakeRows(ctx context.Context, ids []string, fromDt, toDt string) (delivery, dsn []analytics.BreakdownRow, err error) {
	type job struct{ dsnPass bool; chunk []string }
	jobs := []job{}
	for start := 0; start < len(ids); start += alignmentLakeChunk {
		end := start + alignmentLakeChunk
		if end > len(ids) {
			end = len(ids)
		}
		jobs = append(jobs, job{false, ids[start:end]}, job{true, ids[start:end]})
	}

	var (
		mu       sync.Mutex
		firstErr error
	)
	sem := make(chan struct{}, alignmentLakeConcurrency)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			mu.Lock()
			done := firstErr != nil || ctx.Err() != nil
			mu.Unlock()
			if done {
				return
			}
			filter := analytics.BreakdownFilter{
				From: fromDt, To: toDt,
				SourceIn:    []string{"pmta", "ses"},
				CampaignIDs: j.chunk,
				Limit:       5000,
			}
			if j.dsnPass {
				filter.GroupBy = []string{"local_dt", "isp", "dsn_family"}
			} else {
				filter.GroupBy = []string{"local_dt", "isp", "event_type"}
				// Deferral truth = unique held-up mailboxes, not per-retry
				// events (2.6x inflated) — the THROTTLED threshold depends on it.
				filter.DedupDelayByEmail = true
			}
			rows, qerr := analytics.Breakdown(ctx, filter)
			mu.Lock()
			defer mu.Unlock()
			if qerr != nil {
				if firstErr == nil {
					firstErr = qerr
				}
				return
			}
			if j.dsnPass {
				dsn = append(dsn, rows...)
			} else {
				delivery = append(delivery, rows...)
			}
		}(j)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, nil, firstErr
	}
	return delivery, dsn, nil
}

// aggregateAlignmentDelivery folds delivery breakdown rows (local_dt × isp ×
// event_type) into per-ISP cells, keeping only rows with local_dt >= minLocalDt
// ("" = keep all).
func aggregateAlignmentDelivery(rows []analytics.BreakdownRow, minLocalDt string) map[string]*alignmentDelivery {
	out := map[string]*alignmentDelivery{}
	for _, r := range rows {
		if minLocalDt != "" && r.Keys["local_dt"] < minLocalDt {
			continue
		}
		isp := r.Keys["isp"]
		if isp == "" {
			isp = "other"
		}
		d := out[isp]
		if d == nil {
			d = &alignmentDelivery{}
			out[isp] = d
		}
		switch r.Keys["event_type"] {
		case "delivered":
			d.Delivered += r.Count
		case "hard_bounce":
			d.Hard += r.Count
		case "soft_bounce":
			d.Soft += r.Count
		case "reputation_block":
			d.ReputationBlock += r.Count
		case "delivery_delay":
			d.Deferred += r.Count
			// 'administrative' and engagement types are deliberately ignored.
		}
	}
	return out
}

// alignmentDSNFamilies are the diagnostic families the classifier consumes.
var alignmentDSNFamilies = map[string]bool{
	"HM08": true, "HCM2": true, "CS01": true, "CS02": true, "PH01": true,
	"4.7.650": true, "4.7.0": true, "4.7.28": true,
	"S3140": true, "S3150": true, "5.1.1": true, "5.2.2": true,
}

// aggregateAlignmentDSN folds dsn breakdown rows into isp → family → count,
// keeping only the classifier-relevant families and local_dt >= minLocalDt.
func aggregateAlignmentDSN(rows []analytics.BreakdownRow, minLocalDt string) map[string]map[string]int64 {
	out := map[string]map[string]int64{}
	for _, r := range rows {
		if minLocalDt != "" && r.Keys["local_dt"] < minLocalDt {
			continue
		}
		fam := r.Keys["dsn_family"]
		if !alignmentDSNFamilies[fam] {
			continue
		}
		isp := r.Keys["isp"]
		if isp == "" {
			isp = "other"
		}
		m := out[isp]
		if m == nil {
			m = map[string]int64{}
			out[isp] = m
		}
		m[fam] += r.Count
	}
	return out
}

// ─── badge / action classifier (single source; table-tested) ────────────────

type alignmentCellStats struct {
	Delivered       int64
	Hard            int64
	Soft            int64
	ReputationBlock int64
	Deferred        int64
	BlockingDSN     int64 // HM08 + HCM2 + S3140
	HM08            int64
	Hard511         int64 // 5.1.1-family count
	HumanClicks     int64
	Clickers        int64
	PGSent          int64
	Revenue         float64
}

// classifyAlignmentCell derives the DELIVERY-HEALTH badge and the operator
// action for one offer × ISP cell. Thresholds per design doc B5; this function
// is the ONLY place they live. badge_reason always carries the numbers.
func classifyAlignmentCell(c alignmentCellStats) (badge, reason, action string, sampleOK bool) {
	attempted := c.Delivered + c.Hard + c.Soft + c.ReputationBlock
	if attempted < alignmentMinSample {
		return "LOW_VOLUME",
			fmt.Sprintf("only %d attempted (< %d) — sample too small for a verdict", attempted, alignmentMinSample),
			"", false
	}
	blockRate := float64(c.ReputationBlock) / float64(attempted)
	deferredRate := float64(c.Deferred) / float64(attempted)
	hardRate := float64(c.Hard) / float64(attempted)
	clickerRate := 0.0
	if c.PGSent > 0 {
		clickerRate = float64(c.Clickers) / float64(c.PGSent)
	}
	rpm := 0.0
	if c.Delivered > 0 {
		rpm = c.Revenue / float64(c.Delivered) * 1000
	}

	switch {
	case blockRate >= alignmentBlockRateThreshold || c.BlockingDSN >= alignmentBlockingDSNThreshold:
		badge = "BLOCKING"
		reason = fmt.Sprintf("reputation blocks %.1f%% of %d attempted (%d blocked; HM08+HCM2+S3140 = %d)",
			blockRate*100, attempted, c.ReputationBlock, c.BlockingDSN)
		switch {
		case c.HM08 >= alignmentAppleHM08ActionMinimum:
			action = "Pull this offer from Apple — HM08 is a local-policy (offer/content) block, not an audience problem"
		case clickerRate >= alignmentDemandClickerRate:
			action = "Demand exists but the ISP is blocking — fix reputation before scaling; do not raise volume"
		default:
			action = "Reduce volume on this ISP and rebuild reputation before re-scaling"
		}
	case deferredRate >= alignmentDeferredRateThreshold:
		badge = "THROTTLED"
		reason = fmt.Sprintf("deferrals %.1f%% of %d attempted (%d delay events) with block rate %.1f%% — capacity, not reputation",
			deferredRate*100, attempted, c.Deferred, blockRate*100)
		action = "Capacity throttle (4.7.650-class) — slow the send rate; do NOT treat as a reputation problem"
	case hardRate >= alignmentHardRateThreshold && c.Hard511*2 >= c.Hard:
		badge = "LIST_QUALITY"
		reason = fmt.Sprintf("hard bounces %.1f%% of %d attempted, dominated by 5.1.1 invalid-mailbox (%d of %d hard)",
			hardRate*100, attempted, c.Hard511, c.Hard)
		action = "Quarantine the offending data source — hard bounces are 5.1.1 invalid mailboxes, stop mailing them"
	default:
		badge = "HEALTHY"
		reason = fmt.Sprintf("healthy: block rate %.1f%%, hard %.1f%%, deferrals %.1f%% of %d attempted",
			blockRate*100, hardRate*100, deferredRate*100, attempted)
		switch {
		case c.PGSent >= alignmentMismatchMinPGSent && clickerRate < alignmentMismatchClickerRate:
			action = "Audience mismatch — delivery is healthy but the offer is not resonating on this ISP; rotate the offer"
		case c.Revenue > 0 && rpm >= alignmentScaleRPMThreshold:
			action = "Scale — healthy delivery with strong RPM"
		}
	}
	return badge, reason, action, true
}

// ─── dsn_family plain-English decode ─────────────────────────────────────────

var alignmentDSNMeanings = map[string]string{
	"HM08":    "Apple local-policy block — offer/content-level, not the audience",
	"HCM2":    "Apple per-IP reputation block",
	"CS01":    "Apple content-spam block",
	"CS02":    "Apple sustained-spam reputation block",
	"PH01":    "Apple phishing-suspicion block",
	"4.7.650": "Microsoft velocity throttle — capacity, slow down",
	"S3140":   "Microsoft network block",
	"S3150":   "Microsoft throttle/mitigation",
	"4.7.0":   "Gmail rate/auth deferral",
	"4.7.28":  "Gmail rate/auth deferral",
	"5.1.1":   "invalid mailbox — list quality",
	"5.2.2":   "mailbox full",
}

func alignmentDSNMeaning(family string) string {
	if m, ok := alignmentDSNMeanings[family]; ok {
		return m
	}
	return "unclassified — read sample diagnostic"
}

// ─── small numeric helpers (0, never NaN) ────────────────────────────────────

func alignmentDiv(num, den float64) float64 {
	if den <= 0 {
		return 0
	}
	return num / den
}

// ─── PG aggregations shared by the profile handler and the snapshot worker ──

// alignmentEngagement is the PG engagement cell: human money clicks, distinct
// clickers, and the PG sent-event denominator, per ISP bucket.
type alignmentEngagement struct {
	HumanClicks int64
	Clickers    int64
	PGSent      int64
}

// alignmentEngagementChunk bounds the campaign-id set per SENT-count query.
// Sized from live measurement (2026-07-07 prod diagnosis): the ANY(ids) index
// path is fast — a FULL 23,722-id/2.99M-row sent scan runs 7.8s, while
// per-chunk cost is ROUNDTRIP-dominated (~80ms of real work per statement).
// 150-id chunks turned drip-scale offers into 159 statements ≈ 60s of
// roundtrips; 8,000-id chunks keep every statement well under the prod 30s
// statement_timeout in ~3 roundtrips. Chunk counts sum exactly. Clicked rows
// are ~3 orders of magnitude fewer, so the click query runs once over the
// full set (keeps COUNT(DISTINCT clickers) exact).
const alignmentEngagementChunk = 8000

// fetchAlignmentEngagement aggregates mailing_tracking_events for the
// campaign set: sent-event denominator (chunked) plus verdict-human money
// clicks/clickers (single narrow pass over clicked rows using the STORED
// click_verdict column — write-side materialized + partial-indexed; the
// verdict function only computes for pre-backfill NULL rows), bucketed by the
// clean ISP of recipient_domain. NULL recipient_domain folds into 'other'.
func fetchAlignmentEngagement(ctx context.Context, db alignmentDB, orgID interface{}, ids []string, from, to time.Time) (map[string]*alignmentEngagement, error) {
	out := map[string]*alignmentEngagement{}
	if len(ids) == 0 {
		return out, nil
	}
	get := func(isp string) *alignmentEngagement {
		e := out[isp]
		if e == nil {
			e = &alignmentEngagement{}
			out[isp] = e
		}
		return e
	}

	sentQ := `
		SELECT ` + alignmentISPCase("COALESCE(t.recipient_domain,'')") + ` AS isp, COUNT(*)
		FROM mailing_tracking_events t
		WHERE t.organization_id = $1
		  AND t.campaign_id = ANY($2::uuid[])
		  AND t.event_type = 'sent'
		  AND t.event_at >= $3 AND t.event_at <= $4
		GROUP BY 1
	`
	for start := 0; start < len(ids); start += alignmentEngagementChunk {
		end := start + alignmentEngagementChunk
		if end > len(ids) {
			end = len(ids)
		}
		rows, err := db.QueryContext(ctx, sentQ, orgID, pq.Array(ids[start:end]), from, to)
		if err != nil {
			return nil, fmt.Errorf("alignment engagement (sent chunk %d): %w", start/alignmentEngagementChunk, err)
		}
		for rows.Next() {
			var isp string
			var n int64
			if err := rows.Scan(&isp, &n); err != nil {
				rows.Close()
				return nil, err
			}
			get(isp).PGSent += n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	clickQ := `
		SELECT ` + alignmentISPCase("COALESCE(t.recipient_domain,'')") + ` AS isp,
		       COUNT(*) AS human_clicks,
		       COUNT(DISTINCT t.subscriber_id) AS clickers
		FROM mailing_tracking_events t
		WHERE t.organization_id = $1
		  AND t.campaign_id = ANY($2::uuid[])
		  AND t.event_type = 'clicked'
		  AND t.event_at >= $3 AND t.event_at <= $4
		  AND t.link_url ILIKE '%source_id=email%'
		  AND ignite_verdict_is_human(COALESCE(t.click_verdict, ignite_event_verdict(t.user_agent, t.ip_address)))
		GROUP BY 1
	`
	rows, err := db.QueryContext(ctx, clickQ, orgID, pq.Array(ids), from, to)
	if err != nil {
		return nil, fmt.Errorf("alignment engagement (clicks): %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var isp string
		var clicks, clickers int64
		if err := rows.Scan(&isp, &clicks, &clickers); err != nil {
			return nil, err
		}
		e := get(isp)
		e.HumanClicks = clicks
		e.Clickers = clickers
	}
	return out, rows.Err()
}

// alignmentConversions is one ISP (or data-source) slice of the conversion
// ledger.
type alignmentConversions struct {
	Conversions int64
	Revenue     float64
}

// fetchAlignmentConversionsByISP counts offer-scoped conversions per the
// converting subscriber's email-domain ISP. The conv UNION mirrors
// creativesBreakdown (handlers_analytics_creatives.go): offer-suppression
// 'converted' rows (postback truth, revenue 0) + CPM manual conversions
// (sub1 = subscriber UUID, revenue carried). Offers without everflow ids
// cannot be honestly scoped — returns empty (0 conversions), matching the
// creatives handler's convention.
func fetchAlignmentConversionsByISP(ctx context.Context, db alignmentDB, orgID interface{}, efids []string, from, to time.Time) (map[string]*alignmentConversions, error) {
	if len(efids) == 0 {
		return map[string]*alignmentConversions{}, nil
	}
	q := `
		WITH conv AS (
			SELECT s.subscriber_id, 0::numeric AS revenue
			FROM mailing_offer_suppressions s
			WHERE s.organization_id = $1
			  AND s.reason = 'converted'
			  AND s.suppressed_at >= $2 AND s.suppressed_at <= $3
			  AND s.offer_id IN (SELECT id FROM mailing_offers WHERE organization_id = $1 AND everflow_offer_id = ANY($4))
			UNION ALL
			SELECT cm.sub1::uuid AS subscriber_id, COALESCE(cm.revenue, 0)::numeric AS revenue
			FROM mailing_cpm_manual_conversions cm
			WHERE cm.organization_id = $1
			  AND cm.converted_at >= $2 AND cm.converted_at <= $3
			  AND cm.sub1 ~ '^[0-9a-f-]{36}$'
			  AND cm.deal_id IN (SELECT id FROM mailing_cpm_deals WHERE organization_id = $1 AND (everflow_offer_id = ANY($4) OR offer_id IN (SELECT id FROM mailing_offers WHERE organization_id = $1 AND everflow_offer_id = ANY($4))))
		)
		SELECT ` + alignmentISPCase("split_part(sub.email,'@',2)") + ` AS isp,
		       COUNT(*) AS conversions,
		       COALESCE(SUM(cv.revenue), 0)::float8 AS revenue
		FROM conv cv
		JOIN mailing_subscribers sub ON sub.id = cv.subscriber_id AND sub.organization_id = $1
		GROUP BY 1
	`
	rows, err := db.QueryContext(ctx, q, orgID, from, to, pq.Array(efids))
	if err != nil {
		return nil, fmt.Errorf("alignment conversions: %w", err)
	}
	defer rows.Close()
	out := map[string]*alignmentConversions{}
	for rows.Next() {
		var isp string
		var c alignmentConversions
		if err := rows.Scan(&isp, &c.Conversions, &c.Revenue); err != nil {
			return nil, err
		}
		out[isp] = &c
	}
	return out, rows.Err()
}

// resolveOfferConversionPrice returns the per-conversion dollar value used to
// ESTIMATE revenue when the conversion ledgers carry no dollars — which, as
// verified against prod 2026-07-07, is every conversion ever recorded:
// mailing_offer_suppressions has NO revenue column, every
// mailing_cpm_manual_conversions.revenue row is 0, and
// mailing_revenue_attributions is empty. Price = the offer's CPM-deal eCPA
// goal (the same economics the CPM calculator derives: eCPA = budget /
// conversions), else the offer's payout, else 0 — a zero price keeps revenue
// at 0 rather than inventing a number. Estimated revenue is disclosed in
// METRIC_CONTRACT §9.
func resolveOfferConversionPrice(ctx context.Context, db alignmentDB, orgID interface{}, efids []string) float64 {
	if len(efids) == 0 {
		return 0
	}
	var price float64
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(
			(SELECT MAX(d.ecpa_goal) FROM mailing_cpm_deals d
			  WHERE d.organization_id = $1
			    AND COALESCE(d.ecpa_goal, 0) > 0
			    AND (d.everflow_offer_id = ANY($2)
			         OR d.offer_id IN (SELECT id FROM mailing_offers
			                            WHERE organization_id = $1 AND everflow_offer_id = ANY($2)))),
			(SELECT MAX(o.payout) FROM mailing_offers o
			  WHERE o.organization_id = $1 AND o.everflow_offer_id = ANY($2)
			    AND COALESCE(o.payout, 0) > 0),
			0)::float8
	`, orgID, pq.Array(efids)).Scan(&price)
	if err != nil {
		log.Printf("[offer-alignment] conversion price lookup failed (revenue stays ledger-valued): %v", err)
		return 0
	}
	return price
}

// applyEstimatedConversionRevenue prices zero-dollar conversion buckets at
// the offer's per-conversion price. Only fires when the ledger contributed
// NO dollars at all (any real ledger revenue wins over the estimate).
func applyEstimatedConversionRevenue(conv map[string]*alignmentConversions, price float64) {
	if price <= 0 {
		return
	}
	var total float64
	for _, c := range conv {
		total += c.Revenue
	}
	if total != 0 {
		return
	}
	for _, c := range conv {
		c.Revenue = price * float64(c.Conversions)
	}
}

// ─── HTTP: matrix ────────────────────────────────────────────────────────────

// offerAlignmentMatrixRow matches web/src/components/mailing/offeralignment/
// types.ts MatrixRow byte-for-byte. All rates are FRACTIONS 0–1.
type offerAlignmentMatrixRow struct {
	OfferKey            string  `json:"offer_key"`
	OfferName           string  `json:"offer_name"`
	ISP                 string  `json:"isp"`
	Delivered           int64   `json:"delivered"`
	Hard                int64   `json:"hard"`
	Soft                int64   `json:"soft"`
	ReputationBlock     int64   `json:"reputation_block"`
	Deferred            int64   `json:"deferred"`
	BlockRate           float64 `json:"block_rate"`
	HumanClicks         int64   `json:"human_clicks"`
	Clickers            int64   `json:"clickers"`
	ClickerRate         float64 `json:"clicker_rate"`
	Conversions         int64   `json:"conversions"`
	Revenue             float64 `json:"revenue"`
	RPM                 float64 `json:"rpm"`
	EPC                 float64 `json:"epc"`
	Badge               string  `json:"badge"`
	BadgeReason         string  `json:"badge_reason"`
	Action              string  `json:"action"`
	SampleOK            bool    `json:"sample_ok"`
	AttributionCoverage float64 `json:"attribution_coverage"`
}

// HandleOfferAlignmentMatrix serves the snapshot-backed offer × ISP matrix.
//
// GET /api/mailing/offer-alignment/matrix?window=7|30 (trailing 'd' tolerated)
//
// Responses: 200 {rows, refreshed_at, staleness} · 202 {"status":"building"}
// (empty snapshot; async build kicked) · 200 {"disabled":true} (lake dark).
func (s *Server) HandleOfferAlignmentMatrix(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	win := strings.TrimSuffix(strings.TrimSpace(r.URL.Query().Get("window")), "d")
	if win == "" {
		win = "7"
	}
	days, convErr := strconv.Atoi(win)
	if convErr != nil || (days != 7 && days != 30) {
		respondError(w, http.StatusBadRequest, "window must be 7 or 30")
		return
	}
	if !analytics.ReaderEnabled() {
		// Lake dark: the matrix is delivery-led, so the whole screen is a
		// designed disabled state (profile still serves engagement-only).
		respondJSON(w, http.StatusOK, map[string]any{"disabled": true})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	rows, refreshedAt, built, err := s.readOfferAlignmentSnapshot(ctx, orgID, days)
	if err != nil {
		// Boot race: the snapshot table is created by the BACKGROUND migration
		// goroutine; a matrix hit in the first seconds after a fresh boot can
		// land before the CREATE TABLE. That is "building", not a 500 — the
		// worker's next tick populates once the table exists.
		if strings.Contains(err.Error(), "does not exist") {
			respondJSON(w, http.StatusAccepted, map[string]any{"status": "building"})
			return
		}
		respondError(w, http.StatusInternalServerError, "offer_alignment_snapshot_unavailable: "+err.Error())
		return
	}
	if !built {
		// Never refreshed for this (org, window): kick an async build, tell
		// the UI to retry. 202 is the contract's preferred building status.
		s.kickOfferAlignmentRefresh("empty-snapshot")
		respondJSON(w, http.StatusAccepted, map[string]any{"status": "building"})
		return
	}
	// built + zero data rows = a legitimately quiet window: serve the empty
	// matrix (designed empty state), never an eternal building spinner.
	if time.Since(refreshedAt) > offerAlignmentStalenessThreshold {
		s.kickOfferAlignmentRefresh("stale-snapshot")
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"rows":         rows,
		"refreshed_at": refreshedAt.UTC().Format(time.RFC3339),
		"staleness":    time.Since(refreshedAt).Truncate(time.Second).String(),
	})
}

// HandleOfferAlignmentRefresh forces a snapshot rebuild.
//
// POST /api/mailing/offer-alignment/refresh → 202 (fire-and-forget; the
// refresh mutex serialises concurrent rebuilds).
func (s *Server) HandleOfferAlignmentRefresh(w http.ResponseWriter, r *http.Request) {
	s.kickOfferAlignmentRefresh("manual")
	respondJSON(w, http.StatusAccepted, map[string]any{
		"status":    "accepted",
		"message":   "Snapshot rebuild queued — re-fetch the matrix in 1-3 minutes.",
		"queued_at": time.Now().UTC().Format(time.RFC3339),
	})
}

// ─── HTTP: offer profile ─────────────────────────────────────────────────────

// Response structs match types.ts (OfferResponse) byte-for-byte.
type offerAlignmentHeader struct {
	StatusLine  string                    `json:"status_line"`
	Attribution offerAlignmentAttribution `json:"attribution"`
	Delivered   int64                     `json:"delivered"`
	Hard        int64                     `json:"hard"`
	Soft        int64                     `json:"soft"`
	Deferred    int64                     `json:"deferred"`
	RepBlock    int64                     `json:"reputation_block"`
	HumanClicks int64                     `json:"human_clicks"`
	Clickers    int64                     `json:"clickers"`
	PGSent      int64                     `json:"pg_sent"`
	ClickerRate float64                   `json:"clicker_rate"`
	Conversions int64                     `json:"conversions"`
	Revenue     float64                   `json:"revenue"`
	RPM         float64                   `json:"rpm"`
	EPC         float64                   `json:"epc"`
}

type offerAlignmentAttribution struct {
	StampedCampaigns  int `json:"stamped_campaigns"`
	InferredCampaigns int `json:"inferred_campaigns"`
}

type offerAlignmentISPRow struct {
	ISP         string  `json:"isp"`
	Delivered   int64   `json:"delivered"`
	Hard        int64   `json:"hard"`
	Soft        int64   `json:"soft"`
	RepBlock    int64   `json:"reputation_block"`
	Deferred    int64   `json:"deferred"`
	BlockRate   float64 `json:"block_rate"`
	HumanClicks int64   `json:"human_clicks"`
	Clickers    int64   `json:"clickers"`
	PGSent      int64   `json:"pg_sent"`
	ClickerRate float64 `json:"clicker_rate"`
	Conversions int64   `json:"conversions"`
	Revenue     float64 `json:"revenue"`
	RPM         float64 `json:"rpm"`
	EPC         float64 `json:"epc"`
	Badge       string  `json:"badge"`
	BadgeReason string  `json:"badge_reason"`
	Action      string  `json:"action"`
}

type offerAlignmentCreativeRow struct {
	CreativeKey      string  `json:"creative_key"`
	Subject          string  `json:"subject"`
	FromName         string  `json:"from_name"`
	ISP              string  `json:"isp"`
	Delivered        int64   `json:"delivered"`
	Clicks           int64   `json:"clicks"`
	Clickers         int64   `json:"clickers"`
	ClickerRate      float64 `json:"clicker_rate"`
	Conversions      int64   `json:"conversions"`
	Revenue          float64 `json:"revenue"`
	SampleCampaignID string  `json:"sample_campaign_id"`
	HasHTML          bool    `json:"has_html"`
	Inferred         bool    `json:"inferred"`
}

type offerAlignmentDataSourceRow struct {
	DataSource  *string `json:"data_source"` // null ⇒ "(unattributed)" in the UI
	Delivered   int64   `json:"delivered"`
	Hard        int64   `json:"hard"`
	InvalidRate float64 `json:"invalid_rate"`
	Clicks      int64   `json:"clicks"`
	Clickers    int64   `json:"clickers"`
	Conversions int64   `json:"conversions"`
}

// parseAlignmentRange parses from/to (YYYY-MM-DD, America/Denver days) with a
// default of the trailing 7 Denver days. Returns the PG bounds and the lake
// dt strings.
func parseAlignmentRange(r *http.Request) (from, to time.Time, fromDt, toDt string, err error) {
	now := time.Now().In(mstLoc)
	fromDt = strings.TrimSpace(r.URL.Query().Get("from"))
	toDt = strings.TrimSpace(r.URL.Query().Get("to"))
	if toDt == "" {
		toDt = now.Format("2006-01-02")
	}
	if fromDt == "" {
		fromDt = now.AddDate(0, 0, -6).Format("2006-01-02")
	}
	f, e1 := time.ParseInLocation("2006-01-02", fromDt, mstLoc)
	t, e2 := time.ParseInLocation("2006-01-02", toDt, mstLoc)
	if e1 != nil || e2 != nil {
		return time.Time{}, time.Time{}, "", "", fmt.Errorf("from/to must be YYYY-MM-DD")
	}
	if fromDt > toDt {
		return time.Time{}, time.Time{}, "", "", fmt.Errorf("from %s is after to %s", fromDt, toDt)
	}
	from = f
	to = time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, mstLoc)
	return from, to, fromDt, toDt, nil
}

// HandleOfferAlignmentOffer serves the live per-offer profile: header KPIs,
// per-ISP rows (delivery from the lake, engagement from PG), creative ×
// subject × ISP panel, and the data-source panel.
//
// GET /api/mailing/offer-alignment/offer?offer=<key>&from=&to=[&sending_domain=]
func (s *Server) HandleOfferAlignmentOffer(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	offer := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("offer")))
	if offer == "" {
		respondError(w, http.StatusBadRequest, "offer is required")
		return
	}
	sendingDomain := strings.TrimSpace(r.URL.Query().Get("sending_domain"))
	from, to, fromDt, toDt, err := parseAlignmentRange(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), offerAlignmentQueryTimeout)
	defer cancel()

	// Heavy scans run on a dedicated session with a 60s statement_timeout
	// (pool default 30s dies cold/under load on drip-scale sets). Falls back
	// to the pool if the conn can't be prepared.
	var adb alignmentDB = s.mailingDB
	if conn, release, connErr := alignmentSessionConn(ctx, s.mailingDB, 60000); connErr == nil {
		adb = conn
		defer release()
	} else {
		log.Printf("[offer-alignment] session conn unavailable, using pool: %v", connErr)
	}

	slugs, efids, offerName, err := resolveOfferSlugsForKey(ctx, adb, offer)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	patterns := make([]string, len(slugs))
	for i, sl := range slugs {
		patterns[i] = "%/" + sl + "/%"
	}
	if offerName == "" {
		offerName = offer
	}

	set, err := resolveOfferCampaignSet(ctx, adb, orgID, offer, offerName, patterns, from, to, sendingDomain)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	notes := []string{}
	if len(set.IDs) == 0 {
		respondJSON(w, http.StatusOK, map[string]any{
			"header": offerAlignmentHeader{
				StatusLine:  fmt.Sprintf("%s: no campaigns found in %s..%s", offerName, fromDt, toDt),
				Attribution: offerAlignmentAttribution{},
			},
			"isp_rows":     []offerAlignmentISPRow{},
			"creatives":    []offerAlignmentCreativeRow{},
			"data_sources": []offerAlignmentDataSourceRow{},
			"notes":        notes,
		})
		return
	}

	// ── delivery (lake truth) ────────────────────────────────────────────
	deliveryByISP := map[string]*alignmentDelivery{}
	dsnByISP := map[string]map[string]int64{}
	lakeOK := analytics.ReaderEnabled()
	if lakeOK {
		ids := set.IDs
		dRows, dsnRows, lerr := fetchAlignmentLakeRows(ctx, ids, fromDt, toDt)
		if lerr != nil {
			respondError(w, http.StatusInternalServerError, "lake delivery query: "+lerr.Error())
			return
		}
		deliveryByISP = aggregateAlignmentDelivery(dRows, "")
		dsnByISP = aggregateAlignmentDSN(dsnRows, "")
	} else {
		notes = append(notes, "lake read disabled — delivery fields are zero; engagement-only view")
	}

	// ── engagement + conversions (PG) ────────────────────────────────────
	engByISP, err := fetchAlignmentEngagement(ctx, adb, orgID, set.IDs, from, to)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	convByISP, err := fetchAlignmentConversionsByISP(ctx, adb, orgID, efids, from, to)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The conversion ledgers carry no dollars (see resolveOfferConversionPrice)
	// — price zero-revenue buckets at the offer's deal-eCPA/payout estimate.
	convPrice := resolveOfferConversionPrice(ctx, adb, orgID, efids)
	applyEstimatedConversionRevenue(convByISP, convPrice)
	if convPrice > 0 {
		notes = append(notes, fmt.Sprintf("revenue estimated at $%.2f/conversion (deal eCPA goal / offer payout) — conversion ledgers carry no dollars", convPrice))
	}

	// ── per-ISP rows (union of every source's ISPs) ──────────────────────
	ispSet := map[string]bool{}
	for k := range deliveryByISP {
		ispSet[k] = true
	}
	for k := range engByISP {
		ispSet[k] = true
	}
	for k := range convByISP {
		ispSet[k] = true
	}
	ispRows := make([]offerAlignmentISPRow, 0, len(ispSet))
	var hdr offerAlignmentHeader
	for isp := range ispSet {
		d := deliveryByISP[isp]
		if d == nil {
			d = &alignmentDelivery{}
		}
		e := engByISP[isp]
		if e == nil {
			e = &alignmentEngagement{}
		}
		cv := convByISP[isp]
		if cv == nil {
			cv = &alignmentConversions{}
		}
		fams := dsnByISP[isp]
		stats := alignmentCellStats{
			Delivered: d.Delivered, Hard: d.Hard, Soft: d.Soft,
			ReputationBlock: d.ReputationBlock, Deferred: d.Deferred,
			BlockingDSN: fams["HM08"] + fams["HCM2"] + fams["S3140"],
			HM08:        fams["HM08"],
			Hard511:     fams["5.1.1"],
			HumanClicks: e.HumanClicks, Clickers: e.Clickers, PGSent: e.PGSent,
			Revenue: cv.Revenue,
		}
		badge, reason, action, _ := classifyAlignmentCell(stats)
		if !lakeOK {
			badge, reason, action = "NO_LAKE", "lake read disabled — delivery health unknown", ""
		}
		row := offerAlignmentISPRow{
			ISP: isp, Delivered: d.Delivered, Hard: d.Hard, Soft: d.Soft,
			RepBlock: d.ReputationBlock, Deferred: d.Deferred,
			BlockRate:   alignmentDiv(float64(d.ReputationBlock), float64(d.attempted())),
			HumanClicks: e.HumanClicks, Clickers: e.Clickers, PGSent: e.PGSent,
			ClickerRate: alignmentDiv(float64(e.Clickers), float64(e.PGSent)),
			Conversions: cv.Conversions, Revenue: cv.Revenue,
			RPM:   alignmentDiv(cv.Revenue*1000, float64(d.Delivered)),
			EPC:   alignmentDiv(cv.Revenue, float64(e.HumanClicks)),
			Badge: badge, BadgeReason: reason, Action: action,
		}
		ispRows = append(ispRows, row)
		hdr.Delivered += d.Delivered
		hdr.Hard += d.Hard
		hdr.Soft += d.Soft
		hdr.Deferred += d.Deferred
		hdr.RepBlock += d.ReputationBlock
		hdr.HumanClicks += e.HumanClicks
		hdr.Clickers += e.Clickers
		hdr.PGSent += e.PGSent
		hdr.Conversions += cv.Conversions
		hdr.Revenue += cv.Revenue
	}
	sort.Slice(ispRows, func(i, j int) bool { return ispRows[i].Delivered > ispRows[j].Delivered })

	var pgSentTotal int64
	for _, e := range engByISP {
		pgSentTotal += e.PGSent
	}
	hdr.ClickerRate = alignmentDiv(float64(hdr.Clickers), float64(pgSentTotal))
	hdr.RPM = alignmentDiv(hdr.Revenue*1000, float64(hdr.Delivered))
	hdr.EPC = alignmentDiv(hdr.Revenue, float64(hdr.HumanClicks))
	hdr.Attribution = offerAlignmentAttribution{StampedCampaigns: set.Stamped, InferredCampaigns: set.Inferred}
	worst := ""
	for _, row := range ispRows {
		if row.Badge == "BLOCKING" {
			worst = "BLOCKING on " + row.ISP
			break
		}
	}
	hdr.StatusLine = fmt.Sprintf("%s: %d campaigns (%d stamped, %d inferred) · %s delivered · clicker rate %.2f%%",
		offerName, set.Stamped+set.Inferred, set.Stamped, set.Inferred,
		strconv.FormatInt(hdr.Delivered, 10), hdr.ClickerRate*100)
	if worst != "" {
		hdr.StatusLine += " · " + worst
	}

	// ── creative × subject × ISP panel ───────────────────────────────────
	creatives, err := s.fetchAlignmentCreatives(ctx, adb, orgID, set.IDs, patterns, efids, from, to)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for i := range creatives {
		if creatives[i].Revenue == 0 && creatives[i].Conversions > 0 && convPrice > 0 {
			creatives[i].Revenue = convPrice * float64(creatives[i].Conversions)
		}
	}

	// ── data-source panel ────────────────────────────────────────────────
	dataSources, err := s.fetchAlignmentDataSources(ctx, adb, orgID, set.IDs, efids, from, to)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"header":       hdr,
		"isp_rows":     ispRows,
		"creatives":    creatives,
		"data_sources": dataSources,
		"notes":        notes,
	})
}

// fetchAlignmentCreatives is the creativesBreakdown CTE shape
// (handlers_analytics_creatives.go) recut by ISP-of-recipient instead of
// sending_domain and scoped to the offer's campaign set. Delivered here is
// the PG sent-event count per creative × ISP (the contract's per-campaign
// PG delivery proxy — the lake cannot key delivery by creative), so
// clicker_rate stays a same-store fraction. Conversions re-attribute via the
// LATERAL most-recent in-window money-link click. inferred=true when NO
// campaign in the group carries a stamped offer_key.
func (s *Server) fetchAlignmentCreatives(
	ctx context.Context, db alignmentDB, orgID interface{}, ids, patterns, efids []string, from, to time.Time,
) ([]offerAlignmentCreativeRow, error) {
	out := []offerAlignmentCreativeRow{}
	if len(ids) == 0 {
		return out, nil
	}
	ispClick := alignmentISPCase("COALESCE(t.recipient_domain,'')")

	// Conversion-ledger offer scoping — same convention as creativesBreakdown:
	// only when everflow ids are known. Conversion CTEs (and their LATERAL,
	// which needs the slug patterns) are emitted only when BOTH efids and
	// patterns exist; otherwise conversions are 0 per row.
	// $1 org, $2 from, $3 to, $4 campaign ids, $5 row limit,
	// $6 slug patterns (only when conversions are scoped), $7 everflow ids.
	args := []interface{}{orgID, from, to, pq.Array(ids), creativesAnalyticsRowLimit + 1}
	convCTE := ""
	convJoin := ""
	convKeyExpr := "COALESCE(cg.creative_key, sg.creative_key, 'no-html')"
	convSubjExpr := "COALESCE(cg.subject, sg.subject, '')"
	convISPExpr := "COALESCE(cg.isp, sg.isp, 'other')"
	convCountExpr := "0::bigint"
	convRevExpr := "0::float8"
	if len(efids) > 0 && len(patterns) > 0 {
		args = append(args, pq.Array(patterns), pq.Array(efids))
		convCTE = `,
		conv AS (
			SELECT s.subscriber_id, s.suppressed_at AS converted_at, 0::numeric AS revenue
			FROM mailing_offer_suppressions s
			WHERE s.organization_id = $1
			  AND s.reason = 'converted'
			  AND s.suppressed_at >= $2 AND s.suppressed_at <= $3
			  AND s.offer_id IN (SELECT id FROM mailing_offers WHERE organization_id = $1 AND everflow_offer_id = ANY($7))
			UNION ALL
			SELECT cm.sub1::uuid AS subscriber_id, cm.converted_at, COALESCE(cm.revenue, 0)::numeric AS revenue
			FROM mailing_cpm_manual_conversions cm
			WHERE cm.organization_id = $1
			  AND cm.converted_at >= $2 AND cm.converted_at <= $3
			  AND cm.sub1 ~ '^[0-9a-f-]{36}$'
			  AND cm.deal_id IN (SELECT id FROM mailing_cpm_deals WHERE organization_id = $1 AND (everflow_offer_id = ANY($7) OR offer_id IN (SELECT id FROM mailing_offers WHERE organization_id = $1 AND everflow_offer_id = ANY($7))))
		),
		conv_attr AS (
			SELECT cv.revenue, att.creative_key, att.subject, att.isp
			FROM conv cv
			LEFT JOIN LATERAL (
				SELECT d.creative_key,
				       d.subject,
				       ` + ispClick + ` AS isp
				FROM mailing_tracking_events t
				JOIN camp_dim d ON d.id = t.campaign_id
				WHERE t.organization_id = $1
				  AND t.subscriber_id = cv.subscriber_id
				  AND t.campaign_id = ANY($4::uuid[])
				  AND t.event_type = 'clicked'
				  AND t.link_url ILIKE '%source_id=email%'
				  AND t.link_url ILIKE ANY($6)
				  AND t.event_at <= cv.converted_at
				  AND t.event_at >= cv.converted_at - INTERVAL '90 days'
				ORDER BY t.event_at DESC
				LIMIT 1
			) att ON TRUE
		),
		conv_grp AS (
			SELECT creative_key, subject, isp,
			       COUNT(*)     AS conversions,
			       SUM(revenue) AS revenue
			FROM conv_attr
			WHERE creative_key IS NOT NULL
			GROUP BY creative_key, subject, isp
		)`
		convJoin = `FULL OUTER JOIN conv_grp cv
		  ON cv.creative_key = COALESCE(cg.creative_key, sg.creative_key)
		 AND cv.subject = COALESCE(cg.subject, sg.subject)
		 AND cv.isp = COALESCE(cg.isp, sg.isp)`
		convKeyExpr = "COALESCE(cg.creative_key, sg.creative_key, cv.creative_key, 'no-html')"
		convSubjExpr = "COALESCE(cg.subject, sg.subject, cv.subject, '')"
		convISPExpr = "COALESCE(cg.isp, sg.isp, cv.isp, 'other')"
		convCountExpr = "COALESCE(cv.conversions, 0)"
		convRevExpr = "COALESCE(cv.revenue, 0)::float8"
	}

	// Aggregation pushdown (2026-07-07 prod diagnosis, ps8241 = 23,722
	// campaigns / 2.99M sent events per 7d): events are counted per
	// (campaign_id, isp) FIRST, and creative identity comes from camp_dim —
	// ONE md5(html_content) per campaign (MATERIALIZED so the planner can't
	// inline it back to per-event; drip campaigns have NULL html → free).
	// The per-event-row md5 variant of sent_grp measured ~12s alone and blew
	// the prod 30s statement_timeout.
	// Trade-off: clickers = per-campaign DISTINCT summed across a creative's
	// campaigns, so a subscriber clicking the same creative in two campaigns
	// counts twice (within-campaign repeat clicks still dedup — the dominant
	// case). Exact cross-campaign dedup would reintroduce the per-event join.
	q := `
		WITH camp_dim AS MATERIALIZED (
			SELECT c.id,
			       COALESCE(md5(c.html_content), 'no-html') AS creative_key,
			       c.subject,
			       c.from_name,
			       c.html_content IS NOT NULL AS has_html,
			       COALESCE(c.offer_key,'') <> '' AS stamped
			FROM mailing_campaigns c
			WHERE c.organization_id = $1 AND c.id = ANY($4::uuid[])
		),
		clicks_by_campaign AS (
			SELECT t.campaign_id, ` + ispClick + ` AS isp,
			       COUNT(*) AS clicks,
			       COUNT(DISTINCT t.subscriber_id) AS clickers
			FROM mailing_tracking_events t
			WHERE t.organization_id = $1
			  AND t.campaign_id = ANY($4::uuid[])
			  AND t.event_at >= $2 AND t.event_at <= $3
			  AND t.event_type = 'clicked'
			  AND t.link_url ILIKE '%source_id=email%'
			GROUP BY 1, 2
		),
		clicks_grp AS (
			SELECT d.creative_key,
			       d.subject,
			       cb.isp,
			       SUM(cb.clicks)::bigint   AS clicks,
			       SUM(cb.clickers)::bigint AS clickers,
			       min(d.id::text)          AS sample_campaign_id,
			       min(d.from_name)         AS from_name,
			       bool_or(d.has_html)      AS has_html,
			       bool_or(d.stamped)       AS any_stamped
			FROM clicks_by_campaign cb
			JOIN camp_dim d ON d.id = cb.campaign_id
			GROUP BY 1, d.subject, cb.isp
		),
		sent_by_campaign AS (
			SELECT t.campaign_id, ` + ispClick + ` AS isp, COUNT(*) AS delivered
			FROM mailing_tracking_events t
			WHERE t.organization_id = $1
			  AND t.campaign_id = ANY($4::uuid[])
			  AND t.event_at >= $2 AND t.event_at <= $3
			  AND t.event_type = 'sent'
			GROUP BY 1, 2
		),
		sent_grp AS (
			SELECT d.creative_key,
			       d.subject,
			       sb.isp,
			       SUM(sb.delivered)::bigint AS delivered,
			       bool_or(d.stamped)        AS any_stamped
			FROM sent_by_campaign sb
			JOIN camp_dim d ON d.id = sb.campaign_id
			GROUP BY 1, d.subject, sb.isp
		)` + convCTE + `
		SELECT ` + convKeyExpr + ` AS creative_key,
		       ` + convSubjExpr + ` AS subject,
		       ` + convISPExpr + ` AS isp,
		       COALESCE(sg.delivered, 0)                             AS delivered,
		       COALESCE(cg.clicks, 0)                                AS clicks,
		       COALESCE(cg.clickers, 0)                              AS clickers,
		       ` + convCountExpr + `                                 AS conversions,
		       ` + convRevExpr + `                                   AS revenue,
		       cg.sample_campaign_id                                 AS sample_campaign_id,
		       cg.from_name                                          AS from_name,
		       COALESCE(cg.has_html, false)                          AS has_html,
		       NOT COALESCE(cg.any_stamped, sg.any_stamped, false)   AS inferred
		FROM clicks_grp cg
		FULL OUTER JOIN sent_grp sg
		  ON sg.creative_key = cg.creative_key AND sg.subject = cg.subject AND sg.isp = cg.isp
		` + convJoin + `
		ORDER BY clicks DESC, delivered DESC
		LIMIT $5
	`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("alignment creatives: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			row              offerAlignmentCreativeRow
			sampleCampaignID sql.NullString
			fromName         sql.NullString
		)
		if err := rows.Scan(&row.CreativeKey, &row.Subject, &row.ISP, &row.Delivered,
			&row.Clicks, &row.Clickers, &row.Conversions, &row.Revenue,
			&sampleCampaignID, &fromName, &row.HasHTML, &row.Inferred); err != nil {
			return nil, err
		}
		row.SampleCampaignID = sampleCampaignID.String
		row.FromName = fromName.String
		row.ClickerRate = alignmentDiv(float64(row.Clickers), float64(row.Delivered))
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > creativesAnalyticsRowLimit {
		out = out[:creativesAnalyticsRowLimit]
	}
	return out, nil
}

// fetchAlignmentDataSources builds the per-data-source panel: PG sent
// (delivered proxy), hard bounces, human money clicks/clickers via the
// subscriber join, plus offer-scoped conversions per the converting
// subscriber's data_source. NULL/'' data_source is emitted as JSON null
// (the UI renders "(unattributed)"). invalid_rate = hard/(delivered+hard),
// PG scope.
func (s *Server) fetchAlignmentDataSources(
	ctx context.Context, db alignmentDB, orgID interface{}, ids, efids []string, from, to time.Time,
) ([]offerAlignmentDataSourceRow, error) {
	out := []offerAlignmentDataSourceRow{}
	if len(ids) == 0 {
		return out, nil
	}
	// $1 org, $2 campaign ids, $3 from, $4 to.
	const q = `
		SELECT COALESCE(NULLIF(sub.data_source,''),'') AS data_source,
		       COUNT(*) FILTER (WHERE t.event_type = 'sent') AS delivered,
		       COUNT(*) FILTER (WHERE t.event_type = 'bounced' AND t.bounce_type = 'hard') AS hard,
		       COUNT(*) FILTER (WHERE t.event_type = 'clicked'
		         AND t.link_url ILIKE '%source_id=email%'
		         AND ignite_verdict_is_human(COALESCE(t.click_verdict, ignite_event_verdict(t.user_agent, t.ip_address)))) AS clicks,
		       COUNT(DISTINCT t.subscriber_id) FILTER (WHERE t.event_type = 'clicked'
		         AND t.link_url ILIKE '%source_id=email%'
		         AND ignite_verdict_is_human(COALESCE(t.click_verdict, ignite_event_verdict(t.user_agent, t.ip_address)))) AS clickers
		FROM mailing_tracking_events t
		JOIN mailing_subscribers sub ON sub.id = t.subscriber_id AND sub.organization_id = $1
		WHERE t.organization_id = $1
		  AND t.campaign_id = ANY($2::uuid[])
		  AND t.event_at >= $3 AND t.event_at <= $4
		GROUP BY 1
	`
	// Chunked like fetchAlignmentEngagement — drip-scale offers hand this an
	// id set in the tens of thousands; one statement over the full set risks
	// the prod 30s statement_timeout. Counts sum exactly across chunks;
	// clickers (per-chunk DISTINCT summed) can double-count a subscriber
	// clicking in campaigns that landed in different chunks — second-order.
	byKey := map[string]*offerAlignmentDataSourceRow{}
	for start := 0; start < len(ids); start += alignmentEngagementChunk {
		end := start + alignmentEngagementChunk
		if end > len(ids) {
			end = len(ids)
		}
		rows, err := db.QueryContext(ctx, q, orgID, pq.Array(ids[start:end]), from, to)
		if err != nil {
			return nil, fmt.Errorf("alignment data sources (chunk %d): %w", start/alignmentEngagementChunk, err)
		}
		for rows.Next() {
			var key string
			var delivered, hard, clicks, clickers int64
			if err := rows.Scan(&key, &delivered, &hard, &clicks, &clickers); err != nil {
				rows.Close()
				return nil, err
			}
			row := byKey[key]
			if row == nil {
				row = &offerAlignmentDataSourceRow{}
				if key != "" {
					ds := key
					row.DataSource = &ds
				}
				byKey[key] = row
			}
			row.Delivered += delivered
			row.Hard += hard
			row.Clicks += clicks
			row.Clickers += clickers
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	// Conversions per data_source (offer-scoped conv UNION → subscriber join).
	if len(efids) > 0 {
		// $1 org, $2 from, $3 to, $4 everflow ids.
		const cq = `
			WITH conv AS (
				SELECT s.subscriber_id
				FROM mailing_offer_suppressions s
				WHERE s.organization_id = $1
				  AND s.reason = 'converted'
				  AND s.suppressed_at >= $2 AND s.suppressed_at <= $3
				  AND s.offer_id IN (SELECT id FROM mailing_offers WHERE organization_id = $1 AND everflow_offer_id = ANY($4))
				UNION ALL
				SELECT cm.sub1::uuid AS subscriber_id
				FROM mailing_cpm_manual_conversions cm
				WHERE cm.organization_id = $1
				  AND cm.converted_at >= $2 AND cm.converted_at <= $3
				  AND cm.sub1 ~ '^[0-9a-f-]{36}$'
				  AND cm.deal_id IN (SELECT id FROM mailing_cpm_deals WHERE organization_id = $1 AND (everflow_offer_id = ANY($4) OR offer_id IN (SELECT id FROM mailing_offers WHERE organization_id = $1 AND everflow_offer_id = ANY($4))))
			)
			SELECT COALESCE(NULLIF(sub.data_source,''),'') AS data_source, COUNT(*) AS conversions
			FROM conv cv
			JOIN mailing_subscribers sub ON sub.id = cv.subscriber_id AND sub.organization_id = $1
			GROUP BY 1
		`
		crows, err := db.QueryContext(ctx, cq, orgID, from, to, pq.Array(efids))
		if err != nil {
			return nil, fmt.Errorf("alignment data-source conversions: %w", err)
		}
		for crows.Next() {
			var key string
			var n int64
			if err := crows.Scan(&key, &n); err != nil {
				crows.Close()
				return nil, err
			}
			row := byKey[key]
			if row == nil {
				row = &offerAlignmentDataSourceRow{}
				if key != "" {
					ds := key
					row.DataSource = &ds
				}
				byKey[key] = row
			}
			row.Conversions = n
		}
		crows.Close()
		if err := crows.Err(); err != nil {
			return nil, err
		}
	}

	for _, row := range byKey {
		row.InvalidRate = alignmentDiv(float64(row.Hard), float64(row.Delivered+row.Hard))
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Delivered > out[j].Delivered })
	return out, nil
}

// ─── HTTP: evidence ──────────────────────────────────────────────────────────

type offerAlignmentEvidenceRow struct {
	DSNFamily  string `json:"dsn_family"`
	DSNCode    string `json:"dsn_code"`
	Count      int64  `json:"count"`
	FirstSeen  string `json:"first_seen"`
	LastSeen   string `json:"last_seen"`
	SampleDiag string `json:"sample_diag"`
	Meaning    string `json:"meaning"`
}

// HandleOfferAlignmentEvidence returns the grouped diagnostic evidence for
// one offer × ISP: dsn_family/dsn_code counts + first/last seen Denver days +
// the highest-volume sample diagnostic per family, with the plain-English
// decode from alignmentDSNMeanings.
//
// GET /api/mailing/offer-alignment/evidence?offer=<key>&isp=<bucket>&from=&to=
func (s *Server) HandleOfferAlignmentEvidence(w http.ResponseWriter, r *http.Request) {
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	offer := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("offer")))
	if offer == "" {
		respondError(w, http.StatusBadRequest, "offer is required")
		return
	}
	ispBucket := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("isp")))
	if ispBucket == "" {
		respondError(w, http.StatusBadRequest, "isp is required")
		return
	}
	if !alignmentISPBucketRe.MatchString(ispBucket) {
		respondError(w, http.StatusBadRequest, "isp must be a clean-classifier bucket (e.g. gmail, microsoft, apple)")
		return
	}
	from, to, fromDt, toDt, err := parseAlignmentRange(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !analytics.ReaderEnabled() {
		respondJSON(w, http.StatusOK, map[string]any{"disabled": true})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), offerAlignmentQueryTimeout)
	defer cancel()

	var adb alignmentDB = s.mailingDB
	if conn, release, connErr := alignmentSessionConn(ctx, s.mailingDB, 60000); connErr == nil {
		adb = conn
		defer release()
	}
	slugs, _, offerName, err := resolveOfferSlugsForKey(ctx, adb, offer)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	patterns := make([]string, len(slugs))
	for i, sl := range slugs {
		patterns[i] = "%/" + sl + "/%"
	}
	set, err := resolveOfferCampaignSet(ctx, adb, orgID, offer, offerName, patterns, from, to, "")
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(set.IDs) == 0 {
		respondJSON(w, http.StatusOK, map[string]any{"rows": []offerAlignmentEvidenceRow{}})
		return
	}
	ids := set.IDs

	// Breakdown 1 — counts + first/last seen per (family, code):
	// event_type × dsn_family × dsn_code × local_dt (failure types kept Go-side).
	countRows, err := analytics.Breakdown(ctx, analytics.BreakdownFilter{
		From: fromDt, To: toDt,
		GroupBy:     []string{"event_type", "dsn_family", "dsn_code", "local_dt"},
		Eq:          map[string]string{"isp": ispBucket},
		SourceIn:    []string{"pmta", "ses"},
		CampaignIDs: ids,
		Limit:       5000,
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "lake evidence query: "+err.Error())
		return
	}
	// Breakdown 2 — top sample diagnostic per (family, code):
	// event_type × dsn_family × dsn_code × dsn_diag, ordered by count desc.
	diagRows, err := analytics.Breakdown(ctx, analytics.BreakdownFilter{
		From: fromDt, To: toDt,
		GroupBy:     []string{"event_type", "dsn_family", "dsn_code", "dsn_diag"},
		Eq:          map[string]string{"isp": ispBucket},
		SourceIn:    []string{"pmta", "ses"},
		CampaignIDs: ids,
		Limit:       2000,
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "lake evidence sample query: "+err.Error())
		return
	}

	type key struct{ fam, code string }
	agg := map[key]*offerAlignmentEvidenceRow{}
	for _, row := range countRows {
		if !alignmentFailureEventTypes[row.Keys["event_type"]] {
			continue
		}
		k := key{row.Keys["dsn_family"], row.Keys["dsn_code"]}
		e := agg[k]
		if e == nil {
			e = &offerAlignmentEvidenceRow{DSNFamily: k.fam, DSNCode: k.code, Meaning: alignmentDSNMeaning(k.fam)}
			agg[k] = e
		}
		e.Count += row.Count
		dt := row.Keys["local_dt"]
		if e.FirstSeen == "" || dt < e.FirstSeen {
			e.FirstSeen = dt
		}
		if dt > e.LastSeen {
			e.LastSeen = dt
		}
	}
	// Sample diag: breakdown rows arrive count-desc, so the first diag seen
	// for a (family, code) is its highest-volume diagnostic.
	for _, row := range diagRows {
		if !alignmentFailureEventTypes[row.Keys["event_type"]] {
			continue
		}
		k := key{row.Keys["dsn_family"], row.Keys["dsn_code"]}
		if e, ok := agg[k]; ok && e.SampleDiag == "" {
			e.SampleDiag = row.Keys["dsn_diag"]
		}
	}

	rows := make([]offerAlignmentEvidenceRow, 0, len(agg))
	for _, e := range agg {
		rows = append(rows, *e)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Count > rows[j].Count })
	respondJSON(w, http.StatusOK, map[string]any{"rows": rows})
}
