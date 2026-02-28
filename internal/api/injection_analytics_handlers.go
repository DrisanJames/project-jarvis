package api

import (
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// =============================================================================
// INJECTION ANALYTICS HANDLER
// =============================================================================
// Provides comprehensive injection (send volume) analytics from mailing_isp_metrics.
// Includes daily series, ISP breakdown, moving averages, weekly aggregates,
// period-over-period comparison, and volatility metrics.

// InjectionAnalyticsHandler serves injection analytics data
type InjectionAnalyticsHandler struct {
	db *sql.DB
}

// NewInjectionAnalyticsHandler creates a new handler with DB connection
func NewInjectionAnalyticsHandler(db *sql.DB) *InjectionAnalyticsHandler {
	return &InjectionAnalyticsHandler{db: db}
}

// RegisterRoutes registers the injection analytics routes
func (h *InjectionAnalyticsHandler) RegisterRoutes(r chi.Router) {
	r.Get("/", h.HandleGetInjectionAnalytics)
}

// --- Response Types ---

type InjectionAnalyticsResponse struct {
	Period           InjectionPeriod             `json:"period"`
	Summary          InjectionSummary            `json:"summary"`
	Comparison       *InjectionComparison        `json:"comparison"`
	DailySeries      []InjectionDailyPoint       `json:"daily_series"`
	ISPBreakdown     []InjectionISPBreakdown     `json:"isp_breakdown"`
	MovingAverages   InjectionMovingAverages     `json:"moving_averages"`
	WeeklyAggregates []InjectionWeeklyAggregate  `json:"weekly_aggregates"`
	Volatility       InjectionVolatility         `json:"volatility"`
}

type InjectionPeriod struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Days  int    `json:"days"`
	Label string `json:"label"`
}

type InjectionSummary struct {
	TotalInjected   int64   `json:"total_injected"`
	TotalDelivered  int64   `json:"total_delivered"`
	TotalBounced    int64   `json:"total_bounced"`
	TotalOpened     int64   `json:"total_opened"`
	TotalClicked    int64   `json:"total_clicked"`
	TotalComplained int64   `json:"total_complained"`
	DeliveryRate    float64 `json:"delivery_rate"`
	OpenRate        float64 `json:"open_rate"`
	ClickRate       float64 `json:"click_rate"`
	BounceRate      float64 `json:"bounce_rate"`
	ComplaintRate   float64 `json:"complaint_rate"`
	AvgDailyVolume  int64   `json:"avg_daily_volume"`
	PeakDailyVolume int64   `json:"peak_daily_volume"`
	PeakDate        string  `json:"peak_date"`
	LowDailyVolume  int64   `json:"low_daily_volume"`
	LowDate         string  `json:"low_date"`
	ActiveCampaigns int     `json:"active_campaigns"`
	TotalRevenue    float64 `json:"total_revenue"`
}

type InjectionComparison struct {
	PrevTotalInjected       int64   `json:"prev_total_injected"`
	InjectedChangePct       float64 `json:"injected_change_pct"`
	PrevDeliveryRate        float64 `json:"prev_delivery_rate"`
	DeliveryRateChange      float64 `json:"delivery_rate_change"`
	PrevOpenRate            float64 `json:"prev_open_rate"`
	OpenRateChange          float64 `json:"open_rate_change"`
	PrevClickRate           float64 `json:"prev_click_rate"`
	ClickRateChange         float64 `json:"click_rate_change"`
	PrevBounceRate          float64 `json:"prev_bounce_rate"`
	BounceRateChange        float64 `json:"bounce_rate_change"`
	PrevComplaintRate       float64 `json:"prev_complaint_rate"`
	ComplaintRateChange     float64 `json:"complaint_rate_change"`
	PrevAvgDailyVolume      int64   `json:"prev_avg_daily_volume"`
	AvgDailyVolumeChangePct float64 `json:"avg_daily_volume_change_pct"`
	Trend                   string  `json:"trend"`
}

