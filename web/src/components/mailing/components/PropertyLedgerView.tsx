// Property Ledger — per (sending domain × ISP) drip-introduction budgets
// (Vector A plan rev4, Step 19; per-domain VDM lane view + lane content panel
// added on operator direction 2026-08-17).
//
// Semantics surfaced to the operator (I-2/I-3, property_ledger_doc.go):
//   - "Budget edits apply from tomorrow (Denver). Hold is immediate."
//   - Edits ARE approvals; a pending badge shows the staged next-day value.
//   - Holds are server-confirmed (spinner until 200 + refetch), reason
//     required.
//   - CAS via integer lock_version — a 409 means the row changed underneath;
//     refetch and retry.
//   - Global hold banner (fail-closed emergency stop, outranks the overlay
//     kill switch); coverage warnings; run-freshness strip; shadow banner
//     while alerts are off.
//   - Default view = ONE sending domain (highest yesterday VDM send volume;
//     first alphabetically when VDM has no volume yet) with per-lane VDM
//     stats (yesterday sent / delivered% / open / click + 7-day sent, all
//     VDM unique-count semantics, complete UTC days only). "All domains"
//     renders the full grid unchanged.
//   - Lane content panel (single-domain view): which offer / creative /
//     subjects / from-names / touch copy each feed on the brand sends, with
//     in-screen edits (audit-logged server-side).

import React, { useCallback, useEffect, useState } from 'react';
import { apiFetch } from '../shared/apiFetch';
import { usePolling, type PollingState } from '../shared/usePolling';
import {
  buildThrottleDiff, buildThrottlePayload, validateThrottleRows,
  type ThrottleRow,
} from './throttleDiff';

interface Proposal {
  id: string;
  proposed_budget: number;
  base_budget: number;
  basis: string;
  created_at: string;
  expires_at: string;
}

interface LaneVDM {
  sent: number;
  delivered: number;
  opens: number;
  clicks: number;
  delivered_pct: number | null;
  open_rate_vdm: number | null;
  click_rate_vdm: number | null;
  sent_7d: number;
  delivered_7d: number;
  opens_7d: number;
  clicks_7d: number;
  delivered_pct_7d: number | null;
  open_rate_vdm_7d: number | null;
  click_rate_vdm_7d: number | null;
}

interface LedgerRow {
  brand: string;
  isp: string;
  sending_domain: string;
  daily_budget: number;
  hold: boolean;
  hold_reason?: string;
  held_since?: string;
  notes?: string;
  updated_by?: string;
  updated_at: string;
  approved_by?: string;
  approved_at?: string;
  min_budget?: number;
  max_budget?: number;
  lock_version: number;
  pending_budget?: number;
  pending_effective_day?: string;
  proposal?: Proposal;
  introduced_today: number | null;
  introduced_as_of?: string;
  vdm?: LaneVDM;
}

interface RunInfo {
  day: string;
  status: string;
  started_at: string;
  finished_at?: string;
}

interface GlobalHold {
  value: boolean;
  reason: string;
  since: string;
  lock_version: number;
}

interface LedgerResponse {
  day: string;
  rows: LedgerRow[];
  vdm?: { day: string; window_start: string; note: string };
  global_hold: GlobalHold;
  runs: { counter: RunInfo | null; vdm: RunInfo | null; reconciliation: RunInfo | null };
  alerts_enabled: boolean;
}

interface CoverageRow { brand: string; isp: string; status: string; observed_sends: number; }

// ── Lane content panel shapes (property_lane_content.go) ────────────────────

interface LaneCopyRow { id: string; text: string; status: string; serving: boolean; }
interface LaneCreative { id: string; version: number; status: string; updated_at: string; html_bytes: number; }
interface LaneOfferSharing { feeds: number; brands: number; }
interface LaneOffer {
  id: string; name: string; status: string;
  creative: LaneCreative | null;
  subjects: LaneCopyRow[]; from_names: LaneCopyRow[];
  sharing?: LaneOfferSharing;
}
interface LaneDataset {
  id: string; name: string; status: string; express_dispatch: boolean;
  offer: LaneOffer | null;
}
interface LaneTouchCopy {
  vertical: string; touch: number; creative_filename: string;
  subject_line: string; preheader: string; from_name: string;
  offer_id?: string; active: boolean; updated_at: string; updated_by?: string;
}
// Offer bound at the TOUCH level (per-touch override in the orchestrator) —
// how NULL-offer datasets like db/internal_auto still mail a real offer.
interface LaneTouchOffer { binding: string; touch_scope: string[]; offer: LaneOffer | null; }
interface LaneFeed {
  vertical: string; weight: number; datasets: LaneDataset[];
  touch1: LaneTouchCopy | null; followups: LaneTouchCopy[];
  touch_offers: LaneTouchOffer[];
}
interface LaneContentResponse {
  brand: string; sending_domain: string;
  feeds: LaneFeed[]; global_followups: LaneTouchCopy[];
  active_offers: Array<{ id: string; name: string }>;
  preheader_note: string;
}

// ── Supply strip shapes (property_lane_supply.go — Pipeline Cockpit P1) ─────

interface SupplyISP { isp: string; ready: number; }
interface SupplyFeed {
  dataset_id: string; name: string; vertical: string; status: string;
  daily_cap: number;          // SUPPLY-RELEASE budget (lane), not the claim-side ISP cap
  paused_emergency: boolean;
  shared_brands: string[];    // rotation brands this dataset's supply is shared across
  tranche_total: number;
  cleaning: number; pending_eo: number; eo_in_flight: number;
  ready_total: number; ready_by_isp: SupplyISP[];
  held: number; suppressed: number; dead_letter: number;
  mailed_lifetime: number; mailed_today: number;
}
interface SupplyResponse {
  domain: string; brand: string; sending_domain: string; non_ledger: boolean;
  as_of: string; denver_day: string;
  ready_semantics: string; supply_note: string;
  feeds: SupplyFeed[];
}

// ── Throttle shapes (property_lane_supply.go HandleLaneThrottle — Cockpit P2)

interface ThrottleOverride {
  isp: string;
  pct_override: number;
  max_per_wave: number;
  daily_cap?: number;         // absent = lane rides the global default; 0 = hard-suppressed
  updated_at: string;
  updated_by?: string;
}
interface ThrottleFeed {
  dataset_id: string; name: string; vertical: string; status: string;
  paused_emergency: boolean;
  supply_release_daily_cap: number; // SUPPLY-side release budget — distinct system from the claim caps
  shared_brands: string[];
  overrides: ThrottleOverride[];    // CLAIM-side per-ISP enforcement (live orchestrator input)
  default_isps: string[];           // ledger ISPs with no override — ride global defaults
}
interface ThrottleResponse {
  domain: string; brand: string; sending_domain: string; non_ledger: boolean;
  write_enabled: boolean;   // server env PROPERTY_LEDGER_THROTTLE_WRITE_ENABLED — gates the edit surface
  write_flag_env: string;
  write_endpoint: string;
  replacement_note: string;
  enforcement_note: string;
  cap_systems_note: string;
  feeds: ThrottleFeed[];
}

// wcl-heloc is a first-class cockpit domain but NOT a ledger brand (cockpit
// plan §1): it appears in the dropdown, its supply strip is live, and the
// budget table renders an honest "Not in ledger — budgets N/A" state.
const NON_LEDGER_DOMAINS = ['wcl-heloc.com'];

const num = (n: number | null | undefined) => (n ?? 0).toLocaleString();
const pct = (v: number | null | undefined): string =>
  v == null ? '—' : `${(v * 100).toFixed(1)}%`;

const age = (iso?: string): string => {
  if (!iso) return '—';
  const ms = Date.now() - new Date(iso).getTime();
  if (ms < 0) return 'now';
  const m = Math.floor(ms / 60000);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
};

const cellKey = (r: { brand: string; isp: string }) => `${r.brand}/${r.isp}`;

// Dead-lane rule: delivered% under 50 on meaningful volume (≥100 VDM sends).
const isDeadLane = (v?: LaneVDM): boolean =>
  !!v && v.sent >= 100 && v.delivered_pct != null && v.delivered_pct < 0.5;

const ALL_DOMAINS = 'ALL';

