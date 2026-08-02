import React, { useCallback, useEffect, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faFileArrowUp, faSpinner, faPlus, faTableColumns, faCircleExclamation,
  faFileCsv, faCheck, faRotate,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import {
  colors, alpha, panelStyle, tableStyle, thStyle, tdStyle, numTd, numTh, btnStyle,
} from '../shared/theme';
import { SectionHeader, SectionError, EmptyState, ProgressBar, Pill } from '../shared/ui';

// =============================================================================
// LIST IMPORT PANEL — Export & Import screen (REQ-071)
// =============================================================================
// A THIN CLIENT of the server-side import service. Everything that could be
// "smart" happens on the server, on purpose (ruling R2/R3, BACKLOG 2026-08-01):
//
//   * CSV is NEVER parsed here. Headers, preview rows and the row estimate all
//     come from POST /import/preview.
//   * Header→field guessing is NEVER done here. There is no alias table in this
//     file; POST /import/validate owns that vocabulary (import_templates.go:312).
//   * The operator's reviewed mapping is serialized to the {field: column_index}
//     shape the import handler already unmarshals (mailing_import.go:46-47) and
//     posted as `field_mapping` — the thing the platform has never sent
//     (REQ-068), which is why imports have silently been read positionally.
//   * Nothing is reused from the Lists screen's import flow (ruling R3): that
//     component carries its own client CSV parser, and its ~45 class names are
//     all scoped under `.list-portal` in that screen's stylesheet, which does
//     not load in this chunk — rendered here it would come out unstyled. This
//     panel therefore uses shared/theme + shared/ui inline styles only, exactly
//     like the rest of the Export & Import screen (EOCleaning.tsx:7-11).
//
// Bound worth knowing: the per-column override list is built from the fields the
// SERVER recognised in this file's headers (the union of `suggested_field`).
// This panel is not allowed to call /import/fields, and it must not carry a
// field table of its own, so a column can only be re-pointed at a field some
// header in the same file already resolved to. Renaming the CSV header and
// re-selecting the file is the escape hatch, and the UI says so.
//
// State honesty (PORTAL_DESIGN_SYSTEM §1.6) — the target-list fetch models the
// same four states the page models for itself (EOCleaning.tsx:394-414):
// loading / error-with-retry / 404-not-on-this-build / genuinely-empty.

// ── The ONLY five endpoints this panel may touch (REQ-071 allowed list) ──────
const EP = {
  lists: '/api/mailing/lists',
  preview: '/api/mailing/import/preview',
  validate: '/api/mailing/import/validate',
  listImport: '/api/mailing/lists/{listId}/import',
  importJob: '/api/mailing/import-jobs/{jobId}',
} as const;

// The server REJECTS an upload over 32 MB (MaxImportFileBytes/MaxImportFileMB,
// mailing_import.go) with a 413 naming the limit — it no longer truncates.
// Keep this constant in step with the Go one; TestImportRejectsOversizeFileCeilingIsPinned
// fails if they drift. State the ceiling BEFORE the operator picks a file.
const MAX_IMPORT_MB = 32;
const MAX_IMPORT_BYTES = MAX_IMPORT_MB * 1024 * 1024;

const POLL_MS = 1500;

// ── API shapes (mirror the Go handlers; do not drift) ───────────────────────

interface ListRow { id: string; name: string; subscriber_count: number }

// POST /import/preview → import_templates.go:464-470
interface PreviewResponse {
  headers: string[];
  preview_rows: string[][] | null;
  total_estimate: number;
  column_count: number;
}

// POST /import/validate → import_templates.go:418-421
interface ValidateColumn {
  column_index: number;
  original_header: string;
  suggested_field: string | null;
  confidence: string;
  is_custom: boolean;
}
interface ValidateResponse {
  mapping: ValidateColumn[];
  validation: {
    valid: boolean;
    has_email: boolean;
    total_columns: number;
    mapped_columns: number;
    warnings: string[] | null;
    errors: string[] | null;
  };
}

// GET /import-jobs/{jobId} → mailing_import.go:602-607
interface ImportJob {
  id: string;
  list_id: string;
  filename: string;
  total_rows: number;
  processed_rows: number;
  progress_percent: number;
  imported_count: number;
  skipped_count: number;
  error_count: number;
  status: string;
  created_at: string;
  completed_at: string | null;
}

const TERMINAL_JOB_STATUSES = ['completed', 'failed', 'cancelled'];

// ── Local style objects (same inline idiom as EOCleaning.tsx:149-158) ────────

const inputStyle: React.CSSProperties = {
  background: 'rgba(15,23,42,0.6)', border: `1px solid ${colors.hairline}`,
  color: colors.text, borderRadius: 8, padding: '8px 10px', fontSize: 12.5,
  outline: 'none', width: '100%', boxSizing: 'border-box',
};

const labelStyle: React.CSSProperties = {
  fontSize: 10.5, color: colors.textMuted, textTransform: 'uppercase',
  letterSpacing: 0.5, marginBottom: 4, display: 'block',
};

const stepStyle: React.CSSProperties = {
  fontSize: 11, fontWeight: 700, color: colors.indigo300,
  textTransform: 'uppercase', letterSpacing: 0.6, marginBottom: 6,
};

const noteStyle: React.CSSProperties = {
  fontSize: 11.5, color: colors.textMuted, lineHeight: 1.6,
};

const boxStyle = (c: string): React.CSSProperties => ({
  marginTop: 10, padding: '8px 12px', borderRadius: 8, fontSize: 12.5, lineHeight: 1.6,
  background: alpha(c, '14'), border: `1px solid ${alpha(c, '44')}`, color: c,
});

const fmtInt = (n: number): string => n.toLocaleString();
const fmtMB = (bytes: number): string => `${(bytes / (1024 * 1024)).toFixed(1)} MB`;

// Read an error body without assuming it is JSON: HandlePreviewImport answers
// with http.Error (text/plain carrying a JSON string), the list handlers answer
// with respondError (real JSON). Both must render as one honest message.
const errText = async (res: Response): Promise<string> => {
  let msg = `HTTP ${res.status}`;
  try {
    const raw = await res.text();
    const trimmed = raw.trim();
    if (trimmed) {
      try {
        const parsed = JSON.parse(trimmed) as { error?: string };
        if (parsed?.error) return `${msg} — ${parsed.error}`;
      } catch { /* not JSON — fall through to the raw body */ }
      msg += ` — ${trimmed.slice(0, 200)}`;
    }
  } catch { /* body unreadable */ }
  return msg;
};

const asMessage = (e: unknown): string => (e instanceof Error ? e.message : String(e));

// ── Target-list fetch (the panel's own four-state source) ───────────────────

interface ListsState {
  loading: boolean;
  error: string | null;
  unavailable: boolean;   // 404 — endpoint not on this server build
  lists: ListRow[] | null;
  fetchedAt: string | null;
}

const useListsFetch = (): [ListsState, () => void, (l: ListRow) => void] => {
  const [state, setState] = useState<ListsState>({
    loading: true, error: null, unavailable: false, lists: null, fetchedAt: null,
  });
  const [nonce, setNonce] = useState(0);
  const refresh = useCallback(() => setNonce(n => n + 1), []);

  // Optimistic insert after a create, reconciled by the refresh that follows.
  const addLocal = useCallback((row: ListRow) => {
    setState(s => ({ ...s, lists: [row, ...(s.lists ?? [])] }));
  }, []);

  useEffect(() => {
    const ac = new AbortController();
    setState(s => ({ ...s, loading: s.lists == null, error: null }));
    apiFetch(EP.lists, { signal: ac.signal })
      .then(async res => {
        const fetchedAt = new Date().toLocaleTimeString();
        if (res.status === 404) {
          setState({ loading: false, error: null, unavailable: true, lists: null, fetchedAt });
          return;
        }
        if (!res.ok) {
          const msg = await errText(res);
          setState(s => ({ ...s, loading: false, unavailable: false, error: msg, fetchedAt }));
          return;
        }
        const data = (await res.json()) as { lists?: ListRow[] };
        setState({
          loading: false, error: null, unavailable: false,
          lists: data?.lists ?? [], fetchedAt,
        });
      })
      .catch((e: unknown) => {
        if (ac.signal.aborted) return;
        setState(s => ({
          ...s, loading: false, unavailable: false,
          error: asMessage(e), fetchedAt: new Date().toLocaleTimeString(),
        }));
      });
    return () => ac.abort();
  }, [nonce]);

  return [state, refresh, addLocal];
};

// ── Panel ───────────────────────────────────────────────────────────────────

export const ListImportPanel: React.FC = () => {
  const [listsState, refreshLists, addLocalList] = useListsFetch();

  // Step 1 — target list
  const [listId, setListId] = useState('');
  const [newListName, setNewListName] = useState('');
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);

  // Step 2/3 — file + server-derived preview
  const [file, setFile] = useState<File | null>(null);
  const [fileError, setFileError] = useState<string | null>(null);
  const [inspecting, setInspecting] = useState(false);
  const [preview, setPreview] = useState<PreviewResponse | null>(null);
  const [validation, setValidation] = useState<ValidateResponse | null>(null);

  // Step 4 — the operator's reviewed mapping: one target field per column.
  const [targets, setTargets] = useState<(string | null)[]>([]);

  // Step 5/6 — submit + job watch
  const [updateExisting, setUpdateExisting] = useState(true);
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [jobId, setJobId] = useState<string | null>(null);
  const [job, setJob] = useState<ImportJob | null>(null);
  const [jobError, setJobError] = useState<string | null>(null);
  const [watching, setWatching] = useState(false);

  const pollRef = useRef<number | null>(null);
  const stopPoll = useCallback(() => {
    if (pollRef.current != null) { window.clearTimeout(pollRef.current); pollRef.current = null; }
    setWatching(false);
  }, []);
  useEffect(() => () => { if (pollRef.current != null) window.clearTimeout(pollRef.current); }, []);

  const lists = listsState.lists;

  // ── Step 1 action: create a list, select it, reconcile ────────────────────
  const createList = async () => {
    const name = newListName.trim();
    setCreateError(null);
    if (!name) { setCreateError('Name the list first.'); return; }
    setCreating(true);
    try {
      const res = await apiFetch(EP.lists, { method: 'POST', body: JSON.stringify({ name }) });
      if (!res.ok) { setCreateError(await errText(res)); return; }
      const created = (await res.json()) as { id: string; name: string };
      // Patch every local projection of the row, then reconcile with the server.
      addLocalList({ id: created.id, name: created.name, subscriber_count: 0 });
      setListId(created.id);
      setNewListName('');
      refreshLists();
    } catch (e: unknown) {
      setCreateError(asMessage(e));
    } finally {
      setCreating(false);
    }
  };

  // ── Step 2/3 action: hand the file to the server, twice, and keep nothing ──
  const resetFileState = () => {
    setPreview(null); setValidation(null); setTargets([]);
    setJobId(null); setJob(null); setJobError(null); setSubmitError(null);
    stopPoll();
  };

  const inspectFile = async (picked: File) => {
    resetFileState();
    setFile(picked);
    setFileError(null);

    if (picked.size > MAX_IMPORT_BYTES) {
      setFileError(
        `${picked.name} is ${fmtMB(picked.size)} — over the ${MAX_IMPORT_MB} MB import ceiling. ` +
        `Split it into files of ${MAX_IMPORT_MB} MB or less; a larger file cannot be imported whole.`);
      return;
    }

    setInspecting(true);
    try {
      // (a) headers + preview rows + row estimate — server-parsed, always.
      const form = new FormData();
      form.append('file', picked);
      const pRes = await apiFetch(EP.preview, { method: 'POST', body: form });
      if (!pRes.ok) { setFileError(`preview: ${await errText(pRes)}`); return; }
      const pData = (await pRes.json()) as PreviewResponse;
      const headers = pData.headers ?? [];
      setPreview({ ...pData, preview_rows: pData.preview_rows ?? [] });

      // (b) suggested mapping for those headers — server vocabulary, always.
      const vRes = await apiFetch(EP.validate, {
        method: 'POST', body: JSON.stringify({ headers }),
      });
      if (!vRes.ok) { setFileError(`validate: ${await errText(vRes)}`); return; }
      const vData = (await vRes.json()) as ValidateResponse;
      setValidation(vData);
      setTargets(headers.map((_, i) => vData.mapping?.[i]?.suggested_field ?? null));
    } catch (e: unknown) {
      setFileError(asMessage(e));
    } finally {
      setInspecting(false);
    }
  };

  // ── Step 4 derived state: the effective, operator-reviewed mapping ────────
  // Vocabulary offered per column = the union of what the SERVER recognised in
  // this file. No field table lives in this component (ruling R3).
  const serverFields: string[] = Array.from(
    new Set((validation?.mapping ?? [])
      .map(m => m.suggested_field)
      .filter((f): f is string => !!f)),
  ).sort();

  const fieldMapping: Record<string, number> = {};
  const duplicateTargets: string[] = [];
  targets.forEach((f, i) => {
    if (!f) return;
    if (Object.prototype.hasOwnProperty.call(fieldMapping, f)) duplicateTargets.push(f);
    else fieldMapping[f] = i;
  });
  const mappedCount = Object.keys(fieldMapping).length;
  const emailMapped = Object.prototype.hasOwnProperty.call(fieldMapping, 'email');

  const blockingReason: string | null = (() => {
    if (!listId) return 'Pick or create a target list.';
    if (!file) return 'Choose a CSV file.';
    if (fileError) return 'Fix the file problem above.';
    if (!preview) return 'Waiting on the server preview.';
    if (!emailMapped) {
      return 'No column is mapped to email. Every row needs a resolvable email address — '
        + 'nothing can be imported without one.';
    }
    if (duplicateTargets.length > 0) {
      return `Two columns are mapped to the same field (${duplicateTargets.join(', ')}). `
        + 'Each field can come from only one column.';
    }
    return null;
  })();

  // ── Step 5 action: import with the mapping attached ───────────────────────
  const submit = async () => {
    if (blockingReason || !file || !listId) return;
    setSubmitError(null); setJobError(null); setJob(null);
    setSubmitting(true);
    try {
      const form = new FormData();
      form.append('file', file);
      form.append('field_mapping', JSON.stringify(fieldMapping));
      form.append('update_existing', updateExisting ? 'true' : 'false');
      const res = await apiFetch(EP.listImport.replace('{listId}', listId), {
        method: 'POST', body: form,
      });
      if (!res.ok) { setSubmitError(await errText(res)); return; }
      const data = (await res.json()) as { job_id?: string };
      if (!data?.job_id) {
        setSubmitError('The server accepted the upload but returned no job id — progress cannot be tracked.');
        return;
      }
      setJobId(data.job_id);
      setWatching(true);
      pollJob(data.job_id, 0);
    } catch (e: unknown) {
      setSubmitError(asMessage(e));
    } finally {
      setSubmitting(false);
    }
  };

  // ── Step 6: watch to a terminal state — and STOP on a failed read ─────────
  // A poll that swallows a 404/500 and re-arms is a spinner that spins forever;
  // this one surfaces the failure and stops watching.
  const pollJob = useCallback((id: string, attempt: number) => {
    apiFetch(EP.importJob.replace('{jobId}', id))
      .then(async res => {
        if (res.status === 404) {
          setJobError(
            'Import job not found. The upload was accepted, but its job row cannot be read — '
            + 'progress is unknown. Check the list\'s subscriber count before re-importing.');
          setWatching(false);
          return;
        }
        if (!res.ok) {
          setJobError(`job status: ${await errText(res)} — stopped watching after ${attempt + 1} attempt(s).`);
          setWatching(false);
          return;
        }
        const j = (await res.json()) as ImportJob;
        setJob(j);
        if (TERMINAL_JOB_STATUSES.includes(j.status)) { setWatching(false); return; }
        pollRef.current = window.setTimeout(() => pollJob(id, attempt + 1), POLL_MS);
      })
      .catch((e: unknown) => {
        setJobError(`job status: ${asMessage(e)} — stopped watching.`);
        setWatching(false);
      });
  }, []);

  // ── Render ────────────────────────────────────────────────────────────────

  const serverErrors = validation?.validation.errors ?? [];
  const serverWarnings = validation?.validation.warnings ?? [];

  return (
    <div style={panelStyle}>
      <SectionHeader
        title="Import a subscriber list"
        icon={faFileArrowUp}
        right={
          <span style={{ fontSize: 11, color: colors.textMuted }}>
            {listsState.fetchedAt ? `lists fetched ${listsState.fetchedAt}` : ''}
          </span>
        }
      />

      {/* ── STATE 1: loading ────────────────────────────────────────────── */}
      {listsState.loading && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, color: colors.textMuted, fontSize: 13, padding: '14px 4px' }}>
          <FontAwesomeIcon icon={faSpinner} spin /> Loading target lists…
        </div>
      )}

      {/* ── STATE 2: endpoint not on this build (404) ───────────────────── */}
      {!listsState.loading && listsState.unavailable && (
        <div style={boxStyle(colors.warning)}>
          <strong>SOURCE UNAVAILABLE.</strong> <code style={{ fontFamily: 'monospace' }}>{EP.lists}</code> is
          not exposed by this server build, so there is nothing to import into. Importing is disabled
          until that endpoint is deployed here.
        </div>
      )}

      {/* ── STATE 3: fetch failed, with a retry ─────────────────────────── */}
      {!listsState.loading && listsState.error && (
        <SectionError label="target lists" error={listsState.error} onRetry={refreshLists} />
      )}

      {!listsState.loading && !listsState.error && !listsState.unavailable && lists && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>

          {/* ── STEP 1 — target list ──────────────────────────────────── */}
          <div>
            <div style={stepStyle}>Step 1 · Target list</div>
            {/* ── STATE 4: genuinely empty — no lists exist yet ────────── */}
            {lists.length === 0 ? (
              <EmptyState
                icon={faPlus}
                title="No lists exist yet"
                hint="Name one below and create it — that becomes the import target."
              />
            ) : (
              <div style={{ display: 'flex', gap: 14, flexWrap: 'wrap', alignItems: 'flex-end' }}>
                <div style={{ minWidth: 340, flex: 1 }}>
                  <span style={labelStyle}>Import into</span>
                  <select value={listId} onChange={e => setListId(e.target.value)} style={inputStyle}>
                    <option value="">— pick a list —</option>
                    {lists.map(l => (
                      <option key={l.id} value={l.id}>{l.name} ({fmtInt(l.subscriber_count)})</option>
                    ))}
                  </select>
                </div>
                <button
                  type="button"
                  onClick={refreshLists}
                  style={{ ...btnStyle, whiteSpace: 'nowrap' }}
                  title="Re-read the list of lists"
                >
                  <FontAwesomeIcon icon={faRotate} /> Refresh lists
                </button>
              </div>
            )}
            <div style={{ display: 'flex', gap: 14, flexWrap: 'wrap', alignItems: 'flex-end', marginTop: 10 }}>
              <div style={{ minWidth: 280, flex: 1 }}>
                <span style={labelStyle}>…or create a new list</span>
                <input
                  value={newListName}
                  onChange={e => setNewListName(e.target.value)}
                  placeholder="e.g. Partner drop 2026-08-01"
                  style={inputStyle}
                />
              </div>
              <button
                type="button"
                onClick={createList}
                disabled={creating || !newListName.trim()}
                style={{
                  ...btnStyle,
                  opacity: creating || !newListName.trim() ? 0.5 : 1,
                  cursor: creating ? 'wait' : (newListName.trim() ? 'pointer' : 'not-allowed'),
                  whiteSpace: 'nowrap',
                }}
              >
                {creating ? <FontAwesomeIcon icon={faSpinner} spin /> : <FontAwesomeIcon icon={faPlus} />} Create list
              </button>
            </div>
            {createError && <div style={boxStyle(colors.danger)}>{createError}</div>}
          </div>

          {/* ── STEP 2 — the file, with the ceiling stated FIRST ───────── */}
          <div>
            <div style={stepStyle}>Step 2 · CSV file</div>
            <div style={{ ...noteStyle, marginBottom: 8 }}>
              CSV, UTF-8, first row must be the header row. <strong>Maximum {MAX_IMPORT_MB} MB per
              file</strong> — the server reads no further than that, so anything larger must be split
              before it is uploaded. Headers and preview come back from the server; nothing about this
              file is parsed in the browser.
            </div>
            <input
              type="file"
              accept=".csv,text/csv"
              disabled={inspecting}
              onChange={e => { const f = e.target.files?.[0]; if (f) inspectFile(f); }}
              style={{ ...inputStyle, padding: '7px 10px', cursor: inspecting ? 'wait' : 'pointer' }}
            />
            {file && !fileError && (
              <div style={{ ...noteStyle, marginTop: 6 }}>
                <FontAwesomeIcon icon={faFileCsv} style={{ color: colors.indigo400, marginRight: 6 }} />
                {file.name} · {fmtMB(file.size)} of {MAX_IMPORT_MB} MB
              </div>
            )}
            {inspecting && (
              <div style={{ display: 'flex', alignItems: 'center', gap: 10, color: colors.textMuted, fontSize: 12.5, marginTop: 8 }}>
                <FontAwesomeIcon icon={faSpinner} spin /> Reading headers and preview rows on the server…
              </div>
            )}
            {fileError && <div style={boxStyle(colors.danger)}>{fileError}</div>}
          </div>

          {/* ── STEP 3 — server-derived headers + preview rows ─────────── */}
          {preview && (
            <div>
              <div style={stepStyle}>Step 3 · What the server read</div>
              <div style={{ display: 'flex', gap: 20, flexWrap: 'wrap', marginBottom: 8 }}>
                <div>
                  <div style={labelStyle}>Columns</div>
                  <div style={{ fontSize: 18, fontWeight: 700, fontVariantNumeric: 'tabular-nums' }}>
                    {fmtInt(preview.column_count)}
                  </div>
                </div>
                <div>
                  <div style={labelStyle}>Data rows in file</div>
                  <div style={{ fontSize: 18, fontWeight: 700, fontVariantNumeric: 'tabular-nums' }}>
                    {fmtInt(preview.total_estimate)}
                  </div>
                </div>
                <div>
                  <div style={labelStyle}>Columns the server recognised</div>
                  <div style={{ fontSize: 18, fontWeight: 700, fontVariantNumeric: 'tabular-nums' }}>
                    {fmtInt(validation?.validation.mapped_columns ?? 0)} / {fmtInt(preview.column_count)}
                  </div>
                </div>
              </div>
              {(preview.preview_rows ?? []).length === 0 ? (
                <EmptyState
                  title="Header row only"
                  hint="The server found column headers but no data rows underneath them."
                />
              ) : (
                <div style={{ overflowX: 'auto' }}>
                  <table style={tableStyle}>
                    <thead>
                      <tr>
                        {preview.headers.map((h, i) => (
                          <th key={i} style={thStyle} title={h}>{h}</th>
                        ))}
                      </tr>
                    </thead>
                    <tbody>
                      {(preview.preview_rows ?? []).map((row, ri) => (
                        <tr key={ri}>
                          {preview.headers.map((_, ci) => (
                            <td key={ci} style={{ ...tdStyle, maxWidth: 200, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
                              title={row[ci] ?? ''}>
                              {row[ci] ?? ''}
                            </td>
                          ))}
                        </tr>
                      ))}
                    </tbody>
                  </table>
                  <div style={{ ...noteStyle, marginTop: 6 }}>
                    First {(preview.preview_rows ?? []).length} data rows, as the server's CSV reader
                    sees them.
                  </div>
                </div>
              )}
            </div>
          )}

          {/* ── STEP 4 — review + override the mapping ─────────────────── */}
          {preview && validation && (
            <div>
              <div style={stepStyle}>Step 4 · Column mapping — review and override</div>

              {/* has_email:false is an ERROR, not a warning: nothing imports without it. */}
              {serverErrors.length > 0 && (
                <div style={boxStyle(colors.danger)}>
                  <FontAwesomeIcon icon={faCircleExclamation} style={{ marginRight: 6 }} />
                  {serverErrors.join(' ')}
                </div>
              )}
              {serverWarnings.length > 0 && (
                <div style={boxStyle(colors.warning)}>{serverWarnings.join(' ')}</div>
              )}

              <div style={{ overflowX: 'auto', marginTop: 10 }}>
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={numTh}>#</th>
                      <th style={thStyle}>Header in file</th>
                      <th style={thStyle}>First value</th>
                      <th style={thStyle}>Server suggestion</th>
                      <th style={thStyle}>Import as</th>
                    </tr>
                  </thead>
                  <tbody>
                    {preview.headers.map((h, i) => {
                      const sugg = validation.mapping?.[i];
                      const chosen = targets[i] ?? '';
                      const sample = (preview.preview_rows ?? [])[0]?.[i] ?? '';
                      // Options = server suggestions for THIS file (+ whatever is
                      // already chosen), never a client-side field vocabulary.
                      const options = Array.from(new Set(
                        [...serverFields, ...(chosen ? [chosen] : [])],
                      )).sort();
                      return (
                        <tr key={i}>
                          <td style={numTd}>{i}</td>
                          <td style={{ ...tdStyle, fontFamily: 'monospace', fontSize: 11.5 }} title={h}>{h}</td>
                          <td style={{ ...tdStyle, color: colors.textMuted, maxWidth: 180, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
                            title={sample}>
                            {sample || '—'}
                          </td>
                          <td style={tdStyle}>
                            {sugg?.suggested_field
                              ? <Pill color={sugg.confidence === 'high' ? colors.success : colors.warning}>
                                  {sugg.suggested_field} · {sugg.confidence}
                                </Pill>
                              : <span style={{ color: colors.textFaint, fontSize: 11.5 }}>not recognised</span>}
                          </td>
                          <td style={tdStyle}>
                            <select
                              value={chosen}
                              onChange={e => setTargets(t => {
                                const next = [...t];
                                next[i] = e.target.value || null;
                                return next;
                              })}
                              style={{ ...inputStyle, minWidth: 180, padding: '5px 8px' }}
                            >
                              <option value="">— do not import —</option>
                              {options.map(f => <option key={f} value={f}>{f}</option>)}
                            </select>
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>

              <div style={{ ...noteStyle, marginTop: 8 }}>
                <FontAwesomeIcon icon={faTableColumns} style={{ color: colors.indigo400, marginRight: 6 }} />
                {mappedCount} of {preview.column_count} columns will be imported; the rest are dropped.
                The choices offered are the fields the server recognised from this file's own headers —
                to import a column the server did not recognise, rename that header in the CSV to the
                field name and re-select the file.
              </div>
              <div style={{ ...noteStyle, marginTop: 4, fontFamily: 'monospace', color: colors.textFaint }}>
                field_mapping = {JSON.stringify(fieldMapping)}
              </div>
            </div>
          )}

          {/* ── STEP 5 — import ────────────────────────────────────────── */}
          {preview && (
            <div>
              <div style={stepStyle}>Step 5 · Import</div>
              <label style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 12.5, color: colors.text, marginBottom: 10 }}>
                <input type="checkbox" checked={updateExisting} onChange={e => setUpdateExisting(e.target.checked)} />
                Update existing subscribers in this list (unchecked = leave them as they are)
              </label>
              <button
                type="button"
                onClick={submit}
                disabled={!!blockingReason || submitting}
                style={{
                  ...btnStyle,
                  opacity: blockingReason || submitting ? 0.5 : 1,
                  cursor: submitting ? 'wait' : (blockingReason ? 'not-allowed' : 'pointer'),
                }}
              >
                {submitting ? <FontAwesomeIcon icon={faSpinner} spin /> : <FontAwesomeIcon icon={faFileArrowUp} />}
                {' '}Import {fmtInt(preview.total_estimate)} rows
              </button>
              {blockingReason && (
                <div style={boxStyle(colors.danger)}>
                  <FontAwesomeIcon icon={faCircleExclamation} style={{ marginRight: 6 }} />
                  {blockingReason}
                </div>
              )}
              {submitError && <div style={boxStyle(colors.danger)}>{submitError}</div>}
            </div>
          )}

          {/* ── STEP 6 — job progress to a terminal state ──────────────── */}
          {jobId && (
            <div>
              <div style={stepStyle}>Step 6 · Import job</div>
              <div style={{ ...noteStyle, fontFamily: 'monospace', marginBottom: 8 }}>{jobId}</div>

              {jobError && <SectionError label="import job status" error={jobError} onRetry={() => {
                setJobError(null); setWatching(true); pollJob(jobId, 0);
              }} />}

              {!jobError && !job && watching && (
                <div style={{ display: 'flex', alignItems: 'center', gap: 10, color: colors.textMuted, fontSize: 12.5 }}>
                  <FontAwesomeIcon icon={faSpinner} spin /> Waiting for the first job status…
                </div>
              )}

              {job && (
                <div>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 8 }}>
                    <Pill color={
                      job.status === 'completed' ? colors.success
                        : job.status === 'failed' ? colors.danger
                          : colors.info
                    }>
                      {job.status.toUpperCase()}
                    </Pill>
                    {watching && <span style={{ fontSize: 11.5, color: colors.textMuted }}>
                      <FontAwesomeIcon icon={faSpinner} spin /> refreshing every {POLL_MS / 1000}s
                    </span>}
                    {!watching && TERMINAL_JOB_STATUSES.includes(job.status) && (
                      <span style={{ fontSize: 11.5, color: colors.textMuted }}>
                        <FontAwesomeIcon icon={faCheck} /> finished — no longer polling
                      </span>
                    )}
                  </div>
                  <ProgressBar pct={job.total_rows > 0 ? job.processed_rows / job.total_rows : 0} />
                  <div style={{ overflowX: 'auto', marginTop: 10 }}>
                    <table style={tableStyle}>
                      <thead>
                        <tr>
                          <th style={numTh}>Rows read</th>
                          <th style={numTh}>Processed</th>
                          <th style={numTh}>Imported</th>
                          <th style={numTh}>Skipped</th>
                          <th style={numTh}>Errors</th>
                        </tr>
                      </thead>
                      <tbody>
                        <tr>
                          <td style={numTd}>{fmtInt(job.total_rows)}</td>
                          <td style={numTd}>{fmtInt(job.processed_rows)}</td>
                          <td style={{ ...numTd, color: job.imported_count > 0 ? colors.successText : colors.textFaint }}>
                            {fmtInt(job.imported_count)}
                          </td>
                          <td style={numTd}>{fmtInt(job.skipped_count)}</td>
                          <td style={{ ...numTd, color: job.error_count > 0 ? colors.warning : colors.textFaint }}>
                            {fmtInt(job.error_count)}
                          </td>
                        </tr>
                      </tbody>
                    </table>
                  </div>
                  <div style={{ ...noteStyle, marginTop: 8 }}>
                    Rows read = rows the server's CSV reader returned · Imported = new + updated
                    subscribers · Skipped = no resolvable email or refused at ingest · Errors = rows
                    the writer rejected. Counts are the job row's own, read from{' '}
                    <code style={{ fontFamily: 'monospace' }}>{EP.importJob}</code>.
                  </div>
                  {TERMINAL_JOB_STATUSES.includes(job.status) && (
                    <button type="button" onClick={refreshLists} style={{ ...btnStyle, marginTop: 10 }}>
                      <FontAwesomeIcon icon={faRotate} /> Refresh list counts
                    </button>
                  )}
                </div>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  );
};

export default ListImportPanel;
