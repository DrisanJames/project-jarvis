// Package dataingest is the ONE seam every landing/transition writer uses to
// report what it just wrote, for the Data Ingest dashboard (REQ 2026-09-20).
//
// DESIGN (the overseer's ruling, contract §DESIGN): per-OPERATION aggregate
// events, never per-row. A writer emits ONE data.ingest.v1 event per chunk it
// persisted — {op_id, at, supply_class, source_path, partner_id, dataset_id,
// batch_id, lane, transition, by_isp, n} — AFTER its INSERT/UPDATE returned.
// Per-row taps (25 sites in the orchestrator, ~1M events/day) and PG row
// triggers were rejected: the `update_list_counts_fast` precedent cost 2h+ per
// 130k rows, and the operator's requirement is a dashboard that "doesn't load
// the database".
//
// Emit is DARK-SAFE and non-blocking in exactly the same way the lake/ingest
// taps are: it marshals, hands the bytes to eventbus.PublishDataIngest, and
// returns. With KAFKA_BROKERS unset, or the produce_data_ingest flag OFF, or
// the taps unwired, it is a first-line no-op. It never returns an error, never
// blocks, and never panics — a writer must not branch on bus health.
package dataingest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/eventbus"
	isppkg "github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// EventVersion is the `v` stamped on every event. Bump only on a breaking
// shape change (the consumer rejects an unknown version rather than guessing).
const EventVersion = 1

// Supply classes (brain #3589, supersedes #3585). at_rest = an object or rows
// we hold; dynamic = an API feed / site event / pull; internal_transfer = data
// already ours moving between our own stores — NEVER counted as "ingested".
const (
	ClassAtRest           = "at_rest"
	ClassDynamic          = "dynamic"
	ClassInternalTransfer = "internal_transfer"
)

// Source paths — WHERE the operation happened. The set is closed: an unknown
// source_path fails Validate rather than landing in a bucket nobody reads.
const (
	SourcePartnerAPI        = "partner_api"
	SourceCSVUpload         = "csv_upload"
	SourceStaticUpload      = "static_upload"
	SourceDrive             = "drive"
	SourceYahooFamilyInject = "yahoo_family_inject"
	SourceDripSupply        = "drip_supply"
	SourceHydration         = "hydration"
	SourceSiteEvent         = "site_event"
	SourceSubscriberImport  = "subscriber_import"
	SourceDatanorm          = "datanorm"
	SourceSlicer            = "slicer"
	SourceValidator         = "validator"
	SourceOrchestrator      = "orchestrator"
	SourceStampRecovery     = "stamp_recovery"
)

// Transitions — WHAT happened to the records. Exotic low-volume transitions
// (stale reap, remail, family exits, journey advance) are deliberately NOT
// tapped; the nightly reconcile from the tables is the settled truth there.
const (
	TransitionLanded            = "landed"
	TransitionVerdictReady      = "verdict_ready"
	TransitionVerdictSuppressed = "verdict_suppressed"
	TransitionVerdictDead       = "verdict_dead"
	TransitionClaimed           = "claimed"
	TransitionMailed            = "mailed"
	TransitionHydrated          = "hydrated"
	TransitionSiteEvent         = "site_event"
	TransitionLoadRegistered    = "load_registered"
	TransitionTransfer          = "transfer"
)

// ISPUnknown is the by_isp bucket used when a writer has a count but no ISP
// breakdown (a transfer, an object registration). It is deliberately NOT
// isp.Other ("other"), which is a REAL ISP class — folding unknowns into it
// would silently inflate the long tail in every composition chart.
const ISPUnknown = "unknown"

var supplyClasses = map[string]bool{
	ClassAtRest: true, ClassDynamic: true, ClassInternalTransfer: true,
}

var sourcePaths = map[string]bool{
	SourcePartnerAPI: true, SourceCSVUpload: true, SourceStaticUpload: true,
	SourceDrive: true, SourceYahooFamilyInject: true, SourceDripSupply: true,
	SourceHydration: true, SourceSiteEvent: true, SourceSubscriberImport: true,
	SourceDatanorm: true, SourceSlicer: true, SourceValidator: true,
	SourceOrchestrator: true, SourceStampRecovery: true,
}

