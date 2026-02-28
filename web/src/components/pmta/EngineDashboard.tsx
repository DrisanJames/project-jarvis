import React, { useState } from 'react';
import { useApi } from '../../hooks/useApi';
import { Card, CardHeader, CardBody } from '../common/Card';
import { Loading } from '../common/Loading';
import { StatusBadge } from '../common/StatusBadge';
import { MetricCard } from '../common/MetricCard';
import MemoryView from './MemoryView';
import RecallView from './RecallView';

interface AgentState {
  isp: string;
  agent_type: string;
  status: string;
  last_eval_at: string | null;
  decisions_count: number;
}

interface ISPSummary {
  isp: string;
  display_name: string;
  health_score: number;
  agent_states: AgentState[];
  active_agents: number;
  recent_decisions: number;
  bounce_rate: number;
  deferral_rate: number;
  complaint_rate: number;
  suppression_count: number;
  has_emergency: boolean;
  pool_name: string;
  ip_count: number;
}

interface Decision {
  id: string;
  isp: string;
  agent_type: string;
  action_taken: string;
  target_type: string;
  target_value: string;
  result: string;
  created_at: string;
}

interface EngineOverview {
  isps: ISPSummary[];
  total_agents: number;
  active_agents: number;
  recent_decisions: Decision[];
  total_suppressions: number;
}

interface SuppressionStats {
  isp: string;
  total_count: number;
  today_count: number;
  last_24h_count: number;
  last_1h_count: number;
  top_reasons: { reason: string; count: number }[];
  top_campaigns: { campaign_id: string; count: number }[];
  velocity_per_min: number;
}

interface SuppressionItem {
  id: string;
  email: string;
  isp: string;
  reason: string;
  dsn_code: string;
  source_ip: string;
  campaign_id: string;
  suppressed_at: string;
}

type EngineView = 'overview' | 'isp-detail' | 'suppressions' | 'memory' | 'recall';

const ISP_COLORS: Record<string, string> = {
  gmail: '#ea4335',
  yahoo: '#6001d2',
  microsoft: '#00a4ef',
  apple: '#a2aaad',
  comcast: '#ed1c24',
  att: '#009fdb',
  cox: '#0077c8',
  charter: '#0078d4',
};

