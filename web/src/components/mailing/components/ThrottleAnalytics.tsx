import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faSyncAlt, faSpinner, faTachometerAlt, faHistory,
  faBrain, faArrowDown, faArrowUp, faExchangeAlt,
  faExclamationTriangle, faCheckCircle, faTimesCircle,
  faChevronDown, faChevronRight,
} from '@fortawesome/free-solid-svg-icons';
import { useAuth } from '../../../contexts/AuthContext';
import './ThrottleAnalytics.css';

interface LiveRate {
  isp: string;
  display_name: string;
  current_rate: number;
  max_rate: number;
  rate_pct: number;
}

interface ThrottleDecision {
  isp: string;
  action: string;
  effective_rate: number;
  rate_adj: number;
  deferral_rate: number;
  backoff_step: number;
  recovering: boolean;
  result: string;
  created_at: string;
}

interface ThrottleConviction {
  isp: string;
  verdict: string;
  statement: string;
  effective_rate: number;
  deferral_rate: number;
  backoff_step: number;
  recovery_time_min: number;
  created_at: string;
}

interface ThrottleAnalyticsData {
  live_rates: LiveRate[];
  recent_decisions: ThrottleDecision[];
  convictions: ThrottleConviction[];
  updated_at: string;
}

const ISP_COLORS: Record<string, string> = {
  gmail: '#ea4335',
  yahoo: '#7b1fa2',
  microsoft: '#0078d4',
  aol: '#ff6600',
  apple: '#a2aaad',
  comcast: '#e60000',
  att: '#009fdb',
  cox: '#f26522',
  charter: '#0099d6',
};

const ISP_DISPLAY: Record<string, string> = {
  gmail: 'Gmail',
  yahoo: 'Yahoo',
  microsoft: 'Microsoft',
  aol: 'AOL',
  apple: 'Apple',
  comcast: 'Comcast',
  att: 'AT&T',
  cox: 'Cox',
  charter: 'Charter',
};

const ACTION_META: Record<string, { label: string; color: string; icon: typeof faArrowDown }> = {
  reduce_rate: { label: 'Rate Reduced', color: '#ef4444', icon: faArrowDown },
  backoff_mode: { label: 'Backoff Mode', color: '#dc2626', icon: faExclamationTriangle },
  increase_rate: { label: 'Rate Increased', color: '#10b981', icon: faArrowUp },
  snap_to_stable_rate: { label: 'Snap to Stable', color: '#3b82f6', icon: faExchangeAlt },
};

function getBarColor(pct: number): string {
  if (pct >= 80) return '#10b981';
  if (pct >= 40) return '#f59e0b';
  return '#ef4444';
}

function formatTime(dateStr: string): string {
  const d = new Date(dateStr);
  const diff = Date.now() - d.getTime();
  const mins = Math.floor(diff / 60000);
  if (mins < 1) return 'Just now';
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  return `${days}d ago`;
}

function formatTimestamp(dateStr: string): string {
  return new Date(dateStr).toLocaleString('en-US', {
    month: 'short', day: 'numeric',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
    hour12: false,
  });
}

