package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// JourneyBuilder handles visual journey workflow management
type JourneyBuilder struct {
	db        *sql.DB
	mailingSvc *MailingService
}

// NewJourneyBuilder creates a new journey builder service
func NewJourneyBuilder(db *sql.DB, mailingSvc *MailingService) *JourneyBuilder {
	return &JourneyBuilder{
		db:        db,
		mailingSvc: mailingSvc,
	}
}

// Journey represents a visual email journey
type Journey struct {
	ID          string                 `json:"id"`
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Status      string                 `json:"status"` // draft, active, paused, completed
	Nodes       []JourneyNode          `json:"nodes"`
	Connections []JourneyConnection    `json:"connections"`
	CreatedAt   time.Time              `json:"createdAt"`
	UpdatedAt   time.Time              `json:"updatedAt"`
	Stats       *JourneyStats          `json:"stats,omitempty"`
}

// JourneyNode represents a node in the journey canvas
type JourneyNode struct {
	ID          string                 `json:"id"`
	Type        string                 `json:"type"` // trigger, email, delay, condition, split, goal
	Position    Position               `json:"position"`
	Config      map[string]interface{} `json:"config"`
	Connections []string               `json:"connections"`
}

// Position represents x,y coordinates on the canvas
type Position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// JourneyConnection represents a connection between nodes
type JourneyConnection struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label,omitempty"`
}

// JourneyStats holds journey performance stats
type JourneyStats struct {
	Entered   int `json:"entered"`
	Completed int `json:"completed"`
	Active    int `json:"active"`
	Converted int `json:"converted"`
}

// RegisterRoutes registers journey builder routes
func (jb *JourneyBuilder) RegisterRoutes(r chi.Router) {
	r.Get("/journeys", jb.HandleGetJourneys)
	r.Post("/journeys", jb.HandleCreateJourney)
	r.Get("/journeys/{journeyId}", jb.HandleGetJourney)
	r.Put("/journeys/{journeyId}", jb.HandleUpdateJourney)
	r.Delete("/journeys/{journeyId}", jb.HandleDeleteJourney)
	r.Post("/journeys/{journeyId}/activate", jb.HandleActivateJourney)
	r.Post("/journeys/{journeyId}/pause", jb.HandlePauseJourney)
	r.Get("/journeys/{journeyId}/stats", jb.HandleGetJourneyStats)
	r.Get("/journeys/{journeyId}/enrollments", jb.HandleGetEnrollments)
}

// HandleGetJourneys returns all journeys
func (jb *JourneyBuilder) HandleGetJourneys(w http.ResponseWriter, r *http.Request) {
	rows, err := jb.db.QueryContext(r.Context(), `
		SELECT id, name, description, status, nodes, connections, created_at, updated_at
		FROM mailing_journeys
		ORDER BY updated_at DESC
	`)
	if err != nil {
		// Return empty list if table doesn't exist
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"journeys": []Journey{}})
		return
	}
	defer rows.Close()

	journeys := []Journey{}
	for rows.Next() {
		var j Journey
		var nodesJSON, connectionsJSON sql.NullString
		var description sql.NullString
		err := rows.Scan(&j.ID, &j.Name, &description, &j.Status, &nodesJSON, &connectionsJSON, &j.CreatedAt, &j.UpdatedAt)
		if err != nil {
			continue
		}
		j.Description = description.String
		
		if nodesJSON.Valid {
			json.Unmarshal([]byte(nodesJSON.String), &j.Nodes)
		}
		if connectionsJSON.Valid {
			json.Unmarshal([]byte(connectionsJSON.String), &j.Connections)
		}
		
		// Get stats
		j.Stats = jb.getJourneyStats(r.Context(), j.ID)
		
		journeys = append(journeys, j)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"journeys": journeys})
}

