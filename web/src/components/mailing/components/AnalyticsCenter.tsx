import React, { useState, useEffect, useCallback, useMemo } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faChartLine, faEnvelope, faEye, faMousePointer,
  faExclamationTriangle, faBan, faDollarSign,
  faShieldAlt,
  faSyncAlt, faSpinner,
  faDatabase,
  faTrophy,
  faClock, faUserSlash, faBolt, faBroadcastTower,
  faChevronDown, faChevronRight, faLayerGroup,
  faFilter, faInfoCircle,
} from '@fortawesome/free-solid-svg-icons';
import {
  LineChart, Line, XAxis, YAxis, CartesianGrid,
  Tooltip as RechartsTooltip, Legend, ResponsiveContainer,
} from 'recharts';
import { useAuth } from '../../../contexts/AuthContext';

import { AnimatedCounter } from '../shared/AnimatedCounter';
import './AnalyticsCenter.css';

// ─── Types ───────────────────────────────────────────────────────────────────

interface OverviewData {
  totals: {
    sent: number; delivered: number; opens: number; clicks: number;
    hard_bounces: number; soft_bounces: number;
    complaints: number; revenue: number;
    deferred?: number; unsubscribes?: number;
  };
  rates: { open_rate: number; click_rate: number; hard_bounce_rate: number; soft_bounce_rate: number; complaint_rate: number; delivery_rate?: number; unsubscribe_rate?: number; deferral_rate?: number; bounce_rate?: number };
  daily_trend: { date: string; sent: number; delivered: number; opens: number; clicks: number; hard_bounces: number; soft_bounces: number; complaints: number; deferred: number; unsubscribes: number }[];
  granularity: string;
  range: { start: string; end: string };
  sending_domains?: string[];
  api_version?: string;
}


interface CampaignRow {
  id: string; name: string; status: string;
  sent: number; delivered?: number; opens: number; clicks: number;
  hard_bounces: number; soft_bounces: number;
  deferred?: number; unsubscribes?: number; complaints?: number;
  revenue: number;
  open_rate: number; click_rate: number;
  hard_bounce_rate: number; soft_bounce_rate: number;
  created_at: string;
}

interface CampaignData {
  campaigns: CampaignRow[];
}

// ─── Live Sending types ──────────────────────────────────────────────────────

interface LiveWaveCounts {
  total: number;
  planned: number;
  enqueued: number;
  running: number;
  completed: number;
  cancelled: number;
  failed: number;
}

interface LiveTotals {
  recipients: number; sent: number; delivered: number;
  deferred: number; hard_bounces: number; soft_bounces: number;
  opens: number; clicks: number;
}

interface LiveISPPlan {
  isp: string; label: string; status: string;
  sending_domain: string;
  waves: LiveWaveCounts;
  totals: LiveTotals;
  sent_last_1m: number;
  sent_last_5m: number;
}

interface LiveCampaign {
  id: string; name: string; status: string;
  started_at?: string; scheduled_at?: string;
  from_email: string;
  sent_last_1m: number; sent_last_5m: number;
  totals: LiveTotals;
  waves: LiveWaveCounts;
  next_wave_at?: string;
  isp_plans: LiveISPPlan[];
}

interface ActiveSendsData {
  api_version?: string;
  is_active: boolean;
  as_of: string;
  campaigns: LiveCampaign[];
}

// ─── Bounce reasons types ────────────────────────────────────────────────────

interface BounceReasonISP { isp: string; label: string; count: number; }

interface BounceReasonRow {
  code: string;
  label: string;
  category: string;
  count: number;
  pct_of_total: number;
  by_isp: BounceReasonISP[];
}

interface BounceReasonsData {
  api_version?: string;
  total_bounces: number;
  reasons: BounceReasonRow[];
}


interface InfraRow {
  entity: string;
  sent: number;
  delivered: number;
  opens: number;
  clicks: number;
  hard_bounces: number;
  soft_bounces: number;
  complaints: number;
  deferred: number;
  open_rate: number;
  click_rate: number;
  hard_bounce_rate: number;
  soft_bounce_rate: number;
  complaint_rate: number;
  deferral_rate: number;
  parent_sent?: number;
  parent_delivered?: number;
}

interface ISPData {
  isp: string; label: string;
  sent: number; delivered: number; opens: number; clicks: number;
  hard_bounces: number; soft_bounces: number; complaints: number;
  deferred?: number; unsubscribes?: number;
  open_rate: number; click_rate: number; hard_bounce_rate: number; soft_bounce_rate: number; complaint_rate: number;
  delivery_rate?: number; deferral_rate?: number; unsubscribe_rate?: number;
}

const BOUNCE_CATEGORY_COLORS: Record<string, string> = {
  auth: '#ef4444',
  mailbox: '#f59e0b',
  reputation: '#dc2626',
  policy: '#a855f7',
  system: '#6366f1',
  transient: '#3b82f6',
  other: '#64748b',
};

// Brand keywords we recognise in campaign names. Extend this list as new
// brands are onboarded — campaigns whose names don't match any of these fall
// into "Unclassified".
const KNOWN_BRANDS = [
  'Discount Blog',
  'Quiz Fiesta',
  'History Thinking',
  'My Own Health',
] as const;

// Series keywords. Matched case-insensitively.
const KNOWN_SERIES = ['Welcome', 'Engager', 'Acquisition', 'Reactivation'] as const;

function classifyCampaign(name: string): { brand: string; series: string } {
  const lower = name.toLowerCase();
  const brand = KNOWN_BRANDS.find(b => lower.includes(b.toLowerCase())) || 'Unclassified';
  const series = KNOWN_SERIES.find(s => lower.includes(s.toLowerCase())) || 'Other';
  return { brand, series };
}

const ISP_LABELS: Record<string, string> = {
  gmail: 'Gmail', yahoo: 'Yahoo', microsoft: 'Microsoft',
  apple: 'Apple iCloud', comcast: 'Comcast', att: 'AT&T',
  cox: 'Cox', charter: 'Charter/Spectrum', other: 'Other',
};

const ISP_COLORS: Record<string, string> = {
  gmail: '#EA4335', yahoo: '#6001D2', microsoft: '#00A4EF',
  apple: '#A2AAAD', comcast: '#ED1C24', att: '#009FDB',
  cox: '#0070C0', charter: '#0078D4', other: '#6B7280',
};

type TimeRange = 'today' | 'yesterday';

const PAGE_VERSION = '4.3';

// ─── ISP Insights types ──────────────────────────────────────────────────────

// Daily bucket as returned by /analytics/isp-sending-insights for a given
// sending domain + ISP pair. Dates are MST calendar days (backend casts
// event_at AT TIME ZONE 'America/Denver').
interface InsDailyRow {
  date: string;
  sent: number;
  delivered: number;
  hard_bounces: number;
  soft_bounces: number;
  deferred: number;
  opens: number;
  clicks: number;
  bounce_rate?: number;
}

// Pre-computed totals per sending domain across the selected window.
interface InsDomainTotals {
  sent: number;
  delivered: number;
  opens: number;
  clicks: number;
  hard_bounces: number;
  soft_bounces: number;
  open_rate: number;  // opens / delivered (falls back to sent)
  click_rate: number; // clicks / delivered (falls back to sent)
  inbox_rate: number; // delivered / sent
  hard_bounce_rate: number; // hard / sent
  soft_bounce_rate: number; // soft / sent
}

interface InsDomainSeries {
  sending_domain: string;
  daily: InsDailyRow[];
  totals: InsDomainTotals;
}

interface InsTopCampaign {
  campaign_id: string;
  name: string;
  sending_domain: string;
  sent: number;
  delivered: number;
  opens: number;
  clicks: number;
  hard_bounces: number;
  soft_bounces: number;
  open_rate: number;
  click_rate: number;
  hard_bounce_rate: number;
  soft_bounce_rate: number;
}

// Metric the user can plot on the daily trend chart. The backend
// `by_sending_domain` block provides opens and clicks per-day now (v1.4+),
// so all of these are chartable.
type InsMetric =
  | 'delivered'
  | 'sent'
  | 'hard_bounces'
  | 'soft_bounces'
  | 'deferred'
  | 'opens'
  | 'clicks';

// Window lengths the panel supports. Must match parseISPInsightsDays on the
// backend (whitelisted to 3/7/14).
type InsWindow = 3 | 7 | 14;

const INS_METRIC_LABELS: Record<InsMetric, string> = {
  delivered: 'Delivered',
  sent: 'Sent',
  hard_bounces: 'Hard Bounces',
  soft_bounces: 'Soft Bounces',
  deferred: 'Deferred',
  opens: 'Opens',
  clicks: 'Clicks',
};

// Deterministic color for a sending domain. The ISP palette is reserved for
// the global ISP cards, so sending-domain lines use a separate set.
const SENDING_DOMAIN_COLORS = [
  '#00e5ff', '#a855f7', '#f59e0b', '#22c55e',
  '#f472b6', '#60a5fa', '#fbbf24', '#10b981',
];
function colorForDomain(domain: string): string {
  let hash = 0;
  for (let i = 0; i < domain.length; i++) {
    hash = (hash * 31 + domain.charCodeAt(i)) >>> 0;
  }
  return SENDING_DOMAIN_COLORS[hash % SENDING_DOMAIN_COLORS.length];
}

// Kept only so the existing render path can fall back if the backend
// response predates v1.4. Primary path uses the `by_sending_domain` block
// which ships totals directly.
function computeDomainTotals(daily: InsDailyRow[], opens: number, clicks: number): InsDomainTotals {
  const sum = (key: keyof InsDailyRow) => daily.reduce((acc, r) => acc + (Number(r[key]) || 0), 0);
  const sent = sum('sent');
  const delivered = sum('delivered');
  const hard = sum('hard_bounces');
  const soft = sum('soft_bounces');
  const base = delivered || sent || 1;
  const sentBase = sent || 1;
  return {
    sent, delivered, opens, clicks,
    hard_bounces: hard,
    soft_bounces: soft,
    open_rate: (opens / base) * 100,
    click_rate: (clicks / base) * 100,
    inbox_rate: sent > 0 ? (delivered / sent) * 100 : 0,
    hard_bounce_rate: (hard / sentBase) * 100,
    soft_bounce_rate: (soft / sentBase) * 100,
  };
}

