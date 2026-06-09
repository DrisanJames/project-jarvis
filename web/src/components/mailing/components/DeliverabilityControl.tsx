import React, { useState, useEffect, useCallback, useMemo, useRef } from 'react';
import {
  AreaChart, Area, XAxis, YAxis, CartesianGrid,
  Tooltip as RechartsTooltip, Legend, ResponsiveContainer,
} from 'recharts';
import { apiFetch } from '../shared/apiFetch';

// PAGE_VERSION 2.0 (2026-04-27) — ISP Health Center expansion
//
// The screen now stacks (top → bottom):
//   1. Window selector + header KPI strip (sourced from /matrix totals)
//   2. ISP Health Center: real-time stacked-area line chart
//   3. ISP × Sending Domain matrix table
//   4. Deferral / Bounce / FBL drilldown panels
//   5. IP Activity panel (consolidates the retired Warmup screen)
//   6. Existing rate-control cards (unchanged)
//   7. Throughput calculator + variables reference (unchanged)
//
// All new sections poll their own endpoints under /api/mailing/deliverability
// and read from pmta_acct_raw — the canonical PMTA accounting firehose.
// Engagement metrics (opens / clicks) are deliberately absent: this is
// strictly a delivery-focused operator screen.
const PAGE_VERSION = '2.0';

// ---------------------------------------------------------------------------
//  Existing rate-card types (unchanged)
// ---------------------------------------------------------------------------
interface ISPConfig {
  isp: string;
  display_name: string;
  max_msg_rate: number;
  max_connections: number;
  bounce_warn_pct: number;
  bounce_action_pct: number;
  complaint_warn_pct: number;
  complaint_action_pct: number;
  pool_name: string;
  enabled: boolean;
  current_rate: number;
  rate_adjustment: number;
  backoff_count: number;
  in_recovery: boolean;
  ip_count: number;
  sent_1h: number;
  delivered_1h: number;
  hard_bounce_1h: number;
  soft_bounce_1h: number;
  deferred_1h: number;
  complained_1h: number;
}

interface ConfigData {
  configs: ISPConfig[];
  total_capacity_hr: number;
  projected_8h: number;
  api_version: string;
}

// ---------------------------------------------------------------------------
//  ISP Health Center types (v2)
// ---------------------------------------------------------------------------
type WindowKey = '1h' | '6h' | '24h';
type Metric = 'attempted' | 'delivered' | 'hard_bounce' | 'soft_bounce' | 'deferred' | 'complaint' | 'reputation_blocked';
type GroupBy = 'isp' | 'sending_domain';

interface MatrixCell {
  isp: string;
  sending_domain: string;
  attempted: number;
  delivered: number;
  hard_bounce: number;
  soft_bounce: number;
  deferred: number;
  complaint: number;
  reputation_blocked: number;
  accept_pct: number;
  defer_pct: number;
  hard_pct: number;
  soft_pct: number;
  complaint_pct: number;
}

interface MatrixResponse {
  window: string;
  cells: MatrixCell[];
  totals: MatrixCell;
  api_version: string;
}

interface TimeseriesBucket {
  bucket: string;
  series: Record<string, number>;
}

interface TimeseriesResponse {
  metric: Metric;
  group_by: GroupBy;
  window: string;
  bucket: string;
  keys: string[];
  buckets: TimeseriesBucket[];
  total: number;
}

interface DrilldownRow {
  isp: string;
  sending_domain: string;
  dsn_status: string;
  dsn_sample: string;
  count: number;
}

interface IPActivityRow {
  ip: string;
  hostname: string;
  pool: string;
  status: string;
  warmup_day: number;
  sent: number;
  delivered: number;
  hard_bounce: number;
  soft_bounce: number;
  deferred: number;
  complaint: number;
  accept_pct: number;
  last_seen_at: string | null;
}

interface IPActivityResponse {
  window: string;
  active: IPActivityRow[];
  cold: IPActivityRow[];
  paused: IPActivityRow[];
}

// ---------------------------------------------------------------------------
//  Color palettes
// ---------------------------------------------------------------------------
const ISP_COLORS: Record<string, string> = {
  gmail: '#ea4335',
  yahoo: '#6001d2',
  microsoft: '#00a4ef',
  apple: '#a2aaad',
  comcast: '#ed1c24',
  att: '#00a8e0',
  cox: '#0072ce',
  charter: '#0070c0',
  aol: '#31459b',
  sbcglobal: '#1e6091',
  other: '#64748b',
  unknown: '#475569',
};

const SENDING_DOMAIN_COLORS: Record<string, string> = {
  'em.discountblog.com': '#f59e0b',
  'em.quizfiesta.com': '#10b981',
  'em.historythinking.com': '#8b5cf6',
  'em.myownhealth.net': '#ec4899',
  'm.discountblog.com': '#f97316',
  unknown: '#475569',
};

const ISP_ORDER = ['gmail', 'microsoft', 'yahoo', 'apple', 'comcast', 'att', 'cox', 'charter'];

const METRIC_LABELS: Record<Metric, { label: string; color: string }> = {
  attempted: { label: 'Attempted', color: '#3b82f6' },
  delivered: { label: 'Delivered', color: '#10b981' },
  hard_bounce: { label: 'Hard Bounce', color: '#ef4444' },
  soft_bounce: { label: 'Soft Bounce', color: '#f59e0b' },
  deferred: { label: 'Deferred', color: '#fbbf24' },
  complaint: { label: 'Complaint', color: '#a855f7' },
  reputation_blocked: { label: 'Reputation Block', color: '#dc2626' },
};

// Polling cadence per window — user said "10s is fine or even a minute"
const POLL_INTERVAL_MS: Record<WindowKey, number> = {
  '1h': 10_000,
  '6h': 30_000,
  '24h': 60_000,
};

const THROUGHPUT_VARIABLES = [
  { name: 'max_msg_rate', scope: 'Per ISP, hourly', desc: 'Ceiling rate in mailing_engine_isp_config. ThrottleAgent adjusts down from this; never above.' },
  { name: 'warmup_daily_limit', scope: 'Per IP, daily', desc: 'Caps messages per warmup IP per day. Checked by vmtaPool.next().' },
  { name: 'currentRateAdj', scope: 'Per ISP, dynamic', desc: 'ThrottleAgent multiplier (0.0–1.0). Reacts to deferrals >20%. Recovers at +10% steps when deferrals <10%.' },
  { name: 'IP count per pool', scope: 'Per pool', desc: 'More IPs = more parallel capacity. vmtaPool round-robins across available IPs.' },
  { name: 'IP status', scope: 'Per IP', desc: 'Only active and warmup IPs are selected. Quarantined IPs are skipped.' },
  { name: 'max_connections', scope: 'Per ISP', desc: 'PMTA connection limit to ISP MX servers. Too many = blocks; too few = underutilization.' },
  { name: 'PMTA max-msg-rate', scope: 'Per VMTA/pool', desc: 'Server-side rate limit in PMTA config. Must be >= app rate or PMTA bottlenecks.' },
  { name: 'bounce_action_pct', scope: 'Per ISP', desc: 'Hard bounce rate threshold that triggers ThrottleAgent rate reduction.' },
  { name: 'complaint_action_pct', scope: 'Per ISP', desc: 'Complaint rate threshold that triggers ThrottleAgent rate reduction.' },
  { name: 'PER_IP_RATE_LIMITING', scope: 'Global env', desc: 'When enabled, ISP rate is split across IPs for per-IP token bucket sizing.' },
  { name: 'Send worker concurrency', scope: 'Global', desc: 'Number of goroutines in SendWorkerPool processing the queue.' },
  { name: 'Queue depth', scope: 'Global', desc: 'If queue is shallow, throughput is supply-limited regardless of rate settings.' },
];

