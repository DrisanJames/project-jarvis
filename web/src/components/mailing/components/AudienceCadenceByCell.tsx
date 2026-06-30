// Audience Cadence — Messages-to-Convert KPIs per ISP + the ISP Doctrine canon
//
// PAGE_VERSION 3.0 (2026-06-12) — REPURPOSE. Operator ask: "dive into the KPIs
// of an audience member for a given ISP as we know it: establish evolving
// statistics about the amount of messages needed to convert in this ISP
// environment. The doctrine should live here."
//
// Primary surface: per ISP family, the evolving cadence statistics —
//   - messages-to-first-engagement distribution (p25/p50/p75/avg, sampled)
//   - messages-to-CONVERSION distribution (exact; terminal event =
//     mailing_offer_suppressions reason='converted')
//   - conversion per 10k window sends + 4-week weekly trend of convert-p50
// Clicking an ISP expands its full distribution AND its standing doctrine
// (distilled from docs/*_SENDING_DOCTRINE.md, operator-editable via PUT).
//
// Secondary (collapsed): the 2.0 welcome-pool refresh view
// (/api/mailing/analytics/audience-cadence-by-isp) is retained in condensed
// form — it still answers "when do I need to upload more data per ISP?".
//
// Backend: internal/api/audience_cadence_kpis.go
//   GET /api/mailing/audience-cadence/kpis?days=60
//   GET /api/mailing/audience-cadence/doctrines
//   PUT /api/mailing/audience-cadence/doctrines/{isp}

import React, { useCallback, useEffect, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faSpinner, faSyncAlt, faExclamationTriangle, faChevronDown, faChevronRight,
  faBookOpen, faPen, faChartLine, faInfoCircle, faGlobe, faBullseye,
  faHeartPulse, faSave, faTimes,
} from '@fortawesome/free-solid-svg-icons';
import {
  ResponsiveContainer, LineChart, Line, XAxis, YAxis,
  Tooltip as RechartsTooltip,
} from 'recharts';
import { apiFetch } from '../shared/apiFetch';
import { colors } from '../shared/theme';
import { EmptyState, SectionError, PortalKeyframes } from '../shared/ui';

const PAGE_VERSION = '3.0';

// ─── Types (mirror Go response shapes) ─────────────────────────────────────

interface CadenceWeekPoint {
  week_start: string;
  converters: number;
  msgs_to_convert_p50: number;
}

interface CadenceISPKPI {
  isp: string;
  engaged_sample: number;
  msgs_to_engage_p25: number;
  msgs_to_engage_p50: number;
  msgs_to_engage_p75: number;
  msgs_to_engage_avg: number;
  converters: number;
  msgs_to_convert_p25: number;
  msgs_to_convert_p50: number;
  msgs_to_convert_p75: number;
  msgs_to_convert_avg: number;
  window_sends: number;
  conversion_per_10k_sends: number;
  weekly_trend: CadenceWeekPoint[];
}

interface CadenceKPIResponse {
  api_version: string;
  as_of: string;
  days: number;
  window_start: string;
  engaged_sample_limit: number;
  engaged_sample_total: number;
  method_note: string;
  isps: CadenceISPKPI[];
  // Partial / degraded-response signalling (backend may omit on older builds).
  computing?: boolean;
  engage_available?: boolean;
  partial_errors?: string[];
}

interface ISPDoctrine {
  isp: string;
  title: string;
  doctrine_md: string;
  north_stars: string;
  health_bands: string;
  updated_at: string;
}

interface DoctrineDraft {
  title: string;
  doctrine_md: string;
  north_stars: string;
  health_bands: string;
}

// Legacy 2.0 view (condensed subset of the snapshot endpoint).
interface LegacyISPRow {
  isp: string;
  pool_total: number;
  welcome_pool: number;
  never_mailed: number;
  daily_unique_burn_7d: number;
  cycle_days: number;
  activation_pct: number;
  refresh_trigger: string;
  refresh_reason: string;
}

interface LegacyResponse {
  snapshot_refreshed_at: string;
  saturation_threshold: number;
  isp_rows: LegacyISPRow[];
}

// ─── Style tokens ───────────────────────────────────────────────────────────
//
// Mapped onto the canonical indigo design system (shared/theme.ts) so this
// screen matches the Delivery Queue gold standard. The local C.* names are kept
// (the JSX below references them throughout) but now resolve to theme tokens —
// note the hex accents (indigo/green/amber/red/heading) feed the `${color}NN`
// alpha-suffix trick used in the styles, so they must stay hex.

const C = {
  bgDeep:       colors.appBgSolid,         // deep slate-blue page
  bgPanel:      colors.panelBgSolid,       // solid panel (inputs, tooltips)
  bgTable:      colors.panelBg,            // translucent slate-blue surface
  border:       colors.divider,            // indigo-tinted hairline
  borderStrong: colors.panelBorderStrong,  // stronger indigo border
  heading:      colors.heading,            // #dbeafe
  text:         colors.text,
  textSec:      colors.textMuted,
  textMuted:    colors.textFaint,
  indigo:       colors.indigo400,          // #818cf8
  indigoDeep:   colors.indigo500,          // #6366f1
  green:        colors.success,            // #22c55e
  amber:        colors.warning,            // #f59e0b
  red:          colors.danger,             // #ef4444
};

