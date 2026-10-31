import React from 'react';
import { EarlyAccessLink, PageHero, PageLink } from '../parts';

const TIERS = [
  { name: 'Starter', tone: 'cyan', need: 'For a smaller recurring email program.' },
  { name: 'Growth', tone: 'pink', need: 'For more frequent campaigns and follow-ups.' },
  { name: 'Scale', tone: 'violet', need: 'For larger or more complex programs.' },
];

export const PricingPage: React.FC = () => (
  <>
    <PageHero
      eyebrow="Pricing"
      title={
        <>
          Choose the right fit for <span className="jv-hl">your email program.</span>
        </>
      }
      body="Pricing is coming soon. Start with the campaigns you send today and the support you need to grow."
    />
    <section className="jv-section jv-section--tight">
      <div className="jv-wrap jv-pricing">
        {TIERS.map((t) => (
          <article key={t.name} className={`jv-tier jv-tier--${t.tone}`}>
            <h2>{t.name}</h2>
            <p className="jv-tier-tag">{t.need}</p>
            <p className="jv-tier-soon">Coming soon</p>
            <EarlyAccessLink variant="ghost" />
          </article>
        ))}
      </div>
      <div className="jv-cta-row jv-cta-row--center">
        <PageLink to="contact" className="jv-btn jv-btn--ghost">
          Discuss your requirements
        </PageLink>
      </div>
    </section>
  </>
);
