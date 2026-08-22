/**
 * Board Grid — the send-day as a PROPERTY x SLOT grid.
 *
 * The structure of a day (which properties mail, in which slots) is stable; the
 * only daily decision is which offer sits in each cell. So this screen opens on
 * a CLONE of a previous day, already gated, and you edit by exception.
 *
 * Read-only by construction: cloning returns a proposal from the server and
 * nothing here creates a campaign. Deploying a cell still goes through the
 * Campaign Manager, which owns audience planning and the wave sanity check.
 */
import React, { useCallback, useEffect, useMemo, useState } from 'react'
import { apiFetch } from '../shared/apiFetch'
import { colors, panelStyle, panelTitleStyle, btnStyle, pageStyle } from '../shared/theme'

interface Cell {
  property: string
  property_label?: string
  sending_domain?: string
  slot: string
  campaign_id?: string
  name: string
  offer_id?: string
  offer_name?: string
  subject?: string
  status?: string
  recipients: number
  proposed?: boolean
}
interface Finding {
  level: 'blocker' | 'warn'
  code: string
  property: string
  slot: string
  message: string
}
interface Grid {
  date: string
  source_date?: string
  slots: string[]
  properties: string[]
  cells: Cell[]
  findings: Finding[]
  summary: Record<string, number>
}

const dayOffset = (iso: string, n: number): string => {
  const d = new Date(iso + 'T12:00:00Z')
  d.setUTCDate(d.getUTCDate() + n)
  return d.toISOString().slice(0, 10)
}
const todayISO = (): string =>
  new Date(Date.now() - new Date().getTimezoneOffset() * 60000).toISOString().slice(0, 10)

