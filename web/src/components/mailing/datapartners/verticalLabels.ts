// Shared vertical-slug → display-label mapping for the Data Partners screens.
// ONE source of truth: both the landing drip-state cards (PartnerIngestPortal)
// and the drip-performance panel import from here so the same slug never gets
// two different labels on the same screen.

export const VERTICAL_LABEL: Record<string, string> = {
  refi_heloc: 'Refi / HELOC',
  personal_loans: 'Personal Loans',
  tax_relief: 'Tax Relief',
  remodel: 'Remodel',
  direct_offer: 'Direct Offer',
  samsclub_internal: "Sam's Club (internal)",
  clickers_samsclub: "Sam's Club (clickers)",
  metal_roofing_signal: 'Metal Roofing (signal)',
  auto_coverage_internal: 'Auto Coverage (internal)',
};

// labelForVertical resolves a vertical slug to a display label. Mapped slugs win.
// A "<vertical>:governed" governed-pass pointer has no direct map entry: strip the
// trailing ":governed", RE-LOOK-UP the base slug in VERTICAL_LABEL first (so
// samsclub_internal:governed → "Sam's Club (internal) (governed)", not a raw
// humanize), and only humanize when the base is also unmapped. Remaining
// underscores/colons become spaces with each word title-cased —
// so `direct_offer:governed` → "Direct Offer (governed)".
export const labelForVertical = (slug: string): string => {
  const mapped = VERTICAL_LABEL[slug];
  if (mapped) return mapped;
  let rest = slug;
  let governed = false;
  if (rest.endsWith(':governed')) { rest = rest.slice(0, -':governed'.length); governed = true; }
  // Re-check the base slug against the map before falling back to humanize.
  const base = VERTICAL_LABEL[rest] ?? rest
    .replace(/[_:]+/g, ' ')
    .trim()
    .replace(/\b\w/g, c => c.toUpperCase());
  return governed ? `${base} (governed)` : base;
};
