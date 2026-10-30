import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faCircle, faSeedling, faExclamationTriangle, faGaugeHigh,
} from '@fortawesome/free-solid-svg-icons';
import {
  ResponsiveContainer, ComposedChart, Bar, Line, XAxis, YAxis,
  Tooltip, Legend, CartesianGrid,
} from 'recharts';
import { apiFetch } from '../shared/apiFetch';

// WarmupProgressPanel — read-only "Warm-Up Progress" view for the KumoMTA
// new-domain warm-up (mpf / pmd / trb). Everything is re-aggregated server-side
// from tracking events on each poll, so the numbers move as PMTA/Kumo
// accounting events are ingested. Bounces are ALWAYS split hard (red) vs soft
// (amber). Opens/clicks render but are 0 until engagement tracking is wired for
// these brand-new domains (labelled below). Polls every 30s.

const POLL_MS = 30_000;
const WINDOW_HOURS = 72;

const BRAND_LABEL: Record<string, string> = {
  mpf: 'My Personal Financial',
  pmd: 'Pay My Debit',
  trb: 'The Retirement Blog',
};

const BRAND_COLORS = ['var(--j-violet-strong)', 'var(--j-success)', 'var(--j-blue)'];

interface DomainStat {
  brand: string;
  domain: string;
  sent: number;
  delivered: number;
  deferred: number;
  hard_bounce: number;
  soft_bounce: number;
  opened: number;
  clicked: number;
  delivered_pct: number;
}

interface ISPStat {
  brand: string;
  isp: string;
  sent: number;
  delivered: number;
  deferred: number;
  bounced: number;
  opened: number;
  clicked: number;
}

interface CurvePoint {
  hour_utc: string;
  brand: string; // "" = overall
  sent: number;
  delivered: number;
  delivered_pct: number;
}

interface Cap {
  isp: string;
  max_per_wave: number;
}

interface WarmupResponse {
  generated_at: string;
  window_hours: number;
  dataset_id: string;
  brands: string[];
  bounce_split: boolean;
  engagement_note: string;
  domains?: DomainStat[];
  by_isp?: ISPStat[];
  recovery_curve?: CurvePoint[];
  caps?: Cap[];
  domains_error?: string;
  by_isp_error?: string;
  recovery_curve_error?: string;
  caps_error?: string;
}

const brandLabel = (b: string) => BRAND_LABEL[b] ?? b.toUpperCase();

