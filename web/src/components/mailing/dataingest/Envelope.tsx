// Envelope.tsx — the two envelope-level displays every data-ingest pane owes
// the operator (PORTAL_DESIGN_SYSTEM §1.6, §6.2-6.3):
//   NoteBanner   — the API's own `note` printed VERBATIM, amber, never hidden
//                  behind a plausible-looking empty state;
//   FreshnessLine — as_of · source · cache age · query cost, on the pane that
//                  received them (not only the page header).

import React from 'react';
import { colors } from '../shared/theme';

export const NoteBanner: React.FC<{ note?: string | null; label?: string }> = ({ note, label }) => {
  if (!note) return null;
  return (
    <div
      role="note"
      style={{
        background: 'rgba(245,158,11,0.14)',
        border: '1px solid rgba(245,158,11,0.55)',
        borderRadius: 6,
        padding: '8px 12px',
        color: colors.warningText,
        fontSize: 12,
        fontWeight: 600,
      }}
    >
      {label ? `${label}: ` : ''}{note}
    </div>
  );
};

export const fmtAgo = (s: number | null | undefined): string => {
  if (typeof s !== 'number' || !Number.isFinite(s) || s < 0) return 'unknown';
  if (s < 60) return `${Math.round(s)}s`;
  if (s < 3600) return `${Math.round(s / 60)}m`;
  return `${(s / 3600).toFixed(1)}h`;
};

export const FreshnessLine: React.FC<{
  asOf?: string | null;
  source?: string | null;
  cacheAgeSeconds?: number | null;
  queryMs?: number | null;
  extra?: React.ReactNode;
}> = ({ asOf, source, cacheAgeSeconds, queryMs, extra }) => {
  const parts: React.ReactNode[] = [];
  if (asOf) parts.push(`as of ${asOf}`);
  if (source) parts.push(`source ${source}`);
  if (typeof cacheAgeSeconds === 'number' && Number.isFinite(cacheAgeSeconds)) parts.push(`snapshot ${fmtAgo(cacheAgeSeconds)} old`);
  if (typeof queryMs === 'number' && Number.isFinite(queryMs)) parts.push(`query ${queryMs.toLocaleString()} ms`);
  if (parts.length === 0 && !extra) return null;
  return (
    <div style={{ fontSize: 11, color: colors.textFaint, marginTop: 6, fontVariantNumeric: 'tabular-nums' }}>
      {parts.map((p, i) => (
        <span key={i}>{i > 0 ? ' · ' : ''}{p}</span>
      ))}
      {extra ? <span>{parts.length > 0 ? ' · ' : ''}{extra}</span> : null}
    </div>
  );
};
