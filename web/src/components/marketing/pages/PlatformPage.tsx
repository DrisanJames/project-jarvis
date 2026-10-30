import React from 'react';
import { Spot } from '../Art';
import { CtaBand, EarlyAccessButton, Eyebrow, PageHero } from '../parts';

const FEATURES = [
  {
    kind: 'plan' as const,
    eyebrow: 'Campaign calendar',
    title: 'Your whole month on one calendar.',
    body: 'Tell Jarvis what’s coming — a sale, a launch, an event, a new blog post. It plans the emails, drafts them, picks the send times and lines everything up for your approval.',
    points: ['Newsletters and promotions in one view', 'Send times picked per subscriber', 'One-tap approve, one-tap undo'],
  },
  {
    kind: 'creative' as const,
    eyebrow: 'Templates & writing',
    title: 'Beautiful emails in your voice, in minutes.',
    body: 'Drag-and-drop templates in your colours and fonts. Jarvis drafts from your blog, your store or three bullet points, writes subject lines that sound like you, and checks every link and image before anything sends.',
    points: ['Brand kit: logo, colours, fonts', 'Drafts and subject lines in your voice', 'Every link and image checked'],
  },
  {
    kind: 'audience' as const,
    eyebrow: 'Audience & segments',
    title: 'Know who’s who on your list.',
    body: 'Jarvis groups subscribers by what they actually do — bought, clicked, opened — so your VIPs get early access, new subscribers get a proper welcome, and quiet subscribers aren’t pestered.',
    points: ['Segments built from real behaviour', 'Sign-up forms with double opt-in', 'Gentle re-engagement for quiet subscribers'],
  },
  {
    kind: 'send' as const,
    eyebrow: 'Automations',
    title: 'Set it once. Jarvis keeps it running.',
    body: 'Welcome new subscribers, thank first-time customers, win back the ones who drifted, wish people a happy birthday. Pick a recipe; Jarvis writes it and keeps it fresh.',
    points: ['Welcome series', 'Thank-you and win-back emails', 'Birthday and anniversary notes'],
  },
  {
    kind: 'protect' as const,
    eyebrow: 'Deliverability, handled',
    title: 'Land in the inbox, not the spam folder.',
    body: 'Jarvis walks you through setting up your sending domain, paces new lists gently, removes bounces and unsubscribes automatically, and filters robot clicks out of your stats so your numbers are honest.',
    points: ['Guided domain setup (SPF, DKIM, DMARC)', 'Automatic bounce and unsubscribe handling', 'Robot clicks filtered from reports'],
  },
  {
    kind: 'lake' as const,
    eyebrow: 'Reports in plain English',
    title: 'Numbers that tell you what to do next.',
    body: 'Opens, clicks, sales and unsubscribes — and what they mean. Every Monday Jarvis sends a short note: what worked, what didn’t, and one thing to try this week.',
    points: ['Sales from every email', 'Delivered, not just sent', 'A weekly coaching note'],
  },
  {
    kind: 'learn' as const,
    eyebrow: 'Memory',
    title: 'Tell it once. It remembers.',
    body: 'Jarvis keeps your brand voice, your rules and every correction you make. It checks its facts before it reports them and never makes you explain the same thing twice.',
    points: ['Remembers your brand voice', 'Your rules become its rules', 'Facts checked before they’re reported'],
  },
];

export const PlatformPage: React.FC = () => (
  <>
    <PageHero
      eyebrow="Platform"
      title={
        <>
          Email marketing with an AI teammate <span className="jv-hl">built in</span>, not bolted on.
        </>
      }
      body="Jarvis isn't a chatbot stuck on the side of an email tool. It works inside your account — reading the same numbers you do, using the same buttons, and asking before anything it can't undo."
    >
      <div className="jv-cta-row">
        <EarlyAccessButton label="Get a demo" intent="Demo" />
      </div>
    </PageHero>

    <section className="jv-section jv-section--tight">
      <div className="jv-wrap jv-features">
        {FEATURES.map((f, i) => (
          <article key={f.title} className={`jv-feature${i % 2 ? ' jv-feature--flip' : ''}`}>
            <div className="jv-feature-art">
              <Spot kind={f.kind} size={120} />
            </div>
            <div className="jv-feature-copy">
              <Eyebrow>{f.eyebrow}</Eyebrow>
              <h2>{f.title}</h2>
              <p>{f.body}</p>
              <ul className="jv-checks">
                {f.points.map((p) => (
                  <li key={p}>{p}</li>
                ))}
              </ul>
            </div>
          </article>
        ))}
      </div>
    </section>

    <CtaBand title="See Jarvis build your next newsletter." body="A 20-minute walkthrough: import a list, draft a campaign, meet the agent." intent="Demo" />
  </>
);
