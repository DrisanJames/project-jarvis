import React, { useState, useEffect, Suspense, lazy } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { 
  faChartLine, faEnvelope, faBullhorn, faPaperPlane, faRoute,
  faListUl, faCrosshairs, faBolt, faFileImport,
  faBan, faBrain, faRobot, faChartPie, faServer,
  faFileAlt, /* faArrowLeft, */ faFire, faGlobe,
  faSpinner, faRocket, faShieldAlt,
} from '@fortawesome/free-solid-svg-icons';
import { IconDefinition } from '@fortawesome/fontawesome-svg-core';
import { useAuth } from '../../../contexts/AuthContext';
import './MailingPortal.css';

// ── Lazy-loaded heavy components (code-split into separate chunks) ──────────
const ListPortal = lazy(() => import('../components/ListPortal').then(m => ({ default: m.ListPortal })));
const CampaignPortal = lazy(() => import('../components/CampaignPortal').then(m => ({ default: m.CampaignPortal })));
const ISPAgentIntelligence = lazy(() => import('../components/ISPAgentIntelligence').then(m => ({ default: m.ISPAgentIntelligence })));
const SuppressionPortal = lazy(() => import('../components/SuppressionPortal').then(m => ({ default: m.SuppressionPortal })));
const InboxProfiles = lazy(() => import('../components/InboxProfiles').then(m => ({ default: m.InboxProfiles })));
const SendTestEmail = lazy(() => import('../components/SendTestEmail').then(m => ({ default: m.SendTestEmail })));
const JourneyCenter = lazy(() => import('../components/JourneyCenter').then(m => ({ default: m.JourneyCenter })));
const MissionControl = lazy(() => import('../components/MissionControl').then(m => ({ default: m.MissionControl })));
const DomainCenter = lazy(() => import('../components/DomainCenter').then(m => ({ default: m.DomainCenter })));
const AnalyticsCenter = lazy(() => import('../components/AnalyticsCenter').then(m => ({ default: m.AnalyticsCenter })));
const OfferCenter = lazy(() => import('../components/OfferCenter').then(m => ({ default: m.OfferCenter })));
const JarvisDashboard = lazy(() => import('../components/JarvisDashboard').then(m => ({ default: m.JarvisDashboard })));
const PMTACampaignWizard = lazy(() => import('../components/PMTACampaignWizard').then(m => ({ default: m.PMTACampaignWizard })));
const ConsciousnessDashboard = lazy(() => import('../components/ConsciousnessDashboard').then(m => ({ default: m.ConsciousnessDashboard })));
const GlobalSuppressionDashboard = lazy(() => import('../components/GlobalSuppressionDashboard').then(m => ({ default: m.GlobalSuppressionDashboard })));
const DataNormalizerPanel = lazy(() => import('../components/DataNormalizerPanel').then(m => ({ default: m.DataNormalizerPanel })));

// ── Suspense fallback ───────────────────────────────────────────────────────
const ChunkLoader: React.FC = () => (
  <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '50vh', color: '#8b8fa3', gap: 10, fontSize: 14 }}>
    <FontAwesomeIcon icon={faSpinner} spin /> Loading…
  </div>
);

type TabId = 'dashboard' | 'lists' | 'campaign-center' | 'journey-center' | 'suppressions' | 'global-suppression' | 'profiles' | 'send' | 'sending-plans' | 'domain-center' | 'delivery-servers' | 'offers' | 'analytics' | 'segments' | 'automations' | 'ab-tests' | 'import' | 'mission-control' | 'jarvis' | 'pmta-wizard' | 'consciousness' | 'data-import' | 'content-library';

interface Tab {
  id: TabId;
  label: string;
  icon: IconDefinition;
  description: string;
}

const tabs: Tab[] = [
  { id: 'dashboard', label: 'Dashboard', icon: faChartLine, description: 'Real-time overview of email performance' },
  { id: 'pmta-wizard', label: 'PMTA Campaign', icon: faRocket, description: 'ISP-native PMTA campaign builder' },
  { id: 'consciousness', label: 'Consciousness', icon: faBrain, description: 'AI beliefs, philosophies & campaign intelligence' },
  { id: 'campaign-center', label: 'Campaign Center', icon: faBullhorn, description: 'Create, manage & monitor campaigns' },
  { id: 'journey-center', label: 'Journey Center', icon: faRoute, description: 'Monitor & manage automated journeys' },
  { id: 'lists', label: 'Lists & Segments', icon: faListUl, description: 'Manage lists, segments & subscribers' },
  { id: 'automations', label: 'Automations', icon: faBolt, description: 'Drip campaigns & workflows' },
  { id: 'mission-control', label: 'Mission Control', icon: faFire, description: 'Live campaign monitoring & agent decisions' },
  // A/B Tests hidden — functionality lives within Campaign Center
  // { id: 'ab-tests', label: 'A/B Tests', icon: faFlask, description: 'Test subject lines & content' },
  { id: 'suppressions', label: 'Suppressions', icon: faBan, description: 'Blocked email addresses' },
  { id: 'global-suppression', label: 'Global Suppression', icon: faShieldAlt, description: 'Single source of truth — MD5 hashed, ISP-agnostic' },
  { id: 'profiles', label: 'Inbox Intel', icon: faBrain, description: 'Per-recipient intelligence' },
  { id: 'sending-plans', label: 'AI Plans', icon: faRobot, description: 'ISP-specific AI agents & intelligence' },
  { id: 'domain-center', label: 'Domain Center', icon: faGlobe, description: 'Sending, tracking & image domains' },
  { id: 'analytics', label: 'Analytics', icon: faChartPie, description: 'Comprehensive mail & AI analytics' },
  { id: 'content-library', label: 'Content Library', icon: faEnvelope, description: 'Reusable email templates & content blocks' },
  { id: 'delivery-servers', label: 'Servers', icon: faServer, description: 'PMTA servers, IPs & sending infrastructure' },
  { id: 'offers', label: 'Offers', icon: faFileAlt, description: 'Network offers, AI suggestions & creative library' },
  { id: 'jarvis', label: 'Jarvis AI', icon: faRobot, description: 'Autonomous AI campaign orchestrator & monitoring' },
  { id: 'data-import', label: 'Data Import', icon: faFileImport, description: 'S3 data normalization & import monitoring' },
];