export const ThrottleAnalytics: React.FC = () => {
  const { organization } = useAuth();
  const [data, setData] = useState<ThrottleAnalyticsData | null>(null);
  const [loading, setLoading] = useState(true);
  const [expandedISPs, setExpandedISPs] = useState<Set<string>>(new Set());

  const fetchData = useCallback(async () => {
    setLoading(true);
    try {
      const headers: Record<string, string> = { 'Content-Type': 'application/json' };
      if (organization?.id) headers['X-Organization-ID'] = organization.id;
      const res = await fetch('/api/mailing/engine/throttle-analytics', { headers });
      if (res.ok) {
        setData(await res.json());
      }
    } catch (err) {
      console.error('Failed to load throttle analytics:', err);
    } finally {
      setLoading(false);
    }
  }, [organization?.id]);

  useEffect(() => { fetchData(); }, [fetchData]);

  useEffect(() => {
    const interval = setInterval(fetchData, 30000);
    return () => clearInterval(interval);
  }, [fetchData]);

  const toggleISP = (isp: string) => {
    setExpandedISPs(prev => {
      const next = new Set(prev);
      if (next.has(isp)) next.delete(isp);
      else next.add(isp);
      return next;
    });
  };

  if (loading && !data) {
    return (
      <div className="ta-loading">
        <FontAwesomeIcon icon={faSpinner} spin /> Loading throttle analytics...
      </div>
    );
  }

  if (!data) {
    return (
      <div className="ta-empty">
        No throttle analytics data available. The engine may not be running.
      </div>
    );
  }

  const liveRates = data.live_rates ?? [];
  const recentDecisions = data.recent_decisions ?? [];
  const convictions = data.convictions ?? [];

  const sortedRates = [...liveRates].sort((a, b) => a.isp.localeCompare(b.isp));

  const convictionsByISP = convictions.reduce<Record<string, ThrottleConviction[]>>((acc, c) => {
    (acc[c.isp] = acc[c.isp] || []).push(c);
    return acc;
  }, {});

  return (
    <div className="ta-container">
      {/* Header */}
      <div className="ta-section-header">
        <div className="ta-section-title">
          <FontAwesomeIcon icon={faTachometerAlt} />
          <h2>Live ISP Rate Gauges</h2>
        </div>
        <div className="ta-header-right">
          {data.updated_at && (
            <span className="ta-updated">Updated {formatTime(data.updated_at)}</span>
          )}
          <button className="ta-refresh-btn" onClick={fetchData} disabled={loading}>
            <FontAwesomeIcon icon={faSyncAlt} spin={loading} /> Refresh
          </button>
        </div>
      </div>

      {/* Live Rate Gauges */}
      <div className="ta-gauges-grid">
        {sortedRates.map(rate => {
          const color = ISP_COLORS[rate.isp] || '#64748b';
          const barColor = getBarColor(rate.rate_pct);
          const displayName = rate.display_name || ISP_DISPLAY[rate.isp] || rate.isp;
          return (
            <div key={rate.isp} className="ta-gauge-card">
              <div className="ta-gauge-header">
                <span className="ta-gauge-isp" style={{ color }}>{displayName}</span>
                <span className="ta-gauge-pct" style={{ color: barColor }}>
                  {rate.rate_pct.toFixed(0)}%
                </span>
              </div>
              <div className="ta-gauge-bar-bg">
                <div
                  className="ta-gauge-bar-fill"
                  style={{ width: `${Math.min(100, rate.rate_pct)}%`, background: barColor }}
                />
              </div>
              <div className="ta-gauge-footer">
                <span>{rate.current_rate.toLocaleString()} msgs/hr</span>
                <span className="ta-gauge-max">/ {rate.max_rate.toLocaleString()} max</span>
              </div>
            </div>
          );
        })}
      </div>

      {/* Decision Timeline */}
      <div className="ta-section-header" style={{ marginTop: 32 }}>
        <div className="ta-section-title">
          <FontAwesomeIcon icon={faHistory} />
          <h2>Decision Timeline</h2>
        </div>
        <span className="ta-count-badge">{recentDecisions.length} recent</span>
      </div>

      {recentDecisions.length === 0 ? (
        <div className="ta-empty-section">No throttle decisions recorded yet.</div>
      ) : (
        <div className="ta-timeline">
          {recentDecisions.map((d, i) => {
            const meta = ACTION_META[d.action] || { label: d.action, color: '#6b7280', icon: faHistory };
            const ispColor = ISP_COLORS[d.isp] || '#64748b';
            const displayName = ISP_DISPLAY[d.isp] || d.isp;
            return (
              <div key={i} className="ta-timeline-entry">
                <div className="ta-timeline-dot" style={{ background: meta.color }} />
                <div className="ta-timeline-content">
                  <div className="ta-timeline-row-top">
                    <span className="ta-isp-pill" style={{ background: `${ispColor}22`, color: ispColor, borderColor: `${ispColor}44` }}>
                      {displayName}
                    </span>
                    <span className="ta-action-badge" style={{ background: `${meta.color}22`, color: meta.color, borderColor: `${meta.color}44` }}>
                      <FontAwesomeIcon icon={meta.icon} /> {meta.label}
                    </span>
                    <span className="ta-timeline-time">{formatTimestamp(d.created_at)}</span>
                  </div>
                  <div className="ta-timeline-metrics">
                    <span>Rate: <strong>{d.effective_rate.toLocaleString()}</strong> msgs/hr</span>
                    <span>Adj: <strong>{(d.rate_adj * 100).toFixed(1)}%</strong></span>
                    <span>Deferrals: <strong>{d.deferral_rate.toFixed(1)}%</strong></span>
                    {d.backoff_step > 0 && <span>Step: <strong>{d.backoff_step}</strong></span>}
                    {d.recovering && <span className="ta-recovering-badge">Recovering</span>}
                  </div>
                </div>
              </div>
            );
          })}
        </div>
      )}

      {/* Conviction Memory */}
      <div className="ta-section-header" style={{ marginTop: 32 }}>
        <div className="ta-section-title">
          <FontAwesomeIcon icon={faBrain} />
          <h2>Throttle Conviction Memory</h2>
        </div>
        <span className="ta-count-badge">{convictions.length} convictions</span>
      </div>

      {convictions.length === 0 ? (
        <div className="ta-empty-section">No throttle convictions recorded yet.</div>
      ) : (
        <div className="ta-convictions">
          {Object.entries(convictionsByISP).sort(([a], [b]) => a.localeCompare(b)).map(([isp, convictions]) => {
            const expanded = expandedISPs.has(isp);
            const ispColor = ISP_COLORS[isp] || '#64748b';
            const displayName = ISP_DISPLAY[isp] || isp;
            const willCount = convictions.filter(c => c.verdict === 'will').length;
            const wontCount = convictions.filter(c => c.verdict === 'wont').length;
            return (
              <div key={isp} className="ta-conviction-group">
                <button className="ta-conviction-header" onClick={() => toggleISP(isp)}>
                  <FontAwesomeIcon icon={expanded ? faChevronDown : faChevronRight} className="ta-chevron" />
                  <span className="ta-conviction-isp" style={{ color: ispColor }}>{displayName}</span>
                  <span className="ta-verdict-counts">
                    <span className="ta-will-count"><FontAwesomeIcon icon={faCheckCircle} /> {willCount}</span>
                    <span className="ta-wont-count"><FontAwesomeIcon icon={faTimesCircle} /> {wontCount}</span>
                  </span>
                  <span className="ta-conviction-total">{convictions.length} total</span>
                </button>
                {expanded && (
                  <div className="ta-conviction-list">
                    {convictions.map((c, i) => (
                      <div key={i} className="ta-conviction-entry">
                        <div className="ta-conviction-verdict-row">
                          <span className={`ta-verdict-badge ${c.verdict === 'will' ? 'ta-verdict-will' : 'ta-verdict-wont'}`}>
                            <FontAwesomeIcon icon={c.verdict === 'will' ? faCheckCircle : faTimesCircle} />
                            {c.verdict === 'will' ? ' WILL' : ' WONT'}
                          </span>
                          <span className="ta-conviction-time">{formatTimestamp(c.created_at)}</span>
                        </div>
                        <p className="ta-conviction-statement">{c.statement}</p>
                        <div className="ta-conviction-ctx">
                          <span>Rate: {c.effective_rate.toLocaleString()}/hr</span>
                          {c.deferral_rate > 0 && <span>Deferrals: {c.deferral_rate.toFixed(1)}%</span>}
                          {c.backoff_step > 0 && <span>Backoff step: {c.backoff_step}</span>}
                          {c.recovery_time_min > 0 && <span>Recovery: {c.recovery_time_min.toFixed(0)}min</span>}
                        </div>
                      </div>
                    ))}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
};

export default ThrottleAnalytics;
