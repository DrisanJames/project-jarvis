import React from 'react';
import { Spot } from '../Art';
import { CtaBand, EarlyAccessButton, Eyebrow, PageHero } from '../parts';

const FEATURES = [
  {
    kind: 'plan' as const,
    eyebrow: 'Campaign planning',
    title: 'Your whole month, planned in minutes.',
    body: 'Tell Jarvis what’s coming up — a sale, a launch, an event. It suggests the emails, drafts them and lines them up for your approval.',
    points: ['Newsletters and promotions in one calendar', 'Suggestions based on what’s working', 'Approve with one tap'],
  },
  {
    kind: 'creative' as const,
    eyebrow: 'AI writing & design',
    title: 'Beautiful emails, in your voice.',
    body: 'Start from a template in your colours and fonts, or a few bullet points. Jarvis writes the copy and subject lines, and learns how you like to sound.',
    points: ['Templates in your brand', 'Copy and subject lines in your voice', 'Mistakes caught before you send'],
  },
  {
    kind: 'audience' as const,
    eyebrow: 'Smart audiences',
    title: 'The right message for every subscriber.',
    body: 'Jarvis groups your subscribers by what they love, so your best customers get early access, newcomers get a warm welcome, and no one gets too much.',
    points: ['Audiences built for you', 'Sign-up forms in minutes', 'Gentle check-ins for quiet subscribers'],
  },
  {
    kind: 'send' as const,
    eyebrow: 'Automations',
    title: 'Set it once. Jarvis keeps it running.',
    body: 'Welcome new subscribers, thank first-time customers, win back the ones who drifted and celebrate birthdays — on autopilot, always fresh.',
    points: ['Welcome series', 'Thank-you and win-back emails', 'Birthday and anniversary notes'],
  },
  {
    kind: 'protect' as const,
    eyebrow: 'Inbox-ready',
    title: 'Land in the inbox, not the spam folder.',
    body: 'Jarvis takes care of the technical side of getting delivered and keeps your list healthy, so your emails reach the people who want them.',
    points: ['Simple, guided setup', 'A healthy list, automatically', 'Honest stats you can trust'],
  },
  {
    kind: 'lake' as const,
    eyebrow: 'Insights',
    title: 'Know what worked — and what’s next.',
    body: 'Opens, clicks and sales, explained in plain English. Every week Jarvis tells you what worked and suggests one thing to try.',
    points: ['Sales from every email', 'Weekly tips from Jarvis', 'Easy A/B testing'],
  },
];

export const PlatformPage: React.FC = () => (
  <>
    <PageHero
      eyebrow="Features"
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

    <CtaBand title="See Jarvis build your next newsletter." body="A quick walkthrough: bring your list, draft a campaign, meet Jarvis." intent="Demo" />
  </>
);
