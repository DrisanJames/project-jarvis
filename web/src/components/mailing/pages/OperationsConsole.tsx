import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import type { IconDefinition } from '@fortawesome/fontawesome-svg-core';
import {
  faHeartPulse, faLayerGroup, faClipboardCheck, faSeedling, faClock,
  faSpinner, faRotate, faChevronDown, faChevronRight, faCircleCheck, faTriangleExclamation,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import {
  colors, alpha, stateColor, pageStyle, panelStyle, panelTitleStyle, tableStyle,
  thStyle, tdStyle, numTd, numTh, btnStyle,
} from '../shared/theme';
import { Panel, SectionHeader, Stat, Pill, SectionError, EmptyState } from '../shared/ui';

// =============================================================================
// OPERATIONS CONSOLE — Platform Coalition WS3 (REQ-C20)
// =============================================================================
// Answers one question in five seconds: does anything need the operator?
// Order on screen: verdict + count tiles → "Needs attention" (one sentence per
// problem) → collapsible groups (scheduled jobs, background workers, segment
// freshness, send-day checks, engager growth). Healthy items are collapsed and
// every full table stays one click away.
//
// Sources (contract: tasks/eng-team/coalition/SCHEMA-CONTRACTS.md):
//   - GET /api/worker-health                       (mailing_worker_heartbeats)
//   - GET /api/mailing/segment-registry/freshness
//   - GET /api/mailing/ops/job-runs?worker=job:send_day_invariants | job:cohort_growth
//
// All four panels fetch here at page level so the verdict can see them. Each
// keeps its own loading, error, not-on-this-build and empty display. The
// verdict never says healthy unless every source it depends on was read.
//
// Heartbeats: one scheduled job writes up to two rows, `job:<name>` from the job
// and `job:cron:agents.<pkg>.<name>` from its runner. Cards and the attention
// list merge those into one job; the full table keeps every raw row.
// `last_error` is not always an error: workers also use it for status notes
// ("replayed 0, failed 0"), so it only reads as an error when the row failed.
// `running` and `disabled` are normal statuses, not failures.
//
// FilterBar note (PORTAL_DESIGN_SYSTEM §3): this screen exposes the EMPTY
// subset of the canonical filter vocabulary. Every panel is current platform
// state, so no date range, brand, ISP or transport binds to any of it.

// PAGE_VERSION 1.0 — initial Operations console (Coalition WS3, 2026-07-23).
// PAGE_VERSION 2.0 — verdict + needs-attention redesign; Background Workers moved here from the Delivery screen (2026-11-02).
const PAGE_VERSION = '2.0';

const POLL_MS = 60_000;
const FETCH_TIMEOUT_MS = 30_000;
const DENVER = 'America/Denver';
// A job silent this long is listed as stopped, not as a live problem. It is
// still counted on its own tile and in the verdict's second line.
const STOPPED_AFTER_SECONDS = 14 * 86_400;
const STOPPED_AFTER_LABEL = '14 days';
// Rows of one job within this gap of its newest beat belong to the same run.
const SAME_RUN_SECONDS = 900;
const ATTENTION_PREVIEW = 8;

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
  /** A background refresh is in flight; last result stays on screen. */
  refreshing: boolean;
  error: string | null;
  /** true when the server build lacks the endpoint (404). */
  unavailable: boolean;
  data: T | null;
  fetchedAt: string | null;
  ms: number | null;
}

const initialPanel = <T,>(): PanelState<T> => ({
  loading: true, refreshing: false, error: null, unavailable: false, data: null, fetchedAt: null, ms: null,
});

const denverClock = (): string =>
  new Date().toLocaleTimeString('en-US', { timeZone: DENVER, hour: 'numeric', minute: '2-digit', second: '2-digit' });

function usePanelFetch<T>(url: string, nonce: number): [PanelState<T>, () => void] {
  const [state, setState] = useState<PanelState<T>>(initialPanel<T>());
  const [localNonce, setLocalNonce] = useState(0);
  const retry = useCallback(() => setLocalNonce(n => n + 1), []);
  const abortRef = useRef<AbortController | null>(null);

  useEffect(() => {
    abortRef.current?.abort();
    const ac = new AbortController();
    abortRef.current = ac;
    // Spinner only on first paint; later refreshes keep the last result mounted.
    setState(s => ({ ...s, loading: s.data == null && s.error == null && !s.unavailable, refreshing: true }));
    const t0 = performance.now();
    let timedOut = false;
    const timer = setTimeout(() => { timedOut = true; ac.abort(); }, FETCH_TIMEOUT_MS);
    const done = { loading: false, refreshing: false };
    apiFetch(url, { signal: ac.signal })
      .then(async res => {
        const ms = Math.round(performance.now() - t0);
        const fetchedAt = denverClock();
        if (res.status === 404) {
          // Route absent on this server build, said out loud.
          setState({ ...done, error: null, unavailable: true, data: null, fetchedAt, ms });
          return;
        }
        if (!res.ok) {
          let msg = `HTTP ${res.status}`;
          try {
            const body = await res.json();
            if (body?.error) msg += `: ${body.error}`;
          } catch { /* non-JSON error body */ }
          setState({ ...done, error: msg, unavailable: false, data: null, fetchedAt, ms });
          return;
        }
        const data = (await res.json()) as T;
        setState({ ...done, error: null, unavailable: false, data, fetchedAt, ms });
      })
      .catch((e: unknown) => {
        if (ac.signal.aborted && !timedOut) return;
        setState({
          ...done, unavailable: false, data: null, fetchedAt: denverClock(), ms: null,
          error: timedOut ? `No response after ${FETCH_TIMEOUT_MS / 1000} seconds` : e instanceof Error ? e.message : String(e),
        });
      })
      .finally(() => clearTimeout(timer));
    return () => { clearTimeout(timer); ac.abort(); };
  }, [url, nonce, localNonce]);

  return [state, retry];
}

// ── Small display helpers ───────────────────────────────────────────────────

const fmtAge = (seconds: number | null | undefined): string => {
  if (seconds == null || !isFinite(seconds)) return 'unknown';
  const s = Math.max(0, seconds);
  if (s < 90) return `${Math.round(s)} sec`;
  if (s < 5400) return `${Math.round(s / 60)} min`;
  if (s < 172800) return `${(s / 3600).toFixed(1)} hours`;
  return `${(s / 86400).toFixed(1)} days`;
};

const ageSeconds = (iso: string | null): number | null => {
  if (!iso) return null;
  const t = Date.parse(iso);
  return isNaN(t) ? null : (Date.now() - t) / 1000;
};

const fmtCadence = (seconds: number): string => {
  if (seconds <= 0) return 'unknown';
  const unit = (n: number, name: string) => `${n} ${name}${n === 1 ? '' : 's'}`;
  if (seconds % 86400 === 0) return unit(seconds / 86400, 'day');
  if (seconds % 3600 === 0) return unit(seconds / 3600, 'hour');
  if (seconds % 60 === 0) return `${seconds / 60} min`;
  return `${seconds} sec`;
};

const fmtDenver = (iso: string | null | undefined): string => {
  if (!iso) return 'never';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  return d.toLocaleString('en-US', {
    timeZone: DENVER, month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit', timeZoneName: 'short',
  });
};

const fmtInt = (n: number): string => n.toLocaleString();

const plural = (n: number, noun: string): string => `${fmtInt(n)} ${noun}${n === 1 ? '' : 's'}`;

const fmtDelta = (cur: number, prev: number | null): { text: string; color: string } => {
  if (prev == null || prev === 0) return { text: 'no prior snapshot', color: colors.textMuted };
  const d = cur - prev;
  const pct = (d / prev) * 100;
  const sign = d > 0 ? '+' : '';
  const color = d > 0 ? colors.success : d < 0 ? colors.danger : colors.textMuted;
  return { text: `${sign}${fmtInt(d)} (${sign}${pct.toFixed(1)}%)`, color };
};

