// BoardCellShelf — the slide-out editor for one Board Grid cell.
//
// Replaces the old inline <select> cell editor. Shows the cell's identity,
// the gate findings scoped to this (property, slot), and a searchable offer
// picker whose "Apply to grid" edits the LOCAL working grid (BoardGrid's
// applyOffer — which auto re-runs the gates). Nothing here deploys, with TWO
// labeled exceptions:
//   1. attach-offer LIVE — repairs an already deployed campaign that finalized
//      without an offer_id (dry preview → typed-name confirm → confirmed POST,
//      suppression caveat rendered verbatim).
//   2. MAKE IT REAL — after an offer is applied locally to a LIVE cell, drives
//      the existing Day Cards rebuild endpoint (cancel + redeploy with
//      overrides {offer_id, proof_id}): dry preview (confirmed:false) → typed
//      -name confirm → confirmed:true. Proof is auto-resolved from the offer's
//      APPROVED proofs (Creative Studio) — no approved proof, no button.

import React, { useEffect, useMemo, useState } from 'react'
import { apiFetch } from '../shared/apiFetch'
import { colors } from '../shared/theme'
import { SideShelf } from './shared/SideShelf'

// ── Shared shapes (BoardGrid imports these — single definition, no cycle:
//    BoardGrid imports values from here, never the reverse) ─────────────────

export interface BoardCell {
  property: string
  property_label?: string
  sending_domain?: string
  brand_root?: string
  slot: string
  campaign_id?: string
  // source_campaign_id: set on clone-proposal cells (and locally on new-cell
  // proposals) — the campaign whose pmta_config blob /board-grid/stage loads
  // as the deploy payload. A proposal without it cannot be scheduled.
  source_campaign_id?: string
  name: string
  offer_id?: string
  offer_name?: string
  // Local working-cell fields (proposal editing) — the server ignores them.
  proof_id?: string
  proof_name?: string
  inclusion_segments?: string[]        // audience override (segment ids)
  inclusion_segment_names?: string[]   // parallel display names
  new_cell?: boolean                   // materialized from an empty grid cell
  subject?: string
  status?: string
  recipients: number
  proposed?: boolean
}
export interface BoardFindingRow {
  level: 'blocker' | 'warn'
  code: string
  property: string
  slot: string
  message: string
}
export interface OfferOpt { id: string; name: string; key?: string }

// One row of GET /api/mailing/offer-proofs?status=approved&active=true.
export interface OfferProof {
  id: string
  name: string
  offer_key?: string
  approval_status?: string
  is_active?: boolean
  variants?: Array<{ subject?: string; preheader?: string }>
  from_names?: string[]
  approved_domains?: string[]
  created_at?: string
}

export const NO_PROOF_MESSAGE =
  'offer has no approved proof — not board-ready (three-pool rule); pick another offer or approve a proof in Creative Studio'

// Domain identities a proof's approved_domains list may carry for this cell:
// the brand root, the sending domain, and the sending domain stripped of its
// em. prefix (root derived when brand_root is absent).
export const cellDomainRoots = (cell: BoardCell): string[] => {
  const out: string[] = []
  if (cell.brand_root) out.push(cell.brand_root)
  if (cell.sending_domain) {
    out.push(cell.sending_domain)
    out.push(cell.sending_domain.replace(/^em\./i, ''))
  }
  return [...new Set(out)]
}

// Proof resolution for a live rebuild: proof.offer_key must equal the offer's
// key, and when the proof declares approved_domains (non-empty) the cell's
// sending-domain root must be in that list. Newest first — created_at when
// present, id (numeric-aware) as the tiebreak.
export const resolveApprovedProofs = (
  proofs: OfferProof[], offerKey: string, cellDomains: string[],
): OfferProof[] =>
  proofs
    .filter(p => p.offer_key === offerKey)
    .filter(p => {
      const doms = p.approved_domains ?? []
      return doms.length === 0 || cellDomains.some(d => doms.includes(d))
    })
    .sort((a, b) =>
      (b.created_at ?? '').localeCompare(a.created_at ?? '') ||
      b.id.localeCompare(a.id, undefined, { numeric: true }))

// Mirror of the server's isOfferExemptName (offer_gate.go): KUMO-WARM /
// newsletter cells carry NO offer BY DOCTRINE.
export const isOfferExemptName = (name: string): boolean => {
  const n = name.toUpperCase()
  return n.includes('KUMO-WARM') || n.includes('NEWSLETTER')
}

interface AttachPreview {
  campaign?: { id?: string; name?: string; status?: string; offer_id?: string; offer_key?: string }
  offer?: { id?: string; name?: string; key?: string; status?: string }
  suppression_caveat?: string
}
interface AttachResult {
  offer_name?: string
  offer_key?: string
  suppression_caveat?: string
}

// POST /api/mailing/pmta-campaign/day-cards/rebuild shapes.
interface RebuildPreview {
  would_cancel?: unknown
  new_input_summary?: unknown
  partial_send_warning?: string
  offer_gate_warning?: string
}
interface RebuildResult {
  cancelled_campaign_id?: string
  new_campaign_id?: string
  sent_before_cancel?: number
  note?: string
}