export const MailingPortal: React.FC = () => {
  const { organization } = useAuth();
  const [activeTab, setActiveTab] = useState<TabId>('dashboard');
  const [realTimeStats, setRealTimeStats] = useState<any>(null);

  // Cross-component offer state — when user clicks "Use This Offer" in Offer Center,
  // we switch to Campaign Center and pass the selected offer through.
  const [pendingOffer, setPendingOffer] = useState<{ offerId: string; offerName: string } | null>(null);

  const handleUseOffer = (offerId: string, offerName: string) => {
    setPendingOffer({ offerId, offerName });
    setActiveTab('campaign-center');
  };

  // Clear pending offer when leaving campaign-center
  const handleTabChange = (tab: TabId) => {
    if (tab !== 'campaign-center') {
      setPendingOffer(null);
    }
    setActiveTab(tab);
  };

  // Fetch real-time stats for sidebar
  useEffect(() => {
    const fetchStats = () => {
      const headers: HeadersInit = {
        'Content-Type': 'application/json',
      };
      if (organization?.id) {
        headers['X-Organization-ID'] = organization.id;
      }
      fetch('/api/mailing/dashboard', { headers, credentials: 'include' })
        .then(res => res.json())
        .then(data => setRealTimeStats(data))
        .catch(() => {});
    };
    fetchStats();
    const interval = setInterval(fetchStats, 30000); // Refresh every 30s
    return () => clearInterval(interval);
  }, [organization]);

  const renderContent = () => {
    switch (activeTab) {
      case 'dashboard':
        return <EnhancedDashboard />;
      case 'lists':
        return <ListPortal />;
      case 'campaign-center':
        return <CampaignPortal initialOffer={pendingOffer} onOfferConsumed={() => setPendingOffer(null)} />;
      case 'journey-center':
        return <JourneyCenter />;
      case 'sending-plans':
        return <ISPAgentIntelligence />;
      case 'domain-center':
        return <DomainCenter />;
      case 'suppressions':
        return <SuppressionPortal />;
      case 'global-suppression':
        return <GlobalSuppressionDashboard />;
      case 'profiles':
        return <InboxProfiles />;
      case 'send':
        return <SendTestEmail />;
      case 'analytics':
        return <AnalyticsCenter />;
      case 'content-library':
        return <TemplatesManager />;
      case 'delivery-servers':
        return <DeliveryServersManager />;
      case 'offers':
        return <OfferCenter onUseOffer={handleUseOffer} />;
      case 'automations':
        return <AutomationsManager />;
      case 'ab-tests':
        return <ABTestsManager />;
      case 'mission-control':
        return <MissionControl />;
      case 'jarvis':
        return <JarvisDashboard />;
      case 'pmta-wizard':
        return <PMTACampaignWizard onClose={() => handleTabChange('dashboard')} />;
      case 'consciousness':
        return <ConsciousnessDashboard />;
      case 'data-import':
        return <DataNormalizerPanel />;
      default:
        return <EnhancedDashboard />;
    }
  };

  const currentTab = tabs.find(t => t.id === activeTab);

  return (
    <div className="mailing-portal">
      <aside className="mailing-sidebar">
        <div className="sidebar-header">
          <div className="jarvis-logo">
            <FontAwesomeIcon icon={faRobot} className="header-icon" />
            <div className="logo-pulse"></div>
          </div>
          <h1>JARVIS</h1>
          <span className="subtitle">Mailing Platform</span>
          <div className="header-scan-line"></div>
        </div>

        <nav className="sidebar-nav">
          {tabs.map((tab) => (
            <button
              key={tab.id}
              className={`nav-item ${activeTab === tab.id ? 'active' : ''}`}
              onClick={() => handleTabChange(tab.id)}
              title={tab.description}
            >
              <span className="nav-icon"><FontAwesomeIcon icon={tab.icon} /></span>
              <span className="nav-label">{tab.label}</span>
            </button>
          ))}
        </nav>

        <div className="sidebar-footer">
          <div className="quick-stats">
            <div className="quick-stat">
              <span className="quick-stat-value">{realTimeStats?.total_subscribers?.toLocaleString() || '—'}</span>
              <span className="quick-stat-label">Subscribers</span>
            </div>
            <div className="quick-stat">
              <span className="quick-stat-value">{realTimeStats?.performance?.open_rate ? `${(realTimeStats.performance.open_rate * 100).toFixed(1)}%` : '—'}</span>
              <span className="quick-stat-label">Open Rate</span>
            </div>
          </div>
          <div className="connection-status">
            <span className="status-dot active"></span>
            <span>PMTA Connected</span>
          </div>
          {/* Back to Analytics hidden — PMTA-only mode. Uncomment to restore.
          <button className="back-to-analytics" onClick={() => window.location.href = '/'}>
            <FontAwesomeIcon icon={faArrowLeft} /> Back to Analytics Platform
          </button>
          */}
        </div>
      </aside>

      <main className="mailing-content">
        <div className="content-header">
          <h2><FontAwesomeIcon icon={currentTab?.icon || faChartLine} className="content-header-icon" /> {currentTab?.label}</h2>
          <p className="content-description">{currentTab?.description}</p>
        </div>
        <Suspense fallback={<ChunkLoader />}>
          {renderContent()}
        </Suspense>
      </main>
    </div>
  );
};

