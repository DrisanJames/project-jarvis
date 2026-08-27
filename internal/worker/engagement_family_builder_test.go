package worker

// Tests for EngagementFamilyBuilder (operator rulings 2026-07-26). These pin
// EXPECTED BEHAVIOR, not implementation trivia:
//
//   - the click inlet is the CANONICAL action-click predicate (click-cohort
//     doctrine 2026-07-19: an asset/resource fetch is NOT a click) — the
//     unsub/compliance, everflow-import, t.em self-ref, asset-extension and
//     CDN-host exclusions from agents/dbknowledge/_db.py are all present, and
//     no verdict gating sneaks in,
//   - the gmail family scopes recipients to gmail-ISP addresses; the kumo
//     family does not,
//   - family naming is deterministic: 16×2 GMAIL-ENG specs + 9 KUMO-ALLTIME
//     specs, unique names, canonical brand codes,
//   - window math: [from, to) on clean UTC day edges, 30d/60d widths, the
//     kumo all-time epoch, and the partition-month count,
//   - a rebuild is DELETE + INSERT..SELECT inside ONE transaction (windowed
//     families must expel aged-out members) with ledger + count bookkeeping,
//   - segment creation is idempotent (NOT EXISTS) and cleanup-proof
//     (static + keep_active),
//   - the distributed lock gates the pass; a cancelled context issues no
//     queries; the daily anchor is 05:00 UTC.
//
// Pattern follows verified_humans_ledger_test.go: sqlmock with the regex
// matcher, no real DB.

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestEngagementFamilyBuilder_ClickPredicateShape is the strongest, cheapest
// pin: the canonical navigational-click predicate lives in the SQL text.
func TestEngagementFamilyBuilder_ClickPredicateShape(t *testing.T) {
	for _, sqlText := range []string{gmailClickersInsertSQL, kumoAllTimeInsertSQL} {
		// PG_CLICK_NONNAV mirror: empty, unsub/compliance, synthetic everflow
		// markers, unresolved t.em tracker self-refs.
		for _, want := range []string{
			`COALESCE(te.link_url,'') = ''`,
			`'unsub|optout|opt-out|preference|/privacy'`,
			`'^everflow-import:'`,
			`'^https?://t\.em\.'`,
			// PG_CLICK_ASSET mirror: asset extensions + known asset/CDN hosts.
			`\.(css|js|woff2?|ttf|otf|eot|png|jpe?g|gif|svg|ico|webp|map)([?#]|$)`,
			`(fonts\.g|cdn\.|cloudfront|akamai|fastly|jsdelivr|unpkg|gstatic)`,
		} {
			if !strings.Contains(sqlText, want) {
				t.Fatalf("click SQL missing canonical predicate fragment %q in:\n%s", want, sqlText)
			}
		}
		// The exclusions must be NEGATED (NOT nonnav AND NOT asset) around the
		// clicked event — a raw event_type='clicked' cohort is vendor telemetry.
		if !strings.Contains(sqlText, "te.event_type = 'clicked'\n\t  AND NOT (") {
			t.Fatalf("clicked predicate must AND NOT the exclusion groups:\n%s", sqlText)
		}
		// Actor-blind (2026-07-21): no verdict/heuristic gating in click cohorts.
		for _, banned := range []string{"ignite_verdict_is_human", "ignite_event_verdict", "click_verdict"} {
			if strings.Contains(sqlText, banned) {
				t.Fatalf("click SQL must be actor-blind — found %q", banned)
			}
		}
	}
	// Openers select on 'opened' only — no click predicate needed there.
	if !strings.Contains(gmailOpenersInsertSQL, "te.event_type = 'opened'") {
		t.Fatalf("openers SQL must select event_type='opened'")
	}
	if strings.Contains(gmailOpenersInsertSQL, "'clicked'") {
		t.Fatalf("openers SQL must not touch clicked events")
	}
}

