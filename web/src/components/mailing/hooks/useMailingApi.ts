import { useState, useCallback } from 'react';
import type {
  List,
  Subscriber,
  Campaign,
  DeliveryServer,
  SendingPlan,
  DashboardData,
} from '../types';

const API_BASE = '/api/mailing';
const USE_MOCK_DATA = false; // Disabled - using real database API

// Mock data for development/testing
const MOCK_DASHBOARD: DashboardData = {
  overview: {
    total_subscribers: 125000,
    total_lists: 12,
    total_campaigns: 45,
    daily_capacity: 500000,
    daily_used: 125000,
  },
  performance: {
    total_sent: 1250000,
    total_opens: 187500,
    total_clicks: 31250,
    total_revenue: 15625.0,
    open_rate: 15.0,
    click_rate: 2.5,
  },
  recent_campaigns: [],
};

const MOCK_LISTS: List[] = [
  {
    id: 'list-001',
    organization_id: 'org-001',
    name: 'Main Newsletter',
    description: 'Primary newsletter subscribers',
    default_from_name: 'Jarvis Team',
    default_from_email: 'newsletter@ignite.com',
    default_reply_to: 'reply@ignite.com',
    subscriber_count: 75000,
    active_count: 72000,
    opt_in_type: 'double',
    status: 'active',
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
  },
  {
    id: 'list-002',
    organization_id: 'org-001',
    name: 'Premium Members',
    description: 'Premium subscription members',
    default_from_name: 'Jarvis Premium',
    default_from_email: 'premium@ignite.com',
    default_reply_to: 'reply@ignite.com',
    subscriber_count: 25000,
    active_count: 24500,
    opt_in_type: 'single',
    status: 'active',
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
  },
];

const MOCK_CAMPAIGNS: Campaign[] = [
  {
    id: 'camp-001',
    organization_id: 'org-001',
    list_id: 'list-001',
    template_id: null,
    segment_id: null,
    name: 'February Newsletter',
    campaign_type: 'regular',
    subject: 'Your February Updates',
    from_name: 'Jarvis Team',
    from_email: 'newsletter@ignite.com',
    reply_to: 'reply@ignite.com',
    html_content: '<h1>Hello!</h1>',
    plain_content: 'Hello!',
    preview_text: 'Check out our latest updates',
    delivery_server_id: null,
    send_at: null,
    timezone: 'America/New_York',
    ai_send_time_optimization: true,
    ai_content_optimization: false,
    ai_audience_optimization: true,
    status: 'sent',
    total_recipients: 50000,
    sent_count: 50000,
    delivered_count: 49500,
    open_count: 8500,
    unique_open_count: 7500,
    click_count: 1275,
    unique_click_count: 1100,
    bounce_count: 250,
    complaint_count: 15,
    unsubscribe_count: 45,
    revenue: 637.5,
    started_at: new Date().toISOString(),
    completed_at: new Date().toISOString(),
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
  },
  {
    id: 'camp-002',
    organization_id: 'org-001',
    list_id: 'list-001',
    template_id: null,
    segment_id: null,
    name: 'Weekend Special',
    campaign_type: 'regular',
    subject: 'Exclusive Weekend Deals',
    from_name: 'Jarvis Deals',
    from_email: 'deals@ignite.com',
    reply_to: 'reply@ignite.com',
    html_content: '<h1>Weekend Deals!</h1>',
    plain_content: 'Weekend Deals!',
    preview_text: 'Exclusive weekend offers',
    delivery_server_id: null,
    send_at: null,
    timezone: 'America/New_York',
    ai_send_time_optimization: true,
    ai_content_optimization: true,
    ai_audience_optimization: true,
    status: 'sending',
    total_recipients: 30000,
    sent_count: 25000,
    delivered_count: 24800,
    open_count: 3750,
    unique_open_count: 3500,
    click_count: 562,
    unique_click_count: 500,
    bounce_count: 125,
    complaint_count: 8,
    unsubscribe_count: 22,
    revenue: 281.25,
    started_at: new Date().toISOString(),
    completed_at: null,
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
  },
];

