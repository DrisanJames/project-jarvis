import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faBolt,
  faChevronDown,
  faChevronRight,
  faExclamationTriangle,
  faGaugeHigh,
  faHeartPulse,
  faLayerGroup,
  faSkullCrossbones,
  faSpinner,
  faSyncAlt,
  faWater,
} from '@fortawesome/free-solid-svg-icons';
import {
  Area,
  CartesianGrid,
  ComposedChart,
  Legend,
  Line,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';
import { apiFetch } from '../shared/apiFetch';

// OutboxDashboard — the live sending-engine board.
//
// Polls GET /api/mailing/outbox/engine-status (internal/api/outbox_engine_status.go)
// every 10s. One glance answers: are we SENDING / IDLE / BACKLOGGED / in a
// deferral STORM, how close to 24h-peak capacity we are, what the wave manager
// is about to do, and which ISPs are deferring above their own baseline.
// Sections render independently — a failed backend section shows an inline
// error chip, never a blank page. The dead-letter table from the v1 page is
// preserved at the bottom (still /api/outbox/dead-letter), filtered
// client-side to the last 8h, newest first, max 50 rows.
// v2.1: per-ISP rows expand into a per-VMTA pipes sub-table
// (/api/mailing/outbox/isp-pipes, fetched on expand only).
// v2.2: schedule split into Broadcast Sends + Data Partner Drips (same panel,
// partner-drip names parsed into offer · brand · touch · route), new Click
// Funnels section (the waveless click-drip path: enrollments + per-offer 24h
// scorecard), workers moved into their own card grid, and the freed panel is
// now the 24h Performance scoreboard (volume, open/click % raw+human,
// complaints, conversions). Backend counterpart: engine-status API v1.2.

const PAGE_VERSION = '2.2';
const POLL_INTERVAL_MS = 10_000;
const DEAD_LETTER_POLL_EVERY_N_TICKS = 3; // every ~30s
const DEAD_LETTER_WINDOW_HOURS = 8;
const DEAD_LETTER_MAX_ROWS = 50;
const BACKLOG_QUEUED_THRESHOLD = 1_000;
const BACKLOG_AGE_THRESHOLD_SEC = 15 * 60;

// ---------------------------------------------------------------------------
// API types (mirror outbox_engine_status.go)
// ---------------------------------------------------------------------------

interface ISPQueueDepth {
  isp: string;
  queued: number;
}

interface QueueSection {
  ok: boolean;
  error?: string;
  queued: number;
  processing: number;
  sent_24h: number;
  failed_24h: number;
  dead_letter_24h: number;
  oldest_queued_seconds: number;
  by_isp: ISPQueueDepth[];
}

interface UpcomingWave {
  wave_id: string;
  campaign_id: string;
  campaign_name: string;
  wave_number: number;
  scheduled_at: string;
  planned_recipients: number;
}

interface WaveSection {
  ok: boolean;
  error?: string;
  planned_due: number;
  planned_future: number;
  enqueuing: number;
  sending: number;
  // completed_24h counts wave EXECUTIONS, not messages;
  // completed_recipients_24h is the planned-recipient sum of those waves.
  completed_24h: number;
  completed_recipients_24h: number;
  failed_24h: number;
  cancelled_24h: number;
  upcoming: UpcomingWave[];
}

interface MinutePoint {
  minute: string;
  sent: number;
  deferred: number;
}

interface HourPoint {
  hour: string;
  sent: number;
}

interface ThroughputSection {
  ok: boolean;
  error?: string;
  per_minute: MinutePoint[];
  per_hour: HourPoint[];
  current_rate_per_min: number;
  peak_rate_per_min_24h: number;
  utilization_pct: number;
}

interface ISPDeferral {
  isp: string;
  sent_60m: number;
  deferred_60m: number;
  recent_rate: number;
  baseline_rate: number;
  multiplier: number;
  sent_24h: number;
  storm: boolean;
}

interface StormSection {
  ok: boolean;
  error?: string;
  active: boolean;
  isps: ISPDeferral[];
}

interface WorkerBeat {
  name: string;
  last_beat_at: string;
  seconds_since_beat: number;
  expected_interval_seconds: number;
  stalled: boolean;
  last_status: string;
  last_error?: string;
}

interface WorkerSection {
  ok: boolean;
  error?: string;
  workers: WorkerBeat[];
}

interface ClickFunnelOffer {
  everflow_offer_id: string;
  offer_name: string;
  active_enrollments: number;
  due_now: number;
  sent_24h: number;
  opens_24h: number;
  clicks_24h: number;
  converted_24h: number;
}

interface ClickFunnelSection {
  ok: boolean;
  error?: string;
  active_enrollments: number;
  due_now: number;
  sent_24h: number;
  opens_24h: number;
  clicks_24h: number;
  converted_24h: number;
  offers: ClickFunnelOffer[];
}

interface PerformanceSection {
  ok: boolean;
  error?: string;
  window_hours: number;
  sent_24h: number;
  delivered_24h: number;
  opens_raw_24h: number;
  clicks_raw_24h: number;
  human_ok: boolean;
  opens_human_24h: number;
  clicks_human_24h: number;
  complaints_24h: number;
  conversions_24h: number;
}

interface EngineStatusResponse {
  generated_at: string;
  api_version: string;
  cache_age_seconds: number;
  queue: QueueSection;
  waves: WaveSection;
  waves_broadcast: WaveSection;
  waves_partner_drip: WaveSection;
  click_funnels: ClickFunnelSection;
  performance: PerformanceSection;
  throughput: ThroughputSection;
  storm: StormSection;
  workers: WorkerSection;
}

interface DeadLetterRow {
  id: string;
  campaign_id: string;
  subscriber_id: string;
  email: string;
  status: string;
  attempts: number;
  last_error?: string;
  last_attempt_at: string;
}

interface DeadLetterResponse {
  rows: DeadLetterRow[];
  count: number;
}

// isp-pipes drill-down (mirror HandleOutboxISPPipes in outbox_engine_status.go)

interface ISPPipeRow {
  vmta: string;
  attempted: number;
  delivered: number;
  deferred: number;
  hard_bounces: number;
  delivered_rate: number;
  deferral_rate: number;
  hard_bounce_rate: number;
}

interface ISPPipesResponse {
  isp: string;
  window_hours: number;
  generated_at: string;
  cache_age_seconds: number;
  rows: ISPPipeRow[];
}

interface PipesState {
  loading: boolean;
  error: string | null;
  rows: ISPPipeRow[];
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

const fmt = (n: number | null | undefined): string =>
  n == null || Number.isNaN(n) ? '0' : n.toLocaleString();

const fmtRate = (n: number): string =>
  n >= 100 ? Math.round(n).toLocaleString() : n.toFixed(1);

const fmtPct = (frac: number): string => `${(frac * 100).toFixed(1)}%`;

const fmtDuration = (s: number): string => {
  if (s <= 0) return '—';
  if (s < 60) return `${Math.round(s)}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${Math.round(s % 60)}s`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h}h ${m % 60}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
};

const fmtClock = (iso: string): string =>
  new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

const titleCase = (s: string): string =>
  s.length === 0 ? s : s.charAt(0).toUpperCase() + s.slice(1);

// ---------------------------------------------------------------------------
// Engine state derivation
// ---------------------------------------------------------------------------

type EngineState = 'STORM' | 'BACKLOGGED' | 'SENDING' | 'IDLE';

interface EngineSummary {
  state: EngineState;
  color: string;
  sentence: string;
}

const deriveEngine = (d: EngineStatusResponse): EngineSummary => {
  const rate = d.throughput.ok ? d.throughput.current_rate_per_min : 0;
  const util = d.throughput.ok ? d.throughput.utilization_pct : 0;
  const queued = d.queue.ok ? d.queue.queued : 0;
  const oldest = d.queue.ok ? d.queue.oldest_queued_seconds : 0;
  const storms = d.storm.ok ? d.storm.isps.filter((i) => i.storm) : [];

  const ratePhrase = `Sending ${fmtRate(rate)}/min, ${Math.round(util)}% of 24h peak.`;

  if (d.storm.ok && d.storm.active && storms.length > 0) {
    const worst = storms[0];
    return {
      state: 'STORM',
      color: '#ef4444',
      sentence: `${ratePhrase} ${titleCase(worst.isp)} delaying at ${fmtPct(worst.recent_rate)} (${worst.multiplier.toFixed(1)}× baseline) — sending is being paced automatically.${
        storms.length > 1 ? ` ${storms.length - 1} more mailbox provider${storms.length > 2 ? 's' : ''} delaying heavily.` : ''
      }`,
    };
  }
  if (queued > BACKLOG_QUEUED_THRESHOLD && oldest > BACKLOG_AGE_THRESHOLD_SEC) {
    return {
      state: 'BACKLOGGED',
      color: '#f59e0b',
      sentence: `${fmt(queued)} queued with oldest waiting ${fmtDuration(oldest)} — sending is behind schedule. ${ratePhrase}`,
    };
  }
  if (rate >= 1 || (d.queue.ok && d.queue.processing > 0)) {
    const eta = rate > 0 && queued > 0 ? ` Queue clears in about ${fmtDuration((queued / rate) * 60)}.` : '';
    return {
      state: 'SENDING',
      color: '#22c55e',
      sentence: `${ratePhrase} ${fmt(queued)} queued.${eta} Delays normal across mailbox providers.`,
    };
  }
  const dueBit = d.waves.ok
    ? ` ${fmt(d.waves.planned_due)} wave${d.waves.planned_due === 1 ? '' : 's'} due, ${fmt(d.waves.planned_future)} scheduled ahead.`
    : '';
  return {
    state: 'IDLE',
    color: '#94a3b8',
    sentence: `No sends in the last 5 minutes. ${fmt(queued)} queued.${dueBit}`,
  };
};

// ---------------------------------------------------------------------------
// Shared styles
// ---------------------------------------------------------------------------

const PANEL_BG = 'rgba(15,30,60,0.35)';
const PANEL_BORDER = '1px solid rgba(99,102,241,0.18)';
const HEADING_COLOR = '#dbeafe';

const panelStyle: React.CSSProperties = {
  background: PANEL_BG,
  border: PANEL_BORDER,
  borderRadius: 10,
  padding: '14px 16px',
};

const panelTitleStyle: React.CSSProperties = {
  margin: 0,
  fontSize: 13,
  fontWeight: 600,
  color: HEADING_COLOR,
  textTransform: 'uppercase',
  letterSpacing: 0.6,
  display: 'flex',
  alignItems: 'center',
  gap: 8,
};

const thStyle: React.CSSProperties = {
  textAlign: 'left',
  padding: '7px 10px',
  fontWeight: 600,
  fontSize: 11,
  textTransform: 'uppercase',
  letterSpacing: 0.4,
  color: '#94a3b8',
  borderBottom: '1px solid rgba(99,102,241,0.15)',
};

const tdStyle: React.CSSProperties = {
  padding: '7px 10px',
  fontSize: 12,
  color: '#e5e7eb',
  borderTop: '1px solid rgba(99,102,241,0.08)',
};

const numTd: React.CSSProperties = { ...tdStyle, textAlign: 'right', fontVariantNumeric: 'tabular-nums' };
const numTh: React.CSSProperties = { ...thStyle, textAlign: 'right' };

const SectionError: React.FC<{ label: string; error?: string }> = ({ label, error }) => (
  <div
    style={{
      fontSize: 12,
      color: '#fca5a5',
      background: 'rgba(239,68,68,0.08)',
      border: '1px solid rgba(239,68,68,0.25)',
      borderRadius: 6,
      padding: '8px 10px',
    }}
  >
    <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 6 }} />
    {label} unavailable{error ? `: ${error}` : ''}
  </div>
);

