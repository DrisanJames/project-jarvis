import { useMemo } from 'react';
import { SortableTable, SortableColumn } from '../common/SortableTable';
import { EverflowCampaignRevenue } from '../../types';

interface CampaignRevenueTableProps {
  data: EverflowCampaignRevenue[];
  onCampaignClick?: (campaign: EverflowCampaignRevenue) => void;
  maxHeight?: string;
}

export const CampaignRevenueTable: React.FC<CampaignRevenueTableProps> = ({
  data,
  onCampaignClick,
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

  const columns: SortableColumn<EverflowCampaignRevenue>[] = useMemo(() => [
    {
      key: 'mailing_id',
      header: 'Mailing ID',
      width: '100px',
      render: (item) => (
        <span className="mailing-id">{item.mailing_id}</span>
      ),
    },
    {
      key: 'property_code',
      header: 'Property',
      width: '70px',
      render: (item) => (
        <span className="property-code" title={item.property_name}>
          {item.property_code}
        </span>
      ),
    },
    {
      key: 'campaign_name',
      header: 'Campaign Name',
      render: (item) => (
        <span className="campaign-name" title={item.campaign_name || item.offer_name}>
          {item.campaign_name || item.offer_name}
        </span>
      ),
    },
    {
      key: 'clicks',
      header: 'Clicks',
      align: 'right',
      width: '70px',
      render: (item) => formatNumber(item.clicks),
    },
    {
      key: 'conversions',
      header: 'Conv.',
      align: 'right',
      width: '60px',
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
      key: 'conversion_rate',
      header: 'Conv %',
      align: 'right',
      width: '70px',
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
    {
      key: 'audience_size',
      header: 'Audience',
      align: 'right',
      width: '90px',
      render: (item) => (
        <span className={item.ongage_linked ? 'audience-value' : 'audience-unknown'}>
          {item.ongage_linked ? formatNumber(item.audience_size) : '-'}
        </span>
      ),
    },
    {
      key: 'ecpm',
      header: 'eCPM',
      align: 'right',
      width: '80px',
      render: (item) => (
        <span className="ecpm-value">
          {item.ecpm > 0 ? formatCurrency(item.ecpm) : '-'}
        </span>
      ),
    },
  ], []);

  return (
    <div className="campaign-revenue-table">
      <SortableTable
        columns={columns}
        data={data}
        keyExtractor={(item) => item.mailing_id}
        onRowClick={onCampaignClick}
        defaultSortKey="revenue"
        defaultSortDirection="desc"
        stickyHeader
        maxHeight={maxHeight}
        emptyMessage="No campaign revenue data available"
      />
      <style>{`
        .campaign-revenue-table .mailing-id {
          font-family: monospace;
          font-size: 0.85em;
          color: var(--text-muted, #666);
        }
        .campaign-revenue-table .property-code {
          font-family: monospace;
          font-weight: 600;
          color: var(--accent-blue, #3b82f6);
        }
        .campaign-revenue-table .campaign-name {
          font-weight: 500;
        }
        .campaign-revenue-table .revenue-value {
          font-weight: 600;
          color: var(--accent-green, #22c55e);
        }
        .campaign-revenue-table .conv-rate-high {
          color: var(--accent-green, #22c55e);
        }
        .campaign-revenue-table .conv-rate-medium {
          color: var(--accent-yellow, #f59e0b);
        }
        .campaign-revenue-table .conv-rate-low {
          color: var(--accent-red, #ef4444);
        }
        .campaign-revenue-table .audience-value {
          color: var(--text-primary, #fff);
        }
        .campaign-revenue-table .audience-unknown {
          color: var(--text-muted, #666);
        }
        .campaign-revenue-table .ecpm-value {
          color: var(--accent-blue, #3b82f6);
          font-weight: 500;
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

export default CampaignRevenueTable;