const MOCK_SENDING_PLANS: SendingPlan[] = [
  {
    time_period: 'morning',
    name: 'Morning Focus',
    description: 'Concentrated morning send targeting high-engagement subscribers',
    recommended_volume: 50000,
    time_slots: [],
    audience_breakdown: [],
    offer_recommendations: [],
    predictions: {
      estimated_opens: 8500,
      estimated_clicks: 1275,
      estimated_revenue: 637.5,
      estimated_bounce_rate: 1.2,
      estimated_complaint_rate: 0.03,
      revenue_range: [510.0, 765.0],
      confidence_interval: 0.85,
    },
    confidence_score: 0.88,
    ai_explanation: 'Morning sends historically show 30% higher engagement.',
    warnings: [],
    recommendations: ['Ideal for time-sensitive offers', 'Best for high-value content'],
  },
  {
    time_period: 'first_half',
    name: 'First Half Balanced',
    description: 'Extended morning through early afternoon with balanced targeting',
    recommended_volume: 75000,
    time_slots: [],
    audience_breakdown: [],
    offer_recommendations: [],
    predictions: {
      estimated_opens: 11250,
      estimated_clicks: 1687,
      estimated_revenue: 843.75,
      estimated_bounce_rate: 1.5,
      estimated_complaint_rate: 0.04,
      revenue_range: [632.81, 1054.69],
      confidence_interval: 0.82,
    },
    confidence_score: 0.85,
    ai_explanation: 'Balanced approach spreading volume across prime engagement hours.',
    warnings: [],
    recommendations: ['Good for general campaigns', 'Allows afternoon performance review'],
  },
  {
    time_period: 'full_day',
    name: 'Full Day Maximum',
    description: 'Full day send maximizing reach across all subscriber segments',
    recommended_volume: 125000,
    time_slots: [],
    audience_breakdown: [],
    offer_recommendations: [],
    predictions: {
      estimated_opens: 17500,
      estimated_clicks: 2625,
      estimated_revenue: 1312.5,
      estimated_bounce_rate: 1.8,
      estimated_complaint_rate: 0.06,
      revenue_range: [918.75, 1706.25],
      confidence_interval: 0.78,
    },
    confidence_score: 0.8,
    ai_explanation: 'Maximum reach plan utilizing full daily capacity.',
    warnings: ['Higher complaint risk from low-engagement segment'],
    recommendations: ['Best for revenue maximization', 'Good for broad announcements'],
  },
];

async function apiFetch<T>(
  endpoint: string,
  options?: RequestInit
): Promise<T> {
  const response = await fetch(`${API_BASE}${endpoint}`, {
    ...options,
    headers: {
      'Content-Type': 'application/json',
      ...options?.headers,
    },
    credentials: 'include',
  });

  if (!response.ok) {
    const error = await response.json().catch(() => ({ error: 'Request failed' }));
    throw new Error(error.error || 'Request failed');
  }

  return response.json();
}

// Dashboard Hook
export function useDashboard() {
  const [data, setData] = useState<DashboardData | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const fetchDashboard = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const result = await apiFetch<DashboardData>('/dashboard');
      setData(result);
    } catch (err) {
      if (USE_MOCK_DATA) {
        // Use mock data when API is unavailable
        setData(MOCK_DASHBOARD);
      } else {
        setError(err instanceof Error ? err.message : 'Failed to load dashboard');
      }
    } finally {
      setLoading(false);
    }
  }, []);

  return { data, loading, error, refetch: fetchDashboard };
}

// Lists Hook
export function useLists() {
  const [lists, setLists] = useState<List[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const fetchLists = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const result = await apiFetch<{ lists: List[] }>('/lists');
      setLists(result.lists || []);
    } catch (err) {
      if (USE_MOCK_DATA) {
        setLists(MOCK_LISTS);
      } else {
        setError(err instanceof Error ? err.message : 'Failed to load lists');
      }
    } finally {
      setLoading(false);
    }
  }, []);

  const createList = useCallback(async (list: Partial<List>) => {
    const result = await apiFetch<List>('/lists', {
      method: 'POST',
      body: JSON.stringify(list),
    });
    setLists((prev) => [...prev, result]);
    return result;
  }, []);

  return { lists, loading, error, fetchLists, createList };
}

