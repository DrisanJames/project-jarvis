// Send-Day Planner — constants. EVERY value here MUST mirror
// deploy_may12_mature.py. Drift between this file and the Python
// is the failure mode the contract tests in Phase 4 catch.
//
// When the Python script changes anchors / windows / families on a
// given send-day, the operator updates BOTH sides in the same PR.

import type { Brand, ISP, Slot } from './types';

export const PAGE_VERSION = '2.1';

export const BRANDS: Brand[] = ['DB', 'QF', 'HT', 'MH'];

export const SLOT_ORDER: Slot[] = ['eng-w1', 'welcome-newsletter', 'eng-w2', 'eng-w3'];

export const BRAND_LABEL: Record<Brand, string> = {
  DB: 'Discount Blog',
  QF: 'Quiz Fiesta',
  HT: 'History Thinking',
  MH: 'My Own Health',
};

export const SENDING_DOMAIN: Record<Brand, string> = {
  DB: 'em.discountblog.com',
  QF: 'em.quizfiesta.com',
  HT: 'em.historythinking.com',
  MH: 'em.myownhealth.net',
};

export const BRAND_FROM_NAME: Record<Brand, string> = {
  DB: 'Jamie @ Discount Blog',
  QF: 'Quiz Master',
  HT: 'History Thinking',
  MH: 'Arnold @ My Own Health',
};

export const TEMPLATE_CODE: Record<Brand, string> = {
  DB: 'db', QF: 'qf', HT: 'ht', MH: 'mh',
};

// Segment IDs — copied verbatim from deploy_may12_mature.py.
export const MAILGUN_ENGAGED_MASTER_SEGMENT_ID = 'a03b2527-040a-49ae-9bdb-4c8fdab6e96e';
export const WELCOME_SATURATION_EXCLUDE_SEG_ID = '00000000-0000-0000-0000-0000005a7ed8';
export const WCM_MORTGAGE_SEGMENT_ID = 'c885bbf5-f82c-4e11-81da-fdf313cc5d17';
export const VERTICAL_FINANCE_SEGMENT_ID = '31e4f5d7-0afb-4f9f-a962-e8f197e9c383';
export const CONVERTERS_SEGMENT_ID = '021aa07d-9de3-42e4-8506-35a9a9884502';

export const ATTRIBITS_PARTITION_BY_BRAND: Record<Brand, string> = {
  DB: '9743a42c-9e85-4415-bbad-f2ed1498f40f',
  QF: '0f1784d3-d2d5-4891-8b5f-9e2e396cdbba',
  HT: 'e18fa7db-a45f-4c18-baf5-466c3d891d61',
  MH: 'd71c1802-13e8-43b4-ad82-a156f0de9310',
};

export const OPENERS_30D_BY_BRAND: Record<Brand, string> = {
  DB: '73e99960-e25d-4b16-9f21-41209a1254e4',
  QF: '15bb01f1-fd51-4130-98b1-5da099aa8959',
  HT: '5513ec96-a7b1-4d75-82a6-706796fe3ee5',
  MH: 'dd20e326-1a38-430c-89bb-802f8c46c77a',
};

export const STAGGER_MINUTES = 15;
export const WAVE_INTERVAL_MINUTES = 15;

// Slot anchors UTC: hour value matches deploy_may12_mature.SLOT_ANCHORS_UTC.
// The minute anchor is always :01 (mirrors `_anchor(hour, minute=1)`).
export const SLOT_ANCHOR_HOUR_UTC: Record<Slot, number> = {
  'eng-w1': 7,
  'welcome-newsletter': 9,
  'eng-w2': 16,
  'eng-w3': 18,
};

export const SLOT_ANCHOR_MINUTE_UTC = 1;

export const SLOT_WINDOW_HOURS: Record<Slot, number> = {
  'eng-w1': 4,
  'welcome-newsletter': 6,
  'eng-w2': 4,
  'eng-w3': 4,
};

export const SLOT_NAME_FRAGMENT: Record<Slot, string> = {
  'eng-w1': 'Engager Wave 1',
  'welcome-newsletter': 'Welcome - Newsletter',
  'eng-w2': 'Engager Wave 2',
  'eng-w3': 'Engager Wave 3',
};

export const ALL_TARGET_ISPS: ISP[] = [
  'microsoft', 'gmail', 'apple', 'yahoo',
  'comcast', 'charter', 'att', 'aol', 'cox', 'sbcglobal', 'other',
];

// Expected 30D opener counts per brand — used by the engager auto-cap formula.
export const EXPECTED_30D_OPENERS_PER_BRAND: Record<Brand, number> = {
  DB: 14000, QF: 10800, HT: 13800, MH: 12800,
};

// May 12 baseline welcome quotas — the canvas seeds these into the matrix
// when an operator picks "May 12" as the planner date or "Hold all" is the
// only transformation. Mirrors WELCOME_NL_QUOTAS in the Python.
export const WELCOME_NL_QUOTAS_BASELINE: Record<Brand, Record<ISP, number>> = {
  DB: { gmail: 1500, yahoo:  500, microsoft: 11003, apple:   960, aol:  1100, att:   7660, sbcglobal:   50, cox:    496, charter: 825, comcast: 2200, other:  825 },
  QF: { gmail:    0, yahoo:   50, microsoft: 10736, apple:  4500, aol:  2200, att:   7610, sbcglobal:    0, cox:    710, charter: 825, comcast: 2200, other:  825 },
  HT: { gmail: 2833, yahoo:   50, microsoft: 10710, apple:  3300, aol:  2200, att:   7660, sbcglobal:    0, cox:    720, charter: 825, comcast: 2200, other:  825 },
  MH: { gmail:    0, yahoo:   50, microsoft: 10644, apple:  3840, aol:  2200, att:   7660, sbcglobal:    0, cox:    710, charter: 825, comcast: 2200, other:  825 },
};

