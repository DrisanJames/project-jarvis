// SupplyEcosystem.tsx — Pane 1 of the Supply tab (REQ-118 §6, WP10).
//
// The ecosystem queue: the estate strip on top, then ONE ROW PER LANE in the
// planner's frozen rank order, with the rank reason inline. This is the
// in-motion surface — what the mediators are doing right now — so it is the
// only pane that polls (60s, usePolling).
//
// Rules this pane enforces:
//   · health colour is the API's `health` field, never recomputed here;
//   · fill rate = committed ÷ desired (null when desired is 0 — the API
//     returns null and we render "unknown", never 100% or 0%);
//   · every numeric cell carries its label from the response's `labels` map;
//   · null = unknown, muted, never 0.

import React from 'react'
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome'
import { faLayerGroup, faRotate, faInbox } from '@fortawesome/free-solid-svg-icons'
import { colors, alpha, cardGrid } from '../shared/theme'
import { Panel, SectionHeader, Stat, SectionError, EmptyState, LivePill, PortalKeyframes, Pill } from '../shared/ui'
import { usePolling } from '../shared/usePolling'
import {
  EcosystemResponse, EcosystemLaneRow, HealthResponse,
  supplyGet, healthColor, Num, Unknown, LabelChip, HeaderStrip, LoadingRow, ScrollX,
  fmtUSD, fmtPct, fmtTime,
  tableStyle, thStyle, tdStyle, numTd, numTh,
} from './supplyShared'

interface Bundle {
  health: HealthResponse | null
  healthError: string | null
  ecosystem: EcosystemResponse | null
  ecosystemError: string | null
}

const POLL_MS = 60_000

