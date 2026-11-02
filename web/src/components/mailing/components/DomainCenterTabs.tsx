import React, { Suspense, lazy, useState } from 'react';
import { DomainCenter } from './DomainCenter';
import { SubNav } from '../shared/SubNav';

// Domain Center and Smart Links on one screen (operator 2026-11-02). The
// left-nav item opens here; 'smart-links' stays a valid deep link.
const SmartLinkManager = lazy(() => import('./SmartLinkManager').then(m => ({ default: m.SmartLinkManager })));

type Key = 'domains' | 'smart-links';
const TABS: { key: Key; label: string }[] = [
  { key: 'domains', label: 'Domains' },
  { key: 'smart-links', label: 'Smart Links' },
];

export const DomainCenterTabs: React.FC<{ initial?: Key }> = ({ initial = 'domains' }) => {
  const [tab, setTab] = useState<Key>(initial);
  return (
    <div>
      <div style={{ padding: '16px 24px 0' }}>
        <SubNav items={TABS} active={tab} onChange={(k) => setTab(k as Key)} ariaLabel="Domain Center" />
      </div>
      {tab === 'domains' ? <DomainCenter /> : <Suspense fallback={null}><SmartLinkManager /></Suspense>}
    </div>
  );
};
