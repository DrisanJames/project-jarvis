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

// Brands in deterministic round-robin order. Matches deploy_may12_mature.py
// brand codes (lowercase versions used in partner_drip_creatives).
var dripBrands = []string{"db", "ht", "mh", "qf"}

var brandSendingDomain = map[string]string{
	"db": "em.discountblog.com",
	"ht": "em.historythinking.com",
	"mh": "em.myownhealth.net",
	"qf": "em.quizfiesta.com",
}

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
		cfg.PerISPCapPerWave = map[string]int{
			"gmail":     150,
			"yahoo":     20,
			"aol":       20,
			"microsoft": 100,
			"apple":     100,
			"comcast":   60,
			"charter":   60,
			"att":       40,
			"sbcglobal": 40,
			"cox":       40,
			"verizon":   40,
			"other":     100,
		}
	}
	if cfg.ThrottledISPRateThreshold <= 0 {
		cfg.ThrottledISPRateThreshold = 50.0
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &PartnerDripOrchestrator{db: db, cfg: cfg, ctx: ctx, cancel: cancel}
}

func (po *PartnerDripOrchestrator) Start() {
	po.startOnce.Do(func() {
		po.wg.Add(1)
		go po.run()
		log.Printf("[PartnerDripOrchestrator] started — tick=%s min_wave=%d max_wave=%d window_hours=%d creatives_dir=%s",
			po.cfg.TickInterval, po.cfg.MinWaveSize, po.cfg.MaxWaveSize, po.cfg.WindowHours, po.cfg.CreativesDir)
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

// tickOnce iterates over every active vertical with ready records and runs
// at most one wave per vertical per tick. Errors per-vertical are logged
// but do not stop the rest of the loop.
func (po *PartnerDripOrchestrator) tickOnce() {
	if po.cfg.DeployFn == nil {
		log.Println("[PartnerDripOrchestrator] no DeployFn wired — skipping tick")
		return
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
		if err := po.processVertical(po.ctx, v); err != nil {
			log.Printf("[PartnerDripOrchestrator] vertical=%s: %v", v.vertical, err)
		}
	}
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
		       d.id, d.slug, p.slug, p.name, d.flush_window_hours
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
		)
		if err := rows.Scan(&v.vertical, &v.brandIndex, &readyTotal, &oldestAt,
			&datasetID, &datasetSlug, &partnerSlug, &partnerName, &flushHours); err != nil {
			continue
		}
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
	creative, err := po.resolveCreative(ctx, v.vertical, brand)
	if err != nil {
		return fmt.Errorf("resolve_creative: %w", err)
	}

	claimed, err := po.claimRecords(ctx, v.vertical, waveSize)
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
func (po *PartnerDripOrchestrator) computeWaveSize(v verticalState) int {
	if v.readyCount <= 0 {
		return 0
	}
	wavesRemaining := 0
	if v.oldestIngest.Valid {
		deadline := v.oldestIngest.Time.Add(time.Duration(v.flushHours) * time.Hour)
		mins := int(time.Until(deadline).Minutes())
		if mins < int(po.cfg.TickInterval.Minutes()) {
			mins = int(po.cfg.TickInterval.Minutes())
		}
		wavesRemaining = mins / int(po.cfg.TickInterval.Minutes())
	}
	if wavesRemaining <= 0 {
		wavesRemaining = 1
	}
	size := v.readyCount / wavesRemaining
	if size < po.cfg.MinWaveSize {
		size = po.cfg.MinWaveSize
	}
	if size > po.cfg.MaxWaveSize {
		size = po.cfg.MaxWaveSize
	}
	if size > v.readyCount {
		size = v.readyCount
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
	return out, nil
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
	partnerTag := fmt.Sprintf("data_partner:%s/%s", safeIdent(v.partnerSlug), v.vertical)
	htmlSHA := sha256.Sum256([]byte(creative.htmlBody))
	name := fmt.Sprintf("[partner-drip] %s %s %s %s", v.vertical, brand, time.Now().UTC().Format("20060102T1504"), hex.EncodeToString(htmlSHA[:4]))
	_ = partnerTag // future: stored in payload metadata; column-write not yet wired through HandleDeployCampaign

	return engine.PMTACampaignInput{
		Name:          name,
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

func (po *PartnerDripOrchestrator) markMailed(ctx context.Context, recs []claimedRecord, campaignID, brand string) error {
	if len(recs) == 0 {
		return nil
	}
	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = r.id
	}
	_, err := po.db.ExecContext(ctx, `
		UPDATE partner_clean_queue
		SET status = 'mailed',
		    mailed_campaign_id = $2::uuid,
		    mailed_brand = $3,
		    mailed_at = NOW()
		WHERE id = ANY($1::uuid[])
	`, "{"+strings.Join(ids, ",")+"}", campaignID, brand)
	return err
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
