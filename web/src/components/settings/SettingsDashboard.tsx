import React, { useState, useMemo } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faCog,
  faUndo,
  faChevronDown,
  faChevronRight,
  faPalette,
  faFont,
  faRuler,
  faBorderAll,
  faCircle,
  faAdjust,
} from '@fortawesome/free-solid-svg-icons';
import { useTheme, FONT_OPTIONS, DEFAULT_FONT_FAMILY } from '../../context/ThemeContext';

// ── Variable group definitions ──────────────────────────────────────────────

interface VarDef {
  name: string;
  label: string;
  type: 'color' | 'px' | 'weight' | 'shadow' | 'transition';
}

interface VarGroup {
  id: string;
  title: string;
  icon: typeof faPalette;
  vars: VarDef[];
}

const VAR_GROUPS: VarGroup[] = [
  {
    id: 'backgrounds',
    title: 'Background Colors',
    icon: faPalette,
    vars: [
      { name: '--bg-primary', label: 'Primary Background', type: 'color' },
      { name: '--bg-secondary', label: 'Secondary Background', type: 'color' },
      { name: '--bg-tertiary', label: 'Tertiary Background', type: 'color' },
      { name: '--bg-elevated', label: 'Elevated Background', type: 'color' },
    ],
  },
  {
    id: 'text',
    title: 'Text Colors',
    icon: faFont,
    vars: [
      { name: '--text-primary', label: 'Primary Text', type: 'color' },
      { name: '--text-secondary', label: 'Secondary Text', type: 'color' },
      { name: '--text-muted', label: 'Muted Text', type: 'color' },
      { name: '--text-placeholder', label: 'Placeholder Text', type: 'color' },
    ],
  },
  {
    id: 'borders',
    title: 'Border Colors',
    icon: faBorderAll,
    vars: [
      { name: '--border-color', label: 'Border Color', type: 'color' },
      { name: '--border-subtle', label: 'Subtle Border', type: 'color' },
    ],
  },
  {
    id: 'accents',
    title: 'Accent Colors',
    icon: faCircle,
    vars: [
      { name: '--accent-blue', label: 'Blue', type: 'color' },
      { name: '--accent-blue-hover', label: 'Blue Hover', type: 'color' },
      { name: '--accent-green', label: 'Green', type: 'color' },
      { name: '--accent-green-dim', label: 'Green Dim', type: 'color' },
      { name: '--accent-yellow', label: 'Yellow', type: 'color' },
      { name: '--accent-red', label: 'Red', type: 'color' },
      { name: '--accent-purple', label: 'Purple', type: 'color' },
      { name: '--accent-orange', label: 'Orange', type: 'color' },
    ],
  },
  {
    id: 'font-sizes',
    title: 'Font Sizes',
    icon: faFont,
    vars: [
      { name: '--text-xs', label: 'Extra Small', type: 'px' },
      { name: '--text-sm', label: 'Small', type: 'px' },
      { name: '--text-base', label: 'Base', type: 'px' },
      { name: '--text-md', label: 'Medium', type: 'px' },
      { name: '--text-lg', label: 'Large', type: 'px' },
      { name: '--text-xl', label: 'Extra Large', type: 'px' },
      { name: '--text-2xl', label: '2XL', type: 'px' },
      { name: '--text-3xl', label: '3XL', type: 'px' },
    ],
  },
  {
    id: 'font-weights',
    title: 'Font Weights',
    icon: faAdjust,
    vars: [
      { name: '--font-normal', label: 'Normal', type: 'weight' },
      { name: '--font-medium', label: 'Medium', type: 'weight' },
      { name: '--font-semibold', label: 'Semibold', type: 'weight' },
      { name: '--font-bold', label: 'Bold', type: 'weight' },
    ],
  },
  {
    id: 'spacing',
    title: 'Spacing Scale',
    icon: faRuler,
    vars: [
      { name: '--space-1', label: 'Space 1', type: 'px' },
      { name: '--space-2', label: 'Space 2', type: 'px' },
      { name: '--space-3', label: 'Space 3', type: 'px' },
      { name: '--space-4', label: 'Space 4', type: 'px' },
      { name: '--space-5', label: 'Space 5', type: 'px' },
      { name: '--space-6', label: 'Space 6', type: 'px' },
      { name: '--space-8', label: 'Space 8', type: 'px' },
      { name: '--space-10', label: 'Space 10', type: 'px' },
    ],
  },
  {
    id: 'radius',
    title: 'Border Radius',
    icon: faBorderAll,
    vars: [
      { name: '--radius-sm', label: 'Small', type: 'px' },
      { name: '--radius-md', label: 'Medium', type: 'px' },
      { name: '--radius-lg', label: 'Large', type: 'px' },
      { name: '--radius-xl', label: 'XL', type: 'px' },
      { name: '--radius-2xl', label: '2XL', type: 'px' },
    ],
  },
];

