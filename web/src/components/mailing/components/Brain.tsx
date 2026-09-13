// Brain.tsx — the portal surface for the platform-hosted operational memory
// (internal/brain, served by internal/api/brain_handlers.go on the session
// path /api/mailing/brain/*).
//
// PAGE_VERSION 1 (2026-09-13) — three panes over the brain API:
//
//     Claims  →  Capabilities  →  Evals
//
// Rules this screen honours:
//   - It DISPLAYS; it computes nothing. Every verdict (status, last_pass,
//     required_verifier) is the API's own field, mapped to a theme token.
//   - A `candidate` claim is rendered as UNVERIFIED (amber) — the API's own
//     recall note says candidates are labelled, never hidden.
//   - Absence of data is "no rows", never a zero. Loading / error / empty are
//     three different displays, each with a Retry where a fetch failed.
//   - Timestamps render in America/Denver; the raw UTC instant is in `title=`.
//   - Server error strings (400/403 are meaningful here) surface verbatim in a
//     toast — the brain refuses for a REASON, and the operator must read it.
//   - Filters are the shared FilterBar (dates hidden — the brain is not a
//     day-scoped surface) with the screen's own fields in its extraFields slot,
//     per PORTAL_DESIGN_SYSTEM §3: one bar, one chip style, Draft → Run.

import React from 'react'
import { apiFetch } from '../shared/apiFetch'
import { colors, alpha, pageStyle, tdStyle, thStyle, numTd, numTh, tableStyle, btnStyle } from '../shared/theme'
import { Panel, SectionHeader, Stat, SectionError, EmptyState, Pill } from '../shared/ui'
import { SubNav } from '../shared/SubNav'
import {
  FilterBar, denverToday, filterFieldLabelStyle, filterInputStyle,
  type LakeFilterDraft, type AppliedLakeFilters,
} from '../shared/filters'
import { useToast } from '../shared/ToastSystem'

// ═══════════════════════════════════════════════════════════════════════════
// API TYPES (mirror of internal/brain/types.go + store.go input structs)
// ═══════════════════════════════════════════════════════════════════════════

export const CLAIM_TYPES = ['policy', 'definition', 'system_fact', 'historical_finding', 'procedure', 'hypothesis'] as const
export const CLAIM_STATUSES = ['candidate', 'active', 'superseded', 'retracted'] as const
const EVIDENCE_VERDICTS = ['supports', 'refutes', 'inconclusive'] as const
const EVIDENCE_ENVS = ['operator', 'prod-pg-us-west-2', 'lake', 'repo', 'ses-us-west-1']

interface Claim {
  id: number
  org_id: string
  claim_type: string
  authority: string
  title: string
  body: string
  scope: string[] | null
  status: string
  effective_from?: string
  effective_until?: string
  supersedes?: number
  superseded_by?: number
  source: string
  source_ref?: string
  created_by: string
  created_at: string
  updated_at: string
  verified_at?: string
  verified_by?: string
  rank?: number
  evidence_count: number
}

interface Evidence {
  id: number
  claim_id: number
  checker: string
  result: string
  observed_at: string
  environment: string
  code_version: string
  verdict: string
  recorded_by: string
  created_at: string
}

interface ClaimDetail {
  claim: Claim
  evidence: Evidence[] | null
  required_verifier: string
}

interface Capability {
  id: number
  name: string
  version: number
  task: string
  tool: string
  inputs: string
  required_evidence: string
  completion_condition: string
  status: string
  created_by: string
  created_at: string
}

interface Eval {
  id: number
  name: string
  question: string
  checker: string
  spec: Record<string, unknown> | null
  expected: unknown
  tolerance_pct: number
  as_of?: string
  claim_id?: number
  capability_id?: number
  status: string
  last_run_at?: string
  last_pass?: boolean
  last_result?: unknown
  last_code_version?: string
  created_by: string
  created_at: string
}

interface EvalRun {
  id: number
  eval_id: number
  ran_at: string
  pass: boolean
  result: unknown
  error?: string
  code_version: string
  duration_ms: number
}

interface Stats {
  claims: Array<{ claim_type: string; status: string; n: number }> | null
  capabilities: Array<{ name: string; version: number; active: boolean }> | null
  evals: { active: number; passing: number; failing: number; never_run: number } | null
}

// ═══════════════════════════════════════════════════════════════════════════
// FETCH — every request through the shared apiFetch (org header + credentials)
// ═══════════════════════════════════════════════════════════════════════════

const BASE = '/api/mailing/brain'

class BrainError extends Error {
  status: number
  constructor(message: string, status: number) {
    super(message)
    this.name = 'BrainError'
    this.status = status
  }
}

async function parseError(res: Response): Promise<BrainError> {
  let message = `HTTP ${res.status}`
  try {
    const body: unknown = await res.json()
    if (body && typeof body === 'object') {
      const rec = body as Record<string, unknown>
      if (typeof rec.error === 'string' && rec.error.trim()) message = rec.error
    }
  } catch {
    /* non-JSON error body stays as the status line */
  }
  return new BrainError(message, res.status)
}

async function brainGet<T>(path: string, params: Record<string, string | undefined> = {}, signal?: AbortSignal): Promise<T> {
  const qs = new URLSearchParams()
  Object.entries(params).forEach(([k, v]) => { if (v != null && v !== '') qs.set(k, v) })
  const url = `${BASE}${path}${qs.toString() ? `?${qs.toString()}` : ''}`
  const res = await apiFetch(url, signal ? { signal } : {})
  if (!res.ok) throw await parseError(res)
  return (await res.json()) as T
}

async function brainPost<T>(path: string, body: unknown): Promise<T> {
  const res = await apiFetch(`${BASE}${path}`, { method: 'POST', body: JSON.stringify(body) })
  if (!res.ok) throw await parseError(res)
  return (await res.json()) as T
}

const errMsg = (e: unknown): string => (e instanceof Error ? e.message : String(e))

/** One fetch's lifecycle. `idle` = never asked; the four displays are distinct. */
type Loaded<T> =
  | { state: 'idle' }
  | { state: 'loading' }
  | { state: 'error'; error: string }
  | { state: 'ok'; data: T; fetchedAt: number; ms: number }

function useBrainGet<T>(path: string | null, params: Record<string, string | undefined>, nonce: number): [Loaded<T>, () => void] {
  const [st, setSt] = React.useState<Loaded<T>>({ state: 'idle' })
  const [retry, setRetry] = React.useState(0)
  const key = JSON.stringify(params)
  React.useEffect(() => {
    if (path == null) { setSt({ state: 'idle' }); return }
    const ctrl = new AbortController()
    const t0 = performance.now()
    setSt({ state: 'loading' })
    brainGet<T>(path, JSON.parse(key) as Record<string, string | undefined>, ctrl.signal)
      .then(data => setSt({ state: 'ok', data, fetchedAt: Date.now(), ms: Math.round(performance.now() - t0) }))
      .catch(e => { if (!ctrl.signal.aborted) setSt({ state: 'error', error: errMsg(e) }) })
    return () => ctrl.abort()
  }, [path, key, nonce, retry])
  return [st, () => setRetry(r => r + 1)]
}

// ═══════════════════════════════════════════════════════════════════════════
// DISPLAY PRIMITIVES
// ═══════════════════════════════════════════════════════════════════════════

const fmtDenver = (iso: string): string => {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString('en-US', { timeZone: 'America/Denver', hour12: false })
}

