import React from 'react';
import { Spot } from '../Art';
import { EarlyAccessButton, Eyebrow, PageHero } from '../parts';

// Every service listed here is one the production platform runs on today
// (go.mod AWS SDK v2 modules + the ECS/WAF/CloudFront stack in us-west-2).
const STACK = [
  { svc: 'Amazon SES', use: 'Reliable, high-reputation delivery for every newsletter and promotion.' },
  { svc: 'Amazon Bedrock', use: 'Foundation models behind Jarvis’s drafts, subject lines and suggestions.' },
  { svc: 'Amazon ECS', use: 'The app, the sending workers and Jarvis’s scheduled jobs.' },
  { svc: 'Amazon EventBridge', use: 'Schedules sends, daily checks and the weekly report.' },
  { svc: 'Amazon RDS for PostgreSQL', use: 'Contacts, campaigns and Jarvis’s verified memory.' },
  { svc: 'Amazon S3 + Athena', use: 'Reporting: every delivery, open and click, queryable in SQL.' },
  { svc: 'Amazon Data Firehose', use: 'Streams email events into reporting in near real time.' },
  { svc: 'AWS WAF Bot Control', use: 'Filters robot clicks at the edge so reports stay honest.' },
  { svc: 'Amazon CloudFront', use: 'Fast, secure link tracking on every sending domain.' },
  { svc: 'Amazon Route 53 + ACM', use: 'DNS and certificates for sending and tracking domains.' },
  { svc: 'Amazon SQS + DynamoDB', use: 'Event intake and sign-up form state.' },
  { svc: 'AWS Lambda', use: 'Sign-up confirmations and lightweight automation.' },
];

const ASKS = [
  {
    kind: 'learn' as const,
    title: 'Scale Jarvis on Bedrock',
    body: 'Give every small business a full-time AI marketing teammate, not just the ones who can hire one.',
  },
  {
    kind: 'send' as const,
    title: 'Grow on Amazon SES',
    body: 'Bring thousands of small senders onto SES with Jarvis protecting their reputation from day one.',
  },
  {
    kind: 'gate' as const,
    title: 'Go to market together',
    body: 'Launch in 2027 as the AWS-native email platform for small and medium businesses.',
  },
];

export const AwsPage: React.FC = () => (
  <>
    <PageHero
      eyebrow="Built on AWS"
      title={
        <>
          AWS-native, <span className="jv-hl">end to end.</span>
        </>
      }
      body="Jarvis runs entirely on AWS — sending, storage, reporting, edge security and the agent itself. We proved it at volume on our own sending first; now we’re bringing it to small and medium businesses."
    >
      <div className="jv-cta-row">
        <EarlyAccessButton label="Partner with us" intent="AWS partnership" />
      </div>
    </PageHero>

    <section className="jv-section jv-section--tight">
      <div className="jv-wrap">
        <div className="jv-section-head">
          <Eyebrow tone="cyan">The stack</Eyebrow>
          <h2>Twelve AWS services. One agent orchestrating them.</h2>
        </div>
        <div className="jv-stack">
          {STACK.map((s) => (
            <div key={s.svc} className="jv-stack-item">
              <h3>{s.svc}</h3>
              <p>{s.use}</p>
            </div>
          ))}
        </div>
      </div>
    </section>

    <section className="jv-section jv-section--ink">
      <div className="jv-wrap">
        <div className="jv-section-head">
          <Eyebrow tone="lime">What we’re building next with AWS</Eyebrow>
          <h2>From our own sending to every small business.</h2>
        </div>
        <div className="jv-card-grid jv-card-grid--3">
          {ASKS.map((a) => (
            <article key={a.title} className="jv-card jv-card--dark">
              <Spot kind={a.kind} />
              <h3>{a.title}</h3>
              <p>{a.body}</p>
            </article>
          ))}
        </div>
        <div className="jv-cta-row jv-cta-row--center">
          <EarlyAccessButton label="Talk to the founders" intent="AWS partnership" />
        </div>
      </div>
    </section>
  </>
);
