package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// VersionDeliverability tracks the deliverability API contract.
//
// 2.0 (2026-04-27): Added ISP Health Center endpoints sourced from
// pmta_acct_raw — /timeseries, /matrix, /deferrals, /bounces, /fbl,
// /ip-activity. Existing /config and /throughput remain on the same
// codepath and continue to surface the rate-control card data.
const VersionDeliverability = "2.0"

// hardBounceFilterSQL emits the SQL fragment that classifies a hard bounce.
// Kept centralized so the metric definition stays single-source.
const hardBounceFilterSQL = `record_type = 'b' AND bounce_cat IN ('bad-mailbox','bad-domain','inactive-mailbox','no-answer-from-host','quota-issues','routing-errors')`

// softBounceFilterSQL is a real PMTA bounce that is NOT a hard category and
// NOT a reputation block (those go in their own bucket).
const softBounceFilterSQL = `record_type = 'b' AND bounce_cat NOT IN ('bad-mailbox','bad-domain','inactive-mailbox','no-answer-from-host','quota-issues','routing-errors','spam-related','policy-related')`

const reputationBlockedFilterSQL = `record_type = 'b' AND bounce_cat IN ('spam-related','policy-related')`

const attemptedFilterSQL = `record_type IN ('d','b','t','tq','f')`
const deliveredFilterSQL = `record_type = 'd'`
const deferredFilterSQL = `record_type IN ('t','tq')`
const complaintFilterSQL = `record_type = 'f'`

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

// ============================================================================
//  ISP Health Center (v2.0) — sourced from pmta_acct_raw
// ============================================================================
//
// The endpoints below all read from `pmta_acct_raw`, the raw PMTA accounting
// firehose written by internal/engine/ingest.go (line ~357). That table has
// every PMTA event with full classification: record_type, recipient_isp,
// envelope sender (→ sending_domain), vmta, pool, source_ip, bounce_cat,
// dsn_status, dsn_diag, time_logged, received_at.
//
// We deliberately do NOT use mailing_tracking_events here — that table has
// the well-documented multi-row-per-recipient grain bug (project-memory
// "tracking-events-grain-bug") which inflates totals via retries.
//
// All endpoints are read-only, accept window in {1h, 6h, 24h, 7d}, and
// classify metrics via the centralized SQL filter constants defined at the
// top of this file so every endpoint agrees on what "hard bounce" means.

// allowedWindows maps URL window param → SQL interval string.
var allowedWindows = map[string]string{
	"1h":  "1 hour",
	"6h":  "6 hours",
	"24h": "24 hours",
	"7d":  "7 days",
}

// allowedBuckets maps URL bucket param → date_trunc unit.
var allowedBuckets = map[string]string{
	"1m":  "minute",
	"5m":  "5 minutes",
	"15m": "15 minutes",
	"1h":  "hour",
}

func parseWindow(q string, def string) string {
	if v, ok := allowedWindows[q]; ok {
		return v
	}
	return allowedWindows[def]
}

// parseBucket returns a date_trunc-friendly expression for the bucket size.
// PostgreSQL date_trunc only supports a fixed set of units, so we emulate
// 5/15-minute buckets with a numeric truncation on epoch.
func parseBucketExpr(q, def string) (expr string, label string) {
	bucket := q
	if _, ok := allowedBuckets[bucket]; !ok {
		bucket = def
	}
	switch bucket {
	case "1m":
		return "date_trunc('minute', received_at)", "1m"
	case "5m":
		return "to_timestamp(floor(extract(epoch from received_at) / 300) * 300) AT TIME ZONE 'UTC'", "5m"
	case "15m":
		return "to_timestamp(floor(extract(epoch from received_at) / 900) * 900) AT TIME ZONE 'UTC'", "15m"
	case "1h":
		return "date_trunc('hour', received_at)", "1h"
	}
	return "date_trunc('minute', received_at)", "1m"
}

// metricToFilter returns the SQL WHERE fragment that selects rows for a
// given metric name. Returns ("", false) if the metric is unknown.
func metricToFilter(metric string) (string, bool) {
	switch metric {
	case "attempted":
		return attemptedFilterSQL, true
	case "delivered":
		return deliveredFilterSQL, true
	case "hard_bounce":
		return hardBounceFilterSQL, true
	case "soft_bounce":
		return softBounceFilterSQL, true
	case "deferred":
		return deferredFilterSQL, true
	case "complaint":
		return complaintFilterSQL, true
	case "reputation_blocked":
		return reputationBlockedFilterSQL, true
	}
	return "", false
}

