import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faRoute, faUpload, faTriangleExclamation, faCircleCheck, faEnvelope,
  faClock, faFlagCheckered, faBolt, faSpinner, faArrowRight, faCircleInfo,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import { colors } from '../shared/theme';
import { Panel, SectionHeader, Stat, SectionError, EmptyState, Pill, ProgressBar } from '../shared/ui';

/**
 * Click Funnels — operator surface for the click-drip offer lanes.
 *
 * Layout follows the HubSpot workflow-performance model: a lane list, then a
 * vertical node canvas with the funnel's metrics rendered ON the nodes, and a
 * global Rates/Counts toggle (HubSpot's single most useful affordance — the same
 * canvas answers "how many" and "what %" without a second screen).
 *
 * Two honesty rules are baked in and must not be "cleaned up":
 *  - Conversions come from converted_at (the Everflow postback), never from
 *    enrollment status='converted', which is the terminal goal node firing on
 *    sequence completion and overstates conversions ~81x.
 *  - Per-node engagement only exists for touches sent after node-level
 *    attribution shipped. Nodes without it render an explicit "not attributed"
 *    marker rather than a zero that reads as "nobody opened it".
 */

// ---------------------------------------------------------------- types

interface Lane {
  offer_id: string;
  offer_name: string;
  journey_id: string;
  journey_name: string;
  enabled: boolean;
  payout_type: string;
  routing_state: string;
  redirect_offer_id: string;
  routing_recommendation: string;
  slug_inlets: number;
  active_enrollments: number;
  enrolled_30d: number;
  conversions_30d: number;
  touches_30d: number;
  configured_touches: number;
}

interface LaneList {
  lanes: Lane[];
  unmapped_slug_offers: string[];
}

interface FunnelNode {
  node_id: string;
  type: string;
  label: string;
  sequence_index: number;
  delay_ms: number;
  subject: string;
  preheader: string;
  from_name_override: string;
  copy_enabled: boolean;
  copy_missing: boolean;
  body_html: string;
  body_inherited: boolean;
  proof_id: string;
  proof_name: string;
  proof_offer_key: string;
  proof_approval_status: string;
  proof_active: boolean;
  proof_sendable: boolean;
  copy_updated_at: string;
  copy_age_days: number;
  versions: TouchVersion[];
  reached: number;
  awaiting: number;
  errors: number;
  sent: number;
  delivered: number;
  opens: number;
  clicks: number;
  unsubscribes: number;
  hard_bounce: number;
  soft_bounce: number;
  conversions: number;
  human_clicks: number;
  deferred: number;
  open_rate: number;
  click_rate: number;
  human_click_rate: number;
  conversion_rate: number;
  step_through_rate: number;
  attributed: boolean;
}

// One approved advertiser proof from Creative Studio's OFFERS sub-view
// (GET /api/mailing/offer-proofs) — mailing_offer_proofs, not the library.
interface OfferProof {
  id: string;
  name: string;
  offer_key: string;
  approval_status: string;
  is_active: boolean;
  from_names?: string[];
  approved_domains?: string[];
  approved_isps?: string[];
}

interface TouchVersion {
  content_hash: string;
  subject: string;
  preheader: string;
  from_name_override: string;
  body_html: string;
  is_live: boolean;
  first_seen_at: string;
  last_seen_at: string;
  superseded_at: string;
  sent: number;
  delivered: number;
  opens: number;
  clicks: number;
  human_clicks: number;
  open_rate: number;
  click_rate: number;
  attributed: boolean;
}

interface NodesResponse {
  offer_id: string;
  offer_name: string;
  journey_id: string;
  nodes: FunnelNode[];
  total_enrolled: number;
  total_active: number;
  total_converted: number;
  total_completed: number;
  total_exited: number;
  median_hours_to_convert: number | null;
  completion_rate: number;
  conversion_rate: number;
  attribution_note: string;
  engagement_source: string;
  window_from: string;
  window_to: string;
}

interface NodeEnrollmentRow {
  enrollment_id: string;
  email: string;
  status: string;
  current_node_id: string;
  executed_at: string;
  action: string;
  error_message: string;
  converted_at: string | null;
  exit_reason: string;
}

interface UploadPreview {
  offer_id: string;
  journey_id: string;
  lane_enabled: boolean;
  submitted: number;
  malformed: number;
  duplicates_in_file: number;
  unknown_subscriber: number;
  already_active: number;
  already_converted: number;
  recently_triggered: number;
  ready: number;
  sample_ready: string[];
  sample_unknown: string[];
  warnings: string[];
}

interface UploadResult {
  enqueued: number;
  skipped: number;
  note: string;
}

// ---------------------------------------------------------------- helpers

const n = (v: number) => (v ?? 0).toLocaleString();
const pct = (v: number) => `${(v ?? 0).toFixed(2)}%`;

const humanDelay = (ms: number): string => {
  if (!ms) return '';
  const h = ms / 3600000;
  if (h >= 24 && h % 24 === 0) return `${h / 24}d`;
  if (h >= 1) return `${h % 1 === 0 ? h : h.toFixed(1)}h`;
  return `${Math.round(ms / 60000)}m`;
};

const routingColor = (state: string) =>
  state === 'paused_auto' ? colors.warning
    : state === 'redirect' ? colors.indigo300
    : colors.success;

// A dated deadline that has already passed is the failure this screen exists to
// catch: offer 420 shipped "Ends 7/5" for four weeks after it expired. Match
// explicit M/D or Month-D references in the copy and compare to today.
const expiredDateInCopy = (text: string): string | null => {
  if (!text) return null;
  const now = new Date();
  const md = text.match(/\b(\d{1,2})\/(\d{1,2})\b/);
  if (md) {
    const m = parseInt(md[1], 10), d = parseInt(md[2], 10);
    if (m >= 1 && m <= 12 && d >= 1 && d <= 31) {
      const cand = new Date(now.getFullYear(), m - 1, d);
      // Only flag a date in the recent past; a far-future one is a live promo.
      const daysAgo = (now.getTime() - cand.getTime()) / 86400000;
      if (daysAgo > 1 && daysAgo < 300) return md[0];
    }
  }
  return null;
};

const editInput: React.CSSProperties = {
  background: 'rgba(10,16,32,0.6)',
  color: colors.text,
  border: `1px solid ${colors.panelBorder}`,
  borderRadius: 6,
  padding: '6px 9px',
  fontSize: 12,
  width: '100%',
};

const btnPrimary: React.CSSProperties = {
  background: colors.indigo500, color: '#fff', border: 'none', borderRadius: 6,
  padding: '6px 14px', fontSize: 12, fontWeight: 600, cursor: 'pointer',
};

const btnGhost: React.CSSProperties = {
  background: 'transparent', color: colors.textMuted,
  border: `1px solid ${colors.panelBorder}`, borderRadius: 6,
  padding: '6px 12px', fontSize: 12, cursor: 'pointer',
};

