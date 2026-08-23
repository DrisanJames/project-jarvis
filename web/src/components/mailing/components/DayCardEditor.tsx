/**
 * DayCardEditor — SideShelf editor for one Day Card campaign.
 *
 * Drives the cancel-and-rebuild flow:
 *   POST /api/mailing/pmta-campaign/day-cards/rebuild
 *     confirmed:false → dry preview {would_cancel, new_input_summary, partial_send_warning?}
 *     confirmed:true  → {cancelled_campaign_id, new_campaign_id, sent_before_cancel, note}
 *
 * Creative pickers by transport: offer-proofs for pmta/ses, the creatives
 * registry for kumo (approved rows only — the endpoint has no filter, so we
 * filter client-side). Segments/lists pickers reuse the portal-standard
 * GET /api/mailing/segments + /api/mailing/lists (the same endpoints
 * JourneyBuilder/EOCleaning use). Every fetch degrades with explicit error
 * text on 404 — the backend is being built in parallel.
 */
import React, { useCallback, useEffect, useMemo, useState } from 'react'
import { apiFetch } from '../shared/apiFetch'
import { colors, alpha, btnStyle, thStyle, tdStyle } from '../shared/theme'
import { Pill } from '../shared/ui'
import { SideShelf } from './shared/SideShelf'
import { isOfferExemptCampaignName } from './DayCards'
import type { DayCard } from './DayCards'

// ── Pick-list contract types ────────────────────────────────────────────────

interface ProofVariant { subject: string; preheader: string }
interface OfferProof {
  id: string
  name: string
  offer_key: string
  approval_status: string
  is_active: boolean
  variants: ProofVariant[]
  from_names: string[]
  approved_domains: string[]
}
interface KumoCreative {
  id: string
  offer_key: string
  brand_code: string
  filename: string
  subject: string
  approval_status: string
}
interface PickerItem { id: string; name: string; subscriber_count?: number }
interface OfferListItem { id: string; key: string; name: string; status: string }
interface AttachOfferResult {
  campaign_id: string
  offer_id: string
  offer_key: string
  offer_name: string
  suppression_caveat: string
}

interface DryPreview {
  would_cancel: string
  new_input_summary: unknown
  partial_send_warning?: string
}
interface RebuildResult {
  cancelled_campaign_id: string
  new_campaign_id: string
  sent_before_cancel: number
  note: string
}

// ── Helpers ─────────────────────────────────────────────────────────────────

const describe404 = (endpoint: string): string =>
  `${endpoint} returned 404 — this pick-list's backend is not deployed yet.`

/**
 * "YYYY-MM-DDTHH:MM" (a Denver wall-clock) → RFC3339 with Denver's UTC offset
 * for that date (handles MST/MDT via Intl; falls back to -06:00).
 */
const denverNaiveToISO = (naive: string): string => {
  const probe = new Date(`${naive}:00Z`)
  let off = '-06:00'
  try {
    const parts = new Intl.DateTimeFormat('en-US', {
      timeZone: 'America/Denver', timeZoneName: 'longOffset',
    }).formatToParts(probe)
    const tz = parts.find(p => p.type === 'timeZoneName')?.value ?? ''
    const m = tz.match(/GMT([+-]\d{2}):?(\d{2})?/)
    if (m) off = `${m[1]}:${m[2] ?? '00'}`
  } catch { /* keep fallback */ }
  return `${naive}:00${off}`
}

/** ISO scheduled_at → Denver wall-clock "YYYY-MM-DDTHH:MM" for datetime-local. */
const isoToDenverNaive = (iso: string): string => {
  const d = new Date(iso)
  if (!iso || isNaN(d.getTime())) return ''
  const parts = new Intl.DateTimeFormat('en-CA', {
    timeZone: 'America/Denver',
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', hour12: false,
  }).formatToParts(d)
  const get = (t: string) => parts.find(p => p.type === t)?.value ?? ''
  const hour = get('hour') === '24' ? '00' : get('hour')
  return `${get('year')}-${get('month')}-${get('day')}T${hour}:${get('minute')}`
}

// ── Small building blocks ───────────────────────────────────────────────────

const Section: React.FC<{ title: string; children: React.ReactNode }> = ({ title, children }) => (
  <div style={{ padding: '14px 20px', borderBottom: `1px solid ${colors.divider}` }}>
    <div style={{ fontSize: 11, fontWeight: 700, color: colors.indigo300, textTransform: 'uppercase', letterSpacing: 0.6, marginBottom: 10 }}>
      {title}
    </div>
    {children}
  </div>
)

