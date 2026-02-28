import { useState, useEffect, useCallback, useMemo, Fragment } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faSync,
  faArrowUp,
  faArrowDown,
  faMinus,
  faDatabase,
  faChartLine,
  faSortUp,
  faSortDown,
  faChevronDown,
  faChevronRight,
} from '@fortawesome/free-solid-svg-icons';
import {
  AreaChart,
  Area,
  BarChart,
  Bar,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  Legend,
  ResponsiveContainer,
} from 'recharts';
import { MetricCard } from '../common/MetricCard';
import { useDateFilter } from '../../context/DateFilterContext';

// ── Types ────────────────────────────────────────────────────────────────────

interface DailyMetric {
  date: string;
  clicks: number;
  conversions: number;
  revenue: number;
}

interface DataSetCodeMetrics {
  data_set_code: string;
  clicks: number;
  conversions: number;
  revenue: number;
  volume: number;
  cvr: number;
  epc: number;
}

interface OfferPartnerMetrics {
  offer_id: string;
  offer_name: string;
  is_cpm: boolean;
  clicks: number;
  conversions: number;
  revenue: number;
}

interface PartnerPerformance {
  code: string;
  name: string;
  data_set_code: string;
  clicks: number;
  conversions: number;
  revenue: number;
  cpa_revenue: number;
  cpm_revenue: number;
  volume: number;
  payout: number;
  cvr: number;
  epc: number;
  daily_series: DailyMetric[];
  data_set_breakdown: DataSetCodeMetrics[];
  offer_breakdown: OfferPartnerMetrics[];
}

interface PeriodSummary {
  label: string;
  clicks: number;
  conversions: number;
  revenue: number;
  cpa_revenue: number;
  cpm_revenue: number;
  volume: number;
}

interface MoMComparison {
  current_month: PeriodSummary;
  previous_month: PeriodSummary;
  revenue_change_pct: number;
  conversions_change_pct: number;
  clicks_change_pct: number;
}

interface OfferPartnerEntry {
  partner_code: string;
  partner_name: string;
  clicks: number;
  click_share: number;
  conversions: number;
  revenue: number;
}

interface OfferWithPartnerBreakdown {
  offer_id: string;
  offer_name: string;
  is_cpm: boolean;
  total_clicks: number;
  total_conversions: number;
  total_revenue: number;
  partners: OfferPartnerEntry[];
}

interface DataPartnerAnalytics {
  partners: PartnerPerformance[];
  totals: PeriodSummary;
  mom_comparison: MoMComparison;
  cached_at?: string;
  default_volume?: number;
  default_cost_ecpm?: number;
  total_esp_cost?: number;
  cpm_offers?: OfferWithPartnerBreakdown[];
  cpa_offers?: OfferWithPartnerBreakdown[];
}

// ── Helpers ──────────────────────────────────────────────────────────────────

const PARTNER_COLORS = [
  '#6366f1', '#10b981', '#f59e0b', '#ef4444', '#8b5cf6',
  '#ec4899', '#14b8a6', '#f97316', '#06b6d4', '#84cc16',
];

