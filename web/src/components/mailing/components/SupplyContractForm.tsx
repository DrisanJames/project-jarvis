// SupplyContractForm.tsx — draft + approve for the four contract kinds
// (REQ-118 §1.1/§1.5, §6, WP10).
//
// A contract is POLICY: it changes only by an operator action and only at the
// next Denver midnight. So this form is deliberately blunt about state:
//
//   · Save files a DRAFT (POST /supply/contracts/{kind}/{subject}). Nothing
//     mails differently yet.
//   · Approve moves that draft draft → approved → scheduled and mints its
//     integrity token. It becomes ACTIVE at the next Denver midnight — the
//     button says exactly that ("Schedule for tomorrow").
//   · The API validates first and returns the WHOLE field list it rejected
//     ({error, fields:[…]}); those fields are marked inline here.
//
// THE PER-ISP CAP EDITOR is a three-way control per ISP — Off / Lane target / N
// — never a blank number, because a blank that posts as 0 is a silent estate
// stop. "Lane target" reuses the value the mediator is running on today; when
// that value is unknown the control REFUSES to resolve and blocks the save
// instead of substituting 0.
//
// PREFILL: GET /supply/contracts/{kind}/{subject} now returns each version's
// policy `body` (Contract.TokenBody — the exact set of fields POST accepts), so
// the editor opens on WHAT IS RUNNING, not on schema defaults. The prefill
// source is the ACTIVE version, else the scheduled one, else the newest version
// that carries a body; the pane names which. The schema-defaults banner is
// shown ONLY when no body could be sourced — i.e. no version exists at all (or
// the API could not re-read one), which is the only case where the fields below
// really are defaults.
//
// REJECT: a draft / approved / scheduled version can be rejected from the
// version history (status → superseded, reason appended to notes). `active` and
// `superseded` are refused by the API with 409 and that message is shown
// VERBATIM — a live contract is replaced by scheduling its successor, never
// rejected out from under the estate.

import React from 'react'
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome'
import { faFileContract, faCircleCheck, faTriangleExclamation, faBan } from '@fortawesome/free-solid-svg-icons'
import { colors, alpha } from '../shared/theme'
import { SectionError, Pill } from '../shared/ui'
import { useToast } from '../shared/ToastSystem'
import {
  ContractKind, ContractsResponse, ContractVersionRow, ContractBody, SupplyError, ISP_CLASSES,
  HEALTH_BANDS, HEALTH_BAND_EFFECT,
  supplyGet, supplyPost, Unknown, ScrollX, fmtTime, healthColor,
  tableStyle, thStyle, tdStyle,
} from './supplyShared'

// ═══════════════════════════════════════════════════════════════════════════
// FIELD SPECS — one row per policy field, mirroring the Go struct tags
// ═══════════════════════════════════════════════════════════════════════════

type FieldType = 'int' | 'nullable-int' | 'num' | 'text' | 'bool' | 'time' | 'select' | 'csv' | 'band'

interface FieldSpec {
  key: string
  label: string
  help: string
  type: FieldType
  def: string
  options?: string[]
  width?: number
}

const DOMAIN_FIELDS: FieldSpec[] = [
  { key: 'brand_code', label: 'brand_code', help: 'The 2–3 letter brand code this sending domain belongs to. Required.', type: 'text', def: '', width: 90 },
  { key: 'active_window_start', label: 'active_window_start', help: 'First minute of the day this domain may send. The token bucket refills only inside the window; outside it every reservation is granted 0 with binding_reason=outside_window.', type: 'time', def: '01:00' },
  { key: 'active_window_end', label: 'active_window_end', help: 'Last minute of the sending window. Must be after the start.', type: 'time', def: '20:00' },
  { key: 'interval_minutes', label: 'interval_minutes', help: 'Bucket refill interval. refill = effective_daily ÷ ((end − start) ÷ interval). Must be > 0 — bucket.go divides by it.', type: 'int', def: '15', width: 80 },
  { key: 'max_burst_intervals', label: 'max_burst_intervals', help: 'Ceiling on accumulated tokens, in intervals. Bounds the catch-up burst after scheduler downtime. Must be ≥ 1.', type: 'int', def: '2', width: 80 },
  { key: 'ramp_source', label: 'ramp_source', help: 'Who proposes this domain\'s ramp: the sending-domain cards job, or the operator. Blank leaves it unset.', type: 'select', def: '', options: ['', 'sending_domain_cards', 'operator'] },
  { key: 'health_band', label: 'health_band', help: 'POLICY, not an inferred verdict. green = no band ceiling · amber = the governor halves every cell · red = the governor takes every cell to 0. Moving off green requires notes naming the operator ruling, and a band change re-issues the integrity token.', type: 'band', def: 'green' },
  { key: 'ramp_stage', label: 'ramp_stage', help: 'Free text from the ramp job or the operator — display only. No mediator reads it; it never changes a number.', type: 'text', def: '', width: 160 },
]

