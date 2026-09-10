// SupplyVerdict.tsx — Pane 4 of the Supply tab: THE FULFILLMENT VERDICT.
//
// The one screen the operator opens to answer "was the contract honoured?".
// One row per sending domain × ISP for the selected Denver day:
//
//     promised  →  minted  →  sent  →  delivered      verdict
//   [contracted]  [reserved] [actual]  [actual]     fulfilled | under:<reason> | over:<producer>
//
// Operator ruling 2026-09-09: the contract is not "up to", it is "exactly,
// within tolerance". OVER AND UNDER ARE EQUAL-WEIGHT broken promises, so this
// screen never ranks one above the other — the summary counts them together as
// `anomalies`, they sort by the SIZE of the breach, and over-delivery gets the
// same red weight under-delivery does. A screen that painted over-delivery green
// would be teaching the wrong lesson.
//
// It COMPUTES NOTHING. `status`, `reason` and the ±5% tolerance come from
// /api/mailing/supply/verdict, which projects `drip_fulfillment_verdict`, which
// agents/reporting/supply_reconcile.py writes at window close. There is one
// implementation of the rule; this file maps its strings onto colours. Two
// implementations of a verdict is two verdicts.
//
// Composed entirely from shared/ (Panel, SectionHeader, SectionError, EmptyState,
// Stat, Pill) and supplyShared (supplyGet, Num, Unknown, HeaderStrip, ScrollX,
// the table styles). The day comes from the tab's ONE FilterBar — there is no
// filter control in this file (PORTAL_DESIGN_SYSTEM §3 forbids a one-off bar).
// The domain/ISP/status narrowing below is a set of toggle CHIPS over the rows
// already fetched, not a second query surface.

import React from 'react'
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome'
import { faRotate, faScaleBalanced } from '@fortawesome/free-solid-svg-icons'
import { colors, alpha } from '../shared/theme'
import { Panel, SectionHeader, SectionError, EmptyState, Stat, Pill } from '../shared/ui'
import {
  SupplyMeta, supplyGet, Num, Unknown, HeaderStrip, LoadingRow, ScrollX,
  fmtInt, fmtTime,
  tableStyle, thStyle, tdStyle, numTd, numTh,
} from './supplyShared'

// ═══════════════════════════════════════════════════════════════════════════
// API TYPES (mirror of drip_supply_handlers.go dripVerdictRow / dripVerdictSummary)
// ═══════════════════════════════════════════════════════════════════════════

/** The closed status vocabulary (drip_fulfillment_verdict's CHECK constraint). */
export type VerdictStatus = 'fulfilled' | 'under' | 'over' | 'pending' | 'no_contract'

export interface VerdictRow {
  sending_domain: string
  isp: string
  /** `live` = the mediator governed this cell. `shadow` = it only observed. */
  mode: 'live' | 'shadow'
  domain_contract_version: number
  promised: number
  minted: number
  sent: number
  /** null = UNKNOWN (delivery confirms late; the lake read can fail). Never 0. */
  delivered: number | null
  delivered_source: string
  tolerance_pct: number
  status: VerdictStatus
  reason: string
  /** `under:no_lane_balance`, `over:internal_auto_insurance_v10`, `fulfilled`… */
  verdict: string
  delta: number | null
  delta_pct: number | null
  window_closed_at: string | null
  computed_at: string
}

export interface VerdictSummary {
  cells: number
  fulfilled: number
  under: number
  over: number
  pending: number
  no_contract: number
  enforced: number
  anomalies: number
  promised: number
  minted: number
  sent: number
  delivered: number | null
  under_volume: number
  over_volume: number
  computed_at: string | null
}

export interface VerdictResponse extends SupplyMeta {
  summary: VerdictSummary
  verdicts: VerdictRow[]
}

// ═══════════════════════════════════════════════════════════════════════════
// STATUS PRESENTATION — a MAP of the API's string, never a re-derivation
// ═══════════════════════════════════════════════════════════════════════════

const STATUS_COLOR: Record<VerdictStatus, string> = {
  fulfilled: colors.success,
  // Equal weight, by ruling. Both are broken promises.
  under: colors.danger,
  over: colors.danger,
  pending: colors.textFaint,
  no_contract: colors.warning,
}

