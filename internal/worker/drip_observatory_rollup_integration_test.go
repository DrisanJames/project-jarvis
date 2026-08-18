//go:build integration

// Drip Observatory rollup — P2 real-PostgreSQL integration suite (Vector B
// plan rev 4.2 §11, worker scenarios). Sibling of
// drip_observatory_integration_test.go (P1 schema scenarios).
//
// RUN (local apex-postgres ONLY — never prod):
//
//	go test -tags integration -count=1 ./internal/worker/ -run 'TestObsIntegration' -v
//
// §6.10 substrate note (rev 4.2): the local substrate does NOT carry
// mailing_campaigns / mailing_tracking_events / mailing_message_log — this
// suite ships prod-shaped source fixtures. Shape sources: tracking insert
// columns handlers_ses_events.go:556 + migrations/037; message_log insert
// journey_clickdrip_sender.go:667-673; partner_dataset_id columns
// cmd/server/main.go:2181-2182; campaign columns verified against prod
// information_schema 2026-08-17. Fixture DDL lives HERE only — never in
// runStartupMigrations.
package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/brandident"
)

// obsSourceFixtureDDL — prod-shaped minimal source tables (idempotent).
var obsSourceFixtureDDL = []string{
	`CREATE TABLE IF NOT EXISTS mailing_campaigns (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		organization_id UUID,
		name VARCHAR(500) NOT NULL,
		status VARCHAR(50) NOT NULL DEFAULT 'draft',
		partner_dataset_id UUID,
		scheduled_at TIMESTAMPTZ,
		total_recipients INT NOT NULL DEFAULT 0,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	// Same definition as the §4 concurrentIndexSpecs entry (plain build is
	// fine on a fixture table) — also satisfies the worker's §4 index gate.
	`CREATE INDEX IF NOT EXISTS idx_mc_partner_dataset_sched
		ON mailing_campaigns (partner_dataset_id, scheduled_at) WHERE partner_dataset_id IS NOT NULL`,
	`CREATE TABLE IF NOT EXISTS mailing_tracking_events (
		id UUID NOT NULL DEFAULT gen_random_uuid(),
		organization_id UUID,
		campaign_id UUID,
		subscriber_id UUID,
		event_type VARCHAR(50) NOT NULL,
		bounce_type VARCHAR(50),
		link_url TEXT,
		event_at TIMESTAMPTZ NOT NULL,
		recipient_domain TEXT,
		is_machine_click BOOLEAN DEFAULT FALSE,
		ip_address INET,
		user_agent TEXT,
		bounce_reason TEXT,
		click_verdict TEXT,
		PRIMARY KEY (id, event_at)
	) PARTITION BY RANGE (event_at)`,
	`CREATE TABLE IF NOT EXISTS mailing_tracking_events_obs_fixture
		PARTITION OF mailing_tracking_events FOR VALUES FROM ('2020-01-01') TO ('2030-01-01')`,
	`CREATE TABLE IF NOT EXISTS mailing_message_log (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		message_id TEXT,
		organization_id UUID,
		campaign_id UUID,
		subscriber_id UUID,
		email TEXT,
		esp_type TEXT,
		sent_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		partner_dataset_id UUID
	)`,
}

type obsFixtureLane struct {
	t         *testing.T
	w         *DripObservatoryRollup
	orgID     string
	dsID      string
	partnerID string
	batchID   string
	created   time.Time
}

func obsSetupSource(t *testing.T) *DripObservatoryRollup {
	t.Helper()
	db := openDripObsWorkerDB(t)
	for _, ddl := range obsSourceFixtureDDL {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("source fixture DDL: %v\n%s", err, ddl)
		}
	}
	w := NewDripObservatoryRollup(db)
	w.firstDelay = 0
	return w
}

// obsNewLane creates an isolated fixture lane (fresh UUIDs per test) and
// gates the worker to it via DRIP_OBSERVATORY_LANES. created is backdated so
// the bootstrap window covers the fixture days.
func obsNewLane(t *testing.T, w *DripObservatoryRollup, vertical string, createdDaysAgo int) *obsFixtureLane {
	t.Helper()
	db := w.db
	var org, ds, partner string
	if err := db.QueryRow(`SELECT gen_random_uuid()::text, gen_random_uuid()::text, gen_random_uuid()::text`).Scan(&org, &ds, &partner); err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().AddDate(0, 0, -createdDaysAgo)
	// FK chain: data_partners ← partner_datasets ← partner_inbound_batches ←
	// partner_clean_queue (batch_id/partner_id/dataset_id all enforced).
	if _, err := db.Exec(`
		INSERT INTO data_partners (id, organization_id, name, slug, status)
		VALUES ($1, $2, $3, $4, 'active')
	`, partner, org, "obs-fixture-"+partner[:8], "obs-fixture-"+partner[:8]); err != nil {
		t.Fatalf("partner fixture: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO partner_datasets (id, partner_id, name, slug, vertical, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, ds, partner, "obs-fixture-"+ds[:8], "obs-fixture-"+ds[:8], vertical, created); err != nil {
		t.Fatalf("dataset fixture: %v", err)
	}
	var batch string
	if err := db.QueryRow(`
		INSERT INTO partner_inbound_batches (dataset_id, partner_id, s3_bucket, s3_key)
		VALUES ($1::uuid, $2, 'obs-fixture', 'obs-fixture/' || $1::text) RETURNING id
	`, ds, partner).Scan(&batch); err != nil {
		t.Fatalf("batch fixture: %v", err)
	}
	t.Setenv("DRIP_OBSERVATORY_LANES", ds)
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM partner_drip_send_cohort_daily WHERE dataset_id = $1`,
			`DELETE FROM partner_drip_event_daily WHERE dataset_id = $1`,
			`DELETE FROM partner_drip_observatory_run_scope WHERE dataset_id = $1`,
			`DELETE FROM partner_drip_observatory_cursor WHERE dataset_id = $1`,
			`DELETE FROM mailing_everflow_conversions WHERE campaign_id IN (SELECT id FROM mailing_campaigns WHERE partner_dataset_id = $1)`,
			`DELETE FROM pmta_acct_daily_summary WHERE campaign_id IN (SELECT id FROM mailing_campaigns WHERE partner_dataset_id = $1)`,
			`DELETE FROM mailing_tracking_events WHERE campaign_id IN (SELECT id FROM mailing_campaigns WHERE partner_dataset_id = $1)`,
			`DELETE FROM mailing_message_log WHERE partner_dataset_id = $1`,
			`DELETE FROM partner_clean_queue WHERE dataset_id = $1`,
			`DELETE FROM mailing_campaigns WHERE partner_dataset_id = $1`,
			`DELETE FROM partner_inbound_batches WHERE dataset_id = $1`,
			`DELETE FROM partner_datasets WHERE id = $1`,
		} {
			if _, err := db.Exec(q, ds); err != nil {
				t.Logf("cleanup %s: %v", q, err)
			}
		}
		if _, err := db.Exec(`DELETE FROM data_partners WHERE id = $1`, partner); err != nil {
			t.Logf("cleanup data_partners: %v", err)
		}
	})
	return &obsFixtureLane{t: t, w: w, orgID: org, dsID: ds, partnerID: partner, batchID: batch, created: created}
}