const DISPATCH_FIELDS: FieldSpec[] = [
  { key: 'operator_priority_tier', label: 'operator_priority_tier', help: '1 = mails first, 3 = last, 9 = test/exploration (capped at the exploration share of each domain\'s intro tokens).', type: 'int', def: '2', width: 70 },
  { key: 'demand_mode', label: 'demand_mode', help: '`target` honours desired_daily_intros. `consume_available` takes what supply allows, up to daily_ceiling.', type: 'select', def: 'target', options: ['target', 'consume_available'] },
  { key: 'daily_ceiling', label: 'daily_ceiling', help: 'Only meaningful with demand_mode=consume_available. Blank = no ceiling (null).', type: 'nullable-int', def: '', width: 100 },
  { key: 'allowed_domains', label: 'allowed_domains', help: 'Comma-separated brand codes (resolved by brand code first, then by sending domain). A lane can only take capacity from these.', type: 'csv', def: '', width: 320 },
  { key: 'isp_exclusions', label: 'isp_exclusions', help: 'Comma-separated ISP classes this lane must never touch. An excluded ISP receives no intro and no EO order.', type: 'csv', def: '', width: 240 },
  { key: 'ladder_touches', label: 'ladder_touches', help: 'Touches in the ladder: one intro plus up to (touches − 1) follow-ups. Each intro is a follow-up liability.', type: 'int', def: '5', width: 70 },
  { key: 'ladder_gap_hours', label: 'ladder_gap_hours', help: 'Minimum hours between touches on the same recipient.', type: 'int', def: '24', width: 70 },
  { key: 'followups_committed', label: 'followups_committed', help: 'ON: due follow-ups are obligations and are reserved BEFORE any discretionary intro. This is the v1 guardrail.', type: 'bool', def: 'true' },
  { key: 'max_intro_share', label: 'max_intro_share', help: 'Ceiling on the share of a domain×ISP\'s effective capacity this lane\'s intros may take (v1 default 0.40).', type: 'num', def: '0.40', width: 80 },
  { key: 'exploration_share', label: 'exploration_share', help: 'Share of a domain\'s intro tokens reserved for tier-9 lanes. Never taken from the follow-up reserve.', type: 'num', def: '0', width: 80 },
]

const INVENTORY_FIELDS: FieldSpec[] = [
  { key: 'accepted_sources', label: 'accepted_sources', help: 'Comma-separated partner_datasets.slug values this lane may draw records from.', type: 'csv', def: '', width: 320 },
  { key: 'verdict_valid_days', label: 'verdict_valid_days', help: 'How long a validation verdict stays good. A record validated longer ago than this is not fresh-mailable.', type: 'int', def: '60', width: 80 },
  { key: 'eo_enabled', label: 'eo_enabled', help: 'OFF stops all EmailOversight ordering for this lane; the lane then mails only what is already validated.', type: 'bool', def: 'true' },
  { key: 'max_daily_eo_spend_usd', label: 'max_daily_eo_spend_usd', help: 'Hard daily cap on validation spend for this lane.', type: 'num', def: '50', width: 90 },
  { key: 'min_eo_order', label: 'min_eo_order', help: 'Smallest order the supply controller will place. Below this it waits.', type: 'int', def: '1000', width: 90 },
  { key: 'min_coverage_hours', label: 'min_coverage_hours', help: 'Below this many hours of mailable inventory, the controller reorders regardless of provisional awards.', type: 'int', def: '8', width: 80 },
  { key: 'target_coverage_hours', label: 'target_coverage_hours', help: 'The coverage the controller aims to hold.', type: 'int', def: '16', width: 80 },
  { key: 'max_coverage_hours', label: 'max_coverage_hours', help: 'Stop ordering above this coverage — inventory ages and verdicts expire.', type: 'int', def: '36', width: 80 },
  { key: 'remail_enabled', label: 'remail_enabled', help: 'Allow previously mailed records back into the intro pool under the rules below.', type: 'bool', def: 'false' },
  { key: 'remail_after_days', label: 'remail_after_days', help: 'How long a mailed record rests before it is remail-eligible.', type: 'int', def: '7', width: 80 },
  { key: 'remail_mode', label: 'remail_mode', help: 'full_ladder replays the whole ladder; single_touch sends one message.', type: 'select', def: 'full_ladder', options: ['full_ladder', 'single_touch'] },
  { key: 'max_remail_share', label: 'max_remail_share', help: 'Ceiling on the share of this lane\'s intro volume that may be remails.', type: 'num', def: '0.25', width: 80 },
]

const SOURCE_FIELDS: FieldSpec[] = [
  { key: 'record_class', label: 'record_class', help: 'What kind of record this source supplies, e.g. auto_insurance, mortgage. Economics inherit the estate median for the class when a lane\'s own sample is too small.', type: 'text', def: '', width: 160 },
  { key: 'eligible_isps', label: 'eligible_isps', help: 'Comma-separated ISP classes this source may supply into.', type: 'csv', def: '', width: 260 },
  { key: 'max_daily_intake', label: 'max_daily_intake', help: 'Cap on records accepted per day. Blank = no cap (null).', type: 'nullable-int', def: '', width: 110 },
  { key: 'arrival_cadence', label: 'arrival_cadence', help: 'How records arrive — continuous, or in batches.', type: 'select', def: 'continuous', options: ['continuous', 'batch'] },
  { key: 'validated_on_arrival', label: 'validated_on_arrival', help: 'ON: records arrive already validated, so the supply controller does not order EO for them.', type: 'bool', def: 'false' },
  { key: 'record_max_age_days', label: 'record_max_age_days', help: 'Refuse records older than this at ingest. Blank = no age limit (null).', type: 'nullable-int', def: '', width: 110 },
  { key: 'unit_acquisition_cost', label: 'unit_acquisition_cost', help: 'What one raw record from this source costs. Feeds cleaning value and fully-loaded eCPM.', type: 'num', def: '0', width: 110 },
]

const FIELDS: Record<ContractKind, FieldSpec[]> = {
  domain: DOMAIN_FIELDS,
  dispatch: DISPATCH_FIELDS,
  inventory: INVENTORY_FIELDS,
  source: SOURCE_FIELDS,
}

/** Which contract kinds carry a per-ISP integer map, and under which JSON key. */
const ISP_MAP_KEY: Partial<Record<ContractKind, string>> = {
  domain: 'daily_max_by_isp',
  dispatch: 'desired_daily_intros',
}

