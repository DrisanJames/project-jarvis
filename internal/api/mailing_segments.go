package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// VersionMailingSegmentsAPIv1 bumps whenever the v1 segment response shape
// changes. Frontend consumers (PMTACampaignWizard, ListPortal SegmentsManager)
// echo this in their console logs so production drift is observable.
const VersionMailingSegmentsAPIv1 = "1.1.0"

// validSegmentCategories mirrors the enum seeded by runStartupMigrations
// (may29_seg_backfill_*). Keep in sync with web/src/components/mailing/
// components/segCategoryMetadata.ts and the backfill regexes in main.go.
var validSegmentCategories = map[string]bool{
	"engagement_brand":      true,
	"engagement_global":     true,
	"engagement_isp":        true,
	"framework":             true,
	"funnel":                true,
	"cohort_static":         true,
	"suppression_exclusion": true,
	"partner_wave_static":   true,
	"legacy_snapshot":       true,
	"uncategorized":         true,
}

func (s *AdvancedMailingService) HandleGetSegments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgIDFromRequest(r)

	empty := map[string]interface{}{
		"segments":    []map[string]interface{}{},
		"api_version": VersionMailingSegmentsAPIv1,
	}
	if orgID == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(empty)
		return
	}

	// Optional filters: ?category=engagement_brand&status=active&include_archived=true
	// (default behaviour excludes archived to keep the wizard picker clean —
	// the dashboard view passes include_archived=true so operators can see them.)
	q := r.URL.Query()
	categories := q["category"] // ?category=foo&category=bar -> repeated
	statuses := q["status"]
	includeArchived := q.Get("include_archived") == "true" || q.Get("include_archived") == "1"

	clauses := []string{"s.organization_id = $1"}
	args := []interface{}{orgID}
	argN := 2

	if len(categories) > 0 {
		filtered := make([]string, 0, len(categories))
		for _, c := range categories {
			c = strings.TrimSpace(c)
			if c != "" && validSegmentCategories[c] {
				filtered = append(filtered, c)
			}
		}
		if len(filtered) > 0 {
			clauses = append(clauses, fmt.Sprintf("s.category = ANY($%d)", argN))
			args = append(args, pqStringArray(filtered))
			argN++
		}
	}

	if len(statuses) > 0 {
		clauses = append(clauses, fmt.Sprintf("s.status = ANY($%d)", argN))
		args = append(args, pqStringArray(statuses))
		argN++
	} else if !includeArchived {
		clauses = append(clauses, "s.status <> 'archived'")
	}

	query := `
		SELECT s.id, s.name, s.description, s.segment_type, s.subscriber_count, s.status, s.created_at,
		       s.list_id, COALESCE(l.name, ''), COALESCE(s.conditions::text, '[]'),
		       s.updated_at, s.last_calculated_at,
		       COALESCE(s.category, 'uncategorized')
		FROM mailing_segments s
		LEFT JOIN mailing_lists l ON l.id = s.list_id
		WHERE ` + strings.Join(clauses, " AND ") + `
		ORDER BY s.created_at DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		log.Printf("[HandleGetSegments] query error: %v", err)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(empty)
		return
	}
	defer rows.Close()

	var segments []map[string]interface{}
	for rows.Next() {
		var id uuid.UUID
		var listID *uuid.UUID
		var name, desc, segType, status, listName, conditionsRaw, category string
		var subCount int
		var createdAt time.Time
		var updatedAt, lastCalculatedAt sql.NullTime
		rows.Scan(&id, &name, &desc, &segType, &subCount, &status, &createdAt, &listID, &listName, &conditionsRaw, &updatedAt, &lastCalculatedAt, &category)

		seg := map[string]interface{}{
			"id": id.String(), "name": name, "description": desc, "segment_type": segType,
			"subscriber_count": subCount, "status": status, "created_at": createdAt,
			"list_name": listName, "category": category,
		}
		if updatedAt.Valid {
			seg["updated_at"] = updatedAt.Time
		}
		if lastCalculatedAt.Valid {
			seg["last_calculated_at"] = lastCalculatedAt.Time
		}
		if listID != nil {
			seg["list_id"] = listID.String()
		}
		var conditions []interface{}
		if err := json.Unmarshal([]byte(conditionsRaw), &conditions); err == nil && len(conditions) > 0 {
			seg["conditions"] = conditions
		}
		segments = append(segments, seg)
	}
	if segments == nil {
		segments = []map[string]interface{}{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"segments":    segments,
		"api_version": VersionMailingSegmentsAPIv1,
		"filters_applied": map[string]interface{}{
			"category":         categories,
			"status":           statuses,
			"include_archived": includeArchived,
		},
	})
}

// pqStringArray converts a Go []string into the format lib/pq expects for
// PostgreSQL TEXT[] parameters in ANY($1) clauses. Kept inline (instead of
// importing pq.Array) to match the style of the surrounding handlers in
// this file which avoid the pq.Array wrapper.
func pqStringArray(items []string) interface{} {
	// lib/pq is already imported transitively via database/sql driver
	// registration. Use the literal {a,b,c} representation since we
	// control the input set (categories validated, statuses are short
	// trusted strings).
	escaped := make([]string, 0, len(items))
	for _, it := range items {
		// guard against quote-injection by stripping quotes & backslashes;
		// the legitimate values here are always safe identifiers.
		clean := strings.NewReplacer(`"`, ``, `\`, ``, `,`, ``).Replace(it)
		escaped = append(escaped, `"`+clean+`"`)
	}
	return "{" + strings.Join(escaped, ",") + "}"
}

