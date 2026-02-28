import React from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faUpload, faCheckCircle, faTimesCircle, faClock, faFileExcel, faExclamationCircle } from '@fortawesome/free-solid-svg-icons';
import { Card, CardHeader, CardBody } from '../common/Card';
import { MetricCard } from '../common/MetricCard';
import { HealthStatusBadge } from './HealthStatusBadge';
import type { ImportSummary, Import } from './types';

interface ImportsPanelProps {
  data: ImportSummary | null;
  isLoading?: boolean;
}

const formatNumber = (num: number): string => {
  if (num >= 1000000) return `${(num / 1000000).toFixed(1)}M`;
  if (num >= 1000) return `${(num / 1000).toFixed(1)}K`;
  return num.toLocaleString();
};

const formatTimestamp = (timestamp: string): string => {
  if (!timestamp) return 'N/A';
  // Check if it's a Unix timestamp
  const num = parseInt(timestamp);
  if (!isNaN(num) && num > 1000000000) {
    return new Date(num * 1000).toLocaleString();
  }
  const date = new Date(timestamp);
  return date.toLocaleString();
};

const getImportStatusIcon = (imp: Import): React.ReactNode => {
  const status = imp.status_desc?.toLowerCase() || '';
  if (status.includes('completed') || status.includes('done')) {
    return <FontAwesomeIcon icon={faCheckCircle} style={{ color: 'var(--accent-green)' }} />;
  }
  if (status.includes('processing') || status.includes('running')) {
    return <FontAwesomeIcon icon={faClock} style={{ color: 'var(--accent-blue)' }} />;
  }
  if (status.includes('failed') || status.includes('error')) {
    return <FontAwesomeIcon icon={faTimesCircle} style={{ color: 'var(--accent-red)' }} />;
  }
  return <FontAwesomeIcon icon={faFileExcel} style={{ color: 'var(--text-muted)' }} />;
};

const getProgressBar = (progress: string): React.ReactNode => {
  const percent = parseFloat(progress) || 0;
  if (percent >= 100) return null;
  
  return (
    <div style={{
      width: '100%',
      height: '4px',
      backgroundColor: 'var(--border-color)',
      borderRadius: '2px',
      overflow: 'hidden',
      marginTop: '4px',
    }}>
      <div style={{
        width: `${percent}%`,
        height: '100%',
        backgroundColor: 'var(--accent-blue)',
        borderRadius: '2px',
        transition: 'width 0.3s ease',
      }} />
    </div>
  );
};

