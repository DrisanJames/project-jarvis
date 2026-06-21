# Kafka Event Backbone — Qualified Production-Enterprise Plan

**Status:** DESIGN / PLAN for operator approval (2026-06-21). Nothing is built. Produced via the
platform-work framework: deep code research (3 grounded reads) → competing enterprise-design panel
(3 proposals) → judge/synthesis → load-bearing-claim verification against runtime → this qualified plan.

**One-line recommendation:** Adopt Kafka (AWS MSK Provisioned, 3-AZ) as a **durable, replayable event
backbone** for the event plane only — ESP ingestion fan-in, analytics-lake emission, and cross-instance
suppression propagation. **Leave the send queue on Postgres.** Migrate via a reversible, shadow-first,
parity-proven strangler-fig with per-flow kill-switches. Pilot on the lowest-risk flow (lake) to prove
the machinery; deliver the highest unique value (provable cross-instance suppression) once proven.

---

## 1. Scope decision (and why) — verified against the code

**IN (event plane → Kafka):** ESP ingestion fan-in (SES/PMTA/Kumo), analytics lake emission,
cross-instance suppression propagation.

**OUT (stays as-is):** the **send queue** and the **single-leader schedulers**.

| Why OUT | Evidence (verified file:line) |
|---|---|
| The send queue is already a correct transactional outbox; Kafka would *remove* transactional dequeue and *add* double-send risk (mailing twice = reputation/compliance harm). | deterministic idempotency key `uuidv5(campaign,subscriber,wave)` + partial-unique index + `ON CONFLICT DO NOTHING` (`outbox_idempotency.go:24`, `cmd/server/main.go:2246`); `FOR UPDATE SKIP LOCKED` claim + status guards (`send_worker.go:633`,`1758`); `X-Ignite-Idempotency-Key` to PMTA (`send_worker.go:1585`) |
| Single-leader schedulers must not become consumer groups — rebalancing creates a brief double-leader race. | Redis/PG distlock (`pmta_wave_scheduler.go:343`, `suppression_list.go:61`) |

This matches Kafka's own guidance: for a **non-idempotent sink like email**, keep an idempotent
consumer / transactional outbox and dedupe at the sink — which we already do.

---

## 2. The qualified architecture (synthesis of the 3 proposals)

```
                              ┌──────────── MSK Provisioned, 3-AZ, RF=3, min.insync=2, IAM+KMS ───────────┐
 SES/PMTA/Kumo ─┐  Emit() tap │  evt.lake.v1        12p · key=email      · delete 7d                       │
 ingestion      ├──(fire-and──┤  evt.ingest.v1      12p · key=ISPPlanID  · delete 7d                        │
 GlobalSuppress ┘   forget)   │  suppression.state   6p · key=email      · COMPACTED (durable list)         │
                              │  *.dlq               3p · key=event_uid  · delete 14d                       │
                              └───────┬───────────────────────┬───────────────────────┬──────────────────┘
                              ┌───────▼────────┐    ┌──────────▼─────────┐   ┌──────────▼───────────┐
                              │ lake sink      │    │ suppression        │   │ (future) ML / CDC    │
                              │ (reuse Firehose│    │ projector PER TASK │   │ — add a group, zero  │
                              │  flush; later  │    │ → hub.Suppress()   │   │  producer change     │
                              │  Connect S3)   │    │  ADD-only, union   │   └──────────────────────┘
                              └────────────────┘    └────────────────────┘
```

**Topics & keys** (minimal set; partition count is the one hard-to-change knob, so sized for ~10×
headroom — never repartition, it breaks key-ordering):

| Topic | Part | Key | Cleanup | Retention | Rationale |
|---|---|---|---|---|---|
| `evt.lake.v1` | 12 | `email` | delete | 7d | analytics firehose; order-tolerant (dedupes on `event_uid`); replay window |
| `evt.ingest.v1` | 12 | `ISPPlanID`→`email` | delete | 7d | per-send-plan ordering; lossless |
| `suppression.state` | 6 | `email` | **compact** | ∞ (compacted) | durable, replayable suppression list; cold-start rebuild; the enterprise asset |
| `*.dlq` | 3 | `event_uid` | delete | 14d | poison-pill capture; redrive-deduped by id |

**Delivery semantics:** **at-least-once + idempotent consumers**, reusing the deterministic IDs that
already exist (`EventUID` `ses:`/`pmta:`, verified `handlers_ses_events.go:451`, `ingest.go:392`).
Idempotent producer (`acks=all`, `enable.idempotence=true`). **Consumers need no new dedup store** —
the lake's `ON CONFLICT (id,event_at)` and `hub.Suppress()` idempotency already make reprocessing safe.
EOS/transactions are **not** used (the one place they'd help — counters — is deferred; suppression is
set-union and order-independent, so at-least-once is correct).

