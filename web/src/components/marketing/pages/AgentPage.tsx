import React from 'react';
import { NightShiftLoop } from '../Art';
import { JarvisMark } from '../JarvisLogo';
import { CtaBand, Eyebrow, PageHero } from '../parts';

const TRAITS = [
  { title: 'Loyal', tone: 'violet', body: 'Your goals are its goals. Jarvis works for your business, not for a vanity metric.' },
  { title: 'Always on', tone: 'cyan', body: 'Nights, weekends, holidays. Jarvis is there whenever your business needs it.' },
  { title: 'One step ahead', tone: 'pink', body: 'It spots the broken link, the tired subject line, the subscriber drifting away — and says so first.' },
  { title: 'Honest', tone: 'lime', body: 'Straight answers in plain language — good news and bad.' },
];

const HELPS = [
  'Drafts your next newsletter before you’ve thought about it.',
  'Picks the best moment to send for every subscriber.',
  'Catches mistakes before your customers do.',
  'Tells you what worked — in plain English.',
];

const RULES = [
  'Nothing goes out without your approval.',
  'Every suggestion comes with the reason why.',
  'Every change comes with an undo.',
  'It remembers your preferences, so you only say things once.',
  'It tells you when there’s a better way. Politely. Once.',
];

export const AgentPage: React.FC = () => (
  <>
    <PageHero
      eyebrow="Meet Jarvis"
      title={
        <>
          The helper who’s <span className="jv-hl">always there</span> — and occasionally a step ahead of you.
        </>
      }
      body="Jarvis is the marketing teammate most small businesses can’t afford to hire: tireless, loyal, a little witty, and smart enough to catch what you missed. It does the work, explains the work, and lets you take the credit."
    />

    <section className="jv-section jv-section--tight">
      <div className="jv-wrap jv-card-grid jv-card-grid--4">
        {TRAITS.map((t) => (
          <article key={t.title} className={`jv-card jv-card--${t.tone}`}>
            <h3>{t.title}</h3>
            <p>{t.body}</p>
          </article>
        ))}
      </div>
    </section>

    <section className="jv-section jv-section--ink">
      <div className="jv-wrap jv-split">
        <div>
          <Eyebrow tone="lime">Works while you sleep</Eyebrow>
          <h2>While you run the business, Jarvis runs the inbox.</h2>
          <p className="jv-lede">Wake up to emails drafted, sends scheduled and a short note on how yesterday went.</p>
          <div className="jv-night-wrap">
            <NightShiftLoop />
          </div>
        </div>
        <ul className="jv-helps">
          {HELPS.map((h) => (
            <li key={h}>{h}</li>
          ))}
        </ul>
      </div>
    </section>

    <section className="jv-section">
      <div className="jv-wrap jv-split">
        <div className="jv-proof-card jv-proof-card--violet">
          <JarvisMark size={72} mood="wink" />
          <blockquote>
            “I could have sent the sale to everyone at noon. I saved your quiet subscribers for Saturday morning — they
            tend to read on weekends. Want me to make that a habit?”
          </blockquote>
          <span>— Jarvis</span>
        </div>
        <div>
          <Eyebrow tone="pink">You’re always in charge</Eyebrow>
          <h2>Smart enough to act. Wise enough to ask.</h2>
          <ul className="jv-checks">
            {RULES.map((r) => (
              <li key={r}>{r}</li>
            ))}
          </ul>
        </div>
      </div>
    </section>

    <CtaBand title="Hire the best right hand in email." body="No onboarding marathon. Import your list, add your logo, meet Jarvis." />
  </>
);
