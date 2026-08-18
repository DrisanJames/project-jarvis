// throttleDiff.ts — pure diff / validation / payload builders for the
// Property Ledger cockpit throttle editor (Pipeline Cockpit P2).
//
// The server write the cockpit reuses
// (PUT /api/mailing/data-partners/datasets/{id}/isp-distribution,
// HandleUpdateISPDistribution) is DELETE-and-replace per dataset, with one
// carve-out: an ISP included in the write WITHOUT daily_cap keeps its prior
// lane daily budget; an ISP omitted entirely loses its row — INCLUDING its
// lane daily budget — and falls back to the global defaults. The diff
// preview must state all of that per row, before the operator types the
// feed name. These functions are pure so the preview is unit-testable.

export interface ThrottleRow {
  isp: string;
  pct_override: number;
  max_per_wave: number; // 0 = no per-wave override stored
  daily_cap: number | null; // null = global default (and "preserve" on write)
}

export type ThrottleDiffKind = 'added' | 'removed' | 'changed' | 'unchanged';

export interface ThrottleDiffEntry {
  isp: string;
  kind: ThrottleDiffKind;
  before?: ThrottleRow;
  after?: ThrottleRow;
  notes: string[];
}

const norm = (isp: string) => isp.trim().toLowerCase();

const sameRow = (a: ThrottleRow, b: ThrottleRow): boolean =>
  a.pct_override === b.pct_override &&
  a.max_per_wave === b.max_per_wave &&
  a.daily_cap === b.daily_cap;

/**
 * buildThrottleDiff — current server rows vs the editor's proposed rows,
 * rendered as one entry per ISP. Replacement semantics are explicit:
 * an ISP present in `current` but absent from `proposed` is REMOVED
 * (falls back to global defaults, lane daily budget deleted with the row).
 */
export function buildThrottleDiff(
  current: ThrottleRow[],
  proposed: ThrottleRow[],
): ThrottleDiffEntry[] {
  const cur = new Map<string, ThrottleRow>();
  for (const r of current) cur.set(norm(r.isp), { ...r, isp: norm(r.isp) });
  const prop = new Map<string, ThrottleRow>();
  for (const r of proposed) {
    if (norm(r.isp) === '') continue; // blank rows never reach the payload
    prop.set(norm(r.isp), { ...r, isp: norm(r.isp) });
  }

  const isps = Array.from(new Set([...cur.keys(), ...prop.keys()])).sort();
  const out: ThrottleDiffEntry[] = [];
  for (const isp of isps) {
    const before = cur.get(isp);
    const after = prop.get(isp);
    if (before && !after) {
      const notes = ['row removed — this ISP falls back to the global defaults'];
      if (before.daily_cap != null) {
        notes.push(
          `lane daily budget ${before.daily_cap} is REMOVED with the row (delete-and-replace)`,
        );
      }
      out.push({ isp, kind: 'removed', before, notes });
      continue;
    }
    if (!before && after) {
      const notes = ['new override row'];
      if (after.daily_cap === 0) notes.push('daily cap 0 = ISP HARD-SUPPRESSED for this lane');
      out.push({ isp, kind: 'added', after, notes });
      continue;
    }
    if (before && after) {
      const notes: string[] = [];
      // Omitted daily_cap on an INCLUDED row: server preserves the prior
      // lane budget (the snapshot carve-out in HandleUpdateISPDistribution).
      const effectiveAfter = { ...after };
      if (after.daily_cap == null && before.daily_cap != null) {
        effectiveAfter.daily_cap = before.daily_cap;
        notes.push(`daily cap omitted — prior lane budget ${before.daily_cap} is preserved`);
      }
      if (after.daily_cap === 0 && before.daily_cap !== 0) {
        notes.push('daily cap 0 = ISP HARD-SUPPRESSED for this lane');
      }
      if (sameRow(before, effectiveAfter)) {
        out.push({ isp, kind: 'unchanged', before, after: effectiveAfter, notes });
      } else {
        if (before.pct_override !== effectiveAfter.pct_override) {
          notes.push(`pct ${before.pct_override} → ${effectiveAfter.pct_override}`);
        }
        if (before.max_per_wave !== effectiveAfter.max_per_wave) {
          notes.push(`per-wave cap ${before.max_per_wave || 'none'} → ${effectiveAfter.max_per_wave || 'none'}`);
        }
        if (before.daily_cap !== effectiveAfter.daily_cap) {
          notes.push(`daily cap ${before.daily_cap ?? 'default'} → ${effectiveAfter.daily_cap ?? 'default'}`);
        }
        out.push({ isp, kind: 'changed', before, after: effectiveAfter, notes });
      }
    }
  }
  return out;
}

/** validateThrottleRows — client-side mirror of the server's validation. */
export function validateThrottleRows(rows: ThrottleRow[]): string[] {
  const errs: string[] = [];
  const seen = new Set<string>();
  for (const r of rows) {
    const isp = norm(r.isp);
    if (isp === '') {
      errs.push('an override row has no ISP');
      continue;
    }
    if (seen.has(isp)) errs.push(`duplicate ISP "${isp}"`);
    seen.add(isp);
    if (!(r.pct_override >= 0 && r.pct_override <= 1)) {
      errs.push(`${isp}: pct_override must be between 0 and 1 (fraction, not percent)`);
    }
    if (r.max_per_wave < 0 || !Number.isInteger(r.max_per_wave)) {
      errs.push(`${isp}: per-wave cap must be a non-negative integer`);
    }
    if (r.daily_cap != null && (r.daily_cap < 0 || !Number.isInteger(r.daily_cap))) {
      errs.push(`${isp}: daily cap must be a non-negative integer (0 = hard-suppress)`);
    }
  }
  return errs;
}

/**
 * buildThrottlePayload — the exact request body for the existing PUT.
 * daily_cap is included ONLY when set: an omitted daily_cap on an included
 * ISP means "preserve prior" server-side.
 */
export function buildThrottlePayload(rows: ThrottleRow[]): {
  overrides: Array<Record<string, unknown>>;
} {
  return {
    overrides: rows
      .filter((r) => norm(r.isp) !== '')
      .map((r) => {
        const o: Record<string, unknown> = {
          isp: norm(r.isp),
          pct_override: r.pct_override,
          max_per_wave: r.max_per_wave,
        };
        if (r.daily_cap != null) o.daily_cap = r.daily_cap;
        return o;
      }),
  };
}
