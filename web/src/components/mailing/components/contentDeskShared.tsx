// contentDeskShared.tsx — types, fetch helpers and display primitives for the
// Content Desk tab (ContentDesk*.tsx) over /api/mailing/content-desk/*.
//
// Rules this module encodes (PORTAL_DESIGN_SYSTEM §1.6 + §6, METRIC_CONTRACT §11.7):
//   · every request goes through the shared apiFetch (org header + credentials);
//   · a 404 says "not found / backend may not be deployed", never an empty list;
//   · null renders as "unknown" (supplyShared's Unknown), never 0;
//   · NO RAW HTML — every article field is rendered as a React text node, and a
//     field that looks like markup carries a visible "HTML shown escaped" marker;
//   · a review is bound to the revision_hash the reviewer READ; a 409 means the
//     revision moved underneath them (the caller reloads).

import React from 'react'
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome'
import {
  faCircleCheck, faTriangleExclamation, faCircleXmark, faCircleQuestion, faCode,
} from '@fortawesome/free-solid-svg-icons'
import { apiFetch } from '../shared/apiFetch'
import { colors, alpha, btnStyle } from '../shared/theme'
import { Pill } from '../shared/ui'
import { Unknown, fmtClock } from './supplyShared'

// ═══════════════════════════════════════════════════════════════════════════
// TYPES — the API contract (every field the backend may omit is optional/nullable)
// ═══════════════════════════════════════════════════════════════════════════

export interface Site {
  id: string
  brand_code: string
  domain: string
  surface: string
  enabled: boolean
}

export type Severity = 'S1' | 'S2' | 'S3'
export const SEVERITIES: Severity[] = ['S1', 'S2', 'S3']

export type FindingCounts = Partial<Record<Severity, number | null>>

export interface ArticleRow {
  id: string
  site_id: string
  domain: string
  slug: string
  status: string
  title: string
  consequential: boolean
  current_revision_hash: string | null
  updated_at: string | null
  open_findings: FindingCounts | null
}

export interface Block {
  id: string
  type: string
  text?: unknown
  items?: unknown
  rows?: unknown
  [k: string]: unknown
}

export interface ArticlePackage {
  title?: string | null
  excerpt?: string | null
  meta_title?: string | null
  meta_description?: string | null
  blocks?: Block[] | null
  hero_image?: unknown
  subjects?: string[] | null
  preheaders?: string[] | null
}

export interface CodeCheck {
  name: string
  passed: boolean
  detail?: string | null
}

export type Verdict = 'supported' | 'overstated' | 'unsupported'

export interface Judgment {
  block_id: string
  sentence_idx: number
  sentence: string
  verdict: Verdict | string
  lost_qualifier?: string | null
  claim_id: string
  version: number | string
}

export interface Revision {
  id: string
  revision_hash: string
  package: ArticlePackage | null
  checks: { code?: CodeCheck[] | null; judgment?: Judgment[] | null } | null
  usage?: unknown
}

export interface Claim {
  claim_id: string
  version: number | string
  text?: string | null
  type?: string | null
  source_url?: string | null
  passage?: string | null
  context?: string | null
  published_at?: string | null
  effective_at?: string | null
  retrieved_at?: string | null
  jurisdiction?: string | null
  population?: string | null
  conditions?: unknown
  status?: string | null
  derivation?: unknown
}

export interface ReviewFinding {
  severity: Severity
  caught_by: string
  block_id: string
  text: string
}

export interface ReviewRecord {
  id?: string
  role?: string | null
  decision?: string | null
  revision_hash?: string | null
  minutes?: number | null
  findings?: ReviewFinding[] | null
  reviewer?: string | null
  created_at?: string | null
  [k: string]: unknown
}

export interface ArticleDetail {
  article: ArticleRow
  revision: Revision | null
  claims: Claim[] | null
  reviews: ReviewRecord[] | null
}

