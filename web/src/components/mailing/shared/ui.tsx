// ui.tsx — shared indigo UI primitives for the Mailing Portal.
//
// These mirror the building blocks defined inline in the Delivery Queue
// (OutboxDashboard.tsx) so every screen composes from the same parts:
// Panel, SectionHeader, Stat tile, SectionError chip, Pill/StatusPill,
// ProgressBar, LivePill, EmptyState.

import React from "react"
import { FontAwesomeIcon } from "@fortawesome/react-fontawesome"
import type { IconDefinition } from "@fortawesome/fontawesome-svg-core"
import { faExclamationTriangle } from "@fortawesome/free-solid-svg-icons"
import { colors, alpha, panelStyle, panelTitleStyle } from "./theme"

// A surface panel. Optional colored left border (status accent).
export const Panel: React.FC<{
  accent?: string
  style?: React.CSSProperties
  children: React.ReactNode
}> = ({ accent, style, children }) => (
  <div
    style={{
      ...panelStyle,
      ...(accent ? { borderLeft: `4px solid ${accent}` } : {}),
      ...style,
    }}
  >
    {children}
  </div>
)

// Uppercase section title with optional icon + right-aligned slot.
export const SectionHeader: React.FC<{
  title: string
  icon?: IconDefinition
  right?: React.ReactNode
  style?: React.CSSProperties
}> = ({ title, icon, right, style }) => (
  <div
    style={{
      display: "flex",
      alignItems: "center",
      justifyContent: "space-between",
      marginBottom: 10,
      ...style,
    }}
  >
    <h3 style={panelTitleStyle}>
      {icon && <FontAwesomeIcon icon={icon} style={{ color: colors.indigo400 }} />}
      {title}
    </h3>
    {right}
  </div>
)

// KPI tile: small uppercase label over a large tabular number.
export const Stat: React.FC<{
  label: string
  value: React.ReactNode
  sub?: React.ReactNode
  color?: string
  title?: string
}> = ({ label, value, sub, color, title }) => (
  <div style={{ minWidth: 90 }} title={title}>
    <div style={{ fontSize: 10, color: colors.textMuted, textTransform: "uppercase", letterSpacing: 0.5 }}>{label}</div>
    <div style={{ fontSize: 20, fontWeight: 700, color: color ?? colors.text, fontVariantNumeric: "tabular-nums" }}>
      {value}
    </div>
    {sub != null && <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 2 }}>{sub}</div>}
  </div>
)

// Graceful per-section degradation chip (keeps layout stable on partial failure).
export const SectionError: React.FC<{ label: string; error?: string }> = ({ label, error }) => (
  <div
    style={{
      fontSize: 12,
      color: colors.dangerText,
      background: "rgba(239,68,68,0.08)",
      border: "1px solid rgba(239,68,68,0.25)",
      borderRadius: 6,
      padding: "8px 10px",
    }}
  >
    <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 6 }} />
    {label} unavailable{error ? `: ${error}` : ""}
  </div>
)

// Friendly empty state (use instead of a blank screen or an error dump).
export const EmptyState: React.FC<{ icon?: IconDefinition; title: string; hint?: string }> = ({ icon, title, hint }) => (
  <div style={{ textAlign: "center", padding: "32px 16px", color: colors.textFaint }}>
    {icon && <FontAwesomeIcon icon={icon} style={{ fontSize: 26, color: colors.indigo400, marginBottom: 10, opacity: 0.7 }} />}
    <div style={{ fontSize: 14, color: colors.textMuted, fontWeight: 600 }}>{title}</div>
    {hint && <div style={{ fontSize: 12, marginTop: 4 }}>{hint}</div>}
  </div>
)

// Rounded status pill (STORM / SENDING / pass / fail …). Color drives bg+border.
export const Pill: React.FC<{ color: string; children: React.ReactNode; style?: React.CSSProperties }> = ({
  color,
  children,
  style,
}) => (
  <span
    style={{
      display: "inline-flex",
      alignItems: "center",
      gap: 6,
      background: alpha(color, "22"),
      border: `1px solid ${alpha(color, "66")}`,
      color,
      borderRadius: 999,
      padding: "2px 10px",
      fontSize: 11,
      fontWeight: 700,
      letterSpacing: 0.4,
      textTransform: "uppercase",
      ...style,
    }}
  >
    {children}
  </span>
)

// Live/stale heartbeat pill (pulsing dot + "updated Ns ago").
export const LivePill: React.FC<{ live: boolean; agoSeconds?: number }> = ({ live, agoSeconds }) => {
  const color = live ? colors.success : colors.warning
  return (
    <span style={{ display: "inline-flex", alignItems: "center", gap: 6, fontSize: 11, color: colors.textMuted }}>
      <span
        style={{
          width: 8,
          height: 8,
          borderRadius: 999,
          background: color,
          boxShadow: `0 0 0 0 ${alpha(color, "66")}`,
          animation: live ? "portalPulse 1.8s infinite" : "none",
        }}
      />
      <span style={{ color, fontWeight: 700, letterSpacing: 0.6 }}>{live ? "LIVE" : "STALE"}</span>
      {agoSeconds != null && <span>· updated {agoSeconds}s ago</span>}
    </span>
  )
}

// Animated progress / utilization bar (smooth width transition, hot >90%).
export const ProgressBar: React.FC<{ pct: number; height?: number }> = ({ pct, height = 8 }) => {
  const clamped = Math.max(0, Math.min(1, pct))
  const hot = clamped > 0.9
  return (
    <div style={{ background: "rgba(99,102,241,0.12)", borderRadius: 999, height, overflow: "hidden" }}>
      <div
        style={{
          width: `${clamped * 100}%`,
          height: "100%",
          background: hot ? "linear-gradient(90deg,#f59e0b,#ef4444)" : "linear-gradient(90deg,#6366f1,#22c55e)",
          transition: "width 600ms ease",
        }}
      />
    </div>
  )
}

// One keyframe stylesheet, injected once per screen that uses LivePill.
export const PortalKeyframes: React.FC = () => (
  <style>{`@keyframes portalPulse{0%{box-shadow:0 0 0 0 rgba(34,197,94,0.5)}70%{box-shadow:0 0 0 6px rgba(34,197,94,0)}100%{box-shadow:0 0 0 0 rgba(34,197,94,0)}}`}</style>
)
