// bannedCreative — guardrail to ensure the Send-Day Planner refuses to
// emit a creative on the operator's pause-list. Drift between the
// fallback set and the API/DB store is acceptable as long as either
// prevents a banned filename from going live.

import { describe, expect, it } from 'vitest';
import { FALLBACK_BANNED_CREATIVES, basenameLower, isBanned } from '../bannedCreative';
import type { BannedCreative } from '../types';

describe('bannedCreative.basenameLower', () => {
  it('strips path and lowercases', () => {
    expect(basenameLower('docs/emails/Trugreen-6Weeks-FREE.html')).toBe('trugreen-6weeks-free.html');
  });
  it('keeps name when no path', () => {
    expect(basenameLower('foo.html')).toBe('foo.html');
  });
});

describe('bannedCreative.isBanned — fallback set', () => {
  it('detects exact basename match', () => {
    expect(isBanned('trugreen-6weeks-free.html')).toBe(true);
  });
  it('detects path-prefixed match', () => {
    expect(isBanned('docs/emails/trugreen-6weeks-free.html')).toBe(true);
  });
  it('matches case-insensitively', () => {
    expect(isBanned('TRUGREEN-6WEEKS-FREE.html')).toBe(true);
  });
  it('returns false for unrelated filename', () => {
    expect(isBanned('warby-parker-db-newsletter-05122026.html')).toBe(false);
  });
});

describe('bannedCreative.isBanned — API list (BannedCreative[])', () => {
  const apiList: BannedCreative[] = [
    { filename: 'foo.html', reason: 'because', paused_at: '2026-05-01T00:00:00Z' },
    { filename: 'docs/emails/quux-bar-newsletter-05012026.html', reason: 'spam-trap hit', paused_at: '2026-05-02' },
  ];
  it('detects basename match against API list', () => {
    expect(isBanned('foo.html', apiList)).toBe(true);
    expect(isBanned('quux-bar-newsletter-05012026.html', apiList)).toBe(true);
  });
  it('returns false when not in API list (does not fall back)', () => {
    expect(isBanned('trugreen-6weeks-free.html', apiList)).toBe(false);
  });
});

describe('bannedCreative.isBanned — explicit Set override', () => {
  const set = new Set<string>(['blocked.html']);
  it('returns true for set member', () => {
    expect(isBanned('blocked.html', set)).toBe(true);
  });
  it('returns false for non-member', () => {
    expect(isBanned('something-else.html', set)).toBe(false);
  });
});

describe('FALLBACK_BANNED_CREATIVES', () => {
  it('contains the trugreen-6weeks-free.html entry', () => {
    expect(FALLBACK_BANNED_CREATIVES.has('trugreen-6weeks-free.html')).toBe(true);
  });
});