type InjectionDailyPoint struct {
	Date               string   `json:"date"`
	Injected           int64    `json:"injected"`
	Delivered          int64    `json:"delivered"`
	Bounced            int64    `json:"bounced"`
	Opened             int64    `json:"opened"`
	Clicked            int64    `json:"clicked"`
	Complained         int64    `json:"complained"`
	DeliveryRate       float64  `json:"delivery_rate"`
	OpenRate           float64  `json:"open_rate"`
	ClickRate          float64  `json:"click_rate"`
	BounceRate         float64  `json:"bounce_rate"`
	ComplaintRate      float64  `json:"complaint_rate"`
	CumulativeInjected int64   `json:"cumulative_injected"`
	Revenue            float64  `json:"revenue"`
	MA7Injected        *float64 `json:"ma7_injected"`
	MA14Injected       *float64 `json:"ma14_injected"`
	CampaignsActive    int      `json:"campaigns_active"`
}

type InjectionISPBreakdown struct {
	ISP            string  `json:"isp"`
	TotalSent      int64   `json:"total_sent"`
	Delivered      int64   `json:"delivered"`
	DeliveryRate   float64 `json:"delivery_rate"`
	OpenRate       float64 `json:"open_rate"`
	ClickRate      float64 `json:"click_rate"`
	BounceRate     float64 `json:"bounce_rate"`
	ComplaintRate  float64 `json:"complaint_rate"`
	VolumeSharePct float64 `json:"volume_share_pct"`
	Trend          string  `json:"trend"`
	TrendPct       float64 `json:"trend_pct"`
}

type InjectionMAPoint struct {
	Date  string  `json:"date"`
	Value float64 `json:"value"`
}

type InjectionMovingAverages struct {
	MA7  []InjectionMAPoint `json:"ma7"`
	MA14 []InjectionMAPoint `json:"ma14"`
	MA30 []InjectionMAPoint `json:"ma30"`
}

type InjectionWeeklyAggregate struct {
	WeekStart              string   `json:"week_start"`
	WeekEnd                string   `json:"week_end"`
	WeekLabel              string   `json:"week_label"`
	TotalInjected          int64    `json:"total_injected"`
	AvgDaily               int64    `json:"avg_daily"`
	DeliveryRate           float64  `json:"delivery_rate"`
	OpenRate               float64  `json:"open_rate"`
	ChangeFromPrevWeekPct  *float64 `json:"change_from_prev_week_pct"`
}

type InjectionVolatility struct {
	DailyStdDev              float64 `json:"daily_std_dev"`
	CoefficientOfVariation   float64 `json:"coefficient_of_variation"`
	MaxDailySwing            int64   `json:"max_daily_swing"`
	MaxSwingDate             string  `json:"max_swing_date"`
	StabilityScore           string  `json:"stability_score"`
}

// --- Internal data types ---

type dailyRow struct {
	date       time.Time
	injected   int64
	delivered  int64
	bounced    int64
	opened     int64
	clicked    int64
	complained int64
}

type ispRow struct {
	isp        string
	totalSent  int64
	delivered  int64
	bounced    int64
	opened     int64
	clicked    int64
	complained int64
}

// --- Range parsing ---

type rangeSpec struct {
	days  int
	label string
}

var rangeMap = map[string]rangeSpec{
	"7d":   {7, "Last 7 Days"},
	"14d":  {14, "Last 14 Days"},
	"30d":  {30, "Last 30 Days"},
	"60d":  {60, "Last 60 Days"},
	"90d":  {90, "Last 90 Days"},
	"180d": {180, "Last 180 Days"},
	"1y":   {365, "Last Year"},
}