const fieldLabel: React.CSSProperties = {
  display: 'block', fontSize: 11, color: colors.textMuted, marginBottom: 4,
}
const inputStyle: React.CSSProperties = {
  width: '100%', boxSizing: 'border-box',
  background: colors.panelBgSolid, color: colors.text,
  border: `1px solid ${colors.panelBorder}`, borderRadius: 6, padding: '7px 9px', fontSize: 12,
}
const noteStyle: React.CSSProperties = { fontSize: 11, color: colors.textFaint, marginTop: 4 }

/**
 * Generic id picker: search box over a fetched pool, selected ids as
 * removable chips. Unknown ids (from the card) still render as chips.
 */
const IdPicker: React.FC<{
  label: string
  pool: PickerItem[] | null
  poolError: string | null
  selected: string[]
  onChange: (ids: string[]) => void
  disabled?: boolean
}> = ({ label, pool, poolError, selected, onChange, disabled }) => {
  const [q, setQ] = useState('')
  const nameOf = (id: string) => pool?.find(p => p.id === id)?.name ?? id
  const matches = useMemo(() => {
    if (!pool || q.trim().length < 2) return []
    const needle = q.trim().toLowerCase()
    return pool
      .filter(p => !selected.includes(p.id) && (p.name.toLowerCase().includes(needle) || p.id === q.trim()))
      .slice(0, 12)
  }, [pool, q, selected])

  return (
    <div style={{ marginBottom: 10 }}>
      <span style={fieldLabel}>{label}</span>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, marginBottom: 6 }}>
        {selected.length === 0 && (
          <span style={{ fontSize: 11, color: colors.textFaint }}>none selected</span>
        )}
        {selected.map(id => (
          <span key={id} style={{
            display: 'inline-flex', alignItems: 'center', gap: 6,
            background: alpha(colors.indigo500, '14'), border: `1px solid ${colors.panelBorder}`,
            borderRadius: 6, padding: '2px 8px', fontSize: 11, color: colors.text,
          }} title={id}>
            {nameOf(id)}
            {!disabled && (
              <button
                onClick={() => onChange(selected.filter(x => x !== id))}
                style={{ background: 'none', border: 'none', color: colors.textMuted, cursor: 'pointer', padding: 0, fontSize: 12 }}
                aria-label={`remove ${nameOf(id)}`}
              >×</button>
            )}
          </span>
        ))}
      </div>
      {!disabled && (
        <>
          <input
            style={inputStyle}
            placeholder={pool ? `search by name (min 2 chars) · ${pool.length} available` : 'loading…'}
            value={q}
            onChange={e => setQ(e.target.value)}
          />
          {poolError && <div style={{ ...noteStyle, color: colors.dangerText }}>{poolError}</div>}
          {matches.length > 0 && (
            <div style={{ border: `1px solid ${colors.panelBorder}`, borderRadius: 6, marginTop: 4, maxHeight: 180, overflowY: 'auto' }}>
              {matches.map(m => (
                <div
                  key={m.id}
                  onClick={() => { onChange([...selected, m.id]); setQ('') }}
                  style={{ padding: '6px 9px', fontSize: 12, color: colors.text, cursor: 'pointer', borderBottom: `1px solid ${colors.divider}` }}
                >
                  {m.name}
                  {m.subscriber_count != null && (
                    <span style={{ color: colors.textFaint, marginLeft: 8 }}>{m.subscriber_count.toLocaleString()}</span>
                  )}
                </div>
              ))}
            </div>
          )}
        </>
      )}
    </div>
  )
}

// ── Editor ──────────────────────────────────────────────────────────────────

