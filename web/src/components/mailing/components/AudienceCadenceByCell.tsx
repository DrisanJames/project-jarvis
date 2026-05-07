// Audience Cadence — Global per ISP
//
// PAGE_VERSION 2.0 (2026-05-05) — REFACTOR. Replaces the 1.0 per-(brand × ISP)
// view. The welcome pool is a single shared bucket — the deploy scripts hand
// the same six inclusion segments to every brand and the planner shuffles by
// ISP across brands as it picks recipients. So the meaningful exhaustion
// question is global per ISP, not per (sending_domain × ISP). The 1.0 view
// double-counted subscribers touched by multiple brands and gave a misleadingly
// inflated picture of pool size. 2.0 fixes that by grouping on (subscriber →
// ISP) directly, deduped, using mailing_message_log for prior send count
// exactly the way the Welcome planner enforces saturation.
//
// Backend: /api/mailing/analytics/audience-cadence-by-isp  (api_version 2.0)
//
// Standing context (mirrors .scratch/SCHEDULING_INTEGRITY_PLAYBOOK.md §15):
//   - Welcome saturation threshold = 8 (operator-locked)
//   - Activation target = 1% opener share. Below 1% → engagement cold,
//     do NOT upload raw, must source engaged-only data.
//   - Refresh trigger classifier mirrored exactly between Go & Python so
//     the dashboard matches the scratch report.

import React, { useCallback, useEffect, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faSpinner, faSyncAlt, faExclamationTriangle, faCheckCircle,
  faExclamationCircle, faSeedling, faClock, faChartLine, faUpload,
  faShieldAlt, faUserMinus, faSnowflake, faFire, faGlobe, faInfoCircle,
} from '@fortawesome/free-solid-svg-icons';
import {
  ResponsiveContainer, BarChart, Bar, XAxis, YAxis, CartesianGrid,
  Tooltip as RechartsTooltip, Cell as RechartsCell, ReferenceLine,
} from 'recharts';

const PAGE_VERSION = '2.0';

// ─── Types (mirror Go response shape) ─────────────────────────────────────

interface ISPCadenceRow {
  isp: string;
  pool_total: number;
  exempt_opened: number;
  retired_saturated: number;
  never_mailed: number;
  one_send_away: number;
  two_to_three_away: number;
  four_to_five_away: number;
  six_to_eight_away: number;
  welcome_pool: number;
  daily_unique_burn_7d: number;
  unsubscribed_30d: number;
  hard_bounced_30d: number;
  complained_30d: number;
  cycle_days: number;
  saturation_horizon_days: number;
  activation_pct: number;
  activation_target_met: boolean;
  unsub_rate_30d_pct: number;
  hard_bounce_rate_30d_pct: number;
  refresh_trigger: string;
  refresh_reason: string;
}

interface TriggerDef {
  trigger: string;
  rule: string;
  action: string;
}

interface AudienceCadenceData {
  api_version: string;
  as_of: string;
  snapshot_refreshed_at: string;
  snapshot_age_seconds: number;
  saturation_threshold: number;
  activation_target_pct: number;
  burn_window_days: number;
  churn_window_days: number;
  pool_segment_ids: string[];
  isp_rows: ISPCadenceRow[];
  top_now_isps: ISPCadenceRow[];
  totals: ISPCadenceRow;
  trigger_counts: { now: number; this_week: number; ample: number };
  trigger_definitions: TriggerDef[];
  explainer: {
    pool_definition: string;
    burn_definition: string;
    churn_definition: string;
    isp_classification: string;
    why_global_not_brand: string;
  };
}

// ─── Style tokens (match WelcomeAudienceHealth dark theme) ───────────────

const COLORS = {
  bgDeep:        '#0a0e1a',
  bgPanel:       '#0f1424',
  bgPanelAlt:    '#131a2e',
  border:        'rgba(255,255,255,0.06)',
  borderStrong:  'rgba(255,255,255,0.12)',
  textPrimary:   '#e2e8f0',
  textSecondary: '#94a3b8',
  textMuted:     '#64748b',
  accent:        '#818cf8',
  accentAlt:     '#a78bfa',
  accentPink:    '#f472b6',
  good:          '#34d399',
  warn:          '#fbbf24',
  danger:        '#f87171',
  hardBounce:    '#ef4444',
  softBounce:    '#f59e0b',
};

