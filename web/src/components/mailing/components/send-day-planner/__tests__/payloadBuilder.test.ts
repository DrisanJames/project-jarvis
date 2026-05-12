// Contract tests — for each (slot, brand) of {SLOT_ORDER × BRANDS}, the
// canvas's buildPayload(uiState) MUST emit a body byte-equal to the
// `--dump-post-body` golden produced by deploy_may12_mature.py.
//
// HTML content is compared by SHA-256 (the goldens store `_html_sha256`
// + a sidecar `.html` file rather than embedding 30k+ lines of HTML
// directly).

import { describe, expect, it } from 'vitest';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  BRANDS,
  ENGAGER_FAMILY,
  ENGAGER_FAMILY_COPY,
  SLOT_ORDER,
  TEMPLATE_CODE,
  WELCOME_FAMILY_COPY,
  WELCOME_NL_FAMILY,
  ALL_TARGET_ISPS,
} from '../constants';
import { buildPayload } from '../payloadBuilder';
import { baselineMatrix } from '../quotaMatrix';
import { datePrefixFromSendDate, scheduleFor } from '../schedule';
import type { CellDraft } from '../types';

const FIXTURE_DIR = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  '..', '__fixtures__',
);
const SEND_DATE = '2026-05-12';

function sha256(s: string): string {
  return createHash('sha256').update(s, 'utf-8').digest('hex');
}

function buildCellFromConstants(slot: typeof SLOT_ORDER[number], brand: typeof BRANDS[number]): CellDraft {
  const sched = scheduleFor(SEND_DATE, slot, brand);
  const family = slot === 'welcome-newsletter'
    ? WELCOME_NL_FAMILY[brand]
    : (ENGAGER_FAMILY[slot]?.[brand] ?? '');
  const copy = slot === 'welcome-newsletter'
    ? WELCOME_FAMILY_COPY[family]
    : ENGAGER_FAMILY_COPY[family];
  const datePrefix = datePrefixFromSendDate(SEND_DATE);
  const filename = `${family}-${TEMPLATE_CODE[brand]}-newsletter-${datePrefix}.html`;

  // Welcome quotas come from the May-12 baseline matrix; engager quotas
  // come from the auto-cap formula (resolveQuotas() handles that), so an
  // empty ispQuotas{} for engagers is correct.
  const matrix = baselineMatrix();
  const ispQuotas: Partial<Record<typeof ALL_TARGET_ISPS[number], number>> = {};
  if (slot === 'welcome-newsletter') {
    for (const isp of ALL_TARGET_ISPS) {
      const v = matrix[brand][isp] ?? 0;
      if (v > 0) ispQuotas[isp] = v;
    }
  }

  // Load HTML from the sidecar fixture file (pre-pipeline mutated).
  const htmlPath = path.join(FIXTURE_DIR, `${slot.replace(/-/g, '_')}_${brand}.html`);
  const htmlContent = readFileSync(htmlPath, 'utf-8');

  return {
    brand, slot, family, filename, htmlContent,
    subject: copy?.subject ?? '',
    preheader: copy?.preheader ?? '',
    ispQuotas,
    inclusionSegments: [],
    exclusionSegments: [],
    startAtUTC: sched.startAtUTC,
    endAtUTC: sched.endAtUTC,
    windowHours: sched.windowHours,
    status: 'draft',
  };
}

describe('payloadBuilder — golden contract', () => {
  for (const slot of SLOT_ORDER) {
    for (const brand of BRANDS) {
      it(`${slot} × ${brand} matches Python golden`, () => {
        const cell = buildCellFromConstants(slot, brand);
        const datePrefix = datePrefixFromSendDate(SEND_DATE);
        const payload = buildPayload(cell, { datePrefix });

        const goldenPath = path.join(FIXTURE_DIR, `${slot.replace(/-/g, '_')}_${brand}.post.json`);
        const golden = JSON.parse(readFileSync(goldenPath, 'utf-8')) as Record<string, unknown>;

        // SHA-256 check on HTML body — goldens clear html_content and
        // store a sidecar plus _html_sha256 instead.
        const goldenVariants = (golden as { variants: Array<{ _html_sha256?: string }> }).variants;
        const goldenSha = goldenVariants[0]._html_sha256;
        const ourSha = sha256(payload.variants[0].html_content);
        expect(ourSha).toBe(goldenSha);

        // Strip the marker fields from the golden + zero out our HTML
        // content for the structural comparison.
        const goldenForCompare = JSON.parse(JSON.stringify(golden)) as { variants: Array<Record<string, unknown>> };
        for (const v of goldenForCompare.variants) {
          delete v._html_sha256;
          delete v._html_filename;
          v.html_content = '';
        }
        const oursForCompare = JSON.parse(JSON.stringify(payload)) as { variants: Array<Record<string, unknown>> };
        for (const v of oursForCompare.variants) {
          v.html_content = '';
        }
        expect(oursForCompare).toEqual(goldenForCompare);
      });
    }
  }
});
