import React from 'react';
import { render, screen } from '@testing-library/react';
import { describe, it, expect } from 'vitest';

import { Measured } from './Measured';
import { measured, hasValue, type FieldFlags } from './api';

/**
 * What this PINS (REQ 2026-09-20 D7, PORTAL_DESIGN_SYSTEM §6.2):
 *  - a field the API flags `not_measured` renders the WORDS "not yet measured",
 *    never 0 and never 0% — a zero-looking tile reads as "nothing landed";
 *  - a measured field renders the number the backend reported;
 *  - a null value is not measured either, whatever the flag says;
 *  - an UNFLAGGED field is treated as not measured, so a backend that forgets
 *    to flag something can never make the screen invent a number.
 */

const FIELDS: FieldFlags = {
  'dynamic.arrived': 'measured',
  'dynamic.arrival_rate': 'not_measured',
  'at_rest.staged': 'derived',
};

describe('Measured', () => {
  it('renders "not yet measured" for a not_measured field, never 0', () => {
    render(<Measured fields={FIELDS} name="dynamic.arrival_rate" value={0} />);
    expect(screen.getByText('not yet measured')).toBeInTheDocument();
    expect(screen.queryByText('0')).not.toBeInTheDocument();
  });

  it('renders the number for a measured field', () => {
    render(<Measured fields={FIELDS} name="dynamic.arrived" value={313240} />);
    expect(screen.getByText('313,240')).toBeInTheDocument();
    expect(screen.queryByText('not yet measured')).not.toBeInTheDocument();
  });

  it('marks a derived number with a trailing asterisk', () => {
    render(<Measured fields={FIELDS} name="at_rest.staged" value={1959346} />);
    expect(screen.getByText('1,959,346')).toBeInTheDocument();
    expect(screen.getByText('*')).toBeInTheDocument();
  });

  it('renders "not yet measured" when the value is null even if the field is flagged measured', () => {
    render(<Measured fields={FIELDS} name="dynamic.arrived" value={null} />);
    expect(screen.getByText('not yet measured')).toBeInTheDocument();
  });

  it('treats an unflagged field as not measured', () => {
    render(<Measured fields={FIELDS} name="totals.duplicates" value={42} />);
    expect(screen.getByText('not yet measured')).toBeInTheDocument();
    expect(screen.queryByText('42')).not.toBeInTheDocument();
  });

  it('measured()/hasValue() agree with what the component renders', () => {
    expect(measured(FIELDS, 'dynamic.arrived')).toBe('measured');
    expect(measured(FIELDS, 'dynamic.arrival_rate')).toBe('not_measured');
    expect(measured(undefined, 'anything')).toBe('not_measured');
    expect(hasValue(FIELDS, 'dynamic.arrived', 5)).toBe(true);
    expect(hasValue(FIELDS, 'dynamic.arrived', null)).toBe(false);
    expect(hasValue(FIELDS, 'dynamic.arrival_rate', 5)).toBe(false);
  });
});
