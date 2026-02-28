import React from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faShieldAlt, faCheckCircle, faTimesCircle, faExclamationTriangle, faChartPie } from '@fortawesome/free-solid-svg-icons';
import { Card, CardHeader, CardBody } from '../common/Card';
import { MetricCard } from '../common/MetricCard';
import { HealthStatusBadge } from './HealthStatusBadge';
import type { ValidationSummary, ValidationStatus as ValidationStatusType } from './types';

interface ValidationPanelProps {
  data: ValidationSummary | null;
  isLoading?: boolean;
}

const formatNumber = (num: number): string => {
  if (num >= 1000000) return `${(num / 1000000).toFixed(1)}M`;
  if (num >= 1000) return `${(num / 1000).toFixed(1)}K`;
  return num.toLocaleString();
};

const formatTimestamp = (timestamp: string): string => {
  if (!timestamp) return 'N/A';
  const date = new Date(timestamp);
  return date.toLocaleString();
};

const getStatusColor = (statusId: string): string => {
  const status = statusId?.toLowerCase() || '';
  if (status.includes('valid') && !status.includes('invalid')) return 'var(--accent-green)';
  if (status.includes('invalid') || status.includes('bad')) return 'var(--accent-red)';
  if (status.includes('risky') || status.includes('unknown') || status.includes('catch')) return 'var(--accent-yellow)';
  return 'var(--accent-blue)';
};

const getStatusIcon = (statusId: string): React.ReactNode => {
  const status = statusId?.toLowerCase() || '';
  if (status.includes('valid') && !status.includes('invalid')) return <FontAwesomeIcon icon={faCheckCircle} />;
  if (status.includes('invalid') || status.includes('bad')) return <FontAwesomeIcon icon={faTimesCircle} />;
  if (status.includes('risky') || status.includes('unknown')) return <FontAwesomeIcon icon={faExclamationTriangle} />;
  return <FontAwesomeIcon icon={faChartPie} />;
};

