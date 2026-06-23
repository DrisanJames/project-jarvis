// Welcome Audience Health
//
// PAGE_VERSION 1.0 (2026-05-04) — initial. Surfaces the welcome-eligibility
// pool, the saturation exclusion segment, sends-remaining + days-remaining
// sunset buckets, daily burn rate, per-segment breakdown, and a projected
// next-list-upload date so the operator can answer "when do I need to upload
// more data?" at a glance.
//
// Backend: /api/mailing/analytics/welcome-audience-health
// (api_version 1.0, see internal/api/welcome_audience_health.go)
//
// Standing context (mirrors .cursor/rules/send-day-process.mdc §7):
//   - Saturation exclusion segment id 00000000-...-5a7ed8 is wired into every
//     Welcome campaign. Threshold = 8. Refresh script must run daily.
//   - This page is the operator's read-only window into that system. No
//     mutation here — for refresh / threshold changes, see the deploy script
//     and refresh_welcome_saturation_segment.py.

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faSpinner, faSyncAlt, faExclamationTriangle, faUsers,
  faShieldAlt, faSeedling, faClock, faChartLine, faUpload,
  faInfoCircle, faCheckCircle, faExclamationCircle,
} from '@fortawesome/free-solid-svg-icons';
import {
  ResponsiveContainer, BarChart, Bar, XAxis, YAxis, CartesianGrid,
  Tooltip as RechartsTooltip, LineChart, Line, Legend, Cell,
} from 'recharts';
import { apiFetch } from '../shared/apiFetch';

const PAGE_VERSION = '1.0';

// ─── Types ────────────────────────────────────────────────────────────────

interface SaturationSegment {
  id: string;
  name: string;
  threshold: number;
  members: number;
  last_calculated_at: string | null;
}

interface PoolComposition {
  total: number;
  exempt_opened: number;
  retired_saturated: number;
  mailable: number;
  never_mailed: number;
}

interface SunsetBucket {
  bucket: string;
  label: string;
  subscribers: number;
  prior_sends_min?: number;
  prior_sends_max?: number;
}

interface SegmentRow {
  segment_id: string;
  name: string;
  total: number;
  never_mailed: number;
  retired: number;
  exempt_opened: number;
  mailable: number;
  share_of_pool_pct: number;
}

interface DayBurn {
  date: string;
  never_mailed_picked: number;
  retirements: number;
}

interface DailyBurn {
  avg_never_mailed_picked_per_day: number;
  avg_retirements_per_day: number;
  by_day: DayBurn[];
}

interface Cadence {
  window_days: number;
  sends_total: number;
  active_subscribers: number;
  sends_per_active_sub_per_day: number;
}

interface Projection {
  never_mailed_remaining: number;
  trigger_threshold: number;
  burn_rate_per_day: number;
  days_until_upload_needed: number;
  upload_recommended_date: string;
}

interface WelcomeAudienceHealthData {
  api_version: string;
  as_of: string;
  saturation_segment: SaturationSegment;
  pool: PoolComposition;
  sends_remaining: SunsetBucket[];
  days_remaining: SunsetBucket[];
  by_segment: SegmentRow[];
  cadence: Cadence;
  daily_burn: DailyBurn;
  projection: Projection;
  pool_segment_ids: string[];
}

// ─── Style tokens (match Deliverability / Analytics dark theme) ──────────

const COLORS = {
  bgDeep:        '#0a0e1a',
  bgPanel:       '#0f1424',
  bgPanelAlt:    '#131a2e',
  border:        'rgba(255,255,255,0.06)',
  borderStrong:  'rgba(255,255,255,0.12)',
  textPrimary:   '#e2e8f0',
  textSecondary: '#94a3b8',
  textMuted:     '#64748b',
  accent:        '#818cf8',  // indigo-400
  accentAlt:     '#a78bfa',  // violet-400
  accentPink:    '#f472b6',
  good:          '#34d399',
  warn:          '#fbbf24',
  danger:        '#f87171',
  hardBounce:    '#ef4444',
  softBounce:    '#f59e0b',
};

