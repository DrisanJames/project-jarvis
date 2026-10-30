import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import './marketing.css';
import { CONTACT_EMAIL, PAGES, PageId, pageForPath, pathFor } from './brand';
import { JarvisLockup, JarvisMark } from './JarvisLogo';
import { NavContext, PageLink } from './parts';
import { HomePage } from './pages/HomePage';
import { PlatformPage } from './pages/PlatformPage';
import { AgentPage } from './pages/AgentPage';
import { PricingPage } from './pages/PricingPage';
import { AwsPage } from './pages/AwsPage';
import { BrandPage } from './pages/BrandPage';

// Public marketing site, shown to signed-out visitors. Signing in still goes
// through Google OAuth restricted to the allowed Workspace domain
// (internal/auth/auth.go) — nothing here creates accounts.

const FONT_HREF =
  'https://fonts.googleapis.com/css2?family=Bricolage+Grotesque:opsz,wght@12..96,600;12..96,800&family=Inter:wght@400;500;600;700&display=swap';

const NAV: PageId[] = ['platform', 'agent', 'pricing', 'aws'];

interface Props {
  onSignIn: () => void;
}

export const MarketingSite: React.FC<Props> = ({ onSignIn }) => {
  const [page, setPage] = useState<PageId>(() => pageForPath(window.location.pathname));
  const [menuOpen, setMenuOpen] = useState(false);
  const [modalIntent, setModalIntent] = useState<string | null>(null);

  useEffect(() => {
    if (!document.querySelector('link[data-jv-fonts]')) {
      const link = document.createElement('link');
      link.rel = 'stylesheet';
      link.href = FONT_HREF;
      link.setAttribute('data-jv-fonts', '1');
      document.head.appendChild(link);
    }
    document.body.classList.add('jv-body');
    const onPop = () => setPage(pageForPath(window.location.pathname));
    window.addEventListener('popstate', onPop);
    return () => {
      document.body.classList.remove('jv-body');
      window.removeEventListener('popstate', onPop);
    };
  }, []);

  useEffect(() => {
    document.title = PAGES.find((p) => p.id === page)?.title ?? 'Jarvis';
  }, [page]);

  const go = useCallback((id: PageId, hash?: string) => {
    const target = pathFor(id) + (hash ? `#${hash}` : '');
    if (window.location.pathname + window.location.hash !== target) {
      window.history.pushState({}, '', target);
    }
    setPage(id);
    setMenuOpen(false);
    requestAnimationFrame(() => {
      const el = hash ? document.getElementById(hash) : null;
      if (el) el.scrollIntoView({ behavior: 'smooth' });
      else window.scrollTo({ top: 0 });
    });
  }, []);

  const nav = useMemo(
    () => ({ go, openEarlyAccess: (intent?: string) => setModalIntent(intent ?? 'Early access'), signIn: onSignIn }),
    [go, onSignIn],
  );

  const Page = {
    home: HomePage,
    platform: PlatformPage,
    agent: AgentPage,
    pricing: PricingPage,
    aws: AwsPage,
    brand: BrandPage,
  }[page];

  return (
    <NavContext.Provider value={nav}>
      <div className="jv-site">
        <a className="jv-skip" href="#jv-main">
          Skip to content
        </a>
        <div className="jv-announce">
          <span>Jarvis opens to design partners in 2027.</span>
          <button type="button" onClick={() => nav.openEarlyAccess('Design partner')}>
            Claim a seat →
          </button>
        </div>

        <header className="jv-nav">
          <div className="jv-wrap jv-nav-inner">
            <PageLink to="home" className="jv-nav-logo">
              <JarvisLockup size={34} />
            </PageLink>
            <nav className={`jv-nav-links${menuOpen ? ' is-open' : ''}`} aria-label="Main">
              {NAV.map((id) => (
                <PageLink key={id} to={id} className={`jv-nav-link${page === id ? ' is-active' : ''}`}>
                  {PAGES.find((p) => p.id === id)?.label}
                </PageLink>
              ))}
              <div className="jv-nav-actions">
                <button type="button" className="jv-nav-signin" onClick={onSignIn}>
                  Sign in
                </button>
                <button type="button" className="jv-btn jv-btn--primary jv-btn--sm" onClick={() => nav.openEarlyAccess('Demo')}>
                  Get a demo
                </button>
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

        <main id="jv-main">
          <Page />
        </main>

        <footer className="jv-footer">
          <div className="jv-wrap jv-footer-grid">
            <div className="jv-footer-brand">
              <JarvisLockup size={34} inverse />
              <p>The email agent that's already on it.</p>
            </div>
            <div>
              <h4>Product</h4>
              <PageLink to="platform">Platform</PageLink>
              <PageLink to="agent">Meet Jarvis</PageLink>
              <PageLink to="pricing">Pricing</PageLink>
            </div>
            <div>
              <h4>Company</h4>
              <PageLink to="aws">Built on AWS</PageLink>
              <PageLink to="brand">Brand guidelines</PageLink>
              <a href={`mailto:${CONTACT_EMAIL}`}>Contact</a>
            </div>
            <div>
              <h4>Team</h4>
              <button type="button" className="jv-footer-btn" onClick={onSignIn}>
                Team sign in
              </button>
              <span className="jv-footer-note">Company Google accounts only.</span>
            </div>
          </div>
          <div className="jv-wrap jv-footer-base">
            <span>© {new Date().getFullYear()} James Ventures Corp. Jarvis is a product of James Ventures.</span>
            <span>Made with care. Checked twice by Jarvis.</span>
          </div>
        </footer>

        {modalIntent && <EarlyAccessModal intent={modalIntent} onClose={() => setModalIntent(null)} />}
      </div>
    </NavContext.Provider>
  );
};

