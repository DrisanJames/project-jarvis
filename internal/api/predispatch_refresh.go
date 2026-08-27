package api

// Pre-dispatch audience refresh — "rebuild an hour before campaign dispatch".
//
// Operator ruling 2026-08-27: "System should review campaigns coming up in the
// next hour from the broadcast view then refresh the segments. This will allow
// all campaigns from here forward to mail fresh segments."
//
// WHY: a campaign's audience is frozen into mailing_campaign_plan_recipients
// when it finalizes at DEPLOY. Boards promoted days ahead therefore mailed the
// segment membership of the promotion day while the nightly grid build
// refreshed every segment underneath them unused — 36/45 cells on 2026-08-28
// carried 08-24 members (61k engaged people never planned in 3 days, 8.3k of
// them clickers; 60k stale sends the other way).
//
// WHAT ONE TICK DOES (every predispatchTick, single-writer distlock):
//  1. Load BROADCAST campaigns (partner_drip_tag IS NULL, journey_id IS NULL)
//     in status 'scheduled' whose anchor is inside (now+minLead, now+lookahead]
//     and which carry no predispatch_refresh stamp and no queue rows.
//  2. Union their inclusion segments. Every segment whose ledger build is older
//     than predispatchSegmentFresh is re-materialized through MaterializeSegment
//     (PG tracking_events — the freshest signal, 5–50s per grid segment,
//     bounded by the process-wide materialize semaphore). A segment shared by
//     several cells is built once.
//  3. For every cell whose segments are now ALL newer than its plan
//     (min plan_recipients.created_at): rebind. SIBLING-FIRST — deploy the
//     cell's own campaign_input blob under "<name> ~fresh", wait until it is
//     'scheduled' with recipients>0 and waves>0, THEN cancel the original and
//     rename the sibling to the original name (stamping pmta_config.
//     predispatch_refresh + rebuilt_from). A sibling that fails to finalize is
//     cancelled and the original mails as it was — a cell is never lost.
//     Restart-safe: an orphaned "~fresh" row is either adopted (if verified)
//     or cancelled on the next tick.
//  4. Heartbeat 'predispatch_refresh' (worker health) with the tick outcome;
//     cells that reach minLead without fresh segments are logged as MISSED
//     and the heartbeat degrades — silence is never success.
//
// Kill switch: DISABLE_PREDISPATCH_REFRESH=true. Lookahead/lead tunable via
// PREDISPATCH_LOOKAHEAD_MIN / PREDISPATCH_MIN_LEAD_MIN.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/pkg/distlock"
	"github.com/ignite/sparkpost-monitor/internal/worker"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

const (
	predispatchWorkerName    = "predispatch_refresh"
	predispatchLockKey       = "predispatch-refresh"
	predispatchTick          = 2 * time.Minute
	predispatchLookaheadDflt = 75 * time.Minute
	predispatchMinLeadDflt   = 15 * time.Minute
	// A segment whose ledger shows a successful build newer than this is not
	// rebuilt again for the same window (the nightly pass or an earlier cell
	// of the same brand already did it).
	predispatchSegmentFresh = 60 * time.Minute
	predispatchSiblingSfx   = " ~fresh"
	predispatchVerifyEvery  = 10 * time.Second
	predispatchVerifyMax    = 8 * time.Minute
	predispatchStampKey     = "predispatch_refresh"
	// predispatchDoubleSendGuard: an un-adoptable sibling this close to its
	// anchor is cancelled so the original mails alone.
	predispatchDoubleSendGuard = 10 * time.Minute
)

type predispatchCell struct {
	ID          string
	OrgID       string
	Name        string
	ScheduledAt time.Time
	PlanAt      sql.NullTime
	Recipients  int
	Queued      int
	Segments    []string
	ConfigRaw   string
}

type predispatchSegment struct {
	ID         string
	Name       string
	ListID     string
	Conditions string
	Status     string
	BuiltAt    sql.NullTime
	BuildState string
}

// predispatchDeps isolates the side effects so the decision logic is unit
// testable without a database.
type predispatchDeps struct {
	now         func() time.Time
	materialize func(ctx context.Context, seg predispatchSegment) (int, error)
	deploy      func(ctx context.Context, orgID string, input engine.PMTACampaignInput) (string, string, bool, error)
}

