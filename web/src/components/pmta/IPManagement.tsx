import React, { useState, useCallback } from 'react';
import { useApi, useApiMutation } from '../../hooks/useApi';
import { Card, CardHeader, CardBody } from '../common/Card';
import { Loading } from '../common/Loading';
import { StatusBadge } from '../common/StatusBadge';

interface IPAddress {
  id: string;
  ip_address: string;
  hostname: string;
  acquisition_type: string;
  broker: string;
  hosting_provider: string;
  pool_id: string;
  pool_name: string;
  pmta_server_id: string;
  status: string;
  warmup_stage: string;
  warmup_day: number;
  warmup_daily_limit: number;
  reputation_score: number;
  rdns_verified: boolean;
  blacklisted_on: string[];
  total_sent: number;
  total_delivered: number;
  total_bounced: number;
  total_complained: number;
  created_at: string;
}

interface IPPool {
  id: string;
  name: string;
  description: string;
  pool_type: string;
  status: string;
  ip_count: number;
  active_count: number;
}

interface AddIPForm {
  ip_address: string;
  hostname: string;
  acquisition_type: string;
  broker: string;
  hosting_provider: string;
}

const IPManagement: React.FC = () => {
  const [showAddForm, setShowAddForm] = useState(false);
  const [addForm, setAddForm] = useState<AddIPForm>({
    ip_address: '', hostname: '', acquisition_type: 'purchased', broker: '', hosting_provider: '',
  });
  const [actionLoading, setActionLoading] = useState<string | null>(null);

  const { data: ipData, loading, refetch } = useApi<{ ips: IPAddress[]; total: number }>('/api/mailing/ips');
  const { data: poolData } = useApi<{ pools: IPPool[] }>('/api/mailing/ip-pools');
  const { mutate: addIP } = useApiMutation<AddIPForm, { id: string }>('/api/mailing/ips');

  const handleAdd = useCallback(async () => {
    if (!addForm.ip_address || !addForm.hostname) return;
    await addIP(addForm);
    setShowAddForm(false);
    setAddForm({ ip_address: '', hostname: '', acquisition_type: 'purchased', broker: '', hosting_provider: '' });
    refetch();
  }, [addForm, addIP, refetch]);

  const handleAction = useCallback(async (ipId: string, action: string) => {
    setActionLoading(ipId + action);
    try {
      await fetch(`/api/mailing/ips/${ipId}/${action}`, { method: 'POST', credentials: 'include' });
      refetch();
    } finally {
      setActionLoading(null);
    }
  }, [refetch]);

  if (loading && !ipData) return <Loading />;

  const ips = ipData?.ips ?? [];
  const pools = poolData?.pools ?? [];

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1.5rem' }}>
      {/* IP Pools summary */}
      {pools.length > 0 && (
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))', gap: '1rem' }}>
          {pools.map(pool => (
            <Card key={pool.id}>
              <CardBody>
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                  <div>
                    <div style={{ fontWeight: 600 }}>{pool.name}</div>
                    <div style={{ fontSize: '0.8rem', color: '#9ca3af' }}>{pool.pool_type}</div>
                  </div>
                  <div style={{ textAlign: 'right' }}>
                    <div style={{ fontSize: '1.5rem', fontWeight: 700 }}>{pool.active_count}</div>
                    <div style={{ fontSize: '0.75rem', color: '#9ca3af' }}>of {pool.ip_count} IPs</div>
                  </div>
                </div>
              </CardBody>
            </Card>
          ))}
        </div>
      )}

      {/* Add IP button and form */}
      <Card>
        <CardHeader>
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
            <span>Dedicated IPs ({ips.length})</span>
            <button
              onClick={() => setShowAddForm(!showAddForm)}
              style={{
                padding: '0.4rem 0.8rem', background: '#4f46e5', color: '#fff',
                border: 'none', borderRadius: '0.375rem', cursor: 'pointer', fontSize: '0.85rem',
              }}
            >
              {showAddForm ? 'Cancel' : '+ Add IP'}
            </button>
          </div>
        </CardHeader>
        <CardBody>
          {showAddForm && (
            <div style={{ padding: '1rem', marginBottom: '1rem', background: '#1f2937', borderRadius: '0.5rem', display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '0.75rem' }}>
              <input
                placeholder="IP Address (e.g. 1.2.3.4)"
                value={addForm.ip_address}
                onChange={e => setAddForm({ ...addForm, ip_address: e.target.value })}
                style={inputStyle}
              />
              <input
                placeholder="Hostname (e.g. mta1.mail.example.com)"
                value={addForm.hostname}
                onChange={e => setAddForm({ ...addForm, hostname: e.target.value })}
                style={inputStyle}
              />
              <select
                value={addForm.acquisition_type}
                onChange={e => setAddForm({ ...addForm, acquisition_type: e.target.value })}
                style={inputStyle}
              >
                <option value="purchased">Purchased</option>
                <option value="leased">Leased</option>
                <option value="provider">Cloud Provider</option>
                <option value="transferred">Transferred</option>
              </select>
              <input
                placeholder="Broker (e.g. IPXO)"
                value={addForm.broker}
                onChange={e => setAddForm({ ...addForm, broker: e.target.value })}
                style={inputStyle}
              />
              <input
                placeholder="Hosting Provider (e.g. Vultr)"
                value={addForm.hosting_provider}
                onChange={e => setAddForm({ ...addForm, hosting_provider: e.target.value })}
                style={inputStyle}
              />
              <button onClick={handleAdd} style={{ padding: '0.5rem', background: '#4f46e5', color: '#fff', border: 'none', borderRadius: '0.375rem', cursor: 'pointer' }}>
                Register IP
              </button>
            </div>
          )}

          <table style={{ width: '100%', borderCollapse: 'collapse' }}>
            <thead>
              <tr style={{ borderBottom: '1px solid #374151' }}>
                <th style={thStyle}>IP Address</th>
                <th style={thStyle}>Hostname</th>
                <th style={thStyle}>Pool</th>
                <th style={thStyle}>Status</th>
                <th style={{ ...thStyle, textAlign: 'right' }}>Sent</th>
                <th style={{ ...thStyle, textAlign: 'right' }}>Delivered</th>
                <th style={{ ...thStyle, textAlign: 'right' }}>Bounce %</th>
                <th style={{ ...thStyle, textAlign: 'center' }}>rDNS</th>
                <th style={{ ...thStyle, textAlign: 'center' }}>Actions</th>
              </tr>
            </thead>
            <tbody>
              {ips.map(ip => {
                const bounceRate = ip.total_sent > 0 ? (ip.total_bounced / ip.total_sent * 100) : 0;
                return (
                  <tr key={ip.id} style={{ borderBottom: '1px solid #1f2937' }}>
                    <td style={{ ...tdStyle, fontFamily: 'monospace', fontSize: '0.85rem' }}>{ip.ip_address}</td>
                    <td style={{ ...tdStyle, fontSize: '0.85rem' }}>{ip.hostname}</td>
                    <td style={tdStyle}>{ip.pool_name || '—'}</td>
                    <td style={tdStyle}>
                      <StatusBadge status={ip.status === 'active' ? 'healthy' : ip.status === 'warmup' ? 'warning' : ip.status === 'blacklisted' ? 'critical' : 'neutral'} label={ip.status} />
                    </td>
                    <td style={{ ...tdStyle, textAlign: 'right' }}>{ip.total_sent.toLocaleString()}</td>
                    <td style={{ ...tdStyle, textAlign: 'right' }}>{ip.total_delivered.toLocaleString()}</td>
                    <td style={{ ...tdStyle, textAlign: 'right', color: bounceRate > 5 ? '#ef4444' : bounceRate > 2 ? '#f59e0b' : '#10b981' }}>
                      {bounceRate.toFixed(2)}%
                    </td>
                    <td style={{ ...tdStyle, textAlign: 'center' }}>
                      {ip.rdns_verified ? (
                        <span style={{ color: '#10b981' }}>&#10003;</span>
                      ) : (
                        <span style={{ color: '#ef4444' }}>&#10007;</span>
                      )}
                    </td>
                    <td style={{ ...tdStyle, textAlign: 'center' }}>
                      <div style={{ display: 'flex', gap: '0.25rem', justifyContent: 'center' }}>
                        <button
                          onClick={() => handleAction(ip.id, 'check-dns')}
                          disabled={actionLoading === ip.id + 'check-dns'}
                          style={actionBtnStyle}
                          title="Check DNS"
                        >
                          DNS
                        </button>
                        <button
                          onClick={() => handleAction(ip.id, 'check-blacklist')}
                          disabled={actionLoading === ip.id + 'check-blacklist'}
                          style={actionBtnStyle}
                          title="Check Blacklists"
                        >
                          BL
                        </button>
                        {ip.status === 'pending' && (
                          <button
                            onClick={() => handleAction(ip.id, 'warmup/start')}
                            disabled={actionLoading === ip.id + 'warmup/start'}
                            style={{ ...actionBtnStyle, background: '#065f46' }}
                            title="Start Warmup"
                          >
                            Warm
                          </button>
                        )}
                      </div>
                    </td>
                  </tr>
                );
              })}
              {ips.length === 0 && (
                <tr>
                  <td colSpan={9} style={{ padding: '2rem', textAlign: 'center', color: '#9ca3af' }}>
                    No IPs registered. Add your first dedicated IP to get started.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </CardBody>
      </Card>
    </div>
  );
};

const thStyle: React.CSSProperties = {
  textAlign: 'left', padding: '0.5rem', color: '#9ca3af', fontSize: '0.8rem', fontWeight: 500,
};

const tdStyle: React.CSSProperties = {
  padding: '0.5rem', fontSize: '0.9rem',
};

const inputStyle: React.CSSProperties = {
  padding: '0.5rem', background: '#111827', border: '1px solid #374151',
  borderRadius: '0.375rem', color: '#f3f4f6', fontSize: '0.85rem',
};

const actionBtnStyle: React.CSSProperties = {
  padding: '0.2rem 0.5rem', background: '#374151', color: '#d1d5db',
  border: 'none', borderRadius: '0.25rem', cursor: 'pointer', fontSize: '0.75rem',
};

export default IPManagement;
