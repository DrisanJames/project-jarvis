// OfferProfile.tsx — Offer Alignment Level 1: one offer, decomposed.
//
// Hierarchy (PORTAL_DESIGN_SYSTEM §1): KPI band (≤8 tiles) + one plain-English
// status line → per-ISP delivery/engagement table (hard #ef4444 and soft
// #f59e0b ALWAYS split, never combined) → creative × subject panel recut by
// ISP (with the sandboxed-iframe preview reused from the old Creatives tab) →
// data-source panel (incl. the "(unattributed)" row) → a collapsed Diagnostics
// section holding the sending-domain filter (sending domain is NOT a primary
// axis — diagnostic only, hidden by default).
//
// Data: GET /api/mailing/offer-alignment/offer?offer=&from=&to=[&sending_domain=]
// (one fetch feeds all panels; a failure renders this panel's designed error
// state with Retry — the surrounding tab chrome and matrix stay alive).
// Preview: GET /api/mailing/analytics/creatives/preview?campaign_id= (reused).

import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faSpinner, faSyncAlt, faArrowLeft, faImages, faChevronDown, faChevronRight, faStethoscope,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import { Stat, SectionError, EmptyState } from '../shared/ui';
import {
  OfferResponse, OfferIspRow, OfferCreativeRow, CreativePreviewData,
  fetchAlignment, isAbortError,
  fmtNum, fmtMoney, fmtRate, rateWithDenom, safeDiv,
  orderIsps, ispLabel,
} from './types';
import { badgeMeta, BadgePill, InferredChip } from './badges';
import './OfferProfile.css';

// HARD RULE colors (repo CLAUDE.md): hard bounce red, soft bounce amber.
const HARD_RED = '#ef4444';
const SOFT_AMBER = '#f59e0b';
const LOW_VOLUME_FLOOR = 1000; // creatives under this delivered are greyed ("low volume")

interface ProfileState {
  kind: 'loading' | 'building' | 'disabled' | 'error' | 'ready';
  error?: string;
  data?: OfferResponse;
}

interface PreviewState {
  open: boolean;
  loading: boolean;
  subject: string;
  from_name: string;
  html: string;
  note: string;
  error: string;
}

/** Plain-English delivery status, derived from the per-ISP badges when the
 *  server didn't supply header.status_line. */
function deriveStatusLine(rows: OfferIspRow[]): string {
  if (rows.length === 0) return 'No ISP delivery rows in this window.';
  const issues = rows.filter((r) => {
    const m = badgeMeta(r.badge);
    return m !== null && r.badge !== 'LOW_VOLUME' && r.badge !== 'NO_LAKE';
  });
  const healthyN = rows.length - issues.length;
  if (issues.length === 0) return `Healthy at all ${rows.length} ISPs.`;
  const parts = issues.map((r) => `${ispLabel(r.isp)} ${badgeMeta(r.badge)?.label.toUpperCase() ?? r.badge}`);
  return `Healthy at ${healthyN} of ${rows.length} ISPs — ${parts.join(', ')}.`;
}

