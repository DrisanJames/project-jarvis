import React from 'react';
import { render, screen } from '@testing-library/react';
import { describe, it, expect, beforeEach, vi } from 'vitest';

vi.mock('../shared/apiFetch', () => ({ apiFetch: vi.fn() }));
import { apiFetch } from '../shared/apiFetch';

import { YesterdayPanel } from './YesterdayPanel';
import { installRoutes, resp, stubResizeObserver } from './testUtils';
import loads19 from './__fixtures__/loads-2026-09-19.json';
import loads20 from './__fixtures__/loads-2026-09-20.json';

/**
 * Board 3 against the REAL /loads bodies (2026-09-19 with the yahoo_family
 * inject load, 2026-09-20 empty). Pins:
 *  - totals are {landed, mailed, not_mailed, removed, duplicates} — the first
 *    build read mailed_since / not_yet_mailed and always printed words;
 *  - per-load cells ride the `loads` flag; composition rides `composition`;
 *  - an empty measured day says so; a `note` (degraded query) prints verbatim
 *    and the "stamping gap" hint never shows for it.
 */

describe('YesterdayPanel (real /loads bodies)', () => {
  beforeEach(() => {
    stubResizeObserver();
    installRoutes(apiFetch, (url) => {
      if (url.includes('/data-ingest/loads?date=2026-09-19')) return resp(200, loads19);
      if (url.includes('/data-ingest/loads?date=2026-09-20')) return resp(200, loads20);
      return undefined;
    });
  });

  it('opens on D-1 and renders the totals + the load from the Go shapes', async () => {
    render(<YesterdayPanel date="2026-09-20" />);
    expect(await screen.findByText('What arrived on 2026-09-19')).toBeInTheDocument();
    // landed 113,069 appears as the total, the load row and the (unstamped) source row
    expect((await screen.findAllByText('113,069')).length).toBe(3);
    expect(screen.getAllByText('102,306').length).toBe(2);   // not_mailed + load staged
    expect(screen.getAllByText('10,763').length).toBe(2);    // removed + load removed
    expect(screen.getByText('86,906')).toBeInTheDocument();  // yahoo composition
    expect(screen.getByText('Yahoo Family Ramp (site newsletters, 5 touches)')).toBeInTheDocument();
    expect(screen.queryByText('not yet measured')).not.toBeInTheDocument();
  });

  it('renders a measured empty day as empty, not as "not yet measured"', async () => {
    render(<YesterdayPanel date="2026-09-21" />);
    expect(await screen.findByText('What arrived on 2026-09-20')).toBeInTheDocument();
    expect(await screen.findByText('No loads recorded for this day')).toBeInTheDocument();
    expect(screen.queryByText('not yet measured')).not.toBeInTheDocument();
  });

  it('prints the API note verbatim and never the stamping-gap hint when the query degraded', async () => {
    const degraded = {
      ...loads20,
      note: 'loads_query_failed: canceling statement due to statement timeout',
      fields: Object.fromEntries(Object.keys(loads20.fields).map((k) => [k, 'not_measured'])),
      totals: { landed: null, mailed: null, not_mailed: null, removed: null, duplicates: null },
    };
    installRoutes(apiFetch, (url) => (url.includes('/data-ingest/loads?') ? resp(200, degraded) : undefined));
    render(<YesterdayPanel date="2026-09-21" />);
    // the amber banner AND the by-load empty-state hint both carry the note verbatim
    expect((await screen.findAllByText(/loads_query_failed: canceling statement/)).length).toBe(2);
    expect(screen.queryByText('No loads recorded for this day')).not.toBeInTheDocument();
    expect(screen.getAllByText('not yet measured').length).toBeGreaterThanOrEqual(5);
  });
});