const KIND_TITLE: Record<ContractKind, string> = {
  domain: 'Sending-domain contract',
  dispatch: 'Dispatch contract',
  inventory: 'Inventory contract',
  source: 'Source supply contract',
}

const KIND_BLURB: Record<ContractKind, string> = {
  domain: 'What this sending domain may send per ISP per day, and inside which window. The mediator can only reduce these numbers (governors), never raise them.',
  dispatch: 'What this lane WANTS: desired intros per ISP, which domains it may take capacity from, its ladder, and its guardrails.',
  inventory: 'How this lane REPLENISHES: which sources it accepts, its verdict window, its EO budget and coverage band, and its remail policy. No sending target lives here.',
  source: 'What one source supplies: record class, eligible ISPs, intake cap, arrival shape and unit acquisition cost.',
}

// ═══════════════════════════════════════════════════════════════════════════
// PREFILL — the form opens on the running policy, not on schema defaults
// ═══════════════════════════════════════════════════════════════════════════

/**
 * Which version the editor prefills from: the ACTIVE one (what the mediators
 * are honouring right now), else the scheduled one (what takes over at the next
 * Denver midnight), else the newest version that actually carries a body.
 *
 * A version whose `body` is null is SKIPPED, never treated as an empty policy —
 * the API leaves it null when the row could not be re-read, and prefilling
 * zeroes off that would be a silent estate edit.
 */
const pickPrefillSource = (versions: ContractVersionRow[]): ContractVersionRow | null => {
  const withBody = versions.filter(v => v.body != null)
  return withBody.find(v => v.status === 'active')
    ?? withBody.find(v => v.status === 'scheduled')
    ?? withBody.reduce<ContractVersionRow | null>((best, v) => (best == null || v.version > best.version ? v : best), null)
}

/** One policy value → the string the matching input renders. */
const bodyToFormValue = (spec: FieldSpec, raw: unknown): string => {
  switch (spec.type) {
    case 'bool':
      return raw === true ? 'true' : 'false'
    case 'csv':
      return Array.isArray(raw) ? raw.map(v => String(v)).join(', ') : ''
    case 'nullable-int':
      return raw == null ? '' : String(raw)
    case 'time':
      // The Go side normalises a clock to HH:MM (normClock) precisely so the
      // token survives the round trip; <input type="time"> wants the same.
      return typeof raw === 'string' ? raw.slice(0, 5) : ''
    default:
      return raw == null ? '' : String(raw)
  }
}

/** The per-ISP map out of a policy body, or null when the body carries none. */
const bodyISPMap = (body: ContractBody, mapKey: string): Record<string, number> | null => {
  const raw = body[mapKey]
  if (raw == null || typeof raw !== 'object' || Array.isArray(raw)) return null
  const out: Record<string, number> = {}
  Object.entries(raw as Record<string, unknown>).forEach(([isp, v]) => {
    const n = typeof v === 'number' ? v : Number(v)
    if (Number.isFinite(n)) out[isp] = n
  })
  return out
}

// ═══════════════════════════════════════════════════════════════════════════
// STYLES
// ═══════════════════════════════════════════════════════════════════════════

const inputStyle: React.CSSProperties = {
  background: colors.appBgSolid, color: colors.text,
  border: `1px solid ${colors.panelBorderStrong}`, borderRadius: 6,
  padding: '6px 9px', fontSize: 12, outline: 'none',
}
const fieldLabelStyle: React.CSSProperties = {
  display: 'flex', flexDirection: 'column', gap: 4,
  fontSize: 10, color: colors.textFaint, textTransform: 'uppercase', letterSpacing: 0.5,
}
const segBtn = (active: boolean, tone: string): React.CSSProperties => ({
  border: 'none', borderRight: `1px solid ${colors.hairline}`,
  padding: '4px 9px', fontSize: 11, cursor: 'pointer', outline: 'none', whiteSpace: 'nowrap',
  background: active ? alpha(tone, '33') : 'transparent',
  color: active ? tone : colors.textMuted,
  fontWeight: active ? 700 : 400,
})

// ═══════════════════════════════════════════════════════════════════════════
// PER-ISP CAP EDITOR — Off / Lane target / N. Never a blank number.
// ═══════════════════════════════════════════════════════════════════════════

type CapMode = 'off' | 'target' | 'n'
interface CapState { mode: CapMode; n: string }

