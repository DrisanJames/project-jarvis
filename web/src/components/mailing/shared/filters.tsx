// filters.tsx — THE unified filter layer for the Mailing Portal.
//
// This is the shared implementation of PORTAL_DESIGN_SYSTEM.md §3 ("THE
// UNIFIED FILTER STANDARD — no one-off filters"), extracted verbatim from the
// Event Lake Explorer's gold-standard Toolbar (the first consumer). Every
// screen that filters by date range / brand / mailbox provider / transport /
// route type composes from these exports — one vocabulary, one param mapping,
// one Draft → Run model, one chip style. No screen re-invents its own date
// math or param names.
//
// Invariants (design system §3):
//   1. Date presets are America/Denver operating days (Today/Yesterday/7D/14D/30D).
//   2. The bar emits the canonical backend query params: from/to Denver days,
//      brand (apex), isp_group (clean classifier), route_type, and
//      source=pmta|ses / source_in=pmta,ses for the transport selector.
//   3. Edits are a DRAFT; an explicit Run applies them (AppliedLakeFilters.nonce
//      bumps to bypass module caches). The cache key is the serialized URL, so
//      param mapping lives HERE and nowhere else.
//   4. Active filters render as removable chips under the bar.

import React from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faSearch, faTimes } from '@fortawesome/free-solid-svg-icons';
import { colors as theme } from './theme';

// ═══════════════════════════════════════════════════════════════════════════
// TYPES
// ═══════════════════════════════════════════════════════════════════════════

export type RouteTypeFilter = '' | 'pmta_direct' | 'ses' | 'ses_tenant';

// Transport selector — which sending ENVIRONMENT the data covers (operator
// 2026-10-28: "we only have SES and MTA — include the new lanes"). The backend
// vocabulary is GET /api/mailing/analytics/lake/lanes (lake_lanes.go):
//   combined → every real transport (source_in=pmta,ses,kumo; never 'app')
//   ses      → SES account truth (source=ses)
//   mta      → both PMTA boxes (source=pmta)
//   pmta_a / pmta_b → one PMTA box (lane=…: source pmta + that box's VMTA codes)
//   kumo     → both Kumo boxes (source=kumo)
//   colo1 / colo2   → one Kumo box (lane=…: source kumo + that box's VMTA codes)
// Gmail is not a transport in the lake (gm./gt. senders relay through SES and
// SES rows carry no sending domain) — use the ISP filter (gmail) for it.
export type Transport = 'combined' | 'ses' | 'mta' | 'pmta_a' | 'pmta_b' | 'kumo' | 'colo1' | 'colo2';

export const TRANSPORT_OPTIONS: { id: Transport; label: string }[] = [
  { id: 'combined', label: 'Combined' },
  { id: 'ses', label: 'SES' },
  { id: 'mta', label: 'MTA' },
  { id: 'pmta_a', label: 'PMTA A' },
  { id: 'pmta_b', label: 'PMTA B' },
  { id: 'kumo', label: 'Kumo' },
  { id: 'colo1', label: 'Colo 1' },
  { id: 'colo2', label: 'Colo 2' },
];
export const transportLabel = (t: Transport): string => TRANSPORT_OPTIONS.find((o) => o.id === t)?.label ?? t;
// A box lane scopes rows to ONE sending box (by VMTA code) — it is a scope on
// top of the transport, so companion queries that otherwise ignore the
// transport selector still honor it.
export const isBoxLane = (t: Transport): boolean => t === 'pmta_a' || t === 'pmta_b' || t === 'colo1' || t === 'colo2';
// The lake `source` values a transport selection reads.
export function transportSources(t: Transport): Set<string> {
  if (t === 'ses') return new Set(['ses']);
  if (t === 'mta' || t === 'pmta_a' || t === 'pmta_b') return new Set(['pmta']);
  if (t === 'kumo' || t === 'colo1' || t === 'colo2') return new Set(['kumo']);
  return new Set(['pmta', 'ses', 'kumo']);
}

