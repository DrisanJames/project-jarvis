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
//   - Delivery cells come from the Athena lake, source IN ('pmta','ses','kumo'),
//     COUNT(DISTINCT event_uid), scoped by the offer's campaign-ID set.
//     NEVER PG campaign counters.
//   - Engagement comes from the mailing_offer_alignment_clicks rollup: human
//     verdict, money clicks only. clicker_rate divides by lake delivered.
//   - Conversions and revenue come from mailing_everflow_conversions by each
//     conversion's own campaign_id. Unknown revenue is unknown, never $0.
//
// Offer → campaign-set attribution: the key is mailing_campaigns.offer_id.
// Campaigns without one fall back to stamped offer_key, then to inference
// from the campaign-name " - <offer>" suffix and slug-anchored money clicks.
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
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
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
	alignmentMismatchMinDelivered   = 1000  // ...but only with a real engagement sample
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
	if isOfferIDKey(offer) {
		err = db.QueryRowContext(ctx, `SELECT COALESCE(name, '') FROM mailing_offers WHERE id = $1::uuid`, offer).Scan(&offerName)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, "", nil
		}
		return nil, nil, offerName, err
	}
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

// isOfferIDKey reports whether an offer key is a mailing_offers id. Those are
// the keys the matrix uses for every campaign carrying offer_id (most carry no
// offer_key: 18,677 of 24,600 over the 14 days to 2026-10-31).
func isOfferIDKey(key string) bool {
	if len(key) != 36 {
		return false
	}
	_, err := uuid.Parse(key)
	return err == nil
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

	// An offer id is exact: every campaign carrying it, nothing inferred.
	if isOfferIDKey(offerKey) {
		rows, err := db.QueryContext(ctx, `
			SELECT id::text FROM mailing_campaigns
			WHERE organization_id = $1
			  AND offer_id = $2::uuid
			  AND scheduled_at >= $3 AND scheduled_at <= $4
			  AND ($5 = '' OR lower(split_part(from_email,'@',2)) = lower($5))
			ORDER BY scheduled_at DESC
		`, orgID, offerKey, from, to, sendingDomain)
		if err != nil {
			return set, fmt.Errorf("offer campaign set: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return set, err
			}
			set.IDs = append(set.IDs, id)
			set.Stamped++
		}
		return set, rows.Err()
	}

	// (0) stamped: forward data, exact by construction.
	// $1 org, $2 key, $3 from, $4 to, $5 sending-domain ('' = all).
	const stampedQ = `
		SELECT id::text FROM mailing_campaigns
		WHERE organization_id = $1
		  AND lower(COALESCE(offer_key,'')) = lower($2)
		  AND offer_id IS NULL
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
		  AND offer_id IS NULL
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
	// the offer has no slug patterns, and when stamped campaigns already
	// define it: this scan times out on prod and dropped insurhomev1 (3,095
	// campaigns) from a refresh.
	if len(patterns) > 0 && set.Stamped == 0 {
		// $1 org, $2 from, $3 to, $4 slug patterns, $5 domain.
		const clickQ = `
			SELECT DISTINCT t.campaign_id::text
			FROM mailing_tracking_events t
			JOIN mailing_campaigns c
			  ON c.id = t.campaign_id AND c.organization_id = $1
			WHERE t.organization_id = $1
			  AND COALESCE(c.offer_key,'') = ''
			  AND c.offer_id IS NULL
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
// dsn_family). local_dt rides along so a 14-day fetch can be sliced into the
// narrower windows without a second Athena round trip.
const alignmentLakeChunk = 2000

// alignmentLakeSources is every transport that carries delivery rows. kumo
// offers read as zero delivered (and $0 RPM) without it.
var alignmentLakeSources = []string{"pmta", "ses", "kumo"}

// alignmentLakeConcurrency bounds parallel Athena calls per fetch. Serial
// chunking put drip-scale profiles (10 chunks x 2 calls x ~5s) past the
// handler budget; Athena executes concurrent queries happily. 4 keeps a
// single fetch from monopolizing the workgroup while refresh + profile runs
// overlap.
const alignmentLakeConcurrency = 4

func fetchAlignmentLakeRows(ctx context.Context, ids []string, fromDt, toDt string) (delivery, dsn []analytics.BreakdownRow, err error) {
	type job struct {
		dsnPass bool
		chunk   []string
	}
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
				SourceIn:    alignmentLakeSources,
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

// fetchAlignmentLakeClicks aggregates ATHENA navigational clicks + distinct
// clickers per ISP for a campaign set (operator 2026-08-01, non-CPM funnel).
// Chunked at alignmentLakeChunk like fetchAlignmentLakeRows because the reader
// caps a single IN-list at 2000 ids.
//
// Chunking caveat, stated rather than hidden: click COUNTS sum exactly across
// disjoint chunks, but per-chunk DISTINCT CLICKERS sum with double-counting
// when one subscriber clicked campaigns that landed in different chunks. This
// only bites offers with >2000 campaigns in the window (the drip-scale ones);
// it is the same second-order caveat fetchAlignmentEngagement carries, and it
// biases clickers UP, never down.
// Chunks run CONCURRENTLY at alignmentLakeConcurrency, mirroring
// fetchAlignmentLakeRows. Serial chunking measured on prod 2026-08-01 cost ~80s
// for one drip-scale offer's 12 chunks, ×2 windows ×25 offers — enough added
// latency to threaten the 30m offerAlignmentRefreshTimeout. Athena runs
// concurrent queries happily; chunks are disjoint so the merge is order-free.
func fetchAlignmentLakeClicks(ctx context.Context, ids []string, fromDt, toDt string) (map[string]*analytics.ActionClickRow, error) {
	out := map[string]*analytics.ActionClickRow{}
	if len(ids) == 0 {
		return out, nil
	}
	chunks := [][]string{}
	for start := 0; start < len(ids); start += alignmentLakeChunk {
		end := start + alignmentLakeChunk
		if end > len(ids) {
			end = len(ids)
		}
		chunks = append(chunks, ids[start:end])
	}

	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	sem := make(chan struct{}, alignmentLakeConcurrency)
	for i, chunk := range chunks {
		wg.Add(1)
		go func(i int, chunk []string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			mu.Lock()
			done := firstErr != nil || ctx.Err() != nil
			mu.Unlock()
			if done {
				return
			}
			rows, err := analytics.ActionClicks(ctx, analytics.ActionClickFilter{
				From: fromDt, To: toDt, CampaignIDs: chunk, Limit: 5000,
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("lake action clicks (chunk %d): %w", i, err)
				}
				return
			}
			for _, r := range rows {
				cell := out[r.ISP]
				if cell == nil {
					cell = &analytics.ActionClickRow{ISP: r.ISP}
					out[r.ISP] = cell
				}
				cell.Clickers += r.Clickers
				cell.Clicks += r.Clicks
			}
		}(i, chunk)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if len(chunks) > 1 {
		log.Printf("[offer-alignment] lake clicks over %d campaigns in %d chunks — clickers may double-count across chunks",
			len(ids), len(chunks))
	}
	return out, nil
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
	if c.Delivered > 0 {
		clickerRate = float64(c.Clickers) / float64(c.Delivered)
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
		case c.Delivered >= alignmentMismatchMinDelivered && clickerRate < alignmentMismatchClickerRate:
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

// alignmentEngagement is the engagement cell: human money clicks and distinct
// clickers, per ISP bucket.
type alignmentEngagement struct {
	HumanClicks int64
	Clickers    int64
}

// alignmentEngagementChunk bounds the campaign-id set per sent/bounce query in
// the data-source panel.
const alignmentEngagementChunk = 8000

// alignmentMoneyClickPredicate matches an offer click on an events row aliased
// t: the /o/ gateway links every current creative uses, plus legacy cratoolpro
// links. source_id=email alone misses /o/ and read 12 human clicks estate-wide.
const alignmentMoneyClickPredicate = `(t.link_url ~* '^https?://[^/]+/o/' OR t.link_url ILIKE '%source_id=email%')`

// fetchAlignmentEngagement reads human money clicks from
// mailing_offer_alignment_clicks, the rollup RefreshOfferAlignmentSnapshot
// keeps. mailing_tracking_events cannot answer this per offer: the campaign
// scan timed out at 280s for Liberty's set, so every large offer read zero.
// Rows carry Denver days, so from/to resolve to calendar dates.
func fetchAlignmentEngagement(ctx context.Context, db alignmentDB, orgID interface{}, ids []string, from, to time.Time) (map[string]*alignmentEngagement, error) {
	out := map[string]*alignmentEngagement{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT isp, COALESCE(SUM(clicks), 0)::bigint, COUNT(DISTINCT subscriber_id)
		FROM mailing_offer_alignment_clicks
		WHERE organization_id = $1
		  AND campaign_id = ANY($2::uuid[])
		  AND local_dt >= $3::date AND local_dt <= $4::date
		GROUP BY 1
	`, orgID, pq.Array(ids), from.In(mstLoc).Format("2006-01-02"), to.In(mstLoc).Format("2006-01-02"))
	if err != nil {
		return nil, fmt.Errorf("alignment engagement: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var isp string
		e := &alignmentEngagement{}
		if err := rows.Scan(&isp, &e.HumanClicks, &e.Clickers); err != nil {
			return nil, err
		}
		out[isp] = e
	}
	return out, rows.Err()
}

// alignmentConversions is one ISP slice of the conversion ledger.
type alignmentConversions struct {
	Conversions int64
	Revenue     float64
}

// alignmentConvRow is one ledger bucket. Every view (ISP, creative, data
// source, each window) folds these, so one read serves them all.
type alignmentConvRow struct {
	CampaignID  string
	LocalDt     string
	ISP         string
	EverflowID  string
	DataSource  string
	Conversions int64
	Payout      float64 // posted dollars
	Unpriced    int64   // conversions that posted $0
}

// alignmentOnFilePayout prices conversions the network posts at $0, keyed by
// the ledger's everflow_offer_id. Same rates as the conversion report's
// offer_rate (agents/reporting/data/conversion_inputs.json). An offer absent
// here with no posted dollars has unknown revenue and shows —, never $0.00.
var alignmentOnFilePayout = map[string]float64{
	"338": 66.0, // Liberty Mutual: the network posts $0
}

// alignmentExcludedTransactions are ledger rows that are not conversions,
// from conversion_inputs.json exclude_transactions.
var alignmentExcludedTransactions = []string{
	"8fff07cc98394084a181be19d324cbb7", // Liberty $12,000 CPM allocation 2026-09-10
}

// revenue is the bucket's dollars: posted payout plus on-file rate × the
// conversions that posted $0.
func (r alignmentConvRow) revenue() float64 {
	return r.Payout + float64(r.Unpriced)*alignmentOnFilePayout[r.EverflowID]
}

// fetchAlignmentConversionRows reads mailing_everflow_conversions, the ledger
// the Everflow postback and Cake sync write, by each conversion's own
// campaign_id. No offer dictionary sits in the path.
func fetchAlignmentConversionRows(ctx context.Context, db alignmentDB, orgID interface{}, ids []string, from, to time.Time) ([]alignmentConvRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT ec.campaign_id::text,
		       to_char(ec.converted_at AT TIME ZONE 'America/Denver', 'YYYY-MM-DD'),
		       `+alignmentISPCase("split_part(COALESCE(sub.email,''),'@',2)")+`,
		       COALESCE(ec.everflow_offer_id, ''),
		       COALESCE(sub.data_source, ''),
		       COUNT(*),
		       COALESCE(SUM(ec.payout), 0)::float8,
		       COUNT(*) FILTER (WHERE COALESCE(ec.payout, 0) = 0)
		FROM mailing_everflow_conversions ec
		LEFT JOIN mailing_subscribers sub ON sub.id = ec.subscriber_id AND sub.organization_id = $1
		WHERE ec.organization_id = $1
		  AND ec.campaign_id = ANY($2::uuid[])
		  AND ec.converted_at >= $3 AND ec.converted_at <= $4
		  AND COALESCE(ec.transaction_id, '') <> ALL($5)
		GROUP BY 1, 2, 3, 4, 5
	`, orgID, pq.Array(ids), from, to, pq.Array(alignmentExcludedTransactions))
	if err != nil {
		return nil, fmt.Errorf("alignment conversions: %w", err)
	}
	defer rows.Close()
	out := []alignmentConvRow{}
	for rows.Next() {
		var r alignmentConvRow
		if err := rows.Scan(&r.CampaignID, &r.LocalDt, &r.ISP, &r.EverflowID, &r.DataSource,
			&r.Conversions, &r.Payout, &r.Unpriced); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// aggregateAlignmentConversions folds ledger rows inside [minDt, maxDt] into
// per-ISP cells ("" = unbounded on that side).
func aggregateAlignmentConversions(rows []alignmentConvRow, minDt, maxDt string) map[string]*alignmentConversions {
	out := map[string]*alignmentConversions{}
	for _, r := range rows {
		if !alignmentDtInRange(r.LocalDt, minDt, maxDt) {
			continue
		}
		c := out[r.ISP]
		if c == nil {
			c = &alignmentConversions{}
			out[r.ISP] = c
		}
		c.Conversions += r.Conversions
		c.Revenue += r.revenue()
	}
	return out
}

func alignmentDtInRange(dt, minDt, maxDt string) bool {
	return (minDt == "" || dt >= minDt) && (maxDt == "" || dt <= maxDt)
}

// alignmentOfferPriced reports whether any money is known for the offer: a
// posted payout or an on-file rate. CPM offers post no conversions at all, so
// the answer is read over the 14-day set, not a single window.
func alignmentOfferPriced(rows []alignmentConvRow) bool {
	for _, r := range rows {
		if r.Payout > 0 || alignmentOnFilePayout[r.EverflowID] > 0 {
			return true
		}
	}
	return false
}

// alignmentRevenueKnown is false when the dollars in a cell cannot be trusted
// as a number: the offer has no known price, or conversions landed with none.
func alignmentRevenueKnown(offerPriced bool, c *alignmentConversions) bool {
	return offerPriced && !(c.Conversions > 0 && c.Revenue == 0)
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
	RevenueKnown        bool    `json:"revenue_known"` // false ⇒ the UI shows — for revenue, RPM and EPC
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
// GET /api/mailing/offer-alignment/matrix?window=3|7|14 (trailing 'd' tolerated)
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
	if convErr != nil || !isOfferAlignmentWindow(days) {
		respondError(w, http.StatusBadRequest, "window must be 3, 7 or 14")
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
	StatusLine   string                    `json:"status_line"`
	Attribution  offerAlignmentAttribution `json:"attribution"`
	Delivered    int64                     `json:"delivered"`
	Hard         int64                     `json:"hard"`
	Soft         int64                     `json:"soft"`
	Deferred     int64                     `json:"deferred"`
	RepBlock     int64                     `json:"reputation_block"`
	HumanClicks  int64                     `json:"human_clicks"`
	Clickers     int64                     `json:"clickers"`
	ClickerRate  float64                   `json:"clicker_rate"`
	Conversions  int64                     `json:"conversions"`
	Revenue      float64                   `json:"revenue"`
	RevenueKnown bool                      `json:"revenue_known"`
	RPM          float64                   `json:"rpm"`
	EPC          float64                   `json:"epc"`
}

type offerAlignmentAttribution struct {
	StampedCampaigns  int `json:"stamped_campaigns"`
	InferredCampaigns int `json:"inferred_campaigns"`
}

type offerAlignmentISPRow struct {
	ISP          string  `json:"isp"`
	Delivered    int64   `json:"delivered"`
	Hard         int64   `json:"hard"`
	Soft         int64   `json:"soft"`
	RepBlock     int64   `json:"reputation_block"`
	Deferred     int64   `json:"deferred"`
	BlockRate    float64 `json:"block_rate"`
	HumanClicks  int64   `json:"human_clicks"`
	Clickers     int64   `json:"clickers"`
	ClickerRate  float64 `json:"clicker_rate"`
	Conversions  int64   `json:"conversions"`
	Revenue      float64 `json:"revenue"`
	RevenueKnown bool    `json:"revenue_known"`
	RPM          float64 `json:"rpm"`
	EPC          float64 `json:"epc"`
	Badge        string  `json:"badge"`
	BadgeReason  string  `json:"badge_reason"`
	Action       string  `json:"action"`
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
	RevenueKnown     bool    `json:"revenue_known"`
	SampleCampaignID string  `json:"sample_campaign_id"`
	HasHTML          bool    `json:"has_html"`
	Inferred         bool    `json:"inferred"`
}

type offerAlignmentDataSourceRow struct {
	DataSource *string `json:"data_source"` // null ⇒ "(unattributed)" in the UI
	Delivered  int64   `json:"delivered"`
	Hard       int64   `json:"hard"`
	// DeliveredKnown is false when the sent/bounce scan ran out of budget.
	DeliveredKnown bool    `json:"delivered_known"`
	InvalidRate    float64 `json:"invalid_rate"`
	Clicks         int64   `json:"clicks"`
	Clickers       int64   `json:"clickers"`
	Conversions    int64   `json:"conversions"`
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

	slugs, _, offerName, err := resolveOfferSlugsForKey(ctx, adb, offer)
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

	// Same campaign scope as the matrix cell: the 14-day set, events in range.
	scopeFrom := alignmentScopeFrom(from)
	set, err := resolveOfferCampaignSet(ctx, adb, orgID, offer, offerName, patterns, scopeFrom, to, sendingDomain)
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
	convRows, err := fetchAlignmentConversionRows(ctx, adb, orgID, set.IDs, scopeFrom, to)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	offerPriced := alignmentOfferPriced(convRows)
	convByISP := aggregateAlignmentConversions(convRows, fromDt, toDt)

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
			HumanClicks: e.HumanClicks, Clickers: e.Clickers,
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
			HumanClicks: e.HumanClicks, Clickers: e.Clickers,
			ClickerRate: alignmentDiv(float64(e.Clickers), float64(d.Delivered)),
			Conversions: cv.Conversions, Revenue: cv.Revenue,
			RevenueKnown: alignmentRevenueKnown(offerPriced, cv),
			RPM:          alignmentDiv(cv.Revenue*1000, float64(d.Delivered)),
			EPC:          alignmentDiv(cv.Revenue, float64(e.HumanClicks)),
			Badge:        badge, BadgeReason: reason, Action: action,
		}
		ispRows = append(ispRows, row)
		hdr.Delivered += d.Delivered
		hdr.Hard += d.Hard
		hdr.Soft += d.Soft
		hdr.Deferred += d.Deferred
		hdr.RepBlock += d.ReputationBlock
		hdr.HumanClicks += e.HumanClicks
		hdr.Clickers += e.Clickers
		hdr.Conversions += cv.Conversions
		hdr.Revenue += cv.Revenue
	}
	sort.Slice(ispRows, func(i, j int) bool { return ispRows[i].Delivered > ispRows[j].Delivered })

	hdr.ClickerRate = alignmentDiv(float64(hdr.Clickers), float64(hdr.Delivered))
	hdr.RevenueKnown = offerPriced
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
	// Panels fail soft: a slow panel must not take the whole profile down.
	creatives, err := s.fetchAlignmentCreatives(ctx, adb, orgID, set.IDs, convRows, offerPriced, from, to, fromDt, toDt)
	if err != nil {
		log.Printf("[offer-alignment] creatives panel %s: %v", offer, err)
		notes = append(notes, "The creative table did not load. Try again in a minute.")
		creatives = []offerAlignmentCreativeRow{}
	}

	// ── data-source panel ────────────────────────────────────────────────
	dataSources, err := s.fetchAlignmentDataSources(ctx, adb, orgID, set.IDs, convRows, from, to, fromDt, toDt)
	if err != nil {
		log.Printf("[offer-alignment] data-source panel %s: %v", offer, err)
		notes = append(notes, "The data source table did not load. Try again in a minute.")
		dataSources = []offerAlignmentDataSourceRow{}
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"header":       hdr,
		"isp_rows":     ispRows,
		"creatives":    creatives,
		"data_sources": dataSources,
		"notes":        notes,
	})
}

// alignmentDeliveredChunk bounds the per-campaign delivered fetch: the reader
// caps a breakdown at 5,000 rows and a campaign can span several ISPs.
const alignmentDeliveredChunk = 800

// fetchAlignmentDeliveredByCampaign returns lake delivered per campaign × ISP.
func fetchAlignmentDeliveredByCampaign(ctx context.Context, ids []string, fromDt, toDt string) (map[string]map[string]int64, error) {
	out := map[string]map[string]int64{}
	if !analytics.ReaderEnabled() {
		return out, nil
	}
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	sem := make(chan struct{}, alignmentLakeConcurrency)
	for start := 0; start < len(ids); start += alignmentDeliveredChunk {
		end := start + alignmentDeliveredChunk
		if end > len(ids) {
			end = len(ids)
		}
		wg.Add(1)
		go func(chunk []string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rows, err := analytics.Breakdown(ctx, analytics.BreakdownFilter{
				From: fromDt, To: toDt,
				SourceIn:    alignmentLakeSources,
				CampaignIDs: chunk,
				GroupBy:     []string{"campaign_id", "isp"},
				Eq:          map[string]string{"event_type": "delivered"},
				Limit:       5000,
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			for _, r := range rows {
				id, isp := r.Keys["campaign_id"], r.Keys["isp"]
				if isp == "" {
					isp = "other"
				}
				if out[id] == nil {
					out[id] = map[string]int64{}
				}
				out[id][isp] += r.Count
			}
		}(ids[start:end])
	}
	wg.Wait()
	return out, firstErr
}

// alignmentCreativeDim is one campaign's creative identity.
type alignmentCreativeDim struct {
	CreativeKey string
	Subject     string
	FromName    string
	HasHTML     bool
	Stamped     bool
}

// fetchAlignmentCreatives builds the creative × subject × ISP panel per
// campaign, then folds by creative: delivered from the lake, human money
// clicks from the rollup, conversions from the ledger's own campaign_id.
// Clickers are per-campaign DISTINCT summed across a creative's campaigns, so
// one person clicking the same creative in two campaigns counts twice. Delivered
// is lake truth; a lake failure leaves it 0 with clicks and conversions intact.
func (s *Server) fetchAlignmentCreatives(
	ctx context.Context, db alignmentDB, orgID interface{}, ids []string,
	conv []alignmentConvRow, offerPriced bool, from, to time.Time, fromDt, toDt string,
) ([]offerAlignmentCreativeRow, error) {
	out := []offerAlignmentCreativeRow{}
	if len(ids) == 0 {
		return out, nil
	}

	dims := map[string]alignmentCreativeDim{}
	rows, err := db.QueryContext(ctx, `
		SELECT c.id::text,
		       COALESCE(md5(c.html_content), 'no-html'),
		       COALESCE(c.subject, ''),
		       COALESCE(c.from_name, ''),
		       c.html_content IS NOT NULL,
		       COALESCE(c.offer_key,'') <> '' OR c.offer_id IS NOT NULL
		FROM mailing_campaigns c
		WHERE c.organization_id = $1 AND c.id = ANY($2::uuid[])
	`, orgID, pq.Array(ids))
	if err != nil {
		return nil, fmt.Errorf("alignment creatives: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var d alignmentCreativeDim
		if err := rows.Scan(&id, &d.CreativeKey, &d.Subject, &d.FromName, &d.HasHTML, &d.Stamped); err != nil {
			return nil, err
		}
		dims[id] = d
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	type groupKey struct{ creative, subject, isp string }
	type group struct {
		row     offerAlignmentCreativeRow
		stamped bool
	}
	groups := map[groupKey]*group{}
	touch := func(campaignID, isp string) *group {
		d, ok := dims[campaignID]
		if !ok {
			return nil
		}
		k := groupKey{d.CreativeKey, d.Subject, isp}
		g := groups[k]
		if g == nil {
			g = &group{row: offerAlignmentCreativeRow{
				CreativeKey: d.CreativeKey, Subject: d.Subject, ISP: isp, FromName: d.FromName,
			}}
			groups[k] = g
		}
		if g.row.SampleCampaignID == "" || campaignID < g.row.SampleCampaignID {
			g.row.SampleCampaignID = campaignID
			g.row.FromName = d.FromName
		}
		g.row.HasHTML = g.row.HasHTML || d.HasHTML
		g.stamped = g.stamped || d.Stamped
		return g
	}

	delivered, derr := fetchAlignmentDeliveredByCampaign(ctx, ids, fromDt, toDt)
	if derr != nil {
		log.Printf("[offer-alignment] creative delivered (lake, non-fatal): %v", derr)
	}
	for id, byISP := range delivered {
		for isp, n := range byISP {
			if g := touch(id, isp); g != nil {
				g.row.Delivered += n
			}
		}
	}

	clickRows, err := db.QueryContext(ctx, `
		SELECT campaign_id::text, isp, COALESCE(SUM(clicks), 0)::bigint, COUNT(DISTINCT subscriber_id)
		FROM mailing_offer_alignment_clicks
		WHERE organization_id = $1
		  AND campaign_id = ANY($2::uuid[])
		  AND local_dt >= $3::date AND local_dt <= $4::date
		GROUP BY 1, 2
	`, orgID, pq.Array(ids), from.In(mstLoc).Format("2006-01-02"), to.In(mstLoc).Format("2006-01-02"))
	if err != nil {
		return nil, fmt.Errorf("alignment creative clicks: %w", err)
	}
	defer clickRows.Close()
	for clickRows.Next() {
		var id, isp string
		var clicks, clickers int64
		if err := clickRows.Scan(&id, &isp, &clicks, &clickers); err != nil {
			return nil, err
		}
		if g := touch(id, isp); g != nil {
			g.row.Clicks += clicks
			g.row.Clickers += clickers
		}
	}
	if err := clickRows.Err(); err != nil {
		return nil, err
	}

	for _, c := range conv {
		if !alignmentDtInRange(c.LocalDt, fromDt, toDt) {
			continue
		}
		if g := touch(c.CampaignID, c.ISP); g != nil {
			g.row.Conversions += c.Conversions
			g.row.Revenue += c.revenue()
		}
	}

	for _, g := range groups {
		g.row.Inferred = !g.stamped
		g.row.ClickerRate = alignmentDiv(float64(g.row.Clickers), float64(g.row.Delivered))
		g.row.RevenueKnown = alignmentRevenueKnown(offerPriced, &alignmentConversions{
			Conversions: g.row.Conversions, Revenue: g.row.Revenue,
		})
		out = append(out, g.row)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Clicks != b.Clicks {
			return a.Clicks > b.Clicks
		}
		if a.Delivered != b.Delivered {
			return a.Delivered > b.Delivered
		}
		return a.CreativeKey+a.Subject+a.ISP < b.CreativeKey+b.Subject+b.ISP
	})
	if len(out) > creativesAnalyticsRowLimit {
		out = out[:creativesAnalyticsRowLimit]
	}
	return out, nil
}

// alignmentDataSourceSentBudget bounds the only tracking-events scan left on
// the profile. Large offers do not finish inside it; those rows report
// delivered_known=false and the UI shows — rather than a false zero.
const alignmentDataSourceSentBudget = 25 * time.Second

// fetchAlignmentDataSources builds the per-data-source panel: human money
// clicks from the rollup, conversions from the ledger, and PG sent + hard
// bounces when the scan fits alignmentDataSourceSentBudget. NULL/” data_source
// is emitted as JSON null (the UI renders "(unattributed)"). invalid_rate =
// hard/(delivered+hard), PG scope.
func (s *Server) fetchAlignmentDataSources(
	ctx context.Context, db alignmentDB, orgID interface{}, ids []string,
	conv []alignmentConvRow, from, to time.Time, fromDt, toDt string,
) ([]offerAlignmentDataSourceRow, error) {
	out := []offerAlignmentDataSourceRow{}
	if len(ids) == 0 {
		return out, nil
	}
	byKey := map[string]*offerAlignmentDataSourceRow{}
	get := func(key string) *offerAlignmentDataSourceRow {
		row := byKey[key]
		if row == nil {
			row = &offerAlignmentDataSourceRow{}
			if key != "" {
				ds := key
				row.DataSource = &ds
			}
			byKey[key] = row
		}
		return row
	}

	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(NULLIF(sub.data_source,''),''),
		       COALESCE(SUM(c.clicks), 0)::bigint,
		       COUNT(DISTINCT c.subscriber_id)
		FROM mailing_offer_alignment_clicks c
		JOIN mailing_subscribers sub ON sub.id = c.subscriber_id AND sub.organization_id = $1
		WHERE c.organization_id = $1
		  AND c.campaign_id = ANY($2::uuid[])
		  AND c.local_dt >= $3::date AND c.local_dt <= $4::date
		GROUP BY 1
	`, orgID, pq.Array(ids), from.In(mstLoc).Format("2006-01-02"), to.In(mstLoc).Format("2006-01-02"))
	if err != nil {
		return nil, fmt.Errorf("alignment data sources: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var clicks, clickers int64
		if err := rows.Scan(&key, &clicks, &clickers); err != nil {
			return nil, err
		}
		row := get(key)
		row.Clicks = clicks
		row.Clickers = clickers
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, c := range conv {
		if alignmentDtInRange(c.LocalDt, fromDt, toDt) {
			get(c.DataSource).Conversions += c.Conversions
		}
	}

	sent, sentErr := s.fetchAlignmentDataSourceSent(ctx, orgID, ids, from, to)
	if sentErr != nil {
		log.Printf("[offer-alignment] data-source sent scan (shows —): %v", sentErr)
	} else {
		for key, v := range sent {
			row := get(key)
			row.Delivered = v[0]
			row.Hard = v[1]
		}
	}
	for _, row := range byKey {
		row.DeliveredKnown = sentErr == nil
		row.InvalidRate = alignmentDiv(float64(row.Hard), float64(row.Delivered+row.Hard))
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Delivered != out[j].Delivered {
			return out[i].Delivered > out[j].Delivered
		}
		return out[i].Clicks > out[j].Clicks
	})
	return out, nil
}

// fetchAlignmentDataSourceSent counts sent + hard bounces per data source. It
// runs on the pool, not the caller's session: a budget cancel can poison the
// connection, and the profile still needs its session afterwards.
func (s *Server) fetchAlignmentDataSourceSent(ctx context.Context, orgID interface{}, ids []string, from, to time.Time) (map[string][2]int64, error) {
	ctx, cancel := context.WithTimeout(ctx, alignmentDataSourceSentBudget)
	defer cancel()
	// $1 org, $2 campaign ids, $3 from, $4 to.
	const q = `
		SELECT COALESCE(NULLIF(sub.data_source,''),'') AS data_source,
		       COUNT(*) FILTER (WHERE t.event_type = 'sent') AS delivered,
		       COUNT(*) FILTER (WHERE t.event_type = 'bounced' AND t.bounce_type = 'hard') AS hard
		FROM mailing_tracking_events t
		JOIN mailing_subscribers sub ON sub.id = t.subscriber_id AND sub.organization_id = $1
		WHERE t.organization_id = $1
		  AND t.campaign_id = ANY($2::uuid[])
		  AND t.event_type IN ('sent', 'bounced')
		  AND t.event_at >= $3 AND t.event_at <= $4
		GROUP BY 1
	`
	out := map[string][2]int64{}
	for start := 0; start < len(ids); start += alignmentEngagementChunk {
		end := start + alignmentEngagementChunk
		if end > len(ids) {
			end = len(ids)
		}
		rows, err := s.mailingDB.QueryContext(ctx, q, orgID, pq.Array(ids[start:end]), from, to)
		if err != nil {
			return nil, fmt.Errorf("chunk %d: %w", start/alignmentEngagementChunk, err)
		}
		for rows.Next() {
			var key string
			var delivered, hard int64
			if err := rows.Scan(&key, &delivered, &hard); err != nil {
				rows.Close()
				return nil, err
			}
			v := out[key]
			v[0] += delivered
			v[1] += hard
			out[key] = v
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
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
	set, err := resolveOfferCampaignSet(ctx, adb, orgID, offer, offerName, patterns, alignmentScopeFrom(from), to, "")
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
		SourceIn:    alignmentLakeSources,
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
		SourceIn:    alignmentLakeSources,
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
