package worker

// PartnerDripOrchestrator schedules 15-minute mini-campaigns from the
// partner_clean_queue. Each tick:
//   1. For each active vertical, compute wave_size from queue depth and
//      the dataset's flush_window_hours.
//   2. Round-robin pick a brand from BRANDS, skipping paused brands.
//   3. Resolve creative for (vertical, brand) from partner_drip_creatives.
//   4. Apply ISP-rate-limit deferral and per-dataset distribution overrides.
//   5. Atomically claim N FIFO records (status: ready -> claimed).
//   6. Promote claimed records to mailing_subscribers with full provenance
//      columns populated (the data_pipeline.go bug regression: that worker
//      forgot to populate source_system/source_detail/source_metadata).
//   7. Create a per-wave static segment containing exactly the claimed
//      record IDs so the existing audience finalizer picks them up.
//   8. Build a PMTACampaignInput mirroring deploy_may12_mature.py and call
//      DeployFn (in-process — no HTTP self-call).
//   9. Mark records mailed with the resulting campaign_id.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// Brands in deterministic round-robin order. The first 4 are the mature
// brands launched in 2026-04 (db/ht/mh/qf) — they have full per-ISP IP
// pool isolation and the operator's highest cap budgets. The remaining 12
// were brought online in 2026-05-08 / 05-09 / 05-09b and use general-pool
// IPs. Both tiers ship the same partner-drip creatives — the per-ISP cap
// table below is what controls cumulative volume to each ISP per wave.
//
// All 16 brands have:
//   - mailing_sending_profiles row (PMTA, status=active)
//   - per-brand newsletter creatives in docs/emails/ for both the welcome
//     family (amerisave/personal-loans/optima-tax-relief/renewal-by-andersen)
//     and the follow-up families (trugreen + the-capital-wallet)
//   - image-CDN domain entry in mailing_image_domains (verified)
//
// 2026-05-14 expansion ("expand our ingestion system across ALL properties"):
// adds the 12 new-brand codes after the 4 mature codes so the round-robin
// kicks off mature-first and only spills into newer-brand IPs once those
// come up in the rotation. Per-ISP caps still cap cumulative volume.
var dripBrands = []string{
	// Mature 4 — full per-ISP pool isolation since 2026-04.
	"db", "ht", "mh", "qf",
	// New-brand expansion 2026-05-14 — general-pool IPs, gentler caps.
	"bwp", "ci", "cp", "fc", "hws", "lpl", "mrd", "rb", "rru", "tot", "wfy", "yih",
}

var brandSendingDomain = map[string]string{
	// Mature 4
	"db":  "em.discountblog.com",
	"ht":  "em.historythinking.com",
	"mh":  "em.myownhealth.net",
	"qf":  "em.quizfiesta.com",
	// New-brand expansion 2026-05-14
	"bwp": "em.businessweeklypro.com",
	"ci":  "em.casainsure.com",
	"cp":  "em.consumerpro.net",
	"fc":  "em.financialcalculate.com",
	"hws": "em.homewarrantyservices.org",
	"lpl": "em.learnpersonalloans.com",
	"mrd": "em.myrepairdiy.com",
	"rb":  "em.ratesbazar.com",
	"rru": "em.refinanceratesusa.com",
	"tot": "em.thingoftheday.org",
	"wfy": "em.warrantyforyou.com",
	"yih": "em.yourinsurancehub.com",
}

// BrandSendingDomain exposes the orchestrator's brand → sending-domain
// mapping to other packages (notably cmd/server/main.go's
// PausedBrandPredicate wiring) so callers stay in sync with the canonical
// dripBrands list. Returns ("", false) for unknown brands. Adding a brand
// to the orchestrator means updating brandSendingDomain in this file
// only — no cross-package map duplication.
func BrandSendingDomain(brand string) (string, bool) {
	d, ok := brandSendingDomain[brand]
	return d, ok
}

// followupTouchGapHours is the minimum gap between consecutive touches.
// Operator directive 2026-05-14: "campaigns should run daily" — 24h is
// the most defensible cadence (matches inbox-fatigue research and the
// industry's three-touch nurture cycle).
const followupTouchGapHours = 24

// MaxTouchCount is the terminal touch number. After this many touches a
// recipient is permanently retired from the drip rotation regardless of
// engagement state.
const MaxTouchCount = 4

// dataPartnerMasterListID is seeded by startup migration dp_seed_master_list.
const dataPartnerMasterListID = "00000000-0000-0000-0000-0000d4ada4a7"

// CampaignDeployFn is the in-process call signature for HandleDeployCampaign.
// We accept a func so the worker package doesn't need to import internal/api.
type CampaignDeployFn func(ctx context.Context, input engine.PMTACampaignInput) (campaignID string, err error)

// PartnerDripOrchestratorConfig holds runtime knobs.
type PartnerDripOrchestratorConfig struct {
	OrganizationID    string
	TickInterval      time.Duration // default 15 minutes
	MinWaveSize       int           // default 25
	MaxWaveSize       int           // default 5000
	WindowHours       int           // PMTA wave window in hours (default 8 — bypass the source-field sanity check)
	CreativesDir      string        // path to docs/emails (defaults to "docs/emails")
	DeployFn          CampaignDeployFn
	PausedBrandPredicate func(ctx context.Context, brand string) bool // optional — return true to skip brand
	// PerISPCapPerWave caps how many records per ISP family one wave may
	// claim. Protects ISPs from cumulative drip + Welcome + Engager volume
	// overshooting the published caps (Gmail 5000/brand/day, Yahoo 500/brand/day).
	// Default mirrors the conservative drip allotment: gmail=150, yahoo=20, other=100.
	PerISPCapPerWave map[string]int
	// ThrottledISPRateThreshold (msgs_per_hour) below which an ISP is considered
	// in active backoff and that ISP's portion of the wave is deferred. Default 50.
	ThrottledISPRateThreshold float64
	// BrandsPerTick fires up to N brands' welcome waves per vertical per
	// tick (in parallel). Default 4. With 16 brands round-robin and 4
	// brands per tick, the rotation completes one full cycle in 4 ticks
	// (1 hour at 15min cadence). This is what unlocks the "expand
	// ingestion across ALL properties = throughput up" property — without
	// it, adding 12 new brands stretches the rotation 4x and per-brand
	// throughput stays the same.
	BrandsPerTick int
	// FollowupBrandsPerTick mirrors BrandsPerTick but for the follow-up
	// touches. Default 4. The follow-up loop runs against a synthetic
	// 'followup' vertical so its brand-rotation index is independent of
	// the welcome-touch round-robin.
	FollowupBrandsPerTick int
	// MaxFollowupClaimPerVertical caps how many records the follow-up
	// loop will claim per vertical per tick (across all brands), as a
	// safety ceiling. Default 5000 (matches MaxWaveSize). Set to 0 to
	// disable the follow-up loop entirely (kill switch).
	MaxFollowupClaimPerVertical int
	// ClaimedJanitorMaxAge releases partner_clean_queue rows stuck in
	// 'claimed' after a crash between claim and promote/deploy. Only
	// rows with no subscriber_id and no mailed_campaign_id are touched.
	// Default 45m. Set to 0 to disable.
	ClaimedJanitorMaxAge time.Duration
}

type PartnerDripOrchestrator struct {
	db  *sql.DB
	cfg PartnerDripOrchestratorConfig

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	startOnce sync.Once
	stopOnce  sync.Once
}

func NewPartnerDripOrchestrator(db *sql.DB, cfg PartnerDripOrchestratorConfig) *PartnerDripOrchestrator {
	if cfg.OrganizationID == "" {
		cfg.OrganizationID = "00000000-0000-0000-0000-000000000001"
	}
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = 15 * time.Minute
	}
	if cfg.MinWaveSize <= 0 {
		cfg.MinWaveSize = 25
	}
	if cfg.MaxWaveSize <= 0 {
		cfg.MaxWaveSize = 5000
	}
	if cfg.WindowHours <= 0 {
		cfg.WindowHours = 8 // matches the wave_sanity check minimum
	}
	if cfg.CreativesDir == "" {
		cfg.CreativesDir = "docs/emails"
	}
	if cfg.PerISPCapPerWave == nil {
		// Per-ISP per-wave caps. With 4 waves/hour (15-min ticks) and 4
		// brands round-robin, each brand sees one wave per hour, so:
		//
		//   per_brand_per_day = cap * 24
		//
		// Caps below stay under each ISP's published per-brand-per-day
		// guidance with ~5–10% headroom:
		//
		//   gmail     200 -> 4,800/brand/day (Google: 5,000/sender/day)
		//   microsoft 200 -> 4,800/brand/day (no published cap; warm reputation)
		//   apple     200 -> 4,800/brand/day (no published cap; warm reputation)
		//   yahoo     20  -> 480/brand/day  (Yahoo: 500/sender/day)
		//   aol       20  -> 480/brand/day  (matches Yahoo carve)
		//   other     150 -> 3,600/brand/day (mixed deliverability bucket)
		//
		// 2026-05-14 bump: gmail 150->200, microsoft/apple 100->200,
		// other 100->150, comcast/charter 60->100. Operator directive
		// "INCREASE THROUGHPUT. Especially for GMAIL." after observing
		// David Cal's Personal Loans feed shipping 235 gmail/day vs a
		// 2,334-record queue.
		//
		// Pairs with the per-ISP claim strategy in claimRecordsByISPCaps —
		// without that change, raising caps alone has near-zero effect
		// because the old oldest-first claim wastes wave slots on ISPs
		// (yahoo) whose share of the queue dominates and whose cap is
		// then defer-released.
		cfg.PerISPCapPerWave = map[string]int{
			"gmail":     200,
			"yahoo":     20,
			"aol":       20,
			"microsoft": 200,
			"apple":     200,
			"comcast":   100,
			"charter":   100,
			"att":       60,
			"sbcglobal": 60,
			"cox":       60,
			"verizon":   60,
			"other":     150,
		}
	}
	if cfg.ThrottledISPRateThreshold <= 0 {
		cfg.ThrottledISPRateThreshold = 50.0
	}
	if cfg.BrandsPerTick <= 0 {
		cfg.BrandsPerTick = 4
	}
	if cfg.FollowupBrandsPerTick <= 0 {
		cfg.FollowupBrandsPerTick = 4
	}
	if cfg.MaxFollowupClaimPerVertical <= 0 {
		cfg.MaxFollowupClaimPerVertical = cfg.MaxWaveSize
	}
	if cfg.ClaimedJanitorMaxAge == 0 {
		cfg.ClaimedJanitorMaxAge = 45 * time.Minute
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &PartnerDripOrchestrator{db: db, cfg: cfg, ctx: ctx, cancel: cancel}
}

