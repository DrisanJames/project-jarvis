import React from 'react';
import { Table, StatusBadge } from '../common';
import type { ISPMetrics } from '../../types';

interface ISPTableProps {
  data: ISPMetrics[];
  onRowClick?: (isp: ISPMetrics) => void;
}

export const ISPTable: React.FC<ISPTableProps> = ({ data, onRowClick }) => {
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
      key: 'provider',
      header: 'ISP',
      render: (item: ISPMetrics) => (
        <strong>{item.provider}</strong>
      ),
    },
    {
      key: 'metrics.targeted',
      header: 'Volume',
      align: 'right' as const,
      render: (item: ISPMetrics) => (
        <span className="font-mono">{formatNumber(item.metrics.targeted)}</span>
      ),
    },
    {
      key: 'metrics.delivery_rate',
      header: 'Delivered',
      align: 'right' as const,
      render: (item: ISPMetrics) => (
        <span className="font-mono">{formatPercentage(item.metrics.delivery_rate)}</span>
      ),
    },
    {
      key: 'metrics.open_rate',
      header: 'Open Rate',
      align: 'right' as const,
      render: (item: ISPMetrics) => (
        <span className="font-mono">{formatPercentage(item.metrics.open_rate)}</span>
      ),
    },
    {
      key: 'metrics.click_rate',
      header: 'CTR',
      align: 'right' as const,
      render: (item: ISPMetrics) => (
        <span className="font-mono">{formatPercentage(item.metrics.click_rate)}</span>
      ),
    },
    {
      key: 'metrics.complaint_rate',
      header: 'Complaints',
      align: 'right' as const,
      render: (item: ISPMetrics) => (
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
      render: (item: ISPMetrics) => (
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
      render: (item: ISPMetrics) => (
        <StatusBadge status={item.status} />
      ),
    },
  ];

  return (
    <Table
      columns={columns}
      data={data}
      keyExtractor={(item) => item.provider}
      onRowClick={onRowClick}
      emptyMessage="No ISP data available"
    />
  );
};

export default ISPTable;
