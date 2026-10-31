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
//  1. Update the human money-click rollup (refreshOfferAlignmentClicks) once.
//  2. Enumerate active offers over the trailing 14 Denver days by offer_id,
//     with stamped offer_key ∪ board-name tokens for campaigns without one.
//  3. Per offer: resolve the 14d campaign set; run TWO lake Breakdowns
//     (delivery local_dt×isp×event_type + dsn local_dt×isp×dsn_family) and
//     one ledger read over it, and slice each window from local_dt — one
//     Athena round trip pair per offer. Per window: one lake-clicks call and
//     one rollup read.
//  4. classifyAlignmentCell stamps badge/reason/action/sample_ok per cell.
//
// If the lake reader is disabled the refresh no-ops (the matrix endpoint
// serves {"disabled":true} in that case, so an empty snapshot is never
// user-visible).

import (
	"context"
	"database/sql"
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
	// offerAlignmentRefreshInterval — snapshot rebuild cadence. Each rebuild
	// costs ~2 Athena calls per active offer (×10 chunks at drip scale), and
	// every Athena call GETs each lake object in the scanned partitions — at
	// 30m this was ~830M S3 GETs/day (~$330/day, 2026-07 incident). The matrix
	// feeds daily-cadence decisions; 3h staleness is acceptable, and the POST
	// force-refresh endpoint covers anyone who needs it fresher.
	offerAlignmentRefreshInterval = 3 * time.Hour
	// offerAlignmentStalenessThreshold — matrix reads older than this kick an
	// async refresh while still serving the current rows. MUST exceed
	// offerAlignmentRefreshInterval, or portal page views drive the effective
	// refresh cadence and the interval above is inert.
	offerAlignmentStalenessThreshold = offerAlignmentRefreshInterval + 30*time.Minute
	// offerAlignmentRefreshTimeout bounds one full rebuild. Athena serial
	// latency dominates, and drip-scale offers chunk the lake fetch (20k ids =
	// 10 chunks × 2 calls), so a full fleet pass can exceed the old 15m
	// (baseline measured 14m04s before chunking).
	//
	// Raised 30m → 75m on 2026-08-01: the pass gained a THIRD per-offer×window
	// data fetch (fetchAlignmentLakeClicks, for the non-CPM funnel) and the
	// first post-deploy run died at exactly 30m — "[offer-alignment-worker]
	// refresh: context deadline exceeded" — having reached only ~7 of 25
	// offers. That failure mode is silent and total: rows are swapped in ONE
	// DELETE+INSERT tx at the very end, so a timeout means the tx never commits
	// and the WHOLE snapshot (the existing alignment matrix included, not just
	// the new columns) silently stops updating while still serving stale rows.
	// The ceiling that actually matters is offerAlignmentRefreshInterval (3h) —
	// 75m leaves ample headroom, and the advisory lock prevents overlap.
	offerAlignmentRefreshTimeout = 75 * time.Minute
	// offerAlignmentMaxOffers caps the per-org offer enumeration so a
	// pathological slug fan-out can't run the Athena bill unbounded. Offers
	// are ranked by stamped-campaign count, then click volume.
	offerAlignmentMaxOffers = 30
	// offerAlignmentMaxWindow is the widest window: the campaign set and lake
	// fetch run once at this width and the narrower windows slice it.
	offerAlignmentMaxWindow = 14
	// offerAlignmentClickScanTimeout bounds the rollup scan. Cold it costs
	// ~1.3ms per clicked row (3 days = 65s), so a first 14-day build needs
	// minutes; later runs read only the newest two days.
	offerAlignmentClickScanTimeout = 15 * time.Minute
	// offerAlignmentMetaKey marks the per-(org,window) "built" sentinel row —
	// written on every successful refresh even when the window has zero data
	// rows, filtered out of every read. Distinguishes "legitimately empty
	// window" (200 + empty rows) from "never built" (202 building).
	offerAlignmentMetaKey = "__meta__"
	// offerAlignmentRefreshLockID — PG advisory-lock key for cross-task
	// single-flight on the snapshot refresh (arbitrary, unique in this repo).
	offerAlignmentRefreshLockID int64 = 728104407
)

// offerAlignmentWindows are the windows the snapshot serves, in days.
var offerAlignmentWindows = []int{3, 7, 14}

