// Send-Day Planner — first-class multi-campaign canvas.
//
// Operates over a 4 brand × 4 slot grid (16 cells) and emits the SAME
// POST body to /api/mailing/pmta-campaign/deploy that
// deploy_may12_mature.py builds. Phase 4 contract tests assert
// byte-equality to a Python `--dump-post-body` golden fixture.
//
// PAGE_VERSION is bumped on every behaviour change per testing.mdc.

import React, { useCallback, useEffect, useMemo, useReducer, useState } from 'react';
import { useAuth } from '../../../contexts/AuthContext';
import { useToast } from '../shared/ToastSystem';

import { BrandSlotGrid } from './send-day-planner/BrandSlotGrid';
import { QuotaMatrixUI } from './send-day-planner/QuotaMatrixUI';
import { GateStrip, allGatesPass } from './send-day-planner/GateStrip';
import { AuditDrawer } from './send-day-planner/AuditDrawer';
import {
  ALL_TARGET_ISPS,
  BRANDS,
  DEPLOY_ENDPOINT,
  ENGAGER_FAMILY,
  ENGAGER_FAMILY_COPY,
  GATE_C_REQUIRED_COMMIT,
  HEALTH_ENDPOINT,
  PAGE_VERSION,
  SEND_DAY_BANNED_CREATIVES_ENDPOINT,
  SEND_DAY_CREATIVE_RESOLVE_ENDPOINT,
  SEND_DAY_HOST_HEALTH_ATTEST_ENDPOINT,
  SEND_DAY_HOST_HEALTH_ENDPOINT,
  SEND_DAY_PREFLIGHT_BATCH_ENDPOINT,
  SEND_DAY_VOLUME_RECONCILIATION_ENDPOINT,
  SENDING_DOMAIN,
  SLOT_ORDER,
  TEMPLATE_CODE,
  WAVE_SCHEDULER_HEALTH_ENDPOINT,
  WELCOME_FAMILY_COPY,
  WELCOME_NL_FAMILY,
} from './send-day-planner/constants';
import { buildPayload, validateInvariants } from './send-day-planner/payloadBuilder';
import { datePrefixFromSendDate, scheduleFor, toMDTLabel } from './send-day-planner/schedule';
import { baselineMatrix, type QuotaMatrix } from './send-day-planner/quotaMatrix';
import type {
  BannedCreative, Brand, CellDraft, CellKey, GateState, Slot,
} from './send-day-planner/types';

// ─── State / reducer ────────────────────────────────────────────────────────

type CellsState = Record<CellKey, CellDraft>;
type CellsAction =
  | { type: 'init'; cells: CellsState }
  | { type: 'patch'; key: CellKey; patch: Partial<CellDraft> }
  | { type: 'replace'; key: CellKey; cell: CellDraft };

function cellsReducer(state: CellsState, action: CellsAction): CellsState {
  switch (action.type) {
    case 'init':
      return action.cells;
    case 'patch': {
      const existing = state[action.key];
      if (!existing) return state;
      return { ...state, [action.key]: { ...existing, ...action.patch } };
    }
    case 'replace':
      return { ...state, [action.key]: action.cell };
  }
}

const cellKey = (slot: Slot, brand: Brand): CellKey => `${slot}|${brand}`;

/** seedInitialCells — produce a fully-populated cells map for one send-date /
 *  matrix combination. Returned by useReducer's lazy initializer to avoid
 *  the empty-state typing problem. */
