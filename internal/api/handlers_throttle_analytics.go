package api

import (
	"database/sql"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

type throttleAnalyticsHandler struct {
	registry        *engine.ISPRateRegistry
	configs         map[engine.ISP]engine.ISPConfig
	db              *sql.DB
	convictionStore *engine.ConvictionStore
	factory         *engine.AgentFactory
	orgID           string
}

type liveRateEntry struct {
	ISP           string             `json:"isp"`
	DisplayName   string             `json:"display_name"`
	CurrentRate   float64            `json:"current_rate"`
	MaxRate       int                `json:"max_rate"`
	RatePct       float64            `json:"rate_pct"`
	EscalationAdj float64            `json:"escalation_adj,omitempty"`
	IPRates       map[string]float64 `json:"ip_rates,omitempty"`
}

type throttleDecisionEntry struct {
	ISP           string    `json:"isp"`
	Action        string    `json:"action"`
	EffectiveRate float64   `json:"effective_rate"`
	RateAdj       float64   `json:"rate_adj"`
	DeferralRate  float64   `json:"deferral_rate"`
	BackoffStep   float64   `json:"backoff_step"`
	Recovering    bool      `json:"recovering"`
	Result        string    `json:"result"`
	CreatedAt     time.Time `json:"created_at"`
}

type throttleConvictionEntry struct {
	ISP             string    `json:"isp"`
	Verdict         string    `json:"verdict"`
	Statement       string    `json:"statement"`
	EffectiveRate   int       `json:"effective_rate"`
	DeferralRate    float64   `json:"deferral_rate"`
	BackoffStep     int       `json:"backoff_step"`
	RecoveryTimeMin float64   `json:"recovery_time_min"`
	CreatedAt       time.Time `json:"created_at"`

	// Engagement telemetry captured at the moment of the conviction
	OpenRate1h            float64 `json:"open_rate_1h,omitempty"`
	ClickRate1h           float64 `json:"click_rate_1h,omitempty"`
	UniqueClicks          int     `json:"unique_clicks,omitempty"`
	ClickToComplaintRatio float64 `json:"click_to_complaint_ratio,omitempty"`
	EngagementScore       float64 `json:"engagement_score,omitempty"`
}

type throttleAnalyticsResponse struct {
	LiveRates            []liveRateEntry           `json:"live_rates"`
	RecentDecisions      []throttleDecisionEntry   `json:"recent_decisions"`
	Convictions          []throttleConvictionEntry `json:"convictions"`
	PerIPEnabled         bool                      `json:"per_ip_enabled"`
	EscalationEnabled    bool                      `json:"escalation_enabled"`
	UpdatedAt            time.Time                 `json:"updated_at"`
}

type audienceISPEntry struct {
	ISP              string  `json:"isp"`
	DisplayName      string  `json:"display_name"`
	Sent             int     `json:"sent"`
	Delivered        int     `json:"delivered"`
	Opens            int     `json:"opens"`
	Clicks           int     `json:"clicks"`
	HardBounces      int     `json:"hard_bounces"`
	SoftBounces      int     `json:"soft_bounces"`
	Complaints       int     `json:"complaints"`
	Unsubscribes     int     `json:"unsubscribes"`
	OpenRate         float64 `json:"open_rate"`
	ClickRate        float64 `json:"click_rate"`
	NewEngaged       int     `json:"new_engaged"`
	Churned          int     `json:"churned"`
	IntroductionRate float64 `json:"introduction_rate"`
	ChurnRate        float64 `json:"churn_rate"`
}

type audienceAnalyticsResponse struct {
	Date       string             `json:"date"`
	ExcludeMPP bool               `json:"exclude_mpp"`
	ISPs       []audienceISPEntry `json:"isps"`
	Totals     audienceISPEntry   `json:"totals"`
}

func (h *throttleAnalyticsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	resp := throttleAnalyticsResponse{
		LiveRates:         h.buildLiveRates(),
		RecentDecisions:   h.queryRecentDecisions(r),
		Convictions:       h.buildConvictions(),
		PerIPEnabled:      os.Getenv("ENABLE_PER_IP_RATE_LIMITING") == "true",
		EscalationEnabled: os.Getenv("ENABLE_ENGAGEMENT_ESCALATION") == "true",
		UpdatedAt:         time.Now().UTC(),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("[throttle-analytics] encode error: %v", err)
	}
}

func (h *throttleAnalyticsHandler) buildLiveRates() []liveRateEntry {
	if h.registry == nil {
		return []liveRateEntry{}
	}

	allRates := h.registry.GetAllRates()
	entries := make([]liveRateEntry, 0, len(allRates))

	for isp, currentRate := range allRates {
		maxRate := 0
		displayName := string(isp)
		if cfg, ok := h.configs[isp]; ok {
			maxRate = cfg.MaxMsgRate
			if cfg.DisplayName != "" {
				displayName = cfg.DisplayName
			}
		}

		pct := 0.0
		if maxRate > 0 {
			pct = math.Min((currentRate/float64(maxRate))*100, 100)
		}

		entry := liveRateEntry{
			ISP:         string(isp),
			DisplayName: displayName,
			CurrentRate: currentRate,
			MaxRate:     maxRate,
			RatePct:     pct,
		}
		if h.factory != nil {
			if ta := h.factory.GetThrottleAgent(isp); ta != nil {
				state := ta.GetState()
				if state.EscalationAdj > 1.0 {
					entry.EscalationAdj = state.EscalationAdj
				}
			}
		}
		if ipRates := h.registry.GetIPRates(isp); len(ipRates) > 0 {
			entry.IPRates = ipRates
		}
		entries = append(entries, entry)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ISP < entries[j].ISP
	})

	return entries
}