const fetchNote = (p: PanelState<unknown>): string =>
  p.fetchedAt ? `fetched ${p.fetchedAt} Denver${p.ms != null ? ` · ${p.ms} ms` : ''}` : '';

const LoadingRow: React.FC<{ label: string }> = ({ label }) => (
  <div style={{ display: 'flex', alignItems: 'center', gap: 10, color: colors.textMuted, fontSize: 13, padding: '18px 4px' }}>
    <FontAwesomeIcon icon={faSpinner} spin /> {label}
  </div>
);

// An unavailable source must SAY it is unavailable (metric-contract rule).
const UnavailableNote: React.FC<{ endpoint: string; detail: string }> = ({ endpoint, detail }) => (
  <div style={{
    padding: '14px 16px', borderRadius: 8, fontSize: 12.5, lineHeight: 1.6, overflowWrap: 'anywhere',
    background: alpha(colors.warning, '14'), border: `1px solid ${alpha(colors.warning, '44')}`,
    color: colors.warning,
  }}>
    <strong>Source unavailable.</strong> <code style={{ fontFamily: 'monospace' }}>{endpoint}</code> is
    not exposed by this server build. {detail}
  </div>
);

const Unknown: React.FC = () => (
  <span style={{ color: colors.textMuted, fontStyle: 'italic', fontWeight: 500, fontSize: 15 }}>unknown</span>
);

// Scrolls inside itself so a wide table never scrolls the page.
const ScrollTable: React.FC<{ label: string; children: React.ReactNode }> = ({ label, children }) => (
  <div style={{ overflowX: 'auto' }} tabIndex={0} role="region" aria-label={label}>{children}</div>
);

const noteStyle: React.CSSProperties = { fontSize: 11, color: colors.textMuted, marginTop: 8, lineHeight: 1.5 };
const clipCell: React.CSSProperties = { ...tdStyle, maxWidth: 260, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' };

// Text link style row that opens a nested block.
const Disclosure: React.FC<{ label: React.ReactNode; defaultOpen?: boolean; children: React.ReactNode }> = ({ label, defaultOpen, children }) => {
  const [open, setOpen] = useState(!!defaultOpen);
  return (
    <div style={{ marginTop: 10 }}>
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen(v => !v)}
        style={{
          background: 'none', border: 'none', color: colors.indigo400, cursor: 'pointer',
          fontSize: 12, padding: '4px 0', display: 'flex', alignItems: 'center', gap: 6, textAlign: 'left',
        }}>
        <FontAwesomeIcon icon={open ? faChevronDown : faChevronRight} />
        {label}
      </button>
      {open && <div style={{ marginTop: 6 }}>{children}</div>}
    </div>
  );
};

// A status card whose body is hidden until asked for. `defaultOpen` follows the
// data until the operator clicks, so a problem arriving later opens the card.
const GroupCard: React.FC<{
  title: string;
  icon: IconDefinition;
  summary: React.ReactNode;
  badge?: React.ReactNode;
  defaultOpen: boolean;
  note?: string;
  children: React.ReactNode;
}> = ({ title, icon, summary, badge, defaultOpen, note, children }) => {
  const [choice, setChoice] = useState<boolean | null>(null);
  const open = choice ?? defaultOpen;
  return (
    <section style={panelStyle}>
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setChoice(!open)}
        style={{
          display: 'block', width: '100%', background: 'none', border: 'none', padding: 0,
          cursor: 'pointer', textAlign: 'left', color: colors.text,
        }}>
        <span style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 10, flexWrap: 'wrap' }}>
          <span style={panelTitleStyle}>
            <FontAwesomeIcon icon={open ? faChevronDown : faChevronRight} style={{ color: colors.indigo400, fontSize: 11 }} />
            <FontAwesomeIcon icon={icon} style={{ color: colors.indigo400 }} />
            {title}
          </span>
          {badge}
        </span>
        <span style={{ display: 'block', fontSize: 12, color: colors.textMuted, marginTop: 6, lineHeight: 1.5 }}>{summary}</span>
      </button>
      {open && (
        <div style={{ marginTop: 10 }}>
          {children}
          {note && <div style={noteStyle}>{note}</div>}
        </div>
      )}
    </section>
  );
};

// ── Names ───────────────────────────────────────────────────────────────────

// Scheduled jobs beat under `job:<name>` and `job:cron:agents.<pkg>.<name>`.
const jobKey = (name: string): string =>
  name.replace(/^job:cron:agents\.(?:jobs|reporting|journeys)\./, '').replace(/^job:cron:agents\./, '').replace(/^job:/, '');

const NAME_OVERRIDES: Record<string, string> = {
  acq_report: 'Acquisition report',
  brain_eval: 'Brain checks',
  bot_ip_nominator: 'Bot IP nominator',
  bot_session_detect: 'Bot session detection',
  cohort_growth: 'Cohort growth snapshot',
  eo_clean_jobs: 'Email validation cleaning',
  fbl_mailbox_bridge: 'Complaint mailbox bridge',
  ip_capacity_preflight: 'IP capacity check',
  'inboxscore.seed_reader': 'Inbox score seed reader',
  'inboxscore.train': 'Inbox score training',
  nightly_cohort_remint: 'Nightly cohort rebuild',
  nl_refresh: 'Newsletter article refresh',
  nx_eo_supply: 'NX email validation supply',
  ops_readiness: 'Sending readiness check',
  pg_to_lake: 'Postgres to analytics lake sync',
  predispatch_refresh: 'Pre-send audience refresh',
  'scheduling.kumo_warm': 'Kumo warm-up build',
  send_day_invariants: 'Send-day checks',
  ses_dlq_drain: 'SES failed event replay',
  ses_lake_pg_ingest: 'SES events to Postgres',
  ses_pixel_canary: 'SES open pixel test',
  ses_vdm_snapshot: 'SES deliverability snapshot',
  'seslane.shadow': 'SES lane plan (not live)',
  staleness_alarm: 'Data freshness alarm',
  stats: 'Statistics run',
  vdm_reconcile: 'SES deliverability reconcile',
};

const NAME_TOKENS: Record<string, string> = {
  ab: 'A/B', eo: 'email validation', fbl: 'complaint feed', gmail: 'Gmail', inboxscore: 'inbox score', ip: 'IP',
  isp: 'mailbox provider', kumo: 'Kumo', mk: 'MK', msft: 'Microsoft', nl: 'newsletter', nx: 'NX', pg: 'Postgres',
  pmta: 'PMTA', seslane: 'SES lane', ses: 'SES', vdm: 'SES deliverability', waf: 'WAF', wcl: 'WCL', yahoo: 'Yahoo',
};

const sentenceCase = (s: string): string => (s ? s.charAt(0).toUpperCase() + s.slice(1) : s);

const humanName = (key: string): string =>
  NAME_OVERRIDES[key] ?? sentenceCase(key.split(/[._-]+/).filter(Boolean).map(t => NAME_TOKENS[t] ?? t).join(' '));

// "I8 COHORT-REFRESH-RAN" reads as "Cohort refresh ran".
const invariantName = (raw: string): string => sentenceCase(raw.replace(/^I\d+\s+/, '').replace(/-/g, ' ').toLowerCase());

// The cron runner reports "rc=2 after 3 attempt(s): no stderr". Keep the useful part.
const plainError = (raw: string): string => {
  const m = raw.match(/^rc=(\d+) after (\d+) attempt\(s\):\s*(.*)$/s);
  if (!m) return raw;
  if (m[3] === 'no stderr') return `It exited with code ${m[1]} after ${m[2]} tries and logged no detail.`;
  return m[3];
};

// ── Jobs and workers ────────────────────────────────────────────────────────

type Health = 'not_responding' | 'failing' | 'late' | 'stopped' | 'running' | 'ok' | 'off';

