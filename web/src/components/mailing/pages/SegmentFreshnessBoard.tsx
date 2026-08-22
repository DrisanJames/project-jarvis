import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faTableCells, faRotate, faSpinner, faTriangleExclamation,
  faCircleCheck, faCircleXmark, faChevronDown, faChevronRight,
  faGaugeHigh, faHourglassHalf,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import { colors, alpha, panelStyle, btnStyle } from '../shared/theme';
import { SectionHeader, Stat, Pill, SectionError, EmptyState, LivePill, ProgressBar, PortalKeyframes } from '../shared/ui';
import { usePolling } from '../shared/usePolling';

// =============================================================================
// SEGMENT FRESHNESS BOARD — per-sending-domain dashboard, progress-first
// =============================================================================
// BOARD_VERSION 2.0 (2026-08-21) — operator rework after using v1.0 in prod:
//
//   "AAD 30d openers: 1 queued. Then I wait, no progress, so I do not know what
//    to do at this point. I tap x on the notification and the screen seems like
//    it is not doing anything… Segment families Alerts I do not care for.
//    Estate verdict would be nice to see an overall count of active openers and
//    clickers unique and to see the trend."
//   "let's simplify. We need a dashboard feel for the sending domain you are
//    looking at. Default to Discount Blog; when I toggle a dropdown I can filter
//    the stats and understand the segments belonging to that domain."
//
// The backend was NOT broken: the click reached mailing_segment_refresh_requests
// and went queued→running in <1s; a full membership rebuild genuinely takes
// 3–7 minutes on this DB. The defect was entirely that the UI never showed the
// transition, never showed elapsed time, and buried the only visible signal in
// a dismissible toast. v2.0 therefore:
//   1. tracks a per-segment refresh JOB with a live phase + elapsed timer, and
//      renders it ON THE CARD (queued → running → done/failed), never only in
//      a toast. Dismissing the toast NEVER touches job state.
//   2. leads with ONE sending domain (default DB / Discount Blog) as a card
//      dashboard, filtered client-side from the rows the endpoint already
//      returns — no new endpoints, no new query params.
//   3. replaces the dense all-domain table with an ESTATE VERDICT panel:
//      active openers/clickers totals + a per-domain health strip.
//
// Source: GET /api/mailing/segments/freshness (contract fixed with
// internal/api/segment_freshness_handlers.go — this component renders that
// contract's fields and nothing else).
// Action: POST /api/mailing/segments/refresh {"segment_ids":[...]} -> 202
// {queued:[...], already:[...]}.
//
// HONESTY RULES (this screen's law, unchanged from v1.0):
//  - 'unknown' freshness is NEVER green and NEVER rendered as 0.
//  - member_count is number|null — null renders '?', never NaN, never 0.
//  - member_count 0 with a FRESH members_stamped_at is a measurement artifact
//    (the silent-zero defect class): render the stamp, flag the discrepancy,
//    never present the 0 as the audience size.
//  - never-built, built-empty, missing-cell, and fetch-error are four
//    different displays. Absence ≠ zero, anywhere.
//  - The estate totals are a SUM of per-segment counts. That is NOT a
//    deduplicated unique across sending domains and the panel says so.
//  - No trend is drawn: the freshness contract carries no history, and an
//    invented trend is worse than an absent one.

const BOARD_VERSION = '2.0';

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
  member_count: number | null;
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

// Default sending domain for the dashboard (operator: "Default to Discount
// Blog"). If the feed has no DB rows we fall back to the first code
// alphabetically — the selector is never blank.
const DEFAULT_BRAND = 'DB';
const BRAND_STORAGE_KEY = 'segment-freshness-brand';

// Segment brand code → sending domain. VERIFIED 2026-08-21 by reading the
// live `sending_domain` condition off every active grid segment (28 codes,
// 1:1). This is DISPLAY ONLY: segment codes are NOT the Python registry brand
// codes (HWS≠HW, LP≠LPL, MR≠MRD, RRU≠RR, TOT≠TT, YIH≠YI, MPF≠MP, PMD≠PD,
// TRB≠TR, BWP≠BW, WF≠WFY), so never resolve a brand through this map — it
// only labels the dropdown. An unmapped code renders as the bare code.
// REQUIREMENT handed back: expose `sending_domain` on the freshness row so
// this map can be deleted.
const BRAND_DOMAIN: Record<string, string> = {
  AAD: 'em.aadwd.com',
  BCC: 'em.bestcreditcare.com',
  BWP: 'em.businessweeklypro.com',
  CI: 'em.casainsure.com',
  CP: 'em.consumerpro.net',
  DB: 'em.discountblog.com',
  FC: 'em.financialcalculate.com',
  FTH: 'em.firsttimebuyerhomeloan.com',
  HFC: 'em.hfcl.net',
  HLJ: 'em.homeloansbyjaime.com',
  HT: 'em.historythinking.com',
  HTM: 'em.hometracmortgage.com',
  HWS: 'em.homewarrantyservices.org',
  LP: 'em.learnpersonalloans.com',
  MH: 'em.myownhealth.net',
  MPF: 'em.mypersonalfinancial.com',
  MR: 'em.myrepairdiy.com',
  PMD: 'em.paymydebit.com',
  QF: 'em.quizfiesta.com',
  RB: 'em.ratesbazar.com',
  RRU: 'em.refinanceratesusa.com',
  TOT: 'em.thingoftheday.org',
  TRB: 'em.theretirementblog.com',
  USF: 'em.us-finance.com',
  WCL: 'em.wcl-heloc.com',
  WF: 'em.warrantyforyou.com',
  YFB: 'em.yourfinancialblog.com',
  YIH: 'em.yourinsurancehub.com',
};

