// SupplyDomains.tsx — Pane 3 of the Supply tab (REQ-118 §6, WP10).
//
// One row per sending domain: contracted / effective (+ the binding governor) /
// reserved / committed / released / remaining, and whether the domain's day is
// still open or met. Click through to the per-ISP token buckets, the day's
// capacity-ledger entries for that domain, and the domain contract form.
//
// GAP against §6 (reported, not invented): the API's /supply/domains rows carry
// NO ramp stage and NO health band — those live in sending_domain_cards and are
// not part of this response — and there is no reject endpoint for a scheduled
// contract version. This pane therefore shows what the API measures and says so,
// rather than rendering a stage it cannot source.

import React from 'react'
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome'
import { faArrowLeft, faRotate, faGlobe, faGaugeHigh } from '@fortawesome/free-solid-svg-icons'
import { colors, alpha } from '../shared/theme'
import { Panel, SectionHeader, SectionError, EmptyState, Pill } from '../shared/ui'
import {
  DomainsResponse, DomainResponse,
  supplyGet, Num, HeaderStrip, LoadingRow, ScrollX, Reason,
  fmtClock,
  tableStyle, thStyle, tdStyle, numTd, numTh,
} from './supplyShared'
import { SupplyContractForm } from './SupplyContractForm'
import { SupplyLedger } from './SupplyLedger'

