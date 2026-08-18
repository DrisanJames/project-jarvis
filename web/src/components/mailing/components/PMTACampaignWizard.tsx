import React, { useState, useEffect, useCallback, useRef } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faArrowLeft, faArrowRight, faArrowUp, faArrowDown, faCheck, faServer, faGlobe,
  faPenFancy, faUsers, faBrain, faRocket, faSpinner,
  faExclamationTriangle, faCheckCircle, faTimesCircle,
  faTimes, faChartBar, faShieldAlt, faCrosshairs,
  faSave, faGripVertical, faMagic,
  faCopy, faTrophy, faChevronDown, faChevronUp, faSearch, faLock,
} from '@fortawesome/free-solid-svg-icons';
import { useAuth } from '../../../contexts/AuthContext';
import { AnimatedCounter } from '../shared/AnimatedCounter';
import { useToast } from '../shared/ToastSystem';
import { JarvisCompleteModal } from '../shared/JarvisCompleteModal';
import {
  SEGMENT_CATEGORIES,
  getCategoryMeta,
  defaultVisibleCategoriesForPicker,
  type SegmentCategory,
} from './segCategoryMetadata';
import { EngagementTierPicker, type EngagementTiers } from './EngagementTierPicker';
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

interface ISPIntel {
  isp: string;
  display_name: string;
  throughput: {
    max_msg_rate: number;
    active_ips: number;
    max_daily_capacity: number;
    max_hourly_rate: number;
    audience_size: number;
    can_send_in_one_pass: boolean;
    estimated_hours: number;
    status: string;
  };
  warmup_summary: {
    total_ips: number;
    warmed_ips: number;
    warming_ips: number;
    paused_ips: number;
    avg_warmup_day: number;
    daily_limit: number;
    status: string;
  };
  conviction_summary: {
    dominant_verdict: string;
    confidence: number;
    will_count: number;
    wont_count: number;
    key_observations: string[];
    risk_factors: string[];
  };
  active_warnings: string[];
  strategy: string;
}

interface TurbulenceAlert {
  isp: string;
  ip?: string;
  action: string;
  agent_type: string;
  reasoning: string;
  occurred_at: string;
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

// ── Step navigation ──────────────────────────────────────────────────────────

const STEPS = [
  { id: 1, label: 'Mailbox Providers',      icon: faServer },
  { id: 2, label: 'Sending Domain',         icon: faGlobe },
  { id: 3, label: 'Offer + Creative',       icon: faPenFancy },
  { id: 4, label: 'Engagement Audience',    icon: faUsers },
  { id: 5, label: 'Sending Insights',       icon: faBrain },
  { id: 6, label: 'Schedule + Deploy',      icon: faRocket },
];

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
  const [intelLoading, setIntelLoading] = useState(false);

  // Step 1 state
  const [ispReadiness, setISPReadiness] = useState<ISPReadiness[]>([]);
  const [selectedISPs, setSelectedISPs] = useState<string[]>([...ALL_ISPS]);
  const [ispQuotas, setISPQuotas] = useState<Record<string, number>>({ ...DEFAULT_ISP_QUOTAS });
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

  // Step 2 state
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
  // Inclusion ids restored from a draft, held until the property's engagement
  // grid loads and can say which are clickers and which are openers.
  const [restoredInclusionSegments, setRestoredInclusionSegments] = useState<string[]>([]);
  const [selectedClickerIds, setSelectedClickerIds] = useState<string[]>([]);
  const [selectedOpenerIds, setSelectedOpenerIds] = useState<string[]>([]);
  const [excludeClickers, setExcludeClickers] = useState(true);
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

  // Step 5 state
  const [ispIntel, setISPIntel] = useState<ISPIntel[]>([]);
  const [turbulenceAlerts, setTurbulenceAlerts] = useState<TurbulenceAlert[]>([]);
  const [turbulenceDismissed, setTurbulenceDismissed] = useState(false);

  // Step 6 state
  const [campaignName, setCampaignName] = useState('');
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

  // Clone state
  const [showClonePanel, setShowClonePanel] = useState(false);
  const [cloneCandidates, setCloneCandidates] = useState<CloneCandidate[]>([]);
  const [cloneLoading, setCloneLoading] = useState(false);
  const [cloneApplying, setCloneApplying] = useState('');
  const clonePanelRef = useRef<HTMLDivElement>(null);

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

