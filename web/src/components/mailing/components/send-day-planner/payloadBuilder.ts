// payloadBuilder — turns a CellDraft into the exact wire body that
// deploy_may12_mature.py POSTs. This is the contract layer.
//
// Phase 4a tests assert byte-equality against 16 fixtures dumped from
// the Python script via `--dump-post-body`.
//
// Non-negotiable invariants enforced here:
//   * isp_plans[*].time_spans[*].source MUST be "duration-calc" (or
//     "manual" if the operator opens an explicit override). Anything
//     else is silently rejected by waveSanityCheck.
//   * cadence: { mode: "interval", every_minutes: 15, batch_size: 0 }
//   * throttle_strategy: "gentle", timezone: "America/Denver"
//   * randomize_audience: false, send_mode: "scheduled",
//     min_remail_hours: 0, use_master_selection: false,
//     content_locked: false.

import type { Brand, CellDraft, ISP, Slot } from './types';
import {
  ALL_TARGET_ISPS,
  ATTRIBITS_PARTITION_BY_BRAND,
  BRAND_FROM_NAME,
  BRAND_LABEL,
  CONVERTERS_SEGMENT_ID,
  EXPECTED_30D_OPENERS_PER_BRAND,
  MAILGUN_ENGAGED_MASTER_SEGMENT_ID,
  OPENERS_30D_BY_BRAND,
  SENDING_DOMAIN,
  SLOT_NAME_FRAGMENT,
  VERTICAL_FINANCE_SEGMENT_ID,
  WAVE_INTERVAL_MINUTES,
  WCM_MORTGAGE_SEGMENT_ID,
  WELCOME_SATURATION_EXCLUDE_SEG_ID,
} from './constants';

export interface PMTAPayload {
  name: string;
  sending_domain: string;
  variants: Array<{
    variant_name: string;
    subject: string;
    preview_text: string;
    from_name: string;
    html_content: string;
    plain_content: string;
    split_percent: number;
  }>;
  target_isps: ISP[];
  send_priority: Array<{ id: string; type: 'segment' | 'list' }>;
  inclusion_segments: string[];
  inclusion_lists: string[];
  exclusion_segments: string[];
  exclusion_lists: string[];
  timezone: 'America/Denver';
  throttle_strategy: 'gentle';
  isp_quotas: Array<{ isp: ISP; volume: number }>;
  isp_plans: Array<{
    isp: ISP;
    quota: number;
    randomize_audience: false;
    throttle_strategy: 'gentle';
    timezone: 'America/Denver';
    cadence: { mode: 'interval'; every_minutes: number; batch_size: 0 };
    time_spans: Array<{
      type: 'absolute';
      start_at: string;
      end_at: string;
      timezone: 'America/Denver';
      source: 'duration-calc' | 'manual';
    }>;
  }>;
  randomize_audience: false;
  send_mode: 'scheduled';
  scheduled_at: string;
  min_remail_hours: 0;
  use_master_selection: false;
  content_locked: false;
}

/** Resolve the inclusion segments for a (slot, brand). Mirrors
 *  deploy_may12_mature.inclusion_for(). */
export function inclusionFor(slot: Slot, brand: Brand): string[] {
  if (slot === 'eng-w1' || slot === 'eng-w2' || slot === 'eng-w3') {
    return [OPENERS_30D_BY_BRAND[brand]];
  }
  return [
    ATTRIBITS_PARTITION_BY_BRAND[brand],
    MAILGUN_ENGAGED_MASTER_SEGMENT_ID,
    WCM_MORTGAGE_SEGMENT_ID,
    VERTICAL_FINANCE_SEGMENT_ID,
    CONVERTERS_SEGMENT_ID,
  ];
}

/** Resolve exclusion segments — Welcome saturation guard for welcome only. */
export function exclusionFor(slot: Slot): string[] {
  return slot === 'welcome-newsletter' ? [WELCOME_SATURATION_EXCLUDE_SEG_ID] : [];
}

/** Compute the engager auto-cap quota: max(2000, expected*1.5). Mirrors
 *  deploy_may12_mature.quotas_for(engager). */
export function engagerAutoCap(brand: Brand): number {
  return Math.max(2000, Math.floor(EXPECTED_30D_OPENERS_PER_BRAND[brand] * 1.5));
}

/** Resolve the per-(slot, brand) full quota map. For engagers this is
 *  the auto-cap formula applied to ALL_TARGET_ISPS. For welcome it's
 *  the operator-supplied matrix (filtered to >0 ISPs).
 *
 *  Welcome cells whose quota is 0 are excluded from target_isps + isp_plans
 *  to mirror the Python's `q.get(isp, 0) > 0` filter. */
