package api

// Offer Alignment snapshot worker — materialises
// mailing_offer_alignment_snapshot (offer × ISP × window cells) so the matrix
// endpoint serves in milliseconds instead of running ~40 Athena calls per
// page load. Copied 1:1 from the audience-cadence snapshot pattern
// (audience_cadence_by_cell.go): background ticker, refresh mutex, staleness
// threshold, DELETE+INSERT tx, empty→building response + async kick, POST
// force-refresh endpoint.
//
// Refresh shape per organization:
//  1. Enumerate active offers over the trailing 30 Denver days: DISTINCT
//     stamped offer_key ∪ slug-map offers with money clicks.
//  2. Per offer: resolve the campaign set (stamped ∪ inferred) for the 30d
//     and 7d windows; run TWO lake Breakdowns (delivery local_dt×isp×
//     event_type + dsn local_dt×isp×dsn_family) over the 30d set and slice
//     the 7d window from local_dt — one Athena round trip pair per offer.
//  3. Per window: one PG engagement pass (verdict-human money clicks /
//     clickers / sent denominator per ISP) + one conversions pass.
//  4. classifyAlignmentCell stamps badge/reason/action/sample_ok per cell.
//
// If the lake reader is disabled the refresh no-ops (the matrix endpoint
// serves {"disabled":true} in that case, so an empty snapshot is never
// user-visible).

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/analytics"
)

const (
	// offerAlignmentRefreshInterval — snapshot rebuild cadence. 30m (design
	// doc B4): each rebuild costs ~2 Athena calls per active offer.
	offerAlignmentRefreshInterval = 30 * time.Minute
	// offerAlignmentStalenessThreshold — matrix reads older than this kick an
	// async refresh while still serving the current rows.
	offerAlignmentStalenessThreshold = 60 * time.Minute
	// offerAlignmentRefreshTimeout bounds one full rebuild (Athena serial
	// latency dominates; ~25 offers × 2 calls × ~5s ≈ 4-5 min + PG headroom).
	offerAlignmentRefreshTimeout = 15 * time.Minute
	// offerAlignmentMaxOffers caps the per-org offer enumeration so a
	// pathological slug fan-out can't run the Athena bill unbounded. Offers
	// are ranked by stamped-campaign count, then click volume.
	offerAlignmentMaxOffers = 25
	// offerAlignmentMetaKey marks the per-(org,window) "built" sentinel row —
	// written on every successful refresh even when the window has zero data
	// rows, filtered out of every read. Distinguishes "legitimately empty
	// window" (200 + empty rows) from "never built" (202 building).
	offerAlignmentMetaKey = "__meta__"
)

// offerAlignmentRefreshMu serialises snapshot rebuilds (ticker, manual
// refresh, and empty-snapshot kicks can all race).
var offerAlignmentRefreshMu sync.Mutex

// offerAlignmentSnapRow is one materialised offer × ISP × window cell.
// Raw counters only — rates are derived at read time by the matrix handler
// (single derivation site: buildOfferAlignmentMatrixRow).
type offerAlignmentSnapRow struct {
	OfferKey        string
	OfferName       string
	ISP             string
	Delivered       int64
	Hard            int64
	Soft            int64
	ReputationBlock int64
	Deferred        int64
	HumanClicks     int64
	Clickers        int64
	PGSent          int64
	Conversions     int64
	Revenue         float64
	Stamped         int
	Inferred        int
	Badge           string
	BadgeReason     string
	Action          string
	SampleOK        bool
}

// StartOfferAlignmentWorker launches the periodic snapshot refresher. Called
// once from SetMailingDB (server_routes_mailing.go) next to the
// audience-cadence worker; cancellation tied to process lifetime.
func (s *Server) StartOfferAlignmentWorker(ctx context.Context) {
	go func() {
		select {
		case <-time.After(90 * time.Second): // let boot settle first
		case <-ctx.Done():
			return
		}
		runOnce := func() {
			rc, cancel := context.WithTimeout(ctx, offerAlignmentRefreshTimeout)
			defer cancel()
			if err := s.RefreshOfferAlignmentSnapshot(rc); err != nil {
				log.Printf("[offer-alignment-worker] refresh: %v", err)
			}
		}
		runOnce()
		ticker := time.NewTicker(offerAlignmentRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runOnce()
			}
		}
	}()
}

