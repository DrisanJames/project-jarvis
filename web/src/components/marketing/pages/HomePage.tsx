import React, { useState } from 'react';
import { InboxSorterLoop, Spot } from '../Art';
import { JarvisMark } from '../JarvisLogo';
import { AudiencePreview, AutomationPreview, CampaignPreview, ReportPreview } from '../ProductPreview';
import { Availability, CtaBand, EarlyAccessLink, Eyebrow, PageLink } from '../parts';

const CARDS = [
  {
    kind: 'creative' as const,
    tone: 'violet',
    title: 'Campaigns that sound like you.',
    body: 'Start with your idea, content or template. Get help shaping the message, refine it for your brand and prepare it for your audience.',
  },
  {
    kind: 'audience' as const,
    tone: 'lime',
    title: 'Give every audience a relevant message.',
    body: 'Organize contacts around the information and engagement that matter to your business. Make each campaign more focused with audiences you can understand and manage.',
  },
  {
    kind: 'send' as const,
    tone: 'cyan',
    title: 'Keep the conversation going.',
    body: 'Build welcome emails and follow-up sequences that continue after the first send. Set the steps and timing, then review how the journey performs.',
  },
  {
    kind: 'lake' as const,
    tone: 'pink',
    title: 'Know what deserves your attention.',
    body: 'See campaign activity and results in one place. Use clear reporting and AI-assisted summaries to decide what to repeat, adjust or test next.',
  },
];

const STEPS = [
  { title: 'Bring your audience.', body: 'Import your subscribed contacts and organize the information you need for relevant campaigns.' },
  { title: 'Build the campaign.', body: 'Create your message, use AI where it helps and choose the audience and timing.' },
  { title: 'Review and send.', body: 'Check the campaign details, then schedule or send it. Use the results to shape the next campaign.' },
];

const SOLUTIONS = [
  { tone: 'violet', title: 'Newsletters and publishers', body: 'Keep subscribers connected with regular editions and relevant follow-ups.' },
  { tone: 'cyan', title: 'Marketing teams', body: 'Bring announcements, promotions and lifecycle emails into a shared campaign workflow.' },
];

const WALKTHROUGH = [
  { id: 'draft', label: 'Draft', view: <CampaignPreview /> },
  { id: 'audience', label: 'Audience', view: <AudiencePreview /> },
  { id: 'journey', label: 'Follow-up', view: <AutomationPreview /> },
  { id: 'report', label: 'Report', view: <ReportPreview /> },
];

const FAQ = [
  { q: 'What is Jarvis?', a: 'Jarvis is an email marketing and sending platform with AI assistance for campaign work.' },
  {
    q: 'What does the AI help with?',
    a: 'Use Jarvis to help draft messages, explore subject lines and understand campaign results. Review its suggestions as you prepare your next send.',
  },
  {
    q: 'Can I bring my existing contacts?',
    a: 'Yes. Import your subscribed contacts and your opt-outs, so people who left your list stay off it. Start with the migration guide to prepare your audience.',
  },
  { q: 'When can I get access?', a: 'Early access is planned for 2027. Join the list to receive availability updates.' },
];

const Walkthrough: React.FC = () => {
  const [active, setActive] = useState(WALKTHROUGH[0].id);
  return (
    <div>
      <div className="jv-tabs" role="tablist" aria-label="Campaign walkthrough">
        {WALKTHROUGH.map((w) => (
          <button
            key={w.id}
            type="button"
            role="tab"
            id={`jv-tab-${w.id}`}
            aria-selected={active === w.id}
            aria-controls={`jv-tabpanel-${w.id}`}
            className={`jv-tab${active === w.id ? ' is-active' : ''}`}
            onClick={() => setActive(w.id)}
          >
            {w.label}
          </button>
        ))}
      </div>
      {WALKTHROUGH.map((w) => (
        <div key={w.id} role="tabpanel" id={`jv-tabpanel-${w.id}`} aria-labelledby={`jv-tab-${w.id}`} hidden={active !== w.id}>
          {w.view}
        </div>
      ))}
    </div>
  );
};