func isOfferAlignmentWindow(days int) bool {
	for _, w := range offerAlignmentWindows {
		if w == days {
			return true
		}
	}
	return false
}

// alignmentScopeFrom widens a range start to the widest window, so a cell and
// its profile read one campaign set with events in range.
func alignmentScopeFrom(from time.Time) time.Time {
	now := time.Now().In(mstLoc)
	d := now.AddDate(0, 0, -(offerAlignmentMaxWindow - 1))
	widest := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, mstLoc)
	if widest.Before(from) {
		return widest
	}
	return from
}

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
	// LakeClickers/LakeClicks are the ATHENA navigational-click metrics
	// (analytics.ActionClicks) — distinct subscribers and distinct click
	// events on content/commercial links, asset fetches and compliance links
	// removed. They are deliberately NOT the same number as Clickers above:
	// that one is PG-sourced and gated on ignite_verdict_is_human, which the
	// operator's 2026-07-18 signal-grading doctrine classifies as a
	// scanner-STORM filter with a ~20% false-negative floor on proven humans,
	// and which was retired from the click path on 2026-07-21. New surfaces
	// (the non-CPM performance funnel) read the lake pair; the verdict-gated
	// columns are kept so the existing matrix badges are unchanged.
	LakeClickers int64
	LakeClicks   int64
	Conversions  int64
	Revenue      float64
	RevenueKnown bool
	Stamped      int
	Inferred     int
	Badge        string
	BadgeReason  string
	Action       string
	SampleOK     bool
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
// every org with campaigns in the trailing 14 days. Safe to call
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

	// Cross-TASK single-flight: the mutex above only serializes within one
	// process, but every ECS task runs this worker — on 2026-07-08 two tasks
	// refreshed concurrently, doubling Athena spend and PG contention (the
	// contention pushed otherwise-fast statements over the 30s
	// statement_timeout). PG advisory lock on a DEDICATED conn (pooled
	// lock/unlock can land on different sessions and leak the lock —
	// suppression_list.go's known trap). Not acquired ⇒ another task is
	// already refreshing; skip, the snapshot lands either way.
	lockConn, err := s.mailingDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("advisory lock conn: %w", err)
	}
	defer lockConn.Close()
	var lockAcquired bool
	if err := lockConn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", offerAlignmentRefreshLockID).Scan(&lockAcquired); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	if !lockAcquired {
		log.Printf("[offer-alignment-refresh] another task holds the refresh lock — skipping")
		return nil
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		lockConn.ExecContext(unlockCtx, "RESET ALL")
		if _, err := lockConn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock($1)", offerAlignmentRefreshLockID); err != nil {
			log.Printf("[offer-alignment-refresh] WARNING: advisory unlock failed (released at conn close): %v", err)
		}
	}()
	// Heavy read-only scans run on this same dedicated session with an
	// elevated statement_timeout (pool default 30s dies cold/under send load
	// on drip-scale sets — ps8241 sent chunk, 2026-07-08). RESET ALL before
	// unlock returns the session clean.
	if _, err := lockConn.ExecContext(ctx, "SET statement_timeout = '120000'"); err != nil {
		log.Printf("[offer-alignment-refresh] SET statement_timeout failed (pool default applies): %v", err)
	}

	now := time.Now().In(mstLoc)
	toDt := now.Format("2006-01-02")
	widest := alignmentScopeFrom(now)
	widestDt := widest.Format("2006-01-02")
	toTs := now

	orgs, err := s.offerAlignmentOrgs(ctx, widest)
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
		// One scan per refresh serves every offer. A failure leaves the last
		// rollup in place: clicks go stale, they do not go to zero.
		if err := refreshOfferAlignmentClicks(ctx, lockConn, org, widest); err != nil {
			log.Printf("[offer-alignment-refresh] click rollup (org %s, non-fatal): %v", org, err)
		}
		offers, err := s.offerAlignmentOffers(ctx, lockConn, org, widest, toTs)
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
			slugs, _, offerName, err := resolveOfferSlugsForKey(ctx, lockConn, off.key)
			if err != nil {
				log.Printf("[offer-alignment-refresh] slug resolve %s (non-fatal): %v", off.key, err)
				continue
			}
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

			set, err := resolveOfferCampaignSet(ctx, lockConn, org, off.key, offerName, patterns, widest, toTs, "")
			if err != nil {
				log.Printf("[offer-alignment-refresh] campaign set %s (non-fatal): %v", off.key, err)
				continue
			}
			if len(set.IDs) == 0 {
				continue
			}

			// ONE Athena round-trip pair per offer over the 14d-active set.
			// EVERY window is events-in-window over that same set (narrower
			// windows slice by local_dt) so every rate inside a cell shares
			// one campaign scope — a 3d set would give the 3d cell delivery
			// from campaigns its engagement query never saw (late deferral
			// retries) and the rates would mix scopes. Disclosed in
			// METRIC_CONTRACT §9.
			ids := set.IDs
			dRows, dsnRows, err := fetchAlignmentLakeRows(ctx, ids, widestDt, toDt)
			if err != nil {
				log.Printf("[offer-alignment-refresh] lake %s (non-fatal): %v", off.key, err)
				continue
			}
			// One ledger read per offer, sliced per window like the lake rows.
			convRows, err := fetchAlignmentConversionRows(ctx, lockConn, org, ids, widest, toTs)
			if err != nil {
				log.Printf("[offer-alignment-refresh] conversions %s (non-fatal): %v", off.key, err)
				convRows = nil
			}
			offerPriced := err == nil && alignmentOfferPriced(convRows)

			for _, days := range offerAlignmentWindows {
				winDay := now.AddDate(0, 0, -(days - 1))
				winFromDt := winDay.Format("2006-01-02")
				winFromTs := time.Date(winDay.Year(), winDay.Month(), winDay.Day(), 0, 0, 0, 0, mstLoc)
				delivery := aggregateAlignmentDelivery(dRows, winFromDt)
				dsn := aggregateAlignmentDSN(dsnRows, winFromDt)
				// Lake navigational clicks/clickers per ISP. Non-fatal like the
				// engagement fetch below — a lake hiccup
				// leaves the pair at 0 and the rest of the cell still lands.
				lakeClicks, err := fetchAlignmentLakeClicks(ctx, ids, winFromDt, toDt)
				if err != nil {
					log.Printf("[offer-alignment-refresh] lake clicks %s/%dd (non-fatal): %v", off.key, days, err)
					lakeClicks = map[string]*analytics.ActionClickRow{}
				}
				eng, err := fetchAlignmentEngagement(ctx, lockConn, org, ids, winFromTs, toTs)
				if err != nil {
					log.Printf("[offer-alignment-refresh] engagement %s/%dd (non-fatal): %v", off.key, days, err)
					eng = map[string]*alignmentEngagement{}
				}
				conv := aggregateAlignmentConversions(convRows, winFromDt, toDt)

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
				for k := range lakeClicks {
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
					lc := lakeClicks[isp]
					if lc == nil {
						lc = &analytics.ActionClickRow{}
					}
					fams := dsn[isp]
					badge, reason, action, sampleOK := classifyAlignmentCell(alignmentCellStats{
						Delivered: d.Delivered, Hard: d.Hard, Soft: d.Soft,
						ReputationBlock: d.ReputationBlock, Deferred: d.Deferred,
						BlockingDSN: fams["HM08"] + fams["HCM2"] + fams["S3140"],
						HM08:        fams["HM08"],
						Hard511:     fams["5.1.1"],
						HumanClicks: e.HumanClicks, Clickers: e.Clickers,
						Revenue: cv.Revenue,
					})
					rowsByOrg[snapKey{org, days}] = append(rowsByOrg[snapKey{org, days}], offerAlignmentSnapRow{
						OfferKey: off.key, OfferName: offerName, ISP: isp,
						Delivered: d.Delivered, Hard: d.Hard, Soft: d.Soft,
						ReputationBlock: d.ReputationBlock, Deferred: d.Deferred,
						HumanClicks: e.HumanClicks, Clickers: e.Clickers,
						LakeClickers: lc.Clickers, LakeClicks: lc.Clicks,
						Conversions: cv.Conversions, Revenue: cv.Revenue,
						RevenueKnown: alignmentRevenueKnown(offerPriced, cv),
						Stamped:      set.Stamped, Inferred: set.Inferred,
						Badge: badge, BadgeReason: reason, Action: action, SampleOK: sampleOK,
					})
				}
			}
		}
	}

	// Atomic swap: DELETE + INSERT in one tx — scoped to the orgs that
	// enumerated cleanly this cycle (see okOrgs above). Rows older than 7 days
	// and rows from retired windows are pruned regardless, so neither an org
	// that stops sending nor a window change leaves zombie rows.
	tx, err := s.mailingDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM mailing_offer_alignment_snapshot
		WHERE organization_id = ANY($1)
		   OR refreshed_at < NOW() - INTERVAL '7 days'
		   OR window_days <> ALL($2)
	`, pq.Array(okOrgs), pq.Array(offerAlignmentWindows)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete: %w", err)
	}
	// Per-(org,window) built sentinel: marks "this window was refreshed" even
	// when it produced zero data rows, so the matrix can render a designed
	// empty state instead of an eternal 202-building loop on idle orgs.
	// Filtered out of every read (offer_key = offerAlignmentMetaKey).
	for _, org := range okOrgs {
		for _, days := range offerAlignmentWindows {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO mailing_offer_alignment_snapshot
				(organization_id, window_days, offer_key, offer_name, isp,
				 delivered, hard, soft, reputation_block, deferred,
				 human_clicks, clickers, conversions, revenue,
				 stamped_campaigns, inferred_campaigns,
				 badge, badge_reason, action, sample_ok, refreshed_at,
				 lake_clickers, lake_clicks)
				VALUES ($1,$2,$3,'',$4,0,0,0,0,0,0,0,0,0,0,0,'META','','',FALSE,NOW(),0,0)
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
				 human_clicks, clickers, conversions, revenue, revenue_known,
				 stamped_campaigns, inferred_campaigns,
				 badge, badge_reason, action, sample_ok, refreshed_at,
				 lake_clickers, lake_clicks)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,NOW(),$22,$23)
			`, k.org, k.days, c.OfferKey, c.OfferName, c.ISP,
				c.Delivered, c.Hard, c.Soft, c.ReputationBlock, c.Deferred,
				c.HumanClicks, c.Clickers, c.Conversions, c.Revenue, c.RevenueKnown,
				c.Stamped, c.Inferred,
				c.Badge, c.BadgeReason, c.Action, c.SampleOK,
				c.LakeClickers, c.LakeClicks); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("insert (%s/%s/%dd): %w", c.OfferKey, c.ISP, k.days, err)
			}
			rowCount++
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	elapsed := time.Since(start)
	log.Printf("[offer-alignment-refresh] wrote %d rows (%d orgs) in %s (budget %s)", rowCount, len(orgs), elapsed, offerAlignmentRefreshTimeout)
	// Early warning for the silent-stall failure mode: the whole snapshot swaps
	// in one tx at the end, so once a pass exceeds its budget NOTHING updates
	// and the matrix serves stale rows indefinitely with no error surface.
	// Shout while there is still headroom rather than after the cliff.
	if elapsed > (offerAlignmentRefreshTimeout*3)/4 {
		log.Printf("[offer-alignment-refresh] WARNING: pass used %s of its %s budget — approaching the timeout, past which the snapshot silently stops updating",
			elapsed.Round(time.Second), offerAlignmentRefreshTimeout)
	}
	return nil
}