export const BoardGrid: React.FC = () => {
  const [date, setDate] = useState<string>(() => dayOffset(todayISO(), 1))
  const [cloneFrom, setCloneFrom] = useState<string>(() => todayISO())
  const [grid, setGrid] = useState<Grid | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [cloned, setCloned] = useState(false)

  const load = useCallback(async (url: string, isClone: boolean) => {
    setLoading(true); setError(null)
    try {
      const res = await apiFetch(url)
      if (!res.ok) throw new Error(`${res.status} ${await res.text()}`)
      setGrid(await res.json())
      setCloned(isClone)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      setGrid(null)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load(`/api/mailing/board-grid?date=${date}`, false)
  }, [date, load])

  // Findings keyed by cell so each cell can render its own badge.
  const byCell = useMemo(() => {
    const m: Record<string, Finding[]> = {}
    for (const f of grid?.findings ?? []) {
      const k = `${f.property}|${f.slot}`
      ;(m[k] ||= []).push(f)
    }
    return m
  }, [grid])

  const cellAt = useMemo(() => {
    const m: Record<string, Cell> = {}
    for (const c of grid?.cells ?? []) m[`${c.property}|${c.slot}`] = c
    return m
  }, [grid])

  const blockers = grid?.summary?.blocker ?? 0
  const warns = grid?.summary?.warn ?? 0
  const clean = (grid?.cells.length ?? 0) - Object.keys(byCell).length

  return (
    <div style={pageStyle}>
      <div style={{ ...panelStyle, marginBottom: 16 }}>
        <div style={panelTitleStyle}>Board Grid</div>
        <div style={{ display: 'flex', gap: 12, alignItems: 'flex-end', flexWrap: 'wrap' }}>
          <label style={{ fontSize: 12, color: colors.textMuted }}>
            Board date<br />
            <input type="date" value={date} onChange={e => setDate(e.target.value)}
              style={inputStyle} />
          </label>
          <label style={{ fontSize: 12, color: colors.textMuted }}>
            Clone from<br />
            <input type="date" value={cloneFrom} onChange={e => setCloneFrom(e.target.value)}
              style={inputStyle} />
          </label>
          <button style={btnStyle}
            onClick={() => void load(`/api/mailing/board-grid/clone?from=${cloneFrom}&to=${date}`, true)}>
            Clone {cloneFrom} → {date}
          </button>
          <button style={{ ...btnStyle, background: 'transparent' }}
            onClick={() => void load(`/api/mailing/board-grid?date=${date}`, false)}>
            Reload actual
          </button>
        </div>
        {cloned && (
          <div style={{ marginTop: 10, fontSize: 12, color: colors.warning }}>
            Showing a PROPOSAL cloned from {grid?.source_date}. Nothing is created until you deploy
            each cell in the Campaign Manager.
          </div>
        )}
      </div>

      {loading && <div style={{ color: colors.textMuted }}>Loading…</div>}
      {error && <div style={{ ...panelStyle, color: colors.danger }}>Error: {error}</div>}

      {grid && !loading && (
        <>
          <div style={{ ...panelStyle, marginBottom: 16, display: 'flex', gap: 24 }}>
            <Stat label="cells" value={grid.cells.length} color={colors.text} />
            <Stat label="clean" value={clean} color={colors.success} />
            <Stat label="warnings" value={warns} color={colors.warning} />
            <Stat label="blockers" value={blockers} color={colors.danger} />
          </div>

          <div style={{ ...panelStyle, overflowX: 'auto' }}>
            <table style={{ borderCollapse: 'collapse', width: '100%', fontSize: 13 }}>
              <thead>
                <tr>
                  <th style={hdr}>Property</th>
                  {grid.slots.map(s => <th key={s} style={hdr}>{s}</th>)}
                </tr>
              </thead>
              <tbody>
                {grid.properties.map(p => (
                  <tr key={p}>
                    <td style={{ ...cellTd, fontWeight: 600, whiteSpace: 'nowrap' }}>{p}</td>
                    {grid.slots.map(s => {
                      const c = cellAt[`${p}|${s}`]
                      const f = byCell[`${p}|${s}`] ?? []
                      const worst = f.some(x => x.level === 'blocker') ? 'blocker'
                        : f.length ? 'warn' : null
                      return (
                        <td key={s} style={{
                          ...cellTd,
                          background: worst === 'blocker' ? 'rgba(239,68,68,0.12)'
                            : worst === 'warn' ? 'rgba(245,158,11,0.10)' : undefined,
                        }}>
                          {c ? (
                            <div title={f.map(x => `${x.code}: ${x.message}`).join('\n')}>
                              <div style={{ color: colors.text }}>
                                {c.offer_name || <span style={{ color: colors.danger }}>no offer</span>}
                                {worst === 'blocker' && ' ✗'}
                                {worst === 'warn' && ' ⚠'}
                              </div>
                              <div style={{ fontSize: 11, color: colors.textMuted }}>
                                {c.recipients ? c.recipients.toLocaleString() : ''}
                                {c.status ? ` · ${c.status}` : ''}
                              </div>
                            </div>
                          ) : <span style={{ color: colors.textMuted }}>—</span>}
                        </td>
                      )
                    })}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          {grid.findings.length > 0 && (
            <div style={{ ...panelStyle, marginTop: 16 }}>
              <div style={panelTitleStyle}>Findings</div>
              <table style={{ borderCollapse: 'collapse', width: '100%', fontSize: 12 }}>
                <tbody>
                  {grid.findings.map((f, i) => (
                    <tr key={i}>
                      <td style={{ ...cellTd, color: f.level === 'blocker' ? colors.danger : colors.warning, whiteSpace: 'nowrap' }}>
                        {f.level === 'blocker' ? '✗' : '⚠'} {f.code}
                      </td>
                      <td style={{ ...cellTd, whiteSpace: 'nowrap' }}>{f.property} {f.slot}</td>
                      <td style={cellTd}>{f.message}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </div>
  )
}

const Stat: React.FC<{ label: string; value: number; color: string }> = ({ label, value, color }) => (
  <div>
    <div style={{ fontSize: 22, fontWeight: 700, color }}>{value.toLocaleString()}</div>
    <div style={{ fontSize: 11, color: colors.textMuted, textTransform: 'uppercase' }}>{label}</div>
  </div>
)

const inputStyle: React.CSSProperties = {
  background: colors.panelBgSolid, color: colors.text,
  border: `1px solid ${colors.panelBorder}`, borderRadius: 6, padding: '6px 8px', marginTop: 4,
}
const hdr: React.CSSProperties = {
  textAlign: 'left', padding: '8px 10px', color: colors.textMuted,
  borderBottom: `1px solid ${colors.panelBorder}`, fontSize: 11, textTransform: 'uppercase',
  whiteSpace: 'nowrap',
}
const cellTd: React.CSSProperties = {
  padding: '8px 10px', borderBottom: `1px solid ${colors.panelBorder}`,
  verticalAlign: 'top', color: colors.text,
}

export default BoardGrid
