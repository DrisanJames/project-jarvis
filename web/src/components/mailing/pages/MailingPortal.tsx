import React, { useState, useEffect, useCallback, Suspense, lazy } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { 
  faChartLine, faEnvelope, faBullhorn, faPaperPlane, faCalculator,
  faListUl, faCrosshairs, faBolt,
  faBan, faBrain, faRobot, faChartPie, faServer, faDatabase,
  /* faArrowLeft, */ faGlobe, faStore,
  faSpinner, faSeedling, faWandMagicSparkles,
} from '@fortawesome/free-solid-svg-icons';
import { IconDefinition } from '@fortawesome/fontawesome-svg-core';
import { useAuth } from '../../../contexts/AuthContext';
import './MailingPortal.css';
import '../shared/animations.css';
import { ToastProvider } from '../shared/ToastSystem';
import { apiFetch } from '../shared/apiFetch';
import { WorkerHealthWidget } from '../components/WorkerHealthDashboard';

// ── Lazy-loaded heavy components (code-split into separate chunks) ──────────
const ListPortal = lazy(() => import('../components/ListPortal').then(m => ({ default: m.ListPortal })));
const CampaignPortal = lazy(() => import('../components/CampaignPortal').then(m => ({ default: m.CampaignPortal })));
const ISPAgentIntelligence = lazy(() => import('../components/ISPAgentIntelligence').then(m => ({ default: m.ISPAgentIntelligence })));
const SuppressionPortal = lazy(() => import('../components/SuppressionPortal').then(m => ({ default: m.SuppressionPortal })));
const InboxProfiles = lazy(() => import('../components/InboxProfiles').then(m => ({ default: m.InboxProfiles })));
const SendTestEmail = lazy(() => import('../components/SendTestEmail').then(m => ({ default: m.SendTestEmail })));
const MissionControl = lazy(() => import('../components/MissionControl').then(m => ({ default: m.MissionControl })));
const DomainCenter = lazy(() => import('../components/DomainCenter').then(m => ({ default: m.DomainCenter })));
const DomainAgents = lazy(() => import('../components/DomainAgents').then(m => ({ default: m.DomainAgents })));
// AnalyticsCenter retired 2026-06-09: the Event Lake Explorer (now labeled
// "Analytics") is the operator's analytics surface. Component file kept for
// reference; no longer mounted.
const OfferManagement = lazy(() => import('../components/OfferManagement').then(m => ({ default: m.OfferManagement })));
const JarvisDashboard = lazy(() => import('../components/JarvisDashboard').then(m => ({ default: m.JarvisDashboard })));
const PMTACampaignWizard = lazy(() => import('../components/PMTACampaignWizard').then(m => ({ default: m.PMTACampaignWizard })));
const SendDayPlanner = lazy(() => import('../components/SendDayPlanner').then(m => ({ default: m.SendDayPlanner })));
const ConsciousnessDashboard = lazy(() => import('../components/ConsciousnessDashboard').then(m => ({ default: m.ConsciousnessDashboard })));
const GlobalSuppressionDashboard = lazy(() => import('../components/GlobalSuppressionDashboard').then(m => ({ default: m.GlobalSuppressionDashboard })));
const CampaignCopilotPanel = lazy(() => import('../components/CampaignCopilot').then(m => ({ default: m.CampaignCopilot })));
const EmailMarketingAgentPanel = lazy(() => import('../components/EmailMarketingAgent').then(m => ({ default: m.EmailMarketingAgent })));
// WarmupDashboard tab retired 2026-04-27. The IP Activity panel inside
// DeliverabilityControl now surfaces the active/cold/paused IP profile
// (including never-mailed-on IPs) the operator needs. The component file
// and /api/mailing/warmup/dashboard backend remain in place as a safety
// follow-up; nothing else imports them and they cause no harm.
// AudienceAnalytics (2026-06-09) replaced WelcomeAudienceHealth as the
// 'audience-health' tab; the welcome-pool gauge lives on as a sub-tab inside it.
const AudienceAnalytics = lazy(() => import('../components/AudienceAnalytics').then(m => ({ default: m.AudienceAnalytics })));
const AudienceCadenceByCell = lazy(() => import('../components/AudienceCadenceByCell').then(m => ({ default: m.AudienceCadenceByCell })));
const EventLakeExplorer = lazy(() => import('../components/EventLakeExplorer').then(m => ({ default: m.EventLakeExplorer })));
const CreativeStudio = lazy(() => import('../components/CreativeStudio').then(m => ({ default: m.CreativeStudio })));
const OutboxDashboard = lazy(() => import('../components/OutboxDashboard').then(m => ({ default: m.OutboxDashboard })));
const PartnerIngestPortal = lazy(() => import('../datapartners/PartnerIngestPortal').then(m => ({ default: m.PartnerIngestPortal })));
const CpmPlanner = lazy(() => import('../components/CpmPlanner').then(m => ({ default: m.CpmPlanner })));
const SendScorecards = lazy(() => import('../components/SendScorecards').then(m => ({ default: m.SendScorecards })));

// ── Suspense fallback ───────────────────────────────────────────────────────
const ChunkLoader: React.FC = () => (
  <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '50vh', color: 'rgba(180,210,240,0.65)', gap: 10, fontSize: 14 }}>
    <FontAwesomeIcon icon={faSpinner} spin /> Loading…
  </div>
);

type TabId = 'dashboard' | 'lists' | 'campaign-center' | 'suppressions' | 'global-suppression' | 'profiles' | 'send' | 'sending-plans' | 'domain-center' | 'domain-agents' | 'delivery-servers' | 'offers' | 'analytics' | 'segments' | 'automations' | 'ab-tests' | 'import' | 'mission-control' | 'jarvis' | 'pmta-wizard' | 'send-day' | 'consciousness' | 'content-library' | 'marketing-agent' | 'ai-agents' | 'outbox' | 'audience-health' | 'audience-cadence' | 'event-lake' | 'data-partners' | 'creative-studio' | 'cpm-planner' | 'scorecards';

interface Tab {
  id: TabId;
  label: string;
  icon: IconDefinition;
  description: string;
  childIds?: TabId[];
}

