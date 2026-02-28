package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ignite/sparkpost-monitor/internal/kanban"
)

// GetKanbanBoard returns the entire Kanban board
func (h *Handlers) GetKanbanBoard(w http.ResponseWriter, r *http.Request) {
	if h.kanbanService == nil {
		respondError(w, http.StatusServiceUnavailable, "Kanban service not configured")
		return
	}

	board, err := h.kanbanService.GetBoard(r.Context())
	if err != nil {
		log.Printf("ERROR: failed to get kanban board: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve board")
		return
	}

	respondJSON(w, http.StatusOK, board)
}

// UpdateKanbanBoard updates the entire Kanban board (for drag-drop operations)
func (h *Handlers) UpdateKanbanBoard(w http.ResponseWriter, r *http.Request) {
	if h.kanbanService == nil {
		respondError(w, http.StatusServiceUnavailable, "Kanban service not configured")
		return
	}

	var board kanban.KanbanBoard
	if err := json.NewDecoder(r.Body).Decode(&board); err != nil {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("Invalid request body: %v", err))
		return
	}

	if err := h.kanbanService.UpdateBoard(r.Context(), &board); err != nil {
		log.Printf("ERROR: failed to update kanban board: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to update board")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"message":   "Board updated",
		"timestamp": time.Now(),
	})
}

// CreateKanbanCard creates a new card
func (h *Handlers) CreateKanbanCard(w http.ResponseWriter, r *http.Request) {
	if h.kanbanService == nil {
		respondError(w, http.StatusServiceUnavailable, "Kanban service not configured")
		return
	}

	var req kanban.CreateCardRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("Invalid request body: %v", err))
		return
	}

	card, err := h.kanbanService.CreateCard(r.Context(), req)
	if err != nil {
		log.Printf("ERROR: failed to create kanban card: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to create card")
		return
	}

	respondJSON(w, http.StatusCreated, card)
}

// UpdateKanbanCard updates an existing card
func (h *Handlers) UpdateKanbanCard(w http.ResponseWriter, r *http.Request) {
	if h.kanbanService == nil {
		respondError(w, http.StatusServiceUnavailable, "Kanban service not configured")
		return
	}

	cardID := chi.URLParam(r, "cardId")
	if cardID == "" {
		respondError(w, http.StatusBadRequest, "Card ID required")
		return
	}

	var req kanban.UpdateCardRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("Invalid request body: %v", err))
		return
	}

	card, err := h.kanbanService.UpdateCard(r.Context(), cardID, req)
	if err != nil {
		log.Printf("ERROR: failed to update kanban card %s: %v", cardID, err)
		respondError(w, http.StatusInternalServerError, "Failed to update card")
		return
	}

	respondJSON(w, http.StatusOK, card)
}

// MoveKanbanCard moves a card between columns
func (h *Handlers) MoveKanbanCard(w http.ResponseWriter, r *http.Request) {
	if h.kanbanService == nil {
		respondError(w, http.StatusServiceUnavailable, "Kanban service not configured")
		return
	}

	cardID := chi.URLParam(r, "cardId")
	if cardID == "" {
		respondError(w, http.StatusBadRequest, "Card ID required")
		return
	}

	var req kanban.MoveCardRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("Invalid request body: %v", err))
		return
	}
	req.CardID = cardID

	if err := h.kanbanService.MoveCard(r.Context(), req); err != nil {
		log.Printf("ERROR: failed to move kanban card: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to move card")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"message":   "Card moved",
		"timestamp": time.Now(),
	})
}

// CompleteKanbanCard marks a card as complete
func (h *Handlers) CompleteKanbanCard(w http.ResponseWriter, r *http.Request) {
	if h.kanbanService == nil {
		respondError(w, http.StatusServiceUnavailable, "Kanban service not configured")
		return
	}

	cardID := chi.URLParam(r, "cardId")
	if cardID == "" {
		respondError(w, http.StatusBadRequest, "Card ID required")
		return
	}

	if err := h.kanbanService.CompleteCard(r.Context(), cardID); err != nil {
		log.Printf("ERROR: failed to complete kanban card %s: %v", cardID, err)
		respondError(w, http.StatusInternalServerError, "Failed to complete card")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"message":   "Card completed",
		"timestamp": time.Now(),
	})
}