// Enhanced Dashboard with System Explanations
const EnhancedDashboard: React.FC = () => {
  const [dashboard, setDashboard] = useState<any>(null);
  const [throttle, setThrottle] = useState<any>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    Promise.all([
      fetch('/api/mailing/dashboard').then(r => r.json()),
      fetch('/api/mailing/throttle/status').then(r => r.json()),
    ]).then(([dash, thr]) => {
      setDashboard(dash);
      setThrottle(thr);
      setLoading(false);
    }).catch(() => setLoading(false));
  }, []);

  if (loading) return <div className="loading-state">Loading dashboard...</div>;

  return (
    <div className="enhanced-dashboard">
      {/* System Overview Cards */}
      <div className="system-overview">
        <div className="system-card sending">
          <div className="system-header">
            <span className="system-icon"><FontAwesomeIcon icon={faPaperPlane} /></span>
            <h3>Email Sending</h3>
            <span className="status-badge active">Active</span>
          </div>
          <div className="system-description">
            <p>Production emails are sent via <strong>SparkPost API</strong> with real-time tracking.</p>
          </div>
          {/* Daily Cap Gauge */}
          <div className="daily-cap-section">
            <div className="daily-cap-header">
              <span className="daily-cap-title">Daily Sending Cap</span>
              <span className={`daily-cap-pct ${(dashboard?.daily_utilization || 0) > 90 ? 'critical' : (dashboard?.daily_utilization || 0) > 70 ? 'warning' : 'healthy'}`}>
                {(dashboard?.daily_utilization || 0).toFixed(1)}% used
              </span>
            </div>
            <div className="daily-cap-bar">
              <div
                className={`daily-cap-fill ${(dashboard?.daily_utilization || 0) > 90 ? 'critical' : (dashboard?.daily_utilization || 0) > 70 ? 'warning' : 'healthy'}`}
                style={{ width: `${Math.min(dashboard?.daily_utilization || 0, 100)}%` }}
              />
            </div>
            <div className="daily-cap-details">
              <span className="daily-cap-used">{(dashboard?.daily_used || 0).toLocaleString()} sent today</span>
              <span className="daily-cap-total">{(dashboard?.daily_capacity || 0).toLocaleString()} daily cap</span>
            </div>
            <div className="daily-cap-remaining">
              <strong>{(dashboard?.daily_remaining || 0).toLocaleString()}</strong> emails remaining today
            </div>
          </div>
          <div className="system-stats">
            <div className="stat">
              <span className="stat-value">{throttle?.minute_used || 0}/{throttle?.minute_limit || 1000}</span>
              <span className="stat-label">This Minute</span>
            </div>
            <div className="stat">
              <span className="stat-value">{throttle?.hour_used || 0}/{throttle?.hour_limit || 50000}</span>
              <span className="stat-label">This Hour</span>
            </div>
          </div>
        </div>

        <div className="system-card intelligence">
          <div className="system-header">
            <span className="system-icon"><FontAwesomeIcon icon={faBrain} /></span>
            <h3>Inbox Intelligence</h3>
            <span className="status-badge active">Learning</span>
          </div>
          <div className="system-description">
            <p>AI builds a <strong>profile for every recipient</strong> to optimize delivery.</p>
            <ul>
              <li>Tracks engagement per email address</li>
              <li>Learns best send times</li>
              <li>Predicts open/click probability</li>
            </ul>
          </div>
          <div className="system-stats">
            <div className="stat">
              <span className="stat-value">{dashboard?.inbox_profiles || 0}</span>
              <span className="stat-label">Profiles Built</span>
            </div>
          </div>
        </div>

        <div className="system-card suppression">
          <div className="system-header">
            <span className="system-icon"><FontAwesomeIcon icon={faBan} /></span>
            <h3>Deliverability Protection</h3>
            <span className="status-badge active">Protected</span>
          </div>
          <div className="system-description">
            <p><strong>Automatic suppression</strong> prevents sending to risky addresses.</p>
            <ul>
              <li>Hard bounces auto-blocked</li>
              <li>Spam complaints auto-blocked</li>
              <li>Manual suppression lists</li>
            </ul>
          </div>
          <div className="system-stats">
            <div className="stat">
              <span className="stat-value">{dashboard?.total_suppressions || 0}</span>
              <span className="stat-label">Blocked Addresses</span>
            </div>
          </div>
        </div>

        <div className="system-card automation">
          <div className="system-header">
            <span className="system-icon"><FontAwesomeIcon icon={faBolt} /></span>
            <h3>Automation Engine</h3>
            <span className="status-badge active">Running</span>
          </div>
          <div className="system-description">
            <p><strong>Drip campaigns</strong> send automatically based on triggers.</p>
            <ul>
              <li>Welcome series on subscribe</li>
              <li>Timed email sequences</li>
              <li>Behavior-based triggers</li>
            </ul>
          </div>
          <div className="system-stats">
            <div className="stat">
              <span className="stat-value">{dashboard?.active_automations || 0}</span>
              <span className="stat-label">Active Workflows</span>
            </div>
          </div>
        </div>
      </div>

      {/* Performance Metrics */}
      <div className="metrics-section">
        <h3><FontAwesomeIcon icon={faChartLine} /> Real-Time Performance</h3>
        <div className="metrics-grid">
          <div className="metric-card">
            <span className="metric-icon"><FontAwesomeIcon icon={faPaperPlane} /></span>
            <div className="metric-content">
              <span className="metric-value">{dashboard?.performance?.total_sent?.toLocaleString() || 0}</span>
              <span className="metric-label">Emails Sent</span>
            </div>
          </div>
          <div className="metric-card">
            <span className="metric-icon"><FontAwesomeIcon icon={faEnvelope} /></span>
            <div className="metric-content">
              <span className="metric-value">{dashboard?.performance?.open_rate ? `${(dashboard.performance.open_rate * 100).toFixed(1)}%` : '0%'}</span>
              <span className="metric-label">Open Rate</span>
            </div>
          </div>
          <div className="metric-card">
            <span className="metric-icon"><FontAwesomeIcon icon={faCrosshairs} /></span>
            <div className="metric-content">
              <span className="metric-value">{dashboard?.performance?.click_rate ? `${(dashboard.performance.click_rate * 100).toFixed(1)}%` : '0%'}</span>
              <span className="metric-label">Click Rate</span>
            </div>
          </div>
          <div className="metric-card">
            <span className="metric-icon"><FontAwesomeIcon icon={faChartPie} /></span>
            <div className="metric-content">
              <span className="metric-value">${dashboard?.performance?.revenue?.toFixed(2) || '0.00'}</span>
              <span className="metric-label">Revenue</span>
            </div>
          </div>
        </div>
      </div>

      {/* Quick Actions */}
      <div className="quick-actions">
        <h3><FontAwesomeIcon icon={faBolt} /> Quick Actions</h3>
        <div className="actions-grid">
          <button className="action-btn primary" onClick={() => window.location.hash = '#send'}>
            <span><FontAwesomeIcon icon={faPaperPlane} /></span>
            <div>
              <strong>Send Test Email</strong>
              <small>Verify delivery is working</small>
            </div>
          </button>
          <button className="action-btn" onClick={() => window.location.hash = '#campaigns'}>
            <span><FontAwesomeIcon icon={faEnvelope} /></span>
            <div>
              <strong>New Campaign</strong>
              <small>Create a broadcast email</small>
            </div>
          </button>
          <button className="action-btn" onClick={() => window.location.hash = '#import'}>
            <span><FontAwesomeIcon icon={faFileImport} /></span>
            <div>
              <strong>Import Subscribers</strong>
              <small>Upload a CSV file</small>
            </div>
          </button>
          <button className="action-btn" onClick={() => window.location.hash = '#automations'}>
            <span><FontAwesomeIcon icon={faBolt} /></span>
            <div>
              <strong>Create Automation</strong>
              <small>Set up a drip campaign</small>
            </div>
          </button>
        </div>
      </div>

      {/* Recent Activity */}
      <div className="recent-activity">
        <h3><FontAwesomeIcon icon={faListUl} /> Recent Campaigns</h3>
        <div className="activity-list">
          {dashboard?.recent_campaigns?.map((c: any, i: number) => (
            <div key={i} className="activity-item">
              <span className="activity-name">{c.name}</span>
              <span className="activity-status">{c.status}</span>
              <span className="activity-stats">
                {c.sent_count?.toLocaleString()} sent • {c.open_count?.toLocaleString()} opens
              </span>
            </div>
          )) || <p className="no-data">No campaigns yet</p>}
        </div>
      </div>
    </div>
  );
};

