import React, { useState } from 'react';
import { MailingDashboard } from '../components/MailingDashboard';
import { ListsManager } from '../components/ListsManager';
import { CampaignsManager } from '../components/CampaignsManager';
import { SendingPlans } from '../components/SendingPlans';
import './MailingPortal.css';

type TabId = 'dashboard' | 'lists' | 'campaigns' | 'sending-plans' | 'delivery-servers' | 'templates';

interface Tab {
  id: TabId;
  label: string;
  icon: string;
}

const tabs: Tab[] = [
  { id: 'dashboard', label: 'Dashboard', icon: '📊' },
  { id: 'campaigns', label: 'Campaigns', icon: '📧' },
  { id: 'lists', label: 'Lists', icon: '📋' },
  { id: 'sending-plans', label: 'AI Plans', icon: '🤖' },
  { id: 'delivery-servers', label: 'Servers', icon: '🖥️' },
  { id: 'templates', label: 'Templates', icon: '📝' },
];

export const MailingPortal: React.FC = () => {
  const [activeTab, setActiveTab] = useState<TabId>('dashboard');

  const renderContent = () => {
    switch (activeTab) {
      case 'dashboard':
        return <MailingDashboard />;
      case 'lists':
        return <ListsManager />;
      case 'campaigns':
        return <CampaignsManager />;
      case 'sending-plans':
        return <SendingPlans />;
      case 'delivery-servers':
        return <DeliveryServersPlaceholder />;
      case 'templates':
        return <TemplatesPlaceholder />;
      default:
        return <MailingDashboard />;
    }
  };

  return (
    <div className="mailing-portal">
      <aside className="mailing-sidebar">
        <div className="sidebar-header">
          <h1>📬 IGNITE</h1>
          <span className="subtitle">Mailing Platform</span>
        </div>

        <nav className="sidebar-nav">
          {tabs.map((tab) => (
            <button
              key={tab.id}
              className={`nav-item ${activeTab === tab.id ? 'active' : ''}`}
              onClick={() => setActiveTab(tab.id)}
            >
              <span className="nav-icon">{tab.icon}</span>
              <span className="nav-label">{tab.label}</span>
            </button>
          ))}
        </nav>

        <div className="sidebar-footer">
          <div className="quick-stats">
            <div className="quick-stat">
              <span className="quick-stat-value">8.2M</span>
              <span className="quick-stat-label">Daily Capacity</span>
            </div>
            <div className="quick-stat">
              <span className="quick-stat-value">98.5%</span>
              <span className="quick-stat-label">Deliverability</span>
            </div>
          </div>
          <button className="analytics-link" onClick={() => window.location.href = '/'}>
            ← Back to Analytics
          </button>
        </div>
      </aside>

      <main className="mailing-content">
        {renderContent()}
      </main>
    </div>
  );
};

// Placeholder components for features not yet implemented
const DeliveryServersPlaceholder: React.FC = () => (
  <div className="placeholder-page">
    <div className="placeholder-content">
      <span className="placeholder-icon">🖥️</span>
      <h2>Delivery Servers</h2>
      <p>Manage SparkPost, AWS SES, and other email delivery servers.</p>
      <div className="feature-list">
        <div className="feature">✓ Multi-provider support</div>
        <div className="feature">✓ Automatic failover</div>
        <div className="feature">✓ IP warmup management</div>
        <div className="feature">✓ Reputation monitoring</div>
      </div>
      <span className="coming-soon">Coming Soon</span>
    </div>
  </div>
);

const TemplatesPlaceholder: React.FC = () => (
  <div className="placeholder-page">
    <div className="placeholder-content">
      <span className="placeholder-icon">📝</span>
      <h2>Email Templates</h2>
      <p>Create and manage reusable email templates with drag-and-drop editor.</p>
      <div className="feature-list">
        <div className="feature">✓ Drag-and-drop builder</div>
        <div className="feature">✓ Personalization tags</div>
        <div className="feature">✓ Mobile responsive</div>
        <div className="feature">✓ A/B testing support</div>
      </div>
      <span className="coming-soon">Coming Soon</span>
    </div>
  </div>
);

export default MailingPortal;
