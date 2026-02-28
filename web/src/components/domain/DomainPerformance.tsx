import { useState, useMemo } from 'react';
import { UnifiedDomainResponse } from '../../types';
import { useApi } from '../../hooks/useApi';
import { useDateFilter } from '../../context/DateFilterContext';
import { Card, CardBody } from '../common/Card';
import { Loading } from '../common/Loading';
import { StatusBadge } from '../common/StatusBadge';

type SortField = 'domain' | 'provider' | 'volume' | 'delivered' | 'delivery_rate' | 'open_rate' | 'click_rate' | 'complaints' | 'bounces' | 'status';
type SortDirection = 'asc' | 'desc';

interface SortConfig {
  field: SortField;
  direction: SortDirection;
}

export function DomainPerformance() {
  // Use global date filter
  const { dateRange } = useDateFilter();
  
  // Build API URL with date range params
  const apiUrl = `/api/domain/unified?start_date=${dateRange.startDate}&end_date=${dateRange.endDate}&range_type=${dateRange.type}`;
  
  const { data, loading, error, refetch } = useApi<UnifiedDomainResponse>(apiUrl);
  
  const [sortConfig, setSortConfig] = useState<SortConfig>({ field: 'volume', direction: 'desc' });
  const [domainFilter, setDomainFilter] = useState<string>('');
  const [providerFilter, setProviderFilter] = useState<string>('all');

  // Get unique providers for filter dropdown
  const uniqueProviders = useMemo(() => {
    if (!data?.metrics) return [];
    return [...new Set(data.metrics.map(m => m.provider))].sort();
  }, [data]);

  // Filter and sort data
  const filteredAndSortedData = useMemo(() => {
    if (!data?.metrics) return [];

    let filtered = [...data.metrics];

    // Apply domain filter (search)
    if (domainFilter) {
      const searchLower = domainFilter.toLowerCase();
      filtered = filtered.filter(m => 
        m.domain.toLowerCase().includes(searchLower)
      );
    }

    // Apply provider filter
    if (providerFilter !== 'all') {
      filtered = filtered.filter(m => m.provider === providerFilter);
    }

    // Sort
    filtered.sort((a, b) => {
      let aVal: string | number;
      let bVal: string | number;

      switch (sortConfig.field) {
        case 'domain':
          aVal = a.domain.toLowerCase();
          bVal = b.domain.toLowerCase();
          break;
        case 'provider':
          aVal = a.provider;
          bVal = b.provider;
          break;
        case 'volume':
          aVal = a.volume;
          bVal = b.volume;
          break;
        case 'delivered':
          aVal = a.delivered;
          bVal = b.delivered;
          break;
        case 'delivery_rate':
          aVal = a.delivery_rate;
          bVal = b.delivery_rate;
          break;
        case 'open_rate':
          aVal = a.open_rate;
          bVal = b.open_rate;
          break;
        case 'click_rate':
          aVal = a.click_rate;
          bVal = b.click_rate;
          break;
        case 'complaints':
          aVal = a.complaints;
          bVal = b.complaints;
          break;
        case 'bounces':
          aVal = a.bounces;
          bVal = b.bounces;
          break;
        case 'status':
          // Sort by severity: critical > warning > healthy
          const statusOrder = { critical: 3, warning: 2, healthy: 1 };
          aVal = statusOrder[a.status] || 0;
          bVal = statusOrder[b.status] || 0;
          break;
        default:
          return 0;
      }

      if (aVal < bVal) return sortConfig.direction === 'asc' ? -1 : 1;
      if (aVal > bVal) return sortConfig.direction === 'asc' ? 1 : -1;
      return 0;
    });

    return filtered;
  }, [data, sortConfig, domainFilter, providerFilter]);

  // Handle column header click
  const handleSort = (field: SortField) => {
    setSortConfig(prev => ({
      field,
      direction: prev.field === field && prev.direction === 'desc' ? 'asc' : 'desc',
    }));
  };

  // Get sort indicator
  const getSortIndicator = (field: SortField) => {
    if (sortConfig.field !== field) return ' ↕';
    return sortConfig.direction === 'asc' ? ' ↑' : ' ↓';
  };

  // Format numbers
  const formatNumber = (num: number): string => {
    return num.toLocaleString();
  };

  const formatPercent = (num: number): string => {
    return `${(num * 100).toFixed(2)}%`;
  };

  // Provider badge color
  const getProviderColor = (provider: string): string => {
    switch (provider) {
      case 'sparkpost':
        return '#fa6423';
      case 'mailgun':
        return '#c53030';
      case 'ses':
        return '#ff9900';
      default:
        return '#718096';
    }
  };

  if (loading) {
    return <Loading message="Loading domain performance data..." />;
  }

  if (error) {
    return (
      <Card>
        <CardBody>
          <div className="error-state">
            <p>Error loading domain data: {error}</p>
            <button onClick={refetch} className="retry-button">
              Retry
            </button>
          </div>
        </CardBody>
      </Card>
    );
  }

  // Calculate totals
  const totals = filteredAndSortedData.reduce(
    (acc, m) => ({
      volume: acc.volume + m.volume,
      delivered: acc.delivered + m.delivered,
      opens: acc.opens + m.opens,
      clicks: acc.clicks + m.clicks,
      bounces: acc.bounces + m.bounces,
      complaints: acc.complaints + m.complaints,
    }),
    { volume: 0, delivered: 0, opens: 0, clicks: 0, bounces: 0, complaints: 0 }
  );

  // Count by status
  const statusCounts = filteredAndSortedData.reduce(
    (acc, m) => {
      acc[m.status] = (acc[m.status] || 0) + 1;
      return acc;
    },
    {} as Record<string, number>
  );

  // Count by provider
  const providerCounts = filteredAndSortedData.reduce(
    (acc, m) => {
      acc[m.provider] = (acc[m.provider] || 0) + 1;
      return acc;
    },
    {} as Record<string, number>
  );

  return (
    <div className="domain-performance">
      <Card>
        <div className="card-header" style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: '1rem', padding: '1rem 1.5rem', borderBottom: '1px solid var(--border-color)' }}>
          <h2 style={{ margin: 0 }}>Sending Domains - All Providers</h2>
          <div style={{ display: 'flex', gap: '1rem', alignItems: 'center', flexWrap: 'wrap' }}>
            <select
              value={providerFilter}
              onChange={(e) => setProviderFilter(e.target.value)}
              style={{
                padding: '0.5rem',
                borderRadius: '4px',
                border: '1px solid var(--border-color)',
                backgroundColor: 'var(--card-bg)',
                color: 'var(--text-color)',
              }}
            >
              <option value="all">All Providers</option>
              {uniqueProviders.map(p => (
                <option key={p} value={p}>{p.charAt(0).toUpperCase() + p.slice(1)}</option>
              ))}
            </select>
            <input
              type="text"
              placeholder="Search domains..."
              value={domainFilter}
              onChange={(e) => setDomainFilter(e.target.value)}
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
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(130px, 1fr))', gap: '1rem', marginBottom: '1.5rem' }}>
            <div className="summary-stat">
              <div className="stat-label">Total Domains</div>
              <div className="stat-value">{filteredAndSortedData.length}</div>
            </div>
            <div className="summary-stat">
              <div className="stat-label">Total Volume</div>
              <div className="stat-value">{formatNumber(totals.volume)}</div>
            </div>
            <div className="summary-stat">
              <div className="stat-label">Delivery Rate</div>
              <div className="stat-value">{totals.volume > 0 ? formatPercent(totals.delivered / totals.volume) : '-'}</div>
            </div>
            <div className="summary-stat" style={{ borderLeft: '3px solid #48bb78' }}>
              <div className="stat-label">Healthy</div>
              <div className="stat-value" style={{ color: '#48bb78' }}>{statusCounts.healthy || 0}</div>
            </div>
            <div className="summary-stat" style={{ borderLeft: '3px solid #ecc94b' }}>
              <div className="stat-label">Warning</div>
              <div className="stat-value" style={{ color: '#ecc94b' }}>{statusCounts.warning || 0}</div>
            </div>
            <div className="summary-stat" style={{ borderLeft: '3px solid #f56565' }}>
              <div className="stat-label">Critical</div>
              <div className="stat-value" style={{ color: '#f56565' }}>{statusCounts.critical || 0}</div>
            </div>
          </div>

          {/* Provider Breakdown */}
          {Object.keys(providerCounts).length > 1 && (
            <div style={{ marginBottom: '1rem', display: 'flex', gap: '1rem', flexWrap: 'wrap' }}>
              {Object.entries(providerCounts).map(([provider, count]) => (
                <span
                  key={provider}
                  style={{
                    padding: '0.25rem 0.75rem',
                    borderRadius: '16px',
                    backgroundColor: getProviderColor(provider),
                    color: 'white',
                    fontSize: '0.875rem',
                  }}
                >
                  {provider.toUpperCase()}: {count} domains
                </span>
              ))}
            </div>
          )}

          {/* Data Table */}
          <div className="table-container" style={{ overflowX: 'auto' }}>
            <table className="table" style={{ minWidth: '1000px' }}>
              <thead>
                <tr>
                  <th
                    onClick={() => handleSort('provider')}
                    style={{ cursor: 'pointer', userSelect: 'none' }}
                  >
                    Provider{getSortIndicator('provider')}
                  </th>
                  <th
                    onClick={() => handleSort('domain')}
                    style={{ cursor: 'pointer', userSelect: 'none' }}
                  >
                    Domain{getSortIndicator('domain')}
                  </th>
                  <th
                    onClick={() => handleSort('volume')}
                    style={{ cursor: 'pointer', userSelect: 'none', textAlign: 'right' }}
                  >
                    Volume{getSortIndicator('volume')}
                  </th>
                  <th
                    onClick={() => handleSort('delivered')}
                    style={{ cursor: 'pointer', userSelect: 'none', textAlign: 'right' }}
                  >
                    Delivered{getSortIndicator('delivered')}
                  </th>
                  <th
                    onClick={() => handleSort('delivery_rate')}
                    style={{ cursor: 'pointer', userSelect: 'none', textAlign: 'right' }}
                  >
                    Delivery %{getSortIndicator('delivery_rate')}
                  </th>
                  <th
                    onClick={() => handleSort('open_rate')}
                    style={{ cursor: 'pointer', userSelect: 'none', textAlign: 'right' }}
                  >
                    Open Rate{getSortIndicator('open_rate')}
                  </th>
                  <th
                    onClick={() => handleSort('click_rate')}
                    style={{ cursor: 'pointer', userSelect: 'none', textAlign: 'right' }}
                  >
                    CTR{getSortIndicator('click_rate')}
                  </th>
                  <th
                    onClick={() => handleSort('bounces')}
                    style={{ cursor: 'pointer', userSelect: 'none', textAlign: 'right' }}
                  >
                    Bounces{getSortIndicator('bounces')}
                  </th>
                  <th
                    onClick={() => handleSort('complaints')}
                    style={{ cursor: 'pointer', userSelect: 'none', textAlign: 'right' }}
                  >
                    Complaints{getSortIndicator('complaints')}
                  </th>
                  <th
                    onClick={() => handleSort('status')}
                    style={{ cursor: 'pointer', userSelect: 'none', textAlign: 'center' }}
                  >
                    Status{getSortIndicator('status')}
                  </th>
                </tr>
              </thead>
              <tbody>
                {filteredAndSortedData.length === 0 ? (
                  <tr>
                    <td colSpan={10} style={{ textAlign: 'center', padding: '2rem' }}>
                      {data?.providers?.length === 0 
                        ? 'No domain data available.'
                        : 'No domains match your filters'}
                    </td>
                  </tr>
                ) : (
                  filteredAndSortedData.map((metric, index) => (
                    <tr key={`${metric.provider}-${metric.domain}-${index}`}>
                      <td>
                        <span
                          style={{
                            padding: '0.25rem 0.5rem',
                            borderRadius: '4px',
                            backgroundColor: getProviderColor(metric.provider),
                            color: 'white',
                            fontSize: '0.75rem',
                            fontWeight: 'bold',
                            textTransform: 'uppercase',
                          }}
                        >
                          {metric.provider}
                        </span>
                      </td>
                      <td style={{ fontWeight: 500 }}>{metric.domain}</td>
                      <td style={{ textAlign: 'right' }}>{formatNumber(metric.volume)}</td>
                      <td style={{ textAlign: 'right' }}>{formatNumber(metric.delivered)}</td>
                      <td style={{ textAlign: 'right' }}>{formatPercent(metric.delivery_rate)}</td>
                      <td style={{ textAlign: 'right' }}>{formatPercent(metric.open_rate)}</td>
                      <td style={{ textAlign: 'right' }}>{formatPercent(metric.click_rate)}</td>
                      <td style={{ textAlign: 'right' }}>{formatNumber(metric.bounces)}</td>
                      <td style={{ textAlign: 'right' }}>{formatNumber(metric.complaints)}</td>
                      <td style={{ textAlign: 'center' }}>
                        <StatusBadge status={metric.status} />
                      </td>
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>

          {/* Legend */}
          <div style={{ marginTop: '1rem', display: 'flex', gap: '1rem', fontSize: '0.875rem', color: 'var(--text-muted)', flexWrap: 'wrap' }}>
            <span>Click column headers to sort</span>
            <span>•</span>
            <span>Active providers: {data?.providers?.length ? data.providers.join(', ') : 'None'}</span>
          </div>
        </CardBody>
      </Card>

      <style>{`
        .summary-stat {
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
          font-size: 1.5rem;
          font-weight: 600;
          color: var(--text-color);
        }
        .table th {
          background: var(--table-header-bg, #f7fafc);
          position: sticky;
          top: 0;
          z-index: 1;
        }
        .table th:hover {
          background: var(--table-header-hover, #edf2f7);
        }
        .table tr:hover {
          background: var(--table-row-hover, #f7fafc);
        }
        .error-state {
          text-align: center;
          padding: 2rem;
        }
        .retry-button {
          margin-top: 1rem;
          padding: 0.5rem 1rem;
          background: var(--primary-color);
          color: white;
          border: none;
          border-radius: 4px;
          cursor: pointer;
        }
        .retry-button:hover {
          opacity: 0.9;
        }
      `}</style>
    </div>
  );
}

export default DomainPerformance;
