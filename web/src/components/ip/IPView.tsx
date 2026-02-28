import React from 'react';
import { Card, CardHeader, CardBody, Table, StatusBadge, Loading, ErrorDisplay } from '../common';
import { useApi } from '../../hooks/useApi';
import type { IPMetrics } from '../../types';

const POLLING_INTERVAL = 60000;

export const IPView: React.FC = () => {
  const { data, loading, error, refetch } = useApi<{ timestamp: string; data: IPMetrics[] }>(
    '/api/metrics/ip',
    { pollingInterval: POLLING_INTERVAL }
  );

  if (loading && !data) {
    return <Loading message="Loading IP metrics..." />;
  }

  if (error) {
    return <ErrorDisplay message={error} onRetry={refetch} />;
  }

  const ipMetrics = data?.data ?? [];

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
      key: 'ip',
      header: 'IP Address',
      render: (item: IPMetrics) => (
        <span className="font-mono">{item.ip}</span>
      ),
    },
    {
      key: 'pool',
      header: 'Pool',
      render: (item: IPMetrics) => (
        <span style={{ 
          padding: '0.125rem 0.5rem',
          backgroundColor: 'var(--bg-tertiary)',
          borderRadius: '0.25rem',
          fontSize: '0.75rem',
        }}>
          {item.pool}
        </span>
      ),
    },
    {
      key: 'metrics.targeted',
      header: 'Volume',
      align: 'right' as const,
      render: (item: IPMetrics) => (
        <span className="font-mono">{formatNumber(item.metrics.targeted)}</span>
      ),
    },
    {
      key: 'metrics.delivery_rate',
      header: 'Delivered',
      align: 'right' as const,
      render: (item: IPMetrics) => (
        <span className="font-mono">{formatPercentage(item.metrics.delivery_rate)}</span>
      ),
    },
    {
      key: 'metrics.bounce_rate',
      header: 'Bounce Rate',
      align: 'right' as const,
      render: (item: IPMetrics) => (
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
      key: 'metrics.block_rate',
      header: 'Block Rate',
      align: 'right' as const,
      render: (item: IPMetrics) => (
        <span 
          className="font-mono"
          style={{
            color: item.metrics.block_rate > 0.005
              ? 'var(--accent-red)'
              : item.metrics.block_rate > 0.003
              ? 'var(--accent-yellow)'
              : undefined,
          }}
        >
          {formatPercentage(item.metrics.block_rate)}
        </span>
      ),
    },
    {
      key: 'status',
      header: 'Status',
      align: 'center' as const,
      render: (item: IPMetrics) => (
        <StatusBadge status={item.status} />
      ),
    },
  ];

  // Group by pool
  const pools = [...new Set(ipMetrics.map((ip) => ip.pool))];

  return (
    <div>
      {/* Pool Summary */}
      <div className="grid grid-4 mb-6">
        {pools.map((pool) => {
          const poolIPs = ipMetrics.filter((ip) => ip.pool === pool);
          const totalVolume = poolIPs.reduce((sum, ip) => sum + ip.metrics.targeted, 0);
          const avgDelivery = poolIPs.reduce((sum, ip) => sum + ip.metrics.delivery_rate, 0) / poolIPs.length;
          
          return (
            <Card key={pool}>
              <CardBody>
                <div style={{ textAlign: 'center' }}>
                  <div style={{ 
                    fontSize: '0.75rem', 
                    color: 'var(--text-muted)',
                    marginBottom: '0.5rem',
                  }}>
                    {pool}
                  </div>
                  <div style={{ 
                    fontSize: '1.5rem', 
                    fontWeight: 700,
                  }}>
                    {poolIPs.length} IPs
                  </div>
                  <div style={{ 
                    fontSize: '0.875rem', 
                    color: 'var(--text-secondary)',
                  }}>
                    {formatNumber(totalVolume)} sent
                  </div>
                  <div style={{ 
                    fontSize: '0.75rem', 
                    color: 'var(--accent-green)',
                  }}>
                    {formatPercentage(avgDelivery)} delivered
                  </div>
                </div>
              </CardBody>
            </Card>
          );
        })}
      </div>

      {/* All IPs */}
      <Card>
        <CardHeader title="Sending IP Performance" />
        <CardBody>
          <Table
            columns={columns}
            data={ipMetrics}
            keyExtractor={(item) => item.ip}
            emptyMessage="No IP data available"
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

export default IPView;
