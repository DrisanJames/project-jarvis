import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faSpinner, faExclamationTriangle, faFileExport } from '@fortawesome/free-solid-svg-icons';
import {
  ResponsiveContainer, ComposedChart, Bar, Line, XAxis, YAxis,
  Tooltip, Legend, CartesianGrid,
} from 'recharts';
import { apiFetch } from '../shared/apiFetch';

// PartnerQualityReport — the partner-reportable quality funnel for one feed:
// what they sent → what survived intake (suppression/dedup) → EmailOversight
// verdicts with NAMED rejection reasons → mailed → engaged. Replaces the raw
// one-row-per-POST batch list as the primary Inbound Batches view; the raw
// posts remain available below for debugging.

interface QualityTotals {
  posts: number;
  records_received: number;
  queued: number;
  intake_dropped: number;
  eo_pending: number;
  eo_passed: number;
  eo_rejected: number;
  dead_letter: number;
  ready: number;
  mailed: number;
  engaged: number;
  completed: number;
}

interface QualityReport {
  dataset_id: string;
  dataset_name: string;
  partner_name: string;
  vertical: string;
  days: number;
  totals: QualityTotals;
  rejection_reasons: { reason: string; code: number; count: number }[];
  isp_mix: { isp: string; queued: number; ready: number; mailed: number; engaged: number }[];
  daily: { day: string; posts: number; records: number; eo_passed: number; eo_rejected: number; mailed: number; engaged: number }[];
}