// HandleGetInjectionAnalytics handles GET /api/mailing/injection-analytics
// This endpoint is public (no auth required).
func (h *InjectionAnalyticsHandler) HandleGetInjectionAnalytics(w http.ResponseWriter, r *http.Request) {
	// Parse org ID with multiple fallback sources (best-effort, not required)
	orgID := r.Header.Get("X-Organization-ID")
	if orgID == "" {
		var err error
		orgID, err = GetOrgIDStringFromRequest(r)
		if err != nil {
			orgID = ""
		}
	}
	if orgID == "" {
		// Fallback: try to discover an org ID from existing data
		for _, tbl := range []string{"mailing_isp_metrics", "mailing_campaigns", "mailing_subscribers", "mailing_lists"} {
			_ = h.db.QueryRow(fmt.Sprintf("SELECT DISTINCT organization_id FROM %s LIMIT 1", tbl)).Scan(&orgID)
			if orgID != "" {
				break
			}
		}
		// If still empty, use a default — don't block the request
		if orgID == "" {
			orgID = "default"
		}
	}

	// Parse range
	rangeParam := r.URL.Query().Get("range")
	if rangeParam == "" {
		rangeParam = "30d"
	}
	spec, ok := rangeMap[rangeParam]
	if !ok {
		spec = rangeMap["30d"]
	}

	// Parse compare flag
	compareParam := r.URL.Query().Get("compare")
	compare := true
	if compareParam == "false" || compareParam == "0" {
		compare = false
	}

	// Calculate date range
	now := time.Now().UTC()
	endDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	startDate := endDate.AddDate(0, 0, -spec.days+1)

	period := InjectionPeriod{
		Start: startDate.Format("2006-01-02"),
		End:   endDate.Format("2006-01-02"),
		Days:  spec.days,
		Label: spec.label,
	}

	// Fetch current period daily data
	dailyData := h.fetchDailyData(orgID, startDate, endDate)

	// Fetch ISP breakdown
	ispData := h.fetchISPBreakdown(orgID, startDate, endDate)

	// Fetch campaign count
	activeCampaigns := h.fetchCampaignCount(orgID, startDate, endDate)

	// Fetch revenue
	totalRevenue := h.fetchRevenue(orgID, startDate, endDate)

	// Build daily revenue map (approximate: distribute campaign revenue across period)
	dailyRevenue := h.fetchDailyRevenue(orgID, startDate, endDate, len(dailyData))

	// Build summary
	summary := h.buildSummary(dailyData, activeCampaigns, totalRevenue)

	// Fetch per-day campaign counts
	dailyCampaignCounts := h.fetchDailyCampaignCounts(orgID, startDate, endDate)

	// Build daily series with cumulative totals and inline MAs
	dailySeries := h.buildDailySeries(dailyData, dailyRevenue, dailyCampaignCounts)

	// Build ISP breakdown with volume shares and trends
	totalInjectedForShare := summary.TotalInjected
	ispBreakdown := h.buildISPBreakdown(ispData, totalInjectedForShare, orgID, startDate, endDate, spec.days)

	// Build moving averages
	movingAverages := h.buildMovingAverages(dailyData)

	// Build weekly aggregates
	weeklyAggregates := h.buildWeeklyAggregates(dailyData)

	// Build volatility metrics
	volatility := h.buildVolatility(dailyData)

	// Build comparison if requested
	var comparison *InjectionComparison
	if compare {
		comparison = h.buildComparison(orgID, startDate, endDate, spec.days, summary)
	}

	resp := InjectionAnalyticsResponse{
		Period:           period,
		Summary:          summary,
		Comparison:       comparison,
		DailySeries:      dailySeries,
		ISPBreakdown:     ispBreakdown,
		MovingAverages:   movingAverages,
		WeeklyAggregates: weeklyAggregates,
		Volatility:       volatility,
	}

	respondJSON(w, http.StatusOK, resp)
}

// --- Data fetching ---

