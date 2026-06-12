import React, { useState, useEffect, useCallback, useMemo, useRef } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faCalculator, faPlus, faTimes, faTrash, faPen, faSpinner,
  faChevronDown, faChevronRight, faGaugeHigh, faUpload,
} from '@fortawesome/free-solid-svg-icons';
import {
  ResponsiveContainer, BarChart, Bar, XAxis, YAxis, CartesianGrid,
  Tooltip as RechartsTooltip, ReferenceLine, ComposedChart, Line, Legend,
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
  conversions: number;          // TOTAL = tracked + manual (back-compat field name)
  conversions_tracked: number;  // everflow postbacks
  conversions_manual: number;   // operator CSV uploads / quick-adds
  manual_revenue: number;       // raw revenue reported on manual rows
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

interface DailyPoint { date: string; sent: number; conversions: number; }
interface DomainConv { domain: string; conversions: number; }

// Manual conversions (mirror cpmManualConvEntry / HandleListDealConversions).
interface ConvEntry {
  id: string;
  converted_at: string;
  count: number;
  revenue: number;
  sub1: string;
  sub2: string;
  conversion_id: string;
  source: string; // 'csv' | 'manual'
  note: string;
  created_at: string;
}
interface ConvList {
  entries: ConvEntry[];
  totals: { manual_total: number; manual_revenue: number };
}

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

// ── Offer performance (deal detail embed) — mirrors OfferStatsResponse from
// offer_center_handlers.go, served by /cpm-planner/deals/{id}/offer-performance.
// Same shared backend aggregation the Offers tab Performance view uses.
interface OfferPerfTotals {
  sent: number;
  delivered: number;
  opened: number;   // HUMAN opens (machine/MPP excluded server-side)
  clicked: number;  // HUMAN clicks
  hard_bounces: number;
  soft_bounces: number;
  deferred: number;
  complaints: number;
  conversions: number;
  suppression_total: number;
}

interface OfferPerfCampaign {
  id: string;
  name: string;
  status: string;
  scheduled_at: string | null;
  sent: number;
  delivered: number;
  opens: number;
  clicks: number;
}

interface OfferPerfDaily {
  date: string;
  sent: number;
  delivered: number;
  opened: number;
  clicked: number;
  conversions: number;
}

interface SuppressionWeek { week_start: string; count: number; }

interface OfferPerfStats {
  offer_id: string;
  days: number;
  totals: OfferPerfTotals;
  campaign_count: number;
  campaigns: OfferPerfCampaign[];
  daily: OfferPerfDaily[];
  dnm_list_size: number;
  audience_size: number; // 0 = unknown (no completed DNM scrub) → skip share
  suppression_weekly: SuppressionWeek[];
}

interface DealOfferPerformance {
  deal_id: string;
  offer_id: string;
  offer_name: string;
  performance: OfferPerfStats | null;
  note?: string;
}

// sessionStorage handoff keys written by OfferManagement (Offers tab →
// CPM Planner deep links). Read-once on mount, then removed.
const PREFILL_KEY = 'cpmPlannerPrefill';     // JSON {offer_id, everflow_offer_id, payout, name}
const FOCUS_DEAL_KEY = 'cpmPlannerFocusDeal'; // deal id to auto-expand

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
  const [offerPerf, setOfferPerf] = useState<Record<string, DealOfferPerformance>>({});
  const [offerPerfLoading, setOfferPerfLoading] = useState<string | null>(null);
  // Manual conversions per deal (recent entries + totals), refreshed on
  // expand and after every add/upload/delete.
  const [convData, setConvData] = useState<Record<string, ConvList>>({});
  const [convForm, setConvForm] = useState({
    date: new Date().toISOString().slice(0, 10),
    count: '1',
    revenue: '',
    note: '',
  });
  const [convBusy, setConvBusy] = useState(false);
  const [convMsg, setConvMsg] = useState<Record<string, { text: string; ok: boolean }>>({});
  const [convListOpen, setConvListOpen] = useState<Record<string, boolean>>({});
  const csvInputRef = useRef<HTMLInputElement | null>(null);
  // Deal id handed off from the Offers tab ("View in CPM Planner") —
  // auto-expanded once the deal list loads.
  const [focusDealId, setFocusDealId] = useState<string | null>(
    () => sessionStorage.getItem(FOCUS_DEAL_KEY),
  );

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
    const t = window.setInterval(loadAll, 300_000); // 5-min auto-refresh (heavy aggregates server-side)
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
      setOfferPerf({}); // offer mapping may have changed — refetch performance
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

  const loadConversions = useCallback(async (dealId: string) => {
    try {
      const res = await apiFetch(`${API}/deals/${dealId}/conversions`);
      if (res.ok) {
        const j: ConvList = await res.json();
        setConvData(prev => ({ ...prev, [dealId]: j }));
      }
    } catch { /* keep whatever we had */ }
  }, []);

  // After a conversions mutation the deal row (pacing tiles/totals), the
  // entries list AND the cached insights (recommendations + daily series)
  // are all stale — refresh the three together.
  const refreshAfterConv = useCallback(async (dealId: string) => {
    await Promise.all([
      loadAll(),
      loadConversions(dealId),
      (async () => {
        try {
          const res = await apiFetch(`${API}/deals/${dealId}/insights`);
          if (res.ok) {
            const j: Insights = await res.json();
            setInsights(prev => ({ ...prev, [dealId]: j }));
          }
        } catch { /* keep stale insights */ }
      })(),
    ]);
  }, [loadAll, loadConversions]);

  const postConversions = useCallback(async (dealId: string, body: Record<string, unknown>): Promise<boolean> => {
    setConvBusy(true);
    try {
      const res = await apiFetch(`${API}/deals/${dealId}/conversions`, {
        method: 'POST',
        body: JSON.stringify(body),
      });
      const j = await res.json().catch(() => ({} as Record<string, unknown>));
      if (!res.ok) throw new Error((j as { error?: string }).error || `HTTP ${res.status}`);
      const result = j as { inserted: number; duplicates: number; parse_errors: number; errors?: string[] };
      const extra = result.errors && result.errors.length > 0 ? ` — ${result.errors[0]}` : '';
      setConvMsg(prev => ({
        ...prev,
        [dealId]: {
          text: `${fmtInt(result.inserted)} inserted · ${fmtInt(result.duplicates)} duplicates · ${fmtInt(result.parse_errors)} parse errors${extra}`,
          ok: result.parse_errors === 0,
        },
      }));
      await refreshAfterConv(dealId);
      return true;
    } catch (e) {
      setConvMsg(prev => ({
        ...prev,
        [dealId]: { text: e instanceof Error ? e.message : 'upload failed', ok: false },
      }));
      return false;
    } finally {
      setConvBusy(false);
    }
  }, [refreshAfterConv]);

  const addManualConversions = async (d: Deal) => {
    if (!convForm.date) {
      setConvMsg(prev => ({ ...prev, [d.id]: { text: 'Pick a conversion date', ok: false } }));
      return;
    }
    const ok = await postConversions(d.id, {
      entries: [{
        converted_at: convForm.date,
        count: parseInt(convForm.count, 10) || 1,
        revenue: parseFloat(convForm.revenue) || 0,
        note: convForm.note.trim(),
      }],
    });
    if (ok) setConvForm(f => ({ ...f, count: '1', revenue: '', note: '' }));
  };

  const uploadConversionsCsv = async (d: Deal, file: File) => {
    const text = await file.text();
    await postConversions(d.id, { csv: text });
  };

  const deleteConversion = async (d: Deal, entry: ConvEntry) => {
    if (!window.confirm(`Delete this ${entry.source} entry (${entry.count} conversion${entry.count === 1 ? '' : 's'} on ${entry.converted_at.slice(0, 10)})?`)) return;
    try {
      const res = await apiFetch(`${API}/deals/${d.id}/conversions/${entry.id}`, { method: 'DELETE' });
      if (!res.ok) {
        const j = await res.json().catch(() => ({} as { error?: string }));
        throw new Error((j as { error?: string }).error || `HTTP ${res.status}`);
      }
      await refreshAfterConv(d.id);
    } catch (e) {
      setConvMsg(prev => ({ ...prev, [d.id]: { text: e instanceof Error ? e.message : 'delete failed', ok: false } }));
    }
  };

  const expandDeal = useCallback(async (dealId: string, hasInsights: boolean, hasPerf: boolean) => {
    setExpandedId(dealId);
    const tasks: Promise<void>[] = [loadConversions(dealId)];
    if (!hasInsights) {
      setInsightsLoading(dealId);
      tasks.push((async () => {
        try {
          const res = await apiFetch(`${API}/deals/${dealId}/insights`);
          if (res.ok) {
            const j: Insights = await res.json();
            setInsights(prev => ({ ...prev, [dealId]: j }));
          }
        } finally {
          setInsightsLoading(null);
        }
      })());
    }
    if (!hasPerf) {
      setOfferPerfLoading(dealId);
      tasks.push((async () => {
        try {
          const res = await apiFetch(`${API}/deals/${dealId}/offer-performance?days=30`);
          if (res.ok) {
            const j: DealOfferPerformance = await res.json();
            setOfferPerf(prev => ({ ...prev, [dealId]: j }));
          }
        } finally {
          setOfferPerfLoading(null);
        }
      })());
    }
    await Promise.all(tasks);
  }, [loadConversions]);

  const toggleExpand = (d: Deal) => {
    if (expandedId === d.id) { setExpandedId(null); return; }
    expandDeal(d.id, !!insights[d.id], !!offerPerf[d.id]);
  };

  // Offers tab → "Create deal from this offer" handoff: prefill the New Deal
  // modal from sessionStorage (offer id + everflow id + payout-as-eCPA-anchor).
  useEffect(() => {
    const raw = sessionStorage.getItem(PREFILL_KEY);
    if (!raw) return;
    sessionStorage.removeItem(PREFILL_KEY);
    try {
      const p = JSON.parse(raw) as { offer_id?: string; everflow_offer_id?: string; payout?: number; name?: string };
      setEditingId(null);
      setForm({
        ...emptyForm(),
        name: p.name ? `${p.name} — CPM deal` : '',
        offer_id: p.offer_id || '',
        everflow_offer_id: p.everflow_offer_id || '',
        // Offer payout = $ per conversion → natural eCPA-goal anchor.
        ecpa_goal: p.payout && p.payout > 0 ? String(p.payout) : '',
      });
      setFormError(null);
      setShowModal(true);
    } catch { /* malformed handoff — ignore */ }
  }, []);

  // Offers tab → "View in CPM Planner" handoff: auto-expand the linked deal
  // once the list arrives.
  useEffect(() => {
    if (!focusDealId || loading) return;
    sessionStorage.removeItem(FOCUS_DEAL_KEY);
    const d = deals.find(x => x.id === focusDealId);
    setFocusDealId(null);
    if (d) expandDeal(d.id, !!insights[d.id], !!offerPerf[d.id]);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [focusDealId, loading, deals]);

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

  // Offer performance panel embedded in the deal detail — same numbers as the
  // Offers tab Performance view (shared loadOfferStats aggregation server-side).
  const renderOfferPerformance = (d: Deal) => {
    const perf = offerPerf[d.id];
    if (offerPerfLoading === d.id && !perf) {
      return (
        <div style={{ padding: '12px 0', color: C.muted, fontSize: 13 }}>
          <FontAwesomeIcon icon={faSpinner} spin /> Loading offer performance…
        </div>
      );
    }
    if (!perf) return null;
    if (!perf.performance) {
      return (
        <div style={{ padding: '10px 14px', borderRadius: 8, fontSize: 13, color: C.amber, background: 'rgba(245,158,11,0.08)', border: '1px solid rgba(245,158,11,0.35)' }}>
          {perf.note || 'No offer mapped to this deal — offer performance unavailable.'}
        </div>
      );
    }
    const p = perf.performance;
    const t = p.totals;
    const tiles = [
      { label: 'Sent', value: fmtInt(t.sent), sub: `${p.campaign_count} campaigns`, color: C.indigo },
      { label: 'Delivered', value: fmtInt(t.delivered), sub: t.sent > 0 ? `${((t.delivered / t.sent) * 100).toFixed(1)}% of sent` : '—', color: C.green },
      { label: 'Human Opens', value: fmtInt(t.opened), sub: 'machine opens excluded', color: C.green },
      { label: 'Human Clicks', value: fmtInt(t.clicked), sub: 'machine clicks excluded', color: C.amber },
      // Bounce split is ALWAYS hard vs soft — never a combined number.
      { label: 'Hard Bounces', value: fmtInt(t.hard_bounces), sub: 'reputation risk', color: C.red },
      { label: 'Soft Bounces', value: fmtInt(t.soft_bounces), sub: 'usually transient', color: C.amber },
      { label: 'Conversions', value: fmtInt(t.conversions), sub: `conv / ${p.days}d`, color: '#3b82f6' },
      {
        label: 'Suppressed', value: fmtInt(t.suppression_total),
        sub: p.dnm_list_size > 0 ? `DNM list ${fmtInt(p.dnm_list_size)}` : 'all time, all reasons',
        color: C.muted,
      },
    ];
    const dailyData = p.daily.map(x => ({ date: x.date.slice(5), sent: x.sent, conversions: x.conversions }));
    const weeklyData = p.suppression_weekly.map(w => ({ week: w.week_start.slice(5), count: w.count }));
    const suppressedShare = p.audience_size > 0 ? (t.suppression_total / p.audience_size) * 100 : null;

    return (
      <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
        <div style={{ fontSize: 12, fontWeight: 700, color: C.heading, textTransform: 'uppercase', letterSpacing: 0.5 }}>
          Offer performance — {perf.offer_name || perf.offer_id} (last {p.days}d)
        </div>

        {/* Stat tiles */}
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(130px, 1fr))', gap: 10 }}>
          {tiles.map(m => (
            <div key={m.label} style={{ background: 'rgba(10,20,45,0.5)', border: `1px solid ${C.border}`, borderRadius: 8, padding: '10px 12px' }}>
              <div style={{ fontSize: 17, fontWeight: 700, color: m.color }}>{m.value}</div>
              <div style={{ fontSize: 11, color: C.heading, marginTop: 2 }}>{m.label}</div>
              <div style={{ fontSize: 10, color: C.muted, marginTop: 2 }}>{m.sub}</div>
            </div>
          ))}
        </div>

        {/* Suppression ceiling: share of scrub-time audience already suppressed */}
        {suppressedShare !== null ? (
          <div style={{
            padding: '8px 14px', borderRadius: 8, fontSize: 12,
            background: suppressedShare > 50 ? 'rgba(239,68,68,0.08)' : 'rgba(99,102,241,0.08)',
            border: `1px solid ${suppressedShare > 50 ? 'rgba(239,68,68,0.4)' : C.border}`,
            color: suppressedShare > 50 ? C.red : C.heading,
          }}>
            Deal ceiling: {suppressedShare.toFixed(1)}% of the last scrub audience ({fmtInt(p.audience_size)}) is already suppressed for this offer.
          </div>
        ) : (
          <div style={{ fontSize: 11, color: C.muted }}>
            Suppressed share vs audience not shown — no completed DNM scrub provides an audience size for this offer.
          </div>
        )}

        {/* Daily sent + conversions */}
        {dailyData.length > 0 && (
          <div>
            <div style={{ fontSize: 11, fontWeight: 700, color: C.muted, marginBottom: 6, textTransform: 'uppercase', letterSpacing: 0.5 }}>
              Daily sent &amp; conversions
            </div>
            <div style={{ height: 200, background: 'rgba(10,20,45,0.4)', borderRadius: 8, padding: '12px 8px' }}>
              <ResponsiveContainer width="100%" height="100%">
                <ComposedChart data={dailyData} margin={{ top: 8, right: 16, left: 0, bottom: 0 }}>
                  <CartesianGrid strokeDasharray="3 3" stroke="rgba(99,102,241,0.15)" />
                  <XAxis dataKey="date" tick={{ fill: C.muted, fontSize: 10 }} stroke={C.border} interval="preserveStartEnd" />
                  <YAxis yAxisId="vol" tick={{ fill: C.muted, fontSize: 10 }} stroke={C.border} tickFormatter={(v: number) => fmtInt(v)} />
                  <YAxis yAxisId="conv" orientation="right" tick={{ fill: C.muted, fontSize: 10 }} stroke={C.border} allowDecimals={false} />
                  <RechartsTooltip
                    contentStyle={{ background: 'rgba(10,20,45,0.95)', border: `1px solid ${C.border}`, borderRadius: 6, color: C.heading }}
                    formatter={(v: number | string) => (typeof v === 'number' ? fmtInt(v) : v)}
                  />
                  <Legend wrapperStyle={{ fontSize: 11 }} />
                  <Bar yAxisId="vol" dataKey="sent" name="Sent" fill="rgba(99,102,241,0.55)" radius={[2, 2, 0, 0]} />
                  <Line yAxisId="conv" type="monotone" dataKey="conversions" name="Conversions" stroke="#3b82f6" strokeWidth={2} dot={false} />
                </ComposedChart>
              </ResponsiveContainer>
            </div>
          </div>
        )}

        {/* Suppressed-count trend (8 weeks) — deal ceiling awareness */}
        {weeklyData.length > 0 && (
          <div>
            <div style={{ fontSize: 11, fontWeight: 700, color: C.muted, marginBottom: 6, textTransform: 'uppercase', letterSpacing: 0.5 }}>
              Suppressed per week (last 8 weeks, all reasons)
            </div>
            <div style={{ height: 140, background: 'rgba(10,20,45,0.4)', borderRadius: 8, padding: '12px 8px' }}>
              <ResponsiveContainer width="100%" height="100%">
                <BarChart data={weeklyData} margin={{ top: 8, right: 16, left: 0, bottom: 0 }}>
                  <CartesianGrid strokeDasharray="3 3" stroke="rgba(99,102,241,0.15)" />
                  <XAxis dataKey="week" tick={{ fill: C.muted, fontSize: 10 }} stroke={C.border} />
                  <YAxis tick={{ fill: C.muted, fontSize: 10 }} stroke={C.border} tickFormatter={(v: number) => fmtInt(v)} />
                  <RechartsTooltip
                    contentStyle={{ background: 'rgba(10,20,45,0.95)', border: `1px solid ${C.border}`, borderRadius: 6, color: C.heading }}
                    formatter={(v: number) => [fmtInt(v), 'Suppressed']}
                  />
                  <Bar dataKey="count" fill="rgba(239,68,68,0.55)" radius={[3, 3, 0, 0]} />
                </BarChart>
              </ResponsiveContainer>
            </div>
          </div>
        )}

        {/* Recent campaigns for the offer */}
        {p.campaigns.length > 0 && (
          <div>
            <div style={{ fontSize: 11, fontWeight: 700, color: C.muted, marginBottom: 6, textTransform: 'uppercase', letterSpacing: 0.5 }}>
              Recent campaigns {p.campaign_count > p.campaigns.length ? `(${p.campaigns.length} of ${p.campaign_count})` : ''}
            </div>
            <div style={{ background: 'rgba(10,20,45,0.4)', border: `1px solid ${C.border}`, borderRadius: 8, overflowX: 'auto' }}>
              <table style={{ width: '100%', borderCollapse: 'collapse' }}>
                <thead>
                  <tr>
                    <th style={thStyle}>Campaign</th>
                    <th style={thStyle}>Status</th>
                    <th style={thStyle}>Scheduled</th>
                    <th style={thStyle}>Sent</th>
                    <th style={thStyle}>Delivered</th>
                    <th style={thStyle}>Human Opens</th>
                    <th style={thStyle}>Human Clicks</th>
                  </tr>
                </thead>
                <tbody>
                  {p.campaigns.map(c => (
                    <tr key={c.id}>
                      <td style={{ ...tdStyle, maxWidth: 280, overflow: 'hidden', textOverflow: 'ellipsis' }} title={c.name}>{c.name || c.id}</td>
                      <td style={tdStyle}>
                        <span style={{ padding: '2px 8px', borderRadius: 999, fontSize: 11, fontWeight: 700, color: statusColor(c.status === 'sending' || c.status === 'sent' || c.status === 'completed' ? 'active' : c.status), border: `1px solid ${C.border}` }}>
                          {c.status}
                        </span>
                      </td>
                      <td style={tdStyle}>{c.scheduled_at ? new Date(c.scheduled_at).toLocaleString() : '—'}</td>
                      <td style={tdStyle}>{fmtInt(c.sent)}</td>
                      <td style={tdStyle}>{fmtInt(c.delivered)}</td>
                      <td style={tdStyle}>{fmtInt(c.opens)}</td>
                      <td style={tdStyle}>{fmtInt(c.clicks)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        )}
      </div>
    );
  };

  // Conversions block — pacing through conversions, not just volume. Tracked
  // (everflow postback) vs manual (operator CSV/quick-add) split, quick-add
  // form, Everflow CSV upload, and the recent manual entries with delete.
  const renderConversions = (d: Deal) => {
    const data = convData[d.id];
    const msg = convMsg[d.id];
    const listOpen = !!convListOpen[d.id];
    const p = d.progress;
    const chip = (label: string, value: number, color: string) => (
      <span key={label} style={{
        padding: '4px 12px', borderRadius: 999, fontSize: 12, fontWeight: 700,
        color, border: `1px solid ${color}`, background: 'rgba(10,20,45,0.5)',
      }}>
        {label}: {fmtInt(value)}
      </span>
    );
    const smallBtn: React.CSSProperties = {
      padding: '8px 14px', borderRadius: 6, border: `1px solid ${C.border}`,
      background: 'rgba(99,102,241,0.15)', color: C.heading, cursor: convBusy ? 'wait' : 'pointer',
      fontSize: 12, fontWeight: 600, display: 'flex', alignItems: 'center', gap: 6, whiteSpace: 'nowrap',
    };
    return (
      <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
        <div style={{ fontSize: 12, fontWeight: 700, color: C.heading, textTransform: 'uppercase', letterSpacing: 0.5 }}>
          Conversions
        </div>

        {/* Tracked / manual / total split */}
        <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'center' }}>
          {chip('Tracked (postback)', p.conversions_tracked, C.indigo)}
          {chip('Manual', p.conversions_manual, C.amber)}
          {chip('Total', p.conversions, C.green)}
          <span style={{ fontSize: 11, color: C.muted }}>
            of {fmtInt(d.conversions_needed)} needed
            {p.manual_revenue > 0 ? ` · manual revenue ${fmtMoney(p.manual_revenue)}` : ''}
          </span>
        </div>

        {/* Quick-add + CSV upload */}
        <div style={{
          display: 'flex', gap: 10, flexWrap: 'wrap', alignItems: 'flex-end',
          background: 'rgba(10,20,45,0.4)', border: `1px solid ${C.border}`, borderRadius: 8, padding: '10px 12px',
        }}>
          <div>
            <label style={labelStyle}>Date</label>
            <input
              style={{ ...inputStyle, width: 140 }} type="date" value={convForm.date}
              onChange={e => setConvForm({ ...convForm, date: e.target.value })}
            />
          </div>
          <div>
            <label style={labelStyle}>Count</label>
            <input
              style={{ ...inputStyle, width: 80 }} type="number" min="1" step="1" value={convForm.count}
              onChange={e => setConvForm({ ...convForm, count: e.target.value })}
            />
          </div>
          <div>
            <label style={labelStyle}>Revenue ($, optional)</label>
            <input
              style={{ ...inputStyle, width: 140 }} type="number" min="0" step="0.01" value={convForm.revenue}
              placeholder="payout-estimated if 0"
              onChange={e => setConvForm({ ...convForm, revenue: e.target.value })}
            />
          </div>
          <div style={{ flex: 1, minWidth: 160 }}>
            <label style={labelStyle}>Note</label>
            <input
              style={inputStyle} value={convForm.note} placeholder="e.g. advertiser-reported, jun11"
              onChange={e => setConvForm({ ...convForm, note: e.target.value })}
            />
          </div>
          <button onClick={() => addManualConversions(d)} disabled={convBusy} style={smallBtn}>
            {convBusy ? <FontAwesomeIcon icon={faSpinner} spin /> : <FontAwesomeIcon icon={faPlus} />} Add conversions
          </button>
          <button onClick={() => csvInputRef.current?.click()} disabled={convBusy} style={smallBtn} title="Everflow conversions export — deduped on conversion_id, re-upload safe">
            <FontAwesomeIcon icon={faUpload} /> Upload Everflow CSV
          </button>
          <input
            ref={csvInputRef} type="file" accept=".csv,text/csv" style={{ display: 'none' }}
            onChange={e => {
              const f = e.target.files && e.target.files[0];
              e.target.value = '';
              if (f) uploadConversionsCsv(d, f);
            }}
          />
        </div>

        {/* Result toast */}
        {msg && (
          <div style={{
            display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 10,
            padding: '8px 12px', borderRadius: 8, fontSize: 12,
            background: msg.ok ? 'rgba(16,185,129,0.1)' : 'rgba(239,68,68,0.1)',
            border: `1px solid ${msg.ok ? 'rgba(16,185,129,0.4)' : 'rgba(239,68,68,0.4)'}`,
            color: msg.ok ? C.green : C.red,
          }}>
            <span>{msg.text}</span>
            <button
              onClick={() => setConvMsg(prev => { const next = { ...prev }; delete next[d.id]; return next; })}
              style={{ background: 'none', border: 'none', color: 'inherit', cursor: 'pointer' }}
              title="Dismiss"
            >
              <FontAwesomeIcon icon={faTimes} />
            </button>
          </div>
        )}

        {/* Recent manual entries (collapsible) */}
        <div>
          <button
            onClick={() => setConvListOpen(prev => ({ ...prev, [d.id]: !listOpen }))}
            style={{
              background: 'none', border: 'none', color: C.muted, cursor: 'pointer',
              fontSize: 12, fontWeight: 700, padding: 0, display: 'flex', alignItems: 'center', gap: 6,
              textTransform: 'uppercase', letterSpacing: 0.5,
            }}
          >
            <FontAwesomeIcon icon={listOpen ? faChevronDown : faChevronRight} style={{ fontSize: 10 }} />
            Recent manual entries
            {data ? ` — ${fmtInt(data.totals.manual_total)} conversions · ${fmtMoney(data.totals.manual_revenue)} reported` : ''}
          </button>
          {listOpen && (
            !data ? (
              <div style={{ padding: '10px 0', color: C.muted, fontSize: 12 }}>
                <FontAwesomeIcon icon={faSpinner} spin /> Loading…
              </div>
            ) : data.entries.length === 0 ? (
              <div style={{ padding: '10px 0', color: C.muted, fontSize: 12 }}>
                No manual conversions yet — quick-add a count or upload an Everflow CSV.
              </div>
            ) : (
              <div style={{ marginTop: 8, background: 'rgba(10,20,45,0.4)', border: `1px solid ${C.border}`, borderRadius: 8, overflowX: 'auto', maxHeight: 280, overflowY: 'auto' }}>
                <table style={{ width: '100%', borderCollapse: 'collapse' }}>
                  <thead>
                    <tr>
                      <th style={thStyle}>Converted</th>
                      <th style={thStyle}>Count</th>
                      <th style={thStyle}>Revenue</th>
                      <th style={thStyle}>Source</th>
                      <th style={thStyle}>Conversion ID</th>
                      <th style={thStyle}>Sub1 / Sub2</th>
                      <th style={thStyle}>Note</th>
                      <th style={thStyle}></th>
                    </tr>
                  </thead>
                  <tbody>
                    {data.entries.map(e => (
                      <tr key={e.id}>
                        <td style={tdStyle}>{e.converted_at.slice(0, 10)}</td>
                        <td style={tdStyle}>{fmtInt(e.count)}</td>
                        <td style={tdStyle}>{e.revenue > 0 ? fmtMoney(e.revenue) : <span style={{ color: C.muted }}>—</span>}</td>
                        <td style={tdStyle}>
                          <span style={{
                            padding: '2px 8px', borderRadius: 999, fontSize: 11, fontWeight: 700,
                            color: e.source === 'csv' ? C.indigo : C.amber, border: `1px solid ${C.border}`,
                          }}>
                            {e.source}
                          </span>
                        </td>
                        <td style={{ ...tdStyle, maxWidth: 140, overflow: 'hidden', textOverflow: 'ellipsis', color: C.muted }} title={e.conversion_id}>
                          {e.conversion_id || '—'}
                        </td>
                        <td style={{ ...tdStyle, maxWidth: 220, overflow: 'hidden', textOverflow: 'ellipsis', color: C.muted }} title={`${e.sub1} / ${e.sub2}`}>
                          {e.sub1 || e.sub2 ? `${e.sub1 || '—'} / ${e.sub2 || '—'}` : '—'}
                        </td>
                        <td style={{ ...tdStyle, maxWidth: 180, overflow: 'hidden', textOverflow: 'ellipsis' }} title={e.note}>
                          {e.note || <span style={{ color: C.muted }}>—</span>}
                        </td>
                        <td style={{ ...tdStyle, width: 36 }}>
                          <button
                            onClick={() => deleteConversion(d, e)}
                            title="Delete entry"
                            style={{ background: 'none', border: 'none', color: C.red, cursor: 'pointer' }}
                          >
                            <FontAwesomeIcon icon={faTrash} />
                          </button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )
          )}
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

    const chartData = ins.daily_series.map(p => ({ date: p.date.slice(5), sent: p.sent, conversions: p.conversions || 0 }));
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

        {/* Conversions: tracked/manual split, quick-add, CSV upload, entries */}
        <div style={{ borderTop: `1px solid ${C.border}`, paddingTop: 14 }}>
          {renderConversions(d)}
        </div>

        {/* Pace chart: daily sent bars vs required-daily reference, with the
            conversion series (tracked + manual) on the right axis */}
        {chartData.length > 0 && (
          <div>
            <div style={{ fontSize: 12, fontWeight: 700, color: C.heading, marginBottom: 8, textTransform: 'uppercase', letterSpacing: 0.5 }}>
              Daily sent vs required pace ({fmtInt(d.progress.required_daily)}/day) &amp; conversions
            </div>
            <div style={{ height: 220, background: 'rgba(10,20,45,0.4)', borderRadius: 8, padding: '12px 8px' }}>
              <ResponsiveContainer width="100%" height="100%">
                <ComposedChart data={chartData} margin={{ top: 8, right: 16, left: 0, bottom: 0 }}>
                  <CartesianGrid strokeDasharray="3 3" stroke="rgba(99,102,241,0.15)" />
                  <XAxis dataKey="date" tick={{ fill: C.muted, fontSize: 11 }} stroke={C.border} />
                  <YAxis yAxisId="vol" tick={{ fill: C.muted, fontSize: 11 }} stroke={C.border} tickFormatter={(v: number) => fmtInt(v)} />
                  <YAxis yAxisId="conv" orientation="right" tick={{ fill: C.muted, fontSize: 11 }} stroke={C.border} allowDecimals={false} />
                  <RechartsTooltip
                    contentStyle={{ background: 'rgba(10,20,45,0.95)', border: `1px solid ${C.border}`, borderRadius: 6, color: C.heading }}
                    formatter={(v: number | string) => (typeof v === 'number' ? fmtInt(v) : v)}
                  />
                  <Legend wrapperStyle={{ fontSize: 11 }} />
                  <ReferenceLine
                    yAxisId="vol"
                    y={d.progress.required_daily}
                    stroke={C.amber}
                    strokeDasharray="6 4"
                    label={{ value: 'required/day', fill: C.amber, fontSize: 11, position: 'insideTopRight' }}
                  />
                  <Bar yAxisId="vol" dataKey="sent" name="Sent" fill={C.indigo} radius={[3, 3, 0, 0]} />
                  <Line yAxisId="conv" type="monotone" dataKey="conversions" name="Conversions" stroke="#3b82f6" strokeWidth={2} dot={false} />
                </ComposedChart>
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

        {/* Offer performance — same shared aggregation as the Offers tab */}
        <div style={{ borderTop: `1px solid ${C.border}`, paddingTop: 14 }}>
          {renderOfferPerformance(d)}
        </div>
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
                <th style={thStyle}>Volume Sent</th>
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
