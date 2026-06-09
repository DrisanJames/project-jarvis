// Promoted analytics tabs — Phase D companion to AnalyticsCenter.tsx.
//
// Renders the 6 new tabs (Deliverability, Engagement, Offers, Audience,
// Operations, Reports) introduced by the Metrics Consistency & Analytics
// Restructure plan. The original content remains in AnalyticsCenter.tsx
// as the "Overview" tab; this file owns everything else.
//
// Each tab fetches its own slice of data on mount and on range change.
// Loading + empty + error states are explicit so the operator never
// stares at a frozen blank panel.

import React, { useEffect, useState, useCallback, useMemo } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faSpinner, faSync, faExclamationTriangle, faDownload,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';

export const PAGE_VERSION_ANALYTICS_TABS = '1.1'; // SES-aware: PMTA→SES handoff column in Domain×ISP matrix + Growth narrative
// Change history:
// 1.0 — initial Phase D release. Six tabs backed by the
//       /analytics/* promoted endpoints in mailing_analytics_promoted.go.

export type AnalyticsTabKey =
  | 'overview'
  | 'deliverability'
  | 'engagement'
  | 'offers'
  | 'audience'
  | 'operations'
  | 'reports';

export interface AnalyticsTabsProps {
  activeTab: AnalyticsTabKey;
  range: 'today' | 'yesterday';
  excludeMPP: boolean;
  orgId: string | null;
}

// Range param the backend accepts (mirrors AnalyticsCenter.tsx).
function rangeParam(range: 'today' | 'yesterday'): string {
  return `range_type=${range}`;
}

// fetchJSON is the shared GET helper. Mirrors the headers/credentials
// pattern used by the rest of AnalyticsCenter.
async function fetchJSON<T>(url: string, orgId: string | null): Promise<T> {
  // Delegate to the shared apiFetch (org-scoping + credentials). The
  // explicit orgId is passed as a caller header so it still wins over
  // apiFetch's default X-Organization-ID.
  const res = await apiFetch(url, orgId ? { headers: { 'X-Organization-ID': orgId } } : undefined);
  if (!res.ok) {
    throw new Error(`${res.status} ${res.statusText}`);
  }
  return res.json() as Promise<T>;
}

const fmt = (n: number | undefined): string => {
  if (n === undefined || n === null || Number.isNaN(n)) return '0';
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M';
  if (n >= 1_000) return (n / 1_000).toFixed(1) + 'K';
  return n.toLocaleString();
};

const pct = (v: number | undefined): string => {
  if (v === undefined || v === null || Number.isNaN(v)) return '—';
  return v.toFixed(2) + '%';
};

// ─── Generic panel scaffolding ──────────────────────────────────────────────

interface PanelProps {
  title: string;
  description?: string;
  loading: boolean;
  error: string | null;
  empty: boolean;
  emptyMessage?: string;
  children: React.ReactNode;
  onRefresh?: () => void;
  apiVersion?: string;
}

const Panel: React.FC<PanelProps> = ({
  title, description, loading, error, empty, emptyMessage,
  children, onRefresh, apiVersion,
}) => (
  <div className="ac-card ig-card-hover" style={{ marginBottom: 16 }}>
    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: 8 }}>
      <div>
        <h3 style={{ margin: 0 }}>{title}</h3>
        {description && (
          <p style={{ margin: '4px 0 0 0', color: '#94a3b8', fontSize: 12 }}>{description}</p>
        )}
      </div>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
        {apiVersion && <span style={{ fontSize: 11, color: '#64748b' }}>v{apiVersion}</span>}
        {onRefresh && (
          <button
            className="ac-refresh-btn"
            style={{ padding: '4px 10px', fontSize: 12 }}
            onClick={onRefresh}
            disabled={loading}
          >
            <FontAwesomeIcon icon={faSync} spin={loading} /> Refresh
          </button>
        )}
      </div>
    </div>
    {loading && (
      <div className="ac-loading" style={{ padding: 24 }}>
        <FontAwesomeIcon icon={faSpinner} spin /> Loading…
      </div>
    )}
    {!loading && error && (
      <div style={{ padding: 16, color: '#ef4444', display: 'flex', alignItems: 'center', gap: 8 }}>
        <FontAwesomeIcon icon={faExclamationTriangle} /> {error}
      </div>
    )}
    {!loading && !error && empty && (
      <div className="ac-empty-mini" style={{ padding: 24, textAlign: 'center', color: '#64748b' }}>
        {emptyMessage || 'No data for this window.'}
      </div>
    )}
    {!loading && !error && !empty && children}
  </div>
);

// ─── 1. Deliverability tab ──────────────────────────────────────────────────

