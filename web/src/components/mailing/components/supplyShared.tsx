// supplyShared.tsx — types, fetch helpers and display primitives for the
// Supply tab (REQ-118 WP10).
//
// WHY THIS FILE EXISTS: the four Supply panes render the SAME numbers under the
// same rules, and those rules are contract-level, not cosmetic:
//
//   1. Every number carries the API's label (contracted | effective | planned |
//      reserved | actual | forecast). The label comes from the response's
//      `labels` map — the UI never invents one.
//   2. `null` is UNKNOWN and renders as "unknown", never 0. A zero on this
//      screen means the mediator measured zero; an unknown means it could not.
//   3. The health colour is the API's `health` field verbatim. Two
//      implementations of a colour rule is two colour rules
//      (drip_supply_handlers.go dripHealthColour is the only one).
//   4. `as_of`, freshness and contract versions ride in a header strip on every
//      pane — a supply number without its as_of is a number about an unknown
//      moment.
//
// Types mirror the JSON tags in internal/api/drip_supply_handlers.go. Where the
// Go side emits a pointer (*int / *float64 / *time.Time) the field is
// `number | null` / `string | null` here: that nullability IS the contract.

import React from 'react'
import { apiFetch } from '../shared/apiFetch'
import { colors, alpha, tdStyle, thStyle, numTd, numTh, tableStyle } from '../shared/theme'

// ═══════════════════════════════════════════════════════════════════════════
// API TYPES (mirror of drip_supply_handlers.go)
// ═══════════════════════════════════════════════════════════════════════════

/** The closed label vocabulary (drip_supply_handlers.go dripLabelVocab). */
export type SupplyLabel = 'contracted' | 'effective' | 'planned' | 'reserved' | 'actual' | 'forecast'

/** The envelope every Supply response carries (dripSupplyMeta). */
export interface SupplyMeta {
  as_of: string
  day: string
  labels: Record<string, string>
  contract_versions?: Record<string, number>
  /** Anything the response could NOT compute — a null here is unknown, not zero. */
  degraded?: string[]
}

export interface EstateStrip {
  contracted: number | null
  effective: number | null
  reserved: number | null
  committed: number | null
  desired: number | null
  unfilled: number | null
  eo_spend_today_usd: number | null
  stranded_claims: number | null
  domain_isp_cells: number
  lane_isp_cells: number
}

export interface Freshness {
  balances: string | null
  ledger: string | null
  max: string | null
}

export interface HealthResponse extends SupplyMeta {
  estate: EstateStrip
  freshness: Freshness
}

export interface DispatchValue {
  contribution_ecpm: number | null
  observed_ecpm: number | null
  maturity: string
  messages: number | null
  conversions: number | null
  inherited: boolean
}

export interface LaneDemand {
  desired: number | null
  awarded_firm: number | null
  awarded_provisional: number | null
  supply_backed: number | null
  unserved: number | null
  unserved_reason: string
  followups_reserved: number | null
  supply_released: number | null
}

export interface EcosystemLaneRow {
  lane: string
  rank: number | null
  rank_reason: string
  tier: number
  exploration: boolean
  paused: boolean
  dispatch_value: DispatchValue
  demand: LaneDemand
  followups_due: number | null
  fresh_mailable: number | null
  pending_eo: number | null
  remail_eligible: number | null
  clean_ordered_today: number | null
  clean_cost_today_usd: number | null
  sent_today: number | null
  reserved: number | null
  committed: number | null
  fill_rate: number | null
  binding_constraint: string
  health: string
  health_reason: string
  dispatch_contract_version: number
  inventory_contract_version?: number
}

export interface EcosystemResponse extends SupplyMeta {
  lanes: EcosystemLaneRow[]
  plan_frozen_at: string | null
}

export interface LaneISPDemand {
  isp: string
  desired: number | null
  awarded_firm: number | null
  awarded_provisional: number | null
  supply_backed: number | null
  unserved: number | null
  unserved_reason: string
  followups_reserved: number | null
  reserved: number | null
  committed: number | null
  fresh_mailable: number | null
  followups_due: number | null
  pending_eo: number | null
  excluded: boolean
}

export interface LaneCapacityCell {
  sending_domain: string
  isp: string
  contracted: number | null
  effective: number | null
  effective_reason: string
  planned: number | null
  reserved: number | null
  submitted: number | null
  remaining: number | null
  blocked_reason: string
  tokens: number | null
}

export interface LaneSupplyCell {
  source_slug: string
  isp: string
  events: Record<string, number>
  cost_usd: number
}

