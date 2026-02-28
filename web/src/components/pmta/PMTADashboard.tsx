import React, { useState } from 'react';
import { useApi } from '../../hooks/useApi';
import { Card, CardHeader, CardBody } from '../common/Card';
import { Loading } from '../common/Loading';
import { StatusBadge } from '../common/StatusBadge';
import { MetricCard } from '../common/MetricCard';
import IPManagement from './IPManagement';
import IPWarmupDashboard from './IPWarmupDashboard';
import IPXODashboard from './IPXODashboard';
import VultrServers from './VultrServers';
import EngineDashboard from './EngineDashboard';

interface DashboardData {
  server: {
    version: string;
    uptime: string;
    total_queued: number;
    connections_in: number;
    connections_out: number;
  } | null;
  queues: Array<{
    domain: string;
    vmta: string;
    queued: number;
    errors: number;
    expired: number;
  }>;
  vmtas: Array<{
    name: string;
    source_ip: string;
    hostname: string;
    connections_out: number;
    queued: number;
    delivered: number;
    bounced: number;
    delivery_rate: number;
  }>;
  domains: Array<{
    domain: string;
    queued: number;
    delivered: number;
    bounced: number;
    delivery_rate: number;
  }>;
  ip_health: Record<string, {
    ip: string;
    hostname: string;
    total_sent: number;
    total_delivered: number;
    total_bounced: number;
    total_complained: number;
    delivery_rate: number;
    bounce_rate: number;
    complaint_rate: number;
    status: string;
  }>;
  summary: {
    total_queued: number;
    total_delivered: number;
    total_bounced: number;
    total_complained: number;
    overall_delivery_rate: number;
    overall_bounce_rate: number;
    active_ips: number;
    healthy_ips: number;
    warning_ips: number;
    critical_ips: number;
  };
}

type Tab = 'overview' | 'ips' | 'warmup' | 'queues' | 'domains' | 'ipxo' | 'servers' | 'engine';

