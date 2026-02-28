import React, { useState, useEffect, useCallback, useRef } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faPlay, faStop, faRotateRight, faPaperPlane, faEye, faBullseye,
  faGauge, faTriangleExclamation, faRobot, faTrophy, faChartLine,
  faComments, faBolt, faDollarSign, faXmark,
  faPause, faClock, faArrowTrendUp, faCircleExclamation,
  faHandPointer, faSpinner, faChartBar
} from '@fortawesome/free-solid-svg-icons';
import './MissionControl.css';

// ============================================================================
// TYPES
// ============================================================================

interface AgentState {
  phase: string;
  current_tier: number;
  total_sent: number;
  total_opens: number;
  total_clicks: number;
  total_bounces: number;
  total_complaints: number;
  current_open_rate: number;
  current_bounce_rate: number;
  complaint_rate: number;
  inbox_rate: number;
  throttle_rate: number;
  is_paused: boolean;
  pause_reason?: string;
}

interface SendEvent {
  id: string;
  time: string;
  type: string;
  email: string;
  variant_id: string;
  tier: string;
  details?: string;
}

interface AgentDecision {
  id: string;
  time: string;
  type: string;
  reasoning: string;
  action: string;
  impact?: string;
  metrics?: Record<string, number>;
}

interface Consultation {
  id: string;
  time: string;
  from: string;
  message: string;
}

interface ABVariantStats {
  variant_id: string;
  subject_line: string;
  from_name: string;
  sent: number;
  opens: number;
  clicks: number;
  conversions: number;
  revenue: number;
  open_rate: number;
  click_rate: number;
  conversion_rate: number;
  bounce_rate: number;
  complaint_rate: number;
  epc: number;
  confidence: number;
  is_winner: boolean;
  is_eliminated: boolean;
}

interface FunnelStats {
  total_sent: number;
  total_delivered: number;
  total_opened: number;
  total_clicked: number;
  total_converted: number;
  total_revenue: number;
  delivery_rate: number;
  open_rate: number;
  click_rate: number;
  conversion_rate: number;
  click_to_convert: number;
}

interface WarmupTier {
  tier: number;
  name: string;
  volume: number;
  rate_per_min: number;
  status: string;
}

interface Snapshot {
  is_running: boolean;
  recent_events: SendEvent[];
  decisions: AgentDecision[];
  consultations: Consultation[];
  ab_stats: Record<string, ABVariantStats>;
  funnel: FunnelStats;
  agent_state: AgentState;
  warmup_tiers: WarmupTier[];
  config: {
    sending_domain: string;
    esp: string;
    total_records: number;
    dry_run: boolean;
  };
}

interface ManagedISPAgent {
  isp: string;
  domain: string;
  status: string;
  profiles_count: number;
  campaigns_count?: number;
  avg_engagement: number;
}

// ============================================================================
// MAIN COMPONENT
// ============================================================================