function seedInitialCells(sendDate: string, matrix: QuotaMatrix): CellsState {
  const out = {} as CellsState;
  const datePrefix = datePrefixFromSendDate(sendDate);
  for (const slot of SLOT_ORDER) {
    for (const brand of BRANDS) {
      const sched = scheduleFor(sendDate, slot, brand);
      const family = slot === 'welcome-newsletter'
        ? WELCOME_NL_FAMILY[brand]
        : (ENGAGER_FAMILY[slot]?.[brand] ?? '');
      const copy = slot === 'welcome-newsletter'
        ? WELCOME_FAMILY_COPY[family]
        : ENGAGER_FAMILY_COPY[family];
      const filename = family
        ? `${family}-${TEMPLATE_CODE[brand]}-newsletter-${datePrefix}.html`
        : '';
      const ispQuotas: Partial<Record<typeof ALL_TARGET_ISPS[number], number>> = {};
      if (slot === 'welcome-newsletter') {
        for (const isp of ALL_TARGET_ISPS) {
          const v = matrix[brand][isp] ?? 0;
          if (v > 0) ispQuotas[isp] = v;
        }
      }
      out[cellKey(slot, brand)] = {
        brand, slot, family, filename,
        htmlContent: '',
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
  }
  return out;
}

interface SendDayPlannerProps {
  onEditInWizard: (campaignId: string) => void;
  onCampaignPreparing?: (campaignId: string, name: string) => void;
}

// ─── Component ──────────────────────────────────────────────────────────────

export const SendDayPlanner: React.FC<SendDayPlannerProps> = ({
  onEditInWizard, onCampaignPreparing,
}) => {
  const { organization } = useAuth();
  const orgId = organization?.id ?? '00000000-0000-0000-0000-000000000001';
  const { addToast } = useToast();
  const toast = useMemo(() => ({
    success: (title: string, message?: string) => addToast({ type: 'success', title, message }),
    error: (title: string, message?: string) => addToast({ type: 'error', title, message }),
  }), [addToast]);

  // Default to TOMORROW (MDT) — the operator brief is always for the next send-day.
  const [sendDate, setSendDate] = useState(() => {
    const t = new Date();
    t.setUTCDate(t.getUTCDate() + 1);
    return t.toISOString().slice(0, 10);
  });

  const [matrix, setMatrix] = useState<QuotaMatrix>(() => baselineMatrix());
  const [cells, dispatchCells] = useReducer(
    cellsReducer,
    null,
    () => seedInitialCells(sendDate, matrix),
  );
  const [gates, setGates] = useState<GateState>({
    gateA: null, gateB: null, gateC: null, gateD: null, gateE: null, gateF: null,
  });
  const [gatesLoading, setGatesLoading] = useState(false);
  const [bannedList, setBannedList] = useState<BannedCreative[] | undefined>();
  const [auditOpen, setAuditOpen] = useState(false);
  const [deploying, setDeploying] = useState(false);

  // ── Re-seed cells whenever sendDate or matrix change ─────────────────────
  // Preserves status / campaignId / htmlContent / overrides for cells that
  // already had server-side state, to avoid throwing away an in-flight
  // deploy when the operator nudges a quota or shifts the date.
  useEffect(() => {
    const fresh = seedInitialCells(sendDate, matrix);
    const merged = { ...fresh };
    for (const k of Object.keys(fresh) as CellKey[]) {
      const prior = cells[k];
      if (prior && (prior.status !== 'draft' || prior.htmlContent)) {
        merged[k] = {
          ...fresh[k],
          htmlContent: prior.htmlContent || fresh[k].htmlContent,
          subject: prior.subject || fresh[k].subject,
          preheader: prior.preheader || fresh[k].preheader,
          status: prior.status,
          campaignId: prior.campaignId,
          error: prior.error,
        };
      }
    }
    dispatchCells({ type: 'init', cells: merged });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sendDate, matrix]);

  // ── Gate refresh ──────────────────────────────────────────────────────────
  const refreshGates = useCallback(async () => {
    setGatesLoading(true);
    try {
      const headers = { 'X-Organization-ID': orgId };
      const domains = BRANDS.map(b => SENDING_DOMAIN[b]);
      const [hostRes, healthRes, ramprRes, prefRes, banRes, sysRes] = await Promise.allSettled([
        fetch(SEND_DAY_HOST_HEALTH_ENDPOINT, { headers }).then(r => r.json()),
        fetch(WAVE_SCHEDULER_HEALTH_ENDPOINT, { headers }).then(r => r.json()),
        fetch(`${SEND_DAY_VOLUME_RECONCILIATION_ENDPOINT}?date=${sendDate}`, { headers }).then(r => r.json()),
        fetch(SEND_DAY_PREFLIGHT_BATCH_ENDPOINT, {
          method: 'POST', headers: { ...headers, 'Content-Type': 'application/json' },
          body: JSON.stringify({ domains }),
        }).then(r => r.json()),
        fetch(SEND_DAY_BANNED_CREATIVES_ENDPOINT, { headers }).then(r => r.json()),
        fetch(HEALTH_ENDPOINT).then(r => r.json()).catch(() => ({})),
      ]);
      const next: GateState = {
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
        gateE: gates.gateE,
        gateF: ramprRes.status === 'fulfilled' ? {
          passes: !!ramprRes.value.passes,
          today_planned: ramprRes.value.today_planned ?? 0,
          yesterday_planned: ramprRes.value.yesterday_planned ?? 0,
          target: ramprRes.value.target ?? 0,
          ramp_floor: ramprRes.value.ramp_floor ?? 0,
          gap: ramprRes.value.gap ?? 0,
          percent_to_target: ramprRes.value.percent_to_target ?? 0,
        } : null,
      };
      setGates(next);
      if (banRes.status === 'fulfilled' && Array.isArray(banRes.value.banned)) {
        setBannedList(banRes.value.banned);
      }
    } finally {
      setGatesLoading(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [orgId, sendDate]);

  useEffect(() => { refreshGates(); }, [refreshGates]);

  const onToggleAuditReviewed = useCallback(() => {
    setGates(prev => ({
      ...prev,
      gateE: { passes: !(prev.gateE?.passes ?? false), reviewed_at: new Date().toISOString(), reviewed_by: 'operator' },
    }));
  }, []);

  // ── Per-cell actions ──────────────────────────────────────────────────────
  const onReresolve = useCallback(async (cell: CellDraft) => {
    try {
      const r = await fetch(SEND_DAY_CREATIVE_RESOLVE_ENDPOINT, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-Organization-ID': orgId },
        body: JSON.stringify({
          slot: cell.slot, brand: cell.brand, family: cell.family,
          date: sendDate.replace(/-/g, ''),
          filename: cell.filename,
        }),
      });
      const data = await r.json();
      if (r.ok) {
        dispatchCells({ type: 'patch', key: cellKey(cell.slot, cell.brand), patch: {
          htmlContent: data.html_content ?? cell.htmlContent,
          subject: data.subject || cell.subject,
          preheader: data.preheader || cell.preheader,
          filename: data.filename || cell.filename,
          family: data.family || cell.family,
        } });
        toast.success(`Resolved ${cell.brand} · ${cell.slot}`);
      } else {
        toast.error(`Resolve ${cell.brand}/${cell.slot}: ${data.error ?? 'unknown'}`);
      }
    } catch (e) {
      toast.error(`Resolve ${cell.brand}/${cell.slot}: ${String(e)}`);
    }
  }, [orgId, sendDate, toast]);

  const onCancelCell = useCallback(async (cell: CellDraft) => {
    if (!cell.campaignId) return;
    try {
      const r = await fetch(`/api/mailing/campaigns/${cell.campaignId}/cancel`, {
        method: 'POST', headers: { 'X-Organization-ID': orgId },
      });
      if (r.ok) {
        dispatchCells({ type: 'patch', key: cellKey(cell.slot, cell.brand), patch: { status: 'cancelled' } });
        toast.success(`Cancelled ${cell.brand} · ${cell.slot}`);
      } else {
        toast.error(`Cancel failed (${r.status})`);
      }
    } catch (e) {
      toast.error(`Cancel: ${String(e)}`);
    }
  }, [orgId, toast]);

  const onEditInWizardLocal = useCallback((cell: CellDraft) => {
    if (cell.campaignId) onEditInWizard(cell.campaignId);
  }, [onEditInWizard]);

  // ── Deploy with bounded concurrency ───────────────────────────────────────
  const onDeploy = useCallback(async () => {
    if (!allGatesPass(gates)) {
      toast.error('All six pre-deploy gates must pass before deploying.');
      return;
    }
    const cellList = Object.values(cells);
    const datePrefix = datePrefixFromSendDate(sendDate);

    const invariantFails = cellList
      .map(c => ({ c, err: validateInvariants(buildPayload(c, { datePrefix })) }))
      .filter(x => x.err);
    if (invariantFails.length > 0) {
      toast.error(`Invariant violations on ${invariantFails.length} cells; aborting deploy.`);
      return;
    }
    const ok = window.confirm(`Deploy ${cellList.length} campaigns to ${DEPLOY_ENDPOINT}? This is LIVE.`);
    if (!ok) return;

    setDeploying(true);
    try {
      // Bounded concurrency = 4 (matches AudienceFinalizationWorker default).
      const queue = cellList.slice();
      const fire = async (cell: CellDraft) => {
        const k = cellKey(cell.slot, cell.brand);
        dispatchCells({ type: 'patch', key: k, patch: { status: 'deploying', error: undefined } });
        const payload = buildPayload(cell, { datePrefix });
        try {
          const r = await fetch(DEPLOY_ENDPOINT, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json', 'X-Organization-ID': orgId },
            body: JSON.stringify(payload),
          });
          const body = await r.json().catch(() => ({}));
          if (r.ok) {
            const cid = body.campaign_id ?? body.id;
            dispatchCells({ type: 'patch', key: k, patch: { status: 'accepted', campaignId: cid } });
            if (cid && onCampaignPreparing) onCampaignPreparing(cid, payload.name);
          } else {
            dispatchCells({ type: 'patch', key: k, patch: { status: 'failed', error: body.error ?? `HTTP ${r.status}` } });
          }
        } catch (e) {
          dispatchCells({ type: 'patch', key: k, patch: { status: 'failed', error: String(e) } });
        }
      };
      const workers = Array.from({ length: 4 }, async () => {
        while (queue.length) {
          const c = queue.shift();
          if (c) await fire(c);
        }
      });
      await Promise.all(workers);
      toast.success(`Deploy complete: ${cellList.length} campaigns dispatched.`);
    } finally {
      setDeploying(false);
    }
  }, [cells, gates, onCampaignPreparing, orgId, sendDate, toast]);

  // Surface attest action for Gate A.
  const onAttestGateA = useCallback(async (state: 'pass' | 'fail') => {
    for (const key of ['server_a', 'server_b']) {
      await fetch(SEND_DAY_HOST_HEALTH_ATTEST_ENDPOINT, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-Organization-ID': orgId },
        body: JSON.stringify({ server_key: key, state, message: 'attested via canvas', updated_by: 'operator' }),
      });
    }
    await refreshGates();
  }, [orgId, refreshGates]);

  // ── Render ────────────────────────────────────────────────────────────────
  const canDeploy = allGatesPass(gates) && !deploying;
  const allCells = useMemo(() => Object.values(cells), [cells]);

  const sampleCell = allCells[0];
  return (
    <div style={{ padding: 18, color: 'rgba(220,235,250,0.92)' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 16, marginBottom: 16, flexWrap: 'wrap' }}>
        <h2 style={{ margin: 0, fontSize: 18, color: '#00e5ff' }}>Send-Day Planner · Mature Brands</h2>
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
        <button onClick={() => setMatrix(baselineMatrix())} style={btnSecondary}>Reset matrix to May-12 baseline</button>
        <button onClick={() => setAuditOpen(true)} style={btnSecondary}>Open audit JSON</button>
        <button onClick={onDeploy} disabled={!canDeploy} style={canDeploy ? btnDeploy : btnDeployDisabled}
          title={canDeploy ? 'Deploy 16 campaigns' : 'All six gates must pass'}>
          {deploying ? 'Deploying…' : `Deploy ${allCells.length} campaigns`}
        </button>
        {sampleCell && (
          <div style={{ marginLeft: 'auto', fontSize: 11, color: 'rgba(180,210,240,0.6)' }}>
            anchor sample (DB eng-w1): {toMDTLabel(sampleCell.startAtUTC)} MDT
          </div>
        )}
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
            Gate A is operator-attested in v1. {' '}
            <button onClick={() => onAttestGateA('pass')} style={attestBtn}>I confirmed PMTA Server A + B via SSH</button>
          </div>
        )}
      </div>
      <div style={{ marginBottom: 18 }}>
        <BrandSlotGrid
          cells={cells}
          bannedList={bannedList}
          onEdit={onEditInWizardLocal}
          onCancel={onCancelCell}
          onReresolve={onReresolve}
          onOpenAudit={() => setAuditOpen(true)}
        />
      </div>
      <div style={{ marginBottom: 18, padding: 14, background: 'rgba(13,21,38,0.6)', border: '1px solid rgba(0,200,255,0.1)', borderRadius: 8 }}>
        <QuotaMatrixUI matrix={matrix} onChange={setMatrix} />
      </div>
      <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.45)', textAlign: 'right' }}>
        SendDayPlanner v{PAGE_VERSION}
      </div>
      <AuditDrawer
        open={auditOpen}
        onClose={() => setAuditOpen(false)}
        cells={allCells}
        sendDate={sendDate}
        organizationId={orgId}
      />
    </div>
  );
};

