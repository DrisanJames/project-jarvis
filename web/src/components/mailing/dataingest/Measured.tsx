// Measured.tsx — the one component that decides whether a number is shown.
//
// Rule (REQ D7): a field the API flags `not_measured` renders as the words
// "not yet measured", muted — never 0, never 0%. A `derived` field renders
// with a trailing `*` (PORTAL_DESIGN_SYSTEM §1.5). A measured field with a
// null value is ALSO "not yet measured": "could not measure" and "measured
// zero" are different facts and must not share a rendering.

import React from 'react';
import { colors } from '../shared/theme';
import { measured, fmtCount, type Count, type FieldFlags } from './api';

export const NOT_MEASURED = 'not yet measured';

export interface MeasuredProps {
  fields: FieldFlags | undefined;
  /** the field name inside the response envelope's `fields` map */
  name: string;
  value: Count | undefined;
  /** default: thousands-separated integer */
  format?: (n: number) => string;
  color?: string;
  /** appended after a real value (e.g. "/hr") */
  unit?: string;
  style?: React.CSSProperties;
}

export const Measured: React.FC<MeasuredProps> = ({ fields, name, value, format, color, unit, style }) => {
  const flag = measured(fields, name);
  if (flag === 'not_measured' || typeof value !== 'number' || !Number.isFinite(value)) {
    return (
      <span
        title={`${name}: the API reports this is not measured yet`}
        style={{ color: colors.textFaint, fontStyle: 'italic', fontWeight: 400, ...style }}
      >
        {NOT_MEASURED}
      </span>
    );
  }
  return (
    <span
      style={{ color: color ?? colors.text, fontVariantNumeric: 'tabular-nums', ...style }}
      title={flag === 'derived' ? `${name}: derived, not counted directly` : `${name}: measured`}
    >
      {(format ?? fmtCount)(value)}
      {unit ? <span style={{ fontSize: '0.75em', color: colors.textMuted }}>{unit}</span> : null}
      {flag === 'derived' ? <span style={{ color: colors.textMuted }}>*</span> : null}
    </span>
  );
};

export default Measured;