export const PropertyLedgerView: React.FC = () => {
  const [data, setData] = useState<LedgerResponse | null>(null);
  const [coverage, setCoverage] = useState<CoverageRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [edit, setEdit] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState<Record<string, boolean>>({});
  const [ghBusy, setGhBusy] = useState(false);
  const [domain, setDomain] = useState<string | null>(null); // null until data picks the default

  const load = useCallback(async () => {
    setLoading(true); setErr(null);
    try {
      const r = await apiFetch('/api/mailing/pmta-campaign/property-ledger');
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      setData(await r.json());
      const c = await apiFetch('/api/mailing/pmta-campaign/property-ledger/coverage');
      if (c.ok) {
        const cj = await c.json();
        setCoverage((cj.rows ?? []).filter((x: CoverageRow) =>
          x.status === 'EXTRA_CELL' || x.status.startsWith('UNKNOWN_')));
      }
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'load failed');
    } finally { setLoading(false); }
  }, []);

  useEffect(() => { void load(); }, [load]);

  // Default domain = highest yesterday VDM send volume; first alphabetically
  // when VDM shows no volume yet. Picked once, when data first arrives.
  useEffect(() => {
    if (domain !== null || !data || data.rows.length === 0) return;
    const byDomain = new Map<string, number>();
    for (const r of data.rows) {
      const d = r.sending_domain || r.brand;
      byDomain.set(d, (byDomain.get(d) ?? 0) + (r.vdm?.sent ?? 0));
    }
    const domains = Array.from(byDomain.keys()).sort();
    let best = domains[0];
    let bestSent = -1;
    for (const d of domains) {
      const s = byDomain.get(d) ?? 0;
      if (s > bestSent) { best = d; bestSent = s; }
    }
    setDomain(bestSent > 0 ? best : domains[0]);
  }, [data, domain]);

  // Supply poll is hosted HERE (not inside the strip) so the lane table's
  // Ready column reuses the SAME response — one fetch, two renderings
  // (operator addendum: hot queue visible beside the caps). Null when the
  // "All domains" grid is showing (dataset supply is not a grid fact).
  const supply = usePolling<SupplyResponse | null>(
    async (signal) => {
      if (!domain || domain === ALL_DOMAINS) return null;
      const r = await apiFetch(
        `/api/mailing/pmta-campaign/property-ledger/supply?domain=${encodeURIComponent(domain)}`,
        { signal });
      if (!r.ok) {
        // Keep the status in the message: 503 = the covering index is still
        // building (the strip/column render it as "warming up", not failure).
        let msg = `HTTP ${r.status}`;
        try {
          const j = await r.json();
          if (j && typeof j.error === 'string') msg = `HTTP ${r.status}: ${j.error}`;
        } catch { /* non-JSON error body */ }
        throw new Error(msg);
      }
      return r.json() as Promise<SupplyResponse>;
    }, 60_000, [domain]);
  const supplyWarming = !!supply.error && supply.error.startsWith('HTTP 503');

  // Throttle read (Cockpit P2): fetched per domain, refetched after a write.
  const [throttle, setThrottle] = useState<ThrottleResponse | null>(null);
  const [throttleErr, setThrottleErr] = useState<string | null>(null);
  const loadThrottle = useCallback(async () => {
    if (!domain || domain === ALL_DOMAINS) { setThrottle(null); return; }
    setThrottleErr(null);
    try {
      const r = await apiFetch(
        `/api/mailing/pmta-campaign/property-ledger/throttle?domain=${encodeURIComponent(domain)}`);
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      setThrottle(await r.json());
    } catch (e) {
      setThrottleErr(e instanceof Error ? e.message : 'load failed');
    }
  }, [domain]);
  useEffect(() => { setThrottle(null); void loadThrottle(); }, [loadThrottle]);

  const post = async (path: string, body: Record<string, unknown>): Promise<{ ok: boolean; status: number; json: Record<string, unknown> }> => {
    const r = await apiFetch(`/api/mailing/pmta-campaign/property-ledger/${path}`, {
      method: 'POST', body: JSON.stringify(body),
    });
    let json: Record<string, unknown> = {};
    try { json = await r.json(); } catch { /* non-JSON error body */ }
    return { ok: r.ok, status: r.status, json };
  };

  const withRowBusy = async (key: string, fn: () => Promise<void>) => {
    setBusy(b => ({ ...b, [key]: true }));
    setNotice(null);
    try { await fn(); } finally { setBusy(b => ({ ...b, [key]: false })); }
  };

  const handle409 = (json: Record<string, unknown>) => {
    setNotice(`Row changed underneath you (lock_version conflict) — reloaded fresh values. ${String(json.error ?? '')}`);
    void load();
  };

  const saveBudget = (row: LedgerRow, value: number) =>
    withRowBusy(cellKey(row), async () => {
      const res = await post('update', {
        brand: row.brand, isp: row.isp, daily_budget: value, lock_version: row.lock_version,
      });
      if (res.status === 409) { handle409(res.json); return; }
      if (!res.ok) { setNotice(`Budget edit failed: ${String(res.json.error ?? res.status)}`); return; }
      setEdit(s => { const n = { ...s }; delete n[cellKey(row)]; return n; });
      await load();
    });

  // Server-confirmed hold: spinner until the 200 lands, then refetch — the
  // UI never assumes the hold applied.
  const toggleHold = (row: LedgerRow) =>
    withRowBusy(cellKey(row), async () => {
      const reason = window.prompt(
        row.hold ? `Release hold on ${row.brand}/${row.isp} — reason:` : `Hold ${row.brand}/${row.isp} — reason:`);
      if (!reason || !reason.trim()) return;
      const res = await post('update', {
        brand: row.brand, isp: row.isp, hold: !row.hold, reason: reason.trim(), lock_version: row.lock_version,
      });
      if (res.status === 409) { handle409(res.json); return; }
      if (!res.ok) { setNotice(`Hold change failed: ${String(res.json.error ?? res.status)}`); return; }
      await load();
    });

  const approveProposal = (row: LedgerRow) =>
    withRowBusy(cellKey(row), async () => {
      if (!row.proposal) return;
      const res = await post('approve-proposal', { proposal_id: row.proposal.id });
      if (res.status === 409) {
        setNotice(`Proposal for ${row.brand}/${row.isp} was stale — superseded. ${String(res.json.error ?? '')}`);
        await load();
        return;
      }
      if (!res.ok) { setNotice(`Approve failed: ${String(res.json.error ?? res.status)}`); return; }
      await load();
    });

  const approveAll = async () => {
    const ids = (data?.rows ?? []).filter(r => r.proposal).map(r => r.proposal!.id);
    if (ids.length === 0) return;
    if (!window.confirm(`Approve all ${ids.length} open proposals? Each applies from tomorrow (Denver).`)) return;
    setNotice(null);
    const res = await post('approve-proposals', { proposal_ids: ids });
    if (!res.ok) { setNotice(`Bulk approve failed: HTTP ${res.status}`); return; }
    const results = (res.json.results ?? []) as Array<Record<string, unknown>>;
    const failed = results.filter(x => x.status_code !== 200);
    setNotice(failed.length === 0
      ? `All ${results.length} proposals approved (effective tomorrow, Denver).`
      : `${results.length - failed.length} approved, ${failed.length} failed/stale — see refreshed rows.`);
    await load();
  };

  const toggleGlobalHold = async () => {
    if (!data) return;
    const enabling = !data.global_hold.value;
    // Confirmation is deliberate: this zeroes EVERY drip intro cap at once.
    if (!window.confirm(enabling
      ? 'ENGAGE GLOBAL HOLD? Every drip-introduction cap goes to ZERO within one orchestrator tick.'
      : 'Release the global hold? Per-cell budgets resume within one orchestrator tick.')) return;
    const reason = window.prompt(`${enabling ? 'Global hold' : 'Release global hold'} — reason:`);
    if (!reason || !reason.trim()) return;
    setGhBusy(true); setNotice(null);
    try {
      const res = await post('global-hold', {
        value: enabling, reason: reason.trim(), lock_version: data.global_hold.lock_version,
      });
      if (res.status === 409) { handle409(res.json); return; }
      if (!res.ok) { setNotice(`Global hold change failed: ${String(res.json.error ?? res.status)}`); return; }
      await load();
    } finally { setGhBusy(false); }
  };

  const th: React.CSSProperties = {
    textAlign: 'left', padding: '8px 10px', fontSize: 11, letterSpacing: 0.6,
    color: 'rgba(180,210,240,0.65)', textTransform: 'uppercase', whiteSpace: 'nowrap',
  };
  const td: React.CSSProperties = {
    padding: '8px 10px', fontSize: 13, color: '#e6edf5',
    borderTop: '1px solid rgba(255,255,255,0.06)', whiteSpace: 'nowrap',
  };
  const thNum: React.CSSProperties = { ...th, textAlign: 'right' };
  const tdNum: React.CSSProperties = { ...td, textAlign: 'right', fontVariantNumeric: 'tabular-nums' };
  const banner = (bg: string, border: string, color: string): React.CSSProperties => ({
    background: bg, border: `1px solid ${border}`, color, borderRadius: 8,
    padding: '10px 14px', fontSize: 13, marginBottom: 12, fontWeight: 600,
  });

  const allRows = data?.rows ?? [];
  const domains = Array.from(new Set([
    ...allRows.map(r => r.sending_domain || r.brand),
    ...NON_LEDGER_DOMAINS,
  ])).sort();
  const isNonLedgerDomain = domain != null && NON_LEDGER_DOMAINS.includes(domain);
  const showAll = domain === ALL_DOMAINS;
  const rows = showAll ? allRows : allRows.filter(r => (r.sending_domain || r.brand) === domain);
  const selectedBrand = !showAll ? rows[0]?.brand : undefined;
  const proposalCount = allRows.filter(r => r.proposal).length;
  const runs = data?.runs;
  const vdmMeta = data?.vdm;

  // Ready-by-ISP for the lane table (operator addendum): cleaned records
  // ready for dispatch, summed across the domain's feeds — supply is a
  // DATASET fact shared across the rotation, so the tooltip breaks the sum
  // down per feed and carries the shared caveat.
  const readyByISP = new Map<string, { total: number; perFeed: Array<{ name: string; n: number }> }>();
  for (const f of supply.data?.feeds ?? []) {
    for (const e of f.ready_by_isp) {
      const entry = readyByISP.get(e.isp) ?? { total: 0, perFeed: [] };
      entry.total += e.ready;
      entry.perFeed.push({ name: f.name, n: e.ready });
      readyByISP.set(e.isp, entry);
    }
  }
  const supplyFeedCount = supply.data?.feeds.length ?? 0;
  const renderReadyCell = (isp: string): React.ReactNode => {
    if (supplyWarming) {
      return <span style={{ color: 'rgba(180,210,240,0.45)' }}
        title="Supply view warming up — the covering index is still building (CONCURRENTLY); retries automatically every 60s.">
        warming up
      </span>;
    }
    if (!supply.data) {
      return <span style={{ color: 'rgba(180,210,240,0.45)' }}
        title={supply.error ? `Supply fetch failed (${supply.error})` : 'Loading live supply…'}>
        {supply.error ? '—' : '…'}
      </span>;
    }
    const e = readyByISP.get(isp);
    const total = e?.total ?? 0;
    const breakdown = supplyFeedCount > 1 && e
      ? e.perFeed.map(p => `${p.name}: ${num(p.n)}`).join(' · ')
      : '';
    const tip = `Cleaned records claimable now (live queue).`
      + (breakdown ? ` Per feed — ${breakdown}.` : '')
      + ' Supply is dataset-level, shared across the rotation — not owned by this domain.';
    return <span title={tip}
      style={{ color: total > 0 ? '#00b894' : 'rgba(180,210,240,0.5)' }}>
      {num(total)}
    </span>;
  };

  return (
    <div style={{ padding: 20 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 14, marginBottom: 6 }}>
        <h2 style={{ margin: 0, fontSize: 18, color: '#e6edf5' }}>Property Ledger</h2>
        <select
          value={domain ?? ALL_DOMAINS}
          onChange={e => setDomain(e.target.value)}
          style={{ background: 'rgba(255,255,255,0.06)', color: '#e6edf5',
                   border: '1px solid rgba(255,255,255,0.15)', borderRadius: 6,
                   padding: '5px 8px', fontSize: 12 }}>
          {domains.map(d => <option key={d} value={d}>{d}</option>)}
          <option value={ALL_DOMAINS}>All domains</option>
        </select>
        <span style={{ fontSize: 12, color: 'rgba(180,210,240,0.7)' }}>
          {rows.length} {showAll ? 'cells' : 'lanes'} · day {data?.day ?? '—'}
          {vdmMeta ? ` · VDM day ${vdmMeta.day} (UTC)` : ''}
        </span>
        <button onClick={() => void load()} style={{
          marginLeft: 'auto', background: 'transparent', color: '#9fd3ff',
          border: '1px solid rgba(120,180,255,0.35)', borderRadius: 6,
          padding: '6px 12px', fontSize: 12, cursor: 'pointer',
        }}>{loading ? 'Loading…' : 'Refresh'}</button>
      </div>

      <p style={{ fontSize: 12, color: '#facc15', margin: '0 0 12px 0', fontWeight: 600 }}>
        Budget edits apply from tomorrow (Denver). Hold is immediate.
      </p>

      {err && <div style={{ color: '#e94560', fontSize: 12, marginBottom: 10 }}>{err}</div>}
      {notice && <div style={{ color: '#facc15', fontSize: 12, marginBottom: 10 }}>{notice}</div>}

      {data && !data.alerts_enabled && (
        <div style={banner('rgba(99,102,241,0.10)', 'rgba(99,102,241,0.4)', '#a5b4fc')}>
          SHADOW MODE — reconciliation alerts are not being delivered (P7 activation is operator-gated).
        </div>
      )}

      {data?.global_hold.value && (
        <div style={banner('rgba(233,69,96,0.12)', 'rgba(233,69,96,0.5)', '#e94560')}>
          GLOBAL HOLD ENGAGED since {age(data.global_hold.since)} — every drip intro cap is ZERO.
          {data.global_hold.reason ? ` Reason: ${data.global_hold.reason}` : ''}
        </div>
      )}

      {coverage.length > 0 && (
        <div style={banner('rgba(233,69,96,0.12)', 'rgba(233,69,96,0.5)', '#e94560')}>
          COVERAGE WARNING — {coverage.length} cell(s) outside the ledger grid:{' '}
          {coverage.slice(0, 6).map(c => `${c.brand}/${c.isp} (${c.status}, ${num(c.observed_sends)} sends)`).join(', ')}
          {coverage.length > 6 ? ' …' : ''}
        </div>
      )}

      <div style={{ display: 'flex', gap: 16, flexWrap: 'wrap', marginBottom: 12, fontSize: 11, color: 'rgba(180,210,240,0.7)' }}>
        <span>Counter run: {runs?.counter ? `${runs.counter.status} · ${age(runs.counter.finished_at ?? runs.counter.started_at)}` : 'none yet'}</span>
        <span>VDM run: {runs?.vdm ? `${runs.vdm.status} · ${age(runs.vdm.finished_at ?? runs.vdm.started_at)}` : 'none yet'}</span>
        <span>Reconciliation: {runs?.reconciliation ? `${runs.reconciliation.status} · ${age(runs.reconciliation.finished_at ?? runs.reconciliation.started_at)}` : 'none yet (P5)'}</span>
        <button onClick={() => void toggleGlobalHold()} disabled={ghBusy || !data} style={{
          marginLeft: 'auto',
          background: data?.global_hold.value ? 'rgba(0,184,148,0.12)' : 'rgba(233,69,96,0.15)',
          color: data?.global_hold.value ? '#00b894' : '#e94560',
          border: `1px solid ${data?.global_hold.value ? 'rgba(0,184,148,0.4)' : 'rgba(233,69,96,0.4)'}`,
          borderRadius: 6, padding: '5px 12px', fontSize: 11, fontWeight: 700, cursor: 'pointer',
        }}>
          {ghBusy ? 'Confirming…' : data?.global_hold.value ? 'RELEASE GLOBAL HOLD' : 'GLOBAL HOLD'}
        </button>
      </div>

      {proposalCount > 0 && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 10 }}>
          <span style={{ fontSize: 12, color: '#a5b4fc' }}>{proposalCount} open grader proposal(s)</span>
          <button onClick={() => void approveAll()} style={{
            background: 'rgba(99,102,241,0.15)', color: '#a5b4fc',
            border: '1px solid rgba(99,102,241,0.4)', borderRadius: 6,
            padding: '4px 10px', fontSize: 11, fontWeight: 700, cursor: 'pointer',
          }}>Approve all</button>
        </div>
      )}

      {!showAll && domain && (
        <SupplyStrip domain={domain} supply={supply} throttle={throttle}
          throttleErr={throttleErr} onThrottleChanged={() => void loadThrottle()}
          onNotice={setNotice} />
      )}

      <div style={{ overflowX: 'auto' }}>
        <table style={{ borderCollapse: 'collapse', width: '100%', minWidth: showAll ? 1000 : 1250 }}>
          <thead><tr>
            <th style={th}>Property</th>
            <th style={th}>ISP</th>
            <th style={thNum}>Drip Intro Daily Cap</th>
            <th style={thNum}>Introductions Today</th>
            {!showAll && (
              <th style={thNum}
                title="Cleaned records ready for dispatch (claimable now) per ISP, from the live supply queue — summed across this domain's feeds. Supply is dataset-level, shared across the rotation.">
                Ready
              </th>
            )}
            <th style={th}>Drip Intro Hold</th>
            {!showAll && (<>
              <th style={thNum} title={`VDM yesterday (UTC ${vdmMeta?.day ?? ''}) — unique-count semantics`}>Sent (yday)</th>
              <th style={thNum} title="delivery / send">Delivered %</th>
              <th style={thNum} title="unique opens / delivery (VDM unique-count semantics)">Open Rate</th>
              <th style={thNum} title="unique clicks / delivery (VDM unique-count semantics)">Click Rate</th>
              <th style={thNum} title={`VDM sends over the trailing 7 complete UTC days (${vdmMeta?.window_start ?? ''} → ${vdmMeta?.day ?? ''})`}>Sent 7d</th>
            </>)}
            <th style={th}>Proposal</th>
            <th style={th}>Approved</th>
          </tr></thead>
          <tbody>
            {rows.map(r => {
              const key = cellKey(r);
              const isBusy = !!busy[key];
              const dead = !showAll && isDeadLane(r.vdm);
              return (
                <tr key={key} style={{
                  opacity: r.hold || data?.global_hold.value ? 0.55 : 1,
                  background: dead ? 'rgba(233,69,96,0.10)' : undefined,
                }}>
                  <td style={{ ...td, fontWeight: 600 }}>
                    {r.sending_domain || r.brand}
                    <span style={{ color: 'rgba(180,210,240,0.5)', marginLeft: 6, fontWeight: 400 }}>{r.brand}</span>
                  </td>
                  <td style={{ ...td, color: 'rgba(180,210,240,0.75)' }}>
                    {r.isp}
                    {dead && (
                      <span title={`Dead lane: delivered ${pct(r.vdm?.delivered_pct)} on ${num(r.vdm?.sent)} VDM sends yesterday`}
                        style={{ marginLeft: 6, color: '#e94560', fontSize: 10, fontWeight: 700 }}>
                        DEAD LANE
                      </span>
                    )}
                  </td>
                  <td style={{ ...td, textAlign: 'right' }}>
                    <input
                      type="number" min={0} step={100}
                      value={edit[key] ?? String(r.daily_budget)}
                      disabled={isBusy}
                      onChange={e => setEdit(s => ({ ...s, [key]: e.target.value }))}
                      onBlur={e => {
                        const v = parseInt(e.target.value, 10);
                        if (!Number.isNaN(v) && v !== r.daily_budget) void saveBudget(r, v);
                        else setEdit(s => { const n = { ...s }; delete n[key]; return n; });
                      }}
                      style={{ width: 90, textAlign: 'right', background: 'rgba(255,255,255,0.06)',
                               color: '#e6edf5', border: '1px solid rgba(255,255,255,0.15)',
                               borderRadius: 5, padding: '4px 6px', fontSize: 12 }} />
                    {r.pending_budget != null && (
                      <span title={`Staged edit — promotes at the Denver day boundary (${r.pending_effective_day})`}
                        style={{ fontSize: 10, color: '#facc15', marginLeft: 6, fontWeight: 700 }}>
                        → {num(r.pending_budget)} applies tomorrow
                      </span>
                    )}
                  </td>
                  <td style={{ ...td, textAlign: 'right' }}>
                    {r.introduced_today == null
                      ? <span style={{ color: 'rgba(180,210,240,0.45)' }} title="No counter row yet for today — the rollup worker materializes cells every 10 minutes.">no counter yet</span>
                      : <span style={{ color: r.introduced_today > 0 ? '#00b894' : 'rgba(180,210,240,0.6)' }}
                          title={r.introduced_as_of ? `as of ${age(r.introduced_as_of)}` : undefined}>
                          {num(r.introduced_today)}
                        </span>}
                  </td>
                  {!showAll && (
                    <td style={tdNum}>{renderReadyCell(r.isp)}</td>
                  )}
                  <td style={td}>
                    <button
                      onClick={() => void toggleHold(r)}
                      disabled={isBusy}
                      title={r.hold_reason ? `Reason: ${r.hold_reason}` : 'Hold is immediate (server-confirmed)'}
                      style={{ background: r.hold ? 'rgba(233,69,96,0.15)' : 'rgba(0,184,148,0.12)',
                               color: r.hold ? '#e94560' : '#00b894',
                               border: `1px solid ${r.hold ? 'rgba(233,69,96,0.4)' : 'rgba(0,184,148,0.4)'}`,
                               borderRadius: 999, padding: '3px 10px', fontSize: 11,
                               fontWeight: 700, cursor: 'pointer' }}>
                      {isBusy ? 'Confirming…' : r.hold ? 'HELD' : 'ACTIVE'}
                    </button>
                  </td>
                  {!showAll && (r.vdm ? (<>
                    <td style={tdNum}>{num(r.vdm.sent)}</td>
                    <td style={{ ...tdNum, color: dead ? '#e94560' : undefined, fontWeight: dead ? 700 : undefined }}>
                      {pct(r.vdm.delivered_pct)}
                    </td>
                    <td style={tdNum} title={`${num(r.vdm.opens)} unique opens`}>{pct(r.vdm.open_rate_vdm)}</td>
                    <td style={tdNum} title={`${num(r.vdm.clicks)} unique clicks`}>{pct(r.vdm.click_rate_vdm)}</td>
                    <td style={tdNum} title={`7d: delivered ${pct(r.vdm.delivered_pct_7d)} · open ${pct(r.vdm.open_rate_vdm_7d)} · click ${pct(r.vdm.click_rate_vdm_7d)}`}>
                      {num(r.vdm.sent_7d)}
                    </td>
                  </>) : (
                    <td style={{ ...tdNum, color: 'rgba(180,210,240,0.4)' }} colSpan={5}
                      title="No complete VDM rows for this lane (VDM covers gmail/yahoo/aol/microsoft/apple/att/cox)">
                      no VDM data
                    </td>
                  ))}
                  <td style={td}>
                    {r.proposal ? (
                      <span>
                        <span style={{ fontSize: 11, color: '#a5b4fc' }}
                          title={`${r.proposal.basis} · proposed ${age(r.proposal.created_at)} · expires ${age(r.proposal.expires_at)}`}>
                          {num(r.proposal.base_budget)} → {num(r.proposal.proposed_budget)}
                        </span>
                        <button onClick={() => void approveProposal(r)} disabled={isBusy} style={{
                          marginLeft: 8, background: 'rgba(99,102,241,0.15)', color: '#a5b4fc',
                          border: '1px solid rgba(99,102,241,0.4)', borderRadius: 5,
                          padding: '2px 8px', fontSize: 10, fontWeight: 700, cursor: 'pointer',
                        }}>Approve</button>
                      </span>
                    ) : <span style={{ color: 'rgba(180,210,240,0.35)', fontSize: 11 }}>—</span>}
                  </td>
                  <td style={{ ...td, fontSize: 11, color: 'rgba(180,210,240,0.6)' }}>
                    {r.approved_by ? `${r.approved_by} · ${age(r.approved_at)}` : '—'}
                  </td>
                </tr>
              );
            })}
            {rows.length === 0 && !loading && (
              <tr><td style={{ ...td, color: 'rgba(180,210,240,0.5)' }} colSpan={showAll ? 7 : 13}>
                {isNonLedgerDomain
                  ? `${domain} is not in the ledger — budgets N/A. The supply strip above is live; ledger enrollment is a separate operator step.`
                  : 'Ledger empty — the P3 seed (operator-executed) populates the 16-property × 14-ISP grid.'}
              </td></tr>
            )}
          </tbody>
        </table>
      </div>

      {!showAll && selectedBrand && (
        <LaneContentPanel brand={selectedBrand} domain={domain ?? ''} onNotice={setNotice} />
      )}

      <p style={{ fontSize: 11, color: 'rgba(180,210,240,0.55)', marginTop: 14, lineHeight: 1.6 }}>
        One row per sending domain × ISP. Drip Intro Daily Cap = first-touch introductions the drip
        may absorb per Denver day (edits are approvals and apply from tomorrow — the counter worker
        promotes them at the boundary). Introductions Today comes from the materialized counters,
        never a live queue scan. Hold zeroes the cell immediately and is interval-tracked.
        Sent / Delivered % / Open / Click pull from the SES VDM daily snapshots (complete UTC days
        only, VDM unique-count semantics); a red lane means delivered % under 50 on meaningful volume.
      </p>
    </div>
  );
};

