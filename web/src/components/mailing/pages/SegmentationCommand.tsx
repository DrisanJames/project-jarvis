import React, { useCallback, useEffect, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faLayerGroup, faHeartPulse, faTriangleExclamation, faRotate, faSpinner,
  faChevronDown, faChevronRight,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import {
  colors, alpha, pageStyle, panelStyle, tableStyle,
  thStyle, tdStyle, numTd, numTh,
} from '../shared/theme';
import { SectionHeader, Stat, Pill, SectionError, EmptyState } from '../shared/ui';

// =============================================================================
// SEGMENTATION COMMAND — the life-blood screen
// =============================================================================
// The operator's segmentation truth surface. Its one job: make MEMBERSHIP
// staleness impossible to hide. The frozen-segment defect class (band counts
// frozen since jul19, a gmail recalc cron that never ran, a rollup that ran
// once) survived for weeks because every surface showed counts-refresh
// ("segment_refresh OK") while membership rebuilt on a 28-day rotation.
//
// Source: GET /api/mailing/segmentation/health (internal/api/
// segmentation_health.go) — every number on this screen comes from that
// endpoint; nothing is derived client-side beyond formatting and sums the
// endpoint already exposes.
//
// FilterBar note (PORTAL_DESIGN_SYSTEM §3): this screen shows current
// platform state — none of the canonical filter vocabulary (Denver date
// range, brand, ISP, transport) binds to these sources, so the shared
// FilterBar is deliberately not mounted (same ruling as OperationsConsole).
//
// State honesty (§1.6): loading, fetch-error-with-retry, endpoint-not-on-
// this-build (404 = deploy held), and genuinely-empty are all distinct.

// PAGE_VERSION 1.0 — initial Segmentation Command screen (2026-07-26).
const PAGE_VERSION = '1.0';

// ── API shapes (mirror segmentation_health.go; do not drift) ────────────────

type FamilyVerdict = 'LIVE' | 'STALE' | 'STATIC-DECLARED' | 'UNREGISTERED';

interface SegRow {
  id: string;
  name: string;
  segment_type: string;
  subscriber_count: number;
  last_count_at: string | null;
  last_built_at: string | null;
  last_build_status: string;
  ledger_count: number;
  build_source: string;
  member_inserts_24h: number | null; // null = churn unavailable
  campaign_refs_3d: number;
  count_membership_diverged: boolean;
}

interface FamilyRow {
  family_key: string;
  family_pattern: string;
  registered: boolean;
  owner: string;
  cadence: string;
  sla_hours: number;
  keep_policy: string;
  verdict: FamilyVerdict;
  segments_count: number;
  dynamic_count: number;
  static_count: number;
  subscriber_total: number;
  last_membership_build_min: string | null;
  last_membership_build_max: string | null;
  builds_last_24h: number;
  failed_now: number;
  member_inserts_24h: number;
  member_inserts_7d: number;
  campaign_refs_3d: number;
  diverged_count: number;
  segments: SegRow[];
  segments_truncated: boolean;
}

interface WorkerRow {
  name: string;
  kind: string;
  cadence_seconds: number;
  last_beat_at: string | null;
  seconds_since_beat: number | null;
  last_status: string;
  last_error: string;
  cycle_count: number;
  stalled: boolean;
  last_run_status: string;
  last_run_at: string | null;
  light: 'ok' | 'stale' | 'error' | 'stalled' | 'unknown';
}

interface AlertRow {
  severity: 'red' | 'amber';
  verdict: string;
  family_key: string;
  message: string;
  blast_radius: number;
  hours_overdue: number;
}

interface HealthSummary {
  families_live: number;
  families_stale: number;
  families_static: number;
  families_unregistered: number;
  segments_total: number;
  subscriber_total: number;
}

interface HealthResponse {
  api_version: string;
  generated_at: string;
  summary: HealthSummary;
  families: FamilyRow[];
  workers: WorkerRow[];
  alerts: AlertRow[];
  churn_method: string; // 'materialized_window' | 'unavailable'
  refs_method: string;  // 'inclusion_segments_3d' | 'unavailable'
  segments_truncated: boolean;
  registry_available: boolean;
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
    apiFetch('/api/mailing/segmentation/health', { signal: ac.signal })
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

const verdictColor = (v: FamilyVerdict): string => {
  switch (v) {
    case 'LIVE': return colors.success;
    case 'STALE': return colors.danger;
    case 'UNREGISTERED': return colors.warning;
    case 'STATIC-DECLARED': return colors.idle;
  }
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

// Is the family's newest membership build past its SLA? (Server verdict is
// authoritative — this only picks the red text styling for the age cell.)
const buildAgeStyle = (f: FamilyRow): React.CSSProperties => {
  if (f.verdict === 'STALE') return { color: colors.danger, fontWeight: 700 };
  return {};
};

const scrollWrap: React.CSSProperties = { overflowX: 'auto' };
const monoCell: React.CSSProperties = { ...tdStyle, fontFamily: 'monospace', fontSize: 11.5 };

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

const AlertsPanel: React.FC<{ alerts: AlertRow[] }> = ({ alerts }) => (
  <div style={panelStyle}>
    <SectionHeader title="Alerts — ranked by blast radius" icon={faTriangleExclamation} />
    {alerts.length === 0 ? (
      <div style={{ fontSize: 12.5, color: colors.successText }}>
        No STALE or UNREGISTERED families. Every family is either inside its SLA or declared static.
      </div>
    ) : (
      alerts.map((a, i) => {
        const c = a.severity === 'red' ? colors.danger : colors.warning;
        return (
          <div
            key={`${a.family_key}-${i}`}
            style={{
              display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap',
              padding: '8px 12px', marginBottom: 6, borderRadius: 8,
              background: alpha(c, '14'), border: `1px solid ${alpha(c, '44')}`,
            }}
          >
            <Pill color={c}>{a.verdict}</Pill>
            <span style={{ fontWeight: 700, fontSize: 13 }}>{a.family_key}</span>
            <span style={{ fontSize: 12, color: colors.text }}>{a.message}</span>
            <span style={{ fontSize: 11.5, color: colors.textMuted, marginLeft: 'auto', fontVariantNumeric: 'tabular-nums' }}>
              blast radius {fmtInt(a.blast_radius)} members
              {a.hours_overdue > 0 && <span style={{ color: c }}> · {a.hours_overdue.toFixed(1)}h past SLA</span>}
            </span>
          </div>
        );
      })
    )}
  </div>
);

const SegmentDrilldown: React.FC<{ family: FamilyRow; churnAvailable: boolean }> = ({ family, churnAvailable }) => (
  <div style={{ ...scrollWrap, padding: '4px 8px 10px 28px' }}>
    <table style={tableStyle}>
      <thead>
        <tr>
          <th style={thStyle}>Segment</th>
          <th style={thStyle}>Type</th>
          <th style={numTh}>Count (segments)</th>
          <th style={numTh}>Count (ledger)</th>
          <th style={numTh}>Membership built</th>
          <th style={numTh}>Counts refreshed</th>
          <th style={thStyle}>Build status</th>
          <th style={thStyle}>Source</th>
          <th style={numTh}>Inserts 24h</th>
          <th style={numTh}>Refs 3d</th>
          <th style={thStyle}>Divergence</th>
        </tr>
      </thead>
      <tbody>
        {family.segments.map(s => (
          <tr key={s.id}>
            <td style={{ ...monoCell, maxWidth: 320, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={`${s.name} · ${s.id}`}>
              {s.name}
            </td>
            <td style={tdStyle}>{s.segment_type}</td>
            <td style={numTd}>{fmtInt(s.subscriber_count)}</td>
            <td style={numTd}>{fmtInt(s.ledger_count)}</td>
            <td style={{ ...numTd, ...(s.last_built_at == null ? { color: colors.danger, fontWeight: 700 } : {}) }}
              title={s.last_built_at ?? 'no ledger build recorded'}>
              {s.last_built_at == null ? 'NEVER' : fmtRelative(s.last_built_at)}
            </td>
            <td style={numTd} title={s.last_count_at ?? 'never counted'}>{fmtRelative(s.last_count_at)}</td>
            <td style={tdStyle}>
              <Pill color={s.last_build_status === 'ok' ? colors.success
                : s.last_build_status === 'none' ? colors.idle
                : s.last_build_status === 'running' ? colors.info : colors.danger}>
                {s.last_build_status}
              </Pill>
            </td>
            <td style={{ ...tdStyle, color: colors.textMuted }}>{s.build_source || '—'}</td>
            <td style={numTd} title={churnAvailable ? 'rows (re)materialized in the last 24h' : 'churn source unavailable this fetch'}>
              {s.member_inserts_24h == null ? '—*' : fmtInt(s.member_inserts_24h)}
            </td>
            <td style={numTd}>{s.campaign_refs_3d > 0 ? fmtInt(s.campaign_refs_3d) : '—'}</td>
            <td style={tdStyle}>
              {s.count_membership_diverged ? (
                <Pill color={colors.warning} style={{ whiteSpace: 'nowrap' }}>COUNTS≠BUILD</Pill>
              ) : (
                <span style={{ color: colors.textFaint }}>—</span>
              )}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
    {family.segments_truncated && (
      <div style={{ fontSize: 11, color: colors.warningText, marginTop: 6 }}>
        Showing the {family.segments.length} most recently built segments — the family has {fmtInt(family.segments_count)} total.
      </div>
    )}
  </div>
);

// ── Page ────────────────────────────────────────────────────────────────────

export const SegmentationCommand: React.FC = () => {
  const [nonce, setNonce] = useState(0);
  const [state, retry] = useHealthFetch(nonce);
  const [expanded, setExpanded] = useState<string | null>(null);

  const d = state.data;
  const churnAvailable = d?.churn_method === 'materialized_window';
  const refsAvailable = d?.refs_method === 'inclusion_segments_3d';

  return (
    <div style={pageStyle}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
        <div>
          <h2 style={{ margin: 0, fontSize: 20, display: 'flex', alignItems: 'center', gap: 10 }}>
            <FontAwesomeIcon icon={faLayerGroup} style={{ color: colors.indigo400 }} /> Segmentation Command
          </h2>
          <div style={{ fontSize: 12, color: colors.textMuted, marginTop: 4 }}>
            Membership-build truth per segment family — verdicts against declared SLAs, worker liveness, churn and campaign use. v{PAGE_VERSION}
            {state.fetchedAt && <span> · fetched {state.fetchedAt}{state.ms != null ? ` · ${state.ms}ms` : ''}</span>}
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
          not exposed by this server build — the Segmentation Command backend has not been deployed here yet.
        </div>
      )}

      {!state.loading && state.error && (
        <SectionError label="segmentation health" error={state.error} onRetry={retry} />
      )}

      {!state.loading && !state.error && !state.unavailable && d && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
          {/* ── Top strip: estate verdict + worker lights ── */}
          <div style={panelStyle}>
            <SectionHeader title="Estate verdict" icon={faHeartPulse} />
            <div style={{ display: 'flex', gap: 26, flexWrap: 'wrap', marginBottom: 12 }}>
              <Stat label="Live" value={d.summary.families_live} color={colors.success}
                sub="membership built inside SLA" />
              <Stat label="Stale" value={d.summary.families_stale}
                color={d.summary.families_stale > 0 ? colors.danger : colors.textMuted}
                sub="membership build past SLA" />
              <Stat label="Unregistered" value={d.summary.families_unregistered}
                color={d.summary.families_unregistered > 0 ? colors.warning : colors.textMuted}
                sub="no registry owner — invisible to alarms" />
              <Stat label="Static-declared" value={d.summary.families_static} color={colors.idle}
                sub="family declares no refresh SLA" />
              <Stat label="Segments" value={fmtInt(d.summary.segments_total)}
                sub={`across ${d.families.length} families`} />
              <Stat label="Members" value={fmtInt(d.summary.subscriber_total)}
                sub="SUM of segment counts (overlap not deduped)*" />
            </div>
            <WorkerLights workers={d.workers} />
            {!d.registry_available && (
              <div style={{ fontSize: 11.5, color: colors.warningText, marginTop: 10 }}>
                Registry source failed on this fetch — every family below shows UNREGISTERED by construction, not by truth. Retry before acting.
              </div>
            )}
            {d.segments_truncated && (
              <div style={{ fontSize: 11.5, color: colors.warningText, marginTop: 10 }}>
                Segment scan hit its hard bound — family aggregates below are computed over a truncated set.
              </div>
            )}
          </div>

          {/* ── Alerts ── */}
          <AlertsPanel alerts={d.alerts} />

          {/* ── Family table ── */}
          <div style={panelStyle}>
            <SectionHeader title="Segment families — membership build vs SLA" icon={faLayerGroup} />
            {d.families.length === 0 ? (
              <EmptyState
                title="No segments found"
                hint="mailing_segments has no non-archived rows for this org — nothing to verdict."
              />
            ) : (
              <div style={scrollWrap}>
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={thStyle} />
                      <th style={thStyle}>Family</th>
                      <th style={thStyle}>Verdict</th>
                      <th style={thStyle}>Owner</th>
                      <th style={numTh}>SLA</th>
                      <th style={numTh}>Segments</th>
                      <th style={numTh}>Members</th>
                      <th style={numTh}>Last build</th>
                      <th style={numTh}>Builds 24h</th>
                      <th style={numTh}>Inserts 24h</th>
                      <th style={numTh}>Refs 3d</th>
                      <th style={thStyle}>Flags</th>
                    </tr>
                  </thead>
                  <tbody>
                    {d.families.map(f => {
                      const rowKey = `${f.registered ? 'reg' : 'unreg'}:${f.family_pattern}`;
                      const isOpen = expanded === rowKey;
                      return (
                        <React.Fragment key={rowKey}>
                          <tr
                            onClick={() => setExpanded(isOpen ? null : rowKey)}
                            style={{ cursor: 'pointer' }}
                            title={`pattern ${f.family_pattern}${f.cadence ? ` · cadence ${f.cadence}` : ''}`}
                          >
                            <td style={{ ...tdStyle, width: 24, color: colors.textMuted }}>
                              <FontAwesomeIcon icon={isOpen ? faChevronDown : faChevronRight} />
                            </td>
                            <td style={{ ...tdStyle, fontWeight: 700 }}>
                              {f.family_key}
                              <span style={{ fontFamily: 'monospace', fontSize: 10.5, color: colors.textFaint, marginLeft: 8 }}>
                                {f.family_pattern}
                              </span>
                            </td>
                            <td style={tdStyle}><Pill color={verdictColor(f.verdict)}>{f.verdict}</Pill></td>
                            <td style={{ ...tdStyle, maxWidth: 220, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', color: f.registered ? colors.text : colors.warningText }}
                              title={f.registered ? f.owner : 'no registry row — unowned'}>
                              {f.registered ? (f.owner || '—') : 'UNOWNED'}
                            </td>
                            <td style={numTd}>{f.sla_hours > 0 ? `${f.sla_hours}h` : '—'}</td>
                            <td style={numTd} title={`${f.dynamic_count} dynamic · ${f.static_count} static`}>
                              {fmtInt(f.segments_count)}
                              <span style={{ color: colors.textFaint, fontSize: 10.5 }}> ({f.dynamic_count}d/{f.static_count}s)</span>
                            </td>
                            <td style={numTd}>{fmtInt(f.subscriber_total)}</td>
                            <td style={{ ...numTd, ...buildAgeStyle(f) }}
                              title={f.last_membership_build_max
                                ? `newest ${f.last_membership_build_max} · oldest ${f.last_membership_build_min ?? '—'} (ledger last_built_at)`
                                : 'no membership build ever recorded in the ledger'}>
                              {f.last_membership_build_max == null ? 'NEVER' : fmtRelative(f.last_membership_build_max)}
                            </td>
                            <td style={numTd}>{f.builds_last_24h > 0 ? fmtInt(f.builds_last_24h) : '—'}</td>
                            <td style={numTd}
                              title={churnAvailable
                                ? `24h: ${fmtInt(f.member_inserts_24h)} · 7d: ${fmtInt(f.member_inserts_7d)} rows (re)materialized`
                                : 'churn source unavailable this fetch'}>
                              {churnAvailable ? fmtInt(f.member_inserts_24h) : '—*'}
                            </td>
                            <td style={numTd}
                              title={refsAvailable ? 'campaigns created in the last 3 days including this family in inclusion_segments' : 'refs source unavailable this fetch'}>
                              {refsAvailable ? (f.campaign_refs_3d > 0 ? fmtInt(f.campaign_refs_3d) : '—') : '—*'}
                            </td>
                            <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>
                              {f.diverged_count > 0 && (
                                <Pill color={colors.warning} style={{ marginRight: 6 }}
                                  >{f.diverged_count} COUNTS≠BUILD</Pill>
                              )}
                              {f.failed_now > 0 && (
                                <Pill color={colors.danger}>{f.failed_now} FAILED</Pill>
                              )}
                            </td>
                          </tr>
                          {isOpen && (
                            <tr>
                              <td colSpan={12} style={{ padding: 0, borderTop: `1px solid ${colors.divider}`, background: alpha(colors.indigo500, '0d') }}>
                                <SegmentDrilldown family={f} churnAvailable={churnAvailable} />
                              </td>
                            </tr>
                          )}
                        </React.Fragment>
                      );
                    })}
                  </tbody>
                </table>
                <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 8, lineHeight: 1.6 }}>
                  Last build = newest membership build in the family (ledger <code>last_built_at</code>) — NEVER a count-refresh timestamp; red = past the family's SLA.
                  COUNTS≠BUILD = the segment's count clock advanced &gt;24h after its last membership build — the "counts fresh, membership stale" trap, flagged explicitly.
                  * Members = SUM of per-segment counts (overlap not deduped).
                  {!churnAvailable && ' —* Inserts: the bounded churn aggregate over mailing_segment_members timed out or failed this fetch (churn_method=unavailable); no number is shown rather than a wrong one.'}
                  {churnAvailable && ' Inserts 24h counts rows (re)materialized in the window (rebuilds re-stamp all rows), not net-new members.'}
                  {!refsAvailable && ' —* Refs: the campaign-reference scan was unavailable this fetch.'}
                </div>
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  );
};

export default SegmentationCommand;
