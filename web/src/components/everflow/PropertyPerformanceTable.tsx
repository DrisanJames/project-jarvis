import { useMemo } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faQuestionCircle } from '@fortawesome/free-solid-svg-icons';
import { SortableTable, SortableColumn } from '../common/SortableTable';
import { EverflowPropertyPerformance } from '../../types';

interface PropertyPerformanceTableProps {
  data: EverflowPropertyPerformance[];
  onPropertyClick?: (property: EverflowPropertyPerformance) => void;
  maxHeight?: string;
}

export const PropertyPerformanceTable: React.FC<PropertyPerformanceTableProps> = ({
  data,
  onPropertyClick,
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

  const columns: SortableColumn<EverflowPropertyPerformance>[] = useMemo(() => [
    {
      key: 'property_code',
      header: 'Code',
      width: '80px',
      render: (item) => (
        <span className={`property-code ${item.is_unattributed ? 'unattributed' : ''}`}>
          {item.property_code}
        </span>
      ),
    },
    {
      key: 'property_name',
      header: 'Property/Domain',
      render: (item) => (
        <div className="property-name-cell">
          <span 
            className={`property-name ${item.is_unattributed ? 'unattributed' : ''}`} 
            title={item.is_unattributed ? undefined : item.property_name}
          >
            {item.property_name.length > 25 
              ? `${item.property_name.substring(0, 25)}...` 
              : item.property_name}
          </span>
          {item.is_unattributed && item.unattrib_reason && (
            <span className="unattrib-tooltip" title={item.unattrib_reason}>
              <FontAwesomeIcon icon={faQuestionCircle} />
            </span>
          )}
        </div>
      ),
    },
    {
      key: 'unique_offers',
      header: 'Offers',
      align: 'right',
      width: '60px',
      render: (item) => formatNumber(item.unique_offers),
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
      width: '70px',
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
    <div className="property-performance-table">
      <SortableTable
        columns={columns}
        data={data}
        keyExtractor={(item) => item.property_code}
        onRowClick={onPropertyClick}
        defaultSortKey="revenue"
        defaultSortDirection="desc"
        stickyHeader
        maxHeight={maxHeight}
        emptyMessage="No property data available"
      />
      <style>{`
        .property-performance-table .property-code {
          font-family: monospace;
          font-weight: 600;
          color: var(--accent-blue, #3b82f6);
        }
        .property-performance-table .property-code.unattributed {
          color: var(--text-muted, #666);
          font-style: italic;
        }
        .property-performance-table .property-name-cell {
          display: flex;
          align-items: center;
          gap: 0.5rem;
        }
        .property-performance-table .property-name {
          font-weight: 500;
        }
        .property-performance-table .property-name.unattributed {
          color: var(--text-muted, #666);
          font-style: italic;
        }
        .property-performance-table .unattrib-tooltip {
          display: inline-flex;
          align-items: center;
          justify-content: center;
          color: var(--accent-yellow, #f59e0b);
          cursor: help;
        }
        .property-performance-table .unattrib-tooltip:hover {
          color: var(--accent-orange, #ea580c);
        }
        .property-performance-table .revenue-value {
          font-weight: 600;
          color: var(--accent-green, #22c55e);
        }
        .property-performance-table .conv-rate-high {
          color: var(--accent-green, #22c55e);
        }
        .property-performance-table .conv-rate-medium {
          color: var(--accent-yellow, #f59e0b);
        }
        .property-performance-table .conv-rate-low {
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

export default PropertyPerformanceTable;