func (h *InjectionAnalyticsHandler) fetchDailyData(orgID string, start, end time.Time) []dailyRow {
	// Primary source: mailing_isp_metrics (per-ISP daily aggregates)
	query := `
		SELECT metric_date,
		       COALESCE(SUM(total_sent), 0) as injected,
		       COALESCE(SUM(delivered), 0) as delivered,
		       COALESCE(SUM(bounced), 0) as bounced,
		       COALESCE(SUM(opened), 0) as opened,
		       COALESCE(SUM(clicked), 0) as clicked,
		       COALESCE(SUM(complained), 0) as complained
		FROM mailing_isp_metrics
		WHERE organization_id = $1
		  AND metric_date BETWEEN $2 AND $3
		GROUP BY metric_date
		ORDER BY metric_date ASC`

	rows, err := h.db.Query(query, orgID, start.Format("2006-01-02"), end.Format("2006-01-02"))
	if err == nil {
		defer rows.Close()
		var result []dailyRow
		for rows.Next() {
			var d dailyRow
			if err := rows.Scan(&d.date, &d.injected, &d.delivered, &d.bounced, &d.opened, &d.clicked, &d.complained); err != nil {
				continue
			}
			result = append(result, d)
		}
		if len(result) > 0 {
			return result
		}
	}

	// Fallback source: mailing_campaigns (aggregate campaign-level stats by send date)
	fallbackQuery := `
		SELECT DATE(send_at) as metric_date,
		       COALESCE(SUM(sent_count), 0) as injected,
		       COALESCE(SUM(delivered_count), 0) as delivered,
		       COALESCE(SUM(bounce_count), 0) as bounced,
		       COALESCE(SUM(open_count), 0) as opened,
		       COALESCE(SUM(click_count), 0) as clicked,
		       COALESCE(SUM(complaint_count), 0) as complained
		FROM mailing_campaigns
		WHERE organization_id = $1
		  AND send_at BETWEEN $2 AND $3
		  AND status IN ('sent', 'sending', 'completed', 'paused')
		GROUP BY DATE(send_at)
		ORDER BY DATE(send_at) ASC`

	rows2, err2 := h.db.Query(fallbackQuery, orgID, start, end)
	if err2 != nil {
		return nil
	}
	defer rows2.Close()

	var result []dailyRow
	for rows2.Next() {
		var d dailyRow
		if err := rows2.Scan(&d.date, &d.injected, &d.delivered, &d.bounced, &d.opened, &d.clicked, &d.complained); err != nil {
			continue
		}
		result = append(result, d)
	}
	return result
}

func (h *InjectionAnalyticsHandler) fetchISPBreakdown(orgID string, start, end time.Time) []ispRow {
	query := `
		SELECT isp,
		       COALESCE(SUM(total_sent), 0) as total_sent,
		       COALESCE(SUM(delivered), 0) as delivered,
		       COALESCE(SUM(bounced), 0) as bounced,
		       COALESCE(SUM(opened), 0) as opened,
		       COALESCE(SUM(clicked), 0) as clicked,
		       COALESCE(SUM(complained), 0) as complained
		FROM mailing_isp_metrics
		WHERE organization_id = $1
		  AND metric_date BETWEEN $2 AND $3
		GROUP BY isp
		ORDER BY SUM(total_sent) DESC
		LIMIT 15`

	rows, err := h.db.Query(query, orgID, start.Format("2006-01-02"), end.Format("2006-01-02"))
	if err != nil {
		return nil
	}
	defer rows.Close()

	var result []ispRow
	for rows.Next() {
		var d ispRow
		if err := rows.Scan(&d.isp, &d.totalSent, &d.delivered, &d.bounced, &d.opened, &d.clicked, &d.complained); err != nil {
			continue
		}
		result = append(result, d)
	}
	return result
}

func (h *InjectionAnalyticsHandler) fetchCampaignCount(orgID string, start, end time.Time) int {
	var count int
	err := h.db.QueryRow(
		`SELECT COUNT(DISTINCT id) FROM mailing_campaigns WHERE organization_id = $1 AND send_at BETWEEN $2 AND $3`,
		orgID, start, end,
	).Scan(&count)
	if err != nil {
		return 0
	}
	return count
}

func (h *InjectionAnalyticsHandler) fetchRevenue(orgID string, start, end time.Time) float64 {
	var revenue float64
	err := h.db.QueryRow(
		`SELECT COALESCE(SUM(revenue), 0) FROM mailing_campaigns WHERE organization_id = $1 AND send_at BETWEEN $2 AND $3`,
		orgID, start, end,
	).Scan(&revenue)
	if err != nil {
		return 0
	}
	return revenue
}

