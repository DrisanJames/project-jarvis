// DripStepShelf — the slide-out opened by clicking a TouchNode on the Drip
// Journey canvas. Two halves:
//   (a) STEP CONFIG — what this touch ships (subject / preheader / from /
//       creative / offer) with edit actions wired to the SAME lane-content
//       endpoints the Property Ledger screen used (touch-copy edit, subject
//       add/archive, offer-swap with the typed-name confirm).
//   (b) STEP METRICS — perf_7d when the journey payload carries it, plus the
//       edge populations already in the journey payload (waiting, next-due,
//       per-ISP split).
//
// Write gating: the lane-content GET may carry write_enabled/write_flag_env.
// Absent flags = writable (the Property Ledger panel's standing behavior);
// an explicit false renders the read-only banner and disables every action.
// A 404 on lane-content (endpoint not deployed / brand unknown) degrades to
// an error strip — the journey-side config above it still renders.

import React, { useCallback, useEffect, useState } from 'react';
import { apiFetch } from '../shared/apiFetch';
import { colors, alpha, thStyle, tdStyle, numTd, numTh, tableStyle } from '../shared/theme';
import { Pill, SectionError } from '../shared/ui';
import { labelForVertical } from '../datapartners/verticalLabels';
import type { JourneyTouch, JourneyEdge } from './DripJourneyCanvas';

// ── Lane-content shapes (property_lane_content.go — mirrored from
//    PropertyLedgerView's LaneContentPanel, plus the optional write gates) ───

interface LaneCopyRow { id: string; text: string; status: string; serving: boolean; }
interface LaneCreative { id: string; version: number; status: string; updated_at: string; html_bytes: number; }
interface LaneOfferSharing { feeds: number; brands: number; }
interface LaneOffer {
  id: string; name: string; status: string;
  creative: LaneCreative | null;
  subjects: LaneCopyRow[]; from_names: LaneCopyRow[];
  sharing?: LaneOfferSharing;
}
interface LaneDataset {
  id: string; name: string; status: string; express_dispatch: boolean;
  offer: LaneOffer | null;
}
interface LaneTouchCopy {
  vertical: string; touch: number; creative_filename: string;
  subject_line: string; preheader: string; from_name: string;
  offer_id?: string; active: boolean; updated_at: string; updated_by?: string;
}
interface LaneTouchOffer { binding: string; touch_scope: string[]; offer: LaneOffer | null; }
interface LaneFeed {
  vertical: string; weight: number; datasets: LaneDataset[];
  touch1: LaneTouchCopy | null; followups: LaneTouchCopy[];
  touch_offers: LaneTouchOffer[];
}
interface LaneContentResponse {
  brand: string; sending_domain: string;
  feeds: LaneFeed[]; global_followups: LaneTouchCopy[];
  active_offers: Array<{ id: string; name: string }>;
  preheader_note: string;
  // Optional write gates. Absent = writable (LaneContentPanel's behavior).
  write_enabled?: boolean;
  write_flag_env?: string;
}

const UNKNOWN = '—';
const num = (n: number | null | undefined): string =>
  typeof n === 'number' && Number.isFinite(n) ? Math.round(n).toLocaleString() : UNKNOWN;
const pctOf = (n: number | null | undefined, d: number | null | undefined): string =>
  typeof n === 'number' && typeof d === 'number' && d > 0 ? `${((n / d) * 100).toFixed(2)}%` : UNKNOWN;
const shortTime = (iso: string | null | undefined): string => {
  if (!iso) return UNKNOWN;
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return UNKNOWN;
  return d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
};

