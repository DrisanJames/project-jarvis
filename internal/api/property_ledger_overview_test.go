package api

// Estate-overview + lane-label fixtures. What each test PINS:
//   - the overview NEVER runs a full pcq scan: every count query it issues is
//     dataset_id-parameterized (the cached per-feed anatomy path) — asserted
//     by the expected statements themselves;
//   - clean/dirty math: clean = ready + mailed_lifetime, dirty = suppressed +
//     dead_letter, at both totals and lane grain;
//   - lanes group by vertical (2 feeds on one vertical = 1 lane row, feeds=2)
//     and carry the operator display_name when a label row exists;
//   - the 300s whole-overview cache: the second request issues ZERO SQL;
//   - a missing partner_drip_lane_labels table degrades to empty labels —
//     the overview still answers 200;
//   - lane-label writes ride the ROSTER env gate (403 + zero SQL when unset),
//     validate the vertical slug + 80-char cap, and an empty display_name
//     DELETES the row;
//   - the overview + lane-label routes are static siblings of the mounted
//     /pmta-campaign subrouter and BOTH resolve (chi backtracking).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

// overviewExpectIndexOK queues the one-time index-gate probe.
func overviewExpectIndexOK(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`indisvalid`).
		WillReturnRows(sqlmock.NewRows([]string{"ok"}).AddRow(true))
}

func overviewAnatomyRows(datasetID string, tranche, cleaning, pendingEO, eoInFlight,
	ready, held, suppressed, deadLetter, mailedLifetime, mailedToday int64) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"dataset_id", "tranche_total", "cleaning", "pending_eo", "eo_in_flight",
		"ready_total", "held", "suppressed", "dead_letter", "mailed_lifetime", "mailed_today",
	}).AddRow(datasetID, tranche, cleaning, pendingEO, eoInFlight, ready, held,
		suppressed, deadLetter, mailedLifetime, mailedToday)
}

type overviewResp struct {
	Totals map[string]int64 `json:"totals"`
	ByISP  []struct {
		ISP         string `json:"isp"`
		Ready       int64  `json:"ready"`
		MailedToday int64  `json:"mailed_today"`
	} `json:"by_isp"`
	ByLane     []propertyOverviewLane `json:"by_lane"`
	ComputedAt time.Time              `json:"computed_at"`
}