const tabs: Tab[] = [
  { id: 'dashboard', label: 'Dashboard', icon: faChartLine, description: 'Real-time overview of email performance' },
  { id: 'campaign-center', label: 'Campaign Center', icon: faBullhorn, description: 'Create, manage & monitor campaigns', childIds: ['campaign-center', 'pmta-wizard', 'send-day', 'marketing-agent'] },
  { id: 'lists', label: 'Segments', icon: faListUl, description: 'Manage segments, lists & subscribers' },
  { id: 'suppressions', label: 'Suppressions', icon: faBan, description: 'Manage suppression lists & global hub', childIds: ['suppressions', 'global-suppression'] },
  { id: 'ai-agents', label: 'AI Agents', icon: faBrain, description: 'AI-powered insights — ISP agents, inbox intelligence & Jarvis', childIds: ['sending-plans', 'profiles', 'jarvis'] },
  { id: 'domain-center', label: 'Domain Center', icon: faGlobe, description: 'Sending, tracking & image domains' },
  { id: 'domain-agents', label: 'Domain Agents', icon: faRobot, description: 'Per-domain agentic send planning & approval' },
  { id: 'scorecards', label: 'Scorecards', icon: faChartLine, description: 'Send baselines & verdicts per domain × ISP — increase/decrease/maintain — plus historical domain × offer × ISP scorecards' },
  { id: 'cpm-planner', label: 'CPM Planner', icon: faCalculator, description: 'Price CPM deals — planned volume, pace, capacity risk & live earnings vs goal' },
  { id: 'offers', label: 'Offers', icon: faStore, description: 'Offer lifecycle — creatives, compliance, deployment & attribution' },
  { id: 'creative-studio', label: 'Creative Studio', icon: faWandMagicSparkles, description: 'ReviewForge creative archive — browse & preview pipeline-built newsletters per offer × brand' },
  { id: 'event-lake', label: 'Analytics', icon: faChartPie, description: 'S3/Athena email-event lake — ISP, brand & campaign analytics' },
  { id: 'audience-health', label: 'Audience', icon: faSeedling, description: 'Audience analytics — acquisition, churn, source performance, member lookup & welcome pool' },
  { id: 'audience-cadence', label: 'Audience Cadence', icon: faChartLine, description: 'Per (sending_domain × ISP) refresh cadence, churn & 1% activation target' },
  { id: 'content-library', label: 'Content Library', icon: faEnvelope, description: 'Reusable email templates & content blocks' },
  { id: 'delivery-servers', label: 'Servers', icon: faServer, description: 'PMTA servers, IPs & sending infrastructure' },
  { id: 'consciousness', label: 'Consciousness', icon: faCrosshairs, description: 'AI beliefs, philosophies & campaign intelligence' },
  { id: 'data-partners', label: 'Data Partners', icon: faDatabase, description: 'Inbound data partner ingestion — API keys, batches, drip orchestrator, creatives' },
  { id: 'outbox', label: 'Outbox', icon: faPaperPlane, description: 'Durable injection outbox — live state, stuck rows & dead-letter queue' },
];

interface VersionInfo {
  version: string;
  git_sha: string;
  build_time: string;
  go_version: string;
  deployed_at: string;
}

export const MailingPortal: React.FC = () => {
  const { organization } = useAuth();
  const [activeTab, setActiveTab] = useState<TabId>('dashboard');
  const [realTimeStats, setRealTimeStats] = useState<any>(null);
  const [versionInfo, setVersionInfo] = useState<VersionInfo | null>(null);

  // Cross-component offer state — when user clicks "Use This Offer" in Offer Center,
  // we switch to Campaign Center and pass the selected offer through.
  const [pendingOffer, setPendingOffer] = useState<{ offerId: string; offerName: string } | null>(null);
  const [copilotOpen, setCopilotOpen] = useState(false);

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
      apiFetch('/api/mailing/dashboard', { headers, credentials: 'include' })
        .then(res => res.json())
        .then(data => setRealTimeStats(data))
        .catch(() => {});
    };
    fetchStats();
    const interval = setInterval(fetchStats, 30000);
    return () => clearInterval(interval);
  }, [organization]);

  // Fetch version info once on mount
  useEffect(() => {
    apiFetch('/api/mailing/version', { credentials: 'include' })
      .then(res => res.json())
      .then(data => setVersionInfo(data))
      .catch(() => {});
  }, []);

  const renderContent = () => {
    switch (activeTab) {
      case 'dashboard':
        return <EnhancedDashboard />;
      case 'lists':
        return <ListPortal />;
      case 'campaign-center':
      case 'pmta-wizard':
      case 'send-day':
      case 'marketing-agent':
        return <CampaignCenterSection activeSubTab={activeTab} onSubTabChange={setActiveTab} pendingOffer={pendingOffer} onOfferConsumed={() => setPendingOffer(null)} copilotOpen={copilotOpen} setCopilotOpen={setCopilotOpen} />;
      case 'sending-plans':
      case 'profiles':
      case 'jarvis':
      case 'ai-agents':
        return <AIAgentsSection activeSubTab={activeTab === 'ai-agents' ? 'sending-plans' : activeTab} onSubTabChange={setActiveTab} />;
      case 'domain-center':
        return <DomainCenter />;
      case 'domain-agents':
        return <Suspense fallback={<ChunkLoader />}><DomainAgents /></Suspense>;
      case 'suppressions':
      case 'global-suppression':
        return <SuppressionsSection activeSubTab={activeTab} onSubTabChange={setActiveTab} />;
      case 'send':
        return <SendTestEmail />;
      case 'analytics': // legacy id — AnalyticsCenter retired, alias to the lake explorer
      case 'event-lake':
        return <Suspense fallback={<ChunkLoader />}><EventLakeExplorer /></Suspense>;
      case 'creative-studio':
        return <Suspense fallback={<ChunkLoader />}><CreativeStudio /></Suspense>;
      case 'audience-health':
        return <Suspense fallback={<ChunkLoader />}><AudienceAnalytics /></Suspense>;
      case 'audience-cadence':
        return <Suspense fallback={<ChunkLoader />}><AudienceCadenceByCell /></Suspense>;
      case 'content-library':
        return <TemplatesManager />;
      case 'delivery-servers':
        return <DeliveryServersManager />;
      case 'offers':
        return <OfferManagement />;
      case 'automations':
        return <AutomationsManager />;
      case 'ab-tests':
        return <ABTestsManager />;
      case 'mission-control':
        return <MissionControl />;
      case 'consciousness':
        return <ConsciousnessDashboard />;
      case 'data-partners':
        return <Suspense fallback={<ChunkLoader />}><PartnerIngestPortal /></Suspense>;
      case 'outbox':
        return <Suspense fallback={<ChunkLoader />}><OutboxDashboard /></Suspense>;
      case 'cpm-planner':
        return <Suspense fallback={<ChunkLoader />}><CpmPlanner /></Suspense>;
      case 'scorecards':
        return <Suspense fallback={<ChunkLoader />}><SendScorecards /></Suspense>;
      default:
        return <EnhancedDashboard />;
    }
  };

  const resolveCurrentTab = (): Tab | undefined => {
    const direct = tabs.find(t => t.id === activeTab);
    if (direct) return direct;
    return tabs.find(t => t.childIds?.includes(activeTab));
  };
  const currentTab = resolveCurrentTab();

  return (
    <ToastProvider>
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
          {tabs.map((tab) => {
            const isActive = tab.childIds
              ? tab.childIds.includes(activeTab) || activeTab === tab.id
              : activeTab === tab.id;
            return (
              <button
                key={tab.id}
                className={`nav-item ${isActive ? 'active' : ''}`}
                onClick={() => handleTabChange(tab.id)}
                title={tab.description}
              >
                <span className="nav-icon"><FontAwesomeIcon icon={tab.icon} /></span>
                <span className="nav-label">{tab.label}</span>
              </button>
            );
          })}
        </nav>

        <div className="sidebar-footer">
          <div className="quick-stats">
            <div className="quick-stat">
              <span className="quick-stat-value">{realTimeStats?.platform_intelligence?.active_audience_60d?.toLocaleString() || '—'}</span>
              <span className="quick-stat-label">Active Audience</span>
            </div>
            <div className="quick-stat">
              <span className="quick-stat-value">{realTimeStats?.platform_intelligence?.global_churn_pct != null ? `${realTimeStats.platform_intelligence.global_churn_pct.toFixed(2)}%` : '—'}</span>
              <span className="quick-stat-label">Global Churn</span>
            </div>
            <div className="quick-stat">
              <span className="quick-stat-value">{realTimeStats?.platform_intelligence?.global_intro_pct != null ? `${realTimeStats.platform_intelligence.global_intro_pct.toFixed(1)}%` : '—'}</span>
              <span className="quick-stat-label">Global Intro</span>
            </div>
          </div>
          <div className="connection-status">
            <span className={`status-dot ${realTimeStats?.pmta_connected ? 'active' : ''}`}></span>
            <span>{realTimeStats?.pmta_connected ? `PMTA Connected (${realTimeStats.pmta_server_count})` : realTimeStats ? 'PMTA Offline' : 'Connecting...'}</span>
          </div>
          {versionInfo && (
            <div className="sidebar-version-info">
              <div className="version-row">
                <span className="version-label">Version</span>
                <span className="version-value">
                  {versionInfo.git_sha
                    ? versionInfo.git_sha.slice(0, 7)
                    : versionInfo.version || 'dev'}
                </span>
              </div>
              <div className="version-row">
                <span className="version-label">Runtime</span>
                <span className="version-value">{versionInfo.go_version || '—'}</span>
              </div>
              <div className="version-row">
                <span className="version-label">Deployed</span>
                <span className="version-value">
                  {versionInfo.deployed_at
                    ? new Date(versionInfo.deployed_at).toLocaleDateString('en-US', { month: 'short', day: 'numeric' }) + ' ' +
                      new Date(versionInfo.deployed_at).toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit' })
                    : '—'}
                </span>
              </div>
            </div>
          )}
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
    </ToastProvider>
  );
};