func (s *AdvancedMailingService) HandleCreateSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		ListID      string `json:"list_id"`
		SegmentType string `json:"segment_type"`
		Category    string `json:"category"`
		Conditions  []struct {
			Group    int    `json:"group"`
			Field    string `json:"field"`
			Operator string `json:"operator"`
			Value    string `json:"value"`
		} `json:"conditions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		log.Printf("Error decoding request body: %v", err)
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	
	if input.Name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}
	
	segmentID := uuid.New()
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
		return
	}
	
	// Handle optional list_id - use NULL if not provided
	var listID interface{}
	if input.ListID != "" {
		parsedListID, err := uuid.Parse(input.ListID)
		if err != nil {
			http.Error(w, `{"error":"invalid list_id"}`, http.StatusBadRequest)
			return
		}
		listID = parsedListID
	} else {
		listID = nil
	}
	
	// Default segment type to dynamic if not specified
	segmentType := input.SegmentType
	if segmentType == "" {
		segmentType = "dynamic"
	}
	
	// Build conditions JSONB for storage in the main table
	conditionsJSON := buildConditionsJSON(input.Conditions)

	// Validate category against the canonical enum; default to 'uncategorized'
	// when the caller omits the field or sends an unknown value. The wizard's
	// "Create Segment" form should always send an explicit category — older
	// API clients (and create_isp_segments.py) won't, which is fine.
	category := input.Category
	if !validSegmentCategories[category] {
		category = "uncategorized"
	}

	log.Printf("Creating segment: id=%s, org=%s, list=%v, name=%s, type=%s, category=%s, conditions=%s",
		segmentID, orgID, listID, input.Name, segmentType, category, conditionsJSON)

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mailing_segments (id, organization_id, list_id, name, description, segment_type, category, conditions, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'active', NOW(), NOW())
	`, segmentID, orgID, listID, input.Name, input.Description, segmentType, category, conditionsJSON)
	
	if err != nil {
		log.Printf("ERROR creating segment - SQL error: %v", err)
		log.Printf("  - segmentID: %s", segmentID)
		log.Printf("  - orgID: %s", orgID)
		log.Printf("  - listID: %v (type: %T)", listID, listID)
		log.Printf("  - name: %s", input.Name)
		log.Printf("  - description: %s", input.Description)
		log.Printf("  - segmentType: %s", segmentType)
		log.Printf("  - conditionsJSON: %s", conditionsJSON)
		http.Error(w, `{"error":"Failed to create segment"}`, http.StatusInternalServerError)
		return
	}
	
	log.Printf("Successfully created segment: %s", segmentID)
	
	// Also add to segment_conditions table for compatibility (with valid operators only)
	if segmentType == "dynamic" && len(input.Conditions) > 0 {
		for i, c := range input.Conditions {
			// Map operators to database-allowed values
			operator := mapOperatorForDB(c.Operator)
			if operator == "" {
				continue // Skip operators not supported by the conditions table
			}
			
			_, condErr := s.db.ExecContext(ctx, `
				INSERT INTO mailing_segment_conditions (id, segment_id, condition_group, field, operator, value)
				VALUES ($1, $2, $3, $4, $5, $6)
			`, uuid.New(), segmentID, i, c.Field, operator, c.Value)
			if condErr != nil {
				log.Printf("Error adding condition to table: %v", condErr)
				// Don't fail the whole request - conditions are also stored in JSONB
			}
		}
	}
	
	// Calculate the actual subscriber count for this segment
	subscriberCount := s.calculateSegmentCount(ctx, segmentID, listID, input.Conditions)
	
	// Update the segment with the calculated count
	s.db.ExecContext(ctx, `UPDATE mailing_segments SET subscriber_count = $2, updated_at = NOW() WHERE id = $1`, segmentID, subscriberCount)
	
	log.Printf("Segment %s created with %d subscribers", segmentID, subscriberCount)

	if segmentType == "dynamic" {
		listIDStr := ""
		if input.ListID != "" {
			listIDStr = input.ListID
		}
		db := s.db
		sid := segmentID.String()
		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			if count, err := MaterializeSegment(bgCtx, db, sid, listIDStr, conditionsJSON); err != nil {
				log.Printf("[CreateSegment] failed to hydrate segment %s: %v", sid, err)
			} else {
				log.Printf("[CreateSegment] hydrated segment %s with %d members", sid, count)
			}
		}()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":               segmentID.String(),
		"name":             input.Name,
		"segment_type":     segmentType,
		"category":         category,
		"status":           "active",
		"subscriber_count": subscriberCount,
		"api_version":      VersionMailingSegmentsAPIv1,
	})
}

