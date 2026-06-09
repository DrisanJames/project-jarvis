// Reader is the READ side of the Phase 1 S3 event lake. The WRITE side
// (lake_emitter.go) fans per-recipient events to Firehose ->
// s3://ignite-analytics-lake -> Glue table ignite_analytics.email_events
// (partitioned by source, dt). This file adds an Athena-backed query layer on
// top of that table.
//
// Design contract (mirrors the emitter):
//   - DISABLED BY DEFAULT. InitReader is a no-op unless an Athena results
//     output location is provided (wired from env ANALYTICS_ATHENA_OUTPUT in
//     main.go). When disabled, Summary/RecentEvents return a clear error and
//     ReaderEnabled() reports false — zero behaviour change for a server that
//     doesn't set the new env vars. Safe to ship dark.
//   - READ ONLY. This package never writes to the lake (that's the emitter)
//     and never touches Postgres. Athena is a separate store.
//   - SQL-injection safe. Every caller-supplied filter is strictly validated
//     (regex/UUID/clamp) before it is ever placed in a query, then rendered as a
//     single-quoted literal via sqlStr. No untrusted string reaches SQL.
package analytics

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	atypes "github.com/aws/aws-sdk-go-v2/service/athena/types"
)

// Reader owns the Athena client + query configuration for the read layer.
type Reader struct {
	client    *athena.Client
	database  string
	workgroup string
	output    string // S3 results location, e.g. s3://bucket/athena-results/
	region    string
}

var (
	readerMu sync.RWMutex
	reader   *Reader
)

// Validation patterns. Anything caller-supplied must match exactly one of
// these (or be empty) before it is allowed near a query.
var (
	dtRe    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	tokenRe = regexp.MustCompile(`^[a-zA-Z0-9_\-]+$`)
	uuidRe  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

const (
	defaultDatabase  = "ignite_analytics"
	defaultWorkgroup = "primary"
	lakeTable        = "email_events"
)

// SummaryRow is one aggregate bucket from Summary.
type SummaryRow struct {
	EventType string `json:"event_type"`
	Count     int64  `json:"count"`
}

// EventFilter is the (validated) filter set for RecentEvents. All fields are
// optional; empty fields are not applied as predicates.
type EventFilter struct {
	Dt         string // ^\d{4}-\d{2}-\d{2}$
	CampaignID string // UUID
	ISPGroup   string // ^[a-zA-Z0-9_\-]+$
	EventType  string // ^[a-zA-Z0-9_\-]+$
	Limit      int    // clamped to [1,1000]
}

// InitReader wires the global reader. output == "" leaves the reader DISABLED
// (Summary/RecentEvents error, ReaderEnabled() == false). database/workgroup
// fall back to their defaults when empty. region falls back to the default
// chain when empty. Safe to call once at server start.
func InitReader(ctx context.Context, database, workgroup, output, region string) error {
	if output == "" {
		log.Printf("[analytics-lake-read] disabled (ANALYTICS_ATHENA_OUTPUT unset)")
		return nil
	}
	if database == "" {
		database = defaultDatabase
	}
	if workgroup == "" {
		workgroup = defaultWorkgroup
	}
	var opts []func(*awsconfig.LoadOptions) error
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return err
	}
	r := &Reader{
		client:    athena.NewFromConfig(cfg),
		database:  database,
		workgroup: workgroup,
		output:    output,
		region:    region,
	}
	readerMu.Lock()
	reader = r
	readerMu.Unlock()
	log.Printf("[analytics-lake-read] enabled database=%s workgroup=%s output=%s region=%s", database, workgroup, output, region)
	return nil
}

// ReaderEnabled reports whether the read layer is configured.
func ReaderEnabled() bool {
	readerMu.RLock()
	defer readerMu.RUnlock()
	return reader != nil
}

func getReader() *Reader {
	readerMu.RLock()
	defer readerMu.RUnlock()
	return reader
}

// errDisabled is returned by the public query functions when the reader is not
// configured.
var errDisabled = fmt.Errorf("lake read disabled")

// validateDt returns an error if dt is non-empty and not a YYYY-MM-DD date.
func validateDt(name, dt string) error {
	if dt == "" {
		return nil
	}
	if !dtRe.MatchString(dt) {
		return fmt.Errorf("invalid %s: must be YYYY-MM-DD", name)
	}
	return nil
}

// validateToken returns an error if v is non-empty and not a safe token.
func validateToken(name, v string) error {
	if v == "" {
		return nil
	}
	if !tokenRe.MatchString(v) {
		return fmt.Errorf("invalid %s: must match ^[a-zA-Z0-9_-]+$", name)
	}
	return nil
}

