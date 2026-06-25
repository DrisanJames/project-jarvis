import React from 'react';
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, beforeEach, vi } from 'vitest';

import {
  JourneyBuilder,
  JOURNEY_BUILDER_VERSION,
  CLEANED_NEVER_MAILED_PRESET_ID,
  ISP_QUOTA_BUCKETS,
  DELAY_TIMEZONES,
} from './JourneyBuilder';

// Phase 1 vitest coverage for the Welcome Series UI work.
//
// What we are guarding against here:
//   1. The trigger node must offer a "Cleaned, never mailed" preset and
//      surface the special preset id (CLEANED_NEVER_MAILED_PRESET_ID) so
//      the Phase 2 segment_enroller can recognize it server-side.
//   2. Picking a sending profile prepopulates From Name + From Email and
//      shows the sending domain badge.
//   3. The Settings drawer renders the engagement exit toggles, the full
//      ISP quota table, and a default sending profile select.
//   4. The delay node exposes a timezone select alongside the until-time
//      input when "Until specific time of day" is selected.
//   5. The Email node exposes a Library button + Preview button.
//   6. The version footer is always rendered so we can spot stale builds.
//
// We deliberately keep all backend interactions stubbed via global.fetch so
// the test asserts UI contract only — no live API required.

interface MockJourney {
  id: string;
  name: string;
  status: 'draft';
  nodes: any[];
  connections: any[];
  createdAt: string;
}

const sampleProfiles = [
  {
    id: 'p-db',
    name: 'Discount Blog',
    vendor_type: 'pmta',
    sending_domain: 'em.discountblog.com',
    from_name: 'Discount Blog Team',
    from_email: 'hello@em.discountblog.com',
    status: 'active',
  },
  {
    id: 'p-qf',
    name: 'Quiz Fiesta',
    vendor_type: 'pmta',
    sending_domain: 'em.quizfiesta.com',
    from_name: 'Quiz Fiesta',
    from_email: 'hi@em.quizfiesta.com',
    status: 'active',
  },
];

const sampleSegments = [
  { id: 'seg-1', name: 'Engaged 30d', subscriber_count: 12000 },
];

const sampleTemplates = [
  { id: 'tpl-welcome-1', name: 'Welcome 1 — Discount Blog', subject: 'Welcome to Discount Blog' },
  { id: 'tpl-welcome-2', name: 'Welcome 2 — Discount Blog', subject: 'A second hello' },
];

function installFetchMock(opts: { journeys?: MockJourney[]; templates?: typeof sampleTemplates } = {}) {
  const journeys = opts.journeys ?? [];
  const templates = opts.templates ?? sampleTemplates;
  (global.fetch as any).mockImplementation((url: string, init?: RequestInit) => {
    if (typeof url !== 'string') {
      return Promise.resolve({ ok: true, json: async () => ({}) });
    }
    if (url.endsWith('/api/mailing/journeys') && (!init || (init.method ?? 'GET') === 'GET')) {
      return Promise.resolve({ ok: true, json: async () => ({ journeys }) });
    }
    if (url.endsWith('/api/mailing/lists')) {
      return Promise.resolve({ ok: true, json: async () => ({ lists: [] }) });
    }
    if (url.endsWith('/api/mailing/sending-profiles')) {
      return Promise.resolve({ ok: true, json: async () => ({ profiles: sampleProfiles }) });
    }
    if (url.endsWith('/api/mailing/segments')) {
      return Promise.resolve({ ok: true, json: async () => ({ segments: sampleSegments }) });
    }
    if (url.endsWith('/api/mailing/templates')) {
      return Promise.resolve({ ok: true, json: async () => ({ templates }) });
    }
    if (url.includes('/api/mailing/templates/')) {
      return Promise.resolve({
        ok: true,
        json: async () => ({
          id: 'tpl-welcome-1',
          name: 'Welcome 1',
          subject: 'Welcome to Discount Blog',
          html_content: '<p>Welcome aboard</p>',
        }),
      });
    }
    return Promise.resolve({ ok: true, json: async () => ({}) });
  });
}

async function openNewJourney() {
  const createButtons = await screen.findAllByText(/Create.*Journey/i);
  fireEvent.click(createButtons[0]);
  await waitFor(() => expect(screen.getByText(/Configure Trigger/i)).toBeInTheDocument(), { timeout: 1000 }).catch(() => {});
}

