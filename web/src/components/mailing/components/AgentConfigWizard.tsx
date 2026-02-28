import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faStore, faMoneyBill, faChartLine, faUsers, faClock, faRocket,
  faChevronRight, faChevronLeft, faCheck, faSpinner, faBrain, faRobot,
  faBolt, faShieldAlt, faSearch, faExclamationCircle, faStar,
} from '@fortawesome/free-solid-svg-icons';
import { useAuth } from '../../../contexts/AuthContext';
import './AgentConfigWizard.css';

// ─── Types ───────────────────────────────────────────────────────────────────

interface ComparisonOffer {
  offer_id: string;
  offer_name: string;
  offer_type: string;
  network_revenue: number;
  network_epc: number;
  network_cvr: number;
}

interface OfferAnalysis {
  offer: { offer_id: string; offer_name: string; offer_type: string };
  network: { revenue: number; epc: number; cvr: number; clicks: number; conversions: number };
  your_team: { revenue: number; epc: number; cvr: number; clicks: number; conversions: number };
  local_history: { total_sends: number; total_opens: number; total_clicks: number; avg_open_rate: number; avg_click_rate: number };
  available_payout_types: string[];
}

interface RevenueProjection {
  target_revenue: number;
  ecpm_range: { low: number; high: number; mid: number };
  revenue_per_send: { low: number; mid: number; high: number };
  forecasted_volume: { conservative: number; expected: number; optimistic: number };
  target_audience: { conservative: number; expected: number; optimistic: number; bounce_buffer: string };
  projected_revenue: { at_low_ecpm: number; at_mid_ecpm: number; at_high_ecpm: number };
  formula: string;
  confidence: 'high' | 'medium' | 'low';
}

interface AudienceSegment {
  name: string;
  count: number;
}

interface MailingList {
  id: string;
  name: string;
  subscriber_count: number;
  active_count: number;
}

interface SuppressionList {
  id: string;
  name: string;
  entry_count: number;
  matched_count: number;
  is_global: boolean;
}

interface AudienceRecommendation {
  selected_segments: AudienceSegment[];
  total_recipients: number;
  net_recipients: number;
  engagement_quality: string;
  avg_engagement_score: number;
  suppression_estimate: number;
  suppression_checked: boolean;
  suppression_check_ms: number;
  mailing_lists: MailingList[] | null;
  suppression_lists: SuppressionList[] | null;
}

interface ISPScheduleEntry {
  isp: string;
  domain: string;
  recipient_count: number;
  optimal_hour: number;
  percentage: number;
}

interface SendWindowResult {
  window: { send_days: string[]; end_hour: number; timezone: string };
  isp_schedule: ISPScheduleEntry[];
  total_daily_capacity: number;
  estimated_days_to_complete: number;
}

interface DeployedAgent {
  agent_id: string;
  isp: string;
  domain: string;
  status: string;
  recipient_count: number;
  is_new: boolean;
}

interface DeployResult {
  deployed_agents: DeployedAgent[];
  total_agents: number;
  campaign_id: string;
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

const orgFetch = async (url: string, opts?: RequestInit) =>
  fetch(url, { ...opts, headers: { 'Content-Type': 'application/json', ...opts?.headers } });

const fmt = (n: number | undefined | null): string => {
  if (n == null || isNaN(n)) return '0';
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M';
  if (n >= 1_000) return (n / 1_000).toFixed(1) + 'K';
  return n.toLocaleString();
};

const fmtCurrency = (n: number | undefined | null): string => {
  if (n == null || isNaN(n)) return '$0.00';
  return '$' + n.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 });
};

// Precision-aware currency for sub-cent values (e.g. revenue per send = $0.0006)
const fmtPreciseCurrency = (n: number | undefined | null): string => {
  if (n == null || isNaN(n) || n === 0) return '$0.00';
  // For values >= $0.01, use standard 2 decimal places
  if (Math.abs(n) >= 0.01) return fmtCurrency(n);
  // For sub-cent values, show enough decimals to be meaningful (up to 6)
  const abs = Math.abs(n);
  let decimals = 4;
  if (abs < 0.0001) decimals = 6;
  else if (abs < 0.001) decimals = 5;
  return '$' + n.toFixed(decimals);
};

const pct = (n: number | undefined | null): string =>
  (n == null || isNaN(n)) ? '0.00%' : (n * 100).toFixed(2) + '%';

const STEP_LABELS = ['Offer', 'Payout', 'Revenue', 'Audience', 'Window', 'Deploy'];

const PAYOUT_DESCRIPTIONS: Record<string, { label: string; desc: string; icon: typeof faBolt }> = {
  CPS: { label: 'CPS', desc: 'Cost Per Sale — Performance based. Revenue on conversions.', icon: faBolt },
  CPM: { label: 'CPM', desc: 'Cost Per Mille — Volume based. Revenue per 1,000 impressions.', icon: faChartLine },
};

const PAYOUT_TIPS: Record<string, string> = {
  CPS: 'Performance-based: audience quality matters more than volume. Focus on engaged segments with high click-through history.',
  CPM: 'Volume-based: maximize deliverability and inbox placement. Wider audience with consistent sending patterns is key.',
};

const DAY_LABELS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];
const DAY_VALUES = ['monday', 'tuesday', 'wednesday', 'thursday', 'friday', 'saturday', 'sunday'];

const TIMEZONES = [
  { value: 'America/New_York', label: 'Eastern (ET)' },
  { value: 'America/Chicago', label: 'Central (CT)' },
  { value: 'America/Denver', label: 'Mountain (MT)' },
  { value: 'America/Los_Angeles', label: 'Pacific (PT)' },
];

// ─── Component ───────────────────────────────────────────────────────────────

