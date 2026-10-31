import React from 'react';
import { CtaBand, EarlyAccessLink, Eyebrow, PageHero, PageLink } from '../parts';

const STEPS = [
  { title: 'Map', body: 'Map your contacts, subscription preferences, exclusions and essential workflows.' },
  { title: 'Import', body: 'Import supported data and recreate the templates and sequences your team needs first.' },
  { title: 'Review', body: 'Review the setup, check the first campaign and confirm your reporting before expanding the program.' },
];

export const MigrationPage: React.FC = () => (
  <>
    <PageHero
      eyebrow="Migration"
      title={
        <>
          Make your next chapter in email <span className="jv-hl">easier to start.</span>
        </>
      }
      body="Bring your audience and the workflows that matter most. Plan the move to Jarvis around your current program, then prepare a first campaign you can review with confidence."
    >
      <div className="jv-cta-row">
        <PageLink to="contact" className="jv-btn jv-btn--primary">
          Discuss your migration
        </PageLink>
        <EarlyAccessLink variant="ghost" />
      </div>
    </PageHero>
    <section className="jv-section jv-section--tight">
      <div className="jv-wrap">
        <div className="jv-section-head">
          <Eyebrow>Three steps</Eyebrow>
          <h2>A plan you can follow.</h2>
        </div>
        <ol className="jv-steps jv-steps--3">
          {STEPS.map((s, i) => (
            <li key={s.title}>
              <span className="jv-step-n">{i + 1}</span>
              <h3>{s.title}</h3>
              <p>{s.body}</p>
            </li>
          ))}
        </ol>
        <p className="jv-fine">Only import people who asked to hear from you. History, templates and automations from another provider do not move automatically.</p>
      </div>
    </section>
    <CtaBand title="Moving from another email tool?" body="Tell us what you use today and what you need to keep." />
  </>
);
