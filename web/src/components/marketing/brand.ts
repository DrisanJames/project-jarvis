// Jarvis brand tokens + site map. The brand page (/brand) renders from these,
// so the guideline and the site can never drift apart.

export const CONTACT_EMAIL = 'hello@projectjarvis.io';

export interface Swatch {
  name: string;
  hex: string;
  role: string;
  ink: string; // text colour that passes contrast on this swatch
}

export const PALETTE: Swatch[] = [
  { name: 'Jarvis Violet', hex: '#6D28FF', role: 'Primary. The agent itself — logo, links, focus.', ink: '#FFFFFF' },
  { name: 'Signal Pink', hex: '#FF2E88', role: 'Primary call to action. One per view.', ink: '#14112B' },
  { name: 'Tangerine Ping', hex: '#FF8A00', role: 'Things Jarvis already handled. Highlights.', ink: '#14112B' },
  { name: 'Inbox Lime', hex: '#C6F432', role: 'Success, "done", the spark on the mark.', ink: '#14112B' },
  { name: 'Butler Cyan', hex: '#19D3F0', role: "Jarvis's eyes. Data, charts, secondary accents.", ink: '#14112B' },
  { name: 'Midnight Ink', hex: '#14112B', role: 'Body text, dark sections, the face plate.', ink: '#FFFFFF' },
  { name: 'Cream Paper', hex: '#FFF8EE', role: 'Page background. Never pure white for large fields.', ink: '#14112B' },
];

export type PageId = 'home' | 'platform' | 'agent' | 'pricing' | 'aws' | 'brand';

export const PAGES: { id: PageId; path: string; label: string; title: string }[] = [
  { id: 'home', path: '/', label: 'Home', title: 'Jarvis — your newsletter, already handled' },
  { id: 'platform', path: '/platform', label: 'Platform', title: 'Platform — Jarvis' },
  { id: 'agent', path: '/meet-jarvis', label: 'Meet Jarvis', title: 'Meet Jarvis — the agent' },
  { id: 'pricing', path: '/pricing', label: 'Pricing', title: 'Pricing (2027 preview) — Jarvis' },
  { id: 'aws', path: '/built-on-aws', label: 'Built on AWS', title: 'Built on AWS — Jarvis' },
  { id: 'brand', path: '/brand', label: 'Brand', title: 'Brand guidelines — Jarvis' },
];

export const pageForPath = (path: string): PageId => {
  const clean = path.replace(/\/+$/, '') || '/';
  return PAGES.find((p) => p.path === clean)?.id ?? 'home';
};

export const pathFor = (id: PageId): string => PAGES.find((p) => p.id === id)?.path ?? '/';
