import React, { useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faDatabase, faClock, faArrowUp, faStopCircle, faBell, faCalendar } from '@fortawesome/free-solid-svg-icons';
import { Card, CardHeader, CardBody } from '../common/Card';
import { MetricCard } from '../common/MetricCard';
import { HealthStatusBadge } from './HealthStatusBadge';
import type { IngestionSummary, DataSetMetrics, PartnerHealth } from './types';

interface IngestionPanelProps {
  data: IngestionSummary | null;
  isLoading?: boolean;
}

type DateRange = 'today' | '7d' | '30d' | '365d';

const formatNumber = (num: number | undefined | null): string => {
  if (num == null) return '0';
  if (num >= 1000000) return `${(num / 1000000).toFixed(1)}M`;
  if (num >= 1000) return `${(num / 1000).toFixed(1)}K`;
  return num.toLocaleString();
};

const formatTimestamp = (timestamp: string): string => {
  if (!timestamp) return 'N/A';
  const date = new Date(timestamp);
  return date.toLocaleString();
};

const formatHoursAgo = (hours: number): string => {
  if (hours < 1) return 'Less than 1 hour ago';
  if (hours < 24) return `${Math.floor(hours)} hours ago`;
  const days = Math.floor(hours / 24);
  return `${days} day${days > 1 ? 's' : ''} ago`;
};

const formatMinutesAgo = (hours: number): string => {
  const minutes = Math.floor(hours * 60);
  if (minutes < 1) return 'Just now';
  if (minutes < 60) return `${minutes} min ago`;
  return formatHoursAgo(hours);
};