// Automations Manager
const AutomationsManager: React.FC = () => {
  const [automations, setAutomations] = useState<any[]>([]);
  const [lists, setLists] = useState<any[]>([]);
  const [showCreate, setShowCreate] = useState(false);
  const [newAuto, setNewAuto] = useState({ name: '', description: '', trigger_type: 'list_subscribe', list_id: '', steps: [] as any[] });
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    Promise.all([
      fetch('/api/mailing/automations').then(r => r.json()),
      fetch('/api/mailing/lists').then(r => r.json()),
    ]).then(([auto, lst]) => {
      setAutomations(auto.automations || []);
      setLists(lst.lists || []);
      setLoading(false);
    }).catch(() => setLoading(false));
  }, []);

  const createAutomation = async () => {
    try {
      const res = await fetch('/api/mailing/automations', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(newAuto),
      });
      if (res.ok) {
        const data = await res.json();
        setAutomations(prev => [data, ...prev]);
        setShowCreate(false);
      }
    } catch (err) {}
  };

  const activateAutomation = async (id: string) => {
    try {
      await fetch(`/api/mailing/automations/${id}/activate`, { method: 'POST' });
      setAutomations(prev => prev.map(a => a.id === id ? {...a, status: 'active'} : a));
    } catch (err) {}
  };

  const pauseAutomation = async (id: string) => {
    try {
      await fetch(`/api/mailing/automations/${id}/pause`, { method: 'POST' });
      setAutomations(prev => prev.map(a => a.id === id ? {...a, status: 'paused'} : a));
    } catch (err) {}
  };

  const addStep = (type: string) => {
    const order = newAuto.steps.length + 1;
    if (type === 'email') {
      setNewAuto(p => ({...p, steps: [...p.steps, { order, type: 'send_email', subject: '', html_content: '' }]}));
    } else if (type === 'wait') {
      setNewAuto(p => ({...p, steps: [...p.steps, { order, type: 'wait', wait_minutes: 1440 }]}));
    }
  };

  if (loading) return <div className="loading-state">Loading automations...</div>;

  return (
    <div className="manager-page">
      <div className="page-explanation">
        <h3>What are Automations?</h3>
        <p>Automations are <strong>drip campaigns</strong> that send emails automatically based on triggers. 
        For example, when someone subscribes, send a welcome email, wait 1 day, then send a follow-up.</p>
      </div>

      <div className="manager-header">
        <span>{automations.length} Automations</span>
        <button className="btn-primary" onClick={() => setShowCreate(true)}>+ Create Automation</button>
      </div>

      {showCreate && (
        <div className="create-form automation-form">
          <h4>Create Automation Workflow</h4>
          <input placeholder="Automation Name" value={newAuto.name} onChange={e => setNewAuto(p => ({...p, name: e.target.value}))} />
          <input placeholder="Description" value={newAuto.description} onChange={e => setNewAuto(p => ({...p, description: e.target.value}))} />
          
          <div className="form-group">
            <label>Trigger:</label>
            <select value={newAuto.trigger_type} onChange={e => setNewAuto(p => ({...p, trigger_type: e.target.value}))}>
              <option value="list_subscribe">When someone subscribes</option>
              <option value="tag_added">When tag is added</option>
              <option value="api_trigger">API trigger</option>
            </select>
          </div>
          
          <div className="form-group">
            <label>List:</label>
            <select value={newAuto.list_id} onChange={e => setNewAuto(p => ({...p, list_id: e.target.value}))}>
              <option value="">Select a List</option>
              {lists.map(l => <option key={l.id} value={l.id}>{l.name}</option>)}
            </select>
          </div>

          <div className="steps-builder">
            <label>Steps:</label>
            <div className="steps-list">
              {newAuto.steps.map((step, i) => (
                <div key={i} className="step-item">
                  {step.type === 'send_email' ? (
                    <>
                      <span className="step-icon">✉️</span>
                      <input placeholder="Email Subject" value={step.subject} onChange={e => {
                        const steps = [...newAuto.steps];
                        steps[i].subject = e.target.value;
                        setNewAuto(p => ({...p, steps}));
                      }} />
                    </>
                  ) : (
                    <>
                      <span className="step-icon">⏱️</span>
                      <span>Wait</span>
                      <input type="number" value={step.wait_minutes / 60} onChange={e => {
                        const steps = [...newAuto.steps];
                        steps[i].wait_minutes = parseInt(e.target.value) * 60;
                        setNewAuto(p => ({...p, steps}));
                      }} style={{width: 60}} />
                      <span>hours</span>
                    </>
                  )}
                </div>
              ))}
            </div>
            <div className="add-step-btns">
              <button type="button" onClick={() => addStep('email')}>+ Add Email</button>
              <button type="button" onClick={() => addStep('wait')}>+ Add Wait</button>
            </div>
          </div>

          <div className="form-actions">
            <button onClick={() => setShowCreate(false)}>Cancel</button>
            <button className="btn-primary" onClick={createAutomation}>Create</button>
          </div>
        </div>
      )}

      <div className="items-list">
        {automations.map(a => (
          <div key={a.id} className="list-item">
            <div className="item-main">
              <strong>{a.name}</strong>
              <span className="item-description">{a.description}</span>
            </div>
            <div className="item-meta">
              <span className="meta-badge">{a.total_enrolled || 0} enrolled</span>
              <span className={`status-badge ${a.status}`}>{a.status}</span>
            </div>
            <div className="item-actions">
              {a.status === 'active' ? (
                <button onClick={() => pauseAutomation(a.id)}>Pause</button>
              ) : (
                <button className="btn-primary" onClick={() => activateAutomation(a.id)}>Activate</button>
              )}
            </div>
          </div>
        ))}
        {automations.length === 0 && <p className="no-data">No automations yet. Create one to send emails automatically.</p>}
      </div>
    </div>
  );
};

