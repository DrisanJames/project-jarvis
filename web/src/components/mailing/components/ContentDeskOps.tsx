// ContentDeskOps.tsx — Pane 3 of the Content Desk: "Is everything working?"
//
// One screen, answered at a glance, over GET ops-status exactly as the backend
// sends it (contentdesk.OpsStatus). State is encoded in FORM — every status is
// a pill with an icon (ok ✓ / warn ! / fail ✕ / unknown ?), not colour alone.
// Statuses are the server's (checks[].status); the headline is only the worst
// of them, a roll-up — never a recomputed verdict (§6.4). The pipeline table is
// a display pivot of the server's (stage, status, count) rows.
// null anywhere renders "unknown", never 0 (METRIC_CONTRACT §11.7).
// Polls every 30s via usePolling; the last good snapshot stays mounted on a
// failed refresh, with the error shown above it.

import React from 'react'
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome'
import {
  faGaugeHigh, faRotate, faDiagramProject, faRocket, faBoxesStacked, faShieldHalved, faTriangleExclamation,
} from '@fortawesome/free-solid-svg-icons'
import { colors, alpha, btnStyle, cardGrid, stateColor } from '../shared/theme'
import { Panel, SectionHeader, SectionError, EmptyState, Pill, Stat, LivePill, PortalKeyframes, ProgressBar } from '../shared/ui'
import { usePolling } from '../shared/usePolling'
import { ScrollX, LoadingRow, Unknown, fmtTime, fmtUSD, tableStyle, thStyle, tdStyle, numTd, numTh } from './supplyShared'
import {
  cdGet, FetchNote, CheckPill, SafeText, Hash, worstCheck, normCheck, checkColor, fmtHours, pivotPipeline, CHECK_ORDER,
  type OpsStatus, type FetchStamp,
} from './contentDeskShared'

const POLL_MS = 30_000

const n0 = (v: number | null | undefined): React.ReactNode => (v == null ? <Unknown /> : v.toLocaleString())

