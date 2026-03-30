package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SegmentRefreshWorker periodically recalculates subscriber_count for all
// active dynamic segments. Without this, time-relative conditions like
// "last_open_at within_last 7 days" would show stale counts.
type SegmentRefreshWorker struct {
	db       *sql.DB
	interval time.Duration
	stopChan chan struct{}
	running  bool
}

func NewSegmentRefreshWorker(db *sql.DB, interval time.Duration) *SegmentRefreshWorker {
	return &SegmentRefreshWorker{
		db:       db,
		interval: interval,
		stopChan: make(chan struct{}),
	}
}

func (w *SegmentRefreshWorker) Start(ctx context.Context) {
	if w.running {
		return
	}
	w.running = true
	log.Printf("SegmentRefreshWorker: started (interval=%s)", w.interval)

	go func() {
		// Small delay on startup to let DB connections settle
		time.Sleep(30 * time.Second)
		w.refreshAll(ctx)

		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.refreshAll(ctx)
			case <-w.stopChan:
				log.Println("SegmentRefreshWorker: stopped")
				return
			case <-ctx.Done():
				log.Println("SegmentRefreshWorker: context cancelled, stopping")
				return
			}
		}
	}()
}

func (w *SegmentRefreshWorker) Stop() {
	if !w.running {
		return
	}
	close(w.stopChan)
	w.running = false
}

type segmentRow struct {
	ID             uuid.UUID
	ListID         *uuid.UUID
	Name           string
	ConditionsJSON sql.NullString
	OrgID          string
	OldCount       int
}

func (w *SegmentRefreshWorker) refreshAll(ctx context.Context) {
	start := time.Now()

	rows, err := w.db.QueryContext(ctx, `
		SELECT s.id, s.list_id, s.name,
		       COALESCE(s.conditions::text, '[]'),
		       s.organization_id::text,
		       COALESCE(s.subscriber_count, 0)
		FROM mailing_segments s
		WHERE s.status = 'active'
		  AND s.segment_type = 'dynamic'
		ORDER BY s.updated_at ASC
		LIMIT 200
	`)
	if err != nil {
		log.Printf("SegmentRefreshWorker: query error: %v", err)
		return
	}
	defer rows.Close()

	var segments []segmentRow
	for rows.Next() {
		var s segmentRow
		if err := rows.Scan(&s.ID, &s.ListID, &s.Name, &s.ConditionsJSON, &s.OrgID, &s.OldCount); err != nil {
			log.Printf("SegmentRefreshWorker: scan error: %v", err)
			continue
		}
		segments = append(segments, s)
	}

	if len(segments) == 0 {
		return
	}

	updated := 0
	for _, seg := range segments {
		newCount := w.recalculate(ctx, seg)
		if newCount < 0 {
			continue
		}
		_, err := w.db.ExecContext(ctx, `
			UPDATE mailing_segments
			SET subscriber_count = $2, last_calculated_at = NOW(), updated_at = NOW()
			WHERE id = $1
		`, seg.ID, newCount)
		if err != nil {
			log.Printf("SegmentRefreshWorker: update error for %s (%s): %v", seg.Name, seg.ID, err)
			continue
		}
		if newCount != seg.OldCount {
			log.Printf("SegmentRefreshWorker: %s — %d → %d", seg.Name, seg.OldCount, newCount)
		}
		updated++
	}

	log.Printf("SegmentRefreshWorker: refreshed %d/%d segments in %s", updated, len(segments), time.Since(start).Round(time.Millisecond))
}