// refreshOfferAlignmentClicks maintains mailing_offer_alignment_clicks, one
// row per campaign × subscriber × ISP × Denver day of human money clicks. It
// scans clicks once for the whole org, which is what keeps the refresh bounded:
// per-offer scans over mailing_tracking_events never finished (see
// fetchAlignmentEngagement). The MATERIALIZED scan pins the plan to the
// click-verdict partial index; any org or campaign predicate inside it invites
// the planner onto indexes that visit every event type. Only days from the
// newest stored day minus one are rescanned, so late clicks land and refreshes
// after the first stay cheap. The swap is one tx: readers never see a gap.
func refreshOfferAlignmentClicks(ctx context.Context, conn *sql.Conn, orgID string, oldest time.Time) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%d'", offerAlignmentClickScanTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("statement timeout: %w", err)
	}
	var newest sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT MAX(local_dt) FROM mailing_offer_alignment_clicks WHERE organization_id = $1`, orgID).Scan(&newest); err != nil {
		return fmt.Errorf("newest day: %w", err)
	}
	oldestDt := oldest.Format("2006-01-02")
	scanFromDt := oldestDt
	if newest.Valid {
		if dt := newest.Time.UTC().AddDate(0, 0, -1).Format("2006-01-02"); dt > scanFromDt {
			scanFromDt = dt
		}
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM mailing_offer_alignment_clicks
		WHERE organization_id = $1 AND (local_dt < $2::date OR local_dt >= $3::date)
	`, orgID, oldestDt, scanFromDt); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	scanFrom, err := time.ParseInLocation("2006-01-02", scanFromDt, mstLoc)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mailing_offer_alignment_clicks
			(organization_id, campaign_id, subscriber_id, isp, local_dt, clicks)
		WITH clicked AS MATERIALIZED (
			SELECT e.organization_id, e.campaign_id, e.subscriber_id, e.recipient_domain, e.link_url, e.event_at
			FROM mailing_tracking_events e
			WHERE e.event_type = 'clicked'
			  AND e.event_at >= $2
			  AND e.click_verdict IN ('human', 'human-relay', 'proxy-view')
		)
		SELECT t.organization_id, t.campaign_id, t.subscriber_id,
		       `+alignmentISPCase("COALESCE(t.recipient_domain,'')")+`,
		       (t.event_at AT TIME ZONE 'America/Denver')::date,
		       COUNT(*)
		FROM clicked t
		WHERE t.organization_id = $1
		  AND t.campaign_id IS NOT NULL
		  AND t.subscriber_id IS NOT NULL
		  AND `+alignmentMoneyClickPredicate+`
		GROUP BY 1, 2, 3, 4, 5
	`, orgID, scanFrom); err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	return tx.Commit()
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
// window. offer_id is the identity: most campaigns carry it and no offer_key,
// including all of eHealth (1,101 of 1,255 conversions, 7d to 2026-10-31).
// Campaigns without offer_id fall back to stamped offer_key values ∪
// board-name tokens. Ranked by activity, capped at offerAlignmentMaxOffers.
func (s *Server) offerAlignmentOffers(ctx context.Context, db alignmentDB, orgID string, from, to time.Time) ([]alignmentOfferRef, error) {
	type agg struct {
		name   string
		weight int64
	}
	offers := map[string]*agg{}

	// (a) offers by id, weighted by campaign count.
	// $1 org, $2 from, $3 to.
	rows, err := db.QueryContext(ctx, `
		SELECT c.offer_id::text, COALESCE(min(o.name), ''), COUNT(*)
		FROM mailing_campaigns c
		LEFT JOIN mailing_offers o ON o.id = c.offer_id
		WHERE c.organization_id = $1
		  AND c.offer_id IS NOT NULL
		  AND c.scheduled_at >= $2 AND c.scheduled_at <= $3
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
		offers[key] = &agg{name: name, weight: n * 1000}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// (a2) stamped offer keys on campaigns with no offer_id.
	rows, err = db.QueryContext(ctx, `
		SELECT lower(offer_key), COUNT(*)
		FROM mailing_campaigns
		WHERE organization_id = $1
		  AND COALESCE(offer_key,'') <> ''
		  AND offer_id IS NULL
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
	rows, err = db.QueryContext(ctx, `
		WITH named AS (
			SELECT lower(substring(name FROM ' - ([A-Za-z0-9_-]+)$')) AS slug
			FROM mailing_campaigns
			WHERE organization_id = $1
			  AND COALESCE(offer_key,'') = ''
			  AND offer_id IS NULL
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
		       human_clicks, clickers, conversions, revenue::float8, revenue_known,
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
			stamped, inferred int
			refreshed         time.Time
		)
		if err := rows.Scan(&r.OfferKey, &r.OfferName, &r.ISP,
			&r.Delivered, &r.Hard, &r.Soft, &r.ReputationBlock, &r.Deferred,
			&r.HumanClicks, &r.Clickers, &r.Conversions, &r.Revenue, &r.RevenueKnown,
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
		r.ClickerRate = alignmentDiv(float64(r.Clickers), float64(r.Delivered))
		r.RPM = alignmentDiv(r.Revenue*1000, float64(r.Delivered))
		r.EPC = alignmentDiv(r.Revenue, float64(r.HumanClicks))
		r.AttributionCoverage = alignmentDiv(float64(stamped), float64(stamped+inferred))
		out = append(out, r)
	}
	return out, maxRefresh, built, rows.Err()
}