// buildConditionsJSON converts input conditions to JSONB format for storage
func buildConditionsJSON(conditions []struct {
	Group    int    `json:"group"`
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}) string {
	if len(conditions) == 0 {
		return "[]"
	}
	
	// Build a structured conditions array
	result := make([]map[string]interface{}, len(conditions))
	for i, c := range conditions {
		result[i] = map[string]interface{}{
			"group":    c.Group,
			"field":    c.Field,
			"operator": c.Operator,
			"value":    c.Value,
		}
	}
	
	jsonBytes, err := json.Marshal(result)
	if err != nil {
		return "[]"
	}
	return string(jsonBytes)
}

// mapOperatorForDB maps frontend operator names to database-allowed values
// Returns empty string if the operator isn't supported by the conditions table CHECK constraint
func mapOperatorForDB(op string) string {
	// These are the operators allowed by the CHECK constraint:
	// ('equals', 'not_equals', 'contains', 'not_contains', 'starts_with', 'ends_with', 'gt', 'lt', 'gte', 'lte', 'is_null', 'is_not_null', 'in', 'not_in')
	mapping := map[string]string{
		"equals":                 "equals",
		"not_equals":             "not_equals",
		"contains":               "contains",
		"not_contains":           "not_contains",
		"starts_with":            "starts_with",
		"ends_with":              "ends_with",
		"greater_than":           "gt",
		"less_than":              "lt",
		"greater_than_or_equal":  "gte",
		"less_than_or_equal":     "lte",
		"gt":                     "gt",
		"lt":                     "lt",
		"gte":                    "gte",
		"lte":                    "lte",
		"is_null":                "is_null",
		"is_not_null":            "is_not_null",
		"is_empty":               "is_null",      // Map to is_null
		"is_not_empty":           "is_not_null",  // Map to is_not_null
		"in":                     "in",
		"not_in":                 "not_in",
	}
	if mapped, ok := mapping[op]; ok {
		return mapped
	}
	// Operators like 'in_last_days', 'more_than_days_ago' aren't in the constraint
	// They're stored in JSONB but skipped for the conditions table
	return ""
}

// SegmentConditionInput is the input shape for segment conditions from JSON.
type SegmentConditionInput struct {
	Group    int    `json:"group"`
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}

// trackingEventTypeMap maps frontend event field names to DB event_type values.
var trackingEventTypeMap = map[string]string{
	"email_sent":         "sent",
	"email_opened":       "opened",
	"email_clicked":      "clicked",
	"email_bounced":      "bounced",
	"email_delivered":    "delivered",
	"email_unsubscribed": "unsubscribed",
	"email_complained":   "complained",
}

// isEventField returns true if the field maps to mailing_tracking_events.
func isEventField(field string) bool {
	_, ok := trackingEventTypeMap[field]
	return ok
}