// fetchDailyRevenue tries to get per-day revenue from campaigns; if not granular enough, distributes evenly
func (h *InjectionAnalyticsHandler) fetchDailyRevenue(orgID string, start, end time.Time, numDays int) map[string]float64 {
	result := make(map[string]float64)

	// Try to get revenue grouped by send date
	query := `
		SELECT send_at::date as send_date, COALESCE(SUM(revenue), 0) as daily_rev
		FROM mailing_campaigns
		WHERE organization_id = $1 AND send_at BETWEEN $2 AND $3
		GROUP BY send_at::date
		ORDER BY send_date`

	rows, err := h.db.Query(query, orgID, start, end)
	if err != nil {
		return result
	}
	defer rows.Close()

	for rows.Next() {
		var d time.Time
		var rev float64
		if err := rows.Scan(&d, &rev); err != nil {
			continue
		}
		result[d.Format("2006-01-02")] = rev
	}
	return result
}

// fetchDailyCampaignCounts gets campaign count per day for the daily series
func (h *InjectionAnalyticsHandler) fetchDailyCampaignCounts(orgID string, start, end time.Time) map[string]int {
	result := make(map[string]int)

	query := `
		SELECT send_at::date as send_date, COUNT(DISTINCT id)
		FROM mailing_campaigns
		WHERE organization_id = $1 AND send_at BETWEEN $2 AND $3
		GROUP BY send_at::date`

	rows, err := h.db.Query(query, orgID, start, end)
	if err != nil {
		return result
	}
	defer rows.Close()

	for rows.Next() {
		var d time.Time
		var cnt int
		if err := rows.Scan(&d, &cnt); err != nil {
			continue
		}
		result[d.Format("2006-01-02")] = cnt
	}
	return result
}

// --- Summary building ---

func (h *InjectionAnalyticsHandler) buildSummary(data []dailyRow, activeCampaigns int, totalRevenue float64) InjectionSummary {
	s := InjectionSummary{
		ActiveCampaigns: activeCampaigns,
		TotalRevenue:    math.Round(totalRevenue*100) / 100,
	}

	if len(data) == 0 {
		return s
	}

	var peakVol int64 = math.MinInt64
	var lowVol int64 = math.MaxInt64
	var peakDate, lowDate string

	for _, d := range data {
		s.TotalInjected += d.injected
		s.TotalDelivered += d.delivered
		s.TotalBounced += d.bounced
		s.TotalOpened += d.opened
		s.TotalClicked += d.clicked
		s.TotalComplained += d.complained

		if d.injected > peakVol {
			peakVol = d.injected
			peakDate = d.date.Format("2006-01-02")
		}
		if d.injected < lowVol {
			lowVol = d.injected
			lowDate = d.date.Format("2006-01-02")
		}
	}

	// Calculate rates with safe division (prevents NaN from 0/0)
	s.DeliveryRate = safeRate(s.TotalDelivered, s.TotalInjected, 1)
	s.BounceRate = safeRate(s.TotalBounced, s.TotalInjected, 1)
	s.ComplaintRate = safeRate(s.TotalComplained, s.TotalInjected, 2)
	s.OpenRate = safeRate(s.TotalOpened, s.TotalDelivered, 1)
	s.ClickRate = safeRate(s.TotalClicked, s.TotalDelivered, 1)

	numDays := int64(len(data))
	if numDays > 0 {
		s.AvgDailyVolume = s.TotalInjected / numDays
	}
	s.PeakDailyVolume = peakVol
	s.PeakDate = peakDate
	if lowVol == math.MaxInt64 {
		lowVol = 0
	}
	s.LowDailyVolume = lowVol
	s.LowDate = lowDate

	return s
}

// --- Daily series building ---