const Stat: React.FC<{ label: string; value: string; color?: string; title?: string }> = ({ label, value, color, title }) => (
  <div style={{ minWidth: 90 }} title={title}>
    <div style={{ fontSize: 10, color: '#94a3b8', textTransform: 'uppercase', letterSpacing: 0.5 }}>{label}</div>
    <div style={{ fontSize: 20, fontWeight: 700, color: color ?? '#e5e7eb', fontVariantNumeric: 'tabular-nums' }}>
      {value}
    </div>
  </div>
);

// PipesSubTable renders the inline per-VMTA breakdown under an expanded ISP
// row: which MTA pipe is struggling for this family (last 24h, pmta_acct_raw).
const PipesSubTable: React.FC<{ isp: string; state?: PipesState }> = ({ isp, state }) => {
  if (!state || state.loading) {
    return (
      <div style={{ padding: '12px 16px', fontSize: 12, color: '#94a3b8' }}>
        <FontAwesomeIcon icon={faSpinner} spin style={{ marginRight: 6 }} />
        Loading sending servers for {titleCase(isp)}…
      </div>
    );
  }
  if (state.error) {
    return (
      <div style={{ padding: '10px 16px' }}>
        <SectionError label={`${titleCase(isp)} sending servers`} error={state.error} />
      </div>
    );
  }
  if (state.rows.length === 0) {
    return (
      <div style={{ padding: '12px 16px', fontSize: 12, color: '#64748b' }}>
        No delivery activity for {titleCase(isp)} in the last 24h.
      </div>
    );
  }
  return (
    <table style={{ width: '100%', borderCollapse: 'collapse' }}>
      <thead>
        <tr>
          <th style={{ ...thStyle, paddingLeft: 28 }}>Sending Server</th>
          <th style={numTh}>Attempted</th>
          <th style={numTh}>Delivered %</th>
          <th style={numTh}>Delayed %</th>
          <th style={numTh}>Bounces</th>
        </tr>
      </thead>
      <tbody>
        {state.rows.map((p) => {
          const defColor = p.deferral_rate > 0.5 ? '#ef4444' : p.deferral_rate > 0.1 ? '#f59e0b' : '#86efac';
          return (
            <tr key={p.vmta}>
              <td style={{ ...tdStyle, paddingLeft: 28, fontFamily: 'monospace', fontSize: 11, color: '#c7d2fe' }}>
                {p.vmta}
              </td>
              <td style={numTd}>{fmt(p.attempted)}</td>
              <td style={numTd}>{fmtPct(p.delivered_rate)}</td>
              <td style={{ ...numTd, color: defColor, fontWeight: 600 }}>{fmtPct(p.deferral_rate)}</td>
              <td style={{ ...numTd, color: p.hard_bounces > 0 ? '#ef4444' : '#e5e7eb' }}>{fmt(p.hard_bounces)}</td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
};

// ---------------------------------------------------------------------------
// Partner-drip name display
// ---------------------------------------------------------------------------
// The orchestrator names drip campaigns
//   "[partner-drip] <vertical> <brand> <ts> <sha> [ses:<profile>] [tN]"
// (partner_drip_orchestrator.go). Raw, that reads like a log line — parse it
// into "Vertical · BRAND" with touch/route pills; the raw name stays in the
// tooltip. Unparseable names fall back to the raw string.

interface DripNameParts {
  label: string;
  touch: string | null;
  route: string | null;
}

const parsePartnerDripName = (name: string): DripNameParts | null => {
  if (!name.toLowerCase().startsWith('[partner-drip]')) return null;
  let touch: string | null = null;
  let route: string | null = null;
  for (const m of name.matchAll(/\[([^\]]+)\]/g)) {
    const tag = m[1].toLowerCase();
    if (tag.startsWith('ses')) route = 'SES';
    else if (tag.startsWith('pmta')) route = 'PMTA';
    else if (tag.startsWith('kumo')) route = 'Kumo';
    else if (/^t\d+$/.test(tag)) touch = tag.slice(1);
  }
  const core = name.replace(/\[[^\]]+\]/g, ' ').replace(/\s+/g, ' ').trim();
  const tokens = core.split(' ');
  const vertical = (tokens[0] || '').replace(/_/g, ' ').replace(/\b\w/g, (ch) => ch.toUpperCase());
  const brand = (tokens[1] || '').toUpperCase();
  const label = [vertical, brand].filter(Boolean).join(' · ');
  if (!label) return null;
  return { label, touch, route };
};

const tagPillStyle: React.CSSProperties = {
  fontSize: 9,
  fontWeight: 700,
  letterSpacing: 0.5,
  color: '#a5b4fc',
  background: 'rgba(99,102,241,0.14)',
  border: '1px solid rgba(99,102,241,0.35)',
  borderRadius: 999,
  padding: '1px 7px',
  flexShrink: 0,
};

const DripCampaignLabel: React.FC<{ name: string; campaignId: string }> = ({ name, campaignId }) => {
  const parts = parsePartnerDripName(name);
  if (!parts) return <>{name || campaignId.slice(0, 8)}</>;
  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6, maxWidth: '100%' }}>
      <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{parts.label}</span>
      {parts.touch && <span style={tagPillStyle}>T{parts.touch}</span>}
      {parts.route && <span style={tagPillStyle}>{parts.route}</span>}
    </span>
  );
};

