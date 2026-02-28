import { useMemo } from 'react';
import { SortableTable, SortableColumn } from '../common/SortableTable';
import { EverflowOfferPerformance } from '../../types';

interface OfferPerformanceTableProps {
  data: EverflowOfferPerformance[];
  onOfferClick?: (offer: EverflowOfferPerformance) => void;
  maxHeight?: string;
}

export const OfferPerformanceTable: React.FC<OfferPerformanceTableProps> = ({
  data,
  onOfferClick,
  maxHeight = '400px',
}) => {
  const formatCurrency = (value: number): string => {
    return new Intl.NumberFormat('en-US', {
      style: 'currency',
      currency: 'USD',
      minimumFractionDigits: 2,
      maximumFractionDigits: 2,
    }).format(value);
  };

  const formatNumber = (value: number): string => {
    return new Intl.NumberFormat('en-US').format(value);
  };

  const formatPercent = (value: number): string => {
    return `${(value * 100).toFixed(2)}%`;
  };

  const columns: SortableColumn<EverflowOfferPerformance>[] = useMemo(() => [
    {
      key: 'offer_id',
      header: 'Offer ID',
      width: '80px',
      render: (item) => (
        <span className="offer-id">{item.offer_id}</span>
      ),
    },
    {
      key: 'offer_name',
      header: 'Offer Name',
      render: (item) => (
        <span className="offer-name">{item.offer_name}</span>
      ),
    },
    {
      key: 'clicks',
      header: 'Clicks',
      align: 'right',
      width: '80px',
      render: (item) => formatNumber(item.clicks),
    },
    {
      key: 'conversions',
      header: 'Conv.',
      align: 'right',
      width: '80px',
      render: (item) => formatNumber(item.conversions),
    },
    {
      key: 'revenue',
      header: 'Revenue',
      align: 'right',
      width: '100px',
      render: (item) => (
        <span className="revenue-value">{formatCurrency(item.revenue)}</span>
      ),
    },
    {
      key: 'payout',
      header: 'Payout',
      align: 'right',
      width: '100px',
      render: (item) => formatCurrency(item.payout),
    },
    {
      key: 'conversion_rate',
      header: 'Conv. Rate',
      align: 'right',
      width: '90px',
      render: (item) => (
        <span className={getConversionRateClass(item.conversion_rate)}>
          {formatPercent(item.conversion_rate)}
        </span>
      ),
    },
    {
      key: 'epc',
      header: 'EPC',
      align: 'right',
      width: '80px',
      render: (item) => formatCurrency(item.epc),
    },
  ], []);

  return (
    <div className="offer-performance-table">
      <SortableTable
        columns={columns}
        data={data}
        keyExtractor={(item) => item.offer_id}
        onRowClick={onOfferClick}
        defaultSortKey="revenue"
        defaultSortDirection="desc"
        stickyHeader
        maxHeight={maxHeight}
        emptyMessage="No offer data available"
      />
      <style>{`
        .offer-performance-table .offer-id {
          font-family: monospace;
          color: var(--text-muted, #666);
        }
        .offer-performance-table .offer-name {
          font-weight: 500;
        }
        .offer-performance-table .revenue-value {
          font-weight: 600;
          color: var(--accent-green, #22c55e);
        }
        .offer-performance-table .conv-rate-high {
          color: var(--accent-green, #22c55e);
        }
        .offer-performance-table .conv-rate-medium {
          color: var(--accent-yellow, #f59e0b);
        }
        .offer-performance-table .conv-rate-low {
          color: var(--accent-red, #ef4444);
        }
      `}</style>
    </div>
  );
};

function getConversionRateClass(rate: number): string {
  if (rate >= 0.05) return 'conv-rate-high';
  if (rate >= 0.02) return 'conv-rate-medium';
  return 'conv-rate-low';
}

export default OfferPerformanceTable;