func predispatchDisabled() bool {
	return strings.EqualFold(os.Getenv("DISABLE_PREDISPATCH_REFRESH"), "true")
}

func predispatchMinutes(env string, dflt time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 24*60 {
			return time.Duration(n) * time.Minute
		}
	}
	return dflt
}

// StartPreDispatchRefresher wires the loop. Call once at boot next to
// StartAudienceWorker; both ECS instances run it and the distlock elects one.
func (s *PMTACampaignService) StartPreDispatchRefresher(ctx context.Context, rc *redis.Client) {
	if predispatchDisabled() {
		log.Printf("[PreDispatch] disabled by DISABLE_PREDISPATCH_REFRESH")
		return
	}
	s.predispatchRedis = rc
	log.Printf("[PreDispatch] started (tick=%s lookahead=%s min_lead=%s)",
		predispatchTick, predispatchMinutes("PREDISPATCH_LOOKAHEAD_MIN", predispatchLookaheadDflt),
		predispatchMinutes("PREDISPATCH_MIN_LEAD_MIN", predispatchMinLeadDflt))
	go func() {
		t := time.NewTicker(predispatchTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.predispatchTick(ctx)
			}
		}
	}()
}

func (s *PMTACampaignService) predispatchDeps() predispatchDeps {
	d := predispatchDeps{
		now: time.Now,
		materialize: func(ctx context.Context, seg predispatchSegment) (int, error) {
			return materializeSegmentWithLedger(ctx, s.db, seg.ID, seg.ListID, seg.Conditions, "predispatch")
		},
		deploy: s.deployFromInput,
	}
	if s.dayCardsDeployFn != nil {
		d.deploy = s.dayCardsDeployFn
	}
	if s.predispatchNowFn != nil {
		d.now = s.predispatchNowFn
	}
	if s.predispatchMaterializeFn != nil {
		d.materialize = s.predispatchMaterializeFn
	}
	return d
}

func (s *PMTACampaignService) predispatchTick(ctx context.Context) {
	lock := distlock.NewLock(s.predispatchRedis, s.db, predispatchLockKey, 30*time.Minute)
	acquired, err := lock.Acquire(ctx)
	if err != nil {
		log.Printf("[PreDispatch] lock acquire error: %v", err)
		return
	}
	if !acquired {
		return
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("[PreDispatch] lock release error: %v", err)
		}
	}()
	status, msg := s.runPreDispatchPass(ctx, s.predispatchDeps())
	worker.EmitHeartbeat(ctx, s.db, predispatchWorkerName, int(predispatchTick.Seconds()), status, msg)
}

