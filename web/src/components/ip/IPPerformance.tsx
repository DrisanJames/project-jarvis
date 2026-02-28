import { useState, useMemo } from 'react';
import { UnifiedIPResponse } from '../../types';
import { useApi } from '../../hooks/useApi';
import { useDateFilter } from '../../context/DateFilterContext';
import { Card, CardBody } from '../common/Card';
import { Loading } from '../common/Loading';
import { StatusBadge } from '../common/StatusBadge';

type SortField = 'ip' | 'pool' | 'pool_type' | 'provider' | 'volume' | 'delivered' | 'delivery_rate' | 'open_rate' | 'click_rate' | 'complaints' | 'bounces' | 'status';
type SortDirection = 'asc' | 'desc';

interface SortConfig {
  field: SortField;
  direction: SortDirection;
}

// Pool Type Badge Component
function PoolTypeBadge({ type }: { type: string }) {
  const getPoolTypeStyles = () => {
    switch (type) {
      case 'dedicated':
        return {
          backgroundColor: 'rgba(72, 187, 120, 0.15)',
          color: '#48bb78',
          border: '1px solid rgba(72, 187, 120, 0.3)',
        };
      case 'shared':
        return {
          backgroundColor: 'rgba(236, 201, 75, 0.15)',
          color: '#d69e2e',
          border: '1px solid rgba(236, 201, 75, 0.3)',
        };
      default:
        return {
          backgroundColor: 'rgba(160, 174, 192, 0.15)',
          color: '#a0aec0',
          border: '1px solid rgba(160, 174, 192, 0.3)',
        };
    }
  };

  const styles = getPoolTypeStyles();
  const label = type === 'unknown' ? '-' : type.charAt(0).toUpperCase() + type.slice(1);

  return (
    <span
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        gap: '0.25rem',
        padding: '0.2rem 0.5rem',
        borderRadius: '4px',
        fontSize: '0.7rem',
        fontWeight: 600,
        textTransform: 'uppercase',
        letterSpacing: '0.025em',
        ...styles,
      }}
      title={type === 'shared' ? 'Shared IP pool - metrics may be influenced by other senders' : 
             type === 'dedicated' ? 'Dedicated IP - you have full control over reputation' :
             'Pool type not configured'}
    >
      {type === 'shared' && (
        <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
          <path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2" />
          <circle cx="9" cy="7" r="4" />
          <path d="M23 21v-2a4 4 0 0 0-3-3.87" />
          <path d="M16 3.13a4 4 0 0 1 0 7.75" />
        </svg>
      )}
      {type === 'dedicated' && (
        <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
          <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" />
        </svg>
      )}
      {label}
    </span>
  );
}

