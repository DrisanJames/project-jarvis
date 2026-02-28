/**
 * API utilities with organization context
 * 
 * This module provides helper functions for making authenticated API calls
 * that include the organization context from the logged-in user's Google account.
 */

// =============================================================================
// TYPES
// =============================================================================

export interface ApiOptions extends Omit<RequestInit, 'headers'> {
  headers?: Record<string, string>;
}

// =============================================================================
// HELPER FUNCTIONS
// =============================================================================

/**
 * Get headers with organization context
 * Used for all authenticated API calls
 */
export const getApiHeaders = (organizationId?: string): Record<string, string> => {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
  };
  if (organizationId) {
    headers['X-Organization-ID'] = organizationId;
  }
  return headers;
};

/**
 * Make an authenticated API call with organization context
 * 
 * @param url - The API endpoint URL
 * @param organizationId - The organization ID from auth context
 * @param options - Additional fetch options
 * @returns Promise<Response>
 * 
 * @example
 * ```typescript
 * const { organization } = useAuth();
 * const response = await apiFetch('/api/mailing/campaigns', organization?.id);
 * const data = await response.json();
 * ```
 */
export const apiFetch = async (
  url: string,
  organizationId?: string,
  options: ApiOptions = {}
): Promise<Response> => {
  const headers = {
    ...getApiHeaders(organizationId),
    ...(options.headers || {}),
  };

  return fetch(url, {
    ...options,
    headers,
    credentials: 'include',
  });
};

/**
 * Make a GET request with organization context
 */
export const apiGet = async <T>(
  url: string,
  organizationId?: string
): Promise<T> => {
  const response = await apiFetch(url, organizationId);
  if (!response.ok) {
    throw new Error(`API Error: ${response.status}`);
  }
  return response.json();
};

/**
 * Make a POST request with organization context
 */
export const apiPost = async <T>(
  url: string,
  body: unknown,
  organizationId?: string
): Promise<T> => {
  const response = await apiFetch(url, organizationId, {
    method: 'POST',
    body: JSON.stringify(body),
  });
  if (!response.ok) {
    throw new Error(`API Error: ${response.status}`);
  }
  return response.json();
};

/**
 * Make a PUT request with organization context
 */
export const apiPut = async <T>(
  url: string,
  body: unknown,
  organizationId?: string
): Promise<T> => {
  const response = await apiFetch(url, organizationId, {
    method: 'PUT',
    body: JSON.stringify(body),
  });
  if (!response.ok) {
    throw new Error(`API Error: ${response.status}`);
  }
  return response.json();
};

/**
 * Make a DELETE request with organization context
 */
export const apiDelete = async (
  url: string,
  organizationId?: string
): Promise<void> => {
  const response = await apiFetch(url, organizationId, {
    method: 'DELETE',
  });
  if (!response.ok) {
    throw new Error(`API Error: ${response.status}`);
  }
};

// =============================================================================
// HOOKS (can be used in components)
// =============================================================================

// Note: For component-level use, prefer using the useAuth hook from contexts/AuthContext
// and passing organization?.id to these functions
