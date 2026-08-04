import React, { useCallback, useMemo, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faUsers, faRotate, faSpinner, faChevronDown, faChevronRight,
  faTriangleExclamation, faSearch, faHourglassHalf,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import {
  colors, alpha, pageStyle, panelStyle, tableStyle,
  thStyle, tdStyle, numTd, numTh,
} from '../shared/theme';
import { SectionHeader, Stat, Pill, SectionError, EmptyState, LivePill, PortalKeyframes } from '../shared/ui';
import { FilterChip } from '../shared/filters';
import { usePolling } from '../shared/usePolling';
import './AudienceCommand.css';

// =============================================================================
// AUDIENCES — the single source of truth for segmentation (AUDIENCE_UNIFICATION
// Phase 2). Segment-only catalog: NO lists on this screen. Every row answers,
// in order of operator action: is this audience real (members + built-ago +
// verdict), does it perform (7d open/click over delivered), is it in use
// (last mailed + campaigns_30d), and is it about to dissolve (prune clock).
//
// Source: GET /api/mailing/v2/segments/overview (built in a parallel change —
// this screen FAILS SOFT with a designed panel + Retry while that build is
// not deployed / 404s). Row expansion lazily fetches the full criteria JSON
// from the existing GET /api/mailing/v2/segments/{id} and exposes the existing
// POST /api/mailing/v2/segments/{id}/refresh with a busy state.
//
// FilterBar note (PORTAL_DESIGN_SYSTEM §3): none of the canonical lake filter
// vocabulary (Denver date range / brand / isp / transport) binds to this
// catalog source — mounting those fields would violate §3.5 ("filters must
// actually bind"; same ruling as SegmentationCommand / OperationsConsole).
// The screen therefore composes its catalog filters (search / status /
// category / verdict) from the SHARED filter chrome: the shared FilterChip
// for active-filter chips, shared theme tokens for the toolbar — no bespoke
// chip or color idioms. Filtering is client-side over one cheap fetch, so the
// live-apply model applies (§3.3).
//
// State honesty (§1.6): loading, fetch-error-with-retry, endpoint-not-on-this-
// build (404 = deploy held), and genuinely-empty are four distinct displays.
// Poll = 60s via shared usePolling (first-paint spinner only; last good data
// stays mounted on a failed tick with a STALE clock, never a blank screen).
const PAGE_VERSION = '1.0';

const OVERVIEW_URL = '/api/mailing/v2/segments/overview';
const POLL_MS = 60_000;
// DOM guard: the segment estate is 25k+ rows; the catalog renders at most this
// many rows after filters and says so, rather than locking the tab.
const MAX_RENDER = 400;

// ── API shapes (contract with the overview endpoint build; do not drift) ────

type Verdict = 'LIVE' | 'STALE' | 'STATIC-DECLARED' | 'UNREGISTERED';

interface PerfWindow {
  delivered: number;
  unique_opens: number;
  unique_clicks: number;
  open_rate: number;  // fraction: unique_opens / delivered
  click_rate: number; // fraction: unique_clicks / delivered
}

interface OverviewSegment {
  id: string;
  name: string;
  category: string;
  segment_type: string;
  status: string;
  members: number;
  audience_source: string | null; // 'cached' = count served from cache, not a fresh build
  last_built_at: string | null;   // null = NEVER built
  last_build_status: string;
  conditions_summary: string;
  perf_7d: PerfWindow | null;     // null = no rollup for this segment
  perf_30d: PerfWindow | null;
  last_mailed_at: string | null;  // null = never mailed (via mailing_campaign_audiences)
  campaigns_30d: number;
  verdict: Verdict;
  prune_at: string | null;        // null = protected (no prune clock)
}

interface OverviewSummary {
  by_verdict: Partial<Record<Verdict, number>>;
  prunable: number;
}

interface OverviewResponse {
  summary?: OverviewSummary;
  segments?: OverviewSegment[];
  rows?: OverviewSegment[]; // tolerated alternate envelope key
  generated_at?: string;
}

// Discriminated fetch result so the shared polling hook can distinguish
// "endpoint not on this build" (404 — fail-soft panel) from data.
type OverviewResult =
  | { kind: 'ok'; summary: OverviewSummary | null; segments: OverviewSegment[] }
  | { kind: 'unavailable' };

// ── Display helpers ──────────────────────────────────────────────────────────

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

const fmtPct = (rate: number): string => `${(rate * 100).toFixed(2)}%`;

// Verdict pill colors — identical mapping to Segmentation Command (stateColor
// semantics): LIVE green / STALE red / STATIC-DECLARED gray / UNREGISTERED amber.
const verdictColor = (v: Verdict): string => {
  switch (v) {
    case 'LIVE': return colors.success;
    case 'STALE': return colors.danger;
    case 'UNREGISTERED': return colors.warning;
    case 'STATIC-DECLARED': return colors.idle;
  }
};

// Days until prune_at (ceil); null when no prune clock.
const pruneDays = (iso: string | null): number | null => {
  if (!iso) return null;
  const t = Date.parse(iso);
  if (isNaN(t)) return null;
  return Math.ceil((t - Date.now()) / 86_400_000);
};

// Tooltip for a rate cell: names numerator, denominator and both windows.
const rateTitle = (
  label: 'opens' | 'clicks',
  p7: PerfWindow | null,
  p30: PerfWindow | null,
): string => {
  if (!p7 && !p30) return 'no rollup for this segment — the nightly perf rollup has no data yet';
  const one = (w: string, p: PerfWindow) => {
    const num = label === 'opens' ? p.unique_opens : p.unique_clicks;
    const rate = label === 'opens' ? p.open_rate : p.click_rate;
    return `${w}: ${fmtInt(num)} unique ${label} / ${fmtInt(p.delivered)} delivered = ${fmtPct(rate)}`;
  };
  const parts: string[] = [];
  if (p7) parts.push(one('7d', p7));
  if (p30) parts.push(one('30d', p30));
  if (label === 'opens') parts.push('RAW opens (machine incl.) — not a human claim');
  return parts.join(' · ');
};

// ── Sorting ──────────────────────────────────────────────────────────────────

type SortKey = 'name' | 'members' | 'last_built_at' | 'open_rate' | 'last_mailed_at';

const sortValue = (s: OverviewSegment, key: SortKey): string | number | null => {
  switch (key) {
    case 'name': return s.name.toLowerCase();
    case 'members': return s.members;
    case 'last_built_at': return s.last_built_at ? Date.parse(s.last_built_at) : null;
    case 'open_rate': return s.perf_7d ? s.perf_7d.open_rate : null;
    case 'last_mailed_at': return s.last_mailed_at ? Date.parse(s.last_mailed_at) : null;
  }
};

// ── Row expansion: full criteria + refresh action ────────────────────────────

interface DetailState {
  loading: boolean;
  error: string | null;
  criteria: unknown; // segment.conditions preferred, else the conditions rows
}

const CriteriaBlock: React.FC<{ detail: DetailState | undefined; onRetry: () => void }> = ({ detail, onRetry }) => {
  if (!detail || detail.loading) {
    return (
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, color: colors.textMuted, fontSize: 12 }}>
        <FontAwesomeIcon icon={faSpinner} spin /> Loading full criteria…
      </div>
    );
  }
  if (detail.error) {
    return <SectionError label="segment criteria" error={detail.error} onRetry={onRetry} />;
  }
  if (detail.criteria == null) {
    return (
      <div style={{ fontSize: 12, color: colors.textMuted }}>
        No stored criteria — a STATIC segment carries no definition beyond its build source (this is the
        birth defect the Audiences unification is closing, not a fetch failure).
      </div>
    );
  }
  let pretty: string;
  try {
    pretty = JSON.stringify(detail.criteria, null, 2);
  } catch {
    pretty = String(detail.criteria);
  }
  return <pre className="ac-json">{pretty}</pre>;
};

