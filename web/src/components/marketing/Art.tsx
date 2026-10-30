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
      <JarvisMark size={230} title="Jarvis" />
    </div>
    <div className="jv-float jv-float--a">
      <span className="jv-float-dot jv-dot-lime" />
      <div>
        <strong>Already handled</strong>
        <p>Outlook slowed two IPs. I moved that wave 40 min later.</p>
      </div>
    </div>
    <div className="jv-float jv-float--b">
      <span className="jv-float-dot jv-dot-pink" />
      <div>
        <strong>Before you ask</strong>
        <p>Your best subject line is #3. I promoted it.</p>
      </div>
    </div>
    <div className="jv-float jv-float--c">
      <span className="jv-float-dot jv-dot-cyan" />
      <div>
        <strong>Waiting on you</strong>
        <p>Tomorrow's send is staged. One tap to approve.</p>
      </div>
    </div>
  </div>
);