export const OfferProfile: React.FC<{
  offer: string;
  offerName: string;
  focusIsp?: string;
  from: string;
  to: string;
  onRangeChange: (from: string, to: string) => void;
  onBack: () => void;
  onOpenEvidence: (isp: string, badge: string) => void;
}> = ({ offer, offerName, focusIsp, from, to, onRangeChange, onBack, onOpenEvidence }) => {
  const [state, setState] = useState<ProfileState>({ kind: 'loading' });
  const [fetchedAt, setFetchedAt] = useState('');
  const abortRef = useRef<AbortController | null>(null);

  // Creative recut: which ISP lane the creative panel shows ('' = all).
  const [creativeIsp, setCreativeIsp] = useState(focusIsp ?? '');

  // Diagnostics (collapsed by default): sending-domain equality filter,
  // applied to the offer fetch as a diagnostic-only param.
  const [diagOpen, setDiagOpen] = useState(false);
  const [domainDraft, setDomainDraft] = useState('');
  const [domainApplied, setDomainApplied] = useState('');

  const [preview, setPreview] = useState<PreviewState | null>(null);
  const previewAbortRef = useRef<AbortController | null>(null);

  const load = useCallback(async () => {
    abortRef.current?.abort();
    const ctl = new AbortController();
    abortRef.current = ctl;
    setState({ kind: 'loading' });
    try {
      const qs = new URLSearchParams({ offer, from, to });
      if (domainApplied) qs.set('sending_domain', domainApplied);
      const out = await fetchAlignment<OfferResponse>(`/api/mailing/offer-alignment/offer?${qs.toString()}`, ctl.signal);
      if (ctl.signal.aborted) return;
      if (out.kind === 'building') setState({ kind: 'building' });
      else if (out.kind === 'disabled') setState({ kind: 'disabled' });
      else {
        const d = out.data;
        setState({
          kind: 'ready',
          data: {
            header: d.header ?? {},
            isp_rows: Array.isArray(d.isp_rows) ? d.isp_rows : [],
            creatives: Array.isArray(d.creatives) ? d.creatives : [],
            data_sources: Array.isArray(d.data_sources) ? d.data_sources : [],
          },
        });
      }
      setFetchedAt(new Date().toLocaleTimeString('en-US', { hour12: false }));
    } catch (e) {
      if (isAbortError(e)) return;
      setState({ kind: 'error', error: e instanceof Error ? e.message : String(e) });
    }
  }, [offer, from, to, domainApplied]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  useEffect(() => () => previewAbortRef.current?.abort(), []);

  // ── Preview modal (pattern carried over from the old Creatives tab) ──
  const openPreview = useCallback((row: OfferCreativeRow) => {
    previewAbortRef.current?.abort();
    const subject = row.subject || '(no subject)';
    const fromName = row.from_name || '';
    if (!row.has_html || !row.sample_campaign_id) {
      const note = !row.sample_campaign_id
        ? 'No in-range campaign to preview for this row (e.g. attribution from outside the selected window).'
        : 'This is a drip reminder — it reuses the clicked creative at send time, so there is no stored HTML to preview.';
      setPreview({ open: true, loading: false, subject, from_name: fromName, html: '', note, error: '' });
      return;
    }
    const ctl = new AbortController();
    previewAbortRef.current = ctl;
    setPreview({ open: true, loading: true, subject, from_name: fromName, html: '', note: '', error: '' });
    (async () => {
      try {
        const qs = new URLSearchParams({ campaign_id: row.sample_campaign_id });
        const res = await apiFetch(`/api/mailing/analytics/creatives/preview?${qs.toString()}`, { signal: ctl.signal });
        if (!res.ok) {
          let msg = `HTTP ${res.status}`;
          try {
            const b: { error?: string } = await res.json();
            if (b && typeof b.error === 'string' && b.error) msg = b.error;
          } catch { /* keep HTTP status message */ }
          throw new Error(msg);
        }
        const p = (await res.json()) as CreativePreviewData;
        if (ctl.signal.aborted) return;
        setPreview({
          open: true, loading: false,
          subject: p.subject || subject, from_name: p.from_name || fromName,
          html: p.html || '', note: p.has_html ? '' : 'No stored HTML for this campaign.', error: '',
        });
      } catch (e) {
        if (isAbortError(e)) return;
        setPreview({
          open: true, loading: false, subject, from_name: fromName, html: '', note: '',
          error: e instanceof Error ? e.message : String(e),
        });
      }
    })();
  }, []);

  const data = state.kind === 'ready' ? state.data : undefined;
  const ispRows = useMemo(() => {
    const rows = data?.isp_rows ?? [];
    const order = orderIsps(rows.map((r) => r.isp));
    return [...rows].sort((a, b) => order.indexOf(a.isp) - order.indexOf(b.isp));
  }, [data]);

  // KPI totals: header values win; sums over isp_rows are the fallback so the
  // band never renders blank when the header omits a key.
  const kpis = useMemo(() => {
    const sum = (f: (r: OfferIspRow) => number) => ispRows.reduce((a, r) => a + (Number.isFinite(f(r)) ? f(r) : 0), 0);
    const h = data?.header ?? {};
    const delivered = h.delivered ?? sum((r) => r.delivered);
    const pgSent = h.pg_sent ?? sum((r) => r.pg_sent ?? 0);
    const humanClicks = h.human_clicks ?? sum((r) => r.human_clicks);
    const clickers = h.clickers ?? sum((r) => r.clickers);
    const conversions = h.conversions ?? sum((r) => r.conversions);
    const revenue = h.revenue ?? sum((r) => r.revenue);
    return {
      delivered,
      pgSent,
      humanClicks,
      clickers,
      clickerRate: h.clicker_rate ?? safeDiv(clickers, pgSent > 0 ? pgSent : delivered),
      conversions,
      revenue,
      rpm: h.rpm ?? (delivered > 0 ? (revenue / delivered) * 1000 : null),
      epc: h.epc ?? safeDiv(revenue, humanClicks),
    };
  }, [data, ispRows]);

  const statusLine = data ? (data.header.status_line || deriveStatusLine(ispRows)) : '';
  const attribution = data?.header.attribution;

  const ispTotals = useMemo(() => {
    const sum = (f: (r: OfferIspRow) => number) => ispRows.reduce((a, r) => a + (Number.isFinite(f(r)) ? f(r) : 0), 0);
    const delivered = sum((r) => r.delivered);
    const pgSent = sum((r) => r.pg_sent ?? 0);
    const revenue = sum((r) => r.revenue);
    return {
      delivered,
      pgSent,
      blocked: sum((r) => r.reputation_block),
      deferred: sum((r) => r.deferred),
      hard: sum((r) => r.hard),
      soft: sum((r) => r.soft),
      clickers: sum((r) => r.clickers),
      conversions: sum((r) => r.conversions),
      rpm: delivered > 0 ? (revenue / delivered) * 1000 : null,
    };
  }, [ispRows]);

  // Creative panel: ISP chips + recut, low-volume rows greyed and ranked last.
  const creativeIsps = useMemo(() => orderIsps((data?.creatives ?? []).map((c) => c.isp)), [data]);
  const creativeRows = useMemo(() => {
    let rows = data?.creatives ?? [];
    if (creativeIsp) rows = rows.filter((c) => c.isp === creativeIsp);
    return [...rows].sort((a, b) => {
      const aLow = a.delivered < LOW_VOLUME_FLOOR ? 1 : 0;
      const bLow = b.delivered < LOW_VOLUME_FLOOR ? 1 : 0;
      if (aLow !== bLow) return aLow - bLow; // rankable rows first, low-volume greyed below
      return b.clickers - a.clickers;
    });
  }, [data, creativeIsp]);

  // ── Body states ──
  let body: React.ReactNode;
  if (state.kind === 'loading') {
    body = (
      <div className="oa-op-loading">
        <FontAwesomeIcon icon={faSpinner} spin /> Loading offer profile…
      </div>
    );
  } else if (state.kind === 'building') {
    body = (
      <div className="oa-op-loading">
        Profile data is still building server-side.
        <button type="button" className="oa-op-inlineretry" onClick={load}>Retry</button>
      </div>
    );
  } else if (state.kind === 'disabled') {
    body = <EmptyState title="Offer alignment is off" hint="The server reports this feature disabled." />;
  } else if (state.kind === 'error') {
    body = <SectionError label="Offer profile" error={state.error} onRetry={load} />;
  } else if (!data) {
    body = <EmptyState title="No data" hint="Empty response for this offer and window." />;
  } else {
    body = (
      <>
        {/* ── KPI band (8 tiles) + status line ── */}
        <div className="oa-op-kpiband">
          <Stat label="Delivered" value={fmtNum(kpis.delivered)} sub={`${from} → ${to}`} />
          <Stat label="Human clicks" value={fmtNum(kpis.humanClicks)} sub="verdict-human only" />
          <Stat label="Clickers" value={fmtNum(kpis.clickers)} sub="unique people" />
          <Stat
            label="Clicker rate"
            value={fmtRate(kpis.clickerRate)}
            sub={`of ${fmtNum(kpis.pgSent > 0 ? kpis.pgSent : kpis.delivered)} sent (PG-scoped)`}
            title={rateWithDenom(kpis.clickerRate, kpis.pgSent > 0 ? kpis.pgSent : kpis.delivered, 'sent (PG)')}
          />
          <Stat label="Conversions" value={fmtNum(kpis.conversions)} color="#22c55e" sub="last-click attributed" />
          <Stat label="Revenue" value={fmtMoney(kpis.revenue)} />
          <Stat label="RPM" value={kpis.rpm == null ? '—' : fmtMoney(kpis.rpm)} sub={`revenue / ${fmtNum(kpis.delivered)} delivered × 1,000`} />
          <Stat label="EPC" value={kpis.epc == null ? '—' : fmtMoney(kpis.epc)} sub={`revenue / ${fmtNum(kpis.humanClicks)} human clicks`} />
        </div>
        <div className="oa-op-statusline" title="Derived from the per-ISP delivery-health badges below.">
          {statusLine}
        </div>
        {attribution && (
          <div className="oa-op-attribution">
            Attribution: {fmtNum(attribution.stamped_campaigns)} stamped campaign{attribution.stamped_campaigns === 1 ? '' : 's'} ·{' '}
            {fmtNum(attribution.inferred_campaigns)} inferred (historical, name/slug-matched).
          </div>
        )}

        {/* ── Per-ISP table ── */}
        <div className="oa-op-section">
          <div className="oa-op-sectiontitle">Per-ISP delivery &amp; performance</div>
          {ispRows.length === 0 ? (
            <EmptyState title="No ISP rows" hint="No delivery attributed to this offer in the window." />
          ) : (
            <div className="oa-op-tablewrap">
              <table className="oa-op-table">
                <thead>
                  <tr>
                    <th className="oa-op-th oa-op-th--left">ISP</th>
                    <th className="oa-op-th">Delivered</th>
                    <th className="oa-op-th">Blocked</th>
                    <th className="oa-op-th">Deferred</th>
                    <th className="oa-op-th">Hard</th>
                    <th className="oa-op-th">Soft</th>
                    <th className="oa-op-th">Clickers</th>
                    <th className="oa-op-th">Clicker rate</th>
                    <th className="oa-op-th">Conv</th>
                    <th className="oa-op-th">RPM</th>
                    <th className="oa-op-th oa-op-th--left">Badge</th>
                    <th className="oa-op-th oa-op-th--left">Action</th>
                  </tr>
                </thead>
                <tbody>
                  {ispRows.map((r) => {
                    const meta = badgeMeta(r.badge);
                    return (
                      <tr key={r.isp} className={r.isp === focusIsp ? 'oa-op-tr oa-op-tr--focus' : 'oa-op-tr'}>
                        <td className="oa-op-td oa-op-td--left">
                          {ispLabel(r.isp)}
                          {r.isp === focusIsp && <span className="oa-op-focuschip">selected</span>}
                        </td>
                        <td className="oa-op-td">{fmtNum(r.delivered)}</td>
                        <td className="oa-op-td" title={`reputation blocks — ${rateWithDenom(r.block_rate, r.delivered + r.reputation_block + r.hard + r.soft, 'attempted (approx)')}`}>
                          {fmtNum(r.reputation_block)}
                        </td>
                        <td className="oa-op-td">{fmtNum(r.deferred)}</td>
                        <td className="oa-op-td" style={{ color: HARD_RED }}>{fmtNum(r.hard)}</td>
                        <td className="oa-op-td" style={{ color: SOFT_AMBER }}>{fmtNum(r.soft)}</td>
                        <td className="oa-op-td">{fmtNum(r.clickers)}</td>
                        <td className="oa-op-td" title={rateWithDenom(r.clicker_rate, r.pg_sent ?? r.delivered, 'sent (PG)')}>
                          {fmtRate(r.clicker_rate)}
                          <span className="oa-op-denom"> of {fmtNum(r.delivered)}</span>
                        </td>
                        <td className="oa-op-td">{fmtNum(r.conversions)}</td>
                        <td className="oa-op-td">{Number.isFinite(r.rpm) ? fmtMoney(r.rpm) : '—'}</td>
                        <td className="oa-op-td oa-op-td--left">
                          <BadgePill
                            badge={r.badge}
                            reason={r.badge_reason}
                            onClick={meta?.clickable ? () => onOpenEvidence(r.isp, r.badge) : undefined}
                          />
                        </td>
                        <td className="oa-op-td oa-op-td--left oa-op-action">{r.action || '—'}</td>
                      </tr>
                    );
                  })}
                </tbody>
                <tfoot>
                  <tr className="oa-op-tr oa-op-tr--total">
                    <td className="oa-op-td oa-op-td--left">Total</td>
                    <td className="oa-op-td">{fmtNum(ispTotals.delivered)}</td>
                    <td className="oa-op-td">{fmtNum(ispTotals.blocked)}</td>
                    <td className="oa-op-td">{fmtNum(ispTotals.deferred)}</td>
                    <td className="oa-op-td" style={{ color: HARD_RED }}>{fmtNum(ispTotals.hard)}</td>
                    <td className="oa-op-td" style={{ color: SOFT_AMBER }}>{fmtNum(ispTotals.soft)}</td>
                    <td className="oa-op-td">{fmtNum(ispTotals.clickers)}</td>
                    <td className="oa-op-td" title={rateWithDenom(safeDiv(ispTotals.clickers, ispTotals.pgSent > 0 ? ispTotals.pgSent : ispTotals.delivered), ispTotals.pgSent > 0 ? ispTotals.pgSent : ispTotals.delivered, 'sent (PG)')}>
                      {fmtRate(safeDiv(ispTotals.clickers, ispTotals.pgSent > 0 ? ispTotals.pgSent : ispTotals.delivered))}
                    </td>
                    <td className="oa-op-td">{fmtNum(ispTotals.conversions)}</td>
                    <td className="oa-op-td">{ispTotals.rpm == null ? '—' : fmtMoney(ispTotals.rpm)}</td>
                    <td className="oa-op-td oa-op-td--left" colSpan={2} />
                  </tr>
                </tfoot>
              </table>
              <div className="oa-op-footnote">
                Blocked = reputation-class rejections (its own class, not a bounce). Hard (red) and soft (amber)
                bounces are always split, never combined. Click a red/amber badge for the grouped SMTP evidence.
              </div>
            </div>
          )}
        </div>

        {/* ── Creative × subject panel, recut by ISP ── */}
        <div className="oa-op-section">
          <div className="oa-op-sectiontitle">Creatives &amp; subjects by ISP</div>
          <div className="oa-op-chiprow">
            <button
              type="button"
              className={`oa-op-chip${creativeIsp === '' ? ' is-active' : ''}`}
              onClick={() => setCreativeIsp('')}
            >
              All ISPs
            </button>
            {creativeIsps.map((isp) => (
              <button
                key={isp}
                type="button"
                className={`oa-op-chip${creativeIsp === isp ? ' is-active' : ''}`}
                onClick={() => setCreativeIsp(isp)}
              >
                {ispLabel(isp)}
              </button>
            ))}
          </div>
          {creativeRows.length === 0 ? (
            <EmptyState
              title="No creatives in range"
              hint={creativeIsp ? `Nothing sent for this offer at ${ispLabel(creativeIsp)} in the window.` : 'Nothing sent for this offer in the window.'}
            />
          ) : (
            <div className="oa-op-tablewrap">
              <table className="oa-op-table">
                <thead>
                  <tr>
                    <th className="oa-op-th oa-op-th--left">Creative / subject</th>
                    <th className="oa-op-th oa-op-th--left">ISP</th>
                    <th className="oa-op-th">Delivered</th>
                    <th className="oa-op-th">Clicks</th>
                    <th className="oa-op-th">Clickers</th>
                    <th className="oa-op-th">Clicker rate</th>
                    <th className="oa-op-th">Conv</th>
                    <th className="oa-op-th">Revenue</th>
                  </tr>
                </thead>
                <tbody>
                  {creativeRows.map((c, idx) => {
                    const low = c.delivered < LOW_VOLUME_FLOOR;
                    return (
                      <tr key={`${c.creative_key}|${c.isp}|${idx}`} className={low ? 'oa-op-tr oa-op-tr--low' : 'oa-op-tr'}>
                        <td className="oa-op-td oa-op-td--left oa-op-creativecell" title={c.subject}>
                          <div className="oa-op-creative-subject">
                            {c.subject || '(no subject)'}
                            {c.inferred && <InferredChip />}
                            {low && <span className="oa-op-lowchip" title={`Under ${fmtNum(LOW_VOLUME_FLOOR)} delivered — not rankable`}>low volume</span>}
                          </div>
                          {c.from_name && <div className="oa-op-creative-from">from: {c.from_name}</div>}
                          <button type="button" className="oa-op-previewbtn" onClick={() => openPreview(c)}>
                            <FontAwesomeIcon icon={faImages} /> {c.has_html ? 'View creative' : 'Drip — no stored html'}
                          </button>
                        </td>
                        <td className="oa-op-td oa-op-td--left">{ispLabel(c.isp)}</td>
                        <td className="oa-op-td">{fmtNum(c.delivered)}</td>
                        <td className="oa-op-td">{fmtNum(c.clicks)}</td>
                        <td className="oa-op-td">{fmtNum(c.clickers)}</td>
                        <td className="oa-op-td" title={rateWithDenom(c.clicker_rate, c.delivered)}>
                          {fmtRate(c.clicker_rate)}
                          <span className="oa-op-denom"> of {fmtNum(c.delivered)}</span>
                        </td>
                        <td className="oa-op-td">{fmtNum(c.conversions)}</td>
                        <td className="oa-op-td">{fmtMoney(c.revenue)}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
              <div className="oa-op-footnote">
                Ranked by clickers; rows under {fmtNum(LOW_VOLUME_FLOOR)} delivered are greyed and ranked last
                (low volume — not comparable). “Inferred” = historical attribution from campaign name/slug rather
                than a deploy-time stamp.
              </div>
            </div>
          )}
        </div>

        {/* ── Data-source panel ── */}
        <div className="oa-op-section">
          <div className="oa-op-sectiontitle">Data sources</div>
          {data.data_sources.length === 0 ? (
            <EmptyState title="No data-source rows" hint="No subscriber data-source attribution in this window." />
          ) : (
            <div className="oa-op-tablewrap">
              <table className="oa-op-table">
                <thead>
                  <tr>
                    <th className="oa-op-th oa-op-th--left">Data source</th>
                    <th className="oa-op-th">Delivered</th>
                    <th className="oa-op-th">Hard</th>
                    <th className="oa-op-th">Invalid rate</th>
                    <th className="oa-op-th">Clicks</th>
                    <th className="oa-op-th">Clickers</th>
                    <th className="oa-op-th">Conv</th>
                  </tr>
                </thead>
                <tbody>
                  {data.data_sources.map((s, idx) => {
                    const label = s.data_source && s.data_source.trim() ? s.data_source : '(unattributed)';
                    return (
                      <tr key={`${label}|${idx}`} className="oa-op-tr">
                        <td className={`oa-op-td oa-op-td--left${label === '(unattributed)' ? ' oa-op-unattributed' : ''}`}>{label}</td>
                        <td className="oa-op-td">{fmtNum(s.delivered)}</td>
                        <td className="oa-op-td" style={{ color: HARD_RED }}>{fmtNum(s.hard)}</td>
                        <td className="oa-op-td" title={rateWithDenom(s.invalid_rate, s.delivered + s.hard, 'delivered + hard')}>
                          {fmtRate(s.invalid_rate)}
                          <span className="oa-op-denom"> of {fmtNum(s.delivered + s.hard)}</span>
                        </td>
                        <td className="oa-op-td">{fmtNum(s.clicks)}</td>
                        <td className="oa-op-td">{fmtNum(s.clickers)}</td>
                        <td className="oa-op-td">{fmtNum(s.conversions)}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
        </div>

        {/* ── Diagnostics (collapsed; sending domain is NOT a primary axis) ── */}
        <div className="oa-op-section">
          <button type="button" className="oa-op-diagtoggle" onClick={() => setDiagOpen((o) => !o)}>
            <FontAwesomeIcon icon={diagOpen ? faChevronDown : faChevronRight} />
            <FontAwesomeIcon icon={faStethoscope} /> Diagnostics
            {domainApplied && <span className="oa-op-diagactive">sending_domain = {domainApplied}</span>}
          </button>
          {diagOpen && (
            <div className="oa-op-diagbody">
              <div className="oa-op-diagnote">
                Sending domain is a diagnostic filter only — brands are separate senders and domain is not a
                primary axis of this screen. Applying it re-scopes every panel above to that domain.
              </div>
              <div className="oa-op-diagrow">
                <label className="oa-op-diaglabel">
                  Sending domain
                  <input
                    type="text"
                    className="oa-op-diaginput"
                    placeholder="em.discountblog.com"
                    value={domainDraft}
                    onChange={(e) => setDomainDraft(e.target.value)}
                  />
                </label>
                <button
                  type="button"
                  className="oa-op-diagapply"
                  disabled={domainDraft.trim() === domainApplied}
                  onClick={() => setDomainApplied(domainDraft.trim())}
                >
                  Apply
                </button>
                {domainApplied && (
                  <button
                    type="button"
                    className="oa-op-diagclear"
                    onClick={() => {
                      setDomainDraft('');
                      setDomainApplied('');
                    }}
                  >
                    Clear
                  </button>
                )}
              </div>
            </div>
          )}
        </div>
      </>
    );
  }

  const today = new Date().toISOString().slice(0, 10);
  return (
    <div className="oa-op-root">
      {/* ── Header controls ── */}
      <div className="oa-op-header">
        <button type="button" className="oa-op-back" onClick={onBack}>
          <FontAwesomeIcon icon={faArrowLeft} /> Matrix
        </button>
        <div className="oa-op-titleblock">
          <div className="oa-op-title">{offerName || offer}</div>
          <div className="oa-op-offerkey">{offer}</div>
        </div>
        <div className="oa-op-rangectl">
          <label className="oa-op-diaglabel">
            From
            <input type="date" className="oa-op-diaginput" value={from} max={to} onChange={(e) => onRangeChange(e.target.value, to)} />
          </label>
          <label className="oa-op-diaglabel">
            To
            <input type="date" className="oa-op-diaginput" value={to} min={from} max={today} onChange={(e) => onRangeChange(from, e.target.value)} />
          </label>
          <button type="button" className="oa-op-refresh" onClick={load} disabled={state.kind === 'loading'}>
            <FontAwesomeIcon icon={state.kind === 'loading' ? faSpinner : faSyncAlt} spin={state.kind === 'loading'} /> Refresh
          </button>
        </div>
      </div>

      {body}

      <div className="oa-op-timing">
        {fetchedAt ? `fetched ${fetchedAt} · ` : ''}
        {from} → {to} · America/Denver
        {domainApplied ? ` · sending_domain=${domainApplied} (diagnostic)` : ''}
      </div>

      {/* ── Creative HTML preview modal (sandboxed iframe) ── */}
      {preview?.open && (
        <div className="oa-op-modal-backdrop" onClick={() => setPreview(null)}>
          <div className="oa-op-modal" onClick={(e) => e.stopPropagation()}>
            <div className="oa-op-modal-head">
              <div className="oa-op-modal-titles">
                <div className="oa-op-modal-subject">{preview.subject || '(no subject)'}</div>
                {preview.from_name && <div className="oa-op-modal-from">from: {preview.from_name}</div>}
              </div>
              <button type="button" className="oa-op-modal-close" onClick={() => setPreview(null)}>×</button>
            </div>
            <div className="oa-op-modal-body">
              {preview.loading ? (
                <div className="oa-op-modal-loading">
                  <FontAwesomeIcon icon={faSpinner} spin /> Loading creative…
                </div>
              ) : preview.error ? (
                <div className="oa-op-modal-error">Preview failed: {preview.error}</div>
              ) : preview.note ? (
                <div className="oa-op-modal-note">{preview.note}</div>
              ) : (
                <iframe title="creative-preview" sandbox="" srcDoc={preview.html} className="oa-op-modal-iframe" />
              )}
            </div>
          </div>
        </div>
      )}
    </div>
  );
};

export default OfferProfile;
