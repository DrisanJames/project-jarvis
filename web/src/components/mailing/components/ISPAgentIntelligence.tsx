import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faRobot, faBrain, faSyncAlt, faSpinner, faSearch,
  faEnvelope, faEye, faMousePointer, faExclamationTriangle,
  faClock, faCalendarAlt, faChartLine, faArrowDown,
  faDatabase, faNetworkWired, faShieldAlt, faCheck, faTimes,
  faStar, faMinus, faBolt, faGraduationCap,
  faLightbulb, faPaperPlane, faBullhorn, faPlay, faPause,
  faInfoCircle, faChevronDown
} from '@fortawesome/free-solid-svg-icons';
import { useAuth } from '../../../contexts/AuthContext';
import './ISPAgentIntelligence.css';

// ─── Types ───────────────────────────────────────────────────────────────────

interface LearningSources {
  sends: number;
  opens: number;
  clicks: number;
  bounces: number;
  complaints: number;
}

interface AgentKnowledge {
  optimal_send_hour: number;
  optimal_send_day: number;
  engagement_tiers: { high: number; medium: number; low: number; inactive: number };
  risk_factors: string[];
  insights: string[];
  recent_openers: number;
  recent_clickers: number;
}

interface ISPAgent {
  isp: string;
  isp_key: string;
  domain: string;
  status: string;
  profiles_count: number;
  total_sends: number;
  total_opens: number;
  total_clicks: number;
  total_bounces: number;
  total_complaints: number;
  avg_engagement: number;
  avg_open_rate: number;
  avg_click_rate: number;
  data_points_total: number;
  last_learning_at: string;
  first_learning_at: string;
  learning_days: number;
  learning_frequency_hours: number;
  learning_sources: LearningSources;
  knowledge: AgentKnowledge;
}

interface AgentSummary {
  total_agents: number;
  active_agents: number;
  total_profiles: number;
  total_data_points: number;
  last_system_learning: string;
}

interface ManagedAgent {
  id: string;
  isp: string;
  domain: string;
  status: string;
  config: Record<string, unknown>;
  knowledge: Record<string, unknown>;
  total_campaigns: number;
  total_sends: number;
  total_opens: number;
  total_clicks: number;
  total_bounces: number;
  total_complaints: number;
  avg_engagement: number;
  created_at: string;
  updated_at: string;
  last_active_at: string;
  active_campaigns: number;
  profile_count: number;
  avg_profile_engagement: number;
  last_learning_at: string;
}

interface ManagedAgentSummary {
  total_agents: number;
  active_agents: number;
  dormant_agents: number;
  learning_agents: number;
  total_sends: number;
  total_opens: number;
  total_clicks: number;
  avg_engagement: number;
  top_performing_isp: string;
}

interface AgentFeedEntry {
  id: string;
  campaign_id: string;
  email_hash_short: string;
  classification: string;
  content_strategy: string;
  priority: number;
  reasoning: Record<string, any>;
  executed: boolean;
  executed_at: string | null;
  result: string | null;
  created_at: string;
}

interface AgentActivity {
  agent_id: string;
  total_decisions: number;
  by_classification: Record<string, number>;
  by_result: Record<string, number>;
  by_content_strategy: Record<string, number>;
  execution_rate: number;
  campaigns_active: number;
  latest_activity_at: string | null;
  recent_feed: AgentFeedEntry[];
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

const orgFetch = async (url: string, orgId?: string) => {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  if (orgId) headers['X-Organization-ID'] = orgId;
  return fetch(url, { headers });
};

const getISPColor = (isp: string): string => {
  switch (isp) {
    case 'Gmail': return '#ea4335';
    case 'Yahoo': return '#7b1fa2';
    case 'Microsoft': return '#0078d4';
    case 'AOL': return '#ff6600';
    case 'Apple': return '#a2aaad';
    case 'Comcast': return '#e60000';
    case 'AT&T': return '#009fdb';
    case 'Cox': return '#f26522';
    case 'Charter': case 'Charter/Spectrum': return '#0099d6';
    case 'Proton': return '#6d4aff';
    default: return '#64748b';
  }
};

const getISPIcon = (isp: string): string => {
  switch (isp) {
    case 'Gmail': return '📧';
    case 'Yahoo': return '🟣';
    case 'Microsoft': return '🔷';
    case 'AOL': return '🔶';
    case 'Apple': return '🍎';
    case 'Comcast': return '📡';
    case 'AT&T': return '📶';
    case 'Cox': return '🔌';
    case 'Charter': case 'Charter/Spectrum': return '📺';
    default: return '🌐';
  }
};

const getStatusColor = (status: string): string => {
  switch (status) {
    case 'active': return '#10b981';
    case 'idle': return '#f59e0b';
    case 'sleeping': return '#6366f1';
    case 'dormant': return '#6b7280';
    default: return '#6b7280';
  }
};

const getStatusLabel = (status: string): string => {
  switch (status) {
    case 'active': return 'Actively Learning';
    case 'idle': return 'Idle';
    case 'sleeping': return 'Sleeping';
    case 'dormant': return 'Dormant';
    default: return 'Unknown';
  }
};

const getManagedStatusColor = (status: string): string => {
  switch (status) {
    case 'spawned': return '#3b82f6';
    case 'learning': return '#f59e0b';
    case 'sending': return '#10b981';
    case 'adapting': return '#8b5cf6';
    case 'complete': return '#6b7280';
    case 'dormant': return '#4b5563';
    default: return '#6b7280';
  }
};

const getManagedStatusLabel = (status: string): string => {
  switch (status) {
    case 'spawned': return 'Spawned';
    case 'learning': return 'Learning';
    case 'sending': return 'Sending';
    case 'adapting': return 'Adapting';
    case 'complete': return 'Complete';
    case 'dormant': return 'Dormant';
    default: return 'Unknown';
  }
};

const timeAgo = (dateStr?: string): string => {
  if (!dateStr) return 'Never';
  const diff = Date.now() - new Date(dateStr).getTime();
  const mins = Math.floor(diff / 60000);
  if (mins < 1) return 'Just now';
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;
  const months = Math.floor(days / 30);
  return `${months}mo ago`;
};

const formatNumber = (n: number): string => {
  if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
  if (n >= 1000) return (n / 1000).toFixed(1) + 'K';
  return n.toLocaleString();
};

const dayNames = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];