**Broker:** **MSK Provisioned, 3 brokers / 3 AZs, RF=3, `min.insync.replicas=2`, `acks=all`,
`unclean.leader.election=false`.** `kafka.m7g.large` (Graviton) is ~100× our load — we buy
**availability + replay headroom, not throughput** (our event volume is <1 MBps). **MSK Serverless is
disqualified: it cannot do log compaction**, and `suppression.state` is compacted by design.

**Security:** IAM-SASL auth via ECS task roles (no static secrets); TLS in transit; **KMS CMK** at rest;
least-privilege per-principal ACLs (each producer/consumer scoped to its own topics/groups); brokers
in private subnets, reachable only from the ECS task SG (also keeps traffic off the NAT — relevant
after the NAT-cost incident). Emails are PII but cross no *new* data boundary (already in RDS + lake);
bounded topic retention limits raw-PII accumulation; the compacted suppression topic's tombstone is the
natural GDPR-erasure primitive.

---

## 3. Judge decisions — which proposal won each dimension, and why

| Dimension | Winner | Decision & rationale |
|---|---|---|
| Footprint / producer integration | **A (reuse)** | Producer rides the existing `analytics.Emit(Event)` seam; consumers are env-gated goroutines in `cmd/worker`. No Connect, no new service for v1. |
| Consumer idempotency | **A** | Reuse existing deterministic IDs + `ON CONFLICT` + `Suppress()` idempotency → no new dedup table. |
| Durability / broker / compliance asset | **B (enterprise)** | 3-AZ/RF3/ISR2; **compacted `suppression.state`** as a provable, replayable cross-instance suppression list — the single highest-value capability the current system *cannot* do. |
| Schema governance | **Split** | Adopt B's governance *process* (schemas versioned in repo, compatibility discipline, CI check) but **defer the Glue Schema Registry + Avro** — a single Go codebase with one shared `Event` struct is a compile-time contract today. Add the registry when a non-Go/third-party consumer appears. |
| Durable lake (Connect S3 sink) | **B, deferred** | v1 reuses the existing Firehose flush behind a consumer (A); the Connect-based durable/replayable lake (removes lossiness) is a later phase, not worth the Connect runtime on day one. |
| Migration doctrine | **C (risk-first), wholesale** | Dark-passenger shadow, parity proof via deterministic IDs, per-flow Redis kill-switches, suppression monotonicity, auto-abort canaries, deploy discipline, per-phase DoD. This *is* the production-enterprise qualification. |
| Suppression safety model | **C + verified correction** | Kafka propagates **ADDs only**; the union with the existing DB path is monotonic. **Verified caveat:** the hub *does* have an unsuppress/removal path (`global_suppression.go:415-416`), so unsuppress stays on the DB-eventual path — which lags in the **safe** direction (briefly over-suppressing, never under). |

**Strongest objection to our own pick (must be stated):** this introduces a net-new stateful
distributed system (MSK + consumers + flags + parity jobs) to a 2-task / <1 MBps platform whose
dominant risk is *operational surface*, not throughput. **Is the juice worth the squeeze?** It is *only
if* we value (a) a durable/replayable lake over today's lossy one, (b) provable cross-instance
suppression over eventual, and (c) a reusable backbone for future consumers (ML/real-time). If none of
those are near-term goals, this is over-engineering — and the honest fallback is the **MVP subset**
(§7): the compacted `suppression.state` flow alone, leaving the lake on Firehose. The phased plan means
we pay for Phase 1 *to prove value* before committing further.

---

## 4. Pre-mortem — 5 ways this causes a production incident, and the control that prevents each

1. **Producer blocks the SES sync-inline webhook** → latency → ESP retries. **Control:** fire-and-forget
   tap with bounded drop-and-count buffer (the verified lake-emitter contract, `lake_emitter.go:158`);
   broker-down adds 0 ms to the hot path (load-test gate).
2. **Schema-ahead-of-binary repeat (the 06-10 outage class).** **Control:** shadow-table DDL lands in
   `criticalSendPathDDL`/startup migrations *before or with* the consumer; consumers ship **dark**
   (flag default OFF) so deploy ≠ enable.
