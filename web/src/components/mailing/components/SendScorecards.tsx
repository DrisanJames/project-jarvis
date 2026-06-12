import React, { useState, useEffect, useCallback, useMemo } from 'react';
import {
  ResponsiveContainer, ComposedChart, Bar, Line, XAxis, YAxis,
  Tooltip, Legend, CartesianGrid,
} from 'recharts';
import { apiFetch } from '../shared/apiFetch';

// ── Types (mirror internal/api/send_baselines.go JSON) ───────────────────────

interface WindowStats {
  sends: number;
  delivered: number;
  human_opens: number;
  human_clicks: number;
  hard_bounces: number;
  soft_bounces: number;
  complaints: number;
  days_with_sends: number;
  daily_avg_sends: number;
  human_open_rate: number;
  click_rate: number;
  hard_bounce_rate: number;
  soft_bounce_rate: number;
  complaint_rate: number;
}

type Verdict = 'INCREASE' | 'MAINTAIN' | 'DECREASE';

interface BaselineRow {
  sending_domain: string;
  isp: string;
  baseline_daily_sends: number;
  window_days: number;
  active_days: number;
  recent: WindowStats;
  prior: WindowStats;
  verdict: Verdict;
  reasons: string[];
}

interface BaselinesResponse {
  window_days: number;
  generated_at: string;
  summary: { increase: number; maintain: number; decrease: number };
  rows: BaselineRow[];
}

interface PaceRow {
  sending_domain: string;
  isp: string;
  today_sends: number;
  baseline_daily_sends: number;
  pace_pct: number;
  verdict: Verdict;
  alert: boolean;
}

interface TodayResponse {
  day: string;
  day_fraction: number;
  generated_at: string;
  rows: PaceRow[];
}

interface ScorecardRow {
  day: string;
  sending_domain: string;
  offer_name: string;
  isp: string;
  sent: number;
  delivered: number;
  opens: number;
  human_opens: number;
  clicks: number;
  hard_bounces: number;
  soft_bounces: number;
  conversions: number;
}

interface ScorecardsResponse {
  days: number;
  generated_at: string;
  total_rows: number;
  truncated: boolean;
  rows: ScorecardRow[];
}

// ── Theme ─────────────────────────────────────────────────────────────────────

const C = {
  heading: '#dbeafe',
  sub: '#94a3b8',
  tableBg: 'rgba(15,30,60,0.35)',
  border: 'rgba(99,102,241,0.25)',
  indigo: '#6366f1',
  green: '#22c55e',
  amber: '#f59e0b',
  red: '#ef4444',
  gray: '#9ca3af',
};

const verdictColors: Record<Verdict, { fg: string; bg: string }> = {
  INCREASE: { fg: C.green, bg: 'rgba(34,197,94,0.15)' },
  MAINTAIN: { fg: C.gray, bg: 'rgba(156,163,175,0.15)' },
  DECREASE: { fg: C.red, bg: 'rgba(239,68,68,0.15)' },
};

const containerStyle: React.CSSProperties = {
  padding: '24px',
  color: '#e2e8f0',
  fontFamily: 'inherit',
};

const cardStyle: React.CSSProperties = {
  background: C.tableBg,
  border: `1px solid ${C.border}`,
  borderRadius: 12,
  padding: 16,
  marginBottom: 16,
};

const tableStyle: React.CSSProperties = {
  width: '100%',
  borderCollapse: 'collapse',
  fontSize: 13,
};

const thStyle: React.CSSProperties = {
  textAlign: 'left',
  padding: '8px 10px',
  color: C.sub,
  fontWeight: 600,
  fontSize: 11,
  textTransform: 'uppercase',
  letterSpacing: '0.05em',
  borderBottom: `1px solid ${C.border}`,
  whiteSpace: 'nowrap',
};

const tdStyle: React.CSSProperties = {
  padding: '8px 10px',
  borderBottom: '1px solid rgba(99,102,241,0.1)',
  whiteSpace: 'nowrap',
};