// Subscribers Hook
export function useSubscribers(listId: string) {
  const [subscribers, setSubscribers] = useState<Subscriber[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const fetchSubscribers = useCallback(
    async (limit = 50, offset = 0) => {
      if (!listId) return;
      setLoading(true);
      setError(null);
      try {
        const result = await apiFetch<{
          subscribers: Subscriber[];
          total: number;
        }>(`/lists/${listId}/subscribers?limit=${limit}&offset=${offset}`);
        setSubscribers(result.subscribers || []);
        setTotal(result.total);
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to load subscribers');
      } finally {
        setLoading(false);
      }
    },
    [listId]
  );

  const addSubscriber = useCallback(
    async (subscriber: Partial<Subscriber>) => {
      const result = await apiFetch<Subscriber>(`/lists/${listId}/subscribers`, {
        method: 'POST',
        body: JSON.stringify(subscriber),
      });
      setSubscribers((prev) => [result, ...prev]);
      setTotal((prev) => prev + 1);
      return result;
    },
    [listId]
  );

  return { subscribers, total, loading, error, fetchSubscribers, addSubscriber };
}

// Campaigns Hook
export function useCampaigns() {
  const [campaigns, setCampaigns] = useState<Campaign[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const fetchCampaigns = useCallback(async (limit = 50) => {
    setLoading(true);
    setError(null);
    try {
      const result = await apiFetch<{ campaigns: Campaign[] }>(
        `/campaigns?limit=${limit}`
      );
      setCampaigns(result.campaigns || []);
    } catch (err) {
      if (USE_MOCK_DATA) {
        setCampaigns(MOCK_CAMPAIGNS);
      } else {
        setError(err instanceof Error ? err.message : 'Failed to load campaigns');
      }
    } finally {
      setLoading(false);
    }
  }, []);

  const createCampaign = useCallback(async (campaign: Partial<Campaign>) => {
    const result = await apiFetch<Campaign>('/campaigns', {
      method: 'POST',
      body: JSON.stringify(campaign),
    });
    setCampaigns((prev) => [result, ...prev]);
    return result;
  }, []);

  const sendCampaign = useCallback(async (campaignId: string) => {
    const result = await apiFetch<{ message: string; total_recipients: number }>(
      `/campaigns/${campaignId}/send`,
      { method: 'POST' }
    );
    return result;
  }, []);

  // Async send - queues campaign for background processing with throttling
  const sendCampaignAsync = useCallback(async (campaignId: string) => {
    const result = await apiFetch<{ 
      message: string; 
      campaign_id: string;
      status: string;
      total_recipients: number;
    }>(
      `/campaigns/${campaignId}/send-async`,
      { method: 'POST' }
    );
    return result;
  }, []);

  // Set throttle rate for a campaign in progress
  const setThrottle = useCallback(async (campaignId: string, ratePerMinute: number) => {
    const result = await apiFetch<{ message: string; new_rate: number }>(
      `/campaigns/${campaignId}/throttle`,
      { 
        method: 'POST',
        body: JSON.stringify({ rate_per_minute: ratePerMinute }),
      }
    );
    return result;
  }, []);

  // Schedule a campaign for later sending
  const scheduleCampaign = useCallback(async (campaignId: string, scheduledAt: string) => {
    const result = await apiFetch<{ message: string; scheduled_at: string }>(
      `/campaigns/${campaignId}/schedule`,
      { 
        method: 'POST',
        body: JSON.stringify({ scheduled_at: scheduledAt }),
      }
    );
    return result;
  }, []);

  // Pause a sending campaign
  const pauseCampaign = useCallback(async (campaignId: string) => {
    const result = await apiFetch<{ message: string }>(
      `/campaigns/${campaignId}/pause`,
      { method: 'POST' }
    );
    return result;
  }, []);

  // Resume a paused campaign
  const resumeCampaign = useCallback(async (campaignId: string) => {
    const result = await apiFetch<{ message: string }>(
      `/campaigns/${campaignId}/resume`,
      { method: 'POST' }
    );
    return result;
  }, []);

  return { 
    campaigns, 
    loading, 
    error, 
    fetchCampaigns, 
    createCampaign, 
    sendCampaign,
    sendCampaignAsync,
    setThrottle,
    scheduleCampaign,
    pauseCampaign,
    resumeCampaign,
  };
}

// Campaign Detail Hook
export function useCampaign(campaignId: string) {
  const [campaign, setCampaign] = useState<Campaign | null>(null);
  const [stats, setStats] = useState<Record<string, number> | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const fetchCampaign = useCallback(async () => {
    if (!campaignId) return;
    setLoading(true);
    setError(null);
    try {
      const result = await apiFetch<{
        campaign: Campaign;
        stats: Record<string, number>;
      }>(`/campaigns/${campaignId}`);
      setCampaign(result.campaign);
      setStats(result.stats);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load campaign');
    } finally {
      setLoading(false);
    }
  }, [campaignId]);

  return { campaign, stats, loading, error, fetchCampaign };
}

// Delivery Servers Hook
export function useDeliveryServers() {
  const [servers, setServers] = useState<DeliveryServer[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const fetchServers = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const result = await apiFetch<{ servers: DeliveryServer[] }>(
        '/delivery-servers'
      );
      setServers(result.servers || []);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load servers');
    } finally {
      setLoading(false);
    }
  }, []);

  return { servers, loading, error, fetchServers };
}

// Sending Plans Hook
export function useSendingPlans() {
  const [plans, setPlans] = useState<SendingPlan[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const fetchPlans = useCallback(async (date?: string) => {
    setLoading(true);
    setError(null);
    try {
      const params = date ? `?date=${date}` : '';
      const result = await apiFetch<{ plans: SendingPlan[] }>(
        `/sending-plans${params}`
      );
      setPlans(result.plans || []);
    } catch (err) {
      if (USE_MOCK_DATA) {
        setPlans(MOCK_SENDING_PLANS);
      } else {
        setError(err instanceof Error ? err.message : 'Failed to generate plans');
      }
    } finally {
      setLoading(false);
    }
  }, []);

  return { plans, loading, error, fetchPlans };
}