// TestEngagementFamilyBuilder_GmailScope pins the recipient-ISP scoping: both
// gmail statements filter to gmail/googlemail recipients; the kumo family is
// every-ISP (no recipient filter).
func TestEngagementFamilyBuilder_GmailScope(t *testing.T) {
	const gmailFilter = `split_part(lower(s.email), '@', 2) IN ('gmail.com', 'googlemail.com')`
	for _, sqlText := range []string{gmailOpenersInsertSQL, gmailClickersInsertSQL} {
		if !strings.Contains(sqlText, gmailFilter) {
			t.Fatalf("gmail family SQL missing recipient-ISP filter:\n%s", sqlText)
		}
	}
	if strings.Contains(kumoAllTimeInsertSQL, "gmail.com") {
		t.Fatalf("kumo all-time family must NOT be gmail-scoped (every recipient ISP)")
	}
	// Brand scoping: apex + subdomain match on the brand's sending domains.
	for _, sqlText := range []string{gmailOpenersInsertSQL, gmailClickersInsertSQL, kumoAllTimeInsertSQL} {
		if !strings.Contains(sqlText, `(te.sending_domain = $4 OR te.sending_domain LIKE '%.' || $4)`) {
			t.Fatalf("family SQL missing brand sending-domain scope:\n%s", sqlText)
		}
		if !strings.Contains(sqlText, "te.event_at >= $2 AND te.event_at < $3") {
			t.Fatalf("family SQL missing partition-pruning event_at bounds:\n%s", sqlText)
		}
	}
}

// TestEngagementFamilyBuilder_FamilyNaming pins the deterministic spec set:
// 16 brands × {OPEN30D, CLK60D} + 9 kumo brands × {ENG} = 41 unique names on
// the canonical brand codes.
func TestEngagementFamilyBuilder_FamilyNaming(t *testing.T) {
	specs := buildEngagementFamilySpecs()
	if want := 16*2 + 9; len(specs) != want {
		t.Fatalf("want %d specs, got %d", want, len(specs))
	}
	seen := map[string]bool{}
	for _, sp := range specs {
		if seen[sp.name] {
			t.Fatalf("duplicate family name %q", sp.name)
		}
		seen[sp.name] = true
		if sp.apex == "" {
			t.Fatalf("spec %q has no apex", sp.name)
		}
	}
	// Spot-pin canonical codes at the corners (brand_metadata.py registry keys).
	for _, want := range []string{
		"GMAIL-ENG-DB-OPEN30D", "GMAIL-ENG-DB-CLK60D",
		"GMAIL-ENG-WF-OPEN30D", "GMAIL-ENG-WF-CLK60D",
		"KUMO-ALLTIME-MP-ENG", "KUMO-ALLTIME-HTM-ENG",
	} {
		if !seen[want] {
			t.Fatalf("expected family name %q missing", want)
		}
	}
	// Registry contract: every name must match its seeded family_pattern.
	for name := range seen {
		if !strings.HasPrefix(name, "GMAIL-ENG-") && !strings.HasPrefix(name, "KUMO-ALLTIME-") {
			t.Fatalf("name %q matches neither registered family pattern (GMAIL-ENG-%% / KUMO-ALLTIME-%%)", name)
		}
	}
	// Windows: gmail specs carry their ruled widths; kumo is all-time.
	for _, sp := range specs {
		switch {
		case strings.HasSuffix(sp.name, "-OPEN30D"):
			if sp.windowDays != 30 || sp.allTime {
				t.Fatalf("%q must be a 30d window", sp.name)
			}
		case strings.HasSuffix(sp.name, "-CLK60D"):
			if sp.windowDays != 60 || sp.allTime {
				t.Fatalf("%q must be a 60d window", sp.name)
			}
		case strings.HasSuffix(sp.name, "-ENG"):
			if !sp.allTime {
				t.Fatalf("%q must be all-time", sp.name)
			}
		default:
			t.Fatalf("unrecognized family suffix on %q", sp.name)
		}
	}
}