const inputStyle: React.CSSProperties = {
  background: 'rgba(15,23,42,0.8)',
  border: `1px solid ${C.border}`,
  borderRadius: 8,
  color: '#e2e8f0',
  padding: '6px 10px',
  fontSize: 13,
  outline: 'none',
};

const fmtPct = (v: number): string => `${(v * 100).toFixed(2)}%`;
const fmtNum = (v: number): string => v.toLocaleString();

const VerdictPill: React.FC<{ verdict: Verdict }> = ({ verdict }) => {
  const { fg, bg } = verdictColors[verdict];
  return (
    <span style={{
      color: fg, background: bg, border: `1px solid ${fg}40`,
      borderRadius: 999, padding: '2px 10px', fontSize: 11, fontWeight: 700,
      letterSpacing: '0.05em',
    }}>
      {verdict}
    </span>
  );
};

const TabButton: React.FC<{ active: boolean; onClick: () => void; label: string }> = ({ active, onClick, label }) => (
  <button
    onClick={onClick}
    style={{
      background: active ? 'rgba(99,102,241,0.25)' : 'transparent',
      border: `1px solid ${active ? C.indigo : C.border}`,
      color: active ? C.heading : C.sub,
      borderRadius: 8,
      padding: '8px 16px',
      fontSize: 13,
      fontWeight: 600,
      cursor: 'pointer',
    }}
  >
    {label}
  </button>
);

// ── Tab 1: Baselines & Verdicts ───────────────────────────────────────────────

