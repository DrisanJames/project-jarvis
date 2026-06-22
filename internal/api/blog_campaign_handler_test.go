package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Layer 1: Pure logic tests for buildBlogCampaignInput
// ---------------------------------------------------------------------------

func TestBuildBlogInput_PerBrandConfig(t *testing.T) {
	tests := []struct {
		name          string
		sendingDomain string
		wantFromName  string
		wantBrand     string
		wantListCount int
		wantSegCount  int
	}{
		{"DiscountBlog", "em.discountblog.com", "Jamie @ Discount Blog", "Discount Blog", 3, 2},
		{"QuizFiesta", "em.quizfiesta.com", "Quiz Master", "Quiz Fiesta", 3, 2},
		{"HistoryThinking", "em.historythinking.com", "History Thinking", "History Thinking", 3, 2},
		{"MyOwnHealth", "em.myownhealth.net", "Arnold @ My Own Health", "My Own Health", 3, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := BlogCampaignInput{
				SendingDomain: tt.sendingDomain,
				Subject:       "Test subject",
				PreviewText:   "Test preview",
				HTMLContent:   "<html>test</html>",
			}
			result, err := buildBlogCampaignInput(input)
			require.NoError(t, err)

			assert.Equal(t, tt.wantFromName, result.Variants[0].FromName)
			assert.Contains(t, result.Name, tt.wantBrand)
			assert.Contains(t, result.Name, "Engaged Audience")
			assert.Len(t, result.InclusionLists, tt.wantListCount, "seed list count")
			assert.Len(t, result.InclusionSegments, tt.wantSegCount, "segment count")
			assert.Len(t, result.TargetISPs, 8, "all 8 ISPs")
			assert.Empty(t, result.ISPQuotas, "quotas must be empty for unlimited")
			assert.Empty(t, result.ISPPlans, "ISP plans must be empty for legacy path")
			assert.Contains(t, result.ExclusionLists, "global-suppression-list")
			assert.Equal(t, "scheduled", result.SendMode)
			assert.Equal(t, "America/Boise", result.Timezone)
			assert.Equal(t, "auto", result.ThrottleStrategy)
			assert.Equal(t, float64(100), result.Variants[0].SplitPercent)
		})
	}
}

func TestBuildBlogInput_UnknownDomain(t *testing.T) {
	input := BlogCampaignInput{
		SendingDomain: "em.unknown.com",
		Subject:       "Test",
		HTMLContent:   "<html>test</html>",
	}
	_, err := buildBlogCampaignInput(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown sending_domain")
}

func TestBuildBlogInput_MissingSubject(t *testing.T) {
	input := BlogCampaignInput{
		SendingDomain: "em.discountblog.com",
		Subject:       "",
		HTMLContent:   "<html>test</html>",
	}
	_, err := buildBlogCampaignInput(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "subject is required")
}

func TestBuildBlogInput_MissingHTML(t *testing.T) {
	input := BlogCampaignInput{
		SendingDomain: "em.discountblog.com",
		Subject:       "Test",
		HTMLContent:   "",
	}
	_, err := buildBlogCampaignInput(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "html_content is required")
}

func TestBuildBlogInput_DefaultSchedule(t *testing.T) {
	input := BlogCampaignInput{
		SendingDomain: "em.discountblog.com",
		Subject:       "Test",
		HTMLContent:   "<html>test</html>",
	}
	result, err := buildBlogCampaignInput(input)
	require.NoError(t, err)
	require.NotNil(t, result.ScheduledAt)

	mountain, _ := time.LoadLocation("America/Boise")
	inMT := result.ScheduledAt.In(mountain)
	assert.Equal(t, 8, inMT.Hour(), "default schedule should be 8 AM Mountain")
}

func TestBuildBlogInput_CustomSchedule(t *testing.T) {
	custom := time.Date(2026, 4, 1, 14, 0, 0, 0, time.UTC)
	input := BlogCampaignInput{
		SendingDomain: "em.discountblog.com",
		Subject:       "Test",
		HTMLContent:   "<html>test</html>",
		ScheduledAt:   &custom,
	}
	result, err := buildBlogCampaignInput(input)
	require.NoError(t, err)
	assert.Equal(t, custom, *result.ScheduledAt)
}

func TestBuildBlogInput_CampaignNameFormat(t *testing.T) {
	scheduled := time.Date(2026, 3, 26, 14, 0, 0, 0, time.UTC)
	input := BlogCampaignInput{
		SendingDomain: "em.discountblog.com",
		Subject:       "Test",
		HTMLContent:   "<html>test</html>",
		ScheduledAt:   &scheduled,
	}
	result, err := buildBlogCampaignInput(input)
	require.NoError(t, err)
	assert.Equal(t, "03262026 - Discount Blog - Engaged Audience", result.Name)
}

// ---------------------------------------------------------------------------
// Layer 2: HTTP handler tests (sqlmock + httptest)
// ---------------------------------------------------------------------------

func TestHandleBlogCampaign_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)

	mock.MatchExpectationsInOrder(false)

	// Reservation phase: resolve identity (no draft) -> INSERT finalizing_audience -> COMMIT
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id\s+FROM mailing_campaigns`).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(`INSERT INTO mailing_campaigns`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	// Post-commit durability verification (false-success guard)
	mock.ExpectQuery(`SELECT id::text FROM mailing_campaigns`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ok"))

	scheduled := time.Now().UTC().Add(20 * time.Minute).Round(time.Minute)
	body, _ := json.Marshal(BlogCampaignInput{
		SendingDomain: "em.discountblog.com",
		Subject:       "Test subject",
		PreviewText:   "Test preview",
		HTMLContent:   "<html><body>Test</body></html>",
		ScheduledAt:   &scheduled,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/blog-campaign", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", defaultOrgID)
	rr := httptest.NewRecorder()

	service.HandleBlogCampaign(rr, req)

	require.Equal(t, http.StatusAccepted, rr.Code, "body: %s", rr.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp["campaign_id"])
	assert.Contains(t, resp["name"].(string), "Discount Blog")
	assert.Contains(t, resp["name"].(string), "Engaged Audience")
	assert.Equal(t, "finalizing_audience", resp["status"])
}

func TestHandleBlogCampaign_UnknownDomain(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	body, _ := json.Marshal(BlogCampaignInput{
		SendingDomain: "em.fake.com",
		Subject:       "Test",
		HTMLContent:   "<html>test</html>",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/blog-campaign", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	service.HandleBlogCampaign(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "unknown sending_domain")
}

func TestHandleBlogCampaign_MissingSubject(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	body, _ := json.Marshal(BlogCampaignInput{
		SendingDomain: "em.discountblog.com",
		Subject:       "",
		HTMLContent:   "<html>test</html>",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/blog-campaign", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	service.HandleBlogCampaign(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "subject is required")
}

func TestHandleBlogCampaign_MissingHTML(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	body, _ := json.Marshal(BlogCampaignInput{
		SendingDomain: "em.discountblog.com",
		Subject:       "Test",
		HTMLContent:   "",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/blog-campaign", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	service.HandleBlogCampaign(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "html_content is required")
}

func TestHandleBlogCampaign_InvalidJSON(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	service := newTestPMTAService(db, defaultOrgID)
	req := httptest.NewRequest(http.MethodPost, "/api/mailing/blog-campaign", bytes.NewReader([]byte("{garbage")))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	service.HandleBlogCampaign(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "invalid JSON")
}
