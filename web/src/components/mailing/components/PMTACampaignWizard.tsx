import React, { useState, useEffect, useCallback, useMemo, useRef } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faArrowLeft, faArrowRight, faArrowUp, faArrowDown, faCheck, faServer, faGlobe,
  faPenFancy, faUsers, faBrain, faRocket, faSpinner,
  faExclamationTriangle, faCheckCircle, faTimesCircle,
  faTimes, faChartBar, faShieldAlt, faCrosshairs,
  faSave, faGripVertical, faMagic,
  faCopy, faTrophy, faChevronDown, faChevronUp, faSearch, faLock, faInfinity,
  faEye, faNewspaper, faSnowflake, faRotate, faClock, faTemperatureHalf,
} from '@fortawesome/free-solid-svg-icons';
import { useAuth } from '../../../contexts/AuthContext';
import { apiFetch } from '../shared/apiFetch';
import { SectionError, EmptyState } from '../shared/ui';
import { AnimatedCounter } from '../shared/AnimatedCounter';
import { useToast } from '../shared/ToastSystem';
import { JarvisCompleteModal } from '../shared/JarvisCompleteModal';
import {
  SEGMENT_CATEGORIES,
  getCategoryMeta,
  defaultVisibleCategoriesForPicker,
  type SegmentCategory,
} from './segCategoryMetadata';
import { EngagementTierPicker, type EngagementTier, type EngagementTiers } from './EngagementTierPicker';
import { OfferCreativePicker } from './OfferCreativePicker';

const API_BASE = '/api/mailing';

async function orgFetch(url: string, orgId?: string, opts?: RequestInit) {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    ...(orgId ? { 'X-Organization-ID': orgId } : {}),
    ...(opts?.headers as Record<string, string> || {}),
  };
  return fetch(url, { ...opts, headers, credentials: 'include' });
}

// ── ISP visual config ────────────────────────────────────────────────────────

const ISP_META: Record<string, { label: string; color: string; emoji: string }> = {
  gmail:     { label: 'Gmail',               color: '#ea4335', emoji: '📧' },
  yahoo:     { label: 'Yahoo',               color: '#7b1fa2', emoji: '🟣' },
  aol:       { label: 'AOL',                 color: '#ff6600', emoji: '📬' },
  microsoft: { label: 'Microsoft',           color: '#0078d4', emoji: '🔷' },
  apple:     { label: 'Apple iCloud',        color: '#a2aaad', emoji: '🍎' },
  comcast:   { label: 'Comcast',             color: '#e60000', emoji: '📡' },
  att:       { label: 'AT&T',                color: '#009fdb', emoji: '📶' },
  sbcglobal: { label: 'SBC Global/BellSouth', color: '#00a8e0', emoji: '📞' },
  cox:       { label: 'Cox',                 color: '#f26522', emoji: '🔌' },
  charter:   { label: 'Charter/Spectrum',    color: '#0099d6', emoji: '📺' },
};

const ALL_ISPS = ['gmail', 'yahoo', 'aol', 'microsoft', 'apple', 'comcast', 'att', 'sbcglobal', 'cox', 'charter'];

// ── Send-day doctrine ────────────────────────────────────────────────────────
//
// The board mails its engaged tiers FIRST so they warm the inbox/IP before
// anything else arrives (operator doctrine, restated 2026-08-10 — "this is
// meaningful and is a must"). These presets put the same anchors, windows and
// pacing in the wizard. Source of truth for the numbers:
//   anchors  agents/scheduling/board_generator.py ANCHOR_OFFSET_HOURS + start_local
//   windows  agents/scheduling/data/<date>_structure.json throttle_hours
//   pacing   agents/scheduling/legacy_lib.build_isp_plans (15 min, gentle, Denver)
const SEND_DAY_TIMEZONE = 'America/Denver';

// ---------------------------------------------------------------------------
// AUTO-NAMING (operator 2026-08-20). Every campaign-name defect on the 08/20 and
// 08/21 boards came from hand-typing the field: 'MF' for MH, '08062026' and
// '080202026' for 08202026, and a 'DB' name on a consumerpro payload. The name is
// how the board is verified — a mistyped one is invisible to by-name checks and
// forks the property in reporting — so it is derived, not typed.
//
// Shape: MMDDYYYY - BRAND - OFFER, dated for the NEXT send day.
const BRAND_PREFIX: Record<string, string> = {
  historythinking: 'HT', ratesbazar: 'RB', quizfiesta: 'QF',
  learnpersonalloans: 'LPL', myownhealth: 'MH', discountblog: 'DB',
  casainsure: 'CI', consumerpro: 'CP', warrantyforyou: 'WF',
  yourinsurancehub: 'YIH', thingoftheday: 'TOT', financialcalculate: 'FC',
  homewarrantyservices: 'HWS', myrepairdiy: 'MR', businessweeklypro: 'BWP',
  refinanceratesusa: 'RR',
};

// Offer rows carry long catalogue names ("Sam's Club Membership - Partner Drip
// (4989)"); boards use a short token. Longest match wins so 'west shore' is not
// shadowed by a shorter key.
const OFFER_TOKEN: Record<string, string> = {
  "sam's club": 'Sams', 'samsclub': 'Sams',
  'globe life': 'Globe',
  'choice home warranty': 'CHW',
  'accredited debt': 'ADR',
  'freedom debt': 'Freedom',
  'liberty mutual': 'Liberty',
  'adt': 'ADT',
  'metal roofing': 'MR',
  'west shore': 'Westshore', 'westshore': 'Westshore',
  'carshield': 'CarShield',
  'serviceplus': 'ServicePlus',
  'budget blinds': 'Blinds',
};

export const brandPrefixForDomain = (domain: string): string => {
  const apex = (domain || '').replace(/^(em|m)\./i, '').split('.')[0] || '';
  return BRAND_PREFIX[apex.toLowerCase()] || '';
};

export const offerTokenForName = (offerName: string): string => {
  const n = (offerName || '').toLowerCase();
  const hit = Object.keys(OFFER_TOKEN)
    .filter(k => n.includes(k))
    .sort((a, b) => b.length - a.length)[0];
  if (hit) return OFFER_TOKEN[hit];
  // Fallback: first meaningful word, so an unmapped offer still yields a usable
  // token rather than an empty segment.
  const w = (offerName || '').split(/[\s\-—(]+/).filter(Boolean)[0] || '';
  return w ? w.charAt(0).toUpperCase() + w.slice(1) : '';
};

// MMDDYYYY of an INSTANT, read in the send-day zone. The campaign name has to
// say the day the mail actually lands, and the operator's browser is not
// necessarily in Denver — so the calendar date is always read back through the
// send-day zone, never off the local Date getters.
export const sendDayTokenForInstant = (when: Date): string => {
  const parts = new Intl.DateTimeFormat('en-US', {
    timeZone: SEND_DAY_TIMEZONE, year: 'numeric', month: '2-digit', day: '2-digit',
  }).formatToParts(when);
  const g = (t: string) => parts.find(x => x.type === t)?.value || '';
  return `${g('month')}${g('day')}${g('year')}`;
};

// MMDDYYYY for the send day `dayOffset` days from today, in the send-day zone.
export const sendDayDateToken = (dayOffset: number): string => {
  const parts = new Intl.DateTimeFormat('en-US', {
    timeZone: SEND_DAY_TIMEZONE, year: 'numeric', month: '2-digit', day: '2-digit',
  }).formatToParts(new Date());
  const g = (t: string) => parts.find(x => x.type === t)?.value || '';
  const base = new Date(Number(g('year')), Number(g('month')) - 1, Number(g('day')));
  base.setDate(base.getDate() + dayOffset);
  const mm = String(base.getMonth() + 1).padStart(2, '0');
  const dd = String(base.getDate()).padStart(2, '0');
  return `${mm}${dd}${base.getFullYear()}`;
};

/**
 * The date token a campaign should carry, given the schedule it will actually
 * run on. `scheduledAtLocal` is the raw <input type="datetime-local"> value.
 *
 * The date is NEVER a fixed "tomorrow": a preset can move the send to today
 * (applyAnchorPreset shifts to today whenever the anchor is still >15 min out)
 * and the operator can hand-edit the field to any day. A name that disagrees
 * with the payload is the 08/20-08/21 defect all over again — invisible to
 * verify-by-name, and it forks the property in trend reporting.
 */
export const nameDateTokenForSchedule = (
  scheduledAtLocal: string,
  sendMode: 'immediate' | 'scheduled',
): string => {
  if (sendMode === 'immediate') return sendDayDateToken(0);
  if (scheduledAtLocal) {
    const when = new Date(scheduledAtLocal);
    if (!Number.isNaN(when.getTime())) return sendDayTokenForInstant(when);
  }
  // Nothing chosen yet — the wizard's own default anchor is tomorrow 01:01 MT.
  return sendDayDateToken(1);
};

// Default anchor for a newly built campaign: 01:01 in the send-day zone, tomorrow.
const DEFAULT_ANCHOR_HOUR = 1;
const DEFAULT_ANCHOR_MINUTE = 1;

// KumoMTA warm-up estate: YAHOO-FAMILY ONLY. Operator 2026-08-11 — "model
// exactly the aawd and hcfl and prevent any other sending aside from Yahoo
// family" — and agents/scheduling/data/kumo_estate.json carries isp_caps for
// exactly these five lanes and no others. Selecting gmail/microsoft/apple/
// comcast on a kumo route is a doctrine violation, not a tuning choice.
const KUMO_ALLOWED_ISPS = ['yahoo', 'aol', 'att', 'sbcglobal', 'cox'];
const DEFAULT_WAVE_INTERVAL_MINUTES = 15;
const DEFAULT_THROTTLE_STRATEGY = 'gentle';
const THROTTLE_STRATEGIES = ['gentle', 'auto', 'moderate', 'careful'];

interface AnchorPreset {
  id: string;
  label: string;
  localTime: string;      // HH:MM in SEND_DAY_TIMEZONE
  windowHours: number;
  hint: string;
}

const ANCHOR_PRESETS: AnchorPreset[] = [
  { id: 'clk',    label: 'Clickers (anchor)', localTime: '01:01', windowHours: 8,  hint: 'the day\'s first send — clickers lead' },
  { id: 'eng',    label: 'Engagers',          localTime: '02:01', windowHours: 10, hint: '+1h behind the clicker anchor' },
  { id: 'other',  label: 'Everything else',   localTime: '04:01', windowHours: 12, hint: '+3h — after both engaged tiers' },
  { id: 'pmeng',  label: 'PM Engagers',       localTime: '12:01', windowHours: 6,  hint: 'the afternoon engager pass' },
];

const DEFAULT_ISP_QUOTAS: Record<string, number> = {
  gmail:     50000,
  yahoo:     20000,
  aol:       10000,
  microsoft: 20000,
  apple:     10000,
  comcast:    5000,
  att:        5000,
  sbcglobal:  3000,
  cox:        3000,
  charter:    3000,
};

// ── Types ────────────────────────────────────────────────────────────────────

interface ISPReadiness {
  isp: string;
  display_name: string;
  health_score: number;
  status: string;
  active_agents: number;
  total_agents: number;
  bounce_rate: number;
  hard_bounce_rate: number;
  soft_bounce_rate: number;
  deferral_rate: number;
  complaint_rate: number;
  warmup_ips: number;
  active_ips: number;
  quarantined_ips: number;
  max_daily_capacity: number;
  max_hourly_rate: number;
  pool_name: string;
  has_emergency: boolean;
  warnings: string[];
}

interface SendingProfileOption {
  id: string;
  name: string;
  from_name?: string;
  from_email?: string;
  transport: string;   // "ses" | "kumo" | "pmta"
  is_default: boolean;
}

interface SendingDomain {
  domain: string;
  from_name?: string;
  profiles?: SendingProfileOption[];
  dkim_configured: boolean;
  spf_configured: boolean;
  dmarc_configured: boolean;
  pool_name: string;
  ip_count: number;
  ips: string[];
  active_ips: number;
  warmup_ips: number;
  reputation_score: number;
  status: string;
}

interface ContentVariant {
  variant_name: string;
  from_name: string;
  subject: string;
  preview_text: string;
  html_content: string;
  split_percent: number;
}

interface AudienceEstimate {
  total_recipients: number;
  after_suppressions: number;
  suppressed_count: number;
  isp_breakdown: Record<string, number>;
  suppression_sources?: Record<string, number>;
}

interface SendTimeWindowRecommendation {
  day_of_week: string;
  start_hour: number;
  end_hour: number;
  open_rate: number;
  click_rate: number;
  source: 'historical' | 'industry';
  sample_size: number;
  confidence: number;
}

interface SendTimeDataQuality {
  source: string;
  total_sends: number;
  historical_days: number;
  has_historical: boolean;
}

interface ISPRecommendation {
  isp: string;
  display_name: string;
  windows: SendTimeWindowRecommendation[];
  data_quality: SendTimeDataQuality;
}

interface ISPTimeSpanFormState {
  id: string;
  startAt: string;
  endAt: string;
  timezone: string;
  source: string;
}

interface ISPPlanFormState {
  isp: string;
  useCustomSchedule: boolean;
  timezone: string;
  cadenceMode: 'single' | 'interval';
  everyMinutes: number;
  batchSize: number;
  durationHours: number;
  startTime: string;
  throttleStrategy: string;
  timeSpans: ISPTimeSpanFormState[];
}

interface PersistedPMTATimeSpan {
  start_at?: string;
  end_at?: string;
  timezone?: string;
  source?: string;
}

interface PersistedPMTAPlan {
  isp: string;
  quota?: number;
  throttle_strategy?: string;
  timezone?: string;
  cadence?: {
    mode?: 'single' | 'interval';
    every_minutes?: number;
    batch_size?: number;
  };
  time_spans?: PersistedPMTATimeSpan[];
}

interface PersistedPMTACampaignInput {
  campaign_id?: string;
  name?: string;
  timezone?: string;
  throttle_strategy?: string;
  target_isps?: string[];
  sending_domain?: string;
  variants?: ContentVariant[];
  isp_plans?: PersistedPMTAPlan[];
  inclusion_segments?: string[];
  inclusion_lists?: string[];
  send_priority?: { id: string; type: 'list' | 'segment' }[];
  exclusion_segments?: string[];
  exclusion_lists?: string[];
  isp_quotas?: { isp: string; volume: number }[];
  randomize_audience?: boolean;
  send_mode?: 'immediate' | 'scheduled';
  scheduled_at?: string;
  content_locked?: boolean;
  sending_profile_id?: string;
  use_master_selection?: boolean;
  min_remail_hours?: number;
  offer_id?: string;
}

interface PMTADraftResponse {
  campaign_id: string;
  name: string;
  status: string;
  schedule_mode?: 'quick' | 'per-isp';
  updated_at?: string;
  campaign_input: PersistedPMTACampaignInput;
}

interface CloneCandidate {
  id: string;
  name: string;
  status: string;
  sent_count: number;
  open_count: number;
  click_count: number;
  bounce_count: number;
  hard_bounce_count: number;
  soft_bounce_count: number;
  complaint_count: number;
  campaign_date: string;
  open_rate: number;
  click_rate: number;
  bounce_rate: number;
  hard_bounce_rate: number;
  soft_bounce_rate: number;
  complaint_rate: number;
  has_config: boolean;
  recommended: boolean;
}

interface ISPInsight {
  isp: string; label: string;
  sent: number; delivered: number; bounced: number; hard_bounces: number; soft_bounces: number;
  deferred: number; complained: number;
  opened: number; mpp_opens: number; human_opens: number;
  delivery_rate: number; bounce_rate: number; hard_bounce_rate: number; soft_bounce_rate: number;
  deferral_rate: number; complaint_rate: number;
  human_open_rate: number;
  current_quota: number; suggested_quota: number;
  recommendation: string; risk_score: number;
  signals: { type: string; direction?: string; severity?: string; pct?: number; detail: string }[];
  daily: { date: string; sent: number; delivered: number; hard_bounces: number; soft_bounces: number; deferred: number; bounce_rate: number }[];
  hourly_deferrals: number[];
}

const RECOMMENDATION_COLORS: Record<string, string> = {
  INCREASE: '#22c55e', MAINTAIN: '#3b82f6', CAUTION: '#f59e0b', DECREASE: '#ef4444', PAUSE: '#dc2626',
};

const fmtK = (n: number) => n >= 1000 ? `${(n / 1000).toFixed(1)}k` : String(n);

// ── Warm-up branch (KumoMTA newsletter warm-up) ──────────────────────────────
//
// The wizard is ONE wizard with two content/audience branches. Steps 1, 2, 5
// and 6 are shared; only steps 3 and 4 differ. The OFFER branch is the live
// send path and is left byte-identical — every warm-up code path below is
// gated on `warmupActive`, and the offer renderers/validators/payload builder
// are never re-entered from here.
//
// Warm-up content is EDITORIAL by design: offers are banned in it (CLAUDE.md
// §13.1), so this branch has no offer picker at all.

// GET /pmta-campaign/warmup/domains rows. The wire field is `sending_domain`
// (warmup_requests.go warmupDomain.SendingDomain) — reading `domain` filtered
// EVERY row out, which is why the whole warm-up branch was unreachable in the
// UI. `domain` below is a LOCAL normalized alias assigned at the fetch
// boundary; nothing here is read straight off the wire.
interface WarmupDomainRow {
  domain: string;                 // local alias for the wire `sending_domain`
  brand_slug?: string;
  brand_slug_source?: string;     // "creative-filename" (authoritative) | "apex-fallback"
  brand_code?: string;
  apex?: string;
  // from_name/from_email come from the DOMAIN's sending profile, never from a
  // creative row — DKIM alignment drifts otherwise.
  from_name?: string;
  from_email?: string;
  // Optional: if the estate endpoint knows the cold feeds for a property it can
  // return them and the cold-source field upgrades from free text to a picker.
  cold_sources?: string[];
}

// GET /pmta-campaign/warmup/creative. Wire fields are `creative_id` and
// `html_length`; `id`/`html_bytes` below are LOCAL aliases assigned at the
// fetch boundary (warmup_requests.go warmupCreativeResp).
interface WarmupCreative {
  id: string;                     // local alias for the wire `creative_id`
  subject: string;
  preheader: string;
  // FRESHNESS IS `updated_at`. `generated_at` is frozen at first insert and
  // reads days stale on a creative that was refreshed this morning — reading it
  // as freshness is what made two domains look current while they re-sent the
  // same bytes for 15 days. `generated_at` is rendered ONLY as "first created".
  updated_at: string;
  generated_at?: string;
  html_bytes?: number;            // local alias for the wire `html_length`
  sha256?: string;
  // Not part of the documented response shape; read opportunistically so the
  // Preview can render a real body when the endpoint carries one.
  html?: string;
  html_content?: string;
}

// GET /pmta-campaign/warmup/segments. Wire field is `segment_id`; `id` is a
// LOCAL alias assigned at the fetch boundary.
interface WarmupSegment {
  id: string;                     // local alias for the wire `segment_id`
  name: string;
  // ⚠️ ZEROED when a segment refresh times out — a healthy segment can read 0.
  // Never rendered as a confident zero; see warmupSegmentCount().
  subscriber_count?: number | null;
}

interface WarmupRequestRow {
  id?: string;
  sending_domain?: string;
  brand_slug?: string;
  status?: string;              // requested | building | built | failed
  scheduled_at?: string;
  cold_source?: string;
  cold_quota?: number;
  build_note?: string;
  created_at?: string;
  updated_at?: string;
}

// A creative older than this has almost certainly not been re-registered today.
const WARMUP_STALE_MS = 24 * 3600 * 1000;

const WARMUP_STATUS_COLORS: Record<string, string> = {
  requested: '#38bdf8',
  building:  '#f59e0b',
  built:     '#10b981',
  failed:    '#ef4444',
};

// `subscriber_count` is zeroed on a refresh timeout, so 0 is UNKNOWN, not zero.
const warmupSegmentCount = (seg: WarmupSegment): { known: boolean; value: number } => {
  const n = seg.subscriber_count;
  if (n === null || n === undefined || n === 0) return { known: false, value: 0 };
  return { known: true, value: n };
};

// Property slug for the creative/segment lookups. Prefer what the estate
// endpoint states; only fall back to deriving it from the sending domain, and
// the resolved value is always shown in the UI so a wrong guess is visible
// rather than silently querying the wrong brand.
const deriveBrandSlug = (row: WarmupDomainRow | undefined, domain: string): string => {
  if (row?.brand_slug) return row.brand_slug;
  if (row?.brand_code) return row.brand_code.toLowerCase();
  const apex = domain.replace(/^em\./i, '').replace(/^m\./i, '');
  const label = apex.split('.')[0] || '';
  return label.toLowerCase();
};

const fmtBytes = (n?: number): string =>
  n === undefined || n === null ? '—' : `${n.toLocaleString()} bytes`;

// "registered 04:45 today" / "registered Aug 14, 04:45" + an explicit age.
const fmtFreshness = (iso?: string): { text: string; ageMs: number | null } => {
  if (!iso) return { text: 'never registered', ageMs: null };
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return { text: 'unreadable timestamp', ageMs: null };
  const ageMs = Date.now() - t;
  const d = new Date(t);
  const hhmm = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  const sameDay = d.toDateString() === new Date().toDateString();
  const hours = Math.floor(ageMs / 3600000);
  const rel = hours < 1 ? 'under an hour ago' : hours < 48 ? `${hours}h ago` : `${Math.floor(hours / 24)}d ago`;
  return {
    text: sameDay ? `registered ${hhmm} today (${rel})` : `registered ${d.toLocaleDateString()} ${hhmm} (${rel})`,
    ageMs,
  };
};

// ── Newsletters branch (estate-wide daily-fresh newsletters) ─────────────────
//
// The third campaign mode. Content is AUTOMATIC: the daily pipeline registers
// one newsletter per sending domain in Creative Studio, so this mode never
// picks a creative — it AUDITS what will mail (subject, preheader, friendly
// from, freshness, readiness) across every eligible domain at once, takes ONE
// scheduled instant for all of them, and still makes the operator choose the
// audience.
//
// Contract (fixed):
//   GET /pmta-campaign/newsletter/preview?include_html=0|1[&sending_domain=…]
//   -> { day, domains: [ { sending_domain, apex, brand_slug, from_name,
//        from_email, creative_id, creative_sha256, filename, subject,
//        preheader, updated_at, approval_status, html?, status, reason } ] }
// Omitting sending_domain returns ALL eligible domains — the default here.

type NewsletterStatus = 'ready' | 'stale' | 'missing';

interface NewsletterDomainRow {
  sending_domain: string;
  apex?: string;
  brand_slug?: string;
  from_name?: string;
  from_email?: string;
  creative_id?: string;
  creative_sha256?: string;
  filename?: string;
  subject?: string;
  preheader?: string;
  updated_at?: string;
  approval_status?: string;
  html?: string;                 // only when include_html=1
  status?: string;               // ready | missing | stale
  reason?: string;               // a sentence whenever status is not ready
}

const NEWSLETTER_STATUS_COLORS: Record<NewsletterStatus, string> = {
  ready:   '#10b981',
  stale:   '#f59e0b',
  missing: '#ef4444',
};

// The server states `status`. When it does not, the status is DERIVED here and
// flagged as derived — a missing status must never render as "ready".
const newsletterStatusOf = (r: NewsletterDomainRow): { status: NewsletterStatus; stated: boolean } => {
  const s = (r.status || '').trim().toLowerCase();
  if (s === 'ready' || s === 'stale' || s === 'missing') return { status: s, stated: true };
  if (!r.creative_id) return { status: 'missing', stated: false };
  const f = fmtFreshness(r.updated_at);
  if (f.ageMs === null || f.ageMs > WARMUP_STALE_MS) return { status: 'stale', stated: false };
  return { status: 'ready', stated: false };
};

// Why a domain is not ready, always as a sentence. Absence of a creative is the
// single most important thing this screen can tell the operator, so it is never
// rendered as an empty cell.
const newsletterReasonOf = (r: NewsletterDomainRow, st: NewsletterStatus): string => {
  const stated = (r.reason || '').trim();
  if (stated) return stated;
  if (st === 'missing') {
    return `No newsletter is registered for ${r.sending_domain} today — there is no creative row at all, `
      + 'so this domain has nothing to build. Register one (agents.jobs.kumo_newsletter_stage) before queueing.';
  }
  if (st === 'stale') {
    return 'The registered creative has not been re-registered in over 24 hours. A stale newsletter mails '
      + 'byte-identically with no error — re-run the daily registration for this property before queueing.';
  }
  return '';
};

// Engagement posture for the newsletters audience step. ONE choice applied to
// every selected domain; "all engagement" is the typical choice, never an
// automatic one.
type NewsletterAudiencePosture = 'all' | 'clickers' | 'openers';

const NEWSLETTER_POSTURES: { id: NewsletterAudiencePosture; label: string; hint: string }[] = [
  { id: 'all',      label: 'All engagement', hint: 'Every engaged-anchor segment the property resolves — clickers, openers and all-time engaged pools.' },
  { id: 'clickers', label: 'Clickers only',  hint: 'Only anchor segments whose name identifies them as clickers (click = GOLD in the signal grading).' },
  { id: 'openers',  label: 'Openers only',   hint: 'Only anchor segments whose name identifies them as openers (open = silver).' },
];

// Segment kind, read from the NAME. The engaged grid names segments
// "<BRAND> 30D Clickers" / "<BRAND> 7D Openers", so the name is the only
// classifier available on this endpoint. Anything that matches neither is
// counted and shown separately rather than silently dropped.
const newsletterSegmentKind = (name: string): 'clickers' | 'openers' | 'other' => {
  const n = (name || '').toLowerCase();
  if (n.includes('clicker') || n.includes('click')) return 'clickers';
  if (n.includes('opener') || n.includes('open')) return 'openers';
  return 'other';
};

// ── Campaign mode — a THREE-VALUE discriminant, never two booleans ──────────
//
// `warmupActive` used to be a boolean, so a third mode would have taken the
// OFFERS path at every branch point — including the step-6 submit fork, which
// POSTs to /pmta-campaign/deploy. Every fork below switches on this union and
// ends in assertUnreachableMode(), so a fourth mode is a COMPILE error rather
// than a silent offer deploy.
type CampaignMode = 'offers' | 'warmup' | 'newsletter';

function assertUnreachableMode(mode: never): never {
  throw new Error(`Unhandled campaign mode: ${String(mode)}`);
}

// ── Step navigation ──────────────────────────────────────────────────────────

// Step order (operator 2026-08-18): the SENDING DOMAIN comes first. The pinned
// profile decides the transport, and the transport decides which ISP lanes are
// even legal (a kumo route is yahoo-family only) — so choosing providers and
// their quotas before the domain was backwards.
const STEPS = [
  { id: 1, label: 'Sending Domain',         icon: faGlobe },
  { id: 2, label: 'Mailbox Providers',      icon: faServer },
  { id: 3, label: 'Offer + Creative',       icon: faPenFancy },
  { id: 4, label: 'Engagement Audience',    icon: faUsers },
  // Step 5 ("Sending Insights") was retired 2026-08-20 (operator: "Remove Step
  // 5 for both of the flows. It is no longer needed."). The remaining ids are
  // deliberately NOT renumbered — `step === 6`, the getStepErrors cases and the
  // stepAttempted keys all key off these numbers. Only the ORDER matters, and
  // navigation walks this list rather than doing ±1 arithmetic.
  { id: 6, label: 'Schedule + Deploy',      icon: faRocket },
];

// Every mode runs the SAME step ids in the SAME order and only relabels — no
// mode adds, removes or renumbers a step. That matters twice: the header
// counter renders POSITION (correct for the sparse ids), and the indicator
// strip's `s.id < step` completeness test stays valid only while id order and
// list position agree.
const MODE_STEP_OVERRIDES: Record<CampaignMode, Record<number, { label: string; icon: typeof faNewspaper }>> = {
  offers: {},
  warmup: {
    3: { label: 'Newsletter',             icon: faNewspaper },
    4: { label: 'Audience + Cold Source', icon: faSnowflake },
  },
  newsletter: {
    1: { label: 'Mode + Sending Domains', icon: faGlobe },
    3: { label: 'Newsletter Audit',       icon: faNewspaper },
    4: { label: 'Engagement Audience',    icon: faUsers },
    6: { label: 'Schedule + Queue',       icon: faRocket },
  },
};

// ── Main component ───────────────────────────────────────────────────────────

interface PMTACampaignWizardProps {
  onClose?: () => void;
  editCampaignId?: string | null;
  onEditComplete?: () => void;
  onCampaignPreparing?: (id: string, name: string) => void;
}

export const PMTACampaignWizard: React.FC<PMTACampaignWizardProps> = ({ onClose, editCampaignId, onEditComplete, onCampaignPreparing }) => {
  const { organization } = useAuth();
  const orgId = organization?.id || '';
  const { campaignComplete } = useToast();

  const [step, setStep] = useState(1);
  const [showCompleteModal, setShowCompleteModal] = useState(false);
  const [readinessLoading, setReadinessLoading] = useState(false);
  const [audienceDataLoading, setAudienceDataLoading] = useState(false);
  const [estimating, setEstimating] = useState(false);

  // Mailbox Providers step (step 2 since the 2026-08-18 reorder) state
  const [ispReadiness, setISPReadiness] = useState<ISPReadiness[]>([]);
  const [selectedISPs, setSelectedISPs] = useState<string[]>([...ALL_ISPS]);
  const [ispQuotas, setISPQuotas] = useState<Record<string, number>>({ ...DEFAULT_ISP_QUOTAS });
  // "No quota / no caps" is a BULK FORM ACTION, not a mode flag and not a
  // payload field. Turning it on writes 0 into every per-ISP quota input;
  // nothing about what the app READS changes (per-ISP caps are still fetched
  // and still displayed), and the payload carries the same per-ISP numbers it
  // always did — they just happen to be zeros. This holds the pre-zero values
  // so toggling off restores them instead of stranding zeros.
  const [quotaSnapshot, setQuotaSnapshot] = useState<Record<string, number> | null>(null);
  const [randomizeAudience, setRandomizeAudience] = useState(false);
  // content_locked: when on, the PMTA wave dispatcher skips subject/HTML
  // fingerprint mutations at send time. Required for strict advertisers
  // (e.g. TruGreen) who demand byte-faithful delivery of the approved creative.
  // Honeypot injection and URL sanitization remain active.
  const [contentLocked, setContentLocked] = useState(false);

  // ISP Sending Health insights
  const [ispInsights, setIspInsights] = useState<ISPInsight[]>([]);
  const [insightsLoading, setInsightsLoading] = useState(false);
  const [expandedInsightISP, setExpandedInsightISP] = useState<string | null>(null);
  const [insightsCollapsed, setInsightsCollapsed] = useState(false);
  const [insightDomainFilter, setInsightDomainFilter] = useState('');
  const [insightAvailableDomains, setInsightAvailableDomains] = useState<string[]>([]);

  // Deliverability Recommendations modal
  const [delivRecsOpen, setDelivRecsOpen] = useState(false);
  const [delivRecsLoading, setDelivRecsLoading] = useState(false);
  const [delivRecsDomain, setDelivRecsDomain] = useState('');
  const [delivRecsResult, setDelivRecsResult] = useState<any>(null);
  const [delivRecsError, setDelivRecsError] = useState('');
  const [delivRecsDomains, setDelivRecsDomains] = useState<{ domain: string }[]>([]);

  // Sending Domain step (step 1 since the 2026-08-18 reorder) state
  const [sendingDomains, setSendingDomains] = useState<SendingDomain[]>([]);
  const [selectedDomain, setSelectedDomain] = useState('');
  // Pinned sending profile. A domain can carry several active profiles
  // (m.discountblog.com has four), and the server's by-domain auto-lookup takes
  // the most recently created one — so an SES route is only deterministic when
  // the profile is pinned. Empty = keep the legacy auto-lookup.
  const [selectedProfileId, setSelectedProfileId] = useState('');
  // Offer selection (audience unification P3): optional; flows into the
  // draft/deploy payload as offer_id so attribution + offer suppression fire.
  const [offersCatalog, setOffersCatalog] = useState<{ id: string; key: string; name: string; status: string }[]>([]);
  const [selectedOfferId, setSelectedOfferId] = useState('');
  const [offersError, setOffersError] = useState('');

  // Step 3 state
  const [variants, setVariants] = useState<ContentVariant[]>([
    { variant_name: 'A', from_name: '', subject: '', preview_text: '', html_content: '', split_percent: 100 },
  ]);
  // Content is sourced exclusively from the Creative Studio offers library
  // (OfferCreativePicker) — the template / AI-generation / paste-HTML paths were
  // removed so an unapproved creative cannot be scheduled from this wizard.

  // Engagement ranges (step 4 headline) — the board's audience primitive.
  const [engagementTiers, setEngagementTiers] = useState<EngagementTiers | null>(null);
  const [engagementLoading, setEngagementLoading] = useState(false);
  const [engagementError, setEngagementError] = useState('');
  const [engagementReloadKey, setEngagementReloadKey] = useState(0);
  // Recent campaign outcomes for the property — the only way to see an
  // OPERATIONAL hold (staged then cancelled) that no registry records.
  const [sendHistory, setSendHistory] = useState<{
    counts: Record<string, number>; total: number; cancel_rate: number;
    last_sent_at?: string; days: number;
  } | null>(null);
  // Inclusion ids restored from a draft, held until the property's engagement
  // grid loads and can say which are clickers and which are openers.
  const [restoredInclusionSegments, setRestoredInclusionSegments] = useState<string[]>([]);
  const [selectedClickerIds, setSelectedClickerIds] = useState<string[]>([]);
  // All-time engaged pools (no recency window). The kumo warm-up estate's real
  // audience lives here, not in the tiny 30D windows.
  const [selectedOtherIds, setSelectedOtherIds] = useState<string[]>([]);
  const [selectedOpenerIds, setSelectedOpenerIds] = useState<string[]>([]);
  // Engager disjointness. DEFAULT OFF (2026-08-18): it used to default ON, so
  // selecting a clicker tier AND an opener tier silently pushed the clicker
  // segments into exclusion_segments while Send Priority still listed them as
  // locked row #1. The planner denies excluded emails inside qualifyEmail
  // (pmta_campaign_planner.go:934), so the operator's #1 audience contributed
  // ZERO and the panel said the opposite — observed on
  // "Aug18 - DB - OFR-ENG - Globe Life v2": DB 60D Clickers planned 0 of 34,260.
  // Selecting a tier now means MAILING it; disjointness is an explicit opt-in.
  const [excludeClickers, setExcludeClickers] = useState(false);
  // Audience-bound = the standing uncapped engaged-tier doctrine: quota 0 per
  // ISP so the segment is the cap. Capping an engaged tier is the EXCEPTION.
  const [audienceBound, setAudienceBound] = useState(true);
  // Master-list top-up. The column defaults to TRUE server-side, so the wizard
  // always sends this explicitly; the server additionally coerces it to false
  // for uncapped segment audiences (coerceMasterSelectionForSegmentAudience).
  const [masterTopUp, setMasterTopUp] = useState(false);

  // Step 3 (content) — everything comes from the selected Creative Studio proof.
  const [selectedProofId, setSelectedProofId] = useState('');
  const [selectedProofName, setSelectedProofName] = useState('');

  // Step 4 state
  const [lists, setLists] = useState<{ id: string; name: string; subscriber_count: number }[]>([]);
  const [segments, setSegments] = useState<{ id: string; name: string; subscriber_count: number; category?: string; status?: string }[]>([]);
  const [suppressionLists, setSuppressionLists] = useState<{ id: string; name: string; entry_count: number }[]>([]);
  const [selectedLists, setSelectedLists] = useState<string[]>([]);
  const [selectedSegments, setSelectedSegments] = useState<string[]>([]);
  const [sendPriority, setSendPriority] = useState<{ id: string; type: 'list' | 'segment' }[]>([]);
  const [selectedSuppLists, setSelectedSuppLists] = useState<string[]>([]);
  const [selectedExclusionSegments, setSelectedExclusionSegments] = useState<string[]>([]);
  const [inclusionSearch, setInclusionSearch] = useState('');
  const [suppressionSearch, setSuppressionSearch] = useState('');
  const [exclusionSearch, setExclusionSearch] = useState('');
  // Category filter sets default to the operator-facing categories that
  // make sense for inclusion (engagement/framework/funnel/cohort) vs
  // exclusion (suppression/exclusion). Partner-wave-static and
  // legacy_snapshot are intentionally hidden by default; the operator
  // can re-enable them via the filter dropdown or the "Show all" toggle.
  const [activeIncCategories, setActiveIncCategories] = useState<Set<SegmentCategory>>(
    () => defaultVisibleCategoriesForPicker('inclusion'),
  );
  const [activeExcCategories, setActiveExcCategories] = useState<Set<SegmentCategory>>(
    () => defaultVisibleCategoriesForPicker('exclusion'),
  );
  const [showArchived, setShowArchived] = useState(false);
  const [audienceEstimate, setAudienceEstimate] = useState<AudienceEstimate | null>(null);
  const [audienceError, setAudienceError] = useState('');

  // Step 6 state
  const [campaignName, setCampaignName] = useState('');
  // Auto-naming stops the moment the operator types: a derived default is a
  // convenience, never an override of an explicit choice.
  const [nameTouched, setNameTouched] = useState(false);
  // The 01:01-tomorrow schedule default is seeded at most once per campaign
  // build; every operator schedule action afterwards wins.
  const [scheduleSeeded, setScheduleSeeded] = useState(false);
  const [sendMode, setSendMode] = useState<'immediate' | 'scheduled'>('scheduled');
  const [scheduleMode, setScheduleMode] = useState<'quick' | 'per-isp'>('per-isp');
  const [scheduledAt, setScheduledAt] = useState('');
  const [recommendations, setRecommendations] = useState<ISPRecommendation[]>([]);
  const [ispPlansByKey, setISPPlansByKey] = useState<Record<string, ISPPlanFormState>>({});
  const [globalScheduleDuration, setGlobalScheduleDuration] = useState(8);
  const [globalScheduleInterval, setGlobalScheduleInterval] = useState(15);
  const [globalScheduleStart, setGlobalScheduleStart] = useState('');
  const [globalScheduleTimezone, setGlobalScheduleTimezone] = useState(SEND_DAY_TIMEZONE);
  const [campaignTimezone, setCampaignTimezone] = useState(SEND_DAY_TIMEZONE);
  const [throttleStrategy, setThrottleStrategy] = useState(DEFAULT_THROTTLE_STRATEGY);
  const [activePreset, setActivePreset] = useState('');
  // Send-day gate failures come back as HTTP 412 with the failed gates and an
  // override hint. Without a UI for it the Campaign Manager is unusable on any
  // day Gate A has not been attested, so the operator can re-submit the same
  // payload with an audit-logged reason.
  const [gateFailure, setGateFailure] = useState<{ error: string; failed_gates: any[] } | null>(null);
  const [gateOverrideReason, setGateOverrideReason] = useState('');
  const [recsLoading, setRecsLoading] = useState(false);
  const [recsLoaded, setRecsLoaded] = useState(false);
  const [deploying, setDeploying] = useState(false);
  const [deployResult, setDeployResult] = useState<any>(null);
  const [campaignId, setCampaignId] = useState('');
  const [loadingDraft, setLoadingDraft] = useState(true);
  const [savingDraft, setSavingDraft] = useState(false);
  const [draftStatus, setDraftStatus] = useState('');
  const [draftError, setDraftError] = useState('');
  const [domainError, setDomainError] = useState('');
  // Type-to-filter the sending-domain list (operator 2026-08-18) — the estate
  // is 27+ properties and scrolling for one was the slow part.
  const [domainSearch, setDomainSearch] = useState('');

  // Clone state
  const [showClonePanel, setShowClonePanel] = useState(false);
  const [cloneCandidates, setCloneCandidates] = useState<CloneCandidate[]>([]);
  const [cloneError, setCloneError] = useState('');
  const [cloneApex, setCloneApex] = useState('');
  const [cloneLoading, setCloneLoading] = useState(false);
  const [cloneApplying, setCloneApplying] = useState('');
  const clonePanelRef = useRef<HTMLDivElement>(null);

  // ── Warm-up branch state ─────────────────────────────────────────────────
  // Nothing here is read by the offer flow. `warmupActive` is the single gate.
  const [warmupDomains, setWarmupDomains] = useState<WarmupDomainRow[]>([]);
  const [warmupDomainsLoading, setWarmupDomainsLoading] = useState(false);
  const [warmupDomainsError, setWarmupDomainsError] = useState('');
  const [warmupDomainsKey, setWarmupDomainsKey] = useState(0);
  // THE MODE. One three-value discriminant, not two booleans — two booleans
  // give four states, two of which are nonsense.
  const [campaignMode, setCampaignMode] = useState<CampaignMode>('offers');
  // Which domain the warm-up default has already been applied for, so a
  // deliberate mode change is never stomped by a late-arriving domain list.
  const [warmupDefaultAppliedFor, setWarmupDefaultAppliedFor] = useState('');

  // Step 3 (warm-up): the daily-registered newsletter + the operator's overrides.
  const [warmupCreative, setWarmupCreative] = useState<WarmupCreative | null>(null);
  const [warmupCreativeLoading, setWarmupCreativeLoading] = useState(false);
  const [warmupCreativeError, setWarmupCreativeError] = useState('');
  const [warmupCreativeKey, setWarmupCreativeKey] = useState(0);
  const [warmupSubject, setWarmupSubject] = useState('');
  const [warmupPreheader, setWarmupPreheader] = useState('');
  // Empty until the operator edits — so the payload can carry the creative's
  // own copy unchanged and an override is always a deliberate act.
  const [warmupCopyTouched, setWarmupCopyTouched] = useState(false);
  const [warmupPreviewOpen, setWarmupPreviewOpen] = useState(false);
  const [warmupPreviewHtml, setWarmupPreviewHtml] = useState('');
  const [warmupPreviewLoading, setWarmupPreviewLoading] = useState(false);
  const [warmupPreviewError, setWarmupPreviewError] = useState('');

  // Step 4 (warm-up): engaged anchors + the cold pad.
  const [warmupSegments, setWarmupSegments] = useState<WarmupSegment[]>([]);
  const [warmupSegmentsLoading, setWarmupSegmentsLoading] = useState(false);
  const [warmupSegmentsError, setWarmupSegmentsError] = useState('');
  const [warmupSegmentsKey, setWarmupSegmentsKey] = useState(0);
  const [warmupSelectedSegmentIds, setWarmupSelectedSegmentIds] = useState<string[]>([]);
  const [coldSource, setColdSource] = useState('');
  // String, not number: an empty box must stay empty rather than render as 0.
  const [coldQuota, setColdQuota] = useState('');

  // Step 6 (warm-up): the build REQUEST and its status ledger.
  const [warmupSubmitting, setWarmupSubmitting] = useState(false);
  const [warmupResult, setWarmupResult] = useState<{ error?: string; request?: WarmupRequestRow } | null>(null);
  const [warmupRequests, setWarmupRequests] = useState<WarmupRequestRow[] | null>(null);
  const [warmupRequestsLoading, setWarmupRequestsLoading] = useState(false);
  const [warmupRequestsError, setWarmupRequestsError] = useState('');
  const [warmupRequestsKey, setWarmupRequestsKey] = useState(0);
  const [warmupRequestsFetchedAt, setWarmupRequestsFetchedAt] = useState('');

  // ── Newsletters branch state ─────────────────────────────────────────────
  // Estate-wide, not per-`selectedDomain`: one roster, one scheduled instant,
  // one audience posture, N build requests.
  const [newsletterRows, setNewsletterRows] = useState<NewsletterDomainRow[] | null>(null);
  const [newsletterDay, setNewsletterDay] = useState('');
  const [newsletterLoading, setNewsletterLoading] = useState(false);
  const [newsletterError, setNewsletterError] = useState('');
  const [newsletterKey, setNewsletterKey] = useState(0);
  const [newsletterFetchedAt, setNewsletterFetchedAt] = useState('');
  // ALL eligible domains are included by default (the mode's whole point), so
  // this holds the operator's DESELECTIONS. A domain that appears tomorrow is
  // therefore included automatically rather than silently dropped.
  const [newsletterExcluded, setNewsletterExcluded] = useState<Set<string>>(new Set());
  // A stale creative re-mails identical bytes with no error, so stale domains
  // are excluded by default and must be opted back in one at a time.
  const [newsletterStaleOptIn, setNewsletterStaleOptIn] = useState<Set<string>>(new Set());
  // Per-domain body preview, on demand (include_html=1 for ONE domain).
  const [newsletterPreviewDomain, setNewsletterPreviewDomain] = useState('');
  const [newsletterPreviewHtml, setNewsletterPreviewHtml] = useState('');
  const [newsletterPreviewLoading, setNewsletterPreviewLoading] = useState(false);
  const [newsletterPreviewError, setNewsletterPreviewError] = useState('');
  // Audience: ONE posture for all domains, resolved to each domain's own
  // anchor segments so the operator sees what each property will actually mail.
  const [newsletterPosture, setNewsletterPosture] = useState<NewsletterAudiencePosture>('all');
  const [newsletterAnchors, setNewsletterAnchors] = useState<Record<string, WarmupSegment[]>>({});
  const [newsletterAnchorErrors, setNewsletterAnchorErrors] = useState<Record<string, string>>({});
  const [newsletterAnchorsLoading, setNewsletterAnchorsLoading] = useState(false);
  const [newsletterAnchorsKey, setNewsletterAnchorsKey] = useState(0);
  // Fan-out submit: one result row per domain, never a single blended verdict.
  const [newsletterSubmitting, setNewsletterSubmitting] = useState(false);
  const [newsletterResults, setNewsletterResults] = useState<
    { sending_domain: string; ok: boolean; error?: string; request_id?: string }[] | null
  >(null);

  // ── Validation state ─────────────────────────────────────────────────────
  const [stepAttempted, setStepAttempted] = useState<Record<number, boolean>>({});

  // Reset deploy result when navigating away from step 6
  useEffect(() => {
    if (step !== 6 && deployResult) setDeployResult(null);
  }, [step, deployResult]);

  // ── Data fetching with retry ────────────────────────────────────────────

  const fetchWithRetry = useCallback(async (url: string, opts?: RequestInit, retries = 2): Promise<Response> => {
    for (let i = 0; i <= retries; i++) {
      try {
        const res = await orgFetch(url, orgId, opts);
        if (res.ok) return res;
        if (i < retries && res.status >= 500) {
          await new Promise(r => setTimeout(r, 1000 * (i + 1)));
          continue;
        }
        return res;
      } catch (err) {
        if (i < retries) {
          await new Promise(r => setTimeout(r, 1000 * (i + 1)));
          continue;
        }
        throw err;
      }
    }
    return orgFetch(url, orgId, opts);
  }, [orgId]);

  // Stable org-scoped fetcher handed to child panels. Children key their load
  // effects off this identity, so it must NOT be an inline arrow.
  const pickerFetch = useCallback(
    (url: string, opts?: RequestInit) => orgFetch(url, orgId, opts),
    [orgId],
  );

  const fetchReadiness = useCallback(async () => {
    setReadinessLoading(true);
    try {
      const res = await fetchWithRetry(`${API_BASE}/pmta-campaign/readiness`);
      const data = await res.json();
      setISPReadiness(data.isps || []);
    } catch (err) {
      console.warn('[Wizard] readiness fetch failed:', err);
    }
    setReadinessLoading(false);
  }, [fetchWithRetry]);

  const fetchInsights = useCallback(async (domain?: string) => {
    setInsightsLoading(true);
    try {
      const qs = domain ? `?sending_domain=${encodeURIComponent(domain)}` : '';
      const res = await orgFetch(`${API_BASE}/analytics/isp-sending-insights${qs}`, orgId);
      const data = await res.json();
      setIspInsights(data?.isps || []);
      if (data?.sending_domains) {
        setInsightAvailableDomains(data.sending_domains);
      }
    } catch (err) {
      console.warn('[Wizard] ISP insights fetch failed:', err);
    }
    setInsightsLoading(false);
  }, [orgId]);

  const openDelivRecsModal = useCallback(async () => {
    setDelivRecsOpen(true);
    setDelivRecsResult(null);
    setDelivRecsError('');
    if (delivRecsDomains.length === 0) {
      try {
        const res = await fetchWithRetry(`${API_BASE}/pmta-campaign/sending-domains`);
        if (res.ok) {
          const data = await res.json();
          setDelivRecsDomains((data.domains || []).map((d: any) => ({ domain: d.domain || d })));
        }
      } catch { /* ignore */ }
    }
  }, [fetchWithRetry, delivRecsDomains.length]);

  const fetchDeliverabilityRecs = useCallback(async () => {
    if (!delivRecsDomain) return;
    setDelivRecsLoading(true);
    setDelivRecsError('');
    setDelivRecsResult(null);
    try {
      const res = await orgFetch(`${API_BASE}/pmta-campaign/deliverability-recs`, orgId, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ sending_domain: delivRecsDomain }),
      });
      const data = await res.json();
      if (!res.ok) {
        setDelivRecsError(data?.error || 'Failed to get recommendations');
      } else {
        setDelivRecsResult(data);
      }
    } catch (err: any) {
      setDelivRecsError(err?.message || 'Network error');
    }
    setDelivRecsLoading(false);
  }, [delivRecsDomain, orgId]);

  const applyAllDelivRecs = useCallback(() => {
    if (!delivRecsResult?.recommendations) return;
    const updated: Record<string, number> = { ...ispQuotas };
    for (const rec of delivRecsResult.recommendations) {
      const key = rec.isp?.toLowerCase();
      if (key && rec.suggested_quota > 0) {
        updated[key] = rec.suggested_quota;
      }
    }
    setISPQuotas(updated);
    setDelivRecsOpen(false);
  }, [delivRecsResult, ispQuotas]);

  const fetchDomains = useCallback(async () => {
    setDomainError('');
    try {
      const res = await fetchWithRetry(`${API_BASE}/pmta-campaign/sending-domains`);
      if (!res.ok) {
        setDomainError('Failed to load sending domains. Retry or check Domain Center.');
        return;
      }
      const data = await res.json();
      setSendingDomains(data.domains || []);
    } catch {
      setDomainError('Network error loading domains. Click retry.');
    }
  }, [fetchWithRetry]);

  const fetchOffers = useCallback(async () => {
    setOffersError('');
    try {
      const res = await fetchWithRetry(`${API_BASE}/offers/list`);
      if (!res.ok) {
        setOffersError(`Failed to load offers (HTTP ${res.status}).`);
        return;
      }
      const data = await res.json();
      setOffersCatalog(data.offers || []);
    } catch {
      setOffersError('Network error loading offers.');
    }
  }, [fetchWithRetry]);

  // Engagement grid for the selected property. One request serves both the
  // audience step (the chips) and the content step (brand_root, which decides
  // which approved proofs are cleared for this property).
  useEffect(() => {
    if (!selectedDomain) {
      setEngagementTiers(null);
      setEngagementError('');
      return;
    }
    let cancelled = false;
    setEngagementLoading(true);
    setEngagementError('');
    orgFetch(`${API_BASE}/pmta-campaign/engagement-tiers?sending_domain=${encodeURIComponent(selectedDomain)}`, orgId)
      .then(async res => {
        const data = await res.json();
        if (cancelled) return;
        if (!res.ok) { setEngagementError(data.error || `HTTP ${res.status}`); setEngagementTiers(null); return; }
        setEngagementTiers(data as EngagementTiers);
      })
      .catch(err => { if (!cancelled) { setEngagementError(err?.message || 'network error'); setEngagementTiers(null); } })
      .finally(() => { if (!cancelled) setEngagementLoading(false); });
    return () => { cancelled = true; };
  }, [selectedDomain, orgId, engagementReloadKey]);

  useEffect(() => {
    if (!selectedDomain) { setSendHistory(null); return; }
    let cancelled = false;
    orgFetch(`${API_BASE}/pmta-campaign/domain-send-history?sending_domain=${encodeURIComponent(selectedDomain)}&days=7`, orgId)
      .then(r => r.json())
      .then(d => { if (!cancelled && d && !d.error) setSendHistory(d); })
      .catch(() => { /* advisory only — never blocks the flow */ });
    return () => { cancelled = true; };
  }, [selectedDomain, orgId]);

  // Selecting a DIFFERENT property invalidates the property-scoped choices.
  // Guarded by a ref so it does not fire on the initial mount or on the
  // domain a restored draft just installed — otherwise it would immediately
  // wipe the pinned profile that applyCampaignInput restored in the same tick.
  const prevDomainRef = useRef<string | null>(null);
  // The domain a draft/clone restore just installed. hydrateDraft sets it so the
  // effect below can tell "the restore moved the domain" (keep what it restored)
  // from "the operator switched property" (clear what belonged to the old one).
  const hydratedDomainRef = useRef<string | null>(null);
  useEffect(() => {
    const prev = prevDomainRef.current;
    prevDomainRef.current = selectedDomain;
    if (prev === null || prev === selectedDomain) return;
    if (hydratedDomainRef.current === selectedDomain) {
      hydratedDomainRef.current = null;
      return;
    }
    // A restore that did not move the domain leaves the ref set; drop it here so
    // it can never skip a later, genuine property switch.
    hydratedDomainRef.current = null;
    setSelectedClickerIds([]);
    setSelectedOpenerIds([]);
    setSelectedOtherIds([]);
    setSelectedProfileId('');
    // EVERY audience pick is property-scoped, not just the engagement chips.
    // Clearing only the chips is how a restored YI draft carried seven
    // SLOT-*-YI-* cohorts into a discountblog send on 2026-08-18: the wizard
    // kept them in send_priority/inclusion_segments across the property switch
    // and the planner mailed 5,694 recipients with no DB engagement at all.
    // Suppression LISTS are estate-wide (and the global list is auto-ticked),
    // so those stay.
    setSelectedSegments([]);
    setSelectedLists([]);
    setSendPriority([]);
    setSelectedExclusionSegments([]);
  }, [selectedDomain]);

  // Re-hydrate the engagement chips from a restored draft once the grid is in.
  useEffect(() => {
    if (!engagementTiers || restoredInclusionSegments.length === 0) return;
    const clickerIds = new Set(engagementTiers.clickers.map(t => t.segment_id));
    const openerIds = new Set(engagementTiers.openers.map(t => t.segment_id));
    const restoredClickers = restoredInclusionSegments.filter(id => clickerIds.has(id));
    const restoredOpeners = restoredInclusionSegments.filter(id => openerIds.has(id));
    const otherIds = new Set(engagementTiers.other.map(t => t.segment_id));
    const restoredOther = restoredInclusionSegments.filter(id => otherIds.has(id));
    if (restoredClickers.length > 0) setSelectedClickerIds(restoredClickers);
    if (restoredOpeners.length > 0) setSelectedOpenerIds(restoredOpeners);
    if (restoredOther.length > 0) setSelectedOtherIds(restoredOther);
    setRestoredInclusionSegments([]);
  }, [engagementTiers, restoredInclusionSegments]);

  // The pinned profile decides the transport, and the transport decides which
  // creative library and which ISP lanes are legal.
  // Domain filter matches the domain itself, its pool, and its profile names /
  // transports — so "kumo", "ses" or a persona narrows the list too. The
  // currently selected domain is always kept visible so a stale filter can
  // never hide what the campaign is actually pinned to.
  const filteredSendingDomains = useMemo(() => {
    const q = domainSearch.trim().toLowerCase();
    if (!q) return sendingDomains;
    return sendingDomains.filter(d => {
      if (d.domain === selectedDomain) return true;
      const haystack = [
        d.domain,
        d.pool_name || '',
        ...(d.profiles || []).flatMap(p => [p.name || '', p.transport || '', p.from_name || '']),
      ].join(' ').toLowerCase();
      return haystack.includes(q);
    });
  }, [sendingDomains, domainSearch, selectedDomain]);

  // Property code parsed out of a segment name (see foreignSegments below).
  const segmentPropertyCode = (name: string): string => {
    const slot = /^SLOT-[A-Z0-9]+-([A-Z0-9]{2,4})-/.exec(name);
    if (slot) return slot[1];
    const kumo = /^KUMO-ALLTIME-([A-Z0-9]{2,4})-/.exec(name);
    if (kumo) return kumo[1];
    const eng = /^([A-Z]{2,4})\s+\d+D\s+(?:Clickers|Openers)\b/.exec(name);
    if (eng) return eng[1];
    return '';
  };

  // One property answers to MORE THAN ONE code in segment names — verified
  // against all 78,123 active prod segments 2026-08-18: BW/BWP, YI/YIH, HW/HWS,
  // MP/MPF, TR/TRB, PD/PMD, RR/RRU (prefix pairs) and TT/TOT (thingoftheday,
  // the one non-prefix pair). Comparing codes literally would flag a property's
  // own segments as foreign, so compatibility is prefix-or-alias.
  const codesCompatible = (a: string, b: string): boolean => {
    if (!a || !b || a === b) return true;
    if (a.startsWith(b) || b.startsWith(a)) return true;
    const alias: Record<string, string> = { TT: 'TOT', TOT: 'TT' };
    return alias[a] === b;
  };

  // Every code this property answers to, read off its own engagement grid.
  const propertyCodes = useMemo(() => {
    const codes = new Set<string>();
    [
      ...(engagementTiers?.clickers || []),
      ...(engagementTiers?.openers || []),
      ...(engagementTiers?.other || []),
    ].forEach(t => {
      const code = segmentPropertyCode(t.name);
      if (code) codes.add(code);
    });
    return codes;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [engagementTiers]);
  const propertyCode = propertyCodes.size > 0 ? Array.from(propertyCodes)[0] : '';

  // ── Cross-property audience guard (2026-08-18) ───────────────────────────
  // Segment names carry the property they were built for:
  //   "DB 30D Openers"                    → DB
  //   "SLOT-MICROSOFT-YI-C_OPEN_REAL_21D" → YI
  //   "KUMO-ALLTIME-BCC-ENG"              → BCC
  // The pinned property's own code is read off its engagement grid — the only
  // property-authoritative list the wizard already holds — so this needs no new
  // endpoint and no hardcoded brand map. A name whose shape we do not recognise
  // is left alone: the gate fires only when a name states a DIFFERENT property.
  // Brands are SEPARATE senders (CLAUDE.md §7), so mailing another property's
  // cohort from this sending domain is never intentional.
  const foreignSegments = useMemo(() => {
    if (propertyCodes.size === 0) return [];
    const picked = new Set<string>([
      ...selectedSegments,
      ...selectedExclusionSegments,
      ...sendPriority.filter(p => p.type === 'segment').map(p => p.id),
    ]);
    return segments
      .filter(seg => picked.has(seg.id))
      .map(seg => ({ id: seg.id, name: seg.name, code: segmentPropertyCode(seg.name) }))
      .filter(x => x.code !== ''
        && !Array.from(propertyCodes).some(own => codesCompatible(x.code, own)));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [segments, selectedSegments, selectedExclusionSegments, sendPriority, propertyCodes]);

  // The engagement-range selections, in the exact order buildCampaignPayload
  // puts them into send_priority: clickers, then openers, then the all-time
  // pools. This order is DOCTRINE (click = gold, open = silver) and is not
  // operator-reorderable, which is why these rows render locked.
  const engagementPriorityRows = useMemo(() => {
    if (!engagementTiers) return [];
    const kinds: { ids: string[]; tiers: EngagementTier[]; tierLabel: string; color: string }[] = [
      { ids: selectedClickerIds, tiers: engagementTiers.clickers, tierLabel: 'clickers', color: '#f59e0b' },
      { ids: selectedOpenerIds, tiers: engagementTiers.openers, tierLabel: 'openers', color: '#94a3b8' },
      { ids: selectedOtherIds, tiers: engagementTiers.other, tierLabel: 'all-time', color: '#38bdf8' },
    ];
    const rows: { id: string; label: string; tierLabel: string; color: string; count: number }[] = [];
    for (const k of kinds) {
      for (const id of k.ids) {
        const t = k.tiers.find(x => x.segment_id === id);
        rows.push({
          id,
          label: t ? t.name : `Segment ${id.slice(0, 8)}…`,
          tierLabel: k.tierLabel,
          color: k.color,
          count: t ? t.count : 0,
        });
      }
    }
    return rows;
  }, [engagementTiers, selectedClickerIds, selectedOpenerIds, selectedOtherIds]);

  const selectedProfile = sendingDomains
    .find(d => d.domain === selectedDomain)?.profiles
    ?.find(p => p.id === selectedProfileId);
  const isKumoRoute = selectedProfile?.transport === 'kumo';
  const kumoIllegalISPs = isKumoRoute
    ? selectedISPs.filter(i => !KUMO_ALLOWED_ISPS.includes(i))
    : [];

  // ── Warm-up branch: derived ──────────────────────────────────────────────

  const warmupDomainRow = useMemo(
    () => warmupDomains.find(d => d.domain === selectedDomain),
    [warmupDomains, selectedDomain],
  );
  // The eligible set is the SERVER's (mailing_sending_profiles.routing_mode =
  // 'kumo'); the 11 are never hardcoded here.
  const isWarmupDomain = !!selectedDomain && !!warmupDomainRow;

  // ── THE MODE DISCRIMINANT ────────────────────────────────────────────────
  // `activeMode` is the only thing any branch below is allowed to read.
  // 'warmup' collapses to 'offers' when the pinned domain is not a warm-up
  // property, so an impossible (mode, domain) pair can never reach a renderer.
  // 'newsletter' is ESTATE-WIDE and deliberately not domain-gated.
  const activeMode: CampaignMode =
    campaignMode === 'warmup' ? (isWarmupDomain ? 'warmup' : 'offers') : campaignMode;
  const warmupActive = activeMode === 'warmup';
  const newsletterActive = activeMode === 'newsletter';
  const warmupBrandSlug = useMemo(
    () => (isWarmupDomain ? deriveBrandSlug(warmupDomainRow, selectedDomain) : ''),
    [isWarmupDomain, warmupDomainRow, selectedDomain],
  );
  // Was the slug stated by the estate endpoint, or guessed from the domain?
  const warmupSlugIsDerived = isWarmupDomain && !warmupDomainRow?.brand_slug && !warmupDomainRow?.brand_code;

  // Offer-flow work the operator has already done. Toggling never discards it —
  // it stays in state and comes back intact — but the operator is told.
  const offerStateEntered = !!selectedProofId || !!selectedOfferId
    || !!(variants[0]?.subject || '').trim() || !!(variants[0]?.html_content || '').trim();

  const warmupFreshness = useMemo(
    () => fmtFreshness(warmupCreative?.updated_at),
    [warmupCreative],
  );
  const warmupCreativeStale =
    warmupFreshness.ageMs !== null && warmupFreshness.ageMs > WARMUP_STALE_MS;

  const coldQuotaNum = useMemo(() => {
    const t = coldQuota.trim();
    if (t === '') return null;
    const n = Number(t);
    return Number.isFinite(n) && n >= 0 ? Math.floor(n) : null;
  }, [coldQuota]);

  // Engaged side of the mix. Counts that came back 0 are UNKNOWN (a refresh
  // timeout zeroes subscriber_count), so they are counted separately and never
  // folded into the total as zeros.
  const warmupEngagedMix = useMemo(() => {
    const picked = warmupSegments.filter(sg => warmupSelectedSegmentIds.includes(sg.id));
    let known = 0;
    let unknownSegments = 0;
    for (const sg of picked) {
      const c = warmupSegmentCount(sg);
      if (c.known) known += c.value; else unknownSegments += 1;
    }
    return { selected: picked.length, known, unknownSegments };
  }, [warmupSegments, warmupSelectedSegmentIds]);

  // ── Newsletters branch: derived ──────────────────────────────────────────

  // One pass that resolves, per domain: readiness, why-not, and whether it is
  // actually going to be requested. `missing` is BLOCKED (never selectable);
  // `stale` is excluded by default and needs an explicit opt-in.
  const newsletterView = useMemo(() => (newsletterRows || []).map(r => {
    const { status, stated } = newsletterStatusOf(r);
    const reason = newsletterReasonOf(r, status);
    const blocked = status === 'missing';
    const excluded = newsletterExcluded.has(r.sending_domain);
    const staleOptIn = newsletterStaleOptIn.has(r.sending_domain);
    const included = !blocked && !excluded && (status !== 'stale' || staleOptIn);
    return { row: r, status, statusStated: stated, reason, blocked, excluded, staleOptIn, included };
  }), [newsletterRows, newsletterExcluded, newsletterStaleOptIn]);

  const newsletterIncluded = useMemo(
    () => newsletterView.filter(v => v.included),
    [newsletterView],
  );

  const newsletterTally = useMemo(() => ({
    total:    newsletterView.length,
    ready:    newsletterView.filter(v => v.status === 'ready').length,
    stale:    newsletterView.filter(v => v.status === 'stale').length,
    missing:  newsletterView.filter(v => v.status === 'missing').length,
    included: newsletterIncluded.length,
    derived:  newsletterView.filter(v => !v.statusStated).length,
  }), [newsletterView, newsletterIncluded]);

  // Anchor segments per INCLUDED domain, filtered by the one posture. The
  // unfiltered kind breakdown is kept so posture filtering is visible rather
  // than silently dropping segments the name classifier did not recognise.
  const newsletterAudience = useMemo(() => {
    const out: Record<string, {
      picked: WarmupSegment[]; total: number;
      kinds: { clickers: number; openers: number; other: number };
    }> = {};
    for (const v of newsletterIncluded) {
      const segs = newsletterAnchors[v.row.sending_domain] || [];
      const kinds = { clickers: 0, openers: 0, other: 0 };
      segs.forEach(sg => { kinds[newsletterSegmentKind(sg.name)] += 1; });
      const picked = newsletterPosture === 'all'
        ? segs
        : segs.filter(sg => newsletterSegmentKind(sg.name) === newsletterPosture);
      out[v.row.sending_domain] = { picked, total: segs.length, kinds };
    }
    return out;
  }, [newsletterIncluded, newsletterAnchors, newsletterPosture]);

  // Included domains that resolve NO segment under the chosen posture. These
  // would queue a request with an empty audience — surfaced, never shipped
  // quietly.
  const newsletterAudienceGaps = useMemo(
    () => newsletterIncluded
      .map(v => v.row.sending_domain)
      .filter(d => (newsletterAudience[d]?.picked.length || 0) === 0),
    [newsletterIncluded, newsletterAudience],
  );

  const denverDay = (d: Date) => d.toLocaleDateString('en-CA', { timeZone: SEND_DAY_TIMEZONE });

  // One scheduled instant for the request, resolved from whichever schedule
  // control the operator actually used (quick field, or the earliest per-ISP
  // plan start). Returns '' when nothing is set — never "now".
  const intentScheduledAtISO = useMemo(() => {
    if (scheduleMode === 'quick' || !Object.keys(ispPlansByKey).length) {
      return scheduledAt ? new Date(scheduledAt).toISOString() : '';
    }
    const starts: number[] = [];
    if (scheduledAt) starts.push(new Date(scheduledAt).getTime());
    for (const isp of selectedISPs) {
      const plan = ispPlansByKey[isp];
      if (!plan?.useCustomSchedule) continue;
      if (plan.startTime) starts.push(new Date(plan.startTime).getTime());
      (plan.timeSpans || []).forEach(sp => { if (sp.startAt) starts.push(new Date(sp.startAt).getTime()); });
    }
    const valid = starts.filter(t => Number.isFinite(t));
    return valid.length ? new Date(Math.min(...valid)).toISOString() : '';
  }, [scheduleMode, scheduledAt, ispPlansByKey, selectedISPs]);

  // Same steps, same ids, same order in every mode — modes only relabel.
  const activeSteps = useMemo(() => {
    const ov = MODE_STEP_OVERRIDES[activeMode];
    return STEPS.map(st => (ov[st.id] ? { ...st, ...ov[st.id] } : st));
  }, [activeMode]);

  // Navigation is LIST-AWARE, not arithmetic. Step ids are sparse (5 is
  // retired), so ±1 would land on an id that renders nothing. These walk
  // activeSteps, and `nextStepId === null` on the last entry is what hides the
  // Next button — never a hardcoded `step < 6`.
  const stepIndex = activeSteps.findIndex(st => st.id === step);
  const prevStepId = stepIndex > 0 ? activeSteps[stepIndex - 1].id : null;
  const nextStepId = stepIndex >= 0 && stepIndex < activeSteps.length - 1
    ? activeSteps[stepIndex + 1].id
    : null;

  const warmupRequestsDate = useMemo(
    () => denverDay(intentScheduledAtISO ? new Date(intentScheduledAtISO) : new Date()),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [intentScheduledAtISO],
  );

  // ── Warm-up branch: fetches ──────────────────────────────────────────────

  // The eligible-domain list. Loaded once per wizard (and on Retry) — it is
  // what decides whether the toggle is even offered.
  useEffect(() => {
    let cancelled = false;
    setWarmupDomainsLoading(true);
    setWarmupDomainsError('');
    apiFetch(`${API_BASE}/pmta-campaign/warmup/domains`)
      .then(async res => {
        const data = await res.json().catch(() => ({}));
        if (cancelled) return;
        if (!res.ok) {
          setWarmupDomainsError(data?.error || `HTTP ${res.status}`);
          setWarmupDomains([]);
          return;
        }
        // ⚠️ The wire field is `sending_domain`. Filtering on `d.domain`
        // dropped every row, so warmupDomains was always [] and the toggle
        // never rendered — the whole branch was dead in the UI. Normalize
        // ONCE here; everything downstream reads the local `domain` alias.
        const rows: WarmupDomainRow[] = (data?.domains || data || [])
          .map((d: any) => {
            if (typeof d === 'string') return { domain: d } as WarmupDomainRow;
            if (!d) return null;
            const domain = d.sending_domain ?? d.domain;
            return domain ? ({ ...d, domain } as WarmupDomainRow) : null;
          })
          .filter((d: WarmupDomainRow | null): d is WarmupDomainRow => !!d);
        setWarmupDomains(rows);
      })
      .catch(err => { if (!cancelled) { setWarmupDomainsError(err?.message || 'network error'); setWarmupDomains([]); } })
      .finally(() => { if (!cancelled) setWarmupDomainsLoading(false); });
    return () => { cancelled = true; };
  }, [warmupDomainsKey]);

  // Default ON for a warm-up property, OFF for everything else — applied once
  // per domain so an explicit toggle-off survives.
  //
  // The stamp is taken ONLY once eligibility is actually known (isWarmupDomain
  // true). Stamping on the not-yet-loaded state would mark the domain
  // "defaulted" while the estate list was still in flight, and the default
  // would then never apply when it arrived.
  useEffect(() => {
    // NEWSLETTERS is an ESTATE-WIDE mode. Selecting (or clearing) a single
    // sending domain must never knock the operator out of it — that is the
    // one thing a naive port of the old boolean default would do.
    if (campaignMode === 'newsletter') return;
    setNewsletterResults(null);
    if (!selectedDomain || !isWarmupDomain) {
      setCampaignMode(m => (m === 'warmup' ? 'offers' : m));
      setWarmupDefaultAppliedFor('');
      return;
    }
    if (warmupDefaultAppliedFor !== selectedDomain) {
      setCampaignMode('warmup');
      setWarmupDefaultAppliedFor(selectedDomain);
    }
  }, [campaignMode, selectedDomain, isWarmupDomain, warmupDefaultAppliedFor]);

  // Property changed ⇒ its newsletter, its anchors and its cold pad are all
  // property-scoped. Clear them rather than carrying another brand's picks
  // across (the campaign-manager carry-over incident, 2026-08-18).
  useEffect(() => {
    setWarmupCreative(null);
    setWarmupCreativeError('');
    setWarmupSubject('');
    setWarmupPreheader('');
    setWarmupCopyTouched(false);
    setWarmupPreviewHtml('');
    setWarmupPreviewError('');
    setWarmupSegments([]);
    setWarmupSegmentsError('');
    setWarmupSelectedSegmentIds([]);
    setColdSource('');
    setColdQuota('');
    setWarmupResult(null);
  }, [selectedDomain]);

  // The daily-registered newsletter for this property.
  useEffect(() => {
    if (!warmupActive || !warmupBrandSlug) return;
    let cancelled = false;
    setWarmupCreativeLoading(true);
    setWarmupCreativeError('');
    apiFetch(`${API_BASE}/pmta-campaign/warmup/creative?brand_slug=${encodeURIComponent(warmupBrandSlug)}`)
      .then(async res => {
        const data = await res.json().catch(() => ({}));
        if (cancelled) return;
        if (!res.ok) {
          setWarmupCreativeError(data?.error || `HTTP ${res.status}`);
          setWarmupCreative(null);
          return;
        }
        // ⚠️ The wire fields are `creative_id` and `html_length`. Reading
        // `id`/`html_bytes` produced a null creative for a brand whose
        // creative exists, and step 3 then said "no newsletter is registered".
        const raw: any = data?.creative || data || null;
        const c: WarmupCreative | null = raw && (raw.creative_id || raw.id)
          ? {
              ...raw,
              id: raw.creative_id ?? raw.id,
              html_bytes: raw.html_length ?? raw.html_bytes,
            }
          : null;
        setWarmupCreative(c);
        // Prefill the overrides from the creative's own copy, but only while
        // the operator has not edited them — a refetch must never silently
        // overwrite what they typed.
        if (c && !warmupCopyTouched) {
          setWarmupSubject(c.subject || '');
          setWarmupPreheader(c.preheader || '');
        }
      })
      .catch(err => { if (!cancelled) { setWarmupCreativeError(err?.message || 'network error'); setWarmupCreative(null); } })
      .finally(() => { if (!cancelled) setWarmupCreativeLoading(false); });
    return () => { cancelled = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [warmupActive, warmupBrandSlug, warmupCreativeKey]);

  // The property's engaged-anchor segments.
  useEffect(() => {
    if (!warmupActive || !warmupBrandSlug) return;
    let cancelled = false;
    setWarmupSegmentsLoading(true);
    setWarmupSegmentsError('');
    apiFetch(`${API_BASE}/pmta-campaign/warmup/segments?brand_slug=${encodeURIComponent(warmupBrandSlug)}`)
      .then(async res => {
        const data = await res.json().catch(() => ({}));
        if (cancelled) return;
        if (!res.ok) {
          setWarmupSegmentsError(data?.error || `HTTP ${res.status}`);
          setWarmupSegments([]);
          return;
        }
        // ⚠️ The wire field is `segment_id`; reading `id` made every
        // selection an `undefined`.
        const segRows: any[] = Array.isArray(data) ? data : (data?.segments || []);
        setWarmupSegments(segRows
          .map(sg => ({ ...sg, id: sg.segment_id ?? sg.id }))
          .filter((sg: WarmupSegment) => !!sg.id));
      })
      .catch(err => { if (!cancelled) { setWarmupSegmentsError(err?.message || 'network error'); setWarmupSegments([]); } })
      .finally(() => { if (!cancelled) setWarmupSegmentsLoading(false); });
    return () => { cancelled = true; };
  }, [warmupActive, warmupBrandSlug, warmupSegmentsKey]);

  // The build-request ledger for the scheduled day. Polled while any request is
  // still non-terminal — the builder takes ~40 minutes.
  const fetchWarmupRequests = useCallback(async (signal?: AbortSignal) => {
    setWarmupRequestsLoading(true);
    setWarmupRequestsError('');
    try {
      const res = await apiFetch(
        `${API_BASE}/pmta-campaign/warmup/requests?date=${encodeURIComponent(warmupRequestsDate)}`,
        signal ? { signal } : undefined,
      );
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        setWarmupRequestsError(data?.error || `HTTP ${res.status}`);
        setWarmupRequests(null);
        return;
      }
      setWarmupRequests(Array.isArray(data) ? data : (data?.requests || []));
      setWarmupRequestsFetchedAt(new Date().toLocaleTimeString());
    } catch (err: any) {
      if (err?.name === 'AbortError') return;
      setWarmupRequestsError(err?.message || 'network error');
      setWarmupRequests(null);
    } finally {
      setWarmupRequestsLoading(false);
    }
  }, [warmupRequestsDate]);

  useEffect(() => {
    if (!(warmupActive || newsletterActive) || step !== 6) return;
    const ctrl = new AbortController();
    fetchWarmupRequests(ctrl.signal);
    return () => ctrl.abort();
  }, [warmupActive, newsletterActive, step, warmupRequestsKey, fetchWarmupRequests]);

  useEffect(() => {
    if (!(warmupActive || newsletterActive) || step !== 6) return;
    const pending = (warmupRequests || []).some(
      r => (r.status || '').toLowerCase() === 'requested' || (r.status || '').toLowerCase() === 'building',
    );
    if (!pending) return;
    const t = setInterval(() => { fetchWarmupRequests(); }, 30000);
    return () => clearInterval(t);
  }, [warmupActive, newsletterActive, step, warmupRequests, fetchWarmupRequests]);

  // ── Newsletters branch: fetches ──────────────────────────────────────────

  // The estate roster. include_html=0 — bodies are pulled one domain at a time
  // from the Preview button, never 27 at once.
  useEffect(() => {
    if (!newsletterActive) return;
    let cancelled = false;
    setNewsletterLoading(true);
    setNewsletterError('');
    apiFetch(`${API_BASE}/pmta-campaign/newsletter/preview?include_html=0`)
      .then(async res => {
        const data = await res.json().catch(() => ({}));
        if (cancelled) return;
        if (!res.ok) {
          setNewsletterError(data?.error || `HTTP ${res.status}`);
          // null, not [] — a failed fetch is UNKNOWN, never "no domains".
          setNewsletterRows(null);
          return;
        }
        const rows: NewsletterDomainRow[] = (data?.domains || [])
          .filter((r: any) => r && r.sending_domain);
        setNewsletterRows(rows);
        setNewsletterDay(data?.day || '');
        setNewsletterFetchedAt(new Date().toLocaleTimeString());
      })
      .catch(err => {
        if (cancelled) return;
        setNewsletterError(err?.message || 'network error');
        setNewsletterRows(null);
      })
      .finally(() => { if (!cancelled) setNewsletterLoading(false); });
    return () => { cancelled = true; };
  }, [newsletterActive, newsletterKey]);

  // Per-domain engaged anchors, so the audience step is REAL for N domains
  // rather than a posture with nothing behind it. Bounded fan-out: one request
  // per INCLUDED domain, only on the audience step.
  const newsletterIncludedKey = useMemo(
    () => newsletterIncluded.map(v => v.row.sending_domain).sort().join(','),
    [newsletterIncluded],
  );

  useEffect(() => {
    if (!newsletterActive || step !== 4) return;
    const domains = newsletterIncludedKey ? newsletterIncludedKey.split(',') : [];
    if (domains.length === 0) { setNewsletterAnchors({}); setNewsletterAnchorErrors({}); return; }
    let cancelled = false;
    setNewsletterAnchorsLoading(true);
    const slugFor = (d: string) => {
      const row = (newsletterRows || []).find(r => r.sending_domain === d);
      return (row?.brand_slug || '').trim() || deriveBrandSlug(undefined, d);
    };
    Promise.all(domains.map(async d => {
      try {
        const res = await apiFetch(
          `${API_BASE}/pmta-campaign/warmup/segments?brand_slug=${encodeURIComponent(slugFor(d))}`);
        const data = await res.json().catch(() => ({}));
        if (!res.ok) return { d, err: data?.error || `HTTP ${res.status}`, segs: [] as WarmupSegment[] };
        const rows: any[] = Array.isArray(data) ? data : (data?.segments || []);
        return {
          d, err: '',
          segs: rows.map(sg => ({ ...sg, id: sg.segment_id ?? sg.id }))
                    .filter((sg: WarmupSegment) => !!sg.id) as WarmupSegment[],
        };
      } catch (err: any) {
        return { d, err: err?.message || 'network error', segs: [] as WarmupSegment[] };
      }
    })).then(results => {
      if (cancelled) return;
      const segs: Record<string, WarmupSegment[]> = {};
      const errs: Record<string, string> = {};
      results.forEach(r => { segs[r.d] = r.segs; if (r.err) errs[r.d] = r.err; });
      setNewsletterAnchors(segs);
      setNewsletterAnchorErrors(errs);
    }).finally(() => { if (!cancelled) setNewsletterAnchorsLoading(false); });
    return () => { cancelled = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [newsletterActive, step, newsletterIncludedKey, newsletterAnchorsKey]);

  // One domain's body, on demand.
  const openNewsletterPreview = useCallback(async (domain: string) => {
    setNewsletterPreviewDomain(domain);
    setNewsletterPreviewError('');
    setNewsletterPreviewHtml('');
    setNewsletterPreviewLoading(true);
    try {
      const res = await apiFetch(
        `${API_BASE}/pmta-campaign/newsletter/preview?include_html=1&sending_domain=${encodeURIComponent(domain)}`);
      const data = await res.json().catch(() => ({}));
      if (!res.ok) { setNewsletterPreviewError(data?.error || `HTTP ${res.status}`); return; }
      const row: NewsletterDomainRow | undefined = (data?.domains || [])
        .find((r: NewsletterDomainRow) => r?.sending_domain === domain) || (data?.domains || [])[0];
      setNewsletterPreviewHtml(row?.html || '');
    } catch (err: any) {
      setNewsletterPreviewError(err?.message || 'network error');
    } finally {
      setNewsletterPreviewLoading(false);
    }
  }, []);

  const toggleEngagementTier = useCallback((kind: 'clickers' | 'openers' | 'other', segmentId: string) => {
    const apply = (prev: string[]) =>
      prev.includes(segmentId) ? prev.filter(x => x !== segmentId) : [...prev, segmentId];
    if (kind === 'clickers') setSelectedClickerIds(apply);
    else if (kind === 'openers') setSelectedOpenerIds(apply);
    else setSelectedOtherIds(apply);
  }, []);

  const fetchAudienceData = useCallback(async () => {
    setAudienceError('');
    setAudienceDataLoading(true);
    try {
      const [listRes, segRes, suppRes] = await Promise.all([
        fetchWithRetry(`${API_BASE}/lists`),
        // include_archived=true so the wizard can locally toggle showing
        // archived items; default UI hides them via showArchived state.
        fetchWithRetry(`${API_BASE}/segments?include_archived=true`),
        fetchWithRetry(`${API_BASE}/suppression-lists`),
      ]);
      if (!listRes.ok || !segRes.ok || !suppRes.ok) {
        setAudienceError('Some audience data failed to load. Retrying didn\'t help — check configuration.');
      }
      const listData = await listRes.json();
      const segData = await segRes.json();
      const suppData = await suppRes.json();
      setLists(Array.isArray(listData) ? listData : listData.lists || []);
      setSegments(Array.isArray(segData) ? segData : segData.segments || []);
      const parsedSupp = Array.isArray(suppData) ? suppData : suppData.lists || [];
      setSuppressionLists(parsedSupp);
      // Auto-select global suppression list if present
      const globalList = parsedSupp.find((sl: any) => sl.id === 'global-suppression-list');
      if (globalList && !selectedSuppLists.includes(globalList.id)) {
        setSelectedSuppLists(prev => prev.includes(globalList.id) ? prev : [...prev, globalList.id]);
      }
    } catch {
      setAudienceError('Failed to load audience data — network error. Click retry.');
    }
    setAudienceDataLoading(false);
  }, [fetchWithRetry]);

  const fetchAudienceEstimate = useCallback(async () => {
    if (selectedLists.length === 0 && selectedSegments.length === 0) {
      setAudienceEstimate(null);
      return;
    }
    setEstimating(true);
    try {
      const res = await fetchWithRetry(`${API_BASE}/pmta-campaign/estimate-audience`, {
        method: 'POST',
        body: JSON.stringify({
          list_ids: selectedLists,
          segment_ids: selectedSegments,
          suppression_list_ids: selectedSuppLists,
          exclusion_segment_ids: selectedExclusionSegments,
          target_isps: selectedISPs,
        }),
      });
      const data = await res.json();
      setAudienceEstimate(data);
    } catch (err) {
      console.warn('[Wizard] audience estimate failed:', err);
    }
    setEstimating(false);
  }, [fetchWithRetry, selectedLists, selectedSegments, selectedSuppLists, selectedExclusionSegments, selectedISPs]);

  // Load data on step entry
  useEffect(() => {
    if (step === 1) fetchDomains();
    if (step === 2) { fetchReadiness(); fetchInsights(insightDomainFilter || undefined); }
    // Offer-flow data, for the OFFER flow only. Un-gated these fired on every
    // mode and any failure painted an audienceError banner on a step that does
    // not read that data.
    if (step === 3 && activeMode === 'offers') fetchOffers();
    if (step === 4 && activeMode === 'offers') fetchAudienceData();
  }, [step, activeMode, fetchReadiness, fetchInsights, fetchDomains, fetchOffers, fetchAudienceData, insightDomainFilter]);

  // Re-estimate audience when selections change
  useEffect(() => {
    if (step === 4 && activeMode === 'offers') fetchAudienceEstimate();
  }, [step, activeMode, selectedLists, selectedSegments, selectedSuppLists, selectedExclusionSegments, fetchAudienceEstimate]);

  // Fetch send-time recommendations when user switches to scheduled mode
  useEffect(() => {
    if (sendMode !== 'scheduled' || recsLoaded || selectedISPs.length === 0) return;
    let cancelled = false;
    setRecsLoading(true);
    orgFetch(`${API_BASE}/pmta-campaign/send-time-recommendations?isps=${selectedISPs.join(',')}`, orgId)
      .then(r => r.json())
      .then(data => {
        if (!cancelled) {
          setRecommendations(data.recommendations || []);
          setRecsLoaded(true);
        }
      })
      .catch(() => { if (!cancelled) setRecsLoaded(true); })
      .finally(() => { if (!cancelled) setRecsLoading(false); });
    return () => { cancelled = true; };
  }, [sendMode, recsLoaded, selectedISPs, orgId]);

  // ── Step validation ──────────────────────────────────────────────────────

  // ── Mode-dispatched validators ───────────────────────────────────────────
  //
  // Steps 3, 4 and 6 used to fork on a BOOLEAN, so a third mode silently ran
  // the OFFER validators — "Select an approved creative from the Creative
  // Studio offers library" would have blocked a newsletters send forever.
  // Each mode now owns a function and the dispatcher at the bottom is
  // exhaustive over CampaignMode.

  const offerStepErrors = (s: number): string[] => {
    const errors: string[] = [];
    switch (s) {
      case 1:
        if (!selectedDomain) errors.push('Select a sending domain');
        break;
      case 3:
        if (!selectedProofId) errors.push('Select an approved creative from the Creative Studio offers library');
        variants.forEach(v => {
          if (!v.from_name.trim()) errors.push('Select an approved from-name');
          if (!v.subject.trim()) errors.push('Select an approved subject line');
          if (!v.html_content.trim()) errors.push('The selected creative has no HTML content');
        });
        break;
      case 4:
        if (selectedClickerIds.length === 0 && selectedOpenerIds.length === 0
            && selectedOtherIds.length === 0
            && selectedLists.length === 0 && selectedSegments.length === 0) {
          errors.push('Select an engagement range, or a list/segment in the advanced picker');
        }
        if (kumoIllegalISPs.length > 0) {
          errors.push(
            `KumoMTA warm-up is yahoo-family only — remove ${kumoIllegalISPs.join(', ')} ` +
            `(allowed: ${KUMO_ALLOWED_ISPS.join(', ')})`);
        }
        // Belt for the client side of the coercion the server also enforces.
        if (audienceBound && masterTopUp) {
          errors.push('Master-list top-up cannot be combined with an uncapped (audience-bound) send');
        }
        // Fail-closed on another property's cohorts (2026-08-18 incident).
        if (foreignSegments.length > 0) {
          errors.push(
            `These segments belong to another property (${foreignSegments.map(f => f.code).filter((c, i, a) => a.indexOf(c) === i).join(', ')}), ` +
            `not ${propertyCode || selectedDomain}: ${foreignSegments.map(f => f.name).join(', ')}. ` +
            'Remove them — brands are separate senders.');
        }
        // A segment that is both a send-priority source AND excluded contributes
        // ZERO: the planner denies excluded emails inside qualifyEmail. Surface
        // it rather than letting the plan quietly come back short.
        {
          const excludedSet = new Set(selectedExclusionSegments);
          const contradictions = [...selectedSegments, ...sendPriority.filter(p => p.type === 'segment').map(p => p.id)]
            .filter(id => excludedSet.has(id));
          if (contradictions.length > 0) {
            const names = Array.from(new Set(contradictions))
              .map(id => segments.find(sg => sg.id === id)?.name || id.slice(0, 8))
              .join(', ');
            errors.push(`${names} is in BOTH the send audience and the exclusion list — it would mail 0 recipients. Remove it from one side.`);
          }
        }
        break;
      case 6:
        if (!campaignName.trim()) errors.push('Campaign name is required');
        if (sendMode === 'scheduled' && scheduleMode === 'quick') {
          if (!scheduledAt) {
            errors.push('Scheduled date and time is required');
          } else if (new Date(scheduledAt).getTime() <= Date.now()) {
            errors.push('Scheduled date and time must be in the future');
          }
        }
        if (sendMode === 'scheduled' && scheduleMode === 'per-isp') {
          const now = Date.now();
          selectedISPs.forEach(isp => {
            const plan = ispPlansByKey[isp];
            const label = ISP_META[isp]?.label || isp;
            if (!plan?.useCustomSchedule) return;
            const hasStartTime = plan.startTime && plan.startTime.trim() !== '';
            const validSpans = (plan.timeSpans || []).filter(span => span.startAt && span.endAt);
            if (!hasStartTime && validSpans.length === 0) {
              errors.push(`${label}: set a start time or add a time span`);
            } else if (hasStartTime) {
              if (new Date(plan.startTime).getTime() <= now) {
                errors.push(`${label}: start time must be in the future`);
              }
            } else if (validSpans.length > 0) {
              validSpans.forEach((span, idx) => {
                if (new Date(span.startAt).getTime() <= now) {
                  errors.push(`${label}: time span ${idx + 1} start must be in the future`);
                }
                if (new Date(span.endAt).getTime() <= new Date(span.startAt).getTime()) {
                  errors.push(`${label}: time span ${idx + 1} end must be after start`);
                }
              });
            }
          });
        }
        break;
    }
    return errors;
  };

  const warmupStepErrors = (s: number): string[] => {
    const errors: string[] = [];
    switch (s) {
      case 1:
        if (!selectedDomain) errors.push('Select a sending domain');
        break;
      case 3:
        // Warm-up content is the daily-registered newsletter. No offer, no
        // proof — offers are BANNED in warm-up content.
        if (warmupCreativeError) errors.push(`Newsletter could not be loaded (${warmupCreativeError}) — retry before scheduling`);
        else if (!warmupCreativeLoading && !warmupCreative) errors.push('No newsletter is registered for this property today — register one in Creative Studio first');
        if (warmupCreative && !warmupSubject.trim()) errors.push('Subject line is required');
        if (warmupCreative && !warmupPreheader.trim()) errors.push('Preheader is required');
        break;
      case 4: {
        const cold = coldQuotaNum ?? 0;
        if (warmupSelectedSegmentIds.length === 0 && cold <= 0) {
          errors.push('Select at least one engaged-anchor segment, or set a cold quota above 0');
        }
        if (coldQuota.trim() !== '' && coldQuotaNum === null) {
          errors.push('Cold quota must be a whole number of records (0 or more)');
        }
        if (cold > 0 && !coldSource.trim()) {
          errors.push('Choose the cold source the builder should pull those records from');
        }
        if (coldSource.trim() && cold <= 0) {
          errors.push('A cold source is selected but the cold quota is 0 — set a quota or clear the source');
        }
        if (kumoIllegalISPs.length > 0) {
          errors.push(
            `KumoMTA warm-up is yahoo-family only — remove ${kumoIllegalISPs.join(', ')} ` +
            `(allowed: ${KUMO_ALLOWED_ISPS.join(', ')})`);
        }
        break;
      }
      case 6:
        // The warm-up request carries no campaign name — the builder names
        // what it builds. It DOES need one resolvable scheduled instant.
        if (!intentScheduledAtISO) {
          errors.push('Set the send date and time — the build request needs one scheduled instant');
        } else if (new Date(intentScheduledAtISO).getTime() <= Date.now()) {
          errors.push('Scheduled date and time must be in the future');
        }
        if (selectedISPs.length === 0) errors.push('Select at least one mailbox provider');
        break;
    }
    return errors;
  };

  // NEWSLETTERS. Content is automatic, so nothing here validates a creative
  // CHOICE — it validates that what the daily pipeline registered is actually
  // mailable, per domain, and that the operator made the audience decision.
  const newsletterStepErrors = (s: number): string[] => {
    const errors: string[] = [];
    const names = (rows: { row: NewsletterDomainRow }[]) =>
      rows.map(v => v.row.sending_domain).join(', ');
    switch (s) {
      case 1:
        if (newsletterError) {
          errors.push(`The newsletter roster could not be loaded (${newsletterError}) — retry before scheduling. This is NOT a statement that there are no newsletters today.`);
        } else if (newsletterLoading || newsletterRows === null) {
          errors.push('Still loading today’s newsletter roster');
        } else if (newsletterRows.length === 0) {
          errors.push('The roster returned no eligible sending domains for today — there is nothing to schedule');
        } else if (newsletterIncluded.length === 0) {
          errors.push('Every sending domain is excluded, missing a creative, or stale — include at least one');
        }
        break;
      case 3: {
        if (newsletterIncluded.length === 0) {
          errors.push('No sending domain is included — nothing would be queued');
          break;
        }
        const noCreative = newsletterIncluded.filter(v => !(v.row.creative_id || '').trim());
        if (noCreative.length) errors.push(`No creative is registered for ${names(noCreative)} — exclude ${noCreative.length === 1 ? 'it' : 'them'} or register the newsletter first`);
        const noSubject = newsletterIncluded.filter(v => !(v.row.subject || '').trim());
        if (noSubject.length) errors.push(`No subject line for ${names(noSubject)} — a send with an empty subject is not auditable`);
        const noPreheader = newsletterIncluded.filter(v => !(v.row.preheader || '').trim());
        if (noPreheader.length) errors.push(`No preheader for ${names(noPreheader)}`);
        const noFrom = newsletterIncluded.filter(v => !(v.row.from_name || '').trim());
        if (noFrom.length) errors.push(`No friendly-from name resolved for ${names(noFrom)} — the from-name comes from the domain’s sending profile, so this means the profile is missing one`);
        break;
      }
      case 4: {
        if (newsletterIncluded.length === 0) {
          errors.push('No sending domain is included — nothing would be queued');
          break;
        }
        if (newsletterAnchorsLoading) {
          errors.push('Still resolving each domain’s engaged anchors — wait for the audience table to finish');
          break;
        }
        const failed = Object.keys(newsletterAnchorErrors);
        if (failed.length) errors.push(`Engaged anchors could not be loaded for ${failed.join(', ')} — their audience is UNKNOWN, not empty. Retry, or exclude them.`);
        if (newsletterAudienceGaps.length) {
          const label = NEWSLETTER_POSTURES.find(x => x.id === newsletterPosture)?.label || newsletterPosture;
          errors.push(`${newsletterAudienceGaps.join(', ')} resolve no engaged-anchor segment under "${label}" — those requests would carry an EMPTY audience. Change the posture or exclude them.`);
        }
        break;
      }
      case 6:
        if (newsletterIncluded.length === 0) errors.push('No sending domain is included — nothing would be queued');
        if (!intentScheduledAtISO) {
          errors.push('Set the ONE send date and time that applies to every selected sending domain');
        } else if (new Date(intentScheduledAtISO).getTime() <= Date.now()) {
          errors.push('Scheduled date and time must be in the future');
        }
        if (selectedISPs.length === 0) errors.push('Select at least one mailbox provider');
        break;
    }
    return errors;
  };

  // THE dispatcher. `default: assertUnreachableMode(activeMode)` makes a
  // fourth CampaignMode a COMPILE error here rather than a silent fall-through
  // into the offer validators.
  const getStepErrors = (s: number): string[] => {
    // Step 2 is mode-neutral: every mode picks mailbox providers.
    if (s === 2) return selectedISPs.length === 0 ? ['Select at least one mailbox provider'] : [];
    switch (activeMode) {
      case 'offers':     return offerStepErrors(s);
      case 'warmup':     return warmupStepErrors(s);
      case 'newsletter': return newsletterStepErrors(s);
      default:           return assertUnreachableMode(activeMode);
    }
  };

  const canProceed = (): boolean => getStepErrors(step).length === 0;

  const showErr = (s: number) => !!stepAttempted[s];

  const fieldBorder = (isInvalid: boolean) =>
    isInvalid && showErr(step)
      ? '1px solid #ef4444'
      : '1px solid rgba(0,200,255,0.08)';

  const RequiredDot: React.FC = () => (
    <span style={{ color: '#ef4444', marginLeft: 2, fontSize: 10 }}>*</span>
  );

  const StepErrorBanner: React.FC<{ stepNum: number }> = ({ stepNum }) => {
    const errors = getStepErrors(stepNum);
    if (!showErr(stepNum) || errors.length === 0) return null;
    return (
      <div style={{
        background: 'rgba(239,68,68,0.08)', border: '1px solid rgba(239,68,68,0.2)',
        borderRadius: 8, padding: '10px 14px', marginBottom: 16,
        animation: 'igFadeSlide 0.3s ease both',
      }}>
        {errors.map((e, i) => (
          <div key={i} style={{ fontSize: 12, color: '#ef4444', padding: '2px 0', display: 'flex', alignItems: 'center', gap: 6 }}>
            <FontAwesomeIcon icon={faExclamationTriangle} style={{ fontSize: 10 }} /> {e}
          </div>
        ))}
      </div>
    );
  };

  const toDateTimeLocal = (date: Date) => {
    const pad = (n: number) => n.toString().padStart(2, '0');
    return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
  };

  // Offset (minutes) of `tz` at a given instant — DST-correct, no library.
  const tzOffsetMinutes = (date: Date, tz: string): number => {
    const parts = new Intl.DateTimeFormat('en-US', {
      timeZone: tz, hour12: false,
      year: 'numeric', month: '2-digit', day: '2-digit',
      hour: '2-digit', minute: '2-digit', second: '2-digit',
    }).formatToParts(date).reduce<Record<string, string>>((acc, p) => {
      if (p.type !== 'literal') acc[p.type] = p.value;
      return acc;
    }, {});
    const asUTC = Date.UTC(
      Number(parts.year), Number(parts.month) - 1, Number(parts.day),
      Number(parts.hour) % 24, Number(parts.minute), Number(parts.second),
    );
    return (asUTC - date.getTime()) / 60000;
  };

  // The instant of `HH:MM` on (today + dayOffset) in the send-day timezone.
  const sendDayInstant = (dayOffset: number, hh: number, mm: number): Date => {
    const today = new Intl.DateTimeFormat('en-CA', {
      timeZone: SEND_DAY_TIMEZONE, year: 'numeric', month: '2-digit', day: '2-digit',
    }).formatToParts(new Date()).reduce<Record<string, string>>((acc, p) => {
      if (p.type !== 'literal') acc[p.type] = p.value;
      return acc;
    }, {});
    const naive = Date.UTC(
      Number(today.year), Number(today.month) - 1, Number(today.day) + dayOffset, hh, mm, 0,
    );
    // Two passes settle the DST edge (the offset depends on the instant we are
    // still solving for).
    let guess = naive;
    for (let i = 0; i < 2; i++) {
      guess = naive - tzOffsetMinutes(new Date(guess), SEND_DAY_TIMEZONE) * 60000;
    }
    return new Date(guess);
  };

  /**
   * Apply a send-day anchor preset: start time, window, 15-minute cadence,
   * gentle throttle, Denver timezone — the shape the board compiles.
   *
   * "Today" is only chosen when the anchor is more than 15 minutes out.
   * normalizePMTACampaignInput silently downgrades anything inside 5 minutes to
   * an IMMEDIATE send, so a tighter margin would fire the campaign on the spot.
   */
  // Derive "MMDDYYYY - BRAND - OFFER" for the next send day, and default the
  // anchor to 01:01 in the send-day zone. Runs only while the name is untouched.
  const derivedCampaignName = useMemo(() => {
    const brand = brandPrefixForDomain(selectedDomain);
    const offerRow = offersCatalog.find(o => o.id === selectedOfferId);
    const token = offerTokenForName(offerRow?.name || '');
    if (!brand || !token) return '';
    return `${nameDateTokenForSchedule(scheduledAt, sendMode)} - ${brand} - ${token}`;
  }, [selectedDomain, selectedOfferId, offersCatalog, scheduledAt, sendMode]);

  useEffect(() => {
    if (nameTouched || !derivedCampaignName) return;
    setCampaignName(derivedCampaignName);
  }, [derivedCampaignName, nameTouched]);

  // Default the schedule to tomorrow 01:01 MT as soon as a domain is picked, so
  // a campaign is never built against today's already-passed anchor.
  //
  // SEEDS ONCE. `scheduledAt` alone is not a sufficient guard: switching to
  // "Send Now" CLEARS it (:6347), so an effect keyed only on emptiness re-fired
  // on the way back and silently replaced the anchor the operator had already
  // chosen — leaving payload.scheduled_at (tomorrow 01:01) contradicting the
  // per-ISP time_spans and the still-highlighted preset (today 12:01). Same
  // posture as nameTouched: a derived default is a convenience, never an
  // override of an explicit choice.
  useEffect(() => {
    if (!selectedDomain || scheduleSeeded || scheduledAt || sendMode === 'immediate') return;
    setScheduledAt(toDateTimeLocal(sendDayInstant(1, DEFAULT_ANCHOR_HOUR, DEFAULT_ANCHOR_MINUTE)));
    setCampaignTimezone(SEND_DAY_TIMEZONE);
    setScheduleSeeded(true);
  }, [selectedDomain, scheduleSeeded, scheduledAt, sendMode]);

  const applyAnchorPreset = (preset: AnchorPreset) => {
    const [hh, mm] = preset.localTime.split(':').map(Number);
    let when = sendDayInstant(0, hh, mm);
    if (when.getTime() <= Date.now() + 15 * 60 * 1000) {
      when = sendDayInstant(1, hh, mm);
    }
    const local = toDateTimeLocal(when);
    setActivePreset(preset.id);
    setScheduleSeeded(true);
    setSendMode('scheduled');
    setScheduleMode('per-isp');
    setScheduledAt(local);
    setCampaignTimezone(SEND_DAY_TIMEZONE);
    setGlobalScheduleStart(local);
    setGlobalScheduleDuration(preset.windowHours);
    setGlobalScheduleInterval(DEFAULT_WAVE_INTERVAL_MINUTES);
    setGlobalScheduleTimezone(SEND_DAY_TIMEZONE);
    setISPPlansByKey(prev => {
      const next: Record<string, ISPPlanFormState> = { ...prev };
      selectedISPs.forEach(isp => {
        const base = next[isp] || buildDefaultISPPlan(isp);
        next[isp] = {
          ...base,
          useCustomSchedule: true,
          cadenceMode: 'interval',
          everyMinutes: DEFAULT_WAVE_INTERVAL_MINUTES,
          durationHours: preset.windowHours,
          startTime: local,
          timezone: SEND_DAY_TIMEZONE,
          throttleStrategy,
          timeSpans: [],
        };
      });
      return next;
    });
  };

  const toLocalInputValue = (raw?: string) => {
    if (!raw) return '';
    const parsed = new Date(raw);
    return Number.isNaN(parsed.getTime()) ? '' : toDateTimeLocal(parsed);
  };

  const nextScheduleDefault = () => {
    const now = new Date();
    const mstOffset = -7 * 60;
    const localOffset = now.getTimezoneOffset();
    const mstNow = new Date(now.getTime() + (mstOffset + localOffset) * 60000);
    const tomorrow = new Date(mstNow);
    tomorrow.setDate(tomorrow.getDate() + 1);
    tomorrow.setHours(3, 0, 0, 0);
    const localTomorrow3am = new Date(tomorrow.getTime() - (mstOffset + localOffset) * 60000);
    return toDateTimeLocal(localTomorrow3am);
  };

  const nextScheduleFromWindow = (window: SendTimeWindowRecommendation) => {
    const now = new Date();
    const days = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];
    const targetDay = days.indexOf(window.day_of_week);
    const currentDay = now.getDay();
    let daysUntil = (targetDay - currentDay + 7) % 7;
    if (daysUntil === 0 && now.getHours() >= window.start_hour) daysUntil = 7;
    const start = new Date(now);
    start.setDate(start.getDate() + daysUntil);
    start.setHours(window.start_hour, 0, 0, 0);
    const end = new Date(start);
    end.setHours(window.end_hour, 0, 0, 0);
    if (window.end_hour < window.start_hour) {
      end.setDate(end.getDate() + 1);
    }
    return { start, end };
  };

  const hydrateDraft = useCallback((draft: PMTADraftResponse) => {
    const input = draft.campaign_input || {};
    const derivedISPs = Array.from(new Set([
      ...(input.target_isps || []),
      ...((input.isp_plans || []).map(plan => plan.isp).filter(Boolean)),
    ]));
    const nextPriority = (input.send_priority && input.send_priority.length > 0)
      ? input.send_priority
      : [
          ...(input.inclusion_lists || []).map(id => ({ id, type: 'list' as const })),
          ...(input.inclusion_segments || []).map(id => ({ id, type: 'segment' as const })),
        ];
    const nextQuotas = (input.isp_quotas || []).reduce<Record<string, number>>((acc, quota) => {
      if (quota?.isp) acc[quota.isp] = quota.volume || 0;
      return acc;
    }, {});
    const nextPlans = (input.isp_plans || []).reduce<Record<string, ISPPlanFormState>>((acc, plan, index) => {
      if (!plan?.isp) return acc;
      const spans = (plan.time_spans || []).map((span, spanIndex) => ({
        id: `${plan.isp}-draft-${index}-${spanIndex}`,
        startAt: toLocalInputValue(span.start_at),
        endAt: toLocalInputValue(span.end_at),
        timezone: span.timezone || plan.timezone || 'UTC',
        source: span.source || 'manual',
      }));
      let durationHours = 8;
      if (spans.length > 0 && spans[0].startAt && spans[0].endAt) {
        const s = new Date(spans[0].startAt).getTime();
        const e = new Date(spans[0].endAt).getTime();
        if (e > s) durationHours = Math.round((e - s) / 3600000);
      }
      acc[plan.isp] = {
        isp: plan.isp,
        useCustomSchedule: draft.schedule_mode === 'per-isp',
        timezone: plan.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC',
        cadenceMode: plan.cadence?.mode === 'interval' ? 'interval' : 'single',
        everyMinutes: plan.cadence?.every_minutes || 15,
        batchSize: plan.cadence?.batch_size || nextQuotas[plan.isp] || 500,
        durationHours,
        startTime: spans.length > 0 ? spans[0].startAt : '',
        throttleStrategy: plan.throttle_strategy || 'auto',
        timeSpans: spans,
      };
      return acc;
    }, {});

    // Tell the property-change effect that THIS domain move came from a restore,
    // so it keeps the audience the draft carries instead of clearing it.
    hydratedDomainRef.current = input.sending_domain || '';
    setCampaignId(draft.campaign_id || input.campaign_id || '');
    setCampaignName(input.name || draft.name || '');
    if (input.name || draft.name) setNameTouched(true);
    setSelectedISPs(derivedISPs);
    setISPQuotas(nextQuotas);
    setRandomizeAudience(Boolean(input.randomize_audience));
    setContentLocked(Boolean(input.content_locked));
    setSelectedDomain(input.sending_domain || '');
    setVariants(input.variants && input.variants.length > 0
      ? input.variants
      : [{ variant_name: 'A', from_name: '', subject: '', preview_text: '', html_content: '', split_percent: 100 }]);
    setSelectedLists(input.inclusion_lists || []);
    setSelectedSegments(input.inclusion_segments || []);
    setSendPriority(nextPriority);
    setSelectedSuppLists(input.exclusion_lists || []);
    // A board-shaped draft carries its clicker segments in BOTH inclusion and
    // exclusion (that is what the disjointness checkbox emits). Restoring it
    // verbatim would look like a contradiction on screen and trip the step-4
    // gate, so fold the overlap back into the checkbox it came from.
    const restoredInclusion = new Set(input.inclusion_segments || []);
    const restoredExclusion = input.exclusion_segments || [];
    const overlap = restoredExclusion.filter(id => restoredInclusion.has(id));
    setSelectedExclusionSegments(restoredExclusion.filter(id => !restoredInclusion.has(id)));
    setExcludeClickers(overlap.length > 0);
    setSendMode(input.send_mode === 'scheduled' ? 'scheduled' : 'immediate');
    setScheduleMode(draft.schedule_mode === 'per-isp' ? 'per-isp' : 'quick');
    setScheduledAt(toLocalInputValue(input.scheduled_at));
    setScheduleSeeded(true);
    setISPPlansByKey(nextPlans);
    // New round-trip fields. The draft blob is the whole PMTACampaignInput
    // (mailing_campaigns.pmta_config), so these persist with no server change.
    setSelectedProfileId(input.sending_profile_id || '');
    setSelectedOfferId(input.offer_id || '');
    setCampaignTimezone(input.timezone || SEND_DAY_TIMEZONE);
    setThrottleStrategy(input.throttle_strategy || DEFAULT_THROTTLE_STRATEGY);
    if (typeof input.use_master_selection === 'boolean') setMasterTopUp(input.use_master_selection);
    // A restored draft with any finite per-ISP volume was capped on purpose.
    const restoredUncapped = (input.isp_quotas || []).every(q => !q.volume)
      && (input.isp_plans || []).every(pl => !pl.quota);
    setAudienceBound(restoredUncapped);
    // Engagement chips are re-derived from the restored inclusion segments once
    // the property's grid arrives (the grid is the only place that knows which
    // id is a clicker and which is an opener).
    setRestoredInclusionSegments(input.inclusion_segments || []);
  }, []);

  const fetchCloneCandidates = useCallback(async () => {
    setCloneLoading(true);
    setCloneError('');
    try {
      // Scope candidates to the selected domain's APEX. The server resolves the
      // brand root, so em.<apex> and m.<apex> campaigns are offered together.
      const qs = selectedDomain ? `?domain=${encodeURIComponent(selectedDomain)}` : '';
      const res = await fetchWithRetry(`${API_BASE}/pmta-campaign/clone-candidates${qs}`);
      if (res.ok) {
        const data = await res.json();
        setCloneCandidates(data.campaigns || []);
        setCloneApex(data.apex || '');
      } else {
        // A failed load previously fell through here silently and rendered the
        // empty state, which is indistinguishable from "this brand has nothing
        // to clone" — that is the "it just doesn't load" report. Say so instead.
        const body = await res.json().catch(() => null);
        setCloneError(body?.error || `Could not load campaigns (HTTP ${res.status}). Try again.`);
        setCloneCandidates([]);
      }
    } catch (err) {
      console.warn('[Wizard] clone candidates fetch failed:', err);
      setCloneError('Could not reach the server. Try again.');
      setCloneCandidates([]);
    }
    setCloneLoading(false);
  }, [fetchWithRetry, selectedDomain]);

  const applyClone = useCallback(async (candidateId: string) => {
    setCloneApplying(candidateId);
    try {
      const res = await fetchWithRetry(`${API_BASE}/pmta-campaign/${candidateId}/clone-data`);
      if (!res.ok) {
        const data = await res.json().catch(() => null);
        console.warn('[Wizard] clone data error:', data?.error || res.status);
        setCloneApplying('');
        return;
      }
      const draftData = await res.json() as PMTADraftResponse;
      hydrateDraft(draftData);
      setCampaignId('');
      setShowClonePanel(false);
      setStep(1);

      setGlobalScheduleStart(nextScheduleDefault());
      setGlobalScheduleTimezone('America/Denver');

      // Clear stale state from previous wizard sessions
      setStepAttempted({});
      setAudienceEstimate(null);
      setAudienceError('');
      setRecommendations([]);
      setRecsLoaded(false);
      setDeployResult(null);
      setDraftError('');

      setDraftStatus(`Cloned from "${draftData.name?.replace(' (Clone)', '')}"`);
    } catch (err) {
      console.warn('[Wizard] clone apply failed:', err);
    }
    setCloneApplying('');
  }, [fetchWithRetry, hydrateDraft]);

  // Close clone panel on outside click
  useEffect(() => {
    if (!showClonePanel) return;
    const handleClick = (e: MouseEvent) => {
      if (clonePanelRef.current && !clonePanelRef.current.contains(e.target as Node)) {
        setShowClonePanel(false);
      }
    };
    document.addEventListener('mousedown', handleClick);
    return () => document.removeEventListener('mousedown', handleClick);
  }, [showClonePanel]);

  // ── Edit mode: load campaign data when editCampaignId is set ────────
  useEffect(() => {
    if (!editCampaignId || !orgId) return;
    let cancelled = false;
    setLoadingDraft(true);
    setDraftStatus('');
    setDraftError('');
    fetchWithRetry(`${API_BASE}/pmta-campaign/${editCampaignId}/edit-data`)
      .then(async res => {
        const data = await res.json().catch(() => null);
        if (!res.ok) throw new Error(data?.error || `Failed to load campaign (HTTP ${res.status})`);
        return data;
      })
      .then(data => {
        if (cancelled) return;
        hydrateDraft(data);
        if (data.campaign_id) setCampaignId(data.campaign_id);
        setDraftStatus(`Editing campaign: ${data.name || editCampaignId}`);
      })
      .catch((err: any) => {
        if (cancelled) return;
        setDraftError(err?.message || 'Failed to load campaign for editing.');
      })
      .finally(() => { if (!cancelled) setLoadingDraft(false); });
    return () => { cancelled = true; };
  }, [editCampaignId, orgId, fetchWithRetry, hydrateDraft]);

  // ── Load draft on mount (skip when editing an existing campaign) ───
  useEffect(() => {
    if (editCampaignId) return;
    let cancelled = false;

    if (!orgId) {
      setLoadingDraft(false);
      return;
    }

    setLoadingDraft(true);
    fetchWithRetry(`${API_BASE}/pmta-campaign/draft`)
      .then(async res => {
        if (res.status === 404) return null;
        const data = await res.json().catch(() => null);
        if (!res.ok) {
          throw new Error(data?.error || `Failed to load draft (HTTP ${res.status})`);
        }
        return data as PMTADraftResponse;
      })
      .then(async data => {
        if (cancelled) return;
        if (data) {
          hydrateDraft(data);
          setDraftError('');
          const loadedAt = data.updated_at ? new Date(data.updated_at).toLocaleString() : 'earlier';
          setDraftStatus(`Loaded saved draft from ${loadedAt}.`);
          return;
        }
        // No draft — seed quotas from the most recent completed campaign
        try {
          const lqRes = await fetchWithRetry(`${API_BASE}/pmta-campaign/last-quotas`);
          if (!lqRes.ok || cancelled) return;
          const lq = await lqRes.json();
          if (cancelled || !lq?.quotas) return;
          const parsed = (lq.quotas as { isp: string; volume: number }[]).reduce<Record<string, number>>(
            (acc, q) => { if (q?.isp) acc[q.isp] = q.volume || 0; return acc; }, {},
          );
          if (Object.keys(parsed).length > 0) {
            setISPQuotas(parsed);
            const src = lq.source_campaign || 'previous campaign';
            setDraftStatus(`Quotas loaded from: ${src}`);
          }
        } catch { /* fall through to DEFAULT_ISP_QUOTAS */ }
      })
      .catch((err: any) => {
        if (cancelled) return;
        setDraftError(err?.message || 'Failed to load saved draft.');
      })
      .finally(() => {
        if (!cancelled) setLoadingDraft(false);
      });

    return () => {
      cancelled = true;
    };
  }, [editCampaignId, orgId, fetchWithRetry, hydrateDraft]);

  const buildDefaultISPPlan = useCallback((isp: string, previous?: ISPPlanFormState): ISPPlanFormState => ({
    isp,
    useCustomSchedule: previous?.useCustomSchedule ?? true,
    timezone: previous?.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC',
    cadenceMode: previous?.cadenceMode || 'interval',
    everyMinutes: previous?.everyMinutes || 15,
    batchSize: previous?.batchSize || (DEFAULT_ISP_QUOTAS[isp] || 500),
    durationHours: previous?.durationHours || 8,
    startTime: previous?.startTime || '',
    throttleStrategy: previous?.throttleStrategy || 'auto',
    timeSpans: previous?.timeSpans || [],
  }), []);

  const updateISPPlan = (isp: string, updater: (plan: ISPPlanFormState) => ISPPlanFormState) => {
    setISPPlansByKey(prev => {
      const current = prev[isp] || buildDefaultISPPlan(isp);
      return { ...prev, [isp]: updater(current) };
    });
  };

  const addTimeSpanToPlan = (isp: string, span?: Partial<ISPTimeSpanFormState>) => {
    updateISPPlan(isp, plan => ({
      ...plan,
      useCustomSchedule: true,
      timeSpans: [
        ...plan.timeSpans,
        {
          id: `${isp}-${Date.now()}-${plan.timeSpans.length}`,
          startAt: span?.startAt || scheduledAt,
          endAt: span?.endAt || scheduledAt,
          timezone: span?.timezone || plan.timezone,
          source: span?.source || 'manual',
        },
      ],
    }));
  };

  useEffect(() => {
    setISPPlansByKey(prev => {
      const next: Record<string, ISPPlanFormState> = {};
      selectedISPs.forEach(isp => {
        next[isp] = buildDefaultISPPlan(isp, prev[isp]);
      });
      return next;
    });
  }, [selectedISPs, buildDefaultISPPlan]);

  useEffect(() => {
    setRecsLoaded(false);
    setRecommendations([]);
  }, [selectedISPs.join(',')]);

  // ── Deploy ───────────────────────────────────────────────────────────────

  const buildCampaignPayload = useCallback(() => {
    // Audience-bound (the standing uncapped engaged-tier doctrine): volume 0
    // per ISP means "unlimited — the segment is the cap". Emitting a row per
    // selected ISP (rather than dropping them) keeps the ISP set explicit in
    // the persisted quota payload.
    const quotaArray = audienceBound
      ? selectedISPs.map(isp => ({ isp, volume: 0 }))
      : Object.entries(ispQuotas)
          .filter(([, v]) => v > 0)
          .map(([isp, volume]) => ({ isp, volume }));
    const globalScheduleISO = scheduledAt ? new Date(scheduledAt).toISOString() : '';
    const ispPlans = selectedISPs.filter(isp => isp !== 'other').map(isp => {
      const plan = ispPlansByKey[isp] || buildDefaultISPPlan(isp);
      const useGlobalSchedule = scheduleMode === 'quick' || !plan.useCustomSchedule;
      const quota = audienceBound ? 0 : (ispQuotas[isp] || 0);

      let spans: any[] = [];
      // Interval cadence with a server-computed batch on EVERY path. The
      // planner spreads the actual audience across the window when batch_size
      // is 0; sizing the batch from the quota puts the whole send in wave 1
      // whenever the audience and the quota differ (and the server forces
      // mode='interval' anyway — normalizeISPPlan).
      let cadenceMode = 'interval';
      let everyMinutes = DEFAULT_WAVE_INTERVAL_MINUTES;
      let batchSize = 0;

      if (sendMode === 'scheduled') {
        if (useGlobalSchedule) {
          if (globalScheduleISO) {
            // CRITICAL: source MUST be "duration-calc" or "manual" — these are
            // the only literals that bypass `waveSanityCheck`'s minimum-span
            // enforcement (upside-down/internal/api/pmta_campaign_planner.go
            // isUserExplicitSpan ~line 1862). Earlier versions emitted
            // "global-default" with start_at == end_at (zero span); at >=500
            // recipients/ISP that silently failed wave creation. The Quick
            // Schedule mode now spans the campaign over the canonical 8h
            // throttle window starting at the operator-chosen timestamp.
            const start = new Date(globalScheduleISO);
            const end = new Date(start.getTime() + 8 * 3600000);
            spans = [{
              type: 'absolute',
              start_at: start.toISOString(),
              end_at: end.toISOString(),
              timezone: plan.timezone,
              source: 'manual',
            }];
          }
        } else {
          const dur = plan.durationHours || 8;
          const interval = plan.everyMinutes || DEFAULT_WAVE_INTERVAL_MINUTES;
          // batch_size 0 = "spread the ACTUAL audience evenly across the window"
          // (pmta_campaign_planner.buildPMTAWaveSpecs). Deriving it from the
          // quota is wrong whenever the audience differs from the quota, and on
          // an audience-bound plan (quota 0) it previously fell back to the
          // default ISP quota — 50,000 for gmail — which collapsed the whole
          // send into a single wave. The board sends 0 for exactly this reason.
          batchSize = 0;
          cadenceMode = plan.cadenceMode;
          everyMinutes = interval;

          if (plan.startTime) {
            const start = new Date(plan.startTime);
            const end = new Date(start.getTime() + dur * 3600000);
            spans = [{
              type: 'absolute',
              start_at: start.toISOString(),
              end_at: end.toISOString(),
              timezone: plan.timezone,
              source: 'duration-calc',
            }];
          } else if (plan.timeSpans.length > 0) {
            spans = plan.timeSpans
              .filter(span => span.startAt && span.endAt)
              .map(span => ({
                type: 'absolute',
                start_at: new Date(span.startAt).toISOString(),
                end_at: new Date(span.endAt).toISOString(),
                timezone: span.timezone || plan.timezone,
                source: span.source || 'manual',
              }));
          }
        }
      }

      return {
        isp,
        quota,
        randomize_audience: randomizeAudience,
        throttle_strategy: plan.throttleStrategy || throttleStrategy,
        timezone: plan.timezone || campaignTimezone,
        cadence: {
          mode: cadenceMode,
          every_minutes: everyMinutes,
          batch_size: batchSize,
        },
        time_spans: spans,
      };
    });

    // The long-tail 'other' lane rides a synthetic plan. It is included
    // whenever the operator SELECTED it — never conditioned on its quota.
    // 0 means UNLIMITED everywhere else in this payload (normalizePMTACampaign
    // maps volume<=0 to Quota 0 = audience-bound, pmta_campaign_planner.go:388),
    // so the old `otherQuota > 0` test silently DROPPED the whole long-tail
    // lane the moment the operator left its box at 0 — the one place where 0
    // meant "exclude" instead of "unlimited" (operator 2026-08-18).
    const otherQuota = audienceBound ? 0 : (ispQuotas['other'] || 0);
    const includeOther = selectedISPs.includes('other');
    if (includeOther) {
      ispPlans.push({
        isp: 'other',
        quota: otherQuota,
        randomize_audience: randomizeAudience,
        throttle_strategy: throttleStrategy,
        timezone: globalScheduleTimezone || campaignTimezone,
        // Mirror the canonical plans: interval cadence with a server-computed
        // batch, never a single-wave blast sized to the quota.
        cadence: { mode: 'interval', every_minutes: DEFAULT_WAVE_INTERVAL_MINUTES, batch_size: 0 },
        time_spans: ispPlans.length > 0 ? ispPlans[0].time_spans : [],
      });
    }

    const canonicalISPs = selectedISPs.filter(isp => isp !== 'other');
    const targetISPs = includeOther ? [...canonicalISPs, 'other'] : canonicalISPs;

    const engagementIds = [...selectedClickerIds, ...selectedOpenerIds, ...selectedOtherIds];
    const advancedSegmentIds = sendPriority.filter(p => p.type === 'segment').map(p => p.id);
    const mergedSegmentIds = [
      ...engagementIds,
      ...advancedSegmentIds.filter(id => !engagementIds.includes(id)),
    ];
    const mergedSendPriority = [
      ...engagementIds.map(id => ({ id, type: 'segment' as const })),
      ...sendPriority.filter(p => !(p.type === 'segment' && engagementIds.includes(p.id))),
    ];

    const payload: Record<string, any> = {
      name: campaignName,
      target_isps: targetISPs,
      sending_domain: selectedDomain,
      variants,
      isp_plans: ispPlans,
      isp_quotas: quotaArray,
      randomize_audience: randomizeAudience,
      // Clickers lead, then openers, then anything picked in the advanced
      // panel — send_priority is drained in order by the planner, so the
      // highest-signal audience is selected first (signal grading: click =
      // gold, open = silver).
      inclusion_segments: mergedSegmentIds,
      inclusion_lists: sendPriority.filter(p => p.type === 'list').map(p => p.id),
      send_priority: mergedSendPriority,
      exclusion_lists: selectedSuppLists,
      // Engager disjointness (board_generator.py OFR-ENG): when both tiers are
      // selected, the clicker segments are excluded so the same person is not
      // mailed twice on the same day by the two tiers.
      exclusion_segments: [
        ...selectedExclusionSegments,
        ...(excludeClickers && selectedClickerIds.length > 0 && selectedOpenerIds.length > 0
          ? selectedClickerIds.filter(id => !selectedExclusionSegments.includes(id))
          : []),
      ],
      send_days: [],
      send_hour: new Date().getUTCHours(),
      timezone: campaignTimezone,
      throttle_strategy: throttleStrategy,
      send_mode: sendMode,
      content_locked: contentLocked,
      min_remail_hours: 0,
      // ALWAYS explicit. mailing_campaigns.use_master_selection defaults to
      // TRUE, so omitting it puts a segment-sourced engagement send on the
      // master-selection path, where the planner drains the chosen segments and
      // then tops the audience up from mailing_subscriber_domain_state — with
      // no finite quota to stop at, that is the entire sending domain.
      use_master_selection: masterTopUp,
    };
    if (selectedProfileId) {
      // Pin the route. Without this the server takes the most recently created
      // active profile for the domain, which is non-deterministic for any
      // domain carrying both an SES-tenant and a PMTA profile.
      payload.sending_profile_id = selectedProfileId;
    }

    if (campaignId) {
      payload.campaign_id = campaignId;
    }
    if (selectedOfferId) {
      // Chosen offer (mailing_offers UUID) → engine.PMTACampaignInput.OfferID:
      // attribution stamps it at stage/deploy and offer/converted suppression fires.
      payload.offer_id = selectedOfferId;
    }
    if (sendMode === 'scheduled' && scheduledAt) {
      payload.scheduled_at = new Date(scheduledAt).toISOString();
    }
    return payload;
  }, [
    campaignId,
    campaignName,
    buildDefaultISPPlan,
    contentLocked,
    ispPlansByKey,
    ispQuotas,
    randomizeAudience,
    scheduleMode,
    scheduledAt,
    selectedDomain,
    selectedExclusionSegments,
    selectedISPs,
    selectedOfferId,
    selectedProfileId,
    selectedSuppLists,
    sendMode,
    sendPriority,
    variants,
    campaignTimezone,
    throttleStrategy,
    masterTopUp,
    excludeClickers,
    selectedClickerIds,
    selectedOpenerIds,
    selectedOtherIds,
    globalScheduleTimezone,
    audienceBound,
  ]);

  const handleSaveDraft = async () => {
    setSavingDraft(true);
    setDraftError('');
    try {
      const res = await fetchWithRetry(`${API_BASE}/pmta-campaign/draft`, {
        method: 'POST',
        body: JSON.stringify({
          campaign_input: buildCampaignPayload(),
          schedule_mode: scheduleMode,
        }),
      }, 3);
      const data = await res.json();
      if (!res.ok) {
        setDraftError(data.error || `Draft save failed (HTTP ${res.status})`);
        return;
      }
      setCampaignId(data.campaign_id || '');
      setDraftStatus(`Draft saved ${data.updated_at ? new Date(data.updated_at).toLocaleString() : 'successfully'}.`);
    } catch (err: any) {
      setDraftError(err?.message || 'Draft save failed — network error. Click Save Draft to retry.');
    } finally {
      setSavingDraft(false);
    }
  };

  const handleDeploy = useCallback(async (overrideReason?: string) => {
    setDeploying(true);
    setDeployResult(null);
    setGateFailure(null);
    try {
      const body: Record<string, any> = buildCampaignPayload();
      if (overrideReason && overrideReason.trim()) {
        // Audit-logged server-side (auditGateOverride) — the same payload is
        // re-POSTed, only the override envelope is added.
        body.gate_override = { reason: overrideReason.trim() };
      }
      const res = await fetchWithRetry(`${API_BASE}/pmta-campaign/deploy`, {
        method: 'POST',
        body: JSON.stringify(body),
      }, 0);
      const contentType = res.headers.get('content-type') || '';
      if (!contentType.includes('application/json')) {
        setDeployResult({ error: `Deploy timed out (HTTP ${res.status}). The campaign may still be processing — check the campaign list before retrying.` });
        setDeploying(false);
        return;
      }
      const data = await res.json();
      if (res.status === 412) {
        // Send-day gates. Surface which gate failed and let the operator
        // re-submit with a reason instead of dead-ending the wizard.
        setGateFailure({ error: data.error || 'send-day gates failed', failed_gates: data.failed_gates || [] });
        setDeploying(false);
        return;
      }
      if (!res.ok) {
        setDeployResult({ error: data.error || `Deploy failed (HTTP ${res.status})` });
      } else if (res.status === 202) {
        // Async deploy accepted — campaign audience is being finalized in the background
        setCampaignId(data.campaign_id || campaignId);
        setDeployResult({ ...data, status: data.status || 'finalizing_audience' });
        campaignComplete(campaignName || 'Campaign');
        setShowCompleteModal(true);
        if (onCampaignPreparing && data.campaign_id) {
          onCampaignPreparing(data.campaign_id, data.name || campaignName || 'Campaign');
        }
        if (onEditComplete) onEditComplete();
      } else {
        setDeployResult(data);
        setCampaignId(data.campaign_id || campaignId);
        campaignComplete(campaignName || 'Campaign');
        setShowCompleteModal(true);
      }
    } catch (err: any) {
      setDeployResult({ error: err?.message || 'Deploy failed — network error. Click Deploy to retry.' });
    }
    setDeploying(false);
  }, [buildCampaignPayload, campaignComplete, campaignId, campaignName, fetchWithRetry, onCampaignPreparing, onEditComplete]);

  // ── Warm-up branch: preview + submit ─────────────────────────────────────

  // The documented creative response carries sha256/html_bytes but not
  // necessarily the body. If it did, preview from what we already hold;
  // otherwise ask for the body explicitly and say so plainly when there is
  // none, rather than opening a blank white iframe.
  const openWarmupPreview = useCallback(async () => {
    setWarmupPreviewOpen(true);
    setWarmupPreviewError('');
    const inline = warmupCreative?.html || warmupCreative?.html_content || '';
    if (inline) { setWarmupPreviewHtml(inline); return; }
    if (!warmupBrandSlug) { setWarmupPreviewHtml(''); return; }
    setWarmupPreviewLoading(true);
    try {
      // The BODY lives on the newsletter preview endpoint — warmup/creative
      // returns metadata only (it has no include_html handling), which is why
      // this modal could never render anything but the "no body" state.
      const res = await apiFetch(
        `${API_BASE}/pmta-campaign/newsletter/preview?include_html=1&sending_domain=${encodeURIComponent(selectedDomain)}`,
      );
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        setWarmupPreviewError(data?.error || `HTTP ${res.status}`);
        setWarmupPreviewHtml('');
        return;
      }
      const row: NewsletterDomainRow | undefined = (data?.domains || [])[0];
      setWarmupPreviewHtml(row?.html || '');
    } catch (err: any) {
      setWarmupPreviewError(err?.message || 'network error');
      setWarmupPreviewHtml('');
    } finally {
      setWarmupPreviewLoading(false);
    }
  }, [warmupCreative, warmupBrandSlug, selectedDomain]);

  // Records INTENT. It does not send, and it does not create a campaign — a
  // separate builder consumes the request (~40 min, disk-bound). Every string
  // this UI shows about the outcome says "queued for build", never "deployed".
  const handleWarmupRequest = useCallback(async () => {
    setWarmupSubmitting(true);
    setWarmupResult(null);
    try {
      // Same quota shape the offer payload emits: volume 0 per selected ISP on
      // an audience-bound send, else the finite per-ISP quotas. Written out
      // here rather than reusing buildCampaignPayload so the offer path is not
      // re-entered on this branch.
      // ⚠️ MAP, not array: warmupRequestUpsertReq.ISPQuotas is
      // map[string]int, so an array body failed json.Decode and the endpoint
      // answered 400 "invalid JSON body" — nothing was ever queued.
      const ispQuotaMap: Record<string, number> = audienceBound
        ? Object.fromEntries(selectedISPs.map(isp => [isp, 0]))
        : Object.fromEntries(Object.entries(ispQuotas).filter(([, v]) => v > 0));

      const body = {
        sending_domain: selectedDomain,
        brand_slug: warmupBrandSlug,
        creative_id: warmupCreative?.id || '',
        subject: warmupSubject,
        preheader: warmupPreheader,
        audience_segment_ids: warmupSelectedSegmentIds,
        cold_source: coldSource.trim(),
        cold_quota: coldQuotaNum ?? 0,
        isp_quotas: ispQuotaMap,
        scheduled_at: intentScheduledAtISO,
        status: 'requested',
      };

      const res = await apiFetch(`${API_BASE}/pmta-campaign/warmup/request`, {
        method: 'POST',
        body: JSON.stringify(body),
      });
      const ct = res.headers.get('content-type') || '';
      if (!ct.includes('application/json')) {
        setWarmupResult({ error: `The request endpoint returned a non-JSON response (HTTP ${res.status}). Nothing was queued — check the build ledger below before retrying.` });
        return;
      }
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        setWarmupResult({ error: data?.error || `Request failed (HTTP ${res.status})` });
        return;
      }
      setWarmupResult({ request: data?.request || data || {} });
      setWarmupRequestsKey(k => k + 1);
    } catch (err: any) {
      setWarmupResult({ error: err?.message || 'Request failed — network error. Nothing was queued; click Queue for build to retry.' });
    } finally {
      setWarmupSubmitting(false);
    }
  }, [
    audienceBound, coldQuotaNum, coldSource, ispQuotas, selectedDomain, selectedISPs,
    warmupBrandSlug, warmupCreative, warmupPreheader, intentScheduledAtISO,
    warmupSelectedSegmentIds, warmupSubject,
  ]);

  // NEWSLETTERS submit: an N-DOMAIN FAN-OUT of the same intent record the
  // warm-up branch writes. It records intent ONLY — no campaign, no mail — and
  // it deliberately never touches buildCampaignPayload or
  // POST /pmta-campaign/deploy. One request per included domain, ONE shared
  // scheduled instant, and one result row per domain so a partial failure can
  // never read as "all queued".
  const handleNewsletterRequest = useCallback(async () => {
    setNewsletterSubmitting(true);
    setNewsletterResults(null);
    // Audience-bound by doctrine: volume 0 per selected ISP means the segment
    // is the cap. MAP, not array — the endpoint decodes map[string]int.
    const ispQuotaMap: Record<string, number> = audienceBound
      ? Object.fromEntries(selectedISPs.map(isp => [isp, 0]))
      : Object.fromEntries(Object.entries(ispQuotas).filter(([, v]) => v > 0));

    const out: { sending_domain: string; ok: boolean; error?: string; request_id?: string }[] = [];
    for (const v of newsletterIncluded) {
      const r = v.row;
      const slug = (r.brand_slug || '').trim() || deriveBrandSlug(undefined, r.sending_domain);
      const segIds = (newsletterAudience[r.sending_domain]?.picked || []).map(sg => sg.id);
      try {
        const res = await apiFetch(`${API_BASE}/pmta-campaign/warmup/request`, {
          method: 'POST',
          body: JSON.stringify({
            // KIND IS LOAD-BEARING — never omit it. The endpoint defaults a
            // missing kind to 'kumo_warmup' (warmup_requests.go:649), and
            // kumo_warmup is restricted to routing_mode='kumo' (:747-752). So
            // an omitted kind makes this whole mode fail two different ways:
            // the 16 PMTA/SES legacy domains are rejected 400, and the 11 kumo
            // domains are accepted but filed as WARM-UP — which then collides
            // with a genuine warm-up request for the same domain+day on the
            // kind-aware live-slot index, the exact unique violation `kind` was
            // introduced to prevent.
            kind: 'newsletter',
            sending_domain: r.sending_domain,
            brand_slug: slug,
            creative_id: (r.creative_id || '').trim(),
            // The BYTE PIN, not decoration. The server recomputes the sha from
            // the live row and 409s on drift, so a creative refreshed between
            // this audit and the build is refused rather than silently mailed
            // under an approved subject. Omitting it is a 400 for a newsletter
            // request (warmup_requests.go:789) — audited bytes are the contract.
            creative_sha256: (r.creative_sha256 || '').trim(),
            subject: r.subject || '',
            preheader: r.preheader || '',
            audience_segment_ids: segIds,
            isp_quotas: ispQuotaMap,
            scheduled_at: intentScheduledAtISO,
            status: 'requested',
          }),
        });
        const ct = res.headers.get('content-type') || '';
        if (!ct.includes('application/json')) {
          out.push({ sending_domain: r.sending_domain, ok: false,
            error: `non-JSON response (HTTP ${res.status}) — nothing was queued for this domain` });
          continue;
        }
        const data = await res.json().catch(() => ({}));
        if (!res.ok) {
          out.push({ sending_domain: r.sending_domain, ok: false, error: data?.error || `HTTP ${res.status}` });
          continue;
        }
        out.push({ sending_domain: r.sending_domain, ok: true, request_id: (data?.request || data || {}).id });
      } catch (err: any) {
        out.push({ sending_domain: r.sending_domain, ok: false, error: err?.message || 'network error' });
      }
    }
    setNewsletterResults(out);
    setNewsletterSubmitting(false);
    setWarmupRequestsKey(k => k + 1);
  }, [
    audienceBound, ispQuotas, selectedISPs, newsletterIncluded,
    newsletterAudience, intentScheduledAtISO,
  ]);

  // ── Toggle helpers ───────────────────────────────────────────────────────

  // The toggle REFLECTS REALITY: it reads as on only while every lane in the
  // form is actually 0. Edit one field back to a real cap and it un-checks
  // itself rather than continuing to claim "no quota".
  const quotaLanesInForm = useMemo(
    () => Array.from(new Set([...selectedISPs, 'other', ...Object.keys(ispQuotas)])),
    [selectedISPs, ispQuotas],
  );
  const allQuotaLanesZero = quotaLanesInForm.every(isp => !(ispQuotas[isp] > 0));
  const noQuotaOn = quotaSnapshot !== null && allQuotaLanesZero;

  const toggleNoQuota = useCallback((on: boolean) => {
    if (on) {
      // Capture the CURRENT values (not any stale snapshot) so restore is exact.
      setQuotaSnapshot({ ...ispQuotas });
      const zeroed: Record<string, number> = { ...ispQuotas };
      for (const isp of [...selectedISPs, 'other', ...Object.keys(ispQuotas)]) zeroed[isp] = 0;
      setISPQuotas(zeroed);
      return;
    }
    if (quotaSnapshot) setISPQuotas({ ...quotaSnapshot });
    setQuotaSnapshot(null);
  }, [ispQuotas, quotaSnapshot, selectedISPs]);

  const toggleISP = (isp: string) => {
    setSelectedISPs(prev => {
      if (prev.includes(isp)) {
        setISPQuotas(q => { const n = { ...q }; delete n[isp]; return n; });
        return prev.filter(i => i !== isp);
      }
      return [...prev, isp];
    });
  };
  const toggleList = (id: string) => {
    setSelectedLists(prev => {
      if (prev.includes(id)) {
        setSendPriority(p => p.filter(item => !(item.id === id && item.type === 'list')));
        return prev.filter(i => i !== id);
      }
      setSendPriority(p => [...p, { id, type: 'list' }]);
      return [...prev, id];
    });
  };
  const toggleSegment = (id: string) => {
    setSelectedSegments(prev => {
      if (prev.includes(id)) {
        setSendPriority(p => p.filter(item => !(item.id === id && item.type === 'segment')));
        return prev.filter(i => i !== id);
      }
      setSendPriority(p => [...p, { id, type: 'segment' }]);
      return [...prev, id];
    });
  };
  const toggleSuppList = (id: string) => {
    setSelectedSuppLists(prev => prev.includes(id) ? prev.filter(i => i !== id) : [...prev, id]);
  };
  const toggleExclusionSegment = (id: string) => {
    setSelectedExclusionSegments(prev => prev.includes(id) ? prev.filter(i => i !== id) : [...prev, id]);
  };
  const movePriorityUp = (idx: number) => {
    if (idx <= 0) return;
    setSendPriority(prev => {
      const next = [...prev];
      [next[idx - 1], next[idx]] = [next[idx], next[idx - 1]];
      return next;
    });
  };
  const movePriorityDown = (idx: number) => {
    setSendPriority(prev => {
      if (idx >= prev.length - 1) return prev;
      const next = [...prev];
      [next[idx], next[idx + 1]] = [next[idx + 1], next[idx]];
      return next;
    });
  };
  const dragPriorityRef = useRef<number | null>(null);

  useEffect(() => {
    if (lists.length === 0 && segments.length === 0) return;
    setSendPriority(prev => {
      const validListIds = new Set(lists.map(l => l.id));
      const validSegmentIds = new Set(segments.map(s => s.id));
      const pruned = prev.filter(item =>
        item.type === 'list' ? validListIds.has(item.id) : validSegmentIds.has(item.id)
      );
      return pruned.length === prev.length ? prev : pruned;
    });
  }, [lists, segments]);

  // ── Auto-populate from_name when sending domain changes ─────────────────
  useEffect(() => {
    if (!selectedDomain) return;
    const match = sendingDomains.find(d => d.domain === selectedDomain);
    if (!match?.from_name) return;
    setVariants(prev => {
      if (prev.length === 0) return prev;
      const updated = prev.map(v => ({ ...v, from_name: match.from_name! }));
      return updated;
    });
  }, [selectedDomain, sendingDomains]);

  // ── Render helpers ───────────────────────────────────────────────────────

  const statusBadge = (status: string) => {
    const colors: Record<string, string> = { ready: '#10b981', caution: '#f59e0b', degraded: '#f97316', blocked: '#ef4444', green: '#10b981', yellow: '#f59e0b', red: '#ef4444', established: '#10b981', ramping: '#f59e0b', early: '#f97316', healthy: '#10b981', throttled: '#f59e0b' };
    const color = colors[status] || '#64748b';
    return (
      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 4, padding: '2px 8px', borderRadius: 4, fontSize: 11, fontWeight: 600, background: color + '22', color, border: `1px solid ${color}44`, textTransform: 'uppercase' }}>
        <span style={{ width: 6, height: 6, borderRadius: '50%', background: color }} />
        {status}
      </span>
    );
  };

  // ── Step renderers ───────────────────────────────────────────────────────

  const renderStepProviders = () => (
    <div className="wiz-step-content ig-fade-in">
      <h3 style={{ margin: '0 0 4px' }}>Select Mailbox Providers<RequiredDot /></h3>
      <p style={{ margin: '0 0 16px', color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>
        Choose which mailbox providers to target. Cards show live health from the delivery engine.
      </p>
      <StepErrorBanner stepNum={2} />

      {/* ── ISP Sending Health Panel ────────────────────────── */}
      <div style={{
        marginBottom: 20, border: '1px solid rgba(0,200,255,0.1)', borderRadius: 12,
        background: 'linear-gradient(135deg, rgba(10,15,26,0.95), rgba(13,21,38,0.95))',
        overflow: 'hidden',
      }}>
        <button
          onClick={() => setInsightsCollapsed(!insightsCollapsed)}
          style={{
            width: '100%', display: 'flex', alignItems: 'center', justifyContent: 'space-between',
            padding: '12px 16px', background: 'none', border: 'none', cursor: 'pointer', color: '#e0e6f0',
          }}
        >
          <span style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 13, fontWeight: 600 }}>
            <FontAwesomeIcon icon={faShieldAlt} style={{ color: '#00e5ff' }} />
            3-Day Provider Sending Health
            {insightsLoading && <FontAwesomeIcon icon={faSpinner} spin style={{ color: '#64748b', fontSize: 11 }} />}
            {!insightsLoading && ispInsights.length > 0 && (
              <span style={{ fontSize: 11, color: '#64748b', fontWeight: 400 }}>
                — {ispInsights.filter(i => i.recommendation === 'DECREASE' || i.recommendation === 'PAUSE').length} providers need attention
              </span>
            )}
          </span>
          <FontAwesomeIcon icon={insightsCollapsed ? faChevronDown : faChevronUp} style={{ color: '#64748b', fontSize: 11 }} />
        </button>

        {!insightsCollapsed && (
          <div style={{ padding: '0 16px 16px' }}>
            {insightAvailableDomains.length > 0 && (
              <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 12, paddingTop: 4 }}>
                <label style={{ fontSize: 11, color: '#94a3b8', fontWeight: 500, whiteSpace: 'nowrap' }}>Sending Domain:</label>
                <select
                  value={insightDomainFilter}
                  onChange={(e) => setInsightDomainFilter(e.target.value)}
                  style={{
                    flex: '0 1 260px', padding: '5px 10px', borderRadius: 6,
                    border: '1px solid rgba(0,200,255,0.15)', background: 'rgba(10,15,26,0.8)',
                    color: '#e0e6f0', fontSize: 12, cursor: 'pointer',
                    outline: 'none',
                  }}
                >
                  <option value="">All Domains</option>
                  {insightAvailableDomains.map(d => (
                    <option key={d} value={d}>{d}</option>
                  ))}
                </select>
                {insightDomainFilter && (
                  <button
                    onClick={() => setInsightDomainFilter('')}
                    title="Clear filter"
                    style={{
                      background: 'none', border: 'none', color: '#64748b', cursor: 'pointer',
                      fontSize: 11, padding: '2px 6px', borderRadius: 4,
                    }}
                  >
                    <FontAwesomeIcon icon={faTimes} />
                  </button>
                )}
              </div>
            )}
            {insightsLoading && ispInsights.length === 0 && (
              <div style={{ textAlign: 'center', padding: 20, color: '#64748b', fontSize: 12 }}>
                <FontAwesomeIcon icon={faSpinner} spin /> Analyzing 3-day sending performance…
              </div>
            )}

            {!insightsLoading && ispInsights.length === 0 && (
              <div style={{ textAlign: 'center', padding: 20, color: '#4b5563', fontSize: 12 }}>
                No sending data found in the last 3 days{insightDomainFilter ? ` for ${insightDomainFilter}` : ''}.
              </div>
            )}

            {ispInsights.length > 0 && (
              <>
                {ispInsights.some(i => i.suggested_quota !== i.current_quota && i.current_quota > 0) && (
                  <div style={{ display: 'flex', justifyContent: 'flex-end', marginBottom: 10 }}>
                    <button
                      onClick={() => {
                        const updated = { ...ispQuotas };
                        ispInsights.forEach(i => {
                          if (i.suggested_quota !== i.current_quota && i.current_quota > 0) {
                            updated[i.isp] = i.suggested_quota;
                          }
                        });
                        setISPQuotas(updated);
                      }}
                      style={{
                        padding: '6px 14px', borderRadius: 8, border: '1px solid rgba(0,200,255,0.2)',
                        background: 'rgba(0,200,255,0.08)', color: '#00b0ff', fontSize: 11,
                        fontWeight: 600, cursor: 'pointer', display: 'flex', alignItems: 'center', gap: 6,
                      }}
                    >
                      <FontAwesomeIcon icon={faMagic} /> Apply All Suggestions
                    </button>
                  </div>
                )}

                <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: 10 }}>
                  {ispInsights.filter(i => i.isp !== 'other').map(insight => {
                    const meta = ISP_META[insight.isp] || { label: insight.label, color: '#64748b', emoji: '🌐' };
                    const isExpanded = expandedInsightISP === insight.isp;
                    const recColor = RECOMMENDATION_COLORS[insight.recommendation] || '#64748b';
                    const scoreColor = insight.risk_score >= 60 ? '#ef4444' : insight.risk_score >= 40 ? '#f59e0b' : insight.risk_score >= 20 ? '#3b82f6' : '#22c55e';

                    return (
                      <div key={insight.isp} style={{ display: 'flex', flexDirection: 'column' }}>
                        <div
                          onClick={() => setExpandedInsightISP(isExpanded ? null : insight.isp)}
                          style={{
                            padding: '10px 12px', borderRadius: 10, cursor: 'pointer',
                            border: `1px solid ${isExpanded ? meta.color : 'rgba(0,200,255,0.08)'}`,
                            background: isExpanded ? 'rgba(0,200,255,0.04)' : 'rgba(255,255,255,0.02)',
                            transition: 'all 0.2s',
                          }}
                        >
                          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 8 }}>
                            <span style={{ fontSize: 12, fontWeight: 600, color: '#e0e6f0' }}>
                              {meta.emoji} {meta.label}
                            </span>
                            <span style={{
                              fontSize: 10, fontWeight: 700, padding: '2px 8px', borderRadius: 6,
                              background: `${recColor}18`, color: recColor,
                            }}>
                              {insight.recommendation}
                            </span>
                          </div>

                          <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 6 }}>
                            <span style={{ fontSize: 18, fontWeight: 700, color: scoreColor, fontFamily: 'monospace' }}>
                              {insight.risk_score}
                            </span>
                            <span style={{ fontSize: 9, color: '#64748b' }}>RISK</span>
                            <span style={{ marginLeft: 'auto', fontSize: 11, color: '#c0c4d0' }}>{fmtK(insight.sent)} sent</span>
                          </div>

                          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '3px 8px', fontSize: 10 }}>
                            <span style={{ color: '#64748b' }}>Hard <strong style={{ color: insight.hard_bounce_rate > 1 ? '#ef4444' : '#c0c4d0' }}>{insight.hard_bounce_rate}%</strong></span>
                            <span style={{ color: '#64748b' }}>Soft <strong style={{ color: insight.soft_bounce_rate > 3 ? '#f59e0b' : '#c0c4d0' }}>{insight.soft_bounce_rate}%</strong></span>
                            <span style={{ color: '#64748b' }}>Defer <strong style={{ color: insight.deferral_rate > 5 ? '#f59e0b' : '#c0c4d0' }}>{insight.deferral_rate}%</strong></span>
                            <span style={{ color: '#64748b' }}>Cmpl <strong style={{ color: insight.complaint_rate > 0.05 ? '#ef4444' : '#c0c4d0' }}>{insight.complaint_rate}%</strong></span>
                            <span style={{ color: '#64748b' }}>Opens <strong style={{ color: '#22c55e' }}>{insight.human_open_rate}%</strong></span>
                          </div>

                          {insight.mpp_opens > 0 && (
                            <div style={{ marginTop: 4, fontSize: 9, color: '#f59e0b' }}>
                              Apple Mail privacy opens: {fmtK(insight.mpp_opens)} ({insight.opened > 0 ? Math.round(insight.mpp_opens / insight.opened * 100) : 0}%)
                            </div>
                          )}

                          {insight.signals.length > 0 && (
                            <div style={{ marginTop: 6, fontSize: 10, color: '#94a3b8', lineHeight: 1.4, borderTop: '1px solid rgba(0,200,255,0.06)', paddingTop: 6 }}>
                              {insight.signals[0].detail}
                            </div>
                          )}
                        </div>
                      </div>
                    );
                  })}
                </div>

                {/* Expanded detail panel */}
                {expandedInsightISP && (() => {
                  const insight = ispInsights.find(i => i.isp === expandedInsightISP);
                  if (!insight) return null;
                  const meta = ISP_META[insight.isp] || { label: insight.label, color: '#64748b', emoji: '🌐' };
                  const maxDefer = Math.max(...insight.hourly_deferrals, 1);

                  return (
                    <div style={{
                      marginTop: 12, padding: 16, borderRadius: 10,
                      border: `1px solid ${meta.color}40`, background: 'rgba(255,255,255,0.02)',
                    }}>
                      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 14 }}>
                        <h4 style={{ margin: 0, fontSize: 14, fontWeight: 600, color: meta.color }}>
                          {meta.emoji} {meta.label} — Detailed Analysis
                        </h4>
                        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                          {insight.suggested_quota !== insight.current_quota && insight.current_quota > 0 && (
                            <button
                              onClick={() => setISPQuotas(prev => ({ ...prev, [insight.isp]: insight.suggested_quota }))}
                              style={{
                                padding: '4px 10px', borderRadius: 6, fontSize: 10, fontWeight: 600,
                                border: '1px solid rgba(0,200,255,0.2)', background: 'rgba(0,200,255,0.08)',
                                color: '#00b0ff', cursor: 'pointer',
                              }}
                            >
                              Apply Suggested Quota ({fmtK(insight.suggested_quota)})
                            </button>
                          )}
                          <button
                            onClick={() => setExpandedInsightISP(null)}
                            style={{ background: 'none', border: '1px solid rgba(0,200,255,0.1)', color: '#64748b', cursor: 'pointer', borderRadius: 6, width: 28, height: 28, fontSize: 14, display: 'flex', alignItems: 'center', justifyContent: 'center' }}
                          >&times;</button>
                        </div>
                      </div>

                      {/* Quota comparison */}
                      <div style={{ display: 'flex', gap: 16, marginBottom: 14, flexWrap: 'wrap' }}>
                        <div style={{ padding: '8px 14px', borderRadius: 8, background: 'rgba(255,255,255,0.03)', border: '1px solid rgba(0,200,255,0.06)' }}>
                          <div style={{ fontSize: 9, color: '#64748b', textTransform: 'uppercase' }}>Current Quota</div>
                          <div style={{ fontSize: 16, fontWeight: 700, color: '#e0e6f0' }}>{fmtK(insight.current_quota)}</div>
                        </div>
                        <div style={{ padding: '8px 14px', borderRadius: 8, background: 'rgba(255,255,255,0.03)', border: '1px solid rgba(0,200,255,0.06)' }}>
                          <div style={{ fontSize: 9, color: '#64748b', textTransform: 'uppercase' }}>Suggested</div>
                          <div style={{ fontSize: 16, fontWeight: 700, color: RECOMMENDATION_COLORS[insight.recommendation] || '#c0c4d0' }}>
                            {fmtK(insight.suggested_quota)}
                            {insight.suggested_quota !== insight.current_quota && insight.current_quota > 0 && (
                              <span style={{ fontSize: 11, marginLeft: 6, color: insight.suggested_quota < insight.current_quota ? '#ef4444' : '#22c55e' }}>
                                ({insight.suggested_quota < insight.current_quota ? '' : '+'}{Math.round((insight.suggested_quota - insight.current_quota) / insight.current_quota * 100)}%)
                              </span>
                            )}
                          </div>
                        </div>
                        <div style={{ padding: '8px 14px', borderRadius: 8, background: 'rgba(255,255,255,0.03)', border: '1px solid rgba(0,200,255,0.06)' }}>
                          <div style={{ fontSize: 9, color: '#64748b', textTransform: 'uppercase' }}>3-Day Volume</div>
                          <div style={{ fontSize: 16, fontWeight: 700, color: '#e0e6f0' }}>{fmtK(insight.sent)}</div>
                        </div>
                        <div style={{ padding: '8px 14px', borderRadius: 8, background: 'rgba(255,255,255,0.03)', border: '1px solid rgba(0,200,255,0.06)' }}>
                          <div style={{ fontSize: 9, color: '#64748b', textTransform: 'uppercase' }}>Human Opens</div>
                          <div style={{ fontSize: 16, fontWeight: 700, color: '#22c55e' }}>{insight.human_open_rate}%</div>
                        </div>
                      </div>

                      {/* Daily breakdown */}
                      <div style={{ marginBottom: 14 }}>
                        <div style={{ fontSize: 11, fontWeight: 600, color: '#64748b', marginBottom: 6, textTransform: 'uppercase' }}>Daily Breakdown</div>
                        <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 11 }}>
                          <thead>
                            <tr style={{ borderBottom: '1px solid rgba(0,200,255,0.08)' }}>
                              {['Date', 'Sent', 'Delivered', 'Hard', 'Soft', 'Deferred', 'Bounce %'].map(h => (
                                <th key={h} style={{ padding: '4px 8px', textAlign: 'right', color: '#64748b', fontWeight: 600, fontSize: 10 }}>{h}</th>
                              ))}
                            </tr>
                          </thead>
                          <tbody>
                            {insight.daily.map(d => (
                              <tr key={d.date} style={{ borderBottom: '1px solid rgba(0,200,255,0.04)' }}>
                                <td style={{ padding: '4px 8px', color: '#94a3b8' }}>{d.date.slice(5)}</td>
                                <td style={{ padding: '4px 8px', textAlign: 'right', color: '#c0c4d0' }}>{fmtK(d.sent)}</td>
                                <td style={{ padding: '4px 8px', textAlign: 'right', color: '#c0c4d0' }}>{fmtK(d.delivered)}</td>
                                <td style={{ padding: '4px 8px', textAlign: 'right', color: d.hard_bounces > 0 ? '#ef4444' : '#c0c4d0' }}>{d.hard_bounces}</td>
                                <td style={{ padding: '4px 8px', textAlign: 'right', color: d.soft_bounces > 0 ? '#f59e0b' : '#c0c4d0' }}>{d.soft_bounces}</td>
                                <td style={{ padding: '4px 8px', textAlign: 'right', color: d.deferred > 0 ? '#f59e0b' : '#c0c4d0' }}>{d.deferred}</td>
                                <td style={{ padding: '4px 8px', textAlign: 'right', color: d.bounce_rate > 2 ? '#ef4444' : '#c0c4d0' }}>{d.bounce_rate}%</td>
                              </tr>
                            ))}
                          </tbody>
                        </table>
                      </div>

                      {/* Hourly deferral heatmap */}
                      <div style={{ marginBottom: 14 }}>
                        <div style={{ fontSize: 11, fontWeight: 600, color: '#64748b', marginBottom: 6, textTransform: 'uppercase' }}>Hourly Deferral Distribution (UTC)</div>
                        <div style={{ display: 'flex', gap: 2, alignItems: 'flex-end', height: 40 }}>
                          {insight.hourly_deferrals.map((cnt, hr) => (
                            <div
                              key={hr}
                              title={`${hr}:00 — ${cnt} deferrals`}
                              style={{
                                flex: 1, minWidth: 0,
                                height: `${Math.max(4, (cnt / maxDefer) * 40)}px`,
                                background: cnt === 0 ? 'rgba(255,255,255,0.04)' : `rgba(245, 158, 11, ${Math.max(0.2, cnt / maxDefer)})`,
                                borderRadius: 2,
                              }}
                            />
                          ))}
                        </div>
                        <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 8, color: '#4b5563', marginTop: 2 }}>
                          <span>0h</span><span>6h</span><span>12h</span><span>18h</span><span>23h</span>
                        </div>
                      </div>

                      {/* Signals */}
                      {insight.signals.length > 0 && (
                        <div>
                          <div style={{ fontSize: 11, fontWeight: 600, color: '#64748b', marginBottom: 6, textTransform: 'uppercase' }}>Signals</div>
                          {insight.signals.map((sig, i) => (
                            <div key={i} style={{
                              padding: '6px 10px', marginBottom: 4, borderRadius: 6, fontSize: 11, color: '#c0c4d0',
                              background: sig.severity === 'critical' ? 'rgba(239,68,68,0.08)' : sig.severity === 'high' ? 'rgba(245,158,11,0.08)' : 'rgba(0,200,255,0.04)',
                              borderLeft: `3px solid ${sig.severity === 'critical' ? '#ef4444' : sig.severity === 'high' ? '#f59e0b' : '#3b82f6'}`,
                            }}>
                              {sig.detail}
                            </div>
                          ))}
                        </div>
                      )}
                    </div>
                  );
                })()}
              </>
            )}
          </div>
        )}
      </div>

      {/* Skeleton loading */}
      {readinessLoading && ispReadiness.length === 0 && (
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(280px, 1fr))', gap: 12 }}>
          {[1, 2, 3, 4, 5, 6].map(i => (
            <div key={i} style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.06)', borderRadius: 10, padding: 14, height: 130 }}>
              <div style={{ height: 18, width: '60%', background: 'rgba(0,200,255,0.06)', borderRadius: 4, marginBottom: 12, animation: 'igShimmer 1.5s ease infinite' }} />
              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 8 }}>
                {[1, 2, 3, 4].map(j => (
                  <div key={j} style={{ height: 14, background: 'rgba(0,200,255,0.04)', borderRadius: 3, animation: 'igShimmer 1.5s ease infinite', animationDelay: `${j * 0.1}s` }} />
                ))}
              </div>
            </div>
          ))}
        </div>
      )}

      {(!readinessLoading || ispReadiness.length > 0) && (
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(280px, 1fr))', gap: 12 }}>
          {(ispReadiness.length > 0 ? ispReadiness : ALL_ISPS.map(isp => ({ isp, display_name: ISP_META[isp]?.label || isp, health_score: 0, status: 'unknown', active_agents: 0, total_agents: 6, bounce_rate: 0, hard_bounce_rate: 0, soft_bounce_rate: 0, deferral_rate: 0, complaint_rate: 0, warmup_ips: 0, active_ips: 0, quarantined_ips: 0, max_daily_capacity: 0, max_hourly_rate: 0, pool_name: '', has_emergency: false, warnings: [] }))).map((r: any) => {
            const meta = ISP_META[r.isp] || { label: r.display_name, color: '#64748b', emoji: '🌐' };
            const selected = selectedISPs.includes(r.isp);
            return (
              <div
                role="button"
                tabIndex={0}
                aria-pressed={selected}
                aria-label={`Select ${meta.label} provider`}
                key={r.isp}
                className="ig-card-hover"
                onClick={() => toggleISP(r.isp)}
                onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggleISP(r.isp); } }}
                style={{
                  background: selected ? `${meta.color}15` : '#0d1526',
                  border: `2px solid ${selected ? meta.color : 'rgba(0,200,255,0.08)'}`,
                  borderRadius: 10, padding: 14, cursor: 'pointer',
                  transition: 'all 0.25s ease',
                  transform: selected ? 'scale(1.01)' : 'scale(1)',
                  boxShadow: selected ? `0 0 20px ${meta.color}15` : 'none',
                }}
              >
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8 }}>
                  <span style={{ fontSize: 18 }}>{meta.emoji} <strong style={{ color: meta.color }}>{meta.label}</strong></span>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                    {statusBadge(r.status)}
                    {selected && (
                      <span style={{
                        display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
                        width: 20, height: 20, borderRadius: '50%', background: meta.color, color: '#fff', fontSize: 10,
                      }}>
                        <FontAwesomeIcon icon={faCheck} />
                      </span>
                    )}
                  </div>
                </div>
                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '6px 16px', fontSize: 12, color: 'rgba(180,210,240,0.65)' }}>
                  <span>Health: <strong style={{ color: '#e0e6f0' }}>{r.health_score.toFixed(0)}%</strong></span>
                  <span>Agents: <strong style={{ color: '#e0e6f0' }}>{r.active_agents}/{r.total_agents}</strong></span>
                  <span>Active IPs: <strong style={{ color: '#e0e6f0' }}>{r.active_ips}</strong></span>
                  <span>Warmup IPs: <strong style={{ color: '#e0e6f0' }}>{r.warmup_ips}</strong></span>
                  <span>Capacity: <strong style={{ color: '#e0e6f0' }}>{(r.max_daily_capacity / 1000).toFixed(0)}k/day</strong></span>
                  <span>Hard: <strong style={{ color: '#ef4444' }}>{(r.hard_bounce_rate ?? r.bounce_rate ?? 0).toFixed(1)}%</strong> / Soft: <strong style={{ color: '#f59e0b' }}>{(r.soft_bounce_rate ?? 0).toFixed(1)}%</strong></span>
                </div>
                {r.ip_details && r.ip_details.length > 0 && (
                  <div style={{ marginTop: 8, display: 'flex', gap: 4, flexWrap: 'wrap' }}>
                    {r.ip_details.map((ipd: any) => (
                      <span key={ipd.ip} title={`${ipd.ip} — Score: ${ipd.score.toFixed(0)}, Hard: ${(ipd.hard_bounce_rate ?? ipd.bounce_rate ?? 0).toFixed(1)}% / Soft: ${(ipd.soft_bounce_rate ?? 0).toFixed(1)}%, Deferral: ${ipd.deferral_rate.toFixed(1)}%`} style={{
                        display: 'inline-flex', alignItems: 'center', gap: 3, padding: '1px 6px', borderRadius: 3, fontSize: 10, fontFamily: 'monospace',
                        background: ipd.status === 'healthy' ? '#10b98118' : ipd.status === 'throttled' ? '#f59e0b18' : ipd.status === 'blocked' ? '#ef444418' : '#64748b18',
                        color: ipd.status === 'healthy' ? '#10b981' : ipd.status === 'throttled' ? '#f59e0b' : ipd.status === 'blocked' ? '#ef4444' : '#8b8fa3',
                        border: `1px solid ${ipd.status === 'healthy' ? '#10b98130' : ipd.status === 'throttled' ? '#f59e0b30' : ipd.status === 'blocked' ? '#ef444430' : '#64748b30'}`,
                      }}>
                        <span style={{ width: 5, height: 5, borderRadius: '50%', background: 'currentColor' }} />
                        {ipd.ip.split('.').slice(-1)[0]}
                      </span>
                    ))}
                  </div>
                )}
                {(r.blocked_ips > 0 || r.throttled_ips > 0) && (
                  <div style={{ marginTop: 4, fontSize: 11, color: '#8b8fa3' }}>
                    {r.healthy_ips > 0 && <span style={{ color: '#10b981' }}>{r.healthy_ips} healthy</span>}
                    {r.throttled_ips > 0 && <span style={{ color: '#f59e0b', marginLeft: 8 }}>{r.throttled_ips} throttled</span>}
                    {r.blocked_ips > 0 && <span style={{ color: '#ef4444', marginLeft: 8 }}>{r.blocked_ips} blocked</span>}
                  </div>
                )}
                {r.warnings && r.warnings.length > 0 && (
                  <div style={{ marginTop: 8, padding: '6px 8px', background: '#f59e0b15', borderRadius: 6, fontSize: 11, color: '#f59e0b' }}>
                    <FontAwesomeIcon icon={faExclamationTriangle} /> {r.warnings[0]}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}

      {/* Selected ISPs summary - smooth reveal */}
      <div style={{
        maxHeight: selectedISPs.length > 0 ? 60 : 0,
        opacity: selectedISPs.length > 0 ? 1 : 0,
        overflow: 'hidden',
        transition: 'max-height 0.35s ease, opacity 0.3s ease, margin 0.3s ease',
        marginTop: selectedISPs.length > 0 ? 12 : 0,
      }}>
        <div style={{ padding: '8px 12px', background: '#10b98115', borderRadius: 8, fontSize: 13, color: '#10b981' }}>
          <FontAwesomeIcon icon={faCheckCircle} /> {selectedISPs.length} provider{selectedISPs.length > 1 ? 's' : ''} selected: {selectedISPs.map(i => ISP_META[i]?.label || i).join(', ')}
        </div>
      </div>

      {/* Volume Quotas - smooth slide-in */}
      <div style={{
        maxHeight: selectedISPs.length > 0 ? 400 : 0,
        opacity: selectedISPs.length > 0 ? 1 : 0,
        overflow: 'hidden',
        transition: 'max-height 0.4s ease, opacity 0.35s ease, margin 0.3s ease',
        marginTop: selectedISPs.length > 0 ? 16 : 0,
      }}>
        <div style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 14 }}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 8 }}>
            <h4 style={{ margin: 0, fontSize: 13, color: 'rgba(180,210,240,0.65)' }}>
              <FontAwesomeIcon icon={faShieldAlt} /> Volume Quotas <span style={{ fontWeight: 400 }}>(optional)</span>
            </h4>
            <button
              onClick={openDelivRecsModal}
              style={{
                background: 'linear-gradient(135deg, #6366f1 0%, #8b5cf6 100%)',
                border: 'none', borderRadius: 6, padding: '5px 12px',
                color: '#fff', fontSize: 11, fontWeight: 600, cursor: 'pointer',
                display: 'flex', alignItems: 'center', gap: 6,
                transition: 'opacity 0.2s',
              }}
              onMouseEnter={e => (e.currentTarget.style.opacity = '0.85')}
              onMouseLeave={e => (e.currentTarget.style.opacity = '1')}
            >
              <FontAwesomeIcon icon={faBrain} /> Deliverability Recommendations
            </button>
          </div>
          <p style={{ margin: '0 0 8px', fontSize: 11, color: '#64748b' }}>
            Set maximum sends per mailbox provider. <strong>0 means UNLIMITED</strong> — that lane
            mails everyone who qualifies, it is not skipped.
          </p>

          {/* Bulk form action. It rewrites the inputs below and nothing else:
              no new payload field, no change to what is read or displayed. */}
          <div style={{
            marginBottom: 12, borderRadius: 8, padding: '10px 12px',
            background: noQuotaOn ? 'rgba(0,176,255,0.08)' : '#0a0f1a',
            border: `1px solid ${noQuotaOn ? 'rgba(0,176,255,0.45)' : 'rgba(0,200,255,0.10)'}`,
          }}>
            <label style={{ display: 'flex', alignItems: 'flex-start', gap: 9, cursor: 'pointer' }}>
              <input
                type="checkbox"
                checked={noQuotaOn}
                onChange={e => toggleNoQuota(e.target.checked)}
                aria-label="No quota or caps — set every mailbox provider to 0"
                style={{ marginTop: 2, width: 15, height: 15, cursor: 'pointer', accentColor: '#00b0ff' }}
              />
              <span>
                <span style={{ fontSize: 12.5, fontWeight: 600, color: noQuotaOn ? '#38bdf8' : '#e0e6f0' }}>
                  <FontAwesomeIcon icon={faInfinity} style={{ marginRight: 6, fontSize: 11 }} />
                  No quota or caps — send the full audience (sets every provider to 0)
                </span>
                <span style={{ display: 'block', fontSize: 11, color: 'rgba(180,210,240,0.6)', marginTop: 4, lineHeight: 1.55 }}>
                  On this platform a per-provider volume of <strong>0 means AUDIENCE-BOUND</strong>:
                  that lane mails <strong>everyone who qualifies</strong>. It does <strong>not</strong> mean
                  zero sends. The caps themselves are still read and still shown — this only rewrites
                  the numbers in the boxes below, and they stay editable.
                </span>
              </span>
            </label>

            {quotaSnapshot !== null && (
              <div style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.5)', marginTop: 8, paddingLeft: 24 }}>
                {noQuotaOn
                  ? 'Turning this off restores the caps that were in the form before it was switched on.'
                  : 'A provider has been edited back to a real cap, so this is no longer "no quota". '
                    + 'Re-check it to zero the form again from the values showing now.'}
              </div>
            )}

            {audienceBound && (
              <div style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.5)', marginTop: 6, paddingLeft: 24 }}>
                &ldquo;Mail the whole selected audience&rdquo; is already on at the Audience step, which
                sends 0 for every lane regardless of these boxes — this toggle just makes the form
                agree with it.
              </div>
            )}

            {warmupActive && (
              <div style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.5)', marginTop: 6, paddingLeft: 24 }}>
                Warm-up: this does not touch the cold pad quota on the Audience step — that number is
                a count of records to pull, not a cap, so 0 there means &ldquo;no cold pad&rdquo;.
              </div>
            )}
          </div>
          {/* Explicit confirmation, not a footnote: an operator typing 0 must
              see what 0 does BEFORE the review step. */}
          {(() => {
            const zeroLanes = [...selectedISPs, 'other'].filter(
              (isp, i, arr) => arr.indexOf(isp) === i && !(ispQuotas[isp] > 0));
            if (zeroLanes.length === 0) return null;
            return (
              <div style={{
                marginBottom: 12, padding: '8px 12px', borderRadius: 8, fontSize: 12, lineHeight: 1.5,
                background: 'rgba(245,158,11,0.08)', border: '1px solid rgba(245,158,11,0.45)', color: '#fbbf24',
              }}>
                <FontAwesomeIcon icon={faInfinity} />{' '}
                <strong>Uncapped:</strong>{' '}
                {zeroLanes.map(i => ISP_META[i]?.label || i).join(', ')}{' '}
                {zeroLanes.length === 1 ? 'is' : 'are'} at 0 — {zeroLanes.length === 1 ? 'that lane' : 'those lanes'}{' '}
                will send to the ENTIRE qualifying audience with no ceiling.
              </div>
            );
          })()}
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(200px, 1fr))', gap: 8 }}>
            {selectedISPs.map(isp => {
              const meta = ISP_META[isp] || { label: isp, color: '#64748b', emoji: '🌐' };
              return (
                <div key={isp} style={{
                  display: 'flex', alignItems: 'center', gap: 8,
                  padding: '8px 12px', background: '#0a0f1a', borderRadius: 8,
                  border: `1px solid ${meta.color}25`,
                  transition: 'border-color 0.2s, box-shadow 0.2s',
                }}>
                  <span style={{ fontSize: 12, color: meta.color, minWidth: 80, fontWeight: 500 }}>{meta.emoji} {meta.label}</span>
                  <input
                    type="number" min={0} step={1000}
                    value={ispQuotas[isp] || 0}
                    onChange={e => setISPQuotas(prev => ({ ...prev, [isp]: Number(e.target.value) }))}
                    style={{ flex: 1, width: 80, background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 4, color: '#e0e6f0', padding: '4px 8px', fontSize: 12, textAlign: 'right' }}
                  />
                </div>
              );
            })}
            <div style={{
              display: 'flex', alignItems: 'center', gap: 8,
              padding: '8px 12px', background: '#0a0f1a', borderRadius: 8,
              border: '1px solid #64748b25',
              gridColumn: '1 / -1',
            }}>
              <span style={{ fontSize: 12, color: '#94a3b8', minWidth: 80, fontWeight: 500 }}>🌐 Everything Else</span>
              <input
                type="number" min={0} step={100}
                value={ispQuotas['other'] || 0}
                onChange={e => setISPQuotas(prev => ({ ...prev, other: Number(e.target.value) }))}
                style={{ flex: 1, width: 80, background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 4, color: '#e0e6f0', padding: '4px 8px', fontSize: 12, textAlign: 'right' }}
              />
              <span style={{ fontSize: 10, color: '#64748b' }}>Domains not matching any provider above</span>
            </div>
            {Object.values(ispQuotas).some(v => v > 0) && (
              <div style={{ gridColumn: '1 / -1', fontSize: 12, color: '#10b981', padding: '4px 0', fontWeight: 600 }}>
                Total quota: {Object.values(ispQuotas).filter(v => v > 0).reduce((a, b) => a + b, 0).toLocaleString()}
              </div>
            )}
          </div>
        </div>
      </div>

      {/* Deliverability Recommendations Modal */}
      {delivRecsOpen && (
        <div style={{
          position: 'fixed', inset: 0, zIndex: 9999,
          background: 'rgba(0,0,0,0.7)', backdropFilter: 'blur(4px)',
          display: 'flex', alignItems: 'center', justifyContent: 'center',
        }} onClick={() => setDelivRecsOpen(false)}>
          <div style={{
            background: '#0d1526', border: '1px solid rgba(100,130,255,0.18)',
            borderRadius: 14, padding: 24, width: '100%', maxWidth: 600,
            maxHeight: '80vh', overflowY: 'auto',
            boxShadow: '0 20px 60px rgba(0,0,0,0.6)',
          }} onClick={e => e.stopPropagation()}>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
              <h3 style={{ margin: 0, fontSize: 16, color: '#e0e6f0' }}>
                <FontAwesomeIcon icon={faBrain} style={{ color: '#8b5cf6', marginRight: 8 }} />
                Deliverability Recommendations
              </h3>
              <button onClick={() => setDelivRecsOpen(false)} style={{
                background: 'none', border: 'none', color: '#64748b', fontSize: 18, cursor: 'pointer',
              }}>&times;</button>
            </div>

            <div style={{ marginBottom: 16 }}>
              <label style={{ fontSize: 12, color: 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 6 }}>
                Sending Domain
              </label>
              <select
                value={delivRecsDomain}
                onChange={e => { setDelivRecsDomain(e.target.value); setDelivRecsResult(null); setDelivRecsError(''); }}
                style={{
                  width: '100%', background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.12)',
                  borderRadius: 8, color: '#e0e6f0', padding: '10px 12px', fontSize: 13,
                }}
              >
                <option value="">-- Select a domain --</option>
                {delivRecsDomains.map(d => (
                  <option key={d.domain} value={d.domain}>{d.domain}</option>
                ))}
              </select>
            </div>

            <button
              onClick={fetchDeliverabilityRecs}
              disabled={!delivRecsDomain || delivRecsLoading}
              style={{
                width: '100%', padding: '10px 0', borderRadius: 8, border: 'none',
                background: delivRecsDomain ? 'linear-gradient(135deg, #6366f1 0%, #8b5cf6 100%)' : '#1e293b',
                color: delivRecsDomain ? '#fff' : '#475569',
                fontSize: 13, fontWeight: 600, cursor: delivRecsDomain ? 'pointer' : 'not-allowed',
                marginBottom: 16, transition: 'all 0.2s',
              }}
            >
              {delivRecsLoading ? 'Analyzing 3-day data...' : 'Provide Recommendations'}
            </button>

            {delivRecsLoading && (
              <div style={{ textAlign: 'center', padding: 20 }}>
                <div style={{
                  width: 32, height: 32, border: '3px solid #1e293b', borderTopColor: '#8b5cf6',
                  borderRadius: '50%', margin: '0 auto 12px',
                  animation: 'spin 0.8s linear infinite',
                }} />
                <p style={{ fontSize: 12, color: '#64748b', margin: 0 }}>
                  AI is reviewing your provider sending history...
                </p>
                <style>{`@keyframes spin { to { transform: rotate(360deg); } }`}</style>
              </div>
            )}

            {delivRecsError && (
              <div style={{ padding: 12, background: '#ef444420', borderRadius: 8, color: '#ef4444', fontSize: 12, marginBottom: 12 }}>
                {delivRecsError}
              </div>
            )}

            {delivRecsResult && !delivRecsLoading && (
              <div>
                {delivRecsResult.overall_summary && (
                  <div style={{
                    padding: 12, background: '#1e293b', borderRadius: 8, marginBottom: 14,
                    fontSize: 12, color: '#cbd5e1', lineHeight: 1.6,
                    borderLeft: '3px solid #8b5cf6',
                  }}>
                    {delivRecsResult.overall_summary}
                  </div>
                )}

                {delivRecsResult.cautions?.length > 0 && (
                  <div style={{ marginBottom: 14 }}>
                    {delivRecsResult.cautions.map((c: string, i: number) => (
                      <div key={i} style={{
                        padding: '6px 10px', background: '#f59e0b15', borderRadius: 6,
                        fontSize: 11, color: '#f59e0b', marginBottom: 4,
                      }}>
                        ⚠ {c}
                      </div>
                    ))}
                  </div>
                )}

                {delivRecsResult.recommendations?.length > 0 && (
                  <div style={{ display: 'grid', gap: 8, marginBottom: 16 }}>
                    {delivRecsResult.recommendations.map((rec: any) => {
                      const meta = ISP_META[rec.isp] || { label: rec.isp, color: '#64748b', emoji: '🌐' };
                      const riskColor = rec.risk_level === 'high' ? '#ef4444' : rec.risk_level === 'medium' ? '#f59e0b' : '#22c55e';
                      return (
                        <div key={rec.isp} style={{
                          padding: 12, background: '#0a0f1a', borderRadius: 8,
                          border: `1px solid ${meta.color}30`,
                        }}>
                          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 6 }}>
                            <span style={{ fontSize: 13, fontWeight: 600, color: meta.color }}>
                              {meta.emoji} {meta.label}
                            </span>
                            <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                              <span style={{
                                fontSize: 10, padding: '2px 6px', borderRadius: 4,
                                background: riskColor + '20', color: riskColor, fontWeight: 600,
                              }}>
                                {rec.risk_level?.toUpperCase()}
                              </span>
                              <span style={{
                                fontSize: 14, fontWeight: 700, color: '#e0e6f0',
                                fontFamily: 'monospace',
                              }}>
                                {rec.suggested_quota?.toLocaleString()}
                              </span>
                            </div>
                          </div>
                          <div style={{ fontSize: 11, color: '#94a3b8', lineHeight: 1.5 }}>
                            {rec.rationale}
                          </div>
                          {rec.trend && (
                            <span style={{
                              display: 'inline-block', marginTop: 4, fontSize: 10, padding: '1px 6px',
                              borderRadius: 4, background: '#1e293b', color: '#64748b',
                            }}>
                              Trend: {rec.trend}
                            </span>
                          )}
                        </div>
                      );
                    })}
                  </div>
                )}

                {delivRecsResult.recommendations?.length > 0 && (
                  <button
                    onClick={applyAllDelivRecs}
                    style={{
                      width: '100%', padding: '10px 0', borderRadius: 8, border: 'none',
                      background: 'linear-gradient(135deg, #22c55e 0%, #10b981 100%)',
                      color: '#fff', fontSize: 13, fontWeight: 600, cursor: 'pointer',
                    }}
                  >
                    Apply All Quotas
                  </button>
                )}

                {delivRecsResult.recommendations?.length === 0 && !delivRecsResult.overall_summary && (
                  <div style={{ textAlign: 'center', padding: 16, color: '#64748b', fontSize: 12 }}>
                    No recommendations available for this domain.
                  </div>
                )}
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  );

  // ── Newsletters branch: shared bits ──────────────────────────────────────

  // Readiness is never a colour alone: the word is always present, and a
  // status the server did not state is labelled as locally derived.
  const NewsletterReadiness: React.FC<{ status: NewsletterStatus; stated: boolean }> = ({ status, stated }) => (
    <span
      title={stated ? 'Readiness stated by the preview endpoint' : 'The endpoint did not state a status — derived from creative_id and updated_at'}
      style={{
        display: 'inline-block', padding: '2px 8px', borderRadius: 999,
        fontSize: 10.5, fontWeight: 700, letterSpacing: 0.4, textTransform: 'uppercase',
        color: NEWSLETTER_STATUS_COLORS[status],
        background: `${NEWSLETTER_STATUS_COLORS[status]}1f`,
        border: `1px solid ${NEWSLETTER_STATUS_COLORS[status]}66`,
        whiteSpace: 'nowrap',
      }}
    >
      {status}{stated ? '' : '*'}
    </span>
  );

  const newsletterRosterHeader = (extra?: React.ReactNode) => (
    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: 10, flexWrap: 'wrap', marginBottom: 10 }}>
      <h4 style={{ margin: 0, fontSize: 13, color: '#e0e6f0' }}>
        <FontAwesomeIcon icon={faNewspaper} style={{ marginRight: 6, fontSize: 11, color: '#38bdf8' }} />
        Today’s newsletters{newsletterDay ? ` — ${newsletterDay}` : ''}
      </h4>
      <span style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
        {extra}
        <span style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.4)' }}>
          {newsletterLoading ? 'fetching…' : newsletterFetchedAt ? `fetched ${newsletterFetchedAt}` : ''}
        </span>
        <button type="button" onClick={() => setNewsletterKey(k => k + 1)} disabled={newsletterLoading}
          style={{ background: 'transparent', border: '1px solid rgba(0,200,255,0.18)', color: '#00b0ff', borderRadius: 6, padding: '3px 10px', fontSize: 11, cursor: newsletterLoading ? 'default' : 'pointer' }}>
          <FontAwesomeIcon icon={faRotate} spin={newsletterLoading} /> Refresh
        </button>
      </span>
    </div>
  );

  // Four distinct displays. A failed fetch must never read as "no newsletters
  // today" and a domain with no creative must never read as an empty row.
  const renderNewsletterRosterState = (): React.ReactNode => {
    if (newsletterError) {
      return (
        <SectionError
          label="Newsletter roster"
          error={`${newsletterError} — today’s newsletters are UNKNOWN, not absent. Nothing can be audited or queued until this loads.`}
          onRetry={() => setNewsletterKey(k => k + 1)}
        />
      );
    }
    if (newsletterLoading && newsletterRows === null) {
      return (
        <div style={{ fontSize: 12, color: '#7dd3fc', padding: '10px 0' }}>
          <FontAwesomeIcon icon={faSpinner} spin /> Loading today’s newsletter per sending domain…
        </div>
      );
    }
    if (newsletterRows !== null && newsletterRows.length === 0) {
      return (
        <div style={{ background: '#0d1526', border: '1px solid rgba(245,158,11,0.4)', borderRadius: 10, padding: 4 }}>
          <EmptyState
            icon={faNewspaper}
            title="The roster returned no eligible sending domains"
            hint="The endpoint answered successfully with an empty domain list — that is 'built empty', not 'never built'. No newsletter can be scheduled today until the daily registration has run."
          />
        </div>
      );
    }
    return null;
  };

  // Step 1: the compact PICK list. Readiness is visible here so the operator
  // never carries a not-ready domain forward without seeing it.
  const renderNewsletterRoster = () => {
    const state = renderNewsletterRosterState();
    return (
      <div style={{ background: '#0d1526', border: '1px solid rgba(56,189,248,0.25)', borderRadius: 10, padding: 14, marginBottom: 18 }}>
        {newsletterRosterHeader(
          newsletterView.length > 0 ? (
            <>
              <button type="button"
                onClick={() => { setNewsletterExcluded(new Set()); setNewsletterStaleOptIn(new Set()); }}
                style={{ background: 'transparent', border: 'none', color: '#00b0ff', fontSize: 11, cursor: 'pointer' }}>
                include all ready
              </button>
              <button type="button"
                onClick={() => setNewsletterExcluded(new Set(newsletterView.map(v => v.row.sending_domain)))}
                style={{ background: 'transparent', border: 'none', color: '#00b0ff', fontSize: 11, cursor: 'pointer' }}>
                clear
              </button>
            </>
          ) : null,
        )}

        <div style={{ fontSize: 11.5, color: 'rgba(180,210,240,0.6)', lineHeight: 1.55, marginBottom: 10 }}>
          Content is <strong style={{ color: '#e0e6f0' }}>automatic</strong> — one newsletter is generated and
          registered per sending domain each day. This mode never picks a creative; it audits what the
          pipeline produced. Full subject / preheader / from-name / body audit is the next step.
        </div>

        {state || (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
            {newsletterView.map(v => {
              const col = NEWSLETTER_STATUS_COLORS[v.status];
              return (
                <div key={v.row.sending_domain}
                  style={{
                    background: '#0a0f1a', border: `1px solid ${v.included ? 'rgba(0,200,255,0.18)' : `${col}44`}`,
                    borderRadius: 8, padding: '9px 11px',
                  }}>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
                    <input
                      type="checkbox"
                      aria-label={`Include ${v.row.sending_domain}`}
                      checked={v.included}
                      disabled={v.blocked}
                      onChange={e => {
                        const on = e.target.checked;
                        setNewsletterExcluded(prev => {
                          const next = new Set(prev);
                          if (on) next.delete(v.row.sending_domain); else next.add(v.row.sending_domain);
                          return next;
                        });
                        if (v.status === 'stale') {
                          setNewsletterStaleOptIn(prev => {
                            const next = new Set(prev);
                            if (on) next.add(v.row.sending_domain); else next.delete(v.row.sending_domain);
                            return next;
                          });
                        }
                      }}
                      style={{ width: 15, height: 15, cursor: v.blocked ? 'not-allowed' : 'pointer' }}
                    />
                    <span style={{ fontSize: 12.5, color: '#e0e6f0', fontFamily: 'monospace' }}>{v.row.sending_domain}</span>
                    <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.55)' }}>
                      {v.row.from_name || <span style={{ color: '#f59e0b' }}>no from-name</span>}
                    </span>
                    <span style={{ marginLeft: 'auto' }}><NewsletterReadiness status={v.status} stated={v.statusStated} /></span>
                  </div>
                  {v.status !== 'ready' && (
                    <div style={{ fontSize: 11, color: col, marginTop: 6, lineHeight: 1.5, paddingLeft: 25 }}>
                      <FontAwesomeIcon icon={faExclamationTriangle} style={{ fontSize: 10 }} /> {v.reason}
                      {v.status === 'stale' && !v.staleOptIn && ' Tick the box to queue it anyway.'}
                      {v.blocked && ' It cannot be included.'}
                    </div>
                  )}
                </div>
              );
            })}
          </div>
        )}

        {newsletterView.length > 0 && (
          <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)', marginTop: 10, fontVariantNumeric: 'tabular-nums' }}>
            {newsletterTally.included} included of {newsletterTally.total} ·{' '}
            <span style={{ color: NEWSLETTER_STATUS_COLORS.ready }}>{newsletterTally.ready} ready</span> ·{' '}
            <span style={{ color: NEWSLETTER_STATUS_COLORS.stale }}>{newsletterTally.stale} stale</span> ·{' '}
            <span style={{ color: NEWSLETTER_STATUS_COLORS.missing }}>{newsletterTally.missing} missing</span>
            {newsletterTally.derived > 0 && (
              <span style={{ color: '#f59e0b' }}> · {newsletterTally.derived} marked * (status derived here, not stated by the endpoint)</span>
            )}
          </div>
        )}
      </div>
    );
  };

  // Step 3: the AUDIT. Everything that will actually reach an inbox, per
  // domain, in one table — this is the screen the operator signs off on.
  const renderNewsletterStep3 = () => {
    const state = renderNewsletterRosterState();
    const th: React.CSSProperties = {
      textAlign: 'left', padding: '7px 10px', fontSize: 10.5, textTransform: 'uppercase',
      letterSpacing: 0.5, color: 'rgba(180,210,240,0.5)', borderBottom: '1px solid rgba(0,200,255,0.10)',
      whiteSpace: 'nowrap',
    };
    const td: React.CSSProperties = {
      padding: '9px 10px', fontSize: 12, color: '#e0e6f0', verticalAlign: 'top',
      borderBottom: '1px solid rgba(0,200,255,0.06)',
    };
    return (
      <div className="wiz-step-content ig-fade-in">
        <div style={{ marginBottom: 16 }}>
          <h3 style={{ margin: 0 }}>
            <FontAwesomeIcon icon={faNewspaper} style={{ marginRight: 8, color: '#38bdf8' }} />
            Newsletter audit
          </h3>
          <p style={{ margin: '4px 0 0', color: 'rgba(180,210,240,0.65)', fontSize: 13, lineHeight: 1.6 }}>
            Exactly what will mail from each sending domain: friendly-from, subject, preheader, and the
            registered body. Nothing here is editable — content is generated per domain daily and this
            screen exists so it can be <strong>audited</strong> before it goes out.
          </p>
        </div>
        <StepErrorBanner stepNum={3} />

        {/* Readiness first. Absence of a creative is the most important thing
            this screen can say, so it leads. */}
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: 10, marginBottom: 16 }}>
          {([
            { label: 'Included in this run', value: newsletterTally.included, color: '#00b0ff', sub: `of ${newsletterTally.total} eligible domains` },
            { label: 'Ready', value: newsletterTally.ready, color: NEWSLETTER_STATUS_COLORS.ready, sub: 'creative registered and fresh' },
            { label: 'Stale', value: newsletterTally.stale, color: NEWSLETTER_STATUS_COLORS.stale, sub: 'not re-registered in 24h — excluded unless opted in' },
            { label: 'Missing', value: newsletterTally.missing, color: NEWSLETTER_STATUS_COLORS.missing, sub: 'no creative row at all — cannot be included' },
          ]).map(t => (
            <div key={t.label} style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 12 }}>
              <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.6)', marginBottom: 4 }}>{t.label}</div>
              <div style={{ fontSize: 22, fontWeight: 700, color: t.color, fontVariantNumeric: 'tabular-nums' }}>{t.value}</div>
              <div style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.45)', marginTop: 3, lineHeight: 1.4 }}>{t.sub}</div>
            </div>
          ))}
        </div>

        <div style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 14 }}>
          {newsletterRosterHeader()}
          {state || (
            <div style={{ overflowX: 'auto' }}>
              <table style={{ width: '100%', borderCollapse: 'collapse', minWidth: 900 }}>
                <thead>
                  <tr>
                    <th style={th}>Sending domain</th>
                    <th style={th}>Friendly from</th>
                    <th style={th}>Subject</th>
                    <th style={th}>Preheader</th>
                    <th style={th}>Freshness (updated_at)</th>
                    <th style={th}>Readiness</th>
                    <th style={th}></th>
                  </tr>
                </thead>
                <tbody>
                  {newsletterView.map(v => {
                    const r = v.row;
                    const fresh = fmtFreshness(r.updated_at);
                    const col = NEWSLETTER_STATUS_COLORS[v.status];
                    const dim = v.included ? 1 : 0.55;
                    return (
                      <React.Fragment key={r.sending_domain}>
                        <tr style={{ opacity: dim }}>
                          <td style={{ ...td, fontFamily: 'monospace', fontSize: 11.5 }}>
                            {r.sending_domain}
                            <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.4)', marginTop: 3 }}>
                              {r.filename || '—'}
                            </div>
                            <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.35)' }} title={r.creative_sha256 || ''}>
                              sha256 {r.creative_sha256 ? `${r.creative_sha256.slice(0, 12)}…` : '—'}
                            </div>
                          </td>
                          <td style={td}>
                            {r.from_name
                              ? <strong>{r.from_name}</strong>
                              : <span style={{ color: '#f59e0b' }}>no from-name on the sending profile</span>}
                            <div style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.45)', marginTop: 3, fontFamily: 'monospace' }}>
                              {r.from_email || '—'}
                            </div>
                          </td>
                          <td style={{ ...td, maxWidth: 240 }} title={r.subject || ''}>
                            {r.subject || <span style={{ color: '#f59e0b' }}>no subject</span>}
                          </td>
                          <td style={{ ...td, maxWidth: 240, color: 'rgba(180,210,240,0.75)' }} title={r.preheader || ''}>
                            {r.preheader || <span style={{ color: '#f59e0b' }}>no preheader</span>}
                          </td>
                          <td style={{ ...td, fontSize: 11, color: v.status === 'stale' ? '#f59e0b' : 'rgba(180,210,240,0.7)' }}>
                            {fresh.text}
                            {r.approval_status && (
                              <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.4)', marginTop: 3 }}>
                                approval: {r.approval_status}
                              </div>
                            )}
                          </td>
                          <td style={td}><NewsletterReadiness status={v.status} stated={v.statusStated} /></td>
                          <td style={td}>
                            <button type="button"
                              onClick={() => openNewsletterPreview(r.sending_domain)}
                              disabled={!r.creative_id}
                              title={r.creative_id ? 'Render the registered body' : 'There is no creative to preview'}
                              style={{
                                background: 'transparent', border: '1px solid rgba(0,200,255,0.18)',
                                color: r.creative_id ? '#00b0ff' : '#475569', borderRadius: 6,
                                padding: '3px 10px', fontSize: 11, cursor: r.creative_id ? 'pointer' : 'not-allowed',
                                whiteSpace: 'nowrap',
                              }}>
                              <FontAwesomeIcon icon={faEye} /> Preview
                            </button>
                          </td>
                        </tr>
                        {v.status !== 'ready' && (
                          <tr>
                            <td colSpan={7} style={{ padding: '0 10px 9px', borderBottom: '1px solid rgba(0,200,255,0.06)' }}>
                              <div style={{
                                fontSize: 11, color: col, lineHeight: 1.5,
                                background: `${col}12`, border: `1px solid ${col}44`, borderRadius: 6, padding: '7px 9px',
                              }}>
                                <FontAwesomeIcon icon={faExclamationTriangle} style={{ fontSize: 10 }} />{' '}
                                <strong>{v.included ? 'Included anyway' : 'Not in this run'}</strong> — {v.reason}
                              </div>
                            </td>
                          </tr>
                        )}
                      </React.Fragment>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
        </div>

        {newsletterPreviewDomain && (
          <div onClick={() => setNewsletterPreviewDomain('')}
               style={{ position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.75)', zIndex: 9999, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 24 }}>
            <div onClick={e => e.stopPropagation()}
                 style={{ background: '#fff', borderRadius: 10, width: 'min(760px, 100%)', height: '85vh', overflow: 'hidden', display: 'flex', flexDirection: 'column' }}>
              <div style={{ padding: '8px 12px', background: '#0d1526', color: '#e0e6f0', fontSize: 12, display: 'flex', justifyContent: 'space-between', gap: 10 }}>
                <span>{newsletterPreviewDomain} — {fmtFreshness((newsletterRows || []).find(r => r.sending_domain === newsletterPreviewDomain)?.updated_at).text}</span>
                <button onClick={() => setNewsletterPreviewDomain('')} style={{ background: 'transparent', border: 'none', color: '#00b0ff', cursor: 'pointer' }}>close</button>
              </div>
              {newsletterPreviewLoading ? (
                <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', color: '#0f172a', fontSize: 13 }}>
                  <FontAwesomeIcon icon={faSpinner} spin /> &nbsp;Loading the newsletter body…
                </div>
              ) : newsletterPreviewError ? (
                <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 24, color: '#b91c1c', fontSize: 13, textAlign: 'center' }}>
                  Could not load the body: {newsletterPreviewError}
                </div>
              ) : newsletterPreviewHtml ? (
                <iframe title={`${newsletterPreviewDomain} newsletter preview`} srcDoc={newsletterPreviewHtml} sandbox="" style={{ flex: 1, border: 'none', background: '#fff' }} />
              ) : (
                <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 24, color: '#b45309', fontSize: 13, textAlign: 'center' }}>
                  The preview endpoint returned this newsletter’s metadata but no HTML body, so there is
                  nothing to render. This is a blank preview, not a blank creative.
                </div>
              )}
            </div>
          </div>
        )}
      </div>
    );
  };

  // Step 4: the AUDIENCE. Preserved deliberately — "all engagement" is the
  // typical choice, never an automatic one.
  const renderNewsletterStep4 = () => (
    <div className="wiz-step-content ig-fade-in">
      <div style={{ marginBottom: 16 }}>
        <h3 style={{ margin: 0 }}>
          <FontAwesomeIcon icon={faUsers} style={{ marginRight: 8, color: '#38bdf8' }} />
          Engagement audience<RequiredDot />
        </h3>
        <p style={{ margin: '4px 0 0', color: 'rgba(180,210,240,0.65)', fontSize: 13, lineHeight: 1.6 }}>
          One posture, applied to every included sending domain and resolved to that domain’s own
          engaged-anchor segments. Brands are separate senders — no segment ever crosses a property.
        </p>
      </div>
      <StepErrorBanner stepNum={4} />

      <div style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 14, marginBottom: 16 }}>
        <h4 style={{ margin: '0 0 10px', fontSize: 13, color: '#e0e6f0' }}>Engagement posture</h4>
        <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
          {NEWSLETTER_POSTURES.map(pt => {
            const on = newsletterPosture === pt.id;
            return (
              <button key={pt.id} type="button" onClick={() => setNewsletterPosture(pt.id)} title={pt.hint}
                style={{
                  flex: 1, minWidth: 200, textAlign: 'left', padding: '10px 12px', borderRadius: 8,
                  cursor: 'pointer', color: '#e0e6f0',
                  background: on ? 'rgba(0,176,255,0.12)' : '#0a0f1a',
                  border: `1.5px solid ${on ? '#00b0ff' : 'rgba(0,200,255,0.08)'}`,
                }}>
                <div style={{ fontSize: 12.5, fontWeight: 600, color: on ? '#00b0ff' : '#e0e6f0' }}>{pt.label}</div>
                <div style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.55)', marginTop: 4, lineHeight: 1.45 }}>{pt.hint}</div>
              </button>
            );
          })}
        </div>
        <div style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.45)', marginTop: 10, lineHeight: 1.5 }}>
          Clicker / opener is read from the SEGMENT NAME (the engaged grid names them
          “&lt;BRAND&gt; 30D Clickers” / “&lt;BRAND&gt; 7D Openers”) — it is the only classifier this
          endpoint carries. Anything the name does not identify is counted in the “other engaged”
          column below rather than dropped silently, and “All engagement” includes it.
        </div>
      </div>

      {/* Volume posture. Same doctrine, same controls, same words as the offer
          flow — capping an engaged tier is the exception. */}
      <div style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 14, marginBottom: 16 }}>
        <label style={{ display: 'flex', alignItems: 'flex-start', gap: 8, cursor: 'pointer' }}>
          <input type="checkbox" checked={audienceBound}
                 onChange={e => { setAudienceBound(e.target.checked); if (e.target.checked) setMasterTopUp(false); }}
                 style={{ marginTop: 2, width: 15, height: 15, cursor: 'pointer' }} />
          <span style={{ fontSize: 12, color: 'rgba(180,210,240,0.8)' }}>
            <strong style={{ color: '#e0e6f0' }}>Mail the whole selected audience</strong> — no per-ISP cap
            (volume 0 = audience-bound). Segment-sourced sends are audience-bound by standing doctrine;
            the per-provider quotas on the Mailbox Providers step are ignored while it is on.
          </span>
        </label>
        {audienceBound && (
          <div style={{
            marginTop: 10, padding: '10px 12px', borderRadius: 8, fontSize: 12, lineHeight: 1.55,
            background: 'rgba(245,158,11,0.10)', border: '1px solid rgba(245,158,11,0.5)', color: '#fbbf24',
          }}>
            <FontAwesomeIcon icon={faInfinity} />{' '}
            <strong>UNCAPPED — every per-provider quota is 0, for every included domain.</strong>{' '}
            Each of the {newsletterIncluded.length} included sending domain
            {newsletterIncluded.length === 1 ? '' : 's'} will mail its entire qualifying engaged audience
            across {selectedISPs.length} selected provider{selectedISPs.length === 1 ? '' : 's'} — there is
            no ceiling to stop it.
          </div>
        )}
        {/* use_master_selection is NOT a field on the build-request ledger.
            Saying so is the honest display: the operator must know this wizard
            cannot set it here, rather than assuming the offer flow's control
            applied. */}
        <div style={{ marginTop: 10, fontSize: 11.5, color: 'rgba(180,210,240,0.6)', lineHeight: 1.55 }}>
          <FontAwesomeIcon icon={faShieldAlt} style={{ fontSize: 10, color: '#38bdf8' }} />{' '}
          <strong style={{ color: '#e0e6f0' }}>Master-list top-up is not part of a build request.</strong>{' '}
          The <span style={{ fontFamily: 'monospace' }}>use_master_selection</span> column defaults TRUE in
          the database and, combined with an uncapped segment audience, tops up from the whole sending
          domain. This request carries the anchor segments listed below and nothing else — if a build ever
          comes back larger than these numbers, that column is why.
        </div>
      </div>

      <div style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 14 }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: 10, flexWrap: 'wrap', marginBottom: 10 }}>
          <h4 style={{ margin: 0, fontSize: 13, color: '#e0e6f0' }}>Resolved audience per sending domain</h4>
          <button type="button" onClick={() => setNewsletterAnchorsKey(k => k + 1)} disabled={newsletterAnchorsLoading}
            style={{ background: 'transparent', border: '1px solid rgba(0,200,255,0.18)', color: '#00b0ff', borderRadius: 6, padding: '3px 10px', fontSize: 11, cursor: newsletterAnchorsLoading ? 'default' : 'pointer' }}>
            <FontAwesomeIcon icon={faRotate} spin={newsletterAnchorsLoading} /> Re-resolve
          </button>
        </div>

        {newsletterIncluded.length === 0 ? (
          <div style={{ fontSize: 12, color: '#f59e0b', padding: '8px 0' }}>
            No sending domain is included — go back to step 1 and include at least one.
          </div>
        ) : newsletterAnchorsLoading ? (
          <div style={{ fontSize: 12, color: '#7dd3fc', padding: '8px 0' }}>
            <FontAwesomeIcon icon={faSpinner} spin /> Resolving engaged anchors for {newsletterIncluded.length} domains…
          </div>
        ) : (
          <div style={{ overflowX: 'auto' }}>
            <table style={{ width: '100%', borderCollapse: 'collapse', minWidth: 720 }}>
              <thead>
                <tr>
                  {['Sending domain', 'Segments in posture', 'Clickers', 'Openers', 'Other engaged', 'Known subscribers', 'State'].map(h => (
                    <th key={h} style={{
                      textAlign: h === 'Sending domain' || h === 'State' ? 'left' : 'right',
                      padding: '7px 10px', fontSize: 10.5, textTransform: 'uppercase', letterSpacing: 0.5,
                      color: 'rgba(180,210,240,0.5)', borderBottom: '1px solid rgba(0,200,255,0.10)', whiteSpace: 'nowrap',
                    }}>{h}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {newsletterIncluded.map(v => {
                  const d = v.row.sending_domain;
                  const a = newsletterAudience[d];
                  const err = newsletterAnchorErrors[d];
                  const picked = a?.picked || [];
                  let known = 0; let unknown = 0;
                  picked.forEach(sg => { const c = warmupSegmentCount(sg); if (c.known) known += c.value; else unknown += 1; });
                  const numTd: React.CSSProperties = {
                    padding: '9px 10px', fontSize: 12, textAlign: 'right', color: '#e0e6f0',
                    fontVariantNumeric: 'tabular-nums', borderBottom: '1px solid rgba(0,200,255,0.06)',
                  };
                  return (
                    <tr key={d}>
                      <td style={{ padding: '9px 10px', fontSize: 11.5, fontFamily: 'monospace', color: '#e0e6f0', borderBottom: '1px solid rgba(0,200,255,0.06)' }}>
                        {d}
                        <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.4)', marginTop: 3, maxWidth: 260, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}
                             title={picked.map(sg => sg.name).join(', ')}>
                          {picked.map(sg => sg.name).join(', ') || '—'}
                        </div>
                      </td>
                      <td style={numTd}>{picked.length} of {a?.total ?? 0}</td>
                      <td style={numTd}>{a?.kinds.clickers ?? 0}</td>
                      <td style={numTd}>{a?.kinds.openers ?? 0}</td>
                      <td style={numTd}>{a?.kinds.other ?? 0}</td>
                      <td style={numTd}>
                        {known.toLocaleString()}{unknown > 0 ? '+' : ''}
                        {unknown > 0 && (
                          <div style={{ fontSize: 10, color: '#f59e0b' }} title="subscriber_count is zeroed when a segment refresh times out — 0 is UNKNOWN, not zero">
                            {unknown} report no count
                          </div>
                        )}
                      </td>
                      <td style={{ padding: '9px 10px', fontSize: 11, borderBottom: '1px solid rgba(0,200,255,0.06)' }}>
                        {err
                          ? <span style={{ color: '#ef4444' }}>UNKNOWN — {err}</span>
                          : picked.length === 0
                            ? <span style={{ color: '#ef4444' }}>NO AUDIENCE under this posture</span>
                            : <span style={{ color: '#10b981' }}>resolved</span>}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}

        <div style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.4)', marginTop: 10, lineHeight: 1.5 }}>
          These are the segments each request will carry. “Known subscribers” sums only the segments that
          report a count — a segment refresh that timed out writes 0, so a 0 there is UNKNOWN, never a
          confident zero, and is counted separately rather than folded in.
        </div>
      </div>
    </div>
  );

  // ── Mode selector (step 1) ────────────────────────────────────
  // THREE cards, one discriminant. Not two checkboxes: two booleans give four
  // states and two of them are nonsense.
  const MODE_CARDS: { id: CampaignMode; label: string; icon: typeof faNewspaper; blurb: string }[] = [
    { id: 'offers', label: 'Offers', icon: faPenFancy,
      blurb: 'Pick an approved offer creative from the Creative Studio offers library and deploy a campaign.' },
    { id: 'warmup', label: 'Warm-up newsletter', icon: faTemperatureHalf,
      blurb: 'One KumoMTA warm-up property: today’s registered newsletter plus a cold pad. Records a build request.' },
    { id: 'newsletter', label: 'Newsletters', icon: faNewspaper,
      blurb: 'Every eligible sending domain at once. Content is automatic — generated per domain daily — so this mode AUDITS what will mail and takes one send time for all of them.' },
  ];

  const renderModeSelector = () => (
    <div style={{ marginBottom: 18 }}>
      <div style={{ fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.6, color: 'rgba(180,210,240,0.55)', marginBottom: 8 }}>
        Campaign mode
      </div>
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 10 }}>
        {MODE_CARDS.map(card => {
          const on = activeMode === card.id;
          // Warm-up is the ONLY mode with an eligibility precondition, and
          // the reason it is unavailable is always STATED — an absent panel is
          // indistinguishable from "not eligible", which is how the dead
          // warm-up branch hid.
          const warmupBlocked = card.id === 'warmup' && !isWarmupDomain;
          const disabled = warmupBlocked;
          let why = '';
          if (warmupBlocked) {
            why = !selectedDomain ? 'Select a sending domain first.'
              : warmupDomainsLoading ? 'Checking warm-up eligibility…'
              : warmupDomainsError ? `Eligibility UNKNOWN (${warmupDomainsError}) — this is NOT a statement that the property is ineligible.`
              : `${selectedDomain} is not a KumoMTA warm-up property.`;
          }
          return (
            <button
              key={card.id}
              type="button"
              aria-pressed={on}
              disabled={disabled}
              onClick={() => { if (!disabled) setCampaignMode(card.id); }}
              style={{
                textAlign: 'left', padding: 12, borderRadius: 10, cursor: disabled ? 'default' : 'pointer',
                background: on ? 'rgba(0,176,255,0.10)' : '#0d1526',
                border: `1.5px solid ${on ? '#00b0ff' : 'rgba(0,200,255,0.08)'}`,
                opacity: disabled ? 0.5 : 1, color: '#e0e6f0',
              }}
            >
              <div style={{ fontSize: 13, fontWeight: 600, color: on ? '#00b0ff' : '#e0e6f0' }}>
                <FontAwesomeIcon icon={card.icon} style={{ marginRight: 6, fontSize: 11 }} />
                {card.label}
              </div>
              <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.6)', marginTop: 5, lineHeight: 1.5 }}>
                {card.blurb}
              </div>
              {why && (
                <div style={{ fontSize: 10.5, color: '#f59e0b', marginTop: 6, lineHeight: 1.45 }}>{why}</div>
              )}
            </button>
          );
        })}
      </div>

      {/* An unconfirmable eligibility check must be RETRYABLE and must never
          read as "not a warm-up property". */}
      {warmupDomainsError && (
        <div style={{ marginTop: 10 }}>
          <SectionError
            label="Warm-up eligibility"
            error={`${warmupDomainsError} — Warm-up mode is unavailable because we could not confirm whether this property is a warm-up domain. This is NOT a statement that it is not one.`}
            onRetry={() => setWarmupDomainsKey(k => k + 1)}
          />
        </div>
      )}

      {activeMode !== 'offers' && offerStateEntered && (
        <div style={{
          marginTop: 10, padding: '9px 11px', borderRadius: 8, fontSize: 11.5, lineHeight: 1.55,
          background: 'rgba(245,158,11,0.10)', border: '1px solid rgba(245,158,11,0.45)', color: '#fbbf24',
        }}>
          <FontAwesomeIcon icon={faExclamationTriangle} />{' '}
          You already picked offer content{selectedProofName ? ` (${selectedProofName})` : ''}. It is
          <strong> kept, not discarded</strong> — switch back to Offers and the selection is intact.
        </div>
      )}

      {activeMode === 'warmup' && (
        <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)', marginTop: 10 }}>
          Property slug for the newsletter and anchor lookups:{' '}
          <strong style={{ color: '#e0e6f0', fontFamily: 'monospace' }}>{warmupBrandSlug || '—'}</strong>
          {warmupSlugIsDerived && (
            <span style={{ color: '#f59e0b' }}>
              {' '}· derived from the sending domain (the estate list did not state one) — check it
              matches the property before scheduling
            </span>
          )}
        </div>
      )}

      {activeMode === 'newsletter' && (
        <div style={{ fontSize: 11.5, color: 'rgba(180,210,240,0.6)', marginTop: 10, lineHeight: 1.55 }}>
          Newsletters is <strong style={{ color: '#e0e6f0' }}>estate-wide</strong> — the single sending-domain
          picker does not apply. Choose which domains are in today’s run below; every eligible domain is
          included until you deselect it.
        </div>
      )}
    </div>
  );

  const renderStepDomain = () => (
    <div className="wiz-step-content ig-fade-in">
      <h3 style={{ margin: '0 0 4px' }}>
        {newsletterActive ? 'Campaign mode + sending domains' : 'Select Sending Domain'}
        {!newsletterActive && <RequiredDot />}
      </h3>
      <p style={{ margin: '0 0 16px', color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>
        {newsletterActive
          ? 'Newsletters mails every eligible sending domain. Content is generated per domain daily and is not chosen here — deselect any domain you do not want in today’s run.'
          : 'Choose the domain that will appear in the "From" address. Each domain shows DNS and IP pool info.'}
      </p>
      <StepErrorBanner stepNum={1} />

      {renderModeSelector()}

      {newsletterActive && renderNewsletterRoster()}

      {!newsletterActive && (<>
      {domainError && (
        <div style={{ textAlign: 'center', padding: 20, color: '#ef4444', background: '#1c1c2e', borderRadius: 8, marginBottom: 12 }}>
          <p style={{ margin: '0 0 8px' }}>{domainError}</p>
          <button onClick={fetchDomains} style={{ background: '#00b0ff', color: '#fff', border: 'none', borderRadius: 6, padding: '6px 16px', fontSize: 13, cursor: 'pointer' }}>
            Retry
          </button>
        </div>
      )}
      {!domainError && sendingDomains.length === 0 && (
        <div style={{ textAlign: 'center', padding: 40, color: 'rgba(180,210,240,0.65)' }}>
          No sending domains configured. Add domains in Domain Center first.
        </div>
      )}

      {sendingDomains.length > 0 && (() => {
        const shown = filteredSendingDomains.length;
        return (
          <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 12 }}>
            <input
              type="text"
              aria-label="Filter sending domains"
              value={domainSearch}
              onChange={e => setDomainSearch(e.target.value)}
              placeholder="Type to filter sending domains…"
              style={{
                flex: 1, background: '#0a0f1a', color: '#e0e6f0',
                border: '1px solid rgba(0,200,255,0.15)', borderRadius: 8,
                padding: '8px 12px', fontSize: 13,
              }}
            />
            <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)', whiteSpace: 'nowrap' }}>
              {shown} of {sendingDomains.length}
            </span>
            {domainSearch && (
              <button type="button" onClick={() => setDomainSearch('')}
                      style={{ background: 'transparent', border: 'none', color: '#00b0ff', fontSize: 12, cursor: 'pointer' }}>
                clear
              </button>
            )}
          </div>
        );
      })()}

      {sendingDomains.length > 0 && filteredSendingDomains.length === 0 && (
        <div style={{ padding: 20, color: 'rgba(180,210,240,0.6)', fontSize: 13 }}>
          No sending domain matches &ldquo;{domainSearch}&rdquo;.
        </div>
      )}

      <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
        {filteredSendingDomains.map(d => {
          const domainSelected = selectedDomain === d.domain;
          return (
          <div
            role="button"
            tabIndex={0}
            aria-pressed={domainSelected}
            aria-label={`Select ${d.domain} sending domain`}
            key={d.domain}
            className={`ig-card-hover${domainSelected ? ' ig-breathe-border' : ''}`}
            onClick={() => setSelectedDomain(d.domain)}
            onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); setSelectedDomain(d.domain); } }}
            style={{
              background: domainSelected ? 'rgba(0,200,255,0.08)' : '#0d1526',
              border: `2px solid ${domainSelected ? '#00b0ff' : 'rgba(0,200,255,0.08)'}`,
              borderRadius: 10, padding: 14, cursor: 'pointer',
              transition: 'all 0.2s',
            }}
          >
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8 }}>
              <span style={{ fontSize: 15, fontWeight: 600, color: '#e0e6f0' }}>{d.domain}</span>
              {statusBadge(d.status)}
            </div>
            <div style={{ display: 'flex', gap: 12, fontSize: 12, flexWrap: 'wrap' }}>
              <span style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
                <FontAwesomeIcon icon={d.spf_configured ? faCheckCircle : faTimesCircle} style={{ color: d.spf_configured ? '#10b981' : '#ef4444' }} /> SPF
              </span>
              <span style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
                <FontAwesomeIcon icon={d.dkim_configured ? faCheckCircle : faTimesCircle} style={{ color: d.dkim_configured ? '#10b981' : '#ef4444' }} /> DKIM
              </span>
              <span style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
                <FontAwesomeIcon icon={d.dmarc_configured ? faCheckCircle : faTimesCircle} style={{ color: d.dmarc_configured ? '#10b981' : '#ef4444' }} /> DMARC
              </span>
              <span style={{ color: 'rgba(180,210,240,0.65)' }}>Pool: {d.pool_name}</span>
              <span style={{ color: 'rgba(180,210,240,0.65)' }}>IPs: {d.active_ips} active / {d.warmup_ips} warmup</span>
              <span style={{ color: 'rgba(180,210,240,0.65)' }}>Rep: {d.reputation_score.toFixed(0)}%</span>
            </div>
          </div>
          );
        })}
      </div>

      {/* Sending profile — the ROUTE. A domain can carry several active
          profiles (m.discountblog.com has four: SES tenant, SES relay and two
          legacy rows). The server's by-domain lookup takes the most recently
          created one, so an SES route is only deterministic when pinned. */}
      {selectedDomain && (
        <div style={{ marginTop: 20 }}>
          <h3 style={{ margin: '0 0 4px' }}>Sending profile (route)</h3>
          <p style={{ margin: '0 0 10px', color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>
            Pin the profile this campaign sends through. Leave on auto only when the domain has a
            single active profile.
          </p>
          {(() => {
            const profiles = sendingDomains.find(d => d.domain === selectedDomain)?.profiles || [];
            const ambiguous = profiles.length > 1 && !selectedProfileId;
            return (
              <>
                <select
                  aria-label="Select sending profile"
                  value={selectedProfileId}
                  onChange={e => setSelectedProfileId(e.target.value)}
                  style={{
                    background: '#0d1526', color: '#e0e6f0',
                    border: `2px solid ${ambiguous ? 'rgba(245,158,11,0.5)' : 'rgba(0,200,255,0.15)'}`,
                    borderRadius: 8, padding: '8px 10px', fontSize: 13, minWidth: 380,
                  }}
                >
                  <option value="">Auto — most recently created active profile</option>
                  {profiles.map(pr => (
                    <option key={pr.id} value={pr.id}>
                      [{pr.transport.toUpperCase()}] {pr.name}{pr.from_name ? ` — ${pr.from_name}` : ''}
                    </option>
                  ))}
                </select>
                {profiles.length === 0 && (
                  <div style={{ fontSize: 12, color: '#f59e0b', marginTop: 8 }}>
                    <FontAwesomeIcon icon={faExclamationTriangle} /> No active profile is registered for
                    this domain — the deploy preflight will reject it.
                  </div>
                )}
                {ambiguous && (
                  <div style={{ fontSize: 12, color: '#f59e0b', marginTop: 8 }}>
                    <FontAwesomeIcon icon={faExclamationTriangle} /> {profiles.length} active profiles
                    exist for {selectedDomain}. Pin one — auto-resolution is non-deterministic and can
                    silently pick the wrong transport.
                  </div>
                )}
              </>
            );
          })()}
        </div>
      )}

      </>)}
    </div>
  );

  const renderStep3 = () => {
    const v = variants[0];
    return (
      <div className="wiz-step-content ig-fade-in">
        <div style={{ marginBottom: 16 }}>
          <h3 style={{ margin: 0 }}>Offer + Creative</h3>
          <p style={{ margin: '4px 0 0', color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>
            Creative, subject, preheader and from-name come from the Creative Studio offers library.
            Nothing outside the approved pools can be scheduled here.
          </p>
        </div>
        <StepErrorBanner stepNum={3} />

        <OfferCreativePicker
          apiBase={API_BASE}
          // Stable identity. An inline arrow here changed on every wizard
          // render, which re-fired the picker's load effect and overwrote the
          // selected proof with the LIST row — and the list endpoint omits
          // html_content, so Preview opened blank (operator 2026-08-18).
          orgFetch={pickerFetch}
          sendingDomain={selectedDomain}
          brandRoot={engagementTiers?.brand_root || ''}
          offers={offersCatalog}
          offersError={offersError}
          selectedOfferId={selectedOfferId}
          onOfferChange={setSelectedOfferId}
          proofId={selectedProofId}
          subject={v?.subject || ''}
          preheader={v?.preview_text || ''}
          fromName={v?.from_name || ''}
          hasHtml={!!v?.html_content}
          currentHtml={v?.html_content || ''}
          profileFromName={selectedProfile?.from_name || sendingDomains.find(d => d.domain === selectedDomain)?.from_name}
          isKumoRoute={isKumoRoute}
          onApply={sel => {
            setSelectedProofId(sel.proofId);
            setSelectedProofName(sel.proofName);
            // Approved creative ships byte-faithful unless the operator opts
            // out — the same posture the board compiles (content_locked: true).
            setContentLocked(true);
            setVariants([{
              variant_name: 'A',
              from_name: sel.fromName,
              subject: sel.subject,
              preview_text: sel.preheader,
              html_content: sel.html,
              split_percent: 100,
            }]);
          }}
          onFieldChange={f => {
            setVariants(prev => {
              const base = prev[0] || { variant_name: 'A', from_name: '', subject: '', preview_text: '', html_content: '', split_percent: 100 };
              return [{
                ...base,
                subject: f.subject !== undefined ? f.subject : base.subject,
                preview_text: f.preheader !== undefined ? f.preheader : base.preview_text,
                from_name: f.fromName !== undefined ? f.fromName : base.from_name,
              }];
            });
          }}
        />

        {/* content_locked — ship the approved creative byte-faithful. The board
            sets this on every campaign it compiles; the wizard defaults it on
            as soon as an approved proof is selected. */}
        <div style={{
          background: contentLocked ? 'rgba(245,158,11,0.08)' : '#0d1526',
          border: `1px solid ${contentLocked ? 'rgba(245,158,11,0.5)' : 'rgba(0,200,255,0.08)'}`,
          borderRadius: 10, padding: 14, display: 'flex', alignItems: 'flex-start', gap: 12,
        }}>
          <input
            type="checkbox"
            checked={contentLocked}
            onChange={e => setContentLocked(e.target.checked)}
            style={{ width: 18, height: 18, cursor: 'pointer', marginTop: 2, accentColor: '#f59e0b' }}
          />
          <div>
            <div style={{ fontSize: 13, fontWeight: 600, color: contentLocked ? '#f59e0b' : '#e0e6f0' }}>
              <FontAwesomeIcon icon={faLock} style={{ marginRight: 6, fontSize: 11 }} />
              Lock content (ship the approved creative byte-faithful)
            </div>
            <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.6)', marginTop: 3 }}>
              The wave dispatcher skips its subject and HTML fingerprint mutations. Honeypot
              injection and URL sanitisation still run. Required by strict advertisers, and the
              default for anything sourced from an approved proof.
            </div>
          </div>
        </div>

        {selectedProofName && (
          <div style={{ marginTop: 12, fontSize: 11, color: 'rgba(180,210,240,0.5)' }}>
            Shipping <strong style={{ color: '#e0e6f0' }}>{selectedProofName}</strong>
            {v?.html_content ? ` · ${v.html_content.length.toLocaleString()} bytes of approved HTML` : ''}
          </div>
        )}
      </div>
    );
  };

  const renderStep4 = () => {
    const totalSelected = selectedLists.length + selectedSegments.length;
    const totalSuppSelected = selectedSuppLists.length + selectedExclusionSegments.length;

    const AudienceCard: React.FC<{
      name: string; count: number; selected: boolean; type: 'list' | 'segment' | 'suppression' | 'exclusion-segment';
      onToggle: () => void;
      category?: string;
      status?: string;
    }> = ({ name, count, selected, type, onToggle, category, status }) => {
      const colors: Record<string, string> = { list: '#00b0ff', segment: '#8b5cf6', suppression: '#ef4444', 'exclusion-segment': '#f59e0b' };
      const icons: Record<string, any> = { list: faUsers, segment: faChartBar, suppression: faShieldAlt, 'exclusion-segment': faCrosshairs };
      const c = colors[type];
      const meta = (type === 'segment' || type === 'exclusion-segment') ? getCategoryMeta(category) : null;
      const isArchived = status === 'archived';
      return (
        <div
          role="button" tabIndex={0} onClick={onToggle}
          onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onToggle(); } }}
          style={{
            display: 'flex', alignItems: 'center', gap: 10, padding: '10px 12px',
            background: selected ? `${c}12` : '#0a0f1a',
            border: `1.5px solid ${selected ? c : 'rgba(0,200,255,0.06)'}`,
            borderRadius: 8, cursor: 'pointer',
            transition: 'all 0.2s ease',
            transform: selected ? 'scale(1.01)' : 'scale(1)',
            opacity: isArchived ? 0.55 : 1,
          }}
        >
          <div style={{
            width: 32, height: 32, borderRadius: 8, display: 'flex', alignItems: 'center', justifyContent: 'center',
            background: selected ? `${c}20` : 'rgba(0,200,255,0.04)',
            color: selected ? c : 'rgba(180,210,240,0.4)',
            transition: 'all 0.2s ease', fontSize: 13,
          }}>
            <FontAwesomeIcon icon={icons[type]} />
          </div>
          <div style={{ flex: 1, minWidth: 0 }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 6, minWidth: 0 }}>
              <div style={{ fontSize: 12, fontWeight: 500, color: '#e0e6f0', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis', flex: 1, minWidth: 0 }}>{name}</div>
              {meta && (
                <span title={meta.description} className={meta.badgeClass} style={{
                  fontSize: 9, padding: '1px 6px', borderRadius: 999, fontWeight: 600,
                  border: '1px solid', textTransform: 'uppercase', letterSpacing: 0.3, flexShrink: 0,
                }}>{meta.shortLabel}</span>
              )}
              {isArchived && (
                <span style={{ fontSize: 9, color: '#9ca3af', background: 'rgba(75,85,99,0.3)', padding: '1px 6px', borderRadius: 999, flexShrink: 0, border: '1px solid rgba(156,163,175,0.4)' }}>archived</span>
              )}
            </div>
            <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.5)', marginTop: 1 }}>{count.toLocaleString()} {type === 'suppression' ? 'entries' : 'subscribers'}</div>
          </div>
          <div style={{
            width: 20, height: 20, borderRadius: 5,
            border: `2px solid ${selected ? c : 'rgba(180,210,240,0.2)'}`,
            background: selected ? c : 'transparent',
            display: 'flex', alignItems: 'center', justifyContent: 'center',
            transition: 'all 0.2s ease', flexShrink: 0,
          }}>
            {selected && <FontAwesomeIcon icon={faCheck} style={{ color: '#fff', fontSize: 9 }} />}
          </div>
        </div>
      );
    };

    // Compact pill row used at the top of the inclusion + exclusion segment
    // panels so the operator can toggle which segment categories are
    // visible. Mirrors the operator-facing labels from segCategoryMetadata.
    const CategoryFilterRow: React.FC<{
      mode: 'inclusion' | 'exclusion';
      active: Set<SegmentCategory>;
      setActive: React.Dispatch<React.SetStateAction<Set<SegmentCategory>>>;
      showArchivedToggle: boolean;
    }> = ({ active, setActive, showArchivedToggle }) => {
      const counts = SEGMENT_CATEGORIES.reduce<Record<string, number>>((acc, c) => {
        acc[c.id] = segments.filter(s => (s.category || 'uncategorized') === c.id && (showArchived || s.status !== 'archived')).length;
        return acc;
      }, {});
      const total = Object.values(counts).reduce((a, b) => a + b, 0);
      return (
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4, marginBottom: 8, alignItems: 'center' }}>
          {SEGMENT_CATEGORIES.filter(cat => counts[cat.id] > 0).map(cat => {
            const isActive = active.has(cat.id);
            return (
              <button
                key={cat.id}
                onClick={() => setActive(prev => {
                  const next = new Set(prev);
                  if (next.has(cat.id)) next.delete(cat.id);
                  else next.add(cat.id);
                  return next;
                })}
                title={cat.description}
                style={{
                  fontSize: 10, padding: '3px 8px', borderRadius: 999, cursor: 'pointer',
                  background: isActive ? 'rgba(139,92,246,0.18)' : 'rgba(15,23,42,0.6)',
                  color: isActive ? '#d4baff' : 'rgba(180,210,240,0.5)',
                  border: `1px solid ${isActive ? 'rgba(139,92,246,0.5)' : 'rgba(180,210,240,0.12)'}`,
                  fontWeight: 600, letterSpacing: 0.3, transition: 'all 0.15s ease',
                }}
              >{cat.shortLabel} <span style={{ opacity: 0.7 }}>{counts[cat.id]}</span></button>
            );
          })}
          <span style={{ flex: 1 }} />
          {showArchivedToggle && (
            <button
              onClick={() => setShowArchived(v => !v)}
              style={{
                fontSize: 10, padding: '3px 8px', borderRadius: 999, cursor: 'pointer',
                background: showArchived ? 'rgba(245,158,11,0.18)' : 'rgba(15,23,42,0.6)',
                color: showArchived ? '#fbbf24' : 'rgba(180,210,240,0.4)',
                border: `1px solid ${showArchived ? 'rgba(245,158,11,0.5)' : 'rgba(180,210,240,0.12)'}`,
                fontWeight: 600, letterSpacing: 0.3,
              }}
            >{showArchived ? 'hide' : 'show'} archived</button>
          )}
          <button
            onClick={() => setActive(new Set(SEGMENT_CATEGORIES.map(c => c.id)))}
            style={{
              fontSize: 10, padding: '3px 8px', borderRadius: 999, cursor: 'pointer',
              background: 'rgba(15,23,42,0.6)', color: 'rgba(180,210,240,0.5)',
              border: '1px solid rgba(180,210,240,0.12)', fontWeight: 600, letterSpacing: 0.3,
            }}
            title={`Show all ${total} segments across every category`}
          >all</button>
        </div>
      );
    };

    return (
      <div className="wiz-step-content ig-fade-in">
        <h3 style={{ margin: '0 0 4px' }}>Audience + Suppression<RequiredDot /></h3>
        <p style={{ margin: '0 0 16px', color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>
          Build your target audience and apply suppression filters.
        </p>
        <StepErrorBanner stepNum={4} />

        {/* Threshold calibrated against measured prod, not guessed: healthy
            properties sit at 0-1% cancelled over terminal outcomes
            (m.discountblog.com 6 cancelled / 1,896 sent = 0%), while the held
            kumo estate sits at 50%. 25% is the empty middle, so the estate
            still trips as it drifts and no healthy brand ever does. */}
        {sendHistory && sendHistory.total > 0 && sendHistory.cancel_rate >= 0.25 && (
          <div style={{
            marginBottom: 16, padding: '10px 12px', borderRadius: 10,
            background: 'rgba(245,158,11,0.08)', border: '1px solid rgba(245,158,11,0.4)',
            fontSize: 12, color: '#f59e0b',
          }}>
            <FontAwesomeIcon icon={faExclamationTriangle} />{' '}
            <strong>This property looks held back.</strong>{' '}
            {Math.round(sendHistory.cancel_rate * 100)}% of its campaigns that reached a terminal
            state in the last {sendHistory.days} days were <strong>cancelled</strong>
            {' '}({Object.entries(sendHistory.counts).map(([k, v]) => `${v} ${k}`).join(' · ')}).
            {sendHistory.last_sent_at
              ? ` Last actually sent ${new Date(sendHistory.last_sent_at).toLocaleString()}.`
              : ' Nothing has sent in that window.'}
            {' '}Staged-then-cancelled is how this estate is paused — check before scheduling.
          </div>
        )}

        {isKumoRoute && kumoIllegalISPs.length > 0 && (
          <div style={{
            marginBottom: 16, padding: '10px 12px', borderRadius: 10,
            background: 'rgba(239,68,68,0.08)', border: '1px solid rgba(239,68,68,0.45)',
            fontSize: 12, color: '#ef4444',
          }}>
            <FontAwesomeIcon icon={faExclamationTriangle} />{' '}
            <strong>KumoMTA warm-up is yahoo-family only.</strong> Remove{' '}
            <strong>{kumoIllegalISPs.join(', ')}</strong> on the Mailbox Providers step — the estate registry caps
            yahoo, aol, att, sbcglobal and cox, and nothing else may send.
          </div>
        )}

        <EngagementTierPicker
          tiers={engagementTiers}
          loading={engagementLoading}
          error={engagementError}
          selectedClickerIds={selectedClickerIds}
          selectedOpenerIds={selectedOpenerIds}
          selectedOtherIds={selectedOtherIds}
          onToggle={toggleEngagementTier}
          excludeClickers={excludeClickers}
          onExcludeClickersChange={setExcludeClickers}
          onRetry={() => setEngagementReloadKey(k => k + 1)}
        />

        {/* Volume posture. The engaged tier is UNCAPPED and audience-bound by
            standing doctrine — capping it is the exception and needs a reason. */}
        <div style={{
          background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)',
          borderRadius: 10, padding: 14, marginBottom: 16,
        }}>
          <label style={{ display: 'flex', alignItems: 'flex-start', gap: 8, cursor: 'pointer' }}>
            <input type="checkbox" checked={audienceBound}
                   onChange={e => { setAudienceBound(e.target.checked); if (e.target.checked) setMasterTopUp(false); }}
                   style={{ marginTop: 2, width: 15, height: 15, cursor: 'pointer' }} />
            <span style={{ fontSize: 12, color: 'rgba(180,210,240,0.8)' }}>
              <strong style={{ color: '#e0e6f0' }}>Mail the whole selected audience</strong> — no per-ISP
              cap (volume 0 = audience-bound). This is the standing engaged-tier default; the
              per-provider quotas on the Mailbox Providers step are ignored while it is on.
            </span>
          </label>

          {/* Explicit alignment banner (operator 2026-08-18): "if I select zero
              for my quota, it will send everything… this should be a
              confirmation banner that displays just so I am aligned and the
              system is aligned." Names the number, not just the posture. */}
          {audienceBound && (
            <div style={{
              marginTop: 10, padding: '10px 12px', borderRadius: 8, fontSize: 12, lineHeight: 1.55,
              background: 'rgba(245,158,11,0.10)', border: '1px solid rgba(245,158,11,0.5)', color: '#fbbf24',
            }}>
              <FontAwesomeIcon icon={faInfinity} />{' '}
              <strong>UNCAPPED — every per-provider quota is 0.</strong>{' '}
              {audienceEstimate
                ? <>This send will mail all{' '}
                    <strong>{(audienceEstimate.after_suppressions ?? audienceEstimate.total_recipients).toLocaleString()}</strong>{' '}
                    recipients that survive suppression across{' '}
                    {selectedISPs.length} selected provider{selectedISPs.length === 1 ? '' : 's'} — there is
                    no ceiling to stop it.</>
                : <>Every selected provider will mail its entire qualifying audience — there is no ceiling
                    to stop it. Select an audience below to see the exact recipient count.</>}
              {' '}Uncheck the box above to impose per-provider caps instead.
            </div>
          )}

          <label style={{
            display: 'flex', alignItems: 'flex-start', gap: 8, marginTop: 10,
            cursor: audienceBound ? 'default' : 'pointer', opacity: audienceBound ? 0.45 : 1,
          }}>
            <input type="checkbox" checked={masterTopUp && !audienceBound} disabled={audienceBound}
                   onChange={e => setMasterTopUp(e.target.checked)}
                   style={{ marginTop: 2, width: 15, height: 15, cursor: audienceBound ? 'default' : 'pointer' }} />
            <span style={{ fontSize: 12, color: 'rgba(180,210,240,0.75)' }}>
              Top up from the master list when the selected audience does not fill the per-ISP
              quotas.{' '}
              {audienceBound
                ? <em style={{ color: '#f59e0b' }}>Unavailable while the send is audience-bound — with no
                    quota to stop at, the top-up would stream the entire sending domain.</em>
                : 'Sources the remainder from mailing_subscriber_domain_state for this sending domain.'}
            </span>
          </label>
        </div>

        {audienceError && (
          <div style={{ background: '#3b1a1a', border: '1px solid #e53935', borderRadius: 8, padding: '10px 14px', marginBottom: 16, color: '#ff8a80', fontSize: 13, display: 'flex', alignItems: 'center', gap: 8 }}>
            <FontAwesomeIcon icon={faExclamationTriangle} /> {audienceError}
            <button onClick={fetchAudienceData} className="ig-btn-glow" style={{ marginLeft: 'auto', background: 'rgba(0,176,255,0.1)', color: '#00b0ff', border: '1px solid rgba(0,176,255,0.2)', borderRadius: 6, padding: '4px 12px', fontSize: 12, cursor: 'pointer' }}>
              Retry
            </button>
          </div>
        )}

        {/* Skeleton while loading audience data */}
        {audienceDataLoading && lists.length === 0 && segments.length === 0 && (
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16, marginBottom: 16 }}>
            {[0, 1].map(col => (
              <div key={col}>
                <div style={{ height: 16, width: '40%', background: 'rgba(0,200,255,0.06)', borderRadius: 4, marginBottom: 12, animation: 'igShimmer 1.5s ease infinite' }} />
                {[1, 2, 3].map(j => (
                  <div key={j} style={{ height: 48, background: '#0d1526', border: '1px solid rgba(0,200,255,0.04)', borderRadius: 8, marginBottom: 8, animation: 'igShimmer 1.5s ease infinite', animationDelay: `${j * 0.15}s` }} />
                ))}
              </div>
            ))}
          </div>
        )}

        {(!audienceDataLoading || lists.length > 0 || segments.length > 0 || suppressionLists.length > 0) && (
          <>
            {/* Top stat bar */}
            <div style={{ display: 'flex', gap: 12, marginBottom: 16 }}>
              {[
                { label: 'Lists Selected', value: selectedLists.length, total: lists.length, color: '#00b0ff' },
                { label: 'Segments Selected', value: selectedSegments.length, total: segments.length, color: '#8b5cf6' },
                { label: 'Suppression Active', value: totalSuppSelected, total: suppressionLists.length, color: '#ef4444' },
              ].map(stat => (
                <div key={stat.label} style={{
                  flex: 1, background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: '12px 14px',
                  position: 'relative', overflow: 'hidden',
                }}>
                  <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.5)', marginBottom: 4, textTransform: 'uppercase', letterSpacing: 0.5 }}>{stat.label}</div>
                  <div style={{ fontSize: 22, fontWeight: 700, color: stat.value > 0 ? stat.color : '#64748b', transition: 'color 0.3s' }}>
                    {stat.value}<span style={{ fontSize: 12, fontWeight: 400, color: 'rgba(180,210,240,0.4)' }}>/{stat.total}</span>
                  </div>
                  {/* Progress line at bottom */}
                  <div style={{ position: 'absolute', bottom: 0, left: 0, right: 0, height: 2, background: 'rgba(0,200,255,0.04)' }}>
                    <div style={{
                      height: '100%', background: stat.color, borderRadius: 2,
                      width: stat.total > 0 ? `${(stat.value / stat.total) * 100}%` : '0%',
                      transition: 'width 0.4s ease',
                    }} />
                  </div>
                </div>
              ))}
            </div>

            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16, marginBottom: 16 }}>
              {/* Inclusion panel */}
              <div>
                <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 10 }}>
                  <div style={{ width: 4, height: 16, borderRadius: 2, background: '#10b981' }} />
                  <h4 style={{ margin: 0, fontSize: 13, color: '#10b981', fontWeight: 600 }}>Inclusion</h4>
                  <span style={{ fontSize: 10, color: 'rgba(180,210,240,0.4)', marginLeft: 'auto' }}>{totalSelected} selected</span>
                </div>

                {lists.length === 0 && segments.length === 0 && !audienceDataLoading && (
                  <div style={{ background: '#0d1526', border: '1px dashed rgba(0,200,255,0.1)', borderRadius: 10, padding: 24, textAlign: 'center' }}>
                    <FontAwesomeIcon icon={faUsers} style={{ fontSize: 24, color: 'rgba(180,210,240,0.15)', marginBottom: 8 }} />
                    <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.4)' }}>No lists or segments available</div>
                  </div>
                )}

                {(lists.length > 0 || segments.length > 0) && (
                  <div style={{ position: 'relative', marginBottom: 8 }}>
                    <FontAwesomeIcon icon={faSearch} style={{ position: 'absolute', left: 10, top: '50%', transform: 'translateY(-50%)', fontSize: 11, color: 'rgba(180,210,240,0.3)', pointerEvents: 'none' }} />
                    <input
                      type="text" value={inclusionSearch} onChange={e => setInclusionSearch(e.target.value)}
                      placeholder="Search lists & segments..."
                      style={{ width: '100%', boxSizing: 'border-box', padding: '7px 10px 7px 30px', fontSize: 12, background: '#0a0f1e', border: '1px solid rgba(0,200,255,0.1)', borderRadius: 6, color: '#e0e8f0', outline: 'none' }}
                    />
                  </div>
                )}

                {segments.length > 0 && (
                  <CategoryFilterRow mode="inclusion" active={activeIncCategories} setActive={setActiveIncCategories} showArchivedToggle={true} />
                )}

                <div style={{ display: 'flex', flexDirection: 'column', gap: 6, maxHeight: 260, overflowY: 'auto', paddingRight: 4 }}>
                  {lists
                    .filter(l => !inclusionSearch || l.name.toLowerCase().includes(inclusionSearch.toLowerCase()))
                    .sort((a, b) => {
                      const asel = selectedLists.includes(a.id) ? 0 : 1;
                      const bsel = selectedLists.includes(b.id) ? 0 : 1;
                      return asel !== bsel ? asel - bsel : a.name.localeCompare(b.name);
                    })
                    .map(l => (
                    <AudienceCard key={`list-${l.id}`} name={l.name} count={l.subscriber_count || 0}
                      selected={selectedLists.includes(l.id)} type="list" onToggle={() => toggleList(l.id)} />
                  ))}
                  {segments
                    .filter(s => activeIncCategories.has((s.category || 'uncategorized') as SegmentCategory))
                    .filter(s => showArchived || s.status !== 'archived')
                    .filter(s => !inclusionSearch || s.name.toLowerCase().includes(inclusionSearch.toLowerCase()))
                    .sort((a, b) => {
                      const asel = selectedSegments.includes(a.id) ? 0 : 1;
                      const bsel = selectedSegments.includes(b.id) ? 0 : 1;
                      return asel !== bsel ? asel - bsel : a.name.localeCompare(b.name);
                    })
                    .map(s => (
                    <AudienceCard key={`seg-${s.id}`} name={s.name} count={s.subscriber_count || 0}
                      selected={selectedSegments.includes(s.id)} type="segment" onToggle={() => toggleSegment(s.id)}
                      category={s.category} status={s.status} />
                  ))}
                </div>
              </div>

              {/* Suppression panel */}
              <div>
                <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 10 }}>
                  <div style={{ width: 4, height: 16, borderRadius: 2, background: '#ef4444' }} />
                  <h4 style={{ margin: 0, fontSize: 13, color: '#ef4444', fontWeight: 600 }}>Suppression</h4>
                  <span style={{ fontSize: 9, color: 'rgba(180,210,240,0.4)', marginLeft: 6 }}>
                    curated lists only
                  </span>
                  <span style={{ fontSize: 10, color: 'rgba(180,210,240,0.4)', marginLeft: 'auto' }}>{totalSuppSelected} active</span>
                </div>

                {/* The single biggest misread of this panel (operator
                    2026-08-18: "I do not see the globe life suppression"):
                    these are mailing_suppression_lists rows — curated
                    advertiser lists you tick. An offer's OWN suppression
                    ledger (mailing_offer_suppressions: converted subscribers,
                    advertiser scrubs, Optizmo deltas) is keyed to offer_id and
                    fires automatically at plan time; it never appears here.
                    The Offer + Creative step reports its real row count. */}
                <div style={{
                  fontSize: 11, lineHeight: 1.5, color: 'rgba(180,210,240,0.6)',
                  background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)',
                  borderRadius: 6, padding: '7px 10px', marginBottom: 8,
                }}>
                  These are curated advertiser lists — tick the ones this send needs. The selected
                  offer&apos;s OWN suppression (converted subscribers, advertiser scrubs, Optizmo)
                  is applied automatically and is <strong>not</strong> listed here;{' '}
                  {selectedOfferId
                    ? 'the Offer + Creative step shows how many rows it holds.'
                    : 'pick an offer on the Offer + Creative step to see how many rows it holds.'}
                </div>

                {suppressionLists.length === 0 && !audienceDataLoading && (
                  <div style={{ background: '#0d1526', border: '1px dashed rgba(0,200,255,0.1)', borderRadius: 10, padding: 24, textAlign: 'center' }}>
                    <FontAwesomeIcon icon={faShieldAlt} style={{ fontSize: 24, color: 'rgba(180,210,240,0.15)', marginBottom: 8 }} />
                    <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.4)' }}>No suppression lists available</div>
                  </div>
                )}

                {suppressionLists.length > 0 && (
                  <div style={{ position: 'relative', marginBottom: 8 }}>
                    <FontAwesomeIcon icon={faSearch} style={{ position: 'absolute', left: 10, top: '50%', transform: 'translateY(-50%)', fontSize: 11, color: 'rgba(180,210,240,0.3)', pointerEvents: 'none' }} />
                    <input
                      type="text" value={suppressionSearch} onChange={e => setSuppressionSearch(e.target.value)}
                      placeholder="Search suppression..."
                      style={{ width: '100%', boxSizing: 'border-box', padding: '7px 10px 7px 30px', fontSize: 12, background: '#0a0f1e', border: '1px solid rgba(0,200,255,0.1)', borderRadius: 6, color: '#e0e8f0', outline: 'none' }}
                    />
                  </div>
                )}

                <div style={{ display: 'flex', flexDirection: 'column', gap: 6, maxHeight: 200, overflowY: 'auto', paddingRight: 4 }}>
                  {suppressionLists
                    .filter(sl => !suppressionSearch || sl.name.toLowerCase().includes(suppressionSearch.toLowerCase()))
                    .sort((a, b) => {
                      const asel = selectedSuppLists.includes(a.id) ? 0 : 1;
                      const bsel = selectedSuppLists.includes(b.id) ? 0 : 1;
                      return asel !== bsel ? asel - bsel : a.name.localeCompare(b.name);
                    })
                    .map(sl => (
                    <AudienceCard key={`supp-${sl.id}`} name={sl.name} count={sl.entry_count || 0}
                      selected={selectedSuppLists.includes(sl.id)} type="suppression" onToggle={() => toggleSuppList(sl.id)} />
                  ))}
                </div>

                {segments.length > 0 && (
                  <>
                    <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginTop: 14, marginBottom: 8 }}>
                      <div style={{ width: 4, height: 16, borderRadius: 2, background: '#f59e0b' }} />
                      <h4 style={{ margin: 0, fontSize: 13, color: '#f59e0b', fontWeight: 600 }}>Exclusion Segments</h4>
                      <span style={{ fontSize: 10, color: 'rgba(180,210,240,0.4)', marginLeft: 'auto' }}>{selectedExclusionSegments.length} active</span>
                    </div>
                    <div style={{ position: 'relative', marginBottom: 8 }}>
                      <FontAwesomeIcon icon={faSearch} style={{ position: 'absolute', left: 10, top: '50%', transform: 'translateY(-50%)', fontSize: 11, color: 'rgba(180,210,240,0.3)', pointerEvents: 'none' }} />
                      <input
                        type="text" value={exclusionSearch} onChange={e => setExclusionSearch(e.target.value)}
                        placeholder="Search exclusion segments..."
                        style={{ width: '100%', boxSizing: 'border-box', padding: '7px 10px 7px 30px', fontSize: 12, background: '#0a0f1e', border: '1px solid rgba(0,200,255,0.1)', borderRadius: 6, color: '#e0e8f0', outline: 'none' }}
                      />
                    </div>
                    <CategoryFilterRow mode="exclusion" active={activeExcCategories} setActive={setActiveExcCategories} showArchivedToggle={false} />
                    <div style={{ display: 'flex', flexDirection: 'column', gap: 6, maxHeight: 200, overflowY: 'auto', paddingRight: 4 }}>
                      {segments
                        .filter(s => activeExcCategories.has((s.category || 'uncategorized') as SegmentCategory))
                        .filter(s => showArchived || s.status !== 'archived')
                        .filter(s => !exclusionSearch || s.name.toLowerCase().includes(exclusionSearch.toLowerCase()))
                        .sort((a, b) => {
                          const asel = selectedExclusionSegments.includes(a.id) ? 0 : 1;
                          const bsel = selectedExclusionSegments.includes(b.id) ? 0 : 1;
                          return asel !== bsel ? asel - bsel : a.name.localeCompare(b.name);
                        })
                        .map(s => (
                        <AudienceCard key={`excl-seg-${s.id}`} name={s.name} count={s.subscriber_count || 0}
                          selected={selectedExclusionSegments.includes(s.id)} type="exclusion-segment"
                          onToggle={() => toggleExclusionSegment(s.id)}
                          category={s.category} status={s.status} />
                      ))}
                    </div>
                  </>
                )}
              </div>
            </div>

            {/* Another property's cohorts in this send — fail-closed at step 4,
                and named here so the operator can see WHICH rows to drop. */}
            {foreignSegments.length > 0 && (
              <div style={{
                background: 'rgba(239,68,68,0.10)', border: '1px solid rgba(239,68,68,0.5)',
                borderRadius: 10, padding: 14, marginBottom: 16, color: '#fca5a5', fontSize: 12, lineHeight: 1.6,
              }}>
                <FontAwesomeIcon icon={faExclamationTriangle} />{' '}
                <strong>These segments belong to another property</strong> — this campaign sends from{' '}
                <strong>{selectedDomain}</strong>{propertyCode ? ` (${propertyCode})` : ''}, and brands are
                separate senders. Deselect them before continuing:
                <ul style={{ margin: '8px 0 0 18px', padding: 0 }}>
                  {foreignSegments.map(f => (
                    <li key={f.id}><code>{f.name}</code> — {f.code}</li>
                  ))}
                </ul>
              </div>
            )}

            {/* Unified Send Priority.
                The engagement-range chips ARE send priority — the payload puts
                them ahead of everything picked in the advanced panel
                (buildCampaignPayload's mergedSendPriority). The panel used to
                render `sendPriority` alone, so a board built from the chips
                showed an incomplete order ("only openers, not clickers") while
                the real drain order was clickers → openers → all-time →
                advanced. Show the whole order, and label the fixed part as
                fixed: clicks are GOLD, opens silver — that ranking is doctrine,
                not a preference (JAOS signal grading). */}
            {(engagementPriorityRows.length > 0 || sendPriority.length > 1) && (
              <div style={{
                background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 16, marginBottom: 16,
              }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 10 }}>
                  <div style={{ width: 4, height: 16, borderRadius: 2, background: '#00e5ff' }} />
                  <h4 style={{ margin: 0, fontSize: 13, color: '#00e5ff', fontWeight: 600 }}>Send Priority</h4>
                  <span style={{ fontSize: 10, color: 'rgba(180,210,240,0.4)', marginLeft: 'auto' }}>
                    Drained top-down — #1 sends first
                  </span>
                </div>

                {engagementPriorityRows.length > 0 && (
                  <div style={{ marginBottom: sendPriority.length > 0 ? 10 : 0 }}>
                    <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.45)', marginBottom: 6 }}>
                      From the engagement ranges above — fixed order (clicks rank above opens).
                    </div>
                    <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
                      {engagementPriorityRows.map((row, i) => {
                        // A clicker row is INERT while disjointness is on: the
                        // payload puts it in exclusion_segments and the planner
                        // denies every one of its members. The panel used to
                        // render it as locked #1 regardless — the exact reason
                        // "DB 60D Clickers" planned 0 of 34,260 on 2026-08-18.
                        const inert = row.tierLabel === 'clickers'
                          && excludeClickers && selectedOpenerIds.length > 0;
                        return (
                        <div key={`eng-${row.id}`} style={{
                          display: 'flex', alignItems: 'center', gap: 10, padding: '8px 10px',
                          background: '#0a0f1a', borderRadius: 6,
                          border: `1px solid ${inert ? 'rgba(239,68,68,0.5)' : `${row.color}40`}`,
                          opacity: inert ? 0.6 : 1,
                        }}>
                          <span style={{
                            width: 20, height: 20, borderRadius: '50%', background: `${row.color}20`,
                            color: row.color, fontSize: 11, fontWeight: 700,
                            display: 'flex', alignItems: 'center', justifyContent: 'center', flexShrink: 0,
                          }}>{i + 1}</span>
                          <FontAwesomeIcon icon={faLock} style={{ fontSize: 10, color: 'rgba(180,210,240,0.3)' }} />
                          <span style={{
                            fontSize: 12, color: '#e0e6f0', flex: 1,
                            textDecoration: inert ? 'line-through' : 'none',
                          }}>{row.label}</span>
                          {inert && (
                            <span style={{
                              fontSize: 10, fontWeight: 700, color: '#ef4444',
                              border: '1px solid rgba(239,68,68,0.5)', borderRadius: 4, padding: '1px 6px',
                            }}>EXCLUDED — MAILS 0</span>
                          )}
                          <span style={{ fontSize: 11, color: row.color, fontWeight: 600 }}>{row.tierLabel}</span>
                          <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)' }}>
                            {inert ? '0' : row.count.toLocaleString()}
                          </span>
                        </div>
                        );
                      })}
                    </div>
                  </div>
                )}

                {sendPriority.length > 0 && engagementPriorityRows.length > 0 && (
                  <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.45)', marginBottom: 6 }}>
                    Then, from the advanced picker — drag or use arrows to reorder.
                  </div>
                )}

                <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
                  {sendPriority.map((item, idx) => {
                    const isListItem = item.type === 'list';
                    const info = isListItem
                      ? lists.find(l => l.id === item.id)
                      : segments.find(s => s.id === item.id);
                    const label = info ? info.name : `${isListItem ? 'List' : 'Segment'} ${item.id.slice(0, 8)}…`;
                    const count = info ? ((info as any).subscriber_count || 0) : 0;
                    const accent = isListItem ? '#f59e0b' : '#8b5cf6';
                    return (
                      <div
                        key={`${item.type}-${item.id}`}
                        draggable
                        onDragStart={() => { dragPriorityRef.current = idx; }}
                        onDragOver={(e) => { e.preventDefault(); }}
                        onDrop={() => {
                          if (dragPriorityRef.current === null || dragPriorityRef.current === idx) return;
                          setSendPriority(prev => {
                            const next = [...prev];
                            const [moved] = next.splice(dragPriorityRef.current!, 1);
                            next.splice(idx, 0, moved);
                            return next;
                          });
                          dragPriorityRef.current = null;
                        }}
                        onDragEnd={() => { dragPriorityRef.current = null; }}
                        style={{
                          display: 'flex', alignItems: 'center', gap: 10, padding: '8px 12px',
                          background: idx === 0 ? `${accent}11` : '#0a0f1a',
                          border: `1.5px solid ${idx === 0 ? `${accent}4d` : 'rgba(0,200,255,0.06)'}`,
                          borderRadius: 8, cursor: 'grab', userSelect: 'none' as const,
                          transition: 'all 0.2s ease',
                          opacity: info ? 1 : 0.6,
                        }}
                      >
                        <FontAwesomeIcon icon={faGripVertical} style={{ color: 'rgba(180,210,240,0.25)', fontSize: 12 }} />
                        <div style={{
                          width: 24, height: 24, borderRadius: 6, display: 'flex', alignItems: 'center', justifyContent: 'center',
                          background: idx === 0 ? `${accent}33` : 'rgba(0,200,255,0.06)',
                          color: idx === 0 ? accent : 'rgba(180,210,240,0.5)',
                          fontSize: 12, fontWeight: 700,
                        }}>
                          {engagementPriorityRows.length + idx + 1}
                        </div>
                        <div style={{ flex: 1, minWidth: 0 }}>
                          <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                            <span style={{ fontSize: 12, fontWeight: 500, color: info ? '#e0e6f0' : 'rgba(180,210,240,0.4)', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
                              {label}
                            </span>
                            <span style={{
                              fontSize: 9, fontWeight: 600, padding: '1px 5px', borderRadius: 3,
                              background: `${accent}22`, color: accent, textTransform: 'uppercase', letterSpacing: '0.5px',
                              flexShrink: 0,
                            }}>
                              {isListItem ? 'List' : 'Segment'}
                            </span>
                          </div>
                          <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.4)', marginTop: 1 }}>
                            {count.toLocaleString()} {isListItem ? 'subscribers' : 'contacts'}
                            {idx === 0 && ' — sends first for warmup'}
                          </div>
                        </div>
                        <div style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
                          <button
                            onClick={() => movePriorityUp(idx)}
                            disabled={idx === 0}
                            style={{
                              width: 22, height: 18, display: 'flex', alignItems: 'center', justifyContent: 'center',
                              background: 'transparent', border: '1px solid rgba(180,210,240,0.15)', borderRadius: 4,
                              color: idx === 0 ? 'rgba(180,210,240,0.1)' : 'rgba(180,210,240,0.5)', cursor: idx === 0 ? 'default' : 'pointer',
                              fontSize: 9, padding: 0,
                            }}
                          >
                            <FontAwesomeIcon icon={faArrowUp} />
                          </button>
                          <button
                            onClick={() => movePriorityDown(idx)}
                            disabled={idx === sendPriority.length - 1}
                            style={{
                              width: 22, height: 18, display: 'flex', alignItems: 'center', justifyContent: 'center',
                              background: 'transparent', border: '1px solid rgba(180,210,240,0.15)', borderRadius: 4,
                              color: idx === sendPriority.length - 1 ? 'rgba(180,210,240,0.1)' : 'rgba(180,210,240,0.5)',
                              cursor: idx === sendPriority.length - 1 ? 'default' : 'pointer',
                              fontSize: 9, padding: 0,
                            }}
                          >
                            <FontAwesomeIcon icon={faArrowDown} />
                          </button>
                        </div>
                      </div>
                    );
                  })}
                </div>
              </div>
            )}

            {/* Audience funnel estimate */}
            <div style={{
              background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 16,
              position: 'relative', overflow: 'hidden',
            }}>
              <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 14 }}>
                <h4 style={{ margin: 0, fontSize: 13, color: '#e0e6f0', display: 'flex', alignItems: 'center', gap: 6 }}>
                  <FontAwesomeIcon icon={faChartBar} style={{ color: 'rgba(0,229,255,0.5)' }} /> Audience Pipeline
                </h4>
                {estimating && (
                  <span style={{ fontSize: 11, color: 'rgba(0,200,255,0.6)', display: 'flex', alignItems: 'center', gap: 4 }}>
                    <FontAwesomeIcon icon={faSpinner} spin /> Computing...
                  </span>
                )}
              </div>

              {!audienceEstimate && !estimating && (
                <div style={{ textAlign: 'center', padding: '20px 0', color: 'rgba(180,210,240,0.3)', fontSize: 12 }}>
                  Select lists or segments to see audience estimates
                </div>
              )}

              {!audienceEstimate && estimating && (
                <div style={{ display: 'flex', gap: 12 }}>
                  {[1, 2, 3].map(i => (
                    <div key={i} style={{ flex: 1, height: 72, background: 'rgba(0,200,255,0.04)', borderRadius: 8, animation: 'igShimmer 1.5s ease infinite', animationDelay: `${i * 0.2}s` }} />
                  ))}
                </div>
              )}

              {audienceEstimate && (
                <>
                  {/* Funnel visualization */}
                  <div style={{ display: 'flex', alignItems: 'center', gap: 0, marginBottom: 16 }}>
                    {/* Total */}
                    <div style={{ flex: 1, textAlign: 'center', position: 'relative' }}>
                      <div style={{ fontSize: 24, fontWeight: 700, color: '#00b0ff' }}>
                        <AnimatedCounter value={audienceEstimate.total_recipients} />
                      </div>
                      <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.5)', marginTop: 2 }}>Total Recipients</div>
                      <div style={{ position: 'absolute', bottom: -8, left: '10%', right: '10%', height: 3, borderRadius: 2, background: 'rgba(0,176,255,0.15)' }}>
                        <div style={{ height: '100%', width: '100%', borderRadius: 2, background: '#00b0ff' }} />
                      </div>
                    </div>
                    {/* Arrow */}
                    <div style={{ padding: '0 8px', color: 'rgba(180,210,240,0.2)', fontSize: 16 }}>→</div>
                    {/* Suppressed */}
                    <div style={{ flex: 1, textAlign: 'center', position: 'relative' }}>
                      <div style={{ fontSize: 24, fontWeight: 700, color: '#ef4444' }}>
                        -<AnimatedCounter value={audienceEstimate.suppressed_count} />
                      </div>
                      <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.5)', marginTop: 2 }}>Suppressed</div>
                      <div style={{ position: 'absolute', bottom: -8, left: '10%', right: '10%', height: 3, borderRadius: 2, background: 'rgba(239,68,68,0.15)' }}>
                        <div style={{
                          height: '100%', borderRadius: 2, background: '#ef4444',
                          width: audienceEstimate.total_recipients > 0 ? `${(audienceEstimate.suppressed_count / audienceEstimate.total_recipients) * 100}%` : '0%',
                          transition: 'width 0.5s ease',
                        }} />
                      </div>
                    </div>
                    {/* Arrow */}
                    <div style={{ padding: '0 8px', color: 'rgba(180,210,240,0.2)', fontSize: 16 }}>→</div>
                    {/* Net */}
                    <div style={{ flex: 1, textAlign: 'center', position: 'relative' }}>
                      <div style={{ fontSize: 24, fontWeight: 700, color: '#10b981' }}>
                        <AnimatedCounter value={audienceEstimate.after_suppressions} />
                      </div>
                      <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.5)', marginTop: 2 }}>Net Deliverable</div>
                      <div style={{ position: 'absolute', bottom: -8, left: '10%', right: '10%', height: 3, borderRadius: 2, background: 'rgba(16,185,129,0.15)' }}>
                        <div style={{ height: '100%', width: '100%', borderRadius: 2, background: '#10b981' }} />
                      </div>
                    </div>
                  </div>

                  {/* Suppression sources */}
                  {audienceEstimate.suppression_sources && Object.keys(audienceEstimate.suppression_sources).length > 0 && (
                    <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', marginBottom: 12 }}>
                      <span style={{ fontSize: 10, color: '#64748b', alignSelf: 'center', textTransform: 'uppercase', letterSpacing: 0.5 }}>Sources:</span>
                      {Object.entries(audienceEstimate.suppression_sources).map(([source, count]) => (
                        <span key={source} style={{ display: 'inline-flex', alignItems: 'center', gap: 3, padding: '2px 8px', borderRadius: 4, fontSize: 10, background: '#ef444412', color: '#ef4444', border: '1px solid #ef444425' }}>
                          {source}: {(count as number).toLocaleString()}
                        </span>
                      ))}
                    </div>
                  )}

                  {/* ISP breakdown bars */}
                  {audienceEstimate.isp_breakdown && Object.keys(audienceEstimate.isp_breakdown).length > 0 && (
                    <div>
                      <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.4)', marginBottom: 8, textTransform: 'uppercase', letterSpacing: 0.5 }}>Provider Distribution</div>
                      <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
                        {Object.entries(audienceEstimate.isp_breakdown)
                          .sort((a, b) => (b[1] as number) - (a[1] as number))
                          .map(([isp, count]) => {
                          const meta = ISP_META[isp];
                          const pct = audienceEstimate!.after_suppressions > 0 ? ((count as number) / audienceEstimate!.after_suppressions) * 100 : 0;
                          return (
                            <div key={isp} style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                              <span style={{ fontSize: 11, color: meta?.color || 'rgba(180,210,240,0.65)', minWidth: 80, display: 'flex', alignItems: 'center', gap: 4 }}>
                                {meta?.emoji || '🌐'} {meta?.label || isp}
                              </span>
                              <div style={{ flex: 1, height: 6, background: 'rgba(0,200,255,0.04)', borderRadius: 3, overflow: 'hidden' }}>
                                <div style={{
                                  height: '100%', borderRadius: 3,
                                  background: `linear-gradient(90deg, ${meta?.color || '#64748b'}, ${meta?.color || '#64748b'}88)`,
                                  width: `${Math.min(pct, 100)}%`,
                                  transition: 'width 0.5s ease',
                                }} />
                              </div>
                              <span style={{ fontSize: 10, color: 'rgba(180,210,240,0.5)', minWidth: 55, textAlign: 'right' }}>
                                {(count as number).toLocaleString()} <span style={{ color: 'rgba(180,210,240,0.3)' }}>({pct.toFixed(0)}%)</span>
                              </span>
                            </div>
                          );
                        })}
                      </div>
                    </div>
                  )}
                </>
              )}
            </div>
          </>
        )}
      </div>
    );
  };

  // ── Warm-up step 3: Newsletter ───────────────────────────────────────────
  // Replaces Offer + Creative. There is deliberately NO offer picker: offers
  // are banned in warm-up content.
  const renderWarmupStep3 = () => {
    const c = warmupCreative;
    const subjectOverridden = !!c && warmupSubject.trim() !== (c.subject || '').trim();
    const preheaderOverridden = !!c && warmupPreheader.trim() !== (c.preheader || '').trim();

    return (
      <div className="wiz-step-content ig-fade-in">
        <div style={{ marginBottom: 16 }}>
          <h3 style={{ margin: 0 }}>
            <FontAwesomeIcon icon={faNewspaper} style={{ marginRight: 8, color: '#38bdf8' }} />
            Newsletter<RequiredDot />
          </h3>
          <p style={{ margin: '4px 0 0', color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>
            The warm-up creative registered for <strong>{warmupBrandSlug || selectedDomain}</strong> today.
            Warm-up content is editorial — there is no offer to pick, and none may be introduced here.
            Subject and preheader are prefilled from the creative; editing either sends an override
            with the build request.
          </p>
        </div>
        <StepErrorBanner stepNum={3} />

        {/* Four distinct states — a failed fetch must never read as "nothing
            registered", so the error is checked BEFORE emptiness. */}
        {warmupCreativeLoading && (
          <div style={{ padding: '18px 14px', background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, fontSize: 13, color: '#7dd3fc' }}>
            <FontAwesomeIcon icon={faSpinner} spin /> Loading today&rsquo;s registered newsletter…
          </div>
        )}

        {!warmupCreativeLoading && warmupCreativeError && (
          <SectionError
            label="Registered newsletter"
            error={warmupCreativeError}
            onRetry={() => setWarmupCreativeKey(k => k + 1)}
          />
        )}

        {!warmupCreativeLoading && !warmupCreativeError && !c && (
          <div style={{ background: '#0d1526', border: '1px solid rgba(245,158,11,0.4)', borderRadius: 10, padding: 4 }}>
            <EmptyState
              icon={faNewspaper}
              title="No newsletter is registered for this property"
              hint={`Nothing has been registered in Creative Studio for "${warmupBrandSlug || selectedDomain}". This is not an empty creative — there is no row at all. Stage one (agents.jobs.kumo_newsletter_stage) before scheduling; a warm-up request with no creative has nothing to build.`}
            />
          </div>
        )}

        {!warmupCreativeLoading && !warmupCreativeError && c && (
          <>
            {/* FRESHNESS. `updated_at` is the registration time and the only
                honest freshness signal. `generated_at` is frozen at first
                insert — it is shown, but labelled "first created" so it can
                never be misread as "this is today's content". */}
            <div style={{
              background: warmupCreativeStale ? 'rgba(245,158,11,0.10)' : '#0d1526',
              border: `1.5px solid ${warmupCreativeStale ? '#f59e0b' : 'rgba(0,200,255,0.08)'}`,
              borderRadius: 10, padding: 14, marginBottom: 16,
            }}>
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: 12, flexWrap: 'wrap' }}>
                <div style={{ fontSize: 13, fontWeight: 600, color: warmupCreativeStale ? '#f59e0b' : '#10b981' }}>
                  <FontAwesomeIcon icon={faClock} style={{ marginRight: 6, fontSize: 11 }} />
                  Freshness — {warmupFreshness.text}
                </div>
                <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.45)' }}>
                  from <span style={{ fontFamily: 'monospace' }}>updated_at</span>
                </div>
              </div>

              {warmupCreativeStale && (
                <div style={{ fontSize: 12, color: '#f59e0b', marginTop: 8, lineHeight: 1.55 }}>
                  <FontAwesomeIcon icon={faExclamationTriangle} />{' '}
                  <strong>This creative has not been re-registered in over 24 hours.</strong> A stale
                  warm-up creative mails byte-identically forever with no error — that is exactly how
                  two properties re-sent the same articles for 15 days. Re-run the daily registration
                  for this property before queueing a build.
                </div>
              )}

              <div style={{ display: 'flex', gap: 18, flexWrap: 'wrap', marginTop: 10, fontSize: 11, color: 'rgba(180,210,240,0.55)' }}>
                <span title="Frozen at first insert. NOT a freshness signal.">
                  First created:{' '}
                  <strong style={{ color: '#e0e6f0' }}>
                    {c.generated_at ? new Date(c.generated_at).toLocaleString() : '—'}
                  </strong>
                </span>
                <span>Body: <strong style={{ color: '#e0e6f0' }}>{fmtBytes(c.html_bytes)}</strong></span>
                <span title={c.sha256 || ''}>
                  sha256:{' '}
                  <strong style={{ color: '#e0e6f0', fontFamily: 'monospace' }}>
                    {c.sha256 ? `${c.sha256.slice(0, 12)}…` : '—'}
                  </strong>
                </span>
                <span title={c.id}>
                  creative_id:{' '}
                  <strong style={{ color: '#e0e6f0', fontFamily: 'monospace' }}>
                    {c.id ? `${c.id.slice(0, 8)}…` : '—'}
                  </strong>
                </span>
                <button type="button" onClick={openWarmupPreview}
                  style={{ background: 'transparent', border: '1px solid rgba(0,200,255,0.18)', color: '#00b0ff', borderRadius: 6, padding: '3px 12px', fontSize: 11, cursor: 'pointer' }}>
                  <FontAwesomeIcon icon={faEye} /> Preview
                </button>
              </div>
            </div>

            {/* Editable copy — editable exactly where the operator acts. */}
            <div style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 14 }}>
              <h4 style={{ margin: '0 0 10px', fontSize: 13, color: '#e0e6f0' }}>Subject + preheader</h4>

              <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>
                Subject<RequiredDot />
              </label>
              <input
                value={warmupSubject}
                onChange={e => { setWarmupCopyTouched(true); setWarmupSubject(e.target.value); }}
                placeholder="Subject line for this send"
                style={{
                  width: '100%', boxSizing: 'border-box', background: '#0a0f1a', color: '#e0e6f0',
                  border: fieldBorder(!warmupSubject.trim()), borderRadius: 6, padding: '9px 11px', fontSize: 13,
                }}
              />
              <div style={{ fontSize: 10.5, color: subjectOverridden ? '#38bdf8' : 'rgba(180,210,240,0.45)', marginTop: 4, minHeight: 14 }}>
                {subjectOverridden
                  ? <>Override — the registered subject is &ldquo;{c.subject || '(none)'}&rdquo;.{' '}
                      <button type="button" onClick={() => setWarmupSubject(c.subject || '')}
                        style={{ background: 'transparent', border: 'none', color: '#00b0ff', fontSize: 10.5, cursor: 'pointer', padding: 0 }}>revert</button></>
                  : 'Matches the registered creative.'}
              </div>

              <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', display: 'block', margin: '12px 0 4px' }}>
                Preheader<RequiredDot />
              </label>
              <input
                value={warmupPreheader}
                onChange={e => { setWarmupCopyTouched(true); setWarmupPreheader(e.target.value); }}
                placeholder="Preview text shown after the subject in the inbox"
                style={{
                  width: '100%', boxSizing: 'border-box', background: '#0a0f1a', color: '#e0e6f0',
                  border: fieldBorder(!warmupPreheader.trim()), borderRadius: 6, padding: '9px 11px', fontSize: 13,
                }}
              />
              <div style={{ fontSize: 10.5, color: preheaderOverridden ? '#38bdf8' : 'rgba(180,210,240,0.45)', marginTop: 4, minHeight: 14 }}>
                {preheaderOverridden
                  ? <>Override — the registered preheader is &ldquo;{c.preheader || '(none)'}&rdquo;.{' '}
                      <button type="button" onClick={() => setWarmupPreheader(c.preheader || '')}
                        style={{ background: 'transparent', border: 'none', color: '#00b0ff', fontSize: 10.5, cursor: 'pointer', padding: 0 }}>revert</button></>
                  : 'Matches the registered creative.'}
              </div>
            </div>
          </>
        )}

        {warmupPreviewOpen && (
          <div onClick={() => setWarmupPreviewOpen(false)}
               style={{ position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.75)', zIndex: 9999, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 24 }}>
            <div onClick={e => e.stopPropagation()}
                 style={{ background: '#fff', borderRadius: 10, width: 'min(760px, 100%)', height: '85vh', overflow: 'hidden', display: 'flex', flexDirection: 'column' }}>
              <div style={{ padding: '8px 12px', background: '#0d1526', color: '#e0e6f0', fontSize: 12, display: 'flex', justifyContent: 'space-between' }}>
                <span>{warmupBrandSlug || selectedDomain} newsletter — {warmupFreshness.text}</span>
                <button onClick={() => setWarmupPreviewOpen(false)} style={{ background: 'transparent', border: 'none', color: '#00b0ff', cursor: 'pointer' }}>close</button>
              </div>
              {warmupPreviewLoading ? (
                <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', color: '#0f172a', fontSize: 13 }}>
                  <FontAwesomeIcon icon={faSpinner} spin /> &nbsp;Loading the newsletter body…
                </div>
              ) : warmupPreviewError ? (
                <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 24, color: '#b91c1c', fontSize: 13, textAlign: 'center' }}>
                  Could not load the body: {warmupPreviewError}
                </div>
              ) : warmupPreviewHtml ? (
                <iframe title="warm-up newsletter preview" srcDoc={warmupPreviewHtml} sandbox="" style={{ flex: 1, border: 'none', background: '#fff' }} />
              ) : (
                <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 24, color: '#b45309', fontSize: 13, textAlign: 'center' }}>
                  The creative endpoint returned metadata for this newsletter
                  ({fmtBytes(warmupCreative?.html_bytes)}, sha256 {warmupCreative?.sha256?.slice(0, 12) || '—'}…)
                  but no HTML body, so there is nothing to render here. This is a blank preview, not a
                  blank creative.
                </div>
              )}
            </div>
          </div>
        )}
      </div>
    );
  };

  // ── Warm-up step 4: Audience + Cold Source ───────────────────────────────
  // The engaged anchors stay the primary audience; the cold pad is ADDITIVE and
  // is always shown as its own number so the operator sees the MIX, never one
  // blended figure.
  const renderWarmupStep4 = () => {
    const cold = coldQuotaNum ?? 0;
    const mix = warmupEngagedMix;
    const totalKnown = mix.known + cold;

    return (
      <div className="wiz-step-content ig-fade-in">
        <h3 style={{ margin: '0 0 4px' }}>
          <FontAwesomeIcon icon={faSnowflake} style={{ marginRight: 8, color: '#38bdf8' }} />
          Audience + Cold Source<RequiredDot />
        </h3>
        <p style={{ margin: '0 0 16px', color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>
          Pick the engaged anchors this warm-up send leads with, then set how many cold records the
          builder pads with and where they come from.
        </p>
        <StepErrorBanner stepNum={4} />

        {kumoIllegalISPs.length > 0 && (
          <div style={{
            marginBottom: 16, padding: '10px 12px', borderRadius: 10,
            background: 'rgba(239,68,68,0.08)', border: '1px solid rgba(239,68,68,0.45)',
            fontSize: 12, color: '#ef4444',
          }}>
            <FontAwesomeIcon icon={faExclamationTriangle} />{' '}
            <strong>KumoMTA warm-up is yahoo-family only.</strong> Remove{' '}
            <strong>{kumoIllegalISPs.join(', ')}</strong> on the Mailbox Providers step — the estate
            registry caps yahoo, aol, att, sbcglobal and cox, and nothing else may send.
          </div>
        )}

        {/* ── Engaged anchors ──────────────────────────────────────────── */}
        <div style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 14, marginBottom: 16 }}>
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', marginBottom: 10, gap: 10 }}>
            <h4 style={{ margin: 0, fontSize: 13, color: '#e0e6f0' }}>
              <FontAwesomeIcon icon={faUsers} style={{ marginRight: 6, fontSize: 11, color: '#f59e0b' }} />
              Engaged anchor segments
            </h4>
            <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.45)' }}>
              {warmupSelectedSegmentIds.length} selected
            </span>
          </div>

          {warmupSegmentsLoading && (
            <div style={{ fontSize: 12, color: '#7dd3fc', padding: '10px 0' }}>
              <FontAwesomeIcon icon={faSpinner} spin /> Loading anchor segments for {warmupBrandSlug}…
            </div>
          )}

          {!warmupSegmentsLoading && warmupSegmentsError && (
            <SectionError
              label="Anchor segments"
              error={warmupSegmentsError}
              onRetry={() => setWarmupSegmentsKey(k => k + 1)}
            />
          )}

          {!warmupSegmentsLoading && !warmupSegmentsError && warmupSegments.length === 0 && (
            <EmptyState
              icon={faUsers}
              title="No anchor segments exist for this property"
              hint={`The segment list for "${warmupBrandSlug}" came back empty. A warm-up send can still run on a cold pad alone — but if you expected anchors here, build them before scheduling.`}
            />
          )}

          {!warmupSegmentsLoading && !warmupSegmentsError && warmupSegments.length > 0 && (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
              {warmupSegments.map(sg => {
                const picked = warmupSelectedSegmentIds.includes(sg.id);
                const cnt = warmupSegmentCount(sg);
                return (
                  <div
                    key={sg.id}
                    role="button"
                    tabIndex={0}
                    aria-pressed={picked}
                    onClick={() => setWarmupSelectedSegmentIds(prev =>
                      prev.includes(sg.id) ? prev.filter(x => x !== sg.id) : [...prev, sg.id])}
                    onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); setWarmupSelectedSegmentIds(prev => prev.includes(sg.id) ? prev.filter(x => x !== sg.id) : [...prev, sg.id]); } }}
                    style={{
                      display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 12,
                      background: picked ? 'rgba(0,200,255,0.08)' : '#0a0f1a',
                      border: `1.5px solid ${picked ? '#00b0ff' : 'rgba(0,200,255,0.08)'}`,
                      borderRadius: 8, padding: '10px 12px', cursor: 'pointer',
                    }}
                  >
                    <span style={{ fontSize: 12.5, color: '#e0e6f0', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={sg.name}>
                      {picked && <FontAwesomeIcon icon={faCheck} style={{ color: '#00b0ff', marginRight: 6, fontSize: 11 }} />}
                      {sg.name}
                    </span>
                    {/* subscriber_count is ZEROED on a refresh timeout, so 0 is
                        UNKNOWN. Never render it as a confident zero. */}
                    {cnt.known ? (
                      <span style={{ fontSize: 12, color: 'rgba(180,210,240,0.75)', fontVariantNumeric: 'tabular-nums', whiteSpace: 'nowrap' }}>
                        {cnt.value.toLocaleString()} <span style={{ color: 'rgba(180,210,240,0.4)' }}>subscribers</span>
                      </span>
                    ) : (
                      <span
                        title="subscriber_count is zeroed when a segment refresh times out — a healthy segment can read 0. Verify the build ledger before deploying."
                        style={{ fontSize: 11, color: '#f59e0b', whiteSpace: 'nowrap' }}
                      >
                        <FontAwesomeIcon icon={faExclamationTriangle} style={{ fontSize: 10 }} /> count unavailable — verify before deploying
                      </span>
                    )}
                  </div>
                );
              })}
            </div>
          )}
        </div>

        {/* ── Cold source + quota ──────────────────────────────────────── */}
        <div style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 14, marginBottom: 16 }}>
          <h4 style={{ margin: '0 0 4px', fontSize: 13, color: '#e0e6f0' }}>
            <FontAwesomeIcon icon={faSnowflake} style={{ marginRight: 6, fontSize: 11, color: '#38bdf8' }} />
            Cold pad
          </h4>
          <p style={{ margin: '0 0 12px', fontSize: 11.5, color: 'rgba(180,210,240,0.6)', lineHeight: 1.55 }}>
            How many cold records to pad this send with, and which source the builder pulls them from.
            Leave the quota empty or 0 to send to the engaged anchors only.
          </p>

          <div style={{ display: 'grid', gridTemplateColumns: '1.4fr 1fr', gap: 12 }}>
            <div>
              <label htmlFor="warmup-cold-source" style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>
                Cold source
              </label>
              <input
                id="warmup-cold-source"
                list={(warmupDomainRow?.cold_sources || []).length > 0 ? 'warmup-cold-sources' : undefined}
                value={coldSource}
                onChange={e => setColdSource(e.target.value)}
                placeholder="feed / dataset the builder pulls cold records from"
                style={{
                  width: '100%', boxSizing: 'border-box', background: '#0a0f1a', color: '#e0e6f0',
                  border: fieldBorder(cold > 0 && !coldSource.trim()), borderRadius: 6, padding: '9px 11px', fontSize: 13,
                }}
              />
              {(warmupDomainRow?.cold_sources || []).length > 0 && (
                <datalist id="warmup-cold-sources">
                  {(warmupDomainRow?.cold_sources || []).map(src => <option key={src} value={src} />)}
                </datalist>
              )}
              <div style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.45)', marginTop: 4 }}>
                {(warmupDomainRow?.cold_sources || []).length > 0
                  ? `${(warmupDomainRow?.cold_sources || []).length} known source${(warmupDomainRow?.cold_sources || []).length === 1 ? '' : 's'} for this property — type to filter.`
                  : 'No source list is published for this property, so this is free text — it is passed to the builder verbatim.'}
              </div>
            </div>

            <div>
              <label htmlFor="warmup-cold-quota" style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>
                Cold quota (records)
              </label>
              <input
                id="warmup-cold-quota"
                type="number"
                min={0}
                step={100}
                value={coldQuota}
                onChange={e => setColdQuota(e.target.value)}
                placeholder="0"
                style={{
                  width: '100%', boxSizing: 'border-box', background: '#0a0f1a', color: '#e0e6f0',
                  border: fieldBorder(coldQuota.trim() !== '' && coldQuotaNum === null),
                  borderRadius: 6, padding: '9px 11px', fontSize: 13, fontVariantNumeric: 'tabular-nums',
                }}
              />
              <div style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.45)', marginTop: 4 }}>
                {coldQuota.trim() === ''
                  ? 'Empty — no cold pad requested.'
                  : coldQuotaNum === null
                    ? 'Not a whole number of records.'
                    : `${coldQuotaNum.toLocaleString()} cold record${coldQuotaNum === 1 ? '' : 's'}.`}
              </div>
            </div>
          </div>
        </div>

        {/* ── The MIX. Engaged and cold are never blended into one number. ── */}
        <div style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 14 }}>
          <h4 style={{ margin: '0 0 10px', fontSize: 13, color: '#e0e6f0' }}>Audience mix</h4>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 12 }}>
            <div style={{ background: '#0a0f1a', border: '1px solid rgba(245,158,11,0.25)', borderRadius: 8, padding: 12 }}>
              <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.6)', marginBottom: 4 }}>
                Engaged ({mix.selected} segment{mix.selected === 1 ? '' : 's'})
              </div>
              <div style={{ fontSize: 20, fontWeight: 600, color: '#f59e0b', fontVariantNumeric: 'tabular-nums' }}>
                {mix.known.toLocaleString()}{mix.unknownSegments > 0 ? '+' : ''}
              </div>
              <div style={{ fontSize: 10.5, color: mix.unknownSegments > 0 ? '#f59e0b' : 'rgba(180,210,240,0.45)', marginTop: 3 }}>
                {mix.unknownSegments > 0
                  ? `${mix.unknownSegments} selected segment${mix.unknownSegments === 1 ? '' : 's'} report no count — the real figure is higher than this`
                  : 'sum of the selected anchor segments'}
              </div>
            </div>

            <div style={{ background: '#0a0f1a', border: '1px solid rgba(56,189,248,0.25)', borderRadius: 8, padding: 12 }}>
              <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.6)', marginBottom: 4 }}>Cold quota</div>
              <div style={{ fontSize: 20, fontWeight: 600, color: '#38bdf8', fontVariantNumeric: 'tabular-nums' }}>
                {cold.toLocaleString()}
              </div>
              <div style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.45)', marginTop: 3 }}>
                {coldSource.trim() ? `from ${coldSource.trim()}` : 'no source set'}
              </div>
            </div>

            <div style={{ background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.18)', borderRadius: 8, padding: 12 }}>
              <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.6)', marginBottom: 4 }}>Combined target</div>
              <div style={{ fontSize: 20, fontWeight: 600, color: '#e0e6f0', fontVariantNumeric: 'tabular-nums' }}>
                {mix.unknownSegments > 0 ? '≥ ' : ''}{totalKnown.toLocaleString()}
              </div>
              <div style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.45)', marginTop: 3 }}>
                engaged + cold, before suppression
              </div>
            </div>
          </div>
          <div style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.4)', marginTop: 10, lineHeight: 1.5 }}>
            These are PLANNED figures the builder works from — the engaged number is the segments&rsquo;
            own counts and the cold number is the quota you set, neither is a post-suppression
            estimate.
          </div>
        </div>
      </div>
    );
  };

  // ── The submit fork ──────────────────────────────────────────────────────
  // The single highest-blast-radius branch in this file: before, a third mode
  // fell through to handleDeploy() and POSTed an OFFER payload to
  // /pmta-campaign/deploy. It is now an exhaustive switch whose default is a
  // `never` check, so an unhandled mode cannot compile.
  const submitForActiveMode = useCallback(() => {
    switch (activeMode) {
      case 'offers':     handleDeploy(); return;
      case 'warmup':     handleWarmupRequest(); return;
      case 'newsletter': handleNewsletterRequest(); return;
      default:           assertUnreachableMode(activeMode);
    }
  }, [activeMode, handleDeploy, handleWarmupRequest, handleNewsletterRequest]);

  const renderWarmupRequestPanel = () => {
    const rows = warmupRequests || [];
    return (
      <div style={{ background: '#0d1526', border: '1px solid rgba(56,189,248,0.25)', borderRadius: 10, padding: 14, marginBottom: 16 }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: 10, flexWrap: 'wrap', marginBottom: 8 }}>
          <h4 style={{ margin: 0, fontSize: 13, color: '#e0e6f0' }}>
            <FontAwesomeIcon icon={faClock} style={{ marginRight: 6, fontSize: 11, color: '#38bdf8' }} />
            Build requests — {warmupRequestsDate} ({SEND_DAY_TIMEZONE})
          </h4>
          <span style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
            <span style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.4)' }}>
              {warmupRequestsLoading ? 'fetching…' : warmupRequestsFetchedAt ? `fetched ${warmupRequestsFetchedAt}` : ''}
            </span>
            <button type="button" onClick={() => setWarmupRequestsKey(k => k + 1)} disabled={warmupRequestsLoading}
              style={{ background: 'transparent', border: '1px solid rgba(0,200,255,0.18)', color: '#00b0ff', borderRadius: 6, padding: '3px 10px', fontSize: 11, cursor: warmupRequestsLoading ? 'default' : 'pointer' }}>
              <FontAwesomeIcon icon={faRotate} spin={warmupRequestsLoading} /> Refresh
            </button>
          </span>
        </div>

        <div style={{ fontSize: 11.5, color: 'rgba(180,210,240,0.6)', lineHeight: 1.55, marginBottom: 10 }}>
          Queueing a build request does <strong style={{ color: '#e0e6f0' }}>not</strong> send mail and
          does not create a campaign. It records the intent; a separate builder picks it up and takes
          roughly 40 minutes. Watch this ledger for
          {' '}<span style={{ color: WARMUP_STATUS_COLORS.requested }}>requested</span> →
          {' '}<span style={{ color: WARMUP_STATUS_COLORS.building }}>building</span> →
          {' '}<span style={{ color: WARMUP_STATUS_COLORS.built }}>built</span> /
          {' '}<span style={{ color: WARMUP_STATUS_COLORS.failed }}>failed</span>.
        </div>

        {/* error is checked BEFORE emptiness — a failed fetch must never read
            as "no requests for today". */}
        {warmupRequestsError ? (
          <SectionError
            label="Build-request ledger"
            error={`${warmupRequestsError} — the status of today's requests is UNKNOWN, not empty.`}
            onRetry={() => setWarmupRequestsKey(k => k + 1)}
          />
        ) : warmupRequestsLoading && warmupRequests === null ? (
          <div style={{ fontSize: 12, color: '#7dd3fc', padding: '8px 0' }}>
            <FontAwesomeIcon icon={faSpinner} spin /> Loading the build ledger…
          </div>
        ) : rows.length === 0 ? (
          <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.5)', padding: '8px 0' }}>
            No build requests recorded for {warmupRequestsDate}.
          </div>
        ) : (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
            {rows.map((r, i) => {
              const st = (r.status || 'unknown').toLowerCase();
              const col = WARMUP_STATUS_COLORS[st] || '#94a3b8';
              return (
                <div key={r.id || i} style={{ background: '#0a0f1a', border: `1px solid ${col}44`, borderRadius: 8, padding: '10px 12px' }}>
                  <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', gap: 10, flexWrap: 'wrap' }}>
                    <span style={{ fontSize: 12.5, color: '#e0e6f0' }}>
                      {r.sending_domain || r.brand_slug || '—'}
                      {r.scheduled_at && (
                        <span style={{ color: 'rgba(180,210,240,0.5)', fontSize: 11 }}>
                          {' '}· scheduled {new Date(r.scheduled_at).toLocaleString()}
                        </span>
                      )}
                    </span>
                    <span style={{ fontSize: 11, fontWeight: 600, color: col, textTransform: 'uppercase', letterSpacing: 0.4 }}>
                      {st}
                    </span>
                  </div>
                  <div style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.45)', marginTop: 4, fontVariantNumeric: 'tabular-nums' }}>
                    cold {r.cold_quota === undefined || r.cold_quota === null ? '—' : r.cold_quota.toLocaleString()}
                    {r.cold_source ? ` from ${r.cold_source}` : ''}
                    {r.updated_at ? ` · updated ${new Date(r.updated_at).toLocaleTimeString()}` : ''}
                  </div>
                  {st === 'failed' && (
                    <div style={{ fontSize: 11.5, color: '#fca5a5', marginTop: 6, lineHeight: 1.5 }}>
                      <FontAwesomeIcon icon={faExclamationTriangle} style={{ fontSize: 10 }} />{' '}
                      {r.build_note || 'The builder reported a failure but returned no build_note.'}
                    </div>
                  )}
                  {st !== 'failed' && r.build_note && (
                    <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.55)', marginTop: 6 }}>{r.build_note}</div>
                  )}
                </div>
              );
            })}
          </div>
        )}
      </div>
    );
  };

  const renderStep6 = () => (
    <div className="wiz-step-content ig-fade-in">
      <h3 style={{ margin: '0 0 16px' }}>
        {activeMode === 'offers' ? 'Review + Deploy' : 'Review + Queue for build'}
      </h3>
      <StepErrorBanner stepNum={6} />

      {/* ── Send-day anchors ─────────────────────────────────────────────
          The engaged tiers mail FIRST so they warm the inbox/IP before
          anything else arrives. Presets set the anchor, window and pacing the
          board compiles; every control below stays editable. */}
      <div style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 14, marginBottom: 16 }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', marginBottom: 8 }}>
          <h4 style={{ margin: 0, fontSize: 13, color: '#e0e6f0' }}>Send-day anchor</h4>
          <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.45)' }}>{SEND_DAY_TIMEZONE}</span>
        </div>
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
          {ANCHOR_PRESETS.map(preset => (
            <button key={preset.id} type="button" onClick={() => applyAnchorPreset(preset)}
              title={preset.hint}
              style={{
                display: 'flex', flexDirection: 'column', alignItems: 'flex-start', gap: 2,
                padding: '8px 12px', cursor: 'pointer', borderRadius: 8, textAlign: 'left',
                background: activePreset === preset.id ? 'rgba(0,176,255,0.14)' : '#0a0f1a',
                border: `1.5px solid ${activePreset === preset.id ? '#00b0ff' : 'rgba(0,200,255,0.08)'}`,
              }}>
              <span style={{ fontSize: 12, fontWeight: 600, color: activePreset === preset.id ? '#00b0ff' : '#e0e6f0' }}>{preset.label}</span>
              <span style={{ fontSize: 10, color: 'rgba(180,210,240,0.55)' }}>{preset.localTime} MT · {preset.windowHours}h window</span>
            </button>
          ))}
        </div>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12, marginTop: 12 }}>
          <div>
            <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>Campaign timezone</label>
            <input value={campaignTimezone} onChange={e => setCampaignTimezone(e.target.value)}
              style={{ width: '100%', background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 6, color: '#e0e6f0', padding: '8px 10px', fontSize: 12, boxSizing: 'border-box' }} />
          </div>
          <div>
            <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>Throttle profile</label>
            <select value={throttleStrategy} onChange={e => setThrottleStrategy(e.target.value)}
              style={{ width: '100%', background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 6, color: '#e0e6f0', padding: '8px 10px', fontSize: 12 }}>
              {THROTTLE_STRATEGIES.map(t => <option key={t} value={t}>{t}</option>)}
            </select>
          </div>
        </div>
        <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.45)', marginTop: 6 }}>
          The throttle profile is recorded on the plan for audit. Actual pacing is
          window ÷ interval: a {globalScheduleDuration}h window at every{' '}
          {globalScheduleInterval} min produces about{' '}
          {Math.max(4, Math.floor((globalScheduleDuration * 60) / Math.max(1, globalScheduleInterval)) + 1)} waves per provider,
          each sized automatically from the audience that actually qualifies.
        </div>
      </div>
      {loadingDraft && (
        <div style={{ marginBottom: 12, padding: '10px 12px', background: 'rgba(0,176,255,0.08)', border: '1px solid rgba(0,176,255,0.18)', borderRadius: 8, fontSize: 12, color: '#7dd3fc' }}>
          <FontAwesomeIcon icon={faSpinner} spin /> Loading saved draft state...
        </div>
      )}
      {draftError && (
        <div style={{ marginBottom: 12, padding: '10px 12px', background: 'rgba(239,68,68,0.08)', border: '1px solid rgba(239,68,68,0.2)', borderRadius: 8, fontSize: 12, color: '#fca5a5' }}>
          <FontAwesomeIcon icon={faExclamationTriangle} /> {draftError}
        </div>
      )}
      {!loadingDraft && campaignId && (
        <div style={{ marginBottom: 12, padding: '10px 12px', background: 'rgba(16,185,129,0.08)', border: '1px solid rgba(16,185,129,0.18)', borderRadius: 8, fontSize: 12, color: '#86efac' }}>
          <strong>Draft linked</strong> {campaignId}
          {draftStatus ? ` · ${draftStatus}` : ''}
        </div>
      )}
      {!loadingDraft && !campaignId && draftStatus && (
        <div style={{ marginBottom: 12, padding: '10px 12px', background: 'rgba(16,185,129,0.08)', border: '1px solid rgba(16,185,129,0.18)', borderRadius: 8, fontSize: 12, color: '#86efac' }}>
          {draftStatus}
        </div>
      )}

      {gateFailure && (
        <div style={{ marginBottom: 16, padding: 14, background: 'rgba(245,158,11,0.08)', border: '1px solid rgba(245,158,11,0.4)', borderRadius: 10 }}>
          <div style={{ fontSize: 13, fontWeight: 600, color: '#f59e0b', marginBottom: 6 }}>
            <FontAwesomeIcon icon={faExclamationTriangle} /> {gateFailure.error}
          </div>
          <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.7)', marginBottom: 10 }}>
            The send-day gates are a board discipline, not a code error. Clear the gate, or deploy
            anyway with a reason — every override is audit-logged.
          </div>
          {gateFailure.failed_gates.map((g: any, i: number) => (
            <div key={i} style={{ fontSize: 12, color: '#e0e6f0', padding: '4px 0', borderTop: i === 0 ? 'none' : '1px solid rgba(245,158,11,0.15)' }}>
              <strong>Gate {g.Gate || g.gate}</strong> — {g.Name || g.name}
              <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.6)' }}>{g.Detail || g.detail}</div>
            </div>
          ))}
          <input
            value={gateOverrideReason}
            onChange={e => setGateOverrideReason(e.target.value)}
            placeholder="Why must this deploy proceed? (recorded in the audit log)"
            style={{ width: '100%', marginTop: 10, background: '#0a0f1a', border: '1px solid rgba(245,158,11,0.3)', borderRadius: 6, color: '#e0e6f0', padding: '8px 10px', fontSize: 12, boxSizing: 'border-box' }}
          />
          <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
            <button
              onClick={() => handleDeploy(gateOverrideReason)}
              disabled={deploying || gateOverrideReason.trim().length < 10}
              style={{
                background: gateOverrideReason.trim().length >= 10 ? '#f59e0b' : 'rgba(245,158,11,0.25)',
                color: '#0a0f1a', border: 'none', borderRadius: 6, padding: '7px 16px', fontSize: 12,
                fontWeight: 600, cursor: gateOverrideReason.trim().length >= 10 ? 'pointer' : 'default',
              }}>
              {deploying ? 'Deploying…' : 'Deploy with override'}
            </button>
            <button onClick={() => { setGateFailure(null); setGateOverrideReason(''); }}
              style={{ background: 'transparent', color: '#e0e6f0', border: '1px solid rgba(0,200,255,0.12)', borderRadius: 6, padding: '7px 16px', fontSize: 12, cursor: 'pointer' }}>
              Cancel
            </button>
          </div>
        </div>
      )}

      {/* Warm-up branch: the build ledger sits above the form and stays
          visible whether or not a request has just been queued. */}
      {(warmupActive || newsletterActive) && renderWarmupRequestPanel()}

      {/* NEWSLETTERS: ONE scheduled instant, N sending domains. The single
          time and the exact fan-out are stated together so the operator can
          see what "one time for all domains" resolves to before submitting. */}
      {newsletterActive && (
        <div style={{ background: '#0d1526', border: '1px solid rgba(56,189,248,0.25)', borderRadius: 10, padding: 14, marginBottom: 16 }}>
          <h4 style={{ margin: '0 0 10px', fontSize: 13, color: '#e0e6f0' }}>
            <FontAwesomeIcon icon={faNewspaper} style={{ marginRight: 6, fontSize: 11, color: '#38bdf8' }} />
            This run
          </h4>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 12 }}>
            <div style={{ background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.18)', borderRadius: 8, padding: 12 }}>
              <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.6)', marginBottom: 4 }}>Sending domains</div>
              <div style={{ fontSize: 20, fontWeight: 600, color: '#e0e6f0', fontVariantNumeric: 'tabular-nums' }}>
                {newsletterIncluded.length}
              </div>
              <div style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.45)', marginTop: 3 }}>
                one build request each — {newsletterTally.total - newsletterTally.included} excluded
              </div>
            </div>
            <div style={{ background: '#0a0f1a', border: `1px solid ${intentScheduledAtISO ? 'rgba(56,189,248,0.35)' : 'rgba(239,68,68,0.45)'}`, borderRadius: 8, padding: 12 }}>
              <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.6)', marginBottom: 4 }}>
                Scheduled time — applies to ALL of them
              </div>
              <div style={{ fontSize: 14, fontWeight: 600, color: intentScheduledAtISO ? '#38bdf8' : '#ef4444', lineHeight: 1.4 }}>
                {intentScheduledAtISO ? new Date(intentScheduledAtISO).toLocaleString() : 'not set'}
              </div>
              <div style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.45)', marginTop: 3 }}>
                {intentScheduledAtISO
                  ? 'the earliest instant set in the schedule controls below'
                  : 'set it in the schedule controls below'}
              </div>
            </div>
            <div style={{ background: '#0a0f1a', border: '1px solid rgba(245,158,11,0.25)', borderRadius: 8, padding: 12 }}>
              <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.6)', marginBottom: 4 }}>Per-provider quota</div>
              <div style={{ fontSize: 20, fontWeight: 600, color: audienceBound ? '#f59e0b' : '#e0e6f0', fontVariantNumeric: 'tabular-nums' }}>
                {audienceBound ? '0' : 'capped'}
              </div>
              <div style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.45)', marginTop: 3 }}>
                {audienceBound
                  ? `audience-bound across ${selectedISPs.length} provider${selectedISPs.length === 1 ? '' : 's'}`
                  : 'finite per-ISP caps from the providers step'}
              </div>
            </div>
          </div>
          <div style={{ fontSize: 11, color: '#f59e0b', marginTop: 10, lineHeight: 1.55 }}>
            <FontAwesomeIcon icon={faExclamationTriangle} style={{ fontSize: 10 }} />{' '}
            The selected mailbox providers apply to <strong>every</strong> included domain. This screen
            does not know each domain&rsquo;s transport, so it cannot tell you that a KumoMTA-routed
            property is yahoo-family only — the builder enforces that per domain.
          </div>
          <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.55)', marginTop: 8, lineHeight: 1.55 }}>
            Queueing records <strong style={{ color: '#e0e6f0' }}>{newsletterIncluded.length}</strong> build
            request{newsletterIncluded.length === 1 ? '' : 's'}:{' '}
            <span style={{ fontFamily: 'monospace', fontSize: 10.5 }}>
              {newsletterIncluded.map(v => v.row.sending_domain).join(', ') || '—'}
            </span>
          </div>
        </div>
      )}

      {newsletterActive && newsletterResults ? (
        <div style={{ padding: '24px 0' }}>
          <div style={{ textAlign: 'center', color: '#38bdf8', marginBottom: 16 }}>
            <FontAwesomeIcon icon={faClock} size="3x" style={{ marginBottom: 12 }} />
            {/* NOT "deployed", NOT "sending" — nothing has been sent. */}
            <h3 style={{ margin: '0 0 6px' }}>
              Queued for build — {newsletterResults.filter(r => r.ok).length} of {newsletterResults.length}
            </h3>
            <p style={{ margin: 0, fontSize: 13, color: 'rgba(180,210,240,0.75)', lineHeight: 1.6 }}>
              <strong style={{ color: '#e0e6f0' }}>No mail has been sent and no campaign exists yet.</strong>{' '}
              A separate builder consumes these requests; the ledger above moves requested → building →
              built as it does. Every domain is listed individually — a partial failure is never reported
              as success.
            </p>
          </div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
            {newsletterResults.map(r => (
              <div key={r.sending_domain} style={{
                background: '#0a0f1a', borderRadius: 8, padding: '9px 11px',
                border: `1px solid ${r.ok ? 'rgba(16,185,129,0.35)' : 'rgba(239,68,68,0.45)'}`,
              }}>
                <div style={{ display: 'flex', justifyContent: 'space-between', gap: 10, flexWrap: 'wrap' }}>
                  <span style={{ fontSize: 12.5, color: '#e0e6f0', fontFamily: 'monospace' }}>{r.sending_domain}</span>
                  <span style={{ fontSize: 11, fontWeight: 700, letterSpacing: 0.4, textTransform: 'uppercase', color: r.ok ? '#10b981' : '#ef4444' }}>
                    {r.ok ? 'queued' : 'not queued'}
                  </span>
                </div>
                {r.ok
                  ? r.request_id && (
                      <div style={{ fontSize: 10.5, color: 'rgba(180,210,240,0.45)', marginTop: 4, fontFamily: 'monospace' }}>
                        request {r.request_id}
                      </div>
                    )
                  : (
                    <div style={{ fontSize: 11.5, color: '#fca5a5', marginTop: 5, lineHeight: 1.5 }}>
                      <FontAwesomeIcon icon={faTimesCircle} style={{ fontSize: 10 }} /> {r.error || 'unknown error'} — nothing was queued for this domain.
                    </div>
                  )}
              </div>
            ))}
          </div>
          <div style={{ textAlign: 'center', marginTop: 16 }}>
            <button type="button" onClick={() => setNewsletterResults(null)}
              style={{ background: 'transparent', border: '1px solid rgba(0,200,255,0.18)', color: '#00b0ff', borderRadius: 8, padding: '8px 18px', fontSize: 12.5, cursor: 'pointer' }}>
              Back to the form
            </button>
          </div>
        </div>
      ) : warmupActive && warmupResult && !warmupResult.error ? (
        <div style={{ textAlign: 'center', padding: 40 }}>
          <div style={{ color: '#38bdf8' }}>
            <FontAwesomeIcon icon={faClock} size="3x" style={{ marginBottom: 12 }} />
            {/* NOT "deployed", NOT "sending" — nothing has been sent. */}
            <h3 style={{ margin: '0 0 6px' }}>Queued for build</h3>
            <p style={{ margin: '0 0 4px', fontSize: 13, color: 'rgba(180,210,240,0.75)', lineHeight: 1.6 }}>
              The warm-up request for <strong style={{ color: '#e0e6f0' }}>{selectedDomain}</strong> is
              recorded. <strong style={{ color: '#e0e6f0' }}>No mail has been sent and no campaign
              exists yet.</strong> A separate builder consumes this request and takes roughly
              40 minutes; the ledger above moves requested → building → built when it does.
            </p>
            {warmupResult.request?.id && (
              <p style={{ fontSize: 11, color: 'rgba(180,210,240,0.45)', fontFamily: 'monospace' }}>
                request {warmupResult.request.id}
              </p>
            )}
            <div style={{ display: 'flex', gap: 12, justifyContent: 'center', marginTop: 16 }}>
              <button onClick={() => setWarmupRequestsKey(k => k + 1)}
                style={{ background: 'transparent', color: '#e0e6f0', border: '1px solid rgba(0,200,255,0.18)', borderRadius: 8, padding: '10px 24px', fontSize: 14, cursor: 'pointer' }}>
                <FontAwesomeIcon icon={faRotate} /> Refresh status
              </button>
              <button onClick={() => setWarmupResult(null)}
                style={{ background: 'transparent', color: '#e0e6f0', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 8, padding: '10px 24px', fontSize: 14, cursor: 'pointer' }}>
                Edit request
              </button>
              <button onClick={onClose}
                style={{ background: '#00b0ff', color: '#fff', border: 'none', borderRadius: 8, padding: '10px 24px', fontSize: 14, cursor: 'pointer' }}>
                Done
              </button>
            </div>
          </div>
        </div>
      ) : deployResult ? (
        <div style={{ textAlign: 'center', padding: 40 }}>
          {deployResult.error ? (
            <div style={{ color: '#ef4444' }}>
              <FontAwesomeIcon icon={faTimesCircle} size="3x" style={{ marginBottom: 12 }} />
              <h3>Deploy Failed</h3>
              <p>{deployResult.error}</p>
              <div style={{ display: 'flex', gap: 12, justifyContent: 'center', marginTop: 16 }}>
                <button onClick={() => handleDeploy()} disabled={deploying}
                  style={{ background: '#00b0ff', color: '#fff', border: 'none', borderRadius: 8, padding: '10px 24px', fontSize: 14, cursor: 'pointer' }}>
                  {deploying ? 'Retrying…' : 'Retry Deploy'}
                </button>
                <button onClick={() => setDeployResult(null)}
                  style={{ background: 'transparent', color: '#e0e6f0', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 8, padding: '10px 24px', fontSize: 14, cursor: 'pointer' }}>
                  Edit Campaign
                </button>
              </div>
            </div>
          ) : (
            <div style={{ color: '#10b981' }}>
              <FontAwesomeIcon icon={faCheckCircle} size="3x" style={{ marginBottom: 12 }} />
              <h3>Campaign Created</h3>
              <p>ID: {deployResult.campaign_id}</p>
              <p>{deployResult.variant_count} variant{deployResult.variant_count > 1 ? 's' : ''} targeting {deployResult.target_isps?.length} mailbox provider{deployResult.target_isps?.length > 1 ? 's' : ''}</p>
              <button onClick={onClose} style={{ marginTop: 16, background: '#00b0ff', color: '#fff', border: 'none', borderRadius: 8, padding: '10px 24px', fontSize: 14, cursor: 'pointer' }}>
                Done
              </button>
            </div>
          )}
        </div>
      ) : (
        <>
          {/* Campaign name */}
          <div style={{ marginBottom: 16 }}>
            <label style={{ fontSize: 12, color: showErr(6) && !campaignName.trim() ? '#ef4444' : 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>Campaign Name<RequiredDot /></label>
            <input
              value={campaignName} placeholder="e.g. Q1 Gmail Warmup Blast"
              onChange={e => { setNameTouched(true); setCampaignName(e.target.value); }}
              style={{ width: '100%', background: '#0a0f1a', border: fieldBorder(!campaignName.trim()), borderRadius: 8, color: '#e0e6f0', padding: '10px 12px', fontSize: 14, boxSizing: 'border-box', transition: 'border-color 0.2s' }}
            />
            {showErr(6) && !campaignName.trim() && <div style={{ fontSize: 10, color: '#ef4444', marginTop: 3 }}>Campaign name is required</div>}
          </div>

          {/* Send mode toggle */}
          <div style={{ display: 'flex', gap: 8, marginBottom: 16 }}>
            {(['immediate', 'scheduled'] as const).map(mode => (
              <button
                key={mode}
                // Switching to "Send Now" no longer WIPES the chosen anchor.
                // buildCampaignPayload only emits scheduled_at when sendMode is
                // 'scheduled' (:2718) and every step-6 schedule check is gated
                // the same way, so holding the value is inert while immediate —
                // and it means a Send Now / Schedule for Later round trip gives
                // the operator back the anchor they picked instead of silently
                // re-seeding 01:01 tomorrow underneath a still-highlighted preset.
                onClick={() => setSendMode(mode)}
                style={{
                  flex: 1, padding: '10px 0', borderRadius: 8, fontSize: 13, fontWeight: 600,
                  cursor: 'pointer', transition: 'all 0.2s',
                  background: sendMode === mode ? (mode === 'immediate' ? 'rgba(0,200,255,0.12)' : '#f59e0b20') : '#0d1526',
                  color: sendMode === mode ? (mode === 'immediate' ? '#00b0ff' : '#f59e0b') : 'rgba(180,210,240,0.65)',
                  border: `2px solid ${sendMode === mode ? (mode === 'immediate' ? '#00b0ff' : '#f59e0b') : 'rgba(0,200,255,0.08)'}`,
                }}
              >
                {mode === 'immediate' ? 'Send Now' : 'Schedule for Later'}
              </button>
            ))}
          </div>

          {/* Scheduled: recommendations + date picker */}
          {sendMode === 'scheduled' && (
            <div style={{ marginBottom: 16 }}>
              <div style={{ display: 'flex', gap: 8, marginBottom: 12 }}>
                {(['quick', 'per-isp'] as const).map(mode => (
                  <button
                    key={mode}
                    onClick={() => setScheduleMode(mode)}
                    style={{
                      flex: 1,
                      padding: '10px 0',
                      borderRadius: 8,
                      fontSize: 13,
                      fontWeight: 600,
                      cursor: 'pointer',
                      transition: 'all 0.2s',
                      background: scheduleMode === mode ? 'rgba(0,200,255,0.12)' : '#0d1526',
                      color: scheduleMode === mode ? '#00b0ff' : 'rgba(180,210,240,0.65)',
                      border: `2px solid ${scheduleMode === mode ? '#00b0ff' : 'rgba(0,200,255,0.08)'}`,
                    }}
                  >
                    {mode === 'quick' ? 'Quick Schedule' : 'Per-Provider Plans'}
                  </button>
                ))}
              </div>
              {recsLoading && (
                <div style={{ display: 'flex', flexDirection: 'column', gap: 8, marginBottom: 12 }}>
                  {[1, 2, 3].map(i => (
                    <div key={i} style={{ height: 48, background: 'linear-gradient(90deg, #0d1526 25%, rgba(0,200,255,0.08) 50%, #0d1526 75%)', borderRadius: 8, animation: 'shimmer 1.5s infinite' }} />
                  ))}
                </div>
              )}
              {!recsLoading && recommendations.length > 0 && (
                <div style={{ marginBottom: 12 }}>
                  <h4 style={{ margin: '0 0 8px', fontSize: 13, color: '#e0e6f0' }}>Recommended Send Windows</h4>
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
                    {recommendations.map((rec) => {
                      const meta = ISP_META[rec.isp];
                      return (
                        <div key={rec.isp} style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 8, padding: 10 }}>
                          <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 6 }}>
                            <span>{meta?.emoji || '🌐'}</span>
                            <strong style={{ color: meta?.color || '#e0e6f0', fontSize: 13 }}>{rec.display_name}</strong>
                            <span style={{
                              fontSize: 10, padding: '2px 6px', borderRadius: 4, fontWeight: 600,
                              background: rec.data_quality?.has_historical ? '#10b98120' : '#64748b20',
                              color: rec.data_quality?.has_historical ? '#10b981' : 'rgba(180,210,240,0.65)',
                              border: `1px solid ${rec.data_quality?.has_historical ? '#10b98140' : '#64748b40'}`,
                            }}>
                              {rec.data_quality?.has_historical
                                ? `Based on ${(rec.data_quality.total_sends || 0).toLocaleString()} sends`
                                : 'Industry standard'}
                            </span>
                          </div>
                          <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
                            {(rec.windows || []).slice(0, 3).map((w, i: number) => (
                              <button
                                key={i}
                                onClick={() => {
                                  const { start, end } = nextScheduleFromWindow(w);
                                  if (scheduleMode === 'quick') {
                                    setScheduledAt(toDateTimeLocal(start));
                                  } else {
                                    addTimeSpanToPlan(rec.isp, {
                                      startAt: toDateTimeLocal(start),
                                      endAt: toDateTimeLocal(end),
                                      timezone: ispPlansByKey[rec.isp]?.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC',
                                      source: w.source,
                                    });
                                  }
                                }}
                                style={{
                                  padding: '4px 10px', borderRadius: 6, fontSize: 11, cursor: 'pointer',
                                  background: w.source === 'historical' ? 'rgba(0,200,255,0.08)' : '#0a1628',
                                  color: w.source === 'historical' ? '#00b0ff' : 'rgba(180,210,240,0.65)',
                                  border: `1px solid ${w.source === 'historical' ? 'rgba(0,200,255,0.25)' : 'rgba(0,200,255,0.08)'}`,
                                }}
                              >
                                {w.day_of_week} {w.start_hour}:00–{w.end_hour}:00 UTC
                                {w.source === 'historical' && ` (${w.open_rate.toFixed(1)}% open)`}
                              </button>
                            ))}
                          </div>
                          {rec.data_quality?.has_historical && (
                            <div style={{ marginTop: 4, height: 3, borderRadius: 2, background: 'rgba(0,200,255,0.08)', overflow: 'hidden' }}>
                              <div className="ig-progress-fill" style={{ height: '100%', width: `${Math.min((rec.data_quality.total_sends / 1000) * 100, 100)}%`, background: '#10b981', borderRadius: 2 }} />
                            </div>
                          )}
                        </div>
                      );
                    })}
                  </div>
                </div>
              )}
              {scheduleMode === 'quick' ? (
                <div>
                  <label style={{ fontSize: 12, color: showErr(6) && !scheduledAt ? '#ef4444' : 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>Send Date & Time<RequiredDot /></label>
                  <input
                    type="datetime-local"
                    value={scheduledAt}
                    onChange={e => { setScheduleSeeded(true); setScheduledAt(e.target.value); }}
                    min={toDateTimeLocal(new Date(Date.now() + 5 * 60 * 1000))}
                    style={{ width: '100%', background: '#0a0f1a', border: fieldBorder(!scheduledAt), borderRadius: 8, color: '#e0e6f0', padding: '10px 12px', fontSize: 14, boxSizing: 'border-box', transition: 'border-color 0.2s' }}
                  />
                  {showErr(6) && !scheduledAt && <div style={{ fontSize: 10, color: '#ef4444', marginTop: 3 }}>Scheduled date and time is required</div>}
                </div>
              ) : (
                <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
                  {/* Apply to All global settings */}
                  <div style={{ background: '#0d1526', border: '1px solid rgba(0,229,255,0.15)', borderRadius: 10, padding: 14 }}>
                    <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 10 }}>
                      <div style={{ width: 4, height: 16, borderRadius: 2, background: '#00e5ff' }} />
                      <h4 style={{ margin: 0, fontSize: 13, color: '#00e5ff', fontWeight: 600 }}>Global Settings</h4>
                      <span style={{ fontSize: 10, color: 'rgba(180,210,240,0.4)', marginLeft: 'auto' }}>Configure once, apply to all providers</span>
                    </div>
                    <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr 1fr', gap: 10, marginBottom: 10 }}>
                      <div>
                        <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>Start Time</label>
                        <input
                          type="datetime-local"
                          value={globalScheduleStart}
                          onChange={e => setGlobalScheduleStart(e.target.value)}
                          min={toDateTimeLocal(new Date(Date.now() + 5 * 60 * 1000))}
                          style={{ width: '100%', background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 6, color: '#e0e6f0', padding: '8px 10px', fontSize: 12, boxSizing: 'border-box' }}
                        />
                      </div>
                      <div>
                        <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>Duration (hours)</label>
                        <select
                          value={globalScheduleDuration}
                          onChange={e => setGlobalScheduleDuration(Number(e.target.value))}
                          style={{ width: '100%', background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 6, color: '#e0e6f0', padding: '8px 10px', fontSize: 12 }}
                        >
                          {[1, 2, 4, 6, 8, 10, 12, 16, 24].map(h => (
                            <option key={h} value={h}>{h} hour{h > 1 ? 's' : ''}</option>
                          ))}
                        </select>
                      </div>
                      <div>
                        <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>Interval (min)</label>
                        <select
                          value={globalScheduleInterval}
                          onChange={e => setGlobalScheduleInterval(Number(e.target.value))}
                          style={{ width: '100%', background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 6, color: '#e0e6f0', padding: '8px 10px', fontSize: 12 }}
                        >
                          {[5, 10, 15, 30, 60].map(m => (
                            <option key={m} value={m}>Every {m} min</option>
                          ))}
                        </select>
                      </div>
                      <div>
                        <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>Timezone</label>
                        <input
                          value={globalScheduleTimezone}
                          onChange={e => setGlobalScheduleTimezone(e.target.value)}
                          style={{ width: '100%', background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 6, color: '#e0e6f0', padding: '8px 10px', fontSize: 12, boxSizing: 'border-box' }}
                        />
                      </div>
                    </div>
                    <button
                      onClick={() => {
                        setISPPlansByKey(prev => {
                          const next: Record<string, ISPPlanFormState> = {};
                          selectedISPs.forEach(isp => {
                            const existing = prev[isp] || buildDefaultISPPlan(isp);
                            next[isp] = {
                              ...existing,
                              useCustomSchedule: true,
                              timezone: globalScheduleTimezone,
                              cadenceMode: 'interval',
                              everyMinutes: globalScheduleInterval,
                              durationHours: globalScheduleDuration,
                              startTime: globalScheduleStart,
                              timeSpans: [],
                            };
                          });
                          return next;
                        });
                      }}
                      style={{
                        width: '100%', padding: '8px 14px', borderRadius: 6, fontSize: 12, fontWeight: 600, cursor: 'pointer',
                        background: 'rgba(0,229,255,0.12)', color: '#00e5ff', border: '1px solid rgba(0,229,255,0.25)',
                        transition: 'all 0.2s',
                      }}
                    >
                      Apply to All Providers
                    </button>
                  </div>

                  {/* Per-ISP cards */}
                  {selectedISPs.map(isp => {
                    const plan = ispPlansByKey[isp] || buildDefaultISPPlan(isp);
                    const meta = ISP_META[isp];
                    const quota = ispQuotas[isp] || 0;
                    const dur = plan.durationHours || 8;
                    const interval = plan.everyMinutes || 15;
                    const totalIntervals = Math.max(1, Math.floor(dur * 60 / interval));
                    const msgsPerInterval = quota > 0 ? Math.ceil(quota / totalIntervals) : 0;
                    const msgsPerHour = msgsPerInterval * Math.floor(60 / interval);
                    return (
                      <div key={isp} style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 14 }}>
                        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8, marginBottom: 10 }}>
                          <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                            <span style={{ fontSize: 13, fontWeight: 700, color: meta?.color || '#e0e6f0' }}>
                              {meta?.emoji || '🌐'} {meta?.label || isp}
                            </span>
                            <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)', background: 'rgba(0,200,255,0.06)', padding: '2px 8px', borderRadius: 4 }}>
                              Quota: {quota.toLocaleString()}
                            </span>
                          </div>
                          <label style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 11, color: 'rgba(180,210,240,0.65)' }}>
                            <input
                              type="checkbox"
                              checked={plan.useCustomSchedule}
                              onChange={e => updateISPPlan(isp, curr => ({ ...curr, useCustomSchedule: e.target.checked }))}
                            />
                            Custom schedule
                          </label>
                        </div>

                        {plan.useCustomSchedule && (
                          <>
                            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr 1fr', gap: 10, marginBottom: 10 }}>
                              <div>
                                <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>Start Time</label>
                                <input
                                  type="datetime-local"
                                  value={plan.startTime}
                                  onChange={e => updateISPPlan(isp, curr => ({ ...curr, startTime: e.target.value }))}
                                  min={toDateTimeLocal(new Date(Date.now() + 5 * 60 * 1000))}
                                  style={{ width: '100%', background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 6, color: '#e0e6f0', padding: '8px 10px', fontSize: 12, boxSizing: 'border-box' }}
                                />
                              </div>
                              <div>
                                <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>Duration</label>
                                <select
                                  value={plan.durationHours}
                                  onChange={e => updateISPPlan(isp, curr => ({ ...curr, durationHours: Number(e.target.value) }))}
                                  style={{ width: '100%', background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 6, color: '#e0e6f0', padding: '8px 10px', fontSize: 12 }}
                                >
                                  {[1, 2, 4, 6, 8, 10, 12, 16, 24].map(h => (
                                    <option key={h} value={h}>{h}h</option>
                                  ))}
                                </select>
                              </div>
                              <div>
                                <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>Interval</label>
                                <select
                                  value={plan.everyMinutes}
                                  onChange={e => updateISPPlan(isp, curr => ({ ...curr, everyMinutes: Number(e.target.value) }))}
                                  style={{ width: '100%', background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 6, color: '#e0e6f0', padding: '8px 10px', fontSize: 12 }}
                                >
                                  {[5, 10, 15, 30, 60].map(m => (
                                    <option key={m} value={m}>{m} min</option>
                                  ))}
                                </select>
                              </div>
                              <div>
                                <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>Timezone</label>
                                <input
                                  value={plan.timezone}
                                  onChange={e => updateISPPlan(isp, curr => ({ ...curr, timezone: e.target.value }))}
                                  style={{ width: '100%', background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 6, color: '#e0e6f0', padding: '8px 10px', fontSize: 12, boxSizing: 'border-box' }}
                                />
                              </div>
                            </div>

                            {/* Dynamic throughput calculation */}
                            {quota > 0 && (
                              <div style={{
                                background: 'rgba(0,200,255,0.04)', border: '1px solid rgba(0,200,255,0.08)',
                                borderRadius: 6, padding: '8px 12px', marginBottom: 10,
                                display: 'flex', gap: 16, alignItems: 'center', flexWrap: 'wrap',
                              }}>
                                <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)' }}>
                                  {totalIntervals} intervals
                                </span>
                                <span style={{ fontSize: 11, color: '#00e5ff', fontWeight: 600 }}>
                                  ~{msgsPerInterval} msgs/{interval}min
                                </span>
                                <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)' }}>
                                  ~{msgsPerHour.toLocaleString()} msgs/hr
                                </span>
                                <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)' }}>
                                  Batch size: {msgsPerInterval}
                                </span>
                              </div>
                            )}
                          </>
                        )}

                        {!plan.useCustomSchedule && (
                          <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.35)', fontStyle: 'italic', padding: '4px 0' }}>
                            Using global schedule settings
                          </div>
                        )}
                      </div>
                    );
                  })}
                </div>
              )}
            </div>
          )}

          {/* Volume posture, stated at the last gate. The operator asked for a
              confirmation banner so "I am aligned and the system is aligned"
              (2026-08-18) — the review step is where that has to be
              unmissable, not a checkbox two steps back. */}
          {(() => {
            const capped = selectedISPs.filter(i => !audienceBound && (ispQuotas[i] || 0) > 0);
            const uncapped = selectedISPs.filter(i => audienceBound || !((ispQuotas[i] || 0) > 0));
            if (uncapped.length === 0) return null;
            return (
              <div style={{
                marginBottom: 16, padding: '12px 14px', borderRadius: 10, fontSize: 13, lineHeight: 1.6,
                background: 'rgba(245,158,11,0.10)', border: '1px solid rgba(245,158,11,0.55)', color: '#fbbf24',
              }}>
                <div style={{ fontWeight: 700, marginBottom: 4 }}>
                  <FontAwesomeIcon icon={faInfinity} /> UNCAPPED SEND — confirm before deploying
                </div>
                {audienceBound
                  ? 'This campaign is audience-bound: every per-provider quota is 0, so the selected audience IS the cap.'
                  : `Quota 0 = unlimited. ${uncapped.map(i => ISP_META[i]?.label || i).join(', ')} ${uncapped.length === 1 ? 'has' : 'have'} no ceiling.`}
                {' '}
                {audienceEstimate
                  ? <>It will send to <strong>{audienceEstimate.after_suppressions.toLocaleString()}</strong>{' '}
                      recipients after suppression.</>
                  : 'The audience was not estimated — the recipient count is UNKNOWN.'}
                {capped.length > 0 && (
                  <> Capped lanes: {capped.map(i => `${ISP_META[i]?.label || i} ${(ispQuotas[i] || 0).toLocaleString()}`).join(', ')}.</>
                )}
              </div>
            );
          })()}

          {/* Summary cards */}
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12, marginBottom: 16 }}>
            <SummaryCard title="Target Providers" value={selectedISPs.map(i => ISP_META[i]?.label || i).join(', ')} />
            <SummaryCard title="Sending Domain" value={selectedDomain} />
            <SummaryCard title="Variants" value={`${variants.length} variant${variants.length > 1 ? 's' : ''} (${variants.map(v => `${v.variant_name}: ${v.split_percent}%`).join(', ')})`} />
            <SummaryCard title="Audience" value={audienceEstimate ? `${audienceEstimate.after_suppressions.toLocaleString()} recipients` : 'Not estimated'} />
            <SummaryCard title="Schedule Mode" value={sendMode === 'immediate' ? 'Immediate' : scheduleMode === 'quick' ? `Quick: ${scheduledAt || 'Not set'}` : 'Per-provider custom plans'} />
            <SummaryCard title="Provider Plan Summary" value={
              sendMode === 'scheduled' && scheduleMode === 'per-isp'
                ? selectedISPs.map(isp => {
                    const plan = ispPlansByKey[isp];
                    const spanCount = plan?.timeSpans?.length || 0;
                    return `${ISP_META[isp]?.label || isp}: ${spanCount} span${spanCount === 1 ? '' : 's'} / ${plan?.cadenceMode || 'single'}`;
                  }).join(' | ')
                : 'Global schedule applies to all selected providers'
            } />
            <SummaryCard title="From Names" value={variants.map(v => v.from_name).filter(Boolean).join(' / ') || '—'} />
            <SummaryCard title="Subject Lines" value={variants.map(v => v.subject).filter(Boolean).join(' / ') || '—'} />
            <SummaryCard title="Preview Text" value={variants[0]?.preview_text || '(none)'} />
            <SummaryCard title="Provider Quotas" value={
              Object.entries(ispQuotas).filter(([, v]) => v > 0).length > 0
                ? Object.entries(ispQuotas).filter(([, v]) => v > 0).map(([isp, vol]) => `${ISP_META[isp]?.label || isp}: ${vol.toLocaleString()}`).join(' / ')
                : 'Unlimited (no quotas)'
            } />
          </div>

          {/* Randomization toggle — only when quotas are active */}
          {Object.values(ispQuotas).some(v => v > 0) && (
            <div style={{ marginBottom: 16 }}>
              <label style={{ fontSize: 12, color: 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 6 }}>Audience Selection</label>
              <div style={{ display: 'flex', gap: 8 }}>
                {([false, true] as const).map(isRandom => (
                  <button
                    key={String(isRandom)}
                    onClick={() => setRandomizeAudience(isRandom)}
                    style={{
                      flex: 1, padding: '10px 0', borderRadius: 8, fontSize: 13, fontWeight: 600,
                      cursor: 'pointer', transition: 'all 0.2s',
                      background: randomizeAudience === isRandom ? (isRandom ? '#8b5cf620' : 'rgba(0,200,255,0.12)') : '#0d1526',
                      color: randomizeAudience === isRandom ? (isRandom ? '#8b5cf6' : '#00b0ff') : 'rgba(180,210,240,0.65)',
                      border: `2px solid ${randomizeAudience === isRandom ? (isRandom ? '#8b5cf6' : '#00b0ff') : 'rgba(0,200,255,0.08)'}`,
                    }}
                  >
                    {isRandom ? 'Randomize' : 'Sequential'}
                  </button>
                ))}
              </div>
              <div style={{ fontSize: 11, color: '#64748b', marginTop: 4 }}>
                {randomizeAudience
                  ? 'Audience will be shuffled randomly before applying provider quotas.'
                  : 'Subscribers selected in list order until each provider quota is reached.'}
              </div>
            </div>
          )}

          {/* A failed warm-up request queues nothing — say so, and let the
              operator retry from here rather than dead-ending. */}
          {warmupActive && warmupResult?.error && (
            <div style={{ marginBottom: 12, padding: '10px 12px', background: 'rgba(239,68,68,0.08)', border: '1px solid rgba(239,68,68,0.35)', borderRadius: 8, fontSize: 12, color: '#fca5a5' }}>
              <FontAwesomeIcon icon={faTimesCircle} /> {warmupResult.error}
            </div>
          )}

          <div style={{ display: 'flex', gap: 12 }}>
            {/* Save Draft persists an OFFER-flow campaign_input. A warm-up
                request is not a campaign draft, so the control is not offered
                on this branch rather than saving a misleading half-payload. */}
            {activeMode === 'offers' && (
            <button
              onClick={() => {
                const errors = getStepErrors(6);
                if (errors.length > 0) {
                  setStepAttempted(prev => ({ ...prev, 6: true }));
                  return;
                }
                handleSaveDraft();
              }}
              disabled={savingDraft || deploying || loadingDraft}
              style={{
                display: 'flex', alignItems: 'center', justifyContent: 'center', gap: 8,
                flex: 1, padding: '14px 0',
                background: savingDraft ? '#4b5563' : '#0d1526',
                color: '#7dd3fc', border: '1px solid rgba(0,176,255,0.18)', borderRadius: 10, fontSize: 15, fontWeight: 600,
                cursor: savingDraft || deploying || loadingDraft ? 'not-allowed' : 'pointer',
              }}
            >
              {savingDraft
                ? <><FontAwesomeIcon icon={faSpinner} spin /> Saving...</>
                : <><FontAwesomeIcon icon={faSave} /> Save Draft</>
              }
            </button>
            )}
            <button
              className="ig-btn-glow ig-ripple"
              onClick={() => {
                const errors = getStepErrors(6);
                if (errors.length > 0) {
                  setStepAttempted(prev => ({ ...prev, 6: true }));
                  return;
                }
                // EXHAUSTIVE. A mode with no case cannot compile, so a third
                // (or fourth) mode can never fall through to an offer deploy.
                submitForActiveMode();
              }}
              disabled={deploying || savingDraft || warmupSubmitting || newsletterSubmitting}
              style={{
                display: 'flex', alignItems: 'center', justifyContent: 'center', gap: 8,
                flex: 1.4, padding: '14px 0',
                background: (deploying || warmupSubmitting || newsletterSubmitting) ? '#4b5563'
                  : activeMode === 'offers' ? (sendMode === 'scheduled' ? '#f59e0b' : '#00b0ff')
                  : '#38bdf8',
                color: '#fff', border: 'none', borderRadius: 10, fontSize: 15, fontWeight: 600,
                cursor: deploying || savingDraft || warmupSubmitting || newsletterSubmitting ? 'not-allowed' : 'pointer',
              }}
            >
              {/* The two intent-recording modes deliberately never say
                  "Deploy" or "Send" — nothing is sent by this button. */}
              {activeMode === 'warmup'
                ? (warmupSubmitting
                    ? <><FontAwesomeIcon icon={faSpinner} spin /> Queueing…</>
                    : <><FontAwesomeIcon icon={faClock} /> Queue for build</>)
                : activeMode === 'newsletter'
                  ? (newsletterSubmitting
                      ? <><FontAwesomeIcon icon={faSpinner} spin /> Queueing {newsletterIncluded.length} domains…</>
                      : <><FontAwesomeIcon icon={faClock} /> Queue {newsletterIncluded.length} newsletter{newsletterIncluded.length === 1 ? '' : 's'} for build</>)
                  : deploying
                    ? <><FontAwesomeIcon icon={faSpinner} spin /> Deploying...</>
                    : sendMode === 'scheduled'
                      ? <><FontAwesomeIcon icon={faRocket} /> Schedule Campaign</>
                      : <><FontAwesomeIcon icon={faRocket} /> Deploy Now</>
              }
            </button>
          </div>
          {(warmupActive || newsletterActive) && (
            <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)', marginTop: 8, textAlign: 'center', lineHeight: 1.5 }}>
              Queueing records the request{newsletterActive ? 's' : ''} only — it does not send mail, and it
              does not create a campaign. The builder runs separately and takes about 40 minutes.
            </div>
          )}
        </>
      )}
    </div>
  );

  // Mode-dispatched step content. Exhaustive over CampaignMode.
  const renderStepForMode = (s: 3 | 4): React.ReactNode => {
    switch (activeMode) {
      case 'offers':     return s === 3 ? renderStep3() : renderStep4();
      case 'warmup':     return s === 3 ? renderWarmupStep3() : renderWarmupStep4();
      case 'newsletter': return s === 3 ? renderNewsletterStep3() : renderNewsletterStep4();
      default:           return assertUnreachableMode(activeMode);
    }
  };

  const SummaryCard: React.FC<{ title: string; value: string }> = ({ title, value }) => (
    <div style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 8, padding: 12 }}>
      <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', marginBottom: 4 }}>{title}</div>
      <div style={{ fontSize: 13, color: '#e0e6f0', wordBreak: 'break-word' }}>{value || '—'}</div>
    </div>
  );

  // ── Render ─────────────────────────────────────────────────────────────────

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', background: '#0a0f1a', color: '#e0e6f0' }}>
      {/* Header */}
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '14px 20px', borderBottom: '1px solid rgba(0,200,255,0.08)', background: '#0a1628' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          {onClose && (
            <button onClick={onClose} style={{ background: 'none', border: 'none', color: 'rgba(180,210,240,0.65)', cursor: 'pointer', fontSize: 14 }}>
              <FontAwesomeIcon icon={faArrowLeft} />
            </button>
          )}
          <h2 style={{ margin: 0, fontSize: 16, fontWeight: 700, letterSpacing: 1 }}>Campaign Manager</h2>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          {/* Clone button */}
          <div style={{ position: 'relative' }} ref={clonePanelRef}>
            <button
              onClick={() => {
                if (loadingDraft) return;
                // Refetch when opening if we have nothing, if the last attempt
                // failed, or if the operator changed domain since we loaded —
                // otherwise the list silently belongs to the previous apex.
                const staleApex = !!selectedDomain && !!cloneApex
                  && !selectedDomain.endsWith(cloneApex);
                if (!showClonePanel && (cloneCandidates.length === 0 || cloneError || staleApex)) {
                  fetchCloneCandidates();
                }
                setShowClonePanel(!showClonePanel);
              }}
              disabled={loadingDraft}
              style={{
                display: 'flex', alignItems: 'center', gap: 6,
                padding: '5px 12px', borderRadius: 6,
                border: '1px solid rgba(0,200,255,0.15)',
                background: showClonePanel ? 'rgba(0,200,255,0.12)' : 'rgba(0,200,255,0.04)',
                color: loadingDraft ? '#4b5563' : showClonePanel ? '#00b0ff' : 'rgba(180,210,240,0.75)',
                fontSize: 12, cursor: loadingDraft ? 'not-allowed' : 'pointer', whiteSpace: 'nowrap',
                transition: 'all 0.2s',
                opacity: loadingDraft ? 0.5 : 1,
              }}
            >
              <FontAwesomeIcon icon={loadingDraft ? faSpinner : faCopy} spin={loadingDraft} />
              Clone
              <FontAwesomeIcon icon={showClonePanel ? faChevronUp : faChevronDown} style={{ fontSize: 10 }} />
            </button>

            {/* Clone dropdown panel */}
            {showClonePanel && (
              <div style={{
                position: 'absolute', top: '100%', right: 0, marginTop: 6,
                width: 420, maxHeight: 400, overflowY: 'auto',
                background: '#0d1526', border: '1px solid rgba(0,200,255,0.12)',
                borderRadius: 10, boxShadow: '0 12px 40px rgba(0,0,0,0.5)',
                zIndex: 100, padding: 0,
              }}>
                <div style={{ padding: '10px 14px', borderBottom: '1px solid rgba(0,200,255,0.08)', display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
                  <span style={{ fontSize: 12, fontWeight: 600, color: '#e0e6f0' }}>Clone from Previous Campaign</span>
                  <button onClick={() => setShowClonePanel(false)} style={{ background: 'none', border: 'none', color: '#64748b', cursor: 'pointer', fontSize: 12 }}>
                    <FontAwesomeIcon icon={faTimes} />
                  </button>
                </div>

                {cloneLoading && (
                  <div style={{ padding: 20, textAlign: 'center', color: '#64748b', fontSize: 12 }}>
                    <FontAwesomeIcon icon={faSpinner} spin /> Loading campaigns...
                  </div>
                )}

                {!cloneLoading && cloneError && (
                  <div style={{ padding: 16, textAlign: 'center', fontSize: 12, color: '#f87171' }}>
                    {cloneError}
                    <button
                      onClick={fetchCloneCandidates}
                      style={{
                        display: 'block', margin: '8px auto 0', padding: '4px 12px',
                        borderRadius: 5, border: '1px solid rgba(248,113,113,0.35)',
                        background: 'rgba(248,113,113,0.08)', color: '#fca5a5',
                        fontSize: 11, cursor: 'pointer',
                      }}
                    >
                      Retry
                    </button>
                  </div>
                )}

                {!cloneLoading && !cloneError && cloneCandidates.length === 0 && (
                  <div style={{ padding: 20, textAlign: 'center', color: '#64748b', fontSize: 12 }}>
                    No campaigns available to clone
                    {cloneApex ? ` for ${cloneApex}.` : '.'}
                  </div>
                )}

                {!cloneLoading && !cloneError && cloneApex && cloneCandidates.length > 0 && (
                  <div style={{ padding: '6px 12px', fontSize: 10.5, color: '#64748b',
                                borderBottom: '1px solid rgba(255,255,255,0.06)' }}>
                    Showing campaigns across all sending domains for <b>{cloneApex}</b>
                  </div>
                )}

                {!cloneLoading && cloneCandidates.map((c) => (
                  <button
                    key={c.id}
                    onClick={() => applyClone(c.id)}
                    disabled={cloneApplying === c.id}
                    style={{
                      display: 'flex', flexDirection: 'column', gap: 4,
                      width: '100%', padding: '10px 14px', textAlign: 'left',
                      background: c.recommended ? 'rgba(16,185,129,0.06)' : 'transparent',
                      border: 'none', borderBottom: '1px solid rgba(0,200,255,0.05)',
                      color: '#e0e6f0', cursor: cloneApplying ? 'not-allowed' : 'pointer',
                      transition: 'background 0.15s',
                    }}
                    onMouseEnter={e => { if (!c.recommended) (e.target as HTMLElement).closest('button')!.style.background = 'rgba(0,200,255,0.04)'; }}
                    onMouseLeave={e => { if (!c.recommended) (e.target as HTMLElement).closest('button')!.style.background = 'transparent'; }}
                  >
                    <div style={{ display: 'flex', alignItems: 'center', gap: 6, width: '100%' }}>
                      {c.recommended && (
                        <span style={{
                          display: 'inline-flex', alignItems: 'center', gap: 3,
                          padding: '1px 6px', borderRadius: 4, fontSize: 10, fontWeight: 700,
                          background: 'rgba(16,185,129,0.15)', color: '#10b981',
                          border: '1px solid rgba(16,185,129,0.3)',
                        }}>
                          <FontAwesomeIcon icon={faTrophy} /> TOP
                        </span>
                      )}
                      <span style={{ fontSize: 12, fontWeight: 500, flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                        {c.name}
                      </span>
                      {cloneApplying === c.id && <FontAwesomeIcon icon={faSpinner} spin style={{ fontSize: 11, color: '#00b0ff' }} />}
                    </div>
                    <div style={{ display: 'flex', gap: 10, fontSize: 10, color: 'rgba(180,210,240,0.55)' }}>
                      <span>{c.sent_count.toLocaleString()} sent</span>
                      <span style={{ color: c.open_rate > 5 ? '#10b981' : '#f59e0b' }}>{c.open_rate}% opens</span>
                      <span style={{ color: c.click_rate > 1 ? '#10b981' : '#64748b' }}>{c.click_rate}% clicks</span>
                      {(c.bounce_rate > 5 || (c.hard_bounce_rate ?? 0) > 0 || (c.soft_bounce_rate ?? 0) > 0) && (
                        <>
                          <span style={{ color: '#ef4444' }}>{(c.hard_bounce_rate ?? c.bounce_rate ?? 0)}% hard</span>
                          <span style={{ color: '#f59e0b', marginLeft: 4 }}>{(c.soft_bounce_rate ?? 0)}% soft</span>
                        </>
                      )}
                      <span>{new Date(c.campaign_date).toLocaleDateString()}</span>
                    </div>
                  </button>
                ))}
              </div>
            )}
          </div>
          {/* POSITION, not id. Ids are sparse since step 5 was retired, so
              `step` would read "Step 6 of 5". */}
          <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.65)' }}>
            Step {Math.max(1, stepIndex + 1)} of {activeSteps.length}
          </div>
        </div>
      </div>

      {/* Step indicator */}
      <div className="ig-stagger" style={{ display: 'flex', padding: '12px 20px', gap: 4, borderBottom: '1px solid rgba(0,200,255,0.08)', background: '#0a1628', overflowX: 'auto' }}>
        {activeSteps.map((s) => {
          const isActive = s.id === step;
          const isComplete = s.id < step;
          const hasErrors = stepAttempted[s.id] && getStepErrors(s.id).length > 0;
          return (
            <button
              key={s.id}
              className={isActive ? 'ig-pulse-cyan' : undefined}
              onClick={() => { if (s.id < step) setStep(s.id); }}
              style={{
                display: 'flex', alignItems: 'center', gap: 6, position: 'relative',
                padding: '6px 12px', borderRadius: 6, border: 'none',
                background: isActive ? 'rgba(0,200,255,0.12)' : hasErrors ? 'rgba(239,68,68,0.08)' : 'transparent',
                color: hasErrors ? '#ef4444' : isActive ? '#00b0ff' : isComplete ? '#10b981' : '#64748b',
                fontSize: 12, cursor: s.id < step ? 'pointer' : 'default',
                whiteSpace: 'nowrap', fontWeight: isActive ? 600 : 400,
                transition: 'all 0.2s',
              }}
            >
              <FontAwesomeIcon icon={hasErrors ? faExclamationTriangle : isComplete ? faCheck : s.icon} />
              {s.label}
              {hasErrors && (
                <span style={{
                  position: 'absolute', top: 2, right: 4, width: 6, height: 6,
                  borderRadius: '50%', background: '#ef4444',
                }} />
              )}
            </button>
          );
        })}
      </div>

      {/* Step content */}
      <div style={{ flex: 1, overflowY: 'auto', padding: 20 }}>
        {step === 1 && renderStepDomain()}
        {step === 2 && renderStepProviders()}
        {/* THE BRANCH. Steps 1, 2 and 6 are shared shells; 3 and 4 fork by
            MODE through an exhaustive switch whose default is a `never` check,
            so an unhandled mode is a compile error rather than a blank page
            (`tsc` cannot see a missing `{step === N && …}` line, but it can see
            a missing switch case). */}
        {step === 3 && renderStepForMode(3)}
        {step === 4 && renderStepForMode(4)}
        {step === 6 && renderStep6()}
      </div>

      {/* Footer nav */}
      {!deployResult && (
        <div style={{ display: 'flex', justifyContent: 'space-between', padding: '12px 20px', borderTop: '1px solid rgba(0,200,255,0.08)', background: '#0a1628' }}>
          <button
            onClick={() => { if (prevStepId !== null) setStep(prevStepId); }}
            disabled={prevStepId === null}
            style={{
              display: 'flex', alignItems: 'center', gap: 6,
              padding: '8px 18px', borderRadius: 8, border: '1px solid rgba(0,200,255,0.08)',
              background: 'transparent', color: prevStepId === null ? '#4b5563' : '#e0e6f0',
              fontSize: 13, cursor: prevStepId === null ? 'default' : 'pointer',
            }}
          >
            <FontAwesomeIcon icon={faArrowLeft} /> Back
          </button>
          {nextStepId !== null && (
            <button
              className="ig-btn-glow ig-ripple"
              onClick={() => {
                if (canProceed()) {
                  setStepAttempted(prev => ({ ...prev, [step]: false }));
                  setStep(nextStepId);
                } else {
                  setStepAttempted(prev => ({ ...prev, [step]: true }));
                }
              }}
              style={{
                display: 'flex', alignItems: 'center', gap: 6,
                padding: '8px 18px', borderRadius: 8, border: 'none',
                background: '#00b0ff',
                color: '#fff', fontSize: 13,
                cursor: 'pointer',
              }}
            >
              Next <FontAwesomeIcon icon={faArrowRight} />
            </button>
          )}
        </div>
      )}

      <JarvisCompleteModal
        visible={showCompleteModal}
        onClose={() => setShowCompleteModal(false)}
        campaignName={campaignName || 'Campaign'}
        stats={{ recipients: audienceEstimate?.after_suppressions || audienceEstimate?.total_recipients || 0, variants: variants.length }}
      />
    </div>
  );
};