// ── Supply strip — live pcq tranche anatomy per feed (Cockpit P1+P2) ────────
//
// LIVE queue facts (PG, point-in-time, labeled "live queue") — never
// Observatory fact aggregates. Supply is a DATASET fact shared across the
// rotation's brands; the shared-pool indicator renders on every card so
// dataset supply is never presented as domain-owned inventory.
//
// The supply poll lives in the PARENT (PropertyLedgerView) so the lane
// table's Ready column shares the response; this strip is presentational.
// P2 adds the claim-side throttle beside each feed's ready counts: the
// current partner_isp_distribution_overrides rows + effective posture, and
// — ONLY when the server reports write_enabled (env
// PROPERTY_LEDGER_THROTTLE_WRITE_ENABLED=1) — an edit surface that reuses
// the existing PUT /api/mailing/data-partners/datasets/{id}/isp-distribution
// with a full replacement diff preview and a typed-feed-name confirm.

export const SupplyStrip: React.FC<{
  domain: string;
  supply: PollingState<SupplyResponse | null>;
  throttle: ThrottleResponse | null;
  throttleErr: string | null;
  onThrottleChanged: () => void;
  onNotice: (s: string) => void;
}> = ({ domain, supply, throttle, throttleErr, onThrottleChanged, onNotice }) => {
  const { data, loading, error, secondsSinceUpdate } = supply;
  const warming = !!error && error.startsWith('HTTP 503');
  const [editingFeed, setEditingFeed] = useState<string | null>(null);
  const throttleByDataset = new Map<string, ThrottleFeed>();
  for (const tf of throttle?.feeds ?? []) throttleByDataset.set(tf.dataset_id, tf);

  const label: React.CSSProperties = {
    fontSize: 10, letterSpacing: 0.6, textTransform: 'uppercase', color: 'rgba(180,210,240,0.55)',
  };
  const stat = (title: string, value: number, opts?: { color?: string; muted?: boolean; tip?: string }) => (
    <div style={{ minWidth: 84 }} title={opts?.tip}>
      <div style={label}>{title}</div>
      <div style={{
        fontSize: 17, fontWeight: 700, fontVariantNumeric: 'tabular-nums',
        color: opts?.color ?? (opts?.muted ? 'rgba(180,210,240,0.45)' : '#e6edf5'),
      }}>{num(value)}</div>
    </div>
  );

  return (
    <div style={{ marginBottom: 14, border: '1px solid rgba(0,184,148,0.25)', borderRadius: 10,
                  background: 'rgba(0,184,148,0.04)', padding: '12px 16px' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 8 }}>
        <h3 style={{ margin: 0, fontSize: 14, color: '#e6edf5' }}>Supply</h3>
        <span style={{ fontSize: 10, color: '#00b894', fontWeight: 700, letterSpacing: 0.5,
                       border: '1px solid rgba(0,184,148,0.4)', borderRadius: 4, padding: '1px 6px' }}
          title={data?.supply_note ?? 'Live queue facts (point-in-time), not Observatory aggregates.'}>
          LIVE QUEUE
        </span>
        {data ? (
          <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.6)' }}
            title={data.ready_semantics}>
            {data.sending_domain || data.domain} · {data.brand} · Denver day {data.denver_day}
          </span>
        ) : (
          <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.6)' }}>{domain}</span>
        )}
        <span style={{ marginLeft: 'auto', fontSize: 10, color: 'rgba(180,210,240,0.5)' }}>
          {loading ? 'Loading…' : `updated ${secondsSinceUpdate}s ago · polls 60s`}
        </span>
      </div>
      {warming && (
        <div style={{ color: '#facc15', fontSize: 12, marginBottom: 8 }}>
          Supply view warming up — the covering index is still building (CONCURRENTLY, calm-IO
          windows); retries automatically every 60s.
        </div>
      )}
      {error && !warming && (
        <div style={{ color: '#e94560', fontSize: 12, marginBottom: 8 }}>
          Supply refresh failed ({error}){data ? ' — showing last good data' : ''}
        </div>
      )}
      {throttleErr && (
        <div style={{ color: '#e94560', fontSize: 12, marginBottom: 8 }}>
          Throttle read failed ({throttleErr}) — claim-cap posture unavailable.
        </div>
      )}
      {throttle && !throttle.write_enabled && (
        <div style={{ fontSize: 11, color: '#facc15', marginBottom: 8, fontWeight: 600 }}
          title={throttle.enforcement_note}>
          Throttle editing is DISABLED — the server env {throttle.write_flag_env} is not set
          (HOLD-CRITICAL write path; the operator enables it server-side). Panel is read-only.
        </div>
      )}
      {data && (
        <p style={{ fontSize: 10, color: 'rgba(180,210,240,0.5)', margin: '0 0 10px 0' }}>
          {data.ready_semantics}. Release cap is the SUPPLY-side budget (ready-vs-held) — distinct
          from the claim-side per-ISP caps.
        </p>
      )}
      {data && data.feeds.length === 0 && !loading && (
        <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.5)' }}>
          No active feeds serve this domain (partner_drip_vertical_roster → partner_datasets).
        </div>
      )}
      <div style={{ display: 'flex', gap: 12, flexWrap: 'wrap' }}>
        {data?.feeds.map(f => {
          const maxReady = Math.max(1, ...f.ready_by_isp.map(e => e.ready));
          return (
            <div key={f.dataset_id} style={{ flex: '1 1 420px', maxWidth: 640,
                border: '1px solid rgba(255,255,255,0.08)', borderRadius: 8,
                background: 'rgba(0,0,0,0.15)', padding: '10px 12px' }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap', marginBottom: 6 }}>
                <span style={{ fontSize: 13, fontWeight: 700, color: '#e6edf5' }}>{f.name}</span>
                <span style={{ fontSize: 10, color: 'rgba(180,210,240,0.55)' }}>{f.vertical}</span>
                {f.paused_emergency && (
                  <span style={{ fontSize: 9, color: '#e94560', fontWeight: 700,
                                 border: '1px solid rgba(233,69,96,0.4)', borderRadius: 4, padding: '1px 6px' }}>
                    PAUSED
                  </span>
                )}
                <span style={{ fontSize: 10, color: 'rgba(180,210,240,0.6)' }}
                  title="Supply-release budget (partner_datasets.daily_cap): the release job keeps at most this many rows 'ready' per day, parking the rest 'held'. NOT the claim-side per-ISP cap.">
                  release cap {f.daily_cap > 0 ? `${num(f.daily_cap)}/day` : 'uncapped'}
                </span>
                {f.shared_brands.length > 1 && (
                  <span style={{ marginLeft: 'auto', fontSize: 10, color: '#facc15', fontWeight: 700 }}
                    title={`Supply is a DATASET fact — this tranche feeds the whole rotation, not just this domain: ${f.shared_brands.join(', ')}`}>
                    supply shared across {f.shared_brands.length} rotation brands
                  </span>
                )}
              </div>
              <div style={{ display: 'flex', gap: 18, flexWrap: 'wrap', alignItems: 'flex-start' }}>
                {stat('Tranche total', f.tranche_total, { tip: 'Every record ever ingested into this feed (all statuses)' })}
                {stat('Cleaning', f.cleaning, {
                  color: '#9fd3ff',
                  tip: `Actively being cleaned: ${num(f.pending_eo)} pending_eo + ${num(f.eo_in_flight)} eo_in_flight`,
                })}
                <div style={{ minWidth: 180, flex: '1 1 180px' }}>
                  <div style={label}>Ready</div>
                  <div style={{ fontSize: 17, fontWeight: 700, color: '#00b894', fontVariantNumeric: 'tabular-nums' }}
                    title={data.ready_semantics}>
                    {num(f.ready_total)}
                  </div>
                  {f.ready_by_isp.map(e => (
                    <div key={e.isp} style={{ display: 'flex', alignItems: 'center', gap: 6, padding: '1px 0' }}>
                      <span style={{ fontSize: 10, color: 'rgba(180,210,240,0.7)', width: 68 }}>{e.isp}</span>
                      <div style={{ flex: 1, height: 6, background: 'rgba(255,255,255,0.06)', borderRadius: 3 }}>
                        <div style={{ width: `${Math.max(2, Math.round((e.ready / maxReady) * 100))}%`,
                                      height: 6, background: 'rgba(0,184,148,0.55)', borderRadius: 3 }} />
                      </div>
                      <span style={{ fontSize: 10, color: '#e6edf5', minWidth: 46, textAlign: 'right',
                                     fontVariantNumeric: 'tabular-nums' }}>{num(e.ready)}</span>
                    </div>
                  ))}
                  {f.ready_by_isp.length === 0 && (
                    <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.4)' }}>none ready</div>
                  )}
                </div>
                {(() => {
                  // Claim throttle beside the ready counts (Cockpit P2).
                  const tf = throttleByDataset.get(f.dataset_id);
                  if (!tf || !throttle) return null;
                  return (
                    <div style={{ minWidth: 200, flex: '1 1 200px' }}>
                      <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                        <div style={label} title={throttle.cap_systems_note}>
                          Claim caps (per ISP)
                        </div>
                        {throttle.write_enabled ? (
                          <button
                            onClick={() => setEditingFeed(editingFeed === f.dataset_id ? null : f.dataset_id)}
                            title={throttle.enforcement_note}
                            style={{ background: 'rgba(233,69,96,0.12)', color: '#e94560',
                                     border: '1px solid rgba(233,69,96,0.4)', borderRadius: 5,
                                     padding: '1px 8px', fontSize: 10, fontWeight: 700, cursor: 'pointer' }}>
                            {editingFeed === f.dataset_id ? 'Close editor' : 'Edit throttle'}
                          </button>
                        ) : (
                          <span style={{ fontSize: 9, color: 'rgba(180,210,240,0.45)' }}
                            title={`Read-only: server env ${throttle.write_flag_env} is not set.`}>
                            read-only
                          </span>
                        )}
                      </div>
                      {tf.overrides.length === 0 ? (
                        <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.5)', marginTop: 2 }}
                          title={throttle.enforcement_note}>
                          no overrides — every ISP rides the global defaults
                        </div>
                      ) : tf.overrides.map(ov => (
                        <div key={ov.isp}
                          style={{ display: 'flex', gap: 6, alignItems: 'baseline', padding: '1px 0' }}
                          title={`Live enforcement row — updated ${age(ov.updated_at)}${ov.updated_by ? ` by ${ov.updated_by}` : ''}. ${throttle.enforcement_note}`}>
                          <span style={{ fontSize: 10, color: 'rgba(180,210,240,0.7)', width: 68 }}>{ov.isp}</span>
                          <span style={{ fontSize: 10, color: '#e6edf5', fontVariantNumeric: 'tabular-nums' }}>
                            {ov.max_per_wave > 0 ? `wave ≤${num(ov.max_per_wave)}` : 'wave default'}
                            {' · '}
                            {ov.daily_cap == null ? 'day default'
                              : ov.daily_cap === 0 ? 'day SUPPRESSED'
                              : `day ≤${num(ov.daily_cap)}`}
                            {` · pct ${(ov.pct_override * 100).toFixed(0)}%`}
                          </span>
                        </div>
                      ))}
                      {tf.default_isps.length > 0 && (
                        <div style={{ fontSize: 9, color: 'rgba(180,210,240,0.4)', marginTop: 2 }}
                          title={`On global defaults (no override row): ${tf.default_isps.join(', ')}`}>
                          {tf.default_isps.length} ISP{tf.default_isps.length === 1 ? '' : 's'} on global defaults
                        </div>
                      )}
                    </div>
                  );
                })()}
                {stat('Held', f.held, {
                  color: '#facc15',
                  tip: "Reservoir parked by the release job (over the day's release cap) — cleaned, waiting for release",
                })}
                {stat('Suppressed', f.suppressed, { muted: true, tip: 'suppressed_eo — EO rejected' })}
                {stat('Dead letter', f.dead_letter, { muted: true, tip: 'dead_letter — EO retries exhausted' })}
                {stat('Mailed today', f.mailed_today, { tip: `Denver day ${data.denver_day} · lifetime ${num(f.mailed_lifetime)}` })}
              </div>
              {throttle?.write_enabled && editingFeed === f.dataset_id && (() => {
                const tf = throttleByDataset.get(f.dataset_id);
                if (!tf) return null;
                return (
                  <ThrottleEditor feed={tf} note={throttle.replacement_note}
                    onClose={() => setEditingFeed(null)}
                    onSaved={() => { setEditingFeed(null); onThrottleChanged(); }}
                    onNotice={onNotice} />
                );
              })()}
            </div>
          );
        })}
      </div>
    </div>
  );
};