interface TerminalCell {
  sending_domain: string;
  isp: string;
  recipients: number;
  delivered: number;
  hard_bounce: number;
  soft_bounce_final: number;
  deferred_open: number;
  sent_only: number;
  soft_bounce_events: number;
  deferral_events: number;
}
interface TerminalStateResp {
  api_version: string;
  grand_total: TerminalCell;
  by_cell: TerminalCell[];
  by_sending_domain: TerminalCell[];
  by_isp: TerminalCell[];
}

interface DomainISPCell {
  sending_domain: string;
  isp: string;
  sent: number;
  delivered: number;
  relayed_to_ses: number;
  hard_bounces: number;
  soft_bounces: number;
  complaints: number;
  deferred: number;
  hard_bounce_rate: number;
  soft_bounce_rate: number;
  delivery_rate: number;
}
interface DomainISPResp {
  api_version: string;
  cells: DomainISPCell[];
}

const DeliverabilityTab: React.FC<{ range: 'today' | 'yesterday'; orgId: string | null }> = ({ range, orgId }) => {
  const [terminal, setTerminal] = useState<TerminalStateResp | null>(null);
  const [matrix, setMatrix] = useState<DomainISPResp | null>(null);
  const [loadingT, setLoadingT] = useState(true);
  const [loadingM, setLoadingM] = useState(true);
  const [errT, setErrT] = useState<string | null>(null);
  const [errM, setErrM] = useState<string | null>(null);

  const loadAll = useCallback(async () => {
    setLoadingT(true); setLoadingM(true); setErrT(null); setErrM(null);
    try {
      const t = await fetchJSON<TerminalStateResp>(`/api/mailing/analytics/terminal-state-matrix?${rangeParam(range)}`, orgId);
      setTerminal(t);
    } catch (e: any) { setErrT(e?.message || 'Failed to load terminal-state matrix.'); }
    finally { setLoadingT(false); }
    try {
      const m = await fetchJSON<DomainISPResp>(`/api/mailing/analytics/domain-isp-matrix?${rangeParam(range)}`, orgId);
      setMatrix(m);
    } catch (e: any) { setErrM(e?.message || 'Failed to load domain×ISP matrix.'); }
    finally { setLoadingM(false); }
  }, [range, orgId]);

  useEffect(() => { loadAll(); }, [loadAll]);

  const totalRecipients = terminal?.grand_total?.recipients || 0;

  return (
    <>
      <Panel
        title="Terminal-State Recipients"
        description={`Per (campaign, subscriber) outcome — delivered wins everything. ${fmt(totalRecipients)} unique recipient pairs in window.`}
        loading={loadingT}
        error={errT}
        empty={!terminal || (terminal.by_isp || []).length === 0}
        onRefresh={loadAll}
        apiVersion={terminal?.api_version}
      >
        {terminal && (
          <div style={{ overflowX: 'auto' }}>
            <table className="ac-table" style={{ width: '100%' }}>
              <thead>
                <tr>
                  <th style={{ textAlign: 'left' }}>ISP</th>
                  <th>Recipients</th>
                  <th>Delivered</th>
                  <th>Del%</th>
                  <th style={{ color: '#ef4444' }}>Hard Bnc</th>
                  <th style={{ color: '#f59e0b' }}>Soft Bnc (final)</th>
                  <th>Deferred Open</th>
                  <th>Sent Only</th>
                  <th>SB Events</th>
                  <th>Defer Events</th>
                </tr>
              </thead>
              <tbody>
                {(terminal.by_isp || []).sort((a, b) => b.delivered - a.delivered).map(c => {
                  const delPct = c.recipients > 0 ? (c.delivered / c.recipients) * 100 : 0;
                  return (
                    <tr key={c.isp}>
                      <td style={{ textTransform: 'capitalize' }}>{c.isp}</td>
                      <td>{fmt(c.recipients)}</td>
                      <td>{fmt(c.delivered)}</td>
                      <td>{delPct.toFixed(1)}%</td>
                      <td style={{ color: '#ef4444' }}>{fmt(c.hard_bounce)}</td>
                      <td style={{ color: '#f59e0b' }}>{fmt(c.soft_bounce_final)}</td>
                      <td>{fmt(c.deferred_open)}</td>
                      <td style={{ color: '#64748b' }}>{fmt(c.sent_only)}</td>
                      <td>{fmt(c.soft_bounce_events)}</td>
                      <td>{fmt(c.deferral_events)}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </Panel>

      <Panel
        title="Domain × ISP Performance Matrix"
        description="Source: pmta_acct_daily_summary (deduped delivery facts). Delivered = PMTA-direct + SES-confirmed (DELIVERY events). 'PMTA→SES' is the relay handoff, NOT a recipient delivery. Ground truth for sending-domain reputation."
        loading={loadingM}
        error={errM}
        empty={!matrix || (matrix.cells || []).length === 0}
        onRefresh={loadAll}
        apiVersion={matrix?.api_version}
      >
        {matrix && (
          <div style={{ overflowX: 'auto' }}>
            <table className="ac-table" style={{ width: '100%' }}>
              <thead>
                <tr>
                  <th style={{ textAlign: 'left' }}>Sending Domain</th>
                  <th>ISP</th>
                  <th>Sent</th>
                  <th>Delivered</th>
                  <th>Del%</th>
                  <th style={{ color: '#8b5cf6' }} title="PMTA handed the message to SES for delivery. This is NOT a recipient delivery — SES DELIVERY events are folded into the Delivered column.">PMTA→SES</th>
                  <th style={{ color: '#ef4444' }}>Hard%</th>
                  <th style={{ color: '#f59e0b' }}>Soft%</th>
                  <th>Complaints</th>
                  <th>Deferred</th>
                </tr>
              </thead>
              <tbody>
                {(matrix.cells || []).map((c, i) => (
                  <tr key={`${c.sending_domain}|${c.isp}|${i}`}>
                    <td>{c.sending_domain}</td>
                    <td style={{ textTransform: 'capitalize' }}>{c.isp}</td>
                    <td>{fmt(c.sent)}</td>
                    <td>{fmt(c.delivered)}</td>
                    <td>{c.delivery_rate.toFixed(1)}%</td>
                    <td style={{ color: '#8b5cf6' }}>{c.relayed_to_ses ? fmt(c.relayed_to_ses) : '—'}</td>
                    <td style={{ color: '#ef4444' }}>{c.hard_bounce_rate.toFixed(2)}%</td>
                    <td style={{ color: '#f59e0b' }}>{c.soft_bounce_rate.toFixed(2)}%</td>
                    <td>{fmt(c.complaints)}</td>
                    <td>{fmt(c.deferred)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>
    </>
  );
};

// ─── 2. Engagement tab ──────────────────────────────────────────────────────

interface EngagementBucket {
  total: number; mpp: number; scanner: number; human: number; no_send: number;
}
interface EngagementResp {
  api_version: string;
  grand_total: EngagementBucket;
  by_isp: Array<EngagementBucket & { isp: string }>;
  by_sending_domain: Array<EngagementBucket & { sending_domain: string }>;
}

const EngagementTab: React.FC<{ range: 'today' | 'yesterday'; orgId: string | null }> = ({ range, orgId }) => {
  const [data, setData] = useState<EngagementResp | null>(null);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true); setErr(null);
    try {
      const r = await fetchJSON<EngagementResp>(`/api/mailing/analytics/engagement-quality?${rangeParam(range)}`, orgId);
      setData(r);
    } catch (e: any) { setErr(e?.message || 'Failed to load engagement quality.'); }
    finally { setLoading(false); }
  }, [range, orgId]);

  useEffect(() => { load(); }, [load]);

  const grand = data?.grand_total;
  const ratio = (n: number, d: number): string => d > 0 ? `${((n / d) * 100).toFixed(1)}%` : '—';

  return (
    <Panel
      title="True Opens vs Proxy / MPP / Scanner"
      description="Classifies every opened event. Scanner = open within 30s of send. MPP = is_machine_open. No-send = orphan/replay."
      loading={loading}
      error={err}
      empty={!data || (data.grand_total?.total || 0) === 0}
      onRefresh={load}
      apiVersion={data?.api_version}
    >
      {data && grand && (
        <>
          <div className="ac-kpi-grid" style={{ marginBottom: 16 }}>
            <div className="ac-kpi"><div className="ac-kpi-body"><span className="ac-kpi-value">{fmt(grand.total)}</span><span className="ac-kpi-label">Total Opens</span></div></div>
            <div className="ac-kpi"><div className="ac-kpi-body"><span className="ac-kpi-value" style={{ color: '#22c55e' }}>{fmt(grand.human)}</span><span className="ac-kpi-label">Human ({ratio(grand.human, grand.total)})</span></div></div>
            <div className="ac-kpi"><div className="ac-kpi-body"><span className="ac-kpi-value" style={{ color: '#3b82f6' }}>{fmt(grand.mpp)}</span><span className="ac-kpi-label">MPP ({ratio(grand.mpp, grand.total)})</span></div></div>
            <div className="ac-kpi"><div className="ac-kpi-body"><span className="ac-kpi-value" style={{ color: '#f59e0b' }}>{fmt(grand.scanner)}</span><span className="ac-kpi-label">Scanner ({ratio(grand.scanner, grand.total)})</span></div></div>
            <div className="ac-kpi"><div className="ac-kpi-body"><span className="ac-kpi-value" style={{ color: '#a3a3a3' }}>{fmt(grand.no_send)}</span><span className="ac-kpi-label">Orphan / No-Send</span></div></div>
          </div>
          <div style={{ overflowX: 'auto' }}>
            <table className="ac-table" style={{ width: '100%' }}>
              <thead><tr>
                <th style={{ textAlign: 'left' }}>ISP</th>
                <th>Total</th>
                <th style={{ color: '#22c55e' }}>Human</th>
                <th style={{ color: '#3b82f6' }}>MPP</th>
                <th style={{ color: '#f59e0b' }}>Scanner</th>
                <th>Orphan</th>
                <th>Human %</th>
              </tr></thead>
              <tbody>
                {(data.by_isp || []).sort((a, b) => b.total - a.total).map(r => (
                  <tr key={r.isp}>
                    <td style={{ textTransform: 'capitalize' }}>{r.isp}</td>
                    <td>{fmt(r.total)}</td>
                    <td style={{ color: '#22c55e' }}>{fmt(r.human)}</td>
                    <td style={{ color: '#3b82f6' }}>{fmt(r.mpp)}</td>
                    <td style={{ color: '#f59e0b' }}>{fmt(r.scanner)}</td>
                    <td>{fmt(r.no_send)}</td>
                    <td>{ratio(r.human, r.total)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </Panel>
  );
};

// ─── 3. Offers tab (Phase E backed) ─────────────────────────────────────────

interface OfferRow {
  slug: string;
  offer_name: string;
  vertical: string;
  clicks: number;
  unique_clickers: number;
  campaigns_with_slug: number;
}
interface OfferPerfResp {
  api_version: string;
  rows: OfferRow[];
}
interface OfferOverlapResp {
  api_version: string;
  pairs: Array<{ a: string; b: string; both: number }>;
}
interface SlugCoverageResp {
  api_version: string;
  campaigns: Array<{
    campaign_id: string;
    name: string;
    detected_slugs: string[];
    expected_slug: string | null;
    has_tracking_suffix: boolean;
  }>;
}

const OffersTab: React.FC<{ range: 'today' | 'yesterday'; orgId: string | null }> = ({ range, orgId }) => {
  const [perf, setPerf] = useState<OfferPerfResp | null>(null);
  const [overlap, setOverlap] = useState<OfferOverlapResp | null>(null);
  const [coverage, setCoverage] = useState<SlugCoverageResp | null>(null);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true); setErr(null);
    try {
      const [p, o, c] = await Promise.all([
        fetchJSON<OfferPerfResp>(`/api/mailing/analytics/offer-performance?${rangeParam(range)}`, orgId),
        fetchJSON<OfferOverlapResp>(`/api/mailing/analytics/offer-overlap?${rangeParam(range)}`, orgId),
        fetchJSON<SlugCoverageResp>(`/api/mailing/analytics/slug-coverage?${rangeParam(range)}`, orgId),
      ]);
      setPerf(p); setOverlap(o); setCoverage(c);
    } catch (e: any) { setErr(e?.message || 'Failed to load offer analytics.'); }
    finally { setLoading(false); }
  }, [range, orgId]);

  useEffect(() => { load(); }, [load]);

  return (
    <>
      <Panel
        title="Offer Performance (slug-derived)"
        description="Attribution derived from link_url slug pattern. Until INTENT Phase 3 lands, this is the canonical offer view."
        loading={loading} error={err}
        empty={!perf || (perf.rows || []).length === 0}
        onRefresh={load}
        apiVersion={perf?.api_version}
      >
        {perf && (
          <table className="ac-table" style={{ width: '100%' }}>
            <thead><tr>
              <th style={{ textAlign: 'left' }}>Offer</th>
              <th>Slug</th>
              <th>Vertical</th>
              <th>Clicks</th>
              <th>Unique Clickers</th>
              <th>Campaigns</th>
            </tr></thead>
            <tbody>
              {(perf.rows || []).sort((a, b) => b.unique_clickers - a.unique_clickers).map(r => (
                <tr key={r.slug}>
                  <td>{r.offer_name}</td>
                  <td><code style={{ fontSize: 11 }}>{r.slug}</code></td>
                  <td>{r.vertical}</td>
                  <td>{fmt(r.clicks)}</td>
                  <td>{fmt(r.unique_clickers)}</td>
                  <td>{fmt(r.campaigns_with_slug)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>

      <Panel
        title="Cross-Offer Overlap"
        description="Subscribers who clicked multiple offers in the window."
        loading={loading} error={err}
        empty={!overlap || (overlap.pairs || []).length === 0}
        onRefresh={load}
        apiVersion={overlap?.api_version}
      >
        {overlap && (
          <table className="ac-table" style={{ width: '100%' }}>
            <thead><tr>
              <th style={{ textAlign: 'left' }}>Offer A</th>
              <th style={{ textAlign: 'left' }}>Offer B</th>
              <th>Subscribers Clicking Both</th>
            </tr></thead>
            <tbody>
              {(overlap.pairs || []).sort((a, b) => b.both - a.both).map((p, i) => (
                <tr key={`${p.a}|${p.b}|${i}`}>
                  <td>{p.a}</td>
                  <td>{p.b}</td>
                  <td>{fmt(p.both)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>

      <Panel
        title="Slug Coverage Audit"
        description="Campaigns scanned for the expected offer slug + cratoolpro tracking suffix in their creative HTML."
        loading={loading} error={err}
        empty={!coverage || (coverage.campaigns || []).length === 0}
        onRefresh={load}
        apiVersion={coverage?.api_version}
      >
        {coverage && (
          <table className="ac-table" style={{ width: '100%' }}>
            <thead><tr>
              <th style={{ textAlign: 'left' }}>Campaign</th>
              <th>Detected Slugs</th>
              <th>Tracking Suffix</th>
            </tr></thead>
            <tbody>
              {(coverage.campaigns || []).map(c => (
                <tr key={c.campaign_id}>
                  <td>{c.name}</td>
                  <td>{(c.detected_slugs || []).map(s => <code key={s} style={{ marginRight: 6, fontSize: 11 }}>{s}</code>)}</td>
                  <td>{c.has_tracking_suffix ? <span style={{ color: '#22c55e' }}>YES</span> : <span style={{ color: '#ef4444' }}>NO</span>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>
    </>
  );
};

// ─── 4. Audience tab ────────────────────────────────────────────────────────

interface AcquisitionRow {
  day: string; isp: string; brand: string;
  new_subs: number; confirmed_subs: number;
}
interface AcquisitionResp {
  api_version: string;
  rows: AcquisitionRow[];
}
interface SegmentRow {
  segment_id: string;
  segment_name: string;
  member_count: number;
  created_at: string;
  updated_at: string;
  stale_hours: number;
}
interface SegmentResp {
  api_version: string;
  segments: SegmentRow[];
}

const AudienceTab: React.FC<{ range: 'today' | 'yesterday'; orgId: string | null }> = ({ range, orgId }) => {
  const [acq, setAcq] = useState<AcquisitionResp | null>(null);
  const [segs, setSegs] = useState<SegmentResp | null>(null);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true); setErr(null);
    try {
      const [a, s] = await Promise.all([
        fetchJSON<AcquisitionResp>(`/api/mailing/analytics/daily-acquisition?${rangeParam(range)}`, orgId),
        fetchJSON<SegmentResp>(`/api/mailing/analytics/segment-integrity`, orgId),
      ]);
      setAcq(a); setSegs(s);
    } catch (e: any) { setErr(e?.message || 'Failed to load audience analytics.'); }
    finally { setLoading(false); }
  }, [range, orgId]);

  useEffect(() => { load(); }, [load]);

  // Aggregate acquisition rows for the summary table
  const byIsp = useMemo(() => {
    if (!acq?.rows) return [];
    const m = new Map<string, { isp: string; new_subs: number; confirmed_subs: number }>();
    acq.rows.forEach(r => {
      const e = m.get(r.isp) || { isp: r.isp, new_subs: 0, confirmed_subs: 0 };
      e.new_subs += r.new_subs;
      e.confirmed_subs += r.confirmed_subs;
      m.set(r.isp, e);
    });
    return Array.from(m.values()).sort((a, b) => b.new_subs - a.new_subs);
  }, [acq]);

  return (
    <>
      <Panel
        title="Daily Acquisition"
        description="New subscriber rows per ISP × brand for the selected window."
        loading={loading} error={err}
        empty={!acq || (acq.rows || []).length === 0}
        onRefresh={load}
        apiVersion={acq?.api_version}
      >
        {acq && (
          <table className="ac-table" style={{ width: '100%' }}>
            <thead><tr>
              <th style={{ textAlign: 'left' }}>ISP</th>
              <th>New Subs</th>
              <th>Confirmed</th>
              <th>Confirm Rate</th>
            </tr></thead>
            <tbody>
              {byIsp.map(r => (
                <tr key={r.isp}>
                  <td style={{ textTransform: 'capitalize' }}>{r.isp}</td>
                  <td>{fmt(r.new_subs)}</td>
                  <td>{fmt(r.confirmed_subs)}</td>
                  <td>{r.new_subs > 0 ? ((r.confirmed_subs / r.new_subs) * 100).toFixed(1) + '%' : '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>

      <Panel
        title="Segment Integrity"
        description="Active segments with last-refresh staleness. Long stale_hours = segment hasn't materialized recently."
        loading={loading} error={err}
        empty={!segs || (segs.segments || []).length === 0}
        onRefresh={load}
        apiVersion={segs?.api_version}
      >
        {segs && (
          <table className="ac-table" style={{ width: '100%' }}>
            <thead><tr>
              <th style={{ textAlign: 'left' }}>Segment</th>
              <th>Members</th>
              <th>Stale (h)</th>
              <th>Updated</th>
            </tr></thead>
            <tbody>
              {(segs.segments || []).map(s => (
                <tr key={s.segment_id}>
                  <td>{s.segment_name}</td>
                  <td>{fmt(s.member_count)}</td>
                  <td style={{ color: s.stale_hours > 24 ? '#f59e0b' : undefined }}>{s.stale_hours.toFixed(1)}</td>
                  <td style={{ fontSize: 11, color: '#94a3b8' }}>{new Date(s.updated_at).toLocaleString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>
    </>
  );
};

// ─── 5. Operations tab ─────────────────────────────────────────────────────

interface QueueCampaign {
  campaign_id: string;
  name: string;
  status: string;
  created_at: string;
  scheduled_at: string | null;
  started_at: string | null;
  organization_id: string;
  queue_by_status: Record<string, number>;
  queue_total: number;
}
interface QueueResp {
  api_version: string;
  campaigns: QueueCampaign[];
}
interface WaveHealthResp {
  api_version: string;
  summary: { zombies: number; expired: number; due_now: number; planned: number; enqueued: number; running: number };
  samples: Array<{
    wave_id: string; campaign_id: string; campaign_name: string;
    campaign_status: string; wave_status: string;
    scheduled_at: string | null; window_end_at: string | null;
    last_error: string | null;
  }>;
  action_required: boolean;
}
interface DispatchResp {
  api_version: string;
  minutes: number;
  buckets: Array<{ minute: string; sent: number; delivered: number; hard_bounces: number; soft_bounces: number; deferred: number }>;
}

const OperationsTab: React.FC<{ orgId: string | null }> = ({ orgId }) => {
  const [queue, setQueue] = useState<QueueResp | null>(null);
  const [waves, setWaves] = useState<WaveHealthResp | null>(null);
  const [disp, setDisp] = useState<DispatchResp | null>(null);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true); setErr(null);
    try {
      const [q, w, d] = await Promise.all([
        fetchJSON<QueueResp>(`/api/mailing/analytics/queue-status-histogram`, orgId),
        fetchJSON<WaveHealthResp>(`/api/mailing/analytics/wave-scheduler-health`, orgId),
        fetchJSON<DispatchResp>(`/api/mailing/analytics/dispatch-timeline?minutes=60`, orgId),
      ]);
      setQueue(q); setWaves(w); setDisp(d);
    } catch (e: any) { setErr(e?.message || 'Failed to load operations data.'); }
    finally { setLoading(false); }
  }, [orgId]);

  useEffect(() => {
    load();
    const id = window.setInterval(load, 30000);
    return () => window.clearInterval(id);
  }, [load]);

  return (
    <>
      <Panel
        title="Wave Scheduler Health"
        description="Pre-deploy janitor signals: zombies (planned waves on dead campaigns), expired (window passed), due_now (planned and overdue)."
        loading={loading} error={err}
        empty={!waves}
        onRefresh={load}
        apiVersion={waves?.api_version}
      >
        {waves && (
          <>
            {waves.action_required && (
              <div style={{ background: '#7f1d1d', color: '#fecaca', padding: 8, borderRadius: 4, marginBottom: 12 }}>
                <FontAwesomeIcon icon={faExclamationTriangle} /> Action required — run pre-deploy janitor.
              </div>
            )}
            <div className="ac-kpi-grid" style={{ marginBottom: 12 }}>
              <div className="ac-kpi"><div className="ac-kpi-body"><span className="ac-kpi-value" style={{ color: waves.summary.zombies > 0 ? '#ef4444' : '#22c55e' }}>{fmt(waves.summary.zombies)}</span><span className="ac-kpi-label">Zombies</span></div></div>
              <div className="ac-kpi"><div className="ac-kpi-body"><span className="ac-kpi-value" style={{ color: waves.summary.expired > 50 ? '#ef4444' : undefined }}>{fmt(waves.summary.expired)}</span><span className="ac-kpi-label">Expired</span></div></div>
              <div className="ac-kpi"><div className="ac-kpi-body"><span className="ac-kpi-value">{fmt(waves.summary.due_now)}</span><span className="ac-kpi-label">Due Now</span></div></div>
              <div className="ac-kpi"><div className="ac-kpi-body"><span className="ac-kpi-value">{fmt(waves.summary.planned)}</span><span className="ac-kpi-label">Planned</span></div></div>
              <div className="ac-kpi"><div className="ac-kpi-body"><span className="ac-kpi-value">{fmt(waves.summary.enqueued)}</span><span className="ac-kpi-label">Enqueued</span></div></div>
              <div className="ac-kpi"><div className="ac-kpi-body"><span className="ac-kpi-value">{fmt(waves.summary.running)}</span><span className="ac-kpi-label">Running</span></div></div>
            </div>
            {(waves.samples || []).length > 0 && (
              <table className="ac-table" style={{ width: '100%' }}>
                <thead><tr>
                  <th>Campaign</th><th>Camp Status</th><th>Wave Status</th><th>Scheduled</th><th>Window End</th>
                </tr></thead>
                <tbody>
                  {(waves.samples || []).slice(0, 25).map(s => (
                    <tr key={s.wave_id}>
                      <td style={{ fontSize: 11 }}>{s.campaign_name || s.campaign_id}</td>
                      <td>{s.campaign_status}</td>
                      <td>{s.wave_status}</td>
                      <td style={{ fontSize: 11 }}>{s.scheduled_at ? new Date(s.scheduled_at).toLocaleString() : '—'}</td>
                      <td style={{ fontSize: 11 }}>{s.window_end_at ? new Date(s.window_end_at).toLocaleString() : '—'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </>
        )}
      </Panel>

      <Panel
        title="Per-Minute Dispatch Timeline (last 60 min)"
        description="Real-time send / deliver / bounce rates per minute. Auto-refresh every 30 seconds."
        loading={loading} error={err}
        empty={!disp || (disp.buckets || []).length === 0}
        onRefresh={load}
        apiVersion={disp?.api_version}
      >
        {disp && (
          <table className="ac-table" style={{ width: '100%' }}>
            <thead><tr>
              <th>Minute</th><th>Sent</th><th>Delivered</th><th>Hard</th><th>Soft</th><th>Deferred</th>
            </tr></thead>
            <tbody>
              {(disp.buckets || []).slice(-30).reverse().map((b, i) => (
                <tr key={`${b.minute}|${i}`}>
                  <td style={{ fontSize: 11 }}>{new Date(b.minute).toLocaleTimeString()}</td>
                  <td>{fmt(b.sent)}</td>
                  <td>{fmt(b.delivered)}</td>
                  <td style={{ color: '#ef4444' }}>{fmt(b.hard_bounces)}</td>
                  <td style={{ color: '#f59e0b' }}>{fmt(b.soft_bounces)}</td>
                  <td>{fmt(b.deferred)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>

      <Panel
        title="Who Did We Mail (Last 48 hours)"
        description="Per-campaign mailing_campaign_queue status histogram. Use to spot stalled or failed enqueues."
        loading={loading} error={err}
        empty={!queue || (queue.campaigns || []).length === 0}
        onRefresh={load}
        apiVersion={queue?.api_version}
      >
        {queue && (
          <table className="ac-table" style={{ width: '100%' }}>
            <thead><tr>
              <th style={{ textAlign: 'left' }}>Campaign</th>
              <th>Status</th>
              <th>Queue Total</th>
              <th>Sent</th>
              <th>Pending</th>
              <th>Failed</th>
              <th>Other</th>
            </tr></thead>
            <tbody>
              {(queue.campaigns || []).map(c => {
                const sent = c.queue_by_status?.sent || 0;
                const pending = (c.queue_by_status?.pending || 0) + (c.queue_by_status?.queued || 0);
                const failed = c.queue_by_status?.failed || 0;
                const other = c.queue_total - sent - pending - failed;
                return (
                  <tr key={c.campaign_id}>
                    <td style={{ fontSize: 11 }}>{c.name || c.campaign_id}</td>
                    <td>{c.status}</td>
                    <td>{fmt(c.queue_total)}</td>
                    <td style={{ color: '#22c55e' }}>{fmt(sent)}</td>
                    <td>{fmt(pending)}</td>
                    <td style={{ color: '#ef4444' }}>{fmt(failed)}</td>
                    <td style={{ color: '#64748b' }}>{fmt(Math.max(0, other))}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </Panel>
    </>
  );
};

// ─── 6. Reports tab ─────────────────────────────────────────────────────────

interface GrowthDay {
  date: string;
  sent: number;
  delivered: number;
  relayed_to_ses: number;
  hard_bounces: number;
  soft_bounces: number;
  complaints: number;
  deferred: number;
  hard_bounce_rate: number;
  soft_bounce_rate: number;
  delivery_rate: number;
}
interface GrowthResp {
  api_version: string;
  days: number;
  summary: {
    total_sent: number;
    total_delivered: number;
    total_relayed_to_ses: number;
    total_hard: number;
    total_soft: number;
    hard_bounce_rate: number;
    soft_bounce_rate: number;
    delivery_rate: number;
  };
  daily: GrowthDay[];
}

const ReportsTab: React.FC<{ orgId: string | null }> = ({ orgId }) => {
  const [data, setData] = useState<GrowthResp | null>(null);
  const [days, setDays] = useState(30);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true); setErr(null);
    try {
      const r = await fetchJSON<GrowthResp>(`/api/mailing/analytics/growth-narrative?days=${days}`, orgId);
      setData(r);
    } catch (e: any) { setErr(e?.message || 'Failed to load growth narrative.'); }
    finally { setLoading(false); }
  }, [days, orgId]);

  useEffect(() => { load(); }, [load]);

  const downloadCSV = (report: 'domain_isp' | 'growth_narrative') => {
    const url = `/api/mailing/analytics/csv-export?report=${report}&range_type=today`;
    window.open(url, '_blank');
  };

  return (
    <>
      <Panel
        title={`Growth Narrative (${days}-day)`}
        description="Daily totals from pmta_acct_daily_summary. Hard/soft rates per day for reputation review."
        loading={loading} error={err}
        empty={!data || (data.daily || []).length === 0}
        onRefresh={load}
        apiVersion={data?.api_version}
      >
        {data && (
          <>
            <div style={{ marginBottom: 12, display: 'flex', gap: 8 }}>
              {[7, 30, 90].map(d => (
                <button
                  key={d}
                  className={days === d ? 'ac-range-selector active' : 'ac-range-selector'}
                  style={{ padding: '4px 10px', fontSize: 12 }}
                  onClick={() => setDays(d)}
                >
                  {d}-day
                </button>
              ))}
            </div>
            <div className="ac-kpi-grid" style={{ marginBottom: 16 }}>
              <div className="ac-kpi"><div className="ac-kpi-body"><span className="ac-kpi-value">{fmt(data.summary.total_sent)}</span><span className="ac-kpi-label">Total Sent</span></div></div>
              <div className="ac-kpi"><div className="ac-kpi-body"><span className="ac-kpi-value" style={{ color: '#22c55e' }}>{fmt(data.summary.total_delivered)}</span><span className="ac-kpi-label">Total Delivered ({pct(data.summary.delivery_rate)})</span></div></div>
              <div className="ac-kpi"><div className="ac-kpi-body"><span className="ac-kpi-value" style={{ color: '#8b5cf6' }}>{fmt(data.summary.total_relayed_to_ses || 0)}</span><span className="ac-kpi-label">PMTA→SES Handoff (not delivery)</span></div></div>
              <div className="ac-kpi"><div className="ac-kpi-body"><span className="ac-kpi-value" style={{ color: '#ef4444' }}>{pct(data.summary.hard_bounce_rate)}</span><span className="ac-kpi-label">Hard Bnc Rate ({fmt(data.summary.total_hard)})</span></div></div>
              <div className="ac-kpi"><div className="ac-kpi-body"><span className="ac-kpi-value" style={{ color: '#f59e0b' }}>{pct(data.summary.soft_bounce_rate)}</span><span className="ac-kpi-label">Soft Bnc Rate ({fmt(data.summary.total_soft)})</span></div></div>
            </div>
            <table className="ac-table" style={{ width: '100%' }}>
              <thead><tr>
                <th>Date</th>
                <th>Sent</th>
                <th>Delivered</th>
                <th>Del%</th>
                <th style={{ color: '#8b5cf6' }} title="PMTA→SES relay handoff — not a recipient delivery. SES DELIVERY events are folded into Delivered.">PMTA→SES</th>
                <th style={{ color: '#ef4444' }}>Hard%</th>
                <th style={{ color: '#f59e0b' }}>Soft%</th>
              </tr></thead>
              <tbody>
                {(data.daily || []).slice().reverse().map(d => (
                  <tr key={d.date}>
                    <td style={{ fontSize: 11 }}>{new Date(d.date).toLocaleDateString()}</td>
                    <td>{fmt(d.sent)}</td>
                    <td>{fmt(d.delivered)}</td>
                    <td>{d.delivery_rate.toFixed(1)}%</td>
                    <td style={{ color: '#8b5cf6' }}>{d.relayed_to_ses ? fmt(d.relayed_to_ses) : '—'}</td>
                    <td style={{ color: '#ef4444' }}>{d.hard_bounce_rate.toFixed(2)}%</td>
                    <td style={{ color: '#f59e0b' }}>{d.soft_bounce_rate.toFixed(2)}%</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </>
        )}
      </Panel>

      <Panel
        title="CSV Exports"
        description="Download canonical reports for offline analysis."
        loading={false} error={null} empty={false}
      >
        <div style={{ display: 'flex', gap: 12 }}>
          <button className="ac-refresh-btn" onClick={() => downloadCSV('domain_isp')}>
            <FontAwesomeIcon icon={faDownload} /> Domain × ISP (today)
          </button>
          <button className="ac-refresh-btn" onClick={() => downloadCSV('growth_narrative')}>
            <FontAwesomeIcon icon={faDownload} /> Growth narrative (today)
          </button>
        </div>
      </Panel>
    </>
  );
};

// ─── Tab dispatcher ─────────────────────────────────────────────────────────

export const AnalyticsTabs: React.FC<AnalyticsTabsProps> = ({ activeTab, range, orgId }) => {
  // Overview is rendered by AnalyticsCenter directly; this component
  // returns null for that tab so the switch is no-op.
  switch (activeTab) {
    case 'deliverability':
      return <DeliverabilityTab range={range} orgId={orgId} />;
    case 'engagement':
      return <EngagementTab range={range} orgId={orgId} />;
    case 'offers':
      return <OffersTab range={range} orgId={orgId} />;
    case 'audience':
      return <AudienceTab range={range} orgId={orgId} />;
    case 'operations':
      return <OperationsTab orgId={orgId} />;
    case 'reports':
      return <ReportsTab orgId={orgId} />;
    default:
      return null;
  }
};

export default AnalyticsTabs;
