import React, { useState } from 'react';
import { ALL_TARGET_ISPS, BRANDS, BRAND_LABEL } from './constants';
import {
  applyToAll, applyToCell, applyToColumn, applyToRow,
  columnTotal, grandTotal, rowTotal,
} from './quotaMatrix';
import type { QuotaMatrix } from './quotaMatrix';
import type { QuotaTransform } from './types';

interface QuotaMatrixUIProps {
  matrix: QuotaMatrix;
  onChange: (next: QuotaMatrix) => void;
}

const TRANSFORMS: { id: string; label: string; transform: QuotaTransform }[] = [
  { id: 'hold',     label: 'Hold',  transform: { kind: 'hold', value: 0 } },
  { id: 'pct20',    label: '+20%',  transform: { kind: 'scale_pct', value: 0.20 } },
  { id: 'pct10',    label: '+10%',  transform: { kind: 'scale_pct', value: 0.10 } },
  { id: 'add50',    label: '+50',   transform: { kind: 'add_abs', value: 50 } },
  { id: 'add100',   label: '+100',  transform: { kind: 'add_abs', value: 100 } },
  { id: 'zero',     label: 'Zero',  transform: { kind: 'zero', value: 0 } },
];

export const QuotaMatrixUI: React.FC<QuotaMatrixUIProps> = ({ matrix, onChange }) => {
  const [activeXf, setActiveXf] = useState<QuotaTransform>(TRANSFORMS[0].transform);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
        <h3 style={{
          margin: 0, fontSize: 13, color: 'rgba(220,235,250,0.92)',
          textTransform: 'uppercase', letterSpacing: 0.6,
        }}>Welcome Quota Matrix · 4 brands × 11 ISPs</h3>
        <div style={{ display: 'flex', gap: 6 }}>
          {TRANSFORMS.map(t => (
            <button
              key={t.id}
              onClick={() => setActiveXf(t.transform)}
              style={{
                background: activeXf === t.transform ? 'rgba(0,229,255,0.20)' : 'rgba(0,176,255,0.08)',
                border: `1px solid ${activeXf === t.transform ? '#00e5ff' : 'rgba(0,176,255,0.3)'}`,
                color: activeXf === t.transform ? '#00e5ff' : 'rgba(220,235,250,0.85)',
                padding: '4px 10px', borderRadius: 6, fontSize: 11, fontWeight: 600, cursor: 'pointer',
              }}
            >{t.label}</button>
          ))}
        </div>
        <button onClick={() => onChange(applyToAll(matrix, activeXf))} style={pillBtn}>Apply to all</button>
      </div>
      <div style={{ overflowX: 'auto' }}>
        <table style={{
          borderCollapse: 'separate', borderSpacing: 0, width: '100%',
          fontSize: 11, color: 'rgba(220,235,250,0.92)',
        }}>
          <thead>
            <tr>
              <th style={th}>ISP</th>
              {BRANDS.map(b => (
                <th key={b} style={{ ...th, textAlign: 'center' }}>{BRAND_LABEL[b]}</th>
              ))}
              <th style={{ ...th, textAlign: 'center' }}>Total</th>
              <th style={{ ...th, textAlign: 'right' }}>Apply→Row</th>
            </tr>
          </thead>
          <tbody>
            {ALL_TARGET_ISPS.map(isp => (
              <tr key={isp}>
                <td style={{ ...td, textTransform: 'uppercase', fontWeight: 600, color: '#00e5ff' }}>{isp}</td>
                {BRANDS.map(brand => (
                  <td key={brand} style={{ ...td, textAlign: 'center' }}>
                    <input
                      type="number"
                      value={matrix[brand][isp] ?? 0}
                      min={0}
                      onChange={e => onChange(applyToCell(matrix, brand, isp, { kind: 'set_abs', value: parseInt(e.target.value || '0', 10) }))}
                      style={cellInput((matrix[brand][isp] ?? 0) === 0)}
                    />
                  </td>
                ))}
                <td style={{ ...td, textAlign: 'center', color: 'rgba(180,210,240,0.85)' }}>{columnTotal(matrix, isp).toLocaleString()}</td>
                <td style={{ ...td, textAlign: 'right' }}>
                  <button onClick={() => onChange(applyToColumn(matrix, isp, activeXf))} style={pillBtn}>Apply</button>
                </td>
              </tr>
            ))}
            <tr style={{ background: 'rgba(0,176,255,0.05)' }}>
              <td style={{ ...td, fontWeight: 700 }}>Total</td>
              {BRANDS.map(brand => (
                <td key={brand} style={{ ...td, textAlign: 'center', color: '#00e5ff', fontWeight: 700 }}>
                  {rowTotal(matrix, brand).toLocaleString()}
                </td>
              ))}
              <td style={{ ...td, textAlign: 'center', color: '#00e5ff', fontWeight: 700 }}>{grandTotal(matrix).toLocaleString()}</td>
              <td style={{ ...td, textAlign: 'right' }}>
                {BRANDS.map(b => (
                  <button key={b} onClick={() => onChange(applyToRow(matrix, b, activeXf))} style={{ ...pillBtn, marginLeft: 4 }}>Row {b}</button>
                ))}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)' }}>
        Engager rows are read-only and use the auto-cap formula <code style={{ color: '#00e5ff' }}>max(2000, expected×1.5)</code> per brand.
      </div>
    </div>
  );
};

const th: React.CSSProperties = {
  padding: '6px 8px', borderBottom: '1px solid rgba(0,200,255,0.15)',
  textAlign: 'left', fontSize: 10, textTransform: 'uppercase', letterSpacing: 0.5,
  color: 'rgba(180,210,240,0.7)',
};
const td: React.CSSProperties = {
  padding: '4px 6px', borderBottom: '1px solid rgba(0,200,255,0.06)',
};
const pillBtn: React.CSSProperties = {
  background: 'rgba(0,176,255,0.12)', border: '1px solid rgba(0,176,255,0.4)',
  color: '#00b0ff', padding: '3px 8px', borderRadius: 5, fontSize: 10, fontWeight: 600, cursor: 'pointer',
};

function cellInput(isZero: boolean): React.CSSProperties {
  return {
    width: 70, padding: '3px 6px', textAlign: 'right',
    background: isZero ? 'rgba(120,120,120,0.10)' : 'rgba(13,21,38,0.9)',
    border: `1px solid ${isZero ? 'rgba(120,120,120,0.4)' : 'rgba(0,200,255,0.25)'}`,
    color: isZero ? 'rgba(220,235,250,0.5)' : '#cbd5f5',
    borderRadius: 4, fontSize: 11, fontFamily: 'JetBrains Mono, Menlo, monospace',
  };
}