// ─── Knowledge Depth Score ───────────────────────────────────────────────────
const getKnowledgeDepth = (agent: ISPAgent): { score: number; label: string; color: string } => {
  let score = 0;
  // Points for data volume
  if (agent.data_points_total > 100) score += 25;
  else if (agent.data_points_total > 50) score += 20;
  else if (agent.data_points_total > 10) score += 15;
  else if (agent.data_points_total > 0) score += 5;
  // Points for diversity of signals
  if (agent.learning_sources.opens > 0) score += 15;
  if (agent.learning_sources.clicks > 0) score += 15;
  if (agent.learning_sources.bounces > 0) score += 5;
  // Points for recency
  if (agent.status === 'active') score += 20;
  else if (agent.status === 'idle') score += 10;
  // Points for learning duration
  if (agent.learning_days > 30) score += 20;
  else if (agent.learning_days > 7) score += 15;
  else if (agent.learning_days > 1) score += 10;
  else score += 5;

  score = Math.min(100, score);

  let label = 'Novice';
  let color = '#6b7280';
  if (score >= 80) { label = 'Expert'; color = '#10b981'; }
  else if (score >= 60) { label = 'Proficient'; color = '#3b82f6'; }
  else if (score >= 40) { label = 'Learning'; color = '#f59e0b'; }
  else if (score >= 20) { label = 'Beginner'; color = '#ef4444'; }

  return { score, label, color };
};

// ─── Main Component ──────────────────────────────────────────────────────────