export type ReviewRole = 'primary' | 'second'
export type ReviewDecision = 'approve' | 'reject' | 'changes'

export interface ReviewPayload {
  revision_hash: string
  role: ReviewRole
  decision: ReviewDecision
  findings: ReviewFinding[]
  minutes: number
}

export interface BriefInput {
  site_id: string
  reader_question: string
  format: string
  category: string
  angle?: string
}

export interface Brief {
  id?: string
  site_id?: string | null
  reader_question?: string | null
  format?: string | null
  category?: string | null
  angle?: string | null
  status?: string | null
  article_id?: string | null
  article?: { id?: string | null } | null
  created_at?: string | null
  [k: string]: unknown
}

export type CheckStatus = 'ok' | 'warn' | 'fail' | 'unknown'

export interface OpsCheck {
  name: string
  status: CheckStatus | string
  detail?: string | null
  checked_at?: string | null
}

export interface StageCounts {
  queued?: number | null
  running?: number | null
  failed?: number | null
  done?: number | null
}

export interface SupplyConsumer {
  name: string
  brand: string
  eligible: number | null
  runway_days: number | null
  status: string
}

export interface OpsRelease {
  domain: string
  last_status: string | null
  last_at: string | null
  error: string | null
}

export interface OpsStatus {
  pipeline: { by_stage: Record<string, StageCounts> | null } | null
  review_queue: { size: number | null; oldest_age_hours: number | null } | null
  releases: OpsRelease[] | null
  spend: { today_usd: number | null; budget_usd: number | null } | null
  enabled: boolean | null
  models: { write?: string | null; judge?: string | null; light?: string | null } | null
  last_error: string | null
  supply: { consumers: SupplyConsumer[] | null } | null
  checks: OpsCheck[] | null
}

// ═══════════════════════════════════════════════════════════════════════════
// FETCH — every request through the shared apiFetch
// ═══════════════════════════════════════════════════════════════════════════

export const CD_BASE = '/api/mailing/content-desk'

/** A failed Content Desk request; `status` 404 means not found / not deployed. */
export class ContentDeskError extends Error {
  status: number
  constructor(message: string, status: number) {
    super(message)
    this.name = 'ContentDeskError'
    this.status = status
  }
}

async function parseError(res: Response): Promise<ContentDeskError> {
  let message = `HTTP ${res.status}`
  try {
    const body: unknown = await res.json()
    if (body && typeof body === 'object') {
      const rec = body as Record<string, unknown>
      if (typeof rec.error === 'string' && rec.error.trim()) message = `HTTP ${res.status}: ${rec.error}`
    }
  } catch {
    /* a non-JSON error body stays as the status line */
  }
  if (res.status === 404) message += ' — not found (the Content Desk backend may not be deployed yet)'
  return new ContentDeskError(message, res.status)
}

const buildUrl = (path: string, params: Record<string, string | undefined> = {}) => {
  const qs = new URLSearchParams()
  Object.entries(params).forEach(([k, v]) => {
    if (v != null && v !== '') qs.set(k, v)
  })
  const q = qs.toString()
  return `${CD_BASE}${path}${q ? `?${q}` : ''}`
}

export async function cdGet<T>(
  path: string,
  params: Record<string, string | undefined> = {},
  signal?: AbortSignal,
): Promise<T> {
  const res = await apiFetch(buildUrl(path, params), signal ? { signal } : {})
  if (!res.ok) throw await parseError(res)
  return (await res.json()) as T
}

/** POST; an empty or non-JSON success body resolves to null. */
export async function cdPost<T>(path: string, body?: unknown): Promise<T | null> {
  const res = await apiFetch(buildUrl(path), {
    method: 'POST',
    ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
  })
  if (!res.ok) throw await parseError(res)
  const txt = await res.text()
  if (!txt.trim()) return null
  try {
    return JSON.parse(txt) as T
  } catch {
    return null
  }
}