func (po *PartnerDripOrchestrator) Start() {
	po.startOnce.Do(func() {
		po.wg.Add(1)
		go po.run()
		log.Printf("[PartnerDripOrchestrator] started — tick=%s min_wave=%d max_wave=%d window_hours=%d brands_per_tick=%d followup_brands_per_tick=%d max_followup_claim=%d creatives_dir=%s brands=%d",
			po.cfg.TickInterval, po.cfg.MinWaveSize, po.cfg.MaxWaveSize, po.cfg.WindowHours,
			po.cfg.BrandsPerTick, po.cfg.FollowupBrandsPerTick, po.cfg.MaxFollowupClaimPerVertical,
			po.cfg.CreativesDir, len(dripBrands))
	})
}

func (po *PartnerDripOrchestrator) Stop() {
	po.stopOnce.Do(func() {
		po.cancel()
		po.wg.Wait()
		log.Println("[PartnerDripOrchestrator] stopped")
	})
}

func (po *PartnerDripOrchestrator) run() {
	defer po.wg.Done()
	t := time.NewTicker(po.cfg.TickInterval)
	defer t.Stop()
	// Don't wait an entire tick on first boot — drain any waiting backlog.
	po.tickOnce()
	for {
		select {
		case <-po.ctx.Done():
			return
		case <-t.C:
			po.tickOnce()
		}
	}
}

// tickOnce iterates over every active vertical with ready records and
// runs up to BrandsPerTick welcome waves per vertical per tick (in
// parallel brand selection — one wave per (vertical, brand) pair).
// Errors per-vertical are logged but do not stop the rest of the loop.
//
// After the welcome pass, runs the follow-up pass which scans for
// records whose next_touch_at has elapsed and ships them through one or
// more touches at FollowupBrandsPerTick per vertical.
func (po *PartnerDripOrchestrator) tickOnce() {
	if po.cfg.DeployFn == nil {
		log.Println("[PartnerDripOrchestrator] no DeployFn wired — skipping tick")
		return
	}
	if po.cfg.ClaimedJanitorMaxAge > 0 {
		if n, err := po.releaseStaleClaims(po.ctx); err != nil {
			log.Printf("[PartnerDripOrchestrator] claimed janitor: %v", err)
		} else if n > 0 {
			log.Printf("[PartnerDripOrchestrator] claimed janitor released %d stale rows", n)
		}
	}
	if n, err := po.reconcileShippedClaims(po.ctx); err != nil {
		log.Printf("[PartnerDripOrchestrator] reconcile shipped claims: %v", err)
	} else if n > 0 {
		log.Printf("[PartnerDripOrchestrator] reconciled %d claimed rows to mailed (post-deploy markMailed miss)", n)
	}
	verticals, err := po.activeVerticalsWithBacklog(po.ctx)
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] active_verticals: %v", err)
		return
	}
	for _, v := range verticals {
		if po.ctx.Err() != nil {
			return
		}
		// Fire up to BrandsPerTick welcome waves per vertical per tick.
		// Each call advances the brand round-robin pointer and processes
		// a fresh per-ISP-capped wave for that brand.
		brandsThisTick := po.cfg.BrandsPerTick
		if brandsThisTick > len(dripBrands) {
			brandsThisTick = len(dripBrands)
		}
		for i := 0; i < brandsThisTick; i++ {
			if po.ctx.Err() != nil {
				return
			}
			if err := po.processVertical(po.ctx, v); err != nil {
				log.Printf("[PartnerDripOrchestrator] welcome vertical=%s: %v", v.vertical, err)
				// Keep advancing brand index even on error — the next
				// brand's wave is independent.
			}
			// Re-read the vertical state for ready_count and brand_index
			// so the next call doesn't re-process the same brand. We
			// also bail out if no more ready records remain for this
			// vertical (the orchestrator's main goal is responsiveness).
			fresh, err := po.refreshVerticalState(po.ctx, v.vertical)
			if err != nil || fresh == nil || fresh.readyCount <= 0 {
				break
			}
			v = *fresh
		}
	}
	// Follow-up pass: independent of welcome pass, runs across the same
	// verticals (record-vertical, not brand-vertical). The 'followup'
	// drip-state row drives brand rotation for follow-ups.
	if po.cfg.MaxFollowupClaimPerVertical > 0 {
		po.tickFollowups(po.ctx)
	}
}

// refreshVerticalState pulls the up-to-date state of a single vertical
// from the DB. Used between intra-tick calls to processVertical so the
// brand pointer + ready-count reflect the wave we just shipped.
func (po *PartnerDripOrchestrator) refreshVerticalState(ctx context.Context, vertical string) (*verticalState, error) {
	rows, err := po.db.QueryContext(ctx, `
		WITH oldest AS (
			SELECT q.vertical, q.dataset_id, MIN(q.ingested_at) AS oldest_at,
			       COUNT(*) FILTER (WHERE q.status = 'ready') AS ready_total
			FROM partner_clean_queue q
			WHERE q.status = 'ready' AND q.vertical = $1
			GROUP BY q.vertical, q.dataset_id
		)
		SELECT s.vertical, s.next_brand_index,
		       (SELECT SUM(o.ready_total) FROM oldest o WHERE o.vertical = s.vertical) AS ready_total,
		       (SELECT MIN(o.oldest_at) FROM oldest o WHERE o.vertical = s.vertical) AS oldest_at,
		       d.id, d.slug, p.slug, p.name, d.flush_window_hours,
		       COALESCE(d.offer_id::text, '')
		FROM partner_drip_state s
		LEFT JOIN LATERAL (
			SELECT q.dataset_id
			FROM partner_clean_queue q
			WHERE q.vertical = s.vertical AND q.status = 'ready'
			ORDER BY q.ingested_at ASC
			LIMIT 1
		) AS dom ON true
		LEFT JOIN partner_datasets d ON d.id = dom.dataset_id
		LEFT JOIN data_partners p ON p.id = d.partner_id
		WHERE s.vertical = $1
	`, vertical)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nil
	}
	var v verticalState
	var (
		datasetID, datasetSlug, partnerSlug, partnerName sql.NullString
		flushHours                                       sql.NullInt64
		readyTotal                                       sql.NullInt64
		oldestAt                                         sql.NullTime
		offerID                                          string
	)
	if err := rows.Scan(&v.vertical, &v.brandIndex, &readyTotal, &oldestAt,
		&datasetID, &datasetSlug, &partnerSlug, &partnerName, &flushHours, &offerID); err != nil {
		return nil, err
	}
	v.offerID = offerID
	if readyTotal.Valid {
		v.readyCount = int(readyTotal.Int64)
	}
	v.oldestIngest = oldestAt
	if datasetID.Valid {
		v.datasetID = datasetID.String
	}
	if datasetSlug.Valid {
		v.datasetSlug = datasetSlug.String
	}
	if partnerSlug.Valid {
		v.partnerSlug = partnerSlug.String
	}
	if partnerName.Valid {
		v.partnerName = partnerName.String
	}
	if flushHours.Valid {
		v.flushHours = int(flushHours.Int64)
	}
	if v.flushHours <= 0 {
		v.flushHours = 24
	}
	return &v, nil
}

type verticalState struct {
	vertical       string
	brandIndex     int
	readyCount     int
	oldestIngest   sql.NullTime
	flushHours     int
	datasetID      string // dominant dataset id (for ISP overrides + flush window) — picked by oldest ingestion
	datasetSlug    string
	partnerSlug    string
	partnerName    string
	// OfferID is set when the dominant dataset for this vertical is bound to
	// a specific mailing_offers.id (vertical='direct_offer'). When set, the
	// orchestrator pulls creative + subject + from-name from the offer-center
	// tables and stamps OfferID on the deploy payload so the offer-suppression
	// Bloom filter applies and content_locked inherits from the offer. Empty
	// for the legacy 4-vertical drip pool.
	offerID string
}

