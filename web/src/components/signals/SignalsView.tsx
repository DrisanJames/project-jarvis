import React from 'react';
import { Card, CardHeader, CardBody, Table, Loading, ErrorDisplay } from '../common';
import { useApi } from '../../hooks/useApi';
import { useDateFilter } from '../../context/DateFilterContext';
import type { SignalsData, BounceReason, DelayReason, RejectionReason } from '../../types';

const POLLING_INTERVAL = 60000;

export const SignalsView: React.FC = () => {
  // Use global date filter
  const { dateRange } = useDateFilter();
  
  // Build API URL with date range params
  const apiUrl = `/api/metrics/signals?start_date=${dateRange.startDate}&end_date=${dateRange.endDate}&range_type=${dateRange.type}`;
  
  const { data, loading, error, refetch } = useApi<SignalsData>(
    apiUrl,
    { pollingInterval: POLLING_INTERVAL }
  );

  if (loading && !data) {
    return <Loading message="Loading signals..." />;
  }

  if (error) {
    return <ErrorDisplay message={error} onRetry={refetch} />;
  }

  const formatNumber = (n: number): string => {
    if (n >= 1000000) return `${(n / 1000000).toFixed(1)}M`;
    if (n >= 1000) return `${(n / 1000).toFixed(1)}K`;
    return n.toLocaleString();
  };

  const bounceColumns = [
    {
      key: 'reason',
      header: 'Reason',
      render: (item: BounceReason) => (
        <div style={{ maxWidth: '400px' }}>
          <div style={{ fontSize: '0.875rem', marginBottom: '0.25rem' }}>
            {item.reason.length > 100 ? item.reason.substring(0, 100) + '...' : item.reason}
          </div>
          <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>
            {item.bounce_class_name} - {item.bounce_category_name}
          </div>
        </div>
      ),
    },
    {
      key: 'domain',
      header: 'Domain',
      render: (item: BounceReason) => (
        <span style={{ fontSize: '0.875rem' }}>{item.domain || '-'}</span>
      ),
    },
    {
      key: 'count_bounce',
      header: 'Count',
      align: 'right' as const,
      render: (item: BounceReason) => (
        <span className="font-mono">{formatNumber(item.count_bounce)}</span>
      ),
    },
  ];

  const delayColumns = [
    {
      key: 'reason',
      header: 'Reason',
      render: (item: DelayReason) => (
        <div style={{ maxWidth: '500px', fontSize: '0.875rem' }}>
          {item.reason.length > 150 ? item.reason.substring(0, 150) + '...' : item.reason}
        </div>
      ),
    },
    {
      key: 'domain',
      header: 'Domain',
      render: (item: DelayReason) => (
        <span style={{ fontSize: '0.875rem' }}>{item.domain || '-'}</span>
      ),
    },
    {
      key: 'count_delayed',
      header: 'Count',
      align: 'right' as const,
      render: (item: DelayReason) => (
        <span className="font-mono">{formatNumber(item.count_delayed)}</span>
      ),
    },
  ];

  const rejectionColumns = [
    {
      key: 'reason',
      header: 'Reason',
      render: (item: RejectionReason) => (
        <div style={{ maxWidth: '400px' }}>
          <div style={{ fontSize: '0.875rem', marginBottom: '0.25rem' }}>
            {item.reason.length > 100 ? item.reason.substring(0, 100) + '...' : item.reason}
          </div>
          <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>
            {item.rejection_type}
          </div>
        </div>
      ),
    },
    {
      key: 'domain',
      header: 'Domain',
      render: (item: RejectionReason) => (
        <span style={{ fontSize: '0.875rem' }}>{item.domain || '-'}</span>
      ),
    },
    {
      key: 'count_rejected',
      header: 'Count',
      align: 'right' as const,
      render: (item: RejectionReason) => (
        <span className="font-mono">{formatNumber(item.count_rejected)}</span>
      ),
    },
  ];

  return (
    <div>
      {/* Top Issues */}
      {data?.top_issues && data.top_issues.length > 0 && (
        <Card className="mb-6">
          <CardHeader title="🚨 Top Issues" />
          <CardBody>
            <div style={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
              {data.top_issues.map((issue, idx) => (
                <div
                  key={idx}
                  style={{
                    padding: '1rem',
                    backgroundColor: issue.severity === 'critical' 
                      ? 'rgba(239, 68, 68, 0.1)' 
                      : 'rgba(234, 179, 8, 0.1)',
                    borderRadius: '0.5rem',
                    borderLeft: `3px solid ${
                      issue.severity === 'critical' 
                        ? 'var(--accent-red)' 
                        : 'var(--accent-yellow)'
                    }`,
                  }}
                >
                  <div style={{ 
                    display: 'flex', 
                    justifyContent: 'space-between',
                    alignItems: 'flex-start',
                    marginBottom: '0.5rem',
                  }}>
                    <div>
                      <span style={{ 
                        fontSize: '0.75rem',
                        padding: '0.125rem 0.5rem',
                        borderRadius: '0.25rem',
                        backgroundColor: 'rgba(0,0,0,0.2)',
                        marginRight: '0.5rem',
                      }}>
                        {issue.category}
                      </span>
                      {issue.affected_isp && (
                        <span style={{ fontSize: '0.875rem', fontWeight: 500 }}>
                          {issue.affected_isp}
                        </span>
                      )}
                      {issue.affected_ip && (
                        <span style={{ fontSize: '0.875rem', fontFamily: 'monospace' }}>
                          {issue.affected_ip}
                        </span>
                      )}
                    </div>
                    <span style={{ 
                      fontSize: '0.875rem', 
                      fontWeight: 600,
                      fontFamily: 'monospace',
                    }}>
                      {formatNumber(issue.count)}
                    </span>
                  </div>
                  <p style={{ 
                    fontSize: '0.875rem', 
                    color: 'var(--text-secondary)',
                    marginBottom: '0.5rem',
                  }}>
                    {issue.description}
                  </p>
                  <p style={{ 
                    fontSize: '0.813rem', 
                    color: 'var(--accent-blue)',
                  }}>
                    💡 {issue.recommendation}
                  </p>
                </div>
              ))}
            </div>
          </CardBody>
        </Card>
      )}

      {/* Bounce Reasons */}
      <Card className="mb-6">
        <CardHeader title="Bounce Reasons" />
        <CardBody>
          <Table
            columns={bounceColumns}
            data={data?.bounce_reasons ?? []}
            keyExtractor={(_, idx) => `bounce-${idx}`}
            emptyMessage="No bounce data available"
          />
        </CardBody>
      </Card>

      {/* Delay Reasons */}
      <Card className="mb-6">
        <CardHeader title="Delay Reasons" />
        <CardBody>
          <Table
            columns={delayColumns}
            data={data?.delay_reasons ?? []}
            keyExtractor={(_, idx) => `delay-${idx}`}
            emptyMessage="No delay data available"
          />
        </CardBody>
      </Card>

      {/* Rejection Reasons */}
      <Card>
        <CardHeader title="Rejection Reasons" />
        <CardBody>
          <Table
            columns={rejectionColumns}
            data={data?.rejection_reasons ?? []}
            keyExtractor={(_, idx) => `rejection-${idx}`}
            emptyMessage="No rejection data available"
          />
        </CardBody>
      </Card>

      {/* Last updated */}
      <div style={{ 
        marginTop: '1.5rem', 
        textAlign: 'center', 
        color: 'var(--text-muted)',
        fontSize: '0.75rem',
      }}>
        Last updated: {data?.timestamp 
          ? new Date(data.timestamp).toLocaleString() 
          : 'Never'}
      </div>
    </div>
  );
};

export default SignalsView;