export const ValidationPanel: React.FC<ValidationPanelProps> = ({ data, isLoading }) => {
  if (isLoading) {
    return (
      <Card>
        <CardHeader title={<><FontAwesomeIcon icon={faShieldAlt} /> Email Validation (Snowflake)</>} />
        <CardBody>
          <div style={{ textAlign: 'center', padding: '2rem', color: 'var(--text-muted)' }}>
            Loading validation data...
          </div>
        </CardBody>
      </Card>
    );
  }

  if (!data || data.status === 'unknown') {
    return (
      <Card>
        <CardHeader title={<><FontAwesomeIcon icon={faShieldAlt} /> Email Validation (Snowflake)</>} />
        <CardBody>
          <div style={{ textAlign: 'center', padding: '2rem', color: 'var(--text-muted)' }}>
            Snowflake not configured or data unavailable
          </div>
        </CardBody>
      </Card>
    );
  }

  // Calculate percentages for status breakdown
  const totalStatusRecords = data.status_breakdown?.reduce((sum, s) => sum + s.count, 0) || 0;
  const statusWithPercentage = data.status_breakdown?.map(s => ({
    ...s,
    percentage: totalStatusRecords > 0 ? (s.count / totalStatusRecords) * 100 : 0,
  })) || [];

  // Calculate valid rate
  const validRecords = data.status_breakdown?.find(s => 
    s.status_id?.toLowerCase().includes('valid') && !s.status_id?.toLowerCase().includes('invalid')
  )?.count || 0;
  const validRate = totalStatusRecords > 0 ? validRecords / totalStatusRecords : 0;

  return (
    <Card>
      <CardHeader 
        title={
          <span style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
            <FontAwesomeIcon icon={faShieldAlt} /> Email Validation (Snowflake)
            <HealthStatusBadge status={data.status} size="small" />
          </span>
        }
        action={
          <span style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>
            Last updated: {formatTimestamp(data.last_fetch)}
          </span>
        }
      />
      <CardBody>
        {/* Summary Metrics */}
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: '1rem', marginBottom: '1.5rem' }}>
          <MetricCard
            label="Total Records"
            value={formatNumber(data.total_records)}
            subtitle="All time validated"
          />
          <MetricCard
            label="Today's Validations"
            value={formatNumber(data.today_records)}
            status={data.today_records > 0 ? 'healthy' : 'warning'}
          />
          <MetricCard
            label="Valid Rate"
            value={`${(validRate * 100).toFixed(1)}%`}
            status={validRate >= 0.8 ? 'healthy' : validRate >= 0.6 ? 'warning' : 'critical'}
          />
          <MetricCard
            label="Unique Statuses"
            value={data.unique_statuses}
          />
        </div>

        {/* Status Breakdown */}
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '1.5rem' }}>
          {/* Validation Status Breakdown */}
          <div>
            <h4 style={{ marginBottom: '0.75rem', fontSize: '0.9rem', color: 'var(--text-secondary)' }}>
              Validation Status Breakdown
            </h4>
            <div style={{ display: 'flex', flexDirection: 'column', gap: '8px' }}>
              {statusWithPercentage.slice(0, 8).map((status: ValidationStatusType & { percentage: number }) => (
                <div key={status.status_id} style={{ 
                  display: 'flex', 
                  alignItems: 'center', 
                  gap: '8px',
                }}>
                  <div style={{ 
                    width: '100%', 
                    maxWidth: '200px',
                    display: 'flex',
                    alignItems: 'center',
                    gap: '6px',
                    color: getStatusColor(status.status_id),
                  }}>
                    {getStatusIcon(status.status_id)}
                    <span style={{ fontSize: '0.85rem', fontWeight: 500 }}>
                      {status.status_id || 'Unknown'}
                    </span>
                  </div>
                  <div style={{ 
                    flex: 1, 
                    height: '8px', 
                    backgroundColor: 'var(--border-color)',
                    borderRadius: '4px',
                    overflow: 'hidden',
                  }}>
                    <div style={{
                      width: `${status.percentage}%`,
                      height: '100%',
                      backgroundColor: getStatusColor(status.status_id),
                      borderRadius: '4px',
                    }} />
                  </div>
                  <div style={{ 
                    minWidth: '80px', 
                    textAlign: 'right',
                    fontSize: '0.8rem',
                    color: 'var(--text-muted)',
                  }}>
                    {formatNumber(status.count)} ({status.percentage.toFixed(1)}%)
                  </div>
                </div>
              ))}
            </div>
          </div>

          {/* Domain Group Breakdown */}
          <div>
            <h4 style={{ marginBottom: '0.75rem', fontSize: '0.9rem', color: 'var(--text-secondary)' }}>
              Top Email Domains
            </h4>
            <div style={{ overflowX: 'auto' }}>
              <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: '0.85rem' }}>
                <thead>
                  <tr style={{ borderBottom: '1px solid var(--border-color)' }}>
                    <th style={{ textAlign: 'left', padding: '6px', color: 'var(--text-muted)' }}>Domain</th>
                    <th style={{ textAlign: 'right', padding: '6px', color: 'var(--text-muted)' }}>Count</th>
                  </tr>
                </thead>
                <tbody>
                  {data.domain_breakdown?.slice(0, 10).map((domain) => (
                    <tr key={domain.domain_group} style={{ borderBottom: '1px solid var(--border-color)' }}>
                      <td style={{ padding: '6px' }}>
                        <span style={{ fontWeight: 500 }}>{domain.domain_group_short}</span>
                        <span style={{ color: 'var(--text-muted)', marginLeft: '8px', fontSize: '0.75rem' }}>
                          {domain.domain_group}
                        </span>
                      </td>
                      <td style={{ padding: '6px', textAlign: 'right' }}>{formatNumber(domain.count)}</td>
                    </tr>
                  ))}
                  {(!data.domain_breakdown || data.domain_breakdown.length === 0) && (
                    <tr>
                      <td colSpan={2} style={{ padding: '1rem', textAlign: 'center', color: 'var(--text-muted)' }}>
                        No domain data available
                      </td>
                    </tr>
                  )}
                </tbody>
              </table>
            </div>
          </div>
        </div>

        {/* Daily Metrics Trend */}
        {data.daily_metrics && data.daily_metrics.length > 0 && (
          <div style={{ marginTop: '1.5rem' }}>
            <h4 style={{ marginBottom: '0.75rem', fontSize: '0.9rem', color: 'var(--text-secondary)' }}>
              Daily Validation Volume (Last 7 Days)
            </h4>
            <div style={{ display: 'flex', gap: '8px', alignItems: 'flex-end', height: '80px' }}>
              {data.daily_metrics.slice(0, 7).reverse().map((day) => {
                const maxCount = Math.max(...data.daily_metrics.map(d => d.total_records));
                const height = maxCount > 0 ? (day.total_records / maxCount) * 100 : 0;
                return (
                  <div 
                    key={day.date} 
                    style={{ 
                      flex: 1, 
                      display: 'flex', 
                      flexDirection: 'column', 
                      alignItems: 'center',
                      gap: '4px',
                    }}
                  >
                    <div style={{
                      width: '100%',
                      maxWidth: '40px',
                      height: `${height}%`,
                      minHeight: '4px',
                      backgroundColor: 'var(--accent-blue)',
                      borderRadius: '4px 4px 0 0',
                    }} />
                    <span style={{ fontSize: '0.65rem', color: 'var(--text-muted)' }}>
                      {new Date(day.date).toLocaleDateString('en-US', { weekday: 'short' })}
                    </span>
                  </div>
                );
              })}
            </div>
          </div>
        )}
      </CardBody>
    </Card>
  );
};

export default ValidationPanel;
