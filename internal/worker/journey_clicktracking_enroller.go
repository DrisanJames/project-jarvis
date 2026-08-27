package worker

// JourneyClickTrackingEnroller is the INTERNAL-click producer for the
// click-drip queue. It exists because Everflow's per-offer click postbacks
// don't reliably reach us (they fire on conversion and macro substitution is
// flaky), so the real click stream — tens of thousands of internal click
// events per day in mailing_tracking_events — never entered the drip.
//
// Pipeline position:
//
//	mailing_tracking_events (event_type='clicked', money URL)
//	      │  this worker: resolve offer from the trailing slug
//	      ▼
//	mailing_journey_event_triggers (source='internal_click_tracking')
//	      │  JourneyEventEnroller (existing) drains + gates + enrolls
//	      ▼
//	mailing_journey_enrollments → JourneyExecutor → reminder emails
//
// By writing into the SAME trigger queue the Everflow path uses, every
// downstream guarantee is reused unchanged: journey-map gating (offer mapped /
// enabled / non-CPC), already-converted suppression skip, 24h idempotency,
// sending-profile resolution from the original campaign, and the
// ON CONFLICT (journey_id, subscriber_email) enrollment dedup. This worker only
// adds the missing inlet.
//
// Offer identity: mailing_tracking_events.offer_id is 100% NULL in production,
// but the offer is encoded in link_url:
//   - cratoolpro: https://www.cratoolpro.com/BJB4Q5BF/<SLUG>/
//   - eos57ytf (Sam's Club, NDR): https://www.eos57ytf.com/K4C5ZLC/<SLUG>/
//   - xnonu (Empire Today): https://www.xnonu.com/TQ5MX18J/<SLUG>/
//   - muqes (Home Repairly): https://www.muqes.com/TQ5MX18J/<SLUG>/
// mailing_offer_slug_map is the verified slug → everflow_offer_id dictionary; a
// click whose slug is not in the map (or maps to an offer with no journey) is
// skipped.
//
// Multi-instance safety: the server runs on multiple ECS tasks, so this worker
// runs N times concurrently. A single shared cursor row
// (mailing_clickdrip_enroller_cursor) is advanced under SELECT ... FOR UPDATE,
// which serializes time-slice ownership across instances — only one task
// processes any given slice. A per-(subscriber, offer) 24h trigger-dedup guards
// the boundary case.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
)

// Tunables. Click-drip's first reminder is +1h, so enrollment latency of a few
// seconds is irrelevant; we favor a calm cadence and a bounded per-tick scan.
const (
	DefaultClickTrackingInterval   = 30 * time.Second
	DefaultClickTrackingBatchLimit = 500
	// clickTrackingMaxSliceMinutes bounds how much wall-clock time a single
	// tick scans, so a worker that fell behind (deploy, downtime) catches up
	// over several small scans instead of one giant ILIKE table scan.
	clickTrackingMaxSliceMinutes = 15
)

// cratoolproSlugRe extracts the trailing offer slug from a cratoolpro money URL.
// Example: https://www.cratoolpro.com/BJB4Q5BF/KW3Q1DJ/?source_id=email...
var cratoolproSlugRe = regexp.MustCompile(`cratoolpro\.com/BJB4Q5BF/([A-Za-z0-9_-]+)`)

// eos57ytfSlugRe extracts the trailing offer slug from an eos57ytf affiliate
// money URL — both numeric PS#### slugs (Sam's Club PS8241) and alphanumeric
// slugs (NDR 2HH43PB after the 2026-06-11 migration off cratoolpro GK847MZ).
// Example: https://www.eos57ytf.com/K4C5ZLC/2HH43PB/?source_id=email...
var eos57ytfSlugRe = regexp.MustCompile(`(?i)eos57ytf\.com/K4C5ZLC/([A-Za-z0-9_-]+)`)

// xnonuSlugRe extracts the trailing offer slug from an xnonu affiliate money
// URL (Empire Today Flooring, 2026-06-11).
// Example: https://www.xnonu.com/TQ5MX18J/XF1SR2CS/?source_id=email...
var xnonuSlugRe = regexp.MustCompile(`(?i)xnonu\.com/TQ5MX18J/([A-Za-z0-9_-]+)`)