// A/B Tests Manager
const ABTestsManager: React.FC = () => {
  const [tests, setTests] = useState<any[]>([]);
  const [campaigns, setCampaigns] = useState<any[]>([]);
  const [showCreate, setShowCreate] = useState(false);
  const [newTest, setNewTest] = useState({ campaign_id: '', test_type: 'subject', sample_size_percent: 20, winner_criteria: 'open_rate', variants: [{ name: 'A', subject: '' }, { name: 'B', subject: '' }] });
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    Promise.all([
      fetch('/api/mailing/ab-tests').then(r => r.json()),
      fetch('/api/mailing/campaigns').then(r => r.json()),
    ]).then(([ab, camp]) => {
      setTests(ab.tests || []);
      setCampaigns(camp.campaigns || []);
      setLoading(false);
    }).catch(() => setLoading(false));
  }, []);

  const createTest = async () => {
    try {
      const res = await fetch('/api/mailing/ab-tests', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(newTest),
      });
      if (res.ok) {
        const data = await res.json();
        setTests(prev => [data, ...prev]);
        setShowCreate(false);
      }
    } catch (err) {}
  };

  if (loading) return <div className="loading-state">Loading A/B tests...</div>;

  return (
    <div className="manager-page">
      <div className="page-explanation">
        <h3>What is A/B Testing?</h3>
        <p>A/B testing lets you <strong>compare two versions</strong> of an email to see which performs better. 
        Send version A to 10% of your list, version B to another 10%, then send the winner to the remaining 80%.</p>
      </div>

      <div className="manager-header">
        <span>{tests.length} A/B Tests</span>
        <button className="btn-primary" onClick={() => setShowCreate(true)}>+ Create A/B Test</button>
      </div>

      {showCreate && (
        <div className="create-form">
          <h4>Create A/B Test</h4>
          <div className="form-group">
            <label>Campaign:</label>
            <select value={newTest.campaign_id} onChange={e => setNewTest(p => ({...p, campaign_id: e.target.value}))}>
              <option value="">Select a Campaign</option>
              {campaigns.filter(c => c.status === 'draft').map(c => <option key={c.id} value={c.id}>{c.name}</option>)}
            </select>
          </div>
          <div className="form-group">
            <label>Test Type:</label>
            <select value={newTest.test_type} onChange={e => setNewTest(p => ({...p, test_type: e.target.value}))}>
              <option value="subject">Subject Line</option>
              <option value="content">Content</option>
              <option value="from_name">From Name</option>
              <option value="send_time">Send Time</option>
            </select>
          </div>
          <div className="form-group">
            <label>Sample Size: {newTest.sample_size_percent}% per variant</label>
            <input type="range" min="5" max="50" value={newTest.sample_size_percent} onChange={e => setNewTest(p => ({...p, sample_size_percent: parseInt(e.target.value)}))} />
          </div>
          <div className="variants-builder">
            <label>Variants:</label>
            {newTest.variants.map((v, i) => (
              <div key={i} className="variant-row">
                <span className="variant-label">Variant {v.name}:</span>
                <input placeholder="Subject line" value={v.subject} onChange={e => {
                  const variants = [...newTest.variants];
                  variants[i].subject = e.target.value;
                  setNewTest(p => ({...p, variants}));
                }} />
              </div>
            ))}
          </div>
          <div className="form-actions">
            <button onClick={() => setShowCreate(false)}>Cancel</button>
            <button className="btn-primary" onClick={createTest}>Create Test</button>
          </div>
        </div>
      )}

      <div className="items-list">
        {tests.map(t => (
          <div key={t.id} className="list-item">
            <div className="item-main">
              <strong>{t.campaign_name}</strong>
              <span className="item-description">Testing: {t.test_type} • Sample: {t.sample_size_percent}%</span>
            </div>
            <div className="item-meta">
              <span className={`status-badge ${t.status}`}>{t.status}</span>
            </div>
          </div>
        ))}
        {tests.length === 0 && <p className="no-data">No A/B tests yet. Create one to optimize your emails.</p>}
      </div>
    </div>
  );
};

