import React, { createContext, useContext } from 'react';
import { PageId, pathFor } from './brand';

interface MarketingNav {
  go: (id: PageId, hash?: string) => void;
  openEarlyAccess: (intent?: string) => void;
  signIn: () => void;
}

export const NavContext = createContext<MarketingNav>({
  go: () => undefined,
  openEarlyAccess: () => undefined,
  signIn: () => undefined,
});

export const useMarketingNav = () => useContext(NavContext);

// Real <a href> so links work without JS and open in new tabs; plain clicks
// stay inside the SPA.
export const PageLink: React.FC<{ to: PageId; className?: string; children: React.ReactNode }> = ({
  to,
  className,
  children,
}) => {
  const { go } = useMarketingNav();
  return (
    <a
      href={pathFor(to)}
      className={className}
      onClick={(e) => {
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
        e.preventDefault();
        go(to);
      }}
    >
      {children}
    </a>
  );
};

export const EarlyAccessButton: React.FC<{ label?: string; intent?: string; variant?: 'primary' | 'ghost' | 'light' }> = ({
  label = 'Get early access',
  intent,
  variant = 'primary',
}) => {
  const { openEarlyAccess } = useMarketingNav();
  return (
    <button type="button" className={`jv-btn jv-btn--${variant}`} onClick={() => openEarlyAccess(intent)}>
      {label}
    </button>
  );
};

export const Eyebrow: React.FC<{ children: React.ReactNode; tone?: 'violet' | 'pink' | 'lime' | 'cyan' | 'tangerine' }> = ({
  children,
  tone = 'violet',
}) => <span className={`jv-eyebrow jv-eyebrow--${tone}`}>{children}</span>;

export const CtaBand: React.FC<{ title: string; body: string; intent?: string }> = ({ title, body, intent }) => (
  <section className="jv-cta-band">
    <div className="jv-wrap jv-cta-band-inner">
      <div>
        <h2>{title}</h2>
        <p>{body}</p>
      </div>
      <div className="jv-cta-row">
        <EarlyAccessButton intent={intent} />
        <PageLink to="pricing" className="jv-btn jv-btn--light">
          See 2027 pricing
        </PageLink>
      </div>
    </div>
  </section>
);

export const PageHero: React.FC<{ eyebrow: string; title: React.ReactNode; body: string; children?: React.ReactNode }> = ({
  eyebrow,
  title,
  body,
  children,
}) => (
  <section className="jv-page-hero">
    <div className="jv-wrap">
      <Eyebrow>{eyebrow}</Eyebrow>
      <h1>{title}</h1>
      <p className="jv-lede">{body}</p>
      {children}
    </div>
  </section>
);
