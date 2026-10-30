import React from 'react';
import { JarvisMark } from './JarvisLogo';

// Vector art. Style rules (see /brand): flat fills, 2.5px ink outlines,
// rounded joins, one gradient max per illustration, Jarvis present or implied.

const INK = '#14112B';

type SpotKind = 'plan' | 'send' | 'protect' | 'learn' | 'audience' | 'creative' | 'lake' | 'gate';

export const Spot: React.FC<{ kind: SpotKind; size?: number }> = ({ kind, size = 56 }) => {
  const common = { stroke: INK, strokeWidth: 2.5, strokeLinejoin: 'round' as const, strokeLinecap: 'round' as const };
  const body = (() => {
    switch (kind) {
      case 'plan':
        return (
          <>
            <rect x="8" y="12" width="40" height="34" rx="7" fill="#FFF8EE" {...common} />
            <path d="M8 22h40" {...common} />
            <rect x="14" y="27" width="10" height="6" rx="2" fill="#6D28FF" />
            <rect x="27" y="27" width="15" height="6" rx="2" fill="#FF2E88" />
            <rect x="14" y="36" width="18" height="5" rx="2" fill="#19D3F0" />
            <path d="M18 8v8M38 8v8" {...common} />
          </>
        );
      case 'send':
        return (
          <>
            <path d="M8 27 48 10 38 47 27 33Z" fill="#19D3F0" {...common} />
            <path d="M27 33 48 10" {...common} />
            <path d="M27 33v12l5-7" fill="#6D28FF" {...common} />
            <circle cx="12" cy="44" r="3" fill="#C6F432" {...common} />
          </>
        );
      case 'protect':
        return (
          <>
            <path d="M28 6 46 13v13c0 11-7.5 19-18 24C17.5 45 10 37 10 26V13Z" fill="#6D28FF" {...common} />
            <path d="m20 27 6 6 11-12" stroke="#C6F432" strokeWidth="4" strokeLinecap="round" strokeLinejoin="round" fill="none" />
          </>
        );
      case 'learn':
        return (
          <>
            <path d="M28 8c-9 0-15 6.5-15 14.5 0 5.5 3 9 6 11.5V40h18v-6c3-2.5 6-6 6-11.5C43 14.5 37 8 28 8z" fill="#C6F432" {...common} />
            <path d="M21 46h14M23 51h10" {...common} />
            <path d="M28 18v10M23 23h10" stroke="#6D28FF" strokeWidth="3" strokeLinecap="round" />
          </>
        );
      case 'audience':
        return (
          <>
            <circle cx="20" cy="22" r="7" fill="#FF8A00" {...common} />
            <circle cx="37" cy="20" r="6" fill="#19D3F0" {...common} />
            <path d="M8 46c1-8 6-12 12-12s11 4 12 12Z" fill="#FF8A00" {...common} />
            <path d="M29 44c1-7 4-11 9-11 5 0 9 4 10 11Z" fill="#19D3F0" {...common} />
          </>
        );
      case 'creative':
        return (
          <>
            <rect x="9" y="10" width="38" height="36" rx="6" fill="#FFF8EE" {...common} />
            <path d="M9 18 28 31 47 18" {...common} />
            <circle cx="42" cy="12" r="6" fill="#FF2E88" {...common} />
            <path d="M15 39h16" stroke="#6D28FF" strokeWidth="3" strokeLinecap="round" />
          </>
        );
      case 'lake':
        return (
          <>
            <rect x="9" y="30" width="8" height="16" rx="2" fill="#19D3F0" {...common} />
            <rect x="24" y="20" width="8" height="26" rx="2" fill="#6D28FF" {...common} />
            <rect x="39" y="12" width="8" height="34" rx="2" fill="#FF2E88" {...common} />
            <path d="M6 49h44" {...common} />
          </>
        );
      case 'gate':
        return (
          <>
            <rect x="10" y="24" width="36" height="24" rx="6" fill="#FF8A00" {...common} />
            <path d="M18 24v-6a10 10 0 0 1 20 0v6" fill="none" {...common} />
            <circle cx="28" cy="35" r="3.5" fill={INK} />
            <path d="M28 38v4" {...common} />
          </>
        );
    }
  })();
  return (
    <svg width={size} height={size} viewBox="0 0 56 56" aria-hidden="true" xmlns="http://www.w3.org/2000/svg">
      {body}
    </svg>
  );
};

