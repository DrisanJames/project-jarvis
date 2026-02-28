import React, { useMemo } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faChartLine, faEnvelope, faMousePointer, faExclamationTriangle, faBan, faUserMinus, faArrowUp, faArrowDown, faTrophy, faChartBar, faShieldAlt } from '@fortawesome/free-solid-svg-icons';
import { MetricCard, Card, CardHeader, CardBody, Loading, ErrorDisplay } from '../common';
import { useApi } from '../../hooks/useApi';
import { useDateFilter } from '../../context/DateFilterContext';
import type { DashboardData } from '../../types';
import { ISPTable } from '../isp/ISPTable';

// ── Ticker Item ──
interface TickerItemData { label: string; value: string; change?: number; isNegativeMetric?: boolean }
const TickerItem: React.FC<TickerItemData> = ({ label, value, change, isNegativeMetric }) => {
  const hasChange = change !== undefined && change !== null && !isNaN(change);
  const isPositive = hasChange && (isNegativeMetric ? change < 0 : change > 0);
  const isNeg = hasChange && (isNegativeMetric ? change > 0 : change < 0);
  const color = isPositive ? '#22c55e' : isNeg ? '#ef4444' : 'var(--text-muted)';
  return (
    <div style={{ display: 'inline-flex', alignItems: 'center', gap: '0.5rem', padding: '0 1rem', whiteSpace: 'nowrap' }}>
      <span style={{ fontSize: '0.7rem', color: 'var(--text-muted)', fontWeight: 500, textTransform: 'uppercase', letterSpacing: '0.05em' }}>{label}</span>
      <span style={{ fontSize: '0.8rem', fontWeight: 700, fontFamily: 'monospace', color: 'var(--text-primary)' }}>{value}</span>
      {hasChange && (
        <span style={{ fontSize: '0.7rem', fontFamily: 'monospace', fontWeight: 600, color, display: 'flex', alignItems: 'center', gap: '2px' }}>
          <FontAwesomeIcon icon={isPositive ? faArrowUp : faArrowDown} style={{ fontSize: '0.55rem' }} />
          {Math.abs(change).toFixed(2)}%
        </span>
      )}
      <span style={{ color: 'var(--border-color)', margin: '0 0.25rem', fontSize: '0.7rem' }}>│</span>
    </div>
  );
};

const POLLING_INTERVAL = 60000; // 1 minute

// ── Reusable micro-components ──
const MiniGauge: React.FC<{ value: number; max: number; thresholds: [number, number]; invert?: boolean; label: string }> = ({ value, max, thresholds, invert = false, label }) => {
  const pct = Math.min((value / max) * 100, 100);
  const isGood = invert ? value < thresholds[0] : value > thresholds[1];
  const isBad = invert ? value > thresholds[1] : value < thresholds[0];
  const color = isGood ? '#22c55e' : isBad ? '#ef4444' : '#f59e0b';
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '2px' }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: '0.65rem', color: 'var(--text-muted)' }}>
        <span>{label}</span>
        <span style={{ fontFamily: 'monospace', color, fontWeight: 600 }}>{(value * 100).toFixed(2)}%</span>
      </div>
      <div style={{ height: 4, background: 'var(--bg-tertiary, #333)', borderRadius: 2, overflow: 'hidden' }}>
        <div style={{ height: '100%', width: `${pct}%`, background: color, borderRadius: 2, transition: 'width 0.5s ease' }} />
      </div>
    </div>
  );
};

