// Regression guard for the 2026-08-21 operator defect on the Segment
// Freshness board:
//
//   "AAD 30d openers: 1 queued. Then I wait, no progress… I tap x on the
//    notification and the screen seems like it is not doing anything."
//
// The backend was healthy the whole time (queued→running in <1s, a genuine
// 3–7 minute rebuild). The defect was that the UI showed no lifecycle and
// buried the only signal in a dismissible receipt. These tests pin the
// BEHAVIOR, not the markup:
//   1. a queued refresh renders QUEUED on the cell itself,
//   2. dismissing the receipt does NOT clear the in-flight state,
//   3. the server's running state renders with an elapsed timer,
//   4. completion renders DONE with the rebuilt member count, and the card
//      picks up the new count/freshness with no manual reload.

import React from 'react';
import { render, screen, act, fireEvent } from '@testing-library/react';
import { SegmentFreshnessBoard } from './SegmentFreshnessBoard';

vi.mock('../shared/apiFetch', () => ({ apiFetch: vi.fn() }));
import { apiFetch } from '../shared/apiFetch';

const NOW = new Date('2026-08-21T12:00:00Z');

type RefreshState = null | 'queued' | 'running';

interface Row {
  segment_id: string;
  name: string;
  brand: string;
  window_days: number;
  kind: 'openers' | 'clickers';
  status: string;
  member_count: number | null;
  members_stamped_at: string | null;
  last_built_at: string | null;
  build_source: string;
  last_build_status: string;
  last_error: string;
  freshness: 'fresh' | 'aging' | 'stale' | 'unknown';
  refresh_state: RefreshState;
}

const SEG = '11111111-1111-1111-1111-111111111111';

const baseRow = (over: Partial<Row> = {}): Row => ({
  segment_id: SEG,
  name: 'AAD 30D Openers',
  brand: 'AAD',
  window_days: 30,
  kind: 'openers',
  status: 'active',
  member_count: 1200,
  members_stamped_at: '2026-08-18T12:00:00Z',
  last_built_at: '2026-08-18T12:00:00Z', // 3 days old -> stale
  build_source: 'grid_worker',
  last_build_status: 'ok',
  last_error: '',
  freshness: 'stale',
  refresh_state: null,
  ...over,
});

let currentRows: Row[] = [];

const payload = () => ({
  generated_at: NOW.toISOString(),
  worker: { running: true, last_pass_at: NOW.toISOString(), last_pass_outcome: 'ok', degraded: false, leader: true },
  rows: currentRows,
});

const jsonRes = (status: number, body: unknown) => ({
  ok: status >= 200 && status < 300,
  status,
  json: async () => body,
}) as unknown as Response;

describe('SegmentFreshnessBoard — refresh lifecycle', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(NOW);
    currentRows = [baseRow()];
    (apiFetch as unknown as ReturnType<typeof vi.fn>).mockImplementation(
      (url: string, init?: RequestInit) => {
        if (init?.method === 'POST') return Promise.resolve(jsonRes(202, { queued: [SEG], already: [] }));
        return Promise.resolve(jsonRes(200, payload()));
      },
    );
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  const settle = async (ms = 0) => {
    await act(async () => { await vi.advanceTimersByTimeAsync(ms); });
  };

  it('shows queued → running → done ON THE CARD, and a dismissed receipt never clears it', async () => {
    render(<SegmentFreshnessBoard />);
    await settle();

    // The domain defaults to the only brand present.
    expect(screen.getByText(/AAD — em\.aadwd\.com/)).toBeInTheDocument();
    // The count appears on the card AND on the domain KPI tile.
    expect(screen.getAllByText('1,200').length).toBeGreaterThan(0);

    // 1. Queue a rebuild from the card.
    const btn = screen.getByTitle(/Queue a membership rebuild for AAD 30D Openers/);
    await act(async () => { fireEvent.click(btn); });
    await settle();

    expect(screen.getByText('queued')).toBeInTheDocument();
    // The receipt sets the 3–7 minute expectation.
    expect(screen.getByText(/A full membership rebuild takes 3–7 minutes/)).toBeInTheDocument();

    // 2. Dismissing the receipt must NOT cancel or hide the in-flight state.
    await act(async () => { fireEvent.click(screen.getByTitle(/Dismiss this receipt/)); });
    expect(screen.queryByText(/A full membership rebuild takes 3–7 minutes/)).not.toBeInTheDocument();
    expect(screen.getByText('queued')).toBeInTheDocument();
    expect(screen.getAllByText(/rebuilds? in flight/).length).toBeGreaterThan(0);

    // 3. Server flips to running — the card follows, with an elapsed timer.
    currentRows = [baseRow({ refresh_state: 'running' })];
    await settle(11_000);
    expect(screen.getByText('running')).toBeInTheDocument();
    expect(screen.getByText('11s')).toBeInTheDocument();

    // 4. Build completes: request closes, ledger advances, count changes.
    currentRows = [baseRow({
      refresh_state: null,
      member_count: 22840,
      last_built_at: '2026-08-21T12:03:00Z',
      members_stamped_at: '2026-08-21T12:03:00Z',
      freshness: 'fresh',
    })];
    await settle(11_000);
    expect(screen.getByText('done')).toBeInTheDocument();
    expect(screen.getByText('rebuilt: 22,840 members')).toBeInTheDocument();
    // The card itself refreshed — no manual reload.
    expect(screen.getAllByText('22,840').length).toBeGreaterThan(0);
    expect(screen.getByText('fresh')).toBeInTheDocument();
  });

  it('adopts a rebuild already running on the server without claiming a false elapsed time', async () => {
    currentRows = [baseRow({ refresh_state: 'running' })];
    render(<SegmentFreshnessBoard />);
    await settle();

    expect(screen.getByText('running')).toBeInTheDocument();
    expect(screen.getByText('elapsed unknown')).toBeInTheDocument();
  });

  it('never renders a null member_count as zero', async () => {
    currentRows = [baseRow({
      member_count: null,
      last_built_at: null,
      members_stamped_at: null,
      freshness: 'unknown',
    })];
    render(<SegmentFreshnessBoard />);
    await settle();

    expect(screen.getAllByText('?').length).toBeGreaterThan(0);
    expect(screen.getByText('NEVER BUILT')).toBeInTheDocument();
    // Absence is never rendered as a zero — the estate roll-up shows '—' with
    // an explicit reason instead of summing nothing to 0.
    expect(screen.getByText(/no verified 30D opener build in the estate/)).toBeInTheDocument();
    expect(screen.getByText(/no verified 30D clicker build in the estate/)).toBeInTheDocument();
  });
});
