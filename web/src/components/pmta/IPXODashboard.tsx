import React, { useState, useCallback } from 'react';
import { useApi } from '../../hooks/useApi';
import { Card, CardHeader, CardBody } from '../common/Card';
import { Loading } from '../common/Loading';
import { MetricCard } from '../common/MetricCard';
import { StatusBadge } from '../common/StatusBadge';

interface IPXODashboardData {
  configured: boolean;
  message?: string;
  dashboard?: {
    total_prefixes: number;
    total_ips: number;
    announced_count: number;
    unannounced_count: number;
    prefixes: Array<{
      notation: string;
      maskSize: number;
      geodata?: Array<{ provider: string; countryName: string; countryCode: string; cityName: string }>;
      whois?: {
        inetnum: string;
        registrar: string;
        netname: string;
        country: string;
        organisation: string;
        status: string;
      };
      routes?: Array<{ route: string; origin: string; source: string }>;
    }>;
    subscriptions: Array<{
      uuid: string;
      name: string;
      short_description: string;
      status: string;
      started_at: string;
      current_period_start: string;
      current_period_end: string;
      total: { currency: string; amount: string; amount_minor: number };
      has_immediate_termination: boolean;
    }>;
    invoices: Array<{
      uuid: string;
      reference: string;
      status: string;
      placed_at: string;
      total: { currency: string; amount: string; amount_minor: number };
      sub_total: { currency: string; amount: string };
      tax_total: { currency: string; amount: string };
    }>;
    credit_balance?: { available_balance: number; total_balance: number; currency: string };
    subnet_block?: string;
  };
}

interface SyncResult {
  prefixes_fetched: number;
  ips_imported: number;
  ips_updated: number;
  errors?: string[];
}

interface SetupStatusData {
  subnet_leased: boolean;
  subnet_block: string;
  asn_assigned: boolean;
  asn?: number;
  as_name?: string;
  loa_status: string;
  rdns_configured: boolean;
  rdns_note: string;
  forward_dns_note: string;
  server_attached: boolean;
  server_note: string;
}

type SubTab = 'overview' | 'setup' | 'prefixes' | 'subscriptions' | 'invoices';

