// Send-Day Planner — shared types.
//
// Each cell in the 4 brand x 4 slot grid is a CellDraft. The reducer
// over `Record<CellKey, CellDraft>` is what `SendDayPlanner.tsx` owns.
//
// CellDraft must carry every field the payload builder needs to produce
// a wire-identical POST body to deploy_may12_mature.py build_payload().

export type Brand = 'DB' | 'QF' | 'HT' | 'MH';
export type Slot = 'eng-w1' | 'welcome-newsletter' | 'eng-w2' | 'eng-w3';
export type ISP =
  | 'microsoft' | 'gmail' | 'apple' | 'yahoo'
  | 'comcast' | 'charter' | 'att' | 'aol'
  | 'cox' | 'sbcglobal' | 'other';

export type CellKey = `${Slot}|${Brand}`;

export interface QuotaTransform {
  /**
   * `hold` — copy yesterday's value (no-op).
   * `scale_pct` — multiply by (1 + value); value is a fraction (0.20 = +20%).
   * `add_abs` — add `value` to current.
   * `set_abs` — set to `value`.
   * `zero` — set to 0.
   */
  kind: 'hold' | 'scale_pct' | 'add_abs' | 'set_abs' | 'zero';
  value: number;
}

export interface CellDraft {
  brand: Brand;
  slot: Slot;
  /** Family is the creative file family — e.g. "warby-parker". */
  family: string;
  /** Resolved filename — e.g. "warby-parker-db-newsletter-05122026.html". */
  filename: string;
  /** Mutated HTML content (post creative-resolve pipeline). */
  htmlContent: string;
  /** Subject line — derived from family unless operator overrides. */
  subject: string;
  preheader: string;
  /** Per-ISP quota for the cell. Welcome cells use the matrix; engager cells use the auto-cap formula. */
  ispQuotas: Partial<Record<ISP, number>>;
  /** Inclusion + exclusion segments. */
  inclusionSegments: string[];
  exclusionSegments: string[];
  /** UTC start + end for the slot's window (computed from anchors). */
  startAtUTC: string;
  endAtUTC: string;
  /** Window hours used to derive endAtUTC. */
  windowHours: number;
  /** Status from server after deploy. */
  status: 'draft' | 'deploying' | 'accepted' | 'failed' | 'cancelled';
  /** Server campaign id once deploy returns. */
  campaignId?: string;
  /** Deploy error if any. */
  error?: string;
}

export interface GateState {
  // Six gates from .cursor/rules/send-day-process.mdc §2.
  // null = not loaded; { passes: true, ... } = green; { passes: false, ... } = red.
  gateA: GateA | null;
  gateB: GateB | null;
  gateC: GateC | null;
  gateD: GateD | null;
  gateE: GateE | null;
  gateF: GateF | null;
}

export interface GateA {
  passes: boolean;
  servers: Record<string, { state: string; message?: string; last_checked_at?: string }>;
  guidance?: string;
}

export interface GateB {
  passes: boolean;
  zombies: number;
  expired: number;
  due_now: number;
}

export interface GateC {
  passes: boolean;
  git_sha: string;
  required_commit: string; // a92af78
}

export interface GateD {
  passes: boolean;
  results: Record<string, { ok: boolean; errors: { check: string; message: string }[]; warnings: { check: string; message: string }[] }>;
}

export interface GateE {
  passes: boolean;
  reviewed_at?: string;
  reviewed_by?: string;
}

export interface GateF {
  passes: boolean;
  today_planned: number;
  yesterday_planned: number;
  target: number;
  ramp_floor: number;
  gap: number;
  percent_to_target: number;
}

export interface BannedCreative {
  filename: string;
  reason: string;
  paused_at: string;
}
