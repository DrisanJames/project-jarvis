// DripCsvUpload — the "Ingest data" flow: pick a target feed/dataset, upload a
// CSV, preview (ISP composition + suggested column mapping with confidence),
// override the mapping manually, then commit.
//
// Endpoints (NEW — built in parallel; both may 404 until the backend deploys,
// and every failure renders as an explicit error, never as success):
//   POST /api/mailing/partner-ingest-csv/preview  (multipart file + dataset_id)
//   POST /api/mailing/partner-ingest-csv/commit   (multipart file + dataset_id + mapping JSON)
//
// Multipart note: apiFetch deliberately does NOT set Content-Type when the
// body is FormData (verified in shared/apiFetch.ts:36-45) — the browser owns
// the multipart boundary. Org header + credentials still apply.

import React, { useEffect, useMemo, useRef, useState } from 'react';
import { apiFetch } from '../shared/apiFetch';
import { colors, alpha, thStyle, tdStyle, tableStyle } from '../shared/theme';
import { Pill } from '../shared/ui';
import { labelForVertical } from '../datapartners/verticalLabels';

// ── Contract shapes ─────────────────────────────────────────────────────────

interface PreviewMappingEntry { column: string; confidence: number; }
interface PreviewResponse {
  headers: string[];
  suggested_mapping: Record<string, PreviewMappingEntry>;
  sample_rows: Array<Record<string, unknown> | unknown[]>;
  row_count: number;
  invalid_email_count: number;
  isp_breakdown: Array<{ isp: string; count: number; pct: number }>;
}
interface CommitResponse {
  batch_ids: string[];
  records: number;
  skipped_invalid: number;
}

export interface CsvFeedOption {
  dataset_id: string;
  name: string;
  vertical: string;
}

// Mapping targets always offered even when the preview suggests fewer — the
// operator can bind any of these manually.
const BASE_TARGETS = [
  'email', 'first_name', 'last_name', 'zip', 'city', 'state', 'phone',
  'opt_in_date', 'source_url', 'ip_address',
];

const UNKNOWN = '—';
const num = (n: number | null | undefined): string =>
  typeof n === 'number' && Number.isFinite(n) ? Math.round(n).toLocaleString() : UNKNOWN;

const input: React.CSSProperties = {
  background: 'rgba(255,255,255,0.06)', color: colors.text,
  border: `1px solid ${colors.panelBorderStrong}`, borderRadius: 5,
  padding: '5px 8px', fontSize: 12,
};
const smallBtn: React.CSSProperties = {
  background: alpha(colors.indigo500, '22'), color: colors.indigo200,
  border: `1px solid ${alpha(colors.indigo500, '66')}`, borderRadius: 5,
  padding: '5px 12px', fontSize: 11, fontWeight: 700, cursor: 'pointer',
};
const label: React.CSSProperties = {
  fontSize: 10, letterSpacing: 0.6, textTransform: 'uppercase', color: colors.textMuted,
};
const sectionTitle: React.CSSProperties = {
  fontSize: 11, color: colors.heading, fontWeight: 700, letterSpacing: 0.5,
  margin: '14px 0 8px 0',
};
const errBox: React.CSSProperties = {
  fontSize: 12, color: colors.dangerText,
  background: 'rgba(239,68,68,0.08)', border: '1px solid rgba(239,68,68,0.25)',
  borderRadius: 6, padding: '8px 10px', marginTop: 8,
};

const confidenceColor = (c: number): string =>
  c >= 0.85 ? colors.success : c >= 0.5 ? colors.warning : colors.danger;