// BuildSegmentWhereClause builds a SQL WHERE clause from segment conditions.
// Returns the clause string (without "WHERE") and positional args.
// Callers can prefix their own SELECT and append this.
//
// If a condition has field="sending_domain", it is extracted and used to scope
// all event subqueries and time-relative subscriber fields (last_open_at,
// last_click_at) through mailing_tracking_events.sending_domain.
func BuildSegmentWhereClause(listID interface{}, conditions []SegmentConditionInput) (string, []interface{}) {
	// Master List Migration P7: every segment, whether list-scoped or
	// master-list-wide, must exclude globally hard-bounced and complained
	// subscribers. These two columns were added to mailing_subscribers in
	// P1 precisely so selection code has a cheap, denormalized check
	// instead of re-joining mailing_global_suppressions on every query.
	// Keeping this in the shared WHERE builder means the segment
	// materializer, the segment preview UI, and any live segment query
	// all behave identically.
	whereClauses := []string{
		"status IN ('active','confirmed')",
		"hard_bounced_at IS NULL",
		"complained_at IS NULL",
		// Honeypot-flagged scanners are never mailable. Applied at the
		// shared WHERE layer so the materializer, segment preview, and
		// live segment-driven sends all agree.
		"is_bot = false",
	}
	args := []interface{}{}
	argNum := 1

	if listID != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("list_id = $%d", argNum))
		args = append(args, listID)
		argNum++
	}

	// Extract sending_domain filter if present — used to scope event subqueries
	var domainFilter string
	var filtered []SegmentConditionInput
	for _, c := range conditions {
		if c.Field == "sending_domain" {
			domainFilter = c.Value
		} else {
			filtered = append(filtered, c)
		}
	}

	// Fail-closed guard: a segment whose only condition is sending_domain has
	// no event/click/engagement field for the domain scope to attach to. The
	// loop below would emit no domain-scoped predicate, leaving only the base
	// subscriber gates — which match every active subscriber in the org. That
	// silently turns the segment into an org-wide blast list. Refuse to
	// compile by returning a FALSE sentinel; the materializer logs 0 members
	// and the operator gets a visible signal in the UI / next deploy.
	if domainFilter != "" && len(filtered) == 0 {
		log.Printf("[BuildSegmentWhereClause] refusing sending_domain-only segment (domainFilter=%q): no engagement condition to scope. Returning FALSE sentinel.", domainFilter)
		return "FALSE", nil
	}

	for _, c := range filtered {
		// Exclude subscribers whose email appears in lists matching a name pattern
		if c.Field == "exclude_list_pattern" {
			clause := fmt.Sprintf(`NOT EXISTS (
				SELECT 1 FROM mailing_subscribers s_excl
				JOIN mailing_lists l_excl ON s_excl.list_id = l_excl.id
				WHERE LOWER(s_excl.email) = LOWER(mailing_subscribers.email)
				  AND l_excl.name ILIKE $%d
			)`, argNum)
			whereClauses = append(whereClauses, clause)
			args = append(args, "%"+c.Value+"%")
			argNum++
			continue
		}

		if c.Field == "email_clicked_url" {
			clause, newArgs, newArgNum := buildClickedURLWhereClause(c, argNum, domainFilter)
			if clause != "" {
				whereClauses = append(whereClauses, clause)
				args = append(args, newArgs...)
				argNum = newArgNum
			}
			continue
		}

		if isEventField(c.Field) {
			clause, newArgs, newArgNum := buildEventWhereClause(c, argNum, domainFilter)
			if clause != "" {
				whereClauses = append(whereClauses, clause)
				args = append(args, newArgs...)
				argNum = newArgNum
			}
			continue
		}

		// When sending_domain is set, convert subscriber-level engagement
		// fields into domain-scoped event subqueries instead of using the
		// global denormalized column on mailing_subscribers.
		if domainFilter != "" && isDomainScopableField(c.Field, c.Operator) {
			clause, newArgs, newArgNum := buildDomainScopedFieldClause(c, argNum, domainFilter)
			if clause != "" {
				whereClauses = append(whereClauses, clause)
				args = append(args, newArgs...)
				argNum = newArgNum
			}
			continue
		}

		dbField := mapFieldToColumn(c.Field)
		var clause string
		switch c.Operator {
		case "equals", "is":
			clause = fmt.Sprintf("%s = $%d", dbField, argNum)
			args = append(args, c.Value)
			argNum++
		case "not_equals", "is_not":
			clause = fmt.Sprintf("%s != $%d", dbField, argNum)
			args = append(args, c.Value)
			argNum++
		case "contains":
			if c.Field == "email" && strings.HasPrefix(c.Value, "@") {
				clause, newArgs, newArgNum := expandISPEmailContains(dbField, c.Value, argNum)
				args = append(args, newArgs...)
				argNum = newArgNum
				if clause != "" {
					whereClauses = append(whereClauses, clause)
				}
				continue
			}
			clause = fmt.Sprintf("%s ILIKE $%d", dbField, argNum)
			args = append(args, "%"+c.Value+"%")
			argNum++
		case "not_contains":
			clause = fmt.Sprintf("%s NOT ILIKE $%d", dbField, argNum)
			args = append(args, "%"+c.Value+"%")
			argNum++
		case "starts_with":
			clause = fmt.Sprintf("%s ILIKE $%d", dbField, argNum)
			args = append(args, c.Value+"%")
			argNum++
		case "ends_with":
			clause = fmt.Sprintf("%s ILIKE $%d", dbField, argNum)
			args = append(args, "%"+c.Value)
			argNum++
		case "greater_than", "gt":
			clause = fmt.Sprintf("%s > $%d", dbField, argNum)
			args = append(args, c.Value)
			argNum++
		case "less_than", "lt":
			clause = fmt.Sprintf("%s < $%d", dbField, argNum)
			args = append(args, c.Value)
			argNum++
		case "greater_than_or_equal", "gte":
			clause = fmt.Sprintf("%s >= $%d", dbField, argNum)
			args = append(args, c.Value)
			argNum++
		case "less_than_or_equal", "lte":
			clause = fmt.Sprintf("%s <= $%d", dbField, argNum)
			args = append(args, c.Value)
			argNum++
		case "is_empty", "is_null":
			clause = fmt.Sprintf("(%s IS NULL OR %s = '')", dbField, dbField)
		case "is_not_empty", "is_not_null":
			clause = fmt.Sprintf("(%s IS NOT NULL AND %s != '')", dbField, dbField)
		case "in_last_days", "within_last":
			clause = fmt.Sprintf("%s >= NOW() - INTERVAL '%s days'", dbField, c.Value)
		case "more_than_days_ago":
			clause = fmt.Sprintf("%s < NOW() - INTERVAL '%s days'", dbField, c.Value)
		default:
			continue
		}
		if clause != "" {
			whereClauses = append(whereClauses, clause)
		}
	}

	return strings.Join(whereClauses, " AND "), args
}

