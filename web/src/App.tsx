import React from 'react';
// ── Analytics imports (commented out — re-enable when scaling) ───────────────
// import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
// import {
//   faChartLine, faBolt, faPaperPlane, faCloud, faServer, faGlobe,
//   faFileAlt, faDollarSign, faDatabase, faClipboardList, faReceipt,
//   faChartPie, faBrain, faCodeBranch, faLightbulb, faSignal, faCog
// } from '@fortawesome/free-solid-svg-icons';
// import { DashboardView } from './components/dashboard/DashboardView';
// import { ISPPerformance } from './components/isp/ISPPerformance';
// import { IPPerformance } from './components/ip/IPPerformance';
// import { DomainPerformance } from './components/domain/DomainPerformance';
// import { SignalsView } from './components/signals/SignalsView';
// import { AgentChat } from './components/agent/AgentChat';
// import { MailgunDashboard } from './components/mailgun';
// import { SESDashboard } from './components/ses';
// import { OngageDashboard } from './components/ongage';
// import { EverflowDashboard } from './components/everflow';
// import { DataInjectionsDashboard } from './components/datainjections';
// import { KanbanBoard } from './components/kanban';
// import { ContractsDashboard } from './components/contracts';
// import { FinancialDashboard } from './components/financial';
// import { IntelligenceDashboard } from './components/intelligence';
// import { PlanningDashboard } from './components/planning';
// import { SuggestionButton, ImprovementsDashboard } from './components/suggestions';
// import { InjectionAnalytics } from './components/dashboard/InjectionAnalytics';
// import { DataPartnerDashboard } from './components/datapartners/DataPartnerDashboard';
// import { SettingsDashboard } from './components/settings/SettingsDashboard';
// import PMTADashboard from './components/pmta/PMTADashboard';
import { GoogleLogin } from './components/auth';
// import { useApi } from './hooks/useApi';
// import { DateFilter } from './components/common/DateFilter';
// import { PasswordProtect } from './components/common';
// ── End analytics imports ────────────────────────────────────────────────────

import { MailingPortal } from './components/mailing/pages/MailingPortal';
import { AuthProvider } from './contexts/AuthContext';
import { DateFilterProvider } from './context/DateFilterContext';
import { CostOverrideProvider } from './context/CostOverrideContext';
import { ThemeProvider } from './context/ThemeContext';

// PMTA-only mode: App renders the Mailing Portal directly.
// To re-enable the full analytics platform, restore the commented-out
// imports above and the analytics layout below.