export const ISPAgentIntelligence: React.FC = () => {
  const { organization } = useAuth();
  const [agents, setAgents] = useState<ISPAgent[]>([]);
  const [summary, setSummary] = useState<AgentSummary | null>(null);
  const [loading, setLoading] = useState(true);
  const [selectedAgent, setSelectedAgent] = useState<ISPAgent | null>(null);
  const [searchFilter, setSearchFilter] = useState('');
  const [managedAgents, setManagedAgents] = useState<ManagedAgent[]>([]);
  const [managedSummary, setManagedSummary] = useState<ManagedAgentSummary | null>(null);
  const [loadingManaged, setLoadingManaged] = useState(true);
  const [triggeringLearn, setTriggeringLearn] = useState<string | null>(null);
  const [togglingStatus, setTogglingStatus] = useState<string | null>(null);
  const [expandedAgent, setExpandedAgent] = useState<string | null>(null);
  const [agentActivity, setAgentActivity] = useState<AgentActivity | null>(null);
  const [loadingActivity, setLoadingActivity] = useState(false);
  const [feedFilter, setFeedFilter] = useState<string>('');

  const fetchAgents = useCallback(async () => {
    setLoading(true);
    try {
      const res = await orgFetch('/api/mailing/isp-agents', organization?.id);
      const data = await res.json();
      setAgents(data.agents || []);
      setSummary(data.summary || null);
    } catch (err) {
      console.error('Failed to load ISP agents:', err);
    } finally {
      setLoading(false);
    }
  }, [organization]);

  const fetchManagedAgents = useCallback(async () => {
    setLoadingManaged(true);
    try {
      const [agentsRes, summaryRes] = await Promise.all([
        orgFetch('/api/mailing/isp-agents/managed', organization?.id),
        orgFetch('/api/mailing/isp-agents/summary', organization?.id),
      ]);
      const agentsData = await agentsRes.json();
      const summaryData = await summaryRes.json();
      setManagedAgents(agentsData.agents || []);
      setManagedSummary(summaryData.summary || null);
    } catch (err) {
      console.error('Failed to load managed agents:', err);
    } finally {
      setLoadingManaged(false);
    }
  }, [organization]);

  const triggerLearn = useCallback(async (agentId: string) => {
    setTriggeringLearn(agentId);
    try {
      const headers: Record<string, string> = { 'Content-Type': 'application/json' };
      if (organization?.id) headers['X-Organization-ID'] = organization.id;
      await fetch(`/api/mailing/isp-agents/managed/${agentId}/learn`, { method: 'POST', headers });
      await fetchManagedAgents();
    } catch (err) {
      console.error('Failed to trigger learning:', err);
    } finally {
      setTriggeringLearn(null);
    }
  }, [organization, fetchManagedAgents]);

  const toggleAgentStatus = useCallback(async (agentId: string, currentStatus: string) => {
    setTogglingStatus(agentId);
    const newStatus = currentStatus === 'dormant' ? 'learning' : 'dormant';
    try {
      const headers: Record<string, string> = { 'Content-Type': 'application/json' };
      if (organization?.id) headers['X-Organization-ID'] = organization.id;
      await fetch(`/api/mailing/isp-agents/managed/${agentId}/status`, {
        method: 'PATCH',
        headers,
        body: JSON.stringify({ status: newStatus }),
      });
      await fetchManagedAgents();
    } catch (err) {
      console.error('Failed to toggle agent status:', err);
    } finally {
      setTogglingStatus(null);
    }
  }, [organization, fetchManagedAgents]);

  const fetchAgentActivity = useCallback(async (agentId: string) => {
    setLoadingActivity(true);
    try {
      const res = await orgFetch(`/api/mailing/isp-agents/managed/${agentId}/activity`, organization?.id);
      const data = await res.json();
      setAgentActivity(data);
    } catch (err) {
      console.error('Failed to fetch agent activity:', err);
    } finally {
      setLoadingActivity(false);
    }
  }, [organization]);

  useEffect(() => {
    fetchAgents();
    fetchManagedAgents();
  }, [fetchAgents, fetchManagedAgents]);

  useEffect(() => {
    if (!expandedAgent) return;
    const agent = managedAgents.find(a => a.id === expandedAgent);
    const interval = (agent?.status === 'sending' || agent?.status === 'adapting') ? 5000 : 30000;
    fetchAgentActivity(expandedAgent);
    const timer = setInterval(() => fetchAgentActivity(expandedAgent), interval);
    return () => clearInterval(timer);
  }, [expandedAgent, managedAgents, fetchAgentActivity]);

  const filteredAgents = agents.filter(a =>
    !searchFilter ||
    a.isp.toLowerCase().includes(searchFilter.toLowerCase()) ||
    a.domain.toLowerCase().includes(searchFilter.toLowerCase())
  );

  // Sort: active first, then by data points
  const sortedAgents = [...filteredAgents].sort((a, b) => {
    const statusOrder: Record<string, number> = { active: 0, idle: 1, sleeping: 2, dormant: 3 };
    const diff = (statusOrder[a.status] ?? 4) - (statusOrder[b.status] ?? 4);
    if (diff !== 0) return diff;
    return b.data_points_total - a.data_points_total;
  });

  return (
    <div className="ia-container">
      {/* ─── Header ─────────────────────────────────────────────────────── */}
      <div className="ia-header">
        <div className="ia-header-left">
          <div className="ia-header-icon">
            <FontAwesomeIcon icon={faRobot} />
            <span className="ia-pulse" />
          </div>
          <div>
            <h1>ISP Agent Intelligence</h1>
            <p>Specialized AI agents learning the behavior of every ISP</p>
          </div>
        </div>
        <button className="ia-refresh-btn" onClick={() => { fetchAgents(); fetchManagedAgents(); }} disabled={loading || loadingManaged}>
          <FontAwesomeIcon icon={faSyncAlt} spin={loading || loadingManaged} /> Refresh
        </button>
      </div>

      {/* ─── Summary Stats ──────────────────────────────────────────────── */}
      {summary && (
        <div className="ia-summary-bar">
          <div className="ia-summary-card">
            <div className="ia-summary-icon" style={{ background: 'rgba(99, 102, 241, 0.15)', color: '#818cf8' }}>
              <FontAwesomeIcon icon={faRobot} />
            </div>
            <div className="ia-summary-body">
              <span className="ia-summary-value">{summary.total_agents}</span>
              <span className="ia-summary-label">Total Agents</span>
            </div>
          </div>
          <div className="ia-summary-card">
            <div className="ia-summary-icon" style={{ background: 'rgba(16, 185, 129, 0.15)', color: '#10b981' }}>
              <FontAwesomeIcon icon={faBolt} />
            </div>
            <div className="ia-summary-body">
              <span className="ia-summary-value">{summary.active_agents}</span>
              <span className="ia-summary-label">Active Agents</span>
            </div>
          </div>
          <div className="ia-summary-card">
            <div className="ia-summary-icon" style={{ background: 'rgba(245, 158, 11, 0.15)', color: '#f59e0b' }}>
              <FontAwesomeIcon icon={faBrain} />
            </div>
            <div className="ia-summary-body">
              <span className="ia-summary-value">{formatNumber(summary.total_profiles)}</span>
              <span className="ia-summary-label">Profiles Built</span>
            </div>
          </div>
          <div className="ia-summary-card">
            <div className="ia-summary-icon" style={{ background: 'rgba(59, 130, 246, 0.15)', color: '#3b82f6' }}>
              <FontAwesomeIcon icon={faDatabase} />
            </div>
            <div className="ia-summary-body">
              <span className="ia-summary-value">{formatNumber(summary.total_data_points)}</span>
              <span className="ia-summary-label">Data Points</span>
            </div>
          </div>
          <div className="ia-summary-card">
            <div className="ia-summary-icon" style={{ background: 'rgba(236, 72, 153, 0.15)', color: '#ec4899' }}>
              <FontAwesomeIcon icon={faClock} />
            </div>
            <div className="ia-summary-body">
              <span className="ia-summary-value">{timeAgo(summary.last_system_learning)}</span>
              <span className="ia-summary-label">Last Learning</span>
            </div>
          </div>
        </div>
      )}

      {/* ═══ Managed / Spawned Agents Section ═══════════════════════════ */}
      <div className="ia-managed-section">
        <div className="ia-managed-section-header">
          <div className="ia-managed-section-title">
            <FontAwesomeIcon icon={faNetworkWired} />
            <h2>Managed ISP Agents</h2>
          </div>
          <span className="ia-managed-section-subtitle">Persistent agents spawned by campaigns</span>
        </div>

        {/* Managed Summary Bar */}
        {managedSummary && (
          <div className="ia-managed-summary-bar">
            <div className="ia-managed-summary-card">
              <div className="ia-managed-summary-icon" style={{ background: 'rgba(99, 102, 241, 0.15)', color: '#818cf8' }}>
                <FontAwesomeIcon icon={faRobot} />
              </div>
              <div className="ia-managed-summary-body">
                <span className="ia-managed-summary-value">{managedSummary.total_agents}</span>
                <span className="ia-managed-summary-label">Total Agents</span>
              </div>
            </div>
            <div className="ia-managed-summary-card">
              <div className="ia-managed-summary-icon" style={{ background: 'rgba(16, 185, 129, 0.15)', color: '#10b981' }}>
                <FontAwesomeIcon icon={faBolt} />
              </div>
              <div className="ia-managed-summary-body">
                <span className="ia-managed-summary-value">{managedSummary.active_agents}</span>
                <span className="ia-managed-summary-label">Active</span>
              </div>
            </div>
            <div className="ia-managed-summary-card">
              <div className="ia-managed-summary-icon" style={{ background: 'rgba(107, 114, 128, 0.15)', color: '#6b7280' }}>
                <FontAwesomeIcon icon={faPause} />
              </div>
              <div className="ia-managed-summary-body">
                <span className="ia-managed-summary-value">{managedSummary.dormant_agents}</span>
                <span className="ia-managed-summary-label">Dormant</span>
              </div>
            </div>
            <div className="ia-managed-summary-card">
              <div className="ia-managed-summary-icon" style={{ background: 'rgba(245, 158, 11, 0.15)', color: '#f59e0b' }}>
                <FontAwesomeIcon icon={faGraduationCap} />
              </div>
              <div className="ia-managed-summary-body">
                <span className="ia-managed-summary-value">{managedSummary.learning_agents}</span>
                <span className="ia-managed-summary-label">Learning</span>
              </div>
            </div>
            <div className="ia-managed-summary-card">
              <div className="ia-managed-summary-icon" style={{ background: 'rgba(59, 130, 246, 0.15)', color: '#3b82f6' }}>
                <FontAwesomeIcon icon={faPaperPlane} />
              </div>
              <div className="ia-managed-summary-body">
                <span className="ia-managed-summary-value">{formatNumber(managedSummary.total_sends)}</span>
                <span className="ia-managed-summary-label">Total Sends</span>
              </div>
            </div>
            <div className="ia-managed-summary-card">
              <div className="ia-managed-summary-icon" style={{ background: 'rgba(236, 72, 153, 0.15)', color: '#ec4899' }}>
                <FontAwesomeIcon icon={faChartLine} />
              </div>
              <div className="ia-managed-summary-body">
                <span className="ia-managed-summary-value">{managedSummary.avg_engagement.toFixed(1)}</span>
                <span className="ia-managed-summary-label">Avg Engagement</span>
              </div>
            </div>
          </div>
        )}

        {/* Managed Agents Grid */}
        {loadingManaged && managedAgents.length === 0 ? (
          <div className="ia-managed-loading">
            <FontAwesomeIcon icon={faSpinner} spin />
            <span>Loading managed agents...</span>
          </div>
        ) : managedAgents.length === 0 ? (
          <div className="ia-managed-empty">
            <div className="ia-managed-empty-icon">
              <FontAwesomeIcon icon={faInfoCircle} />
            </div>
            <div className="ia-managed-empty-body">
              <h4>No ISP agents have been spawned yet</h4>
              <p>Create a campaign using the AI Agent Wizard to deploy your first agents.</p>
            </div>
          </div>
        ) : (
          <div className="ia-managed-grid">
            {managedAgents.map(agent => {
              const knowledge = agent.knowledge || {};
              const optimalHours = knowledge.optimal_send_hours as number[] | undefined;
              const riskLevel = knowledge.risk_level as string | undefined;
              const bounceRate = knowledge.bounce_rate as number | undefined;
              const statusColor = getManagedStatusColor(agent.status);

              return (
                <div
                  key={agent.id}
                  className={`ia-managed-card ${expandedAgent === agent.id ? 'ia-managed-card-expanded' : ''}`}
                  onClick={() => {
                    if (expandedAgent === agent.id) {
                      setExpandedAgent(null);
                      setAgentActivity(null);
                    } else {
                      setExpandedAgent(agent.id);
                    }
                  }}
                >
                  {/* Card top accent */}
                  <div className="ia-managed-card-accent" style={{ background: `linear-gradient(90deg, ${statusColor}66, ${statusColor}22)` }} />

                  {/* Header: ISP + status */}
                  <div className="ia-managed-card-header">
                    <div className="ia-managed-card-isp">
                      <span className="ia-managed-isp-emoji">{getISPIcon(agent.isp)}</span>
                      <div>
                        <h3>{agent.isp}</h3>
                        <span className="ia-managed-domain">{agent.domain}</span>
                      </div>
                    </div>
                    <div className="ia-managed-header-right">
                      <span
                        className="ia-managed-status-badge"
                        style={{ background: statusColor + '22', color: statusColor, borderColor: statusColor + '44' }}
                      >
                        <span className="ia-managed-status-dot" style={{ background: statusColor }} />
                        {getManagedStatusLabel(agent.status)}
                      </span>
                      <span className={`ia-managed-expand-icon ${expandedAgent === agent.id ? 'ia-managed-expand-icon-open' : ''}`}>
                        <FontAwesomeIcon icon={faChevronDown} />
                      </span>
                    </div>
                  </div>

                  {/* Metrics row */}
                  <div className="ia-managed-metrics">
                    <div className="ia-managed-metric">
                      <FontAwesomeIcon icon={faBullhorn} />
                      <span className="ia-managed-metric-val">{agent.total_campaigns}</span>
                      <span className="ia-managed-metric-lbl">Campaigns</span>
                    </div>
                    <div className="ia-managed-metric">
                      <FontAwesomeIcon icon={faPaperPlane} />
                      <span className="ia-managed-metric-val">{formatNumber(agent.total_sends)}</span>
                      <span className="ia-managed-metric-lbl">Sends</span>
                    </div>
                    <div className="ia-managed-metric">
                      <FontAwesomeIcon icon={faEye} />
                      <span className="ia-managed-metric-val">{formatNumber(agent.total_opens)}</span>
                      <span className="ia-managed-metric-lbl">Opens</span>
                    </div>
                    <div className="ia-managed-metric">
                      <FontAwesomeIcon icon={faMousePointer} />
                      <span className="ia-managed-metric-val">{formatNumber(agent.total_clicks)}</span>
                      <span className="ia-managed-metric-lbl">Clicks</span>
                    </div>
                  </div>

                  {/* Engagement bar */}
                  <div className="ia-managed-engagement">
                    <div className="ia-managed-engagement-header">
                      <span>Engagement Score</span>
                      <span className="ia-managed-engagement-val">{agent.avg_engagement.toFixed(1)}</span>
                    </div>
                    <div className="ia-managed-engagement-bg">
                      <div
                        className="ia-managed-engagement-fill"
                        style={{
                          width: `${Math.min(agent.avg_engagement, 100)}%`,
                          background: agent.avg_engagement >= 60 ? '#10b981' : agent.avg_engagement >= 30 ? '#f59e0b' : '#ef4444'
                        }}
                      />
                    </div>
                  </div>

                  {/* Knowledge insights */}
                  {(optimalHours || riskLevel || bounceRate !== undefined) && (
                    <div className="ia-managed-knowledge">
                      <span className="ia-managed-knowledge-title">
                        <FontAwesomeIcon icon={faLightbulb} /> Knowledge
                      </span>
                      <div className="ia-managed-knowledge-items">
                        {optimalHours && optimalHours.length > 0 && (
                          <span className="ia-managed-knowledge-item">
                            <FontAwesomeIcon icon={faClock} /> Best hours: {optimalHours.slice(0, 3).map(h => `${h}:00`).join(', ')}
                          </span>
                        )}
                        {riskLevel && (
                          <span className={`ia-managed-knowledge-item ia-managed-risk-${riskLevel}`}>
                            <FontAwesomeIcon icon={faShieldAlt} /> Risk: {riskLevel}
                          </span>
                        )}
                        {bounceRate !== undefined && (
                          <span className="ia-managed-knowledge-item">
                            <FontAwesomeIcon icon={faExclamationTriangle} /> Bounce: {bounceRate.toFixed(1)}%
                          </span>
                        )}
                      </div>
                    </div>
                  )}

                  {/* Footer: active campaigns + last active */}
                  <div className="ia-managed-card-footer">
                    <div className="ia-managed-footer-info">
                      <span className="ia-managed-footer-item">
                        <FontAwesomeIcon icon={faBullhorn} /> {agent.active_campaigns} active campaign{agent.active_campaigns !== 1 ? 's' : ''}
                      </span>
                      <span className="ia-managed-footer-item">
                        <FontAwesomeIcon icon={faClock} /> {timeAgo(agent.last_active_at)}
                      </span>
                    </div>
                    <div className="ia-managed-card-actions">
                      <button
                        className="ia-managed-btn ia-managed-btn-learn"
                        onClick={(e) => { e.stopPropagation(); triggerLearn(agent.id); }}
                        disabled={triggeringLearn === agent.id}
                        title="Trigger Learning Cycle"
                      >
                        {triggeringLearn === agent.id ? (
                          <FontAwesomeIcon icon={faSpinner} spin />
                        ) : (
                          <FontAwesomeIcon icon={faGraduationCap} />
                        )}
                        Learn
                      </button>
                      <button
                        className={`ia-managed-btn ${agent.status === 'dormant' ? 'ia-managed-btn-activate' : 'ia-managed-btn-dormant'}`}
                        onClick={(e) => { e.stopPropagation(); toggleAgentStatus(agent.id, agent.status); }}
                        disabled={togglingStatus === agent.id}
                        title={agent.status === 'dormant' ? 'Activate Agent' : 'Set Dormant'}
                      >
                        {togglingStatus === agent.id ? (
                          <FontAwesomeIcon icon={faSpinner} spin />
                        ) : agent.status === 'dormant' ? (
                          <FontAwesomeIcon icon={faPlay} />
                        ) : (
                          <FontAwesomeIcon icon={faPause} />
                        )}
                        {agent.status === 'dormant' ? 'Activate' : 'Dormant'}
                      </button>
                    </div>
                  </div>

                  {/* Expanded Activity Feed */}
                  {expandedAgent === agent.id && (
                    <div className="ia-feed-panel">
                      {loadingActivity && !agentActivity ? (
                        <div className="ia-feed-loading">
                          <FontAwesomeIcon icon={faSpinner} spin /> Loading activity...
                        </div>
                      ) : agentActivity ? (
                        <>
                          {/* Decision Breakdown Bar */}
                          <div className="ia-feed-breakdown">
                            <div className="ia-feed-breakdown-title">Decision Breakdown</div>
                            <div className="ia-feed-breakdown-bar">
                              {Object.entries(agentActivity.by_classification).map(([cls, count]) => {
                                const total = agentActivity.total_decisions || 1;
                                const pct = (count / total) * 100;
                                const color = cls === 'send_now' ? '#10b981' : cls === 'send_later' ? '#3b82f6' : cls === 'defer' ? '#f59e0b' : '#ef4444';
                                return pct > 0 ? (
                                  <div key={cls} className="ia-feed-bar-seg" style={{ width: `${pct}%`, background: color }} title={`${cls}: ${count} (${pct.toFixed(1)}%)`} />
                                ) : null;
                              })}
                            </div>
                            <div className="ia-feed-breakdown-legend">
                              <span className="ia-feed-legend-item"><span className="ia-feed-legend-dot" style={{background:'#10b981'}} /> Send Now: {agentActivity.by_classification.send_now || 0}</span>
                              <span className="ia-feed-legend-item"><span className="ia-feed-legend-dot" style={{background:'#3b82f6'}} /> Later: {agentActivity.by_classification.send_later || 0}</span>
                              <span className="ia-feed-legend-item"><span className="ia-feed-legend-dot" style={{background:'#f59e0b'}} /> Defer: {agentActivity.by_classification.defer || 0}</span>
                              <span className="ia-feed-legend-item"><span className="ia-feed-legend-dot" style={{background:'#ef4444'}} /> Suppress: {agentActivity.by_classification.suppress || 0}</span>
                            </div>
                          </div>

                          {/* Result Stats */}
                          <div className="ia-feed-results">
                            <div className="ia-feed-result-stat">
                              <span className="ia-feed-result-val">{agentActivity.total_decisions.toLocaleString()}</span>
                              <span className="ia-feed-result-lbl">Total Decisions</span>
                            </div>
                            <div className="ia-feed-result-stat">
                              <span className="ia-feed-result-val">{agentActivity.execution_rate.toFixed(1)}%</span>
                              <span className="ia-feed-result-lbl">Executed</span>
                            </div>
                            <div className="ia-feed-result-stat">
                              <span className="ia-feed-result-val">{(agentActivity.by_result.opened || 0).toLocaleString()}</span>
                              <span className="ia-feed-result-lbl">Opens</span>
                            </div>
                            <div className="ia-feed-result-stat">
                              <span className="ia-feed-result-val">{(agentActivity.by_result.clicked || 0).toLocaleString()}</span>
                              <span className="ia-feed-result-lbl">Clicks</span>
                            </div>
                            <div className="ia-feed-result-stat">
                              <span className="ia-feed-result-val">{(agentActivity.by_result.bounced || 0).toLocaleString()}</span>
                              <span className="ia-feed-result-lbl">Bounced</span>
                            </div>
                          </div>

                          {/* Filter chips */}
                          <div className="ia-feed-filters">
                            <span className="ia-feed-filter-label">Filter:</span>
                            {['', 'send_now', 'send_later', 'defer', 'suppress'].map(f => (
                              <button key={f} className={`ia-feed-filter-chip ${feedFilter === f ? 'active' : ''}`}
                                onClick={(e) => { e.stopPropagation(); setFeedFilter(f); }}>
                                {f === '' ? 'All' : f.replace('_', ' ')}
                              </button>
                            ))}
                          </div>

                          {/* Live Decision Feed */}
                          <div className="ia-feed-list">
                            <div className="ia-feed-list-header">
                              <span>Recent Decisions</span>
                              <span className="ia-feed-live-dot" />
                            </div>
                            {(agentActivity.recent_feed || [])
                              .filter(entry => !feedFilter || entry.classification === feedFilter)
                              .map(entry => (
                              <div key={entry.id} className="ia-feed-entry">
                                <div className="ia-feed-entry-time">{timeAgo(entry.created_at)}</div>
                                <div className="ia-feed-entry-body">
                                  <span className={`ia-feed-cls-badge ia-feed-cls-${entry.classification}`}>
                                    {entry.classification.replace('_', ' ')}
                                  </span>
                                  {entry.content_strategy && (
                                    <span className="ia-feed-strategy-badge">
                                      {entry.content_strategy.replace(/_/g, ' ')}
                                    </span>
                                  )}
                                  <span className="ia-feed-priority" title={`Priority: ${entry.priority}`}>
                                    P{entry.priority}
                                  </span>
                                  <span className="ia-feed-hash">#{entry.email_hash_short}</span>
                                </div>
                                <div className="ia-feed-entry-result">
                                  {entry.executed ? (
                                    <span className={`ia-feed-result-badge ia-feed-result-${entry.result || 'sent'}`}>
                                      {entry.result || 'sent'}
                                    </span>
                                  ) : (
                                    <span className="ia-feed-result-badge ia-feed-result-pending">pending</span>
                                  )}
                                </div>
                              </div>
                            ))}
                            {(agentActivity.recent_feed || []).length === 0 && (
                              <div className="ia-feed-empty">No decisions recorded yet for this agent.</div>
                            )}
                          </div>
                        </>
                      ) : null}
                    </div>
                  )}
                </div>
              );
            })}
          </div>
        )}
      </div>

      {/* ═══ Computed ISP Intelligence Section ════════════════════════════ */}
      <div className="ia-computed-section-header">
        <FontAwesomeIcon icon={faBrain} />
        <h2>Computed ISP Intelligence</h2>
      </div>

      {/* ─── Search Bar ─────────────────────────────────────────────────── */}
      <div className="ia-search-bar">
        <div className="ia-search-wrap">
          <FontAwesomeIcon icon={faSearch} className="ia-search-icon" />
          <input
            type="text"
            placeholder="Search by ISP or domain..."
            value={searchFilter}
            onChange={(e) => setSearchFilter(e.target.value)}
            className="ia-search-input"
          />
        </div>
        <div className="ia-agent-count">
          {sortedAgents.length} agent{sortedAgents.length !== 1 ? 's' : ''}
        </div>
      </div>

      {/* ─── Main Content ───────────────────────────────────────────────── */}
      <div className="ia-main">
        {loading && agents.length === 0 ? (
          <div className="ia-loading">
            <FontAwesomeIcon icon={faSpinner} spin size="2x" />
            <p>Initializing ISP agents...</p>
          </div>
        ) : sortedAgents.length === 0 ? (
          <div className="ia-empty">
            <FontAwesomeIcon icon={faRobot} size="3x" />
            <h3>No Agents Found</h3>
            <p>AI agents are created as email data flows through the system. Send emails to start building ISP-specific intelligence.</p>
          </div>
        ) : (
          <div className="ia-agents-grid">
            {sortedAgents.map(agent => {
              const kd = getKnowledgeDepth(agent);
              return (
                <div
                  key={agent.domain}
                  className={`ia-agent-card ${selectedAgent?.domain === agent.domain ? 'ia-agent-active' : ''}`}
                  onClick={() => setSelectedAgent(selectedAgent?.domain === agent.domain ? null : agent)}
                >
                  {/* Agent Header */}
                  <div className="ia-agent-header">
                    <div className="ia-agent-isp">
                      <span className="ia-isp-emoji">{getISPIcon(agent.isp)}</span>
                      <div>
                        <h3>{agent.isp} Agent</h3>
                        <span className="ia-domain-label">{agent.domain}</span>
                      </div>
                    </div>
                    <div className="ia-agent-status">
                      <span className="ia-status-dot" style={{ background: getStatusColor(agent.status) }} />
                      <span className="ia-status-text" style={{ color: getStatusColor(agent.status) }}>
                        {getStatusLabel(agent.status)}
                      </span>
                    </div>
                  </div>

                  {/* Knowledge Depth */}
                  <div className="ia-knowledge-bar">
                    <div className="ia-knowledge-header">
                      <span className="ia-knowledge-label">
                        <FontAwesomeIcon icon={faGraduationCap} /> Knowledge Depth
                      </span>
                      <span className="ia-knowledge-level" style={{ color: kd.color }}>{kd.label}</span>
                    </div>
                    <div className="ia-progress-bg">
                      <div
                        className="ia-progress-fill"
                        style={{ width: `${kd.score}%`, background: `linear-gradient(90deg, ${kd.color}88, ${kd.color})` }}
                      />
                    </div>
                    <span className="ia-knowledge-score">{kd.score}/100</span>
                  </div>

                  {/* Key Metrics Row */}
                  <div className="ia-metrics-row">
                    <div className="ia-mini-metric">
                      <FontAwesomeIcon icon={faBrain} />
                      <span className="ia-mm-val">{agent.profiles_count}</span>
                      <span className="ia-mm-lbl">Profiles</span>
                    </div>
                    <div className="ia-mini-metric">
                      <FontAwesomeIcon icon={faDatabase} />
                      <span className="ia-mm-val">{formatNumber(agent.data_points_total)}</span>
                      <span className="ia-mm-lbl">Data Pts</span>
                    </div>
                    <div className="ia-mini-metric">
                      <FontAwesomeIcon icon={faEye} />
                      <span className="ia-mm-val">{agent.avg_open_rate.toFixed(1)}%</span>
                      <span className="ia-mm-lbl">Open Rate</span>
                    </div>
                    <div className="ia-mini-metric">
                      <FontAwesomeIcon icon={faChartLine} />
                      <span className="ia-mm-val">{agent.avg_engagement.toFixed(0)}</span>
                      <span className="ia-mm-lbl">Eng. Score</span>
                    </div>
                  </div>

                  {/* Learning Activity */}
                  <div className="ia-learning-row">
                    <div className="ia-learn-item">
                      <FontAwesomeIcon icon={faClock} />
                      <span>Last learned: <strong>{timeAgo(agent.last_learning_at)}</strong></span>
                    </div>
                    <div className="ia-learn-item">
                      <FontAwesomeIcon icon={faCalendarAlt} />
                      <span>Learning for: <strong>{agent.learning_days}d</strong></span>
                    </div>
                  </div>

                  {/* Learning Sources Mini */}
                  <div className="ia-sources-mini">
                    <span className="ia-source-tag ia-src-sends">
                      <FontAwesomeIcon icon={faEnvelope} /> {formatNumber(agent.learning_sources.sends)} sends
                    </span>
                    <span className="ia-source-tag ia-src-opens">
                      <FontAwesomeIcon icon={faEye} /> {agent.learning_sources.opens} opens
                    </span>
                    <span className="ia-source-tag ia-src-clicks">
                      <FontAwesomeIcon icon={faMousePointer} /> {agent.learning_sources.clicks} clicks
                    </span>
                    {agent.learning_sources.bounces > 0 && (
                      <span className="ia-source-tag ia-src-bounces">
                        <FontAwesomeIcon icon={faExclamationTriangle} /> {agent.learning_sources.bounces} bounces
                      </span>
                    )}
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </div>

      {/* ─── Detail Slide-over ──────────────────────────────────────────── */}
      {selectedAgent && (
        <div className="ia-detail-panel">
          <button className="ia-detail-close" onClick={() => setSelectedAgent(null)}>
            <FontAwesomeIcon icon={faTimes} />
          </button>

          {/* Detail Header */}
          <div className="ia-detail-header">
            <span className="ia-detail-emoji">{getISPIcon(selectedAgent.isp)}</span>
            <div className="ia-detail-identity">
              <h2>{selectedAgent.isp} Agent</h2>
              <div className="ia-detail-badges">
                <span className="ia-isp-badge" style={{ background: getISPColor(selectedAgent.isp) + '22', color: getISPColor(selectedAgent.isp), borderColor: getISPColor(selectedAgent.isp) + '44' }}>
                  {selectedAgent.domain}
                </span>
                <span className="ia-status-badge" style={{ background: getStatusColor(selectedAgent.status) + '22', color: getStatusColor(selectedAgent.status) }}>
                  <span className="ia-status-dot" style={{ background: getStatusColor(selectedAgent.status) }} />
                  {getStatusLabel(selectedAgent.status)}
                </span>
              </div>
            </div>
          </div>

          {/* Knowledge Depth Detail */}
          {(() => {
            const kd = getKnowledgeDepth(selectedAgent);
            return (
              <div className="ia-detail-section ia-kd-section">
                <h4><FontAwesomeIcon icon={faGraduationCap} /> Knowledge Depth</h4>
                <div className="ia-kd-display">
                  <div className="ia-kd-ring" style={{ '--kd-color': kd.color, '--kd-pct': `${kd.score}%` } as React.CSSProperties}>
                    <span className="ia-kd-num">{kd.score}</span>
                    <span className="ia-kd-lbl">{kd.label}</span>
                  </div>
                  <div className="ia-kd-details">
                    <p>This agent has been learning for <strong>{selectedAgent.learning_days} days</strong> across <strong>{formatNumber(selectedAgent.data_points_total)} data points</strong>.</p>
                    <p>It monitors <strong>{selectedAgent.profiles_count} inbox profiles</strong> and tracks sends, opens, clicks, bounces, and complaints.</p>
                  </div>
                </div>
              </div>
            );
          })()}

          {/* Full Metrics */}
          <div className="ia-detail-section">
            <h4><FontAwesomeIcon icon={faChartLine} /> Performance Metrics</h4>
            <div className="ia-detail-metrics-grid">
              <div className="ia-dm">
                <FontAwesomeIcon icon={faEnvelope} />
                <span className="ia-dm-val">{formatNumber(selectedAgent.total_sends)}</span>
                <span className="ia-dm-lbl">Total Sent</span>
              </div>
              <div className="ia-dm">
                <FontAwesomeIcon icon={faEye} />
                <span className="ia-dm-val">{formatNumber(selectedAgent.total_opens)}</span>
                <span className="ia-dm-lbl">Opens</span>
              </div>
              <div className="ia-dm">
                <FontAwesomeIcon icon={faMousePointer} />
                <span className="ia-dm-val">{formatNumber(selectedAgent.total_clicks)}</span>
                <span className="ia-dm-lbl">Clicks</span>
              </div>
              <div className="ia-dm">
                <FontAwesomeIcon icon={faExclamationTriangle} />
                <span className="ia-dm-val">{selectedAgent.total_bounces}</span>
                <span className="ia-dm-lbl">Bounces</span>
              </div>
            </div>
            {/* Rate bars */}
            <div className="ia-rate-bar">
              <div className="ia-rate-header">
                <span>Open Rate</span>
                <span className="ia-rate-pct">{selectedAgent.avg_open_rate.toFixed(1)}%</span>
              </div>
              <div className="ia-bar-bg">
                <div className="ia-bar-fill ia-bar-opens" style={{ width: `${Math.min(selectedAgent.avg_open_rate, 100)}%` }} />
              </div>
            </div>
            <div className="ia-rate-bar">
              <div className="ia-rate-header">
                <span>Click Rate</span>
                <span className="ia-rate-pct">{selectedAgent.avg_click_rate.toFixed(1)}%</span>
              </div>
              <div className="ia-bar-bg">
                <div className="ia-bar-fill ia-bar-clicks" style={{ width: `${Math.min(selectedAgent.avg_click_rate * 3, 100)}%` }} />
              </div>
            </div>
            <div className="ia-rate-bar">
              <div className="ia-rate-header">
                <span>Avg Engagement</span>
                <span className="ia-rate-pct">{selectedAgent.avg_engagement.toFixed(1)}</span>
              </div>
              <div className="ia-bar-bg">
                <div className="ia-bar-fill ia-bar-engagement" style={{ width: `${Math.min(selectedAgent.avg_engagement, 100)}%` }} />
              </div>
            </div>
          </div>

          {/* Learning Sources */}
          <div className="ia-detail-section ia-sources-section">
            <h4><FontAwesomeIcon icon={faDatabase} /> Learning Sources</h4>
            <div className="ia-sources-breakdown">
              {[
                { label: 'Email Sends', value: selectedAgent.learning_sources.sends, icon: faEnvelope, color: '#3b82f6' },
                { label: 'Opens Tracked', value: selectedAgent.learning_sources.opens, icon: faEye, color: '#10b981' },
                { label: 'Clicks Tracked', value: selectedAgent.learning_sources.clicks, icon: faMousePointer, color: '#f59e0b' },
                { label: 'Bounces Analyzed', value: selectedAgent.learning_sources.bounces, icon: faExclamationTriangle, color: '#ef4444' },
                { label: 'Complaints Analyzed', value: selectedAgent.learning_sources.complaints, icon: faShieldAlt, color: '#ec4899' },
              ].map((src, i) => (
                <div key={i} className="ia-source-item">
                  <div className="ia-source-icon" style={{ color: src.color }}>
                    <FontAwesomeIcon icon={src.icon} />
                  </div>
                  <div className="ia-source-body">
                    <span className="ia-source-val">{formatNumber(src.value)}</span>
                    <span className="ia-source-lbl">{src.label}</span>
                  </div>
                  <div className="ia-source-bar">
                    <div
                      className="ia-source-bar-fill"
                      style={{
                        width: `${selectedAgent.data_points_total > 0 ? Math.max((src.value / selectedAgent.data_points_total) * 100, 2) : 0}%`,
                        background: src.color
                      }}
                    />
                  </div>
                </div>
              ))}
            </div>
          </div>

          {/* Learning Timeline */}
          <div className="ia-detail-section">
            <h4><FontAwesomeIcon icon={faClock} /> Learning Timeline</h4>
            <div className="ia-timeline-grid">
              <div className="ia-tl-item">
                <span className="ia-tl-label">First Data Collected</span>
                <span className="ia-tl-value">{new Date(selectedAgent.first_learning_at).toLocaleDateString()}</span>
              </div>
              <div className="ia-tl-item">
                <span className="ia-tl-label">Last Data Collected</span>
                <span className="ia-tl-value">{timeAgo(selectedAgent.last_learning_at)}</span>
              </div>
              <div className="ia-tl-item">
                <span className="ia-tl-label">Total Learning Period</span>
                <span className="ia-tl-value">{selectedAgent.learning_days} days</span>
              </div>
              <div className="ia-tl-item">
                <span className="ia-tl-label">Avg. Data Point Interval</span>
                <span className="ia-tl-value">
                  {selectedAgent.learning_frequency_hours < 1
                    ? `${(selectedAgent.learning_frequency_hours * 60).toFixed(0)} mins`
                    : selectedAgent.learning_frequency_hours < 24
                    ? `${selectedAgent.learning_frequency_hours.toFixed(1)} hours`
                    : `${(selectedAgent.learning_frequency_hours / 24).toFixed(1)} days`}
                </span>
              </div>
            </div>
          </div>

          {/* Engagement Tiers */}
          <div className="ia-detail-section">
            <h4><FontAwesomeIcon icon={faNetworkWired} /> Engagement Distribution</h4>
            <div className="ia-tier-grid">
              <div className="ia-tier-item ia-tier-high">
                <FontAwesomeIcon icon={faStar} />
                <span className="ia-tier-val">{selectedAgent.knowledge.engagement_tiers.high}</span>
                <span className="ia-tier-lbl">High</span>
              </div>
              <div className="ia-tier-item ia-tier-med">
                <FontAwesomeIcon icon={faChartLine} />
                <span className="ia-tier-val">{selectedAgent.knowledge.engagement_tiers.medium}</span>
                <span className="ia-tier-lbl">Medium</span>
              </div>
              <div className="ia-tier-item ia-tier-low">
                <FontAwesomeIcon icon={faArrowDown} />
                <span className="ia-tier-val">{selectedAgent.knowledge.engagement_tiers.low}</span>
                <span className="ia-tier-lbl">Low</span>
              </div>
              <div className="ia-tier-item ia-tier-inactive">
                <FontAwesomeIcon icon={faMinus} />
                <span className="ia-tier-val">{selectedAgent.knowledge.engagement_tiers.inactive}</span>
                <span className="ia-tier-lbl">Inactive</span>
              </div>
            </div>
          </div>

          {/* AI Optimal Send Time */}
          <div className="ia-detail-section ia-optimal-section">
            <h4><FontAwesomeIcon icon={faCalendarAlt} /> Learned Optimal Send Time</h4>
            <div className="ia-optimal-display">
              <div className="ia-optimal-item">
                <FontAwesomeIcon icon={faCalendarAlt} />
                <span>{dayNames[selectedAgent.knowledge.optimal_send_day % 7]}</span>
              </div>
              <div className="ia-optimal-item">
                <FontAwesomeIcon icon={faClock} />
                <span>{selectedAgent.knowledge.optimal_send_hour}:00 UTC</span>
              </div>
            </div>
          </div>

          {/* AI Insights */}
          {selectedAgent.knowledge.insights && selectedAgent.knowledge.insights.length > 0 && (
            <div className="ia-detail-section ia-insights-section">
              <h4><FontAwesomeIcon icon={faLightbulb} /> Agent Insights</h4>
              <ul className="ia-insights-list">
                {selectedAgent.knowledge.insights.map((insight, i) => (
                  <li key={i}><FontAwesomeIcon icon={faCheck} /> {insight}</li>
                ))}
              </ul>
            </div>
          )}

          {/* Risk Factors */}
          {selectedAgent.knowledge.risk_factors && selectedAgent.knowledge.risk_factors.length > 0 && (
            <div className="ia-detail-section ia-risk-section">
              <h4><FontAwesomeIcon icon={faShieldAlt} /> Risk Factors</h4>
              <ul className="ia-risk-list">
                {selectedAgent.knowledge.risk_factors.map((risk, i) => (
                  <li key={i}><FontAwesomeIcon icon={faExclamationTriangle} /> {risk}</li>
                ))}
              </ul>
            </div>
          )}
        </div>
      )}
    </div>
  );
};

export default ISPAgentIntelligence;