export const AgentConfigWizard: React.FC<{
  onComplete: (config: any) => void;
  onCancel: () => void;
  initialOffer?: { offerId: string; offerName: string } | null;
  onOfferConsumed?: () => void;
}> = ({ onComplete, onCancel, initialOffer, onOfferConsumed }) => {
  const { organization } = useAuth();

  // Wizard state
  const [step, setStep] = useState(0);
  const [error, setError] = useState<string | null>(null);

  // Step 1 – Offer
  const [offers, setOffers] = useState<ComparisonOffer[]>([]);
  const [offersLoading, setOffersLoading] = useState(true);
  const [offerSearch, setOfferSearch] = useState('');
  const [selectedOffer, setSelectedOffer] = useState<ComparisonOffer | null>(null);
  const [analysis, setAnalysis] = useState<OfferAnalysis | null>(null);
  const [analysisLoading, setAnalysisLoading] = useState(false);

  // Step 2 – Payout
  const [payoutType, setPayoutType] = useState<string | null>(null);

  // Step 3 – Revenue
  const [revenueTarget, setRevenueTarget] = useState('');
  const [ecpmLow, setEcpmLow] = useState('');
  const [ecpmHigh, setEcpmHigh] = useState('');
  const [projection, setProjection] = useState<RevenueProjection | null>(null);
  const [projectionLoading, setProjectionLoading] = useState(false);

  // Step 4 – Audience
  const [audience, setAudience] = useState<AudienceRecommendation | null>(null);
  const [audienceLoading, setAudienceLoading] = useState(false);

  // Step 5 – Send Window
  const [sendDays, setSendDays] = useState<string[]>(DAY_VALUES.slice(0, 5));
  const [endHour, setEndHour] = useState(17);
  const [timezone, setTimezone] = useState('America/New_York');
  const [sendWindow, setSendWindow] = useState<SendWindowResult | null>(null);
  const [windowLoading, setWindowLoading] = useState(false);

  // Step 6 – Deploy
  const [deployResult, setDeployResult] = useState<DeployResult | null>(null);
  const [deploying, setDeploying] = useState(false);

  // ─── Fetch Offers ────────────────────────────────────────────────────────
  useEffect(() => {
    const loadOffers = async () => {
      setOffersLoading(true);
      setError(null);
      try {
        const res = await orgFetch('/api/mailing/offer-center/comparison?days=7');
        if (!res.ok) throw new Error('Failed to load offers');
        const data = await res.json();
        const loadedOffers: ComparisonOffer[] = data.offers || [];
        setOffers(loadedOffers);

        // Auto-select offer if navigated from Offer Center
        if (initialOffer && loadedOffers.length > 0) {
          const match = loadedOffers.find(
            o => o.offer_id === initialOffer.offerId || o.offer_name === initialOffer.offerName
          );
          if (match) {
            analyzeOffer(match);
          } else {
            // Offer not in comparison list — create a synthetic entry and analyze it
            const synthetic: ComparisonOffer = {
              offer_id: initialOffer.offerId,
              offer_name: initialOffer.offerName,
              offer_type: '',
              network_revenue: 0,
              network_epc: 0,
              network_cvr: 0,
            };
            analyzeOffer(synthetic);
          }
          onOfferConsumed?.();
        }
      } catch (err: any) {
        setError(err.message);
      } finally {
        setOffersLoading(false);
      }
    };
    loadOffers();
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  // ─── Analyze Offer ───────────────────────────────────────────────────────
  const analyzeOffer = useCallback(async (offer: ComparisonOffer) => {
    setSelectedOffer(offer);
    setAnalysisLoading(true);
    setAnalysis(null);
    setError(null);
    try {
      const res = await orgFetch(
        `/api/mailing/agent-wizard/analyze-offer?offer_id=${encodeURIComponent(offer.offer_id)}&offer_name=${encodeURIComponent(offer.offer_name)}`
      );
      if (!res.ok) throw new Error('Failed to analyze offer');
      const data = await res.json();
      setAnalysis(data);
    } catch (err: any) {
      setError(err.message);
    } finally {
      setAnalysisLoading(false);
    }
  }, []);

  // ─── Revenue Projection (debounced) ──────────────────────────────────────
  useEffect(() => {
    if (step !== 2) return;
    const rt = parseFloat(revenueTarget);
    const el = parseFloat(ecpmLow);
    const eh = parseFloat(ecpmHigh);
    if (!rt || !el || !eh || !selectedOffer || !payoutType) return;

    const timer = setTimeout(async () => {
      setProjectionLoading(true);
      setError(null);
      try {
        const res = await orgFetch('/api/mailing/agent-wizard/revenue-projection', {
          method: 'POST',
          body: JSON.stringify({
            offer_id: selectedOffer.offer_id,
            offer_name: selectedOffer.offer_name,
            payout_type: payoutType,
            revenue_target: rt,
            ecpm_low: el,
            ecpm_high: eh,
          }),
        });
        if (!res.ok) throw new Error('Failed to get projection');
        const data = await res.json();
        setProjection(data.projection);
      } catch (err: any) {
        setError(err.message);
      } finally {
        setProjectionLoading(false);
      }
    }, 500);

    return () => clearTimeout(timer);
  }, [step, revenueTarget, ecpmLow, ecpmHigh, selectedOffer, payoutType]);

  // ─── Audience Selection (on mount of step 4) ────────────────────────────
  useEffect(() => {
    if (step !== 3 || !selectedOffer || !projection) return;
    const loadAudience = async () => {
      setAudienceLoading(true);
      setError(null);
      try {
        const res = await orgFetch('/api/mailing/agent-wizard/audience-selection', {
          method: 'POST',
          body: JSON.stringify({
            offer_id: selectedOffer.offer_id,
            offer_name: selectedOffer.offer_name,
            required_sends: projection.forecasted_volume.expected,
            payout_type: payoutType,
            suppression_list_ids: [],
          }),
        });
        if (!res.ok) throw new Error('Failed to get audience recommendation');
        const data = await res.json();
        setAudience(data.recommendation);
      } catch (err: any) {
        setError(err.message);
      } finally {
        setAudienceLoading(false);
      }
    };
    loadAudience();
  }, [step, selectedOffer, projection, payoutType]);

  // ─── Send Window ─────────────────────────────────────────────────────────
  const configureSendWindow = useCallback(async () => {
    if (!audience) return;
    setWindowLoading(true);
    setError(null);
    try {
      const res = await orgFetch('/api/mailing/agent-wizard/send-window', {
        method: 'POST',
        body: JSON.stringify({
          send_days: sendDays,
          end_hour: endHour,
          timezone,
          audience_size: audience.net_recipients > 0 ? audience.net_recipients : audience.total_recipients,
        }),
      });
      if (!res.ok) throw new Error('Failed to configure send window');
      const data = await res.json();
      setSendWindow(data);
    } catch (err: any) {
      setError(err.message);
    } finally {
      setWindowLoading(false);
    }
  }, [audience, sendDays, endHour, timezone]);

  // ─── Deploy ──────────────────────────────────────────────────────────────
  const deployAgents = useCallback(async () => {
    if (!selectedOffer || !sendWindow) return;
    setDeploying(true);
    setError(null);
    try {
      const res = await orgFetch('/api/mailing/agent-wizard/deploy', {
        method: 'POST',
        body: JSON.stringify({
          campaign_id: organization?.id ? `campaign_${Date.now()}` : `campaign_${Date.now()}`,
          offer_id: selectedOffer.offer_id,
          offer_name: selectedOffer.offer_name,
          isp_schedule: sendWindow.isp_schedule,
          send_window: sendWindow.window,
        }),
      });
      if (!res.ok) throw new Error('Failed to deploy agents');
      const data = await res.json();
      setDeployResult(data);
    } catch (err: any) {
      setError(err.message);
    } finally {
      setDeploying(false);
    }
  }, [selectedOffer, sendWindow, organization]);

  // ─── Navigation helpers ──────────────────────────────────────────────────
  const canProceed = (): boolean => {
    switch (step) {
      case 0: return !!analysis;
      case 1: return !!payoutType;
      case 2: return !!projection;
      case 3: return !!audience;
      case 4: return !!sendWindow;
      default: return false;
    }
  };

  const goNext = () => {
    if (step < 5 && canProceed()) setStep(step + 1);
  };

  const goBack = () => {
    if (step > 0) setStep(step - 1);
  };

  const toggleDay = (day: string) => {
    setSendDays(prev =>
      prev.includes(day) ? prev.filter(d => d !== day) : [...prev, day]
    );
    setSendWindow(null);
  };

  // ─── Filtered offers ────────────────────────────────────────────────────
  const filteredOffers = offers.filter(o =>
    o.offer_name.toLowerCase().includes(offerSearch.toLowerCase())
  );

  // ─── Render Steps ──────────────────────────────────────────────────────

  const renderStepOffer = () => (
    <div className="aw-step-content">
      <h2 className="aw-step-title">
        <FontAwesomeIcon icon={faStore} /> Select an Offer
      </h2>
      <p className="aw-step-subtitle">
        Choose an offer to configure AI agents for. Network performance metrics are shown for the last 7 days.
      </p>

      {offersLoading ? (
        <div className="aw-loading">
          <FontAwesomeIcon icon={faSpinner} />
          <span>Loading offers…</span>
        </div>
      ) : (
        <>
          <div className="aw-search-wrap">
            <FontAwesomeIcon icon={faSearch} />
            <input
              className="aw-search-input"
              type="text"
              placeholder="Search offers…"
              value={offerSearch}
              onChange={e => setOfferSearch(e.target.value)}
            />
          </div>

          <div className="aw-offer-list">
            {filteredOffers.map(o => (
              <div
                key={o.offer_id}
                className={`aw-offer-item${selectedOffer?.offer_id === o.offer_id ? ' selected' : ''}`}
                onClick={() => analyzeOffer(o)}
              >
                <div>
                  <div className="aw-offer-name">{o.offer_name}</div>
                  <div className="aw-offer-type">{o.offer_type}</div>
                </div>
                <div className="aw-offer-metrics">
                  <div className="aw-offer-metric">
                    <div className="aw-offer-metric-label">Revenue</div>
                    <div className="aw-offer-metric-value">{fmtCurrency(o.network_revenue)}</div>
                  </div>
                  <div className="aw-offer-metric">
                    <div className="aw-offer-metric-label">EPC</div>
                    <div className="aw-offer-metric-value">{fmtCurrency(o.network_epc)}</div>
                  </div>
                  <div className="aw-offer-metric">
                    <div className="aw-offer-metric-label">CVR</div>
                    <div className="aw-offer-metric-value">{pct(o.network_cvr)}</div>
                  </div>
                </div>
              </div>
            ))}
            {filteredOffers.length === 0 && (
              <div className="aw-loading">
                <span>No offers match your search.</span>
              </div>
            )}
          </div>

          {analysisLoading && (
            <div className="aw-loading">
              <FontAwesomeIcon icon={faSpinner} />
              <span>Analyzing offer…</span>
            </div>
          )}

          {analysis && !analysisLoading && (
            <div className="aw-analysis-card">
              <div className="aw-analysis-header">
                <FontAwesomeIcon icon={faBrain} />
                <h3>Offer Analysis — {analysis.offer.offer_name}</h3>
              </div>
              <div className="aw-analysis-grid">
                <div className="aw-analysis-section">
                  <h4>Network Performance</h4>
                  <div className="aw-analysis-row">
                    <span>Revenue</span><span>{fmtCurrency(analysis.network.revenue)}</span>
                  </div>
                  <div className="aw-analysis-row">
                    <span>EPC</span><span>{fmtCurrency(analysis.network.epc)}</span>
                  </div>
                  <div className="aw-analysis-row">
                    <span>CVR</span><span>{pct(analysis.network.cvr)}</span>
                  </div>
                  <div className="aw-analysis-row">
                    <span>Clicks</span><span>{fmt(analysis.network.clicks)}</span>
                  </div>
                  <div className="aw-analysis-row">
                    <span>Conversions</span><span>{fmt(analysis.network.conversions)}</span>
                  </div>
                </div>

                <div className="aw-analysis-section">
                  <h4>Your Team</h4>
                  <div className="aw-analysis-row">
                    <span>Revenue</span><span>{fmtCurrency(analysis.your_team.revenue)}</span>
                  </div>
                  <div className="aw-analysis-row">
                    <span>EPC</span><span>{fmtCurrency(analysis.your_team.epc)}</span>
                  </div>
                  <div className="aw-analysis-row">
                    <span>CVR</span><span>{pct(analysis.your_team.cvr)}</span>
                  </div>
                  <div className="aw-analysis-row">
                    <span>Clicks</span><span>{fmt(analysis.your_team.clicks)}</span>
                  </div>
                  <div className="aw-analysis-row">
                    <span>Conversions</span><span>{fmt(analysis.your_team.conversions)}</span>
                  </div>
                </div>

                <div className="aw-analysis-section">
                  <h4>Local History</h4>
                  <div className="aw-analysis-row">
                    <span>Total Sends</span><span>{fmt(analysis.local_history.total_sends)}</span>
                  </div>
                  <div className="aw-analysis-row">
                    <span>Total Opens</span><span>{fmt(analysis.local_history.total_opens)}</span>
                  </div>
                  <div className="aw-analysis-row">
                    <span>Total Clicks</span><span>{fmt(analysis.local_history.total_clicks)}</span>
                  </div>
                  <div className="aw-analysis-row">
                    <span>Open Rate</span><span>{pct(analysis.local_history.avg_open_rate)}</span>
                  </div>
                  <div className="aw-analysis-row">
                    <span>Click Rate</span><span>{pct(analysis.local_history.avg_click_rate)}</span>
                  </div>
                </div>
              </div>
            </div>
          )}
        </>
      )}
    </div>
  );

  const renderStepPayout = () => {
    const types = analysis?.available_payout_types || [];
    return (
      <div className="aw-step-content">
        <h2 className="aw-step-title">
          <FontAwesomeIcon icon={faMoneyBill} /> Choose Payout Model
        </h2>
        <p className="aw-step-subtitle">
          Select how you want to earn revenue on this offer. This affects audience targeting strategy.
        </p>

        <div className="aw-payout-grid">
          {types.map(pt => {
            const info = PAYOUT_DESCRIPTIONS[pt] || { label: pt, desc: pt, icon: faBolt };
            return (
              <div
                key={pt}
                className={`aw-payout-card${payoutType === pt ? ' selected' : ''}`}
                onClick={() => setPayoutType(pt)}
              >
                <div className="aw-payout-icon">
                  <FontAwesomeIcon icon={info.icon} />
                </div>
                <h3>{info.label}</h3>
                <p>{info.desc}</p>
              </div>
            );
          })}
        </div>

        {payoutType && PAYOUT_TIPS[payoutType] && (
          <div className="aw-ai-tip">
            <FontAwesomeIcon icon={faBrain} />
            <span>{PAYOUT_TIPS[payoutType]}</span>
          </div>
        )}
      </div>
    );
  };

  const renderStepRevenue = () => (
    <div className="aw-step-content">
      <h2 className="aw-step-title">
        <FontAwesomeIcon icon={faChartLine} /> Revenue Targets
      </h2>
      <p className="aw-step-subtitle">
        Set your revenue goal and ECPM range. The AI will calculate exact send volume and audience size needed.
      </p>

      <div className="aw-revenue-inputs">
        <div className="aw-input-group">
          <label>Revenue Target ($)</label>
          <input
            type="number"
            placeholder="e.g. 5000"
            value={revenueTarget}
            onChange={e => setRevenueTarget(e.target.value)}
          />
        </div>
        <div className="aw-input-group">
          <label>ECPM Low ($)</label>
          <input
            type="number"
            step="0.01"
            placeholder="e.g. 0.40"
            value={ecpmLow}
            onChange={e => setEcpmLow(e.target.value)}
          />
        </div>
        <div className="aw-input-group">
          <label>ECPM High ($)</label>
          <input
            type="number"
            step="0.01"
            placeholder="e.g. 0.60"
            value={ecpmHigh}
            onChange={e => setEcpmHigh(e.target.value)}
          />
        </div>
      </div>

      {projectionLoading && (
        <div className="aw-loading">
          <FontAwesomeIcon icon={faSpinner} />
          <span>Calculating projection…</span>
        </div>
      )}

      {projection && !projectionLoading && (
        <div className="aw-projection-card">
          <div className="aw-projection-header">
            <h3>
              <FontAwesomeIcon icon={faRobot} /> AI Revenue Projection
            </h3>
            <span className={`aw-confidence ${projection.confidence}`}>
              <FontAwesomeIcon icon={faStar} />
              {projection.confidence} confidence
            </span>
          </div>

          {/* Formula Banner */}
          <div className="aw-formula-banner">
            <span className="aw-formula-label">Formula</span>
            <code>{projection.formula}</code>
          </div>

          {/* ECPM Range */}
          <div className="aw-projection-section">
            <h4>ECPM Range</h4>
            <div className="aw-projection-range-bar">
              <div className="aw-range-endpoint">
                <span className="aw-range-label">Low</span>
                <span className="aw-range-value">{fmtCurrency(projection.ecpm_range.low)}</span>
              </div>
              <div className="aw-range-track">
                <div className="aw-range-fill" />
                <div className="aw-range-mid-marker">
                  <span>{fmtCurrency(projection.ecpm_range.mid)}</span>
                </div>
              </div>
              <div className="aw-range-endpoint">
                <span className="aw-range-label">High</span>
                <span className="aw-range-value">{fmtCurrency(projection.ecpm_range.high)}</span>
              </div>
            </div>
            <div className="aw-projection-sub-row">
              <span>Revenue per send: {fmtPreciseCurrency(projection.revenue_per_send.low)} – {fmtPreciseCurrency(projection.revenue_per_send.high)}</span>
              <span>Mid: {fmtPreciseCurrency(projection.revenue_per_send.mid)} per email</span>
            </div>
          </div>

          {/* Forecasted Volume */}
          <div className="aw-projection-section">
            <h4>Forecasted Send Volume</h4>
            <div className="aw-scenario-grid">
              <div className="aw-scenario-card conservative">
                <span className="aw-scenario-tag">Conservative</span>
                <span className="aw-scenario-value">{fmt(projection.forecasted_volume.conservative)}</span>
                <span className="aw-scenario-detail">sends @ {fmtCurrency(projection.ecpm_range.low)} ECPM</span>
              </div>
              <div className="aw-scenario-card expected">
                <span className="aw-scenario-tag">Expected</span>
                <span className="aw-scenario-value">{fmt(projection.forecasted_volume.expected)}</span>
                <span className="aw-scenario-detail">sends @ {fmtCurrency(projection.ecpm_range.mid)} ECPM</span>
              </div>
              <div className="aw-scenario-card optimistic">
                <span className="aw-scenario-tag">Optimistic</span>
                <span className="aw-scenario-value">{fmt(projection.forecasted_volume.optimistic)}</span>
                <span className="aw-scenario-detail">sends @ {fmtCurrency(projection.ecpm_range.high)} ECPM</span>
              </div>
            </div>
          </div>

          {/* Target Audience */}
          <div className="aw-projection-section">
            <h4>Target Audience Size (incl. {projection.target_audience.bounce_buffer} bounce buffer)</h4>
            <div className="aw-scenario-grid">
              <div className="aw-scenario-card conservative">
                <span className="aw-scenario-tag">Conservative</span>
                <span className="aw-scenario-value">{fmt(projection.target_audience.conservative)}</span>
                <span className="aw-scenario-detail">audience members</span>
              </div>
              <div className="aw-scenario-card expected">
                <span className="aw-scenario-tag">Expected</span>
                <span className="aw-scenario-value">{fmt(projection.target_audience.expected)}</span>
                <span className="aw-scenario-detail">audience members</span>
              </div>
              <div className="aw-scenario-card optimistic">
                <span className="aw-scenario-tag">Optimistic</span>
                <span className="aw-scenario-value">{fmt(projection.target_audience.optimistic)}</span>
                <span className="aw-scenario-detail">audience members</span>
              </div>
            </div>
          </div>

          {/* Projected Revenue */}
          <div className="aw-projection-section">
            <h4>Projected Revenue (at expected volume)</h4>
            <div className="aw-scenario-grid">
              <div className="aw-scenario-card conservative">
                <span className="aw-scenario-tag">Low ECPM</span>
                <span className="aw-scenario-value">{fmtCurrency(projection.projected_revenue.at_low_ecpm)}</span>
              </div>
              <div className="aw-scenario-card expected">
                <span className="aw-scenario-tag">Mid ECPM</span>
                <span className="aw-scenario-value">{fmtCurrency(projection.projected_revenue.at_mid_ecpm)}</span>
              </div>
              <div className="aw-scenario-card optimistic">
                <span className="aw-scenario-tag">High ECPM</span>
                <span className="aw-scenario-value">{fmtCurrency(projection.projected_revenue.at_high_ecpm)}</span>
              </div>
            </div>
          </div>

          {/* Math Breakdown */}
          <div className="aw-math-breakdown">
            <h4><FontAwesomeIcon icon={faBrain} /> Math Breakdown</h4>
            <div className="aw-math-rows">
              <div className="aw-math-row">
                <span>Revenue Target</span>
                <span>{fmtCurrency(projection.target_revenue)}</span>
              </div>
              <div className="aw-math-row">
                <span>ECPM Mid</span>
                <span>{fmtCurrency(projection.ecpm_range.mid)}</span>
              </div>
              <div className="aw-math-row">
                <span>Revenue per send = {fmtCurrency(projection.ecpm_range.mid)} / 1,000</span>
                <span>{fmtPreciseCurrency(projection.revenue_per_send.mid)}</span>
              </div>
              <div className="aw-math-row highlight">
                <span>Sends Needed = {fmtCurrency(projection.target_revenue)} / {fmtPreciseCurrency(projection.revenue_per_send.mid)}</span>
                <span>{fmt(projection.forecasted_volume.expected)}</span>
              </div>
              <div className="aw-math-row highlight">
                <span>Audience = {fmt(projection.forecasted_volume.expected)} x 1.15 (bounce buffer)</span>
                <span>{fmt(projection.target_audience.expected)}</span>
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  );

  const renderStepAudience = () => {
    const suppPct = audience && audience.total_recipients > 0
      ? (audience.suppression_estimate / audience.total_recipients * 100).toFixed(1)
      : '0';
    const netPct = audience && audience.total_recipients > 0
      ? (audience.net_recipients / audience.total_recipients * 100).toFixed(1)
      : '0';

    return (
      <div className="aw-step-content">
        <h2 className="aw-step-title">
          <FontAwesomeIcon icon={faUsers} /> Audience Configuration
        </h2>
        <p className="aw-step-subtitle">
          The AI selected optimal audience segments based on your offer and revenue targets. Suppression is applied automatically.
        </p>

        {audienceLoading ? (
          <div className="aw-loading">
            <FontAwesomeIcon icon={faSpinner} />
            <span>Selecting audience segments &amp; cross-referencing suppression lists…</span>
            <span className="aw-loading-sub">This may take a moment while we verify real overlap.</span>
          </div>
        ) : audience ? (
          <>
            {/* Summary Stats */}
            <div className="aw-audience-summary-bar">
              <div className="aw-audience-summary-stat primary">
                <span className="aw-audience-summary-label">Gross Audience</span>
                <span className="aw-audience-summary-value">{audience.total_recipients.toLocaleString()}</span>
              </div>
              <div className="aw-audience-summary-arrow">→</div>
              <div className="aw-audience-summary-stat danger">
                <span className="aw-audience-summary-label">Suppressed</span>
                <span className="aw-audience-summary-value">−{audience.suppression_estimate.toLocaleString()}</span>
              </div>
              <div className="aw-audience-summary-arrow">→</div>
              <div className="aw-audience-summary-stat success">
                <span className="aw-audience-summary-label">Net Audience</span>
                <span className="aw-audience-summary-value">{audience.net_recipients.toLocaleString()}</span>
              </div>
            </div>

            {/* Visual bar */}
            <div className="aw-audience-bar-visual">
              <div className="aw-audience-bar-track">
                <div className="aw-audience-bar-net" style={{ width: `${netPct}%` }}>
                  <span>{netPct}% deliverable</span>
                </div>
                <div className="aw-audience-bar-supp" style={{ width: `${suppPct}%` }}>
                  <span>{suppPct}% suppressed</span>
                </div>
              </div>
            </div>

            {/* Two-column layout: Target vs Suppression */}
            <div className="aw-audience-two-col">
              {/* Target Audience Column */}
              <div className="aw-audience-col target">
                <div className="aw-audience-col-header target">
                  <FontAwesomeIcon icon={faUsers} />
                  <h3>Target Audience</h3>
                  <span className={`aw-quality-badge ${audience.engagement_quality.toLowerCase()}`}>
                    {audience.engagement_quality}
                  </span>
                </div>

                {/* Mailing Lists */}
                {audience.mailing_lists && audience.mailing_lists.length > 0 && (
                  <div className="aw-audience-subgroup">
                    <h4>Mailing Lists</h4>
                    {audience.mailing_lists.map((list, i) => (
                      <div key={i} className="aw-audience-list-item">
                        <div className="aw-audience-list-info">
                          <span className="aw-audience-list-name">{list.name}</span>
                          <span className="aw-audience-list-sub">{list.active_count.toLocaleString()} active of {list.subscriber_count.toLocaleString()} total</span>
                        </div>
                        <span className="aw-audience-list-count">{list.active_count.toLocaleString()}</span>
                      </div>
                    ))}
                  </div>
                )}

                {/* Segments */}
                <div className="aw-audience-subgroup">
                  <h4>Selected Segments</h4>
                  {audience.selected_segments.map((seg, i) => (
                    <div key={i} className="aw-audience-list-item">
                      <div className="aw-audience-list-info">
                        <span className="aw-audience-list-name">{seg.name}</span>
                      </div>
                      <span className="aw-audience-list-count">{seg.count.toLocaleString()}</span>
                    </div>
                  ))}
                </div>

                <div className="aw-audience-col-total target">
                  <span>Total Target</span>
                  <span>{audience.total_recipients.toLocaleString()}</span>
                </div>
              </div>

              {/* Suppression Column */}
              <div className="aw-audience-col suppression">
                <div className="aw-audience-col-header suppression">
                  <FontAwesomeIcon icon={faShieldAlt} />
                  <h3>Suppression</h3>
                  {audience.suppression_checked && (
                    <span className="aw-crossref-badge">
                      <FontAwesomeIcon icon={faCheck} /> Cross-referenced
                      {audience.suppression_check_ms > 0 && (
                        <span className="aw-crossref-time">
                          {audience.suppression_check_ms < 1000
                            ? `${audience.suppression_check_ms}ms`
                            : `${(audience.suppression_check_ms / 1000).toFixed(1)}s`}
                        </span>
                      )}
                    </span>
                  )}
                </div>

                {audience.suppression_lists && audience.suppression_lists.length > 0 ? (
                  <div className="aw-audience-subgroup">
                    <h4>Suppression Lists — Matched Against Audience</h4>
                    {audience.suppression_lists
                      .filter(sl => sl.entry_count > 0 || sl.matched_count > 0)
                      .map((sl, i) => (
                      <div key={i} className={`aw-audience-list-item supp${sl.matched_count > 0 ? ' has-matches' : ''}`}>
                        <div className="aw-audience-list-info">
                          <span className="aw-audience-list-name">
                            {sl.name}
                            {sl.is_global && <span className="aw-global-badge">GLOBAL</span>}
                          </span>
                          <span className="aw-audience-list-sub">
                            {sl.entry_count.toLocaleString()} entries in list
                            {sl.matched_count > 0 && sl.entry_count > 0 && (
                              <> · {((sl.matched_count / Math.max(audience.total_recipients, 1)) * 100).toFixed(1)}% of audience</>
                            )}
                          </span>
                        </div>
                        <span className={`aw-audience-list-count supp${sl.matched_count > 0 ? ' active' : ''}`}>
                          {sl.matched_count > 0
                            ? `−${sl.matched_count.toLocaleString()}`
                            : '0 matches'}
                        </span>
                      </div>
                    ))}
                    {audience.suppression_lists.filter(sl => sl.entry_count === 0 && sl.matched_count === 0).length > 0 && (
                      <div className="aw-empty-lists-note">
                        {audience.suppression_lists.filter(sl => sl.entry_count === 0 && sl.matched_count === 0).length} empty suppression list(s) skipped
                      </div>
                    )}
                  </div>
                ) : (
                  <div className="aw-audience-subgroup">
                    <h4>Estimated Suppression</h4>
                    <div className="aw-audience-list-item supp">
                      <div className="aw-audience-list-info">
                        <span className="aw-audience-list-name">Auto-estimated (~5% fallback)</span>
                      </div>
                      <span className="aw-audience-list-count supp">−{audience.suppression_estimate.toLocaleString()}</span>
                    </div>
                  </div>
                )}

                <div className="aw-audience-col-total suppression">
                  <span>Total Suppressed</span>
                  <span>−{audience.suppression_estimate.toLocaleString()}</span>
                </div>
              </div>
            </div>

            {/* Engagement Score */}
            <div className="aw-audience-engagement-bar">
              <span>Avg Engagement Score</span>
              <div className="aw-engagement-gauge">
                <div className="aw-engagement-fill" style={{ width: `${Math.min(audience.avg_engagement_score, 100)}%` }} />
              </div>
              <span className="aw-engagement-val">{audience.avg_engagement_score.toFixed(1)}</span>
            </div>
          </>
        ) : null}
      </div>
    );
  };

  const renderStepWindow = () => (
    <div className="aw-step-content">
      <h2 className="aw-step-title">
        <FontAwesomeIcon icon={faClock} /> Send Window
      </h2>
      <p className="aw-step-subtitle">
        Configure when agents should send. The AI will optimize delivery times per ISP.
      </p>

      <div className="aw-window-config">
        <div className="aw-day-selector">
          <label>Send Days</label>
          <div className="aw-day-chips">
            {DAY_LABELS.map((d, i) => (
              <div
                key={d}
                className={`aw-day-chip${sendDays.includes(DAY_VALUES[i]) ? ' active' : ''}`}
                onClick={() => toggleDay(DAY_VALUES[i])}
              >
                {d}
              </div>
            ))}
          </div>
        </div>

        <div className="aw-select-group">
          <label>End Hour</label>
          <select value={endHour} onChange={e => { setEndHour(Number(e.target.value)); setSendWindow(null); }}>
            {Array.from({ length: 12 }, (_, i) => i + 1).map(h => (
              <option key={h} value={h + 12}>{h} PM</option>
            ))}
          </select>
        </div>

        <div className="aw-select-group">
          <label>Timezone</label>
          <select value={timezone} onChange={e => { setTimezone(e.target.value); setSendWindow(null); }}>
            {TIMEZONES.map(tz => (
              <option key={tz.value} value={tz.value}>{tz.label}</option>
            ))}
          </select>
        </div>
      </div>

      <div className="aw-configure-btn-wrap">
        <button
          className="aw-btn aw-btn-primary"
          onClick={configureSendWindow}
          disabled={windowLoading || sendDays.length === 0}
        >
          {windowLoading ? <FontAwesomeIcon icon={faSpinner} className="fa-spin" /> : <FontAwesomeIcon icon={faBolt} />}
          {windowLoading ? 'Optimizing…' : 'Configure Send Window'}
        </button>
      </div>

      {sendWindow && (
        <div className="aw-isp-schedule">
          <h3>
            <FontAwesomeIcon icon={faRobot} /> ISP Delivery Schedule
          </h3>
          <div className="aw-isp-grid">
            {sendWindow.isp_schedule.map((entry, i) => (
              <div key={i} className="aw-isp-card">
                <div className="aw-isp-card-header">
                  <span className="aw-isp-name">{entry.isp}</span>
                  <span className="aw-isp-pct">{(entry.percentage * 100).toFixed(0)}%</span>
                </div>
                <div className="aw-isp-detail">
                  <span>Domain</span><span>{entry.domain}</span>
                </div>
                <div className="aw-isp-detail">
                  <span>Recipients</span><span>{fmt(entry.recipient_count)}</span>
                </div>
                <div className="aw-isp-detail">
                  <span>Optimal Hour</span><span>{entry.optimal_hour > 12 ? entry.optimal_hour - 12 : entry.optimal_hour} {entry.optimal_hour >= 12 ? 'PM' : 'AM'}</span>
                </div>
              </div>
            ))}
          </div>
          <div className="aw-window-summary">
            <div className="aw-window-stat">
              <span className="aw-window-stat-label">Total Daily Capacity</span>
              <span className="aw-window-stat-value">{fmt(sendWindow.total_daily_capacity)}</span>
            </div>
            <div className="aw-window-stat">
              <span className="aw-window-stat-label">Est. Days to Complete</span>
              <span className="aw-window-stat-value">{sendWindow.estimated_days_to_complete}</span>
            </div>
          </div>
        </div>
      )}
    </div>
  );

  const renderStepDeploy = () => {
    if (deployResult) {
      return (
        <div className="aw-step-content">
          <div className="aw-deploy-success">
            <div className="aw-deploy-success-icon">
              <FontAwesomeIcon icon={faCheck} />
            </div>
            <h2>Agents Deployed Successfully</h2>
            <p>{deployResult.total_agents} ISP agents are now active for campaign {deployResult.campaign_id}</p>

            <div className="aw-deployed-grid">
              {deployResult.deployed_agents.map((agent, i) => (
                <div key={i} className="aw-deployed-card">
                  <div className="aw-deployed-card-header">
                    <span className="aw-deployed-card-name">{agent.isp}</span>
                    <span className="aw-deployed-status">{agent.status}</span>
                  </div>
                  <div className="aw-deployed-card-detail">
                    {agent.domain} — {fmt(agent.recipient_count)} recipients
                    {agent.is_new && ' (new)'}
                  </div>
                </div>
              ))}
            </div>

            <button className="aw-btn aw-btn-success" onClick={() => onComplete(deployResult)}>
              <FontAwesomeIcon icon={faCheck} />
              Done — View Campaign
            </button>
          </div>
        </div>
      );
    }

    const sendDayLabels = sendDays.map(d => {
      const idx = DAY_VALUES.indexOf(d);
      return idx >= 0 ? DAY_LABELS[idx] : d;
    }).join(', ');

    const endHourLabel = `${endHour > 12 ? endHour - 12 : endHour} ${endHour >= 12 ? 'PM' : 'AM'}`;

    return (
      <div className="aw-step-content">
        <h2 className="aw-step-title">
          <FontAwesomeIcon icon={faRocket} /> Review &amp; Deploy
        </h2>
        <p className="aw-step-subtitle">
          Review your configuration and deploy ISP agents to begin the campaign.
        </p>

        <div className="aw-review-card">
          <h3><FontAwesomeIcon icon={faStore} /> Configuration Summary</h3>
          <div className="aw-review-rows">
            <div className="aw-review-row">
              <span className="aw-review-row-label">Offer</span>
              <span className="aw-review-row-value">
                {selectedOffer?.offer_name} ({selectedOffer?.offer_type})
              </span>
            </div>
            <div className="aw-review-row">
              <span className="aw-review-row-label">Payout Model</span>
              <span className="aw-review-row-value">{payoutType}</span>
            </div>
            <div className="aw-review-row">
              <span className="aw-review-row-label">Revenue Target</span>
              <span className="aw-review-row-value">{fmtCurrency(parseFloat(revenueTarget) || 0)}</span>
            </div>
            <div className="aw-review-row">
              <span className="aw-review-row-label">ECPM Range</span>
              <span className="aw-review-row-value">
                {fmtCurrency(parseFloat(ecpmLow) || 0)} – {fmtCurrency(parseFloat(ecpmHigh) || 0)}
              </span>
            </div>
            {audience && (
              <div className="aw-review-row">
                <span className="aw-review-row-label">Audience</span>
                <span className="aw-review-row-value">
                  {fmt(audience.net_recipients)} recipients across {audience.selected_segments.length} segments
                </span>
              </div>
            )}
            <div className="aw-review-row">
              <span className="aw-review-row-label">Send Window</span>
              <span className="aw-review-row-value">
                {sendDayLabels} until {endHourLabel}
              </span>
            </div>
          </div>
        </div>

        {sendWindow && (
          <div className="aw-review-card">
            <h3><FontAwesomeIcon icon={faRobot} /> ISP Agents to Deploy</h3>
            <div className="aw-review-agents">
              <div className="aw-isp-grid">
                {sendWindow.isp_schedule.map((entry, i) => (
                  <div key={i} className="aw-isp-card">
                    <div className="aw-isp-card-header">
                      <span className="aw-isp-name">{entry.isp}</span>
                      <span className="aw-isp-pct">{fmt(entry.recipient_count)}</span>
                    </div>
                    <div className="aw-isp-detail">
                      <span>Domain</span><span>{entry.domain}</span>
                    </div>
                  </div>
                ))}
              </div>
            </div>
          </div>
        )}

        <div style={{ display: 'flex', justifyContent: 'center', marginTop: 8 }}>
          <button
            className="aw-btn aw-btn-success"
            onClick={deployAgents}
            disabled={deploying}
          >
            {deploying ? <FontAwesomeIcon icon={faSpinner} className="fa-spin" /> : <FontAwesomeIcon icon={faRocket} />}
            {deploying ? 'Deploying Agents…' : 'Deploy Agents & Launch'}
          </button>
        </div>
      </div>
    );
  };

  // ─── Step renderer ───────────────────────────────────────────────────────
  const renderCurrentStep = () => {
    switch (step) {
      case 0: return renderStepOffer();
      case 1: return renderStepPayout();
      case 2: return renderStepRevenue();
      case 3: return renderStepAudience();
      case 4: return renderStepWindow();
      case 5: return renderStepDeploy();
      default: return null;
    }
  };

  // ─── Main Render ─────────────────────────────────────────────────────────

  return (
    <div className="aw-container">
      {/* Header */}
      <div className="aw-header">
        <div className="aw-header-left">
          <div className="aw-header-icon">
            <FontAwesomeIcon icon={faRobot} />
          </div>
          <div>
            <h1>Agent Configuration Wizard</h1>
            <p>Configure AI-powered ISP agents for your campaign</p>
          </div>
        </div>
        <button className="aw-cancel-btn" onClick={onCancel}>Cancel</button>
      </div>

      {/* Progress */}
      <div className="aw-progress">
        {STEP_LABELS.map((label, i) => (
          <div
            key={i}
            className={`aw-progress-step${i < step ? ' completed' : ''}${i === step ? ' active' : ''}`}
          >
            <div className="aw-progress-dot">
              {i < step ? <FontAwesomeIcon icon={faCheck} /> : i + 1}
            </div>
            <span className="aw-progress-label">{label}</span>
            {i < STEP_LABELS.length - 1 && <div className="aw-progress-line" />}
          </div>
        ))}
      </div>

      {/* Error */}
      {error && (
        <div className="aw-error">
          <FontAwesomeIcon icon={faExclamationCircle} />
          <span>{error}</span>
        </div>
      )}

      {/* Current Step */}
      {renderCurrentStep()}

      {/* Navigation */}
      {!deployResult && (
        <div className="aw-nav">
          <div className="aw-nav-left">
            {step > 0 && (
              <button className="aw-btn aw-btn-secondary" onClick={goBack}>
                <FontAwesomeIcon icon={faChevronLeft} />
                Back
              </button>
            )}
          </div>
          <div className="aw-nav-right">
            {step < 5 && (
              <button
                className="aw-btn aw-btn-primary"
                onClick={goNext}
                disabled={!canProceed()}
              >
                Next
                <FontAwesomeIcon icon={faChevronRight} />
              </button>
            )}
          </div>
        </div>
      )}
    </div>
  );
};
