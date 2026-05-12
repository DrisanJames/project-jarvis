// GateStrip — gates render correctly per state, and allGatesPass()
// returns false unless every gate is green. The Deploy button in the
// canvas guards on allGatesPass() so this is the contract that
// keeps a half-failing send day from going live.

import { describe, expect, it, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { GateStrip, allGatesPass } from '../GateStrip';
import type { GateState } from '../types';

function fullPass(): GateState {
  return {
    gateA: { passes: true, servers: { server_a: { state: 'healthy' }, server_b: { state: 'healthy' } } },
    gateB: { passes: true, zombies: 0, expired: 0, due_now: 0 },
    gateC: { passes: true, git_sha: 'a92af78abcdef0', required_commit: 'a92af78' },
    gateD: { passes: true, results: { 'em.discountblog.com': { ok: true, errors: [], warnings: [] } } },
    gateE: { passes: true, reviewed_at: '2026-05-12T00:00:00Z', reviewed_by: 'operator' },
    gateF: { passes: true, today_planned: 320000, yesterday_planned: 250000, target: 300000, ramp_floor: 285000, gap: 0, percent_to_target: 1.07 },
  };
}

describe('GateStrip', () => {
  it('renders all six gate labels', () => {
    const state = fullPass();
    render(<GateStrip state={state} onToggleAuditReviewed={() => {}} onRefresh={() => {}} loading={false} />);
    expect(screen.getByText(/PMTA Stability/)).toBeInTheDocument();
    expect(screen.getByText(/Wave Dispatcher/)).toBeInTheDocument();
    expect(screen.getByText(/Dead-Letter SHA/)).toBeInTheDocument();
    expect(screen.getByText(/Sending Profiles/)).toBeInTheDocument();
    expect(screen.getByText(/Audit Reviewed/)).toBeInTheDocument();
    expect(screen.getByText(/Volume Ramp/)).toBeInTheDocument();
  });

  it('shows pass for green gates and fail for red', () => {
    const state = fullPass();
    state.gateF = { passes: false, today_planned: 220000, yesterday_planned: 250000, target: 300000, ramp_floor: 285000, gap: -65000, percent_to_target: 0.73 };
    render(<GateStrip state={state} onToggleAuditReviewed={() => {}} onRefresh={() => {}} loading={false} />);
    const passLabels = screen.getAllByText(/^pass$/);
    const failLabels = screen.getAllByText(/^fail$/);
    expect(passLabels.length).toBe(5);
    expect(failLabels.length).toBe(1);
  });

  it('Refresh button fires onRefresh', () => {
    const onRefresh = vi.fn();
    render(<GateStrip state={fullPass()} onToggleAuditReviewed={() => {}} onRefresh={onRefresh} loading={false} />);
    fireEvent.click(screen.getByRole('button', { name: /Refresh/ }));
    expect(onRefresh).toHaveBeenCalledTimes(1);
  });

  it('Refresh button disabled and shows Refreshing… while loading', () => {
    render(<GateStrip state={fullPass()} onToggleAuditReviewed={() => {}} onRefresh={() => {}} loading={true} />);
    const btn = screen.getByRole('button');
    expect(btn).toBeDisabled();
    expect(btn).toHaveTextContent(/Refreshing/);
  });

  it('Gate E click invokes onToggleAuditReviewed', () => {
    const onToggle = vi.fn();
    render(<GateStrip state={fullPass()} onToggleAuditReviewed={onToggle} onRefresh={() => {}} loading={false} />);
    fireEvent.click(screen.getByText(/Audit Reviewed/));
    expect(onToggle).toHaveBeenCalledTimes(1);
  });
});

describe('allGatesPass', () => {
  it('true only when every gate passes', () => {
    expect(allGatesPass(fullPass())).toBe(true);
  });

  it('false if any gate is null', () => {
    const s = fullPass();
    s.gateA = null;
    expect(allGatesPass(s)).toBe(false);
  });

  it('false if any gate fails', () => {
    const s = fullPass();
    s.gateF = { ...s.gateF!, passes: false };
    expect(allGatesPass(s)).toBe(false);
  });

  it('false if Gate E (audit reviewed) is not yet attested', () => {
    const s = fullPass();
    s.gateE = null;
    expect(allGatesPass(s)).toBe(false);
  });
});
