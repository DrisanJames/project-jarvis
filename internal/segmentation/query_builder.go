package segmentation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// QueryBuilder builds SQL queries from segment conditions
type QueryBuilder struct {
	baseTable         string
	args              []interface{}
	argCounter        int
	includeSupressed  bool
	listID            string
	organizationID    string
	debug             bool
}

// NewQueryBuilder creates a new QueryBuilder
func NewQueryBuilder() *QueryBuilder {
	return &QueryBuilder{
		baseTable:  "mailing_subscribers",
		args:       make([]interface{}, 0),
		argCounter: 1,
	}
}

// SetOrganizationID sets the organization filter
func (qb *QueryBuilder) SetOrganizationID(orgID string) *QueryBuilder {
	qb.organizationID = orgID
	return qb
}

// SetListID sets the list filter
func (qb *QueryBuilder) SetListID(listID string) *QueryBuilder {
	qb.listID = listID
	return qb
}

// SetIncludeSuppressed sets whether to include suppressed subscribers
func (qb *QueryBuilder) SetIncludeSuppressed(include bool) *QueryBuilder {
	qb.includeSupressed = include
	return qb
}

// SetDebug enables debug mode
func (qb *QueryBuilder) SetDebug(debug bool) *QueryBuilder {
	qb.debug = debug
	return qb
}

// nextArg returns the next argument placeholder
func (qb *QueryBuilder) nextArg(value interface{}) string {
	qb.args = append(qb.args, value)
	placeholder := fmt.Sprintf("$%d", qb.argCounter)
	qb.argCounter++
	return placeholder
}

// BuildQuery builds a complete SQL query from a ConditionGroupBuilder
func (qb *QueryBuilder) BuildQuery(group ConditionGroupBuilder, globalExclusions []ConditionBuilder) (string, []interface{}, error) {
	// Reset state
	qb.args = make([]interface{}, 0)
	qb.argCounter = 1

	// Build the main SELECT query
	selectClause := `
		SELECT s.id, s.organization_id, s.list_id, s.email, s.first_name, s.last_name,
			s.status, s.engagement_score, s.total_opens, s.total_clicks,
			s.last_open_at, s.last_click_at, s.optimal_send_hour_utc, s.timezone,
			s.subscribed_at, s.custom_fields, s.tags
		FROM mailing_subscribers s
	`

	// Build JOINs if needed for computed fields
	joins := qb.buildJoins(group)

	// Build WHERE clause
	whereConditions := []string{"1=1"}

	// Add organization filter
	if qb.organizationID != "" {
		whereConditions = append(whereConditions, fmt.Sprintf("s.organization_id = %s", qb.nextArg(qb.organizationID)))
	}

	// Add list filter
	if qb.listID != "" {
		whereConditions = append(whereConditions, fmt.Sprintf("s.list_id = %s", qb.nextArg(qb.listID)))
	}

	// Add status filter (default: confirmed only)
	whereConditions = append(whereConditions, "s.status = 'confirmed'")

	// Add suppression filter
	if !qb.includeSupressed {
		whereConditions = append(whereConditions, `
			NOT EXISTS (
				SELECT 1 FROM mailing_suppression_list sup 
				WHERE sup.organization_id = s.organization_id 
				AND sup.email_hash = s.email_hash
				AND (sup.expires_at IS NULL OR sup.expires_at > NOW())
			)
		`)
	}

	// Build main conditions from the group
	mainCondition, err := qb.buildGroupCondition(group)
	if err != nil {
		return "", nil, err
	}
	if mainCondition != "" {
		whereConditions = append(whereConditions, "("+mainCondition+")")
	}

	// Build global exclusions
	for _, exclusion := range globalExclusions {
		exclusionSQL, err := qb.buildCondition(exclusion)
		if err != nil {
			return "", nil, err
		}
		if exclusionSQL != "" {
			whereConditions = append(whereConditions, "NOT ("+exclusionSQL+")")
		}
	}

	// Assemble query
	query := selectClause
	if joins != "" {
		query += joins
	}
	query += "\nWHERE " + strings.Join(whereConditions, "\n  AND ")
	query += "\nORDER BY s.engagement_score DESC"

	return query, qb.args, nil
}