const HEALTH_META: Record<Health, { label: string; color: string; rank: number }> = {
  not_responding: { label: 'Not responding', color: colors.danger, rank: 5 },
  failing: { label: 'Failing', color: colors.danger, rank: 4 },
  late: { label: 'Late', color: colors.warning, rank: 3 },
  stopped: { label: 'Stopped', color: colors.idle, rank: 2 },
  off: { label: 'Turned off', color: colors.idle, rank: 1 },
  running: { label: 'Running', color: colors.info, rank: 0 },
  ok: { label: 'OK', color: colors.success, rank: 0 },
};

const rowHealth = (w: WorkerHealthRow): Health => {
  if (w.last_status === 'disabled') return 'off';
  if (w.seconds_since_beat > STOPPED_AFTER_SECONDS) return 'stopped';
  if (w.stalled) return 'not_responding';
  if (w.last_status && w.last_status !== 'ok' && w.last_status !== 'running') return 'failing';
  const threshold = Math.max(w.expected_interval_seconds * 2, 300); // mirrors segmentWorkerOnline
  if (w.expected_interval_seconds > 0 && w.seconds_since_beat > threshold) return 'late';
  return w.last_status === 'running' ? 'running' : 'ok';
};

interface JobView {
  key: string;
  label: string;
  isJob: boolean;
  rep: WorkerHealthRow; // the row whose status this job displays
  rows: WorkerHealthRow[];
  health: Health;
  error: string; // empty unless the current run failed
  note: string; // status note on a healthy run
}

const rawNames = (j: JobView): string => j.rows.map(r => r.worker_name).join(', ');

const buildJobs = (workers: WorkerHealthRow[]): JobView[] => {
  const groups = new Map<string, WorkerHealthRow[]>();
  workers.forEach(w => {
    const key = w.worker_name.startsWith('job:') ? `job:${jobKey(w.worker_name)}` : w.worker_name;
    groups.set(key, [...(groups.get(key) ?? []), w]);
  });
  return Array.from(groups.entries()).map(([key, rows]) => {
    // An older row from a job's previous wiring must not outvote its current run.
    const newest = Math.min(...rows.map(r => r.seconds_since_beat));
    const current = rows.filter(r => r.seconds_since_beat - newest <= SAME_RUN_SECONDS);
    const rep = current.reduce((a, b) => (HEALTH_META[rowHealth(b)].rank > HEALTH_META[rowHealth(a)].rank ? b : a));
    const health = rowHealth(rep);
    const failed = current.filter(r => r.last_error && r.last_status !== 'ok' && r.last_status !== 'disabled');
    const specific = failed.find(r => !r.last_error?.startsWith('rc=')) ?? failed[0];
    return {
      key: key.replace(/^job:/, ''),
      label: humanName(key.replace(/^job:/, '')),
      isJob: key.startsWith('job:'),
      rep,
      rows,
      health,
      error: specific?.last_error ?? '',
      note: failed.length === 0 ? (current.find(r => r.last_error)?.last_error ?? '') : '',
    };
  }).sort((a, b) => a.label.localeCompare(b.label));
};

const summarizeJobs = (jobs: JobView[]): string => {
  const n = (h: Health[]) => jobs.filter(j => h.includes(j.health)).length;
  const parts = [`${fmtInt(n(['ok', 'running']))} of ${fmtInt(jobs.length)} healthy`];
  const add = (count: number, label: string) => { if (count > 0) parts.push(`${fmtInt(count)} ${label}`); };
  add(n(['failing']), 'failing');
  add(n(['not_responding']), 'not responding');
  add(n(['late']), 'late');
  add(n(['stopped']), 'stopped');
  add(n(['off']), 'turned off');
  return parts.join(' · ');
};

// ── Attention items ─────────────────────────────────────────────────────────

type AttentionKind = 'not_responding' | 'failing' | 'late';

interface AttentionItem {
  id: string;
  group: 'jobs' | 'workers' | 'families' | 'invariants';
  kind: AttentionKind;
  sentence: string;
  detail?: string;
  /** Raw names + Denver time, for the tooltip. */
  title: string;
  age: number;
  order: number; // lower sorts first
}

const jobItem = (j: JobView): AttentionItem | null => {
  if (j.health !== 'failing' && j.health !== 'not_responding' && j.health !== 'late') return null;
  const age = fmtAge(j.rep.seconds_since_beat);
  const every = j.rep.expected_interval_seconds > 0 ? `it should run every ${fmtCadence(j.rep.expected_interval_seconds)}` : '';
  const sentence =
    j.health === 'failing' ? `${j.label} failed ${age} ago.`
    : j.health === 'not_responding' ? `${j.label} has not checked in for ${age}${every ? `; ${every}` : ''}.`
    : `${j.label} is late: last ran ${age} ago${every ? `; ${every}` : ''}.`;
  return {
    id: `job-${j.key}`,
    group: j.isJob ? 'jobs' : 'workers',
    kind: j.health,
    sentence,
    detail: j.error ? `Last error: ${plainError(j.error)}` : undefined,
    title: `${rawNames(j)} · last beat ${fmtDenver(j.rep.last_beat_at)}${j.error ? ` · ${j.error}` : ''}`,
    age: j.rep.seconds_since_beat,
    order: j.health === 'not_responding' ? 1 : j.health === 'failing' ? 2 : 3,
  };
};

// ── Segment families ────────────────────────────────────────────────────────

interface FamilyDisplayState { label: string; color: string }

const familyState = (f: FreshnessRow): FamilyDisplayState => {
  if (!f.active) return { label: 'Disabled', color: colors.idle };
  if (f.owner.toUpperCase().startsWith('RETIRED')) return { label: 'Retired', color: colors.idle };
  if (f.newest_refresh_at == null) return { label: 'Never built', color: f.sla_hours > 0 ? colors.danger : colors.idle };
  if (f.sla_state === 'breached') return { label: 'Out of date', color: colors.warning };
  if (f.sla_state === 'no_sla') return { label: 'No time limit', color: colors.textMuted };
  return { label: 'Fresh', color: colors.success };
};

const familyName = (f: FreshnessRow): string => humanName(f.family_key);

const familyItem = (f: FreshnessRow): AttentionItem | null => {
  const st = familyState(f).label;
  const base = { group: 'families' as const, title: `${f.family_pattern} · owner: ${f.owner} · newest refresh ${fmtDenver(f.newest_refresh_at)}` };
  if (st === 'Never built' && f.sla_hours > 0) {
    return {
      ...base, id: `family-${f.id}`, kind: 'failing', age: Number.MAX_SAFE_INTEGER, order: 2,
      sentence: f.segments_count === 0
        ? `${familyName(f)} segments do not exist yet; they should refresh every ${f.sla_hours} hours.`
        : `${familyName(f)} segments have never been built; they should refresh every ${f.sla_hours} hours.`,
    };
  }
  if (st === 'Out of date') {
    const age = ageSeconds(f.newest_refresh_at) ?? 0;
    return {
      ...base, id: `family-${f.id}`, kind: 'late', age, order: 3,
      sentence: `${familyName(f)} segments were last refreshed ${fmtAge(age)} ago; the limit is ${f.sla_hours} hours.`,
    };
  }
  return null;
};

// ── Send-day checks ─────────────────────────────────────────────────────────

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
  if (u === 'FAIL') return colors.dangerFill;
  return colors.idle; // SKIP
};

interface InvariantRun { run: JobRun; det: InvariantRunDetail }

const invariantRuns = (data: JobRunsResponse | null): InvariantRun[] =>
  (data?.runs ?? [])
    .map(r => ({ run: r, det: parseDetail<InvariantRunDetail>(r.detail) }))
    .filter((x): x is InvariantRun => x.det != null && x.det.results != null);

