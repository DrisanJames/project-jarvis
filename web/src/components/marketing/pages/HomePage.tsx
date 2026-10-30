import React, { useEffect, useRef, useState } from 'react';
import { HeroScene, InboxSorterLoop, Spot } from '../Art';
import { JarvisMark } from '../JarvisLogo';
import { CtaBand, EarlyAccessButton, Eyebrow, PageLink, useMarketingNav } from '../parts';

const PILLARS = [
  {
    kind: 'plan' as const,
    tone: 'violet',
    title: 'Plan',
    body: 'Tell Jarvis the goal. It builds the whole send day — who, when, which inbox, which offer — and stages it for your nod.',
  },
  {
    kind: 'send' as const,
    tone: 'cyan',
    title: 'Send',
    body: 'Amazon SES and your own MTAs behind one switch. Jarvis routes every message by mailbox provider and paces it in waves.',
  },
  {
    kind: 'protect' as const,
    tone: 'pink',
    title: 'Protect',
    body: 'Bounces, blocks, bot clicks, tired lists. Jarvis watches every lane and pulls the brake before your reputation notices.',
  },
  {
    kind: 'learn' as const,
    tone: 'lime',
    title: 'Learn',
    body: 'Every correction you make becomes a rule Jarvis keeps. It checks its own facts and never forgets the lesson.',
  },
];

const STEPS = [
  { n: '01', title: 'Notices', body: 'Delivery, opens, clicks, conversions and mailbox replies, read every 15 minutes.' },
  { n: '02', title: 'Proposes', body: 'A plain-language plan with the numbers behind it. No jargon, no mystery.' },
  { n: '03', title: 'Acts in your lanes', body: 'Inside the limits you set. Anything bigger waits for your yes.' },
  { n: '04', title: 'Remembers', body: 'Every change logged with a one-step undo. Every lesson kept.' },
];

export const HomePage: React.FC = () => {
  const { go } = useMarketingNav();
  return (
    <>
      <section className="jv-hero">
        <div className="jv-wrap jv-hero-grid">
          <div className="jv-hero-copy">
            <Eyebrow tone="pink">The AI agent for email marketing</Eyebrow>
            <h1>
              Your email program, <span className="jv-hl">already handled.</span>
            </h1>
            <p className="jv-lede">
              Jarvis plans, sends and protects your email — and usually spots the problem before you do. Loyal to your
              goals, honest about your numbers, and quietly one step ahead.
            </p>
            <div className="jv-cta-row">
              <EarlyAccessButton />
              <button type="button" className="jv-btn jv-btn--ghost" onClick={() => go('home', 'watch')}>
                Watch Jarvis work
              </button>
            </div>
            <p className="jv-fine">Opening to design partners in 2027. No card, no sales script.</p>
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
            <Eyebrow>One agent, four jobs</Eyebrow>
            <h2>Everything your email team juggles. Jarvis carries it.</h2>
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
              Jarvis follows your lead — and when your plan would cost you inboxes, it says so, fixes it, and tells you
              exactly what changed. Every time.
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
              “Jarvis wasn’t designed in a pitch deck. It runs our own publishing portfolio — 27 sending brands, three
              sending transports, every mailbox provider — every single day.”
            </blockquote>
            <span>— The James Ventures team</span>
          </div>
          <div>
            <Eyebrow tone="tangerine">Built in production first</Eyebrow>
            <h2>Battle-tested on our own inboxes before it touches yours.</h2>
            <ul className="jv-checks">
              <li>Engaged readers mail first and warm the way for everyone else.</li>
              <li>Each mailbox provider gets its own rules, pace and playbook.</li>
              <li>Every live change is logged with a before, an after and an undo.</li>
              <li>Big moves always wait for a human yes.</li>
            </ul>
          </div>
        </div>
      </section>

      <CtaBand
        title="Let Jarvis take it from here."
        body="Join the 2027 design-partner cohort. Bring your goals; Jarvis brings the rest."
      />
    </>
  );
};

const CHAT: { who: 'you' | 'jarvis'; text: string; wink?: boolean }[] = [
  { who: 'you', text: 'Send the Friday promo to everyone at 9am.' },
  {
    who: 'jarvis',
    text: "Done — mostly. 18% of that list hasn't opened in 90 days, and Yahoo is cooling on two of your IPs. Engaged readers go at 9:00; everyone else rides gentle waves until 2pm.",
  },
  {
    who: 'jarvis',
    wink: true,
    text: 'Expect more inboxes and fewer complaints. If you\'d rather blast it anyway, say "undo" and I will. I won\'t even sigh.',
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
