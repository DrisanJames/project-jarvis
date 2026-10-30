import React from 'react';
import { Spot } from '../Art';
import { EarlyAccessButton, Eyebrow, PageHero } from '../parts';

// Every service listed here is one the production platform runs on today
// (go.mod AWS SDK v2 modules + the ECS/WAF/CloudFront estate in us-west-2).
const STACK = [
  { svc: 'Amazon SES', use: 'High-reach delivery for every brand, with event tracking feeding the lake.' },
  { svc: 'Amazon Bedrock', use: 'Foundation-model access for agent and creative workloads.' },
  { svc: 'Amazon ECS', use: 'The platform, its worker fleet and the agent jobs run as ECS services and scheduled tasks.' },
  { svc: 'Amazon EventBridge', use: 'Schedules the agent’s nightly builds, 15-minute checks and morning reports.' },
  { svc: 'Amazon RDS for PostgreSQL', use: 'Subscribers, campaigns, waves and the Brain’s verified memory.' },
  { svc: 'Amazon S3 + Athena', use: 'The analytics lake: delivery truth across every transport, queried in SQL.' },
  { svc: 'Amazon Data Firehose', use: 'Streams sending and engagement events into the lake.' },
  { svc: 'AWS WAF Bot Control', use: 'Challenges bot clicks at the edge so reports count people.' },
  { svc: 'Amazon CloudFront', use: 'Tracking and click edge for every sending domain.' },
  { svc: 'Amazon Route 53 + ACM', use: 'DNS and certificates for every sending and tracking domain.' },
  { svc: 'Amazon SQS + DynamoDB', use: 'Event intake and site sign-up state.' },
  { svc: 'AWS Lambda', use: 'Sign-up confirmation and edge automation.' },
];

const ASKS = [
  {
    kind: 'learn' as const,
    title: 'Scale the agent on Bedrock',
    body: 'Move every lane — not just the busiest — onto continuous agent supervision.',
  },
  {
    kind: 'send' as const,
    title: 'Grow on Amazon SES',
    body: 'Bring design-partner senders onto SES with Jarvis managing their reputation from day one.',
  },
  {
    kind: 'gate' as const,
    title: 'Go to market together',
    body: 'Launch in 2027 as an AWS-native email platform for the next generation of marketers.',
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
      body="Jarvis runs entirely on AWS in us-west-2 — sending, storage, analytics, edge security and the agent itself. We built it in production on our own portfolio first, and we’re ready to bring it to market."
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
          <h2>From our own inboxes to every young marketing team.</h2>
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
