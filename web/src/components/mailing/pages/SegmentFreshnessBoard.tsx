import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faTableCells, faRotate, faSpinner, faTriangleExclamation,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import { colors, alpha, panelStyle, thStyle, tableStyle, btnStyle } from '../shared/theme';
import { SectionHeader, Stat, Pill, SectionError, EmptyState, LivePill, PortalKeyframes } from '../shared/ui';
import { usePolling } from '../shared/usePolling';

// =============================================================================
// SEGMENT FRESHNESS BOARD — the per-sending-domain engaged grid, staleness-first
// =============================================================================
// Operator mandate (2026-08-20): "I need to visually see if the segments are
// stale and how I can get them refreshed." The primary segments are the
// per-sending-domain 7/14/30/60-day Openers/Clickers grid — 8 cells per domain.
//
// Source: GET /api/mailing/segments/freshness (contract fixed with the Go
// builder — this component renders that contract's fields and nothing else).
// Action: POST /api/mailing/segments/refresh {"segment_ids":[...]} -> 202
// {queued:[...], already:[...]}.
//
// HONESTY RULES (this screen's law):
//  - 'unknown' freshness is NEVER green and NEVER rendered as 0 — it renders
//    as an explicit grey UNKNOWN cell.
//  - member_count 0 with a FRESH members_stamped_at is a measurement artifact
//    (the silent-zero defect class): render the stamp, flag the discrepancy,
//    never present the 0 as the audience size.
//  - never-built, built-empty, missing-cell, and fetch-error are four
//    different displays. Absence ≠ zero, anywhere.
//  - Optimistic 'queued' chips from the 202 body expire if the server never
//    confirms them (OPTIMISTIC_TTL_MS) — the board never asserts a queue state
//    the server stopped reporting.

// ── API shapes (mirror the freshness contract; do not drift) ────────────────

type Freshness = 'fresh' | 'aging' | 'stale' | 'unknown';
type RefreshState = null | 'queued' | 'running';
type Kind = 'openers' | 'clickers';

interface FreshnessRow {
  segment_id: string;
  name: string;
  brand: string;
  window_days: number;
  kind: Kind;
  status: string;
  member_count: number;
  members_stamped_at: string | null;
  last_built_at: string | null;
  build_source: string;
  last_build_status: string;
  last_error: string;
  freshness: Freshness;
  refresh_state: RefreshState;
}

interface WorkerInfo {
  running: boolean;
  last_pass_at: string | null;
  last_pass_outcome: string;
  degraded: boolean;
  leader: boolean;
}

interface FreshnessResponse {
  generated_at: string;
  worker: WorkerInfo;
  rows: FreshnessRow[];
}

// 404 (endpoint not on this server build) is a distinct display from an error.
type FreshnessFetch = FreshnessResponse | { unavailable: true };

const isUnavailable = (d: FreshnessFetch): d is { unavailable: true } =>
  (d as { unavailable?: boolean }).unavailable === true;

// ── Constants ───────────────────────────────────────────────────────────────

const WINDOWS = [7, 14, 30, 60] as const;
const KINDS: readonly Kind[] = ['openers', 'clickers'] as const;

// An optimistic 'queued' chip that the server never confirms expires after
// this long, so the board cannot lie "queued" forever on a lost job.
const OPTIMISTIC_TTL_MS = 2 * 60 * 1000;

const POLL_ACTIVE_MS = 15_000; // anything queued/running → tight poll
const POLL_IDLE_MS = 60_000;

// A 0-count with a membership stamp newer than this is flagged as a
// measurement artifact (count zeroed while membership demonstrably built —
// the segment_subscriber_count silent-zero defect class).
const SILENT_ZERO_STAMP_HOURS = 24;

// ── Display helpers ─────────────────────────────────────────────────────────

const fmtInt = (n: number): string => n.toLocaleString();

const ageHours = (iso: string | null): number | null => {
  if (!iso) return null;
  const t = Date.parse(iso);
  return isNaN(t) ? null : (Date.now() - t) / 3_600_000;
};