export const ImportsPanel: React.FC<ImportsPanelProps> = ({ data, isLoading }) => {
  if (isLoading) {
    return (
      <Card>
        <CardHeader title={<><FontAwesomeIcon icon={faUpload} /> Data Imports (Ongage)</>} />
        <CardBody>
          <div style={{ textAlign: 'center', padding: '2rem', color: 'var(--text-muted)' }}>
            Loading import data...
          </div>
        </CardBody>
      </Card>
    );
  }

  if (!data || data.status === 'unknown') {
    return (
      <Card>
        <CardHeader title={<><FontAwesomeIcon icon={faUpload} /> Data Imports (Ongage)</>} />
        <CardBody>
          <div style={{ textAlign: 'center', padding: '2rem', color: 'var(--text-muted)' }}>
            Ongage imports not configured or data unavailable
          </div>
        </CardBody>
      </Card>
    );
  }

  // Calculate success rate
  const successRate = data.total_records > 0 
    ? data.success_records / data.total_records 
    : 0;

  return (
    <Card>
      <CardHeader 
        title={
          <span style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
            <FontAwesomeIcon icon={faUpload} /> Data Imports (Ongage)
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
            label="Total Imports"
            value={data.total_imports}
            subtitle="Last 7 days"
          />
          <MetricCard
            label="Today's Imports"
            value={data.today_imports}
            status={data.today_imports > 0 ? 'healthy' : undefined}
          />
          <MetricCard
            label="Success Rate"
            value={`${(successRate * 100).toFixed(1)}%`}
            status={successRate >= 0.95 ? 'healthy' : successRate >= 0.8 ? 'warning' : 'critical'}
          />
          <MetricCard
            label="In Progress"
            value={data.in_progress}
            status={data.in_progress > 5 ? 'warning' : undefined}
          />
        </div>

        {/* Record Breakdown */}
        <div style={{ 
          display: 'grid', 
          gridTemplateColumns: 'repeat(4, 1fr)', 
          gap: '1rem',
          marginBottom: '1.5rem',
          padding: '1rem',
          backgroundColor: 'var(--bg-secondary)',
          borderRadius: '8px',
        }}>
          <div style={{ textAlign: 'center' }}>
            <div style={{ fontSize: '1.2rem', fontWeight: 600, color: 'var(--accent-green)' }}>
              {formatNumber(data.success_records)}
            </div>
            <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>Successful</div>
          </div>
          <div style={{ textAlign: 'center' }}>
            <div style={{ fontSize: '1.2rem', fontWeight: 600, color: 'var(--accent-red)' }}>
              {formatNumber(data.failed_records)}
            </div>
            <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>Failed</div>
          </div>
          <div style={{ textAlign: 'center' }}>
            <div style={{ fontSize: '1.2rem', fontWeight: 600, color: 'var(--accent-yellow)' }}>
              {formatNumber(data.duplicate_records)}
            </div>
            <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>Duplicates</div>
          </div>
          <div style={{ textAlign: 'center' }}>
            <div style={{ fontSize: '1.2rem', fontWeight: 600, color: 'var(--text-primary)' }}>
              {formatNumber(data.total_records)}
            </div>
            <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>Total Records</div>
          </div>
        </div>

        {/* In Progress Alert */}
        {data.in_progress > 0 && (
          <div style={{
            backgroundColor: 'rgba(59, 130, 246, 0.1)',
            border: '1px solid var(--accent-blue)',
            borderRadius: '8px',
            padding: '0.75rem 1rem',
            marginBottom: '1.5rem',
            display: 'flex',
            alignItems: 'center',
            gap: '8px',
          }}>
            <FontAwesomeIcon icon={faClock} style={{ color: 'var(--accent-blue)' }} />
            <span style={{ color: 'var(--accent-blue)', fontWeight: 500 }}>
              {data.in_progress} import{data.in_progress > 1 ? 's' : ''} currently processing
            </span>
          </div>
        )}

        {/* Failed Records Alert */}
        {data.failed_records > 0 && (
          <div style={{
            backgroundColor: 'rgba(239, 68, 68, 0.1)',
            border: '1px solid var(--accent-red)',
            borderRadius: '8px',
            padding: '0.75rem 1rem',
            marginBottom: '1.5rem',
            display: 'flex',
            alignItems: 'center',
            gap: '8px',
          }}>
            <FontAwesomeIcon icon={faExclamationCircle} style={{ color: 'var(--accent-red)' }} />
            <span style={{ color: 'var(--accent-red)', fontWeight: 500 }}>
              {formatNumber(data.failed_records)} records failed to import
            </span>
          </div>
        )}

        {/* Recent Imports Table */}
        <div>
          <h4 style={{ marginBottom: '0.75rem', fontSize: '0.9rem', color: 'var(--text-secondary)' }}>
            Recent Imports
          </h4>
          <div style={{ overflowX: 'auto' }}>
            <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: '0.85rem' }}>
              <thead>
                <tr style={{ borderBottom: '1px solid var(--border-color)' }}>
                  <th style={{ textAlign: 'left', padding: '8px', color: 'var(--text-muted)' }}>File</th>
                  <th style={{ textAlign: 'center', padding: '8px', color: 'var(--text-muted)' }}>Status</th>
                  <th style={{ textAlign: 'right', padding: '8px', color: 'var(--text-muted)' }}>Total</th>
                  <th style={{ textAlign: 'right', padding: '8px', color: 'var(--text-muted)' }}>Success</th>
                  <th style={{ textAlign: 'right', padding: '8px', color: 'var(--text-muted)' }}>Failed</th>
                  <th style={{ textAlign: 'left', padding: '8px', color: 'var(--text-muted)' }}>Created</th>
                </tr>
              </thead>
              <tbody>
                {data.recent_imports?.slice(0, 10).map((imp: Import) => (
                  <tr key={imp.id} style={{ borderBottom: '1px solid var(--border-color)' }}>
                    <td style={{ padding: '8px' }}>
                      <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
                        <FontAwesomeIcon icon={faFileExcel} style={{ color: 'var(--text-muted)' }} />
                        <span style={{ fontWeight: 500, maxWidth: '200px', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                          {imp.name || `Import ${imp.id}`}
                        </span>
                      </div>
                      {imp.progress && parseFloat(imp.progress) < 100 && getProgressBar(imp.progress)}
                    </td>
                    <td style={{ padding: '8px', textAlign: 'center' }}>
                      <span style={{ display: 'inline-flex', alignItems: 'center', gap: '4px' }}>
                        {getImportStatusIcon(imp)}
                        <span style={{ fontSize: '0.75rem' }}>{imp.status_desc || 'Unknown'}</span>
                      </span>
                    </td>
                    <td style={{ padding: '8px', textAlign: 'right' }}>
                      {formatNumber(parseInt(imp.total) || 0)}
                    </td>
                    <td style={{ padding: '8px', textAlign: 'right', color: 'var(--accent-green)' }}>
                      {formatNumber(parseInt(imp.success) || 0)}
                    </td>
                    <td style={{ 
                      padding: '8px', 
                      textAlign: 'right', 
                      color: parseInt(imp.failed) > 0 ? 'var(--accent-red)' : 'var(--text-muted)',
                    }}>
                      {formatNumber(parseInt(imp.failed) || 0)}
                    </td>
                    <td style={{ padding: '8px', fontSize: '0.8rem', color: 'var(--text-muted)' }}>
                      {formatTimestamp(imp.created)}
                    </td>
                  </tr>
                ))}
                {(!data.recent_imports || data.recent_imports.length === 0) && (
                  <tr>
                    <td colSpan={6} style={{ padding: '1rem', textAlign: 'center', color: 'var(--text-muted)' }}>
                      No recent imports found
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

export default ImportsPanel;
