/**
 * Day Cards — one send-day for one sending domain, campaign by campaign.
 *
 * Header picks a sending domain (from the day-cards/domains endpoint; kumo
 * domains carry a transport pill) and a Denver date. The PREV-DAY strip gives
 * yesterday's totals as context (PG counters — the lake/VDM stays the per-ISP
 * delivery scoreboard). The grid shows one card per campaign on the selected
 * day; clicking a card opens the DayCardEditor SideShelf, which drives the
 * cancel-and-rebuild flow via /api/mailing/pmta-campaign/day-cards/rebuild.
 *
 * Every endpoint here may not be deployed yet (built in parallel with the
 * backend) — a 404 degrades to an explicit message, never a blank screen.
 */
import React, { useCallback, useEffect, useState } from 'react'
import { apiFetch } from '../shared/apiFetch'
import { colors, alpha, panelStyle, btnStyle, pageStyle, cardGrid, thStyle, tdStyle, numTd, numTh, tableStyle } from '../shared/theme'
import { Panel, SectionHeader, Stat, Pill, EmptyState } from '../shared/ui'
import { DayCardEditor } from './DayCardEditor'

// ── Contract types ──────────────────────────────────────────────────────────

export type Transport = 'pmta' | 'ses' | 'kumo'

export interface DayCardDomain {
  sending_domain: string
  transport: Transport
  profile_ids: string[]
}

export interface DayCard {
  id: string
  name: string
  status: string
  scheduled_at: string
  slot: string
  offer_id: string
  offer_key: string
  offer_name: string
  subject: string
  from_name: string
  total_recipients: number
  queued_count: number
  sent_count: number
  delivered_count: number
  open_count: number
  click_count: number
  bounce_count: number
  unsubscribe_count: number
  has_config: boolean
  rebuilt_from: string | null
  transport: Transport
  // Not promised by the contract, but read defensively when the backend
  // includes them so the editor can show the current audience.
  inclusion_segments?: string[]
  exclusion_segments?: string[]
  exclusion_lists?: string[]
  min_remail_hours?: number
}

interface PrevDayTotals {
  campaigns: number
  recipients: number
  sent: number
  delivered: number
  opened: number
  clicked: number
  bounced: number
  unsubscribed: number
}

interface PrevDay {
  date: string
  totals: PrevDayTotals
  campaigns: DayCard[]
}

interface DayCardsResponse {
  date: string
  domain: string
  cards: DayCard[]
  prev_day: PrevDay | null
}

// ── Helpers ─────────────────────────────────────────────────────────────────

/** Today's date in America/Denver as YYYY-MM-DD (en-CA gives ISO order). */
const denverToday = (): string =>
  new Intl.DateTimeFormat('en-CA', { timeZone: 'America/Denver' }).format(new Date())

const fmt = (n: number | undefined | null): string => (n ?? 0).toLocaleString()

const pct = (num: number, den: number): string =>
  den > 0 ? `${((num / den) * 100).toFixed(1)}%` : '—'

const statusColor = (status: string): string => {
  const s = status.toLowerCase()
  if (['failed', 'error'].includes(s)) return colors.danger
  if (['cancelled', 'canceled'].includes(s)) return colors.textFaint
  if (['sending', 'sent', 'completed'].includes(s)) return colors.success
  if (['preparing', 'finalizing_audience', 'queued'].includes(s)) return colors.warning
  if (['scheduled', 'draft'].includes(s)) return colors.indigo400
  return colors.idle
}

const timePart = (iso: string): string => {
  if (!iso) return ''
  const d = new Date(iso)
  if (isNaN(d.getTime())) return iso
  return new Intl.DateTimeFormat('en-US', {
    timeZone: 'America/Denver', hour: '2-digit', minute: '2-digit', hour12: false,
  }).format(d) + ' MT'
}

/** Turn a fetch failure into the explicit degradation text this screen shows. */
const describeError = async (res: Response, endpoint: string): Promise<string> => {
  if (res.status === 404) {
    return `${endpoint} returned 404 — the Day Cards backend is not deployed on this server yet. The screen stays read-empty until it lands.`
  }
  let body = ''
  try { body = await res.text() } catch { /* body unavailable */ }
  return `${endpoint} failed: ${res.status}${body ? ` ${body.slice(0, 400)}` : ''}`
}

// ── Screen ──────────────────────────────────────────────────────────────────

