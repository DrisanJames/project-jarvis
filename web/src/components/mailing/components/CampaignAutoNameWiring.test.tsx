import React from 'react';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

/**
 * RENDER-LEVEL guards for the campaign auto-naming feature
 * (PMTACampaignWizard.tsx:2132 derivedCampaignName / :2143 name effect /
 *  :2149 schedule-default effect / :2155 applyAnchorPreset).
 *
 * The pure-helper tests in CampaignAutoName.test.tsx pin the derivation.
 * These pin the LIFECYCLE — the part that decides which day the name claims
 * and whether it still agrees with the day the campaign will actually send.
 *
 * THE INVARIANT UNDER TEST:
 *   the MMDDYYYY token in the campaign name == the America/Denver calendar
 *   date of the instant the campaign is scheduled to send.
 * A name that claims a different day than the payload is exactly the
 * 08/20-08/21 board defect ('08062026' on an 08/20 campaign): it is invisible
 * to verify-by-name and forks the property in trend reporting.
 */

vi.mock('../../../contexts/AuthContext', () => ({
  useAuth: () => ({ organization: { id: 'org-test' } }),
}));
vi.mock('../shared/ToastSystem', () => ({
  useToast: () => ({ campaignComplete: vi.fn() }),
}));
// Leaf pickers are stubbed down to the callbacks the wizard actually wires:
// the wizard, not the picker, is the system under test.
vi.mock('./OfferCreativePicker', () => ({
  OfferCreativePicker: (p: any) => (
    <button
      onClick={() => {
        p.onOfferChange('offer-sams');
        p.onApply({
          proofId: 'proof-1', proofName: 'Sams proof',
          subject: 'Your membership', preheader: 'ph', fromName: 'Jamie',
          html: '<p>hello</p>',
        });
      }}
    >stub-pick-offer</button>
  ),
}));
vi.mock('./EngagementTierPicker', () => ({
  EngagementTierPicker: (p: any) => (
    <button onClick={() => p.onToggle('clickers', 'seg-clk-1')}>stub-pick-audience</button>
  ),
}));

import { PMTACampaignWizard } from './PMTACampaignWizard';

const DENVER = 'America/Denver';

/** MMDDYYYY of an instant, in the send-day zone. */
const denverToken = (d: Date) => {
  const p = new Intl.DateTimeFormat('en-US', {
    timeZone: DENVER, year: 'numeric', month: '2-digit', day: '2-digit',
  }).formatToParts(d).reduce<Record<string, string>>((a, x) => {
    if (x.type !== 'literal') a[x.type] = x.value; return a;
  }, {});
  return `${p.month}${p.day}${p.year}`;
};

const json = (body: any, status = 200) => ({
  ok: status >= 200 && status < 300,
  status,
  json: async () => body,
  text: async () => JSON.stringify(body),
});

const routeFetch = (url: string) => {
  if (url.includes('/pmta-campaign/draft')) return json({}, 404);
  if (url.includes('/pmta-campaign/sending-domains')) {
    const row = (domain: string) => ({
      domain, status: 'active',
      spf_configured: true, dkim_configured: true, dmarc_configured: true,
      pool_name: 'pool', ip_count: 4, ips: [], active_ips: 4, warmup_ips: 0,
      reputation_score: 98,
    });
    return json({ domains: [row('m.myownhealth.net'), row('m.consumerpro.net')] });
  }
  if (url.includes('/offers/list')) {
    return json({ offers: [
      { id: 'offer-sams', key: 'sams', name: "Sam's Club Membership", status: 'active' },
    ] });
  }
  if (url.includes('/pmta-campaign/warmup/domains')) return json({ domains: [] });
  if (url.includes('/pmta-campaign/engagement-tiers')) {
    return json({
      brand_root: 'myownhealth.net',
      clickers: [{ segment_id: 'seg-clk-1', name: 'MH 30D Clickers', count: 1000 }],
      openers: [], other: [],
    });
  }
  if (url.includes('/pmta-campaign/readiness')) return json({ isps: [] });
  if (url.includes('/segments')) return json({ segments: [] });
  if (url.includes('/suppression-lists')) return json({ lists: [] });
  if (url.includes('/lists')) return json({ lists: [] });
  return json({});
};

