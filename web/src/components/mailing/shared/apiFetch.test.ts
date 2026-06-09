import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { apiFetch } from './apiFetch';
import { __setCurrentOrgId } from '../../../contexts/AuthContext';

// Helper: pull the RequestInit passed to the mocked fetch and read its headers
// as a plain map, regardless of whether they were provided as Headers or object.
function lastCallHeaders(): Record<string, string> {
  const mock = global.fetch as ReturnType<typeof vi.fn>;
  const init = mock.mock.calls[mock.mock.calls.length - 1][1] as RequestInit;
  const h = new Headers(init.headers);
  const out: Record<string, string> = {};
  h.forEach((v, k) => {
    out[k] = v;
  });
  return out;
}

describe('apiFetch', () => {
  beforeEach(() => {
    global.fetch = vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve({}) });
    __setCurrentOrgId('org-123');
  });

  afterEach(() => {
    __setCurrentOrgId(null);
    vi.restoreAllMocks();
  });

  it('attaches the canonical X-Organization-ID header from AuthContext', async () => {
    await apiFetch('/api/mailing/thing');
    const headers = lastCallHeaders();
    expect(headers['x-organization-id']).toBe('org-123');
  });

  it('always sends credentials: include', async () => {
    await apiFetch('/api/mailing/thing');
    const mock = global.fetch as ReturnType<typeof vi.fn>;
    const init = mock.mock.calls[0][1] as RequestInit;
    expect(init.credentials).toBe('include');
  });

  it('merges caller-supplied headers alongside the org header', async () => {
    await apiFetch('/api/mailing/thing', { headers: { 'X-Custom': 'abc' } });
    const headers = lastCallHeaders();
    expect(headers['x-custom']).toBe('abc');
    expect(headers['x-organization-id']).toBe('org-123');
  });

  it('lets a caller-supplied X-Organization-ID win over the default', async () => {
    await apiFetch('/api/mailing/thing', { headers: { 'X-Organization-ID': 'override' } });
    const headers = lastCallHeaders();
    expect(headers['x-organization-id']).toBe('override');
  });

  it('defaults Content-Type to application/json when a body is present', async () => {
    await apiFetch('/api/mailing/thing', { method: 'POST', body: JSON.stringify({ a: 1 }) });
    const headers = lastCallHeaders();
    expect(headers['content-type']).toBe('application/json');
  });

  it('does not set Content-Type when there is no body', async () => {
    await apiFetch('/api/mailing/thing');
    const headers = lastCallHeaders();
    expect(headers['content-type']).toBeUndefined();
  });

  it('preserves a caller-supplied Content-Type', async () => {
    await apiFetch('/api/mailing/thing', {
      method: 'POST',
      body: 'raw',
      headers: { 'Content-Type': 'text/plain' },
    });
    const headers = lastCallHeaders();
    expect(headers['content-type']).toBe('text/plain');
  });

  it('does not force JSON Content-Type on FormData bodies', async () => {
    const fd = new FormData();
    fd.append('file', 'x');
    await apiFetch('/api/mailing/upload', { method: 'POST', body: fd });
    const headers = lastCallHeaders();
    expect(headers['content-type']).toBeUndefined();
    // org header still attached on uploads
    expect(headers['x-organization-id']).toBe('org-123');
  });

  it('still issues the request (without the header) when no org id is set', async () => {
    __setCurrentOrgId(null);
    await apiFetch('/api/mailing/thing');
    const headers = lastCallHeaders();
    expect(headers['x-organization-id']).toBeUndefined();
    expect(global.fetch).toHaveBeenCalled();
  });
});
