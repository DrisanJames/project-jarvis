import React from 'react';
import { LeadForm } from '../LeadForm';
import { CopyBlocks, Eyebrow, PageHero, PageLink } from '../parts';

const WAYS = [
  { title: 'File import', body: 'Bring your subscribed contacts and your opt-outs into Jarvis from a file today.' },
  { title: 'Native integrations', body: 'We’re choosing which connections to build first. Your request helps us decide.' },
  { title: 'Build with the API', body: 'A public API isn’t available yet. Developers can request access.' },
];

export const IntegrationsPage: React.FC = () => (
  <>
    <PageHero
      eyebrow="Integrations"
      title={
        <>
          Make Jarvis part of the way <span className="jv-hl">your business works.</span>
        </>
      }
      body="Bring your customer data and campaign workflows closer together. Tell us which tools you use, and we’ll let you know as connections become available."
    />
    <section className="jv-section jv-section--tight">
      <div className="jv-wrap">
        <CopyBlocks items={WAYS} heading="h2" />
        <div className="jv-cta-row jv-cta-row--center">
          <PageLink to="developers" className="jv-btn jv-btn--ghost">
            Visit the developer page
          </PageLink>
        </div>
      </div>
    </section>
    <section className="jv-section jv-section--cream">
      <div className="jv-wrap jv-split jv-split--top">
        <div>
          <Eyebrow tone="tangerine">Request an integration</Eyebrow>
          <h2>Tell us what you want to connect.</h2>
          <p className="jv-lede">Name the tool and what you’d want to flow between it and Jarvis.</p>
        </div>
        <LeadForm
          kind="contact"
          fixedUseCase="Integration request"
          submitLabel="Send request"
          success="Thanks. We’ve received your request."
          fields={[
            { name: 'name', label: 'Name', kind: 'text', required: true, autoComplete: 'name' },
            { name: 'email', label: 'Work email', kind: 'email', required: true, autoComplete: 'email' },
            { name: 'notes', label: 'What do you want to connect?', kind: 'textarea', required: true },
          ]}
        />
      </div>
    </section>
  </>
);