// DeleteKanbanCard deletes a card
func (h *Handlers) DeleteKanbanCard(w http.ResponseWriter, r *http.Request) {
	if h.kanbanService == nil {
		respondError(w, http.StatusServiceUnavailable, "Kanban service not configured")
		return
	}

	cardID := chi.URLParam(r, "cardId")
	if cardID == "" {
		respondError(w, http.StatusBadRequest, "Card ID required")
		return
	}

	if err := h.kanbanService.DeleteCard(r.Context(), cardID); err != nil {
		log.Printf("ERROR: failed to delete kanban card: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to delete card")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"message":   "Card deleted",
		"timestamp": time.Now(),
	})
}

// GetKanbanDueTasks returns tasks that are due or overdue
func (h *Handlers) GetKanbanDueTasks(w http.ResponseWriter, r *http.Request) {
	if h.kanbanService == nil {
		respondError(w, http.StatusServiceUnavailable, "Kanban service not configured")
		return
	}

	dueTasks, err := h.kanbanService.GetDueTasks(r.Context())
	if err != nil {
		log.Printf("ERROR: failed to get due tasks: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve due tasks")
		return
	}

	respondJSON(w, http.StatusOK, dueTasks)
}

// TriggerKanbanAIAnalysis manually triggers AI analysis
func (h *Handlers) TriggerKanbanAIAnalysis(w http.ResponseWriter, r *http.Request) {
	if h.kanbanAIAnalyzer == nil {
		respondError(w, http.StatusServiceUnavailable, "Kanban AI analyzer not configured")
		return
	}

	result, err := h.kanbanAIAnalyzer.ManualTrigger(r.Context())
	if err != nil {
		log.Printf("ERROR: failed to run AI analysis: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to run AI analysis")
		return
	}

	respondJSON(w, http.StatusOK, result)
}

// GetKanbanVelocityReport returns the velocity report for a specific month
func (h *Handlers) GetKanbanVelocityReport(w http.ResponseWriter, r *http.Request) {
	if h.kanbanArchival == nil {
		respondError(w, http.StatusServiceUnavailable, "Kanban archival service not configured")
		return
	}

	month := chi.URLParam(r, "month")
	if month == "" {
		month = time.Now().Format("2006-01")
	}

	// If asking for current month, get live stats
	if month == time.Now().Format("2006-01") {
		stats, err := h.kanbanService.GetCurrentVelocityStats(r.Context())
		if err != nil {
			log.Printf("ERROR: failed to get velocity stats: %v", err)
			respondError(w, http.StatusInternalServerError, "Failed to retrieve stats")
			return
		}
		respondJSON(w, http.StatusOK, stats)
		return
	}

	report, err := h.kanbanArchival.GetVelocityReport(r.Context(), month)
	if err != nil {
		log.Printf("ERROR: failed to get velocity report for %s: %v", month, err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve velocity report")
		return
	}

	if report == nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("No report found for %s", month))
		return
	}

	respondJSON(w, http.StatusOK, report)
}

// GetKanbanCurrentStats returns current month velocity statistics
func (h *Handlers) GetKanbanCurrentStats(w http.ResponseWriter, r *http.Request) {
	if h.kanbanService == nil {
		respondError(w, http.StatusServiceUnavailable, "Kanban service not configured")
		return
	}

	stats, err := h.kanbanService.GetCurrentVelocityStats(r.Context())
	if err != nil {
		log.Printf("ERROR: failed to get current stats: %v", err)
		respondError(w, http.StatusInternalServerError, "Failed to retrieve current stats")
		return
	}

	respondJSON(w, http.StatusOK, stats)
}