// k8k0hfdtSlugRe extracts the trailing offer slug from a k8k0hfdt affiliate
// money URL (Metal Roofing canonical link as of 2026-07-02, operator-supplied).
// Example: https://www.k8k0hfdt.com/3QJ6DW/3LKS16/?source_id=email...
var k8k0hfdtSlugRe = regexp.MustCompile(`(?i)k8k0hfdt\.com/3QJ6DW/([A-Za-z0-9_-]+)`)

// muqesSlugRe extracts the trailing offer slug from a muqes affiliate money
// URL (Home Repairly Roofing, 2026-06-11).
// Example: https://www.muqes.com/TQ5MX18J/XLRZDZ8K/?source_id=email...
var muqesSlugRe = regexp.MustCompile(`(?i)muqes\.com/TQ5MX18J/([A-Za-z0-9_-]+)`)

// codefortwoSlugRe extracts the trailing offer slug from a codefortwo
// affiliate money URL. 2026-08-03: BOTH August CPM headliners ride this
// network — Metal Roofing (K4C5ZLC/S6WFF5, operator 2026-07-31) and Liberty
// Mutual (K4C5ZLC/KQCKQ7, operator 2026-08-02). Its absence here was the
// aug01 inlet host-list drift class recurring: clicks on these offers never
// reached the enroller. Example:
// https://www.codefortwo.com/K4C5ZLC/S6WFF5/?source_id=email...
var codefortwoSlugRe = regexp.MustCompile(`(?i)codefortwo\.com/K4C5ZLC/([A-Za-z0-9_-]+)`)

// moneySlugRes is the ordered list of per-network slug extractors. Each
// captures the slug as submatch 1, already in the same form the
// mailing_offer_slug_map dictionary keys use.
var moneySlugRes = []*regexp.Regexp{cratoolproSlugRe, eos57ytfSlugRe, xnonuSlugRe, muqesSlugRe, k8k0hfdtSlugRe, codefortwoSlugRe}

// affiliateSlugRe is the legacy PS#### extractor kept as a last-resort
// fallback for affiliate URLs on hosts not covered by moneySlugRes. Example:
// https://www.eos57ytf.com/K4C5ZLC/PS8241/?creative_id=4989&source_id=email...
var affiliateSlugRe = regexp.MustCompile(`(?i)/PS([0-9]+)(?:/|\?|&|$)`)

// consumerSignal is a "click this offer → enroll into a DIFFERENT offer's drip"
// redirect, loaded from mailing_consumer_signal_slugs. Operator directive
// 2026-06-06: a click on Warby Parker (K5C8PQQ) or TruGreen (BXPFT55) marks the
// subscriber as a consumer and funnels them into the Sam's Club (offer 420)
// drip — NOT a Warby/TruGreen drip (those have none).
//
// A redirect cannot be expressed as a plain slug→offer map row because the
// reminder body (journey_event_enroller reads body_html from sub3_campaign_id)
// must be the TARGET offer's creative, not the clicked Warby/TruGreen creative.
// So a redirect carries both the target offer id and the name pattern used to
// resolve the clicker's brand TARGET-offer campaign for sub3.
type consumerSignal struct {
	targetOffer string // everflow_offer_id of the drip to enroll into (e.g. "420")
	namePattern string // ILIKE pattern to find that brand's target-offer campaign
}

// laneRouting is one offer lane's governor-controlled routing directive,
// loaded from mailing_offer_journey_map (routing_state / redirect_offer_id —
// written by JourneyLaneGovernor, 2026-07-10). 'paused_auto' drops new clicks
// for the lane; 'redirect' funnels them into redirectOffer's drip exactly
// like a consumer-signal redirect (target offer's creative, never the clicked
// one).
type laneRouting struct {
	state         string // active | paused_auto | redirect
	redirectOffer string // target everflow_offer_id when state='redirect'
}

