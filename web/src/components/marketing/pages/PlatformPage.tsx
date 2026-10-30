import React from 'react';
import { Spot } from '../Art';
import { CtaBand, EarlyAccessButton, Eyebrow, PageHero } from '../parts';

const FEATURES = [
  {
    kind: 'plan' as const,
    eyebrow: 'Send-day board',
    title: 'The whole day on one board.',
    body: 'Every brand, every mailbox provider, every wave in a single grid. Jarvis builds tomorrow tonight, clones what worked, rotates offers so nobody sees the same one three days running, and stages it for your approval.',
    points: ['Engaged-first anchor order', 'Offer fatigue rules built in', 'One-tap approve, one-step undo'],
  },
  {
    kind: 'send' as const,
    eyebrow: 'Multi-transport sending',
    title: 'Amazon SES and your own MTAs. One switch.',
    body: 'Route by sending profile: Amazon SES for reach, your own PowerMTA or KumoMTA servers for dedicated IP control. If one path pauses, Jarvis can move a domain to the other mid-day.',
    points: ['Per-provider routing and pools', 'Wave pacing and gentle throttles', 'Emergency transport swap'],
  },
  {
    kind: 'audience' as const,
    eyebrow: 'Audience intelligence',
    title: 'Real humans, not just opens.',
    body: 'Most "engagement" is machines: link scanners, image proxies, privacy prefetch. Jarvis grades people by what they did — converted, clicked, opened — and keeps tired addresses from dragging your reputation down.',
    points: ['Conversion > click > open grading', 'Unopened-five caps', 'Fresh data gated into one controlled send'],
  },
  {
    kind: 'creative' as const,
    eyebrow: 'Creative Studio',
    title: 'Every creative, subject and sender in one place.',
    body: 'Approved creatives, subject lines, preheaders and from-names live in the Studio. Jarvis only mails what you approved, then ranks it by what actually performs.',
    points: ['Approved pool only', 'Inbox-score checks before send', 'Performance-ranked subject rotation'],
  },
  {
    kind: 'protect' as const,
    eyebrow: 'Deliverability guardrails',
    title: 'A reputation bodyguard on every lane.',
    body: 'Hard bounces, policy blocks, velocity limits and complaint spikes, read straight from mailbox-provider replies. Bot clicks are challenged at the edge so your reports count people, not scanners.',
    points: ['Hard vs soft bounce split, always', 'Bot challenge on click links', 'Ramps that step, then hold'],
  },
  {
    kind: 'lake' as const,
    eyebrow: 'Analytics lake',
    title: 'One source of truth for every number.',
    body: 'Delivery truth across every transport lands in one lake. Every screen computes a metric the same way, so two dashboards never argue.',
    points: ['Delivered, not merely accepted', 'Per-provider, per-domain, per-wave', 'Daily reports that arrive on time'],
  },
  {
    kind: 'learn' as const,
    eyebrow: 'The Brain',
    title: 'Institutional memory that checks its own facts.',
    body: 'Jarvis records what it learns as claims with evidence attached. Facts get verified, wrong ones get superseded, and your rulings become the rules every future session follows.',
    points: ['Verified vs unverified, always labelled', 'Your corrections become policy', 'Shared by every agent on the team'],
  },
];

export const PlatformPage: React.FC = () => (
  <>
    <PageHero
      eyebrow="Platform"
      title={
        <>
          The sending platform with an agent <span className="jv-hl">built in</span>, not bolted on.
        </>
      }
      body="Jarvis isn't a chat window on top of an email tool. It's the operator inside the platform — reading the same data you do, using the same controls, and asking before anything it can't undo."
    >
      <div className="jv-cta-row">
        <EarlyAccessButton label="Get a demo" intent="Demo" />
      </div>
    </PageHero>

    <section className="jv-section jv-section--tight">
      <div className="jv-wrap jv-features">
        {FEATURES.map((f, i) => (
          <article key={f.title} className={`jv-feature${i % 2 ? ' jv-feature--flip' : ''}`}>
            <div className="jv-feature-art">
              <Spot kind={f.kind} size={120} />
            </div>
            <div className="jv-feature-copy">
              <Eyebrow>{f.eyebrow}</Eyebrow>
              <h2>{f.title}</h2>
              <p>{f.body}</p>
              <ul className="jv-checks">
                {f.points.map((p) => (
                  <li key={p}>{p}</li>
                ))}
              </ul>
            </div>
          </article>
        ))}
      </div>
    </section>

    <CtaBand title="See it run on real inboxes." body="A 30-minute walkthrough of a live send day — board, lanes, reports and the agent." intent="Demo" />
  </>
);
