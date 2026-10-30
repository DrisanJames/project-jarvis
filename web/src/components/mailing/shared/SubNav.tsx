// SubNav — shared horizontal tab-row primitive.
//
// A small reusable sub-navigation row that matches the portal's existing
// section sub-nav look. The container + button styles are copied verbatim
// from MailingPortal.tsx's `subNavStyle` / `subNavBtnStyle(active)` so the
// appearance stays cohesive across screens.
//
// Accessibility: the container is a `role="tablist"`; each button is a
// `role="tab"` with `aria-selected`, preserving the a11y the call sites had.

import React from 'react';

// Copied EXACTLY from MailingPortal.tsx (subNavStyle / subNavBtnStyle) so the
// appearance matches the portal's other section sub-navs.
const subNavStyle: React.CSSProperties = {
  display: 'flex', gap: 4, marginBottom: 20, padding: '4px',
  background: 'var(--j-surface)', borderRadius: 999, border: '1px solid var(--j-border)',
  width: 'fit-content',
};

const subNavBtnStyle = (active: boolean): React.CSSProperties => ({
  padding: '8px 18px', borderRadius: 999, border: 'none', cursor: 'pointer',
  fontSize: 13, fontWeight: active ? 700 : 500, transition: 'all 0.15s',
  background: active ? 'var(--j-violet-strong)' : 'transparent',
  color: active ? 'var(--j-on-accent)' : 'var(--j-text-2)',
});

export interface SubNavItem {
  key: string;
  label: string;
}

export interface SubNavProps {
  items: SubNavItem[];
  active: string;
  onChange: (key: string) => void;
  ariaLabel?: string;
}

export const SubNav: React.FC<SubNavProps> = ({ items, active, onChange, ariaLabel }) => {
  return (
    <div style={subNavStyle} role="tablist" aria-label={ariaLabel}>
      {items.map(item => {
        const isActive = item.key === active;
        return (
          <button
            key={item.key}
            type="button"
            style={subNavBtnStyle(isActive)}
            onClick={() => onChange(item.key)}
            role="tab"
            aria-selected={isActive}
          >
            {item.label}
          </button>
        );
      })}
    </div>
  );
};

export default SubNav;
