// useDataIngestStream — EventSource on /api/mailing/data-ingest/stream.
//
// The counters consumer fans out one `event: delta` per coalesced counter
// increment (REQ: {day, supply_class, transition, by_isp, n, dataset_id}).
// This hook owns ONLY the transport: it buffers the deltas it has seen and
// reports whether the stream is connected. Callers merge those deltas into
// their polled snapshot — the 30s usePolling fallback stays the source of
// truth, the stream just moves the number between polls.
//
// It never throws: a parse failure drops that one event, a transport failure
// reconnects with exponential backoff (1s → 30s cap). Modeled on
// hooks/useConvictionStream.ts.

import { useCallback, useEffect, useRef, useState } from 'react';
import type { IngestDelta } from '../components/mailing/dataingest/api';

const STREAM_URL = '/api/mailing/data-ingest/stream';

export interface DataIngestStream {
  /** newest first, capped at maxBuffer */
  deltas: IngestDelta[];
  /** most recent delta, or null */
  latest: IngestDelta | null;
  /** SSE connected right now */
  live: boolean;
  /** total deltas received this session */
  received: number;
  /** last delta arrival (ms epoch), 0 if none */
  lastAt: number;
  /** drop the buffer (callers do this after folding deltas into a fresh poll) */
  clear: () => void;
}

interface Options {
  enabled?: boolean;
  maxBuffer?: number;
}

function parseDelta(raw: string): IngestDelta | null {
  try {
    const d = JSON.parse(raw) as Partial<IngestDelta>;
    if (!d || typeof d !== 'object') return null;
    if (typeof d.n !== 'number' || !Number.isFinite(d.n)) return null;
    if (typeof d.day !== 'string' || typeof d.transition !== 'string') return null;
    const cls = d.supply_class;
    if (cls !== 'at_rest' && cls !== 'dynamic' && cls !== 'internal_transfer') return null;
    const byIsp: Record<string, number> = {};
    if (d.by_isp && typeof d.by_isp === 'object') {
      for (const [k, v] of Object.entries(d.by_isp)) {
        if (typeof v === 'number' && Number.isFinite(v)) byIsp[k] = v;
      }
    }
    return {
      day: d.day,
      supply_class: cls,
      transition: d.transition,
      by_isp: byIsp,
      n: d.n,
      dataset_id: typeof d.dataset_id === 'string' ? d.dataset_id : '',
    };
  } catch {
    return null;
  }
}

export function useDataIngestStream(options: Options = {}): DataIngestStream {
  const { enabled = true, maxBuffer = 200 } = options;
  const [deltas, setDeltas] = useState<IngestDelta[]>([]);
  const [live, setLive] = useState(false);
  const [received, setReceived] = useState(0);
  const [lastAt, setLastAt] = useState(0);
  const esRef = useRef<EventSource | null>(null);
  const backoffRef = useRef(1000);

  const clear = useCallback(() => setDeltas([]), []);

  useEffect(() => {
    if (!enabled) return;
    // EventSource is absent in some test/SSR environments — degrade to polling
    // only rather than throwing on mount.
    if (typeof EventSource === 'undefined') return;

    let cancelled = false;
    let reconnectTimer: ReturnType<typeof setTimeout>;

    const connect = () => {
      if (cancelled) return;
      let es: EventSource;
      try {
        es = new EventSource(STREAM_URL);
      } catch {
        reconnectTimer = setTimeout(connect, Math.min(backoffRef.current, 30_000));
        backoffRef.current = Math.min(backoffRef.current * 2, 30_000);
        return;
      }
      esRef.current = es;

      es.onopen = () => {
        if (cancelled) return;
        setLive(true);
        backoffRef.current = 1000;
      };

      es.addEventListener('delta', (event) => {
        const d = parseDelta((event as MessageEvent).data as string);
        if (!d || cancelled) return;
        setDeltas((prev) => {
          const next = [d, ...prev];
          return next.length > maxBuffer ? next.slice(0, maxBuffer) : next;
        });
        setReceived((n) => n + 1);
        setLastAt(Date.now());
      });

      es.onerror = () => {
        if (cancelled) return;
        setLive(false);
        es.close();
        esRef.current = null;
        const delay = Math.min(backoffRef.current, 30_000);
        backoffRef.current = Math.min(delay * 2, 30_000);
        reconnectTimer = setTimeout(connect, delay);
      };
    };

    connect();

    return () => {
      cancelled = true;
      clearTimeout(reconnectTimer);
      if (esRef.current) {
        esRef.current.close();
        esRef.current = null;
      }
    };
  }, [enabled, maxBuffer]);

  return {
    deltas,
    latest: deltas.length > 0 ? deltas[0] : null,
    live,
    received,
    lastAt,
    clear,
  };
}

export default useDataIngestStream;
