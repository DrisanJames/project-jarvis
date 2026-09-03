// SupplyLedger.tsx — the two append-only ledgers, with drill-through
// (REQ-118 §1.2, §3, WP10).
//
// The Capacity Ledger counts MESSAGES, one row per wave-level allocation. The
// Supply Ledger counts unique RECORDS, batch-grained. They are never summed
// together; the panel headers say which unit each table is in.
//
// Drill-through: the filters are props (a lane pane passes its lane, a domain
// pane passes its domain) plus in-table ISP / touch-class narrowing. Clicking a
// row's domain or lane narrows to that cell — the ledger is where a number on
// the panes above is traced back to the allocation that produced it.
//
// A ledger response carries `limit`; when the server clamps, the table says so
// rather than pretending the page is the whole day.

import React from 'react'
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome'
import { faRotate, faReceipt, faBoxesStacked } from '@fortawesome/free-solid-svg-icons'
import { colors, alpha } from '../shared/theme'
import { Panel, SectionHeader, SectionError, EmptyState } from '../shared/ui'
import { FilterChip } from '../shared/filters'
import {
  CapacityLedgerResponse, SupplyLedgerResponse,
  supplyGet, Num, HeaderStrip, ScrollX, fmtTime, fmtUSD,
  tableStyle, thStyle, tdStyle, numTd, numTh,
} from './supplyShared'

const STATUS_COLOR: Record<string, string> = {
  reserved: colors.warning,
  committed: colors.success,
  released: colors.idle,
  expired: colors.danger,
}