const fmt = (n: number) => n.toLocaleString('en-US');
const fmtPct = (n: number) => `${n.toFixed(1)}%`;

// Color map for sends-remaining buckets — gradient from danger (about to retire)
// to good (long runway) to muted (already retired/exempt).
const SENDS_BUCKET_COLOR: Record<string, string> = {
  retired:        COLORS.textMuted,
  exempt_opened:  '#3b82f6',          // blue — they're in engager flow
  '1_send_away':  COLORS.danger,
  '2_to_3_away':  '#f97316',          // orange
  '4_to_5_away':  COLORS.warn,
  '6_to_8_away':  '#a3e635',          // lime
  never_mailed:   COLORS.good,
};

const DAYS_BUCKET_COLOR: Record<string, string> = {
  '0_to_1_days':  COLORS.danger,
  '2_to_3_days':  '#f97316',
  this_week:      COLORS.warn,
  next_week:      '#a3e635',
  this_month:     COLORS.good,
  long_runway:    COLORS.accent,
};

// ─── Helpers ──────────────────────────────────────────────────────────────

const urgencyFor = (daysUntil: number, mailableNeverMailed: number) => {
  if (daysUntil <= 7)  return { label: 'UPLOAD NOW',         color: COLORS.danger,  icon: faExclamationTriangle };
  if (daysUntil <= 14) return { label: 'PLAN UPLOAD',        color: COLORS.warn,    icon: faExclamationCircle };
  if (daysUntil <= 30) return { label: 'HEALTHY',            color: COLORS.good,    icon: faCheckCircle };
  if (mailableNeverMailed > 100000) return { label: 'ABUNDANT', color: COLORS.accent, icon: faCheckCircle };
  return { label: 'HEALTHY', color: COLORS.good, icon: faCheckCircle };
};

const formatRelativeDate = (iso: string): string => {
  if (!iso) return '—';
  const d = new Date(iso + 'T00:00:00Z');
  const now = new Date();
  const days = Math.round((d.getTime() - now.getTime()) / (1000 * 60 * 60 * 24));
  if (days <= 0) return 'today';
  if (days === 1) return 'tomorrow';
  if (days < 7)   return `${days} days from now`;
  return d.toLocaleDateString('en-US', { weekday: 'short', month: 'short', day: 'numeric' });
};

// ─── Main component ───────────────────────────────────────────────────────