// BuildCountQuery builds a COUNT query for estimation
func (qb *QueryBuilder) BuildCountQuery(group ConditionGroupBuilder, globalExclusions []ConditionBuilder) (string, []interface{}, error) {
	// Reset state
	qb.args = make([]interface{}, 0)
	qb.argCounter = 1

	selectClause := "SELECT COUNT(*) FROM mailing_subscribers s"

	// Build JOINs if needed
	joins := qb.buildJoins(group)

	// Build WHERE clause
	whereConditions := []string{"1=1"}

	if qb.organizationID != "" {
		whereConditions = append(whereConditions, fmt.Sprintf("s.organization_id = %s", qb.nextArg(qb.organizationID)))
	}

	if qb.listID != "" {
		whereConditions = append(whereConditions, fmt.Sprintf("s.list_id = %s", qb.nextArg(qb.listID)))
	}

	whereConditions = append(whereConditions, "s.status = 'confirmed'")

	if !qb.includeSupressed {
		whereConditions = append(whereConditions, `
			NOT EXISTS (
				SELECT 1 FROM mailing_suppression_list sup 
				WHERE sup.organization_id = s.organization_id 
				AND sup.email_hash = s.email_hash
				AND (sup.expires_at IS NULL OR sup.expires_at > NOW())
			)
		`)
	}

	mainCondition, err := qb.buildGroupCondition(group)
	if err != nil {
		return "", nil, err
	}
	if mainCondition != "" {
		whereConditions = append(whereConditions, "("+mainCondition+")")
	}

	for _, exclusion := range globalExclusions {
		exclusionSQL, err := qb.buildCondition(exclusion)
		if err != nil {
			return "", nil, err
		}
		if exclusionSQL != "" {
			whereConditions = append(whereConditions, "NOT ("+exclusionSQL+")")
		}
	}

	query := selectClause
	if joins != "" {
		query += joins
	}
	query += "\nWHERE " + strings.Join(whereConditions, "\n  AND ")

	return query, qb.args, nil
}

// buildJoins determines what JOINs are needed
func (qb *QueryBuilder) buildJoins(group ConditionGroupBuilder) string {
	joins := []string{}
	
	// Check if we need computed fields
	if qb.needsComputedFields(group) {
		joins = append(joins, `
			LEFT JOIN mailing_subscriber_computed comp ON s.id = comp.subscriber_id
		`)
	}

	return strings.Join(joins, "\n")
}

// needsComputedFields checks if the group uses computed fields
func (qb *QueryBuilder) needsComputedFields(group ConditionGroupBuilder) bool {
	for _, cond := range group.Conditions {
		if cond.ConditionType == ConditionComputed {
			return true
		}
	}
	for _, subGroup := range group.Groups {
		if qb.needsComputedFields(subGroup) {
			return true
		}
	}
	return false
}

// buildGroupCondition builds SQL for a condition group
func (qb *QueryBuilder) buildGroupCondition(group ConditionGroupBuilder) (string, error) {
	parts := []string{}

	// Build individual conditions
	for _, cond := range group.Conditions {
		sql, err := qb.buildCondition(cond)
		if err != nil {
			return "", err
		}
		if sql != "" {
			parts = append(parts, sql)
		}
	}

	// Build nested groups recursively
	for _, subGroup := range group.Groups {
		subSQL, err := qb.buildGroupCondition(subGroup)
		if err != nil {
			return "", err
		}
		if subSQL != "" {
			if subGroup.IsNegated {
				parts = append(parts, "NOT ("+subSQL+")")
			} else {
				parts = append(parts, "("+subSQL+")")
			}
		}
	}

	if len(parts) == 0 {
		return "", nil
	}

	operator := " AND "
	if group.LogicOperator == LogicOr {
		operator = " OR "
	}

	result := strings.Join(parts, operator)
	
	if group.IsNegated {
		result = "NOT (" + result + ")"
	}

	return result, nil
}