export interface LaneEconomicsRow {
  isp: string
  messages: number
  conversions: number
  revenue_usd: number
  gross_ecpm: number | null
  contribution_ecpm: number | null
  fully_loaded_ecpm: number | null
  cleaning_value: number | null
  maturity: string
  sample_ok: boolean
}

export interface TickOutcomeRow {
  tick: string
  pass: string
  outcome: string
  reason: string
  claimed: number
  campaign_id: string | null
  caps_seen: unknown
}

export interface ContractMetaBlock {
  contract_id?: string
  kind?: string
  version?: number
  refs: {
    sending_domain_id?: string
    owned_domain_id?: string
    dataset_ids?: string[]
    segment_ids?: string[]
  }
  mutation: { at: string; by: string; change_ledger_id: string; prior_version: number }
  token: { alg: string; issued_at: string; issued_by: string; value?: string }
}

export interface ContractSummary {
  kind: string
  subject: string
  version: number
  status: string
  effective_at: string
  token_present: boolean
  metadata: ContractMetaBlock
}

export interface LaneResponse extends SupplyMeta {
  lane: string
  tier: number
  paused: boolean
  demand_by_isp: LaneISPDemand[]
  capacity_by_domain_isp: LaneCapacityCell[]
  supply_by_source_isp: LaneSupplyCell[]
  economics_by_isp: LaneEconomicsRow[]
  tick_outcomes_24h: TickOutcomeRow[]
  contracts: ContractSummary[]
  dispatch_value: DispatchValue
}

export interface DomainRow {
  sending_domain: string
  contracted: number | null
  effective: number | null
  effective_reason: string
  reserved: number | null
  committed: number | null
  released: number | null
  remaining: number | null
  status: string
  isp_cells: number
  last_refill_tick: string | null
}

export interface DomainsResponse extends SupplyMeta {
  domains: DomainRow[]
}

export interface DomainISPRow {
  isp: string
  contracted: number | null
  effective: number | null
  effective_reason: string
  tokens: number | null
  reserved: number | null
  committed: number | null
  released: number | null
  remaining: number | null
  last_refill_tick: string | null
}

export interface CapacityLedgerRow {
  allocation_id: string
  idempotency_key: string
  tick: string
  sending_domain: string
  isp: string
  lane: string
  touch_class: string
  domain_contract_version: number
  dispatch_contract_version: number
  requested: number
  reserved: number
  committed: number
  released: number
  status: string
  campaign_id: string | null
  binding_reason: string
  release_reason: string
  domain_balance_after: number
  lane_unfilled_after: number
  created_at: string
  updated_at: string | null
}

export interface DomainResponse extends SupplyMeta {
  sending_domain: string
  buckets_by_isp: DomainISPRow[]
  ledger: CapacityLedgerRow[]
  contracts: ContractSummary[]
}

export interface CapacityLedgerResponse extends SupplyMeta {
  filters: Record<string, string>
  limit: number
  entries: CapacityLedgerRow[]
}

export interface SupplyLedgerRow {
  entry_id: string
  occurred_at: string
  lane: string
  source_slug: string
  isp: string
  event: string
  quantity: number
  unit_cost: number
  total_cost: number
  batch_id: string | null
  reservation_id: string | null
  reason: string
  source_contract_version: number | null
  inventory_contract_version: number | null
}

export interface SupplyLedgerResponse extends SupplyMeta {
  filters: Record<string, string>
  limit: number
  entries: SupplyLedgerRow[]
}

export interface ContractVersionRow {
  id: string
  version: number
  status: string
  effective_at: string
  superseded_at: string | null
  created_by: string
  created_at: string
  approved_by: string
  approved_at: string | null
  change_ledger_id: string
  notes: string
  token_present: boolean
  token_issued_at: string | null
  metadata: ContractMetaBlock
}

export interface ContractsResponse extends SupplyMeta {
  kind: string
  subject: string
  active_version: number | null
  scheduled_version: number | null
  versions: ContractVersionRow[]
}

export type ContractKind = 'domain' | 'dispatch' | 'inventory' | 'source'

/**
 * The 12 canonical ISP classes — the SAME list, in the same order, as
 * dripsupply.ispClasses (internal/worker/dripsupply/contracts.go:50). A domain
 * contract that is missing any one of these is a save error server-side, so the
 * per-ISP editor must render all twelve, always.
 */
export const ISP_CLASSES = [
  'aol', 'apple', 'att', 'charter', 'comcast', 'cox',
  'gmail', 'microsoft', 'other', 'sbcglobal', 'verizon', 'yahoo',
] as const

// ═══════════════════════════════════════════════════════════════════════════
// FETCH — every request through the shared apiFetch (org header + credentials)
// ═══════════════════════════════════════════════════════════════════════════

