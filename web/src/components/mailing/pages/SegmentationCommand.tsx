import React, { useCallback, useEffect, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faHeartPulse, faRotate, faSpinner } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import { colors, alpha, pageStyle, panelStyle } from '../shared/theme';
import { SectionHeader, Stat, SectionError } from '../shared/ui';
import { SegmentFreshnessBoard } from './SegmentFreshnessBoard';

// =============================================================================
// SEGMENTATION — the life-blood screen
// =============================================================================
// The operator's segmentation truth surface. Its one job: make MEMBERSHIP
// staleness impossible to hide, and make fixing it one click away.
//
// The screen is now ONE dashboard (SegmentFreshnessBoard) plus a thin worker /
// totals strip. Everything below the board is deliberately small: the operator
// ruled on 2026-08-21 that the dense family surface was noise —
// "Segment families Alerts I do not care for" — and asked for
// "a dashboard feel for the sending domain you are looking at".
//
// Source: GET /api/mailing/segmentation/health (internal/api/
// segmentation_health.go) for worker liveness + estate totals; the board owns
// GET/POST /api/mailing/segments/{freshness,refresh} independently, so a
// failure in either surface never blanks the other.
//
// FilterBar note (PORTAL_DESIGN_SYSTEM §3): this screen shows current
// platform state — none of the canonical filter vocabulary (Denver date
// range, brand, ISP, transport) binds to these sources, so the shared
// FilterBar is deliberately not mounted (same ruling as OperationsConsole).
// The board's sending-domain selector filters rows it already holds; it is a
// view selector, not a query filter.
//
// State honesty (§1.6): loading, fetch-error-with-retry, endpoint-not-on-
// this-build (404 = deploy held), and genuinely-empty are all distinct.

// PAGE_VERSION 1.5 — operator audit (2026-11-02). Page header removed (portal
// chrome carries it). Health uses ?summary=1, Refresh reloads the board too,
// and the SUM-of-counts Members stat is gone: 50M has no decision value.
// PAGE_VERSION 1.4 — operator rework (2026-08-21). REMOVED: the "Alerts —
// ranked by blast radius" panel and the "Segment families" table (with its
// drilldown, member-scoped perf columns, churn sparkline and per-family EO
// Clean button) — operator: "Segment families Alerts I do not care for."
// Cleaning a segment is unaffected: the EO Cleaning tab has a first-class
// segment picker (EOCleaning.tsx — source_type='segment'), which is now the
// single path. The family verdict tiles went with the table; what remains is
// worker liveness + estate totals, and the Estate Verdict the operator DID
// ask for (active openers/clickers) now lives on the board itself.
// PAGE_VERSION 1.3 — Segment Freshness board (SegmentFreshnessBoard.tsx):
// the per-sending-domain 7/14/30/60d Openers/Clickers grid with per-cell
// freshness verdicts + refresh actions, against GET/POST
// /api/mailing/segments/{freshness,refresh} (operator mandate 2026-08-20:
// "visually see if the segments are stale and how I can get them refreshed").
// PAGE_VERSION 1.2 — EO Clean actions on family + drilldown rows (REMOVED in
// 1.4 — the capability lives on the EO Cleaning tab).
// PAGE_VERSION 1.1 — performance surface: member-scoped 7d/30d windows +
// 14d churn timeline (REMOVED in 1.4 with the family table).

// ── API shapes (mirror segmentation_health.go; do not drift) ────────────────
// Only the fields this screen renders are typed.

interface WorkerRow {
  name: string;
  cadence_seconds: number;
  last_beat_at: string | null;
  last_error: string;
  last_run_status: string;
  last_run_at: string | null;
  light: 'ok' | 'stale' | 'error' | 'stalled' | 'unknown';
}

interface HealthResponse {
  summary: { segments_total: number };
  workers: WorkerRow[];
}

// ── Fetch plumbing (single endpoint; mirrors OperationsConsole's panel hook —
// hoisting the shared hook into shared/ is a noted follow-up, not done here to
// keep this change inside its file boundary) ────────────────────────────────

interface FetchState {
  loading: boolean;
  error: string | null;
  unavailable: boolean; // 404 — endpoint not on this server build
  data: HealthResponse | null;
  fetchedAt: string | null;
  ms: number | null;
}

const HEALTH_URL = '/api/mailing/segmentation/health?summary=1';

const useHealthFetch = (nonce: number): [FetchState, () => void] => {
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
    apiFetch(HEALTH_URL, { signal: ac.signal })
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
          } catch { /* non-JSON error body */ }
          setState({ loading: false, error: msg, unavailable: false, data: null, fetchedAt, ms });
          return;
        }
        const data = (await res.json()) as HealthResponse;
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

// ── Display helpers ─────────────────────────────────────────────────────────

const fmtInt = (n: number): string => n.toLocaleString();