export const SupplyLedger: React.FC<{
  day: string
  lane?: string
  domain?: string
  title?: string
}> = ({ day, lane, domain, title }) => {
  const [isp, setIsp] = React.useState<string>('')
  const [source, setSource] = React.useState<string>('')
  const [cap, setCap] = React.useState<CapacityLedgerResponse | null>(null)
  const [sup, setSup] = React.useState<SupplyLedgerResponse | null>(null)
  const [capErr, setCapErr] = React.useState<string | null>(null)
  const [supErr, setSupErr] = React.useState<string | null>(null)
  const [loading, setLoading] = React.useState(false)
  const [nonce, setNonce] = React.useState(0)

  React.useEffect(() => {
    const ctrl = new AbortController()
    setLoading(true)
    const capReq = supplyGet<CapacityLedgerResponse>('/ledger/capacity', {
      day, lane, domain, isp: isp || undefined,
    }, ctrl.signal)
      .then(d => { setCap(d); setCapErr(null) })
      .catch(e => { if (!ctrl.signal.aborted) setCapErr(e instanceof Error ? e.message : String(e)) })
    const supReq = supplyGet<SupplyLedgerResponse>('/ledger/supply', {
      day, lane, source: source || undefined,
    }, ctrl.signal)
      .then(d => { setSup(d); setSupErr(null) })
      .catch(e => { if (!ctrl.signal.aborted) setSupErr(e instanceof Error ? e.message : String(e)) })
    void Promise.allSettled([capReq, supReq]).then(() => { if (!ctrl.signal.aborted) setLoading(false) })
    return () => ctrl.abort()
  }, [day, lane, domain, isp, source, nonce])

  const capLabels = cap?.labels ?? {}

  return (
    <Panel>
      <SectionHeader
        title={title ?? 'Ledgers'}
        icon={faReceipt}
        right={
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
        }
      />

      <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', marginBottom: 10 }}>
        <span style={{ fontSize: 11, color: colors.textFaint, textTransform: 'uppercase', letterSpacing: 0.5 }}>Scope:</span>
        <FilterChip label={`day=${day}`} />
        {lane && <FilterChip label={`lane=${lane}`} />}
        {domain && <FilterChip label={`domain=${domain}`} />}
        {isp && <FilterChip label={`isp=${isp}`} onRemove={() => setIsp('')} />}
        {source && <FilterChip label={`source=${source}`} onRemove={() => setSource('')} />}
        {loading && <span style={{ fontSize: 11, color: colors.textMuted }}>loading…</span>}
      </div>

      <HeaderStrip meta={cap ?? sup} />

      {/* ── Capacity ledger — MESSAGES ─────────────────────────────── */}
      <div style={{ marginBottom: 16 }}>
        <div style={{ fontSize: 12, fontWeight: 700, color: colors.heading, marginBottom: 6 }}>
          Capacity ledger <span style={{ fontWeight: 400, color: colors.textMuted, fontSize: 11 }}>· messages · one row per wave-level allocation</span>
        </div>
        {capErr ? (
          <SectionError label="Capacity ledger" error={capErr} onRetry={() => setNonce(n => n + 1)} />
        ) : cap == null ? (
          <div style={{ fontSize: 11, color: colors.textFaint }}>Loading…</div>
        ) : cap.entries.length === 0 ? (
          <EmptyState title="No allocations in this scope" hint="The ledger is append-only: no row means no reservation was attempted here today — which is different from a reservation that was granted 0 (that DOES write a row, with binding_reason)." />
        ) : (
          <>
            <ScrollX maxHeight={420}>
              <table style={tableStyle}>
                <thead>
                  <tr>
                    <th style={thStyle}>Tick (Denver)</th>
                    <th style={thStyle}>Domain</th>
                    <th style={thStyle}>ISP</th>
                    <th style={thStyle}>Lane</th>
                    <th style={thStyle} title="intro | followup | remail — intros and follow-ups share ONE balance.">Touch</th>
                    <th style={numTh} title="What the executor asked for (planned).">Requested</th>
                    <th style={numTh} title="What the reservation granted; never decremented afterwards (reserved).">Reserved</th>
                    <th style={numTh} title="What was actually submitted to a transport — a subset of reserved (actual).">Committed</th>
                    <th style={numTh} title="Given back unspent, or expired after 45 minutes with no commit (actual).">Released</th>
                    <th style={thStyle}>Status</th>
                    <th style={thStyle} title="Which term of the reservation minimum bound the grant: requested | domain_tokens | governor:… | lane_demand | supply | plan_share | reserve_timeout | outside_window | no_balance | no_lane_balance.">Binding</th>
                    <th style={thStyle} title="Why the reservation was released or expired (release_reason). Distinct from binding_reason, which records the grant.">Release</th>
                    <th style={numTh} title="The domain balance left after this allocation (effective).">Domain after</th>
                    <th style={numTh} title="The lane's unfilled demand after this allocation (planned).">Lane unfilled</th>
                    <th style={thStyle} title="Contract versions this allocation was made under.">Versions</th>
                    <th style={thStyle}>Campaign</th>
                  </tr>
                </thead>
                <tbody>
                  {cap.entries.map(e => (
                    <tr key={e.allocation_id}>
                      <td style={{ ...tdStyle, whiteSpace: 'nowrap' }} title={`idempotency_key ${e.idempotency_key}`}>{fmtTime(e.tick)}</td>
                      <td style={tdStyle}>{e.sending_domain}</td>
                      <td style={tdStyle}>
                        <button
                          type="button"
                          onClick={() => setIsp(e.isp)}
                          title={`Narrow this ledger to ${e.isp}`}
                          style={{ background: 'transparent', border: 'none', color: colors.indigo300, cursor: 'pointer', padding: 0, fontSize: 12 }}
                        >
                          {e.isp}
                        </button>
                      </td>
                      <td style={tdStyle}>{e.lane}</td>
                      <td style={tdStyle}>{e.touch_class}</td>
                      <td style={numTd}><Num value={e.requested} label={capLabels['ledger.requested']} what="Requested by the executor" /></td>
                      <td style={numTd}><Num value={e.reserved} label={capLabels['ledger.reserved']} what="Granted by the reservation" /></td>
                      <td style={numTd}><Num value={e.committed} label={capLabels['ledger.committed']} what="Submitted to a transport" /></td>
                      <td style={numTd}><Num value={e.released} what="Released or expired" /></td>
                      <td style={tdStyle}>
                        <span style={{ color: STATUS_COLOR[e.status] ?? colors.textMuted, fontSize: 11, fontWeight: 700, textTransform: 'uppercase' }}>
                          {e.status}
                        </span>
                      </td>
                      <td style={{ ...tdStyle, fontSize: 11, color: colors.textMuted }}>{e.binding_reason || '—'}</td>
                      <td style={{ ...tdStyle, fontSize: 11, color: e.release_reason ? colors.warningText : colors.textFaint }}>{e.release_reason || '—'}</td>
                      <td style={numTd}><Num value={e.domain_balance_after} what="Domain balance left after this allocation" /></td>
                      <td style={numTd}><Num value={e.lane_unfilled_after} what="Lane demand still unfilled after this allocation" /></td>
                      <td style={{ ...tdStyle, fontSize: 10, color: colors.textFaint, whiteSpace: 'nowrap' }}>
                        dom v{e.domain_contract_version} · dis v{e.dispatch_contract_version}
                      </td>
                      <td style={{ ...tdStyle, fontFamily: 'monospace', fontSize: 10 }} title={e.campaign_id ?? 'no campaign — this allocation never reached a wave'}>
                        {e.campaign_id ? `${e.campaign_id.slice(0, 8)}…` : '—'}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </ScrollX>
            {cap.entries.length >= cap.limit && (
              <div style={{ fontSize: 11, color: colors.warningText, marginTop: 6 }}>
                Showing the newest {cap.limit} allocations — the server clamped the page. Narrow by ISP or domain to see the rest;
                these rows are NOT the whole day.
              </div>
            )}
          </>
        )}
      </div>

      {/* ── Supply ledger — RECORDS ────────────────────────────────── */}
      <div>
        <div style={{ fontSize: 12, fontWeight: 700, color: colors.heading, marginBottom: 6 }}>
          <FontAwesomeIcon icon={faBoxesStacked} style={{ marginRight: 6, color: colors.indigo400 }} />
          Supply ledger <span style={{ fontWeight: 400, color: colors.textMuted, fontSize: 11 }}>· unique records · batch-grained</span>
        </div>
        {supErr ? (
          <SectionError label="Supply ledger" error={supErr} onRetry={() => setNonce(n => n + 1)} />
        ) : sup == null ? (
          <div style={{ fontSize: 11, color: colors.textFaint }}>Loading…</div>
        ) : sup.entries.length === 0 ? (
          <EmptyState title="No supply events in this scope" hint="Append-only: nothing arrived, was ordered, validated or consumed here today. A measured nothing — not an unknown." />
        ) : (
          <>
            <ScrollX maxHeight={420}>
              <table style={tableStyle}>
                <thead>
                  <tr>
                    <th style={thStyle}>Occurred (Denver)</th>
                    <th style={thStyle}>Lane</th>
                    <th style={thStyle}>Source</th>
                    <th style={thStyle}>ISP</th>
                    <th style={thStyle} title="RECEIVED → PRECHECK_PASSED → VALIDATION_* → MAILABLE → RESERVED_FOR_INTRO → CONSUMED, with SUPPRESSED / INTERNAL_INVALID / EXPIRED / RELEASED as exits.">Event</th>
                    <th style={numTh} title="Records in this ledger entry (actual).">Quantity</th>
                    <th style={numTh} title="Cost per record for this entry.">Unit cost</th>
                    <th style={numTh} title="quantity × unit cost.">Total cost</th>
                    <th style={thStyle}>Reason</th>
                    <th style={thStyle} title="Contract versions in force when the entry was written.">Versions</th>
                    <th style={thStyle} title="The inbound batch or EO order this entry belongs to.">Batch</th>
                  </tr>
                </thead>
                <tbody>
                  {sup.entries.map(e => (
                    <tr key={e.entry_id}>
                      <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>{fmtTime(e.occurred_at)}</td>
                      <td style={tdStyle}>{e.lane}</td>
                      <td style={tdStyle}>
                        <button
                          type="button"
                          onClick={() => setSource(e.source_slug)}
                          title={`Narrow this ledger to source ${e.source_slug}`}
                          style={{ background: 'transparent', border: 'none', color: colors.indigo300, cursor: 'pointer', padding: 0, fontSize: 12 }}
                        >
                          {e.source_slug}
                        </button>
                      </td>
                      <td style={tdStyle}>{e.isp}</td>
                      <td style={{ ...tdStyle, fontSize: 11 }}>{e.event}</td>
                      <td style={numTd}><Num value={e.quantity} label="actual" what="Records in this entry" /></td>
                      <td style={numTd}><Num value={e.unit_cost} format={v => fmtUSD(v, 6)} label="actual" what="Cost per record" /></td>
                      <td style={numTd}><Num value={e.total_cost} format={v => fmtUSD(v, 4)} label="actual" what="quantity × unit cost" /></td>
                      <td style={{ ...tdStyle, fontSize: 11, color: colors.textMuted, maxWidth: 260 }} title={e.reason}>{e.reason || '—'}</td>
                      <td style={{ ...tdStyle, fontSize: 10, color: colors.textFaint, whiteSpace: 'nowrap' }}>
                        {e.source_contract_version != null ? `src v${e.source_contract_version}` : 'src —'}
                        {' · '}
                        {e.inventory_contract_version != null ? `inv v${e.inventory_contract_version}` : 'inv —'}
                      </td>
                      <td style={{ ...tdStyle, fontFamily: 'monospace', fontSize: 10 }} title={e.batch_id ?? 'no batch'}>
                        {e.batch_id ? `${e.batch_id.slice(0, 8)}…` : '—'}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </ScrollX>
            {sup.entries.length >= sup.limit && (
              <div style={{ fontSize: 11, color: colors.warningText, marginTop: 6 }}>
                Showing the newest {sup.limit} entries — the server clamped the page. These rows are NOT the whole day.
              </div>
            )}
          </>
        )}
      </div>
    </Panel>
  )
}

export default SupplyLedger
