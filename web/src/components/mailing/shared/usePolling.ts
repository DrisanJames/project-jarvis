// usePolling.ts — the anti-jank refresh pattern from the Delivery Queue.
//
// Why this exists: most portal screens call setLoading(true) on every poll and
// remount their content, which makes the UI "adjust" and flicker on each tick
// (the exact complaint about Campaign Center). OutboxDashboard avoids this by:
//   - gating the loading spinner to the FIRST paint only,
//   - replacing state atomically and ONLY on success,
//   - keeping the last good data mounted when a refresh fails (stale banner),
//   - ticking a 1s clock for "updated Ns ago" WITHOUT refetching.
// This hook packages that so every screen inherits it for free.

import { useCallback, useEffect, useRef, useState } from "react"

export interface PollingState<T> {
  data: T | null
  /** True only until the first successful (or failed) load resolves. */
  loading: boolean
  /** Last error message; data stays mounted so the screen never blanks. */
  error: string | null
  /** Seconds since the last successful load (for an "updated Ns ago" label). */
  secondsSinceUpdate: number
  /** True when the last load succeeded and is recent. */
  live: boolean
  /** Manual refresh (does not toggle `loading` after first paint). */
  refresh: () => void
}

export function usePolling<T>(
  fetcher: (signal: AbortSignal) => Promise<T>,
  intervalMs: number,
  deps: React.DependencyList = [],
): PollingState<T> {
  const [data, setData] = useState<T | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [tick, setTick] = useState(0) // 1s clock for the "ago" label
  const lastOkRef = useRef<number>(0)
  const firstDoneRef = useRef(false)
  const fetcherRef = useRef(fetcher)
  fetcherRef.current = fetcher

  const run = useCallback(async (signal: AbortSignal) => {
    try {
      const next = await fetcherRef.current(signal)
      if (signal.aborted) return
      // Atomic, success-only replacement — never blank the screen mid-flight.
      setData(next)
      setError(null)
      lastOkRef.current = Date.now()
    } catch (e) {
      if (signal.aborted) return
      // Keep last good data mounted; just surface the error.
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      if (!signal.aborted && !firstDoneRef.current) {
        firstDoneRef.current = true
        setLoading(false)
      }
    }
  }, [])

  const manualRef = useRef<() => void>(() => {})

  useEffect(() => {
    const ctrl = new AbortController()
    run(ctrl.signal)
    const poll = setInterval(() => {
      const c = new AbortController()
      run(c.signal)
    }, intervalMs)
    const clock = setInterval(() => setTick((t) => t + 1), 1000)
    manualRef.current = () => {
      const c = new AbortController()
      run(c.signal)
    }
    return () => {
      ctrl.abort()
      clearInterval(poll)
      clearInterval(clock)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [intervalMs, run, ...deps])

  const secondsSinceUpdate = lastOkRef.current ? Math.max(0, Math.round((Date.now() - lastOkRef.current) / 1000)) : 0
  // `tick` is referenced so the "ago" label re-renders each second.
  void tick
  const live = lastOkRef.current > 0 && secondsSinceUpdate < (intervalMs / 1000) * 3

  return {
    data,
    loading,
    error,
    secondsSinceUpdate,
    live,
    refresh: () => manualRef.current(),
  }
}