// buildCondition builds SQL for a single condition
func (qb *QueryBuilder) buildCondition(cond ConditionBuilder) (string, error) {
	switch cond.ConditionType {
	case ConditionProfile:
		return qb.buildProfileCondition(cond)
	case ConditionCustomField:
		return qb.buildCustomFieldCondition(cond)
	case ConditionEvent:
		return qb.buildEventCondition(cond)
	case ConditionComputed:
		return qb.buildComputedCondition(cond)
	case ConditionTag:
		return qb.buildTagCondition(cond)
	default:
		return qb.buildProfileCondition(cond)
	}
}

// buildProfileCondition builds SQL for profile/attribute conditions
func (qb *QueryBuilder) buildProfileCondition(cond ConditionBuilder) (string, error) {
	field := "s." + cond.Field
	
	switch cond.Operator {
	// String operators
	case OpEquals:
		return fmt.Sprintf("%s = %s", field, qb.nextArg(cond.Value)), nil
	case OpNotEquals:
		return fmt.Sprintf("%s != %s", field, qb.nextArg(cond.Value)), nil
	case OpContains:
		return fmt.Sprintf("%s ILIKE %s", field, qb.nextArg("%"+cond.Value+"%")), nil
	case OpNotContains:
		return fmt.Sprintf("%s NOT ILIKE %s", field, qb.nextArg("%"+cond.Value+"%")), nil
	case OpStartsWith:
		return fmt.Sprintf("%s ILIKE %s", field, qb.nextArg(cond.Value+"%")), nil
	case OpEndsWith:
		return fmt.Sprintf("%s ILIKE %s", field, qb.nextArg("%"+cond.Value)), nil
	case OpIsEmpty:
		return fmt.Sprintf("(%s IS NULL OR %s = '')", field, field), nil
	case OpIsNotEmpty:
		return fmt.Sprintf("(%s IS NOT NULL AND %s != '')", field, field), nil
	case OpMatchesRegex:
		return fmt.Sprintf("%s ~ %s", field, qb.nextArg(cond.Value)), nil

	// Numeric operators
	case OpGt:
		return fmt.Sprintf("%s > %s", field, qb.nextArg(cond.Value)), nil
	case OpGte:
		return fmt.Sprintf("%s >= %s", field, qb.nextArg(cond.Value)), nil
	case OpLt:
		return fmt.Sprintf("%s < %s", field, qb.nextArg(cond.Value)), nil
	case OpLte:
		return fmt.Sprintf("%s <= %s", field, qb.nextArg(cond.Value)), nil
	case OpBetween:
		return fmt.Sprintf("%s BETWEEN %s AND %s", field, qb.nextArg(cond.Value), qb.nextArg(cond.ValueSecondary)), nil
	case OpNotBetween:
		return fmt.Sprintf("%s NOT BETWEEN %s AND %s", field, qb.nextArg(cond.Value), qb.nextArg(cond.ValueSecondary)), nil

	// Date operators
	case OpDateEquals:
		return fmt.Sprintf("DATE(%s) = %s", field, qb.nextArg(cond.Value)), nil
	case OpDateBefore:
		return fmt.Sprintf("%s < %s", field, qb.nextArg(cond.Value)), nil
	case OpDateAfter:
		return fmt.Sprintf("%s > %s", field, qb.nextArg(cond.Value)), nil
	case OpDateBetween:
		return fmt.Sprintf("%s BETWEEN %s AND %s", field, qb.nextArg(cond.Value), qb.nextArg(cond.ValueSecondary)), nil
	case OpInLastDays:
		return fmt.Sprintf("%s >= NOW() - INTERVAL '%s days'", field, cond.Value), nil
	case OpInNextDays:
		return fmt.Sprintf("%s <= NOW() + INTERVAL '%s days' AND %s >= NOW()", field, cond.Value, field), nil
	case OpMoreThanDaysAgo:
		return fmt.Sprintf("%s < NOW() - INTERVAL '%s days'", field, cond.Value), nil
	case OpAnniversaryMonth:
		return fmt.Sprintf("EXTRACT(MONTH FROM %s) = %s", field, qb.nextArg(cond.Value)), nil
	case OpAnniversaryDay:
		// Value format: "MM-DD"
		parts := strings.Split(cond.Value, "-")
		if len(parts) == 2 {
			return fmt.Sprintf("EXTRACT(MONTH FROM %s) = %s AND EXTRACT(DAY FROM %s) = %s", 
				field, qb.nextArg(parts[0]), field, qb.nextArg(parts[1])), nil
		}
		return fmt.Sprintf("TO_CHAR(%s, 'MM-DD') = %s", field, qb.nextArg(cond.Value)), nil

	// Boolean operators
	case OpIsTrue:
		return fmt.Sprintf("%s = TRUE", field), nil
	case OpIsFalse:
		return fmt.Sprintf("%s = FALSE", field), nil

	// NULL checks
	case OpIsNull:
		return fmt.Sprintf("%s IS NULL", field), nil
	case OpIsNotNull:
		return fmt.Sprintf("%s IS NOT NULL", field), nil

	default:
		return "", fmt.Errorf("unsupported operator: %s", cond.Operator)
	}
}