// JourneyClickTrackingEnroller is the worker handle.
type JourneyClickTrackingEnroller struct {
	db         *sql.DB
	interval   time.Duration
	batchLimit int

	// verdictFnMissing latches true when the scan discovers the ignite_*
	// verdict SQL functions are absent (fresh DB / skipped migration); the
	// scan then runs without the pre-enrollment verdict gate for the life of
	// the process (cron backstop still applies). Single tick goroutine.
	verdictFnMissing bool
	stopChan         chan struct{}
	stopOnce         sync.Once

	totalScanned int64
	totalQueued  int64
	totalSkipped int64
	totalErrors  int64
}

// NewJourneyClickTrackingEnroller wires the worker.
func NewJourneyClickTrackingEnroller(db *sql.DB) *JourneyClickTrackingEnroller {
	return &JourneyClickTrackingEnroller{
		db:         db,
		interval:   DefaultClickTrackingInterval,
		batchLimit: DefaultClickTrackingBatchLimit,
		stopChan:   make(chan struct{}),
	}
}

// WithInterval overrides the poll cadence (tests / incidents).
func (w *JourneyClickTrackingEnroller) WithInterval(d time.Duration) *JourneyClickTrackingEnroller {
	if d > 0 {
		w.interval = d
	}
	return w
}

// WithBatchLimit overrides the per-tick cap.
func (w *JourneyClickTrackingEnroller) WithBatchLimit(n int) *JourneyClickTrackingEnroller {
	if n > 0 {
		w.batchLimit = n
	}
	return w
}

// Start runs the polling loop until ctx is cancelled or Stop() is called.
func (w *JourneyClickTrackingEnroller) Start(ctx context.Context) {
	go func() {
		log.Printf("JourneyClickTrackingEnroller: started (interval=%s, batchLimit=%d)", w.interval, w.batchLimit)
		w.tick(ctx)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.tick(ctx)
			case <-w.stopChan:
				log.Println("JourneyClickTrackingEnroller: stopped")
				return
			case <-ctx.Done():
				log.Println("JourneyClickTrackingEnroller: context cancelled, stopping")
				return
			}
		}
	}()
}

// Stop terminates the polling loop. Idempotent.
func (w *JourneyClickTrackingEnroller) Stop() {
	w.stopOnce.Do(func() { close(w.stopChan) })
}

// Stats returns lifetime counters for observability.
func (w *JourneyClickTrackingEnroller) Stats() (scanned, queued, skipped, errors int64) {
	return atomic.LoadInt64(&w.totalScanned),
		atomic.LoadInt64(&w.totalQueued),
		atomic.LoadInt64(&w.totalSkipped),
		atomic.LoadInt64(&w.totalErrors)
}

// clickCandidate is one money-URL click row from the cursor slice.
type clickCandidate struct {
	subscriberID  string
	eventAt       time.Time
	linkURL       string
	sendingDomain string
	campaignID    string
}

