import React, { useCallback, useEffect, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faCalendarCheck, faDownload, faSpinner, faShieldHalved, faFileImport,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import { colors, alpha, panelStyle, btnStyle } from '../shared/theme';
import { daysAgoDenver } from '../shared/filters';
import { SectionHeader, Pill } from '../shared/ui';

// =============================================================================
// SEND-DAY SCRUB — the Optizmo opt-out compliance loop (operator 2026-07-27)
// =============================================================================
// Lives on the Export & Import page. Before each send day:
//   1. Export the day's planned audience as MD5s (union of the "<tok> - "
//      campaigns' inclusion segments — the DB truth, streamed CSV).
//   2. Run the file through Optizmo.
//   3. Import the returned suppression MD5s here — they land in
//      mailing_global_suppressions (resolved back to subscriber emails) and
//      the send worker's global check picks them up before the send.
// Source: /api/mailing/send-day/{date}/audience-md5[/summary] +
// POST /scrub-suppressions (internal/api/send_day_scrub_handlers.go).

interface ScrubSummary {
  api_version: string;
  date: string;
  campaign_name_prefix: string;
  campaigns: number;
  segments: number;
  skipped_segment_refs: string[] | null;
  unique_md5s: number;
}

interface ScrubImportResult {
  api_version: string;
  date: string;
  source: string;
  received: number;
  unique_md5s: number;
  matched_md5s: number;
  suppressed_new: number;
  already_suppressed: number;
  unmatched_md5s: number;
  hub_reloaded: boolean;
  hub_count: number;
  hub_reload_error?: string;
}

const fmtNum = (n: number) => n.toLocaleString('en-US');

// Send days are DENVER days (design system §3.1 — no per-screen date math).
// The previous UTC form (`new Date(Date.now()+24h).toISOString()`) returned the
// day AFTER tomorrow once Denver passed ~18:00, so an evening scrub exported
// the wrong day's audience to Optizmo. daysAgoDenver(-1) is the DST-safe
// "tomorrow in Denver".
const tomorrowISO = (): string => daysAgoDenver(-1);

const inputStyle: React.CSSProperties = {
  background: colors.panelBgSolid,
  border: `1px solid ${alpha(colors.indigo500, '44')}`,
  color: colors.text,
  borderRadius: 8,
  padding: '8px 10px',
  fontSize: 12.5,
};

async function errorDetail(res: Response): Promise<string> {
  let msg = `HTTP ${res.status}`;
  try {
    const body = await res.json();
    if (body?.error) msg += ` — ${body.error}`;
  } catch { /* non-JSON */ }
  return msg;
}

export const SendDayScrubCard: React.FC = () => {
  const [date, setDate] = useState<string>(tomorrowISO());
  const [summary, setSummary] = useState<ScrubSummary | null>(null);
  const [summaryError, setSummaryError] = useState<string | null>(null);
  const [summaryLoading, setSummaryLoading] = useState(false);
  const [exporting, setExporting] = useState(false);
  const [exportError, setExportError] = useState<string | null>(null);
  const [pasted, setPasted] = useState('');
  const [importing, setImporting] = useState(false);
  const [importError, setImportError] = useState<string | null>(null);
  const [importResult, setImportResult] = useState<ScrubImportResult | null>(null);
  const fileRef = useRef<HTMLInputElement | null>(null);

  // Summary auto-loads per date — the operator's pre-flight numbers.
  useEffect(() => {
    if (!date) return;
    const ac = new AbortController();
    setSummaryLoading(true);
    setSummary(null);
    setSummaryError(null);
    (async () => {
      try {
        const res = await apiFetch(`/api/mailing/send-day/${date}/audience-md5/summary`, { signal: ac.signal });
        if (!res.ok) {
          setSummaryError(await errorDetail(res));
          return;
        }
        setSummary(await res.json());
      } catch (e: unknown) {
        if (!ac.signal.aborted) setSummaryError(e instanceof Error ? e.message : String(e));
      } finally {
        if (!ac.signal.aborted) setSummaryLoading(false);
      }
    })();
    return () => ac.abort();
  }, [date]);

  const doExport = useCallback(async () => {
    setExporting(true);
    setExportError(null);
    try {
      const res = await apiFetch(`/api/mailing/send-day/${date}/audience-md5`);
      if (!res.ok) {
        setExportError(await errorDetail(res));
        return;
      }
      const blob = await res.blob();
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = `${date}-audience-md5.csv`;
      document.body.appendChild(a);
      a.click();
      a.remove();
      URL.revokeObjectURL(url);
    } catch (e: unknown) {
      setExportError(e instanceof Error ? e.message : String(e));
    } finally {
      setExporting(false);
    }
  }, [date]);

  const doImport = useCallback(async (text: string) => {
    const md5s = text.split(/[\s,;]+/).map(s => s.trim()).filter(Boolean);
    if (md5s.length === 0) {
      setImportError('No MD5s found in the input.');
      return;
    }
    setImporting(true);
    setImportError(null);
    setImportResult(null);
    try {
      const res = await apiFetch(`/api/mailing/send-day/${date}/scrub-suppressions`, {
        method: 'POST',
        body: JSON.stringify({ md5s }),
      });
      if (!res.ok) {
        setImportError(await errorDetail(res));
        return;
      }
      setImportResult(await res.json());
      setPasted('');
    } catch (e: unknown) {
      setImportError(e instanceof Error ? e.message : String(e));
    } finally {
      setImporting(false);
    }
  }, [date]);

  const onFilePicked = useCallback((f: File | null) => {
    if (!f) return;
    const reader = new FileReader();
    reader.onload = () => void doImport(String(reader.result ?? ''));
    reader.readAsText(f);
  }, [doImport]);

  return (
    <div style={panelStyle}>
      <SectionHeader
        title="Send-Day Scrub (Optizmo opt-out loop)"
        icon={faShieldHalved}
        right={<Pill color={colors.indigo400}>global suppression</Pill>}
      />
      <div style={{ fontSize: 11.5, color: colors.textMuted, marginBottom: 12, lineHeight: 1.6 }}>
        Export the day&apos;s planned audience (union of the date&apos;s staged/scheduled campaigns&apos;
        inclusion segments) as MD5s, run it through Optizmo, then import the returned suppression
        list here — matches land in the global suppression store the send worker reads, BEFORE the send.
      </div>

      <div style={{ display: 'flex', gap: 14, flexWrap: 'wrap', alignItems: 'flex-end' }}>
        <div>
          <div style={{ fontSize: 11, color: colors.textMuted, marginBottom: 4 }}>Send date</div>
          <input type="date" value={date} onChange={e => setDate(e.target.value)} style={inputStyle} />
        </div>
        <button onClick={doExport} disabled={exporting || !summary} style={{
          ...btnStyle, display: 'flex', alignItems: 'center', gap: 8,
          opacity: exporting || !summary ? 0.55 : 1,
        }}>
          <FontAwesomeIcon icon={exporting ? faSpinner : faDownload} spin={exporting} />
          {exporting ? 'Streaming export…' : 'Export MD5s (CSV)'}
        </button>
        <div style={{ fontSize: 12, color: colors.textMuted, display: 'flex', alignItems: 'center', gap: 8, minHeight: 34 }}>
          <FontAwesomeIcon icon={summaryLoading ? faSpinner : faCalendarCheck} spin={summaryLoading} />
          {summaryLoading && <span>Resolving audience…</span>}
          {!summaryLoading && summary && (
            <span>
              <strong style={{ color: colors.text }}>{fmtNum(summary.unique_md5s)}</strong> unique MD5s
              · {fmtNum(summary.segments)} segments · {fmtNum(summary.campaigns)} campaigns
              · prefix <code style={{ fontFamily: 'monospace' }}>{summary.campaign_name_prefix}</code>
              {(summary.skipped_segment_refs?.length ?? 0) > 0 && (
                <span style={{ color: colors.warning }}> · {summary.skipped_segment_refs!.length} segment refs skipped</span>
              )}
            </span>
          )}
          {!summaryLoading && summaryError && (
            <span style={{ color: colors.warning }}>{summaryError}</span>
          )}
        </div>
      </div>

      {exportError && (
        <div style={{
          marginTop: 10, padding: '8px 12px', borderRadius: 8, fontSize: 12.5,
          background: alpha(colors.danger, '14'), border: `1px solid ${alpha(colors.danger, '44')}`, color: colors.danger,
        }}>
          Export failed: {exportError}
        </div>
      )}

      <div style={{ marginTop: 16 }}>
        <div style={{ fontSize: 11, color: colors.textMuted, marginBottom: 4 }}>
          Optizmo return — paste suppression MD5s (one per line) or pick the file. Any non-MD5 row
          refuses the whole import (fail-closed).
        </div>
        <textarea
          value={pasted}
          onChange={e => setPasted(e.target.value)}
          placeholder={'d41d8cd98f00b204e9800998ecf8427e\n…'}
          rows={4}
          style={{ ...inputStyle, width: '100%', fontFamily: 'monospace', fontSize: 11.5, resize: 'vertical' }}
        />
        <div style={{ display: 'flex', gap: 10, marginTop: 8, alignItems: 'center' }}>
          <button
            onClick={() => void doImport(pasted)}
            disabled={importing || pasted.trim() === ''}
            style={{ ...btnStyle, display: 'flex', alignItems: 'center', gap: 8, opacity: importing || pasted.trim() === '' ? 0.55 : 1 }}>
            <FontAwesomeIcon icon={importing ? faSpinner : faFileImport} spin={importing} />
            {importing ? 'Importing…' : 'Import suppressions'}
          </button>
          <input
            ref={fileRef}
            type="file"
            accept=".csv,.txt"
            onChange={e => { onFilePicked(e.target.files?.[0] ?? null); if (fileRef.current) fileRef.current.value = ''; }}
            style={{ fontSize: 11.5, color: colors.textMuted }}
          />
        </div>
      </div>

      {importError && (
        <div style={{
          marginTop: 10, padding: '8px 12px', borderRadius: 8, fontSize: 12.5,
          background: alpha(colors.danger, '14'), border: `1px solid ${alpha(colors.danger, '44')}`, color: colors.danger,
        }}>
          Import refused: {importError}
        </div>
      )}

      {importResult && (
        <div style={{
          marginTop: 10, padding: '10px 14px', borderRadius: 8, fontSize: 12.5, lineHeight: 1.7,
          background: alpha(colors.success, '14'), border: `1px solid ${alpha(colors.success, '44')}`,
          color: colors.successText,
        }}>
          <strong>Scrub landed for {importResult.date}</strong> (source {importResult.source}):{' '}
          {fmtNum(importResult.unique_md5s)} unique · {fmtNum(importResult.matched_md5s)} matched subscribers ·{' '}
          <strong>{fmtNum(importResult.suppressed_new)} newly suppressed</strong> ·{' '}
          {fmtNum(importResult.already_suppressed)} already suppressed · {fmtNum(importResult.unmatched_md5s)} unmatched.
          {importResult.hub_reloaded
            ? <span> Hub cache reloaded ({fmtNum(importResult.hub_count)} entries) — live on the send path.</span>
            : <span style={{ color: colors.warning }}> HUB RELOAD FAILED: {importResult.hub_reload_error} </span>}
        </div>
      )}
    </div>
  );
};