const App: React.FC = () => {
  return (
    <ThemeProvider>
      <AuthProvider>
        <GoogleLogin>
          <DateFilterProvider>
            <CostOverrideProvider>
              <MailingPortal />
            </CostOverrideProvider>
          </DateFilterProvider>
        </GoogleLogin>
      </AuthProvider>
    </ThemeProvider>
  );

  /* ── Analytics layout (commented out — re-enable when scaling) ──────────────

  type View = 'dashboard' | 'isp' | 'ip' | 'domain' | 'signals' | 'agent' | 'mailgun' | 'ses' | 'ongage' | 'everflow' | 'datainjections' | 'datapartners' | 'kanban' | 'contracts' | 'financial' | 'intelligence' | 'planning' | 'improvements' | 'mailing' | 'injection-analytics' | 'settings' | 'pmta';

  interface HealthStatus {
    status: string;
    is_running: boolean;
    last_fetch: string;
  }

  const [activeView, setActiveView] = useState<View>('dashboard');

  const { data: health } = useApi<HealthStatus>('/health', {
    pollingInterval: 30000,
  });

  const navItems: { id: View; label: string; icon?: React.ReactNode }[] = [
    { id: 'mailing', label: 'Mailing', icon: <FontAwesomeIcon icon={faPaperPlane} className="fa-icon" /> },
    { id: 'dashboard', label: 'SparkPost', icon: <FontAwesomeIcon icon={faBolt} className="fa-icon" /> },
    { id: 'mailgun', label: 'Mailgun', icon: <FontAwesomeIcon icon={faPaperPlane} className="fa-icon" /> },
    { id: 'ses', label: 'AWS SES', icon: <FontAwesomeIcon icon={faCloud} className="fa-icon" /> },
    { id: 'pmta', label: 'PowerMTA', icon: <FontAwesomeIcon icon={faServer} className="fa-icon" /> },
    { id: 'ongage', label: 'Ongage', icon: <FontAwesomeIcon icon={faFileAlt} className="fa-icon" /> },
    { id: 'everflow', label: 'Revenue', icon: <FontAwesomeIcon icon={faDollarSign} className="fa-icon" /> },
    { id: 'contracts', label: 'Contracts', icon: <FontAwesomeIcon icon={faReceipt} className="fa-icon" /> },
    { id: 'financial', label: 'Financials', icon: <FontAwesomeIcon icon={faChartPie} className="fa-icon" /> },
    { id: 'intelligence', label: 'Intelligence', icon: <FontAwesomeIcon icon={faBrain} className="fa-icon" /> },
    { id: 'planning', label: 'Planning', icon: <FontAwesomeIcon icon={faCodeBranch} className="fa-icon" /> },
    { id: 'datainjections', label: 'Data Injections', icon: <FontAwesomeIcon icon={faDatabase} className="fa-icon" /> },
    { id: 'datapartners', label: 'Data Partners', icon: <FontAwesomeIcon icon={faDatabase} className="fa-icon" /> },
    { id: 'injection-analytics', label: 'Injection Analytics', icon: <FontAwesomeIcon icon={faSignal} className="fa-icon" /> },
    { id: 'isp', label: 'ISP Performance', icon: <FontAwesomeIcon icon={faChartLine} className="fa-icon" /> },
    { id: 'ip', label: 'Sending IPs', icon: <FontAwesomeIcon icon={faServer} className="fa-icon" /> },
    { id: 'domain', label: 'Domains', icon: <FontAwesomeIcon icon={faGlobe} className="fa-icon" /> },
    { id: 'signals', label: 'Signals', icon: <FontAwesomeIcon icon={faSignal} className="fa-icon" /> },
    { id: 'agent', label: 'Agent', icon: <FontAwesomeIcon icon={faBrain} className="fa-icon" /> },
    { id: 'kanban', label: 'Tasks', icon: <FontAwesomeIcon icon={faClipboardList} className="fa-icon" /> },
    { id: 'improvements', label: 'Improvements', icon: <FontAwesomeIcon icon={faLightbulb} className="fa-icon" /> },
    { id: 'settings', label: 'Settings', icon: <FontAwesomeIcon icon={faCog} className="fa-icon" /> },
  ];

  const renderView = () => {
    switch (activeView) {
      case 'dashboard':
        return <DashboardView />;
      case 'mailgun':
        return <MailgunDashboard />;
      case 'ses':
        return <SESDashboard />;
      case 'pmta':
        return <PMTADashboard />;
      case 'ongage':
        return <OngageDashboard />;
      case 'everflow':
        return <EverflowDashboard />;
      case 'contracts':
        return <ContractsDashboard />;
      case 'financial':
        return (
          <PasswordProtect
            storageKey="financials_auth"
            title="Financials Access"
            description="This section contains sensitive financial data and requires authentication."
          >
            <FinancialDashboard />
          </PasswordProtect>
        );
      case 'intelligence':
        return <IntelligenceDashboard />;
      case 'planning':
        return <PlanningDashboard />;
      case 'datainjections':
        return <DataInjectionsDashboard />;
      case 'datapartners':
        return <DataPartnerDashboard />;
      case 'injection-analytics':
        return <InjectionAnalytics />;
      case 'kanban':
        return <KanbanBoard />;
      case 'isp':
        return <ISPPerformance />;
      case 'ip':
        return <IPPerformance />;
      case 'domain':
        return <DomainPerformance />;
      case 'signals':
        return <SignalsView />;
      case 'agent':
        return <AgentChat />;
      case 'improvements':
        return <ImprovementsDashboard />;
      case 'settings':
        return <SettingsDashboard />;
      case 'mailing':
        return <MailingPortal />;
      default:
        return <DashboardView />;
    }
  };

  const getStatusIndicatorClass = () => {
    if (!health) return '';
    if (health.status === 'healthy' && health.is_running) return '';
    if (health.status === 'degraded') return 'warning';
    return 'error';
  };

  if (activeView === 'mailing') {
    return (
      <ThemeProvider>
      <AuthProvider>
        <GoogleLogin>
          <DateFilterProvider>
            <CostOverrideProvider>
              <MailingPortal />
            </CostOverrideProvider>
          </DateFilterProvider>
        </GoogleLogin>
      </AuthProvider>
      </ThemeProvider>
    );
  }

  return (
    <ThemeProvider>
    <AuthProvider>
      <GoogleLogin>
        <DateFilterProvider>
          <CostOverrideProvider>
        <div className="app app-sidebar-layout">
          <header className="header">
            <div className="header-title">
              <FontAwesomeIcon icon={faBolt} className="header-logo" />
              <h1>Jarvis Analytics</h1>
            </div>
            <div className="header-controls">
              <DateFilter />
              <div className="header-status">
                <span className={`status-indicator ${getStatusIndicatorClass()}`} />
                <span>
                  {health?.is_running ? 'Live' : 'Offline'}
                  {health?.last_fetch && (
                    <span style={{ marginLeft: '8px', color: 'var(--text-muted)' }}>
                      • {new Date(health.last_fetch).toLocaleTimeString()}
                    </span>
                  )}
                </span>
              </div>
              <a
                href="/auth/logout"
                className="btn-ghost btn-small"
                style={{ textDecoration: 'none' }}
              >
                Logout
              </a>
            </div>
          </header>

          <div className="app-body">
            <aside className="sidebar">
              <nav className="sidebar-nav">
                {navItems.map((item) => (
                  <button
                    key={item.id}
                    className={`sidebar-nav-item ${activeView === item.id ? 'active' : ''}`}
                    onClick={() => setActiveView(item.id)}
                    title={item.label}
                  >
                    <span className="sidebar-nav-icon">{item.icon}</span>
                    <span className="sidebar-nav-label">{item.label}</span>
                  </button>
                ))}
              </nav>
            </aside>

            <main className="main-content">
              {renderView()}
            </main>
          </div>

          <SuggestionButton />
        </div>
        </CostOverrideProvider>
      </DateFilterProvider>
    </GoogleLogin>
    </AuthProvider>
    </ThemeProvider>
  );

  ── End analytics layout ─────────────────────────────────────────────────── */
};

export default App;