function fmt$(n: number): string {
  if (n >= 1_000_000) return `$${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `$${(n / 1_000).toFixed(1)}K`;
  return `$${n.toFixed(2)}`;
}

function fmtN(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
  return n.toLocaleString();
}

function fmtPct(n: number): string {
  return `${n >= 0 ? '+' : ''}${n.toFixed(1)}%`;
}

function timeAgo(iso: string): string {
  const diff = Date.now() - new Date(iso).getTime();
  const mins = Math.floor(diff / 60000);
  if (mins < 1) return 'just now';
  if (mins < 60) return `${mins}m ago`;
  const hrs = Math.floor(mins / 60);
  return `${hrs}h ${mins % 60}m ago`;
}

// ── Component ────────────────────────────────────────────────────────────────

type SortKey = 'name' | 'clicks' | 'conversions' | 'cpa_revenue' | 'cpm_revenue' | 'revenue' | 'volume' | 'cost' | 'payout' | 'profit' | 'margin' | 'cvr' | 'epc';

const ECPM_STORAGE_KEY = 'dp_playground_ecpm';
const COST_MODE_KEY = 'dp_playground_cost_mode'; // 'ecpm' | 'total'
const TOTAL_COST_KEY = 'dp_playground_total_cost';
function loadNum(key: string, fallback: number): number {
  try {
    const v = localStorage.getItem(key);
    return v ? parseFloat(v) || fallback : fallback;
  } catch { return fallback; }
}
function loadStr(key: string, fallback: string): string {
  try {
    return localStorage.getItem(key) || fallback;
  } catch { return fallback; }
}

export const DataPartnerDashboard: React.FC = () => {
  const [data, setData] = useState<DataPartnerAnalytics | null>(null);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [sortKey, setSortKey] = useState<SortKey>('revenue');
  const [sortAsc, setSortAsc] = useState(false);
  const [playCostEcpm, setPlayCostEcpm] = useState<number | null>(null);
  const [playTotalCost, setPlayTotalCost] = useState<number | null>(null);
  // Which input the user last edited -- 'ecpm' or 'total'
  const [costMode, setCostMode] = useState<'ecpm' | 'total'>(() => loadStr(COST_MODE_KEY, 'ecpm') as 'ecpm' | 'total');
  const [expandedPartner, setExpandedPartner] = useState<string | null>(null);
  const [expandedSection, setExpandedSection] = useState<'datasets' | 'offers'>('datasets');
  const [offerViewTab, setOfferViewTab] = useState<'cpm' | 'cpa'>('cpm');
  const [expandedOffer, setExpandedOffer] = useState<string | null>(null);

  const { dateRange } = useDateFilter();

  // Once we get server defaults, seed playground values (only once per server response)
  useEffect(() => {
    if (!data) return;
    const savedMode = loadStr(COST_MODE_KEY, 'ecpm') as 'ecpm' | 'total';
    if (savedMode === 'total') {
      // User last edited total cost -- restore that and derive eCPM
      setPlayTotalCost((prev) => {
        if (prev !== null) return prev;
        const saved = loadNum(TOTAL_COST_KEY, 0);
        return saved > 0 ? saved : (data.total_esp_cost ?? 0);
      });
      setCostMode('total');
    } else {
      // User last edited eCPM -- restore that
      setPlayCostEcpm((prev) => {
        if (prev !== null) return prev;
        const saved = loadNum(ECPM_STORAGE_KEY, 0);
        return saved > 0 ? saved : (data.default_cost_ecpm ?? 5);
      });
      setCostMode('ecpm');
    }
  }, [data]);

  // Persist values
  useEffect(() => {
    if (playCostEcpm !== null) localStorage.setItem(ECPM_STORAGE_KEY, String(playCostEcpm));
  }, [playCostEcpm]);
  useEffect(() => {
    if (playTotalCost !== null) localStorage.setItem(TOTAL_COST_KEY, String(playTotalCost));
  }, [playTotalCost]);
  useEffect(() => {
    localStorage.setItem(COST_MODE_KEY, costMode);
  }, [costMode]);

  // The total partner volume used for cost ↔ eCPM conversion
  const totalPartnerVolumeForCalc = data?.totals?.volume ?? data?.default_volume ?? 1;

  // Derive the effective eCPM based on which input the user last edited
  const costEcpm = useMemo(() => {
    if (costMode === 'total' && playTotalCost !== null && totalPartnerVolumeForCalc > 0) {
      return (playTotalCost / totalPartnerVolumeForCalc) * 1000;
    }
    return playCostEcpm ?? data?.default_cost_ecpm ?? 5;
  }, [costMode, playTotalCost, playCostEcpm, data, totalPartnerVolumeForCalc]);

  // Derive total cost for display (when eCPM mode is active)
  const displayTotalCost = useMemo(() => {
    if (costMode === 'total' && playTotalCost !== null) return playTotalCost;
    return (costEcpm * totalPartnerVolumeForCalc) / 1000;
  }, [costMode, playTotalCost, costEcpm, totalPartnerVolumeForCalc]);

  // ── fetch cached data ─────────────────────────────────────────────────────
  const fetchData = useCallback(async () => {
    try {
      setLoading(true);
      setError(null);
      const params = new URLSearchParams({
        start_date: dateRange.startDate,
        end_date: dateRange.endDate,
        range_type: dateRange.type,
      });
      const res = await fetch(`/api/data-partners/analytics?${params}`, {
        credentials: 'include',
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const json: DataPartnerAnalytics = await res.json();
      setData(json);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Unknown error');
    } finally {
      setLoading(false);
    }
  }, [dateRange]);

  // ── manual refresh (rebuilds server cache) ────────────────────────────────
  const handleRefresh = useCallback(async () => {
    try {
      setRefreshing(true);
      const res = await fetch('/api/data-partners/refresh', {
        method: 'POST',
        credentials: 'include',
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const json: DataPartnerAnalytics = await res.json();
      setData(json);
    } catch {
      // Fall back to regular fetch
      await fetchData();
    } finally {
      setRefreshing(false);
    }
  }, [fetchData]);

  useEffect(() => {
    fetchData();
  }, [fetchData]);

  // ── helper: compute cost and payout per data partner ────────────────────
  // Cost = ESP sending cost = volume × eCPM / 1000
  // Payout = Revenue - Cost (what the data partner receives after ESP costs)
  // When costs go UP, payouts go DOWN (inverse relationship)
  const calcCost = useCallback(
    (volume: number) => (volume * costEcpm) / 1000,
    [costEcpm],
  );
  const calcPayout = useCallback(
    (revenue: number, volume: number) => {
      const cost = (volume * costEcpm) / 1000;
      // 50:50 rev share: partner receives half of (revenue - cost)
      return (revenue - cost) / 2;
    },
    [costEcpm],
  );

  // ── sorted partners ──────────────────────────────────────────────────────
  const sortedPartners = useMemo(() => {
    if (!data?.partners) return [];
    const arr = [...data.partners];
    arr.sort((a, b) => {
      let va: number | string = 0;
      let vb: number | string = 0;
      const profitA = a.revenue - calcCost(a.volume);
      const profitB = b.revenue - calcCost(b.volume);
      switch (sortKey) {
        case 'name': va = a.name.toLowerCase(); vb = b.name.toLowerCase(); break;
        case 'clicks': va = a.clicks; vb = b.clicks; break;
        case 'conversions': va = a.conversions; vb = b.conversions; break;
        case 'cpa_revenue': va = a.cpa_revenue; vb = b.cpa_revenue; break;
        case 'cpm_revenue': va = a.cpm_revenue; vb = b.cpm_revenue; break;
        case 'revenue': va = a.revenue; vb = b.revenue; break;
        case 'volume': va = a.volume; vb = b.volume; break;
        case 'cost': va = calcCost(a.volume); vb = calcCost(b.volume); break;
        case 'payout': va = calcPayout(a.revenue, a.volume); vb = calcPayout(b.revenue, b.volume); break;
        case 'profit': va = profitA; vb = profitB; break;
        case 'margin': {
          va = a.revenue > 0 ? profitA / a.revenue : 0;
          vb = b.revenue > 0 ? profitB / b.revenue : 0;
          break;
        }
        case 'cvr': va = a.cvr; vb = b.cvr; break;
        case 'epc': va = a.epc; vb = b.epc; break;
      }
      if (va < vb) return sortAsc ? -1 : 1;
      if (va > vb) return sortAsc ? 1 : -1;
      return 0;
    });
    return arr;
  }, [data, sortKey, sortAsc, calcPayout, calcCost]);

  // ── daily chart data (stacked by partner) ────────────────────────────────
  const dailyChartData = useMemo(() => {
    if (!data?.partners?.length) return [];
    const dateMap: Record<string, Record<string, number>> = {};
    for (const p of data.partners) {
      for (const d of (p.daily_series || [])) {
        if (!dateMap[d.date]) dateMap[d.date] = {};
        dateMap[d.date][p.code] = d.revenue;
      }
    }
    return Object.entries(dateMap)
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([date, partners]) => ({ date, ...partners }));
  }, [data]);

  // ── MoM bar chart data ───────────────────────────────────────────────────
  const momChartData = useMemo(() => {
    if (!data?.mom_comparison) return [];
    const { current_month, previous_month } = data.mom_comparison;
    return [
      { name: 'Revenue', current: current_month.revenue, previous: previous_month.revenue },
      { name: 'Conversions', current: current_month.conversions, previous: previous_month.conversions },
      { name: 'Clicks', current: current_month.clicks, previous: previous_month.clicks },
    ];
  }, [data]);

  const handleSort = (key: SortKey) => {
    if (sortKey === key) setSortAsc(!sortAsc);
    else { setSortKey(key); setSortAsc(false); }
  };

  const topPartner = useMemo(() => {
    if (!data?.partners?.length) return null;
    return data.partners.reduce((top, p) => (p.revenue > top.revenue ? p : top), data.partners[0]);
  }, [data]);

  const mom = data?.mom_comparison;

  // ── render ───────────────────────────────────────────────────────────────

  if (loading && !data) {
    return (
      <div className="datapartners-dashboard">
        <div className="loading-state">
          <FontAwesomeIcon icon={faSync} spin style={{ fontSize: '2rem' }} />
          <p>Loading data partner analytics...</p>
        </div>
      </div>
    );
  }

  if (error && !data) {
    return (
      <div className="datapartners-dashboard">
        <div className="dashboard-header">
          <div className="header-title">
            <FontAwesomeIcon icon={faDatabase} style={{ fontSize: '24px' }} />
            <h2>Data Partners</h2>
          </div>
        </div>
        <div className="error-state card">
          <p>Error loading data: {error}</p>
          <button onClick={fetchData} className="btn btn-primary">
            <FontAwesomeIcon icon={faSync} /> Retry
          </button>
        </div>
      </div>
    );
  }

  return (
    <div className="datapartners-dashboard">
      {/* ── Header ──────────────────────────────────────────────────────────── */}
      <div className="dashboard-header">
        <div className="header-title">
          <FontAwesomeIcon icon={faDatabase} style={{ fontSize: '24px' }} />
          <h2>Data Partners</h2>
          <span className="date-range-badge">{dateRange.label}</span>
        </div>
        <div className="header-actions">
          {data?.cached_at && (
            <span className="last-updated">Cached: {timeAgo(data.cached_at)}</span>
          )}
          <button
            onClick={handleRefresh}
            disabled={refreshing}
            className="btn-icon"
            title="Rebuild cache"
          >
            <FontAwesomeIcon icon={faSync} className={refreshing ? 'spinning' : ''} />
          </button>
        </div>
      </div>

      {/* ── Cost Playground ────────────────────────────────────────────────── */}
      {(() => {
        const totalPartnerVolume = data?.totals?.volume ?? 0;
        const totalCost = calcCost(totalPartnerVolume);
        const totalRevenue = data?.totals?.revenue ?? 0;
        const totalProfit = totalRevenue - totalCost;
        const totalPayout = calcPayout(totalRevenue, totalPartnerVolume);
        return (
          <div className="playground-bar card">
            <div className="playground-inner">
              <div className="playground-label">
                <FontAwesomeIcon icon={faChartLine} style={{ marginRight: '0.4rem' }} />
                Cost Playground
              </div>
              <div className="playground-fields">
                <div className="playground-field">
                  <label htmlFor="play-total-cost">Total Cost ($)</label>
                  <input
                    id="play-total-cost"
                    type="number"
                    value={Math.round(displayTotalCost * 100) / 100}
                    onChange={(e) => {
                      const val = parseFloat(e.target.value) || 0;
                      setPlayTotalCost(val);
                      setCostMode('total');
                    }}
                    step={500}
                    min={0}
                    className={`playground-input wide ${costMode === 'total' ? 'input-active' : ''}`}
                  />
                  <span className="playground-hint">
                    Actual: ${(data?.total_esp_cost ?? 0).toLocaleString(undefined, { maximumFractionDigits: 0 })}
                  </span>
                </div>
                <div className="playground-field">
                  <label htmlFor="play-ecpm">Cost eCPM ($)</label>
                  <input
                    id="play-ecpm"
                    type="number"
                    value={Math.round(costEcpm * 10000) / 10000}
                    onChange={(e) => {
                      const val = parseFloat(e.target.value) || 0;
                      setPlayCostEcpm(val);
                      setCostMode('ecpm');
                    }}
                    step={0.01}
                    min={0}
                    className={`playground-input ${costMode === 'ecpm' ? 'input-active' : ''}`}
                  />
                  <span className="playground-hint">
                    Actual: ${(data?.default_cost_ecpm ?? 0).toFixed(4)}
                  </span>
                </div>
                <div className="playground-divider" />
                <div className="playground-result">
                  <span className="playground-result-label">Volume</span>
                  <span className="playground-result-value volume">{fmtN(totalPartnerVolume)}</span>
                </div>
                <div className="playground-result">
                  <span className="playground-result-label">CPA Rev</span>
                  <span className="playground-result-value revenue">{fmt$(data?.totals?.cpa_revenue ?? 0)}</span>
                </div>
                <div className="playground-result">
                  <span className="playground-result-label">CPM Rev</span>
                  <span className="playground-result-value" style={{ color: 'var(--accent-purple, #8b5cf6)' }}>{fmt$(data?.totals?.cpm_revenue ?? 0)}</span>
                </div>
                <div className="playground-result">
                  <span className="playground-result-label">Total Revenue</span>
                  <span className="playground-result-value revenue">{fmt$(totalRevenue)}</span>
                </div>
                <div className="playground-result">
                  <span className="playground-result-label">ESP Cost</span>
                  <span className="playground-result-value muted">{fmt$(totalCost)}</span>
                </div>
                <div className="playground-result">
                  <span className="playground-result-label">Payout</span>
                  <span className={`playground-result-value ${totalPayout >= 0 ? 'profit-pos' : 'profit-neg'}`} style={{ color: 'var(--accent-blue, #3b82f6)' }}>
                    {fmt$(totalPayout)}
                  </span>
                </div>
                <div className="playground-result">
                  <span className="playground-result-label">Profit</span>
                  <span className={`playground-result-value ${totalProfit >= 0 ? 'profit-pos' : 'profit-neg'}`}>
                    {fmt$(totalProfit)}
                  </span>
                </div>
                <div className="playground-result">
                  <span className="playground-result-label">Margin</span>
                  <span className={`playground-result-value ${totalProfit >= 0 ? 'profit-pos' : 'profit-neg'}`}>
                    {totalRevenue > 0 ? `${((totalProfit / totalRevenue) * 100).toFixed(1)}%` : '—'}
                  </span>
                </div>
              </div>
              <button
                className="btn-link playground-reset"
                onClick={() => {
                  setPlayCostEcpm(data?.default_cost_ecpm ?? 5);
                  setPlayTotalCost(data?.total_esp_cost ?? 0);
                  setCostMode('ecpm');
                  localStorage.removeItem(ECPM_STORAGE_KEY);
                  localStorage.removeItem(TOTAL_COST_KEY);
                  localStorage.removeItem(COST_MODE_KEY);
                }}
              >
                Reset All
              </button>
            </div>
          </div>
        );
      })()}

      {/* ── Metric Cards ────────────────────────────────────────────────────── */}
      <div className="section-header">
        <h3>Overview</h3>
      </div>
      <div className="metric-grid">
        <MetricCard
          label="Total Revenue"
          value={fmt$(data?.totals.revenue ?? 0)}
          subtitle={`CPA: ${fmt$(data?.totals.cpa_revenue ?? 0)} · CPM: ${fmt$(data?.totals.cpm_revenue ?? 0)}`}
          change={mom ? mom.revenue_change_pct / 100 : undefined}
          changeLabel="MoM"
        />
        <MetricCard
          label="Total Conversions"
          value={fmtN(data?.totals.conversions ?? 0)}
          change={mom ? mom.conversions_change_pct / 100 : undefined}
          changeLabel="MoM"
        />
        <MetricCard
          label="Total Clicks"
          value={fmtN(data?.totals.clicks ?? 0)}
          change={mom ? mom.clicks_change_pct / 100 : undefined}
          changeLabel="MoM"
        />
        <MetricCard
          label="Top Partner"
          value={topPartner?.name ?? '-'}
          subtitle={topPartner ? fmt$(topPartner.revenue) : undefined}
        />
      </div>

      {/* ── Daily Revenue Chart ─────────────────────────────────────────────── */}
      {dailyChartData.length > 0 && (
        <>
          <div className="section-header">
            <h3>
              <FontAwesomeIcon icon={faChartLine} style={{ marginRight: '0.4rem' }} />
              Daily Revenue by Partner
            </h3>
          </div>
          <div className="chart-card card">
            <ResponsiveContainer width="100%" height={320}>
              <AreaChart data={dailyChartData}>
                <CartesianGrid strokeDasharray="3 3" stroke="var(--border-color, #3d4f66)" />
                <XAxis
                  dataKey="date"
                  stroke="var(--text-muted, #94a3b8)"
                  fontSize={12}
                  tickFormatter={(d: string) => {
                    const dt = new Date(d + 'T00:00:00');
                    return `${dt.getMonth() + 1}/${dt.getDate()}`;
                  }}
                />
                <YAxis stroke="var(--text-muted, #94a3b8)" fontSize={12} tickFormatter={(v: number) => fmt$(v)} />
                <Tooltip
                  contentStyle={{
                    backgroundColor: 'var(--bg-secondary, #1e293b)',
                    border: '1px solid var(--border-color, #3d4f66)',
                    borderRadius: '8px',
                    color: 'var(--text-primary, #f8fafc)',
                  }}
                  labelStyle={{ color: 'var(--text-secondary, #cbd5e1)' }}
                  formatter={(value: number) => [fmt$(value), '']}
                />
                <Legend />
                {data?.partners.map((p, i) => (
                  <Area
                    key={p.code}
                    type="monotone"
                    dataKey={p.code}
                    name={p.name}
                    stackId="1"
                    stroke={PARTNER_COLORS[i % PARTNER_COLORS.length]}
                    fill={PARTNER_COLORS[i % PARTNER_COLORS.length]}
                    fillOpacity={0.5}
                  />
                ))}
              </AreaChart>
            </ResponsiveContainer>
          </div>
        </>
      )}

      {/* ── Partner Performance Table ───────────────────────────────────────── */}
      <div className="section-header">
        <h3>Partner Performance</h3>
        {sortedPartners.length > 0 && (
          <span className="total-badge">
            Total: {fmt$(data?.totals.revenue ?? 0)}
          </span>
        )}
      </div>
      <div className="card">
        <div className="table-container">
          <table className="table">
            <thead>
              <tr>
                <th style={{ width: '28px' }} />
                <ThSort col="name" label="Partner" active={sortKey} asc={sortAsc} onSort={handleSort} />
                <ThSort col="clicks" label="Clicks" active={sortKey} asc={sortAsc} onSort={handleSort} align="right" />
                <ThSort col="conversions" label="Conv" active={sortKey} asc={sortAsc} onSort={handleSort} align="right" />
                <ThSort col="cpa_revenue" label="CPA Rev" active={sortKey} asc={sortAsc} onSort={handleSort} align="right" />
                <ThSort col="cpm_revenue" label="CPM Rev" active={sortKey} asc={sortAsc} onSort={handleSort} align="right" />
                <ThSort col="revenue" label="Total Rev" active={sortKey} asc={sortAsc} onSort={handleSort} align="right" />
                <ThSort col="volume" label="Volume" active={sortKey} asc={sortAsc} onSort={handleSort} align="right" />
                <ThSort col="cost" label="Cost" active={sortKey} asc={sortAsc} onSort={handleSort} align="right" />
                <ThSort col="payout" label="Payout" active={sortKey} asc={sortAsc} onSort={handleSort} align="right" />
                <ThSort col="profit" label="Profit" active={sortKey} asc={sortAsc} onSort={handleSort} align="right" />
                <ThSort col="margin" label="Margin" active={sortKey} asc={sortAsc} onSort={handleSort} align="right" />
                <ThSort col="epc" label="EPC" active={sortKey} asc={sortAsc} onSort={handleSort} align="right" />
              </tr>
            </thead>
            <tbody>
              {sortedPartners.length === 0 ? (
                <tr>
                  <td colSpan={14} style={{ textAlign: 'center', padding: '2rem', color: 'var(--text-muted)' }}>
                    No data partner activity found in this date range
                  </td>
                </tr>
              ) : (
                sortedPartners.map((p, i) => {
                  const isExpanded = expandedPartner === p.code;
                  const cost = calcCost(p.volume);
                  const payout = calcPayout(p.revenue, p.volume);
                  const profit = p.revenue - cost;
                  const margin = p.revenue > 0 ? (profit / p.revenue) * 100 : 0;
                  const dsBreakdown = p.data_set_breakdown || [];
                  const offerBreakdown = p.offer_breakdown || [];
                  const hasBreakdown = dsBreakdown.length > 0 || offerBreakdown.length > 0;

                  return (
                    <>
                      <tr
                        key={p.code}
                        className={`partner-row ${isExpanded ? 'expanded' : ''} ${hasBreakdown ? 'expandable' : ''}`}
                        onClick={() => {
                          if (hasBreakdown) {
                            setExpandedPartner(isExpanded ? null : p.code);
                          }
                        }}
                      >
                        <td style={{ width: '28px', textAlign: 'center' }}>
                          {hasBreakdown && (
                            <FontAwesomeIcon
                              icon={isExpanded ? faChevronDown : faChevronRight}
                              style={{ fontSize: '0.7rem', color: 'var(--text-muted)' }}
                            />
                          )}
                        </td>
                        <td>
                          <span className="partner-name-cell">
                            <span
                              className="partner-dot"
                              style={{ backgroundColor: PARTNER_COLORS[i % PARTNER_COLORS.length] }}
                            />
                            {p.name}
                            <code className="dataset-code">{p.code}</code>
                          </span>
                        </td>
                        <td style={{ textAlign: 'right' }}>{fmtN(p.clicks)}</td>
                        <td style={{ textAlign: 'right' }}>{fmtN(p.conversions)}</td>
                        <td style={{ textAlign: 'right', color: 'var(--accent-green)' }}>{fmt$(p.cpa_revenue)}</td>
                        <td style={{ textAlign: 'right', color: 'var(--accent-purple, #8b5cf6)' }}>{fmt$(p.cpm_revenue)}</td>
                        <td style={{ textAlign: 'right', color: 'var(--accent-green)', fontWeight: 600 }}>{fmt$(p.revenue)}</td>
                        <td style={{ textAlign: 'right' }}>{fmtN(p.volume)}</td>
                        <td style={{ textAlign: 'right', color: 'var(--accent-orange, #f59e0b)' }}>{fmt$(cost)}</td>
                        <td style={{ textAlign: 'right', color: payout >= 0 ? 'var(--accent-blue, #3b82f6)' : 'var(--accent-red)', fontWeight: 600 }}>{fmt$(payout)}</td>
                        <td style={{ textAlign: 'right', color: profit >= 0 ? 'var(--accent-green)' : 'var(--accent-red)', fontWeight: 600 }}>{fmt$(profit)}</td>
                        <td style={{ textAlign: 'right', color: profit >= 0 ? 'var(--accent-green)' : 'var(--accent-red)' }}>{margin.toFixed(1)}%</td>
                        <td style={{ textAlign: 'right' }}>${p.epc.toFixed(2)}</td>
                      </tr>
                      {isExpanded && (
                        <>
                          {/* Breakdown section tabs */}
                          <tr className="breakdown-tabs-row">
                            <td />
                            <td colSpan={12}>
                              <div className="breakdown-tabs">
                                <button
                                  className={`breakdown-tab ${expandedSection === 'datasets' ? 'active' : ''}`}
                                  onClick={(e) => { e.stopPropagation(); setExpandedSection('datasets'); }}
                                >
                                  Data Sets ({dsBreakdown.length})
                                </button>
                                <button
                                  className={`breakdown-tab ${expandedSection === 'offers' ? 'active' : ''}`}
                                  onClick={(e) => { e.stopPropagation(); setExpandedSection('offers'); }}
                                >
                                  Offers ({offerBreakdown.length})
                                </button>
                              </div>
                            </td>
                          </tr>
                          {expandedSection === 'datasets' && dsBreakdown.map((ds) => {
                            const dsCost = calcCost(ds.volume);
                            const dsProfit = ds.revenue - dsCost;
                            const dsMargin = ds.revenue > 0 ? (dsProfit / ds.revenue) * 100 : 0;
                            return (
                              <tr key={`${p.code}-ds-${ds.data_set_code}`} className="breakdown-row">
                                <td />
                                <td style={{ paddingLeft: '2rem' }}>
                                  <code className="dataset-code">{ds.data_set_code}</code>
                                </td>
                                <td style={{ textAlign: 'right' }}>{fmtN(ds.clicks)}</td>
                                <td style={{ textAlign: 'right' }}>{fmtN(ds.conversions)}</td>
                                <td style={{ textAlign: 'right', color: 'var(--accent-green)' }}>{fmt$(ds.revenue)}</td>
                                <td />
                                <td />
                                <td style={{ textAlign: 'right' }}>{fmtN(ds.volume)}</td>
                                <td style={{ textAlign: 'right', color: 'var(--accent-orange, #f59e0b)' }}>{fmt$(dsCost)}</td>
                                <td style={{ textAlign: 'right', color: dsProfit >= 0 ? 'var(--accent-green)' : 'var(--accent-red)' }}>{fmt$(dsProfit)}</td>
                                <td style={{ textAlign: 'right', color: dsProfit >= 0 ? 'var(--accent-green)' : 'var(--accent-red)' }}>{dsMargin.toFixed(1)}%</td>
                                <td style={{ textAlign: 'right' }}>${ds.epc.toFixed(2)}</td>
                              </tr>
                            );
                          })}
                          {expandedSection === 'offers' && offerBreakdown.map((o) => (
                            <tr key={`${p.code}-offer-${o.offer_id}`} className="breakdown-row">
                              <td />
                              <td style={{ paddingLeft: '2rem' }}>
                                <span className="offer-name">
                                  {o.is_cpm && <span className="cpm-badge">CPM</span>}
                                  {o.offer_name || `Offer #${o.offer_id}`}
                                </span>
                              </td>
                              <td style={{ textAlign: 'right' }}>{fmtN(o.clicks)}</td>
                              <td style={{ textAlign: 'right' }}>{fmtN(o.conversions)}</td>
                              <td style={{ textAlign: 'right', color: o.is_cpm ? 'transparent' : 'var(--accent-green)' }}>
                                {o.is_cpm ? '' : fmt$(o.revenue)}
                              </td>
                              <td style={{ textAlign: 'right', color: o.is_cpm ? 'var(--accent-purple, #8b5cf6)' : 'transparent' }}>
                                {o.is_cpm ? fmt$(o.revenue) : ''}
                              </td>
                              <td style={{ textAlign: 'right', color: 'var(--accent-green)' }}>{fmt$(o.revenue)}</td>
                              <td />
                              <td />
                              <td />
                              <td />
                              <td />
                            </tr>
                          ))}
                        </>
                      )}
                    </>
                  );
                })
              )}
            </tbody>
            {sortedPartners.length > 0 && (() => {
              const totalCost = calcCost(data?.totals.volume ?? 0);
              const totalPayout = calcPayout(data?.totals.revenue ?? 0, data?.totals.volume ?? 0);
              const totalProfit = (data?.totals.revenue ?? 0) - totalCost;
              const totalMargin = (data?.totals.revenue ?? 0) > 0 ? (totalProfit / (data?.totals.revenue ?? 1)) * 100 : 0;
              return (
                <tfoot>
                  <tr>
                    <td />
                    <td style={{ fontWeight: 600 }}>Totals</td>
                    <td style={{ textAlign: 'right', fontWeight: 600 }}>{fmtN(data?.totals.clicks ?? 0)}</td>
                    <td style={{ textAlign: 'right', fontWeight: 600 }}>{fmtN(data?.totals.conversions ?? 0)}</td>
                    <td style={{ textAlign: 'right', fontWeight: 600, color: 'var(--accent-green)' }}>{fmt$(data?.totals.cpa_revenue ?? 0)}</td>
                    <td style={{ textAlign: 'right', fontWeight: 600, color: 'var(--accent-purple, #8b5cf6)' }}>{fmt$(data?.totals.cpm_revenue ?? 0)}</td>
                    <td style={{ textAlign: 'right', fontWeight: 600, color: 'var(--accent-green)' }}>{fmt$(data?.totals.revenue ?? 0)}</td>
                    <td style={{ textAlign: 'right', fontWeight: 600 }}>{fmtN(data?.totals.volume ?? 0)}</td>
                    <td style={{ textAlign: 'right', fontWeight: 600, color: 'var(--accent-orange, #f59e0b)' }}>{fmt$(totalCost)}</td>
                    <td style={{ textAlign: 'right', fontWeight: 600, color: totalPayout >= 0 ? 'var(--accent-blue, #3b82f6)' : 'var(--accent-red)' }}>{fmt$(totalPayout)}</td>
                    <td style={{ textAlign: 'right', fontWeight: 600, color: totalProfit >= 0 ? 'var(--accent-green)' : 'var(--accent-red)' }}>{fmt$(totalProfit)}</td>
                    <td style={{ textAlign: 'right', fontWeight: 600, color: totalProfit >= 0 ? 'var(--accent-green)' : 'var(--accent-red)' }}>{totalMargin.toFixed(1)}%</td>
                    <td />
                  </tr>
                </tfoot>
              );
            })()}
          </table>
        </div>
      </div>

      {/* ── Offer-Centric View ────────────────────────────────────────────── */}
      {data && ((data.cpm_offers && data.cpm_offers.length > 0) || (data.cpa_offers && data.cpa_offers.length > 0)) && (
        <div style={{ marginTop: '1.5rem' }}>
          <div className="section-header">
            <h3>Offer Performance by Partner</h3>
            <div className="offer-view-tabs">
              <button
                className={`offer-view-tab ${offerViewTab === 'cpm' ? 'active' : ''}`}
                onClick={() => { setOfferViewTab('cpm'); setExpandedOffer(null); }}
              >
                CPM Offers ({data.cpm_offers?.length ?? 0})
              </button>
              <button
                className={`offer-view-tab ${offerViewTab === 'cpa' ? 'active' : ''}`}
                onClick={() => { setOfferViewTab('cpa'); setExpandedOffer(null); }}
              >
                CPA Offers ({data.cpa_offers?.length ?? 0})
              </button>
            </div>
          </div>

          <div className="table-wrapper card">
            <table className="data-table">
              <thead>
                <tr>
                  <th style={{ width: '40px' }} />
                  <th>Offer</th>
                  <th style={{ textAlign: 'right' }}>Clicks</th>
                  {offerViewTab === 'cpa' && <th style={{ textAlign: 'right' }}>Conv</th>}
                  <th style={{ textAlign: 'right' }}>Revenue</th>
                  <th style={{ textAlign: 'right' }}>Partners</th>
                </tr>
              </thead>
              <tbody>
                {(offerViewTab === 'cpm' ? data.cpm_offers : data.cpa_offers)?.map((offer) => {
                  const isExpanded = expandedOffer === offer.offer_id;
                  return (
                    <Fragment key={offer.offer_id}>
                      <tr
                        className={`partner-row ${isExpanded ? 'expanded' : ''}`}
                        onClick={() => setExpandedOffer(isExpanded ? null : offer.offer_id)}
                        style={{ cursor: 'pointer' }}
                      >
                        <td>
                          <FontAwesomeIcon
                            icon={isExpanded ? faChevronDown : faChevronRight}
                            style={{ fontSize: '0.7rem', color: 'var(--text-muted)' }}
                          />
                        </td>
                        <td>
                          <span className="offer-name">
                            {offer.is_cpm && <span className="cpm-badge">CPM</span>}
                            {offer.offer_name || `Offer #${offer.offer_id}`}
                          </span>
                        </td>
                        <td style={{ textAlign: 'right' }}>{fmtN(offer.total_clicks)}</td>
                        {offerViewTab === 'cpa' && <td style={{ textAlign: 'right' }}>{fmtN(offer.total_conversions)}</td>}
                        <td style={{ textAlign: 'right', color: 'var(--accent-green)', fontWeight: 600 }}>{fmt$(offer.total_revenue)}</td>
                        <td style={{ textAlign: 'right', color: 'var(--text-muted)' }}>{offer.partners?.length ?? 0}</td>
                      </tr>
                      {isExpanded && offer.partners && (
                        <tr className="breakdown-container-row">
                          <td colSpan={offerViewTab === 'cpa' ? 6 : 5} style={{ padding: 0 }}>
                            <div className="offer-partner-breakdown">
                              <table className="data-table breakdown-table">
                                <thead>
                                  <tr>
                                    <th>Partner</th>
                                    <th style={{ textAlign: 'right' }}>Clicks</th>
                                    <th style={{ textAlign: 'right' }}>Click Share</th>
                                    {offerViewTab === 'cpa' && <th style={{ textAlign: 'right' }}>Conv</th>}
                                    <th style={{ textAlign: 'right' }}>Revenue</th>
                                  </tr>
                                </thead>
                                <tbody>
                                  {offer.partners.map((pe) => (
                                    <tr key={pe.partner_code} className="breakdown-row">
                                      <td style={{ paddingLeft: '1.5rem' }}>
                                        <span className="partner-badge" style={{ fontSize: '0.85rem' }}>
                                          {pe.partner_name || pe.partner_code}
                                        </span>
                                      </td>
                                      <td style={{ textAlign: 'right' }}>{fmtN(pe.clicks)}</td>
                                      <td style={{ textAlign: 'right', color: 'var(--text-muted)' }}>{pe.click_share.toFixed(1)}%</td>
                                      {offerViewTab === 'cpa' && <td style={{ textAlign: 'right' }}>{fmtN(pe.conversions)}</td>}
                                      <td style={{ textAlign: 'right', color: 'var(--accent-green)' }}>{fmt$(pe.revenue)}</td>
                                    </tr>
                                  ))}
                                </tbody>
                                <tfoot>
                                  <tr>
                                    <td style={{ fontWeight: 600, paddingLeft: '1.5rem' }}>Total</td>
                                    <td style={{ textAlign: 'right', fontWeight: 600 }}>{fmtN(offer.total_clicks)}</td>
                                    <td style={{ textAlign: 'right', fontWeight: 600, color: 'var(--text-muted)' }}>100%</td>
                                    {offerViewTab === 'cpa' && <td style={{ textAlign: 'right', fontWeight: 600 }}>{fmtN(offer.total_conversions)}</td>}
                                    <td style={{ textAlign: 'right', fontWeight: 600, color: 'var(--accent-green)' }}>{fmt$(offer.total_revenue)}</td>
                                  </tr>
                                </tfoot>
                              </table>
                            </div>
                          </td>
                        </tr>
                      )}
                    </Fragment>
                  );
                })}
              </tbody>
              {/* Totals footer */}
              {(() => {
                const offers = offerViewTab === 'cpm' ? data.cpm_offers : data.cpa_offers;
                if (!offers || offers.length === 0) return null;
                const totClicks = offers.reduce((s, o) => s + o.total_clicks, 0);
                const totConv = offers.reduce((s, o) => s + o.total_conversions, 0);
                const totRev = offers.reduce((s, o) => s + o.total_revenue, 0);
                return (
                  <tfoot>
                    <tr>
                      <td />
                      <td style={{ fontWeight: 600 }}>Total ({offers.length} offers)</td>
                      <td style={{ textAlign: 'right', fontWeight: 600 }}>{fmtN(totClicks)}</td>
                      {offerViewTab === 'cpa' && <td style={{ textAlign: 'right', fontWeight: 600 }}>{fmtN(totConv)}</td>}
                      <td style={{ textAlign: 'right', fontWeight: 600, color: 'var(--accent-green)' }}>{fmt$(totRev)}</td>
                      <td />
                    </tr>
                  </tfoot>
                );
              })()}
            </table>
          </div>
        </div>
      )}

      {/* ── Month-over-Month Comparison ──────────────────────────────────── */}
      {mom && (
        <>
          <div className="section-header" style={{ marginTop: '1.5rem' }}>
            <h3>Month-over-Month Comparison</h3>
          </div>
          <div className="mom-grid">
            <div className="chart-card card">
              <ResponsiveContainer width="100%" height={260}>
                <BarChart data={momChartData}>
                  <CartesianGrid strokeDasharray="3 3" stroke="var(--border-color, #3d4f66)" />
                  <XAxis dataKey="name" stroke="var(--text-muted)" fontSize={12} />
                  <YAxis stroke="var(--text-muted)" fontSize={12} />
                  <Tooltip
                    contentStyle={{
                      backgroundColor: 'var(--bg-secondary, #1e293b)',
                      border: '1px solid var(--border-color, #3d4f66)',
                      borderRadius: '8px',
                      color: 'var(--text-primary)',
                    }}
                  />
                  <Legend />
                  <Bar dataKey="previous" name={mom.previous_month.label} fill="var(--text-muted, #94a3b8)" radius={[4, 4, 0, 0]} />
                  <Bar dataKey="current" name={mom.current_month.label} fill="var(--accent-blue, #60a5fa)" radius={[4, 4, 0, 0]} />
                </BarChart>
              </ResponsiveContainer>
            </div>
            <div className="mom-summary card">
              <div className="mom-summary-inner">
                <MoMRow label="Revenue" current={fmt$(mom.current_month.revenue)} previous={fmt$(mom.previous_month.revenue)} changePct={mom.revenue_change_pct} />
                <MoMRow label="Conversions" current={fmtN(mom.current_month.conversions)} previous={fmtN(mom.previous_month.conversions)} changePct={mom.conversions_change_pct} />
                <MoMRow label="Clicks" current={fmtN(mom.current_month.clicks)} previous={fmtN(mom.previous_month.clicks)} changePct={mom.clicks_change_pct} />
              </div>
            </div>
          </div>
        </>
      )}

      {/* ── Component Styles ────────────────────────────────────────────────── */}
      <style>{`
        .datapartners-dashboard {
          padding: 1rem;
        }
        .datapartners-dashboard .dashboard-header {
          display: flex;
          justify-content: space-between;
          align-items: center;
          margin-bottom: 1rem;
        }
        .datapartners-dashboard .header-title {
          display: flex;
          align-items: center;
          gap: 0.5rem;
        }
        .datapartners-dashboard .header-title h2 {
          margin: 0;
          font-size: 1.5rem;
        }
        .datapartners-dashboard .header-actions {
          display: flex;
          align-items: center;
          gap: 1rem;
        }
        .datapartners-dashboard .last-updated {
          font-size: 0.85rem;
          color: var(--text-muted, #666);
        }
        .datapartners-dashboard .date-range-badge {
          padding: 0.2rem 0.6rem;
          background: var(--primary-color, #6366f1);
          color: white;
          border-radius: 1rem;
          font-size: 0.75rem;
          font-weight: 500;
        }
        .datapartners-dashboard .btn-icon {
          padding: 0.5rem;
          background: transparent;
          border: 1px solid var(--border-color, #3d4f66);
          border-radius: 6px;
          cursor: pointer;
          color: var(--text-primary);
          display: flex;
          align-items: center;
          justify-content: center;
        }
        .datapartners-dashboard .btn-icon:hover {
          background: var(--bg-tertiary, #334155);
        }
        .spinning {
          animation: dp-spin 1s linear infinite;
        }
        @keyframes dp-spin {
          from { transform: rotate(0deg); }
          to { transform: rotate(360deg); }
        }
        .datapartners-dashboard .metric-grid {
          display: grid;
          grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
          gap: 1rem;
          margin-bottom: 1.5rem;
        }
        .datapartners-dashboard .section-header {
          display: flex;
          justify-content: space-between;
          align-items: center;
          margin-bottom: 0.75rem;
        }
        .datapartners-dashboard .section-header h3 {
          margin: 0;
          font-size: 1.1rem;
        }
        .datapartners-dashboard .total-badge {
          background: var(--accent-green, #22c55e);
          color: white;
          padding: 0.25rem 0.75rem;
          border-radius: 12px;
          font-size: 0.85rem;
          font-weight: 600;
        }
        .datapartners-dashboard .chart-card {
          padding: 1rem;
          margin-bottom: 1.5rem;
        }
        .datapartners-dashboard .loading-state {
          display: flex;
          flex-direction: column;
          align-items: center;
          justify-content: center;
          padding: 4rem;
          color: var(--text-muted, #666);
        }
        .datapartners-dashboard .loading-state p {
          margin-top: 1rem;
        }
        .datapartners-dashboard .error-state {
          padding: 2rem;
          text-align: center;
        }
        .datapartners-dashboard .error-state p {
          margin-bottom: 1rem;
          color: var(--accent-red, #ef4444);
        }

        /* ── Table enhancements ── */
        .datapartners-dashboard .table th {
          cursor: pointer;
          user-select: none;
          transition: color 0.15s;
        }
        .datapartners-dashboard .table th:hover {
          color: var(--text-primary);
        }
        .datapartners-dashboard .table th .sort-icon {
          margin-left: 0.25rem;
          font-size: 0.65rem;
          opacity: 0.7;
        }
        .datapartners-dashboard .table tfoot td {
          border-top: 2px solid var(--border-color, #3d4f66);
          background: var(--bg-tertiary, #334155);
          font-size: var(--text-sm);
        }
        .partner-name-cell {
          display: flex;
          align-items: center;
          gap: 0.5rem;
        }
        .partner-dot {
          width: 10px;
          height: 10px;
          border-radius: 50%;
          flex-shrink: 0;
        }
        .dataset-code {
          font-size: 0.75rem;
          padding: 0.15rem 0.4rem;
          background: var(--bg-tertiary, #334155);
          border-radius: 4px;
          color: var(--text-muted);
        }

        /* ── Cost Playground ── */
        .playground-bar {
          margin-bottom: 1.5rem;
          padding: 1rem 1.25rem;
          border: 1px solid var(--primary-color, #6366f1);
          border-left: 4px solid var(--primary-color, #6366f1);
        }
        .playground-inner {
          display: flex;
          align-items: center;
          gap: 1.5rem;
          flex-wrap: wrap;
        }
        .playground-label {
          font-weight: 600;
          font-size: 0.9rem;
          color: var(--text-primary);
          white-space: nowrap;
        }
        .playground-fields {
          display: flex;
          align-items: center;
          gap: 1.25rem;
          flex-wrap: wrap;
          flex: 1;
        }
        .playground-field {
          display: flex;
          flex-direction: column;
          gap: 0.2rem;
        }
        .playground-field label {
          font-size: 0.7rem;
          color: var(--text-muted);
          text-transform: uppercase;
          letter-spacing: 0.04em;
        }
        .playground-hint {
          font-size: 0.68rem;
          color: var(--text-muted);
          opacity: 0.7;
        }
        .playground-input {
          width: 110px;
          padding: 0.4rem 0.6rem;
          background: var(--bg-tertiary, #334155);
          border: 1px solid var(--border-color, #3d4f66);
          border-radius: 6px;
          color: var(--text-primary);
          font-size: 0.9rem;
          text-align: right;
          font-variant-numeric: tabular-nums;
        }
        .playground-input.wide {
          width: 140px;
        }
        .playground-input.input-active {
          border-color: var(--primary-color, #6366f1);
          background: rgba(99,102,241,0.08);
        }
        .playground-input:focus {
          outline: none;
          border-color: var(--primary-color, #6366f1);
          box-shadow: 0 0 0 2px rgba(99,102,241,0.2);
        }
        .playground-divider {
          width: 1px;
          height: 36px;
          background: var(--border-color, #3d4f66);
        }
        .playground-result {
          display: flex;
          flex-direction: column;
          align-items: flex-end;
          gap: 0.15rem;
        }
        .playground-result-label {
          font-size: 0.7rem;
          color: var(--text-muted);
          text-transform: uppercase;
          letter-spacing: 0.04em;
        }
        .playground-result-value {
          font-size: 1rem;
          font-weight: 700;
          font-variant-numeric: tabular-nums;
        }
        .playground-result-value.volume { color: var(--text-primary); }
        .playground-result-value.payout { color: var(--accent-orange, #f59e0b); }
        .playground-result-value.revenue { color: var(--accent-green, #22c55e); }
        .playground-result-value.profit-pos { color: var(--accent-green, #22c55e); }
        .playground-result-value.profit-neg { color: var(--accent-red, #ef4444); }
        .playground-result-value.muted { color: var(--text-muted); font-weight: 500; font-size: 0.9rem; }
        .playground-reset {
          font-size: 0.8rem;
          color: var(--text-muted);
          background: none;
          border: none;
          cursor: pointer;
          padding: 0;
          text-decoration: underline;
          margin-left: auto;
        }
        .playground-reset:hover {
          color: var(--text-primary);
        }

        /* ── Expandable rows ── */
        .partner-row.expandable {
          cursor: pointer;
        }
        .partner-row.expandable:hover {
          background: var(--bg-tertiary, #334155);
        }
        .partner-row.expanded {
          background: var(--bg-tertiary, #334155);
        }
        .breakdown-row {
          background: var(--bg-primary, #0f172a);
          font-size: 0.88em;
        }
        .breakdown-row td {
          color: var(--text-secondary, #cbd5e1);
          border-bottom: 1px solid rgba(99,102,241,0.1);
        }
        .breakdown-tabs-row td {
          padding: 0.4rem 0.5rem !important;
          border-bottom: none !important;
        }
        .breakdown-tabs {
          display: flex;
          gap: 0.5rem;
        }
        .breakdown-tab {
          padding: 0.25rem 0.75rem;
          font-size: 0.78rem;
          border: 1px solid var(--border-color, #3d4f66);
          border-radius: 4px;
          background: transparent;
          color: var(--text-muted);
          cursor: pointer;
          transition: all 0.15s;
        }
        .breakdown-tab.active {
          background: var(--primary-color, #6366f1);
          color: white;
          border-color: var(--primary-color, #6366f1);
        }
        .breakdown-tab:hover:not(.active) {
          background: var(--bg-tertiary, #334155);
        }
        .offer-name {
          font-size: 0.85rem;
          color: var(--text-secondary, #cbd5e1);
          display: flex;
          align-items: center;
          gap: 0.4rem;
        }
        .cpm-badge {
          display: inline-block;
          font-size: 0.6rem;
          font-weight: 700;
          letter-spacing: 0.05em;
          padding: 0.1rem 0.35rem;
          background: var(--accent-purple, #8b5cf6);
          color: white;
          border-radius: 3px;
          text-transform: uppercase;
          flex-shrink: 0;
        }

        /* ── MoM section ── */
        .mom-grid {
          display: grid;
          grid-template-columns: 1fr 1fr;
          gap: 1rem;
          margin-bottom: 1.5rem;
        }
        @media (max-width: 900px) {
          .mom-grid {
            grid-template-columns: 1fr;
          }
        }
        .mom-summary {
          padding: 1rem;
        }
        .mom-summary-inner {
          display: flex;
          flex-direction: column;
          gap: 0.75rem;
          height: 100%;
          justify-content: center;
        }
        .mom-row {
          display: flex;
          align-items: center;
          justify-content: space-between;
          padding: 0.75rem 1rem;
          background: var(--bg-tertiary, #334155);
          border-radius: var(--radius-lg, 8px);
        }
        .mom-row-label {
          font-size: 0.85rem;
          color: var(--text-muted);
        }
        .mom-row-values {
          display: flex;
          align-items: center;
          gap: 1rem;
        }
        .mom-row-prev {
          font-size: 0.85rem;
          color: var(--text-muted);
        }
        .mom-row-current {
          font-weight: 600;
          color: var(--text-primary);
        }
        .mom-row-change {
          font-size: 0.85rem;
          font-weight: 500;
          display: flex;
          align-items: center;
          gap: 0.25rem;
        }
        .mom-row-change.positive { color: var(--accent-green); }
        .mom-row-change.negative { color: var(--accent-red); }
        .mom-row-change.neutral  { color: var(--text-muted); }
      `}</style>
    </div>
  );
};