export function IPPerformance() {
  // Use global date filter
  const { dateRange } = useDateFilter();
  
  // Build API URL with date range params
  const apiUrl = `/api/ip/unified?start_date=${dateRange.startDate}&end_date=${dateRange.endDate}&range_type=${dateRange.type}`;
  
  const { data, loading, error, refetch } = useApi<UnifiedIPResponse>(apiUrl);
  
  const [sortConfig, setSortConfig] = useState<SortConfig>({ field: 'volume', direction: 'desc' });
  const [ipFilter, setIpFilter] = useState<string>('');
  const [poolFilter, setPoolFilter] = useState<string>('all');
  const [poolTypeFilter, setPoolTypeFilter] = useState<string>('all');
  const [providerFilter, setProviderFilter] = useState<string>('all');

  // Get unique pools, pool types, and providers for filter dropdowns
  const { uniquePools, uniquePoolTypes, uniqueProviders } = useMemo(() => {
    if (!data?.metrics) return { uniquePools: [], uniquePoolTypes: [], uniqueProviders: [] };
    
    const pools = [...new Set(data.metrics.map(m => m.pool).filter(p => p))].sort();
    const poolTypes = [...new Set(data.metrics.map(m => m.pool_type).filter(p => p))].sort();
    const providers = [...new Set(data.metrics.map(m => m.provider))].sort();
    
    return { uniquePools: pools, uniquePoolTypes: poolTypes, uniqueProviders: providers };
  }, [data]);

  // Filter and sort data
  const filteredAndSortedData = useMemo(() => {
    if (!data?.metrics) return [];

    let filtered = [...data.metrics];

    // Apply IP filter (search)
    if (ipFilter) {
      const searchLower = ipFilter.toLowerCase();
      filtered = filtered.filter(m => 
        m.ip.toLowerCase().includes(searchLower) ||
        m.pool.toLowerCase().includes(searchLower)
      );
    }

    // Apply pool filter
    if (poolFilter !== 'all') {
      filtered = filtered.filter(m => m.pool === poolFilter);
    }

    // Apply pool type filter
    if (poolTypeFilter !== 'all') {
      filtered = filtered.filter(m => m.pool_type === poolTypeFilter);
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
        case 'ip':
          // Sort IPs naturally (by octets)
          aVal = a.ip.split('.').map(n => n.padStart(3, '0')).join('.');
          bVal = b.ip.split('.').map(n => n.padStart(3, '0')).join('.');
          break;
        case 'pool':
          aVal = a.pool.toLowerCase();
          bVal = b.pool.toLowerCase();
          break;
        case 'pool_type':
          // Sort by type: shared first (they need more attention)
          const typeOrder = { shared: 1, unknown: 2, dedicated: 3 };
          aVal = typeOrder[a.pool_type] || 2;
          bVal = typeOrder[b.pool_type] || 2;
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
  }, [data, sortConfig, ipFilter, poolFilter, poolTypeFilter, providerFilter]);

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
    return <Loading message="Loading IP performance data..." />;
  }

  if (error) {
    return (
      <Card>
        <CardBody>
          <div className="error-state">
            <p>Error loading IP data: {error}</p>
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

  // Count by pool type
  const poolTypeCounts = filteredAndSortedData.reduce(
    (acc, m) => {
      acc[m.pool_type] = (acc[m.pool_type] || 0) + 1;
      return acc;
    },
    {} as Record<string, number>
  );

  return (
    <div className="ip-performance">
      <Card>
        <div className="card-header" style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: '1rem', padding: '1rem 1.5rem', borderBottom: '1px solid var(--border-color)' }}>
          <h2 style={{ margin: 0 }}>Sending IPs - All Providers</h2>
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
            <select
              value={poolFilter}
              onChange={(e) => setPoolFilter(e.target.value)}
              style={{
                padding: '0.5rem',
                borderRadius: '4px',
                border: '1px solid var(--border-color)',
                backgroundColor: 'var(--card-bg)',
                color: 'var(--text-color)',
              }}
            >
              <option value="all">All Pools</option>
              {uniquePools.map(p => (
                <option key={p} value={p}>{p}</option>
              ))}
            </select>
            <select
              value={poolTypeFilter}
              onChange={(e) => setPoolTypeFilter(e.target.value)}
              style={{
                padding: '0.5rem',
                borderRadius: '4px',
                border: '1px solid var(--border-color)',
                backgroundColor: 'var(--card-bg)',
                color: 'var(--text-color)',
              }}
            >
              <option value="all">All Pool Types</option>
              {uniquePoolTypes.map(t => (
                <option key={t} value={t}>{t.charAt(0).toUpperCase() + t.slice(1)}</option>
              ))}
            </select>
            <input
              type="text"
              placeholder="Search IP or Pool..."
              value={ipFilter}
              onChange={(e) => setIpFilter(e.target.value)}
              style={{
                padding: '0.5rem',
                borderRadius: '4px',
                border: '1px solid var(--border-color)',
                backgroundColor: 'var(--card-bg)',
                color: 'var(--text-color)',
                width: '180px',
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
              <div className="stat-label">Total IPs</div>
              <div className="stat-value">{filteredAndSortedData.length}</div>
            </div>
            <div className="summary-stat">
              <div className="stat-label">Total Volume</div>
              <div className="stat-value">{formatNumber(totals.volume)}</div>
            </div>
            <div className="summary-stat">
              <div className="stat-label">Delivered</div>
              <div className="stat-value">{totals.volume > 0 ? formatPercent(totals.delivered / totals.volume) : '-'}</div>
            </div>
            <div className="summary-stat" style={{ borderLeft: '3px solid #48bb78' }}>
              <div className="stat-label">Dedicated</div>
              <div className="stat-value" style={{ color: '#48bb78' }}>{poolTypeCounts.dedicated || 0}</div>
            </div>
            <div className="summary-stat" style={{ borderLeft: '3px solid #d69e2e' }}>
              <div className="stat-label">Shared</div>
              <div className="stat-value" style={{ color: '#d69e2e' }}>{poolTypeCounts.shared || 0}</div>
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
                    onClick={() => handleSort('ip')}
                    style={{ cursor: 'pointer', userSelect: 'none' }}
                  >
                    IP Address{getSortIndicator('ip')}
                  </th>
                  <th
                    onClick={() => handleSort('pool')}
                    style={{ cursor: 'pointer', userSelect: 'none' }}
                  >
                    Pool{getSortIndicator('pool')}
                  </th>
                  <th
                    onClick={() => handleSort('pool_type')}
                    style={{ cursor: 'pointer', userSelect: 'none', textAlign: 'center' }}
                  >
                    Type{getSortIndicator('pool_type')}
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
                    <td colSpan={11} style={{ textAlign: 'center', padding: '2rem' }}>
                      {data?.providers?.length === 0 
                        ? 'No IP data available. IP metrics are currently only available from SparkPost.'
                        : 'No IP data matches your filters'}
                    </td>
                  </tr>
                ) : (
                  filteredAndSortedData.map((metric, index) => (
                    <tr key={`${metric.provider}-${metric.ip}-${index}`}>
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
                      <td style={{ fontFamily: 'monospace', fontWeight: 500 }}>{metric.ip}</td>
                      <td>
                        {metric.pool || '-'}
                        {metric.pool_description && (
                          <div style={{ fontSize: '0.7rem', color: 'var(--text-muted)' }}>
                            {metric.pool_description}
                          </div>
                        )}
                      </td>
                      <td style={{ textAlign: 'center' }}>
                        <PoolTypeBadge type={metric.pool_type} />
                      </td>
                      <td style={{ textAlign: 'right' }}>{formatNumber(metric.volume)}</td>
                      <td style={{ textAlign: 'right' }}>{formatNumber(metric.delivered)}</td>
                      <td style={{ textAlign: 'right' }}>{formatPercent(metric.delivery_rate)}</td>
                      <td style={{ textAlign: 'right' }}>{formatPercent(metric.open_rate)}</td>
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
            {data?.providers?.length === 1 && data.providers[0] === 'sparkpost' && (
              <>
                <span>•</span>
                <span>Note: IP metrics are currently only available from SparkPost</span>
              </>
            )}
          </div>
          
          {/* Pool Type Legend */}
          <div style={{ 
            marginTop: '1rem', 
            padding: '1rem', 
            backgroundColor: 'var(--bg-tertiary)', 
            borderRadius: '8px',
            fontSize: '0.813rem',
          }}>
            <div style={{ fontWeight: 600, marginBottom: '0.5rem', color: 'var(--text-secondary)' }}>
              Pool Types Explained
            </div>
            <div style={{ display: 'flex', gap: '2rem', flexWrap: 'wrap' }}>
              <div style={{ display: 'flex', alignItems: 'flex-start', gap: '0.5rem' }}>
                <PoolTypeBadge type="dedicated" />
                <span style={{ color: 'var(--text-muted)' }}>
                  You have exclusive use - full control over IP reputation
                </span>
              </div>
              <div style={{ display: 'flex', alignItems: 'flex-start', gap: '0.5rem' }}>
                <PoolTypeBadge type="shared" />
                <span style={{ color: 'var(--text-muted)' }}>
                  Shared with others - metrics may be affected by other senders
                </span>
              </div>
            </div>
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

export default IPPerformance;
