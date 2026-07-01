# PORTAL DESIGN SYSTEM — display, density, and the unified filter standard

**Status: BINDING for all portal screens** (operator directive 2026-07-01). This doc exists to
kill two failure modes: (1) information-dense components formatted so poorly they hurt the user
experience, and (2) one-off implementations — every screen inventing its own filters, tables,
tiles, and colors. Companion: `METRIC_CONTRACT.md` (what the numbers MEAN); this doc is how they
LOOK and how users narrow them.

The **Delivery Queue (OutboxDashboard) is the gold standard**. Its design system is extracted to
`web/src/components/mailing/shared/theme.ts` (tokens + style objects) and
`web/src/components/mailing/shared/ui.tsx` (Panel, SectionHeader, Stat, Pill, LivePill,
ProgressBar, SectionError, EmptyState). Screens compose from these — never re-derive inline colors
or panel styles. Migration precedent: AudienceAnalytics, AudienceCadenceByCell, SegmentsCenter,
EventLakeExplorer (2026-07-01) — all remap their local COLORS onto `theme.colors`.

---

## 1. Layout & hierarchy — the anti-density rules

A screen answers ONE question at the top and lets the user drill down. Concretely:

1. **Max ~8 KPI tiles above the fold.** More than that = the screen has no point of view. Extra
   numbers go into tables, expansions, or tooltips — not more tiles.
2. **Three levels of information, in order:** headline KPIs (the answer) → one trend/comparison
   visual → detail tables (the evidence). Never lead with a wall of table.
3. **Progressive disclosure over density.** Detail belongs in: row expansions (chevron rows),
   tooltips (`title=`), and drill-down views — not in more columns. A table wider than ~12 columns
   must justify every column or move some behind an expansion.
4. **Every table: totals row pinned, sortable headers (that actually sort — pass extraCols to the
   sorter), tabular-nums right-aligned numerics, row hover, horizontal scroll wrapper.** All-16-
   brands surfaces scroll horizontally rather than truncate (operator standing rule, 2026-06-20).
5. **Every number that is a rate discloses its denominator** (tooltip or subtext) and every
   derived/approximate number carries a `*` + explanation (`Attempted*`, `Delivered*`). If a
   metric can't honor the active filters, SAY SO on the tile ("ALL ISPs — isp filter not
   applied"), never render a silently-wrong number (Metric Contract §2).
6. **States are designed, not accidental:** loading = spinner row with a label; empty = friendly
   `EmptyState` with a hint about filters; error = inline `SectionError`/error shell WITH a Retry
   button, per-section (one failed panel never blanks the screen — fail-soft like the Overview's
   engagement fetch). Every data panel shows its fetch timing ("fetched HH:MM:SS · Nms · cached").
7. **Long text truncates with full value in `title=`**; UUIDs render monospace-truncated with the
   full id in the tooltip; prefer names over ids everywhere the name is known.

## 2. Color & typography

- Tokens come from `shared/theme.ts` ONLY: indigo accent family, slate-blue panels, semantic
  success/warning/danger. Append-alpha idiom: `alpha(color,'22')` bg / `'66'` border.
- **HARD RULES (never violate):** hard bounce `#ef4444` red, soft bounce `#f59e0b` amber, never
  combined into one number. State keywords color via `stateColor()` so pass/fail/storm/sending
  are identical across screens.
- Numbers: `fontVariantNumeric: 'tabular-nums'` always. Section titles: uppercase 13px
  (`panelTitleStyle`). Explanatory subtext: 11-12px `textMuted`, complete sentences.
- Charts: recharts, dark grid `rgba(255,255,255,0.06)`, series colors from the shared palette
  (opens cyan `#22d3ee`, clicks violet `#a78bfa`, complaints rose `#fb7185`, info blue `#60a5fa`),
  compact tick formatting, custom dark tooltip. Series toggles as chips.

## 3. THE UNIFIED FILTER STANDARD — no one-off filters

Filtering is a PRODUCT surface, not a per-screen widget. One shared filter bar
(`web/src/components/mailing/shared/FilterBar.tsx`) with these invariants:

1. **One vocabulary, everywhere:** Date range (Denver presets: Today / Yesterday / 7D / 14D /
   30D + custom), Brand (apex domain, datalist of the 16 active brands), Mailbox Provider (clean
   ISP classifier values: gmail/microsoft/yahoo/apple/comcast/aol/att/charter/…), Transport
   (Combined / MTA / SES), and where relevant Route type. A screen exposes a SUBSET of these —
   never a different spelling of them.
2. **One semantics mapping:** the bar emits the canonical query params (`from`/`to` Denver days,
   `brand` apex, `isp` clean, `source`/`source_in` for transport) — the same names the backend
   whitelists. No screen invents its own param names or its own date math (`daysAgoDenver` /
   `denverToday` live in shared).
3. **Draft → Apply (Run) model** for expensive queries: edits are a draft; an explicit Run applies
   them and bypasses caches (nonce bump). Cheap PG screens may apply live — but the CONTROLS look
   the same.
4. **Active filters render as removable chips** under the bar, one style (shared Chip), so the
   user always sees what scope they're in. The applied range is always visible ("2026-06-25 →
   2026-07-01 · America/Denver").
5. **Filters must actually bind.** If a panel on the screen cannot honor an active filter, the
   panel says so explicitly (see Metric Contract §2). A filter that silently doesn't apply to part
   of the screen is a bug (the Overview opens-rate >100% incident, the dropped-`isp` Eq incident —
   both 2026-07-01).
6. **Persistence:** last-used range/filters may persist per-screen in localStorage under
   `<screen>-filters`, guarded parsing, never trusted blindly.

## 4. Data-fetch conventions (the shared hook contract)

- All requests through `shared/apiFetch.ts` (org header + credentials). Never bare `fetch`, never
  a hardcoded host.
- Expensive analytics fetches: module-level FIFO-capped cache keyed by full URL + run-nonce
  (pattern in EventLakeExplorer `fetchBreakdown`); explicit Refresh bypasses the cache — and must
  bypass it for EVERY panel on the screen, including secondary effects (the frozen-hourly-trend
  bug, 2026-07-01).
- Every fetch: AbortController on unmount/re-run; `Promise.allSettled` for independent panels
  (fail-soft per §1.6); truncation banners when the server clamps a result.
- Tabs stay mounted once visited (display:none), so state and caches survive tab flips.

## 5. Screen assembly checklist (apply on every new/edited screen)

1. Page wrapper `pageStyle`; panels via `Panel`/`panelStyle`; titles via `SectionHeader`.
2. Filters via the shared FilterBar with canonical params — no bespoke filter rows.
3. Numbers follow METRIC_CONTRACT.md (sources, denominators, labels); every rate has a
   denominator note; derived values carry `*`.
4. KPI count ≤ ~8; hierarchy KPIs → trend → tables; detail behind expansions/tooltips.
5. All four states designed (loading/empty/error/success) + per-panel timing note.
6. Colors/typography from theme tokens; hard/soft bounce hard rule.
7. `npx tsc --noEmit` + `npm run build` green; verify numbers reconcile with the Reporting screen
   for the same window before shipping (Metric Contract §8).
8. `PAGE_VERSION` bumped with a comment on behavior changes.

## Amendment log

- 2026-07-01: initial version — extracted from the Delivery Queue gold standard, the
  shared/theme.ts + ui.tsx kit, and the Reporting-screen unification work.