// ---------------------------------------------------------------------------
//  Top-level component
// ---------------------------------------------------------------------------
export const DeliverabilityControl: React.FC = () => {
  // Existing rate-card state
  const [data, setData] = useState<ConfigData | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [autoRefresh, setAutoRefresh] = useState(true);
  const [editingRates, setEditingRates] = useState<Record<string, number>>({});
  const [saving, setSaving] = useState<Record<string, boolean>>({});
  const [showVariables, setShowVariables] = useState(false);
  const [targetVolume, setTargetVolume] = useState(36000);
  const [targetHours, setTargetHours] = useState(8);
  const [toast, setToast] = useState<{ msg: string; type: 'success' | 'error' } | null>(null);

  // ISP Health Center state
  const [windowKey, setWindowKey] = useState<WindowKey>('1h');
  const [metric, setMetric] = useState<Metric>('delivered');
  const [groupBy, setGroupBy] = useState<GroupBy>('isp');

  const refreshRef = useRef<ReturnType<typeof setInterval> | undefined>(undefined);

  const fetchData = useCallback(async () => {
    try {
      const res = await apiFetch('/api/mailing/deliverability/config');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const json: ConfigData = await res.json();
      json.configs.sort((a, b) => ISP_ORDER.indexOf(a.isp) - ISP_ORDER.indexOf(b.isp));
      setData(json);
      setError('');
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      setError(msg);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { fetchData(); }, [fetchData]);

  useEffect(() => {
    if (autoRefresh) {
      refreshRef.current = setInterval(fetchData, 15000);
      return () => clearInterval(refreshRef.current);
    }
    clearInterval(refreshRef.current);
    return undefined;
  }, [autoRefresh, fetchData]);

  useEffect(() => {
    if (!toast) return;
    const t = setTimeout(() => setToast(null), 3000);
    return () => clearTimeout(t);
  }, [toast]);

  const handleSaveRate = async (isp: string) => {
    const newRate = editingRates[isp];
    if (!newRate || newRate <= 0) return;
    setSaving(prev => ({ ...prev, [isp]: true }));
    try {
      const res = await apiFetch(`/api/mailing/deliverability/config/${isp}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ max_msg_rate: newRate }),
      });
      if (!res.ok) {
        const body = await res.json().catch(() => ({}));
        throw new Error(body.error || `HTTP ${res.status}`);
      }
      setToast({ msg: `${isp} rate updated to ${newRate}/hr`, type: 'success' });
      setEditingRates(prev => { const n = { ...prev }; delete n[isp]; return n; });
      fetchData();
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      setToast({ msg: `Failed: ${msg}`, type: 'error' });
    } finally {
      setSaving(prev => ({ ...prev, [isp]: false }));
    }
  };

  const handleResetThrottle = async (isp: string) => {
    setSaving(prev => ({ ...prev, [isp]: true }));
    try {
      const res = await apiFetch(`/api/mailing/deliverability/config/${isp}/reset-throttle`, { method: 'POST' });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const body = await res.json();
      setToast({ msg: `${isp} throttle reset — effective rate ${body.effective_rate}/hr`, type: 'success' });
      fetchData();
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      setToast({ msg: `Reset failed: ${msg}`, type: 'error' });
    } finally {
      setSaving(prev => ({ ...prev, [isp]: false }));
    }
  };

  if (loading) return <div style={styles.loading}>Loading deliverability config…</div>;
  if (error) return <div style={styles.error}>Error: {error}</div>;
  if (!data) return null;

  const requiredRate = targetHours > 0 ? Math.ceil(targetVolume / targetHours) : 0;
  const totalEffective = data.configs.reduce((sum, c) => sum + c.current_rate, 0);
  const capacityPct = requiredRate > 0 ? Math.round((totalEffective / requiredRate) * 100) : 0;

  return (
    <div style={styles.container}>
      {toast && (
        <div style={{ ...styles.toast, background: toast.type === 'success' ? '#059669' : '#dc2626' }}>
          {toast.msg}
        </div>
      )}

      {/* Header bar */}
      <div style={styles.header}>
        <div style={styles.headerLeft}>
          <h2 style={styles.title}>Deliverability ISP Health Center</h2>
          <span style={styles.version}>v{PAGE_VERSION} / API v{data.api_version}</span>
        </div>
        <div style={styles.headerStats}>
          <div style={styles.windowSelector} role="tablist" aria-label="Time window">
            {(['1h', '6h', '24h'] as WindowKey[]).map(w => (
              <button
                key={w}
                role="tab"
                aria-selected={windowKey === w}
                onClick={() => setWindowKey(w)}
                style={windowKey === w ? styles.windowBtnActive : styles.windowBtn}
              >
                {w}
              </button>
            ))}
          </div>
          <div style={styles.refreshToggle}>
            <label style={styles.refreshLabel}>
              <input type="checkbox" checked={autoRefresh} onChange={e => setAutoRefresh(e.target.checked)} />
              Auto-refresh
            </label>
          </div>
        </div>
      </div>

      {/* 1. Header KPI strip — totals from /matrix */}
      <KPIStrip windowKey={windowKey} />

      {/* 2. ISP Health Center chart */}
      <HealthCenterChart
        windowKey={windowKey}
        metric={metric}
        groupBy={groupBy}
        onMetricChange={setMetric}
        onGroupByChange={setGroupBy}
      />

      {/* 3. ISP × Sending Domain matrix */}
      <MatrixTable windowKey={windowKey} bounceActionPct={data.configs[0]?.bounce_action_pct ?? 5} complaintActionPct={data.configs[0]?.complaint_action_pct ?? 0.3} />

      {/* 4. Drilldowns */}
      <DrilldownPanel title="Deferral Drilldown" endpoint="deferrals" colHeader="DSN Status" defaultWindow="1h" />
      <DrilldownPanel title="Bounce Drilldown" endpoint="bounces" colHeader="Bounce Category" defaultWindow="24h" />
      <DrilldownPanel title="FBL Drilldown" endpoint="fbl" colHeader="DSN" defaultWindow="7d" />

      {/* 5. IP Activity panel (consolidated from Warmup Dashboard) */}
      <IPActivityPanel />

      {/* Divider */}
      <div style={styles.sectionDivider}>
        <span style={styles.sectionLabel}>Rate Controls (per ISP)</span>
      </div>

      {/* 6. Throughput calculator (existing) */}
      <div style={styles.calculator}>
        <div style={styles.calcRow}>
          <label style={styles.calcLabel}>Target Volume</label>
          <input type="number" value={targetVolume} onChange={e => setTargetVolume(Number(e.target.value))}
            style={styles.calcInput} />
          <label style={styles.calcLabel}>Hours</label>
          <input type="number" value={targetHours} onChange={e => setTargetHours(Number(e.target.value))}
            style={{ ...styles.calcInput, width: 60 }} min={1} max={24} />
          <span style={styles.calcResult}>
            Required: <strong>{fmtNum(requiredRate)}/hr</strong>
          </span>
          <span style={{ ...styles.calcResult, color: capacityPct >= 100 ? '#10b981' : '#ef4444' }}>
            Capacity: <strong>{capacityPct}%</strong>
          </span>
          <span style={styles.calcResult}>
            Effective: <strong>{fmtNum(Math.round(totalEffective))}/hr</strong>
          </span>
          <span style={styles.calcResult}>
            Ceiling: <strong>{fmtNum(data.total_capacity_hr)}/hr</strong>
          </span>
        </div>
      </div>

      {/* 7. Existing ISP rate cards — unchanged */}
      <div style={styles.grid}>
        {data.configs.map(cfg => {
          const color = ISP_COLORS[cfg.isp] || '#6b7280';
          const isYahoo = cfg.isp === 'yahoo';
          const isEditing = cfg.isp in editingRates;
          const isSaving = saving[cfg.isp];
          const adjColor = cfg.rate_adjustment >= 1.0 ? '#10b981' : cfg.rate_adjustment >= 0.7 ? '#f59e0b' : '#ef4444';
          const deliveryRate = cfg.sent_1h > 0 ? Math.round((cfg.delivered_1h / cfg.sent_1h) * 100) : 0;

          return (
            <div key={cfg.isp} style={{ ...styles.card, borderTop: `3px solid ${color}`, ...(isYahoo ? styles.yahooCard : {}) }}>
              {isYahoo && <div style={styles.yahooBadge}>TSS04 WATCH</div>}

              <div style={styles.cardHeader}>
                <span style={{ ...styles.ispName, color }}>{cfg.display_name}</span>
                <span style={{ ...styles.enabledBadge, background: cfg.enabled ? '#065f46' : '#7f1d1d' }}>
                  {cfg.enabled ? 'Enabled' : 'Disabled'}
                </span>
              </div>

              <div style={styles.rateSection}>
                <div style={styles.rateRow}>
                  <span style={styles.rateLabel}>Config Rate</span>
                  <div style={styles.rateInput}>
                    <input type="number" min={1}
                      value={isEditing ? editingRates[cfg.isp] : cfg.max_msg_rate}
                      onChange={e => setEditingRates(prev => ({ ...prev, [cfg.isp]: Number(e.target.value) }))}
                      style={styles.numberInput}
                      disabled={isSaving}
                    />
                    <span style={styles.rateUnit}>/hr</span>
                    {isEditing && editingRates[cfg.isp] !== cfg.max_msg_rate && (
                      <button onClick={() => handleSaveRate(cfg.isp)} disabled={isSaving}
                        style={styles.saveBtn}>
                        {isSaving ? '…' : 'Save'}
                      </button>
                    )}
                  </div>
                </div>
                <div style={styles.rateRow}>
                  <span style={styles.rateLabel}>Effective</span>
                  <span style={{ ...styles.rateValue, color: adjColor }}>
                    {Math.round(cfg.current_rate)}/hr
                  </span>
                </div>
                <div style={styles.rateRow}>
                  <span style={styles.rateLabel}>Adjustment</span>
                  <span style={{ ...styles.rateValue, color: adjColor }}>
                    {cfg.rate_adjustment.toFixed(2)}x
                    {cfg.rate_adjustment < 1.0 && ' — throttled'}
                    {cfg.in_recovery && ' (recovering)'}
                  </span>
                </div>
                {cfg.backoff_count > 0 && (
                  <div style={styles.rateRow}>
                    <span style={styles.rateLabel}>Backoff</span>
                    <span style={{ ...styles.rateValue, color: '#ef4444' }}>
                      Step {cfg.backoff_count}
                    </span>
                  </div>
                )}
              </div>

              <div style={styles.statsGrid}>
                <MiniStat label="Sent" value={cfg.sent_1h} />
                <MiniStat label="Delivered" value={cfg.delivered_1h} color="#10b981" />
                <MiniStat label="Hard Bounce" value={cfg.hard_bounce_1h} color={cfg.hard_bounce_1h > 0 ? '#ef4444' : undefined} />
                <MiniStat label="Soft Bounce" value={cfg.soft_bounce_1h} color={cfg.soft_bounce_1h > 0 ? '#f59e0b' : undefined} />
                <MiniStat label="Deferred" value={cfg.deferred_1h} color={cfg.deferred_1h > 0 ? '#f59e0b' : undefined} />
                <MiniStat label="Complained" value={cfg.complained_1h} color={cfg.complained_1h > 0 ? '#ef4444' : undefined} />
              </div>

              <div style={styles.cardFooter}>
                <span style={styles.footerInfo}>{cfg.ip_count} IPs • {deliveryRate}% delivery • {cfg.pool_name}</span>
                {cfg.rate_adjustment < 1.0 && (
                  <button onClick={() => handleResetThrottle(cfg.isp)} disabled={isSaving}
                    style={styles.resetBtn}>
                    Reset Throttle
                  </button>
                )}
              </div>
            </div>
          );
        })}
      </div>

      {/* Throughput variables reference */}
      <div style={styles.variablesSection}>
        <button onClick={() => setShowVariables(!showVariables)} style={styles.variablesToggle}>
          {showVariables ? '▾' : '▸'} Throughput Variables Reference ({THROUGHPUT_VARIABLES.length})
        </button>
        {showVariables && (
          <div style={styles.variablesTable}>
            <div style={styles.varHeader}>
              <span style={styles.varColName}>Variable</span>
              <span style={styles.varColScope}>Scope</span>
              <span style={styles.varColDesc}>Description</span>
            </div>
            {THROUGHPUT_VARIABLES.map(v => (
              <div key={v.name} style={styles.varRow}>
                <span style={styles.varColName}><code style={styles.code}>{v.name}</code></span>
                <span style={styles.varColScope}>{v.scope}</span>
                <span style={styles.varColDesc}>{v.desc}</span>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
};

// ---------------------------------------------------------------------------
//  KPIStrip — totals from /matrix
// ---------------------------------------------------------------------------
const KPIStrip: React.FC<{ windowKey: WindowKey }> = ({ windowKey }) => {
  const [totals, setTotals] = useState<MatrixCell | null>(null);
  const [stale, setStale] = useState(false);

  useEffect(() => {
    let cancel = false;
    const fetchIt = async () => {
      try {
        const res = await apiFetch(`/api/mailing/deliverability/matrix?window=${windowKey}`);
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const json: MatrixResponse = await res.json();
        if (!cancel) {
          setTotals(json.totals);
          setStale(false);
        }
      } catch {
        if (!cancel) setStale(true);
      }
    };
    fetchIt();
    const id = setInterval(fetchIt, POLL_INTERVAL_MS[windowKey]);
    return () => { cancel = true; clearInterval(id); };
  }, [windowKey]);

  if (!totals) {
    return <div style={styles.kpiStripLoading}>Loading totals…</div>;
  }

  return (
    <div style={{ ...styles.kpiStrip, opacity: stale ? 0.6 : 1 }}>
      <KPI label="Attempted" value={fmtNum(totals.attempted)} sub={`${windowKey} window`} />
      <KPI label="Delivered" value={fmtNum(totals.delivered)} sub={`${totals.accept_pct.toFixed(2)}%`} color="#10b981" />
      <KPI label="Hard Bounce" value={fmtNum(totals.hard_bounce)} sub={`${totals.hard_pct.toFixed(2)}%`} color={totals.hard_bounce > 0 ? '#ef4444' : '#475569'} />
      <KPI label="Soft Bounce" value={fmtNum(totals.soft_bounce)} sub={`${totals.soft_pct.toFixed(2)}%`} color={totals.soft_bounce > 0 ? '#f59e0b' : '#475569'} />
      <KPI label="Deferred" value={fmtNum(totals.deferred)} sub={`${totals.defer_pct.toFixed(2)}%`} color={totals.defer_pct > 20 ? '#f59e0b' : '#94a3b8'} />
      <KPI label="Complaint" value={fmtNum(totals.complaint)} sub={`${totals.complaint_pct.toFixed(3)}%`} color={totals.complaint > 0 ? '#a855f7' : '#475569'} />
      <KPI label="Reputation Block" value={fmtNum(totals.reputation_blocked)} sub="spam/policy" color={totals.reputation_blocked > 0 ? '#dc2626' : '#475569'} />
    </div>
  );
};

const KPI: React.FC<{ label: string; value: string; sub?: string; color?: string }> = ({ label, value, sub, color }) => (
  <div style={styles.kpiCard}>
    <div style={styles.kpiLabel}>{label}</div>
    <div style={{ ...styles.kpiValue, color: color || '#e2e8f0' }}>{value}</div>
    {sub && <div style={styles.kpiSub}>{sub}</div>}
  </div>
);

// ---------------------------------------------------------------------------
//  HealthCenterChart — Recharts AreaChart
// ---------------------------------------------------------------------------
const HealthCenterChart: React.FC<{
  windowKey: WindowKey;
  metric: Metric;
  groupBy: GroupBy;
  onMetricChange: (m: Metric) => void;
  onGroupByChange: (g: GroupBy) => void;
}> = ({ windowKey, metric, groupBy, onMetricChange, onGroupByChange }) => {
  const [data, setData] = useState<TimeseriesResponse | null>(null);
  const [stale, setStale] = useState(false);

  useEffect(() => {
    let cancel = false;
    const fetchIt = async () => {
      try {
        const res = await apiFetch(`/api/mailing/deliverability/timeseries?metric=${metric}&groupBy=${groupBy}&window=${windowKey}`);
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const json: TimeseriesResponse = await res.json();
        if (!cancel) {
          setData(json);
          setStale(false);
        }
      } catch {
        if (!cancel) setStale(true);
      }
    };
    fetchIt();
    const id = setInterval(fetchIt, POLL_INTERVAL_MS[windowKey]);
    return () => { cancel = true; clearInterval(id); };
  }, [windowKey, metric, groupBy]);

  // Flatten { bucket, series: { gmail: 100 } } → [{ ts, gmail: 100, yahoo: 50 }]
  const chartRows = useMemo(() => {
    if (!data) return [];
    return data.buckets.map(b => ({
      ts: formatBucket(b.bucket, windowKey),
      ...b.series,
    }));
  }, [data, windowKey]);

  const palette = groupBy === 'isp' ? ISP_COLORS : SENDING_DOMAIN_COLORS;
  const keys = data?.keys ?? [];

  return (
    <div style={styles.chartCard}>
      <div style={styles.chartHeader}>
        <h3 style={styles.chartTitle}>
          ISP Health Center
          <span style={styles.chartSub}>
            real-time {metric.replace('_', ' ')} by {groupBy.replace('_', ' ')} • {windowKey}
          </span>
        </h3>
        <div style={styles.chartControls}>
          <select value={metric} onChange={e => onMetricChange(e.target.value as Metric)} style={styles.select}>
            {(Object.keys(METRIC_LABELS) as Metric[]).map(m => (
              <option key={m} value={m}>{METRIC_LABELS[m].label}</option>
            ))}
          </select>
          <div style={styles.toggleGroup}>
            <button
              onClick={() => onGroupByChange('isp')}
              style={groupBy === 'isp' ? styles.toggleBtnActive : styles.toggleBtn}
            >by ISP</button>
            <button
              onClick={() => onGroupByChange('sending_domain')}
              style={groupBy === 'sending_domain' ? styles.toggleBtnActive : styles.toggleBtn}
            >by Sending Domain</button>
          </div>
        </div>
      </div>

      <div style={{ ...styles.chartBody, opacity: stale ? 0.5 : 1 }}>
        {chartRows.length === 0 ? (
          <div style={styles.chartEmpty}>No events in window.</div>
        ) : (
          <ResponsiveContainer width="100%" height={320}>
            <AreaChart data={chartRows as Array<Record<string, number | string>>} margin={{ top: 10, right: 16, left: 0, bottom: 0 }}>
              <defs>
                {keys.map(k => (
                  <linearGradient key={k} id={`grad-${k}`} x1="0" y1="0" x2="0" y2="1">
                    <stop offset="0%" stopColor={palette[k] || '#64748b'} stopOpacity={0.6} />
                    <stop offset="100%" stopColor={palette[k] || '#64748b'} stopOpacity={0.05} />
                  </linearGradient>
                ))}
              </defs>
              <CartesianGrid strokeDasharray="3 3" stroke="rgba(100,116,139,0.15)" />
              <XAxis dataKey="ts" tick={{ fontSize: 10, fill: '#64748b' }} interval="preserveStartEnd" />
              <YAxis tick={{ fontSize: 10, fill: '#64748b' }} tickFormatter={fmtNum} />
              <RechartsTooltip
                contentStyle={{ background: '#0f172a', border: '1px solid rgba(100,116,139,0.3)', borderRadius: 6, fontSize: 11 }}
                labelStyle={{ color: '#94a3b8' }}
                itemStyle={{ color: '#e2e8f0' }}
                formatter={(v: number) => fmtNum(v)}
              />
              <Legend wrapperStyle={{ fontSize: 11 }} iconType="square" />
              {keys.map(k => (
                <Area
                  key={k}
                  type="monotone"
                  dataKey={k}
                  stackId="1"
                  stroke={palette[k] || '#64748b'}
                  fill={`url(#grad-${k})`}
                  strokeWidth={1.5}
                  isAnimationActive={false}
                />
              ))}
            </AreaChart>
          </ResponsiveContainer>
        )}
      </div>
    </div>
  );
};