export const errMsg = (e: unknown): string => (e instanceof Error ? e.message : String(e))

export const articlePath = (id: string, tail = '') => `/articles/${encodeURIComponent(id)}${tail}`
export const runPipeline = (id: string) => cdPost<unknown>(articlePath(id, '/run'))
export const withdrawArticle = (id: string) => cdPost<unknown>(articlePath(id, '/withdraw'))

/** When a panel's data was fetched and how long it took (design system §1.6). */
export interface FetchStamp {
  at: number
  ms: number
}

export const FetchNote: React.FC<{ stamp: FetchStamp | null | undefined }> = ({ stamp }) =>
  stamp ? (
    <span style={{ fontSize: 11, color: colors.textFaint, fontVariantNumeric: 'tabular-nums' }}>
      fetched {fmtClock(new Date(stamp.at).toISOString())} MT · {stamp.ms}ms
    </span>
  ) : null

export interface Loadable<T> {
  data: T | null
  loading: boolean
  error: string | null
  stamp: FetchStamp | null
  reload: () => void
}

/**
 * One-shot GET with abort + reload. The last good data stays mounted across a
 * reload of the SAME key (no blanking), but is never shown under a different
 * key (switching articles never flashes the previous article). `path` null =
 * idle (nothing selected).
 */
export function useCdGet<T>(path: string | null, params: Record<string, string | undefined> = {}): Loadable<T> {
  const paramKey = JSON.stringify(params)
  const fullKey = path == null ? null : `${path}?${paramKey}`
  const [state, setState] = React.useState<{
    key: string | null
    data: T | null
    loading: boolean
    error: string | null
    stamp: FetchStamp | null
  }>({ key: null, data: null, loading: path != null, error: null, stamp: null })
  const [nonce, setNonce] = React.useState(0)

  React.useEffect(() => {
    if (path == null) {
      setState({ key: null, data: null, loading: false, error: null, stamp: null })
      return
    }
    const ctrl = new AbortController()
    setState(s => ({ ...s, loading: true, error: null }))
    const t0 = performance.now()
    cdGet<T>(path, JSON.parse(paramKey) as Record<string, string | undefined>, ctrl.signal)
      .then(d => {
        if (ctrl.signal.aborted) return
        setState({ key: fullKey, data: d, loading: false, error: null, stamp: { at: Date.now(), ms: Math.round(performance.now() - t0) } })
      })
      .catch(e => {
        if (ctrl.signal.aborted) return
        setState(s => ({ ...s, loading: false, error: errMsg(e) }))
      })
    return () => ctrl.abort()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [path, paramKey, nonce])

  const reload = React.useCallback(() => setNonce(n => n + 1), [])
  const own = state.key === fullKey
  return {
    data: own ? state.data : null,
    loading: state.loading || (fullKey != null && !own && state.error == null),
    error: state.error,
    stamp: own ? state.stamp : null,
    reload,
  }
}

// ═══════════════════════════════════════════════════════════════════════════
// REVIEW SUBMISSION — bound to the revision the reviewer read
// ═══════════════════════════════════════════════════════════════════════════

export type FindingDraft = ReviewFinding

/**
 * Build the review body. Throws (with an operator-readable message) when the
 * revision hash is missing or minutes is not a number ≥ 0. Findings with blank
 * text are dropped; the rest are trimmed.
 */
export function buildReviewPayload(input: {
  revisionHash: string | null | undefined
  role: ReviewRole
  decision: ReviewDecision
  findings: FindingDraft[]
  minutes: number | string
}): ReviewPayload {
  const hash = (input.revisionHash ?? '').trim()
  if (!hash) throw new Error('No revision hash — reload the article before submitting a review.')
  const raw = String(input.minutes).trim()
  const minutes = Number(raw)
  if (raw === '' || !Number.isFinite(minutes) || minutes < 0) {
    throw new Error('Minutes spent must be a number ≥ 0.')
  }
  const findings = input.findings
    .filter(f => f.text.trim() !== '')
    .map(f => ({ severity: f.severity, caught_by: f.caught_by.trim(), block_id: f.block_id.trim(), text: f.text.trim() }))
  return { revision_hash: hash, role: input.role, decision: input.decision, findings, minutes }
}

export type SubmitResult =
  | { kind: 'ok' }
  | { kind: 'conflict'; message: string }
  | { kind: 'error'; message: string; status: number }

/** POST the review. A 409 is a CONFLICT (revision changed) — never a generic error. */
export async function submitReview(articleId: string, payload: ReviewPayload): Promise<SubmitResult> {
  try {
    await cdPost<unknown>(articlePath(articleId, '/reviews'), payload)
    return { kind: 'ok' }
  } catch (e) {
    if (e instanceof ContentDeskError && e.status === 409) return { kind: 'conflict', message: e.message }
    return { kind: 'error', message: errMsg(e), status: e instanceof ContentDeskError ? e.status : 0 }
  }
}

// ═══════════════════════════════════════════════════════════════════════════
// ESCAPED TEXT — the only way article content reaches the DOM
// ═══════════════════════════════════════════════════════════════════════════

const HTML_RE = /<\/?[a-z!][^>]*>|&(?:#\d+|#x[0-9a-f]+|[a-z][a-z0-9]+);/i

/** True when a string contains tag- or entity-shaped markup. */
export const containsHtml = (v: unknown): boolean => typeof v === 'string' && HTML_RE.test(v)

/** Any API value → a plain string (objects with `text` use it; others JSON). */
export const asText = (v: unknown): string => {
  if (v == null) return ''
  if (typeof v === 'string') return v
  if (typeof v === 'number' || typeof v === 'boolean') return String(v)
  if (Array.isArray(v)) return v.map(asText).join(' · ')
  if (typeof v === 'object' && typeof (v as { text?: unknown }).text === 'string') return (v as { text: string }).text
  try {
    return JSON.stringify(v)
  } catch {
    return String(v)
  }
}

export const EscapedMarker: React.FC = () => (
  <span
    data-testid="html-escaped-marker"
    title="This field contains HTML markup. It is shown ESCAPED, as literal text — the Content Desk never renders raw HTML."
    style={{
      marginLeft: 6, fontSize: 9, fontWeight: 700, letterSpacing: 0.5, textTransform: 'uppercase',
      color: colors.warningText, background: alpha(colors.warning, '14'),
      border: `1px solid ${alpha(colors.warning, '44')}`, borderRadius: 4, padding: '1px 5px',
      whiteSpace: 'nowrap', verticalAlign: 'middle',
    }}
  >
    <FontAwesomeIcon icon={faCode} /> HTML shown escaped
  </span>
)

/** A field value as escaped text. null → unknown; '' → "empty" (built empty ≠ unknown). */
export const SafeText: React.FC<{ value: unknown; unknownHint?: string }> = ({ value, unknownHint }) => {
  if (value == null) return <Unknown hint={unknownHint ?? 'not present in the response'} />
  const s = asText(value)
  if (s === '') return <span style={{ color: colors.textFaint, fontStyle: 'italic' }}>empty</span>
  return (
    <>
      {s}
      {containsHtml(s) && <EscapedMarker />}
    </>
  )
}

/** Only http(s) URLs are ever used as an href / img src. */
export const safeUrl = (v: unknown): string | null =>
  typeof v === 'string' && /^https?:\/\//i.test(v.trim()) ? v.trim() : null

// ═══════════════════════════════════════════════════════════════════════════
// JUDGED SENTENCES — placing judgments into block text
// ═══════════════════════════════════════════════════════════════════════════

export interface SentenceGroup {
  key: string
  blockId: string
  sentenceIdx: number
  sentence: string
  judgments: Judgment[]
}

/** Group judgments by (block, sentence) — one sentence can carry several claims. */
export function groupJudgments(js: Judgment[]): SentenceGroup[] {
  const map = new Map<string, SentenceGroup>()
  js.forEach(j => {
    const key = `${j.block_id}#${j.sentence_idx}`
    const g = map.get(key)
    if (g) g.judgments.push(j)
    else map.set(key, { key, blockId: j.block_id, sentenceIdx: j.sentence_idx, sentence: j.sentence ?? '', judgments: [j] })
  })
  return Array.from(map.values()).sort((a, b) =>
    a.blockId === b.blockId ? a.sentenceIdx - b.sentenceIdx : a.blockId < b.blockId ? -1 : 1,
  )
}

export interface Segment {
  text: string
  group: SentenceGroup | null
}

/**
 * Split a block's text pieces (one for a paragraph, one per list item / table
 * cell) into plain and judged segments. Sentences are located VERBATIM, in
 * sentence_idx order, never overlapping; a sentence that cannot be located is
 * returned in `unplaced` (the UI lists it under the block — never dropped).
 */
export function segmentPieces(pieces: string[], groups: SentenceGroup[]): { pieces: Segment[][]; unplaced: SentenceGroup[] } {
  const placed: Array<{ piece: number; start: number; end: number; group: SentenceGroup }> = []
  const unplaced: SentenceGroup[] = []
  let p = 0
  let off = 0
  groups.forEach(g => {
    const needle = g.sentence.trim()
    if (!needle) {
      unplaced.push(g)
      return
    }
    for (let i = p; i < pieces.length; i++) {
      const at = pieces[i].indexOf(needle, i === p ? off : 0)
      if (at >= 0) {
        placed.push({ piece: i, start: at, end: at + needle.length, group: g })
        p = i
        off = at + needle.length
        return
      }
    }
    unplaced.push(g)
  })
  const out = pieces.map((text, i) => {
    const segs: Segment[] = []
    let cur = 0
    placed.filter(x => x.piece === i).forEach(x => {
      if (x.start > cur) segs.push({ text: text.slice(cur, x.start), group: null })
      segs.push({ text: text.slice(x.start, x.end), group: x.group })
      cur = x.end
    })
    if (cur < text.length || segs.length === 0) segs.push({ text: text.slice(cur), group: null })
    return segs
  })
  return { pieces: out, unplaced }
}

export const claimKey = (id: string, version: number | string) => `${id}@${String(version)}`

// ═══════════════════════════════════════════════════════════════════════════
// STATE → FORM (colour AND icon, so state never rides on colour alone)
// ═══════════════════════════════════════════════════════════════════════════

export const verdictColor = (v: string): string =>
  v === 'supported' ? colors.success : v === 'overstated' ? colors.warning : v === 'unsupported' ? colors.danger : colors.idle

const VERDICT_RANK: Record<string, number> = { unsupported: 3, overstated: 2, supported: 1 }

export const worstVerdict = (js: Judgment[]): string | null =>
  js.reduce<string | null>((w, j) => ((VERDICT_RANK[j.verdict] ?? 0) > (w ? VERDICT_RANK[w] ?? 0 : -1) ? j.verdict : w), null)

export const VerdictChip: React.FC<{ verdict: string }> = ({ verdict }) => (
  <Pill color={verdictColor(verdict)} style={{ fontSize: 10, padding: '1px 8px' }}>
    <span data-testid="verdict-chip">{verdict}</span>
  </Pill>
)

export const checkColor = (s: string): string =>
  s === 'ok' ? colors.success : s === 'warn' ? colors.warning : s === 'fail' ? colors.danger : colors.idle

const CHECK_ICON = { ok: faCircleCheck, warn: faTriangleExclamation, fail: faCircleXmark } as const

export const CHECK_ORDER: Record<string, number> = { fail: 0, warn: 1, unknown: 2, ok: 3 }

/** Normalises a status string to the four-state vocabulary; anything else is unknown. */
export const normCheck = (s: string | null | undefined): CheckStatus =>
  s === 'ok' || s === 'warn' || s === 'fail' ? s : 'unknown'

/** Worst of a set of server-computed statuses (a roll-up, not a recomputation). */
export const worstCheck = (statuses: Array<string | null | undefined>): CheckStatus =>
  statuses.map(normCheck).reduce<CheckStatus>((w, s) => (CHECK_ORDER[s] < CHECK_ORDER[w] ? s : w), 'ok')

export const CheckPill: React.FC<{ status: string | null | undefined; label?: string }> = ({ status, label }) => {
  const s = normCheck(status)
  const icon = s === 'unknown' ? faCircleQuestion : CHECK_ICON[s]
  return (
    <Pill color={checkColor(s)} style={{ fontSize: 10, padding: '1px 8px' }}>
      <FontAwesomeIcon icon={icon} /> {label ?? s}
    </Pill>
  )
}

export const sevColor = (s: Severity): string => (s === 'S1' ? colors.danger : s === 'S2' ? colors.warning : colors.idle)

/** S1/S2/S3 open-finding chips. null counts render as unknown, never 0. */
export const FindingChips: React.FC<{ counts: FindingCounts | null | undefined }> = ({ counts }) => {
  if (!counts) return <Unknown hint="the API sent no open_findings for this article" />
  return (
    <span style={{ display: 'inline-flex', gap: 4 }}>
      {SEVERITIES.map(s => {
        const n = counts[s]
        const c = sevColor(s)
        const zero = n === 0
        return (
          <span
            key={s}
            title={n == null ? `${s}: unknown — not reported` : `${n} open ${s} finding${n === 1 ? '' : 's'}`}
            style={{
              fontSize: 10, fontWeight: 700, fontVariantNumeric: 'tabular-nums', borderRadius: 4, padding: '1px 6px',
              color: zero || n == null ? colors.textFaint : c,
              background: zero || n == null ? 'transparent' : alpha(c, '22'),
              border: `1px solid ${zero || n == null ? colors.hairline : alpha(c, '66')}`,
              fontStyle: n == null ? 'italic' : 'normal',
            }}
          >
            {s} {n == null ? '?' : n}
          </span>
        )
      })}
    </span>
  )
}

export const decisionColor = (d: string | null | undefined): string =>
  d === 'approve' ? colors.success : d === 'changes' ? colors.warning : d === 'reject' ? colors.danger : colors.idle

// ═══════════════════════════════════════════════════════════════════════════
// SMALL FORMATTERS
// ═══════════════════════════════════════════════════════════════════════════

export const hoursSince = (iso: string | null | undefined, now = Date.now()): number | null => {
  if (!iso) return null
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return null
  return Math.max(0, (now - t) / 3_600_000)
}

export const fmtHours = (h: number | null | undefined): string => {
  if (h == null) return ''
  if (h < 1) return `${Math.round(h * 60)}m`
  if (h < 48) return `${h.toFixed(1)}h`
  return `${Math.floor(h / 24)}d ${Math.round(h % 24)}h`
}

export const mono: React.CSSProperties = {
  fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
  fontSize: 11,
}

/** A revision hash / id — monospace, truncated, full value in the tooltip. */
export const Hash: React.FC<{ value: string | null | undefined; n?: number }> = ({ value, n = 10 }) =>
  value ? (
    <span title={value} style={{ ...mono, color: colors.indigo200 }}>
      {value.slice(0, n)}
      {value.length > n ? '…' : ''}
    </span>
  ) : (
    <Unknown hint="no revision hash" />
  )

/** btnStyle recoloured to a semantic tone. */
export const toneBtn = (c: string): React.CSSProperties => ({
  ...btnStyle,
  background: alpha(c, '22'),
  border: `1px solid ${alpha(c, '66')}`,
  color: c,
})