// isDomainScopableField returns true for subscriber fields that should be
// converted to domain-scoped event subqueries when sending_domain is set.
func isDomainScopableField(field, operator string) bool {
	if field != "last_open_at" && field != "last_click_at" {
		return false
	}
	switch operator {
	case "in_last_days", "within_last", "more_than_days_ago":
		return true
	}
	return false
}

// buildDomainScopedFieldClause converts last_open_at/last_click_at subscriber
// fields into EXISTS subqueries against mailing_tracking_events filtered by
// sending_domain, so engagement is scoped to a specific sending domain.
func buildDomainScopedFieldClause(c SegmentConditionInput, argNum int, domain string) (string, []interface{}, int) {
	eventType := "opened"
	if c.Field == "last_click_at" {
		eventType = "clicked"
	}

	switch c.Operator {
	case "in_last_days", "within_last":
		clause := fmt.Sprintf(`EXISTS (
			SELECT 1 FROM mailing_tracking_events e
			WHERE e.subscriber_id = mailing_subscribers.id
			  AND e.event_type = $%d
			  AND e.event_at >= NOW() - INTERVAL '%s days'
			  AND (e.sending_domain = $%d OR e.sending_domain LIKE '%%.' || $%d)
		)`, argNum, c.Value, argNum+1, argNum+1)
		return clause, []interface{}{eventType, domain}, argNum + 2
	case "more_than_days_ago":
		clause := fmt.Sprintf(`NOT EXISTS (
			SELECT 1 FROM mailing_tracking_events e
			WHERE e.subscriber_id = mailing_subscribers.id
			  AND e.event_type = $%d
			  AND e.event_at >= NOW() - INTERVAL '%s days'
			  AND (e.sending_domain = $%d OR e.sending_domain LIKE '%%.' || $%d)
		)`, argNum, c.Value, argNum+1, argNum+1)
		return clause, []interface{}{eventType, domain}, argNum + 2
	}
	return "", nil, argNum
}

// buildEventWhereClause generates an EXISTS/NOT EXISTS subquery against
// mailing_tracking_events for event-based segment conditions.
// domainFilter, when non-empty, adds AND e.sending_domain = $N to scope by sending domain.
func buildEventWhereClause(c SegmentConditionInput, argNum int, domainFilter string) (string, []interface{}, int) {
	eventType := trackingEventTypeMap[c.Field]
	var args []interface{}

	domainClause := ""
	addDomain := func() {
		if domainFilter != "" {
			domainClause = fmt.Sprintf(" AND (e.sending_domain = $%d OR e.sending_domain LIKE '%%.' || $%d)", argNum+1, argNum+1)
			args = append(args, domainFilter)
		}
	}

	switch c.Operator {
	case "equals", "is":
		args = append(args, eventType)
		addDomain()
		clause := fmt.Sprintf(`EXISTS (
			SELECT 1 FROM mailing_tracking_events e
			WHERE e.subscriber_id = mailing_subscribers.id
			  AND e.event_type = $%d%s
		)`, argNum, domainClause)
		return clause, args, argNum + len(args)

	case "not_equals", "is_not":
		args = append(args, eventType)
		addDomain()
		clause := fmt.Sprintf(`NOT EXISTS (
			SELECT 1 FROM mailing_tracking_events e
			WHERE e.subscriber_id = mailing_subscribers.id
			  AND e.event_type = $%d%s
		)`, argNum, domainClause)
		return clause, args, argNum + len(args)

	case "in_last_days", "within_last":
		args = append(args, eventType)
		addDomain()
		clause := fmt.Sprintf(`EXISTS (
			SELECT 1 FROM mailing_tracking_events e
			WHERE e.subscriber_id = mailing_subscribers.id
			  AND e.event_type = $%d
			  AND e.event_at >= NOW() - INTERVAL '%s days'%s
		)`, argNum, c.Value, domainClause)
		return clause, args, argNum + len(args)

	case "not_in_last_days", "more_than_days_ago":
		args = append(args, eventType)
		addDomain()
		clause := fmt.Sprintf(`NOT EXISTS (
			SELECT 1 FROM mailing_tracking_events e
			WHERE e.subscriber_id = mailing_subscribers.id
			  AND e.event_type = $%d
			  AND e.event_at >= NOW() - INTERVAL '%s days'%s
		)`, argNum, c.Value, domainClause)
		return clause, args, argNum + len(args)

	default:
		return "", nil, argNum
	}
}