// addCampaign inserts a campaign whose name is GENERATED from the real
// orchestrator Sprintf literal with a ROSTER code (§11 fixture rule).
func (l *obsFixtureLane) addCampaign(vertical, brandToken, suffix string, scheduledAt time.Time, totalRecipients int) string {
	l.t.Helper()
	var id string
	if err := l.w.db.QueryRow(`
		INSERT INTO mailing_campaigns (organization_id, name, status, partner_dataset_id, scheduled_at, total_recipients)
		VALUES ($1, $2, 'sent', $3, $4, $5) RETURNING id
	`, l.orgID, obsFixtureName(vertical, brandToken, suffix), l.dsID, scheduledAt, totalRecipients).Scan(&id); err != nil {
		l.t.Fatalf("campaign fixture: %v", err)
	}
	return id
}

func (l *obsFixtureLane) addPCQ(campID string, mailedAt time.Time, ispFamily string, n int) {
	l.t.Helper()
	if _, err := l.w.db.Exec(`
		INSERT INTO partner_clean_queue (batch_id, dataset_id, partner_id, vertical, email, email_md5,
			isp_family, status, mailed_campaign_id, mailed_at)
		SELECT $6, $1, $7, 'fixture',
			'obs-' || gs::text || '-' || $3 || '-' || substr($2::text, 1, 8) || '@example.com',
			md5('obs-' || gs::text || $3 || $2::text), $3, 'mailed', $2::uuid, $4
		FROM generate_series(1, $5) gs
	`, l.dsID, campID, ispFamily, mailedAt, n, l.batchID, l.partnerID); err != nil {
		l.t.Fatalf("pcq fixture: %v", err)
	}
}

func (l *obsFixtureLane) addEvents(campID, eventType, rdom, linkURL, verdict string, at time.Time, n int) {
	l.t.Helper()
	if _, err := l.w.db.Exec(`
		INSERT INTO mailing_tracking_events
			(organization_id, campaign_id, subscriber_id, event_type, link_url, event_at, recipient_domain, click_verdict)
		SELECT $1, $2, gen_random_uuid(), $3, NULLIF($4, ''), $5, $6, NULLIF($7, '')
		FROM generate_series(1, $8)
	`, l.orgID, campID, eventType, linkURL, at, rdom, verdict, n); err != nil {
		l.t.Fatalf("event fixture: %v", err)
	}
}

func (l *obsFixtureLane) addAcct(campID string, summaryDate string, isp string, delivered, sesDelivered, relayed, hard, soft, rep, complained, total int) {
	l.t.Helper()
	if _, err := l.w.db.Exec(`
		INSERT INTO pmta_acct_daily_summary
			(summary_date, campaign_id, recipient_isp, delivered, ses_delivered, relayed_to_ses,
			 hard_bounced, soft_bounced, reputation_blocked, complained, total_records, last_updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW())
	`, summaryDate, campID, isp, delivered, sesDelivered, relayed, hard, soft, rep, complained, total); err != nil {
		l.t.Fatalf("acct fixture: %v", err)
	}
}

func (l *obsFixtureLane) runCycle() {
	l.t.Helper()
	if err := l.w.runCycle(context.Background()); err != nil {
		l.t.Fatalf("runCycle: %v", err)
	}
}

// cohortLane reads the published cohort LANE row for (day, touch, brand)
// from the newest completed generation.
func (l *obsFixtureLane) factRow(t *testing.T, table, dayCol, day string, touch int, brand, ispVal string) map[string]interface{} {
	t.Helper()
	plannedExpr := "COALESCE(recipients_planned, -1)"
	if table == "partner_drip_event_daily" {
		plannedExpr = "-1" // no planning column on the event table (§3.2)
	}
	row := l.w.db.QueryRow(fmt.Sprintf(`
		SELECT COALESCE(opens, -1), COALESCE(delivered, -1), COALESCE(messages_dispatched_actual, -1),
		       dispatch_basis, click_actions_wrapper_basis, click_actions_resolved_basis,
		       click_actions_total, human_clicks, click_basis,
		       COALESCE(clicks_wrapper, -1), COALESCE(clicks_money, -1),
		       COALESCE(conversions, -1), %s,
		       brand_code, sending_apex, delivery_status, bounce_status
		FROM %s
		WHERE dataset_id = $1 AND %s = $2 AND touch_number = $3 AND brand_code = $4 AND isp = $5
		  AND run_id IN (SELECT run_id FROM partner_drip_observatory_runs
		                 WHERE status IN ('complete','complete_with_errors') AND source_pass = 'rollup')
	`, plannedExpr, table, dayCol), l.dsID, day, touch, brand, ispVal)
	out := map[string]interface{}{}
	var opens, delivered, dispatched, aw, ar, at2, human, cw, cm, conv, planned int
	var basis, clickBasis, brandCode, apex, dstat, bstat string
	if err := row.Scan(&opens, &delivered, &dispatched, &basis, &aw, &ar, &at2, &human, &clickBasis,
		&cw, &cm, &conv, &planned, &brandCode, &apex, &dstat, &bstat); err != nil {
		t.Fatalf("factRow(%s %s=%s t%d %s isp=%q): %v", table, dayCol, day, touch, brand, ispVal, err)
	}
	out["opens"], out["delivered"], out["dispatched"], out["basis"] = opens, delivered, dispatched, basis
	out["aw"], out["ar"], out["total"], out["human"], out["click_basis"] = aw, ar, at2, human, clickBasis
	out["cw"], out["cm"], out["conversions"], out["planned"] = cw, cm, conv, planned
	out["brand_code"], out["apex"] = brandCode, apex
	out["delivery_status"], out["bounce_status"] = dstat, bstat
	return out
}

