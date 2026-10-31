import React from 'react';
import { JarvisMark } from '../JarvisLogo';
import { CampaignPreview } from '../ProductPreview';
import { Availability, CopyBlocks, CtaBand, EarlyAccessLink, Eyebrow, PageHero, PageLink } from '../parts';

const BLOCKS = [
  { title: 'Move past the blank page.', body: 'Turn a short brief into a draft and explore subject line options. Edit the result to match your message and your brand.' },
  { title: 'Understand the campaign in front of you.', body: 'Ask clear questions about the data in your account. Get explanations that help you decide where to look next.' },
  { title: 'Stay in charge of the send.', body: 'A suggestion is only a suggestion. Campaigns stay drafts until someone on your team schedules or sends them.' },
];

const FLOW = [
  { n: 'Request', body: 'You ask for a welcome email for new subscribers.' },
  { n: 'Suggestion', body: 'Jarvis offers a draft and two subject lines.' },
  { n: 'Draft', body: 'You edit the message and choose the audience.' },
  { n: 'Sent', body: 'You schedule it when you’re happy with it.' },
];

export const AiPage: React.FC = () => (
  <>
    <PageHero
      eyebrow="AI"
      title={
        <>
          Give your email team <span className="jv-hl">more room</span> to do its best work.
        </>
      }
      body="Jarvis helps with the writing, review and interpretation that sit behind a campaign. Start with a useful suggestion, ask for a different approach and make the final call."
    >
      <div className="jv-cta-row">
        <EarlyAccessLink />
        <PageLink to="platform" className="jv-btn jv-btn--ghost">
          Explore the platform
        </PageLink>
      </div>
      <Availability />
    </PageHero>

    <section className="jv-section jv-section--tight">
      <div className="jv-wrap">
        <CopyBlocks items={BLOCKS} heading="h2" />
      </div>
    </section>

    <section className="jv-section jv-section--cream">
      <div className="jv-wrap jv-split">
        <div>
          <Eyebrow tone="tangerine">From request to send</Eyebrow>
          <h2>Know what is a suggestion and what is a send.</h2>
          <ol className="jv-steps jv-steps--list">
            {FLOW.map((s) => (
              <li key={s.n}>
                <h3>{s.n}</h3>
                <p>{s.body}</p>
              </li>
            ))}
          </ol>
        </div>
        <CampaignPreview />
      </div>
    </section>

    <section className="jv-section">
      <div className="jv-wrap jv-split">
        <div className="jv-proof-card">
          <JarvisMark size={64} title="Jarvis" mood="wink" />
          <blockquote>“I’ll get you started. You make the call.”</blockquote>
          <span>Jarvis, your guide in the product</span>
        </div>
        <div>
          <Eyebrow tone="pink">Meet Jarvis</Eyebrow>
          <h2>A helper, in the campaign you’re working on.</h2>
          <p className="jv-lede">Jarvis works next to your draft and your reports, so you can ask for help without leaving the work.</p>
        </div>
      </div>
    </section>

    <CtaBand title="Try Jarvis on your next campaign." body="Early access is planned for 2027. Join the list for updates." />
  </>
);
