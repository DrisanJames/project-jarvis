// Domain Agents — BASELINES & SCORECARDS sections (merged from the old
// standalone Score Cards tab, requirements doc round-3 §5).
//
// Pieces (all consume internal/api/send_baselines.go):
//   - BaselineVerdictBanner: INCREASE/MAINTAIN/DECREASE verdict + reasons for
//     the SELECTED domain (worst verdict across its ISP pairs wins).
//   - TodayPaceSection: today-vs-baseline pace meter (60s refresh), alert
//     styling when sending ABOVE baseline on a DECREASE verdict.
//   - DomainScorecardHistory: historical domain × offer × ISP scorecard
//     table + chart scoped to the selected domain.
//   - AllDomainsBaselines: the cross-domain comparison table (the old tab's
//     main view) — shown when NO domain is selected.
//   - VerdictChip + canonicalDomain + worstVerdict helpers for the left rail.
//
// Domain matching: the rail collapses em./m. spellings into the canonical
// em.<apex> (mirrors canonicalSendingDomain in
// internal/api/domain_agent_handlers.go) — baseline/pace rows are matched the
// same way, and a brand's em.+m. rows merge with worst-verdict-wins.

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import {
  ResponsiveContainer, ComposedChart, Bar, Line, XAxis, YAxis,
  Tooltip, Legend, CartesianGrid,
} from 'recharts';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faChevronDown, faChevronRight } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../../shared/apiFetch';
import {
  C, panelStyle, sectionTitleStyle, thStyle, tdStyle, btnStyle, inputStyle,
  Loading, ErrorState, fmtInt,
} from './ui';
// Indigo design-system tokens (gold standard = OutboxDashboard). Aliased so they
// don't collide with the cyan `./ui` tokens the other sections in this file use.
import {
  panelStyle as iPanel,
  panelTitleStyle as iTitle,
  thStyle as iTh,
  tdStyle as iTd,
  numTd as iNumTd,
  numTh as iNumTh,
  colors as iC,
} from '../../shared/theme';

// ── Types (mirror internal/api/send_baselines.go JSON) ──────────────────────

export type Verdict = 'INCREASE' | 'MAINTAIN' | 'DECREASE';

