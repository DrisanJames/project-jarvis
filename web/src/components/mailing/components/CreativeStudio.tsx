import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faWandMagicSparkles,
  faRotate,
  faCircleCheck,
  faTriangleExclamation,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';

interface CreativeMeta {
  id: string;
  offer_key: string;
  brand_code: string;
  filename: string;
  subject: string;
  preheader: string;
  money_urls: number;
  tagged: boolean;
  source: string;
  forge_brand_key: string;
  sha256: string;
  generated_at: string;
  updated_at: string;
  html_bytes: number;
}

// Creative Studio — read surface over the mailing_creatives registry
// (synced from the operator's ReviewForge archive via
// `python -m agents.scheduling.cli forge-sync`). Browse by offer × brand,
// preview exactly the pipeline-built HTML a deploy would carry.
export const CreativeStudio: React.FC = () => {
  const [creatives, setCreatives] = useState<CreativeMeta[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [offerFilter, setOfferFilter] = useState('');
  const [brandFilter, setBrandFilter] = useState('');
  const [selected, setSelected] = useState<CreativeMeta | null>(null);
  const [previewHtml, setPreviewHtml] = useState<string | null>(null);
  const [previewLoading, setPreviewLoading] = useState(false);

  const fetchCreatives = useCallback(async () => {
    setLoading(true);
    try {
      const params = new URLSearchParams();
      if (offerFilter) params.set('offer', offerFilter);
      if (brandFilter) params.set('brand', brandFilter);
      const res = await apiFetch(`/api/mailing/creatives/?${params.toString()}`, {
        credentials: 'include',
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const json = await res.json();
      setCreatives(json.creatives ?? []);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  }, [offerFilter, brandFilter]);

  useEffect(() => {
    fetchCreatives();
  }, [fetchCreatives]);

  const openPreview = useCallback(async (c: CreativeMeta) => {
    setSelected(c);
    setPreviewHtml(null);
    setPreviewLoading(true);
    try {
      const res = await apiFetch(`/api/mailing/creatives/${c.id}/preview`, {
        credentials: 'include',
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      setPreviewHtml(await res.text());
    } catch (err) {
      setPreviewHtml(
        `<p style="color:#ef4444;font-family:monospace">preview failed: ${
          err instanceof Error ? err.message : String(err)
        }</p>`,
      );
    } finally {
      setPreviewLoading(false);
    }
  }, []);

  const brands = useMemo(
    () => Array.from(new Set(creatives.map((c) => c.brand_code))).sort(),
    [creatives],
  );

  return (
    <div style={{ padding: '24px', color: '#e5e7eb' }}>
      <h1 style={{ margin: 0, fontSize: 22, display: 'flex', alignItems: 'center', gap: 10 }}>
        <FontAwesomeIcon icon={faWandMagicSparkles} style={{ color: '#a78bfa' }} />
        Creative Studio
      </h1>
      <div style={{ fontSize: 12, color: '#94a3b8', marginTop: 4 }}>
        Send-day creative archive (ReviewForge + converter), pipeline-built per brand. Sync from
        the operator CLI: <code>python -m agents.scheduling.cli forge-sync</code>
      </div>

      <div style={{ display: 'flex', gap: 8, marginTop: 16, alignItems: 'center' }}>
        <input
          value={offerFilter}
          onChange={(e) => setOfferFilter(e.target.value)}
          placeholder="Filter by offer…"
          style={{
            background: '#1e293b', border: '1px solid #334155', borderRadius: 6,
            color: '#e5e7eb', padding: '6px 10px', fontSize: 13, width: 220,
          }}
        />
        <select
          value={brandFilter}
          onChange={(e) => setBrandFilter(e.target.value)}
          style={{
            background: '#1e293b', border: '1px solid #334155', borderRadius: 6,
            color: '#e5e7eb', padding: '6px 10px', fontSize: 13,
          }}
        >
          <option value="">All brands</option>
          {brands.map((b) => (
            <option key={b} value={b}>{b}</option>
          ))}
        </select>
        <button
          onClick={fetchCreatives}
          style={{
            background: '#312e81', border: '1px solid #4338ca', borderRadius: 6,
            color: '#e5e7eb', padding: '6px 12px', fontSize: 13, cursor: 'pointer',
            display: 'flex', alignItems: 'center', gap: 6,
          }}
        >
          <FontAwesomeIcon icon={faRotate} spin={loading} /> Refresh
        </button>
        <span style={{ fontSize: 12, color: '#94a3b8' }}>
          {creatives.length} creative{creatives.length === 1 ? '' : 's'}
        </span>
      </div>

      {error && (
        <div style={{ marginTop: 12, color: '#ef4444', fontSize: 13 }}>
          <FontAwesomeIcon icon={faTriangleExclamation} /> {error}
        </div>
      )}

      <div style={{ display: 'flex', gap: 16, marginTop: 16, alignItems: 'flex-start' }}>
        <div style={{ flex: '0 0 540px', maxHeight: '72vh', overflowY: 'auto' }}>
          <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
            <thead>
              <tr style={{ color: '#94a3b8', textAlign: 'left' }}>
                <th style={{ padding: '6px 8px' }}>Offer</th>
                <th style={{ padding: '6px 8px' }}>Brand</th>
                <th style={{ padding: '6px 8px' }}>Generated</th>
                <th style={{ padding: '6px 8px' }}>Checks</th>
              </tr>
            </thead>
            <tbody>
              {creatives.map((c) => (
                <tr
                  key={c.id}
                  onClick={() => openPreview(c)}
                  style={{
                    cursor: 'pointer',
                    background: selected?.id === c.id ? '#1e293b' : 'transparent',
                    borderTop: '1px solid #1f2937',
                  }}
                >
                  <td style={{ padding: '6px 8px' }} title={c.filename}>{c.offer_key}</td>
                  <td style={{ padding: '6px 8px' }}>{c.brand_code}</td>
                  <td style={{ padding: '6px 8px', color: '#94a3b8' }}>
                    {new Date(c.generated_at).toLocaleDateString()}
                  </td>
                  <td style={{ padding: '6px 8px' }}>
                    {c.tagged && c.money_urls > 0 ? (
                      <span style={{ color: '#22c55e' }}>
                        <FontAwesomeIcon icon={faCircleCheck} /> {c.money_urls} money
                      </span>
                    ) : (
                      <span style={{ color: '#f59e0b' }}>
                        <FontAwesomeIcon icon={faTriangleExclamation} />{' '}
                        {c.money_urls === 0 ? 'no money URL' : 'untagged'}
                      </span>
                    )}
                  </td>
                </tr>
              ))}
              {!loading && creatives.length === 0 && (
                <tr>
                  <td colSpan={4} style={{ padding: 16, color: '#94a3b8' }}>
                    Registry is empty — run <code>forge-sync</code> from the operator CLI.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>

        <div style={{ flex: 1, minWidth: 0 }}>
          {selected ? (
            <div>
              <div style={{ fontSize: 13, marginBottom: 8 }}>
                <div style={{ color: '#e5e7eb', fontWeight: 600 }}>{selected.subject}</div>
                <div style={{ color: '#94a3b8', marginTop: 2 }}>{selected.preheader}</div>
                <div style={{ color: '#64748b', marginTop: 2, fontFamily: 'monospace', fontSize: 11 }}>
                  {selected.filename} · {selected.source} · {(selected.html_bytes / 1024).toFixed(0)} KB ·
                  sha {selected.sha256.slice(0, 12)}…
                </div>
              </div>
              {previewLoading ? (
                <div style={{ color: '#94a3b8', fontSize: 13 }}>Loading preview…</div>
              ) : (
                <iframe
                  title="creative-preview"
                  sandbox=""
                  srcDoc={previewHtml ?? ''}
                  style={{
                    width: '100%', height: '68vh', background: '#fff',
                    border: '1px solid #334155', borderRadius: 8,
                  }}
                />
              )}
            </div>
          ) : (
            <div style={{ color: '#64748b', fontSize: 13, padding: 24 }}>
              Select a creative to preview the exact pipeline-built HTML (footer, tracking tags,
              preheader included).
            </div>
          )}
        </div>
      </div>
    </div>
  );
};