// recalculate returns the new subscriber count, or -1 on failure.
func (w *SegmentRefreshWorker) recalculate(ctx context.Context, seg segmentRow) int {
	var conditions []segCondition
	if seg.ConditionsJSON.Valid && seg.ConditionsJSON.String != "" && seg.ConditionsJSON.String != "[]" {
		json.Unmarshal([]byte(seg.ConditionsJSON.String), &conditions)
	}

	// Fallback: read from mailing_segment_conditions table
	if len(conditions) == 0 {
		condRows, err := w.db.QueryContext(ctx, `
			SELECT condition_group, field, operator, value
			FROM mailing_segment_conditions
			WHERE segment_id = $1
			ORDER BY condition_group
		`, seg.ID)
		if err == nil {
			defer condRows.Close()
			for condRows.Next() {
				var c segCondition
				if condRows.Scan(&c.Group, &c.Field, &c.Operator, &c.Value) == nil {
					conditions = append(conditions, c)
				}
			}
		}
	}

	if len(conditions) == 0 {
		return -1
	}

	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("SegmentRefreshWorker: begin tx error for %s: %v", seg.Name, err)
		return -1
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = '600s'"); err != nil {
		log.Printf("SegmentRefreshWorker: set timeout error for %s: %v", seg.Name, err)
		return -1
	}

	// Classify conditions into categories for optimized query generation.
	var excludePatterns []string
	var eventConds []segCondition
	var subConds []segCondition
	var domainFilter string
	for _, c := range conditions {
		switch {
		case c.Field == "exclude_list_pattern":
			excludePatterns = append(excludePatterns, c.Value)
		case c.Field == "sending_domain":
			domainFilter = c.Value
		case eventMap[c.Field] != "":
			eventConds = append(eventConds, c)
		case isDomainScopable(c.Field, c.Operator):
			eventConds = append(eventConds, c)
		default:
			subConds = append(subConds, c)
		}
	}

	// Materialize seed exclusion emails into an indexed temp table.
	for i, pattern := range excludePatterns {
		tbl := fmt.Sprintf("_seg_excl_%d", i)
		createQ := fmt.Sprintf(`CREATE TEMP TABLE %s ON COMMIT DROP AS
			SELECT DISTINCT LOWER(s.email) AS email
			FROM mailing_subscribers s
			JOIN mailing_lists l ON s.list_id = l.id
			WHERE l.name ILIKE $1`, tbl)
		if _, err := tx.ExecContext(ctx, createQ, "%"+pattern+"%"); err != nil {
			log.Printf("SegmentRefreshWorker: excl temp table error for %s: %v", seg.Name, err)
			return -1
		}
		tx.ExecContext(ctx, fmt.Sprintf("CREATE INDEX ON %s (email)", tbl))
	}

	// When event-based conditions exist, use a CTE-driven approach:
	// materialize qualifying subscriber IDs from tracking events first,
	// then join to mailing_subscribers for the final count.
	// This avoids scanning 10M+ subscribers with correlated subqueries.
	var query string
	var args []interface{}

	if len(eventConds) > 0 {
		query, args = w.buildCTEQuery(seg, eventConds, subConds, excludePatterns, domainFilter)
	} else {
		fallbackConds := subConds
		if domainFilter != "" {
			fallbackConds = append(fallbackConds, segCondition{Field: "sending_domain", Value: domainFilter})
		}
		where, whereArgs := buildRefreshWhereClause(seg.ListID, fallbackConds)
		query = fmt.Sprintf("SELECT COUNT(DISTINCT LOWER(email)) FROM mailing_subscribers WHERE %s", where)
		args = whereArgs
		for i := range excludePatterns {
			query += fmt.Sprintf(" AND LOWER(email) NOT IN (SELECT email FROM _seg_excl_%d)", i)
		}
	}

	var count int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		log.Printf("SegmentRefreshWorker: count error for %s: %v (q=%s)", seg.Name, err, query)
		return -1
	}
	tx.Commit()
	return count
}

