//go:build integration

// Pipeline Cockpit P2 integration fixture (plan §4 DoD): a throttle WRITE
// through the ONE writer (HandleUpdateISPDistribution — the endpoint the
// cockpit UI reuses) must be visible to the DRIP ORCHESTRATOR's own read
// shapes — the queries partner_drip_orchestrator.go runs fresh each wave:
//
//	applyDatasetISPCapOverrides: LOWER(TRIM(isp)), max_per_wave
//	    WHERE dataset_id = $1::uuid AND max_per_wave IS NOT NULL AND max_per_wave > 0
//	datasetDailyISPCaps:         LOWER(TRIM(isp)), daily_cap
//	    WHERE dataset_id = $1::uuid AND daily_cap IS NOT NULL
//
// RUN (local apex-postgres ONLY — never prod):
//
//	go test -tags integration -run LaneThrottleWrite ./internal/api/ -v
//
// PREREQUISITES: the local apex-postgres container with the data-partner
// startup migrations applied (boot `go run ./cmd/server/` once). SKIPs with a
// clear message otherwise — check-first, per the Observatory substrate note.
package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

func TestLaneThrottleWriteVisibleToOrchestratorRead(t *testing.T) {
	db := openPropertyLedgerDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Substrate check-first: the overrides table must exist (migrations).
	var reg sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT to_regclass('partner_isp_distribution_overrides')::text`).Scan(&reg); err != nil || !reg.Valid {
		t.Skip("SKIP: partner_isp_distribution_overrides missing — apply startup migrations (boot ./cmd/server once)")
	}

	// Fixture partner + dataset (cleaned up; FK cascade removes overrides).
	var partnerID string
	err := db.QueryRowContext(ctx, `
		INSERT INTO data_partners (name, slug)
		VALUES ('itest-cockpit-p2', 'itest-cockpit-p2')
		ON CONFLICT (slug) DO UPDATE SET updated_at = NOW()
		RETURNING id::text`).Scan(&partnerID)
	if err != nil {
		t.Fatalf("fixture partner insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM data_partners WHERE slug = 'itest-cockpit-p2'`)
	})
	var datasetID string
	err = db.QueryRowContext(ctx, `
		INSERT INTO partner_datasets (partner_id, name, slug, vertical)
		VALUES ($1::uuid, 'itest-cockpit-feed', 'itest-cockpit-feed', 'refi_heloc')
		ON CONFLICT (partner_id, slug) DO UPDATE SET updated_at = NOW()
		RETURNING id::text`, partnerID).Scan(&datasetID)
	if err != nil {
		t.Fatalf("fixture dataset insert: %v", err)
	}

	// WRITE through the one writer — exactly what the cockpit UI submits.
	h := NewPartnerAdminHandler(db)
	body, _ := json.Marshal(map[string]interface{}{
		"overrides": []map[string]interface{}{
			{"isp": "gmail", "pct_override": 0.4, "max_per_wave": 1000, "daily_cap": 500},
			{"isp": "YAHOO", "pct_override": 0.3},
		},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut,
		"/api/mailing/data-partners/datasets/"+datasetID+"/isp-distribution",
		bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", datasetID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	h.HandleUpdateISPDistribution(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("write got %d: %s", rec.Code, rec.Body.String())
	}

	// READ 1 — the orchestrator's per-wave cap overlay shape.
	maxByISP := map[string]int{}
	rows, err := db.QueryContext(ctx, `
		SELECT LOWER(TRIM(isp)) AS isp, max_per_wave
		FROM partner_isp_distribution_overrides
		WHERE dataset_id = $1::uuid
		  AND max_per_wave IS NOT NULL
		  AND max_per_wave > 0`, datasetID)
	if err != nil {
		t.Fatalf("orchestrator max_per_wave read: %v", err)
	}
	for rows.Next() {
		var isp string
		var mpw int
		if err := rows.Scan(&isp, &mpw); err != nil {
			t.Fatal(err)
		}
		maxByISP[isp] = mpw
	}
	rows.Close()
	if maxByISP["gmail"] != 1000 {
		t.Fatalf("orchestrator cap read must see gmail=1000, got %v", maxByISP)
	}
	if _, ok := maxByISP["yahoo"]; ok {
		t.Fatalf("yahoo carried no max_per_wave — must be absent from the cap read: %v", maxByISP)
	}

	// READ 2 — the orchestrator's lane daily-budget shape.
	dailyByISP := map[string]int{}
	rows, err = db.QueryContext(ctx, `
		SELECT LOWER(TRIM(isp)) AS isp, daily_cap
		FROM partner_isp_distribution_overrides
		WHERE dataset_id = $1::uuid AND daily_cap IS NOT NULL`, datasetID)
	if err != nil {
		t.Fatalf("orchestrator daily_cap read: %v", err)
	}
	for rows.Next() {
		var isp string
		var cap int
		if err := rows.Scan(&isp, &cap); err != nil {
			t.Fatal(err)
		}
		dailyByISP[isp] = cap
	}
	rows.Close()
	if dailyByISP["gmail"] != 500 {
		t.Fatalf("orchestrator daily read must see gmail=500, got %v", dailyByISP)
	}
	if _, ok := dailyByISP["yahoo"]; ok {
		t.Fatalf("yahoo carried no daily_cap — must be absent from the daily read: %v", dailyByISP)
	}

	// The cockpit's throttle read reports the same rows (audit columns too).
	var updatedBy sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT updated_by FROM partner_isp_distribution_overrides
		WHERE dataset_id = $1::uuid AND isp = 'gmail'`, datasetID).Scan(&updatedBy); err != nil {
		t.Fatalf("updated_by read: %v", err)
	}
	if !updatedBy.Valid || updatedBy.String == "" {
		t.Fatal("write must stamp updated_by (actor)")
	}
}
