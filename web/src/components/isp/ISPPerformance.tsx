import { useState, useMemo, useCallback } from 'react';
import { UnifiedISPResponse } from '../../types';
import { useApi } from '../../hooks/useApi';
import { useDateFilter } from '../../context/DateFilterContext';
import { Card, CardBody } from '../common/Card';
import { Loading } from '../common/Loading';
import { StatusBadge } from '../common/StatusBadge';

// ─── AI Suggestion Types ──────────────────────────────────────────────────
interface AISuggestion {
  id: string;
  priority: 'critical' | 'high' | 'medium' | 'low';
  category: string;
  title: string;
  description: string;
  impact: string;
  metric: string;
  currentValue: string;
  targetValue: string;
  gradeImpact: string;
}

type SortField = 'isp' | 'provider' | 'volume' | 'delivered' | 'delivery_rate' | 'open_rate' | 'click_rate' | 'complaints' | 'bounces' | 'status';
type SortDirection = 'asc' | 'desc';

interface SortConfig {
  field: SortField;
  direction: SortDirection;
}

export function ISPPerformance() {
  // Use global date filter
  const { dateRange } = useDateFilter();
  
  // Build API URL with date range params
  const apiUrl = `/api/isp/unified?start_date=${dateRange.startDate}&end_date=${dateRange.endDate}&range_type=${dateRange.type}`;
  
  const { data, loading, error, refetch } = useApi<UnifiedISPResponse>(apiUrl);
  
  const [sortConfig, setSortConfig] = useState<SortConfig>({ field: 'volume', direction: 'desc' });
  const [ispFilter, setIspFilter] = useState<string>('');
  const [providerFilter, setProviderFilter] = useState<string>('all');
  const [showSuggestions, setShowSuggestions] = useState(true);

  // Get unique providers for filter dropdown
  const uniqueProviders = useMemo(() => {
    if (!data?.metrics) return [];
    return [...new Set(data.metrics.map(m => m.provider))].sort();
  }, [data]);

  // Filter and sort data
  const filteredAndSortedData = useMemo(() => {
    if (!data?.metrics) return [];

    let filtered = [...data.metrics];

    // Apply ISP filter
    if (ispFilter) {
      filtered = filtered.filter(m => m.isp.toLowerCase().includes(ispFilter.toLowerCase()));
    }

    // Apply provider filter
    if (providerFilter !== 'all') {
      filtered = filtered.filter(m => m.provider === providerFilter);
    }

    // Sort
    filtered.sort((a, b) => {
      let aVal: string | number;
      let bVal: string | number;

      switch (sortConfig.field) {
        case 'isp':
          aVal = a.isp.toLowerCase();
          bVal = b.isp.toLowerCase();
          break;
        case 'provider':
          aVal = a.provider;
          bVal = b.provider;
          break;
        case 'volume':
          aVal = a.volume;
          bVal = b.volume;
          break;
        case 'delivered':
          aVal = a.delivered;
          bVal = b.delivered;
          break;
        case 'delivery_rate':
          aVal = a.delivery_rate;
          bVal = b.delivery_rate;
          break;
        case 'open_rate':
          aVal = a.open_rate;
          bVal = b.open_rate;
          break;
        case 'click_rate':
          aVal = a.click_rate;
          bVal = b.click_rate;
          break;
        case 'complaints':
          aVal = a.complaints;
          bVal = b.complaints;
          break;
        case 'bounces':
          aVal = a.bounces;
          bVal = b.bounces;
          break;
        case 'status':
          // Sort by severity: critical > warning > healthy
          const statusOrder = { critical: 3, warning: 2, healthy: 1 };
          aVal = statusOrder[a.status] || 0;
          bVal = statusOrder[b.status] || 0;
          break;
        default:
          return 0;
      }

      if (aVal < bVal) return sortConfig.direction === 'asc' ? -1 : 1;
      if (aVal > bVal) return sortConfig.direction === 'asc' ? 1 : -1;
      return 0;
    });

    return filtered;
  }, [data, sortConfig, ispFilter, providerFilter]);

  // ─── AI-Based Grade Improvement Suggestions ────────────────────────────
  const aiSuggestions = useMemo((): AISuggestion[] => {
    if (!filteredAndSortedData || filteredAndSortedData.length < 2) return [];
    const d = filteredAndSortedData;
    const suggestions: AISuggestion[] = [];
    let sugId = 0;

    // Aggregate metrics
    const totalVol = d.reduce((s, m) => s + m.volume, 0);
    const totalDel = d.reduce((s, m) => s + m.delivered, 0);
    const totalBounces = d.reduce((s, m) => s + m.bounces, 0);
    const totalComplaints = d.reduce((s, m) => s + m.complaints, 0);
    const totalOpens = d.reduce((s, m) => s + m.opens, 0);
    const avgDeliveryRate = totalVol > 0 ? totalDel / totalVol : 0;
    const overallBounceRate = totalVol > 0 ? totalBounces / totalVol : 0;
    const overallComplaintRate = totalVol > 0 ? totalComplaints / totalVol : 0;
    const overallOpenRate = totalVol > 0 ? totalOpens / totalVol : 0;

    const criticalISPs = d.filter(m => m.status === 'critical');
    const warningISPs = d.filter(m => m.status === 'warning');
    const lowDelivISPs = d.filter(m => m.delivery_rate < 0.90);
    const highBounceISPs = d.filter(m => m.volume > 0 && (m.bounces / m.volume) > 0.05);
    const lowOpenISPs = d.filter(m => m.open_rate < 0.05 && m.volume > 100);

    // 1) Critical ISPs — immediate action
    if (criticalISPs.length > 0) {
      const names = criticalISPs.slice(0, 3).map(m => m.isp).join(', ');
      const affectedVol = criticalISPs.reduce((s, m) => s + m.volume, 0);
      const pctOfTotal = totalVol > 0 ? ((affectedVol / totalVol) * 100).toFixed(1) : '0';
      suggestions.push({
        id: `sug-${sugId++}`,
        priority: 'critical',
        category: 'ISP Health',
        title: `${criticalISPs.length} ISP(s) in critical status`,
        description: `${names}${criticalISPs.length > 3 ? ` and ${criticalISPs.length - 3} more` : ''} are flagged critical. These ISPs represent ${pctOfTotal}% of your send volume. Pause or reduce sending to these ISPs immediately, review bounce/complaint patterns, and warm up again gradually.`,
        impact: 'Resolving critical ISPs can improve your health score by 15-25 points.',
        metric: 'Critical ISPs',
        currentValue: `${criticalISPs.length}`,
        targetValue: '0',
        gradeImpact: '+1 grade level',
      });
    }

    // 2) High complaint rate
    if (overallComplaintRate > 0.0003) {
      const worst = [...d].sort((a, b) => {
        const rA = a.volume > 0 ? a.complaints / a.volume : 0;
        const rB = b.volume > 0 ? b.complaints / b.volume : 0;
        return rB - rA;
      }).slice(0, 3);
      const worstNames = worst.filter(m => m.volume > 0 && (m.complaints / m.volume) > 0.0003).map(m => m.isp).join(', ');

      suggestions.push({
        id: `sug-${sugId++}`,
        priority: overallComplaintRate > 0.001 ? 'critical' : 'high',
        category: 'Complaint Management',
        title: 'Complaint rate exceeds safe threshold',
        description: `Your overall complaint rate is ${(overallComplaintRate * 100).toFixed(4)}%. The industry safe threshold is 0.03%. ${worstNames ? `Worst offenders: ${worstNames}.` : ''} Implement one-click unsubscribe headers, review subject lines for misleading content, tighten list hygiene, and ensure you're only mailing opted-in recipients.`,
        impact: 'Reducing complaints below 0.03% can improve score by 10-20 points.',
        metric: 'Complaint Rate',
        currentValue: `${(overallComplaintRate * 100).toFixed(4)}%`,
        targetValue: '< 0.03%',
        gradeImpact: overallComplaintRate > 0.001 ? '+1-2 grade levels' : '+0.5 grade level',
      });
    }

    // 3) High bounce rate
    if (overallBounceRate > 0.03) {
      suggestions.push({
        id: `sug-${sugId++}`,
        priority: overallBounceRate > 0.08 ? 'critical' : 'high',
        category: 'List Hygiene',
        title: 'Bounce rate is above acceptable levels',
        description: `Overall bounce rate is ${(overallBounceRate * 100).toFixed(2)}% (target: < 3%). ${highBounceISPs.length} ISP(s) exceed 5% bounce rate. Run email validation on your lists, remove addresses that hard-bounced, and implement real-time validation on sign-up forms. Consider a re-engagement campaign for stale addresses before removing them.`,
        impact: 'Dropping bounce rate to < 3% can improve score by 10-15 points.',
        metric: 'Bounce Rate',
        currentValue: `${(overallBounceRate * 100).toFixed(2)}%`,
        targetValue: '< 3.00%',
        gradeImpact: '+0.5-1 grade level',
      });
    }

    // 4) Low delivery rate ISPs
    if (lowDelivISPs.length > 0) {
      const lowNames = lowDelivISPs.slice(0, 3).map(m => `${m.isp} (${(m.delivery_rate * 100).toFixed(1)}%)`).join(', ');
      suggestions.push({
        id: `sug-${sugId++}`,
        priority: lowDelivISPs.length > 3 ? 'high' : 'medium',
        category: 'Delivery Optimization',
        title: `${lowDelivISPs.length} ISP(s) below 90% delivery rate`,
        description: `${lowNames}${lowDelivISPs.length > 3 ? ` and ${lowDelivISPs.length - 3} more` : ''} have sub-90% delivery. Check SPF, DKIM, and DMARC alignment for these domains. Review sending IP reputation on Sender Score and Google Postmaster. Reduce volume to underperforming ISPs and focus on engagement-based sending.`,
        impact: 'Improving delivery rate to > 95% across ISPs adds 10-20 points.',
        metric: 'Low Delivery ISPs',
        currentValue: `${lowDelivISPs.length}`,
        targetValue: '0',
        gradeImpact: '+0.5-1 grade level',
      });
    }

    // 5) Low open rates
    if (overallOpenRate < 0.10) {
      suggestions.push({
        id: `sug-${sugId++}`,
        priority: 'medium',
        category: 'Engagement',
        title: 'Open rates indicate low engagement',
        description: `Average open rate is ${(overallOpenRate * 100).toFixed(2)}% (industry benchmark: 15-25%). ${lowOpenISPs.length > 0 ? `${lowOpenISPs.length} ISP(s) are below 5%.` : ''} Improve subject line relevance, segment your audience by engagement recency, implement send-time optimization per ISP, and use pre-header text effectively. Consider sunsetting subscribers who haven't opened in 90+ days.`,
        impact: 'Better engagement signals improve ISP reputation long-term.',
        metric: 'Open Rate',
        currentValue: `${(overallOpenRate * 100).toFixed(2)}%`,
        targetValue: '> 15.00%',
        gradeImpact: 'Indirect — improves ISP trust score',
      });
    }

    // 6) Warning ISPs — proactive
    if (warningISPs.length > 2 && criticalISPs.length === 0) {
      suggestions.push({
        id: `sug-${sugId++}`,
        priority: 'medium',
        category: 'Proactive Monitoring',
        title: `${warningISPs.length} ISPs in warning status — at risk of degrading`,
        description: `${warningISPs.slice(0, 3).map(m => m.isp).join(', ')} are in warning status. Proactively reduce send volume to these ISPs by 20-30%, monitor their postmaster dashboards (Google Postmaster Tools, Microsoft SNDS), and prioritize engaged recipients for these domains.`,
        impact: 'Preventing warnings from becoming critical protects your score.',
        metric: 'Warning ISPs',
        currentValue: `${warningISPs.length}`,
        targetValue: '0',
        gradeImpact: 'Preventive — avoids score drop',
      });
    }

    // 7) Provider-specific advice if filtering
    if (providerFilter !== 'all' && d.length > 0) {
      const providerBounce = totalVol > 0 ? totalBounces / totalVol : 0;
      const providerComplaint = totalVol > 0 ? totalComplaints / totalVol : 0;
      if (providerBounce > 0.02 || providerComplaint > 0.0003) {
        suggestions.push({
          id: `sug-${sugId++}`,
          priority: 'medium',
          category: `${providerFilter.charAt(0).toUpperCase() + providerFilter.slice(1)} Optimization`,
          title: `Optimize ${providerFilter} sending configuration`,
          description: `Your ${providerFilter} metrics show room for improvement. Review sending IP warmup status, check for shared IP reputation issues, verify authentication records (SPF/DKIM/DMARC) are properly aligned for this provider, and consider dedicated IPs if on shared infrastructure.`,
          impact: 'Provider-specific tuning can improve delivery by 5-10%.',
          metric: 'Provider Health',
          currentValue: `${(avgDeliveryRate * 100).toFixed(1)}% delivery`,
          targetValue: '> 98% delivery',
          gradeImpact: '+5-10 points',
        });
      }
    }

    // 8) General best practice if grade is already good
    if (suggestions.length === 0) {
      suggestions.push({
        id: `sug-${sugId++}`,
        priority: 'low',
        category: 'Maintenance',
        title: 'ISP health is strong — focus on maintaining',
        description: 'Your ISP performance is healthy. Continue monitoring daily, maintain list hygiene practices, keep authentication records updated, and watch for seasonal volume changes that could affect reputation. Consider implementing BIMI for enhanced brand visibility in inboxes.',
        impact: 'Maintaining current practices preserves your grade.',
        metric: 'Health Score',
        currentValue: 'Healthy',
        targetValue: 'Maintain A-grade',
        gradeImpact: 'Maintain current grade',
      });
    }

    // Sort by priority
    const priorityOrder = { critical: 0, high: 1, medium: 2, low: 3 };
    suggestions.sort((a, b) => priorityOrder[a.priority] - priorityOrder[b.priority]);

    return suggestions;
  }, [filteredAndSortedData, providerFilter]);

  const dismissSuggestion = useCallback((id: string) => {
    // Could persist dismissed suggestions to localStorage
    const el = document.getElementById(id);
    if (el) {
      el.style.transition = 'all 0.3s ease';
      el.style.opacity = '0';
      el.style.maxHeight = '0';
      el.style.padding = '0';
      el.style.margin = '0';
      el.style.overflow = 'hidden';
    }
  }, []);

  // Handle column header click
  const handleSort = (field: SortField) => {
    setSortConfig(prev => ({
      field,
      direction: prev.field === field && prev.direction === 'desc' ? 'asc' : 'desc',
    }));
  };

  // Get sort indicator
  const getSortIndicator = (field: SortField) => {
    if (sortConfig.field !== field) return ' ↕';
    return sortConfig.direction === 'asc' ? ' ↑' : ' ↓';
  };

  // Format numbers
  const formatNumber = (num: number): string => {
    return num.toLocaleString();
  };

  const formatPercent = (num: number): string => {
    return `${(num * 100).toFixed(2)}%`;
  };

  // Provider badge color
  const getProviderColor = (provider: string): string => {
    switch (provider) {
      case 'sparkpost':
        return '#fa6423';
      case 'mailgun':
        return '#c53030';
      case 'ses':
        return '#ff9900';
      default:
        return '#718096';
    }
  };

  if (loading) {
    return <Loading message="Loading ISP performance data..." />;
  }

  if (error) {
    return (
      <Card>
        <CardBody>
          <div className="error-state">
            <p>Error loading ISP data: {error}</p>
            <button onClick={refetch} className="retry-button">
              Retry
            </button>
          </div>
        </CardBody>
      </Card>
    );
  }

  // Calculate totals
  const totals = filteredAndSortedData.reduce(
    (acc, m) => ({
      volume: acc.volume + m.volume,
      delivered: acc.delivered + m.delivered,
      opens: acc.opens + m.opens,
      clicks: acc.clicks + m.clicks,
      bounces: acc.bounces + m.bounces,
      complaints: acc.complaints + m.complaints,
    }),
    { volume: 0, delivered: 0, opens: 0, clicks: 0, bounces: 0, complaints: 0 }
  );

  return (
    <div className="isp-performance">
      <Card>
        <div className="card-header" style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: '1rem', padding: '1rem 1.5rem', borderBottom: '1px solid var(--border-color)' }}>
          <h2 style={{ margin: 0 }}>ISP Performance - All Providers</h2>
          <div style={{ display: 'flex', gap: '1rem', alignItems: 'center' }}>
            <select
              value={providerFilter}
              onChange={(e) => setProviderFilter(e.target.value)}
              style={{
                padding: '0.5rem',
                borderRadius: '4px',
                border: '1px solid var(--border-color)',
                backgroundColor: 'var(--card-bg)',
                color: 'var(--text-color)',
              }}
            >
              <option value="all">All Providers</option>
              {uniqueProviders.map(p => (
                <option key={p} value={p}>{p.charAt(0).toUpperCase() + p.slice(1)}</option>
              ))}
            </select>
            <input
              type="text"
              placeholder="Filter by ISP..."
              value={ispFilter}
              onChange={(e) => setIspFilter(e.target.value)}
              style={{
                padding: '0.5rem',
                borderRadius: '4px',
                border: '1px solid var(--border-color)',
                backgroundColor: 'var(--card-bg)',
                color: 'var(--text-color)',
                width: '150px',
              }}
            />
            <button
              onClick={refetch}
              style={{
                padding: '0.5rem 1rem',
                borderRadius: '4px',
                border: 'none',
                backgroundColor: 'var(--primary-color)',
                color: 'white',
                cursor: 'pointer',
              }}
            >
              Refresh
            </button>
          </div>
        </div>
        <CardBody>
          {/* Summary Stats */}
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(150px, 1fr))', gap: '1rem', marginBottom: '1rem' }}>
            <div className="summary-stat">
              <div className="stat-label">Total Volume</div>
              <div className="stat-value">{formatNumber(totals.volume)}</div>
            </div>
            <div className="summary-stat">
              <div className="stat-label">Total Delivered</div>
              <div className="stat-value">{formatNumber(totals.delivered)}</div>
            </div>
            <div className="summary-stat">
              <div className="stat-label">Avg Delivery Rate</div>
              <div className="stat-value">{totals.volume > 0 ? formatPercent(totals.delivered / totals.volume) : '-'}</div>
            </div>
            <div className="summary-stat">
              <div className="stat-label">Total Bounces</div>
              <div className="stat-value">{formatNumber(totals.bounces)}</div>
            </div>
            <div className="summary-stat">
              <div className="stat-label">Total Complaints</div>
              <div className="stat-value">{formatNumber(totals.complaints)}</div>
            </div>
            <div className="summary-stat">
              <div className="stat-label">ISP Count</div>
              <div className="stat-value">{filteredAndSortedData.length}</div>
            </div>
          </div>

          {/* Statistical Distribution Analysis */}
          {filteredAndSortedData.length >= 3 && (() => {
            const d = filteredAndSortedData;
            const healthyCount = d.filter(m => m.status === 'healthy').length;
            const warningCount = d.filter(m => m.status === 'warning').length;
            const criticalCount = d.filter(m => m.status === 'critical').length;
            const healthPct = (healthyCount / d.length) * 100;

            const delivRates = d.map(m => m.delivery_rate);
            const openRates = d.map(m => m.open_rate);
            const mean = (a: number[]) => a.reduce((x, y) => x + y, 0) / a.length;
            const median = (a: number[]) => { const s = [...a].sort((x, y) => x - y); const m = Math.floor(s.length / 2); return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2; };
            const stdDev = (a: number[]) => { const m = mean(a); return Math.sqrt(a.reduce((s, v) => s + (v - m) ** 2, 0) / a.length); };

            const avgDelivery = mean(delivRates);
            const medDelivery = median(delivRates);
            const sdDelivery = stdDev(delivRates);
            const avgOpen = mean(openRates);
            const complaintRate = totals.volume > 0 ? totals.complaints / totals.volume : 0;
            const bounceRate = totals.volume > 0 ? totals.bounces / totals.volume : 0;

            // Health score (0-100)
            const score = Math.min(100, Math.max(0,
              (healthPct * 0.3) +
              (avgDelivery * 100 * 0.3) +
              ((1 - bounceRate) * 100 * 0.2) +
              ((1 - Math.min(complaintRate * 1000, 1)) * 100 * 0.2)
            ));
            const grade = score >= 90 ? 'A' : score >= 80 ? 'B' : score >= 70 ? 'C' : score >= 60 ? 'D' : 'F';
            const gradeColor = score >= 90 ? '#22c55e' : score >= 80 ? '#3b82f6' : score >= 70 ? '#f59e0b' : '#ef4444';

            return (
              <div className="isp-analytics-bar" style={{
                display: 'grid', gridTemplateColumns: '120px 1fr 1fr 1fr', gap: '1rem',
                padding: '1rem', marginBottom: '1.5rem',
                background: 'var(--bg-secondary, #1e1e30)', border: '1px solid var(--border-color, #333)', borderRadius: '10px'
              }}>
                {/* Health Grade */}
                <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center' }}>
                  <div style={{
                    width: 64, height: 64, borderRadius: '50%', border: `3px solid ${gradeColor}`,
                    display: 'flex', alignItems: 'center', justifyContent: 'center', flexDirection: 'column'
                  }}>
                    <span style={{ fontSize: '1.5rem', fontWeight: 800, color: gradeColor, fontFamily: 'monospace', lineHeight: 1 }}>{grade}</span>
                    <span style={{ fontSize: '0.55rem', color: 'var(--text-muted)' }}>{score.toFixed(0)}/100</span>
                  </div>
                  <span style={{ fontSize: '0.6rem', color: 'var(--text-muted)', marginTop: 4 }}>ISP Health</span>
                </div>

                {/* Status Distribution */}
                <div style={{ display: 'flex', flexDirection: 'column', gap: 4, justifyContent: 'center' }}>
                  <span style={{ fontSize: '0.65rem', color: 'var(--text-muted)', textTransform: 'uppercase', fontWeight: 700, letterSpacing: '0.04em' }}>Status Distribution</span>
                  <div style={{ display: 'flex', height: 8, borderRadius: 4, overflow: 'hidden' }}>
                    <div style={{ width: `${(healthyCount / d.length) * 100}%`, background: '#22c55e' }} />
                    <div style={{ width: `${(warningCount / d.length) * 100}%`, background: '#f59e0b' }} />
                    <div style={{ width: `${(criticalCount / d.length) * 100}%`, background: '#ef4444' }} />
                  </div>
                  <div style={{ display: 'flex', gap: '0.75rem', fontSize: '0.7rem' }}>
                    <span style={{ color: '#22c55e' }}>{healthyCount} Healthy</span>
                    <span style={{ color: '#f59e0b' }}>{warningCount} Warning</span>
                    <span style={{ color: '#ef4444' }}>{criticalCount} Critical</span>
                  </div>
                </div>

                {/* Delivery Rate Stats */}
                <div style={{ display: 'flex', flexDirection: 'column', gap: 2, justifyContent: 'center' }}>
                  <span style={{ fontSize: '0.65rem', color: 'var(--text-muted)', textTransform: 'uppercase', fontWeight: 700, letterSpacing: '0.04em' }}>Delivery Rate Distribution</span>
                  <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: '0.5rem', fontSize: '0.75rem' }}>
                    <div><span style={{ color: 'var(--text-muted)', fontSize: '0.6rem' }}>MEAN</span><br /><span style={{ fontFamily: 'monospace', fontWeight: 600 }}>{(avgDelivery * 100).toFixed(2)}%</span></div>
                    <div><span style={{ color: 'var(--text-muted)', fontSize: '0.6rem' }}>MEDIAN</span><br /><span style={{ fontFamily: 'monospace', fontWeight: 600 }}>{(medDelivery * 100).toFixed(2)}%</span></div>
                    <div><span style={{ color: 'var(--text-muted)', fontSize: '0.6rem' }}>STD DEV</span><br /><span style={{ fontFamily: 'monospace', fontWeight: 600 }}>{(sdDelivery * 100).toFixed(2)}%</span></div>
                  </div>
                </div>

                {/* Open Rate & Risk */}
                <div style={{ display: 'flex', flexDirection: 'column', gap: 2, justifyContent: 'center' }}>
                  <span style={{ fontSize: '0.65rem', color: 'var(--text-muted)', textTransform: 'uppercase', fontWeight: 700, letterSpacing: '0.04em' }}>Risk Indicators</span>
                  <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '0.5rem', fontSize: '0.75rem' }}>
                    <div><span style={{ color: 'var(--text-muted)', fontSize: '0.6rem' }}>AVG OPEN</span><br /><span style={{ fontFamily: 'monospace', fontWeight: 600 }}>{(avgOpen * 100).toFixed(2)}%</span></div>
                    <div><span style={{ color: 'var(--text-muted)', fontSize: '0.6rem' }}>BOUNCE</span><br /><span style={{ fontFamily: 'monospace', fontWeight: 600, color: bounceRate > 0.05 ? '#ef4444' : bounceRate > 0.03 ? '#f59e0b' : '#22c55e' }}>{(bounceRate * 100).toFixed(2)}%</span></div>
                    <div><span style={{ color: 'var(--text-muted)', fontSize: '0.6rem' }}>COMPLAINTS</span><br /><span style={{ fontFamily: 'monospace', fontWeight: 600, color: complaintRate > 0.0005 ? '#ef4444' : '#22c55e' }}>{(complaintRate * 100).toFixed(4)}%</span></div>
                    <div><span style={{ color: 'var(--text-muted)', fontSize: '0.6rem' }}>RISK</span><br /><span style={{ fontFamily: 'monospace', fontWeight: 600, color: criticalCount > 0 ? '#ef4444' : warningCount > 2 ? '#f59e0b' : '#22c55e' }}>{criticalCount > 0 ? 'HIGH' : warningCount > 2 ? 'MODERATE' : 'LOW'}</span></div>
                  </div>
                </div>
              </div>
            );
          })()}

          {/* AI-Based Suggestions for Improving Grade */}
          {aiSuggestions.length > 0 && showSuggestions && (
            <div className="isp-ai-suggestions" style={{
              marginBottom: '1.5rem',
              background: 'var(--bg-secondary, #1e1e30)',
              border: '1px solid var(--border-color, #333)',
              borderRadius: '10px',
              overflow: 'hidden',
            }}>
              <div style={{
                display: 'flex', alignItems: 'center', justifyContent: 'space-between',
                padding: '0.75rem 1rem',
                background: 'linear-gradient(135deg, rgba(99,102,241,0.08), rgba(139,92,246,0.05))',
                borderBottom: '1px solid var(--border-color, #333)',
              }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                  <span style={{ fontSize: '1rem' }}>🧠</span>
                  <span style={{ fontSize: '0.8rem', fontWeight: 700, color: '#a5b4fc', letterSpacing: '0.03em' }}>
                    AI Grade Improvement Suggestions
                  </span>
                  <span style={{
                    fontSize: '0.6rem', fontWeight: 700, padding: '2px 8px', borderRadius: '8px',
                    background: 'rgba(99,102,241,0.15)', color: '#818cf8', letterSpacing: '0.05em',
                  }}>
                    {aiSuggestions.length} ACTION{aiSuggestions.length !== 1 ? 'S' : ''}
                  </span>
                </div>
                <button
                  onClick={() => setShowSuggestions(false)}
                  style={{
                    background: 'none', border: 'none', color: 'var(--text-muted)', cursor: 'pointer',
                    fontSize: '0.7rem', padding: '4px 8px', borderRadius: '4px',
                  }}
                  title="Hide suggestions"
                >
                  ✕ Hide
                </button>
              </div>

              <div style={{ padding: '0.75rem', display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
                {aiSuggestions.map((sug) => {
                  const priorityStyles: Record<string, { bg: string; border: string; badge: string; badgeBg: string }> = {
                    critical: { bg: 'rgba(239,68,68,0.04)', border: 'rgba(239,68,68,0.2)', badge: '#f87171', badgeBg: 'rgba(239,68,68,0.12)' },
                    high:     { bg: 'rgba(245,158,11,0.04)', border: 'rgba(245,158,11,0.2)', badge: '#fbbf24', badgeBg: 'rgba(245,158,11,0.12)' },
                    medium:   { bg: 'rgba(99,102,241,0.04)', border: 'rgba(99,102,241,0.15)', badge: '#818cf8', badgeBg: 'rgba(99,102,241,0.1)' },
                    low:      { bg: 'rgba(16,185,129,0.04)', border: 'rgba(16,185,129,0.15)', badge: '#34d399', badgeBg: 'rgba(16,185,129,0.1)' },
                  };
                  const ps = priorityStyles[sug.priority] || priorityStyles.medium;

                  return (
                    <div
                      key={sug.id}
                      id={sug.id}
                      style={{
                        background: ps.bg,
                        border: `1px solid ${ps.border}`,
                        borderRadius: '8px',
                        padding: '0.75rem 1rem',
                        transition: 'all 0.3s ease',
                      }}
                    >
                      <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: '0.75rem' }}>
                        <div style={{ flex: 1 }}>
                          {/* Header row */}
                          <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.35rem', flexWrap: 'wrap' }}>
                            <span style={{
                              fontSize: '0.55rem', fontWeight: 700, textTransform: 'uppercase', letterSpacing: '0.06em',
                              padding: '2px 6px', borderRadius: '4px',
                              background: ps.badgeBg, color: ps.badge,
                            }}>
                              {sug.priority}
                            </span>
                            <span style={{
                              fontSize: '0.55rem', fontWeight: 600, color: 'var(--text-muted)',
                              textTransform: 'uppercase', letterSpacing: '0.04em',
                            }}>
                              {sug.category}
                            </span>
                            <span style={{ fontSize: '0.8rem', fontWeight: 700, color: 'var(--text-color, #e0e0e0)' }}>
                              {sug.title}
                            </span>
                          </div>

                          {/* Description */}
                          <p style={{
                            fontSize: '0.72rem', lineHeight: 1.55, color: 'var(--text-muted, #999)',
                            margin: '0 0 0.5rem',
                          }}>
                            {sug.description}
                          </p>

                          {/* Metrics row */}
                          <div style={{ display: 'flex', gap: '1rem', flexWrap: 'wrap', alignItems: 'center' }}>
                            <div style={{ display: 'flex', alignItems: 'center', gap: '0.35rem' }}>
                              <span style={{ fontSize: '0.6rem', color: 'var(--text-muted)', textTransform: 'uppercase', letterSpacing: '0.04em' }}>Current:</span>
                              <span style={{ fontSize: '0.7rem', fontWeight: 700, fontFamily: 'monospace', color: ps.badge }}>{sug.currentValue}</span>
                            </div>
                            <span style={{ fontSize: '0.7rem', color: 'var(--text-muted)' }}>→</span>
                            <div style={{ display: 'flex', alignItems: 'center', gap: '0.35rem' }}>
                              <span style={{ fontSize: '0.6rem', color: 'var(--text-muted)', textTransform: 'uppercase', letterSpacing: '0.04em' }}>Target:</span>
                              <span style={{ fontSize: '0.7rem', fontWeight: 700, fontFamily: 'monospace', color: '#34d399' }}>{sug.targetValue}</span>
                            </div>
                            <div style={{
                              marginLeft: 'auto',
                              display: 'flex', alignItems: 'center', gap: '0.35rem',
                              padding: '2px 8px', borderRadius: '6px',
                              background: 'rgba(16,185,129,0.08)', border: '1px solid rgba(16,185,129,0.15)',
                            }}>
                              <span style={{ fontSize: '0.6rem', color: '#34d399', fontWeight: 600 }}>IMPACT: {sug.gradeImpact}</span>
                            </div>
                          </div>
                        </div>

                        {/* Dismiss button */}
                        <button
                          onClick={() => dismissSuggestion(sug.id)}
                          style={{
                            background: 'none', border: 'none', color: 'var(--text-muted)',
                            cursor: 'pointer', fontSize: '0.7rem', padding: '2px',
                            opacity: 0.5, flexShrink: 0,
                          }}
                          title="Dismiss suggestion"
                        >
                          ✕
                        </button>
                      </div>
                    </div>
                  );
                })}
              </div>
            </div>
          )}

          {/* Show suggestions toggle when hidden */}
          {!showSuggestions && aiSuggestions.length > 0 && (
            <div style={{ marginBottom: '1rem', textAlign: 'right' }}>
              <button
                onClick={() => setShowSuggestions(true)}
                style={{
                  background: 'rgba(99,102,241,0.08)', border: '1px solid rgba(99,102,241,0.2)',
                  color: '#818cf8', cursor: 'pointer', fontSize: '0.7rem', fontWeight: 600,
                  padding: '6px 14px', borderRadius: '6px',
                }}
              >
                🧠 Show AI Suggestions ({aiSuggestions.length})
              </button>
            </div>
          )}

          {/* Data Table */}
          <div className="table-container" style={{ overflowX: 'auto' }}>
            <table className="table" style={{ minWidth: '900px' }}>
              <thead>
                <tr>
                  <th
                    onClick={() => handleSort('provider')}
                    style={{ cursor: 'pointer', userSelect: 'none' }}
                  >
                    Provider{getSortIndicator('provider')}
                  </th>
                  <th
                    onClick={() => handleSort('isp')}
                    style={{ cursor: 'pointer', userSelect: 'none' }}
                  >
                    ISP{getSortIndicator('isp')}
                  </th>
                  <th
                    onClick={() => handleSort('volume')}
                    style={{ cursor: 'pointer', userSelect: 'none', textAlign: 'right' }}
                  >
                    Volume{getSortIndicator('volume')}
                  </th>
                  <th
                    onClick={() => handleSort('delivered')}
                    style={{ cursor: 'pointer', userSelect: 'none', textAlign: 'right' }}
                  >
                    Delivered{getSortIndicator('delivered')}
                  </th>
                  <th
                    onClick={() => handleSort('delivery_rate')}
                    style={{ cursor: 'pointer', userSelect: 'none', textAlign: 'right' }}
                  >
                    Delivery %{getSortIndicator('delivery_rate')}
                  </th>
                  <th
                    onClick={() => handleSort('open_rate')}
                    style={{ cursor: 'pointer', userSelect: 'none', textAlign: 'right' }}
                  >
                    Open Rate{getSortIndicator('open_rate')}
                  </th>
                  <th
                    onClick={() => handleSort('click_rate')}
                    style={{ cursor: 'pointer', userSelect: 'none', textAlign: 'right' }}
                  >
                    CTR{getSortIndicator('click_rate')}
                  </th>
                  <th
                    onClick={() => handleSort('bounces')}
                    style={{ cursor: 'pointer', userSelect: 'none', textAlign: 'right' }}
                  >
                    Bounces{getSortIndicator('bounces')}
                  </th>
                  <th
                    onClick={() => handleSort('complaints')}
                    style={{ cursor: 'pointer', userSelect: 'none', textAlign: 'right' }}
                  >
                    Complaints{getSortIndicator('complaints')}
                  </th>
                  <th
                    onClick={() => handleSort('status')}
                    style={{ cursor: 'pointer', userSelect: 'none', textAlign: 'center' }}
                  >
                    Status{getSortIndicator('status')}
                  </th>
                </tr>
              </thead>
              <tbody>
                {filteredAndSortedData.length === 0 ? (
                  <tr>
                    <td colSpan={10} style={{ textAlign: 'center', padding: '2rem' }}>
                      No ISP data available
                    </td>
                  </tr>
                ) : (
                  filteredAndSortedData.map((metric, index) => (
                    <tr key={`${metric.provider}-${metric.isp}-${index}`}>
                      <td>
                        <span
                          style={{
                            padding: '0.25rem 0.5rem',
                            borderRadius: '4px',
                            backgroundColor: getProviderColor(metric.provider),
                            color: 'white',
                            fontSize: '0.75rem',
                            fontWeight: 'bold',
                            textTransform: 'uppercase',
                          }}
                        >
                          {metric.provider}
                        </span>
                      </td>
                      <td style={{ fontWeight: 500 }}>{metric.isp}</td>
                      <td style={{ textAlign: 'right' }}>{formatNumber(metric.volume)}</td>
                      <td style={{ textAlign: 'right' }}>{formatNumber(metric.delivered)}</td>
                      <td style={{ textAlign: 'right' }}>{formatPercent(metric.delivery_rate)}</td>
                      <td style={{ textAlign: 'right' }}>{formatPercent(metric.open_rate)}</td>
                      <td style={{ textAlign: 'right' }}>{formatPercent(metric.click_rate)}</td>
                      <td style={{ textAlign: 'right' }}>{formatNumber(metric.bounces)}</td>
                      <td style={{ textAlign: 'right' }}>{formatNumber(metric.complaints)}</td>
                      <td style={{ textAlign: 'center' }}>
                        <StatusBadge status={metric.status} />
                      </td>
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>

          {/* Legend */}
          <div style={{ marginTop: '1rem', display: 'flex', gap: '1rem', fontSize: '0.875rem', color: 'var(--text-muted)' }}>
            <span>Click column headers to sort</span>
            <span>•</span>
            <span>Active providers: {data?.providers?.join(', ') || 'None'}</span>
          </div>
        </CardBody>
      </Card>

      <style>{`
        .summary-stat {
          background: var(--card-bg);
          padding: 1rem;
          border-radius: 8px;
          border: 1px solid var(--border-color);
        }
        .stat-label {
          font-size: 0.75rem;
          color: var(--text-muted);
          text-transform: uppercase;
          margin-bottom: 0.25rem;
        }
        .stat-value {
          font-size: 1.5rem;
          font-weight: 600;
          color: var(--text-color);
        }
        .table th {
          background: var(--table-header-bg, #f7fafc);
          position: sticky;
          top: 0;
          z-index: 1;
        }
        .table th:hover {
          background: var(--table-header-hover, #edf2f7);
        }
        .table tr:hover {
          background: var(--table-row-hover, #f7fafc);
        }
        .error-state {
          text-align: center;
          padding: 2rem;
        }
        .retry-button {
          margin-top: 1rem;
          padding: 0.5rem 1rem;
          background: var(--primary-color);
          color: white;
          border: none;
          border-radius: 4px;
          cursor: pointer;
        }
        .retry-button:hover {
          opacity: 0.9;
        }
      `}</style>
    </div>
  );
}

export default ISPPerformance;
