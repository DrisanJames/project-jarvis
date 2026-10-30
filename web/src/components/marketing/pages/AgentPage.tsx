import React from 'react';
import { NightShiftLoop } from '../Art';
import { JarvisMark } from '../JarvisLogo';
import { CtaBand, Eyebrow, PageHero } from '../parts';

const TRAITS = [
  { title: 'Loyal', tone: 'violet', body: 'Your goals are its goals. Jarvis works for your program, not for a vanity metric.' },
  { title: 'Always on', tone: 'cyan', body: 'Overnight builds, 15-minute checks, the morning report waiting before your coffee.' },
  { title: 'One step ahead', tone: 'pink', body: "It sees the block coming, the list going stale, the offer wearing out — and says so first." },
  { title: 'Honest', tone: 'lime', body: 'Unverified numbers are labelled. Bad news comes early and in plain language.' },
];

const DAY = [
  { t: '01:01', what: 'Your most engaged readers get mail first and warm up the inbox for everyone else.' },
  { t: '02:01', what: 'Fresh sign-ups go out as one controlled send, sized by yesterday’s results.' },
  { t: '07:00', what: 'Your report lands: money, delivery, what Jarvis changed overnight, and why.' },
  { t: 'All day', what: 'Every 15 minutes: lanes checked, stalls repaired, anything risky held for you.' },
  { t: '21:00', what: 'Tomorrow is planned — hold or grow, never a surprise cut — and staged for approval.' },
];

const RULES = [
  'Asks before anything it can’t undo. Deploys, live changes and big volume moves wait for you.',
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
      body="Jarvis is the right hand every marketer wishes they had: tireless, loyal, a little witty, and smart enough to catch what you missed. It does the work, explains the work, and lets you take the credit."
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
          <h2>While you sleep, Jarvis runs the night shift.</h2>
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
            “I could have doubled tomorrow’s volume. I held it instead — Outlook was deferring 11% at noon. I’ve set a
            reminder to step up Thursday if it clears.”
          </blockquote>
          <span>— Jarvis, in a typical morning report</span>
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

    <CtaBand title="Hire the best right hand in email." body="No onboarding marathon. Connect a domain, set your goals, meet Jarvis." />
  </>
);