export const SupplyEcosystem: React.FC<{
  day: string
  onSelectLane: (lane: string) => void
}> = ({ day, onSelectLane }) => {
  // Both halves are fetched independently and fail SOFT: /health needs no
  // contract key, /ecosystem does (503 when CONTRACT_TOKEN_KEY is unset), so
  // one can succeed while the other refuses. Blanking the estate strip because
  // the lane queue failed would hide the only numbers still measurable.
  const state = usePolling<Bundle>(
    React.useCallback(async (signal: AbortSignal): Promise<Bundle> => {
      const [h, e] = await Promise.allSettled([
        supplyGet<HealthResponse>('/health', { day }, signal),
        supplyGet<EcosystemResponse>('/ecosystem', { day }, signal),
      ])
      return {
        health: h.status === 'fulfilled' ? h.value : null,
        healthError: h.status === 'rejected' ? String(h.reason?.message ?? h.reason) : null,
        ecosystem: e.status === 'fulfilled' ? e.value : null,
        ecosystemError: e.status === 'rejected' ? String(e.reason?.message ?? e.reason) : null,
      }
    }, [day]),
    POLL_MS,
    [day],
  )

  const data = state.data
  const eco = data?.ecosystem ?? null
  const health = data?.health ?? null
  const labels = eco?.labels ?? {}
  const healthLabels = health?.labels ?? {}

  if (state.loading && !data) return <LoadingRow what="the ecosystem queue" />

  const lanes = eco?.lanes ?? []
  const shown = lanes.length

  // Column totals over the lanes actually shown. Summed only where every row
  // that has a value contributes — a null lane is EXCLUDED and the count of
  // contributing lanes rides in the title, so a total is never a silent zero.
  const total = (pick: (r: EcosystemLaneRow) => number | null | undefined) => {
    let sum = 0
    let n = 0
    lanes.forEach(r => { const v = pick(r); if (v != null) { sum += v; n += 1 } })
    return { sum: n > 0 ? sum : null, n }
  }

  return (
    <div>
      <PortalKeyframes />

      <HeaderStrip
        meta={health ?? eco}
        freshness={health?.freshness ?? null}
        extra={
          <>
            <LivePill live={state.live} agoSeconds={state.secondsSinceUpdate} />
            <span title="The planner freezes the day's ranks; the executor never re-ranks intraday.">
              plan frozen{' '}
              {eco?.plan_frozen_at
                ? <strong style={{ color: colors.heading }}>{fmtTime(eco.plan_frozen_at)}</strong>
                : <Unknown hint="no drip_daily_plan row carries frozen_at for this day — the planner may not have run" />}
            </span>
            <button
              type="button"
              onClick={state.refresh}
              style={{
                background: alpha(colors.indigo500, '22'), border: `1px solid ${alpha(colors.indigo500, '66')}`,
                color: colors.indigo200, borderRadius: 6, padding: '3px 9px', fontSize: 11,
                fontWeight: 600, cursor: 'pointer',
              }}
            >
              <FontAwesomeIcon icon={faRotate} /> Refresh
            </button>
          </>
        }
      />

      {state.error && <SectionError label="Supply refresh" error={state.error} onRetry={state.refresh} />}

      {/* ── Estate strip ──────────────────────────────────────────────── */}
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader
          title="Estate — capacity and demand for this day"
          icon={faLayerGroup}
          right={
            <span style={{ fontSize: 11, color: colors.textFaint }}>
              {health ? `${health.estate.domain_isp_cells} domain×ISP cells · ${health.estate.lane_isp_cells} lane×ISP cells` : ''}
            </span>
          }
        />
        {data?.healthError ? (
          <SectionError label="Estate strip" error={data.healthError} onRetry={state.refresh} />
        ) : (
          <div style={cardGrid(150)}>
            <Stat
              label="Contracted"
              title="Sum of drip_capacity_balance.contracted across every domain×ISP for this day — what the static domain contracts allow."
              value={<><Num value={health?.estate.contracted} label={healthLabels['estate.contracted']} what="Contracted messages, all domains × ISPs" /><LabelChip label={healthLabels['estate.contracted']} /></>}
            />
            <Stat
              label="Effective"
              title="Contracted after governors (throttle state, SES quota, health band, gmail hold) reduced it. Governors reduce, never raise."
              value={<><Num value={health?.estate.effective} label={healthLabels['estate.effective']} what="Effective messages after governors" /><LabelChip label={healthLabels['estate.effective']} /></>}
              color={colors.indigo200}
            />
            <Stat
              label="Reserved"
              title="Capacity held by live reservations that have not committed yet."
              value={<><Num value={health?.estate.reserved} label={healthLabels['estate.reserved']} what="Reserved messages" /><LabelChip label={healthLabels['estate.reserved']} /></>}
            />
            <Stat
              label="Committed"
              title="Capacity actually submitted to a transport (the ledger's committed column)."
              value={<><Num value={health?.estate.committed} label={healthLabels['estate.committed']} what="Committed messages" /><LabelChip label={healthLabels['estate.committed']} /></>}
              color={colors.success}
            />
            <Stat
              label="Desired"
              title="Sum of the lane side's desired daily intros for this day (drip_lane_balance.desired)."
              value={<><Num value={health?.estate.desired} label={healthLabels['estate.desired']} what="Desired intros, all lanes × ISPs" /><LabelChip label={healthLabels['estate.desired']} /></>}
            />
            <Stat
              label="Unfilled"
              title="Lane demand the planner and executor did not fill (drip_lane_balance.unfilled)."
              value={<><Num value={health?.estate.unfilled} label={healthLabels['estate.unfilled']} what="Unfilled lane demand" /><LabelChip label={healthLabels['estate.unfilled']} /></>}
              color={colors.warning}
            />
            <Stat
              label="EO spend today"
              title="Sum of total_cost on VALIDATION_ORDERED rows in the Supply Ledger for this Denver day. The ledger is append-only: no rows means a real $0.00, not unknown."
              value={<Num value={health?.estate.eo_spend_today_usd} label={healthLabels['estate.eo_spend_today']} format={v => fmtUSD(v)} what="EmailOversight validation spend today" />}
            />
            <Stat
              label="Stranded claims"
              title="partner_clean_queue rows claimed >48h ago with no campaign — the orphan shape the reaper releases. Null means the count timed out (the reap index may still be building), NOT zero."
              value={<Num value={health?.estate.stranded_claims} label={healthLabels['estate.stranded_claims']} what="Claimed rows with no campaign, older than 48h" unknownHint="the stranded-claim count timed out at 20s — idx_pcq_reap_orphans may not be built yet. Unknown, not zero." />}
              color={health?.estate.stranded_claims ? colors.danger : undefined}
            />
          </div>
        )}
      </Panel>

      {/* ── Lane queue ────────────────────────────────────────────────── */}
      <Panel>
        <SectionHeader
          title="Lane queue — frozen rank order"
          icon={faInbox}
          right={<span style={{ fontSize: 11, color: colors.textFaint }}>{shown} lane{shown === 1 ? '' : 's'} with an active dispatch contract</span>}
        />
        {data?.ecosystemError ? (
          <SectionError label="Lane queue" error={data.ecosystemError} onRetry={state.refresh} />
        ) : lanes.length === 0 ? (
          <EmptyState
            title="No lanes in the queue"
            hint="A lane appears here only with an ACTIVE dispatch contract for this day. A lane with no contract is skipped by the executor (skipped:no_contract) — it is not merely quiet."
          />
        ) : (
          <ScrollX>
            <table style={tableStyle}>
              <thead>
                <tr>
                  <th style={thStyle} title="The planner's frozen rank. Lower mails first.">Rank</th>
                  <th style={thStyle}>Lane</th>
                  <th style={thStyle} title="Health is the API's own verdict (red: two consecutive zero/failed ticks with demand · amber: fill rate &lt; 80% · green: otherwise · grey: paused). The UI does not recompute it.">Health</th>
                  <th style={numTh} title="Mature dispatch-contribution eCPM = (revenue − send cost) ÷ messages × 1000, over the ≥7-day-old cohort.">Contribution eCPM</th>
                  <th style={numTh} title="Desired daily intros from the dispatch contract, summed over ISPs.">Desired</th>
                  <th style={numTh} title="Planner award backed by mailable supply now.">Award firm</th>
                  <th style={numTh} title="Planner award the supply controller must still deliver before need time.">Award prov.</th>
                  <th style={numTh} title="Ladder follow-ups due today for this lane. Follow-ups are obligations — reserved before discretionary intros.">Follow-ups due</th>
                  <th style={numTh} title="pcq rows ready, never touched, validated inside the inventory contract's verdict window.">Fresh mailable</th>
                  <th style={numTh} title="Records out for EmailOversight validation (pending_eo / eo_in_flight). Forecast, not measured.">Pending EO</th>
                  <th style={numTh} title="Previously mailed records eligible for a remail under the inventory contract.">Remail elig.</th>
                  <th style={numTh} title="Records ordered for validation today, with today's EO spend for this lane in the tooltip.">Clean ordered</th>
                  <th style={numTh} title="Messages this lane sent today.">Sent today</th>
                  <th style={numTh} title="Capacity held by live reservations for this lane.">Reserved</th>
                  <th style={numTh} title="Capacity committed to a transport for this lane.">Committed</th>
                  <th style={numTh} title="Fill rate = committed ÷ desired. Null (unknown) when desired is 0 — a lane that wants nothing has no fill rate.">Fill rate</th>
                  <th style={thStyle} title="Which term of the reservation minimum bound the grant (domain_tokens | lane_demand | supply | plan_share | governor:… | outside_window).">Binding</th>
                </tr>
              </thead>
              <tbody>
                {lanes.map(row => (
                  <LaneRow key={row.lane} row={row} labels={labels} onSelect={() => onSelectLane(row.lane)} />
                ))}
                <tr>
                  <td style={{ ...tdStyle, fontWeight: 700, color: colors.textMuted }} colSpan={4}>
                    Σ over the {shown} lane{shown === 1 ? '' : 's'} shown
                  </td>
                  {([
                    (r: EcosystemLaneRow) => r.demand.desired,
                    (r: EcosystemLaneRow) => r.demand.awarded_firm,
                    (r: EcosystemLaneRow) => r.demand.awarded_provisional,
                    (r: EcosystemLaneRow) => r.followups_due,
                    (r: EcosystemLaneRow) => r.fresh_mailable,
                    (r: EcosystemLaneRow) => r.pending_eo,
                    (r: EcosystemLaneRow) => r.remail_eligible,
                    (r: EcosystemLaneRow) => r.clean_ordered_today,
                    (r: EcosystemLaneRow) => r.sent_today,
                    (r: EcosystemLaneRow) => r.reserved,
                    (r: EcosystemLaneRow) => r.committed,
                  ]).map((pick, i) => {
                    const t = total(pick)
                    return (
                      <td key={i} style={{ ...numTd, fontWeight: 700 }}>
                        <Num
                          value={t.sum}
                          what={`summed over the ${t.n} of ${shown} lanes that reported a value; lanes reporting unknown are excluded`}
                          unknownHint="no lane reported a value for this column — unknown, not zero"
                        />
                      </td>
                    )
                  })}
                  <td style={numTd} />
                  <td style={tdStyle} />
                </tr>
              </tbody>
            </table>
          </ScrollX>
        )}
      </Panel>
    </div>
  )
}