// buildCustomFieldCondition builds SQL for custom JSONB field conditions
func (qb *QueryBuilder) buildCustomFieldCondition(cond ConditionBuilder) (string, error) {
	// Custom fields are stored in s.custom_fields JSONB
	jsonPath := fmt.Sprintf("s.custom_fields->>'%s'", cond.Field)
	
	// For type-specific operations, we need to cast
	switch cond.FieldType {
	case FieldNumber, FieldInteger, FieldDecimal:
		jsonPath = fmt.Sprintf("(s.custom_fields->>'%s')::numeric", cond.Field)
	case FieldBoolean:
		jsonPath = fmt.Sprintf("(s.custom_fields->>'%s')::boolean", cond.Field)
	case FieldDate, FieldDatetime:
		jsonPath = fmt.Sprintf("(s.custom_fields->>'%s')::timestamp", cond.Field)
	}

	switch cond.Operator {
	case OpEquals:
		return fmt.Sprintf("%s = %s", jsonPath, qb.nextArg(cond.Value)), nil
	case OpNotEquals:
		return fmt.Sprintf("%s != %s", jsonPath, qb.nextArg(cond.Value)), nil
	case OpContains:
		return fmt.Sprintf("s.custom_fields->>'%s' ILIKE %s", cond.Field, qb.nextArg("%"+cond.Value+"%")), nil
	case OpNotContains:
		return fmt.Sprintf("s.custom_fields->>'%s' NOT ILIKE %s", cond.Field, qb.nextArg("%"+cond.Value+"%")), nil
	case OpGt:
		return fmt.Sprintf("%s > %s", jsonPath, qb.nextArg(cond.Value)), nil
	case OpGte:
		return fmt.Sprintf("%s >= %s", jsonPath, qb.nextArg(cond.Value)), nil
	case OpLt:
		return fmt.Sprintf("%s < %s", jsonPath, qb.nextArg(cond.Value)), nil
	case OpLte:
		return fmt.Sprintf("%s <= %s", jsonPath, qb.nextArg(cond.Value)), nil
	case OpBetween:
		return fmt.Sprintf("%s BETWEEN %s AND %s", jsonPath, qb.nextArg(cond.Value), qb.nextArg(cond.ValueSecondary)), nil
	case OpIsNull:
		return fmt.Sprintf("(s.custom_fields->>'%s') IS NULL", cond.Field), nil
	case OpIsNotNull:
		return fmt.Sprintf("(s.custom_fields->>'%s') IS NOT NULL", cond.Field), nil
	case OpIsTrue:
		return fmt.Sprintf("(s.custom_fields->>'%s')::boolean = TRUE", cond.Field), nil
	case OpIsFalse:
		return fmt.Sprintf("(s.custom_fields->>'%s')::boolean = FALSE", cond.Field), nil
	case OpInLastDays:
		return fmt.Sprintf("(s.custom_fields->>'%s')::timestamp >= NOW() - INTERVAL '%s days'", cond.Field, cond.Value), nil
	case OpMoreThanDaysAgo:
		return fmt.Sprintf("(s.custom_fields->>'%s')::timestamp < NOW() - INTERVAL '%s days'", cond.Field, cond.Value), nil
	default:
		return "", fmt.Errorf("unsupported operator for custom field: %s", cond.Operator)
	}
}

