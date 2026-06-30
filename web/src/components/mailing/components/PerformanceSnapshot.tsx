// PerformanceSnapshot — current-day (Denver/MT) aggregate deliverability health,
// grouped by sending domain (default) or by offer. This is a deliverability read,
// NOT a conversions read: "does the inbox like this domain/offer today".
//
// Mounted at the TOP of the Campaigns screen, ABOVE the campaign list, where it
// is now the dashboard HERO: a clear Today's-Deliverability summary (Delivered %,
// Opens, Clicks) with the raw per-group breakdown demoted beneath it. It is
// strictly ADDITIVE and FAIL-SOFT: any fetch/render failure degrades to a small
// inline notice and NEVER breaks the campaigns list.
//
// Refresh is the Delivery-Queue ANTI-JANK pattern (shared/usePolling): the
// spinner is gated to first paint only, data is replaced atomically on success,
// last-good data stays mounted on a failed refresh, and a background poll never
// blanks or reflows the hero.
//
// Backend contract: GET /api/mailing/performance/snapshot?group_by=domain|offer.

import React from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faPaperPlane,
  faShieldHalved,
  faRotateRight,
  faSpinner,
  faTriangleExclamation,
  faClock,
  faTags,
  faChevronDown,
  faChevronUp,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import { colors, alpha } from '../shared/theme';
import { Panel, SectionHeader, Stat, EmptyState, LivePill, PortalKeyframes } from '../shared/ui';
import { usePolling } from '../shared/usePolling';

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

const POLL_INTERVAL_MS = 45000;

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
  if (!Number.isFinite(pct)) return colors.textMuted;
  if (pct >= 95) return colors.success;
  if (pct >= 85) return colors.warning;
  return colors.danger;
};
// Hard% red>2, else neutral text.
const hardColor = (pct: number): string =>
  Number.isFinite(pct) && pct > 2 ? colors.danger : colors.text;
// Soft% amber>1, else neutral text.
const softColor = (pct: number): string =>
  Number.isFinite(pct) && pct > 1 ? colors.warning : colors.text;

// ----------------------------------------------------------------------------
// Indigo styles (shared design system tokens — no inline cyan).
// ----------------------------------------------------------------------------

const segWrapStyle: React.CSSProperties = {
  display: 'inline-flex',
  border: `1px solid ${colors.panelBorder}`,
  borderRadius: 7,
  overflow: 'hidden',
};

const segBtn = (active: boolean): React.CSSProperties => ({
  background: active ? alpha(colors.indigo500, '22') : 'transparent',
  color: active ? colors.indigo200 : colors.textMuted,
  border: 'none',
  padding: '5px 12px',
  fontSize: 12,
  fontWeight: 600,
  cursor: 'pointer',
});

const subHeaderStyle: React.CSSProperties = {
  fontSize: 11,
  fontWeight: 700,
  letterSpacing: 0.6,
  textTransform: 'uppercase',
  color: colors.textMuted,
  display: 'flex',
  alignItems: 'center',
  gap: 7,
  marginBottom: 8,
};

const thStyle: React.CSSProperties = {
  textAlign: 'right',
  padding: '6px 10px',
  fontSize: 10.5,
  textTransform: 'uppercase',
  letterSpacing: 0.5,
  color: colors.textMuted,
  borderBottom: `1px solid ${colors.hairline}`,
  whiteSpace: 'nowrap',
};

const tdStyle: React.CSSProperties = {
  padding: '6px 10px',
  fontSize: 12.5,
  color: colors.text,
  borderBottom: `1px solid ${colors.divider}`,
  whiteSpace: 'nowrap',
  textAlign: 'right',
  fontVariantNumeric: 'tabular-nums',
};

const estMarkStyle: React.CSSProperties = {
  fontSize: 9.5,
  color: colors.textMuted,
  marginLeft: 4,
  fontStyle: 'italic',
};

// A "volume-estimated" cell renders muted with a trailing "est." marker.
const Cell: React.FC<{
  value: string;
  color?: string;
  estimated?: boolean;
}> = ({ value, color, estimated }) => (
  <td style={{ ...tdStyle, color: estimated ? colors.textMuted : color ?? colors.text }}>
    {value}
    {estimated && <span style={estMarkStyle}>est.</span>}
  </td>
);

// ----------------------------------------------------------------------------
// Component
// ----------------------------------------------------------------------------

