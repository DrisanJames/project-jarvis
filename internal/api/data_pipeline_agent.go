package api

// data_pipeline_agent.go — the pipeline agent's log, shown on Data Center →
// Pipeline (screen v2, 2026-11-10):
//
//	POST /api/mailing/data-pipeline/agent   X-Admin-Key ONLY — append entries
//	GET  /api/mailing/data-pipeline/agent   session — latest entries + header
//
// STORAGE: data_pipeline_agent_log (cmd/server/main.go runStartupMigrations),
// append-only, org-scoped. The header answers "is the agent running": the last
// `check` entry's time and status, minutes since it, stale when that is over
// pipelineAgentStaleMinutes (or no check was ever posted), and the agent's
// confinements = body.mandate of the newest `report` entry that carries one.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	pipelineAgentStaleMinutes = 45.0
	pipelineAgentLimitDefault = 50
	pipelineAgentLimitMax     = 200
	pipelineAgentMaxEntries   = 100
	pipelineAgentMaxTitle     = 300
	pipelineAgentMaxBody      = 64 << 10
	pipelineAgentDefaultName  = "pipeline-agent"
)

var (
	pipelineAgentKinds    = []string{"check", "decision", "repair", "learned", "alert", "report"}
	pipelineAgentStatuses = []string{"ok", "warn", "fail"}
)

type pipelineAgentEntry struct {
	ID     int64           `json:"id,omitempty"`
	At     string          `json:"at"`
	Kind   string          `json:"kind"`
	Status string          `json:"status"`
	ISP    string          `json:"isp"`
	Title  string          `json:"title"`
	Body   json.RawMessage `json:"body"`
	// Agent names the writer ("pipeline-agent" by default). Stored inside
	// body.agent so the table shape is unchanged; read back from there.
	Agent string `json:"agent"`
}

type pipelineAgentAppendRequest struct {
	Entries []pipelineAgentEntry `json:"entries"`
}

type pipelineAgentAppendResponse struct {
	diMeta
	Appended int `json:"appended"`
}

type pipelineAgentHeader struct {
	LastCheckAt           *string         `json:"last_check_at"`
	Status                *string         `json:"status"`
	MinutesSinceLastCheck *float64        `json:"minutes_since_last_check"`
	Stale                 bool            `json:"stale"`
	StaleAfterMinutes     float64         `json:"stale_after_minutes"`
	Mandate               json.RawMessage `json:"mandate"`
	MandateAt             *string         `json:"mandate_at"`
	// Scorecard: measured from the log over the last 24 hours and 7 days
	// (data_pipeline_agent_scorecard.go).
	Scorecard *pipelineAgentScorecard `json:"scorecard"`
}

type pipelineAgentResponse struct {
	diMeta
	Header  pipelineAgentHeader  `json:"header"`
	Entries []pipelineAgentEntry `json:"entries"`
}

const dataPipelineAgentInsert = `
	INSERT INTO data_pipeline_agent_log (org_id, at, kind, status, isp, title, body)
	VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)`

const dataPipelineAgentListSQL = `
	SELECT id, at, kind, status, isp, title, body::text
	FROM data_pipeline_agent_log
	WHERE org_id = $1
	ORDER BY at DESC, id DESC
	LIMIT $2`

const dataPipelineAgentLastCheckSQL = `
	SELECT at, status
	FROM data_pipeline_agent_log
	WHERE org_id = $1 AND kind = 'check'
	ORDER BY at DESC, id DESC
	LIMIT 1`

const dataPipelineAgentMandateSQL = `
	SELECT at, (body->'mandate')::text
	FROM data_pipeline_agent_log
	WHERE org_id = $1 AND kind = 'report' AND body->'mandate' IS NOT NULL
	ORDER BY at DESC, id DESC
	LIMIT 1`

