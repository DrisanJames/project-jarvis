package worker

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// claimISPBatch runs one claimISPForOne per ISP IN PARALLEL for the same
// campaign. The claim's candidate subquery JOINs mailing_campaigns; a bare
// `FOR UPDATE SKIP LOCKED` locks the campaign row too, so every sibling claim
// that loses the race on that one row skips ALL of its queue rows and returns 0
// with no error. 2026-11-10: 1.06M due rows, 0 attempted, Microsoft claimed 56%
// of plan and sbcglobal/att/aol/cox 13-20% (repro: two sessions on local PG,
// no OF -> 0 rows, OF q -> 10). The lock must be scoped to the queue alias.
func TestClaimISPForOne_LocksOnlyQueueRows(t *testing.T) {
	src, err := os.ReadFile("send_worker.go")
	require.NoError(t, err)
	body := string(src)
	i := strings.Index(body, "func (p *SendWorkerPool) claimISPForOne(")
	require.Greater(t, i, 0, "claimISPForOne not found — update this guard alongside the refactor")
	j := strings.Index(body[i:], "\nfunc ")
	require.Greater(t, j, 0)
	fn := body[i : i+j]

	require.Contains(t, fn, "JOIN mailing_campaigns camp ON camp.id = q.campaign_id",
		"the claim no longer joins the campaign — re-check whether this guard still applies")
	assert.Contains(t, fn, "FOR UPDATE OF q SKIP LOCKED",
		"the per-ISP claim must lock only the queue rows (OF q); locking the joined campaign row makes parallel per-ISP claims return 0")
	assert.NotRegexp(t, `FOR UPDATE SKIP LOCKED`, fn,
		"bare FOR UPDATE SKIP LOCKED in claimISPForOne also locks mailing_campaigns")
}