// Enhanced Dashboard with System Explanations
//
// PAGE_VERSION is bumped on every behavior/UX change per workspace rule
// testing.mdc so we can confirm a deploy reached the browser.
//
// History:
//   1.0 (2026-05-08) — single dashboard fetch (drops the duplicate
//     /api/mailing/throttle/status call), reads throttle from
//     dashboard.throttle_status, sends X-Organization-ID, surfaces Hard
//     Bounce + Soft Bounce as separate tiles per bounce-metrics.mdc, splits
//     the platform sending gauge into platform-wide vs org's contribution.
const PAGE_VERSION_DASHBOARD = '1.0';

const EnhancedDashboard: React.FC = () => {
  const { organization } = useAuth();
  const [dashboard, setDashboard] = useState<any>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    const headers: HeadersInit = { 'Content-Type': 'application/json' };
    if (organization?.id) {
      headers['X-Organization-ID'] = organization.id;
    }
    // Single round-trip: throttle is delivered as dashboard.throttle_status.
    // The previous code did two GETs (the second being /throttle/status)
    // which returned the exact same payload as dashboard.throttle_status.
    apiFetch('/api/mailing/dashboard', { headers, credentials: 'include' })
      .then(r => r.json())
      .then(dash => {
        setDashboard(dash);
        setLoading(false);
      })
      .catch(() => setLoading(false));
  }, [organization]);

  if (loading) return <div className="loading-state">Loading dashboard...</div>;

  const throttle = dashboard?.throttle_status || {};

  return (
    <div className="enhanced-dashboard ig-fade-in">
      {/* System Overview Cards */}
      <div className="system-overview ig-stagger">
        <WorkerHealthWidget />
        <div className="system-card sending ig-card-hover ig-scan-line">
          <div className="system-header">
            <span className="system-icon"><FontAwesomeIcon icon={faPaperPlane} /></span>
            <h3>Email Sending</h3>
            <span className="status-badge active">Active</span>
          </div>
          <div className="system-description">
            <p>Production emails are routed through <strong>PowerMTA</strong> with per-ISP delivery optimization.</p>
          </div>
          {/* Daily Cap Gauge */}
          <div className="daily-cap-section">
            <div className="daily-cap-header">
              <span className="daily-cap-title">Platform Sending Cap</span>
              <span className={`daily-cap-pct ${(dashboard?.platform_daily_utilization || 0) > 90 ? 'critical' : (dashboard?.platform_daily_utilization || 0) > 70 ? 'warning' : 'healthy'}`}>
                {(dashboard?.platform_daily_utilization || 0).toFixed(1)}% used
              </span>
            </div>
            <div className="daily-cap-bar">
              <div
                className={`daily-cap-fill ${(dashboard?.platform_daily_utilization || 0) > 90 ? 'critical' : (dashboard?.platform_daily_utilization || 0) > 70 ? 'warning' : 'healthy'}`}
                style={{ width: `${Math.min(dashboard?.platform_daily_utilization || 0, 100)}%` }}
              />
            </div>
            <div className="daily-cap-details">
              <span className="daily-cap-used">{(dashboard?.platform_daily_sent ?? dashboard?.daily_used ?? 0).toLocaleString()} sent today (platform)</span>
              <span className="daily-cap-total">{(dashboard?.platform_daily_capacity || 0).toLocaleString()} platform cap</span>
            </div>
            <div className="daily-cap-details" style={{ fontSize: '0.78em', opacity: 0.75 }}>
              <span>Your org: {(dashboard?.org_daily_sent ?? dashboard?.daily_used ?? 0).toLocaleString()} sent</span>
              <span>v{PAGE_VERSION_DASHBOARD}</span>
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

        <div className="system-card intelligence ig-card-hover ig-scan-line">
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
              <span className="stat-value">{(dashboard?.platform_intelligence?.inbox_profiles_today || 0).toLocaleString()}</span>
              <span className="stat-label">Built Today</span>
            </div>
            <div className="stat">
              <span className="stat-value" style={{ fontSize: '0.85em', opacity: 0.7 }}>{(dashboard?.platform_intelligence?.inbox_profiles || 0).toLocaleString()}</span>
              <span className="stat-label">Total Profiles</span>
            </div>
          </div>
        </div>

        <div className="system-card suppression ig-card-hover ig-scan-line">
          <div className="system-header">
            <span className="system-icon"><FontAwesomeIcon icon={faBan} /></span>
            <h3>Deliverability Protection</h3>
            <span className="status-badge active">Protected</span>
          </div>
          <div className="system-description">
            <p><strong>Global suppression</strong> prevents sending to risky addresses.</p>
            <p style={{ fontSize: '0.8rem', opacity: 0.7, margin: '4px 0 0' }}>
              {(dashboard?.global_suppressions_total || 0).toLocaleString()} total blocked addresses
            </p>
          </div>
          <div className="system-stats">
            <div className="stat">
              <span className="stat-value">{(dashboard?.suppressions_today || 0).toLocaleString()}</span>
              <span className="stat-label">Added Today</span>
            </div>
            <div className="stat">
              <span className="stat-value" style={{ fontSize: '1.2rem', opacity: 0.7 }}>{(dashboard?.suppressions_yesterday || 0).toLocaleString()}</span>
              <span className="stat-label">Added Yesterday</span>
            </div>
          </div>
        </div>

        <div className="system-card automation ig-card-hover ig-scan-line">
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
        <h3><FontAwesomeIcon icon={faChartLine} /> Today's Performance</h3>
        <div className="metrics-grid">
          <div className="metric-card">
            <span className="metric-icon"><FontAwesomeIcon icon={faPaperPlane} /></span>
            <div className="metric-content">
              <span className="metric-value">{dashboard?.performance?.delivered?.toLocaleString() || 0}</span>
              <span className="metric-label">Delivered</span>
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
              <span className="metric-value">${(dashboard?.performance?.total_revenue ?? dashboard?.performance?.revenue ?? 0).toFixed ? (dashboard?.performance?.total_revenue ?? dashboard?.performance?.revenue ?? 0).toFixed(2) : '0.00'}</span>
              <span className="metric-label">Revenue</span>
            </div>
          </div>
          {/*
            Per .cursor/rules/bounce-metrics.mdc:
              Never display a single combined "Bounced" metric. Always break
              bounces into hard (#ef4444 = red) and soft (#f59e0b = amber).
            The fields hard_bounces / soft_bounces are returned by HandleDashboard
            from ComputeMetrics; surfacing them here closes the visibility gap
            the audit identified (the Dashboard previously showed zero bounce
            information at all).
          */}
          <div className="metric-card">
            <span className="metric-icon" style={{ color: (dashboard?.performance?.hard_bounces || 0) > 0 ? '#ef4444' : '#475569' }}>
              <FontAwesomeIcon icon={faBan} />
            </span>
            <div className="metric-content">
              <span className="metric-value" style={{ color: (dashboard?.performance?.hard_bounces || 0) > 0 ? '#ef4444' : undefined }}>
                {(dashboard?.performance?.hard_bounces || 0).toLocaleString()}
              </span>
              <span className="metric-label">
                Hard Bounce {dashboard?.performance?.hard_bounce_rate != null && (dashboard.performance.hard_bounce_rate > 0) ? `(${dashboard.performance.hard_bounce_rate.toFixed(2)}%)` : ''}
              </span>
            </div>
          </div>
          <div className="metric-card">
            <span className="metric-icon" style={{ color: (dashboard?.performance?.soft_bounces || 0) > 0 ? '#f59e0b' : '#475569' }}>
              <FontAwesomeIcon icon={faBan} />
            </span>
            <div className="metric-content">
              <span className="metric-value" style={{ color: (dashboard?.performance?.soft_bounces || 0) > 0 ? '#f59e0b' : undefined }}>
                {(dashboard?.performance?.soft_bounces || 0).toLocaleString()}
              </span>
              <span className="metric-label">
                Soft Bounce {dashboard?.performance?.soft_bounce_rate != null && (dashboard.performance.soft_bounce_rate > 0) ? `(${dashboard.performance.soft_bounce_rate.toFixed(2)}%)` : ''}
              </span>
            </div>
          </div>
        </div>
      </div>

      {/* Quick Actions - disabled
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
      */}

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
      apiFetch('/api/mailing/automations').then(r => r.json()),
      apiFetch('/api/mailing/lists').then(r => r.json()),
    ]).then(([auto, lst]) => {
      setAutomations(auto.automations || []);
      setLists(lst.lists || []);
      setLoading(false);
    }).catch(() => setLoading(false));
  }, []);

  const createAutomation = async () => {
    try {
      const res = await apiFetch('/api/mailing/automations', {
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
      await apiFetch(`/api/mailing/automations/${id}/activate`, { method: 'POST' });
      setAutomations(prev => prev.map(a => a.id === id ? {...a, status: 'active'} : a));
    } catch (err) {}
  };

  const pauseAutomation = async (id: string) => {
    try {
      await apiFetch(`/api/mailing/automations/${id}/pause`, { method: 'POST' });
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
      apiFetch('/api/mailing/ab-tests').then(r => r.json()),
      apiFetch('/api/mailing/campaigns').then(r => r.json()),
    ]).then(([ab, camp]) => {
      setTests(ab.tests || []);
      setCampaigns(camp.campaigns || []);
      setLoading(false);
    }).catch(() => setLoading(false));
  }, []);

  const createTest = async () => {
    try {
      const res = await apiFetch('/api/mailing/ab-tests', {
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
      apiFetch('/api/mailing/delivery-servers').then(r => r.json()).catch(() => ({ servers: [] })),
      apiFetch('/api/mailing/pmta-servers').then(r => r.json()).catch(() => ({ servers: [] })),
      apiFetch('/api/mailing/sending-profiles').then(r => r.json()).catch(() => ({ profiles: [] })),
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

// Content Library — reusable email templates organized by sending domain folders
const TemplatesManager: React.FC = () => {
  const [folderTree, setFolderTree] = useState<any[]>([]);
  const [templates, setTemplates] = useState<any[]>([]);
  const [selectedFolder, setSelectedFolder] = useState<string | null>(null);
  const [expandedFolders, setExpandedFolders] = useState<Record<string, boolean>>({});
  const [showCreate, setShowCreate] = useState(false);
  const [previewId, setPreviewId] = useState<string | null>(null);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editData, setEditData] = useState<any>(null);
  const [editSaving, setEditSaving] = useState(false);
  const [newTemplate, setNewTemplate] = useState({ name: '', description: '', subject: '', html_content: '', from_name: '', from_email: '', preview_text: '' });
  const [loading, setLoading] = useState(true);

  const inputStyle: React.CSSProperties = { background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 6, color: '#e0e6f0', padding: '8px 10px', fontSize: 13 };
  const btnGhost: React.CSSProperties = { background: 'none', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 6, padding: '4px 10px', fontSize: 11, cursor: 'pointer' };

  const fetchFolders = useCallback(() => {
    apiFetch('/api/mailing/template-folders/tree', { credentials: 'include' })
      .then(r => r.json())
      .then(data => {
        const tree = data.tree || data.folders || [];
        setFolderTree(tree);
        const expanded: Record<string, boolean> = {};
        tree.forEach((f: any) => { expanded[f.id] = true; });
        setExpandedFolders(prev => ({ ...expanded, ...prev }));
      })
      .catch(() => {});
  }, []);

  const fetchTemplates = useCallback(() => {
    const url = selectedFolder
      ? `/api/mailing/template-folders/${selectedFolder}/templates?recursive=true`
      : '/api/mailing/templates';
    apiFetch(url, { credentials: 'include' })
      .then(r => r.json())
      .then(data => { setTemplates(data.templates || []); setLoading(false); })
      .catch(() => setLoading(false));
  }, [selectedFolder]);

  useEffect(() => { fetchFolders(); }, [fetchFolders]);
  useEffect(() => { fetchTemplates(); }, [fetchTemplates]);

  const createTemplate = async () => {
    try {
      const payload: any = { ...newTemplate };
      if (selectedFolder) payload.folder_id = selectedFolder;
      const res = await apiFetch('/api/mailing/templates', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        credentials: 'include', body: JSON.stringify(payload),
      });
      if (res.ok) {
        fetchTemplates();
        setShowCreate(false);
        setNewTemplate({ name: '', description: '', subject: '', html_content: '', from_name: '', from_email: '', preview_text: '' });
      }
    } catch {}
  };

  const deleteTemplate = async (id: string) => {
    if (!confirm('Delete this template?')) return;
    try {
      await apiFetch(`/api/mailing/templates/${id}`, { method: 'DELETE', credentials: 'include' });
      fetchTemplates();
    } catch {}
  };

  const startEdit = async (t: any) => {
    try {
      const res = await apiFetch(`/api/mailing/templates/${t.id}`, { credentials: 'include' });
      if (res.ok) {
        const full = await res.json();
        const tpl = full.template || full;
        setEditData({
          name: tpl.name || '', subject: tpl.subject || '', from_name: tpl.from_name || '',
          preview_text: tpl.preview_text || '', html_content: tpl.html_content || '',
        });
        setEditingId(t.id);
      }
    } catch {
      setEditData({ name: t.name || '', subject: t.subject || '', from_name: t.from_name || '', preview_text: t.preview_text || '', html_content: t.html_content || '' });
      setEditingId(t.id);
    }
  };

  const saveEdit = async () => {
    if (!editingId || !editData) return;
    setEditSaving(true);
    try {
      const res = await apiFetch(`/api/mailing/templates/${editingId}`, {
        method: 'PUT', headers: { 'Content-Type': 'application/json' },
        credentials: 'include', body: JSON.stringify(editData),
      });
      if (res.ok) {
        setEditingId(null);
        setEditData(null);
        fetchTemplates();
      } else {
        alert('Failed to save template');
      }
    } catch { alert('Failed to save template'); }
    setEditSaving(false);
  };

  const toggleExpand = (id: string) => {
    setExpandedFolders(prev => ({ ...prev, [id]: !prev[id] }));
  };

  const folderItemStyle = (id: string | null): React.CSSProperties => ({
    padding: '7px 10px', borderRadius: 6, cursor: 'pointer', fontSize: 13, marginBottom: 2,
    background: selectedFolder === id ? 'rgba(0,229,255,0.08)' : 'transparent',
    color: selectedFolder === id ? '#00e5ff' : '#e0e6f0',
    border: selectedFolder === id ? '1px solid #00e5ff' : '1px solid transparent',
    display: 'flex', alignItems: 'center', gap: 6, userSelect: 'none',
  });

  const renderFolderTree = (folders: any[], depth: number = 0) => {
    return folders.map((f: any) => {
      const hasChildren = f.children && f.children.length > 0;
      const isExpanded = expandedFolders[f.id];
      return (
        <div key={f.id}>
          <div
            style={{ ...folderItemStyle(f.id), paddingLeft: 10 + depth * 16 }}
            onClick={() => setSelectedFolder(f.id)}
          >
            {hasChildren && (
              <span onClick={e => { e.stopPropagation(); toggleExpand(f.id); }}
                style={{ fontSize: 10, color: 'rgba(180,210,240,0.4)', cursor: 'pointer', width: 12, textAlign: 'center' }}>
                {isExpanded ? '▾' : '▸'}
              </span>
            )}
            {!hasChildren && <span style={{ width: 12 }} />}
            <span style={{ fontSize: 13 }}>{depth === 0 ? '📁' : '📄'}</span>
            <span style={{ flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{f.name}</span>
            {f.template_count > 0 && (
              <span style={{ fontSize: 10, color: 'rgba(180,210,240,0.35)', marginLeft: 'auto' }}>{f.template_count}</span>
            )}
          </div>
          {hasChildren && isExpanded && renderFolderTree(f.children, depth + 1)}
        </div>
      );
    });
  };

  if (loading) return <div className="loading-state">Loading templates...</div>;

  const findFolderName = (id: string | null, folders: any[]): string => {
    if (!id) return 'All Templates';
    for (const f of folders) {
      if (f.id === id) return f.path || f.name;
      if (f.children) {
        const found = findFolderName(id, f.children);
        if (found !== 'All Templates') return found;
      }
    }
    return 'All Templates';
  };

  return (
    <div className="manager-page" style={{ background: '#0a0f1a' }}>
      <div className="page-explanation">
        <h3 style={{ color: '#e0e6f0' }}>Content Library</h3>
        <p style={{ color: 'rgba(180,210,240,0.65)' }}>Reusable email templates organized by sending domain. Templates saved from the <strong>AI Generator</strong> in the Campaign Manager are automatically filed here.</p>
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: '240px 1fr', gap: 16 }}>
        {/* Folder tree sidebar */}
        <div style={{ background: '#0d1526', borderRadius: 10, padding: 12, border: '1px solid rgba(0,200,255,0.08)', maxHeight: 'calc(100vh - 200px)', overflowY: 'auto' }}>
          <h4 style={{ margin: '0 0 10px', fontSize: 13, color: 'rgba(180,210,240,0.65)' }}>Folders</h4>
          <div style={{ ...folderItemStyle(null) }} onClick={() => setSelectedFolder(null)}>
            <span style={{ width: 12 }} /><span style={{ fontSize: 13 }}>🗂️</span> All Templates
          </div>
          {renderFolderTree(folderTree)}
          {folderTree.length === 0 && (
            <p style={{ fontSize: 11, color: 'rgba(180,210,240,0.4)', margin: '8px 0 0' }}>No folders yet.</p>
          )}
        </div>

        {/* Templates */}
        <div>
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 }}>
            <span style={{ fontSize: 14, color: '#e0e6f0' }}>
              {selectedFolder ? `📁 ${findFolderName(selectedFolder, folderTree)}` : 'All Templates'} — {templates.length} template{templates.length !== 1 ? 's' : ''}
            </span>
            <button onClick={() => setShowCreate(true)} style={{ background: '#00e5ff', color: '#0a0f1a', border: 'none', borderRadius: 8, padding: '8px 14px', fontSize: 13, cursor: 'pointer', fontWeight: 600 }}>+ Create Template</button>
          </div>

          {/* Create form */}
          {showCreate && (
            <div style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 16, marginBottom: 12 }}>
              <h4 style={{ margin: '0 0 12px', color: '#00e5ff', fontSize: 14 }}>Create Email Template</h4>
              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 8, marginBottom: 8 }}>
                <input placeholder="Template Name *" value={newTemplate.name} onChange={e => setNewTemplate(p => ({...p, name: e.target.value}))} style={inputStyle} />
                <input placeholder="Description" value={newTemplate.description} onChange={e => setNewTemplate(p => ({...p, description: e.target.value}))} style={inputStyle} />
                <input placeholder="Default Subject" value={newTemplate.subject} onChange={e => setNewTemplate(p => ({...p, subject: e.target.value}))} style={inputStyle} />
                <input placeholder="From Name" value={newTemplate.from_name} onChange={e => setNewTemplate(p => ({...p, from_name: e.target.value}))} style={inputStyle} />
                <input placeholder="Pre-header Text" value={newTemplate.preview_text} onChange={e => setNewTemplate(p => ({...p, preview_text: e.target.value}))} style={{ ...inputStyle, gridColumn: '1 / -1' }} />
              </div>
              <textarea placeholder="HTML Content" value={newTemplate.html_content} onChange={e => setNewTemplate(p => ({...p, html_content: e.target.value}))} rows={6} style={{ width: '100%', ...inputStyle, fontSize: 12, fontFamily: 'monospace', resize: 'vertical', boxSizing: 'border-box', marginBottom: 8 }} />
              <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
                <button onClick={() => setShowCreate(false)} style={{ ...btnGhost, color: 'rgba(180,210,240,0.65)' }}>Cancel</button>
                <button onClick={createTemplate} disabled={!newTemplate.name} style={{ background: '#00e5ff', color: '#0a0f1a', border: 'none', borderRadius: 6, padding: '6px 14px', fontSize: 13, cursor: 'pointer', fontWeight: 600, opacity: newTemplate.name ? 1 : 0.5 }}>Create</button>
              </div>
            </div>
          )}

          {/* Template list */}
          <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
            {templates.map((t: any) => {
              const isEditing = editingId === t.id;
              return (
                <div key={t.id} style={{ background: '#0d1526', border: isEditing ? '1px solid #00e5ff' : '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 14 }}>
                  {isEditing && editData ? (
                    <>
                      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 10 }}>
                        <h4 style={{ margin: 0, fontSize: 14, color: '#00e5ff' }}>Edit Template</h4>
                        <div style={{ display: 'flex', gap: 6 }}>
                          <button onClick={() => { setEditingId(null); setEditData(null); }} style={{ ...btnGhost, color: 'rgba(180,210,240,0.65)' }}>Cancel</button>
                          <button onClick={saveEdit} disabled={editSaving || !editData.name} style={{ background: '#00e5ff', color: '#0a0f1a', border: 'none', borderRadius: 6, padding: '4px 14px', fontSize: 12, cursor: 'pointer', fontWeight: 600, opacity: editData.name ? 1 : 0.5 }}>
                            {editSaving ? 'Saving...' : 'Save'}
                          </button>
                        </div>
                      </div>
                      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 8, marginBottom: 8 }}>
                        <div>
                          <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)', display: 'block', marginBottom: 3 }}>Template Name</label>
                          <input value={editData.name} onChange={e => setEditData((p: any) => ({...p, name: e.target.value}))} style={inputStyle} />
                        </div>
                        <div>
                          <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)', display: 'block', marginBottom: 3 }}>Subject Line</label>
                          <input value={editData.subject} onChange={e => setEditData((p: any) => ({...p, subject: e.target.value}))} style={inputStyle} />
                        </div>
                        <div>
                          <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)', display: 'block', marginBottom: 3 }}>From Name</label>
                          <input value={editData.from_name} onChange={e => setEditData((p: any) => ({...p, from_name: e.target.value}))} style={inputStyle} />
                        </div>
                        <div>
                          <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)', display: 'block', marginBottom: 3 }}>Pre-header</label>
                          <input value={editData.preview_text} onChange={e => setEditData((p: any) => ({...p, preview_text: e.target.value}))} style={inputStyle} />
                        </div>
                      </div>
                      <div>
                        <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)', display: 'block', marginBottom: 3 }}>HTML Content</label>
                        <textarea value={editData.html_content} onChange={e => setEditData((p: any) => ({...p, html_content: e.target.value}))} rows={12}
                          style={{ width: '100%', ...inputStyle, fontSize: 12, fontFamily: 'monospace', resize: 'vertical', boxSizing: 'border-box' }} />
                      </div>
                    </>
                  ) : (
                    <>
                      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 6 }}>
                        <div style={{ flex: 1, minWidth: 0 }}>
                          <strong style={{ color: '#e0e6f0', fontSize: 14 }}>{t.name}</strong>
                          {t.description && <span style={{ marginLeft: 8, fontSize: 11, color: 'rgba(180,210,240,0.5)' }}>{t.description}</span>}
                        </div>
                        <div style={{ display: 'flex', gap: 6, alignItems: 'center', flexShrink: 0 }}>
                          <span style={{ background: t.status === 'active' ? '#00b89420' : 'rgba(0,229,255,0.12)', color: t.status === 'active' ? '#00b894' : '#00e5ff', fontSize: 11, padding: '2px 8px', borderRadius: 4 }}>{t.status}</span>
                          <button onClick={() => setPreviewId(previewId === t.id ? null : t.id)} style={{ ...btnGhost, color: '#00b0ff' }}>Preview</button>
                          <button onClick={() => startEdit(t)} style={{ ...btnGhost, color: '#f59e0b' }}>Edit</button>
                          <button onClick={() => deleteTemplate(t.id)} style={{ ...btnGhost, color: '#e94560' }}>Delete</button>
                        </div>
                      </div>
                      <div style={{ display: 'flex', gap: 16, flexWrap: 'wrap' }}>
                        {t.subject && <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.65)' }}>Subject: <span style={{ color: '#00b0ff' }}>{t.subject}</span></div>}
                        {t.from_name && <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.65)' }}>From: <span style={{ color: '#00b0ff' }}>{t.from_name}</span></div>}
                        {t.preview_text && <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.65)' }}>Pre-header: <span style={{ color: '#8b5cf6' }}>{t.preview_text}</span></div>}
                      </div>
                      {previewId === t.id && t.html_content && (
                        <div style={{ marginTop: 10, background: '#0a0f1a', borderRadius: 8, overflow: 'hidden', border: '1px solid rgba(0,200,255,0.08)' }}>
                          <iframe srcDoc={t.html_content} title={`Preview ${t.name}`} style={{ width: '100%', height: 400, border: 'none' }} sandbox="allow-same-origin" />
                        </div>
                      )}
                    </>
                  )}
                </div>
              );
            })}
            {templates.length === 0 && <p style={{ textAlign: 'center', color: 'rgba(180,210,240,0.65)', fontSize: 13, padding: 40 }}>No templates in this folder.</p>}
          </div>
        </div>
      </div>
    </div>
  );
};

