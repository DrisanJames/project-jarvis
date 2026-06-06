/**
 * Canonical segment category metadata.
 *
 * Mirrors the enum seeded by `runStartupMigrations` in
 * `upside-down/cmd/server/main.go` (may29_seg_backfill_*) and validated
 * server-side in `validSegmentCategories` (mailing_segments.go) and
 * `CreateSegmentRequest`/`UpdateSegment` (segmentation_handlers.go).
 *
 * Adding a category requires four coordinated changes:
 *   1. Add the value to `validSegmentCategories` in mailing_segments.go.
 *   2. Add a `may29_seg_backfill_<name>` UPDATE in runStartupMigrations.
 *   3. Add the entry to SEGMENT_CATEGORIES below.
 *   4. Update the wizard/dashboard default-active sets if needed.
 *
 * Versioned alongside `VersionMailingSegmentsAPIv1` (currently 1.1.0).
 */

export type SegmentCategory =
  | "engagement_brand"
  | "engagement_global"
  | "engagement_isp"
  | "engagement_vertical"
  | "framework"
  | "funnel"
  | "cohort_static"
  | "suppression_exclusion"
  | "partner_wave_static"
  | "legacy_snapshot"
  | "uncategorized";

export interface SegmentCategoryMeta {
  id: SegmentCategory;
  label: string;
  shortLabel: string;
  description: string;
  /** Tailwind-compatible badge classes (bg + text + border). */
  badgeClass: string;
  /** Whether this category is shown by default in the wizard's inclusion picker. */
  defaultInclusionVisible: boolean;
  /** Whether this category is shown by default in the wizard's exclusion picker. */
  defaultExclusionVisible: boolean;
}

export const SEGMENT_CATEGORIES: SegmentCategoryMeta[] = [
  {
    id: "engagement_brand",
    label: "Engagement — Per Property",
    shortLabel: "Brand Eng.",
    description: "7D/14D/30D Openers + Clickers scoped to a single brand domain.",
    badgeClass: "bg-indigo-500/15 text-indigo-300 border-indigo-400/40",
    defaultInclusionVisible: true,
    defaultExclusionVisible: false,
  },
  {
    id: "engagement_global",
    label: "Engagement — Cross Property",
    shortLabel: "Global Eng.",
    description: "7D/14D/30D Openers + Clickers across every sending domain.",
    badgeClass: "bg-purple-500/15 text-purple-300 border-purple-400/40",
    defaultInclusionVisible: true,
    defaultExclusionVisible: false,
  },
  {
    id: "engagement_isp",
    label: "Engagement — Per ISP",
    shortLabel: "ISP Eng.",
    description: "Brand × ISP engagement slices (Gmail, Yahoo, AOL, etc).",
    badgeClass: "bg-blue-500/15 text-blue-300 border-blue-400/40",
    defaultInclusionVisible: true,
    defaultExclusionVisible: false,
  },
  {
    id: "engagement_vertical",
    label: "Engagement — Per Vertical",
    shortLabel: "Vertical Eng.",
    description: "7D/14D/30D Openers + Clickers scoped to a data-provenance vertical (Mortgage, Finance, etc), cross-domain.",
    badgeClass: "bg-cyan-500/15 text-cyan-300 border-cyan-400/40",
    defaultInclusionVisible: true,
    defaultExclusionVisible: false,
  },
  {
    id: "framework",
    label: "Framework",
    shortLabel: "Framework",
    description: "Framework-prefixed master cohorts (Engaged, Recent-Click, etc).",
    badgeClass: "bg-emerald-500/15 text-emerald-300 border-emerald-400/40",
    defaultInclusionVisible: true,
    defaultExclusionVisible: false,
  },
  {
    id: "funnel",
    label: "Funnel",
    shortLabel: "Funnel",
    description: "Week-N funnel cohorts feeding the engager schedule.",
    badgeClass: "bg-teal-500/15 text-teal-300 border-teal-400/40",
    defaultInclusionVisible: true,
    defaultExclusionVisible: false,
  },
  {
    id: "cohort_static",
    label: "Static Cohort",
    shortLabel: "Cohort",
    description: "Pre-computed cohorts: Converters, WCM, Vertical, Master, Mailgun-Engaged.",
    badgeClass: "bg-amber-500/15 text-amber-300 border-amber-400/40",
    defaultInclusionVisible: true,
    defaultExclusionVisible: false,
  },
  {
    id: "suppression_exclusion",
    label: "Suppression / Exclusion",
    shortLabel: "Exclude",
    description: "Welcome-saturated, cold-remail guards, recent-sent windows.",
    badgeClass: "bg-rose-500/15 text-rose-300 border-rose-400/40",
    defaultInclusionVisible: false,
    defaultExclusionVisible: true,
  },
  {
    id: "partner_wave_static",
    label: "Partner Wave (one-shot)",
    shortLabel: "Partner",
    description: "Auto-generated per-wave segments from partner_drip_orchestrator. Archived after 7 days when no active campaign references them.",
    badgeClass: "bg-slate-500/15 text-slate-300 border-slate-400/40",
    defaultInclusionVisible: false,
    defaultExclusionVisible: false,
  },
  {
    id: "legacy_snapshot",
    label: "Legacy Snapshot",
    shortLabel: "Legacy",
    description: "Date-stamped historical snapshots. Kept for audit; do not target in new campaigns.",
    badgeClass: "bg-zinc-500/15 text-zinc-400 border-zinc-500/40",
    defaultInclusionVisible: false,
    defaultExclusionVisible: false,
  },
  {
    id: "uncategorized",
    label: "Uncategorized",
    shortLabel: "?",
    description: "No semantic category yet. Add a backfill rule in main.go if a pattern exists.",
    badgeClass: "bg-neutral-700/30 text-neutral-400 border-neutral-600/40",
    defaultInclusionVisible: true,
    defaultExclusionVisible: false,
  },
];

export const SEGMENT_CATEGORIES_BY_ID: Record<SegmentCategory, SegmentCategoryMeta> =
  SEGMENT_CATEGORIES.reduce((acc, c) => {
    acc[c.id] = c;
    return acc;
  }, {} as Record<SegmentCategory, SegmentCategoryMeta>);

export function getCategoryMeta(id: string | undefined | null): SegmentCategoryMeta {
  if (!id) return SEGMENT_CATEGORIES_BY_ID.uncategorized;
  return (
    SEGMENT_CATEGORIES_BY_ID[id as SegmentCategory] ??
    SEGMENT_CATEGORIES_BY_ID.uncategorized
  );
}

export function defaultVisibleCategoriesForPicker(
  mode: "inclusion" | "exclusion",
): Set<SegmentCategory> {
  const flag = mode === "inclusion" ? "defaultInclusionVisible" : "defaultExclusionVisible";
  return new Set(
    SEGMENT_CATEGORIES.filter((c) => c[flag]).map((c) => c.id),
  );
}
