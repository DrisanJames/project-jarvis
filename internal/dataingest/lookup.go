package dataingest

// lookup.go answers ONE question for the transition writers: what supply class
// does a dataset's data belong to?
//
// WHY IT EXISTS: `landed` carries the class of the door the records came
// through (partner_api → dynamic, csv/static → at_rest), and the batch row now
// stamps it. But `claimed`, `mailed` and the other STATE TRANSITIONS operate on
// partner_clean_queue rows, which carry no supply class of their own — so
// SupplyClassFor("orchestrator") deliberately returns "" (see event.go). The
// class has to come from the dataset.
//
// THE RULE (contract §DESIGN, overseer 2026-09-20): a transition of a dataset
// whose source_channel is 'api_feed' is DYNAMIC; everything else is AT_REST.
// An unresolvable dataset (empty id, missing row, DB error, nil handle) is
// at_rest, which is also what a flat-file dataset gets — the conservative
// answer, and never the empty string, because Emit DROPS an event with an
// invalid supply class.
//
// WHY A CACHE: the orchestrator claims on a 15 s tick across every vertical and
// the validator runs 500-row batches continuously. partner_datasets is a tiny,
// almost-static table, so one lookup per dataset per 60 s is the difference
// between free and a per-wave query the dashboard was explicitly built to
// avoid ("doesn't load the database"). A negative/errored lookup is NOT cached,
// so a transient DB fault self-heals on the next call.

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"
)

// RowQuerier is the minimal read surface the lookup needs. *sql.DB, *sql.Tx and
// dripsupply.Queryer all satisfy it, so a caller inside a transaction can reuse
// its own handle instead of opening a second connection.
type RowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// datasetClassTTL is the cache lifetime. 60 s per the contract: long enough to
// make the claim path free, short enough that re-channeling a dataset in the
// portal is reflected within a tick or two.
const datasetClassTTL = 60 * time.Second

type datasetClassEntry struct {
	class string
	at    time.Time
}

var (
	datasetClassMu    sync.RWMutex
	datasetClassCache = map[string]datasetClassEntry{}
	// datasetClassNow is the clock, swappable by tests.
	datasetClassNow = time.Now
)

// DatasetSupplyClass returns the supply class to stamp on a TRANSITION event
// for datasetID. It never returns "", never blocks on a lock held across I/O,
// and never panics on a nil handle.
func DatasetSupplyClass(ctx context.Context, q RowQuerier, datasetID string) (class string) {
	// Observability must never take a writer down: any panic below (a typed-nil
	// handle a switch missed, a driver that dies mid-scan) answers at_rest.
	class = ClassAtRest
	defer func() {
		if recover() != nil {
			class = ClassAtRest
		}
	}()
	datasetID = strings.TrimSpace(datasetID)
	if datasetID == "" || q == nil || isNilHandle(q) {
		return ClassAtRest
	}
	now := datasetClassNow()
	datasetClassMu.RLock()
	ent, ok := datasetClassCache[datasetID]
	datasetClassMu.RUnlock()
	if ok && now.Sub(ent.at) < datasetClassTTL {
		return ent.class
	}

	var channel sql.NullString
	// Best-effort and short: this runs on a worker tick, never on a request.
	row := q.QueryRowContext(ctx, `SELECT source_channel FROM partner_datasets WHERE id = $1::uuid`, datasetID)
	if err := row.Scan(&channel); err != nil {
		// No row / bad uuid / DB error: answer at_rest and do NOT cache, so the
		// next call re-reads rather than pinning a wrong class for 60 s.
		return ClassAtRest
	}
	class = ClassAtRest
	if strings.EqualFold(strings.TrimSpace(channel.String), "api_feed") {
		class = ClassDynamic
	}
	datasetClassMu.Lock()
	datasetClassCache[datasetID] = datasetClassEntry{class: class, at: now}
	datasetClassMu.Unlock()
	return class
}

// isNilHandle catches the typed-nil trap: a (*sql.DB)(nil) stored in a
// RowQuerier interface is NOT == nil, and calling through it panics. Workers
// are constructed with a nil db in several unit tests, so this is a real path.
func isNilHandle(q RowQuerier) bool {
	switch v := q.(type) {
	case *sql.DB:
		return v == nil
	case *sql.Tx:
		return v == nil
	case *sql.Conn:
		return v == nil
	}
	return false
}

// ResetDatasetClassCache drops every cached entry. Tests only — production
// relies on the TTL.
func ResetDatasetClassCache() {
	datasetClassMu.Lock()
	datasetClassCache = map[string]datasetClassEntry{}
	datasetClassMu.Unlock()
}

// CountByISP is the by_isp builder every writer uses: one pass over the emails
// of the operation, classified through the SAME classifier as the send path.
// Returns nil for an empty input so the event's by_isp is omitted entirely
// (Buckets() then folds N under ISPUnknown) rather than shipping an empty map
// that would fail the sum check.
func CountByISP(emails []string) map[string]int64 {
	if len(emails) == 0 {
		return nil
	}
	out := make(map[string]int64, 8)
	for _, e := range emails {
		out[ClassifyISP(e)]++
	}
	return out
}
