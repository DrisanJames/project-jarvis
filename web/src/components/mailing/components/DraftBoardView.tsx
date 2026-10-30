// Draft Board — sending-domain-centric view of "what we're mailing".
//
// The mental model (operator spec, 2026-06-19): everything stems from the
// SENDING DOMAIN. A domain encapsulates the campaigns it is mailing, each tied
// to a send time (MT). Per campaign we surface creative · subject · preheader,
// and a breakdown by our logical ISPs of the volume target / cap — i.e. volume
// targets by ISP, by sending domain. Pure read view over existing endpoints:
//   GET /api/mailing/campaigns?status=&limit=          (roster + time + audience)
//   GET /api/mailing/pmta-campaign/{id}/edit-data       (per-ISP quotas + creative)
//
// No new backend, no draft persistence — it reads whatever is staged/scheduled
// for the selected day and organises it top-down.

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { useAuth } from '../../../contexts/AuthContext';
import { ALL_TARGET_ISPS } from './send-day-planner/constants';

const DEFAULT_ORG = '00000000-0000-0000-0000-000000000001';

// Logical ISP display labels (canonical order from the send-day planner).
const ISP_LABEL: Record<string, string> = {
  microsoft: 'Microsoft', gmail: 'Gmail', apple: 'Apple', yahoo: 'Yahoo',
  comcast: 'Comcast', charter: 'Charter', att: 'AT&T', aol: 'AOL',
  cox: 'Cox', sbcglobal: 'SBCGlobal', other: 'Other',
};
const ISP_ORDER: string[] = [...ALL_TARGET_ISPS];

interface CampaignRow {
  id: string;
  name: string;
  subject: string;
  status: string;
  total_recipients: number;
  from_email: string;
  from_name: string;
  scheduled_at: string | null;
  created_at: string;
  preview_text: string;
  profile_name?: string;
  segment_names?: string[];
  list_names?: string[];
}

interface IspQuota { isp: string; volume: number; }
// GET /api/mailing/pmta-campaign/{id}/isp-volume → {isps:[...]} — real per-ISP plan
// from mailing_campaign_isp_plans (planned = audience_selected_count; quota 0 = uncapped).
interface IspVolumeRow { isp: string; planned: number; sent: number; quota: number; estimated: number; }
interface Variant { subject?: string; preview_text?: string; from_name?: string; html_content?: string; }
interface EditData {
  sending_domain?: string;
  target_isps?: string[];
  isp_quotas?: IspQuota[];
  variants?: Variant[];
}

const STATUS_PALETTE: Record<string, { bg: string; bd: string; fg: string }> = {
  draft:      { bg: 'rgba(120,120,120,0.12)', bd: 'rgba(120,120,120,0.4)', fg: 'var(--j-text-muted)' },
  scheduled:  { bg: 'color-mix(in srgb, var(--j-blue) 12%, transparent)',   bd: 'color-mix(in srgb, var(--j-blue) 45%, transparent)', fg: 'var(--j-blue)' },
  preparing:  { bg: 'rgba(250,204,21,0.12)',  bd: 'rgba(250,204,21,0.45)', fg: 'var(--j-warn)' },
  finalizing_audience: { bg: 'rgba(250,204,21,0.12)', bd: 'rgba(250,204,21,0.45)', fg: 'var(--j-warn)' },
  sending:    { bg: 'color-mix(in srgb, var(--j-violet) 14%, transparent)',  bd: 'color-mix(in srgb, var(--j-violet) 50%, transparent)',  fg: 'var(--j-violet)' },
  sent:       { bg: 'color-mix(in srgb, var(--j-success) 12%, transparent)',   bd: 'color-mix(in srgb, var(--j-success) 45%, transparent)', fg: 'var(--j-success)' },
  completed:  { bg: 'color-mix(in srgb, var(--j-success) 12%, transparent)',   bd: 'color-mix(in srgb, var(--j-success) 45%, transparent)', fg: 'var(--j-success)' },
  failed:     { bg: 'color-mix(in srgb, var(--j-danger) 12%, transparent)',   bd: 'color-mix(in srgb, var(--j-danger) 50%, transparent)',  fg: 'var(--j-danger)' },
  cancelled:  { bg: 'rgba(180,180,180,0.10)', bd: 'rgba(180,180,180,0.4)', fg: '#bdbdbd' },
};

const StatusPill: React.FC<{ status: string }> = ({ status }) => {
  const p = STATUS_PALETTE[status] ?? { bg: 'rgba(120,120,120,0.12)', bd: 'rgba(120,120,120,0.4)', fg: 'var(--j-text-muted)' };
  return (
    <span style={{
      display: 'inline-block', padding: '2px 8px', borderRadius: 999,
      background: p.bg, border: `1px solid ${p.bd}`, color: p.fg,
      fontSize: 10, fontWeight: 700, letterSpacing: 0.5, whiteSpace: 'nowrap',
    }}>{(status || 'unknown').toUpperCase().replace(/_/g, ' ')}</span>
  );
};