func (h *InjectionAnalyticsHandler) buildDailySeries(data []dailyRow, dailyRevenue map[string]float64, dailyCampaignCounts map[string]int) []InjectionDailyPoint {
	if len(data) == 0 {
		return []InjectionDailyPoint{}
	}

	series := make([]InjectionDailyPoint, 0, len(data))
	var cumulative int64

	for i, d := range data {
		cumulative += d.injected
		dateStr := d.date.Format("2006-01-02")

		p := InjectionDailyPoint{
			Date:               dateStr,
			Injected:           d.injected,
			Delivered:          d.delivered,
			Bounced:            d.bounced,
			Opened:             d.opened,
			Clicked:            d.clicked,
			Complained:         d.complained,
			CumulativeInjected: cumulative,
			Revenue:            dailyRevenue[dateStr],
			CampaignsActive:    dailyCampaignCounts[dateStr],
		}

		if d.injected > 0 {
			p.DeliveryRate = roundTo(float64(d.delivered)/float64(d.injected)*100, 1)
			p.BounceRate = roundTo(float64(d.bounced)/float64(d.injected)*100, 1)
			p.ComplaintRate = roundTo(float64(d.complained)/float64(d.injected)*100, 1)
		}
		if d.delivered > 0 {
			p.OpenRate = roundTo(float64(d.opened)/float64(d.delivered)*100, 1)
			p.ClickRate = roundTo(float64(d.clicked)/float64(d.delivered)*100, 1)
		}

		// Inline 7-day MA
		if i >= 6 {
			ma := movingAverage(data, i, 7)
			p.MA7Injected = &ma
		}
		// Inline 14-day MA
		if i >= 13 {
			ma := movingAverage(data, i, 14)
			p.MA14Injected = &ma
		}

		series = append(series, p)
	}

	return series
}

// --- ISP breakdown ---

func (h *InjectionAnalyticsHandler) buildISPBreakdown(
	ispData []ispRow,
	totalInjected int64,
	orgID string,
	start, end time.Time,
	days int,
) []InjectionISPBreakdown {
	if len(ispData) == 0 {
		return []InjectionISPBreakdown{}
	}

	// Fetch previous period ISP data for trend calculation
	prevStart := start.AddDate(0, 0, -days)
	prevEnd := start.AddDate(0, 0, -1)
	prevISPData := h.fetchISPBreakdown(orgID, prevStart, prevEnd)
	prevISPMap := make(map[string]int64)
	for _, d := range prevISPData {
		prevISPMap[strings.ToLower(d.isp)] = d.totalSent
	}

	result := make([]InjectionISPBreakdown, 0, len(ispData))
	for _, d := range ispData {
		item := InjectionISPBreakdown{
			ISP:       d.isp,
			TotalSent: d.totalSent,
			Delivered: d.delivered,
		}

		if d.totalSent > 0 {
			item.DeliveryRate = roundTo(float64(d.delivered)/float64(d.totalSent)*100, 1)
			item.BounceRate = roundTo(float64(d.bounced)/float64(d.totalSent)*100, 1)
			item.ComplaintRate = roundTo(float64(d.complained)/float64(d.totalSent)*100, 2)
		}
		if d.delivered > 0 {
			item.OpenRate = roundTo(float64(d.opened)/float64(d.delivered)*100, 1)
			item.ClickRate = roundTo(float64(d.clicked)/float64(d.delivered)*100, 1)
		}
		if totalInjected > 0 {
			item.VolumeSharePct = roundTo(float64(d.totalSent)/float64(totalInjected)*100, 1)
		}

		// Calculate trend vs previous period
		prevVol, hasPrev := prevISPMap[strings.ToLower(d.isp)]
		if hasPrev && prevVol > 0 {
			changePct := float64(d.totalSent-prevVol) / float64(prevVol) * 100
			item.TrendPct = roundTo(changePct, 1)
			if changePct > 1 {
				item.Trend = "up"
			} else if changePct < -1 {
				item.Trend = "down"
			} else {
				item.Trend = "stable"
			}
		} else {
			item.Trend = "new"
			item.TrendPct = 0
		}

		result = append(result, item)
	}

	return result
}

// --- Moving averages ---

func (h *InjectionAnalyticsHandler) buildMovingAverages(data []dailyRow) InjectionMovingAverages {
	ma := InjectionMovingAverages{
		MA7:  make([]InjectionMAPoint, 0),
		MA14: make([]InjectionMAPoint, 0),
		MA30: make([]InjectionMAPoint, 0),
	}

	if len(data) == 0 {
		return ma
	}

	for i := range data {
		dateStr := data[i].date.Format("2006-01-02")

		if i >= 6 {
			val := movingAverage(data, i, 7)
			ma.MA7 = append(ma.MA7, InjectionMAPoint{Date: dateStr, Value: math.Round(val)})
		}
		if i >= 13 {
			val := movingAverage(data, i, 14)
			ma.MA14 = append(ma.MA14, InjectionMAPoint{Date: dateStr, Value: math.Round(val)})
		}
		if i >= 29 {
			val := movingAverage(data, i, 30)
			ma.MA30 = append(ma.MA30, InjectionMAPoint{Date: dateStr, Value: math.Round(val)})
		}
	}

	return ma
}

