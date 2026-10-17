import React, { useCallback, useEffect, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faSpinner, faChevronDown, faChevronRight } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';

// CpmMailedTracker — "how much have I mailed to each CPM this month and how far
// am I from fulfilling it" (operator 2026-10-17). Reads GET
// /cpm-planner/mailed: campaign SENT counters per deal for one Denver month —
// no tracking-events scan, so it neither times out nor drifts month to month.
// Target = the deal's monthly target row, else the deal's own plan.

const API = '/api/mailing/cpm-planner';

const C = {
  panel: 'rgba(15,30,60,0.35)',
  border: 'rgba(99,102,241,0.25)',
  heading: '#dbeafe',
  muted: 'rgba(180,210,240,0.65)',
  green: '#10b981',
  red: '#ef4444',
  amber: '#f59e0b',
  indigo: '#6366f1',
};

interface MailedDay { day: string; sent: number; delivered: number; campaigns: number }
interface MailedDeal {
  deal_id: string; name: string; status: string; everflow_offer_id: string;
  target_volume: number; target_budget: number; target_source: 'monthly' | 'deal' | 'none'; ecpm: number;
  sent: number; delivered: number; campaigns: number; booked: number; remaining: number;
  pct_sent: number; earned_usd: number; sent_today: number; avg_3d: number; days_left: number;
  required_daily: number; projected: number; projected_pct: number; on_pace: boolean; last_sent_on: string;
  // eCPM model (cpmMailedModel in cpm_mailed.go)
  ecpa: number; conversions_needed: number; ef_conversions: number; conversions: number;
  revenue: number; revenue_basis: 'paid' | 'model' | 'none'; earned_ecpm: number; actual_ecpa: number;
  projected_conversions: number; projected_revenue: number; budget_ecpm_at_projected: number; revenue_gap: number;
  days: MailedDay[];
}
interface MailedResp {
  month: string; is_current: boolean; today: string; day_of_month: number; days_in_month: number;
  source: string; deals: MailedDeal[];
  portfolio: {
    target_volume: number; sent: number; delivered: number; remaining: number; booked: number; earned_usd: number;
    target_budget: number; revenue: number; projected_revenue: number; projected: number;
  };
}

const fmtInt = (n: number) => Math.round(n).toLocaleString('en-US');
const fmtM = (n: number) => (Math.abs(n) >= 1e6 ? `${(n / 1e6).toFixed(2)}M` : Math.abs(n) >= 1e3 ? `${(n / 1e3).toFixed(0)}k` : fmtInt(n));
const fmtUSD = (n: number) => (n < 0 ? '-$' : '$') + Math.abs(Math.round(n)).toLocaleString('en-US');
const fmtEcpm = (n: number) => (n > 0 ? `$${n.toFixed(2)}` : '—');
const pctColor = (p: number) => (p >= 0.9 ? C.green : p >= 0.75 ? C.amber : C.red);

const th: React.CSSProperties = {
  padding: '8px 10px', textAlign: 'right', fontSize: 11, color: C.muted, textTransform: 'uppercase',
  letterSpacing: 0.5, borderBottom: `1px solid ${C.border}`, whiteSpace: 'nowrap',
};
const td: React.CSSProperties = {
  padding: '8px 10px', textAlign: 'right', fontSize: 13, color: C.heading,
  borderBottom: '1px solid rgba(99,102,241,0.12)', whiteSpace: 'nowrap', fontVariantNumeric: 'tabular-nums',
};