// runPreDispatchPass is one full pass. Returns the heartbeat status + message.
func (s *PMTACampaignService) runPreDispatchPass(ctx context.Context, d predispatchDeps) (string, string) {
	now := d.now()
	lookahead := predispatchMinutes("PREDISPATCH_LOOKAHEAD_MIN", predispatchLookaheadDflt)
	minLead := predispatchMinutes("PREDISPATCH_MIN_LEAD_MIN", predispatchMinLeadDflt)

	s.predispatchRecoverOrphans(ctx, d)

	cells, err := s.predispatchLoadCells(ctx, now.Add(minLead), now.Add(lookahead))
	if err != nil {
		log.Printf("[PreDispatch] load cells: %v", err)
		return "error", "load cells: " + err.Error()
	}
	if len(cells) == 0 {
		return "ok", "no cells in window"
	}

	// ── segments: union, load, rebuild the stale ones once ──────────────
	segIDs := map[string]bool{}
	for _, c := range cells {
		for _, id := range c.Segments {
			segIDs[id] = true
		}
	}
	segs, err := s.predispatchLoadSegments(ctx, keys(segIDs))
	if err != nil {
		log.Printf("[PreDispatch] load segments: %v", err)
		return "error", "load segments: " + err.Error()
	}
	toBuild := predispatchSegmentsToBuild(segs, now)
	built, failed := s.predispatchMaterialize(ctx, d, toBuild)
	if len(toBuild) > 0 {
		log.Printf("[PreDispatch] %d cell(s) in window · %d segment(s) referenced · rebuilt %d · failed %d",
			len(cells), len(segs), built, failed)
		// Re-read the ledger after the builds so readiness uses real timestamps.
		if segs, err = s.predispatchLoadSegments(ctx, keys(segIDs)); err != nil {
			return "error", "reload segments: " + err.Error()
		}
	}

	// ── cells: rebind the ready ones ─────────────────────────────────────
	rebound, missed, skipped := 0, 0, 0
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	var mu sync.Mutex
	for _, c := range cells {
		ready, reason := predispatchCellReady(c, segs, d.now(), minLead)
		if !ready {
			if strings.HasPrefix(reason, "MISSED") {
				missed++
				log.Printf("[PreDispatch] %s %q: %s", c.ID[:8], c.Name, reason)
			} else {
				skipped++
			}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(c predispatchCell) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := s.predispatchRebind(ctx, d, c, segs); err != nil {
				log.Printf("[PreDispatch] REBIND FAILED %q (original untouched): %v", c.Name, err)
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			mu.Lock()
			rebound++
			mu.Unlock()
		}(c)
	}
	wg.Wait()

	msg := fmt.Sprintf("cells=%d rebound=%d waiting=%d missed=%d segs_rebuilt=%d failed=%d",
		len(cells), rebound, skipped, missed, built, failed)
	log.Printf("[PreDispatch] %s", msg)
	if missed > 0 || failed > 0 {
		return "degraded", msg
	}
	return "ok", msg
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// predispatchSegmentsToBuild picks segments whose last successful build is
// older than predispatchSegmentFresh (or that have never been built).
func predispatchSegmentsToBuild(segs map[string]predispatchSegment, now time.Time) []predispatchSegment {
	var out []predispatchSegment
	for _, sg := range segs {
		if sg.Status != "active" {
			continue
		}
		if sg.BuildState == "running" {
			continue // another builder (nightly materializer / grid worker) owns it right now
		}
		if sg.BuiltAt.Valid && now.Sub(sg.BuiltAt.Time) < predispatchSegmentFresh {
			continue
		}
		out = append(out, sg)
	}
	return out
}

// predispatchCellReady decides whether a cell can be rebound now: every
// inclusion segment must have a successful build newer than the cell's plan,
// and the anchor must still be outside minLead. Returns a reason on false;
// reasons starting with "MISSED" mean the cell will mail its old plan.
func predispatchCellReady(c predispatchCell, segs map[string]predispatchSegment, now time.Time, minLead time.Duration) (bool, string) {
	if c.Queued > 0 {
		return false, "MISSED: queue rows already enqueued"
	}
	if !c.ScheduledAt.After(now.Add(minLead)) {
		return false, "MISSED: inside min lead without fresh segments"
	}
	if len(c.Segments) == 0 {
		return false, "no inclusion segments (list-sourced cell)"
	}
	if !c.PlanAt.Valid {
		return false, "no plan recipients yet (still finalizing)"
	}
	for _, id := range c.Segments {
		sg, ok := segs[id]
		if !ok {
			return false, "segment " + id + " not found"
		}
		if sg.Status != "active" {
			return false, "segment " + sg.Name + " not active"
		}
		if !sg.BuiltAt.Valid || !sg.BuiltAt.Time.After(c.PlanAt.Time) {
			return false, "waiting: " + sg.Name + " not newer than plan"
		}
	}
	return true, ""
}

func (s *PMTACampaignService) predispatchLoadCells(ctx context.Context, from, to time.Time) ([]predispatchCell, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id::text, c.organization_id::text, COALESCE(c.name,''), c.scheduled_at,
		       (SELECT min(r.created_at) FROM mailing_campaign_plan_recipients r WHERE r.campaign_id = c.id),
		       COALESCE(c.total_recipients, 0),
		       (SELECT count(*) FROM mailing_campaign_queue q WHERE q.campaign_id = c.id),
		       COALESCE(c.pmta_config->'campaign_input'->'inclusion_segments', '[]'::jsonb)::text,
		       COALESCE(c.pmta_config::text, '')
		FROM mailing_campaigns c
		WHERE c.status = 'scheduled'
		  AND c.partner_drip_tag IS NULL AND c.journey_id IS NULL
		  AND c.scheduled_at > $1 AND c.scheduled_at <= $2
		  AND (c.pmta_config->$3) IS NULL
		  AND c.name NOT LIKE '%' || $4
		ORDER BY c.scheduled_at, c.name
	`, from, to, predispatchStampKey, predispatchSiblingSfx)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []predispatchCell
	for rows.Next() {
		var c predispatchCell
		var segsRaw string
		if err := rows.Scan(&c.ID, &c.OrgID, &c.Name, &c.ScheduledAt, &c.PlanAt, &c.Recipients, &c.Queued, &segsRaw, &c.ConfigRaw); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(segsRaw), &c.Segments)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *PMTACampaignService) predispatchLoadSegments(ctx context.Context, ids []string) (map[string]predispatchSegment, error) {
	out := map[string]predispatchSegment{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id::text, COALESCE(s.name,''), COALESCE(s.list_id::text,''), COALESCE(s.conditions::text,'[]'),
		       COALESCE(s.status,''), l.last_built_at, COALESCE(l.last_build_status,'')
		FROM mailing_segments s
		LEFT JOIN mailing_segment_build_ledger l ON l.segment_id = s.id
		WHERE s.id::text = ANY($1)
	`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sg predispatchSegment
		if err := rows.Scan(&sg.ID, &sg.Name, &sg.ListID, &sg.Conditions, &sg.Status, &sg.BuiltAt, &sg.BuildState); err != nil {
			return nil, err
		}
		// A build that ended in error must not count as fresh.
		if sg.BuildState != "ok" && sg.BuildState != "running" {
			sg.BuiltAt = sql.NullTime{}
		}
		out[sg.ID] = sg
	}
	return out, rows.Err()
}

// predispatchMaterialize rebuilds the given segments with bounded parallelism
// (MaterializeSegment itself also takes the process-wide semaphore).
func (s *PMTACampaignService) predispatchMaterialize(ctx context.Context, d predispatchDeps, segs []predispatchSegment) (built, failed int) {
	if len(segs) == 0 {
		return 0, 0
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, materializeConcurrency)
	var mu sync.Mutex
	for _, sg := range segs {
		wg.Add(1)
		sem <- struct{}{}
		go func(sg predispatchSegment) {
			defer wg.Done()
			defer func() { <-sem }()
			start := time.Now()
			n, err := d.materialize(ctx, sg)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				log.Printf("[PreDispatch] segment %q rebuild FAILED after %s: %v", sg.Name, time.Since(start).Round(time.Second), err)
				return
			}
			built++
			log.Printf("[PreDispatch] segment %q rebuilt: %d members in %s", sg.Name, n, time.Since(start).Round(time.Second))
		}(sg)
	}
	wg.Wait()
	return built, failed
}

