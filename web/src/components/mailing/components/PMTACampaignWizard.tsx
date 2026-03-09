import React, { useState, useEffect, useCallback, useRef } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faArrowLeft, faArrowRight, faArrowUp, faArrowDown, faCheck, faServer, faGlobe,
  faPenFancy, faUsers, faBrain, faRocket, faSpinner,
  faExclamationTriangle, faCheckCircle, faTimesCircle,
  faPlus, faTimes, faChartBar, faShieldAlt, faCrosshairs,
  faMagic, faSave, faEye, faUpload, faCode, faGripVertical,
  faCopy, faTrophy, faChevronDown, faChevronUp,
} from '@fortawesome/free-solid-svg-icons';
import { useAuth } from '../../../contexts/AuthContext';
import { AnimatedCounter } from '../shared/AnimatedCounter';
import { useToast } from '../shared/ToastSystem';
import { JarvisCompleteModal } from '../shared/JarvisCompleteModal';

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
  gmail:     { label: 'Gmail',            color: '#ea4335', emoji: '📧' },
  yahoo:     { label: 'Yahoo',            color: '#7b1fa2', emoji: '🟣' },
  microsoft: { label: 'Microsoft',        color: '#0078d4', emoji: '🔷' },
  apple:     { label: 'Apple iCloud',     color: '#a2aaad', emoji: '🍎' },
  comcast:   { label: 'Comcast',          color: '#e60000', emoji: '📡' },
  att:       { label: 'AT&T',             color: '#009fdb', emoji: '📶' },
  cox:       { label: 'Cox',              color: '#f26522', emoji: '🔌' },
  charter:   { label: 'Charter/Spectrum', color: '#0099d6', emoji: '📺' },
};

const ALL_ISPS = ['gmail', 'yahoo', 'microsoft', 'apple', 'comcast', 'att', 'cox', 'charter'];