export const PartnerQualityReport: React.FC<{ datasetId: string }> = ({ datasetId }) => {
  const [report, setReport] = useState<QualityReport | null>(null);
  const [days, setDays] = useState(14);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const fetchReport = useCallback(() => {
    setLoading(true);
    setError(null);
    apiFetch(`/api/mailing/data-partners/datasets/${datasetId}/quality-report?days=${days}`, { credentials: 'include' })
      .then(r => r.json())
      .then(data => {
        if (data?.error) { setError(data.error); setReport(null); }
        else setReport(data);
      })
      .catch(err => setError(String(err)))
      .finally(() => setLoading(false));
  }, [datasetId, days]);

  useEffect(() => { fetchReport(); }, [fetchReport]);

  if (loading && !report) {
    return <div style={{ color: 'rgba(180,210,240,0.65)', padding: 30 }}><FontAwesomeIcon icon={faSpinner} spin /> Building quality report…</div>;
  }
  if (error) {
    return <div style={{ color: '#ef4444', padding: 20 }}><FontAwesomeIcon icon={faExclamationTriangle} /> {error}</div>;
  }
  if (!report) return null;

  const t = report.totals;
  const pctOf = (n: number, d: number) => d > 0 ? `${((n / d) * 100).toFixed(1)}%` : '—';
  const validated = t.eo_passed + t.eo_rejected;

  // Plain-language verdict line the operator can paste to the partner.
  const verdict = validated > 0
    ? `Of ${t.records_received.toLocaleString()} leads received in the last ${report.days} days, ` +
      `${t.intake_dropped.toLocaleString()} (${pctOf(t.intake_dropped, t.records_received)}) were dropped at intake (already suppressed or duplicate), ` +
      `${t.eo_rejected.toLocaleString()} (${pctOf(t.eo_rejected, validated)} of validated) failed mailbox validation, and ` +
      `${t.eo_passed.toLocaleString()} were accepted for mailing. ${t.mailed.toLocaleString()} have been mailed; ${t.engaged.toLocaleString()} engaged.`
    : 'No validated records in this window yet.';

  const chartData = report.daily.map(d => ({
    day: d.day.slice(5),
    Received: d.records,
    Passed: d.eo_passed,
    Rejected: d.eo_rejected,
    Mailed: d.mailed,
    Engaged: d.engaged,
  }));

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', marginBottom: 10 }}>
        <div>
          <h3 style={{ margin: 0, color: '#dbeafe' }}>{report.partner_name} / {report.dataset_name}</h3>
          <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.6)' }}>{report.vertical} · last {report.days} days</div>
        </div>
        <div style={{ display: 'flex', gap: 6 }}>
          {[7, 14, 30].map(d => (
            <button key={d} onClick={() => setDays(d)} style={{ ...pillBtn, ...(days === d ? pillActive : {}) }}>{d}d</button>
          ))}
        </div>
      </div>

      {/* The partner-reportable verdict */}
      <div style={verdictBox}>
        <FontAwesomeIcon icon={faFileExport} style={{ marginRight: 8, color: '#a5b4fc' }} />
        {verdict}
      </div>

      {/* Funnel cards */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(120px, 1fr))', gap: 8, marginBottom: 16 }}>
        <Stat label="Posts (API calls)" value={t.posts} />
        <Stat label="Leads received" value={t.records_received} />
        <Stat label="Dropped at intake" value={t.intake_dropped} sub={pctOf(t.intake_dropped, t.records_received)} accent={t.intake_dropped > 0 ? '#f59e0b' : undefined} title="Already suppressed / duplicates — never entered the queue" />
        <Stat label="EO passed" value={t.eo_passed} sub={pctOf(t.eo_passed, validated)} accent="#10b981" title="Verified deliverable by EmailOversight" />
        <Stat label="EO rejected" value={t.eo_rejected} sub={pctOf(t.eo_rejected, validated)} accent="#ef4444" title="Failed mailbox validation — see reasons below" />
        <Stat label="Awaiting EO" value={t.eo_pending} accent="#a78bfa" />
        <Stat label="Mailed" value={t.mailed} accent="#6366f1" />
        <Stat label="Engaged" value={t.engaged} sub={pctOf(t.engaged, t.mailed)} accent="#10b981" title="Opened or clicked after mailing" />
      </div>

      {/* Daily trend */}
      {chartData.length > 1 && (
        <div style={{ marginBottom: 16 }}>
          <div style={sectionLabel}>Daily flow — received vs validated vs mailed</div>
          <ResponsiveContainer width="100%" height={200}>
            <ComposedChart data={chartData} margin={{ top: 4, right: 12, left: -14, bottom: 0 }}>
              <CartesianGrid stroke="rgba(120,150,200,0.12)" vertical={false} />
              <XAxis dataKey="day" tick={{ fill: 'rgba(180,210,240,0.6)', fontSize: 11 }} stroke="rgba(120,150,200,0.25)" />
              <YAxis tick={{ fill: 'rgba(180,210,240,0.6)', fontSize: 11 }} stroke="rgba(120,150,200,0.25)" />
              <Tooltip contentStyle={{ background: '#0f1c33', border: '1px solid rgba(120,150,200,0.3)', borderRadius: 6, fontSize: 12 }} labelStyle={{ color: '#dbeafe' }} />
              <Legend wrapperStyle={{ fontSize: 12 }} />
              <Bar dataKey="Received" fill="#6366f1" radius={[2, 2, 0, 0]} />
              <Bar dataKey="Passed" fill="#10b981" radius={[2, 2, 0, 0]} />
              <Bar dataKey="Rejected" fill="#ef4444" radius={[2, 2, 0, 0]} />
              <Line type="monotone" dataKey="Mailed" stroke="#60a5fa" strokeWidth={2} dot={false} />
              <Line type="monotone" dataKey="Engaged" stroke="#a78bfa" strokeWidth={2} dot={false} />
            </ComposedChart>
          </ResponsiveContainer>
        </div>
      )}

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(340px, 1fr))', gap: 16 }}>
        {/* Rejection reasons — what to report back */}
        <div>
          <div style={sectionLabel}>Rejection reasons (report these to the partner)</div>
          <table style={tableStyle}>
            <thead>
              <tr style={{ background: 'rgba(120,150,200,0.06)' }}>
                <th style={th}>EmailOversight verdict</th>
                <th style={thNum}>Leads</th>
                <th style={thNum}>% of validated</th>
              </tr>
            </thead>
            <tbody>
              {report.rejection_reasons.map(rr => (
                <tr key={`${rr.reason}-${rr.code}`}>
                  <td style={td}><span style={{ color: '#ef4444' }}>{rr.reason}</span> <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.45)' }}>(code {rr.code})</span></td>
                  <td style={tdNum}>{rr.count.toLocaleString()}</td>
                  <td style={tdNum}>{pctOf(rr.count, validated)}</td>
                </tr>
              ))}
              {report.rejection_reasons.length === 0 && (
                <tr><td style={{ ...td, color: 'rgba(180,210,240,0.5)' }} colSpan={3}>No rejections in this window.</td></tr>
              )}
            </tbody>
          </table>
        </div>

        {/* ISP mix */}
        <div>
          <div style={sectionLabel}>ISP mix of accepted leads</div>
          <table style={tableStyle}>
            <thead>
              <tr style={{ background: 'rgba(120,150,200,0.06)' }}>
                <th style={th}>ISP</th>
                <th style={thNum}>Queued</th>
                <th style={thNum}>Ready</th>
                <th style={thNum}>Mailed</th>
                <th style={thNum}>Engaged</th>
              </tr>
            </thead>
            <tbody>
              {report.isp_mix.slice(0, 10).map(i => (
                <tr key={i.isp}>
                  <td style={td}>{i.isp}</td>
                  <td style={tdNum}>{i.queued.toLocaleString()}</td>
                  <td style={tdNum}>{i.ready.toLocaleString()}</td>
                  <td style={tdNum}>{i.mailed.toLocaleString()}</td>
                  <td style={tdNum}><span style={{ color: i.engaged > 0 ? '#10b981' : undefined }}>{i.engaged.toLocaleString()}</span></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  );
};

const Stat: React.FC<{ label: string; value: number; sub?: string; accent?: string; title?: string }> = ({ label, value, sub, accent, title }) => (
  <div title={title} style={{ background: 'rgba(0,0,0,0.22)', padding: '10px 8px', borderRadius: 6, textAlign: 'center' }}>
    <div style={{ fontSize: 9.5, color: 'rgba(180,210,240,0.6)', textTransform: 'uppercase', letterSpacing: 0.5 }}>{label}</div>
    <div style={{ fontSize: 17, fontWeight: 700, marginTop: 2, color: accent ?? '#dbeafe' }}>{value.toLocaleString()}</div>
    {sub && <div style={{ fontSize: 11, color: accent ?? 'rgba(180,210,240,0.55)' }}>{sub}</div>}
  </div>
);

const verdictBox: React.CSSProperties = {
  background: 'rgba(99,102,241,0.10)', border: '1px solid rgba(99,102,241,0.3)',
  borderRadius: 8, padding: '12px 14px', fontSize: 13.5, lineHeight: 1.6,
  color: '#dbeafe', marginBottom: 16,
};
const sectionLabel: React.CSSProperties = {
  fontSize: 11, color: 'rgba(180,210,240,0.6)', textTransform: 'uppercase',
  letterSpacing: 0.6, marginBottom: 6,
};
const pillBtn: React.CSSProperties = {
  background: 'rgba(0,0,0,0.25)', color: 'rgba(220,235,250,0.85)',
  border: '1px solid rgba(120,150,200,0.25)', borderRadius: 12, padding: '3px 12px',
  cursor: 'pointer', fontSize: 12,
};
const pillActive: React.CSSProperties = {
  background: 'rgba(99,102,241,0.25)', borderColor: '#6366f1', color: '#dbeafe',
};
const tableStyle: React.CSSProperties = {
  width: '100%', borderCollapse: 'collapse', background: 'rgba(15,30,60,0.35)',
  borderRadius: 8, overflow: 'hidden', fontSize: 13,
};
const th: React.CSSProperties = {
  textAlign: 'left', padding: '8px 10px', color: 'rgba(180,210,240,0.65)',
  fontWeight: 500, fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.5,
};
const thNum: React.CSSProperties = { ...th, textAlign: 'right' };
const td: React.CSSProperties = { padding: '8px 10px', borderTop: '1px solid rgba(120,150,200,0.1)' };
const tdNum: React.CSSProperties = { ...td, fontVariantNumeric: 'tabular-nums', textAlign: 'right' };
