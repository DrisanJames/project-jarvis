import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faRoute, faUpload, faTriangleExclamation, faCircleCheck, faEnvelope,
  faClock, faFlagCheckered, faBolt, faSpinner, faCircleInfo, faMagnifyingGlass,
  faEllipsis, faGaugeHigh, faWandMagicSparkles,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import { colors } from '../shared/theme';
import { Panel, SectionHeader, Stat, SectionError, EmptyState, Pill } from '../shared/ui';
import { DENVER_PRESETS, denverToday, daysAgoDenver } from '../shared/filters';

/**
 * Click Funnels — the click-drip offer lanes, STEPS FIRST.
 *
 * WHAT CHANGED (2026-08-25) and why it must not be "cleaned up" back:
 *
 *  - THE SEQUENCE IS THE PAGE. The old screen opened on a 22-row lane table and
 *    the steps lived in a panel below it, so understanding one funnel meant
 *    scrolling past the estate. A funnel now renders immediately, deep-linkable
 *    by ?offer=, and lanes are a searchable navigator instead of a wall.
 *
 *  - NOTHING IS QUERIED ON THE REQUEST PATH. Both endpoints serve a snapshot
 *    materialized to S3 by ClickFunnelSnapshotWorker. Changing the window
 *    re-sums day-grain rows the snapshot already carries; it is not a query.
 *
 *  - RATES ALWAYS SHIP WITH THEIR PARTS. "22.88%" concealed a denominator of
 *    2,894 messages ACCEPTED by SES, not delivered to anyone. Every rate here
 *    shows numerator / denominator and names the base.
 *
 *  - THE FOUR CLICK VALUES. is_machine_click is INERT in production (zero
 *    `true` rows estate-wide), so a "human click" figure derived from it just
 *    restates the raw click. Raw / classified / qualified + coverage are shown
 *    instead, and qualified is labelled unclassified until the verdict is real.
 *
 *  - COHORT vs ACTIVITY vs STATE are never mixed in one row. Completion is a
 *    cohort rate over MATURE enrollments with administrative exits excluded;
 *    engagement is activity over the selected window; waiting counts are state.
 *
 * The contract for every number is docs/METRIC_CONTRACT.md §10.
 */

// ---------------------------------------------------------------- types

interface Watermarks {
  metrics_through: string;
  metrics_from: string;
  journey_through: string;
  lake_row_count: number;
  lake_error: string;
  lake_lag_note: string;
  reconciled: boolean;
  reconciled_at: string;
}

interface SnapshotMeta {
  snapshot_id: string;
  generated_at: string;
  age_seconds: number;
  storage: string;
  data_quality: string;
  watermarks: Watermarks;
}

interface CatalogRow {
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

  active_now: number;
  waiting_now: number;

  mature_enrolled: number;
  mature_completed: number;
  completion_rate: number;

  conversions_post_enrollment: number;
  conversions_pre_touch: number;
  conversions_drip_attributed: number;

  touches_enabled: number;
  touches_with_proof: number;
  touches_sendable: number;

  alert_count: number;
  alerts: string[];
}

interface CatalogResponse {
  snapshot: SnapshotMeta;
  lanes: CatalogRow[];
  unmapped_slug_offers: string[];
}

interface NodeMetrics {
  has_data: boolean;
  delivered: number;
  relayed: number;
  accepted: number;
  hard_bounce: number;
  soft_bounce: number;
  deferred: number;
  opens: number;
  clicks_raw: number;
  clicks_classified: number;
  clicks_qualified: number;
  clicks_machine: number;
  unsubs: number;
  complaints: number;
  open_rate: number;
  click_rate: number;
  qualified_click_rate: number;
  unsub_rate: number;
  rate_base_label: string;
  classification_coverage: number;
  classification_usable: boolean;
}

interface FunnelAlert {
  code: string;
  severity: string;
  node_id: string;
  message: string;
  count: number;
}

interface NodeView {
  node_id: string;
  type: string;
  label: string;
  sequence_index: number;
  delay_ms: number;

  reached: number;
  awaiting: number;
  error_enrollments: number;
  error_attempts: number;

  subject: string;
  preheader: string;
  from_name_override: string;
  copy_enabled: boolean;
  copy_missing: boolean;
  copy_updated_at: string;

  proof_id: string;
  proof_name: string;
  proof_approval: string;
  proof_active: boolean;
  proof_sendable: boolean;
  body_inherited: boolean;

  shadow_campaign_id: string;
  attributed: boolean;

  conversions: number;
  conversion_lookback_hours: number;

  step_through_rate: number;
  step_through_of: number;
  step_through_label: string;
  conversion_rate: number;
  conversion_measurable: boolean;
  stuck_retry_ratio: number;
  metrics: NodeMetrics;
}

