import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { CampaignDetailsModal } from './CampaignDetailsModal';
import { EnrichedCampaignResponse } from '../../types';

// Mock fetch
const mockFetch = vi.fn();
global.fetch = mockFetch;

const mockEnrichedCampaign: EnrichedCampaignResponse = {
  timestamp: '2026-01-27T12:00:00Z',
  campaign: {
    mailing_id: '3219537162',
    campaign_name: '01272026_TDIH_407_Empire Flooring_Segment1',
    property_code: 'TDIH',
    property_name: 'thisdayinhistory.co',
    offer_id: '407',
    offer_name: 'Empire Flooring CPL',
    clicks: 1500,
    conversions: 15,
    revenue: 3375.00,
    payout: 3375.00,
    subject: 'Transform Your Home with New Floors - Special Offer Inside!',
    sending_domain: 'mail.thisdayinhistory.co',
    esp_name: 'SparkPost',
    esp_connection_id: '100',
    audience_size: 50000,
    sent: 48000,
    delivered: 45600,
    opens: 12000,
    unique_opens: 9120,
    email_clicks: 2500,
    unique_email_clicks: 1824,
    bounces: 300,
    unsubscribes: 45,
    complaints: 5,
    schedule_date: '2026-01-27 08:00:00',
    sending_start_date: '2026-01-27 08:01:00',
    sending_end_date: '2026-01-27 10:30:00',
    status: '60004',
    status_desc: 'Completed',
    sending_segments: [
      { segment_id: '1001', name: 'TDIH Active 30 Days', count: 45000, is_suppression: false },
    ],
    suppression_segments: [
      { segment_id: '1002', name: 'Global Suppression', count: 5000, is_suppression: true },
    ],
    ecpm: 67.50,
    revenue_per_click: 2.25,
    conversion_rate: 0.01,
    delivery_rate: 0.95,
    open_rate: 0.20,
    click_to_open_rate: 0.274,
    ongage_linked: true,
  },
};

const mockCampaignWithoutOngage: EnrichedCampaignResponse = {
  timestamp: '2026-01-27T12:00:00Z',
  campaign: {
    ...mockEnrichedCampaign.campaign,
    ongage_linked: false,
    link_error: 'Ongage client not configured',
    campaign_name: '',
    subject: '',
    sending_domain: '',
    esp_name: '',
    esp_connection_id: '',
    audience_size: 0,
    sent: 0,
    delivered: 0,
    opens: 0,
    unique_opens: 0,
    email_clicks: 0,
    unique_email_clicks: 0,
    bounces: 0,
    unsubscribes: 0,
    complaints: 0,
    schedule_date: '',
    sending_start_date: '',
    sending_end_date: '',
    status: '',
    status_desc: '',
    sending_segments: [],
    suppression_segments: [],
    ecpm: 0,
    delivery_rate: 0,
    open_rate: 0,
    click_to_open_rate: 0,
  },
};

