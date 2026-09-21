import React from 'react';
import { render, screen } from '@testing-library/react';
import { describe, it, expect, beforeEach, vi } from 'vitest';

vi.mock('../shared/apiFetch', () => ({ apiFetch: vi.fn() }));
import { apiFetch } from '../shared/apiFetch';

import { FeedPanel } from './FeedPanel';
import type { FeedRow, FeedsResponse } from './api';
import { installRoutes, resp, stubResizeObserver } from './testUtils';
import detailFx from './__fixtures__/feed-detail.json';
import feedsFx from './__fixtures__/feeds.json';

/**
 * Board 2 against the REAL /feeds/{id} body for Attribits (20c2983b…) and its
 * /feeds list row. Pins:
 *  - composition is {raw:[], staged:[]} (the first build called .reduce on it);
 *  - funnel flags are the FLAT names (cleaned / staged / mailed / engaged);
 *  - the header (partner → name, status, last loaded) comes from the list row
 *    while the detail body does not carry it;
 *  - a null `landed` renders "not yet measured".
 */

const DATASET = '20c2983b-49cb-4d27-a280-c67213dd22a1';
const listRow = (feedsFx as unknown as FeedsResponse).feeds.find((r) => r.dataset_id === DATASET) as FeedRow;

describe('FeedPanel (real /feeds/{id} body)', () => {
  beforeEach(() => {
    stubResizeObserver();
    installRoutes(apiFetch, (url) => {
      if (url.includes(`/data-ingest/feeds/${DATASET}?`)) return resp(200, detailFx);
      return undefined;
    });
  });

  it('renders the funnel and the staged composition from the Go shapes', async () => {
    render(<FeedPanel datasetId={DATASET} date="2026-09-20" listRow={listRow} onBack={() => {}} />);
    expect(await screen.findByText('387,323')).toBeInTheDocument();      // cleaned
    expect(screen.getAllByText('97,601').length).toBe(2);                // staged tile + gmail staged row
    expect(screen.getByText('289,696')).toBeInTheDocument();             // mailed
    expect(screen.getByText('gmail')).toBeInTheDocument();
    expect(screen.getAllByText('not yet measured').length).toBeGreaterThanOrEqual(1); // landed null
  });

  it('takes the header and switches from the /feeds list row when the detail body has none', async () => {
    render(<FeedPanel datasetId={DATASET} date="2026-09-20" listRow={listRow} onBack={() => {}} />);
    await screen.findByText('387,323');
    expect(screen.getByText('AARP Direct → Attribits')).toBeInTheDocument();
    expect(screen.getAllByText('paused').length).toBe(2);               // header pill + switch row, ingest_open=false
    expect(screen.getByText('present')).toBeInTheDocument();             // send_row=true
    expect(screen.getByText(/last loaded 2026-07-03T12:19:44Z/)).toBeInTheDocument();
  });

  it('prefers header fields the detail body carries over the list row', async () => {
    installRoutes(apiFetch, (url) => {
      if (url.includes(`/data-ingest/feeds/${DATASET}?`)) {
        return resp(200, { ...detailFx, name: 'Attribits v11', partner: 'Attribits', lane: 'auto', status: { ...listRow.status, ingest_open: true } });
      }
      return undefined;
    });
    render(<FeedPanel datasetId={DATASET} date="2026-09-20" listRow={listRow} onBack={() => {}} />);
    await screen.findByText('387,323');
    expect(screen.getByText('Attribits → Attribits v11')).toBeInTheDocument();
    expect(screen.queryByText('paused')).not.toBeInTheDocument();
    expect(screen.getByText('open')).toBeInTheDocument();
  });
});

// Intake/sending split (operator ruling, brain #3823): the panel renders the
// two dataset pauses as INDEPENDENT switches and drives them through their own
// endpoints — the sending stop (emergency-stop/resume) never touches intake,
// and the intake pause (intake-pause/intake-resume) never touches sending.
describe('FeedPanel intake vs sending switches', () => {
  beforeEach(() => {
    stubResizeObserver();
    vi.spyOn(window, 'confirm').mockReturnValue(true);
    vi.spyOn(window, 'prompt').mockReturnValue('test reason');
  });

  it('shows sending stopped + intake open for a dataset paused for sending only', async () => {
    installRoutes(apiFetch, (url) => {
      if (url.includes(`/data-ingest/feeds/${DATASET}?`)) {
        return resp(200, { ...detailFx, status: { ...listRow.status, ingest_open: true, sending_paused: true, intake_paused: false } });
      }
      return undefined;
    });
    render(<FeedPanel datasetId={DATASET} date="2026-09-20" listRow={listRow} onBack={() => {}} />);
    await screen.findByText('387,323');
    expect(screen.getByText('open')).toBeInTheDocument();        // intake row
    expect(screen.getByText('stopped')).toBeInTheDocument();     // sending row
    expect(screen.queryByText('paused')).not.toBeInTheDocument();
    expect(screen.getByText('Resume sending')).toBeInTheDocument();
    expect(screen.getByText('Pause intake')).toBeInTheDocument();
  });

  it('shows intake paused + sending live for a dataset paused for intake only', async () => {
    installRoutes(apiFetch, (url) => {
      if (url.includes(`/data-ingest/feeds/${DATASET}?`)) {
        return resp(200, { ...detailFx, status: { ...listRow.status, ingest_open: false, sending_paused: false, intake_paused: true } });
      }
      return undefined;
    });
    render(<FeedPanel datasetId={DATASET} date="2026-09-20" listRow={listRow} onBack={() => {}} />);
    await screen.findByText('387,323');
    expect(screen.getAllByText('paused').length).toBe(2);        // header pill + intake row
    expect(screen.getByText('sending')).toBeInTheDocument();     // sending row
    expect(screen.getByText('Resume intake')).toBeInTheDocument();
    expect(screen.getByText('Stop sending')).toBeInTheDocument();
  });

  it('drives the intake switch through /intake-pause and /intake-resume, never /emergency-stop', async () => {
    const seen = installRoutes(apiFetch, (url) => {
      if (url.includes(`/data-ingest/feeds/${DATASET}?`)) {
        return resp(200, { ...detailFx, status: { ...listRow.status, ingest_open: true, sending_paused: true, intake_paused: false } });
      }
      if (url.endsWith('/intake-pause') || url.endsWith('/intake-resume')) return resp(200, { ok: true });
      return undefined;
    });
    render(<FeedPanel datasetId={DATASET} date="2026-09-20" listRow={listRow} onBack={() => {}} />);
    await screen.findByText('387,323');
    screen.getByText('Pause intake').click();
    await screen.findByText('387,323');
    const posts = seen.filter((u) => u.includes('/data-partners/datasets/'));
    expect(posts.some((u) => u.endsWith(`/datasets/${DATASET}/intake-pause`))).toBe(true);
    expect(posts.some((u) => u.includes('emergency-stop'))).toBe(false);
  });
});
