import React, { useState, useCallback } from 'react';
import { useApi, useApiMutation } from '../../hooks/useApi';
import { Card, CardHeader, CardBody } from '../common/Card';
import { Loading } from '../common/Loading';
import { StatusBadge } from '../common/StatusBadge';

interface IPPool {
  id: string;
  name: string;
  description: string;
  pool_type: string;
  status: string;
  ip_count: number;
  active_count: number;
  created_at: string;
}

interface IPAddress {
  id: string;
  ip_address: string;
  hostname: string;
  pool_id: string;
  pool_name: string;
  status: string;
}

const IPPoolManager: React.FC = () => {
  const [showCreate, setShowCreate] = useState(false);
  const [newPool, setNewPool] = useState({ name: '', description: '', pool_type: 'dedicated' });
  const { data: poolData, loading, refetch: refetchPools } = useApi<{ pools: IPPool[] }>('/api/mailing/ip-pools');
  const { data: ipData, refetch: refetchIPs } = useApi<{ ips: IPAddress[] }>('/api/mailing/ips');
  const { mutate: createPool } = useApiMutation<typeof newPool, { id: string }>('/api/mailing/ip-pools');

  const handleCreate = useCallback(async () => {
    if (!newPool.name) return;
    await createPool(newPool);
    setShowCreate(false);
    setNewPool({ name: '', description: '', pool_type: 'dedicated' });
    refetchPools();
  }, [newPool, createPool, refetchPools]);

  const handleAssign = useCallback(async (poolId: string, ipId: string) => {
    await fetch(`/api/mailing/ip-pools/${poolId}/add-ip`, {
      method: 'POST', credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ip_id: ipId }),
    });
    refetchPools();
    refetchIPs();
  }, [refetchPools, refetchIPs]);

  const handleUnassign = useCallback(async (poolId: string, ipId: string) => {
    await fetch(`/api/mailing/ip-pools/${poolId}/remove-ip`, {
      method: 'POST', credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ip_id: ipId }),
    });
    refetchPools();
    refetchIPs();
  }, [refetchPools, refetchIPs]);

  if (loading && !poolData) return <Loading />;

  const pools = poolData?.pools ?? [];
  const allIPs = ipData?.ips ?? [];
  const unassigned = allIPs.filter(ip => !ip.pool_id);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1.5rem' }}>
      <Card>
        <CardHeader>
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
            <span>IP Pools ({pools.length})</span>
            <button
              onClick={() => setShowCreate(!showCreate)}
              style={{ padding: '0.4rem 0.8rem', background: '#4f46e5', color: '#fff', border: 'none', borderRadius: '0.375rem', cursor: 'pointer', fontSize: '0.85rem' }}
            >
              {showCreate ? 'Cancel' : '+ Create Pool'}
            </button>
          </div>
        </CardHeader>
        <CardBody>
          {showCreate && (
            <div style={{ padding: '1rem', marginBottom: '1rem', background: '#1f2937', borderRadius: '0.5rem', display: 'flex', gap: '0.5rem', alignItems: 'center' }}>
              <input
                placeholder="Pool name"
                value={newPool.name}
                onChange={e => setNewPool({ ...newPool, name: e.target.value })}
                style={inputStyle}
              />
              <input
                placeholder="Description"
                value={newPool.description}
                onChange={e => setNewPool({ ...newPool, description: e.target.value })}
                style={{ ...inputStyle, flex: 1 }}
              />
              <select
                value={newPool.pool_type}
                onChange={e => setNewPool({ ...newPool, pool_type: e.target.value })}
                style={inputStyle}
              >
                <option value="dedicated">Dedicated</option>
                <option value="shared">Shared</option>
                <option value="warmup">Warmup</option>
              </select>
              <button onClick={handleCreate} style={{ padding: '0.4rem 0.8rem', background: '#4f46e5', color: '#fff', border: 'none', borderRadius: '0.375rem', cursor: 'pointer', fontSize: '0.85rem' }}>
                Create
              </button>
            </div>
          )}

          {pools.length > 0 ? (
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(300px, 1fr))', gap: '1rem' }}>
              {pools.map(pool => {
                const poolIPs = allIPs.filter(ip => ip.pool_id === pool.id);
                return (
                  <div key={pool.id} style={{ padding: '1rem', background: '#1f2937', borderRadius: '0.5rem', border: '1px solid #374151' }}>
                    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '0.5rem' }}>
                      <div>
                        <div style={{ fontWeight: 600 }}>{pool.name}</div>
                        <div style={{ fontSize: '0.75rem', color: '#9ca3af' }}>{pool.pool_type} &middot; {pool.description || 'No description'}</div>
                      </div>
                      <StatusBadge status={pool.status === 'active' ? 'healthy' : 'neutral'} label={pool.status} />
                    </div>

                    {/* IPs in pool */}
                    <div style={{ fontSize: '0.85rem', marginTop: '0.5rem' }}>
                      {poolIPs.length > 0 ? poolIPs.map(ip => (
                        <div key={ip.id} style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '0.25rem 0', borderBottom: '1px solid #374151' }}>
                          <span style={{ fontFamily: 'monospace', fontSize: '0.8rem' }}>{ip.ip_address}</span>
                          <button
                            onClick={() => handleUnassign(pool.id, ip.id)}
                            style={{ padding: '0.1rem 0.3rem', background: '#7f1d1d', color: '#fca5a5', border: 'none', borderRadius: '0.15rem', cursor: 'pointer', fontSize: '0.7rem' }}
                          >
                            Remove
                          </button>
                        </div>
                      )) : (
                        <div style={{ color: '#6b7280', fontSize: '0.8rem', padding: '0.25rem 0' }}>No IPs assigned</div>
                      )}
                    </div>

                    {/* Assign dropdown */}
                    {unassigned.length > 0 && (
                      <select
                        onChange={e => { if (e.target.value) handleAssign(pool.id, e.target.value); e.target.value = ''; }}
                        style={{ ...inputStyle, width: '100%', marginTop: '0.5rem', fontSize: '0.8rem' }}
                        defaultValue=""
                      >
                        <option value="">+ Assign IP...</option>
                        {unassigned.map(ip => (
                          <option key={ip.id} value={ip.id}>{ip.ip_address} ({ip.hostname})</option>
                        ))}
                      </select>
                    )}
                  </div>
                );
              })}
            </div>
          ) : (
            <p style={{ color: '#9ca3af', textAlign: 'center', padding: '1.5rem' }}>
              No IP pools created. Create a pool to organize your IPs for rotation.
            </p>
          )}
        </CardBody>
      </Card>
    </div>
  );
};

const inputStyle: React.CSSProperties = {
  padding: '0.4rem', background: '#111827', border: '1px solid #374151',
  borderRadius: '0.375rem', color: '#f3f4f6', fontSize: '0.85rem',
};

export default IPPoolManager;