const STATUS_HELP: Record<VerdictStatus, string> = {
  fulfilled: 'delivered is within ±tolerance of the promise',
  under: 'delivered below the band — the reason names the first broken link (mint → send → deliver)',
  over: 'delivered above the band — the reason names the producer that spent the cell',
  pending: 'delivered not measured yet — unknown, NOT under-delivered. Delivery confirms up to ~3h late at Microsoft.',
  no_contract: 'volume on an ISP the domain contract does not carry a value for — it cannot be graded, and it should not be mailing',
}

const STATUS_ORDER: VerdictStatus[] = ['under', 'over', 'no_contract', 'pending', 'fulfilled']

/** Sort: anomalies first, biggest breach first. Size, not sign. */
const bySeverity = (a: VerdictRow, b: VerdictRow): number => {
  const rank = (r: VerdictRow) => STATUS_ORDER.indexOf(r.status)
  if (rank(a) !== rank(b)) return rank(a) - rank(b)
  return Math.abs(b.delta ?? 0) - Math.abs(a.delta ?? 0)
}

const StatusPill: React.FC<{ row: VerdictRow }> = ({ row }) => (
  <span title={STATUS_HELP[row.status]}>
    <Pill color={STATUS_COLOR[row.status]} style={{ textTransform: 'none', letterSpacing: 0 }}>
      {row.verdict}
    </Pill>
  </span>
)

/**
 * The band a cell had to land in. Rendered next to the promise so "±5%" is a
 * visible number, not a footnote the operator has to remember.
 */
const band = (row: VerdictRow): string => {
  if (row.promised <= 0) return 'exactly 0'
  const slack = Math.round(row.promised * row.tolerance_pct)
  return `${fmtInt(row.promised - slack)}–${fmtInt(row.promised + slack)}`
}

// ═══════════════════════════════════════════════════════════════════════════
// PANE
// ═══════════════════════════════════════════════════════════════════════════