export interface LakeFilterDraft {
  from: string;
  to: string;
  ispGroup: string;
  brand: string;
  routeType: RouteTypeFilter;
  transport: Transport;
}

export interface AppliedLakeFilters extends LakeFilterDraft {
  nonce: number; // bumped on every explicit Run — bypasses the module cache
}

// ═══════════════════════════════════════════════════════════════════════════
// DATE HELPERS — America/Denver operating days
// ═══════════════════════════════════════════════════════════════════════════

// Operating-day helpers — report the day in America/Denver (MST/MDT), the
// operator's send-day timezone. denverToday() returns the current Denver day as
// 'YYYY-MM-DD'; daysAgoDenver(n) walks back n Denver days. The noon-UTC anchor
// in daysAgoDenver keeps the arithmetic DST-safe (no day-boundary slips).
export const denverToday = () =>
  new Intl.DateTimeFormat('en-CA', { timeZone: 'America/Denver' }).format(new Date());

export const daysAgoDenver = (n: number) => {
  const t = new Date();
  const denver = new Intl.DateTimeFormat('en-CA', { timeZone: 'America/Denver' }).format(t);
  const base = new Date(denver + 'T12:00:00Z');
  base.setUTCDate(base.getUTCDate() - n);
  return new Intl.DateTimeFormat('en-CA', { timeZone: 'America/Denver' }).format(base);
};

// The standard Denver date-range presets (design system §3.1).
export const DENVER_PRESETS: Array<{ label: string; from: () => string; to: () => string }> = [
  { label: 'Today', from: () => denverToday(), to: () => denverToday() },
  { label: 'Yesterday', from: () => daysAgoDenver(1), to: () => daysAgoDenver(1) },
  { label: '7D', from: () => daysAgoDenver(6), to: () => denverToday() },
  { label: '14D', from: () => daysAgoDenver(13), to: () => denverToday() },
  { label: '30D', from: () => daysAgoDenver(29), to: () => denverToday() },
];

// ═══════════════════════════════════════════════════════════════════════════
// PARAM MAPPERS — filters → canonical backend query params
// ═══════════════════════════════════════════════════════════════════════════

// Filters → breakdown filter params, WITHOUT the transport→source mapping.
// Used by companion queries that show the route split (direct vs SES relay)
// side by side and must read every transport regardless of the selector
// (otherwise picking MTA would empty the SES side). A BOX lane is different: it
// is a scope (one sending box), so it is carried here too — the route split
// then shows that box's own direct-vs-relay mix.
export function lakeFilterParamsNoTransport(f: LakeFilterDraft): Record<string, string> {
  const out: Record<string, string> = {};
  if (f.ispGroup.trim()) out.isp_group = f.ispGroup.trim();
  if (f.brand.trim()) out.brand = f.brand.trim();
  if (f.routeType) out.route_type = f.routeType;
  if (isBoxLane(f.transport)) out.lane = f.transport;
  return out;
}

// Filters → breakdown filter params (transport-aware). The transport selector
// maps to the backend contract: source=pmta|ses|kumo for a whole transport,
// lane=<box> for one box (the backend adds the source + VMTA-code scope), or
// source_in=pmta,ses,kumo for Combined. Threading transport through here keeps
// the cache key (built from the serialized URL) correct automatically.
export function lakeFilterParams(f: LakeFilterDraft): Record<string, string> {
  const out = lakeFilterParamsNoTransport(f);
  if (isBoxLane(f.transport)) return out; // lane already set
  if (f.transport === 'mta') out.source = 'pmta';
  else if (f.transport === 'ses') out.source = 'ses';
  else if (f.transport === 'kumo') out.source = 'kumo';
  else out.source_in = 'pmta,ses,kumo'; // combined = every real transport
  return out;
}

// ═══════════════════════════════════════════════════════════════════════════
// OPTION LISTS — one vocabulary, everywhere
// ═══════════════════════════════════════════════════════════════════════════