export const ContentDeskOps: React.FC = () => {
  const state = usePolling<{ ops: OpsStatus; stamp: FetchStamp }>(
    React.useCallback(async (signal: AbortSignal) => {
      const t0 = performance.now()
      const ops = await cdGet<OpsStatus>('/ops-status', {}, signal)
      return { ops, stamp: { at: Date.now(), ms: Math.round(performance.now() - t0) } }
    }, []),
    POLL_MS,
  )

  if (state.loading && !state.data) return <LoadingRow what="ops status" />
  if (!state.data) return <SectionError label="Ops status" error={state.error ?? 'no data'} onRetry={state.refresh} />

  const o = state.data.ops ?? ({} as OpsStatus)
  const checks = Array.isArray(o.checks) ? o.checks : null
  const worst = checks && checks.length ? worstCheck(checks.map(c => c.status)) : null
  const counts = (checks ?? []).reduce<Record<string, number>>((m, c) => {
    const s = normCheck(c.status)
    m[s] = (m[s] ?? 0) + 1
    return m
  }, {})

  const pipeline = Array.isArray(o.pipeline) ? o.pipeline : null
  const piv = pipeline ? pivotPipeline(pipeline) : null
  const statusTotal = (st: string) => (pipeline ? pipeline.filter(r => r.status === st).reduce((t, r) => t + r.count, 0) : null)
  const failedTotal = statusTotal('failed')

  const releases = Array.isArray(o.releases) ? o.releases : null
  const releaseErrors = (releases ?? []).filter(r => r.error && r.error.trim()).length

  const consumers = Array.isArray(o.supply?.consumers) ? o.supply.consumers : null
  const shortest = (consumers ?? []).reduce<{ name: string; days: number } | null>((m, c) =>
    c.runway_days == null ? m : m == null || c.runway_days < m.days ? { name: c.name ?? '', days: c.runway_days } : m, null)

  const spend = o.spend?.today_usd ?? null
  const budget = o.spend?.budget_usd ?? null
  const spendPct = spend != null && budget != null && budget > 0 ? spend / budget : null
  const enabled = o.kill_switch?.enabled ?? null
  const oldestSecs = o.review_queue?.oldest_age_seconds ?? null
  const lastErr = o.last_pipeline_error ?? null

  return (
    <div>
      <PortalKeyframes />

      {/* ── The answer ───────────────────────────────────────────────── */}
      <Panel accent={worst ? checkColor(worst) : colors.idle} style={{ marginBottom: 14 }}>
        <SectionHeader
          title="Is everything working?"
          icon={faGaugeHigh}
          right={
            <span style={{ display: 'inline-flex', alignItems: 'center', gap: 10 }}>
              <LivePill live={state.live} agoSeconds={state.secondsSinceUpdate} />
              <FetchNote stamp={state.data.stamp} />
              <button type="button" style={{ ...btnStyle, padding: '3px 9px', fontSize: 11 }} onClick={state.refresh}>
                <FontAwesomeIcon icon={faRotate} /> Refresh
              </button>
            </span>
          }
        />
        {state.error && (
          <div style={{ marginBottom: 10 }}>
            <SectionError label="Ops refresh (showing the last good snapshot)" error={state.error} onRetry={state.refresh} />
          </div>
        )}
        <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap', marginBottom: 12 }}>
          {worst ? <CheckPill status={worst} label={worst === 'ok' ? 'all ok' : worst} /> : <Unknown hint="the backend reported no checks[]" />}
          <span style={{ fontSize: 12, color: colors.textMuted }} title="Worst of the server-computed check statuses — a roll-up, not a recomputation.">
            {checks == null
              ? 'No checks reported — state unknown.'
              : `${counts.fail ?? 0} fail · ${counts.warn ?? 0} warn · ${counts.unknown ?? 0} unknown · ${counts.ok ?? 0} ok — worst of ${checks.length} ops checks`}
          </span>
        </div>

        <div style={cardGrid(150)}>
          <Stat
            label="Kill switch"
            value={
              enabled == null ? <Unknown hint="kill_switch not reported" />
                : enabled ? <Pill color={colors.success}>enabled</Pill>
                  : <Pill color={colors.danger}>killed</Pill>
            }
            sub={enabled == null ? undefined : enabled ? `${o.kill_switch.env}=1 — pipeline may run` : `${o.kill_switch.env} is not 1 — nothing runs`}
          />
          <Stat
            label="Spend today (USD) vs daily budget"
            value={spend == null ? <Unknown hint="today_usd not reported" /> : fmtUSD(spend)}
            color={o.spend?.exceeded || (spendPct != null && spendPct > 0.9) ? colors.dangerText : undefined}
            sub={
              <span>
                of {budget == null ? <Unknown hint="budget_usd not reported" /> : fmtUSD(budget)}
                {o.spend?.day ? ` · ${o.spend.day} (Denver)` : ''}
                {spendPct != null && <> · {(spendPct * 100).toFixed(0)}% used</>}
                {o.spend?.exceeded && <> · exceeded — calls fail closed</>}
                {spendPct != null && <span style={{ display: 'block', marginTop: 4 }}><ProgressBar pct={Math.min(spendPct, 1)} height={6} /></span>}
              </span>
            }
          />
          <Stat
            label="Review queue"
            value={n0(o.review_queue?.size)}
            sub={<span>oldest waiting {oldestSecs == null ? <Unknown hint="nothing in review" /> : fmtHours(oldestSecs / 3600)}</span>}
          />
          <Stat
            label="Pipeline failed runs (all stages)"
            value={n0(failedTotal)}
            color={(failedTotal ?? 0) > 0 ? colors.dangerText : undefined}
            sub={piv ? `across ${piv.stages.length} stage${piv.stages.length === 1 ? '' : 's'}` : undefined}
          />
          <Stat
            label="Releases with an error"
            value={releases == null ? <Unknown hint="releases not reported" /> : releaseErrors}
            color={releaseErrors > 0 ? colors.dangerText : undefined}
            sub={releases == null ? undefined : `of ${releases.length} site${releases.length === 1 ? '' : 's'}`}
          />
          <Stat
            label="Shortest newsletter runway (days)"
            value={consumers == null ? <Unknown hint="no supply_runway report (supply.consumers is null)" /> : shortest == null ? <Unknown hint="no consumer reported runway_days" /> : shortest.days.toLocaleString()}
            color={shortest != null && shortest.days < 2 ? colors.dangerText : undefined}
            sub={shortest?.name}
          />
          <Stat
            label="Models"
            value={<span style={{ fontSize: 12, fontWeight: 600 }}>
              {(['write', 'judge', 'light'] as const).map(k => (
                <span key={k} style={{ display: 'block' }}>
                  <span style={{ color: colors.textFaint }}>{k} </span>
                  {o.models?.[k] ? <SafeText value={o.models[k]} /> : <Unknown />}
                </span>
              ))}
            </span>}
          />
        </div>

        {lastErr && (
          <div data-testid="last-pipeline-error" style={{ marginTop: 12, padding: '8px 10px', borderRadius: 6, background: alpha(colors.danger, '14'), border: `1px solid ${alpha(colors.danger, '44')}`, fontSize: 12, color: colors.dangerText, overflowWrap: 'anywhere' }}>
            <FontAwesomeIcon icon={faTriangleExclamation} /> last pipeline error · {lastErr.stage}
            {lastErr.at ? ` · ${fmtTime(lastErr.at)} MT` : ''} · article <Hash value={lastErr.article_id} />: <SafeText value={lastErr.error} />
          </div>
        )}
      </Panel>

      {/* ── Checks ───────────────────────────────────────────────────── */}
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title="Checks" icon={faShieldHalved} right={<span style={{ fontSize: 11, color: colors.textFaint }}>server checks + the latest Mac-side ops reports — as the backend reports them</span>} />
        {checks == null ? (
          <Unknown hint="checks not reported" />
        ) : checks.length === 0 ? (
          <EmptyState icon={faShieldHalved} title="The backend reported zero checks" hint="An empty list is not a pass — nothing is being checked." />
        ) : (
          <ScrollX>
            <table style={tableStyle}>
              <thead>
                <tr>
                  <th style={thStyle}>Status</th>
                  <th style={thStyle}>Check</th>
                  <th style={thStyle}>Detail</th>
                  <th style={thStyle}>Checked (MT)</th>
                </tr>
              </thead>
              <tbody>
                {[...checks].sort((a, b) => CHECK_ORDER[normCheck(a.status)] - CHECK_ORDER[normCheck(b.status)]).map((c, i) => (
                  <tr key={`${c.name}-${i}`}>
                    <td style={tdStyle}><CheckPill status={c.status} /></td>
                    <td style={{ ...tdStyle, color: colors.heading }}><SafeText value={c.name} /></td>
                    <td style={{ ...tdStyle, color: colors.textMuted, maxWidth: 520, overflowWrap: 'anywhere' }}>{c.detail ? <SafeText value={c.detail} /> : ''}</td>
                    <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>{c.checked_at ? fmtTime(c.checked_at) : <Unknown hint="checked_at not reported" />}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </ScrollX>
        )}
      </Panel>

      <div style={{ ...cardGrid(420), marginBottom: 14 }}>
        {/* ── Pipeline by stage ─────────────────────────────────────── */}
        <Panel>
          <SectionHeader title="Pipeline runs by stage" icon={faDiagramProject} />
          {piv == null ? (
            <Unknown hint="pipeline not reported" />
          ) : piv.stages.length === 0 ? (
            <EmptyState icon={faDiagramProject} title="No stage runs yet" />
          ) : (
            <ScrollX>
              <table style={tableStyle}>
                <thead>
                  <tr>
                    <th style={thStyle}>Stage</th>
                    {piv.statuses.map(st => <th key={st} style={numTh}>{st}</th>)}
                  </tr>
                </thead>
                <tbody>
                  {piv.stages.map(stage => (
                    <tr key={stage} data-testid="pipeline-stage">
                      <td style={tdStyle}>{stage}</td>
                      {piv.statuses.map(st => {
                        const v = piv.count(stage, st)
                        const bad = st === 'failed' && v > 0
                        return <td key={st} style={{ ...numTd, color: bad ? colors.dangerText : numTd.color, fontWeight: bad ? 700 : 400 }}>{v.toLocaleString()}</td>
                      })}
                    </tr>
                  ))}
                </tbody>
                <tfoot>
                  <tr style={{ background: alpha(colors.indigo500, '0d') }}>
                    <td style={{ ...tdStyle, fontWeight: 700 }}>Total</td>
                    {piv.statuses.map(st => <td key={st} style={numTd}>{n0(statusTotal(st))}</td>)}
                  </tr>
                </tfoot>
              </table>
            </ScrollX>
          )}
        </Panel>

        {/* ── Releases per site ─────────────────────────────────────── */}
        <Panel>
          <SectionHeader title="Latest release per site" icon={faRocket} />
          {releases == null ? (
            <Unknown hint="releases not reported" />
          ) : releases.length === 0 ? (
            <EmptyState icon={faRocket} title="No sites reported" />
          ) : (
            <ScrollX>
              <table style={tableStyle}>
                <thead>
                  <tr>
                    <th style={thStyle}>Site</th>
                    <th style={thStyle}>Status</th>
                    <th style={thStyle}>Last at (MT)</th>
                    <th style={thStyle}>Error</th>
                  </tr>
                </thead>
                <tbody>
                  {[...releases]
                    .sort((a, b) => Number(Boolean(b.error)) - Number(Boolean(a.error)) || Number(Boolean(b.status)) - Number(Boolean(a.status)))
                    .map(r => {
                      const at = r.finished_at ?? r.started_at
                      return (
                        <tr key={r.site_id || r.domain} data-testid="release-row">
                          <td style={tdStyle}>{r.domain}</td>
                          <td style={tdStyle}>{r.status ? <Pill color={stateColor(r.status)} style={{ fontSize: 10, padding: '1px 8px' }}>{r.status}</Pill> : <span style={{ fontSize: 11, color: colors.textFaint }}>never released</span>}</td>
                          <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>{at ? fmtTime(at) : r.status ? <Unknown hint="no timestamp" /> : ''}</td>
                          <td style={{ ...tdStyle, color: colors.dangerText, maxWidth: 280, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={r.error ?? undefined}>
                            {r.error ? <SafeText value={r.error} /> : ''}
                          </td>
                        </tr>
                      )
                    })}
                </tbody>
              </table>
            </ScrollX>
          )}
        </Panel>
      </div>

      {/* ── Newsletter supply runway ─────────────────────────────────── */}
      <Panel>
        <SectionHeader title="Newsletter supply runway" icon={faBoxesStacked} right={<span style={{ fontSize: 11, color: colors.textFaint }}>least runway first · from the Mac-side supply_runway report</span>} />
        {consumers == null ? (
          <div style={{ fontSize: 12, color: colors.textMuted }}>
            <Unknown hint="supply.consumers is null in ops-status" /> — no supply_runway report received. Unknown, not zero.
            {o.newsletter_supply_runway_reason && <div style={{ marginTop: 4, color: colors.textFaint }}><SafeText value={o.newsletter_supply_runway_reason} /></div>}
          </div>
        ) : consumers.length === 0 ? (
          <EmptyState icon={faBoxesStacked} title="No newsletter consumers reported" />
        ) : (
          <ScrollX>
            <table style={tableStyle}>
              <thead>
                <tr>
                  <th style={thStyle}>Consumer</th>
                  <th style={thStyle}>Brand</th>
                  <th style={numTh} title="Articles currently eligible for this consumer">Eligible articles</th>
                  <th style={numTh} title="Days this consumer can run on the eligible supply">Runway (days)</th>
                  <th style={thStyle}>Status</th>
                </tr>
              </thead>
              <tbody>
                {[...consumers]
                  .sort((a, b) => (a.runway_days ?? Number.POSITIVE_INFINITY) - (b.runway_days ?? Number.POSITIVE_INFINITY))
                  .map((c, i) => (
                    <tr key={`${c.name ?? ''}-${i}`} data-testid="supply-consumer">
                      <td style={tdStyle}><SafeText value={c.name} /></td>
                      <td style={tdStyle}><SafeText value={c.brand} /></td>
                      <td style={numTd}>{n0(c.eligible)}</td>
                      <td style={numTd}>{n0(c.runway_days)}</td>
                      <td style={tdStyle}>{c.status ? <CheckPill status={normCheck(c.status) === 'unknown' ? undefined : c.status} label={c.status} /> : <Unknown />}</td>
                    </tr>
                  ))}
              </tbody>
            </table>
          </ScrollX>
        )}
      </Panel>
    </div>
  )
}

export default ContentDeskOps