// buildEventCondition builds SQL for event-based conditions
func (qb *QueryBuilder) buildEventCondition(cond ConditionBuilder) (string, error) {
	eventTable := "mailing_custom_events"
	if cond.EventName == "" {
		// Standard email events use tracking_events table
		eventTable = "mailing_tracking_events"
	}

	// Build the event subquery
	switch cond.Operator {
	case OpEventCountGte:
		timeWindow := ""
		if cond.EventTimeWindowDays > 0 {
			timeWindow = fmt.Sprintf("AND e.event_at >= NOW() - INTERVAL '%d days'", cond.EventTimeWindowDays)
		}
		return fmt.Sprintf(`
			(SELECT COUNT(*) FROM %s e 
			 WHERE e.subscriber_id = s.id 
			 AND e.event_name = %s %s) >= %s`,
			eventTable, qb.nextArg(cond.EventName), timeWindow, qb.nextArg(cond.Value)), nil

	case OpEventCountLte:
		timeWindow := ""
		if cond.EventTimeWindowDays > 0 {
			timeWindow = fmt.Sprintf("AND e.event_at >= NOW() - INTERVAL '%d days'", cond.EventTimeWindowDays)
		}
		return fmt.Sprintf(`
			(SELECT COUNT(*) FROM %s e 
			 WHERE e.subscriber_id = s.id 
			 AND e.event_name = %s %s) <= %s`,
			eventTable, qb.nextArg(cond.EventName), timeWindow, qb.nextArg(cond.Value)), nil

	case OpEventCountBetween:
		timeWindow := ""
		if cond.EventTimeWindowDays > 0 {
			timeWindow = fmt.Sprintf("AND e.event_at >= NOW() - INTERVAL '%d days'", cond.EventTimeWindowDays)
		}
		return fmt.Sprintf(`
			(SELECT COUNT(*) FROM %s e 
			 WHERE e.subscriber_id = s.id 
			 AND e.event_name = %s %s) BETWEEN %s AND %s`,
			eventTable, qb.nextArg(cond.EventName), timeWindow, 
			qb.nextArg(cond.Value), qb.nextArg(cond.ValueSecondary)), nil

	case OpEventInLastDays:
		return fmt.Sprintf(`
			EXISTS (
				SELECT 1 FROM %s e 
				WHERE e.subscriber_id = s.id 
				AND e.event_name = %s 
				AND e.event_at >= NOW() - INTERVAL '%s days'
			)`, eventTable, qb.nextArg(cond.EventName), cond.Value), nil

	case OpEventNotInLastDays:
		return fmt.Sprintf(`
			NOT EXISTS (
				SELECT 1 FROM %s e 
				WHERE e.subscriber_id = s.id 
				AND e.event_name = %s 
				AND e.event_at >= NOW() - INTERVAL '%s days'
			)`, eventTable, qb.nextArg(cond.EventName), cond.Value), nil

	case OpEventPropertyEquals:
		// Drill into event properties
		return fmt.Sprintf(`
			EXISTS (
				SELECT 1 FROM %s e 
				WHERE e.subscriber_id = s.id 
				AND e.event_name = %s 
				AND e.properties->>'%s' = %s
				%s
			)`, eventTable, qb.nextArg(cond.EventName), cond.EventPropertyPath, 
			qb.nextArg(cond.Value), qb.buildEventTimeFilter(cond.EventTimeWindowDays)), nil

	case OpEventPropertyContains:
		return fmt.Sprintf(`
			EXISTS (
				SELECT 1 FROM %s e 
				WHERE e.subscriber_id = s.id 
				AND e.event_name = %s 
				AND e.properties->>'%s' ILIKE %s
				%s
			)`, eventTable, qb.nextArg(cond.EventName), cond.EventPropertyPath, 
			qb.nextArg("%"+cond.Value+"%"), qb.buildEventTimeFilter(cond.EventTimeWindowDays)), nil

	default:
		return "", fmt.Errorf("unsupported event operator: %s", cond.Operator)
	}
}

// buildEventTimeFilter builds the time window filter for events
func (qb *QueryBuilder) buildEventTimeFilter(days int) string {
	if days > 0 {
		return fmt.Sprintf("AND e.event_at >= NOW() - INTERVAL '%d days'", days)
	}
	return ""
}