const fmtAgeSeconds = (s: number): string => {
  const v = Math.max(0, s);
  if (v < 90) return `${Math.round(v)}s`;
  if (v < 5400) return `${Math.round(v / 60)}m`;
  if (v < 172800) return `${(v / 3600).toFixed(1)}h`;
  return `${(v / 86400).toFixed(1)}d`;
};

const ageSeconds = (iso: string | null): number | null => {
  if (!iso) return null;
  const t = Date.parse(iso);
  return isNaN(t) ? null : (Date.now() - t) / 1000;
};

const fmtRelative = (iso: string | null): string => {
  const s = ageSeconds(iso);
  return s == null ? '—' : `${fmtAgeSeconds(s)} ago`;
};

const workerLightColor = (light: WorkerRow['light']): string => {
  switch (light) {
    case 'ok': return colors.success;
    case 'stale': return colors.warning;
    case 'unknown': return colors.warning;
    case 'error':
    case 'stalled': return colors.danger;
  }
};

// ── Sections ────────────────────────────────────────────────────────────────

const WorkerLights: React.FC<{ workers: WorkerRow[] }> = ({ workers }) => (
  <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap', alignItems: 'center' }}>
    {workers.map(w => (
      <span
        key={w.name}
        title={[
          `light: ${w.light}`,
          w.last_beat_at ? `last beat ${fmtRelative(w.last_beat_at)}` : 'NEVER BEAT (no heartbeat row)',
          w.cadence_seconds > 0 ? `cadence ${fmtAgeSeconds(w.cadence_seconds)}` : '',
          `last run: ${w.last_run_status}${w.last_run_at ? ` (${fmtRelative(w.last_run_at)})` : ''}`,
          w.last_error ? `last error: ${w.last_error}` : '',
        ].filter(Boolean).join(' · ')}
        style={{
          display: 'inline-flex', alignItems: 'center', gap: 6, fontSize: 11,
          fontFamily: 'monospace', color: colors.text,
          background: alpha(workerLightColor(w.light), '14'),
          border: `1px solid ${alpha(workerLightColor(w.light), '44')}`,
          borderRadius: 999, padding: '3px 10px',
        }}
      >
        <span style={{ width: 8, height: 8, borderRadius: 999, background: workerLightColor(w.light) }} />
        {w.name}
        <span style={{ color: colors.textMuted, textTransform: 'uppercase', fontSize: 9.5 }}>{w.light}</span>
      </span>
    ))}
  </div>
);

// ── Page ────────────────────────────────────────────────────────────────────

export const SegmentationCommand: React.FC = () => {
  const [nonce, setNonce] = useState(0);
  const [state, retry] = useHealthFetch(nonce);
  const d = state.data;

  return (
    <div style={pageStyle}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'flex-end', marginBottom: 8 }}>
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

      {/* ── The dashboard. Own endpoint + polling + states; independent of the
           health fetch below so neither surface can blank the other. ── */}
      <div style={{ marginBottom: 16 }}>
        <SegmentFreshnessBoard refreshNonce={nonce} />
      </div>

      {state.loading && !d && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, color: colors.textMuted, fontSize: 13, padding: '18px 4px' }}>
          <FontAwesomeIcon icon={faSpinner} spin /> Loading segmentation health…
        </div>
      )}

      {!state.loading && state.unavailable && (
        <div style={{
          padding: '14px 16px', borderRadius: 8, fontSize: 12.5, lineHeight: 1.6,
          background: alpha(colors.warning, '14'), border: `1px solid ${alpha(colors.warning, '44')}`,
          color: colors.warning,
        }}>
          <strong>SOURCE UNAVAILABLE.</strong> <code style={{ fontFamily: 'monospace' }}>/api/mailing/segmentation/health</code> is
          not exposed by this server build — the Segmentation backend has not been deployed here yet.
        </div>
      )}

      {!state.loading && state.error && (
        <SectionError label="segmentation health" error={state.error} onRetry={retry} />
      )}

      {!state.error && !state.unavailable && d && (
        <div style={panelStyle}>
          <SectionHeader title="Segmentation workers & estate totals" icon={faHeartPulse} />
          <div style={{ display: 'flex', gap: 26, flexWrap: 'wrap', marginBottom: 12 }}>
            <Stat label="Segments" value={fmtInt(d.summary.segments_total)}
              sub="non-archived segments in this org" />
            <Stat label="Workers" value={d.workers.length}
              sub="segmentation workers tracked" />
          </div>
          <WorkerLights workers={d.workers} />
          <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 10, lineHeight: 1.6 }}>
            Worker dots: hover for last beat, cadence, last run status and last error.
            To clean a segment through EmailOversight, use the <strong>Export &amp; Import</strong> tab. It has a segment picker.
          </div>
        </div>
      )}
    </div>
  );
};

export default SegmentationCommand;