const invariantItems = (latest: InvariantRun | undefined): AttentionItem[] => {
  if (!latest) return [];
  const age = ageSeconds(latest.run.started_at) ?? 0;
  const base = { group: 'invariants' as const, kind: 'failing' as const, age, order: 0 };
  const failed = Object.entries(latest.det.results).filter(([, r]) => r.status.toUpperCase() === 'FAIL');
  if (failed.length === 0 && latest.det.verdict !== 'GREEN') {
    return [{
      ...base, id: 'inv-verdict', title: `run ${fmtDenver(latest.run.started_at)}`,
      sentence: `The send-day checks for ${latest.det.date} came back ${latest.det.verdict} without naming a failed check.`,
    }];
  }
  return failed.map(([name, r]) => ({
    ...base,
    id: `inv-${name}`,
    sentence: `Send-day check "${invariantName(name)}" failed for ${latest.det.date}, ${fmtAge(age)} ago.`,
    detail: r.detail,
    title: `${name} · run ${fmtDenver(latest.run.started_at)}`,
  }));
};

const runsEmptyHint = (what: string): string =>
  `No ${what} are on record. The job has not written its results to the platform, so this is "never recorded", not a healthy empty.`;

// ── Cohort growth ───────────────────────────────────────────────────────────

const cohortAgg = (s: CohortSnapshotDetail) => {
  let clk = 0, opn = 0;
  Object.values(s.snapshot).forEach(slots => {
    clk += slots[CLK] ?? 0;
    opn += slots[OPN] ?? 0;
  });
  return { clk, opn, tot: clk + opn };
};

const cohortSnaps = (data: JobRunsResponse | null): CohortSnapshotDetail[] =>
  (data?.runs ?? [])
    .map(r => parseDetail<CohortSnapshotDetail>(r.detail))
    .filter((d): d is CohortSnapshotDetail => d != null && typeof d.snapshot === 'object' && d.snapshot != null)
    .sort((a, b) => a.date.localeCompare(b.date)); // oldest → newest

// ── Shared pieces ───────────────────────────────────────────────────────────

const AttentionRow: React.FC<{ item: AttentionItem }> = ({ item }) => {
  const meta = HEALTH_META[item.kind];
  return (
    <li style={{ display: 'flex', gap: 10, padding: '9px 0', borderTop: `1px solid ${colors.divider}`, minWidth: 0 }} title={item.title}>
      <span style={{ width: 9, height: 9, borderRadius: 999, flexShrink: 0, marginTop: 5, background: item.kind === 'late' ? colors.warningFill : colors.dangerFill }} />
      <div style={{ minWidth: 0, flex: 1 }}>
        <div style={{ fontSize: 13, color: colors.text, lineHeight: 1.45, overflowWrap: 'anywhere' }}>
          {item.sentence} <span style={{ fontSize: 11, color: meta.color, fontWeight: 600 }}>{meta.label}</span>
        </div>
        {item.detail && (
          <div style={{
            fontSize: 12, color: colors.textMuted, marginTop: 2, lineHeight: 1.45, overflowWrap: 'anywhere',
            display: '-webkit-box', WebkitLineClamp: 2, WebkitBoxOrient: 'vertical', overflow: 'hidden',
          }}>
            {item.detail}
          </div>
        )}
      </div>
    </li>
  );
};

const AttentionList: React.FC<{ items: AttentionItem[] }> = ({ items }) => (
  <ul style={{ listStyle: 'none', margin: 0, padding: 0 }}>
    {items.map(i => <AttentionRow key={i.id} item={i} />)}
  </ul>
);

// One small card per item, same content the Delivery screen's worker cards had.
const CompactGrid: React.FC<{ children: React.ReactNode }> = ({ children }) => (
  <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(min(100%, 220px), 1fr))', gap: 8 }}>{children}</div>
);