// ── Sub-components ───────────────────────────────────────────────────────────

const MoMRow: React.FC<{
  label: string;
  current: string;
  previous: string;
  changePct: number;
}> = ({ label, current, previous, changePct }) => (
  <div className="mom-row">
    <span className="mom-row-label">{label}</span>
    <div className="mom-row-values">
      <span className="mom-row-prev">{previous}</span>
      <span className="mom-row-current">{current}</span>
      <span className={`mom-row-change ${changePct > 0 ? 'positive' : changePct < 0 ? 'negative' : 'neutral'}`}>
        <FontAwesomeIcon icon={changePct > 0 ? faArrowUp : changePct < 0 ? faArrowDown : faMinus} />
        {fmtPct(changePct)}
      </span>
    </div>
  </div>
);

const ThSort: React.FC<{
  col: SortKey;
  label: string;
  active: SortKey;
  asc: boolean;
  onSort: (k: SortKey) => void;
  align?: 'left' | 'right';
}> = ({ col, label, active, asc, onSort, align = 'left' }) => (
  <th style={{ textAlign: align }} onClick={() => onSort(col)}>
    {label}
    {active === col && (
      <FontAwesomeIcon icon={asc ? faSortUp : faSortDown} className="sort-icon" />
    )}
  </th>
);
