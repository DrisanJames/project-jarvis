// SupplyLane.tsx — Pane 2 of the Supply tab (REQ-118 §6, WP10).
//
// One lane, end to end: the five demand numbers per ISP, the capacity ledger by
// domain×ISP (contract vs effective + reason), the supply ledger by source×ISP,
// the three eCPMs with their maturity label, the last 24h of tick outcomes with
// reason text, the dispatch + inventory contracts with version history and
// "schedule for tomorrow", and manual revenue entry.
//
// This pane does NOT poll: it is the study surface. The ecosystem pane is the
// in-motion one (60s). A manual Refresh is always available.

import React from 'react'
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome'
import {
  faArrowLeft, faRotate, faScaleBalanced, faTruckRampBox, faSackDollar,
  faStopwatch, faFileContract, faPenToSquare,
} from '@fortawesome/free-solid-svg-icons'
import { colors, alpha } from '../shared/theme'
import { Panel, SectionHeader, SectionError, EmptyState, Pill } from '../shared/ui'
import { useToast } from '../shared/ToastSystem'
import {
  LaneResponse, EcosystemLaneRow, SupplyError,
  supplyGet, supplyPost, Num, LabelChip, HeaderStrip, LoadingRow, ScrollX, Reason,
  fmtUSD, fmtTime, healthColor,
  tableStyle, thStyle, tdStyle, numTd, numTh,
} from './supplyShared'
import { SupplyContractForm } from './SupplyContractForm'
import { SupplyLedger } from './SupplyLedger'

// The Supply Ledger's event vocabulary (§1.2 drip_supply_ledger.event), in the
// order a record travels. Rendering them in a fixed order keeps the source×ISP
// table comparable row to row; an event the API returns that is not in this
// list is appended so a new event type is never silently dropped.
const SUPPLY_EVENTS = [
  'RECEIVED', 'PRECHECK_PASSED', 'SUPPRESSED', 'INTERNAL_INVALID',
  'VALIDATION_ORDERED', 'VALIDATION_VALID', 'VALIDATION_INVALID', 'VALIDATION_NO_VERDICT',
  'MAILABLE', 'REMAIL_ELIGIBLE', 'RESERVED_FOR_INTRO', 'CONSUMED', 'EXPIRED', 'RELEASED',
]

const OUTCOME_COLOR: Record<string, string> = {
  fired: colors.success,
  skipped: colors.idle,
  zero: colors.warning,
  failed: colors.danger,
}