const CompactCard: React.FC<{ dot: string; name: string; title: string; line1: string; line2?: string }> = ({ dot, name, title, line1, line2 }) => (
  <div style={{ padding: '8px 10px', borderRadius: 8, background: alpha(colors.indigo500, '0d'), border: `1px solid ${colors.divider}`, minWidth: 0 }} title={title}>
    <div style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
      <span style={{ width: 8, height: 8, borderRadius: 999, background: dot, flexShrink: 0 }} />
      <span style={{ fontSize: 12, fontWeight: 600, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{name}</span>
    </div>
    <div style={{ fontSize: 12, color: colors.text, marginTop: 4, fontVariantNumeric: 'tabular-nums' }}>{line1}</div>
    {line2 && <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{line2}</div>}
  </div>
);

const JobCards: React.FC<{ jobs: JobView[] }> = ({ jobs }) => (
  <CompactGrid>
    {jobs.map(j => (
      <CompactCard
        key={j.key}
        dot={HEALTH_META[j.health].color}
        name={j.label}
        title={`${rawNames(j)} · last beat ${fmtDenver(j.rep.last_beat_at)}${j.error || j.note ? ` · ${j.error || j.note}` : ''}`}
        line1={`${j.health === 'running' ? 'Running, started' : 'Last ran'} ${fmtAge(j.rep.seconds_since_beat)} ago`}
        line2={j.health === 'off' ? (j.note || 'Turned off') : j.rep.expected_interval_seconds > 0 ? `expected every ${fmtCadence(j.rep.expected_interval_seconds)}` : undefined}
      />
    ))}
  </CompactGrid>
);

// Status badge for a card header, one word per state so a collapsed card is honest.
const SourceBadge: React.FC<{ panel: PanelState<unknown>; problems: number; empty?: boolean }> = ({ panel, problems, empty }) => {
  if (panel.loading) return <Pill color={colors.idle}>Checking</Pill>;
  if (panel.unavailable) return <Pill color={colors.warning}>Unavailable</Pill>;
  if (panel.error) return <Pill color={colors.danger}>Could not load</Pill>;
  if (problems > 0) return <Pill color={colors.danger}>{plural(problems, 'problem')}</Pill>;
  if (empty) return <Pill color={colors.warning}>No data</Pill>;
  return <Pill color={colors.success}>Healthy</Pill>;
};

// ── Group: JOBS / WORKERS ───────────────────────────────────────────────────

const WorkerTable: React.FC<{ rows: WorkerHealthRow[]; ownerFor: (name: string) => string; label: string }> = ({ rows, ownerFor, label }) => (
  <ScrollTable label={label}>
    <table style={tableStyle}>
      <thead>
        <tr>
          <th style={thStyle}>Name</th>
          <th style={thStyle}>Owner</th>
          <th style={numTh}>Expected every</th>
          <th style={numTh}>Last beat</th>
          <th style={numTh}>Cycles</th>
          <th style={thStyle}>Health</th>
          <th style={thStyle}>Last message</th>
        </tr>
      </thead>
      <tbody>
        {rows.map(w => {
          const h = rowHealth(w);
          const isError = !!w.last_error && w.last_status !== 'ok' && w.last_status !== 'disabled';
          return (
            <tr key={w.worker_name}>
              <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>
                {humanName(jobKey(w.worker_name))}
                <div style={{ fontFamily: 'monospace', fontSize: 10.5, color: colors.textMuted }}>{w.worker_name}</div>
              </td>
              <td style={clipCell} title={ownerFor(w.worker_name)}>{ownerFor(w.worker_name)}</td>
              <td style={numTd} title={`${w.expected_interval_seconds} seconds`}>{fmtCadence(w.expected_interval_seconds)}</td>
              <td style={{ ...numTd, whiteSpace: 'nowrap' }} title={fmtDenver(w.last_beat_at)}>{fmtAge(w.seconds_since_beat)} ago</td>
              <td style={numTd}>{fmtInt(w.cycle_count)}</td>
              <td style={tdStyle}>
                <Pill color={HEALTH_META[h].color} style={{ whiteSpace: 'nowrap' }}>{HEALTH_META[h].label}</Pill>
              </td>
              <td style={{ ...clipCell, maxWidth: 280, color: isError ? colors.danger : colors.textMuted }} title={w.last_error || ''}>
                {w.last_error || 'none'}
              </td>
            </tr>
          );
        })}
      </tbody>
    </table>
  </ScrollTable>
);

const HeartbeatGroup: React.FC<{
  title: string;
  icon: IconDefinition;
  noun: string;
  panel: PanelState<WorkerHealthResponse>;
  retry: () => void;
  jobs: JobView[];
  rows: WorkerHealthRow[];
  items: AttentionItem[];
  ownerFor: (name: string) => string;
}> = ({ title, icon, noun, panel, retry, jobs, rows, items, ownerFor }) => {
  const ready = !panel.loading && !panel.error && !panel.unavailable && panel.data != null;
  const healthy = jobs.filter(j => j.health === 'ok' || j.health === 'running');
  const stopped = jobs.filter(j => j.health === 'stopped');
  const off = jobs.filter(j => j.health === 'off');
  const summary = panel.loading ? 'Loading heartbeats.'
    : !ready ? 'Heartbeats could not be read.'
    : jobs.length === 0 ? `No ${noun}s reported a heartbeat.`
    : summarizeJobs(jobs);
  return (
    <GroupCard
      title={title}
      icon={icon}
      summary={summary}
      badge={<SourceBadge panel={panel} problems={items.length} empty={jobs.length === 0} />}
      defaultOpen={!ready || jobs.length === 0 || items.length > 0}
    >
      {panel.loading && <LoadingRow label="Loading worker heartbeats…" />}
      {!panel.loading && panel.unavailable && (
        <UnavailableNote endpoint="/api/worker-health" detail="Heartbeat health cannot be shown." />
      )}
      {!panel.loading && panel.error && (
        <SectionError label="Worker heartbeats" error={panel.error} onRetry={retry} />
      )}
      {ready && (jobs.length === 0 ? (
        <EmptyState
          title="No heartbeats returned"
          hint="Either nothing has ever reported on this environment, or the heartbeat query failed: this endpoint returns an empty list in both cases."
        />
      ) : (
        <>
          {items.length > 0 && <AttentionList items={items} />}
          {stopped.length > 0 && (
            <Disclosure label={`Stopped: ${plural(stopped.length, noun)} silent more than ${STOPPED_AFTER_LABEL}`}>
              <JobCards jobs={stopped} />
            </Disclosure>
          )}
          {off.length > 0 && (
            <Disclosure label={`Turned off (${fmtInt(off.length)})`}><JobCards jobs={off} /></Disclosure>
          )}
          {healthy.length > 0 && (
            <Disclosure label={`Healthy (${fmtInt(healthy.length)})`}><JobCards jobs={healthy} /></Disclosure>
          )}
          <Disclosure label={`Full table (${plural(rows.length, 'heartbeat')})`}>
            <WorkerTable rows={rows} ownerFor={ownerFor} label={`${title} full table`} />
            <div style={noteStyle}>{fetchNote(panel)}. One row per heartbeat, so a scheduled job can appear twice: once from the job, once from its runner.</div>
          </Disclosure>
        </>
      ))}
    </GroupCard>
  );
};

// ── Group: SEGMENT FRESHNESS ────────────────────────────────────────────────

const FamilyCards: React.FC<{ families: FreshnessRow[] }> = ({ families }) => (
  <CompactGrid>
    {families.map(f => {
      const st = familyState(f);
      // One family key can hold several patterns; the pattern tells them apart.
      const shared = families.filter(x => x.family_key === f.family_key).length > 1;
      return (
        <CompactCard
          key={f.id}
          dot={st.color}
          name={shared ? `${familyName(f)}: ${f.family_pattern}` : familyName(f)}
          title={`${f.family_pattern} · owner: ${f.owner} · newest refresh ${fmtDenver(f.newest_refresh_at)}`}
          line1={f.newest_refresh_at ? `Refreshed ${fmtAge(ageSeconds(f.newest_refresh_at))} ago` : 'Never built'}
          line2={`${plural(f.segments_count, 'segment')}${f.sla_hours > 0 ? ` · limit ${f.sla_hours} hours` : ''}`}
        />
      );
    })}
  </CompactGrid>
);

const FamiliesGroup: React.FC<{
  panel: PanelState<FreshnessResponse>;
  retry: () => void;
  items: AttentionItem[];
}> = ({ panel, retry, items }) => {
  const ready = !panel.loading && !panel.error && !panel.unavailable && panel.data != null;
  const families = panel.data?.families ?? [];
  const flagged = new Set(items.map(i => i.id));
  const others = families.filter(f => !flagged.has(`family-${f.id}`));
  const fresh = others.filter(f => familyState(f).label === 'Fresh');
  const closed = others.filter(f => ['Retired', 'Disabled'].includes(familyState(f).label));
  const noLimit = others.filter(f => !fresh.includes(f) && !closed.includes(f));
  const tracked = fresh.length + items.length;
  const summary = panel.loading ? 'Loading the segment registry.'
    : !ready ? 'The segment registry could not be read.'
    : families.length === 0 ? 'The registry has no segment families.'
    : `${fmtInt(fresh.length)} of ${fmtInt(tracked)} with a time limit are fresh · ${fmtInt(noLimit.length)} have no time limit · ${fmtInt(closed.length)} retired or disabled`;
  return (
    <GroupCard
      title="Segment freshness"
      icon={faLayerGroup}
      summary={summary}
      badge={<SourceBadge panel={panel} problems={items.length} empty={families.length === 0} />}
      defaultOpen={!ready || families.length === 0 || items.length > 0}
    >
      {panel.loading && <LoadingRow label="Loading segment-family registry…" />}
      {!panel.loading && panel.unavailable && (
        <UnavailableNote
          endpoint="/api/mailing/segment-registry/freshness"
          detail="Family ownership and freshness cannot be shown until this API is deployed."
        />
      )}
      {!panel.loading && panel.error && (
        <SectionError label="Segment freshness" error={panel.error} onRetry={retry} />
      )}
      {ready && (families.length === 0 ? (
        <EmptyState
          title="Registry is empty"
          hint="mailing_segment_registry has no rows for this organization. The seed migration has not run here. This is built empty, not an error."
        />
      ) : (
        <>
          {items.length > 0 && <AttentionList items={items} />}
          {fresh.length > 0 && <Disclosure label={`Fresh (${fmtInt(fresh.length)})`}><FamilyCards families={fresh} /></Disclosure>}
          {noLimit.length > 0 && <Disclosure label={`No time limit (${fmtInt(noLimit.length)})`}><FamilyCards families={noLimit} /></Disclosure>}
          {closed.length > 0 && <Disclosure label={`Retired or disabled (${fmtInt(closed.length)})`}><FamilyCards families={closed} /></Disclosure>}
          <Disclosure label={`Full table (${fmtInt(families.length)} families)`}>
            <ScrollTable label="Segment families full table">
              <table style={tableStyle}>
                <thead>
                  <tr>
                    <th style={thStyle}>Family</th>
                    <th style={thStyle}>Pattern</th>
                    <th style={thStyle}>Owner</th>
                    <th style={thStyle}>Refresh schedule</th>
                    <th style={numTh}>Segments</th>
                    <th style={numTh}>Members (estimate)*</th>
                    <th style={numTh}>Newest refresh</th>
                    <th style={numTh}>Time limit</th>
                    <th style={thStyle}>State</th>
                    <th style={thStyle}>Keep policy</th>
                  </tr>
                </thead>
                <tbody>
                  {families.map(f => {
                    const st = familyState(f);
                    return (
                      <tr key={f.id}>
                        <td style={{ ...tdStyle, fontWeight: 600, whiteSpace: 'nowrap' }} title={f.family_key}>{familyName(f)}</td>
                        <td style={{ ...tdStyle, fontFamily: 'monospace', fontSize: 11.5 }} title={f.family_pattern}>{f.family_pattern}</td>
                        <td style={{ ...clipCell, maxWidth: 240 }} title={f.owner}>{f.owner}</td>
                        <td style={tdStyle}>{f.cadence || 'not set'}</td>
                        <td style={numTd}>{fmtInt(f.segments_count)}</td>
                        <td style={numTd} title="SUM(subscriber_count) over matching non-archived segments. An estimate, not a deduped count.">
                          {fmtInt(f.members_estimate)}*
                        </td>
                        <td style={{ ...numTd, whiteSpace: 'nowrap' }} title={fmtDenver(f.newest_refresh_at)}>
                          {f.newest_refresh_at == null ? 'never' : `${fmtAge(ageSeconds(f.newest_refresh_at))} ago`}
                        </td>
                        <td style={numTd}>{f.sla_hours > 0 ? `${f.sla_hours} hours` : 'none'}</td>
                        <td style={tdStyle}><Pill color={st.color} style={{ whiteSpace: 'nowrap' }}>{st.label}</Pill></td>
                        <td style={tdStyle}>
                          <Pill color={f.keep_policy === 'purgeable' ? colors.warning : colors.indigo400}>{f.keep_policy}</Pill>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </ScrollTable>
            <div style={noteStyle}>
              * Members (estimate) = SUM(subscriber_count) across the family's segments, overlap not deduped.
              Never built = no matching segment has a last_calculated_at, which is different from a family whose segments exist but are empty.
              {' '}{fetchNote(panel)}.
            </div>
          </Disclosure>
        </>
      ))}
    </GroupCard>
  );
};

// ── Group: SEND-DAY CHECKS ──────────────────────────────────────────────────

const InvariantsGroup: React.FC<{
  panel: PanelState<JobRunsResponse>;
  retry: () => void;
  runs: InvariantRun[];
  items: AttentionItem[];
}> = ({ panel, retry, runs, items }) => {
  const reachable = !panel.loading && !panel.error && !panel.unavailable && panel.data != null;
  const migrated = reachable && panel.data!.table_present;
  const ready = migrated && runs.length > 0;
  const latest = runs[0];
  const results = latest ? Object.entries(latest.det.results) : [];
  const count = (s: string) => results.filter(([, r]) => r.status.toUpperCase() === s).length;
  const invariantNames = Array.from(new Set(runs.flatMap(r => Object.keys(r.det.results)))).sort();
  const columns = runs.slice(0, 14).reverse(); // oldest → newest, current period rightmost
  const summary = panel.loading ? 'Loading check history.'
    : !reachable ? 'Check history could not be read.'
    : !migrated ? 'The results table does not exist on this database.'
    : !latest ? 'No check results are on record.'
    : `Send date ${latest.det.date}: ${fmtInt(count('PASS'))} passed · ${fmtInt(count('FAIL'))} failed · ${fmtInt(count('SKIP'))} skipped · ran ${fmtAge(ageSeconds(latest.run.started_at))} ago`;
  const badge = reachable && !ready
    ? <Pill color={colors.warning}>{migrated ? 'Not recorded' : 'Not set up'}</Pill>
    : <SourceBadge panel={panel} problems={items.length} />;
  return (
    <GroupCard title="Nightly send-day checks" icon={faClipboardCheck} summary={summary} badge={badge} defaultOpen={!ready || items.length > 0}>
      {panel.loading && <LoadingRow label="Loading check history…" />}
      {!panel.loading && panel.unavailable && (
        <UnavailableNote endpoint="/api/mailing/ops/job-runs" detail="Check history cannot be shown until this API is deployed." />
      )}
      {!panel.loading && panel.error && (
        <SectionError label="Send-day checks" error={panel.error} onRetry={retry} />
      )}
      {reachable && !migrated && (
        <UnavailableNote endpoint="mailing_worker_runs" detail="The runs table has not been migrated on this database: the startup migration create_worker_runs has not applied here." />
      )}
      {migrated && !ready && (
        <EmptyState title="No check results on record" hint={runsEmptyHint('send-day check results')} />
      )}
      {ready && latest && (
        <>
          <div style={{ display: 'flex', gap: 26, flexWrap: 'wrap', marginBottom: 10 }}>
            <Stat
              label="Latest result"
              value={<Pill color={stateColor(latest.det.verdict === 'GREEN' ? 'pass' : 'fail')}>{latest.det.verdict}</Pill>}
              sub={`board ${latest.det.board} · send date ${latest.det.date}`}
            />
            <Stat label="Ran" value={`${fmtAge(ageSeconds(latest.run.started_at))} ago`} sub={fmtDenver(latest.run.started_at)} />
            <Stat
              label="Failed checks"
              value={count('FAIL')}
              color={count('FAIL') > 0 ? colors.danger : colors.textMuted}
              sub={`of ${results.length} checks in the latest run`}
            />
          </div>
          {items.length > 0 && <AttentionList items={items} />}
          <Disclosure label={`History, last ${plural(columns.length, 'run')}`}>
            <ScrollTable label="Send-day check history">
              <table style={tableStyle}>
                <thead>
                  <tr>
                    <th style={thStyle}>Check</th>
                    {columns.map(c => (
                      <th key={c.run.started_at} style={{ ...thStyle, textAlign: 'center' }} title={`${c.det.board} · run ${fmtDenver(c.run.started_at)}`}>
                        {c.det.date.slice(5)}
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {invariantNames.map(name => (
                    <tr key={name}>
                      <td style={{ ...tdStyle, whiteSpace: 'nowrap' }} title={name}>{invariantName(name)}</td>
                      {columns.map(c => {
                        const res = c.det.results[name];
                        return (
                          <td key={c.run.started_at} style={{ ...tdStyle, textAlign: 'center' }}
                            title={res ? `${res.status}: ${res.detail}` : 'not checked this run'}>
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
            </ScrollTable>
            <div style={noteStyle}>
              Green = passed · red = failed · dim = skipped, with the reason in the tooltip. Newest run is the rightmost column. Hover any cell for the check's own evidence line. {fetchNote(panel)}.
            </div>
          </Disclosure>
          <Disclosure label="Latest run detail">
            {results.map(([name, res]) => (
              <div key={name} style={{ display: 'flex', flexWrap: 'wrap', gap: '4px 10px', fontSize: 12, padding: '5px 0', borderBottom: `1px solid ${colors.divider}` }}>
                <Pill color={invStatusColor(res.status)}>{res.status}</Pill>
                <span style={{ fontWeight: 600 }} title={name}>{invariantName(name)}</span>
                <span style={{ color: colors.textMuted, overflowWrap: 'anywhere', minWidth: 0 }}>{res.detail}</span>
              </div>
            ))}
          </Disclosure>
        </>
      )}
    </GroupCard>
  );
};

// ── Group: COHORT GROWTH ────────────────────────────────────────────────────

const CohortGroup: React.FC<{ panel: PanelState<JobRunsResponse>; retry: () => void }> = ({ panel, retry }) => {
  const reachable = !panel.loading && !panel.error && !panel.unavailable && panel.data != null;
  const migrated = reachable && panel.data!.table_present;
  const snaps = useMemo(() => cohortSnaps(panel.data), [panel.data]);
  const ready = migrated && snaps.length > 0;
  const latestSnap = snaps[snaps.length - 1];
  const latest = latestSnap ? cohortAgg(latestSnap) : null;
  const prev = snaps.length > 1 ? cohortAgg(snaps[snaps.length - 2]) : null;
  const dClk = latest ? fmtDelta(latest.clk, prev?.clk ?? null) : null;
  const dOpn = latest ? fmtDelta(latest.opn, prev?.opn ?? null) : null;
  const dTot = latest ? fmtDelta(latest.tot, prev?.tot ?? null) : null;
  const ispRows = latestSnap
    ? Object.entries(latestSnap.snapshot)
      .map(([isp, slots]) => ({ isp, clk: slots[CLK] ?? 0, opn: slots[OPN] ?? 0 }))
      .sort((a, b) => (b.clk + b.opn) - (a.clk + a.opn))
    : [];
  const summary = panel.loading ? 'Loading cohort snapshots.'
    : !reachable ? 'Cohort snapshots could not be read.'
    : !migrated ? 'The results table does not exist on this database.'
    : !latest || !dTot ? 'No cohort snapshots are on record.'
    : `${fmtInt(latest.tot)} engaged on ${latestSnap.date} · ${dTot.text} vs prior snapshot`;
  return (
    <GroupCard
      title="Engager growth"
      icon={faSeedling}
      summary={summary}
      badge={panel.unavailable ? <Pill color={colors.warning}>Unavailable</Pill> : panel.error ? <Pill color={colors.danger}>Could not load</Pill> : undefined}
      defaultOpen={!panel.loading && !ready}
    >
      {panel.loading && <LoadingRow label="Loading cohort-growth snapshots…" />}
      {!panel.loading && panel.unavailable && (
        <UnavailableNote endpoint="/api/mailing/ops/job-runs" detail="The cohort trajectory cannot be shown until this API is deployed." />
      )}
      {!panel.loading && panel.error && (
        <SectionError label="Cohort growth" error={panel.error} onRetry={retry} />
      )}
      {reachable && !migrated && (
        <UnavailableNote endpoint="mailing_worker_runs" detail="The runs table has not been migrated on this database." />
      )}
      {migrated && !ready && (
        <EmptyState title="No cohort snapshots on record" hint={runsEmptyHint('cohort snapshots')} />
      )}
      {ready && latest && dClk && dOpn && dTot && (
        <>
          <div style={{ display: 'flex', gap: 26, flexWrap: 'wrap', marginBottom: 14 }}>
            <Stat label="Clickers, 30 days" value={fmtInt(latest.clk)}
              sub={<span style={{ color: dClk.color }}>{dClk.text} vs prior snapshot</span>}
              title="C_CLICK_ACTION_30D: distinct emails in the family slot ledger, excluded rows omitted" />
            <Stat label="Real openers, 21 days" value={fmtInt(latest.opn)}
              sub={<span style={{ color: dOpn.color }}>{dOpn.text} vs prior snapshot</span>}
              title="C_OPEN_REAL_21D: distinct emails in the family slot ledger, excluded rows omitted" />
            <Stat label="Engaged total" value={fmtInt(latest.tot)}
              sub={<span style={{ color: dTot.color }}>{dTot.text} vs prior snapshot</span>}
              title="Clickers + real openers (slot-disjoint by ledger construction)" />
            <Stat label="Snapshot" value={latestSnap.date} sub={`${plural(snaps.length, 'snapshot')} on record`} />
          </div>
          <Disclosure label={`Daily history, last ${plural(Math.min(14, snaps.length), 'snapshot')}`}>
            <ScrollTable label="Cohort growth history">
              <table style={tableStyle}>
                <thead>
                  <tr>
                    <th style={thStyle}>Day</th>
                    <th style={numTh}>Clickers, 30 days</th>
                    <th style={numTh}>Change</th>
                    <th style={numTh}>Real openers, 21 days</th>
                    <th style={numTh}>Change</th>
                    <th style={numTh}>Engaged total</th>
                    <th style={numTh}>Change</th>
                  </tr>
                </thead>
                <tbody>
                  {snaps.slice(-14).map((s, i, arr) => {
                    const cur = cohortAgg(s);
                    const before = i > 0 ? cohortAgg(arr[i - 1]) : null;
                    const dc = fmtDelta(cur.clk, before?.clk ?? null);
                    const dop = fmtDelta(cur.opn, before?.opn ?? null);
                    const dt = fmtDelta(cur.tot, before?.tot ?? null);
                    return (
                      <tr key={s.date}>
                        <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>{s.date}</td>
                        <td style={numTd}>{fmtInt(cur.clk)}</td>
                        <td style={{ ...numTd, color: dc.color, whiteSpace: 'nowrap' }}>{dc.text}</td>
                        <td style={numTd}>{fmtInt(cur.opn)}</td>
                        <td style={{ ...numTd, color: dop.color, whiteSpace: 'nowrap' }}>{dop.text}</td>
                        <td style={numTd}>{fmtInt(cur.tot)}</td>
                        <td style={{ ...numTd, color: dt.color, whiteSpace: 'nowrap' }}>{dt.text}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </ScrollTable>
            <div style={noteStyle}>
              Counts are distinct emails per slot in mailing_family_slot_ledger (excluded rows omitted), snapshotted daily after the nightly cohort rebuild. Raw counts only, with no human or machine split (operator standing rule). Percent changes divide by the immediately prior snapshot. {fetchNote(panel)}.
            </div>
          </Disclosure>
          <Disclosure label={`By mailbox provider, latest snapshot (${latestSnap.date})`}>
            <ScrollTable label="Cohort growth by mailbox provider">
              <table style={tableStyle}>
                <thead>
                  <tr>
                    <th style={thStyle}>Mailbox provider</th>
                    <th style={numTh}>Clickers, 30 days</th>
                    <th style={numTh}>Real openers, 21 days</th>
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
            </ScrollTable>
          </Disclosure>
        </>
      )}
    </GroupCard>
  );
};

// ── Verdict ─────────────────────────────────────────────────────────────────

interface SourceRead { name: string; state: 'loading' | 'read' | 'unreadable'; reason?: string }

const panelRead = (name: string, p: PanelState<unknown>): SourceRead => {
  if (p.loading) return { name, state: 'loading' };
  if (p.unavailable) return { name, state: 'unreadable', reason: 'not on this server build' };
  if (p.error) return { name, state: 'unreadable', reason: p.error };
  return { name, state: 'read' };
};

const VerdictPanel: React.FC<{
  items: AttentionItem[];
  sources: SourceRead[];
  heartbeatsRead: boolean;
  stopped: number;
  healthy: number;
  total: number;
  checkedAt: string | null;
  refreshing: boolean;
  onRefresh: () => void;
}> = ({ items, sources, heartbeatsRead, stopped, healthy, total, checkedAt, refreshing, onRefresh }) => {
  const n = items.length;
  const loading = sources.some(s => s.state === 'loading');
  const unreadable = sources.filter(s => s.state === 'unreadable');
  const count = (k: AttentionKind) => items.filter(i => i.kind === k).length;
  const failing = count('failing');
  const down = count('not_responding');
  const late = count('late');

  let headline: string;
  let tone: string;
  if (n > 0) {
    headline = `${fmtInt(n)} ${n === 1 ? 'thing needs' : 'things need'} attention`;
    tone = failing + down > 0 ? colors.danger : colors.warning;
  } else if (loading) {
    headline = 'Checking…';
    tone = colors.idle;
  } else if (unreadable.length > 0) {
    headline = 'Could not confirm everything is healthy';
    tone = colors.warning;
  } else {
    headline = 'All systems healthy';
    tone = colors.success;
  }

  const tile = (value: number, zeroMuted = true, color: string = colors.danger) => ({
    value: heartbeatsRead ? fmtInt(value) : <Unknown />,
    color: heartbeatsRead && (value > 0 || !zeroMuted) ? color : colors.textMuted,
  });

  return (
    <Panel accent={tone}>
      <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 12, flexWrap: 'wrap' }}>
        <div style={{ minWidth: 0, flex: '1 1 220px' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 10, fontFamily: 'var(--j-font-display)', fontSize: 24, fontWeight: 800, letterSpacing: -0.3, color: tone, lineHeight: 1.2 }}>
            <FontAwesomeIcon icon={n > 0 || unreadable.length > 0 ? faTriangleExclamation : loading ? faSpinner : faCircleCheck} spin={n === 0 && loading} style={{ fontSize: 20 }} />
            <span>{headline}</span>
          </div>
          <div style={{ fontSize: 12, color: colors.textMuted, marginTop: 6, lineHeight: 1.5 }}>
            {stopped > 0 && <div>{plural(stopped, 'job')} silent more than {STOPPED_AFTER_LABEL} {stopped === 1 ? 'is' : 'are'} counted separately, under Stopped.</div>}
            {unreadable.map(s => (
              <div key={s.name} style={{ color: colors.warning, overflowWrap: 'anywhere' }}>Could not read {s.name}: {s.reason}</div>
            ))}
            {checkedAt && <div>Last checked {checkedAt} Denver. Updates every {POLL_MS / 1000} seconds.</div>}
          </div>
        </div>
        <button type="button" onClick={onRefresh} style={{ ...btnStyle, display: 'flex', alignItems: 'center', gap: 8, flexShrink: 0 }}>
          <FontAwesomeIcon icon={refreshing ? faSpinner : faRotate} spin={refreshing} /> Refresh all
        </button>
      </div>
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(130px, 1fr))', gap: 14, marginTop: 14 }}>
        <Stat label="Failing" {...tile(failing)} sub="jobs, workers and send-day checks" />
        <Stat label="Not responding" {...tile(down)} sub="missed their check-in" />
        <Stat label="Late" {...tile(late, true, colors.warning)} sub="overdue, or segments out of date" />
        <Stat label="Stopped" {...tile(stopped, true, colors.textMuted)} sub={`silent more than ${STOPPED_AFTER_LABEL}`} />
        <Stat label="Healthy" {...tile(healthy, false, colors.success)} sub={heartbeatsRead ? `of ${fmtInt(total)} jobs and workers` : undefined} />
      </div>
    </Panel>
  );
};

const AttentionPanel: React.FC<{ items: AttentionItem[]; loading: boolean; unreadable: boolean }> = ({ items, loading, unreadable }) => {
  const [showAll, setShowAll] = useState(false);
  const visible = showAll ? items : items.slice(0, ATTENTION_PREVIEW);
  return (
    <div style={panelStyle}>
      <SectionHeader title="Needs attention" icon={faTriangleExclamation} style={{ marginBottom: 6 }} />
      {items.length > 0 ? (
        <>
          <AttentionList items={visible} />
          {items.length > ATTENTION_PREVIEW && (
            <button
              type="button"
              aria-expanded={showAll}
              onClick={() => setShowAll(v => !v)}
              style={{ background: 'none', border: 'none', color: colors.indigo400, cursor: 'pointer', fontSize: 12, padding: '8px 0 0' }}>
              {showAll ? 'Show fewer' : `Show all ${fmtInt(items.length)}`}
            </button>
          )}
        </>
      ) : loading ? (
        <LoadingRow label="Checking jobs, workers, segments and send-day checks…" />
      ) : unreadable ? (
        <EmptyState title="Nothing flagged in the sources that could be read" hint="At least one source could not be read, so this is not an all clear." />
      ) : (
        <EmptyState icon={faCircleCheck} title="Nothing needs you right now" />
      )}
    </div>
  );
};

// ── Page ────────────────────────────────────────────────────────────────────

export const OperationsConsole: React.FC = () => {
  const [nonce, setNonce] = useState(0);
  const refreshAll = useCallback(() => setNonce(n => n + 1), []);
  const [workersPanel, retryWorkers] = usePanelFetch<WorkerHealthResponse>('/api/worker-health', nonce);
  const [familiesPanel, retryFamilies] = usePanelFetch<FreshnessResponse>(
    '/api/mailing/segment-registry/freshness', nonce);
  const [invariantsPanel, retryInvariants] = usePanelFetch<JobRunsResponse>(
    '/api/mailing/ops/job-runs?worker=job:send_day_invariants&limit=30', nonce);
  const [cohortPanel, retryCohort] = usePanelFetch<JobRunsResponse>(
    '/api/mailing/ops/job-runs?worker=job:cohort_growth&limit=30', nonce);

  // Heartbeats polled every 10s on the Delivery screen before they moved here.
  useEffect(() => {
    const id = setInterval(() => { if (!document.hidden) refreshAll(); }, POLL_MS);
    return () => clearInterval(id);
  }, [refreshAll]);

  const workers = useMemo(() => workersPanel.data?.workers ?? [], [workersPanel.data]);
  const families = useMemo(() => familiesPanel.data?.families ?? [], [familiesPanel.data]);
  const jobs = useMemo(() => buildJobs(workers), [workers]);
  const runs = useMemo(() => invariantRuns(invariantsPanel.data), [invariantsPanel.data]);

  const heartbeatsReady = !workersPanel.error && !workersPanel.unavailable && workers.length > 0;
  const invariantsReady = !invariantsPanel.error && !invariantsPanel.unavailable && runs.length > 0;

  const jobItems = useMemo(() => jobs.map(jobItem).filter((i): i is AttentionItem => i != null), [jobs]);
  const familyItems = useMemo(() => families.map(familyItem).filter((i): i is AttentionItem => i != null), [families]);
  const checkItems = useMemo(() => invariantItems(runs[0]), [runs]);

  // Failed send-day checks are named one by one, so their job's own "2 failed"
  // heartbeat would count the same problem twice.
  const attention = useMemo(() => [
    ...checkItems,
    ...jobItems.filter(i => !(invariantsReady && i.id === 'job-send_day_invariants')),
    ...familyItems,
  ].sort((a, b) => a.order - b.order || a.age - b.age), [checkItems, jobItems, familyItems, invariantsReady]);

  const sources: SourceRead[] = [
    workersPanel.data && workers.length === 0
      ? { name: 'job and worker heartbeats', state: 'unreadable', reason: 'the server returned none' }
      : panelRead('job and worker heartbeats', workersPanel),
    familiesPanel.data && families.length === 0
      ? { name: 'segment freshness', state: 'unreadable', reason: 'the registry is empty' }
      : panelRead('segment freshness', familiesPanel),
    invariantsPanel.data && !invariantsPanel.data.table_present
      ? { name: 'send-day checks', state: 'unreadable', reason: 'the results table does not exist' }
      : invariantsPanel.data && runs.length === 0
        ? { name: 'send-day checks', state: 'unreadable', reason: 'no results on record' }
        : panelRead('send-day checks', invariantsPanel),
  ];

  // Owner annotation: a registry family that declares this heartbeat_worker
  // names the owning process (the heartbeat table itself is ownerless).
  const ownerFor = useCallback((name: string): string => {
    const fam = families.find(f => f.heartbeat_worker === name);
    if (fam) return fam.owner;
    return name.startsWith('job:') ? 'Scheduled job' : 'Platform worker';
  }, [families]);

  const scheduled = jobs.filter(j => j.isJob);
  const inProcess = jobs.filter(j => !j.isJob);
  const refreshing = [workersPanel, familiesPanel, invariantsPanel, cohortPanel].some(p => p.refreshing);

  return (
    <div
      style={{ ...pageStyle, padding: 'clamp(12px, 4vw, 24px)', minWidth: 0 }}
      data-page-version={PAGE_VERSION}
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 16, minWidth: 0 }}>
        <VerdictPanel
          items={attention}
          sources={sources}
          heartbeatsRead={heartbeatsReady}
          stopped={jobs.filter(j => j.health === 'stopped').length}
          healthy={jobs.filter(j => j.health === 'ok' || j.health === 'running').length}
          total={jobs.length}
          checkedAt={workersPanel.fetchedAt}
          refreshing={refreshing}
          onRefresh={refreshAll}
        />
        <AttentionPanel
          items={attention}
          loading={sources.some(s => s.state === 'loading')}
          unreadable={sources.some(s => s.state === 'unreadable')}
        />
        <HeartbeatGroup
          title="Scheduled jobs"
          icon={faClock}
          noun="job"
          panel={workersPanel}
          retry={retryWorkers}
          jobs={scheduled}
          rows={workers.filter(w => w.worker_name.startsWith('job:'))}
          items={jobItems.filter(i => i.group === 'jobs').sort((a, b) => a.order - b.order || a.age - b.age)}
          ownerFor={ownerFor}
        />
        <HeartbeatGroup
          title="Background workers"
          icon={faHeartPulse}
          noun="worker"
          panel={workersPanel}
          retry={retryWorkers}
          jobs={inProcess}
          rows={workers.filter(w => !w.worker_name.startsWith('job:'))}
          items={jobItems.filter(i => i.group === 'workers').sort((a, b) => a.order - b.order || a.age - b.age)}
          ownerFor={ownerFor}
        />
        <FamiliesGroup panel={familiesPanel} retry={retryFamilies} items={familyItems} />
        <InvariantsGroup panel={invariantsPanel} retry={retryInvariants} runs={runs} items={checkItems} />
        <CohortGroup panel={cohortPanel} retry={retryCohort} />
      </div>
    </div>
  );
};

export default OperationsConsole;