const IPXODashboard: React.FC = () => {
  const [activeTab, setActiveTab] = useState<SubTab>('overview');
  const [syncing, setSyncing] = useState(false);
  const [syncResult, setSyncResult] = useState<SyncResult | null>(null);
  const [asnInput, setAsnInput] = useState('20473');
  const [assigning, setAssigning] = useState(false);
  const [assignResult, setAssignResult] = useState<any>(null);

  const { data, loading, error, refetch } = useApi<IPXODashboardData>('/api/mailing/ipxo/dashboard', {
    pollingInterval: 300000, // 5 min
  });

  const handleSync = useCallback(async (expandIPs: boolean) => {
    setSyncing(true);
    try {
      const resp = await fetch('/api/mailing/ipxo/sync', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify({ expand_ips: expandIPs }),
      });
      const result = await resp.json();
      setSyncResult(result);
      refetch();
    } finally {
      setSyncing(false);
    }
  }, [refetch]);

  if (loading && !data) return <Loading />;
  if (error) return <div style={{ color: '#ef4444', padding: '1rem' }}>Error: {error}</div>;

  if (data && !data.configured) {
    return (
      <Card>
        <CardBody>
          <div style={{ textAlign: 'center', padding: '3rem', color: '#9ca3af' }}>
            <div style={{ fontSize: '1.5rem', marginBottom: '0.5rem' }}>IPXO Not Configured</div>
            <p>Set the following environment variables to enable IPXO integration:</p>
            <div style={{ fontFamily: 'monospace', background: '#1f2937', padding: '1rem', borderRadius: '0.5rem', marginTop: '1rem', textAlign: 'left', display: 'inline-block' }}>
              IPXO_CLIENT_ID=your_client_id<br/>
              IPXO_SECRET_KEY=your_secret_key<br/>
              IPXO_COMPANY_UUID=your_company_uuid
            </div>
          </div>
        </CardBody>
      </Card>
    );
  }

  const d = data?.dashboard;
  const { data: setupData } = useApi<SetupStatusData>('/api/mailing/ipxo/setup-status', {
    pollingInterval: 60000,
  });

  const handleAssignASN = useCallback(async () => {
    if (!d?.subnet_block || !asnInput) return;
    setAssigning(true);
    setAssignResult(null);
    try {
      const resp = await fetch('/api/mailing/ipxo/asn/assign', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify({
          asn: parseInt(asnInput, 10),
          subnet: d.subnet_block,
          company_name: 'RDJ CO LLC.',
        }),
      });
      const result = await resp.json();
      setAssignResult(result);
    } finally {
      setAssigning(false);
    }
  }, [d?.subnet_block, asnInput]);

  const tabs: { key: SubTab; label: string }[] = [
    { key: 'overview', label: 'Overview' },
    { key: 'setup', label: 'IP Setup' },
    { key: 'prefixes', label: `Prefixes (${d?.total_prefixes ?? 0})` },
    { key: 'subscriptions', label: `Subscriptions (${d?.subscriptions?.length ?? 0})` },
    { key: 'invoices', label: `Invoices (${d?.invoices?.length ?? 0})` },
  ];

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1.5rem' }}>
      {/* Sub-tabs */}
      <div style={{ display: 'flex', gap: '0.25rem', borderBottom: '1px solid #374151' }}>
        {tabs.map(tab => (
          <button
            key={tab.key}
            onClick={() => setActiveTab(tab.key)}
            style={{
              padding: '0.4rem 0.8rem', border: 'none', cursor: 'pointer', fontSize: '0.85rem',
              background: activeTab === tab.key ? '#6366f1' : 'transparent',
              color: activeTab === tab.key ? '#fff' : '#9ca3af',
              borderRadius: '0.375rem 0.375rem 0 0',
              fontWeight: activeTab === tab.key ? 600 : 400,
            }}
          >
            {tab.label}
          </button>
        ))}
      </div>

      {activeTab === 'overview' && (
        <>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(180px, 1fr))', gap: '1rem' }}>
            <MetricCard label="Subnet Block" value={d?.subnet_block ?? `${d?.total_prefixes ?? 0} prefix(es)`} />
            <MetricCard label="Total IPs" value={(d?.total_ips ?? 0).toLocaleString()} />
            <MetricCard label="Active Subs" value={String(d?.subscriptions?.length ?? 0)} status="healthy" />
            <MetricCard label="Credit Balance"
              value={`$${(d?.credit_balance?.available_balance ?? 0).toFixed(2)}`} />
          </div>

          {/* Sync controls */}
          <Card>
            <CardHeader>
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                <span>Sync IPXO → Platform</span>
                <div style={{ display: 'flex', gap: '0.5rem' }}>
                  <button
                    onClick={() => handleSync(false)}
                    disabled={syncing}
                    style={syncBtnStyle}
                  >
                    {syncing ? 'Syncing...' : 'Sync Prefixes'}
                  </button>
                  <button
                    onClick={() => handleSync(true)}
                    disabled={syncing}
                    style={{ ...syncBtnStyle, background: '#7c3aed' }}
                    title="Import all /24 as individual IPs into IP management"
                  >
                    Expand & Import All IPs
                  </button>
                </div>
              </div>
            </CardHeader>
            <CardBody>
              {syncResult ? (
                <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(150px, 1fr))', gap: '1rem', fontSize: '0.9rem' }}>
                  <div><span style={{ color: '#9ca3af' }}>Prefixes Fetched</span><br/><strong>{syncResult.prefixes_fetched}</strong></div>
                  <div><span style={{ color: '#9ca3af' }}>IPs Imported</span><br/><strong style={{ color: '#10b981' }}>{syncResult.ips_imported}</strong></div>
                  <div><span style={{ color: '#9ca3af' }}>IPs Updated</span><br/><strong>{syncResult.ips_updated}</strong></div>
                  {syncResult.errors && syncResult.errors.length > 0 && (
                    <div><span style={{ color: '#ef4444' }}>Errors</span><br/><strong style={{ color: '#ef4444' }}>{syncResult.errors.length}</strong></div>
                  )}
                </div>
              ) : (
                <p style={{ color: '#9ca3af', fontSize: '0.85rem' }}>
                  Sync IPXO prefix data into the platform's IP management system. Use "Expand & Import All IPs" to create individual IP records from your /24 block.
                </p>
              )}
            </CardBody>
          </Card>
        </>
      )}

      {activeTab === 'setup' && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
          {/* Step 1: Lease */}
          <Card>
            <CardHeader>
              <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
                <StepIndicator done={setupData?.subnet_leased ?? false} step={1} />
                <span>Lease IPv4 Subnet</span>
              </div>
            </CardHeader>
            <CardBody>
              {setupData?.subnet_leased ? (
                <div style={{ display: 'flex', alignItems: 'center', gap: '1rem' }}>
                  <span style={{ fontFamily: 'monospace', fontSize: '1.1rem', fontWeight: 700, color: '#10b981' }}>
                    {setupData.subnet_block}
                  </span>
                  <StatusBadge status="healthy" label="Active" />
                </div>
              ) : (
                <p style={{ color: '#9ca3af' }}>No active subnet lease found. Lease a /24 from the IPXO Marketplace.</p>
              )}
            </CardBody>
          </Card>

          {/* Step 2: Assign ASN */}
          <Card>
            <CardHeader>
              <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
                <StepIndicator done={setupData?.asn_assigned ?? false} step={2} />
                <span>Assign ASN (Create LOA/ROA)</span>
                <span style={{ fontSize: '0.7rem', background: '#065f46', color: '#6ee7b7', padding: '0.1rem 0.4rem', borderRadius: '0.25rem', marginLeft: '0.5rem' }}>API</span>
              </div>
            </CardHeader>
            <CardBody>
              {setupData?.asn_assigned ? (
                <div>
                  <div style={{ display: 'flex', alignItems: 'center', gap: '1rem' }}>
                    <span>ASN {setupData.asn} ({setupData.as_name})</span>
                    <StatusBadge
                      status={setupData.loa_status === 'Active' ? 'healthy' : 'warning'}
                      label={`LOA: ${setupData.loa_status}`}
                    />
                  </div>
                </div>
              ) : (
                <div style={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
                  <p style={{ color: '#9ca3af', fontSize: '0.85rem', margin: 0 }}>
                    Assign your hosting provider's ASN to enable BGP announcement. Common ASNs:
                  </p>
                  <div style={{ display: 'flex', gap: '0.5rem', flexWrap: 'wrap' }}>
                    {[
                      { asn: '20473', label: 'Vultr Bare Metal' },
                      { asn: '64515', label: 'Vultr VPS' },
                      { asn: '24940', label: 'Hetzner' },
                      { asn: '16509', label: 'AWS' },
                    ].map(({ asn, label }) => (
                      <button
                        key={asn}
                        onClick={() => setAsnInput(asn)}
                        style={{
                          padding: '0.25rem 0.5rem', fontSize: '0.8rem', cursor: 'pointer',
                          background: asnInput === asn ? '#4f46e5' : '#1f2937',
                          color: asnInput === asn ? '#fff' : '#d1d5db',
                          border: '1px solid #374151', borderRadius: '0.25rem',
                        }}
                      >
                        AS{asn} ({label})
                      </button>
                    ))}
                  </div>
                  <div style={{ display: 'flex', gap: '0.5rem', alignItems: 'center' }}>
                    <input
                      type="text"
                      value={asnInput}
                      onChange={e => setAsnInput(e.target.value)}
                      placeholder="ASN number"
                      style={{
                        padding: '0.4rem 0.6rem', background: '#111827', color: '#f3f4f6',
                        border: '1px solid #374151', borderRadius: '0.375rem', fontFamily: 'monospace',
                        width: '120px',
                      }}
                    />
                    <button
                      onClick={handleAssignASN}
                      disabled={assigning || !setupData?.subnet_leased}
                      style={{
                        ...syncBtnStyle,
                        opacity: (assigning || !setupData?.subnet_leased) ? 0.5 : 1,
                      }}
                    >
                      {assigning ? 'Assigning...' : 'Assign ASN & Create LOA'}
                    </button>
                  </div>
                  {assignResult && (
                    <div style={{
                      padding: '0.75rem', borderRadius: '0.375rem', fontSize: '0.85rem',
                      background: assignResult.error ? '#7f1d1d' : '#064e3b',
                      color: assignResult.error ? '#fca5a5' : '#6ee7b7',
                    }}>
                      {assignResult.error || assignResult.message}
                      {assignResult.order_uuid && (
                        <span style={{ display: 'block', marginTop: '0.25rem', fontSize: '0.75rem', opacity: 0.8 }}>
                          Order: {assignResult.order_uuid}
                        </span>
                      )}
                    </div>
                  )}
                </div>
              )}
            </CardBody>
          </Card>

          {/* Step 3: rDNS */}
          <Card>
            <CardHeader>
              <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
                <StepIndicator done={setupData?.rdns_configured ?? false} step={3} />
                <span>Configure rDNS (PTR Records)</span>
                <span style={{ fontSize: '0.7rem', background: '#78350f', color: '#fbbf24', padding: '0.1rem 0.4rem', borderRadius: '0.25rem', marginLeft: '0.5rem' }}>Portal Only</span>
              </div>
            </CardHeader>
            <CardBody>
              <div style={{ color: '#9ca3af', fontSize: '0.85rem' }}>
                <p style={{ margin: '0 0 0.5rem 0' }}>{setupData?.rdns_note}</p>
                <p style={{ margin: '0 0 0.5rem 0', color: '#d1d5db' }}>
                  Each IP needs a PTR record pointing to its hostname (e.g., <code style={{ color: '#a78bfa' }}>144.225.178.1 → mta1.mail.ignitemailing.com</code>).
                </p>
                <a href="https://www.ipxo.com/portal/lease/leased-subnets" target="_blank" rel="noopener noreferrer"
                  style={{ color: '#818cf8', textDecoration: 'underline' }}>
                  Open IPXO Portal → Leased Subnets
                </a>
              </div>
            </CardBody>
          </Card>

          {/* Step 4: Forward DNS */}
          <Card>
            <CardHeader>
              <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
                <StepIndicator done={false} step={4} />
                <span>Configure Forward DNS (A, SPF, DKIM)</span>
                <span style={{ fontSize: '0.7rem', background: '#1e3a5f', color: '#93c5fd', padding: '0.1rem 0.4rem', borderRadius: '0.25rem', marginLeft: '0.5rem' }}>DNS Provider</span>
              </div>
            </CardHeader>
            <CardBody>
              <div style={{ color: '#9ca3af', fontSize: '0.85rem' }}>
                <p style={{ margin: '0 0 0.5rem 0' }}>{setupData?.forward_dns_note}</p>
                <div style={{ background: '#111827', padding: '0.75rem', borderRadius: '0.375rem', fontFamily: 'monospace', fontSize: '0.8rem', lineHeight: 1.6 }}>
                  <div style={{ color: '#6ee7b7' }}>; A Records (one per IP/hostname)</div>
                  <div>mta1.mail.ignitemailing.com  A  144.225.178.1</div>
                  <div>mta2.mail.ignitemailing.com  A  144.225.178.2</div>
                  <div style={{ color: '#6b7280' }}>; ... up to 254</div>
                  <br/>
                  <div style={{ color: '#6ee7b7' }}>; SPF Record on sending domain</div>
                  <div>v=spf1 ip4:144.225.178.0/24 ~all</div>
                  <br/>
                  <div style={{ color: '#6ee7b7' }}>; DKIM — generate via DKIM Manager tab</div>
                </div>
              </div>
            </CardBody>
          </Card>

          {/* Step 5: Server */}
          <Card>
            <CardHeader>
              <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
                <StepIndicator done={setupData?.server_attached ?? false} step={5} />
                <span>BYOIP at Hosting Provider</span>
                <span style={{ fontSize: '0.7rem', background: '#1e3a5f', color: '#93c5fd', padding: '0.1rem 0.4rem', borderRadius: '0.25rem', marginLeft: '0.5rem' }}>Provider</span>
              </div>
            </CardHeader>
            <CardBody>
              <div style={{ color: '#9ca3af', fontSize: '0.85rem' }}>
                <p style={{ margin: '0 0 0.5rem 0' }}>{setupData?.server_note}</p>
                <p style={{ margin: 0, color: '#d1d5db' }}>
                  After IPXO confirms the ROA (~48hrs), submit the /24 block as BYOIP at your hosting provider.
                  They will announce the block via BGP. Then bind the IPs to the server interface and install PowerMTA.
                </p>
              </div>
            </CardBody>
          </Card>
        </div>
      )}

      {activeTab === 'prefixes' && (
        <Card>
          <CardHeader>IP Prefixes from IPXO</CardHeader>
          <CardBody>
            {d?.prefixes && d.prefixes.length > 0 ? (
              <table style={{ width: '100%', borderCollapse: 'collapse' }}>
                <thead>
                  <tr style={{ borderBottom: '1px solid #374151' }}>
                    <th style={thStyle}>Prefix</th>
                    <th style={thStyle}>Size</th>
                    <th style={thStyle}>Registrar</th>
                    <th style={thStyle}>Organisation</th>
                    <th style={thStyle}>Country</th>
                    <th style={thStyle}>Status</th>
                    <th style={thStyle}>Routes</th>
                  </tr>
                </thead>
                <tbody>
                  {d.prefixes.map((p, i) => (
                    <tr key={i} style={{ borderBottom: '1px solid #1f2937' }}>
                      <td style={{ ...tdStyle, fontFamily: 'monospace', fontWeight: 600 }}>{p.notation}</td>
                      <td style={tdStyle}>/{p.maskSize} ({(1 << (32 - p.maskSize)).toLocaleString()} IPs)</td>
                      <td style={{ ...tdStyle, textTransform: 'uppercase' }}>{p.whois?.registrar ?? '—'}</td>
                      <td style={tdStyle}>{p.whois?.organisation ?? '—'}</td>
                      <td style={tdStyle}>{p.whois?.country ?? '—'}</td>
                      <td style={tdStyle}>
                        <StatusBadge
                          status={p.whois?.status?.includes('ALLOCATED') ? 'healthy' : 'neutral'}
                          label={p.whois?.status ?? 'Unknown'}
                        />
                      </td>
                      <td style={tdStyle}>
                        {p.routes && p.routes.length > 0 ? (
                          p.routes.map((r, j) => (
                            <span key={j} style={{ fontSize: '0.75rem', fontFamily: 'monospace', background: '#1f2937', padding: '0.1rem 0.3rem', borderRadius: '0.2rem', marginRight: '0.25rem' }}>
                              AS{r.origin}
                            </span>
                          ))
                        ) : (
                          <span style={{ color: '#f59e0b', fontSize: '0.8rem' }}>Not announced</span>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ) : (
              <p style={{ color: '#9ca3af', textAlign: 'center', padding: '1rem' }}>No prefixes found</p>
            )}
          </CardBody>
        </Card>
      )}

      {activeTab === 'subscriptions' && (
        <Card>
          <CardHeader>Active Subscriptions</CardHeader>
          <CardBody>
            {d?.subscriptions && d.subscriptions.length > 0 ? (
              <table style={{ width: '100%', borderCollapse: 'collapse' }}>
                <thead>
                  <tr style={{ borderBottom: '1px solid #374151' }}>
                    <th style={thStyle}>Subnet</th>
                    <th style={thStyle}>Amount</th>
                    <th style={thStyle}>Period</th>
                    <th style={thStyle}>Started</th>
                    <th style={thStyle}>Status</th>
                  </tr>
                </thead>
                <tbody>
                  {d.subscriptions.map((sub, i) => (
                    <tr key={i} style={{ borderBottom: '1px solid #1f2937' }}>
                      <td style={{ ...tdStyle, fontFamily: 'monospace', fontWeight: 600 }}>{sub.name}</td>
                      <td style={tdStyle}>${sub.total.amount} {sub.total.currency}/mo</td>
                      <td style={{ ...tdStyle, fontSize: '0.8rem' }}>
                        {sub.current_period_start ? new Date(sub.current_period_start).toLocaleDateString() : '—'}
                        {' → '}
                        {sub.current_period_end ? new Date(sub.current_period_end).toLocaleDateString() : '—'}
                      </td>
                      <td style={tdStyle}>{sub.started_at ? new Date(sub.started_at).toLocaleDateString() : '—'}</td>
                      <td style={tdStyle}>
                        <StatusBadge status={sub.status === 'active' ? 'healthy' : 'neutral'} label={sub.status} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ) : (
              <p style={{ color: '#9ca3af', textAlign: 'center', padding: '1rem' }}>No active subscriptions</p>
            )}
          </CardBody>
        </Card>
      )}

      {activeTab === 'invoices' && (
        <Card>
          <CardHeader>Recent Invoices</CardHeader>
          <CardBody>
            {d?.invoices && d.invoices.length > 0 ? (
              <table style={{ width: '100%', borderCollapse: 'collapse' }}>
                <thead>
                  <tr style={{ borderBottom: '1px solid #374151' }}>
                    <th style={thStyle}>Invoice #</th>
                    <th style={thStyle}>Date</th>
                    <th style={{ ...thStyle, textAlign: 'right' }}>Amount</th>
                    <th style={thStyle}>Status</th>
                  </tr>
                </thead>
                <tbody>
                  {d.invoices.map((inv, i) => (
                    <tr key={i} style={{ borderBottom: '1px solid #1f2937' }}>
                      <td style={{ ...tdStyle, fontFamily: 'monospace' }}>{inv.reference || inv.uuid.slice(0, 8)}</td>
                      <td style={tdStyle}>{inv.placed_at ? new Date(inv.placed_at).toLocaleDateString() : '—'}</td>
                      <td style={{ ...tdStyle, textAlign: 'right', fontWeight: 600 }}>${inv.total.amount} {inv.total.currency}</td>
                      <td style={tdStyle}>
                        <StatusBadge
                          status={inv.status === 'paid' ? 'healthy' : inv.status === 'unpaid' ? 'warning' : 'neutral'}
                          label={inv.status}
                        />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ) : (
              <p style={{ color: '#9ca3af', textAlign: 'center', padding: '1rem' }}>No invoices found</p>
            )}
          </CardBody>
        </Card>
      )}
    </div>
  );
};

const thStyle: React.CSSProperties = {
  textAlign: 'left', padding: '0.5rem', color: '#9ca3af', fontSize: '0.8rem', fontWeight: 500,
};

const tdStyle: React.CSSProperties = {
  padding: '0.5rem', fontSize: '0.9rem',
};

const syncBtnStyle: React.CSSProperties = {
  padding: '0.4rem 0.8rem', background: '#4f46e5', color: '#fff',
  border: 'none', borderRadius: '0.375rem', cursor: 'pointer', fontSize: '0.85rem',
};

const StepIndicator: React.FC<{ done: boolean; step: number }> = ({ done, step }) => (
  <div style={{
    width: '1.5rem', height: '1.5rem', borderRadius: '50%', display: 'flex',
    alignItems: 'center', justifyContent: 'center', fontSize: '0.75rem', fontWeight: 700,
    background: done ? '#065f46' : '#1f2937',
    color: done ? '#6ee7b7' : '#6b7280',
    border: `2px solid ${done ? '#10b981' : '#374151'}`,
    flexShrink: 0,
  }}>
    {done ? '✓' : step}
  </div>
);

export default IPXODashboard;