const PerformanceSnapshot: React.FC = () => {
  const [groupBy, setGroupBy] = React.useState<GroupBy>('domain');
  // Breakdown table is opt-in detail: collapsed by default, toggled by its header.
  const [breakdownOpen, setBreakdownOpen] = React.useState(false);

  // Anti-jank polling: spinner only on first paint, atomic success-only swaps,
  // last-good data stays mounted on a failed refresh. groupBy is a dep so a
  // toggle refetches immediately without blanking the hero.
  const fetcher = React.useCallback(
    async (signal: AbortSignal): Promise<SnapshotResponse> => {
      const res = await apiFetch(
        `/api/mailing/performance/snapshot?group_by=${encodeURIComponent(groupBy)}`,
        { signal }
      );
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      return (await res.json()) as SnapshotResponse;
    },
    [groupBy]
  );

  const { data, loading, error, secondsSinceUpdate, live, refresh } = usePolling<SnapshotResponse>(
    fetcher,
    POLL_INTERVAL_MS,
    [groupBy]
  );

  const isOffer = groupBy === 'offer';
  const totals = data?.totals;

  // Totals strip pcts — computed from `totals` (raw counts) so they EXACTLY
  // mirror the per-row backend formulas (finalizeRow): delivery=delivered/sent,
  // open=human_opens/delivered, click=clickers/delivered. Using DISTINCT
  // clickers (not totals.clicks) and a /delivered denominator keeps the header
  // reconciled with its own rows AND with the #customer-acquisition Slack digest
  // (which counts distinct clickers). Divide-by-zero -> NaN (rendered as "—").
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

  // Whole render wrapped so a render-time throw can't bubble into CampaignPortal.
  try {
    return (
      <div style={{ marginBottom: 14 }}>
        <PortalKeyframes />

        {/* ── HERO: Today's Deliverability ─────────────────────────────────── */}
        <Panel accent={colors.indigo500}>
          <SectionHeader
            title="Today's Deliverability · MT"
            icon={faShieldHalved}
            right={
              <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
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
                {data && <LivePill live={live} agoSeconds={secondsSinceUpdate} />}
                {data?.as_of && (
                  <span
                    style={{ fontSize: 11.5, color: colors.textMuted }}
                    title={`As of ${new Date(data.as_of).toLocaleString()}`}
                  >
                    <FontAwesomeIcon icon={faClock} style={{ marginRight: 4 }} />
                    as of {fmtClock(data.as_of)} MT
                  </span>
                )}
                <button
                  onClick={refresh}
                  style={{
                    background: alpha(colors.indigo500, '22'),
                    border: `1px solid ${alpha(colors.indigo500, '66')}`,
                    color: colors.indigo200,
                    borderRadius: 6,
                    padding: '5px 11px',
                    fontSize: 12,
                    fontWeight: 600,
                    cursor: 'pointer',
                    display: 'inline-flex',
                    alignItems: 'center',
                    gap: 6,
                  }}
                >
                  <FontAwesomeIcon icon={faRotateRight} spin={loading} /> Refresh
                </button>
              </div>
            }
          />

          {/* FAIL-SOFT inline error — small, non-blocking (data stays mounted) */}
          {error && (
            <div
              style={{
                display: 'flex',
                alignItems: 'center',
                gap: 10,
                padding: '8px 12px',
                marginBottom: 10,
                borderRadius: 8,
                background: 'rgba(239,68,68,0.08)',
                border: '1px solid rgba(239,68,68,0.25)',
                color: colors.dangerText,
                fontSize: 12,
              }}
            >
              <FontAwesomeIcon icon={faTriangleExclamation} />
              <span style={{ flex: 1 }}>
                performance snapshot did not refresh{data ? ' — showing last known data' : ''}
              </span>
              <button
                onClick={refresh}
                style={{
                  background: 'transparent',
                  border: '1px solid rgba(239,68,68,0.4)',
                  color: colors.dangerText,
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

          {/* First-paint loader only — never on background polls (no layout jump) */}
          {loading && !data ? (
            <div
              style={{
                display: 'flex',
                alignItems: 'center',
                gap: 8,
                color: colors.textMuted,
                fontSize: 12.5,
                padding: '10px 2px',
                minHeight: 52,
              }}
            >
              <FontAwesomeIcon icon={faSpinner} spin /> Loading today's deliverability…
            </div>
          ) : (
            data &&
            totals && (
              <div
                style={{
                  display: 'flex',
                  gap: 28,
                  flexWrap: 'wrap',
                  alignItems: 'flex-start',
                  marginTop: 4,
                  minHeight: 52,
                }}
              >
                <Stat label="Sent" value={fmtInt(totals.sent)} />
                <Stat
                  label="Delivered"
                  value={fmtPct(totalDeliveryPct)}
                  color={deliveryColor(totalDeliveryPct)}
                  sub={`${fmtInt(totals.delivered)} delivered`}
                />
                <Stat
                  label="Opens (human)"
                  value={fmtInt(totals.human_opens)}
                  sub={
                    <span style={{ color: colors.indigo300 }}>{fmtPct(totalOpenPct)} of delivered</span>
                  }
                />
                <Stat
                  label="Clicks"
                  value={fmtInt(totals.clicks)}
                  sub={
                    <span style={{ color: colors.indigo300 }}>{fmtPct(totalClickPct)} of delivered</span>
                  }
                />
              </div>
            )
          )}
        </Panel>

        {/* ── BOTTOM SECTION: collapsible raw per-group breakdown ───────────── */}
        {data && (
          <Panel style={{ marginTop: 12 }}>
            {/* Clickable header row — the ENTIRE row is the toggle target
                (far left, center, far right all work). Chevron points DOWN
                when collapsed (click to expand), UP when expanded (collapse).
                Rendered as a div role="button" so the full-width flex layout
                and edge-to-edge hit area are honored reliably (a native
                <button> does not behave as a flex container in all browsers). */}
            <div
              role="button"
              tabIndex={0}
              onClick={() => setBreakdownOpen((o) => !o)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' || e.key === ' ') {
                  e.preventDefault();
                  setBreakdownOpen((o) => !o);
                }
              }}
              aria-expanded={breakdownOpen}
              style={{
                ...subHeaderStyle,
                width: '100%',
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'space-between',
                marginBottom: breakdownOpen ? 8 : 0,
                background: 'transparent',
                border: 'none',
                padding: 0,
                cursor: 'pointer',
                userSelect: 'none',
              }}
              title={breakdownOpen ? 'Hide breakdown' : 'Show breakdown'}
            >
              <span style={{ display: 'flex', alignItems: 'center', gap: 7 }}>
                <FontAwesomeIcon icon={faTags} style={{ color: colors.indigo400 }} />
                Breakdown by {isOffer ? 'offer' : 'sending domain'}
              </span>
              <FontAwesomeIcon
                icon={breakdownOpen ? faChevronUp : faChevronDown}
                style={{ color: colors.indigo400, fontSize: 11, transition: 'transform 150ms ease' }}
              />
            </div>

            {breakdownOpen &&
              (data.rows.length === 0 ? (
              isOffer ? (
                <EmptyState
                  icon={faTags}
                  title="No offer mapping configured"
                  hint="No offers resolved for today's sends yet — the Domain view is exact."
                />
              ) : (
                <EmptyState icon={faPaperPlane} title="No campaigns sent today yet" />
              )
            ) : (
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
                                color: muted ? colors.textMuted : colors.heading,
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
                            <Cell value={fmtPct(r.open_pct)} color={colors.indigo300} estimated={est} />
                            {/* Clicks are exact even in offer view — never marked est. */}
                            <Cell value={fmtPct(r.click_pct)} color={colors.indigo300} />
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
                      color: colors.textMuted,
                      fontStyle: 'italic',
                    }}
                  >
                    Offer clicks are exact (by destination link); delivery/opens are
                    best-effort by campaign (marked “est.”).
                  </div>
                )}

                {/* Server notes, if any */}
                {data.notes && data.notes.length > 0 && (
                  <div style={{ marginTop: 6, fontSize: 10.5, color: colors.textFaint }}>
                    {data.notes.map((n, i) => (
                      <div key={i}>· {n}</div>
                    ))}
                  </div>
                )}
              </>
              ))}
          </Panel>
        )}
      </div>
    );
  } catch {
    // Render-time fail-soft: collapse to a minimal inline notice.
    return (
      <Panel style={{ marginBottom: 14, color: colors.textMuted, fontSize: 12 }}>
        <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
          <FontAwesomeIcon icon={faTriangleExclamation} /> performance snapshot unavailable
        </span>
      </Panel>
    );
  }
};

export default PerformanceSnapshot;