// ─── Section Sub-Navigation Style ──────────────────────────────────────────
const subNavStyle: React.CSSProperties = {
  display: 'flex', gap: 4, marginBottom: 20, padding: '4px',
  background: 'rgba(0,200,255,0.04)', borderRadius: 10, border: '1px solid rgba(0,200,255,0.08)',
  width: 'fit-content',
};
const subNavBtnStyle = (active: boolean): React.CSSProperties => ({
  padding: '8px 18px', borderRadius: 8, border: 'none', cursor: 'pointer',
  fontSize: 13, fontWeight: active ? 700 : 500, transition: 'all 0.15s',
  background: active ? '#00b0ff' : 'transparent',
  color: active ? '#0a0f1a' : 'rgba(180,210,240,0.75)',
});

// ─── Suppressions Section ──────────────────────────────────────────────────
const SuppressionsSection: React.FC<{ activeSubTab: TabId; onSubTabChange: (t: TabId) => void }> = ({ activeSubTab, onSubTabChange }) => {
  const subTab = activeSubTab === 'global-suppression' ? 'global-suppression' : 'suppressions';
  return (
    <div>
      <div style={subNavStyle}>
        <button style={subNavBtnStyle(subTab === 'suppressions')} onClick={() => onSubTabChange('suppressions')}>Dashboard</button>
        <button style={subNavBtnStyle(subTab === 'global-suppression')} onClick={() => onSubTabChange('global-suppression')}>Global Suppression Hub</button>
      </div>
      <Suspense fallback={<ChunkLoader />}>
        {subTab === 'global-suppression' ? <GlobalSuppressionDashboard /> : <SuppressionPortal />}
      </Suspense>
    </div>
  );
};