// validateUUID returns an error if v is non-empty and not a UUID.
func validateUUID(name, v string) error {
	if v == "" {
		return nil
	}
	if !uuidRe.MatchString(v) {
		return fmt.Errorf("invalid %s: must be a UUID", name)
	}
	return nil
}

func clampLimit(n int) int {
	if n < 1 {
		return 1
	}
	if n > 1000 {
		return 1000
	}
	return n
}

// sqlStr renders an already-validated value as a single-quoted Athena string
// literal. Every caller validates its input with the regexes above (no quotes
// or specials can pass), so this is injection-safe; the ” replacement is
// defense-in-depth. We interpolate literals rather than use Athena
// ExecutionParameters because Athena evaluates a date-shaped parameter like
// 2026-06-03 as integer arithmetic, which breaks BETWEEN against the string
// `dt` partition column (TYPE_MISMATCH: varchar BETWEEN integer and integer).
func sqlStr(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// Summary aggregates event counts by event_type over [fromDt, toDt]
// (inclusive). Both bounds are required and must be YYYY-MM-DD.
//
// SQL (validated dates interpolated as quoted literals; see sqlStr):
//
//	SELECT event_type, COUNT(*) c FROM email_events
//	WHERE dt BETWEEN '<from>' AND '<to>'
//	GROUP BY event_type ORDER BY c DESC
func (r *Reader) Summary(ctx context.Context, fromDt, toDt string) ([]SummaryRow, error) {
	if err := validateDt("from", fromDt); err != nil {
		return nil, err
	}
	if err := validateDt("to", toDt); err != nil {
		return nil, err
	}
	if fromDt == "" || toDt == "" {
		return nil, fmt.Errorf("from and to dates are required")
	}
	sql := "SELECT event_type, COUNT(*) c FROM " + lakeTable +
		" WHERE dt BETWEEN " + sqlStr(fromDt) + " AND " + sqlStr(toDt) +
		" GROUP BY event_type ORDER BY c DESC"
	_, rows, err := r.runQuery(ctx, sql)
	if err != nil {
		return nil, err
	}
	out := make([]SummaryRow, 0, len(rows))
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		c, _ := strconv.ParseInt(row[1], 10, 64)
		out = append(out, SummaryRow{EventType: row[0], Count: c})
	}
	return out, nil
}

// RecentEvents returns the most-recent events matching the (validated) filter,
// newest first. The SELECT column order matches scanEvent below.
func (r *Reader) RecentEvents(ctx context.Context, f EventFilter) ([]Event, error) {
	if err := validateDt("dt", f.Dt); err != nil {
		return nil, err
	}
	if err := validateUUID("campaign_id", f.CampaignID); err != nil {
		return nil, err
	}
	if err := validateToken("isp_group", f.ISPGroup); err != nil {
		return nil, err
	}
	if err := validateToken("event_type", f.EventType); err != nil {
		return nil, err
	}
	limit := clampLimit(f.Limit)

	// Build the WHERE clause from validated filters only. Every value was
	// regex-validated above (no quotes/specials can pass), so it is rendered as a
	// single-quoted literal via sqlStr rather than an Athena ExecutionParameter
	// (which would mis-evaluate date-shaped values as integer arithmetic).
	where := ""
	addPred := func(col, val string) {
		if val == "" {
			return
		}
		if where == "" {
			where = " WHERE "
		} else {
			where += " AND "
		}
		where += col + " = " + sqlStr(val)
	}
	addPred("dt", f.Dt)
	addPred("campaign_id", f.CampaignID)
	addPred("isp_group", f.ISPGroup)
	addPred("event_type", f.EventType)

	// limit is an integer we fully control (clamped to [1,1000]); Athena does
	// not accept a bound parameter in the LIMIT position, so it is rendered
	// from the validated int via strconv — never from caller text.
	sql := "SELECT event_uid, recipient_send_id, campaign_id, subscriber_id, email, " +
		"email_domain, brand, isp_group, route_type, event_type, suppression_reason, " +
		"vmta, pool, bounce_cat, dsn_code, dsn_diag, link_url, source_ip, variant, " +
		"event_at, event_epoch_ms, ingested_at, source, dt FROM " + lakeTable +
		where + " ORDER BY event_epoch_ms DESC LIMIT " + strconv.Itoa(limit)

	_, rows, err := r.runQuery(ctx, sql)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(rows))
	for _, row := range rows {
		out = append(out, scanEvent(row))
	}
	return out, nil
}

