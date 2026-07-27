import React, { useCallback, useEffect, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faBroom, faRotate, faSpinner, faPlay, faPause, faPlus,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import {
  colors, alpha, pageStyle, panelStyle, tableStyle,
  thStyle, tdStyle, numTd, numTh, btnStyle,
} from '../shared/theme';
import { SectionHeader, Pill, SectionError, EmptyState, ProgressBar } from '../shared/ui';

// =============================================================================
// EO CLEANING — ad-hoc EmailOversight cleaning jobs
// =============================================================================
// Operator ask (2026-07-26): "Somewhere in the platform I should be able to
// upload a list and have it cleaned. Or tell the platform to clean a
// particular list or segment from the segment command."
//
// Source: /api/mailing/eo-clean/* (internal/api/eo_clean_handlers.go).
// Jobs are drained by EOCleanJobWorker (internal/worker/eo_clean_jobs.go);
// verdicts land in mailing_eo_validation — the gate every fresh draw reads.
// COST: EmailOversight validation is billed PER RECORD. Already-Verified
// emails are skipped at enqueue (already_clean) — the platform never pays
// twice. The confirm dialog quotes the record count before any POST.
//
// State honesty (§1.6): loading, fetch-error-with-retry, endpoint-not-on-
// this-build (404 = deploy held), and genuinely-empty are all distinct.

const PAGE_VERSION = '1.0';

// ── API shapes (mirror eo_clean_handlers.go; do not drift) ──────────────────

interface CleanJob {
  id: string;
  source_type: 'upload' | 'segment' | 'list';
  source_ref: string;
  label: string;
  status: 'queued' | 'running' | 'paused' | 'done' | 'failed';
  total_count: number;
  already_clean_count: number;
  queued_count: number;
  validated_count: number;
  verified_count: number;
  complainer_count: number;
  undeliverable_count: number;
  other_count: number;
  failed_count: number;
  daily_cap: number;
  created_by: string;
  created_at: string;
  finished_at: string | null;
}

interface JobsResponse {
  api_version: string;
  jobs: CleanJob[];
}

interface PickerSegment { id: string; name: string; subscriber_count: number }
interface PickerList { id: string; name: string; subscriber_count: number }

// ── Fetch plumbing (SegmentationCommand's single-endpoint hook shape) ───────

interface FetchState {
  loading: boolean;
  error: string | null;
  unavailable: boolean; // 404 — endpoint not on this server build
  jobs: CleanJob[] | null;
  fetchedAt: string | null;
}

const useJobsFetch = (nonce: number): [FetchState, () => void] => {
  const [state, setState] = useState<FetchState>({
    loading: true, error: null, unavailable: false, jobs: null, fetchedAt: null,
  });
  const [localNonce, setLocalNonce] = useState(0);
  const retry = useCallback(() => setLocalNonce(n => n + 1), []);
  const abortRef = useRef<AbortController | null>(null);

  useEffect(() => {
    abortRef.current?.abort();
    const ac = new AbortController();
    abortRef.current = ac;
    setState(s => ({ ...s, loading: s.jobs == null, error: null }));
    apiFetch('/api/mailing/eo-clean/jobs', { signal: ac.signal })
      .then(async res => {
        const fetchedAt = new Date().toLocaleTimeString();
        if (res.status === 404) {
          setState({ loading: false, error: null, unavailable: true, jobs: null, fetchedAt });
          return;
        }
        if (!res.ok) {
          let msg = `HTTP ${res.status}`;
          try {
            const body = await res.json();
            if (body?.error) msg += ` — ${body.error}`;
          } catch { /* non-JSON error body */ }
          setState(s => ({ ...s, loading: false, error: msg, unavailable: false, fetchedAt }));
          return;
        }
        const data = (await res.json()) as JobsResponse;
        setState({ loading: false, error: null, unavailable: false, jobs: data.jobs, fetchedAt });
      })
      .catch((e: unknown) => {
        if (ac.signal.aborted) return;
        setState(s => ({
          ...s, loading: false, unavailable: false,
          error: e instanceof Error ? e.message : String(e),
          fetchedAt: new Date().toLocaleTimeString(),
        }));
      });
    return () => ac.abort();
  }, [nonce, localNonce]);

  return [state, retry];
};

// ── Display helpers ─────────────────────────────────────────────────────────

const fmtInt = (n: number): string => n.toLocaleString();

const statusColor = (s: CleanJob['status']): string => {
  switch (s) {
    case 'done': return colors.success;
    case 'running': return colors.info;
    case 'queued': return colors.indigo400;
    case 'paused': return colors.warning;
    case 'failed': return colors.danger;
  }
};

// Progress = terminal items over the payable pool (total minus already-clean).
const jobProgress = (j: CleanJob): number => {
  const payable = j.total_count - j.already_clean_count;
  if (payable <= 0) return 1;
  return (j.validated_count + j.failed_count) / payable;
};

const inputStyle: React.CSSProperties = {
  background: 'rgba(15,23,42,0.6)', border: `1px solid ${colors.hairline}`,
  color: colors.text, borderRadius: 8, padding: '8px 10px', fontSize: 12.5,
  outline: 'none', width: '100%', boxSizing: 'border-box',
};

const labelStyle: React.CSSProperties = {
  fontSize: 10.5, color: colors.textMuted, textTransform: 'uppercase',
  letterSpacing: 0.5, marginBottom: 4, display: 'block',
};

const COST_NOTE = 'EmailOversight validation has a per-record cost. Emails already Verified in mailing_eo_validation are skipped and never re-billed.';

// ── New Job form ────────────────────────────────────────────────────────────

const NewJobForm: React.FC<{ onCreated: () => void }> = ({ onCreated }) => {
  const [sourceType, setSourceType] = useState<'segment' | 'list' | 'upload'>('segment');
  const [segments, setSegments] = useState<PickerSegment[] | null>(null);
  const [lists, setLists] = useState<PickerList[] | null>(null);
  const [pickerError, setPickerError] = useState<string | null>(null);
  const [sourceRef, setSourceRef] = useState('');
  const [label, setLabel] = useState('');
  const [dailyCap, setDailyCap] = useState('');
  const [emailsRaw, setEmailsRaw] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [message, setMessage] = useState<{ ok: boolean; text: string } | null>(null);

  // Segment/list pickers ride the existing endpoints; loaded once on mount.
  useEffect(() => {
    apiFetch('/api/mailing/segments')
      .then(res => res.json())
      .then(d => setSegments((d?.segments ?? []) as PickerSegment[]))
      .catch((e: unknown) => setPickerError(`segments: ${e instanceof Error ? e.message : String(e)}`));
    apiFetch('/api/mailing/lists')
      .then(res => res.json())
      .then(d => setLists((d?.lists ?? []) as PickerList[]))
      .catch((e: unknown) => setPickerError(`lists: ${e instanceof Error ? e.message : String(e)}`));
  }, []);

  const pastedEmails = emailsRaw.split(/[\s,;]+/).map(s => s.trim()).filter(Boolean);

  const recordEstimate = ((): { n: number; what: string } | null => {
    if (sourceType === 'upload') return { n: pastedEmails.length, what: 'pasted emails' };
    if (sourceType === 'segment') {
      const s = segments?.find(x => x.id === sourceRef);
      return s ? { n: s.subscriber_count, what: `members of "${s.name}"` } : null;
    }
    const l = lists?.find(x => x.id === sourceRef);
    return l ? { n: l.subscriber_count, what: `subscribers of "${l.name}"` } : null;
  })();

  const submit = async () => {
    setMessage(null);
    if (sourceType !== 'upload' && !sourceRef) {
      setMessage({ ok: false, text: `Pick a ${sourceType} first.` });
      return;
    }
    if (sourceType === 'upload' && pastedEmails.length === 0) {
      setMessage({ ok: false, text: 'Paste at least one email.' });
      return;
    }
    const cap = dailyCap.trim() === '' ? 0 : Number(dailyCap);
    if (!Number.isFinite(cap) || cap < 0) {
      setMessage({ ok: false, text: 'Daily cap must be a non-negative number (0 or blank = uncapped).' });
      return;
    }
    const n = recordEstimate?.n ?? 0;
    if (!window.confirm(
      `Queue EO cleaning for ${fmtInt(n)} ${recordEstimate?.what ?? 'records'}?\n\n${COST_NOTE}`)) {
      return;
    }
    setSubmitting(true);
    try {
      const body: Record<string, unknown> = {
        source_type: sourceType,
        label: label.trim(),
        daily_cap: cap,
      };
      if (sourceType === 'upload') body.emails = pastedEmails;
      else body.source_ref = sourceRef;
      const res = await apiFetch('/api/mailing/eo-clean/jobs', { method: 'POST', body: JSON.stringify(body) });
      const data = await res.json();
      if (!res.ok) {
        setMessage({ ok: false, text: data?.error ?? `HTTP ${res.status}` });
        return;
      }
      const job = data.job as CleanJob;
      setMessage({
        ok: true,
        text: `Job queued: ${fmtInt(job.queued_count)} to validate, ${fmtInt(job.already_clean_count)} already clean (skipped).`,
      });
      setEmailsRaw(''); setLabel(''); setDailyCap(''); setSourceRef('');
      onCreated();
    } catch (e: unknown) {
      setMessage({ ok: false, text: e instanceof Error ? e.message : String(e) });
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div style={panelStyle}>
      <SectionHeader title="New cleaning job" icon={faPlus} />
      <div style={{ display: 'flex', gap: 14, flexWrap: 'wrap', alignItems: 'flex-end' }}>
        <div style={{ minWidth: 130 }}>
          <span style={labelStyle}>Source</span>
          <select
            value={sourceType}
            onChange={e => { setSourceType(e.target.value as 'segment' | 'list' | 'upload'); setSourceRef(''); setMessage(null); }}
            style={inputStyle}
          >
            <option value="segment">Segment</option>
            <option value="list">List</option>
            <option value="upload">Paste emails</option>
          </select>
        </div>
        {sourceType !== 'upload' && (
          <div style={{ minWidth: 320, flex: 1 }}>
            <span style={labelStyle}>{sourceType === 'segment' ? 'Segment' : 'List'}</span>
            <select value={sourceRef} onChange={e => setSourceRef(e.target.value)} style={inputStyle}>
              <option value="">
                {(sourceType === 'segment' ? segments : lists) == null ? 'Loading…' : `— pick a ${sourceType} —`}
              </option>
              {(sourceType === 'segment' ? segments : lists)?.map(x => (
                <option key={x.id} value={x.id}>
                  {x.name} ({fmtInt(x.subscriber_count)})
                </option>
              ))}
            </select>
          </div>
        )}
        <div style={{ minWidth: 200 }}>
          <span style={labelStyle}>Label (optional)</span>
          <input value={label} onChange={e => setLabel(e.target.value)} placeholder="defaults to source name" style={inputStyle} />
        </div>
        <div style={{ minWidth: 140 }}>
          <span style={labelStyle}>Daily cap (records/day)</span>
          <input value={dailyCap} onChange={e => setDailyCap(e.target.value)} placeholder="0 = uncapped" style={inputStyle} />
        </div>
        <button onClick={submit} disabled={submitting} style={{ ...btnStyle, opacity: submitting ? 0.6 : 1, cursor: submitting ? 'wait' : 'pointer' }}>
          {submitting ? <FontAwesomeIcon icon={faSpinner} spin /> : <FontAwesomeIcon icon={faBroom} />} Queue cleaning
        </button>
      </div>
      {sourceType === 'upload' && (
        <div style={{ marginTop: 12 }}>
          <span style={labelStyle}>Emails — one per line (or comma/space separated); {fmtInt(pastedEmails.length)} parsed</span>
          <textarea
            value={emailsRaw}
            onChange={e => setEmailsRaw(e.target.value)}
            rows={6}
            placeholder={'a@example.com\nb@example.com'}
            style={{ ...inputStyle, fontFamily: 'monospace', resize: 'vertical' }}
          />
          <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 4 }}>
            CSV file upload is a noted follow-up — paste the email column for now. Malformed entries refuse the whole request (nothing is queued).
          </div>
        </div>
      )}
      {pickerError && (
        <div style={{ fontSize: 11.5, color: colors.warningText, marginTop: 8 }}>Picker source degraded: {pickerError}</div>
      )}
      <div style={{ fontSize: 11.5, color: colors.textMuted, marginTop: 10 }}>{COST_NOTE} Global spend is bounded by EO_CLEAN_DAILY_BUDGET across all jobs.</div>
      {message && (
        <div style={{
          marginTop: 10, padding: '8px 12px', borderRadius: 8, fontSize: 12.5,
          background: alpha(message.ok ? colors.success : colors.danger, '14'),
          border: `1px solid ${alpha(message.ok ? colors.success : colors.danger, '44')}`,
          color: message.ok ? colors.successText : colors.danger,
        }}>
          {message.text}
        </div>
      )}
    </div>
  );
};

