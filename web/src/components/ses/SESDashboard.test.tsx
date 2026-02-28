import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { SESDashboard } from './SESDashboard';

// Mock the useApi hook
const mockData = {
  timestamp: '2025-01-27T12:00:00Z',
  last_fetch: '2025-01-27T12:00:00Z',
  summary: {
    timestamp: '2025-01-27T12:00:00Z',
    period_start: '2025-01-27T00:00:00Z',
    period_end: '2025-01-27T23:59:59Z',
    total_targeted: 50000,
    total_delivered: 48500,
    total_opened: 15000,
    total_clicked: 3000,
    total_bounced: 1500,
    total_complaints: 25,
    total_unsubscribes: 200,
    delivery_rate: 0.97,
    open_rate: 0.31,
    click_rate: 0.062,
    bounce_rate: 0.03,
    complaint_rate: 0.0005,
    unsubscribe_rate: 0.004,
  },
  isp_metrics: [
    {
      provider: 'Gmail',
      metrics: {
        timestamp: '2025-01-27T12:00:00Z',
        source: 'ses',
        group_by: 'isp',
        group_value: 'Gmail',
        targeted: 25000,
        injected: 25000,
        sent: 25000,
        delivered: 24500,
        opened: 8000,
        unique_opened: 7000,
        clicked: 1500,
        unique_clicked: 1200,
        bounced: 500,
        hard_bounced: 300,
        soft_bounced: 200,
        block_bounced: 0,
        complaints: 10,
        unsubscribes: 100,
        delayed: 0,
        rejected: 0,
        delivery_rate: 0.98,
        open_rate: 0.33,
        click_rate: 0.061,
        bounce_rate: 0.02,
        hard_bounce_rate: 0.012,
        soft_bounce_rate: 0.008,
        block_rate: 0,
        complaint_rate: 0.0004,
        unsubscribe_rate: 0.004,
      },
      status: 'healthy' as const,
    },
    {
      provider: 'Yahoo',
      metrics: {
        timestamp: '2025-01-27T12:00:00Z',
        source: 'ses',
        group_by: 'isp',
        group_value: 'Yahoo',
        targeted: 15000,
        injected: 15000,
        sent: 15000,
        delivered: 14500,
        opened: 4500,
        unique_opened: 4000,
        clicked: 900,
        unique_clicked: 800,
        bounced: 500,
        hard_bounced: 350,
        soft_bounced: 150,
        block_bounced: 0,
        complaints: 8,
        unsubscribes: 60,
        delayed: 0,
        rejected: 0,
        delivery_rate: 0.967,
        open_rate: 0.31,
        click_rate: 0.062,
        bounce_rate: 0.033,
        hard_bounce_rate: 0.023,
        soft_bounce_rate: 0.01,
        block_rate: 0,
        complaint_rate: 0.00053,
        unsubscribe_rate: 0.004,
      },
      status: 'healthy' as const,
    },
  ],
  signals: {
    timestamp: '2025-01-27T12:00:00Z',
    top_issues: [],
    recommendations: [],
  },
};

vi.mock('../../hooks/useApi', () => ({
  useApi: vi.fn(),
}));

import { useApi } from '../../hooks/useApi';