func TestPropertyLedgerOverview_AggregatesAndCaches(t *testing.T) {
	invalidatePropertyOverviewCache()
	t.Cleanup(invalidatePropertyOverviewCache)
	s, mock := newLedgerServiceWithMock(t)

	overviewExpectIndexOK(mock)
	// Feed identities: two feeds on ONE vertical + one on another.
	mock.ExpectQuery(`FROM partner_datasets`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "vertical"}).
			AddRow("d-1", "Feed One", "consumer").
			AddRow("d-2", "Feed Two", "consumer").
			AddRow("d-3", "Feed Three", "term_life"))
	// Operator labels.
	mock.ExpectQuery(`FROM partner_drip_lane_labels`).
		WillReturnRows(sqlmock.NewRows([]string{"vertical", "display_name"}).
			AddRow("consumer", "Consumer Remarketing"))

	// d-1: 100 ready, 10 cleaning (7 pending / 3 in-flight), 5 held,
	// 20 suppressed, 2 dead, 500 mailed lifetime, 50 today.
	mock.ExpectQuery(`AS tranche_total`).WithArgs("d-1", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(overviewAnatomyRows("d-1", 637, 10, 7, 3, 100, 5, 20, 2, 500, 50))
	mock.ExpectQuery(`GROUP BY isp_family`).WithArgs("d-1").
		WillReturnRows(sqlmock.NewRows([]string{"isp_family", "n"}).
			AddRow("gmail", 60).AddRow("yahoo", 40))
	mock.ExpectQuery(`status = 'mailed'`).WithArgs("d-1", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "n"}).AddRow("gmail", 50))

	// d-2: all zero except 30 ready.
	mock.ExpectQuery(`AS tranche_total`).WithArgs("d-2", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(overviewAnatomyRows("d-2", 30, 0, 0, 0, 30, 0, 0, 0, 0, 0))
	mock.ExpectQuery(`GROUP BY isp_family`).WithArgs("d-2").
		WillReturnRows(sqlmock.NewRows([]string{"isp_family", "n"}).AddRow("gmail", 30))
	mock.ExpectQuery(`status = 'mailed'`).WithArgs("d-2", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "n"}))

	// d-3 (term_life): 10 ready, 4 suppressed, 1 dead, 100 lifetime, 5 today.
	mock.ExpectQuery(`AS tranche_total`).WithArgs("d-3", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(overviewAnatomyRows("d-3", 120, 0, 0, 0, 10, 0, 4, 1, 100, 5))
	mock.ExpectQuery(`GROUP BY isp_family`).WithArgs("d-3").
		WillReturnRows(sqlmock.NewRows([]string{"isp_family", "n"}).AddRow("aol", 10))
	mock.ExpectQuery(`status = 'mailed'`).WithArgs("d-3", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "n"}).AddRow("aol", 5))

	req := httptest.NewRequest("GET", "/api/mailing/pmta-campaign/property-ledger/overview", nil)
	rec := httptest.NewRecorder()
	s.HandlePropertyLedgerOverview(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp overviewResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// Totals + the clean/dirty definitions.
	require.EqualValues(t, 140, resp.Totals["ready"])
	require.EqualValues(t, 10, resp.Totals["cleaning"])
	require.EqualValues(t, 7, resp.Totals["pending_eo"])
	require.EqualValues(t, 3, resp.Totals["eo_in_flight"])
	require.EqualValues(t, 5, resp.Totals["held"])
	require.EqualValues(t, 24, resp.Totals["suppressed"])
	require.EqualValues(t, 3, resp.Totals["dead_letter"])
	require.EqualValues(t, 55, resp.Totals["mailed_today"])
	require.EqualValues(t, 600, resp.Totals["mailed_lifetime"])
	require.EqualValues(t, 740, resp.Totals["clean"], "clean = ready + mailed_lifetime")
	require.EqualValues(t, 27, resp.Totals["dirty"], "dirty = suppressed + dead_letter")

	// Lanes: consumer merges 2 feeds; term_life stands alone; label applied.
	require.Len(t, resp.ByLane, 2)
	consumer := resp.ByLane[0]
	require.Equal(t, "consumer", consumer.Vertical)
	require.Equal(t, "Consumer Remarketing", consumer.DisplayName)
	require.Equal(t, 2, consumer.Feeds)
	require.EqualValues(t, 130, consumer.Ready)
	require.EqualValues(t, 630, consumer.Clean)
	require.EqualValues(t, 22, consumer.Dirty)
	termLife := resp.ByLane[1]
	require.Equal(t, "term_life", termLife.Vertical)
	require.Equal(t, "", termLife.DisplayName)
	require.EqualValues(t, 110, termLife.Clean, "ready 10 + mailed_lifetime 100")

	// ISP merge across feeds.
	byISP := map[string][2]int64{}
	for _, e := range resp.ByISP {
		byISP[e.ISP] = [2]int64{e.Ready, e.MailedToday}
	}
	require.Equal(t, [2]int64{90, 50}, byISP["gmail"])
	require.Equal(t, [2]int64{40, 0}, byISP["yahoo"])
	require.Equal(t, [2]int64{10, 5}, byISP["aol"])

	require.NoError(t, mock.ExpectationsWereMet())

	// SECOND request: served from the 300s cache — ZERO SQL (no queued
	// expectations; an unexpected query would surface as a 500 through the
	// build error path, so a 200 here proves the cache).
	rec2 := httptest.NewRecorder()
	s.HandlePropertyLedgerOverview(rec2, httptest.NewRequest("GET",
		"/api/mailing/pmta-campaign/property-ledger/overview", nil))
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	var resp2 overviewResp
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	require.Equal(t, resp.ComputedAt, resp2.ComputedAt, "same computed_at = same cached payload")
	require.NoError(t, mock.ExpectationsWereMet())
}

// A dev DB without partner_drip_lane_labels: the label read errors and
// degrades to empty labels — the overview still answers 200.
func TestPropertyLedgerOverview_MissingLabelTableDegrades(t *testing.T) {
	invalidatePropertyOverviewCache()
	t.Cleanup(invalidatePropertyOverviewCache)
	s, mock := newLedgerServiceWithMock(t)

	overviewExpectIndexOK(mock)
	mock.ExpectQuery(`FROM partner_datasets`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "vertical"}).
			AddRow("d-1", "Feed One", "consumer"))
	mock.ExpectQuery(`FROM partner_drip_lane_labels`).
		WillReturnError(fmt.Errorf(`pq: relation "partner_drip_lane_labels" does not exist`))
	mock.ExpectQuery(`AS tranche_total`).WithArgs("d-1", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(overviewAnatomyRows("d-1", 10, 0, 0, 0, 10, 0, 0, 0, 0, 0))
	mock.ExpectQuery(`GROUP BY isp_family`).WithArgs("d-1").
		WillReturnRows(sqlmock.NewRows([]string{"isp_family", "n"}))
	mock.ExpectQuery(`status = 'mailed'`).WithArgs("d-1", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"isp", "n"}))

	rec := httptest.NewRecorder()
	s.HandlePropertyLedgerOverview(rec, httptest.NewRequest("GET",
		"/api/mailing/pmta-campaign/property-ledger/overview", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp overviewResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "", resp.ByLane[0].DisplayName)
	require.NoError(t, mock.ExpectationsWereMet())
}

