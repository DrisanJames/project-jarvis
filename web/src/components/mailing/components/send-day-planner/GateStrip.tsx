import React from 'react';
import type { GateState } from './types';

const GATE_LABELS: Record<keyof GateState, string> = {
  gateA: 'A · Sending Server Stability',
  gateB: 'B · Send Batch Dispatcher',
  gateC: 'C · Delivery Build Check',
  gateD: 'D · Sending Profiles',
  gateE: 'E · Audit Reviewed',
  gateF: 'F · Volume Ramp',
};

const GATE_RULE_HINT: Record<keyof GateState, string> = {
  gateA: 'Sending servers up ≥30m, no recent crashes, forwarder verified',
  gateB: 'stuck + expired send batches < 50',
  gateC: 'production build includes the latest delivery-retry fix',
  gateD: 'every domain has an active sending profile and at least one ready IP',
  gateE: 'operator reviewed audit JSON for every cell',
  gateF: 'today_planned ≥ yesterday_planned × 1.20 × 0.95',
};

interface GateChipProps {
  id: keyof GateState;
  state: GateState[keyof GateState];
  detail?: string;
  onToggleE?: () => void;
}

const GateChip: React.FC<GateChipProps> = ({ id, state, detail, onToggleE }) => {
  const passes = state ? (state as { passes: boolean }).passes : false;
  const known = state !== null && state !== undefined;

  let bg = 'rgba(120,120,120,0.12)';
  let bd = 'rgba(120,120,120,0.4)';
  let dot = '#9ca3af';
  let label = 'unknown';
  if (known) {
    if (passes) {
      bg = 'rgba(0, 184, 148, 0.10)';
      bd = 'rgba(0, 184, 148, 0.45)';
      dot = '#00b894';
      label = 'pass';
    } else {
      bg = 'rgba(233, 69, 96, 0.12)';
      bd = 'rgba(233, 69, 96, 0.5)';
      dot = '#e94560';
      label = 'fail';
    }
  }
  const onClick = id === 'gateE' && onToggleE ? onToggleE : undefined;
  return (
    <div
      title={`${GATE_RULE_HINT[id]}${detail ? ` — ${detail}` : ''}`}
      onClick={onClick}
      style={{
        flex: '1 1 0',
        minWidth: 150,
        display: 'flex', flexDirection: 'column', gap: 4,
        padding: '10px 12px',
        background: bg, border: `1px solid ${bd}`, borderRadius: 8,
        fontSize: 12, color: 'rgba(220,235,250,0.92)',
        cursor: onClick ? 'pointer' : 'default',
      }}
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: 6, fontWeight: 600 }}>
        <span style={{ width: 8, height: 8, borderRadius: '50%', background: dot }} />
        <span>{GATE_LABELS[id]}</span>
        <span style={{ marginLeft: 'auto', textTransform: 'uppercase', fontSize: 10, opacity: 0.7 }}>{label}</span>
      </div>
      {detail && <div style={{ fontSize: 11, opacity: 0.75 }}>{detail}</div>}
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
  // Detail strings per gate.
  const dA = state.gateA
    ? `${Object.entries(state.gateA.servers).map(([k, v]) => `${k}=${v.state}`).join(' · ')}`
    : undefined;
  const dB = state.gateB
    ? `stuck=${state.gateB.zombies} expired=${state.gateB.expired} due now=${state.gateB.due_now}`
    : undefined;
  const dC = state.gateC ? `build=${state.gateC.git_sha?.slice(0, 7) || '??'}` : undefined;
  const dD = state.gateD
    ? `${Object.values(state.gateD.results).filter(r => r.ok).length}/${Object.keys(state.gateD.results).length} domains pass`
    : undefined;
  const dE = state.gateE
    ? state.gateE.passes ? `reviewed by ${state.gateE.reviewed_by ?? 'operator'}` : 'click to mark reviewed'
    : 'click to mark reviewed';
  const dF = state.gateF
    ? `${state.gateF.today_planned.toLocaleString()} / ${state.gateF.target.toLocaleString()} (${(state.gateF.percent_to_target * 100).toFixed(0)}%)`
    : undefined;

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
        <h3 style={{ margin: 0, fontSize: 13, color: 'rgba(220,235,250,0.92)', textTransform: 'uppercase', letterSpacing: 0.6 }}>
          Pre-Deploy Gates
        </h3>
        <button onClick={onRefresh} disabled={loading} style={{
          background: 'rgba(0,176,255,0.15)', border: '1px solid rgba(0,176,255,0.4)',
          color: '#00e5ff', padding: '4px 10px', borderRadius: 6, cursor: loading ? 'wait' : 'pointer',
          fontSize: 11, fontWeight: 600,
        }}>{loading ? 'Refreshing…' : 'Refresh'}</button>
      </div>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
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
