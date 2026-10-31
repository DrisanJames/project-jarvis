// Jarvis brand tokens + site map. The brand page (/brand) renders from these,
// so the guideline and the site can never drift apart.

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

export type PageId =
  | 'home'
  | 'platform'
  | 'ai'
  | 'solutions'
  | 'integrations'
  | 'developers'
  | 'pricing'
  | 'migration'
  | 'about'
  | 'demo'
  | 'earlyAccess'
  | 'contact'
  | 'trust'
  | 'privacy'
  | 'brand'
  | 'signIn'
  | 'onboarding'
  | 'notFound';

export interface PageMeta {
  id: PageId;
  path: string;
  label: string;
  title: string;
  description: string;
  noindex?: boolean;
}

// web/scripts/prerender.mjs writes one HTML file per entry, so each of these
// titles and descriptions is in the page before any JavaScript runs.
export const PAGES: PageMeta[] = [
  {
    id: 'home',
    path: '/',
    label: 'Home',
    title: 'Jarvis | AI-Powered Email Marketing Platform',
    description:
      'Create and send email campaigns with Jarvis. Bring audiences, automation and AI assistance together in one email marketing platform. Explore early access.',
  },
  {
    id: 'platform',
    path: '/platform',
    label: 'Platform',
    title: 'Email Marketing Platform | Jarvis',
    description:
      'Manage audiences, build campaigns, automate follow-ups and read your results in one email marketing platform, with AI assistance where the work happens.',
  },
  {
    id: 'ai',
    path: '/ai',
    label: 'AI',
    title: 'AI for Email Marketing | Jarvis',
    description:
      'Jarvis helps with the writing, review and reporting behind a campaign. Start from a useful suggestion, change it and make the final call.',
  },
  {
    id: 'solutions',
    path: '/solutions',
    label: 'Solutions',
    title: 'Email Marketing Solutions | Jarvis',
    description: 'See how newsletters, publishers and marketing teams can run a regular email program with Jarvis.',
  },
  {
    id: 'integrations',
    path: '/integrations',
    label: 'Integrations',
    title: 'Integrations | Jarvis',
    description: 'Tell us which tools you want to connect to Jarvis, and see how to bring your contacts in today.',
  },
  {
    id: 'developers',
    path: '/developers',
    label: 'Developers',
    title: 'Developers | Jarvis',
    description: 'The Jarvis API is not public yet. Request API access and tell us what you want to build.',
  },
  {
    id: 'pricing',
    path: '/pricing',
    label: 'Pricing',
    title: 'Pricing | Jarvis',
    description: 'Jarvis pricing is coming soon. See the plans we are shaping and request early access.',
  },
  {
    id: 'migration',
    path: '/migration',
    label: 'Migration',
    title: 'Switch to Jarvis | Migration',
    description: 'Plan your move to Jarvis: map your contacts and opt-outs, import them, and review your first campaign.',
  },
  {
    id: 'about',
    path: '/about',
    label: 'About',
    title: 'About Jarvis',
    description: 'Jarvis is an email marketing platform from James Ventures Corp, built around the everyday work of email.',
  },
  {
    id: 'demo',
    path: '/demo',
    label: 'Demo',
    title: 'Request a Demo | Jarvis',
    description: 'Tell us what you send and who you send it to, and request a walkthrough of Jarvis.',
  },
  {
    id: 'earlyAccess',
    path: '/early-access',
    label: 'Early access',
    title: 'Request Early Access | Jarvis',
    description: 'Early access to Jarvis is planned for 2027. Join the list for product updates and availability.',
  },
  {
    id: 'contact',
    path: '/contact',
    label: 'Contact',
    title: 'Contact Jarvis',
    description: 'Ask about Jarvis, discuss an integration or get help. Send us a message and the team will reply.',
  },
  {
    id: 'trust',
    path: '/trust',
    label: 'Trust',
    title: 'Trust | Jarvis',
    description: 'How access to Jarvis works, how opt-outs are handled and what happens to the details you send us.',
  },
  {
    id: 'privacy',
    path: '/privacy',
    label: 'Privacy',
    title: 'Privacy Notice | Jarvis',
    description: 'What the forms on this website collect, why we collect it and how to reach us about it.',
  },
  { id: 'brand', path: '/brand', label: 'Brand', title: 'Brand Guidelines | Jarvis', description: 'The Jarvis logo, colours, type and voice.' },
  {
    id: 'signIn',
    path: '/sign-in',
    label: 'Sign in',
    title: 'Sign In | Jarvis',
    description: 'Sign in to Jarvis with your Google account.',
    noindex: true,
  },
  {
    id: 'onboarding',
    path: '/onboarding',
    label: 'Get started',
    title: 'Get Started | Jarvis',
    description: 'Tell us about your business and your email program.',
    noindex: true,
  },
  {
    id: 'notFound',
    path: '/not-found',
    label: 'Not found',
    title: 'Page Not Found | Jarvis',
    description: 'We could not find that page.',
    noindex: true,
  },
];

// Old URLs the server also redirects (internal/api/routes.go).
const LEGACY: Record<string, string> = { '/features': '/platform', '/meet-jarvis': '/ai', '/login': '/sign-in' };

export const canonicalPath = (path: string): string => {
  const clean = path.replace(/\/+$/, '') || '/';
  return LEGACY[clean] ?? clean;
};

export const pageForPath = (path: string): PageId => {
  const clean = canonicalPath(path);
  return PAGES.find((p) => p.path === clean)?.id ?? 'notFound';
};

export const pageMeta = (id: PageId): PageMeta => PAGES.find((p) => p.id === id) ?? PAGES[0];

export const pathFor = (id: PageId): string => pageMeta(id).path;