/** A failed Supply request, carrying the API's field list when it sent one. */
export class SupplyError extends Error {
  status: number
  fields: string[]
  constructor(message: string, status: number, fields: string[] = []) {
    super(message)
    this.name = 'SupplyError'
    this.status = status
    this.fields = fields
  }
}

const SUPPLY_BASE = '/api/mailing/supply'

async function parseError(res: Response): Promise<SupplyError> {
  let message = `HTTP ${res.status}`
  let fields: string[] = []
  try {
    const body: unknown = await res.json()
    if (body && typeof body === 'object') {
      const rec = body as Record<string, unknown>
      if (typeof rec.error === 'string' && rec.error.trim()) message = rec.error
      if (Array.isArray(rec.fields)) fields = rec.fields.filter((f): f is string => typeof f === 'string')
    }
  } catch {
    /* a non-JSON error body stays as the status line */
  }
  return new SupplyError(message, res.status, fields)
}

export async function supplyGet<T>(
  path: string,
  params: Record<string, string | undefined>,
  signal?: AbortSignal,
): Promise<T> {
  const qs = new URLSearchParams()
  Object.entries(params).forEach(([k, v]) => {
    if (v != null && v !== '') qs.set(k, v)
  })
  const url = `${SUPPLY_BASE}${path}${qs.toString() ? `?${qs.toString()}` : ''}`
  const res = await apiFetch(url, signal ? { signal } : {})
  if (!res.ok) throw await parseError(res)
  return (await res.json()) as T
}

export async function supplyPost<T>(path: string, body: unknown): Promise<T> {
  const res = await apiFetch(`${SUPPLY_BASE}${path}`, {
    method: 'POST',
    body: JSON.stringify(body),
  })
  if (!res.ok) throw await parseError(res)
  return (await res.json()) as T
}

// ═══════════════════════════════════════════════════════════════════════════
// DISPLAY PRIMITIVES
// ═══════════════════════════════════════════════════════════════════════════

/**
 * Health colour — a straight MAP of the API's `health` string onto theme
 * tokens. It does NOT recompute the rule (§6 / dripHealthColour).
 */
export const healthColor = (health: string): string => {
  switch (health) {
    case 'red': return colors.danger
    case 'amber': return colors.warning
    case 'green': return colors.success
    case 'grey': return colors.idle
    default: return colors.textFaint
  }
}

/** "unknown" — the ONLY rendering of a null number on this screen. */
export const Unknown: React.FC<{ hint?: string }> = ({ hint }) => (
  <span
    style={{ color: colors.textFaint, fontStyle: 'italic', fontVariantNumeric: 'normal' }}
    title={hint ?? 'unknown — the mediator did not record this number for this day. Not zero.'}
  >
    unknown
  </span>
)

const LABEL_HELP: Record<string, string> = {
  contracted: 'contracted — the static contract\'s policy value (changes only at a Denver midnight)',
  effective: 'effective — contracted after governors reduced it (governors reduce, never raise)',
  planned: 'planned — the daily planner\'s frozen award, not yet capacity consumed',
  reserved: 'reserved — capacity held by a reservation, not yet submitted to a transport',
  actual: 'actual — measured after the fact (committed / recorded / ledgered)',
  forecast: 'forecast — expected, not measured (pending EO, expected arrivals)',
}

/** Human help text for a label, for the title= of every numeric cell. */
export const labelHelp = (label: string | undefined): string =>
  label ? (LABEL_HELP[label] ?? `${label} — see METRIC_CONTRACT.md §11`) : 'no label declared by the API for this number'

/** Small uppercase label chip, e.g. CONTRACTED / EFFECTIVE. */
export const LabelChip: React.FC<{ label: string | undefined }> = ({ label }) => {
  if (!label) return null
  return (
    <span
      title={labelHelp(label)}
      style={{
        marginLeft: 6, fontSize: 9, letterSpacing: 0.6, textTransform: 'uppercase',
        color: colors.indigo300, background: alpha(colors.indigo500, '14'),
        border: `1px solid ${alpha(colors.indigo500, '33')}`, borderRadius: 4, padding: '1px 5px',
        fontWeight: 700, verticalAlign: 'middle',
      }}
    >
      {label}
    </span>
  )
}

export const fmtInt = (n: number | null | undefined): string =>
  n == null ? '' : n.toLocaleString()

export const fmtUSD = (n: number | null | undefined, dp = 2): string =>
  n == null ? '' : `$${n.toLocaleString(undefined, { minimumFractionDigits: dp, maximumFractionDigits: dp })}`

export const fmtPct = (n: number | null | undefined): string =>
  n == null ? '' : `${(n * 100).toFixed(0)}%`

export const fmtTime = (iso: string | null | undefined): string => {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString('en-US', { timeZone: 'America/Denver', hour12: false })
}