// buildClickedURLWhereClause generates an EXISTS subquery against
// mailing_tracking_events for click events filtered by link_url pattern.
func buildClickedURLWhereClause(c SegmentConditionInput, argNum int, domainFilter string) (string, []interface{}, int) {
	var args []interface{}
	args = append(args, "%"+c.Value+"%")

	domainClause := ""
	if domainFilter != "" {
		domainClause = fmt.Sprintf(
			" AND (e.sending_domain = $%d OR e.sending_domain LIKE '%%.' || $%d)",
			argNum+len(args), argNum+len(args),
		)
		args = append(args, domainFilter)
	}

	switch c.Operator {
	case "contains", "equals":
		clause := fmt.Sprintf(`EXISTS (
			SELECT 1 FROM mailing_tracking_events e
			WHERE e.subscriber_id = mailing_subscribers.id
			  AND e.event_type = 'clicked'
			  AND e.link_url ILIKE $%d%s
		)`, argNum, domainClause)
		return clause, args, argNum + len(args)

	case "not_contains":
		clause := fmt.Sprintf(`NOT EXISTS (
			SELECT 1 FROM mailing_tracking_events e
			WHERE e.subscriber_id = mailing_subscribers.id
			  AND e.event_type = 'clicked'
			  AND e.link_url ILIKE $%d%s
		)`, argNum, domainClause)
		return clause, args, argNum + len(args)

	default:
		return "", nil, argNum
	}
}

// BuildSegmentSubscriberQuery returns a SELECT query for subscriber id+email
// matching a segment's conditions. Used by deploy-time pre-computation.
func BuildSegmentSubscriberQuery(listID interface{}, conditions []SegmentConditionInput) (string, []interface{}) {
	where, args := BuildSegmentWhereClause(listID, conditions)
	return fmt.Sprintf("SELECT id::text, email FROM mailing_subscribers WHERE %s", where), args
}

// calculateSegmentCount calculates the actual subscriber count for a segment based on its conditions
func (s *AdvancedMailingService) calculateSegmentCount(ctx context.Context, segmentID uuid.UUID, listID interface{}, conditions []struct {
	Group    int    `json:"group"`
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}) int {
	converted := make([]SegmentConditionInput, len(conditions))
	for i, c := range conditions {
		converted[i] = SegmentConditionInput{Group: c.Group, Field: c.Field, Operator: c.Operator, Value: c.Value}
	}
	where, args := BuildSegmentWhereClause(listID, converted)
	query := fmt.Sprintf("SELECT COUNT(DISTINCT LOWER(email)) FROM mailing_subscribers WHERE %s", where)

	var count int
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&count)
	if err != nil {
		log.Printf("Error calculating segment count: %v (query: %s)", err, query)
		return 0
	}
	return count
}

// expandISPEmailContains generates a WHERE clause fragment that expands a
// single email-domain filter to all sibling domains in the same ISP family.
// Uses isp.DomainSiblings (auto-derived from the canonical domainToISP map).
func expandISPEmailContains(col, value string, argNum int) (string, []interface{}, int) {
	siblings, ok := isp.DomainSiblings[strings.ToLower(value)]
	if !ok || len(siblings) <= 1 {
		return fmt.Sprintf("%s ILIKE $%d", col, argNum),
			[]interface{}{"%" + value + "%"}, argNum + 1
	}
	var parts []string
	var args []interface{}
	for _, dom := range siblings {
		parts = append(parts, fmt.Sprintf("%s ILIKE $%d", col, argNum))
		args = append(args, "%"+dom+"%")
		argNum++
	}
	return "(" + strings.Join(parts, " OR ") + ")", args, argNum
}

// mapFieldToColumn maps frontend field names to database column names
func mapFieldToColumn(field string) string {
	// Direct column mappings
	columnMap := map[string]string{
		"email":                  "email",
		"first_name":             "first_name",
		"last_name":              "last_name",
		"status":                 "status",
		"isp":                    "isp",
		"engagement_score":       "engagement_score",
		"total_emails_received":  "total_emails_received",
		"total_opens":            "total_opens",
		"total_clicks":           "total_clicks",
		"last_email_at":          "last_email_at",
		"last_open_at":           "last_open_at",
		"last_click_at":          "last_click_at",
		"subscribed_at":          "subscribed_at",
		"created_at":             "created_at",
		"source":                 "source",
		"timezone":               "timezone",
	}
	
	if col, ok := columnMap[field]; ok {
		return col
	}
	
	// Custom fields are stored in JSONB - access them with ->> operator
	if strings.HasPrefix(field, "custom.") {
		jsonKey := strings.TrimPrefix(field, "custom.")
		return fmt.Sprintf("custom_fields->>'%s'", jsonKey)
	}
	
	// For fields like 'city', 'company', etc. that are in custom_fields
	customFieldAliases := map[string]bool{
		"city": true, "state": true, "country": true, "postal_code": true,
		"company": true, "job_title": true, "industry": true, "phone": true,
		"language": true,
	}
	if customFieldAliases[field] {
		return fmt.Sprintf("custom_fields->>'%s'", field)
	}
	
	// Default: assume it's a direct column
	return field
}