/** Drive the wizard to step 6 with a domain + offer + audience chosen. */
const driveToSchedule = async () => {
  render(<PMTACampaignWizard />);

  const domainCard = await screen.findByLabelText('Select m.myownhealth.net sending domain');
  fireEvent.click(domainCard);
  fireEvent.click(screen.getByText('Next'));                       // 1 -> 2
  fireEvent.click(screen.getByText('Next'));                       // 2 -> 3
  fireEvent.click(await screen.findByText('stub-pick-offer'));
  fireEvent.click(screen.getByText('Next'));                       // 3 -> 4
  fireEvent.click(await screen.findByText('stub-pick-audience'));
  fireEvent.click(screen.getByText('Next'));                       // 4 -> 6

  const nameInput = await screen.findByPlaceholderText('e.g. Q1 Gmail Warmup Blast') as HTMLInputElement;
  return { nameInput };
};

/**
 * The datetime-local input bound to `scheduledAt` — the value that becomes
 * payload.scheduled_at (PMTACampaignWizard.tsx:2718). It only renders in Quick
 * Schedule mode (:6455), so switch there first. Switching mode does not change
 * the value.
 */
const scheduleInput = () => {
  const quick = screen.getByText('Quick Schedule');
  fireEvent.click(quick);
  const inputs = document.querySelectorAll('input[type="datetime-local"]');
  expect(inputs.length).toBe(1);
  return inputs[0] as HTMLInputElement;
};