  // Selecting a DIFFERENT property invalidates the property-scoped choices.
  // Guarded by a ref so it does not fire on the initial mount or on the
  // domain a restored draft just installed — otherwise it would immediately
  // wipe the pinned profile that applyCampaignInput restored in the same tick.
  const prevDomainRef = useRef<string | null>(null);
  useEffect(() => {
    const prev = prevDomainRef.current;
    prevDomainRef.current = selectedDomain;
    if (prev === null || prev === selectedDomain) return;
    setSelectedClickerIds([]);
    setSelectedOpenerIds([]);
    setSelectedProfileId('');
  }, [selectedDomain]);

  // Re-hydrate the engagement chips from a restored draft once the grid is in.
  useEffect(() => {
    if (!engagementTiers || restoredInclusionSegments.length === 0) return;
    const clickerIds = new Set(engagementTiers.clickers.map(t => t.segment_id));
    const openerIds = new Set(engagementTiers.openers.map(t => t.segment_id));
    const restoredClickers = restoredInclusionSegments.filter(id => clickerIds.has(id));
    const restoredOpeners = restoredInclusionSegments.filter(id => openerIds.has(id));
    if (restoredClickers.length > 0) setSelectedClickerIds(restoredClickers);
    if (restoredOpeners.length > 0) setSelectedOpenerIds(restoredOpeners);
    setRestoredInclusionSegments([]);
  }, [engagementTiers, restoredInclusionSegments]);