const monthKey = (offset: number, fromYM: string) => {
  const [y, m] = fromYM.split('-').map(Number);
  const d = new Date(y, m - 1 + offset, 1);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}`;
};

export const CpmMailedTracker: React.FC = () => {
  const [month, setMonth] = useState<string>(''); // '' = current Denver month
  const [data, setData] = useState<MailedResp | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [open, setOpen] = useState<Record<string, boolean>>({});
  // What-if eCPM per deal (client-side only): goal volume = budget ÷ eCPM × 1000.
  const [whatIf, setWhatIf] = useState<Record<string, string>>({});

  const load = useCallback(async (m: string) => {
    setLoading(true);
    setError(null);
    try {
      const res = await apiFetch(`${API}/mailed${m ? `?month=${m}` : ''}`);
      const body = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(body.error || `HTTP ${res.status}`);
      setData(body as MailedResp);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'failed to load mailed volume');
    } finally {
      setLoading(false);
    }
  }, []);

  // On demand only (mount / month change / Refresh) — never a poll timer.
  useEffect(() => { load(month); }, [load, month]);

  const cur = data?.month || '';
  const pf = data?.portfolio;
  const pfPct = pf && pf.target_volume > 0 ? pf.sent / pf.target_volume : 0;

  return (
    <div style={{ background: C.panel, border: `1px solid ${C.border}`, borderRadius: 10, padding: '14px 18px', marginBottom: 18 }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', flexWrap: 'wrap', gap: 10, marginBottom: 10 }}>
        <div>
          <div style={{ fontSize: 12, fontWeight: 700, letterSpacing: 0.6, textTransform: 'uppercase', color: C.heading }}>
            CPM Mailed vs Goal {data ? `— ${data.month}` : ''}
          </div>
          <div style={{ fontSize: 12, color: C.muted, marginTop: 3 }}>
            Sent from campaign counters, by the Denver day each campaign started.
            {data?.is_current && ` Day ${data.day_of_month} of ${data.days_in_month}.`}
          </div>
        </div>
        <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
          {cur && (
            <>
              <button onClick={() => setMonth(monthKey(-1, cur))} style={btn}>‹ {monthKey(-1, cur)}</button>
              {!data?.is_current && <button onClick={() => setMonth('')} style={btn}>This month</button>}
              <button onClick={() => setMonth(monthKey(1, cur))} style={btn}>{monthKey(1, cur)} ›</button>
            </>
          )}
          <button onClick={() => load(month)} disabled={loading} style={btn}>
            {loading ? <FontAwesomeIcon icon={faSpinner} spin /> : 'Refresh'}
          </button>
        </div>
      </div>

      {error && <div style={{ color: C.red, fontSize: 12, marginBottom: 8 }}>{error}</div>}

      {pf && (
        <div style={{ display: 'flex', gap: 28, flexWrap: 'wrap', marginBottom: 12 }}>
          <Kpi label="Goal" value={fmtInt(pf.target_volume)} />
          <Kpi label="Sent" value={fmtInt(pf.sent)} color={pctColor(pfPct)} sub={`${(pfPct * 100).toFixed(1)}%`} />
          <Kpi label="Remaining" value={fmtInt(pf.remaining)} color={pf.remaining > 0 ? C.amber : C.green} />
          <Kpi label="Booked (not yet sent)" value={fmtInt(pf.booked)} />
          <Kpi label="Revenue MTD" value={fmtUSD(pf.revenue)} sub={pf.sent > 0 ? `${fmtEcpm(pf.revenue / pf.sent * 1000)} eCPM` : undefined} />
          {data?.is_current && (
            <Kpi label="Forecast revenue" value={fmtUSD(pf.projected_revenue)}
              sub={`of ${fmtUSD(pf.target_budget)} · budget ÷ proj. vol ${fmtEcpm(pf.projected > 0 ? pf.target_budget / pf.projected * 1000 : 0)}`} />
          )}
        </div>
      )}

      {data && (
        <div style={{ overflowX: 'auto' }}>
          <table style={{ width: '100%', borderCollapse: 'collapse' }}>
            <thead>
              <tr>
                <th style={{ ...th, textAlign: 'left' }}>Deal</th>
                <th style={th}>Budget</th>
                <th style={th}>Goal</th>
                <th style={th}>Sent</th>
                <th style={th}>% of goal</th>
                <th style={th}>Remaining</th>
                <th style={th}>Booked</th>
                {data.is_current && <th style={th}>Today</th>}
                {data.is_current && <th style={th}>3-day avg</th>}
                {data.is_current && <th style={th}>Need / day ({data.deals[0]?.days_left ?? 0}d)</th>}
                {data.is_current && <th style={th}>Projected</th>}
                <th style={th}>Delivered</th>
                <th style={th}>Camps</th>
              </tr>
            </thead>
            <tbody>
              {data.deals.length === 0 && (
                <tr><td style={{ ...td, textAlign: 'left', color: C.muted }} colSpan={13}>No CPM deal mailed or owed volume this month.</td></tr>
              )}
              {data.deals.map(d => {
                const isOpen = !!open[d.deal_id];
                const behind = data.is_current && d.remaining > 0 && d.avg_3d < d.required_daily;
                return (
                  <React.Fragment key={d.deal_id}>
                    <tr onClick={() => setOpen(o => ({ ...o, [d.deal_id]: !o[d.deal_id] }))} style={{ cursor: 'pointer' }}>
                      <td style={{ ...td, textAlign: 'left' }}>
                        <FontAwesomeIcon icon={isOpen ? faChevronDown : faChevronRight} style={{ fontSize: 10, marginRight: 8, color: C.muted }} />
                        {d.name}
                        {d.target_source !== 'monthly' && (
                          <span title={d.target_source === 'deal' ? 'No monthly target set — goal is the deal-level plan' : 'No goal set'}
                            style={{ marginLeft: 8, fontSize: 10, color: C.amber, border: `1px solid ${C.amber}`, borderRadius: 4, padding: '0 4px' }}>
                            {d.target_source === 'deal' ? 'deal plan' : 'no goal'}
                          </span>
                        )}
                      </td>
                      <td style={td}>{d.target_budget > 0 ? fmtUSD(d.target_budget) : '—'}<span style={{ color: C.muted, fontSize: 11 }}> @ ${d.ecpm}</span></td>
                      <td style={td}>{d.target_volume > 0 ? fmtInt(d.target_volume) : '—'}</td>
                      <td style={{ ...td, fontWeight: 700 }}>{fmtInt(d.sent)}</td>
                      <td style={td}>
                        {d.target_volume > 0 ? (
                          <div style={{ display: 'flex', alignItems: 'center', gap: 8, justifyContent: 'flex-end' }}>
                            <div style={{ width: 80, height: 6, borderRadius: 3, background: 'rgba(99,102,241,0.15)' }}>
                              <div style={{ width: `${Math.min(100, d.pct_sent * 100)}%`, height: 6, borderRadius: 3, background: C.indigo }} />
                            </div>
                            {(d.pct_sent * 100).toFixed(1)}%
                          </div>
                        ) : '—'}
                      </td>
                      <td style={{ ...td, color: d.remaining > 0 ? C.amber : C.green }}>{d.target_volume > 0 ? fmtInt(d.remaining) : '—'}</td>
                      <td style={td}>{d.booked > 0 ? fmtInt(d.booked) : '—'}</td>
                      {data.is_current && <td style={td}>{fmtInt(d.sent_today)}</td>}
                      {data.is_current && <td style={{ ...td, color: behind ? C.red : C.heading }}>{fmtInt(d.avg_3d)}</td>}
                      {data.is_current && <td style={td}>{d.remaining > 0 ? fmtInt(d.required_daily) : '—'}</td>}
                      {data.is_current && (
                        <td style={{ ...td, color: d.target_volume > 0 ? pctColor(d.projected_pct) : C.heading }}>
                          {fmtM(d.projected)}{d.target_volume > 0 && <span style={{ fontSize: 11 }}> ({(d.projected_pct * 100).toFixed(0)}%)</span>}
                        </td>
                      )}
                      <td style={td}>{fmtInt(d.delivered)}</td>
                      <td style={td}>{fmtInt(d.campaigns)}</td>
                    </tr>
                    {isOpen && (
                      <tr>
                        <td colSpan={13} style={{ padding: '6px 10px 12px 34px', borderBottom: '1px solid rgba(99,102,241,0.12)' }}>
                          {d.days.length === 0 ? (
                            <span style={{ color: C.muted, fontSize: 12 }}>No campaigns this month.</span>
                          ) : (
                            <table style={{ borderCollapse: 'collapse' }}>
                              <thead><tr>
                                <th style={{ ...th, textAlign: 'left' }}>Day</th><th style={th}>Sent</th><th style={th}>Delivered</th><th style={th}>Camps</th><th style={th}>Cumulative</th>
                              </tr></thead>
                              <tbody>
                                {d.days.reduce<{ rows: React.ReactNode[]; cum: number }>((acc, x) => {
                                  acc.cum += x.sent;
                                  acc.rows.push(
                                    <tr key={x.day}>
                                      <td style={{ ...td, textAlign: 'left' }}>{x.day}</td>
                                      <td style={td}>{fmtInt(x.sent)}</td>
                                      <td style={td}>{fmtInt(x.delivered)}</td>
                                      <td style={td}>{x.campaigns}</td>
                                      <td style={td}>{fmtInt(acc.cum)}</td>
                                    </tr>,
                                  );
                                  return acc;
                                }, { rows: [], cum: 0 }).rows}
                              </tbody>
                            </table>
                          )}
                        </td>
                      </tr>
                    )}
                  </React.Fragment>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      {data && data.deals.length > 0 && (
        <>
          <div style={{ fontSize: 12, fontWeight: 700, letterSpacing: 0.6, textTransform: 'uppercase', color: C.heading, margin: '18px 0 3px' }}>
            eCPM Model {data.is_current ? '& Month-End Forecast (current cadence)' : ''}
          </div>
          <div style={{ fontSize: 12, color: C.muted, marginBottom: 8 }}>
            Revenue = paid amount entered as a manual conversion with revenue, else Everflow conversions on the deal's campaigns × eCPA.
            Forecast holds yield per send constant and projects volume at the 3-day average. "Budget ÷ volume" is the eCPM if the full budget is paid on the volume mailed.
          </div>
          <div style={{ overflowX: 'auto' }}>
            <table style={{ width: '100%', borderCollapse: 'collapse' }}>
              <thead>
                <tr>
                  <th style={{ ...th, textAlign: 'left' }}>Deal</th>
                  <th style={th}>eCPA</th>
                  <th style={th}>Conv needed</th>
                  <th style={th}>Conv MTD</th>
                  <th style={th}>Revenue MTD</th>
                  <th style={th}>Earned eCPM</th>
                  {data.is_current && <th style={th}>Proj. volume</th>}
                  {data.is_current && <th style={th}>Proj. conv</th>}
                  {data.is_current && <th style={th}>Proj. revenue</th>}
                  <th style={th}>Budget ÷ {data.is_current ? 'proj.' : ''} volume</th>
                  <th style={th}>Gap to budget</th>
                  <th style={th}>What-if eCPM</th>
                  <th style={th}>Volume needed</th>
                  {data.is_current && <th style={th}>Need / day</th>}
                </tr>
              </thead>
              <tbody>
                {data.deals.map(d => {
                  const wi = parseFloat(whatIf[d.deal_id] ?? String(d.ecpm || ''));
                  const volNeeded = wi > 0 && d.target_budget > 0 ? Math.ceil((d.target_budget / wi) * 1000) : 0;
                  const remainingAtWi = Math.max(volNeeded - d.sent, 0);
                  const needDay = d.days_left > 0 ? remainingAtWi / d.days_left : 0;
                  const vol = data.is_current ? d.projected : d.sent;
                  return (
                    <tr key={d.deal_id}>
                      <td style={{ ...td, textAlign: 'left' }}>{d.name}</td>
                      <td style={td}>{d.ecpa > 0 ? fmtUSD(d.ecpa) : '—'}</td>
                      <td style={td}>{d.conversions_needed > 0 ? fmtInt(d.conversions_needed) : '—'}</td>
                      <td style={td} title={`${d.ef_conversions} Everflow postbacks on the deal's campaigns`}>{fmtInt(d.conversions)}</td>
                      <td style={td}>
                        {d.revenue > 0 ? fmtUSD(d.revenue) : '—'}
                        {d.revenue_basis !== 'none' && (
                          <span style={{ marginLeft: 6, fontSize: 10, color: d.revenue_basis === 'paid' ? C.green : C.muted }}>
                            {d.revenue_basis === 'paid' ? 'paid' : 'conv×eCPA'}
                          </span>
                        )}
                      </td>
                      <td style={{ ...td, fontWeight: 700, color: d.earned_ecpm >= d.ecpm && d.ecpm > 0 ? C.green : d.earned_ecpm > 0 ? C.amber : C.muted }}>
                        {fmtEcpm(d.earned_ecpm)}
                      </td>
                      {data.is_current && <td style={td}>{fmtInt(d.projected)}</td>}
                      {data.is_current && <td style={td}>{fmtInt(d.projected_conversions)}</td>}
                      {data.is_current && <td style={td}>{d.projected_revenue > 0 ? fmtUSD(d.projected_revenue) : '—'}</td>}
                      <td style={{ ...td, fontWeight: 700 }} title={`${fmtUSD(d.target_budget)} ÷ ${fmtInt(vol)} × 1000`}>
                        {fmtEcpm(d.budget_ecpm_at_projected)}
                      </td>
                      <td style={{ ...td, color: d.revenue_gap > 0 ? C.amber : C.green }}>{d.target_budget > 0 ? fmtUSD(d.revenue_gap) : '—'}</td>
                      <td style={td} onClick={e => e.stopPropagation()}>
                        $<input
                          value={whatIf[d.deal_id] ?? String(d.ecpm || '')}
                          onChange={e => setWhatIf(w => ({ ...w, [d.deal_id]: e.target.value }))}
                          inputMode="decimal"
                          style={{ width: 52, marginLeft: 2, padding: '3px 6px', background: 'rgba(10,20,45,0.7)', border: `1px solid ${C.border}`, borderRadius: 4, color: C.heading, fontSize: 12, textAlign: 'right' }}
                        />
                      </td>
                      <td style={td}>{volNeeded > 0 ? fmtInt(volNeeded) : '—'}</td>
                      {data.is_current && <td style={td}>{volNeeded > 0 ? (remainingAtWi > 0 ? fmtInt(needDay) : 'met') : '—'}</td>}
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </>
      )}
    </div>
  );
};

const btn: React.CSSProperties = {
  padding: '6px 10px', borderRadius: 8, border: `1px solid ${C.border}`, background: 'transparent',
  color: C.muted, fontSize: 12, cursor: 'pointer',
};

const Kpi: React.FC<{ label: string; value: string; color?: string; sub?: string }> = ({ label, value, color, sub }) => (
  <div>
    <div style={{ fontSize: 10, color: C.muted, textTransform: 'uppercase', letterSpacing: 0.5 }}>{label}</div>
    <div style={{ fontSize: 18, fontWeight: 700, color: color || C.heading }}>
      {value}{sub && <span style={{ fontSize: 12, fontWeight: 500, color: C.muted, marginLeft: 6 }}>{sub}</span>}
    </div>
  </div>
);

export default CpmMailedTracker;
