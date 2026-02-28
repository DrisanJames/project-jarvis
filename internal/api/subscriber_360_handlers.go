package api

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/datanorm"
)

// Subscriber360Handler provides the 360-degree subscriber view.
type Subscriber360Handler struct {
	db          *sql.DB
	eventWriter *datanorm.EventWriter
}

func NewSubscriber360Handler(db *sql.DB, ew *datanorm.EventWriter) *Subscriber360Handler {
	return &Subscriber360Handler{db: db, eventWriter: ew}
}

type subscriber360Response struct {
	Profile         *subscriberProfile     `json:"profile"`
	Events          []eventEntry           `json:"events"`
	EngagementChart []engagementDayData    `json:"engagement_chart"`
	Quality         *qualityInfo           `json:"quality"`
}

type subscriberProfile struct {
	ID                string     `json:"id"`
	Email             string     `json:"email"`
	FirstName         string     `json:"first_name"`
	LastName          string     `json:"last_name"`
	Status            string     `json:"status"`
	DataQualityScore  float64    `json:"data_quality_score"`
	DataSource        string     `json:"data_source"`
	VerificationStatus string   `json:"verification_status"`
	CreatedAt         time.Time  `json:"created_at"`
}

type eventEntry struct {
	ID        int64                  `json:"id"`
	EventType string                 `json:"event_type"`
	Source    string                 `json:"source"`
	Metadata  map[string]interface{} `json:"metadata"`
	EventAt   time.Time              `json:"event_at"`
}

type engagementDayData struct {
	Date      string `json:"date"`
	Opens     int    `json:"opens"`
	Clicks    int    `json:"clicks"`
	PageViews int    `json:"page_views"`
}

type qualityInfo struct {
	Score              float64 `json:"score"`
	VerificationStatus string  `json:"verification_status"`
	DataSource         string  `json:"data_source"`
	TotalEvents        int     `json:"total_events"`
}

// HandleGet360 handles GET /api/v1/subscribers/{email}/360
func (h *Subscriber360Handler) HandleGet360(w http.ResponseWriter, r *http.Request) {
	email := chi.URLParam(r, "email")
	if email == "" {
		http.Error(w, "email required", http.StatusBadRequest)
		return
	}

	emailHash := fmt.Sprintf("%x", sha256.Sum256([]byte(email)))
	ctx := r.Context()

	// 1. Profile
	var profile subscriberProfile
	var dataSource, verStatus sql.NullString
	err := h.db.QueryRowContext(ctx,
		`SELECT id, email, COALESCE(first_name,''), COALESCE(last_name,''), status,
		        COALESCE(data_quality_score,0), data_source, verification_status, created_at
		FROM mailing_subscribers WHERE email = $1 LIMIT 1`, email,
	).Scan(&profile.ID, &profile.Email, &profile.FirstName, &profile.LastName,
		&profile.Status, &profile.DataQualityScore, &dataSource, &verStatus, &profile.CreatedAt)
	if err != nil {
		http.Error(w, "subscriber not found", http.StatusNotFound)
		return
	}
	if dataSource.Valid {
		profile.DataSource = dataSource.String
	}
	if verStatus.Valid {
		profile.VerificationStatus = verStatus.String
	}

	// 2. Recent events
	events, _ := h.eventWriter.QueryByEmail(ctx, emailHash, 100, 0)
	var eventEntries []eventEntry
	for _, e := range events {
		eventEntries = append(eventEntries, eventEntry{
			ID:        e.ID,
			EventType: e.EventType,
			Source:    e.Source,
			Metadata:  e.Metadata,
			EventAt:   e.EventAt,
		})
	}

	// 3. Engagement chart (7-day aggregation)
	chartRows, err := h.db.QueryContext(ctx,
		`SELECT date_trunc('day', event_at)::date as day, event_type, COUNT(*)
		FROM subscriber_events
		WHERE email_hash = $1 AND event_at >= NOW() - INTERVAL '7 days'
		GROUP BY day, event_type
		ORDER BY day`, emailHash)
	var chart []engagementDayData
	if err == nil {
		defer chartRows.Close()
		dayMap := make(map[string]*engagementDayData)
		for chartRows.Next() {
			var day time.Time
			var eventType string
			var count int
			if err := chartRows.Scan(&day, &eventType, &count); err != nil {
				continue
			}
			dateStr := day.Format("2006-01-02")
			if _, ok := dayMap[dateStr]; !ok {
				dayMap[dateStr] = &engagementDayData{Date: dateStr}
			}
			switch eventType {
			case "open":
				dayMap[dateStr].Opens += count
			case "click":
				dayMap[dateStr].Clicks += count
			case "page_view":
				dayMap[dateStr].PageViews += count
			}
		}
		for _, v := range dayMap {
			chart = append(chart, *v)
		}
	}

	// 4. Quality info
	var totalEvents int
	h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM subscriber_events WHERE email_hash = $1`, emailHash).Scan(&totalEvents)

	resp := subscriber360Response{
		Profile:         &profile,
		Events:          eventEntries,
		EngagementChart: chart,
		Quality: &qualityInfo{
			Score:              profile.DataQualityScore,
			VerificationStatus: profile.VerificationStatus,
			DataSource:         profile.DataSource,
			TotalEvents:        totalEvents,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