const fmtAge = (iso: string | null): string => {
  const h = ageHours(iso);
  if (h == null) return '—';
  if (h < 1) return `${Math.max(0, Math.round(h * 60))}m`;
  if (h < 48) return `${h.toFixed(1)}h`;
  return `${(h / 24).toFixed(1)}d`;
};

const freshnessColor = (f: Freshness): string => {
  switch (f) {
    case 'fresh': return colors.success;
    case 'aging': return colors.warning;
    case 'stale': return colors.danger;
    case 'unknown': return colors.idle; // UNKNOWN IS NEVER GREEN
  }
};

// ── Component ───────────────────────────────────────────────────────────────

export const SegmentFreshnessBoard: React.FC = () => {
  // Optimistic queue map: segment_id → epoch-ms when the 202 acknowledged it.
  const [pendingQueued, setPendingQueued] = useState<Record<string, number>>({});
  const [posting, setPosting] = useState(false);
  const [actionMsg, setActionMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const [anyActive, setAnyActive] = useState(false);

  const poll = usePolling<FreshnessFetch>(
    async (signal) => {
      const res = await apiFetch('/api/mailing/segments/freshness', { signal });
      if (res.status === 404) return { unavailable: true };
      if (!res.ok) {
        let msg = `HTTP ${res.status}`;
        try {
          const body = await res.json();
          if (body?.error) msg += ` — ${body.error}`;
        } catch { /* non-JSON error body */ }
        throw new Error(msg);
      }
      return (await res.json()) as FreshnessResponse;
    },
    anyActive ? POLL_ACTIVE_MS : POLL_IDLE_MS,
    [anyActive],
  );

  const data = poll.data && !isUnavailable(poll.data) ? poll.data : null;
  const rows = useMemo(() => data?.rows ?? [], [data]);

  // Effective refresh state = server truth, else a live optimistic chip.
  const effState = useCallback((r: FreshnessRow): RefreshState => {
    if (r.refresh_state) return r.refresh_state;
    const at = pendingQueued[r.segment_id];
    if (at != null && Date.now() - at < OPTIMISTIC_TTL_MS) return 'queued';
    return null;
  }, [pendingQueued]);

  // Reconcile: drop an optimistic entry once the server reports the state
  // itself, or once the segment shows a build newer than the queue moment.
  useEffect(() => {
    if (rows.length === 0) return;
    setPendingQueued(prev => {
      let changed = false;
      const next = { ...prev };
      for (const r of rows) {
        const at = next[r.segment_id];
        if (at == null) continue;
        const built = r.last_built_at ? Date.parse(r.last_built_at) : NaN;
        if (r.refresh_state != null || (!isNaN(built) && built > at) || Date.now() - at >= OPTIMISTIC_TTL_MS) {
          delete next[r.segment_id];
          changed = true;
        }
      }
      return changed ? next : prev;
    });
    // Drive the poll cadence from server-reported activity + live optimism.
    setAnyActive(rows.some(r => r.refresh_state != null) || Object.keys(pendingQueued).length > 0);
  }, [rows, pendingQueued]);

  // Grid projection: brand → `${kind}|${window}` → row.
  const { brands, cellMap, offGrid } = useMemo(() => {
    const map = new Map<string, Map<string, FreshnessRow>>();
    let off = 0;
    for (const r of rows) {
      const isGrid = KINDS.includes(r.kind) && (WINDOWS as readonly number[]).includes(r.window_days);
      if (!isGrid) { off++; continue; }
      let m = map.get(r.brand);
      if (!m) { m = new Map(); map.set(r.brand, m); }
      m.set(`${r.kind}|${r.window_days}`, r);
    }
    return { brands: [...map.keys()].sort(), cellMap: map, offGrid: off };
  }, [rows]);

  const counts = useMemo(() => {
    const c = { fresh: 0, aging: 0, stale: 0, unknown: 0 };
    for (const r of rows) c[r.freshness] = (c[r.freshness] ?? 0) + 1;
    return c;
  }, [rows]);

  const staleRefreshable = useMemo(
    () => rows.filter(r => r.freshness === 'stale' && effState(r) == null).map(r => r.segment_id),
    [rows, effState],
  );

  // ── Refresh POST (segment_ids form of the contract; the per-domain button
  // sends the domain's cell ids rather than guessing a family CODE — segment
  // codes ≠ registry codes is a known trap) ─────────────────────────────────
  const queueRefresh = useCallback(async (ids: string[], what: string) => {
    if (ids.length === 0 || posting) return;
    setPosting(true);
    setActionMsg(null);
    try {
      const res = await apiFetch('/api/mailing/segments/refresh', {
        method: 'POST',
        body: JSON.stringify({ segment_ids: ids }),
      });
      if (res.ok) { // 202 expected
        let queued: string[] = [];
        let already: string[] = [];
        try {
          const body = (await res.json()) as { queued?: string[]; already?: string[] };
          queued = body.queued ?? [];
          already = body.already ?? [];
        } catch { /* body optional — fall through with empty acks */ }
        const now = Date.now();
        setPendingQueued(prev => {
          const next = { ...prev };
          for (const id of [...queued, ...already]) next[id] = now;
          return next;
        });
        setAnyActive(true);
        setActionMsg({
          ok: true,
          text: `${what}: ${queued.length} queued${already.length > 0 ? `, ${already.length} already queued/running` : ''}.`,
        });
        poll.refresh();
      } else {
        let msg = `HTTP ${res.status}`;
        try {
          const body = await res.json();
          if (body?.error) msg += ` — ${body.error}`;
        } catch { /* non-JSON error body */ }
        setActionMsg({ ok: false, text: `${what} failed: ${msg}` });
      }
    } catch (e: unknown) {
      setActionMsg({ ok: false, text: `${what} failed: ${e instanceof Error ? e.message : String(e)}` });
    }
    setPosting(false);
  }, [posting, poll]);

  // ── Cell renderer ─────────────────────────────────────────────────────────
  const renderCell = (brand: string, kind: Kind, win: number): React.ReactNode => {
    const r = cellMap.get(brand)?.get(`${kind}|${win}`);
    if (!r) {
      // Missing cell — the segment does not exist. Absence ≠ zero.
      return (
        <td
          key={`${kind}${win}`}
          title={`${brand} · ${win}d ${kind}: no segment exists for this cell (never built). Absence ≠ zero.`}
          style={{
            padding: 4, borderTop: `1px solid ${colors.divider}`, textAlign: 'center',
            color: colors.textFaint, fontSize: 11,
          }}
        >
          <div style={{ border: `1px dashed ${alpha(colors.idle, '44')}`, borderRadius: 6, padding: '6px 4px' }}>—</div>
        </td>
      );
    }

    const c = freshnessColor(r.freshness);
    const st = effState(r);
    const stampH = ageHours(r.members_stamped_at);
    const neverBuilt = r.last_built_at == null && r.members_stamped_at == null;
    const silentZero = r.member_count === 0 && stampH != null && stampH < SILENT_ZERO_STAMP_HOURS;

    // Count line — honest per state, never a bare misleading 0.
    let countNode: React.ReactNode;
    if (neverBuilt) {
      countNode = <span style={{ color: colors.textFaint }}>—</span>;
    } else if (silentZero) {
      countNode = <span style={{ color: colors.warningText, fontWeight: 700 }}>0*</span>;
    } else if (r.freshness === 'unknown') {
      countNode = r.member_count > 0
        ? <span style={{ color: colors.textMuted }}>{fmtInt(r.member_count)}</span>
        : <span style={{ color: colors.textMuted }}>?</span>;
    } else {
      countNode = <span style={{ color: colors.text, fontWeight: 700 }}>{fmtInt(r.member_count)}</span>;
    }

    const tooltip = [
      `${r.name} (${r.segment_id})`,
      `freshness: ${r.freshness.toUpperCase()} · status: ${r.status || '—'}`,
      `members: ${fmtInt(r.member_count)}${silentZero ? '  ⚠ COUNT 0 but membership stamped ' + fmtAge(r.members_stamped_at) + ' ago — measurement artifact (silent-zero), NOT the audience size' : ''}`,
      `last_built_at: ${r.last_built_at ?? 'NEVER'}${r.last_built_at ? ` (${fmtAge(r.last_built_at)} ago)` : ''}`,
      `members_stamped_at: ${r.members_stamped_at ?? '—'}${r.members_stamped_at ? ` (${fmtAge(r.members_stamped_at)} ago)` : ''}`,
      `build_source: ${r.build_source || '—'} · last_build_status: ${r.last_build_status || '—'}`,
      r.last_error ? `last_error: ${r.last_error}` : '',
      st ? `refresh: ${st}${r.refresh_state == null ? ' (optimistic — awaiting server confirm)' : ''}` : '',
    ].filter(Boolean).join('\n');

    return (
      <td key={`${kind}${win}`} title={tooltip} style={{ padding: 4, borderTop: `1px solid ${colors.divider}` }}>
        <div
          style={{
            background: alpha(c, '14'),
            border: `1px solid ${alpha(c, r.freshness === 'stale' ? '66' : '44')}`,
            borderRadius: 6, padding: '4px 6px', minWidth: 74,
            display: 'flex', flexDirection: 'column', gap: 2,
          }}
        >
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 6 }}>
            <span style={{ fontSize: 12, fontVariantNumeric: 'tabular-nums' }}>{countNode}</span>
            <button
              onClick={() => queueRefresh([r.segment_id], `${brand} ${win}d ${kind}`)}
              disabled={posting || st != null}
              title={st != null
                ? `refresh ${st} — button re-enables when the pass completes`
                : `Queue a membership rebuild for ${brand} ${win}d ${kind}`}
              style={{
                background: 'none', border: 'none', padding: 0,
                color: st != null ? colors.textFaint : colors.indigo300,
                cursor: posting || st != null ? 'default' : 'pointer', fontSize: 11, lineHeight: 1,
              }}
            >
              <FontAwesomeIcon icon={st === 'running' ? faSpinner : faRotate} spin={st === 'running'} />
            </button>
          </div>
          <div style={{ fontSize: 9.5, color: colors.textMuted, display: 'flex', gap: 4, alignItems: 'center', whiteSpace: 'nowrap' }}>
            {r.freshness === 'unknown' && <span style={{ color: colors.idle, fontWeight: 700 }}>UNKNOWN</span>}
            {neverBuilt
              ? <span style={{ color: colors.dangerText, fontWeight: 700 }}>NEVER BUILT</span>
              : silentZero
                ? <span style={{ color: colors.warningText }}>stamped {fmtAge(r.members_stamped_at)} ago</span>
                : <span>built {r.last_built_at ? `${fmtAge(r.last_built_at)} ago` : 'NEVER'}</span>}
            {st && (
              <span style={{
                color: colors.info, fontWeight: 700, textTransform: 'uppercase',
                border: `1px solid ${alpha(colors.info, '44')}`, borderRadius: 999, padding: '0 5px',
              }}>
                {st}
              </span>
            )}
            {!neverBuilt && !silentZero && r.member_count === 0 && r.freshness !== 'unknown' && (
              <span style={{ color: colors.textFaint }}>built empty</span>
            )}
          </div>
        </div>
      </td>
    );
  };

  // ── Render ────────────────────────────────────────────────────────────────
  const w = data?.worker;

  return (
    <div style={panelStyle}>
      <PortalKeyframes />
      <SectionHeader
        title="Segment freshness — per-sending-domain 7/14/30/60d openers × clickers"
        icon={faTableCells}
        right={
          <span style={{ display: 'inline-flex', alignItems: 'center', gap: 12 }}>
            <LivePill live={poll.live} agoSeconds={poll.secondsSinceUpdate} />
            <button
              onClick={() => queueRefresh(staleRefreshable, 'refresh all stale')}
              disabled={posting || staleRefreshable.length === 0}
              title={staleRefreshable.length === 0
                ? 'No stale cells that are not already queued/running'
                : `Queue a rebuild for every STALE cell (${staleRefreshable.length} segments)`}
              style={{ ...btnStyle, opacity: posting || staleRefreshable.length === 0 ? 0.5 : 1 }}
            >
              <FontAwesomeIcon icon={faRotate} /> Refresh all stale ({staleRefreshable.length})
            </button>
          </span>
        }
      />

      {/* ── Worker strip ── */}
      {w && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap', marginBottom: 10 }}>
          <Pill color={w.running ? colors.success : colors.danger}>{w.running ? 'worker running' : 'worker NOT running'}</Pill>
          <Pill color={w.leader ? colors.indigo400 : colors.idle}>{w.leader ? 'leader' : 'not leader'}</Pill>
          <span style={{ fontSize: 11.5, color: colors.textMuted }}>
            last pass: {w.last_pass_at ? `${fmtAge(w.last_pass_at)} ago` : 'NEVER'}
            {w.last_pass_outcome ? ` · ${w.last_pass_outcome}` : ''}
          </span>
          <span style={{ fontSize: 11, color: colors.textFaint }}>
            generated {data ? fmtAge(data.generated_at) : '—'} ago
          </span>
        </div>
      )}
      {w?.degraded && (
        <div style={{
          padding: '10px 14px', borderRadius: 8, marginBottom: 12, fontSize: 12.5, fontWeight: 700,
          background: alpha(colors.warning, '22'), border: `1px solid ${alpha(colors.warning, '66')}`,
          color: colors.warningText, display: 'flex', alignItems: 'center', gap: 10,
        }}>
          <FontAwesomeIcon icon={faTriangleExclamation} />
          worker circuit open — DB under pressure, refreshes paused. Queued refreshes will wait until the circuit closes.
        </div>
      )}

      {/* ── States: loading / 404 / error / data ── */}
      {poll.loading && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, color: colors.textMuted, fontSize: 13, padding: '14px 4px' }}>
          <FontAwesomeIcon icon={faSpinner} spin /> Loading segment freshness…
        </div>
      )}

      {!poll.loading && poll.data && isUnavailable(poll.data) && (
        <div style={{
          padding: '12px 14px', borderRadius: 8, fontSize: 12.5, lineHeight: 1.6,
          background: alpha(colors.warning, '14'), border: `1px solid ${alpha(colors.warning, '44')}`,
          color: colors.warning,
        }}>
          <strong>SOURCE UNAVAILABLE.</strong> <code style={{ fontFamily: 'monospace' }}>/api/mailing/segments/freshness</code> is
          not exposed by this server build — the freshness backend has not been deployed here yet.
        </div>
      )}

      {!poll.loading && poll.error && (
        <div style={{ marginBottom: 10 }}>
          <SectionError label="segment freshness" error={data ? `${poll.error} — showing last good data` : poll.error} onRetry={poll.refresh} />
        </div>
      )}

      {actionMsg && (
        <div style={{
          padding: '8px 12px', borderRadius: 8, marginBottom: 10, fontSize: 12.5,
          background: alpha(actionMsg.ok ? colors.success : colors.danger, '14'),
          border: `1px solid ${alpha(actionMsg.ok ? colors.success : colors.danger, '44')}`,
          color: actionMsg.ok ? colors.successText : colors.dangerText,
          display: 'flex', alignItems: 'center', gap: 10,
        }}>
          <FontAwesomeIcon icon={faRotate} />
          {actionMsg.text}
          <button onClick={() => setActionMsg(null)} style={{
            marginLeft: 'auto', background: 'none', border: 'none', color: 'inherit', cursor: 'pointer', fontSize: 12.5,
          }}>✕</button>
        </div>
      )}

      {data && (
        <>
          {/* ── Cell-state summary ── */}
          <div style={{ display: 'flex', gap: 22, flexWrap: 'wrap', marginBottom: 12 }}>
            <Stat label="Stale" value={counts.stale} color={counts.stale > 0 ? colors.danger : colors.textMuted} sub="build past its freshness bound" />
            <Stat label="Aging" value={counts.aging} color={counts.aging > 0 ? colors.warning : colors.textMuted} sub="approaching stale" />
            <Stat label="Fresh" value={counts.fresh} color={colors.success} sub="recently built" />
            <Stat label="Unknown" value={counts.unknown} color={counts.unknown > 0 ? colors.idle : colors.textMuted} sub="no freshness verdict — not zero, not green" />
            <Stat label="Domains" value={brands.length} sub={`${rows.length - offGrid} grid cells${offGrid > 0 ? ` · +${offGrid} off-grid rows not shown` : ''}`} />
          </div>

          {brands.length === 0 ? (
            <EmptyState
              title="No grid segments in the freshness feed"
              hint="The endpoint answered but returned no 7/14/30/60d openers/clickers rows — the grid has not been built for any sending domain."
            />
          ) : (
            <div style={{ overflowX: 'auto' }}>
              <table style={{ ...tableStyle, minWidth: 980 }}>
                <thead>
                  <tr>
                    <th style={thStyle} rowSpan={2}>Sending domain</th>
                    <th style={{ ...thStyle, textAlign: 'center' }} colSpan={4}>Openers</th>
                    <th style={{ ...thStyle, textAlign: 'center', borderLeft: `1px solid ${colors.hairline}` }} colSpan={4}>Clickers</th>
                    <th style={thStyle} rowSpan={2} />
                  </tr>
                  <tr>
                    {KINDS.map(k => WINDOWS.map((d, i) => (
                      <th key={`${k}${d}`} style={{
                        ...thStyle, textAlign: 'center', fontSize: 10,
                        ...(i === 0 && k === 'clickers' ? { borderLeft: `1px solid ${colors.hairline}` } : {}),
                      }}>
                        {d}d
                      </th>
                    )))}
                  </tr>
                </thead>
                <tbody>
                  {brands.map(brand => {
                    const domainIds = KINDS.flatMap(k => WINDOWS.map(d => cellMap.get(brand)?.get(`${k}|${d}`)))
                      .filter((r): r is FreshnessRow => r != null && effState(r) == null)
                      .map(r => r.segment_id);
                    return (
                      <tr key={brand}>
                        <td style={{
                          padding: '7px 10px', fontSize: 12, fontWeight: 700, color: colors.text,
                          borderTop: `1px solid ${colors.divider}`, fontFamily: 'monospace', whiteSpace: 'nowrap',
                        }}>
                          {brand}
                        </td>
                        {KINDS.map(k => WINDOWS.map(d => renderCell(brand, k, d)))}
                        <td style={{ padding: 4, borderTop: `1px solid ${colors.divider}` }}>
                          <button
                            onClick={() => queueRefresh(domainIds, `refresh family ${brand}`)}
                            disabled={posting || domainIds.length === 0}
                            title={domainIds.length === 0
                              ? `Every ${brand} cell is already queued/running (or no cells exist)`
                              : `Queue a rebuild for all ${domainIds.length} ${brand} grid segments`}
                            style={{
                              background: alpha(colors.indigo500, '22'), border: `1px solid ${alpha(colors.indigo500, '44')}`,
                              color: colors.indigo200, borderRadius: 6, padding: '3px 9px', whiteSpace: 'nowrap',
                              cursor: posting || domainIds.length === 0 ? 'default' : 'pointer', fontSize: 11,
                              opacity: posting || domainIds.length === 0 ? 0.5 : 1,
                            }}
                          >
                            <FontAwesomeIcon icon={faRotate} /> Family
                          </button>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
              <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 8, lineHeight: 1.6 }}>
                Cell color = server freshness verdict: <span style={{ color: colors.success }}>fresh</span> ·{' '}
                <span style={{ color: colors.warning }}>aging</span> ·{' '}
                <span style={{ color: colors.danger }}>stale</span> ·{' '}
                <span style={{ color: colors.idle }}>unknown (never green, never 0)</span>.
                Number = member_count. <span style={{ color: colors.warningText }}>0*</span> = count reads 0 while membership
                was stamped within {SILENT_ZERO_STAMP_HOURS}h — a measurement artifact (silent-zero), NOT the audience size;
                the stamp is shown instead. Dashed — = the segment does not exist (absence ≠ zero). NEVER BUILT and
                "built empty" are distinct. Hover any cell for last_built_at / members_stamped_at / build_source / last_error.
                Refresh buttons POST /api/mailing/segments/refresh; the QUEUED chip is optimistic from the 202 body and expires
                unless the server confirms it. Poll cadence {anyActive ? '15s (refreshes in flight)' : '60s (idle)'}.
              </div>
            </div>
          )}
        </>
      )}
    </div>
  );
};

export default SegmentFreshnessBoard;
