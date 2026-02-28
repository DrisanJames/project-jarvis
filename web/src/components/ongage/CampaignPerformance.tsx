import { useState, useMemo, useEffect, useCallback } from 'react';
import { OngageCampaignsResponse, OngageCampaign } from '../../types';
import { Card, CardBody, SortableTable, StatusBadge, Loading } from '../common';
import type { SortableColumn } from '../common';

interface CampaignPerformanceProps {
  days?: number;
}

export function CampaignPerformance({ days = 1 }: CampaignPerformanceProps) {
  const [data, setData] = useState<OngageCampaignsResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const fetchData = useCallback(async () => {
    try {
      setLoading(true);
      setError(null);
      const response = await fetch(`/api/ongage/campaigns?days=${days}&min_audience=10000`);
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const result = await response.json();
      setData(result);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to fetch campaigns');
    } finally {
      setLoading(false);
    }
  }, [days]);

  useEffect(() => {
    fetchData();
  }, [fetchData]);

  const refetch = () => fetchData();
  
  const [espFilter, setEspFilter] = useState<string>('all');
  const [searchFilter, setSearchFilter] = useState<string>('');
  const [statusFilter, setStatusFilter] = useState<string>('all');

  // Get unique ESPs and statuses for filters
  const { uniqueESPs, uniqueStatuses } = useMemo(() => {
    if (!data?.campaigns) return { uniqueESPs: [], uniqueStatuses: [] };
    const esps = [...new Set(data.campaigns.map(c => c.esp).filter(Boolean))].sort();
    const statuses = [...new Set(data.campaigns.map(c => c.status_desc).filter(Boolean))].sort();
    return { uniqueESPs: esps, uniqueStatuses: statuses };
  }, [data]);

  // Filter data
  const filteredData = useMemo(() => {
    if (!data?.campaigns) return [];
    
    return data.campaigns.filter(c => {
      if (espFilter !== 'all' && c.esp !== espFilter) return false;
      if (statusFilter !== 'all' && c.status_desc !== statusFilter) return false;
      if (searchFilter) {
        const search = searchFilter.toLowerCase();
        if (!c.name.toLowerCase().includes(search) && 
            !c.subject.toLowerCase().includes(search) &&
            !c.id.toLowerCase().includes(search)) {
          return false;
        }
      }
      return true;
    });
  }, [data, espFilter, statusFilter, searchFilter]);

  const formatNumber = (num: number): string => num.toLocaleString();
  const formatPercent = (num: number): string => `${(num * 100).toFixed(2)}%`;
  const formatDate = (dateStr: string): string => {
    if (!dateStr) return '-';
    const date = new Date(dateStr);
    return date.toLocaleDateString() + ' ' + date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  };

  const getESPColor = (esp: string): string => {
    switch (esp?.toLowerCase()) {
      case 'sparkpost': return '#fa6423';
      case 'mailgun': return '#c53030';
      case 'amazon ses': return '#ff9900';
      default: return '#718096';
    }
  };

  const columns: SortableColumn<OngageCampaign>[] = [
    {
      key: 'id',
      header: 'Mailing ID',
      render: (c) => (
        <span style={{ fontFamily: 'monospace', fontSize: '0.8rem' }}>{c.id}</span>
      ),
      width: '110px',
    },
    {
      key: 'name',
      header: 'Campaign',
      render: (c) => (
        <div>
          <div style={{ fontWeight: 500, marginBottom: '0.25rem' }}>{c.name}</div>
          <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)', maxWidth: 300, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
            {c.subject || 'No subject'}
          </div>
        </div>
      ),
      width: '250px',
    },
    {
      key: 'esp',
      header: 'ESP',
      render: (c) => (
        <span style={{
          padding: '0.25rem 0.5rem',
          borderRadius: '4px',
          backgroundColor: getESPColor(c.esp),
          color: 'white',
          fontSize: '0.7rem',
          fontWeight: 'bold',
        }}>
          {c.esp || 'Unknown'}
        </span>
      ),
      width: '100px',
    },
    {
      key: 'schedule_time',
      header: 'Scheduled',
      sortKey: 'schedule_time',
      render: (c) => formatDate(c.schedule_time),
      width: '140px',
    },
    {
      key: 'sent',
      header: 'Sent',
      align: 'right',
      render: (c) => formatNumber(c.sent),
    },
    {
      key: 'delivery_rate',
      header: 'Delivery',
      align: 'right',
      render: (c) => formatPercent(c.delivery_rate),
    },
    {
      key: 'open_rate',
      header: 'Opens',
      align: 'right',
      render: (c) => formatPercent(c.open_rate),
    },
    {
      key: 'click_rate',
      header: 'Clicks',
      align: 'right',
      render: (c) => formatPercent(c.click_rate),
    },
    {
      key: 'ctr',
      header: 'CTR',
      align: 'right',
      render: (c) => formatPercent(c.ctr),
    },
    {
      key: 'complaint_rate',
      header: 'Complaints',
      align: 'right',
      render: (c) => (
        <span style={{ color: c.complaint_rate > 0.001 ? '#c53030' : 'inherit' }}>
          {formatPercent(c.complaint_rate)}
        </span>
      ),
    },
    {
      key: 'status_desc',
      header: 'Status',
      align: 'center',
      render: (c) => {
        const status = c.status_desc.toLowerCase().includes('completed') ? 'healthy' :
                       c.status_desc.toLowerCase().includes('error') ? 'critical' :
                       c.status_desc.toLowerCase().includes('progress') ? 'warning' : 'healthy';
        return <StatusBadge status={status} label={c.status_desc} />;
      },
    },
  ];

  if (loading) {
    return <Loading message="Loading campaign data..." />;
  }

  if (error) {
    return (
      <Card>
        <CardBody>
          <div style={{ textAlign: 'center', padding: '2rem' }}>
            <p>Error loading campaigns: {error}</p>
            <button onClick={refetch} style={{ marginTop: '1rem', padding: '0.5rem 1rem' }}>
              Retry
            </button>
          </div>
        </CardBody>
      </Card>
    );
  }

  // Calculate summary stats
  const summary = filteredData.reduce((acc, c) => ({
    totalSent: acc.totalSent + c.sent,
    totalDelivered: acc.totalDelivered + c.delivered,
    totalOpens: acc.totalOpens + c.unique_opens,
    totalClicks: acc.totalClicks + c.unique_clicks,
    totalComplaints: acc.totalComplaints + c.complaints,
  }), { totalSent: 0, totalDelivered: 0, totalOpens: 0, totalClicks: 0, totalComplaints: 0 });

  return (
    <div className="campaign-performance">
      <Card>
        <div style={{ 
          display: 'flex', 
          justifyContent: 'space-between', 
          alignItems: 'center', 
          padding: '1rem 1.5rem', 
          borderBottom: '1px solid var(--border-color)',
          flexWrap: 'wrap',
          gap: '1rem',
        }}>
          <h2 style={{ margin: 0 }}>Campaign Performance</h2>
          <div style={{ display: 'flex', gap: '0.75rem', flexWrap: 'wrap' }}>
            <select
              value={espFilter}
              onChange={(e) => setEspFilter(e.target.value)}
              style={{
                padding: '0.5rem',
                borderRadius: '4px',
                border: '1px solid var(--border-color)',
                backgroundColor: 'var(--card-bg)',
                color: 'var(--text-color)',
              }}
            >
              <option value="all">All ESPs</option>
              {uniqueESPs.map(esp => (
                <option key={esp} value={esp}>{esp}</option>
              ))}
            </select>
            <select
              value={statusFilter}
              onChange={(e) => setStatusFilter(e.target.value)}
              style={{
                padding: '0.5rem',
                borderRadius: '4px',
                border: '1px solid var(--border-color)',
                backgroundColor: 'var(--card-bg)',
                color: 'var(--text-color)',
              }}
            >
              <option value="all">All Statuses</option>
              {uniqueStatuses.map(status => (
                <option key={status} value={status}>{status}</option>
              ))}
            </select>
            <input
              type="text"
              placeholder="Search campaigns..."
              value={searchFilter}
              onChange={(e) => setSearchFilter(e.target.value)}
              style={{
                padding: '0.5rem',
                borderRadius: '4px',
                border: '1px solid var(--border-color)',
                backgroundColor: 'var(--card-bg)',
                color: 'var(--text-color)',
                width: '200px',
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
          <div style={{ 
            display: 'grid', 
            gridTemplateColumns: 'repeat(auto-fit, minmax(120px, 1fr))', 
            gap: '1rem', 
            marginBottom: '1.5rem' 
          }}>
            <div className="stat-box">
              <div className="stat-label">Campaigns</div>
              <div className="stat-value">{filteredData.length}</div>
            </div>
            <div className="stat-box">
              <div className="stat-label">Total Sent</div>
              <div className="stat-value">{formatNumber(summary.totalSent)}</div>
            </div>
            <div className="stat-box">
              <div className="stat-label">Avg Delivery</div>
              <div className="stat-value">
                {summary.totalSent > 0 ? formatPercent(summary.totalDelivered / summary.totalSent) : '-'}
              </div>
            </div>
            <div className="stat-box">
              <div className="stat-label">Avg Open Rate</div>
              <div className="stat-value">
                {summary.totalSent > 0 ? formatPercent(summary.totalOpens / summary.totalSent) : '-'}
              </div>
            </div>
            <div className="stat-box">
              <div className="stat-label">Avg Click Rate</div>
              <div className="stat-value">
                {summary.totalSent > 0 ? formatPercent(summary.totalClicks / summary.totalSent) : '-'}
              </div>
            </div>
          </div>

          {/* Campaign Table */}
          <SortableTable
            columns={columns}
            data={filteredData}
            keyExtractor={(c) => c.id}
            defaultSortKey="schedule_time"
            defaultSortDirection="desc"
            emptyMessage="No campaigns match your filters"
            stickyHeader
            maxHeight="500px"
          />

          <div style={{ marginTop: '1rem', fontSize: '0.875rem', color: 'var(--text-muted)' }}>
            Showing {filteredData.length} of {data?.count || 0} campaigns
          </div>
        </CardBody>
      </Card>

      <style>{`
        .stat-box {
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
          font-size: 1.25rem;
          font-weight: 600;
        }
      `}</style>
    </div>
  );
}

export default CampaignPerformance;
