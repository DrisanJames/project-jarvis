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
import { BoardCellShelf, resolveApprovedProofs, cellDomainRoots, isOfferExemptName, NO_PROOF_MESSAGE, halfStateBox } from './BoardCellShelf'
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

// ── Schedule proposal (POST /api/mailing/board-grid/stage) ──────────────────
interface StageItem {
  idx: number
  property: string
  slot: string
  name: string
  sourceCampaignId?: string
  offerId?: string
  offerName?: string
  proofId?: string
  proofName?: string
  inclusionSegments?: string[]
  excludeISPs?: string[]
  ispCaps?: Record<string, number>
  audienceLabel: string
  exempt: boolean            // KUMO-WARM / newsletter — no offer by doctrine
  skipReason?: string
  result?: { kind: 'deployed' | 'already_existed' | 'failed'; text: string }
}
interface StageState {
  items: StageItem[]
  typed: string
  running: boolean
  done: boolean
  requestError: string | null // whole-request failure (400 at the door / network)
}
// One row of GET /api/mailing/pmta-campaign/clone-candidates.
interface CloneCandidate {
  id: string
  name: string
  has_config?: boolean
  recommended?: boolean
  campaign_date?: string
}
// One per-cell result of POST /board-grid/stage.
interface StageCellResult {
  property: string
  slot: string
  name: string
  status: 'dry' | 'deployed' | 'already_existed' | 'failed'
  campaign_id?: string
  code?: number
  error?: string
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
  // Gate-run feedback: set after EVERY gates run (auto or button) — the
  // missing feedback the operator flagged ("Re-run Gates does nothing").
  const [gateMsg, setGateMsg] = useState<string | null>(null)
  // BOARD-WIDE timing + throttle (operator ruling: applies to the ENTIRE
  // board, never per cell). slotTimes holds only the REMAPPED columns.
  const [slotTimes, setSlotTimes] = useState<Record<string, string>>({})
  const [throttleStrategy, setThrottleStrategy] = useState('')
  const [windowHours, setWindowHours] = useState(0)
  // Empty-cell "new campaign" flow: clone-candidates cached per property.
  const [candidatesByProp, setCandidatesByProp] = useState<Record<string, CloneCandidate[]>>({})
  const [newCellError, setNewCellError] = useState<string | null>(null)
  const [stage, setStage] = useState<StageState | null>(null)

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
      // A fresh grid is a fresh editing session: board-wide overrides and the
      // last gate-run line belong to the proposal that was just replaced.
      setGateMsg(null)
      setSlotTimes({})
      setThrottleStrategy('')
      setWindowHours(0)
      setNewCellError(null)
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
      // The visible gate-run verdict — every run (auto or button) reports.
      const s = g.summary ?? {}
      setGateMsg(`Gates re-ran at ${new Date().toLocaleTimeString()}: `
        + `${s.clean ?? 0} clean · ${s.warn ?? 0} warning${(s.warn ?? 0) === 1 ? '' : 's'} · `
        + `${s.blocker ?? 0} blocker${(s.blocker ?? 0) === 1 ? '' : 's'}`)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      setGateMsg(null)
    } finally {
      setGating(false)
    }
  }, [serverGrid])

  // Local edit + AUTO gate re-run (findings refresh without hunting for the
  // button). The global Re-run gates button still works via runGates(cells).
  // extras carry the shelf's picked approved proof (proposal cells); a
  // proposal apply WITHOUT extras clears any stored proof — the pick always
  // reflects the current offer.
  const applyOffer = (idx: number, offerId: string, extras?: { proofId: string; proofName: string }) => {
    const opt = offers.find(o => o.id === offerId)
    if (!opt) return
    const next = cells.map((c, i) => {
      if (i !== idx) return c
      let name = c.name
      if (c.new_cell && serverGrid) {
        // Board naming convention for a cell born empty:
        // '<MMDDYYYY> - <PROPERTY> - <offer short>' on the grid's date.
        const [y, m, d] = serverGrid.date.split('-')
        const short = opt.name.trim().split(/\s+/)[0] || 'offer'
        name = `${m}${d}${y} - ${c.property} - ${short}`
      }
      return {
        ...c, name, offer_id: opt.id, offer_name: opt.name,
        proof_id: extras?.proofId, proof_name: extras?.proofName,
      }
    })
    setCells(next)
    setEdited(prev => ({ ...prev, [idx]: true }))
    void runGates(next)
  }

  // Fine-grain ISP controls for a proposal cell (empty = cleared).
  const applyISPControls = (idx: number, excludes: string[], caps: Record<string, number>) => {
    setCells(prev => prev.map((c, i) => i === idx
      ? {
          ...c,
          exclude_isps: excludes.length ? excludes : undefined,
          isp_caps: Object.keys(caps).length ? caps : undefined,
        }
      : c))
    setEdited(prev => ({ ...prev, [idx]: true }))
  }

  // Audience override for a proposal cell (null = back to source audience).
  const applyAudience = (idx: number, segments: Array<{ id: string; name: string }> | null) => {
    setCells(prev => prev.map((c, i) => i === idx
      ? {
          ...c,
          inclusion_segments: segments ? segments.map(s => s.id) : undefined,
          inclusion_segment_names: segments ? segments.map(s => s.name) : undefined,
        }
      : c))
    setEdited(prev => ({ ...prev, [idx]: true }))
  }

  // ── Empty-cell → NEW campaign proposal ────────────────────────────────────
  // An empty (property, slot) cell click materializes a local proposed cell
  // whose deploy payload is the property's most recent campaign WITH a config
  // blob, resolved via the existing clone-candidates endpoint (lazy, cached
  // per property). No candidate with a blob → an explicit message, never a
  // silent dead end.
  const addNewCell = async (property: string, slot: string) => {
    setNewCellError(null)
    const sibling = cells.find(c => c.property === property && (c.brand_root || c.sending_domain))
    const domain = sibling?.brand_root || sibling?.sending_domain || property.toLowerCase()
    let list = candidatesByProp[property]
    if (!list) {
      try {
        const res = await apiFetch(`/api/mailing/pmta-campaign/clone-candidates?domain=${encodeURIComponent(domain)}`)
        if (!res.ok) throw new Error(`HTTP ${res.status}`)
        const j: { campaigns?: CloneCandidate[] } = await res.json()
        list = j.campaigns ?? []
        setCandidatesByProp(prev => ({ ...prev, [property]: list as CloneCandidate[] }))
      } catch (e) {
        setNewCellError(`clone-candidates for ${property}: ${e instanceof Error ? e.message : 'network error'}`)
        return
      }
    }
    // Newest-first list: prefer the recommended candidate with a blob, else
    // the newest one that has a blob.
    const src = list.find(c => c.recommended && c.has_config) ?? list.find(c => c.has_config)
    if (!src) {
      setNewCellError(`no prior campaign payload for ${property} — schedule its first campaign via Campaign Manager`)
      return
    }
    const next: Cell[] = [...cells, {
      property,
      property_label: sibling?.property_label ?? property,
      sending_domain: sibling?.sending_domain,
      brand_root: sibling?.brand_root,
      slot,
      name: '', // generated on offer apply: '<MMDDYYYY> - <PROP> - <offer short>'
      recipients: 0,
      proposed: true,
      new_cell: true,
      source_campaign_id: src.id,
    }]
    const idx = next.length - 1
    setCells(next)
    setEdited(prev => ({ ...prev, [idx]: true }))
    setShelfIdx(idx)
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

  // ── Schedule proposal ─────────────────────────────────────────────────────
  // Findings keyed by cell — declared above the stage memo that reads it.
  const byCell = useMemo(() => {
    const m: Record<string, Finding[]> = {}
    for (const f of findings) {
      const k = `${f.property}|${f.slot}`
      ;(m[k] ||= []).push(f)
    }
    return m
  }, [findings])

  // Every proposal cell, classified: schedulable (offer + approved proof —
  // explicit pick first, else auto-resolved newest by the SAME rules as the
  // shelf — and no blocker-level finding) or skipped with a reason. Offer-
  // exempt names (KUMO-WARM/newsletter) schedule without offer/proof.
  const stageInfo = useMemo(() => {
    const items: StageItem[] = []
    cells.forEach((c, i) => {
      if (!c.proposed) return
      const f = byCell[`${c.property}|${c.slot}`] ?? []
      const hasBlocker = f.some(x => x.level === 'blocker')
      const exempt = isOfferExemptName(c.name)
      // A proof is only needed when the OFFER CHANGED (or the cell was born
      // empty): an unchanged clone cell rides the source blob's own creative,
      // byte-faithful. Requiring — or auto-attaching — a proof on unchanged
      // cells did two bad things: it made a straight clone unschedulable when
      // an offer had no approved proof yet, and it silently REPLACED a proven
      // creative with the newest proof on cells the operator never touched.
      const orig = serverGrid?.cells?.[i]
      const offerChanged = !!c.new_cell || (c.offer_id ?? '') !== (orig?.offer_id ?? '')
      let proofId = c.proof_id
      let proofName = c.proof_name
      if (!proofId && c.offer_id && offerChanged) {
        const key = offers.find(o => o.id === c.offer_id)?.key
        if (key && !proofsError) {
          const eligible = resolveApprovedProofs(proofs, key, cellDomainRoots(c))
          if (eligible.length > 0) {
            proofId = eligible[0].id
            proofName = eligible[0].name
          }
        }
      }
      const item: StageItem = {
        idx: i, property: c.property, slot: c.slot, name: c.name,
        sourceCampaignId: c.source_campaign_id,
        offerId: c.offer_id, offerName: c.offer_name,
        proofId, proofName,
        inclusionSegments: c.inclusion_segments,
        excludeISPs: c.exclude_isps,
        ispCaps: c.isp_caps,
        audienceLabel: c.inclusion_segments?.length
          ? `${c.inclusion_segments.length} segment${c.inclusion_segments.length === 1 ? '' : 's'} override`
          : 'source',
        exempt,
      }
      if (!c.source_campaign_id) item.skipReason = 'no source campaign payload — cannot stage'
      else if (hasBlocker) item.skipReason = 'blocker-level finding on this cell — open it to see the finding'
      else if (!c.name.trim()) item.skipReason = 'no name (apply an offer to generate one)'
      else if (!exempt && !c.offer_id) item.skipReason = 'no offer applied — open the cell and pick one'
      else if (!exempt && offerChanged && !proofId) item.skipReason = proofsError ?? NO_PROOF_MESSAGE
      items.push(item)
    })
    const eligible = items.filter(it => !it.skipReason)
    const anyBlocker = items.some(it => it.skipReason?.includes('blocker'))
    return { items, eligible, anyBlocker }
  }, [cells, byCell, offers, proofs, proofsError, serverGrid])

  const openStage = () => {
    setStage({
      items: stageInfo.items.map(it => ({ ...it })),
      typed: '', running: false, done: false, requestError: null,
    })
  }

  // ONE confirmed POST — the server iterates the cells sequentially and
  // reports per cell; results map back onto the posted order.
  const runStage = async () => {
    if (!stage || !serverGrid || stage.running || stage.done) return
    const items = stage.items.map(it => ({ ...it }))
    const exec = items.filter(it => !it.skipReason)
    setStage(s => (s ? { ...s, running: true, items: items.map(x => ({ ...x })) } : s))
    try {
      const body: Record<string, unknown> = {
        date: serverGrid.date,
        confirmed: true,
        cells: exec.map(it => ({
          property: it.property,
          slot: it.slot,
          name: it.name,
          source_campaign_id: it.sourceCampaignId,
          ...(it.offerId ? { offer_id: it.offerId } : {}),
          ...(it.proofId ? { proof_id: it.proofId } : {}),
          ...(it.inclusionSegments?.length ? { inclusion_segments: it.inclusionSegments } : {}),
          ...(it.excludeISPs?.length ? { exclude_isps: it.excludeISPs } : {}),
          ...(it.ispCaps && Object.keys(it.ispCaps).length ? { isp_caps: it.ispCaps } : {}),
        })),
      }
      if (Object.keys(slotTimes).length > 0) body.slot_times = slotTimes
      if (throttleStrategy) body.throttle_strategy = throttleStrategy
      if (windowHours > 0) body.window_hours = windowHours
      const res = await apiFetch('/api/mailing/board-grid/stage', {
        method: 'POST', body: JSON.stringify(body),
      })
      const raw = await res.text()
      if (!res.ok) {
        setStage(s => (s ? { ...s, running: false, done: true, requestError: `HTTP ${res.status}: ${raw}` } : s))
        return
      }
      let rj: { results?: StageCellResult[] } = {}
      try { rj = JSON.parse(raw) as typeof rj } catch { /* rendered as request error below */ }
      const results = rj.results ?? []
      exec.forEach((it, i) => {
        const r = results[i]
        if (!r) {
          it.result = { kind: 'failed', text: 'no result returned for this cell' }
        } else if (r.status === 'deployed') {
          it.result = { kind: 'deployed', text: `deployed → ${r.campaign_id ?? '?'}` }
        } else if (r.status === 'already_existed') {
          it.result = { kind: 'already_existed', text: `already existed → ${r.campaign_id ?? '?'} (by-name idempotency — converged)` }
        } else {
          it.result = { kind: 'failed', text: `${r.code ?? ''} ${r.error ?? r.status}`.trim() }
        }
      })
      setStage(s => (s ? { ...s, running: false, done: true, items: items.map(x => ({ ...x })) } : s))
    } catch (e) {
      setStage(s => (s ? {
        ...s, running: false, done: true,
        requestError: e instanceof Error ? e.message : 'network error',
      } : s))
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
            Edit each cell's offer + approved proof (and audience if needed), then
            "Schedule proposal" stages the cells as REAL scheduled campaigns on {serverGrid?.date}.
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
              {stageInfo.items.length > 0 && (() => {
                const n = stageInfo.eligible.length
                const reason = n > 0 ? null
                  : stageInfo.anyBlocker ? 'resolve blockers first'
                  : 'cells missing offer/proof'
                return (
                  <button
                    style={{
                      ...btnStyle, background: 'rgba(34,197,94,0.14)',
                      border: '1px solid rgba(34,197,94,0.45)', color: colors.success,
                      opacity: n > 0 ? 1 : 0.55,
                    }}
                    disabled={n === 0 || !!stage}
                    title={reason ?? `stage ${n} proposal cells as campaigns on ${serverGrid.date}`}
                    onClick={openStage}>
                    {n > 0 ? `Schedule proposal (${n} cell${n === 1 ? '' : 's'})`
                      : `Schedule proposal — ${reason}`}
                  </button>
                )
              })()}
              {cloned && (
                <button style={{ ...btnStyle, background: 'transparent' }}
                  onClick={() => void copyProposal()}>
                  {copied ? 'Copied ✓' : 'Copy proposal JSON'}
                </button>
              )}
            </div>
          </div>

          {gateMsg && (
            <div style={{
              marginBottom: 12, fontSize: 12, fontWeight: 700,
              color: (summary.blocker ?? 0) > 0 ? colors.danger
                : (summary.warn ?? 0) > 0 ? colors.warning : colors.success,
            }}>
              {gateMsg}
            </div>
          )}
          {newCellError && (
            <div style={{ ...panelStyle, marginBottom: 12, color: colors.danger, fontSize: 12 }}>
              {newCellError}
              <button style={{ ...btnStyle, background: 'transparent', marginLeft: 10, padding: '2px 8px' }}
                onClick={() => setNewCellError(null)}>dismiss</button>
            </div>
          )}

          {/* ── Board timing & throttle — BOARD-WIDE by operator ruling ──── */}
          {stageInfo.items.length > 0 && (
            <div style={{ ...panelStyle, marginBottom: 12, display: 'flex', gap: 18, alignItems: 'flex-end', flexWrap: 'wrap' }}>
              <div style={{ fontSize: 11, color: colors.textMuted, textTransform: 'uppercase', fontWeight: 700 }}>
                Board timing &amp; throttle
                <div style={{ fontSize: 10, fontWeight: 400, textTransform: 'none', marginTop: 2 }}>
                  applies to the ENTIRE board · edit a column's time in its header
                </div>
              </div>
              <label style={{ fontSize: 12, color: colors.textMuted }}>
                Throttle strategy<br />
                <select style={{ ...inputStyle, minWidth: 190 }}
                  value={throttleStrategy} onChange={e => setThrottleStrategy(e.target.value)}>
                  <option value="">keep source (gentle)</option>
                  <option value="gentle">gentle</option>
                  <option value="auto">auto</option>
                  <option value="even">even</option>
                </select>
              </label>
              <label style={{ fontSize: 12, color: colors.textMuted }}>
                Window hours<br />
                <input type="number" min={0} max={16} style={{ ...inputStyle, width: 90 }}
                  value={windowHours || ''}
                  placeholder="source"
                  onChange={e => {
                    const v = Math.trunc(Number(e.target.value))
                    setWindowHours(Number.isFinite(v) && v > 0 ? Math.min(v, 16) : 0)
                  }} />
              </label>
              <div style={{ fontSize: 11, color: colors.textMuted, maxWidth: 380, lineHeight: 1.5 }}>
                Untouched = <b>gentle · per-cell source window</b>. A window collapses every
                plan's spans to [slot, slot+window]; remapped columns are marked in the headers.
              </div>
              {Object.keys(slotTimes).length > 0 && (
                <button style={{ ...btnStyle, background: 'transparent' }}
                  onClick={() => setSlotTimes({})}>
                  Reset column times
                </button>
              )}
            </div>
          )}

          <div style={{ marginBottom: 12, fontSize: 12, color: colors.textMuted }}>
            Click a cell to open its editor shelf: details, findings, the offer picker, the
            offer's APPROVED proof (its subject/preheader shown), and an optional audience
            override. Click an EMPTY cell (＋) to schedule a new campaign in that slot from the
            property's most recent payload. Edits are LOCAL and gates auto re-run on apply —
            until "Schedule proposal" stages the proposal cells as real campaigns, or (live
            grids) the shelf's MAKE IT REAL / toolbar batch rebuild cancels + redeploys.
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
                    {serverGrid.slots.map(s => (
                      <th key={s} style={hdr}>
                        {(cloned || stageInfo.items.length > 0) ? (
                          <>
                            {/* Editable column time: remaps this slot for the
                                WHOLE proposal (board-level timing ruling). */}
                            <input type="time" value={slotTimes[s] ?? s}
                              title={`Denver fire time for the ${s} column — remapping applies to every cell in it`}
                              onChange={e => {
                                const v = e.target.value
                                setSlotTimes(prev => {
                                  const next = { ...prev }
                                  if (!v || v === s) delete next[s]
                                  else next[s] = v
                                  return next
                                })
                              }}
                              style={{ ...inputStyle, marginTop: 0, padding: '2px 4px', fontSize: 11, width: 92 }} />
                            {slotTimes[s] && (
                              <div style={{ fontSize: 9, color: colors.warning, textTransform: 'none' }}>
                                column was {s}
                              </div>
                            )}
                          </>
                        ) : s}
                      </th>
                    ))}
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
                                  {c.status && (
                                    <span style={c.status === 'failed' ? { color: colors.danger, fontWeight: 700 } : undefined}>
                                      {` · ${c.status}`}
                                    </span>
                                  )}
                                  {/* Multi-campaign slots are sanctioned: add
                                      another campaign at this same anchor. */}
                                  <button type="button"
                                    title={`Add another ${p} campaign at ${s}`}
                                    onClick={e => { e.stopPropagation(); void addNewCell(p, s) }}
                                    style={{
                                      background: 'none', border: 'none', color: colors.textMuted,
                                      cursor: 'pointer', fontSize: 12, padding: '0 4px', marginLeft: 4,
                                    }}>
                                    ＋
                                  </button>
                                </div>
                              </div>
                            ) : (
                              /* EMPTY cell: click to schedule a NEW campaign
                                 in this (property, slot) — payload = the
                                 property's most recent campaign with a config
                                 blob (clone-candidates). */
                              <button type="button"
                                title={`Schedule a new ${p} campaign at ${s}`}
                                onClick={() => void addNewCell(p, s)}
                                style={{
                                  background: 'none', border: `1px dashed ${colors.panelBorder}`,
                                  borderRadius: 6, color: colors.textMuted, cursor: 'pointer',
                                  padding: '2px 12px', fontSize: 13, lineHeight: 1.4,
                                }}>
                                ＋
                              </button>
                            )}
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
                        {FIX_HINTS[f.code] && (
                          <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 2 }}>
                            Fix: {FIX_HINTS[f.code]}
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
              onApplyAudience={applyAudience}
              onApplyISPControls={applyISPControls}
              onAttached={() => { attachedRef.current = true }}
              onRebuilt={handleRebuilt}
              onClose={closeShelf}
            />
          )}
        </>
      )}

      {/* Schedule-proposal dialog — outside the grid gate so results survive
          the post-schedule reload. */}
      {stage && serverGrid && (() => {
        const exec = stage.items.filter(it => !it.skipReason)
        const confirmPhrase = `SCHEDULE ${exec.length}`
        const remaps = Object.entries(slotTimes)
        return (
          <div style={overlayStyle}>
            <div style={modalStyle}>
              <div style={{ ...panelTitleStyle, color: colors.success }}>
                Schedule proposal — {serverGrid.date}
              </div>
              <div style={{ fontSize: 12, color: colors.textMuted, marginBottom: 8, lineHeight: 1.5 }}>
                Each cell deploys as a REAL campaign through the full gated deploy path
                (audience planned at deploy). Source payload = each cell's source campaign;
                offer + approved proof override it. Skipped cells are reported, not silently
                dropped. Re-posting the same proposal converges by name (already-existed).
              </div>
              <div style={{ fontSize: 11, color: colors.text, marginBottom: 10, lineHeight: 1.6 }}>
                <b>Board-wide:</b>{' '}
                throttle {throttleStrategy || 'keep source (gentle)'} ·{' '}
                window {windowHours > 0 ? `${windowHours}h` : 'per-cell source window'} ·{' '}
                {remaps.length > 0
                  ? <>columns remapped: {remaps.map(([from, to]) => `${from}→${to}`).join(', ')}</>
                  : 'no column remaps'}
              </div>
              <div style={{ maxHeight: 340, overflowY: 'auto', marginBottom: 12 }}>
                <table style={{ borderCollapse: 'collapse', width: '100%', fontSize: 12 }}>
                  <thead>
                    <tr>
                      <th style={hdr}>Property</th>
                      <th style={hdr}>Slot</th>
                      <th style={hdr}>Name</th>
                      <th style={hdr}>Offer</th>
                      <th style={hdr}>Proof</th>
                      <th style={hdr}>Audience</th>
                      <th style={hdr}>ISP controls</th>
                      <th style={hdr}>Target (Denver)</th>
                      <th style={hdr}>Result</th>
                    </tr>
                  </thead>
                  <tbody>
                    {stage.items.map(it => (
                      <tr key={`${it.property}|${it.slot}|${it.idx}`}>
                        <td style={{ ...cellTd, whiteSpace: 'nowrap' }}>{it.property}</td>
                        <td style={{ ...cellTd, whiteSpace: 'nowrap' }}>{it.slot}</td>
                        <td style={cellTd}>{it.name || '—'}</td>
                        <td style={cellTd}>{it.offerName || (it.exempt ? '(offer-exempt)' : '—')}</td>
                        {/* No proof = the source blob's creative rides along
                            byte-faithful (unchanged clone cells, exempt cells). */}
                        <td style={cellTd}>{it.proofName || '(source creative)'}</td>
                        <td style={{ ...cellTd, whiteSpace: 'nowrap' }}>{it.audienceLabel}</td>
                        <td style={{ ...cellTd, fontSize: 11 }}>
                          {[
                            ...(it.excludeISPs?.length ? [`−${it.excludeISPs.join(',−')}`] : []),
                            ...Object.entries(it.ispCaps ?? {}).map(([k, v]) => `${k}≤${v.toLocaleString()}`),
                          ].join(' ') || '—'}
                        </td>
                        <td style={{ ...cellTd, whiteSpace: 'nowrap' }}>
                          {serverGrid.date} {slotTimes[it.slot] ?? it.slot}
                        </td>
                        <td style={{
                          ...cellTd,
                          color: it.skipReason ? colors.warning
                            : it.result?.kind === 'failed' ? colors.danger
                            : it.result ? colors.success
                            : colors.textMuted,
                          whiteSpace: 'pre-wrap', wordBreak: 'break-word',
                        }}>
                          {it.skipReason ? `SKIPPED: ${it.skipReason}`
                            : it.result ? `${it.result.kind === 'failed' ? '✗' : '✓'} ${it.result.text}`
                            : stage.running ? '…' : 'pending'}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              {stage.requestError && (
                <div style={halfStateBox}>
                  Stage request FAILED before any per-cell result:{'\n'}{stage.requestError}
                </div>
              )}
              {!stage.running && !stage.done && (
                exec.length > 0 ? (
                  <div style={{ marginTop: 8 }}>
                    <div style={{ fontSize: 11, color: colors.textMuted, marginBottom: 4 }}>
                      Type <b style={{ color: colors.success }}>{confirmPhrase}</b> to execute —
                      this CREATES {exec.length} scheduled campaign{exec.length === 1 ? '' : 's'} on {serverGrid.date}.
                      Nothing is cancelled or modified.
                    </div>
                    <input style={{ ...inputStyle, width: 220 }}
                      placeholder={confirmPhrase}
                      value={stage.typed}
                      onChange={e => {
                        const v = e.target.value
                        setStage(s => (s ? { ...s, typed: v } : s))
                      }} />
                    <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
                      <button
                        style={{
                          ...btnStyle, background: 'rgba(34,197,94,0.14)',
                          border: '1px solid rgba(34,197,94,0.45)', color: colors.success,
                          opacity: stage.typed === confirmPhrase ? 1 : 0.5,
                        }}
                        disabled={stage.typed !== confirmPhrase}
                        onClick={() => void runStage()}>
                        Schedule {exec.length} campaign{exec.length === 1 ? '' : 's'} — LIVE
                      </button>
                      <button style={{ ...btnStyle, background: 'transparent' }}
                        onClick={() => setStage(null)}>
                        Cancel
                      </button>
                    </div>
                  </div>
                ) : (
                  <div style={{ marginTop: 8 }}>
                    <div style={{ fontSize: 12, color: colors.danger, marginBottom: 8 }}>
                      Nothing schedulable — every proposal cell was skipped (reasons above).
                    </div>
                    <button style={{ ...btnStyle, background: 'transparent' }}
                      onClick={() => setStage(null)}>
                      Close
                    </button>
                  </div>
                )
              )}
              {stage.running && (
                <div style={{ fontSize: 12, color: colors.warning, marginTop: 8 }}>
                  Staging… leave this dialog open.
                </div>
              )}
              {stage.done && (
                <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
                  <button style={btnStyle}
                    onClick={() => { setStage(null); loadActual(serverGrid.date) }}>
                    Load {serverGrid.date} actual
                  </button>
                  <button style={{ ...btnStyle, background: 'transparent' }}
                    onClick={() => setStage(null)}>
                    Close
                  </button>
                </div>
              )}
            </div>
          </div>
        )
      })()}

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

// Actionable next step per finding code — every path starts on THIS screen
// (the old hint sent the operator to Day Cards for what the shelf now does).
const FIX_HINTS: Record<string, string> = {
  MISSING_OFFER: 'click the cell → pick an offer → "Attach offer" (LIVE) repairs the deployed row; or cancel it from the shelf and re-stage.',
  OFFER_PENDING: 'nothing to do yet — the audience finalizer writes offer_id last; re-check once the row reaches "scheduled".',
  FAILED_CAMPAIGN: 'the row is inert (failed rows cannot be cancelled) — re-stage this slot via clone or the ＋ cell; the reason is in the message and the cell shelf.',
  SILENT_ZERO: 'open the cell → "Cancel campaign", then re-stage the slot — a scheduled row with 0 recipients dispatches nothing.',
  STUCK_FINALIZE: 'wait for the finalizer or, if it persists, cancel from the shelf and re-stage; check Worker Health if several rows stick.',
  SLOT_COLLISION: 'open the cell (both campaigns are listed in its shelf) and cancel the one that should not fire.',
  REPEAT_OFFER: 'swap one of the repeated cells to a different offer, or cancel the extra from its shelf.',
  LIQUID_SUBJECT: 'the subject renders broken for recipients without personalization data — rebuild the cell with a proof whose subject is plain or uses the safe default-filter idiom.',
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
