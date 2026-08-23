/**
 * Board Grid — the send-day as a PROPERTY x SLOT grid.
 *
 * The structure of a day (which properties mail, in which slots) is stable; the
 * only daily decision is which offer sits in each cell. So this screen opens on
 * a CLONE of a previous day, already gated, and you edit by exception: click a
 * cell's offer to swap it, then re-run the gates over the edited grid.
 *
 * Edits are LOCAL to this screen UNTIL "Rebuild live": the edit-by-exception
 * loop now carries a LIVE execution path that drives the EXISTING Day Cards
 * rebuild endpoint (/api/mailing/pmta-campaign/day-cards/rebuild) — per cell
 * from the shelf's MAKE IT REAL section, or for every edited live cell at once
 * via the toolbar's batch rebuild (typed 'REBUILD N' confirm, sequential,
 * aborts on the first 502 half-state). A clone keeps the source day's slots
 * (the stable structure) with only the name's date token rewritten; staging
 * still happens via /stage-board or the Campaign Manager.
 */
import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { apiFetch } from '../shared/apiFetch'
import { colors, panelStyle, panelTitleStyle, btnStyle, pageStyle } from '../shared/theme'
import { BoardCellShelf, resolveApprovedProofs, cellDomainRoots, NO_PROOF_MESSAGE, halfStateBox } from './BoardCellShelf'
import type { BoardCell as Cell, BoardFindingRow as Finding, OfferOpt, OfferProof } from './BoardCellShelf'

interface Grid {
  date: string
  source_date?: string
  slots: string[]
  properties: string[]
  cells: Cell[]
  findings: Finding[]
  summary: Record<string, number>
}

