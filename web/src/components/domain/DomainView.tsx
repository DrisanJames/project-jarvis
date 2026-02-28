import React from 'react';
import { Card, CardHeader, CardBody, Table, StatusBadge, Loading, ErrorDisplay } from '../common';
import { useApi } from '../../hooks/useApi';
import type { DomainMetrics } from '../../types';

const POLLING_INTERVAL = 60000;

export const DomainView: React.FC = () => {
  const { data, loading, error, refetch } = useApi<{ timestamp: string; data: DomainMetrics[] }>(
    '/api/metrics/domain',
    { pollingInterval: POLLING_INTERVAL }
  );

  if (loading && !data) {
    return <Loading message="Loading domain metrics..." />;
  }

  if (error) {
    return <ErrorDisplay message={error} onRetry={refetch} />;
  }

  const domainMetrics = data?.data ?? [];

  const formatNumber = (n: number): string => {
    if (n >= 1000000) return `${(n / 1000000).toFixed(1)}M`;
    if (n >= 1000) return `${(n / 1000).toFixed(1)}K`;
    return n.toLocaleString();
  };

  const formatPercentage = (n: number): string => {
    return `${(n * 100).toFixed(2)}%`;
  };

  const columns = [
    {
      key: 'domain',
      header: 'Sending Domain',
      render: (item: DomainMetrics) => (
        <strong>{item.domain}</strong>
      ),
    },
    {
      key: 'metrics.targeted',
      header: 'Volume',
      align: 'right' as const,
      render: (item: DomainMetrics) => (
        <span className="font-mono">{formatNumber(item.metrics.targeted)}</span>
      ),
    },
    {
      key: 'metrics.delivery_rate',
      header: 'Delivered',
      align: 'right' as const,
      render: (item: DomainMetrics) => (
        <span className="font-mono">{formatPercentage(item.metrics.delivery_rate)}</span>
      ),
    },
    {
      key: 'metrics.open_rate',
      header: 'Open Rate',
      align: 'right' as const,
      render: (item: DomainMetrics) => (
        <span className="font-mono">{formatPercentage(item.metrics.open_rate)}</span>
      ),
    },
    {
      key: 'metrics.complaint_rate',
      header: 'Complaints',
      align: 'right' as const,
      render: (item: DomainMetrics) => (
        <span 
          className="font-mono"
          style={{
            color: item.metrics.complaint_rate > 0.0005
              ? 'var(--accent-red)'
              : item.metrics.complaint_rate > 0.0003
              ? 'var(--accent-yellow)'
              : undefined,
          }}
        >
          {formatPercentage(item.metrics.complaint_rate)}
        </span>
      ),
    },
    {
      key: 'metrics.bounce_rate',
      header: 'Bounces',
      align: 'right' as const,
      render: (item: DomainMetrics) => (
        <span 
          className="font-mono"
          style={{
            color: item.metrics.bounce_rate > 0.05
              ? 'var(--accent-red)'
              : item.metrics.bounce_rate > 0.03
              ? 'var(--accent-yellow)'
              : undefined,
          }}
        >
          {formatPercentage(item.metrics.bounce_rate)}
        </span>
      ),
    },
    {
      key: 'status',
      header: 'Status',
      align: 'center' as const,
      render: (item: DomainMetrics) => (
        <StatusBadge status={item.status} />
      ),
    },
  ];

  return (
    <div>
      {/* Domain Performance */}
      <Card>
        <CardHeader title="Sending Domain Performance" />
        <CardBody>
          <Table
            columns={columns}
            data={domainMetrics}
            keyExtractor={(item) => item.domain}
            emptyMessage="No domain data available"
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

export default DomainView;
