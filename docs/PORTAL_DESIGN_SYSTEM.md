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

## 2. Color, type & mode — the Jarvis brand (2026-10-29)

The portal wears the marketing site's brand (`web/src/components/marketing/`), in **light and dark**.

- **Every colour is a `--j-*` token** from `web/src/styles/jarvis-tokens.css`. In TSX import
  `colors` / `alpha()` / `gradients` from `shared/theme.ts` (they resolve to `var(--j-*)`); in CSS
  write `var(--j-*)`. **No hex or rgba literals in new code** — a literal is one mode's colour and
  breaks the other.
- **Mode** = `<html data-theme="light|dark">`. `index.html` sets it before first paint from
  `localStorage('jarvis_mode')`; no saved choice → OS preference. Toggle: sun/moon button in the
  sidebar header (`shared/colorMode.ts` → `useColorMode()`).

| Token | Dark | Light | Use |
|---|---|---|---|
| `bg` / `bg-grad` | `#14112b` ink | `#fff8ee` cream | page |
| `surface` · `surface-2` · `surface-3` | `#1c1838` · `#242046` · `#302a5c` | `#fff` · `#fbf6ee` · `#f1e9ff` | panels · inputs/rows · selected |
| `shade` | `#0d0b20` | `#ede5f7` | wells, insets (always via `color-mix`) |
| `border` · `border-subtle` · `border-strong` | `#3d3770` · `#2b2650` · `#666097` | `#ddd3ee` · `#ece5f5` · `#958bab` | lines only — never text; `border-strong` outlines form controls (3:1) |
| `text` · `text-2` · `text-muted` · `text-faint` | `#fff8ee` → `#9b96b8` | `#14112b` → `#66607a` | headings/numbers → captions |
| `on-accent` | `#fff` | `#fff` | text on a filled accent (buttons, active pills) |
| `violet` · `violet-strong` · `violet-soft` | `#b7a3ff` · `#6d28ff` · `#cbbcff` | `#5920d1` · `#6d28ff` · `#5420d0` | primary accent, active nav, links |
| `pink` · `cyan` · `blue` | `#ff84bd` · `#19d3f0` · `#72bbff` | `#980b4c` · `#005a68` · `#14519a` | secondary accents, chart series |
| `success` · `warn` · `danger` (+ `-soft` text) | lime `#c6f432` · `#ffb547` · `#ff869c` | `#2e5d03` · `#853d00` · `#9a0a2b` | semantic |
| `hard-fill` · `soft-fill` | `#ef4444` · `#f59e0b` | `#ef4444` · `#f59e0b` | hard / soft bounce: bars, dots, chart series, tints |
| `hard` · `soft` | `#f47f7f` · `#f5a51d` | `#942929` · `#744800` | hard / soft bounce: text |

- Accents carry a **per-mode value** so the same token reads as text on cream and on ink.
- Translucent fills: `alpha(c,'22')` bg / `'66'` border → `color-mix(in srgb, c N%, transparent)`.
  Hex-suffix concatenation (`c + '22'`, `` `${c}22` ``) does **not** work on tokens — never use it.
- **HARD RULES (never violate):** hard bounce is always red, soft bounce always amber, never
  combined into one number. Anything that signals by colour alone (bars, dots, chart series, tints)
  uses `--j-hard-fill` / `--j-soft-fill` (`colors.dangerFill` / `warningFill`): exactly `#ef4444` /
  `#f59e0b` in both modes. Text uses `--j-hard` / `--j-soft` (`colors.danger` / `warning`), the same
  hues adjusted per mode to pass AA. State keywords colour via `stateColor()` so
  pass/fail/storm/sending are identical across screens.
- **Accessibility (WCAG 2.1 AA, both modes):**
  - Every text token ≥ 4.5:1 on every surface and on a 30% tint of itself. Never dim text with
    `color-mix` or `opacity` — pick `text-2`, `text-muted` or `text-faint`.
  - Arbitrary colours (ISP brands, series) as text go through `Pill`, which pulls the label toward
    `--j-text`. Don't put a raw brand hex on a tint of itself.
  - Every `input`, `select`, `textarea` and icon-only `button` has an accessible name: a `<label>`,
    or `aria-label` matching the visible label.
  - Form controls outline in `border-strong` (3:1). Horizontally scrolling regions get `tabIndex={0}`.
  - A card that holds controls is never itself a `role="button"`: keep the mouse click on the card
    and add an overlay `<button>` with `pointerEvents: 'none'` for keyboard users.
  - Verify with axe-core on real data, in both modes, before calling a palette change done.
