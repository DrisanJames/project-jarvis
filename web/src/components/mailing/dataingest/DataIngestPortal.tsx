// DataIngestPortal — the `data-ingest` tab.
//
// Boards 1, 2, 3 and 5 of the ingest blueprint: Overview (two supply classes,
// day over day, time of day, the two rosters, composition), Feeds (one feed
// with its four real switches), Yesterday (one day's landings and whether they
// have mailed since) and Upload (the S3-first static door).
//
// Live-ness: the SSE stream (`useDataIngestStream`) moves numbers between
// polls; the 30s usePolling reads remain the source of truth, so the screen is
// correct with the stream off — it just updates slower.

import React, { useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faDatabase, faSync } from '@fortawesome/free-solid-svg-icons';
import { SubNav } from '../shared/SubNav';
import { usePolling } from '../shared/usePolling';
import { LivePill, PortalKeyframes, Panel, SectionHeader, SectionError, EmptyState, Pill } from '../shared/ui';
import { colors, pageStyle, btnStyle, tableStyle, thStyle, tdStyle } from '../shared/theme';
import { denverToday } from '../shared/filters';
import { useDataIngestStream } from '../../../hooks/useDataIngestStream';
import { OverviewPanel, FeedStatusPill } from './OverviewPanel';
import { FeedPanel } from './FeedPanel';
import { YesterdayPanel } from './YesterdayPanel';
import { UploadPanel } from './UploadPanel';
import { dataIngestApi, type FeedsResponse, type StateResponse } from './api';

const POLL_MS = 30_000;
const PAGE_VERSION = '1.0';

type View = 'overview' | 'feeds' | 'yesterday' | 'upload';

const VIEWS = [
  { key: 'overview', label: 'Overview' },
  { key: 'feeds', label: 'Feeds' },
  { key: 'yesterday', label: 'Yesterday' },
  { key: 'upload', label: 'Upload' },
];