const TZ = 'America/Denver';
const domainOf = (email: string): string => (email && email.includes('@') ? email.split('@')[1] : '(no domain)').toLowerCase();
const mtDate = (iso: string | null): string => {
  if (!iso) return '';
  try { return new Date(iso).toLocaleDateString('en-CA', { timeZone: TZ }); } catch { return ''; }
};
const mtTime = (iso: string | null): string => {
  if (!iso) return '—';
  try { return new Date(iso).toLocaleString('en-US', { timeZone: TZ, hour: 'numeric', minute: '2-digit', hour12: true }); }
  catch { return '—'; }
};
const num = (n: number): string => (n || 0).toLocaleString();

const STATUS_OPTIONS = ['all', 'draft', 'scheduled', 'sending', 'sent', 'completed', 'cancelled', 'failed'];

export const DraftBoardView: React.FC = () => {
  const { organization } = useAuth();
  const orgId = organization?.id ?? DEFAULT_ORG;

  const todayMT = useMemo(() => {
    try { return new Date().toLocaleDateString('en-CA', { timeZone: TZ }); } catch { return ''; }
  }, []);

  const [date, setDate] = useState<string>(todayMT);
  const [statusFilter, setStatusFilter] = useState<string>('all');
  const [includeDrip, setIncludeDrip] = useState<boolean>(false);
  const [rows, setRows] = useState<CampaignRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [details, setDetails] = useState<Record<string, EditData | 'loading' | 'error'>>({});
  // Per-campaign real ISP plan (isp-volume). 'error' fails SOFT: the IspTable simply
  // renders without the Planned column, exactly as before this feature existed.
  const [ispVol, setIspVol] = useState<Record<string, IspVolumeRow[] | 'loading' | 'error'>>({});
  // Per-campaign action state for approve/disapprove (in-flight + inline error/success).
  // failedGates carries the server's REQ-007 gate verdicts when a deploy was
  // blocked (HTTP 412 {failed_gates}) so the board can offer the override path.
  const [action, setAction] = useState<Record<string, { busy: boolean; error?: string; ok?: string; failedGates?: string[] }>>({});
  const [bulkBusy, setBulkBusy] = useState(false);
  // Operator reason for overriding red send-day gates — required non-empty by
  // the server, audit-logged. Shown only AFTER an approve was gate-blocked.
  const [overrideReason, setOverrideReason] = useState('');

  const headers = useMemo(() => ({ 'X-Organization-ID': orgId }), [orgId]);

  const load = useCallback(async () => {
    setLoading(true); setError(null);
    try {
      // Server-side filter to the selected MT send date + drip exclusion, so we
      // fetch exactly the day's plan rather than paging the whole campaign table.
      const qs = `&scheduled_date=${date}`
        + (statusFilter === 'all' ? '' : `&status=${encodeURIComponent(statusFilter)}`)
        + (includeDrip ? '' : '&exclude_drip=true');
      // The endpoint paginates by `page` (200/page). Walk pages until the server
      // says there are no more (a day can exceed 200 with ISP-split campaigns).
      const PAGE = 200;
      const acc: CampaignRow[] = [];
      for (let page = 1; page <= 30; page++) {
        const r = await fetch(`/api/mailing/campaigns?limit=${PAGE}&page=${page}${qs}`, { headers });
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        const j = await r.json();
        const items: CampaignRow[] = Array.isArray(j) ? j : (j.data ?? j.campaigns ?? j.items ?? []);
        acc.push(...items);
        const pg = j.pagination;
        if (items.length < PAGE || (pg && pg.has_more === false) || (pg && page >= pg.total_pages)) break;
      }
      setRows(acc);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'failed to load campaigns');
      setRows([]);
    } finally {
      setLoading(false);
    }
  }, [headers, statusFilter, includeDrip, date]);

  useEffect(() => { void load(); }, [load]);

  const fetchDetail = useCallback(async (id: string) => {
    setDetails(prev => (prev[id] && prev[id] !== 'error' ? prev : { ...prev, [id]: 'loading' }));
    try {
      const r = await fetch(`/api/mailing/pmta-campaign/${id}/edit-data`, { headers });
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      const j = await r.json();
      const ci: EditData = j.campaign_input ?? j;
      setDetails(prev => ({ ...prev, [id]: ci }));
    } catch {
      setDetails(prev => ({ ...prev, [id]: 'error' }));
    }
  }, [headers]);

  const fetchIspVolume = useCallback(async (id: string) => {
    setIspVol(prev => (prev[id] && prev[id] !== 'error' ? prev : { ...prev, [id]: 'loading' }));
    try {
      const r = await fetch(`/api/mailing/pmta-campaign/${id}/isp-volume`, { headers });
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      const j = await r.json();
      const isps: IspVolumeRow[] = Array.isArray(j.isps) ? j.isps : [];
      setIspVol(prev => ({ ...prev, [id]: isps }));
    } catch {
      setIspVol(prev => ({ ...prev, [id]: 'error' }));
    }
  }, [headers]);

  // APPROVE — fetch the draft's full config, then redeploy carrying its existing
  // campaign_id so the backend promotes the draft row in place (draft →
  // finalizing_audience → scheduled) rather than creating a duplicate.
  const approveCampaign = useCallback(async (id: string, gateOverride?: string): Promise<boolean> => {
    setAction(prev => ({ ...prev, [id]: { busy: true } }));
    try {
      const er = await fetch(`/api/mailing/pmta-campaign/${id}/edit-data`, { headers });
      if (!er.ok) throw new Error(`edit-data HTTP ${er.status}`);
      const ej = await er.json();
      const input = ej.campaign_input ?? ej;
      if (!input || typeof input !== 'object') throw new Error('no campaign_input in edit-data');
      // edit-data STRIPS campaign_id out of campaign_input (handlers_pmta_campaign.go
      // delete(inputMap,"campaign_id")) and returns it as a separate top-level field.
      // Re-attach the real id so the deploy path promotes THIS draft in place — without
      // it the server falls back to "the org's newest draft" and approves the WRONG one.
      (input as Record<string, unknown>).campaign_id = ej.campaign_id || id;
      // Explicit operator override past red send-day gates — the server
      // requires a non-empty reason and audit-logs every use (REQ-007).
      if (gateOverride) (input as Record<string, unknown>).gate_override = { reason: gateOverride };
      const dr = await fetch('/api/mailing/pmta-campaign/deploy', {
        method: 'POST',
        headers: { ...headers, 'Content-Type': 'application/json' },
        body: JSON.stringify(input),
      });
      if (!dr.ok) {
        let msg = `deploy HTTP ${dr.status}`;
        let failedGates: string[] | undefined;
        try {
          const dj = await dr.json();
          if (dj?.error) msg = dj.error;
          if (Array.isArray(dj?.failed_gates)) {
            failedGates = (dj.failed_gates as Array<{ gate?: string }>)
              .map(g => g?.gate ?? '').filter(Boolean);
          }
        } catch { /* keep status */ }
        setAction(prev => ({ ...prev, [id]: { busy: false, error: msg, failedGates } }));
        return false;
      }
      setAction(prev => ({ ...prev, [id]: { busy: false, ok: 'Approved' } }));
      return true;
    } catch (e) {
      setAction(prev => ({ ...prev, [id]: { busy: false, error: e instanceof Error ? e.message : 'approve failed' } }));
      return false;
    }
  }, [headers]);

  // A draft whose scheduled time has already passed would fire IMMEDIATELY (the
  // wave scheduler dispatches scheduled_at <= NOW()), blasting the whole campaign
  // with no throttle window. Block approval until it's re-staged with a future time.
  const isPastDue = useCallback((id: string): boolean => {
    const c = rows.find(r => r.id === id);
    if (!c?.scheduled_at) return false;
    const t = new Date(c.scheduled_at).getTime();
    return !Number.isNaN(t) && t <= Date.now();
  }, [rows]);

  const handleApprove = useCallback(async (id: string) => {
    if (isPastDue(id)) {
      setAction(prev => ({ ...prev, [id]: { busy: false, error: 'Scheduled time has passed — re-stage with a future time before approving (would send immediately, no throttle).' } }));
      return;
    }
    if (!window.confirm('Approve & schedule this campaign?')) return;
    const ok = await approveCampaign(id);
    if (ok) await load();
  }, [approveCampaign, load, isPastDue]);

  // DISAPPROVE — soft-cancel the draft (status → cancelled).
  const handleDisapprove = useCallback(async (id: string) => {
    if (!window.confirm('Discard this draft?')) return;
    setAction(prev => ({ ...prev, [id]: { busy: true } }));
    try {
      const r = await fetch(`/api/mailing/campaigns/${id}/cancel`, { method: 'POST', headers });
      if (!r.ok) {
        let msg = `cancel HTTP ${r.status}`;
        try { const j = await r.json(); if (j?.error) msg = j.error; } catch { /* keep status */ }
        throw new Error(msg);
      }
      setAction(prev => ({ ...prev, [id]: { busy: false, ok: 'Discarded' } }));
      await load();
    } catch (e) {
      setAction(prev => ({ ...prev, [id]: { busy: false, error: e instanceof Error ? e.message : 'cancel failed' } }));
    }
  }, [headers, load]);

  const toggle = useCallback((id: string) => {
    setExpanded(prev => {
      const next = new Set(prev);
      if (next.has(id)) { next.delete(id); }
      else {
        next.add(id);
        if (!details[id] || details[id] === 'error') void fetchDetail(id);
        if (!ispVol[id] || ispVol[id] === 'error') void fetchIspVolume(id);
      }
      return next;
    });
  }, [details, fetchDetail, ispVol, fetchIspVolume]);

  // Filter to the selected MT day, then group by sending domain.
  // Bucket by scheduled_at ONLY — that's the campaign's actual MST send time.
  // Unscheduled campaigns (no scheduled_at) aren't "being mailed at a time",
  // so they don't belong under a send date (using created_at would pull in
  // other days' drafts created today).
  const grouped = useMemo(() => {
    const inDay = rows.filter(c => mtDate(c.scheduled_at) === date);
    const byDomain = new Map<string, CampaignRow[]>();
    for (const c of inDay) {
      const d = domainOf(c.from_email);
      if (!byDomain.has(d)) byDomain.set(d, []);
      byDomain.get(d)!.push(c);
    }
    const out = Array.from(byDomain.entries()).map(([domain, camps]) => ({
      domain,
      campaigns: camps.slice().sort((a, b) =>
        (a.scheduled_at || a.created_at).localeCompare(b.scheduled_at || b.created_at)),
      audience: camps.reduce((s, c) => s + (c.total_recipients || 0), 0),
    }));
    out.sort((a, b) => a.domain.localeCompare(b.domain));
    return out;
  }, [rows, date]);

  const totals = useMemo(() => ({
    domains: grouped.length,
    campaigns: grouped.reduce((s, g) => s + g.campaigns.length, 0),
    audience: grouped.reduce((s, g) => s + g.audience, 0),
  }), [grouped]);

  // Draft campaign ids currently in view (for the whole-board "Approve all").
  const draftIds = useMemo(
    () => grouped.flatMap(g => g.campaigns).filter(c => c.status === 'draft').map(c => c.id),
    [grouped],
  );

  // Drafts whose last approve attempt was blocked server-side by red send-day
  // gates (HTTP 412 + failed_gates, REQ-007), plus the union of gate letters.
  // The override banner renders only when this is non-empty — i.e. only after
  // a failed attempt, never pre-emptively.
  const gateBlocked = useMemo(() => {
    const ids = draftIds.filter(id => (action[id]?.failedGates?.length ?? 0) > 0);
    const gates = new Set<string>();
    ids.forEach(id => (action[id]?.failedGates ?? []).forEach(g => gates.add(g)));
    return { ids, gates: Array.from(gates).sort() };
  }, [draftIds, action]);

  const handleOverrideApprove = useCallback(async () => {
    const reason = overrideReason.trim();
    if (!reason || gateBlocked.ids.length === 0) return;
    if (!window.confirm(
      `Override red send-day gate${gateBlocked.gates.length === 1 ? '' : 's'} (${gateBlocked.gates.join(', ')}) and approve ${gateBlocked.ids.length} draft${gateBlocked.ids.length === 1 ? '' : 's'}?\nThe override is audit-logged with your reason.`,
    )) return;
    setBulkBusy(true);
    try {
      // Sequential, like handleApproveAll — avoids hammering the deploy path.
      for (const id of gateBlocked.ids) {
        await approveCampaign(id, reason);
      }
      setOverrideReason('');
      await load();
    } finally {
      setBulkBusy(false);
    }
  }, [overrideReason, gateBlocked, approveCampaign, load]);

  const handleApproveAll = useCallback(async () => {
    if (draftIds.length === 0) return;
    const eligible = draftIds.filter(id => !isPastDue(id));
    const skipped = draftIds.length - eligible.length;
    if (eligible.length === 0) {
      window.alert('All drafts in view have a past scheduled time — re-stage with future times before approving.');
      return;
    }
    const msg = `Approve & schedule ${eligible.length} draft${eligible.length === 1 ? '' : 's'} in view?`
      + (skipped ? `\n(${skipped} skipped — scheduled time already passed.)` : '');
    if (!window.confirm(msg)) return;
    setBulkBusy(true);
    try {
      // Sequential — promotes each draft in place; avoids hammering the deploy path.
      for (const id of eligible) {
        await approveCampaign(id);
      }
      await load();
    } finally {
      setBulkBusy(false);
    }
  }, [draftIds, approveCampaign, load, isPastDue]);

  const previewCreative = useCallback((html?: string) => {
    if (!html) return;
    const blob = new Blob([html], { type: 'text/html' });
    const url = URL.createObjectURL(blob);
    window.open(url, '_blank', 'noopener');
    setTimeout(() => URL.revokeObjectURL(url), 60_000);
  }, []);

  return (
    <div style={{ padding: 18, color: 'color-mix(in srgb, var(--j-text) 92%, transparent)' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 14, flexWrap: 'wrap', marginBottom: 14 }}>
        <h2 style={{ margin: 0, fontSize: 18, fontWeight: 700 }}>Draft Board · by Sending Domain</h2>
        <input
          type="date" value={date} onChange={e => setDate(e.target.value)}
          style={{
            background: 'color-mix(in srgb, var(--j-shade) 90%, transparent)', color: 'color-mix(in srgb, var(--j-text) 95%, transparent)',
            border: '1px solid color-mix(in srgb, var(--j-cyan) 25%, transparent)', borderRadius: 6, padding: '6px 10px', fontSize: 13,
          }}
        />
        <select
          value={statusFilter} onChange={e => setStatusFilter(e.target.value)}
          style={{
            background: 'color-mix(in srgb, var(--j-shade) 90%, transparent)', color: 'color-mix(in srgb, var(--j-text) 95%, transparent)',
            border: '1px solid color-mix(in srgb, var(--j-cyan) 25%, transparent)', borderRadius: 6, padding: '6px 10px', fontSize: 13,
          }}
        >
          {STATUS_OPTIONS.map(s => <option key={s} value={s}>{s === 'all' ? 'All statuses' : s}</option>)}
        </select>
        <label style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 12, color: 'color-mix(in srgb, var(--j-text) 75%, transparent)', cursor: 'pointer' }}>
          <input type="checkbox" checked={includeDrip} onChange={e => setIncludeDrip(e.target.checked)} />
          Include automated follow-ups
        </label>
        <button onClick={() => void load()} style={ghostBtn}>{loading ? 'Loading…' : 'Refresh'}</button>
        <button
          onClick={() => {
            // Week export (operator 2026-08-03): CSV of all draft+scheduled
            // campaigns in the next 7 days — one row per campaign.
            void (async () => {
              const r = await fetch('/api/mailing/pmta-campaign/board-export?days=7', { headers });
              if (!r.ok) return;
              const blob = await r.blob();
              const a = document.createElement('a');
              a.href = URL.createObjectURL(blob);
              a.download = `board-week-${new Date().toISOString().slice(0, 10)}.csv`;
              a.click();
              URL.revokeObjectURL(a.href);
            })();
          }}
          style={ghostBtn}
        >
          Export week (CSV)
        </button>
        {draftIds.length > 0 && (
          <button
            onClick={() => void handleApproveAll()}
            disabled={bulkBusy}
            style={{ ...approveBtn, opacity: bulkBusy ? 0.6 : 1, cursor: bulkBusy ? 'default' : 'pointer' }}
          >
            {bulkBusy ? 'Approving…' : `✓ Approve all drafts (${draftIds.length})`}
          </button>
        )}
        <div style={{ marginLeft: 'auto', fontSize: 12, color: 'color-mix(in srgb, var(--j-text) 75%, transparent)' }}>
          {totals.domains} domains · {totals.campaigns} campaigns · <span style={{ color: 'var(--j-cyan)', fontWeight: 600 }}>{num(totals.audience)}</span> planned sends
        </div>
      </div>

      {error && (
        <div style={{ padding: 12, borderRadius: 8, background: 'color-mix(in srgb, var(--j-danger) 8%, transparent)', border: '1px solid color-mix(in srgb, var(--j-danger) 40%, transparent)', color: 'var(--j-danger)', fontSize: 13, marginBottom: 12 }}>
          {error}
        </div>
      )}

      {/* Gate-blocked drafts (server 412 failed_gates) — override requires an
          explicit, audit-logged reason. Appears only after a blocked attempt. */}
      {gateBlocked.ids.length > 0 && (
        <div style={{ padding: 12, borderRadius: 8, background: 'rgba(245,158,11,0.08)', border: '1px solid rgba(245,158,11,0.45)', fontSize: 13, marginBottom: 12, display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
          <span style={{ color: '#f59e0b', fontWeight: 600 }}>
            ⛔ {gateBlocked.ids.length} draft{gateBlocked.ids.length === 1 ? '' : 's'} blocked by red send-day gate{gateBlocked.gates.length === 1 ? '' : 's'}: {gateBlocked.gates.join(', ')}
          </span>
          <span style={{ color: 'color-mix(in srgb, var(--j-text) 75%, transparent)' }}>Fix the gate(s) on the Send Day tab, or override with a reason (audit-logged):</span>
          <input
            value={overrideReason}
            onChange={e => setOverrideReason(e.target.value)}
            placeholder="Override reason (required)"
            style={{ flex: 1, minWidth: 220, background: 'color-mix(in srgb, var(--j-shade) 90%, transparent)', color: 'color-mix(in srgb, var(--j-text) 95%, transparent)', border: '1px solid rgba(245,158,11,0.35)', borderRadius: 6, padding: '6px 10px', fontSize: 13 }}
          />
          <button
            onClick={() => void handleOverrideApprove()}
            disabled={bulkBusy || !overrideReason.trim()}
            style={{ background: 'rgba(245,158,11,0.12)', color: '#f59e0b', border: '1px solid rgba(245,158,11,0.45)', padding: '6px 12px', borderRadius: 6, fontSize: 12, fontWeight: 600, opacity: (bulkBusy || !overrideReason.trim()) ? 0.5 : 1, cursor: (bulkBusy || !overrideReason.trim()) ? 'default' : 'pointer' }}
          >
            {bulkBusy ? 'Approving…' : `Override gates & approve (${gateBlocked.ids.length})`}
          </button>
        </div>
      )}

      {!loading && !error && grouped.length === 0 && (
        <div style={{ padding: 24, textAlign: 'center', color: 'color-mix(in srgb, var(--j-text) 55%, transparent)', fontSize: 13 }}>
          No campaigns for {date} (MT){statusFilter !== 'all' ? ` with status "${statusFilter}"` : ''}.
        </div>
      )}

      <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
        {grouped.map(group => (
          <div key={group.domain} style={{
            background: 'color-mix(in srgb, var(--j-shade) 70%, transparent)', border: '1px solid color-mix(in srgb, var(--j-cyan) 15%, transparent)', borderRadius: 10, overflow: 'hidden',
          }}>
            {/* Domain header */}
            <div style={{
              display: 'flex', alignItems: 'center', gap: 12, padding: '10px 14px',
              background: 'linear-gradient(135deg, color-mix(in srgb, var(--j-violet-strong) 16%, transparent), color-mix(in srgb, var(--j-violet-strong) 12%, transparent))',
              borderBottom: '1px solid color-mix(in srgb, var(--j-cyan) 12%, transparent)',
            }}>
              <span style={{ fontSize: 15, fontWeight: 700, color: 'var(--j-text)' }}>{group.domain}</span>
              <span style={{ fontSize: 11, color: 'color-mix(in srgb, var(--j-text) 70%, transparent)' }}>{group.campaigns.length} campaign{group.campaigns.length === 1 ? '' : 's'}</span>
              <span style={{ marginLeft: 'auto', fontSize: 12, color: 'var(--j-cyan)', fontWeight: 600 }}>{num(group.audience)} planned sends</span>
            </div>

            {/* Campaign rows */}
            <div style={{ display: 'flex', flexDirection: 'column' }}>
              {group.campaigns.map(c => {
                const isOpen = expanded.has(c.id);
                const det = details[c.id];
                return (
                  <div key={c.id} style={{ borderTop: '1px solid color-mix(in srgb, var(--j-text) 4%, transparent)' }}>
                    <button
                      onClick={() => toggle(c.id)}
                      style={{
                        width: '100%', display: 'flex', alignItems: 'center', gap: 12, padding: '10px 14px',
                        background: isOpen ? 'color-mix(in srgb, var(--j-cyan) 4%, transparent)' : 'transparent', border: 'none', cursor: 'pointer',
                        textAlign: 'left', color: 'inherit',
                      }}
                    >
                      <span style={{ width: 86, fontSize: 12, fontWeight: 600, color: 'var(--j-text)' }}>
                        {mtTime(c.scheduled_at || c.created_at)} <span style={{ color: 'color-mix(in srgb, var(--j-text) 50%, transparent)' }}>MT</span>
                      </span>
                      <StatusPill status={c.status} />
                      <span style={{ flex: 1, minWidth: 0 }}>
                        <span style={{ display: 'block', fontSize: 13, fontWeight: 600, color: 'color-mix(in srgb, var(--j-text) 95%, transparent)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                          {c.subject || '(no subject)'}
                        </span>
                        <span style={{ display: 'block', fontSize: 11, color: 'color-mix(in srgb, var(--j-text) 50%, transparent)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                          {c.name}
                        </span>
                      </span>
                      <span style={{ fontSize: 13, fontWeight: 600, color: 'var(--j-cyan)', whiteSpace: 'nowrap' }}>{num(c.total_recipients)}</span>
                      <span style={{ fontSize: 12, color: 'color-mix(in srgb, var(--j-text) 60%, transparent)', width: 14, textAlign: 'center' }}>{isOpen ? '▾' : '▸'}</span>
                    </button>

                    {isOpen && (
                      <div style={{ padding: '4px 14px 16px 100px' }}>
                        {/* Creative · subject · preheader */}
                        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 18, marginBottom: 12, fontSize: 12 }}>
                          <Field label="Subject" value={det && det !== 'loading' && det !== 'error' ? (det.variants?.[0]?.subject || c.subject) : c.subject} />
                          <Field label="Preheader" value={det && det !== 'loading' && det !== 'error' ? (det.variants?.[0]?.preview_text || c.preview_text || '—') : (c.preview_text || '—')} />
                          <Field label="From" value={`${c.from_name || '—'} <${c.from_email}>`} />
                          {det && det !== 'loading' && det !== 'error' && det.variants?.[0]?.html_content && (
                            <div>
                              <div style={fieldLabel}>Creative</div>
                              <button onClick={() => previewCreative(det.variants?.[0]?.html_content)} style={linkBtn}>
                                Preview HTML ({Math.round((det.variants[0].html_content!.length) / 1024)} KB) ↗
                              </button>
                            </div>
                          )}
                        </div>

                        {/* WHICH audience this campaign mails — segment/list names from the
                            roster (GET /campaigns emits segment_names + list_names per row). */}
                        <div style={{ marginBottom: 12 }}>
                          <div style={fieldLabel}>Audience</div>
                          {((c.segment_names?.length ?? 0) > 0 || (c.list_names?.length ?? 0) > 0) ? (
                            <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 6 }}>
                              {(c.segment_names ?? []).map(nm => (
                                <span key={`seg-${nm}`} style={segmentChip}>{nm}</span>
                              ))}
                              {(c.list_names?.length ?? 0) > 0 && (
                                <>
                                  <span style={{ ...fieldLabel, marginBottom: 0 }}>Lists:</span>
                                  {(c.list_names ?? []).map(nm => (
                                    <span key={`list-${nm}`} style={listChip}>{nm}</span>
                                  ))}
                                </>
                              )}
                            </div>
                          ) : (
                            <div style={{ display: 'inline-block', padding: '3px 10px', borderRadius: 6, background: 'rgba(245,158,11,0.08)', border: '1px solid rgba(245,158,11,0.45)', color: '#f59e0b', fontSize: 12, fontWeight: 600 }}>
                              ⚠ No named audience recorded
                            </div>
                          )}
                        </div>

                        {/* Per-ISP volume targets / caps */}
                        <div style={fieldLabel}>Volume targets by mailbox provider</div>
                        {det === 'loading' && <div style={{ fontSize: 12, color: 'color-mix(in srgb, var(--j-text) 55%, transparent)' }}>Loading mailbox provider plan…</div>}
                        {det === 'error' && <div style={{ fontSize: 12, color: 'var(--j-danger)' }}>Could not load mailbox provider plan for this campaign.</div>}
                        {det && det !== 'loading' && det !== 'error' && (() => {
                          const iv = ispVol[c.id];
                          return (
                            <IspTable
                              det={det}
                              totalAudience={c.total_recipients}
                              vol={Array.isArray(iv) ? iv : undefined}
                            />
                          );
                        })()}

                        {/* Approve / Disapprove — only for staged drafts. */}
                        {c.status === 'draft' && (() => {
                          const a = action[c.id];
                          return (
                            <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginTop: 14, flexWrap: 'wrap' }}>
                              <button
                                onClick={() => void handleApprove(c.id)}
                                disabled={a?.busy || bulkBusy}
                                style={{ ...approveBtn, opacity: (a?.busy || bulkBusy) ? 0.6 : 1, cursor: (a?.busy || bulkBusy) ? 'default' : 'pointer' }}
                              >
                                {a?.busy ? 'Approving…' : '✓ Approve & schedule'}
                              </button>
                              <button
                                onClick={() => void handleDisapprove(c.id)}
                                disabled={a?.busy || bulkBusy}
                                style={{ ...disapproveBtn, opacity: (a?.busy || bulkBusy) ? 0.6 : 1, cursor: (a?.busy || bulkBusy) ? 'default' : 'pointer' }}
                              >
                                ✕ Disapprove
                              </button>
                              {a?.error && <span style={{ fontSize: 12, color: 'var(--j-danger)' }}>{a.error}</span>}
                              {a?.ok && !a?.error && <span style={{ fontSize: 12, color: 'var(--j-success)', fontWeight: 600 }}>{a.ok} ✓</span>}
                            </div>
                          );
                        })()}
                      </div>
                    )}
                  </div>
                );
              })}
            </div>
          </div>
        ))}
      </div>

      <div style={{ marginTop: 18, fontSize: 10, color: 'color-mix(in srgb, var(--j-text) 35%, transparent)' }}>
        Draft Board v1.3 · audience segments/lists shown per campaign; per-provider Planned = planner-selected recipients (mailing_campaign_isp_plans) · send-day gates enforced server-side on Approve (red gate → blocked; override requires an audit-logged reason) · domain → campaign(MT) → volume targets per mailbox provider · "planned sends" = cumulative recipients across a domain's campaigns/send batches (a member in multiple send batches counts each time, so it exceeds unique reach) · times in America/Denver (MT)
      </div>
    </div>
  );
};