export const WarmupProgressPanel: React.FC = () => {
  const [data, setData] = useState<WarmupResponse | null>(null);
  const [lastFetch, setLastFetch] = useState<Date | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [, setTick] = useState(0);

  const fetchProgress = useCallback(() => {
    apiFetch(`/api/mailing/data-partners/warmup-progress?hours=${WINDOW_HOURS}`, { credentials: 'include' })
      .then(r => r.json())
      .then((d: WarmupResponse) => {
        if (d && Array.isArray(d.domains)) {
          setData(d);
          const err = d.domains_error || d.by_isp_error || d.recovery_curve_error || d.caps_error;
          setError(err ? String(err) : null);
        } else if (d && (d as { error?: string }).error) {
          setError(`Warm-up fetch: ${(d as { error?: string }).error}`);
        }
        setLastFetch(new Date());
      })
      .catch(err => setError(String(err)));
  }, []);

  useEffect(() => {
    fetchProgress();
    const t = setInterval(fetchProgress, POLL_MS);
    const tt = setInterval(() => setTick(x => x + 1), 1000);
    return () => { clearInterval(t); clearInterval(tt); };
  }, [fetchProgress]);

  const secondsAgo = lastFetch ? Math.max(0, Math.round((Date.now() - lastFetch.getTime()) / 1000)) : null;

  const domains = data?.domains ?? [];
  const byISP = data?.by_isp ?? [];
  const caps = data?.caps ?? [];
  const curve = data?.recovery_curve ?? [];

  // Build the recovery chart from the "overall" (brand === '') series, plus a
  // per-brand delivered% line each so the operator can see laggards.
  const hours = Array.from(new Set(curve.map(c => c.hour_utc))).sort();
  const overallByHour: Record<string, CurvePoint> = {};
  const brandPctByHour: Record<string, Record<string, number>> = {};
  curve.forEach(c => {
    if (c.brand === '') overallByHour[c.hour_utc] = c;
    else {
      if (!brandPctByHour[c.hour_utc]) brandPctByHour[c.hour_utc] = {};
      brandPctByHour[c.hour_utc][c.brand] = c.delivered_pct;
    }
  });
  const warmBrands = data?.brands ?? ['mpf', 'pmd', 'trb'];
  const chartData = hours.map(h => {
    const o = overallByHour[h];
    const row: Record<string, number | string> = {
      hour: new Date(h).toLocaleTimeString([], { month: 'numeric', day: 'numeric', hour: 'numeric' }),
      Sent: o?.sent ?? 0,
      Delivered: o?.delivered ?? 0,
      'Delivered %': o ? Number(o.delivered_pct.toFixed(1)) : 0,
    };
    warmBrands.forEach(b => {
      const v = brandPctByHour[h]?.[b];
      if (v !== undefined) row[`${b.toUpperCase()} %`] = Number(v.toFixed(1));
    });
    return row;
  });

  return (
    <div style={{ marginBottom: 28 }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', borderBottom: '1px solid color-mix(in srgb, var(--j-text-muted) 18%, transparent)', paddingBottom: 6, marginBottom: 12 }}>
        <h3 style={{ color: 'var(--j-text)', margin: 0 }}>
          <FontAwesomeIcon icon={faSeedling} style={{ marginRight: 8, color: 'var(--j-success)' }} />
          Warm-Up Progress — new sending domains
        </h3>
        <div style={{ display: 'flex', alignItems: 'center', gap: 14, fontSize: 12 }}>
          <span style={{ color: 'var(--j-success)', display: 'inline-flex', alignItems: 'center', gap: 6 }}>
            <FontAwesomeIcon icon={faCircle} style={{ fontSize: 8 }} beat={secondsAgo !== null && secondsAgo < 3} />
            LIVE — re-aggregated from tracking events every {POLL_MS / 1000}s
            {secondsAgo !== null && <span style={{ color: 'color-mix(in srgb, var(--j-text) 55%, transparent)' }}>(updated {secondsAgo}s ago)</span>}
          </span>
        </div>
      </div>

      <div style={{ fontSize: 12, color: 'color-mix(in srgb, var(--j-text) 60%, transparent)', marginBottom: 12 }}>
        Last {data?.window_hours ?? WINDOW_HOURS}h. Tracking the delivered/sent recovery of <b>mpf / pmd / trb</b> on the new sending domains.
        Bounces split hard (red) vs soft (amber). {data?.engagement_note
          ? <span style={{ color: '#f59e0b' }}> {data.engagement_note} (opens/clicks pending tracking wiring).</span>
          : <span> Opens/clicks pending tracking wiring.</span>}
      </div>

      {error && (
        <div style={{ background: 'rgba(239,68,68,0.18)', border: '1px solid rgba(239,68,68,0.4)', padding: 10, borderRadius: 6, marginBottom: 12, fontSize: 13 }}>
          <FontAwesomeIcon icon={faExclamationTriangle} /> {error}
        </div>
      )}

      {/* ───── Per-domain metric cards ───── */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(300px, 1fr))', gap: 14, marginBottom: 18 }}>
        {domains.map(d => <DomainCard key={d.domain} d={d} />)}
        {domains.length === 0 && !error && (
          <div style={{ color: 'color-mix(in srgb, var(--j-text) 55%, transparent)', fontSize: 13 }}>No warm-up sends in the last {WINDOW_HOURS}h yet.</div>
        )}
      </div>

      {/* ───── Recovery curve ───── */}
      <div style={{ fontSize: 13, color: 'color-mix(in srgb, var(--j-text) 70%, transparent)', marginBottom: 8 }}>
        <FontAwesomeIcon icon={faGaugeHigh} style={{ marginRight: 6 }} />
        Delivered / Sent recovery curve — hourly (UTC). Bars = overall volume, lines = delivered% (overall + per brand).
      </div>
      <div style={card}>
        {chartData.length > 0 ? (
          <ResponsiveContainer width="100%" height={250}>
            <ComposedChart data={chartData} margin={{ top: 4, right: 12, left: -14, bottom: 0 }}>
              <CartesianGrid stroke="color-mix(in srgb, var(--j-text-muted) 12%, transparent)" vertical={false} />
              <XAxis dataKey="hour" tick={{ fill: 'color-mix(in srgb, var(--j-text) 60%, transparent)', fontSize: 11 }} stroke="color-mix(in srgb, var(--j-text-muted) 25%, transparent)" />
              <YAxis yAxisId="vol" tick={{ fill: 'color-mix(in srgb, var(--j-text) 60%, transparent)', fontSize: 11 }} stroke="color-mix(in srgb, var(--j-text-muted) 25%, transparent)" />
              <YAxis yAxisId="pct" orientation="right" domain={[0, 100]} unit="%" tick={{ fill: 'color-mix(in srgb, var(--j-text) 60%, transparent)', fontSize: 11 }} stroke="color-mix(in srgb, var(--j-text-muted) 25%, transparent)" />
              <Tooltip
                contentStyle={{ background: 'var(--j-surface)', border: '1px solid color-mix(in srgb, var(--j-text-muted) 30%, transparent)', borderRadius: 6, fontSize: 12 }}
                labelStyle={{ color: 'var(--j-text)' }}
              />
              <Legend wrapperStyle={{ fontSize: 12 }} />
              <Bar yAxisId="vol" dataKey="Sent" fill="var(--j-violet-strong)" radius={[2, 2, 0, 0]} />
              <Bar yAxisId="vol" dataKey="Delivered" fill="var(--j-success)" radius={[2, 2, 0, 0]} />
              <Line yAxisId="pct" type="monotone" dataKey="Delivered %" stroke="#f59e0b" strokeWidth={2.5} dot={false} />
              {warmBrands.map((b, i) => (
                <Line key={b} yAxisId="pct" type="monotone" dataKey={`${b.toUpperCase()} %`} stroke={BRAND_COLORS[i % BRAND_COLORS.length]} strokeWidth={1.5} strokeDasharray="4 3" dot={false} />
              ))}
            </ComposedChart>
          </ResponsiveContainer>
        ) : (
          <div style={{ color: 'color-mix(in srgb, var(--j-text) 50%, transparent)', fontSize: 12, padding: '24px 0', textAlign: 'center' }}>
            No events in the window yet — the curve fills in as delivery activity arrives.
          </div>
        )}
      </div>

      {/* ───── Per-ISP table ───── */}
      <div style={{ fontSize: 13, color: 'color-mix(in srgb, var(--j-text) 70%, transparent)', margin: '22px 0 8px' }}>
        Per-provider breakdown (last {data?.window_hours ?? WINDOW_HOURS}h) — the warm-up targets Yahoo / AOL only; rates are against sent.
      </div>
      <table style={tableStyle}>
        <thead>
          <tr style={{ background: 'color-mix(in srgb, var(--j-text-muted) 6%, transparent)' }}>
            <th style={th}>Brand</th>
            <th style={th}>Mailbox provider</th>
            <th style={thNum}>Sent</th>
            <th style={thNum} title="Delivered, as % of sent">Delivered</th>
            <th style={thNum} title="Deferred — transient throttling during warm-up">Deferred</th>
            <th style={thNum}>Bounced</th>
            <th style={thNum} title="Raw opens — pending tracking wiring">Opens</th>
            <th style={thNum}>Clicks</th>
          </tr>
        </thead>
        <tbody>
          {byISP.map((i, idx) => (
            <tr key={`${i.brand}-${i.isp}-${idx}`}>
              <td style={td}>
                <div style={{ fontFamily: 'ui-monospace, monospace', color: 'var(--j-violet)' }}>{i.brand.toUpperCase()}</div>
              </td>
              <td style={td}>{i.isp}</td>
              <td style={tdNum}>{i.sent.toLocaleString()}</td>
              <td style={tdNum}><RateCell count={i.delivered} denom={i.sent} color="var(--j-success)" bold /></td>
              <td style={tdNum}><RateCell count={i.deferred} denom={i.sent} color="#f59e0b" /></td>
              <td style={tdNum}><span style={{ color: i.bounced > 0 ? '#ef4444' : 'color-mix(in srgb, var(--j-text) 60%, transparent)' }}>{i.bounced.toLocaleString()}</span></td>
              <td style={tdNum}>{i.opened.toLocaleString()}</td>
              <td style={tdNum}><span style={{ color: 'var(--j-violet)' }}>{i.clicked.toLocaleString()}</span></td>
            </tr>
          ))}
          {byISP.length === 0 && (
            <tr><td style={{ ...td, textAlign: 'center', color: 'color-mix(in srgb, var(--j-text) 50%, transparent)' }} colSpan={8}>No per-ISP data in the window.</td></tr>
          )}
        </tbody>
      </table>

      {/* ───── Current per-wave caps ───── */}
      <div style={{ fontSize: 13, color: 'color-mix(in srgb, var(--j-text) 70%, transparent)', margin: '22px 0 8px' }}>
        Current per-batch provider caps (warm-up dataset overrides) — <span style={{ fontFamily: 'ui-monospace, monospace', fontSize: 11 }}>{data?.dataset_id}</span>
      </div>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
        {caps.map(c => (
          <div key={c.isp} style={{ ...capChip, opacity: c.max_per_wave > 0 ? 1 : 0.45 }}>
            <span style={{ color: 'color-mix(in srgb, var(--j-text) 70%, transparent)' }}>{c.isp}</span>
            <span style={{ marginLeft: 8, fontWeight: 700, color: c.max_per_wave > 0 ? 'var(--j-success)' : 'color-mix(in srgb, var(--j-text) 50%, transparent)' }}>
              {c.max_per_wave > 0 ? c.max_per_wave.toLocaleString() : 'off'}
            </span>
          </div>
        ))}
        {caps.length === 0 && (
          <div style={{ color: 'color-mix(in srgb, var(--j-text) 55%, transparent)', fontSize: 13 }}>No provider caps configured for the warm-up dataset.</div>
        )}
      </div>
    </div>
  );
};

// ───── Per-domain card ─────

const DomainCard: React.FC<{ d: DomainStat }> = ({ d }) => (
  <div style={card}>
    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', marginBottom: 10 }}>
      <div>
        <h4 style={{ margin: 0, color: 'var(--j-text)', fontSize: 14 }}>{brandLabel(d.brand)}</h4>
        <div style={{ fontSize: 11, color: 'var(--j-violet)', fontFamily: 'ui-monospace, monospace' }}>{d.domain}</div>
      </div>
      <span style={{ fontSize: 12, color: 'var(--j-success)', fontWeight: 600 }}>{d.sent.toLocaleString()} sent</span>
    </div>
    <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 6 }}>
      <MiniStat label="Delivered" value={d.delivered} accent="var(--j-success)" sub={d.sent > 0 ? `${d.delivered_pct.toFixed(1)}%` : '—'} />
      <MiniStat label="Deferred" value={d.deferred} accent={d.deferred > 0 ? '#f59e0b' : undefined} title="Transient throttling during warm-up" />
      <MiniStat label="Hard" value={d.hard_bounce} accent={d.hard_bounce > 0 ? '#ef4444' : undefined} title="Permanent failures — reputation risk" />
      <MiniStat label="Soft" value={d.soft_bounce} accent={d.soft_bounce > 0 ? '#f59e0b' : undefined} title="Usually transient" />
      <MiniStat label="Opens" value={d.opened} accent="var(--j-blue)" title="Pending tracking wiring" />
      <MiniStat label="Clicks" value={d.clicked} accent="var(--j-violet)" title="Pending tracking wiring" />
    </div>
  </div>
);