const ISPCapEditor: React.FC<{
  mapKey: string
  kind: ContractKind
  state: Record<string, CapState>
  seed: Record<string, number | null>
  provenance: string
  invalid: boolean
  onChange: (isp: string, next: CapState) => void
}> = ({ mapKey, kind, state, seed, provenance, invalid, onChange }) => (
  <div style={{ marginTop: 12 }}>
    <div style={{ display: 'flex', alignItems: 'baseline', gap: 10, flexWrap: 'wrap', marginBottom: 6 }}>
      <span style={{ fontSize: 11, color: colors.heading, fontWeight: 700, letterSpacing: 0.5, textTransform: 'uppercase' }}>
        {mapKey}
      </span>
      {invalid && <Pill color={colors.danger} style={{ fontSize: 9 }}>rejected by the API</Pill>}
    </div>
    <div style={{ fontSize: 11, color: colors.textMuted, marginBottom: 8 }}>
      {kind === 'domain'
        ? 'All twelve ISP classes must carry an explicit value — a missing key is a save error, never a default. Off means a hard 0 for that ISP.'
        : 'An ISP set to Off is absent from the contract, which the planner reads as "not wanted" (0 desired). Only ISPs you fund are planned.'}
      {' '}Lane target reuses today\'s live value ({provenance}); where that value is unknown the control refuses to resolve rather than posting 0.
    </div>
    <ScrollX>
      <table style={tableStyle}>
        <thead>
          <tr>
            <th style={thStyle}>ISP</th>
            <th style={thStyle}>Mode</th>
            <th style={thStyle} title="The value that will be POSTed for this ISP.">Value posted</th>
            <th style={thStyle} title="Today's live value for this ISP, from the active contract as the mediator projected it.">Lane target today</th>
          </tr>
        </thead>
        <tbody>
          {ISP_CLASSES.map(isp => {
            const st = state[isp] ?? { mode: 'off' as CapMode, n: '' }
            const target = seed[isp]
            const targetUnknown = st.mode === 'target' && target == null
            let posted: React.ReactNode
            if (st.mode === 'off') posted = <span style={{ color: colors.idle }}>{kind === 'domain' ? '0' : 'omitted (0 desired)'}</span>
            else if (st.mode === 'target') {
              posted = targetUnknown
                ? <span style={{ color: colors.dangerText, fontWeight: 700 }}>unresolvable — pick Off or N</span>
                : <strong style={{ color: colors.heading }}>{target?.toLocaleString()}</strong>
            } else {
              posted = st.n.trim() === ''
                ? <span style={{ color: colors.dangerText, fontWeight: 700 }}>enter a number</span>
                : <strong style={{ color: colors.heading }}>{Number(st.n).toLocaleString()}</strong>
            }
            return (
              <tr key={isp}>
                <td style={tdStyle}><strong style={{ color: colors.heading }}>{isp}</strong></td>
                <td style={tdStyle}>
                  <div style={{ display: 'inline-flex', background: colors.appBgSolid, border: `1px solid ${colors.panelBorderStrong}`, borderRadius: 6, overflow: 'hidden' }}>
                    <button type="button" style={segBtn(st.mode === 'off', colors.idle)} onClick={() => onChange(isp, { ...st, mode: 'off' })} title="Hard zero for this ISP.">Off</button>
                    <button type="button" style={segBtn(st.mode === 'target', colors.indigo400)} onClick={() => onChange(isp, { ...st, mode: 'target' })} title="Keep today's live value for this ISP.">Lane target</button>
                    <button type="button" style={segBtn(st.mode === 'n', colors.success)} onClick={() => onChange(isp, { ...st, mode: 'n' })} title="Type an explicit number.">N</button>
                  </div>
                  {st.mode === 'n' && (
                    <input
                      type="number"
                      min={0}
                      value={st.n}
                      onChange={e => onChange(isp, { ...st, n: e.target.value })}
                      placeholder="explicit value"
                      style={{ ...inputStyle, width: 120, marginLeft: 8 }}
                    />
                  )}
                </td>
                <td style={{ ...tdStyle, fontVariantNumeric: 'tabular-nums' }}>{posted}</td>
                <td style={{ ...tdStyle, fontVariantNumeric: 'tabular-nums', color: colors.textMuted }}>
                  {target == null ? <Unknown hint="no live value for this ISP today — Lane target cannot resolve here" /> : target.toLocaleString()}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </ScrollX>
  </div>
)

// ═══════════════════════════════════════════════════════════════════════════
// THE FORM
// ═══════════════════════════════════════════════════════════════════════════

export const SupplyContractForm: React.FC<{
  kind: ContractKind
  subject: string
  seedISPValues?: Record<string, number | null>
  seedISPProvenance?: string
  onChanged?: () => void
}> = ({ kind, subject, seedISPValues, seedISPProvenance, onChanged }) => {
  const toast = useToast()
  const specs = FIELDS[kind]
  const mapKey = ISP_MAP_KEY[kind]
  const seed = seedISPValues ?? {}

  const [versions, setVersions] = React.useState<ContractsResponse | null>(null)
  const [versionsErr, setVersionsErr] = React.useState<string | null>(null)
  const [nonce, setNonce] = React.useState(0)
  const [open, setOpen] = React.useState(false)
  const [busy, setBusy] = React.useState(false)
  const [err, setErr] = React.useState<string | null>(null)
  const [badFields, setBadFields] = React.useState<string[]>([])
  const [savedVersion, setSavedVersion] = React.useState<number | null>(null)

  const [values, setValues] = React.useState<Record<string, string>>(() =>
    Object.fromEntries(specs.map(s => [s.key, s.def])))
  const [notes, setNotes] = React.useState('')
  const [caps, setCaps] = React.useState<Record<string, CapState>>(() =>
    Object.fromEntries(ISP_CLASSES.map(i => [i, { mode: 'target' as CapMode, n: '' }])))

  React.useEffect(() => {
    const ctrl = new AbortController()
    supplyGet<ContractsResponse>(`/contracts/${kind}/${encodeURIComponent(subject)}`, {}, ctrl.signal)
      .then(d => { setVersions(d); setVersionsErr(null) })
      .catch(e => { if (!ctrl.signal.aborted) setVersionsErr(e instanceof Error ? e.message : String(e)) })
    return () => ctrl.abort()
  }, [kind, subject, nonce])

  // ── Prefill from the running policy ──────────────────────────────────────
  // The source version is re-picked on every refresh, but the fields are seeded
  // only when that source CHANGES (`seededFrom`), so a background refresh can
  // never wipe half-typed operator edits.
  const prefillSource = React.useMemo(
    () => pickPrefillSource(versions?.versions ?? []),
    [versions],
  )
  const [seededFrom, setSeededFrom] = React.useState<string | null>(null)

  React.useEffect(() => {
    if (!prefillSource || prefillSource.body == null) return
    const key = `${kind}|${subject}|${prefillSource.id}`
    if (seededFrom === key) return
    const body = prefillSource.body
    setValues(Object.fromEntries(specs.map(s => [
      s.key,
      Object.prototype.hasOwnProperty.call(body, s.key) ? bodyToFormValue(s, body[s.key]) : s.def,
    ])))
    if (mapKey) {
      const m = bodyISPMap(body, mapKey)
      if (m) {
        setCaps(Object.fromEntries(ISP_CLASSES.map(isp => {
          const v = m[isp]
          // A domain contract carries every ISP explicitly; an ISP absent from a
          // dispatch contract means "not wanted" (0 desired) — which is Off, not
          // an explicit 0. Keeping the two apart is the whole point of the control.
          if (v == null) return [isp, { mode: 'off' as CapMode, n: '' }]
          if (v === 0 && kind !== 'domain') return [isp, { mode: 'off' as CapMode, n: '' }]
          return [isp, { mode: 'n' as CapMode, n: String(v) }]
        })))
      }
    }
    // notes is deliberately NOT prefilled: it is this version's justification,
    // and an amber/red band or gmail > 0 must be re-argued, not inherited.
    setSeededFrom(key)
  }, [prefillSource, seededFrom, kind, subject, specs, mapKey])

  const bad = (f: string) => badFields.includes(f)

  /**
   * The band's notes rule, mirrored from DomainContract.Validate: amber/red
   * require notes naming the operator ruling, and the API rejects with
   * field `health_band`. Shown inline so the operator learns it before the
   * round trip — the API stays the authority that enforces it.
   */
  const bandNeedsNotes =
    kind === 'domain'
    && (values['health_band'] === 'amber' || values['health_band'] === 'red')
    && notes.trim() === ''

  // A "Lane target" cell with no live value, or an "N" cell with an empty box,
  // is not a number — the save is blocked rather than silently posting 0.
  const unresolved = React.useMemo(() => {
    if (!mapKey) return []
    return ISP_CLASSES.filter(isp => {
      const st = caps[isp]
      if (!st) return false
      if (st.mode === 'target') return seed[isp] == null
      if (st.mode === 'n') return st.n.trim() === '' || !Number.isFinite(Number(st.n)) || Number(st.n) < 0
      return false
    })
  }, [caps, seed, mapKey])

  const buildBody = (): Record<string, unknown> => {
    const body: Record<string, unknown> = { notes }
    specs.forEach(s => {
      const raw = (values[s.key] ?? s.def).trim()
      switch (s.type) {
        case 'int':
          body[s.key] = Number.parseInt(raw === '' ? s.def : raw, 10)
          break
        case 'nullable-int':
          body[s.key] = raw === '' ? null : Number.parseInt(raw, 10)
          break
        case 'num':
          body[s.key] = Number(raw === '' ? s.def : raw)
          break
        case 'bool':
          body[s.key] = raw === 'true'
          break
        case 'csv':
          body[s.key] = raw === '' ? [] : raw.split(',').map(v => v.trim()).filter(Boolean)
          break
        default:
          body[s.key] = raw
      }
    })
    if (mapKey) {
      const m: Record<string, number> = {}
      ISP_CLASSES.forEach(isp => {
        const st = caps[isp] ?? { mode: 'off' as CapMode, n: '' }
        if (st.mode === 'off') {
          // A domain contract must carry every ISP class explicitly, so Off is
          // an explicit 0. A dispatch contract reads an ABSENT ISP as 0 desired,
          // so Off omits the key — the two are not interchangeable.
          if (kind === 'domain') m[isp] = 0
          return
        }
        if (st.mode === 'target') { const t = seed[isp]; if (t != null) m[isp] = t; return }
        m[isp] = Number(st.n)
      })
      body[mapKey] = m
    }
    return body
  }

  const saveDraft = async () => {
    setBusy(true); setErr(null); setBadFields([]); setSavedVersion(null)
    try {
      if (unresolved.length > 0) {
        setErr(`Unresolved per-ISP values for: ${unresolved.join(', ')}. Pick Off (an explicit 0) or type a number — this form will not post a blank as 0.`)
        if (mapKey) setBadFields([mapKey])
        return
      }
      const res = await supplyPost<{ version: number }>(`/contracts/${kind}/${encodeURIComponent(subject)}`, buildBody())
      setSavedVersion(res.version)
      toast.addToast({
        type: 'success',
        title: `${KIND_TITLE[kind]} draft v${res.version} filed`,
        message: 'Nothing changed yet — approve it to schedule for the next Denver midnight.',
      })
      setNonce(n => n + 1)
      onChanged?.()
    } catch (e) {
      if (e instanceof SupplyError) { setErr(e.message); setBadFields(e.fields) }
      else setErr(e instanceof Error ? e.message : String(e))
    } finally { setBusy(false) }
  }

  const approve = async (version: number) => {
    setBusy(true); setErr(null)
    try {
      await supplyPost(`/contracts/${kind}/${encodeURIComponent(subject)}/${version}/approve`, {})
      toast.addToast({
        type: 'success',
        title: `${KIND_TITLE[kind]} v${version} scheduled`,
        message: 'It becomes ACTIVE at the next Denver midnight, never immediately.',
      })
      setSavedVersion(null)
      setNonce(n => n + 1)
      onChanged?.()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally { setBusy(false) }
  }

  /**
   * Reject a draft / approved / scheduled version: status → superseded, the
   * reason appended to notes, and a scheduled version never activates.
   *
   * The API refuses `active` and `superseded` with 409 and an explanation of
   * WHY; that message is surfaced verbatim rather than being re-worded here —
   * the server owns the rule, this pane displays it.
   */
  const reject = async (version: number, status: string) => {
    const ok = window.confirm(
      `Reject ${kind} contract ${subject} v${version} (currently ${status})?\n\n`
      + 'It is marked SUPERSEDED, the rejection is appended to its notes, and a scheduled '
      + 'version will never activate. This cannot be undone — file a new draft instead.',
    )
    if (!ok) return
    setBusy(true); setErr(null); setBadFields([])
    try {
      await supplyPost(`/contracts/${kind}/${encodeURIComponent(subject)}/${version}/reject`, {})
      toast.addToast({
        type: 'success',
        title: `${KIND_TITLE[kind]} v${version} rejected`,
        message: 'Marked superseded. It will never activate.',
      })
      if (savedVersion === version) setSavedVersion(null)
      setNonce(n => n + 1)
      onChanged?.()
    } catch (e) {
      // 409 (active / superseded) carries the API's own explanation — verbatim.
      if (e instanceof SupplyError) { setErr(e.message); setBadFields(e.fields) }
      else setErr(e instanceof Error ? e.message : String(e))
    } finally { setBusy(false) }
  }

  const drafts = (versions?.versions ?? []).filter(v => v.status === 'draft')
  /** The three lifecycle states the API accepts a rejection from (dripRejectableStatuses). */
  const rejectable = (status: string) => status === 'draft' || status === 'approved' || status === 'scheduled'

  return (
    <div style={{ border: `1px solid ${colors.hairline}`, borderRadius: 8, padding: '12px 14px' }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 10, flexWrap: 'wrap' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 9, flexWrap: 'wrap' }}>
          <FontAwesomeIcon icon={faFileContract} style={{ color: colors.indigo400 }} />
          <strong style={{ color: colors.heading, fontSize: 13 }}>{KIND_TITLE[kind]}</strong>
          <code style={{ fontSize: 11, color: colors.textMuted }}>{subject}</code>
          <span style={{ fontSize: 11, color: colors.textMuted }} title="The version the mediators are honouring right now.">
            active{' '}
            {versions?.active_version != null
              ? <strong style={{ color: colors.successText }}>v{versions.active_version}</strong>
              : <Unknown hint="no ACTIVE contract for this subject — the executor fails closed on it (skipped:no_contract)" />}
          </span>
          <span style={{ fontSize: 11, color: colors.textMuted }} title="Approved and waiting for the next Denver midnight.">
            scheduled{' '}
            {versions?.scheduled_version != null
              ? <strong style={{ color: colors.indigo200 }}>v{versions.scheduled_version}</strong>
              : <span style={{ color: colors.textFaint }}>none</span>}
          </span>
        </div>
        <button
          type="button"
          onClick={() => setOpen(o => !o)}
          style={{
            background: alpha(colors.indigo500, '22'), border: `1px solid ${alpha(colors.indigo500, '66')}`,
            color: colors.indigo200, borderRadius: 6, padding: '4px 10px', fontSize: 11, fontWeight: 600, cursor: 'pointer',
          }}
        >
          {open ? 'Close' : 'Draft a new version'}
        </button>
      </div>

      <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 6 }}>{KIND_BLURB[kind]}</div>

      {versionsErr && (
        <div style={{ marginTop: 8 }}><SectionError label="Contract versions" error={versionsErr} onRetry={() => setNonce(n => n + 1)} /></div>
      )}

      {open && (
        <div style={{ marginTop: 12 }}>
          {prefillSource ? (
            <div
              style={{
                fontSize: 11, color: colors.indigo200, background: alpha(colors.indigo500, '0d'),
                border: `1px solid ${alpha(colors.indigo500, '33')}`, borderRadius: 6, padding: '7px 10px', marginBottom: 12,
              }}
              title="The policy body of that version — the exact field set its integrity token covers — as returned by GET /supply/contracts."
            >
              <FontAwesomeIcon icon={faFileContract} style={{ marginRight: 6 }} />
              Prefilled from <strong>v{prefillSource.version}</strong> ({prefillSource.status}) — the fields below are the
              policy that version carries, not schema defaults. Edit what you mean to change; everything else is filed
              again unchanged. Nothing mails differently until a version is approved AND the next Denver midnight passes.
              {' '}<span style={{ color: colors.textFaint }}>notes are not carried over — this version needs its own.</span>
            </div>
          ) : (
            <div
              style={{
                fontSize: 11, color: colors.warningText, background: 'rgba(245,158,11,0.08)',
                border: '1px solid rgba(245,158,11,0.25)', borderRadius: 6, padding: '7px 10px', marginBottom: 12,
              }}
            >
              <FontAwesomeIcon icon={faTriangleExclamation} style={{ marginRight: 6 }} />
              {(versions?.versions ?? []).length === 0
                ? 'No version of this contract exists yet, so the fields below are the SCHEMA DEFAULTS — nothing is running to prefill from. Set every field deliberately.'
                : 'No version could be re-read for its policy body, so the fields below fell back to the SCHEMA DEFAULTS — they are NOT what is running today. Refresh, and set every field deliberately before you save.'}
              {' '}Drafting is safe: nothing mails differently until a version is approved AND the next Denver midnight passes.
            </div>
          )}

          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 12, alignItems: 'flex-end' }}>
            {specs.map(s => (
              <label key={s.key} style={fieldLabelStyle} title={s.help}>
                {s.label}
                {s.type === 'band' ? (
                  <div>
                    <div style={{ display: 'inline-flex', background: colors.appBgSolid, border: `1px solid ${bad(s.key) ? colors.danger : colors.panelBorderStrong}`, borderRadius: 6, overflow: 'hidden' }}>
                      {HEALTH_BANDS.map(b => (
                        <button
                          key={b}
                          type="button"
                          style={segBtn((values[s.key] ?? s.def) === b, healthColor(b))}
                          onClick={() => setValues(v => ({ ...v, [s.key]: b }))}
                          title={HEALTH_BAND_EFFECT[b]}
                        >
                          {b}
                        </button>
                      ))}
                    </div>
                    <div style={{ fontSize: 10, color: colors.textMuted, marginTop: 4, textTransform: 'none', letterSpacing: 0, maxWidth: 420 }}>
                      {HEALTH_BAND_EFFECT[values[s.key] ?? s.def] ?? ''}
                    </div>
                    {bandNeedsNotes && (
                      <div
                        style={{ fontSize: 10, color: colors.dangerText, marginTop: 3, textTransform: 'none', letterSpacing: 0, maxWidth: 420, fontWeight: 700 }}
                        title="DomainContract.Validate rejects this with field health_band."
                      >
                        health_band &quot;{values[s.key]}&quot; requires notes naming the operator ruling — the API will reject
                        this draft on field <code>health_band</code> until notes are filled in below.
                      </div>
                    )}
                  </div>
                ) : s.type === 'bool' ? (
                  <div style={{ display: 'inline-flex', background: colors.appBgSolid, border: `1px solid ${bad(s.key) ? colors.danger : colors.panelBorderStrong}`, borderRadius: 6, overflow: 'hidden' }}>
                    <button type="button" style={segBtn(values[s.key] === 'true', colors.success)} onClick={() => setValues(v => ({ ...v, [s.key]: 'true' }))}>On</button>
                    <button type="button" style={segBtn(values[s.key] !== 'true', colors.idle)} onClick={() => setValues(v => ({ ...v, [s.key]: 'false' }))}>Off</button>
                  </div>
                ) : s.type === 'select' ? (
                  <select
                    value={values[s.key] ?? s.def}
                    onChange={e => setValues(v => ({ ...v, [s.key]: e.target.value }))}
                    style={{ ...inputStyle, width: s.width ?? 170, ...(bad(s.key) ? { borderColor: colors.danger } : {}) }}
                  >
                    {(s.options ?? []).map(o => <option key={o} value={o}>{o === '' ? '(unset)' : o}</option>)}
                  </select>
                ) : (
                  <input
                    type={s.type === 'time' ? 'time' : (s.type === 'int' || s.type === 'num' || s.type === 'nullable-int') ? 'number' : 'text'}
                    step={s.type === 'num' ? '0.01' : undefined}
                    value={values[s.key] ?? s.def}
                    placeholder={s.type === 'nullable-int' ? 'blank = no limit (null)' : s.type === 'csv' ? 'comma,separated' : ''}
                    onChange={e => setValues(v => ({ ...v, [s.key]: e.target.value }))}
                    style={{ ...inputStyle, width: s.width ?? 140, ...(bad(s.key) ? { borderColor: colors.danger } : {}) }}
                  />
                )}
              </label>
            ))}
            <label style={{ ...fieldLabelStyle, flex: '1 1 320px' }} title="Required when a domain contract opens gmail above 0, and when its health_band leaves green — name the operator ruling. Always carried into the version history, and appended to (never overwritten) if the version is later rejected.">
              notes
              <input
                type="text"
                value={notes}
                onChange={e => setNotes(e.target.value)}
                placeholder={bandNeedsNotes
                  ? `required: the operator ruling behind health_band ${values['health_band']}`
                  : 'why this version exists (required to open gmail > 0)'}
                style={{ ...inputStyle, ...(bad('notes') || bandNeedsNotes ? { borderColor: colors.danger } : {}) }}
              />
            </label>
          </div>

          {mapKey && (
            <ISPCapEditor
              mapKey={mapKey}
              kind={kind}
              state={caps}
              seed={seed}
              provenance={seedISPProvenance ?? 'the day\'s live balances'}
              invalid={bad(mapKey)}
              onChange={(isp, next) => setCaps(c => ({ ...c, [isp]: next }))}
            />
          )}

          <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginTop: 12, flexWrap: 'wrap' }}>
            <button
              type="button"
              disabled={busy}
              onClick={() => { void saveDraft() }}
              style={{
                background: colors.indigo400, color: '#0a0e1a', border: 'none', borderRadius: 6,
                padding: '8px 16px', fontSize: 12, fontWeight: 700, cursor: busy ? 'wait' : 'pointer', opacity: busy ? 0.6 : 1,
              }}
            >
              {busy ? 'Saving…' : 'Save draft'}
            </button>
            {savedVersion != null && (
              <button
                type="button"
                disabled={busy}
                onClick={() => { void approve(savedVersion) }}
                style={{
                  background: alpha(colors.success, '22'), border: `1px solid ${alpha(colors.success, '66')}`,
                  color: colors.successText, borderRadius: 6, padding: '8px 14px', fontSize: 12, fontWeight: 700, cursor: 'pointer',
                }}
                title="Approve v{savedVersion}: it becomes ACTIVE at the next Denver midnight, never immediately."
              >
                <FontAwesomeIcon icon={faCircleCheck} /> Schedule v{savedVersion} for tomorrow
              </button>
            )}
            {unresolved.length > 0 && (
              <span style={{ fontSize: 11, color: colors.dangerText }}>
                {unresolved.length} ISP{unresolved.length === 1 ? '' : 's'} unresolved: {unresolved.join(', ')}
              </span>
            )}
          </div>

          {err && <div style={{ marginTop: 8 }}><SectionError label="Contract" error={err} /></div>}
        </div>
      )}

      {/* ── Version history ──────────────────────────────────────────── */}
      <div style={{ marginTop: 12 }}>
        {versions == null ? (
          <div style={{ fontSize: 11, color: colors.textFaint }}>Loading version history…</div>
        ) : versions.versions.length === 0 ? (
          <div style={{ fontSize: 11, color: colors.warningText }}>
            No contract of this kind exists for <code>{subject}</code>. The mediators fail CLOSED on a missing contract —
            the executor skips the subject with <code>skipped:no_contract</code> and alerts. Draft one.
          </div>
        ) : (
          <ScrollX maxHeight={240}>
            <table style={tableStyle}>
              <thead>
                <tr>
                  <th style={thStyle}>v</th>
                  <th style={thStyle}>Status</th>
                  <th style={thStyle} title="When this version takes (or took) effect — always a Denver midnight.">Effective</th>
                  <th style={thStyle}>Created</th>
                  <th style={thStyle}>Approved</th>
                  <th style={thStyle} title="A scheduled/active contract carries an HMAC over its policy body. LoadActive REFUSES a contract whose token does not match — a hand-edited row cannot be honoured. The value itself is never sent to the browser.">Token</th>
                  <th style={thStyle} title="The change-ledger id this version was written under.">Change ledger</th>
                  <th style={thStyle}>Notes</th>
                  <th style={thStyle} title="Approve schedules a draft for the next Denver midnight. Reject marks a draft/approved/scheduled version superseded — active and superseded versions are refused by the API with 409.">Actions</th>
                </tr>
              </thead>
              <tbody>
                {versions.versions.map(v => (
                  <tr key={v.id}>
                    <td style={tdStyle}><strong style={{ color: colors.heading }}>v{v.version}</strong></td>
                    <td style={tdStyle}>
                      <Pill
                        color={v.status === 'active' ? colors.success : v.status === 'scheduled' ? colors.indigo400 : v.status === 'draft' ? colors.warning : colors.idle}
                        style={{ fontSize: 9 }}
                      >
                        {v.status}
                      </Pill>
                    </td>
                    <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>{fmtTime(v.effective_at)}</td>
                    <td style={{ ...tdStyle, fontSize: 11 }} title={fmtTime(v.created_at)}>{v.created_by || '—'}</td>
                    <td style={{ ...tdStyle, fontSize: 11 }} title={v.approved_at ? fmtTime(v.approved_at) : 'not approved'}>{v.approved_by || '—'}</td>
                    <td style={tdStyle}>
                      {v.token_present
                        ? <span style={{ color: colors.successText, fontSize: 11 }} title={v.token_issued_at ? `issued ${fmtTime(v.token_issued_at)}` : 'issued'}>present</span>
                        : <span style={{ color: colors.textFaint, fontSize: 11 }} title="A token is minted only on the approved → scheduled edge. A draft has none by design.">none</span>}
                    </td>
                    <td style={{ ...tdStyle, fontFamily: 'monospace', fontSize: 10, maxWidth: 180, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={v.change_ledger_id}>
                      {v.change_ledger_id || '—'}
                    </td>
                    <td style={{ ...tdStyle, fontSize: 11, maxWidth: 240, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={v.notes}>
                      {v.notes || '—'}
                    </td>
                    <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>
                      {v.status === 'draft' && (
                        <button
                          type="button"
                          disabled={busy}
                          onClick={() => { void approve(v.version) }}
                          style={{
                            background: alpha(colors.success, '22'), border: `1px solid ${alpha(colors.success, '66')}`,
                            color: colors.successText, borderRadius: 5, padding: '3px 9px', fontSize: 10, fontWeight: 700, cursor: 'pointer',
                          }}
                          title="Approve and schedule for the next Denver midnight."
                        >
                          Schedule for tomorrow
                        </button>
                      )}
                      {rejectable(v.status) ? (
                        <button
                          type="button"
                          disabled={busy}
                          onClick={() => { void reject(v.version, v.status) }}
                          style={{
                            background: alpha(colors.danger, '22'), border: `1px solid ${alpha(colors.danger, '66')}`,
                            color: colors.dangerText, borderRadius: 5, padding: '3px 9px', fontSize: 10, fontWeight: 700,
                            cursor: 'pointer', marginLeft: v.status === 'draft' ? 6 : 0,
                          }}
                          title="Mark this version superseded. A scheduled version rejected here never activates. The reason is appended to its notes; the version itself is never deleted."
                        >
                          <FontAwesomeIcon icon={faBan} /> Reject
                        </button>
                      ) : (
                        <span
                          style={{ fontSize: 10, color: colors.textFaint }}
                          title={v.status === 'active'
                            ? 'An ACTIVE contract is replaced by scheduling its successor, never rejected out from under the estate — a subject with no active contract is a hard stop (skipped:no_contract).'
                            : 'Already terminal — re-rejecting would move superseded_at and rewrite history.'}
                        >
                          not rejectable
                        </span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </ScrollX>
        )}
        {drafts.length > 0 && (
          <div style={{ fontSize: 11, color: colors.warningText, marginTop: 6 }}>
            {drafts.length} unapproved draft{drafts.length === 1 ? '' : 's'} — a draft changes nothing until it is scheduled.
          </div>
        )}
        {/* A reject fired from the history above happens with the drawer closed,
            so its error (the 409 in particular) needs a home out here — printed
            verbatim, because the API owns the rule it is quoting. */}
        {!open && err && <div style={{ marginTop: 8 }}><SectionError label="Contract" error={err} /></div>}
      </div>
    </div>
  )
}

export default SupplyContractForm
