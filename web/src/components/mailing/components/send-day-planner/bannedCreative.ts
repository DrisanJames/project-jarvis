// bannedCreative — single source of truth for "is this filename
// currently retired?". The canvas reads from
// GET /api/mailing/send-day/banned-creatives and falls back to the
// hardcoded set if the endpoint is unreachable.

import type { BannedCreative } from './types';

// LAST-RESORT fallback (mirrors the Python BANNED_CREATIVES seed). The
// canonical store is the mailing_banned_creatives table, populated by
// runStartupMigrations(). The canvas should always prefer the API
// response — this set exists so the canvas degrades gracefully when
// the new endpoint is unreachable.
export const FALLBACK_BANNED_CREATIVES = new Set<string>([
  'trugreen-6weeks-free.html',
]);

// Banned by PREFIX — advertiser families retired from newsletters regardless of
// brand/date suffix. Mirrors eng_w2_rotation.py assert_creative_not_banned()
// (base.startswith("warby-parker-")). Warby Parker banned from NL 2026-06-14.
export const BANNED_PREFIXES: readonly string[] = ['warby-parker-'];

export function basenameLower(filename: string): string {
  const base = filename.split('/').pop() ?? filename;
  return base.toLowerCase();
}

/** isBanned — exact basename match against either the API list or the
 *  fallback set. */
export function isBanned(
  filename: string, banned?: ReadonlySet<string> | BannedCreative[],
): boolean {
  const base = basenameLower(filename);
  // Prefix bans apply regardless of the API/fallback list (advertiser-family retirement).
  if (BANNED_PREFIXES.some(p => base.startsWith(p))) return true;
  if (Array.isArray(banned)) {
    return banned.some(b => basenameLower(b.filename) === base);
  }
  if (banned instanceof Set) {
    return banned.has(base);
  }
  return FALLBACK_BANNED_CREATIVES.has(base);
}
