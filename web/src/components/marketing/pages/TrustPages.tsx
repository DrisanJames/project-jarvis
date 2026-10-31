import React from 'react';
import { CopyBlocks, PageHero, PageLink } from '../parts';

const FACTS = [
  { title: 'Sign-in through Google', body: 'Jarvis has no separate password. Sign-in goes through Google, and Google must confirm your email address.' },
  { title: 'Access is approved', body: 'Signing in with Google does not give an outside account access. New accounts complete a short form, then we reach out.' },
  { title: 'Opt-outs are kept', body: 'Unsubscribes and suppressed addresses are checked before email is sent.' },
];

export const TrustPage: React.FC = () => (
  <>
    <PageHero
      eyebrow="Trust"
      title={
        <>
          Understand how Jarvis <span className="jv-hl">handles your data.</span>
        </>
      }
      body="Here is what we can tell you today. We’ll add to this page as early access gets closer."
    />
    <section className="jv-section jv-section--tight">
      <div className="jv-wrap">
        <CopyBlocks items={FACTS} heading="h2" />
        <p className="jv-fine">
          Questions? <PageLink to="contact">Contact us</PageLink>. See also our <PageLink to="privacy">privacy notice</PageLink>.
        </p>
      </div>
    </section>
  </>
);

export const PrivacyPage: React.FC = () => (
  <>
    <PageHero eyebrow="Privacy" title="Privacy notice" body="This notice covers the forms on this website." />
    <section className="jv-section jv-section--tight">
      <div className="jv-wrap jv-prose">
        <h2>What we collect</h2>
        <p>
          When you fill in a form, we collect what you enter: such as your name, email address, company and what you’re
          interested in. If you sign in with Google, we receive your name and verified email address from Google.
        </p>
        <p>To keep automated abuse out, we also record your browser’s user agent and a one-way hash of your IP address.</p>
        <h2>Why we collect it</h2>
        <p>We use these details to answer your request and to contact you about Jarvis. We send product updates only if you tick the box for them.</p>
        <h2>Who sees it</h2>
        <p>The Jarvis team at James Ventures Corp reads these requests.</p>
        <h2>Contact</h2>
        <p>
          To ask about your details or have them removed, <PageLink to="contact">contact us</PageLink>.
        </p>
      </div>
    </section>
  </>
);
