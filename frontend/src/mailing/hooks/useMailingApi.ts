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
      setError(err instanceof Error ? err.message : 'Failed to load dashboard');
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
      setError(err instanceof Error ? err.message : 'Failed to load lists');
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
      setError(err instanceof Error ? err.message : 'Failed to load campaigns');
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

  return { campaigns, loading, error, fetchCampaigns, createCampaign, sendCampaign };
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
      setError(err instanceof Error ? err.message : 'Failed to generate plans');
    } finally {
      setLoading(false);
    }
  }, []);

  return { plans, loading, error, fetchPlans };
}
