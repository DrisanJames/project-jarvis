package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/ignite/sparkpost-monitor/internal/datanorm"
)

// DataNormHandler exposes API endpoints for the S3 data normalizer.
type DataNormHandler struct {
	normalizer *datanorm.Normalizer
	db         *sql.DB
}

func NewDataNormHandler(normalizer *datanorm.Normalizer, db *sql.DB) *DataNormHandler {
	return &DataNormHandler{normalizer: normalizer, db: db}
}

// HandleTrigger triggers a manual import cycle.
func (h *DataNormHandler) HandleTrigger(w http.ResponseWriter, r *http.Request) {
	if h.normalizer == nil {
		http.Error(w, `{"error":"normalizer not initialized"}`, http.StatusServiceUnavailable)
		return
	}
	if h.normalizer.IsRunning() {
		json.NewEncoder(w).Encode(map[string]string{"status": "already_running"})
		return
	}
	h.normalizer.ManualTrigger()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "triggered"})
}

// HandleStatus returns health and run state of the normalizer.
func (h *DataNormHandler) HandleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	status := map[string]interface{}{
		"initialized": h.normalizer != nil,
	}

	if h.normalizer != nil {
		status["healthy"] = h.normalizer.IsHealthy()
		status["running"] = h.normalizer.IsRunning()
		lastRun := h.normalizer.LastRunAt()
		if !lastRun.IsZero() {
			status["last_run_at"] = lastRun
		}
	}

	// Summary counts from data_import_log
	if h.db != nil {
		var total, completed, failed, processing int
		h.db.QueryRow(`SELECT
			COUNT(*),
			COUNT(*) FILTER (WHERE status = 'completed'),
			COUNT(*) FILTER (WHERE status = 'failed'),
			COUNT(*) FILTER (WHERE status = 'processing')
		FROM data_import_log`).Scan(&total, &completed, &failed, &processing)

		var totalRecords, totalErrors int
		h.db.QueryRow(`SELECT COALESCE(SUM(record_count),0), COALESCE(SUM(error_count),0) FROM data_import_log WHERE status = 'completed'`).Scan(&totalRecords, &totalErrors)

		status["files"] = map[string]int{
			"total":      total,
			"completed":  completed,
			"failed":     failed,
			"processing": processing,
		}
		status["records"] = map[string]int{
			"imported": totalRecords,
			"errors":   totalErrors,
		}
	}

	json.NewEncoder(w).Encode(status)
}

// HandleLogs returns paginated import history from data_import_log.
func (h *DataNormHandler) HandleLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	limit := 50
	offset := 0
	statusFilter := ""

	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	if v := r.URL.Query().Get("status"); v != "" {
		switch v {
		case "completed", "failed", "processing", "pending":
			statusFilter = v
		}
	}

	query := `SELECT id, original_key, renamed_key, classification, record_count, error_count, status, error_message, original_exists, processed_at, created_at
		FROM data_import_log`
	args := []interface{}{}

	if statusFilter != "" {
		query += ` WHERE status = $1`
		args = append(args, statusFilter)
	}

	query += ` ORDER BY created_at DESC LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)
	args = append(args, limit, offset)

	rows, err := h.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type LogEntry struct {
		ID             string  `json:"id"`
		OriginalKey    string  `json:"original_key"`
		RenamedKey     string  `json:"renamed_key"`
		Classification string  `json:"classification"`
		RecordCount    int     `json:"record_count"`
		ErrorCount     int     `json:"error_count"`
		Status         string  `json:"status"`
		ErrorMessage   *string `json:"error_message,omitempty"`
		OriginalExists bool    `json:"original_exists"`
		ProcessedAt    *string `json:"processed_at,omitempty"`
		CreatedAt      string  `json:"created_at"`
	}

	var logs []LogEntry
	for rows.Next() {
		var e LogEntry
		var processedAt, errorMsg sql.NullString
		err := rows.Scan(&e.ID, &e.OriginalKey, &e.RenamedKey, &e.Classification,
			&e.RecordCount, &e.ErrorCount, &e.Status, &errorMsg, &e.OriginalExists,
			&processedAt, &e.CreatedAt)
		if err != nil {
			continue
		}
		if errorMsg.Valid {
			e.ErrorMessage = &errorMsg.String
		}
		if processedAt.Valid {
			e.ProcessedAt = &processedAt.String
		}
		logs = append(logs, e)
	}

	if logs == nil {
		logs = []LogEntry{}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"logs":   logs,
		"limit":  limit,
		"offset": offset,
	})
}

// HandleQualityBreakdown returns verification and domain group distributions for the UI charts.
func (h *DataNormHandler) HandleQualityBreakdown(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	result := map[string]interface{}{}

	if h.db == nil {
		json.NewEncoder(w).Encode(result)
		return
	}

	var totalSubs int
	h.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM mailing_subscribers`).Scan(&totalSubs)
	result["total_subscribers"] = totalSubs

	type VerRow struct {
		Status     string  `json:"verification_status"`
		Count      int     `json:"count"`
		AvgQuality float64 `json:"avg_quality"`
	}
	var verRows []VerRow
	rows, err := h.db.QueryContext(r.Context(),
		`SELECT COALESCE(verification_status, ''), COUNT(*), ROUND(AVG(data_quality_score)::numeric, 2)
		 FROM mailing_subscribers GROUP BY verification_status ORDER BY COUNT(*) DESC`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var v VerRow
			rows.Scan(&v.Status, &v.Count, &v.AvgQuality)
			verRows = append(verRows, v)
		}
	}
	if verRows == nil {
		verRows = []VerRow{}
	}
	result["verification"] = verRows

	type DomRow struct {
		Group string `json:"domain_group"`
		Count int    `json:"count"`
	}
	var domRows []DomRow
	drows, derr := h.db.QueryContext(r.Context(),
		`SELECT COALESCE(custom_fields->>'domain_group', 'other'), COUNT(*)
		 FROM mailing_subscribers
		 WHERE custom_fields->>'domain_group' IS NOT NULL AND custom_fields->>'domain_group' != ''
		 GROUP BY custom_fields->>'domain_group' ORDER BY COUNT(*) DESC LIMIT 15`)
	if derr == nil {
		defer drows.Close()
		for drows.Next() {
			var d DomRow
			drows.Scan(&d.Group, &d.Count)
			domRows = append(domRows, d)
		}
	}
	if domRows == nil {
		domRows = []DomRow{}
	}
	result["domains"] = domRows

	json.NewEncoder(w).Encode(result)
}
