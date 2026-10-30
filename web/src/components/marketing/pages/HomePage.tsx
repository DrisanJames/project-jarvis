import React, { useEffect, useRef, useState } from 'react';
import { HeroScene, InboxSorterLoop, Spot } from '../Art';
import { JarvisMark } from '../JarvisLogo';
import { CtaBand, EarlyAccessButton, Eyebrow, PageLink, useMarketingNav } from '../parts';

const PILLARS = [
  {
    kind: 'creative' as const,
    tone: 'violet',
    title: 'Create',
    body: 'Beautiful newsletters and promotions, drafted in your voice in minutes. You stay the editor-in-chief.',
  },
  {
    kind: 'send' as const,
    tone: 'cyan',
    title: 'Send',
    body: 'Every email arrives when each subscriber is most likely to read it. No guesswork, no spreadsheets.',
  },
  {
    kind: 'audience' as const,
    tone: 'lime',
    title: 'Grow',
    body: 'Sign-up forms, warm welcomes and smart audiences that turn new subscribers into regulars.',
  },
  {
    kind: 'lake' as const,
    tone: 'pink',
    title: 'Understand',
    body: 'What worked, what didn’t and what to try next — explained in plain English, not dashboards.',
  },
];

const USE_CASES = [
  { title: 'Newsletters', body: 'Stay top of mind with a newsletter people look forward to.' },
  { title: 'Sales & promotions', body: 'Launch the sale, remind the browsers, thank the buyers.' },
  { title: 'Product launches', body: 'Build the buzz before launch day and the follow-up after.' },
  { title: 'Welcome emails', body: 'Make a great first impression on every new subscriber.' },
  { title: 'Events', body: 'Invites, reminders and thank-yous that fill the room.' },
  { title: 'Win-backs', body: 'Bring quiet customers back with the right note at the right time.' },
];

const REASONS = [
  { tone: 'violet', title: 'Hours back every week', body: 'Jarvis does the drafting, scheduling and checking. You do the deciding.' },
  { tone: 'pink', title: 'Built to grow opens and sales', body: 'Every send teaches Jarvis more about your audience, so the next one lands better.' },
  { tone: 'cyan', title: 'Peace of mind', body: 'Mistakes get caught before your customers see them. You approve what goes out.' },
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
              Jarvis is the AI email-marketing teammate for small businesses. It writes, schedules and sends your
              newsletters and promotions — and usually spots the problem before you do.
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
              Jarvis follows your lead — and when there’s a better way, it says so, does it, and tells you why.
            </p>
            <PageLink to="agent" className="jv-btn jv-btn--light">
              Meet Jarvis
            </PageLink>
          </div>
          <ChatDemo />
        </div>
      </section>

      <section className="jv-section">
        <div className="jv-wrap">
          <div className="jv-section-head">
            <Eyebrow tone="cyan">Made for every kind of send</Eyebrow>
            <h2>Whatever you’re announcing, Jarvis has it covered.</h2>
          </div>
          <div className="jv-usecases">
            {USE_CASES.map((u) => (
              <article key={u.title}>
                <h3>{u.title}</h3>
                <p>{u.body}</p>
              </article>
            ))}
          </div>
        </div>
      </section>

      <section className="jv-section jv-section--cream">
        <div className="jv-wrap">
          <div className="jv-section-head">
            <Eyebrow tone="tangerine">Why small teams choose Jarvis</Eyebrow>
            <h2>Less busywork. Better email.</h2>
          </div>
          <div className="jv-loop-card">
            <InboxSorterLoop />
          </div>
          <div className="jv-card-grid jv-card-grid--3">
            {REASONS.map((r) => (
              <article key={r.title} className={`jv-card jv-card--${r.tone}`}>
                <h3>{r.title}</h3>
                <p>{r.body}</p>
              </article>
            ))}
          </div>
        </div>
      </section>

      <section className="jv-section">
        <div className="jv-wrap jv-split">
          <div className="jv-proof-card">
            <JarvisMark size={64} mood="wink" />
            <blockquote>“You run the business. I’ll handle the inbox. And I’ll always ask before anything big.”</blockquote>
            <span>— Jarvis</span>
          </div>
          <div>
            <Eyebrow tone="pink">You’re always in charge</Eyebrow>
            <h2>Smart enough to help. Polite enough to ask.</h2>
            <ul className="jv-checks">
              <li>Nothing goes out without your approval.</li>
              <li>Every change comes with an undo.</li>
              <li>Jarvis explains its suggestions in plain English.</li>
              <li>It learns your brand voice and your preferences.</li>
            </ul>
          </div>
        </div>
      </section>

      <CtaBand title="Let Jarvis take it from here." body="Join the 2027 early-access list. Bring your list; Jarvis brings the rest." />
    </>
  );
};

const CHAT: { who: 'you' | 'jarvis'; text: string; wink?: boolean }[] = [
  { who: 'you', text: 'Send the spring sale email to everyone tonight.' },
  {
    who: 'jarvis',
    text: 'Scheduled — with a tweak. Your regular customers get it at 7am, when they usually read. Anyone who’s gone quiet gets a friendly check-in instead.',
  },
  {
    who: 'jarvis',
    wink: true,
    text: 'Fewer unsubscribes, more sales. If you’d rather send it to everyone at once, say “undo” and I will. I won’t even sigh.',
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