// Default + named export so MailingPortal can import either way.
export default SendDayPlanner;

const btnPrimary: React.CSSProperties = {
  background: 'rgba(0,176,255,0.15)', border: '1px solid rgba(0,176,255,0.5)',
  color: '#00e5ff', padding: '6px 12px', borderRadius: 6, fontSize: 12, fontWeight: 600, cursor: 'pointer',
};
const btnSecondary: React.CSSProperties = {
  background: 'rgba(180,180,180,0.10)', border: '1px solid rgba(180,180,180,0.4)',
  color: '#cbd5f5', padding: '6px 12px', borderRadius: 6, fontSize: 12, fontWeight: 600, cursor: 'pointer',
};
const btnDeploy: React.CSSProperties = {
  background: 'linear-gradient(135deg, #6366f1, #8b5cf6)', border: 'none',
  color: '#fff', padding: '8px 18px', borderRadius: 6, fontSize: 13, fontWeight: 700, cursor: 'pointer',
};
const btnDeployDisabled: React.CSSProperties = {
  ...btnDeploy, background: 'rgba(120,120,120,0.25)', cursor: 'not-allowed', opacity: 0.55,
};
const attestBtn: React.CSSProperties = {
  background: 'rgba(0,184,148,0.12)', border: '1px solid rgba(0,184,148,0.5)',
  color: '#00b894', padding: '4px 10px', borderRadius: 5, fontSize: 11, fontWeight: 600, cursor: 'pointer',
};
