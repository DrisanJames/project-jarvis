import React from 'react';
import { Spot } from '../Art';
import { PALETTE } from '../brand';
import { JarvisLockup, JarvisMark } from '../JarvisLogo';
import { Eyebrow, PageHero } from '../parts';

const VOICE = [
  { say: 'Already handled. Here’s what I changed and why.', not: 'Task completed successfully.' },
  { say: 'I held the volume — Outlook was deferring 11% at noon.', not: 'Volume optimization paused due to anomalous signals.' },
  { say: 'You could blast it. I’d wait until 2pm. Your call.', not: 'Are you sure? This action may be suboptimal.' },
  { say: 'This number is unverified. I’m checking it now.', not: 'Results may vary.' },
];

const TRAITS = [
  ['Loyal', 'On the user’s side, every time. Credit goes to them.'],
  ['Anticipatory', 'Says the next thing before it’s asked. Never says “as an AI”.'],
  ['Dry wit', 'One light line, max. Never at the user’s expense.'],
  ['Plain-spoken', 'Numbers and names, no jargon, no hype words.'],
  ['Honest', 'Bad news early. Unverified is labelled unverified.'],
];

const NEVER = [
  'Reference any film, comic, studio, superhero, armoured suit or character from existing franchises.',
  'Write the name with periods or as an acronym. It is “Jarvis”, one word, capital J.',
  'Use a red-and-gold palette, helmets, visors with slit eyes, glowing chest discs or arc shapes.',
  'Call the user “sir” or “boss”. Jarvis speaks to you, by name when it knows it.',
  'Make Jarvis smug. It out-thinks you quietly, then hands you the win.',
  'Promise inbox placement, revenue or open rates. Show the measured number or nothing.',
];