  const toggleEngagementTier = useCallback((kind: 'clickers' | 'openers', segmentId: string) => {
    const apply = (prev: string[]) =>
      prev.includes(segmentId) ? prev.filter(x => x !== segmentId) : [...prev, segmentId];
    if (kind === 'clickers') setSelectedClickerIds(apply);
    else setSelectedOpenerIds(apply);
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

  const fetchIntel = useCallback(async () => {
    setIntelLoading(true);
    try {
      const audiencePerISP: Record<string, number> = {};
      if (audienceEstimate?.isp_breakdown) {
        for (const [k, v] of Object.entries(audienceEstimate.isp_breakdown)) {
          audiencePerISP[k] = v;
        }
      }
      const res = await fetchWithRetry(`${API_BASE}/pmta-campaign/intel`, {
        method: 'POST',
        body: JSON.stringify({
          target_isps: selectedISPs,
          audience_per_isp: audiencePerISP,
          send_day: new Date().toLocaleDateString('en-US', { weekday: 'long' }),
          send_hour: new Date().getUTCHours(),
        }),
      });
      const data = await res.json();
      setISPIntel(data.isps || []);
      setTurbulenceAlerts(data.turbulence_alerts || []);
      setTurbulenceDismissed(false);
    } catch (err) {
      console.warn('[Wizard] intel fetch failed:', err);
    }
    setIntelLoading(false);
  }, [fetchWithRetry, orgId, selectedISPs, audienceEstimate]);

  // Load data on step entry
  useEffect(() => {
    if (step === 1) { fetchReadiness(); fetchInsights(insightDomainFilter || undefined); }
    if (step === 2) fetchDomains();
    if (step === 3) fetchOffers();
    if (step === 4) fetchAudienceData();
    if (step === 5) fetchIntel();
  }, [step, fetchReadiness, fetchInsights, fetchDomains, fetchOffers, fetchAudienceData, fetchIntel, insightDomainFilter]);

  // Re-estimate audience when selections change
  useEffect(() => {
    if (step === 4) fetchAudienceEstimate();
  }, [step, selectedLists, selectedSegments, selectedSuppLists, selectedExclusionSegments, fetchAudienceEstimate]);

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

  const getStepErrors = (s: number): string[] => {
    const errors: string[] = [];
    switch (s) {
      case 1:
        if (selectedISPs.length === 0) errors.push('Select at least one mailbox provider');
        break;
      case 2:
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
            && selectedLists.length === 0 && selectedSegments.length === 0) {
          errors.push('Select an engagement range, or a list/segment in the advanced picker');
        }
        // Belt for the client side of the coercion the server also enforces.
        if (audienceBound && masterTopUp) {
          errors.push('Master-list top-up cannot be combined with an uncapped (audience-bound) send');
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
  const applyAnchorPreset = (preset: AnchorPreset) => {
    const [hh, mm] = preset.localTime.split(':').map(Number);
    let when = sendDayInstant(0, hh, mm);
    if (when.getTime() <= Date.now() + 15 * 60 * 1000) {
      when = sendDayInstant(1, hh, mm);
    }
    const local = toDateTimeLocal(when);
    setActivePreset(preset.id);
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

    setCampaignId(draft.campaign_id || input.campaign_id || '');
    setCampaignName(input.name || draft.name || '');
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
    setSelectedExclusionSegments(input.exclusion_segments || []);
    setSendMode(input.send_mode === 'scheduled' ? 'scheduled' : 'immediate');
    setScheduleMode(draft.schedule_mode === 'per-isp' ? 'per-isp' : 'quick');
    setScheduledAt(toLocalInputValue(input.scheduled_at));
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
    try {
      const res = await fetchWithRetry(`${API_BASE}/pmta-campaign/clone-candidates`);
      if (res.ok) {
        const data = await res.json();
        setCloneCandidates(data.campaigns || []);
      }
    } catch (err) {
      console.warn('[Wizard] clone candidates fetch failed:', err);
    }
    setCloneLoading(false);
  }, [fetchWithRetry]);

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
      setISPIntel([]);
      setTurbulenceAlerts([]);
      setTurbulenceDismissed(false);
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

    // The long-tail 'other' lane rides a synthetic plan. Audience-bound sends
    // include it whenever the operator selected it (quota 0 = unlimited);
    // capped sends include it only when it carries a finite volume.
    const otherQuota = audienceBound ? 0 : (ispQuotas['other'] || 0);
    const includeOther = audienceBound ? selectedISPs.includes('other') : otherQuota > 0;
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

    const engagementIds = [...selectedClickerIds, ...selectedOpenerIds];
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

  // ── Toggle helpers ───────────────────────────────────────────────────────

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

  const renderStep1 = () => (
    <div className="wiz-step-content ig-fade-in">
      <h3 style={{ margin: '0 0 4px' }}>Select Mailbox Providers<RequiredDot /></h3>
      <p style={{ margin: '0 0 16px', color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>
        Choose which mailbox providers to target. Cards show live health from the delivery engine.
      </p>
      <StepErrorBanner stepNum={1} />

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
          <p style={{ margin: '0 0 12px', fontSize: 11, color: '#64748b' }}>
            Set maximum sends per mailbox provider. Leave at 0 for unlimited.
          </p>
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

  const renderStep2 = () => (
    <div className="wiz-step-content ig-fade-in">
      <h3 style={{ margin: '0 0 4px' }}>Select Sending Domain<RequiredDot /></h3>
      <p style={{ margin: '0 0 16px', color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>
        Choose the domain that will appear in the "From" address. Each domain shows DNS and IP pool info.
      </p>
      <StepErrorBanner stepNum={2} />
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
      <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
        {sendingDomains.map(d => {
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
          orgFetch={(url, opts) => orgFetch(url, orgId, opts)}
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
          profileFromName={sendingDomains.find(d => d.domain === selectedDomain)?.from_name}
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

        <EngagementTierPicker
          tiers={engagementTiers}
          loading={engagementLoading}
          error={engagementError}
          selectedClickerIds={selectedClickerIds}
          selectedOpenerIds={selectedOpenerIds}
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
              per-provider quotas on step 1 are ignored while it is on.
            </span>
          </label>

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
                  <span style={{ fontSize: 10, color: 'rgba(180,210,240,0.4)', marginLeft: 'auto' }}>{totalSuppSelected} active</span>
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

            {/* Unified Send Priority */}
            {sendPriority.length > 1 && (
              <div style={{
                background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 16, marginBottom: 16,
              }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 10 }}>
                  <div style={{ width: 4, height: 16, borderRadius: 2, background: '#00e5ff' }} />
                  <h4 style={{ margin: 0, fontSize: 13, color: '#00e5ff', fontWeight: 600 }}>Send Priority</h4>
                  <span style={{ fontSize: 10, color: 'rgba(180,210,240,0.4)', marginLeft: 'auto' }}>Drag or use arrows to reorder — #1 sends first</span>
                </div>
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
                          {idx + 1}
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

  const renderStep5 = () => {
    const MiniGauge: React.FC<{ value: number; max: number; color: string; label: string; size?: number }> = ({ value, max, color, label, size = 44 }) => {
      const pct = max > 0 ? Math.min(value / max, 1) : 0;
      const r = (size - 6) / 2;
      const c = 2 * Math.PI * r;
      const offset = c * (1 - pct);
      return (
        <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 2 }}>
          <svg width={size} height={size} style={{ transform: 'rotate(-90deg)' }}>
            <circle cx={size / 2} cy={size / 2} r={r} fill="none" stroke="rgba(0,200,255,0.06)" strokeWidth={4} />
            <circle cx={size / 2} cy={size / 2} r={r} fill="none" stroke={color} strokeWidth={4}
              strokeDasharray={c} strokeDashoffset={offset} strokeLinecap="round"
              style={{ transition: 'stroke-dashoffset 0.6s ease' }} />
          </svg>
          <div style={{ fontSize: 11, fontWeight: 600, color, marginTop: -size / 2 - 6, lineHeight: `${size}px`, textAlign: 'center' }}>{value}</div>
          <div style={{ fontSize: 9, color: 'rgba(180,210,240,0.4)', marginTop: 2, textAlign: 'center' }}>{label}</div>
        </div>
      );
    };

    return (
      <div className="wiz-step-content ig-fade-in">
        <h3 style={{ margin: '0 0 4px' }}>Sending Insights</h3>
        <p style={{ margin: '0 0 16px', color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>
          Live state of the targeted providers — throughput, warm-up, delivery insights, and active warnings.
        </p>

        {turbulenceAlerts.length > 0 && !turbulenceDismissed && (
          <div style={{
            background: 'rgba(245,158,11,0.08)',
            border: '1px solid rgba(245,158,11,0.3)',
            borderRadius: 10,
            padding: '14px 16px',
            marginBottom: 16,
          }}>
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 8 }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                <span style={{ fontSize: 18 }}>&#9888;</span>
                <span style={{ fontWeight: 700, fontSize: 14, color: '#f59e0b' }}>
                  Turbulence Detected
                </span>
                <span style={{ fontSize: 11, color: 'rgba(245,158,11,0.6)', fontWeight: 500, marginLeft: 4 }}>
                  Advisory only — does not block deployment
                </span>
              </div>
              <button
                onClick={() => setTurbulenceDismissed(true)}
                style={{
                  background: 'transparent', border: 'none', color: 'rgba(245,158,11,0.5)',
                  cursor: 'pointer', fontSize: 18, padding: '0 4px', lineHeight: 1,
                }}
                title="Dismiss"
              >&times;</button>
            </div>
            <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
              {turbulenceAlerts.slice(0, 10).map((alert, i) => (
                <div key={i} style={{
                  display: 'flex', alignItems: 'baseline', gap: 8,
                  fontSize: 12, color: 'rgba(245,158,11,0.85)',
                }}>
                  <span style={{
                    background: 'rgba(245,158,11,0.15)', borderRadius: 4,
                    padding: '1px 6px', fontSize: 10, fontWeight: 600,
                    textTransform: 'uppercase', whiteSpace: 'nowrap',
                  }}>
                    {alert.isp}
                  </span>
                  <span style={{ color: 'rgba(180,210,240,0.5)', fontSize: 11 }}>
                    {alert.action.replace(/_/g, ' ')}{alert.ip ? ` on ${alert.ip}` : ''}
                  </span>
                  <span style={{ color: 'rgba(180,210,240,0.4)', fontSize: 11, flex: 1 }}>
                    {alert.reasoning}
                  </span>
                  <span style={{ color: 'rgba(180,210,240,0.25)', fontSize: 10, whiteSpace: 'nowrap' }}>
                    {new Date(alert.occurred_at).toLocaleTimeString()}
                  </span>
                </div>
              ))}
              {turbulenceAlerts.length > 10 && (
                <div style={{ fontSize: 11, color: 'rgba(245,158,11,0.5)', paddingTop: 4 }}>
                  +{turbulenceAlerts.length - 10} more alerts in the last 24h
                </div>
              )}
            </div>
          </div>
        )}

        {/* Skeleton loading */}
        {intelLoading && ispIntel.length === 0 && (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
            {selectedISPs.slice(0, 3).map((_, i) => (
              <div key={i} style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.06)', borderRadius: 10, padding: 16 }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 16 }}>
                  <div style={{ width: 40, height: 40, borderRadius: '50%', background: 'rgba(0,200,255,0.06)', animation: 'igShimmer 1.5s ease infinite' }} />
                  <div>
                    <div style={{ height: 16, width: 100, background: 'rgba(0,200,255,0.06)', borderRadius: 4, marginBottom: 6, animation: 'igShimmer 1.5s ease infinite' }} />
                    <div style={{ height: 10, width: 60, background: 'rgba(0,200,255,0.04)', borderRadius: 3, animation: 'igShimmer 1.5s ease infinite' }} />
                  </div>
                </div>
                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: 12 }}>
                  {[1, 2, 3].map(j => (
                    <div key={j} style={{ height: 100, background: 'rgba(0,200,255,0.03)', borderRadius: 8, animation: 'igShimmer 1.5s ease infinite', animationDelay: `${j * 0.2}s` }} />
                  ))}
                </div>
              </div>
            ))}
            <div style={{ textAlign: 'center', padding: 10, color: 'rgba(0,200,255,0.4)', fontSize: 12 }}>
              <FontAwesomeIcon icon={faSpinner} spin /> Querying delivery engine...
            </div>
          </div>
        )}

        {(!intelLoading || ispIntel.length > 0) && (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
            {ispIntel.map((intel, idx) => {
              const meta = ISP_META[intel.isp] || { label: intel.display_name, color: '#64748b', emoji: '🌐' };
              const warmupPct = intel.warmup_summary.total_ips > 0
                ? (intel.warmup_summary.warmed_ips / intel.warmup_summary.total_ips) * 100 : 0;
              const confidencePct = (intel.conviction_summary.confidence * 100);
              const isPositive = intel.conviction_summary.dominant_verdict === 'will';
              const totalVotes = intel.conviction_summary.will_count + intel.conviction_summary.wont_count;
              const willPct = totalVotes > 0 ? (intel.conviction_summary.will_count / totalVotes) * 100 : 50;

              return (
                <div key={intel.isp} style={{
                  background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 12, padding: 0,
                  overflow: 'hidden', animation: `igFadeSlide 0.4s ease ${idx * 0.1}s both`,
                }}>
                  {/* ISP header bar */}
                  <div style={{
                    display: 'flex', alignItems: 'center', justifyContent: 'space-between',
                    padding: '12px 16px', borderBottom: '1px solid rgba(0,200,255,0.06)',
                    background: `linear-gradient(90deg, ${meta.color}08, transparent)`,
                  }}>
                    <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
                      <div style={{
                        width: 36, height: 36, borderRadius: '50%', display: 'flex', alignItems: 'center', justifyContent: 'center',
                        background: `${meta.color}15`, fontSize: 18,
                      }}>{meta.emoji}</div>
                      <div>
                        <div style={{ fontSize: 14, fontWeight: 600, color: meta.color }}>{meta.label}</div>
                        <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.4)' }}>
                          {intel.throughput.active_ips} active IPs · {(intel.throughput.max_daily_capacity / 1000).toFixed(0)}k/day capacity
                        </div>
                      </div>
                    </div>
                    <div style={{ display: 'flex', gap: 6 }}>
                      {statusBadge(intel.throughput.status)}
                      {statusBadge(intel.warmup_summary.status)}
                    </div>
                  </div>

                  <div style={{ padding: 16 }}>
                    <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: 12, marginBottom: 12 }}>
                      {/* Throughput panel */}
                      <div style={{ background: '#0a0f1a', borderRadius: 10, padding: 12, border: '1px solid rgba(0,200,255,0.04)' }}>
                        <div style={{ display: 'flex', alignItems: 'center', gap: 4, fontSize: 10, color: 'rgba(180,210,240,0.5)', marginBottom: 10, textTransform: 'uppercase', letterSpacing: 0.5 }}>
                          <FontAwesomeIcon icon={faServer} /> Throughput
                        </div>
                        <div style={{ display: 'flex', justifyContent: 'space-around', marginBottom: 10 }}>
                          <MiniGauge value={intel.throughput.active_ips} max={Math.max(intel.throughput.active_ips, 4)} color="#00b0ff" label="IPs" />
                          <MiniGauge value={Math.round(intel.throughput.max_hourly_rate / 1000)} max={Math.max(Math.round(intel.throughput.max_daily_capacity / 1000 / 24), 1)} color="#10b981" label="k/hr" />
                        </div>
                        <div style={{ fontSize: 11, color: '#e0e6f0', lineHeight: 1.8 }}>
                          <div style={{ display: 'flex', justifyContent: 'space-between' }}>
                            <span style={{ color: 'rgba(180,210,240,0.5)' }}>Audience</span>
                            <strong>{intel.throughput.audience_size.toLocaleString()}</strong>
                          </div>
                          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                            <span style={{ color: 'rgba(180,210,240,0.5)' }}>Status</span>
                            {intel.throughput.can_send_in_one_pass
                              ? <span style={{ fontSize: 10, color: '#10b981', fontWeight: 600 }}>1-PASS ✓</span>
                              : <span style={{ fontSize: 10, color: '#f59e0b', fontWeight: 600 }}>~{intel.throughput.estimated_hours}h</span>}
                          </div>
                        </div>
                      </div>

                      {/* Warmup panel */}
                      <div style={{ background: '#0a0f1a', borderRadius: 10, padding: 12, border: '1px solid rgba(0,200,255,0.04)' }}>
                        <div style={{ display: 'flex', alignItems: 'center', gap: 4, fontSize: 10, color: 'rgba(180,210,240,0.5)', marginBottom: 10, textTransform: 'uppercase', letterSpacing: 0.5 }}>
                          <FontAwesomeIcon icon={faChartBar} /> Warmup
                        </div>
                        {/* Warmup progress bar */}
                        <div style={{ marginBottom: 10 }}>
                          <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 10, marginBottom: 4 }}>
                            <span style={{ color: 'rgba(180,210,240,0.4)' }}>{warmupPct.toFixed(0)}% warmed</span>
                            <span style={{ color: 'rgba(180,210,240,0.4)' }}>{intel.warmup_summary.warmed_ips}/{intel.warmup_summary.total_ips}</span>
                          </div>
                          <div style={{ height: 6, background: 'rgba(0,200,255,0.06)', borderRadius: 3, overflow: 'hidden' }}>
                            <div style={{
                              height: '100%', borderRadius: 3,
                              background: warmupPct >= 80 ? '#10b981' : warmupPct >= 50 ? '#f59e0b' : '#ef4444',
                              width: `${warmupPct}%`, transition: 'width 0.6s ease',
                            }} />
                          </div>
                        </div>
                        {/* IP breakdown */}
                        <div style={{ display: 'flex', gap: 4, marginBottom: 8 }}>
                          {[
                            { label: 'Warmed', count: intel.warmup_summary.warmed_ips, color: '#10b981' },
                            { label: 'Warming', count: intel.warmup_summary.warming_ips, color: '#f59e0b' },
                            { label: 'Paused', count: intel.warmup_summary.paused_ips, color: '#64748b' },
                          ].filter(s => s.count > 0).map(s => (
                            <span key={s.label} style={{
                              display: 'inline-flex', alignItems: 'center', gap: 3, padding: '2px 6px',
                              borderRadius: 4, fontSize: 9, background: `${s.color}15`, color: s.color,
                            }}>
                              <span style={{ width: 4, height: 4, borderRadius: '50%', background: s.color }} />
                              {s.count} {s.label.toLowerCase()}
                            </span>
                          ))}
                        </div>
                        <div style={{ fontSize: 11, display: 'flex', justifyContent: 'space-between', color: 'rgba(180,210,240,0.5)' }}>
                          <span>Daily limit</span>
                          <strong style={{ color: '#e0e6f0' }}>{(intel.warmup_summary.daily_limit / 1000).toFixed(0)}k</strong>
                        </div>
                      </div>

                      {/* Conviction panel */}
                      <div style={{ background: '#0a0f1a', borderRadius: 10, padding: 12, border: '1px solid rgba(0,200,255,0.04)' }}>
                        <div style={{ display: 'flex', alignItems: 'center', gap: 4, fontSize: 10, color: 'rgba(180,210,240,0.5)', marginBottom: 10, textTransform: 'uppercase', letterSpacing: 0.5 }}>
                          <FontAwesomeIcon icon={faBrain} /> Conviction
                        </div>
                        {/* Verdict display */}
                        <div style={{ textAlign: 'center', marginBottom: 8 }}>
                          <div style={{
                            display: 'inline-block', padding: '4px 14px', borderRadius: 6,
                            background: isPositive ? '#10b98118' : '#ef444418',
                            border: `1px solid ${isPositive ? '#10b98130' : '#ef444430'}`,
                            fontSize: 13, fontWeight: 700,
                            color: isPositive ? '#10b981' : '#ef4444',
                          }}>
                            {intel.conviction_summary.dominant_verdict.toUpperCase()}
                          </div>
                          <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.4)', marginTop: 4 }}>
                            {confidencePct.toFixed(0)}% confidence
                          </div>
                        </div>
                        {/* Will vs Wont bar */}
                        <div style={{ marginBottom: 6 }}>
                          <div style={{ display: 'flex', height: 8, borderRadius: 4, overflow: 'hidden', background: '#ef444425' }}>
                            <div style={{
                              height: '100%', background: '#10b981', borderRadius: '4px 0 0 4px',
                              width: `${willPct}%`, transition: 'width 0.5s ease',
                            }} />
                          </div>
                          <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 9, marginTop: 3 }}>
                            <span style={{ color: '#10b981' }}>WILL {intel.conviction_summary.will_count}</span>
                            <span style={{ color: '#ef4444' }}>WONT {intel.conviction_summary.wont_count}</span>
                          </div>
                        </div>
                      </div>
                    </div>

                    {/* Risk factors */}
                    {intel.conviction_summary.risk_factors && intel.conviction_summary.risk_factors.length > 0 && (
                      <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', marginBottom: 8 }}>
                        {intel.conviction_summary.risk_factors.map((rf, i) => (
                          <span key={i} style={{
                            display: 'inline-flex', alignItems: 'center', gap: 4, padding: '4px 10px',
                            borderRadius: 6, fontSize: 10, background: '#f59e0b10', color: '#f59e0b',
                            border: '1px solid #f59e0b20',
                          }}>
                            <FontAwesomeIcon icon={faExclamationTriangle} style={{ fontSize: 9 }} /> {rf}
                          </span>
                        ))}
                      </div>
                    )}

                    {/* Active warnings */}
                    {intel.active_warnings && intel.active_warnings.length > 0 && (
                      <div style={{ padding: '8px 10px', background: '#ef444410', borderRadius: 8, marginBottom: 8, border: '1px solid #ef444415' }}>
                        {intel.active_warnings.map((w, i) => (
                          <div key={i} style={{ fontSize: 11, color: '#ef4444', padding: '2px 0', display: 'flex', alignItems: 'center', gap: 4 }}>
                            <FontAwesomeIcon icon={faExclamationTriangle} style={{ fontSize: 9 }} /> {w}
                          </div>
                        ))}
                      </div>
                    )}

                    {/* Strategy */}
                    <div style={{
                      display: 'flex', alignItems: 'center', gap: 8,
                      padding: '10px 14px', background: 'rgba(0,200,255,0.04)', borderRadius: 8,
                      borderLeft: `3px solid ${meta.color}`,
                    }}>
                      <FontAwesomeIcon icon={faShieldAlt} style={{ color: meta.color, fontSize: 12 }} />
                      <div>
                        <div style={{ fontSize: 9, color: 'rgba(180,210,240,0.4)', textTransform: 'uppercase', letterSpacing: 0.5, marginBottom: 1 }}>Recommended Strategy</div>
                        <div style={{ fontSize: 12, color: '#e0e6f0' }}>{intel.strategy}</div>
                      </div>
                    </div>
                  </div>
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
      <h3 style={{ margin: '0 0 16px' }}>Review + Deploy</h3>
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

      {deployResult ? (
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
              onChange={e => setCampaignName(e.target.value)}
              style={{ width: '100%', background: '#0a0f1a', border: fieldBorder(!campaignName.trim()), borderRadius: 8, color: '#e0e6f0', padding: '10px 12px', fontSize: 14, boxSizing: 'border-box', transition: 'border-color 0.2s' }}
            />
            {showErr(6) && !campaignName.trim() && <div style={{ fontSize: 10, color: '#ef4444', marginTop: 3 }}>Campaign name is required</div>}
          </div>

          {/* Send mode toggle */}
          <div style={{ display: 'flex', gap: 8, marginBottom: 16 }}>
            {(['immediate', 'scheduled'] as const).map(mode => (
              <button
                key={mode}
                onClick={() => { setSendMode(mode); if (mode === 'immediate') { setScheduledAt(''); } }}
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
                    onChange={e => setScheduledAt(e.target.value)}
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

          <div style={{ display: 'flex', gap: 12 }}>
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
            <button
              className="ig-btn-glow ig-ripple"
              onClick={() => {
                const errors = getStepErrors(6);
                if (errors.length > 0) {
                  setStepAttempted(prev => ({ ...prev, 6: true }));
                  return;
                }
                handleDeploy();
              }}
              disabled={deploying || savingDraft}
              style={{
                display: 'flex', alignItems: 'center', justifyContent: 'center', gap: 8,
                flex: 1.4, padding: '14px 0',
                background: deploying ? '#4b5563' : (sendMode === 'scheduled' ? '#f59e0b' : '#00b0ff'),
                color: '#fff', border: 'none', borderRadius: 10, fontSize: 15, fontWeight: 600,
                cursor: deploying || savingDraft ? 'not-allowed' : 'pointer',
              }}
            >
              {deploying
                ? <><FontAwesomeIcon icon={faSpinner} spin /> Deploying...</>
                : sendMode === 'scheduled'
                  ? <><FontAwesomeIcon icon={faRocket} /> Schedule Campaign</>
                  : <><FontAwesomeIcon icon={faRocket} /> Deploy Now</>
              }
            </button>
          </div>
        </>
      )}
    </div>
  );

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
                if (!showClonePanel && cloneCandidates.length === 0) fetchCloneCandidates();
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

                {!cloneLoading && cloneCandidates.length === 0 && (
                  <div style={{ padding: 20, textAlign: 'center', color: '#64748b', fontSize: 12 }}>
                    No campaigns available to clone.
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
          <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.65)' }}>Step {step} of {STEPS.length}</div>
        </div>
      </div>

      {/* Step indicator */}
      <div className="ig-stagger" style={{ display: 'flex', padding: '12px 20px', gap: 4, borderBottom: '1px solid rgba(0,200,255,0.08)', background: '#0a1628', overflowX: 'auto' }}>
        {STEPS.map((s) => {
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
        {step === 1 && renderStep1()}
        {step === 2 && renderStep2()}
        {step === 3 && renderStep3()}
        {step === 4 && renderStep4()}
        {step === 5 && renderStep5()}
        {step === 6 && renderStep6()}
      </div>

      {/* Footer nav */}
      {!deployResult && (
        <div style={{ display: 'flex', justifyContent: 'space-between', padding: '12px 20px', borderTop: '1px solid rgba(0,200,255,0.08)', background: '#0a1628' }}>
          <button
            onClick={() => setStep(Math.max(1, step - 1))}
            disabled={step === 1}
            style={{
              display: 'flex', alignItems: 'center', gap: 6,
              padding: '8px 18px', borderRadius: 8, border: '1px solid rgba(0,200,255,0.08)',
              background: 'transparent', color: step === 1 ? '#4b5563' : '#e0e6f0',
              fontSize: 13, cursor: step === 1 ? 'default' : 'pointer',
            }}
          >
            <FontAwesomeIcon icon={faArrowLeft} /> Back
          </button>
          {step < 6 && (
            <button
              className="ig-btn-glow ig-ripple"
              onClick={() => {
                if (canProceed()) {
                  setStepAttempted(prev => ({ ...prev, [step]: false }));
                  setStep(Math.min(6, step + 1));
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