const IspTable: React.FC<{ det: EditData; totalAudience: number; vol?: IspVolumeRow[] }> = ({ det, totalAudience, vol }) => {
  const quotaByIsp = new Map<string, number>();
  (det.isp_quotas || []).forEach(q => quotaByIsp.set(q.isp, q.volume));
  // Real per-ISP planner-selected recipients (mailing_campaign_isp_plans). Absent
  // (fetch failed / no plan rows yet) → the table renders exactly as before.
  const plannedByIsp = new Map<string, number>();
  (vol ?? []).forEach(v => { if (v && v.isp) plannedByIsp.set(String(v.isp).toLowerCase(), v.planned || 0); });
  const hasPlanned = plannedByIsp.size > 0;
  // The ISPs this campaign actually targets (∪ ISPs with a real plan row), canonical order.
  const targeted = (det.target_isps && det.target_isps.length ? det.target_isps : (det.isp_quotas || []).map(q => q.isp));
  const shown = new Set<string>([...targeted, ...plannedByIsp.keys()]);
  const ordered = ISP_ORDER.filter(i => shown.has(i)).concat([...shown].filter(i => !ISP_ORDER.includes(i)));
  const anyCap = (det.isp_quotas || []).some(q => (q.volume || 0) > 0);
  const plannedSum = [...plannedByIsp.values()].reduce((s, n) => s + n, 0);

  return (
    <table style={{ borderCollapse: 'collapse', fontSize: 12, marginTop: 6, minWidth: 320 }}>
      <thead>
        <tr style={{ color: 'color-mix(in srgb, var(--j-text) 60%, transparent)', textAlign: 'left' }}>
          <th style={thCell}>Mailbox provider</th>
          {hasPlanned && (
            <th style={{ ...thCell, textAlign: 'right' }} title="Planner-selected recipients per provider (mailing_campaign_isp_plans.audience_selected_count)">
              Planned
            </th>
          )}
          <th style={{ ...thCell, textAlign: 'right' }}>{anyCap ? 'Volume target / cap' : 'Cap'}</th>
        </tr>
      </thead>
      <tbody>
        {ordered.map(isp => {
          const v = quotaByIsp.get(isp) ?? 0;
          const p = plannedByIsp.get(isp);
          return (
            <tr key={isp} style={{ borderTop: '1px solid color-mix(in srgb, var(--j-text) 5%, transparent)' }}>
              <td style={tdCell}>{ISP_LABEL[isp] || isp}</td>
              {hasPlanned && (
                <td style={{ ...tdCell, textAlign: 'right', color: (p ?? 0) > 0 ? 'var(--j-cyan)' : 'color-mix(in srgb, var(--j-text) 55%, transparent)', fontWeight: (p ?? 0) > 0 ? 600 : 400 }}>
                  {p === undefined ? '—' : num(p)}
                </td>
              )}
              <td style={{ ...tdCell, textAlign: 'right', color: v > 0 ? 'var(--j-cyan)' : 'color-mix(in srgb, var(--j-text) 55%, transparent)', fontWeight: v > 0 ? 600 : 400 }}>
                {v > 0 ? num(v) : 'Uncapped'}
              </td>
            </tr>
          );
        })}
        <tr style={{ borderTop: '1px solid color-mix(in srgb, var(--j-cyan) 18%, transparent)' }}>
          <td style={{ ...tdCell, fontWeight: 700 }}>Audience (planned)</td>
          {hasPlanned ? (
            <>
              <td
                style={{ ...tdCell, textAlign: 'right', color: 'var(--j-cyan)', fontWeight: 700 }}
                title={`Σ planner-selected per provider. Campaign total_recipients: ${num(totalAudience)}`}
              >
                {num(plannedSum)}
              </td>
              <td style={tdCell} />
            </>
          ) : (
            <td style={{ ...tdCell, textAlign: 'right', color: 'var(--j-cyan)', fontWeight: 700 }}>{num(totalAudience)}</td>
          )}
        </tr>
      </tbody>
    </table>
  );
};

