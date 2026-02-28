import React, { createContext, useContext, useState, useCallback, useEffect, useRef } from 'react';

// ── Default theme extracted from index.css :root ────────────────────────────

export const DEFAULT_THEME: Record<string, string> = {
  // Background Colors
  '--bg-primary': '#0f172a',
  '--bg-secondary': '#1e293b',
  '--bg-tertiary': '#334155',
  '--bg-elevated': '#3d4f66',
  // Text Colors
  '--text-primary': '#f8fafc',
  '--text-secondary': '#cbd5e1',
  '--text-muted': '#94a3b8',
  '--text-placeholder': '#64748b',
  // Border Colors
  '--border-color': '#3d4f66',
  '--border-subtle': '#2d3e50',
  // Accent Colors
  '--accent-blue': '#60a5fa',
  '--accent-blue-hover': '#3b82f6',
  '--accent-green': '#4ade80',
  '--accent-green-dim': '#22c55e',
  '--accent-yellow': '#fbbf24',
  '--accent-red': '#f87171',
  '--accent-purple': '#c084fc',
  '--accent-orange': '#fb923c',
  // Spacing
  '--space-1': '4px',
  '--space-2': '8px',
  '--space-3': '12px',
  '--space-4': '16px',
  '--space-5': '20px',
  '--space-6': '24px',
  '--space-8': '32px',
  '--space-10': '40px',
  // Font Sizes
  '--text-xs': '11px',
  '--text-sm': '13px',
  '--text-base': '14px',
  '--text-md': '15px',
  '--text-lg': '16px',
  '--text-xl': '18px',
  '--text-2xl': '20px',
  '--text-3xl': '24px',
  // Font Weights
  '--font-normal': '400',
  '--font-medium': '500',
  '--font-semibold': '600',
  '--font-bold': '700',
  // Border Radius
  '--radius-sm': '4px',
  '--radius-md': '6px',
  '--radius-lg': '8px',
  '--radius-xl': '12px',
  '--radius-2xl': '16px',
  // Shadows
  '--shadow-sm': '0 1px 2px rgba(0, 0, 0, 0.2)',
  '--shadow-md': '0 2px 8px rgba(0, 0, 0, 0.25)',
  '--shadow-lg': '0 8px 24px rgba(0, 0, 0, 0.3)',
  // Transitions
  '--transition-fast': '100ms ease',
  '--transition-base': '150ms ease',
  '--transition-slow': '250ms ease',
};

export const DEFAULT_FONT_FAMILY = "'Inter', -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif";

export const FONT_OPTIONS: { label: string; value: string; googleFont?: string }[] = [
  { label: 'Inter (Default)', value: "'Inter', -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif" },
  { label: 'System UI', value: "-apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif" },
  { label: 'Roboto', value: "'Roboto', sans-serif", googleFont: 'Roboto:wght@400;500;600;700' },
  { label: 'Poppins', value: "'Poppins', sans-serif", googleFont: 'Poppins:wght@400;500;600;700' },
  { label: 'DM Sans', value: "'DM Sans', sans-serif", googleFont: 'DM+Sans:wght@400;500;600;700' },
  { label: 'Plus Jakarta Sans', value: "'Plus Jakarta Sans', sans-serif", googleFont: 'Plus+Jakarta+Sans:wght@400;500;600;700' },
  { label: 'Source Sans 3', value: "'Source Sans 3', sans-serif", googleFont: 'Source+Sans+3:wght@400;500;600;700' },
  { label: 'Nunito Sans', value: "'Nunito Sans', sans-serif", googleFont: 'Nunito+Sans:wght@400;500;600;700' },
  { label: 'Space Grotesk', value: "'Space Grotesk', sans-serif", googleFont: 'Space+Grotesk:wght@400;500;600;700' },
  { label: 'JetBrains Mono', value: "'JetBrains Mono', monospace", googleFont: 'JetBrains+Mono:wght@400;500;600;700' },
];

const STORAGE_KEY = 'ignite_theme';

interface ThemeSave {
  overrides: Record<string, string>;
  fontFamily: string;
}

// ── Context shape ───────────────────────────────────────────────────────────

interface ThemeContextType {
  /** Current value of a variable (override if set, otherwise default) */
  getVar: (name: string) => string;
  /** Whether a variable has been overridden */
  isOverridden: (name: string) => boolean;
  /** Set a single CSS variable override */
  setVar: (name: string, value: string) => void;
  /** Reset one variable back to the default */
  resetVar: (name: string) => void;
  /** Reset everything to defaults */
  resetAll: () => void;
  /** Number of active overrides */
  overrideCount: number;
  /** Current font family */
  fontFamily: string;
  /** Set font family */
  setFontFamily: (value: string) => void;
}

