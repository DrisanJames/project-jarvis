package api

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/engine"
	"github.com/ignite/sparkpost-monitor/internal/worker"
)

// SendGovernorHeadroom is what the audience planner needs from the wave-level
// SendGovernor (internal/worker/family_governor.go) at DEPLOY time: for a
// governed lane × sending domain × ISP, how many rows may still send today.
// governed=false ⇒ the planner must not clamp (lane not governed, no contract,
// governor off, or an error — which fails OPEN, as the wave hook does).
type SendGovernorHeadroom interface {
	Enabled() bool
	Headroom(ctx context.Context, q worker.FamilyGovernorQueryer, lane, sendingDomain, ispName string, now time.Time) (int, bool, error)
}

var (
	sendGovernorMu sync.RWMutex
	sendGovernor   SendGovernorHeadroom
)

// SetSendGovernor wires the governor into the planner (cmd/server/main.go).
// nil disables the deploy-time clamp; the wave hook is unaffected.
func SetSendGovernor(g SendGovernorHeadroom) {
	sendGovernorMu.Lock()
	sendGovernor = g
	sendGovernorMu.Unlock()
}

// governorHeadroomFor answers the clamp question for one ISP plan of a deploy.
// The lane is the payload's tag or, untagged, the name-derived lane — the same
// rule the wave hook applies (worker.LaneOf). The sending domain is the
// payload's, which is what isp_plans.sending_domain and the wave hook key on.
// `day` is the cell's send day (its earliest span start), not the deploy instant:
// a cell deployed at 23:45 MT for a 02:01 MT anchor belongs to tomorrow.
func governorHeadroomFor(ctx context.Context, db dbQuerier, input engine.PMTACampaignInput, ispName string, day time.Time) (int, bool) {
	sendGovernorMu.RLock()
	g := sendGovernor
	sendGovernorMu.RUnlock()
	if g == nil || !g.Enabled() {
		return 0, false
	}
	q, ok := db.(worker.FamilyGovernorQueryer)
	if !ok {
		return 0, false
	}
	lane := worker.LaneOf(input.Name, input.Lane)
	domain := strings.ToLower(strings.TrimSpace(input.SendingDomain))
	n, governed, err := g.Headroom(ctx, q, lane, domain, ispName, day)
	if err != nil {
		log.Printf("[SendGovernor] PLAN FAIL-OPEN lane=%s domain=%s isp=%s campaign=%q: %v", lane, domain, ispName, input.Name, err)
		return 0, false
	}
	if governed {
		log.Printf("[SendGovernor] PLAN lane=%s domain=%s isp=%s campaign=%q headroom=%d", lane, domain, ispName, input.Name, n)
	}
	return n, governed
}
