import React, { useState, useEffect, useRef, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faBrain, faLightbulb, faCommentDots, faShieldAlt,
  faChartBar, faExclamationTriangle,
  faSyncAlt, faEye, faStream,
  faBolt,
} from '@fortawesome/free-solid-svg-icons';

interface Philosophy {
  id: string;
  isp: string;
  domain: string;
  belief: string;
  explanation: string;
  confidence: number;
  evidence_count: number;
  category: string;
  sentiment: string;
  created_at: string;
  updated_at: string;
  strength: number;
  challenges: number;
  tags?: string[];
}

interface Thought {
  id: string;
  timestamp: string;
  isp?: string;
  agent_type?: string;
  type: string;
  content: string;
  reasoning?: string;
  confidence?: number;
  severity?: string;
  related_ids?: string[];
}

interface ConsciousnessState {
  philosophies: Philosophy[];
  recent_thoughts: Thought[];
  summary: string;
  health_score: number;
  mood: string;
  active_isps: number;
  total_beliefs: number;
  generated_at: string;
}

interface CampaignMetrics {
  campaign_id: string;
  started_at: string;
  updated_at: string;
  sent: number;
  delivered: number;
  soft_bounce: number;
  hard_bounce: number;
  complaints: number;
  unsubscribes: number;
  opens: number;
  unique_opens: number;
  clicks: number;
  unique_clicks: number;
  inactive: number;
  isp_metrics?: Record<string, any>;
  variant_metrics?: Record<string, any>;
}

const moodEmoji: Record<string, string> = {
  confident: '🟢',
  cautious: '🟡',
  concerned: '🟠',
  alert: '🔴',
  observing: '⚪',
};

const sentimentColor: Record<string, string> = {
  positive: '#10b981',
  cautious: '#f59e0b',
  neutral: '#6b7280',
  negative: '#ef4444',
};

const severityColor: Record<string, string> = {
  info: '#8b5cf6',
  caution: '#f59e0b',
  warning: '#f97316',
  critical: '#ef4444',
};

const thoughtTypeIcon: Record<string, any> = {
  observation: faEye,
  decision: faBolt,
  reflection: faLightbulb,
  warning: faExclamationTriangle,
  insight: faBrain,
  philosophy_update: faLightbulb,
};

