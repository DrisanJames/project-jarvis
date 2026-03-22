package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

const VersionDeliverability = "1.0"

type deliverabilityHandler struct {
	db           *sql.DB
	rateRegistry *engine.ISPRateRegistry
	agentFactory *engine.AgentFactory
	ispConfigs   map[engine.ISP]engine.ISPConfig
}

type ispConfigResponse struct {
	ISP                string  `json:"isp"`
	DisplayName        string  `json:"display_name"`
	MaxMsgRate         int     `json:"max_msg_rate"`
	MaxConnections     int     `json:"max_connections"`
	BounceWarnPct      float64 `json:"bounce_warn_pct"`
	BounceActionPct    float64 `json:"bounce_action_pct"`
	ComplaintWarnPct   float64 `json:"complaint_warn_pct"`
	ComplaintActionPct float64 `json:"complaint_action_pct"`
	PoolName           string  `json:"pool_name"`
	Enabled            bool    `json:"enabled"`

	CurrentRate    float64 `json:"current_rate"`
	RateAdjustment float64 `json:"rate_adjustment"`
	BackoffCount   int     `json:"backoff_count"`
	InRecovery     bool    `json:"in_recovery"`
	IPCount        int     `json:"ip_count"`

	Sent1h       int `json:"sent_1h"`
	Delivered1h  int `json:"delivered_1h"`
	HardBounce1h int `json:"hard_bounce_1h"`
	SoftBounce1h int `json:"soft_bounce_1h"`
	Deferred1h   int `json:"deferred_1h"`
	Complained1h int `json:"complained_1h"`
}

// HandleGetConfig returns all ISP configs with live runtime state.
// GET /api/mailing/deliverability/config
func (h *deliverabilityHandler) HandleGetConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	configs := h.agentFactory.GetConfigs()

	allRates := make(map[engine.ISP]float64)
	if h.rateRegistry != nil {
		allRates = h.rateRegistry.GetAllRates()
	}

	throttleStates := h.loadThrottleStates(ctx)

	var result []ispConfigResponse
	totalCapacity := 0.0

	for isp, cfg := range configs {
		currentRate := allRates[isp]
		rateAdj := 1.0
		if cfg.MaxMsgRate > 0 {
			rateAdj = currentRate / float64(cfg.MaxMsgRate)
		}

		entry := ispConfigResponse{
			ISP:                string(isp),
			DisplayName:        cfg.DisplayName,
			MaxMsgRate:         cfg.MaxMsgRate,
			MaxConnections:     cfg.MaxConnections,
			BounceWarnPct:      cfg.BounceWarnPct,
			BounceActionPct:    cfg.BounceActionPct,
			ComplaintWarnPct:   cfg.ComplaintWarnPct,
			ComplaintActionPct: cfg.ComplaintActionPct,
			PoolName:           cfg.PoolName,
			Enabled:            cfg.Enabled,
			CurrentRate:        currentRate,
			RateAdjustment:     math.Round(rateAdj*1000) / 1000,
		}

		if ts, ok := throttleStates[string(isp)]; ok {
			entry.BackoffCount = ts.backoffCount
			entry.InRecovery = ts.inRecovery
		}

		entry.IPCount = h.countIPsForPool(ctx, cfg.PoolName)

		stats := h.loadISPStats1h(ctx, cfg.DomainPatterns)
		entry.Sent1h = stats.sent
		entry.Delivered1h = stats.delivered
		entry.HardBounce1h = stats.hardBounce
		entry.SoftBounce1h = stats.softBounce
		entry.Deferred1h = stats.deferred
		entry.Complained1h = stats.complained

		totalCapacity += float64(cfg.MaxMsgRate)
		result = append(result, entry)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"configs":           result,
		"total_capacity_hr": totalCapacity,
		"projected_8h":      totalCapacity * 8,
		"api_version":       VersionDeliverability,
		"updated_at":        time.Now().UTC(),
	})
}

type ispConfigPatch struct {
	MaxMsgRate         *int     `json:"max_msg_rate"`
	MaxConnections     *int     `json:"max_connections"`
	BounceWarnPct      *float64 `json:"bounce_warn_pct"`
	BounceActionPct    *float64 `json:"bounce_action_pct"`
	ComplaintWarnPct   *float64 `json:"complaint_warn_pct"`
	ComplaintActionPct *float64 `json:"complaint_action_pct"`
	Enabled            *bool    `json:"enabled"`
}

