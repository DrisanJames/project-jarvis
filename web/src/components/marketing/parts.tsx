import React, { createContext, useContext } from 'react';
import { PageId, pathFor } from './brand';

interface MarketingNav {
  go: (id: PageId, hash?: string) => void;
  signIn: () => void;
  // Page the visitor came from, sent with a form as its source page.
  fromPath: string;
}

export const NavContext = createContext<MarketingNav>({
  go: () => undefined,
  signIn: () => undefined,
  fromPath: '',
});

export const useMarketingNav = () => useContext(NavContext);

// Real <a href> so links work without JS and open in new tabs; plain clicks
// stay inside the SPA.
export const PageLink: React.FC<{ to: PageId; hash?: string; className?: string; children: React.ReactNode }> = ({
  to,
  hash,
  className,
  children,
}) => {
  const { go } = useMarketingNav();
  return (
    <a
      href={pathFor(to) + (hash ? `#${hash}` : '')}
      className={className}
      onClick={(e) => {
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
        e.preventDefault();
        go(to, hash);
      }}
    >
      {children}
    </a>
  );
};

export const EarlyAccessLink: React.FC<{ label?: string; variant?: 'primary' | 'ghost' | 'light' }> = ({
  label = 'Request early access',
  variant = 'primary',
}) => (
  <PageLink to="earlyAccess" className={`jv-btn jv-btn--${variant}`}>
    {label}
  </PageLink>
);

export const Eyebrow: React.FC<{ children: React.ReactNode; tone?: 'violet' | 'pink' | 'lime' | 'cyan' | 'tangerine' }> = ({
  children,
  tone = 'violet',
}) => <span className={`jv-eyebrow jv-eyebrow--${tone}`}>{children}</span>;

export const CtaBand: React.FC<{ title: string; body: string }> = ({ title, body }) => (
  <section className="jv-cta-band">
    <div className="jv-wrap jv-cta-band-inner">
      <div>
        <h2>{title}</h2>
        <p>{body}</p>
      </div>
      <div className="jv-cta-row">
        <EarlyAccessLink />
        <PageLink to="platform" className="jv-btn jv-btn--light">
          Explore the platform
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

export const Availability: React.FC = () => (
  <p className="jv-fine">Early access planned for 2027. Join the list for product updates and availability.</p>
);

export const CopyBlocks: React.FC<{ items: { title: string; body: string }[]; heading?: 'h2' | 'h3'; tones?: string[] }> = ({
  items,
  heading: Heading = 'h3',
  tones = ['violet', 'cyan', 'pink', 'lime'],
}) => (
  <div className={`jv-card-grid jv-card-grid--${items.length >= 3 ? '3' : '2'}`}>
    {items.map((c, i) => (
      <article key={c.title} className={`jv-card jv-card--${tones[i % tones.length]}`}>
        <Heading className="jv-card-title">{c.title}</Heading>
        <p>{c.body}</p>
      </article>
    ))}
  </div>
);