func (po *PartnerDripOrchestrator) activeVerticalsWithBacklog(ctx context.Context) ([]verticalState, error) {
	// Pick the oldest ingest per vertical and the dataset/partner that owns it.
	rows, err := po.db.QueryContext(ctx, `
		WITH oldest AS (
			SELECT q.vertical, q.dataset_id, MIN(q.ingested_at) AS oldest_at,
			       COUNT(*) FILTER (WHERE q.status = 'ready') AS ready_total
			FROM partner_clean_queue q
			WHERE q.status = 'ready'
			GROUP BY q.vertical, q.dataset_id
		)
		SELECT s.vertical, s.next_brand_index,
		       (SELECT SUM(o.ready_total) FROM oldest o WHERE o.vertical = s.vertical) AS ready_total,
		       (SELECT MIN(o.oldest_at) FROM oldest o WHERE o.vertical = s.vertical) AS oldest_at,
		       d.id, d.slug, p.slug, p.name, d.flush_window_hours,
		       COALESCE(d.offer_id::text, '')
		FROM partner_drip_state s
		LEFT JOIN LATERAL (
			SELECT q.dataset_id
			FROM partner_clean_queue q
			WHERE q.vertical = s.vertical AND q.status = 'ready'
			ORDER BY q.ingested_at ASC
			LIMIT 1
		) AS dom ON true
		LEFT JOIN partner_datasets d ON d.id = dom.dataset_id
		LEFT JOIN data_partners p ON p.id = d.partner_id
		WHERE EXISTS (SELECT 1 FROM partner_clean_queue q WHERE q.vertical = s.vertical AND q.status = 'ready')
		ORDER BY s.vertical
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []verticalState
	for rows.Next() {
		var v verticalState
		var (
			datasetID, datasetSlug, partnerSlug, partnerName sql.NullString
			flushHours                                       sql.NullInt64
			readyTotal                                       sql.NullInt64
			oldestAt                                         sql.NullTime
			offerID                                          string
		)
		if err := rows.Scan(&v.vertical, &v.brandIndex, &readyTotal, &oldestAt,
			&datasetID, &datasetSlug, &partnerSlug, &partnerName, &flushHours, &offerID); err != nil {
			continue
		}
		v.offerID = offerID
		if !readyTotal.Valid || readyTotal.Int64 == 0 {
			continue
		}
		v.readyCount = int(readyTotal.Int64)
		v.oldestIngest = oldestAt
		if datasetID.Valid {
			v.datasetID = datasetID.String
		}
		if datasetSlug.Valid {
			v.datasetSlug = datasetSlug.String
		}
		if partnerSlug.Valid {
			v.partnerSlug = partnerSlug.String
		}
		if partnerName.Valid {
			v.partnerName = partnerName.String
		}
		if flushHours.Valid {
			v.flushHours = int(flushHours.Int64)
		}
		if v.flushHours <= 0 {
			v.flushHours = 24
		}
		out = append(out, v)
	}
	return out, nil
}

func (po *PartnerDripOrchestrator) processVertical(ctx context.Context, v verticalState) error {
	waveSize := po.computeWaveSize(v)
	if waveSize <= 0 {
		return nil
	}
	brand, newIdx, err := po.pickNextBrand(ctx, v)
	if err != nil {
		return fmt.Errorf("pick_brand: %w", err)
	}
	creative, err := po.resolveCreativeForVertical(ctx, v, brand)
	if err != nil {
		return fmt.Errorf("resolve_creative: %w", err)
	}

	// ISP-aware claim: pull up to `cap` records per ISP-family from the
	// queue (oldest-first within each ISP), bounded by waveSize as a
	// safety ceiling. Replaces the old oldest-first-across-all-ISPs claim
	// that wasted wave slots on yahoo records that would later be
	// deferred and released. See claimRecordsByISPCaps for the full
	// rationale and the May 14 cap bump.
	claimed, err := po.claimRecordsByISPCaps(ctx, v.vertical, po.cfg.PerISPCapPerWave, waveSize)
	if err != nil {
		return fmt.Errorf("claim_records: %w", err)
	}
	if len(claimed) == 0 {
		return nil
	}

	// Apply throttle-based ISP deferral + per-ISP cap. Records cut from this
	// wave are released back to 'ready' so the next tick can revisit them
	// (potentially against a different brand whose throttle state differs).
	keep, deferred, deferralReasons, err := po.applyThroughputSafety(ctx, claimed)
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] throughput_safety check failed for vertical=%s: %v — proceeding without deferral", v.vertical, err)
		keep = claimed
	}
	if len(deferred) > 0 {
		if relErr := po.releaseClaim(ctx, deferred); relErr != nil {
			log.Printf("[PartnerDripOrchestrator] release deferred: %v", relErr)
		}
		log.Printf("[PartnerDripOrchestrator] deferred %d records for vertical=%s reasons=%v",
			len(deferred), v.vertical, deferralReasons)
	}
	if len(keep) == 0 {
		log.Printf("[PartnerDripOrchestrator] vertical=%s: all claimed records deferred — skipping wave", v.vertical)
		return nil
	}
	claimed = keep

	subscriberIDs, err := po.promoteToSubscribers(ctx, v, claimed)
	if err != nil {
		_ = po.releaseClaim(ctx, claimed)
		return fmt.Errorf("promote_subscribers: %w", err)
	}

	segmentID, err := po.createWaveSegment(ctx, v, brand, claimed, subscriberIDs)
	if err != nil {
		_ = po.releaseClaim(ctx, claimed)
		return fmt.Errorf("create_wave_segment: %w", err)
	}

	ispCounts := tallyISPs(claimed)
	input, err := po.buildCampaignInput(v, brand, creative, segmentID, ispCounts)
	if err != nil {
		_ = po.releaseClaim(ctx, claimed)
		return fmt.Errorf("build_input: %w", err)
	}

	campaignID, err := po.cfg.DeployFn(ctx, input)
	if err != nil {
		_ = po.releaseClaim(ctx, claimed)
		return fmt.Errorf("deploy: %w", err)
	}

	// Stamp the partner attribution columns onto the campaign row so the
	// analytics dashboard can join campaigns back to this dataset. Best-effort
	// — a failure here doesn't unship the wave.
	if err := po.stampPartnerAttributionOnCampaign(ctx, campaignID, v.datasetID, v.partnerSlug, v.vertical); err != nil {
		log.Printf("[PartnerDripOrchestrator] stamp_attribution (campaign=%s dataset=%s): %v", campaignID, v.datasetID, err)
	}

	if err := po.markMailed(ctx, claimed, campaignID, brand); err != nil {
		log.Printf("[PartnerDripOrchestrator] mark_mailed (campaign %s already deployed!): %v", campaignID, err)
	}

	if err := po.updateDripState(ctx, v.vertical, newIdx, brand, campaignID, len(claimed)); err != nil {
		log.Printf("[PartnerDripOrchestrator] update_state: %v", err)
	}

	log.Printf("[PartnerDripOrchestrator] wave fired: vertical=%s brand=%s campaign=%s size=%d creative=%s",
		v.vertical, brand, campaignID, len(claimed), creative.filename)
	return nil
}

// computeWaveSize divides remaining queue by waves remaining in the flush
// window, clamped to MIN/MAX.
//
// 2026-05-14 change: the historical "spread evenly across the remaining
// flush window" formula (readyCount / wavesRemaining) was the dominant
// throughput choke. Early in a dataset's life-cycle wavesRemaining was
// near 96 and the wave shrank to ~280 records, far below the per-ISP
// cap budget — so gmail/microsoft/apple records trickled while the
// queue grew. With per-ISP caps now enforced natively at claim time
// (claimRecordsByISPCaps), the wave is naturally smoothed to
// sum(per-ISP caps) regardless of waveSize. Returning MaxWaveSize lets
// the per-ISP caps act as the true throughput regulator and lets
// backlogs drain at the published per-ISP cadence. See operator note
// 2026-05-14 ("INCREASE THROUGHPUT. Especially for GMAIL.") for context.
func (po *PartnerDripOrchestrator) computeWaveSize(v verticalState) int {
	if v.readyCount <= 0 {
		return 0
	}
	size := po.cfg.MaxWaveSize
	if size > v.readyCount {
		size = v.readyCount
	}
	if size < po.cfg.MinWaveSize {
		size = po.cfg.MinWaveSize
	}
	return size
}

// pickNextBrand walks the round-robin starting at v.brandIndex and skips
// any brand the operator has paused (via PausedBrandPredicate). Returns
// the chosen brand + the next index to write to partner_drip_state.
func (po *PartnerDripOrchestrator) pickNextBrand(ctx context.Context, v verticalState) (string, int, error) {
	for offset := 0; offset < len(dripBrands); offset++ {
		idx := (v.brandIndex + offset) % len(dripBrands)
		brand := dripBrands[idx]
		if po.cfg.PausedBrandPredicate != nil && po.cfg.PausedBrandPredicate(ctx, brand) {
			continue
		}
		next := (idx + 1) % len(dripBrands)
		return brand, next, nil
	}
	return "", v.brandIndex, fmt.Errorf("all brands paused — no brand available")
}

type creativeRec struct {
	filename string
	subject  string
	preheader string
	fromName  string
	htmlBody  string
}

// resolveCreativeForVertical returns the creative the orchestrator will use
// for the next wave. It dispatches between two backing stores:
//
//   1. Direct-offer datasets (verticalState.offerID set) — pull a creative
//      from mailing_offer_creatives + a subject from mailing_offer_subject_lines
//      + a from-name from mailing_offer_from_names. Subject + from-name rotate
//      deterministically by wave time so the partner sees the full pool. The
//      HTML lives in mailing_offer_creatives.html_content (already CAN-SPAM
//      footer-injected at upload time by injectUnsubDisclaimer).
//
//   2. Drip-pool datasets (offerID empty) — legacy path, looks up
//      partner_drip_creatives keyed by (vertical, brand) and reads HTML from
//      docs/emails/<filename>.
//
// Either path returns a populated creativeRec with htmlBody pre-loaded so
// the caller doesn't need to know which path produced it.
func (po *PartnerDripOrchestrator) resolveCreativeForVertical(ctx context.Context, v verticalState, brand string) (creativeRec, error) {
	if v.offerID != "" {
		return po.resolveOfferCreative(ctx, v.offerID, brand)
	}
	return po.resolveCreative(ctx, v.vertical, brand)
}

func (po *PartnerDripOrchestrator) resolveCreative(ctx context.Context, vertical, brand string) (creativeRec, error) {
	var c creativeRec
	err := po.db.QueryRowContext(ctx, `
		SELECT creative_filename, subject_line, COALESCE(preheader, ''), from_name
		FROM partner_drip_creatives
		WHERE vertical = $1 AND brand = $2 AND active = true
	`, vertical, brand).Scan(&c.filename, &c.subject, &c.preheader, &c.fromName)
	if err != nil {
		return c, fmt.Errorf("creative lookup (%s/%s): %w", vertical, brand, err)
	}
	body, err := os.ReadFile(filepath.Join(po.cfg.CreativesDir, c.filename))
	if err != nil {
		return c, fmt.Errorf("read creative %s: %w", c.filename, err)
	}
	c.htmlBody = string(body)
	return c, nil
}

// resolveOfferCreative pulls a fresh creative+subject+from-name from the
// offer-center tables for a direct-offer-bound dataset. Selection rules:
//
//   - Creative: pick the latest non-archived row. Most direct-offer feeds
//     ship one approved creative; if there are multiple, prefer status=
//     'approved' over 'generated'. We do NOT rotate creatives per wave —
//     the operator decides which creative is canonical.
//
//   - Subject + from-name: rotate by wave count. We hash (offerID, brand,
//     5-min wave bucket) into a stable index so successive waves get
//     different items but a retry of the same wave hits the same row.
//
// All three pools must be non-empty. If any are empty we fail loud — the
// orchestrator's caller releases the claim and the next tick retries.
func (po *PartnerDripOrchestrator) resolveOfferCreative(ctx context.Context, offerID, brand string) (creativeRec, error) {
	var c creativeRec
	c.filename = "offer:" + offerID

	// Creative HTML — prefer 'approved' over 'generated' over anything else.
	if err := po.db.QueryRowContext(ctx, `
		SELECT html_content
		FROM mailing_offer_creatives
		WHERE offer_id = $1
		  AND COALESCE(status, '') NOT IN ('archived','rejected')
		  AND COALESCE(html_content, '') <> ''
		ORDER BY (CASE COALESCE(status, '')
		            WHEN 'approved' THEN 0
		            WHEN 'generated' THEN 1
		            ELSE 2
		          END), updated_at DESC
		LIMIT 1
	`, offerID).Scan(&c.htmlBody); err != nil {
		return c, fmt.Errorf("offer_creative lookup (offer=%s): %w", offerID, err)
	}

	// Hash bucket for rotation. Granularity 5 minutes — successive waves get
	// different rows; an immediate retry of the same wave is stable.
	bucket := time.Now().UTC().Truncate(5 * time.Minute).Unix()
	rotKey := fmt.Sprintf("%s|%s|%d", offerID, brand, bucket)
	rotSHA := sha256.Sum256([]byte(rotKey))
	rotIdx := int(rotSHA[0])<<8 | int(rotSHA[1])

	// Subject pool.
	subjects, err := po.fetchOfferSubjects(ctx, offerID)
	if err != nil {
		return c, fmt.Errorf("offer_subjects lookup: %w", err)
	}
	if len(subjects) == 0 {
		return c, fmt.Errorf("offer %s has no active subjects — operator must seed at least one before deploying", offerID)
	}
	c.subject = subjects[rotIdx%len(subjects)]

	// From-name pool — prefer the row matching this brand's blog persona when the
	// pool uses a "Partner - {friendly}" prefix (e.g. CarShield direct offers).
	fromNames, err := po.fetchOfferFromNames(ctx, offerID)
	if err != nil {
		return c, fmt.Errorf("offer_from_names lookup: %w", err)
	}
	if len(fromNames) == 0 {
		return c, fmt.Errorf("offer %s has no active from-names — operator must seed at least one before deploying", offerID)
	}
	if matched, ok := po.matchBrandOfferFromName(ctx, brand, fromNames); ok {
		c.fromName = matched
	} else {
		c.fromName = fromNames[rotIdx%len(fromNames)]
	}

	// Preheader is optional on offer-center creatives; pull from creative
	// row if available, fall back to empty (planner accepts empty).
	_ = po.db.QueryRowContext(ctx, `
		SELECT COALESCE(NULLIF(preview_text, ''), '')
		FROM mailing_offer_creatives
		WHERE offer_id = $1
		  AND COALESCE(status, '') NOT IN ('archived','rejected')
		ORDER BY updated_at DESC
		LIMIT 1
	`, offerID).Scan(&c.preheader)

	return c, nil
}

func (po *PartnerDripOrchestrator) fetchOfferSubjects(ctx context.Context, offerID string) ([]string, error) {
	rows, err := po.db.QueryContext(ctx, `
		SELECT subject_line FROM mailing_offer_subject_lines
		WHERE offer_id = $1
		  AND COALESCE(status, '') NOT IN ('archived','rejected')
		  AND COALESCE(subject_line, '') <> ''
		ORDER BY id
	`, offerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, 16)
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err == nil {
			out = append(out, s)
		}
	}
	return out, nil
}

// matchBrandOfferFromName picks the from-name row that ends with this brand's
// mailing_brand_metadata.from_name when the pool uses a partner prefix pattern.
func (po *PartnerDripOrchestrator) matchBrandOfferFromName(ctx context.Context, brand string, fromNames []string) (string, bool) {
	domain, ok := brandSendingDomain[brand]
	if !ok || domain == "" {
		return "", false
	}
	var metaFrom string
	err := po.db.QueryRowContext(ctx, `
		SELECT from_name FROM mailing_brand_metadata
		WHERE sending_domain = $1
		LIMIT 1
	`, domain).Scan(&metaFrom)
	if err != nil || metaFrom == "" {
		return "", false
	}
	suffix := " - " + metaFrom
	for _, fn := range fromNames {
		if strings.HasSuffix(fn, suffix) {
			return fn, true
		}
	}
	return "", false
}

func (po *PartnerDripOrchestrator) fetchOfferFromNames(ctx context.Context, offerID string) ([]string, error) {
	rows, err := po.db.QueryContext(ctx, `
		SELECT from_name FROM mailing_offer_from_names
		WHERE offer_id = $1
		  AND COALESCE(status, '') NOT IN ('archived','rejected')
		  AND COALESCE(from_name, '') <> ''
		ORDER BY id
	`, offerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, 16)
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err == nil {
			out = append(out, s)
		}
	}
	return out, nil
}

type claimedRecord struct {
	id        string
	email     string
	emailMD5  string
	ispFamily string
	datasetID string
	partnerID string
	batchID   string
	extra     []byte
}

func (po *PartnerDripOrchestrator) claimRecords(ctx context.Context, vertical string, waveSize int) ([]claimedRecord, error) {
	rows, err := po.db.QueryContext(ctx, `
		WITH picked AS (
			SELECT id
			FROM partner_clean_queue
			WHERE status = 'ready' AND vertical = $1
			ORDER BY ingested_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
		UPDATE partner_clean_queue q
		SET status = 'claimed', claimed_at = NOW()
		FROM picked
		WHERE q.id = picked.id
		RETURNING q.id, q.email, q.email_md5, q.isp_family, q.dataset_id, q.partner_id, q.batch_id, q.extra_metadata
	`, vertical, waveSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]claimedRecord, 0, waveSize)
	for rows.Next() {
		var r claimedRecord
		if err := rows.Scan(&r.id, &r.email, &r.emailMD5, &r.ispFamily, &r.datasetID, &r.partnerID, &r.batchID, &r.extra); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// claimRecordsByISPCaps claims oldest-first records from partner_clean_queue
// while honoring per-ISP-per-wave caps natively at the SQL level. This is
// the ISP-aware replacement for claimRecords + applyThroughputSafety's
// per-ISP cap branch.
//
// Why a separate function: the old claimRecords pulls oldest-first across
// ALL ISPs, regardless of cap. When yahoo dominates the queue (e.g. 49%
// of records but cap=20), the wave's claim slots get eaten by yahoo
// records that then get deferred and released back to 'ready'. Net effect:
// gmail/microsoft/apple records stuck behind yahoo trickle through far
// below their per-wave caps. Observed on David Cal's Personal Loans feed
// — gmail shipped 235/day against a 200/wave cap that should have shipped
// up to 4,800/day per brand.
//
// New strategy: rank queue rows BY ISP, take the oldest N per ISP up to
// each ISP's cap, then drain by global ingested_at. This guarantees every
// ISP gets its full per-wave budget before any other ISP overshoots.
//
// hardCap is a safety upper bound on total claim size (post-cap). Set to
// MaxWaveSize so a single wave can never exceed that operational ceiling.
func (po *PartnerDripOrchestrator) claimRecordsByISPCaps(ctx context.Context, vertical string, perISPCaps map[string]int, hardCap int) ([]claimedRecord, error) {
	if len(perISPCaps) == 0 {
		return nil, fmt.Errorf("perISPCaps is empty")
	}
	if hardCap <= 0 {
		hardCap = po.cfg.MaxWaveSize
	}

	// Build a VALUES list (isp, cap) for the caps CTE. Args:
	//   $1 = vertical, $2 = hardCap, then $3.. = isp, cap, isp, cap, ...
	args := []interface{}{vertical, hardCap}
	valueClauses := make([]string, 0, len(perISPCaps))
	idx := 3
	for ispName, capValue := range perISPCaps {
		if capValue <= 0 {
			continue
		}
		valueClauses = append(valueClauses, fmt.Sprintf("($%d::text, $%d::int)", idx, idx+1))
		args = append(args, ispName, capValue)
		idx += 2
	}
	if len(valueClauses) == 0 {
		return nil, fmt.Errorf("perISPCaps has no positive entries")
	}

	query := fmt.Sprintf(`
		WITH ranked AS (
			SELECT id, isp_family, ingested_at,
			       ROW_NUMBER() OVER (
			           PARTITION BY COALESCE(NULLIF(isp_family, ''), 'other')
			           ORDER BY ingested_at ASC
			       ) AS rn
			FROM partner_clean_queue
			WHERE status = 'ready' AND vertical = $1
		),
		caps(isp, cap) AS (
			VALUES %s
		),
		eligible AS (
			SELECT r.id
			FROM ranked r
			JOIN caps c ON c.isp = COALESCE(NULLIF(r.isp_family, ''), 'other')
			WHERE r.rn <= c.cap
			ORDER BY r.ingested_at ASC
			LIMIT $2
		),
		picked AS (
			SELECT id FROM partner_clean_queue
			WHERE id IN (SELECT id FROM eligible)
			  AND status = 'ready'
			FOR UPDATE SKIP LOCKED
		)
		UPDATE partner_clean_queue q
		SET status = 'claimed', claimed_at = NOW()
		FROM picked
		WHERE q.id = picked.id
		RETURNING q.id, q.email, q.email_md5, q.isp_family, q.dataset_id, q.partner_id, q.batch_id, q.extra_metadata
	`, strings.Join(valueClauses, ", "))

	rows, err := po.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]claimedRecord, 0, hardCap)
	for rows.Next() {
		var r claimedRecord
		if err := rows.Scan(&r.id, &r.email, &r.emailMD5, &r.ispFamily, &r.datasetID, &r.partnerID, &r.batchID, &r.extra); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// releaseStaleClaims returns zombie 'claimed' rows to 'ready'. These appear
// when the process dies after claimRecordsByISPCaps but before promote/deploy
// completes (no subscriber_id, no mailed_campaign_id). Safe to re-queue.
func (po *PartnerDripOrchestrator) releaseStaleClaims(ctx context.Context) (int64, error) {
	cutoff := time.Now().UTC().Add(-po.cfg.ClaimedJanitorMaxAge)
	res, err := po.db.ExecContext(ctx, `
		UPDATE partner_clean_queue
		SET status = 'ready',
		    claimed_at = NULL
		WHERE status = 'claimed'
		  AND claimed_at IS NOT NULL
		  AND claimed_at < $1
		  AND subscriber_id IS NULL
		  AND mailed_campaign_id IS NULL
	`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// reconcileShippedClaims repairs rows left in status='claimed' after a wave
// deployed successfully but markMailed failed (logged, non-fatal). Without
// this, follow-up touches stay claimed and block claimFollowupRecordsByISPCaps
// (which only picks status='mailed'). Idempotent: only touches rows whose
// last_touch_campaign is already sending or terminal.
func (po *PartnerDripOrchestrator) reconcileShippedClaims(ctx context.Context) (int64, error) {
	res, err := po.db.ExecContext(ctx, `
		UPDATE partner_clean_queue q
		SET status = 'mailed'
		WHERE q.status = 'claimed'
		  AND q.last_touch_campaign_id IS NOT NULL
		  AND EXISTS (
		    SELECT 1 FROM mailing_campaigns c
		    WHERE c.id = q.last_touch_campaign_id
		      AND c.status IN ('sending', 'sent', 'completed', 'completed_with_errors')
		  )
	`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// releaseClaim flips claimed records back to 'ready' so the next tick can
// retry them after a deploy / promote / segment-creation failure.
func (po *PartnerDripOrchestrator) releaseClaim(ctx context.Context, recs []claimedRecord) error {
	if len(recs) == 0 {
		return nil
	}
	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = r.id
	}
	_, err := po.db.ExecContext(ctx, `
		UPDATE partner_clean_queue
		SET status = 'ready', claimed_at = NULL
		WHERE id = ANY($1::uuid[])
	`, "{"+strings.Join(ids, ",")+"}")
	return err
}

// promoteToSubscribers inserts each claimed email into mailing_subscribers
// (under the Data Partners Master list) with full provenance columns. Returns
// the subscriber IDs in the same order as recs (empty string for inserts that
// failed).
func (po *PartnerDripOrchestrator) promoteToSubscribers(ctx context.Context, v verticalState, recs []claimedRecord) ([]string, error) {
	out := make([]string, len(recs))
	tx, err := po.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO mailing_subscribers
		    (id, organization_id, list_id, email, email_hash,
		     status, source, engagement_score,
		     source_system, source_detail, source_metadata, imported_at)
		VALUES ($1, $2, $3, $4, $5,
		        'confirmed', $6, 50.0,
		        $7, $8, $9::jsonb, NOW())
		ON CONFLICT (list_id, email) DO UPDATE SET
		    source = EXCLUDED.source,
		    source_system = EXCLUDED.source_system,
		    source_detail = EXCLUDED.source_detail,
		    source_metadata = EXCLUDED.source_metadata,
		    updated_at = NOW()
		RETURNING id
	`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for i, r := range recs {
		sourceSystem := "data_partner_" + safeIdent(v.partnerSlug)
		sourceDetail := v.datasetSlug + ":" + r.batchID
		metadata := map[string]interface{}{
			"partner_slug": v.partnerSlug,
			"partner_name": v.partnerName,
			"dataset_id":   v.datasetID,
			"dataset_slug": v.datasetSlug,
			"vertical":     v.vertical,
			"batch_id":     r.batchID,
			"clean_q_id":   r.id,
		}
		metaJSON, _ := json.Marshal(metadata)
		var subID string
		err := stmt.QueryRowContext(ctx,
			uuid.New().String(),
			po.cfg.OrganizationID,
			dataPartnerMasterListID,
			r.email,
			r.emailMD5,
			"data_partner",
			sourceSystem,
			sourceDetail,
			string(metaJSON),
		).Scan(&subID)
		if err != nil {
			log.Printf("[PartnerDripOrchestrator] insert subscriber %s: %v", r.email, err)
			continue
		}
		out[i] = subID
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	// Write subscriber_id back to partner_clean_queue so the engagement
	// marker can flip engaged_at when a tracking event arrives. Best-
	// effort — failure here doesn't unship the wave, but the recipient
	// will silently keep getting follow-ups even if they engage. We log
	// loud on any failure so an operator can audit.
	po.linkSubscriberIDsToQueue(ctx, recs, out)
	return out, nil
}

// linkSubscriberIDsToQueue stamps subscriber_id onto each partner_clean_queue
// row. The engagement marker uses this column to flip engaged_at when a
// new tracking event arrives — without it, every follow-up touch fires
// regardless of whether the recipient already engaged.
func (po *PartnerDripOrchestrator) linkSubscriberIDsToQueue(ctx context.Context, recs []claimedRecord, subIDs []string) {
	if len(recs) == 0 || len(subIDs) == 0 {
		return
	}
	pairs := make([]string, 0, len(recs))
	args := make([]interface{}, 0, len(recs)*2)
	posIdx := 1
	for i, r := range recs {
		if i >= len(subIDs) || subIDs[i] == "" {
			continue
		}
		pairs = append(pairs, fmt.Sprintf("($%d::uuid, $%d::uuid)", posIdx, posIdx+1))
		args = append(args, r.id, subIDs[i])
		posIdx += 2
	}
	if len(pairs) == 0 {
		return
	}
	q := fmt.Sprintf(`
		UPDATE partner_clean_queue q
		SET subscriber_id = pairs.subscriber_id
		FROM (VALUES %s) AS pairs(queue_id, subscriber_id)
		WHERE q.id = pairs.queue_id
		  AND q.subscriber_id IS DISTINCT FROM pairs.subscriber_id
	`, strings.Join(pairs, ","))
	if _, err := po.db.ExecContext(ctx, q, args...); err != nil {
		log.Printf("[PartnerDripOrchestrator] link_subscriber_ids: %v", err)
	}
}

// createWaveSegment builds a one-shot static segment named after the wave so
// the audience finalizer pulls exactly the records we claimed (no more, no
// less). Returns the segment_id.
func (po *PartnerDripOrchestrator) createWaveSegment(ctx context.Context, v verticalState, brand string, recs []claimedRecord, subIDs []string) (string, error) {
	segID := uuid.New().String()
	name := fmt.Sprintf("data-partner-wave-%s-%s-%s", v.vertical, brand, time.Now().UTC().Format("20060102T150405"))

	if _, err := po.db.ExecContext(ctx, `
		INSERT INTO mailing_segments (id, organization_id, list_id, name, description, segment_type, conditions, subscriber_count, last_calculated_at, status)
		VALUES ($1, $2, $3, $4, $5, 'static', '[]'::jsonb, $6, NOW(), 'active')
	`, segID, po.cfg.OrganizationID, dataPartnerMasterListID, name,
		fmt.Sprintf("auto-generated by partner_drip_orchestrator for %s/%s wave", v.vertical, brand),
		len(recs)); err != nil {
		return "", fmt.Errorf("insert segment: %w", err)
	}

	const cols = 3
	args := make([]interface{}, 0, len(recs)*cols)
	placeholders := make([]string, 0, len(recs))
	rowIndex := 0
	for i, r := range recs {
		if i >= len(subIDs) || subIDs[i] == "" {
			continue
		}
		offset := rowIndex * cols
		placeholders = append(placeholders, fmt.Sprintf("($%d::uuid, $%d::uuid, $%d)", offset+1, offset+2, offset+3))
		args = append(args, segID, subIDs[i], r.email)
		rowIndex++
	}
	if rowIndex == 0 {
		return segID, nil
	}
	q := fmt.Sprintf(`
		INSERT INTO mailing_segment_members (segment_id, subscriber_id, email)
		VALUES %s
		ON CONFLICT DO NOTHING
	`, strings.Join(placeholders, ","))
	if _, err := po.db.ExecContext(ctx, q, args...); err != nil {
		return "", fmt.Errorf("insert segment members: %w", err)
	}
	return segID, nil
}

// applyThroughputSafety filters claimed records based on:
//   1. Active ISP backoff in mailing_isp_throttle_state (rate < threshold).
//   2. Per-ISP cap per wave (cfg.PerISPCapPerWave).
// Returns (keep, deferred, reasons-by-isp).
func (po *PartnerDripOrchestrator) applyThroughputSafety(ctx context.Context, recs []claimedRecord) ([]claimedRecord, []claimedRecord, map[string]string, error) {
	throttled, err := po.fetchThrottledISPs(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	keep := make([]claimedRecord, 0, len(recs))
	deferred := make([]claimedRecord, 0)
	counts := make(map[string]int)
	reasons := make(map[string]string)
	for _, r := range recs {
		isp := r.ispFamily
		if isp == "" {
			isp = "other"
		}
		if rate, ok := throttled[isp]; ok {
			if existing, exists := reasons[isp]; !exists || existing == "" {
				reasons[isp] = fmt.Sprintf("throttled (rate=%.1f)", rate)
			}
			deferred = append(deferred, r)
			continue
		}
		cap, hasCap := po.cfg.PerISPCapPerWave[isp]
		if !hasCap {
			cap = po.cfg.PerISPCapPerWave["other"]
		}
		if cap > 0 && counts[isp] >= cap {
			if existing, exists := reasons[isp]; !exists || existing == "" {
				reasons[isp] = fmt.Sprintf("per-wave cap reached (%d)", cap)
			}
			deferred = append(deferred, r)
			continue
		}
		counts[isp]++
		keep = append(keep, r)
	}
	return keep, deferred, reasons, nil
}

// fetchThrottledISPs returns isp -> msgs_per_hour for any ISPs whose rate is
// below ThrottledISPRateThreshold. These are skipped from the upcoming wave.
func (po *PartnerDripOrchestrator) fetchThrottledISPs(ctx context.Context) (map[string]float64, error) {
	rows, err := po.db.QueryContext(ctx, `
		SELECT isp, msgs_per_hour
		FROM mailing_isp_throttle_state
		WHERE msgs_per_hour < $1
	`, po.cfg.ThrottledISPRateThreshold)
	if err != nil {
		// Table may not be populated; treat as no throttling.
		if strings.Contains(err.Error(), "does not exist") {
			return map[string]float64{}, nil
		}
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]float64)
	for rows.Next() {
		var isp string
		var rate float64
		if err := rows.Scan(&isp, &rate); err == nil {
			out[strings.ToLower(strings.TrimSpace(isp))] = rate
		}
	}
	return out, nil
}

func tallyISPs(recs []claimedRecord) map[string]int {
	out := make(map[string]int)
	for _, r := range recs {
		isp := r.ispFamily
		if isp == "" {
			isp = "other"
		}
		out[isp]++
	}
	return out
}

// buildCampaignInput mirrors deploy_may12_mature.py's build_payload but
// expressed as the typed engine.PMTACampaignInput. Window is anchored to
// scheduled_at + cfg.WindowHours so the wave_sanity_check 8h floor is
// satisfied and time_spans[*].source = "duration-calc".
func (po *PartnerDripOrchestrator) buildCampaignInput(v verticalState, brand string, creative creativeRec, segmentID string, ispCounts map[string]int) (engine.PMTACampaignInput, error) {
	domain, ok := brandSendingDomain[brand]
	if !ok {
		return engine.PMTACampaignInput{}, fmt.Errorf("unknown brand %q", brand)
	}
	startAt := time.Now().UTC().Add(2 * time.Minute)
	endAt := startAt.Add(time.Duration(po.cfg.WindowHours) * time.Hour)
	span := engine.PMTATimeSpanInput{
		Type:     "absolute",
		StartAt:  &startAt,
		EndAt:    &endAt,
		Timezone: "America/Denver",
		Source:   "duration-calc", // mandatory per sending-throttle.mdc
	}

	plans := make([]engine.PMTAISPScheduleInput, 0, len(ispCounts))
	quotas := make([]engine.ISPQuota, 0, len(ispCounts))
	targetISPs := make([]engine.ISP, 0, len(ispCounts))
	for ispName, count := range ispCounts {
		ispISP := engine.ISP(ispName)
		quotas = append(quotas, engine.ISPQuota{ISP: ispName, Volume: count})
		targetISPs = append(targetISPs, ispISP)
		plans = append(plans, engine.PMTAISPScheduleInput{
			ISP:               ispName,
			Quota:             count,
			RandomizeAudience: false,
			ThrottleStrategy:  "gentle",
			Timezone:          "America/Denver",
			Cadence: engine.PMTACadenceInput{
				Mode:         "interval",
				EveryMinutes: 15,
				BatchSize:    0,
			},
			TimeSpans: []engine.PMTATimeSpanInput{span},
		})
	}

	useMaster := false
	contentLocked := true
	scheduledAt := startAt
	htmlSHA := sha256.Sum256([]byte(creative.htmlBody))
	name := fmt.Sprintf("[partner-drip] %s %s %s %s", v.vertical, brand, time.Now().UTC().Format("20060102T1504"), hex.EncodeToString(htmlSHA[:4]))

	return engine.PMTACampaignInput{
		Name:          name,
		// OfferID is set ONLY for direct-offer datasets. The deploy
		// pipeline uses it to apply the offer's suppression Bloom and to
		// inherit content_locked from the offer when explicit nil; we
		// pass content_locked=true explicitly for safety regardless.
		OfferID:       v.offerID,
		TargetISPs:    targetISPs,
		SendingDomain: domain,
		Variants: []engine.ContentVariant{{
			VariantName:  "A",
			FromName:     creative.fromName,
			Subject:      creative.subject,
			PreviewText:  creative.preheader,
			HTMLContent:  creative.htmlBody,
			PlainContent: "",
			SplitPercent: 100,
		}},
		ISPPlans:           plans,
		InclusionSegments:  []string{segmentID},
		InclusionLists:     []string{},
		SendPriority:       []engine.PriorityItem{{ID: segmentID, Type: "segment"}},
		ExclusionSegments:  []string{},
		ExclusionLists:     []string{},
		Timezone:           "America/Denver",
		ThrottleStrategy:   "gentle",
		ISPQuotas:          quotas,
		RandomizeAudience:  false,
		SendMode:           "scheduled",
		ScheduledAt:        &scheduledAt,
		MinRemailHours:     0,
		UseMasterSelection: &useMaster,
		ContentLocked:      &contentLocked,
	}, nil
}

// stampPartnerAttributionOnCampaign writes partner_drip_tag and
// partner_dataset_id onto the freshly-deployed campaign row. These columns
// are the join key the analytics dashboard uses to slice campaigns by feed.
// HandleDeployCampaign doesn't accept these fields directly, so we patch
// post-deploy. Errors are logged but not fatal — the wave still mails;
// analytics will just be missing this attribution row.
func (po *PartnerDripOrchestrator) stampPartnerAttributionOnCampaign(ctx context.Context, campaignID, datasetID, partnerSlug, vertical string) error {
	if campaignID == "" {
		return nil
	}
	tag := fmt.Sprintf("data_partner:%s/%s", safeIdent(partnerSlug), vertical)
	var datasetArg interface{}
	if datasetID != "" {
		datasetArg = datasetID
	}
	_, err := po.db.ExecContext(ctx, `
		UPDATE mailing_campaigns
		SET partner_drip_tag = $2,
		    partner_dataset_id = $3,
		    updated_at = NOW()
		WHERE id = $1
	`, campaignID, tag, datasetArg)
	return err
}

// markMailed stamps the queue rows with the wave's outcome and advances
// the touch state machine:
//
//   - touch_count -> touch_count + 1
//   - last_touch_brand -> brand
//   - last_touch_campaign_id -> campaignID
//   - next_touch_at -> NOW() + 24h while touch_count < MaxTouchCount, else NULL
//   - terminal_reason -> 'completed' once touch_count reaches MaxTouchCount
//
// status remains 'mailed' on every successful wave so existing analytics
// and dashboard SUM(status='mailed') queries continue to give a useful
// "at least one wave shipped" count. The follow-up orchestrator queries
// touch_count + next_touch_at + engaged_at to find the next-due record.
//
// On the FIRST touch (touch_count was 0) we also stamp mailed_at +
// mailed_campaign_id + mailed_brand so the legacy fields stay populated
// for backwards-compatible dashboards.
func (po *PartnerDripOrchestrator) markMailed(ctx context.Context, recs []claimedRecord, campaignID, brand string) error {
	if len(recs) == 0 {
		return nil
	}
	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = r.id
	}
	gap := time.Duration(followupTouchGapHours) * time.Hour
	nextTouchAt := time.Now().UTC().Add(gap)
	_, err := po.db.ExecContext(ctx, `
		UPDATE partner_clean_queue
		SET status = 'mailed',
		    mailed_campaign_id = COALESCE(mailed_campaign_id, $2::uuid),
		    mailed_brand = COALESCE(mailed_brand, $3),
		    mailed_at = COALESCE(mailed_at, NOW()),
		    touch_count = LEAST(COALESCE(touch_count, 0) + 1, $4),
		    last_touch_brand = $3,
		    last_touch_campaign_id = $2::uuid,
		    next_touch_at = CASE
		        WHEN COALESCE(touch_count, 0) + 1 < $4 THEN $5::timestamptz
		        ELSE NULL
		    END,
		    terminal_reason = CASE
		        WHEN COALESCE(touch_count, 0) + 1 >= $4 THEN 'completed'
		        ELSE terminal_reason
		    END
		WHERE id = ANY($1::uuid[])
	`, "{"+strings.Join(ids, ",")+"}", campaignID, brand, MaxTouchCount, nextTouchAt)
	return err
}

// tickFollowups runs the follow-up touch loop across all verticals
// represented in the queue. For each vertical it picks the next 'followup'
// brand off the round-robin and claims up to per-ISP-capped records that
// satisfy: status='mailed', touch_count IN (1,2,3), next_touch_at <= NOW(),
// engaged_at IS NULL, terminal_reason IS NULL.
//
// The follow-up loop uses a SHARED 'followup' brand round-robin
// (partner_drip_state row vertical='followup') so brand rotation is
// independent of the welcome pipeline. This keeps welcome and follow-up
// traffic spread evenly across all 16 brands.
func (po *PartnerDripOrchestrator) tickFollowups(ctx context.Context) {
	verticals, err := po.followupVerticalsWithDueRecords(ctx)
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] followup_verticals: %v", err)
		return
	}
	if len(verticals) == 0 {
		return
	}
	state, err := po.followupBrandIndex(ctx)
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] followup_brand_index: %v", err)
		return
	}

	brandsThisTick := po.cfg.FollowupBrandsPerTick
	if brandsThisTick > len(dripBrands) {
		brandsThisTick = len(dripBrands)
	}

	for _, v := range verticals {
		// Each vertical is processed independently. The follow-up brand
		// rotation is shared across verticals — every (vertical, brand)
		// combination gets a wave at most once per tick.
		for i := 0; i < brandsThisTick; i++ {
			if ctx.Err() != nil {
				return
			}
			brand, err := po.pickNextFollowupBrand(ctx, state)
			if err != nil {
				log.Printf("[PartnerDripOrchestrator] followup pick_brand: %v", err)
				return
			}
			if err := po.processFollowup(ctx, v, brand); err != nil {
				log.Printf("[PartnerDripOrchestrator] followup vertical=%s brand=%s: %v", v.vertical, brand, err)
			}
			// Advance the round-robin index AFTER every brand attempt,
			// regardless of success. This guarantees we don't get stuck
			// on a single brand if its claim fails.
			state.brandIndex = (state.brandIndex + 1) % len(dripBrands)
			if err := po.persistFollowupBrandIndex(ctx, state.brandIndex); err != nil {
				log.Printf("[PartnerDripOrchestrator] followup persist_brand_index: %v", err)
			}
		}
	}
}

