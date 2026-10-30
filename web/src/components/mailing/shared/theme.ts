// theme.ts — the canonical design system for the Mailing Portal.
//
// Every value resolves through a --j-* CSS variable (styles/jarvis-tokens.css),
// so the whole portal follows the light/dark mode set on <html data-theme>.
// Palette: the Jarvis brand — violet / pink / cyan / lime over ink (dark) or
// cream (light). Spec: docs/PORTAL_DESIGN_SYSTEM.md §2.
//
// Hard rules stay literal and mode-independent: hard bounce #ef4444, soft
// bounce #f59e0b (METRIC_CONTRACT).

import type React from "react"

// ---------------------------------------------------------------------------
// Color tokens
// ---------------------------------------------------------------------------

const v = (t: string) => `var(--j-${t})`
const mix = (c: string, pct: number) => `color-mix(in srgb, ${c} ${pct}%, transparent)`

export const colors = {
  // Page / app chrome
  appBg: v("bg-grad"),
  appBgSolid: v("bg"),

  // Panels / surfaces
  panelBg: v("surface"),
  panelBgSolid: v("surface"),
  panelBorder: v("border"),
  panelBorderStrong: v("border-strong"),
  divider: v("border-subtle"),
  hairline: v("border"),
  hover: mix(v("violet"), 10),

  // Text
  heading: v("text"),
  text: v("text"),
  textMuted: v("text-muted"),
  textFaint: v("text-faint"),

  // Accent family (names kept for existing call sites)
  indigo500: v("violet-strong"),
  indigo400: v("violet"),
  indigo300: v("violet"),
  indigo200: v("violet-soft"),
  pink: v("pink"),
  cyan: v("cyan"),

  // Semantic
  success: v("success"),
  successText: v("success-soft"),
  warning: "#f59e0b",
  warningText: v("warn"),
  danger: "#ef4444",
  dangerText: v("danger-soft"),
  dangerFaint: v("danger-soft"),
  idle: v("text-muted"),
  info: v("violet"),
} as const

// Translucent fills for chips / badges: `alpha(c, '22')` for bg, `'66'` for
// border. Works on var() tokens and hex alike (hex-suffix concatenation does
// not, which is why this is color-mix).
const ALPHA_PCT = { "0d": 5, "14": 8, "22": 13, "33": 20, "44": 27, "66": 40 } as const
export const alpha = (c: string, a: keyof typeof ALPHA_PCT) => mix(c, ALPHA_PCT[a])

export const gradients = {
  // Utilization / progress bar fills
  progressOk: `linear-gradient(90deg,${v("violet-strong")},${v("success")})`,
  progressHot: "linear-gradient(90deg,#f59e0b,#ef4444)",
  // Brand button / accent — the marketing site's violet → pink
  accent: `linear-gradient(135deg,${v("violet-strong")} 0%,${v("pink")} 100%)`,
} as const

// ---------------------------------------------------------------------------
// Style objects (drop-in React.CSSProperties)
// ---------------------------------------------------------------------------

export const panelStyle: React.CSSProperties = {
  background: colors.panelBg,
  border: `1px solid ${colors.panelBorder}`,
  borderRadius: 14,
  padding: "14px 16px",
  boxShadow: v("shadow"),
}

export const panelTitleStyle: React.CSSProperties = {
  margin: 0,
  fontFamily: v("font-display"),
  fontSize: 13,
  fontWeight: 700,
  color: colors.heading,
  textTransform: "uppercase",
  letterSpacing: 0.6,
  display: "flex",
  alignItems: "center",
  gap: 8,
}

export const thStyle: React.CSSProperties = {
  textAlign: "left",
  padding: "7px 10px",
  fontWeight: 600,
  fontSize: 11,
  textTransform: "uppercase",
  letterSpacing: 0.4,
  color: colors.textMuted,
  borderBottom: `1px solid ${colors.hairline}`,
}

export const tdStyle: React.CSSProperties = {
  padding: "7px 10px",
  fontSize: 12,
  color: colors.text,
  borderTop: `1px solid ${colors.divider}`,
}

export const numTd: React.CSSProperties = {
  ...tdStyle,
  textAlign: "right",
  fontVariantNumeric: "tabular-nums",
}

export const numTh: React.CSSProperties = { ...thStyle, textAlign: "right" }

export const tableStyle: React.CSSProperties = {
  width: "100%",
  borderCollapse: "collapse",
}

// Primary action button (indigo, subtle).
export const btnStyle: React.CSSProperties = {
  background: alpha(colors.indigo500, "22"),
  border: `1px solid ${alpha(colors.indigo500, "66")}`,
  color: colors.indigo200,
  borderRadius: 999,
  padding: "7px 12px",
  fontSize: 12,
  fontWeight: 600,
  cursor: "pointer",
}

// Page wrapper used at the top of each screen so every screen shares the same
// padding / text color / min-height as the Delivery Queue.
export const pageStyle: React.CSSProperties = {
  padding: 24,
  color: colors.text,
  minHeight: "100vh",
}

// Responsive auto-fit card grid (the Delivery Queue "Capacity / Schedule" grid).
export const cardGrid = (min = 330): React.CSSProperties => ({
  display: "grid",
  gridTemplateColumns: `repeat(auto-fit, minmax(${min}px, 1fr))`,
  gap: 14,
})

// Semantic color for a verdict / state keyword. Centralized so STORM/SENDING/
// pass/fail coloring is identical everywhere.
export const stateColor = (state: string): string => {
  const s = state.toLowerCase()
  if (["storm", "fail", "failed", "critical", "decrease", "down", "error"].includes(s)) return colors.danger
  if (["backlogged", "warn", "warning", "unknown", "stale", "at_risk"].includes(s)) return colors.warning
  if (["sending", "pass", "passed", "healthy", "ok", "increase", "up", "live"].includes(s)) return colors.success
  if (["idle", "maintain", "neutral", "paused"].includes(s)) return colors.idle
  return colors.indigo400
}