// ─── Campaign Center Section ───────────────────────────────────────────────
interface PreparingCampaign {
  id: string;
  name: string;
  acceptedAt: number;
}

const PreparationBanner: React.FC<{
  campaigns: PreparingCampaign[];
  transitions: { id: string; name: string; status: string }[];
  onDismissTransition: (id: string) => void;
}> = ({ campaigns, transitions, onDismissTransition }) => {
  const [, setTick] = useState(0);
  useEffect(() => {
    if (campaigns.length === 0) return;
    const iv = setInterval(() => setTick(t => t + 1), 1000);
    return () => clearInterval(iv);
  }, [campaigns.length]);

  if (campaigns.length === 0 && transitions.length === 0) return null;

  const elapsed = (ts: number) => {
    const s = Math.floor((Date.now() - ts) / 1000);
    if (s < 60) return `${s}s`;
    return `${Math.floor(s / 60)}m ${s % 60}s`;
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 6, padding: '0 0 8px 0' }}>
      {campaigns.map(c => (
        <div key={c.id} style={{
          display: 'flex', alignItems: 'center', justifyContent: 'space-between',
          padding: '10px 16px', borderRadius: 8,
          background: 'rgba(245, 158, 11, 0.08)', borderLeft: '3px solid #f59e0b',
          fontSize: 13, color: '#fbbf24',
        }}>
          <span style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            <FontAwesomeIcon icon={faSpinner} spin style={{ fontSize: 12 }} />
            <strong>Finalizing Audience:</strong> {c.name}
          </span>
          <span style={{ color: 'rgba(251,191,36,0.6)', fontSize: 12 }}>{elapsed(c.acceptedAt)}</span>
        </div>
      ))}
      {transitions.map(t => (
        <div key={t.id} style={{
          display: 'flex', alignItems: 'center', justifyContent: 'space-between',
          padding: '10px 16px', borderRadius: 8,
          background: t.status === 'failed' ? 'rgba(239,68,68,0.08)' : 'rgba(16,185,129,0.08)',
          borderLeft: `3px solid ${t.status === 'failed' ? '#ef4444' : '#10b981'}`,
          fontSize: 13, color: t.status === 'failed' ? '#f87171' : '#6ee7b7',
        }}>
          <span>
            {t.status === 'failed' ? `Campaign "${t.name}" failed` : `Campaign "${t.name}" is ready`}
          </span>
          <button onClick={() => onDismissTransition(t.id)} style={{
            background: 'none', border: 'none', color: 'inherit', cursor: 'pointer',
            opacity: 0.6, fontSize: 12, padding: '2px 6px',
          }}>dismiss</button>
        </div>
      ))}
    </div>
  );
};