// HandleCreateJourney creates a new journey
func (jb *JourneyBuilder) HandleCreateJourney(w http.ResponseWriter, r *http.Request) {
	var input Journey
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	// Generate ID if not provided
	if input.ID == "" {
		input.ID = fmt.Sprintf("journey-%s", uuid.New().String()[:8])
	}
	if input.Status == "" {
		input.Status = "draft"
	}

	nodesJSON, _ := json.Marshal(input.Nodes)
	connectionsJSON, _ := json.Marshal(input.Connections)

	_, err := jb.db.ExecContext(r.Context(), `
		INSERT INTO mailing_journeys (id, name, description, status, nodes, connections, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
	`, input.ID, input.Name, input.Description, input.Status, string(nodesJSON), string(connectionsJSON))

	if err != nil {
		// Try to create table if it doesn't exist
		jb.ensureTable(r.Context())
		_, err = jb.db.ExecContext(r.Context(), `
			INSERT INTO mailing_journeys (id, name, description, status, nodes, connections, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
		`, input.ID, input.Name, input.Description, input.Status, string(nodesJSON), string(connectionsJSON))
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"failed to create journey: %v"}`, err), http.StatusInternalServerError)
			return
		}
	}

	input.CreatedAt = time.Now()
	input.UpdatedAt = time.Now()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(input)
}

// HandleGetJourney returns a single journey
func (jb *JourneyBuilder) HandleGetJourney(w http.ResponseWriter, r *http.Request) {
	journeyID := chi.URLParam(r, "journeyId")

	var j Journey
	var nodesJSON, connectionsJSON sql.NullString
	var description sql.NullString
	err := jb.db.QueryRowContext(r.Context(), `
		SELECT id, name, description, status, nodes, connections, created_at, updated_at
		FROM mailing_journeys WHERE id = $1
	`, journeyID).Scan(&j.ID, &j.Name, &description, &j.Status, &nodesJSON, &connectionsJSON, &j.CreatedAt, &j.UpdatedAt)

	if err != nil {
		http.Error(w, `{"error":"journey not found"}`, http.StatusNotFound)
		return
	}
	j.Description = description.String

	if nodesJSON.Valid {
		json.Unmarshal([]byte(nodesJSON.String), &j.Nodes)
	}
	if connectionsJSON.Valid {
		json.Unmarshal([]byte(connectionsJSON.String), &j.Connections)
	}
	
	j.Stats = jb.getJourneyStats(r.Context(), j.ID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(j)
}

// HandleUpdateJourney updates a journey
func (jb *JourneyBuilder) HandleUpdateJourney(w http.ResponseWriter, r *http.Request) {
	journeyID := chi.URLParam(r, "journeyId")

	var input Journey
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	nodesJSON, _ := json.Marshal(input.Nodes)
	connectionsJSON, _ := json.Marshal(input.Connections)

	_, err := jb.db.ExecContext(r.Context(), `
		UPDATE mailing_journeys
		SET name = $2, description = $3, status = $4, nodes = $5, connections = $6, updated_at = NOW()
		WHERE id = $1
	`, journeyID, input.Name, input.Description, input.Status, string(nodesJSON), string(connectionsJSON))

	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"failed to update journey: %v"}`, err), http.StatusInternalServerError)
		return
	}

	input.ID = journeyID
	input.UpdatedAt = time.Now()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(input)
}

// HandleDeleteJourney deletes a journey
func (jb *JourneyBuilder) HandleDeleteJourney(w http.ResponseWriter, r *http.Request) {
	journeyID := chi.URLParam(r, "journeyId")

	_, err := jb.db.ExecContext(r.Context(), `DELETE FROM mailing_journeys WHERE id = $1`, journeyID)
	if err != nil {
		http.Error(w, `{"error":"failed to delete journey"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// HandleActivateJourney activates a journey
func (jb *JourneyBuilder) HandleActivateJourney(w http.ResponseWriter, r *http.Request) {
	journeyID := chi.URLParam(r, "journeyId")

	_, err := jb.db.ExecContext(r.Context(), `
		UPDATE mailing_journeys SET status = 'active', updated_at = NOW() WHERE id = $1
	`, journeyID)

	if err != nil {
		http.Error(w, `{"error":"failed to activate journey"}`, http.StatusInternalServerError)
		return
	}

	// Start the journey processing (enroll subscribers from trigger)
	go jb.processJourneyStart(journeyID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"status":  "active",
		"message": "Journey activated. Subscribers will be enrolled based on trigger settings.",
	})
}

// HandlePauseJourney pauses a journey
func (jb *JourneyBuilder) HandlePauseJourney(w http.ResponseWriter, r *http.Request) {
	journeyID := chi.URLParam(r, "journeyId")

	_, err := jb.db.ExecContext(r.Context(), `
		UPDATE mailing_journeys SET status = 'paused', updated_at = NOW() WHERE id = $1
	`, journeyID)

	if err != nil {
		http.Error(w, `{"error":"failed to pause journey"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"status":  "paused",
	})
}

// HandleGetJourneyStats returns stats for a journey
func (jb *JourneyBuilder) HandleGetJourneyStats(w http.ResponseWriter, r *http.Request) {
	journeyID := chi.URLParam(r, "journeyId")
	stats := jb.getJourneyStats(r.Context(), journeyID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// HandleGetEnrollments returns subscribers enrolled in a journey
func (jb *JourneyBuilder) HandleGetEnrollments(w http.ResponseWriter, r *http.Request) {
	journeyID := chi.URLParam(r, "journeyId")

	rows, err := jb.db.QueryContext(r.Context(), `
		SELECT je.id, je.subscriber_email, je.current_node_id, je.status, je.enrolled_at, je.completed_at
		FROM mailing_journey_enrollments je
		WHERE je.journey_id = $1
		ORDER BY je.enrolled_at DESC
		LIMIT 100
	`, journeyID)

	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"enrollments": []interface{}{}})
		return
	}
	defer rows.Close()

	enrollments := []map[string]interface{}{}
	for rows.Next() {
		var id, email, nodeID, status string
		var enrolledAt time.Time
		var completedAt sql.NullTime
		err := rows.Scan(&id, &email, &nodeID, &status, &enrolledAt, &completedAt)
		if err != nil {
			continue
		}
		enrollment := map[string]interface{}{
			"id":             id,
			"email":          email,
			"current_node":   nodeID,
			"status":         status,
			"enrolled_at":    enrolledAt,
		}
		if completedAt.Valid {
			enrollment["completed_at"] = completedAt.Time
		}
		enrollments = append(enrollments, enrollment)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"enrollments": enrollments})
}

