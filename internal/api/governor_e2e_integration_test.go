//go:build integration

// SendGovernor END-TO-END over the REAL schema on local apex-postgres (never prod):
//
//	docker exec apex-postgres psql -U apex_user -d postgres -c "CREATE DATABASE governor_e2e"
//	DATABASE_URL=postgres://apex_user:apex_password@localhost:5432/governor_e2e?sslmode=disable go run ./cmd/migrate/ migrations/
//	go test -tags integration -run GovernorE2ESchema ./cmd/server/
//	go test -tags integration -run GovernorE2E ./internal/api/ -v
//
// The chain under test is the one prod runs: HandleDeployCampaign → the
// audience finalizer (planPMTAAudience → createPMTAWaveCampaign, the 7 tables)
// → EnqueuePMTAWaveGoverned (the dispatcher with the SendGovernor ON) →
// mailing_campaign_queue rows + family_governor_decisions. Assertions are made
// by SQL against the scratch database, by NAME.
package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"

	"github.com/ignite/sparkpost-monitor/internal/worker"
)

const governorE2EDefaultDSN = "postgres://apex_user:apex_password@localhost:5432/governor_e2e?sslmode=disable"

const (
	e2eOrg    = defaultOrgID
	e2eDomain = "m.discountblog.com"
	e2eLane   = "broadcast-cold.m.discountblog.com"
)

func e2eDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("GOVERNOR_E2E_DSN")
	if dsn == "" {
		dsn = governorE2EDefaultDSN
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("scratch db unreachable (%v) — see the file header", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func e2eExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s\n%v", q, err)
	}
}

func e2eReset(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, tbl := range []string{"mailing_campaign_queue", "mailing_campaign_waves", "mailing_campaign_isp_time_spans",
		"mailing_campaign_isp_plans", "mailing_campaign_plan_recipients", "mailing_ab_variants", "mailing_ab_tests",
		"mailing_campaigns", "mailing_segment_members", "mailing_segments", "mailing_subscribers", "mailing_lists",
		"mailing_sending_profiles", "drip_dispatch_contracts", "family_governor_decisions"} {
		e2eExec(t, db, fmt.Sprintf(`DO $$ BEGIN IF to_regclass('public.%s') IS NOT NULL THEN EXECUTE 'TRUNCATE %s CASCADE'; END IF; END $$`, tbl, tbl))
	}
	e2eExec(t, db, `INSERT INTO organizations (id, name, slug) VALUES ($1, 'e2e', 'e2e') ON CONFLICT (id) DO NOTHING`, e2eOrg)
	e2eExec(t, db, `INSERT INTO mailing_sending_profiles (organization_id, name, vendor_type, from_name, from_email, sending_domain, status, is_default)
		VALUES ($1, 'e2e pmta', 'pmta', 'Jamie', 'jamie@m.discountblog.com', $2, 'active', true)`, e2eOrg, e2eDomain)
}

// e2eSegment materializes a static segment of n microsoft (hotmail) members.
func e2eSegment(t *testing.T, db *sql.DB, name string, n int) string {
	t.Helper()
	listID := uuid.New().String()
	e2eExec(t, db, `INSERT INTO mailing_lists (id, organization_id, name) VALUES ($1, $2, $3)`, listID, e2eOrg, name+" list")
	segID := uuid.New().String()
	e2eExec(t, db, `INSERT INTO mailing_segments (id, organization_id, name, segment_type, status, conditions) VALUES ($1, $2, $3, 'static', 'active', '[]')`, segID, e2eOrg, name)
	for i := 0; i < n; i++ {
		sid := uuid.New().String()
		email := fmt.Sprintf("%s.%d@hotmail.com", strings.ToLower(strings.ReplaceAll(name, " ", "")), i)
		e2eExec(t, db, `INSERT INTO mailing_subscribers (id, organization_id, list_id, email, email_hash, status) VALUES ($1, $2, $3, $4, md5($5), 'confirmed')`, sid, e2eOrg, listID, email, email)
		e2eExec(t, db, `INSERT INTO mailing_segment_members (segment_id, subscriber_id, email) VALUES ($1, $2, $3)`, segID, sid, email)
	}
	return segID
}

// e2eContract files an ACTIVE broadcast-cold contract for the domain: per-ISP
// intros + the domain total. version supersedes any earlier active row.
func e2eContract(t *testing.T, db *sql.DB, version, microsoft int) {
	t.Helper()
	e2eExec(t, db, `UPDATE drip_dispatch_contracts SET status='superseded', superseded_at=NOW() WHERE lane=$1 AND status='active'`, e2eLane)
	e2eExec(t, db, `INSERT INTO drip_dispatch_contracts (lane, version, status, effective_at, created_by, desired_daily_intros, daily_ceiling, allowed_domains, isp_exclusions, ladder_touches, max_intro_share)
		VALUES ($1, $2, 'active', NOW() - INTERVAL '1 hour', 'e2e', $3::jsonb, $4, '{DB}', '{}', 1, 1.0)`,
		e2eLane, version, fmt.Sprintf(`{"microsoft": %d}`, microsoft), microsoft)
}

