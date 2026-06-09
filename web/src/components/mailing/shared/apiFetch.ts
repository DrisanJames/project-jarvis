import { getCurrentOrgId } from '../../../contexts/AuthContext';

/**
 * apiFetch is a drop-in replacement for the native `fetch(input, init?)` that
 * strengthens the multi-tenant foundation: every request it issues carries the
 * canonical `X-Organization-ID` header (sourced from AuthContext's module-level
 * org id) plus `credentials: 'include'`, and defaults `Content-Type:
 * application/json` when there is a request body and the caller did not set a
 * content type.
 *
 * Design notes:
 *  - This is a plain module function, NOT a React hook, so it cannot call
 *    useAuth(). It reads the active org id via getCurrentOrgId(), which the
 *    AuthProvider keeps in sync with the same `organization?.id` that
 *    hook-consuming components send today.
 *  - Caller-supplied headers are merged and WIN over the defaults (so an
 *    explicit Content-Type or X-Organization-ID from the caller is preserved).
 *  - If the org id is unavailable it still issues the request (no header),
 *    mirroring today's `if (orgId) headers[...] = orgId` fallback. It never
 *    throws on a missing org id.
 */
export async function apiFetch(
  input: RequestInfo | URL,
  init?: RequestInit
): Promise<Response> {
  // Normalize caller headers into a plain object we can merge predictably.
  const callerHeaders = new Headers(init?.headers);

  const merged = new Headers();

  // Default Content-Type to JSON only when there is a body, the caller didn't
  // set one, and the body is NOT a structured type the browser must own the
  // content type for (FormData needs its multipart boundary; Blob/streams carry
  // their own type). Setting application/json on those would corrupt the request.
  const body = init?.body;
  const bodyOwnsContentType =
    (typeof FormData !== 'undefined' && body instanceof FormData) ||
    (typeof Blob !== 'undefined' && body instanceof Blob) ||
    (typeof URLSearchParams !== 'undefined' && body instanceof URLSearchParams) ||
    (typeof ArrayBuffer !== 'undefined' && body instanceof ArrayBuffer) ||
    (typeof ReadableStream !== 'undefined' && body instanceof ReadableStream);

  if (body != null && !bodyOwnsContentType && !callerHeaders.has('Content-Type')) {
    merged.set('Content-Type', 'application/json');
  }

  // Canonical (uppercase) org header — only if the caller didn't already set it.
  if (!callerHeaders.has('X-Organization-ID')) {
    const orgId = getCurrentOrgId();
    if (orgId) {
      merged.set('X-Organization-ID', orgId);
    }
  }

  // Caller headers win over defaults.
  callerHeaders.forEach((value, key) => {
    merged.set(key, value);
  });

  return fetch(input, {
    credentials: 'include',
    ...init,
    headers: merged,
  });
}

export default apiFetch;
