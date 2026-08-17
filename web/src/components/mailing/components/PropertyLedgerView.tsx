// Property Ledger — per (sending domain × ISP) drip-introduction budgets
// (Vector A plan rev4, Step 19).
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

import React, { useCallback, useEffect, useState } from 'react';
import { apiFetch } from '../shared/apiFetch';

interface Proposal {
  id: string;
  proposed_budget: number;
  base_budget: number;
  basis: string;
  created_at: string;
  expires_at: string;
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
  global_hold: GlobalHold;
  runs: { counter: RunInfo | null; vdm: RunInfo | null; reconciliation: RunInfo | null };
  alerts_enabled: boolean;
}

interface CoverageRow { brand: string; isp: string; status: string; observed_sends: number; }

const num = (n: number | null | undefined) => (n ?? 0).toLocaleString();

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

export const PropertyLedgerView: React.FC = () => {
  const [data, setData] = useState<LedgerResponse | null>(null);
  const [coverage, setCoverage] = useState<CoverageRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [edit, setEdit] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState<Record<string, boolean>>({});
  const [ghBusy, setGhBusy] = useState(false);

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
  const banner = (bg: string, border: string, color: string): React.CSSProperties => ({
    background: bg, border: `1px solid ${border}`, color, borderRadius: 8,
    padding: '10px 14px', fontSize: 13, marginBottom: 12, fontWeight: 600,
  });

  const rows = data?.rows ?? [];
  const proposalCount = rows.filter(r => r.proposal).length;
  const runs = data?.runs;

  return (
    <div style={{ padding: 20 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 14, marginBottom: 6 }}>
        <h2 style={{ margin: 0, fontSize: 18, color: '#e6edf5' }}>Property Ledger</h2>
        <span style={{ fontSize: 12, color: 'rgba(180,210,240,0.7)' }}>
          {rows.length} cells · day {data?.day ?? '—'}
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

      <div style={{ overflowX: 'auto' }}>
        <table style={{ borderCollapse: 'collapse', width: '100%', minWidth: 1000 }}>
          <thead><tr>
            <th style={th}>Property</th>
            <th style={th}>ISP</th>
            <th style={{ ...th, textAlign: 'right' }}>Drip Intro Daily Cap</th>
            <th style={{ ...th, textAlign: 'right' }}>Introductions Today</th>
            <th style={th}>Drip Intro Hold</th>
            <th style={th}>Proposal</th>
            <th style={th}>Approved</th>
          </tr></thead>
          <tbody>
            {rows.map(r => {
              const key = cellKey(r);
              const isBusy = !!busy[key];
              return (
                <tr key={key} style={{ opacity: r.hold || data?.global_hold.value ? 0.55 : 1 }}>
                  <td style={{ ...td, fontWeight: 600 }}>
                    {r.sending_domain || r.brand}
                    <span style={{ color: 'rgba(180,210,240,0.5)', marginLeft: 6, fontWeight: 400 }}>{r.brand}</span>
                  </td>
                  <td style={{ ...td, color: 'rgba(180,210,240,0.75)' }}>{r.isp}</td>
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
              <tr><td style={{ ...td, color: 'rgba(180,210,240,0.5)' }} colSpan={7}>
                Ledger empty — the P3 seed (operator-executed) populates the 16-property × 14-ISP grid.
              </td></tr>
            )}
          </tbody>
        </table>
      </div>

      <p style={{ fontSize: 11, color: 'rgba(180,210,240,0.55)', marginTop: 14, lineHeight: 1.6 }}>
        One row per sending domain × ISP. Drip Intro Daily Cap = first-touch introductions the drip
        may absorb per Denver day (edits are approvals and apply from tomorrow — the counter worker
        promotes them at the boundary). Introductions Today comes from the materialized counters,
        never a live queue scan. Hold zeroes the cell immediately and is interval-tracked.
      </p>
    </div>
  );
};

export default PropertyLedgerView;
