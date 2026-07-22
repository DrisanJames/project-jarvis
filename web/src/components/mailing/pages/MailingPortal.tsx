import React, { useState, useEffect, useCallback, Suspense, lazy } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faChartLine, faEnvelope, faBullhorn, faPaperPlane, faCalculator,
  faListUl, faCrosshairs, faBolt,
  faBan, faBrain, faRobot, faChartPie, faServer, faDatabase,
  /* faArrowLeft, */ faGlobe, faStore,
  faSpinner, faSeedling, faWandMagicSparkles,
  faTruckFast, faFire, faTriangleExclamation, faRoute, faLink,
} from '@fortawesome/free-solid-svg-icons';
import { IconDefinition } from '@fortawesome/fontawesome-svg-core';
import { useAuth } from '../../../contexts/AuthContext';
import './MailingPortal.css';
import '../shared/animations.css';
import { ToastProvider } from '../shared/ToastSystem';
import { apiFetch } from '../shared/apiFetch';
import { colors, stateColor, thStyle, tdStyle, numTd, numTh, tableStyle } from '../shared/theme';
import { Panel, Stat, SectionHeader, EmptyState, ProgressBar, Pill, SectionError } from '../shared/ui';
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
const DraftBoardView = lazy(() => import('../components/DraftBoardView').then(m => ({ default: m.DraftBoardView })));
const ScheduleView = lazy(() => import('../components/ScheduleView').then(m => ({ default: m.ScheduleView })));
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
const SmartLinkManager = lazy(() => import('../components/SmartLinkManager').then(m => ({ default: m.SmartLinkManager })));

// ── Suspense fallback ───────────────────────────────────────────────────────
const ChunkLoader: React.FC = () => (
  <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', height: '50vh', color: 'rgba(180,210,240,0.65)', gap: 10, fontSize: 14 }}>
    <FontAwesomeIcon icon={faSpinner} spin /> Loading…
  </div>
);

type TabId = 'dashboard' | 'lists' | 'campaign-center' | 'suppressions' | 'global-suppression' | 'profiles' | 'send' | 'sending-plans' | 'domain-center' | 'domain-agents' | 'delivery-servers' | 'offers' | 'analytics' | 'segments' | 'automations' | 'ab-tests' | 'import' | 'mission-control' | 'jarvis' | 'pmta-wizard' | 'send-day' | 'draft-board' | 'schedule' | 'consciousness' | 'content-library' | 'marketing-agent' | 'ai-agents' | 'outbox' | 'audience-health' | 'audience-cadence' | 'event-lake' | 'data-partners' | 'creative-studio' | 'cpm-planner' | 'smart-links';

interface Tab {
  id: TabId;
  label: string;
  icon: IconDefinition;
  description: string;
  childIds?: TabId[];
}

const tabs: Tab[] = [
  { id: 'dashboard', label: 'Dashboard', icon: faChartLine, description: 'A real-time overview of your email performance — sends, opens, clicks and deliverability at a glance.' },
  { id: 'campaign-center', label: 'Campaign Center', icon: faBullhorn, description: 'Create, schedule and monitor your email campaigns.', childIds: ['campaign-center', 'pmta-wizard', 'send-day', 'draft-board', 'schedule'] },
  { id: 'lists', label: 'Segments', icon: faListUl, description: 'Build and manage your audience segments, lists and subscribers.' },
  { id: 'suppressions', label: 'Suppressions', icon: faBan, description: 'Manage who you do not email — opt-outs, complaints and do-not-contact lists.', childIds: ['suppressions', 'global-suppression'] },
  { id: 'ai-agents', label: 'AI Agents', icon: faBrain, description: 'AI-powered deliverability tools — inbox intelligence and per-recipient engagement scoring.', childIds: ['profiles'] },
  { id: 'domain-center', label: 'Domain Center', icon: faGlobe, description: 'Manage your sending, tracking and image domains and their authentication.' },
  { id: 'domain-agents', label: 'Domain Agents', icon: faRobot, description: 'AI send-planning and approval for each sending domain — baselines, recommendations and scorecards.' },
  { id: 'cpm-planner', label: 'CPM Planner', icon: faCalculator, description: 'Price and plan CPM deals — projected volume, pace, capacity risk and live earnings vs goal.' },
  { id: 'offers', label: 'Offers', icon: faStore, description: 'Manage your offers end to end — creatives, compliance, scheduling and conversion tracking.' },
  { id: 'creative-studio', label: 'Creative Studio', icon: faWandMagicSparkles, description: 'Browse, preview and manage newsletter creatives for each offer and sending brand.' },
  { id: 'smart-links', label: 'Smart Links', icon: faLink, description: 'Manage smart link gateway routes — brand-domain bot/human routing for offer links.' },
  { id: 'event-lake', label: 'Reporting', icon: faChartPie, description: 'Email performance reporting — deliverability, engagement and results by mailbox provider, brand and campaign. Filter by date and provider to see how each send performed.' },
  { id: 'audience-health', label: 'Audience', icon: faSeedling, description: 'Understand your audience — growth, churn, performance by acquisition source, subscriber lookup and welcome-list capacity.' },
  { id: 'audience-cadence', label: 'Send Frequency', icon: faChartLine, description: 'Recommended send frequency for each mailbox provider to maximize engagement without fatiguing your audience.' },
  { id: 'delivery-servers', label: 'Sending Infrastructure', icon: faServer, description: 'Your sending servers and dedicated IP addresses.' },
  { id: 'consciousness', label: 'Campaign Intelligence', icon: faCrosshairs, description: 'The AI insights and strategy behind your campaigns.' },
  { id: 'data-partners', label: 'Data Partners', icon: faDatabase, description: 'Manage inbound data-partner connections — access keys, submitted lists, automated follow-ups and creatives.' },
  { id: 'outbox', label: 'Delivery Queue', icon: faPaperPlane, description: 'Track emails in progress, queued and any that failed to send.' },
];

interface VersionInfo {
  version: string;
  git_sha: string;
  build_time: string;
  go_version: string;
  deployed_at: string;
}

// The portal has no router, so a browser refresh would otherwise always land
// on the dashboard — persist the active tab and restore it on mount. Stored
// ids are validated against the live tab set (top-level ids + childIds) so a
// renamed/removed tab falls back to the dashboard instead of a blank view.
const ACTIVE_TAB_KEY = 'jarvis.portal.activeTab';
const validTabIds = new Set<string>(tabs.flatMap(t => [t.id, ...(t.childIds || [])]));
const restoreActiveTab = (): TabId => {
  try {
    const saved = localStorage.getItem(ACTIVE_TAB_KEY);
    if (saved && validTabIds.has(saved)) return saved as TabId;
  } catch { /* storage unavailable (private mode) — default */ }
  return 'dashboard';
};