describe('CampaignDetailsModal', () => {
  const mockOnClose = vi.fn();

  beforeEach(() => {
    mockFetch.mockReset();
    mockOnClose.mockReset();
  });

  it('renders loading state initially', () => {
    mockFetch.mockImplementation(() => 
      new Promise(() => {}) // Never resolves
    );
    
    render(<CampaignDetailsModal mailingId="3219537162" onClose={mockOnClose} />);
    
    expect(screen.getByText(/Loading campaign details/i)).toBeInTheDocument();
  });

  it('renders campaign details when loaded', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => mockEnrichedCampaign,
    });

    render(<CampaignDetailsModal mailingId="3219537162" onClose={mockOnClose} />);

    await waitFor(() => {
      expect(screen.getByText(mockEnrichedCampaign.campaign.campaign_name)).toBeInTheDocument();
    });

    // Check revenue is displayed
    expect(screen.getByText('$3,375.00')).toBeInTheDocument();
    
    // Check eCPM is displayed
    expect(screen.getByText('$67.50')).toBeInTheDocument();
    
    // Check Ongage linked status
    expect(screen.getByText(/Ongage Linked/i)).toBeInTheDocument();
    
    // Check subject is displayed
    expect(screen.getByText(mockEnrichedCampaign.campaign.subject)).toBeInTheDocument();
  });

  it('renders error state on fetch failure', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'Campaign not found' }),
    });

    render(<CampaignDetailsModal mailingId="invalid" onClose={mockOnClose} />);

    await waitFor(() => {
      expect(screen.getByText(/Campaign not found/i)).toBeInTheDocument();
    });
  });

  it('calls onClose when close button is clicked', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => mockEnrichedCampaign,
    });

    const user = userEvent.setup();
    render(<CampaignDetailsModal mailingId="3219537162" onClose={mockOnClose} />);

    await waitFor(() => {
      expect(screen.getByText(mockEnrichedCampaign.campaign.campaign_name)).toBeInTheDocument();
    });

    const closeButton = screen.getByLabelText('Close');
    await user.click(closeButton);

    expect(mockOnClose).toHaveBeenCalledTimes(1);
  });

  it('calls onClose when backdrop is clicked', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => mockEnrichedCampaign,
    });

    const user = userEvent.setup();
    render(<CampaignDetailsModal mailingId="3219537162" onClose={mockOnClose} />);

    await waitFor(() => {
      expect(screen.getByText(mockEnrichedCampaign.campaign.campaign_name)).toBeInTheDocument();
    });

    // Click backdrop (modal-backdrop class)
    const backdrop = document.querySelector('.modal-backdrop');
    expect(backdrop).not.toBeNull();
    if (backdrop) {
      await user.click(backdrop);
      expect(mockOnClose).toHaveBeenCalledTimes(1);
    }
  });

  it('calls onClose when Escape key is pressed', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => mockEnrichedCampaign,
    });

    render(<CampaignDetailsModal mailingId="3219537162" onClose={mockOnClose} />);

    await waitFor(() => {
      expect(screen.getByText(mockEnrichedCampaign.campaign.campaign_name)).toBeInTheDocument();
    });

    fireEvent.keyDown(document, { key: 'Escape' });

    expect(mockOnClose).toHaveBeenCalledTimes(1);
  });

  it('displays Ongage unlinked status when not linked', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => mockCampaignWithoutOngage,
    });

    render(<CampaignDetailsModal mailingId="3219537162" onClose={mockOnClose} />);

    await waitFor(() => {
      expect(screen.getByText(/Ongage client not configured/i)).toBeInTheDocument();
    });

    // Email delivery section should not be visible
    expect(screen.queryByText(/Email Delivery/i)).not.toBeInTheDocument();
  });

  it('displays mailing ID in header', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => mockEnrichedCampaign,
    });

    render(<CampaignDetailsModal mailingId="3219537162" onClose={mockOnClose} />);

    await waitFor(() => {
      expect(screen.getByText(/Mailing ID: 3219537162/i)).toBeInTheDocument();
    });
  });

  it('displays sending and suppression segments', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => mockEnrichedCampaign,
    });

    render(<CampaignDetailsModal mailingId="3219537162" onClose={mockOnClose} />);

    await waitFor(() => {
      expect(screen.getByText('TDIH Active 30 Days')).toBeInTheDocument();
      expect(screen.getByText('Global Suppression')).toBeInTheDocument();
    });

    // Check segment counts
    expect(screen.getByText('45,000')).toBeInTheDocument();
    expect(screen.getByText('5,000')).toBeInTheDocument();
  });

  it('displays property and offer info', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => mockEnrichedCampaign,
    });

    render(<CampaignDetailsModal mailingId="3219537162" onClose={mockOnClose} />);

    await waitFor(() => {
      expect(screen.getByText('thisdayinhistory.co')).toBeInTheDocument();
      expect(screen.getByText('Empire Flooring CPL')).toBeInTheDocument();
    });
  });

  it('fetches correct API endpoint', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => mockEnrichedCampaign,
    });

    render(<CampaignDetailsModal mailingId="3219537162" onClose={mockOnClose} />);

    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith('/api/everflow/campaigns/3219537162');
    });
  });

  it('displays conversion metrics correctly', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => mockEnrichedCampaign,
    });

    render(<CampaignDetailsModal mailingId="3219537162" onClose={mockOnClose} />);

    await waitFor(() => {
      // Conversions count
      expect(screen.getByText('15')).toBeInTheDocument();
      // Clicks count
      expect(screen.getByText('1,500')).toBeInTheDocument();
    });
  });

  it('displays email delivery stats when Ongage linked', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => mockEnrichedCampaign,
    });

    render(<CampaignDetailsModal mailingId="3219537162" onClose={mockOnClose} />);

    await waitFor(() => {
      // Check audience size
      expect(screen.getByText('50,000')).toBeInTheDocument();
      // Check delivered
      expect(screen.getByText('45,600')).toBeInTheDocument();
      // Check unique opens
      expect(screen.getByText('9,120')).toBeInTheDocument();
    });
  });

  it('displays sending domain and ESP', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => mockEnrichedCampaign,
    });

    render(<CampaignDetailsModal mailingId="3219537162" onClose={mockOnClose} />);

    await waitFor(() => {
      expect(screen.getByText('mail.thisdayinhistory.co')).toBeInTheDocument();
      expect(screen.getByText('SparkPost')).toBeInTheDocument();
    });
  });
});
