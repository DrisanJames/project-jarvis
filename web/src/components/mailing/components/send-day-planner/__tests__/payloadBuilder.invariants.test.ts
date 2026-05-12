// Source-field guard — explicit regression tests for the wizard bug
// where source: "global-default" was emitted with start_at == end_at,
// causing waveSanityCheck to silently fail. The Send-Day Planner
// MUST never emit anything except "duration-calc" or "manual" in the
// time_spans[].source field, and validateInvariants() MUST reject any
// other source string.

import { describe, expect, it } from 'vitest';
import { ALL_TARGET_ISPS, BRANDS, SLOT_ORDER } from '../constants';
import { baselineMatrix } from '../quotaMatrix';
import { scheduleFor, datePrefixFromSendDate } from '../schedule';
import { buildPayload, validateInvariants, type PMTAPayload } from '../payloadBuilder';
import type { CellDraft, ISP } from '../types';

const SEND_DATE = '2026-05-12';

function minimalCell(slot: typeof SLOT_ORDER[number], brand: typeof BRANDS[number]): CellDraft {
  const sched = scheduleFor(SEND_DATE, slot, brand);
  const ispQuotas: Partial<Record<ISP, number>> = {};
  if (slot === 'welcome-newsletter') {
    const m = baselineMatrix();
    for (const isp of ALL_TARGET_ISPS) {
      const v = m[brand][isp] ?? 0;
      if (v > 0) ispQuotas[isp] = v;
    }
  }
  return {
    brand, slot, family: 'warby-parker', filename: 'x.html',
    htmlContent: '<html></html>',
    subject: 's', preheader: 'p',
    ispQuotas, inclusionSegments: [], exclusionSegments: [],
    startAtUTC: sched.startAtUTC, endAtUTC: sched.endAtUTC,
    windowHours: sched.windowHours, status: 'draft',
  };
}

describe('payloadBuilder invariants — source field', () => {
  it('default source is "duration-calc"', () => {
    const draft = minimalCell('eng-w1', 'DB');
    const payload = buildPayload(draft, { datePrefix: datePrefixFromSendDate(SEND_DATE) });
    for (const plan of payload.isp_plans) {
      for (const span of plan.time_spans) {
        expect(span.source).toBe('duration-calc');
      }
    }
  });

  it('"manual" override is accepted', () => {
    const draft = minimalCell('welcome-newsletter', 'HT');
    const payload = buildPayload(draft, { datePrefix: datePrefixFromSendDate(SEND_DATE), timeSpanSource: 'manual' });
    expect(validateInvariants(payload)).toBeNull();
    for (const plan of payload.isp_plans) {
      for (const span of plan.time_spans) {
        expect(span.source).toBe('manual');
      }
    }
  });

  it('every cell across the 4x4 grid emits an allowed source', () => {
    const dp = datePrefixFromSendDate(SEND_DATE);
    for (const slot of SLOT_ORDER) {
      for (const brand of BRANDS) {
        const payload = buildPayload(minimalCell(slot, brand), { datePrefix: dp });
        expect(validateInvariants(payload)).toBeNull();
      }
    }
  });

  it('validateInvariants rejects "global-default" — wizard regression guard', () => {
    const draft = minimalCell('eng-w1', 'DB');
    const payload = buildPayload(draft, { datePrefix: datePrefixFromSendDate(SEND_DATE) });
    // Force-mutate to simulate any future buggy code path that smuggles
    // in a non-allowed source. validateInvariants MUST flag it.
    const tampered = JSON.parse(JSON.stringify(payload)) as PMTAPayload;
    for (const plan of tampered.isp_plans) {
      for (const span of plan.time_spans) {
        (span as { source: string }).source = 'global-default';
      }
    }
    const err = validateInvariants(tampered);
    expect(err).not.toBeNull();
    expect(err).toMatch(/duration-calc.*manual/);
  });

  it('validateInvariants rejects zero-span (start_at == end_at) — wizard regression guard', () => {
    const draft = minimalCell('eng-w1', 'QF');
    const payload = buildPayload(draft, { datePrefix: datePrefixFromSendDate(SEND_DATE) });
    const tampered = JSON.parse(JSON.stringify(payload)) as PMTAPayload;
    for (const plan of tampered.isp_plans) {
      for (const span of plan.time_spans) {
        span.end_at = span.start_at;
      }
    }
    const err = validateInvariants(tampered);
    expect(err).not.toBeNull();
    expect(err).toMatch(/start_at.*end_at/);
  });

  it('validateInvariants rejects bad cadence', () => {
    const draft = minimalCell('eng-w2', 'MH');
    const payload = buildPayload(draft, { datePrefix: datePrefixFromSendDate(SEND_DATE) });
    const tampered = JSON.parse(JSON.stringify(payload)) as PMTAPayload;
    tampered.isp_plans[0].cadence.every_minutes = 5;
    expect(validateInvariants(tampered)).toMatch(/cadence/);
  });

  it('validateInvariants rejects bad timezone / strategy / mode', () => {
    const dp = datePrefixFromSendDate(SEND_DATE);
    const base = buildPayload(minimalCell('eng-w3', 'HT'), { datePrefix: dp });
    {
      const t = JSON.parse(JSON.stringify(base)) as PMTAPayload;
      (t as { timezone: string }).timezone = 'UTC';
      expect(validateInvariants(t)).toMatch(/timezone/);
    }
    {
      const t = JSON.parse(JSON.stringify(base)) as PMTAPayload;
      (t as { throttle_strategy: string }).throttle_strategy = 'aggressive';
      expect(validateInvariants(t)).toMatch(/throttle_strategy/);
    }
    {
      const t = JSON.parse(JSON.stringify(base)) as PMTAPayload;
      (t as { send_mode: string }).send_mode = 'immediate';
      expect(validateInvariants(t)).toMatch(/send_mode/);
    }
    {
      const t = JSON.parse(JSON.stringify(base)) as PMTAPayload;
      (t as { use_master_selection: boolean }).use_master_selection = true;
      expect(validateInvariants(t)).toMatch(/use_master_selection/);
    }
    {
      const t = JSON.parse(JSON.stringify(base)) as PMTAPayload;
      (t as { content_locked: boolean }).content_locked = true;
      expect(validateInvariants(t)).toMatch(/content_locked/);
    }
  });
});
