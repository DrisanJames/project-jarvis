import React, { useState, useEffect, useCallback, useRef } from 'react';

const PAGE_VERSION = '1.0';

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

const ISP_COLORS: Record<string, string> = {
  gmail: '#ea4335',
  yahoo: '#6001d2',
  microsoft: '#00a4ef',
  apple: '#a2aaad',
  comcast: '#ed1c24',
  att: '#00a8e0',
  cox: '#0072ce',
  charter: '#0070c0',
};

const ISP_ORDER = ['gmail', 'microsoft', 'yahoo', 'apple', 'comcast', 'att', 'cox', 'charter'];

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

export const DeliverabilityControl: React.FC = () => {
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
  const refreshRef = useRef<ReturnType<typeof setInterval>>();

  const fetchData = useCallback(async () => {
    try {
      const res = await fetch('/api/mailing/deliverability/config');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const json: ConfigData = await res.json();
      json.configs.sort((a, b) => ISP_ORDER.indexOf(a.isp) - ISP_ORDER.indexOf(b.isp));
      setData(json);
      setError('');
    } catch (e: any) {
      setError(e.message);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { fetchData(); }, [fetchData]);

  useEffect(() => {
    if (autoRefresh) {
      refreshRef.current = setInterval(fetchData, 15000);
      return () => clearInterval(refreshRef.current);
    } else {
      clearInterval(refreshRef.current);
    }
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
      const res = await fetch(`/api/mailing/deliverability/config/${isp}`, {
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
    } catch (e: any) {
      setToast({ msg: `Failed: ${e.message}`, type: 'error' });
    } finally {
      setSaving(prev => ({ ...prev, [isp]: false }));
    }
  };

  const handleResetThrottle = async (isp: string) => {
    setSaving(prev => ({ ...prev, [isp]: true }));
    try {
      const res = await fetch(`/api/mailing/deliverability/config/${isp}/reset-throttle`, { method: 'POST' });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const body = await res.json();
      setToast({ msg: `${isp} throttle reset — effective rate ${body.effective_rate}/hr`, type: 'success' });
      fetchData();
    } catch (e: any) {
      setToast({ msg: `Reset failed: ${e.message}`, type: 'error' });
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
          <h2 style={styles.title}>Deliverability Control</h2>
          <span style={styles.version}>v{PAGE_VERSION} / API v{data.api_version}</span>
        </div>
        <div style={styles.headerStats}>
          <StatBox label="Config Ceiling" value={`${fmtNum(data.total_capacity_hr)}/hr`} />
          <StatBox label="Effective Rate" value={`${fmtNum(Math.round(totalEffective))}/hr`} color={totalEffective < data.total_capacity_hr * 0.8 ? '#f59e0b' : '#10b981'} />
          <StatBox label="Projected 8h" value={fmtNum(Math.round(totalEffective * 8))} color={totalEffective * 8 >= targetVolume ? '#10b981' : '#ef4444'} />
          <div style={styles.refreshToggle}>
            <label style={styles.refreshLabel}>
              <input type="checkbox" checked={autoRefresh} onChange={e => setAutoRefresh(e.target.checked)} />
              Auto-refresh
            </label>
          </div>
        </div>
      </div>

      {/* Throughput calculator */}
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
        </div>
      </div>

      {/* ISP rate cards */}
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

              {/* Rate controls */}
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

              {/* Stats */}
              <div style={styles.statsGrid}>
                <MiniStat label="Sent" value={cfg.sent_1h} />
                <MiniStat label="Delivered" value={cfg.delivered_1h} color="#10b981" />
                <MiniStat label="Hard Bounce" value={cfg.hard_bounce_1h} color={cfg.hard_bounce_1h > 0 ? '#ef4444' : undefined} />
                <MiniStat label="Soft Bounce" value={cfg.soft_bounce_1h} color={cfg.soft_bounce_1h > 0 ? '#f59e0b' : undefined} />
                <MiniStat label="Deferred" value={cfg.deferred_1h} color={cfg.deferred_1h > 0 ? '#f59e0b' : undefined} />
                <MiniStat label="Complained" value={cfg.complained_1h} color={cfg.complained_1h > 0 ? '#ef4444' : undefined} />
              </div>

              {/* Footer */}
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

const StatBox: React.FC<{ label: string; value: string; color?: string }> = ({ label, value, color }) => (
  <div style={styles.statBox}>
    <div style={styles.statLabel}>{label}</div>
    <div style={{ ...styles.statValue, color: color || '#e2e8f0' }}>{value}</div>
  </div>
);

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
  statBox: { background: 'rgba(30,41,59,0.7)', borderRadius: 8, padding: '8px 16px', textAlign: 'center' as const },
  statLabel: { fontSize: 10, color: '#64748b', textTransform: 'uppercase' as const, letterSpacing: 0.5 },
  statValue: { fontSize: 18, fontWeight: 700, fontVariantNumeric: 'tabular-nums' },
  refreshToggle: { marginLeft: 8 },
  refreshLabel: { fontSize: 12, color: '#94a3b8', cursor: 'pointer', display: 'flex', alignItems: 'center', gap: 4 },
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
