// badges.tsx — the DELIVERY-HEALTH badge channel for Offer Alignment.
//
// Badges are an independent visual channel from the performance color ramp
// (design doc: "PERFORMANCE and DELIVERY HEALTH — never blended"). Status
// colors are reserved and never carried by color alone: every badge renders a
// short letter code + tooltip (corner form) or an uppercase text pill (full
// form). HEALTHY renders no badge.
//
// BLOCKING and THROTTLED badges are drill targets — clicking one opens the
// Level-2 SMTP evidence panel.

import React from 'react';
import './badges.css';

export interface BadgeMeta {
  color: string;
  short: string;
  label: string;
  blurb: string;
  clickable: boolean; // true ⇒ opens the SMTP evidence panel
}

// HARD RULES: BLOCKING red #ef4444, THROTTLED amber #f59e0b (capacity
// language, never reputation), LIST_QUALITY orange, LOW_VOLUME grey.
export const BADGE_META: Record<string, BadgeMeta> = {
  BLOCKING: {
    color: '#ef4444',
    short: 'B',
    label: 'Blocking',
    blurb: 'ISP is rejecting on reputation/policy. Click for the SMTP evidence.',
    clickable: true,
  },
  THROTTLED: {
    color: '#f59e0b',
    short: 'T',
    label: 'Throttled',
    blurb: 'Deferral/velocity throttle — capacity, not reputation. Click for the SMTP evidence.',
    clickable: true,
  },
  LIST_QUALITY: {
    color: '#fb923c',
    short: 'LQ',
    label: 'List quality',
    blurb: 'Hard-bounce (invalid mailbox) rate is elevated for this cell.',
    clickable: false,
  },
  LOW_VOLUME: {
    color: '#64748b',
    short: 'LV',
    label: 'Low volume',
    blurb: 'Under the sample floor — metrics here are not rankable.',
    clickable: false,
  },
  NO_LAKE: {
    color: '#64748b',
    short: 'NL',
    label: 'No delivery data',
    blurb: 'Delivery lake read is off — engagement-only numbers for this cell.',
    clickable: false,
  },
};

/** Meta for a badge string; null for HEALTHY / empty (⇒ render nothing). */
export function badgeMeta(badge: string | null | undefined): BadgeMeta | null {
  if (!badge || badge === 'HEALTHY') return null;
  return (
    BADGE_META[badge] ?? {
      color: '#64748b',
      short: '?',
      label: badge,
      blurb: '',
      clickable: false,
    }
  );
}

/** Small letter chip for a matrix-cell corner. */
export const CornerBadge: React.FC<{
  badge: string;
  reason?: string;
  onClick?: () => void;
}> = ({ badge, reason, onClick }) => {
  const meta = badgeMeta(badge);
  if (!meta) return null;
  const title = `${meta.label}${reason ? ` — ${reason}` : ''}${meta.blurb ? `. ${meta.blurb}` : ''}`;
  const style: React.CSSProperties = {
    color: meta.color,
    background: `${meta.color}22`,
    border: `1px solid ${meta.color}66`,
  };
  if (meta.clickable && onClick) {
    return (
      <button
        type="button"
        className="oa-badge-corner oa-badge-corner--btn"
        style={style}
        title={title}
        onClick={(e) => {
          e.stopPropagation();
          onClick();
        }}
      >
        {meta.short}
      </button>
    );
  }
  return (
    <span className="oa-badge-corner" style={style} title={title}>
      {meta.short}
    </span>
  );
};

/** Full uppercase text pill (profile tables, leaderboard). */
export const BadgePill: React.FC<{
  badge: string;
  reason?: string;
  onClick?: () => void;
}> = ({ badge, reason, onClick }) => {
  const meta = badgeMeta(badge);
  if (!meta) return <span className="oa-badge-healthy">healthy</span>;
  const title = `${meta.label}${reason ? ` — ${reason}` : ''}${meta.blurb ? `. ${meta.blurb}` : ''}`;
  const style: React.CSSProperties = {
    color: meta.color,
    background: `${meta.color}22`,
    border: `1px solid ${meta.color}66`,
  };
  if (meta.clickable && onClick) {
    return (
      <button type="button" className="oa-badge-pill oa-badge-pill--btn" style={style} title={title} onClick={onClick}>
        {meta.label}
      </button>
    );
  }
  return (
    <span className="oa-badge-pill" style={style} title={title}>
      {meta.label}
    </span>
  );
};

/** Tiny "inferred" attribution chip (historical, name/slug-inferred rows). */
export const InferredChip: React.FC = () => (
  <span
    className="oa-inferred-chip"
    title="Historical attribution — inferred from campaign name/slug, not a deploy-time stamp."
  >
    inferred
  </span>
);
