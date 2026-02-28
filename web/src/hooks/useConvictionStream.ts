import { useState, useEffect, useRef, useCallback } from 'react';

export interface Conviction {
  id: string;
  agent_type: string;
  isp: string;
  verdict: 'will' | 'wont';
  statement: string;
  context: Record<string, unknown>;
  confidence: number;
  corroborations: number;
  created_at: string;
  last_seen_at: string;
}

export interface ConvictionStats {
  total: number;
  will: number;
  wont: number;
  perMinute: number;
  byISP: Record<string, { will: number; wont: number }>;
  byAgent: Record<string, { will: number; wont: number }>;
}

interface UseConvictionStreamOptions {
  isp?: string;
  agentType?: string;
  maxBuffer?: number;
  enabled?: boolean;
}

export function useConvictionStream(options: UseConvictionStreamOptions = {}) {
  const { isp, agentType, maxBuffer = 200, enabled = true } = options;
  const [convictions, setConvictions] = useState<Conviction[]>([]);
  const [connected, setConnected] = useState(false);
  const [stats, setStats] = useState<ConvictionStats>({
    total: 0, will: 0, wont: 0, perMinute: 0,
    byISP: {}, byAgent: {},
  });
  const esRef = useRef<EventSource | null>(null);
  const retryRef = useRef(1000);
  const timestampsRef = useRef<number[]>([]);

  const computePerMinute = useCallback(() => {
    const now = Date.now();
    const cutoff = now - 60_000;
    timestampsRef.current = timestampsRef.current.filter(t => t > cutoff);
    return timestampsRef.current.length;
  }, []);

  useEffect(() => {
    setConvictions([]);
    setStats({ total: 0, will: 0, wont: 0, perMinute: 0, byISP: {}, byAgent: {} });
    timestampsRef.current = [];
  }, [isp, agentType]);

  useEffect(() => {
    if (!enabled) return;

    let cancelled = false;
    let reconnectTimer: ReturnType<typeof setTimeout>;

    function connect() {
      if (cancelled) return;

      const params = new URLSearchParams();
      if (isp) params.set('isp', isp);
      if (agentType) params.set('agent_type', agentType);
      const qs = params.toString();
      const url = `/api/mailing/engine/convictions/stream${qs ? '?' + qs : ''}`;

      const es = new EventSource(url);
      esRef.current = es;

      es.onopen = () => {
        setConnected(true);
        retryRef.current = 1000;
      };

      es.addEventListener('conviction', (event) => {
        try {
          const c: Conviction = JSON.parse(event.data);
          timestampsRef.current.push(Date.now());

          setConvictions(prev => {
            const next = [c, ...prev];
            return next.length > maxBuffer ? next.slice(0, maxBuffer) : next;
          });

          setStats(prev => {
            const isWill = c.verdict === 'will';
            const byISP = { ...prev.byISP };
            if (!byISP[c.isp]) byISP[c.isp] = { will: 0, wont: 0 };
            byISP[c.isp] = {
              will: byISP[c.isp].will + (isWill ? 1 : 0),
              wont: byISP[c.isp].wont + (isWill ? 0 : 1),
            };

            const byAgent = { ...prev.byAgent };
            if (!byAgent[c.agent_type]) byAgent[c.agent_type] = { will: 0, wont: 0 };
            byAgent[c.agent_type] = {
              will: byAgent[c.agent_type].will + (isWill ? 1 : 0),
              wont: byAgent[c.agent_type].wont + (isWill ? 0 : 1),
            };

            return {
              total: prev.total + 1,
              will: prev.will + (isWill ? 1 : 0),
              wont: prev.wont + (isWill ? 0 : 1),
              perMinute: computePerMinute(),
              byISP,
              byAgent,
            };
          });
        } catch {
          // ignore parse errors
        }
      });

      es.onerror = () => {
        setConnected(false);
        es.close();
        esRef.current = null;
        const delay = Math.min(retryRef.current, 30_000);
        retryRef.current = delay * 2;
        reconnectTimer = setTimeout(connect, delay);
      };
    }

    connect();

    // Periodic velocity recalculation
    const velInterval = setInterval(() => {
      setStats(prev => ({ ...prev, perMinute: computePerMinute() }));
    }, 5000);

    return () => {
      cancelled = true;
      clearTimeout(reconnectTimer);
      clearInterval(velInterval);
      if (esRef.current) {
        esRef.current.close();
        esRef.current = null;
      }
    };
  }, [isp, agentType, maxBuffer, enabled, computePerMinute]);

  return { convictions, connected, stats };
}