export const DripCsvUpload: React.FC<{
  feeds: CsvFeedOption[];        // datasets already known from the supply payload
  onNotice: (s: string) => void;
  onCommitted?: () => void;      // fired after a successful commit (parent reloads its panels)
  onClose?: () => void;          // "Done" after a commit — closes the hosting shelf
}> = ({ feeds, onNotice, onCommitted, onClose }) => {
  // Supplement the supply-derived feed list with the full dataset roster so a
  // dataset outside the selected domain's supply is still targetable. Failure
  // here is non-fatal: the supply-derived list remains usable.
  const [extraFeeds, setExtraFeeds] = useState<CsvFeedOption[]>([]);
  const [rosterErr, setRosterErr] = useState<string | null>(null);
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const r = await apiFetch('/api/mailing/data-partners/datasets');
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        const j = (await r.json()) as { datasets?: Array<{ id: string; name: string; partner_name?: string; vertical: string }> };
        if (cancelled) return;
        setExtraFeeds((j.datasets ?? []).map((d) => ({
          dataset_id: d.id,
          name: d.partner_name ? `${d.partner_name} / ${d.name}` : d.name,
          vertical: d.vertical,
        })));
      } catch (e) {
        if (!cancelled) setRosterErr(e instanceof Error ? e.message : 'dataset roster fetch failed');
      }
    })();
    return () => { cancelled = true; };
  }, []);

  const options = useMemo(() => {
    const seen = new Set<string>();
    const out: CsvFeedOption[] = [];
    for (const f of [...feeds, ...extraFeeds]) {
      if (seen.has(f.dataset_id)) continue;
      seen.add(f.dataset_id);
      out.push(f);
    }
    return out.sort((a, b) => a.name.localeCompare(b.name));
  }, [feeds, extraFeeds]);

  const [datasetId, setDatasetId] = useState('');
  const [file, setFile] = useState<File | null>(null);
  const fileRef = useRef<HTMLInputElement | null>(null);

  const [previewing, setPreviewing] = useState(false);
  const [preview, setPreview] = useState<PreviewResponse | null>(null);
  const [previewErr, setPreviewErr] = useState<string | null>(null);

  // Manual mapping override: target -> column ('' = unmapped).
  const [mapping, setMapping] = useState<Record<string, string>>({});

  const [committing, setCommitting] = useState(false);
  const [commitErr, setCommitErr] = useState<string | null>(null);
  const [result, setResult] = useState<CommitResponse | null>(null);

  const resetFlow = () => {
    setPreview(null); setPreviewErr(null); setMapping({});
    setCommitErr(null); setResult(null);
  };

  const runPreview = async () => {
    if (!file || !datasetId || previewing) return;
    resetFlow();
    setPreviewing(true);
    try {
      const fd = new FormData();
      fd.append('file', file);
      fd.append('dataset_id', datasetId);
      const r = await apiFetch('/api/mailing/partner-ingest-csv/preview', { method: 'POST', body: fd });
      let j: Record<string, unknown> = {};
      try { j = (await r.json()) as Record<string, unknown>; } catch { /* non-JSON */ }
      if (!r.ok) throw new Error(String(j.error ?? `HTTP ${r.status}`));
      const p = j as unknown as PreviewResponse;
      setPreview(p);
      // Seed the manual mapping from the suggestions.
      const seeded: Record<string, string> = {};
      for (const [target, entry] of Object.entries(p.suggested_mapping ?? {})) {
        if (entry && typeof entry.column === 'string') seeded[target] = entry.column;
      }
      setMapping(seeded);
    } catch (e) {
      setPreviewErr(e instanceof Error ? e.message : 'preview failed');
    } finally {
      setPreviewing(false);
    }
  };

  const runCommit = async () => {
    if (!file || !datasetId || !preview || committing) return;
    const finalMapping: Record<string, string> = {};
    for (const [target, col] of Object.entries(mapping)) {
      if (col) finalMapping[target] = col;
    }
    if (!finalMapping.email) {
      setCommitErr('The email column must be mapped before committing.');
      return;
    }
    if (!window.confirm(
      `Commit ${num(preview.row_count)} row(s) into this dataset?\n\n`
      + `${num(preview.invalid_email_count)} row(s) have invalid emails and will be skipped.\n`
      + `Mapped columns: ${Object.entries(finalMapping).map(([t, c]) => `${t}←${c}`).join(', ')}`,
    )) return;
    setCommitting(true);
    setCommitErr(null);
    try {
      const fd = new FormData();
      fd.append('file', file);
      fd.append('dataset_id', datasetId);
      fd.append('mapping', JSON.stringify(finalMapping));
      const r = await apiFetch('/api/mailing/partner-ingest-csv/commit', { method: 'POST', body: fd });
      let j: Record<string, unknown> = {};
      try { j = (await r.json()) as Record<string, unknown>; } catch { /* non-JSON */ }
      if (!r.ok) throw new Error(String(j.error ?? `HTTP ${r.status}`));
      const res = j as unknown as CommitResponse;
      setResult(res);
      onNotice(`CSV committed — ${num(res.records)} record(s) ingested, ${num(res.skipped_invalid)} skipped, ${res.batch_ids?.length ?? 0} batch(es).`);
      onCommitted?.();
    } catch (e) {
      setCommitErr(e instanceof Error ? e.message : 'commit failed');
    } finally {
      setCommitting(false);
    }
  };

  const targets = useMemo(() => {
    const set = new Set<string>(BASE_TARGETS);
    for (const t of Object.keys(preview?.suggested_mapping ?? {})) set.add(t);
    return Array.from(set);
  }, [preview]);

  // Sample rows render defensively: rows may be objects keyed by header or
  // positional arrays — both are handled.
  const sampleCell = (row: Record<string, unknown> | unknown[], header: string, idx: number): string => {
    const v = Array.isArray(row) ? row[idx] : row[header];
    return v == null ? '' : String(v);
  };

  const ispTotal = (preview?.isp_breakdown ?? []).reduce((a, r) => a + (Number.isFinite(r.count) ? r.count : 0), 0);

  return (
    <div>
      <div style={{ fontSize: 11, color: colors.textMuted, marginBottom: 10, lineHeight: 1.6 }}>
        Uploads a CSV into a feed&apos;s dataset through the partner-ingest pipeline: preview shows the
        file&apos;s ISP composition and a suggested column mapping (with confidence); commit ingests
        through the same cleaning/EO gates as the partner API. Nothing mails directly from here.
      </div>

      {/* Step 1: target + file */}
      <div style={{ display: 'flex', gap: 10, alignItems: 'center', flexWrap: 'wrap' }}>
        <label style={{ fontSize: 11, color: colors.textMuted, display: 'flex', alignItems: 'center', gap: 6 }}>
          Target feed
          <select style={{ ...input, minWidth: 260 }} value={datasetId}
            onChange={(e) => { setDatasetId(e.target.value); resetFlow(); }}>
            <option value="">select a dataset…</option>
            {options.map((o) => (
              <option key={o.dataset_id} value={o.dataset_id}>{o.name} ({labelForVertical(o.vertical)})</option>
            ))}
          </select>
        </label>
        <input
          ref={fileRef}
          type="file"
          accept=".csv,text/csv"
          style={{ fontSize: 11, color: colors.textMuted }}
          onChange={(e) => { setFile(e.target.files?.[0] ?? null); resetFlow(); }}
        />
        <button type="button" style={smallBtn} disabled={!file || !datasetId || previewing}
          onClick={() => void runPreview()}>
          {previewing ? 'Previewing…' : 'Preview'}
        </button>
      </div>
      {options.length === 0 && (
        <div style={{ fontSize: 11, color: colors.warningText, marginTop: 6 }}>
          No datasets available to target{rosterErr ? ` — the dataset roster read failed (${rosterErr})` : ''}.
        </div>
      )}
      {previewErr && (
        <div style={errBox}>
          Preview failed: {previewErr}. If this is a 404, the CSV-ingest endpoints are not deployed yet.
        </div>
      )}

      {/* Step 2: preview */}
      {preview && (
        <>
          <div style={{ display: 'flex', gap: 18, flexWrap: 'wrap', marginTop: 12 }}>
            <div><div style={label}>Rows</div><div style={{ fontSize: 17, fontWeight: 700, fontVariantNumeric: 'tabular-nums', color: colors.text }}>{num(preview.row_count)}</div></div>
            <div>
              <div style={label}>Invalid emails</div>
              <div style={{ fontSize: 17, fontWeight: 700, fontVariantNumeric: 'tabular-nums', color: preview.invalid_email_count > 0 ? colors.warningText : colors.successText }}>
                {num(preview.invalid_email_count)}
              </div>
            </div>
            <div><div style={label}>Columns</div><div style={{ fontSize: 17, fontWeight: 700, fontVariantNumeric: 'tabular-nums', color: colors.text }}>{num(preview.headers.length)}</div></div>
          </div>

          {(preview.isp_breakdown ?? []).length > 0 && (
            <>
              <div style={sectionTitle}>ISP COMPOSITION</div>
              <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(230px, 1fr))', gap: '3px 18px' }}>
                {preview.isp_breakdown.slice().sort((a, b) => b.count - a.count).map((r) => (
                  <div key={r.isp} style={{ display: 'flex', alignItems: 'center', gap: 8 }}
                    title={`${r.isp}: ${num(r.count)} of ${num(ispTotal)} (${(r.pct ?? 0).toFixed(1)}%)`}>
                    <span style={{ fontSize: 11, color: colors.text, width: 76, flex: '0 0 auto' }}>{r.isp}</span>
                    <div style={{ flex: 1, height: 7, background: 'rgba(255,255,255,0.06)', borderRadius: 4 }}>
                      <div style={{ width: `${Math.max(r.count > 0 ? 2 : 0, Math.round(r.pct ?? 0))}%`, height: 7, background: alpha(colors.indigo400, '66'), borderRadius: 4 }} />
                    </div>
                    <span style={{ fontSize: 10, color: colors.textMuted, minWidth: 92, textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>
                      {num(r.count)} · {(r.pct ?? 0).toFixed(1)}%
                    </span>
                  </div>
                ))}
              </div>
            </>
          )}

          <div style={sectionTitle}>COLUMN MAPPING — suggested, override per target</div>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(250px, 1fr))', gap: '6px 18px' }}>
            {targets.map((target) => {
              const suggested = preview.suggested_mapping?.[target];
              const bound = mapping[target] ?? '';
              const overridden = suggested && bound !== suggested.column;
              return (
                <div key={target} style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                  <span style={{ fontSize: 11, color: target === 'email' ? colors.heading : colors.textMuted, width: 92, flex: '0 0 auto', fontWeight: target === 'email' ? 700 : 400 }}>
                    {target}{target === 'email' ? ' *' : ''}
                  </span>
                  <select style={{ ...input, flex: 1, padding: '3px 6px', fontSize: 11 }} value={bound}
                    onChange={(e) => setMapping((m) => ({ ...m, [target]: e.target.value }))}>
                    <option value="">(unmapped)</option>
                    {preview.headers.map((h) => <option key={h} value={h}>{h}</option>)}
                  </select>
                  {suggested && (
                    <span title={overridden
                      ? `Server suggested "${suggested.column}" at ${(suggested.confidence * 100).toFixed(0)}% confidence — you overrode it.`
                      : `Suggested by the server at ${(suggested.confidence * 100).toFixed(0)}% confidence.`}>
                      <Pill color={overridden ? colors.textFaint : confidenceColor(suggested.confidence)} style={{ fontSize: 8, padding: '0 6px' }}>
                        {overridden ? 'overridden' : `${(suggested.confidence * 100).toFixed(0)}%`}
                      </Pill>
                    </span>
                  )}
                </div>
              );
            })}
          </div>

          {(preview.sample_rows ?? []).length > 0 && (
            <>
              <div style={sectionTitle}>SAMPLE ROWS</div>
              <div style={{ overflowX: 'auto' }}>
                <table style={{ ...tableStyle, minWidth: 500 }}>
                  <thead>
                    <tr>{preview.headers.map((h) => <th key={h} style={thStyle}>{h}</th>)}</tr>
                  </thead>
                  <tbody>
                    {preview.sample_rows.slice(0, 5).map((row, i) => (
                      <tr key={i}>
                        {preview.headers.map((h, idx) => (
                          <td key={h} style={{ ...tdStyle, fontSize: 11, maxWidth: 180, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                            {sampleCell(row, h, idx)}
                          </td>
                        ))}
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </>
          )}

          {/* Step 3: commit */}
          {commitErr && <div style={errBox}>Commit failed: {commitErr}</div>}
          {!result ? (
            <div style={{ display: 'flex', gap: 8, marginTop: 14 }}>
              <button type="button"
                style={{ ...smallBtn, background: 'rgba(239,68,68,0.12)', color: colors.dangerText, border: '1px solid rgba(239,68,68,0.40)' }}
                disabled={committing || !mapping.email}
                title={mapping.email ? 'Ingests the file through the partner pipeline.' : 'Map the email column first.'}
                onClick={() => void runCommit()}>
                {committing ? 'Committing…' : `Commit ${num(preview.row_count)} rows…`}
              </button>
              <button type="button" style={{ ...smallBtn, background: 'transparent', color: colors.textMuted, border: `1px solid ${colors.hairline}` }}
                disabled={committing}
                onClick={() => { resetFlow(); setFile(null); if (fileRef.current) fileRef.current.value = ''; }}>
                Start over
              </button>
            </div>
          ) : (
            <div style={{
              marginTop: 14, fontSize: 12, color: colors.successText,
              background: alpha(colors.success, '14'), border: `1px solid ${alpha(colors.success, '44')}`,
              borderRadius: 6, padding: '10px 12px', lineHeight: 1.7,
            }}>
              <b>Committed.</b> {num(result.records)} record(s) ingested ·{' '}
              {num(result.skipped_invalid)} skipped (invalid).
              <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 4 }}>
                Batch id(s): {(result.batch_ids ?? []).map((id) => (
                  <code key={id} style={{ fontFamily: 'monospace', marginRight: 8 }}>{id}</code>
                ))}
                {(result.batch_ids ?? []).length === 0 && UNKNOWN}
              </div>
              <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 4 }}>
                Counts land in AVAILABLE DATA and COLD INTAKE after the slicer + EO validation run.
              </div>
              <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
                <button type="button" style={smallBtn}
                  onClick={() => {
                    // Full reset of the flow state — ready for the next file.
                    resetFlow();
                    setFile(null);
                    if (fileRef.current) fileRef.current.value = '';
                  }}>
                  Ingest another file
                </button>
                {onClose && (
                  <button type="button"
                    style={{ ...smallBtn, background: 'transparent', color: colors.textMuted, border: `1px solid ${colors.hairline}` }}
                    onClick={onClose}>
                    Done
                  </button>
                )}
              </div>
            </div>
          )}
        </>
      )}
    </div>
  );
};

export default DripCsvUpload;
