import React, { useCallback, useEffect, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faLayerGroup, faHeartPulse, faTriangleExclamation, faRotate, faSpinner,
  faChevronDown, faChevronRight, faBroom,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import {
  colors, alpha, pageStyle, panelStyle, tableStyle,
  thStyle, tdStyle, numTd, numTh,
} from '../shared/theme';
import { SectionHeader, Stat, Pill, SectionError, EmptyState } from '../shared/ui';

// =============================================================================
// SEGMENTATION COMMAND — the life-blood screen
// =============================================================================
// The operator's segmentation truth surface. Its one job: make MEMBERSHIP
// staleness impossible to hide. The frozen-segment defect class (band counts
// frozen since jul19, a gmail recalc cron that never ran, a rollup that ran
// once) survived for weeks because every surface showed counts-refresh
// ("segment_refresh OK") while membership rebuilt on a 28-day rotation.
//
// Source: GET /api/mailing/segmentation/health (internal/api/
// segmentation_health.go) — every number on this screen comes from that
// endpoint; nothing is derived client-side beyond formatting and sums the
// endpoint already exposes.
//
// FilterBar note (PORTAL_DESIGN_SYSTEM §3): this screen shows current
// platform state — none of the canonical filter vocabulary (Denver date
// range, brand, ISP, transport) binds to these sources, so the shared
// FilterBar is deliberately not mounted (same ruling as OperationsConsole).
//
// State honesty (§1.6): loading, fetch-error-with-retry, endpoint-not-on-
// this-build (404 = deploy held), and genuinely-empty are all distinct.

// PAGE_VERSION 1.2 — EO Clean actions on family + drilldown rows: POSTs a
// segment-sourced job to /api/mailing/eo-clean/jobs (EO CLEANING SERVICE,
// operator ask 2026-07-26 — "clean a particular list or segment from the
// segment command"). Confirm dialog quotes the record count + per-record cost.
// PAGE_VERSION 1.1 — performance surface: member-scoped 7d/30d windows
// (delivered/opens/action-clicks/complaints/unsubs/hard/soft, engagement
// rate) + 14d churn timeline sparkline, from the nightly
// mailing_segment_perf_daily rollup (operator enrichment ask, 2026-07-26).
const PAGE_VERSION = '1.2';

// EO_CLEAN_COST_NOTE is quoted in every Clean confirm — cleaning is billed
// per record; already-Verified emails are skipped at enqueue (never re-billed).
const EO_CLEAN_COST_NOTE =
  'EmailOversight validation has a per-record cost. Emails already Verified in the validation gate are skipped and never re-billed.';

// ── API shapes (mirror segmentation_health.go; do not drift) ────────────────

type FamilyVerdict = 'LIVE' | 'STALE' | 'STATIC-DECLARED' | 'UNREGISTERED';

interface PerfWindow {
  delivered: number;
  opens: number;
  clicks_action: number;
  complaints: number;
  unsubs: number;
  hard_bounces: number;
  soft_bounces: number;
  engagement_rate: number;
}

interface ChurnDay {
  day: string;
  members: number;
  added: number;
  removed: number;
}

interface SegRow {
  id: string;
  name: string;
  segment_type: string;
  subscriber_count: number;
  last_count_at: string | null;
  last_built_at: string | null;
  last_build_status: string;
  ledger_count: number;
  build_source: string;
  member_inserts_24h: number | null; // null = churn unavailable
  campaign_refs_3d: number;
  count_membership_diverged: boolean;
  perf: PerfWindow | null; // null = rollup unavailable
}

interface FamilyRow {
  family_key: string;
  family_pattern: string;
  registered: boolean;
  owner: string;
  cadence: string;
  sla_hours: number;
  keep_policy: string;
  verdict: FamilyVerdict;
  segments_count: number;
  dynamic_count: number;
  static_count: number;
  subscriber_total: number;
  last_membership_build_min: string | null;
  last_membership_build_max: string | null;
  builds_last_24h: number;
  failed_now: number;
  member_inserts_24h: number;
  member_inserts_7d: number;
  campaign_refs_3d: number;
  diverged_count: number;
  perf: PerfWindow | null;
  churn_timeline: ChurnDay[];
  segments: SegRow[];
  segments_truncated: boolean;
}

interface WorkerRow {
  name: string;
  kind: string;
  cadence_seconds: number;
  last_beat_at: string | null;
  seconds_since_beat: number | null;
  last_status: string;
  last_error: string;
  cycle_count: number;
  stalled: boolean;
  last_run_status: string;
  last_run_at: string | null;
  light: 'ok' | 'stale' | 'error' | 'stalled' | 'unknown';
}

interface AlertRow {
  severity: 'red' | 'amber';
  verdict: string;
  family_key: string;
  message: string;
  blast_radius: number;
  hours_overdue: number;
}

interface HealthSummary {
  families_live: number;
  families_stale: number;
  families_static: number;
  families_unregistered: number;
  segments_total: number;
  subscriber_total: number;
}

interface HealthResponse {
  api_version: string;
  generated_at: string;
  summary: HealthSummary;
  families: FamilyRow[];
  workers: WorkerRow[];
  alerts: AlertRow[];
  churn_method: string; // 'materialized_window' | 'unavailable'
  refs_method: string;  // 'inclusion_segments_3d' | 'unavailable'
  segments_truncated: boolean;
  registry_available: boolean;
  perf_method: string; // 'member_scoped_rollup' | 'unavailable'
  perf_window_days: number;
  churn_timeline_method: string; // 'rollup_daily' | 'unavailable'
}

// ── Fetch plumbing (single endpoint; mirrors OperationsConsole's panel hook —
// hoisting the shared hook into shared/ is a noted follow-up, not done here to
// keep this change inside its file boundary) ────────────────────────────────

interface FetchState {
  loading: boolean;
  error: string | null;
  unavailable: boolean; // 404 — endpoint not on this server build
  data: HealthResponse | null;
  fetchedAt: string | null;
  ms: number | null;
}

const useHealthFetch = (nonce: number, window: '7d' | '30d'): [FetchState, () => void] => {
  const [state, setState] = useState<FetchState>({
    loading: true, error: null, unavailable: false, data: null, fetchedAt: null, ms: null,
  });
  const [localNonce, setLocalNonce] = useState(0);
  const retry = useCallback(() => setLocalNonce(n => n + 1), []);
  const abortRef = useRef<AbortController | null>(null);

  useEffect(() => {
    abortRef.current?.abort();
    const ac = new AbortController();
    abortRef.current = ac;
    setState(s => ({ ...s, loading: true, error: null }));
    const t0 = performance.now();
    apiFetch(`/api/mailing/segmentation/health?window=${window}`, { signal: ac.signal })
      .then(async res => {
        const ms = Math.round(performance.now() - t0);
        const fetchedAt = new Date().toLocaleTimeString();
        if (res.status === 404) {
          setState({ loading: false, error: null, unavailable: true, data: null, fetchedAt, ms });
          return;
        }
        if (!res.ok) {
          let msg = `HTTP ${res.status}`;
          try {
            const body = await res.json();
            if (body?.error) msg += ` — ${body.error}`;
          } catch { /* non-JSON error body */ }
          setState({ loading: false, error: msg, unavailable: false, data: null, fetchedAt, ms });
          return;
        }
        const data = (await res.json()) as HealthResponse;
        setState({ loading: false, error: null, unavailable: false, data, fetchedAt, ms });
      })
      .catch((e: unknown) => {
        if (ac.signal.aborted) return;
        setState({
          loading: false, error: e instanceof Error ? e.message : String(e),
          unavailable: false, data: null, fetchedAt: new Date().toLocaleTimeString(), ms: null,
        });
      });
    return () => ac.abort();
  }, [nonce, localNonce, window]);

  return [state, retry];
};

// ── Display helpers ─────────────────────────────────────────────────────────

const fmtInt = (n: number): string => n.toLocaleString();

const fmtAgeSeconds = (s: number): string => {
  const v = Math.max(0, s);
  if (v < 90) return `${Math.round(v)}s`;
  if (v < 5400) return `${Math.round(v / 60)}m`;
  if (v < 172800) return `${(v / 3600).toFixed(1)}h`;
  return `${(v / 86400).toFixed(1)}d`;
};

const ageSeconds = (iso: string | null): number | null => {
  if (!iso) return null;
  const t = Date.parse(iso);
  return isNaN(t) ? null : (Date.now() - t) / 1000;
};

const fmtRelative = (iso: string | null): string => {
  const s = ageSeconds(iso);
  return s == null ? '—' : `${fmtAgeSeconds(s)} ago`;
};

const verdictColor = (v: FamilyVerdict): string => {
  switch (v) {
    case 'LIVE': return colors.success;
    case 'STALE': return colors.danger;
    case 'UNREGISTERED': return colors.warning;
    case 'STATIC-DECLARED': return colors.idle;
  }
};

const workerLightColor = (light: WorkerRow['light']): string => {
  switch (light) {
    case 'ok': return colors.success;
    case 'stale': return colors.warning;
    case 'unknown': return colors.warning;
    case 'error':
    case 'stalled': return colors.danger;
  }
};

// Is the family's newest membership build past its SLA? (Server verdict is
// authoritative — this only picks the red text styling for the age cell.)
const buildAgeStyle = (f: FamilyRow): React.CSSProperties => {
  if (f.verdict === 'STALE') return { color: colors.danger, fontWeight: 700 };
  return {};
};

const scrollWrap: React.CSSProperties = { overflowX: 'auto' };
const monoCell: React.CSSProperties = { ...tdStyle, fontFamily: 'monospace', fontSize: 11.5 };

const fmtPct = (rate: number): string => `${(rate * 100).toFixed(2)}%`;

// Negative-signal cell: dims at 0, colors when present. Hard bounces red,
// soft amber, complaints red, unsubs amber — NEVER combined into one number.
const negCell = (n: number | undefined, color: string): React.ReactNode => (
  <span style={{ color: n && n > 0 ? color : colors.textFaint, fontWeight: n && n > 0 ? 700 : 400 }}>
    {n == null ? '—*' : fmtInt(n)}
  </span>
);

// ChurnSparkline: 14 Denver days of adds (up, indigo) / derived removals
// (down, red) around a baseline. All values from the endpoint's timeline.
const ChurnSparkline: React.FC<{ timeline: ChurnDay[] }> = ({ timeline }) => {
  if (timeline.length === 0) return <span style={{ color: colors.textFaint }}>—</span>;
  const W = 98, H = 26, mid = H / 2;
  const n = timeline.length;
  const bw = Math.max(2, Math.floor(W / Math.max(n, 1)) - 2);
  const peak = Math.max(1, ...timeline.map(d => Math.max(d.added, d.removed)));
  const latest = timeline[timeline.length - 1];
  const title = `last ${n}d — latest ${latest.day}: ${fmtInt(latest.members)} members, +${fmtInt(latest.added)} added, -${fmtInt(latest.removed)} removed (derived)`;
  return (
    <svg width={W} height={H} role="img" aria-label={title} style={{ display: 'block' }}>
      <title>{title}</title>
      <line x1={0} y1={mid} x2={W} y2={mid} stroke={colors.hairline} strokeWidth={1} />
      {timeline.map((d, i) => {
        const x = i * (bw + 2);
        const up = Math.round((d.added / peak) * (mid - 2));
        const down = Math.round((d.removed / peak) * (mid - 2));
        return (
          <g key={d.day}>
            {up > 0 && <rect x={x} y={mid - up} width={bw} height={up} fill={colors.indigo400} />}
            {down > 0 && <rect x={x} y={mid + 1} width={bw} height={down} fill={colors.danger} opacity={0.85} />}
          </g>
        );
      })}
    </svg>
  );
};

// ── Sections ────────────────────────────────────────────────────────────────

const WorkerLights: React.FC<{ workers: WorkerRow[] }> = ({ workers }) => (
  <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap', alignItems: 'center' }}>
    {workers.map(w => (
      <span
        key={w.name}
        title={[
          `light: ${w.light}`,
          w.last_beat_at ? `last beat ${fmtRelative(w.last_beat_at)}` : 'NEVER BEAT (no heartbeat row)',
          w.cadence_seconds > 0 ? `cadence ${fmtAgeSeconds(w.cadence_seconds)}` : '',
          `last run: ${w.last_run_status}${w.last_run_at ? ` (${fmtRelative(w.last_run_at)})` : ''}`,
          w.last_error ? `last error: ${w.last_error}` : '',
        ].filter(Boolean).join(' · ')}
        style={{
          display: 'inline-flex', alignItems: 'center', gap: 6, fontSize: 11,
          fontFamily: 'monospace', color: colors.text,
          background: alpha(workerLightColor(w.light), '14'),
          border: `1px solid ${alpha(workerLightColor(w.light), '44')}`,
          borderRadius: 999, padding: '3px 10px',
        }}
      >
        <span style={{ width: 8, height: 8, borderRadius: 999, background: workerLightColor(w.light) }} />
        {w.name}
        <span style={{ color: colors.textMuted, textTransform: 'uppercase', fontSize: 9.5 }}>{w.light}</span>
      </span>
    ))}
  </div>
);

const AlertsPanel: React.FC<{ alerts: AlertRow[] }> = ({ alerts }) => (
  <div style={panelStyle}>
    <SectionHeader title="Alerts — ranked by blast radius" icon={faTriangleExclamation} />
    {alerts.length === 0 ? (
      <div style={{ fontSize: 12.5, color: colors.successText }}>
        No STALE or UNREGISTERED families. Every family is either inside its SLA or declared static.
      </div>
    ) : (
      alerts.map((a, i) => {
        const c = a.severity === 'red' ? colors.danger : colors.warning;
        return (
          <div
            key={`${a.family_key}-${i}`}
            style={{
              display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap',
              padding: '8px 12px', marginBottom: 6, borderRadius: 8,
              background: alpha(c, '14'), border: `1px solid ${alpha(c, '44')}`,
            }}
          >
            <Pill color={c}>{a.verdict}</Pill>
            <span style={{ fontWeight: 700, fontSize: 13 }}>{a.family_key}</span>
            <span style={{ fontSize: 12, color: colors.text }}>{a.message}</span>
            <span style={{ fontSize: 11.5, color: colors.textMuted, marginLeft: 'auto', fontVariantNumeric: 'tabular-nums' }}>
              blast radius {fmtInt(a.blast_radius)} members
              {a.hours_overdue > 0 && <span style={{ color: c }}> · {a.hours_overdue.toFixed(1)}h past SLA</span>}
            </span>
          </div>
        );
      })
    )}
  </div>
);

// CleanButton — the EO-clean trigger shared by family + drilldown rows.
const CleanButton: React.FC<{ onClick: () => void; busy: boolean; title: string }> = ({ onClick, busy, title }) => (
  <button
    onClick={e => { e.stopPropagation(); onClick(); }}
    disabled={busy}
    title={title}
    style={{
      background: alpha(colors.indigo500, '22'), border: `1px solid ${alpha(colors.indigo500, '44')}`,
      color: colors.indigo200, borderRadius: 6, padding: '3px 9px',
      cursor: busy ? 'wait' : 'pointer', fontSize: 11, whiteSpace: 'nowrap',
    }}>
    <FontAwesomeIcon icon={faBroom} /> Clean
  </button>
);

const SegmentDrilldown: React.FC<{
  family: FamilyRow;
  churnAvailable: boolean;
  perfAvailable: boolean;
  win: '7d' | '30d';
  onClean: (segments: { id: string; name: string }[], records: number, what: string) => void;
  cleaning: boolean;
}> = ({ family, churnAvailable, perfAvailable, win, onClean, cleaning }) => (
  <div style={{ ...scrollWrap, padding: '4px 8px 10px 28px' }}>
    <table style={tableStyle}>
      <thead>
        <tr>
          <th style={thStyle}>Segment</th>
          <th style={thStyle}>Type</th>
          <th style={numTh}>Count (segments)</th>
          <th style={numTh}>Count (ledger)</th>
          <th style={numTh}>Membership built</th>
          <th style={numTh}>Counts refreshed</th>
          <th style={thStyle}>Build status</th>
          <th style={thStyle}>Source</th>
          <th style={numTh}>Delivered</th>
          <th style={numTh}>Opens</th>
          <th style={numTh}>Clicks*</th>
          <th style={numTh}>Eng rate</th>
          <th style={numTh}>Cmpl</th>
          <th style={numTh}>Unsub</th>
          <th style={numTh}>Hard</th>
          <th style={numTh}>Soft</th>
          <th style={numTh}>Inserts 24h</th>
          <th style={numTh}>Refs 3d</th>
          <th style={thStyle}>Divergence</th>
          <th style={thStyle}>Clean</th>
        </tr>
      </thead>
      <tbody>
        {family.segments.map(s => (
          <tr key={s.id}>
            <td style={{ ...monoCell, maxWidth: 320, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={`${s.name} · ${s.id}`}>
              {s.name}
            </td>
            <td style={tdStyle}>{s.segment_type}</td>
            <td style={numTd}>{fmtInt(s.subscriber_count)}</td>
            <td style={numTd}>{fmtInt(s.ledger_count)}</td>
            <td style={{ ...numTd, ...(s.last_built_at == null ? { color: colors.danger, fontWeight: 700 } : {}) }}
              title={s.last_built_at ?? 'no ledger build recorded'}>
              {s.last_built_at == null ? 'NEVER' : fmtRelative(s.last_built_at)}
            </td>
            <td style={numTd} title={s.last_count_at ?? 'never counted'}>{fmtRelative(s.last_count_at)}</td>
            <td style={tdStyle}>
              <Pill color={s.last_build_status === 'ok' ? colors.success
                : s.last_build_status === 'none' ? colors.idle
                : s.last_build_status === 'running' ? colors.info : colors.danger}>
                {s.last_build_status}
              </Pill>
            </td>
            <td style={{ ...tdStyle, color: colors.textMuted }}>{s.build_source || '—'}</td>
            <td style={numTd} title={`member-scoped ${win}`}>{s.perf ? fmtInt(s.perf.delivered) : '—*'}</td>
            <td style={numTd} title={`RAW opens (machine included) — member-scoped ${win}`}>{s.perf ? fmtInt(s.perf.opens) : '—*'}</td>
            <td style={numTd} title={`ACTION clicks only — member-scoped ${win}`}>{s.perf ? fmtInt(s.perf.clicks_action) : '—*'}</td>
            <td style={numTd} title={`action-clicks / delivered; denominator = ${s.perf ? fmtInt(s.perf.delivered) : 'n/a'}`}>
              {s.perf ? (s.perf.delivered > 0 ? fmtPct(s.perf.engagement_rate) : '—') : '—*'}
            </td>
            <td style={numTd}>{negCell(s.perf?.complaints, colors.danger)}</td>
            <td style={numTd}>{negCell(s.perf?.unsubs, colors.warning)}</td>
            <td style={numTd} title="HARD bounces — never combined with soft">{negCell(s.perf?.hard_bounces, colors.danger)}</td>
            <td style={numTd} title="SOFT bounces — never combined with hard">{negCell(s.perf?.soft_bounces, colors.warning)}</td>
            <td style={numTd} title={churnAvailable ? 'rows (re)materialized in the last 24h' : 'churn source unavailable this fetch'}>
              {s.member_inserts_24h == null ? '—*' : fmtInt(s.member_inserts_24h)}
            </td>
            <td style={numTd}>{s.campaign_refs_3d > 0 ? fmtInt(s.campaign_refs_3d) : '—'}</td>
            <td style={tdStyle}>
              {s.count_membership_diverged ? (
                <Pill color={colors.warning} style={{ whiteSpace: 'nowrap' }}>COUNTS≠BUILD</Pill>
              ) : (
                <span style={{ color: colors.textFaint }}>—</span>
              )}
            </td>
            <td style={tdStyle}>
              <CleanButton
                busy={cleaning}
                title={`Queue an EO cleaning job for "${s.name}" (${fmtInt(s.subscriber_count)} members)`}
                onClick={() => onClean([{ id: s.id, name: s.name }], s.subscriber_count, `segment "${s.name}"`)}
              />
            </td>
          </tr>
        ))}
      </tbody>
    </table>
    {family.segments_truncated && (
      <div style={{ fontSize: 11, color: colors.warningText, marginTop: 6 }}>
        Showing the {family.segments.length} most recently built segments — the family has {fmtInt(family.segments_count)} total.
      </div>
    )}
    {!perfAvailable && (
      <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 6 }}>
        —* performance columns: the nightly rollup (mailing_segment_perf_daily) has no data on this build yet — the segment_perf_rollup worker has not completed a pass.
      </div>
    )}
  </div>
);

// ── Page ────────────────────────────────────────────────────────────────────

export const SegmentationCommand: React.FC = () => {
  const [nonce, setNonce] = useState(0);
  const [win, setWin] = useState<'7d' | '30d'>('7d');
  const [state, retry] = useHealthFetch(nonce, win);
  const [expanded, setExpanded] = useState<string | null>(null);
  const [cleaning, setCleaning] = useState(false);
  const [cleanMsg, setCleanMsg] = useState<{ ok: boolean; text: string } | null>(null);

  // cleanSegments — POST one segment-sourced EO cleaning job per segment
  // (EO CLEANING SERVICE). Confirm quotes record count + per-record cost.
  const cleanSegments = useCallback(async (
    segs: { id: string; name: string }[], records: number, what: string,
  ) => {
    if (segs.length === 0 || cleaning) return;
    const across = segs.length > 1 ? ` across ${segs.length} segments` : '';
    if (!window.confirm(`Queue EO cleaning for ${what} (${fmtInt(records)} records${across})?\n\n${EO_CLEAN_COST_NOTE}`)) {
      return;
    }
    setCleaning(true);
    setCleanMsg(null);
    let queued = 0;
    const errors: string[] = [];
    for (const s of segs) {
      try {
        const res = await apiFetch('/api/mailing/eo-clean/jobs', {
          method: 'POST',
          body: JSON.stringify({ source_type: 'segment', source_ref: s.id, label: s.name }),
        });
        if (!res.ok) {
          let msg = `HTTP ${res.status}`;
          try {
            const body = await res.json();
            if (body?.error) msg += ` — ${body.error}`;
          } catch { /* non-JSON error body */ }
          errors.push(`${s.name}: ${msg}`);
        } else {
          queued++;
        }
      } catch (e: unknown) {
        errors.push(`${s.name}: ${e instanceof Error ? e.message : String(e)}`);
      }
    }
    setCleaning(false);
    setCleanMsg(errors.length === 0
      ? { ok: true, text: `${queued} cleaning job${queued === 1 ? '' : 's'} queued — track progress in the EO Cleaning tab.` }
      : { ok: false, text: `${queued} queued, ${errors.length} failed: ${errors.slice(0, 3).join('; ')}` });
  }, [cleaning]);

  const d = state.data;
  const churnAvailable = d?.churn_method === 'materialized_window';
  const refsAvailable = d?.refs_method === 'inclusion_segments_3d';
  const perfAvailable = d?.perf_method === 'member_scoped_rollup';
  const timelineAvailable = d?.churn_timeline_method === 'rollup_daily';

  const winToggle = (
    <span style={{ display: 'inline-flex', gap: 4 }}>
      {(['7d', '30d'] as const).map(w => (
        <button
          key={w}
          onClick={() => setWin(w)}
          style={{
            background: win === w ? alpha(colors.indigo500, '44') : alpha(colors.indigo500, '14'),
            border: `1px solid ${alpha(colors.indigo500, win === w ? '66' : '33')}`,
            color: win === w ? colors.indigo200 : colors.textMuted,
            borderRadius: 6, padding: '3px 10px', cursor: 'pointer', fontSize: 11.5, fontWeight: 700,
          }}>
          {w.toUpperCase()}
        </button>
      ))}
    </span>
  );

  return (
    <div style={pageStyle}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
        <div>
          <h2 style={{ margin: 0, fontSize: 20, display: 'flex', alignItems: 'center', gap: 10 }}>
            <FontAwesomeIcon icon={faLayerGroup} style={{ color: colors.indigo400 }} /> Segmentation Command
          </h2>
          <div style={{ fontSize: 12, color: colors.textMuted, marginTop: 4 }}>
            Membership-build truth per segment family — verdicts against declared SLAs, worker liveness, churn and campaign use. v{PAGE_VERSION}
            {state.fetchedAt && <span> · fetched {state.fetchedAt}{state.ms != null ? ` · ${state.ms}ms` : ''}</span>}
          </div>
        </div>
        <button
          onClick={() => setNonce(n => n + 1)}
          style={{
            background: alpha(colors.indigo500, '22'), border: `1px solid ${alpha(colors.indigo500, '66')}`,
            color: colors.text, borderRadius: 8, padding: '8px 14px', cursor: 'pointer', fontSize: 12.5,
            display: 'flex', alignItems: 'center', gap: 8,
          }}>
          <FontAwesomeIcon icon={faRotate} /> Refresh
        </button>
      </div>

      {state.loading && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, color: colors.textMuted, fontSize: 13, padding: '18px 4px' }}>
          <FontAwesomeIcon icon={faSpinner} spin /> Loading segmentation health…
        </div>
      )}

      {!state.loading && state.unavailable && (
        <div style={{
          padding: '14px 16px', borderRadius: 8, fontSize: 12.5, lineHeight: 1.6,
          background: alpha(colors.warning, '14'), border: `1px solid ${alpha(colors.warning, '44')}`,
          color: colors.warning,
        }}>
          <strong>SOURCE UNAVAILABLE.</strong> <code style={{ fontFamily: 'monospace' }}>/api/mailing/segmentation/health</code> is
          not exposed by this server build — the Segmentation Command backend has not been deployed here yet.
        </div>
      )}

      {!state.loading && state.error && (
        <SectionError label="segmentation health" error={state.error} onRetry={retry} />
      )}

      {!state.loading && !state.error && !state.unavailable && d && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
          {/* ── Top strip: estate verdict + worker lights ── */}
          <div style={panelStyle}>
            <SectionHeader title="Estate verdict" icon={faHeartPulse} />
            <div style={{ display: 'flex', gap: 26, flexWrap: 'wrap', marginBottom: 12 }}>
              <Stat label="Live" value={d.summary.families_live} color={colors.success}
                sub="membership built inside SLA" />
              <Stat label="Stale" value={d.summary.families_stale}
                color={d.summary.families_stale > 0 ? colors.danger : colors.textMuted}
                sub="membership build past SLA" />
              <Stat label="Unregistered" value={d.summary.families_unregistered}
                color={d.summary.families_unregistered > 0 ? colors.warning : colors.textMuted}
                sub="no registry owner — invisible to alarms" />
              <Stat label="Static-declared" value={d.summary.families_static} color={colors.idle}
                sub="family declares no refresh SLA" />
              <Stat label="Segments" value={fmtInt(d.summary.segments_total)}
                sub={`across ${d.families.length} families`} />
              <Stat label="Members" value={fmtInt(d.summary.subscriber_total)}
                sub="SUM of segment counts (overlap not deduped)*" />
            </div>
            <WorkerLights workers={d.workers} />
            {!d.registry_available && (
              <div style={{ fontSize: 11.5, color: colors.warningText, marginTop: 10 }}>
                Registry source failed on this fetch — every family below shows UNREGISTERED by construction, not by truth. Retry before acting.
              </div>
            )}
            {d.segments_truncated && (
              <div style={{ fontSize: 11.5, color: colors.warningText, marginTop: 10 }}>
                Segment scan hit its hard bound — family aggregates below are computed over a truncated set.
              </div>
            )}
          </div>

          {/* ── Alerts ── */}
          <AlertsPanel alerts={d.alerts} />

          {/* ── EO Clean action result (jobs live on the EO Cleaning tab) ── */}
          {cleanMsg && (
            <div style={{
              padding: '10px 14px', borderRadius: 8, fontSize: 12.5,
              background: alpha(cleanMsg.ok ? colors.success : colors.danger, '14'),
              border: `1px solid ${alpha(cleanMsg.ok ? colors.success : colors.danger, '44')}`,
              color: cleanMsg.ok ? colors.successText : colors.danger,
              display: 'flex', alignItems: 'center', gap: 10,
            }}>
              <FontAwesomeIcon icon={faBroom} />
              {cleanMsg.text}
              <button onClick={() => setCleanMsg(null)} style={{
                marginLeft: 'auto', background: 'none', border: 'none', color: 'inherit',
                cursor: 'pointer', fontSize: 12.5,
              }}>✕</button>
            </div>
          )}

          {/* ── Family table ── */}
          <div style={panelStyle}>
            <SectionHeader
              title={`Segment families — build truth + member-scoped performance (${win})`}
              icon={faLayerGroup}
              right={winToggle}
            />
            {d.families.length === 0 ? (
              <EmptyState
                title="No segments found"
                hint="mailing_segments has no non-archived rows for this org — nothing to verdict."
              />
            ) : (
              <div style={scrollWrap}>
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={thStyle} />
                      <th style={thStyle}>Family</th>
                      <th style={thStyle}>Verdict</th>
                      <th style={numTh}>Segments</th>
                      <th style={numTh}>Members</th>
                      <th style={numTh}>Last build</th>
                      <th style={numTh}>Delivered</th>
                      <th style={numTh}>Opens</th>
                      <th style={numTh}>Clicks*</th>
                      <th style={numTh}>Eng rate</th>
                      <th style={numTh}>Cmpl</th>
                      <th style={numTh}>Unsub</th>
                      <th style={numTh}>Hard</th>
                      <th style={numTh}>Soft</th>
                      <th style={thStyle}>Churn 14d</th>
                      <th style={numTh}>Refs 3d</th>
                      <th style={thStyle}>Flags</th>
                      <th style={thStyle}>Clean</th>
                    </tr>
                  </thead>
                  <tbody>
                    {d.families.map(f => {
                      const rowKey = `${f.registered ? 'reg' : 'unreg'}:${f.family_pattern}`;
                      const isOpen = expanded === rowKey;
                      return (
                        <React.Fragment key={rowKey}>
                          <tr
                            onClick={() => setExpanded(isOpen ? null : rowKey)}
                            style={{ cursor: 'pointer' }}
                            title={`pattern ${f.family_pattern}${f.cadence ? ` · cadence ${f.cadence}` : ''}`}
                          >
                            <td style={{ ...tdStyle, width: 24, color: colors.textMuted }}>
                              <FontAwesomeIcon icon={isOpen ? faChevronDown : faChevronRight} />
                            </td>
                            <td style={{ ...tdStyle, fontWeight: 700 }}
                              title={`${f.family_pattern}${f.registered ? ` · owner: ${f.owner || '—'} · SLA ${f.sla_hours > 0 ? `${f.sla_hours}h` : 'none'}` : ' · UNOWNED (no registry row)'}${f.cadence ? ` · cadence ${f.cadence}` : ''} · builds last 24h: ${f.builds_last_24h} · inserts 24h: ${churnAvailable ? fmtInt(f.member_inserts_24h) : 'n/a'}`}>
                              {f.family_key}
                              <span style={{ fontFamily: 'monospace', fontSize: 10.5, color: f.registered ? colors.textFaint : colors.warningText, marginLeft: 8 }}>
                                {f.registered ? f.family_pattern : 'UNOWNED'}
                              </span>
                            </td>
                            <td style={tdStyle}><Pill color={verdictColor(f.verdict)}>{f.verdict}</Pill></td>
                            <td style={numTd} title={`${f.dynamic_count} dynamic · ${f.static_count} static`}>
                              {fmtInt(f.segments_count)}
                              <span style={{ color: colors.textFaint, fontSize: 10.5 }}> ({f.dynamic_count}d/{f.static_count}s)</span>
                            </td>
                            <td style={numTd}>{fmtInt(f.subscriber_total)}</td>
                            <td style={{ ...numTd, ...buildAgeStyle(f) }}
                              title={f.last_membership_build_max
                                ? `newest ${f.last_membership_build_max} · oldest ${f.last_membership_build_min ?? '—'} (ledger last_built_at)`
                                : 'no membership build ever recorded in the ledger'}>
                              {f.last_membership_build_max == null ? 'NEVER' : fmtRelative(f.last_membership_build_max)}
                            </td>
                            <td style={numTd} title={`member-scoped ${win} — distinct current members with a delivery in the window, any campaign`}>
                              {f.perf ? fmtInt(f.perf.delivered) : '—*'}
                            </td>
                            <td style={numTd} title={`RAW opens (machine included, no human-verdict claim) — member-scoped ${win}`}>
                              {f.perf ? fmtInt(f.perf.opens) : '—*'}
                            </td>
                            <td style={numTd} title={`ACTION clicks only (canonical predicate — asset fetches and unsub/pref links are NOT clicks) — member-scoped ${win}`}>
                              {f.perf ? fmtInt(f.perf.clicks_action) : '—*'}
                            </td>
                            <td style={numTd} title={`action-clicks / delivered (member-scoped ${win}); denominator = delivered ${f.perf ? fmtInt(f.perf.delivered) : 'n/a'}`}>
                              {f.perf ? (f.perf.delivered > 0 ? fmtPct(f.perf.engagement_rate) : '—') : '—*'}
                            </td>
                            <td style={numTd} title={`complaints — member-scoped ${win}`}>{negCell(f.perf?.complaints, colors.danger)}</td>
                            <td style={numTd} title={`unsubscribes — member-scoped ${win}`}>{negCell(f.perf?.unsubs, colors.warning)}</td>
                            <td style={numTd} title={`HARD bounces — member-scoped ${win}, never combined with soft`}>{negCell(f.perf?.hard_bounces, colors.danger)}</td>
                            <td style={numTd} title={`SOFT bounces — member-scoped ${win}, never combined with hard`}>{negCell(f.perf?.soft_bounces, colors.warning)}</td>
                            <td style={tdStyle} title={timelineAvailable ? 'members added (up, indigo) / removed (down, red — derived day-over-day) per Denver day' : 'rollup timeline unavailable'}>
                              {timelineAvailable ? <ChurnSparkline timeline={f.churn_timeline} /> : <span style={{ color: colors.textFaint }}>—*</span>}
                            </td>
                            <td style={numTd}
                              title={refsAvailable ? 'campaigns created in the last 3 days including this family in inclusion_segments' : 'refs source unavailable this fetch'}>
                              {refsAvailable ? (f.campaign_refs_3d > 0 ? fmtInt(f.campaign_refs_3d) : '—') : '—*'}
                            </td>
                            <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>
                              {f.diverged_count > 0 && (
                                <Pill color={colors.warning} style={{ marginRight: 6 }}
                                  >{f.diverged_count} COUNTS≠BUILD</Pill>
                              )}
                              {f.failed_now > 0 && (
                                <Pill color={colors.danger}>{f.failed_now} FAILED</Pill>
                              )}
                            </td>
                            <td style={tdStyle}>
                              <CleanButton
                                busy={cleaning}
                                title={`Queue EO cleaning jobs for the ${f.segments.length} listed segment(s) of ${f.family_key} (${fmtInt(f.subscriber_total)} members total)${f.segments_truncated ? ' — drilldown is truncated; only the listed segments are cleaned' : ''}`}
                                onClick={() => cleanSegments(
                                  f.segments.map(s => ({ id: s.id, name: s.name })),
                                  f.subscriber_total,
                                  `family "${f.family_key}"`)}
                              />
                            </td>
                          </tr>
                          {isOpen && (
                            <tr>
                              <td colSpan={18} style={{ padding: 0, borderTop: `1px solid ${colors.divider}`, background: alpha(colors.indigo500, '0d') }}>
                                <SegmentDrilldown family={f} churnAvailable={churnAvailable} perfAvailable={perfAvailable} win={win} onClean={cleanSegments} cleaning={cleaning} />
                              </td>
                            </tr>
                          )}
                        </React.Fragment>
                      );
                    })}
                  </tbody>
                </table>
                <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 8, lineHeight: 1.6 }}>
                  Last build = newest membership build in the family (ledger <code>last_built_at</code>) — NEVER a count-refresh timestamp; red = past the family's SLA.
                  COUNTS≠BUILD = the segment's count clock advanced &gt;24h after its last membership build — the "counts fresh, membership stale" trap, flagged explicitly.
                  Members = SUM of per-segment counts (overlap not deduped).
                  {' '}Performance columns are <strong>member-scoped {win}</strong> (Denver days, nightly rollup): distinct CURRENT members with the event in the window, from ANY campaign — NOT send-attributed.
                  Opens are RAW (machine included; no human-verdict claim). Clicks* = ACTION clicks only (canonical predicate; asset fetches and unsub/pref links are not clicks — ~93% of raw click events are asset noise). Eng rate = action-clicks / delivered. Hard and soft bounces are never combined.
                  Churn 14d: adds up (indigo) / removals down (red); removals are DERIVED day-over-day from membership snapshots — full rebuilds re-stamp rows, inflating adds and removals equally (net is true).
                  {!perfAvailable && ' —* Performance: the nightly rollup has no data yet on this build (perf_method=unavailable) — no number is shown rather than a wrong one.'}
                  {!churnAvailable && ' —* Inserts (drilldown): the bounded churn aggregate over mailing_segment_members timed out or failed this fetch (churn_method=unavailable).'}
                  {!refsAvailable && ' —* Refs: the campaign-reference scan was unavailable this fetch.'}
                </div>
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  );
};

export default SegmentationCommand;