// Master List Migration P6: per-domain SDS audience health row.
// Mirrors the response shape from GET /api/mailing/analytics/sds-audience-health.
interface SDSDomainRow {
  sending_domain: string;
  total: number;
  cold: number;
  warming: number;
  engaged: number;
  dormant: number;
  unsubscribed: number;
  hard_bounced: number;
  complained: number;
  recently_mailed_7d: number;
  recently_opened_7d: number;
}

// All date boundaries are anchored to America/Denver (MST/MDT).
// MST = UTC-7, MDT = UTC-6. We use a fixed UTC-7 offset per user spec.
const MST_OFFSET_MS = 7 * 60 * 60 * 1000;

function mstMidnight(d: Date): Date {
  const mstDate = new Date(d.getTime() - MST_OFFSET_MS);
  return new Date(Date.UTC(mstDate.getUTCFullYear(), mstDate.getUTCMonth(), mstDate.getUTCDate()) + MST_OFFSET_MS);
}

function computeDateRange(range: TimeRange): { startDate: string; endDate: string } {
  const now = new Date();
  const todayStart = mstMidnight(now);
  if (range === 'yesterday') {
    const yesterdayStart = new Date(todayStart.getTime() - 86_400_000);
    return { startDate: yesterdayStart.toISOString(), endDate: todayStart.toISOString() };
  }
  return { startDate: todayStart.toISOString(), endDate: now.toISOString() };
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

const orgFetch = async (url: string, orgId?: string) => {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  if (orgId) headers['X-Organization-ID'] = orgId;
  return fetch(url, { headers });
};

const fmt = (n: number): string => {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M';
  if (n >= 1_000) return (n / 1_000).toFixed(1) + 'K';
  return n.toLocaleString();
};

const fmtCurrency = (n: number): string => {
  if (n >= 1_000_000) return '$' + (n / 1_000_000).toFixed(2) + 'M';
  if (n >= 1_000) return '$' + (n / 1_000).toFixed(1) + 'K';
  return '$' + n.toFixed(2);
};

const pct = (n: number | undefined | null): string => (n == null || isNaN(n)) ? '0.0%' : n.toFixed(1) + '%';

// Relative "in Xm Ys" for a future ISO timestamp. Returns "—" if already past.
const relativeFuture = (iso?: string): string => {
  if (!iso) return '—';
  const ms = new Date(iso).getTime() - Date.now();
  if (isNaN(ms) || ms <= 0) return 'due';
  const totalSec = Math.floor(ms / 1000);
  const mins = Math.floor(totalSec / 60);
  const secs = totalSec % 60;
  if (mins >= 60) {
    const hours = Math.floor(mins / 60);
    const rem = mins % 60;
    return `${hours}h ${rem}m`;
  }
  if (mins > 0) return `${mins}m ${secs}s`;
  return `${secs}s`;
};

// ─── Live Sending Band ─────────────────────────────────────────────────────

interface LiveSendingBandProps {
  data: ActiveSendsData | null;
  expanded: string | null;
  onToggle: (id: string) => void;
  onRefresh: () => void;
}

const LiveSendingBand: React.FC<LiveSendingBandProps> = ({ data, expanded, onToggle, onRefresh }) => {
  if (!data || !data.campaigns || data.campaigns.length === 0) return null;

  return (
    <div className="ac-card ig-card-hover ac-live-band" style={{ marginBottom: 16 }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 }}>
        <h3 style={{ margin: 0, display: 'flex', alignItems: 'center', gap: 10 }}>
          <span className="ac-live-dot" />
          <FontAwesomeIcon icon={faBroadcastTower} />
          Live Sending
          <span style={{ fontSize: '0.7em', color: '#64748b', fontWeight: 'normal', marginLeft: 8 }}>
            {data.campaigns.length} active · as of {new Date(data.as_of).toLocaleTimeString()}
          </span>
        </h3>
        <button className="ac-btn-refresh" onClick={onRefresh} title="Refresh now">
          <FontAwesomeIcon icon={faSyncAlt} />
        </button>
      </div>

      <div className="ac-live-cards">
        {data.campaigns.map(c => {
          const wavePct = c.waves.total > 0 ? Math.round((c.waves.completed / c.waves.total) * 100) : 0;
          const isExpanded = expanded === c.id;
          return (
            <div key={c.id} className="ac-live-card">
              <div className="ac-live-row1" onClick={() => onToggle(c.id)}>
                <span className={`ac-live-status ac-live-status-${c.status}`}>{c.status}</span>
                <span className="ac-live-name">{c.name}</span>
                <span className="ac-live-throughput">
                  <FontAwesomeIcon icon={faBolt} /> {fmt(c.sent_last_1m)}/min · {fmt(c.sent_last_5m)}/5m
                </span>
                <span className="ac-live-chevron">
                  <FontAwesomeIcon icon={isExpanded ? faChevronDown : faChevronRight} />
                </span>
              </div>

              <div className="ac-live-row2">
                <div className="ac-live-progress">
                  <div className="ac-live-progress-bar" style={{ width: `${wavePct}%` }} />
                </div>
                <div className="ac-live-wave-badges">
                  <span className="ac-live-badge" title="Completed">
                    {c.waves.completed}/{c.waves.total} waves
                  </span>
                  {c.waves.running > 0 && <span className="ac-live-badge ac-live-badge-running">{c.waves.running} running</span>}
                  {c.waves.enqueued > 0 && <span className="ac-live-badge ac-live-badge-enqueued">{c.waves.enqueued} enqueued</span>}
                  {c.waves.planned > 0 && <span className="ac-live-badge ac-live-badge-planned">{c.waves.planned} planned</span>}
                  {c.waves.cancelled > 0 && <span className="ac-live-badge ac-live-badge-cancelled">{c.waves.cancelled} cancelled</span>}
                  {c.waves.failed > 0 && <span className="ac-live-badge ac-live-badge-failed">{c.waves.failed} failed</span>}
                </div>
                <div className="ac-live-next-wave">
                  <FontAwesomeIcon icon={faClock} /> next wave: <strong>{relativeFuture(c.next_wave_at)}</strong>
                </div>
              </div>

              {isExpanded && c.isp_plans && c.isp_plans.length > 0 && (
                <div className="ac-live-isp-rows">
                  <div className="ac-live-isp-head">
                    <span>ISP</span>
                    <span>Status</span>
                    <span>Waves</span>
                    <span>1m</span>
                    <span>5m</span>
                    <span>Domain</span>
                  </div>
                  {c.isp_plans.map(p => (
                    <div key={`${c.id}-${p.isp}`} className="ac-live-isp-row">
                      <span className="ac-live-isp-label">{p.label || p.isp}</span>
                      <span className={`ac-live-isp-status ac-live-status-${p.status}`}>{p.status}</span>
                      <span>{p.waves.completed}/{p.waves.total}</span>
                      <span>{fmt(p.sent_last_1m)}</span>
                      <span>{fmt(p.sent_last_5m)}</span>
                      <span className="ac-live-isp-domain">{p.sending_domain || '—'}</span>
                    </div>
                  ))}
                </div>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
};

// ─── Bounce Reasons Panel ──────────────────────────────────────────────────

interface BounceReasonsPanelProps {
  data: BounceReasonsData | null;
  expandedCode: string | null;
  onToggle: (key: string) => void;
}

const BounceReasonsPanel: React.FC<BounceReasonsPanelProps> = ({ data, expandedCode, onToggle }) => {
  if (!data || !data.reasons || data.reasons.length === 0) {
    return (
      <div className="ac-card ig-card-hover">
        <h3><FontAwesomeIcon icon={faExclamationTriangle} /> Top Bounce Reasons</h3>
        <div className="ac-empty-mini">No bounce events in this range. Nice.</div>
      </div>
    );
  }

  return (
    <div className="ac-card ig-card-hover">
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 }}>
        <h3 style={{ margin: 0 }}>
          <FontAwesomeIcon icon={faExclamationTriangle} /> Top Bounce Reasons
        </h3>
        <span style={{ fontSize: '0.75em', color: '#64748b' }}>{fmt(data.total_bounces)} total bounces</span>
      </div>

      <div className="ac-bounce-list">
        {data.reasons.map((r, i) => {
          const key = `${r.label}-${i}`;
          const isExpanded = expandedCode === key;
          return (
            <div key={key} className="ac-bounce-item">
              <div className="ac-bounce-head" onClick={() => onToggle(key)}>
                <span className="ac-bounce-cat-pill" style={{ background: BOUNCE_CATEGORY_COLORS[r.category] || '#64748b' }}>
                  {r.category}
                </span>
                <span className="ac-bounce-code">{r.code || '—'}</span>
                <span className="ac-bounce-label">{r.label}</span>
                <span className="ac-bounce-count">{fmt(r.count)}</span>
                <span className="ac-bounce-pct">{r.pct_of_total.toFixed(1)}%</span>
                <span className="ac-live-chevron">
                  <FontAwesomeIcon icon={isExpanded ? faChevronDown : faChevronRight} />
                </span>
              </div>
              {isExpanded && r.by_isp && r.by_isp.length > 0 && (
                <div className="ac-bounce-isp-grid">
                  {r.by_isp.map(b => (
                    <div key={`${key}-${b.isp}`} className="ac-bounce-isp-cell">
                      <span>{b.label || b.isp}</span>
                      <strong>{fmt(b.count)}</strong>
                    </div>
                  ))}
                </div>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
};

// ─── Brand × Series Rollup ─────────────────────────────────────────────────

interface BrandSeriesRollupRow {
  brand: string; series: string; campaigns: number;
  sent: number; delivered: number; deferred: number; unsubscribes: number;
  open_rate: number; click_rate: number;
  hard_bounce_rate: number; soft_bounce_rate: number; deferral_rate: number; unsubscribe_rate: number;
  revenue: number;
}

const BrandSeriesRollup: React.FC<{ rows: BrandSeriesRollupRow[] }> = ({ rows }) => {
  if (!rows || rows.length === 0) {
    return null;
  }
  return (
    <div className="ac-card ig-card-hover">
      <h3><FontAwesomeIcon icon={faLayerGroup} /> Brand × Series Rollup</h3>
      <div className="ac-table-wrap">
        <table className="ac-table">
          <thead>
            <tr>
              <th>Brand</th>
              <th>Series</th>
              <th>Campaigns</th>
              <th>Sent</th>
              <th>Delivered</th>
              <th>Open %</th>
              <th>Click %</th>
              <th>Hard %</th>
              <th>Soft %</th>
              <th>Deferred</th>
              <th>Unsubs</th>
              <th>Revenue</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r, i) => (
              <tr key={`${r.brand}-${r.series}-${i}`}>
                <td><strong>{r.brand}</strong></td>
                <td>{r.series}</td>
                <td>{r.campaigns}</td>
                <td>{fmt(r.sent)}</td>
                <td>{fmt(r.delivered)}</td>
                <td className={r.open_rate > 20 ? 'ac-good' : r.open_rate > 10 ? 'ac-ok' : 'ac-bad'}>{pct(r.open_rate)}</td>
                <td className={r.click_rate > 3 ? 'ac-good' : r.click_rate > 1 ? 'ac-ok' : 'ac-bad'}>{pct(r.click_rate)}</td>
                <td className={r.hard_bounce_rate < 2 ? 'ac-good' : r.hard_bounce_rate < 5 ? 'ac-ok' : 'ac-bad'}>{pct(r.hard_bounce_rate)}</td>
                <td className={r.soft_bounce_rate < 2 ? 'ac-good' : r.soft_bounce_rate < 5 ? 'ac-ok' : 'ac-bad'}>{pct(r.soft_bounce_rate)}</td>
                <td>{fmt(r.deferred)}</td>
                <td>{fmt(r.unsubscribes)}</td>
                <td>{fmtCurrency(r.revenue)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
};

// ─── ISP Funnel and Tiles (inside selected-ISP detail) ─────────────────────

const ISPFunnelAndTiles: React.FC<{ card: ISPData | null }> = ({ card }) => {
  if (!card) return null;
  const sent = card.sent || 0;
  const delivered = card.delivered || 0;
  const opens = card.opens || 0;
  const clicks = card.clicks || 0;
  const base = Math.max(sent, 1);
  const steps = [
    { label: 'Sent', value: sent, color: '#a5b4fc' },
    { label: 'Delivered', value: delivered, color: '#22c55e' },
    { label: 'Opens', value: opens, color: '#3b82f6' },
    { label: 'Clicks', value: clicks, color: '#f59e0b' },
  ];
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 12, marginBottom: 12 }}>
      <div className="ac-funnel">
        {steps.map(s => (
          <div key={s.label} className="ac-funnel-step">
            <div className="ac-funnel-meta">
              <span className="ac-funnel-label">{s.label}</span>
              <span className="ac-funnel-value">{fmt(s.value)}</span>
            </div>
            <div className="ac-funnel-bar-track">
              <div className="ac-funnel-bar-fill" style={{ width: `${(s.value / base) * 100}%`, background: s.color }} />
            </div>
          </div>
        ))}
      </div>
      <div className="ac-isp-tile-grid">
        <div className="ac-isp-tile">
          <span className="ac-isp-tile-label">Delivery</span>
          <span className="ac-isp-tile-value" style={{ color: '#22c55e' }}>{pct(card.delivery_rate || 0)}</span>
          <span className="ac-isp-tile-sub">{fmt(delivered)}</span>
        </div>
        <div className="ac-isp-tile">
          <span className="ac-isp-tile-label">Deferred</span>
          <span className="ac-isp-tile-value" style={{ color: '#3b82f6' }}>{pct(card.deferral_rate || 0)}</span>
          <span className="ac-isp-tile-sub">{fmt(card.deferred || 0)}</span>
        </div>
        <div className="ac-isp-tile">
          <span className="ac-isp-tile-label">Hard Bounce</span>
          <span className="ac-isp-tile-value" style={{ color: '#ef4444' }}>{pct(card.hard_bounce_rate)}</span>
          <span className="ac-isp-tile-sub">{fmt(card.hard_bounces)}</span>
        </div>
        <div className="ac-isp-tile">
          <span className="ac-isp-tile-label">Soft Bounce</span>
          <span className="ac-isp-tile-value" style={{ color: '#f59e0b' }}>{pct(card.soft_bounce_rate)}</span>
          <span className="ac-isp-tile-sub">{fmt(card.soft_bounces)}</span>
        </div>
        <div className="ac-isp-tile">
          <span className="ac-isp-tile-label">Unsubscribes</span>
          <span className="ac-isp-tile-value" style={{ color: '#a855f7' }}>{pct(card.unsubscribe_rate || 0)}</span>
          <span className="ac-isp-tile-sub">{fmt(card.unsubscribes || 0)}</span>
        </div>
        <div className="ac-isp-tile">
          <span className="ac-isp-tile-label">Complaints</span>
          <span className="ac-isp-tile-value" style={{ color: '#dc2626' }}>{(card.complaint_rate || 0).toFixed(2)}%</span>
          <span className="ac-isp-tile-sub">{fmt(card.complaints)}</span>
        </div>
      </div>
    </div>
  );
};

// ─── ISP Insights Panel ─────────────────────────────────────────────────────

interface ISPInsightsPanelProps {
  orgId: string;
  sendingDomains: string[];
  onApiVersion: (ver: string) => void;
}

const INS_WINDOWS: InsWindow[] = [3, 7, 14];
const INS_ISP_CHOICES: string[] = [
  'gmail', 'yahoo', 'microsoft', 'apple',
  'comcast', 'att', 'cox', 'charter',
];

const ISPInsightsPanel: React.FC<ISPInsightsPanelProps> = ({ orgId, onApiVersion }) => {
  const [days, setDays] = useState<InsWindow>(7);
  const [isp, setIsp] = useState<string>('gmail');
  const [metric, setMetric] = useState<InsMetric>('delivered');
  const [loading, setLoading] = useState(false);
  const [series, setSeries] = useState<InsDomainSeries[]>([]);
  const [topCampaigns, setTopCampaigns] = useState<InsTopCampaign[]>([]);
  const [windowMst, setWindowMst] = useState<{ start_mst: string; end_mst: string } | null>(null);

  useEffect(() => {
    let cancelled = false;
    const load = async () => {
      setLoading(true);
      try {
        // Single fetch. The backend (v1.4+) returns a `by_sending_domain`
        // block with one series per sending domain, pre-filtered to the
        // selected ISP. Previously the panel fanned out 1 + N requests
        // (one per domain), which serialised on the DB pool and left
        // every request pending for ~60s. Now: 1 request, ~8–15s.
        const res = await orgFetch(
          `/api/mailing/analytics/isp-sending-insights?isp=${encodeURIComponent(isp)}&days=${days}`,
          orgId,
        );
        const data = await res.json();
        if (cancelled) return;
        onApiVersion(data?.api_version || '?');
        setTopCampaigns(Array.isArray(data?.top_campaigns) ? data.top_campaigns : []);
        if (data?.window?.start_mst && data?.window?.end_mst) {
          setWindowMst({ start_mst: data.window.start_mst, end_mst: data.window.end_mst });
        } else {
          setWindowMst(null);
        }

        const bySD: Record<string, { daily: InsDailyRow[]; totals: InsDomainTotals }> =
          data?.by_sending_domain || {};
        const perDomain: InsDomainSeries[] = Object.keys(bySD)
          .sort()
          .map(domain => {
            const entry = bySD[domain] || { daily: [], totals: null as unknown as InsDomainTotals };
            const daily = Array.isArray(entry.daily) ? entry.daily : [];
            const t = entry.totals;
            const totals: InsDomainTotals = t
              ? {
                  sent: Number(t.sent || 0),
                  delivered: Number(t.delivered || 0),
                  opens: Number(t.opens || 0),
                  clicks: Number(t.clicks || 0),
                  hard_bounces: Number(t.hard_bounces || 0),
                  soft_bounces: Number(t.soft_bounces || 0),
                  open_rate: Number(t.open_rate || 0),
                  click_rate: Number(t.click_rate || 0),
                  inbox_rate: Number(t.inbox_rate || 0),
                  hard_bounce_rate: Number(t.hard_bounce_rate || 0),
                  soft_bounce_rate: Number(t.soft_bounce_rate || 0),
                }
              : computeDomainTotals(daily, 0, 0);
            return { sending_domain: domain, daily, totals };
          });

        setSeries(perDomain);
      } catch (err) {
        console.error('ISP Insights load error:', err);
      } finally {
        if (!cancelled) setLoading(false);
      }
    };
    load();
    return () => { cancelled = true; };
  }, [isp, days, orgId, onApiVersion]);

  const mergedDaily = useMemo(() => {
    const dateSet = new Set<string>();
    for (const s of series) s.daily.forEach(r => dateSet.add(r.date));
    const dates = Array.from(dateSet).sort();
    const todayMst = new Date().toLocaleDateString('en-CA', { timeZone: 'America/Denver' });
    return dates.map(date => {
      const row: Record<string, number | string> = { date, isToday: date === todayMst ? 1 : 0 };
      for (const s of series) {
        const hit = s.daily.find(r => r.date === date);
        row[s.sending_domain] = hit ? Number(hit[metric] || 0) : 0;
      }
      return row;
    });
  }, [series, metric]);

  const totalsRow = useMemo(() => {
    const t = series.reduce((acc, s) => ({
      sent: acc.sent + s.totals.sent,
      delivered: acc.delivered + s.totals.delivered,
      opens: acc.opens + s.totals.opens,
      clicks: acc.clicks + s.totals.clicks,
      hard_bounces: acc.hard_bounces + s.totals.hard_bounces,
      soft_bounces: acc.soft_bounces + s.totals.soft_bounces,
    }), { sent: 0, delivered: 0, opens: 0, clicks: 0, hard_bounces: 0, soft_bounces: 0 });
    const base = t.delivered || t.sent || 1;
    const sentBase = t.sent || 1;
    return {
      ...t,
      inbox_rate: t.sent > 0 ? (t.delivered / t.sent) * 100 : 0,
      open_rate: (t.opens / base) * 100,
      click_rate: (t.clicks / base) * 100,
      hard_bounce_rate: (t.hard_bounces / sentBase) * 100,
      soft_bounce_rate: (t.soft_bounces / sentBase) * 100,
    };
  }, [series]);

  const ispLabel = ISP_LABELS[isp] || isp;

  return (
    <div className="ac-card ig-card-hover" style={{ gridColumn: '1 / -1' }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12, flexWrap: 'wrap', gap: 12 }}>
        <div>
          <h3 style={{ margin: 0 }}>
            <FontAwesomeIcon icon={faFilter} /> ISP Insights &mdash; {ispLabel}
          </h3>
          <div style={{ fontSize: '0.72em', color: '#64748b', marginTop: 4 }}>
            Deliverability and engagement by sending domain at the selected ISP &middot; dates in MST
            {windowMst && (
              <span style={{ marginLeft: 8, color: '#475569' }}>
                ({windowMst.start_mst} &rarr; {windowMst.end_mst})
              </span>
            )}
          </div>
        </div>
        <div style={{ display: 'flex', gap: 10, alignItems: 'center', flexWrap: 'wrap' }}>
          <div className="ac-range-selector">
            {INS_WINDOWS.map(w => (
              <button key={w} className={days === w ? 'active' : ''} onClick={() => setDays(w)}>
                {w}d
              </button>
            ))}
          </div>
          <select
            value={isp}
            onChange={(e) => setIsp(e.target.value)}
            style={{
              background: '#0d1526',
              color: '#e0e6f0',
              border: '1px solid rgba(0,200,255,0.15)',
              borderRadius: 10,
              padding: '8px 12px',
              fontSize: 13,
              fontFamily: 'inherit',
              cursor: 'pointer',
            }}
          >
            {INS_ISP_CHOICES.map(k => (
              <option key={k} value={k}>{ISP_LABELS[k] || k}</option>
            ))}
          </select>
        </div>
      </div>

      {loading ? (
        <div className="ac-empty-mini"><FontAwesomeIcon icon={faSpinner} spin /> Loading ISP insights...</div>
      ) : series.length === 0 ? (
        <div className="ac-empty-mini">No sending domains detected. Run a campaign first.</div>
      ) : (
        <>
          {/* Per-sending-domain rate table */}
          <div className="ac-table-wrap" style={{ marginBottom: 16 }}>
            <table className="ac-table">
              <thead>
                <tr>
                  <th>Sending Domain</th>
                  <th>Sent</th>
                  <th>Delivered</th>
                  <th>Inbox %</th>
                  <th>Opens</th>
                  <th>Open %</th>
                  <th>Clicks</th>
                  <th>Click %</th>
                  <th>Hard B <FontAwesomeIcon icon={faInfoCircle} title="Hard bounces indicate invalid addresses and degrade sender reputation. Treat as list hygiene signal." style={{ fontSize: 10, color: '#475569' }} /></th>
                  <th>Soft B <FontAwesomeIcon icon={faInfoCircle} title="Soft bounces retry through the PMTA deferred queue. Not a list-quality signal." style={{ fontSize: 10, color: '#475569' }} /></th>
                </tr>
              </thead>
              <tbody>
                {series.map(s => (
                  <tr key={s.sending_domain}>
                    <td style={{ fontWeight: 500 }}>
                      <span style={{ display: 'inline-block', width: 10, height: 10, borderRadius: 2, background: colorForDomain(s.sending_domain), marginRight: 8 }} />
                      {s.sending_domain}
                    </td>
                    <td>{fmt(s.totals.sent)}</td>
                    <td>{fmt(s.totals.delivered)}</td>
                    <td className={s.totals.inbox_rate > 95 ? 'ac-good' : s.totals.inbox_rate > 90 ? 'ac-ok' : 'ac-bad'}>{pct(s.totals.inbox_rate)}</td>
                    <td>{fmt(s.totals.opens)}</td>
                    <td className={s.totals.open_rate > 20 ? 'ac-good' : s.totals.open_rate > 10 ? 'ac-ok' : 'ac-bad'}>{pct(s.totals.open_rate)}</td>
                    <td>{fmt(s.totals.clicks)}</td>
                    <td className={s.totals.click_rate > 3 ? 'ac-good' : s.totals.click_rate > 1 ? 'ac-ok' : 'ac-bad'}>{pct(s.totals.click_rate)}</td>
                    <td className={s.totals.hard_bounce_rate < 2 ? 'ac-good' : s.totals.hard_bounce_rate < 5 ? 'ac-ok' : 'ac-bad'}>{pct(s.totals.hard_bounce_rate)}</td>
                    <td style={{ color: '#94a3b8' }}>{pct(s.totals.soft_bounce_rate)}</td>
                  </tr>
                ))}
                <tr style={{ borderTop: '2px solid rgba(0,200,255,0.15)', fontWeight: 600, color: '#c0c4d0' }}>
                  <td>All domains</td>
                  <td>{fmt(totalsRow.sent)}</td>
                  <td>{fmt(totalsRow.delivered)}</td>
                  <td className={totalsRow.inbox_rate > 95 ? 'ac-good' : totalsRow.inbox_rate > 90 ? 'ac-ok' : 'ac-bad'}>{pct(totalsRow.inbox_rate)}</td>
                  <td>{fmt(totalsRow.opens)}</td>
                  <td>{pct(totalsRow.open_rate)}</td>
                  <td>{fmt(totalsRow.clicks)}</td>
                  <td>{pct(totalsRow.click_rate)}</td>
                  <td>{pct(totalsRow.hard_bounce_rate)}</td>
                  <td style={{ color: '#94a3b8' }}>{pct(totalsRow.soft_bounce_rate)}</td>
                </tr>
              </tbody>
            </table>
          </div>

          {/* Metric selector + daily trend chart */}
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8 }}>
            <div style={{ fontSize: '0.8em', color: '#94a3b8' }}>
              Daily trend ({days}d, {ispLabel})
            </div>
            <div className="ac-range-selector">
              {(Object.keys(INS_METRIC_LABELS) as InsMetric[]).map(k => (
                <button key={k} className={metric === k ? 'active' : ''} onClick={() => setMetric(k)}>
                  {INS_METRIC_LABELS[k]}
                </button>
              ))}
            </div>
          </div>
          <div className="ac-trend-chart" style={{ marginBottom: 16 }}>
            <ResponsiveContainer width="100%" height={280}>
              <LineChart data={mergedDaily} margin={{ top: 10, right: 10, left: 0, bottom: 0 }}>
                <CartesianGrid strokeDasharray="3 3" stroke="#1e293b" vertical={false} />
                <XAxis
                  dataKey="date"
                  stroke="#334155"
                  tick={{ fill: '#64748b', fontSize: 11 }}
                  tickLine={false}
                  axisLine={{ stroke: '#1e293b' }}
                  tickFormatter={(v: string) => v.slice(5)}
                />
                <YAxis
                  stroke="#334155"
                  tick={{ fill: '#64748b', fontSize: 11 }}
                  tickLine={false}
                  axisLine={false}
                  tickFormatter={(v: number) => v >= 1000 ? `${(v / 1000).toFixed(1)}k` : String(v)}
                />
                <RechartsTooltip
                  contentStyle={{ background: '#0f172a', border: '1px solid #1e293b', borderRadius: 8, fontSize: 12 }}
                  labelStyle={{ color: '#94a3b8' }}
                  formatter={(value: number) => fmt(value)}
                  labelFormatter={(label: string, payload: Array<{ payload?: { isToday?: number } }>) => {
                    const isToday = payload && payload[0]?.payload?.isToday === 1;
                    return isToday ? `${label} (today — partial)` : label;
                  }}
                />
                <Legend wrapperStyle={{ fontSize: 11, color: '#94a3b8' }} />
                {series.map(s => (
                  <Line
                    key={s.sending_domain}
                    type="monotone"
                    dataKey={s.sending_domain}
                    stroke={colorForDomain(s.sending_domain)}
                    strokeWidth={2}
                    dot={false}
                    name={s.sending_domain}
                  />
                ))}
              </LineChart>
            </ResponsiveContainer>
          </div>

          {/* Top campaigns leaderboard */}
          <div style={{ marginBottom: 4, fontSize: '0.8em', color: '#94a3b8', display: 'flex', alignItems: 'center', gap: 6 }}>
            <FontAwesomeIcon icon={faTrophy} style={{ color: '#fbbf24' }} />
            Top campaigns at {ispLabel} ({days}d window, sorted by clicks)
          </div>
          {topCampaigns.length === 0 ? (
            <div className="ac-empty-mini">No campaign engagement recorded at {ispLabel} in this window.</div>
          ) : (
            <div className="ac-table-wrap">
              <table className="ac-table">
                <thead>
                  <tr>
                    <th style={{ width: 40 }}>#</th>
                    <th>Campaign</th>
                    <th>Sending Domain</th>
                    <th>Sent</th>
                    <th>Delivered</th>
                    <th>Opens</th>
                    <th>Open %</th>
                    <th>Clicks</th>
                    <th>Click %</th>
                    <th>Hard B %</th>
                  </tr>
                </thead>
                <tbody>
                  {topCampaigns.map((c, i) => (
                    <tr key={c.campaign_id}>
                      <td style={{ color: i < 3 ? '#fbbf24' : '#475569', fontWeight: 600 }}>{i + 1}</td>
                      <td style={{ fontWeight: 500 }}>{c.name}</td>
                      <td style={{ color: '#94a3b8' }}>
                        <span style={{ display: 'inline-block', width: 8, height: 8, borderRadius: 2, background: colorForDomain(c.sending_domain), marginRight: 6 }} />
                        {c.sending_domain || '—'}
                      </td>
                      <td>{fmt(c.sent)}</td>
                      <td>{fmt(c.delivered)}</td>
                      <td>{fmt(c.opens)}</td>
                      <td className={c.open_rate > 20 ? 'ac-good' : c.open_rate > 10 ? 'ac-ok' : 'ac-bad'}>{pct(c.open_rate)}</td>
                      <td>{fmt(c.clicks)}</td>
                      <td className={c.click_rate > 3 ? 'ac-good' : c.click_rate > 1 ? 'ac-ok' : 'ac-bad'}>{pct(c.click_rate)}</td>
                      <td className={c.hard_bounce_rate < 2 ? 'ac-good' : c.hard_bounce_rate < 5 ? 'ac-ok' : 'ac-bad'}>{pct(c.hard_bounce_rate)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </div>
  );
};

// ─── Component ───────────────────────────────────────────────────────────────

export const AnalyticsCenter: React.FC = () => {
  const { organization } = useAuth();
  const orgId = organization?.id || '';

  const [range, setRange] = useState<TimeRange>('today');
  const [loading, setLoading] = useState(true);

  // Data — overview is the primary source for KPIs, rates, and deliverability
  const [overview, setOverview] = useState<OverviewData | null>(null);
  const [campaigns, setCampaigns] = useState<CampaignData | null>(null);

  // Infrastructure Breakdown state
  const [infraData, setInfraData] = useState<InfraRow[]>([]);
  const [infraLoading, setInfraLoading] = useState(false);
  const [selectedDomain, setSelectedDomain] = useState<string | null>(null);
  const [drilldownType, setDrilldownType] = useState<'ip' | 'isp'>('ip');
  const [selectedCampaign, setSelectedCampaign] = useState<{id: string, name: string} | null>(null);

  // Chart domain filter — drives chart-specific trend data (UI selector TBD)
  const [chartDomain, _setChartDomain] = useState<string>('');
  const [_chartTrend, setChartTrend] = useState<OverviewData['daily_trend']>([]);
  const [_chartLoading, setChartLoading] = useState(false);

  // ISP Performance
  const [ispCards, setIspCards] = useState<ISPData[]>([]);
  const [selectedISP, setSelectedISP] = useState<string | null>(null);
  const [ispTrend, setIspTrend] = useState<OverviewData['daily_trend']>([]);
  const [ispGranularity, setIspGranularity] = useState<string>('day');
  const [ispTrendLoading, setIspTrendLoading] = useState(false);

  // MPP filtering toggle
  const [excludeMPP, setExcludeMPP] = useState(false);

  // Live Sending top band
  const [liveData, setLiveData] = useState<ActiveSendsData | null>(null);
  const [expandedLiveCampaign, setExpandedLiveCampaign] = useState<string | null>(null);

  // Bounce reasons panel
  const [bounceReasons, setBounceReasons] = useState<BounceReasonsData | null>(null);
  const [expandedBounceReason, setExpandedBounceReason] = useState<string | null>(null);

  // Master List Migration P6: SDS audience health per sending domain.
  // Source of truth for "who is mailable on which domain" now lives in
  // mailing_subscriber_domain_state, not the 40+ legacy brand×ISP lists.
  const [sdsHealth, setSdsHealth] = useState<SDSDomainRow[]>([]);
  const [sdsTotal, setSdsTotal] = useState<number>(0);
  const [sdsLoading, setSdsLoading] = useState<boolean>(false);

  // Deployment verification: track API versions from responses
  const [apiVersions, setApiVersions] = useState<Record<string, string>>({});

  const fetchAll = useCallback(async () => {
    setLoading(true);
    try {
      const { startDate, endDate } = computeDateRange(range);
      const mppParam = excludeMPP ? '&exclude_mpp=true' : '';
      const qp = `?start_date=${encodeURIComponent(startDate)}&end_date=${encodeURIComponent(endDate)}&range_type=${range}&days=1${mppParam}`;
      const [ovRes, campRes, ispRes, bounceRes] = await Promise.all([
        orgFetch(`/api/mailing/analytics/overview${qp}`, orgId),
        orgFetch(`/api/mailing/reports/campaigns${qp}`, orgId),
        orgFetch(`/api/mailing/analytics/isp-performance${qp}`, orgId),
        orgFetch(`/api/mailing/analytics/bounce-reasons${qp}`, orgId),
      ]);
      const [ov, camp, ispPerf, bounces] = await Promise.all([
        ovRes.json().catch(() => null),
        campRes.json().catch(() => null),
        ispRes.json().catch(() => null),
        bounceRes.json().catch(() => null),
      ]);
      setOverview(ov);
      setCampaigns(camp);
      setIspCards(ispPerf?.isps || []);
      setBounceReasons(bounces);
      setSelectedISP(null);
      setIspTrend([]);
      setApiVersions(prev => ({
        ...prev,
        overview: ov?.api_version || '?',
        campaigns: camp?.api_version || '?',
        isp_performance: ispPerf?.api_version || '?',
        bounce_reasons: bounces?.api_version || '?',
      }));
    } catch (err) {
      console.error('Analytics load error:', err);
    } finally {
      setLoading(false);
    }
  }, [range, orgId, excludeMPP]);

  useEffect(() => { fetchAll(); }, [fetchAll]);

  // ─── Live Sending polling (20s) ──────────────────────────────────────────
  const fetchLive = useCallback(async () => {
    try {
      const res = await orgFetch('/api/mailing/analytics/active-sends', orgId);
      const data: ActiveSendsData = await res.json();
      setLiveData(data);
      setApiVersions(prev => ({ ...prev, active_sends: data?.api_version || '?' }));
    } catch (err) {
      console.error('Live sends load error:', err);
    }
  }, [orgId]);

  useEffect(() => {
    fetchLive();
    const id = window.setInterval(fetchLive, 20_000);
    return () => window.clearInterval(id);
  }, [fetchLive]);

  useEffect(() => {
    if (!selectedISP) { setIspTrend([]); return; }
    let cancelled = false;
    const load = async () => {
      setIspTrendLoading(true);
      try {
        const { startDate, endDate } = computeDateRange(range);
        const mppP = excludeMPP ? '&exclude_mpp=true' : '';
        const qp = `?start_date=${encodeURIComponent(startDate)}&end_date=${encodeURIComponent(endDate)}&range_type=${range}&isp=${selectedISP}${mppP}`;
        const res = await orgFetch(`/api/mailing/analytics/isp-performance${qp}`, orgId);
        const data = await res.json();
        if (!cancelled) {
          setIspTrend(data?.trend || []);
          setIspGranularity(data?.granularity || 'day');
        }
      } catch (err) {
        console.error('ISP trend load error:', err);
        if (!cancelled) setIspTrend([]);
      } finally {
        if (!cancelled) setIspTrendLoading(false);
      }
    };
    load();
    return () => { cancelled = true; };
  }, [selectedISP, range, orgId]);

  const fetchInfrastructure = useCallback(async (domain: string | null, type: 'ip' | 'isp', campaignId?: string) => {
    setInfraLoading(true);
    try {
      const { startDate, endDate } = computeDateRange(range);
      let qp = `?start_date=${encodeURIComponent(startDate)}&end_date=${encodeURIComponent(endDate)}`;
      if (domain) qp += `&domain=${encodeURIComponent(domain)}&drilldown=${type}`;
      if (campaignId) qp += `&campaign_id=${encodeURIComponent(campaignId)}`;

      const res = await orgFetch(`/api/mailing/reports/infrastructure${qp}`, orgId);
      const result = await res.json();
      setInfraData(result.data || []);
      setApiVersions(prev => ({ ...prev, infrastructure: result.api_version || '?' }));
    } catch (err) {
      console.error('Infra load error:', err);
    } finally {
      setInfraLoading(false);
    }
  }, [range, orgId]);

  useEffect(() => {
    fetchInfrastructure(selectedDomain, drilldownType, selectedCampaign?.id);
  }, [fetchInfrastructure, selectedDomain, drilldownType, range, selectedCampaign?.id]);

  // Master List Migration P6: load SDS audience health. This does not
  // depend on the date-range selector because SDS is a point-in-time
  // snapshot of current per-domain subscriber state, not a time-series.
  const fetchSDSHealth = useCallback(async () => {
    setSdsLoading(true);
    try {
      const res = await orgFetch('/api/mailing/analytics/sds-audience-health', orgId);
      const data = await res.json();
      setSdsHealth(Array.isArray(data?.by_domain) ? data.by_domain : []);
      setSdsTotal(typeof data?.total_rows === 'number' ? data.total_rows : 0);
      setApiVersions(prev => ({ ...prev, sds_audience_health: data?.api_version || '?' }));
    } catch (err) {
      console.error('SDS health load error:', err);
    } finally {
      setSdsLoading(false);
    }
  }, [orgId]);

  useEffect(() => { fetchSDSHealth(); }, [fetchSDSHealth]);

  // Fetch chart-specific trend when domain filter changes
  useEffect(() => {
    if (!chartDomain) {
      setChartTrend(overview?.daily_trend || []);
      return;
    }
    let cancelled = false;
    (async () => {
      setChartLoading(true);
      try {
        const { startDate, endDate } = computeDateRange(range);
        const qp = `?start_date=${encodeURIComponent(startDate)}&end_date=${encodeURIComponent(endDate)}&trend_domain=${encodeURIComponent(chartDomain)}`;
        const res = await orgFetch(`/api/mailing/analytics/overview${qp}`, orgId);
        const data = await res.json();
        if (!cancelled) setChartTrend(data.daily_trend || []);
      } catch { /* noop */ }
      finally { if (!cancelled) setChartLoading(false); }
    })();
    return () => { cancelled = true; };
  }, [chartDomain, overview, range, orgId]);

  // Sync chart trend from overview when no domain filter
  useEffect(() => {
    if (!chartDomain) setChartTrend(overview?.daily_trend || []);
  }, [overview, chartDomain]);

  // ─── Derived Values ────────────────────────────────────────────────────────
  const totals = overview?.totals || { sent: 0, delivered: 0, opens: 0, clicks: 0, hard_bounces: 0, soft_bounces: 0, complaints: 0, revenue: 0, deferred: 0, unsubscribes: 0 };
  const rates = overview?.rates || { open_rate: 0, click_rate: 0, hard_bounce_rate: 0, soft_bounce_rate: 0, complaint_rate: 0, delivery_rate: 0, deferral_rate: 0, unsubscribe_rate: 0 };
  const trend = overview?.daily_trend || [];
  const granularity = overview?.granularity || 'day';

  // ─── Brand × Series rollup (client-side, derived from /reports/campaigns) ──
  const brandSeriesRollup = useMemo(() => {
    const rows = campaigns?.campaigns || [];
    type Bucket = {
      brand: string; series: string; campaigns: number;
      sent: number; delivered: number; opens: number; clicks: number;
      hard_bounces: number; soft_bounces: number; deferred: number; unsubscribes: number;
      revenue: number;
    };
    const map = new Map<string, Bucket>();
    for (const c of rows) {
      const { brand, series } = classifyCampaign(c.name || '');
      const key = `${brand}::${series}`;
      let b = map.get(key);
      if (!b) {
        b = { brand, series, campaigns: 0, sent: 0, delivered: 0, opens: 0, clicks: 0, hard_bounces: 0, soft_bounces: 0, deferred: 0, unsubscribes: 0, revenue: 0 };
        map.set(key, b);
      }
      b.campaigns += 1;
      b.sent += c.sent || 0;
      b.delivered += c.delivered || 0;
      b.opens += c.opens || 0;
      b.clicks += c.clicks || 0;
      b.hard_bounces += c.hard_bounces || 0;
      b.soft_bounces += c.soft_bounces || 0;
      b.deferred += c.deferred || 0;
      b.unsubscribes += c.unsubscribes || 0;
      b.revenue += c.revenue || 0;
    }
    const out = Array.from(map.values()).map(b => {
      const base = b.delivered > 0 ? b.delivered : b.sent;
      const openRate = base > 0 ? (b.opens / base) * 100 : 0;
      const clickRate = base > 0 ? (b.clicks / base) * 100 : 0;
      const hardRate = b.sent > 0 ? (b.hard_bounces / b.sent) * 100 : 0;
      const softRate = b.sent > 0 ? (b.soft_bounces / b.sent) * 100 : 0;
      const deferralRate = b.sent > 0 ? (b.deferred / b.sent) * 100 : 0;
      const unsubRate = b.sent > 0 ? (b.unsubscribes / b.sent) * 100 : 0;
      return { ...b, open_rate: openRate, click_rate: clickRate, hard_bounce_rate: hardRate, soft_bounce_rate: softRate, deferral_rate: deferralRate, unsubscribe_rate: unsubRate };
    });
    out.sort((a, b) => (b.sent || 0) - (a.sent || 0));
    return out;
  }, [campaigns]);

  // Filter bounce reasons for the selected ISP detail pane.
  const bounceReasonsForSelectedISP = useMemo(() => {
    if (!selectedISP || !bounceReasons) return [];
    return bounceReasons.reasons
      .map(r => {
        const hit = r.by_isp.find(b => b.isp === selectedISP);
        return hit ? { ...r, isp_count: hit.count } : null;
      })
      .filter((x): x is BounceReasonRow & { isp_count: number } => !!x && x.isp_count > 0)
      .sort((a, b) => b.isp_count - a.isp_count)
      .slice(0, 6);
  }, [selectedISP, bounceReasons]);

  return (
    <div className="ac-container ig-scan-line">
      {/* ─── Header ─────────────────────────────────────────────────────── */}
      <div className="ac-header">
        <div className="ac-header-left">
          <div className="ac-header-icon"><FontAwesomeIcon icon={faChartLine} /></div>
          <div>
            <h1>Analytics Center</h1>
            <p>Comprehensive mail performance &amp; AI intelligence metrics</p>
          </div>
        </div>
        <div className="ac-header-right">
          <div className="ac-range-selector">
            {([
              { key: 'today' as TimeRange, label: 'Today' },
              { key: 'yesterday' as TimeRange, label: 'Yesterday' },
            ]).map(r => (
              <button key={r.key} className={range === r.key ? 'active' : ''} onClick={() => setRange(r.key)}>
                {r.label}
              </button>
            ))}
          </div>
          <button
            className={`ac-mpp-toggle ${excludeMPP ? 'active' : ''}`}
            onClick={() => setExcludeMPP(prev => !prev)}
            title="Exclude Apple Mail Privacy Protection (MPP) opens from metrics"
          >
            <FontAwesomeIcon icon={faShieldAlt} />
            {excludeMPP ? ' MPP Excluded' : ' Include MPP'}
          </button>
          <button className="ac-refresh-btn ig-btn-glow ig-ripple" onClick={fetchAll} disabled={loading}>
            <FontAwesomeIcon icon={faSyncAlt} spin={loading} /> Refresh
          </button>
        </div>
      </div>

      {loading && !overview ? (
        <div className="ac-loading">
          <FontAwesomeIcon icon={faSpinner} spin size="2x" />
          <p>Loading comprehensive analytics...</p>
        </div>
      ) : (
        <>
          {/* ─── Live Sending Band ────────────────────────────────────── */}
          <LiveSendingBand
            data={liveData}
            expanded={expandedLiveCampaign}
            onToggle={(id) => setExpandedLiveCampaign(prev => prev === id ? null : id)}
            onRefresh={fetchLive}
          />

          {/* ─── KPI Hero Cards ─────────────────────────────────────────── */}
          <div className="ac-kpi-grid ig-stagger">
            <div className="ac-kpi sent ig-card-hover ig-shimmer">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faEnvelope} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value"><AnimatedCounter value={totals.sent} formatFn={fmt} /></span>
                <span className="ac-kpi-label">Emails Sent</span>
              </div>
            </div>
            <div className="ac-kpi opens ig-card-hover ig-shimmer">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faEye} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value"><AnimatedCounter value={rates.open_rate} decimals={1} suffix="%" /></span>
                <span className="ac-kpi-label">Open Rate</span>
                <span className="ac-kpi-sub">{fmt(totals.opens)} opens</span>
              </div>
            </div>
            <div className="ac-kpi clicks ig-card-hover ig-shimmer">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faMousePointer} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value"><AnimatedCounter value={rates.click_rate} decimals={1} suffix="%" /></span>
                <span className="ac-kpi-label">Click Rate</span>
                <span className="ac-kpi-sub">{fmt(totals.clicks)} clicks</span>
              </div>
            </div>
            <div className="ac-kpi revenue ig-card-hover ig-shimmer">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faDollarSign} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value"><AnimatedCounter value={totals.revenue} formatFn={fmtCurrency} /></span>
                <span className="ac-kpi-label">Revenue</span>
                <span className="ac-kpi-sub">{fmtCurrency(totals.sent > 0 ? totals.revenue / totals.sent : 0)}/email</span>
              </div>
            </div>
            <div className="ac-kpi bounces ig-card-hover ig-shimmer">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faExclamationTriangle} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value" style={{ color: '#ef4444' }}><AnimatedCounter value={rates.hard_bounce_rate} decimals={1} suffix="%" /></span>
                <span className="ac-kpi-label">Hard Bounce Rate</span>
                <span className="ac-kpi-sub">{fmt(totals.hard_bounces)} hard bounced</span>
              </div>
            </div>
            <div className="ac-kpi bounces ig-card-hover ig-shimmer">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faExclamationTriangle} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value" style={{ color: '#f59e0b' }}><AnimatedCounter value={rates.soft_bounce_rate} decimals={1} suffix="%" /></span>
                <span className="ac-kpi-label">Soft Bounce Rate</span>
                <span className="ac-kpi-sub">{fmt(totals.soft_bounces)} soft bounced</span>
              </div>
            </div>
            <div className="ac-kpi complaints ig-card-hover ig-shimmer">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faBan} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value"><AnimatedCounter value={rates.complaint_rate} decimals={2} suffix="%" /></span>
                <span className="ac-kpi-label">Complaint Rate</span>
                <span className="ac-kpi-sub">{fmt(totals.complaints)} complaints</span>
              </div>
            </div>
            <div className="ac-kpi deferred ig-card-hover ig-shimmer">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faClock} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value" style={{ color: '#3b82f6' }}><AnimatedCounter value={rates.deferral_rate || 0} decimals={2} suffix="%" /></span>
                <span className="ac-kpi-label">Deferral Rate</span>
                <span className="ac-kpi-sub">{fmt(totals.deferred || 0)} deferred</span>
              </div>
            </div>
            <div className="ac-kpi unsubs ig-card-hover ig-shimmer">
              <div className="ac-kpi-icon"><FontAwesomeIcon icon={faUserSlash} /></div>
              <div className="ac-kpi-body">
                <span className="ac-kpi-value" style={{ color: '#a855f7' }}><AnimatedCounter value={rates.unsubscribe_rate || 0} decimals={2} suffix="%" /></span>
                <span className="ac-kpi-label">Unsubscribe Rate</span>
                <span className="ac-kpi-sub">{fmt(totals.unsubscribes || 0)} unsubscribes</span>
              </div>
            </div>
          </div>

          {/* ─── ISP Performance Cards ─────────────────────────────────── */}
          <div className="ac-card ig-card-hover" style={{ gridColumn: '1 / -1' }}>
            <h3><FontAwesomeIcon icon={faChartLine} /> Performance by ISP</h3>
            {ispCards.length === 0 ? (
              <div className="ac-empty-mini">No ISP data available for this period.</div>
            ) : (
              <>
                <div className="ac-isp-grid">
                  {ispCards.filter(c => c.isp !== 'other').map(card => {
                    const score = Math.max(0, Math.min(100, Math.round((card.open_rate || 0) - ((card.hard_bounce_rate || 0) + (card.soft_bounce_rate || 0)) * 2 - (card.complaint_rate || 0) * 10)));
                    const scoreColor = score >= 60 ? '#22c55e' : score >= 30 ? '#f59e0b' : '#ef4444';
                    const isSelected = selectedISP === card.isp;
                    return (
                      <div
                        key={card.isp}
                        className={`ac-isp-card${isSelected ? ' selected' : ''}`}
                        onClick={() => setSelectedISP(isSelected ? null : card.isp)}
                        style={{ borderColor: isSelected ? ISP_COLORS[card.isp] || '#6366f1' : undefined }}
                      >
                        <div className="ac-isp-card-header">
                          <span className="ac-isp-dot" style={{ background: ISP_COLORS[card.isp] || '#6B7280' }} />
                          <span className="ac-isp-name">{ISP_LABELS[card.isp] || card.label}</span>
                          <span className="ac-isp-score" style={{ color: scoreColor }}>{score}</span>
                        </div>
                        <div className="ac-isp-card-metrics">
                          <div className="ac-isp-metric">
                            <span className="ac-isp-metric-val">{fmt(card.sent)}</span>
                            <span className="ac-isp-metric-lbl">Sent</span>
                          </div>
                          <div className="ac-isp-metric">
                            <span className="ac-isp-metric-val" style={{ color: '#22c55e' }}>{card.open_rate}%</span>
                            <span className="ac-isp-metric-lbl">Opens</span>
                          </div>
                          <div className="ac-isp-metric">
                            <span className="ac-isp-metric-val" style={{ color: '#3b82f6' }}>{card.click_rate}%</span>
                            <span className="ac-isp-metric-lbl">Clicks</span>
                          </div>
                          <div className="ac-isp-metric">
                            <span className="ac-isp-metric-val" style={{ color: '#ef4444' }}>{card.hard_bounce_rate ?? 0}%</span>
                            <span className="ac-isp-metric-lbl">Hard Bnc</span>
                          </div>
                          <div className="ac-isp-metric">
                            <span className="ac-isp-metric-val" style={{ color: '#f59e0b' }}>{card.soft_bounce_rate ?? 0}%</span>
                            <span className="ac-isp-metric-lbl">Soft Bnc</span>
                          </div>
                          <div className="ac-isp-metric">
                            <span className="ac-isp-metric-val" style={{ color: '#ef4444' }}>{card.complaint_rate}%</span>
                            <span className="ac-isp-metric-lbl">Complaints</span>
                          </div>
                        </div>
                      </div>
                    );
                  })}
                </div>

                {selectedISP && (
                  <div className="ac-isp-detail">
                    <div className="ac-isp-detail-header">
                      <h4 style={{ color: ISP_COLORS[selectedISP] || '#a5b4fc' }}>
                        {ISP_LABELS[selectedISP] || selectedISP} — Detailed Metrics
                      </h4>
                      <button className="ac-isp-close" onClick={() => setSelectedISP(null)}>&times;</button>
                    </div>

                    <ISPFunnelAndTiles
                      card={ispCards.find(c => c.isp === selectedISP) || null}
                    />

                    {bounceReasonsForSelectedISP.length > 0 && (
                      <div className="ac-isp-bounce-block">
                        <h5 style={{ margin: '12px 0 8px', fontSize: '0.85em', color: '#94a3b8', textTransform: 'uppercase', letterSpacing: '0.05em' }}>
                          Top Bounce Reasons ({ISP_LABELS[selectedISP] || selectedISP})
                        </h5>
                        <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
                          {bounceReasonsForSelectedISP.map((r, i) => (
                            <div key={`${r.label}-${i}`} className="ac-isp-bounce-row">
                              <span className="ac-bounce-cat-pill" style={{ background: BOUNCE_CATEGORY_COLORS[r.category] || '#64748b' }}>
                                {r.category}
                              </span>
                              <span className="ac-bounce-code">{r.code || '—'}</span>
                              <span className="ac-bounce-label">{r.label}</span>
                              <span className="ac-bounce-count">{fmt(r.isp_count)}</span>
                            </div>
                          ))}
                        </div>
                      </div>
                    )}

                    {ispTrendLoading ? (
                      <div className="ac-empty-mini"><FontAwesomeIcon icon={faSpinner} spin /> Loading trend…</div>
                    ) : ispTrend.length === 0 ? (
                      <div className="ac-empty-mini">No trend data for this ISP.</div>
                    ) : (
                      <div className="ac-trend-chart">
                        <ResponsiveContainer width="100%" height={300}>
                          <LineChart data={ispTrend} margin={{ top: 10, right: 10, left: 0, bottom: 0 }}>
                            <CartesianGrid strokeDasharray="3 3" stroke="#1e293b" vertical={false} />
                            <XAxis
                              dataKey="date"
                              stroke="#334155"
                              tick={{ fill: '#64748b', fontSize: 11 }}
                              tickLine={false}
                              axisLine={{ stroke: '#1e293b' }}
                              minTickGap={30}
                              tickFormatter={(value: string) => {
                                if (ispGranularity === '10min' || ispGranularity === 'hour') {
                                  const t = value.includes('T') ? value.split('T')[1] : value;
                                  const h = parseInt(t.split(':')[0], 10);
                                  const ampm = h >= 12 ? 'PM' : 'AM';
                                  return `${h === 0 ? 12 : h > 12 ? h - 12 : h} ${ampm}`;
                                }
                                return value.slice(5);
                              }}
                            />
                            <YAxis
                              yAxisId="left"
                              stroke="#334155"
                              tick={{ fill: '#64748b', fontSize: 11 }}
                              tickLine={false}
                              axisLine={false}
                              tickFormatter={(v: number) => v >= 1000 ? `${(v / 1000).toFixed(1)}k` : String(v)}
                            />
                            <YAxis
                              yAxisId="right"
                              orientation="right"
                              stroke="#334155"
                              tick={{ fill: '#64748b', fontSize: 11 }}
                              tickLine={false}
                              axisLine={false}
                              tickFormatter={(v: number) => v >= 1000 ? `${(v / 1000).toFixed(1)}k` : String(v)}
                            />
                            <RechartsTooltip
                              contentStyle={{ background: '#1e293b', border: '1px solid #334155', borderRadius: 8, fontSize: 12 }}
                              labelStyle={{ color: '#94a3b8' }}
                              formatter={(value: number, name: string) => [fmt(value), name.charAt(0).toUpperCase() + name.slice(1)]}
                            />
                            <Legend wrapperStyle={{ fontSize: 11, color: '#94a3b8' }} />
                            <Line yAxisId="left" type="monotone" dataKey="sent" stroke={ISP_COLORS[selectedISP] || '#a5b4fc'} strokeWidth={2} dot={false} name="Sent" />
                            <Line yAxisId="right" type="monotone" dataKey="opens" stroke="#22c55e" strokeWidth={1.5} dot={false} name="Opens" />
                            <Line yAxisId="right" type="monotone" dataKey="clicks" stroke="#3b82f6" strokeWidth={1.5} dot={false} name="Clicks" />
                            <Line yAxisId="right" type="monotone" dataKey="hard_bounces" stroke="#ef4444" strokeWidth={1.5} dot={false} name="Hard Bounces" />
                            <Line yAxisId="right" type="monotone" dataKey="soft_bounces" stroke="#f59e0b" strokeWidth={1.5} dot={false} name="Soft Bounces" />
                            <Line yAxisId="right" type="monotone" dataKey="complaints" stroke="#a855f7" strokeWidth={1.5} dot={false} name="Complaints" />
                          </LineChart>
                        </ResponsiveContainer>
                      </div>
                    )}
                  </div>
                )}
              </>
            )}
          </div>

          {/* ─── ISP Insights (MST, multi-day, per sending-domain) ──────── */}
          <ISPInsightsPanel
            orgId={orgId}
            sendingDomains={overview?.sending_domains || []}
            onApiVersion={(ver) => setApiVersions(prev => ({ ...prev, isp_sending_insights: ver }))}
          />

          {/* ─── Main Content (full-width) ────────────────────────────── */}
          <div className="ac-main-content ig-fade-in">

              {/* Daily Trend Chart */}
              <div className="ac-card ig-card-hover">
                <h3><FontAwesomeIcon icon={faChartLine} /> Send Volume &amp; Engagement</h3>
                {trend.length === 0 ? (
                  <div className="ac-empty-mini">No trend data available for this period.</div>
                ) : (
                  <div className="ac-trend-chart">
                    <ResponsiveContainer width="100%" height={300}>
                      <LineChart data={trend} margin={{ top: 10, right: 10, left: 0, bottom: 0 }}>
                        <CartesianGrid strokeDasharray="3 3" stroke="#1e293b" vertical={false} />
                        <XAxis
                          dataKey="date"
                          stroke="#334155"
                          tick={{ fill: '#64748b', fontSize: 11 }}
                          tickLine={false}
                          axisLine={{ stroke: '#1e293b' }}
                          minTickGap={30}
                          tickFormatter={(value: string) => {
                            if (granularity === '10min' || granularity === 'hour') {
                              const t = value.includes('T') ? value.split('T')[1] : value;
                              const h = parseInt(t.split(':')[0], 10);
                              const ampm = h >= 12 ? 'PM' : 'AM';
                              return `${h === 0 ? 12 : h > 12 ? h - 12 : h} ${ampm}`;
                            }
                            return value.slice(5);
                          }}
                        />
                        <YAxis
                          yAxisId="sent"
                          stroke="#334155"
                          tick={{ fill: '#64748b', fontSize: 11 }}
                          tickLine={false}
                          axisLine={false}
                          width={55}
                          tickFormatter={(v: number) => {
                            if (v >= 1_000_000) return (v / 1_000_000).toFixed(1) + 'M';
                            if (v >= 1_000) return (v / 1_000).toFixed(0) + 'K';
                            return String(v);
                          }}
                        />
                        <YAxis
                          yAxisId="engagement"
                          orientation="right"
                          stroke="#334155"
                          tick={{ fill: '#475569', fontSize: 10 }}
                          tickLine={false}
                          axisLine={false}
                          width={45}
                          tickFormatter={(v: number) => {
                            if (v >= 1_000_000) return (v / 1_000_000).toFixed(1) + 'M';
                            if (v >= 1_000) return (v / 1_000).toFixed(0) + 'K';
                            return String(v);
                          }}
                        />
                        <RechartsTooltip
                          contentStyle={{ background: '#0f172a', border: '1px solid #1e293b', borderRadius: 8, fontSize: 12 }}
                          labelStyle={{ color: '#94a3b8', marginBottom: 4 }}
                          itemStyle={{ padding: '1px 0' }}
                          labelFormatter={(label: string) => {
                            if (granularity === '10min' || granularity === 'hour') {
                              const t = String(label).includes('T') ? String(label).split('T')[1] : String(label);
                              return t.slice(0, 5);
                            }
                            return label;
                          }}
                          formatter={(value: number) => fmt(value)}
                        />
                        <Legend
                          verticalAlign="top"
                          height={30}
                          wrapperStyle={{ fontSize: 11, color: '#94a3b8' }}
                        />
                        <Line yAxisId="sent" type="monotone" dataKey="sent" stroke="#00e5ff" strokeWidth={2} dot={false} name="Sent" animationDuration={800} />
                        <Line yAxisId="engagement" type="monotone" dataKey="opens" stroke="#10b981" strokeWidth={2} dot={false} name="Opens" animationDuration={800} />
                        <Line yAxisId="engagement" type="monotone" dataKey="clicks" stroke="#f59e0b" strokeWidth={2} dot={false} name="Clicks" animationDuration={800} />
                        <Line yAxisId="engagement" type="monotone" dataKey="hard_bounces" stroke="#ef4444" strokeWidth={1.5} dot={false} name="Hard Bounces" animationDuration={800} />
                        <Line yAxisId="engagement" type="monotone" dataKey="soft_bounces" stroke="#f59e0b" strokeWidth={1.5} dot={false} name="Soft Bounces" animationDuration={800} />
                        <Line yAxisId="engagement" type="monotone" dataKey="complaints" stroke="#a855f7" strokeWidth={1.5} dot={false} name="Complaints" animationDuration={800} />
                      </LineChart>
                    </ResponsiveContainer>
                  </div>
                )}
              </div>

              {/* Top Bounce Reasons panel */}
              <BounceReasonsPanel
                data={bounceReasons}
                expandedCode={expandedBounceReason}
                onToggle={(key) => setExpandedBounceReason(prev => prev === key ? null : key)}
              />

              {/* Campaign Performance Table */}
              <div className="ac-card ig-card-hover">
                <h3><FontAwesomeIcon icon={faTrophy} /> Campaign Performance</h3>
                {!campaigns?.campaigns?.length ? (
                  <div className="ac-empty-mini">No campaign data yet.</div>
                ) : (
                  <div className="ac-table-wrap">
                    <table className="ac-table">
                      <thead>
                        <tr>
                          <th>Campaign</th>
                          <th>Sent</th>
                          <th>Open %</th>
                          <th>Click %</th>
                          <th>Hard %</th>
                          <th>Soft %</th>
                          <th>Revenue</th>
                          <th>Action</th>
                        </tr>
                      </thead>
                      <tbody>
                        {campaigns.campaigns.slice(0, 10).map(c => (
                          <tr key={c.id}>
                            <td className="ac-camp-name">
                              <span className={`ac-camp-status ${c.status}`} />
                              {c.name}
                            </td>
                            <td>{fmt(c.sent)}</td>
                            <td className={c.open_rate > 20 ? 'ac-good' : c.open_rate > 10 ? 'ac-ok' : 'ac-bad'}>{pct(c.open_rate)}</td>
                            <td className={c.click_rate > 3 ? 'ac-good' : c.click_rate > 1 ? 'ac-ok' : 'ac-bad'}>{pct(c.click_rate)}</td>
                            <td className={c.hard_bounce_rate < 2 ? 'ac-good' : c.hard_bounce_rate < 5 ? 'ac-ok' : 'ac-bad'}>{pct(c.hard_bounce_rate)}</td>
                            <td className={c.soft_bounce_rate < 2 ? 'ac-good' : c.soft_bounce_rate < 5 ? 'ac-ok' : 'ac-bad'}>{pct(c.soft_bounce_rate)}</td>
                            <td>{fmtCurrency(c.revenue)}</td>
                            <td>
                              <button
                                className="ig-btn-glow"
                                style={{ padding: '2px 8px', fontSize: '0.75em', cursor: 'pointer', background: 'transparent', border: '1px solid rgba(255,255,255,0.2)' }}
                                onClick={() => {
                                  setSelectedCampaign({ id: c.id, name: c.name });
                                  setSelectedDomain(null);
                                  document.getElementById('infra-breakdown-section')?.scrollIntoView({ behavior: 'smooth' });
                                }}
                              >
                                View Infra
                              </button>
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </div>

              {/* Brand × Series rollup */}
              <BrandSeriesRollup rows={brandSeriesRollup} />

              {/* ─── SDS Audience Health (Master List) ───────────────────────── */}
              <div id="sds-audience-health-section" className="ac-card ig-card-hover">
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '15px', flexWrap: 'wrap', gap: '10px' }}>
                  <h3 style={{ margin: 0, display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: '8px' }}>
                    <FontAwesomeIcon icon={faDatabase} /> Audience Health by Sending Domain
                    <span style={{ color: '#94a3b8', fontSize: '0.75em', fontWeight: 400 }}>
                      SDS · {fmt(sdsTotal)} total rows
                    </span>
                  </h3>
                </div>
                {sdsLoading ? (
                  <div className="ac-empty-mini"><FontAwesomeIcon icon={faSpinner} spin /> Loading SDS audience health...</div>
                ) : sdsHealth.length === 0 ? (
                  <div className="ac-empty-mini">No SDS rows yet. Backfill or wait for shadow writes to populate.</div>
                ) : (
                  <div className="ac-table-wrap">
                    <table className="ac-table">
                      <thead>
                        <tr>
                          <th>Sending Domain</th>
                          <th>Total</th>
                          <th>Cold</th>
                          <th>Warming</th>
                          <th>Engaged</th>
                          <th>Dormant</th>
                          <th>Unsub</th>
                          <th>Hard B.</th>
                          <th>Comp.</th>
                          <th>Mailed 7d</th>
                          <th>Opened 7d</th>
                        </tr>
                      </thead>
                      <tbody>
                        {sdsHealth.map((r) => (
                          <tr key={r.sending_domain}>
                            <td style={{ fontWeight: 500 }}>{r.sending_domain}</td>
                            <td>{fmt(r.total)}</td>
                            <td style={{ color: '#94a3b8' }}>{fmt(r.cold)}</td>
                            <td style={{ color: '#f59e0b' }}>{fmt(r.warming)}</td>
                            <td style={{ color: '#10b981' }}>{fmt(r.engaged)}</td>
                            <td style={{ color: '#6b7280' }}>{fmt(r.dormant)}</td>
                            <td>{fmt(r.unsubscribed)}</td>
                            <td style={{ color: r.hard_bounced > 0 ? '#ef4444' : '#475569' }}>{fmt(r.hard_bounced)}</td>
                            <td style={{ color: r.complained > 0 ? '#ef4444' : '#475569' }}>{fmt(r.complained)}</td>
                            <td>{fmt(r.recently_mailed_7d)}</td>
                            <td style={{ color: '#60a5fa' }}>{fmt(r.recently_opened_7d)}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </div>

              {/* ─── Infrastructure Breakdown ──────────────────────────────────── */}
              <div id="infra-breakdown-section" className="ac-card ig-card-hover">
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '15px', flexWrap: 'wrap', gap: '10px' }}>
                  <h3 style={{ margin: 0, display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: '8px' }}>
                    <FontAwesomeIcon icon={faDatabase} /> Infrastructure Breakdown
                    {selectedCampaign && (
                      <span style={{ background: 'rgba(59, 130, 246, 0.2)', color: '#60a5fa', padding: '2px 8px', borderRadius: '12px', fontSize: '0.75em', border: '1px solid rgba(59, 130, 246, 0.3)' }}>
                        Campaign: {selectedCampaign.name}
                      </span>
                    )}
                    {selectedDomain && (
                      <span style={{ color: '#94a3b8', fontSize: '0.85em' }}>
                        / Domain: {selectedDomain}
                      </span>
                    )}
                  </h3>

                  <div className="ac-range-selector">
                    {selectedDomain && (
                      <>
                        <button className={drilldownType === 'ip' ? 'active' : ''} onClick={() => setDrilldownType('ip')}>By IP</button>
                        <button className={drilldownType === 'isp' ? 'active' : ''} onClick={() => setDrilldownType('isp')}>By Target ISP</button>
                        <button style={{ marginLeft: '10px', background: '#334155' }} onClick={() => { setSelectedDomain(null); setDrilldownType('ip'); }}>&larr; Back to Domains</button>
                      </>
                    )}
                    {selectedCampaign && (
                      <button
                        style={{ marginLeft: selectedDomain ? '10px' : '0', background: 'rgba(239, 68, 68, 0.2)', color: '#f87171', border: '1px solid rgba(239, 68, 68, 0.3)' }}
                        onClick={() => { setSelectedCampaign(null); setSelectedDomain(null); }}
                      >
                        Clear Campaign Filter &times;
                      </button>
                    )}
                  </div>
                </div>

                {infraLoading ? (
                  <div className="ac-empty-mini"><FontAwesomeIcon icon={faSpinner} spin /> Loading infrastructure data...</div>
                ) : infraData.length === 0 ? (
                  <div className="ac-empty-mini">No infrastructure data found for this period.</div>
                ) : (
                  <div className="ac-table-wrap">
                    {selectedDomain && infraData[0]?.parent_sent != null && (
                      <div style={{ display: 'flex', gap: '20px', padding: '8px 12px', marginBottom: '10px', background: 'rgba(99,102,241,0.1)', borderRadius: '8px', fontSize: '0.85em', color: '#a5b4fc' }}>
                        <span>Domain Total Delivered: <strong style={{ color: '#e0e7ff' }}>{fmt(infraData[0].parent_delivered ?? 0)}</strong></span>
                        <span style={{ color: '#94a3b8' }}>Rates below are calculated against domain totals</span>
                      </div>
                    )}
                    <table className="ac-table">
                      <thead>
                        <tr>
                          <th>{selectedDomain ? (drilldownType === 'ip' ? 'Sending IP' : 'Target ISP') : 'Sending Domain'}</th>
                          <th>Delivered</th>
                          <th>Deferred</th>
                          <th>Opens</th>
                          <th>Clicks</th>
                          <th>Open %</th>
                          <th>Click %</th>
                          <th>Hard %</th>
                          <th>Soft %</th>
                          <th>Deferral %</th>
                          <th>Complaint %</th>
                          {!selectedDomain && <th>Action</th>}
                        </tr>
                      </thead>
                      <tbody>
                        {infraData.map((row, i) => (
                          <tr key={i}>
                            <td style={{ fontWeight: 500 }}>{row.entity}</td>
                            <td>{fmt(row.delivered)}</td>
                            <td>{fmt(row.deferred)}</td>
                            <td>{fmt(row.opens)}</td>
                            <td>{fmt(row.clicks)}</td>
                            <td className={row.open_rate > 20 ? 'ac-good' : row.open_rate > 10 ? 'ac-ok' : 'ac-bad'}>{pct(row.open_rate)}</td>
                            <td className={row.click_rate > 3 ? 'ac-good' : row.click_rate > 1 ? 'ac-ok' : 'ac-bad'}>{pct(row.click_rate)}</td>
                            <td className={row.hard_bounce_rate < 2 ? 'ac-good' : row.hard_bounce_rate < 5 ? 'ac-ok' : 'ac-bad'}>{pct(row.hard_bounce_rate)}</td>
                            <td className={row.soft_bounce_rate < 2 ? 'ac-good' : row.soft_bounce_rate < 5 ? 'ac-ok' : 'ac-bad'}>{pct(row.soft_bounce_rate)}</td>
                            <td className={row.deferral_rate < 1 ? 'ac-good' : row.deferral_rate < 5 ? 'ac-ok' : 'ac-bad'}>{pct(row.deferral_rate)}</td>
                            <td className={row.complaint_rate < 0.1 ? 'ac-good' : 'ac-bad'}>{row.complaint_rate.toFixed(2)}%</td>
                            {!selectedDomain && (
                              <td>
                                <button
                                  className="ig-btn-glow"
                                  style={{ padding: '4px 12px', fontSize: '0.85em', cursor: 'pointer', background: 'transparent', border: '1px solid #ffffff', color: '#ffffff' }}
                                  onClick={() => setSelectedDomain(row.entity)}
                                >
                                  Drilldown
                                </button>
                              </td>
                            )}
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </div>
          </div>
        </>
      )}

      {/* Deployment verification footer */}
      <div style={{
        marginTop: 32, padding: '8px 16px',
        fontSize: '0.7em', color: '#475569',
        borderTop: '1px solid rgba(255,255,255,0.05)',
        display: 'flex', gap: 16, flexWrap: 'wrap',
      }}>
        <span>Page: Analytics Center v{PAGE_VERSION}</span>
        {Object.entries(apiVersions).map(([api, ver]) => (
          <span key={api}>{api}: v{ver}</span>
        ))}
      </div>
    </div>
  );
};

export default AnalyticsCenter;