// buildComputedCondition builds SQL for computed field conditions
func (qb *QueryBuilder) buildComputedCondition(cond ConditionBuilder) (string, error) {
	field := "comp." + cond.Field

	switch cond.Operator {
	case OpGt:
		return fmt.Sprintf("COALESCE(%s, 0) > %s", field, qb.nextArg(cond.Value)), nil
	case OpGte:
		return fmt.Sprintf("COALESCE(%s, 0) >= %s", field, qb.nextArg(cond.Value)), nil
	case OpLt:
		return fmt.Sprintf("COALESCE(%s, 0) < %s", field, qb.nextArg(cond.Value)), nil
	case OpLte:
		return fmt.Sprintf("COALESCE(%s, 0) <= %s", field, qb.nextArg(cond.Value)), nil
	case OpBetween:
		return fmt.Sprintf("COALESCE(%s, 0) BETWEEN %s AND %s", field, qb.nextArg(cond.Value), qb.nextArg(cond.ValueSecondary)), nil
	case OpInLastDays:
		return fmt.Sprintf("%s >= NOW() - INTERVAL '%s days'", field, cond.Value), nil
	case OpMoreThanDaysAgo:
		return fmt.Sprintf("%s < NOW() - INTERVAL '%s days'", field, cond.Value), nil
	case OpIsNull:
		return fmt.Sprintf("%s IS NULL", field), nil
	case OpIsNotNull:
		return fmt.Sprintf("%s IS NOT NULL", field), nil
	default:
		return "", fmt.Errorf("unsupported computed field operator: %s", cond.Operator)
	}
}

// buildTagCondition builds SQL for tag/array conditions
func (qb *QueryBuilder) buildTagCondition(cond ConditionBuilder) (string, error) {
	switch cond.Operator {
	case OpContainsAny:
		// ANY of the tags match
		if len(cond.ValuesArray) == 0 {
			return "FALSE", nil
		}
		placeholders := make([]string, len(cond.ValuesArray))
		for i, v := range cond.ValuesArray {
			placeholders[i] = qb.nextArg(v)
		}
		return fmt.Sprintf("s.tags && ARRAY[%s]", strings.Join(placeholders, ",")), nil

	case OpContainsAll:
		// ALL tags must be present
		if len(cond.ValuesArray) == 0 {
			return "TRUE", nil
		}
		placeholders := make([]string, len(cond.ValuesArray))
		for i, v := range cond.ValuesArray {
			placeholders[i] = qb.nextArg(v)
		}
		return fmt.Sprintf("s.tags @> ARRAY[%s]", strings.Join(placeholders, ",")), nil

	case OpNotContainsAny:
		// NONE of the tags should be present
		if len(cond.ValuesArray) == 0 {
			return "TRUE", nil
		}
		placeholders := make([]string, len(cond.ValuesArray))
		for i, v := range cond.ValuesArray {
			placeholders[i] = qb.nextArg(v)
		}
		return fmt.Sprintf("NOT (s.tags && ARRAY[%s])", strings.Join(placeholders, ",")), nil

	case OpArrayIsEmpty:
		return "(s.tags IS NULL OR array_length(s.tags, 1) IS NULL)", nil

	case OpArrayIsNotEmpty:
		return "(s.tags IS NOT NULL AND array_length(s.tags, 1) > 0)", nil

	default:
		return "", fmt.Errorf("unsupported tag operator: %s", cond.Operator)
	}
}

// HashQuery generates a deterministic hash of the query for caching
func HashQuery(group ConditionGroupBuilder, exclusions []ConditionBuilder, orgID, listID string) string {
	data := struct {
		Group      ConditionGroupBuilder `json:"group"`
		Exclusions []ConditionBuilder    `json:"exclusions"`
		OrgID      string                `json:"org_id"`
		ListID     string                `json:"list_id"`
	}{
		Group:      group,
		Exclusions: exclusions,
		OrgID:      orgID,
		ListID:     listID,
	}

	jsonBytes, _ := json.Marshal(data)
	hash := sha256.Sum256(jsonBytes)
	return hex.EncodeToString(hash[:])
}