export interface BaselineWindowStats {
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

export interface BaselineRow {
  sending_domain: string;
  isp: string;
  baseline_daily_sends: number;
  window_days: number;
  active_days: number;
  recent: BaselineWindowStats;
  prior: BaselineWindowStats;
  verdict: Verdict;
  reasons: string[];
}

export interface BaselinesResponse {
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

interface ScorecardHistoryRow {
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
  rows: ScorecardHistoryRow[];
}

// ── Domain canonicalization (mirrors Go canonicalSendingDomain) ──────────────

// Folds a brand's SES tenant spelling (m.<apex>) into the canonical property
// key (em.<apex>) so both routes roll up together — same as the rail.
export const canonicalDomain = (d: string): string => {
  const lower = d.toLowerCase();
  if (lower.startsWith('m.') && !lower.startsWith('em.')) return `em.${lower.slice(2)}`;
  return lower;
};

const VERDICT_RANK: Record<Verdict, number> = { DECREASE: 0, MAINTAIN: 1, INCREASE: 2 };

// Worst-verdict-wins (DECREASE < MAINTAIN < INCREASE).
export const worstVerdict = (verdicts: Verdict[]): Verdict | null => {
  let worst: Verdict | null = null;
  for (const v of verdicts) {
    if (worst === null || VERDICT_RANK[v] < VERDICT_RANK[worst]) worst = v;
  }
  return worst;
};

// canonical domain → worst verdict across all its (route × ISP) baseline rows.
export const verdictByCanonicalDomain = (rows: BaselineRow[]): Map<string, Verdict> => {
  const map = new Map<string, Verdict>();
  for (const r of rows) {
    const key = canonicalDomain(r.sending_domain);
    const prev = map.get(key);
    if (prev === undefined || VERDICT_RANK[r.verdict] < VERDICT_RANK[prev]) map.set(key, r.verdict);
  }
  return map;
};

// ── Verdict chip ─────────────────────────────────────────────────────────────

const verdictColors: Record<Verdict, string> = {
  INCREASE: C.success,
  MAINTAIN: C.muted,
  DECREASE: C.danger,
};

const verdictBg: Record<Verdict, string> = {
  INCREASE: 'rgba(0,184,148,0.15)',
  MAINTAIN: 'rgba(180,210,240,0.10)',
  DECREASE: 'rgba(233,69,96,0.15)',
};

export const VerdictChip: React.FC<{ verdict: Verdict; compact?: boolean }> = ({ verdict, compact }) => (
  <span
    title={compact ? `Volume recommendation: ${verdict} (worst across this domain's mailbox provider guidelines)` : undefined}
    style={{
      color: verdictColors[verdict],
      background: verdictBg[verdict],
      border: `1px solid ${verdictColors[verdict]}55`,
      borderRadius: 999,
      padding: compact ? '1px 7px' : '2px 10px',
      fontSize: compact ? 9.5 : 11,
      fontWeight: 700,
      letterSpacing: 0.6,
      whiteSpace: 'nowrap',
    }}
  >
    {compact ? ({ INCREASE: 'INC', MAINTAIN: 'MAINT', DECREASE: 'DEC' } as const)[verdict] : verdict}
  </span>
);

const fmtRate = (frac: number): string => `${(frac * 100).toFixed(2)}%`;

// ── Baseline verdict banner (selected domain) ────────────────────────────────

interface BannerProps {
  domain: string;
  data: BaselinesResponse | null;
  loading: boolean;
  error: string | null;
  onRetry: () => void;
}

export const BaselineVerdictBanner: React.FC<BannerProps> = ({ domain, data, loading, error, onRetry }) => {
  const rows = useMemo(
    () => (data?.rows ?? []).filter(r => canonicalDomain(r.sending_domain) === canonicalDomain(domain)),
    [data, domain],
  );
  const overall = worstVerdict(rows.map(r => r.verdict));

  if (loading) return <div style={panelStyle}><Loading label="Loading baseline verdict…" /></div>;
  if (error) {
    return (
      <div style={panelStyle}>
        <ErrorState message={`Failed to load baselines: ${error}`} onRetry={onRetry} />
      </div>
    );
  }
  if (!overall) {
    return (
      <div style={{ ...panelStyle, color: C.muted, fontSize: 13 }}>
        No baseline history for {domain} in the last {data?.window_days ?? 28} days — nothing to judge yet.
      </div>
    );
  }

  // Surface the rows driving the overall verdict first; others collapse below.
  const sorted = [...rows].sort((a, b) =>
    VERDICT_RANK[a.verdict] - VERDICT_RANK[b.verdict] || b.recent.sends - a.recent.sends);

  return (
    <div style={{
      ...panelStyle,
      border: `1px solid ${verdictColors[overall]}55`,
      background: overall === 'DECREASE' ? 'rgba(233,69,96,0.07)' : C.panel,
    }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 10, flexWrap: 'wrap' }}>
        <div style={{ ...sectionTitleStyle, marginBottom: 0 }}>Volume recommendation</div>
        <VerdictChip verdict={overall} />
        <span style={{ fontSize: 11.5, color: C.muted }}>
          worst across {rows.length} route × mailbox provider guideline{rows.length === 1 ? '' : 's'} · {data?.window_days ?? 28}d window
        </span>
      </div>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
        {sorted.map(r => (
          <div key={`${r.sending_domain}|${r.isp}`} style={{ fontSize: 12.5 }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 2 }}>
              <VerdictChip verdict={r.verdict} compact />
              <span style={{ color: C.text, fontWeight: 600 }}>{r.isp}</span>
              {canonicalDomain(r.sending_domain) !== r.sending_domain.toLowerCase() && (
                <span style={{ color: C.muted, fontSize: 11 }}>({r.sending_domain})</span>
              )}
              <span style={{ color: C.muted, fontSize: 11.5 }}>
                baseline {fmtInt(Math.round(r.baseline_daily_sends))}/day · recent open {fmtRate(r.recent.human_open_rate)} ·
                hard <span style={{ color: '#ef4444' }}>{fmtRate(r.recent.hard_bounce_rate)}</span> ·
                soft <span style={{ color: '#f59e0b' }}>{fmtRate(r.recent.soft_bounce_rate)}</span>
              </span>
            </div>
            <ul style={{ margin: '0 0 0 4px', paddingLeft: 18 }}>
              {r.reasons.map((reason, i) => (
                <li key={i} style={{ color: verdictColors[r.verdict], fontSize: 11.5 }}>{reason}</li>
              ))}
            </ul>
          </div>
        ))}
      </div>
    </div>
  );
};