// ---------------------------------------------------------------------------
// Schedule panel — one send family's wave board (used for Broadcast Sends and
// Data Partner Drips; identical display, different context/data).
// ---------------------------------------------------------------------------

const SchedulePanel: React.FC<{
  title: string;
  data?: WaveSection;
  renderCampaign?: (u: UpcomingWave) => React.ReactNode;
  emptyLabel: string;
}> = ({ title, data, renderCampaign, emptyLabel }) => (
  <div style={panelStyle}>
    <h2 style={panelTitleStyle}>
      <FontAwesomeIcon icon={faLayerGroup} style={{ color: '#818cf8' }} />
      {title}
    </h2>
    {!data || !data.ok ? (
      <div style={{ marginTop: 10 }}>
        <SectionError label={title} error={data?.error} />
      </div>
    ) : (
      <>
        <div style={{ display: 'flex', gap: 22, marginTop: 12, flexWrap: 'wrap' }}>
          <Stat label="Due now" value={fmt(data.planned_due)} color={data.planned_due > 0 ? '#f59e0b' : '#e5e7eb'} />
          <Stat label="Scheduled" value={fmt(data.planned_future)} color="#c7d2fe" />
          <Stat label="Preparing" value={fmt(data.enqueuing)} color="#93c5fd" />
          <Stat label="Sending" value={fmt(data.sending)} color="#86efac" />
          <Stat
            label="Batches done 24h"
            value={`${fmt(data.completed_24h)} (${fmt(data.completed_recipients_24h)} recipients)`}
            title="Count of completed send batches, not messages — recipients = planned recipients across those completed batches"
          />
          <Stat
            label="Failed 24h"
            value={fmt(data.failed_24h)}
            color={data.failed_24h > 0 ? '#ef4444' : '#e5e7eb'}
          />
        </div>
        <div style={{ marginTop: 12 }}>
          <div style={{ fontSize: 11, color: '#94a3b8', textTransform: 'uppercase', letterSpacing: 0.5, marginBottom: 6 }}>
            Next 5 send batches
          </div>
          {data.upcoming.length === 0 ? (
            <div style={{ fontSize: 12, color: '#64748b', padding: '10px 0' }}>{emptyLabel}</div>
          ) : (
            <table style={{ width: '100%', borderCollapse: 'collapse' }}>
              <thead>
                <tr>
                  <th style={thStyle}>Campaign</th>
                  <th style={numTh}>Batch</th>
                  <th style={thStyle}>Scheduled</th>
                  <th style={numTh}>Recipients</th>
                </tr>
              </thead>
              <tbody>
                {data.upcoming.map((u) => {
                  const due = new Date(u.scheduled_at).getTime() <= Date.now();
                  return (
                    <tr key={u.wave_id}>
                      <td style={{ ...tdStyle, maxWidth: 220, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={u.campaign_name}>
                        {renderCampaign ? renderCampaign(u) : (u.campaign_name || u.campaign_id.slice(0, 8))}
                      </td>
                      <td style={numTd}>#{u.wave_number}</td>
                      <td style={{ ...tdStyle, color: due ? '#f59e0b' : '#e5e7eb' }}>
                        {new Date(u.scheduled_at).toLocaleString([], {
                          month: 'short',
                          day: 'numeric',
                          hour: '2-digit',
                          minute: '2-digit',
                        })}
                        {due ? ' · due' : ''}
                      </td>
                      <td style={numTd}>{fmt(u.planned_recipients)}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
        </div>
      </>
    )}
  </div>
);

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

export const OutboxDashboard: React.FC = () => {
  const [data, setData] = useState<EngineStatusResponse | null>(null);
  const [deadLetter, setDeadLetter] = useState<DeadLetterRow[]>([]);
  const [loading, setLoading] = useState<boolean>(true);
  const [fetchError, setFetchError] = useState<string | null>(null);
  const [lastFetchAt, setLastFetchAt] = useState<Date | null>(null);
  const [, setClockTick] = useState<number>(0);
  const tickCount = useRef<number>(0);
  // Per-ISP → VMTA pipes drill-down: fetched on expand only (backend caches
  // per-family for ~120s, so re-expanding is cheap).
  const [expandedISP, setExpandedISP] = useState<string | null>(null);
  const [pipes, setPipes] = useState<Record<string, PipesState>>({});

  const fetchPipes = useCallback(async (ispName: string) => {
    setPipes((prev) => ({ ...prev, [ispName]: { loading: true, error: null, rows: prev[ispName]?.rows ?? [] } }));
    try {
      const res = await apiFetch(`/api/mailing/outbox/isp-pipes?isp=${encodeURIComponent(ispName)}`, {
        credentials: 'include',
      });
      if (!res.ok) throw new Error(`isp-pipes HTTP ${res.status}`);
      const json = (await res.json()) as ISPPipesResponse;
      setPipes((prev) => ({ ...prev, [ispName]: { loading: false, error: null, rows: json.rows || [] } }));
    } catch (err) {
      setPipes((prev) => ({
        ...prev,
        [ispName]: { loading: false, error: err instanceof Error ? err.message : String(err), rows: [] },
      }));
    }
  }, []);

  const toggleISP = useCallback(
    (ispName: string) => {
      setExpandedISP((prev) => (prev === ispName ? null : ispName));
      if (expandedISP !== ispName) void fetchPipes(ispName);
    },
    [expandedISP, fetchPipes],
  );

  const fetchStatus = useCallback(async () => {
    try {
      const res = await apiFetch('/api/mailing/outbox/engine-status', { credentials: 'include' });
      if (!res.ok) throw new Error(`engine-status HTTP ${res.status}`);
      const json = (await res.json()) as EngineStatusResponse;
      setData(json);
      setFetchError(null);
      setLastFetchAt(new Date());
    } catch (err) {
      setFetchError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }

    // Dead-letter is heavier and changes slowly — refresh every 3rd tick.
    if (tickCount.current % DEAD_LETTER_POLL_EVERY_N_TICKS === 0) {
      try {
        const res = await apiFetch('/api/outbox/dead-letter?limit=50', { credentials: 'include' });
        if (res.ok) {
          const json = (await res.json()) as DeadLetterResponse;
          setDeadLetter(json.rows || []);
        }
      } catch {
        // dead-letter is a bonus panel; never let it surface as a page error
      }
    }
    tickCount.current += 1;
  }, []);

  useEffect(() => {
    fetchStatus();
    const poll = setInterval(fetchStatus, POLL_INTERVAL_MS);
    const clock = setInterval(() => setClockTick((t) => t + 1), 1000);
    return () => {
      clearInterval(poll);
      clearInterval(clock);
    };
  }, [fetchStatus]);

  const engine = useMemo(() => (data ? deriveEngine(data) : null), [data]);

  const chartData = useMemo(() => {
    if (!data?.throughput.ok) return [];
    return data.throughput.per_minute.map((p) => ({
      label: fmtClock(p.minute),
      sent: p.sent,
      deferred: p.deferred,
    }));
  }, [data]);

  // Per-ISP merged view: storm rows (deferral + sent share) joined with
  // queued depth; queue-only ISPs appended after.
  const ispRows = useMemo(() => {
    if (!data) return [];
    const queuedByISP = new Map<string, number>();
    if (data.queue.ok) data.queue.by_isp.forEach((q) => queuedByISP.set(q.isp, q.queued));
    const totalSent24h = data.storm.ok ? data.storm.isps.reduce((acc, i) => acc + i.sent_24h, 0) : 0;
    const rows = (data.storm.ok ? data.storm.isps : []).map((i) => ({
      isp: i.isp,
      queued: queuedByISP.get(i.isp) ?? 0,
      sent24h: i.sent_24h,
      sentShare: totalSent24h > 0 ? i.sent_24h / totalSent24h : 0,
      recentRate: i.recent_rate,
      baselineRate: i.baseline_rate,
      multiplier: i.multiplier,
      storm: i.storm,
    }));
    const seen = new Set(rows.map((r) => r.isp));
    queuedByISP.forEach((queued, isp) => {
      if (!seen.has(isp)) {
        rows.push({ isp, queued, sent24h: 0, sentShare: 0, recentRate: 0, baselineRate: 0, multiplier: 0, storm: false });
      }
    });
    return rows;
  }, [data]);

  // Dead-letter window: the endpoint returns the most recent rows (newest
  // first by COALESCE(last_attempt_at, created_at), see HandleOutboxDeadLetter
  // in outbox_admin.go) but with no time bound — filter to the last 8h here
  // so the operator sees what's failing NOW, not a week of history.
  const recentDeadLetter = useMemo(() => {
    const cutoff = Date.now() - DEAD_LETTER_WINDOW_HOURS * 3_600_000;
    return deadLetter
      .filter((r) => {
        const t = new Date(r.last_attempt_at).getTime();
        return !Number.isNaN(t) && t >= cutoff;
      })
      .sort((a, b) => new Date(b.last_attempt_at).getTime() - new Date(a.last_attempt_at).getTime())
      .slice(0, DEAD_LETTER_MAX_ROWS);
  }, [deadLetter]);

  const secondsSinceFetch = lastFetchAt ? Math.max(0, Math.round((Date.now() - lastFetchAt.getTime()) / 1000)) : null;

  const drainEtaSec =
    data?.queue.ok && data?.throughput.ok && data.throughput.current_rate_per_min > 0
      ? (data.queue.queued / data.throughput.current_rate_per_min) * 60
      : null;

  return (
    <div style={{ padding: 24, color: '#e5e7eb', minHeight: '100vh' }}>
      <style>{`@keyframes outboxPulse { 0% { opacity: 1; } 50% { opacity: 0.35; } 100% { opacity: 1; } }`}</style>

      {/* ---- Header ---- */}
      <header style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 16 }}>
        <div>
          <h1 style={{ margin: 0, fontSize: 22, color: HEADING_COLOR, display: 'flex', alignItems: 'center', gap: 10 }}>
            <FontAwesomeIcon icon={faBolt} style={{ color: '#818cf8' }} />
            Sending Engine
          </h1>
          <div style={{ fontSize: 12, color: '#94a3b8', marginTop: 4 }}>
            Live send schedule + delivery monitoring · polls every {POLL_INTERVAL_MS / 1000}s · Page v{PAGE_VERSION}
            {data && <> · API v{data.api_version}</>}
          </div>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 14 }}>
          <span style={{ display: 'flex', alignItems: 'center', gap: 7, fontSize: 12, color: '#94a3b8' }}>
            <span
              style={{
                width: 9,
                height: 9,
                borderRadius: '50%',
                background: fetchError ? '#ef4444' : '#22c55e',
                animation: fetchError ? undefined : 'outboxPulse 2s ease-in-out infinite',
                display: 'inline-block',
              }}
            />
            <span style={{ color: fetchError ? '#fca5a5' : '#86efac', fontWeight: 600, letterSpacing: 1 }}>
              {fetchError ? 'STALE' : 'LIVE'}
            </span>
            {secondsSinceFetch != null && <span>updated {secondsSinceFetch}s ago</span>}
          </span>
          <button
            onClick={fetchStatus}
            disabled={loading}
            style={{
              background: 'rgba(99,102,241,0.15)',
              color: '#c7d2fe',
              border: '1px solid rgba(99,102,241,0.4)',
              padding: '6px 14px',
              borderRadius: 6,
              cursor: loading ? 'not-allowed' : 'pointer',
              display: 'flex',
              alignItems: 'center',
              gap: 6,
              fontSize: 13,
            }}
          >
            <FontAwesomeIcon icon={loading ? faSpinner : faSyncAlt} spin={loading} />
            Refresh
          </button>
        </div>
      </header>

      {fetchError && (
        <div
          style={{
            background: 'rgba(239,68,68,0.1)',
            border: '1px solid rgba(239,68,68,0.35)',
            color: '#fecaca',
            padding: '10px 14px',
            borderRadius: 8,
            marginBottom: 14,
            fontSize: 13,
          }}
        >
          <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 8 }} />
          Could not refresh engine status: {fetchError} — showing last known data.
        </div>
      )}

      {loading && !data ? (
        <div style={{ textAlign: 'center', padding: 80, color: '#94a3b8' }}>
          <FontAwesomeIcon icon={faSpinner} spin /> Loading engine state…
        </div>
      ) : data && engine ? (
        <>
          {/* ---- Status hero ---- */}
          <section
            style={{
              ...panelStyle,
              marginBottom: 14,
              display: 'flex',
              alignItems: 'center',
              gap: 20,
              flexWrap: 'wrap',
              borderLeft: `4px solid ${engine.color}`,
            }}
          >
            <span
              style={{
                background: `${engine.color}22`,
                border: `1px solid ${engine.color}66`,
                color: engine.color,
                fontWeight: 800,
                fontSize: 16,
                letterSpacing: 2,
                padding: '8px 18px',
                borderRadius: 999,
              }}
            >
              {engine.state}
            </span>
            <div style={{ flex: 1, minWidth: 280, fontSize: 14, color: '#e5e7eb', lineHeight: 1.5 }}>
              {engine.sentence}
            </div>
            <div style={{ display: 'flex', gap: 24, flexWrap: 'wrap' }}>
              <Stat
                label="Rate /min"
                value={data.throughput.ok ? fmtRate(data.throughput.current_rate_per_min) : '—'}
                color="#86efac"
              />
              <Stat label="Queued" value={data.queue.ok ? fmt(data.queue.queued) : '—'} color="#c7d2fe" />
              <Stat
                label="Oldest queued"
                value={data.queue.ok ? fmtDuration(data.queue.oldest_queued_seconds) : '—'}
                color={
                  data.queue.ok && data.queue.oldest_queued_seconds > BACKLOG_AGE_THRESHOLD_SEC ? '#f59e0b' : '#e5e7eb'
                }
              />
              <Stat
                label="Sent 24h"
                value={data.queue.ok ? fmt(data.queue.sent_24h) : '—'}
                title="Per-recipient sent events — includes automated follow-up sends and other paths, so it can exceed send-batch recipients"
              />
            </div>
          </section>

          {/* ---- Capacity gauge + wave manager ---- */}
          <section
            style={{
              display: 'grid',
              gridTemplateColumns: 'repeat(auto-fit, minmax(330px, 1fr))',
              gap: 14,
              marginBottom: 14,
            }}
          >
            {/* Capacity */}
            <div style={panelStyle}>
              <h2 style={panelTitleStyle}>
                <FontAwesomeIcon icon={faGaugeHigh} style={{ color: '#818cf8' }} />
                Capacity
              </h2>
              {!data.throughput.ok ? (
                <div style={{ marginTop: 10 }}>
                  <SectionError label="Throughput" error={data.throughput.error} />
                </div>
              ) : (
                <>
                  <div style={{ display: 'flex', alignItems: 'baseline', gap: 8, marginTop: 12 }}>
                    <span style={{ fontSize: 30, fontWeight: 800, color: '#c7d2fe', fontVariantNumeric: 'tabular-nums' }}>
                      {fmtRate(data.throughput.current_rate_per_min)}
                    </span>
                    <span style={{ fontSize: 13, color: '#94a3b8' }}>
                      / {fmtRate(data.throughput.peak_rate_per_min_24h)} per min (24h peak)
                    </span>
                  </div>
                  <div
                    style={{
                      marginTop: 10,
                      height: 12,
                      borderRadius: 999,
                      background: 'rgba(148,163,184,0.15)',
                      overflow: 'hidden',
                    }}
                  >
                    <div
                      style={{
                        width: `${Math.min(100, Math.max(0, data.throughput.utilization_pct))}%`,
                        height: '100%',
                        borderRadius: 999,
                        background:
                          data.throughput.utilization_pct > 90
                            ? 'linear-gradient(90deg,#f59e0b,#ef4444)'
                            : 'linear-gradient(90deg,#6366f1,#22c55e)',
                        transition: 'width 600ms ease',
                      }}
                    />
                  </div>
                  <div style={{ display: 'flex', justifyContent: 'space-between', marginTop: 8, fontSize: 12, color: '#94a3b8' }}>
                    <span>{Math.round(data.throughput.utilization_pct)}% of 24h peak</span>
                    <span>
                      Queue clears in:{' '}
                      <span style={{ color: '#e5e7eb', fontWeight: 600 }}>
                        {drainEtaSec != null ? fmtDuration(drainEtaSec) : data.queue.ok && data.queue.queued > 0 ? '∞ (idle)' : '—'}
                      </span>
                    </span>
                  </div>
                  <div style={{ display: 'flex', gap: 24, marginTop: 14, flexWrap: 'wrap' }}>
                    <Stat label="Processing" value={data.queue.ok ? fmt(data.queue.processing) : '—'} color="#93c5fd" />
                    <Stat
                      label="Failed 24h"
                      value={data.queue.ok ? fmt(data.queue.failed_24h) : '—'}
                      color={data.queue.ok && data.queue.failed_24h > 0 ? '#f59e0b' : '#e5e7eb'}
                    />
                    <Stat
                      label="Failed sends 24h"
                      value={data.queue.ok ? fmt(data.queue.dead_letter_24h) : '—'}
                      color={data.queue.ok && data.queue.dead_letter_24h > 0 ? '#ef4444' : '#e5e7eb'}
                    />
                  </div>
                </>
              )}
            </div>

            {/* Send schedule, split by family. Older API payloads (pre-1.2)
                lack the split sections — fall back to the combined waves so a
                deploy skew never blanks the board. */}
            <SchedulePanel
              title="Broadcast Sends"
              data={data.waves_broadcast ?? data.waves}
              emptyLabel="No scheduled broadcast batches."
            />
            <SchedulePanel
              title="Data Partner Drips"
              data={data.waves_partner_drip}
              renderCampaign={(u) => <DripCampaignLabel name={u.campaign_name} campaignId={u.campaign_id} />}
              emptyLabel="No scheduled drip batches."
            />
          </section>

          {/* ---- Click funnels (waveless click-drip reminder path) ---- */}
          <section style={{ ...panelStyle, marginBottom: 14 }}>
            <h2 style={panelTitleStyle}>
              <FontAwesomeIcon icon={faLayerGroup} style={{ color: '#818cf8' }} />
              Click Funnels
              <span style={{ fontSize: 10, fontWeight: 500, color: '#64748b', letterSpacing: 0.4, textTransform: 'none' }}>
                reminder touches dispatch directly per enrollment — no batch schedule
              </span>
            </h2>
            {!data.click_funnels || !data.click_funnels.ok ? (
              <div style={{ marginTop: 10 }}>
                <SectionError label="Click funnels" error={data.click_funnels?.error} />
              </div>
            ) : (
              <>
                <div style={{ display: 'flex', gap: 22, marginTop: 12, flexWrap: 'wrap' }}>
                  <Stat label="Active enrollments" value={fmt(data.click_funnels.active_enrollments)} color="#c7d2fe" />
                  <Stat
                    label="Touches due now"
                    value={fmt(data.click_funnels.due_now)}
                    color={data.click_funnels.due_now > 0 ? '#f59e0b' : '#e5e7eb'}
                  />
                  <Stat label="Touches sent 24h" value={fmt(data.click_funnels.sent_24h)} />
                  <Stat
                    label="Opens 24h"
                    value={fmt(data.click_funnels.opens_24h)}
                    color="#22d3ee"
                    title="Raw open events on click-drip reminders (machine/MPP traffic included)"
                  />
                  <Stat
                    label="Clicks 24h"
                    value={fmt(data.click_funnels.clicks_24h)}
                    color="#a78bfa"
                    title="Raw click events on click-drip reminders (machine traffic included)"
                  />
                  <Stat
                    label="Conversions 24h"
                    value={fmt(data.click_funnels.converted_24h)}
                    color={data.click_funnels.converted_24h > 0 ? '#86efac' : '#e5e7eb'}
                    title="Enrollments exited as converted (Everflow postback) in the last 24h"
                  />
                </div>
                <div style={{ marginTop: 12 }}>
                  <div style={{ fontSize: 11, color: '#94a3b8', textTransform: 'uppercase', letterSpacing: 0.5, marginBottom: 6 }}>
                    Per offer funnel
                  </div>
                  {data.click_funnels.offers.length === 0 ? (
                    <div style={{ fontSize: 12, color: '#64748b', padding: '10px 0' }}>No click-funnel activity yet.</div>
                  ) : (
                    <table style={{ width: '100%', borderCollapse: 'collapse' }}>
                      <thead>
                        <tr>
                          <th style={thStyle}>Offer</th>
                          <th style={numTh}>Active</th>
                          <th style={numTh}>Due now</th>
                          <th style={numTh}>Sent 24h</th>
                          <th style={numTh}>Opens 24h</th>
                          <th style={numTh}>Clicks 24h</th>
                          <th style={numTh}>Conv 24h</th>
                        </tr>
                      </thead>
                      <tbody>
                        {data.click_funnels.offers.map((o) => (
                          <tr key={o.everflow_offer_id || o.offer_name}>
                            <td
                              style={{ ...tdStyle, fontWeight: 600, color: '#dbeafe', maxWidth: 260, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
                              title={`Everflow offer ${o.everflow_offer_id || '—'}`}
                            >
                              {o.offer_name || (o.everflow_offer_id ? `Offer ${o.everflow_offer_id}` : 'Unmapped')}
                            </td>
                            <td style={numTd}>{fmt(o.active_enrollments)}</td>
                            <td style={{ ...numTd, color: o.due_now > 0 ? '#f59e0b' : '#e5e7eb' }}>{fmt(o.due_now)}</td>
                            <td style={numTd}>{fmt(o.sent_24h)}</td>
                            <td style={{ ...numTd, color: '#22d3ee' }}>{fmt(o.opens_24h)}</td>
                            <td style={{ ...numTd, color: '#a78bfa' }}>{fmt(o.clicks_24h)}</td>
                            <td style={{ ...numTd, color: o.converted_24h > 0 ? '#86efac' : '#e5e7eb', fontWeight: 600 }}>
                              {fmt(o.converted_24h)}
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  )}
                </div>
              </>
            )}
          </section>

          {/* ---- Throughput chart ---- */}
          <section style={{ ...panelStyle, marginBottom: 14 }}>
            <h2 style={panelTitleStyle}>
              <FontAwesomeIcon icon={faWater} style={{ color: '#818cf8' }} />
              Throughput — last 60 minutes
            </h2>
            {!data.throughput.ok ? (
              <div style={{ marginTop: 10 }}>
                <SectionError label="Throughput" error={data.throughput.error} />
              </div>
            ) : chartData.length === 0 ? (
              <div style={{ fontSize: 12, color: '#64748b', padding: '24px 0', textAlign: 'center' }}>
                No send events in the last hour.
              </div>
            ) : (
              <div style={{ width: '100%', height: 240, marginTop: 8 }}>
                <ResponsiveContainer>
                  <ComposedChart data={chartData} margin={{ top: 8, right: 16, bottom: 0, left: 0 }}>
                    <defs>
                      <linearGradient id="sentFill" x1="0" y1="0" x2="0" y2="1">
                        <stop offset="0%" stopColor="#6366f1" stopOpacity={0.55} />
                        <stop offset="100%" stopColor="#6366f1" stopOpacity={0.05} />
                      </linearGradient>
                    </defs>
                    <CartesianGrid stroke="rgba(148,163,184,0.12)" vertical={false} />
                    <XAxis dataKey="label" tick={{ fill: '#94a3b8', fontSize: 11 }} interval="preserveStartEnd" minTickGap={40} />
                    <YAxis tick={{ fill: '#94a3b8', fontSize: 11 }} width={52} />
                    <Tooltip
                      contentStyle={{
                        background: '#0f172a',
                        border: '1px solid rgba(99,102,241,0.4)',
                        borderRadius: 8,
                        color: '#e5e7eb',
                        fontSize: 12,
                      }}
                      labelStyle={{ color: '#dbeafe' }}
                    />
                    <Legend wrapperStyle={{ fontSize: 12, color: '#94a3b8' }} />
                    <Area type="monotone" dataKey="sent" name="Sent /min" stroke="#818cf8" strokeWidth={2} fill="url(#sentFill)" />
                    <Line type="monotone" dataKey="deferred" name="Delayed /min" stroke="#f59e0b" strokeWidth={2} dot={false} />
                  </ComposedChart>
                </ResponsiveContainer>
              </div>
            )}
          </section>

          {/* ---- Per-ISP table + workers ---- */}
          <section
            style={{
              display: 'grid',
              gridTemplateColumns: 'minmax(0, 2fr) minmax(280px, 1fr)',
              gap: 14,
              marginBottom: 14,
            }}
          >
            <div style={panelStyle}>
              <h2 style={panelTitleStyle}>
                <FontAwesomeIcon icon={faWater} style={{ color: '#818cf8' }} />
                Per mailbox provider — queue, share, delays
                {data.storm.ok && data.storm.active && (
                  <span
                    style={{
                      marginLeft: 8,
                      background: 'rgba(239,68,68,0.15)',
                      border: '1px solid rgba(239,68,68,0.45)',
                      color: '#fca5a5',
                      fontSize: 10,
                      fontWeight: 700,
                      letterSpacing: 1,
                      padding: '2px 8px',
                      borderRadius: 999,
                    }}
                  >
                    HEAVY DELAYS
                  </span>
                )}
              </h2>
              {!data.storm.ok && !data.queue.ok ? (
                <div style={{ marginTop: 10 }}>
                  <SectionError label="Mailbox provider breakdown" error={data.storm.error || data.queue.error} />
                </div>
              ) : ispRows.length === 0 ? (
                <div style={{ fontSize: 12, color: '#64748b', padding: '16px 0' }}>No mailbox provider activity in the window.</div>
              ) : (
                <table style={{ width: '100%', borderCollapse: 'collapse', marginTop: 8 }}>
                  <thead>
                    <tr>
                      <th style={thStyle}>Mailbox Provider</th>
                      <th style={numTh}>Queued</th>
                      <th style={numTh}>Sent 24h</th>
                      <th style={numTh}>Share</th>
                      <th style={numTh}>Delays 60m</th>
                      <th style={numTh}>Baseline</th>
                      <th style={numTh}>× Base</th>
                    </tr>
                  </thead>
                  <tbody>
                    {ispRows.map((r) => {
                      const isOpen = expandedISP === r.isp;
                      return (
                        <React.Fragment key={r.isp}>
                          <tr
                            onClick={() => toggleISP(r.isp)}
                            title="Click to view per sending server (last 24h)"
                            style={{
                              cursor: 'pointer',
                              background: r.storm
                                ? 'rgba(239,68,68,0.10)'
                                : isOpen
                                  ? 'rgba(99,102,241,0.10)'
                                  : undefined,
                            }}
                          >
                            <td style={{ ...tdStyle, fontWeight: 600, color: r.storm ? '#fca5a5' : '#dbeafe' }}>
                              <FontAwesomeIcon
                                icon={isOpen ? faChevronDown : faChevronRight}
                                style={{ color: '#6366f1', fontSize: 11, marginRight: 7 }}
                              />
                              {titleCase(r.isp)}
                              {r.storm && (
                                <FontAwesomeIcon icon={faExclamationTriangle} style={{ color: '#ef4444', marginLeft: 6 }} title="Heavy delays" />
                              )}
                            </td>
                            <td style={numTd}>{fmt(r.queued)}</td>
                            <td style={numTd}>{fmt(r.sent24h)}</td>
                            <td style={numTd}>{r.sentShare > 0 ? `${(r.sentShare * 100).toFixed(1)}%` : '—'}</td>
                            <td style={{ ...numTd, color: r.storm ? '#ef4444' : r.recentRate > 0.05 ? '#f59e0b' : '#86efac', fontWeight: 600 }}>
                              {fmtPct(r.recentRate)}
                            </td>
                            <td style={numTd}>{fmtPct(r.baselineRate)}</td>
                            <td style={{ ...numTd, color: r.multiplier >= 2 ? '#f59e0b' : '#94a3b8' }}>
                              {r.multiplier > 0 ? `${r.multiplier.toFixed(1)}×` : '—'}
                            </td>
                          </tr>
                          {isOpen && (
                            <tr>
                              <td colSpan={7} style={{ padding: 0, background: 'rgba(0,0,0,0.22)' }}>
                                <PipesSubTable isp={r.isp} state={pipes[r.isp]} />
                              </td>
                            </tr>
                          )}
                        </React.Fragment>
                      );
                    })}
                  </tbody>
                </table>
              )}
            </div>

            {/* 24h performance scoreboard (replaced the worker list, which
                now has its own card grid below). */}
            <div style={panelStyle}>
              <h2 style={panelTitleStyle}>
                <FontAwesomeIcon icon={faHeartPulse} style={{ color: '#818cf8' }} />
                Performance — last 24h
              </h2>
              {!data.performance || !data.performance.ok ? (
                <div style={{ marginTop: 10 }}>
                  <SectionError label="Performance" error={data.performance?.error} />
                </div>
              ) : (
                <>
                  <div style={{ display: 'grid', gridTemplateColumns: 'repeat(2, minmax(0, 1fr))', gap: 14, marginTop: 12 }}>
                    <Stat
                      label="Sent 24h"
                      value={fmt(data.performance.sent_24h)}
                      color="#c7d2fe"
                      title="Per-recipient sent events across all send paths (broadcast, drips, click funnels)"
                    />
                    <Stat
                      label="Delivered 24h"
                      value={fmt(data.performance.delivered_24h)}
                      color="#86efac"
                      title="Delivered events recorded in tracking (campaign-attributed)"
                    />
                    <div>
                      <Stat
                        label="Open % (raw)"
                        value={
                          data.performance.delivered_24h > 0
                            ? fmtPct(data.performance.opens_raw_24h / data.performance.delivered_24h)
                            : '—'
                        }
                        color="#22d3ee"
                        title={`${fmt(data.performance.opens_raw_24h)} raw opens ÷ ${fmt(data.performance.delivered_24h)} delivered — machine/MPP traffic included`}
                      />
                      {data.performance.human_ok && (
                        <div style={{ fontSize: 11, color: '#94a3b8', marginTop: 2 }}>
                          human {fmt(data.performance.opens_human_24h)}
                        </div>
                      )}
                    </div>
                    <div>
                      <Stat
                        label="Click % (raw)"
                        value={
                          data.performance.delivered_24h > 0
                            ? fmtPct(data.performance.clicks_raw_24h / data.performance.delivered_24h)
                            : '—'
                        }
                        color="#a78bfa"
                        title={`${fmt(data.performance.clicks_raw_24h)} raw clicks ÷ ${fmt(data.performance.delivered_24h)} delivered — machine traffic included`}
                      />
                      {data.performance.human_ok && (
                        <div style={{ fontSize: 11, color: '#94a3b8', marginTop: 2 }}>
                          human {fmt(data.performance.clicks_human_24h)}
                        </div>
                      )}
                    </div>
                    <Stat
                      label="Complaints 24h"
                      value={fmt(data.performance.complaints_24h)}
                      color={data.performance.complaints_24h > 0 ? '#fb7185' : '#e5e7eb'}
                      title="Spam-complaint events recorded in tracking over the last 24h"
                    />
                    <Stat
                      label="Conversions 24h"
                      value={fmt(data.performance.conversions_24h)}
                      color={data.performance.conversions_24h > 0 ? '#86efac' : '#e5e7eb'}
                      title="Everflow conversion postbacks received in the last 24h"
                    />
                  </div>
                  <div style={{ fontSize: 11, color: '#64748b', marginTop: 12, lineHeight: 1.5 }}>
                    Rates divide by delivered. Raw opens/clicks include machine and Apple-MPP traffic;
                    "human" is the verdict-filtered count. Refreshes every ~5 minutes.
                  </div>
                </>
              )}
            </div>
          </section>

          {/* ---- Background workers — one card per worker ---- */}
          <section style={{ ...panelStyle, marginBottom: 14 }}>
            <h2 style={panelTitleStyle}>
              <FontAwesomeIcon icon={faHeartPulse} style={{ color: '#818cf8' }} />
              Background Workers
            </h2>
            {!data.workers.ok ? (
              <div style={{ fontSize: 12, color: '#64748b', marginTop: 10 }}>
                Worker status not available in this environment.
              </div>
            ) : data.workers.workers.length === 0 ? (
              <div style={{ fontSize: 12, color: '#64748b', marginTop: 10 }}>No heartbeats recorded yet.</div>
            ) : (
              <div
                style={{
                  display: 'grid',
                  gridTemplateColumns: 'repeat(auto-fill, minmax(220px, 1fr))',
                  gap: 10,
                  marginTop: 12,
                }}
              >
                {data.workers.workers.map((wkr) => {
                  const color = wkr.stalled ? '#ef4444' : wkr.last_status === 'error' ? '#f59e0b' : '#22c55e';
                  return (
                    <div
                      key={wkr.name}
                      style={{
                        padding: '10px 12px',
                        borderRadius: 8,
                        background: wkr.stalled ? 'rgba(239,68,68,0.10)' : 'rgba(99,102,241,0.06)',
                        border: `1px solid ${wkr.stalled ? 'rgba(239,68,68,0.4)' : 'rgba(99,102,241,0.15)'}`,
                      }}
                      title={wkr.last_error || undefined}
                    >
                      <div style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
                        <span style={{ width: 8, height: 8, borderRadius: '50%', background: color, flexShrink: 0 }} />
                        <span style={{ fontSize: 12, fontWeight: 600, color: '#e5e7eb', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                          {wkr.name}
                        </span>
                      </div>
                      <div
                        style={{
                          fontSize: 18,
                          fontWeight: 700,
                          fontVariantNumeric: 'tabular-nums',
                          color: wkr.stalled ? '#fca5a5' : '#e5e7eb',
                          marginTop: 6,
                        }}
                      >
                        {fmtDuration(wkr.seconds_since_beat)} ago
                      </div>
                      <div style={{ fontSize: 10, color: wkr.stalled ? '#fca5a5' : '#64748b', marginTop: 3 }}>
                        {wkr.stalled
                          ? 'NOT RESPONDING'
                          : wkr.expected_interval_seconds > 0
                            ? `expected every ${fmtDuration(wkr.expected_interval_seconds)}`
                            : 'healthy'}
                      </div>
                    </div>
                  );
                })}
              </div>
            )}
          </section>

          {/* ---- Dead-letter (preserved from v1) ---- */}
          <section style={{ ...panelStyle, padding: 0 }}>
            <header
              style={{
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'space-between',
                padding: '12px 16px',
                borderBottom: '1px solid rgba(99,102,241,0.15)',
              }}
            >
              <h2 style={panelTitleStyle}>
                <FontAwesomeIcon icon={faSkullCrossbones} style={{ color: recentDeadLetter.length > 0 ? '#ef4444' : '#64748b' }} />
                Failed Sends ({fmt(recentDeadLetter.length)})
              </h2>
              <span style={{ fontSize: 11, color: '#94a3b8' }}>
                Last {DEAD_LETTER_WINDOW_HOURS}h · newest first · max {DEAD_LETTER_MAX_ROWS}
              </span>
            </header>
            {recentDeadLetter.length === 0 ? (
              <div style={{ padding: 24, textAlign: 'center', color: '#64748b', fontSize: 13 }}>
                No failed sends in the last {DEAD_LETTER_WINDOW_HOURS} hours. Delivery healthy.
              </div>
            ) : (
              <div style={{ overflowX: 'auto' }}>
                <table style={{ width: '100%', borderCollapse: 'collapse' }}>
                  <thead>
                    <tr>
                      <th style={thStyle}>Email</th>
                      <th style={thStyle}>Status</th>
                      <th style={numTh}>Attempts</th>
                      <th style={thStyle}>Last error</th>
                      <th style={thStyle}>Last attempt</th>
                      <th style={thStyle}>Campaign</th>
                    </tr>
                  </thead>
                  <tbody>
                    {recentDeadLetter.map((r) => (
                      <tr key={r.id}>
                        <td style={tdStyle}>{r.email || r.subscriber_id}</td>
                        <td style={{ ...tdStyle, color: '#ef4444' }}>{r.status}</td>
                        <td style={numTd}>{r.attempts}</td>
                        <td
                          style={{ ...tdStyle, maxWidth: 380, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
                          title={r.last_error || ''}
                        >
                          {r.last_error || '—'}
                        </td>
                        <td style={tdStyle}>{r.last_attempt_at ? new Date(r.last_attempt_at).toLocaleString() : '—'}</td>
                        <td style={{ ...tdStyle, fontFamily: 'monospace', fontSize: 11 }}>
                          {r.campaign_id ? r.campaign_id.slice(0, 8) : '—'}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </section>
        </>
      ) : null}
    </div>
  );
};

export default OutboxDashboard;
