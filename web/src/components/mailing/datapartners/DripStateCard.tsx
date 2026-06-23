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
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', marginBottom: 8 }}>
        <h3 style={{ margin: 0, color: '#dbeafe', fontSize: 15 }}>
          <FontAwesomeIcon icon={faRoute} style={{ marginRight: 6, color: '#6366f1' }} />
          {verticalLabel}
        </h3>
        <span style={badge}>{vertical}</span>
      </div>

      <div style={statGrid}>
        <Stat label="Ready" value={readyQueue.toLocaleString()} accent="#10b981" />
        <Stat label="Pending verification" value={pendingEO.toLocaleString()} accent="#a78bfa" />
        <Stat label="Mailed" value={mailedTotal.toLocaleString()} accent="#6366f1" />
      </div>

      <div style={divider} />

      <div style={{ display: 'flex', flexDirection: 'column', gap: 6, fontSize: 12, color: 'rgba(180,210,240,0.7)' }}>
        <div>
          <FontAwesomeIcon icon={faClock} style={{ marginRight: 6 }} />
          Last send batch: {lastWaveAt ? `${new Date(lastWaveAt).toLocaleTimeString()} (${lastWaveBrand?.toUpperCase()}, ${lastWaveSize ?? 0} sent)` : '—'}
        </div>
        <div>
          <FontAwesomeIcon icon={faPaperPlane} style={{ marginRight: 6 }} />
          Next brand in rotation: <b style={{ color: '#cbd5f5' }}>{nextBrand.toUpperCase()}</b>
        </div>
      </div>
    </div>
  );
};

const Stat: React.FC<{ label: string; value: string; accent?: string }> = ({ label, value, accent }) => (
  <div style={{ background: 'rgba(0,0,0,0.2)', padding: 8, borderRadius: 4, textAlign: 'center' }}>
    <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.65)', textTransform: 'uppercase', letterSpacing: 0.5 }}>{label}</div>
    <div style={{ fontSize: 18, fontWeight: 700, marginTop: 2, color: accent ?? '#dbeafe' }}>{value}</div>
  </div>
);

const card: React.CSSProperties = {
  background: 'linear-gradient(135deg, rgba(15,30,60,0.65) 0%, rgba(20,40,80,0.5) 100%)',
  border: '1px solid rgba(120,150,200,0.18)',
  borderRadius: 10, padding: 16,
};
const statGrid: React.CSSProperties = { display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 6 };
const divider: React.CSSProperties = { height: 1, background: 'rgba(120,150,200,0.12)', margin: '12px 0' };
const badge: React.CSSProperties = {
  fontSize: 10, padding: '2px 8px', borderRadius: 12,
  background: 'rgba(99,102,241,0.18)', color: '#a5b4fc',
  fontFamily: 'ui-monospace, monospace',
};