export const BrandPage: React.FC = () => (
  <>
    <PageHero
      eyebrow="Brand guidelines v1 · 2027"
      title={
        <>
          Jarvis is the brand. <span className="jv-hl">The agent is the value.</span>
        </>
      }
      body="Everything we make puts Jarvis — the loyal, always-on helper — at the centre. These are the rules for how it looks, sounds and shows up."
    />

    <section className="jv-section jv-section--tight" id="logo">
      <div className="jv-wrap">
        <div className="jv-section-head">
          <Eyebrow>01 · Logo</Eyebrow>
          <h2>The mark is Jarvis. The wordmark is lowercase.</h2>
          <p className="jv-lede">
            A chat-bubble head (Jarvis talks with you), an ink face plate, cyan eyes, and a lime spark — the idea Jarvis
            had before you asked. Keep clear space equal to the spark’s height on every side. Minimum size 20px.
          </p>
        </div>
        <div className="jv-brand-logos">
          <div className="jv-logo-tile jv-logo-tile--cream">
            <JarvisLockup size={56} />
          </div>
          <div className="jv-logo-tile jv-logo-tile--ink">
            <JarvisLockup size={56} inverse />
          </div>
          <div className="jv-logo-tile jv-logo-tile--violet">
            <JarvisLockup size={56} inverse />
          </div>
          <div className="jv-logo-tile jv-logo-tile--lime">
            <JarvisMark size={96} />
            <JarvisMark size={48} mood="wink" />
            <JarvisMark size={24} />
          </div>
        </div>
        <p className="jv-fine">
          The wink is reserved for moments Jarvis got something done for you: confirmations, success states, the end of a
          report.
        </p>
      </div>
    </section>

    <section className="jv-section jv-section--cream" id="color">
      <div className="jv-wrap">
        <div className="jv-section-head">
          <Eyebrow tone="pink">02 · Colour</Eyebrow>
          <h2>Loud on purpose. Cream underneath.</h2>
          <p className="jv-lede">
            Vibrant accents on a warm cream page. Signal Pink is the one call to action per view; Jarvis Violet is the
            agent. Text on each swatch uses the ink shown — it passes WCAG AA.
          </p>
        </div>
        <div className="jv-swatches">
          {PALETTE.map((s) => (
            <div key={s.hex} className="jv-swatch" style={{ background: s.hex, color: s.ink }}>
              <strong>{s.name}</strong>
              <code>{s.hex}</code>
              <span>{s.role}</span>
            </div>
          ))}
        </div>
        <div className="jv-gradient-strip">
          <span>Signature gradient · Violet → Pink → Tangerine · the mark and hero moments only</span>
        </div>
      </div>
    </section>

    <section className="jv-section jv-section--tight" id="type">
      <div className="jv-wrap jv-split">
        <div>
          <Eyebrow tone="cyan">03 · Type</Eyebrow>
          <h2>Bricolage Grotesque speaks. Inter explains.</h2>
          <p className="jv-lede">
            Headlines in Bricolage Grotesque ExtraBold, tight tracking, sentence case. Body and UI in Inter 400–600. One
            highlighted phrase per headline, underlined in lime.
          </p>
        </div>
        <div className="jv-type-specimen">
          <p className="jv-type-display">Already handled.</p>
          <p className="jv-type-body">
            Inter 18/1.6 for body copy. Jarvis writes short sentences with real numbers, then stops.
          </p>
          <p className="jv-type-meta">BRICOLAGE GROTESQUE 800 · INTER 400 / 600</p>
        </div>
      </div>
    </section>

    <section className="jv-section jv-section--ink" id="voice">
      <div className="jv-wrap">
        <div className="jv-section-head">
          <Eyebrow tone="lime">04 · Voice</Eyebrow>
          <h2>The loyal helper who’s quietly one step ahead.</h2>
        </div>
        <div className="jv-traits">
          {TRAITS.map(([t, d]) => (
            <div key={t}>
              <h3>{t}</h3>
              <p>{d}</p>
            </div>
          ))}
        </div>
        <div className="jv-voice-table">
          <div className="jv-voice-row jv-voice-row--head">
            <span>Jarvis says</span>
            <span>Jarvis never says</span>
          </div>
          {VOICE.map((v) => (
            <div key={v.say} className="jv-voice-row">
              <span>“{v.say}”</span>
              <span>“{v.not}”</span>
            </div>
          ))}
        </div>
      </div>
    </section>

    <section className="jv-section jv-section--tight" id="illustration">
      <div className="jv-wrap jv-split">
        <div>
          <Eyebrow tone="tangerine">05 · Illustration</Eyebrow>
          <h2>Flat vectors, ink outlines, Jarvis in the room.</h2>
          <ul className="jv-checks">
            <li>Flat fills from the palette, 2.5px Midnight Ink outlines, rounded joins.</li>
            <li>At most one gradient per illustration.</li>
            <li>Jarvis appears in, or is implied by, every hero illustration.</li>
            <li>Sparks (four-point stars) mark moments Jarvis acted.</li>
            <li>No stock photos of people at laptops. No 3D chrome.</li>
          </ul>
        </div>
        <div className="jv-spot-grid">
          {(['plan', 'send', 'protect', 'learn', 'audience', 'creative', 'lake', 'gate'] as const).map((k) => (
            <div key={k}>
              <Spot kind={k} size={64} />
            </div>
          ))}
        </div>
      </div>
    </section>

    <section className="jv-section jv-section--tight" id="motion">
      <div className="jv-wrap">
        <div className="jv-section-head">
          <Eyebrow tone="violet">07 · Motion</Eyebrow>
          <h2>Alive, never busy.</h2>
          <p className="jv-lede">
            Jarvis blinks, glances and twinkles — small signs it’s paying attention. One animated loop per page at most,
            every loop under 12 seconds, and all motion stops for people who ask their device for reduced motion.
          </p>
        </div>
        <div className="jv-motion-grid">
          <div>
            <JarvisMark size={96} animated />
            <h3>Blink + twinkle</h3>
            <p>The default living mark. Nav, modals, anywhere Jarvis is present.</p>
          </div>
          <div>
            <JarvisMark size={96} animated look />
            <h3>Glance</h3>
            <p>Eyes move toward the content Jarvis is talking about. Hero moments only.</p>
          </div>
          <div>
            <JarvisMark size={96} mood="wink" />
            <h3>Wink</h3>
            <p>Static, earned. Shown once when Jarvis finished something for you.</p>
          </div>
        </div>
      </div>
    </section>

    <section className="jv-section jv-section--cream" id="never">
      <div className="jv-wrap">
        <div className="jv-section-head">
          <Eyebrow tone="pink">06 · Never</Eyebrow>
          <h2>Our Jarvis is original. Keep it that way.</h2>
        </div>
        <ul className="jv-never">
          {NEVER.map((n) => (
            <li key={n}>{n}</li>
          ))}
        </ul>
      </div>
    </section>
  </>
);
