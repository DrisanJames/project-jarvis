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
  open_rate: number;
  click_rate: number;
  conversion_rate: number;
  step_through_rate: number;
  attributed: boolean;
}

interface NodesResponse {
  offer_id: string;
  journey_id: string;
  nodes: FunnelNode[];
  total_enrolled: number;
  total_active: number;
  total_converted: number;
  attribution_note: string;
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

// ---------------------------------------------------------------- node card

const NodeCard: React.FC<{ node: FunnelNode; mode: 'rates' | 'counts'; entryReached: number }> = ({ node, mode, entryReached }) => {
  const isEmail = node.type === 'email';
  const isGoal = node.type === 'goal';
  const isDelay = node.type === 'delay';

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
          { label: 'Open rate', value: node.attributed ? pct(node.open_rate) : '—', title: 'Opens ÷ delivered for this touch' },
          { label: 'Click rate', value: node.attributed ? pct(node.click_rate) : '—', title: 'Clicks ÷ delivered for this touch' },
          { label: 'Conv rate', value: pct(node.conversion_rate), title: 'Last-touch conversions ÷ people who reached this touch' },
        ]
      : [
          { label: 'Reached', value: n(node.reached), title: 'Distinct enrollments that executed this node' },
          { label: 'Opens', value: node.attributed ? n(node.opens) : '—', title: 'Open events on this touch' },
          { label: 'Clicks', value: node.attributed ? n(node.clicks) : '—', title: 'Click events on this touch' },
          { label: 'Conversions', value: n(node.conversions), title: 'Conversions last-touch attributed to this node' },
        ]
    : [];

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
          </div>

          {/* Creative for this node. Body HTML is per-subscriber (the clicked
              campaign's creative is reused), so subject/preheader is the whole
              of the node-level creative — showing a fake body would mislead. */}
          <div style={{ fontSize: 13, color: colors.text, lineHeight: 1.45 }}>
            {node.subject || <span style={{ color: colors.textFaint, fontStyle: 'italic' }}>inherits the clicked campaign’s subject</span>}
          </div>
          {node.preheader && (
            <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 3 }}>{node.preheader}</div>
          )}
          {node.from_name_override && (
            <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 3 }}>from: {node.from_name_override}</div>
          )}
          <div style={{ fontSize: 11, color: colors.textFaint, marginTop: 6 }}>
            body reuses the creative this subscriber originally clicked
          </div>
        </div>

        <div style={{ display: 'flex', gap: 18, flexWrap: 'wrap' }}>
          {metrics.map(m => (
            <Stat key={m.label} label={m.label} value={m.value} title={m.title} />
          ))}
        </div>
      </div>

      {(node.awaiting > 0 || node.errors > 0) && (
        <div style={{ display: 'flex', gap: 12, marginTop: 10, fontSize: 11, color: colors.textMuted }}>
          {node.awaiting > 0 && <span>{n(node.awaiting)} currently waiting at this node</span>}
          {node.errors > 0 && (
            <span style={{ color: colors.warning }}>
              <FontAwesomeIcon icon={faTriangleExclamation} /> {n(node.errors)} send errors (retried, not skipped)
            </span>
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
        </Panel>
      )}

      {/* Lane list */}
      <Panel style={{ padding: 16 }}>
        <SectionHeader
          title="Click funnels"
          icon={faRoute}
          right={list && (
            <span style={{ fontSize: 11, color: colors.textMuted }}>
              {list.lanes.filter(l => l.enabled).length} active · {n(list.lanes.reduce((s, l) => s + l.active_enrollments, 0))} enrolled
            </span>
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
                  <th style={{ padding: '6px 8px' }}>Offer</th>
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
                    <td style={{ padding: '7px 8px', fontWeight: 600, color: colors.heading }}>
                      {l.offer_id}
                      {!l.enabled && <Pill color={colors.textFaint} style={{ marginLeft: 6 }}>disabled</Pill>}
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
              title={`Funnel · offer ${selected}`}
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
                <div style={{ display: 'flex', gap: 22, flexWrap: 'wrap', marginBottom: 14 }}>
                  <Stat label="Total enrolled" value={n(detail.total_enrolled)} title="All-time enrollments in this lane" />
                  <Stat label="Active now" value={n(detail.total_active)} color={colors.indigo200} />
                  <Stat
                    label="Conversions"
                    value={n(detail.total_converted)}
                    color={colors.success}
                    title="Everflow postback conversions (converted_at) — NOT sequence completions"
                  />
                </div>

                {detail.nodes.map((nd, i) => (
                  <div key={nd.node_id}>
                    {i > 0 && nd.type === 'email' && (
                      <div style={{ paddingLeft: 22, color: colors.textFaint, fontSize: 11 }}>
                        <FontAwesomeIcon icon={faArrowRight} />
                      </div>
                    )}
                    <NodeCard node={nd} mode={mode} entryReached={entryReached} />
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
              laneLabel={`offer ${selected}${lane?.payout_type ? ` (${lane.payout_type})` : ''}`}
              onEnrolled={() => { loadList(); if (selected) loadDetail(selected); }}
            />

            {lane && (
              <Panel style={{ padding: 16 }}>
                <SectionHeader title="Lane config" icon={faRoute} />
                <div style={{ fontSize: 12, color: colors.textMuted, display: 'grid', gap: 6 }}>
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
                  Per-touch subject and preheader are edited through the click-drip admin endpoints
                  (<code>offer-reminder-subjects</code>); the lane binding lives in <code>offer-journey-map</code>.
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