const Field: React.FC<{ label: string; value: string }> = ({ label, value }) => (
  <div style={{ minWidth: 0, maxWidth: 360 }}>
    <div style={fieldLabel}>{label}</div>
    <div style={{ fontSize: 12, color: 'color-mix(in srgb, var(--j-text) 90%, transparent)', overflowWrap: 'anywhere' }}>{value}</div>
  </div>
);

const fieldLabel: React.CSSProperties = { fontSize: 10, fontWeight: 700, letterSpacing: 0.6, textTransform: 'uppercase', color: 'color-mix(in srgb, var(--j-text) 50%, transparent)', marginBottom: 3 };
const segmentChip: React.CSSProperties = { display: 'inline-block', padding: '2px 8px', borderRadius: 999, background: 'color-mix(in srgb, var(--j-violet-strong) 14%, transparent)', border: '1px solid color-mix(in srgb, var(--j-violet-strong) 45%, transparent)', color: 'var(--j-violet)', fontSize: 11, fontWeight: 600, whiteSpace: 'nowrap' };
const listChip: React.CSSProperties = { ...segmentChip, background: 'color-mix(in srgb, var(--j-violet) 12%, transparent)', border: '1px solid color-mix(in srgb, var(--j-violet) 45%, transparent)', color: 'var(--j-violet)' };
const thCell: React.CSSProperties = { padding: '4px 14px 4px 0', fontWeight: 600 };
const tdCell: React.CSSProperties = { padding: '4px 14px 4px 0', color: 'color-mix(in srgb, var(--j-text) 90%, transparent)' };
const ghostBtn: React.CSSProperties = { background: 'color-mix(in srgb, var(--j-blue) 12%, transparent)', color: 'var(--j-blue)', border: '1px solid color-mix(in srgb, var(--j-blue) 35%, transparent)', padding: '6px 12px', borderRadius: 6, fontSize: 12, fontWeight: 600, cursor: 'pointer' };
const linkBtn: React.CSSProperties = { background: 'transparent', color: 'var(--j-blue)', border: 'none', padding: 0, fontSize: 12, fontWeight: 600, cursor: 'pointer', textDecoration: 'underline' };
const approveBtn: React.CSSProperties = { background: 'color-mix(in srgb, var(--j-success) 14%, transparent)', color: 'var(--j-success)', border: '1px solid color-mix(in srgb, var(--j-success) 45%, transparent)', padding: '6px 12px', borderRadius: 6, fontSize: 12, fontWeight: 600, cursor: 'pointer' };
const disapproveBtn: React.CSSProperties = { background: 'color-mix(in srgb, var(--j-danger) 12%, transparent)', color: 'var(--j-danger)', border: '1px solid color-mix(in srgb, var(--j-danger) 45%, transparent)', padding: '6px 12px', borderRadius: 6, fontSize: 12, fontWeight: 600, cursor: 'pointer' };

export default DraftBoardView;