// ── Throttle editor (Cockpit P2, HOLD-CRITICAL write path) ──────────────────
//
// Writes through the EXISTING PUT
// /api/mailing/data-partners/datasets/{id}/isp-distribution (delete-and-
// replace; audit-logged server-side) — never a second writer. Offer-swap-
// grade ceremony: a rendered current→proposed diff (replacement semantics
// spelled out per row) and a typed-feed-name confirm; a mismatch NEVER
// submits. Only reachable when the server reports write_enabled.

interface ThrottleEditRow { isp: string; pct: string; wave: string; day: string; }

const throttleEditRowsFromFeed = (feed: ThrottleFeed): ThrottleEditRow[] =>
  feed.overrides.map(ov => ({
    isp: ov.isp,
    pct: String(ov.pct_override),
    wave: ov.max_per_wave > 0 ? String(ov.max_per_wave) : '',
    day: ov.daily_cap == null ? '' : String(ov.daily_cap),
  }));

const parseThrottleRows = (rows: ThrottleEditRow[]): ThrottleRow[] =>
  rows.map(r => ({
    isp: r.isp,
    pct_override: r.pct.trim() === '' ? NaN : Number(r.pct),
    max_per_wave: r.wave.trim() === '' ? 0 : Number(r.wave),
    daily_cap: r.day.trim() === '' ? null : Number(r.day),
  }));

