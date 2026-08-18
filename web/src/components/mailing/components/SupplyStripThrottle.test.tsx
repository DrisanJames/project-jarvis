// SupplyStripThrottle.test.tsx — Pipeline Cockpit P2 DoD, rendered-markup
// negative control: the throttle EDIT surface renders ONLY when the server
// reports write_enabled=true (env PROPERTY_LEDGER_THROTTLE_WRITE_ENABLED=1,
// surfaced in the throttle read). Flag off ⇒ read-only panel with an honest
// banner NAMING the env var, and no edit control in the markup at all.

import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { SupplyStrip } from './PropertyLedgerView';

const supplyData = {
  domain: 'discountblog.com', brand: 'db', sending_domain: 'em.discountblog.com',
  non_ledger: false, as_of: '2026-08-17T12:00:00Z', denver_day: '2026-08-17',
  ready_semantics: 'ready = EO Verified (1) + Complainer (7) — the validator\'s markReady path',
  supply_note: 'Live queue facts',
  feeds: [{
    dataset_id: '11111111-1111-1111-1111-111111111111', name: 'feed-one',
    vertical: 'homeimprovement', status: 'active', daily_cap: 5000,
    paused_emergency: false, shared_brands: ['db', 'ht'],
    tranche_total: 1000, cleaning: 150, pending_eo: 100, eo_in_flight: 50,
    ready_total: 300, ready_by_isp: [{ isp: 'gmail', ready: 150 }],
    held: 200, suppressed: 40, dead_letter: 10, mailed_lifetime: 300, mailed_today: 25,
  }],
};

const poll = (data: typeof supplyData | null, error: string | null = null) => ({
  data, loading: false, error, secondsSinceUpdate: 5, live: true, refresh: () => {},
});

const throttleResp = (writeEnabled: boolean) => ({
  domain: 'discountblog.com', brand: 'db', sending_domain: 'em.discountblog.com',
  non_ledger: false,
  write_enabled: writeEnabled,
  write_flag_env: 'PROPERTY_LEDGER_THROTTLE_WRITE_ENABLED',
  write_endpoint: '/api/mailing/data-partners/datasets/{id}/isp-distribution',
  replacement_note: 'Writes REPLACE the lane\'s override set.',
  enforcement_note: 'LIVE enforcement input.',
  cap_systems_note: 'Two distinct cap systems.',
  feeds: [{
    dataset_id: '11111111-1111-1111-1111-111111111111', name: 'feed-one',
    vertical: 'homeimprovement', status: 'active', paused_emergency: false,
    supply_release_daily_cap: 5000, shared_brands: ['db', 'ht'],
    overrides: [{ isp: 'gmail', pct_override: 0.4, max_per_wave: 1000, daily_cap: 500,
      updated_at: '2026-08-17T12:00:00Z', updated_by: 'operator' }],
    default_isps: ['yahoo', 'microsoft', 'other'],
  }],
});

const renderStrip = (writeEnabled: boolean) =>
  render(
    <SupplyStrip
      domain="discountblog.com"
      supply={poll(supplyData)}
      throttle={throttleResp(writeEnabled)}
      throttleErr={null}
      onThrottleChanged={() => {}}
      onNotice={() => {}}
    />,
  );

describe('SupplyStrip throttle gating (server flag)', () => {
  it('flag OFF: read-only — banner names the env var, NO edit control rendered', () => {
    renderStrip(false);
    // Honest banner naming the gate env var.
    expect(screen.getByText(/PROPERTY_LEDGER_THROTTLE_WRITE_ENABLED/)).toBeInTheDocument();
    expect(screen.getAllByText(/read-only/i).length).toBeGreaterThan(0);
    // The edit surface is ABSENT from the markup, not merely disabled.
    expect(screen.queryByText('Edit throttle')).toBeNull();
    expect(screen.queryByText(/Replace throttle/)).toBeNull();
    // The READ side still renders: the override row is visible.
    expect(screen.getByText(/wave ≤1,000/)).toBeInTheDocument();
  });

  it('flag ON: the edit control renders and the disabled banner is gone', () => {
    renderStrip(true);
    expect(screen.getByText('Edit throttle')).toBeInTheDocument();
    expect(screen.queryByText(/Throttle editing is DISABLED/)).toBeNull();
  });

  it('supply 503 renders the warming-up state, not a failure', () => {
    render(
      <SupplyStrip
        domain="discountblog.com"
        supply={poll(null, 'HTTP 503: supply view warming up — idx_pcq_dataset_status_mailed is still building')}
        throttle={null}
        throttleErr={null}
        onThrottleChanged={() => {}}
        onNotice={() => {}}
      />,
    );
    expect(screen.getByText(/warming up/i)).toBeInTheDocument();
    expect(screen.queryByText(/Supply refresh failed/)).toBeNull();
  });
});
