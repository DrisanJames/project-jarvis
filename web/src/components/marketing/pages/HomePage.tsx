import React, { useEffect, useRef, useState } from 'react';
import { HeroScene, InboxSorterLoop, Spot } from '../Art';
import { JarvisMark } from '../JarvisLogo';
import { CtaBand, EarlyAccessButton, Eyebrow, PageLink, useMarketingNav } from '../parts';

const PILLARS = [
  {
    kind: 'plan' as const,
    tone: 'violet',
    title: 'Create',
    body: 'Tell Jarvis what’s coming — a sale, a launch, a new post. It plans the emails, drafts them in your voice and lines them up for your nod.',
  },
  {
    kind: 'send' as const,
    tone: 'cyan',
    title: 'Send',
    body: 'Each subscriber gets your email when they usually read. Jarvis handles the inbox-placement homework so you don’t have to.',
  },
  {
    kind: 'protect' as const,
    tone: 'pink',
    title: 'Protect',
    body: 'Broken links, bounces, spam complaints, robot clicks. Jarvis catches them before your customers — or the inboxes — do.',
  },
  {
    kind: 'audience' as const,
    tone: 'lime',
    title: 'Grow',
    body: 'Sign-up forms, a warm welcome series and smart segments that turn new subscribers into regulars.',
  },
];

const STEPS = [
  { n: '01', title: 'Notices', body: 'Opens, clicks, sales, unsubscribes and replies — all day, every day.' },
  { n: '02', title: 'Proposes', body: 'A plain-language plan with the numbers behind it. No jargon, no mystery.' },
  { n: '03', title: 'Acts within your rules', body: 'Small fixes happen on their own. Anything bigger waits for your yes.' },
  { n: '04', title: 'Remembers', body: 'Your brand voice, your rules, every correction. Plus an undo for every change.' },
];

export const HomePage: React.FC = () => {
  const { go } = useMarketingNav();
  return (
    <>
      <section className="jv-hero">
        <div className="jv-wrap jv-hero-grid">
          <div className="jv-hero-copy">
            <Eyebrow tone="pink">Email marketing with an AI right hand</Eyebrow>
            <h1>
              Your newsletter, <span className="jv-hl">already handled.</span>
            </h1>
            <p className="jv-lede">
              Jarvis writes, schedules and sends your newsletters and promotions — and usually spots the problem before
              you do. Built for small teams with big plans.
            </p>
            <div className="jv-cta-row">
              <EarlyAccessButton />
              <button type="button" className="jv-btn jv-btn--ghost" onClick={() => go('home', 'watch')}>
                Watch Jarvis work
              </button>
            </div>
            <p className="jv-fine">Free to start when we open in 2027. No card, no sales script.</p>
          </div>
          <HeroScene />
        </div>
      </section>

      <section className="jv-runs-on">
        <div className="jv-wrap">
          <span>Runs on AWS</span>
          <ul>
            {['Amazon SES', 'Amazon Bedrock', 'Amazon ECS', 'Amazon RDS', 'Amazon Athena', 'AWS WAF', 'Amazon CloudFront'].map((s) => (
              <li key={s}>{s}</li>
            ))}
          </ul>
        </div>
      </section>

      <section className="jv-section">
        <div className="jv-wrap">
          <div className="jv-section-head">
            <Eyebrow>One teammate, four jobs</Eyebrow>
            <h2>The marketing teammate you couldn’t afford. Until now.</h2>
          </div>
          <div className="jv-card-grid jv-card-grid--4">
            {PILLARS.map((p) => (
              <article key={p.title} className={`jv-card jv-card--${p.tone}`}>
                <Spot kind={p.kind} />
                <h3>{p.title}</h3>
                <p>{p.body}</p>
                <PageLink to="platform" className="jv-arrow-link">
                  Learn more
                </PageLink>
              </article>
            ))}
          </div>
        </div>
      </section>

      <section className="jv-section jv-section--ink" id="watch">
        <div className="jv-wrap jv-split">
          <div>
            <Eyebrow tone="lime">Watch Jarvis work</Eyebrow>
            <h2>You ask for one thing. Jarvis delivers the thing you meant.</h2>
            <p className="jv-lede">
              Jarvis follows your lead — and when a plan would cost you subscribers or sales, it says so, fixes it, and
              tells you exactly what changed. Every time.
            </p>
            <PageLink to="agent" className="jv-btn jv-btn--light">
              Meet the agent
            </PageLink>
          </div>
          <ChatDemo />
        </div>
      </section>

      <section className="jv-section">
        <div className="jv-wrap">
          <div className="jv-section-head">
            <Eyebrow tone="cyan">How Jarvis thinks</Eyebrow>
            <h2>A loop that never sleeps, and never goes rogue.</h2>
          </div>
          <div className="jv-loop-card">
            <InboxSorterLoop />
          </div>
          <ol className="jv-steps">
            {STEPS.map((s) => (
              <li key={s.n}>
                <span className="jv-step-n">{s.n}</span>
                <h3>{s.title}</h3>
                <p>{s.body}</p>
              </li>
            ))}
          </ol>
        </div>
      </section>

      <section className="jv-section jv-section--cream">
        <div className="jv-wrap jv-split">
          <div className="jv-proof-card">
            <JarvisMark size={64} />
            <blockquote>
              “Jarvis wasn’t designed in a pitch deck. It grew up running a real sending platform that delivers millions of
              emails every week.”
            </blockquote>
            <span>— The Jarvis team</span>
          </div>
          <div>
            <Eyebrow tone="tangerine">Built in production first</Eyebrow>
            <h2>Battle-tested at scale before it touches your list.</h2>
            <ul className="jv-checks">
              <li>Your most loyal subscribers hear from you first.</li>
              <li>Gmail, Outlook, Yahoo and Apple Mail each get the care they need.</li>
              <li>Every change Jarvis makes is logged with an undo.</li>
              <li>Big moves always wait for your yes.</li>
            </ul>
          </div>
        </div>
      </section>

      <CtaBand
        title="Let Jarvis take it from here."
        body="Join the 2027 early-access list. Bring your list; Jarvis brings the rest."
      />
    </>
  );
};

