import React from 'react';
import { JarvisMark } from '../JarvisLogo';
import { FieldDef, LeadForm, USE_CASES, VOLUME_OPTIONS } from '../LeadForm';
import { Eyebrow } from '../parts';

const FormPage: React.FC<{ eyebrow: string; title: string; body: string; children: React.ReactNode }> = ({ eyebrow, title, body, children }) => (
  <section className="jv-section jv-section--tight jv-form-page">
    <div className="jv-wrap jv-split jv-split--top">
      <div>
        <Eyebrow>{eyebrow}</Eyebrow>
        <h1>{title}</h1>
        <p className="jv-lede">{body}</p>
        <div className="jv-form-guide">
          <JarvisMark size={72} title="" animated look />
        </div>
      </div>
      <div className="jv-form-card">{children}</div>
    </div>
  </section>
);

const EARLY: FieldDef[] = [
  { name: 'email', label: 'Work email', kind: 'email', required: true, autoComplete: 'email', wide: true },
  { name: 'use_case', label: 'Primary use case', kind: 'select', options: USE_CASES, required: true, wide: true },
];

export const EarlyAccessPage: React.FC = () => (
  <FormPage eyebrow="Early access" title="Request early access." body="Early access is planned for 2027. Tell us what you send and we’ll email you about availability.">
    <LeadForm
      kind="early_access"
      fields={EARLY}
      offerUpdates
      submitLabel="Request early access"
      success="Thanks. We’ve received your request. We’ll email you about availability."
    />
  </FormPage>
);

const DEMO: FieldDef[] = [
  { name: 'name', label: 'Name', kind: 'text', required: true, autoComplete: 'name' },
  { name: 'email', label: 'Work email', kind: 'email', required: true, autoComplete: 'email' },
  { name: 'company', label: 'Company', kind: 'text', required: true, autoComplete: 'organization', wide: true },
  { name: 'volume', label: 'Monthly email volume', kind: 'select', options: VOLUME_OPTIONS, required: true },
  { name: 'use_case', label: 'Primary use case', kind: 'select', options: USE_CASES, required: true },
  { name: 'current_platform', label: 'Current platform', kind: 'text', wide: true },
  { name: 'notes', label: 'Additional notes', kind: 'textarea' },
];

export const DemoPage: React.FC = () => (
  <FormPage
    eyebrow="Demo"
    title="See how Jarvis fits your email program."
    body="Tell us what you send, who you send it to and where your team spends the most time. Request a walkthrough focused on your workflow."
  >
    <LeadForm kind="demo" fields={DEMO} submitLabel="Request a demo" success="Thanks. We’ve received your request." />
  </FormPage>
);

const CONTACT: FieldDef[] = [
  { name: 'name', label: 'Name', kind: 'text', required: true, autoComplete: 'name' },
  { name: 'email', label: 'Email', kind: 'email', required: true, autoComplete: 'email' },
  { name: 'use_case', label: 'Topic', kind: 'select', options: ['Sales', 'Product support', 'Developer support'], required: true, wide: true },
  { name: 'notes', label: 'Message', kind: 'textarea', required: true },
];

export const ContactPage: React.FC = () => (
  <FormPage eyebrow="Contact" title="Talk to the right team." body="Ask about Jarvis, discuss an integration or get help with your account.">
    <LeadForm kind="contact" fields={CONTACT} submitLabel="Send message" success="Thanks. We’ve received your message." />
  </FormPage>
);
