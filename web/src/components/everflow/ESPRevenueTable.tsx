import { useMemo } from 'react';
import { SortableTable, SortableColumn } from '../common/SortableTable';
import { EverflowESPRevenue } from '../../types';

interface ESPRevenueTableProps {
  data: EverflowESPRevenue[];
  maxHeight?: string;
}

// Map ESP names to colors for visual distinction
const ESP_COLORS: Record<string, string> = {
  'SparkPost': '#ff6b35',
  'Mailgun': '#e74c3c',
  'Amazon SES': '#ff9900',
  'SES': '#ff9900',
  'SendGrid': '#1a82e2',
  'Postmark': '#ffde00',
  'Mailjet': '#ffa500',
  'Unknown': '#666666',
};

export const ESPRevenueTable: React.FC<ESPRevenueTableProps> = ({
  data,
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
    if (value >= 1000000) {
      return `${(value / 1000000).toFixed(1)}M`;
    }
    if (value >= 1000) {
      return `${(value / 1000).toFixed(1)}K`;
    }
    return new Intl.NumberFormat('en-US').format(value);
  };

  const formatPercent = (value: number): string => {
    return `${value.toFixed(1)}%`;
  };

  const getESPColor = (espName: string): string => {
    return ESP_COLORS[espName] || ESP_COLORS['Unknown'];
  };

  const columns: SortableColumn<EverflowESPRevenue>[] = useMemo(() => [
    {
      key: 'esp_name',
      header: 'ESP',
      width: '140px',
      render: (item) => (
        <div className="esp-name-cell">
          <span 
            className="esp-indicator" 
            style={{ backgroundColor: getESPColor(item.esp_name) }}
          />
          <span className="esp-name">{item.esp_name}</span>
        </div>
      ),
    },
    {
      key: 'campaign_count',
      header: 'Campaigns',
      align: 'right',
      width: '90px',
      render: (item) => formatNumber(item.campaign_count),
    },
    {
      key: 'total_delivered',
      header: 'Delivered',
      align: 'right',
      width: '100px',
      render: (item) => formatNumber(item.total_delivered),
    },
    {
      key: 'revenue',
      header: 'Revenue',
      align: 'right',
      width: '110px',
      render: (item) => (
        <span className="revenue-value">{formatCurrency(item.revenue)}</span>
      ),
    },
    {
      key: 'percentage',
      header: 'Share',
      align: 'right',
      width: '80px',
      render: (item) => (
        <div className="percentage-cell">
          <span className="percentage-value">{formatPercent(item.percentage)}</span>
          <div className="percentage-bar-container">
            <div 
              className="percentage-bar" 
              style={{ 
                width: `${Math.min(item.percentage, 100)}%`,
                backgroundColor: getESPColor(item.esp_name),
              }}
            />
          </div>
        </div>
      ),
    },
    {
      key: 'avg_ecpm',
      header: 'Avg eCPM',
      align: 'right',
      width: '90px',
      render: (item) => formatCurrency(item.avg_ecpm),
    },
    {
      key: 'conversions',
      header: 'Conv.',
      align: 'right',
      width: '70px',
      render: (item) => formatNumber(item.conversions),
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
    <div className="esp-revenue-table">
      <SortableTable
        columns={columns}
        data={data}
        keyExtractor={(item) => item.esp_name}
        defaultSortKey="revenue"
        defaultSortDirection="desc"
        stickyHeader
        maxHeight={maxHeight}
        emptyMessage="No ESP revenue data available"
      />
      <style>{`
        .esp-revenue-table .esp-name-cell {
          display: flex;
          align-items: center;
          gap: 0.5rem;
        }
        .esp-revenue-table .esp-indicator {
          width: 12px;
          height: 12px;
          border-radius: 2px;
          flex-shrink: 0;
        }
        .esp-revenue-table .esp-name {
          font-weight: 600;
        }
        .esp-revenue-table .revenue-value {
          font-weight: 600;
          color: var(--accent-green, #22c55e);
        }
        .esp-revenue-table .percentage-cell {
          display: flex;
          flex-direction: column;
          gap: 0.25rem;
        }
        .esp-revenue-table .percentage-value {
          font-weight: 500;
        }
        .esp-revenue-table .percentage-bar-container {
          width: 100%;
          height: 4px;
          background: var(--bg-secondary, #2a2a3e);
          border-radius: 2px;
          overflow: hidden;
        }
        .esp-revenue-table .percentage-bar {
          height: 100%;
          border-radius: 2px;
          transition: width 0.3s ease;
        }
      `}</style>
    </div>
  );
};

export default ESPRevenueTable;