function formatBucket(iso: string, windowKey: WindowKey): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  if (windowKey === '24h') {
    return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false });
  }
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false });
}

// ---------------------------------------------------------------------------
//  MatrixTable — ISP × Sending Domain grid
// ---------------------------------------------------------------------------
const MatrixTable: React.FC<{ windowKey: WindowKey; bounceActionPct: number; complaintActionPct: number }> = ({ windowKey, bounceActionPct, complaintActionPct }) => {
  const [data, setData] = useState<MatrixResponse | null>(null);

  useEffect(() => {
    let cancel = false;
    const fetchIt = async () => {
      try {
        const res = await apiFetch(`/api/mailing/deliverability/matrix?window=${windowKey}`);
        if (!res.ok) return;
        const json: MatrixResponse = await res.json();
        if (!cancel) setData(json);
      } catch {
        // silent — KPIStrip will surface staleness
      }
    };
    fetchIt();
    const id = setInterval(fetchIt, POLL_INTERVAL_MS[windowKey]);
    return () => { cancel = true; clearInterval(id); };
  }, [windowKey]);

  // Pivot cells into { isp -> { domain -> cell } }
  const pivot = useMemo(() => {
    if (!data) return { isps: [] as string[], domains: [] as string[], grid: {} as Record<string, Record<string, MatrixCell>> };
    const isps = new Set<string>();
    const domains = new Set<string>();
    const grid: Record<string, Record<string, MatrixCell>> = {};
    for (const c of data.cells) {
      isps.add(c.isp);
      domains.add(c.sending_domain);
      if (!grid[c.isp]) grid[c.isp] = {};
      grid[c.isp][c.sending_domain] = c;
    }
    const ispOrder = Array.from(isps).sort((a, b) => {
      const ai = ISP_ORDER.indexOf(a);
      const bi = ISP_ORDER.indexOf(b);
      if (ai === -1 && bi === -1) return a.localeCompare(b);
      if (ai === -1) return 1;
      if (bi === -1) return -1;
      return ai - bi;
    });
    const domainOrder = Array.from(domains).sort();
    return { isps: ispOrder, domains: domainOrder, grid };
  }, [data]);

  if (!data) return <div style={styles.matrixLoading}>Loading matrix…</div>;
  if (pivot.isps.length === 0) {
    return (
      <div style={styles.chartCard}>
        <h3 style={styles.chartTitle}>ISP × Sending Domain</h3>
        <div style={styles.chartEmpty}>No events in window.</div>
      </div>
    );
  }

  return (
    <div style={styles.chartCard}>
      <div style={styles.chartHeader}>
        <h3 style={styles.chartTitle}>
          ISP × Sending Domain
          <span style={styles.chartSub}>{windowKey} • accept rate prominent, hard/soft/defer/complaint as secondary</span>
        </h3>
      </div>
      <div style={styles.matrixWrap}>
        <table style={styles.matrixTable}>
          <thead>
            <tr>
              <th style={styles.matrixIspHead}>ISP</th>
              {pivot.domains.map(d => (
                <th key={d} style={styles.matrixDomainHead}>
                  <span style={{ color: SENDING_DOMAIN_COLORS[d] || '#94a3b8' }}>●</span>{' '}{d}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {pivot.isps.map(isp => (
              <tr key={isp}>
                <td style={{ ...styles.matrixIspCell, color: ISP_COLORS[isp] || '#94a3b8' }}>{isp}</td>
                {pivot.domains.map(d => {
                  const cell = pivot.grid[isp]?.[d];
                  return (
                    <td key={d} style={styles.matrixCell}>
                      {cell ? <CellContent c={cell} bounceActionPct={bounceActionPct} complaintActionPct={complaintActionPct} /> : <span style={styles.matrixDash}>—</span>}
                    </td>
                  );
                })}
              </tr>
            ))}
            <tr style={styles.matrixTotalsRow}>
              <td style={styles.matrixIspCell}>TOTAL</td>
              {pivot.domains.map(d => {
                // Sum across ISPs for this domain
                let attempted = 0, delivered = 0, hard = 0, soft = 0, defer = 0, comp = 0;
                for (const isp of pivot.isps) {
                  const c = pivot.grid[isp]?.[d];
                  if (!c) continue;
                  attempted += c.attempted; delivered += c.delivered;
                  hard += c.hard_bounce; soft += c.soft_bounce;
                  defer += c.deferred; comp += c.complaint;
                }
                const accept = attempted > 0 ? (delivered / attempted) * 100 : 0;
                return (
                  <td key={d} style={styles.matrixCell}>
                    <div style={styles.matrixAccept}>{accept.toFixed(2)}%</div>
                    <div style={styles.matrixSecondary}>
                      attempt {fmtNum(attempted)} • h {fmtNum(hard)} • s {fmtNum(soft)} • d {fmtNum(defer)} • c {fmtNum(comp)}
                    </div>
                  </td>
                );
              })}
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  );
};

const CellContent: React.FC<{ c: MatrixCell; bounceActionPct: number; complaintActionPct: number }> = ({ c, bounceActionPct, complaintActionPct }) => {
  const acceptColor = c.accept_pct >= 99 ? '#10b981' : c.accept_pct >= 95 ? '#f59e0b' : '#ef4444';
  const hardColor = c.hard_pct >= bounceActionPct ? '#ef4444' : c.hard_pct >= bounceActionPct / 2 ? '#f59e0b' : '#475569';
  const compColor = c.complaint_pct >= complaintActionPct ? '#dc2626' : c.complaint_pct >= complaintActionPct / 2 ? '#a855f7' : '#475569';
  return (
    <>
      <div style={{ ...styles.matrixAccept, color: acceptColor }}>{c.accept_pct.toFixed(2)}%</div>
      <div style={styles.matrixSecondary}>
        attempt {fmtNum(c.attempted)} • <span style={{ color: hardColor }}>h {fmtNum(c.hard_bounce)}</span> • s {fmtNum(c.soft_bounce)} • d {fmtNum(c.deferred)} • <span style={{ color: compColor }}>c {fmtNum(c.complaint)}</span>
      </div>
    </>
  );
};

// ---------------------------------------------------------------------------
//  DrilldownPanel — collapsible deferral / bounce / FBL viewer
// ---------------------------------------------------------------------------
const DrilldownPanel: React.FC<{
  title: string;
  endpoint: 'deferrals' | 'bounces' | 'fbl';
  colHeader: string;
  defaultWindow: '1h' | '24h' | '7d';
}> = ({ title, endpoint, colHeader, defaultWindow }) => {
  const [open, setOpen] = useState(false);
  const [rows, setRows] = useState<DrilldownRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [ispFilter, setIspFilter] = useState('');
  const [domainFilter, setDomainFilter] = useState('');

  const fetchIt = useCallback(async () => {
    setLoading(true);
    try {
      const params = new URLSearchParams({ window: defaultWindow, limit: '20' });
      if (ispFilter) params.set('isp', ispFilter);
      if (domainFilter) params.set('sending_domain', domainFilter);
      const res = await apiFetch(`/api/mailing/deliverability/${endpoint}?${params.toString()}`);
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const json: { rows: DrilldownRow[] } = await res.json();
      setRows(json.rows || []);
    } catch {
      setRows([]);
    } finally {
      setLoading(false);
    }
  }, [endpoint, defaultWindow, ispFilter, domainFilter]);

  useEffect(() => {
    if (open) fetchIt();
  }, [open, fetchIt]);

  // Derive available filter chips from current rows so user only sees real options.
  const availableIsps = useMemo(() => Array.from(new Set(rows.map(r => r.isp))).sort(), [rows]);
  const availableDomains = useMemo(() => Array.from(new Set(rows.map(r => r.sending_domain))).sort(), [rows]);

  return (
    <div style={styles.drilldownCard}>
      <button onClick={() => setOpen(!open)} style={styles.drilldownToggle}>
        <span>{open ? '▾' : '▸'} {title}</span>
        <span style={styles.drilldownWindow}>{defaultWindow}</span>
      </button>
      {open && (
        <div style={styles.drilldownBody}>
          <div style={styles.filterChips}>
            <span style={styles.chipLabel}>ISP:</span>
            <button
              onClick={() => setIspFilter('')}
              style={ispFilter === '' ? styles.chipActive : styles.chip}
            >all</button>
            {availableIsps.map(i => (
              <button
                key={i}
                onClick={() => setIspFilter(ispFilter === i ? '' : i)}
                style={ispFilter === i ? styles.chipActive : styles.chip}
              >{i}</button>
            ))}
            <span style={{ ...styles.chipLabel, marginLeft: 12 }}>Domain:</span>
            <button
              onClick={() => setDomainFilter('')}
              style={domainFilter === '' ? styles.chipActive : styles.chip}
            >all</button>
            {availableDomains.map(d => (
              <button
                key={d}
                onClick={() => setDomainFilter(domainFilter === d ? '' : d)}
                style={domainFilter === d ? styles.chipActive : styles.chip}
              >{d}</button>
            ))}
            <button onClick={fetchIt} style={styles.refreshBtn}>↻</button>
          </div>

          {loading ? (
            <div style={styles.drilldownEmpty}>Loading…</div>
          ) : rows.length === 0 ? (
            <div style={styles.drilldownEmpty}>No events in window.</div>
          ) : (
            <table style={styles.drilldownTable}>
              <thead>
                <tr>
                  <th style={styles.drilldownTh}>ISP</th>
                  <th style={styles.drilldownTh}>Sending Domain</th>
                  <th style={styles.drilldownTh}>{colHeader}</th>
                  <th style={styles.drilldownTh}>Sample Diagnostic</th>
                  <th style={{ ...styles.drilldownTh, textAlign: 'right' }}>Count</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r, i) => (
                  <tr key={`${r.isp}-${r.sending_domain}-${r.dsn_status}-${i}`}>
                    <td style={{ ...styles.drilldownTd, color: ISP_COLORS[r.isp] || '#94a3b8' }}>{r.isp}</td>
                    <td style={{ ...styles.drilldownTd, color: SENDING_DOMAIN_COLORS[r.sending_domain] || '#94a3b8' }}>{r.sending_domain}</td>
                    <td style={styles.drilldownTd}>{r.dsn_status || '—'}</td>
                    <td style={{ ...styles.drilldownTd, color: '#64748b', maxWidth: 360, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }} title={r.dsn_sample}>
                      {r.dsn_sample || '—'}
                    </td>
                    <td style={{ ...styles.drilldownTd, textAlign: 'right', fontWeight: 600 }}>{fmtNum(r.count)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      )}
    </div>
  );
};

// ---------------------------------------------------------------------------
//  IPActivityPanel — replaces Warmup Dashboard
// ---------------------------------------------------------------------------
const IPActivityPanel: React.FC = () => {
  const [data, setData] = useState<IPActivityResponse | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancel = false;
    const fetchIt = async () => {
      try {
        const res = await apiFetch('/api/mailing/deliverability/ip-activity?window=24h');
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const json: IPActivityResponse = await res.json();
        if (!cancel) {
          setData(json);
          setLoading(false);
        }
      } catch {
        if (!cancel) setLoading(false);
      }
    };
    fetchIt();
    const id = setInterval(fetchIt, 60_000);
    return () => { cancel = true; clearInterval(id); };
  }, []);

  if (loading) return <div style={styles.matrixLoading}>Loading IP activity…</div>;
  if (!data) return null;

  return (
    <div style={styles.chartCard}>
      <div style={styles.chartHeader}>
        <h3 style={styles.chartTitle}>
          IP Activity
          <span style={styles.chartSub}>
            24h window • Active = sent &gt; 0, Cold = no traffic, Paused = paused/quarantined/reserved
          </span>
        </h3>
      </div>
      <div style={styles.ipColumns}>
        <IPColumn title="Active" color="#10b981" rows={data.active} emptyMsg="No active IPs in 24h." />
        <IPColumn title="Cold" color="#94a3b8" rows={data.cold} emptyMsg="No cold IPs (everything sent in 24h)." />
        <IPColumn title="Paused / Quarantined" color="#ef4444" rows={data.paused} emptyMsg="No paused IPs." />
      </div>
    </div>
  );
};

const IPColumn: React.FC<{ title: string; color: string; rows: IPActivityRow[]; emptyMsg: string }> = ({ title, color, rows, emptyMsg }) => (
  <div style={styles.ipColumn}>
    <div style={{ ...styles.ipColumnHeader, borderTopColor: color }}>
      <span style={{ color }}>● {title}</span>
      <span style={styles.ipColumnCount}>{rows.length}</span>
    </div>
    {rows.length === 0 ? (
      <div style={styles.ipEmpty}>{emptyMsg}</div>
    ) : (
      <div style={styles.ipList}>
        {rows.map(ip => (
          <div key={ip.ip} style={styles.ipRow}>
            <div style={styles.ipRowHead}>
              <span style={styles.ipAddr}>{ip.ip}</span>
              <span style={{ ...styles.ipStatus, color: ipStatusColor(ip.status) }}>{ip.status}</span>
            </div>
            <div style={styles.ipRowMeta}>
              {ip.hostname || '—'} • {ip.pool || '—'}{ip.warmup_day > 0 && ` • day ${ip.warmup_day}`}
            </div>
            {ip.sent > 0 ? (
              <div style={styles.ipRowMetrics}>
                <span>sent <strong>{fmtNum(ip.sent)}</strong></span>
                <span style={{ color: ip.accept_pct >= 99 ? '#10b981' : ip.accept_pct >= 95 ? '#f59e0b' : '#ef4444' }}>
                  {ip.accept_pct.toFixed(2)}%
                </span>
                {ip.hard_bounce > 0 && <span style={{ color: '#ef4444' }}>h {fmtNum(ip.hard_bounce)}</span>}
                {ip.soft_bounce > 0 && <span style={{ color: '#f59e0b' }}>s {fmtNum(ip.soft_bounce)}</span>}
                {ip.deferred > 0 && <span>d {fmtNum(ip.deferred)}</span>}
                {ip.complaint > 0 && <span style={{ color: '#a855f7' }}>c {fmtNum(ip.complaint)}</span>}
              </div>
            ) : (
              <div style={styles.ipRowMetricsCold}>
                {ip.last_seen_at ? `last seen ${new Date(ip.last_seen_at).toLocaleString()}` : 'never seen in PMTA accounting'}
              </div>
            )}
          </div>
        ))}
      </div>
    )}
  </div>
);

function ipStatusColor(status: string): string {
  switch (status) {
    case 'active': return '#10b981';
    case 'warmup': return '#f59e0b';
    case 'cold': return '#94a3b8';
    case 'paused': return '#ef4444';
    case 'quarantined': return '#dc2626';
    default: return '#64748b';
  }
}

const StatBoxUnused: React.FC<{ label: string; value: string; color?: string }> = ({ label, value, color }) => (
  <div style={styles.statBox}>
    <div style={styles.statLabel}>{label}</div>
    <div style={{ ...styles.statValue, color: color || '#e2e8f0' }}>{value}</div>
  </div>
);
// Suppress unused warning for backwards-compat helper kept for potential future use.
void StatBoxUnused;

const MiniStat: React.FC<{ label: string; value: number; color?: string }> = ({ label, value, color }) => (
  <div style={styles.miniStat}>
    <span style={styles.miniLabel}>{label}</span>
    <span style={{ ...styles.miniValue, color: color || '#94a3b8' }}>{fmtNum(value)}</span>
  </div>
);

function fmtNum(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M';
  if (n >= 1_000) return (n / 1_000).toFixed(1) + 'K';
  return n.toLocaleString();
}

const styles: Record<string, React.CSSProperties> = {
  container: { padding: '24px 32px', maxWidth: 1400, margin: '0 auto', fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif' },
  loading: { textAlign: 'center', padding: 80, color: '#94a3b8', fontSize: 14 },
  error: { textAlign: 'center', padding: 80, color: '#ef4444', fontSize: 14 },
  toast: { position: 'fixed', top: 20, right: 20, padding: '10px 20px', borderRadius: 8, color: '#fff', fontSize: 13, fontWeight: 600, zIndex: 9999, boxShadow: '0 4px 12px rgba(0,0,0,0.3)' },
  header: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 20, flexWrap: 'wrap' as const, gap: 12 },
  headerLeft: { display: 'flex', alignItems: 'baseline', gap: 12 },
  title: { margin: 0, fontSize: 22, fontWeight: 700, color: '#e2e8f0' },
  version: { fontSize: 11, color: '#64748b' },
  headerStats: { display: 'flex', gap: 16, alignItems: 'center', flexWrap: 'wrap' as const },
  windowSelector: { display: 'inline-flex', background: 'rgba(15,23,42,0.7)', borderRadius: 8, padding: 3, gap: 2 },
  windowBtn: { background: 'transparent', border: 'none', color: '#94a3b8', padding: '6px 14px', borderRadius: 6, cursor: 'pointer', fontSize: 12, fontWeight: 600 },
  windowBtnActive: { background: 'rgba(59,130,246,0.2)', border: 'none', color: '#3b82f6', padding: '6px 14px', borderRadius: 6, cursor: 'pointer', fontSize: 12, fontWeight: 700 },
  refreshToggle: { marginLeft: 8 },
  refreshLabel: { fontSize: 12, color: '#94a3b8', cursor: 'pointer', display: 'flex', alignItems: 'center', gap: 4 },

  kpiStrip: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(140px, 1fr))', gap: 12, marginBottom: 20, transition: 'opacity 0.3s' },
  kpiStripLoading: { padding: 24, textAlign: 'center' as const, color: '#64748b', background: 'rgba(15,23,42,0.4)', borderRadius: 10, marginBottom: 20, fontSize: 12 },
  kpiCard: { background: 'rgba(15,23,42,0.7)', borderRadius: 10, padding: '12px 16px', border: '1px solid rgba(100,116,139,0.15)' },
  kpiLabel: { fontSize: 10, color: '#64748b', textTransform: 'uppercase' as const, letterSpacing: 0.5, marginBottom: 4 },
  kpiValue: { fontSize: 20, fontWeight: 700, fontVariantNumeric: 'tabular-nums' },
  kpiSub: { fontSize: 10, color: '#94a3b8', marginTop: 2 },

  chartCard: { background: 'rgba(15,23,42,0.5)', borderRadius: 12, padding: '16px 20px', marginBottom: 20, border: '1px solid rgba(100,116,139,0.15)' },
  chartHeader: { display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: 12, flexWrap: 'wrap' as const, gap: 12 },
  chartTitle: { margin: 0, fontSize: 15, fontWeight: 700, color: '#e2e8f0', display: 'flex', alignItems: 'baseline', gap: 12, flexWrap: 'wrap' as const },
  chartSub: { fontSize: 11, fontWeight: 400, color: '#64748b' },
  chartControls: { display: 'flex', gap: 10, alignItems: 'center', flexWrap: 'wrap' as const },
  chartBody: { transition: 'opacity 0.3s' },
  chartEmpty: { padding: 60, textAlign: 'center' as const, color: '#475569', fontSize: 12 },
  select: { background: 'rgba(15,23,42,0.7)', border: '1px solid rgba(100,116,139,0.3)', color: '#e2e8f0', padding: '6px 10px', borderRadius: 6, fontSize: 12, fontWeight: 600 },
  toggleGroup: { display: 'inline-flex', background: 'rgba(15,23,42,0.7)', borderRadius: 6, padding: 2, gap: 2 },
  toggleBtn: { background: 'transparent', border: 'none', color: '#94a3b8', padding: '5px 10px', borderRadius: 4, cursor: 'pointer', fontSize: 11, fontWeight: 600 },
  toggleBtnActive: { background: 'rgba(59,130,246,0.2)', border: 'none', color: '#3b82f6', padding: '5px 10px', borderRadius: 4, cursor: 'pointer', fontSize: 11, fontWeight: 700 },

  matrixWrap: { overflowX: 'auto' as const },
  matrixLoading: { padding: 40, textAlign: 'center' as const, color: '#64748b', fontSize: 12 },
  matrixTable: { width: '100%', borderCollapse: 'collapse' as const, fontSize: 11 },
  matrixIspHead: { textAlign: 'left' as const, padding: '8px 10px', color: '#64748b', fontWeight: 700, textTransform: 'uppercase' as const, fontSize: 10, letterSpacing: 0.5, borderBottom: '1px solid rgba(100,116,139,0.2)' },
  matrixDomainHead: { textAlign: 'left' as const, padding: '8px 10px', color: '#94a3b8', fontWeight: 600, fontSize: 11, borderBottom: '1px solid rgba(100,116,139,0.2)' },
  matrixIspCell: { padding: '8px 10px', fontWeight: 700, fontSize: 12, borderBottom: '1px solid rgba(100,116,139,0.08)' },
  matrixCell: { padding: '8px 10px', borderBottom: '1px solid rgba(100,116,139,0.08)', verticalAlign: 'top' as const },
  matrixAccept: { fontSize: 14, fontWeight: 700, fontVariantNumeric: 'tabular-nums' },
  matrixSecondary: { fontSize: 10, color: '#64748b', marginTop: 2, fontVariantNumeric: 'tabular-nums' },
  matrixDash: { color: '#475569' },
  matrixTotalsRow: { background: 'rgba(15,23,42,0.5)', borderTop: '2px solid rgba(100,116,139,0.3)' },

  drilldownCard: { background: 'rgba(15,23,42,0.5)', borderRadius: 10, marginBottom: 12, border: '1px solid rgba(100,116,139,0.1)' },
  drilldownToggle: { width: '100%', background: 'transparent', border: 'none', color: '#cbd5e1', padding: '12px 18px', textAlign: 'left' as const, cursor: 'pointer', fontSize: 13, fontWeight: 700, display: 'flex', justifyContent: 'space-between', alignItems: 'center' },
  drilldownWindow: { fontSize: 11, color: '#64748b', fontWeight: 500 },
  drilldownBody: { padding: '0 18px 16px 18px' },
  drilldownEmpty: { padding: 30, textAlign: 'center' as const, color: '#64748b', fontSize: 12 },
  drilldownTable: { width: '100%', borderCollapse: 'collapse' as const, fontSize: 11 },
  drilldownTh: { textAlign: 'left' as const, padding: '6px 8px', color: '#64748b', fontWeight: 700, fontSize: 10, textTransform: 'uppercase' as const, letterSpacing: 0.5, borderBottom: '1px solid rgba(100,116,139,0.2)' },
  drilldownTd: { padding: '6px 8px', color: '#cbd5e1', fontVariantNumeric: 'tabular-nums', borderBottom: '1px solid rgba(100,116,139,0.06)' },
  filterChips: { display: 'flex', gap: 6, alignItems: 'center', flexWrap: 'wrap' as const, paddingTop: 6, paddingBottom: 12 },
  chipLabel: { fontSize: 10, color: '#475569', textTransform: 'uppercase' as const, letterSpacing: 0.5 },
  chip: { background: 'rgba(30,41,59,0.6)', border: '1px solid rgba(100,116,139,0.2)', color: '#94a3b8', padding: '3px 10px', borderRadius: 12, fontSize: 11, cursor: 'pointer' },
  chipActive: { background: 'rgba(59,130,246,0.2)', border: '1px solid rgba(59,130,246,0.4)', color: '#3b82f6', padding: '3px 10px', borderRadius: 12, fontSize: 11, cursor: 'pointer', fontWeight: 700 },
  refreshBtn: { marginLeft: 'auto', background: 'rgba(30,41,59,0.6)', border: '1px solid rgba(100,116,139,0.2)', color: '#94a3b8', padding: '3px 10px', borderRadius: 6, fontSize: 11, cursor: 'pointer' },

  ipColumns: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(280px, 1fr))', gap: 12 },
  ipColumn: { background: 'rgba(15,23,42,0.6)', borderRadius: 10, border: '1px solid rgba(100,116,139,0.1)', overflow: 'hidden' },
  ipColumnHeader: { padding: '10px 14px', borderTop: '3px solid', display: 'flex', justifyContent: 'space-between', alignItems: 'center', fontSize: 12, fontWeight: 700 },
  ipColumnCount: { color: '#64748b', fontSize: 11 },
  ipList: { maxHeight: 340, overflowY: 'auto' as const, padding: '0 12px 8px 12px' },
  ipRow: { padding: '8px 0', borderBottom: '1px solid rgba(100,116,139,0.08)' },
  ipRowHead: { display: 'flex', justifyContent: 'space-between', alignItems: 'baseline' },
  ipAddr: { fontSize: 12, fontWeight: 700, color: '#e2e8f0', fontFamily: '"SF Mono", "Fira Code", monospace' },
  ipStatus: { fontSize: 9, fontWeight: 700, textTransform: 'uppercase' as const, letterSpacing: 0.5 },
  ipRowMeta: { fontSize: 10, color: '#64748b', marginTop: 2 },
  ipRowMetrics: { fontSize: 10, color: '#94a3b8', marginTop: 4, display: 'flex', gap: 10, flexWrap: 'wrap' as const, fontVariantNumeric: 'tabular-nums' },
  ipRowMetricsCold: { fontSize: 10, color: '#475569', fontStyle: 'italic' as const, marginTop: 4 },
  ipEmpty: { padding: 20, textAlign: 'center' as const, color: '#475569', fontSize: 11 },

  sectionDivider: { display: 'flex', alignItems: 'center', gap: 12, marginTop: 24, marginBottom: 12 },
  sectionLabel: { fontSize: 11, fontWeight: 700, color: '#475569', textTransform: 'uppercase' as const, letterSpacing: 1 },

  statBox: { background: 'rgba(30,41,59,0.7)', borderRadius: 8, padding: '8px 16px', textAlign: 'center' as const },
  statLabel: { fontSize: 10, color: '#64748b', textTransform: 'uppercase' as const, letterSpacing: 0.5 },
  statValue: { fontSize: 18, fontWeight: 700, fontVariantNumeric: 'tabular-nums' },

  calculator: { background: 'rgba(30,41,59,0.5)', borderRadius: 10, padding: '12px 20px', marginBottom: 20, border: '1px solid rgba(100,116,139,0.2)' },
  calcRow: { display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' as const },
  calcLabel: { fontSize: 12, color: '#94a3b8', fontWeight: 600 },
  calcInput: { background: 'rgba(15,23,42,0.6)', border: '1px solid rgba(100,116,139,0.3)', borderRadius: 6, padding: '6px 10px', color: '#e2e8f0', fontSize: 14, width: 120, fontVariantNumeric: 'tabular-nums' },
  calcResult: { fontSize: 13, color: '#94a3b8' },
  grid: { display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(320px, 1fr))', gap: 16, marginBottom: 24 },
  card: { background: 'rgba(15,23,42,0.7)', borderRadius: 10, padding: 16, border: '1px solid rgba(100,116,139,0.15)', transition: 'border-color 0.2s' },
  yahooCard: { boxShadow: '0 0 0 1px rgba(96,1,210,0.3), 0 0 20px rgba(96,1,210,0.08)' },
  yahooBadge: { fontSize: 9, fontWeight: 700, color: '#fbbf24', background: 'rgba(251,191,36,0.1)', padding: '2px 8px', borderRadius: 4, marginBottom: 8, display: 'inline-block', letterSpacing: 1 },
  cardHeader: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 },
  ispName: { fontSize: 16, fontWeight: 700 },
  enabledBadge: { fontSize: 10, padding: '2px 8px', borderRadius: 10, color: '#fff', fontWeight: 600 },
  rateSection: { marginBottom: 12, padding: '8px 0', borderTop: '1px solid rgba(100,116,139,0.1)', borderBottom: '1px solid rgba(100,116,139,0.1)' },
  rateRow: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '3px 0' },
  rateLabel: { fontSize: 11, color: '#64748b', textTransform: 'uppercase' as const, letterSpacing: 0.5 },
  rateInput: { display: 'flex', alignItems: 'center', gap: 6 },
  numberInput: { background: 'rgba(30,41,59,0.8)', border: '1px solid rgba(100,116,139,0.3)', borderRadius: 4, padding: '4px 8px', color: '#e2e8f0', fontSize: 14, width: 80, textAlign: 'right' as const, fontVariantNumeric: 'tabular-nums' },
  rateUnit: { fontSize: 11, color: '#64748b' },
  rateValue: { fontSize: 14, fontWeight: 600, fontVariantNumeric: 'tabular-nums' },
  saveBtn: { background: '#2563eb', color: '#fff', border: 'none', borderRadius: 4, padding: '4px 10px', fontSize: 11, fontWeight: 600, cursor: 'pointer' },
  statsGrid: { display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 4, marginBottom: 10 },
  miniStat: { display: 'flex', flexDirection: 'column' as const, alignItems: 'center', padding: '4px 0' },
  miniLabel: { fontSize: 9, color: '#475569', textTransform: 'uppercase' as const, letterSpacing: 0.5 },
  miniValue: { fontSize: 13, fontWeight: 600, fontVariantNumeric: 'tabular-nums' },
  cardFooter: { display: 'flex', justifyContent: 'space-between', alignItems: 'center', paddingTop: 8, borderTop: '1px solid rgba(100,116,139,0.1)' },
  footerInfo: { fontSize: 10, color: '#475569' },
  resetBtn: { background: 'rgba(239,68,68,0.15)', color: '#ef4444', border: '1px solid rgba(239,68,68,0.3)', borderRadius: 4, padding: '4px 10px', fontSize: 10, fontWeight: 600, cursor: 'pointer' },
  variablesSection: { marginTop: 8, borderTop: '1px solid rgba(100,116,139,0.1)', paddingTop: 12 },
  variablesToggle: { background: 'none', border: 'none', color: '#94a3b8', fontSize: 13, cursor: 'pointer', fontWeight: 600, padding: '4px 0' },
  variablesTable: { marginTop: 8, background: 'rgba(15,23,42,0.5)', borderRadius: 8, overflow: 'hidden', border: '1px solid rgba(100,116,139,0.1)' },
  varHeader: { display: 'grid', gridTemplateColumns: '180px 120px 1fr', padding: '8px 12px', background: 'rgba(30,41,59,0.6)', fontSize: 10, fontWeight: 700, color: '#64748b', textTransform: 'uppercase' as const, letterSpacing: 0.5 },
  varRow: { display: 'grid', gridTemplateColumns: '180px 120px 1fr', padding: '6px 12px', borderTop: '1px solid rgba(100,116,139,0.08)', fontSize: 12, color: '#94a3b8' },
  varColName: { fontWeight: 600, color: '#cbd5e1' },
  varColScope: { color: '#64748b' },
  varColDesc: {},
  code: { background: 'rgba(100,116,139,0.15)', padding: '1px 4px', borderRadius: 3, fontSize: 11, fontFamily: '"SF Mono", "Fira Code", monospace' },
};