func (l *obsFixtureLane) countRows(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := l.w.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE dataset_id = $1`, table), l.dsID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func obsDenverDayAgo(days int) string {
	return dripObsDenverDay(time.Now().AddDate(0, 0, -days))
}

// ---------------------------------------------------------------------------
// §11 #2 (+#23 worker half): full backfill, late event → full recompute
// ---------------------------------------------------------------------------

func TestObsIntegration_LateEventFullRecompute(t *testing.T) {
	w := obsSetupSource(t)
	l := obsNewLane(t, w, "internal_auto_insurance", 5)

	day := obsDenverDayAgo(3)
	dLo, _, _ := dripObsDayBounds(day)
	camp := l.addCampaign("internal_auto_insurance", "db", "", dLo.Add(9*time.Hour), 120)
	l.addPCQ(camp, dLo.Add(9*time.Hour), "gmail", 50)
	l.addEvents(camp, "opened", "gmail.com", "", "", dLo.Add(10*time.Hour), 100)
	// Long-tail recipient (§11 #23 worker half): must roll up as isp='other'
	// and pass the widened vocab CHECK.
	l.addEvents(camp, "opened", "randomtail-isp.net", "", "", dLo.Add(11*time.Hour), 4)

	l.runCycle() // bootstrap: full history [dataset.created_at, now)

	var bootstrapped bool
	if err := w.db.QueryRow(`
		SELECT bootstrapped_at IS NOT NULL FROM partner_drip_observatory_cursor
		WHERE source_pass = 'rollup' AND dataset_id = $1`, l.dsID).Scan(&bootstrapped); err != nil || !bootstrapped {
		t.Fatalf("bootstrap must set bootstrapped_at (err=%v got=%v)", err, bootstrapped)
	}

	lane := l.factRow(t, "partner_drip_send_cohort_daily", "cohort_day", day, 1, "db", "")
	if lane["opens"] != 104 {
		t.Fatalf("bootstrap cohort lane opens = %v, want 104", lane["opens"])
	}
	if lane["dispatched"] != 50 || lane["basis"] != "pcq" {
		t.Fatalf("bootstrap dispatch = %v basis %v, want 50/pcq", lane["dispatched"], lane["basis"])
	}
	if lane["planned"] != 120 {
		t.Fatalf("recipients_planned = %v, want 120 (lane rows only, never a denominator)", lane["planned"])
	}
	tail := l.factRow(t, "partner_drip_send_cohort_daily", "cohort_day", day, 1, "db", "other")
	if tail["opens"] != 4 {
		t.Fatalf("long-tail 'other' cohort row opens = %v, want 4 (#23 worker half)", tail["opens"])
	}

	// ONE late open lands inside the next incremental window: the FULL-KEY
	// recompute must yield 101 gmail-day opens... i.e. 100 stored + 1 late →
	// 101, never 1 (§6.3).
	l.addEvents(camp, "opened", "gmail.com", "", "", time.Now().Add(-time.Minute), 1)
	l.runCycle()

	lane = l.factRow(t, "partner_drip_send_cohort_daily", "cohort_day", day, 1, "db", "")
	if lane["opens"] != 105 {
		t.Fatalf("late-event recompute cohort lane opens = %v, want 105 (100+4 stored + 1 late — never 1)", lane["opens"])
	}
	gmail := l.factRow(t, "partner_drip_send_cohort_daily", "cohort_day", day, 1, "db", "gmail")
	if gmail["opens"] != 101 {
		t.Fatalf("late-event recompute gmail cohort opens = %v, want 101", gmail["opens"])
	}
	// §11 #19 half: the late open ALSO lands on its own event_day row.
	lateDay := dripObsDenverDay(time.Now().Add(-time.Minute))
	ev := l.factRow(t, "partner_drip_event_daily", "event_day", lateDay, 1, "db", "gmail")
	if ev["opens"] != 1 {
		t.Fatalf("late open event-day row opens = %v, want 1", ev["opens"])
	}
}

// ---------------------------------------------------------------------------
// §11 #4: midnight-spanning campaign — ALL outcomes → FIRST-dispatch day
// ---------------------------------------------------------------------------

func TestObsIntegration_MidnightSpanningCampaign(t *testing.T) {
	w := obsSetupSource(t)
	l := obsNewLane(t, w, "internal_auto_insurance", 6)

	day := obsDenverDayAgo(4)
	dLo, dHi, _ := dripObsDayBounds(day)
	// Dispatch 23:30 Denver; opens & clicks land after Denver midnight.
	camp := l.addCampaign("internal_auto_insurance", "db", "[ses:93938919]", dHi.Add(-30*time.Minute), 40)
	l.addPCQ(camp, dHi.Add(-30*time.Minute), "yahoo", 30)
	l.addEvents(camp, "opened", "yahoo.com", "", "", dHi.Add(2*time.Hour), 7)
	l.addEvents(camp, "clicked", "yahoo.com", "https://t.em.discountblog.com/o/tok123", "human", dHi.Add(3*time.Hour), 2)

	_ = dLo
	l.runCycle()

	lane := l.factRow(t, "partner_drip_send_cohort_daily", "cohort_day", day, 1, "db", "")
	if lane["opens"] != 7 || lane["total"] != 2 || lane["click_basis"] != "wrapper" || lane["human"] != 2 {
		t.Fatalf("midnight-spanning cohort lane = %+v, want opens=7 actions=2 wrapper human=2 on FIRST-dispatch day", lane)
	}
	// No cohort row may exist on the events' own day.
	nextDay := dripObsDenverDay(dHi.Add(2 * time.Hour))
	var n int
	if err := w.db.QueryRow(`
		SELECT COUNT(*) FROM partner_drip_send_cohort_daily WHERE dataset_id = $1 AND cohort_day = $2
	`, l.dsID, nextDay).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cohort rows on event day = %d (err=%v), want 0 — outcomes attribute to first-dispatch day", n, err)
	}
	// The events still appear on their own EVENT day (§3.1 two clocks).
	ev := l.factRow(t, "partner_drip_event_daily", "event_day", nextDay, 1, "db", "yahoo")
	if ev["opens"] != 7 {
		t.Fatalf("event-day row opens = %v, want 7", ev["opens"])
	}
}

// ---------------------------------------------------------------------------
// §11 #5 + #18: click canonical per campaign-day; mixed at aggregation
// ---------------------------------------------------------------------------

func TestObsIntegration_MixedBasisAggregate(t *testing.T) {
	w := obsSetupSource(t)
	l := obsNewLane(t, w, "internal_auto_insurance", 5)

	day := obsDenverDayAgo(2)
	dLo, _, _ := dripObsDayBounds(day)
	// Campaign A: wrapper-basis campaign-day — §11 #18: wrapper + resolved
	// rows for ONE physical click → 1 action, basis 'wrapper'.
	campA := l.addCampaign("internal_auto_insurance", "db", "", dLo.Add(8*time.Hour), 10)
	l.addPCQ(campA, dLo.Add(8*time.Hour), "gmail", 5)
	l.addEvents(campA, "clicked", "gmail.com", "https://t.em.discountblog.com/o/tokA?x=1", "human", dLo.Add(9*time.Hour), 2)
	l.addEvents(campA, "clicked", "gmail.com", "https://www.cratoolpro.com/offer/a?source_id=email&sub1=u&sub3=c", "human", dLo.Add(9*time.Hour), 2)
	// Campaign B: resolved-only campaign-day (SES wrapper rows absent).
	campB := l.addCampaign("internal_auto_insurance", "db", "[t1]", dLo.Add(8*time.Hour+time.Minute), 10)
	l.addPCQ(campB, dLo.Add(8*time.Hour+time.Minute), "gmail", 5)
	l.addEvents(campB, "clicked", "gmail.com", "https://www.cratoolpro.com/offer/b?source_id=email&sub1=u&sub3=c", "human", dLo.Add(10*time.Hour), 3)

	l.runCycle()

	gmail := l.factRow(t, "partner_drip_send_cohort_daily", "cohort_day", day, 1, "db", "gmail")
	// A: wrapper rows exist (2) → actions = wrapper count 2 (its 2 money
	// rows are raw diagnostics, NOT actions). B: no wrapper rows → fallback,
	// actions = money count 3. Aggregate: total = 2+3, basis 'mixed'.
	if gmail["aw"] != 2 || gmail["ar"] != 3 || gmail["total"] != 5 || gmail["click_basis"] != "mixed" {
		t.Fatalf("mixed aggregate = W%v R%v total %v basis %v, want 2/3/5/mixed", gmail["aw"], gmail["ar"], gmail["total"], gmail["click_basis"])
	}
	if gmail["cw"] != 2 || gmail["cm"] != 5 {
		t.Fatalf("raw clicks = wrapper %v money %v, want 2/5 (raw stored, never summed into actions)", gmail["cw"], gmail["cm"])
	}
	if gmail["human"] != 5 {
		t.Fatalf("human_clicks = %v, want 5 (each campaign's chosen-basis rows)", gmail["human"])
	}
}

// ---------------------------------------------------------------------------
// §11 #6/#6b: the full §6.6 identity chain on real fixture names
// ---------------------------------------------------------------------------

func TestObsIntegration_BrandCodeApexMapping(t *testing.T) {
	w := obsSetupSource(t)
	l := obsNewLane(t, w, "internal_auto_insurance", 4)

	day := obsDenverDayAgo(2)
	dLo, _, _ := dripObsDayBounds(day)
	campDB := l.addCampaign("internal_auto_insurance", "db", "", dLo.Add(8*time.Hour), 10)
	l.addPCQ(campDB, dLo.Add(8*time.Hour), "gmail", 5)
	// Unknown token → brand_unknown quarantine, zero fact rows (#6d covers
	// the run-status half).
	campZZ := l.addCampaign("internal_auto_insurance", "zzq", "", dLo.Add(8*time.Hour), 10)
	l.addPCQ(campZZ, dLo.Add(8*time.Hour), "gmail", 5)

	l.runCycle()

	row := l.factRow(t, "partner_drip_send_cohort_daily", "cohort_day", day, 1, "db", "gmail")
	if row["brand_code"] != "db" || row["apex"] != "discountblog.com" {
		t.Fatalf("chain result = (%v,%v), want (db, discountblog.com)", row["brand_code"], row["apex"])
	}
	if row["dispatched"] != 5 {
		t.Fatalf("resolved campaign dispatch = %v, want 5 (quarantined campaign contributes nothing)", row["dispatched"])
	}
	var qn int
	if err := w.db.QueryRow(`
		SELECT COUNT(*) FROM partner_drip_observatory_quarantine WHERE reason = 'brand_unknown' AND quarantine_key = $1
	`, campZZ).Scan(&qn); err != nil || qn != 1 {
		t.Fatalf("brand_unknown quarantine rows for zzq campaign = %d (err=%v), want 1", qn, err)
	}
}

func TestObsIntegration_OrchestratorCodeMismatchClass(t *testing.T) {
	w := obsSetupSource(t)
	l := obsNewLane(t, w, "internal_auto_insurance", 4)

	day := obsDenverDayAgo(2)
	dLo, _, _ := dripObsDayBounds(day)
	campW := l.addCampaign("internal_auto_insurance", "wfy", "", dLo.Add(8*time.Hour), 10)
	l.addPCQ(campW, dLo.Add(8*time.Hour), "gmail", 4)
	campY := l.addCampaign("internal_auto_insurance", "yih", "[t2]", dLo.Add(9*time.Hour), 10)
	l.addPCQ(campY, dLo.Add(9*time.Hour), "gmail", 6)

	l.runCycle()

	// wfy/yih (the 11/27 orchestrator≠brandident mismatch class) resolve via
	// the chain — NOT quarantined, NOT collide-resolved.
	rw := l.factRow(t, "partner_drip_send_cohort_daily", "cohort_day", day, 1, "wf", "gmail")
	if rw["apex"] != "warrantyforyou.com" || rw["dispatched"] != 4 {
		t.Fatalf("wfy chain = %+v, want warrantyforyou.com / 4", rw)
	}
	ry := l.factRow(t, "partner_drip_send_cohort_daily", "cohort_day", day, 2, "yi", "gmail")
	if ry["apex"] != "yourinsurancehub.com" || ry["dispatched"] != 6 {
		t.Fatalf("yih chain = %+v, want yourinsurancehub.com / 6", ry)
	}
	var qn int
	if err := w.db.QueryRow(`
		SELECT COUNT(*) FROM partner_drip_observatory_quarantine WHERE quarantine_key IN ($1, $2)
	`, campW, campY).Scan(&qn); err != nil || qn != 0 {
		t.Fatalf("mismatch-class campaigns quarantined %d times (err=%v), want 0", qn, err)
	}
}

// §11 #6c: overlay-only code wcl (→ m.wcl-heloc.com) resolves through the
// mailing_brand_metadata overlay path once its brandident onboard row exists
// (union-never-replacement DB refresh); without it, brand_unknown.
func TestObsIntegration_OverlayBrandWCL(t *testing.T) {
	w := obsSetupSource(t)
	l := obsNewLane(t, w, "refi_heloc", 4)

	// Overlay row (prod-verified shape: code WCL → m.wcl-heloc.com) + the
	// brandident onboard row that makes chain link 4 resolvable.
	if _, err := w.db.Exec(`
		INSERT INTO mailing_brand_metadata (brand_root, brand_code, brand_label, sending_domain,
			tracking_domain, image_domain, from_name, from_email, reply_email, physical_address)
		VALUES ('wcl-heloc.com', 'WCL', 'obs-fixture', 'm.wcl-heloc.com',
			't.wcl-heloc.com', 'img.wcl-heloc.com', 'fixture', 'f@wcl-heloc.com', 'r@wcl-heloc.com', 'fixture')
		ON CONFLICT DO NOTHING
	`); err != nil {
		t.Fatalf("overlay fixture: %v", err)
	}
	if _, err := w.db.Exec(`
		INSERT INTO mailing_brand_codes (brand_code, apex, source) VALUES ('wcl', 'wcl-heloc.com', 'onboard')
		ON CONFLICT DO NOTHING
	`); err != nil {
		t.Fatalf("brand_codes onboard fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = w.db.Exec(`DELETE FROM mailing_brand_codes WHERE brand_code = 'wcl' AND source = 'onboard'`)
		_, _ = w.db.Exec(`DELETE FROM mailing_brand_metadata WHERE brand_code = 'WCL' AND brand_label = 'obs-fixture'`)
		// Restore literal-only brandident state for later tests in this
		// process (union is per-refresh, not sticky).
		_ = brandident.RefreshFromDB(context.Background(), w.db)
	})

	day := obsDenverDayAgo(2)
	dLo, _, _ := dripObsDayBounds(day)
	camp := l.addCampaign("refi_heloc", "wcl", "", dLo.Add(8*time.Hour), 10)
	l.addPCQ(camp, dLo.Add(8*time.Hour), "gmail", 3)

	l.runCycle()

	row := l.factRow(t, "partner_drip_send_cohort_daily", "cohort_day", day, 1, "wcl", "gmail")
	if row["apex"] != "wcl-heloc.com" || row["dispatched"] != 3 {
		t.Fatalf("wcl overlay chain = %+v, want wcl-heloc.com / 3 (m.-label stripped)", row)
	}
}

// §11 #6d: token outside static map + overlay → brand_unknown quarantine,
// zero fact rows for that campaign, run complete_with_errors.
func TestObsIntegration_UnknownCodeQuarantine(t *testing.T) {
	w := obsSetupSource(t)
	l := obsNewLane(t, w, "internal_auto_insurance", 4)

	day := obsDenverDayAgo(2)
	dLo, _, _ := dripObsDayBounds(day)
	camp := l.addCampaign("internal_auto_insurance", "qqz", "", dLo.Add(8*time.Hour), 10)
	l.addPCQ(camp, dLo.Add(8*time.Hour), "gmail", 5)

	l.runCycle()

	if n := l.countRows(t, "partner_drip_send_cohort_daily"); n != 0 {
		t.Fatalf("quarantined campaign produced %d cohort rows, want 0", n)
	}
	if n := l.countRows(t, "partner_drip_event_daily"); n != 0 {
		t.Fatalf("quarantined campaign produced %d event rows, want 0", n)
	}
	var status string
	var quarantined int
	if err := w.db.QueryRow(`
		SELECT r.status, r.quarantined_units FROM partner_drip_observatory_runs r
		WHERE r.source_pass = 'rollup' AND r.run_id IN
			(SELECT run_id FROM partner_drip_observatory_quarantine WHERE quarantine_key = $1)
	`, camp).Scan(&status, &quarantined); err != nil {
		t.Fatalf("run lookup: %v", err)
	}
	if status != "complete_with_errors" || quarantined < 1 {
		t.Fatalf("run = %s quarantined=%d, want complete_with_errors / ≥1", status, quarantined)
	}
}

// ---------------------------------------------------------------------------
// §11 #1: swap scope — exact pairs only
// ---------------------------------------------------------------------------

func TestObsIntegration_SwapScopeExactPairs(t *testing.T) {
	w := obsSetupSource(t)
	db := w.db

	org := "0b5e17a1-0000-4000-8000-00000000ee01"
	dsA := "0b5e17a1-0000-4000-8000-00000000ee0a"
	dsB := "0b5e17a1-0000-4000-8000-00000000ee0b"
	day1, day2 := "2026-08-01", "2026-08-02"

	insFact := func(runID, ds, day string) {
		t.Helper()
		if _, err := db.Exec(`
			INSERT INTO partner_drip_send_cohort_daily
				(run_id, organization_id, cohort_day, dataset_id, vertical, touch_number, brand_code,
				 sending_apex, dimension_scope, isp, dispatch_basis,
				 click_actions_wrapper_basis, click_actions_resolved_basis, click_actions_total, human_clicks, click_basis,
				 dispatch_status, delivery_status, bounce_status, engagement_status, conversion_status)
			VALUES ($1,$2,$3,$4,'fixture',1,'db','discountblog.com','lane','', 'pcq',
			        0,0,0,0,'none','available','available','partial','available','available')
		`, runID, org, day, ds); err != nil {
			t.Fatalf("fact fixture: %v", err)
		}
	}
	var oldRun, newRun string
	if err := db.QueryRow(`INSERT INTO partner_drip_observatory_runs (operational_day, source_pass, status, completed_at)
		VALUES (CURRENT_DATE, 'rollup', 'complete', NOW()) RETURNING run_id`).Scan(&oldRun); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`INSERT INTO partner_drip_observatory_runs (operational_day, source_pass)
		VALUES (CURRENT_DATE, 'rollup') RETURNING run_id`).Scan(&newRun); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM partner_drip_send_cohort_daily WHERE run_id IN ($1, $2)`, oldRun, newRun)
		_, _ = db.Exec(`DELETE FROM partner_drip_observatory_runs WHERE run_id IN ($1, $2)`, oldRun, newRun)
	})

	// Old generation: all four (day × dataset) combinations.
	insFact(oldRun, dsA, day1)
	insFact(oldRun, dsA, day2)
	insFact(oldRun, dsB, day1)
	insFact(oldRun, dsB, day2)
	// New generation scoped to the EXACT pairs (day1,A) + (day2,B) only.
	for _, pair := range [][2]string{{day1, dsA}, {day2, dsB}} {
		if _, err := db.Exec(`
			INSERT INTO partner_drip_observatory_run_scope (run_id, fact_kind, organization_id, day, dataset_id)
			VALUES ($1, 'cohort', $2, $3, $4)`, newRun, org, pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
		insFact(newRun, pair[1], pair[0])
	}

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got, err := w.acquireLock(ctx, conn); err != nil || !got {
		t.Fatalf("fixture lock acquire: got=%v err=%v", got, err)
	}
	defer conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, dripObservatoryLockID)

	if err := w.swapPublish(ctx, conn, newRun, "complete", 2, 2, 0); err != nil {
		t.Fatalf("swapPublish: %v", err)
	}

	survivors := map[[2]string]string{}
	rows, err := db.Query(`
		SELECT dataset_id, cohort_day::text, run_id FROM partner_drip_send_cohort_daily
		WHERE run_id IN ($1, $2)`, oldRun, newRun)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var ds, day, run string
		if err := rows.Scan(&ds, &day, &run); err != nil {
			t.Fatal(err)
		}
		survivors[[2]string{ds, day}] = run
	}
	// Scoped pairs swapped to the new generation…
	if survivors[[2]string{dsA, day1}] != newRun || survivors[[2]string{dsB, day2}] != newRun {
		t.Fatalf("scoped pairs not swapped: %v", survivors)
	}
	// …and the UNscoped combinations (day1,B)/(day2,A) remain the OLD rows.
	if survivors[[2]string{dsB, day1}] != oldRun || survivors[[2]string{dsA, day2}] != oldRun {
		t.Fatalf("unscoped pairs touched by swap: %v", survivors)
	}
}

