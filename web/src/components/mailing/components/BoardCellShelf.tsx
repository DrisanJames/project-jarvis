// BoardCellShelf — the slide-out editor for one Board Grid cell.
//
// Replaces the old inline <select> cell editor. Shows the cell's identity,
// the gate findings scoped to this (property, slot), and a searchable offer
// picker whose "Apply to grid" edits the LOCAL working grid (BoardGrid's
// applyOffer — which auto re-runs the gates). Nothing here deploys, with ONE
// labeled exception: the attach-offer LIVE action, which repairs an already
// deployed campaign that finalized without an offer_id (dry preview → typed
// -name confirm → confirmed POST, suppression caveat rendered verbatim).

import React, { useMemo, useState } from 'react'
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
  name: string
  offer_id?: string
  offer_name?: string
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
export interface OfferOpt { id: string; name: string }

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

const statusColor = (s?: string): string => {
  if (!s) return colors.textMuted
  if (['failed', 'cancelled', 'deleted'].includes(s)) return colors.danger
  if (['sent', 'completed'].includes(s)) return colors.success
  if (['sending', 'preparing', 'finalizing_audience'].includes(s)) return colors.warning
  return colors.textMuted
}

export const BoardCellShelf: React.FC<{
  date: string
  entries: Array<{ idx: number; cell: BoardCell }>  // all campaigns at this (property, slot)
  activeIdx: number
  onSelectEntry: (idx: number) => void
  findings: BoardFindingRow[]                        // whole-grid findings; filtered here
  offers: OfferOpt[]
  offersError: string | null
  edited: boolean
  gating: boolean
  cloneMode: boolean
  onApplyOffer: (idx: number, offerId: string) => void
  onAttached: () => void                             // a confirmed LIVE attach landed
  onClose: () => void
}> = ({ date, entries, activeIdx, onSelectEntry, findings, offers, offersError,
        edited, gating, cloneMode, onApplyOffer, onAttached, onClose }) => {
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

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    return q ? offers.filter(o => o.name.toLowerCase().includes(q)) : offers
  }, [offers, query])

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
                onClick={() => onApplyOffer(entry.idx, selOffer)}>
                {gating ? 'Gating…' : 'Apply to grid'}
              </button>
            </div>
            <div style={{ fontSize: 10, color: colors.textMuted, marginTop: 4 }}>
              Apply updates the LOCAL working grid and auto re-runs the gates — findings above refresh.
            </div>
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
                  ? 'This campaign already carries an offer — no live repair applies. Changing a deployed campaign’s offer means cancel + rebuild a sibling via Day Cards.'
                  : 'This cell is offer-exempt (KUMO-WARM / newsletter) — no offer belongs here by doctrine.'}
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
          Grid edits are LOCAL decision support — nothing deploys from this screen except the
          explicit attach-offer action above, which is labeled LIVE.
        </div>
      </div>
    </SideShelf>
  )
}

export default BoardCellShelf