const POLL_ACTIVE_MS = 10_000; // anything queued/running → tight poll
const POLL_IDLE_MS = 60_000;

// A rebuild that we saw open (queued/running) and then saw close without the
// ledger advancing is only called FAILED after this grace — the request-status
// write and the ledger write are not one transaction.
const GRACE_AFTER_OPEN_MS = 45_000;
// A request the server NEVER reported as queued/running within this window is
// reported LOST rather than left spinning forever.
const NEVER_SEEN_TTL_MS = 2 * 60_000;
// How long a finished (done/failed/lost) job stays visible on its card.
const TERMINAL_KEEP_MS = 5 * 60_000;
// Operator expectation-setting: measured 3–7 min for a full membership
// rebuild on this DB (FC 7D Openers = 195s / 22,840 members, 2026-08-21).
const TYPICAL_REBUILD_MS = 7 * 60_000;

// A 0-count with a membership stamp newer than this is flagged as a
// measurement artifact (count zeroed while membership demonstrably built —
// the segment_subscriber_count silent-zero defect class).
const SILENT_ZERO_STAMP_HOURS = 24;

// ── Refresh job lifecycle (the fix for "no progress") ───────────────────────

type JobPhase = 'queued' | 'running' | 'done' | 'failed' | 'lost';

interface RefreshJob {
  segmentId: string;
  label: string;
  /** epoch ms when WE posted it. null = adopted from the server (e.g. after a
   *  reload, or a worker-initiated pass) — elapsed is then unknown, not 0. */
  requestedAt: number | null;
  firstSeenAt: number;
  /** last poll at which the server reported this segment queued/running. */
  lastOpenAt: number | null;
  phase: JobPhase;
  /** last_built_at at request time — completion = the ledger moves past it. */
  baselineBuiltAt: number | null;
  finishedAt: number | null;
  detail: string;
}

const isOpenPhase = (p: JobPhase): boolean => p === 'queued' || p === 'running';

// ── Display helpers ─────────────────────────────────────────────────────────

const fmtInt = (n: number | null): string => (n == null ? '?' : n.toLocaleString());

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