// Server text is rendered VERBATIM — strings as-is, structures as pretty JSON.
const verbatim = (v: unknown): string =>
  v === undefined || v === null ? '—' : typeof v === 'string' ? v : JSON.stringify(v, null, 2)

const label: React.CSSProperties = {
  fontSize: 10, letterSpacing: 0.6, textTransform: 'uppercase', color: colors.textMuted,
}
const sectionTitle: React.CSSProperties = {
  fontSize: 11, color: colors.text, fontWeight: 700, letterSpacing: 0.5, margin: '18px 0 8px 0',
  textTransform: 'uppercase',
}
const input: React.CSSProperties = {
  background: colors.panelBgSolid, color: colors.text,
  border: `1px solid ${colors.panelBorder}`, borderRadius: 6, padding: '6px 8px', fontSize: 12,
}
const smallBtn: React.CSSProperties = {
  background: 'rgba(99,102,241,0.20)', color: colors.text,
  border: '1px solid rgba(99,102,241,0.45)', borderRadius: 6,
  padding: '5px 12px', fontSize: 12, fontWeight: 700, cursor: 'pointer',
}
const dangerBtn: React.CSSProperties = {
  ...smallBtn, background: 'rgba(239,68,68,0.14)', border: '1px solid rgba(239,68,68,0.45)',
  color: colors.danger,
}
const errorChip: React.CSSProperties = {
  display: 'inline-block', fontSize: 11, fontWeight: 700, color: colors.danger,
  background: 'rgba(239,68,68,0.12)', border: '1px solid rgba(239,68,68,0.40)',
  borderRadius: 6, padding: '5px 10px',
}
const caveatBox: React.CSSProperties = {
  fontSize: 11, color: colors.warning, background: 'rgba(245,158,11,0.10)',
  border: '1px solid rgba(245,158,11,0.40)', borderRadius: 6, padding: '7px 10px',
  marginTop: 6, lineHeight: 1.5, whiteSpace: 'pre-wrap',
}
const preBox: React.CSSProperties = {
  margin: '2px 0 8px 0', padding: '6px 8px', background: colors.panelBgSolid,
  border: `1px solid ${colors.panelBorder}`, borderRadius: 6, fontSize: 10,
  whiteSpace: 'pre-wrap', wordBreak: 'break-word', color: colors.text,
  maxHeight: 220, overflowY: 'auto',
}
// The 502 half-state (old campaign possibly cancelled, new deploy failed) is
// the loudest thing on the screen. Exported — BoardGrid's batch dialog uses it.
export const halfStateBox: React.CSSProperties = {
  fontSize: 12, color: colors.danger, background: 'rgba(239,68,68,0.14)',
  border: '2px solid rgba(239,68,68,0.70)', borderRadius: 8, padding: '10px 12px',
  marginTop: 8, lineHeight: 1.6, whiteSpace: 'pre-wrap', fontWeight: 700,
}

const statusColor = (s?: string): string => {
  if (!s) return colors.textMuted
  if (['failed', 'cancelled', 'deleted'].includes(s)) return colors.danger
  if (['sent', 'completed'].includes(s)) return colors.success
  if (['sending', 'preparing', 'finalizing_audience'].includes(s)) return colors.warning
  return colors.textMuted
}

// One row of GET /api/mailing/segments (the DayCardEditor picker's endpoint).
interface SegmentOpt { id: string; name: string; subscriber_count?: number }

