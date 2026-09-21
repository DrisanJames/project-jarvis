// testUtils — shared plumbing for the data-ingest panel tests: a URL-routed
// apiFetch mock returning the REAL prod bodies under __fixtures__/, and the
// ResizeObserver stub recharts' ResponsiveContainer needs under jsdom.

import { vi, type Mock } from 'vitest';

export const resp = (status: number, body: unknown) => ({
  ok: status >= 200 && status < 300,
  status,
  json: async () => JSON.parse(JSON.stringify(body)),
  text: async () => JSON.stringify(body),
});

export type Route = (url: string, init?: RequestInit) => ReturnType<typeof resp> | undefined;

/** Install a URL router on the mocked apiFetch; unmatched URLs 404. */
export function installRoutes(apiFetch: unknown, route: Route): string[] {
  const seen: string[] = [];
  (apiFetch as Mock).mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.toString() : (input as Request).url;
    seen.push(url);
    return route(url, init) ?? resp(404, { error: `unrouted in test: ${url}` });
  });
  return seen;
}

export function stubResizeObserver(): void {
  if (typeof (globalThis as { ResizeObserver?: unknown }).ResizeObserver === 'undefined') {
    class RO {
      observe(): void { /* noop */ }
      unobserve(): void { /* noop */ }
      disconnect(): void { /* noop */ }
    }
    (globalThis as { ResizeObserver?: unknown }).ResizeObserver = RO;
  }
}

export { vi };
