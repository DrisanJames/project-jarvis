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