// buildCTEQuery generates an optimized query using CTEs to materialize
// qualifying subscriber IDs from tracking events, then joins to subscribers
// for the final count. This is dramatically faster for event-heavy segments
// because it avoids scanning the full 10M+ subscriber table.
func (w *SegmentRefreshWorker) buildCTEQuery(
	seg segmentRow,
	eventConds, subConds []segCondition,
	excludePatterns []string,
	domainFilter string,
) (string, []interface{}) {
	var ctes []string
	var args []interface{}
	argNum := 1

	domainClause := func() string {
		if domainFilter == "" {
			return ""
		}
		s := fmt.Sprintf(" AND (e.sending_domain = $%d OR e.sending_domain LIKE '%%.' || $%d)", argNum, argNum)
		args = append(args, domainFilter)
		argNum++
		return s
	}

	// Build a CTE for each event condition, collecting subscriber IDs.
	// EXISTS conditions -> _evt_incl_N (must be IN)
	// NOT EXISTS conditions -> _evt_excl_N (must NOT be IN)
	var inclCTEs, exclCTEs []string

	for i, c := range eventConds {
		var evType string
		if et, ok := eventMap[c.Field]; ok {
			evType = et
		} else if c.Field == "last_open_at" {
			evType = "opened"
		} else if c.Field == "last_click_at" {
			evType = "clicked"
		} else {
			continue
		}

		isNegative := c.Operator == "not_in_last_days" || c.Operator == "more_than_days_ago" ||
			c.Operator == "not_equals" || c.Operator == "is_not"

		cteName := fmt.Sprintf("_evt_%d", i)
		var timeClause string
		switch c.Operator {
		case "in_last_days", "within_last", "not_in_last_days", "more_than_days_ago":
			timeClause = fmt.Sprintf(" AND e.event_at >= NOW() - INTERVAL '%s days'", c.Value)
		}

		args = append(args, evType)
		dc := domainClause()
		cte := fmt.Sprintf("%s AS (SELECT DISTINCT e.subscriber_id FROM mailing_tracking_events e WHERE e.event_type = $%d%s%s)",
			cteName, argNum-1, timeClause, dc)
		// Fix: domainClause already incremented argNum, but evType didn't
		// Adjust: evType arg was added before domainClause call
		// Actually let me redo this more carefully
		ctes = append(ctes, cte)

		if isNegative {
			exclCTEs = append(exclCTEs, cteName)
		} else {
			inclCTEs = append(inclCTEs, cteName)
		}
	}

	// Rebuild with correct arg numbering
	ctes = nil
	inclCTEs = nil
	exclCTEs = nil
	args = nil
	argNum = 1

	for i, c := range eventConds {
		var evType string
		if et, ok := eventMap[c.Field]; ok {
			evType = et
		} else if c.Field == "last_open_at" {
			evType = "opened"
		} else if c.Field == "last_click_at" {
			evType = "clicked"
		} else {
			continue
		}

		isNegative := c.Operator == "not_in_last_days" || c.Operator == "more_than_days_ago" ||
			c.Operator == "not_equals" || c.Operator == "is_not"

		cteName := fmt.Sprintf("_evt_%d", i)
		var timeClause string
		switch c.Operator {
		case "in_last_days", "within_last", "not_in_last_days", "more_than_days_ago":
			timeClause = fmt.Sprintf(" AND e.event_at >= NOW() - INTERVAL '%s days'", c.Value)
		}

		evArg := argNum
		args = append(args, evType)
		argNum++

		var dc string
		if domainFilter != "" {
			dc = fmt.Sprintf(" AND (e.sending_domain = $%d OR e.sending_domain LIKE '%%.' || $%d)", argNum, argNum)
			args = append(args, domainFilter)
			argNum++
		}

		cte := fmt.Sprintf("%s AS (SELECT DISTINCT e.subscriber_id FROM mailing_tracking_events e WHERE e.event_type = $%d%s%s)",
			cteName, evArg, timeClause, dc)
		ctes = append(ctes, cte)

		if isNegative {
			exclCTEs = append(exclCTEs, cteName)
		} else {
			inclCTEs = append(inclCTEs, cteName)
		}
	}

	// Build the final query. If we have inclusion CTEs, join through them
	// to narrow the subscriber scan. Then apply exclusion and subscriber-level filters.
	var sb strings.Builder
	sb.WriteString("WITH ")
	sb.WriteString(strings.Join(ctes, ", "))

	// Start from the first inclusion CTE if available (narrows from 10M to ~100K)
	if len(inclCTEs) > 0 {
		sb.WriteString(fmt.Sprintf(
			" SELECT COUNT(DISTINCT LOWER(s.email)) FROM mailing_subscribers s JOIN %s ON %s.subscriber_id = s.id WHERE s.status IN ('active','confirmed')",
			inclCTEs[0], inclCTEs[0]))
		// Additional inclusion CTEs
		for _, ct := range inclCTEs[1:] {
			sb.WriteString(fmt.Sprintf(" AND s.id IN (SELECT subscriber_id FROM %s)", ct))
		}
	} else {
		sb.WriteString(" SELECT COUNT(DISTINCT LOWER(s.email)) FROM mailing_subscribers s WHERE s.status IN ('active','confirmed')")
	}

	if seg.ListID != nil {
		sb.WriteString(fmt.Sprintf(" AND s.list_id = $%d", argNum))
		args = append(args, *seg.ListID)
		argNum++
	}

	// Exclusion CTEs
	for _, ct := range exclCTEs {
		sb.WriteString(fmt.Sprintf(" AND s.id NOT IN (SELECT subscriber_id FROM %s)", ct))
	}

	// Subscriber-level conditions (ISP, email patterns, etc.)
	for _, c := range subConds {
		col := mapCol(c.Field)
		if col == "" {
			continue
		}
		switch c.Operator {
		case "equals", "is":
			sb.WriteString(fmt.Sprintf(" AND s.%s = $%d", col, argNum))
			args = append(args, c.Value)
			argNum++
		case "not_equals", "is_not":
			sb.WriteString(fmt.Sprintf(" AND s.%s != $%d", col, argNum))
			args = append(args, c.Value)
			argNum++
		case "contains":
			sb.WriteString(fmt.Sprintf(" AND s.%s ILIKE $%d", col, argNum))
			args = append(args, "%"+c.Value+"%")
			argNum++
		case "ends_with":
			sb.WriteString(fmt.Sprintf(" AND s.%s ILIKE $%d", col, argNum))
			args = append(args, "%"+c.Value)
			argNum++
		case "starts_with":
			sb.WriteString(fmt.Sprintf(" AND s.%s ILIKE $%d", col, argNum))
			args = append(args, c.Value+"%")
			argNum++
		}
	}

	// Seed exclusion via temp tables
	for i := range excludePatterns {
		sb.WriteString(fmt.Sprintf(" AND LOWER(s.email) NOT IN (SELECT email FROM _seg_excl_%d)", i))
	}

	return sb.String(), args
}