const CampaignCenterSection: React.FC<{
  activeSubTab: TabId;
  onSubTabChange: (t: TabId) => void;
  pendingOffer: { offerId: string; offerName: string } | null;
  onOfferConsumed: () => void;
  copilotOpen: boolean;
  setCopilotOpen: (v: boolean) => void;
}> = ({ activeSubTab, onSubTabChange, pendingOffer, onOfferConsumed, copilotOpen, setCopilotOpen }) => {
  const { organization } = useAuth();
  const subTab = (['pmta-wizard', 'send-day', 'marketing-agent'].includes(activeSubTab)) ? activeSubTab : 'campaign-center';
  const [editCampaignId, setEditCampaignId] = useState<string | null>(null);
  const [preparingCampaigns, setPreparingCampaigns] = useState<PreparingCampaign[]>([]);
  const [transitions, setTransitions] = useState<{ id: string; name: string; status: string }[]>([]);

  const handleEditInWizard = useCallback((id: string) => {
    setEditCampaignId(id);
    onSubTabChange('pmta-wizard');
  }, [onSubTabChange]);

  const handleEditComplete = useCallback(() => {
    setEditCampaignId(null);
  }, []);

  const handleCampaignPreparing = useCallback((id: string, name: string) => {
    setPreparingCampaigns(prev => [...prev.filter(c => c.id !== id), { id, name, acceptedAt: Date.now() }]);
  }, []);

  // Poll preparing campaigns for status transitions
  useEffect(() => {
    if (preparingCampaigns.length === 0) return;
    const orgId = organization?.id;
    if (!orgId) return;
    const iv = setInterval(async () => {
      for (const c of preparingCampaigns) {
        try {
          const res = await apiFetch(`/api/mailing/campaigns/${c.id}`, {
            headers: { 'X-Organization-ID': orgId },
          });
          if (!res.ok) continue;
          const data = await res.json();
          if (data.status && data.status !== 'preparing' && data.status !== 'finalizing_audience') {
            setPreparingCampaigns(prev => prev.filter(p => p.id !== c.id));
            setTransitions(prev => [...prev, { id: c.id, name: c.name, status: data.status }]);
            if (data.status !== 'failed') {
              setTimeout(() => setTransitions(prev => prev.filter(t => t.id !== c.id)), 5000);
            }
          }
        } catch { /* ignore polling errors */ }
      }
    }, 5000);
    return () => clearInterval(iv);
  }, [preparingCampaigns, organization?.id]);

  const handleDismissTransition = useCallback((id: string) => {
    setTransitions(prev => prev.filter(t => t.id !== id));
  }, []);

  return (
    <div>
      <div style={subNavStyle}>
        <button style={subNavBtnStyle(subTab === 'campaign-center')} onClick={() => onSubTabChange('campaign-center')}>Dashboard</button>
        <button style={subNavBtnStyle(subTab === 'pmta-wizard')} onClick={() => onSubTabChange('pmta-wizard')}>Campaign Manager</button>
        <button style={subNavBtnStyle(subTab === 'send-day')} onClick={() => onSubTabChange('send-day')}>Send Day</button>
        <button style={subNavBtnStyle(subTab === 'marketing-agent')} onClick={() => onSubTabChange('marketing-agent')}>Marketing Agent</button>
      </div>
      <PreparationBanner campaigns={preparingCampaigns} transitions={transitions} onDismissTransition={handleDismissTransition} />
      <Suspense fallback={<ChunkLoader />}>
        {subTab === 'pmta-wizard' ? (
          <>
            <PMTACampaignWizard
              onClose={() => { handleEditComplete(); onSubTabChange('campaign-center'); }}
              editCampaignId={editCampaignId}
              onEditComplete={handleEditComplete}
              onCampaignPreparing={handleCampaignPreparing}
            />
            <button
              onClick={() => setCopilotOpen(true)}
              title="Campaign Copilot"
              style={{
                position: 'fixed', bottom: 24, right: 24, zIndex: 9990,
                width: 52, height: 52, borderRadius: 14,
                background: 'linear-gradient(135deg, #6366f1, #8b5cf6)',
                border: 'none', color: '#fff', cursor: 'pointer',
                boxShadow: '0 4px 20px rgba(99,102,241,0.4)',
                display: 'flex', alignItems: 'center', justifyContent: 'center',
                fontSize: 20, fontWeight: 700, transition: 'transform 0.15s',
              }}
              onMouseEnter={e => (e.currentTarget.style.transform = 'scale(1.08)')}
              onMouseLeave={e => (e.currentTarget.style.transform = 'scale(1)')}
            >AI</button>
            <Suspense fallback={null}>
              <CampaignCopilotPanel isOpen={copilotOpen} onClose={() => setCopilotOpen(false)} />
            </Suspense>
          </>
        ) : subTab === 'marketing-agent' ? (
          <EmailMarketingAgentPanel />
        ) : subTab === 'send-day' ? (
          <SendDayPlanner onEditInWizard={handleEditInWizard} onCampaignPreparing={handleCampaignPreparing} />
        ) : (
          <CampaignPortal initialOffer={pendingOffer} onOfferConsumed={onOfferConsumed} onEditInWizard={handleEditInWizard} />
        )}
      </Suspense>
    </div>
  );
};