const PMTADashboard: React.FC = () => {
  const [activeTab, setActiveTab] = useState<Tab>('overview');
  const { data, loading, error } = useApi<DashboardData>('/api/mailing/pmta/dashboard', {
    pollingInterval: 60000,
  });

  const tabs: { key: Tab; label: string }[] = [
    { key: 'overview', label: 'Overview' },
    { key: 'ips', label: 'IP Management' },
    { key: 'warmup', label: 'IP Warmup' },
    { key: 'queues', label: 'Queues' },
    { key: 'domains', label: 'Domains' },
    { key: 'ipxo', label: 'IPXO' },
    { key: 'servers', label: 'Servers' },
    { key: 'engine', label: 'Engine' },
  ];

  if (loading && !data) return <Loading />;
  if (error) return <div className="error-display"><p>Error loading PMTA dashboard: {error}</p></div>;

  const summary = data?.summary;

  return (
    <div style={{ padding: '1.5rem' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: '1rem', marginBottom: '1.5rem' }}>
        <h2 style={{ margin: 0, fontSize: '1.5rem', fontWeight: 600 }}>PowerMTA Dashboard</h2>
        {data?.server && (
          <span style={{ fontSize: '0.8rem', color: '#9ca3af', background: '#1f2937', padding: '0.25rem 0.75rem', borderRadius: '9999px' }}>
            v{data.server.version} &middot; Up {data.server.uptime}
          </span>
        )}
      </div>

      {/* Tab navigation */}
      <div style={{ display: 'flex', gap: '0.25rem', marginBottom: '1.5rem', borderBottom: '1px solid #374151' }}>
        {tabs.map(tab => (
          <button
            key={tab.key}
            onClick={() => setActiveTab(tab.key)}
            style={{
              padding: '0.5rem 1rem',
              background: activeTab === tab.key ? '#4f46e5' : 'transparent',
              color: activeTab === tab.key ? '#fff' : '#9ca3af',
              border: 'none',
              borderRadius: '0.375rem 0.375rem 0 0',
              cursor: 'pointer',
              fontWeight: activeTab === tab.key ? 600 : 400,
            }}
          >
            {tab.label}
          </button>
        ))}
      </div>

      {activeTab === 'overview' && (
        <>
          {/* Summary metrics */}
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: '1rem', marginBottom: '1.5rem' }}>
            <MetricCard label="Total Queued" value={summary?.total_queued?.toLocaleString() ?? '0'} />
            <MetricCard label="Delivered" value={summary?.total_delivered?.toLocaleString() ?? '0'} />
            <MetricCard label="Delivery Rate" value={`${(summary?.overall_delivery_rate ?? 0).toFixed(1)}%`} status={
              (summary?.overall_delivery_rate ?? 100) >= 95 ? 'healthy' : (summary?.overall_delivery_rate ?? 100) >= 90 ? 'warning' : 'critical'
            } />
            <MetricCard label="Bounce Rate" value={`${(summary?.overall_bounce_rate ?? 0).toFixed(2)}%`} status={
              (summary?.overall_bounce_rate ?? 0) <= 2 ? 'healthy' : (summary?.overall_bounce_rate ?? 0) <= 5 ? 'warning' : 'critical'
            } />
            <MetricCard label="Active IPs" value={String(summary?.active_ips ?? 0)} />
            <MetricCard label="IP Health" value={`${summary?.healthy_ips ?? 0} / ${summary?.active_ips ?? 0}`} status={
              (summary?.critical_ips ?? 0) > 0 ? 'critical' : (summary?.warning_ips ?? 0) > 0 ? 'warning' : 'healthy'
            } />
          </div>

          {/* Server status */}
          {data?.server && (
            <Card>
              <CardHeader>Server Status</CardHeader>
              <CardBody>
                <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(150px, 1fr))', gap: '1rem' }}>
                  <div><span style={{ color: '#9ca3af', fontSize: '0.8rem' }}>Connections In</span><br/><strong>{data.server.connections_in}</strong></div>
                  <div><span style={{ color: '#9ca3af', fontSize: '0.8rem' }}>Connections Out</span><br/><strong>{data.server.connections_out}</strong></div>
                  <div><span style={{ color: '#9ca3af', fontSize: '0.8rem' }}>Total Queued</span><br/><strong>{data.server.total_queued.toLocaleString()}</strong></div>
                </div>
              </CardBody>
            </Card>
          )}

          {/* VMTA (IP) performance table */}
          {data?.vmtas && data.vmtas.length > 0 && (
            <Card>
              <CardHeader>Virtual MTA Performance</CardHeader>
              <CardBody>
                <table style={{ width: '100%', borderCollapse: 'collapse' }}>
                  <thead>
                    <tr style={{ borderBottom: '1px solid #374151' }}>
                      <th style={{ textAlign: 'left', padding: '0.5rem', color: '#9ca3af', fontSize: '0.8rem' }}>VMTA</th>
                      <th style={{ textAlign: 'left', padding: '0.5rem', color: '#9ca3af', fontSize: '0.8rem' }}>Source IP</th>
                      <th style={{ textAlign: 'right', padding: '0.5rem', color: '#9ca3af', fontSize: '0.8rem' }}>Delivered</th>
                      <th style={{ textAlign: 'right', padding: '0.5rem', color: '#9ca3af', fontSize: '0.8rem' }}>Bounced</th>
                      <th style={{ textAlign: 'right', padding: '0.5rem', color: '#9ca3af', fontSize: '0.8rem' }}>Rate</th>
                      <th style={{ textAlign: 'right', padding: '0.5rem', color: '#9ca3af', fontSize: '0.8rem' }}>Queued</th>
                      <th style={{ textAlign: 'center', padding: '0.5rem', color: '#9ca3af', fontSize: '0.8rem' }}>Status</th>
                    </tr>
                  </thead>
                  <tbody>
                    {data.vmtas.map((v, i) => (
                      <tr key={i} style={{ borderBottom: '1px solid #1f2937' }}>
                        <td style={{ padding: '0.5rem', fontFamily: 'monospace', fontSize: '0.85rem' }}>{v.name}</td>
                        <td style={{ padding: '0.5rem', fontFamily: 'monospace', fontSize: '0.85rem' }}>{v.source_ip}</td>
                        <td style={{ padding: '0.5rem', textAlign: 'right' }}>{v.delivered.toLocaleString()}</td>
                        <td style={{ padding: '0.5rem', textAlign: 'right' }}>{v.bounced.toLocaleString()}</td>
                        <td style={{ padding: '0.5rem', textAlign: 'right', color: v.delivery_rate >= 95 ? '#10b981' : v.delivery_rate >= 90 ? '#f59e0b' : '#ef4444' }}>
                          {v.delivery_rate.toFixed(1)}%
                        </td>
                        <td style={{ padding: '0.5rem', textAlign: 'right' }}>{v.queued.toLocaleString()}</td>
                        <td style={{ padding: '0.5rem', textAlign: 'center' }}>
                          <StatusBadge status={v.delivery_rate >= 95 ? 'healthy' : v.delivery_rate >= 90 ? 'warning' : 'critical'} />
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </CardBody>
            </Card>
          )}

          {(!data?.vmtas || data.vmtas.length === 0) && !data?.server && (
            <Card>
              <CardBody>
                <div style={{ textAlign: 'center', padding: '2rem', color: '#9ca3af' }}>
                  <p style={{ fontSize: '1.1rem', marginBottom: '0.5rem' }}>No PMTA server connected</p>
                  <p style={{ fontSize: '0.85rem' }}>Register a PMTA server and add IPs to get started.</p>
                </div>
              </CardBody>
            </Card>
          )}
        </>
      )}

      {activeTab === 'ips' && <IPManagement />}
      {activeTab === 'warmup' && <IPWarmupDashboard />}

      {activeTab === 'queues' && (
        <Card>
          <CardHeader>Mail Queues</CardHeader>
          <CardBody>
            {data?.queues && data.queues.length > 0 ? (
              <table style={{ width: '100%', borderCollapse: 'collapse' }}>
                <thead>
                  <tr style={{ borderBottom: '1px solid #374151' }}>
                    <th style={{ textAlign: 'left', padding: '0.5rem', color: '#9ca3af', fontSize: '0.8rem' }}>Domain</th>
                    <th style={{ textAlign: 'left', padding: '0.5rem', color: '#9ca3af', fontSize: '0.8rem' }}>VMTA</th>
                    <th style={{ textAlign: 'right', padding: '0.5rem', color: '#9ca3af', fontSize: '0.8rem' }}>Queued</th>
                    <th style={{ textAlign: 'right', padding: '0.5rem', color: '#9ca3af', fontSize: '0.8rem' }}>Errors</th>
                    <th style={{ textAlign: 'right', padding: '0.5rem', color: '#9ca3af', fontSize: '0.8rem' }}>Expired</th>
                  </tr>
                </thead>
                <tbody>
                  {data.queues.map((q, i) => (
                    <tr key={i} style={{ borderBottom: '1px solid #1f2937' }}>
                      <td style={{ padding: '0.5rem' }}>{q.domain}</td>
                      <td style={{ padding: '0.5rem', fontFamily: 'monospace', fontSize: '0.85rem' }}>{q.vmta}</td>
                      <td style={{ padding: '0.5rem', textAlign: 'right' }}>{q.queued.toLocaleString()}</td>
                      <td style={{ padding: '0.5rem', textAlign: 'right', color: q.errors > 0 ? '#ef4444' : undefined }}>{q.errors}</td>
                      <td style={{ padding: '0.5rem', textAlign: 'right', color: q.expired > 0 ? '#f59e0b' : undefined }}>{q.expired}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ) : (
              <p style={{ color: '#9ca3af', textAlign: 'center', padding: '1rem' }}>No queued messages</p>
            )}
          </CardBody>
        </Card>
      )}

      {activeTab === 'domains' && (
        <Card>
          <CardHeader>Destination Domain Performance</CardHeader>
          <CardBody>
            {data?.domains && data.domains.length > 0 ? (
              <table style={{ width: '100%', borderCollapse: 'collapse' }}>
                <thead>
                  <tr style={{ borderBottom: '1px solid #374151' }}>
                    <th style={{ textAlign: 'left', padding: '0.5rem', color: '#9ca3af', fontSize: '0.8rem' }}>Domain</th>
                    <th style={{ textAlign: 'right', padding: '0.5rem', color: '#9ca3af', fontSize: '0.8rem' }}>Delivered</th>
                    <th style={{ textAlign: 'right', padding: '0.5rem', color: '#9ca3af', fontSize: '0.8rem' }}>Bounced</th>
                    <th style={{ textAlign: 'right', padding: '0.5rem', color: '#9ca3af', fontSize: '0.8rem' }}>Delivery Rate</th>
                    <th style={{ textAlign: 'right', padding: '0.5rem', color: '#9ca3af', fontSize: '0.8rem' }}>Queued</th>
                  </tr>
                </thead>
                <tbody>
                  {data.domains.map((d, i) => (
                    <tr key={i} style={{ borderBottom: '1px solid #1f2937' }}>
                      <td style={{ padding: '0.5rem' }}>{d.domain}</td>
                      <td style={{ padding: '0.5rem', textAlign: 'right' }}>{d.delivered.toLocaleString()}</td>
                      <td style={{ padding: '0.5rem', textAlign: 'right' }}>{d.bounced.toLocaleString()}</td>
                      <td style={{ padding: '0.5rem', textAlign: 'right', color: d.delivery_rate >= 95 ? '#10b981' : d.delivery_rate >= 90 ? '#f59e0b' : '#ef4444' }}>
                        {d.delivery_rate.toFixed(1)}%
                      </td>
                      <td style={{ padding: '0.5rem', textAlign: 'right' }}>{d.queued.toLocaleString()}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ) : (
              <p style={{ color: '#9ca3af', textAlign: 'center', padding: '1rem' }}>No domain data available</p>
            )}
          </CardBody>
        </Card>
      )}

      {activeTab === 'ipxo' && <IPXODashboard />}
      {activeTab === 'servers' && <VultrServers />}
      {activeTab === 'engine' && <EngineDashboard />}
    </div>
  );
};

export default PMTADashboard;