// ── Page ─────────────────────────────────────────────────────────────────────

export const AudienceCommand: React.FC = () => {
  // Fetch timing note (§1.6) — kept in refs so the polling hook stays generic.
  const fetchedAtRef = useRef<string | null>(null);
  const fetchMsRef = useRef<number | null>(null);

  const fetcher = useCallback(async (signal: AbortSignal): Promise<OverviewResult> => {
    const t0 = performance.now();
    const res = await apiFetch(OVERVIEW_URL, { signal });
    fetchMsRef.current = Math.round(performance.now() - t0);
    fetchedAtRef.current = new Date().toLocaleTimeString();
    if (res.status === 404) return { kind: 'unavailable' };
    if (!res.ok) {
      let msg = `HTTP ${res.status}`;
      try {
        const body = await res.json();
        if (body?.error) msg += ` — ${body.error}`;
      } catch { /* non-JSON error body */ }
      throw new Error(msg);
    }
    const body = (await res.json()) as OverviewResponse;
    const segments = body.segments ?? body.rows;
    if (!Array.isArray(segments)) {
      throw new Error('unexpected response shape — no segments array in overview payload');
    }
    return { kind: 'ok', summary: body.summary ?? null, segments };
  }, []);

  const poll = usePolling<OverviewResult>(fetcher, POLL_MS, []);

  // ── Catalog filters (client-side, live-apply) ──
  const [search, setSearch] = useState('');
  const [status, setStatus] = useState<string>('all');
  const [categories, setCategories] = useState<Set<string>>(new Set());
  const [verdicts, setVerdicts] = useState<Set<Verdict>>(new Set());
  const [sortKey, setSortKey] = useState<SortKey>('members');
  const [sortDir, setSortDir] = useState<'asc' | 'desc'>('desc');

  // ── Row expansion + per-row actions ──
  const [expanded, setExpanded] = useState<string | null>(null);
  const [details, setDetails] = useState<Record<string, DetailState>>({});
  const [refreshing, setRefreshing] = useState<Record<string, boolean>>({});
  const [refreshMsg, setRefreshMsg] = useState<Record<string, { ok: boolean; text: string }>>({});

  const allRows: OverviewSegment[] = poll.data?.kind === 'ok' ? poll.data.segments : [];
  const summary: OverviewSummary | null = poll.data?.kind === 'ok' ? poll.data.summary : null;
  const unavailable = poll.data?.kind === 'unavailable';

  const statusOptions = useMemo(
    () => Array.from(new Set(allRows.map(r => r.status).filter(Boolean))).sort(),
    [allRows],
  );
  // Categories ranked by segment count so the chip row leads with the big families.
  const categoryOptions = useMemo(() => {
    const counts = new Map<string, number>();
    for (const r of allRows) {
      const c = r.category || '(uncategorized)';
      counts.set(c, (counts.get(c) ?? 0) + 1);
    }
    return Array.from(counts.entries()).sort((a, b) => b[1] - a[1]);
  }, [allRows]);
  const CHIP_CAT_LIMIT = 18;
  const chipCategories = categoryOptions.slice(0, CHIP_CAT_LIMIT);
  const overflowCategories = categoryOptions.slice(CHIP_CAT_LIMIT);

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    const rows = allRows.filter(r => {
      if (status !== 'all' && r.status !== status) return false;
      if (verdicts.size > 0 && !verdicts.has(r.verdict)) return false;
      if (categories.size > 0 && !categories.has(r.category || '(uncategorized)')) return false;
      if (q) {
        const hay = `${r.name} ${r.category} ${r.conditions_summary}`.toLowerCase();
        if (!hay.includes(q)) return false;
      }
      return true;
    });
    rows.sort((a, b) => {
      const va = sortValue(a, sortKey);
      const vb = sortValue(b, sortKey);
      if (va == null && vb == null) return 0;
      if (va == null) return 1; // nulls always last
      if (vb == null) return -1;
      const cmp = typeof va === 'string' && typeof vb === 'string'
        ? va.localeCompare(vb)
        : (va as number) - (vb as number);
      return sortDir === 'asc' ? cmp : -cmp;
    });
    return rows;
  }, [allRows, search, status, verdicts, categories, sortKey, sortDir]);

  const rendered = filtered.slice(0, MAX_RENDER);

  // ── KPI band numbers (estate-level, not filter-scoped) ──
  const activeCount = useMemo(() => allRows.filter(r => r.status === 'active').length, [allRows]);
  const membersSum = useMemo(() => allRows.reduce((acc, r) => acc + (r.members || 0), 0), [allRows]);
  const staleCount = summary?.by_verdict?.STALE ?? allRows.filter(r => r.verdict === 'STALE').length;
  const prunableCount = summary?.prunable ?? allRows.filter(r => r.prune_at != null).length;

  // Filtered totals row (pinned): members sum + aggregate 7d rates over rows
  // WITH a rollup, computed from counts (Σopens/Σdelivered) — never an average
  // of rates.
  const totals = useMemo(() => {
    let members = 0, campaigns = 0, delivered = 0, opens = 0, clicks = 0, withPerf = 0;
    for (const r of filtered) {
      members += r.members || 0;
      campaigns += r.campaigns_30d || 0;
      if (r.perf_7d) {
        withPerf++;
        delivered += r.perf_7d.delivered;
        opens += r.perf_7d.unique_opens;
        clicks += r.perf_7d.unique_clicks;
      }
    }
    return { members, campaigns, delivered, opens, clicks, withPerf };
  }, [filtered]);

  const toggleSet = <T,>(set: Set<T>, v: T): Set<T> => {
    const next = new Set(set);
    if (next.has(v)) next.delete(v); else next.add(v);
    return next;
  };

  const onSort = (key: SortKey) => {
    if (sortKey === key) {
      setSortDir(d => (d === 'asc' ? 'desc' : 'asc'));
    } else {
      setSortKey(key);
      setSortDir(key === 'name' ? 'asc' : 'desc');
    }
  };
  const sortArrow = (key: SortKey) => (sortKey === key ? (sortDir === 'asc' ? ' ▲' : ' ▼') : '');

  // Lazy criteria fetch on expand — GET /v2/segments/{id} ({segment, conditions}).
  const loadDetail = useCallback(async (id: string) => {
    setDetails(d => ({ ...d, [id]: { loading: true, error: null, criteria: null } }));
    try {
      const res = await apiFetch(`/api/mailing/v2/segments/${id}`);
      if (!res.ok) {
        let msg = `HTTP ${res.status}`;
        try { const b = await res.json(); if (b?.error) msg += ` — ${b.error}`; } catch { /* non-JSON */ }
        setDetails(d => ({ ...d, [id]: { loading: false, error: msg, criteria: null } }));
        return;
      }
      const body = await res.json() as { segment?: { conditions?: unknown } | null; conditions?: unknown };
      const seg = body.segment ?? null;
      const segConditions = seg && typeof seg === 'object' ? seg.conditions : undefined;
      const hasSegConditions = segConditions != null
        && !(typeof segConditions === 'object' && Object.keys(segConditions as object).length === 0);
      const rowConditions = Array.isArray(body.conditions) && body.conditions.length > 0 ? body.conditions : null;
      const criteria = hasSegConditions ? segConditions : rowConditions;
      setDetails(d => ({ ...d, [id]: { loading: false, error: null, criteria } }));
    } catch (e: unknown) {
      setDetails(d => ({
        ...d,
        [id]: { loading: false, error: e instanceof Error ? e.message : String(e), criteria: null },
      }));
    }
  }, []);

  const onExpand = (id: string) => {
    const next = expanded === id ? null : id;
    setExpanded(next);
    if (next && !details[next]) void loadDetail(next);
  };

  // Refresh action — POST /v2/segments/{id}/refresh (200 = synchronous rebuild,
  // 202 = async lake build queued). Busy state per row; catalog reconciles via
  // an immediate poll refresh (the 60s poll is the standing reconcile).
  const doRefresh = useCallback(async (id: string, name: string) => {
    if (refreshing[id]) return;
    setRefreshing(r => ({ ...r, [id]: true }));
    setRefreshMsg(m => { const rest = { ...m }; delete rest[id]; return rest; });
    try {
      const res = await apiFetch(`/api/mailing/v2/segments/${id}/refresh?force=false`, { method: 'POST' });
      if (res.status === 202) {
        setRefreshMsg(m => ({ ...m, [id]: { ok: true, text: 'Lake build queued (async, single slot) — members reconcile when the build lands.' } }));
      } else if (res.ok) {
        setRefreshMsg(m => ({ ...m, [id]: { ok: true, text: 'Membership rebuilt — catalog numbers reconcile on the next poll tick.' } }));
      } else {
        let msg = `HTTP ${res.status}`;
        try { const b = await res.json(); if (b?.error) msg += ` — ${b.error}`; } catch { /* non-JSON */ }
        setRefreshMsg(m => ({ ...m, [id]: { ok: false, text: `Refresh failed for "${name}": ${msg}` } }));
      }
    } catch (e: unknown) {
      setRefreshMsg(m => ({ ...m, [id]: { ok: false, text: e instanceof Error ? e.message : String(e) } }));
    } finally {
      setRefreshing(r => ({ ...r, [id]: false }));
      poll.refresh();
    }
  }, [refreshing, poll]);

  // ── Toolbar (shared filter chrome; see FilterBar note above) ──
  const chipStyle = (active: boolean, tone: string): React.CSSProperties => ({
    border: `1px solid ${active ? tone + '88' : colors.panelBorderStrong}`,
    background: active ? alpha(tone, '22') : 'transparent',
    color: active ? tone : colors.textMuted,
    borderRadius: 999, padding: '4px 11px', fontSize: 11.5, fontWeight: 600,
    cursor: 'pointer', whiteSpace: 'nowrap',
  });

  const inputStyle: React.CSSProperties = {
    background: colors.appBgSolid, color: colors.text,
    border: `1px solid ${colors.panelBorderStrong}`, borderRadius: 6,
    padding: '7px 10px', fontSize: 13, outline: 'none',
  };
  const fieldLabelStyle: React.CSSProperties = {
    display: 'flex', flexDirection: 'column', gap: 4,
    fontSize: 10, color: colors.textFaint, textTransform: 'uppercase', letterSpacing: 0.5,
  };

  const activeChips: Array<{ label: string; tone?: string; onRemove: () => void }> = [];
  if (search.trim()) activeChips.push({ label: `search="${search.trim()}"`, onRemove: () => setSearch('') });
  if (status !== 'all') activeChips.push({ label: `status=${status}`, tone: colors.indigo300, onRemove: () => setStatus('all') });
  categories.forEach(c => activeChips.push({ label: `category=${c}`, tone: colors.indigo200, onRemove: () => setCategories(s => toggleSet(s, c)) }));
  verdicts.forEach(v => activeChips.push({ label: `verdict=${v}`, tone: verdictColor(v), onRemove: () => setVerdicts(s => toggleSet(s, v)) }));

  const d = poll.data;

  return (
    <div style={pageStyle}>
      <PortalKeyframes />

      {/* ── Header + last-refresh clock ── */}
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4, gap: 12, flexWrap: 'wrap' }}>
        <div>
          <h2 style={{ margin: 0, fontSize: 20, display: 'flex', alignItems: 'center', gap: 10 }}>
            <FontAwesomeIcon icon={faUsers} style={{ color: colors.indigo400 }} /> Audiences
          </h2>
          <div style={{ fontSize: 12, color: colors.textMuted, marginTop: 4 }}>
            The single source of truth for segmentation — performance subsets, counts, freshness, pruning. Segments only; lists never appear here. v{PAGE_VERSION}
            {fetchedAtRef.current && <span> · fetched {fetchedAtRef.current}{fetchMsRef.current != null ? ` · ${fetchMsRef.current}ms` : ''}</span>}
          </div>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          <LivePill live={poll.live} agoSeconds={poll.secondsSinceUpdate} />
          <button
            onClick={() => poll.refresh()}
            style={{
              background: alpha(colors.indigo500, '22'), border: `1px solid ${alpha(colors.indigo500, '66')}`,
              color: colors.text, borderRadius: 8, padding: '8px 14px', cursor: 'pointer', fontSize: 12.5,
              display: 'flex', alignItems: 'center', gap: 8,
            }}>
            <FontAwesomeIcon icon={faRotate} /> Refresh
          </button>
        </div>
      </div>

      {/* ── Loading (first paint only — usePolling keeps data mounted after) ── */}
      {poll.loading && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, color: colors.textMuted, fontSize: 13, padding: '18px 4px' }}>
          <FontAwesomeIcon icon={faSpinner} spin /> Loading the audience catalog…
        </div>
      )}

      {/* ── 404 fail-soft: overview endpoint not on this server build ── */}
      {!poll.loading && unavailable && (
        <div style={{
          padding: '14px 16px', borderRadius: 8, fontSize: 12.5, lineHeight: 1.6,
          background: alpha(colors.warning, '14'), border: `1px solid ${alpha(colors.warning, '44')}`,
          color: colors.warning, display: 'flex', alignItems: 'center', gap: 14, flexWrap: 'wrap',
        }}>
          <span>
            <strong>SOURCE UNAVAILABLE.</strong> <code style={{ fontFamily: 'monospace' }}>{OVERVIEW_URL}</code> is not
            exposed by this server build — the Audiences overview backend has not been deployed here yet. The screen
            keeps retrying every {POLL_MS / 1000}s.
          </span>
          <button
            onClick={() => poll.refresh()}
            style={{
              background: alpha(colors.warning, '22'), border: `1px solid ${alpha(colors.warning, '66')}`,
              color: colors.warningText, borderRadius: 6, padding: '5px 12px', cursor: 'pointer',
              fontSize: 12, fontWeight: 600,
            }}>
            Retry now
          </button>
        </div>
      )}

      {/* ── Fetch error: clear panel + retry; last good data stays below if any ── */}
      {!poll.loading && poll.error && !unavailable && (
        <div style={{ marginBottom: 12 }}>
          <SectionError label="audience overview" error={poll.error} onRetry={() => poll.refresh()} />
        </div>
      )}

      {!poll.loading && !unavailable && d?.kind === 'ok' && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>

          {/* ── (a) KPI band ── */}
          <div style={panelStyle}>
            <SectionHeader title="Estate at a glance" icon={faUsers} />
            <div style={{ display: 'flex', gap: 26, flexWrap: 'wrap' }}>
              <Stat label="Active segments" value={fmtInt(activeCount)}
                sub={`of ${fmtInt(allRows.length)} total in catalog`} />
              <Stat label="Members" value={fmtInt(membersSum)}
                sub="SUM of per-segment counts — overlap NOT deduped*"
                title="A subscriber in N segments counts N times; this is a size-of-estate signal, not a reachable-audience count." />
              <Stat label="Stale" value={fmtInt(staleCount)}
                color={staleCount > 0 ? colors.danger : colors.textMuted}
                sub="membership build past declared SLA" />
              <Stat label="Prunable" value={fmtInt(prunableCount)}
                color={prunableCount > 0 ? colors.warning : colors.textMuted}
                sub="no campaign reference in 30d — prune clock running" />
            </div>
          </div>

          {/* ── (b) Catalog filters (shared chrome; live-apply, client-side) ── */}
          <div style={{ background: colors.panelBgSolid, border: `1px solid ${colors.hairline}`, borderRadius: 10, padding: '14px 18px' }}>
            <div style={{ display: 'flex', alignItems: 'flex-end', gap: 12, flexWrap: 'wrap' }}>
              <label style={fieldLabelStyle}>
                <span><FontAwesomeIcon icon={faSearch} style={{ marginRight: 5 }} />Search</span>
                <input type="text" value={search} placeholder="name, category or definition"
                  onChange={e => setSearch(e.target.value)}
                  style={{ ...inputStyle, width: 230 }} />
              </label>
              <label style={fieldLabelStyle}>Status
                <select value={status} onChange={e => setStatus(e.target.value)} style={{ ...inputStyle, width: 130 }}>
                  <option value="all">all</option>
                  {statusOptions.map(s => <option key={s} value={s}>{s}</option>)}
                </select>
              </label>
              <div style={fieldLabelStyle}>Verdict
                <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
                  {(['LIVE', 'STALE', 'STATIC-DECLARED', 'UNREGISTERED'] as Verdict[]).map(v => (
                    <button key={v} type="button" style={chipStyle(verdicts.has(v), verdictColor(v))}
                      onClick={() => setVerdicts(s => toggleSet(s, v))}>
                      {v}
                    </button>
                  ))}
                </div>
              </div>
            </div>
            {categoryOptions.length > 0 && (
              <div style={{ display: 'flex', alignItems: 'center', gap: 6, flexWrap: 'wrap', marginTop: 10 }}>
                <span style={{ fontSize: 10, color: colors.textFaint, textTransform: 'uppercase', letterSpacing: 0.5 }}>Category</span>
                {chipCategories.map(([c, n]) => (
                  <button key={c} type="button" style={chipStyle(categories.has(c), colors.indigo300)}
                    title={`${fmtInt(n)} segments`}
                    onClick={() => setCategories(s => toggleSet(s, c))}>
                    {c} <span style={{ opacity: 0.65, fontVariantNumeric: 'tabular-nums' }}>{fmtInt(n)}</span>
                  </button>
                ))}
                {overflowCategories.length > 0 && (
                  <select value="" onChange={e => { if (e.target.value) setCategories(s => toggleSet(s, e.target.value)); }}
                    style={{ ...inputStyle, padding: '4px 8px', fontSize: 11.5 }}>
                    <option value="">+{overflowCategories.length} more…</option>
                    {overflowCategories.map(([c, n]) => <option key={c} value={c}>{c} ({fmtInt(n)})</option>)}
                  </select>
                )}
              </div>
            )}
            {activeChips.length > 0 && (
              <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', marginTop: 12 }}>
                <span style={{ fontSize: 11, color: colors.textFaint, textTransform: 'uppercase', letterSpacing: 0.5 }}>Active:</span>
                {activeChips.map((c, i) => <FilterChip key={i} label={c.label} tone={c.tone} onRemove={c.onRemove} />)}
              </div>
            )}
          </div>

          {/* ── (c) The catalog table ── */}
          <div style={panelStyle}>
            <SectionHeader
              title={`Segment catalog — ${fmtInt(filtered.length)} of ${fmtInt(allRows.length)} segments`}
              icon={faUsers}
            />
            {filtered.length === 0 ? (
              <EmptyState
                title={allRows.length === 0 ? 'No segments in the catalog' : 'No segments match the active filters'}
                hint={allRows.length === 0
                  ? 'The overview endpoint returned an empty catalog for this org — nothing to display (built empty, not a fetch failure).'
                  : 'Remove a chip above or clear the search to widen the catalog.'}
              />
            ) : (
              <div className="ac-scroll">
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={{ ...thStyle, width: 24 }} />
                      <th style={thStyle} className="ac-sort" onClick={() => onSort('name')}>Name{sortArrow('name')}</th>
                      <th style={thStyle}>Definition</th>
                      <th style={numTh} className="ac-sort" onClick={() => onSort('members')}
                        title="Materialized member count; subline = when membership was last BUILT (ledger), never a count-refresh clock">
                        Members{sortArrow('members')}
                      </th>
                      <th style={numTh} className="ac-sort" onClick={() => onSort('last_built_at')}
                        title="Membership last built (build ledger)">Built{sortArrow('last_built_at')}</th>
                      <th style={numTh} className="ac-sort" onClick={() => onSort('open_rate')}
                        title="unique opens ÷ delivered, trailing 7 Denver days (RAW, machine incl.); 30d in the cell tooltip">
                        Open%/deliv · 7d{sortArrow('open_rate')}
                      </th>
                      <th style={numTh}
                        title="unique clicks ÷ delivered, trailing 7 Denver days; 30d in the cell tooltip">
                        Click%/deliv · 7d
                      </th>
                      <th style={numTh} className="ac-sort" onClick={() => onSort('last_mailed_at')}
                        title="Most recent campaign referencing this segment; subline = campaigns in the last 30d">
                        Last mailed{sortArrow('last_mailed_at')}
                      </th>
                      <th style={thStyle}>Verdict</th>
                      <th style={thStyle} title="Self-pruning: segments with no campaign reference in 30d dissolve (archive-only). '—' = protected.">Prune</th>
                    </tr>
                  </thead>
                  <tbody>
                    {/* Pinned totals row over the FILTERED set (design system §1.4) */}
                    <tr style={{ background: alpha(colors.indigo500, '0d') }}>
                      <td style={tdStyle} />
                      <td style={{ ...tdStyle, fontWeight: 700 }}>TOTALS (filtered)</td>
                      <td style={tdStyle} />
                      <td style={{ ...numTd, fontWeight: 700 }} title="SUM of per-segment counts — overlap NOT deduped">
                        {fmtInt(totals.members)}*
                      </td>
                      <td style={numTd} />
                      <td style={{ ...numTd, fontWeight: 700 }}
                        title={totals.withPerf > 0
                          ? `Σ unique opens ${fmtInt(totals.opens)} / Σ delivered ${fmtInt(totals.delivered)} across the ${fmtInt(totals.withPerf)} filtered segments WITH a 7d rollup (computed from counts, never an average of rates)`
                          : 'no filtered segment has a 7d rollup'}>
                        {totals.delivered > 0 ? fmtPct(totals.opens / totals.delivered) : '—'}
                      </td>
                      <td style={{ ...numTd, fontWeight: 700 }}
                        title={totals.withPerf > 0
                          ? `Σ unique clicks ${fmtInt(totals.clicks)} / Σ delivered ${fmtInt(totals.delivered)} across the ${fmtInt(totals.withPerf)} filtered segments WITH a 7d rollup`
                          : 'no filtered segment has a 7d rollup'}>
                        {totals.delivered > 0 ? fmtPct(totals.clicks / totals.delivered) : '—'}
                      </td>
                      <td style={numTd} title="campaigns_30d summed across the filtered set">
                        {fmtInt(totals.campaigns)} <span style={{ color: colors.textFaint, fontSize: 10.5 }}>cmp/30d</span>
                      </td>
                      <td style={tdStyle} />
                      <td style={tdStyle} />
                    </tr>
                    {rendered.map(s => {
                      const isOpen = expanded === s.id;
                      const neverBuilt = s.last_built_at == null;
                      const cached = s.audience_source === 'cached';
                      const pd = pruneDays(s.prune_at);
                      const msg = refreshMsg[s.id];
                      return (
                        <React.Fragment key={s.id}>
                          <tr className="ac-row" onClick={() => onExpand(s.id)}>
                            <td style={{ ...tdStyle, width: 24, color: colors.textMuted }}>
                              <FontAwesomeIcon icon={isOpen ? faChevronDown : faChevronRight} />
                            </td>
                            <td style={{ ...tdStyle, maxWidth: 300 }} title={`${s.name} · ${s.id}`}>
                              <div style={{ fontWeight: 600, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{s.name}</div>
                              <div style={{ fontSize: 10.5, color: colors.textFaint }}>
                                {s.category || '(uncategorized)'} · {s.segment_type}{s.status !== 'active' ? ` · ${s.status}` : ''}
                              </div>
                            </td>
                            <td style={tdStyle}>
                              <div className="ac-def" title={s.conditions_summary || 'no stored definition'}>
                                {s.conditions_summary || <span style={{ color: colors.textFaint }}>no stored definition</span>}
                              </div>
                            </td>
                            <td style={numTd}>
                              <div style={{ fontWeight: 600 }}>{fmtInt(s.members)}</div>
                              {neverBuilt ? (
                                <div style={{ fontSize: 10.5, color: colors.danger, fontWeight: 700 }}
                                  title="No membership build ever recorded — the count is not backed by a materialization">
                                  never built
                                </div>
                              ) : (
                                <div style={{ fontSize: 10.5, color: cached ? colors.warning : colors.textFaint }}
                                  title={cached
                                    ? `audience_source=cached — count served from cache, not a fresh build · built ${s.last_built_at}`
                                    : `built ${s.last_built_at} · build status ${s.last_build_status}`}>
                                  built {fmtRelative(s.last_built_at)}{cached ? ' · cached' : ''}
                                </div>
                              )}
                            </td>
                            <td style={{ ...numTd, ...(neverBuilt ? { color: colors.danger, fontWeight: 700 } : {}) }}
                              title={s.last_built_at ?? 'no membership build ever recorded'}>
                              {neverBuilt ? 'NEVER' : fmtRelative(s.last_built_at)}
                            </td>
                            <td style={numTd} title={rateTitle('opens', s.perf_7d, s.perf_30d)}>
                              {s.perf_7d ? fmtPct(s.perf_7d.open_rate) : <span style={{ color: colors.textFaint }}>—</span>}
                            </td>
                            <td style={numTd} title={rateTitle('clicks', s.perf_7d, s.perf_30d)}>
                              {s.perf_7d ? fmtPct(s.perf_7d.click_rate) : <span style={{ color: colors.textFaint }}>—</span>}
                            </td>
                            <td style={numTd}>
                              {s.last_mailed_at == null ? (
                                <span style={{ color: colors.warning, fontWeight: 700, whiteSpace: 'nowrap' }}
                                  title="No campaign has ever referenced this segment (mailing_campaign_audiences)">
                                  <FontAwesomeIcon icon={faTriangleExclamation} style={{ marginRight: 5 }} />never mailed
                                </span>
                              ) : (
                                <>
                                  <div title={s.last_mailed_at}>{fmtRelative(s.last_mailed_at)}</div>
                                  <div style={{ fontSize: 10.5, color: colors.textFaint }}>{fmtInt(s.campaigns_30d)} cmp/30d</div>
                                </>
                              )}
                            </td>
                            <td style={tdStyle}><Pill color={verdictColor(s.verdict)}>{s.verdict}</Pill></td>
                            <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>
                              {pd == null ? (
                                <span style={{ color: colors.textFaint }} title="Protected — no prune clock (registry keep policy or recent campaign use)">—</span>
                              ) : (
                                <span style={{ color: pd <= 7 ? colors.danger : colors.warning, fontWeight: 700 }}
                                  title={`prune_at ${s.prune_at} — archive-only dissolve, never a hard delete`}>
                                  <FontAwesomeIcon icon={faHourglassHalf} style={{ marginRight: 5 }} />
                                  {pd <= 0 ? 'dissolving' : `dissolves in ${pd}d`}
                                </span>
                              )}
                            </td>
                          </tr>
                          {isOpen && (
                            <tr>
                              <td colSpan={10} style={{ padding: 0, borderTop: `1px solid ${colors.divider}`, background: alpha(colors.indigo500, '0d') }}>
                                <div className="ac-expand" onClick={e => e.stopPropagation()}>
                                  <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
                                    <span style={{ fontFamily: 'monospace', fontSize: 11, color: colors.textFaint }}>{s.id}</span>
                                    <Pill color={s.last_build_status === 'ok' ? colors.success
                                      : s.last_build_status === 'none' || s.last_build_status === '' ? colors.idle
                                      : s.last_build_status === 'running' ? colors.info : colors.danger}>
                                      build: {s.last_build_status || 'none'}
                                    </Pill>
                                    {s.audience_source && (
                                      <span style={{ fontSize: 11, color: cached ? colors.warningText : colors.textMuted }}>
                                        audience_source={s.audience_source}
                                      </span>
                                    )}
                                    <button
                                      onClick={() => void doRefresh(s.id, s.name)}
                                      disabled={!!refreshing[s.id]}
                                      title="POST /v2/segments/{id}/refresh — synchronous rebuild for dynamic segments, queued async build for lake segments"
                                      style={{
                                        marginLeft: 'auto',
                                        background: alpha(colors.indigo500, '22'), border: `1px solid ${alpha(colors.indigo500, '66')}`,
                                        color: colors.indigo200, borderRadius: 6, padding: '5px 12px',
                                        cursor: refreshing[s.id] ? 'wait' : 'pointer', fontSize: 12, fontWeight: 600,
                                        display: 'inline-flex', alignItems: 'center', gap: 7,
                                      }}>
                                      <FontAwesomeIcon icon={refreshing[s.id] ? faSpinner : faRotate} spin={!!refreshing[s.id]} />
                                      {refreshing[s.id] ? 'Refreshing…' : 'Refresh membership'}
                                    </button>
                                  </div>
                                  {msg && (
                                    <div style={{
                                      fontSize: 12, padding: '7px 10px', borderRadius: 6,
                                      background: alpha(msg.ok ? colors.success : colors.danger, '14'),
                                      border: `1px solid ${alpha(msg.ok ? colors.success : colors.danger, '44')}`,
                                      color: msg.ok ? colors.successText : colors.dangerText,
                                    }}>
                                      {msg.text}
                                    </div>
                                  )}
                                  <div>
                                    <div style={{ fontSize: 10.5, color: colors.textFaint, textTransform: 'uppercase', letterSpacing: 0.5, marginBottom: 5 }}>
                                      Full criteria
                                    </div>
                                    <CriteriaBlock detail={details[s.id]} onRetry={() => void loadDetail(s.id)} />
                                  </div>
                                </div>
                              </td>
                            </tr>
                          )}
                        </React.Fragment>
                      );
                    })}
                  </tbody>
                </table>
                {filtered.length > rendered.length && (
                  <div style={{ fontSize: 11.5, color: colors.warningText, marginTop: 8 }}>
                    Showing the first {fmtInt(rendered.length)} of {fmtInt(filtered.length)} matching segments (sorted by {sortKey}) — refine the filters to narrow the catalog; totals above cover ALL {fmtInt(filtered.length)} matches.
                  </div>
                )}
                <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 8, lineHeight: 1.6 }}>
                  Members* = per-segment materialized counts; sums are NOT deduped across segments.
                  Built = membership build ledger (<code>last_built_at</code>) — never a count-refresh clock; amber "cached" = the count came from cache, red NEVER = no build recorded.
                  Open%/Click% = unique opens/clicks ÷ delivered over the trailing 7 Denver days from the nightly segment perf rollup (opens are RAW, machine incl.); 30d window in each cell's tooltip; "—" = no rollup for that segment.
                  Last mailed comes from campaign→audience links (<code>mailing_campaign_audiences</code>); prune = archive-only dissolve when a segment goes 30d without a campaign reference.
                </div>
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  );
};

export default AudienceCommand;
