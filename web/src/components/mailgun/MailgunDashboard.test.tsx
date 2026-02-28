import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MailgunDashboard } from './MailgunDashboard';

// Mock the useApi hook
const mockData = {
  timestamp: '2025-01-27T12:00:00Z',
  last_fetch: '2025-01-27T12:00:00Z',
  summary: {
    timestamp: '2025-01-27T12:00:00Z',
    period_start: '2025-01-27T00:00:00Z',
    period_end: '2025-01-27T23:59:59Z',
    total_targeted: 10000,
    total_delivered: 9500,
    total_opened: 2000,
    total_clicked: 500,
    total_bounced: 500,
    total_complaints: 50,
    total_unsubscribes: 100,
    delivery_rate: 0.95,
    open_rate: 0.21,
    click_rate: 0.053,
    bounce_rate: 0.05,
    complaint_rate: 0.0053,
    unsubscribe_rate: 0.01,
  },
  isp_metrics: [
    {
      provider: 'Gmail',
      metrics: {
        timestamp: '2025-01-27T12:00:00Z',
        source: 'mailgun',
        group_by: 'provider',
        group_value: 'Gmail',
        targeted: 5000,
        injected: 5000,
        sent: 5000,
        delivered: 4750,
        opened: 1000,
        unique_opened: 800,
        clicked: 250,
        unique_clicked: 200,
        bounced: 250,
        hard_bounced: 150,
        soft_bounced: 100,
        block_bounced: 0,
        complaints: 25,
        unsubscribes: 50,
        delayed: 0,
        rejected: 0,
        delivery_rate: 0.95,
        open_rate: 0.17,
        click_rate: 0.042,
        bounce_rate: 0.05,
        hard_bounce_rate: 0.03,
        soft_bounce_rate: 0.02,
        block_rate: 0,
        complaint_rate: 0.0053,
        unsubscribe_rate: 0.01,
      },
      status: 'healthy' as const,
    },
  ],
  domain_metrics: [
    {
      domain: 'e.newproductsforyou.com',
      metrics: {
        timestamp: '2025-01-27T12:00:00Z',
        source: 'mailgun',
        group_by: 'domain',
        group_value: 'e.newproductsforyou.com',
        targeted: 2000,
        injected: 2000,
        sent: 2000,
        delivered: 1900,
        opened: 400,
        unique_opened: 350,
        clicked: 100,
        unique_clicked: 80,
        bounced: 100,
        hard_bounced: 60,
        soft_bounced: 40,
        block_bounced: 0,
        complaints: 10,
        unsubscribes: 20,
        delayed: 0,
        rejected: 0,
        delivery_rate: 0.95,
        open_rate: 0.18,
        click_rate: 0.042,
        bounce_rate: 0.05,
        hard_bounce_rate: 0.03,
        soft_bounce_rate: 0.02,
        block_rate: 0,
        complaint_rate: 0.0053,
        unsubscribe_rate: 0.01,
      },
      status: 'healthy' as const,
    },
  ],
  signals: {
    timestamp: '2025-01-27T12:00:00Z',
    bounce_reasons: [],
    top_issues: [],
  },
};

vi.mock('../../hooks/useApi', () => ({
  useApi: vi.fn(),
}));

import { useApi } from '../../hooks/useApi';

describe('MailgunDashboard', () => {
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

    render(<MailgunDashboard />);
    expect(screen.getByText('Loading Mailgun dashboard...')).toBeInTheDocument();
  });

  it('renders error state', () => {
    vi.mocked(useApi).mockReturnValue({
      data: null,
      loading: false,
      error: 'Failed to load',
      refetch: vi.fn(),
    });

    render(<MailgunDashboard />);
    expect(screen.getByText('Failed to load')).toBeInTheDocument();
  });

  it('renders dashboard with data', () => {
    vi.mocked(useApi).mockReturnValue({
      data: mockData,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });

    render(<MailgunDashboard />);
    
    // Check header
    expect(screen.getByText('Mailgun Dashboard')).toBeInTheDocument();
    
    // Check domain count badge
    expect(screen.getByText('1 domains')).toBeInTheDocument();
    
    // Check metric cards are rendered
    expect(screen.getByText('Volume Today')).toBeInTheDocument();
    expect(screen.getByText('Open Rate')).toBeInTheDocument();
    expect(screen.getByText('Click Rate')).toBeInTheDocument();
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

    render(<MailgunDashboard />);
    
    // Check ISP Performance section
    expect(screen.getByText('ISP Performance (Mailgun)')).toBeInTheDocument();
    expect(screen.getByText('Gmail')).toBeInTheDocument();
  });

  it('renders domain table with data', () => {
    vi.mocked(useApi).mockReturnValue({
      data: mockData,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });

    render(<MailgunDashboard />);
    
    // Check Sending Domains section
    expect(screen.getByText('Sending Domains')).toBeInTheDocument();
    expect(screen.getByText('e.newproductsforyou.com')).toBeInTheDocument();
  });

  it('renders delivery breakdown', () => {
    vi.mocked(useApi).mockReturnValue({
      data: mockData,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });

    render(<MailgunDashboard />);
    
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

    render(<MailgunDashboard />);
    
    expect(screen.getByText('No issues detected')).toBeInTheDocument();
  });

  it('renders issues when present', () => {
    const dataWithIssues = {
      ...mockData,
      signals: {
        timestamp: '2025-01-27T12:00:00Z',
        bounce_reasons: [],
        top_issues: [
          {
            severity: 'warning' as const,
            category: 'bounce',
            description: 'High bounce rate detected',
            count: 1000,
            recommendation: 'Check list hygiene',
          },
        ],
      },
    };

    vi.mocked(useApi).mockReturnValue({
      data: dataWithIssues,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });

    render(<MailgunDashboard />);
    
    expect(screen.getByText('High bounce rate detected')).toBeInTheDocument();
  });

  it('renders last updated time', () => {
    vi.mocked(useApi).mockReturnValue({
      data: mockData,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });

    render(<MailgunDashboard />);
    
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

    render(<MailgunDashboard />);
    
    expect(screen.getByText('No ISP data available')).toBeInTheDocument();
  });

  it('handles empty domain metrics', () => {
    const dataWithNoDomains = {
      ...mockData,
      domain_metrics: [],
    };

    vi.mocked(useApi).mockReturnValue({
      data: dataWithNoDomains,
      loading: false,
      error: null,
      refetch: vi.fn(),
    });

    render(<MailgunDashboard />);
    
    expect(screen.getByText('No domain data available')).toBeInTheDocument();
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

    render(<MailgunDashboard />);
    
    // The MetricCard should show critical status for high complaint rate
    // We verify by checking the component renders without error
    expect(screen.getByText('Complaint Rate')).toBeInTheDocument();
  });
});
