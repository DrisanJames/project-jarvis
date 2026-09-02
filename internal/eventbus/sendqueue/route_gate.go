package sendqueue

// route_gate.go is the CANONICAL send-path gate for the Kafka send queue. It is
// an ENVIRONMENT READ (plus one optional runtime kill switch) with no dependency
// on a wired producer, so any package — api, mailing, worker,
// repository/postgres — can ask the same question without an import cycle (this
// package only imports internal/eventbus).
//
// ─────────────────────────────────────────────────────────────────────────────
// TWO QUESTIONS, TWO ANSWERS (REQ-090). Conflating them is what produced the
// 2026-09-01 ":1077 half-off" state: KAFKA_SEND_QUEUE_ENABLED=1 with
// KAFKA_SEND_QUEUE_ALL=0 meant *nothing was routed* while five direct-enqueue
// paths still answered 409 "kafka send-routing is ON".
//
//  1. WIRING — "should this process construct the send-queue producer, consumer
//     and reconciler?"  →  SendRouteEnabled().
//     TRUE on KAFKA_SEND_QUEUE_ENABLED alone. This is what cmd/server's
//     wireSendQueue (via worker.KafkaSendQueueEnabled) asks, and it is the ONLY
//     question ENABLED answers. ENABLED=1 keeps the consumer draining a backlog
//     without routing one new recipient.
//
//  2. ROUTING — "do THESE recipients travel through Kafka instead of being
//     INSERTed directly into mailing_campaign_queue?"  →  SendRouteMatches(
//     waveID, campaignID), THE ROUTING PREDICATE. TRUE only on
//     KAFKA_SEND_QUEUE_ALL or an allowlist hit — never on ENABLED.
//
// The wave dispatcher asks (2) per wave (worker.kafkaRouteWave, which adds
// "…and a producer is wired"). The five SK-5 direct-enqueue guards ask (2) in
// its no-id form, SendRoutingActive() — they must block a direct INSERT exactly
// when a wave *would* route, and not one moment earlier.
//
// WHY the guards do not require a wired producer: they must fail LOUDLY the
// moment the operator turns ROUTING on — including the window before a producer
// exists — rather than silently writing a row that bypasses the hard send path.
// A misconfig (routing on, no producer) surfaces as a visible, Kafka-attributable
// block, never a silent bypass.
//
// RUNTIME KILL SWITCH: SetSendRouteFlag installs an eventbus.FlagGate named
// "send_route" (env KAFKA_FLAG_SEND_ROUTE, Redis key kafka:flag:send_route).
// It is a VETO over the env decision, in one direction only:
//
//	Redis `SET kafka:flag:send_route 0`  → routing OFF fleet-wide within ~15s,
//	                                       no task-def roll, no deploy.
//	Redis key absent / `1` / no gate     → the env decides (identical to before).
//
// It can never route mail the env does not already route: a kill switch may only
// ever remove send volume, never add it.
//
// DARK BY DEFAULT: with KAFKA_SEND_QUEUE_ALL / KAFKA_SEND_QUEUE_ENABLED /
// KAFKA_SEND_QUEUE_WAVES / KAFKA_SEND_QUEUE_CAMPAIGNS all unset, every function
// here is false and every gated path behaves byte-identically to today.

import (
	"os"
	"sort"
	"strings"
	"sync"
)

// RouteAny is the sentinel id meaning "any wave / any campaign". Passed to
// SendRouteMatches it answers "is ANYTHING routed right now?" — the question the
// five direct-enqueue guards ask, since they hold no wave/campaign context. It
// matches any NON-EMPTY allowlist (an empty allowlist still routes nothing).
const RouteAny = "*"

// RouteFlag is the runtime kill switch's shape (satisfied by
// *eventbus.FlagGate). Declared as an interface so this low-level package keeps
// its single-import property and stays unit-testable without Redis.
type RouteFlag interface{ Enabled() bool }

// sendRouteFlag holds the installed kill switch. nil (the default, and the value
// in every test and in the dark build) means OPEN — env alone decides.
// RWMutex-guarded, mirroring worker.kafkaSendProducer's idiom.
var (
	sendRouteFlagMu sync.RWMutex
	sendRouteFlag   RouteFlag
)

// SetSendRouteFlag installs (or clears, with nil) the runtime routing kill
// switch. Called once from cmd/server wiring.
func SetSendRouteFlag(f RouteFlag) {
	sendRouteFlagMu.Lock()
	sendRouteFlag = f
	sendRouteFlagMu.Unlock()
}

