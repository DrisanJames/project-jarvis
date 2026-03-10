package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// HandleListStrategies returns all domain strategies for the org.
func (a *EmailMarketingAgent) HandleListStrategies(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT id::text, sending_domain, strategy, params::text, created_at, updated_at
		 FROM agent_domain_strategies WHERE organization_id = $1 ORDER BY sending_domain`, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	type strategyEntry struct {
		ID            string                 `json:"id"`
		SendingDomain string                 `json:"sending_domain"`
		Strategy      string                 `json:"strategy"`
		Params        map[string]interface{} `json:"params"`
		CreatedAt     time.Time              `json:"created_at"`
		UpdatedAt     time.Time              `json:"updated_at"`
	}
	var strategies []strategyEntry
	for rows.Next() {
		var s strategyEntry
		var paramsStr string
		rows.Scan(&s.ID, &s.SendingDomain, &s.Strategy, &paramsStr, &s.CreatedAt, &s.UpdatedAt)
		s.Params = map[string]interface{}{}
		json.Unmarshal([]byte(paramsStr), &s.Params)
		strategies = append(strategies, s)
	}
	if strategies == nil {
		strategies = []strategyEntry{}
	}
	respondJSON(w, http.StatusOK, strategies)
}

// HandleSaveStrategy creates or upserts a domain strategy.
func (a *EmailMarketingAgent) HandleSaveStrategy(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)
	var input struct {
		SendingDomain string                 `json:"sending_domain"`
		Strategy      string                 `json:"strategy"`
		Params        map[string]interface{} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	input.SendingDomain = strings.TrimSpace(input.SendingDomain)
	input.Strategy = strings.TrimSpace(input.Strategy)
	if input.SendingDomain == "" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "sending_domain is required"})
		return
	}
	if input.Strategy != "warmup" && input.Strategy != "performance" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "strategy must be 'warmup' or 'performance'"})
		return
	}
	if input.Params == nil {
		input.Params = map[string]interface{}{}
	}
	paramsJSON, _ := json.Marshal(input.Params)

	var id string
	err := a.db.QueryRowContext(r.Context(),
		`INSERT INTO agent_domain_strategies (organization_id, sending_domain, strategy, params)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (organization_id, sending_domain) DO UPDATE SET strategy=EXCLUDED.strategy, params=EXCLUDED.params, updated_at=NOW()
		 RETURNING id::text`,
		orgID, input.SendingDomain, input.Strategy, string(paramsJSON)).Scan(&id)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"id": id, "sending_domain": input.SendingDomain, "strategy": input.Strategy, "params": input.Params,
	})
}

// HandleUpdateStrategy updates an existing strategy by ID.
func (a *EmailMarketingAgent) HandleUpdateStrategy(w http.ResponseWriter, r *http.Request) {
	stratID := chi.URLParam(r, "id")
	orgID := getOrgID(r)
	var input struct {
		Strategy string                 `json:"strategy"`
		Params   map[string]interface{} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if input.Strategy != "warmup" && input.Strategy != "performance" {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "strategy must be 'warmup' or 'performance'"})
		return
	}
	paramsJSON, _ := json.Marshal(input.Params)
	result, err := a.db.ExecContext(r.Context(),
		`UPDATE agent_domain_strategies SET strategy = $1, params = $2, updated_at = NOW()
		 WHERE id = $3 AND organization_id = $4`,
		input.Strategy, string(paramsJSON), stratID, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "strategy not found"})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"status": "updated"})
}

// HandleDeleteStrategy deletes a strategy by ID.
func (a *EmailMarketingAgent) HandleDeleteStrategy(w http.ResponseWriter, r *http.Request) {
	stratID := chi.URLParam(r, "id")
	orgID := getOrgID(r)
	result, err := a.db.ExecContext(r.Context(),
		`DELETE FROM agent_domain_strategies WHERE id = $1 AND organization_id = $2`, stratID, orgID)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		respondJSON(w, http.StatusNotFound, map[string]string{"error": "strategy not found"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