const MiniStat: React.FC<{ label: string; value: number; accent?: string; title?: string; sub?: string }> = ({ label, value, accent, title, sub }) => (
  <div title={title} style={{ background: 'color-mix(in srgb, var(--j-shade) 50%, transparent)', padding: 6, borderRadius: 4, textAlign: 'center' }}>
    <div style={{ fontSize: 9, color: 'color-mix(in srgb, var(--j-text) 60%, transparent)', textTransform: 'uppercase', letterSpacing: 0.5 }}>{label}</div>
    <div style={{ fontSize: 15, fontWeight: 700, marginTop: 2, color: accent ?? 'var(--j-text)' }}>{value.toLocaleString()}</div>
    {sub && <div style={{ fontSize: 9, color: accent ?? 'color-mix(in srgb, var(--j-text) 55%, transparent)', marginTop: 1 }}>{sub}</div>}
  </div>
);

// RateCell shows a count with its rate against SENT; "—" until any mail has gone.
const RateCell: React.FC<{ count: number; denom: number; color: string; bold?: boolean }> = ({ count, denom, color, bold }) => {
  const rate = denom > 0 ? (count / denom) * 100 : null;
  return (
    <span>
      <span style={{ color, fontWeight: bold ? 700 : 400 }}>{count.toLocaleString()}</span>
      <span style={{ fontSize: 11, color: 'color-mix(in srgb, var(--j-text) 50%, transparent)', marginLeft: 4 }}>
        {rate === null ? '—' : `${rate.toFixed(rate >= 10 ? 0 : 1)}%`}
      </span>
    </span>
  );
};

