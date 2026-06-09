// Domain Agents — shared dark-theme tokens + tiny loading/error primitives.

import React from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faSpinner, faTriangleExclamation, faRotateRight } from '@fortawesome/free-solid-svg-icons';

export const C = {
  bg: '#0a0f1a',
  panel: 'rgba(13,21,38,0.6)',
  panelBorder: 'rgba(0,200,255,0.12)',
  text: '#e0e6f0',
  muted: 'rgba(180,210,240,0.65)',
  accent: '#00e5ff',
  accent2: '#00b0ff',
  success: '#00b894',
  warning: '#fdcb6e',
  danger: '#e94560',
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
  borderBottom: '1px solid rgba(0,200,255,0.06)',
  whiteSpace: 'nowrap',
};

export const btnStyle: React.CSSProperties = {
  background: 'rgba(0,176,255,0.15)',
  border: '1px solid rgba(0,176,255,0.5)',
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
  background: 'rgba(10,15,26,0.8)',
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
      background: 'rgba(233,69,96,0.10)',
      border: '1px solid rgba(233,69,96,0.4)',
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