// predispatchRebind deploys a fresh sibling of the cell, verifies it, then
// swaps it in under the original name. The original is only cancelled after
// the sibling is proven.
func (s *PMTACampaignService) predispatchRebind(ctx context.Context, d predispatchDeps, c predispatchCell, segs map[string]predispatchSegment) error {
	var cfg pmtaCampaignConfig
	if err := json.Unmarshal([]byte(c.ConfigRaw), &cfg); err != nil {
		return fmt.Errorf("unreadable campaign_input blob: %w", err)
	}
	input := cfg.CampaignInput
	if len(input.ISPPlans) == 0 || len(input.Variants) == 0 {
		return fmt.Errorf("campaign_input blob incomplete (plans=%d variants=%d)", len(input.ISPPlans), len(input.Variants))
	}
	input.CampaignID = ""
	input.Name = c.Name + predispatchSiblingSfx

	sibID, sibStatus, existed, err := d.deploy(ctx, c.OrgID, input)
	if err != nil {
		return fmt.Errorf("sibling deploy: %w", err)
	}
	if existed {
		return fmt.Errorf("sibling name %q already live (%s, %s) — orphan recovery will handle it", input.Name, sibID, sibStatus)
	}

	ok, recips, waves, verr := s.predispatchVerifySibling(ctx, d, sibID)
	if verr != nil || !ok {
		s.predispatchCancel(ctx, sibID)
		if verr != nil {
			return fmt.Errorf("sibling %s verify: %w (sibling cancelled)", sibID, verr)
		}
		return fmt.Errorf("sibling %s did not finalize (recips=%d waves=%d) — sibling cancelled", sibID, recips, waves)
	}
	if err := s.predispatchSwap(ctx, d, c, sibID, recips, segs); err != nil {
		return err
	}
	log.Printf("[PreDispatch] %q rebound: %s → %s recipients %d → %d (%+d)",
		c.Name, c.ID[:8], sibID[:8], c.Recipients, recips, recips-c.Recipients)
	return nil
}