const ISP_LABEL: Record<string, string> = {
  apple: 'Apple iCloud',
  microsoft: 'Microsoft',
  gmail: 'Gmail',
  yahoo: 'Yahoo',
  aol: 'AOL',
  comcast: 'Comcast / Xfinity',
  att: 'AT&T / SBC',
  charter: 'Charter / Spectrum',
  other: 'Other / B2B',
};

const fmt = (n: number) => n.toLocaleString('en-US');
const fmt1 = (n: number) => (Number.isFinite(n) ? n.toFixed(1) : '—');
const fmt2 = (n: number) => (Number.isFinite(n) ? n.toFixed(2) : '—');
const label = (isp: string) => ISP_LABEL[isp] ?? isp;

// ─── Markdown-lite renderer (bold / headers / bullets) ─────────────────────

const renderInline = (text: string): React.ReactNode[] =>
  text.split(/\*\*(.+?)\*\*/g).map((part, i) =>
    i % 2 === 1
      ? <strong key={i} style={{ color: C.heading }}>{part}</strong>
      : <React.Fragment key={i}>{part}</React.Fragment>
  );

const MarkdownLite: React.FC<{ text: string }> = ({ text }) => {
  const lines = text.split(/\r?\n/);
  return (
    <div style={{ fontSize: 12.5, lineHeight: 1.65, color: C.text }}>
      {lines.map((line, i) => {
        const t = line.trim();
        if (t === '') return <div key={i} style={{ height: 8 }} />;
        const h = t.match(/^#{1,4}\s+(.*)$/);
        if (h) {
          return (
            <div key={i} style={{ color: C.heading, fontWeight: 700, fontSize: 13, margin: '8px 0 4px' }}>
              {renderInline(h[1])}
            </div>
          );
        }
        const b = t.match(/^[-*]\s+(.*)$/);
        if (b) {
          return (
            <div key={i} style={{ display: 'flex', gap: 8, marginBottom: 3 }}>
              <span style={{ color: C.indigo }}>•</span>
              <span>{renderInline(b[1])}</span>
            </div>
          );
        }
        return <div key={i} style={{ marginBottom: 2 }}>{renderInline(t)}</div>;
      })}
    </div>
  );
};

// ─── Range bar: p25–p75 band with p50 marker ───────────────────────────────

const RangeBar: React.FC<{ p25: number; p50: number; p75: number; max: number; color: string }> = ({
  p25, p50, p75, max, color,
}) => {
  const scale = max > 0 ? max : 1;
  const pct = (v: number) => `${Math.min(100, (v / scale) * 100)}%`;
  return (
    <div style={{ position: 'relative', height: 8, background: 'rgba(255,255,255,0.05)', borderRadius: 4, minWidth: 90 }}>
      <div style={{
        position: 'absolute', left: pct(p25), width: `calc(${pct(p75)} - ${pct(p25)})`,
        top: 0, bottom: 0, background: `${color}44`, borderRadius: 4,
      }} />
      <div style={{
        position: 'absolute', left: pct(p50), top: -2, bottom: -2, width: 3,
        background: color, borderRadius: 2, transform: 'translateX(-1px)',
      }} />
    </div>
  );
};

// ─── Headline insights ──────────────────────────────────────────────────────

const MIN_CONVERTERS = 5;

function ispHeadline(r: CadenceISPKPI, engageComputing: boolean): string {
  const name = label(r.isp);
  if (r.converters >= MIN_CONVERTERS) {
    if (engageComputing) {
      return `${name} converts at message ~${Math.round(r.msgs_to_convert_p50)} (p50); engagement cadence still computing (large dataset). ${fmt2(r.conversion_per_10k_sends)} conversions per 10k sends.`;
    }
    return `${name} converts at message ~${Math.round(r.msgs_to_convert_p50)} (p50), engages at ~${Math.round(r.msgs_to_engage_p50)}; ${fmt2(r.conversion_per_10k_sends)} conversions per 10k sends.`;
  }
  if (engageComputing) {
    return `${name}: engagement cadence still computing (large dataset)…`;
  }
  if (r.engaged_sample > 0) {
    return `${name} engages at message ~${Math.round(r.msgs_to_engage_p50)} (p50); only ${r.converters} converter${r.converters === 1 ? '' : 's'} in window — convert read not yet stable.`;
  }
  return `${name}: no engagement sample in window (this route may not report opens — see best practices).`;
}

function globalHeadline(isps: CadenceISPKPI[]): string | null {
  const eligible = isps.filter((r) => r.converters >= MIN_CONVERTERS && r.msgs_to_convert_p50 > 0);
  if (eligible.length < 2) return null;
  const sorted = [...eligible].sort((a, b) => a.msgs_to_convert_p50 - b.msgs_to_convert_p50);
  const fast = sorted[0];
  const slow = sorted[sorted.length - 1];
  if (fast.isp === slow.isp) return null;
  return `${label(fast.isp)} converts at message ~${Math.round(fast.msgs_to_convert_p50)} (p50); ${label(slow.isp)} at ~${Math.round(slow.msgs_to_convert_p50)} — front-load ${label(fast.isp)}, ladder ${label(slow.isp)}.`;
}

// ─── Main component (export name kept — existing mount depends on it) ──────

export const AudienceCadenceByCell: React.FC = () => {
  const [days, setDays] = useState<number>(60);
  const [data, setData] = useState<CadenceKPIResponse | null>(null);
  const [doctrines, setDoctrines] = useState<Record<string, ISPDoctrine>>({});
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [expanded, setExpanded] = useState<string | null>(null);

  // doctrine editor
  const [editingISP, setEditingISP] = useState<string | null>(null);
  const [draft, setDraft] = useState<DoctrineDraft>({ title: '', doctrine_md: '', north_stars: '', health_bands: '' });
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState('');

  // legacy section
  const [legacyOpen, setLegacyOpen] = useState(false);
  const [legacy, setLegacy] = useState<LegacyResponse | null>(null);
  const [legacyLoading, setLegacyLoading] = useState(false);
  const [legacyError, setLegacyError] = useState('');

  const fetchKPIs = useCallback(async (d: number) => {
    setLoading(true);
    try {
      const res = await apiFetch(`/api/mailing/audience-cadence/kpis?days=${d}`, { credentials: 'include' });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const json: CadenceKPIResponse = await res.json();
      setData(json);
      setError('');
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  const fetchDoctrines = useCallback(async () => {
    try {
      const res = await apiFetch('/api/mailing/audience-cadence/doctrines', { credentials: 'include' });
      if (!res.ok) return;
      const json: { doctrines: ISPDoctrine[] } = await res.json();
      const map: Record<string, ISPDoctrine> = {};
      for (const d of json.doctrines) map[d.isp] = d;
      setDoctrines(map);
    } catch {
      /* doctrine list is non-fatal — KPI table still renders */
    }
  }, []);

  useEffect(() => { fetchKPIs(days); }, [fetchKPIs, days]);
  useEffect(() => { fetchDoctrines(); }, [fetchDoctrines]);

  const openLegacy = useCallback(async () => {
    setLegacyOpen((o) => !o);
    if (legacy || legacyLoading) return;
    setLegacyLoading(true);
    try {
      const res = await apiFetch('/api/mailing/analytics/audience-cadence-by-isp', { credentials: 'include' });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const json: LegacyResponse = await res.json();
      setLegacy(json);
      setLegacyError('');
    } catch (e) {
      setLegacyError(e instanceof Error ? e.message : String(e));
    } finally {
      setLegacyLoading(false);
    }
  }, [legacy, legacyLoading]);

  const startEdit = (isp: string) => {
    const d = doctrines[isp];
    setDraft({
      title: d?.title ?? `${label(isp)} — best practices`,
      doctrine_md: d?.doctrine_md ?? '',
      north_stars: d?.north_stars ?? '',
      health_bands: d?.health_bands ?? '',
    });
    setSaveError('');
    setEditingISP(isp);
  };

  const saveDoctrine = async () => {
    if (!editingISP) return;
    setSaving(true);
    setSaveError('');
    try {
      const res = await apiFetch(`/api/mailing/audience-cadence/doctrines/${editingISP}`, {
        method: 'PUT',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(draft),
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const saved: ISPDoctrine = await res.json();
      setDoctrines((m) => ({ ...m, [saved.isp]: saved }));
      setEditingISP(null);
    } catch (e) {
      setSaveError(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  };

  if (loading && !data) {
    return (
      <div style={styles.loadingShell}>
        <PortalKeyframes />
        <FontAwesomeIcon icon={faSpinner} spin style={{ color: C.indigo }} /> Loading cadence KPIs…
      </div>
    );
  }
  if (error || !data) {
    return (
      <div style={styles.errorShell}>
        {error ? (
          <div style={{ width: '100%', maxWidth: 520 }}>
            <SectionError label="Audience cadence KPIs" error={error} />
          </div>
        ) : (
          <EmptyState icon={faChartLine} title="No cadence data yet" hint="No KPI sample is available for this window." />
        )}
        <button style={styles.btn} onClick={() => fetchKPIs(days)}>
          <FontAwesomeIcon icon={faSyncAlt} /> Retry
        </button>
      </div>
    );
  }

  // Union of KPI rows + doctrine-only ISPs (a doctrine can exist for an ISP
  // with no PG-visible sample — gmail is SES pixel-blind).
  const kpiByISP: Record<string, CadenceISPKPI> = {};
  for (const r of data.isps) kpiByISP[r.isp] = r;
  const order = ['apple', 'microsoft', 'gmail', 'yahoo', 'aol', 'comcast', 'att', 'charter', 'other'];
  const allISPs = order.filter((i) => kpiByISP[i] || doctrines[i])
    .concat(Object.keys(doctrines).filter((i) => !order.includes(i)));

  const maxEngageP75 = Math.max(1, ...data.isps.map((r) => r.msgs_to_engage_p75));
  const maxConvertP75 = Math.max(1, ...data.isps.map((r) => r.msgs_to_convert_p75));
  const headline = globalHeadline(data.isps);

  // The messages-to-first-engagement cohort scan is the heavy section; the
  // backend reports engage_available=false (with computing=true) while it is
  // still being computed on a large dataset. Show a friendly placeholder for
  // the engage columns instead of a hard error or misleading zeros.
  const engageComputing = data.engage_available === false;
  // Small inline indigo "computing" chip reused in the engage cells.
  const computingChip = (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6, color: C.indigo, fontSize: 11 }}>
      <FontAwesomeIcon icon={faSpinner} spin /> computing…
    </span>
  );

  return (
    <div style={styles.page}>
      <PortalKeyframes />
      {/* ── Header ── */}
      <div style={styles.header}>
        <div>
          <h1 style={styles.title}>
            <FontAwesomeIcon icon={faBullseye} style={{ color: C.indigo, marginRight: 12 }} />
            Audience Cadence — Messages to Convert, per Mailbox Provider
          </h1>
          <p style={styles.subtitle}>
            How many messages does an audience member need before they engage — and before they
            convert — in each mailbox provider environment? Evolving statistics from the live send,
            tracking, and conversion data. Click a mailbox provider for its full distribution and its <strong>standing best practices</strong>.
          </p>
        </div>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          {[30, 60, 90].map((d) => (
            <button
              key={d}
              style={{
                ...styles.btn,
                ...(days === d ? { borderColor: C.indigo, color: C.indigo, background: `${C.indigoDeep}22` } : {}),
              }}
              onClick={() => setDays(d)}
              disabled={loading}
            >
              {d}d
            </button>
          ))}
          <button style={styles.btn} onClick={() => fetchKPIs(days)} disabled={loading}>
            <FontAwesomeIcon icon={loading ? faSpinner : faSyncAlt} spin={loading} /> Reload
          </button>
        </div>
      </div>

      {/* ── Global headline ── */}
      {headline && (
        <div style={styles.headlineStrip}>
          <FontAwesomeIcon icon={faChartLine} style={{ color: C.green, marginRight: 8 }} />
          {headline}
        </div>
      )}

      {/* ── Computing (partial / degraded) banner ── */}
      {engageComputing && (
        <div style={styles.computingStrip}>
          <FontAwesomeIcon icon={faSpinner} spin style={{ color: C.indigo, marginRight: 8 }} />
          Engagement cadence is <strong>still computing</strong> over a large dataset — the
          messages-to-engage distribution will appear once the scan completes. Conversion,
          trend, and send KPIs below are live in the meantime.
        </div>
      )}

      {/* ── Sampling caption ── */}
      <div style={styles.captionStrip}>
        <FontAwesomeIcon icon={faInfoCircle} style={{ marginRight: 6 }} />
        Window: last <strong>{data.days} days</strong> · engaged sample {fmt(data.engaged_sample_total)} of max{' '}
        {fmt(data.engaged_sample_limit)} · {data.method_note}
      </div>

      {/* ── Per-ISP rows ── */}
      <div style={styles.panel}>
        <table style={styles.table}>
          <thead>
            <tr>
              <th style={{ ...styles.th, textAlign: 'left', width: 28 }} />
              <th style={{ ...styles.th, textAlign: 'left' }}>Mailbox Provider</th>
              <th style={{ ...styles.th, textAlign: 'left', minWidth: 140 }}>Msgs → engage (p50, p25–p75)</th>
              <th style={styles.th}>Msgs → convert<br />p50</th>
              <th style={styles.th}>Conv /<br />10k sends</th>
              <th style={styles.th}>Engaged<br />sample</th>
              <th style={styles.th}>Converters</th>
              <th style={{ ...styles.th, textAlign: 'center', minWidth: 130 }}>Convert-p50 trend (4w)</th>
            </tr>
          </thead>
          <tbody>
            {allISPs.map((isp) => {
              const r = kpiByISP[isp];
              const doc = doctrines[isp];
              const isOpen = expanded === isp;
              return (
                <React.Fragment key={isp}>
                  <tr
                    style={{ cursor: 'pointer', background: isOpen ? `${C.indigoDeep}14` : 'transparent' }}
                    onClick={() => setExpanded(isOpen ? null : isp)}
                  >
                    <td style={styles.td}>
                      <FontAwesomeIcon icon={isOpen ? faChevronDown : faChevronRight} style={{ color: C.textMuted, fontSize: 11 }} />
                    </td>
                    <td style={{ ...styles.td, textAlign: 'left' }}>
                      <div style={{ fontWeight: 700, color: C.heading }}>
                        {label(isp)}
                        {doc && (
                          <FontAwesomeIcon icon={faBookOpen} style={{ color: C.indigo, marginLeft: 8, fontSize: 11 }} title="Best practices on file" />
                        )}
                      </div>
                      <div style={{ fontSize: 10.5, color: C.textSec, maxWidth: 360, marginTop: 2 }}>
                        {r ? ispHeadline(r, engageComputing) : 'No reportable sample in window — best practices only.'}
                      </div>
                    </td>
                    <td style={{ ...styles.td, textAlign: 'left' }}>
                      {engageComputing ? computingChip
                        : r && r.engaged_sample > 0 ? (
                        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
                          <span style={{ fontWeight: 700, color: C.indigo, minWidth: 30, textAlign: 'right' }}>
                            {fmt1(r.msgs_to_engage_p50)}
                          </span>
                          <RangeBar p25={r.msgs_to_engage_p25} p50={r.msgs_to_engage_p50} p75={r.msgs_to_engage_p75} max={maxEngageP75} color={C.indigo} />
                          <span style={{ fontSize: 10, color: C.textMuted, whiteSpace: 'nowrap' }}>
                            {fmt1(r.msgs_to_engage_p25)}–{fmt1(r.msgs_to_engage_p75)}
                          </span>
                        </div>
                      ) : <span style={{ color: C.textMuted }}>—</span>}
                    </td>
                    <td style={{ ...styles.tdNum, color: C.green, fontWeight: 700 }}>
                      {r && r.converters > 0 ? fmt1(r.msgs_to_convert_p50) : '—'}
                    </td>
                    <td style={{ ...styles.tdNum, color: r && r.conversion_per_10k_sends > 0 ? C.amber : C.textMuted, fontWeight: 600 }}>
                      {r && r.window_sends > 0 ? fmt2(r.conversion_per_10k_sends) : '—'}
                    </td>
                    <td style={styles.tdNum}>
                      {engageComputing ? <FontAwesomeIcon icon={faSpinner} spin style={{ color: C.indigo }} /> : r ? fmt(r.engaged_sample) : '—'}
                    </td>
                    <td style={{ ...styles.tdNum, color: r && r.converters >= MIN_CONVERTERS ? C.text : C.textMuted }}>
                      {r ? fmt(r.converters) : '—'}
                    </td>
                    <td style={{ ...styles.td, textAlign: 'center' }}>
                      {r && r.weekly_trend.length > 1 ? (
                        <div style={{ width: 130, height: 36, margin: '0 auto' }}>
                          <ResponsiveContainer width="100%" height="100%">
                            <LineChart data={r.weekly_trend} margin={{ top: 4, right: 4, bottom: 0, left: 4 }}>
                              <XAxis dataKey="week_start" hide />
                              <YAxis hide domain={['auto', 'auto']} />
                              <RechartsTooltip
                                contentStyle={{ background: C.bgPanel, border: `1px solid ${C.borderStrong}`, borderRadius: 6, fontSize: 11 }}
                                labelStyle={{ color: C.heading }}
                                formatter={(v: number | string) => [fmt1(Number(v)), 'msgs→convert p50']}
                              />
                              <Line type="monotone" dataKey="msgs_to_convert_p50" stroke={C.green} strokeWidth={2} dot={{ r: 2, fill: C.green }} />
                            </LineChart>
                          </ResponsiveContainer>
                        </div>
                      ) : <span style={{ fontSize: 10, color: C.textMuted }}>insufficient</span>}
                    </td>
                  </tr>

                  {/* ── Expanded: distribution + doctrine ── */}
                  {isOpen && (
                    <tr>
                      <td colSpan={8} style={{ padding: 0, borderBottom: `1px solid ${C.border}` }}>
                        <div style={styles.expandWrap}>
                          {/* Distribution detail */}
                          <div style={styles.expandCol}>
                            <h3 style={styles.expandTitle}>
                              <FontAwesomeIcon icon={faChartLine} style={{ color: C.indigo, marginRight: 8 }} />
                              Distribution — {label(isp)} ({data.days}d window)
                            </h3>
                            {r ? (
                              <>
                                <table style={styles.miniTable}>
                                  <thead>
                                    <tr>
                                      <th style={{ ...styles.miniTh, textAlign: 'left' }}>Metric</th>
                                      <th style={styles.miniTh}>p25</th>
                                      <th style={styles.miniTh}>p50</th>
                                      <th style={styles.miniTh}>p75</th>
                                      <th style={styles.miniTh}>avg</th>
                                      <th style={styles.miniTh}>n</th>
                                    </tr>
                                  </thead>
                                  <tbody>
                                    <tr>
                                      <td style={{ ...styles.miniTd, textAlign: 'left', color: C.indigo, fontWeight: 600 }}>Messages → first engagement</td>
                                      {engageComputing ? (
                                        <td style={{ ...styles.miniTd, textAlign: 'center', color: C.indigo }} colSpan={5}>
                                          {computingChip}
                                        </td>
                                      ) : (
                                        <>
                                          <td style={styles.miniTd}>{fmt1(r.msgs_to_engage_p25)}</td>
                                          <td style={{ ...styles.miniTd, fontWeight: 700, color: C.heading }}>{fmt1(r.msgs_to_engage_p50)}</td>
                                          <td style={styles.miniTd}>{fmt1(r.msgs_to_engage_p75)}</td>
                                          <td style={styles.miniTd}>{fmt1(r.msgs_to_engage_avg)}</td>
                                          <td style={styles.miniTd}>{fmt(r.engaged_sample)}</td>
                                        </>
                                      )}
                                    </tr>
                                    <tr>
                                      <td style={{ ...styles.miniTd, textAlign: 'left', color: C.green, fontWeight: 600 }}>Messages → conversion</td>
                                      <td style={styles.miniTd}>{fmt1(r.msgs_to_convert_p25)}</td>
                                      <td style={{ ...styles.miniTd, fontWeight: 700, color: C.heading }}>{fmt1(r.msgs_to_convert_p50)}</td>
                                      <td style={styles.miniTd}>{fmt1(r.msgs_to_convert_p75)}</td>
                                      <td style={styles.miniTd}>{fmt1(r.msgs_to_convert_avg)}</td>
                                      <td style={styles.miniTd}>{fmt(r.converters)}</td>
                                    </tr>
                                  </tbody>
                                </table>
                                <div style={{ marginTop: 10 }}>
                                  <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 6 }}>
                                    <span style={{ fontSize: 11, color: C.textSec, minWidth: 64 }}>engage</span>
                                    {engageComputing
                                      ? computingChip
                                      : <RangeBar p25={r.msgs_to_engage_p25} p50={r.msgs_to_engage_p50} p75={r.msgs_to_engage_p75} max={Math.max(maxEngageP75, maxConvertP75)} color={C.indigo} />}
                                  </div>
                                  <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
                                    <span style={{ fontSize: 11, color: C.textSec, minWidth: 64 }}>convert</span>
                                    <RangeBar p25={r.msgs_to_convert_p25} p50={r.msgs_to_convert_p50} p75={r.msgs_to_convert_p75} max={Math.max(maxEngageP75, maxConvertP75)} color={C.green} />
                                  </div>
                                </div>
                                <div style={{ fontSize: 11.5, color: C.textSec, marginTop: 12 }}>
                                  Window sends: <strong style={{ color: C.text }}>{fmt(r.window_sends)}</strong>
                                  {' '}· converters: <strong style={{ color: C.text }}>{fmt(r.converters)}</strong>
                                  {' '}· conversion per 10k sends:{' '}
                                  <strong style={{ color: C.amber }}>{fmt2(r.conversion_per_10k_sends)}</strong>
                                </div>
                                {r.weekly_trend.length > 0 && (
                                  <div style={{ marginTop: 10 }}>
                                    <div style={{ fontSize: 11, color: C.textSec, marginBottom: 4 }}>Weekly msgs→convert p50 (week of conversion):</div>
                                    {r.weekly_trend.map((wp) => (
                                      <div key={wp.week_start} style={{ fontSize: 11.5, color: C.text, display: 'flex', gap: 12 }}>
                                        <span style={{ color: C.textMuted, minWidth: 80 }}>{wp.week_start}</span>
                                        <span style={{ color: C.green, fontWeight: 600, minWidth: 40, textAlign: 'right' }}>{fmt1(wp.msgs_to_convert_p50)}</span>
                                        <span style={{ color: C.textMuted }}>({fmt(wp.converters)} converters)</span>
                                      </div>
                                    ))}
                                  </div>
                                )}
                              </>
                            ) : (
                              <div style={{ fontSize: 12, color: C.textMuted }}>
                                No KPI sample for this mailbox provider in the window. If this sending route
                                is relayed (Gmail and the relay-routed brands), engagement is not reported
                                here — judge it from Reporting per the best practices.
                              </div>
                            )}
                          </div>

                          {/* Doctrine */}
                          <div style={{ ...styles.expandCol, borderLeft: `1px solid ${C.border}` }}>
                            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start' }}>
                              <h3 style={styles.expandTitle}>
                                <FontAwesomeIcon icon={faBookOpen} style={{ color: C.amber, marginRight: 8 }} />
                                {doc?.title || `${label(isp)} — no best practices on file`}
                              </h3>
                              {editingISP !== isp && (
                                <button style={styles.btnSmall} onClick={(e) => { e.stopPropagation(); startEdit(isp); }}>
                                  <FontAwesomeIcon icon={faPen} /> {doc ? 'Edit' : 'Add best practices'}
                                </button>
                              )}
                            </div>

                            {editingISP === isp ? (
                              <div onClick={(e) => e.stopPropagation()}>
                                <label style={styles.editLabel}>Title</label>
                                <input
                                  style={styles.editInput}
                                  value={draft.title}
                                  onChange={(e) => setDraft({ ...draft, title: e.target.value })}
                                />
                                <label style={styles.editLabel}>Best practices (markdown: **bold**, ## headers, - bullets)</label>
                                <textarea
                                  style={{ ...styles.editArea, minHeight: 160 }}
                                  value={draft.doctrine_md}
                                  onChange={(e) => setDraft({ ...draft, doctrine_md: e.target.value })}
                                />
                                <label style={styles.editLabel}>North stars</label>
                                <textarea
                                  style={{ ...styles.editArea, minHeight: 90 }}
                                  value={draft.north_stars}
                                  onChange={(e) => setDraft({ ...draft, north_stars: e.target.value })}
                                />
                                <label style={styles.editLabel}>Health bands</label>
                                <textarea
                                  style={{ ...styles.editArea, minHeight: 70 }}
                                  value={draft.health_bands}
                                  onChange={(e) => setDraft({ ...draft, health_bands: e.target.value })}
                                />
                                {saveError && (
                                  <div style={{ color: C.red, fontSize: 11.5, marginBottom: 8 }}>
                                    <FontAwesomeIcon icon={faExclamationTriangle} /> Save failed: {saveError}
                                  </div>
                                )}
                                <div style={{ display: 'flex', gap: 8 }}>
                                  <button
                                    style={{ ...styles.btnSmall, borderColor: C.green, color: C.green }}
                                    onClick={saveDoctrine}
                                    disabled={saving}
                                  >
                                    <FontAwesomeIcon icon={saving ? faSpinner : faSave} spin={saving} /> Save
                                  </button>
                                  <button style={styles.btnSmall} onClick={() => setEditingISP(null)} disabled={saving}>
                                    <FontAwesomeIcon icon={faTimes} /> Cancel
                                  </button>
                                </div>
                              </div>
                            ) : doc ? (
                              <>
                                <MarkdownLite text={doc.doctrine_md} />
                                {doc.north_stars && (
                                  <div style={styles.doctrineBlock}>
                                    <div style={{ ...styles.doctrineBlockTitle, color: C.green }}>
                                      <FontAwesomeIcon icon={faBullseye} style={{ marginRight: 6 }} /> North stars
                                    </div>
                                    <MarkdownLite text={doc.north_stars} />
                                  </div>
                                )}
                                {doc.health_bands && (
                                  <div style={styles.doctrineBlock}>
                                    <div style={{ ...styles.doctrineBlockTitle, color: C.amber }}>
                                      <FontAwesomeIcon icon={faHeartPulse} style={{ marginRight: 6 }} /> Health bands
                                    </div>
                                    <MarkdownLite text={doc.health_bands} />
                                  </div>
                                )}
                                <div style={{ fontSize: 10, color: C.textMuted, marginTop: 10 }}>
                                  Last updated {new Date(doc.updated_at).toLocaleString('en-US', { hour12: false })}
                                </div>
                              </>
                            ) : (
                              <div style={{ fontSize: 12, color: C.textMuted }}>
                                No best practices set for this mailbox provider yet. Use "Add best practices" to write some.
                              </div>
                            )}
                          </div>
                        </div>
                      </td>
                    </tr>
                  )}
                </React.Fragment>
              );
            })}
          </tbody>
        </table>
      </div>

      {/* ── Legacy welcome-pool view (collapsed secondary) ── */}
      <div style={styles.panel}>
        <button style={styles.legacyToggle} onClick={openLegacy}>
          <FontAwesomeIcon icon={legacyOpen ? faChevronDown : faChevronRight} style={{ marginRight: 8 }} />
          <FontAwesomeIcon icon={faGlobe} style={{ color: C.indigo, marginRight: 8 }} />
          Welcome-pool refresh view — "when do I need to upload more data per mailbox provider?"
        </button>
        {legacyOpen && (
          <div style={{ marginTop: 12 }}>
            {legacyLoading && (
              <div style={{ color: C.textSec, fontSize: 12 }}>
                <FontAwesomeIcon icon={faSpinner} spin /> Loading snapshot…
              </div>
            )}
            {legacyError && (
              <div style={{ color: C.red, fontSize: 12 }}>
                <FontAwesomeIcon icon={faExclamationTriangle} /> {legacyError}
              </div>
            )}
            {legacy && (
              <>
                <div style={{ fontSize: 11, color: C.textMuted, marginBottom: 8 }}>
                  Snapshot built {new Date(legacy.snapshot_refreshed_at).toLocaleString('en-US', { hour12: false })} ·
                  saturation threshold {legacy.saturation_threshold} sends w/o open
                </div>
                <table style={styles.table}>
                  <thead>
                    <tr>
                      <th style={{ ...styles.th, textAlign: 'left' }}>Mailbox Provider</th>
                      <th style={styles.th}>Pool</th>
                      <th style={styles.th}>Welcome mailable</th>
                      <th style={styles.th}>Never mailed</th>
                      <th style={styles.th}>Daily burn</th>
                      <th style={styles.th}>Cycle days</th>
                      <th style={styles.th}>Activation %</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>Refresh trigger</th>
                    </tr>
                  </thead>
                  <tbody>
                    {legacy.isp_rows.map((row) => {
                      const trigColor = row.refresh_trigger === 'NOW' ? C.red
                        : row.refresh_trigger === 'THIS_WEEK' ? C.amber
                        : row.refresh_trigger === 'AMPLE' ? C.indigo : C.green;
                      return (
                        <tr key={row.isp}>
                          <td style={{ ...styles.td, textAlign: 'left', fontWeight: 700, color: C.heading }}>{label(row.isp)}</td>
                          <td style={styles.tdNum}>{fmt(row.pool_total)}</td>
                          <td style={{ ...styles.tdNum, color: C.indigo, fontWeight: 600 }}>{fmt(row.welcome_pool)}</td>
                          <td style={styles.tdNum}>{fmt(row.never_mailed)}</td>
                          <td style={styles.tdNum}>{fmt(row.daily_unique_burn_7d)}</td>
                          <td style={styles.tdNum}>{row.cycle_days > 0 ? `${row.cycle_days.toFixed(1)}d` : '—'}</td>
                          <td style={styles.tdNum}>{row.activation_pct.toFixed(2)}%</td>
                          <td style={{ ...styles.td, textAlign: 'left' }}>
                            <span style={{ color: trigColor, fontWeight: 700, fontSize: 11 }}>{row.refresh_trigger}</span>
                            <span style={{ color: C.textMuted, fontSize: 10, marginLeft: 8 }}>{row.refresh_reason}</span>
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </>
            )}
          </div>
        )}
      </div>

      <div style={styles.footer}>
        Page v{PAGE_VERSION} · API v{data.api_version} · as of {new Date(data.as_of).toLocaleString('en-US', { hour12: false })}
        {' '}· Source: <code>/api/mailing/audience-cadence/kpis</code> + <code>/doctrines</code>
      </div>
    </div>
  );
};

// ─── Styles ──────────────────────────────────────────────────────────────────

const styles: Record<string, React.CSSProperties> = {
  page: {
    background: colors.appBg,
    minHeight: '100%',
    padding: 24,
    color: C.text,
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif',
  },
  header: {
    display: 'flex',
    alignItems: 'flex-start',
    justifyContent: 'space-between',
    marginBottom: 14,
    gap: 16,
    flexWrap: 'wrap',
  },
  title: { fontSize: 24, fontWeight: 700, margin: 0, color: C.heading, display: 'flex', alignItems: 'center' },
  subtitle: { fontSize: 13, color: C.textSec, margin: '6px 0 0', maxWidth: 880, lineHeight: 1.5 },
  btn: {
    background: C.bgPanel,
    border: `1px solid ${C.borderStrong}`,
    color: C.text,
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
  btnSmall: {
    background: C.bgPanel,
    border: `1px solid ${C.borderStrong}`,
    color: C.text,
    borderRadius: 6,
    padding: '5px 10px',
    fontSize: 11,
    cursor: 'pointer',
    display: 'inline-flex',
    alignItems: 'center',
    gap: 6,
    fontWeight: 500,
    whiteSpace: 'nowrap',
  },
  headlineStrip: {
    background: `${C.green}11`,
    border: `1px solid ${C.green}44`,
    borderRadius: 8,
    padding: '10px 14px',
    fontSize: 13,
    color: C.heading,
    fontWeight: 600,
    marginBottom: 10,
  },
  computingStrip: {
    background: `${C.indigoDeep}14`,
    border: `1px solid ${C.indigoDeep}44`,
    borderRadius: 8,
    padding: '10px 14px',
    fontSize: 12.5,
    color: C.heading,
    lineHeight: 1.5,
    marginBottom: 10,
  },
  captionStrip: {
    background: C.bgTable,
    border: `1px solid ${C.border}`,
    borderRadius: 8,
    padding: '8px 14px',
    fontSize: 11,
    color: C.textSec,
    lineHeight: 1.55,
    marginBottom: 14,
  },
  panel: {
    background: C.bgTable,
    border: `1px solid ${C.border}`,
    borderRadius: 8,
    padding: 16,
    marginBottom: 16,
  },
  table: { width: '100%', borderCollapse: 'collapse', fontSize: 12 },
  th: {
    padding: '8px 10px',
    textAlign: 'right',
    color: C.textMuted,
    fontWeight: 500,
    borderBottom: `1px solid ${C.border}`,
    fontSize: 11,
    whiteSpace: 'nowrap',
    verticalAlign: 'bottom',
  },
  td: {
    padding: '10px 10px',
    color: C.text,
    borderBottom: `1px solid ${C.border}`,
    verticalAlign: 'middle',
  },
  tdNum: {
    padding: '10px 10px',
    color: C.text,
    borderBottom: `1px solid ${C.border}`,
    textAlign: 'right',
    fontVariantNumeric: 'tabular-nums',
  },
  expandWrap: {
    display: 'grid',
    gridTemplateColumns: 'minmax(320px, 5fr) minmax(360px, 7fr)',
    gap: 0,
    background: 'rgba(15,30,60,0.5)',
  },
  expandCol: { padding: 18 },
  expandTitle: { fontSize: 14, fontWeight: 700, color: C.heading, margin: '0 0 12px' },
  miniTable: { width: '100%', borderCollapse: 'collapse', fontSize: 12, background: C.bgTable, borderRadius: 6 },
  miniTh: {
    padding: '6px 10px',
    textAlign: 'right',
    color: C.textMuted,
    fontWeight: 500,
    borderBottom: `1px solid ${C.border}`,
    fontSize: 10.5,
  },
  miniTd: {
    padding: '7px 10px',
    color: C.text,
    borderBottom: `1px solid ${C.border}`,
    textAlign: 'right',
    fontVariantNumeric: 'tabular-nums',
  },
  doctrineBlock: {
    marginTop: 12,
    background: C.bgTable,
    border: `1px solid ${C.border}`,
    borderRadius: 8,
    padding: '10px 12px',
  },
  doctrineBlockTitle: { fontSize: 12, fontWeight: 700, marginBottom: 6 },
  editLabel: { display: 'block', fontSize: 11, color: C.textSec, margin: '10px 0 4px', fontWeight: 600 },
  editInput: {
    width: '100%',
    boxSizing: 'border-box',
    background: C.bgPanel,
    border: `1px solid ${C.borderStrong}`,
    borderRadius: 6,
    color: C.text,
    fontSize: 12.5,
    padding: '8px 10px',
  },
  editArea: {
    width: '100%',
    boxSizing: 'border-box',
    background: C.bgPanel,
    border: `1px solid ${C.borderStrong}`,
    borderRadius: 6,
    color: C.text,
    fontSize: 12.5,
    padding: '8px 10px',
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
    lineHeight: 1.5,
    resize: 'vertical',
  },
  legacyToggle: {
    background: 'transparent',
    border: 'none',
    color: C.textSec,
    cursor: 'pointer',
    fontSize: 13,
    fontWeight: 600,
    display: 'flex',
    alignItems: 'center',
    padding: 0,
  },
  loadingShell: {
    minHeight: 400,
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    color: C.textSec,
    background: C.bgDeep,
    fontSize: 14,
    gap: 8,
  },
  errorShell: {
    minHeight: 400,
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    color: C.red,
    background: C.bgDeep,
    fontSize: 14,
    gap: 16,
    flexDirection: 'column',
  },
  footer: {
    fontSize: 10,
    color: C.textMuted,
    textAlign: 'center',
    padding: 16,
  },
};

export default AudienceCadenceByCell;
