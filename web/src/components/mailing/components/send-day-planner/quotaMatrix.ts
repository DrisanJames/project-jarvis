// quotaMatrix — pure transformation helpers for the per-(brand, ISP)
// welcome quota matrix. The 4 brand x 11 ISP matrix is operated on
// with apply-to-cell / apply-to-row / apply-to-column / apply-to-all
// dispatches. All functions here MUST be pure for Phase 4a tests.

import { ALL_TARGET_ISPS, BRANDS, WELCOME_NL_QUOTAS_BASELINE } from './constants';
import type { Brand, ISP, QuotaTransform } from './types';

export type QuotaMatrix = Record<Brand, Record<ISP, number>>;

/** Deep-clone the May 12 baseline matrix. The canvas seeds the matrix with
 *  this when "May 12" is selected and resets to it via "Hold all". */
export function baselineMatrix(): QuotaMatrix {
  return JSON.parse(JSON.stringify(WELCOME_NL_QUOTAS_BASELINE)) as QuotaMatrix;
}

/** applyTransform — given a starting matrix value, return the new value
 *  for the supplied transform. Mirrors the operator-language vocabulary
 *  in deploy_may12_mature.py header lines 28-39 (hold / +20% / +50 / +100). */
export function applyTransform(value: number, transform: QuotaTransform): number {
  switch (transform.kind) {
    case 'hold':
      return value;
    case 'scale_pct':
      // value is a fraction (0.20 = +20%). Round half-up to integer.
      return Math.round(value * (1 + transform.value));
    case 'add_abs':
      return value + transform.value;
    case 'set_abs':
      return Math.max(0, transform.value);
    case 'zero':
      return 0;
  }
}

/** applyToCell — return a new matrix with the transform applied to one cell. */
export function applyToCell(
  matrix: QuotaMatrix, brand: Brand, isp: ISP, transform: QuotaTransform,
): QuotaMatrix {
  const next = JSON.parse(JSON.stringify(matrix)) as QuotaMatrix;
  next[brand][isp] = applyTransform(matrix[brand][isp] ?? 0, transform);
  return next;
}

/** applyToRow — apply transform to every ISP for a brand. */
export function applyToRow(
  matrix: QuotaMatrix, brand: Brand, transform: QuotaTransform,
): QuotaMatrix {
  const next = JSON.parse(JSON.stringify(matrix)) as QuotaMatrix;
  for (const isp of ALL_TARGET_ISPS) {
    next[brand][isp] = applyTransform(matrix[brand][isp] ?? 0, transform);
  }
  return next;
}

/** applyToColumn — apply transform to every brand for an ISP. */
export function applyToColumn(
  matrix: QuotaMatrix, isp: ISP, transform: QuotaTransform,
): QuotaMatrix {
  const next = JSON.parse(JSON.stringify(matrix)) as QuotaMatrix;
  for (const brand of BRANDS) {
    next[brand][isp] = applyTransform(matrix[brand][isp] ?? 0, transform);
  }
  return next;
}

/** applyToAll — apply transform to every cell. */
export function applyToAll(matrix: QuotaMatrix, transform: QuotaTransform): QuotaMatrix {
  const next = JSON.parse(JSON.stringify(matrix)) as QuotaMatrix;
  for (const brand of BRANDS) {
    for (const isp of ALL_TARGET_ISPS) {
      next[brand][isp] = applyTransform(matrix[brand][isp] ?? 0, transform);
    }
  }
  return next;
}

/** rowTotal — sum across ISPs for a brand. */
export function rowTotal(matrix: QuotaMatrix, brand: Brand): number {
  return ALL_TARGET_ISPS.reduce((s, isp) => s + (matrix[brand][isp] ?? 0), 0);
}

/** columnTotal — sum across brands for an ISP. */
export function columnTotal(matrix: QuotaMatrix, isp: ISP): number {
  return BRANDS.reduce((s, brand) => s + (matrix[brand][isp] ?? 0), 0);
}

/** grandTotal — sum across the entire matrix. */
export function grandTotal(matrix: QuotaMatrix): number {
  return BRANDS.reduce((s, brand) => s + rowTotal(matrix, brand), 0);
}