export const BoardCellShelf: React.FC<{
  date: string
  entries: Array<{ idx: number; cell: BoardCell }>  // all campaigns at this (property, slot)
  activeIdx: number
  onSelectEntry: (idx: number) => void
  findings: BoardFindingRow[]                        // whole-grid findings; filtered here
  offers: OfferOpt[]
  offersError: string | null
  proofs: OfferProof[]                               // approved+active proofs (fetched once by BoardGrid)
  proofsError: string | null
  edited: boolean
  gating: boolean
  cloneMode: boolean
  // Proposal apply: extras carry the picked approved proof (clone/new-cell
  // mode). Absent extras on a proposal apply CLEARS any stored proof.
  onApplyOffer: (idx: number, offerId: string, extras?: { proofId: string; proofName: string }) => void
  // Audience override apply (proposal cells): null clears the override back
  // to "source audience carried over".
  onApplyAudience: (idx: number, segments: SegmentOpt[] | null) => void
  onAttached: () => void                             // a confirmed LIVE attach landed
  onRebuilt: () => void                              // a confirmed LIVE rebuild landed
  onClose: () => void
}> = ({ date, entries, activeIdx, onSelectEntry, findings, offers, offersError,
        proofs, proofsError, edited, gating, cloneMode, onApplyOffer, onApplyAudience,
        onAttached, onRebuilt, onClose }) => {
  const entry = entries.find(e => e.idx === activeIdx) ?? entries[0]
  const cell = entry?.cell

  const [query, setQuery] = useState('')
  const [selOffer, setSelOffer] = useState('')
  const [attach, setAttach] = useState<{
    phase: 'idle' | 'previewing' | 'preview' | 'confirming' | 'done'
    preview?: AttachPreview
    result?: AttachResult
    error?: string
  }>({ phase: 'idle' })
  const [typedName, setTypedName] = useState('')

  // ── MAKE IT REAL (live rebuild) state ─────────────────────────────────────
  const [selProof, setSelProof] = useState('')
  const [rebuild, setRebuild] = useState<{
    phase: 'idle' | 'previewing' | 'preview' | 'confirming' | 'done' | 'halfstate'
    preview?: RebuildPreview
    result?: RebuildResult
    halfStateText?: string
    error?: string
  }>({ phase: 'idle' })
  const [rebuildTyped, setRebuildTyped] = useState('')

  // ── PROPOSAL (clone / new-cell) state ────────────────────────────────────
  // A proposal cell is editable toward /board-grid/stage: it needs an offer
  // AND that offer's APPROVED proof (Creative Studio) picked here, plus an
  // optional audience override. proposalMode also covers new cells
  // materialized from an empty grid slot on a non-clone grid.
  const proposalMode = cloneMode || !!cell?.proposed
  const [selCloneProof, setSelCloneProof] = useState('')
  const [audOpen, setAudOpen] = useState(false)
  const [audSel, setAudSel] = useState<SegmentOpt[]>([])
  const [audQuery, setAudQuery] = useState('')
  const [segPool, setSegPool] = useState<SegmentOpt[] | null>(null)
  const [segError, setSegError] = useState<string | null>(null)

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    return q ? offers.filter(o => o.name.toLowerCase().includes(q)) : offers
  }, [offers, query])

  // Prefill the picker with the proposal cell's applied offer so its proof
  // (and copy) are visible on reopen. Proposal-only: the live attach flow
  // must keep its explicit empty start.
  useEffect(() => {
    if (proposalMode) setSelOffer(cell?.offer_id ?? '')
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [proposalMode, cell?.property, cell?.slot, cell?.source_campaign_id, cell?.campaign_id])

  // The PICKED offer's approved proofs, scoped to this cell's domain roots —
  // the same resolution the live-rebuild path uses.
  const selOfferKey = offers.find(o => o.id === selOffer)?.key
  const cloneProofs = useMemo(() => {
    if (!proposalMode || !selOffer || !selOfferKey || !cell) return []
    return resolveApprovedProofs(proofs, selOfferKey, cellDomainRoots(cell))
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [proposalMode, selOffer, selOfferKey, proofs, cell?.brand_root, cell?.sending_domain])

  // Auto-select: keep a still-valid manual pick, else the cell's stored
  // proof, else the newest ([0]).
  useEffect(() => {
    setSelCloneProof(prev =>
      cloneProofs.some(p => p.id === prev) ? prev
        : cloneProofs.some(p => p.id === cell?.proof_id) ? (cell?.proof_id ?? '')
        : (cloneProofs[0]?.id ?? ''))
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cloneProofs, cell?.proof_id])

  // Audience override state follows the cell under the shelf.
  useEffect(() => {
    const ids = cell?.inclusion_segments ?? []
    const names = cell?.inclusion_segment_names ?? []
    setAudSel(ids.map((id, i) => ({ id, name: names[i] ?? id })))
    setAudOpen(ids.length > 0)
    setAudQuery('')
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cell?.property, cell?.slot, cell?.campaign_id, cell?.source_campaign_id])

  // Segment pool, fetched lazily on the first override toggle — the same
  // endpoint + shape the Day Cards editor picker reads.
  useEffect(() => {
    if (!audOpen || segPool !== null) return
    void (async () => {
      try {
        const res = await apiFetch('/api/mailing/segments')
        if (!res.ok) throw new Error(`segments: HTTP ${res.status}`)
        const j: { segments?: SegmentOpt[] } = await res.json()
        setSegPool(j.segments ?? [])
        setSegError(null)
      } catch (e) {
        setSegPool([])
        setSegError(e instanceof Error ? e.message : 'network error')
      }
    })()
  }, [audOpen, segPool])

  const audMatches = useMemo(() => {
    if (!segPool || audQuery.trim().length < 2) return []
    const needle = audQuery.trim().toLowerCase()
    const chosen = new Set(audSel.map(s => s.id))
    return segPool
      .filter(p => !chosen.has(p.id) && (p.name.toLowerCase().includes(needle) || p.id === audQuery.trim()))
      .slice(0, 12)
  }, [segPool, audQuery, audSel])

  // The applied offer's approved proofs, scoped to this cell's domain roots.
  const offerKey = offers.find(o => o.id === cell?.offer_id)?.key
  const rebuildProofs = useMemo(() => {
    if (!cell?.offer_id || !offerKey) return []
    return resolveApprovedProofs(proofs, offerKey, cellDomainRoots(cell))
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cell?.offer_id, cell?.brand_root, cell?.sending_domain, offerKey, proofs])

  // Preselect the newest ([0]) proof; keep a still-valid manual pick.
  useEffect(() => {
    setSelProof(prev => (rebuildProofs.some(p => p.id === prev) ? prev : (rebuildProofs[0]?.id ?? '')))
  }, [rebuildProofs])

  // A different campaign/offer under the shelf invalidates any preview.
  useEffect(() => {
    setRebuild({ phase: 'idle' })
    setRebuildTyped('')
  }, [cell?.campaign_id, cell?.offer_id])

  if (!cell) return null

  const cellFindings = findings.filter(f => f.property === cell.property && f.slot === cell.slot)
  const liveActionable = !cloneMode && !!cell.campaign_id && !cell.proposed
  const attachEligible = liveActionable && !cell.offer_id && !isOfferExemptName(cell.name)

  const postAttach = async (confirmed: boolean) => {
    if (!cell.campaign_id || !selOffer) return
    setAttach(a => ({ ...a, phase: confirmed ? 'confirming' : 'previewing', error: undefined }))
    try {
      const res = await apiFetch(`/api/mailing/pmta-campaign/${cell.campaign_id}/attach-offer`, {
        method: 'POST',
        body: JSON.stringify({ offer_id: selOffer, confirmed }),
      })
      const body: Record<string, unknown> = await res.json().catch(() => ({}))
      if (!res.ok) {
        setAttach(a => ({
          ...a,
          phase: confirmed ? 'preview' : 'idle',
          error: `HTTP ${res.status}: ${String(body.error ?? 'attach failed')}`,
        }))
        return
      }
      if (!confirmed) {
        setAttach({ phase: 'preview', preview: body as AttachPreview })
      } else {
        setAttach({ phase: 'done', result: body as AttachResult })
        onAttached()
      }
    } catch (e) {
      setAttach(a => ({
        ...a,
        phase: confirmed ? 'preview' : 'idle',
        error: e instanceof Error ? e.message : 'network error',
      }))
    }
  }

  // Drives the EXISTING Day Cards rebuild endpoint. confirmed:false = dry
  // preview; confirmed:true is only reachable behind the typed-name confirm.
  const postRebuild = async (confirmed: boolean) => {
    if (!cell.campaign_id || !cell.offer_id || !selProof) return
    setRebuild(r => ({ ...r, phase: confirmed ? 'confirming' : 'previewing', error: undefined }))
    try {
      const res = await apiFetch('/api/mailing/pmta-campaign/day-cards/rebuild', {
        method: 'POST',
        body: JSON.stringify({
          campaign_id: cell.campaign_id,
          confirmed,
          overrides: { offer_id: cell.offer_id, proof_id: selProof },
        }),
      })
      const raw = await res.text()
      let body: Record<string, unknown> = {}
      try { body = JSON.parse(raw) as Record<string, unknown> } catch { /* non-JSON body — raw is rendered verbatim */ }
      if (res.status === 502) {
        // Half-state: the old campaign may be cancelled with the new deploy
        // failed. Rendered verbatim + prominent; no silent retry.
        setRebuild({ phase: 'halfstate', halfStateText: raw })
        return
      }
      if (!res.ok) {
        setRebuild(r => ({
          ...r,
          phase: confirmed ? 'preview' : 'idle',
          error: `HTTP ${res.status}: ${String(body.error ?? raw ?? 'rebuild failed')}`,
        }))
        return
      }
      if (!confirmed) {
        setRebuild({ phase: 'preview', preview: body as RebuildPreview })
      } else {
        setRebuild({ phase: 'done', result: body as RebuildResult })
        onRebuilt()
      }
    } catch (e) {
      setRebuild(r => ({
        ...r,
        phase: confirmed ? 'preview' : 'idle',
        error: e instanceof Error ? e.message : 'network error',
      }))
    }
  }

  return (
    <SideShelf title={`${cell.property} · ${cell.slot}`} width={560} onClose={onClose}>
      <div style={{ padding: '14px 20px 28px 20px' }}>

        {entries.length > 1 && (
          <div style={{ marginBottom: 12 }}>
            <div style={label}>{entries.length} campaigns at this anchor</div>
            {entries.map(e => (
              <button key={e.idx} type="button"
                onClick={() => onSelectEntry(e.idx)}
                style={{
                  ...smallBtn, display: 'block', width: '100%', textAlign: 'left', marginTop: 4,
                  background: e.idx === entry.idx ? 'rgba(99,102,241,0.28)' : 'transparent',
                  fontWeight: e.idx === entry.idx ? 700 : 400,
                }}>
                {e.cell.name}
              </button>
            ))}
          </div>
        )}

        {/* ── CELL ─────────────────────────────────────────────────────── */}
        <div style={{ ...sectionTitle, marginTop: 0 }}>Cell</div>
        <div style={{ display: 'grid', gridTemplateColumns: '110px 1fr', rowGap: 6, columnGap: 10, fontSize: 12 }}>
          <span style={label}>Name</span>
          <span style={{ color: colors.text }}>{cell.name}</span>
          <span style={label}>Status</span>
          <span>
            <span style={{
              display: 'inline-block', fontSize: 10, fontWeight: 700, textTransform: 'uppercase',
              color: statusColor(cell.status), border: `1px solid ${statusColor(cell.status)}`,
              borderRadius: 8, padding: '1px 8px',
            }}>
              {cell.status || (cell.proposed ? 'proposed' : 'unknown')}
            </span>
            {edited && <span style={{ marginLeft: 8, fontSize: 10, fontWeight: 700, color: colors.warning }}>EDITED (local)</span>}
          </span>
          <span style={label}>Recipients</span>
          <span style={{ color: colors.text, fontVariantNumeric: 'tabular-nums' }}>
            {cell.recipients ? cell.recipients.toLocaleString() : '—'}
          </span>
          <span style={label}>Subject</span>
          <span title={cell.subject || undefined}
            style={{ color: colors.textMuted, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
            {cell.subject || '—'}
          </span>
          <span style={label}>Sending domain</span>
          <span style={{ color: colors.text }}>{cell.sending_domain || '—'}</span>
          <span style={label}>Campaign id</span>
          <span style={{ fontFamily: 'monospace', fontSize: 10, color: colors.textMuted }}>
            {cell.campaign_id || (cell.proposed ? '(proposal — not created)' : '—')}
          </span>
        </div>

        {/* ── FINDINGS for this cell ───────────────────────────────────── */}
        <div style={sectionTitle}>Findings — this cell</div>
        {cellFindings.length === 0 ? (
          <div style={{ fontSize: 12, color: colors.success }}>Clean — no findings at this anchor.</div>
        ) : (
          cellFindings.map((f, i) => (
            <div key={i} style={{
              fontSize: 12, lineHeight: 1.5, padding: '6px 10px', borderRadius: 6, marginBottom: 6,
              color: f.level === 'blocker' ? colors.danger : colors.warning,
              background: f.level === 'blocker' ? 'rgba(239,68,68,0.10)' : 'rgba(245,158,11,0.08)',
              border: `1px solid ${f.level === 'blocker' ? 'rgba(239,68,68,0.35)' : 'rgba(245,158,11,0.35)'}`,
            }}>
              <b>{f.level === 'blocker' ? '✗' : '⚠'} {f.code}</b>
              <div style={{ color: colors.text, marginTop: 2 }}>{f.message}</div>
            </div>
          ))
        )}

        {/* ── OFFER ────────────────────────────────────────────────────── */}
        <div style={sectionTitle}>Offer</div>
        <div style={{ fontSize: 12, marginBottom: 8 }}>
          <span style={label}>Current: </span>
          {cell.offer_name
            ? <span style={{ color: colors.text }}>{cell.offer_name}</span>
            : <span style={{ color: isOfferExemptName(cell.name) ? colors.textMuted : colors.danger }}>
                {isOfferExemptName(cell.name) ? 'none (offer-exempt by doctrine)' : 'no offer'}
              </span>}
          {cell.offer_id && (
            <span style={{ fontFamily: 'monospace', fontSize: 10, color: colors.textMuted, marginLeft: 8 }}>
              {cell.offer_id}
            </span>
          )}
          {proposalMode && cell.proof_name && (
            <div style={{ marginTop: 4 }}>
              <span style={label}>Proof: </span>
              <span style={{ color: colors.text }}>{cell.proof_name}</span>
            </div>
          )}
        </div>

        {offersError ? (
          <div style={errorChip}>{offersError}</div>
        ) : offers.length === 0 ? (
          <div style={{ fontSize: 12, color: colors.textMuted }}>
            No active offers in the catalog — nothing to pick from.
          </div>
        ) : (
          <>
            <input style={{ ...input, width: '100%', boxSizing: 'border-box' }}
              placeholder={`Filter ${offers.length} active offers…`}
              value={query} onChange={e => setQuery(e.target.value)} />
            <div style={{ display: 'flex', gap: 8, marginTop: 6, alignItems: 'center' }}>
              <select style={{ ...input, flex: 1, minWidth: 0 }}
                value={selOffer} onChange={e => setSelOffer(e.target.value)}>
                <option value="">select offer… ({filtered.length} match{filtered.length === 1 ? '' : 'es'})</option>
                {filtered.map(o => <option key={o.id} value={o.id}>{o.name}</option>)}
              </select>
              <button type="button" style={{ ...smallBtn, opacity: selOffer ? 1 : 0.5 }}
                disabled={!selOffer || gating}
                onClick={() => {
                  const chosen = cloneProofs.find(p => p.id === selCloneProof)
                  onApplyOffer(entry.idx, selOffer,
                    proposalMode && chosen ? { proofId: chosen.id, proofName: chosen.name } : undefined)
                }}>
                {gating ? 'Gating…' : 'Apply to grid'}
              </button>
            </div>

            {/* ── PROOF PICKER (proposal cells): the Creative Studio approved
                   proof that will SHIP — its subject/preheader shown so the
                   operator SEES the copy before scheduling. ─────────────── */}
            {proposalMode && selOffer && (
              proofsError ? (
                <div style={{ ...errorChip, marginTop: 8 }}>{proofsError}</div>
              ) : !selOfferKey ? (
                <div style={{ ...errorChip, marginTop: 8 }}>
                  offer key unknown for {selOffer} — cannot resolve approved proofs
                </div>
              ) : cloneProofs.length === 0 ? (
                <div style={{ ...errorChip, marginTop: 8 }}>{NO_PROOF_MESSAGE}</div>
              ) : (
                <div style={{ marginTop: 8 }}>
                  <div style={label}>Approved proof ({cloneProofs.length}, newest first) — ships as the creative</div>
                  {cloneProofs.length > 1 ? (
                    <select style={{ ...input, width: '100%', boxSizing: 'border-box', marginTop: 4 }}
                      value={selCloneProof} onChange={e => setSelCloneProof(e.target.value)}>
                      {cloneProofs.map(p => <option key={p.id} value={p.id}>{p.name}</option>)}
                    </select>
                  ) : (
                    <div style={{ fontSize: 12, color: colors.text, marginTop: 4 }}>{cloneProofs[0].name}</div>
                  )}
                  {(() => {
                    const p = cloneProofs.find(x => x.id === selCloneProof) ?? cloneProofs[0]
                    const v = p?.variants?.[0]
                    return (
                      <div style={{ display: 'grid', gridTemplateColumns: '80px 1fr', rowGap: 4, columnGap: 10, fontSize: 12, marginTop: 6 }}>
                        <span style={label}>Subject</span>
                        <span style={{ color: colors.text }}>{v?.subject || '—'}</span>
                        <span style={label}>Preheader</span>
                        <span style={{ color: colors.textMuted }}>{v?.preheader || '—'}</span>
                        {p?.from_names?.[0] && (<>
                          <span style={label}>From</span>
                          <span style={{ color: colors.textMuted }}>{p.from_names[0]}</span>
                        </>)}
                      </div>
                    )
                  })()}
                </div>
              )
            )}
            <div style={{ fontSize: 10, color: colors.textMuted, marginTop: 4 }}>
              Apply updates the LOCAL working grid and auto re-runs the gates — findings above refresh.
              {proposalMode && ' The picked proof rides with the cell into "Schedule proposal".'}
            </div>
          </>
        )}

        {/* ── AUDIENCE (proposal cells) ─────────────────────────────────── */}
        {proposalMode && (
          <>
            <div style={sectionTitle}>Audience</div>
            {!audOpen ? (
              <div style={{ fontSize: 12, color: colors.textMuted, lineHeight: 1.5 }}>
                Source audience carried over — the source campaign's segments, send
                priority and reserves ride along untouched.
                <div style={{ marginTop: 6 }}>
                  <button type="button" style={smallBtn} onClick={() => setAudOpen(true)}>
                    Override audience
                  </button>
                </div>
              </div>
            ) : (
              <div>
                <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, marginBottom: 6 }}>
                  {audSel.length === 0 && (
                    <span style={{ fontSize: 11, color: colors.textMuted }}>no segments selected yet</span>
                  )}
                  {audSel.map(s => (
                    <span key={s.id} title={s.id} style={{
                      display: 'inline-flex', alignItems: 'center', gap: 6,
                      background: 'rgba(99,102,241,0.14)', border: `1px solid ${colors.panelBorder}`,
                      borderRadius: 6, padding: '2px 8px', fontSize: 11, color: colors.text,
                    }}>
                      {s.name}
                      <button type="button" aria-label={`remove ${s.name}`}
                        onClick={() => setAudSel(prev => prev.filter(x => x.id !== s.id))}
                        style={{ background: 'none', border: 'none', color: colors.textMuted, cursor: 'pointer', padding: 0, fontSize: 12 }}>
                        ×
                      </button>
                    </span>
                  ))}
                </div>
                <input style={{ ...input, width: '100%', boxSizing: 'border-box' }}
                  placeholder={segPool ? `search segments by name (min 2 chars) · ${segPool.length} available` : 'loading segments…'}
                  value={audQuery} onChange={e => setAudQuery(e.target.value)} />
                {segError && <div style={{ ...errorChip, marginTop: 6 }}>{segError}</div>}
                {audMatches.length > 0 && (
                  <div style={{ border: `1px solid ${colors.panelBorder}`, borderRadius: 6, marginTop: 4, maxHeight: 180, overflowY: 'auto' }}>
                    {audMatches.map(m => (
                      <div key={m.id}
                        onClick={() => { setAudSel(prev => [...prev, { id: m.id, name: m.name }]); setAudQuery('') }}
                        style={{ padding: '6px 9px', fontSize: 12, color: colors.text, cursor: 'pointer', borderBottom: `1px solid ${colors.panelBorder}` }}>
                        {m.name}
                        {m.subscriber_count != null && (
                          <span style={{ color: colors.textMuted, marginLeft: 8 }}>{m.subscriber_count.toLocaleString()}</span>
                        )}
                      </div>
                    ))}
                  </div>
                )}
                <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
                  <button type="button" style={{ ...smallBtn, opacity: audSel.length ? 1 : 0.5 }}
                    disabled={audSel.length === 0 || gating}
                    onClick={() => onApplyAudience(entry.idx, audSel)}>
                    Apply audience override ({audSel.length})
                  </button>
                  <button type="button" style={{ ...smallBtn, background: 'transparent' }}
                    onClick={() => { setAudOpen(false); setAudSel([]); onApplyAudience(entry.idx, null) }}>
                    Clear override (keep source audience)
                  </button>
                </div>
                <div style={{ fontSize: 10, color: colors.textMuted, marginTop: 4 }}>
                  An override REPLACES the source's audience program at stage time —
                  its send priority and segment reserves are cleared with it.
                </div>
              </div>
            )}
          </>
        )}

        {/* ── LIVE ACTIONS ─────────────────────────────────────────────── */}
        {liveActionable && (
          <>
            <div style={{ ...sectionTitle, color: colors.danger }}>Live actions</div>
            {attachEligible ? (
              <div style={{ border: '1px solid rgba(239,68,68,0.35)', borderRadius: 8, padding: '10px 12px' }}>
                <div style={{ fontSize: 12, fontWeight: 700, color: colors.danger, marginBottom: 4 }}>
                  Attach offer to live campaign — LIVE WRITE
                </div>
                <div style={{ fontSize: 11, color: colors.textMuted, lineHeight: 1.5, marginBottom: 8 }}>
                  This campaign finalized with no offer_id. Attaching repairs conversion attribution
                  on the deployed row. Pick the offer above, preview first, then confirm by typing
                  the campaign name.
                </div>
                {attach.phase === 'idle' || attach.phase === 'previewing' ? (
                  <button type="button" style={{ ...smallBtn, opacity: selOffer ? 1 : 0.5 }}
                    disabled={!selOffer || attach.phase === 'previewing'}
                    onClick={() => void postAttach(false)}>
                    {attach.phase === 'previewing' ? 'Previewing…' : 'Preview attach (dry run)'}
                  </button>
                ) : attach.phase === 'done' ? (
                  <div style={{ fontSize: 12, color: colors.success }}>
                    Attached <b>{attach.result?.offer_name || selOffer}</b> to the live campaign.
                    {attach.result?.suppression_caveat && (
                      <div style={caveatBox}>{attach.result.suppression_caveat}</div>
                    )}
                    <div style={{ fontSize: 11, color: colors.warning, marginTop: 6 }}>
                      Full correctness (offer suppression subtracted from the audience) requires a
                      Day Cards rebuild of this campaign.
                    </div>
                  </div>
                ) : (
                  <>
                    <div style={{ fontSize: 11, color: colors.text, lineHeight: 1.6 }}>
                      <div style={label}>Dry preview</div>
                      campaign: {attach.preview?.campaign?.name || cell.name} ({attach.preview?.campaign?.status || cell.status})<br />
                      attach offer: <b>{attach.preview?.offer?.name || selOffer}</b>
                      {attach.preview?.offer?.key ? ` (key ${attach.preview.offer.key})` : ''}
                      {attach.preview?.offer?.status ? ` · ${attach.preview.offer.status}` : ''}
                    </div>
                    {attach.preview?.suppression_caveat && (
                      <div style={caveatBox}>{attach.preview.suppression_caveat}</div>
                    )}
                    <div style={{ marginTop: 8 }}>
                      <div style={label}>Type the campaign name exactly to confirm</div>
                      <input style={{ ...input, width: '100%', boxSizing: 'border-box', marginTop: 4 }}
                        placeholder={cell.name}
                        value={typedName} onChange={e => setTypedName(e.target.value)} />
                      <div style={{ display: 'flex', gap: 8, marginTop: 6 }}>
                        <button type="button"
                          style={{ ...dangerBtn, opacity: typedName === cell.name ? 1 : 0.5 }}
                          disabled={typedName !== cell.name || attach.phase === 'confirming'}
                          onClick={() => void postAttach(true)}>
                          {attach.phase === 'confirming' ? 'Attaching…' : 'Confirm LIVE attach'}
                        </button>
                        <button type="button" style={{ ...smallBtn, background: 'transparent' }}
                          onClick={() => { setAttach({ phase: 'idle' }); setTypedName('') }}>
                          Cancel
                        </button>
                      </div>
                    </div>
                  </>
                )}
                {attach.error && <div style={{ ...errorChip, marginTop: 8 }}>{attach.error}</div>}
              </div>
            ) : (
              <div style={{ fontSize: 11, color: colors.textMuted, lineHeight: 1.5 }}>
                {cell.offer_id
                  ? 'This campaign already carries an offer — no attach repair applies. To CHANGE it: apply a different offer above, then use MAKE IT REAL below to cancel + rebuild the live campaign.'
                  : 'This cell is offer-exempt (KUMO-WARM / newsletter) — no offer belongs here by doctrine.'}
              </div>
            )}

            {/* ── MAKE IT REAL — rebuild the live campaign with the locally
                   applied offer + its auto-resolved approved proof ─────────── */}
            {edited && cell.offer_id && (
              <div style={{ border: '1px solid rgba(239,68,68,0.35)', borderRadius: 8, padding: '10px 12px', marginTop: 10 }}>
                <div style={{ fontSize: 12, fontWeight: 700, color: colors.danger, marginBottom: 4 }}>
                  MAKE IT REAL — rebuild live campaign
                </div>
                <div style={{ fontSize: 11, color: colors.textMuted, lineHeight: 1.5, marginBottom: 8 }}>
                  Cancels the deployed campaign and redeploys it with the offer applied
                  above (<b style={{ color: colors.text }}>{cell.offer_name || cell.offer_id}</b>) and its approved
                  proof — the proof's html becomes the creative; its first subject / from-name fill in
                  automatically unless overridden. Preview first, then confirm by typing the campaign name.
                </div>
                {proofsError ? (
                  <div style={errorChip}>{proofsError}</div>
                ) : !offerKey ? (
                  <div style={errorChip}>offer key unknown for {cell.offer_id} — cannot resolve approved proofs</div>
                ) : rebuildProofs.length === 0 ? (
                  <div style={errorChip}>{NO_PROOF_MESSAGE}</div>
                ) : (
                  <>
                    <div style={{ fontSize: 11, marginBottom: 8 }}>
                      <span style={label}>Approved proof ({rebuildProofs.length}, newest first): </span>
                      {rebuildProofs.length > 1 ? (
                        <select style={{ ...input, width: '100%', boxSizing: 'border-box', marginTop: 4 }}
                          value={selProof}
                          onChange={e => {
                            setSelProof(e.target.value)
                            setRebuild({ phase: 'idle' })
                            setRebuildTyped('')
                          }}>
                          {rebuildProofs.map(p => <option key={p.id} value={p.id}>{p.name}</option>)}
                        </select>
                      ) : (
                        <span style={{ color: colors.text }}>{rebuildProofs[0].name}</span>
                      )}
                    </div>
                    {rebuild.phase === 'idle' || rebuild.phase === 'previewing' ? (
                      <button type="button" style={{ ...smallBtn, opacity: selProof ? 1 : 0.5 }}
                        disabled={!selProof || rebuild.phase === 'previewing'}
                        onClick={() => void postRebuild(false)}>
                        {rebuild.phase === 'previewing' ? 'Previewing…' : 'Preview live rebuild (dry run)'}
                      </button>
                    ) : rebuild.phase === 'halfstate' ? (
                      <div style={halfStateBox}>
                        HALF-STATE (HTTP 502): the old campaign may be CANCELLED with the new deploy
                        FAILED. Verify in Day Cards before retrying.{'\n\n'}{rebuild.halfStateText}
                      </div>
                    ) : rebuild.phase === 'done' ? (
                      <div style={{ fontSize: 12, color: colors.success, lineHeight: 1.6 }}>
                        Rebuilt LIVE: cancelled{' '}
                        <span style={{ fontFamily: 'monospace', fontSize: 11 }}>{rebuild.result?.cancelled_campaign_id || cell.campaign_id}</span>
                        {' '}→ new{' '}
                        <b style={{ fontFamily: 'monospace', fontSize: 11 }}>{rebuild.result?.new_campaign_id || '?'}</b>
                        {typeof rebuild.result?.sent_before_cancel === 'number' && (
                          <> · sent before cancel: {rebuild.result.sent_before_cancel.toLocaleString()}</>
                        )}
                        {rebuild.result?.note && <div style={caveatBox}>{rebuild.result.note}</div>}
                      </div>
                    ) : (
                      <>
                        <div style={{ fontSize: 11, color: colors.text, lineHeight: 1.6 }}>
                          <div style={label}>Dry preview — would cancel</div>
                          <pre style={preBox}>{verbatim(rebuild.preview?.would_cancel)}</pre>
                          <div style={label}>New input summary</div>
                          <pre style={preBox}>{verbatim(rebuild.preview?.new_input_summary)}</pre>
                        </div>
                        {rebuild.preview?.partial_send_warning && (
                          <div style={caveatBox}>{rebuild.preview.partial_send_warning}</div>
                        )}
                        {rebuild.preview?.offer_gate_warning && (
                          <div style={caveatBox}>{rebuild.preview.offer_gate_warning}</div>
                        )}
                        <div style={{ marginTop: 8 }}>
                          <div style={label}>Type the campaign name exactly to confirm — this CANCELS and REDEPLOYS</div>
                          <input style={{ ...input, width: '100%', boxSizing: 'border-box', marginTop: 4 }}
                            placeholder={cell.name}
                            value={rebuildTyped} onChange={e => setRebuildTyped(e.target.value)} />
                          <div style={{ display: 'flex', gap: 8, marginTop: 6 }}>
                            <button type="button"
                              style={{ ...dangerBtn, opacity: rebuildTyped === cell.name ? 1 : 0.5 }}
                              disabled={rebuildTyped !== cell.name || rebuild.phase === 'confirming'}
                              onClick={() => void postRebuild(true)}>
                              {rebuild.phase === 'confirming' ? 'Rebuilding…' : 'Rebuild live — LIVE'}
                            </button>
                            <button type="button" style={{ ...smallBtn, background: 'transparent' }}
                              disabled={rebuild.phase === 'confirming'}
                              onClick={() => { setRebuild({ phase: 'idle' }); setRebuildTyped('') }}>
                              Cancel
                            </button>
                          </div>
                        </div>
                      </>
                    )}
                    {rebuild.error && <div style={{ ...errorChip, marginTop: 8 }}>{rebuild.error}</div>}
                  </>
                )}
              </div>
            )}

            <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 10, lineHeight: 1.5 }}>
              <b>Open in Day Cards:</b> Day Cards tab → select domain <b>{cell.sending_domain || cell.property}</b>,
              date <b>{date}</b>. (No cross-tab navigation from this screen.)
            </div>
          </>
        )}

        <div style={{
          fontSize: 11, color: colors.textMuted, marginTop: 18, paddingTop: 10,
          borderTop: `1px solid ${colors.panelBorder}`, lineHeight: 1.5,
        }}>
          Grid edits are LOCAL until acted on — nothing deploys from this screen except the
          explicit actions above labeled LIVE (attach-offer, and MAKE IT REAL which rebuilds
          the live campaign via the Day Cards rebuild path).
        </div>
      </div>
    </SideShelf>
  )
}

export default BoardCellShelf
