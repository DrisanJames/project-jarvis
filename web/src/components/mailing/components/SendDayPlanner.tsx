// Send-Day Planner — pre-deploy gates over the REAL plan.
//
// 2026-06-20 rewrite (operator: "rewire to mirror reality"). The planner used to
// SYNTHESIZE a 4-brand × 4-slot engager-newsletter plan from hardcoded constants
// (ENGAGER_FAMILY / EXPECTED_30D_OPENERS_PER_BRAND × 1.5) and deploy THAT — so it
// showed loan newsletters regardless of what was actually scheduled (e.g. it could
// never show the real Sam's/Liberty board). It is now a faithful mirror: the six
// pre-deploy gates (A–F) sit on top of the REAL plan for the selected date, rendered
// by DraftBoardView (the sending-domain → campaign → per-ISP view that reads the
// live campaigns endpoint). No more client-side synthesis; the plan you see is the
// plan that is actually staged/scheduled. Deploys happen through the Draft Board's
// Approve action / the scheduling pipeline, not a synthetic POST from this screen.
//
// PAGE_VERSION is bumped on every behaviour change per testing.mdc.

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { useAuth } from '../../../contexts/AuthContext';

import { GateStrip } from './send-day-planner/GateStrip';
import { BrandWaveGrid } from './BrandWaveGrid';
import {
  BRANDS,
  GATE_C_REQUIRED_COMMIT,
  HEALTH_ENDPOINT,
  PAGE_VERSION,
  SEND_DAY_HOST_HEALTH_ATTEST_ENDPOINT,
  SEND_DAY_HOST_HEALTH_ENDPOINT,
  SEND_DAY_PREFLIGHT_BATCH_ENDPOINT,
  SEND_DAY_VOLUME_RECONCILIATION_ENDPOINT,
  SENDING_DOMAIN,
  WAVE_SCHEDULER_HEALTH_ENDPOINT,
} from './send-day-planner/constants';
import type { GateState } from './send-day-planner/types';

interface SendDayPlannerProps {
  // Retained for the MailingPortal call site; edit/cancel now happen inside the
  // embedded Draft Board, so these are no longer invoked here.
  onEditInWizard: (campaignId: string) => void;
  onCampaignPreparing?: (campaignId: string, name: string) => void;
}