func (s *AdvancedMailingService) HandleGetSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	segmentID := chi.URLParam(r, "segmentId")

	var id uuid.UUID
	var listID *uuid.UUID
	var name, desc, segType, status, conditionsRaw string
	var subCount int

	err := s.db.QueryRowContext(ctx, `
		SELECT id, list_id, name, description, segment_type, subscriber_count, status,
		       COALESCE(conditions::text, '[]')
		FROM mailing_segments WHERE id = $1
	`, segmentID).Scan(&id, &listID, &name, &desc, &segType, &subCount, &status, &conditionsRaw)

	if err != nil {
		http.Error(w, `{"error":"segment not found"}`, http.StatusNotFound)
		return
	}

	var conditions []interface{}
	json.Unmarshal([]byte(conditionsRaw), &conditions)

	// Fallback: if JSONB conditions are empty, read from mailing_segment_conditions table
	if len(conditions) == 0 {
		rows, qErr := s.db.QueryContext(ctx, `
			SELECT condition_group, field, operator, value
			FROM mailing_segment_conditions
			WHERE segment_id = $1
			ORDER BY condition_group
		`, segmentID)
		if qErr == nil {
			defer rows.Close()
			for rows.Next() {
				var group int
				var field, operator, value string
				if err := rows.Scan(&group, &field, &operator, &value); err == nil {
					conditions = append(conditions, map[string]interface{}{
						"group": group, "field": field, "operator": operator, "value": value,
					})
				}
			}
		}
	}

	resp := map[string]interface{}{
		"id": id.String(), "name": name, "description": desc,
		"segment_type": segType, "subscriber_count": subCount, "status": status, "conditions": conditions,
	}
	if listID != nil {
		resp["list_id"] = listID.String()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *AdvancedMailingService) HandleUpdateSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	segmentID := chi.URLParam(r, "segmentId")

	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		ListID      string `json:"list_id"`
		Status      string `json:"status"`
		Category    string `json:"category"`
		Conditions  []struct {
			Group    int    `json:"group"`
			Field    string `json:"field"`
			Operator string `json:"operator"`
			Value    string `json:"value"`
		} `json:"conditions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	var listID interface{}
	if input.ListID != "" {
		parsed, err := uuid.Parse(input.ListID)
		if err != nil {
			http.Error(w, `{"error":"invalid list_id"}`, http.StatusBadRequest)
			return
		}
		listID = parsed
	}

	status := input.Status
	if status == "" {
		status = "active"
	}

	conditionsJSON := buildConditionsJSON(input.Conditions)

	// Category is an optional field. When omitted (empty string) the existing
	// row's category is preserved via COALESCE($7, category) so older API
	// callers (notably `.scratch/may29_seed_engagement_segments.py`'s
	// reactivate path and the SegmentsManager edit dialog before frontend
	// catches up to v1.1.0) don't blank out a backfilled category.
	var categoryArg interface{}
	if input.Category != "" && validSegmentCategories[input.Category] {
		categoryArg = input.Category
	}

	_, err := s.db.ExecContext(ctx, `
		UPDATE mailing_segments
		SET name = $2, description = $3, list_id = $4, status = $5,
		    conditions = $6, category = COALESCE($7, category), updated_at = NOW()
		WHERE id = $1
	`, segmentID, input.Name, input.Description, listID, status, conditionsJSON, categoryArg)
	if err != nil {
		log.Printf("[HandleUpdateSegment] update error: %v", err)
		http.Error(w, `{"error":"failed to update segment"}`, http.StatusInternalServerError)
		return
	}

	s.db.ExecContext(ctx, `DELETE FROM mailing_segment_conditions WHERE segment_id = $1`, segmentID)
	for i, c := range input.Conditions {
		operator := mapOperatorForDB(c.Operator)
		if operator == "" {
			continue
		}
		s.db.ExecContext(ctx, `
			INSERT INTO mailing_segment_conditions (id, segment_id, condition_group, field, operator, value)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, uuid.New(), segmentID, i, c.Field, operator, c.Value)
	}

	segUUID, _ := uuid.Parse(segmentID)
	subscriberCount := s.calculateSegmentCount(ctx, segUUID, listID, input.Conditions)
	s.db.ExecContext(ctx, `UPDATE mailing_segments SET subscriber_count = $2 WHERE id = $1`, segmentID, subscriberCount)

	// Surface the resolved category in the response so the wizard can
	// invalidate any client-side cache and re-fetch with the correct badge.
	var resolvedCategory string
	s.db.QueryRowContext(ctx, `SELECT COALESCE(category, 'uncategorized') FROM mailing_segments WHERE id = $1`, segmentID).Scan(&resolvedCategory)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":               segmentID,
		"updated":          true,
		"subscriber_count": subscriberCount,
		"category":         resolvedCategory,
		"api_version":      VersionMailingSegmentsAPIv1,
	})
}