const EngineDashboard: React.FC = () => {
  const [view, setView] = useState<EngineView>('overview');
  const [selectedISP, setSelectedISP] = useState<string>('');
  const [suppressionSearch, setSuppressionSearch] = useState('');
  const [suppressionPage, setSuppressionPage] = useState(0);

  const { data: overview, loading } = useApi<EngineOverview>('/api/mailing/engine/dashboard', {
    pollingInterval: 10000,
  });

  const { data: ispDetail } = useApi<{
    isp: string;
    agent_states: AgentState[];
    signals: { bounce_rate_1h: number; deferral_rate_5m: number; complaint_rate_1h: number };
    recent_decisions: Decision[];
    suppression_stats: SuppressionStats;
  }>(`/api/mailing/engine/isp/${selectedISP}/dashboard`, {
    enabled: view === 'isp-detail' && !!selectedISP,
    pollingInterval: 10000,
  });

  const { data: suppressions } = useApi<{ items: SuppressionItem[]; total: number }>(
    `/api/mailing/engine/isp/${selectedISP}/suppressions?search=${suppressionSearch}&limit=50&offset=${suppressionPage * 50}`,
    { enabled: view === 'suppressions' && !!selectedISP }
  );

  const handleISPClick = (isp: string) => {
    setSelectedISP(isp);
    setView('isp-detail');
  };

  const handleSuppressionView = (isp: string) => {
    setSelectedISP(isp);
    setView('suppressions');
    setSuppressionPage(0);
    setSuppressionSearch('');
  };

  if (loading && !overview) return <Loading />;

  const healthColor = (score: number) =>
    score >= 80 ? '#10b981' : score >= 60 ? '#f59e0b' : '#ef4444';

  const statusColor = (status: string) => {
    switch (status) {
      case 'active': return '#10b981';
      case 'firing': return '#ef4444';
      case 'paused': return '#6b7280';
      case 'cooldown': return '#f59e0b';
      default: return '#9ca3af';
    }
  };

  return (
    <div style={{ padding: '1.5rem' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: '1rem', marginBottom: '1.5rem' }}>
        <h2 style={{ margin: 0, fontSize: '1.5rem', fontWeight: 600 }}>
          Traffic Governance Engine
        </h2>
        <span style={{
          fontSize: '0.75rem', color: '#9ca3af', background: '#1f2937',
          padding: '0.25rem 0.75rem', borderRadius: '9999px',
        }}>
          {overview?.total_agents ?? 0} agents | {overview?.active_agents ?? 0} active
        </span>

        <div style={{ display: 'flex', gap: '0.25rem', marginLeft: 'auto' }}>
          {([
            { key: 'overview', label: 'Overview' },
            { key: 'memory', label: 'Memory' },
            { key: 'recall', label: 'Recall' },
          ] as const).map(tab => (
            <button
              key={tab.key}
              onClick={() => setView(tab.key)}
              style={{
                padding: '0.35rem 0.75rem',
                background: view === tab.key ? '#4f46e5' : '#374151',
                color: view === tab.key ? '#fff' : '#d1d5db',
                border: 'none', borderRadius: '0.375rem', cursor: 'pointer',
                fontSize: '0.8rem', fontWeight: view === tab.key ? 600 : 400,
              }}
            >
              {tab.label}
            </button>
          ))}
        </div>
      </div>

      {view === 'overview' && overview && (
        <>
          {/* Summary Metrics */}
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(180px, 1fr))', gap: '1rem', marginBottom: '1.5rem' }}>
            <MetricCard label="Total Agents" value={String(overview.total_agents)} />
            <MetricCard label="Active Agents" value={String(overview.active_agents)} status={overview.active_agents === overview.total_agents ? 'healthy' : 'warning'} />
            <MetricCard label="Recent Decisions" value={String(overview.recent_decisions?.length ?? 0)} />
            <MetricCard label="Total Suppressions" value={overview.total_suppressions.toLocaleString()} />
          </div>

          {/* ISP Health Grid */}
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(300px, 1fr))', gap: '1rem', marginBottom: '1.5rem' }}>
            {overview.isps?.map(isp => (
              <Card key={isp.isp}>
                <div
                  style={{ padding: '1rem', cursor: 'pointer' }}
                  onClick={() => handleISPClick(isp.isp)}
                >
                  <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: '0.75rem' }}>
                    <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                      <div style={{
                        width: '0.75rem', height: '0.75rem', borderRadius: '50%',
                        background: ISP_COLORS[isp.isp] || '#6b7280',
                      }} />
                      <span style={{ fontWeight: 600, fontSize: '1rem' }}>{isp.display_name}</span>
                    </div>
                    <div style={{
                      padding: '0.15rem 0.5rem', borderRadius: '0.25rem', fontSize: '0.75rem', fontWeight: 700,
                      background: `${healthColor(isp.health_score)}20`, color: healthColor(isp.health_score),
                    }}>
                      {isp.health_score.toFixed(0)}
                    </div>
                  </div>

                  {isp.has_emergency && (
                    <div style={{
                      background: '#7f1d1d', color: '#fca5a5', padding: '0.35rem 0.5rem',
                      borderRadius: '0.25rem', fontSize: '0.75rem', fontWeight: 600, marginBottom: '0.5rem',
                    }}>
                      EMERGENCY ACTIVE
                    </div>
                  )}

                  <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: '0.5rem', marginBottom: '0.5rem' }}>
                    <div>
                      <div style={{ fontSize: '0.65rem', color: '#9ca3af', textTransform: 'uppercase' }}>Bounce</div>
                      <div style={{ fontSize: '0.9rem', fontWeight: 600, color: isp.bounce_rate > 5 ? '#ef4444' : isp.bounce_rate > 2 ? '#f59e0b' : '#10b981' }}>
                        {isp.bounce_rate.toFixed(2)}%
                      </div>
                    </div>
                    <div>
                      <div style={{ fontSize: '0.65rem', color: '#9ca3af', textTransform: 'uppercase' }}>Deferral</div>
                      <div style={{ fontSize: '0.9rem', fontWeight: 600, color: isp.deferral_rate > 20 ? '#ef4444' : isp.deferral_rate > 10 ? '#f59e0b' : '#10b981' }}>
                        {isp.deferral_rate.toFixed(2)}%
                      </div>
                    </div>
                    <div>
                      <div style={{ fontSize: '0.65rem', color: '#9ca3af', textTransform: 'uppercase' }}>Complaint</div>
                      <div style={{ fontSize: '0.9rem', fontWeight: 600, color: isp.complaint_rate > 0.06 ? '#ef4444' : isp.complaint_rate > 0.03 ? '#f59e0b' : '#10b981' }}>
                        {isp.complaint_rate.toFixed(3)}%
                      </div>
                    </div>
                  </div>

                  <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: '0.75rem', color: '#9ca3af' }}>
                    <span>{isp.active_agents}/6 agents</span>
                    <span>{isp.recent_decisions} decisions</span>
                    <span
                      onClick={(e) => { e.stopPropagation(); handleSuppressionView(isp.isp); }}
                      style={{ cursor: 'pointer', color: '#818cf8', textDecoration: 'underline' }}
                    >
                      {isp.suppression_count.toLocaleString()} suppressed
                    </span>
                  </div>
                </div>
              </Card>
            ))}
          </div>

          {/* Recent Decisions Feed */}
          {overview.recent_decisions && overview.recent_decisions.length > 0 && (
            <Card>
              <CardHeader title="Recent Decisions" />
              <CardBody>
                <div style={{ maxHeight: '400px', overflow: 'auto' }}>
                  <table style={{ width: '100%', borderCollapse: 'collapse' }}>
                    <thead>
                      <tr style={{ borderBottom: '1px solid #374151' }}>
                        <th style={{ textAlign: 'left', padding: '0.5rem', color: '#9ca3af', fontSize: '0.75rem' }}>Time</th>
                        <th style={{ textAlign: 'left', padding: '0.5rem', color: '#9ca3af', fontSize: '0.75rem' }}>ISP</th>
                        <th style={{ textAlign: 'left', padding: '0.5rem', color: '#9ca3af', fontSize: '0.75rem' }}>Agent</th>
                        <th style={{ textAlign: 'left', padding: '0.5rem', color: '#9ca3af', fontSize: '0.75rem' }}>Action</th>
                        <th style={{ textAlign: 'left', padding: '0.5rem', color: '#9ca3af', fontSize: '0.75rem' }}>Target</th>
                        <th style={{ textAlign: 'center', padding: '0.5rem', color: '#9ca3af', fontSize: '0.75rem' }}>Result</th>
                      </tr>
                    </thead>
                    <tbody>
                      {overview.recent_decisions.slice(0, 20).map((d, i) => (
                        <tr key={d.id || i} style={{ borderBottom: '1px solid #1f2937' }}>
                          <td style={{ padding: '0.4rem 0.5rem', fontSize: '0.8rem', color: '#d1d5db' }}>
                            {new Date(d.created_at).toLocaleTimeString()}
                          </td>
                          <td style={{ padding: '0.4rem 0.5rem' }}>
                            <span style={{
                              fontSize: '0.7rem', fontWeight: 600, padding: '0.1rem 0.35rem',
                              borderRadius: '0.2rem', background: `${ISP_COLORS[d.isp] || '#6b7280'}30`,
                              color: ISP_COLORS[d.isp] || '#9ca3af',
                            }}>
                              {d.isp}
                            </span>
                          </td>
                          <td style={{ padding: '0.4rem 0.5rem', fontSize: '0.8rem' }}>{d.agent_type}</td>
                          <td style={{ padding: '0.4rem 0.5rem', fontSize: '0.8rem', fontFamily: 'monospace' }}>{d.action_taken}</td>
                          <td style={{ padding: '0.4rem 0.5rem', fontSize: '0.8rem', fontFamily: 'monospace', color: '#d1d5db' }}>{d.target_value}</td>
                          <td style={{ padding: '0.4rem 0.5rem', textAlign: 'center' }}>
                            <StatusBadge status={d.result === 'applied' ? 'healthy' : d.result === 'pending' ? 'warning' : 'critical'} />
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </CardBody>
            </Card>
          )}
        </>
      )}

      {view === 'isp-detail' && ispDetail && (
        <>
          <h3 style={{ margin: '0 0 1rem', fontSize: '1.2rem' }}>
            <span style={{ color: ISP_COLORS[selectedISP] || '#9ca3af' }}>{selectedISP.toUpperCase()}</span> Agent Cluster
          </h3>

          {/* Agent Cards */}
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: '1rem', marginBottom: '1.5rem' }}>
            {(ispDetail.agent_states ?? []).map(agent => (
              <Card key={agent.agent_type}>
                <div style={{ padding: '0.75rem' }}>
                  <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '0.5rem' }}>
                    <span style={{ fontWeight: 600, fontSize: '0.85rem', textTransform: 'capitalize' }}>{agent.agent_type}</span>
                    <span style={{
                      width: '0.5rem', height: '0.5rem', borderRadius: '50%',
                      background: statusColor(agent.status),
                    }} />
                  </div>
                  <div style={{ fontSize: '0.75rem', color: '#9ca3af' }}>
                    Status: <span style={{ color: statusColor(agent.status), fontWeight: 600 }}>{agent.status}</span>
                  </div>
                  <div style={{ fontSize: '0.75rem', color: '#9ca3af' }}>
                    Decisions: {agent.decisions_count}
                  </div>
                  {agent.last_eval_at && (
                    <div style={{ fontSize: '0.7rem', color: '#6b7280', marginTop: '0.25rem' }}>
                      Last eval: {new Date(agent.last_eval_at).toLocaleTimeString()}
                    </div>
                  )}
                </div>
              </Card>
            ))}
          </div>

          {/* Signal Gauges */}
          {ispDetail.signals && (
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: '1rem', marginBottom: '1.5rem' }}>
              <GaugeCard label="Bounce Rate (1h)" value={ispDetail.signals.bounce_rate_1h} warn={2} danger={5} suffix="%" />
              <GaugeCard label="Deferral Rate (5m)" value={ispDetail.signals.deferral_rate_5m} warn={20} danger={50} suffix="%" />
              <GaugeCard label="Complaint Rate (1h)" value={ispDetail.signals.complaint_rate_1h} warn={0.03} danger={0.06} suffix="%" />
            </div>
          )}

          {/* Suppression Stats Panel */}
          {ispDetail.suppression_stats && (
            <Card>
              <CardHeader title="Suppression Stats" />
              <CardBody>
                <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(120px, 1fr))', gap: '1rem', marginBottom: '1rem' }}>
                  <div>
                    <div style={{ fontSize: '0.65rem', color: '#9ca3af', textTransform: 'uppercase' }}>Total</div>
                    <div style={{ fontSize: '1.2rem', fontWeight: 700 }}>{ispDetail.suppression_stats.total_count?.toLocaleString()}</div>
                  </div>
                  <div>
                    <div style={{ fontSize: '0.65rem', color: '#9ca3af', textTransform: 'uppercase' }}>Today</div>
                    <div style={{ fontSize: '1.2rem', fontWeight: 700 }}>{ispDetail.suppression_stats.today_count?.toLocaleString()}</div>
                  </div>
                  <div>
                    <div style={{ fontSize: '0.65rem', color: '#9ca3af', textTransform: 'uppercase' }}>Last 1h</div>
                    <div style={{ fontSize: '1.2rem', fontWeight: 700 }}>{ispDetail.suppression_stats.last_1h_count?.toLocaleString()}</div>
                  </div>
                  <div>
                    <div style={{ fontSize: '0.65rem', color: '#9ca3af', textTransform: 'uppercase' }}>Velocity</div>
                    <div style={{ fontSize: '1.2rem', fontWeight: 700 }}>{ispDetail.suppression_stats.velocity_per_min?.toFixed(1)}/min</div>
                  </div>
                </div>

                {ispDetail.suppression_stats.top_reasons && ispDetail.suppression_stats.top_reasons.length > 0 && (
                  <div style={{ marginTop: '0.5rem' }}>
                    <div style={{ fontSize: '0.75rem', color: '#9ca3af', marginBottom: '0.5rem' }}>Top Reasons</div>
                    {ispDetail.suppression_stats.top_reasons.slice(0, 5).map((r, i) => (
                      <div key={i} style={{ display: 'flex', justifyContent: 'space-between', padding: '0.2rem 0', fontSize: '0.8rem' }}>
                        <span style={{ fontFamily: 'monospace' }}>{r.reason}</span>
                        <span style={{ color: '#9ca3af' }}>{r.count.toLocaleString()}</span>
                      </div>
                    ))}
                  </div>
                )}

                <button
                  onClick={() => handleSuppressionView(selectedISP)}
                  style={{
                    marginTop: '1rem', padding: '0.4rem 0.75rem', background: '#4f46e5',
                    color: '#fff', border: 'none', borderRadius: '0.375rem', cursor: 'pointer',
                    fontSize: '0.8rem',
                  }}
                >
                  View Full Suppression List
                </button>
              </CardBody>
            </Card>
          )}

          {/* Decision Timeline */}
          {ispDetail.recent_decisions && ispDetail.recent_decisions.length > 0 && (
            <Card>
              <CardHeader title="Decision Timeline" />
              <CardBody>
                <div style={{ maxHeight: '300px', overflow: 'auto' }}>
                  {ispDetail.recent_decisions.map((d, i) => (
                    <div key={d.id || i} style={{
                      display: 'flex', gap: '0.75rem', padding: '0.5rem 0',
                      borderBottom: '1px solid #1f2937', alignItems: 'center',
                    }}>
                      <span style={{ fontSize: '0.75rem', color: '#6b7280', minWidth: '70px' }}>
                        {new Date(d.created_at).toLocaleTimeString()}
                      </span>
                      <span style={{
                        fontSize: '0.7rem', fontWeight: 600, padding: '0.1rem 0.35rem',
                        borderRadius: '0.2rem', background: '#374151', color: '#d1d5db',
                        textTransform: 'capitalize', minWidth: '70px', textAlign: 'center',
                      }}>{d.agent_type}</span>
                      <span style={{ fontSize: '0.8rem', fontFamily: 'monospace', flex: 1 }}>{d.action_taken}</span>
                      <span style={{ fontSize: '0.8rem', color: '#9ca3af', fontFamily: 'monospace' }}>{d.target_value}</span>
                    </div>
                  ))}
                </div>
              </CardBody>
            </Card>
          )}
        </>
      )}

      {view === 'suppressions' && selectedISP && (
        <>
          <h3 style={{ margin: '0 0 1rem', fontSize: '1.2rem' }}>
            <span style={{ color: ISP_COLORS[selectedISP] || '#9ca3af' }}>{selectedISP.toUpperCase()}</span> Suppression List
          </h3>

          <div style={{ display: 'flex', gap: '0.5rem', marginBottom: '1rem' }}>
            <input
              type="text"
              placeholder="Search email..."
              value={suppressionSearch}
              onChange={e => { setSuppressionSearch(e.target.value); setSuppressionPage(0); }}
              style={{
                flex: 1, padding: '0.5rem 0.75rem', background: '#1f2937', border: '1px solid #374151',
                borderRadius: '0.375rem', color: '#fff', fontSize: '0.85rem',
              }}
            />
            <button
              onClick={() => handleISPClick(selectedISP)}
              style={{
                padding: '0.5rem 0.75rem', background: '#374151', color: '#d1d5db',
                border: 'none', borderRadius: '0.375rem', cursor: 'pointer', fontSize: '0.8rem',
              }}
            >
              Back to ISP Detail
            </button>
          </div>

          <Card>
            <CardBody>
              {suppressions && suppressions.items && suppressions.items.length > 0 ? (
                <>
                  <div style={{ marginBottom: '0.5rem', fontSize: '0.8rem', color: '#9ca3af' }}>
                    {suppressions.total.toLocaleString()} total suppressions
                  </div>
                  <table style={{ width: '100%', borderCollapse: 'collapse' }}>
                    <thead>
                      <tr style={{ borderBottom: '1px solid #374151' }}>
                        <th style={{ textAlign: 'left', padding: '0.5rem', color: '#9ca3af', fontSize: '0.75rem' }}>Email</th>
                        <th style={{ textAlign: 'left', padding: '0.5rem', color: '#9ca3af', fontSize: '0.75rem' }}>Reason</th>
                        <th style={{ textAlign: 'left', padding: '0.5rem', color: '#9ca3af', fontSize: '0.75rem' }}>DSN Code</th>
                        <th style={{ textAlign: 'left', padding: '0.5rem', color: '#9ca3af', fontSize: '0.75rem' }}>Source IP</th>
                        <th style={{ textAlign: 'left', padding: '0.5rem', color: '#9ca3af', fontSize: '0.75rem' }}>Suppressed At</th>
                      </tr>
                    </thead>
                    <tbody>
                      {suppressions.items.map((s, i) => (
                        <tr key={s.id || i} style={{ borderBottom: '1px solid #1f2937' }}>
                          <td style={{ padding: '0.4rem 0.5rem', fontSize: '0.8rem', fontFamily: 'monospace' }}>{s.email}</td>
                          <td style={{ padding: '0.4rem 0.5rem', fontSize: '0.8rem' }}>
                            <span style={{
                              padding: '0.1rem 0.35rem', borderRadius: '0.2rem',
                              background: '#7f1d1d', color: '#fca5a5', fontSize: '0.7rem',
                            }}>{s.reason}</span>
                          </td>
                          <td style={{ padding: '0.4rem 0.5rem', fontSize: '0.8rem', fontFamily: 'monospace', color: '#9ca3af' }}>{s.dsn_code}</td>
                          <td style={{ padding: '0.4rem 0.5rem', fontSize: '0.8rem', fontFamily: 'monospace', color: '#9ca3af' }}>{s.source_ip}</td>
                          <td style={{ padding: '0.4rem 0.5rem', fontSize: '0.8rem', color: '#9ca3af' }}>
                            {new Date(s.suppressed_at).toLocaleString()}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>

                  {/* Pagination */}
                  <div style={{ display: 'flex', justifyContent: 'center', gap: '0.5rem', marginTop: '1rem' }}>
                    <button
                      onClick={() => setSuppressionPage(p => Math.max(0, p - 1))}
                      disabled={suppressionPage === 0}
                      style={{
                        padding: '0.35rem 0.75rem', background: '#374151', color: '#d1d5db',
                        border: 'none', borderRadius: '0.375rem', cursor: 'pointer', fontSize: '0.8rem',
                        opacity: suppressionPage === 0 ? 0.5 : 1,
                      }}
                    >
                      Previous
                    </button>
                    <span style={{ padding: '0.35rem 0.5rem', fontSize: '0.8rem', color: '#9ca3af' }}>
                      Page {suppressionPage + 1}
                    </span>
                    <button
                      onClick={() => setSuppressionPage(p => p + 1)}
                      disabled={!suppressions || (suppressionPage + 1) * 50 >= suppressions.total}
                      style={{
                        padding: '0.35rem 0.75rem', background: '#374151', color: '#d1d5db',
                        border: 'none', borderRadius: '0.375rem', cursor: 'pointer', fontSize: '0.8rem',
                        opacity: (!suppressions || (suppressionPage + 1) * 50 >= suppressions.total) ? 0.5 : 1,
                      }}
                    >
                      Next
                    </button>
                  </div>
                </>
              ) : (
                <p style={{ color: '#9ca3af', textAlign: 'center', padding: '2rem' }}>
                  {suppressionSearch ? 'No matching suppressions found' : 'No suppressions for this ISP'}
                </p>
              )}
            </CardBody>
          </Card>
        </>
      )}

      {view === 'memory' && <MemoryView />}
      {view === 'recall' && <RecallView />}
    </div>
  );
};

const GaugeCard: React.FC<{ label: string; value: number; warn: number; danger: number; suffix: string }> = ({
  label, value, warn, danger, suffix,
}) => {
  const color = value >= danger ? '#ef4444' : value >= warn ? '#f59e0b' : '#10b981';
  return (
    <Card>
      <div style={{ padding: '0.75rem', textAlign: 'center' }}>
        <div style={{ fontSize: '0.7rem', color: '#9ca3af', textTransform: 'uppercase', marginBottom: '0.25rem' }}>
          {label}
        </div>
        <div style={{ fontSize: '1.5rem', fontWeight: 700, color }}>
          {value.toFixed(value < 1 ? 3 : 2)}{suffix}
        </div>
      </div>
    </Card>
  );
};

export default EngineDashboard;