// classifyISP normalizes a possibly-NULL recipient_isp value to a stable
// string. "other" is a real ISP bucket per the engine's ISPRegistry — we
// preserve it so the matrix doesn't silently drop the long tail.
const ispCoalesceSQL = `COALESCE(NULLIF(recipient_isp, ''), 'other')`

// sendingDomainSQL extracts envelope sending domain from the raw sender
// column. We strip surrounding angle brackets PMTA sometimes emits and
// fall back to 'unknown' so empty-sender FBL records still appear.
const sendingDomainSQL = `LOWER(NULLIF(split_part(regexp_replace(sender, '[<>]', '', 'g'), '@', 2), ''))`

// ----------------------------------------------------------------------------
//  Time series — drives the real-time line chart
// ----------------------------------------------------------------------------

type timeseriesPoint struct {
	Bucket string             `json:"bucket"`
	Series map[string]int     `json:"series"`
}

// HandleGetTimeSeries returns bucketed counts for one metric, grouped by
// either recipient_isp or sending_domain.
//
// GET /api/mailing/deliverability/timeseries
//   ?metric=attempted|delivered|hard_bounce|soft_bounce|deferred|complaint|reputation_blocked
//   &groupBy=isp|sending_domain
//   &window=1h|6h|24h
//   &bucket=1m|5m|15m|1h
func (h *deliverabilityHandler) HandleGetTimeSeries(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	metric := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("metric")))
	if metric == "" {
		metric = "delivered"
	}
	filter, ok := metricToFilter(metric)
	if !ok {
		http.Error(w, `{"error":"invalid metric"}`, http.StatusBadRequest)
		return
	}

	groupBy := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("groupBy")))
	if groupBy == "" {
		groupBy = "isp"
	}
	var groupExpr string
	switch groupBy {
	case "isp":
		groupExpr = ispCoalesceSQL
	case "sending_domain":
		groupExpr = "COALESCE(" + sendingDomainSQL + ", 'unknown')"
	default:
		http.Error(w, `{"error":"invalid groupBy"}`, http.StatusBadRequest)
		return
	}

	window := parseWindow(r.URL.Query().Get("window"), "1h")
	bucketExpr, bucketLabel := parseBucketExpr(r.URL.Query().Get("bucket"), defaultBucketFor(window))

	query := `
		SELECT ` + bucketExpr + ` AS bucket,
		       ` + groupExpr + ` AS series_key,
		       COUNT(*) AS value
		FROM pmta_acct_raw
		WHERE received_at > NOW() - $1::interval
		  AND ` + filter + `
		GROUP BY 1, 2
		ORDER BY 1 ASC, 2 ASC`

	rows, err := h.db.QueryContext(ctx, query, window)
	if err != nil {
		log.Printf("[deliverability/timeseries] query failed: %v", err)
		http.Error(w, `{"error":"database query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	pointsByBucket := make(map[string]*timeseriesPoint)
	keys := make(map[string]struct{})
	order := []string{}

	for rows.Next() {
		var ts time.Time
		var key string
		var value int
		if err := rows.Scan(&ts, &key, &value); err != nil {
			continue
		}
		bucket := ts.UTC().Format(time.RFC3339)
		if _, exists := pointsByBucket[bucket]; !exists {
			pointsByBucket[bucket] = &timeseriesPoint{
				Bucket: bucket,
				Series: make(map[string]int),
			}
			order = append(order, bucket)
		}
		pointsByBucket[bucket].Series[key] = value
		keys[key] = struct{}{}
	}

	out := make([]*timeseriesPoint, 0, len(order))
	keyList := make([]string, 0, len(keys))
	for k := range keys {
		keyList = append(keyList, k)
	}
	for _, b := range order {
		// Fill missing keys with zero so chart libraries don't break series
		// continuity when a bucket has no events for a given key.
		for _, k := range keyList {
			if _, ok := pointsByBucket[b].Series[k]; !ok {
				pointsByBucket[b].Series[k] = 0
			}
		}
		out = append(out, pointsByBucket[b])
	}

	total := 0
	for _, p := range out {
		for _, v := range p.Series {
			total += v
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"metric":      metric,
		"group_by":    groupBy,
		"window":      window,
		"bucket":      bucketLabel,
		"keys":        keyList,
		"buckets":     out,
		"total":       total,
		"api_version": VersionDeliverability,
		"updated_at":  time.Now().UTC(),
	})
}

func defaultBucketFor(window string) string {
	switch window {
	case "1 hour":
		return "1m"
	case "6 hours":
		return "5m"
	case "24 hours":
		return "15m"
	case "7 days":
		return "1h"
	}
	return "1m"
}

// ----------------------------------------------------------------------------
//  ISP × Sending-Domain matrix
// ----------------------------------------------------------------------------

type matrixCell struct {
	ISP               string  `json:"isp"`
	SendingDomain     string  `json:"sending_domain"`
	Attempted         int     `json:"attempted"`
	Delivered         int     `json:"delivered"`
	HardBounce        int     `json:"hard_bounce"`
	SoftBounce        int     `json:"soft_bounce"`
	Deferred          int     `json:"deferred"`
	Complaint         int     `json:"complaint"`
	ReputationBlocked int     `json:"reputation_blocked"`
	AcceptPct         float64 `json:"accept_pct"`
	DeferPct          float64 `json:"defer_pct"`
	HardPct           float64 `json:"hard_pct"`
	SoftPct           float64 `json:"soft_pct"`
	ComplaintPct      float64 `json:"complaint_pct"`
}

// HandleGetMatrix returns ISP × sending_domain breakdown for the window.
//
// GET /api/mailing/deliverability/matrix?window=1h|6h|24h|7d
func (h *deliverabilityHandler) HandleGetMatrix(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	window := parseWindow(r.URL.Query().Get("window"), "1h")

	query := `
		SELECT
			` + ispCoalesceSQL + ` AS isp,
			COALESCE(` + sendingDomainSQL + `, 'unknown') AS sending_domain,
			COUNT(*) FILTER (WHERE ` + attemptedFilterSQL + `) AS attempted,
			COUNT(*) FILTER (WHERE ` + deliveredFilterSQL + `) AS delivered,
			COUNT(*) FILTER (WHERE ` + hardBounceFilterSQL + `) AS hard_bounce,
			COUNT(*) FILTER (WHERE ` + softBounceFilterSQL + `) AS soft_bounce,
			COUNT(*) FILTER (WHERE ` + deferredFilterSQL + `) AS deferred,
			COUNT(*) FILTER (WHERE ` + complaintFilterSQL + `) AS complaint,
			COUNT(*) FILTER (WHERE ` + reputationBlockedFilterSQL + `) AS reputation_blocked
		FROM pmta_acct_raw
		WHERE received_at > NOW() - $1::interval
		GROUP BY 1, 2
		HAVING COUNT(*) > 0
		ORDER BY attempted DESC`

	rows, err := h.db.QueryContext(ctx, query, window)
	if err != nil {
		log.Printf("[deliverability/matrix] query failed: %v", err)
		http.Error(w, `{"error":"database query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	cells := []matrixCell{}
	totals := matrixCell{ISP: "_total", SendingDomain: "_all"}

	for rows.Next() {
		var c matrixCell
		if err := rows.Scan(&c.ISP, &c.SendingDomain, &c.Attempted, &c.Delivered,
			&c.HardBounce, &c.SoftBounce, &c.Deferred, &c.Complaint, &c.ReputationBlocked); err != nil {
			continue
		}
		c.AcceptPct = pct(c.Delivered, c.Attempted)
		c.DeferPct = pct(c.Deferred, c.Attempted)
		c.HardPct = pct(c.HardBounce, c.Attempted)
		c.SoftPct = pct(c.SoftBounce, c.Attempted)
		c.ComplaintPct = pct(c.Complaint, c.Attempted)
		cells = append(cells, c)

		totals.Attempted += c.Attempted
		totals.Delivered += c.Delivered
		totals.HardBounce += c.HardBounce
		totals.SoftBounce += c.SoftBounce
		totals.Deferred += c.Deferred
		totals.Complaint += c.Complaint
		totals.ReputationBlocked += c.ReputationBlocked
	}

	totals.AcceptPct = pct(totals.Delivered, totals.Attempted)
	totals.DeferPct = pct(totals.Deferred, totals.Attempted)
	totals.HardPct = pct(totals.HardBounce, totals.Attempted)
	totals.SoftPct = pct(totals.SoftBounce, totals.Attempted)
	totals.ComplaintPct = pct(totals.Complaint, totals.Attempted)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"window":      window,
		"cells":       cells,
		"totals":      totals,
		"api_version": VersionDeliverability,
		"updated_at":  time.Now().UTC(),
	})
}