export const SupplyLane: React.FC<{
  day: string
  lane: string | null
  lanes: EcosystemLaneRow[]
  onSelectLane: (lane: string) => void
  onBack: () => void
}> = ({ day, lane, lanes, onSelectLane, onBack }) => {
  const [data, setData] = React.useState<LaneResponse | null>(null)
  const [loading, setLoading] = React.useState(false)
  const [error, setError] = React.useState<string | null>(null)
  const [nonce, setNonce] = React.useState(0)

  React.useEffect(() => {
    if (!lane) { setData(null); setError(null); return }
    const ctrl = new AbortController()
    setLoading(true)
    supplyGet<LaneResponse>(`/lanes/${encodeURIComponent(lane)}`, { day }, ctrl.signal)
      .then(d => { setData(d); setError(null) })
      .catch(e => { if (!ctrl.signal.aborted) setError(e instanceof Error ? e.message : String(e)) })
      .finally(() => { if (!ctrl.signal.aborted) setLoading(false) })
    return () => ctrl.abort()
  }, [lane, day, nonce])

  // No lane selected: pick one. Selection is the drill-down (a lane is chosen by
  // clicking it in the queue), so this list is the same choice by another route
  // — not a second, forked filter control.
  if (!lane) {
    return (
      <Panel>
        <SectionHeader title="Pick a lane" icon={faScaleBalanced} />
        {lanes.length === 0 ? (
          <EmptyState
            title="No lane selected"
            hint="Open the Ecosystem pane and click a lane row, or reload the queue — a lane appears only with an active dispatch contract."
          />
        ) : (
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
            {lanes.map(l => (
              <button
                key={l.lane}
                type="button"
                onClick={() => onSelectLane(l.lane)}
                title={l.health_reason || `health = ${l.health}`}
                style={{
                  display: 'inline-flex', alignItems: 'center', gap: 7,
                  background: alpha(colors.indigo500, '14'),
                  border: `1px solid ${alpha(colors.indigo500, '44')}`,
                  color: colors.text, borderRadius: 999, padding: '5px 12px',
                  fontSize: 12, cursor: 'pointer',
                }}
              >
                <span style={{ width: 8, height: 8, borderRadius: 999, background: healthColor(l.health) }} />
                {l.lane}
                <span style={{ color: colors.textFaint, fontSize: 10 }}>tier {l.tier}</span>
              </button>
            ))}
          </div>
        )}
      </Panel>
    )
  }

  if (loading && !data) return <LoadingRow what={`lane ${lane}`} />

  const labels = data?.labels ?? {}

  return (
    <div>
      <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 10, flexWrap: 'wrap' }}>
        <button
          type="button"
          onClick={onBack}
          style={{
            background: 'transparent', border: `1px solid ${colors.panelBorderStrong}`,
            color: colors.textMuted, borderRadius: 6, padding: '4px 10px', fontSize: 12, cursor: 'pointer',
          }}
        >
          <FontAwesomeIcon icon={faArrowLeft} /> Ecosystem queue
        </button>
        <h2 style={{ margin: 0, fontSize: 18, color: colors.heading }}>{lane}</h2>
        {data && (
          <>
            <Pill color={colors.indigo400} style={{ fontSize: 10 }}>tier {data.tier}</Pill>
            {data.paused && <Pill color={colors.idle} style={{ fontSize: 10 }}>paused</Pill>}
          </>
        )}
        <button
          type="button"
          onClick={() => setNonce(n => n + 1)}
          style={{
            background: alpha(colors.indigo500, '22'), border: `1px solid ${alpha(colors.indigo500, '66')}`,
            color: colors.indigo200, borderRadius: 6, padding: '4px 10px', fontSize: 11, fontWeight: 600, cursor: 'pointer',
          }}
        >
          <FontAwesomeIcon icon={faRotate} /> Refresh
        </button>
      </div>

      <HeaderStrip meta={data} />

      {error && <SectionError label={`Lane ${lane}`} error={error} onRetry={() => setNonce(n => n + 1)} />}

      {data && (
        <>
          {/* ── Demand: the five numbers per ISP ─────────────────────── */}
          <Panel style={{ marginBottom: 14 }}>
            <SectionHeader
              title="Demand by ISP — desired · firm · provisional · supply-backed · unserved"
              icon={faScaleBalanced}
              right={<DispatchValueChip data={data} />}
            />
            {data.demand_by_isp.length === 0 ? (
              <EmptyState title="No demand rows" hint="No drip_lane_balance or drip_daily_plan row exists for this lane and day — the planner may not have run. This is not the same as a lane that wants nothing." />
            ) : (
              <ScrollX>
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={thStyle}>ISP</th>
                      <th style={numTh} title="Desired daily intros from the dispatch contract (contracted).">Desired</th>
                      <th style={numTh} title="Award the planner could back with mailable supply now (planned).">Award firm</th>
                      <th style={numTh} title="Award the supply controller must still deliver before need time (planned).">Award prov.</th>
                      <th style={numTh} title="Award actually backed by supply = firm + the provisional part supply can plausibly cover (planned).">Supply-backed</th>
                      <th style={numTh} title="desired − firm − provisional, with the planner's recorded reason (planned).">Unserved</th>
                      <th style={numTh} title="Due follow-ups reserved before any discretionary intro (planned).">Follow-ups reserved</th>
                      <th style={numTh} title="Capacity held by live reservations for this lane×ISP (reserved).">Reserved</th>
                      <th style={numTh} title="Capacity committed to a transport (actual).">Committed</th>
                      <th style={numTh} title="pcq rows ready, never touched, validated inside the verdict window (actual).">Fresh mailable</th>
                      <th style={numTh} title="Ladder follow-ups whose next_touch_at falls in this day (actual).">Follow-ups due</th>
                      <th style={numTh} title="Records out for validation — forecast, not measured.">Pending EO</th>
                    </tr>
                  </thead>
                  <tbody>
                    {data.demand_by_isp.map(d => (
                      <tr key={d.isp} style={d.excluded ? { opacity: 0.55 } : undefined}>
                        <td style={tdStyle}>
                          <strong style={{ color: colors.heading }}>{d.isp}</strong>
                          {d.excluded && (
                            <div style={{ fontSize: 10, color: colors.warningText }} title="This ISP is in the dispatch contract's isp_exclusions — it receives no intros and no EO order.">
                              excluded by contract
                            </div>
                          )}
                        </td>
                        <td style={numTd}><Num value={d.desired} label={labels['demand_by_isp.desired']} what="Desired daily intros" /></td>
                        <td style={numTd}><Num value={d.awarded_firm} label={labels['demand_by_isp.awarded_firm']} what="Firm planner award" /></td>
                        <td style={numTd}><Num value={d.awarded_provisional} label={labels['demand_by_isp.awarded_provisional']} what="Provisional planner award" /></td>
                        <td style={numTd}><Num value={d.supply_backed} label={labels['demand_by_isp.supply_backed']} what="Award backed by supply" /></td>
                        <td style={numTd}>
                          <Num value={d.unserved} label={labels['demand_by_isp.unserved']} what="desired − firm − provisional" color={d.unserved ? colors.warningText : undefined} />
                          <Reason text={d.unserved_reason} />
                        </td>
                        <td style={numTd}><Num value={d.followups_reserved} label={labels['demand_by_isp.followups_reserved']} what="Follow-ups reserved first" /></td>
                        <td style={numTd}><Num value={d.reserved} label={labels['demand_by_isp.reserved']} what="Held by live reservations" /></td>
                        <td style={numTd}><Num value={d.committed} label={labels['demand_by_isp.committed']} what="Committed to a transport" /></td>
                        <td style={numTd}><Num value={d.fresh_mailable} label={labels['demand_by_isp.fresh_mailable']} what="Fresh mailable records" /></td>
                        <td style={numTd}><Num value={d.followups_due} label={labels['demand_by_isp.followups_due']} what="Follow-ups due today" /></td>
                        <td style={numTd}><Num value={d.pending_eo} label={labels['demand_by_isp.pending_eo']} what="Records out for validation" /></td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </ScrollX>
            )}
          </Panel>

          {/* ── Capacity ledger by domain×ISP ────────────────────────── */}
          <Panel style={{ marginBottom: 14 }}>
            <SectionHeader title="Capacity by sending domain × ISP — contract vs effective" icon={faTruckRampBox} />
            {data.capacity_by_domain_isp.length === 0 ? (
              <EmptyState title="No capacity cells" hint="No drip_capacity_balance row for any of this lane's allowed domains on this day — capacity is unknown for the lane, not zero." />
            ) : (
              <ScrollX maxHeight={420}>
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={thStyle}>Sending domain</th>
                      <th style={thStyle}>ISP</th>
                      <th style={numTh} title="The domain contract's daily_max_by_isp value (contracted).">Contract</th>
                      <th style={numTh} title="Contracted after governors reduced it (effective). The reason names the binding governor.">Effective</th>
                      <th style={numTh} title="The planner's award for this lane on this domain×ISP (planned).">Planned</th>
                      <th style={numTh} title="Held by live reservations (reserved).">Reserved</th>
                      <th style={numTh} title="Submitted to a transport (actual).">Submitted</th>
                      <th style={numTh} title="effective − reserved − committed, floored at 0 (effective).">Remaining</th>
                      <th style={numTh} title="Token-bucket balance right now: refill = effective ÷ active intervals, capped at max_burst_intervals (effective).">Tokens</th>
                      <th style={thStyle} title="Why this cell cannot grant right now, when it cannot.">Blocked</th>
                    </tr>
                  </thead>
                  <tbody>
                    {data.capacity_by_domain_isp.map(c => (
                      <tr key={`${c.sending_domain}|${c.isp}`}>
                        <td style={tdStyle}><strong style={{ color: colors.heading }}>{c.sending_domain}</strong></td>
                        <td style={tdStyle}>{c.isp}</td>
                        <td style={numTd}><Num value={c.contracted} label={labels['capacity.contracted']} what="Domain contract daily max for this ISP" /></td>
                        <td style={numTd}>
                          <Num
                            value={c.effective}
                            label={labels['capacity.effective']}
                            what="Contracted after governors"
                            color={c.effective != null && c.contracted != null && c.effective < c.contracted ? colors.warningText : undefined}
                          />
                          <Reason text={c.effective_reason} />
                        </td>
                        <td style={numTd}><Num value={c.planned} label={labels['capacity.planned']} what="Planner award for this lane here" /></td>
                        <td style={numTd}><Num value={c.reserved} label={labels['capacity.reserved']} what="Held by live reservations" /></td>
                        <td style={numTd}><Num value={c.submitted} label={labels['capacity.submitted']} what="Submitted to a transport" /></td>
                        <td style={numTd}><Num value={c.remaining} label={labels['capacity.remaining']} what="effective − reserved − committed" /></td>
                        <td style={numTd}><Num value={c.tokens} label={labels['capacity.tokens']} format={v => v.toFixed(1)} what="Token-bucket balance" /></td>
                        <td style={{ ...tdStyle, fontSize: 11, color: c.blocked_reason ? colors.warningText : colors.textFaint }}>
                          {c.blocked_reason || '—'}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </ScrollX>
            )}
          </Panel>

          {/* ── Supply ledger by source×ISP ──────────────────────────── */}
          <Panel style={{ marginBottom: 14 }}>
            <SectionHeader
              title="Supply by source × ISP — the day's ledger, in records"
              icon={faTruckRampBox}
              right={<span style={{ fontSize: 11, color: colors.textFaint }}>Units: unique RECORDS (the capacity side counts MESSAGES)</span>}
            />
            {data.supply_by_source_isp.length === 0 ? (
              <EmptyState title="No supply-ledger rows today" hint="The Supply Ledger is append-only: no row means nothing arrived, was ordered or was consumed for this lane today — a measured nothing, not an unknown." />
            ) : (
              <SupplyBySourceTable rows={data.supply_by_source_isp} label={labels['supply_by_source_isp.events']} />
            )}
          </Panel>

          {/* ── Economics ────────────────────────────────────────────── */}
          <Panel style={{ marginBottom: 14 }}>
            <SectionHeader
              title="Economics by ISP — three eCPMs, each with its maturity"
              icon={faSackDollar}
              right={<span style={{ fontSize: 11, color: colors.textFaint }}>7-day attribution · rank uses the ≥7-day-old cohort only</span>}
            />
            {data.economics_by_isp.length === 0 ? (
              <EmptyState title="No economics rows" hint="drip_lane_economics has no row for this lane and day — revenue and cost are unknown, not zero." />
            ) : (
              <ScrollX>
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={thStyle}>ISP</th>
                      <th style={numTh} title="Messages in the cohort (the denominator of every eCPM below).">Messages</th>
                      <th style={numTh} title="Conversions counted UNJOINED (three mailing_offers rows share EF 162 and would fan a join ×3).">Conversions</th>
                      <th style={numTh} title="Everflow payout attributed by campaign_id + drip_manual_revenue.">Revenue</th>
                      <th style={numTh} title="Gross eCPM = revenue ÷ messages × 1000.">Gross eCPM</th>
                      <th style={numTh} title="Dispatch contribution eCPM = (revenue − send cost) ÷ messages × 1000. EO cost is SUNK here — this is the ranking number for already-mailable inventory.">Contribution eCPM</th>
                      <th style={numTh} title="Fully loaded net eCPM = (revenue − send − EO − acquisition − infra share) ÷ messages × 1000. Reporting only; infra share is NULL until the operator supplies the monthly figures.">Fully loaded eCPM</th>
                      <th style={numTh} title="Expected revenue per raw record − acquisition − expected EO ÷ yield − expected send cost over the ladder. Drives which lane gets EO spend first.">Cleaning value</th>
                      <th style={thStyle} title="mature = the ≥7-day-old cohort · incomplete = the window has not closed · unknown = no data. Only a mature number ranks.">Maturity</th>
                    </tr>
                  </thead>
                  <tbody>
                    {data.economics_by_isp.map(e => (
                      <tr key={e.isp}>
                        <td style={tdStyle}><strong style={{ color: colors.heading }}>{e.isp}</strong></td>
                        <td style={numTd}><Num value={e.messages} label={labels['economics_by_isp']} what="Messages in the cohort" /></td>
                        <td style={numTd}><Num value={e.conversions} label={labels['economics_by_isp']} what="Conversions (unjoined)" /></td>
                        <td style={numTd}><Num value={e.revenue_usd} label={labels['economics_by_isp']} format={v => fmtUSD(v)} what="Attributed revenue" /></td>
                        <td style={numTd}><Num value={e.gross_ecpm} label={labels['economics_by_isp']} format={v => fmtUSD(v)} what="revenue ÷ messages × 1000" /></td>
                        <td style={numTd}><Num value={e.contribution_ecpm} label={labels['economics_by_isp']} format={v => fmtUSD(v)} what="(revenue − send cost) ÷ messages × 1000" /></td>
                        <td style={numTd}><Num value={e.fully_loaded_ecpm} label={labels['economics_by_isp']} format={v => fmtUSD(v)} what="(revenue − send − EO − acquisition − infra) ÷ messages × 1000" unknownHint="unknown — infra share is NULL until OVH + IPXO monthly figures are supplied (§4)" /></td>
                        <td style={numTd}><Num value={e.cleaning_value} label={labels['economics_by_isp']} format={v => fmtUSD(v, 4)} what="Expected value of cleaning one raw record" /></td>
                        <td style={tdStyle}>
                          <span style={{ color: e.maturity === 'mature' ? colors.successText : colors.warningText, fontSize: 11, fontWeight: 600 }}>
                            {e.maturity}
                          </span>
                          {!e.sample_ok && (
                            <div style={{ fontSize: 10, color: colors.textFaint }} title="Below the minimum sample for a rank (20k messages or 5 conversions) — the lane inherits the estate median for its record class.">
                              below min sample · inherits estate median
                            </div>
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </ScrollX>
            )}
          </Panel>

          {/* ── Tick outcomes ────────────────────────────────────────── */}
          <Panel style={{ marginBottom: 14 }}>
            <SectionHeader
              title="Tick outcomes — last 24 hours"
              icon={faStopwatch}
              right={<span style={{ fontSize: 11, color: colors.textFaint }}>Every tick records fired / skipped / zero / failed with a reason. Nothing is silent.</span>}
            />
            {data.tick_outcomes_24h.length === 0 ? (
              <EmptyState title="No tick outcomes in 24h" hint="The executor writes one outcome row per tick per lane per pass. No rows means the executor did not run this lane at all — investigate rather than read it as quiet." />
            ) : (
              <ScrollX maxHeight={320}>
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={thStyle}>Tick (Denver)</th>
                      <th style={thStyle}>Pass</th>
                      <th style={thStyle}>Outcome</th>
                      <th style={numTh} title="Records claimed on this tick (actual).">Claimed</th>
                      <th style={thStyle}>Reason</th>
                      <th style={thStyle} title="The caps the executor saw on this tick (caps_seen). Hover for the raw JSON.">Caps seen</th>
                      <th style={thStyle}>Campaign</th>
                    </tr>
                  </thead>
                  <tbody>
                    {data.tick_outcomes_24h.map((t, i) => {
                      const caps = t.caps_seen == null ? '' : JSON.stringify(t.caps_seen)
                      return (
                        <tr key={`${t.tick}|${t.pass}|${i}`}>
                          <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>{fmtTime(t.tick)}</td>
                          <td style={tdStyle}>{t.pass}</td>
                          <td style={tdStyle}>
                            <span style={{ color: OUTCOME_COLOR[t.outcome] ?? colors.textMuted, fontWeight: 700, fontSize: 11, textTransform: 'uppercase' }}>
                              {t.outcome}
                            </span>
                          </td>
                          <td style={numTd}><Num value={t.claimed} label={labels['tick_outcomes_24h']} what="Records claimed on this tick" /></td>
                          <td style={{ ...tdStyle, fontSize: 11, color: colors.textMuted, maxWidth: 340 }} title={t.reason}>{t.reason || '—'}</td>
                          <td style={{ ...tdStyle, fontSize: 10, color: colors.textFaint, maxWidth: 220, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={caps || 'no caps recorded on this tick'}>
                            {caps || '—'}
                          </td>
                          <td style={{ ...tdStyle, fontFamily: 'monospace', fontSize: 10 }} title={t.campaign_id ?? 'no campaign — this tick deployed nothing'}>
                            {t.campaign_id ? `${t.campaign_id.slice(0, 8)}…` : '—'}
                          </td>
                        </tr>
                      )
                    })}
                  </tbody>
                </table>
              </ScrollX>
            )}
          </Panel>

          {/* ── Contracts ────────────────────────────────────────────── */}
          <Panel style={{ marginBottom: 14 }}>
            <SectionHeader title="Contracts — dispatch and inventory" icon={faFileContract} />
            <div style={{ display: 'grid', gap: 14 }}>
              <SupplyContractForm
                kind="dispatch"
                subject={lane}
                seedISPValues={Object.fromEntries(data.demand_by_isp.map(d => [d.isp, d.desired]))}
                seedISPProvenance="desired_daily_intros from the day's active dispatch contract, as projected into drip_lane_balance.desired"
                onChanged={() => setNonce(n => n + 1)}
              />
              <SupplyContractForm
                kind="inventory"
                subject={lane}
                onChanged={() => setNonce(n => n + 1)}
              />
            </div>
          </Panel>

          {/* ── Manual revenue ───────────────────────────────────────── */}
          <ManualRevenuePanel lane={lane} onSaved={() => setNonce(n => n + 1)} />

          {/* ── Drill-through to the raw ledgers ─────────────────────── */}
          <SupplyLedger day={day} lane={lane} title={`Ledger drill-through — ${lane}`} />
        </>
      )}
    </div>
  )
}

const DispatchValueChip: React.FC<{ data: LaneResponse }> = ({ data }) => {
  const dv = data.dispatch_value
  return (
    <span style={{ fontSize: 11, color: colors.textMuted }} title="The lane's ranking value: mature dispatch-contribution eCPM. `inherited` means the estate median for the record class was used because the lane's own sample was too small.">
      contribution eCPM{' '}
      <strong style={{ color: colors.heading }}>
        <Num value={dv.contribution_ecpm} format={v => fmtUSD(v)} label="actual" what="(revenue − send cost) ÷ messages × 1000" />
      </strong>
      <LabelChip label="actual" />
      <span style={{ marginLeft: 8, color: dv.maturity === 'mature' ? colors.textFaint : colors.warningText }}>
        {dv.maturity}{dv.inherited ? ' · inherited estate median' : ''}
      </span>
    </span>
  )
}

const SupplyBySourceTable: React.FC<{ rows: LaneResponse['supply_by_source_isp']; label: string | undefined }> = ({ rows, label }) => {
  // Columns = the fixed event order plus anything new the API sent, so a newly
  // added event type shows up rather than disappearing.
  const seen = new Set<string>()
  rows.forEach(r => Object.keys(r.events).forEach(k => seen.add(k)))
  const extra = [...seen].filter(k => !SUPPLY_EVENTS.includes(k)).sort()
  const cols = [...SUPPLY_EVENTS.filter(e => seen.has(e)), ...extra]
  return (
    <ScrollX maxHeight={420}>
      <table style={tableStyle}>
        <thead>
          <tr>
            <th style={thStyle}>Source</th>
            <th style={thStyle}>ISP</th>
            {cols.map(c => (
              <th key={c} style={numTh} title={`Supply Ledger event ${c} — records, summed over this Denver day (actual).`}>
                {c.toLowerCase().replace(/_/g, ' ')}
              </th>
            ))}
            <th style={numTh} title="Ledger cost for this source×ISP today (unit_cost × quantity).">Cost</th>
          </tr>
        </thead>
        <tbody>
          {rows.map(r => (
            <tr key={`${r.source_slug}|${r.isp}`}>
              <td style={tdStyle}><strong style={{ color: colors.heading }}>{r.source_slug}</strong></td>
              <td style={tdStyle}>{r.isp}</td>
              {cols.map(c => (
                <td key={c} style={numTd}>
                  {/* An absent event key is a measured nothing in an append-only
                      ledger, so it renders as 0 — with the reason in the title. */}
                  <Num
                    value={r.events[c] ?? 0}
                    label={label}
                    what={r.events[c] == null
                      ? `no ${c} row for this source×ISP today — the Supply Ledger is append-only, so this is a measured zero`
                      : `${c} records today`}
                    color={r.events[c] == null ? colors.textFaint : undefined}
                  />
                </td>
              ))}
              <td style={numTd}><Num value={r.cost_usd} label={label} format={v => fmtUSD(v, 4)} what="Ledger cost today" /></td>
            </tr>
          ))}
        </tbody>
      </table>
    </ScrollX>
  )
}

// ─────────────────────────────────────────────────────────────────────────────
// Manual revenue — the audited entry for lanes whose revenue is not in Everflow
// ─────────────────────────────────────────────────────────────────────────────

const inputStyle: React.CSSProperties = {
  background: colors.appBgSolid, color: colors.text,
  border: `1px solid ${colors.panelBorderStrong}`, borderRadius: 6,
  padding: '6px 9px', fontSize: 12, outline: 'none',
}

const fieldLabelStyle: React.CSSProperties = {
  display: 'flex', flexDirection: 'column', gap: 4,
  fontSize: 10, color: colors.textFaint, textTransform: 'uppercase', letterSpacing: 0.5,
}

const ManualRevenuePanel: React.FC<{ lane: string; onSaved: () => void }> = ({ lane, onSaved }) => {
  const toast = useToast()
  const [open, setOpen] = React.useState(false)
  const [busy, setBusy] = React.useState(false)
  const [err, setErr] = React.useState<string | null>(null)
  const [badFields, setBadFields] = React.useState<string[]>([])
  const [form, setForm] = React.useState({
    revenue_date: '', attribution_start: '', attribution_end: '',
    amount: '', source: '', reference: '',
  })

  const set = (k: keyof typeof form, v: string) => setForm(f => ({ ...f, [k]: v }))
  const bad = (f: string) => badFields.includes(f)

  const submit = async () => {
    setBusy(true); setErr(null); setBadFields([])
    try {
      const amount = Number(form.amount)
      if (!Number.isFinite(amount)) {
        setBadFields(['amount']); setErr('amount must be a number'); return
      }
      await supplyPost('/manual-revenue', {
        lane,
        revenue_date: form.revenue_date,
        attribution_start: form.attribution_start,
        attribution_end: form.attribution_end,
        amount,
        source: form.source,
        reference: form.reference,
      })
      toast.addToast({ type: 'success', title: 'Manual revenue recorded', message: `${lane} · ${form.amount}` })
      setForm({ revenue_date: '', attribution_start: '', attribution_end: '', amount: '', source: '', reference: '' })
      setOpen(false)
      onSaved()
    } catch (e) {
      if (e instanceof SupplyError) { setErr(e.message); setBadFields(e.fields) }
      else setErr(e instanceof Error ? e.message : String(e))
    } finally { setBusy(false) }
  }

  return (
    <Panel style={{ marginBottom: 14 }}>
      <SectionHeader
        title="Manual revenue"
        icon={faPenToSquare}
        right={
          <button
            type="button"
            onClick={() => setOpen(o => !o)}
            style={{
              background: alpha(colors.indigo500, '22'), border: `1px solid ${alpha(colors.indigo500, '66')}`,
              color: colors.indigo200, borderRadius: 6, padding: '4px 10px', fontSize: 11, fontWeight: 600, cursor: 'pointer',
            }}
          >
            {open ? 'Cancel' : 'Add entry'}
          </button>
        }
      />
      <div style={{ fontSize: 11, color: colors.textMuted, marginBottom: open ? 10 : 0 }}>
        For lanes whose revenue lives outside Everflow. Every entry is audited (entered_by + entered_at) and feeds the
        economics job alongside attributed Everflow payout. Correct a past entry by filing a new one — the ledger is append-only.
      </div>
      {open && (
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 10, alignItems: 'flex-end' }}>
          <label style={fieldLabelStyle}>Revenue date
            <input type="date" value={form.revenue_date} onChange={e => set('revenue_date', e.target.value)}
              style={{ ...inputStyle, ...(bad('revenue_date') ? { borderColor: colors.danger } : {}) }} />
          </label>
          <label style={fieldLabelStyle}>Attribution start
            <input type="date" value={form.attribution_start} onChange={e => set('attribution_start', e.target.value)}
              style={{ ...inputStyle, ...(bad('attribution_start') ? { borderColor: colors.danger } : {}) }} />
          </label>
          <label style={fieldLabelStyle}>Attribution end
            <input type="date" value={form.attribution_end} onChange={e => set('attribution_end', e.target.value)}
              style={{ ...inputStyle, ...(bad('attribution_end') ? { borderColor: colors.danger } : {}) }} />
          </label>
          <label style={fieldLabelStyle}>Amount (USD)
            <input type="number" step="0.01" value={form.amount} onChange={e => set('amount', e.target.value)}
              placeholder="0.00"
              style={{ ...inputStyle, width: 120, ...(bad('amount') ? { borderColor: colors.danger } : {}) }} />
          </label>
          <label style={fieldLabelStyle}>Source
            <input type="text" value={form.source} onChange={e => set('source', e.target.value)}
              placeholder="e.g. partner statement"
              style={{ ...inputStyle, width: 180, ...(bad('source') ? { borderColor: colors.danger } : {}) }} />
          </label>
          <label style={fieldLabelStyle}>Reference
            <input type="text" value={form.reference} onChange={e => set('reference', e.target.value)}
              placeholder="invoice / report id"
              style={{ ...inputStyle, width: 180, ...(bad('reference') ? { borderColor: colors.danger } : {}) }} />
          </label>
          <button
            type="button"
            disabled={busy}
            onClick={() => { void submit() }}
            style={{
              background: colors.indigo400, color: '#0a0e1a', border: 'none', borderRadius: 6,
              padding: '8px 16px', fontSize: 12, fontWeight: 700, cursor: busy ? 'wait' : 'pointer', opacity: busy ? 0.6 : 1,
            }}
          >
            {busy ? 'Saving…' : 'Record entry'}
          </button>
        </div>
      )}
      {err && (
        <div style={{ marginTop: 8 }}>
          <SectionError label="Manual revenue" error={err} />
        </div>
      )}
    </Panel>
  )
}

export default SupplyLane