// TestEngagementFamilyBuilder_WindowMath pins the [from, to) computation on
// clean UTC day edges and the partition-month count.
func TestEngagementFamilyBuilder_WindowMath(t *testing.T) {
	now := time.Date(2026, 7, 26, 5, 0, 3, 0, time.UTC) // the 05:00 UTC tick

	from, to := engagementFamilyWindow(now, 30)
	if want := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC); !to.Equal(want) {
		t.Fatalf("open window to: want %s, got %s", want, to)
	}
	if want := time.Date(2026, 6, 26, 0, 0, 0, 0, time.UTC); !from.Equal(want) {
		t.Fatalf("open window from: want %s, got %s", want, from)
	}
	if got := partitionMonthsTouched(from, to); got != 2 {
		t.Fatalf("30d window (jun26→jul26) must touch 2 partition months, got %d", got)
	}

	from, to = engagementFamilyWindow(now, 60)
	if want := time.Date(2026, 5, 27, 0, 0, 0, 0, time.UTC); !from.Equal(want) {
		t.Fatalf("click window from: want %s, got %s", want, from)
	}
	if got := partitionMonthsTouched(from, to); got != 3 {
		t.Fatalf("60d window (may27→jul26) must touch 3 partition months, got %d", got)
	}

	from, to = kumoAllTimeWindow(now)
	if !from.Equal(kumoAllTimeEpoch) {
		t.Fatalf("kumo from must be the epoch (%s), got %s", kumoAllTimeEpoch, from)
	}
	if got := partitionMonthsTouched(from, to); got != 7 {
		t.Fatalf("all-time window (jan→jul26) must touch 7 partition months, got %d", got)
	}

	// Exclusive upper bound: a window ending exactly on a month boundary must
	// NOT touch the next month's partition.
	if got := partitionMonthsTouched(
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)); got != 1 {
		t.Fatalf("[jun1, jul1) must touch exactly 1 month, got %d", got)
	}
}

// TestEngagementFamilyBuilder_EnsureSegmentShape pins the idempotent,
// cleanup-proof creation contract (the seed_verified_humans_segments idiom).
func TestEngagementFamilyBuilder_EnsureSegmentShape(t *testing.T) {
	sqlText := engagementFamilyEnsureSegmentSQL
	for _, want := range []string{
		"WHERE NOT EXISTS", // idempotent per (org, name)
		"m.name = $1",      // keyed by deterministic name
		"'static'",         // out of the dynamic-only materializer
		"TRUE, $4",         // keep_active=TRUE → cleanup-proof
		"SELECT DISTINCT organization_id FROM mailing_segments", // org-scoped
		"'sending_domain'", // scope condition readable by tooling
	} {
		if !strings.Contains(sqlText, want) {
			t.Fatalf("ensure-segment SQL missing %q:\n%s", want, sqlText)
		}
	}
	if !strings.Contains(engagementFamilyLedgerUpsertSQL, "ON CONFLICT (segment_id) DO UPDATE") {
		t.Fatalf("build-ledger upsert must be re-run safe")
	}
}