// Delivery Servers Manager — renders from API response + PMTA servers
const DeliveryServersManager: React.FC = () => {
  const [servers, setServers] = useState<any[]>([]);
  const [pmtaServers, setPmtaServers] = useState<any[]>([]);
  const [profiles, setProfiles] = useState<any[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    Promise.all([
      fetch('/api/mailing/delivery-servers').then(r => r.json()).catch(() => ({ servers: [] })),
      fetch('/api/mailing/pmta/servers').then(r => r.json()).catch(() => ({ servers: [] })),
      fetch('/api/mailing/sending-profiles').then(r => r.json()).catch(() => ({ profiles: [] })),
    ]).then(([ds, pmta, prof]) => {
      setServers(ds.servers || []);
      setPmtaServers(pmta.servers || []);
      setProfiles((prof.profiles || []).filter((p: any) => p.vendor_type === 'pmta'));
      setLoading(false);
    });
  }, []);

  if (loading) return <div className="loading-state">Loading servers...</div>;

  const allServers = [
    ...pmtaServers.map((s: any) => ({ ...s, source: 'pmta-registry' })),
    ...servers.filter((s: any) => s.server_type === 'pmta').map((s: any) => ({ ...s, source: 'delivery-servers' })),
  ];

  const hasPMTA = allServers.length > 0 || profiles.length > 0;

  return (
    <div className="manager-page">
      <div className="page-explanation">
        <h3>PMTA Delivery Infrastructure</h3>
        <p>Your mail is routed through <strong>PowerMTA</strong> servers. Each server manages dedicated IPs, 
        DKIM signing, and ISP-specific routing for maximum deliverability.</p>
      </div>

      {!hasPMTA && (
        <div className="no-data" style={{textAlign:'center', padding:'40px 20px'}}>
          <p>No PMTA servers configured yet. Run the seed migration to populate infrastructure.</p>
        </div>
      )}

      <div className="servers-grid">
        {allServers.map((s, i) => (
          <div key={s.id || i} className="server-card">
            <div className="server-header">
              <span className="server-icon" style={{fontSize:'1.5em'}}>
                {String.fromCodePoint(0x1F4E8)}
              </span>
              <h4>{s.name || 'PMTA Server'}</h4>
              <span className={`status-dot ${s.status || s.health_status || 'active'}`}></span>
            </div>
            <p className="server-description">
              Host: <strong>{s.host || s.hostname || s.region || 'N/A'}</strong>
              {s.smtp_port ? ` | Port: ${s.smtp_port}` : ''}
              {s.provider ? ` | Provider: ${s.provider}` : ''}
            </p>
            <div className="server-stats">
              <div className="stat">
                <span className="stat-label">Type</span>
                <span className="stat-value">PMTA</span>
              </div>
              <div className="stat">
                <span className="stat-label">Status</span>
                <span className="stat-value capitalize">{s.status || s.health_status || 'active'}</span>
              </div>
              {(s.hourly_quota || s.daily_quota) && (
                <div className="stat">
                  <span className="stat-label">Quota</span>
                  <span className="stat-value">{(s.hourly_quota || 0).toLocaleString()}/hr</span>
                </div>
              )}
            </div>
          </div>
        ))}
      </div>

      {profiles.length > 0 && (
        <>
          <h4 style={{margin:'24px 0 12px', color:'#e2e8f0'}}>PMTA Sending Profiles</h4>
          <div className="items-list">
            {profiles.map((p: any) => (
              <div key={p.id} className="list-item">
                <div className="item-main">
                  <strong>{p.name}</strong>
                  <span className="item-description">{p.from_email} via {p.smtp_host}:{p.smtp_port}</span>
                </div>
                <div className="item-meta">
                  <span className="meta-badge">{(p.hourly_limit || 0).toLocaleString()}/hr</span>
                  <span className={`status-badge ${p.status}`}>{p.status}</span>
                </div>
              </div>
            ))}
          </div>
        </>
      )}

      <div className="server-info">
        <h4>How PMTA Sending Works</h4>
        <ol>
          <li><strong>PMTA Relay</strong> — Emails are relayed through your dedicated PMTA server with per-ISP routing rules.</li>
          <li><strong>Suppression Check</strong> — Before sending, each address is checked against bounces, complaints, and the global suppression hub.</li>
          <li><strong>IP Rotation</strong> — Messages rotate across your dedicated IP pool based on ISP and warmup stage.</li>
          <li><strong>DKIM Signing</strong> — PMTA applies domain-specific DKIM signatures on outbound mail.</li>
          <li><strong>Tracking</strong> — Opens and clicks are tracked through the platform's tracking pixel and link wrapper.</li>
        </ol>
      </div>
    </div>
  );
};

