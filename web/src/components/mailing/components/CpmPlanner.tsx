import React, { useState, useEffect, useCallback, useMemo } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faCalculator, faPlus, faTimes, faTrash, faPen, faSpinner,
  faChevronDown, faChevronRight, faGaugeHigh,
} from '@fortawesome/free-solid-svg-icons';
import {
  ResponsiveContainer, BarChart, Bar, XAxis, YAxis, CartesianGrid,
  Tooltip as RechartsTooltip, ReferenceLine,
} from 'recharts';
import { apiFetch } from '../shared/apiFetch';

// CpmPlanner — living pricing/pacing screen for email CPM deals.
//
// The core math IS the product (the operator's spreadsheet):
//   planned_volume     = budget / ecpm_goal * 1000
//   conversions_needed = ceil(budget / ecpa_goal)
//   days_to_finish     = ceil(planned_volume / avg_campaign_size)
//
// Deals persist server-side; live delivery (tracking events) and conversion
// ground truth (everflow postbacks → offer suppressions) map back onto each
// deal, capacity risk comes from the platform's 14-day sending trend, and
// rule-based recommendations say how to hit the objective.

const API = '/api/mailing/cpm-planner';

// ─── Theme ───────────────────────────────────────────────────────────────────
const C = {
  panel: 'rgba(15,30,60,0.35)',
  border: 'rgba(99,102,241,0.25)',
  heading: '#dbeafe',
  muted: 'rgba(180,210,240,0.65)',
  indigo: '#6366f1',
  green: '#10b981',
  red: '#ef4444',
  amber: '#f59e0b',
};

// ─── Types (mirror cpm_planner_handlers.go JSON) ─────────────────────────────
interface DealProgress {
  sent: number;
  delivered: number;
  opened: number;
  clicked: number;
  hard_bounces: number;
  soft_bounces: number;
  conversions: number;
  payout: number;
  pct_volume_delivered: number;
  revenue_earned: number;
  actual_ecpm: number;
  actual_ecpa: number;
  days_elapsed: number;
  required_daily: number;
  actual_daily: number;
  on_pace: boolean;
}

interface Deal {
  id: string;
  name: string;
  offer_id: string;
  offer_name: string;
  everflow_offer_id: string;
  budget: number;
  ecpm_goal: number;
  ecpa_goal: number;
  avg_campaign_size: number;
  start_date: string;
  status: string;
  notes: string;
  planned_volume: number;
  conversions_needed: number;
  days_to_finish: number;
  progress: DealProgress;
}

interface Capacity {
  platform_daily: number;
  total_required_daily: number;
  utilization_pct: number;
  headroom: number;
  risk: string;
  active_deals: number;
}

interface DailyPoint { date: string; sent: number; }
interface DomainConv { domain: string; conversions: number; }

interface Insights {
  deal: Deal;
  capacity: Capacity;
  daily_series: DailyPoint[];
  top_domains: DomainConv[];
  recommendations: string[];
}

interface OfferLite {
  id: string;
  name: string;
  everflow_offer_id: string;
  payout: number;
}

// ─── Helpers ─────────────────────────────────────────────────────────────────
const fmtMoney = (n: number): string =>
  '$' + n.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
const fmtInt = (n: number): string => Math.round(n).toLocaleString('en-US');

// The operator's spreadsheet math — verified against the ADT reference deal:
// $2,000 budget / $0.70 eCPM / $38 eCPA / 160k avg → 2,857,143 / 53 / 18.
function computePlan(budget: number, ecpm: number, ecpa: number, avgSize: number) {
  const planned = budget > 0 && ecpm > 0 ? Math.ceil((budget / ecpm) * 1000) : 0;
  const conversions = budget > 0 && ecpa > 0 ? Math.ceil(budget / ecpa) : 0;
  const days = planned > 0 && avgSize > 0 ? Math.ceil(planned / avgSize) : 0;
  return { planned, conversions, days };
}

const riskColor = (risk: string): string =>
  risk === 'HIGH' ? C.red : risk === 'MODERATE' ? C.amber : C.green;

const statusColor = (s: string): string =>
  s === 'active' ? C.green : s === 'paused' ? C.amber : C.muted;