export const MailingPortal: React.FC = () => {
  const { organization } = useAuth();
  const [activeTab, setActiveTab] = useState<TabId>(restoreActiveTab);
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

  // Persist every tab change (both handleTabChange and the jarvis:navigate
  // event path land here) so a refresh restores the operator's screen.
  useEffect(() => {
    try { localStorage.setItem(ACTIVE_TAB_KEY, activeTab); } catch { /* non-fatal */ }
  }, [activeTab]);

  // Cross-tab deep links — components rendered without portal props (e.g.
  // Offer Center → CPM Planner) navigate by dispatching a 'jarvis:navigate'
  // CustomEvent with { tab }. Hash changes are not observed anywhere in the
  // portal, so this event is the lightest working mechanism.
  useEffect(() => {
    const onNavigate = (e: Event) => {
      const tab = (e as CustomEvent<{ tab?: string }>).detail?.tab;
      if (tab) {
        if (tab !== 'campaign-center') setPendingOffer(null);
        setActiveTab(tab as TabId);
      }
    };
    window.addEventListener('jarvis:navigate', onNavigate);
    return () => window.removeEventListener('jarvis:navigate', onNavigate);
  }, []);

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
      case 'draft-board':
      case 'schedule':
      case 'marketing-agent':
        return <CampaignCenterSection activeSubTab={activeTab} onSubTabChange={setActiveTab} pendingOffer={pendingOffer} onOfferConsumed={() => setPendingOffer(null)} copilotOpen={copilotOpen} setCopilotOpen={setCopilotOpen} />;
      case 'sending-plans':
      case 'profiles':
      case 'jarvis':
      case 'ai-agents':
        return <AIAgentsSection activeSubTab={activeTab === 'ai-agents' ? 'profiles' : activeTab} onSubTabChange={setActiveTab} />;
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
      case 'smart-links':
        return <Suspense fallback={<ChunkLoader />}><SmartLinkManager /></Suspense>;
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
          <h1>Jarvis</h1>
          <span className="subtitle">Email Delivery Suite</span>
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
//   1.1 (2026-06-12) — Today's Performance now prefers the analytics lake
//     (Athena) for delivered/opens/clicks/hard/soft, with a "lake" source
//     badge; falls back to the PG numbers (badge explains SES routes are
//     pixel-blind in PG) when the lake reader is disabled or errors.
//     Revenue stays from the PG dashboard payload.
//   1.2 (2026-07-01) — Today's Performance converges with the Reporting tab
//     per docs/METRIC_CONTRACT.md: lake read switched from /lake/summary
//     (unfiltered, ~48h UTC window, app-stream double count) to the canonical
//     /lake/breakdown with local_dt=<Denver day> + source_in=pmta,ses;
//     opens/clicks tiles now show RAW counts from /analytics/engagement with a
//     "machine incl. · human N" subtext (fail-soft to lake raw counts); bounce
//     denominator = delivered + hard + soft + untyped 'bounced'.
//   1.3 (2026-07-02) — Central operations console rebuild (PORTAL_DESIGN_SYSTEM
//     §1 hierarchy). Bug fixes: 6-KPI grid is now repeat(3,1fr) (was repeat(4)
//     which orphaned 2 cards); Audience Growth sumRows reads r.count (was r.c —
//     the backend serializes AudienceRow as json:"count", so it always read 0).
//     New modules, each fail-soft (one failed fetch never blanks the console):
//     In-Transit (top-5 most-recently-started sending campaigns, polled 45s),
//     Deferring ISPs + reason codes (polled 45s), Trending Offers (24h
//     conversions), Click Funnels (live click-drip lanes — replaces the dead
//     /journey-center/overview source), and a shared-table Recent Campaigns.
//   1.4 (2026-07-02) — dashboard audit fixes (METRIC_CONTRACT / PORTAL_DESIGN_SYSTEM):
//     * In-Transit hero now passes exclude_drip=true so it shows the operator's
//       real broadcast waves (was 100% partner-drip micro-campaigns) and labels
//       the true "N sending" total from the paginated envelope (BUG-4/5/6).
//     * Open/Click rate capped at 100% + denominator/approximation tooltip
//       (was 84.5% uncapped, same-day-opens ÷ same-day-delivered cohort skew) (BUG-1).
//     * Soft-bounce tile marked `*` with a "transient, later retried/delivered"
//       tooltip — number unchanged, disclosed per DESIGN_SYSTEM §1.5 (BUG-2).
//     * Audience Growth reads the NEW GET /dashboard/audience-growth (PG
//       mailing_subscribers, 7 Denver days) instead of the stale Athena audience
//       table that reported a false +0 net (BUG-10/11).
//     * Deferring-ISPs panel relabeled "Deferrals · transient retries" with
//       neutral accent + raised danger thresholds so transient PMTA retries no
//       longer read as an outage; top reason skips the generic '4.0.0' noise
//       bucket (BUG-7/8, latter server-side).
//     * System-health status pills derived from live data (was hardcoded
//       Active/Learning/Protected/Running) (BUG-12).
//     * SectionError gains a Retry button; the one-shot Growth/Trending/Funnels
//       fetches are re-callable so a failed panel isn't stuck until reload (BUG-15).
const PAGE_VERSION_DASHBOARD = '1.4';

// Today's lake counts (reclassified event_type buckets from the canonical
// /analytics/lake/breakdown query — Denver day, source_in=pmta,ses; see
// docs/METRIC_CONTRACT.md §1/§4. The 'app' mirror stream double-counts
// delivered and is excluded).
interface LakeTodayCounts {
  delivered: number;
  opens: number; // raw lake open events — fallback when the engagement fetch fails
  clicks: number; // raw lake click events — fallback when the engagement fetch fails
  hard: number;
  soft: number;
  bouncedUntyped: number; // ses-source 'bounced' (no hard/soft split)
}

// Raw + human opens/clicks from GET /api/mailing/analytics/engagement
// (PG mailing_tracking_events + ignite_event_verdict — METRIC_CONTRACT §6).
// Same endpoint the Reporting tab's KPI strip uses.
interface EngTodayCounts {
  raw_opens: number;
  human_opens: number;
  human_openers: number;
  raw_clicks: number;
  human_clicks: number;
  human_clickers: number;
}

// Today's date (YYYY-MM-DD) in America/Denver — mirrors the dashboard
// handler's "today" window (operator-local calendar day).
const denverToday = (): string =>
  new Intl.DateTimeFormat('en-CA', { timeZone: 'America/Denver' }).format(new Date());

// --- Audience growth (acquisition vs churn) ------------------------------
// Now sourced from GET /dashboard/audience-growth (PG mailing_subscribers over
// the last 7 Denver days). The prior lake-window helpers (nextUTCDay /
// denverDaysAgo) and the sumRows reducer were removed with the stale Athena
// audience-table fetch (BUG-10).
interface AudienceGrowth {
  acquired7d: number;
  churned7d: number;
  // Activated pool: subscribers mailed ≤4 times who opened or clicked. A
  // state/pool count (all-time), not a 7-day flow like acquired/churned.
  activated: number;
}

// --- In-transit campaigns (GET /campaigns?status=sending) ----------------
// Fields per CampaignBuilder.HandleListCampaigns (campaign_builder_crud.go);
// the campaigns array arrives under the paginated envelope's `data` key.
interface InTransitCampaign {
  id: string;
  name: string;
  status: string;
  total_recipients: number;
  sent_count: number;
  from_name?: string;
  profile_name?: string;
  started_at?: string;
  scheduled_at?: string;
}

// --- Deferring ISPs (GET /dashboard/deferring-isps) ----------------------
interface DeferReason { code: string; count: number; sample: string }
interface DeferringISP { isp: string; deferred: number; reasons: DeferReason[] }

// --- Trending offers (GET /dashboard/trending-offers) --------------------
interface TrendingOffer { offer_id: string; name: string; conversions: number }

// --- Click funnels (GET /dashboard/click-funnels) — LIVE click-drip lanes.
// Replaces the dead /journey-center/overview source (METRIC per handler doc).
interface ClickFunnelLane {
  offer_id: string;
  name: string;
  enabled: boolean;
  payout_type: string;
  active_enrolled: number;
  completed_or_converted: number; // LIFETIME (all-time), not windowed
}
interface ClickFunnelsOverview {
  lanes: ClickFunnelLane[];
  total_active_lanes: number;
  total_enrolled: number;
}

// Short "HH:MM" Denver clock for a timestamp (in-transit "started" column).
const denverClock = (iso?: string): string => {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return '—';
  return new Intl.DateTimeFormat('en-US', {
    timeZone: 'America/Denver', hour: '2-digit', minute: '2-digit', hour12: false,
  }).format(d);
};

const EnhancedDashboard: React.FC = () => {
  const { organization } = useAuth();
  const [dashboard, setDashboard] = useState<any>(null);
  const [loading, setLoading] = useState(true);
  // null = lake disabled/unavailable/errored → render the PG fallback.
  const [lakeToday, setLakeToday] = useState<LakeTodayCounts | null>(null);
  // Raw+human engagement for today (PG + verdict). Fail-soft: null → the
  // open/click tiles fall back to the lake's raw counts (never blank).
  const [engToday, setEngToday] = useState<EngTodayCounts | null>(null);
  // Console modules — each is optional/failure-tolerant. A slow or unavailable
  // source shows a per-panel error/empty state and never blanks the console
  // (fail-soft, PORTAL_DESIGN_SYSTEM §1.6). `null` = still loading / no data;
  // the paired *Err string flips a panel into its SectionError shell.
  const [growth, setGrowth] = useState<AudienceGrowth | null>(null);
  const [growthErr, setGrowthErr] = useState<string | null>(null);
  const [inTransit, setInTransit] = useState<InTransitCampaign[] | null>(null);
  // Total count of non-drip campaigns in the `sending` state (from the paginated
  // envelope), so the panel can say "N sending" rather than just "top 5".
  const [inTransitTotal, setInTransitTotal] = useState<number>(0);
  const [inTransitErr, setInTransitErr] = useState<string | null>(null);
  const [deferring, setDeferring] = useState<DeferringISP[] | null>(null);
  const [deferringErr, setDeferringErr] = useState<string | null>(null);
  const [trending, setTrending] = useState<{ rows: TrendingOffer[]; total: number } | null>(null);
  const [trendingErr, setTrendingErr] = useState<string | null>(null);
  const [funnels, setFunnels] = useState<ClickFunnelsOverview | null>(null);
  const [funnelsErr, setFunnelsErr] = useState<string | null>(null);

  // Acquisition vs churn over the last 7 America/Denver days. BUG-10/11 fix:
  // reads the NEW GET /dashboard/audience-growth (PG mailing_subscribers, org-
  // scoped, exactly 7 Denver days) instead of the stale Athena audience table
  // (last refresh 2026-06-09) that reported a false "+0 net". Membership truth
  // (who was acquired/churned) lives in PG, not the delivery lake. Re-callable
  // (BUG-15) so a failed one-shot fetch can be retried without a page reload.
  const loadGrowth = useCallback(async (signal?: AbortSignal) => {
    setGrowthErr(null);
    try {
      const res = await apiFetch('/api/mailing/dashboard/audience-growth', { credentials: 'include', signal });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      if (signal?.aborted) return;
      if (data && typeof data.acquired_7d === 'number') {
        setGrowth({ acquired7d: data.acquired_7d || 0, churned7d: data.churned_7d || 0, activated: data.activated || 0 });
      } else {
        throw new Error('unexpected payload');
      }
    } catch (e: any) {
      if (signal?.aborted) return;
      // Leave growth=null so the panel shows its unavailable state (never a
      // misleading green +0). Record the error to offer a Retry affordance.
      setGrowth(null);
      setGrowthErr(e?.message || 'unreachable');
    }
  }, []);
  useEffect(() => {
    const ctrl = new AbortController();
    loadGrowth(ctrl.signal);
    return () => ctrl.abort();
  }, [loadGrowth]);

  // Click funnels — LIVE click-drip lanes (replaces the dead journey-center
  // overview). One-shot (lane membership changes slowly) but re-callable so a
  // failed fetch can be retried without a page reload (BUG-15).
  const loadFunnels = useCallback(async (signal?: AbortSignal) => {
    try {
      const res = await apiFetch('/api/mailing/dashboard/click-funnels', { credentials: 'include', signal });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      if (signal?.aborted) return;
      setFunnels({
        lanes: Array.isArray(data?.lanes) ? data.lanes : [],
        total_active_lanes: Number(data?.total_active_lanes) || 0,
        total_enrolled: Number(data?.total_enrolled) || 0,
      });
      setFunnelsErr(null);
    } catch (e: any) {
      if (signal?.aborted) return;
      setFunnelsErr(e?.message || 'unreachable');
    }
  }, []);
  useEffect(() => {
    const ctrl = new AbortController();
    loadFunnels(ctrl.signal);
    return () => ctrl.abort();
  }, [loadFunnels]);

  // In-transit campaigns — the "what's mailing right now" hero. BUG-4 fix:
  // exclude_drip=true hides the continuous partner-drip micro-campaigns (which,
  // being the most-recently-CREATED rows, were 100% of the unfiltered window and
  // hid the operator's real broadcast waves). Polled every 45s so the console
  // feels live. Sort client-side by started_at desc (fall back to scheduled_at)
  // and keep the 5 most-recently-started. BUG-5: the paginated envelope's
  // pagination.total is the count of ALL non-drip sending campaigns (same
  // filters), surfaced as "N sending" in the header. Re-callable for BUG-15.
  const loadInTransit = useCallback(async (signal?: AbortSignal) => {
    try {
      const res = await apiFetch('/api/mailing/campaigns?status=sending&exclude_drip=true&limit=50', { credentials: 'include', signal });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      if (signal?.aborted) return;
      const rows: InTransitCampaign[] = Array.isArray(data?.data) ? data.data : (Array.isArray(data?.campaigns) ? data.campaigns : []);
      const total = Number(data?.pagination?.total);
      const startedMs = (c: InTransitCampaign) => {
        const t = c.started_at || c.scheduled_at;
        const ms = t ? new Date(t).getTime() : 0;
        return isNaN(ms) ? 0 : ms;
      };
      const top5 = [...rows].sort((a, b) => startedMs(b) - startedMs(a)).slice(0, 5);
      setInTransit(top5);
      setInTransitTotal(Number.isFinite(total) ? total : rows.length);
      setInTransitErr(null);
    } catch (e: any) {
      if (signal?.aborted) return;
      setInTransitErr(e?.message || 'unreachable');
    }
  }, []);
  useEffect(() => {
    const ctrl = new AbortController();
    loadInTransit(ctrl.signal);
    const id = window.setInterval(() => loadInTransit(), 45000);
    return () => { ctrl.abort(); window.clearInterval(id); };
  }, [loadInTransit]);

  // Deferring ISPs + reason codes (last 24h). Polled every 45s; re-callable (BUG-15).
  const loadDeferring = useCallback(async (signal?: AbortSignal) => {
    try {
      const res = await apiFetch('/api/mailing/dashboard/deferring-isps?hours=24', { credentials: 'include', signal });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      if (signal?.aborted) return;
      setDeferring(Array.isArray(data?.rows) ? data.rows : []);
      setDeferringErr(null);
    } catch (e: any) {
      if (signal?.aborted) return;
      setDeferringErr(e?.message || 'unreachable');
    }
  }, []);
  useEffect(() => {
    const ctrl = new AbortController();
    loadDeferring(ctrl.signal);
    const id = window.setInterval(() => loadDeferring(), 45000);
    return () => { ctrl.abort(); window.clearInterval(id); };
  }, [loadDeferring]);

  // Trending offers (24h conversions). One-shot — conversions are sparse.
  // Re-callable so a failed fetch can be retried without a reload (BUG-15).
  const loadTrending = useCallback(async (signal?: AbortSignal) => {
    try {
      const res = await apiFetch('/api/mailing/dashboard/trending-offers?hours=24', { credentials: 'include', signal });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      if (signal?.aborted) return;
      setTrending({
        rows: Array.isArray(data?.rows) ? data.rows : [],
        total: Number(data?.total_conversions) || 0,
      });
      setTrendingErr(null);
    } catch (e: any) {
      if (signal?.aborted) return;
      setTrendingErr(e?.message || 'unreachable');
    }
  }, []);
  useEffect(() => {
    const ctrl = new AbortController();
    loadTrending(ctrl.signal);
    return () => ctrl.abort();
  }, [loadTrending]);

  // Lake fetch for Today's Performance. Independent of the PG dashboard fetch
  // so a slow/failed Athena query never blocks the rest of the dashboard.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const statusRes = await apiFetch('/api/mailing/analytics/lake/status', { credentials: 'include' });
        if (!statusRes.ok) return;
        const status = await statusRes.json();
        if (!status?.enabled_read) return;

        // Canonical Reporting-tab query (METRIC_CONTRACT §1/§4): reclassified
        // event_type buckets over transport sources only (pmta+ses — the 'app'
        // mirror stream double-counts delivered). from/to are UTC dt partition
        // bounds; passing local_dt makes the backend widen partitions and apply
        // the precise Denver-day predicate.
        const day = denverToday();
        const bdRes = await apiFetch(
          `/api/mailing/analytics/lake/breakdown?from=${day}&to=${day}&group_by=event_type&local_dt=${day}&source_in=pmta,ses&limit=100`,
          { credentials: 'include' },
        );
        if (!bdRes.ok) return;
        const bd = await bdRes.json();
        if (bd?.disabled || !Array.isArray(bd?.rows)) return;

        const byType: Record<string, number> = {};
        for (const row of bd.rows as Array<{ keys?: { event_type?: string }; count?: number }>) {
          const t = row?.keys?.event_type || '';
          byType[t] = (byType[t] || 0) + (Number(row?.count) || 0);
        }
        if (!cancelled) {
          setLakeToday({
            delivered: byType['delivered'] || 0,
            opens: byType['open'] || 0,
            clicks: byType['click'] || 0,
            hard: byType['hard_bounce'] || 0,
            soft: byType['soft_bounce'] || 0,
            bouncedUntyped: byType['bounced'] || 0,
          });
        }
      } catch {
        // Lake unreachable → keep the PG fallback (lakeToday stays null).
      }
    })();
    return () => { cancelled = true; };
  }, []);

  // Raw + human opens/clicks for the Denver day — the same PG+verdict endpoint
  // the Reporting tab's KPI strip uses (METRIC_CONTRACT §6). Fail-soft: on any
  // failure engToday stays null and the tiles show the lake counts instead.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const day = denverToday();
        const res = await apiFetch(
          `/api/mailing/analytics/engagement?from=${day}&to=${day}`,
          { credentials: 'include' },
        );
        if (!res.ok) return;
        const data = await res.json();
        if (!cancelled && data && typeof data.raw_opens === 'number') {
          setEngToday({
            raw_opens: data.raw_opens || 0,
            human_opens: data.human_opens || 0,
            human_openers: data.human_openers || 0,
            raw_clicks: data.raw_clicks || 0,
            human_clicks: data.human_clicks || 0,
            human_clickers: data.human_clickers || 0,
          });
        }
      } catch {
        // Engagement endpoint unreachable → tiles fall back to lake counts.
      }
    })();
    return () => { cancelled = true; };
  }, []);

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

  // Today's Performance source selection: lake (Athena) when available, else
  // the PG dashboard payload. Revenue ALWAYS comes from the PG payload.
  const perf = dashboard?.performance || {};
  const todayDelivered: number = lakeToday ? lakeToday.delivered : (perf.delivered || 0);
  // Opens/clicks tiles: RAW (machine incl.) counts from the engagement
  // endpoint ONLY. The lake's own open/click buckets under source_in=pmta,ses
  // are the SES-webhook slice — a small fraction of true opens (PMTA emits
  // none) — so falling back to them silently collapsed the tile to a large
  // undercount with a plausible-looking rate. When the engagement fetch fails
  // the tiles now say "unavailable", matching the Reporting tab's fail-soft
  // (QA gate finding, 2026-07-01). Rates divide by lake delivered — the same
  // convention as the Reporting KPI strip (METRIC_CONTRACT §2/§6).
  const todayOpens: number | null = lakeToday && engToday ? engToday.raw_opens : null;
  const todayClicks: number | null = lakeToday && engToday ? engToday.raw_clicks : null;
  // BUG-1: cap at 100%. The numerator (same-day open/click events) and the
  // denominator (same-day delivered) are DIFFERENT cohorts — opens today may be
  // against mail delivered on a prior day — so the ratio can legitimately exceed
  // 100% (prod showed 84.5%+ and could top 100). Capping keeps the tile honest;
  // the metric-card tooltip discloses the same-day-over-same-day approximation.
  const todayOpenRate: number | null = lakeToday
    ? (engToday && lakeToday.delivered > 0 ? Math.min(100, (engToday.raw_opens / lakeToday.delivered) * 100) : null)
    : (perf.open_rate ? Math.min(100, perf.open_rate * 100) : 0);
  const todayClickRate: number | null = lakeToday
    ? (engToday && lakeToday.delivered > 0 ? Math.min(100, (engToday.raw_clicks / lakeToday.delivered) * 100) : null)
    : (perf.click_rate ? Math.min(100, perf.click_rate * 100) : 0);
  // Same-day approximation note reused as the open/click tile tooltip (BUG-1).
  const rateApproxNote = 'Approximate: same-day open/click events ÷ same-day delivered. These are different cohorts — today\'s opens may be against mail delivered on a prior day — so the raw ratio can exceed 100% and is capped at 100% here. Human counts are verdict-filtered.';
  const todayHard: number = lakeToday ? lakeToday.hard : (perf.hard_bounces || 0);
  const todaySoft: number = lakeToday ? lakeToday.soft : (perf.soft_bounces || 0);
  // Bounce-rate denominator = DERIVED attempted (METRIC_CONTRACT §2):
  // delivered + hard + soft + untyped ('bounced', ses-source, no hard/soft split).
  const todayProcessed = lakeToday
    ? (lakeToday.delivered + lakeToday.hard + lakeToday.soft + lakeToday.bouncedUntyped)
    : 0;
  const todayHardRate: number | null = lakeToday
    ? (todayProcessed > 0 ? (todayHard / todayProcessed) * 100 : 0)
    : (perf.hard_bounce_rate ?? null);
  const todaySoftRate: number | null = lakeToday
    ? (todayProcessed > 0 ? (todaySoft / todayProcessed) * 100 : 0)
    : (perf.soft_bounce_rate ?? null);

  // Deferring-ISP count severity accent (BUG-7). Deferrals are TRANSIENT retry
  // events (many per message), NOT failures — so the old >=5000/>=500 red/amber
  // bands lit every ISP red on a normal prod day and read like an outage. Red is
  // now reserved for genuinely extreme volume; most counts render neutral.
  const deferSeverity = (n: number): string =>
    n >= 200000 ? colors.danger : n >= 50000 ? colors.warning : colors.text;

  return (
    <div className="enhanced-dashboard ig-fade-in">
      {/* ── Row 1 · Today's Performance — the answer at the top (KPIs) ────── */}
      <div className="metrics-section">
        <h3>
          <FontAwesomeIcon icon={faChartLine} /> Today's Performance
          {' '}
          <span
            title={lakeToday
              ? 'Source: analytics event lake (Athena) — Denver calendar day, sources pmta+ses only (the app mirror stream is excluded so delivered is not double-counted). Opens/clicks are raw (machine incl.) from the PG+verdict engagement endpoint.'
              : 'Source: Postgres tracking tables — SES routes are pixel-blind here (SES strips tracking pixels), so opens/clicks under-count on SES-routed mail.'}
            style={{
              fontSize: 10, fontWeight: 600, letterSpacing: 0.5, textTransform: 'uppercase',
              padding: '2px 8px', borderRadius: 10, verticalAlign: 'middle', marginLeft: 6,
              background: lakeToday ? 'rgba(99,102,241,0.18)' : 'rgba(148,163,184,0.15)',
              color: lakeToday ? '#a5b4fc' : '#94a3b8',
              border: `1px solid ${lakeToday ? 'rgba(99,102,241,0.4)' : 'rgba(148,163,184,0.3)'}`,
            }}
          >
            {lakeToday ? 'lake · Denver day · pmta+ses' : 'pg — SES routes are pixel-blind here'}
          </span>
        </h3>
        <div className="metrics-grid">
          <div className="metric-card" title="Today's send volume — total mail attempted (delivered + hard + soft + untyped bounces), the Denver-day count of everything that left. Delivered is the subset that landed.">
            <span className="metric-icon"><FontAwesomeIcon icon={faTruckFast} /></span>
            <div className="metric-content">
              <span className="metric-value">{todayProcessed.toLocaleString()}</span>
              <span className="metric-label">Volume{todayProcessed > 0 ? ` · ${((todayDelivered / todayProcessed) * 100).toFixed(1)}% delivered` : ''}</span>
            </div>
          </div>
          <div className="metric-card">
            <span className="metric-icon"><FontAwesomeIcon icon={faPaperPlane} /></span>
            <div className="metric-content">
              <span className="metric-value">{todayDelivered.toLocaleString()}</span>
              <span className="metric-label">Delivered</span>
            </div>
          </div>
          <div className="metric-card" title={rateApproxNote}>
            <span className="metric-icon"><FontAwesomeIcon icon={faEnvelope} /></span>
            <div className="metric-content">
              <span className="metric-value">{todayOpenRate != null ? `${todayOpenRate.toFixed(1)}%` : '—'}</span>
              <span className="metric-label">Open Rate*{todayOpens != null ? ` (${todayOpens.toLocaleString()} opens)` : ''}</span>
              {lakeToday ? (
                <span style={{ display: 'block', fontSize: 10, opacity: 0.65 }}>
                  {engToday ? `machine incl. · human ${engToday.human_opens.toLocaleString()}` : 'engagement unavailable'}
                </span>
              ) : null}
            </div>
          </div>
          <div className="metric-card" title={rateApproxNote}>
            <span className="metric-icon"><FontAwesomeIcon icon={faCrosshairs} /></span>
            <div className="metric-content">
              <span className="metric-value">{todayClickRate != null ? `${todayClickRate.toFixed(1)}%` : '—'}</span>
              <span className="metric-label">Click Rate*{todayClicks != null ? ` (${todayClicks.toLocaleString()} clicks)` : ''}</span>
              {lakeToday ? (
                <span style={{ display: 'block', fontSize: 10, opacity: 0.65 }}>
                  {engToday ? `machine incl. · human ${engToday.human_clicks.toLocaleString()}` : 'engagement unavailable'}
                </span>
              ) : null}
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
            <span className="metric-icon" style={{ color: todayHard > 0 ? '#ef4444' : '#475569' }}>
              <FontAwesomeIcon icon={faBan} />
            </span>
            <div className="metric-content">
              <span className="metric-value" style={{ color: todayHard > 0 ? '#ef4444' : undefined }}>
                {todayHard.toLocaleString()}
              </span>
              <span className="metric-label">
                Hard Bounce {todayHardRate != null && todayHardRate > 0 ? `(${todayHardRate.toFixed(2)}%)` : ''}
              </span>
            </div>
          </div>
          <div
            className="metric-card"
            title="Soft bounces from the analytics lake include transient failures (greylisting, throttling, temporary defers) that were later RETRIED and delivered — so this is NOT a final-undelivered rate and reads higher than the mail that actually failed. Hard bounces (list-hygiene failures) are the reputation signal."
          >
            <span className="metric-icon" style={{ color: todaySoft > 0 ? '#f59e0b' : '#475569' }}>
              <FontAwesomeIcon icon={faBan} />
            </span>
            <div className="metric-content">
              <span className="metric-value" style={{ color: todaySoft > 0 ? '#f59e0b' : undefined }}>
                {todaySoft.toLocaleString()}
              </span>
              <span className="metric-label">
                Soft Bounce {todaySoftRate != null && todaySoftRate > 0 ? `(${todaySoftRate.toFixed(2)}%)*` : '*'}
              </span>
            </div>
          </div>
        </div>
      </div>

      {/* ── Row 2 · In-Transit (hero) + Platform Sending Cap ─────────────── */}
      <div style={{ display: 'grid', gridTemplateColumns: 'minmax(0, 2fr) minmax(320px, 1fr)', gap: 14, marginBottom: 18, alignItems: 'stretch' }}>
        {/* In-transit: "what's mailing right now" — top 5 most recently started */}
        <Panel accent={colors.indigo500}>
          <SectionHeader
            title="In Transit · Now Mailing"
            icon={faTruckFast}
            right={<span style={{ fontSize: 11, color: colors.textMuted }} title="Broadcast waves only — continuous partner-drip micro-campaigns are excluded (exclude_drip). Count = all non-drip campaigns in the sending state; the list shows the most recently started of them.">
              {inTransit === null ? '…' : `${inTransitTotal.toLocaleString()} sending${inTransit.length < inTransitTotal ? ` · top ${inTransit.length} by start` : ''}`} · live 45s
            </span>}
          />
          {inTransitErr ? (
            <SectionError label="In-transit campaigns" error={inTransitErr} onRetry={() => loadInTransit()} />
          ) : inTransit == null ? (
            <div style={{ fontSize: 12, color: colors.textMuted, padding: '18px 4px' }}>
              <FontAwesomeIcon icon={faSpinner} spin style={{ marginRight: 8 }} /> Loading in-transit campaigns…
            </div>
          ) : inTransit.length === 0 ? (
            <EmptyState icon={faPaperPlane} title="Nothing in transit" hint="Campaigns appear here the moment they enter the sending state." />
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column' }}>
              {inTransit.map((c) => {
                const pct = c.total_recipients > 0 ? c.sent_count / c.total_recipients : 0;
                return (
                  <div key={c.id} style={{ padding: '10px 0', borderTop: `1px solid ${colors.divider}` }}>
                    <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 12, marginBottom: 6 }}>
                      <span title={c.name} style={{ fontSize: 13, fontWeight: 600, color: colors.text, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', maxWidth: '62%' }}>{c.name}</span>
                      <span style={{ fontSize: 11, color: colors.textMuted, fontVariantNumeric: 'tabular-nums', whiteSpace: 'nowrap' }}>started {denverClock(c.started_at || c.scheduled_at)}</span>
                    </div>
                    <ProgressBar pct={pct} />
                    <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginTop: 5, fontSize: 11, color: colors.textMuted }}>
                      <span title={c.profile_name ? `profile: ${c.profile_name}` : undefined} style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', maxWidth: '55%' }}>
                        {c.from_name || c.profile_name || '—'}
                      </span>
                      <span style={{ fontVariantNumeric: 'tabular-nums' }}>
                        <strong style={{ color: colors.indigo200 }}>{c.sent_count.toLocaleString()}</strong> / {c.total_recipients.toLocaleString()} ({(pct * 100).toFixed(0)}%)
                      </span>
                    </div>
                  </div>
                );
              })}
            </div>
          )}
        </Panel>

        {/* Platform Sending Cap / throttle (kept — existing system card) */}
        <div className="system-card sending ig-card-hover ig-scan-line">
          <div className="system-header">
            <span className="system-icon"><FontAwesomeIcon icon={faPaperPlane} /></span>
            <h3>Email Sending</h3>
            {/* BUG-12: derive the pill from live utilization instead of a static
                green "Active" that reassured even when the cap was maxed or idle. */}
            {(() => {
              const util = dashboard?.platform_daily_utilization || 0;
              const sent = dashboard?.platform_daily_sent ?? dashboard?.daily_used ?? 0;
              if (util > 90) return <span className="status-badge scheduled" title={`${util.toFixed(1)}% of daily cap used`}>Near cap</span>;
              if (sent > 0) return <span className="status-badge active" title={`${Number(sent).toLocaleString()} sent today`}>Active</span>;
              return <span className="status-badge draft" title="No sends recorded yet today">Idle</span>;
            })()}
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
      </div>

      {/* ── Row 3 · Deferring ISPs · Trending Offers · Click Funnels ──────── */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(320px, 1fr))', gap: 14, marginBottom: 18, alignItems: 'start' }}>
        {/* Deferring ISPs + reason codes (last 24h, live 45s) */}
        <Panel accent={colors.indigo500}>
          <SectionHeader
            title="Deferrals · transient retries · 24h"
            icon={faTriangleExclamation}
            right={<span style={{ fontSize: 11, color: colors.textMuted }} title="Deferrals are TRANSIENT retry events (greylisting, throttling, temporary defers) — the ISP asked us to try again later, not a failure. Many retries occur per message; a high count on a healthy day is normal.">PMTA · retries, not failures · live 45s</span>}
          />
          {deferringErr ? (
            <SectionError label="Deferring ISPs" error={deferringErr} onRetry={() => loadDeferring()} />
          ) : deferring == null ? (
            <div style={{ fontSize: 12, color: colors.textMuted, padding: '14px 4px' }}>
              <FontAwesomeIcon icon={faSpinner} spin style={{ marginRight: 8 }} /> Loading deferrals…
            </div>
          ) : deferring.length === 0 ? (
            <EmptyState icon={faServer} title="No deferrals in the last 24h" hint="ISPs deferring PMTA mail (with DSN reason codes) appear here." />
          ) : (
            <div style={{ overflowX: 'auto' }}>
              <table style={tableStyle}>
                <thead>
                  <tr>
                    <th style={thStyle}>ISP</th>
                    <th style={numTh} title="Transient retry events in the last 24h — not failures">Retries</th>
                    <th style={thStyle}>Top reason</th>
                  </tr>
                </thead>
                <tbody>
                  {deferring.slice(0, 8).map((d) => {
                    const top = d.reasons && d.reasons[0];
                    return (
                      <tr key={d.isp}>
                        <td style={tdStyle}>{d.isp}</td>
                        <td style={{ ...numTd, color: deferSeverity(d.deferred), fontWeight: 700 }}>{d.deferred.toLocaleString()}</td>
                        <td style={tdStyle} title={top?.sample || 'no DSN sample'}>
                          {top ? `${top.code} (${top.count.toLocaleString()})` : '—'}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
        </Panel>

        {/* Trending offers — 24h conversions (sparse by design) */}
        <Panel accent={colors.success}>
          <SectionHeader
            title="Trending Offers · 24h"
            icon={faFire}
            right={trending ? (
              <span style={{ fontSize: 11, color: colors.textMuted }}>
                {trending.total.toLocaleString()} conversions
              </span>
            ) : undefined}
          />
          {trendingErr ? (
            <SectionError label="Trending offers" error={trendingErr} onRetry={() => loadTrending()} />
          ) : trending == null ? (
            <div style={{ fontSize: 12, color: colors.textMuted, padding: '14px 4px' }}>
              <FontAwesomeIcon icon={faSpinner} spin style={{ marginRight: 8 }} /> Loading conversions…
            </div>
          ) : trending.rows.length === 0 ? (
            <EmptyState icon={faFire} title="No conversions in last 24h" hint="Conversions are sparse — only CPA/CPL offers write a conversion ledger row." />
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column' }}>
              {trending.rows.map((o, i) => (
                <div key={o.offer_id} style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 12, padding: '7px 0', borderTop: `1px solid ${colors.divider}` }}>
                  <span style={{ fontSize: 13, color: colors.text, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', maxWidth: '72%' }} title={o.name || o.offer_id}>
                    <span style={{ color: colors.textFaint, marginRight: 6 }}>{i + 1}.</span>{o.name || o.offer_id}
                  </span>
                  <span style={{ fontSize: 13, fontWeight: 700, color: colors.successText, fontVariantNumeric: 'tabular-nums' }}>{o.conversions.toLocaleString()}</span>
                </div>
              ))}
            </div>
          )}
        </Panel>

        {/* Click funnels — LIVE click-drip lanes */}
        <Panel accent={colors.indigo500}>
          <SectionHeader
            title="Click Funnels"
            icon={faRoute}
            right={funnels ? (
              <span style={{ fontSize: 11, color: colors.textMuted }}>
                {funnels.total_active_lanes.toLocaleString()} active lanes · {funnels.total_enrolled.toLocaleString()} enrolled
              </span>
            ) : undefined}
          />
          {funnelsErr ? (
            <SectionError label="Click funnels" error={funnelsErr} onRetry={() => loadFunnels()} />
          ) : funnels == null ? (
            <div style={{ fontSize: 12, color: colors.textMuted, padding: '14px 4px' }}>
              <FontAwesomeIcon icon={faSpinner} spin style={{ marginRight: 8 }} /> Loading lanes…
            </div>
          ) : funnels.lanes.length === 0 ? (
            <EmptyState icon={faRoute} title="No click-drip lanes" hint="Live click-drip offer lanes and their enrollment appear here." />
          ) : (
            <>
              <div style={{ display: 'flex', gap: 24, marginBottom: 8 }}>
                <Stat label="Active lanes" value={funnels.total_active_lanes.toLocaleString()} color={colors.indigo200} title="Lanes with enabled=true in mailing_offer_journey_map" />
                <Stat label="Enrolled" value={funnels.total_enrolled.toLocaleString()} color={colors.text} title="Sum of currently-active enrollments across all lanes" />
              </div>
              <div style={{ display: 'flex', flexDirection: 'column' }}>
                {[...funnels.lanes].sort((a, b) => b.active_enrolled - a.active_enrolled).slice(0, 5).map((l) => (
                  <div key={l.offer_id} style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 10, padding: '6px 0', borderTop: `1px solid ${colors.divider}` }}>
                    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6, minWidth: 0 }}>
                      <span style={{ fontSize: 13, color: colors.text, fontWeight: 600, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', maxWidth: 150 }} title={l.name || `offer ${l.offer_id}`}>
                        {l.name || `offer ${l.offer_id}`}
                      </span>
                      {!l.enabled && <Pill color={colors.idle} style={{ padding: '0 6px', fontSize: 9 }}>off</Pill>}
                    </span>
                    <span style={{ fontSize: 12, color: colors.textMuted, fontVariantNumeric: 'tabular-nums', whiteSpace: 'nowrap' }}>
                      <strong style={{ color: colors.indigo200 }}>{l.active_enrolled.toLocaleString()}</strong> active
                      <span title="lifetime (all-time), not windowed"> · {l.completed_or_converted.toLocaleString()} done*</span>
                    </span>
                  </div>
                ))}
              </div>
              <div style={{ fontSize: 10, color: colors.textFaint, marginTop: 8 }}>* completed/converted is lifetime (all-time), not last-24h.</div>
            </>
          )}
        </Panel>
      </div>

      {/* ── Row 4 · Audience Growth + Recent Campaigns ───────────────────── */}
      <div style={{ display: 'grid', gridTemplateColumns: 'minmax(320px, 1fr) minmax(0, 1.4fr)', gap: 14, marginBottom: 18, alignItems: 'start' }}>
        <Panel accent={!growth ? undefined : growth.acquired7d - growth.churned7d >= 0 ? colors.success : colors.danger}>
          <SectionHeader title="Audience Growth · 7 Denver days" icon={faSeedling} />
          {growth ? (
            <>
              <div style={{ display: 'flex', gap: 28, flexWrap: 'wrap' }}>
                <Stat label="Acquired" value={growth.acquired7d.toLocaleString()} color={colors.successText} title="Subscribers acquired in the last 7 Denver days (COALESCE(subscribed_at, created_at))" />
                <Stat label="Churned" value={growth.churned7d.toLocaleString()} color={colors.dangerText} title="Subscribers who unsubscribed or reached a terminal status (bounced/complained/blacklisted/unsubscribed) in the last 7 Denver days" />
                <Stat label="Activated" value={growth.activated.toLocaleString()} color={colors.indigo300} title="Early-funnel pool: subscribers we have mailed ≤4 times who have opened or clicked (current state, all-time — not a 7-day flow)" />
                <Stat
                  label="Net"
                  value={`${growth.acquired7d - growth.churned7d >= 0 ? '+' : ''}${(growth.acquired7d - growth.churned7d).toLocaleString()}`}
                  color={growth.acquired7d - growth.churned7d >= 0 ? colors.success : colors.danger}
                  title="Net change = acquired − churned"
                />
              </div>
              <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 10 }}>
                Acquired vs churned across the last 7 America/Denver days (PG mailing_subscribers — membership truth, not the delivery lake). Activated = early-funnel pool (mailed ≤4×, opened or clicked), a current-state count.
              </div>
            </>
          ) : growthErr ? (
            <SectionError label="Audience growth" error={growthErr} onRetry={() => loadGrowth()} />
          ) : (
            <EmptyState icon={faSeedling} title="Audience growth unavailable" hint="Acquisition vs churn appears once the subscriber store is reachable." />
          )}
        </Panel>

        <Panel accent={colors.indigo500}>
          <SectionHeader title="Recent Campaigns" icon={faListUl} />
          {Array.isArray(dashboard?.recent_campaigns) && dashboard.recent_campaigns.length > 0 ? (
            <div style={{ overflowX: 'auto' }}>
              <table style={tableStyle}>
                <thead>
                  <tr>
                    <th style={thStyle}>Campaign</th>
                    <th style={thStyle}>Status</th>
                    <th style={numTh}>Sent</th>
                    <th style={numTh}>Opens</th>
                  </tr>
                </thead>
                <tbody>
                  {dashboard.recent_campaigns.map((c: any, i: number) => (
                    <tr key={c.id || i}>
                      <td style={{ ...tdStyle, maxWidth: 260, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={c.name}>{c.name}</td>
                      <td style={tdStyle}>
                        <Pill color={stateColor(c.status || '')} style={{ padding: '1px 8px', fontSize: 10 }}>{c.status || 'unknown'}</Pill>
                      </td>
                      <td style={numTd}>{Number(c.sent_count || 0).toLocaleString()}</td>
                      <td style={numTd}>{Number(c.open_count || 0).toLocaleString()}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <EmptyState icon={faListUl} title="No campaigns yet" hint="Recently created campaigns and their send/open counts appear here." />
          )}
        </Panel>
      </div>

      {/* ── System Health (kept working pieces) ──────────────────────────── */}
      <div className="system-overview ig-stagger">
        <WorkerHealthWidget />

        <div className="system-card intelligence ig-card-hover ig-scan-line">
          <div className="system-header">
            <span className="system-icon"><FontAwesomeIcon icon={faBrain} /></span>
            <h3>Inbox Intelligence</h3>
            {/* BUG-12: "Learning" only when profiles were actually built today. */}
            {(dashboard?.platform_intelligence?.inbox_profiles_today || 0) > 0
              ? <span className="status-badge active" title={`${Number(dashboard?.platform_intelligence?.inbox_profiles_today || 0).toLocaleString()} profiles built today`}>Learning</span>
              : <span className="status-badge draft" title="No inbox profiles built yet today">Idle</span>}
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
            {/* BUG-12: "Protected" only when a suppression list actually exists. */}
            {(dashboard?.global_suppressions_total || 0) > 0
              ? <span className="status-badge active" title={`${Number(dashboard?.global_suppressions_total || 0).toLocaleString()} addresses blocked`}>Protected</span>
              : <span className="status-badge draft" title="No suppression entries loaded">Inactive</span>}
          </div>
          <div className="system-description">
            <p style={{ fontSize: '0.8rem', opacity: 0.7, margin: 0 }}>
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
            {/* BUG-12: "Running" only when there are active workflows. */}
            {(dashboard?.active_automations || 0) > 0
              ? <span className="status-badge active" title={`${Number(dashboard?.active_automations || 0).toLocaleString()} active workflows`}>Running</span>
              : <span className="status-badge draft" title="No active workflows">Idle</span>}
          </div>
          <div className="system-stats">
            <div className="stat">
              <span className="stat-value">{dashboard?.active_automations || 0}</span>
              <span className="stat-label">Active Workflows</span>
            </div>
          </div>
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
        <h3>Sending Infrastructure</h3>
        <p>Your mail is sent through dedicated sending servers. Each manages dedicated IP addresses,
        authentication (DKIM), and provider-specific routing for maximum deliverability.</p>
      </div>

      {!hasPMTA && (
        <div className="no-data" style={{textAlign:'center', padding:'40px 20px'}}>
          <p>No sending servers configured yet. Your sending infrastructure will appear here once set up.</p>
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
        <h4>How Sending Works</h4>
        <ol>
          <li><strong>Delivery</strong> — Emails are sent through your dedicated sending servers with per-provider routing rules.</li>
          <li><strong>Suppression Check</strong> — Before sending, each address is checked against bounces, complaints, and your global do-not-contact list.</li>
          <li><strong>IP Rotation</strong> — Messages rotate across your dedicated IP addresses based on mailbox provider and warm-up stage.</li>
          <li><strong>Authentication</strong> — Domain-specific DKIM signatures are applied to every outgoing message.</li>
          <li><strong>Tracking</strong> — Opens and clicks are measured through the platform tracking pixel and link wrapper.</li>
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
  const subTab = (['pmta-wizard', 'send-day', 'draft-board', 'marketing-agent', 'schedule'].includes(activeSubTab)) ? activeSubTab : 'campaign-center';
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
        <button style={subNavBtnStyle(subTab === 'draft-board')} onClick={() => onSubTabChange('draft-board')}>Draft Board</button>
        <button style={subNavBtnStyle(subTab === 'schedule')} onClick={() => onSubTabChange('schedule')}>Schedule</button>
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
        ) : subTab === 'draft-board' ? (
          <DraftBoardView />
        ) : subTab === 'schedule' ? (
          <ScheduleView />
        ) : (
          <CampaignPortal initialOffer={pendingOffer} onOfferConsumed={onOfferConsumed} onEditInWizard={handleEditInWizard} />
        )}
      </Suspense>
    </div>
  );
};

// ─── AI Agents Section ─────────────────────────────────────────────────────
const AIAgentsSection: React.FC<{ activeSubTab: TabId; onSubTabChange: (t: TabId) => void }> = ({ activeSubTab }) => {
  // AI Plans (sending-plans) and Jarvis are hidden from the nav for now; only
  // Inbox Intel (profiles) is exposed. The render branches are kept so existing
  // deep links / cross-tab events still resolve, but the buttons are gone.
  const subTab = (['profiles', 'jarvis', 'sending-plans'].includes(activeSubTab)) ? activeSubTab : 'profiles';
  return (
    <div>
      <Suspense fallback={<ChunkLoader />}>
        {subTab === 'jarvis' ? <JarvisDashboard /> : subTab === 'sending-plans' ? <ISPAgentIntelligence /> : <InboxProfiles />}
      </Suspense>
    </div>
  );
};

// _AnalyticsDashboard and SuggestionsWidget legacy stubs were removed in
// PAGE_VERSION_DASHBOARD = 1.0 (2026-05-08). AnalyticsCenter was retired
// 2026-06-09; the active analytics surface is EventLakeExplorer (tab id
// 'event-lake', labeled "Analytics", mounted in renderContent above).

export default MailingPortal;