// validatePipelineAgentEntries normalizes in place; at defaults to now.
func validatePipelineAgentEntries(entries []pipelineAgentEntry, now time.Time) error {
	for i := range entries {
		e := &entries[i]
		e.Kind = strings.ToLower(strings.TrimSpace(e.Kind))
		e.Status = strings.ToLower(strings.TrimSpace(e.Status))
		e.ISP = strings.ToLower(strings.TrimSpace(e.ISP))
		e.Title = strings.TrimSpace(e.Title)
		if !pipelineIn(pipelineAgentKinds, e.Kind) {
			return fmt.Errorf("entries[%d]: kind %q not one of %s", i, e.Kind, strings.Join(pipelineAgentKinds, ", "))
		}
		if !pipelineIn(pipelineAgentStatuses, e.Status) {
			return fmt.Errorf("entries[%d]: status %q not one of %s", i, e.Status, strings.Join(pipelineAgentStatuses, ", "))
		}
		if e.ISP != "" && !pipelineIn(pipelineISPs, e.ISP) {
			return fmt.Errorf("entries[%d]: isp %q not one of %s (or empty)", i, e.ISP, strings.Join(pipelineISPs, ", "))
		}
		if e.Title == "" || len(e.Title) > pipelineAgentMaxTitle {
			return fmt.Errorf("entries[%d]: title is required and at most %d characters", i, pipelineAgentMaxTitle)
		}
		if len(e.Body) == 0 || string(e.Body) == "null" {
			e.Body = json.RawMessage(`{}`)
		}
		if len(e.Body) > pipelineAgentMaxBody {
			return fmt.Errorf("entries[%d]: body larger than %d bytes", i, pipelineAgentMaxBody)
		}
		var obj map[string]interface{}
		if err := json.Unmarshal(e.Body, &obj); err != nil || obj == nil {
			return fmt.Errorf("entries[%d]: body must be a JSON object", i)
		}
		e.Agent = strings.TrimSpace(e.Agent)
		if e.Agent == "" {
			if a, ok := obj["agent"].(string); ok && strings.TrimSpace(a) != "" {
				e.Agent = strings.TrimSpace(a)
			} else {
				e.Agent = pipelineAgentDefaultName
			}
		}
		if len(e.Agent) > 64 {
			return fmt.Errorf("entries[%d]: agent at most 64 characters", i)
		}
		obj["agent"] = e.Agent
		nb, err := json.Marshal(obj)
		if err != nil {
			return fmt.Errorf("entries[%d]: body: %v", i, err)
		}
		e.Body = nb
		if strings.TrimSpace(e.At) == "" {
			e.At = now.UTC().Format(time.RFC3339)
		} else if t, err := time.Parse(time.RFC3339, e.At); err != nil {
			return fmt.Errorf("entries[%d]: at must be RFC3339", i)
		} else {
			e.At = t.UTC().Format(time.RFC3339)
		}
	}
	return nil
}

// HandleAgentAppend appends one or more entries ({"entries":[...]} or a single
// entry object). Append-only: a retried POST appends again.
func (s *DataPipelineService) HandleAgentAppend(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		respondError(w, http.StatusBadRequest, "unreadable body: "+err.Error())
		return
	}
	var req pipelineAgentAppendRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || len(req.Entries) == 0 {
		var single pipelineAgentEntry
		sdec := json.NewDecoder(bytes.NewReader(raw))
		sdec.DisallowUnknownFields()
		if err := sdec.Decode(&single); err != nil || single.Kind == "" {
			respondError(w, http.StatusBadRequest, "body must be {\"entries\":[...]} or one entry object")
			return
		}
		req.Entries = []pipelineAgentEntry{single}
	}
	if len(req.Entries) > pipelineAgentMaxEntries {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("at most %d entries per request", pipelineAgentMaxEntries))
		return
	}
	if err := validatePipelineAgentEntries(req.Entries, s.now()); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	out := pipelineAgentAppendResponse{diMeta: s.meta(r)}
	qctx, cancel := context.WithTimeout(r.Context(), pipelineWriteTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(qctx, nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "agent_tx_failed: "+err.Error())
		return
	}
	defer tx.Rollback() //nolint:errcheck // committed below on success
	for _, e := range req.Entries {
		if _, err := tx.ExecContext(qctx, dataPipelineAgentInsert,
			out.OrgID, e.At, e.Kind, e.Status, e.ISP, e.Title, string(e.Body)); err != nil {
			respondError(w, http.StatusInternalServerError, "agent_write_failed: "+err.Error())
			return
		}
		out.Appended++
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, "agent_commit_failed: "+err.Error())
		return
	}
	out.mark("appended", fieldMeasured)
	respondJSON(w, http.StatusOK, out)
}