export const SupplyVerdict: React.FC<{
  day: string
  onSelectDomain?: (domain: string) => void
}> = ({ day, onSelectDomain }) => {
  const [data, setData] = React.useState<VerdictResponse | null>(null)
  const [err, setErr] = React.useState<string | null>(null)
  const [loading, setLoading] = React.useState(false)
  const [nonce, setNonce] = React.useState(0)
  // Narrowing chips over the rows ALREADY fetched — not a filter bar, not a
  // second query. `null` = show everything.
  const [statusPick, setStatusPick] = React.useState<VerdictStatus | null>(null)
  const [ispPick, setIspPick] = React.useState<string | null>(null)

  React.useEffect(() => {
    const ctrl = new AbortController()
    setLoading(true)
    supplyGet<VerdictResponse>('/verdict', { day }, ctrl.signal)
      .then(d => { setData(d); setErr(null) })
      .catch(e => { if (!ctrl.signal.aborted) setErr(e instanceof Error ? e.message : String(e)) })
      .finally(() => { if (!ctrl.signal.aborted) setLoading(false) })
    return () => ctrl.abort()
  }, [day, nonce])

  if (loading && !data) return <LoadingRow what="the fulfillment verdict" />

  const rows = data?.verdicts ?? []
  const s = data?.summary
  const labels = data?.labels ?? {}
  const shown = rows
    .filter(r => (statusPick ? r.status === statusPick : true))
    .filter(r => (ispPick ? r.isp === ispPick : true))
    .sort(bySeverity)

  return (
    <div>
      <HeaderStrip
        meta={data}
        extra={
          <>
            {s?.computed_at && (
              <span title="When the verdict runner last wrote this day. The verdict is a point-in-time judgement, not a live number.">
                verdict computed <strong style={{ color: colors.heading }}>{fmtTime(s.computed_at)}</strong>
              </span>
            )}
            <button
              type="button"
              onClick={() => setNonce(n => n + 1)}
              style={{
                background: alpha(colors.indigo500, '22'), border: `1px solid ${alpha(colors.indigo500, '66')}`,
                color: colors.indigo200, borderRadius: 6, padding: '3px 9px', fontSize: 11, fontWeight: 600, cursor: 'pointer',
              }}
            >
              <FontAwesomeIcon icon={faRotate} /> Refresh
            </button>
          </>
        }
      />

      <Panel>
        <SectionHeader
          title="Fulfillment verdict — did each domain × ISP get exactly what its contract promised?"
          icon={faScaleBalanced}
          right={
            <span style={{ fontSize: 11, color: colors.textFaint }}>
              tolerance ±{s ? Math.round((rows[0]?.tolerance_pct ?? 0.05) * 100) : 5}% of the promise · over and under are equal-weight
            </span>
          }
        />

        {err ? (
          <SectionError label="Fulfillment verdict" error={err} onRetry={() => setNonce(n => n + 1)} />
        ) : !s || s.cells === 0 ? (
          <EmptyState
            title="No verdict for this day"
            hint="The verdict runner (agents/reporting/supply_reconcile.py --verdict) has not written this day. That means the verdict is UNKNOWN — not that every contract was honoured."
          />
        ) : (
          <>
            <div style={{ display: 'flex', flexWrap: 'wrap', gap: 26, marginBottom: 14 }}>
              <Stat
                label="Anomalies"
                value={`${fmtInt(s.anomalies)} / ${fmtInt(s.cells)}`}
                color={s.anomalies > 0 ? colors.danger : colors.success}
                sub={`${fmtInt(s.under)} under · ${fmtInt(s.over)} over`}
                title="Cells whose delivered volume fell outside the promise's tolerance band, either way."
              />
              <Stat
                label="Under volume"
                value={fmtInt(s.under_volume)}
                color={s.under_volume > 0 ? colors.danger : colors.textMuted}
                sub="messages short of the promise"
              />
              <Stat
                label="Over volume"
                value={fmtInt(s.over_volume)}
                color={s.over_volume > 0 ? colors.danger : colors.textMuted}
                sub="messages beyond the promise"
              />
              <Stat
                label="Promised"
                value={fmtInt(s.promised)}
                sub="contracted"
                title="Sum of the domain contracts' daily_max_by_isp for this day."
              />
              <Stat
                label="Delivered"
                value={s.delivered == null ? <Unknown hint="no cell has a measured delivered figure yet" /> : fmtInt(s.delivered)}
                sub="actual · lake"
                title="Per-ISP delivery truth from ignite_analytics.email_events, attributed to the day's send cohort. Never PG counters."
              />
              <Stat
                label="Enforced"
                value={`${fmtInt(s.enforced)} / ${fmtInt(s.cells)}`}
                color={s.enforced === s.cells ? colors.success : colors.warning}
                sub="cells the mediator governed"
                title="A cell judged on the SHADOW surface is a would-be reading: the numbers are real, the enforcement was not."
              />
            </div>

            {/* Narrowing chips over the fetched rows. */}
            <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, alignItems: 'center', marginBottom: 10 }}>
              <span style={{ fontSize: 10, color: colors.textFaint, textTransform: 'uppercase', letterSpacing: 0.5 }}>show</span>
              {([null, ...STATUS_ORDER] as (VerdictStatus | null)[]).map(st => {
                const n = st == null ? s.cells : rows.filter(r => r.status === st).length
                if (st != null && n === 0) return null
                const active = statusPick === st
                return (
                  <button
                    key={st ?? 'all'}
                    type="button"
                    onClick={() => setStatusPick(active ? null : st)}
                    title={st == null ? 'every cell' : STATUS_HELP[st]}
                    style={{
                      background: active ? alpha(st == null ? colors.indigo500 : STATUS_COLOR[st], '33') : 'transparent',
                      border: `1px solid ${alpha(st == null ? colors.indigo500 : STATUS_COLOR[st], active ? '66' : '33')}`,
                      color: st == null ? colors.indigo200 : STATUS_COLOR[st],
                      borderRadius: 999, padding: '2px 10px', fontSize: 11, fontWeight: 600, cursor: 'pointer',
                    }}
                  >
                    {st ?? 'all'} {fmtInt(n)}
                  </button>
                )
              })}
              {ispPick && (
                <button
                  type="button"
                  onClick={() => setIspPick(null)}
                  style={{
                    background: alpha(colors.indigo500, '22'), border: `1px solid ${alpha(colors.indigo500, '66')}`,
                    color: colors.indigo200, borderRadius: 999, padding: '2px 10px', fontSize: 11, cursor: 'pointer',
                  }}
                >
                  isp={ispPick} ✕
                </button>
              )}
              <span style={{ fontSize: 11, color: colors.textFaint, marginLeft: 'auto' }}>
                {fmtInt(shown.length)} of {fmtInt(rows.length)} cell{rows.length === 1 ? '' : 's'}
              </span>
            </div>

            <ScrollX maxHeight={620}>
              <table style={tableStyle}>
                <thead>
                  <tr>
                    <th style={thStyle}>Sending domain</th>
                    <th style={thStyle}>ISP</th>
                    <th style={numTh} title="The contract's daily_max_by_isp for this day, and the band ±tolerance around it.">
                      Promised
                    </th>
                    <th style={numTh} title="What the mediator held: SUM(reserved) − SUM(released) on the authoritative ledger surface.">
                      Minted
                    </th>
                    <th style={numTh} title="PG: enqueued_count across the day's [partner-drip] ISP plans.">
                      Sent
                    </th>
                    <th style={numTh} title="The lake: delivered events for the day's send cohort. Null = unknown, never 0.">
                      Delivered
                    </th>
                    <th style={numTh}>Δ vs promise</th>
                    <th style={thStyle}>Verdict</th>
                    <th style={thStyle} title="live = the mediator governed the cell; shadow = it only observed.">
                      Surface
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {shown.map(r => (
                    <tr
                      key={`${r.sending_domain}/${r.isp}`}
                      style={{
                        cursor: onSelectDomain ? 'pointer' : undefined,
                        background: r.status === 'under' || r.status === 'over'
                          ? alpha(colors.danger, '0d') : undefined,
                      }}
                      onClick={onSelectDomain ? () => onSelectDomain(r.sending_domain) : undefined}
                    >
                      <td style={tdStyle}>
                        {r.sending_domain}
                        <div style={{ fontSize: 10, color: colors.textFaint }}>
                          contract v{r.domain_contract_version || '—'}
                        </div>
                      </td>
                      <td
                        style={{ ...tdStyle, cursor: 'pointer' }}
                        onClick={e => { e.stopPropagation(); setIspPick(p => (p === r.isp ? null : r.isp)) }}
                        title="Click to narrow the table to this ISP"
                      >
                        {r.isp}
                      </td>
                      <td style={numTd}>
                        <Num value={r.promised} label={labels['promised']} what="the contract's promise for this cell" />
                        <div style={{ fontSize: 10, color: colors.textFaint }}>band {band(r)}</div>
                      </td>
                      <td style={numTd}>
                        <Num value={r.minted} label={labels['minted']} what="reserved − released on the authoritative ledger surface" />
                      </td>
                      <td style={numTd}>
                        <Num value={r.sent} label={labels['sent']} what="enqueued_count on the day's [partner-drip] ISP plans" />
                      </td>
                      <td style={numTd}>
                        <Num
                          value={r.delivered}
                          label={labels['delivered']}
                          what={r.delivered_source || 'the analytics lake'}
                          unknownHint="delivery is not measured for this cell yet — unknown, NOT zero. Microsoft confirms up to ~3h late."
                        />
                      </td>
                      <td style={numTd}>
                        {r.delta == null ? (
                          <Unknown hint="no delivered figure, so there is no delta" />
                        ) : (
                          <span style={{ color: r.status === 'fulfilled' ? colors.textMuted : STATUS_COLOR[r.status] }}>
                            {r.delta > 0 ? '+' : ''}{fmtInt(r.delta)}
                            {r.delta_pct != null && (
                              <span style={{ color: colors.textFaint, marginLeft: 5 }}>
                                {r.delta_pct > 0 ? '+' : ''}{(r.delta_pct * 100).toFixed(0)}%
                              </span>
                            )}
                          </span>
                        )}
                      </td>
                      <td style={tdStyle}><StatusPill row={r} /></td>
                      <td style={tdStyle}>
                        <span
                          style={{ fontSize: 11, color: r.mode === 'live' ? colors.successText : colors.textFaint }}
                          title={r.mode === 'live'
                            ? 'the mediator governed this cell — this verdict describes enforcement that happened'
                            : 'the mediator only observed this cell — a would-be reading'}
                        >
                          {r.mode}
                        </span>
                      </td>
                    </tr>
                  ))}
                  {shown.length === 0 && (
                    <tr>
                      <td style={tdStyle} colSpan={9}>
                        <span style={{ color: colors.textMuted, fontSize: 12 }}>
                          No cell matches the current narrowing.
                        </span>
                      </td>
                    </tr>
                  )}
                </tbody>
              </table>
            </ScrollX>
          </>
        )}
      </Panel>
    </div>
  )
}

export default SupplyVerdict
