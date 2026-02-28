import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faStore, faLightbulb, faPalette, faGlobe, faSearch,
  faFilter, faTimes, faPlus, faTrash, faArrowUp, faArrowDown,
  faStar, faFire, faBolt, faChartLine, faExternalLinkAlt,
  faSyncAlt, faSpinner, faEye, faCaretUp, faCaretDown,
  faMinus, faCrosshairs, faChartBar, faDollarSign, faPercent
} from '@fortawesome/free-solid-svg-icons';
import { useAuth } from '../../../contexts/AuthContext';
import './OfferCenter.css';

// ─── Types ───────────────────────────────────────────────────────────────────

interface ComparisonOffer {
  offer_id: string;
  offer_name: string;
  offer_type: string;
  network_clicks: number;
  network_conversions: number;
  network_revenue: number;
  network_epc: number;
  network_cvr: number;
  your_clicks: number;
  your_conversions: number;
  your_revenue: number;
  your_epc: number;
  your_cvr: number;
  delta_epc: number;
  delta_cvr: number;
}

interface Totals {
  clicks: number;
  conversions: number;
  revenue: number;
  epc: number;
  cvr: number;
}

interface ComparisonData {
  offers: ComparisonOffer[];
  your_totals: Totals;
  network_totals: Totals;
}

interface Suggestion {
  offer_id: string;
  offer_name: string;
  type: string;
  reasoning: string;
  score: number;
  metrics: Record<string, number | undefined>;
}

interface Creative {
  id: string;
  offer_id: string;
  offer_name: string;
  creative_name: string;
  source: string;
  html_content: string;
  text_content: string;
  ai_optimized: boolean;
  tags: string[];
  use_count: number;
  status: string;
  created_at: string;
}

interface TopOffer {
  offer_id: string;
  offer_name: string;
  offer_type: string;
  network_revenue: number;
  epc: number;
  cvr: number;
  trend: string;
  ai_score: number;
  ai_recommendation: string;
}

interface PerformanceMetrics {
  clicks: number;
  unique_clicks: number;
  conversions: number;
  revenue: number;
  payout: number;
  profit: number;
  margin: number;
  epc: number;
  cvr: number;
  cpa: number;
  rpc: number;
}

interface DailyTrendEntry {
  date: string;
  clicks: number;
  conversions: number;
  revenue: number;
  payout: number;
  profit: number;
  epc: number;
  cvr: number;
}

interface OfferPerformance {
  offer_id: string;
  offer_name: string;
  offer_type: string;
  network: PerformanceMetrics | null;
  your_team: PerformanceMetrics | null;
  daily_trend: DailyTrendEntry[];
}

type Tab = 'comparison' | 'suggestions' | 'creatives' | 'top' | 'performance';
type TimeRange = 'today' | '7' | '14' | 'mtd';
type SortField = 'revenue' | 'epc' | 'delta_epc';

// ─── Helpers ─────────────────────────────────────────────────────────────────

const orgFetch = async (url: string, orgId?: string) => {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  if (orgId) headers['X-Organization-ID'] = orgId;
  return fetch(url, { headers });
};

const fmt = (n: number | undefined | null): string => {
  if (n == null || isNaN(n)) return '0';
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M';
  if (n >= 1_000) return (n / 1_000).toFixed(1) + 'K';
  return n.toLocaleString();
};

const fmtCurrency = (n: number | undefined | null): string => {
  if (n == null || isNaN(n)) return '$0.00';
  if (n >= 1_000_000) return '$' + (n / 1_000_000).toFixed(2) + 'M';
  if (n >= 1_000) return '$' + (n / 1_000).toFixed(1) + 'K';
  return '$' + n.toFixed(2);
};

const pct = (n: number | undefined | null): string => (n == null || isNaN(n)) ? '0.0%' : n.toFixed(1) + '%';

const typeBadgeClass = (t: string): string => {
  const key = t.toLowerCase();
  if (key === 'cpm') return 'oc-type-cpm';
  if (key === 'cps') return 'oc-type-cps';
  if (key === 'cpa') return 'oc-type-cpa';
  if (key === 'cpl') return 'oc-type-cpl';
  return 'oc-type-default';
};

const sugBadgeClass = (t: string): string => {
  const key = t.toLowerCase();
  if (key === 'trending') return 'oc-sug-trending';
  if (key === 'untested') return 'oc-sug-untested';
  if (key === 'high_epc') return 'oc-sug-high_epc';
  if (key === 'opportunity') return 'oc-sug-opportunity';
  return 'oc-sug-default';
};

const sugIcon = (t: string) => {
  const key = t.toLowerCase();
  if (key === 'trending') return faFire;
  if (key === 'untested') return faBolt;
  if (key === 'high_epc') return faChartLine;
  return faStar;
};

const scoreTier = (s: number): string => {
  if (s >= 70) return 'high';
  if (s >= 40) return 'medium';
  return 'low';
};

const fmtDate = (s: string): string => {
  const d = new Date(s);
  return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' });
};

const fmtPct = (n: number | undefined | null): string => (n == null || isNaN(n)) ? '—' : (n >= 0 ? '+' : '') + n.toFixed(1) + '%';

// ─── Statistical helpers for the trader dashboard ────────────────────────────

type ChartMetric = 'revenue' | 'clicks' | 'epc' | 'cvr' | 'profit';