// Content Library — reusable email templates
const TemplatesManager: React.FC = () => {
  const [templates, setTemplates] = useState<any[]>([]);
  const [showCreate, setShowCreate] = useState(false);
  const [newTemplate, setNewTemplate] = useState({ name: '', description: '', subject: '', html_content: '', from_name: '', from_email: '' });
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    fetch('/api/mailing/templates')
      .then(r => r.json())
      .then(data => {
        setTemplates(data.templates || []);
        setLoading(false);
      })
      .catch(() => setLoading(false));
  }, []);

  const createTemplate = async () => {
    try {
      const res = await fetch('/api/mailing/templates', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(newTemplate),
      });
      if (res.ok) {
        const data = await res.json();
        setTemplates(prev => [data, ...prev]);
        setShowCreate(false);
        setNewTemplate({ name: '', description: '', subject: '', html_content: '', from_name: '', from_email: '' });
      }
    } catch (err) {}
  };

  const cloneTemplate = async (id: string) => {
    try {
      const res = await fetch(`/api/mailing/templates/${id}/clone`, { method: 'POST' });
      if (res.ok) {
        const data = await res.json();
        setTemplates(prev => [data, ...prev]);
      }
    } catch (err) {}
  };

  if (loading) return <div className="loading-state">Loading templates...</div>;

  return (
    <div className="manager-page">
      <div className="page-explanation">
        <h3>What are Templates?</h3>
        <p>Templates are <strong>reusable email designs</strong> that you can use across multiple campaigns. 
        Create once, use many times.</p>
      </div>

      <div className="manager-header">
        <span>{templates.length} Templates</span>
        <button className="btn-primary" onClick={() => setShowCreate(true)}>+ Create Template</button>
      </div>

      {showCreate && (
        <div className="create-form template-form">
          <h4>Create Email Template</h4>
          <input placeholder="Template Name" value={newTemplate.name} onChange={e => setNewTemplate(p => ({...p, name: e.target.value}))} />
          <input placeholder="Description" value={newTemplate.description} onChange={e => setNewTemplate(p => ({...p, description: e.target.value}))} />
          <input placeholder="Default Subject" value={newTemplate.subject} onChange={e => setNewTemplate(p => ({...p, subject: e.target.value}))} />
          <input placeholder="From Name" value={newTemplate.from_name} onChange={e => setNewTemplate(p => ({...p, from_name: e.target.value}))} />
          <input placeholder="From Email" value={newTemplate.from_email} onChange={e => setNewTemplate(p => ({...p, from_email: e.target.value}))} />
          <textarea placeholder="HTML Content" value={newTemplate.html_content} onChange={e => setNewTemplate(p => ({...p, html_content: e.target.value}))} rows={10} />
          <div className="form-actions">
            <button onClick={() => setShowCreate(false)}>Cancel</button>
            <button className="btn-primary" onClick={createTemplate}>Create</button>
          </div>
        </div>
      )}

      <div className="items-list">
        {templates.map(t => (
          <div key={t.id} className="list-item">
            <div className="item-main">
              <strong>{t.name}</strong>
              <span className="item-description">{t.description || t.subject}</span>
            </div>
            <div className="item-meta">
              <span className={`status-badge ${t.status}`}>{t.status}</span>
            </div>
            <div className="item-actions">
              <button onClick={() => cloneTemplate(t.id)}>Clone</button>
            </div>
          </div>
        ))}
        {templates.length === 0 && <p className="no-data">No templates yet. Create one to speed up campaign creation.</p>}
      </div>
    </div>
  );
};