const currentThrottleRows = (feed: ThrottleFeed): ThrottleRow[] =>
  feed.overrides.map(ov => ({
    isp: ov.isp,
    pct_override: ov.pct_override,
    max_per_wave: ov.max_per_wave,
    daily_cap: ov.daily_cap == null ? null : ov.daily_cap,
  }));

const ThrottleEditor: React.FC<{
  feed: ThrottleFeed;
  note: string;
  onClose: () => void;
  onSaved: () => void;
  onNotice: (s: string) => void;
}> = ({ feed, note, onClose, onSaved, onNotice }) => {
  const [rows, setRows] = useState<ThrottleEditRow[]>(() => throttleEditRowsFromFeed(feed));
  const [addISP, setAddISP] = useState('');
  const [busy, setBusy] = useState(false);

  const proposed = parseThrottleRows(rows);
  const errors = validateThrottleRows(proposed);
  const diff = errors.length === 0 ? buildThrottleDiff(currentThrottleRows(feed), proposed) : [];
  const changes = diff.filter(d => d.kind !== 'unchanged');

  const availableISPs = feed.default_isps.filter(g => !rows.some(r => r.isp === g));

  const input: React.CSSProperties = {
    background: 'rgba(255,255,255,0.06)', color: '#e6edf5',
    border: '1px solid rgba(255,255,255,0.15)', borderRadius: 5,
    padding: '3px 6px', fontSize: 11, width: 80,
  };
  const smallBtn: React.CSSProperties = {
    background: 'rgba(99,102,241,0.15)', color: '#a5b4fc',
    border: '1px solid rgba(99,102,241,0.4)', borderRadius: 5,
    padding: '2px 8px', fontSize: 10, fontWeight: 700, cursor: 'pointer',
  };
  const diffColor: Record<string, string> = {
    added: '#00b894', removed: '#e94560', changed: '#facc15', unchanged: 'rgba(180,210,240,0.45)',
  };

  const submit = async () => {
    if (errors.length > 0 || busy) return;
    // Same gravity as the offer swap: type the feed name to proceed. A
    // mismatch NEVER submits (client-side hard stop).
    const typed = window.prompt(
      `THROTTLE REPLACE changes LIVE claim routing for this feed on the next orchestrator wave.\n\n${note}\n\nType the feed name exactly to confirm:\n${feed.name}`);
    if (typed === null) return;
    if (typed !== feed.name) {
      onNotice('Throttle write cancelled — feed name did not match.');
      return;
    }
    setBusy(true);
    try {
      const r = await apiFetch(
        `/api/mailing/data-partners/datasets/${feed.dataset_id}/isp-distribution`,
        { method: 'PUT', body: JSON.stringify(buildThrottlePayload(proposed)) });
      let json: Record<string, unknown> = {};
      try { json = await r.json(); } catch { /* non-JSON error body */ }
      if (!r.ok) {
        onNotice(`Throttle write failed: ${String(json.error ?? r.status)}`);
        return;
      }
      onNotice(`Throttle replaced for "${feed.name}" — ${String(json.override_count ?? proposed.length)} override row(s) now live (next wave picks them up).`);
      onSaved();
    } finally { setBusy(false); }
  };

  return (
    <div style={{ marginTop: 10, borderTop: '1px solid rgba(233,69,96,0.25)', paddingTop: 8 }}>
      <div style={{ fontSize: 10, color: '#facc15', fontWeight: 600, marginBottom: 6 }}>
        {note}
      </div>
      {rows.map((r, i) => (
        <div key={r.isp} style={{ display: 'flex', gap: 8, alignItems: 'center', padding: '2px 0', flexWrap: 'wrap' }}>
          <span style={{ fontSize: 11, color: '#e6edf5', width: 80, fontWeight: 600 }}>{r.isp}</span>
          <label style={{ fontSize: 9, color: 'rgba(180,210,240,0.55)' }}>
            pct (0–1){' '}
            <input style={input} value={r.pct} placeholder="0.4"
              onChange={e => setRows(s => s.map((x, j) => j === i ? { ...x, pct: e.target.value } : x))} />
          </label>
          <label style={{ fontSize: 9, color: 'rgba(180,210,240,0.55)' }}>
            per-wave cap{' '}
            <input style={input} value={r.wave} placeholder="default"
              onChange={e => setRows(s => s.map((x, j) => j === i ? { ...x, wave: e.target.value } : x))} />
          </label>
          <label style={{ fontSize: 9, color: 'rgba(180,210,240,0.55)' }}
            title="Empty = keep this ISP's existing lane daily budget (server preserves it). 0 = hard-suppress the ISP for this lane.">
            daily cap (empty = keep){' '}
            <input style={input} value={r.day} placeholder="keep"
              onChange={e => setRows(s => s.map((x, j) => j === i ? { ...x, day: e.target.value } : x))} />
          </label>
          <button style={{ ...smallBtn, background: 'rgba(233,69,96,0.12)', color: '#e94560',
                           border: '1px solid rgba(233,69,96,0.4)' }}
            title="Removes this ISP's row entirely — it falls back to the global defaults and its lane daily budget is deleted."
            onClick={() => setRows(s => s.filter((_, j) => j !== i))}>
            Remove
          </button>
        </div>
      ))}
      <div style={{ display: 'flex', gap: 6, alignItems: 'center', marginTop: 4 }}>
        <select value={addISP} onChange={e => setAddISP(e.target.value)}
          style={{ ...input, width: 130 }}>
          <option value="">add ISP…</option>
          {availableISPs.map(g => <option key={g} value={g}>{g}</option>)}
        </select>
        <button style={smallBtn} disabled={addISP === ''}
          onClick={() => {
            if (addISP === '') return;
            setRows(s => [...s, { isp: addISP, pct: '0', wave: '', day: '' }]);
            setAddISP('');
          }}>
          Add override
        </button>
      </div>
      {errors.length > 0 && (
        <div style={{ marginTop: 6 }}>
          {errors.map((e, i) => (
            <div key={i} style={{ fontSize: 10, color: '#e94560', fontWeight: 600 }}>{e}</div>
          ))}
        </div>
      )}
      {errors.length === 0 && (
        <div style={{ marginTop: 6 }}>
          <div style={{ fontSize: 10, letterSpacing: 0.6, textTransform: 'uppercase',
                        color: 'rgba(180,210,240,0.55)' }}>
            Change preview (current → proposed)
          </div>
          {changes.length === 0 && (
            <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.45)' }}>no changes</div>
          )}
          {changes.map(d => (
            <div key={d.isp} style={{ fontSize: 10, color: diffColor[d.kind], padding: '1px 0' }}>
              <b>{d.isp}</b> — {d.kind}{d.notes.length > 0 ? `: ${d.notes.join('; ')}` : ''}
            </div>
          ))}
        </div>
      )}
      <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
        <button style={{ ...smallBtn, background: 'rgba(233,69,96,0.15)', color: '#e94560',
                         border: '1px solid rgba(233,69,96,0.4)' }}
          disabled={busy || errors.length > 0 || changes.length === 0}
          onClick={() => void submit()}>
          {busy ? 'Replacing…' : 'Replace throttle…'}
        </button>
        <button style={{ ...smallBtn, background: 'transparent', color: 'rgba(180,210,240,0.6)',
                         border: '1px solid rgba(255,255,255,0.15)' }}
          disabled={busy} onClick={onClose}>
          Cancel
        </button>
      </div>
    </div>
  );
};