const ThemeContext = createContext<ThemeContextType | null>(null);

// ── Google Font loader helper ───────────────────────────────────────────────

const loadedFonts = new Set<string>();

function loadGoogleFont(spec: string) {
  if (loadedFonts.has(spec)) return;
  loadedFonts.add(spec);
  const link = document.createElement('link');
  link.rel = 'stylesheet';
  link.href = `https://fonts.googleapis.com/css2?family=${spec}&display=swap`;
  document.head.appendChild(link);
}

// ── Provider ────────────────────────────────────────────────────────────────

export const ThemeProvider: React.FC<{ children: React.ReactNode }> = ({ children }) => {
  const [overrides, setOverrides] = useState<Record<string, string>>({});
  const [fontFamily, setFontFamilyState] = useState<string>(DEFAULT_FONT_FAMILY);
  const saveTimeout = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Load from localStorage on mount and apply immediately
  useEffect(() => {
    try {
      const raw = localStorage.getItem(STORAGE_KEY);
      if (raw) {
        const saved: ThemeSave = JSON.parse(raw);
        if (saved.overrides && typeof saved.overrides === 'object') {
          setOverrides(saved.overrides);
          for (const [name, value] of Object.entries(saved.overrides)) {
            document.documentElement.style.setProperty(name, value);
          }
        }
        if (saved.fontFamily) {
          setFontFamilyState(saved.fontFamily);
          document.body.style.fontFamily = saved.fontFamily;
          // Load Google Font if needed
          const match = FONT_OPTIONS.find((f) => f.value === saved.fontFamily);
          if (match?.googleFont) loadGoogleFont(match.googleFont);
        }
      }
    } catch {
      // Corrupt storage — ignore
    }
  }, []);

  // Debounced save to localStorage
  const persistSoon = useCallback(
    (newOverrides: Record<string, string>, newFont: string) => {
      if (saveTimeout.current) clearTimeout(saveTimeout.current);
      saveTimeout.current = setTimeout(() => {
        localStorage.setItem(STORAGE_KEY, JSON.stringify({ overrides: newOverrides, fontFamily: newFont }));
      }, 300);
    },
    [],
  );

  const setVar = useCallback(
    (name: string, value: string) => {
      document.documentElement.style.setProperty(name, value);
      setOverrides((prev) => {
        const next = { ...prev, [name]: value };
        persistSoon(next, fontFamily);
        return next;
      });
    },
    [persistSoon, fontFamily],
  );

  const resetVar = useCallback(
    (name: string) => {
      document.documentElement.style.removeProperty(name);
      setOverrides((prev) => {
        const next = { ...prev };
        delete next[name];
        persistSoon(next, fontFamily);
        return next;
      });
    },
    [persistSoon, fontFamily],
  );

  const resetAll = useCallback(() => {
    for (const name of Object.keys(overrides)) {
      document.documentElement.style.removeProperty(name);
    }
    document.body.style.fontFamily = '';
    setOverrides({});
    setFontFamilyState(DEFAULT_FONT_FAMILY);
    localStorage.removeItem(STORAGE_KEY);
  }, [overrides]);

  const setFontFamily = useCallback(
    (value: string) => {
      document.body.style.fontFamily = value;
      const match = FONT_OPTIONS.find((f) => f.value === value);
      if (match?.googleFont) loadGoogleFont(match.googleFont);
      setFontFamilyState(value);
      setOverrides((prev) => {
        persistSoon(prev, value);
        return prev;
      });
    },
    [persistSoon],
  );

  const getVar = useCallback(
    (name: string) => overrides[name] ?? DEFAULT_THEME[name] ?? '',
    [overrides],
  );

  const isOverridden = useCallback((name: string) => name in overrides, [overrides]);

  return (
    <ThemeContext.Provider
      value={{
        getVar,
        isOverridden,
        setVar,
        resetVar,
        resetAll,
        overrideCount: Object.keys(overrides).length,
        fontFamily,
        setFontFamily,
      }}
    >
      {children}
    </ThemeContext.Provider>
  );
};

export function useTheme(): ThemeContextType {
  const ctx = useContext(ThemeContext);
  if (!ctx) throw new Error('useTheme must be used within <ThemeProvider>');
  return ctx;
}
