import React, { useState, useEffect, useCallback } from 'react';
import { useToast } from '../shared/ToastSystem';
import { apiFetch } from '../shared/apiFetch';

// Shared manual per-offer suppression panel: paste or upload one/many
// addresses straight onto an offer's suppression list
// (mailing_offer_suppressions). Used by the Offer Center Compliance tab and
// the Suppression Portal's Offer Suppression view.

const API = '/api/mailing/offer-center';

const btn: React.CSSProperties = {
  padding: '8px 16px', borderRadius: 8, border: 'none', cursor: 'pointer',
  fontSize: 12, fontWeight: 600, background: '#6366f1', color: '#fff',
};

const thStyle: React.CSSProperties = {
  textAlign: 'left', fontSize: 10, textTransform: 'uppercase', letterSpacing: 0.5,
  color: 'rgba(255,255,255,0.4)', padding: '6px 10px', borderBottom: '1px solid rgba(255,255,255,0.08)',
};

const tdStyle: React.CSSProperties = {
  fontSize: 12, color: '#e0e6f0', padding: '6px 10px', borderBottom: '1px solid rgba(255,255,255,0.04)',
};

export interface OfferSuppressionEntry {
  id: string;
  email: string;
  email_hash: string;
  reason: string;
  source: string;
  suppressed_at: string;
}