// ── Lane content panel — what this lane sends, editable in-screen ───────────

const LaneContentPanel: React.FC<{
  brand: string; domain: string; onNotice: (s: string) => void;
}> = ({ brand, domain, onNotice }) => {
  const [content, setContent] = useState<LaneContentResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [open, setOpen] = useState<Record<string, boolean>>({});
  const [newSubject, setNewSubject] = useState<Record<string, string>>({});
  const [touchEdit, setTouchEdit] = useState<Record<string, { subject_line: string; preheader: string; from_name: string }>>({});
  const [swapSel, setSwapSel] = useState<Record<string, string>>({});
  const [saving, setSaving] = useState<Record<string, boolean>>({});
  // Creative preview (operator addendum): fetched ONLY on click (the HTML is
  // ~65KB per creative — never in the list payload), rendered in a sandboxed
  // iframe (srcdoc, sandbox="" — no scripts), keyed by creative id.
  const [preview, setPreview] = useState<Record<string, {
    open: boolean; loading: boolean; html?: string; err?: string;
  }>>({});

  const loadContent = useCallback(async () => {
    setLoading(true); setErr(null);
    try {
      const r = await apiFetch(`/api/mailing/pmta-campaign/property-ledger/lane-content?brand=${encodeURIComponent(brand)}`);
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      setContent(await r.json());
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'load failed');
    } finally { setLoading(false); }
  }, [brand]);

  useEffect(() => { setContent(null); setOpen({}); setTouchEdit({}); void loadContent(); }, [loadContent]);

  const post = async (path: string, body: Record<string, unknown>): Promise<{ ok: boolean; status: number; json: Record<string, unknown> }> => {
    const r = await apiFetch(`/api/mailing/pmta-campaign/property-ledger/lane-content/${path}`, {
      method: 'POST', body: JSON.stringify(body),
    });
    let json: Record<string, unknown> = {};
    try { json = await r.json(); } catch { /* non-JSON error body */ }
    return { ok: r.ok, status: r.status, json };
  };

  const withSaving = async (key: string, fn: () => Promise<void>) => {
    setSaving(s => ({ ...s, [key]: true }));
    try { await fn(); } finally { setSaving(s => ({ ...s, [key]: false })); }
  };

  const addSubject = (offer: LaneOffer, dsKey: string) =>
    withSaving(`add/${dsKey}`, async () => {
      const text = (newSubject[dsKey] ?? '').trim();
      if (!text) return;
      const res = await post('subject', { offer_id: offer.id, action: 'add', subject_line: text });
      if (!res.ok) { onNotice(`Subject add failed: ${String(res.json.error ?? res.status)}`); return; }
      setNewSubject(s => ({ ...s, [dsKey]: '' }));
      await loadContent();
    });

  const archiveSubject = (offer: LaneOffer, row: LaneCopyRow) =>
    withSaving(`arch/${row.id}`, async () => {
      if (!window.confirm(`Archive this subject from "${offer.name}"?\n\n${row.text}`)) return;
      const res = await post('subject', { offer_id: offer.id, action: 'archive', subject_id: row.id });
      if (!res.ok) { onNotice(`Subject archive failed: ${String(res.json.error ?? res.status)}`); return; }
      await loadContent();
    });

  const touchKey = (t: LaneTouchCopy) => `${t.vertical}/${t.touch}`;

  const saveTouch = (t: LaneTouchCopy) =>
    withSaving(`touch/${touchKey(t)}`, async () => {
      const e = touchEdit[touchKey(t)];
      if (!e) return;
      const body: Record<string, unknown> = { vertical: t.vertical, brand, touch: t.touch };
      if (e.subject_line !== t.subject_line) body.subject_line = e.subject_line;
      if (e.preheader !== t.preheader) body.preheader = e.preheader;
      if (e.from_name !== t.from_name) body.from_name = e.from_name;
      if (body.subject_line === undefined && body.preheader === undefined && body.from_name === undefined) {
        setTouchEdit(s => { const n = { ...s }; delete n[touchKey(t)]; return n; });
        return;
      }
      const res = await post('touch-copy', body);
      if (!res.ok) { onNotice(`Touch copy update failed: ${String(res.json.error ?? res.status)}`); return; }
      setTouchEdit(s => { const n = { ...s }; delete n[touchKey(t)]; return n; });
      await loadContent();
    });

  const swapOffer = (ds: LaneDataset) =>
    withSaving(`swap/${ds.id}`, async () => {
      const offerId = swapSel[ds.id];
      if (!offerId || offerId === (ds.offer?.id ?? '')) return;
      // Same gravity as the hold confirm: type the feed name to proceed.
      const typed = window.prompt(
        `OFFER SWAP changes what this feed mails.\n\nType the feed name exactly to confirm:\n${ds.name}`);
      if (typed === null) return;
      if (typed !== ds.name) { onNotice('Offer swap cancelled — feed name did not match.'); return; }
      const res = await post('offer-swap', { dataset_id: ds.id, offer_id: offerId, confirm_name: typed });
      if (!res.ok) { onNotice(`Offer swap failed: ${String(res.json.error ?? res.status)}`); return; }
      onNotice(`Feed "${ds.name}" now mails ${String(res.json.offer_name ?? offerId)}.`);
      setSwapSel(s => { const n = { ...s }; delete n[ds.id]; return n; });
      await loadContent();
    });

  const panel: React.CSSProperties = {
    marginTop: 18, border: '1px solid rgba(99,102,241,0.25)', borderRadius: 10,
    background: 'rgba(99,102,241,0.05)', padding: '14px 16px',
  };
  const label: React.CSSProperties = {
    fontSize: 10, letterSpacing: 0.6, textTransform: 'uppercase', color: 'rgba(180,210,240,0.55)',
  };
  const input: React.CSSProperties = {
    background: 'rgba(255,255,255,0.06)', color: '#e6edf5',
    border: '1px solid rgba(255,255,255,0.15)', borderRadius: 5,
    padding: '4px 6px', fontSize: 12,
  };
  const smallBtn: React.CSSProperties = {
    background: 'rgba(99,102,241,0.15)', color: '#a5b4fc',
    border: '1px solid rgba(99,102,241,0.4)', borderRadius: 5,
    padding: '3px 10px', fontSize: 11, fontWeight: 700, cursor: 'pointer',
  };

  // Shared pools renderer — used by dataset-bound AND touch-level offers so
  // both get the identical edit surface. poolKey scopes the add-input state.
  const renderOfferPools = (offer: LaneOffer, poolKey: string) => (
    <div style={{ display: 'flex', gap: 24, flexWrap: 'wrap', marginTop: 6 }}>
      <div style={{ flex: '2 1 340px' }}>
        <div style={label}>Serving subjects (preheader draws from this pool)</div>
        {offer.sharing && offer.sharing.feeds > 1 && (
          <div style={{ fontSize: 10, color: '#facc15', fontWeight: 700, margin: '2px 0 4px 0' }}
            title="Subject/from-name pools live on the OFFER — every feed bound to this offer (dataset- or touch-level) rotates through the same pool.">
            Pool shared by {offer.sharing.feeds} feeds across {offer.sharing.brands} brand{offer.sharing.brands === 1 ? '' : 's'} — edits here change live rotation on all of them.
          </div>
        )}
        {offer.subjects.filter(sr => sr.serving).map(sr => (
          <div key={sr.id} style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '2px 0' }}>
            <span style={{ fontSize: 12, color: '#e6edf5' }}>{sr.text}</span>
            <button style={{ ...smallBtn, padding: '1px 7px', fontSize: 9 }}
              disabled={!!saving[`arch/${sr.id}`]}
              onClick={() => void archiveSubject(offer, sr)}>
              {saving[`arch/${sr.id}`] ? '…' : 'Archive'}
            </button>
          </div>
        ))}
        {offer.subjects.filter(sr => sr.serving).length === 0 && (
          <div style={{ fontSize: 11, color: '#e94560', fontWeight: 700 }}>
            EMPTY SUBJECT POOL — the lane fails at send time
          </div>
        )}
        <div style={{ display: 'flex', gap: 6, marginTop: 4 }}>
          <input style={{ ...input, flex: 1 }} placeholder="Add a subject line…"
            value={newSubject[poolKey] ?? ''}
            onChange={e => setNewSubject(s => ({ ...s, [poolKey]: e.target.value }))} />
          <button style={smallBtn} disabled={!!saving[`add/${poolKey}`] || !(newSubject[poolKey] ?? '').trim()}
            onClick={() => void addSubject(offer, poolKey)}>
            {saving[`add/${poolKey}`] ? 'Adding…' : 'Add'}
          </button>
        </div>
        {offer.subjects.some(sr => !sr.serving) && (
          <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.4)', marginTop: 4 }}>
            {offer.subjects.filter(sr => !sr.serving).length} archived
          </div>
        )}
      </div>
      <div style={{ flex: '1 1 200px' }}>
        <div style={label}>Serving from-names</div>
        {offer.from_names.filter(fr => fr.serving).map(fr => (
          <div key={fr.id} style={{ fontSize: 12, color: '#e6edf5', padding: '2px 0' }}>{fr.text}</div>
        ))}
        {offer.from_names.filter(fr => fr.serving).length === 0 && (
          <div style={{ fontSize: 11, color: '#e94560', fontWeight: 700 }}>
            EMPTY FROM-NAME POOL — the lane fails at send time
          </div>
        )}
      </div>
    </div>
  );

  const togglePreview = (creative: LaneCreative) => {
    const cur = preview[creative.id];
    const opening = !(cur?.open);
    setPreview(s => ({ ...s, [creative.id]: { ...(s[creative.id] ?? { loading: false }), open: opening } }));
    // Fetch once, on first open only — the panel list never carries HTML.
    if (opening && cur?.html === undefined && !cur?.loading) {
      setPreview(s => ({ ...s, [creative.id]: { open: true, loading: true } }));
      void (async () => {
        try {
          const r = await apiFetch(
            `/api/mailing/pmta-campaign/property-ledger/lane-content/creative?id=${encodeURIComponent(creative.id)}`);
          if (!r.ok) throw new Error(`HTTP ${r.status}`);
          const j = await r.json();
          setPreview(s => ({ ...s, [creative.id]: { open: true, loading: false, html: String(j.html ?? '') } }));
        } catch (e) {
          setPreview(s => ({ ...s, [creative.id]: {
            open: true, loading: false, err: e instanceof Error ? e.message : 'preview fetch failed',
          } }));
        }
      })();
    }
  };

  const renderCreativeBadge = (offer: LaneOffer) =>
    offer.creative ? (
      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
        <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.7)' }}
          title={`${num(offer.creative.html_bytes)} bytes · id ${offer.creative.id}`}>
          creative v{offer.creative.version} ({offer.creative.status}) · {age(offer.creative.updated_at)}
        </span>
        <button style={smallBtn}
          title="Fetches the creative HTML and renders it sandboxed (no scripts). Loaded only when opened."
          onClick={() => togglePreview(offer.creative!)}>
          {preview[offer.creative.id]?.open ? '▾ Preview' : '▸ Preview'}
        </button>
      </span>
    ) : (
      <span style={{ fontSize: 11, color: '#e94560', fontWeight: 700 }}>NO SERVING CREATIVE</span>
    );

  const renderCreativePreview = (offer: LaneOffer) => {
    const creative = offer.creative;
    if (!creative) return null;
    const p = preview[creative.id];
    if (!p?.open) return null;
    return (
      <div style={{ marginTop: 8 }}>
        {p.loading && (
          <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)' }}>Loading preview…</div>
        )}
        {p.err && (
          <div style={{ fontSize: 11, color: '#e94560' }}>Preview failed ({p.err})</div>
        )}
        {p.html !== undefined && !p.loading && !p.err && (
          <iframe
            title={`Creative preview — ${offer.name || creative.id}`}
            sandbox=""
            srcDoc={p.html}
            style={{ width: '100%', height: 480, border: '1px solid rgba(255,255,255,0.12)',
                     borderRadius: 8, background: '#ffffff' }} />
        )}
      </div>
    );
  };

  const renderTouchRow = (t: LaneTouchCopy) => {
    const key = touchKey(t);
    const e = touchEdit[key];
    const isSaving = !!saving[`touch/${key}`];
    return (
      <div key={key} style={{ display: 'flex', gap: 10, alignItems: 'center', flexWrap: 'wrap',
                              padding: '6px 0', borderTop: '1px solid rgba(255,255,255,0.06)' }}>
        <span style={{ fontSize: 11, color: '#a5b4fc', fontWeight: 700, minWidth: 34 }}>
          T{t.touch}{t.vertical === '' ? ' (global)' : ''}
        </span>
        <span style={{ fontSize: 10, color: 'rgba(180,210,240,0.5)', minWidth: 110 }}>
          {t.creative_filename === '' ? "uses the offer's creative" : t.creative_filename}
        </span>
        {e ? (
          <>
            <input style={{ ...input, flex: '2 1 220px' }} value={e.subject_line} placeholder="subject"
              onChange={ev => setTouchEdit(s => ({ ...s, [key]: { ...e, subject_line: ev.target.value } }))} />
            <input style={{ ...input, flex: '2 1 180px' }} value={e.preheader} placeholder="preheader"
              onChange={ev => setTouchEdit(s => ({ ...s, [key]: { ...e, preheader: ev.target.value } }))} />
            <input style={{ ...input, flex: '1 1 130px' }} value={e.from_name} placeholder="from name"
              onChange={ev => setTouchEdit(s => ({ ...s, [key]: { ...e, from_name: ev.target.value } }))} />
            <button style={smallBtn} disabled={isSaving} onClick={() => void saveTouch(t)}>
              {isSaving ? 'Saving…' : 'Save'}
            </button>
            <button style={{ ...smallBtn, background: 'transparent', color: 'rgba(180,210,240,0.6)',
                             border: '1px solid rgba(255,255,255,0.15)' }}
              onClick={() => setTouchEdit(s => { const n = { ...s }; delete n[key]; return n; })}>
              Cancel
            </button>
          </>
        ) : (
          <>
            <span style={{ fontSize: 12, color: '#e6edf5', flex: '2 1 220px' }} title="subject">{t.subject_line || '—'}</span>
            <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', flex: '2 1 180px' }} title="preheader">{t.preheader || '—'}</span>
            <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.8)', flex: '1 1 130px' }} title="from name">{t.from_name || '—'}</span>
            <button style={smallBtn}
              onClick={() => setTouchEdit(s => ({ ...s, [key]: {
                subject_line: t.subject_line, preheader: t.preheader, from_name: t.from_name } }))}>
              Edit
            </button>
          </>
        )}
      </div>
    );
  };

  return (
    <div style={panel}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 4 }}>
        <h3 style={{ margin: 0, fontSize: 14, color: '#e6edf5' }}>What this lane sends</h3>
        <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.6)' }}>{domain} · {brand}</span>
        {loading && <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)' }}>Loading…</span>}
      </div>
      {err && <div style={{ color: '#e94560', fontSize: 12, marginBottom: 8 }}>{err}</div>}
      {content && (
        <p style={{ fontSize: 10, color: 'rgba(180,210,240,0.5)', margin: '0 0 10px 0' }}>
          {content.preheader_note}
        </p>
      )}
      {content && content.feeds.length === 0 && !loading && (
        <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.5)' }}>
          No active feeds on this brand (partner_drip_vertical_roster).
        </div>
      )}
      {content?.feeds.map(feed => {
        const expanded = !!open[feed.vertical];
        return (
          <div key={feed.vertical} style={{ border: '1px solid rgba(255,255,255,0.08)', borderRadius: 8,
                                            marginBottom: 10, background: 'rgba(0,0,0,0.15)' }}>
            <div onClick={() => setOpen(s => ({ ...s, [feed.vertical]: !expanded }))}
              style={{ display: 'flex', alignItems: 'center', gap: 12, padding: '8px 12px', cursor: 'pointer' }}>
              <span style={{ fontSize: 12, color: '#9fd3ff' }}>{expanded ? '▾' : '▸'}</span>
              <span style={{ fontSize: 13, fontWeight: 700, color: '#e6edf5' }}>{feed.vertical}</span>
              <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.55)' }}>
                {feed.datasets.length} feed{feed.datasets.length === 1 ? '' : 's'}
                {(() => {
                  // Summary covers BOTH binding paths: dataset offers and
                  // touch-level offers (NULL-offer datasets, QA SEV-2).
                  const names = [
                    ...feed.datasets.filter(d => d.offer).map(d => d.offer!.name || d.offer!.id),
                    ...feed.touch_offers.filter(t => t.offer).map(t => `${t.offer!.name || t.offer!.id} (touch-level)`),
                  ].filter((v, i, a) => a.indexOf(v) === i);
                  return names.length > 0 ? ` · offer: ${names.join(', ')}` : '';
                })()}
              </span>
            </div>
            {expanded && (
              <div style={{ padding: '0 12px 10px 34px' }}>
                {feed.datasets.map(ds => {
                  const isSwapping = !!saving[`swap/${ds.id}`];
                  return (
                    <div key={ds.id} style={{ padding: '8px 0', borderTop: '1px solid rgba(255,255,255,0.06)' }}>
                      <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
                        <span style={{ fontSize: 12, fontWeight: 600, color: '#e6edf5' }}>{ds.name}</span>
                        {ds.express_dispatch && <span style={{ fontSize: 9, color: '#facc15', fontWeight: 700 }}>EXPRESS</span>}
                        {ds.offer ? (
                          <span style={{ fontSize: 11, color: '#a5b4fc' }}>
                            offer: <b>{ds.offer.name || ds.offer.id}</b> ({ds.offer.status || '?'})
                          </span>
                        ) : (
                          <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)' }}>
                            no dataset-level offer — see touch bindings below
                          </span>
                        )}
                        {ds.offer && renderCreativeBadge(ds.offer)}
                        <span style={{ marginLeft: 'auto', display: 'flex', gap: 6, alignItems: 'center' }}>
                          <select value={swapSel[ds.id] ?? (ds.offer?.id ?? '')} disabled={isSwapping}
                            onChange={e => setSwapSel(s => ({ ...s, [ds.id]: e.target.value }))}
                            style={{ ...input, maxWidth: 220 }}>
                            {!ds.offer && <option value="">(no offer)</option>}
                            {content.active_offers.map(o => <option key={o.id} value={o.id}>{o.name}</option>)}
                          </select>
                          <button style={smallBtn}
                            disabled={isSwapping || !swapSel[ds.id] || swapSel[ds.id] === (ds.offer?.id ?? '')}
                            onClick={() => void swapOffer(ds)}
                            title="Changes what this feed mails — requires typing the feed name to confirm">
                            {isSwapping ? 'Swapping…' : 'Swap offer'}
                          </button>
                        </span>
                      </div>
                      {ds.offer && renderOfferPools(ds.offer, ds.id)}
                      {ds.offer && renderCreativePreview(ds.offer)}
                    </div>
                  );
                })}
                {feed.touch_offers.map(to => to.offer ? (
                  <div key={`${feed.vertical}/to/${to.offer.id}`}
                    style={{ padding: '8px 0', borderTop: '1px solid rgba(255,255,255,0.06)' }}>
                    <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
                      <span title="This offer is set on individual touches of the ladder, not on the feed itself — feeds without a feed-level offer still mail it."
                        style={{ fontSize: 9, color: '#a5b4fc', fontWeight: 700, letterSpacing: 0.5,
                                 border: '1px solid rgba(99,102,241,0.4)', borderRadius: 4, padding: '1px 6px' }}>
                        OFFER SET PER TOUCH · {to.touch_scope.join(', ')}
                      </span>
                      <span style={{ fontSize: 11, color: '#a5b4fc' }}>
                        offer: <b>{to.offer.name || to.offer.id}</b> ({to.offer.status || '?'})
                      </span>
                      {renderCreativeBadge(to.offer)}
                    </div>
                    {renderOfferPools(to.offer, `${feed.vertical}/${to.offer.id}`)}
                    {renderCreativePreview(to.offer)}
                  </div>
                ) : null)}
                {(feed.touch1 || feed.followups.length > 0) && (
                  <div style={{ marginTop: 8 }}>
                    <div style={label}>Touch copy (this brand's ladder rows)</div>
                    {feed.touch1 && renderTouchRow(feed.touch1)}
                    {feed.followups.map(renderTouchRow)}
                  </div>
                )}
              </div>
            )}
          </div>
        );
      })}
      {content && content.global_followups.length > 0 && (
        <div style={{ marginTop: 6 }}>
          <div style={label}
            title="These rows are shared: any feed on this brand without its own touch-specific copy mails these. Editing them changes every feed that falls back here.">
            Fallback offers — mailed when this feed has no touch-specific copy (shared across all feeds)
          </div>
          {content.global_followups.map(renderTouchRow)}
        </div>
      )}
    </div>
  );
};

export default PropertyLedgerView;