// HandlePatchConfig updates ISP config fields and hot-reloads into the engine.
// PATCH /api/mailing/deliverability/config/{isp}
func (h *deliverabilityHandler) HandlePatchConfig(w http.ResponseWriter, r *http.Request) {
	ispParam := chi.URLParam(r, "isp")
	if ispParam == "" {
		http.Error(w, `{"error":"missing isp parameter"}`, http.StatusBadRequest)
		return
	}
	isp := engine.ISP(strings.ToLower(ispParam))

	var patch ispConfigPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}

	if patch.MaxMsgRate != nil && *patch.MaxMsgRate <= 0 {
		http.Error(w, `{"error":"max_msg_rate must be positive"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	updatedCfg, found := h.agentFactory.UpdateConfig(isp, func(cfg *engine.ISPConfig) {
		if patch.MaxMsgRate != nil {
			cfg.MaxMsgRate = *patch.MaxMsgRate
		}
		if patch.MaxConnections != nil {
			cfg.MaxConnections = *patch.MaxConnections
		}
		if patch.BounceWarnPct != nil {
			cfg.BounceWarnPct = *patch.BounceWarnPct
		}
		if patch.BounceActionPct != nil {
			cfg.BounceActionPct = *patch.BounceActionPct
		}
		if patch.ComplaintWarnPct != nil {
			cfg.ComplaintWarnPct = *patch.ComplaintWarnPct
		}
		if patch.ComplaintActionPct != nil {
			cfg.ComplaintActionPct = *patch.ComplaintActionPct
		}
		if patch.Enabled != nil {
			cfg.Enabled = *patch.Enabled
		}
	})
	if !found {
		http.Error(w, `{"error":"ISP not found in engine"}`, http.StatusNotFound)
		return
	}

	orgID, _ := GetOrgIDFromRequest(r)
	_, err := h.db.ExecContext(ctx, `
		UPDATE mailing_engine_isp_config
		SET max_msg_rate = $1, max_connections = $2,
		    bounce_warn_pct = $3, bounce_action_pct = $4,
		    complaint_warn_pct = $5, complaint_action_pct = $6,
		    enabled = $7, updated_at = NOW()
		WHERE isp = $8 AND ($9 = '' OR organization_id = $9::uuid)
	`, updatedCfg.MaxMsgRate, updatedCfg.MaxConnections,
		updatedCfg.BounceWarnPct, updatedCfg.BounceActionPct,
		updatedCfg.ComplaintWarnPct, updatedCfg.ComplaintActionPct,
		updatedCfg.Enabled, string(isp), orgID)
	if err != nil {
		log.Printf("[deliverability] DB update failed for %s: %v", isp, err)
		http.Error(w, `{"error":"database update failed"}`, http.StatusInternalServerError)
		return
	}

	if h.rateRegistry != nil && patch.MaxMsgRate != nil {
		h.rateRegistry.SetRate(isp, float64(*patch.MaxMsgRate))
		log.Printf("[deliverability] hot-reload: %s rate updated to %d/hr", isp, *patch.MaxMsgRate)
	}

	h.ispConfigs[isp] = updatedCfg

	newRate := float64(updatedCfg.MaxMsgRate)
	if h.rateRegistry != nil {
		newRate = h.rateRegistry.GetRate(isp)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"isp":            string(isp),
		"max_msg_rate":   updatedCfg.MaxMsgRate,
		"effective_rate": newRate,
		"updated":        true,
	})
}

// HandleResetThrottle clears throttle state for one ISP and restores full rate.
// POST /api/mailing/deliverability/config/{isp}/reset-throttle
func (h *deliverabilityHandler) HandleResetThrottle(w http.ResponseWriter, r *http.Request) {
	ispParam := chi.URLParam(r, "isp")
	if ispParam == "" {
		http.Error(w, `{"error":"missing isp parameter"}`, http.StatusBadRequest)
		return
	}
	isp := engine.ISP(strings.ToLower(ispParam))

	effectiveRate, found := h.agentFactory.ResetThrottle(isp)
	if !found {
		http.Error(w, `{"error":"ISP not found in engine"}`, http.StatusNotFound)
		return
	}

	log.Printf("[deliverability] throttle reset for %s, effective rate now %d/hr", isp, effectiveRate)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"isp":            string(isp),
		"effective_rate": effectiveRate,
		"throttle_reset": true,
	})
}

// HandleGetThroughput returns real-time throughput snapshot.
// GET /api/mailing/deliverability/throughput
func (h *deliverabilityHandler) HandleGetThroughput(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	configs := h.agentFactory.GetConfigs()
	allRates := make(map[engine.ISP]float64)
	if h.rateRegistry != nil {
		allRates = h.rateRegistry.GetAllRates()
	}

	type ispThroughput struct {
		ISP           string  `json:"isp"`
		DisplayName   string  `json:"display_name"`
		ConfigRate    int     `json:"config_rate"`
		EffectiveRate float64 `json:"effective_rate"`
		Sent1h        int     `json:"sent_1h"`
		Delivered1h   int     `json:"delivered_1h"`
		IPsActive     int     `json:"ips_active"`
		IPsTotal      int     `json:"ips_total"`
	}

	var isps []ispThroughput
	totalConfig := 0.0
	totalEffective := 0.0
	totalSent := 0
	totalDelivered := 0

	for isp, cfg := range configs {
		effective := allRates[isp]
		stats := h.loadISPStats1h(ctx, cfg.DomainPatterns)
		ipActive, ipTotal := h.countIPsDetailed(ctx, cfg.PoolName)

		isps = append(isps, ispThroughput{
			ISP:           string(isp),
			DisplayName:   cfg.DisplayName,
			ConfigRate:    cfg.MaxMsgRate,
			EffectiveRate: effective,
			Sent1h:        stats.sent,
			Delivered1h:   stats.delivered,
			IPsActive:     ipActive,
			IPsTotal:      ipTotal,
		})
		totalConfig += float64(cfg.MaxMsgRate)
		totalEffective += effective
		totalSent += stats.sent
		totalDelivered += stats.delivered
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"isps":               isps,
		"total_config_hr":    totalConfig,
		"total_effective_hr": totalEffective,
		"projected_8h":       totalEffective * 8,
		"total_sent_1h":      totalSent,
		"total_delivered_1h": totalDelivered,
		"api_version":        VersionDeliverability,
		"updated_at":         time.Now().UTC(),
	})
}

// --- helpers ---

type throttleStateRow struct {
	backoffCount int
	inRecovery   bool
}

func (h *deliverabilityHandler) loadThrottleStates(ctx context.Context) map[string]throttleStateRow {
	states := make(map[string]throttleStateRow)
	rows, err := h.db.QueryContext(ctx, `
		SELECT isp, backoff_count, in_recovery
		FROM mailing_engine_throttle_agent_state
		WHERE updated_at > NOW() - INTERVAL '4 hours'
	`)
	if err != nil {
		return states
	}
	defer rows.Close()
	for rows.Next() {
		var isp string
		var bc int
		var recovery bool
		if rows.Scan(&isp, &bc, &recovery) == nil {
			states[isp] = throttleStateRow{backoffCount: bc, inRecovery: recovery}
		}
	}
	return states
}

type ispStats struct {
	sent       int
	delivered  int
	hardBounce int
	softBounce int
	deferred   int
	complained int
}

func (h *deliverabilityHandler) loadISPStats1h(ctx context.Context, domains []string) ispStats {
	if len(domains) == 0 {
		return ispStats{}
	}
	domainArr := "{" + strings.Join(lowerAllDomains(domains), ",") + "}"
	var s ispStats
	_ = h.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE event_type IN ('sent','delivered','bounced','deferred','complained')) AS sent,
			COUNT(*) FILTER (WHERE event_type = 'delivered') AS delivered,
			COUNT(*) FILTER (WHERE event_type = 'bounced' AND bounce_class IN ('10','30','90')) AS hard_bounce,
			COUNT(*) FILTER (WHERE event_type = 'bounced' AND bounce_class NOT IN ('10','30','90')) AS soft_bounce,
			COUNT(*) FILTER (WHERE event_type = 'deferred') AS deferred,
			COUNT(*) FILTER (WHERE event_type = 'complained') AS complained
		FROM mailing_tracking_events
		WHERE LOWER(recipient_domain) = ANY($1::text[])
		AND event_at > NOW() - INTERVAL '1 hour'
	`, domainArr).Scan(&s.sent, &s.delivered, &s.hardBounce, &s.softBounce, &s.deferred, &s.complained)
	return s
}

func (h *deliverabilityHandler) countIPsForPool(ctx context.Context, poolName string) int {
	var count int
	_ = h.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_ip_addresses ip
		JOIN mailing_ip_pools p ON ip.pool_id = p.id
		WHERE p.name LIKE $1 AND ip.status IN ('active','warmup')
	`, "%"+poolName+"%").Scan(&count)
	return count
}

func (h *deliverabilityHandler) countIPsDetailed(ctx context.Context, poolName string) (active int, total int) {
	_ = h.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE ip.status IN ('active','warmup')),
			COUNT(*)
		FROM mailing_ip_addresses ip
		JOIN mailing_ip_pools p ON ip.pool_id = p.id
		WHERE p.name LIKE $1
	`, "%"+poolName+"%").Scan(&active, &total)
	return
}

func lowerAllDomains(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.ToLower(s)
	}
	return out
}