const label: React.CSSProperties = {
  fontSize: 10, letterSpacing: 0.6, textTransform: 'uppercase', color: colors.textMuted,
};
const input: React.CSSProperties = {
  background: 'rgba(255,255,255,0.06)', color: colors.text,
  border: `1px solid ${colors.panelBorderStrong}`, borderRadius: 5,
  padding: '4px 6px', fontSize: 12,
};
const smallBtn: React.CSSProperties = {
  background: alpha(colors.indigo500, '22'), color: colors.indigo200,
  border: `1px solid ${alpha(colors.indigo500, '66')}`, borderRadius: 5,
  padding: '3px 10px', fontSize: 11, fontWeight: 700, cursor: 'pointer',
};
const ghostBtn: React.CSSProperties = {
  ...smallBtn, background: 'transparent', color: colors.textMuted, border: `1px solid ${colors.hairline}`,
};
const sectionTitle: React.CSSProperties = {
  fontSize: 11, color: colors.heading, fontWeight: 700, letterSpacing: 0.5,
  margin: '16px 0 8px 0',
};
const noteStyle: React.CSSProperties = {
  fontSize: 11, color: colors.textMuted, lineHeight: 1.6, marginTop: 6,
};

export const DripStepShelf: React.FC<{
  brand: string;
  sendingDomain: string | null;
  vertical: string;
  laneDisplay?: string;          // friendly lane name (display_name || label); falls back to labelForVertical
  touch: JourneyTouch;
  edgeOut: JourneyEdge | null;   // this touch → next touch
  edgeIn: JourneyEdge | null;    // previous touch → this touch
  delayHours: number;
  dueNow: number | null;         // journey.totals.due_now (whole drip)
  onChanged: () => void;         // reload the journey after a write
  onNotice: (s: string) => void;
}> = ({ brand, sendingDomain, vertical, laneDisplay, touch, edgeOut, edgeIn, delayHours, dueNow, onChanged, onNotice }) => {
  // ── lane-content fetch (own lifecycle; keyed by brand) ────────────────────
  const [content, setContent] = useState<LaneContentResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const loadContent = useCallback(async () => {
    setLoading(true); setErr(null);
    try {
      const r = await apiFetch(
        `/api/mailing/pmta-campaign/property-ledger/lane-content?brand=${encodeURIComponent(brand)}`);
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      setContent((await r.json()) as LaneContentResponse);
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'load failed');
    } finally { setLoading(false); }
  }, [brand]);

  useEffect(() => { setContent(null); void loadContent(); }, [loadContent]);

  const writable = content != null && content.write_enabled !== false;
  // Screen doctrine: write surfaces render READ-ONLY, never hidden. Controls
  // below stay visible when !writable, disabled, carrying this title.
  const roTitle = !writable
    ? `READ-ONLY — the server reports write_enabled=false${content?.write_flag_env ? ` (env ${content.write_flag_env} is not set)` : ''}. Nothing on this shelf will save.`
    : undefined;

  // The feed for this vertical, and this touch's copy row (feed-specific first,
  // global fallback second) — the row's OWN vertical is what the POST must
  // carry (a global row has vertical '', and posting the lane's vertical for it
  // would create a lane-specific override instead of editing the shared row).
  const feed = (content?.feeds ?? []).find((f) => f.vertical === vertical) ?? null;
  const copyRow: LaneTouchCopy | null = (() => {
    if (feed) {
      if (touch.touch === 1 && feed.touch1) return feed.touch1;
      const own = feed.followups.find((t) => t.touch === touch.touch);
      if (own) return own;
    }
    return (content?.global_followups ?? []).find((t) => t.touch === touch.touch) ?? null;
  })();

  // The offer this touch resolves to: match the journey's offer_id first, then
  // touch-level bindings, then the feed's dataset-level offer. When neither
  // matches and we fall back to the FIRST offer in the payload, that is an
  // INFERENCE, not a resolution — flagged so edits are verified first.
  const { offer, offerInferred } = ((): { offer: LaneOffer | null; offerInferred: boolean } => {
    const all: LaneOffer[] = [
      ...(feed?.datasets ?? []).map((d) => d.offer).filter((o): o is LaneOffer => !!o),
      ...(feed?.touch_offers ?? []).map((t) => t.offer).filter((o): o is LaneOffer => !!o),
    ];
    if (touch.offer_id) {
      const hit = all.find((o) => o.id === touch.offer_id);
      if (hit) return { offer: hit, offerInferred: false };
    }
    const touchScoped = (feed?.touch_offers ?? []).find((t) =>
      t.offer && t.touch_scope.some((s) => s.replace(/[^0-9]/g, '') === String(touch.touch)));
    if (touchScoped?.offer) return { offer: touchScoped.offer, offerInferred: false };
    return { offer: all[0] ?? null, offerInferred: all.length > 0 };
  })();

  // ── writes (same endpoints as LaneContentPanel) ───────────────────────────
  const [saving, setSaving] = useState<Record<string, boolean>>({});
  const withSaving = async (key: string, fn: () => Promise<void>) => {
    setSaving((s) => ({ ...s, [key]: true }));
    try { await fn(); } finally { setSaving((s) => ({ ...s, [key]: false })); }
  };
  const post = async (path: string, body: Record<string, unknown>) => {
    const r = await apiFetch(`/api/mailing/pmta-campaign/property-ledger/lane-content/${path}`, {
      method: 'POST', body: JSON.stringify(body),
    });
    let json: Record<string, unknown> = {};
    try { json = (await r.json()) as Record<string, unknown>; } catch { /* non-JSON */ }
    return { ok: r.ok, status: r.status, json };
  };

  // Touch-copy edit
  const [copyEdit, setCopyEdit] = useState<{ subject_line: string; preheader: string; from_name: string } | null>(null);
  const saveTouchCopy = () => withSaving('touch', async () => {
    if (!copyEdit || !copyRow) return;
    const body: Record<string, unknown> = { vertical: copyRow.vertical, brand, touch: copyRow.touch };
    if (copyEdit.subject_line !== copyRow.subject_line) body.subject_line = copyEdit.subject_line;
    if (copyEdit.preheader !== copyRow.preheader) body.preheader = copyEdit.preheader;
    if (copyEdit.from_name !== copyRow.from_name) body.from_name = copyEdit.from_name;
    if (body.subject_line === undefined && body.preheader === undefined && body.from_name === undefined) {
      setCopyEdit(null);
      return;
    }
    const res = await post('touch-copy', body);
    if (!res.ok) { onNotice(`Touch copy update failed: ${String(res.json.error ?? res.status)}`); return; }
    onNotice(`Touch ${copyRow.touch} copy updated${copyRow.vertical === '' ? ' (shared global row — every feed that falls back here changed)' : ''}.`);
    setCopyEdit(null);
    await loadContent();
    onChanged();
  });

  // Subject add / archive on the resolved offer
  const [newSubject, setNewSubject] = useState('');
  const addSubject = () => withSaving('addSub', async () => {
    if (!offer) return;
    const text = newSubject.trim();
    if (!text) return;
    const res = await post('subject', { offer_id: offer.id, action: 'add', subject_line: text });
    if (!res.ok) { onNotice(`Subject add failed: ${String(res.json.error ?? res.status)}`); return; }
    setNewSubject('');
    await loadContent();
    onChanged();
  });
  const archiveSubject = (row: LaneCopyRow) => withSaving(`arch/${row.id}`, async () => {
    if (!offer) return;
    if (!window.confirm(`Archive this subject from "${offer.name}"?\n\n${row.text}`)) return;
    const res = await post('subject', { offer_id: offer.id, action: 'archive', subject_id: row.id });
    if (!res.ok) { onNotice(`Subject archive failed: ${String(res.json.error ?? res.status)}`); return; }
    await loadContent();
    onChanged();
  });

  // Offer swap (per dataset on this feed) — typed-name confirm, ThrottleEditor pattern
  const [swapSel, setSwapSel] = useState<Record<string, string>>({});
  const swapOffer = (ds: LaneDataset) => withSaving(`swap/${ds.id}`, async () => {
    const offerId = swapSel[ds.id];
    if (!offerId || offerId === (ds.offer?.id ?? '')) return;
    const typed = window.prompt(
      `OFFER SWAP changes what this feed mails on the next wave.\n\nType the feed name exactly to confirm:\n${ds.name}`);
    if (typed === null) return;
    if (typed !== ds.name) { onNotice('Offer swap cancelled — feed name did not match.'); return; }
    const res = await post('offer-swap', { dataset_id: ds.id, offer_id: offerId, confirm_name: typed });
    if (!res.ok) { onNotice(`Offer swap failed: ${String(res.json.error ?? res.status)}`); return; }
    onNotice(`Feed "${ds.name}" now mails ${String(res.json.offer_name ?? offerId)}.`);
    setSwapSel((s) => { const n = { ...s }; delete n[ds.id]; return n; });
    await loadContent();
    onChanged();
  });

  // ── Release next touch now (journey/advance) ──────────────────────────────
  // Rows waiting to RECEIVE touch N sit at touch_count = N-1, which is exactly
  // the inbound edge's from_touch — the payload's `touch` is derived from the
  // edge, never recomputed from the shelf's touch number.
  const [releaseN, setReleaseN] = useState('1000');
  const releaseNextTouch = () => withSaving('release', async () => {
    if (!edgeIn) return;
    const n = parseInt(releaseN, 10);
    if (!Number.isFinite(n) || n < 1 || n > 50000) {
      onNotice('Release cancelled — count must be between 1 and 50,000.');
      return;
    }
    const laneName = laneDisplay || labelForVertical(vertical);
    if (!window.confirm(
      `Make ${n.toLocaleString()} waiting records due immediately for touch ${touch.touch} on ${laneName}? `
      + `They will mail on the orchestrator's next tick (≤15 min), subject to all caps.`,
    )) return;
    let r: Response;
    try {
      r = await apiFetch('/api/mailing/pmta-campaign/property-ledger/journey/advance', {
        method: 'POST',
        body: JSON.stringify({ vertical, touch: edgeIn.from_touch, limit: n, brand }),
      });
    } catch (e) {
      onNotice(`Release failed: ${e instanceof Error ? e.message : 'network error'} — nothing was made due.`);
      return;
    }
    let json: Record<string, unknown> = {};
    try { json = (await r.json()) as Record<string, unknown>; } catch { /* non-JSON */ }
    if (r.status === 404) {
      onNotice('Release failed: this server does not have the journey/advance endpoint yet (HTTP 404) — nothing was made due.');
      return;
    }
    if (!r.ok) {
      onNotice(`Release failed: ${String(json.error ?? r.status)} — nothing was made due.`);
      return;
    }
    const advanced = typeof json.advanced === 'number' ? json.advanced : null;
    onNotice(
      `${advanced == null ? 'Some' : advanced.toLocaleString()} record(s) made due now for touch ${touch.touch}`
      + `${typeof json.note === 'string' && json.note ? ` — ${json.note}` : ''}. The orchestrator picks them up within ~15 min.`,
    );
    onChanged();
  });

  const servingSubjects = (offer?.subjects ?? []).filter((s) => s.serving);
  const servingFromNames = (offer?.from_names ?? []).filter((f) => f.serving);
  const perf = touch.perf_7d;

  return (
    <div style={{ padding: '14px 20px 24px 20px' }}>
      <div style={{ fontSize: 11, color: colors.textMuted, marginBottom: 8 }} title={`vertical: ${vertical}`}>
        {sendingDomain ?? brand} · {laneDisplay || labelForVertical(vertical)} · inter-touch delay {Number.isFinite(delayHours) ? `${delayHours}h` : UNKNOWN}
      </div>

      {/* ── (a) STEP CONFIG ─────────────────────────────────────────────── */}
      <div style={sectionTitle}>STEP CONFIG — what touch {touch.touch} ships</div>

      {!touch.configured ? (
        <div style={{ fontSize: 12, color: colors.warningText, lineHeight: 1.6 }}>
          This touch is <b>not configured</b> — the ladder retires here. No message is built for
          subscribers reaching this rung. Configure copy for it via the lane content editor below
          (a saved touch-copy row makes it configured).
        </div>
      ) : (
        <div style={{ display: 'grid', gridTemplateColumns: '96px 1fr', rowGap: 6, columnGap: 10, fontSize: 12 }}>
          <span style={label}>Subject</span>
          <span style={{ color: colors.text }}>{touch.subject_line || <span style={{ color: colors.warningText }}>(no subject)</span>}</span>
          <span style={label}>Preheader</span>
          <span style={{ color: colors.textMuted }}>{touch.preheader || UNKNOWN}</span>
          <span style={label}>From</span>
          <span style={{ color: colors.indigo200 }}>{touch.from_name || UNKNOWN}</span>
          <span style={label}>Content</span>
          <span>
            {touch.content_source
              ? (
                <>
                  <Pill color={touch.content_source === 'offer' ? colors.indigo400 : colors.textMuted} style={{ fontSize: 9, marginRight: 6 }}>
                    {touch.content_source === 'offer' ? 'OFFER' : touch.content_source === 'file' ? 'FILE' : touch.content_source}
                  </Pill>
                  <span style={{ color: touch.resolves === false ? colors.dangerText : colors.textMuted, fontSize: 11 }}>
                    {touch.resolution_label || touch.creative_filename || UNKNOWN}
                  </span>
                </>
              )
              : <span style={{ fontFamily: 'monospace', fontSize: 11, color: colors.textFaint }}>{touch.creative_filename || 'content path not reported'}</span>}
            {touch.resolves === false && (
              <div style={{ fontSize: 11, color: colors.dangerText, marginTop: 3 }}>
                UNRESOLVABLE — this touch will not send.{touch.resolution_problem ? ` ${touch.resolution_problem}` : ''}
              </div>
            )}
          </span>
        </div>
      )}

      {/* lane-content-backed editing */}
      {loading && <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 10 }}>Loading lane content…</div>}
      {err && (
        <div style={{ marginTop: 10 }}>
          <SectionError label="Lane content (edit surface)" error={err} onRetry={() => void loadContent()} />
          <div style={noteStyle}>
            The config shown above is the live journey read; only the EDIT surface is unavailable.
          </div>
        </div>
      )}
      {content && content.write_enabled === false && (
        <div style={{
          fontSize: 11, fontWeight: 600, color: colors.warningText,
          background: alpha(colors.warning, '14'),
          border: `1px solid ${alpha(colors.warning, '44')}`,
          borderRadius: 6, padding: '6px 10px', marginTop: 10,
        }}>
          Lane content is READ-ONLY — the server reports write_enabled=false
          {content.write_flag_env ? <> (env <code style={{ fontFamily: 'monospace' }}>{content.write_flag_env}</code> is not set)</> : ''}.
          Nothing on this shelf will save.
        </div>
      )}

      {content && (
        <>
          {/* Touch copy edit */}
          <div style={sectionTitle}>TOUCH COPY {copyRow?.vertical === '' ? '· shared global fallback row' : ''}</div>
          {!copyRow ? (
            <div style={{ fontSize: 11, color: colors.textFaint }}>
              No touch-copy row exists for touch {touch.touch} on this lane — nothing to edit here.
            </div>
          ) : copyEdit ? (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
              <input style={input} value={copyEdit.subject_line} placeholder="subject"
                onChange={(e) => setCopyEdit((s) => (s ? { ...s, subject_line: e.target.value } : s))} />
              <input style={input} value={copyEdit.preheader} placeholder="preheader"
                onChange={(e) => setCopyEdit((s) => (s ? { ...s, preheader: e.target.value } : s))} />
              <input style={input} value={copyEdit.from_name} placeholder="from name"
                onChange={(e) => setCopyEdit((s) => (s ? { ...s, from_name: e.target.value } : s))} />
              {copyRow.vertical === '' && (
                <div style={{ fontSize: 10, color: colors.warningText }}>
                  This is the SHARED global fallback row — saving changes every feed that falls back to it.
                </div>
              )}
              <div style={{ display: 'flex', gap: 8 }}>
                <button type="button" style={smallBtn} disabled={!!saving.touch} onClick={() => void saveTouchCopy()}>
                  {saving.touch ? 'Saving…' : 'Save'}
                </button>
                <button type="button" style={ghostBtn} onClick={() => setCopyEdit(null)}>Cancel</button>
              </div>
            </div>
          ) : (
            <div style={{ display: 'flex', gap: 10, alignItems: 'center', flexWrap: 'wrap', fontSize: 12 }}>
              <span style={{ color: colors.text, flex: '2 1 200px' }} title="subject">{copyRow.subject_line || UNKNOWN}</span>
              <span style={{ color: colors.textMuted, flex: '2 1 160px' }} title="preheader">{copyRow.preheader || UNKNOWN}</span>
              <span style={{ color: colors.textMuted, flex: '1 1 120px' }} title="from name">{copyRow.from_name || UNKNOWN}</span>
              {/* Write surfaces render READ-ONLY, never hidden. */}
              <button type="button" style={{ ...smallBtn, opacity: writable ? 1 : 0.5, cursor: writable ? 'pointer' : 'not-allowed' }}
                disabled={!writable}
                title={roTitle}
                onClick={() => setCopyEdit({
                  subject_line: copyRow.subject_line, preheader: copyRow.preheader, from_name: copyRow.from_name,
                })}>
                Edit
              </button>
            </div>
          )}

          {/* Offer + subject pool */}
          <div style={sectionTitle}>OFFER &amp; SUBJECT POOL{offerInferred ? ' (inferred)' : ''}</div>
          {!offer ? (
            <div style={{ fontSize: 11, color: colors.textFaint }}>
              No offer resolved for this touch in the lane content payload.
            </div>
          ) : (
            <>
              <div style={{ fontSize: 12, color: colors.indigo200, marginBottom: 6 }}>
                offer: <b>{offer.name || offer.id}</b> ({offer.status || '?'})
                {offerInferred && (
                  <span
                    style={{ marginLeft: 8 }}
                    title="No offer matched this touch's offer_id or a touch-scoped binding — this is the FIRST offer in the lane-content payload, an inference. Confirm it is the right offer before editing its pools."
                  >
                    <Pill color={colors.warning} style={{ fontSize: 9 }}>offer inferred — verify before editing</Pill>
                  </span>
                )}
                {offer.creative
                  ? <span style={{ fontSize: 11, color: colors.textMuted, marginLeft: 8 }}>
                      creative v{offer.creative.version} ({offer.creative.status}) · {shortTime(offer.creative.updated_at)}
                    </span>
                  : <span style={{ fontSize: 11, color: colors.dangerText, marginLeft: 8, fontWeight: 700 }}>NO SERVING CREATIVE</span>}
              </div>
              {offer.sharing && offer.sharing.feeds > 1 && (
                <div style={{ fontSize: 10, color: colors.warningText, fontWeight: 700, marginBottom: 4 }}>
                  Pool shared by {offer.sharing.feeds} feeds across {offer.sharing.brands} brand{offer.sharing.brands === 1 ? '' : 's'} —
                  edits here change live rotation on all of them.
                </div>
              )}
              <div style={label}>Serving subjects</div>
              {servingSubjects.map((sr) => (
                <div key={sr.id} style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '2px 0' }}>
                  <span style={{ fontSize: 12, color: colors.text, flex: 1 }}>{sr.text}</span>
                  <button type="button"
                    style={{ ...ghostBtn, padding: '1px 7px', fontSize: 9, opacity: writable ? 1 : 0.5, cursor: writable ? 'pointer' : 'not-allowed' }}
                    disabled={!writable || !!saving[`arch/${sr.id}`]}
                    title={roTitle}
                    onClick={() => void archiveSubject(sr)}>
                    {saving[`arch/${sr.id}`] ? '…' : 'Archive'}
                  </button>
                </div>
              ))}
              {servingSubjects.length === 0 && (
                <div style={{ fontSize: 11, color: colors.dangerText, fontWeight: 700 }}>
                  EMPTY SUBJECT POOL — the lane fails at send time
                </div>
              )}
              <div style={{ display: 'flex', gap: 6, marginTop: 4 }}>
                <input style={{ ...input, flex: 1, opacity: writable ? 1 : 0.5 }} placeholder="Add a subject line…"
                  disabled={!writable} title={roTitle}
                  value={newSubject} onChange={(e) => setNewSubject(e.target.value)} />
                <button type="button"
                  style={{ ...smallBtn, opacity: writable ? 1 : 0.5, cursor: writable ? 'pointer' : 'not-allowed' }}
                  disabled={!writable || !!saving.addSub || !newSubject.trim()}
                  title={roTitle}
                  onClick={() => void addSubject()}>
                  {saving.addSub ? 'Adding…' : 'Add'}
                </button>
              </div>
              <div style={{ ...label, marginTop: 8 }}>Serving from-names</div>
              {servingFromNames.map((fr) => (
                <div key={fr.id} style={{ fontSize: 12, color: colors.text, padding: '1px 0' }}>{fr.text}</div>
              ))}
              {servingFromNames.length === 0 && (
                <div style={{ fontSize: 11, color: colors.dangerText, fontWeight: 700 }}>
                  EMPTY FROM-NAME POOL — the lane fails at send time
                </div>
              )}
            </>
          )}

          {/* Offer swap — per dataset on this feed. Rendered even when not
              writable (write surfaces are read-only, never hidden). */}
          {feed && feed.datasets.length > 0 && (
            <>
              <div style={sectionTitle}>OFFER SWAP (per feed dataset)</div>
              {feed.datasets.map((ds) => (
                <div key={ds.id} style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', padding: '3px 0' }}>
                  <span style={{ fontSize: 12, color: colors.text, fontWeight: 600 }}>{ds.name}</span>
                  <span style={{ fontSize: 11, color: colors.textMuted }}>
                    {ds.offer ? <>mails <b>{ds.offer.name || ds.offer.id}</b></> : 'no dataset-level offer'}
                  </span>
                  <select
                    style={{ ...input, maxWidth: 200, opacity: writable ? 1 : 0.5 }}
                    value={swapSel[ds.id] ?? (ds.offer?.id ?? '')}
                    disabled={!writable || !!saving[`swap/${ds.id}`]}
                    title={roTitle}
                    onChange={(e) => setSwapSel((s) => ({ ...s, [ds.id]: e.target.value }))}
                  >
                    {!ds.offer && <option value="">(no offer)</option>}
                    {content.active_offers.map((o) => <option key={o.id} value={o.id}>{o.name}</option>)}
                  </select>
                  <button type="button"
                    style={{ ...smallBtn, background: 'rgba(239,68,68,0.12)', color: colors.dangerText, border: '1px solid rgba(239,68,68,0.40)', opacity: writable ? 1 : 0.5, cursor: writable ? 'pointer' : 'not-allowed' }}
                    disabled={!writable || !!saving[`swap/${ds.id}`] || !swapSel[ds.id] || swapSel[ds.id] === (ds.offer?.id ?? '')}
                    title={roTitle ?? 'Changes what this feed mails — requires typing the feed name to confirm.'}
                    onClick={() => void swapOffer(ds)}>
                    {saving[`swap/${ds.id}`] ? 'Swapping…' : 'Swap offer…'}
                  </button>
                </div>
              ))}
            </>
          )}
        </>
      )}

      {/* ── (b) STEP METRICS ────────────────────────────────────────────── */}
      <div style={sectionTitle}>STEP METRICS</div>

      {perf ? (
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(110px, 1fr))', gap: 10, marginBottom: 10 }}>
          <div><div style={label}>Sent (7d)</div><div style={{ fontSize: 17, fontWeight: 700, fontVariantNumeric: 'tabular-nums', color: colors.text }}>{num(perf.sent)}</div></div>
          <div><div style={label}>Delivered (7d)</div><div style={{ fontSize: 17, fontWeight: 700, fontVariantNumeric: 'tabular-nums', color: colors.text }}>{num(perf.delivered)}<span style={{ fontSize: 10, color: colors.textMuted, marginLeft: 5 }}>{pctOf(perf.delivered, perf.sent)} of sent</span></div></div>
          <div><div style={label}>Opened (7d)</div><div style={{ fontSize: 17, fontWeight: 700, fontVariantNumeric: 'tabular-nums', color: colors.text }}>{num(perf.opened)}<span style={{ fontSize: 10, color: colors.textMuted, marginLeft: 5 }}>{pctOf(perf.opened, perf.delivered)} of delivered</span></div></div>
          <div><div style={label}>Clicked (7d)</div><div style={{ fontSize: 17, fontWeight: 700, fontVariantNumeric: 'tabular-nums', color: colors.indigo200 }}>{num(perf.clicked)}<span style={{ fontSize: 10, color: colors.textMuted, marginLeft: 5 }}>{pctOf(perf.clicked, perf.delivered)} of delivered</span></div></div>
        </div>
      ) : (
        <div style={{ fontSize: 11, color: colors.textFaint, marginBottom: 10 }}>
          This server did not report 7-day performance for this touch (perf_7d absent) — unknown, not zero.
        </div>
      )}

      {edgeIn && (
        <div style={{ marginBottom: 8 }}>
          <div style={{ fontSize: 12, color: colors.text, marginBottom: 4 }}>
            Waiting to REACH this touch: <b>{num(edgeIn.waiting)}</b>
            <span style={{ fontSize: 11, color: colors.textMuted }}> · soonest {shortTime(edgeIn.soonest)}</span>
          </div>
          {/* Release control only when some of the waiting rows are actually in
              the FUTURE (waiting > drip-wide due-now backlog). When everything
              waiting is already due, an advance would be a no-op — say so. */}
          {edgeIn.waiting > (dueNow ?? 0) ? (
            <div style={{
              border: `1px solid ${alpha(colors.indigo500, '44')}`, borderRadius: 6,
              padding: '7px 10px', marginTop: 2,
            }}>
              <div style={{ ...label, marginBottom: 4 }}>Release next touch now</div>
              <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                <input
                  type="number" min={1} max={50000} step={100}
                  style={{ ...input, width: 96, opacity: writable ? 1 : 0.5 }}
                  value={releaseN}
                  disabled={!writable || !!saving.release}
                  title={roTitle}
                  onChange={(e) => setReleaseN(e.target.value)}
                />
                <button type="button"
                  style={{ ...smallBtn, opacity: writable ? 1 : 0.5, cursor: writable ? 'pointer' : 'not-allowed' }}
                  disabled={!writable || !!saving.release}
                  title={roTitle ?? `Makes up to this many waiting records at touch_count ${edgeIn.from_touch} due NOW. The orchestrator claims them within ~15 min; every cap (ledger, throttle, brand budget) still binds.`}
                  onClick={() => void releaseNextTouch()}>
                  {saving.release ? 'Releasing…' : `Make due for touch ${touch.touch}`}
                </button>
                <span style={{ fontSize: 10, color: colors.textFaint }}>max 50,000 · caps still bind</span>
              </div>
            </div>
          ) : (
            <div style={{ fontSize: 11, color: colors.textFaint }}>
              All waiting records are already due — they mail on the next tick.
            </div>
          )}
        </div>
      )}
      {edgeOut ? (
        <>
          <div style={{ fontSize: 12, color: colors.text, marginBottom: 4 }}>
            Waiting AFTER this touch (→ touch {edgeOut.to_touch}): <b>{num(edgeOut.waiting)}</b>
            <span style={{ fontSize: 11, color: colors.textMuted }}> · soonest {shortTime(edgeOut.soonest)} · latest {shortTime(edgeOut.latest)}</span>
          </div>
          {(edgeOut.by_isp ?? []).length > 0 ? (
            <table style={{ ...tableStyle, maxWidth: 420 }}>
              <thead>
                <tr>
                  <th style={thStyle}>ISP</th>
                  <th style={numTh}>Waiting</th>
                  <th style={numTh} title="Share of this edge's waiting population.">% of edge</th>
                </tr>
              </thead>
              <tbody>
                {(edgeOut.by_isp ?? []).slice().sort((a, b) => b.waiting - a.waiting).map((r) => (
                  <tr key={r.isp}>
                    <td style={tdStyle}>{r.isp}</td>
                    <td style={numTd}>{num(r.waiting)}</td>
                    <td style={numTd}>{pctOf(r.waiting, edgeOut.waiting)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : (
            <div style={{ fontSize: 11, color: colors.textFaint }}>
              No per-ISP split reported for this edge — unknown, not zero.
            </div>
          )}
        </>
      ) : (
        <div style={{ fontSize: 11, color: colors.textFaint }}>
          No outgoing edge reported for this touch — either it is the last rung or the server sent no edge (unknown, not zero).
        </div>
      )}
      <div style={noteStyle}>
        Drip-wide due-now backlog: <b>{num(dueNow)}</b> (rows whose next_touch_at already passed,
        every rung). Edge counts above are populations sitting between two specific rungs.
      </div>
    </div>
  );
};

export default DripStepShelf;