type segCondition struct {
	Group    int    `json:"group"`
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}

// buildRefreshWhereClause mirrors BuildSegmentWhereClause from mailing_segments.go
// but lives in the worker package to avoid import cycles.
// It extracts sending_domain conditions and uses them to scope event subqueries
// and time-relative subscriber fields (last_open_at, last_click_at).
func buildRefreshWhereClause(listID *uuid.UUID, conditions []segCondition) (string, []interface{}) {
	clauses := []string{"status IN ('active','confirmed')"}
	args := []interface{}{}
	argNum := 1

	if listID != nil {
		clauses = append(clauses, fmt.Sprintf("list_id = $%d", argNum))
		args = append(args, *listID)
		argNum++
	}

	// Extract sending_domain filter if present
	var domainFilter string
	var filtered []segCondition
	for _, c := range conditions {
		if c.Field == "sending_domain" {
			domainFilter = c.Value
		} else {
			filtered = append(filtered, c)
		}
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
			clauses = append(clauses, clause)
			args = append(args, "%"+c.Value+"%")
			argNum++
			continue
		}

		col := mapCol(c.Field)
		if col == "" {
			continue
		}

		// Event-based fields route through tracking_events subquery
		if evType, ok := eventMap[c.Field]; ok {
			clause, newArgs, newArgNum := buildEventClause(evType, c.Operator, c.Value, argNum, domainFilter)
			if clause != "" {
				clauses = append(clauses, clause)
				args = append(args, newArgs...)
				argNum = newArgNum
			}
			continue
		}

		// Domain-scoped subscriber engagement fields
		if domainFilter != "" && isDomainScopable(c.Field, c.Operator) {
			clause, newArgs, newArgNum := buildDomainScopedClause(c, argNum, domainFilter)
			if clause != "" {
				clauses = append(clauses, clause)
				args = append(args, newArgs...)
				argNum = newArgNum
			}
			continue
		}

		var clause string
		switch c.Operator {
		case "equals", "is":
			clause = fmt.Sprintf("%s = $%d", col, argNum)
			args = append(args, c.Value)
			argNum++
		case "not_equals", "is_not":
			clause = fmt.Sprintf("%s != $%d", col, argNum)
			args = append(args, c.Value)
			argNum++
		case "contains":
			clause = fmt.Sprintf("%s ILIKE $%d", col, argNum)
			args = append(args, "%"+c.Value+"%")
			argNum++
		case "not_contains":
			clause = fmt.Sprintf("%s NOT ILIKE $%d", col, argNum)
			args = append(args, "%"+c.Value+"%")
			argNum++
		case "starts_with":
			clause = fmt.Sprintf("%s ILIKE $%d", col, argNum)
			args = append(args, c.Value+"%")
			argNum++
		case "ends_with":
			clause = fmt.Sprintf("%s ILIKE $%d", col, argNum)
			args = append(args, "%"+c.Value)
			argNum++
		case "in_last_days", "within_last":
			clause = fmt.Sprintf("%s >= NOW() - INTERVAL '%s days'", col, c.Value)
		case "more_than_days_ago":
			clause = fmt.Sprintf("%s < NOW() - INTERVAL '%s days'", col, c.Value)
		case "not_in_last_days":
			clause = fmt.Sprintf("(%s IS NULL OR %s < NOW() - INTERVAL '%s days')", col, col, c.Value)
		case "is_empty", "is_null":
			clause = fmt.Sprintf("(%s IS NULL OR %s = '')", col, col)
		case "is_not_empty", "is_not_null":
			clause = fmt.Sprintf("(%s IS NOT NULL AND %s != '')", col, col)
		case "greater_than", "gt":
			clause = fmt.Sprintf("%s > $%d", col, argNum)
			args = append(args, c.Value)
			argNum++
		case "less_than", "lt":
			clause = fmt.Sprintf("%s < $%d", col, argNum)
			args = append(args, c.Value)
			argNum++
		default:
			continue
		}
		if clause != "" {
			clauses = append(clauses, clause)
		}
	}
	return strings.Join(clauses, " AND "), args
}