// followupState is a tiny in-memory mirror of the 'followup' partner_drip_state
// row used to track the brand round-robin pointer between intra-tick calls.
type followupState struct {
	brandIndex int
}

func (po *PartnerDripOrchestrator) followupBrandIndex(ctx context.Context) (*followupState, error) {
	var idx int
	err := po.db.QueryRowContext(ctx,
		`SELECT next_brand_index FROM partner_drip_state WHERE vertical = 'followup'`,
	).Scan(&idx)
	if err == sql.ErrNoRows {
		// Seeded by startup migration but defensive fallback.
		_, _ = po.db.ExecContext(ctx,
			`INSERT INTO partner_drip_state (vertical, next_brand_index) VALUES ('followup', 0) ON CONFLICT (vertical) DO NOTHING`)
		return &followupState{brandIndex: 0}, nil
	}
	if err != nil {
		return nil, err
	}
	return &followupState{brandIndex: idx % len(dripBrands)}, nil
}

func (po *PartnerDripOrchestrator) persistFollowupBrandIndex(ctx context.Context, idx int) error {
	_, err := po.db.ExecContext(ctx, `
		INSERT INTO partner_drip_state (vertical, next_brand_index, updated_at)
		VALUES ('followup', $1, NOW())
		ON CONFLICT (vertical) DO UPDATE SET
		    next_brand_index = EXCLUDED.next_brand_index,
		    updated_at = NOW()
	`, idx)
	return err
}

