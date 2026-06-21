package sendqueue

// route_gate.go is the CANONICAL, low-level "is Kafka the hard send path?" gate
// (SK-5). It is a PURE ENVIRONMENT READ with no dependency on a wired producer,
// so any package — api, mailing, worker, repository/postgres — can ask the same
// question without an import cycle (this package only imports internal/eventbus).
//
// WHY a pure env read (and not the worker's kafkaRouteWave, which also requires a
// producer): the bypass-enqueue sites use this gate to BLOCK direct INSERTs into
// mailing_campaign_queue when Kafka routing is configured ON. We want those
// admin/legacy paths to fail LOUDLY the moment the operator turns routing on —
// even in the window before/without a producer being wired — rather than silently
// writing a row that bypasses the Kafka hard send path. A misconfig (flag on, no
// producer) must surface as a visible, Kafka-attributable block, never a silent
// bypass.
//
// DARK BY DEFAULT: with KAFKA_SEND_QUEUE_ALL / KAFKA_SEND_QUEUE_ENABLED /
// KAFKA_SEND_QUEUE_WAVES / KAFKA_SEND_QUEUE_CAMPAIGNS all unset, SendRouteEnabled
// returns false and every gated path behaves byte-identically to today.

import (
	"os"
	"strings"
)

// SendRouteEnabled reports whether Kafka send-routing is configured ON — i.e.
// whether mailing_campaign_queue rows are (or may be) routed through Kafka rather
// than INSERTed directly. It is true when ANY of the routing envs is set:
//
//   - KAFKA_SEND_QUEUE_ENABLED truthy (1/true/yes/on), OR
//   - KAFKA_SEND_QUEUE_ALL truthy, OR
//   - KAFKA_SEND_QUEUE_WAVES non-empty, OR
//   - KAFKA_SEND_QUEUE_CAMPAIGNS non-empty.
//
// This MUST stay in lock-step with worker.KafkaSendQueueEnabled (the wiring gate)
// — same env names, same semantics — so the wave path's routing decision and the
// bypass paths' blocking decision are driven by one identical configuration.
// With every routing env unset it returns false: the byte-identical dark default.
func SendRouteEnabled() bool {
	if envTruthy(os.Getenv("KAFKA_SEND_QUEUE_ENABLED")) {
		return true
	}
	if envTruthy(os.Getenv("KAFKA_SEND_QUEUE_ALL")) {
		return true
	}
	if strings.TrimSpace(os.Getenv("KAFKA_SEND_QUEUE_WAVES")) != "" {
		return true
	}
	if strings.TrimSpace(os.Getenv("KAFKA_SEND_QUEUE_CAMPAIGNS")) != "" {
		return true
	}
	return false
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