// Helper functions

func (jb *JourneyBuilder) getJourneyStats(ctx interface{}, journeyID string) *JourneyStats {
	stats := &JourneyStats{}
	
	// Count enrollments by status
	jb.db.QueryRow(`
		SELECT 
			COUNT(*) FILTER (WHERE status = 'active'),
			COUNT(*) FILTER (WHERE status = 'completed'),
			COUNT(*) FILTER (WHERE status = 'converted'),
			COUNT(*)
		FROM mailing_journey_enrollments
		WHERE journey_id = $1
	`, journeyID).Scan(&stats.Active, &stats.Completed, &stats.Converted, &stats.Entered)
	
	return stats
}

func (jb *JourneyBuilder) ensureTable(ctx interface{}) {
	jb.db.Exec(`
		CREATE TABLE IF NOT EXISTS mailing_journeys (
			id VARCHAR(100) PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			description TEXT,
			status VARCHAR(50) DEFAULT 'draft',
			nodes JSONB DEFAULT '[]',
			connections JSONB DEFAULT '[]',
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW()
		);
		
		CREATE TABLE IF NOT EXISTS mailing_journey_enrollments (
			id VARCHAR(100) PRIMARY KEY,
			journey_id VARCHAR(100) REFERENCES mailing_journeys(id),
			subscriber_email VARCHAR(255) NOT NULL,
			current_node_id VARCHAR(100),
			status VARCHAR(50) DEFAULT 'active',
			enrolled_at TIMESTAMPTZ DEFAULT NOW(),
			completed_at TIMESTAMPTZ,
			UNIQUE(journey_id, subscriber_email)
		);
		
		CREATE INDEX IF NOT EXISTS idx_journey_enrollments_journey ON mailing_journey_enrollments(journey_id);
		CREATE INDEX IF NOT EXISTS idx_journey_enrollments_email ON mailing_journey_enrollments(subscriber_email);
	`)
}

func (jb *JourneyBuilder) processJourneyStart(journeyID string) {
	// Get journey details
	var nodesJSON string
	var listID sql.NullString
	err := jb.db.QueryRow(`
		SELECT nodes FROM mailing_journeys WHERE id = $1
	`, journeyID).Scan(&nodesJSON)
	if err != nil {
		return
	}

	var nodes []JourneyNode
	json.Unmarshal([]byte(nodesJSON), &nodes)

	// Find trigger node and get list ID
	for _, node := range nodes {
		if node.Type == "trigger" {
			if lid, ok := node.Config["listId"].(string); ok {
				listID.String = lid
				listID.Valid = true
			}
			break
		}
	}

	if !listID.Valid {
		return
	}

	// Get subscribers from the list (confirmed = active subscribers)
	rows, err := jb.db.Query(`
		SELECT email FROM mailing_subscribers WHERE list_id = $1 AND status = 'confirmed'
	`, listID.String)
	if err != nil {
		return
	}
	defer rows.Close()

	// Enroll each subscriber
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			continue
		}

		// Find first node after trigger
		firstNodeID := ""
		for _, node := range nodes {
			if node.Type != "trigger" {
				firstNodeID = node.ID
				break
			}
		}

		// Insert enrollment
		enrollmentID := fmt.Sprintf("enroll-%s", uuid.New().String()[:8])
		jb.db.Exec(`
			INSERT INTO mailing_journey_enrollments (id, journey_id, subscriber_email, current_node_id, status, enrolled_at)
			VALUES ($1, $2, $3, $4, 'active', NOW())
			ON CONFLICT (journey_id, subscriber_email) DO NOTHING
		`, enrollmentID, journeyID, email, firstNodeID)
	}
}