// ─── Shared styles ───────────────────────────────────────────────────────────
const inputStyle: React.CSSProperties = {
  width: '100%',
  padding: '8px 10px',
  background: 'rgba(10,20,45,0.7)',
  border: `1px solid ${C.border}`,
  borderRadius: 6,
  color: C.heading,
  fontSize: 13,
  boxSizing: 'border-box',
};

const labelStyle: React.CSSProperties = {
  display: 'block',
  fontSize: 11,
  color: C.muted,
  marginBottom: 4,
  textTransform: 'uppercase',
  letterSpacing: 0.5,
};

const thStyle: React.CSSProperties = {
  padding: '10px 12px',
  textAlign: 'left',
  fontSize: 11,
  color: C.muted,
  textTransform: 'uppercase',
  letterSpacing: 0.5,
  borderBottom: `1px solid ${C.border}`,
  whiteSpace: 'nowrap',
};

const tdStyle: React.CSSProperties = {
  padding: '10px 12px',
  fontSize: 13,
  color: C.heading,
  borderBottom: '1px solid rgba(99,102,241,0.12)',
  whiteSpace: 'nowrap',
};

// ─── Deal form state ─────────────────────────────────────────────────────────
interface FormState {
  name: string;
  offer_id: string;
  everflow_offer_id: string;
  budget: string;
  ecpm_goal: string;
  ecpa_goal: string;
  avg_campaign_size: string;
  start_date: string;
  status: string;
  notes: string;
}

const emptyForm = (): FormState => ({
  name: '',
  offer_id: '',
  everflow_offer_id: '',
  budget: '',
  ecpm_goal: '',
  ecpa_goal: '',
  avg_campaign_size: '160000',
  start_date: new Date().toISOString().slice(0, 10),
  status: 'active',
  notes: '',
});

