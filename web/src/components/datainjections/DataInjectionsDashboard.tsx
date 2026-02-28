import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faSync, faChartLine, faExclamationTriangle, faCheckCircle } from '@fortawesome/free-solid-svg-icons';
import { Card, CardHeader, CardBody } from '../common/Card';
import { HealthStatusBadge } from './HealthStatusBadge';
import { IngestionPanel } from './IngestionPanel';
import { ValidationPanel } from './ValidationPanel';
import { ImportsPanel } from './ImportsPanel';
import { useDateFilter } from '../../context/DateFilterContext';
import type { DataInjectionsDashboard as DashboardData, HealthStatus } from './types';

interface DataInjectionsDashboardProps {
  autoRefresh?: boolean;
  refreshInterval?: number; // in seconds
}

export const DataInjectionsDashboard: React.FC<DataInjectionsDashboardProps> = ({
  autoRefresh = true,
  refreshInterval = 60,
}) => {
  const [dashboard, setDashboard] = useState<DashboardData | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [lastRefresh, setLastRefresh] = useState<Date | null>(null);
  const [isRefreshing, setIsRefreshing] = useState(false);
  
  // Use global date filter
  const { dateRange } = useDateFilter();

  const fetchDashboard = useCallback(async (showLoading = true) => {
    try {
      if (showLoading) setLoading(true);
      setIsRefreshing(true);
      setError(null);

      // Build URL with date range params
      const params = new URLSearchParams({
        start_date: dateRange.startDate,
        end_date: dateRange.endDate,
        range_type: dateRange.type,
      });
      
      const response = await fetch(`/api/data-injections/dashboard?${params}`);
      
      if (!response.ok) {
        const errorData = await response.json().catch(() => ({}));
        throw new Error(errorData.error || `HTTP ${response.status}`);
      }

      const data = await response.json();
      setDashboard(data);
      setLastRefresh(new Date());
    } catch (err) {
      console.error('Failed to fetch data injections dashboard:', err);
      setError(err instanceof Error ? err.message : 'Failed to fetch data');
    } finally {
      setLoading(false);
      setIsRefreshing(false);
    }
  }, [dateRange]); // Refetch when date range changes

  const triggerRefresh = useCallback(async () => {
    try {
      setIsRefreshing(true);
      await fetch('/api/data-injections/refresh', { method: 'POST' });
      // Wait a moment for data to refresh
      setTimeout(() => fetchDashboard(false), 2000);
    } catch (err) {
      console.error('Failed to trigger refresh:', err);
    }
  }, [fetchDashboard]);

  // Initial fetch
  useEffect(() => {
    fetchDashboard();
  }, [fetchDashboard]);

  // Auto refresh
  useEffect(() => {
    if (!autoRefresh) return;

    const interval = setInterval(() => {
      fetchDashboard(false);
    }, refreshInterval * 1000);

    return () => clearInterval(interval);
  }, [autoRefresh, refreshInterval, fetchDashboard]);

  const getOverallHealthMessage = (health: HealthStatus, issues: string[]): string => {
    switch (health) {
      case 'healthy':
        return 'All data injection pipelines are operating normally';
      case 'warning':
        return issues.length > 0 ? issues[0] : 'Some data injection issues detected';
      case 'critical':
        return issues.length > 0 ? issues[0] : 'Critical data injection issues detected';
      default:
        return 'Data injection status unknown';
    }
  };

  if (loading && !dashboard) {
    return (
      <div className="dashboard-container">
        <div style={{ textAlign: 'center', padding: '4rem', color: 'var(--text-muted)' }}>
          <FontAwesomeIcon icon={faSync} spin style={{ marginBottom: '1rem', fontSize: '32px' }} />
          <div>Loading Data Injections Dashboard...</div>
        </div>
      </div>
    );
  }

  if (error && !dashboard) {
    return (
      <div className="dashboard-container">
        <Card>
          <CardBody>
            <div style={{ textAlign: 'center', padding: '2rem' }}>
              <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginBottom: '1rem', fontSize: '48px', color: 'var(--accent-yellow)' }} />
              <h3 style={{ marginBottom: '0.5rem' }}>Unable to Load Data Injections</h3>
              <p style={{ color: 'var(--text-muted)', marginBottom: '1rem' }}>{error}</p>
              <button 
                onClick={() => fetchDashboard()}
                style={{
                  padding: '8px 16px',
                  backgroundColor: 'var(--accent-blue)',
                  color: 'white',
                  border: 'none',
                  borderRadius: '4px',
                  cursor: 'pointer',
                }}
              >
                Retry
              </button>
            </div>
          </CardBody>
        </Card>
      </div>
    );
  }

  return (
    <div className="dashboard-container">
      {/* Header with Overall Health */}
      <Card style={{ marginBottom: '1.5rem' }}>
        <CardHeader
          title={
            <span style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
              <FontAwesomeIcon icon={faChartLine} />
              Data Injections Pipeline
              <span style={{
                padding: '0.25rem 0.75rem',
                backgroundColor: 'var(--primary-color)',
                color: 'white',
                borderRadius: '1rem',
                fontSize: '0.75rem',
                fontWeight: 500,
              }}>
                {dateRange.label}
              </span>
              {dashboard && (
                <HealthStatusBadge status={dashboard.overall_health} size="medium" />
              )}
            </span>
          }
          action={
            <div style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
              {lastRefresh && (
                <span style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>
                  Updated: {lastRefresh.toLocaleTimeString()}
                </span>
              )}
              <button
                onClick={triggerRefresh}
                disabled={isRefreshing}
                style={{
                  display: 'flex',
                  alignItems: 'center',
                  gap: '6px',
                  padding: '6px 12px',
                  backgroundColor: 'var(--bg-secondary)',
                  color: 'var(--text-primary)',
                  border: '1px solid var(--border-color)',
                  borderRadius: '4px',
                  cursor: isRefreshing ? 'not-allowed' : 'pointer',
                  opacity: isRefreshing ? 0.6 : 1,
                }}
              >
                <FontAwesomeIcon icon={faSync} spin={isRefreshing} />
                {isRefreshing ? 'Refreshing...' : 'Refresh'}
              </button>
            </div>
          }
        />
        <CardBody>
          {/* Health Status Summary */}
          <div style={{
            display: 'flex',
            alignItems: 'center',
            gap: '12px',
            padding: '0.5rem 0',
          }}>
            {dashboard?.overall_health === 'healthy' ? (
              <FontAwesomeIcon icon={faCheckCircle} style={{ color: 'var(--accent-green)' }} />
            ) : dashboard?.overall_health === 'warning' ? (
              <FontAwesomeIcon icon={faExclamationTriangle} style={{ color: 'var(--accent-yellow)' }} />
            ) : (
              <FontAwesomeIcon icon={faExclamationTriangle} style={{ color: 'var(--accent-red)' }} />
            )}
            <span style={{ color: 'var(--text-secondary)' }}>
              {dashboard && getOverallHealthMessage(dashboard.overall_health, dashboard.health_issues || [])}
            </span>
          </div>

          {/* Health Issues List */}
          {dashboard?.health_issues && dashboard.health_issues.length > 1 && (
            <div style={{
              marginTop: '1rem',
              padding: '0.75rem',
              backgroundColor: 'var(--bg-secondary)',
              borderRadius: '8px',
            }}>
              <div style={{ 
                fontSize: '0.8rem', 
                fontWeight: 500, 
                marginBottom: '0.5rem',
                color: 'var(--text-secondary)',
              }}>
                All Issues:
              </div>
              <ul style={{ 
                margin: 0, 
                paddingLeft: '1.5rem',
                fontSize: '0.85rem',
                color: 'var(--text-muted)',
              }}>
                {dashboard.health_issues.map((issue, index) => (
                  <li key={index} style={{ marginBottom: '4px' }}>{issue}</li>
                ))}
              </ul>
            </div>
          )}

          {/* Pipeline Flow Visualization */}
          <div style={{
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            gap: '1rem',
            marginTop: '1.5rem',
            padding: '1rem',
            backgroundColor: 'var(--bg-secondary)',
            borderRadius: '8px',
          }}>
            <div style={{ textAlign: 'center' }}>
              <div style={{ 
                width: '60px', 
                height: '60px', 
                borderRadius: '50%', 
                display: 'flex', 
                alignItems: 'center', 
                justifyContent: 'center',
                backgroundColor: dashboard?.ingestion?.status === 'healthy' 
                  ? 'rgba(52, 211, 153, 0.2)' 
                  : dashboard?.ingestion?.status === 'warning'
                  ? 'rgba(251, 191, 36, 0.2)'
                  : 'rgba(148, 163, 184, 0.2)',
                marginBottom: '0.5rem',
              }}>
                <span style={{ fontSize: '1.5rem' }}>📥</span>
              </div>
              <div style={{ fontSize: '0.75rem', fontWeight: 500 }}>Ingestion</div>
              <div style={{ fontSize: '0.65rem', color: 'var(--text-muted)' }}>Azure</div>
            </div>
            
            <div style={{ color: 'var(--text-muted)', fontSize: '1.5rem' }}>→</div>
            
            <div style={{ textAlign: 'center' }}>
              <div style={{ 
                width: '60px', 
                height: '60px', 
                borderRadius: '50%', 
                display: 'flex', 
                alignItems: 'center', 
                justifyContent: 'center',
                backgroundColor: dashboard?.validation?.status === 'healthy' 
                  ? 'rgba(52, 211, 153, 0.2)' 
                  : dashboard?.validation?.status === 'warning'
                  ? 'rgba(251, 191, 36, 0.2)'
                  : 'rgba(148, 163, 184, 0.2)',
                marginBottom: '0.5rem',
              }}>
                <span style={{ fontSize: '1.5rem' }}>✓</span>
              </div>
              <div style={{ fontSize: '0.75rem', fontWeight: 500 }}>Validation</div>
              <div style={{ fontSize: '0.65rem', color: 'var(--text-muted)' }}>Snowflake</div>
            </div>
            
            <div style={{ color: 'var(--text-muted)', fontSize: '1.5rem' }}>→</div>
            
            <div style={{ textAlign: 'center' }}>
              <div style={{ 
                width: '60px', 
                height: '60px', 
                borderRadius: '50%', 
                display: 'flex', 
                alignItems: 'center', 
                justifyContent: 'center',
                backgroundColor: dashboard?.import?.status === 'healthy' 
                  ? 'rgba(52, 211, 153, 0.2)' 
                  : dashboard?.import?.status === 'warning'
                  ? 'rgba(251, 191, 36, 0.2)'
                  : 'rgba(148, 163, 184, 0.2)',
                marginBottom: '0.5rem',
              }}>
                <span style={{ fontSize: '1.5rem' }}>📤</span>
              </div>
              <div style={{ fontSize: '0.75rem', fontWeight: 500 }}>Import</div>
              <div style={{ fontSize: '0.65rem', color: 'var(--text-muted)' }}>Ongage</div>
            </div>
          </div>
        </CardBody>
      </Card>

      {/* Ingestion Panel */}
      <div style={{ marginBottom: '1.5rem' }}>
        <IngestionPanel data={dashboard?.ingestion || null} isLoading={loading} />
      </div>

      {/* Validation Panel */}
      <div style={{ marginBottom: '1.5rem' }}>
        <ValidationPanel data={dashboard?.validation || null} isLoading={loading} />
      </div>

      {/* Imports Panel */}
      <div style={{ marginBottom: '1.5rem' }}>
        <ImportsPanel data={dashboard?.import || null} isLoading={loading} />
      </div>
    </div>
  );
};

export default DataInjectionsDashboard;
