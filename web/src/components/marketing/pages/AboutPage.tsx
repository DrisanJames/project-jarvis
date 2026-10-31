import React from 'react';
import { CtaBand, PageHero } from '../parts';

export const AboutPage: React.FC = () => (
  <>
    <PageHero
      eyebrow="About"
      title={
        <>
          Make email marketing <span className="jv-hl">easier to run.</span>
        </>
      }
      body="Jarvis is built around the everyday work of email marketing: creating a useful message, choosing an audience, preparing a send and understanding the response. We bring that work into one platform and use AI to help teams move through it."
    />
    <section className="jv-section jv-section--tight">
      <div className="jv-wrap jv-prose">
        <p>We believe useful AI should make its contribution clear. It should help people create, review and understand their work while keeping the team’s goals in view.</p>
        <p>Jarvis is a product of James Ventures Corp.</p>
      </div>
    </section>
    <CtaBand title="Follow along." body="Early access is planned for 2027." />
  </>
);