export const DashboardView: React.FC = () => {
  // Use global date filter
  const { dateRange } = useDateFilter();
  
  // Build API URL with date range params
  const apiUrl = `/api/dashboard?start_date=${dateRange.startDate}&end_date=${dateRange.endDate}&range_type=${dateRange.type}`;
  
  const { data, loading, error, refetch } = useApi<DashboardData>(
    apiUrl,
    { pollingInterval: POLLING_INTERVAL }
  );

  // ── Deliverability Health Score ──
  const healthScore = useMemo(() => {
    const s = data?.summary;
    if (!s) return null;
    const deliveryRate = s.delivery_rate ?? 0;
    const openRate = s.open_rate ?? 0;
    const clickRate = s.click_rate ?? 0;
    const bounceRate = s.bounce_rate ?? 0;
    const complaintRate = s.complaint_rate ?? 0;

    // Weighted composite score (0-100)
    const score = Math.min(100, Math.max(0,
      (deliveryRate * 40) +          // 40% weight on delivery
      (openRate * 25) +              // 25% weight on opens
      (clickRate * 15) +             // 15% weight on clicks
      ((1 - bounceRate) * 10) +      // 10% weight on low bounces
      ((1 - complaintRate * 1000) * 10) // 10% weight on low complaints
    ));

    const grade = score >= 90 ? 'A' : score >= 80 ? 'B' : score >= 70 ? 'C' : score >= 60 ? 'D' : 'F';
    const gradeColor = score >= 90 ? '#22c55e' : score >= 80 ? '#3b82f6' : score >= 70 ? '#f59e0b' : '#ef4444';
    const signal = score >= 85 ? 'STRONG' : score >= 70 ? 'MODERATE' : score >= 50 ? 'WEAK' : 'CRITICAL';
    const signalColor = score >= 85 ? '#22c55e' : score >= 70 ? '#f59e0b' : '#ef4444';

    return { score, grade, gradeColor, signal, signalColor, deliveryRate, openRate, clickRate, bounceRate, complaintRate };
  }, [data?.summary]);

  // ── ISP Distribution Analysis ──
  const ispAnalysis = useMemo(() => {
    const isps = data?.isp_metrics;
    if (!isps || isps.length === 0) return null;

    const total = isps.length;
    const healthy = isps.filter(i => i.status === 'healthy').length;
    const warning = isps.filter(i => i.status === 'warning').length;
    const critical = isps.filter(i => i.status === 'critical').length;
    const totalVolume = isps.reduce((s, i) => s + (i.metrics.sent || i.metrics.targeted || 0), 0);
    const avgDeliveryRate = isps.reduce((s, i) => s + (i.metrics.delivery_rate || 0), 0) / total;
    const avgOpenRate = isps.reduce((s, i) => s + (i.metrics.open_rate || 0), 0) / total;

    return { total, healthy, warning, critical, totalVolume, avgDeliveryRate, avgOpenRate };
  }, [data?.isp_metrics]);

  // Calculate top performing ISPs - must be called before any early returns
  const topPerformingISPs = useMemo(() => {
    if (!data?.isp_metrics || data.isp_metrics.length === 0) return null;

    const isps = data.isp_metrics.map(isp => {
      const volume = isp.metrics.sent || isp.metrics.targeted || 0;
      const clickRate = isp.metrics.click_rate || 0;
      const openRate = isp.metrics.open_rate || 0;
      const deliveryRate = isp.metrics.delivery_rate || 0;
      const ctr = openRate > 0 ? clickRate / openRate : 0; // Click-to-open rate
      
      // Composite performance score (weighted)
      // Volume weight: 30%, CTR: 30%, Delivery: 20%, Open Rate: 20%
      const normalizedVolume = Math.min(volume / 1000000, 1); // Normalize to 0-1 (1M as max)
      const performanceScore = (
        (normalizedVolume * 0.3) +
        (ctr * 0.3) +
        (deliveryRate * 0.2) +
        (openRate * 0.2)
      ) * 100;

      return {
        provider: isp.provider,
        volume,
        clickRate,
        openRate,
        deliveryRate,
        ctr,
        clicks: isp.metrics.clicked || isp.metrics.unique_clicked || 0,
        performanceScore,
        status: isp.status,
      };
    });

    // Sort by different criteria
    const byVolume = [...isps].sort((a, b) => b.volume - a.volume).slice(0, 5);
    const byCTR = [...isps].filter(i => i.volume >= 1000).sort((a, b) => b.ctr - a.ctr).slice(0, 5);
    const byPerformance = [...isps].filter(i => i.volume >= 1000).sort((a, b) => b.performanceScore - a.performanceScore).slice(0, 5);

    return { byVolume, byCTR, byPerformance };
  }, [data?.isp_metrics]);

  // ── Ticker data ──
  const tickerItems = useMemo((): TickerItemData[] => {
    const s = data?.summary;
    if (!s) return [];
    const fN = (n: number) => { if (n >= 1000000) return `${(n / 1000000).toFixed(1)}M`; if (n >= 1000) return `${(n / 1000).toFixed(1)}K`; return n.toLocaleString(); };
    const fP = (n: number) => `${(n * 100).toFixed(2)}%`;
    return [
      { label: 'VOL', value: fN(s.total_targeted ?? 0), change: s.volume_change },
      { label: 'DEL', value: fP(s.delivery_rate ?? 0) },
      { label: 'OPEN', value: fP(s.open_rate ?? 0), change: s.open_rate_change },
      { label: 'CTR', value: fP(s.click_rate ?? 0) },
      { label: 'COMP', value: fP(s.complaint_rate ?? 0), change: s.complaint_change, isNegativeMetric: true },
      { label: 'BNCE', value: fP(s.bounce_rate ?? 0), isNegativeMetric: true },
    ];
  }, [data?.summary]);

  // Helper functions
  const formatNumber = (n: number): string => {
    if (n >= 1000000) return `${(n / 1000000).toFixed(1)}M`;
    if (n >= 1000) return `${(n / 1000).toFixed(1)}K`;
    return n.toLocaleString();
  };

  const formatPercent = (n: number): string => `${(n * 100).toFixed(2)}%`;

  // Early returns for loading/error states - after all hooks
  if (loading && !data) {
    return <Loading message="Loading dashboard..." />;
  }

  if (error) {
    return <ErrorDisplay message={error} onRetry={refetch} />;
  }

  const summary = data?.summary;

  return (
    <div>
      {/* Header with date range */}
      <div style={{ 
        marginBottom: '1.5rem',
        display: 'flex',
        alignItems: 'center',
        gap: '0.75rem',
      }}>
        <FontAwesomeIcon icon={faChartLine} style={{ color: 'var(--accent-blue)', fontSize: '24px' }} />
        <h2 style={{ margin: 0 }}>SparkPost Dashboard</h2>
        <span style={{
          padding: '0.25rem 0.75rem',
          backgroundColor: 'var(--primary-color)',
          color: 'white',
          borderRadius: '1rem',
          fontSize: '0.75rem',
          fontWeight: 500,
        }}>
          {dateRange.label}
        </span>
        <span style={{
          padding: '0.25rem 0.75rem',
          backgroundColor: 'rgba(59, 130, 246, 0.2)',
          color: 'var(--accent-blue)',
          borderRadius: '1rem',
          fontSize: '0.75rem',
          fontWeight: 500,
        }}>
          {data?.isp_metrics?.length ?? 0} ISPs
        </span>
      </div>

      {/* ── Market Ticker Strip ── */}
      {tickerItems.length > 0 && (
        <div style={{
          marginBottom: '1.25rem', padding: '0.5rem 0', overflowX: 'auto',
          background: 'linear-gradient(90deg, rgba(59,130,246,0.05) 0%, rgba(59,130,246,0.02) 100%)',
          borderTop: '1px solid rgba(59,130,246,0.2)', borderBottom: '1px solid rgba(59,130,246,0.2)',
          borderRadius: '6px', display: 'flex', alignItems: 'center',
          scrollbarWidth: 'none',
        }}>
          <span style={{ padding: '0 0.75rem', fontSize: '0.65rem', fontWeight: 700, color: 'var(--accent-blue)', letterSpacing: '0.1em', textTransform: 'uppercase', whiteSpace: 'nowrap' }}>
            SPARKPOST
          </span>
          {tickerItems.map((item, idx) => <TickerItem key={idx} {...item} />)}
        </div>
      )}

      {/* Summary Metrics */}
      <div className="grid grid-5 mb-6">
        <MetricCard
          label="Volume"
          value={summary?.total_targeted ?? 0}
          change={summary?.volume_change}
          changeLabel="vs prior period"
        />
        <MetricCard
          label="Open Rate"
          value={summary?.open_rate ?? 0}
          format="percentage"
          change={summary?.open_rate_change}
        />
        <MetricCard
          label="Click Rate"
          value={summary?.click_rate ?? 0}
          format="percentage"
        />
        <MetricCard
          label="Complaint Rate"
          value={summary?.complaint_rate ?? 0}
          format="percentage"
          change={summary?.complaint_change}
          status={
            (summary?.complaint_rate ?? 0) > 0.0005
              ? 'critical'
              : (summary?.complaint_rate ?? 0) > 0.0003
              ? 'warning'
              : 'healthy'
          }
        />
        <MetricCard
          label="Bounce Rate"
          value={summary?.bounce_rate ?? 0}
          format="percentage"
          status={
            (summary?.bounce_rate ?? 0) > 0.05
              ? 'critical'
              : (summary?.bounce_rate ?? 0) > 0.03
              ? 'warning'
              : 'healthy'
          }
        />
      </div>

      {/* ── Deliverability Health Score Panel ── */}
      {healthScore && (
        <Card className="mb-6">
          <CardBody>
            <div style={{ display: 'grid', gridTemplateColumns: '160px 1fr 1fr', gap: '1.5rem', alignItems: 'center' }}>
              {/* Health Grade */}
              <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', gap: '0.25rem' }}>
                <div style={{
                  width: 80, height: 80, borderRadius: '50%',
                  border: `4px solid ${healthScore.gradeColor}`,
                  display: 'flex', alignItems: 'center', justifyContent: 'center',
                  flexDirection: 'column',
                }}>
                  <span style={{ fontSize: '1.8rem', fontWeight: 800, color: healthScore.gradeColor, fontFamily: 'monospace', lineHeight: 1 }}>
                    {healthScore.grade}
                  </span>
                  <span style={{ fontSize: '0.6rem', color: 'var(--text-muted)', fontWeight: 600 }}>
                    {healthScore.score.toFixed(0)}/100
                  </span>
                </div>
                <span style={{
                  marginTop: '0.25rem', padding: '0.15rem 0.5rem', borderRadius: '4px',
                  fontSize: '0.65rem', fontWeight: 700, letterSpacing: '0.05em', fontFamily: 'monospace',
                  background: `${healthScore.signalColor}20`, color: healthScore.signalColor,
                }}>
                  {healthScore.signal}
                </span>
                <span style={{ fontSize: '0.65rem', color: 'var(--text-muted)', display: 'flex', alignItems: 'center', gap: '4px' }}>
                  <FontAwesomeIcon icon={faShieldAlt} style={{ fontSize: '0.55rem' }} /> Deliverability Health
                </span>
              </div>

              {/* Rate Gauges */}
              <div style={{ display: 'flex', flexDirection: 'column', gap: '0.6rem' }}>
                <MiniGauge value={healthScore.deliveryRate} max={1} thresholds={[0.9, 0.95]} label="Delivery Rate" />
                <MiniGauge value={healthScore.openRate} max={1} thresholds={[0.1, 0.2]} label="Open Rate" />
                <MiniGauge value={healthScore.clickRate} max={1} thresholds={[0.01, 0.03]} label="Click Rate" />
              </div>

              {/* Negative Rate Gauges */}
              <div style={{ display: 'flex', flexDirection: 'column', gap: '0.6rem' }}>
                <MiniGauge value={healthScore.bounceRate} max={0.1} thresholds={[0.03, 0.05]} invert label="Bounce Rate" />
                <MiniGauge value={healthScore.complaintRate} max={0.005} thresholds={[0.0003, 0.0005]} invert label="Complaint Rate" />
                {ispAnalysis && (
                  <div style={{ display: 'flex', gap: '0.5rem', justifyContent: 'space-between', fontSize: '0.7rem' }}>
                    <span style={{ display: 'flex', alignItems: 'center', gap: '4px' }}>
                      <span style={{ width: 8, height: 8, borderRadius: '50%', background: '#22c55e', display: 'inline-block' }} />
                      {ispAnalysis.healthy} Healthy
                    </span>
                    <span style={{ display: 'flex', alignItems: 'center', gap: '4px' }}>
                      <span style={{ width: 8, height: 8, borderRadius: '50%', background: '#f59e0b', display: 'inline-block' }} />
                      {ispAnalysis.warning} Warning
                    </span>
                    <span style={{ display: 'flex', alignItems: 'center', gap: '4px' }}>
                      <span style={{ width: 8, height: 8, borderRadius: '50%', background: '#ef4444', display: 'inline-block' }} />
                      {ispAnalysis.critical} Critical
                    </span>
                  </div>
                )}
              </div>
            </div>
          </CardBody>
        </Card>
      )}

      {/* Top Performing ISPs */}
      {topPerformingISPs && (
        <Card className="mb-6">
          <CardHeader title="Top Performing ISPs" />
          <CardBody>
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: '1.5rem' }}>
              {/* By Volume */}
              <div>
                <div style={{ 
                  display: 'flex', 
                  alignItems: 'center', 
                  gap: '0.5rem', 
                  marginBottom: '1rem',
                  color: 'var(--accent-blue)',
                }}>
                  <FontAwesomeIcon icon={faChartBar} />
                  <h4 style={{ margin: 0, fontSize: '0.875rem' }}>By Volume</h4>
                </div>
                <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
                  {topPerformingISPs.byVolume.map((isp, idx) => (
                    <div 
                      key={isp.provider} 
                      style={{
                        display: 'flex',
                        alignItems: 'center',
                        gap: '0.75rem',
                        padding: '0.5rem 0.75rem',
                        backgroundColor: idx === 0 ? 'rgba(59, 130, 246, 0.1)' : 'var(--bg-tertiary)',
                        borderRadius: '6px',
                        borderLeft: idx === 0 ? '3px solid var(--accent-blue)' : 'none',
                      }}
                    >
                      <span style={{ 
                        width: '20px', 
                        textAlign: 'center',
                        fontWeight: 600,
                        color: idx === 0 ? 'var(--accent-blue)' : 'var(--text-muted)',
                        fontSize: '0.75rem',
                      }}>
                        {idx === 0 ? <FontAwesomeIcon icon={faTrophy} /> : `#${idx + 1}`}
                      </span>
                      <span style={{ flex: 1, fontSize: '0.875rem', fontWeight: idx === 0 ? 600 : 400 }}>
                        {isp.provider}
                      </span>
                      <span style={{ 
                        fontFamily: 'monospace', 
                        fontSize: '0.8rem',
                        fontWeight: 600,
                        color: 'var(--accent-blue)',
                      }}>
                        {formatNumber(isp.volume)}
                      </span>
                    </div>
                  ))}
                </div>
              </div>

              {/* By CTR (Click-to-Open) */}
              <div>
                <div style={{ 
                  display: 'flex', 
                  alignItems: 'center', 
                  gap: '0.5rem', 
                  marginBottom: '1rem',
                  color: 'var(--accent-green)',
                }}>
                  <FontAwesomeIcon icon={faMousePointer} />
                  <h4 style={{ margin: 0, fontSize: '0.875rem' }}>By CTR (Click-to-Open)</h4>
                </div>
                <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
                  {topPerformingISPs.byCTR.map((isp, idx) => (
                    <div 
                      key={isp.provider} 
                      style={{
                        display: 'flex',
                        alignItems: 'center',
                        gap: '0.75rem',
                        padding: '0.5rem 0.75rem',
                        backgroundColor: idx === 0 ? 'rgba(34, 197, 94, 0.1)' : 'var(--bg-tertiary)',
                        borderRadius: '6px',
                        borderLeft: idx === 0 ? '3px solid var(--accent-green)' : 'none',
                      }}
                    >
                      <span style={{ 
                        width: '20px', 
                        textAlign: 'center',
                        fontWeight: 600,
                        color: idx === 0 ? 'var(--accent-green)' : 'var(--text-muted)',
                        fontSize: '0.75rem',
                      }}>
                        {idx === 0 ? <FontAwesomeIcon icon={faTrophy} /> : `#${idx + 1}`}
                      </span>
                      <span style={{ flex: 1, fontSize: '0.875rem', fontWeight: idx === 0 ? 600 : 400 }}>
                        {isp.provider}
                      </span>
                      <div style={{ textAlign: 'right' }}>
                        <span style={{ 
                          fontFamily: 'monospace', 
                          fontSize: '0.8rem',
                          fontWeight: 600,
                          color: 'var(--accent-green)',
                        }}>
                          {formatPercent(isp.ctr)}
                        </span>
                        <div style={{ fontSize: '0.65rem', color: 'var(--text-muted)' }}>
                          {formatNumber(isp.clicks)} clicks
                        </div>
                      </div>
                    </div>
                  ))}
                </div>
              </div>

              {/* By Performance Score */}
              <div>
                <div style={{ 
                  display: 'flex', 
                  alignItems: 'center', 
                  gap: '0.5rem', 
                  marginBottom: '1rem',
                  color: 'var(--accent-yellow)',
                }}>
                  <FontAwesomeIcon icon={faArrowUp} />
                  <h4 style={{ margin: 0, fontSize: '0.875rem' }}>By Performance Score</h4>
                </div>
                <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
                  {topPerformingISPs.byPerformance.map((isp, idx) => (
                    <div 
                      key={isp.provider} 
                      style={{
                        display: 'flex',
                        alignItems: 'center',
                        gap: '0.75rem',
                        padding: '0.5rem 0.75rem',
                        backgroundColor: idx === 0 ? 'rgba(234, 179, 8, 0.1)' : 'var(--bg-tertiary)',
                        borderRadius: '6px',
                        borderLeft: idx === 0 ? '3px solid var(--accent-yellow)' : 'none',
                      }}
                    >
                      <span style={{ 
                        width: '20px', 
                        textAlign: 'center',
                        fontWeight: 600,
                        color: idx === 0 ? 'var(--accent-yellow)' : 'var(--text-muted)',
                        fontSize: '0.75rem',
                      }}>
                        {idx === 0 ? <FontAwesomeIcon icon={faTrophy} /> : `#${idx + 1}`}
                      </span>
                      <span style={{ flex: 1, fontSize: '0.875rem', fontWeight: idx === 0 ? 600 : 400 }}>
                        {isp.provider}
                      </span>
                      <div style={{ textAlign: 'right' }}>
                        <span style={{ 
                          fontFamily: 'monospace', 
                          fontSize: '0.8rem',
                          fontWeight: 600,
                          color: 'var(--accent-yellow)',
                        }}>
                          {isp.performanceScore.toFixed(1)}
                        </span>
                        <div style={{ fontSize: '0.65rem', color: 'var(--text-muted)' }}>
                          {formatPercent(isp.deliveryRate)} del
                        </div>
                      </div>
                    </div>
                  ))}
                </div>
              </div>
            </div>

            {/* Legend */}
            <div style={{ 
              marginTop: '1rem', 
              paddingTop: '1rem', 
              borderTop: '1px solid var(--border-color)',
              fontSize: '0.75rem',
              color: 'var(--text-muted)',
              display: 'flex',
              gap: '1.5rem',
            }}>
              <span><strong>Volume:</strong> Total emails sent</span>
              <span><strong>CTR:</strong> Click-to-open rate (clicks / opens)</span>
              <span><strong>Score:</strong> Composite (30% volume, 30% CTR, 20% delivery, 20% opens)</span>
            </div>
          </CardBody>
        </Card>
      )}

      {/* Alerts */}
      {data?.alerts && data.alerts.active_count > 0 && (
        <Card className="mb-6">
          <CardHeader 
            title={`Active Alerts (${data.alerts.active_count})`}
          />
          <CardBody>
            <div style={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
              {data.alerts.items
                .filter((alert) => !alert.acknowledged)
                .slice(0, 5)
                .map((alert) => (
                  <div
                    key={alert.id}
                    style={{
                      padding: '0.75rem 1rem',
                      backgroundColor: alert.severity === 'critical' 
                        ? 'rgba(239, 68, 68, 0.1)' 
                        : 'rgba(234, 179, 8, 0.1)',
                      borderRadius: '0.5rem',
                      borderLeft: `3px solid ${
                        alert.severity === 'critical' 
                          ? 'var(--accent-red)' 
                          : 'var(--accent-yellow)'
                      }`,
                    }}
                  >
                    <div style={{ 
                      display: 'flex', 
                      justifyContent: 'space-between',
                      alignItems: 'flex-start',
                      marginBottom: '0.25rem',
                    }}>
                      <strong style={{ fontSize: '0.875rem' }}>{alert.title}</strong>
                      <span style={{ 
                        fontSize: '0.75rem', 
                        color: 'var(--text-muted)',
                      }}>
                        {new Date(alert.timestamp).toLocaleTimeString()}
                      </span>
                    </div>
                    <p style={{ 
                      fontSize: '0.813rem', 
                      color: 'var(--text-secondary)',
                      marginBottom: '0.5rem',
                    }}>
                      {alert.description}
                    </p>
                    <p style={{ 
                      fontSize: '0.75rem', 
                      color: 'var(--text-muted)',
                    }}>
                      💡 {alert.recommendation}
                    </p>
                  </div>
                ))}
            </div>
          </CardBody>
        </Card>
      )}

      {/* ISP Performance */}
      <Card className="mb-6">
        <CardHeader title="ISP Performance" />
        <CardBody>
          <ISPTable data={data?.isp_metrics ?? []} />
        </CardBody>
      </Card>

      {/* Delivery Breakdown */}
      <div className="grid grid-2">
        <Card>
          <CardHeader title="Delivery Breakdown" />
          <CardBody>
            <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
              <MetricRow
                icon={<FontAwesomeIcon icon={faEnvelope} />}
                label="Delivered"
                value={summary?.total_delivered ?? 0}
                percentage={summary?.delivery_rate ?? 0}
              />
              <MetricRow
                icon={<FontAwesomeIcon icon={faChartLine} />}
                label="Opened"
                value={summary?.total_opened ?? 0}
                percentage={summary?.open_rate ?? 0}
              />
              <MetricRow
                icon={<FontAwesomeIcon icon={faMousePointer} />}
                label="Clicked"
                value={summary?.total_clicked ?? 0}
                percentage={summary?.click_rate ?? 0}
              />
              <MetricRow
                icon={<FontAwesomeIcon icon={faExclamationTriangle} />}
                label="Bounced"
                value={summary?.total_bounced ?? 0}
                percentage={summary?.bounce_rate ?? 0}
                isNegative
              />
              <MetricRow
                icon={<FontAwesomeIcon icon={faBan} />}
                label="Complaints"
                value={summary?.total_complaints ?? 0}
                percentage={summary?.complaint_rate ?? 0}
                isNegative
              />
              <MetricRow
                icon={<FontAwesomeIcon icon={faUserMinus} />}
                label="Unsubscribes"
                value={summary?.total_unsubscribes ?? 0}
                percentage={summary?.unsubscribe_rate ?? 0}
                isNegative
              />
            </div>
          </CardBody>
        </Card>

        <Card>
          <CardHeader title="Top Issues" />
          <CardBody>
            {data?.signals?.top_issues && data.signals.top_issues.length > 0 ? (
              <div style={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
                {data.signals.top_issues.slice(0, 5).map((issue, idx) => (
                  <div
                    key={idx}
                    style={{
                      padding: '0.75rem',
                      backgroundColor: 'var(--bg-tertiary)',
                      borderRadius: '0.5rem',
                    }}
                  >
                    <div style={{ 
                      display: 'flex', 
                      justifyContent: 'space-between',
                      marginBottom: '0.25rem',
                    }}>
                      <span style={{ 
                        fontSize: '0.75rem',
                        padding: '0.125rem 0.5rem',
                        borderRadius: '0.25rem',
                        backgroundColor: issue.severity === 'critical' 
                          ? 'rgba(239, 68, 68, 0.2)' 
                          : 'rgba(234, 179, 8, 0.2)',
                        color: issue.severity === 'critical' 
                          ? 'var(--accent-red)' 
                          : 'var(--accent-yellow)',
                      }}>
                        {issue.category}
                      </span>
                      <span style={{ 
                        fontSize: '0.75rem', 
                        color: 'var(--text-muted)',
                        fontFamily: 'monospace',
                      }}>
                        {issue.count.toLocaleString()}
                      </span>
                    </div>
                    <p style={{ 
                      fontSize: '0.813rem', 
                      color: 'var(--text-secondary)',
                      overflow: 'hidden',
                      textOverflow: 'ellipsis',
                      whiteSpace: 'nowrap',
                    }}>
                      {issue.description}
                    </p>
                  </div>
                ))}
              </div>
            ) : (
              <p style={{ color: 'var(--text-muted)', textAlign: 'center' }}>
                No issues detected
              </p>
            )}
          </CardBody>
        </Card>
      </div>

      {/* Last updated */}
      <div style={{ 
        marginTop: '1.5rem', 
        textAlign: 'center', 
        color: 'var(--text-muted)',
        fontSize: '0.75rem',
      }}>
        Last updated: {data?.last_fetch 
          ? new Date(data.last_fetch).toLocaleString() 
          : 'Never'}
        {' • '}
        Refreshes every minute
      </div>
    </div>
  );
};

interface MetricRowProps {
  icon: React.ReactNode;
  label: string;
  value: number;
  percentage: number;
  isNegative?: boolean;
}

const MetricRow: React.FC<MetricRowProps> = ({ icon, label, value, percentage, isNegative }) => {
  const formatNumber = (n: number): string => {
    if (n >= 1000000) return `${(n / 1000000).toFixed(1)}M`;
    if (n >= 1000) return `${(n / 1000).toFixed(1)}K`;
    return n.toLocaleString();
  };

  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
      <div style={{ 
        color: isNegative ? 'var(--accent-red)' : 'var(--accent-blue)',
        opacity: 0.8,
      }}>
        {icon}
      </div>
      <div style={{ flex: 1 }}>
        <div style={{ fontSize: '0.875rem' }}>{label}</div>
      </div>
      <div style={{ textAlign: 'right' }}>
        <div style={{ 
          fontSize: '0.875rem', 
          fontWeight: 600,
          fontFamily: 'monospace',
        }}>
          {formatNumber(value)}
        </div>
        <div style={{ 
          fontSize: '0.75rem', 
          color: 'var(--text-muted)',
        }}>
          {(percentage * 100).toFixed(2)}%
        </div>
      </div>
    </div>
  );
};

export default DashboardView;
