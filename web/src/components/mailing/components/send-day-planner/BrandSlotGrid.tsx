import React from 'react';
import { BRANDS, BRAND_LABEL, SLOT_NAME_FRAGMENT, SLOT_ORDER } from './constants';
import { resolveQuotas } from './payloadBuilder';
import { isBanned } from './bannedCreative';
import { toMDTLabel } from './schedule';
import type { BannedCreative, Brand, CellDraft, CellKey, Slot } from './types';

interface BrandSlotGridProps {
  cells: Record<CellKey, CellDraft>;
  bannedList?: BannedCreative[];
  onEdit: (cell: CellDraft) => void;
  onCancel: (cell: CellDraft) => void;
  onReresolve: (cell: CellDraft) => void;
  onOpenAudit: (cell: CellDraft) => void;
}

const cellKey = (slot: Slot, brand: Brand): CellKey => `${slot}|${brand}`;

const StatusBadge: React.FC<{ status: CellDraft['status'] }> = ({ status }) => {
  const palette: Record<CellDraft['status'], { bg: string; bd: string; fg: string; label: string }> = {
    draft:      { bg: 'rgba(120,120,120,0.12)', bd: 'rgba(120,120,120,0.4)', fg: '#9ca3af',   label: 'DRAFT' },
    deploying:  { bg: 'rgba(0,176,255,0.12)',   bd: 'rgba(0,176,255,0.45)', fg: '#00b0ff',   label: 'DEPLOYING' },
    accepted:   { bg: 'rgba(0,184,148,0.12)',   bd: 'rgba(0,184,148,0.45)', fg: '#00b894',   label: 'ACCEPTED' },
    failed:     { bg: 'rgba(233,69,96,0.12)',   bd: 'rgba(233,69,96,0.5)',  fg: '#e94560',   label: 'FAILED' },
    cancelled:  { bg: 'rgba(180,180,180,0.10)', bd: 'rgba(180,180,180,0.4)',fg: '#bdbdbd',   label: 'CANCELLED' },
  };
  const p = palette[status];
  return (
    <span style={{
      display: 'inline-block', padding: '2px 8px', borderRadius: 999,
      background: p.bg, border: `1px solid ${p.bd}`, color: p.fg,
      fontSize: 10, fontWeight: 700, letterSpacing: 0.5,
    }}>{p.label}</span>
  );
};

const Cell: React.FC<{
  draft: CellDraft;
  banned: boolean;
  onEdit: () => void;
  onCancel: () => void;
  onReresolve: () => void;
  onOpenAudit: () => void;
}> = ({ draft, banned, onEdit, onCancel, onReresolve, onOpenAudit }) => {
  const { quotas } = resolveQuotas(draft);
  const total = quotas.reduce((s, q) => s + q.volume, 0);
  return (
    <div style={{
      display: 'flex', flexDirection: 'column', gap: 6,
      padding: 12, borderRadius: 8,
      background: banned ? 'rgba(233,69,96,0.06)' : 'rgba(0,229,255,0.04)',
      border: `1px solid ${banned ? 'rgba(233,69,96,0.4)' : 'rgba(0,200,255,0.15)'}`,
      minHeight: 130,
    }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
        <StatusBadge status={draft.status} />
        {banned && <span style={{ fontSize: 10, color: '#e94560', fontWeight: 700 }}>BANNED CREATIVE</span>}
      </div>
      <div style={{ fontSize: 13, fontWeight: 600, color: 'rgba(220,235,250,0.95)' }}>{draft.family || '— pick family —'}</div>
      <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)' }} title={draft.filename}>{draft.filename || '(no creative resolved)'}</div>
      <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 11, color: 'rgba(180,210,240,0.7)' }}>
        <span>{toMDTLabel(draft.startAtUTC)} MDT · {draft.windowHours}h</span>
        <span>{quotas.length} ISP</span>
      </div>
      <div style={{ fontSize: 12, color: '#00e5ff', fontWeight: 600 }}>{total.toLocaleString()} planned</div>
      {draft.error && <div style={{ fontSize: 11, color: '#e94560' }}>{draft.error}</div>}
      <div style={{ display: 'flex', gap: 6, marginTop: 'auto' }}>
        <button onClick={onOpenAudit} style={btnStyle('rgba(0,176,255,0.15)', '#00b0ff')}>Audit</button>
        <button onClick={onReresolve} style={btnStyle('rgba(0,184,148,0.15)', '#00b894')}>Re-resolve</button>
        <button onClick={onEdit} disabled={!draft.campaignId} style={btnStyle('rgba(180,180,180,0.12)', '#cbd5f5', !draft.campaignId)} title={draft.campaignId ? 'Open in Campaign Wizard' : 'Edit available after deploy'}>Edit</button>
        <button onClick={onCancel} disabled={draft.status !== 'accepted'} style={btnStyle('rgba(233,69,96,0.12)', '#e94560', draft.status !== 'accepted')}>Cancel</button>
      </div>
    </div>
  );
};

function btnStyle(bg: string, fg: string, disabled = false): React.CSSProperties {
  return {
    background: bg, color: fg, border: `1px solid ${fg}33`,
    padding: '4px 8px', borderRadius: 6, fontSize: 10, fontWeight: 600,
    cursor: disabled ? 'not-allowed' : 'pointer', opacity: disabled ? 0.4 : 1,
  };
}

export const BrandSlotGrid: React.FC<BrandSlotGridProps> = ({
  cells, bannedList, onEdit, onCancel, onReresolve, onOpenAudit,
}) => {
  return (
    <div style={{ overflowX: 'auto' }}>
      <div style={{
        display: 'grid',
        gridTemplateColumns: `120px repeat(${BRANDS.length}, minmax(220px, 1fr))`,
        gap: 8,
      }}>
        <div />
        {BRANDS.map(b => (
          <div key={b} style={{
            padding: '8px 12px', textAlign: 'center', fontWeight: 700, fontSize: 13,
            color: 'rgba(220,235,250,0.92)', background: 'rgba(0,176,255,0.08)',
            borderRadius: 6,
          }}>{BRAND_LABEL[b]}</div>
        ))}
        {SLOT_ORDER.map(slot => (
          <React.Fragment key={slot}>
            <div style={{
              padding: '8px 4px', textAlign: 'right', fontSize: 11, fontWeight: 700,
              color: 'rgba(180,210,240,0.75)', textTransform: 'uppercase', letterSpacing: 0.4,
              alignSelf: 'center',
            }}>{SLOT_NAME_FRAGMENT[slot]}</div>
            {BRANDS.map(brand => {
              const draft = cells[cellKey(slot, brand)];
              if (!draft) return <div key={brand} />;
              return (
                <Cell
                  key={brand}
                  draft={draft}
                  banned={isBanned(draft.filename, bannedList)}
                  onEdit={() => onEdit(draft)}
                  onCancel={() => onCancel(draft)}
                  onReresolve={() => onReresolve(draft)}
                  onOpenAudit={() => onOpenAudit(draft)}
                />
              );
            })}
          </React.Fragment>
        ))}
      </div>
    </div>
  );
};
