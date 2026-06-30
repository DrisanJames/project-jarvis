import React from 'react';
import { colors } from '../../shared/theme';
import { Pill } from '../../shared/ui';
import type { GateA, GateState } from './types';

const GATE_LABELS: Record<keyof GateState, string> = {
  gateA: 'A · Sending Server Stability',
  gateB: 'B · Send Batch Dispatcher',
  gateC: 'C · Delivery Build Check',
  gateD: 'D · Sending Profiles',
  gateE: 'E · Audit Reviewed',
  gateF: 'F · Volume Ramp',
};

const GATE_RULE_HINT: Record<keyof GateState, string> = {
  gateA: 'Both sending servers attested healthy (operator confirmation — ECS cannot SSH the PMTA boxes)',
  gateB: 'Orphaned "zombie" planned waves (parent campaign already cancelled/failed/sent) + expired waves < 50',
  gateC: 'production build includes the latest delivery-retry fix',
  gateD: 'every domain has an active sending profile and at least one ready IP',
  gateE: 'operator reviewed audit JSON for every cell',
  gateF: 'today_planned ≥ yesterday_planned × 1.20 × 0.95 (planned = max of committed recipients + staged draft ISP quotas)',
};

// A gate renders in one of four visual states. Only a genuine failure is red;
// an un-attested Gate A (servers "unknown", no real failure signal) is amber
// "needs attestation" rather than a scary red FAIL.
type GateStatus = 'pass' | 'fail' | 'attention' | 'idle';

const STATUS_COLOR: Record<GateStatus, string> = {
  pass: colors.success,
  fail: colors.danger,
  attention: colors.warning,
  idle: colors.idle,
};

const STATUS_LABEL: Record<GateStatus, string> = {
  pass: 'pass',
  fail: 'fail',
  attention: 'needs attestation',
  idle: 'loading',
};

function resolveGate(id: keyof GateState, state: GateState[keyof GateState]): GateStatus {
  if (state === null || state === undefined) return 'idle';
  const passes = (state as { passes: boolean }).passes;
  if (passes) return 'pass';
  // Gate A is operator-attested. Un-attested servers come back as "unknown"
  // (no live telemetry) — that is NOT a failure, it is "awaiting confirmation".
  // Only a server explicitly attested "fail" is a real red failure.
  if (id === 'gateA') {
    const servers = (state as GateA).servers ?? {};
    const anyFailure = Object.values(servers).some(
      v => (v?.state ?? '').toLowerCase() === 'fail',
    );
    return anyFailure ? 'fail' : 'attention';
  }
  return 'fail';
}

interface GateChipProps {
  id: keyof GateState;
  state: GateState[keyof GateState];
  detail?: string;
  onToggleE?: () => void;
}

const GateChip: React.FC<GateChipProps> = ({ id, state, detail, onToggleE }) => {
  const status = resolveGate(id, state);
  const accent = STATUS_COLOR[status];
  const onClick = id === 'gateE' && onToggleE ? onToggleE : undefined;
  return (
    <div
      title={`${GATE_RULE_HINT[id]}${detail ? ` — ${detail}` : ''}`}
      onClick={onClick}
      style={{
        flex: '1 1 0',
        minWidth: 168,
        display: 'flex',
        flexDirection: 'column',
        gap: 6,
        padding: '12px 14px',
        background: colors.panelBg,
        border: `1px solid ${colors.panelBorder}`,
        borderLeft: `4px solid ${accent}`,
        borderRadius: 10,
        color: colors.text,
        cursor: onClick ? 'pointer' : 'default',
      }}
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
        <span style={{ fontSize: 12, fontWeight: 600, color: colors.heading }}>{GATE_LABELS[id]}</span>
        <span style={{ marginLeft: 'auto' }}>
          <Pill color={accent}>{STATUS_LABEL[status]}</Pill>
        </span>
      </div>
      {detail && <div style={{ fontSize: 11, color: colors.textMuted }}>{detail}</div>}
    </div>
  );
};

interface GateStripProps {
  state: GateState;
  onToggleAuditReviewed: () => void;
  onRefresh: () => void;
  loading: boolean;
}

export const GateStrip: React.FC<GateStripProps> = ({ state, onToggleAuditReviewed, onRefresh, loading }) => {
  // ── Detail strings per gate ───────────────────────────────────────────────
  const dA = (() => {
    if (!state.gateA) return undefined;
    const status = resolveGate('gateA', state.gateA);
    if (status === 'pass') {
      return Object.keys(state.gateA.servers)
        .map(k => `${k.replace('server_', 'server ')} healthy`)
        .join(' · ') || 'both servers attested healthy';
    }
    if (status === 'fail') {
      const failed = Object.entries(state.gateA.servers)
        .filter(([, v]) => (v?.state ?? '').toLowerCase() === 'fail')
        .map(([k]) => k.replace('server_', 'server '));
      return `failure reported on ${failed.join(', ') || 'a sending server'} — do not send until resolved`;
    }
    // attention / un-attested
    return 'awaiting operator confirmation — attest sending servers A + B below';
  })();

  const dB = state.gateB
    ? `zombie waves=${state.gateB.zombies} · expired=${state.gateB.expired} · due now=${state.gateB.due_now}`
    : undefined;
  const dC = state.gateC ? `build=${state.gateC.git_sha?.slice(0, 7) || '??'}` : undefined;
  const dD = state.gateD
    ? `${Object.values(state.gateD.results).filter(r => r.ok).length}/${Object.keys(state.gateD.results).length} domains pass`
    : undefined;
  const dE = state.gateE
    ? state.gateE.passes ? `reviewed by ${state.gateE.reviewed_by ?? 'operator'}` : 'click to mark reviewed'
    : 'click to mark reviewed';
  const dF = state.gateF
    ? `${state.gateF.today_planned.toLocaleString()} planned vs ${state.gateF.target.toLocaleString()} target (${(state.gateF.percent_to_target * 100).toFixed(0)}%) · incl. staged drafts`
    : undefined;

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
        <h3
          style={{
            margin: 0,
            fontSize: 13,
            fontWeight: 600,
            color: colors.heading,
            textTransform: 'uppercase',
            letterSpacing: 0.6,
          }}
        >
          Pre-Deploy Gates
        </h3>
        <button
          onClick={onRefresh}
          disabled={loading}
          style={{
            background: colors.hover,
            border: `1px solid ${colors.panelBorderStrong}`,
            color: colors.indigo200,
            padding: '4px 12px',
            borderRadius: 8,
            cursor: loading ? 'wait' : 'pointer',
            fontSize: 11,
            fontWeight: 600,
          }}
        >
          {loading ? 'Refreshing…' : 'Refresh'}
        </button>
      </div>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 10 }}>
        <GateChip id="gateA" state={state.gateA} detail={dA} />
        <GateChip id="gateB" state={state.gateB} detail={dB} />
        <GateChip id="gateC" state={state.gateC} detail={dC} />
        <GateChip id="gateD" state={state.gateD} detail={dD} />
        <GateChip id="gateE" state={state.gateE} detail={dE} onToggleE={onToggleAuditReviewed} />
        <GateChip id="gateF" state={state.gateF} detail={dF} />
      </div>
    </div>
  );
};

/** allGatesPass — Deploy button must verify this is true before firing. */
export function allGatesPass(state: GateState): boolean {
  return Boolean(
    state.gateA?.passes
    && state.gateB?.passes
    && state.gateC?.passes
    && state.gateD?.passes
    && state.gateE?.passes
    && state.gateF?.passes,
  );
}
