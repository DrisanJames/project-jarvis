import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { apiFetch } from '../shared/apiFetch';
import { EventLakeExplorer } from './EventLakeExplorer';

vi.mock('../shared/apiFetch', () => ({ apiFetch: vi.fn() }));
vi.mock('../offeralignment/OfferAlignmentTab', () => ({ default: () => null }));
vi.mock('../growth/GrowthTab', () => ({ default: () => null }));

const CUBE = {
  columns: ['local_dt', 'lane', 'isp', 'brand', 'event_type', 'count'],
  dict: {
    local_dt: ['2026-10-30'],
    lane: ['ses', 'pmta_a', ''],
    isp: ['gmail', 'yahoo'],
    brand: ['discountblog.com', 'em.discountblog.com'],
    event_type: ['delivered', 'open'],
  },
  rows: [
    [0, 0, 0, 0, 0, 1000],
    [0, 1, 1, 1, 0, 200],
    [0, 0, 0, 1, 1, 70],
    [0, 2, 1, 1, 1, 5],
  ],
};

const DIM_CUBE = {
  columns: ['value', 'lane', 'isp', 'event_type', 'count'],
  dict: { value: ['hotmail.com', 'outlook.com'], lane: ['ses'], isp: ['microsoft'], event_type: ['delivered'] },
  rows: [[0, 0, 0, 0, 30], [1, 0, 0, 0, 12]],
};

const respond = (body: unknown) => ({ ok: true, json: async () => body }) as Response;
const calls = (part: string) => vi.mocked(apiFetch).mock.calls.filter(([url]) => String(url).includes(part)).length;

async function renderLoaded() {
  render(<EventLakeExplorer />);
  await screen.findAllByText('Delivered');
}

const tile = (label: string) => screen.getAllByText(label)[0].parentElement as HTMLElement;

beforeEach(() => {
  vi.stubGlobal('ResizeObserver', class { observe() {} unobserve() {} disconnect() {} });
  vi.mocked(apiFetch).mockImplementation(async (url) => {
    const u = String(url);
    if (u.includes('/lake/status')) return respond({ enabled_read: true });
    if (u.includes('/lake/cube')) return respond(u.includes('dim=') ? DIM_CUBE : CUBE);
    if (u.includes('/deferral-funnel')) return respond({ total: { deferred: 0, recovered: 0, pending: 0, bounced: 0 }, rows: [] });
    throw new Error(`unexpected fetch ${u}`);
  });
});

describe('Reporting filters', () => {
  it('shows opens from the cube and reads one cube for the page', async () => {
    await renderLoaded();
    expect(tile('Delivered')).toHaveTextContent('1,200');
    expect(tile('Opens')).toHaveTextContent('75');
    expect(calls('/lake/cube')).toBe(1);
  });

  it('Transport slices in the browser, with a spinner and no fetch', async () => {
    await renderLoaded();
    const before = vi.mocked(apiFetch).mock.calls.length;
    fireEvent.click(screen.getByRole('button', { name: 'SES' }));
    expect(await screen.findByRole('status')).toHaveTextContent('Updating');
    expect(tile('Opens')).toHaveTextContent('70');
    expect(vi.mocked(apiFetch).mock.calls.length).toBe(before);
  });

  it('Provider and Brand slice with no fetch', async () => {
    await renderLoaded();
    const before = vi.mocked(apiFetch).mock.calls.length;
    fireEvent.change(screen.getByLabelText('Provider'), { target: { value: 'gmail' } });
    fireEvent.change(screen.getByLabelText('Brand'), { target: { value: 'discountblog.com' } });
    fireEvent.click(screen.getByRole('button', { name: /Run/ }));
    await waitFor(() => expect(tile('Delivered')).toHaveTextContent('1,000'));
    expect(vi.mocked(apiFetch).mock.calls.length).toBe(before);
  });

  it('Refresh refetches', async () => {
    await renderLoaded();
    fireEvent.click(screen.getByRole('button', { name: /Refresh/ }));
    await waitFor(() => expect(calls('refresh=1')).toBe(1));
  });
});

describe('Dimensions', () => {
  async function openDimensions() {
    await renderLoaded();
    fireEvent.click(screen.getByRole('button', { name: 'Dimensions' }));
    await screen.findByRole('button', { name: 'Sending Route' });
  }

  it('has no Source or Variant rows', async () => {
    await openDimensions();
    expect(screen.queryByRole('button', { name: 'Source' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Variant' })).toBeNull();
  });

  it('Rows on the main cube make no fetch and use the route names', async () => {
    await openDimensions();
    const before = vi.mocked(apiFetch).mock.calls.length;
    fireEvent.click(screen.getByRole('button', { name: 'Sending Route' }));
    const table = await screen.findByRole('table');
    expect(within(table).getByText('SES')).toBeTruthy();
    expect(within(table).getByText('PMTA A')).toBeTruthy();
    expect(vi.mocked(apiFetch).mock.calls.length).toBe(before);
  });

  it('groups brand by root domain and opens to sub domains', async () => {
    await openDimensions();
    fireEvent.click(screen.getByRole('button', { name: 'Brand' }));
    const cell = await screen.findByRole('cell', { name: 'discountblog.com' });
    expect(screen.queryByText('em.discountblog.com')).toBeNull();
    fireEvent.click(cell);
    expect(await screen.findByText('em.discountblog.com')).toBeTruthy();
  });

  it('Email Domain is one cached trip, grouped then expandable', async () => {
    await openDimensions();
    fireEvent.click(screen.getByRole('button', { name: 'Email Domain' }));
    const group = await screen.findByRole('cell', { name: 'Microsoft' });
    expect(calls('dim=email_domain')).toBe(1);
    fireEvent.click(group);
    expect(await screen.findByText('hotmail.com')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Brand' }));
    fireEvent.click(screen.getByRole('button', { name: 'Email Domain' }));
    await screen.findByRole('cell', { name: 'Microsoft' });
    expect(calls('dim=email_domain')).toBe(1);
  });
});