func (h *throttleAnalyticsHandler) queryRecentDecisions(r *http.Request) []throttleDecisionEntry {
	if h.db == nil {
		return []throttleDecisionEntry{}
	}

	rows, err := h.db.QueryContext(r.Context(),
		`SELECT isp, action_taken, action_params, result, created_at
		 FROM mailing_engine_decisions
		 WHERE organization_id = $1 AND agent_type = 'throttle'
		 ORDER BY created_at DESC
		 LIMIT 100`, h.orgID)
	if err != nil {
		log.Printf("[throttle-analytics] query error: %v", err)
		return []throttleDecisionEntry{}
	}
	defer rows.Close()

	entries := make([]throttleDecisionEntry, 0)
	for rows.Next() {
		var (
			isp, action, result string
			paramsRaw           []byte
			createdAt           time.Time
		)
		if err := rows.Scan(&isp, &action, &paramsRaw, &result, &createdAt); err != nil {
			log.Printf("[throttle-analytics] row scan error: %v", err)
			continue
		}

		var params map[string]interface{}
		_ = json.Unmarshal(paramsRaw, &params)

		entries = append(entries, throttleDecisionEntry{
			ISP:           isp,
			Action:        action,
			EffectiveRate: jsonFloat(params, "effective_rate"),
			RateAdj:       jsonFloat(params, "rate_adj"),
			DeferralRate:  jsonFloat(params, "deferral_rate"),
			BackoffStep:   jsonFloat(params, "backoff_step"),
			Recovering:    jsonBool(params, "recovering"),
			Result:        result,
			CreatedAt:     createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		log.Printf("[throttle-analytics] rows iteration error: %v", err)
	}

	return entries
}

func (h *throttleAnalyticsHandler) buildConvictions() []throttleConvictionEntry {
	if h.convictionStore == nil {
		return []throttleConvictionEntry{}
	}

	entries := make([]throttleConvictionEntry, 0)
	for _, isp := range engine.AllISPs() {
		recent := h.convictionStore.RecallRecent(isp, engine.AgentThrottle, 10)
		for i := len(recent) - 1; i >= 0; i-- {
			c := recent[i]
			entries = append(entries, throttleConvictionEntry{
				ISP:             string(c.ISP),
				Verdict:         string(c.Verdict),
				Statement:       c.Statement,
				EffectiveRate:   c.Context.EffectiveRate,
				DeferralRate:    c.Context.DeferralRate,
				BackoffStep:     c.Context.BackoffStep,
				RecoveryTimeMin: c.Context.RecoveryTimeMin,
				CreatedAt:       c.CreatedAt,

				OpenRate1h:            c.Context.OpenRate1h,
				ClickRate1h:           c.Context.ClickRate1h,
				UniqueClicks:          c.Context.UniqueClicks,
				ClickToComplaintRatio: c.Context.ClickToComplaintRatio,
				EngagementScore:       c.Context.EngagementScore,
			})
		}
	}

	return entries
}

var ispDisplayNames = map[string]string{
	"gmail":     "Gmail",
	"yahoo":     "Yahoo",
	"microsoft": "Microsoft",
	"apple":     "Apple",
	"comcast":   "Comcast",
	"charter":   "Charter",
	"att":       "AT&T",
	"cox":       "Cox",
	"other":     "Other ISPs",
}

func (h *throttleAnalyticsHandler) handleAudienceAnalytics(w http.ResponseWriter, r *http.Request) {
	dateStr := r.URL.Query().Get("date")
	if dateStr == "" {
		dateStr = time.Now().Format("2006-01-02")
	}
	excludeMPP := r.URL.Query().Get("exclude_mpp") == "true"

	if h.db == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(audienceAnalyticsResponse{
			Date: dateStr, ExcludeMPP: excludeMPP,
			ISPs: []audienceISPEntry{}, Totals: audienceISPEntry{},
		})
		return
	}

	ctx := r.Context()
	ispMap := make(map[string]*audienceISPEntry)

	getEntry := func(domain string) *audienceISPEntry {
		group := isp.GroupFromDomain(domain)
		if e, ok := ispMap[group]; ok {
			return e
		}
		dn := ispDisplayNames[group]
		if dn == "" {
			dn = group
		}
		e := &audienceISPEntry{ISP: group, DisplayName: dn}
		ispMap[group] = e
		return e
	}

	// Query 1: daily volume by domain
	rows, err := h.db.QueryContext(ctx, `
		SELECT COALESCE(t.recipient_domain, LOWER(SPLIT_PART(s.email, '@', 2)), '') AS domain,
		       SUM(CASE WHEN t.event_type = 'sent' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN t.event_type = 'delivered' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN t.event_type = 'opened'
		            AND ($2::boolean = FALSE OR COALESCE(t.is_machine_open, FALSE) = FALSE)
		            THEN 1 ELSE 0 END),
		       SUM(CASE WHEN t.event_type = 'clicked' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN t.event_type = 'bounced' AND COALESCE(t.bounce_type, '') = 'hard' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN t.event_type IN ('bounced', 'deferred')
		            AND COALESCE(t.bounce_type, '') != 'hard' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN t.event_type = 'complained' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN t.event_type = 'unsubscribed' THEN 1 ELSE 0 END)
		FROM mailing_tracking_events t
		LEFT JOIN mailing_subscribers s ON s.id = t.subscriber_id
		WHERE DATE(t.event_at) = $1::date
		  AND t.organization_id = $3::uuid
		GROUP BY domain`, dateStr, excludeMPP, h.orgID)
	if err != nil {
		log.Printf("[audience-analytics] volume query error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var domain string
		var sent, delivered, opens, clicks, hardB, softB, complaints, unsubs int
		if err := rows.Scan(&domain, &sent, &delivered, &opens, &clicks, &hardB, &softB, &complaints, &unsubs); err != nil {
			log.Printf("[audience-analytics] scan error: %v", err)
			continue
		}
		e := getEntry(domain)
		e.Sent += sent
		e.Delivered += delivered
		e.Opens += opens
		e.Clicks += clicks
		e.HardBounces += hardB
		e.SoftBounces += softB
		e.Complaints += complaints
		e.Unsubscribes += unsubs
	}

	// Query 2: new engaged (introduction) by domain
	rows2, err := h.db.QueryContext(ctx, `
		SELECT sub.domain, COUNT(DISTINCT sub.subscriber_id)
		FROM (
		    SELECT t.subscriber_id,
		           COALESCE(t.recipient_domain, LOWER(SPLIT_PART(s.email, '@', 2)), '') AS domain
		    FROM mailing_tracking_events t
		    LEFT JOIN mailing_subscribers s ON s.id = t.subscriber_id
		    WHERE t.event_type IN ('opened', 'clicked')
		      AND t.subscriber_id IS NOT NULL
		      AND t.organization_id = $2::uuid
		      AND ($3::boolean = FALSE OR t.event_type != 'opened' OR COALESCE(t.is_machine_open, FALSE) = FALSE)
		    GROUP BY t.subscriber_id, COALESCE(t.recipient_domain, LOWER(SPLIT_PART(s.email, '@', 2)), '')
		    HAVING DATE(MIN(t.event_at)) = $1::date
		) sub
		GROUP BY sub.domain`, dateStr, h.orgID, excludeMPP)
	if err != nil {
		log.Printf("[audience-analytics] introduction query error: %v", err)
	} else {
		defer rows2.Close()
		for rows2.Next() {
			var domain string
			var cnt int
			if err := rows2.Scan(&domain, &cnt); err != nil {
				continue
			}
			getEntry(domain).NewEngaged += cnt
		}
	}

	// Query 3: churned by domain
	rows3, err := h.db.QueryContext(ctx, `
		SELECT COALESCE(t.recipient_domain, LOWER(SPLIT_PART(s.email, '@', 2)), ''),
		       COUNT(DISTINCT t.subscriber_id)
		FROM mailing_tracking_events t
		LEFT JOIN mailing_subscribers s ON s.id = t.subscriber_id
		WHERE t.event_type IN ('bounced', 'complained', 'unsubscribed')
		  AND (t.event_type != 'bounced' OR COALESCE(t.bounce_type, '') = 'hard')
		  AND t.subscriber_id IS NOT NULL
		  AND DATE(t.event_at) = $1::date
		  AND t.organization_id = $2::uuid
		GROUP BY COALESCE(t.recipient_domain, LOWER(SPLIT_PART(s.email, '@', 2)), '')`,
		dateStr, h.orgID)
	if err != nil {
		log.Printf("[audience-analytics] churn query error: %v", err)
	} else {
		defer rows3.Close()
		for rows3.Next() {
			var domain string
			var cnt int
			if err := rows3.Scan(&domain, &cnt); err != nil {
				continue
			}
			getEntry(domain).Churned += cnt
		}
	}

	// Compute rates and build sorted slice
	var totals audienceISPEntry
	totals.ISP = "total"
	totals.DisplayName = "Total"

	ordered := make([]audienceISPEntry, 0, len(ispMap))
	for _, group := range isp.KnownGroups() {
		if e, ok := ispMap[group]; ok {
			e.OpenRate = calcPct(e.Opens, e.Delivered)
			e.ClickRate = calcPct(e.Clicks, e.Delivered)
			e.IntroductionRate = calcPct(e.NewEngaged, e.Delivered)
			e.ChurnRate = calcPct(e.Churned, e.Delivered)
			ordered = append(ordered, *e)
			accumulateTotals(&totals, e)
		}
	}
	if e, ok := ispMap["other"]; ok {
		e.OpenRate = calcPct(e.Opens, e.Delivered)
		e.ClickRate = calcPct(e.Clicks, e.Delivered)
		e.IntroductionRate = calcPct(e.NewEngaged, e.Delivered)
		e.ChurnRate = calcPct(e.Churned, e.Delivered)
		ordered = append(ordered, *e)
		accumulateTotals(&totals, e)
	}

	totals.OpenRate = calcPct(totals.Opens, totals.Delivered)
	totals.ClickRate = calcPct(totals.Clicks, totals.Delivered)
	totals.IntroductionRate = calcPct(totals.NewEngaged, totals.Delivered)
	totals.ChurnRate = calcPct(totals.Churned, totals.Delivered)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(audienceAnalyticsResponse{
		Date:       dateStr,
		ExcludeMPP: excludeMPP,
		ISPs:       ordered,
		Totals:     totals,
	})
}

func calcPct(num, denom int) float64 {
	if denom == 0 {
		return 0
	}
	return math.Round(float64(num)/float64(denom)*10000) / 100
}

func accumulateTotals(totals, e *audienceISPEntry) {
	totals.Sent += e.Sent
	totals.Delivered += e.Delivered
	totals.Opens += e.Opens
	totals.Clicks += e.Clicks
	totals.HardBounces += e.HardBounces
	totals.SoftBounces += e.SoftBounces
	totals.Complaints += e.Complaints
	totals.Unsubscribes += e.Unsubscribes
	totals.NewEngaged += e.NewEngaged
	totals.Churned += e.Churned
}

func jsonFloat(m map[string]interface{}, key string) float64 {
	if m == nil {
		return 0
	}
	if v, ok := m[key]; ok {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return 0
}

func jsonBool(m map[string]interface{}, key string) bool {
	if m == nil {
		return false
	}
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}
