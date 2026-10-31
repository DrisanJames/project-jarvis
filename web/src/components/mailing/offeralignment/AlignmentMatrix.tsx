// AlignmentMatrix.tsx — Conversions tab, Level 0.
//
// Offers as rows × ISPs as columns. Each cell is a hybrid highlight-table
// entry: the metric number is ALWAYS printed (text ink, never series color), a
// sequential single-hue indigo ramp carries magnitude on the SELECTED
// performance metric only (RPM | human click rate | clicker rate), a thin bar
// under the number carries delivered volume, and a corner badge carries the
// independent delivery-health channel. LOW_VOLUME cells are grey — excluded
// from the ramp so noisy rates never look hot.
//
// Data: GET /api/mailing/offer-alignment/matrix?window=3|7|14 (pre-built
// snapshot). 202/{status:'building'} renders the designed "snapshot building"
// state with a retry; {disabled:true} renders the feature-dark card. Right
// rail: offer leaderboard (RPM, clicker rate, delivered N, active badges).

import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faSpinner, faSyncAlt, faHourglassHalf, faMoon, faTrophy } from '@fortawesome/free-solid-svg-icons';
import { SectionError, EmptyState } from '../shared/ui';
import {
  MatrixResponse, MatrixRow, MatrixMetric, WindowDays,
  fetchAlignment, isAbortError,
  fmtNum, fmtMoney, fmtRate, rateWithDenom, safeDiv,
  orderIsps, ispLabel,
} from './types';
import { badgeMeta, CornerBadge, BadgePill } from './badges';
import './AlignmentMatrix.css';

const METRICS: Array<{ id: MatrixMetric; label: string; blurb: string }> = [
  { id: 'rpm', label: 'RPM', blurb: 'money per 1,000 emails delivered' },
  { id: 'human_click_rate', label: 'Human click rate', blurb: 'clicks by real people ÷ delivered' },
  { id: 'clicker_rate', label: 'Clicker rate', blurb: 'people who clicked ÷ delivered' },
];

const metricValue = (r: MatrixRow, m: MatrixMetric): number | null => {
  switch (m) {
    case 'rpm':
      return r.revenue_known && Number.isFinite(r.rpm) ? r.rpm : null;
    case 'human_click_rate':
      return safeDiv(r.human_clicks, r.delivered);
    case 'clicker_rate':
      return Number.isFinite(r.clicker_rate) ? r.clicker_rate : null;
  }
};

const fmtMetric = (v: number | null, m: MatrixMetric): string =>
  v == null ? '—' : m === 'rpm' ? fmtMoney(v) : fmtRate(v, 2);

interface MatrixState {
  kind: 'loading' | 'building' | 'disabled' | 'error' | 'ready';
  error?: string;
  data?: MatrixResponse;
}