// SendRouteFlagOpen reports whether the kill switch permits routing. TRUE when
// no flag is installed (dark/test builds) — the switch can only ever take
// routing away, never grant it.
func SendRouteFlagOpen() bool {
	sendRouteFlagMu.RLock()
	f := sendRouteFlag
	sendRouteFlagMu.RUnlock()
	if f == nil {
		return true
	}
	return f.Enabled()
}

// SendRouteMatches is THE ROUTING PREDICATE — the single function that decides
// whether recipients travel through Kafka. Every routing decision in the tree
// resolves here:
//
//   - worker.kafkaRouteWave(waveID, campaignID)  → per-wave dispatch decision
//   - SendRoutingActive()                        → the five SK-5 guards
//
// It is TRUE when the env routes this (wave, campaign) AND the runtime kill
// switch is open. KAFKA_SEND_QUEUE_ENABLED is deliberately NOT consulted: it is
// the WIRING gate (see SendRouteEnabled), not a routing decision.
func SendRouteMatches(waveID, campaignID string) bool {
	return sendRouteEnvMatches(waveID, campaignID) && SendRouteFlagOpen()
}

// SendRoutingActive is the no-id form of the routing predicate: "would any wave
// route right now?". This is what the five direct-enqueue guards call — they
// block a direct INSERT exactly when routing is live.
func SendRoutingActive() bool { return SendRouteMatches(RouteAny, RouteAny) }

// SendRouteEnvConfigured is the PURE-ENV any-id form (no kill switch). It is the
// env layer the "send_route" FlagGate resolves against, so a *present* Redis key
// overrides it and an absent one falls back to exactly this. Never call it as a
// routing decision — use SendRouteMatches / SendRoutingActive.
func SendRouteEnvConfigured() bool { return sendRouteEnvMatches(RouteAny, RouteAny) }

// sendRouteEnvMatches is the env half of the routing predicate:
//
//   - KAFKA_SEND_QUEUE_ALL truthy      → every wave routes; OR
//   - KAFKA_SEND_QUEUE_WAVES     lists waveID; OR
//   - KAFKA_SEND_QUEUE_CAMPAIGNS lists campaignID.
//
// RouteAny matches any non-empty allowlist. With all three unset it is false —
// the byte-identical dark default.
func sendRouteEnvMatches(waveID, campaignID string) bool {
	if envTruthy(os.Getenv("KAFKA_SEND_QUEUE_ALL")) {
		return true
	}
	if listContains(os.Getenv("KAFKA_SEND_QUEUE_WAVES"), waveID) {
		return true
	}
	if listContains(os.Getenv("KAFKA_SEND_QUEUE_CAMPAIGNS"), campaignID) {
		return true
	}
	return false
}

// SendRouteEnabled is the WIRING gate: "should this process construct the
// send-queue producer/consumer/reconciler?" It is true when
// KAFKA_SEND_QUEUE_ENABLED is truthy OR any routing env is set, so turning
// routing on without ENABLED still wires the transport that carries it.
//
// It is NOT a routing decision and MUST NOT gate a direct-enqueue path: with
// ENABLED=1 and ALL=0 (task-def 1077) the consumer is wired and drains, while
// nothing routes. worker.KafkaSendQueueEnabled delegates here so the wiring
// question has exactly one implementation.
func SendRouteEnabled() bool {
	if envTruthy(os.Getenv("KAFKA_SEND_QUEUE_ENABLED")) {
		return true
	}
	return sendRouteEnvMatches(RouteAny, RouteAny)
}