func pct(num, denom int) float64 {
	if denom <= 0 {
		return 0
	}
	return math.Round(float64(num)/float64(denom)*10000) / 100
}

// ----------------------------------------------------------------------------
//  Drilldowns: deferrals, bounces, FBL
// ----------------------------------------------------------------------------

type deferralRow struct {
	ISP           string `json:"isp"`
	SendingDomain string `json:"sending_domain"`
	DSNStatus     string `json:"dsn_status"`
	DSNSample     string `json:"dsn_sample"`
	Count         int    `json:"count"`
}

// HandleGetDeferrals returns top deferral DSN strings per ISP × sending_domain.
//
// GET /api/mailing/deliverability/deferrals?window=1h|24h&isp=&sending_domain=&limit=20
func (h *deliverabilityHandler) HandleGetDeferrals(w http.ResponseWriter, r *http.Request) {
	h.runDrilldown(w, r, drilldownConfig{
		name:       "deferrals",
		recordCond: deferredFilterSQL,
		groupCol:   "dsn_status",
	})
}

// HandleGetBounces returns bounce category breakdown per ISP × sending_domain.
//
// GET /api/mailing/deliverability/bounces?window=24h&isp=&sending_domain=&limit=20
func (h *deliverabilityHandler) HandleGetBounces(w http.ResponseWriter, r *http.Request) {
	h.runDrilldown(w, r, drilldownConfig{
		name:       "bounces",
		recordCond: "record_type = 'b'",
		groupCol:   "bounce_cat",
	})
}