interface LaneResponse {
  snapshot: SnapshotMeta;
  lane: CatalogRow;
  window_from: string;
  window_to: string;
  ladder_hours: number;
  maturity_hours: number;
  total_enrolled: number;
  in_flight: number;
  exits_behavioral: number;
  exits_administrative: number;
  exits_converted: number;
  goal_node_reached: number;
  median_hours_enroll_to_conversion: number | null;
  median_hours_first_send_to_conversion: number | null;
  nodes: NodeView[];
  alerts: FunnelAlert[];
  notes: string[];
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

const severityColor = (s: string) =>
  s === 'critical' ? colors.danger : s === 'warning' ? colors.warning : colors.textMuted;

const qualityColor = (q: string) =>
  q === 'ok' ? colors.success : q === 'degraded' ? colors.warning : colors.danger;

const ago = (secs: number): string => {
  if (secs < 90) return `${Math.max(0, Math.round(secs))}s ago`;
  if (secs < 5400) return `${Math.round(secs / 60)}m ago`;
  return `${(secs / 3600).toFixed(1)}h ago`;
};

// A dated deadline that has already passed is the failure this screen exists to
// catch: offer 420 shipped "Ends 7/5" for four weeks after it expired.
const expiredDateInCopy = (text: string): string | null => {
  if (!text) return null;
  const now = new Date();
  const md = text.match(/\b(\d{1,2})\/(\d{1,2})\b/);
  if (md) {
    const m = parseInt(md[1], 10), d = parseInt(md[2], 10);
    if (m >= 1 && m <= 12 && d >= 1 && d <= 31) {
      const cand = new Date(now.getFullYear(), m - 1, d);
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

const LaneEditor: React.FC<{
  lane: CatalogRow | null;
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


// ------------------------------------------------------- node card

// MetricCell renders one figure with its parts underneath. The sub-line is not
// decoration: a rate whose numerator and denominator are hidden is how this
// screen reported 22.88% over a base of messages handed to SES.
const MetricCell: React.FC<{
  label: string; value: string; sub?: string; tone?: string; title?: string;
}> = ({ label, value, sub, tone, title }) => (
  <div style={{ minWidth: 118 }} title={title}>
    <div style={{ fontSize: 10, color: colors.textFaint, textTransform: 'uppercase', letterSpacing: 0.4 }}>{label}</div>
    <div style={{ fontSize: 15, fontWeight: 700, color: tone ?? colors.heading, fontVariantNumeric: 'tabular-nums' }}>{value}</div>
    {sub && <div style={{ fontSize: 10, color: colors.textFaint, fontVariantNumeric: 'tabular-nums' }}>{sub}</div>}
  </div>
);

// Band groups metrics that answer the same KIND of question. Performance, flow,
// operations and configuration are different categories and were previously
// mixed into one row of five percentages.
const Band: React.FC<{ title: string; tone?: string; children: React.ReactNode }> = ({ title, tone, children }) => (
  <div style={{ marginTop: 10 }}>
    <div style={{
      fontSize: 10, fontWeight: 700, letterSpacing: 0.6, textTransform: 'uppercase',
      color: tone ?? colors.textFaint, marginBottom: 5,
    }}>{title}</div>
    <div style={{ display: 'flex', gap: 20, flexWrap: 'wrap' }}>{children}</div>
  </div>
);

const NodeCard: React.FC<{
  node: NodeView;
  offerId: string;
  windowLabel: string;
  onSaved: () => void;
}> = ({ node, offerId, windowLabel, onSaved }) => {
  const isEmail = node.type === 'email';
  const isDelay = node.type === 'delay';
  const isGoal = node.type === 'goal';
  const isTrigger = node.type === 'trigger';

  const [drill, setDrill] = useState<NodeEnrollmentRow[] | null>(null);
  const [drillErrOnly, setDrillErrOnly] = useState(false);
  const [drillBusy, setDrillBusy] = useState(false);
  const [editing, setEditing] = useState(false);
  const [picking, setPicking] = useState(false);
  const [studio, setStudio] = useState<OfferProof[] | null>(null);
  const [studioFilter, setStudioFilter] = useState('');
  const [bodyErr, setBodyErr] = useState<string | null>(null);
  const [draft, setDraft] = useState({
    subject: '', preheader: '', from_name_override: '', enabled: true,
  });
  const [saving, setSaving] = useState(false);
  const [saveErr, setSaveErr] = useState<string | null>(null);

  const loadDrill = async (errorsOnly: boolean) => {
    setDrillBusy(true); setDrillErrOnly(errorsOnly);
    try {
      const qs = errorsOnly ? '?action=error&limit=200' : '?limit=200';
      const res = await apiFetch(
        `/api/mailing/click-funnels/${encodeURIComponent(offerId)}/nodes/${encodeURIComponent(node.node_id)}/enrollments${qs}`
      );
      const body = await res.json();
      setDrill(body?.enrollments ?? []);
    } catch {
      setDrill([]);
    } finally {
      setDrillBusy(false);
    }
  };

  const save = async () => {
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

  // Creative Studio -> OFFERS. Only ACTIVE proofs are listed; the sender refuses
  // anything not approved AND active, so offering the rest would invite picking
  // something that silently never mails.
  const loadStudio = async () => {
    try {
      const res = await apiFetch('/api/mailing/offer-proofs?active=true');
      const body = await res.json();
      const rows: OfferProof[] = body?.proofs ?? [];
      const q = studioFilter.trim().toLowerCase();
      setStudio(q ? rows.filter(p => `${p.name} ${p.offer_key}`.toLowerCase().includes(q)) : rows);
    } catch {
      setStudio([]);
    }
  };

  const selectCreative = async (creativeId: string) => {
    setBodyErr(null);
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
    }
  };

  // ── connectors: delay, goal, trigger ──────────────────────────────────────
  // The TRIGGER used to render as a full touch card with copy controls it does
  // not have ("Touch ?" with a copy-disabled pill). It is an entry point.
  if (isTrigger) {
    return (
      <div style={{ display: 'flex', alignItems: 'center', gap: 10, padding: '2px 0 6px 22px', color: colors.textMuted, fontSize: 12 }}>
        <FontAwesomeIcon icon={faBolt} style={{ color: colors.indigo300 }} />
        <span>enrolls on click postback</span>
        {node.awaiting > 0 && <Pill color={colors.indigo300}>{n(node.awaiting)} here now</Pill>}
      </div>
    );
  }

  if (isDelay) {
    return (
      <div style={{ display: 'flex', alignItems: 'center', gap: 10, padding: '4px 0 4px 22px', color: colors.textMuted, fontSize: 12 }}>
        <FontAwesomeIcon icon={faClock} style={{ color: colors.indigo400 }} />
        <span>wait {humanDelay(node.delay_ms) || node.label}</span>
        {node.awaiting > 0 && <Pill color={colors.indigo300}>{n(node.awaiting)} waiting here</Pill>}
      </div>
    );
  }

  if (isGoal) {
    return (
      <div style={{ display: 'flex', alignItems: 'center', gap: 10, padding: '8px 0 0 22px', color: colors.textMuted, fontSize: 12 }}>
        <FontAwesomeIcon icon={faFlagCheckered} style={{ color: colors.indigo300 }} />
        <span>sequence complete — {n(node.reached)} reached the goal node</span>
        <span style={{ fontSize: 10, color: colors.textFaint }}>(flow diagnostic; enrollment status is canonical)</span>
      </div>
    );
  }

  if (!isEmail) return null;

  const m = node.metrics;
  const staleDate = expiredDateInCopy(`${node.subject} ${node.preheader}`);
  const stuck = node.error_enrollments > 0 && node.stuck_retry_ratio >= 20;
  const accent = stuck || (!node.copy_missing && node.proof_id && !node.proof_sendable)
    ? colors.danger
    : (!node.attributed || node.copy_missing || !node.proof_id) ? colors.warning : colors.indigo500;

  return (
    <Panel accent={accent} style={{ padding: 14, marginBottom: 2 }}>
      {/* identity */}
      <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 14, flexWrap: 'wrap' }}>
        <div style={{ minWidth: 260, flex: '1 1 340px' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 4, flexWrap: 'wrap' }}>
            <FontAwesomeIcon icon={faEnvelope} style={{ color: colors.indigo400 }} />
            <span style={{ fontWeight: 700, color: colors.heading, fontSize: 13 }}>
              Touch {node.sequence_index >= 0 ? node.sequence_index + 1 : '?'}
            </span>
            <span style={{ fontSize: 11, color: colors.textFaint }}>{node.node_id}</span>
            {!node.copy_enabled && !node.copy_missing && <Pill color={colors.warning}>copy disabled</Pill>}
            {node.copy_missing && <Pill color={colors.warning}>no copy row</Pill>}
            {!node.attributed && <Pill color={colors.danger}>not measurable</Pill>}
            {staleDate && <Pill color={colors.danger}>expired date “{staleDate}”</Pill>}
          </div>
          {node.subject
            ? <div style={{ color: colors.text, fontSize: 13 }}>{node.subject}</div>
            : <div style={{ color: colors.textFaint, fontSize: 12, fontStyle: 'italic' }}>
                inherits the clicked campaign’s subject
              </div>}
          {node.preheader && <div style={{ color: colors.textMuted, fontSize: 11 }}>{node.preheader}</div>}
        </div>
        <div style={{ display: 'flex', gap: 6, alignItems: 'flex-start' }}>
          <button onClick={() => { setPicking(!picking); if (!studio) loadStudio(); }} style={btnGhost}>
            <FontAwesomeIcon icon={faWandMagicSparkles} /> Creative
          </button>
          <button
            onClick={() => {
              setDraft({
                subject: node.subject, preheader: node.preheader,
                from_name_override: node.from_name_override,
                enabled: node.copy_missing ? true : node.copy_enabled,
              });
              setEditing(!editing);
            }}
            style={btnGhost}
          >Edit copy</button>
        </div>
      </div>

      {/* ── PERFORMANCE — what the mail did, over the selected window ── */}
      <Band title={`Performance · ${windowLabel}`} tone={colors.indigo300}>
        {!node.attributed ? (
          <div style={{ fontSize: 12, color: colors.danger, maxWidth: 560 }}>
            This touch has no shadow campaign, so the lake cannot attribute a single open or click
            to it. Its sends are invisible here until attribution is repaired — the numbers are
            missing, not zero.
          </div>
        ) : !m.has_data ? (
          <div style={{ fontSize: 12, color: colors.textMuted }}>No activity in this window.</div>
        ) : (
          <>
            <MetricCell
              label="Accepted" value={n(m.accepted)}
              sub={`${n(m.delivered)} delivered + ${n(m.relayed)} relayed`}
              title="Accepted = delivered + relayed_to_ses. This lane hands mail to SES, which books relayed_to_ses rather than delivered. Accepted is NOT inbox placement."
            />
            <MetricCell
              label="Open" value={pct(m.open_rate)} sub={`${n(m.opens)} / ${n(m.accepted)} accepted`}
              title="Unique openers over accepted mail."
            />
            <MetricCell
              label="Click (raw)" value={pct(m.click_rate)} sub={`${n(m.clicks_raw)} / ${n(m.accepted)} accepted`}
              title="Unique clickers over accepted mail. Includes machine clicks."
            />
            <MetricCell
              label="Qualified click"
              value={m.classification_usable ? pct(m.qualified_click_rate) : '—'}
              tone={m.classification_usable ? colors.success : colors.textFaint}
              sub={m.classification_usable
                ? `${n(m.clicks_qualified)} / ${n(m.accepted)} · ${pct(m.classification_coverage)} classified`
                : `unclassified — ${pct(m.classification_coverage)} carry a verdict, 0 machine`}
              title="is_machine_click is INERT in production: zero click rows estate-wide are flagged machine. Until a real verdict lands, a 'qualified' figure would simply restate the raw click, so it is withheld rather than dressed up as a human signal."
            />
            <MetricCell
              label="Unsub" value={pct(m.unsub_rate)} sub={`${n(m.unsubs)} / ${n(m.accepted)}`}
              tone={m.unsub_rate > 0.5 ? colors.warning : undefined}
            />
          </>
        )}
      </Band>

      {/* ── FLOW — cohort movement through the ladder ── */}
      <Band title="Flow · lifetime">
        <MetricCell
          label="Step-through" value={pct(node.step_through_rate)}
          sub={node.step_through_of > 0 ? `${n(node.reached)} ${node.step_through_label}` : undefined}
          title="Distinct enrollments that executed this node, over the first node in the graph that logs execution — which is the first WAIT, not total enrolled."
        />
        <MetricCell label="Waiting here now" value={n(node.awaiting)} tone={colors.indigo200} />
        <MetricCell
          label="Conversions"
          value={node.conversion_measurable ? pct(node.conversion_rate) : '—'}
          tone={node.conversions > 0 ? colors.success : colors.textFaint}
          sub={node.conversion_measurable
            ? `${n(node.conversions)} last-touch within ${node.conversion_lookback_hours}h`
            : 'none attributable to this touch'}
          title="Last-touch attribution within the lane's ladder window. A conversion outside every touch's lookback is lane-attributed, never credited to a touch — so this reads '—' rather than 0.00% when nothing is attributable."
        />
      </Band>

      {/* ── OPERATIONS — health, not performance ── */}
      {(node.error_enrollments > 0 || m.deferred > 0 || m.hard_bounce > 0) && (
        <Band title="Operations" tone={stuck ? colors.danger : colors.warning}>
          {node.error_enrollments > 0 && (
            <MetricCell
              label={stuck ? 'STUCK RETRY' : 'Send failures'}
              value={`${n(node.error_enrollments)} mailbox${node.error_enrollments === 1 ? '' : 'es'}`}
              tone={stuck ? colors.danger : colors.warning}
              sub={`${n(node.error_attempts)} attempts · ${n(node.stuck_retry_ratio)}x each`}
              title="Affected ENROLLMENTS is the primary figure; attempts is secondary. The execution log writes one row per attempt, so a handful of looping mailboxes used to render as tens of thousands of broken recipients."
            />
          )}
          {m.deferred > 0 && (
            <MetricCell label="Deferred" value={n(m.deferred)} sub="unique mailboxes"
              title="Distinct mailboxes with a delivery_delay, not delay events — delay notices are per retry and event-counting inflates them ~2.6x." />
          )}
          {(m.hard_bounce > 0 || m.soft_bounce > 0) && (
            <MetricCell label="Bounces" value={`${n(m.hard_bounce)} hard`} sub={`${n(m.soft_bounce)} soft`}
              tone={m.hard_bounce > 0 ? colors.danger : colors.textMuted}
              title="Hard and soft are never combined: hard is a reputation event, soft is usually transient." />
          )}
          <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
            <button onClick={() => loadDrill(true)} style={btnGhost} disabled={drillBusy}>
              {drillBusy && drillErrOnly ? <FontAwesomeIcon icon={faSpinner} spin /> : null} Show failures
            </button>
          </div>
        </Band>
      )}

      {/* ── CONFIGURATION — what will actually mail ── */}
      <Band title="Configuration">
        <div style={{ fontSize: 12, color: colors.textMuted, display: 'flex', gap: 18, flexWrap: 'wrap', alignItems: 'center' }}>
          <span>
            from: <span style={{ color: colors.text }}>{node.from_name_override || 'lane default'}</span>
          </span>
          {node.proof_id ? (
            <span>
              creative:{' '}
              <span style={{ color: node.proof_sendable ? colors.success : colors.danger }}>
                <FontAwesomeIcon icon={node.proof_sendable ? faCircleCheck : faTriangleExclamation} />{' '}
                {node.proof_name || node.proof_id}
              </span>
              {!node.proof_sendable && (
                <span style={{ color: colors.danger, marginLeft: 6 }}>
                  ({node.proof_approval || 'unset'}/{node.proof_active ? 'active' : 'inactive'} — the sender REFUSES this
                  and falls through to inherited creative)
                </span>
              )}
            </span>
          ) : (
            <span style={{ color: colors.warning }}>
              <FontAwesomeIcon icon={faTriangleExclamation} /> no Creative Studio proof — mails whatever creative the
              subscriber clicked
            </span>
          )}
          {node.copy_updated_at && (
            <span style={{ color: colors.textFaint }}>copy updated {node.copy_updated_at.slice(0, 10)}</span>
          )}
        </div>
      </Band>

      {/* creative picker */}
      {picking && (
        <div style={{ marginTop: 10, borderTop: `1px solid ${colors.divider}`, paddingTop: 10 }}>
          <div style={{ display: 'flex', gap: 8, marginBottom: 8 }}>
            <input
              value={studioFilter}
              onChange={e => setStudioFilter(e.target.value)}
              onKeyDown={e => { if (e.key === 'Enter') loadStudio(); }}
              placeholder="filter approved creatives…"
              style={{ ...editInput, width: 260 }}
            />
            <button onClick={loadStudio} style={btnGhost}>Search</button>
          </div>
          {bodyErr && <div style={{ color: colors.danger, fontSize: 12, marginBottom: 6 }}>{bodyErr}</div>}
          <div style={{ maxHeight: 220, overflowY: 'auto', display: 'grid', gap: 4 }}>
            {(studio ?? []).map(p => (
              <button key={p.id} onClick={() => selectCreative(p.id)}
                style={{ ...btnGhost, textAlign: 'left', display: 'flex', justifyContent: 'space-between', gap: 10 }}>
                <span style={{ color: colors.text }}>{p.name}</span>
                <span style={{ color: colors.textFaint, fontSize: 11 }}>{p.offer_key} · {p.approval_status}</span>
              </button>
            ))}
            {studio && studio.length === 0 && (
              <div style={{ fontSize: 12, color: colors.textMuted }}>
                No approved + active proofs matched. Creative Studio → Offers is the only source; the sender refuses
                anything else.
              </div>
            )}
          </div>
        </div>
      )}

      {/* copy editor */}
      {editing && (
        <div style={{ marginTop: 10, borderTop: `1px solid ${colors.divider}`, paddingTop: 10, display: 'grid', gap: 6 }}>
          <input value={draft.subject} onChange={e => setDraft({ ...draft, subject: e.target.value })}
            placeholder="subject" style={editInput} />
          <input value={draft.preheader} onChange={e => setDraft({ ...draft, preheader: e.target.value })}
            placeholder="preheader" style={editInput} />
          <input value={draft.from_name_override} onChange={e => setDraft({ ...draft, from_name_override: e.target.value })}
            placeholder="from-name override (blank = lane default)" style={editInput} />
          <label style={{ fontSize: 12, color: colors.textMuted, display: 'flex', gap: 6, alignItems: 'center' }}>
            <input type="checkbox" checked={draft.enabled} onChange={e => setDraft({ ...draft, enabled: e.target.checked })} />
            copy enabled (unchecked = this touch falls back to the clicked campaign’s copy)
          </label>
          {saveErr && <div style={{ color: colors.danger, fontSize: 12 }}>{saveErr}</div>}
          <div style={{ display: 'flex', gap: 8 }}>
            <button onClick={save} disabled={saving} style={btnPrimary}>
              {saving ? <FontAwesomeIcon icon={faSpinner} spin /> : null} Save
            </button>
            <button onClick={() => setEditing(false)} style={btnGhost}>Cancel</button>
          </div>
        </div>
      )}

      {/* enrollment drill-down */}
      <div style={{ marginTop: 8, display: 'flex', gap: 8, alignItems: 'center' }}>
        <button onClick={() => loadDrill(false)} style={{ ...btnGhost, fontSize: 11 }} disabled={drillBusy}>
          Matching enrollments
        </button>
        {drill && (
          <button onClick={() => setDrill(null)} style={{ ...btnGhost, fontSize: 11 }}>hide</button>
        )}
      </div>
      {drill && (
        <div style={{ marginTop: 6, maxHeight: 260, overflowY: 'auto', fontSize: 11 }}>
          <div style={{ color: colors.textFaint, marginBottom: 4 }}>
            {drill.length} record{drill.length === 1 ? '' : 's'}{drillErrOnly ? ' (failures only)' : ''} — this is a
            direct audit read, not part of the snapshot.
          </div>
          <table style={{ width: '100%', borderCollapse: 'collapse' }}>
            <tbody>
              {drill.map(rowItem => (
                <tr key={rowItem.enrollment_id + rowItem.executed_at} style={{ borderTop: `1px solid ${colors.divider}` }}>
                  <td style={{ padding: '4px 6px', color: colors.text }}>{rowItem.email}</td>
                  <td style={{ padding: '4px 6px', color: colors.textMuted }}>{rowItem.status}</td>
                  <td style={{ padding: '4px 6px', color: colors.textFaint }}>{(rowItem.executed_at || '').slice(0, 16)}</td>
                  <td style={{ padding: '4px 6px', color: rowItem.action === 'error' ? colors.danger : colors.textMuted }}>
                    {rowItem.error_message || rowItem.action}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
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

// LaneNavigator is the searchable lane picker. It replaces the 22-row table
// that used to block the page: it is filterable, it shows the health signals an
// operator picks a lane BY, and it never loads a lane's graph or metrics —
// those arrive only when a lane is selected.
const LaneNavigator: React.FC<{
  lanes: CatalogRow[];
  selected: string | null;
  onPick: (offerId: string) => void;
  onNew: () => void;
}> = ({ lanes, selected, onPick, onNew }) => {
  const [q, setQ] = useState('');
  const [state, setState] = useState<'all' | 'enabled' | 'disabled'>('enabled');
  const [studio, setStudio] = useState<'any' | 'gaps'>('any');
  const [alertsOnly, setAlertsOnly] = useState(false);
  const [open, setOpen] = useState(false);

  const filtered = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return lanes.filter(l => {
      if (state === 'enabled' && !l.enabled) return false;
      if (state === 'disabled' && l.enabled) return false;
      if (studio === 'gaps' && l.touches_enabled > 0 && l.touches_sendable >= l.touches_enabled) return false;
      if (studio === 'gaps' && l.touches_enabled === 0) return false;
      if (alertsOnly && l.alert_count === 0) return false;
      if (!needle) return true;
      return `${l.offer_id} ${l.offer_name} ${l.payout_type} ${l.routing_state}`.toLowerCase().includes(needle);
    });
  }, [lanes, q, state, studio, alertsOnly]);

  const chip = (active: boolean): React.CSSProperties => ({
    ...btnGhost,
    fontSize: 11,
    padding: '4px 10px',
    color: active ? colors.indigo200 : colors.textMuted,
    borderColor: active ? colors.indigo400 : colors.panelBorder,
    background: active ? 'rgba(99,102,241,0.12)' : 'transparent',
  });

  return (
    <Panel style={{ padding: 12 }}>
      <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
        <div style={{ position: 'relative', flex: '1 1 240px', minWidth: 200 }}>
          <FontAwesomeIcon icon={faMagnifyingGlass}
            style={{ position: 'absolute', left: 10, top: 9, color: colors.textFaint, fontSize: 12 }} />
          <input
            value={q}
            onChange={e => { setQ(e.target.value); setOpen(true); }}
            onFocus={() => setOpen(true)}
            placeholder="search funnels by offer id, name, payout…"
            style={{ ...editInput, paddingLeft: 28 }}
          />
        </div>
        <div style={{ display: 'flex', gap: 4 }}>
          {(['enabled', 'all', 'disabled'] as const).map(s => (
            <button key={s} onClick={() => setState(s)} style={chip(state === s)}>{s}</button>
          ))}
        </div>
        <button onClick={() => setStudio(studio === 'gaps' ? 'any' : 'gaps')} style={chip(studio === 'gaps')}>
          Studio gaps
        </button>
        <button onClick={() => setAlertsOnly(!alertsOnly)} style={chip(alertsOnly)}>
          <FontAwesomeIcon icon={faTriangleExclamation} /> alerts
        </button>
        <div style={{ flex: 1 }} />
        <button onClick={() => setOpen(!open)} style={btnGhost}>
          {open ? 'Hide' : 'Browse'} {filtered.length} funnel{filtered.length === 1 ? '' : 's'}
        </button>
        <button onClick={onNew} style={btnGhost}>+ New</button>
      </div>

      {open && (
        <div style={{ marginTop: 10, maxHeight: 300, overflowY: 'auto', display: 'grid', gap: 2 }}>
          {filtered.length === 0 && (
            <div style={{ fontSize: 12, color: colors.textMuted, padding: 8 }}>No funnel matches these filters.</div>
          )}
          {filtered.map(l => {
            const studioGap = l.touches_enabled > 0 && l.touches_sendable < l.touches_enabled;
            return (
              <button
                key={l.offer_id}
                onClick={() => { onPick(l.offer_id); setOpen(false); }}
                style={{
                  ...btnGhost,
                  display: 'grid',
                  gridTemplateColumns: 'minmax(0,2fr) 90px 90px 110px 90px',
                  gap: 10, alignItems: 'center', textAlign: 'left',
                  background: l.offer_id === selected ? 'rgba(99,102,241,0.14)' : 'transparent',
                  opacity: l.enabled ? 1 : 0.55,
                }}
              >
                <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  <span style={{ color: colors.heading, fontWeight: 600 }}>{l.offer_name || `Offer ${l.offer_id}`}</span>
                  <span style={{ color: colors.textFaint, fontSize: 10, marginLeft: 6 }}>offer {l.offer_id}</span>
                </span>
                <span style={{ fontSize: 11, color: colors.textMuted, fontVariantNumeric: 'tabular-nums' }}>
                  {n(l.active_now)} active
                </span>
                <span style={{ fontSize: 11, color: colors.textMuted, fontVariantNumeric: 'tabular-nums' }}>
                  {l.mature_enrolled > 0 ? pct(l.completion_rate) : '—'}
                </span>
                <span style={{ fontSize: 11, color: studioGap ? colors.warning : colors.textMuted }}>
                  studio {l.touches_sendable}/{l.touches_enabled}
                </span>
                <span style={{ fontSize: 11, display: 'flex', gap: 6, alignItems: 'center' }}>
                  <Pill color={routingColor(l.routing_state)}>{l.routing_state}</Pill>
                  {l.alert_count > 0 && (
                    <span style={{ color: colors.warning }}>
                      <FontAwesomeIcon icon={faTriangleExclamation} /> {l.alert_count}
                    </span>
                  )}
                </span>
              </button>
            );
          })}
        </div>
      )}
    </Panel>
  );
};

// HealthStrip sits directly under the lane header, BEFORE the touches. An
// operator needs to know whether the lane is healthy before reading individual
// steps — the previous layout put this after them.
const HealthStrip: React.FC<{ d: LaneResponse }> = ({ d }) => {
  const l = d.lane;
  const cell = (label: string, value: string, sub?: string, tone?: string, title?: string) => (
    <div style={{ minWidth: 130 }} title={title}>
      <div style={{ fontSize: 10, color: colors.textFaint, textTransform: 'uppercase', letterSpacing: 0.4 }}>{label}</div>
      <div style={{ fontSize: 17, fontWeight: 700, color: tone ?? colors.heading, fontVariantNumeric: 'tabular-nums' }}>{value}</div>
      {sub && <div style={{ fontSize: 10, color: colors.textFaint }}>{sub}</div>}
    </div>
  );
  return (
    <div style={{
      display: 'flex', gap: 24, flexWrap: 'wrap', padding: '10px 12px', marginBottom: 12,
      borderRadius: 8, background: 'rgba(10,16,32,0.5)', border: `1px solid ${colors.panelBorder}`,
    }}>
      {cell('Active now', n(l.active_now), `${n(l.waiting_now)} waiting at a step`, colors.indigo200,
        'STATE: point-in-time, no window.')}
      {cell('Completion', l.mature_enrolled > 0 ? pct(l.completion_rate) : '—',
        `${n(l.mature_completed)} / ${n(l.mature_enrolled)} mature`, colors.success,
        `COHORT: enrollments older than ${d.maturity_hours}h (the ${d.ladder_hours}h ladder + 24h grace), with administrative exits excluded. Younger enrollments have not had time to finish and are shown separately as in-flight.`)}
      {cell('In flight', n(d.in_flight), 'too young to have finished', colors.textMuted,
        'Reported separately and deliberately excluded from every rate.')}
      {cell('Conversions', n(l.conversions_drip_attributed),
        `${n(l.conversions_post_enrollment)} total · ${n(l.conversions_pre_touch)} before any send`,
        l.conversions_drip_attributed > 0 ? colors.success : colors.textMuted,
        'Three figures, never one. Most click-drip conversions are caused by the ORIGINAL click, not a drip touch — only the drip-attributed figure belongs to this funnel.')}
      {cell('Time to goal',
        d.median_hours_first_send_to_conversion == null ? '—' : `${d.median_hours_first_send_to_conversion.toFixed(1)}h`,
        d.median_hours_enroll_to_conversion == null ? undefined : `${d.median_hours_enroll_to_conversion.toFixed(1)}h from enrollment`,
        undefined,
        'Median from the FIRST DRIP SEND to conversion. The enrollment-to-conversion figure is shown beneath because it largely measures the original click, not the drip.')}
      {cell('Exits', n(d.exits_behavioral), `${n(d.exits_administrative)} operator bulk actions`, colors.warning,
        'Behavioral exits are lane outcomes. Administrative exits are operator bulk purges and are excluded from every rate here.')}
    </div>
  );
};

export const ClickFunnels: React.FC = () => {
  const [catalog, setCatalog] = useState<CatalogResponse | null>(null);
  const [catalogErr, setCatalogErr] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(() => {
    // Deep link: ?offer=420 opens ON the funnel, never on a list.
    try { return new URLSearchParams(window.location.search).get('offer'); } catch { return null; }
  });
  const [detail, setDetail] = useState<LaneResponse | null>(null);
  const [detailErr, setDetailErr] = useState<string | null>(null);
  const [detailBusy, setDetailBusy] = useState(false);
  const [editorFor, setEditorFor] = useState<{ lane: CatalogRow | null; preset?: string } | null>(null);
  const [journeyOptions, setJourneyOptions] = useState<{ id: string; name: string }[]>([]);
  const [showAdmin, setShowAdmin] = useState(false);
  const [win, setWin] = useState({ from: daysAgoDenver(29), to: denverToday() });

  const loadCatalog = useCallback(async () => {
    setCatalogErr(null);
    try {
      const res = await apiFetch('/api/mailing/click-funnels/catalog');
      const body = await res.json();
      if (!res.ok) throw new Error(body?.error || `HTTP ${res.status}`);
      setCatalog(body);
      return body as CatalogResponse;
    } catch (e: any) {
      setCatalogErr(e?.message ?? String(e));
      return null;
    }
  }, []);

  const loadDetail = useCallback(async (offerId: string, from: string, to: string) => {
    setDetailErr(null); setDetailBusy(true);
    try {
      const res = await apiFetch(
        `/api/mailing/click-funnels/${encodeURIComponent(offerId)}?from=${from}&to=${to}`);
      const body = await res.json();
      if (!res.ok) throw new Error(body?.error || `HTTP ${res.status}`);
      setDetail(body);
    } catch (e: any) {
      setDetailErr(e?.message ?? String(e));
      setDetail(null);
    } finally {
      setDetailBusy(false);
    }
  }, []);

  // First paint: fetch the catalog and open a funnel immediately. Without a
  // deep link that is the busiest lane — the catalog is already sorted by
  // enabled, then live activity.
  useEffect(() => {
    (async () => {
      const body = await loadCatalog();
      if (!body?.lanes?.length) return;
      setSelected(prev => {
        if (prev && body.lanes.some(l => l.offer_id === prev)) return prev;
        return body.lanes[0].offer_id;
      });
    })();
  }, [loadCatalog]);

  useEffect(() => {
    if (!selected) return;
    loadDetail(selected, win.from, win.to);
    try {
      const u = new URL(window.location.href);
      u.searchParams.set('offer', selected);
      window.history.replaceState({}, '', u.toString());
    } catch { /* deep link is a convenience, never a hard dependency */ }
  }, [selected, win.from, win.to, loadDetail]);

  useEffect(() => {
    (async () => {
      try {
        const res = await apiFetch('/api/mailing/journeys');
        const body = await res.json();
        setJourneyOptions((body?.journeys ?? []).map((j: any) => ({ id: j.id, name: j.name })));
      } catch { setJourneyOptions([]); }
    })();
  }, []);

  const windowLabel = `${win.from} → ${win.to}`;
  const snap = detail?.snapshot ?? catalog?.snapshot;

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      {editorFor && (
        <div
          onClick={() => setEditorFor(null)}
          style={{
            position: 'fixed', inset: 0, zIndex: 1000, background: 'rgba(4,8,18,0.72)',
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
                loadCatalog();
              }}
            />
          </div>
        </div>
      )}

      {/* Orphan inlets: an enabled money-slug with no lane silently drops every
          click it receives (skip_reason=offer_unmapped_at_processing). */}
      {catalog && catalog.unmapped_slug_offers?.length > 0 && (
        <Panel accent={colors.warning} style={{ padding: 10 }}>
          <div style={{ fontSize: 12, color: colors.warning, fontWeight: 600 }}>
            <FontAwesomeIcon icon={faTriangleExclamation} /> {catalog.unmapped_slug_offers.length} offer
            {catalog.unmapped_slug_offers.length === 1 ? ' has' : 's have'} a live money-link inlet but no funnel —
            every click they receive is dropped.
          </div>
          <div style={{ display: 'flex', gap: 6, marginTop: 6, flexWrap: 'wrap' }}>
            {catalog.unmapped_slug_offers.map(o => (
              <button key={o} onClick={() => setEditorFor({ lane: null, preset: o })} style={{ ...btnGhost, fontSize: 11 }}>
                Create funnel for offer {o}
              </button>
            ))}
          </div>
        </Panel>
      )}

      {catalogErr ? (
        <SectionError label="Click funnels" error={catalogErr} onRetry={loadCatalog} />
      ) : !catalog ? (
        <Panel style={{ padding: 16 }}>
          <div style={{ color: colors.textMuted, fontSize: 13 }}>
            <FontAwesomeIcon icon={faSpinner} spin /> Loading snapshot…
          </div>
        </Panel>
      ) : (
        <LaneNavigator
          lanes={catalog.lanes}
          selected={selected}
          onPick={setSelected}
          onNew={() => setEditorFor({ lane: null })}
        />
      )}

      {/* ── THE FUNNEL ── the page's subject, not a panel below a table. */}
      {selected && (
        <Panel style={{ padding: 16 }}>
          <SectionHeader
            title={detail?.lane.offer_name ? `${detail.lane.offer_name} · EF ${selected}` : `Offer ${selected}`}
            icon={faRoute}
            right={
              <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                <div style={{ display: 'flex', gap: 3 }}>
                  {DENVER_PRESETS.map(p => {
                    const active = win.from === p.from() && win.to === p.to();
                    return (
                      <button
                        key={p.label}
                        onClick={() => setWin({ from: p.from(), to: p.to() })}
                        style={{
                          ...btnGhost, fontSize: 11, padding: '4px 9px',
                          color: active ? colors.indigo200 : colors.textMuted,
                          borderColor: active ? colors.indigo400 : colors.panelBorder,
                          background: active ? 'rgba(99,102,241,0.12)' : 'transparent',
                        }}
                      >{p.label}</button>
                    );
                  })}
                </div>
                <button onClick={() => setShowAdmin(!showAdmin)} style={btnGhost} title="Lane config and clicker upload">
                  <FontAwesomeIcon icon={faEllipsis} />
                </button>
              </div>
            }
          />

          {/* provenance — never a "live" badge; engagement has a ~6h floor. */}
          {snap && (
            <div style={{ fontSize: 11, color: colors.textFaint, marginBottom: 10, display: 'flex', gap: 12, flexWrap: 'wrap' }}>
              <span>
                <FontAwesomeIcon icon={faGaugeHigh} /> snapshot{' '}
                <span style={{ color: qualityColor(snap.data_quality) }}>{snap.data_quality}</span>{' '}
                · built {ago(snap.age_seconds)}
              </span>
              <span>engagement complete through <strong style={{ color: colors.textMuted }}>
                {snap.watermarks.metrics_through || 'unknown'}</strong></span>
              {snap.watermarks.lake_error && (
                <span style={{ color: colors.warning }}>
                  <FontAwesomeIcon icon={faTriangleExclamation} /> last lake pass failed — engagement is stale, not zero
                </span>
              )}
            </div>
          )}

          {detailErr ? (
            <SectionError label="Funnel" error={detailErr} onRetry={() => selected && loadDetail(selected, win.from, win.to)} />
          ) : !detail ? (
            <div style={{ color: colors.textMuted, fontSize: 13 }}>
              <FontAwesomeIcon icon={faSpinner} spin /> Loading funnel…
            </div>
          ) : (
            <>
              <HealthStrip d={detail} />

              {detail.alerts.length > 0 && (
                <div style={{ display: 'grid', gap: 4, marginBottom: 12 }}>
                  {detail.alerts.map((a, i) => (
                    <div key={`${a.code}-${a.node_id}-${i}`} style={{
                      fontSize: 12, color: severityColor(a.severity),
                      display: 'flex', gap: 8, alignItems: 'baseline',
                    }}>
                      <FontAwesomeIcon icon={faTriangleExclamation} />
                      {a.node_id && <span style={{ color: colors.textFaint, fontSize: 11 }}>{a.node_id}</span>}
                      <span>{a.message}</span>
                    </div>
                  ))}
                </div>
              )}

              {detailBusy && (
                <div style={{ fontSize: 11, color: colors.textFaint, marginBottom: 6 }}>
                  <FontAwesomeIcon icon={faSpinner} spin /> re-aggregating window…
                </div>
              )}

              {detail.nodes.map(nd => (
                <NodeCard
                  key={nd.node_id}
                  node={nd}
                  offerId={selected}
                  windowLabel={windowLabel}
                  onSaved={() => loadDetail(selected, win.from, win.to)}
                />
              ))}

              <div style={{ marginTop: 14, fontSize: 11, color: colors.textFaint, lineHeight: 1.6 }}>
                <FontAwesomeIcon icon={faCircleInfo} /> How these numbers are defined:
                <ul style={{ margin: '6px 0 0 18px', padding: 0 }}>
                  {detail.notes.map((note, i) => <li key={i}>{note}</li>)}
                </ul>
              </div>
            </>
          )}

          {/* Administrative controls live behind the ellipsis: they mutate the
              lane, they are not reading surface. */}
          {showAdmin && detail && (
            <div style={{ marginTop: 16, display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(280px,1fr))', gap: 12 }}>
              <UploadPanel
                offerId={selected}
                laneLabel={`${detail.lane.offer_name || `Offer ${selected}`}${detail.lane.payout_type ? ` · ${detail.lane.payout_type}` : ''}`}
                onEnrolled={() => { loadCatalog(); loadDetail(selected, win.from, win.to); }}
              />
              <Panel style={{ padding: 14 }}>
                <SectionHeader
                  title="Lane config"
                  icon={faRoute}
                  right={<button onClick={() => setEditorFor({ lane: detail.lane })} style={btnGhost}>Edit</button>}
                />
                <div style={{ fontSize: 12, color: colors.textMuted, display: 'grid', gap: 5 }}>
                  <div>Journey: <span style={{ color: colors.text }}>{detail.lane.journey_name || detail.lane.journey_id || '—'}</span></div>
                  <div>Payout: <span style={{ color: colors.text }}>{detail.lane.payout_type || '—'}</span></div>
                  <div>Money-slug inlets: <span style={{ color: detail.lane.slug_inlets === 0 ? colors.warning : colors.text }}>
                    {detail.lane.slug_inlets}</span></div>
                  <div>Ladder: <span style={{ color: colors.text }}>{detail.ladder_hours}h</span>
                    <span style={{ color: colors.textFaint }}> · mature at {detail.maturity_hours}h</span></div>
                  <div>Creative Studio: <span style={{ color: detail.lane.touches_sendable < detail.lane.touches_enabled ? colors.warning : colors.success }}>
                    {detail.lane.touches_sendable}/{detail.lane.touches_enabled} touches sendable</span></div>
                  <div>Routing: <span style={{ color: routingColor(detail.lane.routing_state) }}>{detail.lane.routing_state}</span></div>
                </div>
                <div style={{ marginTop: 8, fontSize: 11, color: colors.textFaint, lineHeight: 1.5 }}>
                  Delays and touch count come from the journey graph, which every lane currently shares — changing it
                  here would re-shape all {catalog?.lanes.length ?? 0} funnels at once, so it stays read-only until
                  lanes get their own journey.
                </div>
              </Panel>
            </div>
          )}
        </Panel>
      )}

      {catalog && catalog.lanes.length === 0 && (
        <EmptyState icon={faRoute} title="No click funnels configured"
          hint="Map an offer to a journey to create a funnel." />
      )}
    </div>
  );
};

export default ClickFunnels;
