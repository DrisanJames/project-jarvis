import React, { useCallback, useEffect, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faGears, faHeartPulse, faLayerGroup, faClipboardCheck, faSeedling,
  faSpinner, faRotate, faChevronDown, faChevronRight,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import {
  colors, alpha, stateColor, pageStyle, panelStyle, tableStyle,
  thStyle, tdStyle, numTd, numTh,
} from '../shared/theme';
import { SectionHeader, Stat, Pill, SectionError, EmptyState } from '../shared/ui';

// =============================================================================
// OPERATIONS CONSOLE — Platform Coalition WS3 (REQ-C20)
// =============================================================================
// The UI face of the coalition: every registered job/worker (owner, cadence,
// heartbeat, health), segment-family freshness vs SLA, the nightly send-day
// invariant history, and the aggregate engager (cohort) trajectory.
//
// Sources (contract: tasks/eng-team/coalition/SCHEMA-CONTRACTS.md):
//   - GET /api/worker-health                       (heartbeats, exists today)
//   - GET /api/mailing/segment-registry/freshness  (WS2 §1, deploy-held)
//   - GET /api/mailing/ops/job-runs?worker=…       (WS3 §6, deploy-held) —
//     history rows written by the Python jobs (WS4 requirements filed in
//     DECISIONS.md): job:send_day_invariants, job:cohort_growth.
//
// FilterBar note (PORTAL_DESIGN_SYSTEM §3): this screen exposes the EMPTY
// subset of the canonical filter vocabulary — every panel is current platform
// state (no Denver date range, brand, ISP, or transport binds to any of these
// sources), so mounting the shared FilterBar would violate "filters must
// actually bind". No bespoke filters are invented either.
//
// State honesty (METRIC_CONTRACT / AS-6.1): each panel distinguishes loading,
// fetch error (with retry), endpoint-not-on-this-build (deploy held), source
// table not migrated, "never recorded", and genuinely-empty results.

// PAGE_VERSION 1.0 — initial Operations console (Coalition WS3, 2026-07-23).
const PAGE_VERSION = '1.0';

// ── API shapes (SCHEMA-CONTRACTS.md; do not rename without a contract rev) ──

interface WorkerHealthRow {
  worker_name: string;
  last_beat_at: string;
  seconds_since_beat: number;
  last_status: string;
  last_error?: string;
  cycle_count: number;
  expected_interval_seconds: number;
  stalled: boolean;
  stalled_since?: string | null;
}

interface WorkerHealthResponse {
  api_version: string;
  generated_at: string;
  stalled_count: number;
  workers: WorkerHealthRow[];
}

interface FreshnessHeartbeat {
  last_beat_at: string;
  last_status: string;
  stalled: boolean;
}

interface FreshnessRow {
  id: string;
  family_key: string;
  family_pattern: string;
  owner: string;
  cadence: string;
  sla_hours: number;
  keep_policy: string;
  active: boolean;
  segments_count: number;
  members_estimate: number;
  newest_refresh_at: string | null; // null = NEVER BUILT (AS-6.1)
  oldest_refresh_at: string | null;
  heartbeat_worker: string;
  heartbeat: FreshnessHeartbeat | null;
  sla_state: 'ok' | 'breached' | 'no_sla';
}

interface FreshnessResponse {
  generated_at: string;
  families: FreshnessRow[];
}

interface JobRun {
  started_at: string;
  finished_at: string;
  duration_ms: number;
  status: string;
  items_processed: number;
  items_failed: number;
  detail: string;
}

interface JobRunsResponse {
  api_version: string;
  generated_at: string;
  worker: string;
  table_present: boolean;
  runs: JobRun[];
}

// detail JSON of job:send_day_invariants (mirrors invariants_history.jsonl)
interface InvariantRunDetail {
  run_at: string;
  date: string;
  board: string;
  verdict: string; // GREEN | RED
  results: Record<string, { status: string; detail: string }>;
}

// detail JSON of job:cohort_growth (SCHEMA-CONTRACTS §6 / DECISIONS WS4 req)
interface CohortSnapshotDetail {
  date: string;
  snapshot: Record<string, Record<string, number>>; // isp → slot → distinct emails
}

const CLK = 'C_CLICK_ACTION_30D';
const OPN = 'C_OPEN_REAL_21D';

// ── Panel fetch plumbing ────────────────────────────────────────────────────

interface PanelState<T> {
  loading: boolean;
  error: string | null;
  /** true when the server build lacks the endpoint (404) — deploy-held Go. */
  unavailable: boolean;
  data: T | null;
  fetchedAt: string | null;
  ms: number | null;
}

const initialPanel = <T,>(): PanelState<T> => ({
  loading: true, error: null, unavailable: false, data: null, fetchedAt: null, ms: null,
});

function usePanelFetch<T>(url: string, nonce: number): [PanelState<T>, () => void] {
  const [state, setState] = useState<PanelState<T>>(initialPanel<T>());
  const [localNonce, setLocalNonce] = useState(0);
  const retry = useCallback(() => setLocalNonce(n => n + 1), []);
  const abortRef = useRef<AbortController | null>(null);

  useEffect(() => {
    abortRef.current?.abort();
    const ac = new AbortController();
    abortRef.current = ac;
    setState(s => ({ ...s, loading: true, error: null }));
    const t0 = performance.now();
    apiFetch(url, { signal: ac.signal })
      .then(async res => {
        const ms = Math.round(performance.now() - t0);
        const fetchedAt = new Date().toLocaleTimeString();
        if (res.status === 404) {
          // Route absent on this server build — the deploy-held state, said out loud.
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
        const data = (await res.json()) as T;
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
  }, [url, nonce, localNonce]);

  return [state, retry];
}

// ── Small display helpers ───────────────────────────────────────────────────

const fmtAge = (seconds: number | null | undefined): string => {
  if (seconds == null || !isFinite(seconds)) return '—';
  const s = Math.max(0, seconds);
  if (s < 90) return `${Math.round(s)}s`;
  if (s < 5400) return `${Math.round(s / 60)}m`;
  if (s < 172800) return `${(s / 3600).toFixed(1)}h`;
  return `${(s / 86400).toFixed(1)}d`;
};

const ageSeconds = (iso: string | null): number | null => {
  if (!iso) return null;
  const t = Date.parse(iso);
  return isNaN(t) ? null : (Date.now() - t) / 1000;
};

const fmtCadence = (seconds: number): string => {
  if (seconds <= 0) return '—';
  if (seconds % 3600 === 0) return `${seconds / 3600}h`;
  if (seconds % 60 === 0) return `${seconds / 60}m`;
  return `${seconds}s`;
};

const fmtInt = (n: number): string => n.toLocaleString();

const fmtDelta = (cur: number, prev: number | null): { text: string; color: string } => {
  if (prev == null || prev === 0) return { text: '—', color: colors.textMuted };
  const d = cur - prev;
  const pct = (d / prev) * 100;
  const sign = d > 0 ? '+' : '';
  const color = d > 0 ? colors.success : d < 0 ? colors.danger : colors.textMuted;
  return { text: `${sign}${fmtInt(d)} (${sign}${pct.toFixed(1)}%)`, color };
};

const fetchNote = (p: PanelState<unknown>): string =>
  p.fetchedAt ? `fetched ${p.fetchedAt}${p.ms != null ? ` · ${p.ms}ms` : ''}` : '';

const LoadingRow: React.FC<{ label: string }> = ({ label }) => (
  <div style={{ display: 'flex', alignItems: 'center', gap: 10, color: colors.textMuted, fontSize: 13, padding: '18px 4px' }}>
    <FontAwesomeIcon icon={faSpinner} spin /> {label}
  </div>
);

// Explicit "this source is not reachable on this build" display — an
// unavailable source must SAY it is unavailable (metric-contract rule).
const UnavailableNote: React.FC<{ endpoint: string; detail: string }> = ({ endpoint, detail }) => (
  <div style={{
    padding: '14px 16px', borderRadius: 8, fontSize: 12.5, lineHeight: 1.6,
    background: alpha(colors.warning, '14'), border: `1px solid ${alpha(colors.warning, '44')}`,
    color: colors.warning,
  }}>
    <strong>SOURCE UNAVAILABLE.</strong> <code style={{ fontFamily: 'monospace' }}>{endpoint}</code> is
    not exposed by this server build. {detail}
  </div>
);

const scrollWrap: React.CSSProperties = { overflowX: 'auto' };
const monoCell: React.CSSProperties = { ...tdStyle, fontFamily: 'monospace', fontSize: 11.5 };

// ── Section: JOBS ───────────────────────────────────────────────────────────

const jobHealth = (w: WorkerHealthRow): string => {
  if (w.stalled) return 'stalled';
  if (w.last_status && w.last_status !== 'ok') return 'error';
  const threshold = Math.max(w.expected_interval_seconds * 2, 300); // mirrors segmentWorkerOnline
  if (w.expected_interval_seconds > 0 && w.seconds_since_beat > threshold) return 'stale';
  return 'ok';
};

const jobHealthColor = (h: string): string =>
  h === 'stalled' || h === 'error' ? colors.danger : h === 'stale' ? colors.warning : colors.success;

const JobsSection: React.FC<{ nonce: number; families: FreshnessRow[] | null }> = ({ nonce, families }) => {
  const [panel, retry] = usePanelFetch<WorkerHealthResponse>('/api/worker-health', nonce);

  // Owner annotation: a registry family that declares this heartbeat_worker
  // names the owning process (client-side join; the heartbeat table itself is
  // ownerless by design).
  const ownerFor = (name: string): string => {
    const fam = families?.find(f => f.heartbeat_worker === name);
    if (fam) return fam.owner;
    return name.startsWith('job:') ? 'external job (cron, agents/jobs scaffold)' : 'platform worker (Go, in-process)';
  };

  return (
    <div style={panelStyle}>
      <SectionHeader
        title="Jobs & Workers — heartbeat health"
        icon={faHeartPulse}
        right={<span style={{ fontSize: 11, color: colors.textMuted }}>{fetchNote(panel)}</span>}
      />
      {panel.loading && <LoadingRow label="Loading worker heartbeats…" />}
      {!panel.loading && panel.unavailable && (
        <UnavailableNote endpoint="/api/worker-health" detail="Heartbeat health cannot be shown." />
      )}
      {!panel.loading && panel.error && (
        <SectionError label="worker heartbeats" error={panel.error} onRetry={retry} />
      )}
      {!panel.loading && !panel.error && !panel.unavailable && panel.data && (() => {
        const workers = panel.data.workers;
        if (workers.length === 0) {
          return (
            <EmptyState
              title="No heartbeats recorded"
              hint="mailing_worker_heartbeats has no rows — no instrumented worker or scaffold job has ever beat on this environment."
            />
          );
        }
        const byHealth = (h: string) => workers.filter(w => jobHealth(w) === h).length;
        const external = workers.filter(w => w.worker_name.startsWith('job:')).length;
        return (
          <>
            <div style={{ display: 'flex', gap: 26, flexWrap: 'wrap', marginBottom: 14 }}>
              <Stat label="Instrumented" value={workers.length} sub={`${external} external job:* · ${workers.length - external} platform`} />
              <Stat label="OK" value={byHealth('ok')} color={colors.success} />
              <Stat label="Stale" value={byHealth('stale')} color={byHealth('stale') > 0 ? colors.warning : colors.textMuted}
                sub="beat older than 2× cadence" />
              <Stat label="Stalled / Error" value={byHealth('stalled') + byHealth('error')}
                color={byHealth('stalled') + byHealth('error') > 0 ? colors.danger : colors.textMuted}
                sub="monitor-flagged or last run failed" />
            </div>
            <div style={scrollWrap}>
              <table style={tableStyle}>
                <thead>
                  <tr>
                    <th style={thStyle}>Job / Worker</th>
                    <th style={thStyle}>Kind</th>
                    <th style={thStyle}>Owner</th>
                    <th style={numTh}>Cadence</th>
                    <th style={numTh}>Last beat</th>
                    <th style={numTh}>Cycles</th>
                    <th style={thStyle}>Health</th>
                    <th style={thStyle}>Last error</th>
                  </tr>
                </thead>
                <tbody>
                  {workers.map(w => {
                    const h = jobHealth(w);
                    return (
                      <tr key={w.worker_name}>
                        <td style={monoCell}>{w.worker_name}</td>
                        <td style={tdStyle}>{w.worker_name.startsWith('job:') ? 'external' : 'platform'}</td>
                        <td style={{ ...tdStyle, maxWidth: 260, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={ownerFor(w.worker_name)}>
                          {ownerFor(w.worker_name)}
                        </td>
                        <td style={numTd} title={`expected_interval_seconds=${w.expected_interval_seconds}`}>
                          {fmtCadence(w.expected_interval_seconds)}
                        </td>
                        <td style={numTd} title={w.last_beat_at}>{fmtAge(w.seconds_since_beat)} ago</td>
                        <td style={numTd}>{fmtInt(w.cycle_count)}</td>
                        <td style={tdStyle}>
                          <Pill color={jobHealthColor(h)}>{h.toUpperCase()}</Pill>
                        </td>
                        <td style={{ ...tdStyle, maxWidth: 280, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', color: w.last_error ? colors.danger : colors.textMuted }}
                          title={w.last_error || ''}>
                          {w.last_error || '—'}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          </>
        );
      })()}
    </div>
  );
};

// ── Section: SEGMENT FAMILIES ───────────────────────────────────────────────

interface FamilyDisplayState { label: string; color: string }

const familyState = (f: FreshnessRow): FamilyDisplayState => {
  if (!f.active) return { label: 'DISABLED', color: colors.idle };
  if (f.owner.toUpperCase().startsWith('RETIRED')) return { label: 'RETIRED', color: colors.idle };
  if (f.newest_refresh_at == null) return { label: 'NEVER BUILT', color: colors.danger };
  if (f.sla_state === 'breached') return { label: 'STALE', color: colors.warning };
  if (f.sla_state === 'no_sla') return { label: 'NO SLA', color: colors.textMuted };
  return { label: 'FRESH', color: colors.success };
};

const FamiliesSection: React.FC<{
  panel: PanelState<FreshnessResponse>;
  retry: () => void;
}> = ({ panel, retry }) => (
  <div style={panelStyle}>
    <SectionHeader
      title="Segment Families — freshness vs SLA"
      icon={faLayerGroup}
      right={<span style={{ fontSize: 11, color: colors.textMuted }}>{fetchNote(panel)}</span>}
    />
    {panel.loading && <LoadingRow label="Loading segment-family registry…" />}
    {!panel.loading && panel.unavailable && (
      <UnavailableNote
        endpoint="/api/mailing/segment-registry/freshness"
        detail="The WS2 registry API (upside-down d3745f2) has not been deployed to this server — family ownership and freshness cannot be shown until the operator releases the held deploy."
      />
    )}
    {!panel.loading && panel.error && (
      <SectionError label="segment-family freshness" error={panel.error} onRetry={retry} />
    )}
    {!panel.loading && !panel.error && !panel.unavailable && panel.data && (
      panel.data.families.length === 0 ? (
        <EmptyState
          title="Registry is empty"
          hint="mailing_segment_registry has no rows for this org — the seed migration has not run here. This is 'built empty', not an error."
        />
      ) : (
        <div style={scrollWrap}>
          <table style={tableStyle}>
            <thead>
              <tr>
                <th style={thStyle}>Family</th>
                <th style={thStyle}>Pattern</th>
                <th style={thStyle}>Owner</th>
                <th style={thStyle}>Cadence</th>
                <th style={numTh}>Segments</th>
                <th style={numTh}>Members est.</th>
                <th style={numTh}>Newest refresh</th>
                <th style={numTh}>SLA</th>
                <th style={thStyle}>State</th>
                <th style={thStyle}>Keep policy</th>
              </tr>
            </thead>
            <tbody>
              {panel.data.families.map(f => {
                const st = familyState(f);
                const refreshAge = ageSeconds(f.newest_refresh_at);
                return (
                  <tr key={f.id}>
                    <td style={{ ...tdStyle, fontWeight: 600 }}>{f.family_key}</td>
                    <td style={monoCell} title={f.family_pattern}>{f.family_pattern}</td>
                    <td style={{ ...tdStyle, maxWidth: 240, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={f.owner}>{f.owner}</td>
                    <td style={tdStyle}>{f.cadence || '—'}</td>
                    <td style={numTd}>{fmtInt(f.segments_count)}</td>
                    <td style={numTd} title="SUM(subscriber_count) over matching non-archived segments — an estimate, not a deduped count">
                      {fmtInt(f.members_estimate)}*
                    </td>
                    <td style={numTd} title={f.newest_refresh_at ?? 'never built'}>
                      {f.newest_refresh_at == null ? '—' : `${fmtAge(refreshAge)} ago`}
                    </td>
                    <td style={numTd}>{f.sla_hours > 0 ? `${f.sla_hours}h` : '—'}</td>
                    <td style={tdStyle}><Pill color={st.color}>{st.label}</Pill></td>
                    <td style={tdStyle}>
                      <Pill color={f.keep_policy === 'purgeable' ? colors.warning : colors.indigo400}>
                        {f.keep_policy.toUpperCase()}
                      </Pill>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
          <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 8 }}>
            * Members est. = SUM(subscriber_count) across the family's segments (overlap not deduped).
            NEVER BUILT = no matching segment has a last_calculated_at — distinct from a registry family whose segments exist but are empty.
          </div>
        </div>
      )
    )}
  </div>
);

// ── Section: INVARIANTS ─────────────────────────────────────────────────────

const parseDetail = <T,>(raw: string): T | null => {
  try {
    return JSON.parse(raw) as T;
  } catch {
    return null;
  }
};

const invStatusColor = (s: string): string => {
  const u = s.toUpperCase();
  if (u === 'PASS') return colors.success;
  if (u === 'FAIL') return colors.danger;
  return colors.idle; // SKIP
};

const runsEmptyHint = (worker: string, wsReq: string): string =>
  `No ${worker} rows exist in mailing_worker_runs — the Python job does not yet write its results to the platform. ` +
  `Requirement filed for WS4 (${wsReq}, tasks/eng-team/coalition/DECISIONS.md); until it lands, history lives only in .scratch/reports/ on the operator machine.`;

const InvariantsSection: React.FC<{ nonce: number }> = ({ nonce }) => {
  const [panel, retry] = usePanelFetch<JobRunsResponse>(
    '/api/mailing/ops/job-runs?worker=job:send_day_invariants&limit=30', nonce);
  const [expandedRun, setExpandedRun] = useState<string | null>(null);

  return (
    <div style={panelStyle}>
      <SectionHeader
        title="Send-Day Invariants — nightly red/green history"
        icon={faClipboardCheck}
        right={<span style={{ fontSize: 11, color: colors.textMuted }}>{fetchNote(panel)}</span>}
      />
      {panel.loading && <LoadingRow label="Loading invariant-run history…" />}
      {!panel.loading && panel.unavailable && (
        <UnavailableNote
          endpoint="/api/mailing/ops/job-runs"
          detail="The WS3 job-runs read API is built on upside-down main but its deploy is held — invariant history cannot be shown until the operator releases it."
        />
      )}
      {!panel.loading && panel.error && (
        <SectionError label="invariant runs" error={panel.error} onRetry={retry} />
      )}
      {!panel.loading && !panel.error && !panel.unavailable && panel.data && (() => {
        if (!panel.data.table_present) {
          return (
            <UnavailableNote
              endpoint="mailing_worker_runs"
              detail="The runs table has not been migrated on this database — the startup migration (create_worker_runs) has not applied here."
            />
          );
        }
        const runs = panel.data.runs
          .map(r => ({ run: r, det: parseDetail<InvariantRunDetail>(r.detail) }))
          .filter((x): x is { run: JobRun; det: InvariantRunDetail } => x.det != null);
        if (runs.length === 0) {
          return (
            <EmptyState
              title="No invariant results recorded on the platform"
              hint={runsEmptyHint('job:send_day_invariants', 'invariant suite → mailing_worker_runs')}
            />
          );
        }
        const latest = runs[0];
        const invariantNames = Array.from(
          new Set(runs.flatMap(r => Object.keys(r.det.results)))
        ).sort();
        const columns = runs.slice(0, 14).reverse(); // oldest → newest, current period rightmost
        return (
          <>
            <div style={{ display: 'flex', gap: 26, flexWrap: 'wrap', marginBottom: 14 }}>
              <Stat
                label="Latest verdict"
                value={<Pill color={stateColor(latest.det.verdict === 'GREEN' ? 'pass' : 'fail')}>{latest.det.verdict}</Pill>}
                sub={`board ${latest.det.board} · send date ${latest.det.date}`}
              />
              <Stat label="Ran" value={`${fmtAge(ageSeconds(latest.run.started_at))} ago`} sub={latest.run.started_at} />
              <Stat
                label="Failing invariants"
                value={Object.values(latest.det.results).filter(x => x.status.toUpperCase() === 'FAIL').length}
                color={latest.det.verdict === 'GREEN' ? colors.textMuted : colors.danger}
                sub={`of ${Object.keys(latest.det.results).length} asserted (latest run)`}
              />
            </div>
            <div style={scrollWrap}>
              <table style={tableStyle}>
                <thead>
                  <tr>
                    <th style={thStyle}>Invariant</th>
                    {columns.map(c => (
                      <th key={c.run.started_at} style={{ ...thStyle, textAlign: 'center' }} title={`${c.det.board} · run ${c.run.started_at}`}>
                        {c.det.date.slice(5)}
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {invariantNames.map(name => (
                    <tr key={name}>
                      <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>{name}</td>
                      {columns.map(c => {
                        const res = c.det.results[name];
                        return (
                          <td key={c.run.started_at} style={{ ...tdStyle, textAlign: 'center' }}
                            title={res ? `${res.status}: ${res.detail}` : 'not asserted this run'}>
                            {res ? (
                              <span style={{
                                display: 'inline-block', width: 12, height: 12, borderRadius: 6,
                                background: invStatusColor(res.status),
                                opacity: res.status.toUpperCase() === 'SKIP' ? 0.45 : 1,
                              }} />
                            ) : <span style={{ color: colors.textMuted }}>·</span>}
                          </td>
                        );
                      })}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <div style={{ fontSize: 11, color: colors.textMuted, margin: '8px 0 10px' }}>
              Green = PASS · red = FAIL · dim = SKIP (with reason in the tooltip). Newest run is the rightmost column. Hover any cell for the assertion's own evidence line.
            </div>
            <button
              onClick={() => setExpandedRun(expandedRun ? null : latest.run.started_at)}
              style={{
                background: 'none', border: 'none', color: colors.indigo400, cursor: 'pointer',
                fontSize: 12, padding: 0, display: 'flex', alignItems: 'center', gap: 6,
              }}>
              <FontAwesomeIcon icon={expandedRun ? faChevronDown : faChevronRight} />
              Latest run detail ({latest.det.board})
            </button>
            {expandedRun && (
              <div style={{ marginTop: 8 }}>
                {Object.entries(latest.det.results).map(([name, res]) => (
                  <div key={name} style={{ display: 'flex', gap: 10, fontSize: 12, padding: '4px 0', borderBottom: `1px solid ${alpha(colors.indigo500, '14')}` }}>
                    <Pill color={invStatusColor(res.status)}>{res.status.toUpperCase()}</Pill>
                    <span style={{ fontWeight: 600, whiteSpace: 'nowrap' }}>{name}</span>
                    <span style={{ color: colors.textMuted }}>{res.detail}</span>
                  </div>
                ))}
              </div>
            )}
          </>
        );
      })()}
    </div>
  );
};

// ── Section: COHORT GROWTH ──────────────────────────────────────────────────

const CohortSection: React.FC<{ nonce: number }> = ({ nonce }) => {
  const [panel, retry] = usePanelFetch<JobRunsResponse>(
    '/api/mailing/ops/job-runs?worker=job:cohort_growth&limit=30', nonce);
  const [showPerIsp, setShowPerIsp] = useState(false);

  return (
    <div style={panelStyle}>
      <SectionHeader
        title="Cohort Growth — aggregate engager trajectory"
        icon={faSeedling}
        right={<span style={{ fontSize: 11, color: colors.textMuted }}>{fetchNote(panel)}</span>}
      />
      {panel.loading && <LoadingRow label="Loading cohort-growth snapshots…" />}
      {!panel.loading && panel.unavailable && (
        <UnavailableNote
          endpoint="/api/mailing/ops/job-runs"
          detail="The WS3 job-runs read API is built on upside-down main but its deploy is held — the cohort trajectory cannot be shown until the operator releases it."
        />
      )}
      {!panel.loading && panel.error && (
        <SectionError label="cohort growth" error={panel.error} onRetry={retry} />
      )}
      {!panel.loading && !panel.error && !panel.unavailable && panel.data && (() => {
        if (!panel.data.table_present) {
          return (
            <UnavailableNote
              endpoint="mailing_worker_runs"
              detail="The runs table has not been migrated on this database."
            />
          );
        }
        const snaps = panel.data.runs
          .map(r => parseDetail<CohortSnapshotDetail>(r.detail))
          .filter((d): d is CohortSnapshotDetail => d != null && typeof d.snapshot === 'object')
          .sort((a, b) => a.date.localeCompare(b.date)); // oldest → newest
        if (snaps.length === 0) {
          return (
            <EmptyState
              title="No cohort snapshots recorded on the platform"
              hint={runsEmptyHint('job:cohort_growth', 'cohort_growth.py → mailing_worker_runs')}
            />
          );
        }
        const agg = (s: CohortSnapshotDetail) => {
          let clk = 0, opn = 0;
          Object.values(s.snapshot).forEach(slots => {
            clk += slots[CLK] ?? 0;
            opn += slots[OPN] ?? 0;
          });
          return { clk, opn, tot: clk + opn };
        };
        const latest = agg(snaps[snaps.length - 1]);
        const prev = snaps.length > 1 ? agg(snaps[snaps.length - 2]) : null;
        const dClk = fmtDelta(latest.clk, prev?.clk ?? null);
        const dOpn = fmtDelta(latest.opn, prev?.opn ?? null);
        const dTot = fmtDelta(latest.tot, prev?.tot ?? null);
        const latestSnap = snaps[snaps.length - 1];
        const ispRows = Object.entries(latestSnap.snapshot)
          .map(([isp, slots]) => ({ isp, clk: slots[CLK] ?? 0, opn: slots[OPN] ?? 0 }))
          .sort((a, b) => (b.clk + b.opn) - (a.clk + a.opn));
        return (
          <>
            <div style={{ display: 'flex', gap: 26, flexWrap: 'wrap', marginBottom: 14 }}>
              <Stat label="Clickers 30D" value={fmtInt(latest.clk)}
                sub={<span style={{ color: dClk.color }}>{dClk.text} vs prior snapshot</span>}
                title="C_CLICK_ACTION_30D — distinct emails in the family slot ledger, excluded rows omitted" />
              <Stat label="Real openers 21D" value={fmtInt(latest.opn)}
                sub={<span style={{ color: dOpn.color }}>{dOpn.text} vs prior snapshot</span>}
                title="C_OPEN_REAL_21D — distinct emails in the family slot ledger, excluded rows omitted" />
              <Stat label="Engaged total" value={fmtInt(latest.tot)}
                sub={<span style={{ color: dTot.color }}>{dTot.text} vs prior snapshot</span>}
                title="Clickers 30D + Real openers 21D (slot-disjoint by ledger construction)" />
              <Stat label="Snapshot" value={latestSnap.date} sub={`${snaps.length} snapshots on record`} />
            </div>
            <div style={scrollWrap}>
              <table style={tableStyle}>
                <thead>
                  <tr>
                    <th style={thStyle}>Day</th>
                    <th style={numTh}>Clickers 30D</th>
                    <th style={numTh}>Δ</th>
                    <th style={numTh}>Real openers 21D</th>
                    <th style={numTh}>Δ</th>
                    <th style={numTh}>Engaged total</th>
                    <th style={numTh}>Δ</th>
                  </tr>
                </thead>
                <tbody>
                  {snaps.slice(-14).map((s, i, arr) => {
                    const cur = agg(s);
                    const before = i > 0 ? agg(arr[i - 1]) : null;
                    const dc = fmtDelta(cur.clk, before?.clk ?? null);
                    const dop = fmtDelta(cur.opn, before?.opn ?? null);
                    const dt = fmtDelta(cur.tot, before?.tot ?? null);
                    return (
                      <tr key={s.date}>
                        <td style={tdStyle}>{s.date}</td>
                        <td style={numTd}>{fmtInt(cur.clk)}</td>
                        <td style={{ ...numTd, color: dc.color }}>{dc.text}</td>
                        <td style={numTd}>{fmtInt(cur.opn)}</td>
                        <td style={{ ...numTd, color: dop.color }}>{dop.text}</td>
                        <td style={numTd}>{fmtInt(cur.tot)}</td>
                        <td style={{ ...numTd, color: dt.color }}>{dt.text}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
            <div style={{ fontSize: 11, color: colors.textMuted, margin: '8px 0 10px' }}>
              Counts are distinct emails per slot in mailing_family_slot_ledger (excluded rows omitted), snapshotted daily after the nightly cohort re-mint. Raw counts only — no human/machine split (operator standing rule). Δ denominators are the immediately prior snapshot.
            </div>
            <button
              onClick={() => setShowPerIsp(v => !v)}
              style={{
                background: 'none', border: 'none', color: colors.indigo400, cursor: 'pointer',
                fontSize: 12, padding: 0, display: 'flex', alignItems: 'center', gap: 6,
              }}>
              <FontAwesomeIcon icon={showPerIsp ? faChevronDown : faChevronRight} />
              Per-ISP breakdown (latest snapshot, {latestSnap.date})
            </button>
            {showPerIsp && (
              <div style={{ ...scrollWrap, marginTop: 8 }}>
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={thStyle}>ISP</th>
                      <th style={numTh}>Clickers 30D</th>
                      <th style={numTh}>Real openers 21D</th>
                      <th style={numTh}>Engaged total</th>
                    </tr>
                  </thead>
                  <tbody>
                    {ispRows.map(r => (
                      <tr key={r.isp}>
                        <td style={tdStyle}>{r.isp}</td>
                        <td style={numTd}>{fmtInt(r.clk)}</td>
                        <td style={numTd}>{fmtInt(r.opn)}</td>
                        <td style={numTd}>{fmtInt(r.clk + r.opn)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </>
        );
      })()}
    </div>
  );
};

// ── Page ────────────────────────────────────────────────────────────────────

export const OperationsConsole: React.FC = () => {
  const [nonce, setNonce] = useState(0);
  const [familiesPanel, retryFamilies] = usePanelFetch<FreshnessResponse>(
    '/api/mailing/segment-registry/freshness', nonce);

  return (
    <div style={pageStyle}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
        <div>
          <h2 style={{ margin: 0, fontSize: 20, display: 'flex', alignItems: 'center', gap: 10 }}>
            <FontAwesomeIcon icon={faGears} style={{ color: colors.indigo400 }} /> Operations
          </h2>
          <div style={{ fontSize: 12, color: colors.textMuted, marginTop: 4 }}>
            Platform ownership console — jobs, segment-family SLAs, send-day invariants, engager growth. v{PAGE_VERSION}
          </div>
        </div>
        <button
          onClick={() => setNonce(n => n + 1)}
          style={{
            background: alpha(colors.indigo500, '22'), border: `1px solid ${alpha(colors.indigo500, '66')}`,
            color: colors.text, borderRadius: 8, padding: '8px 14px', cursor: 'pointer', fontSize: 12.5,
            display: 'flex', alignItems: 'center', gap: 8,
          }}>
          <FontAwesomeIcon icon={faRotate} /> Refresh all
        </button>
      </div>

      <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
        <JobsSection nonce={nonce} families={familiesPanel.data?.families ?? null} />
        <FamiliesSection panel={familiesPanel} retry={retryFamilies} />
        <InvariantsSection nonce={nonce} />
        <CohortSection nonce={nonce} />
      </div>
    </div>
  );
};

export default OperationsConsole;