const LaneRow: React.FC<{
  row: EcosystemLaneRow
  labels: Record<string, string>
  onSelect: () => void
}> = ({ row, labels, onSelect }) => {
  const [hover, setHover] = React.useState(false)
  const hc = healthColor(row.health)
  const dv = row.dispatch_value
  return (
    <tr
      onClick={onSelect}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      style={{ cursor: 'pointer', background: hover ? colors.hover : undefined }}
      title="Open this lane's detail pane"
    >
      <td style={{ ...tdStyle, textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>
        <Num value={row.rank} label={labels['rank']} what="Frozen planner rank" unknownHint="this lane has no drip_daily_plan row for the day — the planner did not rank it" />
      </td>
      <td style={tdStyle}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 6, flexWrap: 'wrap' }}>
          <strong style={{ color: colors.heading }}>{row.lane}</strong>
          <span
            title={`Operator priority tier ${row.tier} (1 = first, 3 = last, 9 = test/exploration). Contract policy.`}
            style={{
              fontSize: 10, fontWeight: 700, color: colors.indigo300,
              border: `1px solid ${alpha(colors.indigo500, '44')}`, borderRadius: 4, padding: '0 5px',
            }}
          >
            tier {row.tier}
          </span>
          {row.exploration && <Pill color={colors.info} style={{ fontSize: 9, padding: '1px 7px' }}>exploration</Pill>}
          {row.paused && <Pill color={colors.idle} style={{ fontSize: 9, padding: '1px 7px' }}>paused</Pill>}
          <span style={{ fontSize: 10, color: colors.textFaint }} title="The dispatch contract version this row was projected against.">
            dispatch v{row.dispatch_contract_version}
            {row.inventory_contract_version ? ` · inventory v${row.inventory_contract_version}` : ''}
          </span>
        </div>
        {row.rank_reason && (
          <div style={{ fontSize: 10, color: colors.textFaint, marginTop: 2 }} title={row.rank_reason}>
            {row.rank_reason}
          </div>
        )}
      </td>
      <td style={tdStyle}>
        <span
          title={row.health_reason || `health = ${row.health} (from the API; the UI does not recompute the rule)`}
          style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}
        >
          <span style={{ width: 9, height: 9, borderRadius: 999, background: hc, flexShrink: 0 }} />
          <span style={{ color: hc, fontSize: 11, fontWeight: 700, textTransform: 'uppercase' }}>{row.health}</span>
        </span>
        {row.health_reason && <div style={{ fontSize: 10, color: colors.textFaint, marginTop: 1 }}>{row.health_reason}</div>}
      </td>
      <td style={numTd}>
        <Num
          value={dv.contribution_ecpm}
          label={labels['dispatch_value']}
          format={v => fmtUSD(v)}
          what={`(revenue − send cost) ÷ messages × 1000 over ${dv.messages?.toLocaleString() ?? 'unknown'} messages / ${dv.conversions?.toLocaleString() ?? 'unknown'} conversions`}
          unknownHint="no economics row for this lane — unknown, not $0.00"
        />
        <div style={{ fontSize: 10, color: dv.maturity === 'mature' ? colors.textFaint : colors.warningText }}>
          {dv.maturity}{dv.inherited ? ' · estate median (inherited)' : ''}
        </div>
      </td>
      <td style={numTd}><Num value={row.demand.desired} label={labels['demand.desired']} what="Desired daily intros (dispatch contract)" /></td>
      <td style={numTd}><Num value={row.demand.awarded_firm} label={labels['demand.awarded_firm']} what="Planner award backed by mailable supply" /></td>
      <td style={numTd}>
        <Num value={row.demand.awarded_provisional} label={labels['demand.awarded_provisional']} what="Planner award awaiting supply" />
        {row.demand.unserved_reason && <Reasonette text={`unserved: ${row.demand.unserved_reason}`} />}
      </td>
      <td style={numTd}><Num value={row.followups_due} label={labels['followups_due']} what="Ladder follow-ups due today" /></td>
      <td style={numTd}><Num value={row.fresh_mailable} label={labels['fresh_mailable']} what="Fresh, validated, never-touched records" /></td>
      <td style={numTd}><Num value={row.pending_eo} label={labels['pending_eo']} what="Records out for validation (forecast, not measured)" /></td>
      <td style={numTd}><Num value={row.remail_eligible} label={labels['remail_eligible']} what="Remail-eligible records under the inventory contract" /></td>
      <td style={numTd}>
        <Num
          value={row.clean_ordered_today}
          label={labels['clean_ordered_today']}
          what={`Records ordered for validation today · spend ${row.clean_cost_today_usd == null ? 'unknown' : fmtUSD(row.clean_cost_today_usd)}`}
        />
      </td>
      <td style={numTd}><Num value={row.sent_today} label={labels['sent_today']} what="Messages sent by this lane today" /></td>
      <td style={numTd}><Num value={row.reserved} label={labels['reserved']} what="Capacity held by live reservations" /></td>
      <td style={numTd}><Num value={row.committed} label={labels['committed']} what="Capacity committed to a transport" /></td>
      <td style={numTd}>
        <Num
          value={row.fill_rate}
          label={labels['fill_rate']}
          format={v => fmtPct(v)}
          what="committed ÷ desired"
          unknownHint="fill rate is undefined here: desired is 0 (the lane wants nothing) or was not measured. Not 0%."
          color={row.fill_rate != null && row.fill_rate < 0.8 ? colors.warningText : undefined}
        />
      </td>
      <td style={{ ...tdStyle, fontSize: 11, color: colors.textMuted }} title="Which term of the reservation minimum bound the grant.">
        {row.binding_constraint || <span style={{ color: colors.textFaint }}>—</span>}
      </td>
    </tr>
  )
}

const Reasonette: React.FC<{ text: string }> = ({ text }) => (
  <div style={{ fontSize: 10, color: colors.textFaint, fontVariantNumeric: 'normal' }} title={text}>{text}</div>
)

export default SupplyEcosystem
