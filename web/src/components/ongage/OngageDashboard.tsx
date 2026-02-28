import { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { 
  faTachometerAlt, 
  faEnvelope, 
  faFileAlt, 
  faClock, 
  faUsers, 
  faArrowUp,
  faLayerGroup
} from '@fortawesome/free-solid-svg-icons';
import { OngageDashboardResponse, OngageHealthResponse } from '../../types';
import { Card, CardBody, MetricCard, Loading, StatusBadge } from '../common';
import { CampaignPerformance } from './CampaignPerformance';
import { SubjectLineAnalysis } from './SubjectLineAnalysis';
import { ScheduleOptimization } from './ScheduleOptimization';
import { useDateFilter } from '../../context/DateFilterContext';

type TabId = 'overview' | 'campaigns' | 'subjects' | 'schedule' | 'audience' | 'pipeline';

interface Tab {
  id: TabId;
  label: string;
  icon: React.ReactNode;
}

const TABS: Tab[] = [
  { id: 'overview', label: 'Overview', icon: <FontAwesomeIcon icon={faTachometerAlt} /> },
  { id: 'campaigns', label: 'Campaigns', icon: <FontAwesomeIcon icon={faEnvelope} /> },
  { id: 'subjects', label: 'Subject Lines', icon: <FontAwesomeIcon icon={faFileAlt} /> },
  { id: 'schedule', label: 'Send Times', icon: <FontAwesomeIcon icon={faClock} /> },
  { id: 'audience', label: 'Audience', icon: <FontAwesomeIcon icon={faUsers} /> },
  { id: 'pipeline', label: 'Pipeline', icon: <FontAwesomeIcon icon={faArrowUp} /> },
];

export function OngageDashboard() {
  const [activeTab, setActiveTab] = useState<TabId>('overview');
  const [dashboardData, setDashboardData] = useState<OngageDashboardResponse | null>(null);
  const [healthData, setHealthData] = useState<OngageHealthResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  
  // Use global date filter
  const { dateRange } = useDateFilter();

  const fetchData = useCallback(async () => {
    try {
      setLoading(true);
      setError(null);
      
      // Build URL with global date range params
      const params = new URLSearchParams({
        start_date: dateRange.startDate,
        end_date: dateRange.endDate,
        range_type: dateRange.type,
      });
      
      const [dashboardRes, healthRes] = await Promise.all([
        fetch(`/api/ongage/dashboard?${params}`),
        fetch('/api/ongage/health'),
      ]);
      
      if (!dashboardRes.ok) {
        throw new Error(`HTTP ${dashboardRes.status}`);
      }
      
      const dashData = await dashboardRes.json();
      const healthInfo = await healthRes.json();
      
      setDashboardData(dashData);
      setHealthData(healthInfo);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to fetch Ongage data');
    } finally {
      setLoading(false);
    }
  }, [dateRange]);

  useEffect(() => {
    fetchData();
  }, [fetchData]);

  const refetch = () => fetchData();

  const formatNumber = (num: number): string => num?.toLocaleString() || '0';
  const formatPercent = (num: number): string => `${((num || 0) * 100).toFixed(2)}%`;

  if (loading) {
    return <Loading message="Loading Ongage dashboard..." />;
  }

  if (error) {
    return (
      <Card>
        <CardBody>
          <div style={{ textAlign: 'center', padding: '2rem' }}>
            <h3>Error loading Ongage data</h3>
            <p>{error}</p>
            <button 
              onClick={refetch}
              style={{
                marginTop: '1rem',
                padding: '0.5rem 1rem',
                backgroundColor: 'var(--primary-color)',
                color: 'white',
                border: 'none',
                borderRadius: '4px',
                cursor: 'pointer',
              }}
            >
              Retry
            </button>
          </div>
        </CardBody>
      </Card>
    );
  }

  // Calculate summary metrics from dashboard data
  const totalSent = dashboardData?.campaigns?.reduce((sum, c) => sum + c.sent, 0) || 0;
  const totalDelivered = dashboardData?.campaigns?.reduce((sum, c) => sum + c.delivered, 0) || 0;
  const totalOpens = dashboardData?.campaigns?.reduce((sum, c) => sum + c.unique_opens, 0) || 0;
  const totalClicks = dashboardData?.campaigns?.reduce((sum, c) => sum + c.unique_clicks, 0) || 0;

  const avgDeliveryRate = totalSent > 0 ? totalDelivered / totalSent : 0;
  const avgOpenRate = totalSent > 0 ? totalOpens / totalSent : 0;
  const avgClickRate = totalSent > 0 ? totalClicks / totalSent : 0;

  // Get top performing subjects
  const topSubjects = dashboardData?.subject_analysis
    ?.filter(s => s.performance === 'high')
    .slice(0, 3) || [];

  // Get ESP breakdown
  const espBreakdown = dashboardData?.esp_performance || [];

  // Get audience engagement
  const audienceEngagement = dashboardData?.audience_analysis || [];
  const highEngagement = audienceEngagement.filter(a => a.engagement === 'high').length;
  const mediumEngagement = audienceEngagement.filter(a => a.engagement === 'medium').length;
  const lowEngagement = audienceEngagement.filter(a => a.engagement === 'low').length;

  // Today's targeted is available in dashboardData?.today_targeted if needed
  
  const renderOverview = () => (
    <div className="overview-grid">
      {/* Date Range Info */}
      <div style={{ 
        marginBottom: '1rem', 
        padding: '0.75rem 1rem', 
        backgroundColor: 'var(--bg-tertiary)', 
        borderRadius: '8px',
        fontSize: '0.875rem',
        color: 'var(--text-secondary)',
        display: 'flex',
        alignItems: 'center',
        gap: '0.5rem',
      }}>
        <FontAwesomeIcon icon={faClock} />
        <span>
          Showing campaigns from <strong>{dashboardData?.start_date || dateRange.startDate}</strong> to <strong>{dashboardData?.end_date || dateRange.endDate}</strong> ({dateRange.label})
        </span>
        <span style={{ marginLeft: 'auto', fontSize: '0.75rem', color: 'var(--text-muted)' }}>
          Min. audience: 10K
        </span>
      </div>

      {/* Key Metrics */}
      <div className="metrics-row">
        <MetricCard
          label="Total Campaigns"
          value={dashboardData?.total_campaigns || 0}
          subtitle={dateRange.label}
        />
        <MetricCard
          label="Total Sent"
          value={formatNumber(totalSent)}
          subtitle={dateRange.label}
        />
        <MetricCard
          label="Delivery Rate"
          value={formatPercent(avgDeliveryRate)}
          status={avgDeliveryRate >= 0.95 ? 'healthy' : avgDeliveryRate >= 0.90 ? 'warning' : 'critical'}
          subtitle="From campaigns"
        />
        <MetricCard
          label="Open Rate"
          value={formatPercent(avgOpenRate)}
          status={avgOpenRate >= 0.20 ? 'healthy' : avgOpenRate >= 0.12 ? 'warning' : 'critical'}
          subtitle="From campaigns"
        />
        <MetricCard
          label="Click Rate"
          value={formatPercent(avgClickRate)}
          status={avgClickRate >= 0.03 ? 'healthy' : avgClickRate >= 0.015 ? 'warning' : 'critical'}
          subtitle="From campaigns"
        />
      </div>

      {/* Two-column layout */}
      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '1.5rem', marginTop: '1.5rem' }}>
        {/* ESP Performance */}
        <Card>
          <div style={{ padding: '1rem 1.5rem', borderBottom: '1px solid var(--border-color)' }}>
            <h3 style={{ margin: 0 }}>ESP Performance</h3>
          </div>
          <CardBody>
            {espBreakdown.length === 0 ? (
              <p style={{ textAlign: 'center', color: 'var(--text-muted)' }}>No ESP data available</p>
            ) : (
              <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
                {espBreakdown.map(esp => (
                  <div key={esp.connection_id} style={{ 
                    padding: '1rem', 
                    borderRadius: '8px', 
                    backgroundColor: 'var(--card-bg)',
                    border: '1px solid var(--border-color)',
                  }}>
                    <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: '0.5rem' }}>
                      <span style={{ fontWeight: 600 }}>{esp.esp_name || 'Unknown ESP'}</span>
                      <span style={{ fontSize: '0.875rem', color: 'var(--text-muted)' }}>
                        {formatNumber(esp.total_sent)} sent
                      </span>
                    </div>
                    <div style={{ display: 'flex', gap: '1rem', fontSize: '0.875rem' }}>
                      <span>Del: {formatPercent(esp.delivery_rate)}</span>
                      <span>Opens: {formatPercent(esp.open_rate)}</span>
                      <span>Clicks: {formatPercent(esp.click_rate)}</span>
                    </div>
                  </div>
                ))}
              </div>
            )}
          </CardBody>
        </Card>

        {/* Top Subjects */}
        <Card>
          <div style={{ padding: '1rem 1.5rem', borderBottom: '1px solid var(--border-color)' }}>
            <h3 style={{ margin: 0 }}>Top Performing Subjects</h3>
          </div>
          <CardBody>
            {topSubjects.length === 0 ? (
              <p style={{ textAlign: 'center', color: 'var(--text-muted)' }}>No high-performing subjects yet</p>
            ) : (
              <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
                {topSubjects.map((subject, i) => (
                  <div key={i} style={{ 
                    padding: '1rem', 
                    borderRadius: '8px', 
                    backgroundColor: 'var(--card-bg)',
                    border: '1px solid var(--border-color)',
                  }}>
                    <div style={{ 
                      fontWeight: 500, 
                      marginBottom: '0.5rem',
                      overflow: 'hidden',
                      textOverflow: 'ellipsis',
                      whiteSpace: 'nowrap',
                    }}>
                      {subject.subject}
                    </div>
                    <div style={{ display: 'flex', gap: '1rem', fontSize: '0.75rem', color: 'var(--text-muted)' }}>
                      <span>Open: {formatPercent(subject.avg_open_rate)}</span>
                      <span>Click: {formatPercent(subject.avg_click_rate)}</span>
                      <span>{subject.campaign_count} campaigns</span>
                    </div>
                  </div>
                ))}
              </div>
            )}
          </CardBody>
        </Card>
      </div>

      {/* Audience Engagement */}
      <Card style={{ marginTop: '1.5rem' }}>
        <div style={{ padding: '1rem 1.5rem', borderBottom: '1px solid var(--border-color)' }}>
          <h3 style={{ margin: 0 }}>Audience Engagement</h3>
        </div>
        <CardBody>
          <div style={{ display: 'flex', gap: '2rem', justifyContent: 'center' }}>
            <div style={{ textAlign: 'center' }}>
              <div style={{ fontSize: '2rem', fontWeight: 'bold', color: '#48bb78' }}>{highEngagement}</div>
              <div style={{ fontSize: '0.875rem', color: 'var(--text-muted)' }}>High Engagement</div>
            </div>
            <div style={{ textAlign: 'center' }}>
              <div style={{ fontSize: '2rem', fontWeight: 'bold', color: '#ecc94b' }}>{mediumEngagement}</div>
              <div style={{ fontSize: '0.875rem', color: 'var(--text-muted)' }}>Medium Engagement</div>
            </div>
            <div style={{ textAlign: 'center' }}>
              <div style={{ fontSize: '2rem', fontWeight: 'bold', color: '#f56565' }}>{lowEngagement}</div>
              <div style={{ fontSize: '0.875rem', color: 'var(--text-muted)' }}>Low Engagement</div>
            </div>
          </div>
        </CardBody>
      </Card>
    </div>
  );

  const renderAudience = () => (
    <Card>
      <div style={{ padding: '1rem 1.5rem', borderBottom: '1px solid var(--border-color)' }}>
        <h2 style={{ margin: 0 }}>Audience Analysis</h2>
      </div>
      <CardBody>
        {audienceEngagement.length === 0 ? (
          <p style={{ textAlign: 'center', color: 'var(--text-muted)', padding: '2rem' }}>
            No audience data available
          </p>
        ) : (
          <div style={{ overflowX: 'auto' }}>
            <table className="table" style={{ width: '100%' }}>
              <thead>
                <tr>
                  <th>Segment</th>
                  <th style={{ textAlign: 'right' }}>Campaigns</th>
                  <th style={{ textAlign: 'right' }}>Targeted</th>
                  <th style={{ textAlign: 'right' }}>Open Rate</th>
                  <th style={{ textAlign: 'right' }}>Click Rate</th>
                  <th style={{ textAlign: 'center' }}>Engagement</th>
                </tr>
              </thead>
              <tbody>
                {audienceEngagement.map(a => (
                  <tr key={a.segment_id}>
                    <td style={{ fontWeight: 500 }}>{a.segment_name || 'Unknown Segment'}</td>
                    <td style={{ textAlign: 'right' }}>{a.campaign_count}</td>
                    <td style={{ textAlign: 'right' }}>{formatNumber(a.total_targeted)}</td>
                    <td style={{ textAlign: 'right' }}>{formatPercent(a.avg_open_rate)}</td>
                    <td style={{ textAlign: 'right' }}>{formatPercent(a.avg_click_rate)}</td>
                    <td style={{ textAlign: 'center' }}>
                      <StatusBadge 
                        status={a.engagement === 'high' ? 'healthy' : a.engagement === 'medium' ? 'warning' : 'critical'}
                        label={a.engagement.charAt(0).toUpperCase() + a.engagement.slice(1)}
                      />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </CardBody>
    </Card>
  );

  const renderPipeline = () => {
    const pipelineData = dashboardData?.pipeline_metrics || [];
    
    return (
      <Card>
        <div style={{ padding: '1rem 1.5rem', borderBottom: '1px solid var(--border-color)' }}>
          <h2 style={{ margin: 0 }}>Daily Pipeline</h2>
        </div>
        <CardBody>
          {pipelineData.length === 0 ? (
            <p style={{ textAlign: 'center', color: 'var(--text-muted)', padding: '2rem' }}>
              No pipeline data available
            </p>
          ) : (
            <div style={{ overflowX: 'auto' }}>
              <table className="table" style={{ width: '100%' }}>
                <thead>
                  <tr>
                    <th>Date</th>
                    <th style={{ textAlign: 'right' }}>Targeted</th>
                    <th style={{ textAlign: 'right' }}>Sent</th>
                    <th style={{ textAlign: 'right' }}>Delivered</th>
                    <th style={{ textAlign: 'right' }}>Opens</th>
                    <th style={{ textAlign: 'right' }}>Clicks</th>
                    <th style={{ textAlign: 'right' }}>Delivery %</th>
                    <th style={{ textAlign: 'right' }}>Open %</th>
                  </tr>
                </thead>
                <tbody>
                  {pipelineData.map(p => (
                    <tr key={p.date}>
                      <td style={{ fontWeight: 500 }}>{p.date}</td>
                      <td style={{ textAlign: 'right' }}>{formatNumber(p.total_targeted)}</td>
                      <td style={{ textAlign: 'right' }}>{formatNumber(p.total_sent)}</td>
                      <td style={{ textAlign: 'right' }}>{formatNumber(p.total_delivered)}</td>
                      <td style={{ textAlign: 'right' }}>{formatNumber(p.total_opens)}</td>
                      <td style={{ textAlign: 'right' }}>{formatNumber(p.total_clicks)}</td>
                      <td style={{ textAlign: 'right' }}>{formatPercent(p.delivery_rate)}</td>
                      <td style={{ textAlign: 'right' }}>{formatPercent(p.open_rate)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </CardBody>
      </Card>
    );
  };

  // Calculate days from date range for sub-components
  const getDaysFromDateRange = (): number => {
    const start = new Date(dateRange.startDate);
    const end = new Date(dateRange.endDate);
    return Math.max(1, Math.ceil((end.getTime() - start.getTime()) / (1000 * 60 * 60 * 24)) + 1);
  };
  
  const days = getDaysFromDateRange();

  const renderContent = () => {
    switch (activeTab) {
      case 'overview':
        return renderOverview();
      case 'campaigns':
        return <CampaignPerformance days={days} />;
      case 'subjects':
        return <SubjectLineAnalysis days={days} />;
      case 'schedule':
        return <ScheduleOptimization />;
      case 'audience':
        return renderAudience();
      case 'pipeline':
        return renderPipeline();
      default:
        return renderOverview();
    }
  };

  return (
    <div className="ongage-dashboard">
      {/* Header */}
      <div style={{ 
        display: 'flex', 
        justifyContent: 'space-between', 
        alignItems: 'center', 
        marginBottom: '1.5rem',
      }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
          <FontAwesomeIcon icon={faLayerGroup} style={{ color: 'var(--accent-purple, #8b5cf6)', fontSize: '24px' }} />
          <div>
            <h2 style={{ margin: 0 }}>Ongage Campaign Platform</h2>
            <p style={{ margin: '0.25rem 0 0 0', color: 'var(--text-muted)', fontSize: '0.75rem' }}>
              Campaign management across SparkPost, Mailgun, and SES
            </p>
          </div>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
          {/* Show current date range from global filter */}
          <span style={{ 
            padding: '0.25rem 0.75rem', 
            backgroundColor: 'var(--primary-color)', 
            color: 'white',
            borderRadius: '1rem',
            fontSize: '0.75rem',
            fontWeight: 500
          }}>
            {dateRange.label}
          </span>
          <span style={{
            padding: '0.25rem 0.75rem',
            backgroundColor: 'rgba(139, 92, 246, 0.2)',
            color: 'var(--accent-purple, #8b5cf6)',
            borderRadius: '1rem',
            fontSize: '0.75rem',
            fontWeight: 500,
          }}>
            {dashboardData?.total_campaigns || 0} campaigns
          </span>
          <StatusBadge 
            status={healthData?.status === 'healthy' ? 'healthy' : 
                   healthData?.status === 'degraded' ? 'warning' : 'critical'}
            label={healthData?.status || 'Unknown'}
          />
          <span style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>
            Last sync: {dashboardData?.last_fetch 
              ? new Date(dashboardData.last_fetch).toLocaleTimeString() 
              : 'Never'}
          </span>
        </div>
      </div>

      {/* Tabs */}
      <div style={{ 
        display: 'flex', 
        gap: '0.5rem', 
        marginBottom: '1.5rem',
        borderBottom: '1px solid var(--border-color)',
        paddingBottom: '0.5rem',
        overflowX: 'auto',
      }}>
        {TABS.map(tab => (
          <button
            key={tab.id}
            onClick={() => setActiveTab(tab.id)}
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: '0.5rem',
              padding: '0.75rem 1rem',
              border: 'none',
              borderRadius: '4px 4px 0 0',
              backgroundColor: activeTab === tab.id ? 'var(--primary-color)' : 'transparent',
              color: activeTab === tab.id ? 'white' : 'var(--text-color)',
              cursor: 'pointer',
              fontWeight: activeTab === tab.id ? 600 : 400,
              whiteSpace: 'nowrap',
            }}
          >
            {tab.icon}
            {tab.label}
          </button>
        ))}
      </div>

      {/* Content */}
      {renderContent()}

      <style>{`
        .metrics-row {
          display: grid;
          grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
          gap: 1rem;
        }
      `}</style>
    </div>
  );
}

export default OngageDashboard;