// HandleAgentLog — GET /agent?limit=50
func (s *DataPipelineService) HandleAgentLog(w http.ResponseWriter, r *http.Request) {
	limit := pipelineAgentLimitDefault
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > pipelineAgentLimitMax {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("limit must be 1..%d", pipelineAgentLimitMax))
			return
		}
		limit = n
	}
	if s.db == nil {
		respondError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	out := pipelineAgentResponse{
		diMeta:  s.meta(r),
		Entries: []pipelineAgentEntry{},
		Header:  pipelineAgentHeader{Stale: true, StaleAfterMinutes: pipelineAgentStaleMinutes, Mandate: json.RawMessage(`null`)},
	}
	qctx, cancel := context.WithTimeout(r.Context(), pipelineQueryTimeout)
	defer cancel()

	rows, err := s.db.QueryContext(qctx, dataPipelineAgentListSQL, out.OrgID, limit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "agent_query_failed: "+err.Error())
		return
	}
	for rows.Next() {
		var e pipelineAgentEntry
		var at time.Time
		var body string
		if err := rows.Scan(&e.ID, &at, &e.Kind, &e.Status, &e.ISP, &e.Title, &body); err != nil {
			rows.Close()
			respondError(w, http.StatusInternalServerError, "agent_scan_failed: "+err.Error())
			return
		}
		e.At = at.UTC().Format(time.RFC3339)
		e.Body = json.RawMessage(body)
		e.Agent = agentNameFromBody(e.Body)
		out.Entries = append(out.Entries, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		respondError(w, http.StatusInternalServerError, "agent_query_failed: "+err.Error())
		return
	}
	rows.Close()

	var checkAt time.Time
	var status string
	switch err := s.db.QueryRowContext(qctx, dataPipelineAgentLastCheckSQL, out.OrgID).Scan(&checkAt, &status); {
	case err == sql.ErrNoRows:
		// never checked in: stale, header fields null
	case err != nil:
		respondError(w, http.StatusInternalServerError, "agent_check_query_failed: "+err.Error())
		return
	default:
		at := checkAt.UTC().Format(time.RFC3339)
		mins := s.now().Sub(checkAt).Minutes()
		out.Header.LastCheckAt, out.Header.Status, out.Header.MinutesSinceLastCheck = &at, &status, &mins
		out.Header.Stale = mins > pipelineAgentStaleMinutes
	}

	var mandateAt time.Time
	var mandate string
	switch err := s.db.QueryRowContext(qctx, dataPipelineAgentMandateSQL, out.OrgID).Scan(&mandateAt, &mandate); {
	case err == sql.ErrNoRows:
	case err != nil:
		respondError(w, http.StatusInternalServerError, "agent_mandate_query_failed: "+err.Error())
		return
	default:
		at := mandateAt.UTC().Format(time.RFC3339)
		out.Header.Mandate, out.Header.MandateAt = json.RawMessage(mandate), &at
	}
	sc, err := s.agentScorecard(qctx, out.OrgID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "agent_scorecard_query_failed: "+err.Error())
		return
	}
	out.Header.Scorecard = sc
	out.mark("entries", fieldMeasured)
	out.mark("scorecard", fieldDerived)
	out.markAll(fieldDerived, "stale", "minutes_since_last_check")
	respondJSON(w, http.StatusOK, out)
}