const CHAT: { who: 'you' | 'jarvis'; text: string; wink?: boolean }[] = [
  { who: 'you', text: 'Send the spring sale email to everyone tonight.' },
  {
    who: 'jarvis',
    text: "Scheduled — with two tweaks. 1,200 people haven't opened anything since last spring, so they get a friendly “still want to hear from us?” note instead. And your regular buyers get it at 7am, when they usually read.",
  },
  {
    who: 'jarvis',
    wink: true,
    text: 'Fewer unsubscribes, more sales. If you\'d rather send it to everyone at once, say "undo" and I will. I won\'t even sigh.',
  },
  { who: 'you', text: '…fine. Keep it.' },
];

// Plays the conversation once when it scrolls into view; Jarvis "types" first.
const ChatDemo: React.FC = () => {
  const ref = useRef<HTMLDivElement>(null);
  const reduced = typeof window !== 'undefined' && window.matchMedia?.('(prefers-reduced-motion: reduce)').matches;
  const [shown, setShown] = useState(reduced ? CHAT.length : 0);
  const [typing, setTyping] = useState(false);

  useEffect(() => {
    if (reduced || !ref.current) return;
    let timers: number[] = [];
    const io = new IntersectionObserver(
      ([entry]) => {
        if (!entry.isIntersecting) return;
        io.disconnect();
        let t = 300;
        CHAT.forEach((m, i) => {
          if (m.who === 'jarvis') {
            timers.push(window.setTimeout(() => setTyping(true), t));
            t += 1100;
          }
          timers.push(
            window.setTimeout(() => {
              setTyping(false);
              setShown(i + 1);
            }, t),
          );
          t += m.who === 'jarvis' ? 1400 : 900;
        });
      },
      { threshold: 0.4 },
    );
    io.observe(ref.current);
    return () => {
      io.disconnect();
      timers.forEach(clearTimeout);
      timers = [];
    };
  }, [reduced]);

  return (
    <div className="jv-chat" ref={ref} aria-label="A conversation with Jarvis">
      {CHAT.slice(0, shown).map((m) =>
        m.who === 'you' ? (
          <div key={m.text} className="jv-msg jv-msg--you">
            {m.text}
          </div>
        ) : (
          <div key={m.text} className="jv-msg jv-msg--jarvis">
            <JarvisMark size={30} mood={m.wink ? 'wink' : 'idle'} animated={!m.wink} />
            <div>{m.text}</div>
          </div>
        ),
      )}
      {typing && (
        <div className="jv-typing" aria-label="Jarvis is typing">
          <span />
          <span />
          <span />
        </div>
      )}
      {/* Reserve the final height so the section doesn't jump while it plays. */}
      {shown < CHAT.length && <div className="jv-chat-spacer" style={{ minHeight: (CHAT.length - shown) * 64 }} />}
    </div>
  );
};