export function resolveQuotas(
  draft: CellDraft,
): { ispsForPlans: ISP[]; quotas: Array<{ isp: ISP; volume: number }> } {
  if (draft.slot !== 'welcome-newsletter') {
    const cap = engagerAutoCap(draft.brand);
    return {
      ispsForPlans: ALL_TARGET_ISPS.slice(),
      quotas: ALL_TARGET_ISPS.map(isp => ({ isp, volume: cap })),
    };
  }
  const isps: ISP[] = [];
  const quotas: Array<{ isp: ISP; volume: number }> = [];
  for (const isp of ALL_TARGET_ISPS) {
    const v = draft.ispQuotas[isp] ?? 0;
    if (v > 0) {
      isps.push(isp);
      quotas.push({ isp, volume: v });
    }
  }
  return { ispsForPlans: isps, quotas };
}

/** Format a Date as the deploy script's wire format: "YYYY-MM-DDTHH:MM:SSZ".
 *  Note: NOT toISOString() because that emits ".000Z" milliseconds; the
 *  Python uses `strftime("%Y-%m-%dT%H:%M:%SZ")` with no fractional. */
export function formatWireUTC(iso: string): string {
  const d = new Date(iso);
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())}T${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}:${pad(d.getUTCSeconds())}Z`;
}

export interface BuildPayloadOptions {
  /** Override the time-span source. Default: "duration-calc". */
  timeSpanSource?: 'duration-calc' | 'manual';
  /** Override the campaign name. Default: derived from date / brand / slot / family. */
  campaignName?: string;
  /** Send-date prefix used in the campaign name. Format MMDDYYYY. */
  datePrefix: string;
}

/** buildPayload — produces the canonical POST body. Output MUST match
 *  the Python script's body byte-for-byte (modulo HTML SHA-256). */
export function buildPayload(draft: CellDraft, opts: BuildPayloadOptions): PMTAPayload {
  const { ispsForPlans, quotas } = resolveQuotas(draft);
  const inclusion = inclusionFor(draft.slot, draft.brand);
  const exclusion = exclusionFor(draft.slot);
  const source = opts.timeSpanSource ?? 'duration-calc';

  const startWire = formatWireUTC(draft.startAtUTC);
  const endWire = formatWireUTC(draft.endAtUTC);

  const span = {
    type: 'absolute' as const,
    start_at: startWire,
    end_at: endWire,
    timezone: 'America/Denver' as const,
    source,
  };

  const ispPlans = ispsForPlans.map(isp => {
    const q = draft.ispQuotas[isp] ?? (draft.slot === 'welcome-newsletter' ? 0 : engagerAutoCap(draft.brand));
    return {
      isp,
      quota: q,
      randomize_audience: false as const,
      throttle_strategy: 'gentle' as const,
      timezone: 'America/Denver' as const,
      cadence: { mode: 'interval' as const, every_minutes: WAVE_INTERVAL_MINUTES, batch_size: 0 as const },
      time_spans: [span],
    };
  });

  const name = opts.campaignName
    ?? `${opts.datePrefix} - ${BRAND_LABEL[draft.brand]} - ${SLOT_NAME_FRAGMENT[draft.slot]} - ${draft.family}`;

  return {
    name,
    sending_domain: SENDING_DOMAIN[draft.brand],
    variants: [{
      variant_name: 'A',
      subject: draft.subject,
      preview_text: draft.preheader,
      from_name: BRAND_FROM_NAME[draft.brand],
      html_content: draft.htmlContent,
      plain_content: '',
      split_percent: 100,
    }],
    target_isps: ispsForPlans,
    send_priority: [{ id: inclusion[0], type: 'segment' }],
    inclusion_segments: inclusion,
    inclusion_lists: [],
    exclusion_segments: exclusion,
    exclusion_lists: [],
    timezone: 'America/Denver',
    throttle_strategy: 'gentle',
    isp_quotas: quotas,
    isp_plans: ispPlans,
    randomize_audience: false,
    send_mode: 'scheduled',
    scheduled_at: startWire,
    min_remail_hours: 0,
    use_master_selection: false,
    content_locked: false,
  };
}

/** validateInvariants — used by tests AND at runtime before deploy.
 *  Returns null if ok, error string if not. */
export function validateInvariants(payload: PMTAPayload): string | null {
  for (const plan of payload.isp_plans) {
    for (const span of plan.time_spans) {
      if (span.source !== 'duration-calc' && span.source !== 'manual') {
        return `time_spans[].source must be "duration-calc" or "manual" — got "${(span as { source: string }).source}". This silently fails waveSanityCheck.`;
      }
      if (!span.start_at || !span.end_at || span.start_at === span.end_at) {
        return `time_spans[] requires non-empty start_at != end_at — got start=${span.start_at} end=${span.end_at}`;
      }
    }
    if (plan.cadence.mode !== 'interval' || plan.cadence.every_minutes !== 15 || plan.cadence.batch_size !== 0) {
      return `cadence must be { interval, 15, 0 } per send-day rules — got ${JSON.stringify(plan.cadence)}`;
    }
  }
  if (payload.timezone !== 'America/Denver') return 'timezone must be America/Denver';
  if (payload.throttle_strategy !== 'gentle') return 'throttle_strategy must be gentle';
  if (payload.send_mode !== 'scheduled') return 'send_mode must be scheduled';
  if (payload.use_master_selection) return 'use_master_selection must be false';
  if (payload.content_locked) return 'content_locked must be false';
  return null;
}