export const HomePage: React.FC = () => (
  <>
    <section className="jv-hero">
      <div className="jv-wrap jv-hero-grid">
        <div className="jv-hero-copy">
          <Eyebrow tone="pink">AI-powered email marketing</Eyebrow>
          <h1>
            The email marketing platform with <span className="jv-hl">AI on your team.</span>
          </h1>
          <p className="jv-lede">
            Create campaigns, manage audiences and send email in one place. Jarvis helps with the work behind every
            campaign, so your team can move from idea to send with less manual effort.
          </p>
          <div className="jv-cta-row">
            <EarlyAccessLink />
            <PageLink to="platform" className="jv-btn jv-btn--ghost">
              Explore the platform
            </PageLink>
          </div>
          <Availability />
        </div>
        <CampaignPreview />
      </div>
    </section>

    <section className="jv-section">
      <div className="jv-wrap">
        <div className="jv-section-head">
          <Eyebrow>The platform</Eyebrow>
          <h2>Bring your email program together.</h2>
          <p className="jv-lede">
            Build a newsletter, launch a promotion or welcome a new subscriber. Manage the work in one platform, with a
            clear view of what you’re sending, who it’s for and how it performs.
          </p>
        </div>
        <div className="jv-card-grid jv-card-grid--4">
          {CARDS.map((c) => (
            <article key={c.title} className={`jv-card jv-card--${c.tone}`}>
              <Spot kind={c.kind} />
              <h3>{c.title}</h3>
              <p>{c.body}</p>
            </article>
          ))}
        </div>
        <div className="jv-cta-row jv-cta-row--center">
          <PageLink to="platform" className="jv-btn jv-btn--ghost">
            Explore the platform
          </PageLink>
        </div>
      </div>
    </section>

    <section className="jv-section jv-section--ink">
      <div className="jv-wrap jv-split">
        <div>
          <Eyebrow tone="lime">AI in the work</Eyebrow>
          <h2>An extra pair of hands for your email marketing.</h2>
          <p className="jv-lede">
            Ask Jarvis to help draft a campaign, review a message or explain a report. Work from a useful starting
            point, make it your own and keep control of the decisions that matter.
          </p>
          <PageLink to="ai" className="jv-btn jv-btn--light">
            See how Jarvis helps
          </PageLink>
        </div>
        <div className="jv-chat" aria-label="Example request and response">
          <div className="jv-msg jv-msg--you">
            Draft a welcome email for new newsletter subscribers. Keep it warm, brief and focused on what they’ll
            receive.
          </div>
          <div className="jv-msg jv-msg--jarvis">
            <JarvisMark size={30} title="Jarvis" animated />
            <div>Here’s a first draft and two subject line options. Review the message and choose the audience before scheduling.</div>
          </div>
        </div>
      </div>
    </section>

    <section className="jv-section jv-section--cream">
      <div className="jv-wrap jv-split">
        <div>
          <Eyebrow tone="tangerine">How it works</Eyebrow>
          <h2>Make your next campaign easier to launch.</h2>
          <p className="jv-lede">Bring your audience and your message. Jarvis helps you turn them into a campaign you can review and send.</p>
          <ol className="jv-steps jv-steps--list">
            {STEPS.map((s, i) => (
              <li key={s.title}>
                <span className="jv-step-n">{i + 1}</span>
                <h3>{s.title}</h3>
                <p>{s.body}</p>
              </li>
            ))}
          </ol>
        </div>
        <InboxSorterLoop />
      </div>
    </section>

    <section className="jv-section">
      <div className="jv-wrap">
        <div className="jv-section-head">
          <Eyebrow tone="cyan">Solutions</Eyebrow>
          <h2>Built around the emails your business sends.</h2>
          <p className="jv-lede">
            Keep a publication consistent, support a promotion or help a new customer get started. Find the workflow
            that matches your program.
          </p>
        </div>
        <div className="jv-card-grid jv-card-grid--2">
          {SOLUTIONS.map((s) => (
            <article key={s.title} className={`jv-card jv-card--${s.tone}`}>
              <h3>{s.title}</h3>
              <p>{s.body}</p>
            </article>
          ))}
        </div>
        <div className="jv-cta-row jv-cta-row--center">
          <PageLink to="solutions" className="jv-btn jv-btn--ghost">
            Find your use case
          </PageLink>
        </div>
      </div>
    </section>

    <section className="jv-section jv-section--cream">
      <div className="jv-wrap">
        <div className="jv-section-head">
          <Eyebrow>See it</Eyebrow>
          <h2>See the work behind the send.</h2>
          <p className="jv-lede">Explore a campaign from the first draft through audience selection, follow-up and reporting.</p>
        </div>
        <Walkthrough />
      </div>
    </section>

    <section className="jv-section">
      <div className="jv-wrap jv-split">
        <div>
          <Eyebrow tone="pink">Integrations</Eyebrow>
          <h2>Connect email to the rest of your business.</h2>
          <p className="jv-lede">We’re choosing which connections to build first. Tell us what you want to connect.</p>
          <PageLink to="integrations" className="jv-btn jv-btn--ghost">
            Tell us what you want to connect
          </PageLink>
        </div>
        <div className="jv-faq">
          <h2 className="jv-faq-title">Frequently asked questions</h2>
          {FAQ.map((f) => (
            <details key={f.q}>
              <summary>{f.q}</summary>
              <p>{f.a}</p>
            </details>
          ))}
        </div>
      </div>
    </section>

    <CtaBand title="Give your next campaign a head start." body="See how Jarvis can fit the way your team creates and sends email." />
  </>
);