// ─── Component ───────────────────────────────────────────────────────────────
export const CpmPlanner: React.FC = () => {
  const [deals, setDeals] = useState<Deal[]>([]);
  const [capacity, setCapacity] = useState<Capacity | null>(null);
  const [offers, setOffers] = useState<OfferLite[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const [showModal, setShowModal] = useState(false);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [form, setForm] = useState<FormState>(emptyForm());
  const [saving, setSaving] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);

  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [insights, setInsights] = useState<Record<string, Insights>>({});
  const [insightsLoading, setInsightsLoading] = useState<string | null>(null);

  const loadAll = useCallback(async () => {
    try {
      const [dRes, cRes] = await Promise.all([
        apiFetch(`${API}/deals`),
        apiFetch(`${API}/capacity`),
      ]);
      if (!dRes.ok) throw new Error(`deals: HTTP ${dRes.status}`);
      if (!cRes.ok) throw new Error(`capacity: HTTP ${cRes.status}`);
      const dJson = await dRes.json();
      const cJson = await cRes.json();
      setDeals(dJson.deals || []);
      setCapacity(cJson);
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'failed to load');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    loadAll();
    const t = window.setInterval(loadAll, 30000); // 30s auto-refresh
    return () => window.clearInterval(t);
  }, [loadAll]);

  useEffect(() => {
    apiFetch(`${API}/offers-lite`)
      .then(r => (r.ok ? r.json() : Promise.reject(new Error(`HTTP ${r.status}`))))
      .then(j => setOffers(j.offers || []))
      .catch(() => setOffers([]));
  }, []);

  // Live formula preview inside the form, as the user types.
  const livePlan = useMemo(
    () =>
      computePlan(
        parseFloat(form.budget) || 0,
        parseFloat(form.ecpm_goal) || 0,
        parseFloat(form.ecpa_goal) || 0,
        parseInt(form.avg_campaign_size, 10) || 0,
      ),
    [form.budget, form.ecpm_goal, form.ecpa_goal, form.avg_campaign_size],
  );

  const openCreate = () => {
    setEditingId(null);
    setForm(emptyForm());
    setFormError(null);
    setShowModal(true);
  };

  const openEdit = (d: Deal) => {
    setEditingId(d.id);
    setForm({
      name: d.name,
      offer_id: d.offer_id,
      everflow_offer_id: d.everflow_offer_id,
      budget: String(d.budget),
      ecpm_goal: String(d.ecpm_goal),
      ecpa_goal: d.ecpa_goal > 0 ? String(d.ecpa_goal) : '',
      avg_campaign_size: String(d.avg_campaign_size),
      start_date: d.start_date,
      status: d.status,
      notes: d.notes,
    });
    setFormError(null);
    setShowModal(true);
  };

  const saveDeal = async () => {
    const budget = parseFloat(form.budget);
    const ecpm = parseFloat(form.ecpm_goal);
    if (!form.name.trim()) { setFormError('Name is required'); return; }
    if (!(budget > 0)) { setFormError('Budget must be > 0'); return; }
    if (!(ecpm > 0)) { setFormError('eCPM goal must be > 0'); return; }
    setSaving(true);
    setFormError(null);
    try {
      const body: Record<string, unknown> = {
        name: form.name.trim(),
        offer_id: form.offer_id,
        everflow_offer_id: form.everflow_offer_id.trim(),
        budget,
        ecpm_goal: ecpm,
        ecpa_goal: parseFloat(form.ecpa_goal) || 0,
        avg_campaign_size: parseInt(form.avg_campaign_size, 10) || 160000,
        start_date: form.start_date,
        notes: form.notes,
      };
      if (editingId) body.status = form.status;
      const res = await apiFetch(editingId ? `${API}/deals/${editingId}` : `${API}/deals`, {
        method: editingId ? 'PUT' : 'POST',
        body: JSON.stringify(body),
      });
      if (!res.ok) {
        const j = await res.json().catch(() => ({}));
        throw new Error(j.error || `HTTP ${res.status}`);
      }
      setShowModal(false);
      setInsights({}); // invalidate cached insights
      await loadAll();
    } catch (e) {
      setFormError(e instanceof Error ? e.message : 'save failed');
    } finally {
      setSaving(false);
    }
  };

  const deleteDeal = async (d: Deal) => {
    if (!window.confirm(`Delete CPM deal "${d.name}"?`)) return;
    try {
      const res = await apiFetch(`${API}/deals/${d.id}`, { method: 'DELETE' });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      if (expandedId === d.id) setExpandedId(null);
      await loadAll();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'delete failed');
    }
  };

  const toggleExpand = async (d: Deal) => {
    if (expandedId === d.id) { setExpandedId(null); return; }
    setExpandedId(d.id);
    if (!insights[d.id]) {
      setInsightsLoading(d.id);
      try {
        const res = await apiFetch(`${API}/deals/${d.id}/insights`);
        if (res.ok) {
          const j: Insights = await res.json();
          setInsights(prev => ({ ...prev, [d.id]: j }));
        }
      } finally {
        setInsightsLoading(null);
      }
    }
  };

  // ─── Render pieces ─────────────────────────────────────────────────────────
  const renderCapacityStrip = () => {
    if (!capacity) return null;
    const util = capacity.utilization_pct * 100;
    return (
      <div style={{
        display: 'flex', gap: 24, alignItems: 'center', flexWrap: 'wrap',
        background: C.panel, border: `1px solid ${C.border}`, borderRadius: 10,
        padding: '14px 18px', marginBottom: 18,
      }}>
        <FontAwesomeIcon icon={faGaugeHigh} style={{ color: C.indigo, fontSize: 20 }} />
        <div>
          <div style={{ fontSize: 11, color: C.muted, textTransform: 'uppercase', letterSpacing: 0.5 }}>Platform daily (14d avg)</div>
          <div style={{ fontSize: 18, fontWeight: 700, color: C.heading }}>{fmtInt(capacity.platform_daily)}</div>
        </div>
        <div>
          <div style={{ fontSize: 11, color: C.muted, textTransform: 'uppercase', letterSpacing: 0.5 }}>Required daily ({capacity.active_deals} active deal{capacity.active_deals === 1 ? '' : 's'})</div>
          <div style={{ fontSize: 18, fontWeight: 700, color: C.heading }}>{fmtInt(capacity.total_required_daily)}</div>
        </div>
        <div>
          <div style={{ fontSize: 11, color: C.muted, textTransform: 'uppercase', letterSpacing: 0.5 }}>Headroom</div>
          <div style={{ fontSize: 18, fontWeight: 700, color: capacity.headroom >= 0 ? C.green : C.red }}>
            {capacity.headroom >= 0 ? '' : '−'}{fmtInt(Math.abs(capacity.headroom))}/day
          </div>
        </div>
        <div>
          <div style={{ fontSize: 11, color: C.muted, textTransform: 'uppercase', letterSpacing: 0.5 }}>Utilization</div>
          <div style={{ fontSize: 18, fontWeight: 700, color: riskColor(capacity.risk) }}>{util.toFixed(1)}%</div>
        </div>
        <span style={{
          marginLeft: 'auto', padding: '4px 14px', borderRadius: 999, fontSize: 12, fontWeight: 700,
          color: riskColor(capacity.risk), border: `1px solid ${riskColor(capacity.risk)}`,
          background: 'rgba(10,20,45,0.5)',
        }}>
          CAPACITY RISK: {capacity.risk}
        </span>
      </div>
    );
  };

  const renderProgressBar = (d: Deal) => {
    const pct = Math.min(d.progress.pct_volume_delivered * 100, 100);
    return (
      <div style={{ minWidth: 140 }}>
        <div style={{ height: 8, background: 'rgba(10,20,45,0.8)', borderRadius: 4, overflow: 'hidden' }}>
          <div style={{
            width: `${pct}%`, height: '100%', borderRadius: 4,
            background: d.progress.on_pace ? C.green : C.amber,
            transition: 'width 0.4s ease',
          }} />
        </div>
        <div style={{ fontSize: 11, color: C.muted, marginTop: 3 }}>
          {fmtInt(d.progress.sent)} / {fmtInt(d.planned_volume)} ({(d.progress.pct_volume_delivered * 100).toFixed(1)}%)
        </div>
      </div>
    );
  };

  const renderInsights = (d: Deal) => {
    const ins = insights[d.id];
    if (insightsLoading === d.id && !ins) {
      return (
        <div style={{ padding: 24, textAlign: 'center', color: C.muted }}>
          <FontAwesomeIcon icon={faSpinner} spin /> Loading insights…
        </div>
      );
    }
    if (!ins) return <div style={{ padding: 24, color: C.muted }}>No insights available.</div>;

    const chartData = ins.daily_series.map(p => ({ date: p.date.slice(5), sent: p.sent }));
    return (
      <div style={{ padding: '16px 20px', display: 'flex', flexDirection: 'column', gap: 16 }}>
        {/* Recommendations */}
        <div>
          <div style={{ fontSize: 12, fontWeight: 700, color: C.heading, marginBottom: 8, textTransform: 'uppercase', letterSpacing: 0.5 }}>
            Recommendations
          </div>
          {ins.recommendations.length === 0 ? (
            <div style={{ color: C.muted, fontSize: 13 }}>Nothing to flag — deal is tracking.</div>
          ) : (
            <ul style={{ margin: 0, paddingLeft: 18, display: 'flex', flexDirection: 'column', gap: 6 }}>
              {ins.recommendations.map((rec, i) => (
                <li key={i} style={{ fontSize: 13, color: C.heading, lineHeight: 1.5 }}>{rec}</li>
              ))}
            </ul>
          )}
        </div>

        {/* Pace chart: daily sent bars vs required-daily reference */}
        {chartData.length > 0 && (
          <div>
            <div style={{ fontSize: 12, fontWeight: 700, color: C.heading, marginBottom: 8, textTransform: 'uppercase', letterSpacing: 0.5 }}>
              Daily sent vs required pace ({fmtInt(d.progress.required_daily)}/day)
            </div>
            <div style={{ height: 220, background: 'rgba(10,20,45,0.4)', borderRadius: 8, padding: '12px 8px' }}>
              <ResponsiveContainer width="100%" height="100%">
                <BarChart data={chartData} margin={{ top: 8, right: 16, left: 0, bottom: 0 }}>
                  <CartesianGrid strokeDasharray="3 3" stroke="rgba(99,102,241,0.15)" />
                  <XAxis dataKey="date" tick={{ fill: C.muted, fontSize: 11 }} stroke={C.border} />
                  <YAxis tick={{ fill: C.muted, fontSize: 11 }} stroke={C.border} tickFormatter={(v: number) => fmtInt(v)} />
                  <RechartsTooltip
                    contentStyle={{ background: 'rgba(10,20,45,0.95)', border: `1px solid ${C.border}`, borderRadius: 6, color: C.heading }}
                    formatter={(v: number) => [fmtInt(v), 'Sent']}
                  />
                  <ReferenceLine
                    y={d.progress.required_daily}
                    stroke={C.amber}
                    strokeDasharray="6 4"
                    label={{ value: 'required/day', fill: C.amber, fontSize: 11, position: 'insideTopRight' }}
                  />
                  <Bar dataKey="sent" fill={C.indigo} radius={[3, 3, 0, 0]} />
                </BarChart>
              </ResponsiveContainer>
            </div>
          </div>
        )}

        {/* Top converting domains */}
        {ins.top_domains.length > 0 && (
          <div>
            <div style={{ fontSize: 12, fontWeight: 700, color: C.heading, marginBottom: 8, textTransform: 'uppercase', letterSpacing: 0.5 }}>
              Top converting domains
            </div>
            <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
              {ins.top_domains.map(td => (
                <span key={td.domain} style={{
                  padding: '4px 12px', borderRadius: 999, fontSize: 12,
                  background: 'rgba(16,185,129,0.12)', border: '1px solid rgba(16,185,129,0.4)', color: C.green,
                }}>
                  {td.domain} · {td.conversions}
                </span>
              ))}
            </div>
          </div>
        )}
      </div>
    );
  };

  const renderModal = () => {
    if (!showModal) return null;
    return (
      <div
        style={{
          position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.65)', zIndex: 1000,
          display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 20,
        }}
        onClick={() => !saving && setShowModal(false)}
      >
        <div
          style={{
            background: 'rgb(13,22,45)', border: `1px solid ${C.border}`, borderRadius: 12,
            width: 'min(640px, 100%)', maxHeight: '90vh', overflowY: 'auto', padding: 24,
          }}
          onClick={e => e.stopPropagation()}
        >
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 18 }}>
            <h3 style={{ margin: 0, color: C.heading, fontSize: 17 }}>
              {editingId ? 'Edit CPM Deal' : 'New CPM Deal'}
            </h3>
            <button
              onClick={() => setShowModal(false)}
              style={{ background: 'none', border: 'none', color: C.muted, cursor: 'pointer', fontSize: 16 }}
              title="Close"
            >
              <FontAwesomeIcon icon={faTimes} />
            </button>
          </div>

          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 14 }}>
            <div style={{ gridColumn: '1 / -1' }}>
              <label style={labelStyle}>Deal name</label>
              <input
                style={inputStyle}
                value={form.name}
                placeholder="e.g. ADT — June CPM buy"
                onChange={e => setForm({ ...form, name: e.target.value })}
              />
            </div>
            <div style={{ gridColumn: '1 / -1' }}>
              <label style={labelStyle}>Offer (maps live delivery + conversions back to the deal)</label>
              <select
                style={inputStyle}
                value={form.offer_id}
                onChange={e => {
                  const o = offers.find(x => x.id === e.target.value);
                  setForm({
                    ...form,
                    offer_id: e.target.value,
                    everflow_offer_id: o ? o.everflow_offer_id : form.everflow_offer_id,
                  });
                }}
              >
                <option value="">— no offer mapped —</option>
                {offers.map(o => (
                  <option key={o.id} value={o.id}>
                    {o.name}{o.payout > 0 ? ` (${fmtMoney(o.payout)} payout)` : ''}
                  </option>
                ))}
              </select>
            </div>
            <div>
              <label style={labelStyle}>Budget ($)</label>
              <input
                style={inputStyle} type="number" min="0" step="0.01"
                value={form.budget} placeholder="2000 (ADT example)"
                onChange={e => setForm({ ...form, budget: e.target.value })}
              />
            </div>
            <div>
              <label style={labelStyle}>eCPM goal ($ per 1,000 sent)</label>
              <input
                style={inputStyle} type="number" min="0" step="0.01"
                value={form.ecpm_goal} placeholder="0.70 (ADT example)"
                onChange={e => setForm({ ...form, ecpm_goal: e.target.value })}
              />
            </div>
            <div>
              <label style={labelStyle}>eCPA goal ($ per conversion)</label>
              <input
                style={inputStyle} type="number" min="0" step="0.01"
                value={form.ecpa_goal} placeholder="38 (ADT example)"
                onChange={e => setForm({ ...form, ecpa_goal: e.target.value })}
              />
            </div>
            <div>
              <label style={labelStyle}>Avg campaign size</label>
              <input
                style={inputStyle} type="number" min="1" step="1000"
                value={form.avg_campaign_size} placeholder="160000"
                onChange={e => setForm({ ...form, avg_campaign_size: e.target.value })}
              />
            </div>
            <div>
              <label style={labelStyle}>Start date</label>
              <input
                style={inputStyle} type="date"
                value={form.start_date}
                onChange={e => setForm({ ...form, start_date: e.target.value })}
              />
            </div>
            {editingId && (
              <div>
                <label style={labelStyle}>Status</label>
                <select
                  style={inputStyle}
                  value={form.status}
                  onChange={e => setForm({ ...form, status: e.target.value })}
                >
                  <option value="active">active</option>
                  <option value="paused">paused</option>
                  <option value="completed">completed</option>
                </select>
              </div>
            )}
            <div style={{ gridColumn: '1 / -1' }}>
              <label style={labelStyle}>Notes</label>
              <textarea
                style={{ ...inputStyle, minHeight: 60, resize: 'vertical' }}
                value={form.notes}
                onChange={e => setForm({ ...form, notes: e.target.value })}
              />
            </div>
          </div>

          {/* Live spreadsheet math */}
          <div style={{
            marginTop: 16, padding: '14px 16px', borderRadius: 8,
            background: 'rgba(99,102,241,0.08)', border: `1px solid ${C.border}`,
            display: 'flex', gap: 28, flexWrap: 'wrap',
          }}>
            <div>
              <div style={{ fontSize: 11, color: C.muted }}>planned volume = budget / eCPM × 1,000</div>
              <div style={{ fontSize: 20, fontWeight: 700, color: C.indigo }}>
                {livePlan.planned > 0 ? fmtInt(livePlan.planned) : '—'}
              </div>
            </div>
            <div>
              <div style={{ fontSize: 11, color: C.muted }}>conversions needed = ⌈budget / eCPA⌉</div>
              <div style={{ fontSize: 20, fontWeight: 700, color: C.green }}>
                {livePlan.conversions > 0 ? fmtInt(livePlan.conversions) : '—'}
              </div>
            </div>
            <div>
              <div style={{ fontSize: 11, color: C.muted }}>days to finish = ⌈planned / avg campaign⌉</div>
              <div style={{ fontSize: 20, fontWeight: 700, color: C.amber }}>
                {livePlan.days > 0 ? livePlan.days : '—'}
              </div>
            </div>
          </div>

          {formError && (
            <div style={{ marginTop: 12, color: C.red, fontSize: 13 }}>{formError}</div>
          )}

          <div style={{ marginTop: 18, display: 'flex', justifyContent: 'flex-end', gap: 10 }}>
            <button
              onClick={() => setShowModal(false)}
              disabled={saving}
              style={{
                padding: '8px 18px', borderRadius: 6, border: `1px solid ${C.border}`,
                background: 'transparent', color: C.muted, cursor: 'pointer', fontSize: 13,
              }}
            >
              Cancel
            </button>
            <button
              onClick={saveDeal}
              disabled={saving}
              style={{
                padding: '8px 18px', borderRadius: 6, border: 'none',
                background: C.indigo, color: '#fff', cursor: 'pointer', fontSize: 13, fontWeight: 600,
              }}
            >
              {saving ? <FontAwesomeIcon icon={faSpinner} spin /> : editingId ? 'Save changes' : 'Create deal'}
            </button>
          </div>
        </div>
      </div>
    );
  };

  // ─── Layout ────────────────────────────────────────────────────────────────
  return (
    <div style={{ padding: 24 }}>
      {/* Header */}
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 18 }}>
        <div>
          <h2 style={{ margin: 0, color: C.heading, fontSize: 22, display: 'flex', alignItems: 'center', gap: 10 }}>
            <FontAwesomeIcon icon={faCalculator} style={{ color: C.indigo }} />
            CPM Planner
          </h2>
          <div style={{ color: C.muted, fontSize: 13, marginTop: 4 }}>
            Price CPM deals, plan the volume, and track live delivery, pace and earnings against goal.
          </div>
        </div>
        <button
          onClick={openCreate}
          style={{
            padding: '9px 18px', borderRadius: 8, border: 'none',
            background: `linear-gradient(135deg, ${C.indigo}, #8b5cf6)`,
            color: '#fff', fontWeight: 600, fontSize: 13, cursor: 'pointer',
            display: 'flex', alignItems: 'center', gap: 8,
          }}
        >
          <FontAwesomeIcon icon={faPlus} /> New Deal
        </button>
      </div>

      {error && (
        <div style={{
          marginBottom: 14, padding: '10px 14px', borderRadius: 8, fontSize: 13,
          background: 'rgba(239,68,68,0.1)', border: '1px solid rgba(239,68,68,0.4)', color: C.red,
        }}>
          {error}
        </div>
      )}

      {/* Capacity strip */}
      {renderCapacityStrip()}

      {/* Deals */}
      {loading ? (
        <div style={{ padding: 60, textAlign: 'center', color: C.muted }}>
          <FontAwesomeIcon icon={faSpinner} spin style={{ fontSize: 22 }} />
        </div>
      ) : deals.length === 0 ? (
        <div style={{
          background: C.panel, border: `1px dashed ${C.border}`, borderRadius: 12,
          padding: '48px 32px', textAlign: 'center',
        }}>
          <FontAwesomeIcon icon={faCalculator} style={{ fontSize: 30, color: C.indigo, marginBottom: 14 }} />
          <div style={{ color: C.heading, fontSize: 16, fontWeight: 600, marginBottom: 10 }}>
            No CPM deals yet
          </div>
          <div style={{ color: C.muted, fontSize: 13, lineHeight: 1.8, maxWidth: 560, margin: '0 auto 18px' }}>
            A deal is priced from three numbers:<br />
            <span style={{ color: C.heading }}>planned volume = budget ÷ eCPM goal × 1,000</span> ·{' '}
            <span style={{ color: C.heading }}>conversions needed = ⌈budget ÷ eCPA goal⌉</span> ·{' '}
            <span style={{ color: C.heading }}>days to finish = ⌈planned volume ÷ avg campaign size⌉</span>
            <br />
            Example — ADT: $2,000 budget at a $0.70 eCPM goal and $38 eCPA goal with 160,000-recipient
            campaigns → <span style={{ color: C.indigo }}>2,857,143 planned sends</span>,{' '}
            <span style={{ color: C.green }}>53 conversions needed</span>,{' '}
            <span style={{ color: C.amber }}>18 days to finish</span>.
          </div>
          <button
            onClick={openCreate}
            style={{
              padding: '9px 20px', borderRadius: 8, border: 'none', background: C.indigo,
              color: '#fff', fontWeight: 600, fontSize: 13, cursor: 'pointer',
            }}
          >
            <FontAwesomeIcon icon={faPlus} /> Create your first deal
          </button>
        </div>
      ) : (
        <div style={{ background: C.panel, border: `1px solid ${C.border}`, borderRadius: 12, overflowX: 'auto' }}>
          <table style={{ width: '100%', borderCollapse: 'collapse' }}>
            <thead>
              <tr>
                <th style={thStyle}></th>
                <th style={thStyle}>Name</th>
                <th style={thStyle}>Offer</th>
                <th style={thStyle}>Budget</th>
                <th style={thStyle}>eCPM Goal</th>
                <th style={thStyle}>Planned Volume</th>
                <th style={thStyle}>Conversions</th>
                <th style={thStyle}>Days</th>
                <th style={thStyle}>Delivered</th>
                <th style={thStyle}>Actual eCPM</th>
                <th style={thStyle}>eCPA Actual</th>
                <th style={thStyle}>Status</th>
                <th style={thStyle}></th>
              </tr>
            </thead>
            <tbody>
              {deals.map(d => {
                const expanded = expandedId === d.id;
                const convColor =
                  d.progress.conversions >= d.conversions_needed && d.conversions_needed > 0
                    ? C.green
                    : d.progress.conversions > 0
                      ? C.amber
                      : C.muted;
                // Operator earns: actual eCPM at/above goal = green.
                const ecpmColor = d.progress.actual_ecpm >= d.ecpm_goal && d.progress.sent > 0 ? C.green : C.red;
                return (
                  <React.Fragment key={d.id}>
                    <tr
                      onClick={() => toggleExpand(d)}
                      style={{ cursor: 'pointer', background: expanded ? 'rgba(99,102,241,0.08)' : 'transparent' }}
                    >
                      <td style={{ ...tdStyle, width: 28, color: C.muted }}>
                        <FontAwesomeIcon icon={expanded ? faChevronDown : faChevronRight} style={{ fontSize: 11 }} />
                      </td>
                      <td style={{ ...tdStyle, fontWeight: 600 }}>{d.name}</td>
                      <td style={{ ...tdStyle, color: d.offer_id ? C.heading : C.muted }}>
                        {d.offer_name || (d.everflow_offer_id ? `EF ${d.everflow_offer_id}` : 'unmapped')}
                      </td>
                      <td style={tdStyle}>{fmtMoney(d.budget)}</td>
                      <td style={tdStyle}>{fmtMoney(d.ecpm_goal)}</td>
                      <td style={tdStyle}>{fmtInt(d.planned_volume)}</td>
                      <td style={tdStyle}>
                        <span style={{ color: convColor, fontWeight: 600 }}>{fmtInt(d.progress.conversions)}</span>
                        <span style={{ color: C.muted }}> / {fmtInt(d.conversions_needed)}</span>
                      </td>
                      <td style={tdStyle}>
                        <span style={{ color: d.progress.days_elapsed > d.days_to_finish ? C.red : C.heading }}>
                          {d.progress.days_elapsed}
                        </span>
                        <span style={{ color: C.muted }}> / {d.days_to_finish}</span>
                      </td>
                      <td style={tdStyle}>{renderProgressBar(d)}</td>
                      <td style={tdStyle}>
                        <span style={{ color: ecpmColor, fontWeight: 600 }}>
                          {d.progress.sent > 0 ? fmtMoney(d.progress.actual_ecpm) : '—'}
                        </span>
                        <span style={{ color: C.muted, fontSize: 11 }}> vs {fmtMoney(d.ecpm_goal)}</span>
                      </td>
                      <td style={tdStyle}>
                        {d.progress.actual_ecpa > 0 ? (
                          <span style={{ color: d.ecpa_goal > 0 && d.progress.actual_ecpa <= d.ecpa_goal ? C.green : C.amber }}>
                            {fmtMoney(d.progress.actual_ecpa)}
                          </span>
                        ) : (
                          <span style={{ color: C.muted }}>—</span>
                        )}
                      </td>
                      <td style={tdStyle}>
                        <span style={{
                          padding: '3px 10px', borderRadius: 999, fontSize: 11, fontWeight: 700,
                          color: statusColor(d.status), border: `1px solid ${statusColor(d.status)}`,
                        }}>
                          {d.status.toUpperCase()}
                        </span>
                      </td>
                      <td style={{ ...tdStyle, width: 70 }}>
                        <button
                          onClick={e => { e.stopPropagation(); openEdit(d); }}
                          title="Edit deal"
                          style={{ background: 'none', border: 'none', color: C.muted, cursor: 'pointer', marginRight: 10 }}
                        >
                          <FontAwesomeIcon icon={faPen} />
                        </button>
                        <button
                          onClick={e => { e.stopPropagation(); deleteDeal(d); }}
                          title="Delete deal"
                          style={{ background: 'none', border: 'none', color: C.red, cursor: 'pointer' }}
                        >
                          <FontAwesomeIcon icon={faTrash} />
                        </button>
                      </td>
                    </tr>
                    {expanded && (
                      <tr>
                        <td colSpan={13} style={{ ...tdStyle, padding: 0, background: 'rgba(10,20,45,0.35)', whiteSpace: 'normal' }}>
                          {renderInsights(d)}
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

      {renderModal()}
    </div>
  );
};

export default CpmPlanner;
