import React, { useCallback, useEffect, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faSliders, faStore, faRotate, faSpinner, faFloppyDisk, faPlay, faClockRotateLeft,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import {
  colors, alpha, pageStyle, panelStyle, tableStyle,
  thStyle, tdStyle, numTd, numTh, btnStyle,
} from '../shared/theme';
import { SectionHeader, Pill, SectionError, EmptyState } from '../shared/ui';

// =============================================================================
// FRESH BROADCAST CONFIG — the fresh-introduction broadcast control surface
// =============================================================================
// Single source of truth for the fresh-introduction broadcast program. The
// operator edits per-stream knobs here (enabled, daily cap, per-ISP caps,
// offer, throttle window); the Python build pipeline reads the SAME table
// (mailing_stream_broadcast_config — cross-team contract). Backend:
// GET/PUT /api/mailing/stream-broadcast/config (stream_broadcast_config.go).
//
// Concurrency: saves carry the row's updated_at (optimistic lock). A 409
// means someone else saved first — the screen says so and asks for a reload,
// never silently overwrites.
//
// FilterBar note (PORTAL_DESIGN_SYSTEM §3): current-state config screen —
// none of the canonical filter vocabulary binds; no FilterBar mounted.

// PAGE_VERSION 1.1 — Fresh Broadcast RUNNER surface (2026-07-28): 'Run DRY
// now' button (POST /api/mailing/fresh-broadcast/runs), per-stream last-run
// panel (fill/cap/short + campaigns + trigger + time), and the auto_stage
// toggle (nightly worker LIVE staging — drafts only; warning dialog).
// PAGE_VERSION 1.0 — initial Fresh Broadcast Config screen (2026-07-26).
const PAGE_VERSION = '1.1';

// ── API shapes (mirror stream_broadcast_config.go; do not drift) ────────────

interface StreamRow {
  stream_key: string;
  enabled: boolean;
  daily_cap: number;
  isp_caps: Record<string, number>;
  offer: string;
  throttle_hours: number;
  label: string;
  seg_prefix: string;
  vertical_tag: string | null;
  dataset_ids: string[];
  primary_sites: string[];
  secondary_sites: string[];
  eo_mailable: string[];
  sending_domain: string | null;    // explicit-domain streams (wcm) — read-only
  sending_profile_id: string | null; // read-only
  updated_at: string;
  updated_by: string;
}

interface BenchOffer {
  key: string;
  exists: boolean;
  offer_id: string;
  offer_name: string;
  status: string;
  matched_by: string;
  proofs: number;
  smart_links: number;
  readiness: 'ready' | 'attention' | 'not_onboarded';
}

interface ConfigResponse {
  api_version: string;
  generated_at: string;
  streams: StreamRow[];
  bench: BenchOffer[];
  bench_available: boolean;
}

interface PutResponse {
  stream: StreamRow;
  warnings: string[];
}

// ── Runner API shapes (mirror fresh_broadcast_handlers.go; do not drift) ────

interface RunCampaign {
  campaign_id?: string;
  name: string;
  site: string;
  sending_domain: string;
  planned: number;
  status: string; // draft | planned | refused
  reason?: string;
}

interface RunStream {
  stream: string;
  status: string; // ok | skipped | refused | error
  reason?: string;
  drawn: number;
  fill: number;
  cap: number;
  short: number;
  per_isp?: Record<string, number>;
  masked?: number;
  segments_created: number;
  members_added: number;
  claimed: number;
  campaigns: RunCampaign[];
}

interface RunResults {
  run_id: string;
  date: string;
  dry: boolean;
  trigger: string;
  status: string;
  streams: RunStream[] | null;
}

interface RunRecord {
  id: string;
  date: string;
  dry: boolean;
  trigger: string;
  status: string;
  results: RunResults | Record<string, never>;
  created_at: string;
}

interface RunnerStatus {
  streams: { stream_key: string; enabled: boolean; auto_stage: boolean }[];
  runs: RunRecord[];
}

// ── Fetch plumbing ──────────────────────────────────────────────────────────

interface FetchState {
  loading: boolean;
  error: string | null;
  unavailable: boolean;
  data: ConfigResponse | null;
  fetchedAt: string | null;
  ms: number | null;
}

const useConfigFetch = (nonce: number): [FetchState, () => void] => {
  const [state, setState] = useState<FetchState>({
    loading: true, error: null, unavailable: false, data: null, fetchedAt: null, ms: null,
  });
  const [localNonce, setLocalNonce] = useState(0);
  const retry = useCallback(() => setLocalNonce(n => n + 1), []);
  const abortRef = useRef<AbortController | null>(null);

  useEffect(() => {
    abortRef.current?.abort();
    const ac = new AbortController();
    abortRef.current = ac;
    setState(s => ({ ...s, loading: true, error: null }));
    const t0 = performance.now();
    apiFetch('/api/mailing/stream-broadcast/config', { signal: ac.signal })
      .then(async res => {
        const ms = Math.round(performance.now() - t0);
        const fetchedAt = new Date().toLocaleTimeString();
        if (res.status === 404) {
          setState({ loading: false, error: null, unavailable: true, data: null, fetchedAt, ms });
          return;
        }
        if (!res.ok) {
          let msg = `HTTP ${res.status}`;
          try {
            const body = await res.json();
            if (body?.error) msg += ` — ${body.error}`;
          } catch { /* non-JSON body */ }
          setState({ loading: false, error: msg, unavailable: false, data: null, fetchedAt, ms });
          return;
        }
        const data = (await res.json()) as ConfigResponse;
        setState({ loading: false, error: null, unavailable: false, data, fetchedAt, ms });
      })
      .catch((e: unknown) => {
        if (ac.signal.aborted) return;
        setState({
          loading: false, error: e instanceof Error ? e.message : String(e),
          unavailable: false, data: null, fetchedAt: new Date().toLocaleTimeString(), ms: null,
        });
      });
    return () => ac.abort();
  }, [nonce, localNonce]);

  return [state, retry];
};

// ── Helpers ─────────────────────────────────────────────────────────────────

const readinessColor = (r: BenchOffer['readiness']): string =>
  r === 'ready' ? colors.success : r === 'attention' ? colors.warning : colors.danger;

const readinessLabel = (b: BenchOffer): string => {
  if (b.readiness === 'not_onboarded') return 'NOT ONBOARDED';
  if (b.status !== 'active') return b.status.toUpperCase(); // e.g. DRAFT
  if (b.proofs <= 0) return 'NO PROOFS';
  if (b.smart_links <= 0) return 'NO SMARTLINK';
  return 'READY';
};

const fmtRelative = (iso: string): string => {
  const t = Date.parse(iso);
  if (isNaN(t)) return '—';
  const s = Math.max(0, (Date.now() - t) / 1000);
  if (s < 90) return `${Math.round(s)}s ago`;
  if (s < 5400) return `${Math.round(s / 60)}m ago`;
  if (s < 172800) return `${(s / 3600).toFixed(1)}h ago`;
  return `${(s / 86400).toFixed(1)}d ago`;
};

// Editable draft per stream.
interface Draft {
  enabled: boolean;
  daily_cap: string;      // input strings; validated on save
  isp_caps: string;       // JSON text, e.g. {"apple":0}
  offer: string;
  throttle_hours: string;
}

const draftFrom = (s: StreamRow): Draft => ({
  enabled: s.enabled,
  daily_cap: String(s.daily_cap),
  isp_caps: JSON.stringify(s.isp_caps ?? {}),
  offer: s.offer,
  throttle_hours: String(s.throttle_hours),
});

const isDirty = (s: StreamRow, d: Draft): boolean =>
  d.enabled !== s.enabled ||
  d.daily_cap !== String(s.daily_cap) ||
  d.isp_caps !== JSON.stringify(s.isp_caps ?? {}) ||
  d.offer !== s.offer ||
  d.throttle_hours !== String(s.throttle_hours);

// Client-side validation mirror (server re-validates authoritatively).
const validateDraft = (d: Draft, bench: BenchOffer[]): string | null => {
  const cap = Number(d.daily_cap);
  if (!Number.isInteger(cap) || cap < 0) return 'daily cap must be an integer >= 0';
  const throttle = Number(d.throttle_hours);
  if (!Number.isInteger(throttle) || throttle < 1 || throttle > 24) return 'throttle must be 1–24 hours';
  let caps: unknown;
  try {
    caps = JSON.parse(d.isp_caps || '{}');
  } catch {
    return 'ISP caps must be valid JSON, e.g. {"apple":0}';
  }
  if (typeof caps !== 'object' || caps === null || Array.isArray(caps)) {
    return 'ISP caps must be a JSON object of {isp: cap}';
  }
  for (const [isp, v] of Object.entries(caps as Record<string, unknown>)) {
    if (typeof v !== 'number' || !Number.isInteger(v) || v < 0) {
      return `ISP cap for "${isp}" must be an integer >= 0`;
    }
  }
  if (!d.offer.trim()) return 'offer is required';
  const b = bench.find(x => x.key === d.offer);
  if (b && b.readiness === 'not_onboarded') {
    return `"${d.offer}" is NOT ONBOARDED in mailing_offers — the server will refuse this save; onboard the offer first`;
  }
  return null;
};

const inputStyle: React.CSSProperties = {
  background: 'rgba(15,30,60,0.55)',
  border: `1px solid ${colors.panelBorderStrong}`,
  color: colors.text,
  borderRadius: 6,
  padding: '5px 8px',
  fontSize: 12,
  fontVariantNumeric: 'tabular-nums',
};

const chip = (text: string, color: string, key?: string): React.ReactNode => (
  <span key={key ?? text} style={{
    display: 'inline-block', fontSize: 10.5, fontFamily: 'monospace',
    color, background: alpha(color, '14'), border: `1px solid ${alpha(color, '44')}`,
    borderRadius: 5, padding: '1px 6px', marginRight: 4, marginBottom: 2,
  }}>{text}</span>
);

// ── Page ────────────────────────────────────────────────────────────────────

interface SaveState {
  saving: boolean;
  error: string | null;
  conflict: boolean;
  warnings: string[];
}

export const FreshBroadcastConfig: React.FC = () => {
  const [nonce, setNonce] = useState(0);
  const [state, retry] = useConfigFetch(nonce);
  const [drafts, setDrafts] = useState<Record<string, Draft>>({});
  const [saves, setSaves] = useState<Record<string, SaveState>>({});

  // ── Runner state (status panel + Run DRY now + auto_stage) ──
  const [runnerStatus, setRunnerStatus] = useState<RunnerStatus | null>(null);
  const [runnerError, setRunnerError] = useState<string | null>(null);
  const [running, setRunning] = useState(false);
  const [lastRunNow, setLastRunNow] = useState<RunResults | null>(null);
  const [autoStageBusy, setAutoStageBusy] = useState<Record<string, boolean>>({});

  const refreshRunnerStatus = useCallback(() => {
    apiFetch('/api/mailing/fresh-broadcast/status')
      .then(async res => {
        if (res.status === 404) { setRunnerStatus(null); setRunnerError(null); return; }
        if (!res.ok) { setRunnerError(`HTTP ${res.status}`); return; }
        setRunnerStatus((await res.json()) as RunnerStatus);
        setRunnerError(null);
      })
      .catch((e: unknown) => setRunnerError(e instanceof Error ? e.message : String(e)));
  }, []);

  useEffect(() => { refreshRunnerStatus(); }, [nonce, refreshRunnerStatus]);

  const runDryNow = async () => {
    setRunning(true);
    setLastRunNow(null);
    try {
      const res = await apiFetch('/api/mailing/fresh-broadcast/runs', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ dry: true, trigger: 'screen' }),
      });
      if (!res.ok) {
        let msg = `HTTP ${res.status}`;
        try { const body = await res.json(); if (body?.error) msg += ` — ${body.error}`; } catch { /* non-JSON */ }
        setRunnerError(msg);
        return;
      }
      setLastRunNow((await res.json()) as RunResults);
      refreshRunnerStatus();
    } catch (e: unknown) {
      setRunnerError(e instanceof Error ? e.message : String(e));
    } finally {
      setRunning(false);
    }
  };

  const toggleAutoStage = async (streamKey: string, next: boolean) => {
    if (next && !window.confirm(
      `Enable AUTO-STAGE for "${streamKey}"?\n\nThe nightly worker (09:00 UTC) will run the LIVE stage step for this stream: `
      + 'dated segments are created, queue rows are CLAIMED, and draft campaigns are staged automatically.\n\n'
      + 'Campaigns still land as DRAFTS — the Draft Board remains the approval gate; nothing sends without your approval.',
    )) return;
    setAutoStageBusy(prev => ({ ...prev, [streamKey]: true }));
    try {
      const res = await apiFetch('/api/mailing/fresh-broadcast/auto-stage', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ stream_key: streamKey, auto_stage: next }),
      });
      if (!res.ok) {
        let msg = `auto-stage HTTP ${res.status}`;
        try { const body = await res.json(); if (body?.error) msg += ` — ${body.error}`; } catch { /* non-JSON */ }
        setRunnerError(msg);
        return;
      }
      refreshRunnerStatus();
    } catch (e: unknown) {
      setRunnerError(e instanceof Error ? e.message : String(e));
    } finally {
      setAutoStageBusy(prev => ({ ...prev, [streamKey]: false }));
    }
  };

  const autoStageFor = (key: string): boolean =>
    runnerStatus?.streams.find(s => s.stream_key === key)?.auto_stage ?? false;

  // Latest recorded run + per-stream latest results for the panel.
  const latestRun = lastRunNow
    ?? (runnerStatus?.runs?.length ? (runnerStatus.runs[0].results as RunResults) : null);
  const latestRunMeta = lastRunNow
    ? { trigger: lastRunNow.trigger, dry: lastRunNow.dry, at: 'just now', status: lastRunNow.status }
    : (runnerStatus?.runs?.length
      ? {
        trigger: runnerStatus.runs[0].trigger, dry: runnerStatus.runs[0].dry,
        at: fmtRelative(runnerStatus.runs[0].created_at), status: runnerStatus.runs[0].status,
      }
      : null);

  const d = state.data;

  // (Re)seed drafts whenever fresh data lands.
  useEffect(() => {
    if (!d) return;
    const next: Record<string, Draft> = {};
    d.streams.forEach(s => { next[s.stream_key] = draftFrom(s); });
    setDrafts(next);
    setSaves({});
  }, [d]);

  const setDraft = (key: string, patch: Partial<Draft>) =>
    setDrafts(prev => ({ ...prev, [key]: { ...prev[key], ...patch } }));

  const save = async (s: StreamRow) => {
    const draft = drafts[s.stream_key];
    if (!draft || !d) return;
    const clientErr = validateDraft(draft, d.bench);
    if (clientErr) {
      setSaves(prev => ({ ...prev, [s.stream_key]: { saving: false, error: clientErr, conflict: false, warnings: [] } }));
      return;
    }
    setSaves(prev => ({ ...prev, [s.stream_key]: { saving: true, error: null, conflict: false, warnings: [] } }));
    try {
      const res = await apiFetch('/api/mailing/stream-broadcast/config', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          stream_key: s.stream_key,
          enabled: draft.enabled,
          daily_cap: Number(draft.daily_cap),
          isp_caps: JSON.parse(draft.isp_caps || '{}') as Record<string, number>,
          offer: draft.offer.trim(),
          throttle_hours: Number(draft.throttle_hours),
          expected_updated_at: s.updated_at,
        }),
      });
      if (res.status === 409) {
        setSaves(prev => ({
          ...prev,
          [s.stream_key]: {
            saving: false, conflict: true, warnings: [],
            error: 'Someone else saved this stream since you loaded it — refresh to load their version, then re-apply your edit.',
          },
        }));
        return;
      }
      if (!res.ok) {
        let msg = `HTTP ${res.status}`;
        try {
          const body = await res.json();
          if (body?.error) msg += ` — ${body.error}`;
        } catch { /* non-JSON body */ }
        setSaves(prev => ({ ...prev, [s.stream_key]: { saving: false, error: msg, conflict: false, warnings: [] } }));
        return;
      }
      const out = (await res.json()) as PutResponse;
      setSaves(prev => ({ ...prev, [s.stream_key]: { saving: false, error: null, conflict: false, warnings: out.warnings ?? [] } }));
      // Refetch so updated_at/updated_by and the row are server-truth.
      setNonce(n => n + 1);
    } catch (e: unknown) {
      setSaves(prev => ({
        ...prev,
        [s.stream_key]: { saving: false, error: e instanceof Error ? e.message : String(e), conflict: false, warnings: [] },
      }));
    }
  };

  return (
    <div style={pageStyle}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
        <div>
          <h2 style={{ margin: 0, fontSize: 20, display: 'flex', alignItems: 'center', gap: 10 }}>
            <FontAwesomeIcon icon={faSliders} style={{ color: colors.indigo400 }} /> Fresh Broadcast Config
          </h2>
          <div style={{ fontSize: 12, color: colors.textMuted, marginTop: 4 }}>
            Single source of truth for the fresh-introduction broadcast program — the build pipeline reads this same table. v{PAGE_VERSION}
            {state.fetchedAt && <span> · fetched {state.fetchedAt}{state.ms != null ? ` · ${state.ms}ms` : ''}</span>}
          </div>
        </div>
        <div style={{ display: 'flex', gap: 8 }}>
          <button
            onClick={runDryNow}
            disabled={running}
            title="Invoke the fresh-broadcast runner in DRY mode — draws every enabled stream, reports fill vs cap and the campaigns it WOULD stage. No writes."
            style={{
              background: alpha(colors.success, '22'), border: `1px solid ${alpha(colors.success, '66')}`,
              color: colors.text, borderRadius: 8, padding: '8px 14px',
              cursor: running ? 'default' : 'pointer', opacity: running ? 0.6 : 1, fontSize: 12.5,
              display: 'flex', alignItems: 'center', gap: 8,
            }}>
            {running ? <FontAwesomeIcon icon={faSpinner} spin /> : <FontAwesomeIcon icon={faPlay} />}
            Run DRY now
          </button>
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
      </div>

      {state.loading && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, color: colors.textMuted, fontSize: 13, padding: '18px 4px' }}>
          <FontAwesomeIcon icon={faSpinner} spin /> Loading stream broadcast config…
        </div>
      )}

      {!state.loading && state.unavailable && (
        <div style={{
          padding: '14px 16px', borderRadius: 8, fontSize: 12.5, lineHeight: 1.6,
          background: alpha(colors.warning, '14'), border: `1px solid ${alpha(colors.warning, '44')}`,
          color: colors.warning,
        }}>
          <strong>SOURCE UNAVAILABLE.</strong> <code style={{ fontFamily: 'monospace' }}>/api/mailing/stream-broadcast/config</code> is
          not exposed by this server build — the Fresh Broadcast backend has not been deployed here yet.
        </div>
      )}

      {!state.loading && state.error && (
        <SectionError label="stream broadcast config" error={state.error} onRetry={retry} />
      )}

      {!state.loading && !state.error && !state.unavailable && d && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
          {/* ── Offer bench ── */}
          <div style={panelStyle}>
            <SectionHeader title="Offer bench — intro readiness" icon={faStore} />
            {!d.bench_available ? (
              <div style={{ fontSize: 12.5, color: colors.warningText }}>
                Bench readiness sources failed on this fetch — no lights shown rather than wrong ones. Retry.
              </div>
            ) : (
              <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap' }}>
                {d.bench.map(b => {
                  const c = readinessColor(b.readiness);
                  return (
                    <span
                      key={b.key}
                      title={b.exists
                        ? `${b.offer_name} · status ${b.status} · ${b.proofs} active proofs · ${b.smart_links} smartlink rows · matched by ${b.matched_by}`
                        : 'no mailing_offers row resolves to this key — onboard the offer before assigning it'}
                      style={{
                        display: 'inline-flex', alignItems: 'center', gap: 8, fontSize: 12,
                        color: colors.text, background: alpha(c, '14'),
                        border: `1px solid ${alpha(c, '44')}`, borderRadius: 8, padding: '6px 12px',
                      }}
                    >
                      <span style={{ width: 9, height: 9, borderRadius: 999, background: c }} />
                      <span style={{ fontFamily: 'monospace' }}>{b.key}</span>
                      <Pill color={c}>{readinessLabel(b)}</Pill>
                    </span>
                  );
                })}
              </div>
            )}
            <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 10 }}>
              Green = offer row active with ≥1 active proof and a seeded smartlink. Amber = row exists but needs attention (draft status, missing proofs, or no smartlink). Red = no mailing_offers row resolves to the key — selectable below only with a blocking warning; the server refuses the save.
            </div>
          </div>

          {/* ── Streams ── */}
          <div style={panelStyle}>
            <SectionHeader title="Streams — fresh-introduction knobs" icon={faSliders} />
            {d.streams.length === 0 ? (
              <EmptyState
                title="No stream config rows"
                hint="mailing_stream_broadcast_config has no rows for this org — the seed migration has not run here."
              />
            ) : (
              <div style={{ overflowX: 'auto' }}>
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={thStyle}>Stream</th>
                      <th style={thStyle}>Enabled</th>
                      <th style={numTh}>Daily cap</th>
                      <th style={thStyle}>Offer</th>
                      <th style={thStyle}>ISP caps (JSON)</th>
                      <th style={numTh}>Throttle</th>
                      <th style={thStyle}>Auto-stage</th>
                      <th style={thStyle}>Routing (read-only)</th>
                      <th style={thStyle}>Updated</th>
                      <th style={thStyle} />
                    </tr>
                  </thead>
                  <tbody>
                    {d.streams.map(s => {
                      const draft = drafts[s.stream_key] ?? draftFrom(s);
                      const dirty = isDirty(s, draft);
                      const sv = saves[s.stream_key];
                      return (
                        <React.Fragment key={s.stream_key}>
                          <tr>
                            <td style={{ ...tdStyle, minWidth: 180 }}>
                              <div style={{ fontWeight: 700 }}>{s.label || s.stream_key}</div>
                              <div style={{ fontFamily: 'monospace', fontSize: 10.5, color: colors.textFaint }}>
                                {s.stream_key} · {s.seg_prefix}{s.vertical_tag ? ` · ${s.vertical_tag}` : ''}
                              </div>
                            </td>
                            <td style={tdStyle}>
                              <input
                                type="checkbox"
                                checked={draft.enabled}
                                onChange={e => setDraft(s.stream_key, { enabled: e.target.checked })}
                              />
                            </td>
                            <td style={numTd}>
                              <input
                                type="number" min={0} step={1000}
                                value={draft.daily_cap}
                                onChange={e => setDraft(s.stream_key, { daily_cap: e.target.value })}
                                style={{ ...inputStyle, width: 90, textAlign: 'right' }}
                              />
                            </td>
                            <td style={tdStyle}>
                              <select
                                value={draft.offer}
                                onChange={e => setDraft(s.stream_key, { offer: e.target.value })}
                                style={{ ...inputStyle, width: 160 }}
                              >
                                {/* current value first if it's off-bench */}
                                {!d.bench.some(b => b.key === draft.offer) && (
                                  <option value={draft.offer}>{draft.offer}</option>
                                )}
                                {d.bench.map(b => (
                                  <option key={b.key} value={b.key}>
                                    {b.key}{b.readiness === 'not_onboarded' ? ' (NOT ONBOARDED)' : b.status && b.status !== 'active' ? ` (${b.status.toUpperCase()})` : ''}
                                  </option>
                                ))}
                              </select>
                            </td>
                            <td style={tdStyle}>
                              <input
                                type="text"
                                value={draft.isp_caps}
                                onChange={e => setDraft(s.stream_key, { isp_caps: e.target.value })}
                                style={{ ...inputStyle, width: 140, fontFamily: 'monospace' }}
                                placeholder='{"apple":0}'
                              />
                            </td>
                            <td style={numTd}>
                              <input
                                type="number" min={1} max={24}
                                value={draft.throttle_hours}
                                onChange={e => setDraft(s.stream_key, { throttle_hours: e.target.value })}
                                style={{ ...inputStyle, width: 52, textAlign: 'right' }}
                              />
                              <span style={{ fontSize: 11, color: colors.textMuted, marginLeft: 4 }}>h</span>
                            </td>
                            <td style={tdStyle}>
                              <label
                                title="When ON, the nightly worker (09:00 UTC) runs the LIVE stage step for this stream: segments + queue claims + DRAFT campaigns. The Draft Board remains the approval gate — auto-stage never promotes or sends."
                                style={{ display: 'inline-flex', alignItems: 'center', gap: 6, cursor: 'pointer' }}
                              >
                                <input
                                  type="checkbox"
                                  checked={autoStageFor(s.stream_key)}
                                  disabled={autoStageBusy[s.stream_key] === true || runnerStatus === null}
                                  onChange={e => toggleAutoStage(s.stream_key, e.target.checked)}
                                />
                                {autoStageBusy[s.stream_key]
                                  ? <FontAwesomeIcon icon={faSpinner} spin style={{ fontSize: 10 }} />
                                  : autoStageFor(s.stream_key)
                                    ? <Pill color={colors.warning}>NIGHTLY</Pill>
                                    : <span style={{ fontSize: 10.5, color: colors.textFaint }}>manual</span>}
                              </label>
                            </td>
                            <td style={{ ...tdStyle, maxWidth: 260 }}>
                              <div>
                                {s.sending_domain && chip(s.sending_domain, colors.success, `d-${s.stream_key}`)}
                                {s.primary_sites.map(p => chip(p, colors.indigo400, `p-${s.stream_key}-${p}`))}
                                {s.secondary_sites.map(p => chip(p, colors.idle, `s-${s.stream_key}-${p}`))}
                                {!s.sending_domain && s.primary_sites.length === 0 && s.secondary_sites.length === 0 && (
                                  <span style={{ color: colors.textFaint, fontSize: 11 }}>no routing declared</span>
                                )}
                              </div>
                              <div style={{ fontSize: 10.5, color: colors.textFaint, marginTop: 2 }}
                                title={`eo_mailable: ${s.eo_mailable.join(', ')}`}>
                                {s.dataset_ids.length} dataset{s.dataset_ids.length === 1 ? '' : 's'} · EO: {s.eo_mailable.join('/') || '—'}
                              </div>
                            </td>
                            <td style={{ ...tdStyle, whiteSpace: 'nowrap' }} title={`${s.updated_at} by ${s.updated_by}`}>
                              <div>{fmtRelative(s.updated_at)}</div>
                              <div style={{ fontSize: 10.5, color: colors.textFaint }}>{s.updated_by || '—'}</div>
                            </td>
                            <td style={tdStyle}>
                              <button
                                onClick={() => save(s)}
                                disabled={!dirty || sv?.saving === true}
                                style={{
                                  ...btnStyle,
                                  opacity: dirty && !sv?.saving ? 1 : 0.4,
                                  cursor: dirty && !sv?.saving ? 'pointer' : 'default',
                                  display: 'inline-flex', alignItems: 'center', gap: 6,
                                }}>
                                {sv?.saving ? <FontAwesomeIcon icon={faSpinner} spin /> : <FontAwesomeIcon icon={faFloppyDisk} />}
                                Save
                              </button>
                            </td>
                          </tr>
                          {(sv?.error || (sv?.warnings.length ?? 0) > 0) && (
                            <tr>
                              <td colSpan={10} style={{ padding: '4px 12px 10px', borderTop: 'none' }}>
                                {sv?.error && (
                                  <div style={{ fontSize: 12, color: sv.conflict ? colors.warningText : colors.dangerText }}>
                                    {sv.error}
                                  </div>
                                )}
                                {sv?.warnings.map((wmsg, i) => (
                                  <div key={i} style={{ fontSize: 12, color: colors.warningText }}>⚠ {wmsg}</div>
                                ))}
                              </td>
                            </tr>
                          )}
                        </React.Fragment>
                      );
                    })}
                  </tbody>
                </table>
                <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 8, lineHeight: 1.6 }}>
                  Editable: enabled, daily cap, offer, ISP caps, throttle window. Routing columns are read-only mirrors of the stream router registry; a green chip is an EXPLICIT sending domain (the stream mails via its own lane, not the brand-code pools).
                  ISP caps are absolute per-ISP daily ceilings as JSON (<code>{'{"apple":0}'}</code> = do not mail apple). Saves are optimistic-locked on the row's updated_at — a conflict never silently overwrites.
                </div>
              </div>
            )}
          </div>

          {/* ── Last run (fresh-broadcast runner) ── */}
          <div style={panelStyle}>
            <SectionHeader title="Fresh broadcast runner — last run" icon={faClockRotateLeft} />
            {runnerError && (
              <div style={{ fontSize: 12, color: colors.dangerText, marginBottom: 8 }}>⚠ {runnerError}</div>
            )}
            {!latestRun || !latestRunMeta ? (
              <div style={{ fontSize: 12.5, color: colors.textMuted }}>
                No runs recorded yet. The nightly worker runs a DRY pass at 09:00 UTC; &quot;Run DRY now&quot; invokes the same runner on demand.
              </div>
            ) : (
              <>
                <div style={{ fontSize: 12, color: colors.textMuted, marginBottom: 10, display: 'flex', gap: 10, alignItems: 'center' }}>
                  <Pill color={latestRunMeta.dry ? colors.indigo400 : colors.warning}>
                    {latestRunMeta.dry ? 'DRY' : 'LIVE'}
                  </Pill>
                  <span>trigger: <code style={{ fontFamily: 'monospace' }}>{latestRunMeta.trigger}</code></span>
                  <span>date: {latestRun.date}</span>
                  <span>{latestRunMeta.at}</span>
                  <Pill color={latestRunMeta.status === 'ok' ? colors.success : colors.warning}>
                    {latestRunMeta.status.toUpperCase()}
                  </Pill>
                </div>
                <div style={{ overflowX: 'auto' }}>
                  <table style={tableStyle}>
                    <thead>
                      <tr>
                        <th style={thStyle}>Stream</th>
                        <th style={thStyle}>Status</th>
                        <th style={numTh}>Fill</th>
                        <th style={numTh}>Cap</th>
                        <th style={numTh}>Short</th>
                        <th style={numTh}>Staged</th>
                        <th style={thStyle}>Campaigns</th>
                      </tr>
                    </thead>
                    <tbody>
                      {(latestRun.streams ?? []).map(st => (
                        <tr key={st.stream}>
                          <td style={{ ...tdStyle, fontFamily: 'monospace' }}>{st.stream}</td>
                          <td style={tdStyle} title={st.reason || ''}>
                            <Pill color={st.status === 'ok' ? colors.success : st.status === 'skipped' ? colors.idle : colors.danger}>
                              {st.status.toUpperCase()}
                            </Pill>
                            {st.reason && (
                              <div style={{ fontSize: 10.5, color: colors.textFaint, marginTop: 2, maxWidth: 320 }}>{st.reason}</div>
                            )}
                          </td>
                          <td style={numTd}>{st.fill.toLocaleString()}</td>
                          <td style={numTd}>{st.cap.toLocaleString()}</td>
                          <td style={{ ...numTd, color: st.short > 0 ? colors.warningText : colors.textMuted }}
                            title={st.short > 0 ? 'Ready stock cannot meet the cap — this shortfall IS the daily data ask for this feed.' : ''}>
                            {st.short.toLocaleString()}
                          </td>
                          <td style={numTd} title={`segments ${st.segments_created} · members ${st.members_added.toLocaleString()} · claimed ${st.claimed.toLocaleString()}`}>
                            {st.members_added.toLocaleString()}
                          </td>
                          <td style={tdStyle}>
                            {st.campaigns.length === 0
                              ? <span style={{ color: colors.textFaint, fontSize: 11 }}>—</span>
                              : st.campaigns.map(c => (
                                <div key={`${st.stream}-${c.site}`} style={{ fontSize: 11, marginBottom: 2 }}
                                  title={`${c.name} · ${c.sending_domain} · ${c.planned.toLocaleString()} planned${c.campaign_id ? ` · ${c.campaign_id}` : ''}${c.reason ? ` · ${c.reason}` : ''}`}>
                                  {chip(c.site, c.status === 'refused' ? colors.danger : c.status === 'draft' ? colors.success : colors.indigo400, `${st.stream}-${c.site}`)}
                                  <span style={{ fontFamily: 'monospace', color: colors.textMuted }}>
                                    {c.planned.toLocaleString()} · {c.status}
                                    {c.campaign_id ? ` · ${c.campaign_id.slice(0, 8)}` : ''}
                                  </span>
                                </div>
                              ))}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
                <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 8, lineHeight: 1.6 }}>
                  Draft campaigns are reviewed and promoted on the <strong>Draft Board</strong> — the runner never deploys.
                  A SHORT value is the operator&apos;s daily data ask for that feed. LIVE runs skip any stream whose dated segments already exist (resume-safe).
                </div>
              </>
            )}
          </div>
        </div>
      )}
    </div>
  );
};

export default FreshBroadcastConfig;