func (po *PartnerDripOrchestrator) pickNextFollowupBrand(ctx context.Context, state *followupState) (string, error) {
	for offset := 0; offset < len(dripBrands); offset++ {
		idx := (state.brandIndex + offset) % len(dripBrands)
		brand := dripBrands[idx]
		if po.cfg.PausedBrandPredicate != nil && po.cfg.PausedBrandPredicate(ctx, brand) {
			continue
		}
		state.brandIndex = idx
		return brand, nil
	}
	return "", fmt.Errorf("all brands paused — no follow-up brand available")
}

// followupVerticalsWithDueRecords returns the verticals that have at
// least one queue row whose next_touch_at has elapsed and isn't engaged
// or terminal yet. We pull dataset attribution from whichever queue row
// is oldest-due so the wave inherits dataset metadata for the analytics
// stamp.
func (po *PartnerDripOrchestrator) followupVerticalsWithDueRecords(ctx context.Context) ([]verticalState, error) {
	rows, err := po.db.QueryContext(ctx, `
		WITH due AS (
			SELECT q.vertical, q.dataset_id,
			       MIN(q.next_touch_at) AS oldest_due,
			       COUNT(*) FILTER (
			           WHERE q.status = 'mailed'
			             AND q.next_touch_at IS NOT NULL
			             AND q.next_touch_at <= NOW()
			             AND q.engaged_at IS NULL
			             AND q.terminal_reason IS NULL
			             AND q.touch_count BETWEEN 1 AND $1
			       ) AS due_total
			FROM partner_clean_queue q
			WHERE q.status = 'mailed'
			  AND q.engaged_at IS NULL
			  AND q.terminal_reason IS NULL
			  AND q.touch_count BETWEEN 1 AND $1
			GROUP BY q.vertical, q.dataset_id
		)
		SELECT v.vertical,
		       (SELECT SUM(due_total) FROM due WHERE due.vertical = v.vertical) AS due_count,
		       d.id, d.slug, p.slug, p.name, d.flush_window_hours,
		       COALESCE(d.offer_id::text, '')
		FROM (SELECT DISTINCT vertical FROM due WHERE due_total > 0) v
		LEFT JOIN LATERAL (
			SELECT du.dataset_id
			FROM due du
			WHERE du.vertical = v.vertical AND due_total > 0
			ORDER BY oldest_due ASC NULLS LAST
			LIMIT 1
		) AS dom ON true
		LEFT JOIN partner_datasets d ON d.id = dom.dataset_id
		LEFT JOIN data_partners p ON p.id = d.partner_id
		ORDER BY v.vertical
	`, MaxTouchCount-1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []verticalState
	for rows.Next() {
		var v verticalState
		var (
			datasetID, datasetSlug, partnerSlug, partnerName sql.NullString
			flushHours                                       sql.NullInt64
			dueCount                                         sql.NullInt64
			offerID                                          string
		)
		if err := rows.Scan(&v.vertical, &dueCount,
			&datasetID, &datasetSlug, &partnerSlug, &partnerName, &flushHours, &offerID); err != nil {
			continue
		}
		v.offerID = offerID
		if !dueCount.Valid || dueCount.Int64 == 0 {
			continue
		}
		v.readyCount = int(dueCount.Int64)
		if datasetID.Valid {
			v.datasetID = datasetID.String
		}
		if datasetSlug.Valid {
			v.datasetSlug = datasetSlug.String
		}
		if partnerSlug.Valid {
			v.partnerSlug = partnerSlug.String
		}
		if partnerName.Valid {
			v.partnerName = partnerName.String
		}
		if flushHours.Valid {
			v.flushHours = int(flushHours.Int64)
		}
		if v.flushHours <= 0 {
			v.flushHours = 24
		}
		out = append(out, v)
	}
	return out, nil
}

// processFollowup ships one follow-up wave for one (vertical, brand)
// pair. Records are claimed by ISP up to per-ISP caps, promoted to
// mailing_subscribers (idempotent — most are already there from the
// welcome wave), wrapped in a per-wave segment, and shipped via
// DeployFn. On success markMailed advances touch_count and stamps the
// next_touch_at clock.
func (po *PartnerDripOrchestrator) processFollowup(ctx context.Context, v verticalState, brand string) error {
	if MaxTouchCount-1 < 1 {
		return nil
	}
	hardCap := po.cfg.MaxFollowupClaimPerVertical
	if hardCap <= 0 || hardCap > po.cfg.MaxWaveSize {
		hardCap = po.cfg.MaxWaveSize
	}

	claimed, err := po.claimFollowupRecordsByISPCaps(ctx, v.vertical, po.cfg.PerISPCapPerWave, hardCap)
	if err != nil {
		return fmt.Errorf("claim_followup: %w", err)
	}
	if len(claimed) == 0 {
		return nil
	}

	keep, deferred, deferralReasons, err := po.applyThroughputSafety(ctx, claimed)
	if err != nil {
		log.Printf("[PartnerDripOrchestrator] followup throughput_safety failed for vertical=%s: %v — proceeding without deferral", v.vertical, err)
		keep = claimed
	}
	if len(deferred) > 0 {
		if relErr := po.releaseClaim(ctx, deferred); relErr != nil {
			log.Printf("[PartnerDripOrchestrator] followup release deferred: %v", relErr)
		}
		log.Printf("[PartnerDripOrchestrator] followup deferred %d records for vertical=%s reasons=%v",
			len(deferred), v.vertical, deferralReasons)
	}
	if len(keep) == 0 {
		log.Printf("[PartnerDripOrchestrator] followup vertical=%s: all claimed records deferred — skipping wave", v.vertical)
		return nil
	}
	claimed = keep

	// Determine touch_number for the resolveFollowupCreative call. All
	// claimed records share the same target touch (touch_count + 1)
	// because the claim query partitions by touch_count and only takes
	// records whose current touch is the same.
	var touchNum int
	if err := po.db.QueryRowContext(ctx, `
		SELECT MAX(touch_count) + 1 FROM partner_clean_queue WHERE id = ANY($1::uuid[])
	`, "{"+strings.Join(claimedRecordIDs(claimed), ",")+"}").Scan(&touchNum); err != nil {
		_ = po.releaseClaim(ctx, claimed)
		return fmt.Errorf("compute_touch_number: %w", err)
	}
	if touchNum < 2 || touchNum > MaxTouchCount {
		_ = po.releaseClaim(ctx, claimed)
		return fmt.Errorf("invalid touch_number %d for vertical=%s", touchNum, v.vertical)
	}

	creative, err := po.resolveFollowupCreative(ctx, brand, touchNum)
	if err != nil {
		_ = po.releaseClaim(ctx, claimed)
		return fmt.Errorf("resolve_followup_creative: %w", err)
	}

	// Promote subscribers — idempotent because the ON CONFLICT branch in
	// promoteToSubscribers updates source_metadata + returns the
	// existing subscriber id.
	subscriberIDs, err := po.promoteToSubscribers(ctx, v, claimed)
	if err != nil {
		_ = po.releaseClaim(ctx, claimed)
		return fmt.Errorf("promote_followup_subscribers: %w", err)
	}

	segmentID, err := po.createWaveSegment(ctx, v, brand, claimed, subscriberIDs)
	if err != nil {
		_ = po.releaseClaim(ctx, claimed)
		return fmt.Errorf("create_followup_segment: %w", err)
	}

	ispCounts := tallyISPs(claimed)
	input, err := po.buildCampaignInput(v, brand, creative, segmentID, ispCounts)
	if err != nil {
		_ = po.releaseClaim(ctx, claimed)
		return fmt.Errorf("build_followup_input: %w", err)
	}
	// Tag the campaign name so analytics distinguishes welcome vs.
	// follow-up waves at a glance.
	input.Name = fmt.Sprintf("%s [t%d]", input.Name, touchNum)

	campaignID, err := po.cfg.DeployFn(ctx, input)
	if err != nil {
		_ = po.releaseClaim(ctx, claimed)
		return fmt.Errorf("deploy_followup: %w", err)
	}

	if err := po.stampPartnerAttributionOnCampaign(ctx, campaignID, v.datasetID, v.partnerSlug, v.vertical); err != nil {
		log.Printf("[PartnerDripOrchestrator] followup stamp_attribution (campaign=%s dataset=%s): %v", campaignID, v.datasetID, err)
	}

	if err := po.markMailed(ctx, claimed, campaignID, brand); err != nil {
		log.Printf("[PartnerDripOrchestrator] followup mark_mailed (campaign %s already deployed!): %v", campaignID, err)
	}

	log.Printf("[PartnerDripOrchestrator] followup wave fired: vertical=%s brand=%s touch=%d campaign=%s size=%d creative=%s",
		v.vertical, brand, touchNum, campaignID, len(claimed), creative.filename)
	return nil
}

func claimedRecordIDs(recs []claimedRecord) []string {
	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = r.id
	}
	return ids
}

