import { useState, useEffect, useCallback, useRef } from 'react';

// Import date filter context types
interface DateRange {
  type: string;
  startDate: string;
  endDate: string;
  label: string;
}

interface ApiState<T> {
  data: T | null;
  loading: boolean;
  error: string | null;
}

interface UseApiOptions {
  pollingInterval?: number;
  enabled?: boolean;
  /** If true, adds date range params to the request. Default: false for backward compatibility */
  useDateFilter?: boolean;
  /** External date range to use (for components using the context) */
  dateRange?: DateRange | null;
}

// Helper to build URL with date range
const buildUrlWithDateRange = (baseUrl: string, dateRange: DateRange | null | undefined): string => {
  if (!dateRange) return baseUrl;
  const separator = baseUrl.includes('?') ? '&' : '?';
  return `${baseUrl}${separator}start_date=${dateRange.startDate}&end_date=${dateRange.endDate}&range_type=${dateRange.type}`;
};

export function useApi<T>(
  url: string,
  options: UseApiOptions = {}
): ApiState<T> & { refetch: () => Promise<void> } {
  const { pollingInterval, enabled = true, useDateFilter = false, dateRange } = options;
  const [state, setState] = useState<ApiState<T>>({
    data: null,
    loading: true,
    error: null,
  });

  const abortControllerRef = useRef<AbortController | null>(null);

  // Build the final URL with date range if needed
  const finalUrl = useDateFilter && dateRange ? buildUrlWithDateRange(url, dateRange) : url;

  const fetchData = useCallback(async () => {
    if (!enabled) return;

    // Cancel any in-flight request
    if (abortControllerRef.current) {
      abortControllerRef.current.abort();
    }

    abortControllerRef.current = new AbortController();

    try {
      const response = await fetch(finalUrl, {
        signal: abortControllerRef.current.signal,
        credentials: 'include',
      });

      if (!response.ok) {
        throw new Error(`HTTP error! status: ${response.status}`);
      }

      const data = await response.json();
      setState({ data, loading: false, error: null });
    } catch (error) {
      if (error instanceof Error && error.name === 'AbortError') {
        return; // Ignore aborted requests
      }
      setState((prev) => ({
        ...prev,
        loading: false,
        error: error instanceof Error ? error.message : 'Unknown error',
      }));
    }
  }, [finalUrl, enabled]);

  useEffect(() => {
    fetchData();

    // Set up polling if interval is specified
    let intervalId: ReturnType<typeof setInterval> | null = null;
    if (pollingInterval && enabled) {
      intervalId = setInterval(fetchData, pollingInterval);
    }

    return () => {
      if (abortControllerRef.current) {
        abortControllerRef.current.abort();
      }
      if (intervalId) {
        clearInterval(intervalId);
      }
    };
  }, [fetchData, pollingInterval, enabled]);

  return { ...state, refetch: fetchData };
}

export function useApiMutation<TData, TResponse>(url: string) {
  const [state, setState] = useState<{
    loading: boolean;
    error: string | null;
    data: TResponse | null;
  }>({
    loading: false,
    error: null,
    data: null,
  });

  const mutate = useCallback(
    async (data?: TData): Promise<TResponse | null> => {
      setState({ loading: true, error: null, data: null });

      try {
        const response = await fetch(url, {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
          },
          credentials: 'include',
          body: data ? JSON.stringify(data) : undefined,
        });

        if (!response.ok) {
          throw new Error(`HTTP error! status: ${response.status}`);
        }

        const responseData = await response.json();
        setState({ loading: false, error: null, data: responseData });
        return responseData;
      } catch (error) {
        const errorMessage =
          error instanceof Error ? error.message : 'Unknown error';
        setState({ loading: false, error: errorMessage, data: null });
        return null;
      }
    },
    [url]
  );

  return { ...state, mutate };
}