/** Elapsed as "3m 12s" — the operator's missing signal. */
const fmtElapsed = (ms: number): string => {
  const s = Math.max(0, Math.floor(ms / 1000));
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, '0')}s`;
};

const freshnessColor = (f: Freshness): string => {
  switch (f) {
    case 'fresh': return colors.success;
    case 'aging': return colors.warning;
    case 'stale': return colors.danger;
    case 'unknown': return colors.idle; // UNKNOWN IS NEVER GREEN
  }
};

const phaseColor = (p: JobPhase): string => {
  switch (p) {
    case 'queued': return colors.info;
    case 'running': return colors.indigo300;
    case 'done': return colors.success;
    case 'failed': return colors.danger;
    case 'lost': return colors.warning;
  }
};

// Worst-first severity so a domain dot reports its worst cell, and 'unknown'
// (no verified measurement) is never softened into 'aging'.
const SEVERITY: Record<Freshness, number> = { stale: 3, unknown: 2, aging: 1, fresh: 0 };

const brandLabel = (code: string): string => {
  const d = BRAND_DOMAIN[code];
  return d ? `${code} — ${d}` : code;
};

const readStoredBrand = (): string => {
  try {
    const v = window.localStorage.getItem(BRAND_STORAGE_KEY);
    return v && /^[A-Z]{2,4}$/.test(v) ? v : '';
  } catch { return ''; } // storage blocked — fall through to the default
};

// ── Component ───────────────────────────────────────────────────────────────

export const SegmentFreshnessBoard: React.FC = () => {
  const [jobs, setJobs] = useState<Record<string, RefreshJob>>({});
  const [posting, setPosting] = useState(false);
  const [actionMsg, setActionMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const [anyActive, setAnyActive] = useState(false);
  const [brandChoice, setBrandChoice] = useState<string>(readStoredBrand);
  const [estateWin, setEstateWin] = useState<number>(30);
  const [showOffGrid, setShowOffGrid] = useState(false);

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

  // ── Job reconciliation: the single place the lifecycle advances ──────────
  // Runs once per successful poll. Adopts server-reported activity we did not
  // start, advances queued→running, and closes a job as done/failed/lost with
  // the server's own reason string.
  useEffect(() => {
    if (!data) return;
    const now = Date.now();
    const byId = new Map(rows.map(r => [r.segment_id, r]));

    setJobs(prev => {
      const next: Record<string, RefreshJob> = { ...prev };
      let changed = false;

      // 1. Adopt any open server state we are not already tracking (page
      //    reload mid-build, or a worker-initiated pass). requestedAt stays
      //    null so the card says "elapsed unknown" rather than lying "0s".
      for (const r of rows) {
        if (!r.refresh_state) continue;
        const existing = next[r.segment_id];
        if (existing && isOpenPhase(existing.phase)) continue;
        next[r.segment_id] = {
          segmentId: r.segment_id,
          label: r.name,
          requestedAt: null,
          firstSeenAt: now,
          lastOpenAt: now,
          phase: r.refresh_state,
          baselineBuiltAt: r.last_built_at ? Date.parse(r.last_built_at) : null,
          finishedAt: null,
          detail: '',
        };
        changed = true;
      }

      // 2. Advance / close everything we are tracking.
      for (const id of Object.keys(next)) {
        const j = next[id];
        if (!isOpenPhase(j.phase)) {
          if (j.finishedAt != null && now - j.finishedAt > TERMINAL_KEEP_MS) {
            delete next[id];
            changed = true;
          }
          continue;
        }
        const r = byId.get(id);
        if (!r) continue; // segment vanished from the feed — leave the job as-is

        if (r.refresh_state === 'queued' || r.refresh_state === 'running') {
          next[id] = { ...j, phase: r.refresh_state, lastOpenAt: now };
          changed = true;
          continue;
        }

        // No open request on the server. Did the build ledger move?
        const built = r.last_built_at ? Date.parse(r.last_built_at) : NaN;
        const advanced = !isNaN(built) && (j.baselineBuiltAt == null || built > j.baselineBuiltAt);
        if (advanced) {
          next[id] = {
            ...j, phase: 'done', finishedAt: now,
            detail: `rebuilt: ${fmtInt(r.member_count)} members`,
          };
          changed = true;
        } else if (j.lastOpenAt != null && now - j.lastOpenAt > GRACE_AFTER_OPEN_MS) {
          const reason = r.last_error
            || (r.last_build_status ? `last_build_status=${r.last_build_status}` : '');
          next[id] = {
            ...j, phase: 'failed', finishedAt: now,
            detail: reason || 'the request left the queue without recording a new build',
          };
          changed = true;
        } else if (j.lastOpenAt == null && now - j.firstSeenAt > NEVER_SEEN_TTL_MS) {
          next[id] = {
            ...j, phase: 'lost', finishedAt: now,
            detail: 'the server never reported this request as queued or running',
          };
          changed = true;
        }
      }

      // NOTE: anyActive is derived in its own effect below — a state updater
      // must stay pure, so the poll cadence is never flipped from in here.
      return changed ? next : prev;
    });
  }, [data, rows]);

  // Poll cadence follows real activity: tight while anything is queued/running.
  useEffect(() => {
    setAnyActive(Object.values(jobs).some(j => isOpenPhase(j.phase)));
  }, [jobs]);

  // Persist the operator's domain choice (guarded — storage may be blocked).
  useEffect(() => {
    if (!brandChoice) return;
    try { window.localStorage.setItem(BRAND_STORAGE_KEY, brandChoice); } catch { /* ignore */ }
  }, [brandChoice]);

  // ── Projections ──────────────────────────────────────────────────────────
  // brand → `${kind}|${window}` → row, plus anything grid-shaped that does not
  // land in the 8 cells (never silently dropped).
  const { brands, cellMap, offGridByBrand } = useMemo(() => {
    const map = new Map<string, Map<string, FreshnessRow>>();
    const off = new Map<string, FreshnessRow[]>();
    for (const r of rows) {
      const inGrid = KINDS.includes(r.kind) && (WINDOWS as readonly number[]).includes(r.window_days);
      const key = `${r.kind}|${r.window_days}`;
      let m = map.get(r.brand);
      if (!m) { m = new Map(); map.set(r.brand, m); }
      if (!inGrid || m.has(key)) {
        const list = off.get(r.brand) ?? [];
        list.push(r);
        off.set(r.brand, list);
        continue;
      }
      m.set(key, r);
    }
    return { brands: [...map.keys()].sort(), cellMap: map, offGridByBrand: off };
  }, [rows]);

  // Never blank: stored choice → DB → first alphabetically.
  const selected = useMemo(() => {
    if (brands.length === 0) return '';
    if (brandChoice && brands.includes(brandChoice)) return brandChoice;
    if (brands.includes(DEFAULT_BRAND)) return DEFAULT_BRAND;
    return brands[0];
  }, [brands, brandChoice]);

  const cellsFor = useCallback((brand: string): (FreshnessRow | undefined)[] =>
    KINDS.flatMap(k => WINDOWS.map(d => cellMap.get(brand)?.get(`${k}|${d}`))),
  [cellMap]);

  const domainCells = useMemo(() => cellsFor(selected), [cellsFor, selected]);

  const domainStats = useMemo(() => {
    const present = domainCells.filter((r): r is FreshnessRow => r != null);
    const stale = present.filter(r => r.freshness === 'stale').length;
    const fresh = present.filter(r => r.freshness === 'fresh').length;
    const unknown = present.filter(r => r.freshness === 'unknown').length;
    const missing = domainCells.length - present.length;
    let oldest: string | null = null;
    for (const r of present) {
      if (!r.last_built_at) continue;
      if (oldest == null || Date.parse(r.last_built_at) < Date.parse(oldest)) oldest = r.last_built_at;
    }
    const neverBuilt = present.filter(r => r.last_built_at == null).length;
    const inFlight = present.filter(r => {
      const j = jobs[r.segment_id];
      return j != null && isOpenPhase(j.phase);
    }).length;
    return { present, stale, fresh, unknown, missing, oldest, neverBuilt, inFlight };
  }, [domainCells, jobs]);

  // Refresh targets for a set of rows: skip anything already in flight.
  const refreshable = useCallback((list: (FreshnessRow | undefined)[]): FreshnessRow[] =>
    list.filter((r): r is FreshnessRow =>
      r != null && !isOpenPhase(jobs[r.segment_id]?.phase ?? 'done')),
  [jobs]);

  const domainRefreshable = useMemo(() => refreshable(domainCells), [refreshable, domainCells]);
  const staleRefreshable = useMemo(
    () => refreshable(rows.filter(r => r.freshness === 'stale')),
    [refreshable, rows],
  );

  const activeJobs = useMemo(
    () => Object.values(jobs).filter(j => isOpenPhase(j.phase)).sort((a, b) => a.firstSeenAt - b.firstSeenAt),
    [jobs],
  );

  // ── Estate verdict (replaces the dense all-domain table) ─────────────────
  const estate = useMemo(() => {
    let openers = 0, clickers = 0;
    let oCells = 0, cCells = 0, noMeasure = 0, artifacts = 0;
    for (const b of brands) {
      for (const k of KINDS) {
        const r = cellMap.get(b)?.get(`${k}|${estateWin}`);
        if (!r || r.member_count == null) { noMeasure++; continue; }
        const stampH = ageHours(r.members_stamped_at);
        if (r.member_count === 0 && stampH != null && stampH < SILENT_ZERO_STAMP_HOURS) artifacts++;
        if (k === 'openers') { openers += r.member_count; oCells++; }
        else { clickers += r.member_count; cCells++; }
      }
    }
    return { openers, clickers, oCells, cCells, noMeasure, artifacts, domains: brands.length };
  }, [brands, cellMap, estateWin]);

  const healthStrip = useMemo(() => brands.map(b => {
    const cells = cellsFor(b);
    const present = cells.filter((r): r is FreshnessRow => r != null);
    let worst: Freshness = present.length === 0 ? 'unknown' : 'fresh';
    for (const r of present) if (SEVERITY[r.freshness] > SEVERITY[worst]) worst = r.freshness;
    const missing = cells.length - present.length;
    if (missing > 0 && SEVERITY.unknown > SEVERITY[worst]) worst = 'unknown';
    return { brand: b, worst, missing, cells: present.length };
  }), [brands, cellsFor]);

  // ── Refresh POST ─────────────────────────────────────────────────────────
  // The per-domain button sends the domain's cell ids rather than guessing a
  // family CODE — segment codes ≠ registry codes is a known trap.
  const queueRefresh = useCallback(async (targets: FreshnessRow[], what: string) => {
    if (targets.length === 0 || posting) return;
    setPosting(true);
    setActionMsg(null);
    try {
      const res = await apiFetch('/api/mailing/segments/refresh', {
        method: 'POST',
        body: JSON.stringify({ segment_ids: targets.map(t => t.segment_id) }),
      });
      if (res.ok) { // 202 expected
        let queued: string[] = [];
        let already: string[] = [];
        try {
          const body = (await res.json()) as { queued?: string[]; already?: string[] };
          queued = body.queued ?? [];
          already = body.already ?? [];
        } catch { /* body optional — fall through with empty acks */ }
        const acked = new Set([...queued, ...already]);
        const trackAll = acked.size === 0; // no ack lists → track what we sent
        const now = Date.now();
        setJobs(prev => {
          const next = { ...prev };
          for (const t of targets) {
            if (!trackAll && !acked.has(t.segment_id)) continue;
            next[t.segment_id] = {
              segmentId: t.segment_id,
              label: t.name,
              requestedAt: now,
              firstSeenAt: now,
              lastOpenAt: null,
              phase: 'queued',
              baselineBuiltAt: t.last_built_at ? Date.parse(t.last_built_at) : null,
              finishedAt: null,
              detail: '',
            };
          }
          return next;
        });
        setAnyActive(true);
        setActionMsg({
          ok: true,
          text: `${what} — ${queued.length} queued${already.length > 0 ? `, ${already.length} already in flight` : ''}. `
            + 'A full membership rebuild takes 3–7 minutes; live progress is on the card, not here.',
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

  // ── Job line — the live lifecycle rendered ON the card ───────────────────
  // Plain render functions, NOT nested components: a component declared inside
  // the render body is a new type every render, so React would unmount and
  // remount all 8 cards on every 1s clock tick (killing the progress bar's
  // transition and any hover state).
  const renderJobLine = (job: RefreshJob): React.ReactNode => {
    const c = phaseColor(job.phase);
    const elapsedMs = job.requestedAt != null ? Date.now() - job.requestedAt : null;
    const icon = job.phase === 'running' ? faSpinner
      : job.phase === 'queued' ? faHourglassHalf
        : job.phase === 'done' ? faCircleCheck
          : job.phase === 'failed' ? faCircleXmark : faTriangleExclamation;
    return (
      <div style={{
        marginTop: 6, padding: '5px 7px', borderRadius: 6,
        background: alpha(c, '14'), border: `1px solid ${alpha(c, '44')}`,
      }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 11, fontWeight: 700, color: c }}>
          <FontAwesomeIcon icon={icon} spin={job.phase === 'running'} />
          <span style={{ textTransform: 'uppercase', letterSpacing: 0.5 }}>{job.phase}</span>
          {isOpenPhase(job.phase) && (
            <span style={{ marginLeft: 'auto', fontVariantNumeric: 'tabular-nums', fontWeight: 600 }}>
              {elapsedMs != null ? fmtElapsed(elapsedMs) : 'elapsed unknown'}
            </span>
          )}
        </div>
        {isOpenPhase(job.phase) && (
          <div style={{ marginTop: 4 }}>
            <ProgressBar pct={elapsedMs != null ? Math.min(elapsedMs / TYPICAL_REBUILD_MS, 0.95) : 0.05} height={4} />
            <div style={{ fontSize: 9.5, color: colors.textMuted, marginTop: 3, lineHeight: 1.4 }}>
              {elapsedMs != null
                ? 'elapsed against a typical 3–7 min rebuild — the server does not report % complete'
                : 'started before this view loaded — the server does not send a request timestamp'}
            </div>
          </div>
        )}
        {!isOpenPhase(job.phase) && job.detail && (
          <div style={{ fontSize: 10, color: colors.textMuted, marginTop: 3, lineHeight: 1.4 }}>{job.detail}</div>
        )}
      </div>
    );
  };

  // ── Cell card ────────────────────────────────────────────────────────────
  const renderCellCard = (brand: string, kind: Kind, win: number): React.ReactNode => {
    const r = cellMap.get(brand)?.get(`${kind}|${win}`);
    const title = `${win}D ${kind === 'openers' ? 'Openers' : 'Clickers'}`;

    if (!r) {
      // Missing cell — the segment does not exist. Absence ≠ zero.
      return (
        <div
          key={`${kind}${win}`}
          title={`${brand} · ${title}: no segment exists for this cell (never built). Absence ≠ zero — this is not a count of 0.`}
          style={{
            border: `1px dashed ${alpha(colors.idle, '44')}`, borderRadius: 8, padding: '10px 12px',
            display: 'flex', flexDirection: 'column', gap: 4, minHeight: 108,
          }}
        >
          <div style={{ fontSize: 11, fontWeight: 700, color: colors.textMuted, letterSpacing: 0.4 }}>{title}</div>
          <div style={{ fontSize: 26, fontWeight: 700, color: colors.textFaint, lineHeight: 1.1 }}>—</div>
          <div style={{ fontSize: 10.5, color: colors.textFaint }}>NO SEGMENT — never created</div>
        </div>
      );
    }

    const c = freshnessColor(r.freshness);
    const job = jobs[r.segment_id];
    const inFlight = job != null && isOpenPhase(job.phase);
    const justDone = job != null && job.phase === 'done';
    const stampH = ageHours(r.members_stamped_at);
    const neverBuilt = r.last_built_at == null && r.members_stamped_at == null;
    const silentZero = r.member_count === 0 && stampH != null && stampH < SILENT_ZERO_STAMP_HOURS;

    // Big count — honest per state, never a bare misleading 0.
    let countNode: React.ReactNode;
    if (neverBuilt) {
      countNode = <span style={{ color: colors.textFaint }}>—</span>;
    } else if (silentZero) {
      countNode = <span style={{ color: colors.warningText }}>0*</span>;
    } else if (r.freshness === 'unknown') {
      countNode = r.member_count != null && r.member_count > 0
        ? <span style={{ color: colors.textMuted }}>{fmtInt(r.member_count)}</span>
        : <span style={{ color: colors.textMuted }}>?</span>;
    } else {
      countNode = <span style={{ color: colors.text }}>{fmtInt(r.member_count)}</span>;
    }

    const tooltip = [
      `${r.name} (${r.segment_id})`,
      `freshness: ${r.freshness.toUpperCase()} · status: ${r.status || '—'}`,
      `members: ${fmtInt(r.member_count)}${silentZero ? '  ⚠ COUNT 0 but membership stamped ' + fmtAge(r.members_stamped_at) + ' ago — measurement artifact (silent-zero), NOT the audience size' : ''}`,
      `last_built_at: ${r.last_built_at ?? 'NEVER'}${r.last_built_at ? ` (${fmtAge(r.last_built_at)} ago)` : ''}`,
      `members_stamped_at: ${r.members_stamped_at ?? '—'}${r.members_stamped_at ? ` (${fmtAge(r.members_stamped_at)} ago)` : ''}`,
      `build_source: ${r.build_source || '—'} · last_build_status: ${r.last_build_status || '—'}`,
      r.last_error ? `last_error: ${r.last_error}` : '',
    ].filter(Boolean).join('\n');

    return (
      <div
        key={`${kind}${win}`}
        title={tooltip}
        style={{
          background: alpha(c, '14'),
          border: `1px solid ${alpha(justDone ? colors.success : c, r.freshness === 'stale' || justDone ? '66' : '44')}`,
          borderRadius: 8, padding: '10px 12px', minHeight: 108,
          display: 'flex', flexDirection: 'column', gap: 3,
        }}
      >
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 6 }}>
          <span style={{ fontSize: 11, fontWeight: 700, color: colors.heading, letterSpacing: 0.4 }}>{title}</span>
          <Pill color={c} style={{ fontSize: 9.5, padding: '1px 7px' }}>{r.freshness}</Pill>
        </div>

        <div style={{ fontSize: 26, fontWeight: 700, lineHeight: 1.1, fontVariantNumeric: 'tabular-nums' }}>
          {countNode}
        </div>

        <div style={{ fontSize: 10.5, color: colors.textMuted, lineHeight: 1.4 }}>
          {neverBuilt
            ? <span style={{ color: colors.dangerText, fontWeight: 700 }}>NEVER BUILT</span>
            : silentZero
              ? <span style={{ color: colors.warningText }}>0* artifact — membership stamped {fmtAge(r.members_stamped_at)} ago</span>
              : <span>built {r.last_built_at ? `${fmtAge(r.last_built_at)} ago` : 'NEVER'}</span>}
          {!neverBuilt && !silentZero && r.member_count === 0 && r.freshness !== 'unknown' && (
            <span style={{ color: colors.textFaint }}> · built empty</span>
          )}
        </div>

        {job && renderJobLine(job)}

        <button
          onClick={() => queueRefresh([r], r.name)}
          disabled={posting || inFlight}
          title={inFlight
            ? `A rebuild is already ${job?.phase} for ${r.name} — the button re-enables when it finishes`
            : `Queue a membership rebuild for ${r.name} (takes 3–7 minutes)`}
          style={{
            marginTop: 'auto', background: alpha(colors.indigo500, inFlight ? '0d' : '22'),
            border: `1px solid ${alpha(colors.indigo500, inFlight ? '33' : '44')}`,
            color: inFlight ? colors.textFaint : colors.indigo200,
            borderRadius: 6, padding: '4px 8px', fontSize: 10.5, fontWeight: 600,
            cursor: posting || inFlight ? 'default' : 'pointer',
          }}
        >
          <FontAwesomeIcon icon={faRotate} /> {inFlight ? 'rebuilding…' : 'Refresh'}
        </button>
      </div>
    );
  };

  // ── Render ───────────────────────────────────────────────────────────────
  const w = data?.worker;
  const cards = (kind: Kind) => (
    <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(168px, 1fr))', gap: 10 }}>
      {WINDOWS.map(d => renderCellCard(selected, kind, d))}
    </div>
  );

  const offGrid = offGridByBrand.get(selected) ?? [];

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <PortalKeyframes />

      {/* ═══ DOMAIN DASHBOARD ═══ */}
      <div style={panelStyle}>
        <SectionHeader
          title="Segment freshness — sending-domain dashboard"
          icon={faTableCells}
          right={
            <span style={{ display: 'inline-flex', alignItems: 'center', gap: 12 }}>
              <LivePill live={poll.live} agoSeconds={poll.secondsSinceUpdate} />
              <button
                onClick={() => queueRefresh(staleRefreshable, `refresh all stale (${staleRefreshable.length})`)}
                disabled={posting || staleRefreshable.length === 0}
                title={staleRefreshable.length === 0
                  ? 'No stale cells that are not already rebuilding'
                  : `Queue a rebuild for every STALE cell across the estate (${staleRefreshable.length} segments)`}
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
              generated {data ? fmtAge(data.generated_at) : '—'} ago · poll {anyActive ? '10s (rebuilds in flight)' : '60s (idle)'} · v{BOARD_VERSION}
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
            worker circuit open — DB under pressure, refreshes paused. Queued rebuilds will wait until the circuit closes.
          </div>
        )}

        {/* ── States: loading / 404 / error ── */}
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

        {/* ── In-flight strip: NOT dismissible. This is the progress signal the
             operator lost when they closed the toast. ── */}
        {activeJobs.length > 0 && (
          <div style={{
            padding: '8px 12px', borderRadius: 8, marginBottom: 10, fontSize: 11.5,
            background: alpha(colors.indigo500, '14'), border: `1px solid ${alpha(colors.indigo500, '44')}`,
            color: colors.indigo200,
          }}>
            <div style={{ fontWeight: 700, display: 'flex', alignItems: 'center', gap: 8, marginBottom: 4 }}>
              <FontAwesomeIcon icon={faSpinner} spin />
              {activeJobs.length} rebuild{activeJobs.length === 1 ? '' : 's'} in flight — a full rebuild takes 3–7 minutes
            </div>
            <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
              {activeJobs.map(j => (
                <span key={j.segmentId} style={{
                  fontFamily: 'monospace', fontSize: 10.5, padding: '1px 7px', borderRadius: 999,
                  border: `1px solid ${alpha(phaseColor(j.phase), '44')}`, color: phaseColor(j.phase),
                  fontVariantNumeric: 'tabular-nums',
                }}>
                  {j.label} · {j.phase}
                  {j.requestedAt != null ? ` ${fmtElapsed(Date.now() - j.requestedAt)}` : ''}
                </span>
              ))}
            </div>
          </div>
        )}

        {/* ── Action receipt. Closing this NEVER cancels or hides a rebuild —
             the cards and the in-flight strip above own that state. ── */}
        {actionMsg && (
          <div style={{
            padding: '8px 12px', borderRadius: 8, marginBottom: 10, fontSize: 12.5,
            background: alpha(actionMsg.ok ? colors.success : colors.danger, '14'),
            border: `1px solid ${alpha(actionMsg.ok ? colors.success : colors.danger, '44')}`,
            color: actionMsg.ok ? colors.successText : colors.dangerText,
            display: 'flex', alignItems: 'flex-start', gap: 10,
          }}>
            <FontAwesomeIcon icon={faRotate} style={{ marginTop: 3 }} />
            <span style={{ lineHeight: 1.5 }}>{actionMsg.text}</span>
            <button
              onClick={() => setActionMsg(null)}
              title="Dismiss this receipt. Rebuilds in flight keep running and keep showing progress on their cards."
              style={{ marginLeft: 'auto', background: 'none', border: 'none', color: 'inherit', cursor: 'pointer', fontSize: 12.5 }}
            >✕</button>
          </div>
        )}

        {data && brands.length === 0 && (
          <EmptyState
            title="No grid segments in the freshness feed"
            hint="The endpoint answered but returned no 7/14/30/60d Openers/Clickers rows — the grid has not been built for any sending domain."
          />
        )}

        {data && brands.length > 0 && (
          <>
            {/* ── Domain selector ── */}
            <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap', marginBottom: 12 }}>
              <label style={{ fontSize: 11, color: colors.textMuted, textTransform: 'uppercase', letterSpacing: 0.5 }}>
                Sending domain
              </label>
              <select
                value={selected}
                onChange={e => setBrandChoice(e.target.value)}
                style={{
                  background: colors.panelBgSolid, color: colors.text, fontSize: 12.5, fontWeight: 600,
                  border: `1px solid ${alpha(colors.indigo500, '66')}`, borderRadius: 8, padding: '6px 10px',
                  minWidth: 300,
                }}
              >
                {brands.map(b => <option key={b} value={b}>{brandLabel(b)}</option>)}
              </select>
              <button
                onClick={() => queueRefresh(domainRefreshable, `refresh all ${selected} cells`)}
                disabled={posting || domainRefreshable.length === 0}
                title={domainRefreshable.length === 0
                  ? `Every ${selected} cell is already rebuilding (or no cells exist)`
                  : `Queue a rebuild for all ${domainRefreshable.length} ${selected} grid segments (3–7 minutes each)`}
                style={{ ...btnStyle, opacity: posting || domainRefreshable.length === 0 ? 0.5 : 1 }}
              >
                <FontAwesomeIcon icon={faRotate} /> Refresh all {domainRefreshable.length} cells
              </button>
              <span style={{ fontSize: 11, color: colors.textFaint }}>
                {brands.length} sending domains in the feed · filtered client-side from one fetch
              </span>
            </div>

            {/* ── Domain KPIs ── */}
            <div style={{ display: 'flex', gap: 26, flexWrap: 'wrap', marginBottom: 14 }}>
              <Stat
                label="Openers 30D"
                value={fmtInt(cellMap.get(selected)?.get('openers|30')?.member_count ?? null)}
                sub="members in the 30-day opener segment"
                color={colors.text}
              />
              <Stat
                label="Clickers 30D"
                value={fmtInt(cellMap.get(selected)?.get('clickers|30')?.member_count ?? null)}
                sub="members in the 30-day clicker segment"
                color={colors.text}
              />
              <Stat
                label="Stale cells"
                value={`${domainStats.stale} / ${domainStats.present.length}`}
                color={domainStats.stale > 0 ? colors.danger : colors.textMuted}
                sub="build older than 48h (of cells that exist)"
              />
              <Stat
                label="Fresh cells"
                value={`${domainStats.fresh} / ${domainStats.present.length}`}
                color={colors.success}
                sub="built within 26h"
              />
              <Stat
                label="Oldest build"
                value={domainStats.oldest ? fmtAge(domainStats.oldest) : '—'}
                color={colors.textMuted}
                sub={domainStats.neverBuilt > 0
                  ? `${domainStats.neverBuilt} cell(s) NEVER built — excluded, not counted as 0`
                  : 'age of the oldest cell in this domain'}
              />
              <Stat
                label="Rebuilding"
                value={domainStats.inFlight}
                color={domainStats.inFlight > 0 ? colors.indigo300 : colors.textMuted}
                sub="cells queued or running right now"
              />
              {(domainStats.missing > 0 || domainStats.unknown > 0) && (
                <Stat
                  label="No measurement"
                  value={domainStats.missing + domainStats.unknown}
                  color={colors.warning}
                  title="Neither of these is a count of zero. A missing cell has no segment at all; an unknown cell has a segment with no verified successful build."
                  sub={`${domainStats.missing} cell(s) do not exist · ${domainStats.unknown} built but UNKNOWN — neither is a zero`}
                />
              )}
            </div>

            {/* ── The 8 cells ── */}
            <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
              <div>
                <div style={{ fontSize: 11, fontWeight: 700, color: colors.textMuted, letterSpacing: 0.6, marginBottom: 6 }}>
                  OPENERS — {BRAND_DOMAIN[selected] ?? selected}
                </div>
                {cards('openers')}
              </div>
              <div>
                <div style={{ fontSize: 11, fontWeight: 700, color: colors.textMuted, letterSpacing: 0.6, marginBottom: 6 }}>
                  CLICKERS — {BRAND_DOMAIN[selected] ?? selected}
                </div>
                {cards('clickers')}
              </div>
            </div>

            {/* ── Off-grid rows for this domain — counted, never dropped ── */}
            {offGrid.length > 0 && (
              <div style={{ marginTop: 12, fontSize: 11.5 }}>
                <button
                  onClick={() => setShowOffGrid(v => !v)}
                  style={{
                    background: 'none', border: 'none', color: colors.indigo300, cursor: 'pointer',
                    fontSize: 11.5, padding: 0, display: 'inline-flex', alignItems: 'center', gap: 6,
                  }}
                >
                  <FontAwesomeIcon icon={showOffGrid ? faChevronDown : faChevronRight} />
                  {offGrid.length} other {selected} segment{offGrid.length === 1 ? '' : 's'} outside the 8-cell grid
                </button>
                {showOffGrid && (
                  <div style={{ marginTop: 6, display: 'flex', flexDirection: 'column', gap: 4 }}>
                    {offGrid.map(r => (
                      <div key={r.segment_id} style={{
                        display: 'flex', gap: 10, alignItems: 'center', fontSize: 11,
                        color: colors.textMuted, fontFamily: 'monospace',
                      }}>
                        <Pill color={freshnessColor(r.freshness)} style={{ fontSize: 9, padding: '0 6px' }}>{r.freshness}</Pill>
                        <span style={{ color: colors.text }}>{r.name}</span>
                        <span style={{ fontVariantNumeric: 'tabular-nums' }}>{fmtInt(r.member_count)} members</span>
                        <span>built {r.last_built_at ? `${fmtAge(r.last_built_at)} ago` : 'NEVER'}</span>
                      </div>
                    ))}
                  </div>
                )}
              </div>
            )}

            <div style={{ fontSize: 10.5, color: colors.textMuted, marginTop: 10, lineHeight: 1.6 }}>
              Card color = server freshness verdict: <span style={{ color: colors.success }}>fresh &lt;26h</span> ·{' '}
              <span style={{ color: colors.warning }}>aging 26–48h</span> ·{' '}
              <span style={{ color: colors.danger }}>stale &gt;48h</span> ·{' '}
              <span style={{ color: colors.idle }}>unknown = no verified build (never green, never 0)</span>.
              Big number = <code>member_count</code> from the build ledger. <span style={{ color: colors.warningText }}>0*</span> = the
              count reads 0 while membership was stamped within {SILENT_ZERO_STAMP_HOURS}h — a measurement artifact (silent-zero),
              NOT the audience size. A dashed card = the segment does not exist (absence ≠ zero); NEVER BUILT and "built empty"
              are distinct states. Refresh POSTs to <code>/api/mailing/segments/refresh</code> and the card then shows the live
              lifecycle (queued → running with elapsed → done/failed with the server's reason); the receipt banner is only a
              receipt — closing it never cancels a rebuild.
            </div>
          </>
        )}
      </div>

      {/* ═══ ESTATE VERDICT ═══ */}
      {data && brands.length > 0 && (
        <div style={panelStyle}>
          <SectionHeader
            title="Estate verdict — active openers & clickers"
            icon={faGaugeHigh}
            right={
              <span style={{ display: 'inline-flex', gap: 4 }}>
                {WINDOWS.map(d => (
                  <button
                    key={d}
                    onClick={() => setEstateWin(d)}
                    style={{
                      background: estateWin === d ? alpha(colors.indigo500, '44') : alpha(colors.indigo500, '14'),
                      border: `1px solid ${alpha(colors.indigo500, estateWin === d ? '66' : '33')}`,
                      color: estateWin === d ? colors.indigo200 : colors.textMuted,
                      borderRadius: 6, padding: '3px 10px', cursor: 'pointer', fontSize: 11.5, fontWeight: 700,
                    }}
                  >
                    {d}D
                  </button>
                ))}
              </span>
            }
          />

          <div style={{ display: 'flex', gap: 30, flexWrap: 'wrap', marginBottom: 10 }}>
            <Stat
              label={`Active openers — ${estateWin}D`}
              /* No contributing cell means NO MEASUREMENT, which is not zero. */
              value={estate.oCells === 0 ? '—' : estate.openers.toLocaleString()}
              color={estate.oCells === 0 ? colors.textMuted : colors.text}
              sub={estate.oCells === 0
                ? `no verified ${estateWin}D opener build in the estate — not a zero`
                : `SUM over ${estate.oCells} of ${estate.domains} domain segments*`}
            />
            <Stat
              label={`Active clickers — ${estateWin}D`}
              value={estate.cCells === 0 ? '—' : estate.clickers.toLocaleString()}
              color={estate.cCells === 0 ? colors.textMuted : colors.text}
              sub={estate.cCells === 0
                ? `no verified ${estateWin}D clicker build in the estate — not a zero`
                : `SUM over ${estate.cCells} of ${estate.domains} domain segments*`}
            />
            <Stat
              label="Domains"
              value={estate.domains}
              color={colors.textMuted}
              sub="sending domains with grid segments"
            />
            <Stat
              label="Cells with no measurement"
              value={estate.noMeasure}
              color={estate.noMeasure > 0 ? colors.idle : colors.textMuted}
              sub={`${estateWin}D cells with no verified build — contribute nothing, NOT zero`}
            />
            <Stat
              label="Silent-zero artifacts"
              value={estate.artifacts}
              color={estate.artifacts > 0 ? colors.warning : colors.textMuted}
              sub="counted as 0 in the sums above — the true total is higher"
            />
          </div>

          <div style={{
            fontSize: 11, color: colors.warningText, lineHeight: 1.6, marginBottom: 12,
            background: alpha(colors.warning, '0d'), border: `1px solid ${alpha(colors.warning, '33')}`,
            borderRadius: 8, padding: '8px 12px',
          }}>
            <strong>* These are SUMS of per-segment counts, not deduplicated uniques.</strong> A subscriber who
            engaged on two sending domains is counted twice, so the true estate-unique is LOWER than the number
            shown. Deduplication cannot be computed in the browser from per-segment counts — it needs a
            server-side DISTINCT over segment membership.
            <br />
            <strong>No trend is drawn.</strong> <code>/api/mailing/segments/freshness</code> returns only the
            CURRENT build ledger — it carries no historical member counts, so any trend line here would be
            fabricated. A trend needs a server-side member-count history source.
          </div>

          {/* ── Per-domain health strip: worst freshness per domain ── */}
          <div style={{ fontSize: 11, color: colors.textMuted, marginBottom: 6, letterSpacing: 0.5 }}>
            ESTATE STALENESS — one dot per sending domain, colored by its WORST cell. Click to open that domain.
          </div>
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
            {healthStrip.map(h => (
              <button
                key={h.brand}
                onClick={() => setBrandChoice(h.brand)}
                title={`${brandLabel(h.brand)}\nworst cell: ${h.worst.toUpperCase()}\n${h.cells} of 8 cells exist${h.missing > 0 ? ` · ${h.missing} MISSING (absence, not zero)` : ''}`}
                style={{
                  display: 'inline-flex', alignItems: 'center', gap: 6,
                  background: h.brand === selected ? alpha(colors.indigo500, '33') : alpha(freshnessColor(h.worst), '14'),
                  border: `1px solid ${alpha(h.brand === selected ? colors.indigo500 : freshnessColor(h.worst), '66')}`,
                  color: h.brand === selected ? colors.indigo200 : colors.textMuted,
                  borderRadius: 999, padding: '2px 9px', fontSize: 10.5, fontWeight: 700,
                  cursor: 'pointer', fontFamily: 'monospace',
                }}
              >
                <span style={{
                  width: 7, height: 7, borderRadius: 999, background: freshnessColor(h.worst),
                  display: 'inline-block',
                }} />
                {h.brand}
                {h.missing > 0 && <span style={{ color: colors.warning }}>·{h.cells}/8</span>}
              </button>
            ))}
          </div>
        </div>
      )}
    </div>
  );
};

export default SegmentFreshnessBoard;
