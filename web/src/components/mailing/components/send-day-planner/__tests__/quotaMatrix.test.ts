// quotaMatrix tests — table-driven validation of the operator transforms
// the canvas exposes (hold / +pct / +abs / set_abs / zero) plus
// row/column/all dispatchers.

import { describe, expect, it } from 'vitest';
import {
  applyToAll,
  applyToCell,
  applyToColumn,
  applyToRow,
  applyTransform,
  baselineMatrix,
  columnTotal,
  grandTotal,
  rowTotal,
} from '../quotaMatrix';
import { ALL_TARGET_ISPS, BRANDS, WELCOME_NL_QUOTAS_BASELINE } from '../constants';
import type { QuotaTransform } from '../types';

describe('quotaMatrix.applyTransform — pure value math', () => {
  const cases: Array<{ name: string; value: number; t: QuotaTransform; expected: number }> = [
    { name: 'hold returns input', value: 1500, t: { kind: 'hold', value: 0 }, expected: 1500 },
    { name: '+20% on 1500 = 1800', value: 1500, t: { kind: 'scale_pct', value: 0.20 }, expected: 1800 },
    { name: '+50% on 100 = 150', value: 100, t: { kind: 'scale_pct', value: 0.5 }, expected: 150 },
    { name: '-10% on 1000 = 900', value: 1000, t: { kind: 'scale_pct', value: -0.10 }, expected: 900 },
    { name: '+50 abs on 200 = 250', value: 200, t: { kind: 'add_abs', value: 50 }, expected: 250 },
    { name: 'set_abs to 5000', value: 9999, t: { kind: 'set_abs', value: 5000 }, expected: 5000 },
    { name: 'set_abs negative clamped to 0', value: 100, t: { kind: 'set_abs', value: -50 }, expected: 0 },
    { name: 'zero', value: 1500, t: { kind: 'zero', value: 0 }, expected: 0 },
    { name: 'scale_pct rounds half-up', value: 333, t: { kind: 'scale_pct', value: 0.20 }, expected: 400 },
  ];
  for (const c of cases) {
    it(c.name, () => {
      expect(applyTransform(c.value, c.t)).toBe(c.expected);
    });
  }
});

describe('quotaMatrix.baselineMatrix — May-12 anchor', () => {
  it('returns a deep copy', () => {
    const a = baselineMatrix();
    const b = baselineMatrix();
    a.DB.gmail = 99999;
    expect(b.DB.gmail).not.toBe(99999);
    expect(b.DB.gmail).toBe(WELCOME_NL_QUOTAS_BASELINE.DB.gmail);
  });

  it('contains all 4 brands × 11 ISPs', () => {
    const m = baselineMatrix();
    for (const b of BRANDS) {
      for (const isp of ALL_TARGET_ISPS) {
        expect(typeof m[b][isp]).toBe('number');
      }
    }
  });

  it('grandTotal of baseline matches Python total (sum of WELCOME_NL_QUOTAS)', () => {
    // Sum of WELCOME_NL_QUOTAS_BASELINE — locked baseline number.
    const expected = Object.values(WELCOME_NL_QUOTAS_BASELINE)
      .reduce((s, row) => s + Object.values(row).reduce((s2, v) => s2 + v, 0), 0);
    expect(grandTotal(baselineMatrix())).toBe(expected);
  });
});

describe('quotaMatrix.applyToCell', () => {
  it('changes only the targeted cell', () => {
    const m = baselineMatrix();
    const out = applyToCell(m, 'DB', 'gmail', { kind: 'set_abs', value: 9999 });
    expect(out.DB.gmail).toBe(9999);
    expect(out.DB.yahoo).toBe(m.DB.yahoo);
    expect(out.QF.gmail).toBe(m.QF.gmail);
    expect(m.DB.gmail).toBe(WELCOME_NL_QUOTAS_BASELINE.DB.gmail); // input unmutated
  });
});

describe('quotaMatrix.applyToRow', () => {
  it('+20% on every ISP for one brand leaves other brands alone', () => {
    const m = baselineMatrix();
    const out = applyToRow(m, 'QF', { kind: 'scale_pct', value: 0.20 });
    for (const isp of ALL_TARGET_ISPS) {
      expect(out.QF[isp]).toBe(Math.round((m.QF[isp] ?? 0) * 1.20));
    }
    for (const isp of ALL_TARGET_ISPS) {
      expect(out.DB[isp]).toBe(m.DB[isp]);
      expect(out.HT[isp]).toBe(m.HT[isp]);
      expect(out.MH[isp]).toBe(m.MH[isp]);
    }
  });

  it('zero on a row sets that brand row to all zeros', () => {
    const out = applyToRow(baselineMatrix(), 'MH', { kind: 'zero', value: 0 });
    expect(rowTotal(out, 'MH')).toBe(0);
    expect(rowTotal(out, 'DB')).toBeGreaterThan(0);
  });
});

describe('quotaMatrix.applyToColumn', () => {
  it('set_abs on one ISP applies to every brand', () => {
    const out = applyToColumn(baselineMatrix(), 'gmail', { kind: 'set_abs', value: 7777 });
    for (const b of BRANDS) {
      expect(out[b].gmail).toBe(7777);
    }
    expect(columnTotal(out, 'gmail')).toBe(7777 * BRANDS.length);
    // yahoo column unchanged
    expect(columnTotal(out, 'yahoo')).toBe(columnTotal(baselineMatrix(), 'yahoo'));
  });
});

describe('quotaMatrix.applyToAll', () => {
  it('+20% on all = baseline grand × 1.2 (within rounding)', () => {
    const baseline = baselineMatrix();
    const out = applyToAll(baseline, { kind: 'scale_pct', value: 0.20 });
    // Every cell rounded individually, so the sum is approximate but
    // within ~ count*0.5 = ~22 cells × 0.5 → tolerance 25.
    const expected = grandTotal(baseline) * 1.20;
    const observed = grandTotal(out);
    expect(Math.abs(observed - expected)).toBeLessThan(25);
  });

  it('hold all = identity', () => {
    const baseline = baselineMatrix();
    const out = applyToAll(baseline, { kind: 'hold', value: 0 });
    expect(out).toEqual(baseline);
  });

  it('zero all = empty matrix', () => {
    const out = applyToAll(baselineMatrix(), { kind: 'zero', value: 0 });
    expect(grandTotal(out)).toBe(0);
  });
});

describe('quotaMatrix totals', () => {
  it('rowTotal + rowTotal across brands = grandTotal', () => {
    const m = baselineMatrix();
    const sumRows = BRANDS.reduce((s, b) => s + rowTotal(m, b), 0);
    expect(sumRows).toBe(grandTotal(m));
  });
  it('columnTotal across ISPs = grandTotal', () => {
    const m = baselineMatrix();
    const sumCols = ALL_TARGET_ISPS.reduce((s, isp) => s + columnTotal(m, isp), 0);
    expect(sumCols).toBe(grandTotal(m));
  });
});