// Default engager + welcome family rotation for May 12. Mirrors
// ENGAGER_FAMILY + WELCOME_NL_FAMILY in the Python script. Operator can
// override per-cell via the canvas drawer.
// warby-parker is BANNED from newsletters (advertiser directive 2026-06-14) —
// replaced with personal-loans (the approved NL family; mirrors eng_w2_rotation.py).
export const ENGAGER_FAMILY: Record<Slot, Partial<Record<Brand, string>>> = {
  'eng-w1': { DB: 'quicken-loans',     QF: 'the-capital-wallet', HT: 'north-star-loans',    MH: 'personal-loans' },
  'eng-w2': { DB: 'the-capital-wallet', QF: 'personal-loans',     HT: 'quicken-loans',       MH: 'north-star-loans' },
  'eng-w3': { DB: 'personal-loans',     QF: 'north-star-loans',   HT: 'the-capital-wallet',  MH: 'quicken-loans' },
  'welcome-newsletter': {},
};
export const WELCOME_NL_FAMILY: Record<Brand, string> = {
  DB: 'north-star-loans', QF: 'quicken-loans', HT: 'personal-loans', MH: 'the-capital-wallet',
};

// Hard deploy targets — the canvas's Deploy button posts here.
export const DEPLOY_ENDPOINT = '/api/mailing/pmta-campaign/deploy';
export const DRY_RUN_ENDPOINT = '/api/mailing/pmta-campaign/dry-run';

// Send-day endpoints registered in upside-down/internal/api/server_routes_mailing.go.
export const SEND_DAY_VOLUME_RECONCILIATION_ENDPOINT = '/api/mailing/send-day/volume-reconciliation';
export const SEND_DAY_BANNED_CREATIVES_ENDPOINT = '/api/mailing/send-day/banned-creatives';
export const SEND_DAY_PREFLIGHT_BATCH_ENDPOINT = '/api/mailing/send-day/preflight-batch';
export const SEND_DAY_CREATIVE_RESOLVE_ENDPOINT = '/api/mailing/send-day/creative-resolve';
export const SEND_DAY_HOST_HEALTH_ENDPOINT = '/api/mailing/send-day/host-health';
export const SEND_DAY_HOST_HEALTH_ATTEST_ENDPOINT = '/api/mailing/send-day/host-health/attest';
export const WAVE_SCHEDULER_HEALTH_ENDPOINT = '/api/mailing/analytics/wave-scheduler-health';
export const HEALTH_ENDPOINT = '/health';

// Gate D domain source — the live fleet, NOT the 4-brand board above.
// (BRANDS/SENDING_DOMAIN mirror the May-12 board and predate the 16-brand
// fleet; Gate D must preflight every active sending domain.)
export const SENDING_DOMAINS_ENDPOINT = '/api/mailing/pmta-campaign/sending-domains';

// Required commit SHA prefix for Gate C — the IsPMTATransient classifier
// fix. Code in production must contain this commit or later.
export const GATE_C_REQUIRED_COMMIT = 'a92af78';

// Family copy tables — mirror ENGAGER_FAMILY_COPY + WELCOME_FAMILY_COPY.
// Used as the FALLBACK when /creative-resolve doesn't return a subject
// (offline / disk-load mode failed).
export const ENGAGER_FAMILY_COPY: Record<string, { subject: string; preheader: string }> = {
  // Fallback only (creative-resolve supplies the real subject). personal-loans
  // replaced the banned warby-parker family (2026-06-14).
  'personal-loans': {
    subject: 'Personal loan rates you might not know you qualify for',
    preheader: "A quick look at what today's personal-loan offers actually cost.",
  },
  'quicken-loans': {
    subject: 'Rates just moved again — your HELOC window is closing',
    preheader: "What this week's rate shift means for borrowing against your home.",
  },
  'the-capital-wallet': {
    subject: "You're losing money this month and don't know it yet",
    preheader: 'Three line items on your bills that are quietly inflating — fix them in 5 minutes.',
  },
  'north-star-loans': {
    subject: 'Balance transfer offers expire faster than the rate suggests',
    preheader: "What the promo window really costs once it ends — don't get caught.",
  },
};

export const WELCOME_FAMILY_COPY: Record<string, { subject: string; preheader: string }> = {
  // Fallback only (creative-resolve supplies the real subject). personal-loans
  // replaced the banned warby-parker family (2026-06-14).
  'personal-loans': {
    subject: 'How personal loans actually work in 2026',
    preheader: 'A plain-English look at rates, terms, and when a personal loan makes sense.',
  },
  'quicken-loans': {
    subject: 'How home equity actually moves when rates do',
    preheader: "A plain-English breakdown of when borrowing against your home is and isn't worth it.",
  },
  'the-capital-wallet': {
    subject: 'Where most monthly waste is hiding',
    preheader: "Three line items worth a five-minute audit before next month's statements.",
  },
  'north-star-loans': {
    subject: 'Reading the fine print on balance-transfer offers',
    preheader: 'What the introductory rate really costs once the promo window ends.',
  },
};
