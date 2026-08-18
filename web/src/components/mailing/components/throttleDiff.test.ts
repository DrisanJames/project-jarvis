// throttleDiff.test.ts — Pipeline Cockpit P2 DoD: the replacement semantics
// the diff preview renders must be provably correct (the write is a LIVE
// enforcement input; the preview is the operator's last look before the
// typed-name confirm).

import { describe, it, expect } from 'vitest';
import {
  buildThrottleDiff, buildThrottlePayload, validateThrottleRows,
  type ThrottleRow,
} from './throttleDiff';

const row = (isp: string, pct: number, wave: number, day: number | null): ThrottleRow =>
  ({ isp, pct_override: pct, max_per_wave: wave, daily_cap: day });

describe('buildThrottleDiff — delete-and-replace semantics', () => {
  it('marks an omitted ISP as removed and calls out the lost lane daily budget', () => {
    const diff = buildThrottleDiff([row('gmail', 0.4, 1000, 500)], []);
    expect(diff).toHaveLength(1);
    expect(diff[0].kind).toBe('removed');
    expect(diff[0].notes.join(' ')).toContain('global defaults');
    expect(diff[0].notes.join(' ')).toContain('500');
    expect(diff[0].notes.join(' ')).toContain('REMOVED');
  });

  it('removed ISP without a daily budget gets only the fallback note', () => {
    const diff = buildThrottleDiff([row('aol', 0.1, 200, null)], []);
    expect(diff[0].kind).toBe('removed');
    expect(diff[0].notes).toHaveLength(1);
  });

  it('marks a new ISP as added and flags daily_cap 0 as hard-suppression', () => {
    const diff = buildThrottleDiff([], [row('microsoft', 0.2, 0, 0)]);
    expect(diff[0].kind).toBe('added');
    expect(diff[0].notes.join(' ')).toContain('HARD-SUPPRESSED');
  });

  it('treats an included row with omitted daily_cap as PRESERVING the prior budget', () => {
    // Server carve-out: an ISP included without daily_cap keeps its prior
    // lane budget — so identical other fields means UNCHANGED, not changed.
    const diff = buildThrottleDiff(
      [row('yahoo', 0.3, 400, 150)],
      [row('yahoo', 0.3, 400, null)],
    );
    expect(diff[0].kind).toBe('unchanged');
    expect(diff[0].notes.join(' ')).toContain('150');
    expect(diff[0].notes.join(' ')).toContain('preserved');
    expect(diff[0].after?.daily_cap).toBe(150);
  });

  it('reports field-level changes', () => {
    const diff = buildThrottleDiff(
      [row('gmail', 0.4, 1000, 500)],
      [row('gmail', 0.5, 800, 500)],
    );
    expect(diff[0].kind).toBe('changed');
    const notes = diff[0].notes.join(' ');
    expect(notes).toContain('0.4 → 0.5');
    expect(notes).toContain('1000 → 800');
  });

  it('normalizes ISP case/whitespace like the server does', () => {
    const diff = buildThrottleDiff(
      [row('gmail', 0.4, 1000, null)],
      [row(' GMAIL ', 0.4, 1000, null)],
    );
    expect(diff).toHaveLength(1);
    expect(diff[0].kind).toBe('unchanged');
  });

  it('ignores blank-ISP editor rows', () => {
    const diff = buildThrottleDiff([], [row('', 0.4, 0, null)]);
    expect(diff).toHaveLength(0);
  });
});

describe('validateThrottleRows — client mirror of server validation', () => {
  it('rejects pct outside [0,1] (the server 400s the same input)', () => {
    expect(validateThrottleRows([row('gmail', 1.5, 0, null)]).join(' '))
      .toContain('between 0 and 1');
    expect(validateThrottleRows([row('gmail', NaN, 0, null)])).not.toHaveLength(0);
  });
  it('rejects duplicates and negatives, accepts a clean set', () => {
    expect(validateThrottleRows([row('gmail', 0.4, 100, 50), row('GMAIL', 0.2, 0, null)])
      .join(' ')).toContain('duplicate');
    expect(validateThrottleRows([row('gmail', 0.4, -5, null)]).join(' '))
      .toContain('non-negative');
    expect(validateThrottleRows([row('gmail', 0.4, 100, 0), row('yahoo', 0, 0, null)]))
      .toHaveLength(0);
  });
});

describe('buildThrottlePayload — exact body for the existing PUT', () => {
  it('omits daily_cap when null (server preserves prior) and keeps explicit 0', () => {
    const p = buildThrottlePayload([row('Gmail', 0.4, 1000, null), row('yahoo', 0.3, 0, 0)]);
    expect(p.overrides[0]).toEqual({ isp: 'gmail', pct_override: 0.4, max_per_wave: 1000 });
    expect(p.overrides[0]).not.toHaveProperty('daily_cap');
    expect(p.overrides[1]).toEqual({ isp: 'yahoo', pct_override: 0.3, max_per_wave: 0, daily_cap: 0 });
  });
  it('drops blank-ISP rows', () => {
    expect(buildThrottlePayload([row('', 0.4, 0, null)]).overrides).toHaveLength(0);
  });
});