export const MissionControl: React.FC = () => {
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [loading, setLoading] = useState(true);
  const [activePanel, setActivePanel] = useState<string>('overview');
  const [consultInput, setConsultInput] = useState('');
  const [liveAgents, setLiveAgents] = useState<ManagedISPAgent[]>([]);
  const eventLogRef = useRef<HTMLDivElement>(null);
  const chatLogRef = useRef<HTMLDivElement>(null);

  const fetchSnapshot = useCallback(async () => {
    try {
      // Try live campaign data first — if an active campaign exists, use real metrics.
      // Fall back to simulation snapshot for dry-run / demo mode.
      let res = await fetch('/api/mailing/campaigns/active/live');
      let data = await res.json();

      // If no active live campaign found, fall back to simulation
      if (!data.campaign_id && !data.is_running) {
        res = await fetch('/api/mailing/simulation/snapshot');
        data = await res.json();
      }

      setSnapshot(data);
    } catch (err) {
      // Fallback to simulation if live endpoint doesn't exist yet
      try {
        const res = await fetch('/api/mailing/simulation/snapshot');
        const data = await res.json();
        setSnapshot(data);
      } catch {
        console.error('Snapshot fetch failed:', err);
      }
    } finally {
      setLoading(false);
    }
  }, []);

  const fetchLiveAgents = useCallback(async () => {
    try {
      const [sendingRes, adaptingRes] = await Promise.all([
        fetch('/api/mailing/isp-agents/managed?status=sending'),
        fetch('/api/mailing/isp-agents/managed?status=adapting'),
      ]);
      const [sendingData, adaptingData] = await Promise.all([
        sendingRes.json().catch(() => ({ agents: [] })),
        adaptingRes.json().catch(() => ({ agents: [] })),
      ]);
      const sending: ManagedISPAgent[] = sendingData.agents || sendingData || [];
      const adapting: ManagedISPAgent[] = adaptingData.agents || adaptingData || [];
      setLiveAgents([...sending, ...adapting]);
    } catch {
      // Silently ignore
    }
  }, []);

  useEffect(() => {
    fetchSnapshot();
    fetchLiveAgents();
    const interval = setInterval(() => { fetchSnapshot(); fetchLiveAgents(); }, 1500);
    return () => clearInterval(interval);
  }, [fetchSnapshot, fetchLiveAgents]);

  useEffect(() => {
    if (eventLogRef.current) eventLogRef.current.scrollTop = eventLogRef.current.scrollHeight;
  }, [snapshot?.recent_events?.length]);

  useEffect(() => {
    if (chatLogRef.current) chatLogRef.current.scrollTop = chatLogRef.current.scrollHeight;
  }, [snapshot?.consultations?.length]);

  async function startSim() {
    await fetch('/api/mailing/simulation/start', { method: 'POST' });
    fetchSnapshot();
  }

  async function stopSim() {
    await fetch('/api/mailing/simulation/stop', { method: 'POST' });
    fetchSnapshot();
  }

  async function resetSim() {
    await fetch('/api/mailing/simulation/reset', { method: 'POST' });
    fetchSnapshot();
  }

  async function sendConsult() {
    if (!consultInput.trim()) return;
    await fetch('/api/mailing/simulation/consult', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ message: consultInput }),
    });
    setConsultInput('');
    fetchSnapshot();
  }

  if (loading) {
    return (
      <div className="mc-loading">
        <FontAwesomeIcon icon={faSpinner} spin className="mc-loading-icon" />
        <p>Initializing Mission Control...</p>
      </div>
    );
  }

  if (!snapshot) return <div className="mc-loading"><p>Failed to connect</p></div>;

  // Ensure nested objects have safe defaults (merge to handle partial objects)
  const configDefaults = { sending_domain: '', esp: '', total_records: 0, dry_run: false };
  const config = { ...configDefaults, ...(snapshot.config || {}) };

  const stateDefaults: AgentState = {
    phase: 'idle', current_tier: 0, total_sent: 0, total_opens: 0, total_clicks: 0,
    total_bounces: 0, total_complaints: 0, current_open_rate: 0, current_bounce_rate: 0,
    complaint_rate: 0, inbox_rate: 0, throttle_rate: 0, is_paused: false,
  };
  const state: AgentState = { ...stateDefaults, ...(snapshot.agent_state || {}) };

  const funnelDefaults: FunnelStats = {
    total_sent: 0, total_delivered: 0, total_opened: 0, total_clicked: 0,
    total_converted: 0, total_revenue: 0, delivery_rate: 0, open_rate: 0,
    click_rate: 0, conversion_rate: 0, click_to_convert: 0,
  };
  const funnel: FunnelStats = { ...funnelDefaults, ...(snapshot.funnel || {}) };
  const abStats = Object.values(snapshot.ab_stats || {}).sort((a, b) => b.open_rate - a.open_rate);

  return (
    <div className="mc-root">
      {/* Header */}
      <div className="mc-header">
        <div className="mc-header-left">
          <FontAwesomeIcon icon={faBullseye} className="mc-header-icon" />
          <div>
            <h2 className="mc-title">
              Campaign Mission Control
              {config.dry_run && <span className="mc-badge mc-badge-warn">DRY RUN</span>}
            </h2>
            <p className="mc-subtitle">{config.sending_domain || 'No domain'} · {(config.esp || 'N/A').toUpperCase()} · {(config.total_records || 0).toLocaleString()} records</p>
          </div>
        </div>
        <div className="mc-header-right">
          {!snapshot.is_running ? (
            <>
              <button className="mc-btn mc-btn-green" onClick={startSim}>
                <FontAwesomeIcon icon={faPlay} /> Start Simulation
              </button>
              <button className="mc-btn mc-btn-ghost" onClick={resetSim}>
                <FontAwesomeIcon icon={faRotateRight} /> Reset
              </button>
            </>
          ) : (
            <button className="mc-btn mc-btn-red" onClick={stopSim}>
              <FontAwesomeIcon icon={faStop} /> Stop
            </button>
          )}
          <span className={`mc-phase mc-phase-${state.phase || 'idle'}`}>
            <FontAwesomeIcon icon={state.is_paused ? faPause : faClock} />
            {state.is_paused ? 'PAUSED' : (state.phase || 'idle').replace(/_/g, ' ').toUpperCase()}
          </span>
        </div>
      </div>

      {/* KPI Bar */}
      <div className="mc-kpi-bar">
        <KPI label="Sent" value={state.total_sent.toLocaleString()} icon={faPaperPlane} />
        <KPI label="Opens" value={state.total_opens.toLocaleString()} icon={faEye} />
        <KPI label="Open Rate" value={`${state.current_open_rate.toFixed(1)}%`} icon={faBullseye}
          className={state.current_open_rate >= 5 ? 'mc-kpi-green' : state.current_open_rate >= 3 ? 'mc-kpi-amber' : 'mc-kpi-red'} />
        <KPI label="Clicks" value={state.total_clicks.toLocaleString()} icon={faHandPointer} />
        <KPI label="Bounce%" value={`${state.current_bounce_rate.toFixed(2)}%`} icon={faCircleExclamation}
          className={state.current_bounce_rate < 3 ? 'mc-kpi-green' : 'mc-kpi-red'} />
        <KPI label="Complaint%" value={`${state.complaint_rate.toFixed(3)}%`} icon={faTriangleExclamation}
          className={state.complaint_rate < 0.08 ? 'mc-kpi-green' : 'mc-kpi-red'} />
        <KPI label="Throttle" value={`${state.throttle_rate}/min`} icon={faGauge} />
        <KPI label="Conv" value={funnel.total_converted.toLocaleString()} icon={faDollarSign} className="mc-kpi-green" />
      </div>

      {/* Main Layout */}
      <div className="mc-main">
        {/* Left Panel */}
        <div className="mc-left">
          {/* Panel Tabs */}
          <div className="mc-panel-tabs">
            {[
              { id: 'overview', label: 'Overview & Funnel', icon: faChartBar },
              { id: 'ab_test', label: 'A/B Split Test', icon: faBullseye },
              { id: 'decisions', label: 'Agent Decisions', icon: faRobot },
              { id: 'events', label: 'Live Events', icon: faBolt },
            ].map(tab => (
              <button key={tab.id} onClick={() => setActivePanel(tab.id)}
                className={`mc-panel-tab ${activePanel === tab.id ? 'active' : ''}`}>
                <FontAwesomeIcon icon={tab.icon} /> {tab.label}
              </button>
            ))}
          </div>

          {/* Overview Panel */}
          {activePanel === 'overview' && (
            <div className="mc-panel">
              {/* Funnel */}
              <div className="mc-card">
                <h3 className="mc-card-title"><FontAwesomeIcon icon={faChartLine} /> Offer Funnel — Sam's Club $50 Membership</h3>
                <div className="mc-funnel">
                  {[
                    { label: 'Sent', value: funnel.total_sent, rate: '100%', cls: 'blue' },
                    { label: 'Delivered', value: funnel.total_delivered, rate: `${funnel.delivery_rate.toFixed(1)}%`, cls: 'cyan' },
                    { label: 'Opened', value: funnel.total_opened, rate: `${funnel.open_rate.toFixed(1)}%`, cls: 'green' },
                    { label: 'Clicked', value: funnel.total_clicked, rate: `${funnel.click_rate.toFixed(2)}%`, cls: 'amber' },
                    { label: 'Converted', value: funnel.total_converted, rate: `${funnel.conversion_rate.toFixed(3)}%`, cls: 'purple' },
                  ].map((step, i, arr) => (
                    <React.Fragment key={step.label}>
                      <div className="mc-funnel-step">
                        <div className={`mc-funnel-bar mc-funnel-${step.cls}`}
                          style={{ height: `${Math.max(20, (step.value / Math.max(funnel.total_sent, 1)) * 80)}px` }}>
                          <span className="mc-funnel-value">{step.value.toLocaleString()}</span>
                        </div>
                        <span className="mc-funnel-label">{step.label}</span>
                        <span className="mc-funnel-rate">{step.rate}</span>
                      </div>
                      {i < arr.length - 1 && <span className="mc-funnel-arrow">→</span>}
                    </React.Fragment>
                  ))}
                </div>
                <div className="mc-funnel-summary">
                  <span>Revenue: <strong>${funnel.total_revenue.toFixed(2)}</strong></span>
                  <span>Click→Convert: <strong>{funnel.click_to_convert.toFixed(1)}%</strong></span>
                </div>
              </div>

              {/* Warmup Tiers */}
              <div className="mc-card">
                <h3 className="mc-card-title"><FontAwesomeIcon icon={faArrowTrendUp} /> Warmup Tier Progress</h3>
                <div className="mc-tiers">
                  {(snapshot.warmup_tiers || []).map(tier => (
                    <div key={tier.tier} className="mc-tier-row">
                      <span className={`mc-tier-badge mc-tier-${tier.status}`}>T{tier.tier}</span>
                      <span className="mc-tier-name">{tier.name}</span>
                      <div className="mc-tier-bar-track">
                        <div className={`mc-tier-bar-fill mc-tier-fill-${tier.status}`}
                          style={{ width: tier.status === 'completed' ? '100%' : tier.status === 'sending' ? '50%' : '0%' }} />
                      </div>
                      <span className="mc-tier-volume">{tier.volume.toLocaleString()}</span>
                    </div>
                  ))}
                </div>
              </div>
            </div>
          )}

          {/* A/B Test Panel */}
          {activePanel === 'ab_test' && (
            <div className="mc-panel">
              <div className="mc-card">
                <h3 className="mc-card-title">
                  <FontAwesomeIcon icon={faBullseye} /> A/B Split Test — {abStats.length} Variants
                  {abStats.some(v => v.is_winner) && <span className="mc-badge mc-badge-green">WINNER</span>}
                </h3>
                <div className="mc-ab-table-wrapper">
                  <table className="mc-ab-table">
                    <thead>
                      <tr>
                        <th>ID</th><th>Subject Line</th><th>From</th><th>Sent</th>
                        <th>Opens</th><th>Open%</th><th>Clicks</th><th>Click%</th>
                        <th>Conv</th><th>Rev</th><th>EPC</th><th>Conf</th><th></th>
                      </tr>
                    </thead>
                    <tbody>
                      {abStats.map((v, idx) => (
                        <tr key={v.variant_id} className={`${v.is_winner ? 'mc-ab-winner' : ''} ${v.is_eliminated ? 'mc-ab-eliminated' : ''} ${idx === 0 && !v.is_winner ? 'mc-ab-leading' : ''}`}>
                          <td className="mc-ab-id">{v.variant_id.replace('variant_', '')}</td>
                          <td className="mc-ab-subject">{v.subject_line}</td>
                          <td className="mc-ab-from">{v.from_name}</td>
                          <td>{v.sent.toLocaleString()}</td>
                          <td>{v.opens.toLocaleString()}</td>
                          <td className={v.open_rate >= 5 ? 'mc-val-green' : v.open_rate >= 4 ? 'mc-val-amber' : 'mc-val-red'}>
                            <strong>{v.open_rate.toFixed(1)}%</strong>
                          </td>
                          <td>{v.clicks.toLocaleString()}</td>
                          <td className="mc-val-blue">{v.click_rate.toFixed(2)}%</td>
                          <td className="mc-val-purple">{v.conversions}</td>
                          <td className="mc-val-green">${v.revenue.toFixed(0)}</td>
                          <td className="mc-val-amber">${v.epc.toFixed(2)}</td>
                          <td>{v.confidence.toFixed(0)}%</td>
                          <td>
                            {v.is_winner && <FontAwesomeIcon icon={faTrophy} className="mc-icon-gold" />}
                            {v.is_eliminated && <FontAwesomeIcon icon={faXmark} className="mc-icon-red" />}
                            {idx === 0 && !v.is_winner && !v.is_eliminated && <FontAwesomeIcon icon={faArrowTrendUp} className="mc-icon-blue" />}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>

                {/* Race Chart */}
                <div className="mc-race-chart">
                  <h4>Open Rate Race</h4>
                  {abStats.slice(0, 8).map(v => {
                    const maxRate = abStats[0]?.open_rate || 1;
                    return (
                      <div key={v.variant_id} className="mc-race-row">
                        <span className="mc-race-label">{v.variant_id.replace('variant_', '')}</span>
                        <div className="mc-race-track">
                          <div className={`mc-race-bar ${v.is_winner ? 'mc-race-winner' : v.is_eliminated ? 'mc-race-elim' : ''}`}
                            style={{ width: `${(v.open_rate / Math.max(maxRate, 0.1)) * 100}%` }} />
                        </div>
                        <span className="mc-race-value">{v.open_rate.toFixed(1)}%</span>
                      </div>
                    );
                  })}
                </div>
              </div>
            </div>
          )}

          {/* Decisions Panel */}
          {activePanel === 'decisions' && (
            <div className="mc-panel">
              <div className="mc-card">
                <h3 className="mc-card-title"><FontAwesomeIcon icon={faRobot} /> Agent Decision Log — {(snapshot.decisions || []).length} Decisions</h3>
                {(snapshot.decisions || []).length === 0 ? (
                  <div className="mc-empty"><FontAwesomeIcon icon={faRobot} /><p>Start the simulation to see decisions</p></div>
                ) : (
                  <div className="mc-decisions-list">
                    {(snapshot.decisions || []).slice().reverse().map(d => (
                      <div key={d.id} className={`mc-decision mc-decision-${d.type || 'info'}`}>
                        <div className="mc-decision-header">
                          <span className="mc-decision-type">{(d.type || 'decision').replace(/_/g, ' ')}</span>
                          <span className="mc-decision-time">{d.time ? new Date(d.time).toLocaleTimeString() : ''}</span>
                        </div>
                        <p className="mc-decision-reasoning">{d.reasoning}</p>
                        <p className="mc-decision-action">{d.action}</p>
                        {d.impact && <p className="mc-decision-impact">{d.impact}</p>}
                        {d.metrics && (
                          <div className="mc-decision-metrics">
                            {Object.entries(d.metrics).map(([k, v]) => (
                              <span key={k}>{k.replace(/_/g, ' ')}: <strong>{typeof v === 'number' && v < 1 ? v.toFixed(3) : v.toLocaleString()}</strong></span>
                            ))}
                          </div>
                        )}
                      </div>
                    ))}
                  </div>
                )}
              </div>
            </div>
          )}

          {/* Live Events Panel */}
          {activePanel === 'events' && (
            <div className="mc-panel">
              <div className="mc-card">
                <h3 className="mc-card-title">
                  <FontAwesomeIcon icon={faBolt} /> Live Event Stream
                  {snapshot.is_running && <span className="mc-live-dot" />}
                </h3>
                <div ref={eventLogRef} className="mc-event-log">
                  {(snapshot.recent_events || []).length === 0 ? (
                    <div className="mc-empty"><p>Waiting for events...</p></div>
                  ) : (
                    (snapshot.recent_events || []).map(evt => (
                      <div key={evt.id} className="mc-event-row">
                        <span className="mc-event-time">{evt.time ? new Date(evt.time).toLocaleTimeString() : ''}</span>
                        <span className={`mc-event-badge mc-event-${evt.type || 'info'}`}>{(evt.type || 'event').toUpperCase()}</span>
                        <span className="mc-event-email">{evt.email}</span>
                        <span className="mc-event-variant">{evt.variant_id}</span>
                        {evt.details && <span className="mc-event-details">{evt.details}</span>}
                      </div>
                    ))
                  )}
                </div>
              </div>
            </div>
          )}
        </div>

        {/* Right Panel: Consultation */}
        <div className="mc-right">
          {/* Chat */}
          <div className="mc-chat-card">
            <div className="mc-chat-header">
              <FontAwesomeIcon icon={faComments} /> Agent Consultation
            </div>
            <div ref={chatLogRef} className="mc-chat-log">
              {(snapshot.consultations || []).length === 0 ? (
                <div className="mc-chat-empty">
                  <FontAwesomeIcon icon={faComments} />
                  <p>Chat with the agent</p>
                  <span>Try: "slow down", "status", "pause"</span>
                </div>
              ) : (
                (snapshot.consultations || []).map(c => (
                  <div key={c.id} className={`mc-chat-msg mc-chat-${c.from}`}>
                    <span className="mc-chat-sender">{c.from === 'human' ? 'You' : 'Agent'}</span>
                    <p>{c.message}</p>
                  </div>
                ))
              )}
            </div>
            <div className="mc-chat-input">
              <input
                type="text"
                value={consultInput}
                onChange={(e) => setConsultInput(e.target.value)}
                onKeyDown={(e) => e.key === 'Enter' && sendConsult()}
                placeholder="Consult the agent..."
              />
              <button onClick={sendConsult} disabled={!consultInput.trim()}>
                <FontAwesomeIcon icon={faPaperPlane} />
              </button>
            </div>
          </div>

          {/* Live ISP Agents */}
          <div className="mc-live-agents">
            <h4><FontAwesomeIcon icon={faRobot} /> Live ISP Agents</h4>
            {liveAgents.length === 0 ? (
              <p className="mc-live-agents-empty">No ISP agents are currently active</p>
            ) : (
              <div className="mc-live-agents-list">
                {liveAgents.map(a => (
                  <div key={`${a.isp}-${a.domain}`} className="mc-live-agent-card">
                    <div className="mc-live-agent-header">
                      <span className="mc-live-agent-name">{a.isp}</span>
                      <span className={`mc-live-agent-status mc-live-st-${a.status}`}>{a.status}</span>
                    </div>
                    <span className="mc-live-agent-domain">{a.domain}</span>
                    <div className="mc-live-agent-meta">
                      <span>{a.campaigns_count ?? 0} campaign{(a.campaigns_count ?? 0) !== 1 ? 's' : ''}</span>
                      <span>{a.profiles_count} profiles</span>
                    </div>
                  </div>
                ))}
              </div>
            )}
          </div>

          {/* Quick Commands */}
          <div className="mc-quick-cmds">
            <h4>Quick Commands</h4>
            <div className="mc-cmd-grid">
              {['Slow down', 'Speed up', 'Pause', 'Resume', 'Status', 'How are conversions?'].map(cmd => (
                <button key={cmd} onClick={() => setConsultInput(cmd)} className="mc-cmd-btn">{cmd}</button>
              ))}
            </div>
          </div>

          {/* Recent Decisions Mini */}
          <div className="mc-recent-decisions">
            <h4><FontAwesomeIcon icon={faRobot} /> Recent Actions</h4>
            {(snapshot.decisions || []).slice(-5).reverse().map(d => (
              <div key={d.id} className="mc-recent-item">
                <p className="mc-recent-action">{d.action}</p>
                <span className="mc-recent-time">{d.time ? new Date(d.time).toLocaleTimeString() : ''}</span>
              </div>
            ))}
            {(snapshot.decisions || []).length === 0 && <p className="mc-recent-empty">No decisions yet</p>}
          </div>
        </div>
      </div>
    </div>
  );
};

// KPI Sub-component
function KPI({ label, value, icon, className }: { label: string; value: string; icon: any; className?: string }) {
  return (
    <div className={`mc-kpi ${className || ''}`}>
      <FontAwesomeIcon icon={icon} className="mc-kpi-icon" />
      <span className="mc-kpi-label">{label}</span>
      <span className="mc-kpi-value">{value}</span>
    </div>
  );
}

export default MissionControl;