// tick processes one bounded slice of internal clicks under a cursor lock.
func (w *JourneyClickTrackingEnroller) tick(ctx context.Context) {
	tickCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// Ensure the singleton cursor row exists. Default last_event_at=NOW() means
	// a fresh deploy starts go-forward (no retroactive enrollment of historical
	// clickers, which would blast reminders at everyone at once).
	if _, err := w.db.ExecContext(tickCtx,
		`INSERT INTO mailing_clickdrip_enroller_cursor (id) VALUES (1) ON CONFLICT (id) DO NOTHING`); err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyClickTrackingEnroller: ensure cursor: %v", err)
		return
	}

	// Load the slug dictionary once per tick (8 rows; trivial).
	slugToOffer, err := w.loadSlugMap(tickCtx)
	if err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyClickTrackingEnroller: load slug map: %v", err)
		return
	}
	if len(slugToOffer) == 0 {
		return // nothing mapped yet
	}

	// Load consumer-signal redirects (Warby/TruGreen → Sam's Club) and, for each
	// distinct target-offer name pattern, the per-brand-root → most-recent
	// target-offer campaign map used to resolve sub3_campaign_id so the reminder
	// pitches the TARGET offer's creative. Loaded once per tick (a few rows /
	// ~14 brands); read-only, outside the cursor tx to minimize lock time.
	consumerSignals, err := w.loadConsumerSignals(tickCtx)
	if err != nil {
		// Non-fatal: a load failure must not stall the normal click→drip flow.
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyClickTrackingEnroller: load consumer signals: %v", err)
		consumerSignals = nil
	}
	brandCampaignByPattern := make(map[string]map[string]string)
	for _, sig := range consumerSignals {
		if _, done := brandCampaignByPattern[sig.namePattern]; done {
			continue
		}
		brandCampaignByPattern[sig.namePattern] = w.loadBrandCampaignMap(tickCtx, sig.namePattern)
	}

	// Load the lane governor's routing directives (paused_auto / redirect)
	// once per tick alongside the slug map. Non-fatal on error: an empty map
	// means every lane routes as 'active' — the enroller's processing-time
	// routing_state re-check is the backstop.
	laneRoutingByOffer, err := w.loadLaneRouting(tickCtx)
	if err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyClickTrackingEnroller: load lane routing: %v", err)
		laneRoutingByOffer = nil
	}

	tx, err := w.db.BeginTx(tickCtx, nil)
	if err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyClickTrackingEnroller: begin tx: %v", err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Serialize slice ownership across ECS tasks.
	var cursor time.Time
	if err := tx.QueryRowContext(tickCtx,
		`SELECT last_event_at FROM mailing_clickdrip_enroller_cursor WHERE id=1 FOR UPDATE`).Scan(&cursor); err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyClickTrackingEnroller: lock cursor: %v", err)
		return
	}

	// Upper bound: at most clickTrackingMaxSliceMinutes past the cursor, never
	// beyond NOW().
	now := time.Now().UTC()
	upper := cursor.Add(clickTrackingMaxSliceMinutes * time.Minute)
	if upper.After(now) {
		upper = now
	}
	if !upper.After(cursor) {
		// Cursor already at/after the cap; nothing to do, release the lock.
		committed = true
		_ = tx.Commit()
		return
	}

	rows, err := tx.QueryContext(tickCtx, clickTrackingScanQuery(w.verdictFnMissing), cursor, upper, w.batchLimit)
	if err != nil {
		// Fresh-DB / skipped-migration window: the default-on verdict
		// predicate depends on the ignite_* SQL functions (created by the
		// same startup-migration slice). If they are absent, fall back to the
		// unfiltered scan for the life of the process — the machine-click
		// gate cron remains the verdict backstop — instead of stalling the
		// whole click→trigger stage. The error aborted this tx; next tick
		// runs the fallback query.
		if !w.verdictFnMissing && strings.Contains(err.Error(), "ignite_event_verdict") &&
			strings.Contains(err.Error(), "does not exist") {
			w.verdictFnMissing = true
			log.Printf("JourneyClickTrackingEnroller: ignite_event_verdict missing — disabling pre-enrollment verdict gate for this process (cron backstop still applies)")
			return
		}
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyClickTrackingEnroller: scan clicks: %v", err)
		return
	}

	var cands []clickCandidate
	for rows.Next() {
		var c clickCandidate
		if err := rows.Scan(&c.subscriberID, &c.eventAt, &c.linkURL, &c.sendingDomain, &c.campaignID); err != nil {
			rows.Close()
			atomic.AddInt64(&w.totalErrors, 1)
			log.Printf("JourneyClickTrackingEnroller: scan row: %v", err)
			return
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyClickTrackingEnroller: rows err: %v", err)
		return
	}

	atomic.AddInt64(&w.totalScanned, int64(len(cands)))

	queued := 0
	for _, c := range cands {
		// Resolve the offer this click enrolls into. A consumer-signal slug
		// (Warby/TruGreen) REDIRECTS to a different drip (Sam's Club 420); any
		// other money slug maps to its own offer as usual.
		offerID := ""
		source := "internal_click_tracking"
		// sub3_campaign_id defaults to the campaign the subscriber clicked, so
		// the reminder reuses that creative. For a redirect it is instead the
		// brand's TARGET-offer campaign (set below).
		var campaignArg interface{} = ""
		if c.campaignID != "" {
			campaignArg = c.campaignID
		}

		slug, hasSlug := extractMoneySlug(c.linkURL)
		if sig, isSignal := consumerSignals[slug]; hasSlug && isSignal {
			// Redirect: enroll into the target offer's drip with the brand's
			// target-offer creative as the reminder body. If this brand has no
			// recent target-offer campaign we cannot pitch the redirected offer
			// without a misaligned creative/money-link — skip rather than send
			// the wrong thing.
			brandRoot := brand.Root(c.sendingDomain)
			targetCampaign := ""
			if m := brandCampaignByPattern[sig.namePattern]; m != nil {
				targetCampaign = m[brandRoot]
			}
			if targetCampaign == "" {
				atomic.AddInt64(&w.totalSkipped, 1)
				continue
			}
			offerID = sig.targetOffer
			campaignArg = targetCampaign
			source = "consumer_signal_redirect"
		} else if o, ok := resolveOfferFromLink(c.linkURL, slugToOffer); ok {
			// Honor the lane governor's routing directive for the resolved
			// offer (Unit D4, 2026-07-10).
			switch lr := laneRoutingByOffer[o]; lr.state {
			case "paused_auto":
				// Governor paused the lane — drop the click.
				atomic.AddInt64(&w.totalSkipped, 1)
				continue
			case "redirect":
				// Treat EXACTLY like a consumer-signal redirect: the reminder
				// must pitch the TARGET offer's creative, resolved per brand
				// via the same name-pattern machinery. The pattern comes from
				// the consumer-signal rows targeting the redirect offer (the
				// same pattern source loadConsumerSignals uses); no pattern or
				// no recent brand campaign → skip rather than misalign the
				// creative/money-link.
				pattern := patternForTargetOffer(consumerSignals, lr.redirectOffer)
				targetCampaign := ""
				if pattern != "" {
					brandRoot := brand.Root(c.sendingDomain)
					if m := brandCampaignByPattern[pattern]; m != nil {
						targetCampaign = m[brandRoot]
					}
				}
				if lr.redirectOffer == "" || targetCampaign == "" {
					// Loud skip: a governor redirect that cannot resolve a
					// creative-safe target must not eat clicks silently
					// (empty pattern = redirect target is not among the
					// consumer-signal target offers; empty campaign = this
					// brand has no recent target-offer creative).
					log.Printf("JourneyClickTrackingEnroller: redirect skip offer=%s target=%q pattern=%q brand=%s campaignResolved=%t",
						o, lr.redirectOffer, pattern, brand.Root(c.sendingDomain), targetCampaign != "")
					atomic.AddInt64(&w.totalSkipped, 1)
					continue
				}
				offerID = lr.redirectOffer
				campaignArg = targetCampaign
				source = "lane_governor_redirect"
			default:
				offerID = o
			}
		} else {
			atomic.AddInt64(&w.totalSkipped, 1)
			continue
		}

		// Per-(subscriber, offer) 24h dedup — keeps the trigger queue tidy and
		// guards the cursor-boundary reprocess case. (The downstream enroller
		// also dedups, but suppressing here avoids noise.) For a redirect this
		// dedups against the TARGET offer, so two consumer-signal clicks (or one
		// Sam's Club + one Warby click) within 24h enroll the subscriber once.
		var dup int
		if err := tx.QueryRowContext(tickCtx, `
			SELECT 1 FROM mailing_journey_event_triggers
			WHERE subscriber_id=$1 AND everflow_offer_id=$2
			  AND received_at > NOW() - INTERVAL '24 hours'
			LIMIT 1
		`, c.subscriberID, offerID).Scan(&dup); err == nil {
			atomic.AddInt64(&w.totalSkipped, 1)
			continue
		} else if err != sql.ErrNoRows {
			atomic.AddInt64(&w.totalErrors, 1)
			log.Printf("JourneyClickTrackingEnroller: dedup check: %v", err)
			continue
		}

		if _, err := tx.ExecContext(tickCtx, `
			INSERT INTO mailing_journey_event_triggers (
				id, source, everflow_offer_id, subscriber_id, subscriber_email,
				sub2_brand, sub3_campaign_id, click_id, sending_profile_id,
				sending_domain, click_url, status, received_at
			) VALUES (
				gen_random_uuid(), $1, $2, $3, NULL,
				$4, $5, '', NULL,
				$6, $7, 'pending', NOW()
			)
		`,
			source,
			offerID,
			c.subscriberID,
			brand.Root(c.sendingDomain),
			campaignArg,
			c.sendingDomain,
			c.linkURL,
		); err != nil {
			atomic.AddInt64(&w.totalErrors, 1)
			log.Printf("JourneyClickTrackingEnroller: insert trigger (offer=%s sub=%s): %v", offerID, c.subscriberID, err)
			continue
		}
		queued++
	}

	// Advance the cursor. If we hit the batch limit there may be more clicks in
	// (lastEventAt, upper]; resume from the last row's event_at next tick.
	// Otherwise advance to the slice upper bound.
	newCursor := upper
	if len(cands) >= w.batchLimit && len(cands) > 0 {
		newCursor = cands[len(cands)-1].eventAt
	}
	if _, err := tx.ExecContext(tickCtx,
		`UPDATE mailing_clickdrip_enroller_cursor SET last_event_at=$1, updated_at=NOW() WHERE id=1`,
		newCursor); err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyClickTrackingEnroller: advance cursor: %v", err)
		return
	}

	if err := tx.Commit(); err != nil {
		atomic.AddInt64(&w.totalErrors, 1)
		log.Printf("JourneyClickTrackingEnroller: commit: %v", err)
		return
	}
	committed = true

	atomic.AddInt64(&w.totalQueued, int64(queued))
	if queued > 0 || len(cands) > 0 {
		log.Printf("JourneyClickTrackingEnroller: slice (%s, %s] scanned=%d queued=%d (cursor→%s)",
			cursor.Format(time.RFC3339), upper.Format(time.RFC3339), len(cands), queued, newCursor.Format(time.RFC3339))
	}
}

