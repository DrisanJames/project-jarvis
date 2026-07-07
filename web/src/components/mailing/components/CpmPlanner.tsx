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
// deal, capacity risk comes from the platform's 3-day sending trend, and
// rule-based recommendations say how to hit the objective.
//
// Screen flow (operator 2026-07-01): 1· deals define budget/eCPM/eCPA → volume
// requirements, 2· current-month pacing (MTD vs target, 3-day rate, month-end
// projection), 3· monthly history & forward planning. Performance detail
// (conversions, creatives, subjects, domains — drip included) lives in the
// expanded deal row.

const API = '/api/mailing/cpm-planner';

// Next n month keys (YYYY-MM) strictly AFTER fromYM.
const nextMonthKeys = (fromYM: string, n: number): string[] => {
  const [y, m] = fromYM.split('-').map(Number);
  if (!y || !m) return [];
  return Array.from({ length: n }, (_, i) => {
    const d = new Date(y, m - 1 + i + 1, 1);
    return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}`;
  });
};

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
  // Deadline pacing — non-null only when the deal has an end_date (see Deal).
  days_to_deadline: number | null;
  required_daily_to_deadline: number | null;
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
  end_date: string; // YYYY-MM-DD, '' when no deadline
  status: string;
  notes: string;
  conversions_override: number | null;
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

// Linked campaigns (mirror HandleListDealCampaigns / cpmDealCampaign).
interface DealCampaign {
  campaign_id: string;
  name: string;
  status: string;
  delivered: number;
  added_at: string;
}
interface VolumeToGoal {
  planned_volume: number;
  delivered_total: number;
  delivered_associated: number;
  remaining: number;
  actual_daily: number;
  days_to_goal: number;
}
interface DealCampaignList {
  campaigns: DealCampaign[];
  volume_to_goal: VolumeToGoal;
}
interface CampaignCandidate { id: string; name: string; status: string; created_at: string; }

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

// ── Attribution gap (mirror cpmAttributionGap / HandleAttributionGap) ────────
// Every delivered event in the window lands in exactly one bucket, so
// under-attribution is a visible number instead of silently-low deal volume.
interface AttributionGapCampaign {
  name: string;
  offer_key: string;
  attribution_source: string;
  delivered: number;
  has_offer: boolean;
}
interface AttributionGap {
  days: number;
  window_start: string;
  deal_delivered: number;
  offer_no_deal_delivered: number;
  unidentified_delivered: number;
  deal_campaigns: number;
  offer_no_deal_campaigns: number;
  unidentified_campaigns: number;
  top_unattributed: AttributionGapCampaign[];
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

// ─── Monthly historics & planning types (mirror cpm_planner_handlers.go) ─────
interface MonthlyTarget {
  budget: number | null;
  volume: number | null;
  ecpm: number | null;
  ecpa: number | null;
  notes: string;
}
interface MonthDeal {
  deal_id: string;
  name: string;
  offer_name: string;
  has_target: boolean;
  target: MonthlyTarget;
  delivered: number;
  revenue: number;          // CPM-billable: delivered/1000 × eCPM
  conversions: number;      // tracked + manual for THIS month (no lifetime override)
  conversions_tracked: number;
  conversions_manual: number;
  ecpm: number;             // effective eCPM used for revenue (target if set, else deal goal)
}
interface MonthPortfolio {
  delivered: number;
  revenue: number;
  conversions: number;
  target_budget: number;
  target_volume: number;
}
interface MonthRow {
  month: string;            // YYYY-MM
  is_current: boolean;
  is_future: boolean;
  portfolio: MonthPortfolio;
  deals: MonthDeal[];
}
// Per-deal editable target buffer (string-typed form inputs).
interface TargetEdit {
  budget: string;
  volume: string;
  ecpm: string;
  ecpa: string;
  notes: string;
}

// ─── Current-month pacing types (mirror HandleCurrentMonthPacing) ────────────
interface PacingDeal {
  deal_id: string;
  name: string;
  offer_name: string;
  everflow_offer_id: string;
  status: string;
  target_volume: number;
  target_budget: number;
  target_source: 'monthly' | 'deal' | 'none';
  has_monthly_target: boolean;
  monthly_budget: number | null;
  monthly_ecpm: number | null;
  monthly_ecpa: number | null;
  monthly_notes: string;
  ecpm: number;
  mtd_delivered: number;
  mtd_conversions: number;
  conversions_needed: number;
  actual_ecpm: number;
  actual_ecpa: number;
  rate_3d: number;
  required_daily: number;
  projected: number;
  projected_pct: number;
  on_pace: boolean;
}
interface PacingResp {
  month: string; // YYYY-MM
  day_of_month: number;
  days_in_month: number;
  deals: PacingDeal[];
  portfolio: {
    target_volume: number;
    mtd_delivered: number;
    mtd_conversions: number;
    projected: number;
    required_daily: number;
  };
}

// ─── Month-to-date creative/subject rows (mirror HandleAnalyticsCreatives) ───
interface CreativeRow {
  creative_key: string;
  creative_label: string;
  subject: string;
  sending_domain: string;
  from_name: string | null;
  delivered: number;
  clicks: number;
  clickers: number;
  click_rate: number;
  conversions: number;
  conv_rate: number;
  revenue: number;
  has_html: boolean; // false ⇒ drip send (no stored html)
}
interface CreativesState {
  view: 'creative' | 'subject';
  rows: CreativeRow[];
  loading: boolean;
  error: string | null;
}

// Small labelled stat used in the monthly portfolio summary.
const Stat: React.FC<{ label: string; value: string; color?: string }> = ({ label, value, color }) => (
  <div>
    <div style={{ fontSize: 10, color: C.muted, textTransform: 'uppercase', letterSpacing: 0.5 }}>{label}</div>
    <div style={{ fontSize: 16, fontWeight: 700, color: color || C.heading }}>{value}</div>
  </div>
);

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
  end_date: string;
  status: string;
  notes: string;
  conversions_override: string;
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
  end_date: '',
  status: 'active',
  notes: '',
  conversions_override: '',
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
  // Transient "recalculated" flash shown after a deal save refreshes all surfaces.
  const [recalcFlash, setRecalcFlash] = useState(false);

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
  // Linked campaigns + volume-to-goal per deal.
  const [campData, setCampData] = useState<Record<string, DealCampaignList>>({});
  const [campOpen, setCampOpen] = useState<Record<string, boolean>>({});
  const [campCandidates, setCampCandidates] = useState<Record<string, CampaignCandidate[]>>({});
  const [campQuery, setCampQuery] = useState<Record<string, string>>({});
  const [campBusy, setCampBusy] = useState(false);
  // Deal id handed off from the Offers tab ("View in CPM Planner") —
  // auto-expanded once the deal list loads.
  const [focusDealId, setFocusDealId] = useState<string | null>(
    () => sessionStorage.getItem(FOCUS_DEAL_KEY),
  );

  // Monthly historics & planning view.
  const [months, setMonths] = useState<MonthRow[]>([]);
  const [monthsLoading, setMonthsLoading] = useState(false);
  const [monthsLoaded, setMonthsLoaded] = useState(false);
  const [monthsError, setMonthsError] = useState<string | null>(null);
  const [plannerMonth, setPlannerMonth] = useState<string>(''); // future month being planned
  const [historyMonth, setHistoryMonth] = useState<string>(''); // past month being viewed
  const [curMonthYM, setCurMonthYM] = useState<string>('');     // current Denver YYYY-MM (from /months)
  const [targetEdits, setTargetEdits] = useState<Record<string, TargetEdit>>({});
  const [targetSaving, setTargetSaving] = useState<string | null>(null);
  const prevShowModal = useRef(false);

  // Current-month pacing (MTD vs target, 3-day rate, projection).
  const [pacing, setPacing] = useState<PacingResp | null>(null);
  const [pacingLoading, setPacingLoading] = useState(false);
  const [pacingError, setPacingError] = useState<string | null>(null);

  // Attribution coverage — windowed delivered scan server-side, so on demand
  // only (mount / explicit refresh), never on the 5-min timer (QA C4).
  const [attrGap, setAttrGap] = useState<AttributionGap | null>(null);
  const [attrGapLoading, setAttrGapLoading] = useState(false);
  const [attrGapError, setAttrGapError] = useState<string | null>(null);
  const [attrGapOpen, setAttrGapOpen] = useState(false);

  // Month-to-date creative/subject performance per expanded deal.
  const [creatives, setCreatives] = useState<Record<string, CreativesState>>({});

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

  // ─── Monthly historics & planning ──────────────────────────────────────────
  // Lazy: loaded ONCE on mount and on explicit refresh / after a save — never on
  // the 5-min auto-refresh (the per-deal monthly aggregates are heavier server-side).
  const loadMonths = useCallback(async () => {
    setMonthsLoading(true);
    try {
      const res = await apiFetch(`${API}/months?months=6`);
      if (!res.ok) throw new Error(`months: HTTP ${res.status}`);
      const j = await res.json();
      const rows: MonthRow[] = j.months || [];
      setMonths(rows);
      setMonthsError(null);
      setMonthsLoaded(true);
      const curYM: string = j.current_month || '';
      setCurMonthYM(curYM);
      // Planner defaults to the nearest future month; History to the most
      // recent closed month. Keep the user's pick when still valid.
      setPlannerMonth(prev => (prev && prev > curYM) ? prev : (curYM ? nextMonthKeys(curYM, 1)[0] : ''));
      // historyMonth: '' = Lifetime (the default view); only keep a chosen
      // month if it still exists in the response.
      setHistoryMonth(prev => {
        if (!prev) return '';
        const past = rows.filter(r => !r.is_current && !r.is_future);
        return past.some(r => r.month === prev) ? prev : '';
      });
    } catch (e) {
      setMonthsError(e instanceof Error ? e.message : 'failed to load months');
    } finally {
      setMonthsLoading(false);
    }
  }, []);

  useEffect(() => { loadMonths(); }, [loadMonths]); // once on mount; excluded from the 5-min timer

  // Current-month pacing — month-scoped events scan server-side, so on demand
  // only (mount / explicit refresh / after a current-month target save), never
  // on the 5-min timer (QA C4 precedent).
  const loadPacing = useCallback(async () => {
    setPacingLoading(true);
    try {
      const res = await apiFetch(`${API}/pacing`);
      if (!res.ok) throw new Error(`pacing: HTTP ${res.status}`);
      setPacing(await res.json());
      setPacingError(null);
    } catch (e) {
      setPacingError(e instanceof Error ? e.message : 'failed to load pacing');
    } finally {
      setPacingLoading(false);
    }
  }, []);

  useEffect(() => { loadPacing(); }, [loadPacing]);

  const loadAttrGap = useCallback(async () => {
    setAttrGapLoading(true);
    try {
      const res = await apiFetch(`${API}/attribution-gap?days=30`);
      if (!res.ok) throw new Error(`attribution-gap: HTTP ${res.status}`);
      setAttrGap(await res.json());
      setAttrGapError(null);
    } catch (e) {
      setAttrGapError(e instanceof Error ? e.message : 'failed to load attribution coverage');
    } finally {
      setAttrGapLoading(false);
    }
  }, []);

  useEffect(() => { loadAttrGap(); }, [loadAttrGap]);

  // Refresh months after the deal modal closes (a deal may have been created from
  // "Add deal to month") so freshly-created deals appear in the planning grid.
  useEffect(() => {
    if (prevShowModal.current && !showModal && monthsLoaded) loadMonths();
    prevShowModal.current = showModal;
  }, [showModal, monthsLoaded, loadMonths]);

  // Edit buffers are keyed by `${month}:${dealId}` so an unsaved edit on one
  // month never bleeds into another month's row or saves to the wrong month.
  const editKey = (month: string, dealId: string) => `${month}:${dealId}`;

  // Re-sum a month row's portfolio from its (already-patched) deal rows —
  // pure local math, mirrors HandleMonths' roll-up.
  const resumPortfolio = (row: MonthRow): MonthRow => {
    const pf: MonthPortfolio = { delivered: 0, revenue: 0, conversions: 0, target_budget: 0, target_volume: 0 };
    for (const d of row.deals) {
      pf.delivered += d.delivered;
      pf.revenue += d.revenue;
      pf.conversions += d.conversions;
      if (d.has_target) {
        if (d.target.budget != null) pf.target_budget += d.target.budget;
        if (d.target.volume != null) pf.target_volume += d.target.volume;
      }
    }
    return { ...row, portfolio: pf };
  };

  // Client mirror of the server's cpmPacingMath — lets a pacing-row save
  // reflect instantly instead of waiting on the /pacing re-fetch.
  const clientPacingMath = (target: number, mtd: number, rate3d: number, day: number, dim: number) => {
    const daysIncl = Math.max(1, dim - day + 1);
    const required = target > mtd ? (target - mtd) / daysIncl : 0;
    const after = Math.max(0, dim - day);
    const projected = mtd + Math.round(rate3d * after);
    const pct = target > 0 ? projected / target : 0;
    return { required, projected, pct, onPace: target > 0 && projected >= target };
  };

  // Optimistically patch one pacing row after a current-month target
  // save/delete (null target fields = deleted). loadPacing() reconciles after.
  const patchPacingDeal = (dealId: string, tgt: { budget: number | null; ecpm: number | null; ecpa: number | null; notes: string } | null) => {
    setPacing(prev => {
      if (!prev) return prev;
      const dealMeta = deals.find(x => x.id === dealId);
      const nextDeals = prev.deals.map(p => {
        if (p.deal_id !== dealId) return p;
        let np: PacingDeal;
        if (tgt) {
          // Merge like the server's COALESCE: null field = keep stored value.
          const budget = tgt.budget != null ? tgt.budget : p.monthly_budget;
          const ecpm = tgt.ecpm != null ? tgt.ecpm : p.monthly_ecpm;
          const ecpa = tgt.ecpa != null ? tgt.ecpa : p.monthly_ecpa;
          const effEcpm = ecpm != null && ecpm > 0 ? ecpm : (dealMeta?.ecpm_goal || p.ecpm);
          const effEcpa = ecpa != null && ecpa > 0 ? ecpa : (dealMeta?.ecpa_goal || 0);
          const targetVolume = budget != null && budget > 0 && effEcpm > 0 ? Math.ceil((budget / effEcpm) * 1000) : p.target_volume;
          np = {
            ...p, has_monthly_target: true, monthly_budget: budget, monthly_ecpm: ecpm, monthly_ecpa: ecpa,
            monthly_notes: tgt.notes, ecpm: effEcpm,
            target_budget: budget != null ? budget : p.target_budget,
            target_volume: targetVolume, target_source: 'monthly',
            conversions_needed: (() => {
              const eb = budget != null && budget > 0 ? budget : p.target_budget; // deal-plan fallback budget
              return eb > 0 && effEcpa > 0 ? Math.ceil(eb / effEcpa) : p.conversions_needed;
            })(),
            actual_ecpa: (() => {
              const eb = budget != null && budget > 0 ? budget : p.target_budget;
              return eb > 0 && p.mtd_conversions > 0 ? eb / p.mtd_conversions : p.actual_ecpa;
            })(),
          };
        } else {
          // Deleted: fall back to the deal's own plan (the server's rule).
          const planned = dealMeta && dealMeta.status === 'active' ? dealMeta.planned_volume : 0;
          np = {
            ...p, has_monthly_target: false, monthly_budget: null, monthly_ecpm: null, monthly_ecpa: null,
            monthly_notes: '', ecpm: dealMeta?.ecpm_goal || p.ecpm,
            target_budget: planned > 0 ? (dealMeta?.budget || 0) : 0,
            target_volume: planned, target_source: planned > 0 ? 'deal' : 'none',
          };
        }
        const m = clientPacingMath(np.target_volume, np.mtd_delivered, np.rate_3d, prev.day_of_month, prev.days_in_month);
        return { ...np, required_daily: m.required, projected: m.projected, projected_pct: m.pct, on_pace: m.onPace };
      }).filter(p => p.deal_id !== dealId || p.target_volume > 0 || p.mtd_delivered > 0 || p.mtd_conversions > 0);
      const portfolio = nextDeals.reduce((acc, p) => ({
        target_volume: acc.target_volume + p.target_volume,
        mtd_delivered: acc.mtd_delivered + p.mtd_delivered,
        mtd_conversions: acc.mtd_conversions + p.mtd_conversions,
        projected: acc.projected + p.projected,
        required_daily: acc.required_daily + p.required_daily,
      }), { target_volume: 0, mtd_delivered: 0, mtd_conversions: 0, projected: 0, required_daily: 0 });
      return { ...prev, deals: nextDeals, portfolio };
    });
  };

  // Saving/deleting a TARGET never changes the month's ACTUALS, so instead of
  // blocking on the heavy /months re-fetch (the old "save takes forever, then
  // nothing changed" symptom), patch the grid locally with the exact semantics
  // the server applied and return immediately. The Refresh button still exists
  // to reconcile actuals.
  const patchMonthDeal = (month: string, dealId: string, apply: (md: MonthDeal) => MonthDeal | null) => {
    setMonths(prev => prev.map(row => {
      if (row.month !== month) return row;
      let found = false;
      const nextDeals: MonthDeal[] = [];
      for (const md of row.deals) {
        if (md.deal_id !== dealId) { nextDeals.push(md); continue; }
        found = true;
        const patched = apply(md);
        if (patched) nextDeals.push(patched); // null ⇒ row no longer belongs in the month
      }
      if (!found) {
        // Deal had no target/activity in this month yet (a synthesized editable
        // row) — materialize it the way the server would.
        const deal = deals.find(x => x.id === dealId);
        const fresh = apply({
          deal_id: dealId, name: deal?.name || '', offer_name: deal?.offer_name || '',
          has_target: false, target: { budget: null, volume: null, ecpm: null, ecpa: null, notes: '' },
          delivered: 0, revenue: 0, conversions: 0, conversions_tracked: 0, conversions_manual: 0,
          ecpm: deal?.ecpm_goal || 0,
        });
        if (fresh) nextDeals.push(fresh);
      }
      return resumPortfolio({ ...row, deals: nextDeals });
    }));
  };

  const saveTarget = async (dealId: string, month: string) => {
    if (!month) return;
    const key = editKey(month, dealId);
    const e = targetEdits[key];
    if (!e) return;
    setTargetSaving(dealId);
    try {
      // Parse defensively: a blank field is "leave unchanged"; a non-numeric
      // entry (commas, "$", typos) is rejected rather than silently coerced to a
      // wrong number or NaN→null (which would corrupt or clear the target).
      const num = (s: string): number | undefined => {
        if (s.trim() === '') return undefined;
        const v = parseFloat(s);
        return Number.isFinite(v) ? v : undefined;
      };
      const body: Record<string, unknown> = { notes: e.notes || '' };
      const b = num(e.budget); if (b !== undefined) body.budget = b;
      const m = num(e.ecpm); if (m !== undefined) body.ecpm = m;
      const a = num(e.ecpa); if (a !== undefined) body.ecpa = a;
      // Volume is NOT operator-entered — it is derived from budget ÷ eCPM × 1000
      // (the planned-volume formula). Send the computed value whenever both inputs
      // exist so the saved monthly target's volume always matches the math.
      let vol: number | undefined;
      if (b !== undefined && m !== undefined && m > 0) {
        vol = Math.ceil((b / m) * 1000);
        body.volume = vol;
      }
      const res = await apiFetch(`${API}/deals/${dealId}/monthly/${month}`, {
        method: 'PUT', body: JSON.stringify(body),
      });
      if (!res.ok) {
        const j = await res.json().catch(() => ({}));
        throw new Error(j.error || `HTTP ${res.status}`);
      }
      setTargetEdits(prev => { const n = { ...prev }; delete n[key]; return n; });
      // Mirror the server's COALESCE partial-merge locally: an omitted field
      // keeps its stored value; notes always replaces.
      patchMonthDeal(month, dealId, md => {
        const target: MonthlyTarget = {
          budget: b !== undefined ? b : md.target.budget,
          ecpm: m !== undefined ? m : md.target.ecpm,
          ecpa: a !== undefined ? a : md.target.ecpa,
          volume: vol !== undefined ? vol : md.target.volume,
          notes: (body.notes as string) || '',
        };
        const dealMeta = deals.find(x => x.id === dealId);
        const effEcpm = target.ecpm != null && target.ecpm > 0 ? target.ecpm : (dealMeta?.ecpm_goal || md.ecpm);
        return {
          ...md, has_target: true, target, ecpm: effEcpm,
          revenue: md.delivered > 0 && effEcpm > 0 ? (md.delivered / 1000) * effEcpm : 0,
        };
      });
      setMonthsError(null);
      // A current-month save reflects instantly (optimistic pacing patch);
      // the background /pacing re-fetch reconciles the derived numbers.
      if (pacing && month === pacing.month) {
        patchPacingDeal(dealId, {
          budget: b !== undefined ? b : null,
          ecpm: m !== undefined ? m : null,
          ecpa: a !== undefined ? a : null,
          notes: (body.notes as string) || '',
        });
        void loadPacing();
      }
    } catch (err) {
      setMonthsError(err instanceof Error ? err.message : 'save failed');
    } finally {
      setTargetSaving(null);
    }
  };

  // Delete a planned monthly target/budget for a deal in the selected month.
  const deleteTarget = async (dealId: string, month: string) => {
    if (!month) return;
    setTargetSaving(dealId);
    try {
      const res = await apiFetch(`${API}/deals/${dealId}/monthly/${month}`, { method: 'DELETE' });
      if (!res.ok && res.status !== 404) {
        const j = await res.json().catch(() => ({}));
        throw new Error(j.error || `HTTP ${res.status}`);
      }
      setTargetEdits(prev => { const n = { ...prev }; delete n[editKey(month, dealId)]; return n; });
      // Local patch: clear the target; a row with no target and no activity
      // leaves the month entirely (the server's inclusion rule).
      patchMonthDeal(month, dealId, md => {
        if (md.delivered === 0 && md.conversions === 0) return null;
        const dealMeta = deals.find(x => x.id === dealId);
        const effEcpm = dealMeta?.ecpm_goal || md.ecpm;
        return {
          ...md, has_target: false,
          target: { budget: null, volume: null, ecpm: null, ecpa: null, notes: '' },
          ecpm: effEcpm,
          revenue: md.delivered > 0 && effEcpm > 0 ? (md.delivered / 1000) * effEcpm : 0,
        };
      });
      setMonthsError(null);
      if (pacing && month === pacing.month) {
        patchPacingDeal(dealId, null); // deleted — fall back to deal plan
        void loadPacing();
      }
    } catch (err) {
      setMonthsError(err instanceof Error ? err.message : 'delete failed');
    } finally {
      setTargetSaving(null);
    }
  };

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
      end_date: d.end_date || '',
      status: d.status,
      notes: d.notes,
      conversions_override: d.conversions_override != null ? String(d.conversions_override) : '',
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
        // '' clears the deadline server-side; a date sets the "finish sooner" lever.
        end_date: form.end_date.trim(),
        notes: form.notes,
      };
      if (editingId) {
        body.status = form.status;
        // >0 pins the count; 0 (blank) clears the override → computed count.
        body.conversions_override = form.conversions_override.trim()
          ? parseInt(form.conversions_override, 10) || 0
          : 0;
      }
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
      // A deal edit changes budget/eCPM → planned volume, which every dependent
      // surface reads. Refresh them ALL (same loaders the Refresh buttons use),
      // not just the deal table — otherwise the Pacing board + Planner grid show
      // stale volume until a manual Refresh (the static-volume bug, 2026-07-02).
      await Promise.all([loadAll(), loadPacing(), loadMonths()]);
      // Also refresh EVERY dependent surface for the currently-expanded deal —
      // loadAll/loadPacing/loadMonths don't touch the open detail panel, which
      // reads budget-derived numbers. setInsights({})/setOfferPerf({}) above
      // cleared its cached insights → renderInsights early-returns to
      // "No insights available" until refetched (B5); campData holds the
      // server-derived planned volume-to-goal, stale until reloaded (B6).
      if (expandedId) {
        const hadCamps = !!campData[expandedId];
        await Promise.all([
          expandDeal(expandedId, false, false), // refetch insights + offer-performance (cache was cleared)
          hadCamps ? loadCampaigns(expandedId) : Promise.resolve(), // reload volume-to-goal for the new budget
        ]);
      }
      // Tiny affordance so the operator sees the numbers were recomputed.
      setRecalcFlash(true);
      window.setTimeout(() => setRecalcFlash(false), 2500);
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

  // ── Linked campaigns ──────────────────────────────────────────────
  const loadCampaigns = useCallback(async (dealId: string) => {
    try {
      const res = await apiFetch(`${API}/deals/${dealId}/campaigns`);
      if (res.ok) { const j = await res.json(); setCampData(prev => ({ ...prev, [dealId]: j })); }
    } catch { /* non-fatal */ }
  }, []);

  const searchCandidates = useCallback(async (dealId: string, q: string) => {
    try {
      const res = await apiFetch(`${API}/deals/${dealId}/campaign-candidates?q=${encodeURIComponent(q)}&limit=50`);
      if (res.ok) {
        const j = await res.json();
        setCampCandidates(prev => ({ ...prev, [dealId]: j.candidates || [] }));
      }
    } catch { /* non-fatal */ }
  }, []);

  const associateCampaign = async (dealId: string, campaignID: string) => {
    setCampBusy(true);
    try {
      await apiFetch(`${API}/deals/${dealId}/campaigns`, {
        method: 'POST', body: JSON.stringify({ campaign_ids: [campaignID] }),
      });
      await Promise.all([loadCampaigns(dealId), searchCandidates(dealId, campQuery[dealId] || ''), loadAll()]);
    } finally { setCampBusy(false); }
  };

  const removeCampaign = async (dealId: string, campaignID: string) => {
    setCampBusy(true);
    try {
      await apiFetch(`${API}/deals/${dealId}/campaigns/${campaignID}`, { method: 'DELETE' });
      await Promise.all([loadCampaigns(dealId), loadAll()]);
    } finally { setCampBusy(false); }
  };

  // Month-to-date creative/subject breakdown for one deal — reuses the
  // Analytics Creatives aggregation (drip sends included; has_html=false rows
  // ARE the drip touches). Offer resolved by the deal's everflow id.
  const loadCreatives = useCallback(async (deal: Deal, view: 'creative' | 'subject') => {
    const offer = deal.everflow_offer_id || deal.offer_name;
    if (!offer) return;
    setCreatives(prev => ({ ...prev, [deal.id]: { view, rows: prev[deal.id]?.rows || [], loading: true, error: null } }));
    try {
      // Browser-LOCAL month start (not toISOString/UTC — on a Denver evening at
      // month boundary UTC is already next month, which would request 0 rows).
      const now = new Date();
      const monthStart = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, '0')}-01`;
      const res = await apiFetch(
        `/api/mailing/analytics/creatives?offer=${encodeURIComponent(offer)}&view=${view}&start_date=${monthStart}`,
      );
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const j = await res.json();
      setCreatives(prev => ({ ...prev, [deal.id]: { view, rows: j.rows || [], loading: false, error: null } }));
    } catch (e) {
      setCreatives(prev => ({
        ...prev,
        [deal.id]: { view, rows: [], loading: false, error: e instanceof Error ? e.message : 'failed to load' },
      }));
    }
  }, []);

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
    if (!creatives[d.id]) void loadCreatives(d, 'creative');
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
          <div style={{ fontSize: 11, color: C.muted, textTransform: 'uppercase', letterSpacing: 0.5 }}>Platform daily (3d avg)</div>
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

  // 0.5 · Attribution coverage — is deal volume complete? Every delivered
  // event in the last 30d is bucketed: captured by a deal / offer-identified
  // but no deal claims it / no offer identity. The two non-deal buckets are
  // the volume the operator felt was missing (2026-07-07); the backfill +
  // deploy-time stamp drive them toward zero.
  const renderAttributionCoverage = () => {
    const g = attrGap;
    const total = g ? g.deal_delivered + g.offer_no_deal_delivered + g.unidentified_delivered : 0;
    const coverage = g && total > 0 ? (g.deal_delivered / total) * 100 : null;
    const covColor = coverage == null ? C.muted : coverage >= 90 ? C.green : coverage >= 70 ? C.amber : C.red;
    return (
      <div style={{ background: C.panel, border: `1px solid ${C.border}`, borderRadius: 10, padding: '12px 18px', marginBottom: 18 }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: 10 }}>
          <div style={{ display: 'flex', gap: 24, alignItems: 'center', flexWrap: 'wrap' }}>
            <div>
              <div style={{ fontSize: 11, color: C.muted, textTransform: 'uppercase', letterSpacing: 0.5 }}>
                Deal-attribution coverage (30d delivered)
              </div>
              <div style={{ fontSize: 18, fontWeight: 700, color: covColor }}>
                {coverage != null ? `${coverage.toFixed(1)}%` : '—'}
              </div>
            </div>
            {g && (
              <>
                <Stat label="In a deal" value={fmtInt(g.deal_delivered)} color={C.green} />
                <Stat label={`Offer-identified, no deal (${g.offer_no_deal_campaigns} camps)`} value={fmtInt(g.offer_no_deal_delivered)} color={g.offer_no_deal_delivered > 0 ? C.amber : C.muted} />
                <Stat label={`No offer identity (${g.unidentified_campaigns} camps)`} value={fmtInt(g.unidentified_delivered)} color={g.unidentified_delivered > 0 ? C.red : C.muted} />
              </>
            )}
          </div>
          <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
            {g && g.top_unattributed.length > 0 && (
              <button onClick={() => setAttrGapOpen(o => !o)}
                style={{ padding: '6px 10px', borderRadius: 8, border: `1px solid ${C.border}`, background: 'transparent', color: C.muted, fontSize: 12, cursor: 'pointer' }}>
                <FontAwesomeIcon icon={attrGapOpen ? faChevronDown : faChevronRight} style={{ fontSize: 10, marginRight: 6 }} />
                Top unattributed
              </button>
            )}
            <button onClick={loadAttrGap} disabled={attrGapLoading}
              style={{ padding: '6px 10px', borderRadius: 8, border: `1px solid ${C.border}`, background: 'transparent', color: C.muted, fontSize: 12, cursor: 'pointer' }}>
              {attrGapLoading ? <FontAwesomeIcon icon={faSpinner} spin /> : 'Refresh'}
            </button>
          </div>
        </div>
        {attrGapError && <div style={{ color: C.red, fontSize: 12, marginTop: 8 }}>{attrGapError}</div>}
        {attrGapOpen && g && g.top_unattributed.length > 0 && (
          <div style={{ marginTop: 10, overflowX: 'auto', border: `1px solid ${C.border}`, borderRadius: 8, background: 'rgba(10,20,45,0.4)' }}>
            <table style={{ width: '100%', borderCollapse: 'collapse' }}>
              <thead><tr>
                <th style={thStyle}>Campaign (not counted by any deal)</th>
                <th style={thStyle}>Delivered (30d)</th>
                <th style={thStyle}>Offer Key</th>
                <th style={thStyle}>Attribution</th>
              </tr></thead>
              <tbody>
                {g.top_unattributed.map((c, i) => (
                  <tr key={`${c.name}:${i}`}>
                    <td style={{ ...tdStyle, maxWidth: 380, overflow: 'hidden', textOverflow: 'ellipsis' }} title={c.name}>{c.name}</td>
                    <td style={tdStyle}>{fmtInt(c.delivered)}</td>
                    <td style={tdStyle}>{c.offer_key || <span style={{ color: C.muted }}>—</span>}</td>
                    <td style={tdStyle}>
                      <span style={{ fontSize: 11, color: c.has_offer ? C.amber : C.red }}>
                        {c.attribution_source || 'none'}
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    );
  };

  // 1 · Current-month pacing — the month IN MOTION (operator 2026-07-01):
  // this is the primary, EDITABLE surface. Each row carries the month's
  // budget/eCPM/eCPA inputs (save/delete hits the same monthly-target API the
  // planner uses) plus the month-scoped strong metrics: MTD delivered vs
  // target, required/day over the days left, 3-day rate, projection,
  // conversions vs needed, actual eCPM/eCPA, status.
  const renderPacing = () => {
    const monthName = pacing ? monthLabel(pacing.month) : '';
    const daysLeft = pacing ? pacing.days_in_month - pacing.day_of_month + 1 : 0;
    const numInput: React.CSSProperties = {
      width: 84, padding: '5px 7px', background: 'rgba(10,20,45,0.7)',
      border: `1px solid ${C.border}`, borderRadius: 6, color: C.heading, fontSize: 12, boxSizing: 'border-box',
    };

    const pacingRow = (p: PacingDeal) => {
      const month = pacing!.month;
      const key = editKey(month, p.deal_id);
      const buf: TargetEdit = targetEdits[key] || {
        budget: p.monthly_budget != null ? String(p.monthly_budget) : '',
        volume: '',
        ecpm: p.monthly_ecpm != null ? String(p.monthly_ecpm) : '',
        ecpa: p.monthly_ecpa != null ? String(p.monthly_ecpa) : '',
        notes: p.monthly_notes || '',
      };
      const set = (k: keyof TargetEdit, v: string) =>
        setTargetEdits(prev => ({ ...prev, [key]: { ...(prev[key] || buf), [k]: v } }));
      const dirty = !!targetEdits[key];
      const busy = targetSaving === p.deal_id;
      const pct = p.target_volume > 0 ? Math.min((p.mtd_delivered / p.target_volume) * 100, 100) : 0;
      // Live-derived month target while typing; falls back to the server's number.
      const bv = parseFloat(buf.budget); const mv = parseFloat(buf.ecpm);
      const liveTarget = bv > 0 && mv > 0 ? Math.ceil((bv / mv) * 1000) : 0;
      const dealMeta = deals.find(x => x.id === p.deal_id);
      const ecpaGoal = p.monthly_ecpa != null && p.monthly_ecpa > 0 ? p.monthly_ecpa : (dealMeta?.ecpa_goal || 0);
      // Conv-needed live-derives as the operator types (budget or eCPA):
      // ⌈effective budget ÷ effective eCPA⌉, falling back to the saved value.
      const av = parseFloat(buf.ecpa);
      const effBudget = bv > 0 ? bv : p.target_budget;
      const effEcpa = av > 0 ? av : ecpaGoal;
      const liveConvNeeded = effBudget > 0 && effEcpa > 0 ? Math.ceil(effBudget / effEcpa) : p.conversions_needed;
      const convColor = liveConvNeeded > 0 && p.mtd_conversions >= liveConvNeeded
        ? C.green : p.mtd_conversions > 0 ? C.amber : C.muted;
      return (
        <tr key={p.deal_id}>
          <td style={tdStyle}>
            <div style={{ color: C.heading }}>{p.name}</div>
            {p.offer_name && <div style={{ fontSize: 11, color: C.muted }}>{p.offer_name}</div>}
          </td>
          <td style={tdStyle}><input type="number" style={numInput} value={buf.budget} onChange={e => set('budget', e.target.value)} placeholder="$" /></td>
          <td style={tdStyle}><input type="number" step="0.01" style={numInput} value={buf.ecpm} onChange={e => set('ecpm', e.target.value)} placeholder="eCPM" /></td>
          <td style={tdStyle}><input type="number" step="0.01" style={numInput} value={buf.ecpa} onChange={e => set('ecpa', e.target.value)} placeholder="eCPA" /></td>
          <td style={tdStyle}>
            <span style={{ color: C.heading }} title="budget ÷ eCPM × 1,000">
              {liveTarget > 0 ? fmtInt(liveTarget) : p.target_volume > 0 ? fmtInt(p.target_volume) : '—'}
            </span>
            {liveTarget === 0 && p.target_source === 'deal' && (
              <span style={{ fontSize: 10, color: C.amber }} title="No monthly budget saved — pacing against the deal's lifetime plan. Enter a budget + eCPM and Save to pin this month."> deal plan</span>
            )}
          </td>
          <td style={tdStyle}>
            <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
              <button onClick={() => saveTarget(p.deal_id, month)} disabled={!dirty || busy}
                style={{
                  padding: '5px 12px', borderRadius: 6, border: 'none', fontSize: 12, fontWeight: 600,
                  cursor: dirty ? 'pointer' : 'default',
                  background: dirty ? C.indigo : 'rgba(99,102,241,0.2)', color: '#fff', opacity: dirty ? 1 : 0.5,
                }}>
                {busy ? <FontAwesomeIcon icon={faSpinner} spin /> : 'Save'}
              </button>
              {p.has_monthly_target && (
                <button onClick={() => deleteTarget(p.deal_id, month)} disabled={busy}
                  title="Delete this month's target"
                  style={{ padding: '5px 9px', borderRadius: 6, border: `1px solid ${C.border}`, fontSize: 12, cursor: 'pointer', background: 'transparent', color: C.red }}>
                  <FontAwesomeIcon icon={faTrash} />
                </button>
              )}
            </div>
          </td>
          <td style={tdStyle}>
            <div style={{ minWidth: 130 }}>
              {p.target_volume > 0 && (
                <div style={{ height: 7, background: 'rgba(10,20,45,0.8)', borderRadius: 4, overflow: 'hidden', marginBottom: 3 }}>
                  <div style={{ width: `${pct}%`, height: '100%', borderRadius: 4, background: p.on_pace ? C.green : C.amber }} />
                </div>
              )}
              <span style={{ color: C.heading }}>{fmtInt(p.mtd_delivered)}</span>
              {p.target_volume > 0 && <span style={{ fontSize: 11, color: C.muted }}> ({pct.toFixed(0)}%)</span>}
            </div>
          </td>
          <td style={tdStyle}>{p.required_daily > 0 ? fmtInt(p.required_daily) : '—'}</td>
          <td style={tdStyle}>{fmtInt(p.rate_3d)}/day</td>
          <td style={tdStyle}>
            <span style={{ color: p.target_volume > 0 ? (p.on_pace ? C.green : C.amber) : C.muted, fontWeight: 600 }}>
              {fmtInt(p.projected)}
            </span>
            {p.target_volume > 0 && (
              <span style={{
                marginLeft: 8, padding: '2px 8px', borderRadius: 999, fontSize: 10, fontWeight: 700,
                color: p.on_pace ? C.green : C.amber, border: `1px solid ${p.on_pace ? C.green : C.amber}`,
              }}>
                {p.on_pace ? 'ON PACE' : `${Math.round(p.projected_pct * 100)}%`}
              </span>
            )}
          </td>
          <td style={tdStyle}>
            <span style={{ color: convColor, fontWeight: 600 }}>{fmtInt(p.mtd_conversions)}</span>
            <span style={{ color: C.muted }}> / {liveConvNeeded > 0 ? fmtInt(liveConvNeeded) : '—'}</span>
          </td>
          <td style={tdStyle}>
            {p.mtd_delivered > 0 && p.actual_ecpm > 0 ? (
              <>
                <span style={{ color: p.actual_ecpm >= p.ecpm ? C.green : C.red, fontWeight: 600 }}>{fmtMoney(p.actual_ecpm)}</span>
                <span style={{ color: C.muted, fontSize: 11 }}> vs {fmtMoney(p.ecpm)}</span>
              </>
            ) : <span style={{ color: C.muted }}>—</span>}
          </td>
          <td style={tdStyle}>
            {p.actual_ecpa > 0 ? (
              <span style={{ color: ecpaGoal > 0 && p.actual_ecpa <= ecpaGoal ? C.green : C.amber }}>{fmtMoney(p.actual_ecpa)}</span>
            ) : <span style={{ color: C.muted }}>—</span>}
          </td>
          <td style={tdStyle}>
            <span style={{
              padding: '3px 10px', borderRadius: 999, fontSize: 11, fontWeight: 700,
              color: statusColor(p.status), border: `1px solid ${statusColor(p.status)}`,
            }}>
              {p.status.toUpperCase()}
            </span>
          </td>
        </tr>
      );
    };

    return (
      <div style={{ background: C.panel, border: `1px solid ${C.border}`, borderRadius: 12, padding: 18, marginBottom: 18 }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: 12, marginBottom: 6 }}>
          <div>
            <div style={{ fontSize: 12, fontWeight: 700, letterSpacing: 0.6, textTransform: 'uppercase', color: C.heading }}>
              1 · Current-Month Pacing{pacing ? ` — ${monthName}` : ''}
            </div>
            <div style={{ fontSize: 12, color: C.muted, marginTop: 3 }}>
              {pacing
                ? `Day ${pacing.day_of_month} of ${pacing.days_in_month} · edit this month's budget/eCPM/eCPA per deal; Required/Day spreads the remaining target over the ${daysLeft} days left (incl. today); Projected = MTD + 3-day rate to month-end.`
                : 'MTD delivered vs target, at the last-3-day send rate.'}
            </div>
          </div>
          <button onClick={loadPacing} disabled={pacingLoading}
            style={{ padding: '6px 10px', borderRadius: 8, border: `1px solid ${C.border}`, background: 'transparent', color: C.muted, fontSize: 12, cursor: 'pointer' }}>
            {pacingLoading ? <FontAwesomeIcon icon={faSpinner} spin /> : 'Refresh'}
          </button>
        </div>

        {pacingError && <div style={{ color: C.red, fontSize: 12, marginBottom: 10 }}>{pacingError}</div>}
        {monthsError && <div style={{ color: C.red, fontSize: 12, marginBottom: 10 }}>{monthsError}</div>}

        {!pacing ? (
          <div style={{ padding: 24, textAlign: 'center', color: C.muted }}>
            {pacingLoading ? <FontAwesomeIcon icon={faSpinner} spin /> : 'No pacing data.'}
          </div>
        ) : pacing.deals.length === 0 ? (
          <div style={{ padding: 16, textAlign: 'center', color: C.muted, fontSize: 13 }}>
            Nothing pacing this month yet — create a deal, then set its monthly budget here.
          </div>
        ) : (
          <>
            <div style={{ display: 'flex', gap: 24, flexWrap: 'wrap', marginBottom: 12, padding: '10px 14px', background: 'rgba(10,20,45,0.5)', borderRadius: 8 }}>
              <Stat label="Month target" value={pacing.portfolio.target_volume > 0 ? fmtInt(pacing.portfolio.target_volume) : '—'} />
              <Stat label="MTD delivered" value={fmtInt(pacing.portfolio.mtd_delivered)} />
              <Stat label={`Required / day (${daysLeft}d left)`} value={fmtInt(pacing.portfolio.required_daily)} />
              <Stat
                label="Projected month-end"
                value={fmtInt(pacing.portfolio.projected)}
                color={pacing.portfolio.target_volume > 0 && pacing.portfolio.projected >= pacing.portfolio.target_volume ? C.green : C.amber}
              />
              <Stat label="MTD conversions" value={fmtInt(pacing.portfolio.mtd_conversions)} />
            </div>
            <div style={{ overflowX: 'auto' }}>
              <table style={{ width: '100%', borderCollapse: 'collapse' }}>
                <thead><tr>
                  <th style={thStyle}>Deal</th>
                  <th style={thStyle}>Budget</th>
                  <th style={thStyle}>eCPM Goal</th>
                  <th style={thStyle}>eCPA Goal</th>
                  <th style={thStyle}>Month Target</th>
                  <th style={thStyle}></th>
                  <th style={thStyle}>MTD Delivered</th>
                  <th style={thStyle} title={`Remaining target ÷ ${daysLeft} days left (incl. today)`}>Req / Day ({daysLeft}d left)</th>
                  <th style={thStyle}>3-Day Rate</th>
                  <th style={thStyle}>Projected</th>
                  <th style={thStyle}>Conv</th>
                  <th style={thStyle}>Actual eCPM</th>
                  <th style={thStyle}>eCPA Actual</th>
                  <th style={thStyle}>Status</th>
                </tr></thead>
                <tbody>{pacing.deals.map(pacingRow)}</tbody>
              </table>
            </div>
          </>
        )}
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
          {fmtInt(d.progress.delivered)} / {fmtInt(d.planned_volume)} ({(d.progress.pct_volume_delivered * 100).toFixed(1)}%)
        </div>
        {d.end_date && d.progress.required_daily_to_deadline != null && (
          <div style={{ fontSize: 10, color: C.amber, marginTop: 2 }}
            title={`Deliver the remaining volume by ${d.end_date}`}>
            ⏱ {d.end_date} · {d.progress.days_to_deadline}d left · need {fmtInt(d.progress.required_daily_to_deadline)}/day
          </div>
        )}
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
      { label: 'Human Opens', value: fmtInt(t.opened), sub: 'Apple Mail privacy opens excluded', color: C.green },
      { label: 'Human Clicks', value: fmtInt(t.clicked), sub: 'automated/bot clicks excluded', color: C.amber },
      // Bounce split is ALWAYS hard vs soft — never a combined number.
      { label: 'Hard Bounces', value: fmtInt(t.hard_bounces), sub: 'provider deliverability risk', color: C.red },
      { label: 'Soft Bounces', value: fmtInt(t.soft_bounces), sub: 'usually transient', color: C.amber },
      { label: 'Conversions', value: fmtInt(t.conversions), sub: `conv / ${p.days}d`, color: '#3b82f6' },
      {
        label: 'Suppressed', value: fmtInt(t.suppression_total),
        sub: p.dnm_list_size > 0 ? `Suppression list ${fmtInt(p.dnm_list_size)}` : 'all time, all reasons',
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
            Deal ceiling: {suppressedShare.toFixed(1)}% of the last cleaned audience ({fmtInt(p.audience_size)}) is already on the suppression list for this offer.
          </div>
        ) : (
          <div style={{ fontSize: 11, color: C.muted }}>
            Suppressed share vs audience not shown — no completed list cleaning provides an audience size for this offer.
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
          {chip('Tracked (offer tracking)', p.conversions_tracked, C.indigo)}
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
          <button onClick={() => csvInputRef.current?.click()} disabled={convBusy} style={smallBtn} title="Offer-tracking conversions export — duplicates removed automatically, re-upload safe">
            <FontAwesomeIcon icon={faUpload} /> Upload offer-tracking CSV
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
                No manual conversions yet — quick-add a count or upload an offer-tracking CSV.
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
                      <th style={thStyle}>Tracking tags</th>
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

  // ── Linked campaigns & volume-to-goal panel ──────────────────────
  const renderCampaigns = (d: Deal) => {
    const cd = campData[d.id];
    const open = !!campOpen[d.id];
    const cands = campCandidates[d.id] || [];
    const v = cd?.volume_to_goal;
    return (
      <div style={{ padding: '12px 20px', borderTop: `1px solid ${C.border}` }}>
        <button
          onClick={() => {
            const next = !open;
            setCampOpen(prev => ({ ...prev, [d.id]: next }));
            if (next && !cd) { loadCampaigns(d.id); searchCandidates(d.id, ''); }
          }}
          style={{ background: 'none', border: 'none', color: C.muted, cursor: 'pointer', fontSize: 12,
            fontWeight: 700, padding: 0, display: 'flex', alignItems: 'center', gap: 6,
            textTransform: 'uppercase', letterSpacing: 0.5 }}
        >
          <FontAwesomeIcon icon={open ? faChevronDown : faChevronRight} style={{ fontSize: 10 }} />
          Linked campaigns &amp; volume to goal
          {cd ? ` — ${cd.campaigns.length} linked` : ''}
        </button>
        {open && (
          <div style={{ marginTop: 10, display: 'flex', flexDirection: 'column', gap: 12 }}>
            {/* Volume-to-goal rollup */}
            {v && (
              <div style={{ display: 'flex', gap: 18, flexWrap: 'wrap', background: 'rgba(10,20,45,0.4)',
                border: `1px solid ${C.border}`, borderRadius: 8, padding: '10px 14px' }}>
                {[
                  ['Planned volume', fmtInt(v.planned_volume)],
                  ['Delivered (all)', fmtInt(v.delivered_total)],
                  ['Remaining to goal', fmtInt(v.remaining)],
                  ['Current daily', fmtInt(Math.round(v.actual_daily))],
                  ['Days to goal', v.actual_daily > 0 ? `${Math.ceil(v.days_to_goal)}d` : '∞'],
                ].map(([k, val]) => (
                  <div key={k}>
                    <div style={{ fontSize: 11, color: C.muted, textTransform: 'uppercase', letterSpacing: 0.5 }}>{k}</div>
                    <div style={{ fontSize: 16, fontWeight: 700, color: C.heading }}>{val}</div>
                  </div>
                ))}
              </div>
            )}
            {/* Associate picker */}
            <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
              <input
                placeholder="Search campaigns to link…"
                value={campQuery[d.id] || ''}
                onChange={e => {
                  const q = e.target.value;
                  setCampQuery(prev => ({ ...prev, [d.id]: q }));
                  searchCandidates(d.id, q);
                }}
                style={{ flex: 1, padding: '6px 10px', background: 'rgba(10,20,45,0.6)',
                  border: `1px solid ${C.border}`, borderRadius: 6, color: C.heading, fontSize: 13 }}
              />
            </div>
            {cands.length > 0 && (
              <div style={{ maxHeight: 160, overflowY: 'auto', border: `1px solid ${C.border}`, borderRadius: 6 }}>
                {cands.map(c => (
                  <div key={c.id} style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center',
                    padding: '5px 10px', borderBottom: `1px solid ${C.border}`, fontSize: 12 }}>
                    <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', color: C.heading }} title={c.name}>{c.name}</span>
                    <button disabled={campBusy} onClick={() => associateCampaign(d.id, c.id)}
                      style={{ background: C.indigo, border: 'none', color: '#fff', borderRadius: 5,
                        padding: '3px 10px', cursor: 'pointer', fontSize: 11, fontWeight: 700, flexShrink: 0 }}>
                      + Link
                    </button>
                  </div>
                ))}
              </div>
            )}
            {/* Linked list */}
            {cd && cd.campaigns.length > 0 && (
              <table style={{ width: '100%', borderCollapse: 'collapse' }}>
                <thead><tr>
                  <th style={thStyle}>Campaign</th><th style={thStyle}>Status</th>
                  <th style={thStyle}>Delivered</th><th style={thStyle}></th>
                </tr></thead>
                <tbody>
                  {cd.campaigns.map(c => (
                    <tr key={c.campaign_id}>
                      <td style={{ ...tdStyle, maxWidth: 320, overflow: 'hidden', textOverflow: 'ellipsis' }} title={c.name}>{c.name}</td>
                      <td style={tdStyle}>{c.status}</td>
                      <td style={tdStyle}>{fmtInt(c.delivered)}</td>
                      <td style={{ ...tdStyle, width: 36 }}>
                        <button onClick={() => removeCampaign(d.id, c.campaign_id)} title="Unlink campaign"
                          style={{ background: 'none', border: 'none', color: C.red, cursor: 'pointer' }}>
                          <FontAwesomeIcon icon={faTrash} />
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        )}
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
        {/* Deadline pace — the "finish sooner" lever (only when end_date is set) */}
        {d.end_date && d.progress.required_daily_to_deadline != null && (
          <div style={{
            padding: '10px 14px', borderRadius: 8, fontSize: 13,
            background: 'rgba(245,158,11,0.08)', border: `1px solid ${C.amber}`, color: C.heading,
          }}>
            <span style={{ fontWeight: 700, color: C.amber }}>Deadline: {d.end_date}</span>
            {' · '}{d.progress.days_to_deadline}d left · need{' '}
            <span style={{ fontWeight: 700 }}>{fmtInt(d.progress.required_daily_to_deadline)}/day</span>
            {' '}to deliver the remaining {fmtInt(Math.max(0, d.planned_volume - d.progress.delivered))} by then
            {' '}(vs {fmtInt(d.progress.required_daily)}/day on the {d.days_to_finish}-day plan).
          </div>
        )}
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

        {/* Linked campaigns & volume-to-goal */}
        {renderCampaigns(d)}

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

        {/* Month-to-date creative/subject performance — what's actually working
            for this offer THIS month (drip touches included, has_html=false). */}
        {renderCreativesMTD(d)}

        {/* Offer performance — same shared aggregation as the Offers tab */}
        <div style={{ borderTop: `1px solid ${C.border}`, paddingTop: 14 }}>
          {renderOfferPerformance(d)}
        </div>
      </div>
    );
  };

  const renderCreativesMTD = (d: Deal) => {
    if (!d.everflow_offer_id && !d.offer_name) return null; // unmapped deal — nothing to rank
    const cs = creatives[d.id];
    const view = cs?.view || 'creative';
    const viewBtn = (v: 'creative' | 'subject', label: string) => (
      <button key={v} onClick={() => loadCreatives(d, v)}
        style={{
          padding: '4px 12px', borderRadius: 14, fontSize: 11, cursor: 'pointer',
          border: `1px solid ${view === v ? C.indigo : C.border}`,
          background: view === v ? 'rgba(99,102,241,0.25)' : 'transparent',
          color: view === v ? C.heading : C.muted,
        }}>
        {label}
      </button>
    );
    return (
      <div style={{ borderTop: `1px solid ${C.border}`, paddingTop: 14 }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: 8, marginBottom: 8 }}>
          <div style={{ fontSize: 12, fontWeight: 700, color: C.heading, textTransform: 'uppercase', letterSpacing: 0.5 }}>
            What's working this month
          </div>
          <div style={{ display: 'flex', gap: 6 }}>
            {viewBtn('creative', 'By creative')}
            {viewBtn('subject', 'By subject')}
          </div>
        </div>
        {cs?.error && <div style={{ color: C.red, fontSize: 12 }}>{cs.error}</div>}
        {!cs || cs.loading ? (
          <div style={{ padding: 16, textAlign: 'center', color: C.muted }}><FontAwesomeIcon icon={faSpinner} spin /></div>
        ) : cs.rows.length === 0 ? (
          <div style={{ padding: 12, color: C.muted, fontSize: 12 }}>No sends for this offer yet this month.</div>
        ) : (
          <div style={{ overflowX: 'auto' }}>
            <table style={{ width: '100%', borderCollapse: 'collapse' }}>
              <thead><tr>
                <th style={thStyle}>{view === 'creative' ? 'Creative' : 'Subject'}</th>
                <th style={thStyle}>Brand</th>
                <th style={thStyle}>Delivered</th>
                <th style={thStyle}>Clicks</th>
                <th style={thStyle}>Click Rate</th>
                <th style={thStyle}>Conv</th>
                <th style={thStyle}>Conv Rate</th>
              </tr></thead>
              <tbody>
                {cs.rows.slice(0, 8).map((row, i) => (
                  <tr key={`${row.creative_key || row.subject}:${row.sending_domain}:${i}`}>
                    <td style={{ ...tdStyle, maxWidth: 340, overflow: 'hidden', textOverflow: 'ellipsis' }}>
                      <span style={{ color: C.heading }} title={view === 'creative' ? row.creative_label : row.subject}>
                        {view === 'creative' ? (row.creative_label || row.subject) : row.subject}
                      </span>
                      {!row.has_html && (
                        <span style={{ marginLeft: 6, fontSize: 10, color: C.indigo, border: `1px solid ${C.border}`, borderRadius: 999, padding: '1px 6px' }}
                          title="Drip/journey touch — no stored broadcast HTML">
                          drip
                        </span>
                      )}
                    </td>
                    <td style={tdStyle}>{row.sending_domain}</td>
                    <td style={tdStyle}>{fmtInt(row.delivered)}</td>
                    <td style={tdStyle}>{fmtInt(row.clicks)}</td>
                    <td style={tdStyle}>{(row.click_rate * 100).toFixed(2)}%</td>
                    <td style={{ ...tdStyle, color: row.conversions > 0 ? C.green : C.muted }}>{fmtInt(row.conversions)}</td>
                    <td style={tdStyle}>{row.conv_rate > 0 ? `${(row.conv_rate * 100).toFixed(1)}%` : '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
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
            <div>
              <label style={labelStyle}>Deadline (optional)</label>
              <input
                style={inputStyle} type="date"
                value={form.end_date}
                min={form.start_date || undefined}
                onChange={e => setForm({ ...form, end_date: e.target.value })}
              />
              <div style={{ fontSize: 10, color: C.muted, marginTop: 4 }}>
                Finish-sooner lever: set an end date and the deal shows the
                required/day to deliver the remaining volume by then. Blank = no deadline.
              </div>
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
            {editingId && (
              <div>
                <label style={labelStyle}>Conversions (override)</label>
                <input
                  style={inputStyle} type="number" min="0" step="1"
                  value={form.conversions_override} placeholder="auto (tracked + uploaded)"
                  onChange={e => setForm({ ...form, conversions_override: e.target.value })}
                />
                <div style={{ fontSize: 10, color: C.muted, marginTop: 4 }}>
                  Pin the true conversion count (e.g. Everflow). Blank = use tracked + uploaded.
                  Drives eCPA Actual = budget ÷ conversions.
                </div>
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
  // ─── Monthly historics & planning render ─────────────────────────────────────
  const monthLabel = (ym: string): string => {
    const parts = ym.split('-');
    const names = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
    const mi = parseInt(parts[1], 10) - 1;
    return `${names[mi] || parts[1]} ${parts[0]}`;
  };

  // Shared bits for the planner/history month grids.
  const monthNumInput: React.CSSProperties = {
    width: 96, padding: '6px 8px', background: 'rgba(10,20,45,0.7)',
    border: `1px solid ${C.border}`, borderRadius: 6, color: C.heading, fontSize: 12, boxSizing: 'border-box',
  };
  const synthMonthDeal = (deal: Deal): MonthDeal => ({
    deal_id: deal.id, name: deal.name, offer_name: deal.offer_name,
    has_target: false, target: { budget: null, volume: null, ecpm: null, ecpa: null, notes: '' },
    delivered: 0, revenue: 0, conversions: 0, conversions_tracked: 0, conversions_manual: 0, ecpm: deal.ecpm_goal,
  });

  // 2 · Planner — FUTURE months only (operator 2026-07-01: "select future
  // months and update, never previous"). The current month is edited in
  // Pacing; closed months live read-only in History.
  const renderPlanner = () => {
    const futureKeys = (() => {
      const keys = new Set<string>(curMonthYM ? nextMonthKeys(curMonthYM, 6) : []);
      months.forEach(m => { if (m.is_future) keys.add(m.month); });
      return Array.from(keys).sort();
    })();
    const selKey = plannerMonth && futureKeys.includes(plannerMonth) ? plannerMonth : (futureKeys[0] || '');
    const sel = months.find(m => m.month === selKey) || null; // null = future month with no saved targets yet

    const targetRow = (d: MonthDeal) => {
      const key = editKey(selKey, d.deal_id);
      const buf: TargetEdit = targetEdits[key] || {
        budget: d.target.budget != null ? String(d.target.budget) : '',
        volume: d.target.volume != null ? String(d.target.volume) : '',
        ecpm: d.target.ecpm != null ? String(d.target.ecpm) : '',
        ecpa: d.target.ecpa != null ? String(d.target.ecpa) : '',
        notes: d.target.notes || '',
      };
      const set = (k: keyof TargetEdit, v: string) =>
        setTargetEdits(prev => ({ ...prev, [key]: { ...(prev[key] || buf), [k]: v } }));
      const dirty = !!targetEdits[key];
      return (
        <tr key={d.deal_id}>
          <td style={tdStyle}>
            <div style={{ color: C.heading }}>{d.name}</div>
            {d.offer_name && <div style={{ fontSize: 11, color: C.muted }}>{d.offer_name}</div>}
          </td>
          <td style={tdStyle}><input type="number" style={monthNumInput} value={buf.budget} onChange={e => set('budget', e.target.value)} placeholder="$" /></td>
          <td style={tdStyle}>
            {(() => {
              const bv = parseFloat(buf.budget); const mv = parseFloat(buf.ecpm);
              const planned = bv > 0 && mv > 0 ? Math.ceil((bv / mv) * 1000) : 0;
              return <span style={{ color: planned > 0 ? C.heading : C.muted }} title="Derived: budget ÷ eCPM × 1000">{planned > 0 ? fmtInt(planned) : '—'}</span>;
            })()}
          </td>
          <td style={tdStyle}><input type="number" step="0.01" style={monthNumInput} value={buf.ecpm} onChange={e => set('ecpm', e.target.value)} placeholder="eCPM" /></td>
          <td style={tdStyle}><input type="number" step="0.01" style={monthNumInput} value={buf.ecpa} onChange={e => set('ecpa', e.target.value)} placeholder="eCPA" /></td>
          <td style={tdStyle}><input style={{ ...monthNumInput, width: 150 }} value={buf.notes} onChange={e => set('notes', e.target.value)} placeholder="notes" /></td>
          <td style={tdStyle}>
            <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
              <button onClick={() => saveTarget(d.deal_id, selKey)} disabled={!dirty || targetSaving === d.deal_id}
                style={{
                  padding: '5px 12px', borderRadius: 6, border: 'none', fontSize: 12, fontWeight: 600,
                  cursor: dirty ? 'pointer' : 'default',
                  background: dirty ? C.indigo : 'rgba(99,102,241,0.2)', color: '#fff', opacity: dirty ? 1 : 0.5,
                }}>
                {targetSaving === d.deal_id ? <FontAwesomeIcon icon={faSpinner} spin /> : 'Save'}
              </button>
              {d.has_target && (
                <button onClick={() => deleteTarget(d.deal_id, selKey)} disabled={targetSaving === d.deal_id}
                  title="Delete this month's planned budget"
                  style={{ padding: '5px 9px', borderRadius: 6, border: `1px solid ${C.border}`, fontSize: 12, cursor: 'pointer', background: 'transparent', color: C.red }}>
                  <FontAwesomeIcon icon={faTrash} />
                </button>
              )}
            </div>
          </td>
        </tr>
      );
    };

    return (
      <div style={{ background: C.panel, border: `1px solid ${C.border}`, borderRadius: 12, padding: 18, marginBottom: 18 }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: 12, marginBottom: 14 }}>
          <div>
            <div style={{ fontSize: 12, fontWeight: 700, letterSpacing: 0.6, textTransform: 'uppercase', color: C.heading }}>
              2 · Planner — Future Months
            </div>
            <div style={{ fontSize: 12, color: C.muted, marginTop: 3 }}>
              Set coming months' budgets ahead of time. This month is edited in Pacing above; closed months are read-only in History below.
            </div>
          </div>
          <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
            {futureKeys.map(k => (
              <button key={k} onClick={() => setPlannerMonth(k)}
                style={{
                  padding: '6px 12px', borderRadius: 16, fontSize: 12, cursor: 'pointer', whiteSpace: 'nowrap',
                  border: `1px solid ${k === selKey ? C.indigo : C.border}`,
                  background: k === selKey ? 'rgba(99,102,241,0.25)' : 'transparent',
                  color: k === selKey ? C.heading : C.muted,
                }}>
                {monthLabel(k)}
              </button>
            ))}
          </div>
        </div>

        {deals.length === 0 ? (
          <div style={{ padding: 16, textAlign: 'center', color: C.muted, fontSize: 13 }}>
            Create a deal first — then plan its coming months here.
          </div>
        ) : (
          <div style={{ overflowX: 'auto' }}>
            <table style={{ width: '100%', borderCollapse: 'collapse' }}>
              <thead><tr>
                <th style={thStyle}>Deal</th>
                <th style={thStyle}>Budget</th>
                <th style={thStyle}>Volume Goal</th>
                <th style={thStyle}>eCPM</th>
                <th style={thStyle}>eCPA</th>
                <th style={thStyle}>Notes</th>
                <th style={thStyle}></th>
              </tr></thead>
              <tbody>
                {deals.map(deal => targetRow(sel?.deals.find(x => x.deal_id === deal.id) || synthMonthDeal(deal)))}
              </tbody>
            </table>
          </div>
        )}

        <button onClick={openCreate}
          style={{ marginTop: 12, padding: '8px 14px', borderRadius: 8, border: `1px dashed ${C.border}`, background: 'transparent', color: C.indigo, fontSize: 13, cursor: 'pointer' }}>
          <FontAwesomeIcon icon={faPlus} /> Add deal to month
        </button>
      </div>
    );
  };

  // Read-only month breakdown inside Deal History & Performance — replaces
  // the old standalone Monthly History section.
  const renderMonthBreakdown = () => {
    const sel = months.find(m => m.month === historyMonth) || null;
    if (!monthsLoaded && monthsLoading) {
      return <div style={{ background: C.panel, border: `1px solid ${C.border}`, borderRadius: 12, padding: 30, textAlign: 'center', color: C.muted }}><FontAwesomeIcon icon={faSpinner} spin /></div>;
    }
    if (!sel) {
      return <div style={{ background: C.panel, border: `1px solid ${C.border}`, borderRadius: 12, padding: 20, textAlign: 'center', color: C.muted, fontSize: 13 }}>No data for {monthLabel(historyMonth)}.</div>;
    }
    return (
      <div style={{ background: C.panel, border: `1px solid ${C.border}`, borderRadius: 12, padding: 18 }}>
        <div style={{ display: 'flex', gap: 24, flexWrap: 'wrap', marginBottom: 14, padding: '10px 14px', background: 'rgba(10,20,45,0.5)', borderRadius: 8 }}>
          <Stat label="Closed month" value={monthLabel(sel.month)} />
          <Stat label="Delivered" value={fmtInt(sel.portfolio.delivered)} />
          <Stat label="Revenue" value={fmtMoney(sel.portfolio.revenue)} color={C.green} />
          <Stat label="Conversions" value={fmtInt(sel.portfolio.conversions)} />
          <Stat label="Target budget" value={sel.portfolio.target_budget > 0 ? fmtMoney(sel.portfolio.target_budget) : '—'} />
          <Stat label="Target volume" value={sel.portfolio.target_volume > 0 ? fmtInt(sel.portfolio.target_volume) : '—'} />
        </div>
        {sel.deals.length === 0 ? (
          <div style={{ padding: 16, textAlign: 'center', color: C.muted, fontSize: 13 }}>
            No deals with activity or targets for {monthLabel(sel.month)}.
          </div>
        ) : (
          <div style={{ overflowX: 'auto' }}>
            <table style={{ width: '100%', borderCollapse: 'collapse' }}>
              <thead><tr>
                <th style={thStyle}>Deal</th>
                <th style={thStyle}>Target Budget</th>
                <th style={thStyle}>Target Volume</th>
                <th style={thStyle}>eCPM</th>
                <th style={thStyle}>eCPA</th>
                <th style={thStyle}>Delivered</th>
                <th style={thStyle}>Revenue</th>
                <th style={thStyle}>Conv</th>
              </tr></thead>
              <tbody>
                {sel.deals.map(d => {
                  const pct = d.target.volume && d.target.volume > 0 ? (d.delivered / d.target.volume) * 100 : null;
                  return (
                    <tr key={d.deal_id}>
                      <td style={tdStyle}>
                        <div style={{ color: C.heading }}>{d.name}</div>
                        {d.offer_name && <div style={{ fontSize: 11, color: C.muted }}>{d.offer_name}</div>}
                      </td>
                      <td style={tdStyle}>{d.target.budget != null ? fmtMoney(d.target.budget) : '—'}</td>
                      <td style={tdStyle}>{d.target.volume != null ? fmtInt(d.target.volume) : '—'}</td>
                      <td style={tdStyle}>{d.ecpm > 0 ? '$' + d.ecpm.toFixed(2) : '—'}</td>
                      <td style={tdStyle}>{d.target.ecpa != null ? fmtMoney(d.target.ecpa) : '—'}</td>
                      <td style={tdStyle}>
                        {fmtInt(d.delivered)}
                        {pct != null && <span style={{ fontSize: 11, color: C.muted }}> ({Math.round(pct)}%)</span>}
                      </td>
                      <td style={{ ...tdStyle, color: C.green }}>{fmtMoney(d.revenue)}</td>
                      <td style={tdStyle}>{fmtInt(d.conversions)}</td>
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
            Set each deal's budget, eCPM goal and eCPA target → see the volume required, how this month
            is pacing against it, and which creatives, subjects and brands are earning it.
          </div>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          {recalcFlash && (
            <span style={{
              padding: '5px 12px', borderRadius: 999, fontSize: 12, fontWeight: 700,
              color: C.green, border: `1px solid ${C.green}`, background: 'rgba(16,185,129,0.1)',
              whiteSpace: 'nowrap',
            }}>
              Recalculated
            </span>
          )}
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
      </div>

      {error && (
        <div style={{
          marginBottom: 14, padding: '10px 14px', borderRadius: 8, fontSize: 13,
          background: 'rgba(239,68,68,0.1)', border: '1px solid rgba(239,68,68,0.4)', color: C.red,
        }}>
          {error}
        </div>
      )}

      {/* 0 · Platform capacity — topmost dashboard (operator 2026-07-01) */}
      <div style={{ marginBottom: 6 }}>
        <div style={{ fontSize: 12, fontWeight: 700, letterSpacing: 0.6, textTransform: 'uppercase', color: C.heading, marginBottom: 8 }}>
          Platform Capacity — Last 3 Days
        </div>
      </div>
      {renderCapacityStrip()}

      {/* 0.5 · Attribution coverage — is deal volume complete? */}
      {renderAttributionCoverage()}

      {/* 1 · Current-month pacing — the month in motion (editable) */}
      {renderPacing()}

      {/* 2 · Planner — future months only */}
      {renderPlanner()}

      {/* 3 · Deal history & performance — Lifetime by default; month pills
          drive the read-only per-month breakdown (replaces the old separate
          Monthly History section — operator 2026-07-01). */}
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-end', flexWrap: 'wrap', gap: 12, marginBottom: 10 }}>
        <div>
          <div style={{ fontSize: 12, fontWeight: 700, letterSpacing: 0.6, textTransform: 'uppercase', color: C.heading }}>
            3 · Deal History &amp; Performance
          </div>
          <div style={{ fontSize: 12, color: C.muted, marginTop: 3 }}>
            {historyMonth
              ? `${monthLabel(historyMonth)} — target vs what actually delivered (read-only).`
              : "Lifetime totals since each deal's start date (not this month). Click a row for performance detail."}
          </div>
        </div>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
          <button onClick={() => setHistoryMonth('')}
            style={{
              padding: '6px 12px', borderRadius: 16, fontSize: 12, cursor: 'pointer', whiteSpace: 'nowrap',
              border: `1px solid ${historyMonth === '' ? C.indigo : C.border}`,
              background: historyMonth === '' ? 'rgba(99,102,241,0.25)' : 'transparent',
              color: historyMonth === '' ? C.heading : C.muted, fontWeight: 700,
            }}>
            Lifetime
          </button>
          {months.filter(m => !m.is_current && !m.is_future).map(m => (
            <button key={m.month} onClick={() => setHistoryMonth(m.month)}
              style={{
                padding: '6px 12px', borderRadius: 16, fontSize: 12, cursor: 'pointer', whiteSpace: 'nowrap',
                border: `1px solid ${m.month === historyMonth ? C.indigo : C.border}`,
                background: m.month === historyMonth ? 'rgba(99,102,241,0.25)' : 'transparent',
                color: m.month === historyMonth ? C.heading : C.muted,
              }}>
              {monthLabel(m.month)}
            </button>
          ))}
        </div>
      </div>
      {historyMonth ? renderMonthBreakdown() : loading ? (
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
                        {d.offer_name || (d.everflow_offer_id ? `Offer ${d.everflow_offer_id}` : 'unmapped')}
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
