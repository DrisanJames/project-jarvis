package api

// Scheduler-bridge tools for the Campaign Copilot.
//
// The REAL scheduling pipeline (agents/scheduling) runs LOCALLY on the
// operator's machine — the platform cannot execute it. These tools only
// enqueue/read the command-bridge table `mailing_scheduler_commands`
// (DDL in cmd/server/main.go runStartupMigrations; a local poller owned by
// the operator-side tooling claims 'queued' rows, runs the command, and
// writes status/output_tail/exit_code back). All tools are org-scoped and
// touch ONLY the bridge table plus read-only board queries.
//
//   queue_scheduler_command  — INSERT status='queued', whitelisted commands
//                              ONLY: build-send-day, fresh-bcast, promote,
//                              intake-forecast, write-directive. args JSON
//                              passes through verbatim.
//   get_scheduler_command    — one command by id (prefix ok): status,
//                              output_tail, timestamps.
//   list_scheduler_commands  — recent commands, newest first.
//   get_board_state          — read-only: campaigns named '<montok><dd> - %'
//                              for a date, grouped by sending domain.
//
// GET /api/mailing/copilot/commands (HandleListCommands) backs the portal's
// command-status strip with the same table.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// schedulerCommandWhitelist is the EXACT allowed command set. Anything else
// is rejected before touching the database.
var schedulerCommandWhitelist = map[string]bool{
	"build-send-day":  true,
	"fresh-bcast":     true,
	"promote":         true,
	"intake-forecast": true,
	"write-directive": true,
}

func schedulerCommandAllowedList() string {
	return "build-send-day, fresh-bcast, promote, intake-forecast, write-directive"
}

// ─── queue_scheduler_command ─────────────────────────────────────────────────

func (c *CampaignCopilot) toolQueueSchedulerCommand(ctx context.Context, orgID string, args map[string]interface{}) (interface{}, string) {
	command, _ := args["command"].(string)
	command = strings.TrimSpace(command)
	if !schedulerCommandWhitelist[command] {
		return map[string]string{
			"error": fmt.Sprintf("command %q is not allowed — allowed commands: %s", command, schedulerCommandAllowedList()),
		}, ""
	}

	// args object passes through verbatim; default {}.
	cmdArgs := []byte(`{}`)
	if a, ok := args["args"]; ok && a != nil {
		if b, err := json.Marshal(a); err == nil {
			cmdArgs = b
		}
	}

	requestedBy := "copilot"
	if m, ok := ctx.Value(copilotModelCtxKey).(string); ok && m != "" {
		requestedBy = "copilot:" + m
	}

	var id string
	var createdAt time.Time
	err := c.db.QueryRowContext(ctx,
		`INSERT INTO mailing_scheduler_commands (organization_id, command, args, status, requested_by)
		 VALUES ($1, $2, $3, 'queued', $4)
		 RETURNING id::text, created_at`,
		orgID, command, cmdArgs, requestedBy).Scan(&id, &createdAt)
	if err != nil {
		log.Printf("[CampaignCopilot] toolQueueSchedulerCommand insert: %v", err)
		return map[string]string{"error": err.Error()}, ""
	}

	return map[string]interface{}{
		"id":           id,
		"command":      command,
		"status":       "queued",
		"requested_by": requestedBy,
		"created_at":   createdAt.Format(time.RFC3339),
		"note":         "Queued for the operator's local scheduler runner. Expect ~1-15 min before completion — poll get_scheduler_command(id) before summarizing results.",
	}, "Queued scheduler command: " + command
}

// ─── get_scheduler_command ───────────────────────────────────────────────────

func (c *CampaignCopilot) toolGetSchedulerCommand(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	id, _ := args["id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return map[string]string{"error": "id required"}
	}

	row := c.db.QueryRowContext(ctx,
		`SELECT id::text, command, args::text, status, requested_by, output_tail,
		        exit_code, created_at, updated_at
		 FROM mailing_scheduler_commands
		 WHERE organization_id = $1 AND id::text LIKE $2 || '%'
		 ORDER BY created_at DESC
		 LIMIT 1`, orgID, id)

	cmd, err := scanSchedulerCommand(row)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	return cmd
}

// ─── list_scheduler_commands ─────────────────────────────────────────────────