// No backend lead store yet: the form composes an email to the team. Swapping
// in a POST later only touches submit().
const EarlyAccessModal: React.FC<{ intent: string; onClose: () => void }> = ({ intent, onClose }) => {
  const [sent, setSent] = useState(false);
  const firstField = useRef<HTMLInputElement>(null);

  useEffect(() => {
    firstField.current?.focus();
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && onClose();
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onClose]);

  const submit = (e: React.FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    const f = new FormData(e.currentTarget);
    const line = (k: string) => `${k}: ${String(f.get(k) ?? '').trim()}`;
    const body = [line('Name'), line('Work email'), line('Company'), line('Monthly volume'), '', String(f.get('Notes') ?? '')].join('\n');
    const subject = `Jarvis ${intent} — ${String(f.get('Company') ?? '').trim() || 'request'}`;
    window.location.href = `mailto:${CONTACT_EMAIL}?subject=${encodeURIComponent(subject)}&body=${encodeURIComponent(body)}`;
    setSent(true);
  };

  return (
    <div className="jv-modal-backdrop" onClick={onClose}>
      <div className="jv-modal" role="dialog" aria-modal="true" aria-labelledby="jv-modal-title" onClick={(e) => e.stopPropagation()}>
        <button type="button" className="jv-modal-close" aria-label="Close" onClick={onClose}>
          ×
        </button>
        <div className="jv-modal-head">
          <JarvisMark size={52} mood={sent ? 'wink' : 'idle'} />
          <div>
            <h2 id="jv-modal-title">{sent ? 'Noted. I’ll take it from here.' : `${intent}: let’s talk`}</h2>
            <p>
              {sent
                ? `Your email app should have opened with everything filled in. If it didn't, write to ${CONTACT_EMAIL}.`
                : 'A few fields. I already guessed the rest.'}
            </p>
          </div>
        </div>
        {!sent && (
          <form className="jv-form" onSubmit={submit}>
            <label>
              Name
              <input ref={firstField} name="Name" required autoComplete="name" />
            </label>
            <label>
              Work email
              <input name="Work email" type="email" required autoComplete="email" />
            </label>
            <label>
              Company
              <input name="Company" required autoComplete="organization" />
            </label>
            <label>
              Monthly email volume
              <select name="Monthly volume" defaultValue="100k–1M">
                <option>Under 100k</option>
                <option>100k–1M</option>
                <option>1M–10M</option>
                <option>10M+</option>
              </select>
            </label>
            <label className="jv-form-wide">
              What should Jarvis take off your plate?
              <textarea name="Notes" rows={3} placeholder="Deliverability, scheduling, reporting, all of it…" />
            </label>
            <button type="submit" className="jv-btn jv-btn--primary jv-form-wide">
              Send it to the team
            </button>
          </form>
        )}
      </div>
    </div>
  );
};

export default MarketingSite;
