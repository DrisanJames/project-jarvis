// DatasetKeysPanel — API-key management for one partner dataset, rendered
// inline under the Partners & Datasets table.
//
// Endpoints (NEW — may 404 until the backend deploys; every failure is shown,
// never swallowed):
//   GET  /api/mailing/data-partners/datasets/{id}/keys
//   POST /api/mailing/data-partners/datasets/{id}/rotate-key  → {api_key, api_key_warning}
//   POST /api/mailing/data-partners/keys/{keyId}/revoke
//
// The rotated key is shown ONCE with a copy button — only the prefix is stored
// server-side, matching the onboarding wizard's contract.

import React, { useCallback, useEffect, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faCopy, faExclamationTriangle, faKey, faSpinner, faTimes } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';

interface DatasetKey {
  id: string;
  key_prefix: string;
  status: string;
  last_used_at: string | null;
  created_at: string;
  revoked_at: string | null;
}

const fmt = (iso: string | null | undefined): string => {
  if (!iso) return '—';
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString();
};

const panel: React.CSSProperties = {
  marginTop: 14, background: 'rgba(15,30,60,0.35)',
  border: '1px solid rgba(99,102,241,0.25)', borderRadius: 10, padding: '14px 16px',
  color: 'rgba(220,235,250,0.92)',
};
const th: React.CSSProperties = {
  textAlign: 'left', padding: '8px 10px', fontSize: 11, letterSpacing: 0.5,
  color: 'rgba(180,210,240,0.65)', textTransform: 'uppercase', whiteSpace: 'nowrap',
};
const td: React.CSSProperties = {
  padding: '8px 10px', fontSize: 13, borderTop: '1px solid rgba(255,255,255,0.06)', whiteSpace: 'nowrap',
};
const ghostBtn: React.CSSProperties = {
  background: 'transparent', color: '#6366f1',
  border: '1px solid rgba(99,102,241,0.4)', padding: '4px 10px', borderRadius: 4,
  cursor: 'pointer', fontSize: 12,
};
const dangerBtn: React.CSSProperties = {
  background: 'rgba(239,68,68,0.12)', color: '#ef4444',
  border: '1px solid rgba(239,68,68,0.35)', padding: '4px 10px', borderRadius: 4,
  cursor: 'pointer', fontSize: 12,
};
const errBox: React.CSSProperties = {
  background: 'rgba(239,68,68,0.18)', border: '1px solid rgba(239,68,68,0.4)',
  padding: 10, borderRadius: 6, marginBottom: 10, fontSize: 12, color: '#fca5a5',
};