const BaselinesTab: React.FC = () => {
  const [data, setData] = useState<BaselinesResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [domainFilter, setDomainFilter] = useState('');
  const [expanded, setExpanded] = useState<Set<string>>(new Set());

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await apiFetch('/api/mailing/send-baselines?days=28');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      setData(await res.json() as BaselinesResponse);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'failed to load baselines');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  const domains = useMemo(() => {
    const set = new Set<string>();
    (data?.rows ?? []).forEach(r => set.add(r.sending_domain));
    return Array.from(set).sort();
  }, [data]);

  const rows = useMemo(
    () => (data?.rows ?? []).filter(r => domainFilter === '' || r.sending_domain === domainFilter),
    [data, domainFilter],
  );

  const toggle = (key: string) => {
    setExpanded(prev => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key); else next.add(key);
      return next;
    });
  };

  if (loading) return <div style={{ color: C.sub, padding: 24 }}>Loading baselines…</div>;
  if (error) return <div style={{ color: C.red, padding: 24 }}>Error: {error}</div>;
  if (!data) return null;

  return (
    <div>
      {/* Summary strip */}
      <div style={{ display: 'flex', gap: 12, marginBottom: 16, flexWrap: 'wrap' }}>
        {([
          ['DECREASE', data.summary.decrease],
          ['MAINTAIN', data.summary.maintain],
          ['INCREASE', data.summary.increase],
        ] as Array<[Verdict, number]>).map(([v, n]) => (
          <div key={v} style={{ ...cardStyle, marginBottom: 0, padding: '12px 20px', minWidth: 140 }}>
            <div style={{ fontSize: 24, fontWeight: 700, color: verdictColors[v].fg }}>{n}</div>
            <div style={{ fontSize: 11, color: C.sub, textTransform: 'uppercase', letterSpacing: '0.05em' }}>{v}</div>
          </div>
        ))}
        <div style={{ marginLeft: 'auto', display: 'flex', gap: 8, alignItems: 'center' }}>
          <select value={domainFilter} onChange={e => setDomainFilter(e.target.value)} style={inputStyle}>
            <option value="">All domains</option>
            {domains.map(d => <option key={d} value={d}>{d}</option>)}
          </select>
          <button onClick={() => { void load(); }} style={{ ...inputStyle, cursor: 'pointer', borderColor: C.indigo }}>Refresh</button>
        </div>
      </div>

      <div style={{ ...cardStyle, overflowX: 'auto' }}>
        <table style={tableStyle}>
          <thead>
            <tr>
              <th style={thStyle}>Domain</th>
              <th style={thStyle}>ISP</th>
              <th style={{ ...thStyle, textAlign: 'right' }}>Baseline /day</th>
              <th style={{ ...thStyle, textAlign: 'right' }}>Recent open</th>
              <th style={{ ...thStyle, textAlign: 'right' }}>Prior open</th>
              <th style={{ ...thStyle, textAlign: 'right', color: C.red }}>Hard</th>
              <th style={{ ...thStyle, textAlign: 'right', color: C.amber }}>Soft</th>
              <th style={{ ...thStyle, textAlign: 'right' }}>Complaints</th>
              <th style={thStyle}>Verdict</th>
            </tr>
          </thead>
          <tbody>
            {rows.map(r => {
              const key = `${r.sending_domain}|${r.isp}`;
              const isOpen = expanded.has(key);
              return (
                <React.Fragment key={key}>
                  <tr onClick={() => toggle(key)} style={{ cursor: 'pointer' }} title="Click for verdict reasons">
                    <td style={{ ...tdStyle, color: C.heading, fontWeight: 600 }}>{r.sending_domain}</td>
                    <td style={tdStyle}>{r.isp}</td>
                    <td style={{ ...tdStyle, textAlign: 'right' }}>{fmtNum(Math.round(r.baseline_daily_sends))}</td>
                    <td style={{ ...tdStyle, textAlign: 'right' }}>{fmtPct(r.recent.human_open_rate)}</td>
                    <td style={{ ...tdStyle, textAlign: 'right', color: C.sub }}>{fmtPct(r.prior.human_open_rate)}</td>
                    <td style={{ ...tdStyle, textAlign: 'right', color: C.red }}>{fmtPct(r.recent.hard_bounce_rate)}</td>
                    <td style={{ ...tdStyle, textAlign: 'right', color: C.amber }}>{fmtPct(r.recent.soft_bounce_rate)}</td>
                    <td style={{ ...tdStyle, textAlign: 'right' }}>{fmtPct(r.recent.complaint_rate)}</td>
                    <td style={tdStyle}><VerdictPill verdict={r.verdict} /></td>
                  </tr>
                  {isOpen && (
                    <tr>
                      <td colSpan={9} style={{ ...tdStyle, background: 'rgba(15,23,42,0.5)' }}>
                        <div style={{ padding: '4px 8px' }}>
                          <div style={{ color: C.sub, fontSize: 11, marginBottom: 4 }}>
                            Recent 7d: {fmtNum(r.recent.sends)} sends over {r.recent.days_with_sends} active days
                            ({fmtNum(Math.round(r.recent.daily_avg_sends))}/day) ·
                            Prior: {fmtNum(r.prior.sends)} sends ({fmtNum(Math.round(r.prior.daily_avg_sends))}/day) ·
                            Clicks recent {fmtPct(r.recent.click_rate)} vs prior {fmtPct(r.prior.click_rate)}
                          </div>
                          <ul style={{ margin: 0, paddingLeft: 18 }}>
                            {r.reasons.map((reason, i) => (
                              <li key={i} style={{ color: verdictColors[r.verdict].fg, fontSize: 12, whiteSpace: 'normal' }}>{reason}</li>
                            ))}
                          </ul>
                        </div>
                      </td>
                    </tr>
                  )}
                </React.Fragment>
              );
            })}
            {rows.length === 0 && (
              <tr><td colSpan={9} style={{ ...tdStyle, color: C.sub, textAlign: 'center' }}>No scorecard history for this window.</td></tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
};

// ── Tab 2: Today vs Baseline ──────────────────────────────────────────────────

const TodayTab: React.FC = () => {
  const [data, setData] = useState<TodayResponse | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    const load = async () => {
      try {
        const res = await apiFetch('/api/mailing/send-baselines/today');
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const body = await res.json() as TodayResponse;
        if (!cancelled) { setData(body); setError(null); }
      } catch (e) {
        if (!cancelled) setError(e instanceof Error ? e.message : 'failed to load pace board');
      }
    };
    void load();
    const id = window.setInterval(() => { void load(); }, 60_000);
    return () => { cancelled = true; window.clearInterval(id); };
  }, []);

  if (error) return <div style={{ color: C.red, padding: 24 }}>Error: {error}</div>;
  if (!data) return <div style={{ color: C.sub, padding: 24 }}>Loading pace board…</div>;

  const alerts = data.rows.filter(r => r.alert);
  const dayPct = Math.round(data.day_fraction * 100);

  return (
    <div>
      <div style={{ display: 'flex', gap: 12, alignItems: 'center', marginBottom: 16, flexWrap: 'wrap' }}>
        <div style={{ ...cardStyle, marginBottom: 0, padding: '12px 20px' }}>
          <div style={{ fontSize: 13, color: C.heading, fontWeight: 600 }}>{data.day} (UTC) — {dayPct}% of day elapsed</div>
          <div style={{ fontSize: 11, color: C.sub }}>Refreshes every 60s · updated {new Date(data.generated_at).toLocaleTimeString()}</div>
        </div>
        {alerts.length > 0 && (
          <div style={{ ...cardStyle, marginBottom: 0, padding: '12px 20px', borderColor: C.red, background: 'rgba(239,68,68,0.1)' }}>
            <div style={{ color: C.red, fontWeight: 700, fontSize: 13 }}>
              {alerts.length} pair{alerts.length === 1 ? '' : 's'} sending ABOVE baseline on a DECREASE verdict
            </div>
          </div>
        )}
      </div>

      <div style={{ ...cardStyle, overflowX: 'auto' }}>
        <table style={tableStyle}>
          <thead>
            <tr>
              <th style={thStyle}>Domain</th>
              <th style={thStyle}>ISP</th>
              <th style={{ ...thStyle, textAlign: 'right' }}>Today</th>
              <th style={{ ...thStyle, textAlign: 'right' }}>Baseline /day</th>
              <th style={{ ...thStyle, width: 220 }}>Pace vs baseline</th>
              <th style={thStyle}>Verdict</th>
            </tr>
          </thead>
          <tbody>
            {data.rows.map(r => {
              const key = `${r.sending_domain}|${r.isp}`;
              const pace = r.pace_pct;
              const barWidth = Math.min(pace, 150);
              const barColor = r.alert ? C.red : pace > 100 ? C.amber : C.indigo;
              return (
                <tr key={key} style={r.alert ? { background: 'rgba(239,68,68,0.08)', outline: `1px solid ${C.red}40` } : undefined}>
                  <td style={{ ...tdStyle, color: C.heading, fontWeight: 600 }}>{r.sending_domain}</td>
                  <td style={tdStyle}>{r.isp}</td>
                  <td style={{ ...tdStyle, textAlign: 'right' }}>{fmtNum(r.today_sends)}</td>
                  <td style={{ ...tdStyle, textAlign: 'right', color: C.sub }}>{fmtNum(Math.round(r.baseline_daily_sends))}</td>
                  <td style={tdStyle}>
                    <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                      <div style={{ flex: 1, height: 8, background: 'rgba(15,23,42,0.8)', borderRadius: 4, overflow: 'hidden', minWidth: 100 }}>
                        <div style={{ width: `${(barWidth / 150) * 100}%`, height: '100%', background: barColor, borderRadius: 4 }} />
                      </div>
                      <span style={{ fontSize: 12, color: barColor, fontWeight: 600, minWidth: 48, textAlign: 'right' }}>
                        {r.baseline_daily_sends > 0 ? `${pace.toFixed(0)}%` : '—'}
                      </span>
                    </div>
                  </td>
                  <td style={tdStyle}>
                    <VerdictPill verdict={r.verdict} />
                    {r.alert && <span style={{ color: C.red, fontSize: 11, fontWeight: 700, marginLeft: 8 }}>OVER BASELINE</span>}
                  </td>
                </tr>
              );
            })}
            {data.rows.length === 0 && (
              <tr><td colSpan={6} style={{ ...tdStyle, color: C.sub, textAlign: 'center' }}>No sends recorded today and no baselines yet.</td></tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
};

// ── Tab 3: Scorecards (historical) ────────────────────────────────────────────

interface ChartPoint {
  day: string;
  sent: number;
  human_opens: number;
  hard_bounces: number;
}

const ScorecardsTab: React.FC = () => {
  const [data, setData] = useState<ScorecardsResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [days, setDays] = useState<number>(14);
  const [domain, setDomain] = useState('');
  const [offer, setOffer] = useState('');
  const [ispFilter, setIspFilter] = useState('');

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const params = new URLSearchParams({ days: String(days) });
      if (domain) params.set('domain', domain);
      if (offer) params.set('offer', offer);
      if (ispFilter) params.set('isp', ispFilter);
      const res = await apiFetch(`/api/mailing/send-scorecards?${params.toString()}`);
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      setData(await res.json() as ScorecardsResponse);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'failed to load scorecards');
    } finally {
      setLoading(false);
    }
  }, [days, domain, offer, ispFilter]);

  useEffect(() => {
    const t = window.setTimeout(() => { void load(); }, 300); // debounce filter typing
    return () => window.clearTimeout(t);
  }, [load]);

  const chartData: ChartPoint[] = useMemo(() => {
    const byDay = new Map<string, ChartPoint>();
    (data?.rows ?? []).forEach(r => {
      const p = byDay.get(r.day) ?? { day: r.day, sent: 0, human_opens: 0, hard_bounces: 0 };
      p.sent += r.sent;
      p.human_opens += r.human_opens;
      p.hard_bounces += r.hard_bounces;
      byDay.set(r.day, p);
    });
    return Array.from(byDay.values()).sort((a, b) => a.day.localeCompare(b.day));
  }, [data]);

  return (
    <div>
      <div style={{ display: 'flex', gap: 8, marginBottom: 16, flexWrap: 'wrap', alignItems: 'center' }}>
        {[7, 14, 30].map(d => (
          <TabButton key={d} active={days === d} onClick={() => setDays(d)} label={`${d}d`} />
        ))}
        <input style={{ ...inputStyle, width: 180 }} placeholder="Filter domain…" value={domain} onChange={e => setDomain(e.target.value)} />
        <input style={{ ...inputStyle, width: 180 }} placeholder="Filter offer…" value={offer} onChange={e => setOffer(e.target.value)} />
        <input style={{ ...inputStyle, width: 140 }} placeholder="ISP (exact)…" value={ispFilter} onChange={e => setIspFilter(e.target.value)} />
        {data?.truncated && <span style={{ color: C.amber, fontSize: 12 }}>Result truncated — narrow the filters.</span>}
      </div>

      {error && <div style={{ color: C.red, marginBottom: 12 }}>Error: {error}</div>}

      <div style={{ ...cardStyle, height: 280 }}>
        <div style={{ color: C.heading, fontWeight: 600, fontSize: 13, marginBottom: 8 }}>Daily totals for current filter</div>
        <ResponsiveContainer width="100%" height={230}>
          <ComposedChart data={chartData}>
            <CartesianGrid stroke="rgba(99,102,241,0.15)" strokeDasharray="3 3" />
            <XAxis dataKey="day" stroke={C.sub} fontSize={11} />
            <YAxis yAxisId="left" stroke={C.sub} fontSize={11} />
            <YAxis yAxisId="right" orientation="right" stroke={C.sub} fontSize={11} />
            <Tooltip
              contentStyle={{ background: 'rgba(15,23,42,0.95)', border: `1px solid ${C.border}`, borderRadius: 8, color: '#e2e8f0' }}
              labelStyle={{ color: C.heading }}
            />
            <Legend wrapperStyle={{ fontSize: 12 }} />
            <Bar yAxisId="left" dataKey="sent" name="Sent" fill={C.indigo} radius={[3, 3, 0, 0]} />
            <Line yAxisId="right" type="monotone" dataKey="human_opens" name="Human opens" stroke={C.green} dot={false} strokeWidth={2} />
            <Line yAxisId="right" type="monotone" dataKey="hard_bounces" name="Hard bounces" stroke={C.red} dot={false} strokeWidth={2} />
          </ComposedChart>
        </ResponsiveContainer>
      </div>

      <div style={{ ...cardStyle, overflowX: 'auto' }}>
        {loading ? (
          <div style={{ color: C.sub, padding: 12 }}>Loading scorecards…</div>
        ) : (
          <table style={tableStyle}>
            <thead>
              <tr>
                <th style={thStyle}>Day</th>
                <th style={thStyle}>Domain</th>
                <th style={thStyle}>Offer</th>
                <th style={thStyle}>ISP</th>
                <th style={{ ...thStyle, textAlign: 'right' }}>Sent</th>
                <th style={{ ...thStyle, textAlign: 'right' }}>Delivered</th>
                <th style={{ ...thStyle, textAlign: 'right' }}>Opens</th>
                <th style={{ ...thStyle, textAlign: 'right', color: C.green }}>Human opens</th>
                <th style={{ ...thStyle, textAlign: 'right' }}>Clicks</th>
                <th style={{ ...thStyle, textAlign: 'right', color: C.red }}>Hard</th>
                <th style={{ ...thStyle, textAlign: 'right', color: C.amber }}>Soft</th>
                <th style={{ ...thStyle, textAlign: 'right', color: C.green }}>Conversions</th>
              </tr>
            </thead>
            <tbody>
              {(data?.rows ?? []).map((r, i) => (
                <tr key={`${r.day}|${r.sending_domain}|${r.offer_name}|${r.isp}|${i}`}>
                  <td style={{ ...tdStyle, color: C.sub }}>{r.day}</td>
                  <td style={{ ...tdStyle, color: C.heading, fontWeight: 600 }}>{r.sending_domain}</td>
                  <td style={tdStyle}>{r.offer_name}</td>
                  <td style={tdStyle}>{r.isp}</td>
                  <td style={{ ...tdStyle, textAlign: 'right' }}>{fmtNum(r.sent)}</td>
                  <td style={{ ...tdStyle, textAlign: 'right' }}>{fmtNum(r.delivered)}</td>
                  <td style={{ ...tdStyle, textAlign: 'right' }}>{fmtNum(r.opens)}</td>
                  <td style={{ ...tdStyle, textAlign: 'right', color: C.green }}>{fmtNum(r.human_opens)}</td>
                  <td style={{ ...tdStyle, textAlign: 'right' }}>{fmtNum(r.clicks)}</td>
                  <td style={{ ...tdStyle, textAlign: 'right', color: C.red }}>{fmtNum(r.hard_bounces)}</td>
                  <td style={{ ...tdStyle, textAlign: 'right', color: C.amber }}>{fmtNum(r.soft_bounces)}</td>
                  <td style={{ ...tdStyle, textAlign: 'right', color: C.green }}>{fmtNum(r.conversions)}</td>
                </tr>
              ))}
              {(data?.rows ?? []).length === 0 && (
                <tr><td colSpan={12} style={{ ...tdStyle, color: C.sub, textAlign: 'center' }}>No scorecard rows for this filter.</td></tr>
              )}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
};

// ── Screen ────────────────────────────────────────────────────────────────────

type InnerTab = 'baselines' | 'today' | 'history';

export const SendScorecards: React.FC = () => {
  const [tab, setTab] = useState<InnerTab>('baselines');

  return (
    <div style={containerStyle}>
      <div style={{ marginBottom: 4 }}>
        <h2 style={{ color: C.heading, fontSize: 20, fontWeight: 700, margin: 0 }}>Send Baselines &amp; Scorecards</h2>
        <p style={{ color: C.sub, fontSize: 13, margin: '4px 0 0 0' }}>
          Per (sending domain × ISP) volume-governance baselines — should we increase, decrease or maintain —
          plus historical post-send scorecards by domain × offer × ISP.
        </p>
      </div>

      <div style={{ display: 'flex', gap: 8, margin: '16px 0' }}>
        <TabButton active={tab === 'baselines'} onClick={() => setTab('baselines')} label="Baselines & Verdicts" />
        <TabButton active={tab === 'today'} onClick={() => setTab('today')} label="Today vs Baseline" />
        <TabButton active={tab === 'history'} onClick={() => setTab('history')} label="Scorecards (historical)" />
      </div>

      {tab === 'baselines' && <BaselinesTab />}
      {tab === 'today' && <TodayTab />}
      {tab === 'history' && <ScorecardsTab />}
    </div>
  );
};