var transitions = map[string]bool{
	TransitionLanded: true, TransitionVerdictReady: true,
	TransitionVerdictSuppressed: true, TransitionVerdictDead: true,
	TransitionClaimed: true, TransitionMailed: true, TransitionHydrated: true,
	TransitionSiteEvent: true, TransitionLoadRegistered: true,
	TransitionTransfer: true,
}

// ValidTransition / ValidSupplyClass / ValidSourcePath are exported so handlers
// (POST /events) and the consumer agree on the vocabulary with no second list.
func ValidTransition(s string) bool  { return transitions[s] }
func ValidSupplyClass(s string) bool { return supplyClasses[s] }
func ValidSourcePath(s string) bool  { return sourcePaths[s] }

// Transitions returns the closed transition set (sorted-order not guaranteed);
// read-only copy so a caller cannot mutate the vocabulary.
func Transitions() []string {
	out := make([]string, 0, len(transitions))
	for t := range transitions {
		out = append(out, t)
	}
	return out
}

// SupplyClasses returns the three classes in dashboard order.
func SupplyClasses() []string {
	return []string{ClassAtRest, ClassDynamic, ClassInternalTransfer}
}

// Event is the data.ingest.v1 payload. Field names/tags are the WIRE CONTRACT —
// the Python writers (agents/jobs/*) POST this exact JSON to
// POST /api/mailing/data-ingest/events, so renaming a tag breaks them silently.
type Event struct {
	V      int       `json:"v"`
	OpID   string    `json:"op_id"`
	At     time.Time `json:"at"`
	Class  string    `json:"supply_class"`
	Source string    `json:"source_path"`

	PartnerID string `json:"partner_id"`
	DatasetID string `json:"dataset_id"`
	BatchID   string `json:"batch_id"`
	Lane      string `json:"lane"`

	Transition string `json:"transition"`

	// ByISP is the per-ISP split of this one operation. Empty is legal (the
	// counters then bucket N under ISPUnknown); when present its values must
	// sum to N, which Validate enforces so a half-filled map cannot quietly
	// under-count a day.
	ByISP map[string]int64 `json:"by_isp,omitempty"`
	N     int64            `json:"n"`
}

// NewOpID returns the idempotency key for one operation. The consumer SETNXs
// di:op:<op_id> (72h), so a Kafka replay of the same operation counts once.
func NewOpID() string { return uuid.NewString() }