// --- Weekly aggregates ---

func (h *InjectionAnalyticsHandler) buildWeeklyAggregates(data []dailyRow) []InjectionWeeklyAggregate {
	if len(data) == 0 {
		return []InjectionWeeklyAggregate{}
	}

	// Group by ISO week starting Monday
	type weekBucket struct {
		start     time.Time
		end       time.Time
		injected  int64
		delivered int64
		opened    int64
		days      int
	}

	var buckets []weekBucket
	bucketMap := make(map[string]int) // week key -> index

	for _, d := range data {
		// Find the Monday of this date's week
		weekday := d.date.Weekday()
		if weekday == 0 {
			weekday = 7 // Sunday = 7
		}
		monday := d.date.AddDate(0, 0, -int(weekday-1))
		sunday := monday.AddDate(0, 0, 6)
		key := monday.Format("2006-01-02")

		idx, exists := bucketMap[key]
		if !exists {
			idx = len(buckets)
			bucketMap[key] = idx
			buckets = append(buckets, weekBucket{start: monday, end: sunday})
		}

		buckets[idx].injected += d.injected
		buckets[idx].delivered += d.delivered
		buckets[idx].opened += d.opened
		buckets[idx].days++
	}

	// Sort by start date
	sort.Slice(buckets, func(i, j int) bool {
		return buckets[i].start.Before(buckets[j].start)
	})

	result := make([]InjectionWeeklyAggregate, 0, len(buckets))
	for i, b := range buckets {
		wa := InjectionWeeklyAggregate{
			WeekStart:     b.start.Format("2006-01-02"),
			WeekEnd:       b.end.Format("2006-01-02"),
			WeekLabel:     fmt.Sprintf("Week %d", i+1),
			TotalInjected: b.injected,
		}

		if b.days > 0 {
			wa.AvgDaily = b.injected / int64(b.days)
		}
		if b.injected > 0 {
			wa.DeliveryRate = roundTo(float64(b.delivered)/float64(b.injected)*100, 1)
		}
		if b.delivered > 0 {
			wa.OpenRate = roundTo(float64(b.opened)/float64(b.delivered)*100, 1)
		}

		// Change from previous week
		if i > 0 && buckets[i-1].injected > 0 {
			change := float64(b.injected-buckets[i-1].injected) / float64(buckets[i-1].injected) * 100
			rounded := roundTo(change, 1)
			wa.ChangeFromPrevWeekPct = &rounded
		}

		result = append(result, wa)
	}

	return result
}

// --- Volatility ---

func (h *InjectionAnalyticsHandler) buildVolatility(data []dailyRow) InjectionVolatility {
	v := InjectionVolatility{
		StabilityScore: "insufficient_data",
	}

	if len(data) < 2 {
		return v
	}

	// Collect daily injection volumes
	volumes := make([]float64, len(data))
	for i, d := range data {
		volumes[i] = float64(d.injected)
	}

	// Mean
	var sum float64
	for _, vol := range volumes {
		sum += vol
	}
	mean := sum / float64(len(volumes))

	// Standard deviation
	var sqDiffSum float64
	for _, vol := range volumes {
		diff := vol - mean
		sqDiffSum += diff * diff
	}
	stdDev := math.Sqrt(sqDiffSum / float64(len(volumes)))
	v.DailyStdDev = math.Round(stdDev)

	// Coefficient of variation
	if mean > 0 {
		v.CoefficientOfVariation = roundTo(stdDev/mean*100, 1)
	}

	// Max daily swing (largest day-over-day change)
	var maxSwing int64
	var maxSwingDate string
	for i := 1; i < len(data); i++ {
		swing := injAbs64(data[i].injected - data[i-1].injected)
		if swing > maxSwing {
			maxSwing = swing
			maxSwingDate = data[i].date.Format("2006-01-02")
		}
	}
	v.MaxDailySwing = maxSwing
	v.MaxSwingDate = maxSwingDate

	// Stability score based on coefficient of variation
	cv := v.CoefficientOfVariation
	switch {
	case cv < 10:
		v.StabilityScore = "very_stable"
	case cv < 20:
		v.StabilityScore = "stable"
	case cv < 35:
		v.StabilityScore = "moderate"
	case cv < 50:
		v.StabilityScore = "volatile"
	default:
		v.StabilityScore = "highly_volatile"
	}

	return v
}