// ─── AI Agents Section ─────────────────────────────────────────────────────
const AIAgentsSection: React.FC<{ activeSubTab: TabId; onSubTabChange: (t: TabId) => void }> = ({ activeSubTab, onSubTabChange }) => {
  const subTab = (['profiles', 'jarvis'].includes(activeSubTab)) ? activeSubTab : 'sending-plans';
  return (
    <div>
      <div style={subNavStyle}>
        <button style={subNavBtnStyle(subTab === 'sending-plans')} onClick={() => onSubTabChange('sending-plans')}>AI Plans</button>
        <button style={subNavBtnStyle(subTab === 'profiles')} onClick={() => onSubTabChange('profiles')}>Inbox Intel</button>
        <button style={subNavBtnStyle(subTab === 'jarvis')} onClick={() => onSubTabChange('jarvis')}>Jarvis</button>
      </div>
      <Suspense fallback={<ChunkLoader />}>
        {subTab === 'profiles' ? <InboxProfiles /> : subTab === 'jarvis' ? <JarvisDashboard /> : <ISPAgentIntelligence />}
      </Suspense>
    </div>
  );
};

// _AnalyticsDashboard and SuggestionsWidget legacy stubs were removed in
// PAGE_VERSION_DASHBOARD = 1.0 (2026-05-08). AnalyticsCenter was retired
// 2026-06-09; the active analytics surface is EventLakeExplorer (tab id
// 'event-lake', labeled "Analytics", mounted in renderContent above).

export default MailingPortal;

