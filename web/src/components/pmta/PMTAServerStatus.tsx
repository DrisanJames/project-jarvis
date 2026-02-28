import React, { useCallback } from 'react';
import { useApi } from '../../hooks/useApi';
import { Card, CardHeader, CardBody } from '../common/Card';
import { Loading } from '../common/Loading';
import { StatusBadge } from '../common/StatusBadge';

interface PMTAServer {
  id: string;
  name: string;
  host: string;
  smtp_port: number;
  mgmt_port: number;
  provider: string;
  status: string;
  health_status: string;
  ip_count: number;
  last_health_check?: string;
  created_at: string;
}

interface ServerStatusData {
  version: string;
  uptime: string;
  total_queued: number;
  connections_in: number;
  connections_out: number;
  checked_at: string;
}

interface Props {
  serverId: string;
}

const PMTAServerStatus: React.FC<Props> = ({ serverId }) => {
  const { data: status, loading, error, refetch } = useApi<ServerStatusData>(
    `/api/mailing/pmta-servers/${serverId}/status`,
    { pollingInterval: 30000 }
  );

  const handleSync = useCallback(async () => {
    await fetch(`/api/mailing/pmta-servers/${serverId}/sync`, {
      method: 'POST', credentials: 'include',
    });
    refetch();
  }, [serverId, refetch]);

  if (loading && !status) return <Loading />;

  return (
    <Card>
      <CardHeader>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
          <span>PMTA Server</span>
          <div style={{ display: 'flex', gap: '0.5rem' }}>
            <button
              onClick={handleSync}
              style={{ padding: '0.3rem 0.6rem', background: '#065f46', color: '#6ee7b7', border: 'none', borderRadius: '0.25rem', cursor: 'pointer', fontSize: '0.8rem' }}
            >
              Sync Config
            </button>
            <button
              onClick={() => refetch()}
              style={{ padding: '0.3rem 0.6rem', background: '#374151', color: '#d1d5db', border: 'none', borderRadius: '0.25rem', cursor: 'pointer', fontSize: '0.8rem' }}
            >
              Refresh
            </button>
          </div>
        </div>
      </CardHeader>
      <CardBody>
        {error ? (
          <div style={{ padding: '1rem', textAlign: 'center', color: '#ef4444' }}>
            <p>Cannot reach PMTA server</p>
            <p style={{ fontSize: '0.8rem', color: '#9ca3af', marginTop: '0.25rem' }}>{error}</p>
          </div>
        ) : status ? (
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(150px, 1fr))', gap: '1rem' }}>
            <div>
              <div style={{ color: '#9ca3af', fontSize: '0.75rem' }}>Version</div>
              <div style={{ fontWeight: 600, fontSize: '1rem' }}>{status.version}</div>
            </div>
            <div>
              <div style={{ color: '#9ca3af', fontSize: '0.75rem' }}>Uptime</div>
              <div style={{ fontWeight: 600, fontSize: '1rem' }}>{status.uptime}</div>
            </div>
            <div>
              <div style={{ color: '#9ca3af', fontSize: '0.75rem' }}>Queued</div>
              <div style={{ fontWeight: 600, fontSize: '1rem' }}>{status.total_queued.toLocaleString()}</div>
            </div>
            <div>
              <div style={{ color: '#9ca3af', fontSize: '0.75rem' }}>Conn In</div>
              <div style={{ fontWeight: 600, fontSize: '1rem' }}>{status.connections_in}</div>
            </div>
            <div>
              <div style={{ color: '#9ca3af', fontSize: '0.75rem' }}>Conn Out</div>
              <div style={{ fontWeight: 600, fontSize: '1rem' }}>{status.connections_out}</div>
            </div>
          </div>
        ) : null}
      </CardBody>
    </Card>
  );
};

export const PMTAServerList: React.FC = () => {
  const { data, loading, error } = useApi<{ servers: PMTAServer[] }>('/api/mailing/pmta-servers');

  if (loading && !data) return <Loading />;
  if (error) return <div>Error: {error}</div>;

  const servers = data?.servers ?? [];

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      {servers.map(srv => (
        <Card key={srv.id}>
          <CardBody>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '0.5rem' }}>
              <div>
                <div style={{ fontWeight: 600 }}>{srv.name}</div>
                <div style={{ fontSize: '0.8rem', color: '#9ca3af', fontFamily: 'monospace' }}>
                  {srv.host}:{srv.smtp_port} &middot; mgmt :{srv.mgmt_port}
                </div>
              </div>
              <div style={{ display: 'flex', gap: '0.5rem', alignItems: 'center' }}>
                <StatusBadge
                  status={srv.health_status === 'healthy' ? 'healthy' : srv.health_status === 'unreachable' ? 'critical' : 'neutral'}
                  label={srv.health_status}
                />
                <span style={{ fontSize: '0.75rem', color: '#9ca3af' }}>{srv.ip_count} IPs</span>
              </div>
            </div>
            {srv.provider && (
              <div style={{ fontSize: '0.75rem', color: '#6b7280' }}>Provider: {srv.provider}</div>
            )}
          </CardBody>
        </Card>
      ))}
      {servers.length === 0 && (
        <Card>
          <CardBody>
            <p style={{ color: '#9ca3af', textAlign: 'center', padding: '1.5rem' }}>
              No PMTA servers registered.
            </p>
          </CardBody>
        </Card>
      )}
    </div>
  );
};

export default PMTAServerStatus;