export const ManualOfferSuppressionPanel: React.FC<{ offerId: string }> = ({ offerId }) => {
  const { addToast } = useToast();
  const [pasteText, setPasteText] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [resultMsg, setResultMsg] = useState('');
  const [total, setTotal] = useState<number | null>(null);
  const [entries, setEntries] = useState<OfferSuppressionEntry[]>([]);
  const [expanded, setExpanded] = useState(false);

  const fetchEntries = useCallback(async () => {
    try {
      const res = await apiFetch(`${API}/offers/${offerId}/suppressions?limit=15`, { credentials: 'include' });
      if (res.ok) {
        const data = await res.json();
        setTotal(data.total ?? 0);
        setEntries(data.entries ?? []);
      }
    } catch { /* non-fatal */ }
  }, [offerId]);

  useEffect(() => { fetchEntries(); }, [fetchEntries]);

  const parseEmails = (text: string): string[] => {
    const out = new Set<string>();
    for (const token of text.split(/[\s,;]+/)) {
      const e = token.trim().toLowerCase().replace(/^["']|["']$/g, '');
      if (e && e.includes('@')) out.add(e);
    }
    return Array.from(out);
  };

  const submitEmails = async (emails: string[]) => {
    if (emails.length === 0) {
      addToast({ type: 'warning', title: 'No valid email addresses found' });
      return;
    }
    setSubmitting(true);
    setResultMsg('');
    let added = 0, matched = 0, requested = 0;
    const notFound: string[] = [];
    try {
      for (let i = 0; i < emails.length; i += 1000) {
        const batch = emails.slice(i, i + 1000);
        const res = await apiFetch(`${API}/offers/${offerId}/suppressions`, {
          method: 'POST', credentials: 'include',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ emails: batch, reason: 'manual', source: 'manual_upload' }),
        });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) {
          addToast({ type: 'error', title: 'Suppression upload failed', message: (data as { error?: string }).error || res.statusText });
          setSubmitting(false);
          return;
        }
        requested += data.requested ?? batch.length;
        added += data.added ?? 0;
        matched += data.matched ?? 0;
        notFound.push(...(data.not_found ?? []));
      }
      const parts = [`${requested.toLocaleString()} submitted`, `${matched.toLocaleString()} matched subscribers`, `${added.toLocaleString()} newly suppressed`];
      if (notFound.length > 0) parts.push(`${notFound.length.toLocaleString()} not in subscriber base`);
      setResultMsg(parts.join(' | ') + (notFound.length > 0 ? ` — not found: ${notFound.slice(0, 5).join(', ')}${notFound.length > 5 ? '…' : ''}` : ''));
      addToast({ type: 'success', title: 'Offer suppression updated', message: `${added.toLocaleString()} address${added === 1 ? '' : 'es'} added` });
      setPasteText('');
      fetchEntries();
    } catch {
      addToast({ type: 'error', title: 'Network error during suppression upload' });
    }
    setSubmitting(false);
  };

  const handleFile = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    const reader = new FileReader();
    reader.onload = () => submitEmails(parseEmails(String(reader.result || '')));
    reader.readAsText(file);
    e.target.value = '';
  };

  const removeEntry = async (email: string) => {
    try {
      const res = await apiFetch(`${API}/offers/${offerId}/suppressions?email=${encodeURIComponent(email)}`, {
        method: 'DELETE', credentials: 'include',
      });
      if (res.ok) {
        addToast({ type: 'success', title: 'Address removed from offer suppression' });
        fetchEntries();
      } else {
        addToast({ type: 'error', title: 'Failed to remove address' });
      }
    } catch { addToast({ type: 'error', title: 'Network error' }); }
  };

  return (
    <div style={{ marginBottom: 20, padding: 16, background: '#0d1526', borderRadius: 10, border: '1px solid rgba(255,255,255,0.06)' }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 8 }}>
        <div style={{ fontSize: 13, fontWeight: 600, color: '#e0e6f0' }}>Manual Address Suppression</div>
        {total !== null && (
          <span style={{ fontSize: 11, color: 'rgba(255,255,255,0.45)' }}>{total.toLocaleString()} total entries on this offer</span>
        )}
      </div>
      <div style={{ fontSize: 11, color: 'rgba(255,255,255,0.45)', marginBottom: 10 }}>
        Paste one or more addresses (newline / comma separated) or upload a .csv/.txt file. Addresses are matched
        against the subscriber base and blocked for this offer at planning and send time.
      </div>
      <textarea
        value={pasteText}
        onChange={e => setPasteText(e.target.value)}
        placeholder={'someone@example.com\nanother@example.com'}
        rows={3}
        style={{
          width: '100%', boxSizing: 'border-box', resize: 'vertical', fontSize: 12, fontFamily: 'monospace',
          background: '#0a0f1d', color: '#e0e6f0', border: '1px solid rgba(255,255,255,0.1)', borderRadius: 8, padding: 10,
        }}
        disabled={submitting}
      />
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginTop: 8 }}>
        <button
          style={{ ...btn, opacity: submitting || parseEmails(pasteText).length === 0 ? 0.5 : 1 }}
          disabled={submitting || parseEmails(pasteText).length === 0}
          onClick={() => submitEmails(parseEmails(pasteText))}
        >
          {submitting ? 'Suppressing…' : `Suppress ${parseEmails(pasteText).length || ''} Address${parseEmails(pasteText).length === 1 ? '' : 'es'}`}
        </button>
        <label style={{ fontSize: 12, color: 'rgba(255,255,255,0.65)', cursor: submitting ? 'not-allowed' : 'pointer' }}>
          or upload file:{' '}
          <input type="file" accept=".csv,.txt" onChange={handleFile} disabled={submitting} style={{ fontSize: 12, color: '#e0e6f0' }} />
        </label>
      </div>
      {resultMsg && (
        <div style={{ marginTop: 8, fontSize: 12, color: '#22c55e' }}>{resultMsg}</div>
      )}
      {entries.length > 0 && (
        <div style={{ marginTop: 12 }}>
          <button
            style={{ background: 'none', border: 'none', padding: 0, fontSize: 11, color: '#818cf8', cursor: 'pointer' }}
            onClick={() => setExpanded(x => !x)}
          >
            {expanded ? '▾ Hide' : '▸ Show'} recent entries
          </button>
          {expanded && (
            <table style={{ width: '100%', borderCollapse: 'collapse', marginTop: 8 }}>
              <thead>
                <tr>
                  <th style={thStyle}>Email</th>
                  <th style={thStyle}>Reason</th>
                  <th style={thStyle}>Source</th>
                  <th style={thStyle}>Suppressed</th>
                  <th style={thStyle} />
                </tr>
              </thead>
              <tbody>
                {entries.map(en => (
                  <tr key={en.id}>
                    <td style={{ ...tdStyle, fontFamily: 'monospace', fontSize: 11 }}>{en.email || en.email_hash}</td>
                    <td style={tdStyle}>{en.reason || '—'}</td>
                    <td style={tdStyle}>{en.source || '—'}</td>
                    <td style={tdStyle}>{en.suppressed_at ? new Date(en.suppressed_at).toLocaleString() : '—'}</td>
                    <td style={tdStyle}>
                      {en.email && (
                        <button
                          style={{ background: 'none', border: 'none', color: '#ef4444', cursor: 'pointer', fontSize: 11 }}
                          title="Remove from offer suppression"
                          onClick={() => removeEntry(en.email)}
                        >
                          remove
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      )}
    </div>
  );
};