// ── Batch live rebuild (toolbar "Rebuild N edited live") ────────────────────
interface BatchItem {
  idx: number
  property: string
  slot: string
  name: string
  campaignId?: string
  offerId?: string          // captured at dialog-open time — the reload at the
  proofId?: string          // end must not change what was executed
  proofName?: string
  oldOffer: string
  newOffer: string
  skipReason?: string
  result?: { kind: 'ok' | 'failed' | 'halfstate' | 'aborted'; text: string }
}
interface BatchState {
  items: BatchItem[]
  typed: string
  running: boolean
  done: boolean
  abortedText: string | null  // the verbatim 502 body that aborted the batch
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
  // Approved+active offer proofs (fetched once) — the shelf's MAKE IT REAL
  // section and the batch rebuild resolve each offer's proof from this pool.
  const [proofs, setProofs] = useState<OfferProof[]>([])
  const [proofsError, setProofsError] = useState<string | null>(null)
  const [batch, setBatch] = useState<BatchState | null>(null)
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
        const oj: { offers?: Array<{ id: string; key?: string; name: string; status?: string }> } = await res.json()
        setOffers((oj.offers ?? [])
          .filter(x => x.status === 'active')
          .map(x => ({ id: x.id, name: x.name, key: x.key }))
          .sort((a, b) => a.name.localeCompare(b.name)))
        setOffersError(null)
      } catch (e) {
        setOffers([])
        setOffersError(`offer catalog unavailable: ${e instanceof Error ? e.message : 'network error'}`)
      }
    })()
  }, [])

  // Approved proofs, fetched once. `key` on offers ↔ `offer_key` on proofs is
  // the join. A failure is surfaced in the shelf, never an empty rebuild path.
  useEffect(() => {
    void (async () => {
      try {
        const res = await apiFetch('/api/mailing/offer-proofs?status=approved&active=true')
        if (!res.ok) {
          setProofs([])
          setProofsError(`proof catalog unavailable: HTTP ${res.status}`)
          return
        }
        const pj: { proofs?: OfferProof[] } = await res.json()
        setProofs(pj.proofs ?? [])
        setProofsError(null)
      } catch (e) {
        setProofs([])
        setProofsError(`proof catalog unavailable: ${e instanceof Error ? e.message : 'network error'}`)
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

  // A confirmed LIVE rebuild landed (shelf MAKE IT REAL): reload the actual
  // grid — the reload replaces the working copies from the server, which also
  // clears the rebuilt cell's edited flag.
  const handleRebuilt = () => {
    if (serverGrid) loadActual(serverGrid.date)
  }

  // ── Batch live rebuild ────────────────────────────────────────────────────
  // Build the summary dialog: one row per edited cell, with the proof resolved
  // by the SAME rules as the shelf (offer_key match + approved_domains scope,
  // newest first). Cells that can't execute are kept and reported, not hidden.
  const openBatch = () => {
    const items: BatchItem[] = Object.keys(edited).map(Number).sort((a, b) => a - b)
      .filter(i => cells[i] !== undefined)
      .map(i => {
        const c = cells[i]
        const orig = serverGrid?.cells?.[i]
        const base: BatchItem = {
          idx: i, property: c.property, slot: c.slot, name: c.name,
          campaignId: c.campaign_id, offerId: c.offer_id,
          oldOffer: orig?.offer_name || orig?.offer_id || '(none)',
          newOffer: c.offer_name || c.offer_id || '(none)',
        }
        if (!c.campaign_id || c.proposed) {
          return { ...base, skipReason: 'no live campaign at this cell — nothing to rebuild' }
        }
        if (!c.offer_id) return { ...base, skipReason: 'no offer applied' }
        const key = offers.find(o => o.id === c.offer_id)?.key
        if (!key) return { ...base, skipReason: `offer key unknown for ${c.offer_id} — cannot resolve approved proofs` }
        if (proofsError) return { ...base, skipReason: proofsError }
        const eligible = resolveApprovedProofs(proofs, key, cellDomainRoots(c))
        if (eligible.length === 0) return { ...base, skipReason: NO_PROOF_MESSAGE }
        return { ...base, proofId: eligible[0].id, proofName: eligible[0].name }
      })
    setBatch({ items, typed: '', running: false, done: false, abortedText: null })
  }

  // Sequential confirmed:true rebuilds — one campaign at a time, results
  // verbatim per cell, ABORT the remainder on the first 502 half-state, then
  // reload the actual grid regardless of outcome.
  const runBatch = async () => {
    if (!batch || !serverGrid || batch.running || batch.done) return
    const items = batch.items.map(it => ({ ...it }))
    setBatch(b => (b ? { ...b, running: true, items: items.map(x => ({ ...x })) } : b))
    let abortedText: string | null = null
    for (const it of items) {
      if (it.skipReason) continue
      if (abortedText) {
        it.result = { kind: 'aborted', text: 'not attempted — batch aborted after 502 half-state' }
        setBatch(b => (b ? { ...b, items: items.map(x => ({ ...x })) } : b))
        continue
      }
      try {
        const res = await apiFetch('/api/mailing/pmta-campaign/day-cards/rebuild', {
          method: 'POST',
          body: JSON.stringify({
            campaign_id: it.campaignId,
            confirmed: true,
            overrides: { offer_id: it.offerId, proof_id: it.proofId },
          }),
        })
        const raw = await res.text()
        if (res.status === 502) {
          it.result = { kind: 'halfstate', text: raw }
          abortedText = raw
        } else if (!res.ok) {
          it.result = { kind: 'failed', text: `HTTP ${res.status}: ${raw}` }
        } else {
          let rj: { cancelled_campaign_id?: string; new_campaign_id?: string; sent_before_cancel?: number; note?: string } = {}
          try { rj = JSON.parse(raw) as typeof rj } catch { /* non-JSON success body — ids unknown */ }
          it.result = {
            kind: 'ok',
            text: `cancelled ${rj.cancelled_campaign_id ?? '?'} → new ${rj.new_campaign_id ?? '?'}`
              + (typeof rj.sent_before_cancel === 'number' ? ` · sent before cancel: ${rj.sent_before_cancel.toLocaleString()}` : '')
              + (rj.note ? ` · ${rj.note}` : ''),
          }
        }
      } catch (e) {
        it.result = { kind: 'failed', text: e instanceof Error ? e.message : 'network error' }
      }
      setBatch(b => (b ? { ...b, items: items.map(x => ({ ...x })) } : b))
    }
    setBatch(b => (b ? { ...b, running: false, done: true, abortedText } : b))
    loadActual(serverGrid.date)
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
              {!cloned && editedCount > 0 && (
                <button
                  style={{
                    ...btnStyle, background: 'rgba(239,68,68,0.14)',
                    border: '1px solid rgba(239,68,68,0.45)', color: colors.danger,
                  }}
                  disabled={!!batch}
                  onClick={openBatch}>
                  Rebuild {editedCount} edited live
                </button>
              )}
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
            picker. Edits are LOCAL to this screen and gates auto re-run on apply — local UNTIL
            "Rebuild live": the shelf's MAKE IT REAL action (or the toolbar batch rebuild) cancels
            + redeploys the live campaign with the new offer and its approved proof. The shelf's
            attach-offer LIVE repair for no-offer cells is unchanged.
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
              proofs={proofs}
              proofsError={proofsError}
              edited={!!edited[shelfIdx]}
              gating={gating}
              cloneMode={cloned}
              onApplyOffer={applyOffer}
              onAttached={() => { attachedRef.current = true }}
              onRebuilt={handleRebuilt}
              onClose={closeShelf}
            />
          )}
        </>
      )}

      {/* Batch live rebuild dialog — rendered OUTSIDE the grid gate so the
          per-cell results stay visible through the end-of-batch reload. */}
      {batch && (() => {
        const execCount = batch.items.filter(it => !it.skipReason).length
        const confirmPhrase = `REBUILD ${execCount}`
        return (
          <div style={overlayStyle}>
            <div style={modalStyle}>
              <div style={{ ...panelTitleStyle, color: colors.danger }}>
                Rebuild edited cells — LIVE
              </div>
              <div style={{ fontSize: 12, color: colors.textMuted, marginBottom: 10, lineHeight: 1.5 }}>
                Sequentially CANCELS + REDEPLOYS each edited live campaign with its new offer and
                that offer's approved proof (auto-resolved, newest first). Skipped cells are
                reported below, not silently dropped. The batch ABORTS on the first 502 half-state.
              </div>
              <div style={{ maxHeight: 340, overflowY: 'auto', marginBottom: 12 }}>
                <table style={{ borderCollapse: 'collapse', width: '100%', fontSize: 12 }}>
                  <thead>
                    <tr>
                      <th style={hdr}>Property</th>
                      <th style={hdr}>Slot</th>
                      <th style={hdr}>Offer (old → new)</th>
                      <th style={hdr}>Proof</th>
                      <th style={hdr}>Result</th>
                    </tr>
                  </thead>
                  <tbody>
                    {batch.items.map(it => (
                      <tr key={it.idx}>
                        <td style={{ ...cellTd, whiteSpace: 'nowrap' }}>{it.property}</td>
                        <td style={{ ...cellTd, whiteSpace: 'nowrap' }}>{it.slot}</td>
                        <td style={cellTd}>{it.oldOffer} → <b>{it.newOffer}</b></td>
                        <td style={cellTd}>{it.proofName || '—'}</td>
                        <td style={{
                          ...cellTd,
                          color: it.skipReason ? colors.warning
                            : it.result?.kind === 'ok' ? colors.success
                            : it.result ? colors.danger
                            : colors.textMuted,
                          whiteSpace: 'pre-wrap', wordBreak: 'break-word',
                        }}>
                          {it.skipReason ? `SKIPPED: ${it.skipReason}`
                            : it.result ? `${it.result.kind === 'ok' ? '✓' : '✗'} ${it.result.text}`
                            : batch.running ? '…' : 'pending'}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              {batch.abortedText !== null && (
                <div style={halfStateBox}>
                  HALF-STATE (HTTP 502) — batch ABORTED. That campaign may be CANCELLED with its
                  redeploy FAILED. Verify in Day Cards before retrying.{'\n\n'}{batch.abortedText}
                </div>
              )}
              {!batch.running && !batch.done && (
                execCount > 0 ? (
                  <div style={{ marginTop: 8 }}>
                    <div style={{ fontSize: 11, color: colors.textMuted, marginBottom: 4 }}>
                      Type <b style={{ color: colors.danger }}>{confirmPhrase}</b> to execute — this
                      CANCELS and REDEPLOYS {execCount} live campaign{execCount === 1 ? '' : 's'}.
                    </div>
                    <input style={{ ...inputStyle, width: 220 }}
                      placeholder={confirmPhrase}
                      value={batch.typed}
                      onChange={e => {
                        const v = e.target.value
                        setBatch(b => (b ? { ...b, typed: v } : b))
                      }} />
                    <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
                      <button
                        style={{
                          ...btnStyle, background: 'rgba(239,68,68,0.14)',
                          border: '1px solid rgba(239,68,68,0.45)', color: colors.danger,
                          opacity: batch.typed === confirmPhrase ? 1 : 0.5,
                        }}
                        disabled={batch.typed !== confirmPhrase}
                        onClick={() => void runBatch()}>
                        Execute {execCount} rebuild{execCount === 1 ? '' : 's'} — LIVE
                      </button>
                      <button style={{ ...btnStyle, background: 'transparent' }}
                        onClick={() => setBatch(null)}>
                        Cancel
                      </button>
                    </div>
                  </div>
                ) : (
                  <div style={{ marginTop: 8 }}>
                    <div style={{ fontSize: 12, color: colors.danger, marginBottom: 8 }}>
                      Nothing executable — every edited cell was skipped (reasons above).
                    </div>
                    <button style={{ ...btnStyle, background: 'transparent' }}
                      onClick={() => setBatch(null)}>
                      Close
                    </button>
                  </div>
                )
              )}
              {batch.running && (
                <div style={{ fontSize: 12, color: colors.warning, marginTop: 8 }}>
                  Executing sequentially… leave this dialog open.
                </div>
              )}
              {batch.done && (
                <button style={{ ...btnStyle, marginTop: 8 }} onClick={() => setBatch(null)}>
                  Close
                </button>
              )}
            </div>
          </div>
        )
      })()}
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
const overlayStyle: React.CSSProperties = {
  position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.55)', zIndex: 60,
  display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 24,
}
const modalStyle: React.CSSProperties = {
  ...panelStyle, width: 'min(880px, 100%)', maxHeight: '85vh', overflowY: 'auto',
}

export default BoardGrid
