package worker

// REQ-082 — campaign completion must be gated on produced <= landed.
//
// Background (2026-09-01): both completion sites decided "this campaign is
// finished" from the ABSENCE of active queue rows. That inference is only valid
// while every recipient the dispatcher counted has actually reached
// mailing_campaign_queue. It stopped being valid when the wave dispatcher gained
// a Kafka route — a routed wave is marked 'completed' with its full
// enqueued_recipients at PRODUCE time — so campaigns flipped 'sent' with 0-40%
// of their audience still parked in the broker, and the OutboxSelfCheck janitor
// then cancelled 80,514 of those recipients as they landed.
//
// These tests pin the gate itself, at the level where a regression would
// actually happen: someone editing one of the two completion queries.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// TestCompletionGatedOnLandedClause pins the shape of the shared gate: it must
// compare the campaign's summed wave enqueued_recipients against the caller's
// landed expression, with <= (not <), and it must tolerate a campaign with no
// waves at all (COALESCE to 0) rather than dropping it from completion forever.
func TestCompletionGatedOnLandedClause(t *testing.T) {
	clause := campaignLandedGateClause("COUNT(q.id)")

	require.Contains(t, clause, "SUM(w2.enqueued_recipients)",
		"the gate must measure produced from the waves, not from a campaign counter")
	require.Contains(t, clause, "mailing_campaign_waves",
		"the gate must read the wave table")
	require.Contains(t, clause, "<= COUNT(q.id)",
		"the gate must admit completion only when landed >= produced")
	require.Contains(t, clause, "COALESCE(",
		"a campaign with zero waves must gate on 0, not on NULL (NULL <= n is NULL = never completes)")
}

// TestCompletionGatedOnLandedRuntimeSite drives the live runtime sweep and
// asserts the gate rides in BOTH of its completion queries — the 'sending' sweep
// and the pre-enqueued draft/scheduled sweep. sqlmock's regexp matcher fails the
// test if either query loses the predicate.
func TestCompletionGatedOnLandedRuntimeSite(t *testing.T) {
	db, mock, cleanup := setupTestDB(t)
	defer cleanup()

	scheduler := NewCampaignScheduler(db)
	scheduler.ctx, scheduler.cancel = scheduler.newContext()

	// The 'sending' completion query must carry the gate.
	mock.ExpectQuery(`SELECT c\.id[\s\S]*SUM\(w2\.enqueued_recipients\)[\s\S]*<= COUNT\(q\.id\)`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "sent", "failed", "skipped", "pending", "total"}))
	// The pre-enqueued (draft/scheduled) completion query must carry it too —
	// gating only one site means the other still flips campaigns terminal.
	mock.ExpectQuery(`c\.status IN \('draft', 'scheduled'\)[\s\S]*SUM\(w2\.enqueued_recipients\)[\s\S]*<= COUNT\(q\.id\)`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "sent", "failed", "total"}))
	// Orphan sweep that closes the function; not part of this contract.
	mock.ExpectExec(`UPDATE mailing_campaigns`).WillReturnResult(sqlmock.NewResult(0, 0))

	scheduler.checkCompletedCampaigns()
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestCompletionGatedOnLandedBootSite is the cross-site guard. The boot
// migration complete_finished_campaigns lives in cmd/server/main.go (package
// main, not importable from here), and gating only the runtime sweep is useless:
// every ECS bounce re-runs the boot sweep and would flip exactly the campaigns
// the runtime refused. So this test reads main.go off disk and asserts the
// predicate is present inside that migration entry.
func TestCompletionGatedOnLandedBootSite(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "server", "main.go"))
	require.NoError(t, err, "cmd/server/main.go must be readable from the worker package")

	const marker = `{"complete_finished_campaigns"`
	start := strings.Index(string(src), marker)
	require.GreaterOrEqual(t, start, 0, "complete_finished_campaigns migration entry not found")
	// The entry ends at the next migration entry in the slice.
	rest := string(src)[start+len(marker):]
	end := strings.Index(rest, "\n\t\t{\"")
	require.Greater(t, end, 0, "could not delimit the complete_finished_campaigns entry")
	entry := rest[:end]

	require.Contains(t, entry, "SUM(w3.enqueued_recipients)",
		"boot completion must gate on produced (summed wave enqueued_recipients)")
	require.Contains(t, entry, "COUNT(*) FROM mailing_campaign_queue q2",
		"boot completion must gate on landed (queue row count)")
	require.Contains(t, entry, "<=",
		"boot completion must admit completion only when landed >= produced")
	require.Contains(t, entry, "COALESCE(",
		"a campaign with zero waves must gate on 0, not NULL")
}