export const SupplyDomains: React.FC<{
  day: string
  domain: string | null
  onSelectDomain: (domain: string | null) => void
}> = ({ day, domain, onSelectDomain }) => {
  const [list, setList] = React.useState<DomainsResponse | null>(null)
  const [listErr, setListErr] = React.useState<string | null>(null)
  const [listLoading, setListLoading] = React.useState(false)
  const [detail, setDetail] = React.useState<DomainResponse | null>(null)
  const [detailErr, setDetailErr] = React.useState<string | null>(null)
  const [detailLoading, setDetailLoading] = React.useState(false)
  const [nonce, setNonce] = React.useState(0)

  React.useEffect(() => {
    const ctrl = new AbortController()
    setListLoading(true)
    supplyGet<DomainsResponse>('/domains', { day }, ctrl.signal)
      .then(d => { setList(d); setListErr(null) })
      .catch(e => { if (!ctrl.signal.aborted) setListErr(e instanceof Error ? e.message : String(e)) })
      .finally(() => { if (!ctrl.signal.aborted) setListLoading(false) })
    return () => ctrl.abort()
  }, [day, nonce])

  React.useEffect(() => {
    if (!domain) { setDetail(null); setDetailErr(null); return }
    const ctrl = new AbortController()
    setDetailLoading(true)
    supplyGet<DomainResponse>(`/domains/${encodeURIComponent(domain)}`, { day }, ctrl.signal)
      .then(d => { setDetail(d); setDetailErr(null) })
      .catch(e => { if (!ctrl.signal.aborted) setDetailErr(e instanceof Error ? e.message : String(e)) })
      .finally(() => { if (!ctrl.signal.aborted) setDetailLoading(false) })
    return () => ctrl.abort()
  }, [domain, day, nonce])

  if (domain) {
    return (
      <DomainDetail
        day={day}
        domain={domain}
        data={detail}
        error={detailErr}
        loading={detailLoading}
        onBack={() => onSelectDomain(null)}
        onRefresh={() => setNonce(n => n + 1)}
      />
    )
  }

  if (listLoading && !list) return <LoadingRow what="the domain capacity table" />

  const labels = list?.labels ?? {}
  const rows = list?.domains ?? []

  return (
    <div>
      <HeaderStrip
        meta={list}
        extra={
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

      <Panel>
        <SectionHeader
          title="Sending domains — contracted capacity vs what the day actually consumed"
          icon={faGlobe}
          right={<span style={{ fontSize: 11, color: colors.textFaint }}>{rows.length} domain{rows.length === 1 ? '' : 's'} with a capacity balance today</span>}
        />
        {listErr ? (
          <SectionError label="Domain table" error={listErr} onRetry={() => setNonce(n => n + 1)} />
        ) : rows.length === 0 ? (
          <EmptyState
            title="No domain capacity rows for this day"
            hint="drip_capacity_balance is rebuilt at midnight and on demand. No rows means capacity is UNKNOWN for this day — not that every domain is capped at zero."
          />
        ) : (
          <ScrollX>
            <table style={tableStyle}>
              <thead>
                <tr>
                  <th style={thStyle}>Sending domain</th>
                  <th style={numTh} title="Number of ISP cells (buckets) this domain has today.">ISP cells</th>
                  <th style={numTh} title="Sum of the domain contract's daily_max_by_isp over the day's cells (contracted).">Contracted</th>
                  <th style={numTh} title="Contracted after governors reduced it (effective). The reason is the most common binding governor across the domain's cells.">Effective</th>
                  <th style={numTh} title="Held by live reservations (reserved).">Reserved</th>
                  <th style={numTh} title="Submitted to a transport (actual).">Committed</th>
                  <th style={numTh} title="Reservations released or expired without submitting (actual).">Released</th>
                  <th style={numTh} title="effective − reserved − committed, floored at 0 (effective).">Remaining</th>
                  <th style={thStyle} title="open = capacity remains · met = the day's effective capacity is fully reserved or committed.">Status</th>
                  <th style={thStyle} title="Newest token-bucket refill across the domain's cells — how current this row is.">Last refill</th>
                </tr>
              </thead>
              <tbody>
                {rows.map(r => (
                  <DomainRowView key={r.sending_domain} row={r} labels={labels} onOpen={() => onSelectDomain(r.sending_domain)} />
                ))}
              </tbody>
            </table>
          </ScrollX>
        )}
        <div style={{ marginTop: 10, fontSize: 11, color: colors.textFaint }}>
          Ramp stage and ISP health band are not in this response — they live in <code>sending_domain_cards</code> and reach
          the supply chain only as governor inputs (they reduce <em>effective</em>, and the reduction shows in the effective
          reason). Read them on Domain Agents / Domain Center.
        </div>
      </Panel>
    </div>
  )
}

const DomainRowView: React.FC<{
  row: DomainsResponse['domains'][number]
  labels: Record<string, string>
  onOpen: () => void
}> = ({ row, labels, onOpen }) => {
  const [hover, setHover] = React.useState(false)
  const met = row.status === 'met'
  return (
    <tr
      onClick={onOpen}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      style={{ cursor: 'pointer', background: hover ? colors.hover : undefined }}
      title="Open this domain's per-ISP buckets, ledger and contract"
    >
      <td style={tdStyle}><strong style={{ color: colors.heading }}>{row.sending_domain}</strong></td>
      <td style={numTd}>{row.isp_cells}</td>
      <td style={numTd}><Num value={row.contracted} label={labels['contracted']} what="Domain contract daily max, summed over ISPs" /></td>
      <td style={numTd}>
        <Num
          value={row.effective}
          label={labels['effective']}
          what="Contracted after governors"
          color={row.effective != null && row.contracted != null && row.effective < row.contracted ? colors.warningText : undefined}
        />
        <Reason text={row.effective_reason} />
      </td>
      <td style={numTd}><Num value={row.reserved} label={labels['reserved']} what="Held by live reservations" /></td>
      <td style={numTd}><Num value={row.committed} label={labels['committed']} what="Submitted to a transport" /></td>
      <td style={numTd}><Num value={row.released} label={labels['released']} what="Released or expired without submitting" /></td>
      <td style={numTd}><Num value={row.remaining} label={labels['remaining']} what="effective − reserved − committed" /></td>
      <td style={tdStyle}>
        <Pill color={met ? colors.success : colors.indigo400} style={{ fontSize: 10 }}>{row.status}</Pill>
      </td>
      <td style={{ ...tdStyle, fontSize: 11, color: colors.textMuted, whiteSpace: 'nowrap' }}>
        {row.last_refill_tick ? fmtClock(row.last_refill_tick) : <span style={{ color: colors.textFaint, fontStyle: 'italic' }} title="no cell recorded a refill tick — unknown, not never">unknown</span>}
      </td>
    </tr>
  )
}

const DomainDetail: React.FC<{
  day: string
  domain: string
  data: DomainResponse | null
  error: string | null
  loading: boolean
  onBack: () => void
  onRefresh: () => void
}> = ({ day, domain, data, error, loading, onBack, onRefresh }) => {
  const labels = data?.labels ?? {}
  const seed = React.useMemo(() => {
    const out: Record<string, number | null> = {}
    ;(data?.buckets_by_isp ?? []).forEach(b => { out[b.isp] = b.contracted })
    return out
  }, [data])

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
          <FontAwesomeIcon icon={faArrowLeft} /> All domains
        </button>
        <h2 style={{ margin: 0, fontSize: 18, color: colors.heading }}>{domain}</h2>
        <button
          type="button"
          onClick={onRefresh}
          style={{
            background: alpha(colors.indigo500, '22'), border: `1px solid ${alpha(colors.indigo500, '66')}`,
            color: colors.indigo200, borderRadius: 6, padding: '4px 10px', fontSize: 11, fontWeight: 600, cursor: 'pointer',
          }}
        >
          <FontAwesomeIcon icon={faRotate} /> Refresh
        </button>
      </div>

      <HeaderStrip meta={data} />
      {error && <SectionError label={`Domain ${domain}`} error={error} onRetry={onRefresh} />}
      {loading && !data && <LoadingRow what={`domain ${domain}`} />}

      {data && (
        <>
          <Panel style={{ marginBottom: 14 }}>
            <SectionHeader title="Token buckets by ISP" icon={faGaugeHigh} />
            {data.buckets_by_isp.length === 0 ? (
              <EmptyState title="No buckets for this domain today" hint="Capacity is unknown for this domain today — not zero. Check that the domain has an active contract and that the balance rebuild ran." />
            ) : (
              <ScrollX>
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={thStyle}>ISP</th>
                      <th style={numTh} title="daily_max_by_isp from the active domain contract (contracted).">Contracted</th>
                      <th style={numTh} title="Contracted after governors (effective), with the binding governor named below.">Effective</th>
                      <th style={numTh} title="Current token-bucket balance: refill = effective ÷ active intervals per tick, capped at max_burst_intervals × refill (effective).">Tokens</th>
                      <th style={numTh} title="Held by live reservations (reserved).">Reserved</th>
                      <th style={numTh} title="Submitted to a transport (actual).">Committed</th>
                      <th style={numTh} title="Released or expired without submitting (actual).">Released</th>
                      <th style={numTh} title="effective − reserved − committed, floored at 0 (effective).">Remaining</th>
                      <th style={thStyle}>Last refill</th>
                    </tr>
                  </thead>
                  <tbody>
                    {data.buckets_by_isp.map(b => (
                      <tr key={b.isp}>
                        <td style={tdStyle}><strong style={{ color: colors.heading }}>{b.isp}</strong></td>
                        <td style={numTd}><Num value={b.contracted} label={labels['contracted']} what="Contract daily max for this ISP" /></td>
                        <td style={numTd}>
                          <Num
                            value={b.effective}
                            label={labels['effective']}
                            what="After governors"
                            color={b.effective != null && b.contracted != null && b.effective < b.contracted ? colors.warningText : undefined}
                          />
                          <Reason text={b.effective_reason} />
                        </td>
                        <td style={numTd}><Num value={b.tokens} label={labels['tokens']} format={v => v.toFixed(1)} what="Token-bucket balance now" /></td>
                        <td style={numTd}><Num value={b.reserved} label={labels['reserved']} what="Held by live reservations" /></td>
                        <td style={numTd}><Num value={b.committed} label={labels['committed']} what="Submitted to a transport" /></td>
                        <td style={numTd}><Num value={b.released} label={labels['released']} what="Released or expired" /></td>
                        <td style={numTd}><Num value={b.remaining} label={labels['remaining']} what="effective − reserved − committed" /></td>
                        <td style={{ ...tdStyle, fontSize: 11, color: colors.textMuted, whiteSpace: 'nowrap' }}>
                          {b.last_refill_tick ? fmtClock(b.last_refill_tick) : <span style={{ color: colors.textFaint, fontStyle: 'italic' }}>unknown</span>}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </ScrollX>
            )}
          </Panel>

          <Panel style={{ marginBottom: 14 }}>
            <SectionHeader title="Domain contract" icon={faGlobe} />
            <SupplyContractForm
              kind="domain"
              subject={domain}
              seedISPValues={seed}
              seedISPProvenance="drip_capacity_balance.contracted for this domain today — i.e. the ACTIVE contract's daily_max_by_isp as the mediator projected it"
              onChanged={onRefresh}
            />
          </Panel>

          <SupplyLedger day={day} domain={domain} title={`Ledger drill-through — ${domain}`} />
        </>
      )}
    </div>
  )
}

export default SupplyDomains
