// PerformanceSnapshot — current-day (Denver/MT) aggregate deliverability health,
// grouped by sending domain (default) or by offer. This is a deliverability read,
// NOT a conversions read: "does the inbox like this domain/offer today".
//
// Mounted at the TOP of the Campaigns screen, ABOVE the campaign list. It is
// strictly ADDITIVE and FAIL-SOFT: any fetch/render failure degrades to a small
// inline "unavailable — retry" row and NEVER breaks the campaigns list.
//
// Backend contract: GET /api/mailing/performance/snapshot?group_by=domain|offer
// (a parallel builder owns the endpoint — this codes to that exact shape).

import React, { useCallback, useEffect, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faPaperPlane,
  faEnvelope,
  faMousePointer,
  faShieldHalved,
  faRotateRight,
  faSpinner,
  faTriangleExclamation,
  faClock,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import { C } from './domain-agents/ui';

// ----------------------------------------------------------------------------
// Contract types — mirror the documented endpoint response exactly.
// ----------------------------------------------------------------------------

type GroupBy = 'domain' | 'offer';

interface SnapshotTotals {
  sent: number;
  delivered: number;
  reached: number;
  human_opens: number;
  clicks: number;
  clickers: number;
  hard: number;
  soft: number;
  unsub: number;
}

interface SnapshotRow {
  key: string;
  label: string;
  sent: number;
  delivered: number;
  reached: number;
  delivery_pct: number;
  human_opens: number;
  open_pct: number;
  clicks: number;
  clickers: number;
  click_pct: number;
  ctor_pct: number;
  hard: number;
  hard_pct: number;
  soft: number;
  soft_pct: number;
  unattributed: boolean;
  clicks_exact: boolean;
  volume_estimated: boolean;
}

interface SnapshotResponse {
  group_by: GroupBy;
  denver_date: string;
  as_of: string;
  totals: SnapshotTotals;
  rows: SnapshotRow[];
  notes: string[];
}

// ----------------------------------------------------------------------------
// Small local formatters (the response sends *_pct as percent numbers, e.g. 96.2).
// ----------------------------------------------------------------------------

const fmtInt = (n: number): string =>
  Number.isFinite(n) ? Math.round(n).toLocaleString('en-US') : '—';

const fmtPct = (pct: number): string =>
  Number.isFinite(pct) ? `${pct.toFixed(1)}%` : '—';

const fmtClock = (iso: string): string => {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  try {
    return d.toLocaleTimeString('en-US', {
      hour: '2-digit',
      minute: '2-digit',
      timeZone: 'America/Denver',
    });
  } catch {
    return d.toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit' });
  }
};

// Health coloring — delivery% green≥95 / amber≥85 / red<85.
const deliveryColor = (pct: number): string => {
  if (!Number.isFinite(pct)) return C.muted;
  if (pct >= 95) return C.success;
  if (pct >= 85) return C.warning;
  return C.danger;
};
// Hard% red>2, else neutral text.
const hardColor = (pct: number): string =>
  Number.isFinite(pct) && pct > 2 ? C.danger : C.text;
// Soft% amber>1, else neutral text.
const softColor = (pct: number): string =>
  Number.isFinite(pct) && pct > 1 ? C.warning : C.text;

// ----------------------------------------------------------------------------
// Styles — mirror the domain-agents dark-theme inline idiom.
// ----------------------------------------------------------------------------

const wrapStyle: React.CSSProperties = {
  background: C.panel,
  border: `1px solid ${C.panelBorder}`,
  borderRadius: 10,
  padding: 14,
  marginBottom: 14,
};

const titleStyle: React.CSSProperties = {
  fontSize: 12,
  fontWeight: 700,
  letterSpacing: 1.1,
  textTransform: 'uppercase',
  color: C.accent,
};

const segWrapStyle: React.CSSProperties = {
  display: 'inline-flex',
  border: `1px solid ${C.panelBorder}`,
  borderRadius: 7,
  overflow: 'hidden',
};

const segBtn = (active: boolean): React.CSSProperties => ({
  background: active ? 'rgba(0,176,255,0.20)' : 'transparent',
  color: active ? C.text : C.muted,
  border: 'none',
  padding: '5px 12px',
  fontSize: 12,
  fontWeight: 600,
  cursor: 'pointer',
});

const tileStyle: React.CSSProperties = {
  flex: 1,
  minWidth: 110,
  background: 'rgba(10,15,26,0.55)',
  border: `1px solid ${C.panelBorder}`,
  borderRadius: 8,
  padding: '10px 12px',
};

const tileLabelStyle: React.CSSProperties = {
  fontSize: 10.5,
  textTransform: 'uppercase',
  letterSpacing: 0.7,
  color: C.muted,
  display: 'flex',
  alignItems: 'center',
  gap: 6,
  marginBottom: 4,
};

const tileValueStyle: React.CSSProperties = {
  fontSize: 20,
  fontWeight: 700,
  color: C.text,
};

const thStyle: React.CSSProperties = {
  textAlign: 'right',
  padding: '6px 10px',
  fontSize: 10.5,
  textTransform: 'uppercase',
  letterSpacing: 0.5,
  color: C.muted,
  borderBottom: `1px solid ${C.panelBorder}`,
  whiteSpace: 'nowrap',
};

const tdStyle: React.CSSProperties = {
  padding: '6px 10px',
  fontSize: 12.5,
  color: C.text,
  borderBottom: '1px solid rgba(0,200,255,0.06)',
  whiteSpace: 'nowrap',
  textAlign: 'right',
};

const estMarkStyle: React.CSSProperties = {
  fontSize: 9.5,
  color: C.muted,
  marginLeft: 4,
  fontStyle: 'italic',
};

// A "volume-estimated" cell renders muted with a trailing "est." marker.
const Cell: React.FC<{
  value: string;
  color?: string;
  estimated?: boolean;
}> = ({ value, color, estimated }) => (
  <td style={{ ...tdStyle, color: estimated ? C.muted : color ?? C.text }}>
    {value}
    {estimated && <span style={estMarkStyle}>est.</span>}
  </td>
);

// ----------------------------------------------------------------------------
// Component
// ----------------------------------------------------------------------------

const PerformanceSnapshot: React.FC = () => {
  const [groupBy, setGroupBy] = useState<GroupBy>('domain');
  const [data, setData] = useState<SnapshotResponse | null>(null);
  const [loading, setLoading] = useState<boolean>(true);
  const [error, setError] = useState<string | null>(null);

  // Guard against state updates after unmount / out-of-order responses.
  const reqSeq = useRef<number>(0);
  const mounted = useRef<boolean>(true);

  const fetchSnapshot = useCallback(
    async (gb: GroupBy): Promise<void> => {
      const seq = ++reqSeq.current;
      setLoading(true);
      setError(null);
      try {
        const res = await apiFetch(
          `/api/mailing/performance/snapshot?group_by=${encodeURIComponent(gb)}`
        );
        if (!res.ok) {
          throw new Error(`HTTP ${res.status}`);
        }
        const json: SnapshotResponse = await res.json();
        if (!mounted.current || seq !== reqSeq.current) return;
        setData(json);
        setLoading(false);
      } catch (e) {
        if (!mounted.current || seq !== reqSeq.current) return;
        // FAIL-SOFT: capture the error, never rethrow — the campaigns list lives on.
        setError(e instanceof Error ? e.message : 'snapshot unavailable');
        setLoading(false);
      }
    },
    []
  );

  // Fetch on mount + on groupBy change.
  useEffect(() => {
    fetchSnapshot(groupBy);
  }, [groupBy, fetchSnapshot]);

  // 45s auto-refresh (re-reads the current groupBy via a ref so we don't reset
  // the interval on each toggle).
  const groupByRef = useRef<GroupBy>(groupBy);
  groupByRef.current = groupBy;
  useEffect(() => {
    mounted.current = true;
    const id = window.setInterval(() => {
      fetchSnapshot(groupByRef.current);
    }, 45000);
    return () => {
      mounted.current = false;
      window.clearInterval(id);
    };
  }, [fetchSnapshot]);

  const isOffer = groupBy === 'offer';
  const totals = data?.totals;

  // Totals strip pcts — computed from `totals` (raw counts) so they EXACTLY
  // mirror the per-row backend formulas (finalizeRow): delivery=delivered/sent,
  // open=human_opens/delivered, click=clickers/delivered. Using DISTINCT
  // clickers (not totals.clicks) and a /delivered denominator keeps the header
  // reconciled with its own rows AND with the #customer-acquisition Slack digest
  // (which counts distinct clickers). Divide-by-zero -> NaN (rendered as "—").
  // These are locally-computed from raw counts, so *100 is correct here (the
  // backend already emits 0-100 for the row *_pct fields — never reuse those).
  const totalDeliveryPct =
    totals && totals.sent > 0 ? (totals.delivered / totals.sent) * 100 : NaN;
  const totalOpenPct =
    totals && totals.delivered > 0
      ? (totals.human_opens / totals.delivered) * 100
      : NaN;
  const totalClickPct =
    totals && totals.delivered > 0
      ? (totals.clickers / totals.delivered) * 100
      : NaN;

  // Whole component wrapped so a render-time throw can't bubble into CampaignPortal.
  try {
    return (
      <div style={wrapStyle}>
        {/* Header row */}
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: 12,
            flexWrap: 'wrap',
            marginBottom: 12,
          }}
        >
          <span style={titleStyle}>Today's deliverability · MT</span>

          <div style={segWrapStyle} role="group" aria-label="Group by">
            <button
              style={segBtn(!isOffer)}
              onClick={() => setGroupBy('domain')}
              title="Group by sending domain (exact)"
            >
              Domain
            </button>
            <button
              style={segBtn(isOffer)}
              onClick={() => setGroupBy('offer')}
              title="Group by offer (clicks exact; volume best-effort)"
            >
              Offer
            </button>
          </div>

          <span
            style={{
              marginLeft: 'auto',
              display: 'inline-flex',
              alignItems: 'center',
              gap: 10,
              fontSize: 11.5,
              color: C.muted,
            }}
          >
            {data?.as_of && (
              <span title={`As of ${new Date(data.as_of).toLocaleString()}`}>
                <FontAwesomeIcon icon={faClock} style={{ marginRight: 4 }} />
                as of {fmtClock(data.as_of)} MT
              </span>
            )}
            <button
              onClick={() => fetchSnapshot(groupBy)}
              disabled={loading}
              style={{
                background: 'rgba(0,176,255,0.15)',
                border: '1px solid rgba(0,176,255,0.5)',
                color: C.text,
                borderRadius: 6,
                padding: '5px 11px',
                fontSize: 12,
                cursor: loading ? 'not-allowed' : 'pointer',
                display: 'inline-flex',
                alignItems: 'center',
                gap: 6,
                opacity: loading ? 0.6 : 1,
              }}
            >
              <FontAwesomeIcon icon={faRotateRight} spin={loading} /> Refresh
            </button>
          </span>
        </div>

        {/* FAIL-SOFT inline error — small, non-blocking */}
        {error && (
          <div
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: 10,
              padding: '8px 12px',
              marginBottom: 10,
              borderRadius: 8,
              background: 'rgba(233,69,96,0.10)',
              border: '1px solid rgba(233,69,96,0.35)',
              color: C.danger,
              fontSize: 12,
            }}
          >
            <FontAwesomeIcon icon={faTriangleExclamation} />
            <span style={{ flex: 1 }}>performance snapshot unavailable</span>
            <button
              onClick={() => fetchSnapshot(groupBy)}
              style={{
                background: 'transparent',
                border: '1px solid rgba(233,69,96,0.5)',
                color: C.danger,
                borderRadius: 6,
                padding: '3px 10px',
                fontSize: 11.5,
                cursor: 'pointer',
              }}
            >
              retry
            </button>
          </div>
        )}

        {/* Thin loading bar — no layout jump (skeleton replaces nothing structurally) */}
        {loading && !data && (
          <div
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: 8,
              color: C.muted,
              fontSize: 12.5,
              padding: '10px 2px',
            }}
          >
            <FontAwesomeIcon icon={faSpinner} spin /> Loading today's deliverability…
          </div>
        )}

        {/* Totals strip — render whenever we have data (even stale during a refresh) */}
        {data && totals && (
          <div
            style={{
              display: 'flex',
              gap: 10,
              flexWrap: 'wrap',
              marginBottom: 12,
            }}
          >
            <div style={tileStyle}>
              <div style={tileLabelStyle}>
                <FontAwesomeIcon icon={faPaperPlane} /> Sent
              </div>
              <div style={tileValueStyle}>{fmtInt(totals.sent)}</div>
            </div>
            <div style={tileStyle}>
              <div style={tileLabelStyle}>
                <FontAwesomeIcon icon={faShieldHalved} /> Delivery
              </div>
              <div style={{ ...tileValueStyle, color: deliveryColor(totalDeliveryPct) }}>
                {fmtPct(totalDeliveryPct)}
              </div>
            </div>
            <div style={tileStyle}>
              <div style={tileLabelStyle}>
                <FontAwesomeIcon icon={faEnvelope} /> Opens (human)
              </div>
              <div style={tileValueStyle}>
                {fmtInt(totals.human_opens)}{' '}
                <span style={{ fontSize: 12, color: C.accent }}>{fmtPct(totalOpenPct)}</span>
              </div>
            </div>
            <div style={tileStyle}>
              <div style={tileLabelStyle}>
                <FontAwesomeIcon icon={faMousePointer} /> Clicks
              </div>
              <div style={tileValueStyle}>
                {fmtInt(totals.clicks)}{' '}
                <span style={{ fontSize: 12, color: C.accent }}>{fmtPct(totalClickPct)}</span>
              </div>
            </div>
          </div>
        )}

        {/* Per-group table */}
        {data && data.rows.length === 0 && !loading && (
          <div style={{ color: C.muted, fontSize: 12.5, padding: '8px 2px' }}>
            No campaigns sent today yet.
          </div>
        )}

        {data && data.rows.length > 0 && (
          <>
            <div style={{ overflowX: 'auto' }}>
              <table style={{ borderCollapse: 'collapse', width: '100%' }}>
                <thead>
                  <tr>
                    <th style={{ ...thStyle, textAlign: 'left' }}>
                      {isOffer ? 'Offer' : 'Domain'}
                    </th>
                    <th style={thStyle}>Sent</th>
                    <th style={thStyle}>Deliv %</th>
                    <th style={thStyle}>Open %</th>
                    <th style={thStyle}>Click %</th>
                    {isOffer && <th style={thStyle}>Clickers</th>}
                    <th style={thStyle}>🔴 Hard %</th>
                    <th style={thStyle}>🟠 Soft %</th>
                  </tr>
                </thead>
                <tbody>
                  {data.rows.map((r) => {
                    const muted = r.unattributed;
                    const est = r.volume_estimated;
                    return (
                      <tr key={r.key}>
                        <td
                          style={{
                            ...tdStyle,
                            textAlign: 'left',
                            fontWeight: 600,
                            color: muted ? C.muted : C.text,
                            fontStyle: muted ? 'italic' : 'normal',
                          }}
                          title={r.label}
                        >
                          {r.label}
                        </td>
                        <Cell value={fmtInt(r.sent)} estimated={est} />
                        <Cell
                          value={fmtPct(r.delivery_pct)}
                          color={deliveryColor(r.delivery_pct)}
                          estimated={est}
                        />
                        <Cell
                          value={fmtPct(r.open_pct)}
                          color={C.accent}
                          estimated={est}
                        />
                        {/* Clicks are exact even in offer view — never marked est. */}
                        <Cell value={fmtPct(r.click_pct)} color={C.accent} />
                        {isOffer && <Cell value={fmtInt(r.clickers)} />}
                        <Cell
                          value={fmtPct(r.hard_pct)}
                          color={hardColor(r.hard_pct)}
                          estimated={est}
                        />
                        <Cell
                          value={fmtPct(r.soft_pct)}
                          color={softColor(r.soft_pct)}
                          estimated={est}
                        />
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>

            {/* OFFER-view honesty caption */}
            {isOffer && (
              <div
                style={{
                  marginTop: 8,
                  fontSize: 11,
                  color: C.muted,
                  fontStyle: 'italic',
                }}
              >
                Offer clicks are exact (by destination link); delivery/opens are
                best-effort by campaign (marked “est.”).
              </div>
            )}

            {/* Server notes, if any */}
            {data.notes && data.notes.length > 0 && (
              <div style={{ marginTop: 6, fontSize: 10.5, color: C.muted }}>
                {data.notes.map((n, i) => (
                  <div key={i}>· {n}</div>
                ))}
              </div>
            )}
          </>
        )}
      </div>
    );
  } catch {
    // Render-time fail-soft: collapse to a minimal inline notice.
    return (
      <div
        style={{
          ...wrapStyle,
          color: C.muted,
          fontSize: 12,
          display: 'flex',
          alignItems: 'center',
          gap: 8,
        }}
      >
        <FontAwesomeIcon icon={faTriangleExclamation} /> performance snapshot
        unavailable
      </div>
    );
  }
};

export default PerformanceSnapshot;