export const DataIngestPortal: React.FC = () => {
  const [view, setView] = useState<View>('overview');
  const [date, setDate] = useState<string>(() => denverToday());
  const [selectedFeed, setSelectedFeed] = useState<string>('');

  const stream = useDataIngestStream();
  const feeds = usePolling<FeedsResponse>((signal) => dataIngestApi.feeds(signal), POLL_MS, []);
  const state = usePolling<StateResponse>((signal) => dataIngestApi.state(signal), POLL_MS, []);

  const openFeed = (datasetId: string) => {
    setSelectedFeed(datasetId);
    setView('feeds');
  };

  const refreshAll = () => {
    feeds.refresh();
    state.refresh();
  };

  const countersLive = state.data?.counters.live ?? null;
  const asOf = state.data?.as_of ?? null;

  const feedRoster = useMemo(() => (feeds.data?.feeds ?? []).slice().sort((a, b) => a.name.localeCompare(b.name)), [feeds.data]);

  return (
    <div style={pageStyle}>
      <PortalKeyframes />

      <div style={{ display: 'flex', alignItems: 'flex-start', gap: 14, flexWrap: 'wrap', marginBottom: 14 }}>
        <div style={{ maxWidth: 720 }}>
          <h2 style={{ margin: 0, color: colors.heading, display: 'flex', alignItems: 'center', gap: 10 }}>
            <FontAwesomeIcon icon={faDatabase} style={{ color: colors.indigo400 }} /> Data Ingest
          </h2>
          <div style={{ fontSize: 13, color: colors.textMuted, marginTop: 4 }}>
            Two supply classes, never mixed: what is LOADED and at rest (static files in S3, rows parked in the
            database) and what ARRIVES on its own clock (API feeds, site events, pulls). Internal transfers are
            shown and never counted as ingest. v{PAGE_VERSION}
          </div>
        </div>

        <div style={{ marginLeft: 'auto', display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
          <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'flex-end', gap: 2 }}>
            <LivePill live={stream.live || state.live} agoSeconds={state.secondsSinceUpdate} />
            <span style={{ fontSize: 10, color: colors.textFaint }}>
              {stream.live ? `stream · ${stream.received} delta${stream.received === 1 ? '' : 's'}` : 'stream offline · 30s polling'}
              {asOf ? ` · as of ${asOf}` : ''}
            </span>
          </div>
          {countersLive === false && <Pill color={colors.warning}>counters stalled</Pill>}
          <label style={{ fontSize: 11, color: colors.textMuted, display: 'flex', alignItems: 'center', gap: 6 }}>
            Denver day
            <input
              type="date"
              value={date}
              max={denverToday()}
              onChange={(e) => setDate(e.target.value || denverToday())}
              style={{
                background: 'rgba(15,23,42,0.6)', color: colors.text, border: `1px solid ${colors.panelBorder}`,
                borderRadius: 8, padding: '6px 10px', fontSize: 12, fontFamily: 'monospace',
              }}
            />
          </label>
          <button type="button" style={btnStyle} onClick={refreshAll}>
            <FontAwesomeIcon icon={faSync} spin={feeds.loading || state.loading} style={{ marginRight: 6 }} /> Refresh
          </button>
        </div>
      </div>

      <SubNav
        items={VIEWS}
        active={view}
        onChange={(k) => setView(k as View)}
        ariaLabel="Data ingest sections"
      />

      {state.error && !state.data && <SectionError label="Ingest state" error={state.error} onRetry={state.refresh} />}

      {view === 'overview' && (
        <OverviewPanel
          date={date}
          deltas={stream.deltas}
          feeds={feeds.data}
          feedsError={feeds.error}
          refreshFeeds={feeds.refresh}
          onOpenFeed={openFeed}
          onGoUpload={() => setView('upload')}
        />
      )}

      {view === 'feeds' && selectedFeed && (
        <FeedPanel datasetId={selectedFeed} date={date} onBack={() => setSelectedFeed('')} />
      )}

      {view === 'feeds' && !selectedFeed && (
        <Panel>
          <SectionHeader title="Pick a feed" icon={faDatabase} right={<span style={{ fontSize: 11, color: colors.textMuted }}>one row per partner × dataset → lane</span>} />
          {feeds.error && !feeds.data && <SectionError label="Feeds" error={feeds.error} onRetry={feeds.refresh} />}
          {feeds.data && feedRoster.length === 0 && <EmptyState title="No feeds" hint="No partner dataset is registered for this organization." />}
          {feedRoster.length > 0 && (
            <div style={{ overflowX: 'auto' }}>
              <table style={{ ...tableStyle, minWidth: 700 }}>
                <thead>
                  <tr>
                    <th style={thStyle}>Feed</th>
                    <th style={thStyle}>Lane</th>
                    <th style={thStyle}>Class</th>
                    <th style={thStyle}>Status</th>
                    <th style={thStyle}>Last event</th>
                    <th style={thStyle} />
                  </tr>
                </thead>
                <tbody>
                  {feedRoster.map((r) => (
                    <tr key={r.dataset_id}>
                      <td style={tdStyle}>{r.name}</td>
                      <td style={{ ...tdStyle, color: colors.textMuted }}>{r.lane || '—'}</td>
                      <td style={tdStyle}>{r.supply_class}</td>
                      <td style={tdStyle}><FeedStatusPill row={r} /></td>
                      <td style={{ ...tdStyle, color: colors.textMuted, fontSize: 11 }}>{r.last_event ?? '—'}</td>
                      <td style={tdStyle}>
                        <button type="button" style={{ ...btnStyle, padding: '3px 10px', fontSize: 11 }} onClick={() => openFeed(r.dataset_id)}>Open</button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>
      )}

      {view === 'yesterday' && <YesterdayPanel date={date} />}

      {view === 'upload' && (
        <UploadPanel feeds={feedRoster} feedsError={feeds.error} onOpenFeed={openFeed} />
      )}
    </div>
  );
};

export default DataIngestPortal;