// claimFollowupRecordsByISPCaps claims oldest-due-first records from
// partner_clean_queue for a given vertical, partitioned by ISP and
// capped by perISPCaps. Records must be:
//
//   - status = 'mailed'
//   - touch_count BETWEEN 1 AND MaxTouchCount-1 (welcome shipped, < max)
//   - next_touch_at <= NOW() (gap window elapsed)
//   - engaged_at IS NULL (recipient hasn't engaged with prior touches)
//   - terminal_reason IS NULL (operator hasn't manually retired the row)
//
// Within a single wave we constrain claims to a single touch_count value
// so the resolveFollowupCreative step can pick the right family without
// running per-record. We pick whichever touch_count has the most
// outstanding due rows for this vertical at claim time — this keeps the
// rotation balanced between touch 2/3/4 in a steady-state queue.
//
// Returns the claimed records with status flipped to 'claimed' (so they
// won't be re-picked by a concurrent tick) and the wave_segment + deploy
// pipeline can carry on. On failure the caller releases via releaseClaim.
func (po *PartnerDripOrchestrator) claimFollowupRecordsByISPCaps(ctx context.Context, vertical string, perISPCaps map[string]int, hardCap int) ([]claimedRecord, error) {
	if len(perISPCaps) == 0 {
		return nil, fmt.Errorf("perISPCaps is empty")
	}
	if hardCap <= 0 {
		hardCap = po.cfg.MaxWaveSize
	}

	// First pick the dominant touch_count for this vertical.
	var targetTouchCount int
	if err := po.db.QueryRowContext(ctx, `
		SELECT touch_count
		FROM partner_clean_queue
		WHERE status = 'mailed'
		  AND vertical = $1
		  AND touch_count BETWEEN 1 AND $2
		  AND next_touch_at <= NOW()
		  AND engaged_at IS NULL
		  AND terminal_reason IS NULL
		GROUP BY touch_count
		ORDER BY COUNT(*) DESC, touch_count ASC
		LIMIT 1
	`, vertical, MaxTouchCount-1).Scan(&targetTouchCount); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("pick_dominant_touch: %w", err)
	}

	// Build VALUES list for caps CTE.
	args := []interface{}{vertical, hardCap, targetTouchCount}
	valueClauses := make([]string, 0, len(perISPCaps))
	idx := 4
	for ispName, capValue := range perISPCaps {
		if capValue <= 0 {
			continue
		}
		valueClauses = append(valueClauses, fmt.Sprintf("($%d::text, $%d::int)", idx, idx+1))
		args = append(args, ispName, capValue)
		idx += 2
	}
	if len(valueClauses) == 0 {
		return nil, fmt.Errorf("perISPCaps has no positive entries")
	}

	query := fmt.Sprintf(`
		WITH ranked AS (
			SELECT id, isp_family, next_touch_at,
			       ROW_NUMBER() OVER (
			           PARTITION BY COALESCE(NULLIF(isp_family, ''), 'other')
			           ORDER BY next_touch_at ASC NULLS LAST
			       ) AS rn
			FROM partner_clean_queue
			WHERE status = 'mailed'
			  AND vertical = $1
			  AND touch_count = $3
			  AND next_touch_at <= NOW()
			  AND engaged_at IS NULL
			  AND terminal_reason IS NULL
		),
		caps(isp, cap) AS (
			VALUES %s
		),
		eligible AS (
			SELECT r.id
			FROM ranked r
			JOIN caps c ON c.isp = COALESCE(NULLIF(r.isp_family, ''), 'other')
			WHERE r.rn <= c.cap
			ORDER BY r.next_touch_at ASC NULLS LAST
			LIMIT $2
		),
		picked AS (
			SELECT id FROM partner_clean_queue
			WHERE id IN (SELECT id FROM eligible)
			  AND status = 'mailed'
			  AND touch_count = $3
			  AND next_touch_at <= NOW()
			  AND engaged_at IS NULL
			  AND terminal_reason IS NULL
			FOR UPDATE SKIP LOCKED
		)
		UPDATE partner_clean_queue q
		SET status = 'claimed', claimed_at = NOW()
		FROM picked
		WHERE q.id = picked.id
		RETURNING q.id, q.email, q.email_md5, q.isp_family, q.dataset_id, q.partner_id, q.batch_id, q.extra_metadata
	`, strings.Join(valueClauses, ", "))

	rows, err := po.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]claimedRecord, 0, hardCap)
	for rows.Next() {
		var r claimedRecord
		if err := rows.Scan(&r.id, &r.email, &r.emailMD5, &r.ispFamily, &r.datasetID, &r.partnerID, &r.batchID, &r.extra); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// resolveFollowupCreative looks up the (brand, touch_number) row in
// partner_drip_followup_creatives and reads the HTML body from disk.
// Errors out loud — the caller releases the claim if this fails so the
// records get retried on the next tick.
func (po *PartnerDripOrchestrator) resolveFollowupCreative(ctx context.Context, brand string, touchNumber int) (creativeRec, error) {
	var c creativeRec
	err := po.db.QueryRowContext(ctx, `
		SELECT creative_filename, subject_line, COALESCE(preheader, ''), from_name
		FROM partner_drip_followup_creatives
		WHERE brand = $1 AND touch_number = $2 AND active = true
	`, brand, touchNumber).Scan(&c.filename, &c.subject, &c.preheader, &c.fromName)
	if err != nil {
		return c, fmt.Errorf("followup_creative lookup (%s/t%d): %w", brand, touchNumber, err)
	}
	body, err := os.ReadFile(filepath.Join(po.cfg.CreativesDir, c.filename))
	if err != nil {
		return c, fmt.Errorf("read followup_creative %s: %w", c.filename, err)
	}
	c.htmlBody = string(body)
	return c, nil
}

func (po *PartnerDripOrchestrator) updateDripState(ctx context.Context, vertical string, nextIdx int, brand, campaignID string, waveSize int) error {
	_, err := po.db.ExecContext(ctx, `
		INSERT INTO partner_drip_state (vertical, next_brand_index, last_wave_at, last_wave_campaign_id, last_wave_brand, last_wave_size, updated_at)
		VALUES ($1, $2, NOW(), $3::uuid, $4, $5, NOW())
		ON CONFLICT (vertical) DO UPDATE SET
		    next_brand_index = EXCLUDED.next_brand_index,
		    last_wave_at = EXCLUDED.last_wave_at,
		    last_wave_campaign_id = EXCLUDED.last_wave_campaign_id,
		    last_wave_brand = EXCLUDED.last_wave_brand,
		    last_wave_size = EXCLUDED.last_wave_size,
		    updated_at = NOW()
	`, vertical, nextIdx, campaignID, brand, waveSize)
	return err
}

// safeIdent returns a slug suitable for use inside source_system labels.
// Only [a-z0-9_] characters retained, trims duplicate underscores.
func safeIdent(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "unknown"
	}
	out := make([]byte, 0, len(s))
	last := byte(0)
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9':
			out = append(out, ch)
			last = ch
		default:
			if last != '_' {
				out = append(out, '_')
				last = '_'
			}
		}
	}
	return strings.Trim(string(out), "_")
}