// clickTrackingPreVerdictDisabled gates the pre-enrollment human-verdict
// predicate in the tick scan query. Truthy CLICKDRIP_PREVERDICT_DISABLED
// omits the predicate (legacy behavior: machine clicks enter the queue and
// only the downstream hygiene crons weed them).
func clickTrackingPreVerdictDisabled() bool {
	return envTruthy(os.Getenv("CLICKDRIP_PREVERDICT_DISABLED"))
}

// clickTrackingScanQuery builds the per-tick money-click scan SQL
// ($1=cursor, $2=upper, $3=limit). SIGNAL GRADING (operator 2026-07-18): a
// money-link CLICK is the strongest possible human signal (GOLD) — MPP/proxy
// cannot manufacture one — and the verdict is a scanner-STORM filter, not a
// human detector (~20% false-neg on proven converters; SafeLinks/proxy clicks
// verdict machine). So the pre-enrollment gate now EXCLUDES ONLY the seed-farm
// ('farm' verdict = 75.98.0.0/16, the one tried-and-true machine signal), never
// the whole machine/unknown bucket. Truthy CLICKDRIP_PREVERDICT_DISABLED still
// omits even the farm predicate. The .scratch/journey_machine_click_gate.py
// cron applies EXACTLY this farm-only predicate retroactively as the backstop.
func clickTrackingScanQuery(verdictFnMissing bool) string {
	verdictPred := ""
	if !clickTrackingPreVerdictDisabled() && !verdictFnMissing {
		verdictPred = "AND COALESCE(t.click_verdict, ignite_event_verdict(t.user_agent, t.ip_address)) <> 'farm'\n\t\t  "
	}
	return fmt.Sprintf(`
		SELECT t.subscriber_id::text, t.event_at, t.link_url,
		       COALESCE(t.sending_domain,''), COALESCE(t.campaign_id::text,'')
		FROM mailing_tracking_events t
		WHERE t.event_type='clicked'
		  AND t.event_at > $1 AND t.event_at <= $2
		  %sAND (
		        t.link_url ILIKE '%%cratoolpro.com/BJB4Q5BF/%%'
		     OR t.link_url ILIKE '%%eos57ytf.com/K4C5ZLC/%%'
		     OR t.link_url ILIKE '%%xnonu.com/TQ5MX18J/%%'
		     OR t.link_url ILIKE '%%muqes.com/TQ5MX18J/%%'
		     OR t.link_url ILIKE '%%k8k0hfdt.com/3QJ6DW/%%'
		     OR t.link_url ILIKE '%%codefortwo.com/K4C5ZLC/%%'
		      )
		  AND NOT EXISTS (
		        SELECT 1 FROM mailing_campaigns c
		        WHERE c.id = t.campaign_id
		          AND (c.campaign_type = 'click_drip'
		               -- Converter-journey thank-you clicks are consent signals for the
		               -- CONVERTER trigger sequences, never generic-drip enrollments
		               -- (operator ruling 2026-07-27; QA gauntlet §4).
		               OR c.name LIKE '%%CONVERTER-THANKYOU%%'
		               OR c.name LIKE '%%ABANDON-RECOVERY%%')
		      )
		ORDER BY t.event_at ASC
		LIMIT $3
	`, verdictPred)
}