// listContains reports whether the comma/space-separated allowlist contains the
// target id (case-insensitive, trimmed). An empty list never matches; RouteAny
// matches any non-empty list.
func listContains(list, target string) bool {
	if strings.TrimSpace(list) == "" || target == "" {
		return false
	}
	if target == RouteAny {
		return true
	}
	target = strings.ToLower(strings.TrimSpace(target))
	for _, f := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		if strings.ToLower(strings.TrimSpace(f)) == target {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────────
// /health surface (REQ-090 DoD 2)
// ─────────────────────────────────────────────────────────────────────────────

// Routing is the VERBATIM env snapshot of the four send-routing levers plus the
// three resolved answers. It exists because on 2026-09-01 an operator reading
// /health saw `enabled: true, consumer_running: true` and could not tell that
// zero recipients were being routed: the values that decided it were visible
// only in the ECS task definition.
type Routing struct {
	// The four envs, exactly as the process sees them ("" when unset).
	All       string `json:"all"`
	Enabled   string `json:"enabled"`
	Waves     string `json:"waves"`
	Campaigns string `json:"campaigns"`

	// Active is the ROUTING PREDICATE's answer (SendRoutingActive): are
	// recipients being routed to Kafka right now?
	Active bool `json:"active"`
	// WiringEnabled is SendRouteEnabled(): is the producer/consumer wired?
	WiringEnabled bool `json:"wiring_enabled"`
	// FlagOpen is the runtime kill switch (Redis kafka:flag:send_route). False
	// means routing is vetoed regardless of the envs above.
	FlagOpen bool `json:"flag_open"`
}

// RoutingSnapshot reads the four envs verbatim and resolves the predicate. Cheap
// (four Getenv + an atomic load); safe to call per /health request.
func RoutingSnapshot() Routing {
	return Routing{
		All:           os.Getenv("KAFKA_SEND_QUEUE_ALL"),
		Enabled:       os.Getenv("KAFKA_SEND_QUEUE_ENABLED"),
		Waves:         os.Getenv("KAFKA_SEND_QUEUE_WAVES"),
		Campaigns:     os.Getenv("KAFKA_SEND_QUEUE_CAMPAIGNS"),
		Active:        SendRoutingActive(),
		WiringEnabled: SendRouteEnabled(),
		FlagOpen:      SendRouteFlagOpen(),
	}
}

// SendPathEnvNames is THE inventory of environment levers that can change what
// the send path does — the send-transport kill-switch table (findings
// 2026-09-01 send-transport.md, SEV-3) encoded ONCE, in Go, so /health and the
// deploy manifest cannot drift apart. cmd/server/env_manifest_sendpath_test.go
// asserts every name here is declared in deploy/env.manifest.json.
//
// DELIBERATE EXCLUSIONS from the table, each already covered elsewhere on
// /health or unsafe to publish:
//   - KAFKA_BROKERS / _SASL_MECHANISM / _TLS / _ALLOW_AUTO_TOPICS → connection
//     config, already summarised as event_bus.enabled / brokers_set;
//   - KAFKA_FLAG_PRODUCE_LAKE / _INGEST / _SUPPRESS → already event_bus.flags.*;
//   - REDIS_URL → carries credentials; /health must never echo it.
var SendPathEnvNames = []string{
	// Kafka send-queue routing + wiring + runtime kill switch.
	"KAFKA_SEND_QUEUE_ALL",
	"KAFKA_SEND_QUEUE_ENABLED",
	"KAFKA_SEND_QUEUE_WAVES",
	"KAFKA_SEND_QUEUE_CAMPAIGNS",
	"KAFKA_FLAG_SEND_ROUTE",
	// Wave dispatch / enqueue.
	"DISABLE_SETBASED_ENQUEUE",
	"DISABLE_CAP_AWARE_CLAIM",
	"PER_WAVE_TOUCH_CAP",
	"PER_WAVE_WINDOW_HOURS",
	"DISABLE_DISPATCHER_FAIRNESS",
	"WAVE_PROCESSOR_TIMEOUT_SECONDS",
	// Send worker + transport.
	"DISPATCH_CAMPAIGN_PARALLELISM",
	"DISABLE_SEND_OWNERSHIP_RECHECK",
	"DISABLE_RENDER_FAILCLOSED",
	"DISABLE_MESSAGE_DATASET_STAMP",
	"DISABLE_BRAND_IMAGE_HOST_SWAP",
	"DISABLE_SES_OPEN_PIXEL",
	"DISABLE_SES_CLICK_WRAP",
	// Outbox / recovery / janitor.
	"OUTBOX_MODE",
	"DISABLE_FAILED_ROW_RECOVERY",
	"OUTBOX_SELFCHECK_JANITOR_DISABLED",
	"DISABLE_CONSUMER_HANDLE_TIMEOUT",
}

// SendPathKillSwitches returns the send-path env levers that are EXPLICITLY SET
// in this process, name → value verbatim.
//
// "Non-default" here means SET AT ALL: every name in SendPathEnvNames has a
// documented code default that applies when the variable is absent, so an entry
// appearing in this map is exactly "somebody reached in and changed this". An
// empty map means the send path is running on pure code defaults. None of these
// keys is a secret (see deploy/env.manifest.json — secrets are class "secret"
// and are not in this list), so the values are safe to publish on /health.
func SendPathKillSwitches() map[string]string {
	out := make(map[string]string, 4)
	for _, name := range SendPathEnvNames {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			out[name] = v
		}
	}
	return out
}

// SendPathEnvNamesSorted returns the inventory in stable sorted order (used by
// the manifest cross-check test and any operator listing).
func SendPathEnvNamesSorted() []string {
	out := append([]string(nil), SendPathEnvNames...)
	sort.Strings(out)
	return out
}

// envTruthy is the local truthy parser (1/true/yes/on, case-insensitive),
// matching worker.envTruthy so the two gates agree exactly.
func envTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
