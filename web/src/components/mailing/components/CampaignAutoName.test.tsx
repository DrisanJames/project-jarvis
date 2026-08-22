import { describe, it, expect } from 'vitest';
import {
  brandPrefixForDomain, offerTokenForName, sendDayDateToken,
  sendDayTokenForInstant, nameDateTokenForSchedule,
} from './PMTACampaignWizard';

/**
 * Every campaign-name defect on the 08/20-08/21 boards came from hand-typing:
 *   'MF' for MH · '08062026' and '080202026' for 08202026 · a 'DB' name on a
 *   consumerpro payload. These pin the derivation that replaces the typing.
 */
describe('brandPrefixForDomain', () => {
  it('maps SES m.* domains to the board brand prefix', () => {
    expect(brandPrefixForDomain('m.myownhealth.net')).toBe('MH');
    expect(brandPrefixForDomain('m.consumerpro.net')).toBe('CP');
    expect(brandPrefixForDomain('m.discountblog.com')).toBe('DB');
    expect(brandPrefixForDomain('m.homewarrantyservices.org')).toBe('HWS');
  });

  it('maps PMTA em.* domains identically — prefix must not depend on transport', () => {
    expect(brandPrefixForDomain('em.quizfiesta.com')).toBe('QF');
    expect(brandPrefixForDomain('em.learnpersonalloans.com')).toBe('LPL');
  });

  it('never invents a brand for an unknown domain', () => {
    // 'MF' was accepted as a brand because nothing validated it.
    expect(brandPrefixForDomain('m.notabrand.com')).toBe('');
    expect(brandPrefixForDomain('')).toBe('');
  });
});

describe('offerTokenForName', () => {
  it('shortens catalogue names to the board token', () => {
    expect(offerTokenForName("Sam's Club Membership - Partner Drip (4989)")).toBe('Sams');
    expect(offerTokenForName('Globe Life - New Form')).toBe('Globe');
    expect(offerTokenForName('Choice Home Warranty CPL')).toBe('CHW');
    expect(offerTokenForName('Accredited Debt Relief')).toBe('ADR');
    expect(offerTokenForName('Get Metal Roofing (EF 9539)')).toBe('MR');
  });

  it('prefers the longest match so a short key cannot shadow a longer one', () => {
    // 'adt' must not win inside 'West Shore ... ' style names
    expect(offerTokenForName('West Shore Windows')).toBe('Westshore');
  });

  it('falls back to the first word rather than an empty segment', () => {
    expect(offerTokenForName('Brandnew Offer XYZ')).toBe('Brandnew');
    expect(offerTokenForName('')).toBe('');
  });
});

describe('sendDayDateToken', () => {
  it('is 8 digits — the 080202026 / 08062026 defects were length and value errors', () => {
    expect(sendDayDateToken(1)).toMatch(/^\d{8}$/);
    expect(sendDayDateToken(0)).toMatch(/^\d{8}$/);
  });

  it('tomorrow differs from today', () => {
    expect(sendDayDateToken(1)).not.toBe(sendDayDateToken(0));
  });

  it('parses as MMDDYYYY with a plausible month and day', () => {
    const t = sendDayDateToken(1);
    const mm = Number(t.slice(0, 2)); const dd = Number(t.slice(2, 4));
    const yyyy = Number(t.slice(4));
    expect(mm).toBeGreaterThanOrEqual(1); expect(mm).toBeLessThanOrEqual(12);
    expect(dd).toBeGreaterThanOrEqual(1); expect(dd).toBeLessThanOrEqual(31);
    expect(yyyy).toBeGreaterThanOrEqual(2026);
  });
});

describe('nameDateTokenForSchedule', () => {
  // The name must claim the day the mail actually lands. A fixed "tomorrow"
  // is how a campaign scheduled for the 21st ends up named 0822 — invisible to
  // verify-by-name, and a forked property in trend reporting.
  it('reads the date off the schedule the operator actually set', () => {
    expect(nameDateTokenForSchedule('2026-08-21T12:01', 'scheduled')).toBe('08212026');
    expect(nameDateTokenForSchedule('2026-08-25T01:01', 'scheduled')).toBe('08252026');
  });

  it('an immediate send is named for TODAY, never tomorrow', () => {
    expect(nameDateTokenForSchedule('2026-08-25T01:01', 'immediate')).toBe(sendDayDateToken(0));
    expect(nameDateTokenForSchedule('', 'immediate')).toBe(sendDayDateToken(0));
  });

  it('falls back to the wizard default (tomorrow 01:01 MT) only when nothing is set', () => {
    expect(nameDateTokenForSchedule('', 'scheduled')).toBe(sendDayDateToken(1));
    expect(nameDateTokenForSchedule('not-a-date', 'scheduled')).toBe(sendDayDateToken(1));
  });

  it('reads the calendar date in the SEND-DAY zone, not the browser zone', () => {
    // 2026-08-22 05:30 UTC is still 2026-08-21 in Denver. A campaign firing at
    // that instant belongs to the 08/21 board.
    expect(sendDayTokenForInstant(new Date('2026-08-22T05:30:00Z'))).toBe('08212026');
    expect(sendDayTokenForInstant(new Date('2026-08-22T06:30:00Z'))).toBe('08222026');
  });
});