// offerAlignmentKickPending bounds background kicks to ONE pending goroutine:
// an empty/stale matrix under UI polling would otherwise pile up goroutines
// all waiting their turn on the refresh mutex.
var offerAlignmentKickPending atomic.Bool

// kickOfferAlignmentRefresh runs a refresh in the background with a fresh
// context (fire-and-forget; the mutex serialises with the ticker, and the
// pending flag drops redundant kicks while one is queued/running).
func (s *Server) kickOfferAlignmentRefresh(trigger string) {
	if !offerAlignmentKickPending.CompareAndSwap(false, true) {
		return // a kick is already queued or running — this one is redundant
	}
	go func() {
		defer offerAlignmentKickPending.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), offerAlignmentRefreshTimeout)
		defer cancel()
		start := time.Now()
		if err := s.RefreshOfferAlignmentSnapshot(ctx); err != nil {
			log.Printf("[offer-alignment] %s refresh error: %v", trigger, err)
			return
		}
		log.Printf("[offer-alignment] %s refresh ok in %s", trigger, time.Since(start))
	}()
}

// RefreshOfferAlignmentSnapshot rebuilds mailing_offer_alignment_snapshot for
// every org with campaigns in the trailing 30 days. Safe to call
// concurrently (mutex) and on restart (full DELETE+INSERT; the table carries
// no incremental state). Honours ctx cancellation between offers.
func (s *Server) RefreshOfferAlignmentSnapshot(ctx context.Context) error {
	offerAlignmentRefreshMu.Lock()
	defer offerAlignmentRefreshMu.Unlock()

	if s.mailingDB == nil {
		return fmt.Errorf("mailing db not configured")
	}
	if !analytics.ReaderEnabled() {
		log.Printf("[offer-alignment-refresh] lake read disabled — skipping (matrix serves disabled)")
		return nil
	}

	now := time.Now().In(mstLoc)
	toDt := now.Format("2006-01-02")
	from30 := now.AddDate(0, 0, -29)
	from30Dt := from30.Format("2006-01-02")
	from7 := now.AddDate(0, 0, -6)
	from7Dt := from7.Format("2006-01-02")
	from30Ts := time.Date(from30.Year(), from30.Month(), from30.Day(), 0, 0, 0, 0, mstLoc)
	from7Ts := time.Date(from7.Year(), from7.Month(), from7.Day(), 0, 0, 0, 0, mstLoc)
	toTs := now

	orgs, err := s.offerAlignmentOrgs(ctx, from30Ts)
	if err != nil {
		return fmt.Errorf("org enumeration: %w", err)
	}

	type snapKey struct {
		org  string
		days int
	}
	rowsByOrg := map[snapKey][]offerAlignmentSnapRow{}
	// okOrgs = orgs whose enumeration succeeded this cycle. Only THEIR rows
	// are deleted+rewritten: an org whose enumeration hit a transient error
	// keeps its previous (stale-marked) snapshot instead of flipping to
	// no-data for a cycle.
	okOrgs := make([]string, 0, len(orgs))
	start := time.Now()
	for _, org := range orgs {
		offers, err := s.offerAlignmentOffers(ctx, org, from30Ts, toTs)
		if err != nil {
			log.Printf("[offer-alignment-refresh] offer enumeration (org %s, non-fatal): %v", org, err)
			continue
		}
		okOrgs = append(okOrgs, org)
		for _, off := range offers {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			slugs, efids, offerName, err := resolveOfferSlugsForKey(ctx, s.mailingDB, off.key)
			if err != nil {
				log.Printf("[offer-alignment-refresh] slug resolve %s (non-fatal): %v", off.key, err)
				continue
			}
			// Ledgers carry no dollars — matrix revenue is estimated at the
			// offer's deal-eCPA/payout price (METRIC_CONTRACT §9).
			convPrice := resolveOfferConversionPrice(ctx, s.mailingDB, org, efids)
			if offerName == "" {
				offerName = off.name
			}
			if offerName == "" {
				offerName = off.key
			}
			patterns := make([]string, len(slugs))
			for i, sl := range slugs {
				patterns[i] = "%/" + sl + "/%"
			}

			set30, err := resolveOfferCampaignSet(ctx, s.mailingDB, org, off.key, offerName, patterns, from30Ts, toTs, "")
			if err != nil {
				log.Printf("[offer-alignment-refresh] campaign set %s (non-fatal): %v", off.key, err)
				continue
			}
			if len(set30.IDs) == 0 {
				continue
			}

			// ONE Athena round-trip pair per offer over the 30d-active set.
			// BOTH windows are events-in-window over that same set (7d slices
			// by local_dt for delivery and by event time for engagement/
			// conversions) so every rate inside a cell shares one campaign
			// scope — a 7d set would give the 7d cell delivery from campaigns
			// its engagement query never saw (late deferral retries) and the
			// rates would mix scopes. Disclosed in METRIC_CONTRACT §9.
			ids, _ := lakeCampaignIDs(set30)
			dRows, dsnRows, err := fetchAlignmentLakeRows(ctx, ids, from30Dt, toDt)
			if err != nil {
				log.Printf("[offer-alignment-refresh] lake %s (non-fatal): %v", off.key, err)
				continue
			}

			for _, win := range []struct {
				days       int
				minLocalDt string
				fromTs     time.Time
			}{
				{7, from7Dt, from7Ts},
				{30, "", from30Ts},
			} {
				delivery := aggregateAlignmentDelivery(dRows, win.minLocalDt)
				dsn := aggregateAlignmentDSN(dsnRows, win.minLocalDt)
				eng, err := fetchAlignmentEngagement(ctx, s.mailingDB, org, set30.IDs, win.fromTs, toTs)
				if err != nil {
					log.Printf("[offer-alignment-refresh] engagement %s/%dd (non-fatal): %v", off.key, win.days, err)
					eng = map[string]*alignmentEngagement{}
				}
				conv, err := fetchAlignmentConversionsByISP(ctx, s.mailingDB, org, efids, win.fromTs, toTs)
				if err != nil {
					log.Printf("[offer-alignment-refresh] conversions %s/%dd (non-fatal): %v", off.key, win.days, err)
					conv = map[string]*alignmentConversions{}
				}
				applyEstimatedConversionRevenue(conv, convPrice)

				ispSet := map[string]bool{}
				for k := range delivery {
					ispSet[k] = true
				}
				for k := range eng {
					ispSet[k] = true
				}
				for k := range conv {
					ispSet[k] = true
				}
				for isp := range ispSet {
					d := delivery[isp]
					if d == nil {
						d = &alignmentDelivery{}
					}
					e := eng[isp]
					if e == nil {
						e = &alignmentEngagement{}
					}
					cv := conv[isp]
					if cv == nil {
						cv = &alignmentConversions{}
					}
					fams := dsn[isp]
					badge, reason, action, sampleOK := classifyAlignmentCell(alignmentCellStats{
						Delivered: d.Delivered, Hard: d.Hard, Soft: d.Soft,
						ReputationBlock: d.ReputationBlock, Deferred: d.Deferred,
						BlockingDSN: fams["HM08"] + fams["HCM2"] + fams["S3140"],
						HM08:        fams["HM08"],
						Hard511:     fams["5.1.1"],
						HumanClicks: e.HumanClicks, Clickers: e.Clickers, PGSent: e.PGSent,
						Revenue: cv.Revenue,
					})
					rowsByOrg[snapKey{org, win.days}] = append(rowsByOrg[snapKey{org, win.days}], offerAlignmentSnapRow{
						OfferKey: off.key, OfferName: offerName, ISP: isp,
						Delivered: d.Delivered, Hard: d.Hard, Soft: d.Soft,
						ReputationBlock: d.ReputationBlock, Deferred: d.Deferred,
						HumanClicks: e.HumanClicks, Clickers: e.Clickers, PGSent: e.PGSent,
						Conversions: cv.Conversions, Revenue: cv.Revenue,
						Stamped: set30.Stamped, Inferred: set30.Inferred,
						Badge: badge, BadgeReason: reason, Action: action, SampleOK: sampleOK,
					})
				}
			}
		}
	}

	// Atomic swap: DELETE + INSERT in one tx — scoped to the orgs that
	// enumerated cleanly this cycle (see okOrgs above). Rows older than 7 days
	// are pruned regardless, so orgs that stop sending don't keep zombie rows.
	tx, err := s.mailingDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM mailing_offer_alignment_snapshot
		WHERE organization_id = ANY($1) OR refreshed_at < NOW() - INTERVAL '7 days'
	`, pq.Array(okOrgs)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete: %w", err)
	}
	// Per-(org,window) built sentinel: marks "this window was refreshed" even
	// when it produced zero data rows, so the matrix can render a designed
	// empty state instead of an eternal 202-building loop on idle orgs.
	// Filtered out of every read (offer_key = offerAlignmentMetaKey).
	for _, org := range okOrgs {
		for _, days := range []int{7, 30} {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO mailing_offer_alignment_snapshot
				(organization_id, window_days, offer_key, offer_name, isp,
				 delivered, hard, soft, reputation_block, deferred,
				 human_clicks, clickers, pg_sent, conversions, revenue,
				 stamped_campaigns, inferred_campaigns,
				 badge, badge_reason, action, sample_ok, refreshed_at)
				VALUES ($1,$2,$3,'',$4,0,0,0,0,0,0,0,0,0,0,0,0,'META','','',FALSE,NOW())
			`, org, days, offerAlignmentMetaKey, offerAlignmentMetaKey); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("meta insert (%s/%dd): %w", org, days, err)
			}
		}
	}
	rowCount := 0
	for k, rows := range rowsByOrg {
		for _, c := range rows {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO mailing_offer_alignment_snapshot
				(organization_id, window_days, offer_key, offer_name, isp,
				 delivered, hard, soft, reputation_block, deferred,
				 human_clicks, clickers, pg_sent, conversions, revenue,
				 stamped_campaigns, inferred_campaigns,
				 badge, badge_reason, action, sample_ok, refreshed_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,NOW())
			`, k.org, k.days, c.OfferKey, c.OfferName, c.ISP,
				c.Delivered, c.Hard, c.Soft, c.ReputationBlock, c.Deferred,
				c.HumanClicks, c.Clickers, c.PGSent, c.Conversions, c.Revenue,
				c.Stamped, c.Inferred,
				c.Badge, c.BadgeReason, c.Action, c.SampleOK); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("insert (%s/%s/%dd): %w", c.OfferKey, c.ISP, k.days, err)
			}
			rowCount++
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	log.Printf("[offer-alignment-refresh] wrote %d rows (%d orgs) in %s", rowCount, len(orgs), time.Since(start))
	return nil
}

// offerAlignmentOrgs enumerates organizations with campaigns in the window.
func (s *Server) offerAlignmentOrgs(ctx context.Context, since time.Time) ([]string, error) {
	rows, err := s.mailingDB.QueryContext(ctx,
		`SELECT DISTINCT organization_id::text FROM mailing_campaigns WHERE scheduled_at >= $1`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// alignmentOfferRef is one enumerated offer (key + best-known display name).
type alignmentOfferRef struct {
	key  string
	name string
}

// offerAlignmentOffers enumerates the active offers for one org over the
// window: stamped offer_key values ∪ slug-map offers whose money slugs were
// actually clicked (the inferred/historical inlet, same derivation as
// HandleAnalyticsCreativeOffers). Ranked by activity, capped at
// offerAlignmentMaxOffers.
func (s *Server) offerAlignmentOffers(ctx context.Context, orgID string, from, to time.Time) ([]alignmentOfferRef, error) {
	type agg struct {
		name   string
		weight int64
	}
	offers := map[string]*agg{}

	// (a) stamped offer keys, weighted by campaign count.
	// $1 org, $2 from, $3 to.
	rows, err := s.mailingDB.QueryContext(ctx, `
		SELECT lower(offer_key), COUNT(*)
		FROM mailing_campaigns
		WHERE organization_id = $1
		  AND COALESCE(offer_key,'') <> ''
		  AND scheduled_at >= $2 AND scheduled_at <= $3
		GROUP BY 1
	`, orgID, from, to)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key string
		var n int64
		if err := rows.Scan(&key, &n); err != nil {
			rows.Close()
			return nil, err
		}
		offers[key] = &agg{weight: n * 1000} // stamped offers rank first
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// (b) historical inlet: board-convention name-suffix tokens from
	// UNSTAMPED campaigns. This deliberately reads mailing_campaigns (small,
	// ~200k rows/30d) instead of scanning 30 days of mailing_tracking_events
	// for money-click slugs — the click-scan version hit the prod 30s
	// statement_timeout on first live refresh (2026-07-07 boot log) and wrote
	// 0 rows. Same extraction rule as parseOfferTokenFromCampaignName: last
	// " - " token, only on wave-convention names.
	rows, err = s.mailingDB.QueryContext(ctx, `
		WITH named AS (
			SELECT lower(substring(name FROM ' - ([A-Za-z0-9_-]+)$')) AS slug
			FROM mailing_campaigns
			WHERE organization_id = $1
			  AND COALESCE(offer_key,'') = ''
			  AND scheduled_at >= $2 AND scheduled_at <= $3
			  AND name ~ '\mW[0-9]+-'
			  AND substring(name FROM ' - ([A-Za-z0-9_-]+)$') !~ '^W[0-9]+-'
		)
		SELECT n.slug, COALESCE(min(m.offer_name), ''), COUNT(*)
		FROM named n
		LEFT JOIN mailing_offer_slug_map m ON upper(m.cratoolpro_slug) = upper(n.slug)
		WHERE n.slug IS NOT NULL AND n.slug <> ''
		GROUP BY 1
	`, orgID, from, to)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key, name string
		var n int64
		if err := rows.Scan(&key, &name, &n); err != nil {
			rows.Close()
			return nil, err
		}
		a := offers[key]
		if a == nil {
			a = &agg{}
			offers[key] = a
		}
		a.weight += n
		if a.name == "" {
			a.name = name
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]alignmentOfferRef, 0, len(offers))
	for k, a := range offers {
		out = append(out, alignmentOfferRef{key: k, name: a.name})
	}
	// Rank by activity weight desc, key asc (deterministic snapshots), cap.
	sort.Slice(out, func(i, j int) bool {
		wi, wj := offers[out[i].key].weight, offers[out[j].key].weight
		if wi != wj {
			return wi > wj
		}
		return out[i].key < out[j].key
	})
	if len(out) > offerAlignmentMaxOffers {
		out = out[:offerAlignmentMaxOffers]
	}
	return out, nil
}

// readOfferAlignmentSnapshot loads the matrix rows for one org + window and
// derives every rate in one place (fractions 0–1; 0 not NaN on zero
// denominators, per the frontend contract). built=true when the window was
// refreshed at least once (data rows OR the __meta__ sentinel present) — the
// caller uses it to tell "legitimately empty" from "never built".
func (s *Server) readOfferAlignmentSnapshot(ctx context.Context, orgID interface{}, windowDays int) ([]offerAlignmentMatrixRow, time.Time, bool, error) {
	rows, err := s.mailingDB.QueryContext(ctx, `
		SELECT offer_key, offer_name, isp,
		       delivered, hard, soft, reputation_block, deferred,
		       human_clicks, clickers, pg_sent, conversions, revenue::float8,
		       stamped_campaigns, inferred_campaigns,
		       badge, badge_reason, action, sample_ok, refreshed_at
		FROM mailing_offer_alignment_snapshot
		WHERE organization_id = $1 AND window_days = $2
		ORDER BY delivered DESC, offer_key, isp
	`, orgID, windowDays)
	if err != nil {
		return nil, time.Time{}, false, err
	}
	defer rows.Close()
	out := []offerAlignmentMatrixRow{}
	built := false
	var maxRefresh time.Time
	for rows.Next() {
		var (
			r                 offerAlignmentMatrixRow
			pgSent            int64
			stamped, inferred int
			refreshed         time.Time
		)
		if err := rows.Scan(&r.OfferKey, &r.OfferName, &r.ISP,
			&r.Delivered, &r.Hard, &r.Soft, &r.ReputationBlock, &r.Deferred,
			&r.HumanClicks, &r.Clickers, &pgSent, &r.Conversions, &r.Revenue,
			&stamped, &inferred,
			&r.Badge, &r.BadgeReason, &r.Action, &r.SampleOK, &refreshed); err != nil {
			return nil, time.Time{}, false, err
		}
		built = true
		if refreshed.After(maxRefresh) {
			maxRefresh = refreshed
		}
		if r.OfferKey == offerAlignmentMetaKey {
			continue // built sentinel — never a data row
		}
		attempted := r.Delivered + r.Hard + r.Soft + r.ReputationBlock
		r.BlockRate = alignmentDiv(float64(r.ReputationBlock), float64(attempted))
		r.ClickerRate = alignmentDiv(float64(r.Clickers), float64(pgSent))
		r.RPM = alignmentDiv(r.Revenue*1000, float64(r.Delivered))
		r.EPC = alignmentDiv(r.Revenue, float64(r.HumanClicks))
		r.AttributionCoverage = alignmentDiv(float64(stamped), float64(stamped+inferred))
		out = append(out, r)
	}
	return out, maxRefresh, built, rows.Err()
}
