// DataCenter — one screen for every record entering or leaving the platform:
// the ingest boards, the partner roster and its reservoir, and Export & Import.

import React, { Suspense, lazy, useState } from 'react';
import { SubNav } from '../shared/SubNav';
import { colors } from '../shared/theme';
import { Panel } from '../shared/ui';
import { DataIngestPortal, type View as IngestView } from '../dataingest/DataIngestPortal';
import { PartnerIngestPortal, type PartnerSection } from '../datapartners/PartnerIngestPortal';

const EOCleaning = lazy(() => import('../pages/EOCleaning').then(m => ({ default: m.EOCleaning })));

export type DataCenterTab = IngestView | PartnerSection | 'export';

const TABS: { key: DataCenterTab; label: string }[] = [
  { key: 'overview', label: 'Overview' },
  { key: 'feeds', label: 'Feeds' },
  { key: 'yesterday', label: 'Yesterday' },
  { key: 'upload', label: 'Upload' },
  { key: 'partners', label: 'Partners' },
  { key: 'reservoir', label: 'Reservoir' },
  { key: 'batches', label: 'Batches' },
  { key: 'activations', label: 'Partner Results' },
  { key: 'export', label: 'Export & Import' },
  { key: 'audit', label: 'Audit Log' },
];

const INGEST_TABS: readonly string[] = ['overview', 'feeds', 'yesterday', 'upload'];

const FLOW = [
  { title: 'Intake stays open', body: 'Partner API, CSV and SFTP files land every day. A sending pause never closes intake.' },
  { title: 'Validated', body: 'Records pass suppression and EmailOversight before they can mail.' },
  { title: 'Daily gate, 02:01 MT', body: 'Fresh records wait for the gate and go out once a day as one controlled newsletter send, sized by contract. Yahoo family and Internal Auto keep their own flows.' },
  { title: 'Newsletter first', body: 'New people get a newsletter. Offers wait until an open or click.' },
];

export const DataCenter: React.FC<{ initial?: DataCenterTab }> = ({ initial = 'overview' }) => {
  const [tab, setTab] = useState<DataCenterTab>(initial);

  return (
    <div style={{ padding: 24, color: colors.text }}>
      <SubNav items={TABS} active={tab} onChange={(k) => setTab(k as DataCenterTab)} ariaLabel="Data Center sections" />

      {tab === 'overview' && (
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))', gap: 12, marginBottom: 16 }}>
          {FLOW.map((f, i) => (
            <Panel key={f.title} style={{ padding: 14 }}>
              <div style={{ fontSize: 13, fontWeight: 700, color: colors.heading }}>{i + 1}. {f.title}</div>
              <div style={{ fontSize: 12, color: colors.textMuted, marginTop: 4 }}>{f.body}</div>
            </Panel>
          ))}
        </div>
      )}

      {INGEST_TABS.includes(tab) && <DataIngestPortal view={tab as IngestView} onViewChange={setTab} />}
      {tab === 'export' && <Suspense fallback={null}><EOCleaning /></Suspense>}
      {tab !== 'export' && !INGEST_TABS.includes(tab) && <PartnerIngestPortal section={tab as PartnerSection} />}
    </div>
  );
};

export default DataCenter;
