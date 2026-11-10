// pipelineApi.ts — typed client for GET /api/mailing/data-pipeline/summary.
//
// THE GO STRUCTS ARE THE CONTRACT: every type mirrors the `json:"…"` tags in
// internal/api/data_pipeline_daily_handlers.go (pipelineSummaryResponse and
// friends). Every count is nullable — null means no row was posted for it and
// renders "not yet measured", never 0 (METRIC_CONTRACT §11.7).

import { apiFetch } from '../shared/apiFetch';
import type { Count, FieldFlags } from '../dataingest/api';

export const DATA_PIPELINE_BASE = '/api/mailing/data-pipeline';

export type PipelineISPId = 'microsoft' | 'apple' | 'comcast' | 'yahoo_family' | 'longtail' | 'windstream';

export type PipelineStage =
  | 'received' | 'raw' | 'cleaned_ok' | 'cleaned_rejected' | 'clean' | 'unique_mailed'
  | 'target' | 'contract' | 'first_mailed' | 'in_funnel' | 'promoted' | 'retired';

/** pipelineFigures — one ISP's latest day, or the lane total. */
export interface PipelineFigures {
  received: Count;
  raw: Count;
  cleaned_ok: Count;
  cleaned_rejected: Count;
  clean: Count;
  unique_mailed: Count;
  target: Count;
  contract: Count;
  first_mailed: Count;
  in_funnel: Count;
  promoted: Count;
  retired: Count;
  /** clean ÷ contract; null when contract is 0 or unknown */
  days_of_supply: number | null;
  /** first_mailed ÷ target × 100 */
  pct_of_target: number | null;
  /** first_mailed ÷ unique_mailed × 100 */
  new_share: number | null;
  /** days_of_supply below low_days_threshold (server verdict) */
  low: boolean;
}

/** pipelineISP */
export interface PipelineISP extends PipelineFigures {
  isp: PipelineISPId;
  /** "longtail" for windstream (a sub-row, already inside the long tail) */
  parent?: string;
  avg_7d: Partial<Record<PipelineStage, number | null>>;
}

export interface PipelineFunnelStep {
  stage: PipelineStage;
  today: Count;
  avg_7d: number | null;
  drop_from_prior: Count;
  pct_of_prior: number | null;
}

export interface PipelineDomainISP {
  isp: PipelineISPId;
  contract: Count;
  first_mailed: Count;
  target: Count;
  pct_of_contract: number | null;
}

export interface PipelineDomain {
  sending_domain: string;
  contract: Count;
  first_mailed: Count;
  target: Count;
  pct_of_contract: number | null;
  by_isp: PipelineDomainISP[];
}

export interface PipelineSourceISP {
  isp: PipelineISPId;
  raw: Count;
  clean: Count;
}

export interface PipelineSource {
  source: string;
  raw: Count;
  clean: Count;
  by_isp: PipelineSourceISP[];
}

export interface PipelineSummary {
  as_of: string;
  source: string;
  fields: FieldFlags;
  organization_id?: string;
  lane: string;
  days: number;
  from: string;
  to: string;
  denver_day: string;
  latest_day: string | null;
  low_days_threshold: number;
  row_count: number;
  isps: PipelineISP[];
  totals: PipelineFigures;
  totals_avg_7d: Partial<Record<PipelineStage, number | null>>;
  funnel: PipelineFunnelStep[];
  series: { day: string; isp: PipelineISPId; stages: Partial<Record<PipelineStage, number>> }[];
  domains: PipelineDomain[];
  sources: PipelineSource[];
  degraded: string[];
}

export async function fetchPipelineSummary(lane: string, days: number, signal?: AbortSignal): Promise<PipelineSummary> {
  const r = await apiFetch(
    `${DATA_PIPELINE_BASE}/summary?lane=${encodeURIComponent(lane)}&days=${encodeURIComponent(String(days))}`,
    { signal },
  );
  if (!r.ok) {
    let msg = `HTTP ${r.status}`;
    try {
      const b = await r.json();
      if (b && typeof b.error === 'string') msg = `${msg}: ${b.error}`;
    } catch { /* non-JSON error body */ }
    throw new Error(msg);
  }
  return (await r.json()) as PipelineSummary;
}

export const ISP_LABEL: Record<PipelineISPId, string> = {
  microsoft: 'Microsoft',
  apple: 'Apple',
  comcast: 'Comcast',
  yahoo_family: 'Yahoo family',
  longtail: 'Long tail',
  windstream: 'Windstream (part of long tail)',
};

export const ispLabel = (isp: string): string => ISP_LABEL[isp as PipelineISPId] ?? isp;

export const SOURCE_LABEL: Record<string, string> = {
  partner: 'Partner',
  vertical: 'Vertical',
};

export const sourceLabel = (s: string): string =>
  SOURCE_LABEL[s] ?? s.replace(/_/g, ' ').replace(/^\w/, (c) => c.toUpperCase());
