package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/worker"
)

const VersionDataPipeline = "1.0"

type DataPipelineHandlers struct {
	db       *sql.DB
	pipeline *worker.DataPipeline
	server   *Server
}

func NewDataPipelineHandlers(db *sql.DB, pipeline *worker.DataPipeline) *DataPipelineHandlers {
	return &DataPipelineHandlers{db: db, pipeline: pipeline}
}

func (h *DataPipelineHandlers) getPipeline() *worker.DataPipeline {
	if h.pipeline != nil {
		return h.pipeline
	}
	if h.server != nil {
		return h.server.DataPipeline
	}
	return nil
}

type pipelineDailyStat struct {
	Verified   int `json:"verified"`
	Suppressed int `json:"suppressed"`
	Deduped    int `json:"deduped"`
	Files      int `json:"files"`
}

// HandleGetPipelineStats returns aggregate stats for the pipeline dashboard.
func (h *DataPipelineHandlers) HandleGetPipelineStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	type stats struct {
		TotalRuns       int                `json:"total_runs"`
		LastRunAt       *string            `json:"last_run_at"`
		LastRunStatus   string             `json:"last_run_status"`
		TotalVerified   int                `json:"total_verified"`
		TotalSuppressed int                `json:"total_suppressed"`
		TotalDeduped    int                `json:"total_deduped"`
		TotalFiles      int                `json:"total_files"`
		FilesAvailable  int                `json:"files_available"`
		FilesProcessed  int                `json:"files_processed"`
		Today           pipelineDailyStat  `json:"today"`
		Yesterday       pipelineDailyStat  `json:"yesterday"`
		APIVersion      string             `json:"api_version"`
	}

	var s stats
	s.APIVersion = VersionDataPipeline

	row := h.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(emails_verified), 0),
		       COALESCE(SUM(emails_suppressed), 0),
		       COALESCE(SUM(emails_deduped), 0),
		       COALESCE(SUM(files_processed), 0)
		FROM data_pipeline_runs`)
	row.Scan(&s.TotalRuns, &s.TotalVerified, &s.TotalSuppressed, &s.TotalDeduped, &s.TotalFiles)

	var lastAt sql.NullTime
	var lastStatus sql.NullString
	h.db.QueryRowContext(ctx,
		`SELECT started_at, status FROM data_pipeline_runs ORDER BY started_at DESC LIMIT 1`,
	).Scan(&lastAt, &lastStatus)
	if lastAt.Valid {
		ts := lastAt.Time.Format(time.RFC3339)
		s.LastRunAt = &ts
	}
	if lastStatus.Valid {
		s.LastRunStatus = lastStatus.String
	}

	h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM data_pipeline_files WHERE status = 'available'`,
	).Scan(&s.FilesAvailable)
	h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM data_pipeline_files WHERE status = 'completed'`,
	).Scan(&s.FilesProcessed)

	h.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(emails_verified), 0),
		       COALESCE(SUM(emails_suppressed), 0),
		       COALESCE(SUM(emails_deduped), 0),
		       COALESCE(SUM(files_processed), 0)
		FROM data_pipeline_runs
		WHERE started_at >= CURRENT_DATE`,
	).Scan(&s.Today.Verified, &s.Today.Suppressed, &s.Today.Deduped, &s.Today.Files)

	h.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(emails_verified), 0),
		       COALESCE(SUM(emails_suppressed), 0),
		       COALESCE(SUM(emails_deduped), 0),
		       COALESCE(SUM(files_processed), 0)
		FROM data_pipeline_runs
		WHERE started_at >= CURRENT_DATE - INTERVAL '1 day' AND started_at < CURRENT_DATE`,
	).Scan(&s.Yesterday.Verified, &s.Yesterday.Suppressed, &s.Yesterday.Deduped, &s.Yesterday.Files)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s)
}

// HandleGetPipelineRuns returns a paginated list of pipeline runs.
func (h *DataPipelineHandlers) HandleGetPipelineRuns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit := 20

	rows, err := h.db.QueryContext(ctx, `
		SELECT id::text, started_at, completed_at, status,
		       files_processed, emails_total, emails_verified,
		       emails_suppressed, emails_deduped, error_message,
		       notification_sent
		FROM data_pipeline_runs
		ORDER BY started_at DESC
		LIMIT $1`, limit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	type run struct {
		ID              string  `json:"id"`
		StartedAt       string  `json:"started_at"`
		CompletedAt     *string `json:"completed_at"`
		Status          string  `json:"status"`
		FilesProcessed  int     `json:"files_processed"`
		EmailsTotal     int     `json:"emails_total"`
		EmailsVerified  int     `json:"emails_verified"`
		EmailsSuppressed int    `json:"emails_suppressed"`
		EmailsDeduped   int     `json:"emails_deduped"`
		ErrorMessage    *string `json:"error_message"`
		NotificationSent bool   `json:"notification_sent"`
	}

	var runs []run
	for rows.Next() {
		var r run
		var startedAt time.Time
		var completedAt sql.NullTime
		var errMsg sql.NullString
		err := rows.Scan(&r.ID, &startedAt, &completedAt, &r.Status,
			&r.FilesProcessed, &r.EmailsTotal, &r.EmailsVerified,
			&r.EmailsSuppressed, &r.EmailsDeduped, &errMsg, &r.NotificationSent)
		if err != nil {
			continue
		}
		r.StartedAt = startedAt.Format(time.RFC3339)
		if completedAt.Valid {
			ts := completedAt.Time.Format(time.RFC3339)
			r.CompletedAt = &ts
		}
		if errMsg.Valid {
			r.ErrorMessage = &errMsg.String
		}
		runs = append(runs, r)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"runs":        runs,
		"api_version": VersionDataPipeline,
	})
}

// HandleGetDomainHealth returns per-domain/ISP list health with deficit info.
func (h *DataPipelineHandlers) HandleGetDomainHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := h.db.QueryContext(ctx, `
		SELECT dl.sending_domain, dl.isp, dl.list_id::text, l.name,
		       COALESCE((SELECT COUNT(*) FROM mailing_subscribers s WHERE s.list_id = dl.list_id AND s.status = 'confirmed'), 0) AS subscriber_count,
		       COALESCE(pf.files_available, 0) AS files_available
		FROM data_pipeline_domain_lists dl
		JOIN mailing_lists l ON l.id = dl.list_id
		LEFT JOIN LATERAL (
			SELECT COUNT(*) AS files_available FROM data_pipeline_files
			WHERE isp_normalized = dl.isp AND status = 'available'
		) pf ON true
		ORDER BY dl.sending_domain, dl.isp`)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	type domainHealth struct {
		SendingDomain   string `json:"sending_domain"`
		ISP             string `json:"isp"`
		ListID          string `json:"list_id"`
		ListName        string `json:"list_name"`
		SubscriberCount int    `json:"subscriber_count"`
		FilesAvailable  int    `json:"files_available"`
	}

	var results []domainHealth
	for rows.Next() {
		var d domainHealth
		rows.Scan(&d.SendingDomain, &d.ISP, &d.ListID, &d.ListName, &d.SubscriberCount, &d.FilesAvailable)
		results = append(results, d)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"domain_health": results,
		"api_version":   VersionDataPipeline,
	})
}

// HandleGetPipelineChart returns daily aggregates for the last 30 days.
func (h *DataPipelineHandlers) HandleGetPipelineChart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := h.db.QueryContext(ctx, `
		SELECT DATE(started_at) AS day,
		       COALESCE(SUM(emails_verified), 0),
		       COALESCE(SUM(emails_suppressed), 0),
		       COALESCE(SUM(emails_deduped), 0),
		       COALESCE(SUM(files_processed), 0),
		       COUNT(*)
		FROM data_pipeline_runs
		WHERE started_at >= CURRENT_DATE - INTERVAL '30 days'
		GROUP BY DATE(started_at)
		ORDER BY day ASC`)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	type dayData struct {
		Date       string `json:"date"`
		Verified   int    `json:"verified"`
		Suppressed int    `json:"suppressed"`
		Deduped    int    `json:"deduped"`
		Files      int    `json:"files"`
		Runs       int    `json:"runs"`
	}

	var data []dayData
	for rows.Next() {
		var d dayData
		var day time.Time
		rows.Scan(&day, &d.Verified, &d.Suppressed, &d.Deduped, &d.Files, &d.Runs)
		d.Date = day.Format("2006-01-02")
		data = append(data, d)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"chart_data":  data,
		"api_version": VersionDataPipeline,
	})
}

// HandleTriggerPipeline manually triggers a pipeline run.
func (h *DataPipelineHandlers) HandleTriggerPipeline(w http.ResponseWriter, r *http.Request) {
	p := h.getPipeline()
	if p == nil {
		http.Error(w, `{"error":"pipeline not initialized"}`, 500)
		return
	}

	go func() {
		log.Println("[DataPipeline] manual trigger via API")
		p.RunOnce(context.Background())
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "triggered",
		"message": "Pipeline run started in background",
	})
}

// HandleValidateExisting triggers EO validation for subscribers loaded without
// prior verification. Pass ?since=2026-04-07 to scope by creation date.
func (h *DataPipelineHandlers) HandleValidateExisting(w http.ResponseWriter, r *http.Request) {
	p := h.getPipeline()
	if p == nil {
		http.Error(w, `{"error":"pipeline not initialized"}`, 500)
		return
	}

	sinceStr := r.URL.Query().Get("since")
	if sinceStr == "" {
		http.Error(w, `{"error":"missing required query parameter: since (YYYY-MM-DD)"}`, 400)
		return
	}
	since, err := time.Parse("2006-01-02", sinceStr)
	if err != nil {
		http.Error(w, `{"error":"invalid since format, expected YYYY-MM-DD"}`, 400)
		return
	}

	go func() {
		log.Printf("[DataPipeline] manual EO validation trigger via API (since=%s)", sinceStr)
		p.ValidateExistingSubscribers(context.Background(), since)
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "triggered",
		"message": "EO validation of existing subscribers started in background",
		"since":   sinceStr,
	})
}