3. **A suppressed address gets mailed.** **Control:** Kafka carries **ADD-only**; the in-memory set is a
   monotonic superset of today's; `suppressed_but_would_mail` canary auto-aborts on any nonzero; the DB
   path is never retired (Kafka off = today's behavior). Verified: no unsuppress travels via Kafka.
4. **Consumer-group rebalance double-leads a scheduler.** **Control:** architectural prohibition —
   schedulers stay on distlock and never consume Kafka to make singleton decisions; PR-review gate.
5. **MSK outage takes the platform down.** **Control:** Kafka is never on the send-decision path; every
   flow degrades to its legacy path; a full cluster loss costs *shadow/parity data + suppression
   convergence speed*, never a send.

---

## 5. Migration plan (strangler-fig, shadow-first, reversible) — the qualification

Every flow runs Kafka as a **dark passenger** alongside the authoritative legacy path. Kafka output goes
to **shadow** tables/prefixes until parity is proven and a human promotes it. Each flow has a
**Redis-backed kill-switch** (env default + a `kafka:flag:<name>` key polled ~15 s) that flips a flow
OFF across both tasks in <30 s **with no deploy**.

**Parity is provable** via the deterministic IDs: completeness (`kafka ⊇ live`), excess, and field
fidelity, reconciled per send-day. Hard-zero metrics reset the window on any violation.

| Phase | Scope | Entry → Exit (DoD) | Rollback |
|---|---|---|---|
| **0 Infra (dark)** | MSK 3-AZ, topics, IAM/KMS, client + flag poller shipped with all flags OFF | `/health` shows `kafka.enabled, flows OFF`; clean rolling deploy; schedulers reclaim distlock; counters 0; webhook latency unchanged (broker-down load test) | leave flags OFF |
| **1 Lake shadow** (lowest risk — already lossy, no send impact; **proves the machinery + monitoring win**) | `Emit` also produces `evt.lake.v1`; shadow consumer → shadow S3 prefix; reconcile vs Firehose | lake missing-rate ≤0.05%, field-mismatch 0.00%, 7 send-days incl. 2 high-volume; consumer-lag p99 <30 s | `KAFKA_PRODUCE_LAKE=off` (Redis) → Firehose-only instantly |
| **2 Ingest shadow** (lossless required, still no live-table writes) | taps on SES/PMTA/Kumo → `evt.ingest.v1`; shadow consumer → `ingest_events_shadow`; reconcile incl. **dedup parity** | missing 0.00%, field-mismatch 0.00%, dedup exact, 7 send-days | producer flags off; legacy ingestion untouched |
| **3 Suppression shadow** (highest pre-cutover risk) | hub also produces `suppression.state` (ADD-only); per-task projector records *would-apply* set; **live hashset still mutated only by the DB path** | `suppressed_but_would_mail = 0` exactly; Kafka convergence p99 ≤ current DB-eventual p99; superset/monotonicity audit clean; 7 send-days | producer flag off; behavior never changed in this phase |
| **4 Promote** (A→B→C, one at a time) | flip `KAFKA_AUTHORITATIVE_*` on; legacy demoted to shadow (suppression's DB path **kept forever** as the durable floor) | promoted path shows zero divergence 7 days; **rollback flip exercised** in a calm window | flip authoritative OFF → legacy (still running) in <30 s |

**Deploy discipline per phase (inherited):** schema before/with binary; clean tree only (`deploy.sh`
aborts dirty); ship dark then enable; flip flags **outside an active send window**; one flow per deploy.

**Auto-abort canaries:** ingest mismatch (any nonzero 15 min) → auto-flip producer OFF;
`suppressed_but_would_mail > 0` ever → auto-flip OFF + page; per the no-panic rule, auto-abort returns
to the safe legacy path and **posts to Slack** — it never stops the send-day or contacts ISPs.

---

## 6. Observability / SLOs (the measure-and-monitor goal — lands at Phase 1)

| SLI | Target | Source |
|---|---|---|
| Lake e2e latency (`event_at`→lake) p99 | <90 s | `ingested_at − event_at` (both already in the Event struct) |
| Suppression propagation p99 | <10 s (and ≤ current) | projector apply timestamp |
| `suppressed_but_would_mail` | **0** (page on >0) | shadow replay |
| Consumer lag (per group) | <30 s / <50k | MSK `MaxOffsetLag` → CloudWatch; Burrow later |
| DLQ rate | 0 (page on >0) | `*.dlq` |
| Under-replicated partitions / disk | 0 / <70% | MSK CloudWatch |

Surface a one-tile "event-bus health" strip on the existing Event Lake / engine board; add
`kafka_produced/drop/err` + flag state to `/health`. No new console day one.

---

## 7. Cost & the MVP fallback

- **Full v1:** ~$0.5k–0.7k/mo (3×`m7g.large` + EBS + KMS; consumers ride existing ECS; lake volume
  unchanged). Connect-based durable lake (deferred) adds ~$150–300/mo.
- **MVP (if budget/time constrained):** build **only the compacted `suppression.state` flow** — the one
  capability the current system genuinely cannot do (durable, provable, cross-instance, restart-surviving
  suppression). Leave the lake on Firehose. ~half the cost, one consumer pattern, full upgrade path later.

---

## 8. Open decisions for the operator

1. **Approve the qualified scope** (event backbone; send queue stays Postgres)?
2. **Full v1 vs MVP-suppression-first?** (Recommend: full v1, but pilot Phase 1 on lake to prove machinery.)
3. **Pilot flow:** lake-first (lowest risk, proves machinery + monitoring) — agree?
4. **Defer Glue Schema Registry + MSK Connect** to later phases (governance-as-process now)? (Recommend: yes.)
5. Greenlight to convert this plan into an execution-ready phase-0 ticket set (still no code until you say build)?

*Sources: Confluent EOS & delivery-semantics; AWS MSK cluster-types & Serverless limits. All current-state
claims verified against the codebase at the cited file:line on 2026-06-21.*