export const WelcomeAudienceHealth: React.FC = () => {
  const [data, setData] = useState<WelcomeAudienceHealthData | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');

  const fetchData = useCallback(async () => {
    setLoading(true);
    try {
      const res = await apiFetch('/api/mailing/analytics/welcome-audience-health', {
        credentials: 'include',
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const json: WelcomeAudienceHealthData = await res.json();
      setData(json);
      setError('');
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { fetchData(); }, [fetchData]);

  const urgency = useMemo(() => {
    if (!data) return null;
    return urgencyFor(data.projection.days_until_upload_needed, data.projection.never_mailed_remaining);
  }, [data]);

  if (loading && !data) {
    return (
      <div style={styles.loadingShell}>
        <FontAwesomeIcon icon={faSpinner} spin /> Loading audience health…
      </div>
    );
  }

  if (error || !data) {
    return (
      <div style={styles.errorShell}>
        <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 8 }} />
        {error || 'No data available.'}
        <button style={styles.refreshBtn} onClick={fetchData}>Retry</button>
      </div>
    );
  }

  const { pool, sends_remaining, days_remaining, by_segment, daily_burn, projection, saturation_segment, cadence } = data;

  return (
    <div style={styles.page}>
      {/* ─── Header ──────────────────────────────────────────────────── */}
      <div style={styles.header}>
        <div>
          <h1 style={styles.title}>Welcome Audience Health</h1>
          <p style={styles.subtitle}>
            How many fresh subscribers are left, when retirement will start to bite, and when you need to upload more data.
          </p>
        </div>
        <button style={styles.refreshBtn} onClick={fetchData} disabled={loading}>
          <FontAwesomeIcon icon={loading ? faSpinner : faSyncAlt} spin={loading} /> Refresh
        </button>
      </div>

      {/* ─── Hero — when do I need to upload? ───────────────────────── */}
      <div style={{ ...styles.heroCard, borderColor: urgency?.color, boxShadow: `0 0 0 1px ${urgency?.color}33, 0 8px 32px rgba(0,0,0,0.4)` }}>
        <div style={styles.heroLeft}>
          <div style={{ ...styles.heroBadge, background: `${urgency?.color}22`, color: urgency?.color, borderColor: urgency?.color }}>
            <FontAwesomeIcon icon={urgency?.icon ?? faCheckCircle} /> {urgency?.label}
          </div>
          <div style={styles.heroNumber}>
            <span style={{ color: urgency?.color }}>{projection.days_until_upload_needed}</span>
            <span style={styles.heroNumberSuffix}>days</span>
          </div>
          <div style={styles.heroLabel}>until next list upload recommended</div>
          <div style={styles.heroSubLabel}>
            Recommended date: <strong style={{ color: COLORS.textPrimary }}>{projection.upload_recommended_date}</strong>
            <span style={{ color: COLORS.textMuted, marginLeft: 6 }}>({formatRelativeDate(projection.upload_recommended_date)})</span>
          </div>
        </div>
        <div style={styles.heroRight}>
          <HeroMetric
            label="Never-mailed remaining"
            value={fmt(projection.never_mailed_remaining)}
            sub={`${fmt(projection.trigger_threshold)} = upload trigger`}
            icon={faSeedling}
            color={COLORS.good}
          />
          <HeroMetric
            label="Daily burn (avg, last 14d)"
            value={`${fmt(projection.burn_rate_per_day)}/day`}
            sub={`${fmt(daily_burn.avg_retirements_per_day)} retire/day`}
            icon={faChartLine}
            color={COLORS.accent}
          />
          <HeroMetric
            label="Pool cadence"
            value={`${cadence.sends_per_active_sub_per_day.toFixed(2)}/day`}
            sub={`${fmt(cadence.active_subscribers)} active subs (14d)`}
            icon={faClock}
            color={COLORS.accentAlt}
          />
        </div>
      </div>

      {/* ─── Pool composition stat row ─────────────────────────────── */}
      <div style={styles.statRow}>
        <StatCard
          label="Total Pool"
          value={fmt(pool.total)}
          sub="Distinct confirmed subscribers across 6 inclusion segments"
          icon={faUsers}
          color={COLORS.textPrimary}
        />
        <StatCard
          label="Mailable"
          value={fmt(pool.mailable)}
          sub={`${fmtPct((pool.mailable / Math.max(pool.total, 1)) * 100)} of pool — neither retired nor opened`}
          icon={faSeedling}
          color={COLORS.good}
        />
        <StatCard
          label="Never-Mailed"
          value={fmt(pool.never_mailed)}
          sub={`${fmtPct((pool.never_mailed / Math.max(pool.total, 1)) * 100)} of pool — your reach runway`}
          icon={faSeedling}
          color={COLORS.accent}
        />
        <StatCard
          label={`Retired (>${saturation_segment.threshold} sends, no opens)`}
          value={fmt(saturation_segment.members)}
          sub="In saturation segment, excluded from every Welcome send"
          icon={faShieldAlt}
          color={COLORS.warn}
        />
        <StatCard
          label="Exempt (opened ≥1)"
          value={fmt(pool.exempt_opened)}
          sub="Not subject to saturation rule — handed off to ongoing engagement"
          icon={faCheckCircle}
          color="#3b82f6"
        />
      </div>

      {/* ─── Daily burn trend ───────────────────────────────────────── */}
      <div style={styles.panel}>
        <div style={styles.panelHeader}>
          <div>
            <h2 style={styles.panelTitle}>Daily Burn — Last 14 Days</h2>
            <p style={styles.panelSubtitle}>
              How many never-mailed subscribers we reached per day, and how many crossed the {saturation_segment.threshold + 1}-send sunset threshold.
            </p>
          </div>
        </div>
        <div style={{ height: 280 }}>
          <ResponsiveContainer>
            <LineChart data={daily_burn.by_day} margin={{ top: 10, right: 24, bottom: 8, left: 12 }}>
              <CartesianGrid stroke="rgba(255,255,255,0.04)" />
              <XAxis
                dataKey="date"
                stroke={COLORS.textMuted}
                tick={{ fill: COLORS.textMuted, fontSize: 11 }}
                tickFormatter={(s) => new Date(s).toLocaleDateString('en-US', { month: 'short', day: 'numeric' })}
              />
              <YAxis stroke={COLORS.textMuted} tick={{ fill: COLORS.textMuted, fontSize: 11 }} />
              <RechartsTooltip
                contentStyle={{ background: COLORS.bgDeep, border: `1px solid ${COLORS.borderStrong}`, borderRadius: 6, fontSize: 12 }}
                labelStyle={{ color: COLORS.textPrimary }}
                formatter={(v: number, key: string) => [fmt(v), key === 'never_mailed_picked' ? 'Never-mailed picked' : 'Retirements']}
              />
              <Legend wrapperStyle={{ fontSize: 12, color: COLORS.textSecondary }} />
              <Line
                type="monotone"
                dataKey="never_mailed_picked"
                name="Never-mailed picked"
                stroke={COLORS.good}
                strokeWidth={2}
                dot={{ r: 3, fill: COLORS.good }}
              />
              <Line
                type="monotone"
                dataKey="retirements"
                name="Retirements (crossed threshold)"
                stroke={COLORS.warn}
                strokeWidth={2}
                strokeDasharray="4 4"
                dot={{ r: 3, fill: COLORS.warn }}
              />
            </LineChart>
          </ResponsiveContainer>
        </div>
        <div style={styles.captionRow}>
          <FontAwesomeIcon icon={faInfoCircle} style={{ color: COLORS.textMuted }} />
          <span style={{ color: COLORS.textSecondary, fontSize: 12 }}>
            Burn-rate floor of 200/day used in projection calcs to avoid inflating the date on a quiet day.
          </span>
        </div>
      </div>

      {/* ─── Sunset buckets — sends + days side by side ─────────────── */}
      <div style={styles.twoColRow}>
        <div style={styles.panel}>
          <div style={styles.panelHeader}>
            <div>
              <h2 style={styles.panelTitle}>Sends Remaining Until Sunset</h2>
              <p style={styles.panelSubtitle}>
                Concrete — no cadence assumption. Subscribers retire after more than {saturation_segment.threshold} sends with no opens.
              </p>
            </div>
          </div>
          <BucketBar buckets={sends_remaining} colorMap={SENDS_BUCKET_COLOR} />
        </div>

        <div style={styles.panel}>
          <div style={styles.panelHeader}>
            <div>
              <h2 style={styles.panelTitle}>Days Remaining Until Sunset</h2>
              <p style={styles.panelSubtitle}>
                Translated using {cadence.sends_per_active_sub_per_day.toFixed(2)} sends/active-sub/day (14d window).
              </p>
            </div>
          </div>
          <BucketBar buckets={days_remaining} colorMap={DAYS_BUCKET_COLOR} />
        </div>
      </div>

      {/* ─── Per-segment breakdown ──────────────────────────────────── */}
      <div style={styles.panel}>
        <div style={styles.panelHeader}>
          <div>
            <h2 style={styles.panelTitle}>Inclusion Segments — Composition</h2>
            <p style={styles.panelSubtitle}>
              The six source segments that feed the four Welcome slots. "Never-mailed" is the freshness reservoir.
            </p>
          </div>
        </div>
        <div style={styles.tableWrap}>
          <table style={styles.table}>
            <thead>
              <tr>
                <th style={{ ...styles.th, textAlign: 'left' }}>Segment</th>
                <th style={styles.th}>Total</th>
                <th style={styles.th}>Mailable</th>
                <th style={styles.th}>Never-Mailed</th>
                <th style={styles.th}>Retired</th>
                <th style={styles.th}>Exempt (opened)</th>
                <th style={styles.th}>Share of Pool</th>
              </tr>
            </thead>
            <tbody>
              {by_segment.map((s) => (
                <tr key={s.segment_id} style={styles.tr}>
                  <td style={{ ...styles.td, textAlign: 'left', fontFamily: 'monospace', fontSize: 12 }}>
                    <div style={{ color: COLORS.textPrimary }}>{s.name}</div>
                    <div style={{ color: COLORS.textMuted, fontSize: 10 }}>{s.segment_id.slice(0, 13)}…</div>
                  </td>
                  <td style={styles.td}>{fmt(s.total)}</td>
                  <td style={{ ...styles.td, color: COLORS.good, fontWeight: 600 }}>{fmt(s.mailable)}</td>
                  <td style={{ ...styles.td, color: COLORS.accent, fontWeight: 600 }}>{fmt(s.never_mailed)}</td>
                  <td style={{ ...styles.td, color: COLORS.warn }}>{fmt(s.retired)}</td>
                  <td style={{ ...styles.td, color: COLORS.textSecondary }}>{fmt(s.exempt_opened)}</td>
                  <td style={styles.td}>{fmtPct(s.share_of_pool_pct)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>

      {/* ─── Saturation segment metadata strip ─────────────────────── */}
      <div style={styles.metaStrip}>
        <span><FontAwesomeIcon icon={faShieldAlt} /> {saturation_segment.name}</span>
        <span style={{ color: COLORS.textMuted }}>id: <span style={{ fontFamily: 'monospace' }}>{saturation_segment.id}</span></span>
        <span style={{ color: COLORS.textMuted }}>threshold: &gt; {saturation_segment.threshold} sends, never opened</span>
        <span style={{ color: COLORS.textMuted }}>
          last refreshed: {saturation_segment.last_calculated_at
            ? new Date(saturation_segment.last_calculated_at).toLocaleString('en-US', { hour12: false, dateStyle: 'short', timeStyle: 'short' })
            : 'never — refresh required'}
        </span>
      </div>

      {/* ─── Action card — what to do next ─────────────────────────── */}
      <div style={styles.actionCard}>
        <FontAwesomeIcon icon={faUpload} style={{ fontSize: 20, color: urgency?.color }} />
        <div style={{ flex: 1 }}>
          <div style={{ fontWeight: 600, color: COLORS.textPrimary, marginBottom: 4 }}>Recommended Action</div>
          <div style={{ color: COLORS.textSecondary, fontSize: 13 }}>
            {projection.days_until_upload_needed <= 7
              ? `Pool is depleted. Upload your next subscriber list this week — by ${projection.upload_recommended_date} at the latest.`
              : projection.days_until_upload_needed <= 14
              ? `Plan the next upload for the second half of next week (~${projection.upload_recommended_date}). The current pool will sustain mail through ${formatRelativeDate(projection.upload_recommended_date)}.`
              : projection.never_mailed_remaining > 100000
              ? `Pool is healthy — ${fmt(projection.never_mailed_remaining)} never-mailed subscribers remain. Next upload not needed before ${projection.upload_recommended_date} (${formatRelativeDate(projection.upload_recommended_date)}).`
              : `Pool runway is healthy. Continue monitoring — next upload trigger fires at ${fmt(projection.trigger_threshold)} never-mailed subscribers (~${projection.upload_recommended_date}).`}
          </div>
        </div>
      </div>

      {/* ─── Footer / version stripe ───────────────────────────────── */}
      <div style={styles.footer}>
        <span>Page: Welcome Audience Health v{PAGE_VERSION}</span>
        <span>API: welcome-audience-health v{data.api_version}</span>
        <span>As of: {new Date(data.as_of).toLocaleString('en-US', { hour12: false })}</span>
      </div>
    </div>
  );
};

// ─── Sub-components ──────────────────────────────────────────────────────

const HeroMetric: React.FC<{ label: string; value: string; sub: string; icon: any; color: string }> = ({
  label, value, sub, icon, color,
}) => (
  <div style={styles.heroMetric}>
    <FontAwesomeIcon icon={icon} style={{ color, fontSize: 16, marginBottom: 4 }} />
    <div style={{ fontSize: 11, color: COLORS.textMuted, textTransform: 'uppercase', letterSpacing: 0.5 }}>{label}</div>
    <div style={{ fontSize: 22, fontWeight: 700, color: COLORS.textPrimary, marginTop: 2 }}>{value}</div>
    <div style={{ fontSize: 11, color: COLORS.textSecondary, marginTop: 2 }}>{sub}</div>
  </div>
);

const StatCard: React.FC<{ label: string; value: string; sub: string; icon: any; color: string }> = ({
  label, value, sub, icon, color,
}) => (
  <div style={styles.statCard}>
    <div style={{ display: 'flex', alignItems: 'center', gap: 8, color: COLORS.textMuted, fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.5 }}>
      <FontAwesomeIcon icon={icon} style={{ color }} /> {label}
    </div>
    <div style={{ fontSize: 28, fontWeight: 700, color, marginTop: 6 }}>{value}</div>
    <div style={{ fontSize: 11, color: COLORS.textSecondary, marginTop: 4, lineHeight: 1.4 }}>{sub}</div>
  </div>
);

const BucketBar: React.FC<{ buckets: SunsetBucket[]; colorMap: Record<string, string> }> = ({ buckets, colorMap }) => {
  const data = buckets.map((b) => ({
    bucket: b.bucket,
    label: b.label,
    subscribers: b.subscribers,
    fill: colorMap[b.bucket] ?? COLORS.accent,
  }));
  return (
    <div style={{ height: Math.max(220, data.length * 36) }}>
      <ResponsiveContainer>
        <BarChart data={data} layout="vertical" margin={{ top: 6, right: 32, bottom: 6, left: 8 }}>
          <CartesianGrid stroke="rgba(255,255,255,0.04)" horizontal={false} />
          <XAxis type="number" stroke={COLORS.textMuted} tick={{ fill: COLORS.textMuted, fontSize: 11 }} tickFormatter={fmt} />
          <YAxis
            type="category"
            dataKey="label"
            stroke={COLORS.textMuted}
            tick={{ fill: COLORS.textSecondary, fontSize: 11 }}
            width={240}
          />
          <RechartsTooltip
            contentStyle={{ background: COLORS.bgDeep, border: `1px solid ${COLORS.borderStrong}`, borderRadius: 6, fontSize: 12 }}
            labelStyle={{ color: COLORS.textPrimary }}
            formatter={(v: number) => [fmt(v), 'Subscribers']}
          />
          <Bar dataKey="subscribers" radius={[0, 4, 4, 0]} barSize={20}>
            {data.map((d, i) => (
              <Cell key={i} fill={d.fill} />
            ))}
          </Bar>
        </BarChart>
      </ResponsiveContainer>
    </div>
  );
};

// ─── Styles ─────────────────────────────────────────────────────────────

const styles: Record<string, React.CSSProperties> = {
  page: {
    padding: '24px 28px 64px',
    background: COLORS.bgDeep,
    minHeight: '100vh',
    color: COLORS.textPrimary,
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif',
  },
  loadingShell: {
    display: 'flex', alignItems: 'center', justifyContent: 'center',
    height: '50vh', color: COLORS.textSecondary, gap: 10, fontSize: 14,
  },
  errorShell: {
    margin: 24, padding: 16, background: COLORS.bgPanel, borderRadius: 8,
    color: COLORS.danger, display: 'flex', alignItems: 'center', gap: 8,
  },
  header: {
    display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start',
    marginBottom: 24, gap: 16,
  },
  title: {
    margin: 0, fontSize: 24, fontWeight: 700, color: COLORS.textPrimary,
    letterSpacing: -0.3,
  },
  subtitle: {
    margin: '6px 0 0', fontSize: 13, color: COLORS.textSecondary, maxWidth: 720,
  },
  refreshBtn: {
    background: COLORS.bgPanel, color: COLORS.textPrimary,
    border: `1px solid ${COLORS.borderStrong}`, padding: '8px 16px',
    borderRadius: 6, fontSize: 13, cursor: 'pointer',
    display: 'flex', alignItems: 'center', gap: 8, height: 36,
  },
  heroCard: {
    display: 'flex', gap: 32,
    padding: 24, background: COLORS.bgPanel,
    border: `1px solid ${COLORS.border}`, borderRadius: 12,
    marginBottom: 20,
  },
  heroLeft: {
    flex: '0 0 320px', display: 'flex', flexDirection: 'column', gap: 4,
    paddingRight: 24, borderRight: `1px solid ${COLORS.border}`,
  },
  heroRight: {
    flex: 1, display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)',
    gap: 16,
  },
  heroBadge: {
    alignSelf: 'flex-start',
    padding: '4px 10px', borderRadius: 4, fontSize: 11, fontWeight: 700,
    letterSpacing: 0.5, border: '1px solid', display: 'inline-flex',
    alignItems: 'center', gap: 6, marginBottom: 8,
  },
  heroNumber: {
    fontSize: 64, fontWeight: 800, lineHeight: 1, display: 'flex',
    alignItems: 'baseline', gap: 8, marginTop: 8,
  },
  heroNumberSuffix: {
    fontSize: 16, color: COLORS.textMuted, fontWeight: 500,
  },
  heroLabel: {
    fontSize: 13, color: COLORS.textSecondary, marginTop: 8,
  },
  heroSubLabel: {
    fontSize: 12, color: COLORS.textMuted, marginTop: 8,
  },
  heroMetric: {
    display: 'flex', flexDirection: 'column',
    padding: '12px 16px', background: COLORS.bgPanelAlt,
    border: `1px solid ${COLORS.border}`, borderRadius: 8,
  },
  statRow: {
    display: 'grid', gridTemplateColumns: 'repeat(5, 1fr)',
    gap: 12, marginBottom: 20,
  },
  statCard: {
    padding: 16, background: COLORS.bgPanel,
    border: `1px solid ${COLORS.border}`, borderRadius: 8,
  },
  panel: {
    background: COLORS.bgPanel,
    border: `1px solid ${COLORS.border}`, borderRadius: 8,
    padding: 20, marginBottom: 20,
  },
  panelHeader: {
    display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start',
    marginBottom: 12, gap: 16,
  },
  panelTitle: {
    margin: 0, fontSize: 16, fontWeight: 600, color: COLORS.textPrimary,
  },
  panelSubtitle: {
    margin: '4px 0 0', fontSize: 12, color: COLORS.textSecondary, maxWidth: 720,
  },
  twoColRow: {
    display: 'grid', gridTemplateColumns: 'repeat(2, 1fr)', gap: 16,
    marginBottom: 0,
  },
  captionRow: {
    display: 'flex', alignItems: 'center', gap: 6, marginTop: 8,
  },
  tableWrap: {
    overflowX: 'auto',
  },
  table: {
    width: '100%', borderCollapse: 'collapse', fontSize: 13,
  },
  th: {
    padding: '8px 12px', textAlign: 'right', color: COLORS.textMuted,
    fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.5,
    borderBottom: `1px solid ${COLORS.borderStrong}`, fontWeight: 600,
  },
  tr: { borderBottom: `1px solid ${COLORS.border}` },
  td: {
    padding: '10px 12px', textAlign: 'right', color: COLORS.textPrimary,
    fontVariantNumeric: 'tabular-nums',
  },
  metaStrip: {
    display: 'flex', flexWrap: 'wrap', gap: 16, alignItems: 'center',
    padding: '10px 16px', background: COLORS.bgPanelAlt,
    border: `1px solid ${COLORS.border}`, borderRadius: 6,
    fontSize: 12, color: COLORS.textSecondary, marginBottom: 16,
  },
  actionCard: {
    display: 'flex', gap: 16, alignItems: 'flex-start',
    padding: 16, background: COLORS.bgPanel,
    border: `1px solid ${COLORS.borderStrong}`, borderRadius: 8,
    marginBottom: 32,
  },
  footer: {
    marginTop: 32, padding: '8px 16px',
    fontSize: 11, color: COLORS.textMuted,
    borderTop: `1px solid ${COLORS.border}`,
    display: 'flex', gap: 16, flexWrap: 'wrap',
  },
};