const TRIGGER_META: Record<string, { label: string; color: string; icon: any; order: number }> = {
  NOW:             { label: 'NOW',        color: COLORS.danger,    icon: faExclamationTriangle, order: 0 },
  THIS_WEEK:       { label: 'THIS WEEK',  color: '#f97316',        icon: faExclamationCircle,   order: 1 },
  TWO_WEEKS:       { label: '2 WEEKS',    color: COLORS.warn,      icon: faClock,               order: 2 },
  ONE_MONTH_PLUS:  { label: '1 MONTH+',   color: COLORS.good,      icon: faCheckCircle,         order: 3 },
  AMPLE:           { label: 'AMPLE',      color: COLORS.accent,    icon: faSnowflake,           order: 4 },
  UNKNOWN:         { label: '—',          color: COLORS.textMuted, icon: faClock,               order: 5 },
};

const ISP_LABEL: Record<string, string> = {
  gmail: 'Gmail',
  yahoo: 'Yahoo',
  microsoft: 'Microsoft',
  apple: 'Apple iCloud',
  aol: 'AOL',
  comcast: 'Comcast / Xfinity',
  charter: 'Charter / Spectrum',
  att: 'AT&T',
  sbcglobal: 'SBCGlobal / Bell',
  cox: 'Cox',
  other: 'Other / B2B',
};

const fmt = (n: number) => n.toLocaleString('en-US');
const fmtPct = (n: number) => `${n.toFixed(2)}%`;
const fmtDays = (n: number) => (n > 0 ? `${n.toFixed(1)}d` : '—');

// ─── Main component ───────────────────────────────────────────────────────

