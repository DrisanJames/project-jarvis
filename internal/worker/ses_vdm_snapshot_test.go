package worker

// Step-16 fixtures (Vector A plan rev4): work-set gap generation, alias
// summing, disabled heartbeat-only, lease sizing. Permanent fixtures (I-11).
//
// The 48h finalization boundary and the finalized-row-upsert-is-a-no-op
// guard are SQL semantics — pinned in the real-PG integration suite
// (property_ledger_p4_integration_test.go).

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	sespkg "github.com/ignite/sparkpost-monitor/internal/ses"
)

func d(y int, m time.Month, day int) time.Time {
	return time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
}

// TestVDMWorkSetGapAfterAbsentDays: three fully-absent days (no rows, no
// runs) are discoverable ONLY from the last completed run day.
func TestVDMWorkSetGapAfterAbsentDays(t *testing.T) {
	today := d(2026, 8, 17)
	last := d(2026, 8, 13)
	got := vdmWorkSet(&last, nil, today, sesVDMCatchupMaxDays)
	want := []time.Time{d(2026, 8, 14), d(2026, 8, 15), d(2026, 8, 16)}
	if len(got) != len(want) {
		t.Fatalf("work set = %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("work set[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

// TestVDMWorkSetBootstrapAndBound: no completed run ever → the full bounded
// window; never more than maxDays.
func TestVDMWorkSetBootstrapAndBound(t *testing.T) {
	today := d(2026, 8, 17)
	got := vdmWorkSet(nil, nil, today, sesVDMCatchupMaxDays)
	if len(got) != sesVDMCatchupMaxDays {
		t.Fatalf("bootstrap work set = %d days, want %d", len(got), sesVDMCatchupMaxDays)
	}
	if !got[len(got)-1].Equal(d(2026, 8, 16)) {
		t.Fatalf("bootstrap must end at yesterday, got %s", got[len(got)-1])
	}
	// A last-completed 60 days back still bounds to the most recent maxDays.
	old := d(2026, 6, 1)
	got = vdmWorkSet(&old, nil, today, sesVDMCatchupMaxDays)
	if len(got) != sesVDMCatchupMaxDays {
		t.Fatalf("bounded work set = %d days, want %d", len(got), sesVDMCatchupMaxDays)
	}
}

// TestVDMWorkSetIncludesUnfinalizedDays: a day whose run completed but whose
// rows are <48h old re-fetches until finalization.
func TestVDMWorkSetIncludesUnfinalizedDays(t *testing.T) {
	today := d(2026, 8, 17)
	last := d(2026, 8, 16) // caught up — gap walk is empty
	got := vdmWorkSet(&last, []time.Time{d(2026, 8, 15), d(2026, 8, 16)}, today, sesVDMCatchupMaxDays)
	want := []time.Time{d(2026, 8, 15), d(2026, 8, 16)}
	if len(got) != len(want) {
		t.Fatalf("work set = %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("work set[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

// TestVDMAliasSumming: two raw names canonicalizing to ONE group produce ONE
// summed row whose raw_isps lists both — never an overwrite.
func TestVDMAliasSumming(t *testing.T) {
	results := []*sespkg.IdentityISPMetrics{
		{Identity: "em.discountblog.com", RawISP: "Gmail",
			Values: map[string]int64{sespkg.MetricSend: 100, sespkg.MetricDelivery: 90}},
		// "GMAIL" is unmapped → canonicalizes to lower("GMAIL") = "gmail" —
		// the alias collision the summing exists to survive.
		{Identity: "em.discountblog.com", RawISP: "GMAIL",
			Values: map[string]int64{sespkg.MetricSend: 11, sespkg.MetricDelivery: 9}},
		{Identity: "em.discountblog.com", RawISP: "Hotmail",
			Values: map[string]int64{sespkg.MetricSend: 5}},
	}
	rows := sumVDMRawResults(results, nil)

	g := rows["gmail"]
	if g == nil {
		t.Fatal("no canonical gmail row")
	}
	if g.values[sespkg.MetricSend] != 111 || g.values[sespkg.MetricDelivery] != 99 {
		t.Fatalf("alias sums wrong: %+v", g.values)
	}
	if len(g.rawISPs) != 2 || g.rawISPs[0] != "GMAIL" || g.rawISPs[1] != "Gmail" {
		t.Fatalf("raw_isps must list both contributors sorted: %v", g.rawISPs)
	}
	if !g.complete {
		t.Fatal("clean fetches must be complete")
	}
	if ms := rows["microsoft"]; ms == nil || ms.values[sespkg.MetricSend] != 5 {
		t.Fatalf("Hotmail must land canonical microsoft: %+v", rows["microsoft"])
	}
}

// TestVDMSummingMarksIncomplete: a missing metric or a failed raw fetch makes
// the canonical cell incomplete (I-7: it can never finalize).
func TestVDMSummingMarksIncomplete(t *testing.T) {
	results := []*sespkg.IdentityISPMetrics{
		{Identity: "x", RawISP: "Yahoo",
			Values:         map[string]int64{sespkg.MetricSend: 10},
			MissingMetrics: []string{sespkg.MetricOpen}},
	}
	rows := sumVDMRawResults(results, map[string]string{"Cox": "throttled"})

	y := rows["yahoo"]
	if y == nil || y.complete {
		t.Fatalf("missing metric must mark incomplete: %+v", y)
	}
	if len(y.missing) != 1 || y.missing[0] != sespkg.MetricOpen {
		t.Fatalf("missing metrics not carried: %v", y.missing)
	}
	c := rows["cox"]
	if c == nil || c.complete || c.fetchError == "" {
		t.Fatalf("failed raw fetch must produce an incomplete cell with fetch_error: %+v", c)
	}
}

func TestSESVDMSnapshotDisabledHeartbeatOnly(t *testing.T) {
	t.Setenv("SES_VDM_SNAPSHOT_DISABLED", "true")
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	w := NewSESVDMSnapshotWorker(db, nil, "us-west-1")

	mock.ExpectExec(`INSERT INTO mailing_worker_heartbeats`).
		WithArgs(sesVDMSnapshotWorkerName, "disabled", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w.tick(context.Background())

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("disabled tick must be heartbeat-only: %v", err)
	}
}

// TestVDMLeaseSizedForCatchup: the lease covers the worst-case 14-day
// catch-up with margin (plan Step 16).
func TestVDMLeaseSizedForCatchup(t *testing.T) {
	ttl := vdmLeaseTTL(16, 7)
	perDay := time.Duration(16*7) * (sesVDMPerCallPause + sesVDMPerCallEstimate)
	if ttl < time.Duration(sesVDMCatchupMaxDays)*perDay {
		t.Fatalf("lease %s smaller than the 14-day catch-up estimate", ttl)
	}
	if ttl < 30*time.Minute {
		t.Fatalf("lease %s lost its margin", ttl)
	}
}

// TestVDMIdentitiesAndRaws pins the fetch grid: 16 roster identities + the
// explicit non-ledger extras (wcl funnel), and the env-extensible raw set.
func TestVDMIdentitiesAndRaws(t *testing.T) {
	ids := vdmIdentities()
	if len(ids) != 17 {
		t.Fatalf("vdmIdentities() = %d, want 17 (16 drip roster domains + m.wcl-heloc.com): %v", len(ids), ids)
	}
	found := false
	for _, d := range ids {
		if d == "m.wcl-heloc.com" {
			found = true
		}
	}
	if !found {
		t.Fatalf("m.wcl-heloc.com (non-ledger extra) missing from identities: %v", ids)
	}
	t.Setenv("SES_VDM_EXTRA_IDENTITIES", " m.example.com , m.wcl-heloc.com ")
	ids = vdmIdentities()
	if len(ids) != 18 { // env extra added, wcl deduped
		t.Fatalf("env-extended identities = %d, want 18: %v", len(ids), ids)
	}
	t.Setenv("SES_VDM_ISPS", " Outlook , Gmail ,")
	raws := vdmRawISPs()
	seen := map[string]bool{}
	for _, r := range raws {
		seen[r] = true
	}
	if !seen["Outlook"] {
		t.Fatalf("SES_VDM_ISPS extension lost: %v", raws)
	}
	if len(raws) != 8 { // 7 defaults + Outlook; Gmail deduped
		t.Fatalf("raw set = %d, want 8: %v", len(raws), raws)
	}
}