// loadLaneRouting returns the governor's per-offer routing directives from
// mailing_offer_journey_map. Rows default to state='active' via COALESCE, so
// a pre-governor DB (columns just added by startup migration) behaves exactly
// like today. Missing-column errors are tolerated the same way
// loadConsumerSignals tolerates a missing table (fresh DB before migrations).
func (w *JourneyClickTrackingEnroller) loadLaneRouting(ctx context.Context) (map[string]laneRouting, error) {
	out := make(map[string]laneRouting)
	rows, err := w.db.QueryContext(ctx, `
		SELECT everflow_offer_id, COALESCE(routing_state,'active'), COALESCE(redirect_offer_id,'')
		FROM mailing_offer_journey_map
	`)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return out, nil
		}
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var offer, state, redirect string
		if err := rows.Scan(&offer, &state, &redirect); err != nil {
			return nil, err
		}
		out[offer] = laneRouting{state: state, redirectOffer: redirect}
	}
	return out, rows.Err()
}

// patternForTargetOffer resolves the campaign name pattern for a lane-governor
// redirect target. Redirect targets reuse the consumer-signal pattern source
// (mailing_consumer_signal_slugs.target_campaign_name_ilike): the first
// enabled signal row whose target offer matches supplies the pattern. Returns
// "" when the target has no pattern — the caller skips rather than pitch a
// misaligned creative.
func patternForTargetOffer(signals map[string]consumerSignal, targetOffer string) string {
	if targetOffer == "" {
		return ""
	}
	for _, sig := range signals {
		if sig.targetOffer == targetOffer && sig.namePattern != "" {
			return sig.namePattern
		}
	}
	return ""
}