// ---------------------------------------------------------------------------
// §11 #10: lock-session loss mid-run → run cancels, no swap
// ---------------------------------------------------------------------------

func TestObsIntegration_LockSessionLossMidRun(t *testing.T) {
	w := obsSetupSource(t)
	l := obsNewLane(t, w, "internal_auto_insurance", 4)

	day := obsDenverDayAgo(2)
	dLo, _, _ := dripObsDayBounds(day)
	camp := l.addCampaign("internal_auto_insurance", "db", "", dLo.Add(8*time.Hour), 10)
	l.addPCQ(camp, dLo.Add(8*time.Hour), "gmail", 5)

	// Kill the reserved lock session between stage-1 discovery and the
	// scope publish — pool reads keep working, conn writes must fail and
	// the ownership check must cancel the run (mark failed, NO swap).
	w.hookAfterDiscovery = func(dsID string) {
		if dsID != l.dsID || w.lockPID == 0 {
			return
		}
		if _, err := w.db.Exec(`SELECT pg_terminate_backend($1)`, w.lockPID); err != nil {
			t.Logf("terminate: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Cleanup(func() { w.hookAfterDiscovery = nil })

	err := w.runCycle(context.Background())
	if err == nil {
		t.Fatal("runCycle must surface the cancelled run as an error")
	}

	// The run this cycle opened is 'failed', and NO fact rows published.
	var status string
	if qerr := w.db.QueryRow(`
		SELECT status FROM partner_drip_observatory_runs
		WHERE source_pass = 'rollup' ORDER BY started_at DESC LIMIT 1
	`).Scan(&status); qerr != nil {
		t.Fatal(qerr)
	}
	if status != "failed" {
		t.Fatalf("run status = %s, want failed", status)
	}
	if n := l.countRows(t, "partner_drip_send_cohort_daily"); n != 0 {
		t.Fatalf("cancelled run published %d cohort rows, want 0 (no swap)", n)
	}
}

// ---------------------------------------------------------------------------
// §11 #17: acct last_updated_at change OUTSIDE the event window re-scopes
// ---------------------------------------------------------------------------

func TestObsIntegration_OutOfWatermarkCorrection(t *testing.T) {
	w := obsSetupSource(t)
	l := obsNewLane(t, w, "internal_auto_insurance", 8)

	day := obsDenverDayAgo(6)
	dLo, _, _ := dripObsDayBounds(day)
	camp := l.addCampaign("internal_auto_insurance", "db", "", dLo.Add(8*time.Hour), 10)
	l.addPCQ(camp, dLo.Add(8*time.Hour), "gmail", 8)
	l.addAcct(camp, day, "gmail", 6, 0, 2, 1, 1, 0, 0, 12)

	l.runCycle() // bootstrap

	row := l.factRow(t, "partner_drip_send_cohort_daily", "cohort_day", day, 1, "db", "gmail")
	if row["delivered"] != 6 {
		t.Fatalf("bootstrap delivered = %v, want 6", row["delivered"])
	}

	// Correction arrives for the OLD summary_date — only last_updated_at is
	// inside the next discovery window (§6.3: the acct upsert stamp is the
	// delivery change-discovery cursor).
	if _, err := w.db.Exec(`
		UPDATE pmta_acct_daily_summary SET delivered = 25, last_updated_at = NOW() WHERE campaign_id = $1
	`, camp); err != nil {
		t.Fatal(err)
	}
	l.runCycle()

	row = l.factRow(t, "partner_drip_send_cohort_daily", "cohort_day", day, 1, "db", "gmail")
	if row["delivered"] != 25 {
		t.Fatalf("out-of-watermark correction: delivered = %v, want 25", row["delivered"])
	}
	ev := l.factRow(t, "partner_drip_event_daily", "event_day", day, 1, "db", "gmail")
	if ev["delivered"] != 25 {
		t.Fatalf("event-day (summary_date UTC receipt basis) delivered = %v, want 25", ev["delivered"])
	}
}

// ---------------------------------------------------------------------------
// §11 #18: one click, two rows (wrapper + resolved) → ONE action
// ---------------------------------------------------------------------------

func TestObsIntegration_OneClickTwoRowsOneAction(t *testing.T) {
	w := obsSetupSource(t)
	l := obsNewLane(t, w, "internal_auto_insurance", 4)

	day := obsDenverDayAgo(2)
	dLo, _, _ := dripObsDayBounds(day)
	// One SES-tracked physical click = wrapper row + resolved (money) row.
	campA := l.addCampaign("internal_auto_insurance", "db", "", dLo.Add(8*time.Hour), 10)
	l.addPCQ(campA, dLo.Add(8*time.Hour), "gmail", 5)
	l.addEvents(campA, "clicked", "gmail.com", "https://t.em.discountblog.com/o/onetok", "human", dLo.Add(9*time.Hour), 1)
	l.addEvents(campA, "clicked", "gmail.com", "https://www.cratoolpro.com/offer/one?source_id=email&sub1=u&sub3=c", "human", dLo.Add(9*time.Hour), 1)

	// A resolved-only campaign-day labels 'resolved-only'.
	day2 := obsDenverDayAgo(1)
	d2Lo, _, _ := dripObsDayBounds(day2)
	campB := l.addCampaign("internal_auto_insurance", "db", "[t2]", d2Lo.Add(8*time.Hour), 10)
	l.addPCQ(campB, d2Lo.Add(8*time.Hour), "gmail", 5)
	l.addEvents(campB, "clicked", "gmail.com", "https://www.cratoolpro.com/offer/two?source_id=email&sub1=u&sub3=c", "", d2Lo.Add(9*time.Hour), 1)

	l.runCycle()

	a := l.factRow(t, "partner_drip_send_cohort_daily", "cohort_day", day, 1, "db", "gmail")
	if a["total"] != 1 || a["click_basis"] != "wrapper" || a["cw"] != 1 || a["cm"] != 1 {
		t.Fatalf("wrapper+resolved pair = total %v basis %v cw %v cm %v, want 1/wrapper/1/1", a["total"], a["click_basis"], a["cw"], a["cm"])
	}
	b := l.factRow(t, "partner_drip_send_cohort_daily", "cohort_day", day2, 2, "db", "gmail")
	if b["total"] != 1 || b["click_basis"] != "resolved-only" {
		t.Fatalf("resolved-only = total %v basis %v, want 1/resolved-only", b["total"], b["click_basis"])
	}
}

// ---------------------------------------------------------------------------
// §11 #19: late open lands on its event_day AND updates its cohort_day row
// ---------------------------------------------------------------------------

func TestObsIntegration_EventVsCohortDay(t *testing.T) {
	w := obsSetupSource(t)
	l := obsNewLane(t, w, "internal_auto_insurance", 6)

	cohortDay := obsDenverDayAgo(4)
	cLo, _, _ := dripObsDayBounds(cohortDay)
	camp := l.addCampaign("internal_auto_insurance", "db", "", cLo.Add(8*time.Hour), 10)
	l.addPCQ(camp, cLo.Add(8*time.Hour), "gmail", 5)
	l.addEvents(camp, "opened", "gmail.com", "", "", cLo.Add(9*time.Hour), 3)
	l.runCycle()

	// A late open 3 days after dispatch, inside the incremental window.
	l.addEvents(camp, "opened", "gmail.com", "", "", time.Now().Add(-time.Minute), 1)
	// Conversions attribute to lane rows on BOTH clocks too (§6.6).
	if _, err := w.db.Exec(`
		INSERT INTO mailing_everflow_conversions (organization_id, campaign_id, payout, converted_at)
		VALUES ($1, $2, 12.50, NOW() - interval '1 minute')
	`, l.orgID, camp); err != nil {
		t.Fatal(err)
	}
	l.runCycle()

	lateDay := dripObsDenverDay(time.Now().Add(-time.Minute))
	ev := l.factRow(t, "partner_drip_event_daily", "event_day", lateDay, 1, "db", "gmail")
	if ev["opens"] != 1 {
		t.Fatalf("event-day opens = %v, want 1 (the late open on ITS day)", ev["opens"])
	}
	co := l.factRow(t, "partner_drip_send_cohort_daily", "cohort_day", cohortDay, 1, "db", "gmail")
	if co["opens"] != 4 {
		t.Fatalf("cohort-day opens = %v, want 4 (3 + the late one on the FIRST-dispatch day)", co["opens"])
	}
	laneEv := l.factRow(t, "partner_drip_event_daily", "event_day", lateDay, 1, "db", "")
	if laneEv["conversions"] != 1 {
		t.Fatalf("event-day lane conversions = %v, want 1", laneEv["conversions"])
	}
	laneCo := l.factRow(t, "partner_drip_send_cohort_daily", "cohort_day", cohortDay, 1, "db", "")
	if laneCo["conversions"] != 1 {
		t.Fatalf("cohort lane conversions = %v, want 1", laneCo["conversions"])
	}
}

// ---------------------------------------------------------------------------
// P2 gate 3 analog (§6.10): accounting identity on the staged fixtures
// ---------------------------------------------------------------------------

func TestObsIntegration_AcctTerminalIdentity(t *testing.T) {
	w := obsSetupSource(t)
	l := obsNewLane(t, w, "internal_auto_insurance", 6)

	day := obsDenverDayAgo(3)
	dLo, _, _ := dripObsDayBounds(day)
	// t2 follow-up: NO pcq rows (mailed_* is first-touch-only), NO message
	// log → dispatch falls to 'acct_terminal' = delivered + relayed + hard +
	// reputation + soft + complained (NEVER total_records).
	camp := l.addCampaign("internal_auto_insurance", "db", "[t2]", dLo.Add(8*time.Hour), 10)
	l.addAcct(camp, day, "gmail", 10, 5, 20, 3, 2, 1, 1, 60)

	l.runCycle()

	row := l.factRow(t, "partner_drip_send_cohort_daily", "cohort_day", day, 2, "db", "gmail")
	// delivered column = SUM(delivered)+SUM(ses_delivered) = 15 (metrics.go
	// semantics); acct_terminal dispatch = 15+20+3+1+2+1 = 42 ≠ total_records 60.
	if row["delivered"] != 15 {
		t.Fatalf("delivered = %v, want 15 (delivered + ses_delivered)", row["delivered"])
	}
	if row["basis"] != "acct_terminal" || row["dispatched"] != 42 {
		t.Fatalf("dispatch = %v/%v, want acct_terminal/42 (never total_records)", row["basis"], row["dispatched"])
	}
	// Accounting identity (gate 3): total_records ≥ Σ categories.
	var total, categories int
	if err := w.db.QueryRow(`
		SELECT total_records, delivered + ses_delivered + relayed_to_ses + hard_bounced + soft_bounced
		       + reputation_blocked + complained
		FROM pmta_acct_daily_summary WHERE campaign_id = $1`, camp).Scan(&total, &categories); err != nil {
		t.Fatal(err)
	}
	if total < categories {
		t.Fatalf("accounting identity violated in fixture: total_records %d < Σcategories %d", total, categories)
	}
}

// ---------------------------------------------------------------------------
// P2 gate 2 analog (§6.10): shadow reconciliation — the observatory's
// published rows vs direct source aggregation over the same fixtures
// ---------------------------------------------------------------------------

func TestObsIntegration_ShadowReconciliation(t *testing.T) {
	w := obsSetupSource(t)
	l := obsNewLane(t, w, "internal_auto_insurance", 6)

	day := obsDenverDayAgo(3)
	dLo, _, _ := dripObsDayBounds(day)
	camp := l.addCampaign("internal_auto_insurance", "db", "", dLo.Add(8*time.Hour), 80)
	l.addPCQ(camp, dLo.Add(8*time.Hour), "gmail", 40)
	l.addPCQ(camp, dLo.Add(8*time.Hour), "yahoo", 20)
	l.addEvents(camp, "opened", "gmail.com", "", "", dLo.Add(10*time.Hour), 25)
	l.addEvents(camp, "opened", "yahoo.com", "", "", dLo.Add(11*time.Hour), 10)
	l.addEvents(camp, "unsubscribed", "gmail.com", "", "", dLo.Add(12*time.Hour), 2)
	l.addEvents(camp, "bounced", "gmail.com", "", "", dLo.Add(10*time.Hour), 3) // non-validation: acct owns bounce classes
	l.addAcct(camp, day, "gmail", 30, 5, 4, 2, 1, 1, 1, 50)

	l.runCycle()

	// Reconciliation 1 — dispatch: observatory lane row vs pcq GROUP BY.
	var srcDispatch int
	if err := w.db.QueryRow(`
		SELECT COUNT(*) FROM partner_clean_queue WHERE mailed_campaign_id = $1 AND mailed_at IS NOT NULL
	`, camp).Scan(&srcDispatch); err != nil {
		t.Fatal(err)
	}
	lane := l.factRow(t, "partner_drip_send_cohort_daily", "cohort_day", day, 1, "db", "")
	if lane["dispatched"] != srcDispatch {
		t.Fatalf("dispatch reconciliation: observatory %v vs source %d", lane["dispatched"], srcDispatch)
	}

	// Reconciliation 2 — engagement events (past-tense types).
	var srcOpens, srcUnsubs int
	if err := w.db.QueryRow(`
		SELECT COUNT(*) FILTER (WHERE event_type = 'opened'),
		       COUNT(*) FILTER (WHERE event_type = 'unsubscribed')
		FROM mailing_tracking_events WHERE campaign_id = $1
	`, camp).Scan(&srcOpens, &srcUnsubs); err != nil {
		t.Fatal(err)
	}
	if lane["opens"] != srcOpens {
		t.Fatalf("opens reconciliation: observatory %v vs source %d", lane["opens"], srcOpens)
	}

	// Reconciliation 3 — delivered (metrics.go semantics: delivered +
	// ses_delivered; never another formula).
	var srcDelivered int
	if err := w.db.QueryRow(`
		SELECT COALESCE(SUM(delivered),0) + COALESCE(SUM(ses_delivered),0)
		FROM pmta_acct_daily_summary WHERE campaign_id = $1
	`, camp).Scan(&srcDelivered); err != nil {
		t.Fatal(err)
	}
	if lane["delivered"] != srcDelivered {
		t.Fatalf("delivered reconciliation: observatory %v vs source %d", lane["delivered"], srcDelivered)
	}

	// Reconciliation 4 — ISP split sums to the lane row (no leakage).
	var ispSumOpens int
	if err := w.db.QueryRow(`
		SELECT COALESCE(SUM(opens), 0) FROM partner_drip_send_cohort_daily
		WHERE dataset_id = $1 AND cohort_day = $2 AND dimension_scope = 'lane_isp'
	`, l.dsID, day).Scan(&ispSumOpens); err != nil {
		t.Fatal(err)
	}
	if ispSumOpens != srcOpens {
		t.Fatalf("ISP-split reconciliation: Σ lane_isp opens %d vs source %d", ispSumOpens, srcOpens)
	}

	// Guard: tracking 'bounced' rows WITHOUT bounce_type='validation' never
	// leak into the acct bounce classes (§3.8 — three DISTINCT classes from
	// acct only).
	var hard int
	if err := w.db.QueryRow(`
		SELECT COALESCE(hard_bounced, -1) FROM partner_drip_send_cohort_daily
		WHERE dataset_id = $1 AND cohort_day = $2 AND dimension_scope = 'lane'
	`, l.dsID, day).Scan(&hard); err != nil {
		t.Fatal(err)
	}
	if hard != 2 {
		t.Fatalf("hard_bounced = %d, want 2 (acct only — tracking bounces excluded)", hard)
	}
}
