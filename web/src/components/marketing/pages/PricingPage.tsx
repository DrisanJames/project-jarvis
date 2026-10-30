import React from 'react';
import { EarlyAccessButton, Eyebrow, PageHero } from '../parts';

// 2027 go-to-market preview. Indicative only — labelled as such on the page.
const TIERS = [
  {
    name: 'Sidekick',
    price: '$0',
    per: '/month',
    tagline: 'For your first list.',
    tone: 'cyan',
    cta: 'Start free',
    features: ['Up to 5,000 contacts', '1 sending domain on Amazon SES', 'Jarvis daily report', 'Creative Studio basics'],
  },
  {
    name: 'Right Hand',
    price: '$149',
    per: '/month',
    tagline: 'For growing teams who send every day.',
    tone: 'pink',
    cta: 'Get early access',
    featured: true,
    features: [
      'Up to 100,000 contacts',
      '5 sending domains',
      'Jarvis plans and stages every send day',
      'Per-provider routing and wave pacing',
      'Bot-click protection',
    ],
  },
  {
    name: 'Chief of Staff',
    price: '$899',
    per: '/month',
    tagline: 'For portfolios and high-volume senders.',
    tone: 'violet',
    cta: 'Talk to us',
    features: [
      'Up to 1M contacts',
      'Unlimited sending domains',
      'Bring your own MTAs and dedicated IPs',
      'The Brain: verified team memory',
      'Analytics lake + exports',
    ],
  },
  {
    name: 'Mission Control',
    price: 'Custom',
    per: '',
    tagline: 'For estates sending tens of millions.',
    tone: 'lime',
    cta: 'Contact sales',
    features: ['Multi-transport estate', 'Dedicated deliverability engineer', 'Custom agent playbooks', 'SSO and audit exports'],
  },
];

const FAQ = [
  {
    q: 'Is this pricing final?',
    a: 'No. It is our 2027 launch preview. Design partners lock their rate for the first year.',
  },
  {
    q: 'Does Jarvis send anything without me?',
    a: 'Only inside the limits you set. Deploys, live changes and volume increases beyond your rules wait for a human yes.',
  },
  {
    q: 'Can I bring my own sending infrastructure?',
    a: 'Yes. Amazon SES is built in; Chief of Staff and above connect your own PowerMTA or KumoMTA servers too.',
  },
  {
    q: 'What counts as a contact?',
    a: 'A unique, mailable address. Suppressed and unsubscribed addresses are free — Jarvis keeps them suppressed.',
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
      body="Every plan includes the agent. Bigger plans give it more lanes to run. Indicative pricing for our 2027 launch."
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