export const DayCards: React.FC = () => {
  const [domains, setDomains] = useState<DayCardDomain[]>([])
  const [domainsError, setDomainsError] = useState<string | null>(null)
  const [domain, setDomain] = useState<string>('')
  const [date, setDate] = useState<string>(denverToday)

  const [data, setData] = useState<DayCardsResponse | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [prevOpen, setPrevOpen] = useState(true)
  const [editing, setEditing] = useState<DayCard | null>(null)

  const load = useCallback(async (dom: string, d: string) => {
    if (!dom || !d) return
    setLoading(true)
    setError(null)
    try {
      const endpoint = `/api/mailing/pmta-campaign/day-cards?domain=${encodeURIComponent(dom)}&date=${d}`
      const res = await apiFetch(endpoint)
      if (!res.ok) throw new Error(await describeError(res, 'GET /api/mailing/pmta-campaign/day-cards'))
      const j: DayCardsResponse = await res.json()
      setData({ ...j, cards: j.cards ?? [], prev_day: j.prev_day ?? null })
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      setData(null)
    } finally {
      setLoading(false)
    }
  }, [])

  // Domains once on mount; auto-select the first and load today for it.
  useEffect(() => {
    void (async () => {
      try {
        const res = await apiFetch('/api/mailing/pmta-campaign/day-cards/domains')
        if (!res.ok) throw new Error(await describeError(res, 'GET /api/mailing/pmta-campaign/day-cards/domains'))
        const j = await res.json()
        const rows: DayCardDomain[] = Array.isArray(j) ? j : (j.domains ?? [])
        setDomains(rows)
        if (rows.length > 0) {
          setDomain(rows[0].sending_domain)
          void load(rows[0].sending_domain, denverToday())
        }
      } catch (e) {
        setDomainsError(e instanceof Error ? e.message : String(e))
      }
    })()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const selectedTransport: Transport =
    domains.find(x => x.sending_domain === domain)?.transport ?? 'pmta'

  const prev = data?.prev_day ?? null

  return (
    <div style={pageStyle}>
      {/* Header controls */}
      <Panel style={{ marginBottom: 16 }}>
        <SectionHeader title="Day Cards" />
        <div style={{ display: 'flex', gap: 12, alignItems: 'flex-end', flexWrap: 'wrap' }}>
          <label style={{ fontSize: 12, color: colors.textMuted }}>
            Sending domain<br />
            <select
              value={domain}
              onChange={e => { setDomain(e.target.value); void load(e.target.value, date) }}
              style={{ ...inputStyle, minWidth: 260 }}
            >
              {domains.length === 0 && <option value="">no domains loaded</option>}
              {domains.map(d => (
                <option key={d.sending_domain} value={d.sending_domain}>
                  {d.sending_domain}{d.transport === 'kumo' ? '  · kumo' : ''}
                </option>
              ))}
            </select>
          </label>
          {selectedTransport === 'kumo' && <Pill color={colors.warning}>kumo</Pill>}
          {selectedTransport !== 'kumo' && domain !== '' && (
            <Pill color={colors.indigo400}>{selectedTransport}</Pill>
          )}
          <label style={{ fontSize: 12, color: colors.textMuted }}>
            Date (Denver)<br />
            <input
              type="date"
              value={date}
              onChange={e => setDate(e.target.value)}
              onBlur={e => void load(domain, e.target.value)}
              onKeyDown={e => { if (e.key === 'Enter') void load(domain, (e.target as HTMLInputElement).value) }}
              style={inputStyle}
            />
          </label>
          <button style={btnStyle} onClick={() => void load(domain, date)}>Load</button>
          <button
            style={{ ...btnStyle, background: 'transparent' }}
            onClick={() => void load(domain, date)}
            disabled={loading || !domain}
          >
            Refresh
          </button>
        </div>
        {domainsError && (
          <div style={{ marginTop: 10, fontSize: 12, color: colors.dangerText }}>{domainsError}</div>
        )}
      </Panel>

      {loading && <div style={{ color: colors.textMuted, marginBottom: 12 }}>Loading…</div>}
      {error && (
        <Panel accent={colors.danger} style={{ marginBottom: 16 }}>
          <div style={{ fontSize: 13, color: colors.dangerText }}>{error}</div>
        </Panel>
      )}

      {/* PREV-DAY strip */}
      {data && prev && (
        <Panel style={{ marginBottom: 16 }}>
          <SectionHeader
            title={`Yesterday — ${data.domain} (${prev.date})`}
            right={
              <button style={{ ...btnStyle, background: 'transparent' }} onClick={() => setPrevOpen(o => !o)}>
                {prevOpen ? 'Collapse' : 'Expand'}
              </button>
            }
          />
          {prevOpen && (
            <>
              <div style={{ display: 'flex', gap: 24, flexWrap: 'wrap', marginBottom: 12 }}>
                <Stat label="campaigns" value={fmt(prev.totals.campaigns)} sub="mailed yesterday" />
                <Stat label="recipients" value={fmt(prev.totals.recipients)} sub="planned recipients" />
                <Stat label="sent" value={fmt(prev.totals.sent)} sub="of recipients" />
                <Stat
                  label="delivered %"
                  value={pct(prev.totals.delivered, prev.totals.sent)}
                  sub={`${fmt(prev.totals.delivered)} of ${fmt(prev.totals.sent)} sent`}
                  color={colors.success}
                />
                <Stat label="opens" value={fmt(prev.totals.opened)} sub={`of ${fmt(prev.totals.delivered)} delivered`} />
                <Stat label="clicks" value={fmt(prev.totals.clicked)} sub={`of ${fmt(prev.totals.delivered)} delivered`} />
                <Stat label="bounces" value={fmt(prev.totals.bounced)} sub={`of ${fmt(prev.totals.sent)} sent`} color={colors.warning} />
                <Stat label="unsubs" value={fmt(prev.totals.unsubscribed)} sub={`of ${fmt(prev.totals.delivered)} delivered`} />
              </div>
              {(prev.campaigns ?? []).length > 0 && (
                <div style={{ overflowX: 'auto' }}>
                  <table style={tableStyle}>
                    <thead>
                      <tr>
                        <th style={thStyle}>Campaign</th>
                        <th style={thStyle}>Slot</th>
                        <th style={numTh}>Sent</th>
                        <th style={numTh}>Delivered</th>
                        <th style={numTh}>Opens</th>
                        <th style={numTh}>Clicks</th>
                      </tr>
                    </thead>
                    <tbody>
                      {prev.campaigns.map(c => (
                        <tr key={c.id}>
                          <td style={tdStyle}>{c.name}</td>
                          <td style={tdStyle}>{c.slot || '—'}</td>
                          <td style={numTd}>{fmt(c.sent_count)}</td>
                          <td style={numTd}>{fmt(c.delivered_count)}</td>
                          <td style={numTd}>{fmt(c.open_count)}</td>
                          <td style={numTd}>{fmt(c.click_count)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
              <div style={{ marginTop: 10, fontSize: 11, color: colors.textFaint }}>
                PG counters — per-ISP delivery truth is VDM/lake; context, not the scoreboard.
              </div>
            </>
          )}
        </Panel>
      )}

      {/* Cards grid */}
      {data && (
        <>
          <SectionHeader title={`${data.date} — ${data.cards.length} campaign${data.cards.length === 1 ? '' : 's'}`} style={{ marginBottom: 12 }} />
          {data.cards.length === 0 ? (
            <Panel>
              <EmptyState title="No campaigns on this day" hint="Pick another date or sending domain." />
            </Panel>
          ) : (
            <div style={cardGrid(340)}>
              {data.cards.map(c => {
                const cancelled = ['cancelled', 'canceled'].includes(c.status.toLowerCase())
                return (
                  <div
                    key={c.id}
                    onClick={() => setEditing(c)}
                    style={{
                      ...panelStyle,
                      cursor: 'pointer',
                      opacity: cancelled ? 0.5 : 1,
                      borderLeft: `4px solid ${statusColor(c.status)}`,
                    }}
                  >
                    <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap', marginBottom: 6 }}>
                      <Pill color={statusColor(c.status)}>{c.status}</Pill>
                      {c.slot && (
                        <span style={{
                          fontSize: 11, fontWeight: 700, color: colors.indigo300,
                          background: alpha(colors.indigo500, '14'), borderRadius: 6, padding: '2px 8px',
                        }}>
                          {c.slot}
                        </span>
                      )}
                      <Pill color={c.transport === 'kumo' ? colors.warning : colors.indigo400}>{c.transport}</Pill>
                      {!c.has_config && (
                        <span title="This campaign predates the stored deploy config (pre-blob) — the original inputs are not recoverable, so rebuild is unavailable.">
                          <Pill color={colors.danger}>no config</Pill>
                        </span>
                      )}
                    </div>
                    <div style={{ fontSize: 13, fontWeight: 700, color: colors.text, marginBottom: 4 }}>
                      {c.name}
                    </div>
                    <div style={{ fontSize: 12, color: colors.textMuted, marginBottom: 2 }}>
                      {c.offer_name || 'no offer'} · {timePart(c.scheduled_at)}
                    </div>
                    {c.subject && (
                      <div style={{
                        fontSize: 12, color: colors.textFaint, marginBottom: 6,
                        whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis',
                      }} title={c.subject}>
                        “{c.subject}”
                      </div>
                    )}
                    <div style={{ display: 'flex', gap: 16, fontSize: 12, color: colors.textMuted, fontVariantNumeric: 'tabular-nums' }}>
                      <span>recipients <b style={{ color: colors.text }}>{fmt(c.total_recipients)}</b></span>
                      <span>queued <b style={{ color: colors.text }}>{fmt(c.queued_count)}</b></span>
                      <span>sent <b style={{ color: colors.text }}>{fmt(c.sent_count)}</b></span>
                    </div>
                    {c.rebuilt_from && (
                      <div style={{ marginTop: 6, fontSize: 11, color: colors.warningText }}>
                        rebuilt from {c.rebuilt_from}
                      </div>
                    )}
                  </div>
                )
              })}
            </div>
          )}
        </>
      )}

      {!data && !loading && !error && domains.length === 0 && !domainsError && (
        <Panel><EmptyState title="Loading sending domains…" /></Panel>
      )}

      {editing && (
        <DayCardEditor
          card={editing}
          domain={domain}
          onClose={() => setEditing(null)}
          onRebuilt={() => { setEditing(null); void load(domain, date) }}
        />
      )}
    </div>
  )
}

const inputStyle: React.CSSProperties = {
  background: colors.panelBgSolid,
  color: colors.text,
  border: `1px solid ${colors.panelBorder}`,
  borderRadius: 6,
  padding: '6px 8px',
  marginTop: 4,
}

export default DayCards