// Hero scene: Jarvis at centre, three "already handled" cards orbiting.
export const HeroScene: React.FC = () => (
  <div className="jv-hero-scene" aria-hidden="true">
    <svg className="jv-hero-blobs" viewBox="0 0 520 480" xmlns="http://www.w3.org/2000/svg">
      <path d="M260 30c95 0 205 52 225 150s-48 230-170 262S52 418 38 300 165 30 260 30z" fill="#6D28FF" />
      <path d="M420 60c40 10 70 50 60 90s-60 40-90 20-40-60-20-90 30-24 50-20z" fill="#C6F432" />
      <circle cx="80" cy="390" r="46" fill="#19D3F0" />
      <path d="M88 96l7 14 14 7-14 7-7 14-7-14-14-7 14-7z" fill="#FF8A00" />
      <path d="M452 360l5 10 10 5-10 5-5 10-5-10-10-5 10-5z" fill="#FFF8EE" />
    </svg>
    <div className="jv-hero-bot">
      <JarvisMark size={230} title="Jarvis" animated look />
    </div>
    <div className="jv-float jv-float--a">
      <span className="jv-float-dot jv-dot-lime" />
      <div>
        <strong>Already handled</strong>
        <p>Your sale email had a broken link. Fixed before it went out.</p>
      </div>
    </div>
    <div className="jv-float jv-float--b">
      <span className="jv-float-dot jv-dot-pink" />
      <div>
        <strong>Before you ask</strong>
        <p>Subject line B is winning. Sending the rest with B.</p>
      </div>
    </div>
    <div className="jv-float jv-float--c">
      <span className="jv-float-dot jv-dot-cyan" />
      <div>
        <strong>Waiting on you</strong>
        <p>Thursday's newsletter is drafted. One tap to approve.</p>
      </div>
    </div>
  </div>
);

// ── Animated vignettes ("loops"). Pure SVG + CSS, a few KB each, frozen on
// their first frame under prefers-reduced-motion. Use at most one per page.

const Envelope: React.FC<{ fill: string; mark?: string }> = ({ fill, mark }) => (
  <>
    <rect x="-10" y="136" width="46" height="32" rx="6" fill={fill} stroke={INK} strokeWidth="2.5" />
    <path d="M-10 140l23 15 23-15" fill="none" stroke={INK} strokeWidth="2.5" strokeLinejoin="round" />
    {mark && (
      <text x="13" y="164" textAnchor="middle" className="jv-loop-glyph">
        {mark}
      </text>
    )}
  </>
);

// Mail rides the belt into the inbox; the scanner envelope gets plucked.
export const InboxSorterLoop: React.FC = () => (
  <svg className="jv-loop jv-loop--sorter" viewBox="0 0 480 220" role="img" aria-label="Jarvis sends the good emails on their way and catches the one with a mistake">
    <rect x="1" y="1" width="478" height="218" rx="24" fill="#FFFFFF" stroke={INK} strokeWidth="2.5" />
    <rect x="20" y="170" width="380" height="14" rx="7" fill={INK} />
    {[40, 110, 180, 250, 320, 380].map((x) => (
      <circle key={x} cx={x} cy="177" r="3" fill="#FFF8EE" />
    ))}
    <g transform="translate(212 18)">
      <JarvisMark size={64} animated look />
    </g>
    <g className="jv-loop-burst">
      <path d="M244 70l4 8 8 4-8 4-4 8-4-8-8-4 8-4z" fill="#C6F432" stroke={INK} strokeWidth="1.5" />
    </g>
    <text x="300" y="56" className="jv-loop-label">only the good stuff.</text>
    <g className="jv-env">
      <Envelope fill="#19D3F0" />
    </g>
    <g className="jv-env" style={{ animationDelay: '-1.5s' }}>
      <Envelope fill="#FFE1BD" />
    </g>
    <g className="jv-env jv-env--bot" style={{ animationDelay: '-3s' }}>
      <Envelope fill="#FF2E88" mark="!" />
    </g>
    <g className="jv-env" style={{ animationDelay: '-4.5s' }}>
      <Envelope fill="#E4D8FF" />
    </g>
    <path d="M404 140h60v52a8 8 0 0 1-8 8h-44a8 8 0 0 1-8-8z" fill="#C6F432" stroke={INK} strokeWidth="2.5" strokeLinejoin="round" />
    <text x="434" y="182" textAnchor="middle" className="jv-loop-label">inbox</text>
  </svg>
);