// PAYOUT_TYPES mirrors payoutTypeAllowed() in click_drip_admin_handlers.go,
// which itself mirrors the CHECK constraint. Sending anything else yields a 400.
const PAYOUT_TYPES = ['CPM', 'eCPM', 'CPA', 'CPL', 'CPC', 'IO', 'PRV', 'UNKNOWN'];

// LaneEditor creates or updates one offer lane through the existing
// offer-journey-map upsert. It deliberately does NOT edit the journey GRAPH:
// every lane currently points at the same journey row, so changing a delay or
// adding a touch there would silently re-shape all ~22 funnels at once. That
// needs a per-lane journey before it can be safe, so the graph is read-only
// here and the reason is stated in the UI rather than hidden.
const LaneEditor: React.FC<{
  lane: Lane | null;
  presetOffer?: string;
  journeyOptions: { id: string; name: string }[];
  onSaved: (offerId: string) => void;
  onCancel: () => void;
}> = ({ lane, presetOffer, journeyOptions, onSaved, onCancel }) => {
  const [offerId, setOfferId] = useState(lane?.offer_id ?? presetOffer ?? '');
  const [journeyId, setJourneyId] = useState(lane?.journey_id ?? journeyOptions[0]?.id ?? '');
  const [payout, setPayout] = useState(lane?.payout_type || 'UNKNOWN');
  const [enabled, setEnabled] = useState(lane?.enabled ?? false);
  const [notes, setNotes] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const isNew = !lane;

  const save = async () => {
    if (!offerId.trim()) { setErr('Offer id is required.'); return; }
    setBusy(true); setErr(null);
    try {
      const res = await apiFetch(`/api/mailing/offer-journey-map/${encodeURIComponent(offerId.trim())}`, {
        method: 'PUT',
        body: JSON.stringify({
          click_journey_id: journeyId,
          payout_type: payout,
          enabled,
          notes,
        }),
      });
      const body = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(body?.error || `HTTP ${res.status}`);
      onSaved(offerId.trim());
    } catch (e: any) {
      setErr(e?.message ?? String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Panel accent={colors.indigo500} style={{ padding: 16 }}>
      <SectionHeader
        title={isNew ? 'Create click funnel' : (lane!.offer_name || `Offer ${lane!.offer_id}`)}
        icon={faRoute}
      />
      <div style={{ display: 'grid', gap: 8 }}>
        <label style={{ fontSize: 11, color: colors.textMuted }}>
          Everflow offer id
          <input
            value={offerId}
            onChange={e => setOfferId(e.target.value)}
            disabled={!isNew}
            placeholder="e.g. 1054"
            style={{ ...editInput, marginTop: 3, opacity: isNew ? 1 : 0.6 }}
          />
        </label>
        <label style={{ fontSize: 11, color: colors.textMuted }}>
          Journey
          <select value={journeyId} onChange={e => setJourneyId(e.target.value)} style={{ ...editInput, marginTop: 3 }}>
            {journeyOptions.length === 0 && <option value="">(no journeys found)</option>}
            {journeyOptions.map(j => <option key={j.id} value={j.id}>{j.name || j.id}</option>)}
          </select>
        </label>
        <label style={{ fontSize: 11, color: colors.textMuted }}>
          Payout type
          <select value={payout} onChange={e => setPayout(e.target.value)} style={{ ...editInput, marginTop: 3 }}>
            {PAYOUT_TYPES.map(p => <option key={p} value={p}>{p}</option>)}
          </select>
        </label>
        <label style={{ fontSize: 11, color: colors.textMuted }}>
          Notes
          <input value={notes} onChange={e => setNotes(e.target.value)} placeholder="why this lane exists" style={{ ...editInput, marginTop: 3 }} />
        </label>
        <label style={{ fontSize: 12, color: colors.text, display: 'flex', alignItems: 'center', gap: 8 }}>
          <input type="checkbox" checked={enabled} onChange={e => setEnabled(e.target.checked)} />
          Enabled — clicks on this offer enroll immediately
        </label>
        {err && <div style={{ fontSize: 12, color: colors.danger }}>{err}</div>}
        <div style={{ display: 'flex', gap: 8, marginTop: 2 }}>
          <button onClick={save} disabled={busy} style={btnPrimary}>{busy ? 'Saving…' : isNew ? 'Create funnel' : 'Save'}</button>
          <button onClick={onCancel} disabled={busy} style={btnGhost}>Cancel</button>
        </div>
      </div>
    </Panel>
  );
};

// ---------------------------------------------------------------- node card

const NodeCard: React.FC<{
  node: FunnelNode;
  mode: 'rates' | 'counts';
  entryReached: number;
  offerId: string;
  onSaved: () => void;
}> = ({ node, mode, entryReached, offerId, onSaved }) => {
  const [drill, setDrill] = useState<NodeEnrollmentRow[] | null>(null);
  const [drillErrOnly, setDrillErrOnly] = useState(false);
  const [drillBusy, setDrillBusy] = useState(false);

  const loadDrill = async (errorsOnly: boolean) => {
    setDrillBusy(true); setDrillErrOnly(errorsOnly);
    try {
      const qs = errorsOnly ? '?action=error&limit=50' : '?limit=50';
      const res = await apiFetch(
        `/api/mailing/click-funnels/${encodeURIComponent(offerId)}/nodes/${encodeURIComponent(node.node_id)}/enrollments${qs}`
      );
      const body = await res.json();
      setDrill(res.ok ? (body.enrollments ?? []) : []);
    } catch {
      setDrill([]);
    } finally {
      setDrillBusy(false);
    }
  };
  const isEmail = node.type === 'email';
  const isGoal = node.type === 'goal';
  const isDelay = node.type === 'delay';

  const [editing, setEditing] = useState(false);
  const [expanded, setExpanded] = useState(false);
  const [picking, setPicking] = useState(false);
  const [studio, setStudio] = useState<OfferProof[] | null>(null);
  const [studioFilter, setStudioFilter] = useState('');
  const [bodySaving, setBodySaving] = useState(false);
  const [bodyErr, setBodyErr] = useState<string | null>(null);
  const [draft, setDraft] = useState({ subject: '', preheader: '', from_name_override: '', enabled: true });
  const [saving, setSaving] = useState(false);
  const [saveErr, setSaveErr] = useState<string | null>(null);

  const openEditor = () => {
    setDraft({
      subject: node.subject,
      preheader: node.preheader,
      from_name_override: node.from_name_override,
      enabled: node.copy_missing ? true : node.copy_enabled,
    });
    setSaveErr(null);
    setEditing(true);
  };

  const save = async () => {
    if (!draft.subject.trim()) { setSaveErr('Subject is required.'); return; }
    setSaving(true); setSaveErr(null);
    try {
      const res = await apiFetch(
        `/api/mailing/offer-reminder-subjects/${encodeURIComponent(offerId)}/${node.sequence_index}`,
        { method: 'PUT', body: JSON.stringify(draft) }
      );
      const body = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(body?.error || `HTTP ${res.status}`);
      setEditing(false);
      onSaved();
    } catch (e: any) {
      setSaveErr(e?.message ?? String(e));
    } finally {
      setSaving(false);
    }
  };

  if (isDelay) {
    return (
      <div style={{ display: 'flex', alignItems: 'center', gap: 10, padding: '4px 0 4px 22px', color: colors.textMuted, fontSize: 12 }}>
        <FontAwesomeIcon icon={faClock} style={{ color: colors.indigo400 }} />
        <span>wait {humanDelay(node.delay_ms) || node.label}</span>
        {node.awaiting > 0 && (
          <Pill color={colors.indigo300}>{n(node.awaiting)} waiting here</Pill>
        )}
      </div>
    );
  }

  if (isGoal) {
    return (
      <div style={{ display: 'flex', alignItems: 'center', gap: 10, padding: '8px 0 0 22px', color: colors.textMuted, fontSize: 12 }}>
        <FontAwesomeIcon icon={faFlagCheckered} style={{ color: colors.indigo300 }} />
        <span>sequence complete — {n(node.reached)} finished all touches</span>
      </div>
    );
  }

  const metrics = isEmail
    ? mode === 'rates'
      ? [
          { label: 'Step-through', value: pct(node.step_through_rate), title: `${n(node.reached)} of ${n(entryReached)} who entered reached this touch` },
          { label: 'Open rate', value: node.attributed ? pct(node.open_rate) : '—', title: 'Opens ÷ delivered (lake opens carry no machine flag — raw)' },
          { label: 'Click rate', value: node.attributed ? pct(node.click_rate) : '—', title: 'All clicks ÷ delivered for this touch' },
          { label: 'Human click', value: node.attributed ? pct(node.human_click_rate) : '—', title: 'is_machine_click=false ÷ delivered — the trustworthy engagement signal' },
          { label: 'Conv rate', value: pct(node.conversion_rate), title: 'Last-touch conversions ÷ people who reached this touch' },
        ]
      : [
          { label: 'Reached', value: n(node.reached), title: 'Distinct enrollments that executed this node' },
          { label: 'Opens', value: node.attributed ? n(node.opens) : '—', title: 'Open events on this touch (raw — no machine flag in the lake)' },
          { label: 'Clicks', value: node.attributed ? n(node.clicks) : '—', title: 'All click events on this touch' },
          { label: 'Human', value: node.attributed ? n(node.human_clicks) : '—', title: 'Clicks with is_machine_click=false' },
          { label: 'Conversions', value: n(node.conversions), title: 'Conversions last-touch attributed to this node' },
        ]
    : [];

  // The touch stores a REFERENCE into the Creative Studio registry, not a copy
  // of the HTML. Studio is the platform's source of truth for creatives, so
  // pointing at one keeps its approval state, money-link check, preview and
  // proof-send — all of which a pasted blob would have thrown away. Saving goes
  // through the same reminder-subjects upsert as the rest of the copy, so
  // subject/preheader/from and creative stay one atomic creative version.
  // Creative Studio → Offers. Only ACTIVE proofs are listed; the sender will
  // refuse anything not approved AND active, so offering the rest would invite
  // selecting something that silently never mails.
  const loadStudio = async () => {
    try {
      const res = await apiFetch('/api/mailing/offer-proofs?active=true');
      const body = await res.json();
      const rows: OfferProof[] = body?.proofs ?? [];
      const q = studioFilter.trim().toLowerCase();
      setStudio(q ? rows.filter(p =>
        `${p.name} ${p.offer_key}`.toLowerCase().includes(q)) : rows);
    } catch {
      setStudio([]);
    }
  };

  const selectCreative = async (creativeId: string) => {
    setBodySaving(true); setBodyErr(null);
    try {
      const res = await apiFetch(
        `/api/mailing/offer-reminder-subjects/${encodeURIComponent(offerId)}/${node.sequence_index}`,
        {
          method: 'PUT',
          body: JSON.stringify({
            subject: node.subject,
            preheader: node.preheader,
            from_name_override: node.from_name_override,
            enabled: node.copy_missing ? true : node.copy_enabled,
            proof_id: creativeId,
          }),
        }
      );
      const b = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(b?.error || `HTTP ${res.status}`);
      setPicking(false);
      onSaved();
    } catch (e: any) {
      setBodyErr(e?.message ?? String(e));
    } finally {
      setBodySaving(false);
    }
  };

  const staleDate = expiredDateInCopy(`${node.subject} ${node.preheader}`);

  return (
    <Panel
      accent={node.errors > 0 ? colors.warning : colors.indigo500}
      style={{ padding: 14, marginBottom: 2 }}
    >
      <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 14, flexWrap: 'wrap' }}>
        <div style={{ minWidth: 240, flex: '1 1 300px' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 4 }}>
            <FontAwesomeIcon icon={faEnvelope} style={{ color: colors.indigo400 }} />
            <span style={{ fontWeight: 700, color: colors.heading, fontSize: 13 }}>
              Touch {node.sequence_index >= 0 ? node.sequence_index + 1 : '?'}
            </span>
            <span style={{ fontSize: 11, color: colors.textFaint }}>{node.node_id}</span>
            {!node.copy_enabled && !node.copy_missing && <Pill color={colors.warning}>copy disabled</Pill>}
            {node.copy_missing && <Pill color={colors.warning}>no copy row</Pill>}
            {!node.attributed && <Pill color={colors.textFaint}>not node-attributed</Pill>}
            {staleDate && <Pill color={colors.danger}>expired date “{staleDate}”</Pill>}
            {node.copy_age_days > 30 && <Pill color={colors.warning}>copy {node.copy_age_days}d old</Pill>}
            {node.body_inherited && <Pill color={colors.textFaint}>body inherited</Pill>}
          </div>

          {/* Creative for this node. Body HTML is per-subscriber (the clicked
              campaign's creative is reused), so subject/preheader is the whole
              of the node-level creative — showing a fake body would mislead. */}
          {editing ? (
            <div style={{ display: 'grid', gap: 6, marginTop: 4 }}>
              <input
                value={draft.subject}
                onChange={e => setDraft({ ...draft, subject: e.target.value })}
                placeholder="Subject (required)"
                style={editInput}
              />
              <input
                value={draft.preheader}
                onChange={e => setDraft({ ...draft, preheader: e.target.value })}
                placeholder="Preheader"
                style={editInput}
              />
              <input
                value={draft.from_name_override}
                onChange={e => setDraft({ ...draft, from_name_override: e.target.value })}
                placeholder="From-name override (blank = offer default)"
                style={editInput}
              />
              <label style={{ fontSize: 11, color: colors.textMuted, display: 'flex', alignItems: 'center', gap: 6 }}>
                <input
                  type="checkbox"
                  checked={draft.enabled}
                  onChange={e => setDraft({ ...draft, enabled: e.target.checked })}
                />
                touch enabled
              </label>
              {saveErr && <div style={{ fontSize: 11, color: colors.danger }}>{saveErr}</div>}
              <div style={{ display: 'flex', gap: 8 }}>
                <button onClick={save} disabled={saving} style={btnPrimary}>
                  {saving ? 'Saving…' : 'Save'}
                </button>
                <button onClick={() => setEditing(false)} disabled={saving} style={btnGhost}>Cancel</button>
              </div>
            </div>
          ) : (
            <>
              <div style={{ fontSize: 13, color: colors.text, lineHeight: 1.45 }}>
                {node.subject || <span style={{ color: colors.textFaint, fontStyle: 'italic' }}>inherits the clicked campaign’s subject</span>}
              </div>
              {node.preheader && (
                <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 3 }}>{node.preheader}</div>
              )}
              {node.from_name_override && (
                <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 3 }}>from: {node.from_name_override}</div>
              )}
              <div style={{ fontSize: 11, color: colors.textFaint, marginTop: 6, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                <span>
                  {node.proof_id
                    ? `proof: ${node.proof_name || node.proof_id}`
                    : node.body_inherited
                      ? 'no offer proof selected — inherits whatever this subscriber clicked'
                      : 'body snapshot set for this touch'}
                </span>
                <button onClick={() => setExpanded(v => !v)} style={{ ...btnGhost, padding: '2px 8px', fontSize: 11 }}>
                  {expanded ? 'Hide creative' : 'View creative'}
                </button>
                <button onClick={openEditor} style={{ ...btnGhost, padding: '2px 8px', fontSize: 11 }}>
                  Edit copy
                </button>
              </div>
            </>
          )}
        </div>

        <div style={{ display: 'flex', gap: 18, flexWrap: 'wrap' }}>
          {metrics.map(m => (
            <Stat key={m.label} label={m.label} value={m.value} title={m.title} />
          ))}
        </div>
      </div>

      {(node.awaiting > 0 || node.errors > 0 || node.deferred > 0) && (
        <div style={{ display: 'flex', gap: 12, marginTop: 10, fontSize: 11, color: colors.textMuted, flexWrap: 'wrap' }}>
          {node.awaiting > 0 && <span>{n(node.awaiting)} currently waiting at this node</span>}
          {node.deferred > 0 && <span>{n(node.deferred)} deferred (unique mailboxes)</span>}
          {node.errors > 0 && (
            <span style={{ color: colors.warning }}>
              <FontAwesomeIcon icon={faTriangleExclamation} /> {n(node.errors)} send errors (retried, not skipped)
            </span>
          )}
        </div>
      )}

      {/* Expanded creative: WHICH Creative Studio creative this touch sends,
          with the registry's own approval + money-link state, plus the version
          history whose superseded entries are frozen aggregates. */}
      {expanded && (
        <div style={{ marginTop: 12, borderTop: `1px solid ${colors.divider}`, paddingTop: 12 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 8, flexWrap: 'wrap' }}>
            <span style={{ fontSize: 11, color: colors.textMuted, textTransform: 'uppercase', letterSpacing: 0.5 }}>
              Offer proof
            </span>
            {node.proof_id ? (
              <>
                <span style={{ fontSize: 12, color: colors.text }}>{node.proof_name || node.proof_id}</span>
                {node.proof_offer_key && (
                  <span style={{ fontSize: 10, color: colors.textFaint }}>{node.proof_offer_key}</span>
                )}
                <Pill color={node.proof_approval_status === 'approved' ? colors.success : colors.warning}>
                  {node.proof_approval_status || 'unreviewed'}
                </Pill>
                <Pill color={node.proof_active ? colors.success : colors.warning}>
                  {node.proof_active ? 'active' : 'inactive'}
                </Pill>
                {!node.proof_sendable && (
                  <Pill color={colors.danger}>WILL NOT SEND — needs approved + active</Pill>
                )}
              </>
            ) : (
              <span style={{ fontSize: 12, color: colors.textFaint }}>
                none selected — each subscriber receives the creative they originally clicked
              </span>
            )}
            <button
              onClick={() => { setPicking(v => !v); if (!studio) loadStudio(); }}
              disabled={bodySaving}
              style={{ ...btnGhost, padding: '2px 10px', fontSize: 11 }}
            >
              {picking ? 'Close' : node.proof_id ? 'Change proof' : 'Choose an offer proof'}
            </button>
            {node.proof_id && (
              <a
                href={`/api/mailing/offer-proofs/${encodeURIComponent(node.proof_id)}/preview`}
                target="_blank" rel="noreferrer"
                style={{ ...btnGhost, padding: '2px 10px', fontSize: 11, textDecoration: 'none' }}
              >
                Open preview
              </a>
            )}
          </div>

          {bodyErr && <div style={{ fontSize: 11, color: colors.danger, marginBottom: 6 }}>{bodyErr}</div>}

          {picking && (
            <div style={{ border: `1px solid ${colors.panelBorder}`, borderRadius: 6, padding: 10, marginBottom: 10 }}>
              <div style={{ display: 'flex', gap: 8, marginBottom: 8 }}>
                <input
                  value={studioFilter}
                  onChange={e => setStudioFilter(e.target.value)}
                  onKeyDown={e => { if (e.key === 'Enter') loadStudio(); }}
                  placeholder="filter proofs by name or offer key (e.g. metal)"
                  style={{ ...editInput, flex: 1 }}
                />
                <button onClick={loadStudio} style={{ ...btnGhost, padding: '4px 12px', fontSize: 11 }}>Search</button>
              </div>
              <div style={{ maxHeight: 260, overflowY: 'auto' }}>
                {studio == null ? (
                  <div style={{ fontSize: 11, color: colors.textFaint }}>Loading offer proofs…</div>
                ) : studio.length === 0 ? (
                  <div style={{ fontSize: 11, color: colors.textFaint }}>No active offer proofs match.</div>
                ) : (
                  <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 11 }}>
                    <tbody>
                      {studio.map(cv => (
                        <tr key={cv.id} style={{ borderTop: `1px solid ${colors.divider}` }}>
                          <td style={{ padding: '5px 6px', color: colors.text }}>
                            {cv.name || cv.id}
                            <div style={{ color: colors.textFaint, fontSize: 10 }}>{cv.offer_key}</div>
                          </td>
                          <td style={{ padding: '5px 6px' }}>
                            <Pill color={cv.approval_status === 'approved' ? colors.success : colors.warning}>
                              {cv.approval_status || 'unreviewed'}
                            </Pill>
                          </td>
                          <td style={{ padding: '5px 6px' }}>
                            {cv.approval_status !== 'approved' && (
                              <span style={{ fontSize: 10, color: colors.danger }}>not sendable</span>
                            )}
                          </td>
                          <td style={{ padding: '5px 6px', textAlign: 'right', whiteSpace: 'nowrap' }}>
                            <a href={`/api/mailing/offer-proofs/${cv.id}/preview`} target="_blank" rel="noreferrer"
                               style={{ ...btnGhost, padding: '2px 8px', fontSize: 10, textDecoration: 'none', marginRight: 6 }}>
                              preview
                            </a>
                            <button
                              onClick={() => selectCreative(cv.id)}
                              disabled={bodySaving || cv.id === node.proof_id || cv.approval_status !== 'approved'}
                              style={{ ...btnPrimary, padding: '2px 10px', fontSize: 10 }}
                              title={cv.approval_status !== 'approved' ? 'Only approved proofs may send' : undefined}
                            >
                              {cv.id === node.proof_id ? 'current' : 'Use'}
                            </button>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                )}
              </div>
              <div style={{ fontSize: 10, color: colors.textFaint, marginTop: 8 }}>
                Only approved + active proofs can send. Selecting one mints a new version — this touch's current metrics freeze as a
                historical aggregate and the new creative starts its own lifetime record.
              </div>
            </div>
          )}

          {/* Version history */}
          <div style={{ marginTop: 14 }}>
            <div style={{ fontSize: 11, color: colors.textMuted, textTransform: 'uppercase', letterSpacing: 0.5, marginBottom: 6 }}>
              Creative versions — lifetime metrics per subject + creative
            </div>
            {(!node.versions || node.versions.length === 0) ? (
              <div style={{ fontSize: 11, color: colors.textFaint }}>
                No versions recorded yet. A version is registered the first time this touch sends;
                changing the creative or any copy field starts a new one.
              </div>
            ) : (
              <div style={{ display: 'grid', gap: 6 }}>
                {node.versions.map(v => (
                  <div
                    key={v.content_hash}
                    style={{
                      border: `1px solid ${v.is_live ? colors.panelBorderStrong : colors.divider}`,
                      borderRadius: 6, padding: 8,
                      opacity: v.is_live ? 1 : 0.72,
                    }}
                  >
                    <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap', marginBottom: 4 }}>
                      <Pill color={v.is_live ? colors.success : colors.textFaint}>
                        {v.is_live ? 'live' : 'sunset'}
                      </Pill>
                      <span style={{ fontSize: 12, color: colors.text }}>{v.subject || '(no subject)'}</span>
                      <span style={{ fontSize: 10, color: colors.textFaint }}>{v.content_hash}</span>
                    </div>
                    <div style={{ fontSize: 10, color: colors.textFaint, marginBottom: 4 }}>
                      {v.is_live
                        ? `live since ${v.first_seen_at?.slice(0, 10)}`
                        : `${v.first_seen_at?.slice(0, 10)} → ${(v.superseded_at || v.last_seen_at)?.slice(0, 10)} · frozen historical aggregate`}
                    </div>
                    <div style={{ display: 'flex', gap: 16, flexWrap: 'wrap' }}>
                      <Stat label="Delivered" value={v.attributed ? n(v.delivered) : '—'} />
                      <Stat label="Open rate" value={v.attributed ? pct(v.open_rate) : '—'} />
                      <Stat label="Click rate" value={v.attributed ? pct(v.click_rate) : '—'} />
                      <Stat label="Human clicks" value={v.attributed ? n(v.human_clicks) : '—'} />
                    </div>
                  </div>
                ))}
              </div>
            )}
          </div>
        </div>
      )}

      {/* Matching enrollments — the records behind the aggregate. */}
      <div style={{ display: 'flex', gap: 8, marginTop: 10, flexWrap: 'wrap' }}>
        <button onClick={() => loadDrill(false)} disabled={drillBusy} style={{ ...btnGhost, padding: '3px 10px', fontSize: 11 }}>
          {drillBusy && !drillErrOnly ? 'Loading…' : 'Matching enrollments'}
        </button>
        {node.errors > 0 && (
          <button onClick={() => loadDrill(true)} disabled={drillBusy} style={{ ...btnGhost, padding: '3px 10px', fontSize: 11, color: colors.warning }}>
            {drillBusy && drillErrOnly ? 'Loading…' : `Show ${n(node.errors)} failures`}
          </button>
        )}
        {drill && (
          <button onClick={() => setDrill(null)} style={{ ...btnGhost, padding: '3px 10px', fontSize: 11 }}>Hide</button>
        )}
      </div>

      {drill && (
        <div style={{ marginTop: 8, maxHeight: 260, overflow: 'auto', border: `1px solid ${colors.divider}`, borderRadius: 6 }}>
          {drill.length === 0 ? (
            <div style={{ padding: 10, fontSize: 11, color: colors.textFaint }}>No matching enrollments.</div>
          ) : (
            <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 11 }}>
              <tbody>
                {drill.map(d => (
                  <tr key={d.enrollment_id} style={{ borderTop: `1px solid ${colors.divider}` }}>
                    <td style={{ padding: '5px 8px', color: colors.text }}>{d.email || d.enrollment_id}</td>
                    <td style={{ padding: '5px 8px', color: colors.textMuted, whiteSpace: 'nowrap' }}>{d.status}</td>
                    <td style={{ padding: '5px 8px', color: colors.textFaint, whiteSpace: 'nowrap' }}>{d.executed_at}</td>
                    <td style={{ padding: '5px 8px', color: d.action === 'error' ? colors.warning : colors.textMuted }}>
                      {d.action === 'error' ? (d.error_message || 'error') : d.action}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      )}

      <div style={{ marginTop: 10 }}>
        <ProgressBar pct={node.step_through_rate} />
      </div>
    </Panel>
  );
};

// ---------------------------------------------------------------- upload

const UploadPanel: React.FC<{ offerId: string; laneLabel: string; onEnrolled: () => void }> = ({ offerId, laneLabel, onEnrolled }) => {
  const [raw, setRaw] = useState('');
  const [preview, setPreview] = useState<UploadPreview | null>(null);
  const [result, setResult] = useState<UploadResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const reset = () => { setPreview(null); setResult(null); setErr(null); };

  const doPreview = async () => {
    setBusy(true); setErr(null); setResult(null);
    try {
      const res = await apiFetch('/api/mailing/click-funnels/upload/preview', {
        method: 'POST',
        body: JSON.stringify({ offer_id: offerId, raw }),
      });
      const body = await res.json();
      if (!res.ok) throw new Error(body?.error || `HTTP ${res.status}`);
      setPreview(body);
    } catch (e: any) {
      setErr(e?.message ?? String(e));
    } finally {
      setBusy(false);
    }
  };

  const doEnroll = async () => {
    if (!preview) return;
    setBusy(true); setErr(null);
    try {
      const res = await apiFetch('/api/mailing/click-funnels/upload/enroll', {
        method: 'POST',
        body: JSON.stringify({ offer_id: offerId, raw, confirm: true }),
      });
      const body = await res.json();
      if (!res.ok) throw new Error(body?.error || `HTTP ${res.status}`);
      setResult(body);
      setPreview(null);
      onEnrolled();
    } catch (e: any) {
      setErr(e?.message ?? String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Panel style={{ padding: 16 }}>
      <SectionHeader
        title="Upload clickers"
        icon={faUpload}
        right={<span style={{ fontSize: 11, color: colors.textMuted }}>into {laneLabel}</span>}
      />
      <div style={{ fontSize: 12, color: colors.textMuted, marginBottom: 8 }}>
        Paste the <strong>sub1</strong> column from your offer-network report — one subscriber id per line
        (commas, tabs and a header row are fine). Nothing is enrolled until you confirm.
      </div>

      <textarea
        value={raw}
        onChange={e => { setRaw(e.target.value); reset(); }}
        placeholder={'sub1\n3f6c9d2a-1b44-4c8e-9f21-7a5e2c4d8b90\n8a1e7b33-5c92-4d17-b6e4-1f30a9c2d745'}
        spellCheck={false}
        style={{
          width: '100%', minHeight: 130, resize: 'vertical',
          background: 'rgba(10,16,32,0.6)', color: colors.text,
          border: `1px solid ${colors.panelBorder}`, borderRadius: 8,
          padding: 10, fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
          fontSize: 12, lineHeight: 1.5,
        }}
      />

      <div style={{ display: 'flex', gap: 10, marginTop: 10, alignItems: 'center' }}>
        <button
          onClick={doPreview}
          disabled={busy || !raw.trim()}
          style={{
            background: colors.indigo500, color: '#fff', border: 'none', borderRadius: 8,
            padding: '8px 16px', fontSize: 13, fontWeight: 600,
            cursor: busy || !raw.trim() ? 'not-allowed' : 'pointer', opacity: busy || !raw.trim() ? 0.5 : 1,
          }}
        >
          {busy ? <><FontAwesomeIcon icon={faSpinner} spin /> Working…</> : 'Preview'}
        </button>
        {preview && (
          <button
            onClick={doEnroll}
            disabled={busy || preview.ready === 0}
            style={{
              background: preview.ready === 0 ? 'rgba(100,116,139,0.3)' : colors.success,
              color: '#fff', border: 'none', borderRadius: 8,
              padding: '8px 16px', fontSize: 13, fontWeight: 700,
              cursor: preview.ready === 0 ? 'not-allowed' : 'pointer',
            }}
          >
            Enroll {n(preview.ready)}
          </button>
        )}
      </div>

      {err && <div style={{ marginTop: 10 }}><SectionError label="Upload" error={err} /></div>}

      {preview && (
        <div style={{ marginTop: 14, borderTop: `1px solid ${colors.divider}`, paddingTop: 12 }}>
          <div style={{ display: 'flex', gap: 18, flexWrap: 'wrap' }}>
            <Stat label="Ready" value={n(preview.ready)} color={colors.success} title="Will be queued for enrollment" />
            <Stat label="Already active" value={n(preview.already_active)} title="Already in this funnel" />
            <Stat label="Converted" value={n(preview.already_converted)} title="Already converted — never re-enrolled" />
            <Stat label="Recently queued" value={n(preview.recently_triggered)} title="Triggered for this offer in the last 24h" />
            <Stat label="Unknown" value={n(preview.unknown_subscriber)} title="No subscriber row for this id" />
            <Stat label="Malformed" value={n(preview.malformed)} title="Not a valid subscriber id" />
            <Stat label="Dupes in paste" value={n(preview.duplicates_in_file)} />
          </div>
          {preview.warnings?.map((wmsg, i) => (
            <div key={i} style={{ marginTop: 10, fontSize: 12, color: colors.warning }}>
              <FontAwesomeIcon icon={faTriangleExclamation} /> {wmsg}
            </div>
          ))}
          {preview.sample_unknown?.length > 0 && (
            <div style={{ marginTop: 8, fontSize: 11, color: colors.textFaint, fontFamily: 'ui-monospace, monospace' }}>
              unrecognized sample: {preview.sample_unknown.join(', ')}
            </div>
          )}
          <div style={{ marginTop: 10, fontSize: 11, color: colors.textMuted }}>
            <FontAwesomeIcon icon={faCircleInfo} /> Enrolling starts the sequence — the first touch fires on the
            first node’s delay. Accepted sends cannot be recalled.
          </div>
        </div>
      )}

      {result && (
        <div style={{ marginTop: 14, borderTop: `1px solid ${colors.divider}`, paddingTop: 12 }}>
          <div style={{ fontSize: 13, color: colors.success, fontWeight: 600 }}>
            <FontAwesomeIcon icon={faCircleCheck} /> Queued {n(result.enqueued)} subscriber{result.enqueued === 1 ? '' : 's'}
            {result.skipped > 0 && <span style={{ color: colors.textMuted, fontWeight: 400 }}> · {n(result.skipped)} skipped</span>}
          </div>
          <div style={{ marginTop: 6, fontSize: 11, color: colors.textMuted }}>{result.note}</div>
        </div>
      )}
    </Panel>
  );
};

// ---------------------------------------------------------------- main

export const ClickFunnels: React.FC = () => {
  const [list, setList] = useState<LaneList | null>(null);
  const [listErr, setListErr] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [detail, setDetail] = useState<NodesResponse | null>(null);
  const [detailErr, setDetailErr] = useState<string | null>(null);
  const [mode, setMode] = useState<'rates' | 'counts'>('rates');
  const [editorFor, setEditorFor] = useState<{ lane: Lane | null; preset?: string } | null>(null);
  const [journeyOptions, setJourneyOptions] = useState<{ id: string; name: string }[]>([]);

  const loadList = useCallback(async () => {
    setListErr(null);
    try {
      const res = await apiFetch('/api/mailing/click-funnels');
      const body = await res.json();
      if (!res.ok) throw new Error(body?.error || `HTTP ${res.status}`);
      setList(body);
      setSelected(prev => prev ?? (body.lanes?.find((l: Lane) => l.enabled)?.offer_id ?? body.lanes?.[0]?.offer_id ?? null));
    } catch (e: any) {
      setListErr(e?.message ?? String(e));
    }
  }, []);

  const loadDetail = useCallback(async (offerId: string) => {
    setDetailErr(null);
    setDetail(null);
    try {
      const res = await apiFetch(`/api/mailing/click-funnels/${encodeURIComponent(offerId)}/nodes`);
      const body = await res.json();
      if (!res.ok) throw new Error(body?.error || `HTTP ${res.status}`);
      setDetail(body);
    } catch (e: any) {
      setDetailErr(e?.message ?? String(e));
    }
  }, []);

  useEffect(() => { loadList(); }, [loadList]);
  useEffect(() => { if (selected) loadDetail(selected); }, [selected, loadDetail]);

  // Journey options for the lane editor. Non-fatal: an empty list just means the
  // editor cannot offer a choice, not that the screen is broken.
  useEffect(() => {
    (async () => {
      try {
        const res = await apiFetch('/api/mailing/journeys');
        if (!res.ok) return;
        const body = await res.json();
        const raw = Array.isArray(body) ? body : (body?.journeys ?? []);
        setJourneyOptions(raw.map((j: any) => ({ id: j.id, name: j.name })).filter((j: any) => j.id));
      } catch { /* editor degrades to whatever the lane already points at */ }
    })();
  }, []);

  const lane = useMemo(
    () => list?.lanes.find(l => l.offer_id === selected) ?? null,
    [list, selected]
  );

  const entryReached = useMemo(
    () => (detail?.nodes ?? []).reduce((mx, nd) => Math.max(mx, nd.reached), 0),
    [detail]
  );

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
      {/* Lane editor as an overlay. It used to render inside the lane-list panel
          at the top of the page, so clicking Edit down in Lane config scrolled
          nothing into view and looked broken. A modal is visible from wherever
          the operator clicked. */}
      {editorFor && (
        <div
          onClick={() => setEditorFor(null)}
          style={{
            position: 'fixed', inset: 0, zIndex: 1000,
            background: 'rgba(4,8,18,0.72)',
            display: 'flex', alignItems: 'flex-start', justifyContent: 'center',
            padding: '8vh 16px 16px', overflowY: 'auto',
          }}
        >
          <div onClick={e => e.stopPropagation()} style={{ width: '100%', maxWidth: 520 }}>
            <LaneEditor
              lane={editorFor.lane}
              presetOffer={editorFor.preset}
              journeyOptions={journeyOptions}
              onCancel={() => setEditorFor(null)}
              onSaved={(offerId) => {
                setEditorFor(null);
                setSelected(offerId);
                loadList();
                loadDetail(offerId);
              }}
            />
          </div>
        </div>
      )}

      {/* Orphan inlets — an enabled money-slug with no lane silently drops every
          click it receives (skip_reason=offer_unmapped_at_processing). */}
      {list && list.unmapped_slug_offers?.length > 0 && (
        <Panel accent={colors.warning} style={{ padding: 12 }}>
          <div style={{ fontSize: 13, color: colors.warning, fontWeight: 600 }}>
            <FontAwesomeIcon icon={faTriangleExclamation} /> {list.unmapped_slug_offers.length} offer
            {list.unmapped_slug_offers.length === 1 ? ' has' : 's have'} a live money-link inlet but no funnel
          </div>
          <div style={{ fontSize: 12, color: colors.textMuted, marginTop: 4 }}>
            Offer {list.unmapped_slug_offers.join(', ')} — clicks reach the queue and are dropped as
            <code style={{ margin: '0 4px' }}>offer_unmapped_at_processing</code>. Configure a funnel or disable the slug inlet.
          </div>
          <div style={{ display: 'flex', gap: 8, marginTop: 8, flexWrap: 'wrap' }}>
            {list.unmapped_slug_offers.map(o => (
              <button key={o} onClick={() => setEditorFor({ lane: null, preset: o })} style={btnGhost}>
                Create funnel for offer {o}
              </button>
            ))}
          </div>
        </Panel>
      )}

      {/* Lane list */}
      <Panel style={{ padding: 16 }}>
        <SectionHeader
          title="Click funnels"
          icon={faRoute}
          right={list && (
            <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
              <span style={{ fontSize: 11, color: colors.textMuted }}>
                {list.lanes.filter(l => l.enabled).length} active · {n(list.lanes.reduce((s, l) => s + l.active_enrollments, 0))} enrolled
              </span>
              <button onClick={() => setEditorFor({ lane: null })} style={btnGhost}>+ New funnel</button>
            </div>
          )}
        />

        {listErr ? (
          <SectionError label="Click funnels" error={listErr} onRetry={loadList} />
        ) : !list ? (
          <div style={{ color: colors.textMuted, fontSize: 13 }}><FontAwesomeIcon icon={faSpinner} spin /> Loading…</div>
        ) : list.lanes.length === 0 ? (
          <EmptyState icon={faRoute} title="No click funnels configured" hint="Map an offer to a journey to create a funnel." />
        ) : (
          <div style={{ overflowX: 'auto' }}>
            <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 12 }}>
              <thead>
                <tr style={{ color: colors.textMuted, textAlign: 'left' }}>
                  <th style={{ padding: '6px 8px' }}>Funnel</th>
                  <th style={{ padding: '6px 8px' }}>Payout</th>
                  <th style={{ padding: '6px 8px' }}>Inlets</th>
                  <th style={{ padding: '6px 8px', textAlign: 'right' }}>Active</th>
                  <th style={{ padding: '6px 8px', textAlign: 'right' }}>Enrolled 30d</th>
                  <th style={{ padding: '6px 8px', textAlign: 'right' }}>Touches 30d</th>
                  <th style={{ padding: '6px 8px', textAlign: 'right' }}>Conv 30d</th>
                  <th style={{ padding: '6px 8px' }}>Routing</th>
                </tr>
              </thead>
              <tbody>
                {list.lanes.map(l => (
                  <tr
                    key={l.offer_id}
                    onClick={() => setSelected(l.offer_id)}
                    style={{
                      cursor: 'pointer',
                      background: l.offer_id === selected ? colors.hover : 'transparent',
                      borderTop: `1px solid ${colors.divider}`,
                      opacity: l.enabled ? 1 : 0.5,
                    }}
                  >
                    <td style={{ padding: '7px 8px' }}>
                      <div style={{ fontWeight: 600, color: colors.heading }}>
                        {l.offer_name || `Offer ${l.offer_id}`}
                        {!l.enabled && <Pill color={colors.textFaint} style={{ marginLeft: 6 }}>disabled</Pill>}
                      </div>
                      {/* id kept as a subline: it is the key every other surface
                          (slug map, journey map, Everflow) is scoped by. */}
                      <div style={{ fontSize: 10, color: colors.textFaint }}>offer {l.offer_id}</div>
                    </td>
                    <td style={{ padding: '7px 8px', color: colors.textMuted }}>{l.payout_type || '—'}</td>
                    <td style={{ padding: '7px 8px', color: l.slug_inlets === 0 ? colors.warning : colors.textMuted }}>
                      {l.slug_inlets === 0 ? 'none' : l.slug_inlets}
                    </td>
                    <td style={{ padding: '7px 8px', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{n(l.active_enrollments)}</td>
                    <td style={{ padding: '7px 8px', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{n(l.enrolled_30d)}</td>
                    <td style={{ padding: '7px 8px', textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>{n(l.touches_30d)}</td>
                    <td style={{ padding: '7px 8px', textAlign: 'right', fontVariantNumeric: 'tabular-nums', color: l.conversions_30d > 0 ? colors.success : colors.textMuted }}>
                      {n(l.conversions_30d)}
                    </td>
                    <td style={{ padding: '7px 8px' }}>
                      <Pill color={routingColor(l.routing_state)}>{l.routing_state}</Pill>
                      {l.routing_recommendation && l.routing_recommendation !== l.routing_state && (
                        <span style={{ fontSize: 10, color: colors.textFaint, marginLeft: 6 }}>
                          advises {l.routing_recommendation}
                        </span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>

      {/* Funnel detail */}
      {selected && (
        <div style={{ display: 'grid', gridTemplateColumns: 'minmax(0,2fr) minmax(300px,1fr)', gap: 16, alignItems: 'start' }}>
          <Panel style={{ padding: 16 }}>
            <SectionHeader
              title={detail?.offer_name ? `${detail.offer_name}` : `Offer ${selected}`}
              icon={faBolt}
              right={
                <div style={{ display: 'flex', gap: 4, background: 'rgba(10,16,32,0.6)', borderRadius: 8, padding: 3 }}>
                  {(['rates', 'counts'] as const).map(m => (
                    <button
                      key={m}
                      onClick={() => setMode(m)}
                      style={{
                        background: mode === m ? colors.indigo500 : 'transparent',
                        color: mode === m ? '#fff' : colors.textMuted,
                        border: 'none', borderRadius: 6, padding: '4px 12px',
                        fontSize: 11, fontWeight: 600, cursor: 'pointer', textTransform: 'capitalize',
                      }}
                    >
                      {m}
                    </button>
                  ))}
                </div>
              }
            />

            {detailErr ? (
              <SectionError label="Funnel nodes" error={detailErr} onRetry={() => selected && loadDetail(selected)} />
            ) : !detail ? (
              <div style={{ color: colors.textMuted, fontSize: 13 }}><FontAwesomeIcon icon={faSpinner} spin /> Loading…</div>
            ) : (
              <>
                <div style={{ display: 'flex', gap: 22, flexWrap: 'wrap', marginBottom: 6 }}>
                  <Stat label="Total enrolled" value={n(detail.total_enrolled)} title="All-time enrollments in this lane" />
                  <Stat label="Active now" value={n(detail.total_active)} color={colors.indigo200} />
                  <Stat
                    label="Completed"
                    value={n(detail.total_completed)}
                    sub={pct(detail.completion_rate)}
                    title="Finished all four touches (terminal goal node) — sequence completion, NOT a conversion"
                  />
                  <Stat
                    label="Exited early"
                    value={n(detail.total_exited)}
                    color={colors.warning}
                    title="Left the funnel before completing — engagement watcher, postback exit or suppression"
                  />
                  <Stat
                    label="Conversions"
                    value={n(detail.total_converted)}
                    sub={pct(detail.conversion_rate)}
                    color={colors.success}
                    title="Everflow postback conversions (converted_at) — NOT sequence completions"
                  />
                  <Stat
                    label="Time to goal"
                    value={detail.median_hours_to_convert == null ? '—' : `${detail.median_hours_to_convert.toFixed(1)}h`}
                    title="Median hours from enrollment to conversion"
                  />
                </div>
                <div style={{ fontSize: 11, color: colors.textFaint, marginBottom: 12 }}>
                  Engagement source:{' '}
                  <strong style={{ color: detail.engagement_source === 'lake' ? colors.success : colors.warning }}>
                    {detail.engagement_source === 'lake' ? 'analytics lake (Athena)' : 'Postgres fallback'}
                  </strong>{' '}
                  · window {detail.window_from} → {detail.window_to}
                </div>

                {detail.nodes.map((nd, i) => (
                  <div key={nd.node_id}>
                    {i > 0 && nd.type === 'email' && (
                      <div style={{ paddingLeft: 22, color: colors.textFaint, fontSize: 11 }}>
                        <FontAwesomeIcon icon={faArrowRight} />
                      </div>
                    )}
                    <NodeCard node={nd} mode={mode} entryReached={entryReached} offerId={selected} onSaved={() => loadDetail(selected)} />
                  </div>
                ))}

                <div style={{ marginTop: 12, fontSize: 11, color: colors.textFaint, lineHeight: 1.5 }}>
                  <FontAwesomeIcon icon={faCircleInfo} /> {detail.attribution_note}
                </div>
              </>
            )}
          </Panel>

          <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
            <UploadPanel
              offerId={selected}
              laneLabel={`${lane?.offer_name || `Offer ${selected}`}${lane?.payout_type ? ` · ${lane.payout_type}` : ''}`}
              onEnrolled={() => { loadList(); if (selected) loadDetail(selected); }}
            />

            {lane && (
              <Panel style={{ padding: 16 }}>
                <SectionHeader
                  title="Lane config"
                  icon={faRoute}
                  right={<button onClick={() => setEditorFor({ lane })} style={btnGhost}>Edit</button>}
                />
                <div style={{ fontSize: 12, color: colors.textMuted, display: 'grid', gap: 6 }}>
                  <div>Offer: <span style={{ color: colors.text }}>{lane.offer_name || `Offer ${lane.offer_id}`}</span>
                    <span style={{ color: colors.textFaint }}> (id {lane.offer_id})</span></div>
                  <div>Journey: <span style={{ color: colors.text }}>{lane.journey_name || lane.journey_id || '—'}</span></div>
                  <div>Payout type: <span style={{ color: colors.text }}>{lane.payout_type || '—'}</span></div>
                  <div>Money-slug inlets: <span style={{ color: lane.slug_inlets === 0 ? colors.warning : colors.text }}>{lane.slug_inlets}</span></div>
                  <div>Touches configured: <span style={{ color: colors.text }}>{lane.configured_touches}</span></div>
                  <div>Routing state: <span style={{ color: routingColor(lane.routing_state) }}>{lane.routing_state}</span></div>
                  {lane.redirect_offer_id && <div>Redirects to: <span style={{ color: colors.text }}>{lane.redirect_offer_id}</span></div>}
                  {lane.routing_recommendation && (
                    <div>Governor advises: <span style={{ color: colors.indigo200 }}>{lane.routing_recommendation}</span></div>
                  )}
                </div>
                <div style={{ marginTop: 10, fontSize: 11, color: colors.textFaint, lineHeight: 1.5 }}>
                  Per-touch copy is editable on each touch above. Delays and the number of touches come
                  from the journey graph, which every lane currently shares — changing it here would
                  re-shape all {list?.lanes.length ?? 0} funnels at once, so it stays read-only until lanes
                  get their own journey.
                </div>
              </Panel>
            )}
          </div>
        </div>
      )}
    </div>
  );
};

export default ClickFunnels;
