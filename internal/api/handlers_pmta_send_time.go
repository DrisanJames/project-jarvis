package api

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// PMTASendTimeHandler exposes send-time recommendation endpoints
// for the PMTA campaign wizard.
type PMTASendTimeHandler struct {
	svc *mailing.AISendTimeService
}

// NewPMTASendTimeHandler creates the handler.
func NewPMTASendTimeHandler(db *sql.DB) *PMTASendTimeHandler {
	return &PMTASendTimeHandler{svc: mailing.NewAISendTimeService(db)}
}

// RegisterRoutes mounts the send-time routes under /pmta-campaign.
func (h *PMTASendTimeHandler) RegisterRoutes(r chi.Router) {
	r.Get("/pmta-campaign/send-time-recommendations", h.HandleGetRecommendations)
}

// HandleGetRecommendations returns per-ISP send-time windows.
// Query params: isps (comma-separated ISP names).
func (h *PMTASendTimeHandler) HandleGetRecommendations(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	ispsParam := r.URL.Query().Get("isps")
	if ispsParam == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "isps query parameter is required"})
		return
	}

	targetISPs := strings.Split(ispsParam, ",")
	for i := range targetISPs {
		targetISPs[i] = strings.TrimSpace(targetISPs[i])
	}

	recs, err := h.svc.GetISPRecommendations(r.Context(), orgID, targetISPs)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"recommendations": recs,
	})
}