// The static overview/lane-label routes must coexist with the mounted
// /pmta-campaign subrouter (chi static-first + backtracking) — this pins the
// registration approach used in server_routes_mailing.go.
func TestPropertyOverviewRouteCoexistsWithMount(t *testing.T) {
	r := chi.NewRouter()
	r.Route("/pmta-campaign", func(cr chi.Router) {
		cr.Get("/property-ledger/supply", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("supply"))
		})
	})
	r.Get("/pmta-campaign/property-ledger/overview", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("overview"))
	})
	r.Put("/pmta-campaign/property-ledger/lane-label", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("label"))
	})

	for _, tc := range []struct{ method, path, want string }{
		{"GET", "/pmta-campaign/property-ledger/supply", "supply"},
		{"GET", "/pmta-campaign/property-ledger/overview", "overview"},
		{"PUT", "/pmta-campaign/property-ledger/lane-label", "label"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		require.Equal(t, tc.want, rec.Body.String(), "%s %s", tc.method, tc.path)
	}
}

// ── lane labels ─────────────────────────────────────────────────────────────

func labelPut(t *testing.T, s *PMTACampaignService, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT",
		"/api/mailing/pmta-campaign/property-ledger/lane-label", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Email", "operator@jv")
	rec := httptest.NewRecorder()
	s.HandleLaneLabelUpsert(rec, req)
	return rec
}

// Gate off (env unset): 403 with ZERO SQL — a real gate, not a late refusal.
func TestLaneLabelWriteGateBlocksWithZeroSQL(t *testing.T) {
	t.Setenv(laneRosterWriteFlagEnv, "")
	s, mock := newLedgerServiceWithMock(t)
	rec := labelPut(t, s, `{"vertical":"consumer","display_name":"Consumer"}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), laneRosterWriteFlagEnv)
	require.NoError(t, mock.ExpectationsWereMet(), "gated request must execute zero statements")
}

func TestLaneLabelUpsertValidation(t *testing.T) {
	t.Setenv(laneRosterWriteFlagEnv, "1")
	s, mock := newLedgerServiceWithMock(t)
	for name, body := range map[string]string{
		"bad slug": `{"vertical":"Consumer Loans!","display_name":"x"}`,
		"empty":    `{"vertical":"","display_name":"x"}`,
		"too long": `{"vertical":"consumer","display_name":"` + strings.Repeat("a", 81) + `"}`,
		"bad json": `{`,
	} {
		rec := labelPut(t, s, body)
		require.Equal(t, http.StatusBadRequest, rec.Code, name)
	}
	require.NoError(t, mock.ExpectationsWereMet(), "validation rejects before any DB access")
}

func TestLaneLabelUpsertAndDelete(t *testing.T) {
	t.Setenv(laneRosterWriteFlagEnv, "1")
	invalidatePropertyOverviewCache()
	t.Cleanup(invalidatePropertyOverviewCache)
	s, mock := newLedgerServiceWithMock(t)

	// Upsert (trimmed value reaches the DB).
	mock.ExpectExec(`INSERT INTO partner_drip_lane_labels`).
		WithArgs("consumer", "Consumer Remarketing", "operator@jv").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	rec := labelPut(t, s, `{"vertical":"consumer","display_name":"  Consumer Remarketing  "}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"deleted":false`)

	// Empty display_name DELETES the row.
	mock.ExpectExec(`DELETE FROM partner_drip_lane_labels`).
		WithArgs("consumer").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO partner_admin_audit_log`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	rec2 := labelPut(t, s, `{"vertical":"consumer","display_name":""}`)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	require.Contains(t, rec2.Body.String(), `"deleted":true`)

	require.NoError(t, mock.ExpectationsWereMet())
}