/** Denver-rendered timestamp; the UTC instant is the tooltip. Absent → muted "—" with a reason. */
const Ts: React.FC<{ iso: string | null | undefined; absent?: string }> = ({ iso, absent }) => {
  if (!iso) return <span style={{ color: colors.textFaint, fontStyle: 'italic' }} title={absent ?? 'no timestamp recorded'}>—</span>
  const d = new Date(iso)
  const utc = Number.isNaN(d.getTime()) ? iso : d.toISOString()
  return <span title={`${utc} (UTC)`} style={{ fontVariantNumeric: 'tabular-nums', whiteSpace: 'nowrap' }}>{fmtDenver(iso)} MT</span>
}

const fmtClock = (ms: number): string => new Date(ms).toLocaleTimeString('en-US', { timeZone: 'America/Denver', hour12: false })

const STATUS_BADGE: Record<string, { color: string; label: string; help: string }> = {
  candidate: { color: colors.warning, label: 'UNVERIFIED', help: 'candidate — recorded but not verified; cite it as such or verify it first' },
  active: { color: colors.success, label: 'ACTIVE', help: 'active — verified by the required verifier with stored evidence' },
  superseded: { color: colors.idle, label: 'SUPERSEDED', help: 'superseded — replaced by a newer claim (see superseded_by)' },
  retracted: { color: colors.danger, label: 'RETRACTED', help: 'retracted — withdrawn with a reason' },
}

const StatusBadge: React.FC<{ status: string }> = ({ status }) => {
  const b = STATUS_BADGE[status]
  if (!b) return <Pill color={colors.textFaint}>{status}</Pill>
  return <span title={b.help}><Pill color={b.color}>{b.label}</Pill></span>
}

const ScopeChips: React.FC<{ scope: string[] | null | undefined }> = ({ scope }) => {
  if (!scope || scope.length === 0) return <span style={{ color: colors.textFaint, fontStyle: 'italic' }}>no scope</span>
  return (
    <span style={{ display: 'inline-flex', gap: 4, flexWrap: 'wrap' }}>
      {scope.map(s => (
        <span key={s} style={{
          fontSize: 10, padding: '1px 6px', borderRadius: 4,
          color: colors.indigo300, background: alpha(colors.indigo500, '14'), border: `1px solid ${alpha(colors.indigo500, '33')}`,
        }}>{s}</span>
      ))}
    </span>
  )
}

const mono: React.CSSProperties = {
  fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace', fontSize: 11,
  whiteSpace: 'pre-wrap', wordBreak: 'break-word', margin: 0,
  background: colors.appBgSolid, border: `1px solid ${colors.hairline}`, borderRadius: 6, padding: '8px 10px',
  color: colors.text, maxHeight: 320, overflow: 'auto',
}

const PassPill: React.FC<{ pass: boolean | null | undefined }> = ({ pass }) => {
  if (pass == null) return <span title="never run — no run has been recorded for this eval"><Pill color={colors.idle}>never</Pill></span>
  return pass ? <Pill color={colors.success}>pass</Pill> : <Pill color={colors.danger}>fail</Pill>
}

const LoadingRow: React.FC<{ what: string }> = ({ what }) => (
  <div style={{ padding: '22px 10px', color: colors.textMuted, fontSize: 12 }}>Loading {what}…</div>
)

const FetchNote: React.FC<{ st: Loaded<unknown> }> = ({ st }) =>
  st.state === 'ok'
    ? <span style={{ fontSize: 11, color: colors.textFaint }}>fetched {fmtClock(st.fetchedAt)} MT · {st.ms}ms</span>
    : null

const ScrollX: React.FC<{ children: React.ReactNode }> = ({ children }) => (
  <div style={{ overflowX: 'auto' }}>{children}</div>
)

const dangerBtn: React.CSSProperties = {
  ...btnStyle, background: alpha(colors.danger, '22'), border: `1px solid ${alpha(colors.danger, '66')}`, color: colors.dangerText,
}
const smallBtn: React.CSSProperties = { ...btnStyle, padding: '4px 9px', fontSize: 11 }
const labelStyle: React.CSSProperties = { ...filterFieldLabelStyle, minWidth: 140 }
const inputStyle: React.CSSProperties = { ...filterInputStyle, width: '100%', boxSizing: 'border-box' }
const textareaStyle: React.CSSProperties = { ...inputStyle, minHeight: 72, fontFamily: 'inherit', resize: 'vertical' }
const monoInput: React.CSSProperties = { ...textareaStyle, fontFamily: mono.fontFamily }
const formGrid: React.CSSProperties = { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))', gap: 10, marginTop: 8 }

const Field: React.FC<{ label: string; children: React.ReactNode; span?: boolean; hint?: string }> = ({ label, children, span, hint }) => (
  <label style={{ ...labelStyle, ...(span ? { gridColumn: '1 / -1' } : {}) }} title={hint}>
    {label}
    {children}
  </label>
)

const asJSON = (v: unknown): string => {
  try { return JSON.stringify(v, null, 2) } catch { return String(v) }
}

// ═══════════════════════════════════════════════════════════════════════════
// STATS STRIP — on every pane (a memory number without its moment is a number
// about an unknown moment)
// ═══════════════════════════════════════════════════════════════════════════

const StatsStrip: React.FC<{ nonce: number }> = ({ nonce }) => {
  const [st, retry] = useBrainGet<Stats>('/stats', {}, nonce)
  if (st.state === 'loading' || st.state === 'idle') return <Panel style={{ marginBottom: 12 }}><LoadingRow what="brain stats" /></Panel>
  if (st.state === 'error') return <div style={{ marginBottom: 12 }}><SectionError label="Brain stats" error={st.error} onRetry={retry} /></div>
  const { data } = st
  const claims = data.claims ?? []
  const byStatus = (s: string) => claims.filter(c => c.status === s).reduce((a, c) => a + c.n, 0)
  const byType = new Map<string, Array<{ status: string; n: number }>>()
  claims.forEach(c => { byType.set(c.claim_type, [...(byType.get(c.claim_type) ?? []), { status: c.status, n: c.n }]) })
  const caps = data.capabilities ?? []
  const ev = data.evals
  return (
    <Panel style={{ marginBottom: 12 }}>
      <SectionHeader title="Brain" right={<FetchNote st={st} />} />
      <div style={{ display: 'flex', gap: 22, flexWrap: 'wrap' }}>
        <Stat label="Claims active" value={byStatus('active')} color={colors.success} title="claims with status=active (verified)" />
        <Stat label="Claims unverified" value={byStatus('candidate')} color={colors.warning} title="claims with status=candidate — recorded, not yet verified" />
        <Stat label="Superseded / retracted" value={`${byStatus('superseded')} / ${byStatus('retracted')}`} color={colors.textMuted} />
        <Stat label="Capabilities" value={caps.length === 0 ? 'none' : `${caps.filter(c => c.active).length} active`} sub={caps.length > 0 ? `${caps.length} named` : undefined} title="distinct capability names; active = has an active version" />
        {ev ? (
          <>
            <Stat label="Evals passing" value={ev.passing} color={colors.success} title="active evals whose last run passed" />
            <Stat label="Evals failing" value={ev.failing} color={colors.danger} title="active evals whose last run failed" />
            <Stat label="Evals never run" value={ev.never_run} color={colors.idle} sub={`${ev.active} active`} title="active evals with no recorded run" />
          </>
        ) : (
          <Stat label="Evals" value={<span style={{ color: colors.textFaint, fontStyle: 'italic', fontSize: 14 }}>unknown</span>} title="the stats response carried no evals block" />
        )}
      </div>
      {byType.size > 0 && (
        <div style={{ marginTop: 10, fontSize: 11, color: colors.textMuted, display: 'flex', gap: 14, flexWrap: 'wrap' }}>
          {[...byType.entries()].map(([t, rows]) => (
            <span key={t}><strong style={{ color: colors.indigo200 }}>{t}</strong>{' '}
              {rows.map(r => `${r.status} ${r.n}`).join(' · ')}
            </span>
          ))}
        </div>
      )}
    </Panel>
  )
}

