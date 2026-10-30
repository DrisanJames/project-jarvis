import React from 'react';
import { EarlyAccessButton, Eyebrow, PageHero } from '../parts';

// 2027 go-to-market preview. Indicative only — labelled as such on the page.
const TIERS = [
  {
    name: 'Sidekick',
    price: '$0',
    per: '/month',
    tagline: 'For your first newsletter.',
    tone: 'cyan',
    cta: 'Start free',
    features: ['Up to 500 contacts', '1,000 emails a month', 'Templates and a sign-up form', 'Jarvis weekly tips'],
  },
  {
    name: 'Right Hand',
    price: '$19',
    per: '/month',
    tagline: 'For small businesses ready to grow.',
    tone: 'pink',
    cta: 'Get early access',
    featured: true,
    features: [
      'Up to 2,500 contacts',
      'Jarvis drafts emails and subject lines',
      'Welcome, thank-you and win-back automations',
      'Smart send times',
      'Reports in plain English',
    ],
  },
  {
    name: 'Chief of Staff',
    price: '$79',
    per: '/month',
    tagline: 'For busy teams with growing lists.',
    tone: 'violet',
    cta: 'Get early access',
    features: [
      'Up to 10,000 contacts',
      'Unlimited automations',
      'Behaviour-based segments',
      'A/B tests Jarvis runs for you',
      'Priority support',
    ],
  },
  {
    name: 'Mission Control',
    price: 'Custom',
    per: '',
    tagline: 'For multi-location and high-volume senders.',
    tone: 'lime',
    cta: 'Contact sales',
    features: ['50,000+ contacts', 'Multiple brands or locations', 'A dedicated success manager', 'Single sign-on'],
  },
];

const FAQ = [
  {
    q: 'Is this pricing final?',
    a: 'No. It is our 2027 launch preview. Early-access members lock their rate for the first year.',
  },
  {
    q: 'Does Jarvis send anything without me?',
    a: 'Only inside rules you set. New campaigns, big list changes and anything unusual wait for your yes.',
  },
  {
    q: 'Can I bring my list from another email tool?',
    a: 'Yes. Bring your list and Jarvis tidies it up before your first send. Only import people who asked to hear from you.',
  },
  {
    q: 'What counts as a contact?',
    a: 'A unique, active subscriber. Unsubscribed addresses are always free.',
  },
];

export const PricingPage: React.FC = () => (
  <>
    <PageHero
      eyebrow="Pricing · 2027 preview"
      title={
        <>
          Pick how much Jarvis <span className="jv-hl">takes off your plate.</span>
        </>
      }
      body="Every plan includes Jarvis. Bigger plans give it more to look after. Indicative pricing for our 2027 launch."
    />

    <section className="jv-section jv-section--tight">
      <div className="jv-wrap jv-pricing">
        {TIERS.map((t) => (
          <article key={t.name} className={`jv-tier jv-tier--${t.tone}${t.featured ? ' is-featured' : ''}`}>
            {t.featured && <span className="jv-tier-badge">Most popular</span>}
            <h3>{t.name}</h3>
            <p className="jv-tier-tag">{t.tagline}</p>
            <p className="jv-tier-price">
              {t.price}
              <span>{t.per}</span>
            </p>
            <EarlyAccessButton label={t.cta} intent={`${t.name} plan`} variant={t.featured ? 'primary' : 'ghost'} />
            <ul className="jv-checks">
              {t.features.map((f) => (
                <li key={f}>{f}</li>
              ))}
            </ul>
          </article>
        ))}
      </div>
    </section>

    <section className="jv-section jv-section--cream">
      <div className="jv-wrap jv-faq">
        <div className="jv-section-head">
          <Eyebrow tone="tangerine">Questions</Eyebrow>
          <h2>Jarvis anticipated a few of these.</h2>
        </div>
        {FAQ.map((f) => (
          <details key={f.q}>
            <summary>{f.q}</summary>
            <p>{f.a}</p>
          </details>
        ))}
      </div>
    </section>
  </>
);
