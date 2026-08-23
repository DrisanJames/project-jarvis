/**
 * Board Grid — the send-day as a PROPERTY x SLOT grid.
 *
 * The structure of a day (which properties mail, in which slots) is stable; the
 * only daily decision is which offer sits in each cell. So this screen opens on
 * a CLONE of a previous day, already gated, and you edit by exception: click a
 * cell's offer to swap it, then re-run the gates over the edited grid.
 *
 * Decision-support only: edits are LOCAL to this screen until acted on —
 * nothing here writes to campaigns. A clone keeps the source day's slots (the
 * stable structure) with only the name's date token rewritten; staging still
 * happens via /stage-board or the Campaign Manager.
 */
import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { apiFetch } from '../shared/apiFetch'
import { colors, panelStyle, panelTitleStyle, btnStyle, pageStyle } from '../shared/theme'
import { BoardCellShelf } from './BoardCellShelf'
import type { BoardCell as Cell, BoardFindingRow as Finding, OfferOpt } from './BoardCellShelf'

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
  // serverGrid is the last thing the SERVER said (actual day or clone
  // proposal). cells/findings/summary are the working copies the operator
  // edits and re-gates; "Reset edits" restores them from serverGrid.
  const [serverGrid, setServerGrid] = useState<Grid | null>(null)
  const [cells, setCells] = useState<Cell[]>([])
  const [findings, setFindings] = useState<Finding[]>([])
  const [summary, setSummary] = useState<Record<string, number>>({})
  const [edited, setEdited] = useState<Record<number, boolean>>({})
  // shelfIdx: index into cells of the campaign open in the side shelf.
  const [shelfIdx, setShelfIdx] = useState<number | null>(null)
  const [offers, setOffers] = useState<OfferOpt[]>([])
  const [offersError, setOffersError] = useState<string | null>(null)
  // A confirmed LIVE attach landed while the shelf was open — reload the
  // actual grid when the shelf closes so the row reflects the server.
  const attachedRef = useRef(false)
  const [loading, setLoading] = useState(false)
  const [gating, setGating] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [cloned, setCloned] = useState(false)
  const [copied, setCopied] = useState(false)

  const load = useCallback(async (url: string, isClone: boolean) => {
    setLoading(true); setError(null)
    try {
      const res = await apiFetch(url)
      if (!res.ok) throw new Error(`${res.status} ${await res.text()}`)
      const g: Grid = await res.json()
      setServerGrid(g)
      setCells(g.cells ?? [])
      setFindings(g.findings ?? [])
      setSummary(g.summary ?? {})
      setEdited({})
      // Do NOT force-close the shelf here: a load resolving AFTER the
      // operator opened a cell (double-fired date-change loads, background
      // refresh) silently yanked the shelf shut. The render gate already
      // hides the shelf gracefully when its index no longer resolves; only
      // clamp an index that fell off the end of a smaller day.
      setShelfIdx(prev => (prev !== null && prev >= (g.cells ?? []).length ? null : prev))
      setCloned(isClone)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      setServerGrid(null); setCells([]); setFindings([]); setSummary({})
    } finally {
      setLoading(false)
    }
  }, [])

  const loadActual = useCallback((d: string) => {
    void load(`/api/mailing/board-grid?date=${d}`, false)
  }, [load])

  // Load once on mount. The date input does NOT reload on every keystroke —
  // blur/Enter or the Load button fire the fetch.
  useEffect(() => {
    loadActual(date)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // Offer catalog for the shelf picker, fetched once. GET /api/mailing/offers
  // 404s in prod (that route does not exist) — the REAL endpoint is
  // /api/mailing/offers/list → {offers:[{id,key,name,everflow_id,status}]}.
  // A failure is SURFACED (offersError chip in the shelf), never swallowed
  // into an empty picker.
  useEffect(() => {
    void (async () => {
      try {
        const res = await apiFetch('/api/mailing/offers/list')
        if (!res.ok) {
          setOffers([])
          setOffersError(`offer catalog unavailable: HTTP ${res.status}`)
          return
        }
        const oj: { offers?: Array<{ id: string; name: string; status?: string }> } = await res.json()
        setOffers((oj.offers ?? [])
          .filter(x => x.status === 'active')
          .map(x => ({ id: x.id, name: x.name }))
          .sort((a, b) => a.name.localeCompare(b.name)))
        setOffersError(null)
      } catch (e) {
        setOffers([])
        setOffersError(`offer catalog unavailable: ${e instanceof Error ? e.message : 'network error'}`)
      }
    })()
  }, [])

  // Gates run over an explicit cell array (not the state variable) so an
  // applyOffer can gate the NEXT cells synchronously with the edit.
  const runGates = useCallback(async (cellsToGate: Cell[]) => {
    if (!serverGrid) return
    setGating(true); setError(null)
    try {
      const res = await apiFetch('/api/mailing/board-grid/gates', {
        method: 'POST',
        body: JSON.stringify({ date: serverGrid.date, cells: cellsToGate }),
      })
      if (!res.ok) throw new Error(`${res.status} ${await res.text()}`)
      const g: Grid = await res.json()
      setFindings(g.findings ?? [])
      setSummary(g.summary ?? {})
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setGating(false)
    }
  }, [serverGrid])

  // Local edit + AUTO gate re-run (findings refresh without hunting for the
  // button). The global Re-run gates button still works via runGates(cells).
  const applyOffer = (idx: number, offerId: string) => {
    const opt = offers.find(o => o.id === offerId)
    if (!opt) return
    const next = cells.map((c, i) =>
      i === idx ? { ...c, offer_id: opt.id, offer_name: opt.name } : c)
    setCells(next)
    setEdited(prev => ({ ...prev, [idx]: true }))
    void runGates(next)
  }

  const resetEdits = () => {
    if (!serverGrid) return
    setCells(serverGrid.cells ?? [])
    setFindings(serverGrid.findings ?? [])
    setSummary(serverGrid.summary ?? {})
    setEdited({})
  }

  const closeShelf = () => {
    setShelfIdx(null)
    if (attachedRef.current && serverGrid) {
      attachedRef.current = false
      loadActual(serverGrid.date)
    }
  }

  const copyProposal = async () => {
    if (!serverGrid) return
    const payload = JSON.stringify({
      date: serverGrid.date,
      cells: cells.map(c => ({
        property: c.property,
        brand_root: c.brand_root ?? '',
        slot: c.slot,
        offer: c.offer_name || c.offer_id || '',
        name: c.name,
      })),
    }, null, 2)
    try {
      await navigator.clipboard.writeText(payload)
    } catch {
      const ta = document.createElement('textarea')
      ta.value = payload
      ta.style.position = 'fixed'
      document.body.appendChild(ta)
      ta.select()
      document.execCommand('copy')
      document.body.removeChild(ta)
    }
    setCopied(true)
    window.setTimeout(() => setCopied(false), 2000)
  }

  // Findings keyed by cell so each cell can render its own badge.
  const byCell = useMemo(() => {
    const m: Record<string, Finding[]> = {}
    for (const f of findings) {
      const k = `${f.property}|${f.slot}`
      ;(m[k] ||= []).push(f)
    }
    return m
  }, [findings])

  // ALL campaign indices at each (property, slot) — a collision is real data
  // and must not be collapsed to one cell.
  const idxAt = useMemo(() => {
    const m: Record<string, number[]> = {}
    cells.forEach((c, i) => {
      const k = `${c.property}|${c.slot}`
      ;(m[k] ||= []).push(i)
    })
    return m
  }, [cells])

  const editedCount = Object.keys(edited).length
  const blockers = summary.blocker ?? 0
  const warns = summary.warn ?? 0
  const clean = summary.clean ?? 0

  return (
    <div style={pageStyle}>
      <div style={{ ...panelStyle, marginBottom: 16 }}>
        <div style={panelTitleStyle}>Board Grid</div>
        <div style={{ display: 'flex', gap: 12, alignItems: 'flex-end', flexWrap: 'wrap' }}>
          <label style={{ fontSize: 12, color: colors.textMuted }}>
            Board date<br />
            <input type="date" value={date}
              onChange={e => setDate(e.target.value)}
              onBlur={e => loadActual(e.target.value)}
              onKeyDown={e => { if (e.key === 'Enter') loadActual((e.target as HTMLInputElement).value) }}
              style={inputStyle} />
          </label>
          <button style={btnStyle} onClick={() => loadActual(date)}>Load</button>
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
            onClick={() => loadActual(date)}>
            Reload actual
          </button>
        </div>
        {cloned && (
          <div style={{ marginTop: 10, fontSize: 12, color: colors.warning }}>
            Showing a PROPOSAL cloned from {serverGrid?.source_date}: it keeps that day's slots
            (the stable structure) with each name's date token rewritten to {serverGrid?.date}.
            Nothing is created here — staging still happens via /stage-board or the Campaign Manager.
          </div>
        )}
      </div>

      {loading && <div style={{ color: colors.textMuted }}>Loading…</div>}
      {error && <div style={{ ...panelStyle, color: colors.danger }}>Error: {error}</div>}

      {serverGrid && !loading && (
        <>
          <div style={{ ...panelStyle, marginBottom: 16, display: 'flex', gap: 24, alignItems: 'center', flexWrap: 'wrap' }}>
            <Stat label="cells" value={cells.length} color={colors.text} />
            <Stat label="clean" value={clean} color={colors.success} />
            <Stat label="warnings" value={warns} color={colors.warning} />
            <Stat label="blockers" value={blockers} color={colors.danger} />
            <Stat label="edited" value={editedCount} color={colors.warning} />
            <div style={{ display: 'flex', gap: 8, marginLeft: 'auto', flexWrap: 'wrap' }}>
              <button style={btnStyle} disabled={gating} onClick={() => void runGates(cells)}>
                {gating ? 'Gating…' : 'Re-run gates'}
              </button>
              <button style={{ ...btnStyle, background: 'transparent' }}
                disabled={editedCount === 0} onClick={resetEdits}>
                Reset edits
              </button>
              {cloned && (
                <button style={{ ...btnStyle, background: 'transparent' }}
                  onClick={() => void copyProposal()}>
                  {copied ? 'Copied ✓' : 'Copy proposal JSON'}
                </button>
              )}
            </div>
          </div>

          <div style={{ marginBottom: 12, fontSize: 12, color: colors.textMuted }}>
            Click a cell to open its editor shelf: details, this cell's findings, and the offer
            picker. Edits are LOCAL to this screen and gates auto re-run on apply — nothing
            deploys from here except the shelf's explicit attach-offer LIVE action.
          </div>

          {cells.length === 0 ? (
            <div style={{ ...panelStyle, color: colors.textMuted }}>
              No campaigns on this date.
            </div>
          ) : (
            <div style={{ ...panelStyle, overflowX: 'auto' }}>
              <table style={{ borderCollapse: 'collapse', width: '100%', fontSize: 13 }}>
                <thead>
                  <tr>
                    <th style={hdr}>Property</th>
                    {serverGrid.slots.map(s => <th key={s} style={hdr}>{s}</th>)}
                  </tr>
                </thead>
                <tbody>
                  {serverGrid.properties.map(p => (
                    <tr key={p}>
                      <td style={{ ...cellTd, fontWeight: 600, whiteSpace: 'nowrap' }}>{p}</td>
                      {serverGrid.slots.map(s => {
                        const idxs = idxAt[`${p}|${s}`] ?? []
                        const f = byCell[`${p}|${s}`] ?? []
                        const worst = f.some(x => x.level === 'blocker') ? 'blocker'
                          : f.length ? 'warn' : null
                        const idx = idxs[0]
                        const c = idx !== undefined ? cells[idx] : undefined
                        const isEdited = idxs.some(i => edited[i])
                        const tooltip = [
                          ...f.map(x => `${x.code}: ${x.message}`),
                          ...(idxs.length > 1
                            ? ['—', `${idxs.length} campaigns at this anchor:`,
                               ...idxs.map(i => `  ${cells[i].name}`)]
                            : []),
                        ].join('\n')
                        return (
                          <td key={s} style={{
                            ...cellTd,
                            background: worst === 'blocker' ? 'rgba(239,68,68,0.12)'
                              : worst === 'warn' ? 'rgba(245,158,11,0.10)' : undefined,
                          }}>
                            {c ? (
                              /* The WHOLE cell opens the shelf — a label-only
                                 click target left a dead zone right of short
                                 labels ("no offer") that swallowed clicks. */
                              <div title={tooltip} style={{ cursor: 'pointer' }}
                                onClick={() => setShelfIdx(idx)}>
                                <div style={{ color: colors.text }}>
                                  {c.offer_name || <span style={{ color: colors.danger }}>no offer</span>}
                                  {worst === 'blocker' && ' ✗'}
                                  {worst === 'warn' && ' ⚠'}
                                  {idxs.length > 1 && (
                                    <span style={multiBadge}>×{idxs.length}</span>
                                  )}
                                  {isEdited && <span style={editedChip}>edited</span>}
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
          )}

          {findings.length > 0 && (
            <div style={{ ...panelStyle, marginTop: 16 }}>
              <div style={panelTitleStyle}>Findings</div>
              <table style={{ borderCollapse: 'collapse', width: '100%', fontSize: 12 }}>
                <tbody>
                  {findings.map((f, i) => (
                    <tr key={i}>
                      <td style={{ ...cellTd, color: f.level === 'blocker' ? colors.danger : colors.warning, whiteSpace: 'nowrap' }}>
                        {f.level === 'blocker' ? '✗' : '⚠'} {f.code}
                      </td>
                      <td style={{ ...cellTd, whiteSpace: 'nowrap' }}>{f.property} {f.slot}</td>
                      <td style={cellTd}>
                        {f.message}
                        {f.code === 'MISSING_OFFER' && (
                          <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 2 }}>
                            Fix: Day Cards → open the campaign → attach offer or rebuild with offer.
                          </div>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}

          {shelfIdx !== null && cells[shelfIdx] && (
            <BoardCellShelf
              date={serverGrid.date}
              entries={(idxAt[`${cells[shelfIdx].property}|${cells[shelfIdx].slot}`] ?? [shelfIdx])
                .map(i => ({ idx: i, cell: cells[i] }))}
              activeIdx={shelfIdx}
              onSelectEntry={setShelfIdx}
              findings={findings}
              offers={offers}
              offersError={offersError}
              edited={!!edited[shelfIdx]}
              gating={gating}
              cloneMode={cloned}
              onApplyOffer={applyOffer}
              onAttached={() => { attachedRef.current = true }}
              onClose={closeShelf}
            />
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
const multiBadge: React.CSSProperties = {
  marginLeft: 6, padding: '0 5px', borderRadius: 8, fontSize: 10, fontWeight: 700,
  background: 'rgba(239,68,68,0.25)', color: colors.danger,
}
const editedChip: React.CSSProperties = {
  marginLeft: 6, padding: '0 5px', borderRadius: 8, fontSize: 10, fontWeight: 700,
  background: 'rgba(99,102,241,0.25)', color: colors.text, textTransform: 'uppercase',
}

export default BoardGrid