func e2eService(t *testing.T, db *sql.DB) *PMTACampaignService {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return &PMTACampaignService{
		db: db, orgID: e2eOrg, suppMatcher: NewSuppressionMatcher(),
		colCache: probeCampaignColumns(ctx, db), preflightFn: passingPreflight, gateEvalFn: allGreenGates,
		skipBackgroundDeploy: true,
	}
}

func e2ePayload(name, segID string, start, end time.Time) map[string]any {
	span := map[string]any{"type": "absolute", "start_at": start.UTC().Format(time.RFC3339), "end_at": end.UTC().Format(time.RFC3339), "source": "duration-calc", "timezone": "America/Denver"}
	return map[string]any{
		"name": name, "lane": "cold", "sending_domain": e2eDomain, "target_isps": []string{"microsoft"},
		"variants":           []map[string]any{{"subject": "Jamie here", "from_name": "Jamie", "from_email": "jamie@m.discountblog.com", "html_content": "<html><body>e2e</body></html>", "weight": 100}},
		"inclusion_segments": []string{segID}, "exclusion_segments": []string{}, "exclusion_lists": []string{},
		"isp_quotas": []map[string]any{{"isp": "microsoft", "volume": 0}},
		"isp_plans": []map[string]any{{"isp": "microsoft", "quota": 0, "cadence": map[string]any{"mode": "interval", "every_minutes": 15},
			"timezone": "America/Denver", "throttle_strategy": "gentle", "time_spans": []map[string]any{span}}},
		"send_mode": "scheduled", "scheduled_at": start.UTC().Format(time.RFC3339), "timezone": "America/Denver",
		"throttle_strategy": "gentle", "min_remail_hours": 0, "use_master_selection": false, "content_locked": true, "randomize_audience": false,
	}
}