export const AlignmentMatrix: React.FC<{
  windowDays: WindowDays;
  onWindowChange: (w: WindowDays) => void;
  onOpenProfile: (offerKey: string, offerName: string, isp?: string) => void;
  onOpenEvidence: (offerKey: string, offerName: string, isp: string, badge: string) => void;
}> = ({ windowDays, onWindowChange, onOpenProfile, onOpenEvidence }) => {
  const [state, setState] = useState<MatrixState>({ kind: 'loading' });
  const [metric, setMetric] = useState<MatrixMetric>('rpm');
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(async () => {
    abortRef.current?.abort();
    const ctl = new AbortController();
    abortRef.current = ctl;
    setState((s) => (s.kind === 'ready' ? s : { kind: 'loading' }));
    try {
      const out = await fetchAlignment<MatrixResponse>(
        `/api/mailing/offer-alignment/matrix?window=${windowDays}`,
        ctl.signal
      );
      if (ctl.signal.aborted) return;
      if (out.kind === 'building') setState({ kind: 'building' });
      else if (out.kind === 'disabled') setState({ kind: 'disabled' });
      else setState({ kind: 'ready', data: { ...out.data, rows: Array.isArray(out.data.rows) ? out.data.rows : [] } });
    } catch (e) {
      if (isAbortError(e)) return;
      setState({ kind: 'error', error: e instanceof Error ? e.message : String(e) });
    }
  }, [windowDays]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  const rows = state.kind === 'ready' && state.data ? state.data.rows : [];

  // Grid: offer rows (ordered by total delivered desc) × ISP columns.
  const grid = useMemo(() => {
    const isps = orderIsps(rows.map((r) => r.isp));
    const byOffer = new Map<string, { name: string; cells: Map<string, MatrixRow>; delivered: number }>();
    for (const r of rows) {
      let o = byOffer.get(r.offer_key);
      if (!o) {
        o = { name: r.offer_name || r.offer_key, cells: new Map(), delivered: 0 };
        byOffer.set(r.offer_key, o);
      }
      o.cells.set(r.isp, r);
      o.delivered += r.delivered;
    }
    const offers = Array.from(byOffer.entries())
      .map(([key, o]) => ({ key, ...o }))
      .sort((a, b) => b.delivered - a.delivered);

    // Ramp + volume-bar scales. LOW_VOLUME cells are excluded from the ramp
    // scale so a noisy small-sample rate can never define "hot".
    let maxMetric = 0;
    let maxDelivered = 0;
    for (const r of rows) {
      if (r.delivered > maxDelivered) maxDelivered = r.delivered;
      if (r.badge === 'LOW_VOLUME') continue;
      const v = metricValue(r, metric);
      if (v != null && v > maxMetric) maxMetric = v;
    }
    return { isps, offers, maxMetric, maxDelivered };
  }, [rows, metric]);

  // Right-rail leaderboard: per-offer aggregate, ranked by RPM.
  const leaderboard = useMemo(() => {
    const agg = new Map<string, { name: string; delivered: number; revenue: number; revenueKnown: boolean; clickers: number; badges: Set<string> }>();
    for (const r of rows) {
      let a = agg.get(r.offer_key);
      if (!a) {
        a = { name: r.offer_name || r.offer_key, delivered: 0, revenue: 0, revenueKnown: false, clickers: 0, badges: new Set() };
        agg.set(r.offer_key, a);
      }
      a.delivered += r.delivered;
      a.revenue += r.revenue;
      a.revenueKnown = a.revenueKnown || r.revenue_known;
      a.clickers += r.clickers;
      if (r.badge && r.badge !== 'HEALTHY' && r.badge !== 'LOW_VOLUME') a.badges.add(r.badge);
    }
    return Array.from(agg.entries())
      .map(([key, a]) => ({
        key,
        name: a.name,
        delivered: a.delivered,
        rpm: a.revenueKnown && a.delivered > 0 ? (a.revenue / a.delivered) * 1000 : null,
        clickerRate: safeDiv(a.clickers, a.delivered),
        badges: Array.from(a.badges),
      }))
      .sort((a, b) => (b.rpm ?? -1) - (a.rpm ?? -1));
  }, [rows]);

  const cellTitle = (r: MatrixRow): string => {
    const v = metricValue(r, metric);
    const lines: string[] = [];
    if (metric === 'rpm') {
      lines.push(r.revenue_known
        ? `RPM ${fmtMetric(v, metric)} = ${fmtMoney(r.revenue)} revenue / ${fmtNum(r.delivered)} delivered × 1,000`
        : 'RPM unknown: no payout on record for this offer');
    } else if (metric === 'human_click_rate') {
      lines.push(`Human click rate ${rateWithDenom(v, r.delivered)} (${fmtNum(r.human_clicks)} human clicks)`);
    } else {
      lines.push(`Clicker rate ${rateWithDenom(v, r.delivered)} (${fmtNum(r.clickers)} clickers)`);
    }
    lines.push(`Delivered ${fmtNum(r.delivered)} · conversions ${fmtNum(r.conversions)} · revenue ${r.revenue_known ? fmtMoney(r.revenue) : '—'}`);
    const meta = badgeMeta(r.badge);
    if (meta) lines.push(`${meta.label}${r.badge_reason ? ` — ${r.badge_reason}` : ''}`);
    if (r.action) lines.push(`Action: ${r.action}`);
    if (!r.sample_ok) lines.push('Sample below threshold — treat rates as indicative only.');
    lines.push(`Attribution coverage ${fmtRate(r.attribution_coverage)}`);
    return lines.join('\n');
  };

  const metricBlurb = METRICS.find((m) => m.id === metric)?.blurb ?? '';

  // ── Controls row (always rendered so window/retry stay reachable) ──
  const controls = (
    <div className="oa-mx-controls">
      <div className="oa-mx-seg" role="group" aria-label="Window">
        {([3, 7, 14] as const).map((w) => (
          <button
            key={w}
            type="button"
            className={`oa-mx-seg-btn${windowDays === w ? ' is-active' : ''}`}
            onClick={() => onWindowChange(w)}
          >
            {w}D
          </button>
        ))}
      </div>
      <div className="oa-mx-seg" role="group" aria-label="Metric">
        {METRICS.map((m) => (
          <button
            key={m.id}
            type="button"
            className={`oa-mx-seg-btn${metric === m.id ? ' is-active' : ''}`}
            title={m.blurb}
            onClick={() => setMetric(m.id)}
          >
            {m.label}
          </button>
        ))}
      </div>
      <span className="oa-mx-metricnote">Color shows {metricBlurb}. Badges show delivery problems.</span>
      <button type="button" className="oa-mx-refresh" onClick={load} disabled={state.kind === 'loading'}>
        <FontAwesomeIcon icon={state.kind === 'loading' ? faSpinner : faSyncAlt} spin={state.kind === 'loading'} /> Refresh
      </button>
    </div>
  );

  let body: React.ReactNode;
  if (state.kind === 'loading') {
    body = (
      <div className="oa-mx-statecard">
        <FontAwesomeIcon icon={faSpinner} spin /> Loading conversions…
      </div>
    );
  } else if (state.kind === 'building') {
    body = (
      <div className="oa-mx-statecard">
        <FontAwesomeIcon icon={faHourglassHalf} className="oa-mx-statecard-icon" />
        <div>
          <div className="oa-mx-statecard-title">Building the numbers</div>
          <div className="oa-mx-statecard-body">
            The {windowDays}-day numbers are being built. This can take up to 30 minutes.
          </div>
          <button type="button" className="oa-mx-retry" onClick={load}>
            <FontAwesomeIcon icon={faSyncAlt} /> Retry
          </button>
        </div>
      </div>
    );
  } else if (state.kind === 'disabled') {
    body = (
      <div className="oa-mx-statecard">
        <FontAwesomeIcon icon={faMoon} className="oa-mx-statecard-icon" />
        <div>
          <div className="oa-mx-statecard-title">Conversions is off</div>
          <div className="oa-mx-statecard-body">This screen is turned off on the server.</div>
          <button type="button" className="oa-mx-retry" onClick={load}>
            <FontAwesomeIcon icon={faSyncAlt} /> Retry
          </button>
        </div>
      </div>
    );
  } else if (state.kind === 'error') {
    body = <SectionError label="Conversions" error={state.error} onRetry={load} />;
  } else if (grid.offers.length === 0) {
    body = (
      <EmptyState
        title="No offers in this window"
        hint={`No offer mail was delivered in the last ${windowDays} days.${windowDays < 14 ? ' Try 14D.' : ''}`}
      />
    );
  } else {
    body = (
      <div className="oa-mx-layout">
        {/* ── The matrix ── */}
        <div className="oa-mx-tablewrap">
          <table className="oa-mx-table">
            <thead>
              <tr>
                <th className="oa-mx-th oa-mx-th--offer">Offer</th>
                {grid.isps.map((isp) => (
                  <th key={isp} className="oa-mx-th">{ispLabel(isp)}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {grid.offers.map((o) => (
                <tr key={o.key}>
                  <td className="oa-mx-offercell">
                    <button
                      type="button"
                      className="oa-mx-offerlink"
                      title={`${o.name} (${o.key}) — open profile`}
                      onClick={() => onOpenProfile(o.key, o.name)}
                    >
                      {o.name}
                    </button>
                    <div className="oa-mx-offersub">{fmtNum(o.delivered)} delivered</div>
                  </td>
                  {grid.isps.map((isp) => {
                    const r = o.cells.get(isp);
                    if (!r) {
                      return (
                        <td key={isp} className="oa-mx-cellwrap">
                          <div className="oa-mx-cell oa-mx-cell--empty" title="No sends for this offer × ISP in the window">—</div>
                        </td>
                      );
                    }
                    const v = metricValue(r, metric);
                    const low = r.badge === 'LOW_VOLUME';
                    // Sequential indigo ramp — magnitude on the selected metric
                    // only; LOW_VOLUME stays flat grey (no ramp).
                    const t = !low && v != null && grid.maxMetric > 0 ? Math.min(1, v / grid.maxMetric) : 0;
                    const bg = low
                      ? 'color-mix(in srgb, var(--j-text-muted) 10%, transparent)'
                      : `rgba(99,102,241,${(0.05 + 0.5 * t).toFixed(3)})`;
                    const volPct = grid.maxDelivered > 0 ? (r.delivered / grid.maxDelivered) * 100 : 0;
                    return (
                      <td key={isp} className="oa-mx-cellwrap">
                        <div
                          className={`oa-mx-cell${low ? ' oa-mx-cell--low' : ''}`}
                          style={{ background: bg }}
                          title={cellTitle(r)}
                          onClick={() => onOpenProfile(o.key, o.name, isp)}
                        >
                          {/* Keyboard route; a role=button cell can't hold the badge button. */}
                          <button
                            type="button"
                            className="oa-mx-cell-hit"
                            aria-label={`Open ${o.name}, ${isp}`}
                            onClick={(e) => {
                              e.stopPropagation();
                              onOpenProfile(o.key, o.name, isp);
                            }}
                          />
                          <div className="oa-mx-cell-top">
                            <span className="oa-mx-cell-value">{fmtMetric(v, metric)}</span>
                            <CornerBadge
                              badge={r.badge}
                              reason={r.badge_reason}
                              onClick={
                                badgeMeta(r.badge)?.clickable
                                  ? () => onOpenEvidence(o.key, o.name, isp, r.badge)
                                  : undefined
                              }
                            />
                          </div>
                          <div className="oa-mx-volbar" title={`${fmtNum(r.delivered)} delivered (bar scaled to the matrix max of ${fmtNum(grid.maxDelivered)})`}>
                            <div className="oa-mx-volbar-fill" style={{ width: `${Math.max(volPct, r.delivered > 0 ? 2 : 0)}%` }} />
                          </div>
                        </div>
                      </td>
                    );
                  })}
                </tr>
              ))}
            </tbody>
          </table>
          <div className="oa-mx-footnote">
            Each cell shows {METRICS.find((m) => m.id === metric)?.label}. The thin bar shows how much was delivered.
            Corner letters: B blocked · T slowed down · LQ bad addresses · LV too little mail to judge. A dash in RPM
            means we have no payout on record. Click a cell for the offer. Click a red or amber letter for the mail
            server replies.
          </div>
        </div>

        {/* ── Right rail: offer leaderboard ── */}
        <div className="oa-mx-rail">
          <div className="oa-mx-rail-title">
            <FontAwesomeIcon icon={faTrophy} /> Offer leaderboard
          </div>
          {leaderboard.map((o, i) => (
            <button
              key={o.key}
              type="button"
              className="oa-mx-leader"
              title={`${o.name} (${o.key}) — open profile`}
              onClick={() => onOpenProfile(o.key, o.name)}
            >
              <span className="oa-mx-leader-rank">{i + 1}</span>
              <span className="oa-mx-leader-main">
                <span className="oa-mx-leader-name">{o.name}</span>
                <span className="oa-mx-leader-sub">
                  RPM {o.rpm == null ? '—' : fmtMoney(o.rpm)} · clickers{' '}
                  <span title={rateWithDenom(o.clickerRate, o.delivered)}>{fmtRate(o.clickerRate)}</span> ·{' '}
                  {fmtNum(o.delivered)} delivered
                </span>
                {o.badges.length > 0 && (
                  <span className="oa-mx-leader-badges">
                    {o.badges.map((b) => (
                      <BadgePill key={b} badge={b} />
                    ))}
                  </span>
                )}
              </span>
            </button>
          ))}
        </div>
      </div>
    );
  }

  const snap = state.kind === 'ready' ? state.data : undefined;
  return (
    <div className="oa-mx-root">
      {controls}
      {body}
      <div className="oa-mx-timing">
        {snap?.refreshed_at
          ? `Updated ${new Date(snap.refreshed_at).toLocaleTimeString('en-US', { timeZone: 'America/Denver', hour: 'numeric', minute: '2-digit' })} Denver time`
          : ''}
      </div>
    </div>
  );
};

export default AlignmentMatrix;
