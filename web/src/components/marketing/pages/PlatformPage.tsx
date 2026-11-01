import React from 'react';
import { JarvisMark } from '../JarvisLogo';
import { AudiencePreview, AutomationPreview, CampaignPreview, ReportPreview } from '../ProductPreview';
import { Availability, CtaBand, EarlyAccessLink, Eyebrow, PageHero, PageLink } from '../parts';

const CHAPTERS = [
  {
    id: 'campaigns',
    tone: 'violet',
    eyebrow: 'Campaigns',
    title: 'From first idea to ready to send.',
    body: 'Build newsletters, promotions and announcements around your brand. Use AI to help shape the copy, then check the subject line, sender, audience and schedule before it goes out.',
    points: ['Start from a message, a template or a short brief', 'Preview the campaign before it sends', 'Start every campaign as a draft'],
    view: <CampaignPreview />,
  },
  {
    id: 'audiences',
    tone: 'lime',
    eyebrow: 'Audiences',
    title: 'Put the right audience behind every campaign.',
    body: 'Organize your subscribers using the details and engagement available in Jarvis. Build focused groups for messages that make sense to the people receiving them.',
    points: ['Import contacts from a file', 'Group people by fields and activity', 'Keep unsubscribes and exclusions out of your sends'],
    view: <AudiencePreview />,
  },
  {
    id: 'automation',
    tone: 'cyan',
    eyebrow: 'Automation',
    title: 'Keep your follow-ups moving.',
    body: 'Build email sequences for the moments that matter to your audience. Set the messages and timing, then watch the journey as it runs.',
    points: ['Introduce new subscribers with a welcome series', 'Decide what comes next and when', 'Refine the sequence as your program develops'],
    view: <AutomationPreview />,
  },
  {
    id: 'analytics',
    tone: 'pink',
    eyebrow: 'Reporting',
    title: 'See what happened. Know where to look next.',
    body: 'Bring campaign results into one view. Review sending outcomes, clicks and other engagement together, and look for patterns across campaigns instead of judging a send by one number.',
    points: ['Sending outcomes for each campaign', 'Compare activity across sends', 'Ask Jarvis to explain what you’re looking at'],
    view: <ReportPreview />,
  },
];

export const PlatformPage: React.FC = () => {
  return (
    <>
      <PageHero
        eyebrow="Platform"
        title={
          <>
            Everything behind your next send, <span className="jv-hl">in one place.</span>
          </>
        }
        body="Manage your audiences, build campaigns and keep follow-ups moving with Jarvis. AI assistance fits into the work, so your team can spend more time on the message and the people receiving it."
      >
        <div className="jv-cta-row">
          <EarlyAccessLink />
          <PageLink to="ai" className="jv-btn jv-btn--ghost">
            See how AI helps
          </PageLink>
        </div>
        <Availability />
      </PageHero>

      <section className="jv-section jv-section--tight">
        <div className="jv-wrap jv-chapters">
          {CHAPTERS.map((c, i) => (
            <article key={c.id} id={c.id} className={`jv-chapter jv-chapter--${c.tone}${i % 2 ? ' jv-chapter--flip' : ''}`}>
              <div className="jv-chapter-stage">
                <span className="jv-plx-blob" data-speed="-0.14" aria-hidden="true" />
                <span className="jv-plx-ring" data-speed="0.2" aria-hidden="true" />
                <div className="jv-plx-view" data-speed="-0.05">
                  {c.view}
                </div>
                {i === 0 && (
                  <div className="jv-plx-guide" data-speed="0.12">
                    <JarvisMark size={56} title="" animated look />
                  </div>
                )}
              </div>
              <div className="jv-chapter-copy">
                <Eyebrow>{c.eyebrow}</Eyebrow>
                <h2>{c.title}</h2>
                <p>{c.body}</p>
                <ul className="jv-checks">
                  {c.points.map((p) => (
                    <li key={p}>{p}</li>
                  ))}
                </ul>
              </div>
            </article>
          ))}
        </div>
      </section>

      <section className="jv-section jv-section--cream">
        <div className="jv-wrap">
          <div className="jv-section-head">
            <Eyebrow tone="tangerine">Sending health</Eyebrow>
            <h2>Make sending health part of the workflow.</h2>
            <p className="jv-lede">
              Review delivery outcomes and identify changes that deserve a closer look. Respect unsubscribes and manage
              exclusions across your sends, so your audience stays ready for the next campaign.
            </p>
          </div>
        </div>
      </section>

      <CtaBand title="A clear path from draft to results." body="Prepare your content, select the right audience, review the details and learn from what happened." />
    </>
  );
};
