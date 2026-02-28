import { render, screen, waitFor } from '@testing-library/react';
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { DataInjectionsDashboard } from './DataInjectionsDashboard';
import { DateFilterProvider } from '../../context/DateFilterContext';
import type { DataInjectionsDashboard as DashboardData } from './types';

const renderWithProvider = (ui: React.ReactElement) =>
  render(<DateFilterProvider>{ui}</DateFilterProvider>);

// Mock fetch
const mockDashboard: DashboardData = {
  timestamp: new Date().toISOString(),
  overall_health: 'healthy',
  health_issues: [],
  ingestion: {
    status: 'healthy',
    total_records: 100000,
    today_records: 5000,
    data_sets_active: 3,
    data_sets_with_gaps: 0,
    data_sets: [
      {
        data_set_code: 'GLB_BR',
        data_partner: 'GLOBE USA',
        data_set_name: 'GLOBE_USA_BEDROCK',
        record_count: 50000,
        today_count: 2500,
        last_timestamp: new Date().toISOString(),
        has_gap: false,
        gap_hours: 0.5,
      },
    ],
    daily_counts: [],
    last_fetch: new Date().toISOString(),
  },
  validation: {
    status: 'healthy',
    total_records: 80000,
    today_records: 4000,
    unique_statuses: 5,
    status_breakdown: [
      { status_id: 'valid', count: 70000 },
      { status_id: 'invalid', count: 10000 },
    ],
    daily_metrics: [],
    domain_breakdown: [
      { domain_group: 'Gmail', domain_group_short: 'GMAL', count: 40000 },
    ],
    last_fetch: new Date().toISOString(),
  },
  import: {
    status: 'healthy',
    total_imports: 25,
    today_imports: 5,
    total_records: 50000,
    success_records: 48000,
    failed_records: 500,
    duplicate_records: 1500,
    in_progress: 0,
    completed: 25,
    recent_imports: [],
    daily_metrics: [],
    last_fetch: new Date().toISOString(),
  },
};

describe('DataInjectionsDashboard', () => {
  beforeEach(() => {
    global.fetch = vi.fn();
  });

  afterEach(() => {
    vi.resetAllMocks();
  });

  it('renders loading state initially', () => {
    vi.mocked(global.fetch).mockImplementation(() => 
      new Promise(resolve => setTimeout(() => resolve({
        ok: true,
        json: () => Promise.resolve(mockDashboard),
      } as Response), 1000))
    );

    renderWithProvider(<DataInjectionsDashboard autoRefresh={false} />);
    expect(screen.getByText(/Loading Data Injections Dashboard/i)).toBeInTheDocument();
  });

  it('renders dashboard after data loads', async () => {
    vi.mocked(global.fetch).mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve(mockDashboard),
    } as Response);

    renderWithProvider(<DataInjectionsDashboard autoRefresh={false} />);
    
    await waitFor(() => {
      expect(screen.getByText(/Data Injections Pipeline/i)).toBeInTheDocument();
    });
  });

  it('displays health status', async () => {
    vi.mocked(global.fetch).mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve(mockDashboard),
    } as Response);

    renderWithProvider(<DataInjectionsDashboard autoRefresh={false} />);
    
    await waitFor(() => {
      // Check for pipeline text and healthy status operating text
      expect(screen.getByText(/All data injection pipelines are operating normally/i)).toBeInTheDocument();
    });
  });

  it('renders all three panels', async () => {
    vi.mocked(global.fetch).mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve(mockDashboard),
    } as Response);

    renderWithProvider(<DataInjectionsDashboard autoRefresh={false} />);
    
    await waitFor(() => {
      expect(screen.getByText(/Partner Data Ingestion/i)).toBeInTheDocument();
      expect(screen.getByText(/Email Validation/i)).toBeInTheDocument();
      expect(screen.getByText(/Data Imports/i)).toBeInTheDocument();
    });
  });

  it('handles API error gracefully', async () => {
    vi.mocked(global.fetch).mockRejectedValueOnce(new Error('Network error'));

    renderWithProvider(<DataInjectionsDashboard autoRefresh={false} />);
    
    await waitFor(() => {
      expect(screen.getByText(/Unable to Load Data Injections/i)).toBeInTheDocument();
    });
  });

  it('displays warning status with issues', async () => {
    const warningDashboard = {
      ...mockDashboard,
      overall_health: 'warning' as const,
      health_issues: ['Warning: Some data partner feeds have gaps'],
    };

    vi.mocked(global.fetch).mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve(warningDashboard),
    } as Response);

    renderWithProvider(<DataInjectionsDashboard autoRefresh={false} />);
    
    await waitFor(() => {
      // Look for the specific issue text rather than just "Warning"
      expect(screen.getByText(/Some data partner feeds have gaps/i)).toBeInTheDocument();
    });
  });
});