func e2ePost(t *testing.T, svc *PMTACampaignService, path string, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/pmta-campaign"+path, bytes.NewReader(raw))
	req.Header.Set("X-Organization-ID", e2eOrg)
	req.Header.Set("X-Internal-Caller", "cold_sidecar")
	rr := httptest.NewRecorder()
	if path == "/dry-run" {
		svc.HandleDryRunCampaign(rr, req)
	} else {
		svc.HandleDeployCampaign(rr, req)
	}
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func e2eCampaign(t *testing.T, db *sql.DB, name string) (id, status string, total int) {
	t.Helper()
	if err := db.QueryRow(`SELECT id::text, status, COALESCE(total_recipients,0) FROM mailing_campaigns WHERE name=$1`, name).Scan(&id, &status, &total); err != nil {
		t.Fatalf("campaign %q: %v", name, err)
	}
	return
}

func e2eCount(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestGovernorE2E_DeployFinalizeWavesEnqueue(t *testing.T) {
	db := e2eDB(t)
	e2eReset(t, db)
	t.Setenv(worker.SendGovernorModeEnv, "on")
	t.Setenv(worker.SendGovernorLanesEnv, "family,cold")
	t.Setenv("DISABLE_SEND_DAY_GATE_ENFORCEMENT", "1")
	t.Setenv("DISABLE_WAVE_AB_SPLIT", "true")
	t.Setenv("KAFKA_SEND_QUEUE_ALL", "")
	t.Setenv("KAFKA_SEND_QUEUE_WAVES", "")
	t.Setenv("KAFKA_SEND_QUEUE_CAMPAIGNS", "")
	t.Setenv("DISABLE_SETBASED_ENQUEUE", "")
	gov := worker.NewFamilyGovernor(db)
	SetSendGovernor(gov)
	t.Cleanup(func() { SetSendGovernor(nil) })
	svc := e2eService(t, db)

	segA := e2eSegment(t, db, "COLD e2e A", 500)
	segB := e2eSegment(t, db, "COLD e2e B", 500)
	e2eContract(t, db, 1, 150)
	start := time.Now().Add(30 * time.Minute)
	end := start.Add(4 * time.Hour)
	nameA := "09252026 - DB - NL-MS-NEWSLETTER-D12-COLD"
	nameB := "09252026 - DB - NL-MS-NEWSLETTER-D12-COLD-TOPUP"

	// D — dry-run previews the CLAMPED audience and writes nothing.
	code, out := e2ePost(t, svc, "/dry-run", e2ePayload(nameA, segA, start, end))
	if code != 200 {
		t.Fatalf("dry-run %d %v", code, out)
	}
	if tr, _ := out["total_recipients"].(float64); int(tr) != 150 {
		t.Fatalf("dry-run total_recipients = %v, want 150 (500 members clamped to the contract)", out["total_recipients"])
	}
	if n := e2eCount(t, db, `SELECT count(*) FROM mailing_campaigns`); n != 0 {
		t.Fatalf("dry-run wrote %d campaigns", n)
	}

	// C — a closed window is refused before any write.
	code, out = e2ePost(t, svc, "/deploy", e2ePayload("09232026 - DB - NL-YF-NEWSLETTER-D14-ENG", segA, time.Now().Add(-4*time.Hour), time.Now().Add(-3*time.Minute)))
	if code != 400 || !strings.Contains(fmt.Sprint(out["error"]), "send window closed") {
		t.Fatalf("closed window: %d %v", code, out)
	}
	if n := e2eCount(t, db, `SELECT count(*) FROM mailing_campaigns`); n != 0 {
		t.Fatalf("a refused deploy wrote %d campaigns", n)
	}

	// A — audience-bound cold cell (quota 0) on 500 members → BUILT at 150.
	code, out = e2ePost(t, svc, "/deploy", e2ePayload(nameA, segA, start, end))
	if code != 202 {
		t.Fatalf("deploy A: %d %v", code, out)
	}
	svc.processNextFinalizingCampaign(context.Background())
	idA, statusA, totalA := e2eCampaign(t, db, nameA)
	if statusA != "scheduled" || totalA != 150 {
		t.Fatalf("A after finalize: status=%s total=%d (want scheduled/150)", statusA, totalA)
	}
	if sel := e2eCount(t, db, `SELECT COALESCE(SUM(audience_selected_count),0) FROM mailing_campaign_isp_plans WHERE campaign_id=$1::uuid`, idA); sel != 150 {
		t.Fatalf("A isp plan selected = %d", sel)
	}
	if planned := e2eCount(t, db, `SELECT COALESCE(SUM(planned_recipients),0) FROM mailing_campaign_waves WHERE campaign_id=$1::uuid`, idA); planned != 150 {
		t.Fatalf("A waves planned = %d", planned)
	}

	// B — a second cell in the SAME lane × domain × day: headroom is 0 because
	// A already committed the contract → nothing selected → the finalizer
	// marks it failed (no qualified recipients). Nothing over-committed.
	code, out = e2ePost(t, svc, "/deploy", e2ePayload(nameB, segB, start.Add(10*time.Minute), end))
	if code != 202 {
		t.Fatalf("deploy B: %d %v", code, out)
	}
	svc.processNextFinalizingCampaign(context.Background())
	_, statusB, totalB := e2eCampaign(t, db, nameB)
	if statusB != "failed" || totalB != 0 {
		t.Fatalf("B after finalize: status=%s total=%d (want failed/0)", statusB, totalB)
	}

	// E — the wave hook: lower the contract to 120 (new version) and fire A's
	// waves through the governed dispatcher; a FRESH governor sees v2.
	e2eContract(t, db, 2, 120)
	gov2 := worker.NewFamilyGovernor(db)
	e2eExec(t, db, `UPDATE mailing_campaign_waves SET scheduled_at = NOW() - INTERVAL '1 minute', window_start_at = NOW() - INTERVAL '1 minute' WHERE campaign_id=$1::uuid`, idA)
	rows, err := db.Query(`SELECT id::text FROM mailing_campaign_waves WHERE campaign_id=$1::uuid ORDER BY wave_number`, idA)
	if err != nil {
		t.Fatal(err)
	}
	var waves []string
	for rows.Next() {
		var w string
		rows.Scan(&w)
		waves = append(waves, w)
	}
	rows.Close()
	enq := 0
	for _, w := range waves {
		n, err := worker.EnqueuePMTAWaveGoverned(context.Background(), db, w, gov2)
		if err != nil {
			t.Fatalf("wave %s: %v", w, err)
		}
		enq += n
	}
	queued := e2eCount(t, db, `SELECT count(*) FROM mailing_campaign_queue WHERE campaign_id=$1::uuid`, idA)
	if enq != 120 || queued != 120 {
		t.Fatalf("enqueued=%d queue rows=%d, want 120 (contract v2)", enq, queued)
	}
	dec := e2eCount(t, db, `SELECT count(*) FROM family_governor_decisions WHERE lane='cold' AND sending_domain=$1 AND isp='microsoft' AND reason IN ('trim','deny')`, e2eDomain)
	if dec == 0 {
		t.Fatal("no trim/deny decision was ledgered for the cold lane")
	}
	if within := e2eCount(t, db, `SELECT count(*) FROM family_governor_decisions WHERE lane='cold' AND mode='on'`); within == 0 {
		t.Fatal("decisions must carry mode=on and lane=cold")
	}
	t.Logf("A built at 150 of 500 (contract v1), B refused at 0, waves enqueued %d of 150 planned under contract v2=120, %d trim/deny decisions", enq, dec)
}