// scanEvent maps a result row (string cells) back into an Event. Column order
// MUST match the SELECT list in RecentEvents.
func scanEvent(row []string) Event {
	get := func(i int) string {
		if i < len(row) {
			return row[i]
		}
		return ""
	}
	epoch, _ := strconv.ParseInt(get(20), 10, 64)
	return Event{
		EventUID:          get(0),
		RecipientSendID:   get(1),
		CampaignID:        get(2),
		SubscriberID:      get(3),
		Email:             get(4),
		EmailDomain:       get(5),
		Brand:             get(6),
		ISPGroup:          get(7),
		RouteType:         get(8),
		EventType:         get(9),
		SuppressionReason: get(10),
		VMTA:              get(11),
		Pool:              get(12),
		BounceCat:         get(13),
		DSNCode:           get(14),
		DSNDiag:           get(15),
		LinkURL:           get(16),
		SourceIP:          get(17),
		Variant:           get(18),
		EventAt:           get(19),
		EventEpochMs:      epoch,
		IngestedAt:        get(21),
		Source:            get(22),
		Dt:                get(23),
	}
}

// runQuery executes sql against Athena and returns the column headers and the
// data rows (header row stripped). All values are interpolated as validated
// quoted literals by the callers (see sqlStr), not bound parameters. Bounded by ctx.
func (r *Reader) runQuery(ctx context.Context, sql string) (cols []string, rows [][]string, err error) {
	start := &athena.StartQueryExecutionInput{
		QueryString: aws.String(sql),
		QueryExecutionContext: &atypes.QueryExecutionContext{
			Database: aws.String(r.database),
		},
		WorkGroup: aws.String(r.workgroup),
		ResultConfiguration: &atypes.ResultConfiguration{
			OutputLocation: aws.String(r.output),
		},
	}
	startOut, err := r.client.StartQueryExecution(ctx, start)
	if err != nil {
		return nil, nil, err
	}
	qid := startOut.QueryExecutionId

	// Poll until the query reaches a terminal state, bounded by ctx.
	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}
		exec, gerr := r.client.GetQueryExecution(ctx, &athena.GetQueryExecutionInput{
			QueryExecutionId: qid,
		})
		if gerr != nil {
			return nil, nil, gerr
		}
		state := exec.QueryExecution.Status.State
		switch state {
		case atypes.QueryExecutionStateSucceeded:
			goto results
		case atypes.QueryExecutionStateFailed, atypes.QueryExecutionStateCancelled:
			reason := ""
			if exec.QueryExecution.Status.StateChangeReason != nil {
				reason = *exec.QueryExecution.Status.StateChangeReason
			}
			return nil, nil, fmt.Errorf("athena query %s: %s", state, reason)
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}

results:
	firstPage := true
	var nextToken *string
	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}
		in := &athena.GetQueryResultsInput{QueryExecutionId: qid}
		if nextToken != nil {
			in.NextToken = nextToken
		}
		res, rerr := r.client.GetQueryResults(ctx, in)
		if rerr != nil {
			return nil, nil, rerr
		}
		if res.ResultSet != nil {
			for i, rrow := range res.ResultSet.Rows {
				cells := make([]string, 0, len(rrow.Data))
				for _, d := range rrow.Data {
					if d.VarCharValue == nil {
						cells = append(cells, "")
					} else {
						cells = append(cells, *d.VarCharValue)
					}
				}
				// The very first row of the very first page is the column
				// header — capture it, then skip.
				if firstPage && i == 0 {
					cols = cells
					continue
				}
				rows = append(rows, cells)
			}
		}
		firstPage = false
		if res.NextToken == nil {
			break
		}
		nextToken = res.NextToken
	}
	return cols, rows, nil
}

// --- package-level convenience wrappers (used by HTTP handlers) ---

// Summary runs against the global reader. Returns errDisabled when the reader
// is not configured.
func Summary(ctx context.Context, fromDt, toDt string) ([]SummaryRow, error) {
	r := getReader()
	if r == nil {
		return nil, errDisabled
	}
	return r.Summary(ctx, fromDt, toDt)
}

// RecentEvents runs against the global reader. Returns errDisabled when the
// reader is not configured.
func RecentEvents(ctx context.Context, f EventFilter) ([]Event, error) {
	r := getReader()
	if r == nil {
		return nil, errDisabled
	}
	return r.RecentEvents(ctx, f)
}

// IsDisabledErr reports whether err is the "lake read disabled" sentinel so
// handlers can return a graceful 200 instead of a 5xx.
func IsDisabledErr(err error) bool {
	return err == errDisabled
}