// =========================================================================
// HandleDeployCampaign adapter
// =========================================================================
//
// WrapPMTACampaignDeploy turns the existing http.HandlerFunc-style HandleDeploy
// into a typed in-process call. We use httptest.NewRecorder + http.NewRequest
// so we don't have to refactor the existing handler. The handler reads the
// body as JSON, persists the campaign row, and queues the audience finalizer
// goroutine. The 202 response carries the campaign_id we extract here.

type deployHandlerSig func(http.ResponseWriter, *http.Request)

// WrapPMTACampaignDeploy adapts an HTTP handler into a typed CampaignDeployFn.
// The handler is invoked in-process via httptest, so no real network call.
func WrapPMTACampaignDeploy(h deployHandlerSig) CampaignDeployFn {
	return func(ctx context.Context, input engine.PMTACampaignInput) (string, error) {
		body, err := json.Marshal(input)
		if err != nil {
			return "", fmt.Errorf("marshal input: %w", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign/deploy", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Internal-Caller", "partner_drip_orchestrator")
		req = req.WithContext(ctx)
		rr := httptest.NewRecorder()
		h(rr, req)
		if rr.Code >= 400 {
			respBody, _ := io.ReadAll(rr.Body)
			return "", fmt.Errorf("HandleDeployCampaign returned %d: %s", rr.Code, string(respBody))
		}
		var out struct {
			CampaignID string `json:"campaign_id"`
			ID         string `json:"id"`
			Error      string `json:"error,omitempty"`
		}
		respBody, _ := io.ReadAll(rr.Body)
		if err := json.Unmarshal(respBody, &out); err != nil {
			return "", fmt.Errorf("unmarshal response: %w (raw=%s)", err, string(respBody))
		}
		if out.Error != "" {
			return "", fmt.Errorf("HandleDeployCampaign error: %s", out.Error)
		}
		if out.CampaignID != "" {
			return out.CampaignID, nil
		}
		if out.ID != "" {
			return out.ID, nil
		}
		return "", fmt.Errorf("HandleDeployCampaign returned no campaign_id (status=%d)", rr.Code)
	}
}