// The 16 active brands (apex domains) — mirrors brand_metadata.py, the
// canonical registry. Datalist suggestions for the Brand field.
export const BRAND_OPTIONS = [
  'discountblog.com',
  'historythinking.com',
  'myownhealth.net',
  'quizfiesta.com',
  'businessweeklypro.com',
  'financialcalculate.com',
  'consumerpro.net',
  'homewarrantyservices.org',
  'refinanceratesusa.com',
  'thingoftheday.org',
  'yourinsurancehub.com',
  'myrepairdiy.com',
  'casainsure.com',
  'learnpersonalloans.com',
  'ratesbazar.com',
  'warrantyforyou.com',
];

// Clean ISP classifier values — datalist suggestions for the Mailbox Provider field.
export const COMMON_ISP_GROUPS = ['gmail', 'yahoo', 'microsoft', 'aol', 'comcast', 'charter', 'att', 'verizon', 'other'];

// ═══════════════════════════════════════════════════════════════════════════
// STYLES — the shared filter-bar look (theme tokens; matches the Event Lake
// Explorer / Delivery Queue panel chrome exactly)
// ═══════════════════════════════════════════════════════════════════════════

const INFO_BLUE = 'var(--j-blue)'; // route_type chip tone (shared chart palette blue)

const fstyles: Record<string, React.CSSProperties> = {
  toolbar: {
    background: theme.panelBgSolid, border: `1px solid ${theme.hairline}`,
    borderRadius: 10, padding: '14px 18px', marginBottom: 16,
  },
  toolbarRow: {
    display: 'flex', alignItems: 'flex-end', gap: 12, flexWrap: 'wrap',
  },
  presetRow: { display: 'flex', gap: 6, alignItems: 'flex-end', paddingBottom: 1 },
  presetBtn: {
    border: '1px solid', borderRadius: 999, padding: '6px 12px',
    fontSize: 12, fontWeight: 600, cursor: 'pointer', background: 'transparent',
    whiteSpace: 'nowrap',
  },
  chipRow: {
    display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', marginTop: 12,
  },
  chipRowLabel: {
    fontSize: 11, color: theme.textFaint, textTransform: 'uppercase', letterSpacing: 0.5,
  },
  chip: {
    display: 'inline-flex', alignItems: 'center', gap: 6,
    border: '1px solid', borderRadius: 999, padding: '4px 10px',
    fontSize: 12, fontWeight: 500, fontVariantNumeric: 'tabular-nums',
  },
  chipX: {
    background: 'transparent', border: 'none', color: 'inherit',
    cursor: 'pointer', fontSize: 10, padding: 0, display: 'inline-flex',
  },
  fieldLabel: {
    display: 'flex', flexDirection: 'column', gap: 4,
    fontSize: 10, color: theme.textFaint, textTransform: 'uppercase', letterSpacing: 0.5,
  },
  input: {
    background: theme.appBgSolid, color: theme.text,
    border: `1px solid ${theme.panelBorderStrong}`, borderRadius: 6,
    padding: '7px 10px', fontSize: 13, outline: 'none',
  },
  segmented: {
    display: 'inline-flex', background: theme.appBgSolid,
    border: `1px solid ${theme.panelBorderStrong}`, borderRadius: 6, overflow: 'hidden',
  },
  segmentedBtn: {
    border: 'none', borderRight: `1px solid ${theme.hairline}`,
    padding: '7px 12px', fontSize: 13, cursor: 'pointer',
    outline: 'none', whiteSpace: 'nowrap',
  },
  primaryBtn: {
    background: theme.indigo400, color: 'var(--j-bg)',
    border: 'none', padding: '8px 16px', borderRadius: 6,
    fontSize: 13, fontWeight: 600, cursor: 'pointer',
    display: 'flex', alignItems: 'center', gap: 8, height: 34,
  },
};

// Field + input styles, exported so screen-specific fields passed through the
// FilterBar's `extraFields` slot render identically to the built-in ones.
export const filterFieldLabelStyle: React.CSSProperties = fstyles.fieldLabel;
export const filterInputStyle: React.CSSProperties = fstyles.input;

// ═══════════════════════════════════════════════════════════════════════════
// COMPONENTS
// ═══════════════════════════════════════════════════════════════════════════