func (s *PMTACampaignService) predispatchVerifySibling(ctx context.Context, d predispatchDeps, sibID string) (bool, int, int, error) {
	deadline := d.now().Add(predispatchVerifyMax)
	for {
		var status string
		var recips, waves int
		err := s.db.QueryRowContext(ctx, `
			SELECT c.status, COALESCE(c.total_recipients,0),
			       (SELECT count(*) FROM mailing_campaign_waves w WHERE w.campaign_id = c.id)
			FROM mailing_campaigns c WHERE c.id = $1
		`, sibID).Scan(&status, &recips, &waves)
		if err != nil {
			return false, 0, 0, err
		}
		switch status {
		case "scheduled":
			return recips > 0 && waves > 0, recips, waves, nil
		case "failed", "cancelled", "deleted":
			return false, recips, waves, nil
		}
		if d.now().After(deadline) {
			return false, recips, waves, fmt.Errorf("timeout waiting for finalization (status=%s)", status)
		}
		select {
		case <-ctx.Done():
			return false, 0, 0, ctx.Err()
		case <-time.After(predispatchVerifyEvery):
		}
	}
}

func (s *PMTACampaignService) predispatchCancel(ctx context.Context, id string) {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE mailing_campaigns SET status = 'cancelled', completed_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status NOT IN ('sent','completed','completed_with_errors','deleted')
	`, id); err != nil {
		log.Printf("[PreDispatch] cancel %s failed: %v", id, err)
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE mailing_campaign_queue SET status = 'cancelled', updated_at = NOW()
		WHERE campaign_id = $1 AND status IN ('queued','paused')
	`, id); err != nil {
		log.Printf("[PreDispatch] queue cancel %s failed: %v", id, err)
	}
}

// predispatchSwap: cancel original, rename sibling to the original name, stamp.
// Guarded so a queue row appearing on the original mid-swap aborts the swap
// (the sibling is then cancelled and the original mails).
func (s *PMTACampaignService) predispatchSwap(ctx context.Context, d predispatchDeps, c predispatchCell, sibID string, recips int, segs map[string]predispatchSegment) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var queued int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mailing_campaign_queue WHERE campaign_id = $1`, c.ID).Scan(&queued); err != nil {
		return err
	}
	if queued > 0 {
		s.predispatchCancel(ctx, sibID)
		return fmt.Errorf("original %s enqueued %d rows mid-swap — sibling cancelled, original mails", c.ID, queued)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE mailing_campaigns SET status = 'cancelled', completed_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status = 'scheduled'
	`, c.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		s.predispatchCancel(ctx, sibID)
		return fmt.Errorf("original %s left 'scheduled' mid-swap — sibling cancelled", c.ID)
	}
	stamp := map[string]interface{}{
		"at":                d.now().UTC().Format(time.RFC3339),
		"from_campaign_id":  c.ID,
		"recipients_before": c.Recipients,
		"recipients_after":  recips,
		"segments":          predispatchSegmentStamps(c, segs),
	}
	stampJSON, _ := json.Marshal(stamp)
	if _, err := tx.ExecContext(ctx, `
		UPDATE mailing_campaigns
		SET name = $2::text,
		    pmta_config = jsonb_set(
		        jsonb_set(
		            jsonb_set(COALESCE(pmta_config,'{}'::jsonb), '{campaign_input,name}', to_jsonb($2::text), true),
		            '{rebuilt_from}', to_jsonb($3::text), true),
		        ARRAY['predispatch_refresh']::text[], $4::jsonb, true),
		    updated_at = NOW()
		WHERE id = $1::uuid
	`, sibID, c.Name, c.ID, string(stampJSON)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	writeAuditLog(ctx, s.db, "system:predispatch_refresh", "predispatch_rebind", "mailing_campaign", c.ID,
		map[string]interface{}{"campaign_id": c.ID, "name": c.Name, "recipients": c.Recipients},
		map[string]interface{}{"new_campaign_id": sibID, "recipients": recips, "stamp": stamp})
	return nil
}