// ── Today-vs-baseline pace meter (selected domain) ───────────────────────────

interface MergedPaceRow {
  isp: string;
  today_sends: number;
  baseline_daily_sends: number;
  verdict: Verdict;
  alert: boolean;
  routes: string[];
}

export const TodayPaceSection: React.FC<{ domain: string }> = ({ domain }) => {
  const [data, setData] = useState<TodayResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [reloadKey, setReloadKey] = useState(0);

  useEffect(() => {
    let cancelled = false;
    const load = async () => {
      try {
        const res = await apiFetch('/api/mailing/send-baselines/today');
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const body = await res.json() as TodayResponse;
        if (!cancelled) { setData(body); setError(null); }
      } catch (e) {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e));
      }
    };
    void load();
    const id = window.setInterval(() => { void load(); }, 60_000);
    return () => { cancelled = true; window.clearInterval(id); };
  }, [reloadKey]);

  // em.+m. route rows merge per ISP: sums for volume, worst verdict wins,
  // alert recomputed on the merged numbers (and kept if any source row fired).
  const rows = useMemo<MergedPaceRow[]>(() => {
    const canon = canonicalDomain(domain);
    const byIsp = new Map<string, MergedPaceRow>();
    for (const r of (data?.rows ?? [])) {
      if (canonicalDomain(r.sending_domain) !== canon) continue;
      let m = byIsp.get(r.isp);
      if (!m) {
        m = { isp: r.isp, today_sends: 0, baseline_daily_sends: 0, verdict: r.verdict, alert: false, routes: [] };
        byIsp.set(r.isp, m);
      }
      m.today_sends += r.today_sends;
      m.baseline_daily_sends += r.baseline_daily_sends;
      if (VERDICT_RANK[r.verdict] < VERDICT_RANK[m.verdict]) m.verdict = r.verdict;
      m.alert = m.alert || r.alert;
      m.routes.push(r.sending_domain);
    }
    const out = Array.from(byIsp.values());
    for (const m of out) {
      m.alert = m.alert || (m.verdict === 'DECREASE' && m.baseline_daily_sends > 0 && m.today_sends > m.baseline_daily_sends);
    }
    return out.sort((a, b) => Number(b.alert) - Number(a.alert) || b.today_sends - a.today_sends);
  }, [data, domain]);

  const alerts = rows.filter(r => r.alert);
  const dayPct = data ? Math.round(data.day_fraction * 100) : 0;

  return (
    <div style={panelStyle}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 12, flexWrap: 'wrap' }}>
        <div style={{ ...sectionTitleStyle, marginBottom: 0 }}>Today vs baseline</div>
        {data && (
          <span style={{ fontSize: 11.5, color: C.muted }}>
            {data.day} (UTC) — {dayPct}% of day elapsed · refreshes every 60s
          </span>
        )}
        {alerts.length > 0 && (
          <span style={{
            color: C.danger, background: 'rgba(233,69,96,0.12)', border: `1px solid ${C.danger}66`,
            borderRadius: 6, padding: '3px 10px', fontSize: 11.5, fontWeight: 700,
          }}>
            {alerts.length} mailbox provider{alerts.length === 1 ? '' : 's'} sending above the recommended volume
          </span>
        )}
      </div>

      {error && <ErrorState message={`Failed to load pace board: ${error}`} onRetry={() => setReloadKey(k => k + 1)} />}
      {!error && !data && <Loading label="Loading pace board…" />}

      {!error && data && rows.length === 0 && (
        <div style={{ color: C.muted, fontSize: 13, padding: '4px 2px' }}>
          No sends today and no baselines yet for {domain}.
        </div>
      )}

      {!error && data && rows.length > 0 && (
        <div style={{ overflowX: 'auto' }}>
          <table style={{ borderCollapse: 'collapse', width: '100%' }}>
            <thead>
              <tr>
                <th style={thStyle}>Provider</th>
                <th style={{ ...thStyle, textAlign: 'right' }}>Today</th>
                <th style={{ ...thStyle, textAlign: 'right' }}>Baseline /day</th>
                <th style={{ ...thStyle, width: 220 }}>Pace vs baseline</th>
                <th style={thStyle}>Verdict</th>
              </tr>
            </thead>
            <tbody>
              {rows.map(r => {
                const pace = r.baseline_daily_sends > 0 ? (r.today_sends / r.baseline_daily_sends) * 100 : 0;
                const barWidth = Math.min(pace, 150);
                const barColor = r.alert ? C.danger : pace > 100 ? C.warning : C.accent2;
                return (
                  <tr
                    key={r.isp}
                    title={r.routes.length > 1 ? `Merged routes: ${r.routes.join(', ')}` : r.routes[0]}
                    style={r.alert ? { background: 'rgba(233,69,96,0.08)', outline: `1px solid ${C.danger}40` } : undefined}
                  >
                    <td style={{ ...tdStyle, fontWeight: 600 }}>{r.isp}</td>
                    <td style={{ ...tdStyle, textAlign: 'right' }}>{fmtInt(r.today_sends)}</td>
                    <td style={{ ...tdStyle, textAlign: 'right', color: C.muted }}>{fmtInt(Math.round(r.baseline_daily_sends))}</td>
                    <td style={tdStyle}>
                      <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                        <div style={{ flex: 1, height: 8, background: 'rgba(10,15,26,0.8)', borderRadius: 4, overflow: 'hidden', minWidth: 100 }}>
                          <div style={{ width: `${(barWidth / 150) * 100}%`, height: '100%', background: barColor, borderRadius: 4 }} />
                        </div>
                        <span style={{ fontSize: 12, color: barColor, fontWeight: 600, minWidth: 48, textAlign: 'right' }}>
                          {r.baseline_daily_sends > 0 ? `${pace.toFixed(0)}%` : '—'}
                        </span>
                      </div>
                    </td>
                    <td style={tdStyle}>
                      <VerdictChip verdict={r.verdict} compact />
                      {r.alert && <span style={{ color: C.danger, fontSize: 11, fontWeight: 700, marginLeft: 8 }}>OVER BASELINE</span>}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
};

// ── Historical domain × offer × ISP scorecards (selected domain) ─────────────

const DAY_CHOICES = [7, 14, 30] as const;

interface ChartPoint {
  day: string;
  sent: number;
  human_opens: number;
  hard_bounces: number;
}

export const DomainScorecardHistory: React.FC<{ domain: string }> = ({ domain }) => {
  const [data, setData] = useState<ScorecardsResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [days, setDays] = useState<number>(14);
  const [offer, setOffer] = useState('');
  const [ispFilter, setIspFilter] = useState('');

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      // Scope by the apex so the server's substring filter matches both the
      // em.<apex> (PMTA) and m.<apex> (SES) route spellings of this property.
      const apex = canonicalDomain(domain).replace(/^em\./, '');
      const params = new URLSearchParams({ days: String(days), domain: apex });
      if (offer) params.set('offer', offer);
      if (ispFilter) params.set('isp', ispFilter);
      const res = await apiFetch(`/api/mailing/send-scorecards?${params.toString()}`);
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      setData(await res.json() as ScorecardsResponse);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, [domain, days, offer, ispFilter]);

  useEffect(() => {
    const t = window.setTimeout(() => { void load(); }, 300); // debounce filter typing
    return () => window.clearTimeout(t);
  }, [load]);

  const rows = data?.rows ?? [];

  const chartData: ChartPoint[] = useMemo(() => {
    const byDay = new Map<string, ChartPoint>();
    rows.forEach(r => {
      const p = byDay.get(r.day) ?? { day: r.day, sent: 0, human_opens: 0, hard_bounces: 0 };
      p.sent += r.sent;
      p.human_opens += r.human_opens;
      p.hard_bounces += r.hard_bounces;
      byDay.set(r.day, p);
    });
    return Array.from(byDay.values()).sort((a, b) => a.day.localeCompare(b.day));
  }, [rows]);

  return (
    <div style={panelStyle}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 12, flexWrap: 'wrap' }}>
        <div style={{ ...sectionTitleStyle, marginBottom: 0, marginRight: 4 }}>Offer × Provider scorecards</div>
        {DAY_CHOICES.map(d => (
          <button
            key={d}
            onClick={() => setDays(d)}
            style={{
              ...btnStyle,
              padding: '4px 10px',
              fontSize: 12,
              background: days === d ? 'rgba(0,229,255,0.2)' : 'rgba(0,176,255,0.08)',
              border: days === d ? `1px solid ${C.accent}` : '1px solid rgba(0,176,255,0.25)',
              color: days === d ? C.accent : C.muted,
            }}
          >
            {d}d
          </button>
        ))}
        <input
          style={{ ...inputStyle, width: 170 }}
          placeholder="Filter offer…"
          value={offer}
          onChange={e => setOffer(e.target.value)}
        />
        <input
          style={{ ...inputStyle, width: 130 }}
          placeholder="Provider (exact)…"
          value={ispFilter}
          onChange={e => setIspFilter(e.target.value)}
        />
        {data?.truncated && <span style={{ color: C.warning, fontSize: 12 }}>Result truncated — narrow the filters.</span>}
      </div>

      {error && <ErrorState message={`Failed to load scorecards: ${error}`} onRetry={load} />}

      {!error && (
        <>
          <div style={{ fontSize: 11.5, color: C.muted, marginBottom: 10 }}>
            Conversions are offer-level metrics credited to the day's highest-volume sending
            domain for that offer × mailbox provider — read them as offer performance, not domain attribution.
          </div>

          <div style={{ height: 260, marginBottom: 14 }}>
            <ResponsiveContainer width="100%" height={250}>
              <ComposedChart data={chartData}>
                <CartesianGrid stroke="rgba(0,200,255,0.10)" strokeDasharray="3 3" />
                <XAxis dataKey="day" stroke={C.muted} fontSize={11} />
                <YAxis yAxisId="left" stroke={C.muted} fontSize={11} />
                <YAxis yAxisId="right" orientation="right" stroke={C.muted} fontSize={11} />
                <Tooltip
                  contentStyle={{ background: 'rgba(10,15,26,0.95)', border: `1px solid ${C.panelBorder}`, borderRadius: 8, color: C.text }}
                  labelStyle={{ color: C.accent }}
                />
                <Legend wrapperStyle={{ fontSize: 12 }} />
                <Bar yAxisId="left" dataKey="sent" name="Sent" fill={C.accent2} radius={[3, 3, 0, 0]} />
                <Line yAxisId="right" type="monotone" dataKey="human_opens" name="Human opens" stroke={C.success} dot={false} strokeWidth={2} />
                <Line yAxisId="right" type="monotone" dataKey="hard_bounces" name="Hard bounces" stroke={C.danger} dot={false} strokeWidth={2} />
              </ComposedChart>
            </ResponsiveContainer>
          </div>

          {loading ? (
            <Loading label="Loading scorecards…" />
          ) : (
            <div style={{ overflowX: 'auto' }}>
              <table style={{ borderCollapse: 'collapse', width: '100%' }}>
                <thead>
                  <tr>
                    <th style={thStyle}>Day</th>
                    <th style={thStyle}>Route</th>
                    <th style={thStyle}>Offer</th>
                    <th style={thStyle}>Provider</th>
                    <th style={{ ...thStyle, textAlign: 'right' }}>Sent</th>
                    <th style={{ ...thStyle, textAlign: 'right' }}>Delivered</th>
                    <th style={{ ...thStyle, textAlign: 'right' }}>Opens</th>
                    <th style={{ ...thStyle, textAlign: 'right', color: C.success }}>Human opens</th>
                    <th style={{ ...thStyle, textAlign: 'right' }}>Clicks</th>
                    <th style={{ ...thStyle, textAlign: 'right', color: C.danger }}>Hard</th>
                    <th style={{ ...thStyle, textAlign: 'right', color: C.warning }}>Soft</th>
                    <th style={{ ...thStyle, textAlign: 'right', color: C.success }}>Conv</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((r, i) => (
                    <tr key={`${r.day}|${r.sending_domain}|${r.offer_name}|${r.isp}|${i}`}>
                      <td style={{ ...tdStyle, color: C.muted, fontSize: 12 }}>{r.day}</td>
                      <td style={{ ...tdStyle, fontSize: 12, color: C.muted }}>{r.sending_domain}</td>
                      <td style={tdStyle}>{r.offer_name}</td>
                      <td style={tdStyle}>{r.isp}</td>
                      <td style={{ ...tdStyle, textAlign: 'right' }}>{fmtInt(r.sent)}</td>
                      <td style={{ ...tdStyle, textAlign: 'right' }}>{fmtInt(r.delivered)}</td>
                      <td style={{ ...tdStyle, textAlign: 'right' }}>{fmtInt(r.opens)}</td>
                      <td style={{ ...tdStyle, textAlign: 'right', color: C.success }}>{fmtInt(r.human_opens)}</td>
                      <td style={{ ...tdStyle, textAlign: 'right' }}>{fmtInt(r.clicks)}</td>
                      <td style={{ ...tdStyle, textAlign: 'right', color: r.hard_bounces > 0 ? C.danger : C.text }}>{fmtInt(r.hard_bounces)}</td>
                      <td style={{ ...tdStyle, textAlign: 'right', color: r.soft_bounces > 0 ? C.warning : C.text }}>{fmtInt(r.soft_bounces)}</td>
                      <td style={{ ...tdStyle, textAlign: 'right', color: C.success }}>{fmtInt(r.conversions)}</td>
                    </tr>
                  ))}
                  {rows.length === 0 && (
                    <tr><td colSpan={12} style={{ ...tdStyle, color: C.muted, textAlign: 'center' }}>No scorecard rows for this filter.</td></tr>
                  )}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </div>
  );
};

// ── All-domains comparison view (no domain selected) ─────────────────────────

interface AllDomainsProps {
  data: BaselinesResponse | null;
  loading: boolean;
  error: string | null;
  onRetry: () => void;
  onSelectDomain: (canonical: string) => void;
}

// One rolled-up row per canonical sending domain; the per-ISP BaselineRows
// become its expandable detail (mirrors the OutboxDashboard ISP→VMTA drill-down).
interface DomainRollup {
  canonical: string;
  rows: BaselineRow[];          // per-ISP rows feeding this domain
  ispCount: number;             // distinct mailbox providers
  totalBaseline: number;        // Σ baseline_daily_sends
  recentOpen: number;           // volume-weighted recent open rate
  priorOpen: number;            // volume-weighted prior open rate
  hard: number;                 // volume-weighted recent hard-bounce rate
  soft: number;                 // volume-weighted recent soft-bounce rate
  complaint: number;            // volume-weighted recent complaint rate
  verdict: Verdict;             // worst verdict across the domain's ISP pairs
}

export const AllDomainsBaselines: React.FC<AllDomainsProps> = ({ data, loading, error, onRetry, onSelectDomain }) => {
  // Two independent expansion sets: domains (the rollup rows) and ISP rows
  // (the verdict-reasons drill-down nested inside an expanded domain).
  const [expandedDomains, setExpandedDomains] = useState<Set<string>>(new Set());
  const [expandedIsp, setExpandedIsp] = useState<Set<string>>(new Set());

  const toggleDomain = (key: string) =>
    setExpandedDomains(prev => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key); else next.add(key);
      return next;
    });
  const toggleIsp = (key: string) =>
    setExpandedIsp(prev => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key); else next.add(key);
      return next;
    });

  // Group rows by canonical domain (folds em.+m. routes together, same as the
  // rail), then aggregate. Rates are volume-weighted by recent send volume
  // (falling back to baseline_daily_sends when a pair has no recent sends);
  // baseline/day is a straight sum. Worst-verdict-wins per domain.
  const rollups = useMemo<DomainRollup[]>(() => {
    const byDomain = new Map<string, BaselineRow[]>();
    for (const r of data?.rows ?? []) {
      const key = canonicalDomain(r.sending_domain);
      const list = byDomain.get(key);
      if (list) list.push(r); else byDomain.set(key, [r]);
    }

    const wavg = (rows: BaselineRow[], pick: (r: BaselineRow) => number, weight: (r: BaselineRow) => number): number => {
      let num = 0, den = 0;
      for (const r of rows) {
        const w = weight(r);
        num += pick(r) * w;
        den += w;
      }
      return den > 0 ? num / den : 0;
    };
    const recentW = (r: BaselineRow) => (r.recent.sends > 0 ? r.recent.sends : r.baseline_daily_sends);
    const priorW = (r: BaselineRow) => (r.prior.sends > 0 ? r.prior.sends : r.baseline_daily_sends);

    const out: DomainRollup[] = [];
    for (const [canonical, rows] of byDomain) {
      out.push({
        canonical,
        rows: [...rows].sort((a, b) =>
          VERDICT_RANK[a.verdict] - VERDICT_RANK[b.verdict] || b.baseline_daily_sends - a.baseline_daily_sends),
        ispCount: new Set(rows.map(r => r.isp)).size,
        totalBaseline: rows.reduce((s, r) => s + r.baseline_daily_sends, 0),
        recentOpen: wavg(rows, r => r.recent.human_open_rate, recentW),
        priorOpen: wavg(rows, r => r.prior.human_open_rate, priorW),
        hard: wavg(rows, r => r.recent.hard_bounce_rate, recentW),
        soft: wavg(rows, r => r.recent.soft_bounce_rate, recentW),
        complaint: wavg(rows, r => r.recent.complaint_rate, recentW),
        verdict: worstVerdict(rows.map(r => r.verdict)) ?? 'MAINTAIN',
      });
    }
    // Surface the domains that need attention first: worst verdict, then volume.
    return out.sort((a, b) =>
      VERDICT_RANK[a.verdict] - VERDICT_RANK[b.verdict] || b.totalBaseline - a.totalBaseline);
  }, [data]);

  if (loading) return <div style={iPanel}><Loading label="Loading baselines…" /></div>;
  if (error) {
    return (
      <div style={iPanel}>
        <ErrorState message={`Failed to load baselines: ${error}`} onRetry={onRetry} />
      </div>
    );
  }
  if (!data) return null;

  return (
    <div style={iPanel}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 12, flexWrap: 'wrap' }}>
        <h3 style={iTitle}>All domains — baselines &amp; verdicts</h3>
        <span style={{ fontSize: 11.5, color: iC.textMuted }}>
          {data.window_days}d window · click a domain to expand its mailbox providers
        </span>
        <div style={{ marginLeft: 'auto', display: 'flex', gap: 14 }}>
          {([
            ['DECREASE', data.summary.decrease],
            ['MAINTAIN', data.summary.maintain],
            ['INCREASE', data.summary.increase],
          ] as Array<[Verdict, number]>).map(([v, n]) => (
            <span key={v} style={{ fontSize: 12.5, color: verdictColors[v], fontWeight: 700 }}>
              {n} {v}
            </span>
          ))}
        </div>
      </div>

      <div style={{ overflowX: 'auto' }}>
        <table style={{ borderCollapse: 'collapse', width: '100%' }}>
          <thead>
            <tr>
              <th style={iTh}>Domain</th>
              <th style={iTh}>Providers</th>
              <th style={iNumTh}>Baseline /day</th>
              <th style={iNumTh}>Recent open</th>
              <th style={iNumTh}>Prior open</th>
              <th style={{ ...iNumTh, color: iC.dangerText }}>Hard</th>
              <th style={{ ...iNumTh, color: iC.warningText }}>Soft</th>
              <th style={iNumTh}>Complaints</th>
              <th style={iTh}>Verdict</th>
            </tr>
          </thead>
          <tbody>
            {rollups.map(d => {
              const isOpen = expandedDomains.has(d.canonical);
              return (
                <React.Fragment key={d.canonical}>
                  <tr
                    onClick={() => toggleDomain(d.canonical)}
                    title="Click to expand mailbox providers"
                    style={{ cursor: 'pointer', background: isOpen ? iC.hover : undefined }}
                  >
                    <td style={{ ...iTd, fontWeight: 600, color: iC.heading }}>
                      <FontAwesomeIcon
                        icon={isOpen ? faChevronDown : faChevronRight}
                        style={{ color: iC.indigo500, fontSize: 11, marginRight: 7 }}
                      />
                      <span
                        onClick={e => { e.stopPropagation(); onSelectDomain(d.canonical); }}
                        title={`Open the ${d.canonical} domain agent`}
                        style={{ color: iC.indigo300, cursor: 'pointer' }}
                      >
                        {d.canonical}
                      </span>
                    </td>
                    <td style={{ ...iTd, color: iC.textMuted }}>
                      {d.ispCount} provider{d.ispCount === 1 ? '' : 's'}
                    </td>
                    <td style={iNumTd}>{fmtInt(Math.round(d.totalBaseline))}</td>
                    <td style={iNumTd}>{fmtRate(d.recentOpen)}</td>
                    <td style={{ ...iNumTd, color: iC.textMuted }}>{fmtRate(d.priorOpen)}</td>
                    <td style={{ ...iNumTd, color: iC.dangerText }}>{fmtRate(d.hard)}</td>
                    <td style={{ ...iNumTd, color: iC.warningText }}>{fmtRate(d.soft)}</td>
                    <td style={iNumTd}>{fmtRate(d.complaint)}</td>
                    <td style={iTd}><VerdictChip verdict={d.verdict} compact /></td>
                  </tr>
                  {isOpen && (
                    <tr>
                      <td colSpan={9} style={{ padding: 0, background: iC.appBgSolid }}>
                        <table style={{ borderCollapse: 'collapse', width: '100%' }}>
                          <thead>
                            <tr>
                              <th style={{ ...iTh, paddingLeft: 28 }}>Provider</th>
                              <th style={iNumTh}>Baseline /day</th>
                              <th style={iNumTh}>Recent open</th>
                              <th style={iNumTh}>Prior open</th>
                              <th style={{ ...iNumTh, color: iC.dangerText }}>Hard</th>
                              <th style={{ ...iNumTh, color: iC.warningText }}>Soft</th>
                              <th style={iNumTh}>Complaints</th>
                              <th style={iTh}>Verdict</th>
                            </tr>
                          </thead>
                          <tbody>
                            {d.rows.map(r => {
                              const ispKey = `${r.sending_domain}|${r.isp}`;
                              const ispOpen = expandedIsp.has(ispKey);
                              const routeDiffers = canonicalDomain(r.sending_domain) !== r.sending_domain.toLowerCase();
                              return (
                                <React.Fragment key={ispKey}>
                                  <tr
                                    onClick={() => toggleIsp(ispKey)}
                                    title="Click for verdict reasons"
                                    style={{ cursor: 'pointer', background: ispOpen ? iC.hover : undefined }}
                                  >
                                    <td style={{ ...iTd, paddingLeft: 28 }}>
                                      <FontAwesomeIcon
                                        icon={ispOpen ? faChevronDown : faChevronRight}
                                        style={{ color: iC.indigo400, fontSize: 10, marginRight: 7 }}
                                      />
                                      {r.isp}
                                      {routeDiffers && (
                                        <span style={{ color: iC.textFaint, fontSize: 11, marginLeft: 6 }}>({r.sending_domain})</span>
                                      )}
                                    </td>
                                    <td style={iNumTd}>{fmtInt(Math.round(r.baseline_daily_sends))}</td>
                                    <td style={iNumTd}>{fmtRate(r.recent.human_open_rate)}</td>
                                    <td style={{ ...iNumTd, color: iC.textMuted }}>{fmtRate(r.prior.human_open_rate)}</td>
                                    <td style={{ ...iNumTd, color: iC.dangerText }}>{fmtRate(r.recent.hard_bounce_rate)}</td>
                                    <td style={{ ...iNumTd, color: iC.warningText }}>{fmtRate(r.recent.soft_bounce_rate)}</td>
                                    <td style={iNumTd}>{fmtRate(r.recent.complaint_rate)}</td>
                                    <td style={iTd}><VerdictChip verdict={r.verdict} compact /></td>
                                  </tr>
                                  {ispOpen && (
                                    <tr>
                                      <td colSpan={8} style={{ ...iTd, background: iC.panelBgSolid }}>
                                        <div style={{ padding: '4px 8px 4px 28px' }}>
                                          <div style={{ color: iC.textMuted, fontSize: 11, marginBottom: 4 }}>
                                            Recent: {fmtInt(r.recent.sends)} sends over {r.recent.days_with_sends} active days
                                            ({fmtInt(Math.round(r.recent.daily_avg_sends))}/day) ·
                                            Prior: {fmtInt(r.prior.sends)} sends ({fmtInt(Math.round(r.prior.daily_avg_sends))}/day) ·
                                            Clicks recent {fmtRate(r.recent.click_rate)} vs prior {fmtRate(r.prior.click_rate)}
                                          </div>
                                          <ul style={{ margin: 0, paddingLeft: 18 }}>
                                            {r.reasons.map((reason, i) => (
                                              <li key={i} style={{ color: verdictColors[r.verdict], fontSize: 12, whiteSpace: 'normal' }}>{reason}</li>
                                            ))}
                                          </ul>
                                        </div>
                                      </td>
                                    </tr>
                                  )}
                                </React.Fragment>
                              );
                            })}
                          </tbody>
                        </table>
                      </td>
                    </tr>
                  )}
                </React.Fragment>
              );
            })}
            {rollups.length === 0 && (
              <tr><td colSpan={9} style={{ ...iTd, color: iC.textMuted, textAlign: 'center' }}>No scorecard history for this window.</td></tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
};
