import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import './marketing.css';
import { PageId, canonicalPath, pageForPath, pageMeta, pathFor } from './brand';
import { JarvisChat } from './JarvisChat';
import { JarvisLockup } from './JarvisLogo';
import { NavContext, PageLink } from './parts';
import { HomePage } from './pages/HomePage';
import { PlatformPage } from './pages/PlatformPage';
import { AiPage } from './pages/AiPage';
import { SolutionsPage } from './pages/SolutionsPage';
import { IntegrationsPage } from './pages/IntegrationsPage';
import { DevelopersPage } from './pages/DevelopersPage';
import { PricingPage } from './pages/PricingPage';
import { MigrationPage } from './pages/MigrationPage';
import { AboutPage } from './pages/AboutPage';
import { ContactPage, DemoPage, EarlyAccessPage } from './pages/ConversionPages';
import { PrivacyPage, TrustPage } from './pages/TrustPages';
import { BrandPage } from './pages/BrandPage';
import { SignInPage } from './pages/SignInPage';
import { OnboardingPage } from './pages/OnboardingPage';
import { NotFoundPage } from './pages/NotFoundPage';

// Public marketing site, shown to signed-out visitors. Access to the portal is
// decided only in internal/auth/auth.go; nothing here grants anything.

const NAV: PageId[] = ['platform', 'ai', 'solutions', 'integrations', 'pricing', 'developers'];

const PAGE_COMPONENTS: Record<PageId, React.FC> = {
  home: HomePage,
  platform: PlatformPage,
  ai: AiPage,
  solutions: SolutionsPage,
  integrations: IntegrationsPage,
  developers: DevelopersPage,
  pricing: PricingPage,
  migration: MigrationPage,
  about: AboutPage,
  demo: DemoPage,
  earlyAccess: EarlyAccessPage,
  contact: ContactPage,
  trust: TrustPage,
  privacy: PrivacyPage,
  brand: BrandPage,
  signIn: SignInPage,
  onboarding: OnboardingPage,
  notFound: NotFoundPage,
};

const setMeta = (selector: string, attr: string, value: string) => {
  document.head.querySelector(selector)?.setAttribute(attr, value);
};

interface Props {
  onSignIn: () => void;
  authError?: string | null;
  // Set by the prerender, where there is no window.
  initialPath?: string;
}

export const MarketingSite: React.FC<Props> = ({ onSignIn, authError = null, initialPath }) => {
  const [page, setPage] = useState<PageId>(() => pageForPath(initialPath ?? window.location.pathname));
  const [menuOpen, setMenuOpen] = useState(false);
  const fromPath = useRef('');

  useEffect(() => {
    document.body.classList.add('jv-body');
    // A legacy URL that reached the client: show the address it now lives at.
    const canon = canonicalPath(window.location.pathname);
    if (canon !== window.location.pathname && pageForPath(canon) !== 'notFound') {
      window.history.replaceState({}, '', canon + window.location.search + window.location.hash);
    }
    const onPop = () => setPage(pageForPath(window.location.pathname));
    window.addEventListener('popstate', onPop);
    return () => {
      document.body.classList.remove('jv-body');
      window.removeEventListener('popstate', onPop);
    };
  }, []);

  useEffect(() => {
    const meta = pageMeta(page);
    document.title = meta.title;
    setMeta('meta[name="description"]', 'content', meta.description);
    setMeta('meta[property="og:title"]', 'content', meta.title);
    setMeta('meta[property="og:description"]', 'content', meta.description);
    setMeta('link[rel="canonical"]', 'href', `https://projectjarvis.io${meta.path}`);
  }, [page]);

  const go = useCallback((id: PageId, hash?: string) => {
    const target = pathFor(id) + (hash ? `#${hash}` : '');
    fromPath.current = window.location.pathname;
    if (window.location.pathname + window.location.hash !== target) {
      window.history.pushState({}, '', target);
    }
    setPage(id);
    setMenuOpen(false);
    requestAnimationFrame(() => {
      const el = hash ? document.getElementById(hash) : null;
      if (el) el.scrollIntoView();
      else window.scrollTo({ top: 0 });
      document.getElementById('jv-main')?.focus({ preventScroll: true });
    });
  }, []);

  const nav = useMemo(() => ({ go, signIn: onSignIn, fromPath: fromPath.current }), [go, onSignIn, page]);

  const Page = PAGE_COMPONENTS[page];

  return (
    <NavContext.Provider value={nav}>
      <div className="jv-site">
        <a className="jv-skip" href="#jv-main">
          Skip to content
        </a>
        <aside className="jv-announce" aria-label="Availability">
          <span>Early access planned for 2027.</span>
          <PageLink to="earlyAccess">Join the list →</PageLink>
        </aside>

        <header className="jv-nav">
          <div className="jv-wrap jv-nav-inner">
            <PageLink to="home" className="jv-nav-logo">
              <JarvisLockup size={34} animated />
            </PageLink>
            <nav className={`jv-nav-links${menuOpen ? ' is-open' : ''}`} aria-label="Main">
              {NAV.map((id) => (
                <PageLink key={id} to={id} className={`jv-nav-link${page === id ? ' is-active' : ''}`}>
                  {pageMeta(id).label}
                </PageLink>
              ))}
              <div className="jv-nav-actions">
                <PageLink to="signIn" className="jv-nav-signin">
                  Sign in
                </PageLink>
                <PageLink to="earlyAccess" className="jv-btn jv-btn--primary jv-btn--sm">
                  Request early access
                </PageLink>
              </div>
            </nav>
            <button
              type="button"
              className="jv-burger"
              aria-label={menuOpen ? 'Close menu' : 'Open menu'}
              aria-expanded={menuOpen}
              onClick={() => setMenuOpen((o) => !o)}
            >
              <span />
              <span />
              <span />
            </button>
          </div>
        </header>

        <main id="jv-main" tabIndex={-1}>
          {page === 'signIn' ? <SignInPage error={authError} /> : <Page />}
        </main>

        <footer className="jv-footer">
          <div className="jv-wrap jv-footer-grid">
            <div className="jv-footer-brand">
              <JarvisLockup size={34} inverse />
              <p>AI-powered email marketing.</p>
            </div>
            <div>
              <h2>Product</h2>
              <PageLink to="platform">Platform</PageLink>
              <PageLink to="ai">AI</PageLink>
              <PageLink to="solutions">Solutions</PageLink>
              <PageLink to="integrations">Integrations</PageLink>
              <PageLink to="pricing">Pricing</PageLink>
              <PageLink to="developers">Developers</PageLink>
              <PageLink to="migration">Migration</PageLink>
            </div>
            <div>
              <h2>Company</h2>
              <PageLink to="about">About</PageLink>
              <PageLink to="contact">Contact</PageLink>
              <PageLink to="trust">Trust</PageLink>
              <PageLink to="privacy">Privacy</PageLink>
              <PageLink to="brand">Brand</PageLink>
            </div>
            <div>
              <h2>Account</h2>
              <PageLink to="signIn">Sign in</PageLink>
              <PageLink to="earlyAccess">Early access</PageLink>
              <PageLink to="demo">Request a demo</PageLink>
            </div>
          </div>
          <div className="jv-wrap jv-footer-base">
            <span>© {new Date().getFullYear()} James Ventures Corp. Jarvis is a product of James Ventures Corp.</span>
          </div>
        </footer>

        <JarvisChat />
      </div>
    </NavContext.Provider>
  );
};

export default MarketingSite;
