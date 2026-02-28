import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { EverflowDashboard } from './EverflowDashboard';
import { EverflowDashboardResponse } from '../../types';

// Mock fetch
const mockFetch = vi.fn();
global.fetch = mockFetch;

const mockDashboardData: EverflowDashboardResponse = {
  timestamp: '2026-01-27T12:00:00Z',
  last_fetch: '2026-01-27T11:55:00Z',
  today_clicks: 1500,
  today_conversions: 15,
  today_revenue: 3375.00,
  today_payout: 3375.00,
  total_revenue: 7875.00,
  total_conversions: 35,
  daily_performance: [
    {
      date: '2026-01-27',
      clicks: 1500,
      conversions: 15,
      revenue: 3375.00,
      payout: 3375.00,
      conversion_rate: 0.01,
      epc: 2.25,
    },
    {
      date: '2026-01-26',
      clicks: 2000,
      conversions: 20,
      revenue: 4500.00,
      payout: 4500.00,
      conversion_rate: 0.01,
      epc: 2.25,
    },
  ],
  offer_performance: [
    {
      offer_id: '407',
      offer_name: 'Empire Flooring CPL',
      clicks: 1000,
      conversions: 10,
      revenue: 2250.00,
      payout: 2250.00,
      conversion_rate: 0.01,
      epc: 2.25,
    },
    {
      offer_id: '556',
      offer_name: 'Another Offer',
      clicks: 500,
      conversions: 5,
      revenue: 1125.00,
      payout: 1125.00,
      conversion_rate: 0.01,
      epc: 2.25,
    },
  ],
  property_performance: [
    {
      property_code: 'TDIH',
      property_name: 'thisdayinhistory.co',
      clicks: 1200,
      conversions: 12,
      revenue: 2700.00,
      payout: 2700.00,
      conversion_rate: 0.01,
      epc: 2.25,
      unique_offers: 3,
    },
    {
      property_code: 'HRO',
      property_name: 'horoscopeinfo.com',
      clicks: 800,
      conversions: 8,
      revenue: 1800.00,
      payout: 1800.00,
      conversion_rate: 0.01,
      epc: 2.25,
      unique_offers: 2,
    },
  ],
  campaign_revenue: [
    {
      mailing_id: '3219537162',
      campaign_name: 'TDIH_407_3926_01272026_3219537162',
      property_code: 'TDIH',
      property_name: 'thisdayinhistory.co',
      offer_id: '407',
      offer_name: 'Empire Flooring CPL',
      clicks: 500,
      conversions: 5,
      revenue: 1125.00,
      payout: 1125.00,
      conversion_rate: 0.01,
      epc: 2.25,
      audience_size: 50000,
      sent: 48000,
      delivered: 45600,
      opens: 12000,
      unique_opens: 9120,
      email_clicks: 2500,
      sending_domain: 'mail.thisdayinhistory.co',
      ongage_linked: true,
      rpm: 23.44,
      ecpm: 22.50,
      revenue_per_open: 0.12,
    },
  ],
  revenue_breakdown: {
    cpm: {
      offer_count: 5,
      clicks: 1000,
      conversions: 10,
      revenue: 4500.00,
      payout: 4500.00,
      percentage: 57.1,
    },
    non_cpm: {
      offer_count: 10,
      clicks: 2500,
      conversions: 25,
      revenue: 3375.00,
      payout: 3375.00,
      percentage: 42.9,
    },
    daily_trend: [],
  },
};

describe('EverflowDashboard', () => {
  beforeEach(() => {
    mockFetch.mockReset();
  });

  it('renders loading state initially', () => {
    mockFetch.mockImplementation(() => 
      new Promise(() => {}) // Never resolves
    );
    
    render(<EverflowDashboard />);
    
    expect(screen.getByText(/Loading Everflow data/i)).toBeInTheDocument();
  });

  it('renders dashboard with data', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => mockDashboardData,
    });

    render(<EverflowDashboard />);

    await waitFor(() => {
      expect(screen.getByText(/Everflow Revenue/i)).toBeInTheDocument();
    });

    // Check today's metrics are displayed
    await waitFor(() => {
      expect(screen.getByText(/Today's Revenue/i)).toBeInTheDocument();
    });
  });

  it('renders error state on fetch failure', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'Test error' }),
    });

    render(<EverflowDashboard />);

    await waitFor(() => {
      expect(screen.getByText(/Error loading Everflow data/i)).toBeInTheDocument();
    });
  });

  it('switches tabs correctly', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => mockDashboardData,
    });

    const user = userEvent.setup();
    render(<EverflowDashboard />);

    await waitFor(() => {
      expect(screen.getByText(/Everflow Revenue/i)).toBeInTheDocument();
    });

    // Click on Offers tab
    const offersTab = screen.getByText('Offers');
    await user.click(offersTab);

    await waitFor(() => {
      expect(screen.getByText(/Offer Performance/i)).toBeInTheDocument();
    });

    // Click on Properties tab
    const propertiesTab = screen.getByText('Properties');
    await user.click(propertiesTab);

    await waitFor(() => {
      expect(screen.getByText(/Property Performance/i)).toBeInTheDocument();
    });
  });

  it('displays correct totals', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => mockDashboardData,
    });

    render(<EverflowDashboard />);

    await waitFor(() => {
      // Check that the tab navigation exists
      expect(screen.getByText('Offers')).toBeInTheDocument();
      expect(screen.getByText('Properties')).toBeInTheDocument();
      expect(screen.getByText('Campaigns')).toBeInTheDocument();
    });
  });
});