const computeStats = (trend: DailyTrendEntry[]) => {
  if (!trend || trend.length === 0) return null;

  const revenues = trend.map(d => d.revenue);
  const profits = trend.map(d => d.profit);
  const clicks = trend.map(d => d.clicks);
  const n = revenues.length;

  const sum = (a: number[]) => a.reduce((s, v) => s + v, 0);
  const mean = (a: number[]) => a.length ? sum(a) / a.length : 0;
  const median = (a: number[]) => {
    const sorted = [...a].sort((x, y) => x - y);
    const mid = Math.floor(sorted.length / 2);
    return sorted.length % 2 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2;
  };
  const stdDev = (a: number[]) => {
    const m = mean(a);
    return Math.sqrt(a.reduce((s, v) => s + (v - m) ** 2, 0) / a.length);
  };

  const avgRevenue = mean(revenues);
  const medianRevenue = median(revenues);
  const revenueStdDev = stdDev(revenues);
  const totalRevenue = sum(revenues);
  const totalProfit = sum(profits);
  const totalClicks = sum(clicks);

  // Best / worst day
  const bestIdx = revenues.indexOf(Math.max(...revenues));
  const worstIdx = revenues.indexOf(Math.min(...revenues));
  const bestDay = trend[bestIdx];
  const worstDay = trend[worstIdx];

  // Day-over-day changes
  const dodChanges: number[] = [];
  for (let i = 1; i < n; i++) {
    if (revenues[i - 1] > 0) {
      dodChanges.push((revenues[i] - revenues[i - 1]) / revenues[i - 1] * 100);
    }
  }
  const avgDodChange = dodChanges.length ? mean(dodChanges) : 0;

  // Streak: consecutive positive or negative days (from most recent)
  let streak = 0;
  let streakDir: 'up' | 'down' | 'flat' = 'flat';
  if (n >= 2) {
    const lastChange = revenues[n - 1] - revenues[n - 2];
    streakDir = lastChange > 0 ? 'up' : lastChange < 0 ? 'down' : 'flat';
    for (let i = n - 1; i >= 1; i--) {
      const change = revenues[i] - revenues[i - 1];
      if ((streakDir === 'up' && change > 0) || (streakDir === 'down' && change < 0)) {
        streak++;
      } else break;
    }
  }

  // Momentum: compare last 3 days avg vs prior 3 days avg
  let momentum: 'bullish' | 'bearish' | 'neutral' = 'neutral';
  let momentumPct = 0;
  if (n >= 6) {
    const recent3 = mean(revenues.slice(n - 3));
    const prior3 = mean(revenues.slice(n - 6, n - 3));
    if (prior3 > 0) {
      momentumPct = (recent3 - prior3) / prior3 * 100;
      momentum = momentumPct > 5 ? 'bullish' : momentumPct < -5 ? 'bearish' : 'neutral';
    }
  } else if (n >= 2) {
    const lastVal = revenues[n - 1];
    const prevVal = revenues[n - 2];
    if (prevVal > 0) {
      momentumPct = (lastVal - prevVal) / prevVal * 100;
      momentum = momentumPct > 5 ? 'bullish' : momentumPct < -5 ? 'bearish' : 'neutral';
    }
  }

  // Volatility: coefficient of variation
  const volatility = avgRevenue > 0 ? (revenueStdDev / avgRevenue) * 100 : 0;
  const volLevel: 'low' | 'medium' | 'high' = volatility < 20 ? 'low' : volatility < 50 ? 'medium' : 'high';

  // Simple moving averages (3-day and 7-day) for chart overlay
  const sma = (arr: number[], window: number): (number | null)[] =>
    arr.map((_, i) => i < window - 1 ? null : mean(arr.slice(i - window + 1, i + 1)));

  const sma3 = sma(revenues, 3);
  const sma7 = sma(revenues, Math.min(7, n));

  return {
    tradingDays: n,
    totalRevenue,
    totalProfit,
    totalClicks,
    avgRevenue,
    medianRevenue,
    revenueStdDev,
    bestDay,
    worstDay,
    avgDodChange,
    streak,
    streakDir,
    momentum,
    momentumPct,
    volatility,
    volLevel,
    sma3,
    sma7,
    dodChanges,
  };
};

// ─── Component ───────────────────────────────────────────────────────────────