// ── Helpers ─────────────────────────────────────────────────────────────────

function parsePx(v: string): number {
  return parseInt(v, 10) || 0;
}

// ── Component ───────────────────────────────────────────────────────────────

export const SettingsDashboard: React.FC = () => {
  const theme = useTheme();
  const [expandedGroups, setExpandedGroups] = useState<Set<string>>(
    new Set(['backgrounds', 'text', 'accents']),
  );

  const toggleGroup = (id: string) => {
    setExpandedGroups((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  // ── Live preview samples ────────────────────────────────────────────────
  const previewCards = useMemo(() => {
    const bg1 = theme.getVar('--bg-primary');
    const bg2 = theme.getVar('--bg-secondary');
    const bg3 = theme.getVar('--bg-tertiary');
    const t1 = theme.getVar('--text-primary');
    const t2 = theme.getVar('--text-secondary');
    const tm = theme.getVar('--text-muted');
    const bc = theme.getVar('--border-color');
    const ab = theme.getVar('--accent-blue');
    const ag = theme.getVar('--accent-green');
    const ar = theme.getVar('--accent-red');
    const ay = theme.getVar('--accent-yellow');
    return { bg1, bg2, bg3, t1, t2, tm, bc, ab, ag, ar, ay };
  }, [theme]);

  return (
    <div className="settings-dashboard">
      {/* ── Header ──────────────────────────────────────────────────────── */}
      <div className="dashboard-header">
        <div className="header-title">
          <FontAwesomeIcon icon={faCog} style={{ fontSize: '24px' }} />
          <h2>Settings &amp; Theme</h2>
          {theme.overrideCount > 0 && (
            <span className="settings-badge">{theme.overrideCount} customized</span>
          )}
        </div>
        <div className="header-actions">
          <button
            className="btn btn-secondary btn-small"
            onClick={theme.resetAll}
            disabled={theme.overrideCount === 0 && theme.fontFamily === DEFAULT_FONT_FAMILY}
          >
            <FontAwesomeIcon icon={faUndo} style={{ marginRight: '0.4rem' }} />
            Reset All
          </button>
        </div>
      </div>

      {/* ── Live Preview ───────────────────────────────────────────────── */}
      <div className="section-header">
        <h3>Live Preview</h3>
      </div>
      <div className="settings-preview-card card" style={{ padding: '1.25rem' }}>
        <div className="settings-preview-grid">
          {/* Sample card */}
          <div
            className="settings-preview-sample"
            style={{
              background: previewCards.bg2,
              border: `1px solid ${previewCards.bc}`,
              borderRadius: theme.getVar('--radius-lg'),
              padding: '1rem',
            }}
          >
            <div style={{ fontSize: theme.getVar('--text-lg'), fontWeight: 600, color: previewCards.t1, marginBottom: '0.5rem' }}>
              Sample Heading
            </div>
            <div style={{ fontSize: theme.getVar('--text-sm'), color: previewCards.t2, marginBottom: '0.75rem' }}>
              This is secondary text showing how body content looks with the current theme.
            </div>
            <div style={{ fontSize: theme.getVar('--text-xs'), color: previewCards.tm }}>
              Muted caption text
            </div>
          </div>
          {/* Accent swatches */}
          <div className="settings-preview-swatches">
            <div className="settings-swatch-row">
              {[
                { label: 'Blue', color: previewCards.ab },
                { label: 'Green', color: previewCards.ag },
                { label: 'Red', color: previewCards.ar },
                { label: 'Yellow', color: previewCards.ay },
              ].map((s) => (
                <div key={s.label} className="settings-swatch">
                  <div className="settings-swatch-circle" style={{ backgroundColor: s.color }} />
                  <span>{s.label}</span>
                </div>
              ))}
            </div>
            {/* Sample button */}
            <div style={{ display: 'flex', gap: '0.5rem', marginTop: '0.75rem' }}>
              <button
                className="btn btn-primary btn-small"
                style={{ background: previewCards.ab, border: 'none', pointerEvents: 'none' }}
              >
                Primary Button
              </button>
              <button
                className="btn btn-secondary btn-small"
                style={{ background: previewCards.bg3, color: previewCards.t1, border: `1px solid ${previewCards.bc}`, pointerEvents: 'none' }}
              >
                Secondary
              </button>
            </div>
            {/* Background layers */}
            <div className="settings-bg-preview" style={{ marginTop: '0.75rem' }}>
              {[
                { label: 'Primary', color: previewCards.bg1 },
                { label: 'Secondary', color: previewCards.bg2 },
                { label: 'Tertiary', color: previewCards.bg3 },
              ].map((b) => (
                <div key={b.label} className="settings-bg-chip" style={{ backgroundColor: b.color, border: `1px solid ${previewCards.bc}` }}>
                  <span style={{ color: previewCards.t2, fontSize: '0.7rem' }}>{b.label}</span>
                </div>
              ))}
            </div>
          </div>
        </div>
      </div>

      {/* ── Font Family ────────────────────────────────────────────────── */}
      <div className="section-header" style={{ marginTop: '1.5rem' }}>
        <h3>
          <FontAwesomeIcon icon={faFont} style={{ marginRight: '0.4rem' }} />
          Font Family
        </h3>
        {theme.fontFamily !== DEFAULT_FONT_FAMILY && (
          <button
            className="settings-reset-btn"
            title="Reset to default"
            onClick={() => theme.setFontFamily(DEFAULT_FONT_FAMILY)}
          >
            <FontAwesomeIcon icon={faUndo} />
          </button>
        )}
      </div>
      <div className="card" style={{ padding: '1rem' }}>
        <select
          className="settings-font-select"
          value={theme.fontFamily}
          onChange={(e) => theme.setFontFamily(e.target.value)}
        >
          {FONT_OPTIONS.map((f) => (
            <option key={f.value} value={f.value}>{f.label}</option>
          ))}
        </select>
        <div className="settings-font-preview" style={{ fontFamily: theme.fontFamily }}>
          The quick brown fox jumps over the lazy dog — 0123456789
        </div>
      </div>

      {/* ── Variable Groups ────────────────────────────────────────────── */}
      {VAR_GROUPS.map((group) => (
        <div key={group.id} style={{ marginTop: '1.25rem' }}>
          <div
            className="section-header settings-group-header"
            onClick={() => toggleGroup(group.id)}
          >
            <h3>
              <FontAwesomeIcon
                icon={expandedGroups.has(group.id) ? faChevronDown : faChevronRight}
                style={{ marginRight: '0.5rem', fontSize: '0.75rem' }}
              />
              <FontAwesomeIcon icon={group.icon} style={{ marginRight: '0.4rem' }} />
              {group.title}
            </h3>
            <span className="settings-group-count">
              {group.vars.filter((v) => theme.isOverridden(v.name)).length} / {group.vars.length}
            </span>
          </div>
          {expandedGroups.has(group.id) && (
            <div className="card settings-vars-card">
              <div className="settings-vars-grid">
                {group.vars.map((v) => (
                  <VarControl key={v.name} def={v} />
                ))}
              </div>
            </div>
          )}
        </div>
      ))}

      {/* ── Styles ─────────────────────────────────────────────────────── */}
      <style>{STYLES}</style>
    </div>
  );
};

// ── Individual variable control ─────────────────────────────────────────────

const VarControl: React.FC<{ def: VarDef }> = ({ def }) => {
  const theme = useTheme();
  const value = theme.getVar(def.name);
  const overridden = theme.isOverridden(def.name);

  return (
    <div className={`settings-var ${overridden ? 'overridden' : ''}`}>
      <div className="settings-var-header">
        <label className="settings-var-label">{def.label}</label>
        <code className="settings-var-name">{def.name}</code>
        {overridden && (
          <button
            className="settings-reset-btn"
            title="Reset to default"
            onClick={() => theme.resetVar(def.name)}
          >
            <FontAwesomeIcon icon={faUndo} />
          </button>
        )}
      </div>
      <div className="settings-var-control">
        {def.type === 'color' && (
          <ColorInput value={value} onChange={(v) => theme.setVar(def.name, v)} />
        )}
        {def.type === 'px' && (
          <PxInput value={value} onChange={(v) => theme.setVar(def.name, v)} />
        )}
        {def.type === 'weight' && (
          <WeightInput value={value} onChange={(v) => theme.setVar(def.name, v)} />
        )}
        {(def.type === 'shadow' || def.type === 'transition') && (
          <TextInput value={value} onChange={(v) => theme.setVar(def.name, v)} />
        )}
      </div>
    </div>
  );
};

// ── Input widgets ───────────────────────────────────────────────────────────

const ColorInput: React.FC<{ value: string; onChange: (v: string) => void }> = ({ value, onChange }) => {
  // Ensure we have a valid 6-char hex for the color picker
  const hexForPicker = value.startsWith('#') && (value.length === 7 || value.length === 4) ? value : '#000000';

  return (
    <div className="settings-color-input">
      <input
        type="color"
        value={hexForPicker}
        onChange={(e) => onChange(e.target.value)}
        className="settings-color-picker"
      />
      <input
        type="text"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="settings-text-field"
        spellCheck={false}
      />
      <div className="settings-color-swatch-preview" style={{ backgroundColor: value }} />
    </div>
  );
};

const PxInput: React.FC<{ value: string; onChange: (v: string) => void }> = ({ value, onChange }) => (
  <div className="settings-px-input">
    <input
      type="number"
      value={parsePx(value)}
      onChange={(e) => onChange(`${e.target.value}px`)}
      min={0}
      max={200}
      step={1}
      className="settings-number-field"
    />
    <span className="settings-unit">px</span>
    <div className="settings-px-preview" style={{ width: value, height: '8px', background: 'var(--accent-blue)', borderRadius: '4px' }} />
  </div>
);

const WeightInput: React.FC<{ value: string; onChange: (v: string) => void }> = ({ value, onChange }) => (
  <div className="settings-weight-input">
    <select
      value={value}
      onChange={(e) => onChange(e.target.value)}
      className="settings-select-field"
    >
      {[100, 200, 300, 400, 500, 600, 700, 800, 900].map((w) => (
        <option key={w} value={String(w)}>{w}</option>
      ))}
    </select>
    <span className="settings-weight-preview" style={{ fontWeight: Number(value) }}>Aa</span>
  </div>
);

const TextInput: React.FC<{ value: string; onChange: (v: string) => void }> = ({ value, onChange }) => (
  <input
    type="text"
    value={value}
    onChange={(e) => onChange(e.target.value)}
    className="settings-text-field settings-text-wide"
    spellCheck={false}
  />
);

// ── Styles ──────────────────────────────────────────────────────────────────

const STYLES = `
  .settings-dashboard {
    padding: 1rem;
  }

  .settings-badge {
    padding: 0.2rem 0.6rem;
    background: var(--accent-purple, #c084fc);
    color: white;
    border-radius: 1rem;
    font-size: 0.75rem;
    font-weight: 500;
  }

  /* ── Preview ── */
  .settings-preview-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 1.25rem;
  }
  @media (max-width: 700px) {
    .settings-preview-grid { grid-template-columns: 1fr; }
  }
  .settings-preview-swatches {
    display: flex;
    flex-direction: column;
  }
  .settings-swatch-row {
    display: flex;
    gap: 1rem;
    flex-wrap: wrap;
  }
  .settings-swatch {
    display: flex;
    align-items: center;
    gap: 0.4rem;
    font-size: 0.8rem;
    color: var(--text-secondary);
  }
  .settings-swatch-circle {
    width: 20px;
    height: 20px;
    border-radius: 50%;
    border: 2px solid var(--border-color);
  }
  .settings-bg-preview {
    display: flex;
    gap: 0.5rem;
  }
  .settings-bg-chip {
    flex: 1;
    padding: 0.4rem 0.5rem;
    border-radius: 6px;
    text-align: center;
  }

  /* ── Font selector ── */
  .settings-font-select {
    width: 100%;
    padding: 0.5rem 0.75rem;
    background: var(--bg-tertiary);
    border: 1px solid var(--border-color);
    border-radius: 6px;
    color: var(--text-primary);
    font-size: 0.9rem;
    cursor: pointer;
  }
  .settings-font-select option {
    background: var(--bg-secondary);
    color: var(--text-primary);
  }
  .settings-font-preview {
    margin-top: 0.75rem;
    padding: 0.75rem;
    background: var(--bg-tertiary);
    border-radius: 6px;
    font-size: 1rem;
    color: var(--text-primary);
    line-height: 1.6;
  }

  /* ── Group headers ── */
  .settings-group-header {
    cursor: pointer;
    user-select: none;
    padding: 0.25rem 0;
  }
  .settings-group-header:hover h3 {
    color: var(--text-primary);
  }
  .settings-group-count {
    font-size: 0.75rem;
    color: var(--text-muted);
    background: var(--bg-tertiary);
    padding: 0.15rem 0.5rem;
    border-radius: 10px;
  }

  /* ── Variable cards ── */
  .settings-vars-card {
    padding: 1rem;
  }
  .settings-vars-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
    gap: 1rem;
  }
  .settings-var {
    padding: 0.75rem;
    background: var(--bg-tertiary);
    border-radius: var(--radius-md);
    border: 1px solid transparent;
    transition: border-color 0.15s;
  }
  .settings-var.overridden {
    border-color: var(--accent-purple);
  }
  .settings-var-header {
    display: flex;
    align-items: center;
    gap: 0.4rem;
    margin-bottom: 0.5rem;
    flex-wrap: wrap;
  }
  .settings-var-label {
    font-size: 0.85rem;
    font-weight: 600;
    color: var(--text-primary);
  }
  .settings-var-name {
    font-size: 0.65rem;
    padding: 0.1rem 0.35rem;
    background: var(--bg-primary);
    border-radius: 4px;
    color: var(--text-muted);
    margin-left: auto;
  }
  .settings-var-control {
    display: flex;
    align-items: center;
  }

  /* ── Reset button ── */
  .settings-reset-btn {
    background: transparent;
    border: none;
    color: var(--accent-purple);
    cursor: pointer;
    padding: 0.2rem 0.35rem;
    border-radius: 4px;
    font-size: 0.7rem;
    display: flex;
    align-items: center;
  }
  .settings-reset-btn:hover {
    background: var(--bg-elevated);
    color: var(--accent-red);
  }

  /* ── Color input ── */
  .settings-color-input {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    width: 100%;
  }
  .settings-color-picker {
    width: 36px;
    height: 36px;
    padding: 0;
    border: 2px solid var(--border-color);
    border-radius: 6px;
    cursor: pointer;
    background: none;
    flex-shrink: 0;
  }
  .settings-color-picker::-webkit-color-swatch-wrapper {
    padding: 2px;
  }
  .settings-color-picker::-webkit-color-swatch {
    border: none;
    border-radius: 4px;
  }
  .settings-color-swatch-preview {
    width: 24px;
    height: 24px;
    border-radius: 50%;
    border: 2px solid var(--border-color);
    flex-shrink: 0;
  }

  /* ── Shared field styles ── */
  .settings-text-field {
    flex: 1;
    padding: 0.4rem 0.6rem;
    background: var(--bg-primary);
    border: 1px solid var(--border-color);
    border-radius: 6px;
    color: var(--text-primary);
    font-size: 0.8rem;
    font-family: 'SF Mono', 'Fira Code', monospace;
  }
  .settings-text-field:focus {
    outline: none;
    border-color: var(--accent-blue);
    box-shadow: 0 0 0 2px rgba(96,165,250,0.2);
  }
  .settings-text-wide {
    width: 100%;
  }

  /* ── Px input ── */
  .settings-px-input {
    display: flex;
    align-items: center;
    gap: 0.4rem;
    width: 100%;
  }
  .settings-number-field {
    width: 72px;
    padding: 0.4rem 0.5rem;
    background: var(--bg-primary);
    border: 1px solid var(--border-color);
    border-radius: 6px;
    color: var(--text-primary);
    font-size: 0.85rem;
    text-align: right;
  }
  .settings-number-field:focus {
    outline: none;
    border-color: var(--accent-blue);
    box-shadow: 0 0 0 2px rgba(96,165,250,0.2);
  }
  .settings-unit {
    font-size: 0.75rem;
    color: var(--text-muted);
  }
  .settings-px-preview {
    margin-left: auto;
    transition: width 0.2s;
  }

  /* ── Weight input ── */
  .settings-weight-input {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    width: 100%;
  }
  .settings-select-field {
    flex: 1;
    padding: 0.4rem 0.6rem;
    background: var(--bg-primary);
    border: 1px solid var(--border-color);
    border-radius: 6px;
    color: var(--text-primary);
    font-size: 0.85rem;
    cursor: pointer;
  }
  .settings-select-field option {
    background: var(--bg-secondary);
    color: var(--text-primary);
  }
  .settings-weight-preview {
    font-size: 1.25rem;
    color: var(--text-primary);
  }
`;

export default SettingsDashboard;