export const AudienceCadenceByCell: React.FC = () => {
  const [data, setData] = useState<AudienceCadenceData | null>(null);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState('');
  const [showExplainer, setShowExplainer] = useState(false);

  const fetchData = useCallback(async () => {
    setLoading(true);
    try {
      const res = await fetch('/api/mailing/analytics/audience-cadence-by-isp', {
        credentials: 'include',
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const json: AudienceCadenceData = await res.json();
      setData(json);
      setError('');
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  const forceRefresh = useCallback(async () => {
    setRefreshing(true);
    try {
      const res = await fetch('/api/mailing/analytics/audience-cadence-by-isp/refresh', {
        method: 'POST',
        credentials: 'include',
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      await fetchData();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setRefreshing(false);
    }
  }, [fetchData]);

  useEffect(() => { fetchData(); }, [fetchData]);

  if (loading && !data) {
    return (
      <div style={styles.loadingShell}>
        <FontAwesomeIcon icon={faSpinner} spin /> Loading audience cadence…
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

  const isps = data.isp_rows;
  const totals = data.totals;
  const fastestCycle = isps.find((r) => r.cycle_days > 0);

  return (
    <div style={styles.page}>
      {/* ─── Header ──────────────────────────────────────────────────── */}
      <div style={styles.header}>
        <div>
          <h1 style={styles.title}>
            <FontAwesomeIcon icon={faGlobe} style={{ color: COLORS.accent, marginRight: 12 }} />
            Audience Cadence — Global per ISP
          </h1>
          <p style={styles.subtitle}>
            The welcome pool is a <strong>single shared bucket</strong> across all four brands. The
            planner shuffles by ISP as it picks recipients. So the meaningful exhaustion question
            is global per ISP, not per (sending_domain × ISP). This dashboard answers
            “when do I need to upload more data per ISP?”
          </p>
        </div>
        <div style={{ display: 'flex', gap: 8 }}>
          <button style={styles.refreshBtn} onClick={fetchData} disabled={loading}>
            <FontAwesomeIcon icon={loading ? faSpinner : faSyncAlt} spin={loading} /> Reload
          </button>
          <button
            style={{ ...styles.refreshBtn, borderColor: COLORS.accent, color: COLORS.accent }}
            onClick={forceRefresh}
            disabled={refreshing}
            title="Re-run the heavy aggregation queries against the live database (~1-3 minutes)"
          >
            <FontAwesomeIcon icon={refreshing ? faSpinner : faSyncAlt} spin={refreshing} /> {refreshing ? 'Rebuilding…' : 'Rebuild snapshot'}
          </button>
        </div>
      </div>

      {/* ─── Snapshot freshness banner ──────────────────────────────── */}
      <div style={{
        ...styles.metaStrip,
        marginBottom: 8,
        background: data.snapshot_age_seconds > 1800 ? `${COLORS.warn}11` : COLORS.bgPanelAlt,
        borderColor: data.snapshot_age_seconds > 1800 ? `${COLORS.warn}55` : COLORS.border,
      }}>
        <span>
          <FontAwesomeIcon icon={faClock} style={{ marginRight: 4 }} />
          Snapshot built: <strong>{new Date(data.snapshot_refreshed_at).toLocaleString('en-US', { hour12: false })}</strong>
          {' '}({Math.floor(data.snapshot_age_seconds / 60)}m {data.snapshot_age_seconds % 60}s ago)
        </span>
        <span style={{ color: COLORS.textMuted }}>
          Auto-refresh every 15 min · Click "Rebuild snapshot" after a list upload to see immediately
        </span>
      </div>

      {/* ─── Standing rules strip ────────────────────────────────────── */}
      <div style={styles.metaStrip}>
        <span><FontAwesomeIcon icon={faShieldAlt} /> Saturation threshold: <strong>{data.saturation_threshold}</strong> sends w/o open → auto-excluded from Welcome</span>
        <span><FontAwesomeIcon icon={faSeedling} /> Activation target: <strong>{data.activation_target_pct}%</strong> opener share</span>
        <span><FontAwesomeIcon icon={faChartLine} /> Burn window: <strong>{data.burn_window_days}d</strong> avg unique Welcome recipients/day</span>
        <span><FontAwesomeIcon icon={faUserMinus} /> Churn window: <strong>{data.churn_window_days}d</strong></span>
        <button
          style={{ ...styles.linkBtn, marginLeft: 'auto' }}
          onClick={() => setShowExplainer((s) => !s)}
        >
          <FontAwesomeIcon icon={faInfoCircle} /> {showExplainer ? 'Hide' : 'How is this calculated?'}
        </button>
      </div>

      {/* ─── Explainer (collapsible) ─────────────────────────────────── */}
      {showExplainer && (
        <div style={styles.explainerPanel}>
          <div style={styles.explainerRow}><strong>Pool definition:</strong> {data.explainer.pool_definition}</div>
          <div style={styles.explainerRow}><strong>Burn:</strong> {data.explainer.burn_definition}</div>
          <div style={styles.explainerRow}><strong>Churn:</strong> {data.explainer.churn_definition}</div>
          <div style={styles.explainerRow}><strong>ISP classification:</strong> {data.explainer.isp_classification}</div>
          <div style={styles.explainerRow}><strong>Why global, not per brand:</strong> {data.explainer.why_global_not_brand}</div>
          <div style={{ ...styles.explainerRow, color: COLORS.textMuted, fontSize: 11 }}>
            Pool segment IDs: {data.pool_segment_ids.join(', ')}
          </div>
        </div>
      )}

      {/* ─── KPI strip ─────────────────────────────────────────────── */}
      <div style={styles.kpiRow}>
        <KpiCard
          icon={faGlobe}
          color={COLORS.accent}
          value={fmt(totals.pool_total)}
          label="Total pool (deduped)"
          sub={`across ${isps.length} ISPs`}
        />
        <KpiCard
          icon={faSeedling}
          color={COLORS.good}
          value={fmt(totals.welcome_pool)}
          label="Welcome-mailable"
          sub={`${fmt(totals.never_mailed)} never mailed`}
        />
        <KpiCard
          icon={faFire}
          color={COLORS.danger}
          value={fmt(data.trigger_counts.now)}
          label="ISPs needing data NOW"
          sub="Refresh today/tomorrow"
        />
        <KpiCard
          icon={faExclamationCircle}
          color="#f97316"
          value={fmt(data.trigger_counts.this_week)}
          label="ISPs needing data this week"
          sub="Within 7 days"
        />
        <KpiCard
          icon={faSnowflake}
          color={COLORS.accent}
          value={fmt(data.trigger_counts.ample)}
          label="AMPLE ISPs"
          sub="Engagement cold — do NOT upload raw"
        />
        <KpiCard
          icon={faChartLine}
          color={COLORS.accentAlt}
          value={fmt(totals.daily_unique_burn_7d)}
          label="Daily unique burn"
          sub={`${data.burn_window_days}d avg, all sending domains`}
        />
      </div>

      {/* ─── Top NOW ISPs ─────────────────────────────────────────────── */}
      {data.top_now_isps.length > 0 && (
        <div style={{ ...styles.panel, borderColor: `${COLORS.danger}55` }}>
          <div style={styles.panelHeader}>
            <div>
              <h2 style={styles.panelTitle}>
                <FontAwesomeIcon icon={faFire} style={{ color: COLORS.danger, marginRight: 8 }} />
                Top Refresh Priority — NOW ISPs
              </h2>
              <p style={styles.panelSubtitle}>
                Cycle ≤ 10 days AND opener share ≥ {data.activation_target_pct}%. These are
                engagement-warm and burning fast. Upload here lands.
              </p>
            </div>
          </div>
          <div style={styles.tableWrap}>
            <table style={styles.table}>
              <thead>
                <tr>
                  <th style={{ ...styles.th, textAlign: 'left' }}>#</th>
                  <th style={{ ...styles.th, textAlign: 'left' }}>ISP</th>
                  <th style={styles.th}>Welcome Pool</th>
                  <th style={styles.th}>Never Mailed</th>
                  <th style={styles.th}>1-3 Sends Left</th>
                  <th style={styles.th}>Daily Burn</th>
                  <th style={styles.th}>Cycle Days</th>
                  <th style={styles.th}>Activation</th>
                  <th style={styles.th}>Action</th>
                </tr>
              </thead>
              <tbody>
                {data.top_now_isps.map((r, i) => (
                  <tr key={r.isp}>
                    <td style={{ ...styles.td, color: COLORS.textMuted }}>{i + 1}</td>
                    <td style={{ ...styles.td, textAlign: 'left', fontWeight: 700 }}>
                      {ISP_LABEL[r.isp] ?? r.isp}
                    </td>
                    <td style={styles.tdNum}>{fmt(r.welcome_pool)}</td>
                    <td style={styles.tdNum}>{fmt(r.never_mailed)}</td>
                    <td style={{ ...styles.tdNum, color: COLORS.warn }}>
                      {fmt(r.four_to_five_away + r.two_to_three_away + r.one_send_away)}
                    </td>
                    <td style={{ ...styles.tdNum, color: COLORS.accentAlt, fontWeight: 700 }}>
                      {fmt(r.daily_unique_burn_7d)}
                    </td>
                    <td style={{ ...styles.tdNum, color: COLORS.danger, fontWeight: 700 }}>
                      {fmtDays(r.cycle_days)}
                    </td>
                    <td style={{ ...styles.tdNum, color: COLORS.good }}>
                      {fmtPct(r.activation_pct)}
                    </td>
                    <td style={{ ...styles.td, fontSize: 11, color: COLORS.textSecondary }}>
                      Upload {fmt(r.daily_unique_burn_7d * 14)}+ {ISP_LABEL[r.isp] ?? r.isp} subs (2 weeks of burn)
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {/* ─── Charts row ─────────────────────────────────────────────── */}
      <div style={styles.chartRow}>
        <div style={styles.chartPanel}>
          <h3 style={styles.chartTitle}>Cycle Days by ISP — runway until next refresh</h3>
          <ResponsiveContainer width="100%" height={260}>
            <BarChart data={isps.filter(r => r.daily_unique_burn_7d > 0).map(r => ({
              isp: ISP_LABEL[r.isp] ?? r.isp,
              cycle: r.cycle_days,
              trigger: r.refresh_trigger,
            })).sort((a,b) => a.cycle - b.cycle)}>
              <CartesianGrid stroke="#1e293b" strokeDasharray="3 3" />
              <XAxis dataKey="isp" stroke={COLORS.textMuted} tick={{ fontSize: 11 }} angle={-25} textAnchor="end" height={60} />
              <YAxis stroke={COLORS.textMuted} tick={{ fontSize: 11 }} label={{ value: 'days', angle: -90, position: 'insideLeft', fill: COLORS.textMuted, fontSize: 11 }} />
              <RechartsTooltip
                contentStyle={{ background: COLORS.bgPanel, border: `1px solid ${COLORS.borderStrong}`, borderRadius: 6 }}
                labelStyle={{ color: COLORS.textPrimary }}
              />
              <ReferenceLine y={10} stroke={COLORS.danger} strokeDasharray="3 3" label={{ value: 'NOW', position: 'right', fill: COLORS.danger, fontSize: 10 }} />
              <ReferenceLine y={20} stroke="#f97316" strokeDasharray="3 3" label={{ value: 'WEEK', position: 'right', fill: '#f97316', fontSize: 10 }} />
              <Bar dataKey="cycle">
                {isps.filter(r => r.daily_unique_burn_7d > 0).sort((a,b) => a.cycle_days - b.cycle_days).map((r, i) => (
                  <RechartsCell key={i} fill={TRIGGER_META[r.refresh_trigger]?.color ?? COLORS.textMuted} />
                ))}
              </Bar>
            </BarChart>
          </ResponsiveContainer>
        </div>

        <div style={styles.chartPanel}>
          <h3 style={styles.chartTitle}>Activation % by ISP — engagement floor at {data.activation_target_pct}%</h3>
          <ResponsiveContainer width="100%" height={260}>
            <BarChart data={isps.map(r => ({
              isp: ISP_LABEL[r.isp] ?? r.isp,
              activation: r.activation_pct,
              met: r.activation_target_met,
            })).sort((a,b) => b.activation - a.activation)}>
              <CartesianGrid stroke="#1e293b" strokeDasharray="3 3" />
              <XAxis dataKey="isp" stroke={COLORS.textMuted} tick={{ fontSize: 11 }} angle={-25} textAnchor="end" height={60} />
              <YAxis stroke={COLORS.textMuted} tick={{ fontSize: 11 }} label={{ value: '%', angle: -90, position: 'insideLeft', fill: COLORS.textMuted, fontSize: 11 }} />
              <RechartsTooltip
                contentStyle={{ background: COLORS.bgPanel, border: `1px solid ${COLORS.borderStrong}`, borderRadius: 6 }}
                labelStyle={{ color: COLORS.textPrimary }}
              />
              <ReferenceLine y={data.activation_target_pct} stroke={COLORS.warn} strokeDasharray="3 3" label={{ value: `${data.activation_target_pct}% target`, position: 'right', fill: COLORS.warn, fontSize: 10 }} />
              <Bar dataKey="activation">
                {isps.sort((a,b) => b.activation_pct - a.activation_pct).map((r, i) => (
                  <RechartsCell key={i} fill={r.activation_target_met ? COLORS.good : COLORS.accent} />
                ))}
              </Bar>
            </BarChart>
          </ResponsiveContainer>
        </div>
      </div>

      {/* ─── ISP Matrix table ───────────────────────────────────────── */}
      <div style={styles.panel}>
        <div style={styles.panelHeader}>
          <div>
            <h2 style={styles.panelTitle}>
              <FontAwesomeIcon icon={faChartLine} style={{ color: COLORS.accent, marginRight: 8 }} />
              ISP Cadence Matrix
            </h2>
            <p style={styles.panelSubtitle}>
              Sorted by refresh urgency. Welcome pool buckets show subscribers by remaining sends
              before saturation (8 sends w/o open). Pool exhaustion = pool ÷ daily burn.
            </p>
          </div>
        </div>
        <div style={styles.tableWrap}>
          <table style={styles.table}>
            <thead>
              <tr>
                <th style={{ ...styles.th, textAlign: 'left' }}>ISP</th>
                <th style={styles.th}>Pool</th>
                <th style={styles.th}>Opener<br/>(Engager)</th>
                <th style={styles.th}>Retired<br/>(8+ sends)</th>
                <th style={styles.th}>Welcome<br/>Mailable</th>
                <th style={styles.th}>Never<br/>Mailed</th>
                <th style={styles.th}>1 Send<br/>Left</th>
                <th style={styles.th}>2-3<br/>Left</th>
                <th style={styles.th}>4-5<br/>Left</th>
                <th style={styles.th}>6-8<br/>Left</th>
                <th style={styles.th}>Daily<br/>Burn</th>
                <th style={styles.th}>Cycle</th>
                <th style={styles.th}>Activation %</th>
                <th style={styles.th}>30d<br/>Unsubs</th>
                <th style={styles.th}>30d<br/>Hard&nbsp;Bnc</th>
                <th style={styles.th}>Refresh Trigger</th>
              </tr>
            </thead>
            <tbody>
              {isps.map((r) => {
                const tm = TRIGGER_META[r.refresh_trigger] ?? TRIGGER_META.UNKNOWN;
                return (
                  <tr key={r.isp}>
                    <td style={{ ...styles.td, textAlign: 'left', fontWeight: 700 }}>
                      {ISP_LABEL[r.isp] ?? r.isp}
                    </td>
                    <td style={styles.tdNum}>{fmt(r.pool_total)}</td>
                    <td style={{ ...styles.tdNum, color: COLORS.good }}>{fmt(r.exempt_opened)}</td>
                    <td style={{ ...styles.tdNum, color: COLORS.textMuted }}>{fmt(r.retired_saturated)}</td>
                    <td style={{ ...styles.tdNum, color: COLORS.accent, fontWeight: 700 }}>{fmt(r.welcome_pool)}</td>
                    <td style={styles.tdNum}>{fmt(r.never_mailed)}</td>
                    <td style={{ ...styles.tdNum, color: r.one_send_away > 0 ? COLORS.danger : COLORS.textMuted }}>
                      {fmt(r.one_send_away)}
                    </td>
                    <td style={{ ...styles.tdNum, color: r.two_to_three_away > 0 ? COLORS.warn : COLORS.textMuted }}>
                      {fmt(r.two_to_three_away)}
                    </td>
                    <td style={styles.tdNum}>{fmt(r.four_to_five_away)}</td>
                    <td style={styles.tdNum}>{fmt(r.six_to_eight_away)}</td>
                    <td style={{ ...styles.tdNum, color: COLORS.accentAlt, fontWeight: 700 }}>
                      {fmt(r.daily_unique_burn_7d)}
                    </td>
                    <td style={{ ...styles.tdNum, color: tm.color, fontWeight: 700 }}>
                      {fmtDays(r.cycle_days)}
                    </td>
                    <td style={{ ...styles.tdNum, color: r.activation_target_met ? COLORS.good : COLORS.accent }}>
                      {fmtPct(r.activation_pct)}
                    </td>
                    <td style={{ ...styles.tdNum, color: r.unsubscribed_30d > 0 ? COLORS.warn : COLORS.textMuted }}>
                      {fmt(r.unsubscribed_30d)}
                    </td>
                    <td style={{ ...styles.tdNum, color: r.hard_bounced_30d > 0 ? COLORS.hardBounce : COLORS.textMuted }}>
                      {fmt(r.hard_bounced_30d)}
                    </td>
                    <td style={{ ...styles.td }}>
                      <div style={{ display: 'inline-flex', alignItems: 'center', gap: 6,
                        padding: '4px 10px', borderRadius: 14, background: `${tm.color}22`,
                        border: `1px solid ${tm.color}66`, color: tm.color, fontSize: 11, fontWeight: 700 }}>
                        <FontAwesomeIcon icon={tm.icon} /> {tm.label}
                      </div>
                      <div style={{ fontSize: 10, color: COLORS.textMuted, marginTop: 4 }}>
                        {r.refresh_reason}
                      </div>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </div>

      {/* ─── Trigger legend ─────────────────────────────────────────── */}
      <div style={styles.panel}>
        <h2 style={styles.panelTitle}>
          <FontAwesomeIcon icon={faUpload} style={{ color: COLORS.accent, marginRight: 8 }} />
          Refresh trigger key
        </h2>
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(280px, 1fr))', gap: 12, marginTop: 12 }}>
          {data.trigger_definitions.map((td) => {
            const tm = TRIGGER_META[td.trigger] ?? TRIGGER_META.UNKNOWN;
            return (
              <div key={td.trigger} style={{
                background: COLORS.bgPanelAlt,
                border: `1px solid ${tm.color}55`,
                borderRadius: 8,
                padding: 12,
              }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 6 }}>
                  <FontAwesomeIcon icon={tm.icon} style={{ color: tm.color }} />
                  <span style={{ fontWeight: 700, color: tm.color }}>{tm.label}</span>
                </div>
                <div style={{ fontSize: 11, color: COLORS.textMuted, marginBottom: 4 }}>
                  Rule: {td.rule}
                </div>
                <div style={{ fontSize: 12, color: COLORS.textPrimary }}>
                  {td.action}
                </div>
              </div>
            );
          })}
        </div>
      </div>

      <div style={styles.footer}>
        Page v{PAGE_VERSION} · API v{data.api_version} · {data.isp_rows.length} ISPs · Source: <code>/api/mailing/analytics/audience-cadence-by-isp</code>
        {fastestCycle && (
          <> · Fastest burn: <strong>{ISP_LABEL[fastestCycle.isp] ?? fastestCycle.isp}</strong> ({fmtDays(fastestCycle.cycle_days)} cycle)</>
        )}
      </div>
    </div>
  );
};

// ─── Sub-components ──────────────────────────────────────────────────────

const KpiCard: React.FC<{
  icon: any; color: string; value: string; label: string; sub: string;
}> = ({ icon, color, value, label, sub }) => (
  <div style={{ ...styles.kpiCard, borderColor: `${color}55` }}>
    <FontAwesomeIcon icon={icon} style={{ color, fontSize: 18 }} />
    <div style={{ ...styles.kpiValue, color }}>{value}</div>
    <div style={styles.kpiLabel}>{label}</div>
    <div style={styles.kpiSub}>{sub}</div>
  </div>
);

// ─── Styles ──────────────────────────────────────────────────────────────

const styles: Record<string, React.CSSProperties> = {
  page: {
    background: COLORS.bgDeep,
    minHeight: '100%',
    padding: 24,
    color: COLORS.textPrimary,
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif',
  },
  header: {
    display: 'flex',
    alignItems: 'flex-start',
    justifyContent: 'space-between',
    marginBottom: 16,
    gap: 16,
  },
  title: { fontSize: 24, fontWeight: 700, margin: 0, color: COLORS.textPrimary, display: 'flex', alignItems: 'center' },
  subtitle: { fontSize: 13, color: COLORS.textSecondary, margin: '6px 0 0', maxWidth: 880, lineHeight: 1.5 },
  refreshBtn: {
    background: COLORS.bgPanel,
    border: `1px solid ${COLORS.borderStrong}`,
    color: COLORS.textPrimary,
    borderRadius: 6,
    padding: '8px 14px',
    fontSize: 12,
    cursor: 'pointer',
    display: 'inline-flex',
    alignItems: 'center',
    gap: 6,
    fontWeight: 500,
    whiteSpace: 'nowrap',
  },
  linkBtn: {
    background: 'transparent',
    border: 'none',
    color: COLORS.accent,
    cursor: 'pointer',
    fontSize: 11,
    fontWeight: 500,
    display: 'inline-flex',
    alignItems: 'center',
    gap: 4,
  },
  metaStrip: {
    display: 'flex',
    gap: 24,
    flexWrap: 'wrap',
    background: COLORS.bgPanelAlt,
    border: `1px solid ${COLORS.border}`,
    borderRadius: 8,
    padding: '10px 14px',
    fontSize: 12,
    color: COLORS.textSecondary,
    marginBottom: 16,
    alignItems: 'center',
  },
  explainerPanel: {
    background: COLORS.bgPanelAlt,
    border: `1px solid ${COLORS.accent}55`,
    borderRadius: 8,
    padding: 16,
    marginBottom: 16,
    fontSize: 12,
    color: COLORS.textPrimary,
    lineHeight: 1.6,
  },
  explainerRow: { marginBottom: 8 },
  kpiRow: {
    display: 'grid',
    gridTemplateColumns: 'repeat(auto-fit, minmax(180px, 1fr))',
    gap: 10,
    marginBottom: 16,
  },
  kpiCard: {
    background: COLORS.bgPanel,
    border: `1px solid ${COLORS.border}`,
    borderRadius: 8,
    padding: 12,
    display: 'flex',
    flexDirection: 'column',
    gap: 4,
  },
  kpiValue: { fontSize: 22, fontWeight: 700, marginTop: 4 },
  kpiLabel: { fontSize: 11, color: COLORS.textPrimary, fontWeight: 500 },
  kpiSub: { fontSize: 10, color: COLORS.textMuted },
  panel: {
    background: COLORS.bgPanel,
    border: `1px solid ${COLORS.border}`,
    borderRadius: 8,
    padding: 16,
    marginBottom: 16,
  },
  panelHeader: { display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: 8 },
  panelTitle: { fontSize: 16, fontWeight: 700, margin: 0, color: COLORS.textPrimary },
  panelSubtitle: { fontSize: 11, color: COLORS.textMuted, margin: '4px 0 0', maxWidth: 720 },
  tableWrap: { overflowX: 'auto', marginTop: 8 },
  table: { width: '100%', borderCollapse: 'collapse', fontSize: 12 },
  th: {
    padding: '8px 10px',
    textAlign: 'right',
    color: COLORS.textMuted,
    fontWeight: 500,
    borderBottom: `1px solid ${COLORS.border}`,
    fontSize: 11,
    whiteSpace: 'nowrap',
    verticalAlign: 'bottom',
  },
  td: {
    padding: '10px 10px',
    color: COLORS.textPrimary,
    borderBottom: `1px solid ${COLORS.border}`,
  },
  tdNum: {
    padding: '10px 10px',
    color: COLORS.textPrimary,
    borderBottom: `1px solid ${COLORS.border}`,
    textAlign: 'right',
    fontVariantNumeric: 'tabular-nums',
  },
  chartRow: {
    display: 'grid',
    gridTemplateColumns: 'repeat(auto-fit, minmax(420px, 1fr))',
    gap: 16,
    marginBottom: 16,
  },
  chartPanel: {
    background: COLORS.bgPanel,
    border: `1px solid ${COLORS.border}`,
    borderRadius: 8,
    padding: 16,
  },
  chartTitle: { fontSize: 13, fontWeight: 600, color: COLORS.textPrimary, margin: '0 0 12px' },
  loadingShell: {
    minHeight: 400,
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    color: COLORS.textSecondary,
    background: COLORS.bgDeep,
    fontSize: 14,
    gap: 8,
  },
  errorShell: {
    minHeight: 400,
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    color: COLORS.danger,
    background: COLORS.bgDeep,
    fontSize: 14,
    gap: 16,
    flexDirection: 'column',
  },
  footer: {
    fontSize: 10,
    color: COLORS.textMuted,
    textAlign: 'center',
    padding: 16,
  },
};

export default AudienceCadenceByCell;