export const ConsciousnessDashboard: React.FC = () => {
  const [state, setState] = useState<ConsciousnessState | null>(null);
  const [campaigns, setCampaigns] = useState<CampaignMetrics[]>([]);
  const [liveThoughts, setLiveThoughts] = useState<Thought[]>([]);
  const [activeSection, setActiveSection] = useState<'overview' | 'philosophies' | 'thoughts' | 'campaigns'>('overview');
  const [selectedISP, setSelectedISP] = useState<string>('all');
  const [loading, setLoading] = useState(true);
  const thoughtsEndRef = useRef<HTMLDivElement>(null);
  const eventSourceRef = useRef<EventSource | null>(null);

  const fetchState = useCallback(async () => {
    try {
      const [stateRes, campaignsRes] = await Promise.all([
        fetch('/api/mailing/consciousness/state'),
        fetch('/api/mailing/campaign-events/campaigns'),
      ]);
      if (stateRes.ok) setState(await stateRes.json());
      if (campaignsRes.ok) setCampaigns(await campaignsRes.json());
    } catch {
      // silent
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchState();
    const interval = setInterval(fetchState, 15000);
    return () => clearInterval(interval);
  }, [fetchState]);

  useEffect(() => {
    const es = new EventSource('/api/mailing/consciousness/thoughts/stream');
    es.addEventListener('thought', (e) => {
      const thought: Thought = JSON.parse(e.data);
      setLiveThoughts(prev => {
        const next = [...prev, thought];
        return next.length > 200 ? next.slice(-200) : next;
      });
    });
    eventSourceRef.current = es;
    return () => es.close();
  }, []);

  useEffect(() => {
    if (activeSection === 'thoughts') {
      thoughtsEndRef.current?.scrollIntoView({ behavior: 'smooth' });
    }
  }, [liveThoughts, activeSection]);

  const allThoughts = [
    ...(state?.recent_thoughts || []),
    ...liveThoughts,
  ].sort((a, b) => new Date(b.timestamp).getTime() - new Date(a.timestamp).getTime());

  const filteredPhilosophies = state?.philosophies?.filter(
    p => selectedISP === 'all' || p.isp === selectedISP
  ) || [];

  if (loading) {
    return (
      <div style={styles.loadingContainer}>
        <FontAwesomeIcon icon={faBrain} spin style={{ fontSize: 48, color: '#8b5cf6' }} />
        <p style={{ color: '#8b8fa3', marginTop: 16 }}>Initializing consciousness...</p>
      </div>
    );
  }

  return (
    <div style={styles.container}>
      {/* Header */}
      <div style={styles.header}>
        <div style={styles.headerLeft}>
          <div style={styles.consciousnessIcon}>
            <FontAwesomeIcon icon={faBrain} style={{ fontSize: 28, color: '#8b5cf6' }} />
            <div style={styles.iconPulse} />
          </div>
          <div>
            <h1 style={styles.title}>Consciousness</h1>
            <p style={styles.subtitle}>
              {state?.summary || 'Initializing...'}
            </p>
          </div>
        </div>
        <div style={styles.headerRight}>
          <div style={styles.moodBadge}>
            <span style={{ fontSize: 20 }}>{moodEmoji[state?.mood || 'observing']}</span>
            <span style={styles.moodLabel}>{state?.mood || 'observing'}</span>
          </div>
          <div style={styles.healthScore}>
            <span style={styles.healthValue}>{Math.round(state?.health_score || 0)}</span>
            <span style={styles.healthLabel}>Health</span>
          </div>
          <div style={styles.statBadge}>
            <span style={styles.statValue}>{state?.total_beliefs || 0}</span>
            <span style={styles.statLabel}>Beliefs</span>
          </div>
          <div style={styles.statBadge}>
            <span style={styles.statValue}>{state?.active_isps || 0}</span>
            <span style={styles.statLabel}>ISPs</span>
          </div>
          <button onClick={fetchState} style={styles.refreshBtn}>
            <FontAwesomeIcon icon={faSyncAlt} />
          </button>
        </div>
      </div>

      {/* Section Tabs */}
      <div style={styles.sectionTabs}>
        {(['overview', 'philosophies', 'thoughts', 'campaigns'] as const).map(section => (
          <button
            key={section}
            onClick={() => setActiveSection(section)}
            style={{
              ...styles.sectionTab,
              ...(activeSection === section ? styles.sectionTabActive : {}),
            }}
          >
            <FontAwesomeIcon icon={
              section === 'overview' ? faEye :
              section === 'philosophies' ? faLightbulb :
              section === 'thoughts' ? faCommentDots :
              faChartBar
            } style={{ marginRight: 6 }} />
            {section.charAt(0).toUpperCase() + section.slice(1)}
            {section === 'thoughts' && liveThoughts.length > 0 && (
              <span style={styles.liveDot} />
            )}
          </button>
        ))}
      </div>

      {/* Content */}
      <div style={styles.content}>
        {activeSection === 'overview' && renderOverview(state, campaigns, allThoughts)}
        {activeSection === 'philosophies' && renderPhilosophies(filteredPhilosophies, selectedISP, setSelectedISP)}
        {activeSection === 'thoughts' && renderThoughts(allThoughts, thoughtsEndRef)}
        {activeSection === 'campaigns' && renderCampaigns(campaigns)}
      </div>
    </div>
  );
};

function renderOverview(
  state: ConsciousnessState | null,
  campaigns: CampaignMetrics[],
  thoughts: Thought[],
) {
  const recentThoughts = thoughts.slice(0, 15);
  const positivePhils = state?.philosophies?.filter(p => p.sentiment === 'positive').length || 0;
  const cautiousPhils = state?.philosophies?.filter(p => p.sentiment === 'cautious').length || 0;
  const negativePhils = state?.philosophies?.filter(p => p.sentiment === 'negative').length || 0;

  return (
    <div style={styles.overviewGrid}>
      {/* Belief Distribution */}
      <div style={styles.card}>
        <h3 style={styles.cardTitle}>
          <FontAwesomeIcon icon={faShieldAlt} style={{ marginRight: 8, color: '#8b5cf6' }} />
          Belief Distribution
        </h3>
        <div style={styles.beliefBars}>
          <div style={styles.beliefRow}>
            <span style={{ color: '#10b981' }}>Positive</span>
            <div style={styles.beliefBarTrack}>
              <div style={{ ...styles.beliefBarFill, width: `${positivePhils / Math.max(1, (state?.total_beliefs || 1)) * 100}%`, background: '#10b981' }} />
            </div>
            <span style={styles.beliefCount}>{positivePhils}</span>
          </div>
          <div style={styles.beliefRow}>
            <span style={{ color: '#f59e0b' }}>Cautious</span>
            <div style={styles.beliefBarTrack}>
              <div style={{ ...styles.beliefBarFill, width: `${cautiousPhils / Math.max(1, (state?.total_beliefs || 1)) * 100}%`, background: '#f59e0b' }} />
            </div>
            <span style={styles.beliefCount}>{cautiousPhils}</span>
          </div>
          <div style={styles.beliefRow}>
            <span style={{ color: '#ef4444' }}>Negative</span>
            <div style={styles.beliefBarTrack}>
              <div style={{ ...styles.beliefBarFill, width: `${negativePhils / Math.max(1, (state?.total_beliefs || 1)) * 100}%`, background: '#ef4444' }} />
            </div>
            <span style={styles.beliefCount}>{negativePhils}</span>
          </div>
        </div>
      </div>

      {/* Strongest Beliefs */}
      <div style={styles.card}>
        <h3 style={styles.cardTitle}>
          <FontAwesomeIcon icon={faLightbulb} style={{ marginRight: 8, color: '#f59e0b' }} />
          Strongest Beliefs
        </h3>
        <div style={styles.beliefsList}>
          {(state?.philosophies || []).slice(0, 5).map(p => (
            <div key={p.id} style={styles.miniPhilosophy}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 4 }}>
                <span style={{
                  width: 8, height: 8, borderRadius: '50%',
                  background: sentimentColor[p.sentiment] || '#6b7280',
                }} />
                <span style={{ color: '#cbd5e1', fontWeight: 600, fontSize: 12, textTransform: 'uppercase' }}>
                  {p.isp} / {p.domain}
                </span>
                <span style={{ marginLeft: 'auto', color: '#64748b', fontSize: 11 }}>
                  {Math.round(p.strength * 100)}% strength
                </span>
              </div>
              <p style={{ color: '#94a3b8', fontSize: 13, margin: 0, lineHeight: 1.4 }}>{p.belief}</p>
            </div>
          ))}
        </div>
      </div>

      {/* Active Campaigns */}
      <div style={styles.card}>
        <h3 style={styles.cardTitle}>
          <FontAwesomeIcon icon={faChartBar} style={{ marginRight: 8, color: '#06b6d4' }} />
          Active Campaigns
        </h3>
        {campaigns.length === 0 ? (
          <p style={{ color: '#64748b', fontSize: 13 }}>No active campaigns</p>
        ) : (
          <div style={styles.campaignMiniList}>
            {campaigns.slice(0, 4).map(c => (
              <div key={c.campaign_id} style={styles.campaignMiniItem}>
                <span style={{ color: '#e2e8f0', fontWeight: 600, fontSize: 13 }}>{c.campaign_id}</span>
                <div style={styles.miniStats}>
                  <span style={{ color: '#10b981' }}>{c.delivered} dlvd</span>
                  <span style={{ color: '#06b6d4' }}>{c.unique_opens} opens</span>
                  <span style={{ color: '#f59e0b' }}>{c.unique_clicks} clicks</span>
                  {c.complaints > 0 && <span style={{ color: '#ef4444' }}>{c.complaints} complaints</span>}
                </div>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Live Thought Feed */}
      <div style={{ ...styles.card, gridColumn: '1 / -1' }}>
        <h3 style={styles.cardTitle}>
          <FontAwesomeIcon icon={faStream} style={{ marginRight: 8, color: '#8b5cf6' }} />
          Live Thought Stream
          <span style={styles.liveDot} />
        </h3>
        <div style={styles.thoughtFeed}>
          {recentThoughts.length === 0 ? (
            <p style={{ color: '#64748b', fontSize: 13 }}>Awaiting observations...</p>
          ) : (
            recentThoughts.map(t => (
              <div key={t.id} style={styles.thoughtItem}>
                <FontAwesomeIcon
                  icon={thoughtTypeIcon[t.type] || faCommentDots}
                  style={{ color: severityColor[t.severity || 'info'] || '#8b5cf6', flexShrink: 0, marginTop: 2 }}
                />
                <div style={{ flex: 1, minWidth: 0 }}>
                  <p style={{ color: '#e2e8f0', fontSize: 13, margin: 0, lineHeight: 1.4 }}>{t.content}</p>
                  <span style={{ color: '#475569', fontSize: 11 }}>
                    {new Date(t.timestamp).toLocaleTimeString()}
                    {t.isp && ` · ${t.isp}`}
                    {t.agent_type && ` · ${t.agent_type}`}
                  </span>
                </div>
              </div>
            ))
          )}
        </div>
      </div>
    </div>
  );
}

function renderPhilosophies(
  philosophies: Philosophy[],
  selectedISP: string,
  setSelectedISP: (isp: string) => void,
) {
  const isps = ['all', 'gmail', 'yahoo', 'microsoft', 'apple', 'comcast', 'att', 'cox', 'charter'];

  return (
    <div>
      {/* ISP Filter */}
      <div style={styles.ispFilter}>
        {isps.map(isp => (
          <button
            key={isp}
            onClick={() => setSelectedISP(isp)}
            style={{
              ...styles.ispFilterBtn,
              ...(selectedISP === isp ? styles.ispFilterBtnActive : {}),
            }}
          >
            {isp === 'all' ? 'All ISPs' : isp.charAt(0).toUpperCase() + isp.slice(1)}
          </button>
        ))}
      </div>

      {/* Philosophy Cards */}
      <div style={styles.philosophyGrid}>
        {philosophies.length === 0 ? (
          <div style={styles.emptyState}>
            <FontAwesomeIcon icon={faLightbulb} style={{ fontSize: 48, color: '#374151', marginBottom: 16 }} />
            <p style={{ color: '#64748b' }}>No philosophies formed yet. The system needs more observations.</p>
          </div>
        ) : (
          philosophies.map(p => (
            <div key={p.id} style={styles.philosophyCard}>
              <div style={styles.philosophyHeader}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                  <span style={{
                    padding: '2px 8px', borderRadius: 4, fontSize: 11, fontWeight: 700,
                    textTransform: 'uppercase', letterSpacing: 0.5,
                    background: sentimentColor[p.sentiment] + '22',
                    color: sentimentColor[p.sentiment],
                  }}>{p.sentiment}</span>
                  <span style={{ color: '#8b8fa3', fontSize: 12 }}>{p.isp} / {p.domain}</span>
                </div>
                <span style={{ color: '#64748b', fontSize: 11 }}>{p.evidence_count} observations</span>
              </div>

              <p style={styles.philosophyBelief}>{p.belief}</p>
              <p style={styles.philosophyExplanation}>{p.explanation}</p>

              <div style={styles.philosophyFooter}>
                <div style={styles.strengthBar}>
                  <div style={{ ...styles.strengthFill, width: `${p.strength * 100}%` }} />
                </div>
                <span style={{ color: '#64748b', fontSize: 11 }}>
                  {Math.round(p.confidence * 100)}% confidence · {Math.round(p.strength * 100)}% strength
                  {p.challenges > 0 && ` · ${p.challenges} challenges`}
                </span>
              </div>

              {p.tags && p.tags.length > 0 && (
                <div style={styles.tagRow}>
                  {p.tags.map(tag => (
                    <span key={tag} style={styles.tag}>{tag}</span>
                  ))}
                </div>
              )}
            </div>
          ))
        )}
      </div>
    </div>
  );
}

function renderThoughts(thoughts: Thought[], endRef: React.RefObject<HTMLDivElement>) {
  return (
    <div style={styles.thoughtsContainer}>
      {thoughts.length === 0 ? (
        <div style={styles.emptyState}>
          <FontAwesomeIcon icon={faCommentDots} style={{ fontSize: 48, color: '#374151', marginBottom: 16 }} />
          <p style={{ color: '#64748b' }}>No thoughts yet. The system is still forming its initial observations.</p>
        </div>
      ) : (
        <>
          {thoughts.map(t => (
            <div key={t.id} style={{
              ...styles.thoughtCard,
              borderLeft: `3px solid ${severityColor[t.severity || 'info'] || '#8b5cf6'}`,
            }}>
              <div style={styles.thoughtCardHeader}>
                <FontAwesomeIcon
                  icon={thoughtTypeIcon[t.type] || faCommentDots}
                  style={{ color: severityColor[t.severity || 'info'], marginRight: 8 }}
                />
                <span style={{ color: '#94a3b8', fontSize: 12, textTransform: 'uppercase', fontWeight: 600 }}>
                  {t.type}
                </span>
                {t.isp && <span style={styles.thoughtTag}>{t.isp}</span>}
                {t.agent_type && <span style={styles.thoughtTag}>{t.agent_type}</span>}
                <span style={{ marginLeft: 'auto', color: '#475569', fontSize: 11 }}>
                  {new Date(t.timestamp).toLocaleString()}
                </span>
              </div>
              <p style={{ color: '#e2e8f0', fontSize: 14, margin: '8px 0', lineHeight: 1.5 }}>{t.content}</p>
              {t.reasoning && (
                <p style={{ color: '#64748b', fontSize: 12, margin: '4px 0 0', fontStyle: 'italic' }}>
                  Reasoning: {t.reasoning}
                </p>
              )}
              {t.confidence != null && t.confidence > 0 && (
                <span style={{ color: '#475569', fontSize: 11 }}>Confidence: {Math.round(t.confidence * 100)}%</span>
              )}
            </div>
          ))}
          <div ref={endRef as React.RefObject<HTMLDivElement>} />
        </>
      )}
    </div>
  );
}

function renderCampaigns(campaigns: CampaignMetrics[]) {
  return (
    <div>
      {campaigns.length === 0 ? (
        <div style={styles.emptyState}>
          <FontAwesomeIcon icon={faChartBar} style={{ fontSize: 48, color: '#374151', marginBottom: 16 }} />
          <p style={{ color: '#64748b' }}>No campaigns tracked yet. Events will appear as campaigns are sent.</p>
        </div>
      ) : (
        <div style={styles.campaignGrid}>
          {campaigns.map(c => {
            const deliveryRate = c.sent > 0 ? (c.delivered / c.sent * 100) : 0;
            const openRate = c.delivered > 0 ? (c.unique_opens / c.delivered * 100) : 0;
            const clickRate = c.delivered > 0 ? (c.unique_clicks / c.delivered * 100) : 0;

            return (
              <div key={c.campaign_id} style={styles.campaignCard}>
                <div style={styles.campaignCardHeader}>
                  <h3 style={{ color: '#e2e8f0', fontSize: 16, margin: 0, fontWeight: 700 }}>
                    {c.campaign_id}
                  </h3>
                  <span style={{ color: '#475569', fontSize: 11 }}>
                    Started {new Date(c.started_at).toLocaleDateString()}
                  </span>
                </div>

                <div style={styles.metricsGrid}>
                  <MetricBox label="Sent" value={c.sent} color="#8b5cf6" />
                  <MetricBox label="Delivered" value={c.delivered} color="#10b981" sub={`${deliveryRate.toFixed(1)}%`} />
                  <MetricBox label="Opens" value={c.opens} color="#06b6d4" sub={`${c.unique_opens} unique`} />
                  <MetricBox label="Clicks" value={c.clicks} color="#3b82f6" sub={`${c.unique_clicks} unique`} />
                  <MetricBox label="Soft Bounce" value={c.soft_bounce} color="#f59e0b" />
                  <MetricBox label="Hard Bounce" value={c.hard_bounce} color="#ef4444" />
                  <MetricBox label="Complaints" value={c.complaints} color="#dc2626" />
                  <MetricBox label="Unsubscribes" value={c.unsubscribes} color="#f97316" />
                  <MetricBox label="Inactive" value={c.inactive} color="#6b7280" sub="4+ sends, 0 engagement" />
                </div>

                <div style={styles.rateRow}>
                  <RateBar label="Delivery" value={deliveryRate} color="#10b981" />
                  <RateBar label="Open Rate" value={openRate} color="#06b6d4" />
                  <RateBar label="Click Rate" value={clickRate} color="#3b82f6" />
                </div>

                {c.isp_metrics && Object.keys(c.isp_metrics).length > 0 && (
                  <div style={styles.ispBreakdown}>
                    <h4 style={{ color: '#94a3b8', fontSize: 12, margin: '12px 0 8px', textTransform: 'uppercase' }}>ISP Breakdown</h4>
                    {Object.entries(c.isp_metrics).map(([isp, m]: [string, any]) => (
                      <div key={isp} style={styles.ispRow}>
                        <span style={{ color: '#cbd5e1', fontWeight: 600, fontSize: 12, width: 80 }}>{isp}</span>
                        <span style={{ color: '#10b981', fontSize: 11 }}>{m.delivered} dlvd</span>
                        <span style={{ color: '#06b6d4', fontSize: 11 }}>{m.unique_opens} opens</span>
                        <span style={{ color: '#3b82f6', fontSize: 11 }}>{m.unique_clicks} clicks</span>
                        {(m.hard_bounce > 0 || m.soft_bounce > 0) && (
                          <span style={{ color: '#ef4444', fontSize: 11 }}>{m.hard_bounce + m.soft_bounce} bounces</span>
                        )}
                      </div>
                    ))}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

const MetricBox: React.FC<{ label: string; value: number; color: string; sub?: string }> = ({ label, value, color, sub }) => (
  <div style={styles.metricBox}>
    <span style={{ color, fontSize: 24, fontWeight: 700, fontFamily: 'monospace' }}>
      {value.toLocaleString()}
    </span>
    <span style={{ color: '#94a3b8', fontSize: 11 }}>{label}</span>
    {sub && <span style={{ color: '#475569', fontSize: 10 }}>{sub}</span>}
  </div>
);

const RateBar: React.FC<{ label: string; value: number; color: string }> = ({ label, value, color }) => (
  <div style={{ flex: 1 }}>
    <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 4 }}>
      <span style={{ color: '#94a3b8', fontSize: 11 }}>{label}</span>
      <span style={{ color, fontSize: 11, fontWeight: 600 }}>{value.toFixed(1)}%</span>
    </div>
    <div style={{ height: 4, background: '#1e293b', borderRadius: 2 }}>
      <div style={{ height: 4, borderRadius: 2, background: color, width: `${Math.min(value, 100)}%`, transition: 'width 0.5s ease' }} />
    </div>
  </div>
);

const styles: Record<string, React.CSSProperties> = {
  container: {
    padding: 24,
    maxWidth: 1400,
    margin: '0 auto',
  },
  loadingContainer: {
    display: 'flex',
    flexDirection: 'column',
    alignItems: 'center',
    justifyContent: 'center',
    height: '60vh',
  },
  header: {
    display: 'flex',
    justifyContent: 'space-between',
    alignItems: 'center',
    marginBottom: 24,
    flexWrap: 'wrap',
    gap: 16,
  },
  headerLeft: {
    display: 'flex',
    alignItems: 'center',
    gap: 16,
  },
  consciousnessIcon: {
    position: 'relative',
    width: 56,
    height: 56,
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    background: 'linear-gradient(135deg, rgba(139,92,246,0.15), rgba(59,130,246,0.15))',
    borderRadius: 16,
    border: '1px solid rgba(139,92,246,0.3)',
  },
  iconPulse: {
    position: 'absolute',
    inset: -4,
    borderRadius: 20,
    border: '1px solid rgba(139,92,246,0.2)',
    animation: 'pulse 2s ease-in-out infinite',
  },
  title: {
    color: '#f1f5f9',
    fontSize: 28,
    fontWeight: 800,
    margin: 0,
    letterSpacing: -0.5,
  },
  subtitle: {
    color: '#64748b',
    fontSize: 14,
    margin: '4px 0 0',
  },
  headerRight: {
    display: 'flex',
    alignItems: 'center',
    gap: 12,
  },
  moodBadge: {
    display: 'flex',
    alignItems: 'center',
    gap: 8,
    padding: '8px 16px',
    background: 'rgba(30,41,59,0.8)',
    borderRadius: 12,
    border: '1px solid rgba(51,65,85,0.5)',
  },
  moodLabel: {
    color: '#cbd5e1',
    fontSize: 13,
    fontWeight: 600,
    textTransform: 'capitalize' as const,
  },
  healthScore: {
    display: 'flex',
    flexDirection: 'column' as const,
    alignItems: 'center',
    padding: '6px 14px',
    background: 'rgba(30,41,59,0.8)',
    borderRadius: 12,
    border: '1px solid rgba(51,65,85,0.5)',
  },
  healthValue: {
    color: '#10b981',
    fontSize: 20,
    fontWeight: 800,
    fontFamily: 'monospace',
  },
  healthLabel: {
    color: '#475569',
    fontSize: 10,
    textTransform: 'uppercase' as const,
  },
  statBadge: {
    display: 'flex',
    flexDirection: 'column' as const,
    alignItems: 'center',
    padding: '6px 14px',
    background: 'rgba(30,41,59,0.8)',
    borderRadius: 12,
    border: '1px solid rgba(51,65,85,0.5)',
  },
  statValue: {
    color: '#e2e8f0',
    fontSize: 18,
    fontWeight: 700,
    fontFamily: 'monospace',
  },
  statLabel: {
    color: '#475569',
    fontSize: 10,
    textTransform: 'uppercase' as const,
  },
  refreshBtn: {
    background: 'none',
    border: '1px solid rgba(51,65,85,0.5)',
    color: '#64748b',
    padding: '8px 12px',
    borderRadius: 8,
    cursor: 'pointer',
    fontSize: 14,
  },
  sectionTabs: {
    display: 'flex',
    gap: 4,
    marginBottom: 24,
    background: 'rgba(15,23,42,0.5)',
    padding: 4,
    borderRadius: 12,
    border: '1px solid rgba(30,41,59,0.5)',
  },
  sectionTab: {
    flex: 1,
    padding: '10px 16px',
    background: 'transparent',
    border: 'none',
    color: '#64748b',
    fontSize: 13,
    fontWeight: 600,
    cursor: 'pointer',
    borderRadius: 8,
    transition: 'all 0.2s',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    position: 'relative' as const,
  },
  sectionTabActive: {
    background: 'rgba(139,92,246,0.15)',
    color: '#c4b5fd',
    border: '1px solid rgba(139,92,246,0.3)',
  },
  liveDot: {
    width: 6,
    height: 6,
    borderRadius: '50%',
    background: '#10b981',
    marginLeft: 6,
    animation: 'pulse 1.5s ease-in-out infinite',
  },
  content: {},
  overviewGrid: {
    display: 'grid',
    gridTemplateColumns: 'repeat(3, 1fr)',
    gap: 16,
  },
  card: {
    background: 'rgba(15,23,42,0.6)',
    border: '1px solid rgba(30,41,59,0.8)',
    borderRadius: 12,
    padding: 20,
  },
  cardTitle: {
    color: '#e2e8f0',
    fontSize: 14,
    fontWeight: 700,
    margin: '0 0 16px',
    display: 'flex',
    alignItems: 'center',
  },
  beliefBars: {
    display: 'flex',
    flexDirection: 'column' as const,
    gap: 12,
  },
  beliefRow: {
    display: 'flex',
    alignItems: 'center',
    gap: 10,
    fontSize: 13,
  },
  beliefBarTrack: {
    flex: 1,
    height: 6,
    background: '#1e293b',
    borderRadius: 3,
    overflow: 'hidden' as const,
  },
  beliefBarFill: {
    height: '100%',
    borderRadius: 3,
    transition: 'width 0.5s ease',
  },
  beliefCount: {
    color: '#94a3b8',
    fontSize: 13,
    fontWeight: 600,
    width: 20,
    textAlign: 'right' as const,
  },
  beliefsList: {
    display: 'flex',
    flexDirection: 'column' as const,
    gap: 12,
  },
  miniPhilosophy: {
    padding: '10px 12px',
    background: 'rgba(30,41,59,0.4)',
    borderRadius: 8,
  },
  campaignMiniList: {
    display: 'flex',
    flexDirection: 'column' as const,
    gap: 10,
  },
  campaignMiniItem: {
    padding: '8px 12px',
    background: 'rgba(30,41,59,0.4)',
    borderRadius: 8,
  },
  miniStats: {
    display: 'flex',
    gap: 12,
    marginTop: 4,
    fontSize: 11,
  },
  thoughtFeed: {
    maxHeight: 400,
    overflow: 'auto',
    display: 'flex',
    flexDirection: 'column' as const,
    gap: 8,
  },
  thoughtItem: {
    display: 'flex',
    gap: 10,
    padding: '8px 12px',
    background: 'rgba(30,41,59,0.3)',
    borderRadius: 8,
  },
  ispFilter: {
    display: 'flex',
    gap: 6,
    marginBottom: 20,
    flexWrap: 'wrap' as const,
  },
  ispFilterBtn: {
    padding: '6px 14px',
    background: 'rgba(30,41,59,0.6)',
    border: '1px solid rgba(51,65,85,0.5)',
    color: '#94a3b8',
    borderRadius: 8,
    fontSize: 12,
    fontWeight: 600,
    cursor: 'pointer',
    transition: 'all 0.2s',
  },
  ispFilterBtnActive: {
    background: 'rgba(139,92,246,0.2)',
    borderColor: 'rgba(139,92,246,0.5)',
    color: '#c4b5fd',
  },
  philosophyGrid: {
    display: 'grid',
    gridTemplateColumns: 'repeat(2, 1fr)',
    gap: 16,
  },
  emptyState: {
    gridColumn: '1 / -1',
    textAlign: 'center' as const,
    padding: 60,
  },
  philosophyCard: {
    background: 'rgba(15,23,42,0.6)',
    border: '1px solid rgba(30,41,59,0.8)',
    borderRadius: 12,
    padding: 20,
  },
  philosophyHeader: {
    display: 'flex',
    justifyContent: 'space-between',
    alignItems: 'center',
    marginBottom: 12,
  },
  philosophyBelief: {
    color: '#e2e8f0',
    fontSize: 15,
    lineHeight: 1.5,
    margin: '0 0 8px',
    fontWeight: 500,
  },
  philosophyExplanation: {
    color: '#64748b',
    fontSize: 13,
    lineHeight: 1.5,
    margin: '0 0 12px',
  },
  philosophyFooter: {
    display: 'flex',
    flexDirection: 'column' as const,
    gap: 6,
  },
  strengthBar: {
    height: 4,
    background: '#1e293b',
    borderRadius: 2,
    overflow: 'hidden' as const,
  },
  strengthFill: {
    height: '100%',
    background: 'linear-gradient(90deg, #8b5cf6, #06b6d4)',
    borderRadius: 2,
    transition: 'width 0.5s ease',
  },
  tagRow: {
    display: 'flex',
    gap: 6,
    marginTop: 10,
  },
  tag: {
    padding: '2px 8px',
    background: 'rgba(30,41,59,0.6)',
    border: '1px solid rgba(51,65,85,0.5)',
    borderRadius: 4,
    color: '#94a3b8',
    fontSize: 11,
  },
  thoughtsContainer: {
    maxHeight: 'calc(100vh - 250px)',
    overflow: 'auto',
    display: 'flex',
    flexDirection: 'column' as const,
    gap: 8,
  },
  thoughtCard: {
    background: 'rgba(15,23,42,0.6)',
    border: '1px solid rgba(30,41,59,0.8)',
    borderRadius: 8,
    padding: 16,
  },
  thoughtCardHeader: {
    display: 'flex',
    alignItems: 'center',
    gap: 8,
    marginBottom: 4,
    flexWrap: 'wrap' as const,
  },
  thoughtTag: {
    padding: '1px 6px',
    background: 'rgba(139,92,246,0.15)',
    borderRadius: 4,
    color: '#c4b5fd',
    fontSize: 10,
    fontWeight: 600,
  },
  campaignGrid: {
    display: 'grid',
    gridTemplateColumns: '1fr',
    gap: 16,
  },
  campaignCard: {
    background: 'rgba(15,23,42,0.6)',
    border: '1px solid rgba(30,41,59,0.8)',
    borderRadius: 12,
    padding: 20,
  },
  campaignCardHeader: {
    display: 'flex',
    justifyContent: 'space-between',
    alignItems: 'center',
    marginBottom: 16,
  },
  metricsGrid: {
    display: 'grid',
    gridTemplateColumns: 'repeat(auto-fill, minmax(120px, 1fr))',
    gap: 12,
    marginBottom: 16,
  },
  metricBox: {
    display: 'flex',
    flexDirection: 'column' as const,
    alignItems: 'center',
    padding: '12px 8px',
    background: 'rgba(30,41,59,0.4)',
    borderRadius: 8,
    gap: 2,
  },
  rateRow: {
    display: 'flex',
    gap: 20,
    padding: '12px 0',
    borderTop: '1px solid rgba(30,41,59,0.8)',
  },
  ispBreakdown: {},
  ispRow: {
    display: 'flex',
    gap: 16,
    padding: '6px 0',
    borderBottom: '1px solid rgba(30,41,59,0.4)',
    alignItems: 'center',
  },
};