func (c *CampaignCopilot) toolListSchedulerCommands(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	limit := 10
	if l, ok := args["limit"].(float64); ok && int(l) > 0 {
		limit = min(int(l), 50)
	}

	rows, err := c.db.QueryContext(ctx,
		`SELECT id::text, command, args::text, status, requested_by, output_tail,
		        exit_code, created_at, updated_at
		 FROM mailing_scheduler_commands
		 WHERE organization_id = $1
		 ORDER BY created_at DESC
		 LIMIT `+fmt.Sprintf("%d", limit), orgID)
	if err != nil {
		log.Printf("[CampaignCopilot] toolListSchedulerCommands: %v", err)
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()

	commands := []map[string]interface{}{}
	for rows.Next() {
		cmd, err := scanSchedulerCommand(rows)
		if err != nil {
			continue
		}
		commands = append(commands, cmd)
	}
	return map[string]interface{}{"commands": commands, "count": len(commands)}
}

// scanSchedulerCommand reads one bridge-command row (rowScanner is the
// package-wide *sql.Row / *sql.Rows interface from offer_center_lifecycle.go).
func scanSchedulerCommand(row rowScanner) (map[string]interface{}, error) {
	var id, command, argsJSON, status, requestedBy, outputTail string
	var exitCode *int
	var createdAt, updatedAt time.Time
	if err := row.Scan(&id, &command, &argsJSON, &status, &requestedBy, &outputTail,
		&exitCode, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	// Keep the chat context lean: only the last 2000 chars of output.
	const tailMax = 2000
	if len(outputTail) > tailMax {
		outputTail = "…" + outputTail[len(outputTail)-tailMax:]
	}
	out := map[string]interface{}{
		"id":           id,
		"command":      command,
		"args":         json.RawMessage(argsJSON),
		"status":       status,
		"requested_by": requestedBy,
		"output_tail":  outputTail,
		"created_at":   createdAt.Format(time.RFC3339),
		"updated_at":   updatedAt.Format(time.RFC3339),
	}
	if exitCode != nil {
		out["exit_code"] = *exitCode
	}
	return out, nil
}

// ─── get_board_state ─────────────────────────────────────────────────────────

var boardDateTokenRe = regexp.MustCompile(`^[a-z]{3}[0-9]{2}$`)

// boardPrefixForDate maps "2026-07-29" → "jul29" (the board-generator naming
// convention: campaigns are named '<montok><dd> - …'). A pre-formed token
// like "jul29" passes through.
func boardPrefixForDate(date string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(date))
	if boardDateTokenRe.MatchString(d) {
		return d, nil
	}
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return "", fmt.Errorf("date must be YYYY-MM-DD or a token like jul29 (got %q)", date)
	}
	return strings.ToLower(t.Format("Jan")) + t.Format("02"), nil
}

func (c *CampaignCopilot) toolGetBoardState(ctx context.Context, orgID string, args map[string]interface{}) interface{} {
	date, _ := args["date"].(string)
	prefix, err := boardPrefixForDate(date)
	if err != nil {
		return map[string]string{"error": err.Error()}
	}

	rows, err := c.db.QueryContext(ctx,
		`SELECT COALESCE(split_part(from_email, '@', 2), ''), name, status,
		        COALESCE(total_recipients, 0), (offer_id IS NOT NULL)
		 FROM mailing_campaigns
		 WHERE organization_id = $1 AND name LIKE $2
		 ORDER BY from_email, name`,
		orgID, prefix+" - %")
	if err != nil {
		log.Printf("[CampaignCopilot] toolGetBoardState: %v", err)
		return map[string]string{"error": err.Error()}
	}
	defer rows.Close()

	byDomain := map[string][]map[string]interface{}{}
	total := 0
	for rows.Next() {
		var domain, name, status string
		var recipients int
		var hasOffer bool
		if rows.Scan(&domain, &name, &status, &recipients, &hasOffer) != nil {
			continue
		}
		if domain == "" {
			domain = "(no sending domain)"
		}
		byDomain[domain] = append(byDomain[domain], map[string]interface{}{
			"name":             name,
			"status":           status,
			"total_recipients": recipients,
			"has_offer":        hasOffer,
		})
		total++
	}

	return map[string]interface{}{
		"date_prefix":    prefix,
		"campaign_count": total,
		"by_domain":      byDomain,
		"note":           "Campaigns matching '" + prefix + " - %'. has_offer=false means offer attribution (and offer suppression) will NOT fire for that campaign.",
	}
}

// ─── GET /api/mailing/copilot/commands ───────────────────────────────────────

// HandleListCommands backs the portal's command-status strip: the most
// recent bridge commands for the org, newest first.
func (c *CampaignCopilot) HandleListCommands(w http.ResponseWriter, r *http.Request) {
	orgID := getOrgID(r)

	limit := 10
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
		limit = min(l, 50)
	}

	rows, err := c.db.QueryContext(r.Context(),
		`SELECT id::text, command, args::text, status, requested_by, output_tail,
		        exit_code, created_at, updated_at
		 FROM mailing_scheduler_commands
		 WHERE organization_id = $1
		 ORDER BY created_at DESC
		 LIMIT `+fmt.Sprintf("%d", limit), orgID)
	if err != nil {
		log.Printf("[CampaignCopilot] HandleListCommands: %v", err)
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	commands := []map[string]interface{}{}
	for rows.Next() {
		cmd, err := scanSchedulerCommand(rows)
		if err != nil {
			continue
		}
		commands = append(commands, cmd)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"commands": commands})
}