// ── Page ────────────────────────────────────────────────────────────────────

export const EOCleaning: React.FC = () => {
  const [nonce, setNonce] = useState(0);
  const [state, retry] = useJobsFetch(nonce);
  const [actionError, setActionError] = useState<string | null>(null);
  const [busyJob, setBusyJob] = useState<string | null>(null);

  // Gentle auto-refresh while jobs are active (progress is server-side).
  useEffect(() => {
    const hasActive = state.jobs?.some(j => j.status === 'queued' || j.status === 'running');
    if (!hasActive) return;
    const t = window.setInterval(() => setNonce(n => n + 1), 20000);
    return () => window.clearInterval(t);
  }, [state.jobs]);

  const transition = async (job: CleanJob, action: 'pause' | 'resume') => {
    setActionError(null);
    setBusyJob(job.id);
    try {
      const res = await apiFetch(`/api/mailing/eo-clean/jobs/${job.id}/${action}`, { method: 'POST' });
      if (!res.ok) {
        let msg = `HTTP ${res.status}`;
        try {
          const body = await res.json();
          if (body?.error) msg += ` — ${body.error}`;
        } catch { /* non-JSON */ }
        setActionError(`${action} "${job.label}": ${msg}`);
        return;
      }
      setNonce(n => n + 1);
    } catch (e: unknown) {
      setActionError(`${action} "${job.label}": ${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setBusyJob(null);
    }
  };

  const jobs = state.jobs;

  return (
    <div style={pageStyle}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
        <div>
          <h2 style={{ margin: 0, fontSize: 20, display: 'flex', alignItems: 'center', gap: 10 }}>
            <FontAwesomeIcon icon={faBroom} style={{ color: colors.indigo400 }} /> EO Cleaning
          </h2>
          <div style={{ fontSize: 12, color: colors.textMuted, marginTop: 4 }}>
            Clean any list, segment or pasted upload through EmailOversight — verdicts land in the platform validation gate (mailing_eo_validation). v{PAGE_VERSION}
            {state.fetchedAt && <span> · fetched {state.fetchedAt}</span>}
          </div>
        </div>
        <button
          onClick={() => setNonce(n => n + 1)}
          style={{
            background: alpha(colors.indigo500, '22'), border: `1px solid ${alpha(colors.indigo500, '66')}`,
            color: colors.text, borderRadius: 8, padding: '8px 14px', cursor: 'pointer', fontSize: 12.5,
            display: 'flex', alignItems: 'center', gap: 8,
          }}>
          <FontAwesomeIcon icon={faRotate} /> Refresh
        </button>
      </div>

      {state.loading && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, color: colors.textMuted, fontSize: 13, padding: '18px 4px' }}>
          <FontAwesomeIcon icon={faSpinner} spin /> Loading cleaning jobs…
        </div>
      )}

      {!state.loading && state.unavailable && (
        <div style={{
          padding: '14px 16px', borderRadius: 8, fontSize: 12.5, lineHeight: 1.6,
          background: alpha(colors.warning, '14'), border: `1px solid ${alpha(colors.warning, '44')}`,
          color: colors.warning,
        }}>
          <strong>SOURCE UNAVAILABLE.</strong> <code style={{ fontFamily: 'monospace' }}>/api/mailing/eo-clean/jobs</code> is
          not exposed by this server build — the EO Cleaning backend has not been deployed here yet.
        </div>
      )}

      {!state.loading && state.error && (
        <SectionError label="eo cleaning jobs" error={state.error} onRetry={retry} />
      )}

      {!state.loading && !state.error && !state.unavailable && jobs && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
          <NewJobForm onCreated={() => setNonce(n => n + 1)} />

          <div style={panelStyle}>
            <SectionHeader title="Cleaning jobs" icon={faBroom} />
            {actionError && (
              <div style={{
                marginBottom: 10, padding: '8px 12px', borderRadius: 8, fontSize: 12.5,
                background: alpha(colors.danger, '14'), border: `1px solid ${alpha(colors.danger, '44')}`, color: colors.danger,
              }}>
                {actionError}
              </div>
            )}
            {jobs.length === 0 ? (
              <EmptyState
                title="No cleaning jobs yet"
                hint="Queue one above, or use the Clean action on a segment in Segmentation Command."
              />
            ) : (
              <div style={{ overflowX: 'auto' }}>
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={thStyle}>Label</th>
                      <th style={thStyle}>Source</th>
                      <th style={thStyle}>Status</th>
                      <th style={{ ...thStyle, minWidth: 140 }}>Progress</th>
                      <th style={numTh}>Total</th>
                      <th style={numTh}>Already clean</th>
                      <th style={numTh}>Pending</th>
                      <th style={numTh}>Verified</th>
                      <th style={numTh}>Complainer</th>
                      <th style={numTh}>Undeliverable</th>
                      <th style={numTh}>Other</th>
                      <th style={numTh}>Failed</th>
                      <th style={numTh}>Daily cap</th>
                      <th style={thStyle}>Actions</th>
                    </tr>
                  </thead>
                  <tbody>
                    {jobs.map(j => {
                      const pct = jobProgress(j);
                      const canPause = j.status === 'queued' || j.status === 'running';
                      const canResume = j.status === 'paused';
                      return (
                        <tr key={j.id}>
                          <td style={{ ...tdStyle, fontWeight: 700, maxWidth: 260, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
                            title={`${j.label} · ${j.id} · created ${j.created_at}${j.finished_at ? ` · finished ${j.finished_at}` : ''}`}>
                            {j.label || '(unlabeled)'}
                          </td>
                          <td style={{ ...tdStyle, fontFamily: 'monospace', fontSize: 11 }}
                            title={j.source_ref || 'pasted emails'}>
                            {j.source_type}
                          </td>
                          <td style={tdStyle}><Pill color={statusColor(j.status)}>{j.status.toUpperCase()}</Pill></td>
                          <td style={tdStyle} title={`${fmtInt(j.validated_count + j.failed_count)} of ${fmtInt(j.total_count - j.already_clean_count)} payable records processed`}>
                            <ProgressBar pct={pct} />
                            <div style={{ fontSize: 10.5, color: colors.textMuted, marginTop: 2, fontVariantNumeric: 'tabular-nums' }}>
                              {(pct * 100).toFixed(1)}%
                            </div>
                          </td>
                          <td style={numTd}>{fmtInt(j.total_count)}</td>
                          <td style={numTd} title="already Verified in mailing_eo_validation — skipped, never billed">
                            {fmtInt(j.already_clean_count)}
                          </td>
                          <td style={numTd}>{fmtInt(j.queued_count)}</td>
                          <td style={{ ...numTd, color: j.verified_count > 0 ? colors.successText : colors.textFaint }}>{fmtInt(j.verified_count)}</td>
                          <td style={numTd} title="Complainer IS mailable (standing doctrine)">{fmtInt(j.complainer_count)}</td>
                          <td style={{ ...numTd, color: j.undeliverable_count > 0 ? colors.danger : colors.textFaint }}>{fmtInt(j.undeliverable_count)}</td>
                          <td style={numTd} title="SpamTrap / Bot / Seed Account / Catch All / Role / Suppressed">{fmtInt(j.other_count)}</td>
                          <td style={{ ...numTd, color: j.failed_count > 0 ? colors.warning : colors.textFaint }}
                            title="no verdict obtained after retries — NOT written to the validation gate">
                            {fmtInt(j.failed_count)}
                          </td>
                          <td style={numTd}>{j.daily_cap > 0 ? fmtInt(j.daily_cap) : '∞'}</td>
                          <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>
                            {(canPause || canResume) && (
                              <button
                                onClick={() => transition(j, canPause ? 'pause' : 'resume')}
                                disabled={busyJob === j.id}
                                style={{
                                  background: alpha(canPause ? colors.warning : colors.success, '22'),
                                  border: `1px solid ${alpha(canPause ? colors.warning : colors.success, '44')}`,
                                  color: canPause ? colors.warningText : colors.successText,
                                  borderRadius: 6, padding: '4px 10px', cursor: busyJob === j.id ? 'wait' : 'pointer', fontSize: 11.5,
                                }}>
                                <FontAwesomeIcon icon={canPause ? faPause : faPlay} /> {canPause ? 'Pause' : 'Resume'}
                              </button>
                            )}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
                <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 8, lineHeight: 1.6 }}>
                  Progress = validated + failed over the payable pool (total − already clean). Verdicts write mailing_eo_validation
                  (newest-wins, source_file eo-clean-job/&lt;id&gt;) — the same gate fresh draws read. Failed = no verdict after retries;
                  nothing is invented for the validation gate. The worker drains jobs FIFO within the per-job daily cap and the global
                  EO_CLEAN_DAILY_BUDGET; active jobs auto-refresh every 20s.
                </div>
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  );
};

export default EOCleaning;
