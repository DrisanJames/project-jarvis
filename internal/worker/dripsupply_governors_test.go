package worker

// dripsupply_governors_test.go — D3. The wiring test: a governor that ships
// implemented, unit-tested and REGISTERED NOWHERE is not a safety, it is a
// safety on paper. This fails if any shipped governor is dropped from the
// stack, which is the state cmd/server/main.go was in before this change.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ignite/sparkpost-monitor/internal/worker/dripsupply"
)

func stubSESQuota(context.Context) (float64, float64, error) { return 1_000_000, 0, nil }

// Every shipped GovernorReader is in the stack cmd/server/main.go wires.
func TestDripSupplyWiresEveryShippedGovernor(t *testing.T) {
	g := DripSupplyGovernors(nil, "", stubSESQuota)
	assert.Equal(t, []string{"throttle", "gmail_hold", "ses_quota"}, DripSupplyGovernorNames(g),
		"a shipped governor was dropped from the stack — cmd/server/main.go wired only \"throttle\" before D3")
	require.Len(t, g, 3)
}

// NEGATIVE CONTROL. The pre-D3 wiring — ThrottleGovernor alone — must FAIL the
// assertion above. Without this the test could be passing on an assertion that
// cannot fail.
func TestDripSupplyGovernorNamesRejectsThePreD3Stack(t *testing.T) {
	preD3 := dripsupply.Governors{dripsupply.ThrottleGovernor{}}
	assert.NotEqual(t, []string{"throttle", "gmail_hold", "ses_quota"}, DripSupplyGovernorNames(preD3))
	assert.Equal(t, []string{"throttle"}, DripSupplyGovernorNames(preD3),
		"this is exactly what prod had: no SES quota ceiling in the reservation path")
}

// KILL SWITCH. DRIP_SUPPLY_GOVERNORS_OFF drops a governor with no deploy —
// which matters most for gmail_hold, the one governor that fails CLOSED.
func TestDripSupplyGovernorsKillSwitch(t *testing.T) {
	t.Setenv(DripSupplyGovernorsOffEnv, "ses_quota, gmail_hold")
	assert.Equal(t, []string{"throttle"},
		DripSupplyGovernorNames(DripSupplyGovernors(nil, "", stubSESQuota)))

	t.Setenv(DripSupplyGovernorsOffEnv, "throttle")
	assert.Equal(t, []string{"gmail_hold", "ses_quota"},
		DripSupplyGovernorNames(DripSupplyGovernors(nil, "", stubSESQuota)))
}

// The stack must satisfy the interface the reservation path takes, and a nil
// SES reader must be tolerated (a box with no SES credentials still boots).
func TestDripSupplyGovernorsSatisfyGovernorReader(t *testing.T) {
	var _ dripsupply.GovernorReader = DripSupplyGovernors(nil, "", nil)

	g := DripSupplyGovernors(nil, "", nil)
	require.Len(t, g, 3)
	// nil DB + nil SES reader: every governor is inert, none errors, none
	// invents a ceiling. An error here would fail the whole wave closed.
	cs, err := g.Ceilings(context.Background(), time.Time{}, "em.discountblog.com", "gmail", dripsupply.Window{})
	require.NoError(t, err)
	assert.Empty(t, cs, "unconfigured governors must never invent a ceiling")
}
