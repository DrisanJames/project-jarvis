import React, { useState, useCallback } from 'react';
import { useApi } from '../../hooks/useApi';
import { Card, CardHeader, CardBody } from '../common/Card';
import { Loading } from '../common/Loading';
import { StatusBadge } from '../common/StatusBadge';

interface VultrStatusData {
  configured: boolean;
  message?: string;
  server_count?: number;
  servers?: Array<{
    id: string;
    label: string;
    hostname: string;
    main_ip: string;
    region: string;
    plan: string;
    os: string;
    status: string;
    power_status: string;
    ram: string;
    disk: string;
    cpu_count: number;
    date_created: string;
  }>;
  bgp?: {
    enabled: boolean;
    asn: number;
    password: string;
    networks: Array<{ v4_subnet: string; v4_subnet_len: number }>;
  };
  bgp_error?: string;
}

interface Plan {
  id: string;
  cpu_count: number;
  cpu_model: string;
  cpu_threads: number;
  ram: number;
  disk: number;
  disk_count: number;
  bandwidth: number;
  monthly_cost: number;
  locations: string[];
}

interface Region {
  id: string;
  city: string;
  country: string;
  continent: string;
}

type View = 'servers' | 'provision' | 'bgp';

const VultrServers: React.FC = () => {
  const [view, setView] = useState<View>('servers');
  const [provisionForm, setProvisionForm] = useState({
    region: 'ewr', plan: '', label: 'pmta-node-1',
    hostname: 'pmta1.mail.ignitemailing.com',
    subnet_block: '144.225.178.0/24', install_pmta: true,
  });
  const [provisioning, setProvisioning] = useState(false);
  const [provisionResult, setProvisionResult] = useState<any>(null);
  const [loaText, setLoaText] = useState('');
  const [actioningId, setActioningId] = useState<string | null>(null);

  const { data, loading, error, refetch } = useApi<VultrStatusData>('/api/mailing/vultr/status', {
    pollingInterval: 30000,
  });
  const { data: plansData } = useApi<{ plans: Plan[] }>('/api/mailing/vultr/plans');
  const { data: regionsData } = useApi<{ regions: Region[] }>('/api/mailing/vultr/regions');

  const handleProvision = useCallback(async () => {
    setProvisioning(true);
    setProvisionResult(null);
    try {
      const resp = await fetch('/api/mailing/vultr/servers', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify(provisionForm),
      });
      const result = await resp.json();
      setProvisionResult(result);
      if (!result.error) refetch();
    } finally {
      setProvisioning(false);
    }
  }, [provisionForm, refetch]);

  const handleServerAction = useCallback(async (id: string, action: 'reboot' | 'halt') => {
    setActioningId(id);
    await fetch(`/api/mailing/vultr/servers/${id}/${action}`, {
      method: 'POST', credentials: 'include',
    });
    setTimeout(() => { refetch(); setActioningId(null); }, 3000);
  }, [refetch]);

  const handleGenerateLOA = useCallback(async () => {
    const resp = await fetch('/api/mailing/vultr/generate-loa', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      credentials: 'include',
      body: JSON.stringify({
        company_name: 'RDJ CO LLC.',
        subnet: '144.225.178.0/24',
        contact_name: 'Drisan James',
        email: 'drisanjames@gmail.com',
        phone: '',
      }),
    });
    const result = await resp.json();
    setLoaText(result.loa);
  }, []);

  if (loading && !data) return <Loading />;
  if (error) return <div style={{ color: '#ef4444', padding: '1rem' }}>Error: {error}</div>;

  if (data && !data.configured) {
    return (
      <Card>
        <CardBody>
          <div style={{ textAlign: 'center', padding: '3rem', color: '#9ca3af' }}>
            <div style={{ fontSize: '1.5rem', marginBottom: '0.5rem' }}>Vultr Not Configured</div>
            <p>Set <code>VULTR_API_KEY</code> to enable server provisioning.</p>
            <p style={{ fontSize: '0.85rem' }}>Get your API key from <a href="https://my.vultr.com/settings/#settingsapi" target="_blank" rel="noopener noreferrer" style={{ color: '#818cf8' }}>Vultr Settings → API</a></p>
          </div>
        </CardBody>
      </Card>
    );
  }

  const views: { key: View; label: string }[] = [
    { key: 'servers', label: `Servers (${data?.server_count ?? 0})` },
    { key: 'provision', label: 'Provision New' },
    { key: 'bgp', label: 'BGP Status' },
  ];

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      <div style={{ display: 'flex', gap: '0.25rem', borderBottom: '1px solid #374151' }}>
        {views.map(v => (
          <button key={v.key} onClick={() => setView(v.key)} style={{
            padding: '0.4rem 0.8rem', border: 'none', cursor: 'pointer', fontSize: '0.85rem',
            background: view === v.key ? '#6366f1' : 'transparent',
            color: view === v.key ? '#fff' : '#9ca3af',
            borderRadius: '0.375rem 0.375rem 0 0',
            fontWeight: view === v.key ? 600 : 400,
          }}>
            {v.label}
          </button>
        ))}
      </div>

      {view === 'servers' && (
        <Card>
          <CardHeader title="Bare Metal Servers" />
          <CardBody>
            {data?.servers && data.servers.length > 0 ? (
              <table style={{ width: '100%', borderCollapse: 'collapse' }}>
                <thead>
                  <tr style={{ borderBottom: '1px solid #374151' }}>
                    <th style={thStyle}>Label</th>
                    <th style={thStyle}>IP</th>
                    <th style={thStyle}>Region</th>
                    <th style={thStyle}>OS</th>
                    <th style={thStyle}>CPU</th>
                    <th style={thStyle}>RAM</th>
                    <th style={thStyle}>Status</th>
                    <th style={thStyle}>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {data.servers.map(srv => (
                    <tr key={srv.id} style={{ borderBottom: '1px solid #1f2937' }}>
                      <td style={{ ...tdStyle, fontWeight: 600 }}>{srv.label}</td>
                      <td style={{ ...tdStyle, fontFamily: 'monospace' }}>{srv.main_ip}</td>
                      <td style={tdStyle}>{srv.region}</td>
                      <td style={tdStyle}>{srv.os}</td>
                      <td style={tdStyle}>{srv.cpu_count} cores</td>
                      <td style={tdStyle}>{srv.ram}</td>
                      <td style={tdStyle}>
                        <StatusBadge
                          status={srv.power_status === 'running' ? 'healthy' : srv.status === 'active' ? 'warning' : 'neutral'}
                          label={srv.power_status || srv.status}
                        />
                      </td>
                      <td style={tdStyle}>
                        <div style={{ display: 'flex', gap: '0.25rem' }}>
                          <button onClick={() => handleServerAction(srv.id, 'reboot')}
                            disabled={actioningId === srv.id}
                            style={{ ...btnSmall, background: '#1e40af' }}>
                            Reboot
                          </button>
                          <button onClick={() => handleServerAction(srv.id, 'halt')}
                            disabled={actioningId === srv.id}
                            style={{ ...btnSmall, background: '#7f1d1d' }}>
                            Halt
                          </button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ) : (
              <div style={{ textAlign: 'center', padding: '2rem', color: '#9ca3af' }}>
                <p>No bare metal servers provisioned yet.</p>
                <button onClick={() => setView('provision')} style={btnPrimary}>
                  Provision Your First PMTA Server
                </button>
              </div>
            )}
          </CardBody>
        </Card>
      )}

      {view === 'provision' && (
        <Card>
          <CardHeader title="Provision PMTA Bare Metal Server" />
          <CardBody>
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '1rem', maxWidth: '700px' }}>
              <div>
                <label style={labelStyle}>Region</label>
                <select value={provisionForm.region}
                  onChange={e => setProvisionForm({ ...provisionForm, region: e.target.value })}
                  style={inputStyle}>
                  {regionsData?.regions?.map(r => (
                    <option key={r.id} value={r.id}>{r.city}, {r.country} ({r.id})</option>
                  )) ?? <option value="ewr">New Jersey (ewr)</option>}
                </select>
              </div>

              <div>
                <label style={labelStyle}>Plan</label>
                <select value={provisionForm.plan}
                  onChange={e => setProvisionForm({ ...provisionForm, plan: e.target.value })}
                  style={inputStyle}>
                  <option value="">Select plan...</option>
                  {plansData?.plans?.map(p => (
                    <option key={p.id} value={p.id}>
                      {p.cpu_model} ({p.cpu_count}c/{p.ram / 1024}GB) — ${p.monthly_cost}/mo
                    </option>
                  ))}
                </select>
              </div>

              <div>
                <label style={labelStyle}>Label</label>
                <input value={provisionForm.label}
                  onChange={e => setProvisionForm({ ...provisionForm, label: e.target.value })}
                  style={inputStyle} />
              </div>

              <div>
                <label style={labelStyle}>Hostname</label>
                <input value={provisionForm.hostname}
                  onChange={e => setProvisionForm({ ...provisionForm, hostname: e.target.value })}
                  style={inputStyle} />
              </div>

              <div style={{ gridColumn: '1 / -1' }}>
                <label style={labelStyle}>Subnet Block (for IP binding)</label>
                <input value={provisionForm.subnet_block}
                  onChange={e => setProvisionForm({ ...provisionForm, subnet_block: e.target.value })}
                  style={{ ...inputStyle, fontFamily: 'monospace' }} />
              </div>

              <div style={{ gridColumn: '1 / -1' }}>
                <label style={{ ...labelStyle, display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                  <input type="checkbox" checked={provisionForm.install_pmta}
                    onChange={e => setProvisionForm({ ...provisionForm, install_pmta: e.target.checked })} />
                  Install PowerMTA on first boot
                </label>
              </div>
            </div>

            <div style={{ marginTop: '1rem', display: 'flex', gap: '0.5rem' }}>
              <button onClick={handleProvision} disabled={provisioning || !provisionForm.plan}
                style={{ ...btnPrimary, opacity: (provisioning || !provisionForm.plan) ? 0.5 : 1 }}>
                {provisioning ? 'Provisioning...' : 'Deploy Server'}
              </button>
            </div>

            {provisionResult && (
              <div style={{
                marginTop: '1rem', padding: '1rem', borderRadius: '0.5rem',
                background: provisionResult.error ? '#7f1d1d' : '#064e3b',
                color: provisionResult.error ? '#fca5a5' : '#6ee7b7',
              }}>
                {provisionResult.error ? (
                  <p>{provisionResult.error}</p>
                ) : (
                  <>
                    <p style={{ fontWeight: 600, marginBottom: '0.5rem' }}>Server deploying: {provisionResult.server_id}</p>
                    {provisionResult.steps?.map((step: any, i: number) => (
                      <div key={i} style={{ fontSize: '0.85rem', marginLeft: '0.5rem', marginBottom: '0.25rem' }}>
                        {step.status === 'completed' ? '✓' : step.status === 'manual_required' ? '⚠' : '○'}{' '}
                        {step.name}
                        {step.detail && <span style={{ opacity: 0.7 }}> — {step.detail}</span>}
                      </div>
                    ))}
                  </>
                )}
              </div>
            )}

            <div style={{ marginTop: '1.5rem', padding: '0.75rem', background: '#1f2937', borderRadius: '0.5rem', fontSize: '0.8rem', color: '#9ca3af' }}>
              <strong style={{ color: '#d1d5db' }}>What happens on deploy:</strong>
              <ol style={{ margin: '0.5rem 0 0 1rem', lineHeight: 1.8 }}>
                <li>Vultr provisions bare metal (CentOS Stream 9) — ~15 min</li>
                <li>Cloud-init runs: installs BIRD2, binds 254 IPs to dummy0</li>
                <li>If BGP enabled: configures BIRD to announce your /24 via AS20473</li>
                <li>If PMTA enabled: installs PowerMTA with per-IP virtual MTAs</li>
                <li>Opens ports 25, 587, 19000 in firewall</li>
              </ol>
            </div>
          </CardBody>
        </Card>
      )}

      {view === 'bgp' && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
          <Card>
            <CardHeader title="BGP Status" />
            <CardBody>
              {data?.bgp ? (
                <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: '1rem' }}>
                  <div>
                    <span style={{ color: '#9ca3af', fontSize: '0.8rem' }}>Status</span><br/>
                    <StatusBadge status={data.bgp.enabled ? 'healthy' : 'warning'}
                      label={data.bgp.enabled ? 'Enabled' : 'Not Enabled'} />
                  </div>
                  <div>
                    <span style={{ color: '#9ca3af', fontSize: '0.8rem' }}>ASN</span><br/>
                    <strong>{data.bgp.asn || '—'}</strong>
                  </div>
                  <div>
                    <span style={{ color: '#9ca3af', fontSize: '0.8rem' }}>BGP Password</span><br/>
                    <code style={{ fontSize: '0.85rem', color: '#a78bfa' }}>{data.bgp.password || '—'}</code>
                  </div>
                  {data.bgp.networks?.length > 0 && (
                    <div>
                      <span style={{ color: '#9ca3af', fontSize: '0.8rem' }}>Announced Networks</span><br/>
                      {data.bgp.networks.map((n, i) => (
                        <span key={i} style={{ fontFamily: 'monospace' }}>{n.v4_subnet}/{n.v4_subnet_len}</span>
                      ))}
                    </div>
                  )}
                </div>
              ) : data?.bgp_error ? (
                <p style={{ color: '#f59e0b' }}>BGP info unavailable: {data.bgp_error}</p>
              ) : (
                <p style={{ color: '#9ca3af' }}>Loading BGP status...</p>
              )}
            </CardBody>
          </Card>

          {!data?.bgp?.enabled && (
            <Card>
              <CardHeader title={
                <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                  <span>Request BGP Access</span>
                  <span style={{ fontSize: '0.7rem', background: '#78350f', color: '#fbbf24', padding: '0.1rem 0.4rem', borderRadius: '0.25rem' }}>Manual Step</span>
                </div>
              } />
              <CardBody>
                <div style={{ fontSize: '0.85rem', color: '#9ca3af' }}>
                  <p style={{ marginBottom: '0.75rem' }}>BGP must be requested through the Vultr Portal (one-time, ~48-72hr review). Steps:</p>
                  <ol style={{ margin: '0 0 1rem 1rem', lineHeight: 1.8, color: '#d1d5db' }}>
                    <li>Go to <a href="https://my.vultr.com/network/bgp/" target="_blank" rel="noopener noreferrer" style={{ color: '#818cf8' }}>Vultr Portal → Network → BGP</a></li>
                    <li>Click "Get Started"</li>
                    <li>Toggle "I have my own IP space"</li>
                    <li>Enter ASN: <code style={{ color: '#a78bfa' }}>20473</code>, IP block: <code style={{ color: '#a78bfa' }}>144.225.178.0/24</code></li>
                    <li>Upload the LOA (generate below)</li>
                    <li>Select "Default Only" for routes</li>
                    <li>Submit — Vultr reviews within 48-72 hours</li>
                  </ol>

                  <button onClick={handleGenerateLOA} style={btnPrimary}>Generate LOA Document</button>

                  {loaText && (
                    <pre style={{
                      marginTop: '0.75rem', padding: '1rem', background: '#111827',
                      borderRadius: '0.375rem', fontSize: '0.8rem', color: '#d1d5db',
                      whiteSpace: 'pre-wrap', lineHeight: 1.6,
                    }}>
                      {loaText}
                    </pre>
                  )}
                </div>
              </CardBody>
            </Card>
          )}
        </div>
      )}
    </div>
  );
};

const thStyle: React.CSSProperties = {
  textAlign: 'left', padding: '0.5rem', color: '#9ca3af', fontSize: '0.8rem', fontWeight: 500,
};
const tdStyle: React.CSSProperties = { padding: '0.5rem', fontSize: '0.9rem' };
const labelStyle: React.CSSProperties = { display: 'block', fontSize: '0.8rem', color: '#9ca3af', marginBottom: '0.25rem' };
const inputStyle: React.CSSProperties = {
  width: '100%', padding: '0.4rem 0.6rem', background: '#111827', color: '#f3f4f6',
  border: '1px solid #374151', borderRadius: '0.375rem', fontSize: '0.9rem',
};
const btnPrimary: React.CSSProperties = {
  padding: '0.5rem 1rem', background: '#4f46e5', color: '#fff',
  border: 'none', borderRadius: '0.375rem', cursor: 'pointer', fontSize: '0.9rem', fontWeight: 600,
};
const btnSmall: React.CSSProperties = {
  padding: '0.2rem 0.5rem', color: '#fff', border: 'none',
  borderRadius: '0.25rem', cursor: 'pointer', fontSize: '0.75rem',
};

export default VultrServers;