func (s *AdvancedMailingService) HandlePreviewSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	segmentID := chi.URLParam(r, "segmentId")
	segmentUUID, _ := uuid.Parse(segmentID)
	
	// Get segment with conditions from JSONB column
	var listID *uuid.UUID
	var conditionsJSON sql.NullString
	
	err := s.db.QueryRowContext(ctx, `SELECT list_id, conditions FROM mailing_segments WHERE id = $1`, segmentID).Scan(&listID, &conditionsJSON)
	if err != nil {
		http.Error(w, `{"error":"segment not found"}`, http.StatusNotFound)
		return
	}
	
	// Parse conditions from JSONB
	var conditions []struct {
		Group    int    `json:"group"`
		Field    string `json:"field"`
		Operator string `json:"operator"`
		Value    string `json:"value"`
	}
	
	if conditionsJSON.Valid && conditionsJSON.String != "" && conditionsJSON.String != "[]" {
		json.Unmarshal([]byte(conditionsJSON.String), &conditions)
	}

	// Fallback: if JSONB conditions column is empty, read from mailing_segment_conditions table
	if len(conditions) == 0 {
		rows, qErr := s.db.QueryContext(ctx, `
			SELECT condition_group, field, operator, value
			FROM mailing_segment_conditions
			WHERE segment_id = $1
			ORDER BY condition_group
		`, segmentID)
		if qErr == nil {
			defer rows.Close()
			for rows.Next() {
				var c struct {
					Group    int    `json:"group"`
					Field    string `json:"field"`
					Operator string `json:"operator"`
					Value    string `json:"value"`
				}
				if err := rows.Scan(&c.Group, &c.Field, &c.Operator, &c.Value); err == nil {
					conditions = append(conditions, c)
				}
			}
		}
	}

	// Convert *uuid.UUID to interface{} correctly: a nil *uuid.UUID wrapped in
	// interface{} is non-nil, which would cause BuildSegmentWhereClause to add
	// a broken "list_id = NULL" equality clause. Pass a true nil instead.
	var listIDArg interface{}
	if listID != nil {
		listIDArg = *listID
	}

	count := s.calculateSegmentCount(ctx, segmentUUID, listIDArg, conditions)
	
	// Update segment count
	s.db.ExecContext(ctx, `UPDATE mailing_segments SET subscriber_count = $2, updated_at = NOW() WHERE id = $1`, segmentID, count)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"subscriber_count": count,
		"segment_id":       segmentID,
	})
}

func (s *AdvancedMailingService) HandleRefreshAllSegments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, list_id, name, COALESCE(conditions::text, '[]')
		FROM mailing_segments
		WHERE status = 'active' AND segment_type = 'dynamic'
		ORDER BY name
	`)
	if err != nil {
		http.Error(w, `{"error":"query error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type result struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		OldCount int    `json:"old_count"`
		NewCount int    `json:"new_count"`
	}
	var results []result

	for rows.Next() {
		var id uuid.UUID
		var listID *uuid.UUID
		var name, condJSON string
		if err := rows.Scan(&id, &listID, &name, &condJSON); err != nil {
			log.Printf("[SegmentRefresh] scan error: %v", err)
			continue
		}

		var conditions []struct {
			Group    int    `json:"group"`
			Field    string `json:"field"`
			Operator string `json:"operator"`
			Value    string `json:"value"`
		}
		if condJSON != "" && condJSON != "[]" {
			if err := json.Unmarshal([]byte(condJSON), &conditions); err != nil {
				log.Printf("[SegmentRefresh] JSON parse error for %s: %v (json=%s)", name, err, safePrefix(condJSON, 100))
			}
		}
		if len(conditions) == 0 {
			log.Printf("[SegmentRefresh] skipping %s: no conditions (json=%s)", name, condJSON)
			continue
		}

		var listIDArg interface{}
		if listID != nil {
			listIDArg = *listID
		}

		var oldCount int
		s.db.QueryRowContext(ctx, `SELECT COALESCE(subscriber_count,0) FROM mailing_segments WHERE id = $1`, id).Scan(&oldCount)

		newCount := s.calculateSegmentCount(ctx, id, listIDArg, conditions)
		s.db.ExecContext(ctx, `UPDATE mailing_segments SET subscriber_count = $2, last_calculated_at = NOW(), updated_at = NOW() WHERE id = $1`, id, newCount)

		results = append(results, result{ID: id.String(), Name: name, OldCount: oldCount, NewCount: newCount})
		log.Printf("[SegmentRefresh] %s: %d → %d (conditions=%d)", name, oldCount, newCount, len(conditions))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"refreshed": len(results),
		"segments":  results,
	})
}

func (s *AdvancedMailingService) HandleDeleteSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	segmentID := chi.URLParam(r, "segmentId")
	
	s.db.ExecContext(ctx, `DELETE FROM mailing_segments WHERE id = $1`, segmentID)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"deleted": segmentID})
}
