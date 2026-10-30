import React, { useId } from 'react';

// The Jarvis mark: a chat-bubble head (Jarvis talks to you), an ink face plate,
// cyan eyes, and a lime spark antenna (the idea Jarvis had before you asked).
// Deliberately round and friendly — no helmets, slits, armour or red/gold.

interface MarkProps {
  size?: number;
  mood?: 'idle' | 'wink';
  className?: string;
  title?: string;
}

export const JarvisMark: React.FC<MarkProps> = ({ size = 40, mood = 'idle', className, title = 'Jarvis' }) => {
  const gid = useId().replace(/:/g, '');
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 64 64"
      role="img"
      aria-label={title}
      className={className}
      xmlns="http://www.w3.org/2000/svg"
    >
      <defs>
        <linearGradient id={`jvg-${gid}`} x1="6" y1="12" x2="58" y2="62" gradientUnits="userSpaceOnUse">
          <stop offset="0" stopColor="#6D28FF" />
          <stop offset="0.55" stopColor="#FF2E88" />
          <stop offset="1" stopColor="#FF8A00" />
        </linearGradient>
      </defs>
      <path d="M32 3.5l2.2 4.3 4.3 2.2-4.3 2.2L32 16.5l-2.2-4.3-4.3-2.2 4.3-2.2z" fill="#C6F432" />
      <rect x="30.6" y="14" width="2.8" height="5" rx="1.4" fill="#14112B" />
      <path
        d="M24 18h16c10.5 0 18 7.5 18 17.5S50.5 53 40 53H22l-11 8.5 2.6-11.2C8.6 47 6 41.6 6 35.5 6 25.5 13.5 18 24 18z"
        fill={`url(#jvg-${gid})`}
      />
      <rect x="14" y="26" width="36" height="19" rx="9.5" fill="#14112B" />
      <circle cx="24.5" cy="35.5" r="4" fill="#19D3F0" />
      {mood === 'wink' ? (
        <path d="M35.5 35.8q4-3.6 8 0" stroke="#19D3F0" strokeWidth="2.6" strokeLinecap="round" fill="none" />
      ) : (
        <circle cx="39.5" cy="35.5" r="4" fill="#19D3F0" />
      )}
      <circle cx="26" cy="34" r="1.2" fill="#FFFFFF" />
      {mood !== 'wink' && <circle cx="41" cy="34" r="1.2" fill="#FFFFFF" />}
    </svg>
  );
};

interface LockupProps {
  size?: number;
  inverse?: boolean;
}

export const JarvisLockup: React.FC<LockupProps> = ({ size = 36, inverse = false }) => (
  <span className={`jv-lockup${inverse ? ' jv-lockup--inverse' : ''}`}>
    <JarvisMark size={size} />
    <span className="jv-wordmark" style={{ fontSize: size * 0.78 }}>
      jarvis
    </span>
  </span>
);