export const OfferCenter: React.FC<{
  onUseOffer?: (offerId: string, offerName: string) => void;
}> = ({ onUseOffer }) => {
  const { organization } = useAuth();
  const orgId = organization?.id || '';

  // Tab state
  const [activeTab, setActiveTab] = useState<Tab>('comparison');

  // Comparison state
  const [compRange, setCompRange] = useState<TimeRange>('7');
  const [compData, setCompData] = useState<ComparisonData | null>(null);
  const [compLoading, setCompLoading] = useState(false);
  const [compError, setCompError] = useState('');
  const [compSort, setCompSort] = useState<SortField>('revenue');

  // Suggestions state
  const [suggestions, setSuggestions] = useState<Suggestion[]>([]);
  const [sugLoading, setSugLoading] = useState(false);
  const [sugError, setSugError] = useState('');

  // Creatives state
  const [creatives, setCreatives] = useState<Creative[]>([]);
  const [creativesLoading, setCreativesLoading] = useState(false);
  const [creativesError, setCreativesError] = useState('');
  const [creativeSearch, setCreativeSearch] = useState('');
  const [creativeFilter, setCreativeFilter] = useState('');
  const [showForm, setShowForm] = useState(false);
  const [previewCreative, setPreviewCreative] = useState<Creative | null>(null);
  const [formData, setFormData] = useState({
    offer_id: '', offer_name: '', creative_name: '',
    html_content: '', text_content: '', source: 'manual', tags: ''
  });
  const [formSaving, setFormSaving] = useState(false);

  // Top offers state
  const [topOffers, setTopOffers] = useState<TopOffer[]>([]);
  const [topLoading, setTopLoading] = useState(false);
  const [topError, setTopError] = useState('');

  // Performance history state
  const [perfOfferId, setPerfOfferId] = useState('');
  const [perfData, setPerfData] = useState<OfferPerformance | null>(null);
  const [perfLoading, setPerfLoading] = useState(false);
  const [perfError, setPerfError] = useState('');
  const [perfChartMetric, setPerfChartMetric] = useState<ChartMetric>('revenue');

  // ─── Fetch: Comparison ──────────────────────────────────────────────────

  const fetchComparison = useCallback(async () => {
    setCompLoading(true);
    setCompError('');
    try {
      const res = await orgFetch(`/api/mailing/offer-center/comparison?days=${compRange}`, orgId);
      if (!res.ok) throw new Error('Failed to load comparison data');
      const data = await res.json();
      setCompData(data);
    } catch (e: unknown) {
      setCompError(e instanceof Error ? e.message : 'Failed to load');
    } finally {
      setCompLoading(false);
    }
  }, [compRange, orgId]);

  // ─── Fetch: Suggestions ─────────────────────────────────────────────────

  const fetchSuggestions = useCallback(async () => {
    setSugLoading(true);
    setSugError('');
    try {
      const res = await orgFetch('/api/mailing/offer-center/suggestions', orgId);
      if (!res.ok) throw new Error('Failed to load suggestions');
      const data = await res.json();
      setSuggestions(data.suggestions || []);
    } catch (e: unknown) {
      setSugError(e instanceof Error ? e.message : 'Failed to load');
    } finally {
      setSugLoading(false);
    }
  }, [orgId]);

  // ─── Fetch: Creatives ───────────────────────────────────────────────────

  const fetchCreatives = useCallback(async () => {
    setCreativesLoading(true);
    setCreativesError('');
    try {
      const params = new URLSearchParams({ status: 'active' });
      if (creativeSearch) params.set('search', creativeSearch);
      if (creativeFilter) params.set('offer_id', creativeFilter);
      const res = await orgFetch(`/api/mailing/offer-center/creatives?${params}`, orgId);
      if (!res.ok) throw new Error('Failed to load creatives');
      const data = await res.json();
      setCreatives(data.creatives || []);
    } catch (e: unknown) {
      setCreativesError(e instanceof Error ? e.message : 'Failed to load');
    } finally {
      setCreativesLoading(false);
    }
  }, [orgId, creativeSearch, creativeFilter]);

  // ─── Fetch: Top Network Offers ──────────────────────────────────────────

  const fetchTopOffers = useCallback(async () => {
    setTopLoading(true);
    setTopError('');
    try {
      const res = await orgFetch('/api/everflow/network-top-offers', orgId);
      if (!res.ok) throw new Error('Failed to load top offers');
      const data = await res.json();
      setTopOffers(Array.isArray(data) ? data : data.offers || []);
    } catch (e: unknown) {
      setTopError(e instanceof Error ? e.message : 'Failed to load');
    } finally {
      setTopLoading(false);
    }
  }, [orgId]);

  // ─── Fetch: Offer Performance History ───────────────────────────────────

  const fetchPerformance = useCallback(async (offerId: string) => {
    if (!offerId) return;
    setPerfLoading(true);
    setPerfError('');
    setPerfData(null);
    try {
      const res = await orgFetch(`/api/mailing/offer-center/performance?offer_id=${encodeURIComponent(offerId)}`, orgId);
      if (!res.ok) throw new Error('Failed to load offer performance');
      const data = await res.json();
      setPerfData(data);
    } catch (e: unknown) {
      setPerfError(e instanceof Error ? e.message : 'Failed to load');
    } finally {
      setPerfLoading(false);
    }
  }, [orgId]);

  // Helper: navigate to performance tab for a specific offer
  const showPerformance = useCallback((offerId: string) => {
    setPerfOfferId(offerId);
    setActiveTab('performance');
    fetchPerformance(offerId);
  }, [fetchPerformance]);

  // ─── Effects ────────────────────────────────────────────────────────────

  useEffect(() => { fetchComparison(); }, [fetchComparison]);
  useEffect(() => {
    if (activeTab === 'suggestions' && suggestions.length === 0 && !sugLoading) fetchSuggestions();
  }, [activeTab, suggestions.length, sugLoading, fetchSuggestions]);
  useEffect(() => {
    if (activeTab === 'creatives') fetchCreatives();
  }, [activeTab, fetchCreatives]);
  useEffect(() => {
    if (activeTab === 'top' && topOffers.length === 0 && !topLoading) fetchTopOffers();
  }, [activeTab, topOffers.length, topLoading, fetchTopOffers]);

  // ─── Save Creative ──────────────────────────────────────────────────────

  const handleSaveCreative = async () => {
    if (!formData.creative_name || !formData.offer_id) return;
    setFormSaving(true);
    try {
      const body = {
        offer_id: formData.offer_id,
        offer_name: formData.offer_name,
        creative_name: formData.creative_name,
        html_content: formData.html_content,
        text_content: formData.text_content,
        source: formData.source,
        tags: formData.tags.split(',').map(t => t.trim()).filter(Boolean)
      };
      const res = await fetch('/api/mailing/offer-center/creatives', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          ...(orgId ? { 'X-Organization-ID': orgId } : {})
        },
        body: JSON.stringify(body)
      });
      if (!res.ok) throw new Error('Save failed');
      setShowForm(false);
      setFormData({ offer_id: '', offer_name: '', creative_name: '', html_content: '', text_content: '', source: 'manual', tags: '' });
      fetchCreatives();
    } catch {
      // Error handled silently – form stays open
    } finally {
      setFormSaving(false);
    }
  };

  // ─── Delete Creative ────────────────────────────────────────────────────

  const handleDeleteCreative = async (id: string) => {
    try {
      const res = await fetch(`/api/mailing/offer-center/creatives/${id}`, {
        method: 'DELETE',
        headers: {
          'Content-Type': 'application/json',
          ...(orgId ? { 'X-Organization-ID': orgId } : {})
        }
      });
      if (!res.ok) throw new Error('Delete failed');
      setCreatives(prev => prev.filter(c => c.id !== id));
    } catch {
      // Silently handle
    }
  };

  // ─── Sorted Offers ──────────────────────────────────────────────────────

  const sortedOffers = compData && compData.offers
    ? [...compData.offers].sort((a, b) => {
        if (compSort === 'revenue') return b.your_revenue - a.your_revenue;
        if (compSort === 'epc') return b.your_epc - a.your_epc;
        return b.delta_epc - a.delta_epc;
      })
    : [];

  // ─── Unique offers for filter dropdown ──────────────────────────────────

  const uniqueOffers = creatives.reduce<{ id: string; name: string }[]>((acc, c) => {
    if (!acc.find(o => o.id === c.offer_id)) acc.push({ id: c.offer_id, name: c.offer_name });
    return acc;
  }, []);

  // ─── Render ─────────────────────────────────────────────────────────────

  return (
    <div className="oc-container">
      {/* Header */}
      <div className="oc-header">
        <div className="oc-header-left">
          <div className="oc-header-icon">
            <FontAwesomeIcon icon={faStore} />
          </div>
          <div>
            <h1>Offer Center</h1>
            <p>Network intelligence, AI suggestions, and creative management</p>
          </div>
        </div>
        <div className="oc-header-right">
          <button
            className="oc-refresh-btn"
            onClick={() => {
              if (activeTab === 'comparison') fetchComparison();
              else if (activeTab === 'suggestions') fetchSuggestions();
              else if (activeTab === 'creatives') fetchCreatives();
              else fetchTopOffers();
            }}
            disabled={compLoading || sugLoading || creativesLoading || topLoading}
          >
            <FontAwesomeIcon icon={faSyncAlt} spin={compLoading || sugLoading || creativesLoading || topLoading} />
            Refresh
          </button>
        </div>
      </div>

      {/* Tabs */}
      <div className="oc-tabs">
        <button className={`oc-tab${activeTab === 'comparison' ? ' active' : ''}`} onClick={() => setActiveTab('comparison')}>
          <FontAwesomeIcon icon={faChartLine} /> Network Comparison
        </button>
        <button className={`oc-tab${activeTab === 'suggestions' ? ' active' : ''}`} onClick={() => setActiveTab('suggestions')}>
          <FontAwesomeIcon icon={faLightbulb} /> AI Suggestions
        </button>
        <button className={`oc-tab${activeTab === 'creatives' ? ' active' : ''}`} onClick={() => setActiveTab('creatives')}>
          <FontAwesomeIcon icon={faPalette} /> Creative Library
        </button>
        <button className={`oc-tab${activeTab === 'top' ? ' active' : ''}`} onClick={() => setActiveTab('top')}>
          <FontAwesomeIcon icon={faGlobe} /> Top Network Offers
        </button>
        <button className={`oc-tab${activeTab === 'performance' ? ' active' : ''}`} onClick={() => setActiveTab('performance')}>
          <FontAwesomeIcon icon={faChartLine} /> Performance History
        </button>
      </div>

      {/* ═══════ TAB 1: Network Comparison ═══════ */}
      {activeTab === 'comparison' && (
        <div>
          {/* Range + Sort controls */}
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16, flexWrap: 'wrap', gap: 12 }}>
            <div className="oc-range-selector">
              {([
                { value: 'today' as TimeRange, label: 'Today' },
                { value: '7' as TimeRange, label: '7d' },
                { value: '14' as TimeRange, label: '14d' },
                { value: 'mtd' as TimeRange, label: 'MTD' },
              ]).map(r => (
                <button key={r.value} className={compRange === r.value ? 'active' : ''} onClick={() => setCompRange(r.value)}>
                  {r.label}
                </button>
              ))}
            </div>
            <div className="oc-sort-bar">
              <FontAwesomeIcon icon={faFilter} />
              <span>Sort:</span>
              {([['revenue', 'Revenue'], ['epc', 'EPC'], ['delta_epc', 'Delta']] as [SortField, string][]).map(([key, label]) => (
                <button key={key} className={`oc-sort-btn${compSort === key ? ' active' : ''}`} onClick={() => setCompSort(key)}>
                  {label}
                </button>
              ))}
            </div>
          </div>

          {compLoading && (
            <div className="oc-loading">
              <FontAwesomeIcon icon={faSpinner} spin />
              <p>Loading comparison data…</p>
            </div>
          )}

          {compError && (
            <div className="oc-error">
              <p>{compError}</p>
              <button onClick={fetchComparison}>Retry</button>
            </div>
          )}

          {!compLoading && !compError && compData && (
            <>
              {/* Summary Bar */}
              <div className="oc-summary-bar">
                <div className="oc-summary-section yours">
                  <div className="oc-summary-label yours-label">
                    <FontAwesomeIcon icon={faChartLine} /> Your Team Totals
                  </div>
                  <div className="oc-summary-metrics">
                    <div className="oc-summary-metric">
                      <div className="oc-summary-value">{fmt(compData.your_totals.clicks)}</div>
                      <div className="oc-summary-metric-label">Clicks</div>
                    </div>
                    <div className="oc-summary-metric">
                      <div className="oc-summary-value">{fmt(compData.your_totals.conversions)}</div>
                      <div className="oc-summary-metric-label">Conversions</div>
                    </div>
                    <div className="oc-summary-metric">
                      <div className="oc-summary-value">{fmtCurrency(compData.your_totals.revenue)}</div>
                      <div className="oc-summary-metric-label">Revenue</div>
                    </div>
                    <div className="oc-summary-metric">
                      <div className="oc-summary-value">{fmtCurrency(compData.your_totals.epc)}</div>
                      <div className="oc-summary-metric-label">EPC</div>
                    </div>
                    <div className="oc-summary-metric">
                      <div className="oc-summary-value">{pct(compData.your_totals.cvr)}</div>
                      <div className="oc-summary-metric-label">CVR</div>
                    </div>
                  </div>
                </div>
                <div className="oc-summary-section network">
                  <div className="oc-summary-label network-label">
                    <FontAwesomeIcon icon={faGlobe} /> Network Totals
                  </div>
                  <div className="oc-summary-metrics">
                    <div className="oc-summary-metric">
                      <div className="oc-summary-value">{fmt(compData.network_totals.clicks)}</div>
                      <div className="oc-summary-metric-label">Clicks</div>
                    </div>
                    <div className="oc-summary-metric">
                      <div className="oc-summary-value">{fmt(compData.network_totals.conversions)}</div>
                      <div className="oc-summary-metric-label">Conversions</div>
                    </div>
                    <div className="oc-summary-metric">
                      <div className="oc-summary-value">{fmtCurrency(compData.network_totals.revenue)}</div>
                      <div className="oc-summary-metric-label">Revenue</div>
                    </div>
                    <div className="oc-summary-metric">
                      <div className="oc-summary-value">{fmtCurrency(compData.network_totals.epc)}</div>
                      <div className="oc-summary-metric-label">EPC</div>
                    </div>
                    <div className="oc-summary-metric">
                      <div className="oc-summary-value">{pct(compData.network_totals.cvr)}</div>
                      <div className="oc-summary-metric-label">CVR</div>
                    </div>
                  </div>
                </div>
              </div>

              {/* Comparison Table */}
              {sortedOffers.length === 0 ? (
                <div className="oc-empty">
                  <FontAwesomeIcon icon={faChartLine} />
                  <p>No comparison data available for this period.</p>
                </div>
              ) : (
                <div className="oc-table-wrap">
                  <table className="oc-table">
                    <thead>
                      <tr>
                        <th rowSpan={2} style={{ verticalAlign: 'bottom' }}>Offer</th>
                        <th rowSpan={2} style={{ verticalAlign: 'bottom' }}>Type</th>
                        <th colSpan={4} className="oc-th-group network-group">Network</th>
                        <th colSpan={4} className="oc-th-group yours-group">Your Team</th>
                        <th colSpan={2} className="oc-th-group delta-group">Delta</th>
                        <th rowSpan={2} style={{ verticalAlign: 'bottom' }}></th>
                      </tr>
                      <tr>
                        <th>Clicks</th><th>Conv</th><th>Revenue</th><th>EPC</th>
                        <th>Clicks</th><th>Conv</th><th>Revenue</th><th>EPC</th>
                        <th>EPC</th><th>CVR</th>
                      </tr>
                    </thead>
                    <tbody>
                      {sortedOffers.map(o => (
                        <tr key={o.offer_id}>
                          <td><span className="oc-offer-name" title={o.offer_name}>{o.offer_name}</span></td>
                          <td><span className={`oc-type-badge ${typeBadgeClass(o.offer_type)}`}>{o.offer_type}</span></td>
                          <td>{fmt(o.network_clicks)}</td>
                          <td>{fmt(o.network_conversions)}</td>
                          <td>{fmtCurrency(o.network_revenue)}</td>
                          <td>{fmtCurrency(o.network_epc)}</td>
                          <td>{fmt(o.your_clicks)}</td>
                          <td>{fmt(o.your_conversions)}</td>
                          <td>{fmtCurrency(o.your_revenue)}</td>
                          <td>{fmtCurrency(o.your_epc)}</td>
                          <td>
                            <span className={`oc-delta ${o.delta_epc > 0 ? 'positive' : o.delta_epc < 0 ? 'negative' : 'neutral'}`}>
                              {o.delta_epc !== 0 && <FontAwesomeIcon icon={o.delta_epc > 0 ? faArrowUp : faArrowDown} />}
                              {fmtCurrency(Math.abs(o.delta_epc))}
                            </span>
                          </td>
                          <td>
                            <span className={`oc-delta ${o.delta_cvr > 0 ? 'positive' : o.delta_cvr < 0 ? 'negative' : 'neutral'}`}>
                              {o.delta_cvr !== 0 && <FontAwesomeIcon icon={o.delta_cvr > 0 ? faArrowUp : faArrowDown} />}
                              {pct(Math.abs(o.delta_cvr))}
                            </span>
                          </td>
                          <td style={{ whiteSpace: 'nowrap' }}>
                            <button
                              className="oc-use-btn-sm"
                              onClick={() => showPerformance(o.offer_id)}
                              title="View offer performance history"
                              style={{ marginRight: 4 }}
                            >
                              <FontAwesomeIcon icon={faEye} />
                            </button>
                            <button
                              className="oc-use-btn-sm"
                              onClick={() => onUseOffer?.(o.offer_id, o.offer_name)}
                              title="Create campaign with this offer"
                            >
                              <FontAwesomeIcon icon={faExternalLinkAlt} /> Mail
                            </button>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </>
          )}
        </div>
      )}

      {/* ═══════ TAB 2: AI Suggestions ═══════ */}
      {activeTab === 'suggestions' && (
        <div>
          <div className="oc-suggestions-header">
            <h2><FontAwesomeIcon icon={faLightbulb} /> What Should You Mail Today?</h2>
            <p>AI-powered offer recommendations based on network performance and your sending history</p>
          </div>

          {sugLoading && (
            <div className="oc-loading">
              <FontAwesomeIcon icon={faSpinner} spin />
              <p>Analyzing offers…</p>
            </div>
          )}

          {sugError && (
            <div className="oc-error">
              <p>{sugError}</p>
              <button onClick={fetchSuggestions}>Retry</button>
            </div>
          )}

          {!sugLoading && !sugError && suggestions.length === 0 && (
            <div className="oc-empty">
              <FontAwesomeIcon icon={faLightbulb} />
              <p>No suggestions available right now. Check back later.</p>
            </div>
          )}

          {!sugLoading && !sugError && suggestions.length > 0 && (
            <div className="oc-suggestions-grid">
              {suggestions.map(s => (
                <div className="oc-suggestion-card" key={s.offer_id}>
                  <div className="oc-suggestion-top">
                    <span className="oc-suggestion-name">{s.offer_name}</span>
                    <span className={`oc-sug-badge ${sugBadgeClass(s.type)}`}>
                      <FontAwesomeIcon icon={sugIcon(s.type)} />
                      {s.type.replace('_', ' ')}
                    </span>
                  </div>

                  <p className="oc-sug-reasoning">{s.reasoning}</p>

                  <div className="oc-sug-metrics">
                    <div className="oc-sug-metric">
                      <div className="oc-sug-metric-value">{fmtCurrency(s.metrics?.network_epc)}</div>
                      <div className="oc-sug-metric-label">Network EPC</div>
                    </div>
                    <div className="oc-sug-metric">
                      <div className="oc-sug-metric-value">{pct(s.metrics?.network_cvr)}</div>
                      <div className="oc-sug-metric-label">Network CVR</div>
                    </div>
                    {(s.metrics?.your_epc ?? 0) > 0 && (
                      <div className="oc-sug-metric">
                        <div className="oc-sug-metric-value">{fmtCurrency(s.metrics.your_epc)}</div>
                        <div className="oc-sug-metric-label">Your EPC</div>
                      </div>
                    )}
                    {(s.metrics?.your_cvr ?? 0) > 0 && (
                      <div className="oc-sug-metric">
                        <div className="oc-sug-metric-value">{pct(s.metrics.your_cvr)}</div>
                        <div className="oc-sug-metric-label">Your CVR</div>
                      </div>
                    )}
                  </div>

                  <div className="oc-score-bar">
                    <div className="oc-score-track">
                      <div
                        className={`oc-score-fill ${scoreTier(s.score)}`}
                        style={{ width: `${Math.min(s.score, 100)}%` }}
                      />
                    </div>
                    <span className="oc-score-value">{s.score}</span>
                  </div>

                  <button
                    className="oc-use-btn"
                    onClick={() => onUseOffer?.(s.offer_id, s.offer_name)}
                  >
                    <FontAwesomeIcon icon={faExternalLinkAlt} /> Use This Offer
                  </button>
                </div>
              ))}
            </div>
          )}
        </div>
      )}

      {/* ═══════ TAB 3: Creative Library ═══════ */}
      {activeTab === 'creatives' && (
        <div>
          {/* Toolbar */}
          <div className="oc-creative-toolbar">
            <div className="oc-search-wrap">
              <FontAwesomeIcon icon={faSearch} />
              <input
                className="oc-search-input"
                placeholder="Search creatives…"
                value={creativeSearch}
                onChange={e => setCreativeSearch(e.target.value)}
              />
            </div>
            <select
              className="oc-filter-select"
              value={creativeFilter}
              onChange={e => setCreativeFilter(e.target.value)}
            >
              <option value="">All Offers</option>
              {uniqueOffers.map(o => (
                <option key={o.id} value={o.id}>{o.name}</option>
              ))}
            </select>
            <button className="oc-add-creative-btn" onClick={() => setShowForm(true)}>
              <FontAwesomeIcon icon={faPlus} /> Save New Creative
            </button>
          </div>

          {creativesLoading && (
            <div className="oc-loading">
              <FontAwesomeIcon icon={faSpinner} spin />
              <p>Loading creatives…</p>
            </div>
          )}

          {creativesError && (
            <div className="oc-error">
              <p>{creativesError}</p>
              <button onClick={fetchCreatives}>Retry</button>
            </div>
          )}

          {!creativesLoading && !creativesError && creatives.length === 0 && (
            <div className="oc-empty">
              <FontAwesomeIcon icon={faPalette} />
              <p>No creatives found. Save your first creative to get started.</p>
            </div>
          )}

          {!creativesLoading && !creativesError && creatives.length > 0 && (
            <div className="oc-creative-grid">
              {creatives.map(c => (
                <div className="oc-creative-card" key={c.id}>
                  <div className="oc-creative-header">
                    <span className="oc-creative-name">{c.creative_name}</span>
                    <div className="oc-creative-badges">
                      <span className={`oc-source-badge oc-source-${c.source}`}>
                        {c.source.replace('_', ' ')}
                      </span>
                      {c.ai_optimized && (
                        <span className="oc-source-badge oc-ai-badge">AI</span>
                      )}
                    </div>
                  </div>
                  <div className="oc-creative-offer">{c.offer_name}</div>
                  <div className="oc-creative-meta">
                    <span><FontAwesomeIcon icon={faEye} /> {c.use_count} uses</span>
                    <span>{fmtDate(c.created_at)}</span>
                  </div>
                  {c.tags && c.tags.length > 0 && (
                    <div className="oc-creative-tags">
                      {c.tags.map(tag => (
                        <span className="oc-tag" key={tag}>{tag}</span>
                      ))}
                    </div>
                  )}
                  <div className="oc-creative-actions">
                    <button className="oc-preview-btn" onClick={() => setPreviewCreative(c)}>
                      <FontAwesomeIcon icon={faEye} /> Preview
                    </button>
                    <button className="oc-delete-btn" onClick={() => handleDeleteCreative(c.id)}>
                      <FontAwesomeIcon icon={faTrash} />
                    </button>
                  </div>
                </div>
              ))}
            </div>
          )}

          {/* New Creative Modal */}
          {showForm && (
            <div className="oc-modal-backdrop" onClick={() => setShowForm(false)}>
              <div className="oc-modal" onClick={e => e.stopPropagation()}>
                <h3>
                  Save New Creative
                  <button className="oc-modal-close" onClick={() => setShowForm(false)}>
                    <FontAwesomeIcon icon={faTimes} />
                  </button>
                </h3>
                <div className="oc-form-group">
                  <label>Offer ID</label>
                  <input
                    className="oc-form-input"
                    value={formData.offer_id}
                    onChange={e => setFormData(p => ({ ...p, offer_id: e.target.value }))}
                    placeholder="e.g. 12345"
                  />
                </div>
                <div className="oc-form-group">
                  <label>Offer Name</label>
                  <input
                    className="oc-form-input"
                    value={formData.offer_name}
                    onChange={e => setFormData(p => ({ ...p, offer_name: e.target.value }))}
                    placeholder="Offer name"
                  />
                </div>
                <div className="oc-form-group">
                  <label>Creative Name</label>
                  <input
                    className="oc-form-input"
                    value={formData.creative_name}
                    onChange={e => setFormData(p => ({ ...p, creative_name: e.target.value }))}
                    placeholder="My Email Creative v1"
                  />
                </div>
                <div className="oc-form-group">
                  <label>HTML Content</label>
                  <textarea
                    className="oc-form-textarea"
                    value={formData.html_content}
                    onChange={e => setFormData(p => ({ ...p, html_content: e.target.value }))}
                    placeholder="Paste HTML email content…"
                  />
                </div>
                <div className="oc-form-group">
                  <label>Text Content</label>
                  <textarea
                    className="oc-form-textarea"
                    value={formData.text_content}
                    onChange={e => setFormData(p => ({ ...p, text_content: e.target.value }))}
                    placeholder="Plain text version…"
                  />
                </div>
                <div className="oc-form-group">
                  <label>Source</label>
                  <select
                    className="oc-form-input"
                    value={formData.source}
                    onChange={e => setFormData(p => ({ ...p, source: e.target.value }))}
                  >
                    <option value="manual">Manual</option>
                    <option value="everflow">Everflow</option>
                    <option value="ai_generated">AI Generated</option>
                  </select>
                </div>
                <div className="oc-form-group">
                  <label>Tags (comma separated)</label>
                  <input
                    className="oc-form-input"
                    value={formData.tags}
                    onChange={e => setFormData(p => ({ ...p, tags: e.target.value }))}
                    placeholder="health, weight-loss, email"
                  />
                </div>
                <div className="oc-form-actions">
                  <button className="oc-form-cancel" onClick={() => setShowForm(false)}>Cancel</button>
                  <button
                    className="oc-form-submit"
                    disabled={formSaving || !formData.creative_name || !formData.offer_id}
                    onClick={handleSaveCreative}
                  >
                    {formSaving ? <><FontAwesomeIcon icon={faSpinner} spin /> Saving…</> : 'Save Creative'}
                  </button>
                </div>
              </div>
            </div>
          )}

          {/* Preview Modal */}
          {previewCreative && (
            <div className="oc-modal-backdrop" onClick={() => setPreviewCreative(null)}>
              <div className="oc-preview-modal" onClick={e => e.stopPropagation()}>
                <h3 style={{ margin: '0 0 16px', fontSize: 18, fontWeight: 700, color: '#fff', display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                  {previewCreative.creative_name}
                  <button className="oc-modal-close" onClick={() => setPreviewCreative(null)}>
                    <FontAwesomeIcon icon={faTimes} />
                  </button>
                </h3>
                {previewCreative.html_content && (
                  <div
                    className="oc-preview-content"
                    dangerouslySetInnerHTML={{ __html: previewCreative.html_content }}
                  />
                )}
                {previewCreative.text_content && (
                  <div className="oc-preview-text">{previewCreative.text_content}</div>
                )}
                {!previewCreative.html_content && !previewCreative.text_content && (
                  <div className="oc-empty">
                    <p>No content available for this creative.</p>
                  </div>
                )}
              </div>
            </div>
          )}
        </div>
      )}

      {/* ═══════ TAB 4: Top Network Offers ═══════ */}
      {activeTab === 'top' && (
        <div>
          {topLoading && (
            <div className="oc-loading">
              <FontAwesomeIcon icon={faSpinner} spin />
              <p>Loading top network offers…</p>
            </div>
          )}

          {topError && (
            <div className="oc-error">
              <p>{topError}</p>
              <button onClick={fetchTopOffers}>Retry</button>
            </div>
          )}

          {!topLoading && !topError && topOffers.length === 0 && (
            <div className="oc-empty">
              <FontAwesomeIcon icon={faGlobe} />
              <p>No top offers data available.</p>
            </div>
          )}

          {!topLoading && !topError && topOffers.length > 0 && (
            <div className="oc-top-grid">
              {topOffers.map(o => (
                <div className="oc-top-card" key={o.offer_id}>
                  <div className="oc-top-header">
                    <span className="oc-top-name">{o.offer_name}</span>
                    <span className={`oc-type-badge ${typeBadgeClass(o.offer_type)}`}>{o.offer_type}</span>
                  </div>

                  <div className="oc-top-metrics">
                    <div className="oc-top-metric">
                      <div className="oc-top-metric-value">{fmtCurrency(o.network_revenue)}</div>
                      <div className="oc-top-metric-label">Revenue</div>
                    </div>
                    <div className="oc-top-metric">
                      <div className="oc-top-metric-value">{fmtCurrency(o.epc)}</div>
                      <div className="oc-top-metric-label">EPC</div>
                    </div>
                    <div className="oc-top-metric">
                      <div className="oc-top-metric-value">{pct(o.cvr)}</div>
                      <div className="oc-top-metric-label">CVR</div>
                    </div>
                  </div>

                  <div className="oc-top-ai-row">
                    <div className="oc-ai-score">
                      <FontAwesomeIcon icon={faStar} />
                      <span className="oc-ai-score-val">{o.ai_score}</span>
                    </div>
                    <span className={`oc-trend-badge oc-trend-${o.trend === 'up' ? 'up' : o.trend === 'down' ? 'down' : 'stable'}`}>
                      <FontAwesomeIcon icon={o.trend === 'up' ? faArrowUp : o.trend === 'down' ? faArrowDown : faChartLine} />
                      {o.trend}
                    </span>
                  </div>

                  {o.ai_recommendation && (
                    <div className="oc-ai-rec">
                      <FontAwesomeIcon icon={faLightbulb} />
                      {o.ai_recommendation}
                    </div>
                  )}

                  <button
                    className="oc-use-btn"
                    onClick={() => onUseOffer?.(o.offer_id, o.offer_name)}
                  >
                    <FontAwesomeIcon icon={faExternalLinkAlt} /> Mail This Offer
                  </button>
                </div>
              ))}
            </div>
          )}
        </div>
      )}

      {/* ═══════ TAB 5: Performance Terminal ═══════ */}
      {activeTab === 'performance' && (
        <div className="oc-terminal">
          {/* Offer Picker */}
          <div className="oc-perf-picker">
            <label><FontAwesomeIcon icon={faCrosshairs} /> Instrument:</label>
            <select
              className="oc-filter-select"
              value={perfOfferId}
              onChange={e => { setPerfOfferId(e.target.value); fetchPerformance(e.target.value); }}
            >
              <option value="">— Select offer to analyze —</option>
              {(compData?.offers || []).map(o => (
                <option key={o.offer_id} value={o.offer_id}>{o.offer_name}</option>
              ))}
            </select>
          </div>

          {perfLoading && (
            <div className="oc-loading">
              <FontAwesomeIcon icon={faSpinner} spin />
              <p>Fetching market data…</p>
            </div>
          )}

          {perfError && (
            <div className="oc-error">
              <p>{perfError}</p>
              <button onClick={() => fetchPerformance(perfOfferId)}>Retry</button>
            </div>
          )}

          {!perfLoading && !perfError && !perfData && !perfOfferId && (
            <div className="oc-empty">
              <FontAwesomeIcon icon={faChartLine} />
              <p>Select an instrument above or click the eye icon in the Comparison tab to open the performance terminal.</p>
            </div>
          )}

          {!perfLoading && !perfError && perfData && (() => {
            const stats = computeStats(perfData.daily_trend || []);
            const trend = perfData.daily_trend || [];
            const team = perfData.your_team;
            const net = perfData.network;

            // Helper to get chart values for the selected metric
            const chartValues = trend.map(d => {
              switch (perfChartMetric) {
                case 'revenue': return d.revenue;
                case 'clicks': return d.clicks;
                case 'epc': return d.epc;
                case 'cvr': return d.cvr;
                case 'profit': return d.profit;
                default: return d.revenue;
              }
            });
            const maxChart = Math.max(...chartValues, 0.01);
            const minChart = Math.min(...chartValues, 0);
            const chartRange = maxChart - Math.min(minChart, 0);

            const chartFmt = (v: number) => {
              if (perfChartMetric === 'cvr') return v.toFixed(1) + '%';
              if (perfChartMetric === 'clicks') return fmt(v);
              return fmtCurrency(v);
            };

            return (
            <div className="oc-perf-content">

              {/* ── Ticker Bar ── */}
              <div className="oc-ticker-bar">
                <div className="oc-ticker-name">
                  <h3>{perfData.offer_name || `Offer ${perfData.offer_id}`}</h3>
                  {perfData.offer_type && (
                    <span className={`oc-type-badge ${typeBadgeClass(perfData.offer_type)}`}>{perfData.offer_type}</span>
                  )}
                </div>
                {stats && (
                  <div className="oc-ticker-signals">
                    <span className={`oc-signal oc-signal-${stats.momentum}`}>
                      <FontAwesomeIcon icon={stats.momentum === 'bullish' ? faCaretUp : stats.momentum === 'bearish' ? faCaretDown : faMinus} />
                      {stats.momentum.toUpperCase()}
                    </span>
                    <span className={`oc-signal oc-signal-vol-${stats.volLevel}`}>
                      VOL: {stats.volLevel.toUpperCase()}
                    </span>
                    {stats.streak > 0 && (
                      <span className={`oc-signal oc-signal-${stats.streakDir}`}>
                        <FontAwesomeIcon icon={stats.streakDir === 'up' ? faCaretUp : faCaretDown} />
                        {stats.streak}d STREAK
                      </span>
                    )}
                    <span className="oc-signal oc-signal-days">
                      {stats.tradingDays} TRADING DAYS
                    </span>
                  </div>
                )}
              </div>

              {/* ── KPI Grid (Your Team) ── */}
              {team && (
                <div className="oc-kpi-grid">
                  <div className="oc-kpi-card oc-kpi-revenue">
                    <div className="oc-kpi-icon"><FontAwesomeIcon icon={faDollarSign} /></div>
                    <div className="oc-kpi-body">
                      <div className="oc-kpi-val">{fmtCurrency(team.revenue)}</div>
                      <div className="oc-kpi-lbl">Revenue</div>
                    </div>
                  </div>
                  <div className="oc-kpi-card oc-kpi-payout">
                    <div className="oc-kpi-icon"><FontAwesomeIcon icon={faDollarSign} /></div>
                    <div className="oc-kpi-body">
                      <div className="oc-kpi-val">{fmtCurrency(team.payout)}</div>
                      <div className="oc-kpi-lbl">Payout</div>
                    </div>
                  </div>
                  <div className={`oc-kpi-card ${team.profit >= 0 ? 'oc-kpi-profit-pos' : 'oc-kpi-profit-neg'}`}>
                    <div className="oc-kpi-icon"><FontAwesomeIcon icon={team.profit >= 0 ? faCaretUp : faCaretDown} /></div>
                    <div className="oc-kpi-body">
                      <div className="oc-kpi-val">{fmtCurrency(team.profit)}</div>
                      <div className="oc-kpi-lbl">Profit ({pct(team.margin)} margin)</div>
                    </div>
                  </div>
                  <div className="oc-kpi-card oc-kpi-clicks">
                    <div className="oc-kpi-icon"><FontAwesomeIcon icon={faCrosshairs} /></div>
                    <div className="oc-kpi-body">
                      <div className="oc-kpi-val">{fmt(team.clicks)}</div>
                      <div className="oc-kpi-lbl">Clicks{team.unique_clicks ? ` (${fmt(team.unique_clicks)} uniq)` : ''}</div>
                    </div>
                  </div>
                  <div className="oc-kpi-card oc-kpi-conv">
                    <div className="oc-kpi-icon"><FontAwesomeIcon icon={faChartBar} /></div>
                    <div className="oc-kpi-body">
                      <div className="oc-kpi-val">{fmt(team.conversions)}</div>
                      <div className="oc-kpi-lbl">Conversions</div>
                    </div>
                  </div>
                  <div className="oc-kpi-card oc-kpi-epc">
                    <div className="oc-kpi-icon"><FontAwesomeIcon icon={faPercent} /></div>
                    <div className="oc-kpi-body">
                      <div className="oc-kpi-val">{fmtCurrency(team.epc)}</div>
                      <div className="oc-kpi-lbl">EPC</div>
                    </div>
                  </div>
                  <div className="oc-kpi-card oc-kpi-cvr">
                    <div className="oc-kpi-icon"><FontAwesomeIcon icon={faPercent} /></div>
                    <div className="oc-kpi-body">
                      <div className="oc-kpi-val">{pct(team.cvr)}</div>
                      <div className="oc-kpi-lbl">CVR</div>
                    </div>
                  </div>
                  <div className="oc-kpi-card oc-kpi-rpc">
                    <div className="oc-kpi-icon"><FontAwesomeIcon icon={faDollarSign} /></div>
                    <div className="oc-kpi-body">
                      <div className="oc-kpi-val">{fmtCurrency(team.rpc)}</div>
                      <div className="oc-kpi-lbl">Rev/Conv</div>
                    </div>
                  </div>
                </div>
              )}

              {/* ── Network Benchmark (compact) ── */}
              {net && (
                <div className="oc-net-bench">
                  <div className="oc-net-bench-label"><FontAwesomeIcon icon={faGlobe} /> NETWORK BENCHMARK</div>
                  <div className="oc-net-bench-row">
                    <span>{fmt(net.clicks)} clicks</span>
                    <span className="oc-net-sep">|</span>
                    <span>{fmt(net.conversions)} conv</span>
                    <span className="oc-net-sep">|</span>
                    <span>{fmtCurrency(net.revenue)} rev</span>
                    <span className="oc-net-sep">|</span>
                    <span>{fmtCurrency(net.epc)} EPC</span>
                    <span className="oc-net-sep">|</span>
                    <span>{pct(net.cvr)} CVR</span>
                    {net.rpc > 0 && <><span className="oc-net-sep">|</span><span>{fmtCurrency(net.rpc)} R/Conv</span></>}
                  </div>
                  {team && (
                    <div className="oc-net-delta-row">
                      <span className={team.epc >= net.epc ? 'oc-delta-pos' : 'oc-delta-neg'}>
                        <FontAwesomeIcon icon={team.epc >= net.epc ? faCaretUp : faCaretDown} />
                        EPC {net.epc > 0 ? fmtPct((team.epc - net.epc) / net.epc * 100) : '—'} vs network
                      </span>
                      <span className={team.cvr >= net.cvr ? 'oc-delta-pos' : 'oc-delta-neg'}>
                        <FontAwesomeIcon icon={team.cvr >= net.cvr ? faCaretUp : faCaretDown} />
                        CVR {net.cvr > 0 ? fmtPct((team.cvr - net.cvr) / net.cvr * 100) : '—'} vs network
                      </span>
                    </div>
                  )}
                </div>
              )}

              {!team && !net && (
                <div className="oc-empty"><p>No team or network performance data available for this offer.</p></div>
              )}

              {/* ── Chart Area ── */}
              {trend.length > 0 && (
                <div className="oc-chart-section">
                  <div className="oc-chart-header">
                    <h4><FontAwesomeIcon icon={faChartLine} /> Daily Performance</h4>
                    <div className="oc-chart-toggles">
                      {([
                        { key: 'revenue' as ChartMetric, label: 'Revenue' },
                        { key: 'profit' as ChartMetric, label: 'Profit' },
                        { key: 'clicks' as ChartMetric, label: 'Clicks' },
                        { key: 'epc' as ChartMetric, label: 'EPC' },
                        { key: 'cvr' as ChartMetric, label: 'CVR' },
                      ]).map(m => (
                        <button
                          key={m.key}
                          className={`oc-chart-toggle${perfChartMetric === m.key ? ' active' : ''}`}
                          onClick={() => setPerfChartMetric(m.key)}
                        >
                          {m.label}
                        </button>
                      ))}
                    </div>
                  </div>

                  {/* Y-axis labels + bars */}
                  <div className="oc-chart-container">
                    <div className="oc-chart-y-axis">
                      <span>{chartFmt(maxChart)}</span>
                      <span>{chartFmt(maxChart * 0.5)}</span>
                      <span>{chartFmt(0)}</span>
                    </div>
                    <div className="oc-chart-area">
                      {/* Grid lines */}
                      <div className="oc-chart-grid">
                        <div className="oc-chart-gridline" style={{ bottom: '100%' }} />
                        <div className="oc-chart-gridline" style={{ bottom: '50%' }} />
                        <div className="oc-chart-gridline" style={{ bottom: '0%' }} />
                      </div>
                      {/* Bars */}
                      <div className="oc-chart-bars">
                        {trend.map((d, i) => {
                          const val = chartValues[i];
                          const pctH = chartRange > 0 ? Math.max((val / chartRange) * 100, 1) : 2;
                          const isGreen = i > 0 ? val >= chartValues[i - 1] : true;
                          const prevVal = i > 0 ? chartValues[i - 1] : val;
                          const dayDelta = prevVal > 0 ? ((val - prevVal) / prevVal * 100) : 0;
                          return (
                            <div className="oc-chart-bar-col" key={i}
                              title={`${d.date}\n${chartFmt(val)}${i > 0 ? `\nΔ ${dayDelta >= 0 ? '+' : ''}${dayDelta.toFixed(1)}%` : ''}`}
                            >
                              <div
                                className={`oc-chart-bar ${isGreen ? 'bar-green' : 'bar-red'}`}
                                style={{ height: `${pctH}%` }}
                              />
                              <span className="oc-chart-bar-date">{d.date.split('-').slice(1).join('/')}</span>
                            </div>
                          );
                        })}
                      </div>
                    </div>
                  </div>
                </div>
              )}

              {/* ── Statistics Panel ── */}
              {stats && (
                <div className="oc-stats-panel">
                  <h4><FontAwesomeIcon icon={faChartBar} /> Analytics</h4>
                  <div className="oc-stats-grid">
                    <div className="oc-stat-item">
                      <div className="oc-stat-val">{fmtCurrency(stats.totalRevenue)}</div>
                      <div className="oc-stat-lbl">Total Revenue</div>
                    </div>
                    <div className="oc-stat-item">
                      <div className="oc-stat-val">{fmtCurrency(stats.totalProfit)}</div>
                      <div className="oc-stat-lbl">Total Profit</div>
                    </div>
                    <div className="oc-stat-item">
                      <div className="oc-stat-val">{fmtCurrency(stats.avgRevenue)}</div>
                      <div className="oc-stat-lbl">Avg Daily Revenue</div>
                    </div>
                    <div className="oc-stat-item">
                      <div className="oc-stat-val">{fmtCurrency(stats.medianRevenue)}</div>
                      <div className="oc-stat-lbl">Median Daily Rev</div>
                    </div>
                    <div className="oc-stat-item">
                      <div className="oc-stat-val">{fmtCurrency(stats.revenueStdDev)}</div>
                      <div className="oc-stat-lbl">Std Deviation</div>
                    </div>
                    <div className="oc-stat-item">
                      <div className="oc-stat-val">{stats.volatility.toFixed(1)}%</div>
                      <div className="oc-stat-lbl">Volatility (CV)</div>
                    </div>
                    <div className="oc-stat-item oc-stat-best">
                      <div className="oc-stat-val">{fmtCurrency(stats.bestDay.revenue)}</div>
                      <div className="oc-stat-lbl">Best Day ({stats.bestDay.date})</div>
                    </div>
                    <div className="oc-stat-item oc-stat-worst">
                      <div className="oc-stat-val">{fmtCurrency(stats.worstDay.revenue)}</div>
                      <div className="oc-stat-lbl">Worst Day ({stats.worstDay.date})</div>
                    </div>
                    <div className="oc-stat-item">
                      <div className="oc-stat-val">{fmtPct(stats.avgDodChange)}</div>
                      <div className="oc-stat-lbl">Avg Day/Day Δ</div>
                    </div>
                    <div className="oc-stat-item">
                      <div className="oc-stat-val">{fmtPct(stats.momentumPct)}</div>
                      <div className="oc-stat-lbl">Momentum (recent vs prior)</div>
                    </div>
                    <div className="oc-stat-item">
                      <div className="oc-stat-val">{fmt(stats.totalClicks)}</div>
                      <div className="oc-stat-lbl">Total Clicks</div>
                    </div>
                    <div className="oc-stat-item">
                      <div className="oc-stat-val">{stats.tradingDays}</div>
                      <div className="oc-stat-lbl">Trading Days</div>
                    </div>
                  </div>
                </div>
              )}

              {/* ── Daily Ledger Table ── */}
              {trend.length > 0 && (
                <div className="oc-ledger-section">
                  <h4><FontAwesomeIcon icon={faChartLine} /> Daily Ledger</h4>
                  <div className="oc-table-wrap">
                    <table className="oc-table oc-ledger-table">
                      <thead>
                        <tr>
                          <th>Date</th>
                          <th>Revenue</th>
                          <th>Payout</th>
                          <th>Profit</th>
                          <th>Clicks</th>
                          <th>Conv</th>
                          <th>EPC</th>
                          <th>CVR</th>
                          <th>Day Δ</th>
                        </tr>
                      </thead>
                      <tbody>
                        {[...trend].reverse().map((d, i, arr) => {
                          const prevIdx = i + 1; // reversed: next index = previous day
                          const prev = prevIdx < arr.length ? arr[prevIdx] : null;
                          const dayDelta = prev && prev.revenue > 0 ? ((d.revenue - prev.revenue) / prev.revenue * 100) : null;
                          const isGreen = dayDelta !== null ? dayDelta >= 0 : true;
                          return (
                            <tr key={i} className={`oc-ledger-row ${isGreen ? 'ledger-green' : 'ledger-red'}`}>
                              <td className="oc-ledger-date">{d.date}</td>
                              <td className="oc-ledger-num">{fmtCurrency(d.revenue)}</td>
                              <td className="oc-ledger-num">{fmtCurrency(d.payout)}</td>
                              <td className={`oc-ledger-num ${d.profit >= 0 ? 'oc-ledger-profit-pos' : 'oc-ledger-profit-neg'}`}>{fmtCurrency(d.profit)}</td>
                              <td className="oc-ledger-num">{fmt(d.clicks)}</td>
                              <td className="oc-ledger-num">{fmt(d.conversions)}</td>
                              <td className="oc-ledger-num">{fmtCurrency(d.epc)}</td>
                              <td className="oc-ledger-num">{pct(d.cvr)}</td>
                              <td className={`oc-ledger-delta ${isGreen ? 'oc-delta-pos' : 'oc-delta-neg'}`}>
                                {dayDelta !== null ? (
                                  <>
                                    <FontAwesomeIcon icon={isGreen ? faCaretUp : faCaretDown} />
                                    {Math.abs(dayDelta).toFixed(1)}%
                                  </>
                                ) : '—'}
                              </td>
                            </tr>
                          );
                        })}
                      </tbody>
                    </table>
                  </div>
                </div>
              )}

              {/* Action */}
              <div style={{ marginTop: 20, textAlign: 'right' }}>
                <button
                  className="oc-use-btn"
                  onClick={() => onUseOffer?.(perfData.offer_id, perfData.offer_name)}
                >
                  <FontAwesomeIcon icon={faExternalLinkAlt} /> Create Campaign with This Offer
                </button>
              </div>
            </div>
            );
          })()}
        </div>
      )}
    </div>
  );
};
