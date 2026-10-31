import React from 'react';
import { Availability, CtaBand, EarlyAccessLink, Eyebrow, PageHero } from '../parts';

const SOLUTIONS = [
  {
    id: 'newsletters',
    tone: 'violet',
    eyebrow: 'Newsletters and publishers',
    title: 'Keep your next edition moving.',
    body: 'Give every newsletter a consistent home, from the first draft to the next send. Jarvis helps you prepare content, organize subscriber groups and review how each edition performs.',
    follow: 'Build a rhythm your readers recognize. Use a repeatable publishing workflow for regular editions, announcements and welcome messages.',
    examples: ['Weekly publication', 'New-subscriber welcome series', 'A targeted update for an interested subscriber group'],
  },
  {
    id: 'marketing-teams',
    tone: 'cyan',
    eyebrow: 'Marketing teams',
    title: 'Give every campaign a clearer path to launch.',
    body: 'Bring announcements, promotions and customer follow-ups into a shared workflow. Use Jarvis to help create the message and keep the campaign details organized.',
    follow: 'Make room for the next good idea. Reduce repetitive campaign preparation and keep useful results close to the next decision.',
    examples: ['Product announcement', 'Event invitation', 'Promotion', 'Customer onboarding sequence'],
  },
];

export const SolutionsPage: React.FC = () => (
  <>
    <PageHero
      eyebrow="Solutions"
      title={
        <>
          Build an email program that <span className="jv-hl">fits your business.</span>
        </>
      }
      body="Start with the messages you need to send and the people you need to reach. Jarvis brings the campaign workflow and AI assistance together around that work."
    >
      <div className="jv-cta-row">
        <EarlyAccessLink />
      </div>
      <Availability />
    </PageHero>

    {SOLUTIONS.map((s, i) => (
      <section key={s.id} id={s.id} className={`jv-section${i % 2 ? ' jv-section--cream' : ' jv-section--tight'}`}>
        <div className="jv-wrap jv-split jv-split--top">
          <div>
            <Eyebrow>{s.eyebrow}</Eyebrow>
            <h2>{s.title}</h2>
            <p className="jv-lede">{s.body}</p>
            <p className="jv-lede">{s.follow}</p>
          </div>
          <article className={`jv-card jv-card--${s.tone}`}>
            <h3>Useful examples</h3>
            <ul className="jv-checks">
              {s.examples.map((e) => (
                <li key={e}>{e}</li>
              ))}
            </ul>
          </article>
        </div>
      </section>
    ))}

    <CtaBand title="Tell us what you send." body="We’ll keep you posted on early access." />
  </>
);
