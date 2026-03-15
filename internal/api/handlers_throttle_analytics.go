package api

import (
	"database/sql"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/engine"
)

type throttleAnalyticsHandler struct {
	registry        *engine.ISPRateRegistry
	configs         map[engine.ISP]engine.ISPConfig
	db              *sql.DB
	convictionStore *engine.ConvictionStore
	orgID           string
}

type liveRateEntry struct {
	ISP         string  `json:"isp"`
	DisplayName string  `json:"display_name"`
	CurrentRate float64 `json:"current_rate"`
	MaxRate     int     `json:"max_rate"`
	RatePct     float64 `json:"rate_pct"`
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
}

type throttleAnalyticsResponse struct {
	LiveRates       []liveRateEntry           `json:"live_rates"`
	RecentDecisions []throttleDecisionEntry   `json:"recent_decisions"`
	Convictions     []throttleConvictionEntry `json:"convictions"`
	UpdatedAt       time.Time                 `json:"updated_at"`
}

func (h *throttleAnalyticsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	resp := throttleAnalyticsResponse{
		LiveRates:       h.buildLiveRates(),
		RecentDecisions: h.queryRecentDecisions(r),
		Convictions:     h.buildConvictions(),
		UpdatedAt:       time.Now().UTC(),
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

		entries = append(entries, liveRateEntry{
			ISP:         string(isp),
			DisplayName: displayName,
			CurrentRate: currentRate,
			MaxRate:     maxRate,
			RatePct:     pct,
		})
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
			})
		}
	}

	return entries
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