- **Type:** headings Bricolage Grotesque (`--j-font-display`, 700–800, tight tracking); everything
  else Inter (`--j-font-body`). Numbers `fontVariantNumeric: 'tabular-nums'` always. Section titles
  uppercase 13px (`panelTitleStyle`). Explanatory subtext 11–12px `textMuted`, complete sentences.
- **Shape:** panels 14px radius + `--j-shadow`; buttons, nav items and sub-nav tabs are pills
  (`999px`); the active nav item is a violet-tint pill with violet text and a pink icon.
- **Logo:** `JarvisMark` (`components/marketing/JarvisLogo.tsx`, motion in `JarvisLogo.css`) in the
  sidebar header and on the sign-in card — never a generic robot icon.
- **Charts:** recharts; grid `color-mix(var(--j-text) 6%)`; series opens `cyan`, clicks `violet`,
  complaints `danger`, info `blue` (all tokens, so they adapt per mode); compact ticks; tooltip on
  `surface` with `border`. Series toggles as chips.

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

## 6. Labelled numbers, unknown-vs-zero, and drill-down sections (REQ-118, 2026-09-03)

Added with the Supply tab (`components/Supply*.tsx`), and binding on any screen that projects a
mediator's own records rather than raw event counts.

1. **A number carries its kind, not just its value.** Where the backend ships a `labels` map naming
   each figure (`contracted | effective | planned | reserved | actual | forecast`), the screen
   renders that label — as a chip on a KPI tile, in the `title=` of every table cell — and NEVER
   invents one. Two figures with the same magnitude and different labels are different facts.
2. **`null` renders as "unknown", muted and italic. Never 0, never "—" where a count belongs.**
   "Never built", "built empty", "measured zero" and "could not measure" are four different displays.
   When the API names what it could not compute (a `degraded[]` array), the screen prints those lines
   verbatim rather than showing a plausible-looking blank.
3. **`as_of`, data freshness and contract/config versions live in a header strip on every pane** of
   such a screen, not only on the first one. A number whose moment and governing policy version are
   off-screen is not decision-grade.
4. **A verdict computed server-side is displayed, not recomputed.** Health colours, statuses and
   bands come from the API field; the UI maps the string to a theme token. Two implementations of a
   rule is two rules.
5. **Progressive drill-down beats a second filter.** A multi-pane section narrows by CLICKING the row
   (queue → entity → records), and the active narrowing shows as a shared `FilterChip` with an ✕.
   Do not add a bespoke entity picker beside the shared FilterBar — §3 still holds.
6. **Single-day surfaces say so.** A screen that reads one Denver day composes the shared FilterBar
   (which is a range bar), uses the `To` day, and prints a warning when a wider range was applied —
   it never silently averages or silently truncates the range.

## Amendment log

- 2026-07-01: initial version — extracted from the Delivery Queue gold standard, the
  shared/theme.ts + ui.tsx kit, and the Reporting-screen unification work.
- 2026-09-03: §6 added with the REQ-118 Supply tab — display the API's label vocabulary rather than
  inventing one; null renders as "unknown", never 0, and `degraded[]` notes print verbatim; `as_of` +
  freshness + contract versions on EVERY pane; server-computed verdicts (health colour) are mapped,
  never recomputed; drill-down instead of a bespoke second filter, with the shared FilterChip showing
  the active narrowing; single-day screens use the shared range bar's `To` day and warn when a wider
  range was applied.
- 2026-10-29: §2 rewritten: portal restyled to the Jarvis marketing brand with light/dark mode. All portal colour literals codemodded to `--j-*` tokens (`styles/jarvis-tokens.css`); `theme.ts` resolves to tokens and `alpha()` is `color-mix`; legacy `index.css` vars alias the tokens; sidebar + sign-in carry `JarvisMark`. Supersedes the indigo/slate palette.
- 2026-10-30: §2 accessibility pass (operator). Light tokens darkened to AA including on self-tints; `--j-hard` / `--j-soft` added (literals kept in dark); translucent text replaced with solid tokens; accessible names added to controls; rules recorded in §2.
- 2026-10-30: dark mode brought to AA; bounce colours split into `-fill` (exact literals, both modes) and text tokens; Drip Journey touch cards no longer nest interactive controls.