export const IngestionPanel: React.FC<IngestionPanelProps> = ({ data, isLoading }) => {
  const [dateRange, setDateRange] = useState<DateRange>('today');

  if (isLoading) {
    return (
      <Card>
        <CardHeader title={<><FontAwesomeIcon icon={faDatabase} /> Partner Data Ingestion (Azure)</>} />
        <CardBody>
          <div style={{ textAlign: 'center', padding: '2rem', color: 'var(--text-muted)' }}>
            Loading ingestion data...
          </div>
        </CardBody>
      </Card>
    );
  }

  if (!data || data.status === 'unknown') {
    return (
      <Card>
        <CardHeader title={<><FontAwesomeIcon icon={faDatabase} /> Partner Data Ingestion (Azure)</>} />
        <CardBody>
          <div style={{ textAlign: 'center', padding: '2rem', color: 'var(--text-muted)' }}>
            Azure Table Storage not configured or data unavailable
          </div>
        </CardBody>
      </Card>
    );
  }

  const partnerAlerts = data.partner_alerts || [];
  const systemHealth = data.system_health;
  
  // Get historical data for selected range
  const historicalData = dateRange !== 'today' && data.historical ? data.historical[dateRange] : null;
  
  // Calculate display values based on date range
  const displayRecords = dateRange === 'today' ? data.today_records : (historicalData?.total_records || 0);
  const displayLabel = dateRange === 'today' ? "Today's Records" : `${dateRange.toUpperCase()} Records`;

  return (
    <Card>
      <CardHeader 
        title={
          <span style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
            <FontAwesomeIcon icon={faDatabase} /> Partner Data Ingestion (Azure)
            <HealthStatusBadge status={data.status} size="small" />
          </span>
        }
        action={
          <div style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
            {/* Date Range Filter */}
            <div style={{ display: 'flex', gap: '4px' }}>
              {(['today', '7d', '30d', '365d'] as DateRange[]).map((range) => (
                <button
                  key={range}
                  onClick={() => setDateRange(range)}
                  style={{
                    padding: '4px 8px',
                    fontSize: '0.7rem',
                    border: 'none',
                    borderRadius: '4px',
                    cursor: 'pointer',
                    backgroundColor: dateRange === range ? 'var(--accent-blue)' : 'var(--bg-tertiary)',
                    color: dateRange === range ? 'white' : 'var(--text-muted)',
                    transition: 'all 0.2s ease',
                  }}
                >
                  {range === 'today' ? 'Today' : range.toUpperCase()}
                </button>
              ))}
            </div>
            <span style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>
              Last updated: {formatTimestamp(data.last_fetch)}
            </span>
          </div>
        }
      />
      <CardBody>
        {/* CRITICAL: System Health Alert */}
        {systemHealth && !systemHealth.processor_running && (
          <div style={{
            backgroundColor: 'rgba(239, 68, 68, 0.15)',
            border: '2px solid var(--accent-red)',
            borderRadius: '8px',
            padding: '1rem',
            marginBottom: '1.5rem',
          }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: '8px', marginBottom: '0.5rem', color: 'var(--accent-red)' }}>
              <FontAwesomeIcon icon={faStopCircle} />
              <strong>CRITICAL: Data Processor Not Running</strong>
            </div>
            <div style={{ fontSize: '0.9rem', color: 'var(--text-primary)' }}>
              No data has been hydrated in <strong>{systemHealth.hours_since_hydration.toFixed(1)} hours</strong>.
              The processor should hydrate at least one data set every hour.
            </div>
            <div style={{ fontSize: '0.8rem', color: 'var(--text-muted)', marginTop: '0.5rem' }}>
              Last hydration: {systemHealth.last_hydration_time ? formatTimestamp(systemHealth.last_hydration_time) : 'Unknown'}
            </div>
          </div>
        )}

        {/* System Health Status (when healthy) */}
        {systemHealth && systemHealth.processor_running && (
          <div style={{
            backgroundColor: 'rgba(34, 197, 94, 0.1)',
            border: '1px solid var(--accent-green)',
            borderRadius: '8px',
            padding: '0.75rem 1rem',
            marginBottom: '1.5rem',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
          }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: '8px', color: 'var(--accent-green)' }}>
              <FontAwesomeIcon icon={faArrowUp} />
              <span>Processor Running</span>
            </div>
            <span style={{ fontSize: '0.8rem', color: 'var(--text-muted)' }}>
              Last hydration: {formatMinutesAgo(systemHealth.hours_since_hydration)}
            </span>
          </div>
        )}

        {/* Summary Metrics */}
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: '1rem', marginBottom: '1.5rem' }}>
          <MetricCard
            label="Total Records"
            value={formatNumber(data.total_records)}
            subtitle="All time"
          />
          <MetricCard
            label={displayLabel}
            value={formatNumber(displayRecords)}
            status={displayRecords > 0 ? 'healthy' : 'warning'}
            subtitle={historicalData ? `Avg: ${formatNumber(historicalData.daily_average)}/day` : undefined}
          />
          <MetricCard
            label="Accepted Today"
            value={formatNumber(data.accepted_today)}
            status={data.accepted_today > 0 ? 'healthy' : 'warning'}
          />
          <MetricCard
            label="Active Data Sets"
            value={data.data_sets_active}
            status="healthy"
          />
        </div>

        {/* Partner Alerts (Less prominent) */}
        {partnerAlerts.length > 0 && (
          <div style={{
            backgroundColor: 'rgba(251, 191, 36, 0.08)',
            border: '1px solid rgba(251, 191, 36, 0.3)',
            borderRadius: '8px',
            padding: '0.75rem 1rem',
            marginBottom: '1.5rem',
          }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: '8px', marginBottom: '0.5rem', color: 'var(--accent-yellow)' }}>
              <FontAwesomeIcon icon={faBell} />
              <span style={{ fontSize: '0.85rem', fontWeight: 500 }}>Partner Feed Alerts ({partnerAlerts.length})</span>
            </div>
            <div style={{ fontSize: '0.8rem', color: 'var(--text-secondary)' }}>
              {partnerAlerts.slice(0, 3).map((alert: PartnerHealth) => (
                <div key={alert.data_set_code} style={{ marginBottom: '4px', display: 'flex', alignItems: 'center', gap: '8px' }}>
                  <span style={{ 
                    width: '6px', 
                    height: '6px', 
                    borderRadius: '50%', 
                    backgroundColor: alert.status === 'critical' ? 'var(--accent-red)' : 'var(--accent-yellow)',
                  }} />
                  <span><strong>{alert.data_partner || alert.data_set_code}</strong></span>
                  <span style={{ color: 'var(--text-muted)' }}>— no data in {Math.floor(alert.gap_hours)}h</span>
                </div>
              ))}
              {partnerAlerts.length > 3 && (
                <div style={{ color: 'var(--text-muted)', marginTop: '4px' }}>
                  +{partnerAlerts.length - 3} more partners with gaps
                </div>
              )}
            </div>
          </div>
        )}

        {/* Historical Comparison Chart (when not "today") */}
        {historicalData && historicalData.daily_counts && historicalData.daily_counts.length > 0 && (
          <div style={{ marginBottom: '1.5rem' }}>
            <h4 style={{ marginBottom: '0.75rem', fontSize: '0.9rem', color: 'var(--text-secondary)', display: 'flex', alignItems: 'center', gap: '8px' }}>
              <FontAwesomeIcon icon={faCalendar} />
              Daily Volume ({dateRange.toUpperCase()})
            </h4>
            <div style={{ 
              display: 'flex', 
              gap: '2px', 
              height: '60px', 
              alignItems: 'flex-end',
              padding: '0.5rem',
              backgroundColor: 'var(--bg-primary)',
              borderRadius: '8px',
            }}>
              {/* Group by date and show bars */}
              {(() => {
                const dailyTotals = new Map<string, number>();
                historicalData.daily_counts.forEach(dc => {
                  dailyTotals.set(dc.date, (dailyTotals.get(dc.date) || 0) + dc.count);
                });
                const sortedDays = Array.from(dailyTotals.entries()).sort((a, b) => a[0].localeCompare(b[0]));
                const maxCount = Math.max(...sortedDays.map(d => d[1]), 1);
                
                return sortedDays.slice(-30).map(([date, count]) => {
                  const height = (count / maxCount) * 100;
                  const isToday = date === new Date().toISOString().split('T')[0];
                  return (
                    <div
                      key={date}
                      title={`${date}: ${formatNumber(count)} records`}
                      style={{
                        flex: 1,
                        minWidth: '4px',
                        maxWidth: '20px',
                        height: `${height}%`,
                        backgroundColor: isToday ? 'var(--accent-blue)' : 'var(--accent-green)',
                        borderRadius: '2px 2px 0 0',
                        opacity: isToday ? 1 : 0.6,
                        cursor: 'pointer',
                      }}
                    />
                  );
                });
              })()}
            </div>
          </div>
        )}

        {/* Data Sets Table */}
        <div style={{ marginTop: '1rem' }}>
          <h4 style={{ marginBottom: '0.75rem', fontSize: '0.9rem', color: 'var(--text-secondary)' }}>
            Data Sets by Partner
          </h4>
          <div style={{ overflowX: 'auto' }}>
            <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: '0.85rem' }}>
              <thead>
                <tr style={{ borderBottom: '1px solid var(--border-color)' }}>
                  <th style={{ textAlign: 'left', padding: '8px', color: 'var(--text-muted)' }}>Data Set</th>
                  <th style={{ textAlign: 'left', padding: '8px', color: 'var(--text-muted)' }}>Partner</th>
                  <th style={{ textAlign: 'right', padding: '8px', color: 'var(--text-muted)' }}>Total</th>
                  <th style={{ textAlign: 'right', padding: '8px', color: 'var(--text-muted)' }}>Today</th>
                  <th style={{ textAlign: 'left', padding: '8px', color: 'var(--text-muted)' }}>Last Update</th>
                  <th style={{ textAlign: 'center', padding: '8px', color: 'var(--text-muted)' }}>Status</th>
                </tr>
              </thead>
              <tbody>
                {data.data_sets?.map((ds: DataSetMetrics) => (
                  <tr key={ds.data_set_code} style={{ borderBottom: '1px solid var(--border-color)' }}>
                    <td style={{ padding: '8px', fontWeight: 500 }}>{ds.data_set_code}</td>
                    <td style={{ padding: '8px', color: 'var(--text-secondary)' }}>{ds.data_partner || '-'}</td>
                    <td style={{ padding: '8px', textAlign: 'right' }}>{formatNumber(ds.record_count)}</td>
                    <td style={{ 
                      padding: '8px', 
                      textAlign: 'right',
                      color: ds.today_count > 0 ? 'var(--accent-green)' : 'var(--text-muted)',
                    }}>
                      {ds.today_count > 0 ? `+${formatNumber(ds.today_count)}` : '-'}
                    </td>
                    <td style={{ padding: '8px', fontSize: '0.8rem', color: 'var(--text-muted)' }}>
                      {ds.last_timestamp ? new Date(ds.last_timestamp).toLocaleString() : 'N/A'}
                    </td>
                    <td style={{ padding: '8px', textAlign: 'center' }}>
                      {ds.gap_hours > 24 ? (
                        <span style={{ 
                          color: ds.gap_hours > 48 ? 'var(--accent-red)' : 'var(--accent-yellow)',
                          display: 'inline-flex',
                          alignItems: 'center',
                          gap: '4px',
                          fontSize: '0.75rem',
                        }}>
                          <FontAwesomeIcon icon={faClock} /> {Math.floor(ds.gap_hours)}h gap
                        </span>
                      ) : (
                        <span style={{ color: 'var(--accent-green)' }}>
                          <FontAwesomeIcon icon={faArrowUp} />
                        </span>
                      )}
                    </td>
                  </tr>
                ))}
                {(!data.data_sets || data.data_sets.length === 0) && (
                  <tr>
                    <td colSpan={6} style={{ padding: '1rem', textAlign: 'center', color: 'var(--text-muted)' }}>
                      No data sets found
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>
        </div>
      </CardBody>
    </Card>
  );
};

export default IngestionPanel;