export const SendDayPlanner: React.FC<SendDayPlannerProps> = () => {
  const { organization } = useAuth();
  const orgId = organization?.id ?? '00000000-0000-0000-0000-000000000001';

  // Default to TOMORROW (MDT) — the operator brief is always for the next send-day.
  const [sendDate, setSendDate] = useState(() => {
    const t = new Date();
    t.setUTCDate(t.getUTCDate() + 1);
    return t.toISOString().slice(0, 10);
  });

  const [gates, setGates] = useState<GateState>({
    gateA: null, gateB: null, gateC: null, gateD: null, gateE: null, gateF: null,
  });
  const [gatesLoading, setGatesLoading] = useState(false);

  // ── Gate refresh (A–F, all live endpoints) ───────────────────────────────
  const refreshGates = useCallback(async () => {
    setGatesLoading(true);
    try {
      const headers = { 'X-Organization-ID': orgId };
      const domains = BRANDS.map(b => SENDING_DOMAIN[b]);
      const [hostRes, healthRes, ramprRes, prefRes, sysRes] = await Promise.allSettled([
        fetch(SEND_DAY_HOST_HEALTH_ENDPOINT, { headers }).then(r => r.json()),
        fetch(WAVE_SCHEDULER_HEALTH_ENDPOINT, { headers }).then(r => r.json()),
        fetch(`${SEND_DAY_VOLUME_RECONCILIATION_ENDPOINT}?date=${sendDate}`, { headers }).then(r => r.json()),
        fetch(SEND_DAY_PREFLIGHT_BATCH_ENDPOINT, {
          method: 'POST', headers: { ...headers, 'Content-Type': 'application/json' },
          body: JSON.stringify({ domains }),
        }).then(r => r.json()),
        fetch(HEALTH_ENDPOINT).then(r => r.json()).catch(() => ({})),
      ]);
      setGates(prev => ({
        gateA: hostRes.status === 'fulfilled' ? {
          passes: !!hostRes.value.ok,
          servers: hostRes.value.servers ?? {},
          guidance: hostRes.value.guidance,
        } : null,
        gateB: healthRes.status === 'fulfilled' ? {
          passes: (healthRes.value.summary?.zombies ?? 0) < 50 && (healthRes.value.summary?.expired ?? 0) < 50,
          zombies: healthRes.value.summary?.zombies ?? 0,
          expired: healthRes.value.summary?.expired ?? 0,
          due_now: healthRes.value.summary?.due_now ?? 0,
        } : null,
        gateC: sysRes.status === 'fulfilled' ? {
          passes: typeof sysRes.value?.build?.git_sha === 'string',
          git_sha: sysRes.value?.build?.git_sha ?? '',
          required_commit: GATE_C_REQUIRED_COMMIT,
        } : null,
        gateD: prefRes.status === 'fulfilled' ? {
          passes: !!prefRes.value.all_ok,
          results: prefRes.value.results ?? {},
        } : null,
        // Gate E is operator-attested; preserve previous toggle state.
        gateE: prev.gateE,
        gateF: ramprRes.status === 'fulfilled' ? {
          passes: !!ramprRes.value.passes,
          today_planned: ramprRes.value.today_planned ?? 0,
          yesterday_planned: ramprRes.value.yesterday_planned ?? 0,
          target: ramprRes.value.target ?? 0,
          ramp_floor: ramprRes.value.ramp_floor ?? 0,
          gap: ramprRes.value.gap ?? 0,
          percent_to_target: ramprRes.value.percent_to_target ?? 0,
        } : null,
      }));
    } finally {
      setGatesLoading(false);
    }
  }, [orgId, sendDate]);

  useEffect(() => { refreshGates(); }, [refreshGates]);

  const onToggleAuditReviewed = useCallback(() => {
    setGates(prev => ({
      ...prev,
      gateE: { passes: !(prev.gateE?.passes ?? false), reviewed_at: new Date().toISOString(), reviewed_by: 'operator' },
    }));
  }, []);

  // Gate A is operator-attested in v1 (the agent shell is firewalled from PMTA SSH).
  const onAttestGateA = useCallback(async (state: 'pass' | 'fail') => {
    for (const key of ['server_a', 'server_b']) {
      await fetch(SEND_DAY_HOST_HEALTH_ATTEST_ENDPOINT, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-Organization-ID': orgId },
        body: JSON.stringify({ server_key: key, state, message: 'attested via planner', updated_by: 'operator' }),
      });
    }
    await refreshGates();
  }, [orgId, refreshGates]);

  // ── Render ────────────────────────────────────────────────────────────────
  const sendDateMemo = useMemo(() => sendDate, [sendDate]);
  return (
    <div style={{ padding: 18, color: 'rgba(220,235,250,0.92)' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 16, marginBottom: 16, flexWrap: 'wrap' }}>
        <h2 style={{ margin: 0, fontSize: 18, color: '#00e5ff' }}>Send Day Planner · Gates + Plan</h2>
        <label style={{ fontSize: 12, color: 'rgba(180,210,240,0.75)' }}>
          Date{' '}
          <input
            type="date"
            value={sendDate}
            onChange={e => setSendDate(e.target.value)}
            style={{
              background: 'rgba(13,21,38,0.9)', border: '1px solid rgba(0,200,255,0.25)',
              color: '#cbd5f5', padding: '4px 8px', borderRadius: 6, fontSize: 12,
            }}
          />
        </label>
        <button onClick={refreshGates} style={btnPrimary}>Refresh gates</button>
        <div style={{ marginLeft: 'auto', fontSize: 11, color: 'rgba(180,210,240,0.6)' }}>
          mirrors the live plan for {sendDate}
        </div>
      </div>
      <div style={{ marginBottom: 16 }}>
        <GateStrip
          state={gates}
          onToggleAuditReviewed={onToggleAuditReviewed}
          onRefresh={refreshGates}
          loading={gatesLoading}
        />
        {!gates.gateA?.passes && (
          <div style={{ marginTop: 8, fontSize: 11, color: 'rgba(180,210,240,0.7)' }}>
            Gate A is confirmed manually for now. {' '}
            <button onClick={() => onAttestGateA('pass')} style={attestBtn}>I confirmed sending server A + B is healthy</button>
          </div>
        )}
      </div>
      {/* The REAL plan for the selected date — ALL brands as horizontally-scrollable
          columns × wave rows, read live from the campaigns endpoint. No synthesis. */}
      <BrandWaveGrid date={sendDateMemo} />
      <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.45)', textAlign: 'right', marginTop: 12 }}>
        SendDayPlanner v{PAGE_VERSION}
      </div>
    </div>
  );
};

// Default + named export so MailingPortal can import either way.
export default SendDayPlanner;

const btnPrimary: React.CSSProperties = {
  background: 'rgba(0,176,255,0.15)', border: '1px solid rgba(0,176,255,0.5)',
  color: '#00e5ff', padding: '6px 12px', borderRadius: 6, fontSize: 12, fontWeight: 600, cursor: 'pointer',
};
const attestBtn: React.CSSProperties = {
  background: 'rgba(0,184,148,0.12)', border: '1px solid rgba(0,184,148,0.5)',
  color: '#00b894', padding: '4px 10px', borderRadius: 5, fontSize: 11, fontWeight: 600, cursor: 'pointer',
};