// --- Comparison ---

func (h *InjectionAnalyticsHandler) buildComparison(
	orgID string,
	currentStart, currentEnd time.Time,
	days int,
	currentSummary InjectionSummary,
) *InjectionComparison {
	prevEnd := currentStart.AddDate(0, 0, -1)
	prevStart := prevEnd.AddDate(0, 0, -days+1)

	prevData := h.fetchDailyData(orgID, prevStart, prevEnd)

	prevSummary := h.buildSummary(prevData, 0, 0)

	c := &InjectionComparison{
		PrevTotalInjected:  prevSummary.TotalInjected,
		PrevDeliveryRate:   prevSummary.DeliveryRate,
		PrevOpenRate:       prevSummary.OpenRate,
		PrevClickRate:      prevSummary.ClickRate,
		PrevBounceRate:     prevSummary.BounceRate,
		PrevComplaintRate:  prevSummary.ComplaintRate,
		PrevAvgDailyVolume: prevSummary.AvgDailyVolume,
	}

	// Calculate changes
	if prevSummary.TotalInjected > 0 {
		c.InjectedChangePct = roundTo(
			float64(currentSummary.TotalInjected-prevSummary.TotalInjected)/float64(prevSummary.TotalInjected)*100, 1)
	}

	c.DeliveryRateChange = roundTo(currentSummary.DeliveryRate-prevSummary.DeliveryRate, 1)
	c.OpenRateChange = roundTo(currentSummary.OpenRate-prevSummary.OpenRate, 1)
	c.ClickRateChange = roundTo(currentSummary.ClickRate-prevSummary.ClickRate, 1)
	c.BounceRateChange = roundTo(currentSummary.BounceRate-prevSummary.BounceRate, 1)
	c.ComplaintRateChange = roundTo(currentSummary.ComplaintRate-prevSummary.ComplaintRate, 2)

	if prevSummary.AvgDailyVolume > 0 {
		c.AvgDailyVolumeChangePct = roundTo(
			float64(currentSummary.AvgDailyVolume-prevSummary.AvgDailyVolume)/float64(prevSummary.AvgDailyVolume)*100, 1)
	}

	// Overall trend
	if c.InjectedChangePct > 2 {
		c.Trend = "up"
	} else if c.InjectedChangePct < -2 {
		c.Trend = "down"
	} else {
		c.Trend = "stable"
	}

	return c
}

// --- Utility functions ---

// safeRate calculates (numerator / denominator * 100) safely, returning 0 on divide-by-zero
func safeRate(numerator, denominator int64, decimals int) float64 {
	if denominator == 0 {
		return 0
	}
	return roundTo(float64(numerator)/float64(denominator)*100, decimals)
}

// safePctChange calculates ((current - prev) / prev * 100) safely
func safePctChange(current, prev int64, decimals int) float64 {
	if prev == 0 {
		return 0
	}
	return roundTo(float64(current-prev)/float64(prev)*100, decimals)
}

// movingAverage computes an N-day moving average ending at index i
func movingAverage(data []dailyRow, i int, window int) float64 {
	if i < window-1 || window == 0 {
		return 0
	}
	var sum int64
	for j := i - window + 1; j <= i; j++ {
		sum += data[j].injected
	}
	return float64(sum) / float64(window)
}

// injAbs64 returns absolute value of int64
func injAbs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