export const DatasetKeysPanel: React.FC<{
  datasetId: string;
  datasetName: string;
  onClose: () => void;
}> = ({ datasetId, datasetName, onClose }) => {
  const [keys, setKeys] = useState<DatasetKey[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [rotated, setRotated] = useState<{ api_key: string; warning: string } | null>(null);
  const [copied, setCopied] = useState(false);

  const load = useCallback(async () => {
    setLoading(true); setErr(null);
    try {
      const r = await apiFetch(`/api/mailing/data-partners/datasets/${datasetId}/keys`);
      let j: unknown = null;
      try { j = await r.json(); } catch { /* non-JSON */ }
      if (!r.ok) {
        const e = (j as { error?: unknown } | null)?.error;
        throw new Error(typeof e === 'string' ? e : `HTTP ${r.status}`);
      }
      // Contract: a bare array; tolerate {keys:[...]} too.
      const list = Array.isArray(j) ? j : (j as { keys?: DatasetKey[] } | null)?.keys ?? [];
      setKeys(list as DatasetKey[]);
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'load failed');
      setKeys(null);
    } finally { setLoading(false); }
  }, [datasetId]);

  useEffect(() => { setRotated(null); void load(); }, [load]);

  const rotate = async () => {
    if (busy) return;
    if (!window.confirm(
      `Rotate the API key for "${datasetName}"?\n\nA NEW key is generated and shown once. `
      + `The partner must switch to it — depending on server policy the old key may stop working.`,
    )) return;
    setBusy(true); setErr(null);
    try {
      const r = await apiFetch(`/api/mailing/data-partners/datasets/${datasetId}/rotate-key`, { method: 'POST' });
      let j: Record<string, unknown> = {};
      try { j = (await r.json()) as Record<string, unknown>; } catch { /* non-JSON */ }
      if (!r.ok) throw new Error(String(j.error ?? `HTTP ${r.status}`));
      setRotated({
        api_key: String(j.api_key ?? ''),
        warning: String(j.api_key_warning ?? 'Show this key to the partner ONCE — it cannot be retrieved later.'),
      });
      setCopied(false);
      await load();
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'rotate failed');
    } finally { setBusy(false); }
  };

  const revoke = async (k: DatasetKey) => {
    if (busy) return;
    if (!window.confirm(
      `REVOKE key ${k.key_prefix}… for "${datasetName}"?\n\nThe partner's POSTs with this key start `
      + `failing immediately. This cannot be undone.`,
    )) return;
    setBusy(true); setErr(null);
    try {
      const r = await apiFetch(`/api/mailing/data-partners/keys/${k.id}/revoke`, { method: 'POST' });
      let j: Record<string, unknown> = {};
      try { j = (await r.json()) as Record<string, unknown>; } catch { /* non-JSON */ }
      if (!r.ok) throw new Error(String(j.error ?? `HTTP ${r.status}`));
      await load();
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'revoke failed');
    } finally { setBusy(false); }
  };

  const copyKey = async () => {
    if (!rotated?.api_key) return;
    try {
      await navigator.clipboard.writeText(rotated.api_key);
      setCopied(true);
    } catch { /* clipboard denied — the key is still visible to copy manually */ }
  };

  return (
    <div style={panel}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 10 }}>
        <h3 style={{ margin: 0, fontSize: 14, color: '#dbeafe' }}>
          <FontAwesomeIcon icon={faKey} style={{ marginRight: 8, color: '#818cf8' }} />
          API keys — {datasetName}
        </h3>
        <button onClick={() => void rotate()} disabled={busy} style={{ ...dangerBtn, marginLeft: 'auto' }}>
          {busy ? <FontAwesomeIcon icon={faSpinner} spin /> : 'Rotate key…'}
        </button>
        <button onClick={onClose} style={ghostBtn} aria-label="Close keys panel">
          <FontAwesomeIcon icon={faTimes} /> Close
        </button>
      </div>

      {err && (
        <div style={errBox}>
          <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 6 }} />
          {err}{err.startsWith('HTTP 404') ? ' — the key-management endpoints may not be deployed yet.' : ''}
        </div>
      )}

      {rotated && rotated.api_key && (
        <div style={{
          background: 'rgba(245,158,11,0.12)', border: '1px solid rgba(245,158,11,0.35)',
          padding: 12, borderRadius: 6, marginBottom: 12,
        }}>
          <div style={{ marginBottom: 8, fontSize: 12, color: '#fcd34d', fontWeight: 600 }}>
            <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 6 }} />
            {rotated.warning}
          </div>
          <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
            <code style={{
              flex: 1, fontFamily: 'ui-monospace, monospace', fontSize: 13,
              background: 'rgba(0,0,0,0.35)', border: '1px solid rgba(255,255,255,0.12)',
              borderRadius: 4, padding: '8px 10px', wordBreak: 'break-all', whiteSpace: 'normal',
            }}>
              {rotated.api_key}
            </code>
            <button onClick={() => void copyKey()} style={ghostBtn}>
              <FontAwesomeIcon icon={faCopy} /> {copied ? 'Copied' : 'Copy'}
            </button>
          </div>
        </div>
      )}

      {loading && <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.6)' }}><FontAwesomeIcon icon={faSpinner} spin /> Loading keys…</div>}

      {!loading && keys && keys.length === 0 && (
        <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.55)' }}>
          No keys recorded for this dataset.
        </div>
      )}

      {!loading && keys && keys.length > 0 && (
        <table style={{ width: '100%', borderCollapse: 'collapse' }}>
          <thead>
            <tr>
              <th style={th}>Key prefix</th>
              <th style={th}>Status</th>
              <th style={th}>Created</th>
              <th style={th}>Last used</th>
              <th style={th}>Revoked</th>
              <th style={th} />
            </tr>
          </thead>
          <tbody>
            {keys.map((k) => {
              const revoked = k.status === 'revoked' || !!k.revoked_at;
              return (
                <tr key={k.id} style={{ opacity: revoked ? 0.55 : 1 }}>
                  <td style={{ ...td, fontFamily: 'ui-monospace, monospace' }}>{k.key_prefix}…</td>
                  <td style={{ ...td, color: revoked ? '#f59e0b' : '#10b981', fontWeight: 600 }}>{k.status}</td>
                  <td style={td}>{fmt(k.created_at)}</td>
                  <td style={td} title="Last time the partner authenticated with this key.">{fmt(k.last_used_at)}</td>
                  <td style={td}>{fmt(k.revoked_at)}</td>
                  <td style={td}>
                    {!revoked && (
                      <button onClick={() => void revoke(k)} disabled={busy} style={dangerBtn}>
                        Revoke…
                      </button>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}

      <p style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)', marginTop: 10, lineHeight: 1.6 }}>
        Only the key prefix is stored — a full key is shown exactly once, at rotation. Revoking is
        immediate and irreversible; rotate first, hand the partner the new key, then revoke the old
        one when they have switched.
      </p>
    </div>
  );
};

export default DatasetKeysPanel;