// Normalize fills the defaults a caller may reasonably omit: version, op id,
// timestamp, the supply class implied by the source path, and N derived from
// ByISP. It never invents a transition. Mutates in place.
func (e *Event) Normalize() {
	if e.V == 0 {
		e.V = EventVersion
	}
	if strings.TrimSpace(e.OpID) == "" {
		e.OpID = NewOpID()
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	e.Class = strings.TrimSpace(e.Class)
	e.Source = strings.TrimSpace(e.Source)
	e.Transition = strings.TrimSpace(e.Transition)
	e.Lane = strings.TrimSpace(e.Lane)
	if e.Class == "" {
		e.Class = SupplyClassFor(e.Source)
	}
	if e.N == 0 && len(e.ByISP) > 0 {
		var sum int64
		for _, v := range e.ByISP {
			sum += v
		}
		e.N = sum
	}
}

// Validate is the closed-vocabulary gate. It is called by Emit (which drops a
// bad event rather than producing it), by POST /events (which 400s), and by the
// consumer (which errors so the record goes to the DLQ instead of miscounting).
func (e *Event) Validate() error {
	if e.V != EventVersion {
		return fmt.Errorf("dataingest: unsupported version %d (want %d)", e.V, EventVersion)
	}
	if strings.TrimSpace(e.OpID) == "" {
		return fmt.Errorf("dataingest: op_id is required (it is the idempotency key)")
	}
	if e.At.IsZero() {
		return fmt.Errorf("dataingest: at is required")
	}
	if !supplyClasses[e.Class] {
		return fmt.Errorf("dataingest: invalid supply_class %q", e.Class)
	}
	if !sourcePaths[e.Source] {
		return fmt.Errorf("dataingest: invalid source_path %q", e.Source)
	}
	if !transitions[e.Transition] {
		return fmt.Errorf("dataingest: invalid transition %q", e.Transition)
	}
	if e.N < 0 {
		return fmt.Errorf("dataingest: n must be >= 0, got %d", e.N)
	}
	if len(e.ByISP) > 0 {
		var sum int64
		for isp, v := range e.ByISP {
			if strings.TrimSpace(isp) == "" {
				return fmt.Errorf("dataingest: by_isp has an empty ISP key")
			}
			if v < 0 {
				return fmt.Errorf("dataingest: by_isp[%s] must be >= 0, got %d", isp, v)
			}
			sum += v
		}
		if sum != e.N {
			return fmt.Errorf("dataingest: by_isp sums to %d but n=%d", sum, e.N)
		}
	}
	for _, id := range []struct{ name, val string }{
		{"partner_id", e.PartnerID}, {"dataset_id", e.DatasetID}, {"batch_id", e.BatchID},
	} {
		if id.val == "" {
			continue
		}
		if _, err := uuid.Parse(id.val); err != nil {
			return fmt.Errorf("dataingest: %s %q is not a uuid", id.name, id.val)
		}
	}
	return nil
}

// Buckets returns the per-ISP split the counters should apply: ByISP when the
// writer supplied one, else the whole N under ISPUnknown. Never nil.
func (e *Event) Buckets() map[string]int64 {
	if len(e.ByISP) > 0 {
		return e.ByISP
	}
	return map[string]int64{ISPUnknown: e.N}
}

// SupplyClassFor returns the supply class a source path implies, or "" when the
// path carries no inherent class and the CALLER must supply it.
//
// The empty cases are deliberate: slicer / validator / orchestrator /
// stamp_recovery are transitions of data that was already ingested, so their
// class is the BATCH's class (dynamic for a partner API feed, at_rest for a CSV
// or static object). Guessing there would relabel a partner feed as at_rest the
// moment it moved through the slicer.
func SupplyClassFor(sourcePath string) string {
	switch strings.TrimSpace(sourcePath) {
	case SourcePartnerAPI, SourceSiteEvent:
		return ClassDynamic
	case SourceCSVUpload, SourceStaticUpload, SourceDrive, SourceSubscriberImport, SourceDatanorm:
		return ClassAtRest
	case SourceYahooFamilyInject, SourceDripSupply, SourceHydration:
		return ClassInternalTransfer
	default:
		return ""
	}
}

// ClassifyISP maps an email address to its canonical ISP group. It DELEGATES to
// internal/pkg/isp (the one classifier the send path, segments and the lake all
// use) so the dashboard's composition can never disagree with the rest of the
// platform. An empty/garbage address lands in isp.Other, exactly as elsewhere.
func ClassifyISP(email string) string { return isppkg.Group(email) }

// Emit publishes ONE operation event. Contract: non-blocking, error-free,
// panic-free, and a no-op when the bus is dark or the flag is OFF.
//
// A malformed event is DROPPED here rather than produced: a bad record would
// only travel to the consumer, fail Validate again, and cost a DLQ round trip.
// Callers must therefore treat Emit as best-effort observability — the nightly
// reconcile from the tables, not this event, is the settled truth.
func Emit(ctx context.Context, ev Event) {
	defer func() { _ = recover() }()
	if ctx != nil && ctx.Err() != nil {
		return
	}
	ev.Normalize()
	if err := ev.Validate(); err != nil {
		return
	}
	b, err := json.Marshal(&ev)
	if err != nil {
		return
	}
	// key = dataset_id so every event for one feed lands on one partition and
	// its counters are applied in order. An event with no dataset (a subscriber
	// import, a site event) keys on the op id — random spread, still stable.
	key := ev.DatasetID
	if key == "" {
		key = ev.OpID
	}
	eventbus.PublishDataIngest(key, b)
}