const DEFAULT_ISP_QUOTAS: Record<string, number> = {
  gmail:     50000,
  yahoo:     20000,
  microsoft: 20000,
  apple:     10000,
  comcast:    5000,
  att:        5000,
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

interface SendingDomain {
  domain: string;
  from_name?: string;
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
  complaint_count: number;
  campaign_date: string;
  open_rate: number;
  click_rate: number;
  bounce_rate: number;
  complaint_rate: number;
  has_config: boolean;
  recommended: boolean;
}

// ── Step navigation ──────────────────────────────────────────────────────────

const STEPS = [
  { id: 1, label: 'ISP Targeting',          icon: faServer },
  { id: 2, label: 'Sending Domain',         icon: faGlobe },
  { id: 3, label: 'Content + A/B',          icon: faPenFancy },
  { id: 4, label: 'Audience + Suppression', icon: faUsers },
  { id: 5, label: 'Infrastructure Intel',   icon: faBrain },
  { id: 6, label: 'Review + Deploy',        icon: faRocket },
];

// ── Main component ───────────────────────────────────────────────────────────

interface PMTACampaignWizardProps {
  onClose?: () => void;
}

export const PMTACampaignWizard: React.FC<PMTACampaignWizardProps> = ({ onClose }) => {
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

  // Step 2 state
  const [sendingDomains, setSendingDomains] = useState<SendingDomain[]>([]);
  const [selectedDomain, setSelectedDomain] = useState('');

  // Step 3 state
  const [variants, setVariants] = useState<ContentVariant[]>([
    { variant_name: 'A', from_name: '', subject: '', preview_text: '', html_content: '', split_percent: 100 },
  ]);
  const [templates, setTemplates] = useState<any[]>([]);
  const [showTemplatePicker, setShowTemplatePicker] = useState(false);

  // AI Generation state
  const [showAIGenerator, setShowAIGenerator] = useState(false);
  const [aiCampaignType, setAICampaignType] = useState('');
  const [aiGenerating, setAIGenerating] = useState(false);
  const [aiVariations, setAIVariations] = useState<any[]>([]);
  const [aiSelectedIdxs, setAISelectedIdxs] = useState<number[]>([]);
  const [aiPreviewIdx, setAIPreviewIdx] = useState<number | null>(null);
  const [aiError, setAIError] = useState('');
  const [aiSaving, setAISaving] = useState(false);

  // Step 4 state
  const [lists, setLists] = useState<{ id: string; name: string; subscriber_count: number }[]>([]);
  const [segments, setSegments] = useState<{ id: string; name: string; subscriber_count: number }[]>([]);
  const [suppressionLists, setSuppressionLists] = useState<{ id: string; name: string; entry_count: number }[]>([]);
  const [selectedLists, setSelectedLists] = useState<string[]>([]);
  const [selectedSegments, setSelectedSegments] = useState<string[]>([]);
  const [sendPriority, setSendPriority] = useState<{ id: string; type: 'list' | 'segment' }[]>([]);
  const [selectedSuppLists, setSelectedSuppLists] = useState<string[]>([]);
  const [selectedExclusionSegments, setSelectedExclusionSegments] = useState<string[]>([]);
  const [audienceEstimate, setAudienceEstimate] = useState<AudienceEstimate | null>(null);
  const [audienceError, setAudienceError] = useState('');

  // Step 5 state
  const [ispIntel, setISPIntel] = useState<ISPIntel[]>([]);

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
  const [globalScheduleTimezone, setGlobalScheduleTimezone] = useState(
    Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
  );
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

  const fetchAudienceData = useCallback(async () => {
    setAudienceError('');
    setAudienceDataLoading(true);
    try {
      const [listRes, segRes, suppRes] = await Promise.all([
        fetchWithRetry(`${API_BASE}/lists`),
        fetchWithRetry(`${API_BASE}/segments`),
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
    } catch (err) {
      console.warn('[Wizard] intel fetch failed:', err);
    }
    setIntelLoading(false);
  }, [fetchWithRetry, orgId, selectedISPs, audienceEstimate]);

  const fetchTemplates = useCallback(async () => {
    try {
      const res = await fetchWithRetry(`${API_BASE}/templates`);
      const data = await res.json();
      setTemplates(data.templates || []);
    } catch (err) {
      console.warn('[Wizard] templates fetch failed:', err);
    }
  }, [fetchWithRetry]);

  const handleAIGenerate = useCallback(async () => {
    if (!aiCampaignType || !selectedDomain) return;
    setAIGenerating(true);
    setAIError('');
    setAIVariations([]);
    setAISelectedIdxs([]);
    setAIPreviewIdx(null);
    try {
      const res = await orgFetch(`${API_BASE}/ai/generate-templates`, orgId, {
        method: 'POST',
        body: JSON.stringify({ campaign_type: aiCampaignType, sending_domain: selectedDomain }),
      });
      const data = await res.json();
      if (!res.ok) {
        setAIError(data.error || `Generation failed (HTTP ${res.status})`);
      } else {
        setAIVariations(data.variations || []);
      }
    } catch (err: any) {
      setAIError(err?.message || 'Generation failed — network error');
    }
    setAIGenerating(false);
  }, [aiCampaignType, selectedDomain, orgId]);

  const handleAIUseSelected = () => {
    if (aiSelectedIdxs.length === 0) return;
    const picked = aiSelectedIdxs.map(i => aiVariations[i]).filter(Boolean);
    const names = ['A', 'B', 'C', 'D'];
    const newVariants: ContentVariant[] = picked.map((v, i) => ({
      variant_name: names[i] || String.fromCharCode(65 + i),
      from_name: v.from_name || '',
      subject: v.subject || '',
      preview_text: v.preview_text || '',
      html_content: v.html_content || '',
      split_percent: Math.floor(100 / picked.length),
    }));
    if (newVariants.length > 0) {
      const remainder = 100 - newVariants.reduce((s, v) => s + v.split_percent, 0);
      newVariants[newVariants.length - 1].split_percent += remainder;
    }
    setVariants(newVariants);
    setShowAIGenerator(false);
  };

  const handleAISaveToLibrary = async () => {
    if (aiSelectedIdxs.length === 0 || !selectedDomain) return;
    setAISaving(true);
    try {
      const folderRes = await orgFetch(`${API_BASE}/template-folders`, orgId, {
        method: 'POST',
        body: JSON.stringify({ path: selectedDomain }),
      });
      const folder = await folderRes.json();
      const folderId = folder?.id;

      for (const idx of aiSelectedIdxs) {
        const v = aiVariations[idx];
        if (!v) continue;
        await orgFetch(`${API_BASE}/templates`, orgId, {
          method: 'POST',
          body: JSON.stringify({
            name: `${aiCampaignType} — Variant ${v.variant_name}`,
            description: `AI-generated ${aiCampaignType} template for ${selectedDomain}`,
            subject: v.subject,
            from_name: v.from_name,
            html_content: v.html_content,
            folder_id: folderId || undefined,
            status: 'active',
          }),
        });
      }
      fetchTemplates();
    } catch { /* noop */ }
    setAISaving(false);
  };

  // Load data on step entry
  useEffect(() => {
    if (step === 1) fetchReadiness();
    if (step === 2) fetchDomains();
    if (step === 3) fetchTemplates();
    if (step === 4) fetchAudienceData();
    if (step === 5) fetchIntel();
  }, [step, fetchReadiness, fetchDomains, fetchTemplates, fetchAudienceData, fetchIntel]);

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
        if (selectedISPs.length === 0) errors.push('Select at least one target ISP');
        break;
      case 2:
        if (!selectedDomain) errors.push('Select a sending domain');
        break;
      case 3:
        variants.forEach(v => {
          if (!v.from_name.trim()) errors.push(`Variant ${v.variant_name}: From Name is required`);
          if (!v.subject.trim()) errors.push(`Variant ${v.variant_name}: Subject Line is required`);
          if (!v.html_content.trim()) errors.push(`Variant ${v.variant_name}: HTML Content is required`);
        });
        if (Math.abs(variants.reduce((sum, v) => sum + v.split_percent, 0) - 100) >= 1) {
          errors.push('Split percentages must sum to 100%');
        }
        break;
      case 4:
        if (selectedLists.length === 0 && selectedSegments.length === 0) errors.push('Select at least one list or segment');
        break;
      case 6:
        if (!campaignName.trim()) errors.push('Campaign name is required');
        if (sendMode === 'scheduled' && scheduleMode === 'quick' && !scheduledAt) {
          errors.push('Scheduled date and time is required');
        }
        if (sendMode === 'scheduled' && scheduleMode === 'per-isp') {
          selectedISPs.forEach(isp => {
            const plan = ispPlansByKey[isp];
            if (!plan?.useCustomSchedule) return;
            const hasStartTime = plan.startTime && plan.startTime.trim() !== '';
            const validSpans = (plan.timeSpans || []).filter(span => span.startAt && span.endAt);
            if (!hasStartTime && validSpans.length === 0) {
              errors.push(`${ISP_META[isp]?.label || isp}: set a start time or add a time span`);
            } else if (validSpans.length > 0) {
              validSpans.forEach((span, idx) => {
                if (new Date(span.endAt).getTime() <= new Date(span.startAt).getTime()) {
                  errors.push(`${ISP_META[isp]?.label || isp}: time span ${idx + 1} end must be after start`);
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

  const toLocalInputValue = (raw?: string) => {
    if (!raw) return '';
    const parsed = new Date(raw);
    return Number.isNaN(parsed.getTime()) ? '' : toDateTimeLocal(parsed);
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

      // Clear stale state from previous wizard sessions
      setStepAttempted({});
      setAudienceEstimate(null);
      setAudienceError('');
      setISPIntel([]);
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

  useEffect(() => {
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
      .then(data => {
        if (cancelled || !data) return;
        hydrateDraft(data);
        setDraftError('');
        const loadedAt = data.updated_at ? new Date(data.updated_at).toLocaleString() : 'earlier';
        setDraftStatus(`Loaded saved draft from ${loadedAt}.`);
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
  }, [orgId, fetchWithRetry, hydrateDraft]);

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
    const quotaArray = Object.entries(ispQuotas)
      .filter(([, v]) => v > 0)
      .map(([isp, volume]) => ({ isp, volume }));
    const globalScheduleISO = scheduledAt ? new Date(scheduledAt).toISOString() : '';
    const ispPlans = selectedISPs.map(isp => {
      const plan = ispPlansByKey[isp] || buildDefaultISPPlan(isp);
      const useGlobalSchedule = scheduleMode === 'quick' || !plan.useCustomSchedule;
      const quota = ispQuotas[isp] || 0;

      let spans: any[] = [];
      let cadenceMode = 'single';
      let everyMinutes = 0;
      let batchSize = quota;

      if (sendMode === 'scheduled') {
        if (useGlobalSchedule) {
          if (globalScheduleISO) {
            spans = [{
              type: 'absolute',
              start_at: globalScheduleISO,
              end_at: globalScheduleISO,
              timezone: plan.timezone,
              source: 'global-default',
            }];
          }
        } else {
          const dur = plan.durationHours || 8;
          const interval = plan.everyMinutes || 15;
          const totalIntervals = Math.max(1, Math.floor(dur * 60 / interval));
          batchSize = quota > 0 ? Math.ceil(quota / totalIntervals) : plan.batchSize;
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
        throttle_strategy: plan.throttleStrategy || 'auto',
        timezone: plan.timezone,
        cadence: {
          mode: cadenceMode,
          every_minutes: everyMinutes,
          batch_size: batchSize,
        },
        time_spans: spans,
      };
    });

    const payload: Record<string, any> = {
      name: campaignName,
      target_isps: selectedISPs,
      sending_domain: selectedDomain,
      variants,
      isp_plans: ispPlans,
      isp_quotas: quotaArray,
      randomize_audience: randomizeAudience,
      inclusion_segments: sendPriority.filter(p => p.type === 'segment').map(p => p.id),
      inclusion_lists: sendPriority.filter(p => p.type === 'list').map(p => p.id),
      send_priority: sendPriority,
      exclusion_lists: selectedSuppLists,
      exclusion_segments: selectedExclusionSegments,
      send_days: [],
      send_hour: new Date().getUTCHours(),
      timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
      throttle_strategy: 'auto',
      send_mode: sendMode,
    };

    if (campaignId) {
      payload.campaign_id = campaignId;
    }
    if (sendMode === 'scheduled' && scheduledAt) {
      payload.scheduled_at = new Date(scheduledAt).toISOString();
    }
    return payload;
  }, [
    campaignId,
    campaignName,
    buildDefaultISPPlan,
    ispPlansByKey,
    ispQuotas,
    randomizeAudience,
    scheduleMode,
    scheduledAt,
    selectedDomain,
    selectedExclusionSegments,
    selectedISPs,
    selectedSuppLists,
    sendMode,
    sendPriority,
    variants,
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

  const handleDeploy = useCallback(async () => {
    setDeploying(true);
    setDeployResult(null);
    try {
      const res = await fetchWithRetry(`${API_BASE}/pmta-campaign/deploy`, {
        method: 'POST',
        body: JSON.stringify(buildCampaignPayload()),
      }, 3);
      const data = await res.json();
      if (!res.ok) {
        setDeployResult({ error: data.error || `Deploy failed (HTTP ${res.status})` });
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
  }, [buildCampaignPayload, campaignComplete, campaignId, campaignName, fetchWithRetry]);

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
      if (prev[0].from_name && prev[0].from_name.trim() !== '') return prev;
      const updated = [...prev];
      updated[0] = { ...updated[0], from_name: match.from_name! };
      return updated;
    });
  }, [selectedDomain, sendingDomains]);

  // ── Variant management ───────────────────────────────────────────────────

  const addVariant = () => {
    const names = ['A', 'B', 'C', 'D'];
    if (variants.length >= 4) return;
    const newPercent = Math.floor(100 / (variants.length + 1));
    const updated = variants.map(v => ({ ...v, split_percent: newPercent }));
    updated.push({
      variant_name: names[variants.length],
      from_name: '', subject: '', preview_text: '', html_content: '',
      split_percent: 100 - (newPercent * variants.length),
    });
    setVariants(updated);
  };

  const removeVariant = (idx: number) => {
    if (variants.length <= 1) return;
    const updated = variants.filter((_, i) => i !== idx);
    const each = Math.floor(100 / updated.length);
    const final = updated.map((v, i) => ({
      ...v,
      split_percent: i === updated.length - 1 ? 100 - each * (updated.length - 1) : each,
    }));
    setVariants(final);
  };

  const updateVariant = (idx: number, field: keyof ContentVariant, value: string | number) => {
    setVariants(prev => prev.map((v, i) => i === idx ? { ...v, [field]: value } : v));
  };

  // Track which variants have preview open
  const [variantPreviews, setVariantPreviews] = useState<Record<number, boolean>>({});
  const toggleVariantPreview = (idx: number) => {
    setVariantPreviews(prev => ({ ...prev, [idx]: !prev[idx] }));
  };

  // Refs for textarea cursor tracking (one per variant)
  const textareaRefs = useRef<Record<number, HTMLTextAreaElement | null>>({});

  const insertTagAtCursor = (idx: number, syntax: string) => {
    const textarea = textareaRefs.current[idx];
    const v = variants[idx];
    if (!v) return;
    const pos = textarea?.selectionStart ?? v.html_content.length;
    const before = v.html_content.slice(0, pos);
    const after = v.html_content.slice(pos);
    updateVariant(idx, 'html_content', before + syntax + after);
    setTimeout(() => {
      if (textarea) {
        textarea.focus();
        const newPos = pos + syntax.length;
        textarea.setSelectionRange(newPos, newPos);
      }
    }, 30);
  };

  const handleHTMLFileUpload = (idx: number, file: File) => {
    const reader = new FileReader();
    reader.onload = (e) => {
      const content = e.target?.result;
      if (typeof content === 'string') {
        updateVariant(idx, 'html_content', content);
      }
    };
    reader.readAsText(file);
  };

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
      <h3 style={{ margin: '0 0 4px' }}>Select Target ISPs<RequiredDot /></h3>
      <p style={{ margin: '0 0 16px', color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>
        Choose which ISP ecosystems to target. Cards show live health from the governance engine.
      </p>
      <StepErrorBanner stepNum={1} />

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
          {(ispReadiness.length > 0 ? ispReadiness : ALL_ISPS.map(isp => ({ isp, display_name: ISP_META[isp]?.label || isp, health_score: 0, status: 'unknown', active_agents: 0, total_agents: 6, bounce_rate: 0, deferral_rate: 0, complaint_rate: 0, warmup_ips: 0, active_ips: 0, quarantined_ips: 0, max_daily_capacity: 0, max_hourly_rate: 0, pool_name: '', has_emergency: false, warnings: [] }))).map((r: any) => {
            const meta = ISP_META[r.isp] || { label: r.display_name, color: '#64748b', emoji: '🌐' };
            const selected = selectedISPs.includes(r.isp);
            return (
              <div
                role="button"
                tabIndex={0}
                aria-pressed={selected}
                aria-label={`Select ${meta.label} ISP`}
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
                  <span>Bounce: <strong style={{ color: r.bounce_rate > 5 ? '#ef4444' : '#e0e6f0' }}>{r.bounce_rate.toFixed(1)}%</strong></span>
                </div>
                {r.ip_details && r.ip_details.length > 0 && (
                  <div style={{ marginTop: 8, display: 'flex', gap: 4, flexWrap: 'wrap' }}>
                    {r.ip_details.map((ipd: any) => (
                      <span key={ipd.ip} title={`${ipd.ip} — Score: ${ipd.score.toFixed(0)}, Bounce: ${ipd.bounce_rate.toFixed(1)}%, Deferral: ${ipd.deferral_rate.toFixed(1)}%`} style={{
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
          <FontAwesomeIcon icon={faCheckCircle} /> {selectedISPs.length} ISP{selectedISPs.length > 1 ? 's' : ''} selected: {selectedISPs.map(i => ISP_META[i]?.label || i).join(', ')}
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
          <h4 style={{ margin: '0 0 8px', fontSize: 13, color: 'rgba(180,210,240,0.65)' }}>
            <FontAwesomeIcon icon={faShieldAlt} /> Volume Quotas <span style={{ fontWeight: 400 }}>(optional)</span>
          </h4>
          <p style={{ margin: '0 0 12px', fontSize: 11, color: '#64748b' }}>
            Set maximum sends per ISP. Leave at 0 for unlimited.
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
              <span style={{ fontSize: 10, color: '#64748b' }}>Domains not matching any ISP above</span>
            </div>
            {Object.values(ispQuotas).some(v => v > 0) && (
              <div style={{ gridColumn: '1 / -1', fontSize: 12, color: '#10b981', padding: '4px 0', fontWeight: 600 }}>
                Total quota: {Object.values(ispQuotas).filter(v => v > 0).reduce((a, b) => a + b, 0).toLocaleString()}
              </div>
            )}
          </div>
        </div>
      </div>
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
    </div>
  );

  const loadTemplate = (tpl: any, variantIdx: number) => {
    updateVariant(variantIdx, 'subject', tpl.subject || '');
    updateVariant(variantIdx, 'html_content', tpl.html_content || '');
    if (tpl.from_name) updateVariant(variantIdx, 'from_name', tpl.from_name);
    if (tpl.preview_text) updateVariant(variantIdx, 'preview_text', tpl.preview_text);
    setShowTemplatePicker(false);
  };

  const CAMPAIGN_TYPES = [
    { id: 'welcome', label: 'Welcome Series', desc: 'New subscriber onboarding' },
    { id: 'newsletter', label: 'Newsletter', desc: 'Content-driven update' },
    { id: 'promotional', label: 'Promotional', desc: 'Offers & deals' },
    { id: 'winback', label: 'Win-Back', desc: 'Re-engage dormant subs' },
    { id: 're-engagement', label: 'Re-Engagement', desc: 'Gentle nudge campaign' },
    { id: 'announcement', label: 'Announcement', desc: 'Product or feature reveal' },
    { id: 'trivia', label: 'Trivia / Interactive', desc: 'Fun engagement campaign' },
  ];

  const renderAIGenerator = () => (
    <div style={{ background: '#1a1033', border: '1px solid #00e5ff', borderRadius: 12, padding: 20, marginBottom: 16 }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
        <div>
          <h4 style={{ margin: 0, color: 'rgba(0,200,255,0.7)', fontSize: 15 }}><FontAwesomeIcon icon={faMagic} /> AI Template Generator</h4>
          <p style={{ margin: '4px 0 0', color: 'rgba(180,210,240,0.65)', fontSize: 12 }}>Select a campaign type. AI will analyze <strong style={{ color: '#00b0ff' }}>{selectedDomain}</strong> for branding and generate 5 production-ready variations.</p>
        </div>
        <button onClick={() => setShowAIGenerator(false)} style={{ background: 'none', border: 'none', color: 'rgba(180,210,240,0.65)', cursor: 'pointer', fontSize: 16 }}><FontAwesomeIcon icon={faTimes} /></button>
      </div>

      {/* Campaign type selector */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(150px, 1fr))', gap: 8, marginBottom: 16 }}>
        {CAMPAIGN_TYPES.map(ct => (
          <div
            key={ct.id}
            role="button"
            tabIndex={0}
            aria-pressed={aiCampaignType === ct.id}
            onClick={() => setAICampaignType(ct.id)}
            onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); setAICampaignType(ct.id); } }}
            style={{
              background: aiCampaignType === ct.id ? 'rgba(0,200,255,0.12)' : '#0a0f1a',
              border: `2px solid ${aiCampaignType === ct.id ? '#00e5ff' : 'rgba(0,200,255,0.08)'}`,
              borderRadius: 8, padding: '10px 12px', cursor: 'pointer', transition: 'all 0.2s',
            }}
          >
            <div style={{ fontSize: 13, fontWeight: 600, color: aiCampaignType === ct.id ? 'rgba(0,200,255,0.7)' : '#e0e6f0' }}>{ct.label}</div>
            <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', marginTop: 2 }}>{ct.desc}</div>
          </div>
        ))}
      </div>

      {/* Generate button */}
      <button
        onClick={handleAIGenerate}
        disabled={!aiCampaignType || aiGenerating}
        style={{
          display: 'flex', alignItems: 'center', gap: 8, background: aiCampaignType && !aiGenerating ? '#00e5ff' : '#4b5563',
          color: '#fff', border: 'none', borderRadius: 8, padding: '10px 20px', fontSize: 14, fontWeight: 600,
          cursor: aiCampaignType && !aiGenerating ? 'pointer' : 'not-allowed', width: '100%', justifyContent: 'center',
        }}
      >
        {aiGenerating ? <><FontAwesomeIcon icon={faSpinner} spin /> Analyzing {selectedDomain} &amp; generating 5 variations...</> : <><FontAwesomeIcon icon={faMagic} /> Generate 5 Variations</>}
      </button>

      {aiError && (
        <div style={{ marginTop: 12, background: '#3b1a1a', border: '1px solid #e53935', borderRadius: 8, padding: '10px 14px', color: '#ff8a80', fontSize: 13 }}>
          <FontAwesomeIcon icon={faExclamationTriangle} /> {aiError}
        </div>
      )}

      {/* Generated variations */}
      {aiVariations.length > 0 && (
        <div style={{ marginTop: 16 }}>
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 }}>
            <span style={{ color: 'rgba(0,200,255,0.7)', fontSize: 13, fontWeight: 600 }}>Select variations to use ({aiSelectedIdxs.length} selected)</span>
            <div style={{ display: 'flex', gap: 8 }}>
              <button
                onClick={handleAISaveToLibrary}
                disabled={aiSelectedIdxs.length === 0 || aiSaving}
                style={{ display: 'flex', alignItems: 'center', gap: 6, background: aiSelectedIdxs.length > 0 && !aiSaving ? '#0d1526' : 'transparent', color: aiSelectedIdxs.length > 0 ? '#10b981' : '#4b5563', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 8, padding: '6px 12px', fontSize: 12, cursor: aiSelectedIdxs.length > 0 ? 'pointer' : 'default' }}
              >
                <FontAwesomeIcon icon={aiSaving ? faSpinner : faSave} spin={aiSaving} /> Save to Library
              </button>
              <button
                onClick={handleAIUseSelected}
                disabled={aiSelectedIdxs.length === 0}
                style={{ display: 'flex', alignItems: 'center', gap: 6, background: aiSelectedIdxs.length > 0 ? '#00e5ff' : '#4b5563', color: '#fff', border: 'none', borderRadius: 8, padding: '6px 12px', fontSize: 12, cursor: aiSelectedIdxs.length > 0 ? 'pointer' : 'default' }}
              >
                <FontAwesomeIcon icon={faCheck} /> Use Selected
              </button>
            </div>
          </div>

          <div className="ig-scale-in" style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(260px, 1fr))', gap: 10 }}>
            {aiVariations.map((v: any, idx: number) => {
              const isSelected = aiSelectedIdxs.includes(idx);
              return (
                <div
                  key={idx}
                  onClick={() => setAISelectedIdxs(prev => prev.includes(idx) ? prev.filter(i => i !== idx) : [...prev, idx])}
                  style={{
                    background: isSelected ? 'rgba(0,200,255,0.08)' : '#0a0f1a',
                    border: `2px solid ${isSelected ? '#00e5ff' : 'rgba(0,200,255,0.08)'}`,
                    borderRadius: 10, padding: 14, cursor: 'pointer', transition: 'all 0.2s', position: 'relative',
                  }}
                >
                  {isSelected && (
                    <div style={{ position: 'absolute', top: 8, right: 8, background: '#00e5ff', borderRadius: '50%', width: 22, height: 22, display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
                      <FontAwesomeIcon icon={faCheck} style={{ color: '#fff', fontSize: 11 }} />
                    </div>
                  )}
                  <div style={{ fontSize: 14, fontWeight: 600, color: 'rgba(0,200,255,0.7)', marginBottom: 8 }}>Variant {v.variant_name}</div>
                  <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', marginBottom: 4 }}>From: <span style={{ color: '#e0e6f0' }}>{v.from_name}</span></div>
                  <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', marginBottom: 10 }}>Subject: <span style={{ color: '#e0e6f0' }}>{v.subject}</span></div>
                  <div style={{ display: 'flex', gap: 6 }}>
                    <button
                      onClick={(e) => { e.stopPropagation(); setAIPreviewIdx(aiPreviewIdx === idx ? null : idx); }}
                      style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center', gap: 4, background: '#0d1526', color: '#00b0ff', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 6, padding: '6px 0', fontSize: 11, cursor: 'pointer' }}
                    >
                      <FontAwesomeIcon icon={faEye} /> Preview
                    </button>
                  </div>
                  {aiPreviewIdx === idx && (
                    <div style={{ marginTop: 10, background: '#fff', borderRadius: 8, overflow: 'hidden', maxHeight: 300, overflowY: 'auto' }}>
                      <iframe
                        srcDoc={v.html_content}
                        title={`Preview ${v.variant_name}`}
                        style={{ width: '100%', height: 280, border: 'none' }}
                        sandbox="allow-same-origin"
                      />
                    </div>
                  )}
                </div>
              );
            })}
          </div>
        </div>
      )}
    </div>
  );

  const renderStep3 = () => (
    <div className="wiz-step-content ig-fade-in">
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
        <div>
          <h3 style={{ margin: 0 }}>Content + A/B Split Testing</h3>
          <p style={{ margin: '4px 0 0', color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>Configure from-names, subject lines, and content. Add variants for A/B testing.</p>
        </div>
        <div style={{ display: 'flex', gap: 8 }}>
          <button className="ig-btn-cyber" onClick={() => { setShowAIGenerator(!showAIGenerator); setShowTemplatePicker(false); }} style={{ display: 'flex', alignItems: 'center', gap: 6, background: 'linear-gradient(135deg, #00e5ff, #00b0ff)', color: '#fff', border: 'none', borderRadius: 8, padding: '8px 14px', fontSize: 13, cursor: 'pointer', fontWeight: 600 }}>
            <FontAwesomeIcon icon={faMagic} /> Generate
          </button>
          <button onClick={() => { setShowTemplatePicker(!showTemplatePicker); setShowAIGenerator(false); }} style={{ display: 'flex', alignItems: 'center', gap: 6, background: '#0d1526', color: '#00b0ff', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 8, padding: '8px 14px', fontSize: 13, cursor: 'pointer' }}>
            <FontAwesomeIcon icon={faPenFancy} /> Load Template
          </button>
          {variants.length < 4 && (
            <button onClick={addVariant} style={{ display: 'flex', alignItems: 'center', gap: 6, background: '#00b0ff', color: '#fff', border: 'none', borderRadius: 8, padding: '8px 14px', fontSize: 13, cursor: 'pointer' }}>
              <FontAwesomeIcon icon={faPlus} /> Add Variant
            </button>
          )}
        </div>
      </div>
      <StepErrorBanner stepNum={3} />

      {showTemplatePicker && (
        <div style={{ background: '#0d1526', border: '1px solid #00b0ff', borderRadius: 10, padding: 16, marginBottom: 16 }}>
          <h4 style={{ margin: '0 0 12px', color: '#00b0ff', fontSize: 14 }}>Content Library — Select a Template</h4>
          {templates.length === 0 ? (
            <p style={{ color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>No templates saved yet. Create templates in the Content Library tab.</p>
          ) : (
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(220px, 1fr))', gap: 10 }}>
              {templates.map((tpl: any) => (
                <div key={tpl.id} style={{ background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 8, padding: 12, cursor: 'pointer', transition: 'border-color 0.2s' }}
                  onMouseEnter={e => (e.currentTarget.style.borderColor = '#00b0ff')}
                  onMouseLeave={e => (e.currentTarget.style.borderColor = 'rgba(0,200,255,0.08)')}>
                  <strong style={{ color: '#e0e6f0', fontSize: 13, display: 'block', marginBottom: 4 }}>{tpl.name}</strong>
                  <span style={{ color: 'rgba(180,210,240,0.65)', fontSize: 12 }}>{tpl.subject || tpl.description || 'No subject'}</span>
                  <div style={{ marginTop: 8, display: 'flex', gap: 6 }}>
                    {variants.map((v, idx) => (
                      <button key={idx} onClick={() => loadTemplate(tpl, idx)}
                        style={{ fontSize: 11, background: '#00b0ff', color: '#fff', border: 'none', borderRadius: 4, padding: '4px 8px', cursor: 'pointer' }}>
                        {String.fromCodePoint(0x2192)} Variant {v.variant_name}
                      </button>
                    ))}
                  </div>
                </div>
              ))}
            </div>
          )}
        </div>
      )}

      {showAIGenerator && renderAIGenerator()}

      {variants.map((v, idx) => (
        <div key={idx} className="ig-card-hover" style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 16, marginBottom: 12, position: 'relative' }}>
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 }}>
            <span style={{ fontSize: 14, fontWeight: 600, color: '#00b0ff' }}>Variant {v.variant_name}</span>
            <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
              <label style={{ fontSize: 12, color: 'rgba(180,210,240,0.65)', display: 'flex', alignItems: 'center', gap: 4 }}>
                Split:
                <input
                  type="number" min={1} max={100} value={v.split_percent}
                  onChange={e => updateVariant(idx, 'split_percent', Number(e.target.value))}
                  style={{ width: 50, background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 6, color: '#e0e6f0', padding: '4px 6px', fontSize: 12, textAlign: 'center' }}
                />%
              </label>
              {variants.length > 1 && (
                <button onClick={() => removeVariant(idx)} style={{ background: 'none', border: 'none', color: '#ef4444', cursor: 'pointer', fontSize: 14 }}>
                  <FontAwesomeIcon icon={faTimes} />
                </button>
              )}
            </div>
          </div>
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 10, marginBottom: 10 }}>
            <div>
              <label style={{ fontSize: 11, color: showErr(3) && !v.from_name.trim() ? '#ef4444' : 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>From Name<RequiredDot /></label>
              <input
                value={v.from_name} placeholder="e.g. Jarvis Team"
                onChange={e => updateVariant(idx, 'from_name', e.target.value)}
                style={{ width: '100%', background: '#0a0f1a', border: fieldBorder(!v.from_name.trim()), borderRadius: 6, color: '#e0e6f0', padding: '8px 10px', fontSize: 13, boxSizing: 'border-box', transition: 'border-color 0.2s' }}
              />
              {showErr(3) && !v.from_name.trim() && <div style={{ fontSize: 10, color: '#ef4444', marginTop: 3 }}>Required</div>}
            </div>
            <div>
              <label style={{ fontSize: 11, color: showErr(3) && !v.subject.trim() ? '#ef4444' : 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>Subject Line<RequiredDot /> <span style={{ color: '#64748b' }}>({v.subject.length} chars)</span></label>
              <input
                value={v.subject} placeholder="e.g. Don't miss this deal"
                onChange={e => updateVariant(idx, 'subject', e.target.value)}
                style={{ width: '100%', background: '#0a0f1a', border: fieldBorder(!v.subject.trim()), borderRadius: 6, color: '#e0e6f0', padding: '8px 10px', fontSize: 13, boxSizing: 'border-box', transition: 'border-color 0.2s' }}
              />
              {showErr(3) && !v.subject.trim()
                ? <div style={{ fontSize: 10, color: '#ef4444', marginTop: 2 }}>Required</div>
                : <div style={{ fontSize: 10, color: '#64748b', marginTop: 2 }}>Supports Liquid: {'{{ first_name }}'}, {'{{ last_name }}'}, {'{{ email }}'}</div>
              }
            </div>
          </div>
          <div style={{ marginBottom: 10 }}>
            <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>Pre-header <span style={{ color: '#64748b' }}>({v.preview_text.length}/150 chars)</span></label>
            <input
              value={v.preview_text} placeholder="Preview text shown in inbox before opening"
              onChange={e => updateVariant(idx, 'preview_text', e.target.value)}
              maxLength={150}
              style={{ width: '100%', background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 6, color: '#e0e6f0', padding: '8px 10px', fontSize: 13, boxSizing: 'border-box' }}
            />
            <div style={{ fontSize: 10, color: '#64748b', marginTop: 2 }}>Supports Liquid tags. Shown as email preview text in inbox.</div>
          </div>

          {/* HTML Content with merge tag toolbar, upload, and preview */}
          <div>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 6 }}>
              <label style={{ fontSize: 11, color: showErr(3) && !v.html_content.trim() ? '#ef4444' : 'rgba(180,210,240,0.65)' }}>HTML Content<RequiredDot /></label>
              <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
                {/* Upload HTML file */}
                <label style={{ display: 'flex', alignItems: 'center', gap: 4, background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 6, padding: '4px 10px', fontSize: 11, color: 'rgba(180,210,240,0.65)', cursor: 'pointer' }}>
                  <FontAwesomeIcon icon={faUpload} />
                  Upload HTML
                  <input
                    type="file" accept=".html,.htm" style={{ display: 'none' }}
                    onChange={e => { const f = e.target.files?.[0]; if (f) handleHTMLFileUpload(idx, f); e.target.value = ''; }}
                  />
                </label>
                {/* Preview toggle */}
                <button
                  onClick={() => toggleVariantPreview(idx)}
                  style={{ display: 'flex', alignItems: 'center', gap: 4, background: variantPreviews[idx] ? 'rgba(0,200,255,0.12)' : '#0a0f1a', color: variantPreviews[idx] ? '#00b0ff' : 'rgba(180,210,240,0.65)', border: `1px solid ${variantPreviews[idx] ? '#00b0ff' : 'rgba(0,200,255,0.08)'}`, borderRadius: 6, padding: '4px 10px', fontSize: 11, cursor: 'pointer' }}
                >
                  <FontAwesomeIcon icon={faEye} /> {variantPreviews[idx] ? 'Hide Preview' : 'Preview'}
                </button>
              </div>
            </div>

            {/* Merge tag quick-insert toolbar */}
            <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4, marginBottom: 6, alignItems: 'center' }}>
              <span style={{ fontSize: 10, color: '#64748b', fontWeight: 600, marginRight: 4 }}>
                <FontAwesomeIcon icon={faCode} /> Insert Tag:
              </span>
              {[
                { label: 'First Name', syntax: '{{ first_name | default: "there" }}' },
                { label: 'Email', syntax: '{{ email }}' },
                { label: 'Company', syntax: '{{ custom.company }}' },
                { label: 'Unsubscribe', syntax: '{{ system.unsubscribe_url }}' },
                { label: 'Preferences', syntax: '{{ system.preferences_url }}' },
              ].map(tag => (
                <button
                  key={tag.label}
                  onClick={() => insertTagAtCursor(idx, tag.syntax)}
                  title={tag.syntax}
                  style={{ padding: '3px 8px', borderRadius: 4, fontSize: 10, fontWeight: 500, background: 'rgba(0,200,255,0.08)', color: 'rgba(0,200,255,0.7)', border: '1px solid #3d3e4e', cursor: 'pointer', transition: 'all 0.15s' }}
                  onMouseEnter={e => { e.currentTarget.style.background = 'rgba(0,200,255,0.19)'; e.currentTarget.style.borderColor = '#00b0ff'; }}
                  onMouseLeave={e => { e.currentTarget.style.background = 'rgba(0,200,255,0.08)'; e.currentTarget.style.borderColor = '#3d3e4e'; }}
                >
                  {tag.label}
                </button>
              ))}
            </div>

            <textarea
              ref={el => { textareaRefs.current[idx] = el; }}
              value={v.html_content} rows={12} placeholder="Paste or type HTML content here, or upload an .html file above..."
              onChange={e => updateVariant(idx, 'html_content', e.target.value)}
              style={{ width: '100%', background: '#0a0f1a', border: fieldBorder(!v.html_content.trim()), borderRadius: 6, color: '#e0e6f0', padding: '8px 10px', fontSize: 12, fontFamily: 'monospace', resize: 'vertical', boxSizing: 'border-box', minHeight: 150, transition: 'border-color 0.2s' }}
            />
            {showErr(3) && !v.html_content.trim() && <div style={{ fontSize: 10, color: '#ef4444', marginTop: 3 }}>HTML content is required</div>}

            {/* Show detected liquid tags */}
            {v.html_content.match(/\{\{[^}]+\}\}/g) && (
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4, marginTop: 6, alignItems: 'center' }}>
                <span style={{ fontSize: 10, color: '#10b981', fontWeight: 600 }}>Detected tags:</span>
                {[...new Set(v.html_content.match(/\{\{[^}]+\}\}/g) || [])].slice(0, 10).map((tag, i) => (
                  <code key={i} style={{ padding: '2px 6px', borderRadius: 4, fontSize: 10, background: '#10b98115', color: '#10b981', border: '1px solid #10b98130' }}>{tag}</code>
                ))}
              </div>
            )}

            {/* Inline preview */}
            {variantPreviews[idx] && v.html_content.trim() && (
              <div style={{ marginTop: 10, borderRadius: 8, overflow: 'hidden', border: '1px solid rgba(0,200,255,0.08)' }}>
                <div style={{ background: 'rgba(0,200,255,0.08)', padding: '6px 10px', fontSize: 11, color: 'rgba(180,210,240,0.65)', display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                  <span>HTML Preview</span>
                  <span style={{ fontSize: 10, color: '#64748b' }}>Liquid tags shown as raw text (resolved at send time)</span>
                </div>
                <div style={{ background: '#fff' }}>
                  <iframe
                    srcDoc={v.html_content}
                    title={`Preview Variant ${v.variant_name}`}
                    style={{ width: '100%', height: 400, border: 'none' }}
                    sandbox="allow-same-origin"
                  />
                </div>
              </div>
            )}
          </div>
        </div>
      ))}

      {/* Split validation */}
      {(() => {
        const total = variants.reduce((s, v) => s + v.split_percent, 0);
        if (Math.abs(total - 100) >= 1) {
          return (
            <div style={{ padding: '8px 12px', background: '#ef444415', borderRadius: 8, fontSize: 12, color: '#ef4444' }}>
              <FontAwesomeIcon icon={faExclamationTriangle} /> Split percentages sum to {total}% — must equal 100%.
            </div>
          );
        }
        return null;
      })()}
    </div>
  );

  const renderStep4 = () => {
    const totalSelected = selectedLists.length + selectedSegments.length;
    const totalSuppSelected = selectedSuppLists.length + selectedExclusionSegments.length;

    const AudienceCard: React.FC<{
      name: string; count: number; selected: boolean; type: 'list' | 'segment' | 'suppression' | 'exclusion-segment';
      onToggle: () => void;
    }> = ({ name, count, selected, type, onToggle }) => {
      const colors: Record<string, string> = { list: '#00b0ff', segment: '#8b5cf6', suppression: '#ef4444', 'exclusion-segment': '#f59e0b' };
      const icons: Record<string, any> = { list: faUsers, segment: faChartBar, suppression: faShieldAlt, 'exclusion-segment': faCrosshairs };
      const c = colors[type];
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
            <div style={{ fontSize: 12, fontWeight: 500, color: '#e0e6f0', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{name}</div>
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

    return (
      <div className="wiz-step-content ig-fade-in">
        <h3 style={{ margin: '0 0 4px' }}>Audience + Suppression<RequiredDot /></h3>
        <p style={{ margin: '0 0 16px', color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>
          Build your target audience and apply suppression filters.
        </p>
        <StepErrorBanner stepNum={4} />

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

                <div style={{ display: 'flex', flexDirection: 'column', gap: 6, maxHeight: 260, overflowY: 'auto', paddingRight: 4 }}>
                  {lists.map(l => (
                    <AudienceCard key={`list-${l.id}`} name={l.name} count={l.subscriber_count || 0}
                      selected={selectedLists.includes(l.id)} type="list" onToggle={() => toggleList(l.id)} />
                  ))}
                  {segments.map(s => (
                    <AudienceCard key={`seg-${s.id}`} name={s.name} count={s.subscriber_count || 0}
                      selected={selectedSegments.includes(s.id)} type="segment" onToggle={() => toggleSegment(s.id)} />
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

                <div style={{ display: 'flex', flexDirection: 'column', gap: 6, maxHeight: 200, overflowY: 'auto', paddingRight: 4 }}>
                  {suppressionLists.map(sl => (
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
                    <div style={{ display: 'flex', flexDirection: 'column', gap: 6, maxHeight: 200, overflowY: 'auto', paddingRight: 4 }}>
                      {segments.map(s => (
                        <AudienceCard key={`excl-seg-${s.id}`} name={s.name} count={s.subscriber_count || 0}
                          selected={selectedExclusionSegments.includes(s.id)} type="exclusion-segment"
                          onToggle={() => toggleExclusionSegment(s.id)} />
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
                      <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.4)', marginBottom: 8, textTransform: 'uppercase', letterSpacing: 0.5 }}>ISP Distribution</div>
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
        <h3 style={{ margin: '0 0 4px' }}>Infrastructure Intelligence</h3>
        <p style={{ margin: '0 0 16px', color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>
          Live state of the targeted ecosystem — throughput, warmup, conviction insights, and active warnings.
        </p>

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
              <FontAwesomeIcon icon={faSpinner} spin /> Querying governance engine...
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

      {deployResult ? (
        <div style={{ textAlign: 'center', padding: 40 }}>
          {deployResult.error ? (
            <div style={{ color: '#ef4444' }}>
              <FontAwesomeIcon icon={faTimesCircle} size="3x" style={{ marginBottom: 12 }} />
              <h3>Deploy Failed</h3>
              <p>{deployResult.error}</p>
              <div style={{ display: 'flex', gap: 12, justifyContent: 'center', marginTop: 16 }}>
                <button onClick={handleDeploy} disabled={deploying}
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
              <p>{deployResult.variant_count} variant{deployResult.variant_count > 1 ? 's' : ''} targeting {deployResult.target_isps?.length} ISP{deployResult.target_isps?.length > 1 ? 's' : ''}</p>
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
                    {mode === 'quick' ? 'Quick Schedule' : 'Per-ISP Plans'}
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
                      <span style={{ fontSize: 10, color: 'rgba(180,210,240,0.4)', marginLeft: 'auto' }}>Configure once, apply to all ISPs</span>
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
                      Apply to All ISPs
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
            <SummaryCard title="Target ISPs" value={selectedISPs.map(i => ISP_META[i]?.label || i).join(', ')} />
            <SummaryCard title="Sending Domain" value={selectedDomain} />
            <SummaryCard title="Variants" value={`${variants.length} variant${variants.length > 1 ? 's' : ''} (${variants.map(v => `${v.variant_name}: ${v.split_percent}%`).join(', ')})`} />
            <SummaryCard title="Audience" value={audienceEstimate ? `${audienceEstimate.after_suppressions.toLocaleString()} recipients` : 'Not estimated'} />
            <SummaryCard title="Schedule Mode" value={sendMode === 'immediate' ? 'Immediate' : scheduleMode === 'quick' ? `Quick: ${scheduledAt || 'Not set'}` : 'Per-ISP custom plans'} />
            <SummaryCard title="ISP Plan Summary" value={
              sendMode === 'scheduled' && scheduleMode === 'per-isp'
                ? selectedISPs.map(isp => {
                    const plan = ispPlansByKey[isp];
                    const spanCount = plan?.timeSpans?.length || 0;
                    return `${ISP_META[isp]?.label || isp}: ${spanCount} span${spanCount === 1 ? '' : 's'} / ${plan?.cadenceMode || 'single'}`;
                  }).join(' | ')
                : 'Global schedule applies to all selected ISPs'
            } />
            <SummaryCard title="From Names" value={variants.map(v => v.from_name).filter(Boolean).join(' / ') || '—'} />
            <SummaryCard title="Subject Lines" value={variants.map(v => v.subject).filter(Boolean).join(' / ') || '—'} />
            <SummaryCard title="Pre-header" value={variants[0]?.preview_text || '(none)'} />
            <SummaryCard title="ISP Quotas" value={
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
                  ? 'Audience will be shuffled randomly before applying ISP quotas.'
                  : 'Subscribers selected in list order until each ISP quota is reached.'}
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
          <h2 style={{ margin: 0, fontSize: 16, fontWeight: 700, letterSpacing: 1 }}>PMTA Campaign Wizard</h2>
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
                    No PMTA campaigns available to clone.
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
                      {c.bounce_rate > 5 && <span style={{ color: '#ef4444' }}>{c.bounce_rate}% bounced</span>}
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
