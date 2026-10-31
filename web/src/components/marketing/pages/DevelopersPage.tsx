import React from 'react';
import { LeadForm } from '../LeadForm';
import { CopyBlocks, Eyebrow, PageHero } from '../parts';

const PLANS = [
  { title: 'Make your first request.', body: 'A short guide to credentials and a first read-only request, published with the API.' },
  { title: 'Build a useful workflow.', body: 'Worked examples for keeping contacts in sync and reading campaign reports.' },
  { title: 'Find the details you need.', body: 'Fields, permissions, limits and errors for each documented operation.' },
];

export const DevelopersPage: React.FC = () => (
  <>
    <PageHero
      eyebrow="Developers · Preview"
      title={
        <>
          Build Jarvis into the way <span className="jv-hl">your business works.</span>
        </>
      }
      body="The Jarvis API isn’t public yet. Tell us what you want to build and we’ll let you know when access opens."
    />
    <section className="jv-section jv-section--tight">
      <div className="jv-wrap">
        <div className="jv-section-head">
          <Eyebrow>What we’re planning</Eyebrow>
          <h2>Documentation you can run.</h2>
        </div>
        <CopyBlocks items={PLANS} />
      </div>
    </section>
    <section className="jv-section jv-section--cream">
      <div className="jv-wrap jv-split jv-split--top">
        <div>
          <Eyebrow tone="tangerine">Preview</Eyebrow>
          <h2>Request API access.</h2>
          <p className="jv-lede">Tell us about your project so we can shape the API around real work.</p>
        </div>
        <LeadForm
          kind="api_access"
          submitLabel="Request API access"
          success="Thanks. We’ve received your request. We’ll email you about availability."
          fields={[
            { name: 'name', label: 'Name', kind: 'text', required: true, autoComplete: 'name' },
            { name: 'email', label: 'Work email', kind: 'email', required: true, autoComplete: 'email' },
            { name: 'company', label: 'Company', kind: 'text', required: true, autoComplete: 'organization', wide: true },
            { name: 'use_case', label: 'What do you want to build?', kind: 'text', required: true, wide: true },
            { name: 'notes', label: 'Anything else', kind: 'textarea' },
          ]}
        />
      </div>
    </section>
  </>
);