// ───── Styles ─────

const card: React.CSSProperties = {
  background: 'linear-gradient(135deg, color-mix(in srgb, var(--j-shade) 65%, transparent) 0%, color-mix(in srgb, var(--j-shade) 50%, transparent) 100%)',
  border: '1px solid color-mix(in srgb, var(--j-text-muted) 18%, transparent)',
  borderRadius: 10, padding: 14,
};
const tableStyle: React.CSSProperties = {
  width: '100%', borderCollapse: 'collapse', background: 'color-mix(in srgb, var(--j-shade) 35%, transparent)',
  borderRadius: 8, overflow: 'hidden', fontSize: 13,
};
const th: React.CSSProperties = {
  textAlign: 'left', padding: '8px 10px', color: 'color-mix(in srgb, var(--j-text) 65%, transparent)',
  fontWeight: 500, fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.5,
};
const thNum: React.CSSProperties = { ...th, textAlign: 'right' };
const td: React.CSSProperties = { padding: '8px 10px', borderTop: '1px solid color-mix(in srgb, var(--j-text-muted) 10%, transparent)' };
const tdNum: React.CSSProperties = { ...td, fontVariantNumeric: 'tabular-nums', textAlign: 'right' };
const capChip: React.CSSProperties = {
  background: 'color-mix(in srgb, var(--j-shade) 55%, transparent)', border: '1px solid color-mix(in srgb, var(--j-text-muted) 20%, transparent)',
  borderRadius: 6, padding: '6px 12px', fontSize: 13,
};
