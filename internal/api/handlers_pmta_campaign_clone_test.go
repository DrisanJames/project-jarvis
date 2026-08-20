package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test 1: Clone candidates are ordered by recency, recommended badge preserved
// ---------------------------------------------------------------------------

func TestHandleCloneCandidates_RecencyOrder(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	svc := newTestPMTAService(db, defaultOrgID)

	now := time.Now().UTC()
	cols := []string{
		"id", "name", "status",
		"sent_count", "open_count", "click_count", "bounce_count",
		"hard_bounce_count", "soft_bounce_count", "complaint_count",
		"campaign_date",
		"open_rate", "click_rate", "bounce_rate",
		"hard_bounce_rate", "soft_bounce_rate", "complaint_rate",
		"has_config",
	}
	rows := sqlmock.NewRows(cols).
		// Most recent campaign (low open rate)
		AddRow("c-newest", "Newest Campaign", "completed",
			1000, 50, 10, 5, 3, 2, 0,
			now.Add(-1*time.Hour),
			0.05, 0.01, 0.005, 0.003, 0.002, 0.0,
			true).
		// Older campaign (high open rate — would be first under old sort)
		AddRow("c-best", "Best Performer", "completed",
			5000, 2500, 500, 10, 5, 5, 0,
			now.Add(-48*time.Hour),
			0.50, 0.10, 0.002, 0.001, 0.001, 0.0,
			true).
		// Oldest campaign
		AddRow("c-oldest", "Oldest Campaign", "sent",
			2000, 200, 40, 20, 10, 10, 1,
			now.Add(-720*time.Hour),
			0.10, 0.02, 0.01, 0.005, 0.005, 0.0005,
			false)

	mock.ExpectQuery("SELECT c.id::text, c.name, c.status").WillReturnRows(rows)

	req := httptest.NewRequest(http.MethodGet, "/api/mailing/pmta-campaign/clone-candidates", nil)
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	svc.HandleCloneCandidates(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp struct {
		Campaigns []struct {
			ID          string  `json:"id"`
			Name        string  `json:"name"`
			OpenRate    float64 `json:"open_rate"`
			Recommended bool    `json:"recommended"`
		} `json:"campaigns"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Campaigns, 3)

	assert.Equal(t, "c-newest", resp.Campaigns[0].ID, "most recent should be first")
	assert.Equal(t, "c-best", resp.Campaigns[1].ID, "second most recent should be second")
	assert.Equal(t, "c-oldest", resp.Campaigns[2].ID, "oldest should be last")

	// Recommended badge should be on the best performer regardless of sort order
	var recommendedID string
	for _, c := range resp.Campaigns {
		if c.Recommended {
			recommendedID = c.ID
		}
	}
	assert.Equal(t, "c-best", recommendedID, "recommended badge should be on best performer")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// Test 2: Clone from campaign with full pmta_config preserves all fields
// ---------------------------------------------------------------------------

func TestHandleCloneData_FullPMTAConfig(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	svc := newTestPMTAService(db, defaultOrgID)

	campaignInput := map[string]interface{}{
		"campaign_id":        "original-id",
		"name":               "Original Campaign",
		"target_isps":        []string{"gmail", "yahoo"},
		"sending_domain":     "em.example.com",
		"inclusion_lists":    []string{"list-uuid-1", "list-uuid-2"},
		"inclusion_segments": []string{"seg-uuid-1"},
		"exclusion_lists":    []string{"supp-uuid-1"},
		"exclusion_segments": []string{"supp-seg-1"},
		"send_priority":      []map[string]string{{"id": "list-uuid-1", "type": "list"}, {"id": "seg-uuid-1", "type": "segment"}},
		"randomize_audience": true,
		"send_mode":          "scheduled",
		"variants": []map[string]interface{}{
			{"variant_name": "A", "from_name": "Sender A", "subject": "Subject A", "preview_text": "Preview A", "html_content": "<h1>A</h1>", "split_percent": 60.0},
			{"variant_name": "B", "from_name": "Sender B", "subject": "Subject B", "preview_text": "Preview B", "html_content": "<h1>B</h1>", "split_percent": 40.0},
		},
		"isp_quotas": []map[string]interface{}{
			{"isp": "gmail", "volume": 5000},
			{"isp": "yahoo", "volume": 3000},
		},
	}
	configBlob, _ := json.Marshal(map[string]interface{}{
		"campaign_input": campaignInput,
		"schedule_mode":  "per-isp",
	})

	mock.ExpectQuery("SELECT name, status").
		WillReturnRows(sqlmock.NewRows([]string{"name", "status", "pmta_config", "completed_at"}).
			AddRow("Original Campaign", "completed", string(configBlob), time.Now().UTC()))

	// enrichCloneInput will query for sending_domain (already present, so skipped),
	// then lists (already present, so skipped), then randomize (key exists, so skipped),
	// then variants (present with content, so skipped).
	// No additional queries expected.

	req := makeCloneDataRequest(t, "some-campaign-id")
	rr := httptest.NewRecorder()
	svc.HandleCloneData(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "", resp["campaign_id"], "campaign_id should be empty for clone")
	assert.Equal(t, "Original Campaign (Clone)", resp["name"])
	assert.Equal(t, "draft", resp["status"])
	assert.Equal(t, "per-isp", resp["schedule_mode"])
	assert.Equal(t, "some-campaign-id", resp["source_id"])

	inputRaw, _ := json.Marshal(resp["campaign_input"])
	var input map[string]interface{}
	require.NoError(t, json.Unmarshal(inputRaw, &input))

	assert.Equal(t, "Original Campaign (Clone)", input["name"])
	assert.Nil(t, input["campaign_id"], "campaign_id should be stripped")
	assert.Equal(t, "em.example.com", input["sending_domain"])
	assert.Equal(t, true, input["randomize_audience"])
	assert.Equal(t, "scheduled", input["send_mode"])

	variants, _ := input["variants"].([]interface{})
	require.Len(t, variants, 2, "both A/B variants should be preserved")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// Test 3: Clone from campaign without pmta_config — full fallback
// ---------------------------------------------------------------------------

func TestHandleCloneData_NoPMTAConfig_FullReconstruction(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	svc := newTestPMTAService(db, defaultOrgID)
	schedAt := time.Now().UTC().Add(2 * time.Hour)

	// Initial query returns empty pmta_config
	mock.ExpectQuery("SELECT name, status").
		WillReturnRows(sqlmock.NewRows([]string{"name", "status", "pmta_config", "completed_at"}).
			AddRow("No Config Campaign", "completed", "{}", time.Now().UTC()))

	// buildCloneInputFromDB: base campaign query
	mock.ExpectQuery("SELECT COALESCE\\(subject").
		WillReturnRows(sqlmock.NewRows(
			[]string{"subject", "from_name", "from_email", "html_content", "preview_text",
				"list_ids", "suppression_list_ids", "suppression_segment_ids", "scheduled_at"}).
			AddRow("Test Subject", "Test Sender", "test@em.example.com", "<h1>Test</h1>", "Preview",
				`["list-1","list-2"]`, `["supp-1"]`, `["supp-seg-1"]`, schedAt))

	// buildCloneInputFromDB: ISP plans
	mock.ExpectQuery("SELECT isp, quota").
		WillReturnRows(sqlmock.NewRows(
			[]string{"isp", "quota", "throttle_strategy", "timezone", "sending_domain", "randomize_audience"}).
			AddRow("gmail", 5000, "auto", "UTC", "em.example.com", true).
			AddRow("yahoo", 3000, "auto", "America/New_York", "em.example.com", true))

	// buildCloneInputFromDB: AB variants
	mock.ExpectQuery("SELECT v.variant_name").
		WillReturnRows(sqlmock.NewRows(
			[]string{"variant_name", "from_name", "subject", "preheader", "html_content", "split_percent"}).
			AddRow("A", "Sender A", "Subject A", "Preview A", "<h1>A</h1>", 60).
			AddRow("B", "Sender B", "Subject B", "Preview B", "<h1>B</h1>", 40))

	// enrichCloneInput: sending_domain present → skip
	// enrichCloneInput: lists present → skip
	// enrichCloneInput: randomize_audience key exists → skip
	// enrichCloneInput: variants present → skip

	req := makeCloneDataRequest(t, "fallback-campaign")
	rr := httptest.NewRecorder()
	svc.HandleCloneData(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "quick", resp["schedule_mode"])

	inputRaw, _ := json.Marshal(resp["campaign_input"])
	var input map[string]interface{}
	require.NoError(t, json.Unmarshal(inputRaw, &input))

	assert.Equal(t, "em.example.com", input["sending_domain"])
	assert.Equal(t, true, input["randomize_audience"])
	assert.Equal(t, "scheduled", input["send_mode"])

	incLists, _ := input["inclusion_lists"].([]interface{})
	assert.Len(t, incLists, 2)
	exclLists, _ := input["exclusion_lists"].([]interface{})
	assert.Len(t, exclLists, 1)
	exclSegs, _ := input["exclusion_segments"].([]interface{})
	assert.Len(t, exclSegs, 1)

	isps, _ := input["target_isps"].([]interface{})
	assert.Len(t, isps, 2)

	variants, _ := input["variants"].([]interface{})
	require.Len(t, variants, 2)
	v0, _ := variants[0].(map[string]interface{})
	assert.Equal(t, "A", v0["variant_name"])
	assert.Equal(t, "Preview A", v0["preview_text"], "preheader should map to preview_text")
	assert.Equal(t, float64(60), v0["split_percent"])
	v1, _ := variants[1].(map[string]interface{})
	assert.Equal(t, "B", v1["variant_name"])
	assert.Equal(t, float64(40), v1["split_percent"])

	assert.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// Test 4: Multi-variant A/B ordering and split percent
// ---------------------------------------------------------------------------

func TestHandleCloneData_MultiVariantAB(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	svc := newTestPMTAService(db, defaultOrgID)

	// Campaign with empty pmta_config triggers fallback
	mock.ExpectQuery("SELECT name, status").
		WillReturnRows(sqlmock.NewRows([]string{"name", "status", "pmta_config", "completed_at"}).
			AddRow("AB Campaign", "completed", "{}", time.Now().UTC()))

	// Base campaign
	mock.ExpectQuery("SELECT COALESCE\\(subject").
		WillReturnRows(sqlmock.NewRows(
			[]string{"subject", "from_name", "from_email", "html_content", "preview_text",
				"list_ids", "suppression_list_ids", "suppression_segment_ids", "scheduled_at"}).
			AddRow("Base Subject", "Base Sender", "base@em.test.com", "<h1>Base</h1>", "base preview",
				`["list-1"]`, `[]`, `[]`, nil))

	// ISP plans
	mock.ExpectQuery("SELECT isp, quota").
		WillReturnRows(sqlmock.NewRows(
			[]string{"isp", "quota", "throttle_strategy", "timezone", "sending_domain", "randomize_audience"}).
			AddRow("gmail", 3000, "auto", "UTC", "em.test.com", false))

	// 3 AB variants
	mock.ExpectQuery("SELECT v.variant_name").
		WillReturnRows(sqlmock.NewRows(
			[]string{"variant_name", "from_name", "subject", "preheader", "html_content", "split_percent"}).
			AddRow("A", "From A", "Subj A", "Prev A", "<h1>A</h1>", 34).
			AddRow("B", "From B", "Subj B", "Prev B", "<h1>B</h1>", 33).
			AddRow("C", "From C", "Subj C", "Prev C", "<h1>C</h1>", 33))

	// enrichCloneInput: exclusion_lists and exclusion_segments are empty → lists query
	mock.ExpectQuery("SELECT COALESCE\\(list_ids").
		WillReturnRows(sqlmock.NewRows(
			[]string{"list_ids", "suppression_list_ids", "suppression_segment_ids", "scheduled_at"}).
			AddRow(`["list-1"]`, `[]`, `[]`, nil))

	req := makeCloneDataRequest(t, "ab-campaign")
	rr := httptest.NewRecorder()
	svc.HandleCloneData(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))

	inputRaw, _ := json.Marshal(resp["campaign_input"])
	var input map[string]interface{}
	require.NoError(t, json.Unmarshal(inputRaw, &input))

	variants, _ := input["variants"].([]interface{})
	require.Len(t, variants, 3, "all 3 variants should be returned")

	names := make([]string, len(variants))
	splits := make([]float64, len(variants))
	previews := make([]string, len(variants))
	for i, v := range variants {
		vm := v.(map[string]interface{})
		names[i], _ = vm["variant_name"].(string)
		splits[i], _ = vm["split_percent"].(float64)
		previews[i], _ = vm["preview_text"].(string)
	}
	assert.Equal(t, []string{"A", "B", "C"}, names, "variants should be alphabetically ordered")
	assert.Equal(t, []float64{34, 33, 33}, splits, "split percents should be preserved")
	assert.Equal(t, []string{"Prev A", "Prev B", "Prev C"}, previews, "preheader should map to preview_text")

	assert.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// Test 5: Clone with inclusion/exclusion lists and segments
// ---------------------------------------------------------------------------

func TestHandleCloneData_ListsAndSegments(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	svc := newTestPMTAService(db, defaultOrgID)

	// Campaign with empty pmta_config
	mock.ExpectQuery("SELECT name, status").
		WillReturnRows(sqlmock.NewRows([]string{"name", "status", "pmta_config", "completed_at"}).
			AddRow("Lists Campaign", "completed", "{}", time.Now().UTC()))

	// Base campaign with all list/segment columns populated
	mock.ExpectQuery("SELECT COALESCE\\(subject").
		WillReturnRows(sqlmock.NewRows(
			[]string{"subject", "from_name", "from_email", "html_content", "preview_text",
				"list_ids", "suppression_list_ids", "suppression_segment_ids", "scheduled_at"}).
			AddRow("Subj", "Sender", "s@em.test.com", "<h1>Hi</h1>", "prev",
				`["inc-list-1","inc-list-2","inc-list-3"]`,
				`["exc-list-1","exc-list-2"]`,
				`["exc-seg-1","exc-seg-2","exc-seg-3"]`,
				nil))

	// ISP plans
	mock.ExpectQuery("SELECT isp, quota").
		WillReturnRows(sqlmock.NewRows(
			[]string{"isp", "quota", "throttle_strategy", "timezone", "sending_domain", "randomize_audience"}).
			AddRow("gmail", 1000, "auto", "UTC", "em.test.com", false))

	// No AB variants
	mock.ExpectQuery("SELECT v.variant_name").
		WillReturnRows(sqlmock.NewRows(
			[]string{"variant_name", "from_name", "subject", "preheader", "html_content", "split_percent"}))

	req := makeCloneDataRequest(t, "lists-campaign")
	rr := httptest.NewRecorder()
	svc.HandleCloneData(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))

	inputRaw, _ := json.Marshal(resp["campaign_input"])
	var input map[string]interface{}
	require.NoError(t, json.Unmarshal(inputRaw, &input))

	incLists, _ := input["inclusion_lists"].([]interface{})
	assert.Equal(t, 3, len(incLists), "inclusion_lists should map from list_ids")

	exclLists, _ := input["exclusion_lists"].([]interface{})
	assert.Equal(t, 2, len(exclLists), "exclusion_lists should map from suppression_list_ids")

	exclSegs, _ := input["exclusion_segments"].([]interface{})
	assert.Equal(t, 3, len(exclSegs), "exclusion_segments should map from suppression_segment_ids")

	incSegs, _ := input["inclusion_segments"].([]interface{})
	assert.Equal(t, 0, len(incSegs), "inclusion_segments not recoverable, should be empty")

	assert.Equal(t, "immediate", input["send_mode"], "scheduled_at is nil, so send_mode = immediate")

	assert.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// Test 6: Clone with stale pmta_config (missing newer keys) — enrichment
// ---------------------------------------------------------------------------

func TestHandleCloneData_StalePMTAConfig_Enrichment(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	svc := newTestPMTAService(db, defaultOrgID)
	schedAt := time.Now().UTC().Add(1 * time.Hour)

	// Stale config: has sending_domain + target_isps + variants but is missing
	// send_mode, inclusion_lists, randomize_audience key entirely
	staleInput := map[string]interface{}{
		"name":           "Stale Campaign",
		"target_isps":    []string{"gmail"},
		"sending_domain": "em.stale.com",
		"variants": []map[string]interface{}{
			{"variant_name": "A", "from_name": "Sender", "subject": "Subj", "preview_text": "Prev", "html_content": "<h1>Hi</h1>", "split_percent": 100.0},
		},
	}
	configBlob, _ := json.Marshal(map[string]interface{}{
		"campaign_input": staleInput,
	})

	mock.ExpectQuery("SELECT name, status").
		WillReturnRows(sqlmock.NewRows([]string{"name", "status", "pmta_config", "completed_at"}).
			AddRow("Stale Campaign", "completed", string(configBlob), time.Now().UTC()))

	// enrichCloneInput: sending_domain is present → skip domain query
	// enrichCloneInput: inclusion_lists is nil → needs lists query
	// enrichCloneInput: send_mode is empty → needs lists query (combined)
	mock.ExpectQuery("SELECT COALESCE\\(list_ids").
		WillReturnRows(sqlmock.NewRows(
			[]string{"list_ids", "suppression_list_ids", "suppression_segment_ids", "scheduled_at"}).
			AddRow(`["enriched-list-1"]`, `["enriched-supp-1"]`, `["enriched-supp-seg-1"]`, schedAt))

	// enrichCloneInput: randomize_audience key absent → query ISP plans
	mock.ExpectQuery("SELECT bool_and").
		WillReturnRows(sqlmock.NewRows([]string{"bool_and"}).AddRow(true))

	// enrichCloneInput: variants present with content → skip

	req := makeCloneDataRequest(t, "stale-campaign")
	rr := httptest.NewRecorder()
	svc.HandleCloneData(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))

	inputRaw, _ := json.Marshal(resp["campaign_input"])
	var input map[string]interface{}
	require.NoError(t, json.Unmarshal(inputRaw, &input))

	assert.Equal(t, "Stale Campaign (Clone)", input["name"])
	assert.Equal(t, "em.stale.com", input["sending_domain"], "existing domain should be preserved")
	assert.Equal(t, "scheduled", input["send_mode"], "send_mode should be inferred from scheduled_at")
	assert.Equal(t, true, input["randomize_audience"], "randomize should be enriched from ISP plans")

	incLists, _ := input["inclusion_lists"].([]interface{})
	require.Len(t, incLists, 1)
	assert.Equal(t, "enriched-list-1", incLists[0])

	exclLists, _ := input["exclusion_lists"].([]interface{})
	require.Len(t, exclLists, 1)
	assert.Equal(t, "enriched-supp-1", exclLists[0])

	exclSegs, _ := input["exclusion_segments"].([]interface{})
	require.Len(t, exclSegs, 1)
	assert.Equal(t, "enriched-supp-seg-1", exclSegs[0])

	assert.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// Test 7: Clone with no ISP plans or AB rows — graceful degradation
// ---------------------------------------------------------------------------

func TestHandleCloneData_NoISPPlans_NoABRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	svc := newTestPMTAService(db, defaultOrgID)

	// Empty pmta_config
	mock.ExpectQuery("SELECT name, status").
		WillReturnRows(sqlmock.NewRows([]string{"name", "status", "pmta_config", "completed_at"}).
			AddRow("Bare Campaign", "draft", "{}", nil))

	// Base campaign with from_email but no lists or scheduled_at
	mock.ExpectQuery("SELECT COALESCE\\(subject").
		WillReturnRows(sqlmock.NewRows(
			[]string{"subject", "from_name", "from_email", "html_content", "preview_text",
				"list_ids", "suppression_list_ids", "suppression_segment_ids", "scheduled_at"}).
			AddRow("Bare Subject", "Bare Sender", "bare@fallback.com", "<p>Bare</p>", "bare prev",
				`[]`, `[]`, `[]`, nil))

	// No ISP plans
	mock.ExpectQuery("SELECT isp, quota").
		WillReturnRows(sqlmock.NewRows(
			[]string{"isp", "quota", "throttle_strategy", "timezone", "sending_domain", "randomize_audience"}))

	// No AB variants
	mock.ExpectQuery("SELECT v.variant_name").
		WillReturnRows(sqlmock.NewRows(
			[]string{"variant_name", "from_name", "subject", "preheader", "html_content", "split_percent"}))

	// enrichCloneInput: sending_domain is "fallback.com" (from from_email split) → skip
	// enrichCloneInput: all list arrays are empty → lists query
	mock.ExpectQuery("SELECT COALESCE\\(list_ids").
		WillReturnRows(sqlmock.NewRows(
			[]string{"list_ids", "suppression_list_ids", "suppression_segment_ids", "scheduled_at"}).
			AddRow(`[]`, `[]`, `[]`, nil))

	// enrichCloneInput: randomize_audience key exists (false) → skip
	// enrichCloneInput: variants have content → skip

	req := makeCloneDataRequest(t, "bare-campaign")
	rr := httptest.NewRecorder()
	svc.HandleCloneData(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "quick", resp["schedule_mode"])

	inputRaw, _ := json.Marshal(resp["campaign_input"])
	var input map[string]interface{}
	require.NoError(t, json.Unmarshal(inputRaw, &input))

	assert.Equal(t, "Bare Campaign (Clone)", input["name"])

	// sending_domain falls back to splitting from_email
	assert.Equal(t, "fallback.com", input["sending_domain"],
		"should fall back to splitting from_email when no ISP plans")

	assert.Equal(t, false, input["randomize_audience"])
	assert.Equal(t, "immediate", input["send_mode"])

	// Single variant from base columns
	variants, _ := input["variants"].([]interface{})
	require.Len(t, variants, 1)
	v0, _ := variants[0].(map[string]interface{})
	assert.Equal(t, "A", v0["variant_name"])
	assert.Equal(t, "Bare Subject", v0["subject"])
	assert.Equal(t, "bare prev", v0["preview_text"])
	assert.Equal(t, float64(100), v0["split_percent"])

	// Empty ISP arrays
	isps, _ := input["target_isps"].([]interface{})
	assert.Empty(t, isps)

	assert.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func makeCloneDataRequest(t *testing.T, campaignID string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/mailing/pmta-campaign/"+campaignID+"/clone-data", nil)
	req.Header.Set("X-Organization-ID", defaultOrgID)

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("campaignId", campaignID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	return req
}

// ---------------------------------------------------------------------------
// Apex scoping (operator 2026-08-19): the picker is reached by selecting a
// SENDING DOMAIN, but candidates must be scoped to that domain's BRAND ROOT.
// 16 of 28 apexes send from more than one domain (em.* and m.*), so an
// exact-domain filter would hide most of a brand's own history.
// ---------------------------------------------------------------------------

func cloneCandidateRows() *sqlmock.Rows {
	cols := []string{
		"id", "name", "status", "sent_count", "open_count", "click_count",
		"bounce_count", "hard_bounce_count", "soft_bounce_count", "complaint_count",
		"campaign_date", "open_rate", "click_rate", "bounce_rate",
		"hard_bounce_rate", "soft_bounce_rate", "complaint_rate", "has_config",
	}
	return sqlmock.NewRows(cols).AddRow(
		"c-1", "08192026 - HT - Globe", "sent", 100, 10, 2, 1, 1, 0, 0,
		time.Now().UTC(), 0.10, 0.02, 0.01, 0.01, 0.0, 0.0, false)
}

func TestHandleCloneCandidates_ApexScoped_MatchesBothSendingDomains(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	svc := newTestPMTAService(db, defaultOrgID)

	// em.historythinking.com must resolve to the APEX historythinking.com, and the
	// apex must be the bound parameter — that is what lets m.historythinking.com
	// campaigns appear alongside em.* ones.
	mock.ExpectQuery("JOIN mailing_sending_profiles").
		WithArgs(defaultOrgID, "historythinking.com").
		WillReturnRows(cloneCandidateRows())

	req := httptest.NewRequest(http.MethodGet,
		"/api/mailing/pmta-campaign/clone-candidates?domain=em.historythinking.com", nil)
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	svc.HandleCloneCandidates(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp struct {
		Campaigns []struct{ ID string `json:"id"` } `json:"campaigns"`
		Apex      string                            `json:"apex"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "historythinking.com", resp.Apex,
		"response must echo the apex it scoped to, not the sending domain")
	require.Len(t, resp.Campaigns, 1)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleCloneCandidates_NoDomain_StaysOrgWide(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	svc := newTestPMTAService(db, defaultOrgID)

	// NEGATIVE PATH: with no domain the picker must NOT join profiles and must
	// bind only the org — i.e. the pre-existing org-wide behaviour is preserved.
	mock.ExpectQuery("FROM mailing_campaigns c").
		WithArgs(defaultOrgID).
		WillReturnRows(cloneCandidateRows())

	req := httptest.NewRequest(http.MethodGet, "/api/mailing/pmta-campaign/clone-candidates", nil)
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	svc.HandleCloneCandidates(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp struct {
		Apex string `json:"apex"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Empty(t, resp.Apex, "unfiltered list must not claim an apex")
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleCloneCandidates_BroadcastOnly_AndLimit10(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	svc := newTestPMTAService(db, defaultOrgID)

	// Operator 2026-08-19: last 10 only, broadcast only. Assert the SQL carries a
	// POSITIVE campaign_type allowlist — the previous "<> 'click_drip'" form let a
	// journey_node campaign through, and would let any future type through too.
	mock.ExpectQuery("c.campaign_type = 'regular'").
		WithArgs(defaultOrgID).
		WillReturnRows(cloneCandidateRows())

	req := httptest.NewRequest(http.MethodGet, "/api/mailing/pmta-campaign/clone-candidates", nil)
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()
	svc.HandleCloneCandidates(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestHandleCloneCandidates_LimitIsTen(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	svc := newTestPMTAService(db, defaultOrgID)

	mock.ExpectQuery("LIMIT 10").
		WithArgs(defaultOrgID, "discountblog.com").
		WillReturnRows(cloneCandidateRows())

	req := httptest.NewRequest(http.MethodGet,
		"/api/mailing/pmta-campaign/clone-candidates?domain=m.discountblog.com", nil)
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()
	svc.HandleCloneCandidates(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.NoError(t, mock.ExpectationsWereMet())
}