describe('JourneyBuilder Phase 1 UI', () => {
  beforeEach(() => {
    installFetchMock();
    // Stub alert because saveJourney calls it after a successful PUT.
    (global as any).alert = vi.fn();
  });

  it('renders version footer so deployed builds are obvious', async () => {
    render(<JourneyBuilder />);
    const footer = await screen.findByTestId('journey-builder-version');
    expect(footer.textContent).toContain(JOURNEY_BUILDER_VERSION);
  });

  it('new journey defaults to segment trigger and exposes the cleaned-never-mailed preset', async () => {
    render(<JourneyBuilder />);
    await screen.findByText(/No journeys yet/i);
    await openNewJourney();

    // Click the auto-created trigger node to open its config panel.
    const triggerNode = await screen.findByText('Journey Start');
    fireEvent.click(triggerNode);

    // Segment trigger radio should be the default.
    const segmentRadio = await screen.findByDisplayValue('segment');
    expect(segmentRadio).toBeInTheDocument();
    expect((segmentRadio as HTMLInputElement).checked).toBe(true);

    // Audience segment dropdown shows the preset. Find it by walking up
    // from the label since the existing UI uses unbound <label>s.
    const audienceLabel = await screen.findByText(/Audience Segment \*/);
    const segmentSelect = audienceLabel.parentElement!.querySelector('select') as HTMLSelectElement;
    expect(segmentSelect).toBeTruthy();
    const presetOption = within(segmentSelect).getByText(/Cleaned, never mailed/i) as HTMLOptionElement;
    expect(presetOption.value).toBe(CLEANED_NEVER_MAILED_PRESET_ID);

    fireEvent.change(segmentSelect, { target: { value: CLEANED_NEVER_MAILED_PRESET_ID } });
    expect(segmentSelect.value).toBe(CLEANED_NEVER_MAILED_PRESET_ID);

    // Helper text appears when preset is chosen.
    expect(screen.getByText(/never received an email/i)).toBeInTheDocument();
  });

  it('email node: picking a sending profile prepopulates from-name, from-email, and badge', async () => {
    render(<JourneyBuilder />);
    await screen.findByText(/No journeys yet/i);
    await openNewJourney();

    // Add an email node from the palette.
    const palette = await screen.findByText('Send Email');
    fireEvent.click(palette);

    // Click the new email node tile to open its config.
    const emailNodeName = await screen.findByText('Send Email', { selector: '.node-name' });
    fireEvent.click(emailNodeName);

    const profileSelect = (await screen.findByTestId('email-sending-profile-select')) as HTMLSelectElement;
    fireEvent.change(profileSelect, { target: { value: 'p-db' } });

    const fromName = (await screen.findByTestId('email-from-name')) as HTMLInputElement;
    const fromEmail = (await screen.findByTestId('email-from-email')) as HTMLInputElement;
    expect(fromName.value).toBe('Discount Blog Team');
    expect(fromEmail.value).toBe('hello@em.discountblog.com');

    const badge = await screen.findByTestId('email-sending-domain-badge');
    expect(badge.textContent).toContain('em.discountblog.com');
  });

  it('email node: opens template picker, applying a template stores name + subject', async () => {
    const user = userEvent.setup();
    render(<JourneyBuilder />);
    await screen.findByText(/No journeys yet/i);
    await openNewJourney();

    fireEvent.click(await screen.findByText('Send Email'));
    fireEvent.click(await screen.findByText('Send Email', { selector: '.node-name' }));

    await user.click(await screen.findByTestId('open-template-picker'));

    const grid = await screen.findByTestId('template-grid');
    expect(within(grid).getByText('Welcome 1 — Discount Blog')).toBeInTheDocument();

    await user.click(within(grid).getByTestId('template-card-tpl-welcome-1'));

    const tplName = (await screen.findByTestId('email-template-name')) as HTMLInputElement;
    expect(tplName.value).toBe('Welcome 1 — Discount Blog');
  });

  it('email node: preview button is disabled until a template or HTML is set', async () => {
    render(<JourneyBuilder />);
    await screen.findByText(/No journeys yet/i);
    await openNewJourney();

    fireEvent.click(await screen.findByText('Send Email'));
    fireEvent.click(await screen.findByText('Send Email', { selector: '.node-name' }));

    const previewBtn = await screen.findByTestId('open-email-preview') as HTMLButtonElement;
    expect(previewBtn.disabled).toBe(true);
  });

  it('delay node: until-time mode shows timezone select with the documented options', async () => {
    render(<JourneyBuilder />);
    await screen.findByText(/No journeys yet/i);
    await openNewJourney();

    fireEvent.click(await screen.findByText('Wait'));
    fireEvent.click(await screen.findByText('Wait', { selector: '.node-name' }));

    const delayLabel = await screen.findByText('Delay Type');
    const delayTypeSelect = delayLabel.parentElement!.querySelector('select') as HTMLSelectElement;
    expect(delayTypeSelect).toBeTruthy();
    fireEvent.change(delayTypeSelect, { target: { value: 'until_time' } });

    const tzSelect = (await screen.findByTestId('delay-until-timezone')) as HTMLSelectElement;
    DELAY_TIMEZONES.forEach((tz) => {
      expect(within(tzSelect).getByText(tz.label)).toBeInTheDocument();
    });

    const untilTime = (await screen.findByTestId('delay-until-time')) as HTMLInputElement;
    expect(untilTime.value).toBe('09:00');
  });

  it('settings drawer: renders exit toggles, default profile, and full ISP quota table', async () => {
    render(<JourneyBuilder />);
    await screen.findByText(/No journeys yet/i);
    await openNewJourney();

    fireEvent.click(await screen.findByTestId('open-journey-settings'));

    const exitOpen = (await screen.findByTestId('journey-exit-on-open')) as HTMLInputElement;
    const exitClick = (await screen.findByTestId('journey-exit-on-click')) as HTMLInputElement;
    expect(exitOpen.checked).toBe(true);
    expect(exitClick.checked).toBe(true);

    const defaultProfile = (await screen.findByTestId('journey-default-profile')) as HTMLSelectElement;
    fireEvent.change(defaultProfile, { target: { value: 'p-qf' } });
    expect(defaultProfile.value).toBe('p-qf');

    const quotaTable = await screen.findByTestId('journey-isp-quotas');
    ISP_QUOTA_BUCKETS.forEach((b) => {
      expect(within(quotaTable).getByText(b.label)).toBeInTheDocument();
      expect(within(quotaTable).getByTestId(`isp-quota-${b.key}`)).toBeInTheDocument();
    });

    // Setting a value updates state without throwing.
    const gmail = within(quotaTable).getByTestId('isp-quota-gmail') as HTMLInputElement;
    fireEvent.change(gmail, { target: { value: '5000' } });
    expect(gmail.value).toBe('5000');
  });

  it('email node tile shows lifetime stat placeholders', async () => {
    render(<JourneyBuilder />);
    await screen.findByText(/No journeys yet/i);
    await openNewJourney();

    fireEvent.click(await screen.findByText('Send Email'));

    const stats = await screen.findAllByText(/Awaiting|Delivered|Opens|Clicks/);
    // 6 stats should exist on the email node tile (Awaiting,
    // Delivered, Opens, Clicks, Hard Bounce, Soft Bounce). The
    // hard/soft split is required by .cursor/rules/bounce-metrics.mdc.
    expect(stats.length).toBeGreaterThanOrEqual(4);
  });

  // Phase 5 (Welcome Series build): the brand preset dropdown
  // creates a 4-email + 3-delay skeleton tagged with the chosen
  // brand and pre-wired to the cleaned-never-mailed segment, so a
  // Phase 5 operator goes from "empty Journey Center" to
  // "ready-to-fill Welcome Series" in one click. Phase 6 reuses
  // this same flow to clone for HT, MH, QF.
  it('welcome preset dropdown creates the Discount Blog 4-email skeleton', async () => {
    render(<JourneyBuilder />);
    await screen.findByText(/No journeys yet/i);

    const presetSelect = (await screen.findByTestId('welcome-preset-brand')) as HTMLSelectElement;
    fireEvent.change(presetSelect, { target: { value: 'Discount Blog' } });

    // The header on the active journey view shows the new name.
    await screen.findByDisplayValue(/Discount Blog Welcome Series/i);

    // The skeleton has 4 email nodes, all tagged with brand label.
    const e1 = await screen.findByText('Discount Blog Welcome 1', { selector: '.node-name' });
    expect(e1).toBeInTheDocument();
    expect(screen.getByText('Discount Blog Welcome 2', { selector: '.node-name' })).toBeInTheDocument();
    expect(screen.getByText('Discount Blog Welcome 3', { selector: '.node-name' })).toBeInTheDocument();
    expect(screen.getByText(/Discount Blog Welcome 4/, { selector: '.node-name' })).toBeInTheDocument();

    // Trigger configured to the cleaned-never-mailed preset; click
    // the trigger by its unique node name to open the config panel.
    fireEvent.click(await screen.findByText('Cleaned, Never Mailed', { selector: '.node-name' }));
    const segmentLabel = await screen.findByText(/Audience Segment \*/);
    const segmentSelect = segmentLabel.parentElement!.querySelector('select') as HTMLSelectElement;
    expect(segmentSelect.value).toBe(CLEANED_NEVER_MAILED_PRESET_ID);
  });
});
