import React from 'react';
import { NightShiftLoop } from '../Art';
import { JarvisMark } from '../JarvisLogo';
import { CtaBand, Eyebrow, PageHero } from '../parts';

const TRAITS = [
  { title: 'Loyal', tone: 'violet', body: 'Your goals are its goals. Jarvis works for your business, not for a vanity metric.' },
  { title: 'Always on', tone: 'cyan', body: 'Drafts overnight, watches your sends all day, and has your report ready before your coffee.' },
  { title: 'One step ahead', tone: 'pink', body: 'It spots the broken link, the tired subject line, the subscriber drifting away — and says so first.' },
  { title: 'Honest', tone: 'lime', body: 'Unverified numbers are labelled. Bad news comes early and in plain language.' },
];

const DAY = [
  { t: '06:00', what: 'This week’s newsletter is drafted from your latest posts and products, waiting for your thumbs-up.' },
  { t: '07:00', what: 'Your most loyal readers get it first, at the time they usually open.' },
  { t: 'All day', what: 'Bounces, spam complaints and broken links caught and fixed — anything bigger held for you.' },
  { t: '17:00', what: 'A two-line note: what’s working today and one idea for tomorrow.' },
  { t: 'Monday', what: 'Your weekly report: sales from email, list growth, and the one thing to try next.' },
];

const RULES = [
  'Asks before anything it can’t undo. New campaigns, big list changes and anything unusual wait for you.',
  'Shows its work. Every number comes with where it came from and when.',
  'Logs every change with a before, an after and the undo.',
  'Remembers your corrections and turns them into rules.',
  'Tells you when you’re wrong. Politely. Once.',
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
          <Eyebrow tone="lime">A day with Jarvis</Eyebrow>
          <h2>While you run the business, Jarvis runs the inbox.</h2>
          <p className="jv-lede">The same rhythm, every day, with the receipts to prove it.</p>
          <div className="jv-night-wrap">
            <NightShiftLoop />
          </div>
        </div>
        <ol className="jv-timeline">
          {DAY.map((d) => (
            <li key={d.t}>
              <span>{d.t}</span>
              <p>{d.what}</p>
            </li>
          ))}
        </ol>
      </div>
    </section>

    <section className="jv-section">
      <div className="jv-wrap jv-split">
        <div className="jv-proof-card jv-proof-card--violet">
          <JarvisMark size={72} mood="wink" />
          <blockquote>
            “I could have sent the sale to everyone at noon. I held your quiet subscribers until Saturday — they
            unsubscribe twice as often on Fridays. Want me to make that the rule?”
          </blockquote>
          <span>— Jarvis, in a typical weekly note</span>
        </div>
        <div>
          <Eyebrow tone="pink">House rules</Eyebrow>
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