// HandleGetFBL returns FBL complaint breakdown per ISP × sending_domain.
// FBL events are infrequent so window default is wider (24h).
//
// GET /api/mailing/deliverability/fbl?window=7d&isp=&sending_domain=&limit=50
func (h *deliverabilityHandler) HandleGetFBL(w http.ResponseWriter, r *http.Request) {
	h.runDrilldown(w, r, drilldownConfig{
		name:       "fbl",
		recordCond: complaintFilterSQL,
		groupCol:   "dsn_status",
		windowDef:  "7d",
	})
}

type drilldownConfig struct {
	name       string
	recordCond string
	groupCol   string // "dsn_status" or "bounce_cat"
	windowDef  string // optional override; default "24h"
}

func (h *deliverabilityHandler) runDrilldown(w http.ResponseWriter, r *http.Request, cfg drilldownConfig) {
	ctx := r.Context()
	def := cfg.windowDef
	if def == "" {
		def = "24h"
	}
	window := parseWindow(r.URL.Query().Get("window"), def)

	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	args := []interface{}{window}
	conds := []string{
		"received_at > NOW() - $1::interval",
		cfg.recordCond,
	}

	if isp := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("isp"))); isp != "" {
		args = append(args, isp)
		conds = append(conds, fmt.Sprintf("LOWER(COALESCE(recipient_isp,'other')) = $%d", len(args)))
	}
	if sd := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sending_domain"))); sd != "" {
		args = append(args, sd)
		conds = append(conds, fmt.Sprintf("%s = $%d", sendingDomainSQL, len(args)))
	}

	groupExpr := "COALESCE(NULLIF(" + cfg.groupCol + ",''), 'unspecified')"

	args = append(args, limit)
	limitArg := fmt.Sprintf("$%d", len(args))

	query := `
		SELECT
			` + ispCoalesceSQL + ` AS isp,
			COALESCE(` + sendingDomainSQL + `, 'unknown') AS sending_domain,
			` + groupExpr + ` AS group_key,
			COALESCE(MAX(NULLIF(dsn_diag, '')), '') AS dsn_sample,
			COUNT(*) AS count
		FROM pmta_acct_raw
		WHERE ` + strings.Join(conds, " AND ") + `
		GROUP BY 1, 2, 3
		ORDER BY count DESC
		LIMIT ` + limitArg

	rows, err := h.db.QueryContext(ctx, query, args...)
	if err != nil {
		log.Printf("[deliverability/%s] query failed: %v", cfg.name, err)
		http.Error(w, `{"error":"database query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := []deferralRow{}
	for rows.Next() {
		var r deferralRow
		if err := rows.Scan(&r.ISP, &r.SendingDomain, &r.DSNStatus, &r.DSNSample, &r.Count); err != nil {
			continue
		}
		// Truncate long DSN diagnostic samples — they can be 1-2KB strings.
		if len(r.DSNSample) > 240 {
			r.DSNSample = r.DSNSample[:237] + "..."
		}
		out = append(out, r)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"window":      window,
		"limit":       limit,
		"rows":        out,
		"api_version": VersionDeliverability,
		"updated_at":  time.Now().UTC(),
	})
}

// ----------------------------------------------------------------------------
//  IP Activity (consolidates the retired Warmup screen)
// ----------------------------------------------------------------------------

type ipActivityRow struct {
	IP            string  `json:"ip"`
	Hostname      string  `json:"hostname"`
	Pool          string  `json:"pool"`
	Status        string  `json:"status"`
	WarmupDay     int     `json:"warmup_day"`
	Sent          int     `json:"sent"`
	Delivered     int     `json:"delivered"`
	HardBounce    int     `json:"hard_bounce"`
	SoftBounce    int     `json:"soft_bounce"`
	Deferred      int     `json:"deferred"`
	Complaint     int     `json:"complaint"`
	AcceptPct     float64 `json:"accept_pct"`
	LastSeenAt    *string `json:"last_seen_at"`
}

type ipActivityResp struct {
	Window     string          `json:"window"`
	Active     []ipActivityRow `json:"active"`
	Cold       []ipActivityRow `json:"cold"`
	Paused     []ipActivityRow `json:"paused"`
	APIVersion string          `json:"api_version"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// HandleGetIPActivity classifies every known IP into Active/Cold/Paused based
// on whether it has any rows in pmta_acct_raw within the window. This
// replaces the retired Warmup Dashboard's per-IP detail and surfaces the
// "never mailed on" set the user explicitly asked for.
//
// GET /api/mailing/deliverability/ip-activity?window=24h
func (h *deliverabilityHandler) HandleGetIPActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	window := parseWindow(r.URL.Query().Get("window"), "24h")

	query := `
		WITH activity AS (
			SELECT
				source_ip,
				COUNT(*) FILTER (WHERE ` + attemptedFilterSQL + `) AS sent,
				COUNT(*) FILTER (WHERE ` + deliveredFilterSQL + `) AS delivered,
				COUNT(*) FILTER (WHERE ` + hardBounceFilterSQL + `) AS hard_bounce,
				COUNT(*) FILTER (WHERE ` + softBounceFilterSQL + `) AS soft_bounce,
				COUNT(*) FILTER (WHERE ` + deferredFilterSQL + `) AS deferred,
				COUNT(*) FILTER (WHERE ` + complaintFilterSQL + `) AS complaint,
				MAX(received_at) AS last_seen_at
			FROM pmta_acct_raw
			WHERE received_at > NOW() - $1::interval
			  AND source_ip IS NOT NULL AND source_ip <> ''
			GROUP BY source_ip
		)
		SELECT
			HOST(ip.ip_address) AS ip,
			COALESCE(ip.hostname, '') AS hostname,
			COALESCE(p.name, '') AS pool,
			COALESCE(ip.status, 'unknown') AS status,
			COALESCE(ip.warmup_day, 0) AS warmup_day,
			COALESCE(a.sent, 0) AS sent,
			COALESCE(a.delivered, 0) AS delivered,
			COALESCE(a.hard_bounce, 0) AS hard_bounce,
			COALESCE(a.soft_bounce, 0) AS soft_bounce,
			COALESCE(a.deferred, 0) AS deferred,
			COALESCE(a.complaint, 0) AS complaint,
			a.last_seen_at
		FROM mailing_ip_addresses ip
		LEFT JOIN mailing_ip_pools p ON p.id = ip.pool_id
		LEFT JOIN activity a ON a.source_ip = HOST(ip.ip_address)
		ORDER BY ip.ip_address ASC`

	rows, err := h.db.QueryContext(ctx, query, window)
	if err != nil {
		log.Printf("[deliverability/ip-activity] query failed: %v", err)
		http.Error(w, `{"error":"database query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	resp := ipActivityResp{
		Window:     window,
		Active:     []ipActivityRow{},
		Cold:       []ipActivityRow{},
		Paused:     []ipActivityRow{},
		APIVersion: VersionDeliverability,
		UpdatedAt:  time.Now().UTC(),
	}

	for rows.Next() {
		var row ipActivityRow
		var lastSeen sql.NullTime
		if err := rows.Scan(&row.IP, &row.Hostname, &row.Pool, &row.Status, &row.WarmupDay,
			&row.Sent, &row.Delivered, &row.HardBounce, &row.SoftBounce,
			&row.Deferred, &row.Complaint, &lastSeen); err != nil {
			continue
		}
		row.AcceptPct = pct(row.Delivered, row.Sent)
		if lastSeen.Valid {
			s := lastSeen.Time.UTC().Format(time.RFC3339)
			row.LastSeenAt = &s
		}

		switch {
		case row.Status == "paused" || row.Status == "quarantined" || row.Status == "reserved":
			resp.Paused = append(resp.Paused, row)
		case row.Sent > 0:
			resp.Active = append(resp.Active, row)
		default:
			// Cold: known IP, status active/warmup/cold but ZERO traffic in window.
			// This includes "never mailed on" IPs that the user explicitly asked
			// for visibility into.
			resp.Cold = append(resp.Cold, row)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
