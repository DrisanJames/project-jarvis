// types.ts — Offer Alignment API contract (PART B2 of the 2026-07-07 design
// doc) plus the small fetch/format helpers every level shares.
//
// Endpoints (all READ ONLY, called via apiFetch):
//   GET /api/mailing/offer-alignment/matrix?window=7|30
//       → { rows: MatrixRow[], refreshed_at, staleness }
//       → 202/503 { status: 'building' }  (snapshot warming — designed state)
//       → 200 { disabled: true }          (feature dark — designed state)
//   GET /api/mailing/offer-alignment/offer?offer=<key>&from=&to=[&sending_domain=]
//       → OfferResponse
//   GET /api/mailing/offer-alignment/evidence?offer=<key>&isp=<bucket>&from=&to=
//       → { rows: EvidenceRow[] }
//
// Rate convention (contract assumption, mirrors the rest of the portal):
// clicker_rate / block_rate / invalid_rate / attribution_coverage are FRACTIONS
// (0–1); the UI multiplies by 100 and ALWAYS discloses the denominator.

import { apiFetch } from '../shared/apiFetch';

// ── Badges — the independent DELIVERY-HEALTH channel (never blended with the
// performance color ramp). HEALTHY renders no badge at all.
export type BadgeKind =
  | 'HEALTHY'
  | 'BLOCKING'
  | 'THROTTLED'
  | 'LIST_QUALITY'
  | 'LOW_VOLUME'
  | 'NO_LAKE';

export interface MatrixRow {
  offer_key: string;
  offer_name: string;
  isp: string;
  delivered: number;
  hard: number;
  soft: number;
  reputation_block: number;
  deferred: number;
  block_rate: number;        // fraction of attempted
  human_clicks: number;
  clickers: number;
  clicker_rate: number;      // fraction of delivered
  conversions: number;
  revenue: number;
  rpm: number;               // revenue / delivered × 1000
  epc: number;               // revenue / human clicks
  badge: string;             // BadgeKind expected; unknown values tolerated
  badge_reason: string;
  action: string;
  sample_ok: boolean;
  attribution_coverage: number; // fraction of the offer's CAMPAIGNS that are stamped (count-based, not delivery-weighted)
}

export interface MatrixResponse {
  rows: MatrixRow[];
  refreshed_at: string;
  // Server-described snapshot age; shape not pinned by the contract, so we
  // tolerate either a string ("4m") or a number of seconds and render as-is.
  staleness: string | number;
}

export interface OfferAttribution {
  stamped_campaigns: number;
  inferred_campaigns: number;
}

// Header KPIs: the contract says "…kpis" without pinning every key, so all
// numerics are optional and the profile falls back to sums over isp_rows.
export interface OfferHeader {
  status_line?: string;
  attribution?: OfferAttribution;
  delivered?: number;
  hard?: number;
  soft?: number;
  deferred?: number;
  reputation_block?: number;
  human_clicks?: number;
  clickers?: number;
  clicker_rate?: number;
  conversions?: number;
  revenue?: number;
  rpm?: number;
  epc?: number;
}

export interface OfferIspRow {
  isp: string;
  delivered: number;
  hard: number;
  soft: number;
  reputation_block: number;
  deferred: number;
  block_rate: number;
  human_clicks: number;
  clickers: number;
  clicker_rate: number;
  conversions: number;
  revenue: number;
  rpm: number;
  epc: number;
  badge: string;
  badge_reason: string;
  action: string;
}

export interface OfferCreativeRow {
  creative_key: string;
  subject: string;
  from_name: string;
  isp: string;
  delivered: number;
  clicks: number;
  clickers: number;
  clicker_rate: number;   // fraction of delivered
  conversions: number;
  revenue: number;
  sample_campaign_id: string;
  has_html: boolean;
  inferred: boolean;      // historical (name/slug-inferred) attribution
}

export interface OfferDataSourceRow {
  data_source: string | null; // null/'' ⇒ "(unattributed)" row
  delivered: number;
  hard: number;
  invalid_rate: number;       // fraction of delivered+hard
  clicks: number;
  clickers: number;
  conversions: number;
}