// Night falls, Jarvis keeps working, the sun comes up on a finished report.
export const NightShiftLoop: React.FC = () => (
  <svg className="jv-loop jv-loop--night" viewBox="0 0 360 240" role="img" aria-label="Jarvis works through the night and has your report ready by morning">
    <defs>
      <linearGradient id="jv-dawn" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0" stopColor="#6D28FF" />
        <stop offset="1" stopColor="#FF8A00" />
      </linearGradient>
      <clipPath id="jv-night-clip">
        <rect x="0" y="0" width="360" height="240" rx="24" />
      </clipPath>
    </defs>
    <g clipPath="url(#jv-night-clip)">
      <rect width="360" height="240" fill="#14112B" />
      <rect className="jv-night-dawn" width="360" height="240" fill="url(#jv-dawn)" />
      {[
        [40, 40, 0],
        [92, 76, -1.2],
        [150, 30, -2.4],
        [262, 64, -0.6],
        [318, 34, -1.8],
      ].map(([x, y, d]) => (
        <path
          key={`${x}-${y}`}
          className="jv-night-star"
          style={{ animationDelay: `${d}s` }}
          d={`M${x} ${y - 6}l2 4 4 2-4 2-2 4-2-4-4-2 4-2z`}
          fill="#FFF8EE"
        />
      ))}
      <circle className="jv-night-moon" cx="290" cy="58" r="20" fill="#FFF8EE" />
      <circle className="jv-night-sun" cx="306" cy="250" r="26" fill="#C6F432" stroke={INK} strokeWidth="2.5" />
      <rect x="0" y="196" width="360" height="44" fill="#2A2550" />
      <g transform="translate(138 104)">
        <JarvisMark size={92} animated />
      </g>
      <g transform="translate(248 166)">
        <path d="M0 0h22v26a6 6 0 0 1-6 6H6a6 6 0 0 1-6-6z" fill="#FF2E88" stroke={INK} strokeWidth="2.5" />
        <path d="M22 8h4a5 5 0 0 1 0 10h-4" fill="none" stroke="#FF2E88" strokeWidth="3.5" />
        <path className="jv-night-steam" d="M7-6q-4-6 0-12M15-6q-4-6 0-12" fill="none" stroke="#FFF8EE" strokeWidth="2" strokeLinecap="round" />
      </g>
      <g className="jv-night-report">
        <rect x="40" y="132" width="62" height="56" rx="8" fill="#FFFFFF" stroke={INK} strokeWidth="2.5" />
        <path d="M50 146h30M50 156h40M50 166h22" stroke="#6D28FF" strokeWidth="3" strokeLinecap="round" />
        <circle cx="94" cy="134" r="10" fill="#C6F432" stroke={INK} strokeWidth="2.5" />
        <path d="m89 134 4 4 6-7" fill="none" stroke={INK} strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" />
      </g>
    </g>
    <rect x="1.25" y="1.25" width="357.5" height="237.5" rx="23" fill="none" stroke={INK} strokeWidth="2.5" />
  </svg>
);