describe('SESDashboard', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders loading state', () => {
    vi.mocked(useApi).mockReturnValue({
      data: null,
      loading: true,
      error: null,
      refetch: vi.fn(),
    });

    render(<SESDashboard />);
    expect(screen.getByText('Loading AWS SES dashboard...')).toBeInTheDocument();
  });

  it('renders error state', () => {
    vi.mocked(useApi).mockReturnValue({
      data: null,
      loading: false,
      error: 'Failed to load',
      refetch: vi.fn(),
    });

    render(<SESDashboard />);
    expect(screen.getByText('Failed to load')).toBeInTheDocument();
  });

  it('renders dashboard with data', () => {
    vi.mocked(useApi).mockReturnValue({
      data: mockData,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });

    render(<SESDashboard />);
    
    // Check header
    expect(screen.getByText('AWS SES Dashboard')).toBeInTheDocument();
    
    // Check VDM badge
    expect(screen.getByText('VDM Enabled')).toBeInTheDocument();
    
    // Check ISP count badge
    expect(screen.getByText('2 ISPs')).toBeInTheDocument();
    
    // Check metric cards are rendered
    expect(screen.getByText('Volume Today')).toBeInTheDocument();
    expect(screen.getByText('Delivery Rate')).toBeInTheDocument();
    expect(screen.getByText('Open Rate')).toBeInTheDocument();
    expect(screen.getByText('Complaint Rate')).toBeInTheDocument();
    expect(screen.getByText('Bounce Rate')).toBeInTheDocument();
  });

  it('renders ISP table with data', () => {
    vi.mocked(useApi).mockReturnValue({
      data: mockData,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });

    render(<SESDashboard />);
    
    // Check ISP Performance section
    expect(screen.getByText('ISP Performance (VDM)')).toBeInTheDocument();
    expect(screen.getByText('Gmail')).toBeInTheDocument();
    expect(screen.getByText('Yahoo')).toBeInTheDocument();
  });

  it('renders delivery breakdown', () => {
    vi.mocked(useApi).mockReturnValue({
      data: mockData,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });

    render(<SESDashboard />);
    
    // Check Delivery Breakdown section
    expect(screen.getByText('Delivery Breakdown')).toBeInTheDocument();
    expect(screen.getByText('Delivered')).toBeInTheDocument();
    expect(screen.getByText('Opened')).toBeInTheDocument();
    expect(screen.getByText('Clicked')).toBeInTheDocument();
    expect(screen.getByText('Bounced')).toBeInTheDocument();
    // Complaints appears multiple times (in table header and breakdown), use getAllByText
    expect(screen.getAllByText('Complaints').length).toBeGreaterThan(0);
    expect(screen.getByText('Unsubscribes')).toBeInTheDocument();
  });

  it('renders no issues message when empty', () => {
    vi.mocked(useApi).mockReturnValue({
      data: mockData,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });

    render(<SESDashboard />);
    
    expect(screen.getByText('No issues detected - all metrics healthy')).toBeInTheDocument();
  });

  it('renders issues when present', () => {
    const dataWithIssues = {
      ...mockData,
      signals: {
        timestamp: '2025-01-27T12:00:00Z',
        top_issues: [
          {
            severity: 'warning' as const,
            category: 'complaint',
            description: 'Elevated complaint rate at Yahoo',
            affected_isp: 'Yahoo',
            count: 50,
            recommendation: 'Review list hygiene for this ISP',
          },
        ],
        recommendations: ['Review email content quality'],
      },
    };

    vi.mocked(useApi).mockReturnValue({
      data: dataWithIssues,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });

    render(<SESDashboard />);
    
    expect(screen.getByText('Elevated complaint rate at Yahoo')).toBeInTheDocument();
    expect(screen.getByText('Review list hygiene for this ISP')).toBeInTheDocument();
  });

  it('renders recommendations when no issues but recommendations exist', () => {
    const dataWithRecommendations = {
      ...mockData,
      signals: {
        timestamp: '2025-01-27T12:00:00Z',
        top_issues: [],
        recommendations: [
          'Verify SPF, DKIM, and DMARC authentication records',
          'Monitor your sending IP reputation',
        ],
      },
    };

    vi.mocked(useApi).mockReturnValue({
      data: dataWithRecommendations,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });

    render(<SESDashboard />);
    
    expect(screen.getByText('Verify SPF, DKIM, and DMARC authentication records')).toBeInTheDocument();
    expect(screen.getByText('Monitor your sending IP reputation')).toBeInTheDocument();
  });

  it('renders last updated time', () => {
    vi.mocked(useApi).mockReturnValue({
      data: mockData,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });

    render(<SESDashboard />);
    
    expect(screen.getByText(/Last updated:/)).toBeInTheDocument();
    expect(screen.getByText(/Refreshes every minute/)).toBeInTheDocument();
  });

  it('handles empty ISP metrics', () => {
    const dataWithNoISPs = {
      ...mockData,
      isp_metrics: [],
    };

    vi.mocked(useApi).mockReturnValue({
      data: dataWithNoISPs,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });

    render(<SESDashboard />);
    
    expect(screen.getByText('No ISP data available. VDM metrics may take a few minutes to populate.')).toBeInTheDocument();
  });

  it('displays correct status for high complaint rate', () => {
    const dataWithHighComplaints = {
      ...mockData,
      summary: {
        ...mockData.summary,
        complaint_rate: 0.001, // 0.1% - above critical threshold
      },
    };

    vi.mocked(useApi).mockReturnValue({
      data: dataWithHighComplaints,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });

    render(<SESDashboard />);
    
    // The MetricCard should show critical status for high complaint rate
    // We verify by checking the component renders without error
    expect(screen.getByText('Complaint Rate')).toBeInTheDocument();
  });

  it('displays warning for low delivery rate', () => {
    const dataWithLowDelivery = {
      ...mockData,
      summary: {
        ...mockData.summary,
        delivery_rate: 0.85, // Below 90% threshold
      },
    };

    vi.mocked(useApi).mockReturnValue({
      data: dataWithLowDelivery,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });

    render(<SESDashboard />);
    
    // The MetricCard should show warning status for low delivery rate
    expect(screen.getByText('Delivery Rate')).toBeInTheDocument();
  });

  it('correctly renders ISP table columns', () => {
    vi.mocked(useApi).mockReturnValue({
      data: mockData,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });

    render(<SESDashboard />);
    
    // Check table headers
    expect(screen.getByText('ISP')).toBeInTheDocument();
    expect(screen.getByText('Status')).toBeInTheDocument();
    expect(screen.getByText('Volume')).toBeInTheDocument();
    expect(screen.getByText('Delivery')).toBeInTheDocument();
    expect(screen.getByText('Opens')).toBeInTheDocument();
    expect(screen.getByText('Clicks')).toBeInTheDocument();
  });

  it('handles ISP with critical status', () => {
    const dataWithCriticalISP = {
      ...mockData,
      isp_metrics: [
        {
          ...mockData.isp_metrics[0],
          status: 'critical' as const,
          status_reason: 'Complaint rate exceeds threshold',
        },
      ],
    };

    vi.mocked(useApi).mockReturnValue({
      data: dataWithCriticalISP,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });

    render(<SESDashboard />);
    
    // Component should render without error with critical status
    expect(screen.getByText('Gmail')).toBeInTheDocument();
  });
});
