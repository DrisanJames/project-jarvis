import { renderHook, waitFor, act } from '@testing-library/react';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { useApi, useApiMutation } from './useApi';

describe('useApi', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('fetches data successfully', async () => {
    const mockData = { id: 1, name: 'Test' };
    
    (global.fetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve(mockData),
    });

    const { result } = renderHook(() => useApi<typeof mockData>('/api/test'));

    expect(result.current.loading).toBe(true);
    
    await waitFor(() => {
      expect(result.current.loading).toBe(false);
    });

    expect(result.current.data).toEqual(mockData);
    expect(result.current.error).toBeNull();
  });

  it('handles fetch error', async () => {
    (global.fetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      ok: false,
      status: 500,
    });

    const { result } = renderHook(() => useApi('/api/test'));

    await waitFor(() => {
      expect(result.current.loading).toBe(false);
    });

    expect(result.current.data).toBeNull();
    expect(result.current.error).toContain('HTTP error');
  });

  it('handles network error', async () => {
    (global.fetch as ReturnType<typeof vi.fn>).mockRejectedValueOnce(
      new Error('Network error')
    );

    const { result } = renderHook(() => useApi('/api/test'));

    await waitFor(() => {
      expect(result.current.loading).toBe(false);
    });

    expect(result.current.error).toBe('Network error');
  });

  it('does not fetch when disabled', async () => {
    const { result } = renderHook(() => 
      useApi('/api/test', { enabled: false })
    );

    // Should remain in loading state since fetch never happens
    expect(result.current.loading).toBe(true);
    expect(global.fetch).not.toHaveBeenCalled();
  });

  it('refetch works correctly', async () => {
    const mockData1 = { value: 1 };
    const mockData2 = { value: 2 };

    (global.fetch as ReturnType<typeof vi.fn>)
      .mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve(mockData1),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve(mockData2),
      });

    const { result } = renderHook(() => useApi<typeof mockData1>('/api/test'));

    await waitFor(() => {
      expect(result.current.data).toEqual(mockData1);
    });

    await act(async () => {
      await result.current.refetch();
    });

    expect(result.current.data).toEqual(mockData2);
  });
});

describe('useApiMutation', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('mutates data successfully', async () => {
    const mockResponse = { success: true };
    
    (global.fetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve(mockResponse),
    });

    const { result } = renderHook(() => 
      useApiMutation<{ name: string }, typeof mockResponse>('/api/test')
    );

    expect(result.current.loading).toBe(false);

    let mutationResult: typeof mockResponse | null = null;
    await act(async () => {
      mutationResult = await result.current.mutate({ name: 'Test' });
    });

    expect(mutationResult).toEqual(mockResponse);
    expect(result.current.data).toEqual(mockResponse);
    expect(result.current.error).toBeNull();
    
    expect(global.fetch).toHaveBeenCalledWith('/api/test', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
      },
      credentials: 'include',
      body: JSON.stringify({ name: 'Test' }),
    });
  });

  it('handles mutation error', async () => {
    (global.fetch as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      ok: false,
      status: 400,
    });

    const { result } = renderHook(() => 
      useApiMutation<{ name: string }, unknown>('/api/test')
    );

    await act(async () => {
      await result.current.mutate({ name: 'Test' });
    });

    expect(result.current.data).toBeNull();
    expect(result.current.error).toContain('HTTP error');
  });
});
