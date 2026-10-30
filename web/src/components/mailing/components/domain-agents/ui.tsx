// Domain Agents — shared dark-theme tokens + tiny loading/error primitives.

import React from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faSpinner, faTriangleExclamation, faRotateRight } from '@fortawesome/free-solid-svg-icons';

export const C = {
  bg: 'var(--j-bg)',
  panel: 'color-mix(in srgb, var(--j-shade) 60%, transparent)',
  panelBorder: 'color-mix(in srgb, var(--j-cyan) 12%, transparent)',
  text: 'var(--j-text)',
  muted: 'color-mix(in srgb, var(--j-text) 65%, transparent)',
  accent: 'var(--j-cyan)',
  accent2: 'var(--j-blue)',
  success: 'var(--j-success)',
  warning: 'var(--j-warn)',
  danger: 'var(--j-danger)',
} as const;

export const panelStyle: React.CSSProperties = {
  background: C.panel,
  border: `1px solid ${C.panelBorder}`,
  borderRadius: 10,
  padding: 16,
};

export const sectionTitleStyle: React.CSSProperties = {
  fontSize: 12,
  fontWeight: 700,
  letterSpacing: 1.2,
  textTransform: 'uppercase',
  color: C.accent,
  marginBottom: 12,
};

export const thStyle: React.CSSProperties = {
  textAlign: 'left',
  padding: '6px 10px',
  fontSize: 11,
  textTransform: 'uppercase',
  letterSpacing: 0.6,
  color: C.muted,
  borderBottom: `1px solid ${C.panelBorder}`,
  whiteSpace: 'nowrap',
};

export const tdStyle: React.CSSProperties = {
  padding: '6px 10px',
  fontSize: 13,
  color: C.text,
  borderBottom: '1px solid color-mix(in srgb, var(--j-cyan) 6%, transparent)',
  whiteSpace: 'nowrap',
};

export const btnStyle: React.CSSProperties = {
  background: 'color-mix(in srgb, var(--j-blue) 15%, transparent)',
  border: '1px solid color-mix(in srgb, var(--j-blue) 50%, transparent)',
  color: C.text,
  borderRadius: 6,
  padding: '7px 14px',
  fontSize: 13,
  cursor: 'pointer',
};

export const btnDisabledStyle: React.CSSProperties = {
  ...btnStyle,
  background: 'rgba(120,120,120,0.18)',
  border: '1px solid rgba(120,120,120,0.35)',
  color: C.muted,
  cursor: 'not-allowed',
  opacity: 0.6,
};

export const inputStyle: React.CSSProperties = {
  background: 'color-mix(in srgb, var(--j-shade) 80%, transparent)',
  border: `1px solid ${C.panelBorder}`,
  color: C.text,
  borderRadius: 6,
  padding: '6px 10px',
  fontSize: 13,
  width: '100%',
  boxSizing: 'border-box',
};

export const Loading: React.FC<{ label?: string }> = ({ label = 'Loading…' }) => (
  <div style={{ display: 'flex', alignItems: 'center', gap: 8, color: C.muted, fontSize: 13, padding: '14px 4px' }}>
    <FontAwesomeIcon icon={faSpinner} spin /> {label}
  </div>
);

export const ErrorState: React.FC<{ message: string; onRetry: () => void }> = ({ message, onRetry }) => (
  <div
    style={{
      display: 'flex',
      alignItems: 'center',
      gap: 10,
      padding: '10px 14px',
      borderRadius: 8,
      background: 'color-mix(in srgb, var(--j-danger) 10%, transparent)',
      border: '1px solid color-mix(in srgb, var(--j-danger) 40%, transparent)',
      color: C.danger,
      fontSize: 13,
    }}
  >
    <FontAwesomeIcon icon={faTriangleExclamation} />
    <span style={{ flex: 1 }}>{message}</span>
    <button onClick={onRetry} style={{ ...btnStyle, padding: '5px 12px' }}>
      <FontAwesomeIcon icon={faRotateRight} style={{ marginRight: 6 }} />
      Retry
    </button>
  </div>
);

export const fmtInt = (n: number): string => n.toLocaleString('en-US');

export const fmtPct = (frac: number): string =>
  Number.isFinite(frac) ? `${(frac * 100).toFixed(1)}%` : '—';

// For values the API already expresses as 0–100 (open_pct, human_open_pct_7d).
export const fmtPct100 = (pct: number): string =>
  Number.isFinite(pct) ? `${pct.toFixed(1)}%` : '—';
