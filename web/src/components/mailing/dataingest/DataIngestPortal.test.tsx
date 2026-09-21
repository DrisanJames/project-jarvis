import React from 'react';
import { render, screen, fireEvent } from '@testing-library/react';
import { describe, it, expect, beforeEach, vi } from 'vitest';

vi.mock('../shared/apiFetch', () => ({ apiFetch: vi.fn() }));
import { apiFetch } from '../shared/apiFetch';

import { DataIngestPortal } from './DataIngestPortal';
import { installRoutes, resp, stubResizeObserver } from './testUtils';
import dayFx from './__fixtures__/day.json';
import hoursFx from './__fixtures__/hours.json';
import feedsFx from './__fixtures__/feeds.json';
import stateFx from './__fixtures__/state.json';

/**
 * The tab shell against the REAL /state and /feeds bodies. Pins:
 *  - the counters pill reads running / redis_available / measured_today
 *    (the first build read a `live` field Go never sends, so the stall pill
 *    was dead code);
 *  - cache_age_seconds + query_ms are shown;
 *  - the feed roster lists all 62 feeds, with '' timestamps shown as absent.
 */

describe('DataIngestPortal (real /state + /feeds bodies)', () => {
  beforeEach(() => {
    stubResizeObserver();
    installRoutes(apiFetch, (url) => {
      if (url.endsWith('/data-ingest/state')) return resp(200, stateFx);
      if (url.endsWith('/data-ingest/feeds')) return resp(200, feedsFx);
      if (url.includes('/data-ingest/day?')) return resp(200, dayFx);
      if (url.includes('/data-ingest/hours?')) return resp(200, hoursFx);
      return undefined;
    });
  });

  it('shows the counters consumer state from the Go fields and the snapshot cost', async () => {
    render(<DataIngestPortal />);
    expect(await screen.findByText('counters running')).toBeInTheDocument();
    expect(screen.getByText(/no events today · never/)).toBeInTheDocument(); // measured_today=false, last_handled_at=''
    expect(screen.queryByText('counters stalled')).not.toBeInTheDocument();
    // /state and /feeds share the reservoir snapshot (query_ms 10526, cache 378s): header + both feed panes
    expect(screen.getAllByText(/query 10,526 ms/).length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText(/snapshot 6m old/).length).toBeGreaterThanOrEqual(1);          // cache_age_seconds 378
  });

  it('lists all 62 feeds on the Feeds view with absent timestamps shown as absent', async () => {
    render(<DataIngestPortal />);
    await screen.findByText('counters running');
    fireEvent.click(screen.getByRole('tab', { name: 'Feeds' }));
    expect(await screen.findByText(/62 feeds/)).toBeInTheDocument();
    expect(screen.getAllByText('Attribits').length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText('never').length).toBeGreaterThan(0);  // last_loaded ''
    expect(screen.getAllByText('—').length).toBeGreaterThanOrEqual(62); // last_event '' on every row
  });
});
