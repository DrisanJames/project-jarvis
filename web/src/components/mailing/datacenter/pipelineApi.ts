// pipelineApi.ts — typed client for GET /api/mailing/data-pipeline/summary.
//
// THE GO STRUCTS ARE THE CONTRACT: every type mirrors the `json:"…"` tags in
// internal/api/data_pipeline_daily_handlers.go (pipelineSummaryResponse and
// friends). Every count is nullable — null means no row was posted for it and
// renders "not yet measured", never 0 (METRIC_CONTRACT §11.7).

import { apiFetch } from '../shared/apiFetch';
import type { Count, FieldFlags } from '../dataingest/api';

export const DATA_PIPELINE_BASE = '/api/mailing/data-pipeline';

export type PipelineISPId =
  | 'microsoft' | 'apple' | 'comcast'
  | 'yahoo_family' | 'yahoo' | 'aol' | 'att' | 'sbcglobal' | 'cox' | 'verizon' | 'frontier'
  | 'longtail' | 'windstream';

export type PipelineStage =
  | 'received' | 'raw' | 'cleaned_ok' | 'cleaned_rejected' | 'clean' | 'unique_mailed'
  | 'target' | 'contract' | 'first_mailed' | 'in_funnel' | 'promoted' | 'retired'
  | 'dirty_outside_queue' | 'touch_1' | 'touch_2' | 'touch_3' | 'touch_4' | 'touch_5'
  | 'cohort_mailed' | 'cohort_activated' | 'ramp_target'
  | 'cleaning_ordered' | 'cleaning_passed' | 'cleaning_cost_cents'
  | 'arrivals' | 'sent' | 'delivered' | 'deferred';

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
  dirty_outside_queue: Count;
  touch_1: Count;
  touch_2: Count;
  touch_3: Count;
  touch_4: Count;
  touch_5: Count;
  cohort_mailed: Count;
  cohort_activated: Count;
  ramp_target: Count;
  cleaning_ordered: Count;
  cleaning_passed: Count;
  cleaning_cost_cents: Count;
  arrivals: Count;
  sent: Count;
  delivered: Count;
  deferred: Count;
  /** clean ÷ contract; null when contract is 0 or unknown */
  days_of_supply: number | null;
  /** first_mailed ÷ target × 100 */
  pct_of_target: number | null;
  /** first_mailed ÷ unique_mailed × 100 */
  new_share: number | null;
  /** days_of_supply below low_days_threshold (server verdict) */
  low: boolean;
  /** Σ activated ÷ Σ mailed × 100 over the cohort days of the last 7 */
  activation_rate_7d: number | null;
  cleaning_pass_rate: number | null;
  delivered_rate: number | null;
  deferred_rate: number | null;
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
  ramp_target: Count;
  pct_of_contract: number | null;
  over_contract: boolean;
  off_ramp: boolean;
}

export interface PipelineDomain {
  sending_domain: string;
  contract: Count;
  first_mailed: Count;
  target: Count;
  ramp_target: Count;
  pct_of_contract: number | null;
  over_contract: boolean;
  off_ramp: boolean;
  by_isp: PipelineDomainISP[];
}

export interface PipelineSourceISP {
  isp: PipelineISPId;
  raw: Count;
  clean: Count;
  dirty_outside_queue: Count;
}

export interface PipelineSource {
  source: string;
  raw: Count;
  clean: Count;
  dirty_outside_queue: Count;
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
  activation: { day: string; isp: PipelineISPId; mailed: Count; activated: Count; rate: number | null }[];
  cleaning: { day: string; isp: PipelineISPId; ordered: Count; passed: Count; pass_rate: number | null; cost_cents: Count }[];
  cleaning_mtd: {
    month: string;
    from: string;
    ordered: Count;
    passed: Count;
    cost_cents: Count;
    by_isp: { isp: PipelineISPId; ordered: Count; passed: Count; cost_cents: Count }[];
  };
  degraded: string[];
}

/** pipelineAgentEntry (data_pipeline_agent.go) */
export interface PipelineAgentEntry {
  id?: number;
  at: string;
  kind: 'check' | 'decision' | 'repair' | 'learned' | 'alert' | 'report';
  status: 'ok' | 'warn' | 'fail';
  isp: string;
  title: string;
  body: Record<string, unknown>;
}

/** pipelineAgentResponse */
export interface PipelineAgentLog {
  as_of: string;
  source: string;
  fields: FieldFlags;
  header: {
    last_check_at: string | null;
    status: 'ok' | 'warn' | 'fail' | null;
    minutes_since_last_check: number | null;
    stale: boolean;
    stale_after_minutes: number;
    mandate: Record<string, unknown> | null;
    mandate_at: string | null;
  };
  entries: PipelineAgentEntry[];
}

async function getJSON<T>(path: string, signal?: AbortSignal): Promise<T> {
  const r = await apiFetch(`${DATA_PIPELINE_BASE}${path}`, { signal });
  if (!r.ok) {
    let msg = `HTTP ${r.status}`;
    try {
      const b = await r.json();
      if (b && typeof b.error === 'string') msg = `${msg}: ${b.error}`;
    } catch { /* non-JSON error body */ }
    throw new Error(msg);
  }
  return (await r.json()) as T;
}

export const fetchPipelineSummary = (lane: string, days: number, signal?: AbortSignal) =>
  getJSON<PipelineSummary>(`/summary?lane=${encodeURIComponent(lane)}&days=${encodeURIComponent(String(days))}`, signal);

export const fetchPipelineAgent = (limit: number, signal?: AbortSignal) =>
  getJSON<PipelineAgentLog>(`/agent?limit=${encodeURIComponent(String(limit))}`, signal);

export const ISP_LABEL: Record<PipelineISPId, string> = {
  microsoft: 'Microsoft',
  apple: 'Apple',
  comcast: 'Comcast',
  yahoo_family: 'Yahoo family',
  yahoo: 'Yahoo',
  aol: 'AOL',
  att: 'AT&T',
  sbcglobal: 'SBCGlobal',
  cox: 'Cox',
  verizon: 'Verizon',
  frontier: 'Frontier',
  longtail: 'Long tail',
  windstream: 'Windstream (part of long tail)',
};

export const ispLabel = (isp: string): string => ISP_LABEL[isp as PipelineISPId] ?? isp;

export const SOURCE_LABEL: Record<string, string> = {
  partner: 'Partner',
  vertical: 'Vertical',
  s3: 'Amazon S3 storage',
  sftp: 'SFTP drop',
  local: 'Local files',
  subscriber_base: 'Existing subscriber base',
};

export const sourceLabel = (s: string): string =>
  SOURCE_LABEL[s] ?? s.replace(/_/g, ' ').replace(/^\w/, (c) => c.toUpperCase());