// Legacy Analytics Dashboard Component (replaced by AnalyticsCenter)
// @ts-ignore: Kept as reference
const _AnalyticsDashboard: React.FC = () => {
  const [optimalSend, setOptimalSend] = React.useState<any>(null);
  const [overview, setOverview] = React.useState<any>(null);
  const [loading, setLoading] = React.useState(true);

  React.useEffect(() => {
    Promise.all([
      fetch('/api/mailing/analytics/optimal-send').then(r => r.json()),
      fetch('/api/mailing/analytics/overview?days=30').then(r => r.json()),
    ]).then(([opt, ovr]) => {
      setOptimalSend(opt);
      setOverview(ovr);
      setLoading(false);
    }).catch(() => setLoading(false));
  }, []);

  if (loading) return <div className="loading-state">Loading analytics...</div>;

  return (
    <div className="analytics-dashboard">
      <div className="page-explanation">
        <h3>Analytics & Insights</h3>
        <p>Review your email performance metrics and get <strong>AI-powered recommendations</strong> for improvement.</p>
      </div>

      <div className="analytics-grid">
        <div className="analytics-card optimal-time">
          <h3>🎯 Optimal Send Time</h3>
          {optimalSend && (
            <>
              <div className="optimal-display">
                <div className="optimal-day">{optimalSend.optimal_day_name}</div>
                <div className="optimal-hour">{optimalSend.optimal_hour}:00</div>
              </div>
              <div className="confidence">
                <span>Confidence: {(optimalSend.confidence * 100).toFixed(0)}%</span>
                <div className="confidence-bar">
                  <div style={{ width: `${optimalSend.confidence * 100}%` }} />
                </div>
              </div>
              <div className="reasoning-list">
                <h4>Why this time?</h4>
                <ul>
                  {optimalSend.reasoning?.map((r: string, i: number) => (
                    <li key={i}>{r}</li>
                  ))}
                </ul>
              </div>
            </>
          )}
        </div>

        <div className="analytics-card period-stats">
          <h3>📊 30-Day Performance</h3>
          {overview && (
            <div className="stats-grid">
              <div className="stat-item">
                <span className="stat-value">{overview.totals?.sent?.toLocaleString() || 0}</span>
                <span className="stat-label">Emails Sent</span>
              </div>
              <div className="stat-item">
                <span className="stat-value">{overview.rates?.open_rate?.toFixed(1) || 0}%</span>
                <span className="stat-label">Open Rate</span>
              </div>
              <div className="stat-item">
                <span className="stat-value">{overview.rates?.click_rate?.toFixed(1) || 0}%</span>
                <span className="stat-label">Click Rate</span>
              </div>
              <div className="stat-item">
                <span className="stat-value">{overview.rates?.bounce_rate?.toFixed(2) || 0}%</span>
                <span className="stat-label">Bounce Rate</span>
              </div>
            </div>
          )}
        </div>

        <div className="analytics-card best-practices">
          <h3>💡 Industry Best Practices</h3>
          <div className="practice-list">
            <div className="practice-item">
              <span className="practice-icon">📅</span>
              <div>
                <strong>Timing</strong>
                <p>Tuesday-Thursday 9-11am shows highest engagement</p>
              </div>
            </div>
            <div className="practice-item">
              <span className="practice-icon">📱</span>
              <div>
                <strong>Mobile First</strong>
                <p>60%+ opens are on mobile - ensure responsive design</p>
              </div>
            </div>
            <div className="practice-item">
              <span className="practice-icon">✍️</span>
              <div>
                <strong>Subject Lines</strong>
                <p>Keep under 50 characters, test emojis and personalization</p>
              </div>
            </div>
            <div className="practice-item">
              <span className="practice-icon">🎨</span>
              <div>
                <strong>CTA Buttons</strong>
                <p>Above-the-fold placement increases clicks by 30%</p>
              </div>
            </div>
          </div>
        </div>

        <div className="analytics-card benchmarks">
          <h3>📊 Industry Benchmarks</h3>
          <div className="benchmark-grid">
            <div className="benchmark">
              <div className="benchmark-value">15-25%</div>
              <div className="benchmark-label">Open Rate</div>
            </div>
            <div className="benchmark">
              <div className="benchmark-value">2.5-4%</div>
              <div className="benchmark-label">Click Rate</div>
            </div>
            <div className="benchmark">
              <div className="benchmark-value">&lt;0.1%</div>
              <div className="benchmark-label">Complaint Rate</div>
            </div>
            <div className="benchmark">
              <div className="benchmark-value">&lt;2%</div>
              <div className="benchmark-label">Bounce Rate</div>
            </div>
          </div>
        </div>

        <div className="analytics-card suggestions">
          <h3>💭 Improvement Suggestions</h3>
          <SuggestionsWidget />
        </div>
      </div>
    </div>
  );
};

// Suggestions Widget
const SuggestionsWidget: React.FC = () => {
  const [suggestions, setSuggestions] = React.useState<any[]>([]);
  const [newSuggestion, setNewSuggestion] = React.useState('');
  const [category, setCategory] = React.useState('content');

  React.useEffect(() => {
    fetch('/api/mailing/suggestions')
      .then(res => res.json())
      .then(data => setSuggestions(data.suggestions || []))
      .catch(() => {});
  }, []);

  const addSuggestion = async () => {
    if (!newSuggestion) return;
    try {
      const res = await fetch('/api/mailing/suggestions', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ category, description: newSuggestion, impact: 'TBD' }),
      });
      if (res.ok) {
        const data = await res.json();
        setSuggestions(prev => [data, ...prev]);
        setNewSuggestion('');
      }
    } catch (err) {}
  };

  return (
    <div className="suggestions-widget">
      <div className="add-suggestion">
        <select value={category} onChange={(e) => setCategory(e.target.value)}>
          <option value="content">Content</option>
          <option value="timing">Timing</option>
          <option value="targeting">Targeting</option>
          <option value="creative">Creative</option>
        </select>
        <input
          type="text"
          placeholder="Add your suggestion..."
          value={newSuggestion}
          onChange={(e) => setNewSuggestion(e.target.value)}
          onKeyPress={(e) => e.key === 'Enter' && addSuggestion()}
        />
        <button onClick={addSuggestion}>Add</button>
      </div>
      <div className="suggestions-list">
        {suggestions.slice(0, 5).map((s, i) => (
          <div key={i} className="suggestion-item">
            <span className="suggestion-category">{s.category}</span>
            <span className="suggestion-text">{s.description}</span>
            <span className={`suggestion-status ${s.status}`}>{s.status}</span>
          </div>
        ))}
        {suggestions.length === 0 && <p className="no-data">No suggestions yet. Add your ideas!</p>}
      </div>
    </div>
  );
};

export default MailingPortal;