describe('PMTACampaignWizard auto-naming lifecycle', () => {
  it('an untouched auto-name follows a property switch', async () => {
    // The 08/20 board carried a 'DB'-named campaign whose payload sent from
    // consumerpro. While the operator has not typed, the derived name must
    // track the property that is actually selected.
    const { nameInput } = await driveToSchedule();
    await waitFor(() => expect(nameInput.value).toContain(' - MH - '));

    for (let i = 0; i < 4; i++) fireEvent.click(screen.getByText('Back')); // 6->4->3->2->1
    fireEvent.click(await screen.findByLabelText('Select m.consumerpro.net sending domain'));
    fireEvent.click(screen.getByText('Next'));
    fireEvent.click(screen.getByText('Next'));
    fireEvent.click(await screen.findByText('stub-pick-offer'));
    fireEvent.click(screen.getByText('Next'));
    fireEvent.click(await screen.findByText('stub-pick-audience'));
    fireEvent.click(screen.getByText('Next'));

    const after = await screen.findByPlaceholderText('e.g. Q1 Gmail Warmup Blast') as HTMLInputElement;
    await waitFor(() => expect(after.value).toBe(
      `${denverToken(new Date(scheduleInput().value))} - CP - Sams`));
  });

  beforeEach(() => {
    // Only Date is faked, so RTL's promise/microtask flushing is untouched.
    // 2026-08-21 09:00 MDT: the 12:01 PM-engager anchor is still >15 min out,
    // so applyAnchorPreset picks TODAY (PMTACampaignWizard.tsx:2157-2160).
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(new Date('2026-08-21T15:00:00Z'));
    (global.fetch as any) = vi.fn(async (url: string) => routeFetch(String(url)));
  });
  afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); });

  it('auto-fills the name for the day it defaults the schedule to', async () => {
    const { nameInput } = await driveToSchedule();
    await waitFor(() => expect(nameInput.value).not.toBe(''));

    const sched = scheduleInput();
    expect(sched.value).not.toBe('');
    expect(nameInput.value).toBe(`${denverToken(new Date(sched.value))} - MH - Sams`);
  });

  it('REGRESSION: the name follows the schedule when an anchor preset moves it to today', async () => {
    const { nameInput } = await driveToSchedule();
    await waitFor(() => expect(nameInput.value).not.toBe(''));

    // The operator picks the afternoon-engager anchor. applyAnchorPreset
    // (PMTACampaignWizard.tsx:2155) sets the send to TODAY 12:01 MT because it
    // is more than 15 minutes out — but derivedCampaignName (:2132) hardcodes
    // sendDayDateToken(1) and has no scheduledAt dependency, so the name keeps
    // claiming TOMORROW. That is a campaign named for the wrong day.
    fireEvent.click(screen.getByText('PM Engagers'));

    await waitFor(() => {
      const sched = scheduleInput();
      expect(nameInput.value.slice(0, 8)).toBe(denverToken(new Date(sched.value)));
    });
  });

  it('REGRESSION: the name follows a hand-edited schedule date', async () => {
    const { nameInput } = await driveToSchedule();
    await waitFor(() => expect(nameInput.value).not.toBe(''));

    const sched = scheduleInput();
    // Operator moves the send out to the 25th.
    fireEvent.change(sched, { target: { value: '2026-08-25T01:01' } });

    await waitFor(() => {
      expect(nameInput.value.slice(0, 8)).toBe(denverToken(new Date(scheduleInput().value)));
    });
  });

  it('keeps nameTouched across step navigation', async () => {
    const { nameInput } = await driveToSchedule();
    await waitFor(() => expect(nameInput.value).not.toBe(''));
    fireEvent.change(nameInput, { target: { value: 'MANUAL OVERRIDE' } });

    fireEvent.click(screen.getByText('Back'));   // 6 -> 4
    fireEvent.click(screen.getByText('Back'));   // 4 -> 3
    fireEvent.click(screen.getByText('Next'));   // 3 -> 4
    fireEvent.click(screen.getByText('Next'));   // 4 -> 6

    const again = await screen.findByPlaceholderText('e.g. Q1 Gmail Warmup Blast') as HTMLInputElement;
    await new Promise(r => setTimeout(r, 0));
    expect(again.value).toBe('MANUAL OVERRIDE');
  });

  it('REGRESSION: a Send Now / Schedule round trip keeps the anchor the operator picked', async () => {
    // The 01:01-tomorrow default used to re-seed here, because switching to
    // "Send Now" cleared scheduledAt and the seeding effect keyed only on
    // emptiness. Result: payload.scheduled_at said 08/22 01:01 while the
    // still-highlighted "PM Engagers" preset and every per-ISP time_span said
    // 08/21 12:01 — a 13-hour, one-day disagreement inside one payload.
    const { nameInput } = await driveToSchedule();
    fireEvent.click(screen.getByText('PM Engagers'));
    const chosen = scheduleInput().value;
    expect(chosen).toBe('2026-08-21T12:01');

    fireEvent.click(screen.getByText('Send Now'));
    fireEvent.click(screen.getByText('Schedule for Later'));

    expect(scheduleInput().value).toBe(chosen);
    await waitFor(() => expect(nameInput.value.slice(0, 8))
      .toBe(denverToken(new Date(scheduleInput().value))));
  });

  it('REGRESSION: the schedule field can be cleared and stays cleared', async () => {
    // With the seeding effect keyed only on `!scheduledAt`, blanking the field
    // re-fired it on the next commit and the value reappeared — the operator
    // could not empty their own input.
    await driveToSchedule();
    const sched = scheduleInput();
    fireEvent.change(sched, { target: { value: '' } });
    await new Promise(r => setTimeout(r, 0));
    expect(scheduleInput().value).toBe('');
  });

  it('never clobbers a name the operator typed', async () => {
    const { nameInput } = await driveToSchedule();
    await waitFor(() => expect(nameInput.value).not.toBe(''));

    fireEvent.change(nameInput, { target: { value: 'MANUAL OVERRIDE' } });
    fireEvent.click(screen.getByText('PM Engagers'));
    fireEvent.change(scheduleInput(), { target: { value: '2026-08-25T01:01' } });

    await new Promise(r => setTimeout(r, 0));
    expect(nameInput.value).toBe('MANUAL OVERRIDE');
  });
});