export const DayCardEditor: React.FC<{
  card: DayCard
  domain: string
  onClose: () => void
  onRebuilt: () => void
}> = ({ card, domain, onClose, onRebuilt }) => {
  const readOnly = !card.has_config
  const isKumo = card.transport === 'kumo'
  const apex = domain.replace(/^em\./, '')

  // ── Creative pick-lists ──
  const [proofs, setProofs] = useState<OfferProof[] | null>(null)
  const [creatives, setCreatives] = useState<KumoCreative[] | null>(null)
  const [pickError, setPickError] = useState<string | null>(null)
  const [selProofId, setSelProofId] = useState<string>('')       // '' = keep current
  const [selCreativeId, setSelCreativeId] = useState<string>('') // '' = keep current
  const [previewHtml, setPreviewHtml] = useState<string | null>(null)
  const [previewError, setPreviewError] = useState<string | null>(null)

  // ── Override fields ──
  const [name, setName] = useState(card.name)
  const [subject, setSubject] = useState(card.subject ?? '')
  const [previewText, setPreviewText] = useState('')
  const [fromName, setFromName] = useState(card.from_name ?? '')
  const [inclusionSegments, setInclusionSegments] = useState<string[]>(card.inclusion_segments ?? [])
  const [exclusionSegments, setExclusionSegments] = useState<string[]>(card.exclusion_segments ?? [])
  const [exclusionLists, setExclusionLists] = useState<string[]>(card.exclusion_lists ?? [])
  const [minRemail, setMinRemail] = useState<string>(card.min_remail_hours != null ? String(card.min_remail_hours) : '')
  const [scheduledLocal, setScheduledLocal] = useState<string>(() => isoToDenverNaive(card.scheduled_at))

  // ── Offer binding ──
  const missingOffer = !card.offer_id && !isOfferExemptCampaignName(card.name)
  const [offerPool, setOfferPool] = useState<OfferListItem[] | null>(null)
  const [offerError, setOfferError] = useState<string | null>(null)
  const [selOfferId, setSelOfferId] = useState<string>(card.offer_id ?? '')
  const [attachBusy, setAttachBusy] = useState(false)
  const [attachError, setAttachError] = useState<string | null>(null)
  const [attachResult, setAttachResult] = useState<AttachOfferResult | null>(null)

  // ── Pickers pools ──
  const [segPool, setSegPool] = useState<PickerItem[] | null>(null)
  const [segError, setSegError] = useState<string | null>(null)
  const [listPool, setListPool] = useState<PickerItem[] | null>(null)
  const [listError, setListError] = useState<string | null>(null)

  // ── Rebuild flow state ──
  const [dry, setDry] = useState<DryPreview | null>(null)
  const [busy, setBusy] = useState(false)
  const [flowError, setFlowError] = useState<string | null>(null)
  const [halfState, setHalfState] = useState<string | null>(null)
  const [result, setResult] = useState<RebuildResult | null>(null)

  // Any override change invalidates a stale dry preview.
  const invalidate = useCallback(<T,>(setter: (v: T) => void) => (v: T) => {
    setter(v)
    setDry(null)
  }, [])

  // Load pick-lists once.
  useEffect(() => {
    if (readOnly) return
    void (async () => {
      try {
        if (isKumo) {
          const ep = `/api/mailing/creatives?brand=${encodeURIComponent(apex)}&limit=50`
          const res = await apiFetch(ep)
          if (!res.ok) throw new Error(res.status === 404 ? describe404('GET /api/mailing/creatives') : `GET /api/mailing/creatives failed: ${res.status}`)
          const j = await res.json()
          const all: KumoCreative[] = j.creatives ?? []
          // Endpoint has no approval filter — approved only, client-side.
          setCreatives(all.filter(c => c.approval_status === 'approved'))
        } else {
          const res = await apiFetch('/api/mailing/offer-proofs?status=approved&active=true')
          if (!res.ok) throw new Error(res.status === 404 ? describe404('GET /api/mailing/offer-proofs') : `GET /api/mailing/offer-proofs failed: ${res.status}`)
          const j = await res.json()
          setProofs(j.proofs ?? [])
        }
      } catch (e) {
        setPickError(e instanceof Error ? e.message : String(e))
      }
    })()
    void (async () => {
      try {
        const res = await apiFetch('/api/mailing/segments')
        if (!res.ok) throw new Error(res.status === 404 ? describe404('GET /api/mailing/segments') : `segments: ${res.status}`)
        const j = await res.json()
        setSegPool((j.segments ?? []) as PickerItem[])
      } catch (e) {
        setSegError(e instanceof Error ? e.message : String(e))
      }
    })()
    void (async () => {
      try {
        const res = await apiFetch('/api/mailing/lists')
        if (!res.ok) throw new Error(res.status === 404 ? describe404('GET /api/mailing/lists') : `lists: ${res.status}`)
        const j = await res.json()
        setListPool((j.lists ?? []) as PickerItem[])
      } catch (e) {
        setListError(e instanceof Error ? e.message : String(e))
      }
    })()
  }, [readOnly, isKumo, apex])

  // Offer pool — loaded even for read-only (pre-blob) cards: attach-offer
  // repairs the ROW and needs no stored deploy config.
  useEffect(() => {
    void (async () => {
      try {
        const res = await apiFetch('/api/mailing/offers/list')
        if (!res.ok) throw new Error(res.status === 404 ? describe404('GET /api/mailing/offers/list') : `offers: ${res.status}`)
        const j = await res.json()
        setOfferPool((j.offers ?? []) as OfferListItem[])
      } catch (e) {
        setOfferError(e instanceof Error ? e.message : String(e))
      }
    })()
  }, [])

  // Preview the selected creative in an iframe (fetched via apiFetch so the
  // org header rides along; iframe src would drop it).
  useEffect(() => {
    const id = isKumo ? selCreativeId : selProofId
    if (!id) { setPreviewHtml(null); setPreviewError(null); return }
    const ep = isKumo
      ? `/api/mailing/creatives/${id}/preview`
      : `/api/mailing/offer-proofs/${id}/preview`
    let stale = false
    void (async () => {
      setPreviewHtml(null)
      setPreviewError(null)
      try {
        const res = await apiFetch(ep)
        if (!res.ok) throw new Error(res.status === 404 ? describe404(`GET ${ep}`) : `preview failed: ${res.status}`)
        const html = await res.text()
        if (!stale) setPreviewHtml(html)
      } catch (e) {
        if (!stale) setPreviewError(e instanceof Error ? e.message : String(e))
      }
    })()
    return () => { stale = true }
  }, [selProofId, selCreativeId, isKumo])

  // Selecting a proof fills subject/preheader/from suggestions (variants[0]).
  const pickProof = (id: string) => {
    setSelProofId(id)
    setDry(null)
    const p = proofs?.find(x => x.id === id)
    if (p) {
      if (p.variants?.[0]?.subject) setSubject(p.variants[0].subject)
      if (p.variants?.[0]?.preheader) setPreviewText(p.variants[0].preheader)
      if (p.from_names?.[0]) setFromName(p.from_names[0])
    }
  }

  // ── Time validation ──
  const scheduledISO = scheduledLocal ? denverNaiveToISO(scheduledLocal) : ''
  const timeChanged = scheduledLocal !== isoToDenverNaive(card.scheduled_at)
  const timeInvalid = timeChanged && (
    !scheduledISO || new Date(scheduledISO).getTime() < Date.now() + 10 * 60 * 1000
  )

  // ── Build the overrides object (only what changed) ──
  const buildOverrides = (): Record<string, unknown> => {
    const o: Record<string, unknown> = {}
    if (name !== card.name) o.name = name
    if (subject !== (card.subject ?? '')) o.subject = subject
    if (previewText !== '') o.preview_text = previewText
    if (fromName !== (card.from_name ?? '')) o.from_name = fromName
    if (!isKumo && selProofId) o.proof_id = selProofId
    if (isKumo && selCreativeId) o.creative_id = selCreativeId
    const inclChanged = JSON.stringify(inclusionSegments) !== JSON.stringify(card.inclusion_segments ?? [])
    if (inclChanged) o.inclusion_segments = inclusionSegments
    const exclChanged = JSON.stringify(exclusionSegments) !== JSON.stringify(card.exclusion_segments ?? [])
    if (exclChanged) o.exclusion_segments = exclusionSegments
    const listsChanged = JSON.stringify(exclusionLists) !== JSON.stringify(card.exclusion_lists ?? [])
    if (listsChanged) o.exclusion_lists = exclusionLists
    if (minRemail !== (card.min_remail_hours != null ? String(card.min_remail_hours) : '') && minRemail !== '') {
      const n = Number(minRemail)
      if (!isNaN(n)) o.min_remail_hours = n
    }
    if (timeChanged && scheduledISO) o.scheduled_at = scheduledISO
    if (selOfferId && selOfferId !== (card.offer_id ?? '')) o.offer_id = selOfferId
    return o
  }

  // ── Attach-offer repair (no rebuild): sets offer_id on the LIVE row and
  // re-stamps attribution. The audience was already planned, so the offer's
  // suppression file did NOT apply — the server says so in every response
  // (suppression_caveat) and we render it verbatim.
  const runAttach = async () => {
    if (!selOfferId) { setAttachError('Pick an offer first.'); return }
    const typed = window.prompt(
      `ATTACH OFFER sets the offer on the live campaign WITHOUT rebuilding it.\n`
      + `The audience was already planned, so the offer's suppression file did NOT apply.\n\n`
      + `Type the campaign name exactly to confirm:\n${card.name}`,
    )
    if (typed === null) return
    if (typed !== card.name) {
      setAttachError('Attach cancelled — campaign name did not match.')
      return
    }
    setAttachBusy(true); setAttachError(null)
    try {
      const res = await apiFetch(`/api/mailing/pmta-campaign/${card.id}/attach-offer`, {
        method: 'POST',
        body: JSON.stringify({ offer_id: selOfferId, confirmed: true }),
      })
      const bodyText = await res.text().catch(() => '')
      if (!res.ok) {
        throw new Error(res.status === 404
          ? describe404(`POST /api/mailing/pmta-campaign/{id}/attach-offer`)
          : `Attach failed: ${res.status} ${bodyText.slice(0, 400)}`)
      }
      setAttachResult(JSON.parse(bodyText) as AttachOfferResult)
    } catch (e) {
      setAttachError(e instanceof Error ? e.message : String(e))
    } finally {
      setAttachBusy(false)
    }
  }

  const postRebuild = async (confirmed: boolean): Promise<Response> =>
    apiFetch('/api/mailing/pmta-campaign/day-cards/rebuild', {
      method: 'POST',
      body: JSON.stringify({ campaign_id: card.id, confirmed, overrides: buildOverrides() }),
    })

  const runPreview = async () => {
    setBusy(true); setFlowError(null); setHalfState(null)
    try {
      const res = await postRebuild(false)
      if (!res.ok) {
        const body = await res.text().catch(() => '')
        throw new Error(res.status === 404
          ? describe404('POST /api/mailing/pmta-campaign/day-cards/rebuild')
          : `Preview failed: ${res.status} ${body.slice(0, 400)}`)
      }
      setDry(await res.json() as DryPreview)
    } catch (e) {
      setFlowError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  const runRebuild = async () => {
    const typed = window.prompt(
      `REBUILD cancels the existing campaign and deploys a sibling with your edits.\n\n`
      + `Type the campaign name exactly to confirm:\n${card.name}`,
    )
    if (typed === null) return
    if (typed !== card.name) {
      setFlowError('Rebuild cancelled — campaign name did not match.')
      return
    }
    setBusy(true); setFlowError(null); setHalfState(null)
    try {
      const res = await postRebuild(true)
      const bodyText = await res.text().catch(() => '')
      if (res.status === 502) {
        // Half-state: original cancelled, sibling deploy failed. Show verbatim.
        setHalfState(bodyText || '502 with empty body — original may be cancelled with no sibling deployed. Verify the campaign in the DB before retrying.')
        return
      }
      if (!res.ok) {
        throw new Error(res.status === 404
          ? describe404('POST /api/mailing/pmta-campaign/day-cards/rebuild')
          : `Rebuild failed: ${res.status} ${bodyText.slice(0, 400)}`)
      }
      setResult(JSON.parse(bodyText) as RebuildResult)
    } catch (e) {
      setFlowError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  const summaryText = (v: unknown): string =>
    typeof v === 'string' ? v : JSON.stringify(v, null, 2)

  // ── Success panel replaces the whole editor body ──
  if (result) {
    return (
      <SideShelf title={<>Rebuilt — {card.name}</>} onClose={onRebuilt} width={560}>
        <Section title="Rebuild complete">
          <div style={{ fontSize: 13, color: colors.successText, marginBottom: 10 }}>
            Campaign rebuilt.
          </div>
          <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 12 }}>
            <tbody>
              <tr><td style={tdStyle}>Cancelled</td><td style={{ ...tdStyle, fontFamily: 'monospace' }}>{result.cancelled_campaign_id}</td></tr>
              <tr><td style={tdStyle}>New campaign</td><td style={{ ...tdStyle, fontFamily: 'monospace', color: colors.successText }}>{result.new_campaign_id}</td></tr>
              <tr><td style={tdStyle}>Sent before cancel</td><td style={tdStyle}>{result.sent_before_cancel.toLocaleString()}</td></tr>
            </tbody>
          </table>
          {result.note && <div style={{ ...noteStyle, marginTop: 8, color: colors.textMuted }}>{result.note}</div>}
          <button style={{ ...btnStyle, marginTop: 14 }} onClick={onRebuilt}>Close and reload day</button>
        </Section>
      </SideShelf>
    )
  }

  return (
    <SideShelf
      title={
        <span style={{ display: 'inline-flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
          {card.name}
          <Pill color={isKumo ? colors.warning : colors.indigo400}>{card.transport}</Pill>
        </span>
      }
      onClose={onClose}
      width={620}
    >
      {readOnly && (
        <div style={{
          margin: '14px 20px', padding: '10px 12px', fontSize: 12,
          color: colors.warningText, background: alpha(colors.warning, '14'),
          border: `1px solid ${alpha(colors.warning, '44')}`, borderRadius: 8,
        }}>
          READ-ONLY — this campaign predates the stored deploy config (pre-blob), so its
          original inputs cannot be recovered and rebuild is unavailable. View only.
        </div>
      )}

      {/* CREATIVE */}
      <Section title="Creative">
        <div style={{ fontSize: 12, color: colors.textMuted, marginBottom: 10 }}>
          Current: <span style={{ color: colors.text }}>“{card.subject || 'no subject'}”</span>
          {' '}from <span style={{ color: colors.text }}>{card.from_name || '—'}</span>
        </div>
        {!readOnly && (
          <>
            <span style={fieldLabel}>
              {isKumo ? `Creative (registry, brand ${apex}, approved only)` : 'Offer proof (approved + active)'}
            </span>
            {pickError ? (
              <div style={{ ...noteStyle, color: colors.dangerText }}>{pickError}</div>
            ) : (
              <select
                style={{ ...inputStyle, marginBottom: 8 }}
                value={isKumo ? selCreativeId : selProofId}
                onChange={e => (isKumo ? (setSelCreativeId(e.target.value), setDry(null)) : pickProof(e.target.value))}
              >
                <option value="">keep current creative</option>
                {isKumo
                  ? (creatives ?? []).map(c => (
                      <option key={c.id} value={c.id}>{c.filename || c.offer_key} — {c.subject}</option>
                    ))
                  : (proofs ?? []).map(p => (
                      <option key={p.id} value={p.id}>{p.name} ({p.offer_key})</option>
                    ))}
              </select>
            )}
            {isKumo && creatives !== null && creatives.length === 0 && !pickError && (
              <div style={noteStyle}>No approved creatives in the registry for brand {apex}.</div>
            )}
            {previewError && <div style={{ ...noteStyle, color: colors.dangerText }}>{previewError}</div>}
            {previewHtml && (
              <iframe
                title="creative preview"
                sandbox=""
                srcDoc={previewHtml}
                style={{ width: '100%', height: 280, background: '#fff', border: `1px solid ${colors.panelBorder}`, borderRadius: 8, marginBottom: 10 }}
              />
            )}
            <span style={fieldLabel}>Name</span>
            <input style={{ ...inputStyle, marginBottom: 8 }} value={name} onChange={e => invalidate(setName)(e.target.value)} />
            <span style={fieldLabel}>Subject</span>
            <input style={{ ...inputStyle, marginBottom: 8 }} value={subject} onChange={e => invalidate(setSubject)(e.target.value)} />
            <span style={fieldLabel}>Preview text (preheader)</span>
            <input style={{ ...inputStyle, marginBottom: 8 }} value={previewText} onChange={e => invalidate(setPreviewText)(e.target.value)} placeholder="leave empty to keep current" />
            <span style={fieldLabel}>From name</span>
            <input style={inputStyle} value={fromName} onChange={e => invalidate(setFromName)(e.target.value)} />
          </>
        )}
      </Section>

      {/* OFFER */}
      <Section title="Offer">
        <div style={{ fontSize: 12, color: colors.textMuted, marginBottom: 10 }}>
          Current: <span style={{ color: card.offer_id ? colors.text : colors.dangerText }}>
            {card.offer_name || card.offer_key || (card.offer_id ? card.offer_id : 'NO OFFER')}
          </span>
        </div>
        {missingOffer && !attachResult && (
          <div style={{
            marginBottom: 10, padding: '10px 12px', borderRadius: 8, fontSize: 12,
            color: colors.dangerText, background: alpha(colors.danger, '14'),
            border: `1px solid ${alpha(colors.danger, '66')}`,
          }}>
            <div style={{ fontWeight: 700, color: colors.danger, marginBottom: 4 }}>NO OFFER attached</div>
            This campaign's audience was planned WITHOUT any advertiser suppression file
            (offer-level suppression keys off offer_id), and its conversions cannot be
            attributed to an offer. Attach an offer to repair attribution, or rebuild with
            the offer for full correctness (the rebuild re-plans the audience WITH the
            offer's suppression file).
          </div>
        )}
        {offerError ? (
          <div style={{ ...noteStyle, color: colors.dangerText }}>{offerError}</div>
        ) : (
          <select
            style={{ ...inputStyle, marginBottom: 8 }}
            value={selOfferId}
            onChange={e => { setSelOfferId(e.target.value); setDry(null) }}
            disabled={attachBusy}
          >
            <option value="">{card.offer_id ? 'keep current offer' : 'select offer…'}</option>
            {(offerPool ?? []).map(o => (
              <option key={o.id} value={o.id}>{o.name}{o.key ? ` (${o.key})` : ''}</option>
            ))}
          </select>
        )}
        {attachError && (
          <div style={{ ...noteStyle, color: colors.dangerText, marginBottom: 6 }}>{attachError}</div>
        )}
        {attachResult ? (
          <div style={{
            padding: '10px 12px', borderRadius: 8, fontSize: 12,
            background: alpha(colors.warning, '14'), border: `1px solid ${alpha(colors.warning, '44')}`,
          }}>
            <div style={{ fontWeight: 700, color: colors.successText, marginBottom: 4 }}>
              Offer attached: {attachResult.offer_name}{attachResult.offer_key ? ` (${attachResult.offer_key})` : ''}
            </div>
            <div style={{ color: colors.warningText, marginBottom: 6 }}>
              {attachResult.suppression_caveat}
            </div>
            {!readOnly && (
              <div style={{ color: colors.textMuted, marginBottom: 8 }}>
                Full fix: use "Preview rebuild" → "Rebuild campaign" below with the offer
                selected — the sibling re-plans its audience WITH the offer's suppression file.
              </div>
            )}
            <button style={btnStyle} onClick={onRebuilt}>Close and reload day</button>
          </div>
        ) : missingOffer && (
          <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap' }}>
            <button
              style={{ ...btnStyle, opacity: selOfferId ? 1 : 0.5 }}
              disabled={attachBusy || !selOfferId}
              title="Sets offer_id on the live campaign and re-stamps attribution. Does NOT re-plan the audience — the offer's suppression file still did not apply."
              onClick={() => void runAttach()}
            >
              {attachBusy ? 'Attaching…' : 'Attach offer'}
            </button>
            {!readOnly && (
              <button
                style={{ ...btnStyle, opacity: selOfferId ? 1 : 0.5 }}
                disabled={busy || attachBusy || !selOfferId}
                title="The full fix: cancel + redeploy a sibling with this offer — the planner applies the offer's suppression file. Runs the normal Preview → Rebuild flow."
                onClick={() => void runPreview()}
              >
                Rebuild with offer
              </button>
            )}
          </div>
        )}
        {!missingOffer && !readOnly && (
          <div style={noteStyle}>
            Changing the offer is applied on rebuild (it re-plans the audience with the
            new offer's suppression file).
          </div>
        )}
      </Section>

      {/* AUDIENCE */}
      <Section title="Audience">
        {card.inclusion_segments == null && (
          <div style={{ ...noteStyle, marginBottom: 8 }}>
            The card did not carry current segment ids — an empty selection keeps the
            campaign's existing audience; adding segments REPLACES it on rebuild.
          </div>
        )}
        <IdPicker
          label="Inclusion segments"
          pool={segPool}
          poolError={segError}
          selected={inclusionSegments}
          onChange={invalidate(setInclusionSegments)}
          disabled={readOnly}
        />
      </Section>

      {/* SUPPRESSION */}
      <Section title="Suppression">
        <IdPicker
          label="Exclusion segments"
          pool={segPool}
          poolError={segError}
          selected={exclusionSegments}
          onChange={invalidate(setExclusionSegments)}
          disabled={readOnly}
        />
        <IdPicker
          label="Exclusion lists"
          pool={listPool}
          poolError={listError}
          selected={exclusionLists}
          onChange={invalidate(setExclusionLists)}
          disabled={readOnly}
        />
        <span style={fieldLabel}>Min remail hours</span>
        <input
          type="number"
          min={0}
          style={{ ...inputStyle, maxWidth: 140 }}
          value={minRemail}
          onChange={e => invalidate(setMinRemail)(e.target.value)}
          placeholder="keep current"
          disabled={readOnly}
        />
      </Section>

      {/* TIME */}
      <Section title="Time">
        <span style={fieldLabel}>Scheduled at (America/Denver)</span>
        <input
          type="datetime-local"
          style={{ ...inputStyle, maxWidth: 240 }}
          value={scheduledLocal}
          onChange={e => invalidate(setScheduledLocal)(e.target.value)}
          disabled={readOnly}
        />
        {timeInvalid && (
          <div style={{ ...noteStyle, color: colors.dangerText }}>
            New time must be at least 10 minutes from now.
          </div>
        )}
        <div style={noteStyle}>
          Moving time rewrites every ISP plan span, preserving durations.
        </div>
      </Section>

      {/* Footer — the rebuild flow */}
      {!readOnly && (
        <div style={{ padding: '14px 20px 24px' }}>
          {flowError && (
            <div style={{ fontSize: 12, color: colors.dangerText, marginBottom: 10 }}>{flowError}</div>
          )}
          {halfState && (
            <div style={{
              marginBottom: 12, padding: '12px 14px', borderRadius: 8,
              background: alpha(colors.danger, '14'), border: `1px solid ${alpha(colors.danger, '66')}`,
            }}>
              <div style={{ fontSize: 12, fontWeight: 700, color: colors.danger, marginBottom: 6 }}>
                HALF-STATE (502) — the original campaign was CANCELLED but the sibling deploy failed.
              </div>
              <div style={{ fontSize: 12, color: colors.dangerText, whiteSpace: 'pre-wrap', fontFamily: 'monospace' }}>
                {halfState}
              </div>
              <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 8 }}>
                Nothing is scheduled in this slot right now. Re-run 'Preview rebuild' then
                'Rebuild campaign' to retry the sibling deploy, or redeploy via the Campaign Manager.
              </div>
            </div>
          )}
          {dry && (
            <div style={{
              marginBottom: 12, padding: '12px 14px', borderRadius: 8,
              background: alpha(colors.indigo500, '0d'), border: `1px solid ${colors.panelBorder}`,
            }}>
              <div style={{ ...thStyle, borderBottom: 'none', padding: 0, marginBottom: 6 }}>Dry preview</div>
              <div style={{ fontSize: 12, color: colors.text, marginBottom: 6 }}>
                Would cancel: <span style={{ fontFamily: 'monospace' }}>{dry.would_cancel}</span>
              </div>
              <pre style={{
                fontSize: 11, color: colors.textMuted, whiteSpace: 'pre-wrap', margin: 0,
                maxHeight: 200, overflowY: 'auto',
              }}>
                {summaryText(dry.new_input_summary)}
              </pre>
              {dry.partial_send_warning && (
                <div style={{
                  marginTop: 8, padding: '8px 10px', borderRadius: 6, fontSize: 12, fontWeight: 600,
                  color: colors.danger, background: alpha(colors.danger, '14'), border: `1px solid ${alpha(colors.danger, '66')}`,
                }}>
                  {dry.partial_send_warning} — those recipients become re-eligible.
                </div>
              )}
            </div>
          )}
          <div style={{ display: 'flex', gap: 10 }}>
            <button style={btnStyle} disabled={busy || timeInvalid} onClick={() => void runPreview()}>
              {busy && !dry ? 'Previewing…' : 'Preview rebuild'}
            </button>
            <button
              style={{
                ...btnStyle,
                background: dry ? alpha(colors.danger, '22') : 'transparent',
                border: `1px solid ${alpha(colors.danger, dry ? '66' : '33')}`,
                color: dry ? colors.dangerText : colors.textFaint,
                cursor: dry ? 'pointer' : 'not-allowed',
              }}
              disabled={busy || !dry || timeInvalid}
              title={dry ? 'Cancels the original and deploys a sibling with your edits' : 'Run Preview rebuild first'}
              onClick={() => void runRebuild()}
            >
              Rebuild campaign
            </button>
          </div>
          {!dry && (
            <div style={noteStyle}>Preview is required before rebuild; any edit clears a stale preview.</div>
          )}
        </div>
      )}
    </SideShelf>
  )
}

export default DayCardEditor