export const fmtClock = (iso: string | null | undefined): string => {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleTimeString('en-US', { timeZone: 'America/Denver', hour12: false })
}

/**
 * Num — one numeric cell. `value` null renders <Unknown/>; every cell carries
 * its label (and the label's meaning) in the title attribute, per the operator
 * rule "every number is labelled contracted | effective | planned | reserved |
 * actual | forecast".
 */
export const Num: React.FC<{
  value: number | null | undefined
  label?: string | undefined
  format?: (n: number) => string
  what?: string
  unknownHint?: string
  color?: string
}> = ({ value, label, format, what, unknownHint, color }) => {
  if (value == null) return <Unknown {...(unknownHint ? { hint: unknownHint } : {})} />
  const rendered = format ? format(value) : value.toLocaleString()
  const title = [what, label ? labelHelp(label) : null].filter(Boolean).join(' · ')
  return (
    <span title={title || undefined} style={color ? { color } : undefined}>
      {rendered}
    </span>
  )
}

/** Muted reason text under / beside a number (unserved_reason, effective_reason…). */
export const Reason: React.FC<{ text: string; prefix?: string }> = ({ text, prefix }) => {
  if (!text) return null
  return (
    <div style={{ fontSize: 10, color: colors.textFaint, marginTop: 1 }} title={text}>
      {prefix ? `${prefix} ` : ''}{text}
    </div>
  )
}

/** Wide tables scroll inside their own container — the page body never does. */
export const ScrollX: React.FC<{ children: React.ReactNode; maxHeight?: number }> = ({ children, maxHeight }) => (
  <div style={{ overflowX: 'auto', ...(maxHeight ? { maxHeight, overflowY: 'auto' } : {}) }}>{children}</div>
)

/**
 * HeaderStrip — as_of, freshness, contract versions and the API's `degraded`
 * notes. Rendered on EVERY pane: a supply number without its as_of is a number
 * about an unknown moment, and a `degraded` note is the difference between
 * "measured empty" and "could not measure".
 */
export const HeaderStrip: React.FC<{
  meta: SupplyMeta | null
  freshness?: Freshness | null
  extra?: React.ReactNode
}> = ({ meta, freshness, extra }) => {
  if (!meta) return null
  const versions = Object.entries(meta.contract_versions ?? {})
  return (
    <div
      style={{
        display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 14,
        fontSize: 11, color: colors.textMuted, marginBottom: 10,
        padding: '7px 10px', borderRadius: 8,
        background: alpha(colors.indigo500, '0d'),
        border: `1px solid ${colors.hairline}`,
      }}
    >
      <span title="The Denver operating day this whole pane is scoped to.">
        day <strong style={{ color: colors.heading }}>{meta.day}</strong> · America/Denver
      </span>
      <span title="Server time the response was assembled (as_of). Every number on this pane is as of this instant.">
        as_of <strong style={{ color: colors.heading }}>{fmtTime(meta.as_of)}</strong>
      </span>
      {freshness && (
        <>
          <span title="Newest last_refill_tick across the day's domain×ISP capacity balances — how fresh the capacity side is.">
            balances {freshness.balances ? fmtClock(freshness.balances) : <Unknown hint="no balance row carried a refill tick for this day" />}
          </span>
          <span title="Newest updated_at in the capacity ledger for this day — how fresh the allocation side is.">
            ledger {freshness.ledger ? fmtClock(freshness.ledger) : <Unknown hint="no capacity-ledger row for this day" />}
          </span>
        </>
      )}
      {versions.length > 0 && (
        <span title="The active contract versions this projection was computed against.">
          contracts{' '}
          {versions.map(([k, v]) => (
            <strong key={k} style={{ color: colors.indigo200, marginLeft: 5 }}>{k} v{v}</strong>
          ))}
        </span>
      )}
      {extra}
      {(meta.degraded ?? []).length > 0 && (
        <div style={{ flexBasis: '100%', display: 'flex', flexDirection: 'column', gap: 3, marginTop: 2 }}>
          {(meta.degraded ?? []).map((d, i) => (
            <span key={i} style={{ color: colors.warningText, fontSize: 11 }}>
              could not measure: {d}
            </span>
          ))}
        </div>
      )}
    </div>
  )
}

/** Loading row — a labelled spinner, never a bare blank pane. */
export const LoadingRow: React.FC<{ what: string }> = ({ what }) => (
  <div style={{ padding: '22px 10px', color: colors.textMuted, fontSize: 12 }}>Loading {what}…</div>
)

// Re-export the table style objects so the panes import ONE module.
export { tdStyle, thStyle, numTd, numTh, tableStyle }