// loadSlugMap returns enabled slug → everflow_offer_id mappings.
func (w *JourneyClickTrackingEnroller) loadSlugMap(ctx context.Context) (map[string]string, error) {
	rows, err := w.db.QueryContext(ctx,
		`SELECT cratoolpro_slug, everflow_offer_id FROM mailing_offer_slug_map WHERE enabled=TRUE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var slug, offer string
		if err := rows.Scan(&slug, &offer); err != nil {
			return nil, err
		}
		out[normalizeSlug(slug)] = offer
	}
	return out, rows.Err()
}

// loadConsumerSignals returns enabled consumer-signal redirects keyed by
// normalized slug. A click on one of these slugs enrolls the subscriber into
// the target offer's drip (with that offer's creative) instead of the clicked
// offer. Empty/missing table is fine — returns an empty map.
func (w *JourneyClickTrackingEnroller) loadConsumerSignals(ctx context.Context) (map[string]consumerSignal, error) {
	out := make(map[string]consumerSignal)
	rows, err := w.db.QueryContext(ctx,
		`SELECT slug, target_everflow_offer_id, target_campaign_name_ilike
		   FROM mailing_consumer_signal_slugs WHERE enabled=TRUE`)
	if err != nil {
		// Table may not exist yet on a fresh DB before startup migrations run.
		if strings.Contains(err.Error(), "does not exist") {
			return out, nil
		}
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var slug, target, pattern string
		if err := rows.Scan(&slug, &target, &pattern); err != nil {
			return nil, err
		}
		if target == "" || pattern == "" {
			continue
		}
		out[normalizeSlug(slug)] = consumerSignal{targetOffer: target, namePattern: pattern}
	}
	return out, rows.Err()
}

// loadBrandCampaignMap returns, for each brand root, the most-recent campaign id
// whose name matches namePattern and that carries a usable creative — the source
// of the reminder body for a consumer-signal redirect. Sam's Club campaigns are
// not offer_id-linked in production, so name matching is the only reliable
// signal. brand.Root() collapses the em.<brand> click domain and the m.<brand>
// Sam's Club send domain to the same root. Returns an empty map on error so a
// failed load simply means redirects skip (never a misaligned send).
func (w *JourneyClickTrackingEnroller) loadBrandCampaignMap(ctx context.Context, namePattern string) map[string]string {
	out := make(map[string]string)
	rows, err := w.db.QueryContext(ctx, `
		SELECT c.id::text, COALESCE(p.sending_domain,'')
		FROM mailing_campaigns c
		LEFT JOIN mailing_sending_profiles p ON p.id = c.sending_profile_id
		WHERE c.name ILIKE $1
		  AND c.name NOT ILIKE '%proof%'
		  AND c.campaign_type <> 'click_drip'
		  AND c.html_content IS NOT NULL AND length(c.html_content) > 0
		  AND c.created_at >= NOW() - INTERVAL '14 days'
		ORDER BY c.created_at DESC
	`, namePattern)
	if err != nil {
		log.Printf("JourneyClickTrackingEnroller: load brand campaign map (%q): %v", namePattern, err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, dom string
		if err := rows.Scan(&id, &dom); err != nil {
			continue
		}
		root := brand.Root(dom)
		if root == "" {
			continue
		}
		if _, exists := out[root]; !exists {
			out[root] = id // first row per brand = most recent (ORDER BY created_at DESC)
		}
	}
	return out
}

// extractMoneySlug pulls the normalized offer slug from a money URL, handling
// the cratoolpro publisher path and the eos57ytf / xnonu affiliate paths
// (plus the legacy PS#### fallback). ok=false when the link carries no
// recognizable money slug.
func extractMoneySlug(linkURL string) (string, bool) {
	for _, re := range moneySlugRes {
		if m := re.FindStringSubmatch(linkURL); len(m) >= 2 {
			return normalizeSlug(m[1]), true
		}
	}
	if m := affiliateSlugRe.FindStringSubmatch(linkURL); len(m) >= 2 {
		return normalizeSlug("PS" + m[1]), true
	}
	return "", false
}

// resolveOfferFromLink extracts a money-URL slug and maps it to an everflow
// offer id. ok=false when the link carries no recognizable slug or the slug is
// not in the dictionary.
func resolveOfferFromLink(linkURL string, slugToOffer map[string]string) (string, bool) {
	for _, re := range moneySlugRes {
		if m := re.FindStringSubmatch(linkURL); len(m) >= 2 {
			if offer, ok := slugToOffer[normalizeSlug(m[1])]; ok {
				return offer, true
			}
		}
	}
	if m := affiliateSlugRe.FindStringSubmatch(linkURL); len(m) >= 2 {
		if offer, ok := slugToOffer[normalizeSlug("PS"+m[1])]; ok {
			return offer, true
		}
	}
	return "", false
}

// normalizeSlug upper-cases and trims a slug so map lookups are case-stable.
func normalizeSlug(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}