export interface OfferResponse {
  header: OfferHeader;
  isp_rows: OfferIspRow[];
  creatives: OfferCreativeRow[];
  data_sources: OfferDataSourceRow[];
}

export interface EvidenceRow {
  dsn_family: string;
  dsn_code: string;
  count: number;
  first_seen: string;
  last_seen: string;
  sample_diag: string;
  meaning: string; // plain-English decode from the server's Go map
}

export interface EvidenceResponse {
  rows: EvidenceRow[];
}

// Preview reuses the existing creatives endpoint (unchanged contract).
export interface CreativePreviewData {
  campaign_id: string;
  subject: string;
  from_name: string;
  html: string;
  has_html: boolean;
}

export type MatrixMetric = 'rpm' | 'human_click_rate' | 'clicker_rate';

// ── Fetch outcome — every panel handles all four shapes fail-soft ──────────

export type FetchOutcome<T> =
  | { kind: 'ready'; data: T }
  | { kind: 'building' }
  | { kind: 'disabled' };

/**
 * Shared GET helper for the offer-alignment family. Resolves the three
 * designed non-error shapes ({status:'building'} on any HTTP status, HTTP 202,
 * {disabled:true}) and throws an Error (server {error} verbatim when present)
 * for everything else so callers render their designed error state.
 */
export async function fetchAlignment<T>(url: string, signal?: AbortSignal): Promise<FetchOutcome<T>> {
  const res = await apiFetch(url, { signal });
  let body: unknown = null;
  try {
    body = await res.json();
  } catch {
    /* non-JSON body — handled by status checks below */
  }
  const rec = (body && typeof body === 'object' ? body : {}) as Record<string, unknown>;
  if (rec.status === 'building' || res.status === 202) return { kind: 'building' };
  if (rec.disabled === true) return { kind: 'disabled' };
  if (!res.ok) {
    const msg = typeof rec.error === 'string' && rec.error ? rec.error : `HTTP ${res.status}`;
    throw new Error(msg);
  }
  return { kind: 'ready', data: body as T };
}

export const isAbortError = (e: unknown): boolean =>
  e instanceof DOMException && e.name === 'AbortError';

// ── Formatters (denominator-disclosing by convention) ──────────────────────

export const fmtNum = (n: number | null | undefined): string =>
  n == null || !Number.isFinite(n) ? '—' : n.toLocaleString('en-US');

export const fmtMoney = (n: number | null | undefined): string =>
  n == null || !Number.isFinite(n)
    ? '—'
    : n.toLocaleString('en-US', { style: 'currency', currency: 'USD', minimumFractionDigits: 2, maximumFractionDigits: 2 });

/** Fraction (0–1) → percent string. */
export const fmtRate = (frac: number | null | undefined, digits = 1): string =>
  frac == null || !Number.isFinite(frac) ? '—' : `${(frac * 100).toFixed(digits)}%`;

/** "4.2% of 128,401 delivered" — the mandated denominator-disclosing form. */
export const rateWithDenom = (
  frac: number | null | undefined,
  denom: number | null | undefined,
  noun = 'delivered'
): string => `${fmtRate(frac)} of ${fmtNum(denom)} ${noun}`;

export const safeDiv = (num: number, denom: number): number | null =>
  denom > 0 ? num / denom : null;

// ── ISP column ordering — canonical clean-classifier order, unknowns after ──

export const ISP_ORDER = ['gmail', 'microsoft', 'yahoo', 'apple', 'comcast', 'aol', 'att', 'charter'];

export function orderIsps(isps: Iterable<string>): string[] {
  const uniq = Array.from(new Set(isps));
  return uniq.sort((a, b) => {
    const ia = ISP_ORDER.indexOf(a);
    const ib = ISP_ORDER.indexOf(b);
    if (ia !== -1 && ib !== -1) return ia - ib;
    if (ia !== -1) return -1;
    if (ib !== -1) return 1;
    return a.localeCompare(b);
  });
}

export const ispLabel = (isp: string): string =>
  isp ? isp.charAt(0).toUpperCase() + isp.slice(1) : '(unknown)';
