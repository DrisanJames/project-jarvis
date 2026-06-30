// theme.ts — the canonical indigo design system for the Mailing Portal.
//
// This is extracted from the Delivery Queue ("Sending Engine") screen
// (OutboxDashboard.tsx), which is the gold standard every other screen is now
// modeled after. Import these tokens / style objects instead of re-deriving
// inline colors so the whole portal stays visually consistent.
//
// Palette: indigo (#6366f1 / #818cf8 / #c7d2fe) over translucent slate-blue
// panels (rgba(15,30,60,0.35)) on a deep slate-blue page background.

import type React from "react"

// ---------------------------------------------------------------------------
// Color tokens
// ---------------------------------------------------------------------------

export const colors = {
  // Page / app chrome
  appBg: "linear-gradient(180deg, #0b1226 0%, #070a16 50%, #0a0e1c 100%)",
  appBgSolid: "#0a0e1c",

  // Panels / surfaces
  panelBg: "rgba(15,30,60,0.35)",
  panelBgSolid: "#0f1629",
  panelBorder: "rgba(99,102,241,0.18)",
  panelBorderStrong: "rgba(99,102,241,0.30)",
  divider: "rgba(99,102,241,0.08)",
  hairline: "rgba(99,102,241,0.15)",
  hover: "rgba(99,102,241,0.10)",

  // Text
  heading: "#dbeafe",
  text: "#e5e7eb",
  textMuted: "#94a3b8",
  textFaint: "#64748b",

  // Indigo accent family
  indigo500: "#6366f1",
  indigo400: "#818cf8",
  indigo300: "#a5b4fc",
  indigo200: "#c7d2fe",

  // Semantic
  success: "#22c55e",
  successText: "#86efac",
  warning: "#f59e0b",
  warningText: "#fcd34d",
  danger: "#ef4444",
  dangerText: "#fca5a5",
  dangerFaint: "#fecaca",
  idle: "#94a3b8",
  info: "#818cf8",
} as const

// Translucent fills for status chips / badges (color + alpha suffix trick from
// OutboxDashboard: `color + '22'` for bg, `color + '66'` for border).
export const alpha = (hex: string, a: "0d" | "14" | "22" | "33" | "44" | "66") => `${hex}${a}`

export const gradients = {
  // Utilization / progress bar fills
  progressOk: "linear-gradient(90deg,#6366f1,#22c55e)",
  progressHot: "linear-gradient(90deg,#f59e0b,#ef4444)",
  // Brand button / accent
  accent: "linear-gradient(135deg,#6366f1 0%,#818cf8 100%)",
} as const

// ---------------------------------------------------------------------------
// Style objects (drop-in React.CSSProperties)
// ---------------------------------------------------------------------------

export const panelStyle: React.CSSProperties = {
  background: colors.panelBg,
  border: `1px solid ${colors.panelBorder}`,
  borderRadius: 10,
  padding: "14px 16px",
}

export const panelTitleStyle: React.CSSProperties = {
  margin: 0,
  fontSize: 13,
  fontWeight: 600,
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
  borderRadius: 8,
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
