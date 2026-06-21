package eventbus

// publish.go is the SINGLE, centralized, dark-safe seam the ingestion hot path
// uses to mirror events onto Kafka. It exists so every hot-path edit is one tiny
// guarded call (PublishLake / PublishIngest / PublishSuppressAdd) and ALL of the
// safety — nil checks, flag gating, panic recovery, fire-and-forget — lives here
// in one place instead of being duplicated at each call site.
//
// Dark-by-default contract:
//
//   - The package-level taps and gates are nil until an explicit WireTaps(...)
//     call (made by the cmd integration step, SA-6) installs them. Until then,
//     every Publish* is a cheap no-op: it returns before touching anything.
//   - Even once wired, a Publish* produces only when its per-flow FlagGate is
//     Enabled() (default OFF). A missing broker list leaves WireTaps installing
//     nil taps, so the no-op path holds regardless.
//   - A Publish* NEVER blocks (the Tap is drop-on-full), NEVER returns an error
//     to the caller, and NEVER panics (a recover guards the whole call). The hot
//     path therefore has byte-identical behavior with the bus off — exactly the
//     lake-emitter's disabled-by-default posture.
//
// Concurrency note: WireTaps is expected to be called exactly once at startup,
// strictly before any hot-path Publish* fires, so the package-level pointers are
// only written once (during init) and read thereafter. No locking is needed for
// that one-shot install-before-use lifecycle.

// Topic names for the three producer flows. Kept as exported constants so wiring
// code, consumers, and tests agree on spelling.
const (
	// TopicLake mirrors the analytics event lake feed (Firehose -> Parquet).
	TopicLake = "evt.lake.v1"
	// TopicIngest carries raw ingest events (SES SNS + PMTA/Kumo accounting).
	TopicIngest = "evt.ingest.v1"
	// TopicSuppression carries ADD-ONLY suppression state changes. Unsuppress is
	// intentionally NOT mirrored here — see PublishSuppressAdd.
	TopicSuppression = "suppression.state"
)

// Package-level registry, all nil by default. WireTaps installs them; until then
// every Publish* is a no-op.
var (
	lakeTap     *Tap
	ingestTap   *Tap
	suppressTap *Tap

	lakeGate     *FlagGate
	ingestGate   *FlagGate
	suppressGate *FlagGate
)

// WireTaps installs the producer taps and their per-flow flag gates. It is called
// once from the cmd integration layer (SA-6) after the eventbus Config/Producer
// is constructed. Any argument may be nil — a nil tap or nil gate simply leaves
// that flow a permanent no-op, which is the dark default. This function performs
// no I/O and never fails.
func WireTaps(
	lake, ingest, suppress *Tap,
	lakeFlag, ingestFlag, suppressFlag *FlagGate,
) {
	lakeTap, ingestTap, suppressTap = lake, ingest, suppress
	lakeGate, ingestGate, suppressGate = lakeFlag, ingestFlag, suppressFlag
}

// gateOpen reports whether a flow may produce: the gate must be non-nil and
// Enabled(). A nil gate is treated as CLOSED (OFF) — fail-safe to the legacy
// path, never fail-open onto Kafka.
func gateOpen(g *FlagGate) bool {
	return g != nil && g.Enabled()
}

// publish is the shared guarded body for all three flows. It recovers from any
// panic so a defect in the bus can never crash the hot path.
func publish(tap *Tap, gate *FlagGate, topic, key string, value []byte) {
	if tap == nil || !gateOpen(gate) {
		return
	}
	defer func() { _ = recover() }()
	tap.Publish(topic, []byte(key), value)
}

// PublishLake mirrors one event-lake record onto TopicLake. No-op when unwired
// or when the lake flag is OFF. key is typically the recipient email (partition
// affinity); value is the JSON already marshaled for Firehose.
func PublishLake(key string, value []byte) {
	publish(lakeTap, lakeGate, TopicLake, key, value)
}

// PublishIngest mirrors one raw ingest event (SES or PMTA/Kumo) onto
// TopicIngest. No-op when unwired or when the ingest flag is OFF. key is an
// ISPPlan/campaign id when available, else the email.
func PublishIngest(key string, value []byte) {
	publish(ingestTap, ingestGate, TopicIngest, key, value)
}

// PublishSuppressAdd mirrors one suppression ADD onto TopicSuppression. No-op
// when unwired or when the suppression flag is OFF.
//
// ADD-ONLY by design: only the add path taps this. Unsuppress/removal stays
// DB-only — Kafka carries ADDs only, and a lagging unsuppress is the safe
// over-suppression direction. Do NOT add a remove variant here.
func PublishSuppressAdd(key string, value []byte) {
	publish(suppressTap, suppressGate, TopicSuppression, key, value)
}

// FlowStats is a cheap read-only snapshot of one producer flow's wiring + flag
// state. All fields are derived from package-level pointers with no I/O, so it
// is safe to call on the /health hot path.
type FlowStats struct {
	// Wired reports whether a live (non-nil-producer) Tap is installed.
	Wired bool `json:"wired"`
	// FlagOn reports whether this flow's FlagGate is currently Enabled().
	FlagOn bool `json:"flag_on"`
}

// ProducerSnapshot aggregates the producer-side taps' lifetime counters and the
// per-flow wiring/flag state for the /health surface. It performs NO network
// I/O — it only reads atomic counters and package-level pointers — so it is
// cheap enough to call on every health probe.
type ProducerSnapshot struct {
	// Aggregate lifetime counters across all wired taps.
	Produced uint64 `json:"produced"`
	Dropped  uint64 `json:"dropped"`
	Failed   uint64 `json:"failed"`

	// Per-flow wiring + flag state.
	Lake     FlowStats `json:"lake"`
	Ingest   FlowStats `json:"ingest"`
	Suppress FlowStats `json:"suppress"`
}

// flowStats reads one tap/gate pair without touching the broker.
func flowStats(tap *Tap, gate *FlagGate) (FlowStats, uint64, uint64, uint64) {
	fs := FlowStats{
		Wired:  tap != nil && tap.Enabled(),
		FlagOn: gateOpen(gate),
	}
	var p, d, f uint64
	if tap != nil {
		p, d, f = tap.Stats()
	}
	return fs, p, d, f
}

// ProducerStats returns a cheap read-only snapshot of the producer-side taps and
// their per-flow flag gates for /health. Before WireTaps is called (the dark
// default) every tap/gate is nil, so the snapshot reports all-zero counters and
// Wired=false / FlagOn=false for every flow.
func ProducerStats() ProducerSnapshot {
	var s ProducerSnapshot
	var p, d, f uint64

	s.Lake, p, d, f = flowStats(lakeTap, lakeGate)
	s.Produced += p
	s.Dropped += d
	s.Failed += f

	s.Ingest, p, d, f = flowStats(ingestTap, ingestGate)
	s.Produced += p
	s.Dropped += d
	s.Failed += f

	s.Suppress, p, d, f = flowStats(suppressTap, suppressGate)
	s.Produced += p
	s.Dropped += d
	s.Failed += f

	return s
}