// ═══════════════════════════════════════════════════════════════════════════
// CLAIMS PANE
// ═══════════════════════════════════════════════════════════════════════════

type ClaimMode = 'search' | 'recent'

interface ClaimFilter {
  mode: ClaimMode
  q: string
  claimType: string
  status: string // '' = server default (search: active,candidate; recent: all) | one status | 'active,candidate'
  scope: string
  days: string
}

const emptyClaimFilter = (): ClaimFilter => ({ mode: 'recent', q: '', claimType: '', status: '', scope: '', days: '7' })

// The FilterBar requires the lake draft shape; dates are hidden here and the
// brand/isp/route/transport fields are all off — the bar carries ONLY this
// screen's fields, but it is still the ONE bar (§3).
const lakeDraft = (): LakeFilterDraft => ({ from: denverToday(), to: denverToday(), ispGroup: '', brand: '', routeType: '', transport: 'combined' })

const ClaimsPane: React.FC<{ bump: () => void; nonce: number }> = ({ bump, nonce }) => {
  const { addToast } = useToast()
  const [draft, setDraft] = React.useState<ClaimFilter>(emptyClaimFilter)
  const [applied, setApplied] = React.useState<ClaimFilter & { nonce: number }>(() => ({ ...emptyClaimFilter(), nonce: 0 }))
  const [ld, setLd] = React.useState<LakeFilterDraft>(lakeDraft)
  const [la, setLa] = React.useState<AppliedLakeFilters>(() => ({ ...lakeDraft(), nonce: 0 }))
  const [selected, setSelected] = React.useState<number | null>(null)
  const [showNew, setShowNew] = React.useState(false)

  const run = React.useCallback((next: ClaimFilter) => {
    if (next.mode === 'search' && !next.q.trim()) {
      addToast({ type: 'warning', title: 'Search needs a query', message: 'Recall requires q — switch to Recent to browse without one.' })
      return
    }
    setApplied(a => ({ ...next, nonce: a.nonce + 1 }))
  }, [addToast])

  const isSearch = applied.mode === 'search'
  const recallStatus = applied.status // '' → server default active,candidate
  const recentStatus = applied.status.includes(',') ? '' : applied.status // recent takes ONE status
  const params: Record<string, string | undefined> = isSearch
    ? { q: applied.q.trim(), claim_type: applied.claimType, scope: applied.scope.trim(), status: recallStatus, limit: '50' }
    : { days: applied.days, status: recentStatus, limit: '200' }
  const [st, retry] = useBrainGet<{ claims: Claim[] | null; note?: string }>(isSearch ? '/recall' : '/recent', params, applied.nonce + nonce)

  // Recent has no claim_type / scope / multi-status params server-side: narrow
  // client-side and SAY SO, so the row count is never mistaken for the server's.
  let rows: Claim[] = st.state === 'ok' ? (st.data.claims ?? []) : []
  let clientNarrowed = false
  if (!isSearch && st.state === 'ok') {
    const statuses = applied.status ? applied.status.split(',') : null
    const scopes = applied.scope.trim() ? applied.scope.split(',').map(s => s.trim()).filter(Boolean) : null
    const before = rows.length
    rows = rows.filter(c =>
      (!applied.claimType || c.claim_type === applied.claimType) &&
      (!statuses || statuses.length === 1 || statuses.includes(c.status)) &&
      (!scopes || scopes.some(s => (c.scope ?? []).includes(s))))
    clientNarrowed = rows.length !== before
  }

  const chips: Array<{ label: string; tone?: string; onRemove?: () => void }> = [
    { label: isSearch ? `search "${applied.q.trim()}"` : `recent ${applied.days}d` },
  ]
  if (applied.claimType) chips.push({ label: `claim_type=${applied.claimType}`, tone: colors.indigo300, onRemove: () => { const n = { ...draft, claimType: '' }; setDraft(n); run(n) } })
  if (applied.status) chips.push({ label: `status=${applied.status}`, tone: colors.warning, onRemove: () => { const n = { ...draft, status: '' }; setDraft(n); run(n) } })
  else chips.push({ label: isSearch ? 'status=active,candidate (default)' : 'status=any' })
  if (applied.scope.trim()) chips.push({ label: `scope=${applied.scope.trim()}`, tone: colors.indigo200, onRemove: () => { const n = { ...draft, scope: '' }; setDraft(n); run(n) } })

  // One bump re-reads the list (nonce dep), the stats strip and the open detail.
  const reloadAll = bump

  return (
    <>
      <FilterBar
        draft={ld}
        setDraft={setLd}
        applied={la}
        onRun={next => { setLa(a => ({ ...next, nonce: a.nonce + 1 })); run(draft) }}
        show={{}}
        hideDates
        activeLabel="Active (claims):"
        extraChips={chips}
        extraFields={
          <>
            <label style={filterFieldLabelStyle}>mode
              <select value={draft.mode} onChange={e => setDraft(d => ({ ...d, mode: e.target.value as ClaimMode }))} style={{ ...filterInputStyle, width: 110 }}>
                <option value="recent">Recent</option>
                <option value="search">Search</option>
              </select>
            </label>
            {draft.mode === 'search' ? (
              <label style={filterFieldLabelStyle}>q
                <input type="text" value={draft.q} placeholder="websearch query" onChange={e => setDraft(d => ({ ...d, q: e.target.value }))}
                  onKeyDown={e => { if (e.key === 'Enter') run(draft) }} style={{ ...filterInputStyle, width: 240 }} />
              </label>
            ) : (
              <label style={filterFieldLabelStyle}>days
                <input type="number" min={1} max={365} value={draft.days} onChange={e => setDraft(d => ({ ...d, days: e.target.value }))} style={{ ...filterInputStyle, width: 70 }} />
              </label>
            )}
            <label style={filterFieldLabelStyle}>claim_type
              <select value={draft.claimType} onChange={e => setDraft(d => ({ ...d, claimType: e.target.value }))} style={{ ...filterInputStyle, width: 150 }}>
                <option value="">all</option>
                {CLAIM_TYPES.map(t => <option key={t} value={t}>{t}</option>)}
              </select>
            </label>
            <label style={filterFieldLabelStyle}>status
              <select value={draft.status} onChange={e => setDraft(d => ({ ...d, status: e.target.value }))} style={{ ...filterInputStyle, width: 170 }}>
                <option value="">{draft.mode === 'search' ? 'active + candidate (default)' : 'any'}</option>
                {draft.mode === 'search' && <option value="active,candidate,superseded,retracted">any</option>}
                {CLAIM_STATUSES.map(s => <option key={s} value={s}>{s}</option>)}
              </select>
            </label>
            <label style={filterFieldLabelStyle}>scope
              <input type="text" value={draft.scope} placeholder="a,b (any match)" onChange={e => setDraft(d => ({ ...d, scope: e.target.value }))} style={{ ...filterInputStyle, width: 160 }} />
            </label>
          </>
        }
      />

      <div style={{ display: 'grid', gridTemplateColumns: selected != null || showNew ? 'minmax(0, 1fr) minmax(0, 1fr)' : '1fr', gap: 14, alignItems: 'start' }}>
        <Panel>
          <SectionHeader
            title={isSearch ? 'Recall' : 'Recent claims'}
            right={
              <span style={{ display: 'flex', gap: 10, alignItems: 'center' }}>
                <FetchNote st={st} />
                <button type="button" style={btnStyle} onClick={() => { setShowNew(v => !v); setSelected(null) }}>{showNew ? 'Close form' : 'New claim'}</button>
              </span>
            }
          />
          {st.state === 'ok' && st.data.note && (
            <div style={{ fontSize: 11, color: colors.warningText, marginBottom: 8 }}>{st.data.note}</div>
          )}
          {!isSearch && clientNarrowed && st.state === 'ok' && (
            <div style={{ fontSize: 11, color: colors.warningText, marginBottom: 8 }}>
              Recent narrows claim_type / scope / multi-status client-side from the {(st.data.claims ?? []).length} most recent rows the server returned (cap 200) — older matches are not shown.
            </div>
          )}
          {(st.state === 'loading' || st.state === 'idle') && <LoadingRow what="claims" />}
          {st.state === 'error' && <SectionError label="Claims" error={st.error} onRetry={retry} />}
          {st.state === 'ok' && rows.length === 0 && (
            <EmptyState title="No claims match" hint={isSearch ? 'Recall found nothing for this query under the active filters (full-text, then trigram-title fallback).' : `No claims were created in the last ${applied.days} days under the active filters.`} />
          )}
          {st.state === 'ok' && rows.length > 0 && (
            <ScrollX>
              <table style={tableStyle}>
                <thead>
                  <tr>
                    <th style={numTh}>id</th>
                    <th style={thStyle}>status</th>
                    <th style={thStyle}>type</th>
                    <th style={thStyle}>title</th>
                    <th style={thStyle}>scope</th>
                    <th style={numTh} title="evidence rows stored for this claim">evid.</th>
                    <th style={thStyle}>verified</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map(c => {
                    const sel = c.id === selected
                    return (
                      <tr key={c.id} onClick={() => { setSelected(c.id); setShowNew(false) }}
                        style={{ cursor: 'pointer', background: sel ? colors.hover : undefined }}>
                        <td style={numTd}>{c.id}</td>
                        <td style={tdStyle}><StatusBadge status={c.status} /></td>
                        <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>{c.claim_type}</td>
                        <td style={{ ...tdStyle, maxWidth: 360, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={c.title}>{c.title}</td>
                        <td style={tdStyle}><ScopeChips scope={c.scope} /></td>
                        <td style={numTd}>{c.evidence_count}</td>
                        <td style={{ ...tdStyle, fontSize: 11 }}>
                          {c.verified_by ? <>{c.verified_by} · <Ts iso={c.verified_at} /></> : <span style={{ color: colors.textFaint, fontStyle: 'italic' }}>not verified</span>}
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </ScrollX>
          )}
        </Panel>

        {showNew && <NewClaimForm onCreated={c => { setShowNew(false); setSelected(c.id); reloadAll() }} onCancel={() => setShowNew(false)} />}
        {selected != null && !showNew && (
          <ClaimDetailPanel id={selected} nonce={nonce} onClose={() => setSelected(null)} onChanged={reloadAll} onOpen={setSelected} />
        )}
      </div>
    </>
  )
}

const NewClaimForm: React.FC<{ onCreated: (c: Claim) => void; onCancel: () => void }> = ({ onCreated, onCancel }) => {
  const { addToast } = useToast()
  const [f, setF] = React.useState({ claim_type: 'system_fact', title: '', body: '', scope: '' })
  const [busy, setBusy] = React.useState(false)
  const submit = async () => {
    setBusy(true)
    try {
      const c = await brainPost<Claim>('/claims/', {
        claim_type: f.claim_type, title: f.title, body: f.body,
        scope: f.scope.split(',').map(s => s.trim()).filter(Boolean), source: 'portal', created_by: 'operator',
      })
      addToast({ type: 'success', title: `Claim #${c.id} recorded as ${c.status}`, message: STATUS_BADGE[c.status]?.help })
      onCreated(c)
    } catch (e) {
      addToast({ type: 'error', title: 'Claim not recorded', message: errMsg(e), duration: 9000 })
    } finally { setBusy(false) }
  }
  return (
    <Panel accent={colors.indigo500}>
      <SectionHeader title="New claim" right={<button type="button" style={smallBtn} onClick={onCancel}>Cancel</button>} />
      <div style={{ fontSize: 11, color: colors.textMuted }}>
        Recorded as <strong>candidate</strong> (UNVERIFIED) with source=<code>portal</code>. Activation needs the required verifier for its type.
      </div>
      <div style={formGrid}>
        <Field label="claim_type">
          <select value={f.claim_type} onChange={e => setF(x => ({ ...x, claim_type: e.target.value }))} style={inputStyle}>
            {CLAIM_TYPES.map(t => <option key={t} value={t}>{t}</option>)}
          </select>
        </Field>
        <Field label="scope (comma list)" hint="e.g. isp:gmail,lane:yahoo_family">
          <input type="text" value={f.scope} onChange={e => setF(x => ({ ...x, scope: e.target.value }))} style={inputStyle} />
        </Field>
        <Field label="title" span>
          <input type="text" value={f.title} onChange={e => setF(x => ({ ...x, title: e.target.value }))} style={inputStyle} />
        </Field>
        <Field label="body" span>
          <textarea value={f.body} onChange={e => setF(x => ({ ...x, body: e.target.value }))} style={textareaStyle} />
        </Field>
      </div>
      <div style={{ marginTop: 10 }}>
        <button type="button" style={btnStyle} disabled={busy || !f.title.trim()} onClick={submit}>{busy ? 'Recording…' : 'Record claim'}</button>
      </div>
    </Panel>
  )
}

type ClaimAction = 'verify' | 'evidence' | 'retract' | 'supersede' | null

const ClaimDetailPanel: React.FC<{
  id: number; nonce: number; onClose: () => void; onChanged: () => void; onOpen: (id: number) => void
}> = ({ id, nonce, onClose, onChanged, onOpen }) => {
  const { addToast } = useToast()
  const [st, retry] = useBrainGet<ClaimDetail>(`/claims/${id}`, {}, nonce)
  const [action, setAction] = React.useState<ClaimAction>(null)
  const [busy, setBusy] = React.useState(false)
  const [ev, setEv] = React.useState({ checker: '', result: '', environment: 'operator', verdict: 'supports', code_version: '' })
  const [verifierName, setVerifierName] = React.useState('operator')
  const [reason, setReason] = React.useState('')
  const [newID, setNewID] = React.useState('')

  React.useEffect(() => { setAction(null); setReason(''); setNewID('') }, [id])

  const post = async (path: string, body: unknown, okTitle: string) => {
    setBusy(true)
    try {
      await brainPost<unknown>(path, body)
      addToast({ type: 'success', title: okTitle })
      setAction(null); setReason(''); setNewID('')
      setEv(e => ({ ...e, checker: '', result: '' }))
      onChanged()
    } catch (e) {
      // 400/403 strings are the brain's own refusal reasons — verbatim.
      addToast({ type: 'error', title: `${okTitle} refused`, message: errMsg(e), duration: 10000 })
    } finally { setBusy(false) }
  }

  const evidenceBody = (env: string) => ({
    checker: ev.checker, result: ev.result, environment: env, verdict: ev.verdict,
    ...(ev.code_version.trim() ? { code_version: ev.code_version.trim() } : {}),
  })

  if (st.state === 'loading' || st.state === 'idle') return <Panel><LoadingRow what={`claim #${id}`} /></Panel>
  if (st.state === 'error') return <Panel><SectionError label={`Claim #${id}`} error={st.error} onRetry={retry} /><div style={{ marginTop: 8 }}><button type="button" style={smallBtn} onClick={onClose}>Close</button></div></Panel>
  const { claim: c, required_verifier } = st.data
  const evidence = st.data.evidence ?? []
  const terminal = c.status === 'superseded' || c.status === 'retracted'
  const evidenceFields = (
    <>
      <Field label="checker (query / command / file:line)" span>
        <textarea value={ev.checker} onChange={e => setEv(x => ({ ...x, checker: e.target.value }))} style={monoInput} />
      </Field>
      <Field label="result (verbatim; credentials are redacted server-side)" span>
        <textarea value={ev.result} onChange={e => setEv(x => ({ ...x, result: e.target.value }))} style={monoInput} />
      </Field>
    </>
  )

  return (
    <Panel accent={STATUS_BADGE[c.status]?.color ?? colors.indigo500}>
      <SectionHeader
        title={`Claim #${c.id}`}
        right={<span style={{ display: 'flex', gap: 8, alignItems: 'center' }}><FetchNote st={st} /><button type="button" style={smallBtn} onClick={onClose}>Close</button></span>}
      />
      <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap', marginBottom: 6 }}>
        <StatusBadge status={c.status} />
        <Pill color={colors.indigo400}>{c.claim_type}</Pill>
        <span style={{ fontSize: 11, color: colors.textMuted }} title="who may move this claim to active (brain.RequiredVerifier)">
          required verifier: <strong style={{ color: colors.indigo200 }}>{required_verifier}</strong> · authority {c.authority}
        </span>
      </div>
      <div style={{ fontSize: 14, fontWeight: 600, color: colors.heading, marginBottom: 6 }}>{c.title}</div>
      <pre style={{ ...mono, fontFamily: 'inherit', fontSize: 12, maxHeight: 260 }}>{c.body || <span style={{ color: colors.textFaint, fontStyle: 'italic' }}>(empty body)</span>}</pre>
      <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 8, display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: '4px 14px' }}>
        <span>scope <ScopeChips scope={c.scope} /></span>
        <span>source {c.source}{c.source_ref ? ` · ${c.source_ref}` : ''}</span>
        <span>created {c.created_by} · <Ts iso={c.created_at} /></span>
        <span>updated <Ts iso={c.updated_at} /></span>
        <span>verified {c.verified_by ? <>{c.verified_by} · <Ts iso={c.verified_at} /></> : <em style={{ color: colors.textFaint }}>not verified</em>}</span>
        <span>effective {c.effective_from ?? <em style={{ color: colors.textFaint }}>open</em>} → {c.effective_until ?? <em style={{ color: colors.textFaint }}>open</em>}</span>
        {c.supersedes != null && <span>supersedes <button type="button" style={{ ...smallBtn, padding: '1px 6px' }} onClick={() => onOpen(c.supersedes as number)}>#{c.supersedes}</button></span>}
        {c.superseded_by != null && <span>superseded by <button type="button" style={{ ...smallBtn, padding: '1px 6px' }} onClick={() => onOpen(c.superseded_by as number)}>#{c.superseded_by}</button></span>}
      </div>

      <SectionHeader title={`Evidence (${evidence.length})`} style={{ marginTop: 14 }} />
      {evidence.length === 0 ? (
        <EmptyState title="No evidence rows" hint="A claim without a stored checker + result is not verified. Add evidence or verify with a result." />
      ) : (
        <ScrollX>
          <table style={tableStyle}>
            <thead>
              <tr>
                <th style={thStyle}>verdict</th>
                <th style={thStyle}>checker</th>
                <th style={thStyle}>result</th>
                <th style={thStyle}>observed</th>
                <th style={thStyle}>env</th>
                <th style={thStyle}>code</th>
                <th style={thStyle}>by</th>
              </tr>
            </thead>
            <tbody>
              {evidence.map(e => (
                <tr key={e.id}>
                  <td style={tdStyle}>
                    <Pill color={e.verdict === 'supports' ? colors.success : e.verdict === 'refutes' ? colors.danger : colors.idle}>{e.verdict}</Pill>
                  </td>
                  <td style={{ ...tdStyle, minWidth: 180 }}><pre style={{ ...mono, maxHeight: 140 }}>{e.checker}</pre></td>
                  <td style={{ ...tdStyle, minWidth: 220 }}><pre style={{ ...mono, maxHeight: 140 }}>{e.result}</pre></td>
                  <td style={{ ...tdStyle, fontSize: 11 }}><Ts iso={e.observed_at} /></td>
                  <td style={{ ...tdStyle, fontSize: 11 }}>{e.environment || <em style={{ color: colors.textFaint }}>—</em>}</td>
                  <td style={{ ...tdStyle, fontSize: 11, fontFamily: mono.fontFamily }} title={e.code_version}>{e.code_version ? e.code_version.slice(0, 10) : <em style={{ color: colors.textFaint }}>—</em>}</td>
                  <td style={{ ...tdStyle, fontSize: 11 }}>{e.recorded_by}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </ScrollX>
      )}

      <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginTop: 14 }}>
        <button type="button" style={btnStyle} disabled={terminal || c.status === 'active'} onClick={() => setAction(a => a === 'verify' ? null : 'verify')}
          title={required_verifier === 'operator' ? 'policy/definition claims activate only on an operator verification' : `this type is verified by ${required_verifier}; the server decides whether operator counts`}>
          Verify as operator
        </button>
        <button type="button" style={btnStyle} disabled={terminal} onClick={() => setAction(a => a === 'evidence' ? null : 'evidence')}>Add evidence</button>
        <button type="button" style={dangerBtn} disabled={terminal} onClick={() => setAction(a => a === 'retract' ? null : 'retract')}>Retract</button>
        <button type="button" style={btnStyle} disabled={terminal} onClick={() => setAction(a => a === 'supersede' ? null : 'supersede')}>Supersede</button>
        {terminal && <span style={{ fontSize: 11, color: colors.textFaint, alignSelf: 'center' }}>{c.status} claims are frozen.</span>}
      </div>

      {action === 'verify' && (
        <div style={{ marginTop: 10 }}>
          <div style={{ fontSize: 11, color: colors.textMuted }}>
            POST /claims/{c.id}/verify as <code>verifier_role=operator</code>. Required verifier for <strong>{c.claim_type}</strong> is <strong>{required_verifier}</strong>
            {required_verifier !== 'operator' && ' — the server may still accept an operator verification, or refuse with its reason.'}
            {' '}Evidence environment is fixed to <code>operator</code>.
          </div>
          <div style={formGrid}>
            <Field label="verifier_name"><input type="text" value={verifierName} onChange={e => setVerifierName(e.target.value)} style={inputStyle} /></Field>
            <Field label="verdict">
              <select value={ev.verdict} onChange={e => setEv(x => ({ ...x, verdict: e.target.value }))} style={inputStyle}>
                {EVIDENCE_VERDICTS.map(v => <option key={v} value={v}>{v}</option>)}
              </select>
            </Field>
            {evidenceFields}
          </div>
          <button type="button" style={{ ...btnStyle, marginTop: 8 }} disabled={busy || !ev.checker.trim() || !ev.result.trim()}
            onClick={() => post(`/claims/${c.id}/verify`, { verifier_role: 'operator', verifier_name: verifierName || 'operator', evidence: evidenceBody('operator') }, `Verify #${c.id}`)}>
            {busy ? 'Verifying…' : 'Verify'}
          </button>
        </div>
      )}
      {action === 'evidence' && (
        <div style={{ marginTop: 10 }}>
          <div style={{ fontSize: 11, color: colors.textMuted }}>POST /claims/{c.id}/evidence — does not change status by itself.</div>
          <div style={formGrid}>
            <Field label="environment">
              <input type="text" list="brain-envs" value={ev.environment} onChange={e => setEv(x => ({ ...x, environment: e.target.value }))} style={inputStyle} />
              <datalist id="brain-envs">{EVIDENCE_ENVS.map(v => <option key={v} value={v} />)}</datalist>
            </Field>
            <Field label="verdict">
              <select value={ev.verdict} onChange={e => setEv(x => ({ ...x, verdict: e.target.value }))} style={inputStyle}>
                {EVIDENCE_VERDICTS.map(v => <option key={v} value={v}>{v}</option>)}
              </select>
            </Field>
            <Field label="code_version (git sha, optional)"><input type="text" value={ev.code_version} onChange={e => setEv(x => ({ ...x, code_version: e.target.value }))} style={inputStyle} /></Field>
            {evidenceFields}
          </div>
          <button type="button" style={{ ...btnStyle, marginTop: 8 }} disabled={busy || !ev.checker.trim() || !ev.result.trim()}
            onClick={() => post(`/claims/${c.id}/evidence`, { ...evidenceBody(ev.environment), recorded_by: 'operator' }, `Evidence on #${c.id}`)}>
            {busy ? 'Adding…' : 'Add evidence'}
          </button>
        </div>
      )}
      {action === 'retract' && (
        <div style={{ marginTop: 10 }}>
          <div style={formGrid}>
            <Field label="reason (required)" span><textarea value={reason} onChange={e => setReason(e.target.value)} style={textareaStyle} /></Field>
          </div>
          <button type="button" style={{ ...dangerBtn, marginTop: 8 }} disabled={busy || !reason.trim()}
            onClick={() => post(`/claims/${c.id}/retract`, { reason: reason.trim(), by: 'operator' }, `Retract #${c.id}`)}>
            {busy ? 'Retracting…' : 'Retract claim'}
          </button>
        </div>
      )}
      {action === 'supersede' && (
        <div style={{ marginTop: 10 }}>
          <div style={{ fontSize: 11, color: colors.textMuted }}>Marks #{c.id} superseded by an EXISTING claim id (record the replacement first with New claim).</div>
          <div style={formGrid}>
            <Field label="new_id"><input type="number" min={1} value={newID} onChange={e => setNewID(e.target.value)} style={inputStyle} /></Field>
            <Field label="reason" span><textarea value={reason} onChange={e => setReason(e.target.value)} style={textareaStyle} /></Field>
          </div>
          <button type="button" style={{ ...btnStyle, marginTop: 8 }} disabled={busy || !newID.trim()}
            onClick={() => post(`/claims/${c.id}/supersede`, { new_id: Number(newID), reason: reason.trim(), by: 'operator' }, `Supersede #${c.id}`)}>
            {busy ? 'Superseding…' : `Supersede with #${newID || '?'}`}
          </button>
        </div>
      )}
    </Panel>
  )
}

// ═══════════════════════════════════════════════════════════════════════════
// CAPABILITIES PANE
// ═══════════════════════════════════════════════════════════════════════════

const CapabilitiesPane: React.FC<{ bump: () => void; nonce: number }> = ({ bump, nonce }) => {
  const { addToast } = useToast()
  const [st, retry] = useBrainGet<{ capabilities: Capability[] | null }>('/capabilities/', { include_retired: '1' }, nonce)
  const [showForm, setShowForm] = React.useState(false)
  const [busy, setBusy] = React.useState(false)
  const [f, setF] = React.useState({ name: '', task: '', tool: '', inputs: '', required_evidence: '', completion_condition: '' })
  const rows = st.state === 'ok' ? (st.data.capabilities ?? []) : []

  const submit = async () => {
    setBusy(true)
    try {
      const c = await brainPost<Capability>('/capabilities/', { ...f, created_by: 'operator' })
      addToast({ type: 'success', title: `${c.name} v${c.version} registered`, message: 'The previous active version (if any) is now retired.' })
      setShowForm(false); setF({ name: '', task: '', tool: '', inputs: '', required_evidence: '', completion_condition: '' })
      bump()
    } catch (e) {
      addToast({ type: 'error', title: 'Capability not registered', message: errMsg(e), duration: 9000 })
    } finally { setBusy(false) }
  }

  return (
    <>
      {showForm && (
        <Panel accent={colors.indigo500} style={{ marginBottom: 14 }}>
          <SectionHeader title="Register new version" right={<button type="button" style={smallBtn} onClick={() => setShowForm(false)}>Cancel</button>} />
          <div style={{ fontSize: 11, color: colors.textMuted }}>Versions are never edited in place: this creates version N+1 of <code>name</code> and retires the active one. name must match <code>^[a-z][a-z0-9_]{'{1,63}'}$</code>.</div>
          <div style={formGrid}>
            <Field label="name"><input type="text" value={f.name} onChange={e => setF(x => ({ ...x, name: e.target.value }))} style={{ ...inputStyle, fontFamily: mono.fontFamily }} /></Field>
            <Field label="tool (endpoint / module / command)"><input type="text" value={f.tool} onChange={e => setF(x => ({ ...x, tool: e.target.value }))} style={{ ...inputStyle, fontFamily: mono.fontFamily }} /></Field>
            <Field label="task (the recognised question it answers)" span><textarea value={f.task} onChange={e => setF(x => ({ ...x, task: e.target.value }))} style={textareaStyle} /></Field>
            <Field label="inputs"><textarea value={f.inputs} onChange={e => setF(x => ({ ...x, inputs: e.target.value }))} style={textareaStyle} /></Field>
            <Field label="required_evidence"><textarea value={f.required_evidence} onChange={e => setF(x => ({ ...x, required_evidence: e.target.value }))} style={textareaStyle} /></Field>
            <Field label="completion_condition" span><textarea value={f.completion_condition} onChange={e => setF(x => ({ ...x, completion_condition: e.target.value }))} style={textareaStyle} /></Field>
          </div>
          <button type="button" style={{ ...btnStyle, marginTop: 8 }} disabled={busy || !f.name.trim() || !f.task.trim() || !f.tool.trim()} onClick={submit}>{busy ? 'Registering…' : 'Register version'}</button>
        </Panel>
      )}
      <Panel>
        <SectionHeader
          title="Capabilities"
          right={<span style={{ display: 'flex', gap: 10, alignItems: 'center' }}><FetchNote st={st} /><button type="button" style={btnStyle} onClick={() => setShowForm(v => !v)}>Register new version</button></span>}
        />
        {(st.state === 'loading' || st.state === 'idle') && <LoadingRow what="capabilities" />}
        {st.state === 'error' && <SectionError label="Capabilities" error={st.error} onRetry={retry} />}
        {st.state === 'ok' && rows.length === 0 && <EmptyState title="No capabilities registered" hint="Including retired versions. Register one above." />}
        {st.state === 'ok' && rows.length > 0 && (
          <ScrollX>
            <table style={tableStyle}>
              <thead>
                <tr>
                  <th style={thStyle}>name</th>
                  <th style={numTh}>ver</th>
                  <th style={thStyle}>status</th>
                  <th style={thStyle}>task</th>
                  <th style={thStyle}>tool</th>
                  <th style={thStyle}>inputs</th>
                  <th style={thStyle}>required_evidence</th>
                  <th style={thStyle}>completion_condition</th>
                  <th style={thStyle}>created</th>
                </tr>
              </thead>
              <tbody>
                {rows.map(c => (
                  <tr key={c.id} style={{ opacity: c.status === 'active' ? 1 : 0.6 }}>
                    <td style={{ ...tdStyle, fontFamily: mono.fontFamily, whiteSpace: 'nowrap' }}>{c.name}</td>
                    <td style={numTd}>{c.version}</td>
                    <td style={tdStyle}><Pill color={c.status === 'active' ? colors.success : colors.idle}>{c.status}</Pill></td>
                    <td style={{ ...tdStyle, minWidth: 220 }}>{c.task}</td>
                    <td style={{ ...tdStyle, fontFamily: mono.fontFamily, fontSize: 11, minWidth: 180, wordBreak: 'break-all' }}>{c.tool}</td>
                    <td style={{ ...tdStyle, minWidth: 160, fontSize: 11 }}>{c.inputs || <em style={{ color: colors.textFaint }}>—</em>}</td>
                    <td style={{ ...tdStyle, minWidth: 160, fontSize: 11 }}>{c.required_evidence || <em style={{ color: colors.textFaint }}>—</em>}</td>
                    <td style={{ ...tdStyle, minWidth: 160, fontSize: 11 }}>{c.completion_condition || <em style={{ color: colors.textFaint }}>—</em>}</td>
                    <td style={{ ...tdStyle, fontSize: 11 }}>{c.created_by} · <Ts iso={c.created_at} /></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </ScrollX>
        )}
      </Panel>
    </>
  )
}

// ═══════════════════════════════════════════════════════════════════════════
// EVALS PANE
// ═══════════════════════════════════════════════════════════════════════════

const specText = (spec: Record<string, unknown> | null): string => {
  if (!spec) return ''
  const q = spec.query
  if (typeof q === 'string') return q
  const p = spec.path
  const m = spec.method
  const u = spec.url
  const parts = [typeof m === 'string' ? m : null, typeof u === 'string' ? u : null, typeof p === 'string' ? p : null].filter(Boolean)
  return parts.length ? parts.join(' ') : asJSON(spec)
}

const runDiffs = (result: unknown): string[] => {
  if (!result || typeof result !== 'object') return []
  const d = (result as Record<string, unknown>).diffs
  return Array.isArray(d) ? d.filter((x): x is string => typeof x === 'string') : []
}
const runError = (result: unknown): string | null => {
  if (!result || typeof result !== 'object') return null
  const e = (result as Record<string, unknown>).error
  return typeof e === 'string' ? e : null
}

const EvalsPane: React.FC<{ bump: () => void; nonce: number }> = ({ bump, nonce }) => {
  const { addToast } = useToast()
  const [st, retry] = useBrainGet<{ evals: Eval[] | null }>('/evals/', { all: '1' }, nonce)
  const [open, setOpen] = React.useState<number | null>(null)
  const [runs, setRuns] = React.useState<Record<number, Loaded<{ runs: EvalRun[] | null }>>>({})
  const [lastRun, setLastRun] = React.useState<Record<number, EvalRun>>({})
  const [busy, setBusy] = React.useState<number | null>(null)
  const rows = st.state === 'ok' ? (st.data.evals ?? []) : []

  const loadRuns = async (id: number) => {
    setRuns(r => ({ ...r, [id]: { state: 'loading' } }))
    const t0 = performance.now()
    try {
      const data = await brainGet<{ runs: EvalRun[] | null }>(`/evals/${id}/runs`, { limit: '50' })
      setRuns(r => ({ ...r, [id]: { state: 'ok', data, fetchedAt: Date.now(), ms: Math.round(performance.now() - t0) } }))
    } catch (e) {
      setRuns(r => ({ ...r, [id]: { state: 'error', error: errMsg(e) } }))
    }
  }

  const runNow = async (ev: Eval) => {
    setBusy(ev.id)
    try {
      const r = await brainPost<EvalRun>(`/evals/${ev.id}/run`, {})
      setLastRun(m => ({ ...m, [ev.id]: r }))
      setOpen(ev.id)
      addToast({ type: r.pass ? 'success' : 'warning', title: `${ev.name}: ${r.pass ? 'PASS' : 'FAIL'}`, message: r.error || (runDiffs(r.result).length ? `${runDiffs(r.result).length} diff(s)` : `${r.duration_ms}ms`), duration: 8000 })
      bump()
      if (runs[ev.id]) void loadRuns(ev.id)
    } catch (e) {
      addToast({ type: 'error', title: `Run ${ev.name} refused`, message: errMsg(e), duration: 10000 })
    } finally { setBusy(null) }
  }

  const retire = async (ev: Eval) => {
    if (!window.confirm(`Retire eval "${ev.name}" (#${ev.id})? The worker stops re-running it.`)) return
    setBusy(ev.id)
    try {
      await brainPost<unknown>(`/evals/${ev.id}/retire`, {})
      addToast({ type: 'success', title: `${ev.name} retired` })
      bump()
    } catch (e) {
      addToast({ type: 'error', title: `Retire ${ev.name} refused`, message: errMsg(e), duration: 10000 })
    } finally { setBusy(null) }
  }

  return (
    <Panel>
      <SectionHeader title="Evals" right={<FetchNote st={st} />} />
      <div style={{ fontSize: 11, color: colors.textMuted, marginBottom: 8 }}>
        Pinned answers a fresh session must still reproduce. <code>as_of</code> is the date the expected values were pinned; <code>last_pass</code> is the worker's verdict, shown, not recomputed.
      </div>
      {(st.state === 'loading' || st.state === 'idle') && <LoadingRow what="evals" />}
      {st.state === 'error' && <SectionError label="Evals" error={st.error} onRetry={retry} />}
      {st.state === 'ok' && rows.length === 0 && <EmptyState title="No evals" hint="Including retired ones. Evals are registered by the console client (POST /brain/evals/)." />}
      {st.state === 'ok' && rows.length > 0 && (
        <ScrollX>
          <table style={tableStyle}>
            <thead>
              <tr>
                <th style={thStyle}></th>
                <th style={thStyle}>name</th>
                <th style={thStyle}>checker</th>
                <th style={thStyle}>last_pass</th>
                <th style={thStyle}>last_run_at</th>
                <th style={thStyle}>code</th>
                <th style={thStyle}>as_of</th>
                <th style={numTh}>tol %</th>
                <th style={thStyle}>question</th>
                <th style={thStyle}></th>
              </tr>
            </thead>
            <tbody>
              {rows.map(ev => {
                const isOpen = open === ev.id
                const lr = lastRun[ev.id]
                const rs = runs[ev.id]
                const retired = ev.status !== 'active'
                return (
                  <React.Fragment key={ev.id}>
                    <tr style={{ opacity: retired ? 0.6 : 1 }}>
                      <td style={{ ...tdStyle, cursor: 'pointer', width: 18 }} onClick={() => setOpen(isOpen ? null : ev.id)}>{isOpen ? '▾' : '▸'}</td>
                      <td style={{ ...tdStyle, fontFamily: mono.fontFamily, whiteSpace: 'nowrap', cursor: 'pointer' }} onClick={() => setOpen(isOpen ? null : ev.id)}>
                        {ev.name}{retired && <Pill color={colors.idle} style={{ marginLeft: 6 }}>{ev.status}</Pill>}
                      </td>
                      <td style={{ ...tdStyle, fontSize: 11 }}>{ev.checker}</td>
                      <td style={tdStyle}><PassPill pass={ev.last_pass} /></td>
                      <td style={{ ...tdStyle, fontSize: 11 }}><Ts iso={ev.last_run_at} absent="never run" /></td>
                      <td style={{ ...tdStyle, fontSize: 11, fontFamily: mono.fontFamily }} title={ev.last_code_version}>{ev.last_code_version ? ev.last_code_version.slice(0, 10) : <em style={{ color: colors.textFaint }}>—</em>}</td>
                      <td style={{ ...tdStyle, fontSize: 11 }}>{ev.as_of ?? <em style={{ color: colors.textFaint }} title="no as_of pinned on this eval">—</em>}</td>
                      <td style={numTd}>{ev.tolerance_pct}</td>
                      <td style={{ ...tdStyle, minWidth: 220 }}>{ev.question}</td>
                      <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>
                        <span style={{ display: 'inline-flex', gap: 6 }}>
                          <button type="button" style={smallBtn} disabled={busy === ev.id} onClick={() => runNow(ev)}>{busy === ev.id ? 'Running…' : 'Run now'}</button>
                          <button type="button" style={smallBtn} onClick={() => { setOpen(ev.id); void loadRuns(ev.id) }}>Runs</button>
                          <button type="button" style={{ ...dangerBtn, padding: '4px 9px', fontSize: 11 }} disabled={retired || busy === ev.id} onClick={() => retire(ev)}>Retire</button>
                        </span>
                      </td>
                    </tr>
                    {isOpen && (
                      <tr>
                        <td colSpan={10} style={{ ...tdStyle, background: alpha(colors.indigo500, '0d') }}>
                          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(320px, 1fr))', gap: 12 }}>
                            <div>
                              <div style={{ fontSize: 10, color: colors.textFaint, textTransform: 'uppercase', letterSpacing: 0.5, marginBottom: 4 }}>spec ({ev.checker})</div>
                              <pre style={mono}>{specText(ev.spec) || <em style={{ color: colors.textFaint }}>no spec</em>}</pre>
                            </div>
                            <div>
                              <div style={{ fontSize: 10, color: colors.textFaint, textTransform: 'uppercase', letterSpacing: 0.5, marginBottom: 4 }}>expected</div>
                              <pre style={mono}>{ev.expected == null ? <em style={{ color: colors.textFaint }}>none pinned</em> : asJSON(ev.expected)}</pre>
                            </div>
                            {lr && (
                              <div style={{ gridColumn: '1 / -1' }}>
                                <div style={{ fontSize: 10, color: colors.textFaint, textTransform: 'uppercase', letterSpacing: 0.5, marginBottom: 4 }}>
                                  run just now · <PassPill pass={lr.pass} /> · {lr.duration_ms}ms · code {lr.code_version ? lr.code_version.slice(0, 10) : '—'}
                                </div>
                                {lr.error && <div style={{ fontSize: 11, color: colors.dangerText, marginBottom: 4 }}>{lr.error}</div>}
                                {runDiffs(lr.result).length > 0 ? (
                                  <ul style={{ margin: 0, paddingLeft: 18, fontSize: 11, color: colors.warningText }}>{runDiffs(lr.result).map((d, i) => <li key={i}>{d}</li>)}</ul>
                                ) : !lr.error && <div style={{ fontSize: 11, color: colors.successText }}>no diffs</div>}
                                <pre style={{ ...mono, marginTop: 6, maxHeight: 200 }}>{asJSON(lr.result)}</pre>
                              </div>
                            )}
                            {rs && (
                              <div style={{ gridColumn: '1 / -1' }}>
                                <div style={{ fontSize: 10, color: colors.textFaint, textTransform: 'uppercase', letterSpacing: 0.5, marginBottom: 4 }}>run history <FetchNote st={rs} /></div>
                                {rs.state === 'loading' && <LoadingRow what="runs" />}
                                {rs.state === 'error' && <SectionError label="Runs" error={rs.error} onRetry={() => loadRuns(ev.id)} />}
                                {rs.state === 'ok' && (rs.data.runs ?? []).length === 0 && <EmptyState title="No runs recorded" hint="Run now records the first one." />}
                                {rs.state === 'ok' && (rs.data.runs ?? []).length > 0 && (
                                  <table style={tableStyle}>
                                    <thead><tr><th style={thStyle}>pass</th><th style={thStyle}>ran_at</th><th style={numTh}>ms</th><th style={thStyle}>code</th><th style={thStyle}>diffs / error</th></tr></thead>
                                    <tbody>
                                      {(rs.data.runs ?? []).map(r => (
                                        <tr key={r.id}>
                                          <td style={tdStyle}><PassPill pass={r.pass} /></td>
                                          <td style={{ ...tdStyle, fontSize: 11 }}><Ts iso={r.ran_at} /></td>
                                          <td style={numTd}>{r.duration_ms}</td>
                                          <td style={{ ...tdStyle, fontSize: 11, fontFamily: mono.fontFamily }} title={r.code_version}>{r.code_version ? r.code_version.slice(0, 10) : '—'}</td>
                                          <td style={{ ...tdStyle, fontSize: 11 }}>
                                            {r.error || runError(r.result) ? <span style={{ color: colors.dangerText }}>{r.error || runError(r.result)}</span>
                                              : runDiffs(r.result).length ? runDiffs(r.result).join(' · ') : <span style={{ color: colors.textFaint }}>none</span>}
                                          </td>
                                        </tr>
                                      ))}
                                    </tbody>
                                  </table>
                                )}
                              </div>
                            )}
                          </div>
                        </td>
                      </tr>
                    )}
                  </React.Fragment>
                )
              })}
            </tbody>
          </table>
        </ScrollX>
      )}
    </Panel>
  )
}

// ═══════════════════════════════════════════════════════════════════════════
// TAB
// ═══════════════════════════════════════════════════════════════════════════

type Pane = 'claims' | 'capabilities' | 'evals'
const PANES = [
  { key: 'claims', label: 'Claims' },
  { key: 'capabilities', label: 'Capabilities' },
  { key: 'evals', label: 'Evals' },
]

export const Brain: React.FC = () => {
  const [pane, setPane] = React.useState<Pane>('claims')
  // Bumped after any write so the stats strip (and the other panes, once
  // visited) re-read — a verify changes claim counts, a run changes eval counts.
  const [nonce, setNonce] = React.useState(0)
  const bump = React.useCallback(() => setNonce(n => n + 1), [])

  return (
    <div style={pageStyle}>
      <div style={{ marginBottom: 14 }}>
        <h1 style={{ margin: 0, fontSize: 22, color: colors.heading }}>Brain</h1>
        <div style={{ fontSize: 12, color: colors.textMuted, marginTop: 4, maxWidth: 900 }}>
          The platform's operational memory: claims with their evidence, the capabilities that answer recognised tasks, and the evals that
          pin known answers. A <em>candidate</em> claim is UNVERIFIED until its required verifier records a result. Times are America/Denver;
          hover for UTC.
        </div>
      </div>

      <StatsStrip nonce={nonce} />

      <SubNav items={PANES} active={pane} onChange={k => setPane(k as Pane)} ariaLabel="Brain panes" />

      {/* Panes stay mounted once visited (PORTAL_DESIGN_SYSTEM §4). */}
      <div style={{ display: pane === 'claims' ? 'block' : 'none' }}><ClaimsPane bump={bump} nonce={nonce} /></div>
      <div style={{ display: pane === 'capabilities' ? 'block' : 'none' }}><CapabilitiesPane bump={bump} nonce={nonce} /></div>
      <div style={{ display: pane === 'evals' ? 'block' : 'none' }}><EvalsPane bump={bump} nonce={nonce} /></div>
    </div>
  )
}

export default Brain
