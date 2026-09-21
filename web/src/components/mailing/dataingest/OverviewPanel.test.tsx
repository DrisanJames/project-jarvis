import React from 'react';
import { render, screen, waitFor } from '@testing-library/react';
import { describe, it, expect, beforeEach, vi } from 'vitest';

vi.mock('../shared/apiFetch', () => ({ apiFetch: vi.fn() }));
import { apiFetch } from '../shared/apiFetch';

import { OverviewPanel } from './OverviewPanel';
import type { FeedsResponse } from './api';
import { installRoutes, resp, stubResizeObserver } from './testUtils';
import dayFx from './__fixtures__/day.json';
import hoursFx from './__fixtures__/hours.json';
import feedsFx from './__fixtures__/feeds.json';

/**
 * Board 1 against the REAL /day, /hours and /feeds bodies (prod fc03054,
 * 2026-09-21). Pins the contract defects found in the 2026-09-20 review:
 *  - /day carries at_rest / dynamic / internal_transfer at TOP level (the
 *    first build read `classes.*` and crashed on the first poll);
 *  - flags are FLAT Go names (`parked_in_db`, not `at_rest.parked_in_db`), so
 *    the reservoir numbers render as numbers;
 *  - a null counter field renders "not yet measured", never 0;
 *  - the delta buffer is cleared once the poll has landed (no double count);
 *  - a feed with no supply class is listed as unclassed, not hidden.
 */

describe('OverviewPanel (real /day body)', () => {
  beforeEach(() => {
    stubResizeObserver();
    installRoutes(apiFetch, (url) => {
      if (url.includes('/data-ingest/day?')) return resp(200, dayFx);
      if (url.includes('/data-ingest/hours?')) return resp(200, hoursFx);
      return undefined;
    });
  });

  const renderPanel = (over: Partial<React.ComponentProps<typeof OverviewPanel>> = {}) => {
    const clearDeltas = vi.fn();
    const utils = render(
      <OverviewPanel
        date="2026-09-20"
        deltas={[]}
        clearDeltas={clearDeltas}
        feeds={feedsFx as unknown as FeedsResponse}
        feedsError={null}
        refreshFeeds={() => {}}
        onOpenFeed={() => {}}
        onGoUpload={() => {}}
        {...over}
      />,
    );
    return { ...utils, clearDeltas };
  };

  it('renders the reservoir tiles from the top-level at_rest block', async () => {
    renderPanel();
    // parked_in_db and raw are both 4,467,634 (held) — two cells, never a crash
    expect((await screen.findAllByText('4,467,634')).length).toBe(2);
    expect(screen.getByText('1,959,346')).toBeInTheDocument();   // staged
    expect(screen.getByText('11,165,307')).toBeInTheDocument();  // static_objects
    expect(screen.getByText('1,411,828')).toBeInTheDocument();   // inflight
    expect(screen.getByText('6,292,458')).toBeInTheDocument();   // mailed lifetime
    expect(screen.getByText('992,029')).toBeInTheDocument();     // removed
  });

  it('renders null counter fields as "not yet measured", never 0', async () => {
    renderPanel();
    await screen.findAllByText('4,467,634');
    // arrived / yesterday / arrival_rate / feeds_live / landed / n are all null + not_measured
    expect(screen.getAllByText('not yet measured').length).toBeGreaterThanOrEqual(6);
    // the null tiles must not have turned into a zero (the feed tables below
    // legitimately hold measured zeros for idle datasets)
    for (const label of ['Arrived today', 'Yesterday', 'Arrival rate', 'Feeds live']) {
      const tile = screen.getByText(label).parentElement as HTMLElement;
      expect(tile.textContent).toContain('not yet measured');
      expect(tile.textContent).not.toMatch(/\b0\b/);
    }
  });

  it('clears the SSE delta buffer once the poll has landed', async () => {
    const { clearDeltas } = renderPanel();
    await screen.findAllByText('4,467,634');
    await waitFor(() => expect(clearDeltas).toHaveBeenCalled());
  });

  it('lists feeds without a supply class as unclassed rather than hiding them', async () => {
    renderPanel();
    await screen.findAllByText('4,467,634');
    expect(screen.getByText(/Unclassed · supply_class not stamped/)).toBeInTheDocument();
    expect(screen.getByText('Attribits')).toBeInTheDocument();
    expect(screen.getByText('419,847')).toBeInTheDocument(); // Attribits records (flag `records` = measured)
  });
});
