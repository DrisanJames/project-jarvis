import React from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faRoute, faClock, faPaperPlane } from '@fortawesome/free-solid-svg-icons';

const BRANDS = ['db', 'ht', 'mh', 'qf'];

interface Props {
  vertical: string;
  verticalLabel: string;
  readyQueue: number;
  pendingEO: number;
  mailedTotal: number;
  lastWaveAt: string | null;
  lastWaveBrand: string | null;
  lastWaveSize: number | null;
  nextBrandIndex: number;
  onChanged: () => void;
}

export const DripStateCard: React.FC<Props> = ({
  vertical, verticalLabel, readyQueue, pendingEO, mailedTotal,
  lastWaveAt, lastWaveBrand, lastWaveSize, nextBrandIndex,
  onChanged,
}) => {
  const nextBrand = BRANDS[nextBrandIndex % BRANDS.length];
  void onChanged; // future: trigger-now button

  return (
    <div style={card}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: 8, marginBottom: 8, minWidth: 0 }}>
        <h3 style={{ margin: 0, color: 'var(--j-text)', fontSize: 15, minWidth: 0, whiteSpace: 'normal', overflowWrap: 'anywhere' }}>
          <FontAwesomeIcon icon={faRoute} style={{ marginRight: 6, color: 'var(--j-violet-strong)' }} />
          {verticalLabel}
        </h3>
        <span style={badge}>{vertical}</span>
      </div>

      <div style={statGrid}>
        <Stat label="Ready" value={readyQueue.toLocaleString()} accent="var(--j-success)" />
        <Stat label="Pending verification" value={pendingEO.toLocaleString()} accent="var(--j-violet)" />
        <Stat label="Mailed" value={mailedTotal.toLocaleString()} accent="var(--j-violet-strong)" />
      </div>

      <div style={divider} />

      <div style={{ display: 'flex', flexDirection: 'column', gap: 6, fontSize: 12, color: 'var(--j-text-2)' }}>
        <div>
          <FontAwesomeIcon icon={faClock} style={{ marginRight: 6 }} />
          Last send batch: {lastWaveAt ? `${new Date(lastWaveAt).toLocaleTimeString()} (${lastWaveBrand?.toUpperCase()}, ${lastWaveSize ?? 0} sent)` : '—'}
        </div>
        <div>
          <FontAwesomeIcon icon={faPaperPlane} style={{ marginRight: 6 }} />
          Next brand in rotation: <b style={{ color: 'var(--j-text-2)' }}>{nextBrand.toUpperCase()}</b>
        </div>
      </div>
    </div>
  );
};

const Stat: React.FC<{ label: string; value: string; accent?: string }> = ({ label, value, accent }) => (
  <div style={{ background: 'color-mix(in srgb, var(--j-shade) 50%, transparent)', padding: 8, borderRadius: 4, textAlign: 'center' }}>
    <div style={{ fontSize: 10, color: 'var(--j-text-muted)', textTransform: 'uppercase', letterSpacing: 0.5 }}>{label}</div>
    <div style={{ fontSize: 18, fontWeight: 700, marginTop: 2, color: accent ?? 'var(--j-text)' }}>{value}</div>
  </div>
);

const card: React.CSSProperties = {
  background: 'linear-gradient(135deg, color-mix(in srgb, var(--j-shade) 65%, transparent) 0%, color-mix(in srgb, var(--j-shade) 50%, transparent) 100%)',
  border: '1px solid color-mix(in srgb, var(--j-text-muted) 18%, transparent)',
  borderRadius: 10, padding: 16, minWidth: 0,
};
const statGrid: React.CSSProperties = { display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 6 };
const divider: React.CSSProperties = { height: 1, background: 'color-mix(in srgb, var(--j-text-muted) 12%, transparent)', margin: '12px 0' };
const badge: React.CSSProperties = {
  fontSize: 10, padding: '2px 8px', borderRadius: 12,
  background: 'color-mix(in srgb, var(--j-violet-strong) 18%, transparent)', color: 'var(--j-violet)',
  fontFamily: 'ui-monospace, monospace',
  whiteSpace: 'nowrap', flexShrink: 0,
};