// Removable filter chip (design system §3.4) — one chip style portal-wide.
export const FilterChip: React.FC<{ label: string; onRemove?: () => void; tone?: string }> = ({ label, onRemove, tone }) => (
  <span style={{ ...fstyles.chip, borderColor: `color-mix(in srgb, ${(tone || theme.indigo400)} 33%, transparent)`, color: tone || theme.indigo400 }}>
    {label}
    {onRemove && (
      <button style={fstyles.chipX} onClick={onRemove} title="Remove filter">
        <FontAwesomeIcon icon={faTimes} />
      </button>
    )}
  </span>
);

// Which fields the consuming screen exposes. Omit the prop entirely for the
// full bar; pass a subset to hide fields (a screen exposes a SUBSET of the
// shared vocabulary — never a different spelling of it, design system §3.1).
export interface FilterBarShow {
  brand?: boolean;
  isp?: boolean;
  routeType?: boolean;
  transport?: boolean;
}

const SHOW_ALL: FilterBarShow = { brand: true, isp: true, routeType: true, transport: true };

// The unified filter bar: Denver presets + date inputs + the enabled fields +
// Run button + active-filter chips. Draft → Run model — edits mutate `draft`;
// Run calls onRun(draft) and the caller applies it (bumping the nonce).
export const FilterBar: React.FC<{
  draft: LakeFilterDraft;
  setDraft: React.Dispatch<React.SetStateAction<LakeFilterDraft>>;
  applied: AppliedLakeFilters;
  onRun: (next: LakeFilterDraft) => void;
  show?: FilterBarShow;
  activeLabel?: string; // chip-row scope label, e.g. 'Active (Overview & Dimensions):'
  // State-based surfaces (e.g. a review queue) hide the date controls: a date
  // window there would silently drop the oldest waiting item.
  hideDates?: boolean;
  // Screen-specific fields rendered INSIDE the one bar (use
  // filterFieldLabelStyle / filterInputStyle) and their chips in the same chip
  // row — so a screen with its own dimensions still has ONE bar, ONE chip style.
  extraFields?: React.ReactNode;
  extraChips?: Array<{ label: string; tone?: string; onRemove?: () => void }>;
}> = ({ draft, setDraft, applied, onRun, show, activeLabel, hideDates, extraFields, extraChips }) => {
  const on = show ?? SHOW_ALL;
  const uid = React.useId(); // unique datalist ids — the bar can mount more than once per page

  const chips: Array<{ label: string; tone?: string; onRemove?: () => void }> = hideDates ? [] : [
    { label: `${applied.from} → ${applied.to}` },
  ];
  if (on.isp && applied.ispGroup.trim()) chips.push({
    label: `isp_group=${applied.ispGroup.trim()}`, tone: theme.indigo300,
    onRemove: () => { const next = { ...draft, ispGroup: '' }; setDraft(next); onRun(next); },
  });
  if (on.brand && applied.brand.trim()) chips.push({
    label: `brand=${applied.brand.trim()}`, tone: theme.indigo200,
    onRemove: () => { const next = { ...draft, brand: '' }; setDraft(next); onRun(next); },
  });
  if (on.routeType && applied.routeType) chips.push({
    label: `route_type=${applied.routeType}`, tone: INFO_BLUE,
    onRemove: () => { const next = { ...draft, routeType: '' as RouteTypeFilter }; setDraft(next); onRun(next); },
  });
  if (on.transport && applied.transport !== 'combined') chips.push({
    label: `transport=${transportLabel(applied.transport)}`, tone: theme.success,
    onRemove: () => { const next = { ...draft, transport: 'combined' as Transport }; setDraft(next); onRun(next); },
  });
  if (extraChips) chips.push(...extraChips);

  return (
    <div style={fstyles.toolbar}>
      <div style={fstyles.toolbarRow}>
        {!hideDates && (<>
        <div style={fstyles.presetRow}>
          {DENVER_PRESETS.map((p) => {
            const active = draft.from === p.from() && draft.to === p.to();
            return (
              <button
                key={p.label}
                style={{
                  ...fstyles.presetBtn,
                  color: active ? theme.indigo400 : theme.textMuted,
                  borderColor: active ? `color-mix(in srgb, ${theme.indigo400} 40%, transparent)` : theme.panelBorderStrong,
                  background: active ? `color-mix(in srgb, ${theme.indigo400} 8%, transparent)` : 'transparent',
                }}
                onClick={() => setDraft((d) => ({ ...d, from: p.from(), to: p.to() }))}
              >
                {p.label}
              </button>
            );
          })}
        </div>
        <label style={fstyles.fieldLabel}>From
          <input type="date" value={draft.from} max={draft.to}
            onChange={(e) => setDraft((d) => ({ ...d, from: e.target.value }))} style={fstyles.input} />
        </label>
        <label style={fstyles.fieldLabel}>To
          <input type="date" value={draft.to} min={draft.from} max={denverToday()}
            onChange={(e) => setDraft((d) => ({ ...d, to: e.target.value }))} style={fstyles.input} />
        </label>
        </>)}
        {on.isp && (
          <>
            <label style={fstyles.fieldLabel}>isp_group
              <input type="text" list={`${uid}-isp-groups`} value={draft.ispGroup} placeholder="all"
                onChange={(e) => setDraft((d) => ({ ...d, ispGroup: e.target.value }))}
                style={{ ...fstyles.input, width: 120 }} />
            </label>
            <datalist id={`${uid}-isp-groups`}>
              {COMMON_ISP_GROUPS.map((g) => <option key={g} value={g} />)}
            </datalist>
          </>
        )}
        {on.brand && (
          <>
            <label style={fstyles.fieldLabel}>brand
              <input type="text" list={`${uid}-brands`} value={draft.brand} placeholder="all"
                onChange={(e) => setDraft((d) => ({ ...d, brand: e.target.value }))}
                style={{ ...fstyles.input, width: 150 }} />
            </label>
            <datalist id={`${uid}-brands`}>
              {BRAND_OPTIONS.map((b) => <option key={b} value={b} />)}
            </datalist>
          </>
        )}
        {on.routeType && (
          <label style={fstyles.fieldLabel}>route_type
            <select value={draft.routeType}
              onChange={(e) => setDraft((d) => ({ ...d, routeType: e.target.value as RouteTypeFilter }))}
              style={{ ...fstyles.input, width: 130 }}>
              <option value="">all</option>
              <option value="pmta_direct">pmta_direct</option>
              <option value="ses">ses</option>
              <option value="ses_tenant">ses_tenant</option>
            </select>
          </label>
        )}
        {on.transport && (
          <div style={fstyles.fieldLabel}>Transport
            <div style={fstyles.segmented}>
              {TRANSPORT_OPTIONS.map((opt) => {
                const active = draft.transport === opt.id;
                return (
                  <button
                    key={opt.id}
                    type="button"
                    onClick={() => setDraft((d) => ({ ...d, transport: opt.id }))}
                    style={{
                      ...fstyles.segmentedBtn,
                      color: active ? theme.indigo400 : theme.textMuted,
                      background: active ? `color-mix(in srgb, ${theme.indigo400} 12%, transparent)` : 'transparent',
                      fontWeight: active ? 600 : 400,
                    }}
                  >
                    {opt.label}
                  </button>
                );
              })}
            </div>
          </div>
        )}
        {extraFields}
        <button style={fstyles.primaryBtn} onClick={() => onRun(draft)}>
          <FontAwesomeIcon icon={faSearch} /> Run
        </button>
      </div>
      <div style={fstyles.chipRow}>
        <span style={fstyles.chipRowLabel}>{activeLabel ?? 'Active:'}</span>
        {chips.length === 0 && <span style={{ fontSize: 12, color: theme.textFaint }}>none</span>}
        {chips.map((c, i) => <FilterChip key={i} label={c.label} tone={c.tone} onRemove={c.onRemove} />)}
      </div>
    </div>
  );
};
