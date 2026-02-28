import React from 'react';
import { Card, CardHeader, CardBody, Loading, ErrorDisplay } from '../common';
import { ISPTable } from './ISPTable';
import { useApi } from '../../hooks/useApi';
import type { ISPMetrics } from '../../types';

const POLLING_INTERVAL = 60000;

export const ISPView: React.FC = () => {
  const { data, loading, error, refetch } = useApi<{ timestamp: string; data: ISPMetrics[] }>(
    '/api/metrics/isp',
    { pollingInterval: POLLING_INTERVAL }
  );

  if (loading && !data) {
    return <Loading message="Loading ISP metrics..." />;
  }

  if (error) {
    return <ErrorDisplay message={error} onRetry={refetch} />;
  }

  const ispMetrics = data?.data ?? [];

  // Group ISPs by status
  const critical = ispMetrics.filter((isp) => isp.status === 'critical');
  const warning = ispMetrics.filter((isp) => isp.status === 'warning');
  const healthy = ispMetrics.filter((isp) => isp.status === 'healthy');

  return (
    <div>
      {/* Status Summary */}
      <div className="grid grid-3 mb-6">
        <Card>
          <CardBody>
            <div style={{ textAlign: 'center' }}>
              <div style={{ 
                fontSize: '2.5rem', 
                fontWeight: 700, 
                color: 'var(--accent-green)',
              }}>
                {healthy.length}
              </div>
              <div style={{ color: 'var(--text-secondary)' }}>Healthy ISPs</div>
            </div>
          </CardBody>
        </Card>
        <Card>
          <CardBody>
            <div style={{ textAlign: 'center' }}>
              <div style={{ 
                fontSize: '2.5rem', 
                fontWeight: 700, 
                color: 'var(--accent-yellow)',
              }}>
                {warning.length}
              </div>
              <div style={{ color: 'var(--text-secondary)' }}>Warning</div>
            </div>
          </CardBody>
        </Card>
        <Card>
          <CardBody>
            <div style={{ textAlign: 'center' }}>
              <div style={{ 
                fontSize: '2.5rem', 
                fontWeight: 700, 
                color: 'var(--accent-red)',
              }}>
                {critical.length}
              </div>
              <div style={{ color: 'var(--text-secondary)' }}>Critical</div>
            </div>
          </CardBody>
        </Card>
      </div>

      {/* Critical ISPs */}
      {critical.length > 0 && (
        <Card className="mb-6">
          <CardHeader title="⚠️ Critical ISPs" />
          <CardBody>
            <ISPTable data={critical} />
          </CardBody>
        </Card>
      )}

      {/* Warning ISPs */}
      {warning.length > 0 && (
        <Card className="mb-6">
          <CardHeader title="⚡ Warning ISPs" />
          <CardBody>
            <ISPTable data={warning} />
          </CardBody>
        </Card>
      )}

      {/* All ISPs */}
      <Card>
        <CardHeader title="All ISP Performance" />
        <CardBody>
          <ISPTable data={ispMetrics} />
        </CardBody>
      </Card>

      {/* Last updated */}
      <div style={{ 
        marginTop: '1.5rem', 
        textAlign: 'center', 
        color: 'var(--text-muted)',
        fontSize: '0.75rem',
      }}>
        Last updated: {data?.timestamp 
          ? new Date(data.timestamp).toLocaleString() 
          : 'Never'}
      </div>
    </div>
  );
};

export default ISPView;