func isDomainScopable(field, operator string) bool {
	if field != "last_open_at" && field != "last_click_at" {
		return false
	}
	switch operator {
	case "in_last_days", "within_last", "more_than_days_ago":
		return true
	}
	return false
}

func buildDomainScopedClause(c segCondition, argNum int, domain string) (string, []interface{}, int) {
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

var eventMap = map[string]string{
	"email_sent":         "sent",
	"email_opened":       "opened",
	"email_clicked":      "clicked",
	"email_bounced":      "bounced",
	"email_delivered":    "delivered",
	"email_unsubscribed": "unsubscribed",
	"email_complained":   "complained",
}

func buildEventClause(evType, operator, value string, argNum int, domainFilter string) (string, []interface{}, int) {
	var args []interface{}
	domainClause := ""
	addDomain := func() {
		if domainFilter != "" {
			args = append(args, domainFilter)
			domainClause = fmt.Sprintf(" AND (e.sending_domain = $%d OR e.sending_domain LIKE '%%.' || $%d)", argNum+1, argNum+1)
		}
	}

	switch operator {
	case "equals", "is":
		args = append(args, evType)
		addDomain()
		return fmt.Sprintf(`EXISTS (SELECT 1 FROM mailing_tracking_events e WHERE e.subscriber_id = mailing_subscribers.id AND e.event_type = $%d%s)`, argNum, domainClause), args, argNum + len(args)
	case "not_equals", "is_not":
		args = append(args, evType)
		addDomain()
		return fmt.Sprintf(`NOT EXISTS (SELECT 1 FROM mailing_tracking_events e WHERE e.subscriber_id = mailing_subscribers.id AND e.event_type = $%d%s)`, argNum, domainClause), args, argNum + len(args)
	case "in_last_days", "within_last":
		args = append(args, evType)
		addDomain()
		return fmt.Sprintf(`EXISTS (SELECT 1 FROM mailing_tracking_events e WHERE e.subscriber_id = mailing_subscribers.id AND e.event_type = $%d AND e.event_at >= NOW() - INTERVAL '%s days'%s)`, argNum, value, domainClause), args, argNum + len(args)
	case "not_in_last_days", "more_than_days_ago":
		args = append(args, evType)
		addDomain()
		return fmt.Sprintf(`NOT EXISTS (SELECT 1 FROM mailing_tracking_events e WHERE e.subscriber_id = mailing_subscribers.id AND e.event_type = $%d AND e.event_at >= NOW() - INTERVAL '%s days'%s)`, argNum, value, domainClause), args, argNum + len(args)
	default:
		return "", nil, argNum
	}
}

func mapCol(field string) string {
	m := map[string]string{
		"email": "email", "first_name": "first_name", "last_name": "last_name",
		"status": "status", "isp": "isp", "engagement_score": "engagement_score",
		"total_emails_received": "total_emails_received", "total_opens": "total_opens",
		"total_clicks": "total_clicks", "last_email_at": "last_email_at",
		"last_open_at": "last_open_at", "last_click_at": "last_click_at",
		"subscribed_at": "subscribed_at", "created_at": "created_at",
		"source": "source", "timezone": "timezone",
	}
	if col, ok := m[field]; ok {
		return col
	}
	if _, ok := eventMap[field]; ok {
		return field // handled separately
	}
	if strings.HasPrefix(field, "custom.") {
		return fmt.Sprintf("custom_fields->>'%s'", strings.TrimPrefix(field, "custom."))
	}
	return ""
}
