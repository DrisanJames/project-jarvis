// EvidencePanel.tsx — Offer Alignment Level 2: grouped SMTP evidence.
//
// Reached by clicking a BLOCKING/THROTTLED badge. Shows the grouped DSN
// evidence behind the badge — family, code, plain-English meaning (decoded
// server-side: HM08 = Apple local-policy block, 4.7.650 = Microsoft velocity
// throttle, 5.1.1 = invalid mailbox, …), counts, first/last seen, and a
// monospace sample diagnostic line.
//
// Data: GET /api/mailing/offer-alignment/evidence?offer=&isp=&from=&to=

import React, { useCallback, useEffect, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faSpinner, faSyncAlt, faArrowLeft } from '@fortawesome/free-solid-svg-icons';
import { SectionError, EmptyState } from '../shared/ui';
import { EvidenceResponse, EvidenceRow, fetchAlignment, isAbortError, fmtNum, ispLabel } from './types';
import { BadgePill } from './badges';
import './EvidencePanel.css';

interface EvidenceState {
  kind: 'loading' | 'error' | 'ready' | 'unavailable';
  error?: string;
  rows?: EvidenceRow[];
}

export const EvidencePanel: React.FC<{
  offer: string;
  offerName: string;
  isp: string;
  badge?: string;
  from: string;
  to: string;
  onBack: () => void;
}> = ({ offer, offerName, isp, badge, from, to, onBack }) => {
  const [state, setState] = useState<EvidenceState>({ kind: 'loading' });
  const [fetchedAt, setFetchedAt] = useState('');
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(async () => {
    abortRef.current?.abort();
    const ctl = new AbortController();
    abortRef.current = ctl;
    setState({ kind: 'loading' });
    try {
      const qs = new URLSearchParams({ offer, isp, from, to });
      const out = await fetchAlignment<EvidenceResponse>(`/api/mailing/offer-alignment/evidence?${qs.toString()}`, ctl.signal);
      if (ctl.signal.aborted) return;
      if (out.kind === 'ready') {
        const rows = Array.isArray(out.data.rows) ? out.data.rows : [];
        setState({ kind: 'ready', rows: [...rows].sort((a, b) => b.count - a.count) });
      } else {
        // building/disabled — evidence is lake-backed; render the designed
        // unavailable state rather than an error.
        setState({ kind: 'unavailable' });
      }
      setFetchedAt(new Date().toLocaleTimeString('en-US', { hour12: false }));
    } catch (e) {
      if (isAbortError(e)) return;
      setState({ kind: 'error', error: e instanceof Error ? e.message : String(e) });
    }
  }, [offer, isp, from, to]);

  useEffect(() => {
    load();
    return () => abortRef.current?.abort();
  }, [load]);

  let body: React.ReactNode;
  if (state.kind === 'loading') {
    body = (
      <div className="oa-ev-loading">
        <FontAwesomeIcon icon={faSpinner} spin /> Loading SMTP evidence…
      </div>
    );
  } else if (state.kind === 'error') {
    body = <SectionError label="SMTP evidence" error={state.error} onRetry={load} />;
  } else if (state.kind === 'unavailable') {
    body = (
      <EmptyState
        title="Evidence unavailable"
        hint="The delivery-lake read behind SMTP evidence is off or still building — retry shortly."
      />
    );
  } else if (!state.rows || state.rows.length === 0) {
    body = (
      <EmptyState
        title="No grouped SMTP evidence"
        hint={`No DSN rows for ${offerName || offer} at ${ispLabel(isp)} between ${from} and ${to}.`}
      />
    );
  } else {
    const total = state.rows.reduce((a, r) => a + r.count, 0);
    body = (
      <div className="oa-ev-tablewrap">
        <table className="oa-ev-table">
          <thead>
            <tr>
              <th className="oa-ev-th oa-ev-th--left">Family</th>
              <th className="oa-ev-th oa-ev-th--left">DSN</th>
              <th className="oa-ev-th oa-ev-th--left">What it means</th>
              <th className="oa-ev-th">Count</th>
              <th className="oa-ev-th oa-ev-th--left">First seen</th>
              <th className="oa-ev-th oa-ev-th--left">Last seen</th>
              <th className="oa-ev-th oa-ev-th--left">Sample diagnostic</th>
            </tr>
          </thead>
          <tbody>
            {state.rows.map((r, idx) => (
              <tr key={`${r.dsn_family}|${r.dsn_code}|${idx}`} className="oa-ev-tr">
                <td className="oa-ev-td oa-ev-td--left oa-ev-mono">{r.dsn_family || '—'}</td>
                <td className="oa-ev-td oa-ev-td--left oa-ev-mono">{r.dsn_code || '—'}</td>
                <td className="oa-ev-td oa-ev-td--left oa-ev-meaning">{r.meaning || '—'}</td>
                <td className="oa-ev-td" title={total > 0 ? `${((r.count / total) * 100).toFixed(1)}% of ${fmtNum(total)} grouped events` : undefined}>
                  {fmtNum(r.count)}
                </td>
                <td className="oa-ev-td oa-ev-td--left oa-ev-when">{r.first_seen || '—'}</td>
                <td className="oa-ev-td oa-ev-td--left oa-ev-when">{r.last_seen || '—'}</td>
                <td className="oa-ev-td oa-ev-td--left oa-ev-diag" title={r.sample_diag}>
                  {r.sample_diag || '—'}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        <div className="oa-ev-footnote">
          {fmtNum(total)} grouped events across {state.rows.length} DSN famil{state.rows.length === 1 ? 'y' : 'ies'}.
          Meanings are decoded server-side from the DSN family (e.g. HM08 = Apple local-policy block — offer/content,
          not audience; 4.7.650 = Microsoft velocity throttle — capacity, slow down; 5.1.1 = invalid mailbox — list
          quality).
        </div>
      </div>
    );
  }

  return (
    <div className="oa-ev-root">
      <div className="oa-ev-header">
        <button type="button" className="oa-ev-back" onClick={onBack}>
          <FontAwesomeIcon icon={faArrowLeft} /> {offerName || offer}
        </button>
        <div className="oa-ev-titleblock">
          <div className="oa-ev-title">
            SMTP evidence — {offerName || offer} × {ispLabel(isp)}
            {badge && <BadgePill badge={badge} />}
          </div>
          <div className="oa-ev-sub">
            Grouped rejection/deferral diagnostics behind this badge · {from} → {to} · America/Denver
          </div>
        </div>
        <button type="button" className="oa-ev-refresh" onClick={load} disabled={state.kind === 'loading'}>
          <FontAwesomeIcon icon={state.kind === 'loading' ? faSpinner : faSyncAlt} spin={state.kind === 'loading'} /> Refresh
        </button>
      </div>
      {body}
      {fetchedAt && <div className="oa-ev-timing">fetched {fetchedAt}</div>}
    </div>
  );
};

export default EvidencePanel;