// expectFamilyRebuild registers one successful DELETE+INSERT rebuild tx plus
// its best-effort bookkeeping (ledger upsert + subscriber_count refresh).
func expectFamilyRebuild(mock sqlmock.Sqlmock, segmentID, apex string, count int64) {
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM mailing_segment_members WHERE segment_id").
		WithArgs(segmentID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("ON CONFLICT (segment_id, subscriber_id) DO NOTHING")).
		WithArgs(segmentID, sqlmock.AnyArg(), sqlmock.AnyArg(), apex).
		WillReturnResult(sqlmock.NewResult(0, count))
	mock.ExpectCommit()
	mock.ExpectExec("INSERT INTO mailing_segment_build_ledger").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE mailing_segments").
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectFamilyBookkeeping registers the trailing pass writes (heartbeat, run
// summary, retention prune) and the advisory unlock.
func expectFamilyBookkeeping(mock sqlmock.Sqlmock) {
	mock.ExpectExec("INSERT INTO mailing_worker_heartbeats").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO mailing_worker_runs").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM mailing_worker_runs").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("pg_advisory_unlock").WillReturnResult(sqlmock.NewResult(0, 1))
}

// TestEngagementFamilyBuilder_Pass drives a full pass over sqlmock: lock → 41
// idempotent ensure-inserts → target listing (one segment resolved) → ONE
// DELETE+INSERT rebuild tx with bookkeeping → heartbeat/run summary.
func TestEngagementFamilyBuilder_Pass(t *testing.T) {
	db, mock := newLedgerMockDB(t)
	seg := "22222222-2222-2222-2222-222222222222"

	mock.ExpectQuery("pg_try_advisory_lock").
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	for range buildEngagementFamilySpecs() {
		mock.ExpectExec("INSERT INTO mailing_segments").
			WillReturnResult(sqlmock.NewResult(0, 0))
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_segments s")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}).
			AddRow(seg, "GMAIL-ENG-DB-OPEN30D"))
	expectFamilyRebuild(mock, seg, "discountblog.com", 42)
	expectFamilyBookkeeping(mock)

	w := NewEngagementFamilyBuilder(db, nil)
	w.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestEngagementFamilyBuilder_PerSegmentIsolation proves one family's rebuild
// failure (tx rolls back, no bookkeeping) does not stop the pass.
func TestEngagementFamilyBuilder_PerSegmentIsolation(t *testing.T) {
	db, mock := newLedgerMockDB(t)
	segA := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	segB := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	mock.ExpectQuery("pg_try_advisory_lock").
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	for range buildEngagementFamilySpecs() {
		mock.ExpectExec("INSERT INTO mailing_segments").
			WillReturnResult(sqlmock.NewResult(0, 0))
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM mailing_segments s")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}).
			AddRow(segA, "GMAIL-ENG-DB-OPEN30D").
			AddRow(segB, "GMAIL-ENG-HT-OPEN30D"))

	// Segment A: DELETE succeeds, INSERT times out → rollback, NO bookkeeping
	// (A keeps last night's members — the tx rolled back whole).
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM mailing_segment_members WHERE segment_id").
		WithArgs(segA).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("ON CONFLICT (segment_id, subscriber_id) DO NOTHING")).
		WithArgs(segA, sqlmock.AnyArg(), sqlmock.AnyArg(), "discountblog.com").
		WillReturnError(errors.New("canceling statement due to statement timeout"))
	mock.ExpectRollback()

	// Segment B: full success.
	expectFamilyRebuild(mock, segB, "historythinking.com", 7)
	expectFamilyBookkeeping(mock)

	w := NewEngagementFamilyBuilder(db, nil)
	w.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("segment B must complete despite segment A failing: %v", err)
	}
}

// TestEngagementFamilyBuilder_DistlockNotAcquired proves the pass is fully
// gated by the lock.
func TestEngagementFamilyBuilder_DistlockNotAcquired(t *testing.T) {
	db, mock := newLedgerMockDB(t)
	mock.ExpectQuery("pg_try_advisory_lock").
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(false))

	w := NewEngagementFamilyBuilder(db, nil)
	w.RunOnce(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestEngagementFamilyBuilder_ContextCancellation proves a cancelled context
// issues no queries.
func TestEngagementFamilyBuilder_ContextCancellation(t *testing.T) {
	db, mock := newLedgerMockDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w := NewEngagementFamilyBuilder(db, nil)
	w.RunOnce(ctx)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("cancelled ctx must issue no queries: %v", err)
	}
}

// TestEngagementFamilyBuilder_DailyAnchor sanity-checks the 05:00 UTC tick.
func TestEngagementFamilyBuilder_DailyAnchor(t *testing.T) {
	w := NewEngagementFamilyBuilder(nil, nil)
	before := time.Date(2026, 7, 26, 3, 0, 0, 0, time.UTC)
	if got := w.timeUntilNextTick(before); got != 2*time.Hour {
		t.Fatalf("before-anchor: want 2h, got %s", got)
	}
	after := time.Date(2026, 7, 26, 6, 0, 0, 0, time.UTC)
	if got := w.timeUntilNextTick(after); got != 23*time.Hour {
		t.Fatalf("after-anchor: want 23h, got %s", got)
	}
}