func predispatchSegmentStamps(c predispatchCell, segs map[string]predispatchSegment) []map[string]string {
	out := make([]map[string]string, 0, len(c.Segments))
	for _, id := range c.Segments {
		sg := segs[id]
		built := ""
		if sg.BuiltAt.Valid {
			built = sg.BuiltAt.Time.UTC().Format(time.RFC3339)
		}
		out = append(out, map[string]string{"id": id, "name": sg.Name, "built_at": built})
	}
	return out
}

// predispatchRecoverOrphans finishes or discards "~fresh" siblings left by a
// crash between deploy and swap. Adopt when the sibling is verified and the
// original is still scheduled; cancel the sibling otherwise.
func (s *PMTACampaignService) predispatchRecoverOrphans(ctx context.Context, d predispatchDeps) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, organization_id::text, name, status, COALESCE(total_recipients,0),
		       (SELECT count(*) FROM mailing_campaign_waves w WHERE w.campaign_id = c.id),
		       created_at, scheduled_at
		FROM mailing_campaigns c
		WHERE name LIKE '%' || $1 AND status IN ('scheduled','failed','finalizing_audience','preparing')
	`, predispatchSiblingSfx)
	if err != nil {
		log.Printf("[PreDispatch] orphan scan: %v", err)
		return
	}
	type orphan struct {
		id, org, name, status string
		recips, waves         int
		created               time.Time
		anchor                sql.NullTime
	}
	var orphans []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.id, &o.org, &o.name, &o.status, &o.recips, &o.waves, &o.created, &o.anchor); err == nil {
			orphans = append(orphans, o)
		}
	}
	rows.Close()
	for _, o := range orphans {
		if o.status == "finalizing_audience" || o.status == "preparing" {
			if d.now().Sub(o.created) < predispatchVerifyMax {
				continue // an in-flight rebind on this or the other instance
			}
		}
		orig := strings.TrimSuffix(o.name, predispatchSiblingSfx)
		var origID, origStatus string
		var origRecips, queued int
		oerr := s.db.QueryRowContext(ctx, `
			SELECT id::text, status, COALESCE(total_recipients,0),
			       (SELECT count(*) FROM mailing_campaign_queue q WHERE q.campaign_id = c.id)
			FROM mailing_campaigns c
			WHERE organization_id = $1 AND name = $2 AND status NOT IN ('cancelled','deleted','failed')
			ORDER BY created_at DESC LIMIT 1
		`, o.org, orig).Scan(&origID, &origStatus, &origRecips, &queued)
		if o.status == "scheduled" && o.recips > 0 && o.waves > 0 && oerr == nil && origStatus == "scheduled" && queued == 0 {
			c := predispatchCell{ID: origID, OrgID: o.org, Name: orig, Recipients: origRecips}
			if err := s.predispatchSwap(ctx, d, c, o.id, o.recips, map[string]predispatchSegment{}); err != nil {
				log.Printf("[PreDispatch] orphan %q adopt failed: %v", o.name, err)
				// Last resort: a sibling and its original must never BOTH reach
				// the anchor (double send). Inside the guard window the
				// original wins and the sibling is discarded.
				if o.anchor.Valid && o.anchor.Time.Before(d.now().Add(predispatchDoubleSendGuard)) {
					log.Printf("[PreDispatch] orphan %q inside double-send guard — sibling cancelled, original mails", o.name)
					s.predispatchCancel(ctx, o.id)
				}
			} else {
				log.Printf("[PreDispatch] orphan %q adopted as %q", o.name, orig)
			}
			continue
		}
		if oerr == sql.ErrNoRows && o.status == "scheduled" && o.recips > 0 && o.waves > 0 {
			// Original already cancelled (crash after cancel, before rename): just take the name.
			if _, err := s.db.ExecContext(ctx, `
				UPDATE mailing_campaigns SET name = $2::text,
				    pmta_config = jsonb_set(COALESCE(pmta_config,'{}'::jsonb), '{campaign_input,name}', to_jsonb($2::text), true),
				    updated_at = NOW() WHERE id = $1::uuid
			`, o.id, orig); err == nil {
				log.Printf("[PreDispatch] orphan %q renamed to %q (original already cancelled)", o.name, orig)
			}
			continue
		}
		log.Printf("[PreDispatch] orphan %q (%s recips=%d waves=%d) discarded", o.name, o.status, o.recips, o.waves)
		s.predispatchCancel(ctx, o.id)
	}
}
