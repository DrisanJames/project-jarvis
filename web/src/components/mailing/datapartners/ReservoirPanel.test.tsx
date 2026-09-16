import React from 'react';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { describe, it, expect, beforeEach, vi } from 'vitest';

import { ReservoirPanel } from './ReservoirPanel';

/**
 * What these PIN (all three were real defects or real prod behaviour):
 *  - the totals render from the BACKEND's own field names, so a rename on the
 *    Go side fails here instead of silently rendering blanks;
 *  - a failed reservoir fetch renders an explicit error and NEVER zeros — a
 *    zero-looking reservoir reads as "we hold nothing";
 *  - a failed dataset roster is surfaced in the upload shelf. On 2026-09-16
 *    /data-partners/datasets was answering 500 in prod, and the first version
 *    of this panel swallowed that, showing an empty picker with no reason.
 */

const RESERVOIR = {
  generated_at: '2026-09-16T12:00:00Z',
  organization_id: '00000000-0000-0000-0000-000000000001',
  cached: false,
  age_seconds: 0,
  query_ms: 8822,
  total: 2196089,
  mailable_total: 484725,
  by_status: [
    { status: 'held', count: 1271065 },
    { status: 'mailed', count: 439792 },
    { status: 'ready', count: 484725 },
  ],
  by_vertical: [
    {
      vertical: 'refi_heloc',
      total: 2195582,
      by_status: { ready: 484725, held: 1271065, mailed: 439792 },
      by_isp: { gmail: 374377, microsoft: 550140, yahoo: 1271065 },
      mailable: 484725,
      reserve: 1271065,
      exhausted: 439792,
    },
  ],
  cells: [],
};

const resp = (status: number, body: unknown) => ({
  ok: status >= 200 && status < 300,
  status,
  json: () => Promise.resolve(body),
});

/** Route each call by URL; unmatched URLs fail loudly rather than hang. */
function routeFetch(handlers: Record<string, () => unknown>) {
  (global.fetch as unknown as ReturnType<typeof vi.fn>).mockImplementation((input: unknown) => {
    const url = String(input);
    for (const [frag, make] of Object.entries(handlers)) {
      if (url.includes(frag)) return Promise.resolve(make());
    }
    return Promise.reject(new Error(`unexpected fetch: ${url}`));
  });
}

describe('ReservoirPanel', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders the totals the backend reports', async () => {
    routeFetch({
      '/reservoir': () => resp(200, RESERVOIR),
      '/datasets': () => resp(200, { datasets: [] }),
    });

    render(<ReservoirPanel />);

    expect(await screen.findByText('2,196,089')).toBeInTheDocument(); // total
    // 484,725 is the headline mailable number, the vertical row's mailable
    // cell AND the 'ready' status card — the screen repeats it on purpose, so
    // assert presence rather than uniqueness.
    expect(screen.getAllByText('484,725').length).toBeGreaterThan(0);
    expect(screen.getAllByText('1,271,065').length).toBeGreaterThan(0);
    expect(screen.getByText('Mailable now')).toBeInTheDocument();
    // 'Reserve' is both the headline label and the table column header.
    expect(screen.getAllByText('Reserve').length).toBeGreaterThan(0);
  });

  it('shows an explicit error and no zeros when the reservoir read fails', async () => {
    routeFetch({
      '/reservoir': () => resp(500, { error: 'reservoir_query_failed' }),
      '/datasets': () => resp(200, { datasets: [] }),
    });

    render(<ReservoirPanel />);

    expect(await screen.findByText(/Reservoir totals unavailable/)).toBeInTheDocument();
    expect(screen.getByText(/Nothing below is a real zero/)).toBeInTheDocument();
    expect(screen.queryByText('Mailable now')).not.toBeInTheDocument();
  });

  it('surfaces a failed dataset roster in the upload shelf instead of an empty picker', async () => {
    routeFetch({
      '/reservoir': () => resp(200, RESERVOIR),
      '/datasets': () => resp(500, { error: 'list_datasets_failed: pq: canceling statement due to statement timeout' }),
    });

    render(<ReservoirPanel />);
    await screen.findByText('2,196,089');

    fireEvent.click(screen.getByRole('button', { name: /Upload a file/ }));

    await waitFor(() => {
      expect(screen.getByText(/dataset list could not be loaded/)).toBeInTheDocument();
    });
    expect(screen.getByText(/list_datasets_failed/)).toBeInTheDocument();
  });

  it('expands a vertical to its ISP split', async () => {
    routeFetch({
      '/reservoir': () => resp(200, RESERVOIR),
      '/datasets': () => resp(200, { datasets: [] }),
    });

    render(<ReservoirPanel />);
    await screen.findByText('2,196,089');

    fireEvent.click(screen.getByRole('button', { name: 'Show' }));

    expect(await screen.findByText('374,377')).toBeInTheDocument(); // gmail
    expect(screen.getByText('550,140')).toBeInTheDocument();        // microsoft
  });
});
