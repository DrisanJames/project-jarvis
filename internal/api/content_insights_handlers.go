package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
)

// ContentInsightsHandler exposes A/B content learning analytics.
type ContentInsightsHandler struct {
	db *sql.DB
}

func NewContentInsightsHandler(db *sql.DB) *ContentInsightsHandler {
	return &ContentInsightsHandler{db: db}
}

type contentPattern struct {
	SubjectStyle  string  `json:"subject_style"`
	LayoutStyle   string  `json:"layout_style"`
	CTAStyle      string  `json:"cta_style"`
	Tone          string  `json:"tone"`
	AvgOpenRate   float64 `json:"avg_open_rate"`
	AvgClickRate  float64 `json:"avg_click_rate"`
	TotalSamples  int     `json:"total_samples"`
	CampaignCount int     `json:"campaign_count"`
	Wins          int     `json:"wins"`
}

// HandleGetLearnings handles GET /api/v1/content-learnings
func (h *ContentInsightsHandler) HandleGetLearnings(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	orgID := "00000000-0000-0000-0000-000000000001"

	rows, err := h.db.QueryContext(r.Context(),
		`SELECT COALESCE(subject_style,''), COALESCE(layout_style,''), COALESCE(cta_style,''), COALESCE(tone,''),
		        AVG(open_rate) as avg_open, AVG(click_rate) as avg_click,
		        SUM(sample_size) as total_samples, COUNT(*) as campaign_count,
		        COUNT(*) FILTER (WHERE is_winner) as wins
		FROM content_learnings
		WHERE organization_id = $1
		GROUP BY subject_style, layout_style, cta_style, tone
		ORDER BY avg_open DESC
		LIMIT $2`, orgID, limit)
	if err != nil {
		http.Error(w, "query error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var patterns []contentPattern
	for rows.Next() {
		var p contentPattern
		if err := rows.Scan(&p.SubjectStyle, &p.LayoutStyle, &p.CTAStyle, &p.Tone,
			&p.AvgOpenRate, &p.AvgClickRate, &p.TotalSamples, &p.CampaignCount, &p.Wins); err != nil {
			continue
		}
		patterns = append(patterns, p)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(patterns)
}

// HandleGetRecommendation handles GET /api/v1/content-learnings/recommend
func (h *ContentInsightsHandler) HandleGetRecommendation(w http.ResponseWriter, r *http.Request) {
	orgID := "00000000-0000-0000-0000-000000000001"

	var p contentPattern
	err := h.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(subject_style,''), COALESCE(layout_style,''), COALESCE(cta_style,''), COALESCE(tone,''),
		        AVG(open_rate), AVG(click_rate), SUM(sample_size), COUNT(*)
		FROM content_learnings
		WHERE organization_id = $1 AND is_winner = TRUE
		GROUP BY subject_style, layout_style, cta_style, tone
		ORDER BY AVG(open_rate) DESC
		LIMIT 1`, orgID,
	).Scan(&p.SubjectStyle, &p.LayoutStyle, &p.CTAStyle, &p.Tone,
		&p.AvgOpenRate, &p.AvgClickRate, &p.TotalSamples, &p.CampaignCount)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"message":"no learnings yet"}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(p)
}
