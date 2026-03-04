import React, { useState, useEffect, useCallback, useRef } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faArrowLeft, faArrowRight, faCheck, faServer, faGlobe,
  faPenFancy, faUsers, faBrain, faRocket, faSpinner,
  faExclamationTriangle, faCheckCircle, faTimesCircle,
  faPlus, faTimes, faChartBar, faShieldAlt,
  faMagic, faSave, faEye, faUpload, faCode,
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
  const [loading, setLoading] = useState(false);

  // Step 1 state
  const [ispReadiness, setISPReadiness] = useState<ISPReadiness[]>([]);
  const [selectedISPs, setSelectedISPs] = useState<string[]>([]);
  const [ispQuotas, setISPQuotas] = useState<Record<string, number>>({});
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
  const [segments, setSegments] = useState<{ id: string; name: string; cached_count: number }[]>([]);
  const [suppressionLists, setSuppressionLists] = useState<{ id: string; name: string; entry_count: number }[]>([]);
  const [selectedLists, setSelectedLists] = useState<string[]>([]);
  const [selectedSegments, setSelectedSegments] = useState<string[]>([]);
  const [selectedSuppLists, setSelectedSuppLists] = useState<string[]>([]);
  const [audienceEstimate, setAudienceEstimate] = useState<AudienceEstimate | null>(null);
  const [audienceError, setAudienceError] = useState('');

  // Step 5 state
  const [ispIntel, setISPIntel] = useState<ISPIntel[]>([]);

  // Step 6 state
  const [campaignName, setCampaignName] = useState('');
  const [sendMode, setSendMode] = useState<'immediate' | 'scheduled'>('immediate');
  const [scheduledAt, setScheduledAt] = useState('');
  const [recommendations, setRecommendations] = useState<any[]>([]);
  const [recsLoading, setRecsLoading] = useState(false);
  const [recsLoaded, setRecsLoaded] = useState(false);
  const [deploying, setDeploying] = useState(false);
  const [deployResult, setDeployResult] = useState<any>(null);
  const [domainError, setDomainError] = useState('');
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
    setLoading(true);
    try {
      const res = await fetchWithRetry(`${API_BASE}/pmta-campaign/readiness`);
      const data = await res.json();
      setISPReadiness(data.isps || []);
    } catch (err) {
      console.warn('[Wizard] readiness fetch failed:', err);
    }
    setLoading(false);
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
  }, [fetchWithRetry]);

  const fetchAudienceEstimate = useCallback(async () => {
    if (selectedLists.length === 0 && selectedSegments.length === 0) {
      setAudienceEstimate(null);
      return;
    }
    try {
      const res = await fetchWithRetry(`${API_BASE}/pmta-campaign/estimate-audience`, {
        method: 'POST',
        body: JSON.stringify({
          list_ids: selectedLists,
          segment_ids: selectedSegments,
          suppression_list_ids: selectedSuppLists,
          target_isps: selectedISPs,
        }),
      });
      const data = await res.json();
      setAudienceEstimate(data);
    } catch (err) {
      console.warn('[Wizard] audience estimate failed:', err);
    }
  }, [fetchWithRetry, selectedLists, selectedSegments, selectedSuppLists, selectedISPs]);

  const fetchIntel = useCallback(async () => {
    setLoading(true);
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
    setLoading(false);
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
  }, [step, selectedLists, selectedSegments, selectedSuppLists, fetchAudienceEstimate]);

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

  const canProceed = (): boolean => {
    switch (step) {
      case 1: return selectedISPs.length > 0;
      case 2: return selectedDomain !== '';
      case 3: return variants.every(v => v.from_name && v.subject && v.html_content.trim().length > 0) &&
                     Math.abs(variants.reduce((s, v) => s + v.split_percent, 0) - 100) < 1;
      case 4: return selectedLists.length > 0 || selectedSegments.length > 0;
      case 5: return true;
      case 6: return campaignName.trim() !== '' && (sendMode === 'immediate' || !!scheduledAt);
      default: return false;
    }
  };

  // ── Deploy ───────────────────────────────────────────────────────────────

  const handleDeploy = async () => {
    setDeploying(true);
    setDeployResult(null);
    try {
      const quotaArray = Object.entries(ispQuotas)
        .filter(([, v]) => v > 0)
        .map(([isp, volume]) => ({ isp, volume }));
      const payload: Record<string, any> = {
        name: campaignName,
        target_isps: selectedISPs,
        sending_domain: selectedDomain,
        variants,
        isp_quotas: quotaArray,
        randomize_audience: randomizeAudience,
        inclusion_segments: selectedSegments,
        inclusion_lists: selectedLists,
        exclusion_lists: selectedSuppLists,
        send_days: [],
        send_hour: new Date().getUTCHours(),
        timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
        throttle_strategy: 'auto',
        send_mode: sendMode,
      };
      if (sendMode === 'scheduled' && scheduledAt) {
        payload.scheduled_at = new Date(scheduledAt).toISOString();
      }
      const res = await fetchWithRetry(`${API_BASE}/pmta-campaign/deploy`, {
        method: 'POST',
        body: JSON.stringify(payload),
      }, 3);
      const data = await res.json();
      if (!res.ok) {
        setDeployResult({ error: data.error || `Deploy failed (HTTP ${res.status})` });
      } else {
        setDeployResult(data);
        campaignComplete(campaignName || 'Campaign');
        setShowCompleteModal(true);
      }
    } catch (err: any) {
      setDeployResult({ error: err?.message || 'Deploy failed — network error. Click Deploy to retry.' });
    }
    setDeploying(false);
  };

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
    setSelectedLists(prev => prev.includes(id) ? prev.filter(i => i !== id) : [...prev, id]);
  };
  const toggleSegment = (id: string) => {
    setSelectedSegments(prev => prev.includes(id) ? prev.filter(i => i !== id) : [...prev, id]);
  };
  const toggleSuppList = (id: string) => {
    setSelectedSuppLists(prev => prev.includes(id) ? prev.filter(i => i !== id) : [...prev, id]);
  };

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
      <h3 style={{ margin: '0 0 4px' }}>Select Target ISPs</h3>
      <p style={{ margin: '0 0 16px', color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>
        Choose which ISP ecosystems to target. Cards show live health from the governance engine.
      </p>
      {loading && <div style={{ textAlign: 'center', padding: 40, color: 'rgba(180,210,240,0.65)' }}><FontAwesomeIcon icon={faSpinner} spin /> Loading readiness data...</div>}
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
                transition: 'all 0.2s',
              }}
            >
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8 }}>
                <span style={{ fontSize: 18 }}>{meta.emoji} <strong style={{ color: meta.color }}>{meta.label}</strong></span>
                {statusBadge(r.status)}
              </div>
              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '6px 16px', fontSize: 12, color: 'rgba(180,210,240,0.65)' }}>
                <span>Health: <strong style={{ color: '#e0e6f0' }}>{r.health_score.toFixed(0)}%</strong></span>
                <span>Agents: <strong style={{ color: '#e0e6f0' }}>{r.active_agents}/{r.total_agents}</strong></span>
                <span>Active IPs: <strong style={{ color: '#e0e6f0' }}>{r.active_ips}</strong></span>
                <span>Warmup IPs: <strong style={{ color: '#e0e6f0' }}>{r.warmup_ips}</strong></span>
                <span>Capacity: <strong style={{ color: '#e0e6f0' }}>{(r.max_daily_capacity / 1000).toFixed(0)}k/day</strong></span>
                <span>Bounce: <strong style={{ color: r.bounce_rate > 5 ? '#ef4444' : '#e0e6f0' }}>{r.bounce_rate.toFixed(1)}%</strong></span>
              </div>
              {/* Per-IP status breakdown */}
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
      {selectedISPs.length > 0 && (
        <div style={{ marginTop: 12, padding: '8px 12px', background: '#10b98115', borderRadius: 8, fontSize: 13, color: '#10b981' }}>
          <FontAwesomeIcon icon={faCheckCircle} /> {selectedISPs.length} ISP{selectedISPs.length > 1 ? 's' : ''} selected: {selectedISPs.map(i => ISP_META[i]?.label || i).join(', ')}
        </div>
      )}

      {selectedISPs.length > 0 && (
        <div style={{ marginTop: 16, background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 14 }}>
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
                <div key={isp} style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '6px 10px', background: '#0a0f1a', borderRadius: 6, border: '1px solid rgba(0,200,255,0.06)' }}>
                  <span style={{ fontSize: 12, color: meta.color, minWidth: 80 }}>{meta.emoji} {meta.label}</span>
                  <input
                    type="number" min={0} step={1000}
                    value={ispQuotas[isp] || 0}
                    onChange={e => setISPQuotas(prev => ({ ...prev, [isp]: Number(e.target.value) }))}
                    style={{ flex: 1, width: 80, background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 4, color: '#e0e6f0', padding: '4px 8px', fontSize: 12, textAlign: 'right' }}
                  />
                </div>
              );
            })}
          </div>
        </div>
      )}
    </div>
  );

  const renderStep2 = () => (
    <div className="wiz-step-content ig-fade-in">
      <h3 style={{ margin: '0 0 4px' }}>Select Sending Domain</h3>
      <p style={{ margin: '0 0 16px', color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>
        Choose the domain that will appear in the "From" address. Each domain shows DNS and IP pool info.
      </p>
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
              <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>From Name</label>
              <input
                value={v.from_name} placeholder="e.g. Jarvis Team"
                onChange={e => updateVariant(idx, 'from_name', e.target.value)}
                style={{ width: '100%', background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 6, color: '#e0e6f0', padding: '8px 10px', fontSize: 13, boxSizing: 'border-box' }}
              />
            </div>
            <div>
              <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>Subject Line <span style={{ color: '#64748b' }}>({v.subject.length} chars)</span></label>
              <input
                value={v.subject} placeholder="e.g. Don't miss this deal"
                onChange={e => updateVariant(idx, 'subject', e.target.value)}
                style={{ width: '100%', background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 6, color: '#e0e6f0', padding: '8px 10px', fontSize: 13, boxSizing: 'border-box' }}
              />
              <div style={{ fontSize: 10, color: '#64748b', marginTop: 2 }}>Supports Liquid: {'{{ first_name }}'}, {'{{ last_name }}'}, {'{{ email }}'}</div>
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
              <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)' }}>HTML Content</label>
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
              style={{ width: '100%', background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 6, color: '#e0e6f0', padding: '8px 10px', fontSize: 12, fontFamily: 'monospace', resize: 'vertical', boxSizing: 'border-box', minHeight: 150 }}
            />

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

  const renderStep4 = () => (
    <div className="wiz-step-content ig-fade-in">
      <h3 style={{ margin: '0 0 4px' }}>Audience + Suppression</h3>
      <p style={{ margin: '0 0 16px', color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>
        Select inclusion lists/segments and exclusion suppression lists.
      </p>
      {audienceError && (
        <div style={{ background: '#3b1a1a', border: '1px solid #e53935', borderRadius: 8, padding: '10px 14px', marginBottom: 16, color: '#ff8a80', fontSize: 13, display: 'flex', alignItems: 'center', gap: 8 }}>
          <FontAwesomeIcon icon={faExclamationTriangle} /> {audienceError}
          <button onClick={fetchAudienceData} style={{ marginLeft: 'auto', background: '#00b0ff', color: '#fff', border: 'none', borderRadius: 6, padding: '4px 12px', fontSize: 12, cursor: 'pointer' }}>
            Retry
          </button>
        </div>
      )}

      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16, marginBottom: 16 }}>
        {/* Inclusion */}
        <div>
          <h4 style={{ margin: '0 0 8px', fontSize: 13, color: '#10b981' }}>
            <FontAwesomeIcon icon={faCheckCircle} /> Inclusion
          </h4>
          <div style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 8, padding: 10, maxHeight: 200, overflowY: 'auto' }}>
            {lists.length === 0 && segments.length === 0 && (
              <div style={{ color: '#64748b', fontSize: 12, padding: 10 }}>No lists or segments available.</div>
            )}
            {lists.map(l => (
              <label key={`list-${l.id}`} style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '6px 4px', cursor: 'pointer', fontSize: 12, color: '#e0e6f0', borderBottom: '1px solid #0a1628' }}>
                <input type="checkbox" checked={selectedLists.includes(l.id)} onChange={() => toggleList(l.id)} />
                <span style={{ flex: 1 }}>{l.name}</span>
                <span style={{ color: 'rgba(180,210,240,0.65)' }}>{(l.subscriber_count || 0).toLocaleString()}</span>
              </label>
            ))}
            {segments.map(s => (
              <label key={`seg-${s.id}`} style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '6px 4px', cursor: 'pointer', fontSize: 12, color: '#00b0ff', borderBottom: '1px solid #0a1628' }}>
                <input type="checkbox" checked={selectedSegments.includes(s.id)} onChange={() => toggleSegment(s.id)} />
                <span style={{ flex: 1 }}>{s.name}</span>
                <span style={{ color: 'rgba(180,210,240,0.65)' }}>{(s.cached_count || 0).toLocaleString()}</span>
              </label>
            ))}
          </div>
        </div>

        {/* Exclusion */}
        <div>
          <h4 style={{ margin: '0 0 8px', fontSize: 13, color: '#ef4444' }}>
            <FontAwesomeIcon icon={faTimesCircle} /> Suppression
          </h4>
          <div style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 8, padding: 10, maxHeight: 200, overflowY: 'auto' }}>
            {suppressionLists.length === 0 && (
              <div style={{ color: '#64748b', fontSize: 12, padding: 10 }}>No suppression lists available.</div>
            )}
            {suppressionLists.map(sl => (
              <label key={`supp-${sl.id}`} style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '6px 4px', cursor: 'pointer', fontSize: 12, color: '#e0e6f0', borderBottom: '1px solid #0a1628' }}>
                <input type="checkbox" checked={selectedSuppLists.includes(sl.id)} onChange={() => toggleSuppList(sl.id)} />
                <span style={{ flex: 1 }}>{sl.name}</span>
                <span style={{ color: 'rgba(180,210,240,0.65)' }}>{(sl.entry_count || 0).toLocaleString()}</span>
              </label>
            ))}
          </div>
        </div>
      </div>

      {/* Audience estimate */}
      {audienceEstimate && (
        <div style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 14 }}>
          <h4 style={{ margin: '0 0 10px', fontSize: 13, color: '#e0e6f0' }}>
            <FontAwesomeIcon icon={faChartBar} /> Audience Estimate
          </h4>
          <div style={{ display: 'flex', gap: 20, fontSize: 13, marginBottom: 12, flexWrap: 'wrap' }}>
            <span style={{ color: 'rgba(180,210,240,0.65)' }}>Total: <strong style={{ color: '#e0e6f0' }}>{audienceEstimate.total_recipients.toLocaleString()}</strong></span>
            <span style={{ color: 'rgba(180,210,240,0.65)' }}>Suppressed: <strong style={{ color: '#ef4444' }}>-{audienceEstimate.suppressed_count.toLocaleString()}</strong></span>
            <span style={{ color: 'rgba(180,210,240,0.65)' }}>Net: <strong style={{ color: '#10b981' }}>{audienceEstimate.after_suppressions.toLocaleString()}</strong></span>
          </div>
          {audienceEstimate.suppression_sources && Object.keys(audienceEstimate.suppression_sources).length > 0 && (
            <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', marginBottom: 12 }}>
              <span style={{ fontSize: 11, color: '#64748b', alignSelf: 'center' }}>Sources:</span>
              {Object.entries(audienceEstimate.suppression_sources).map(([source, count]) => (
                <span key={source} style={{ display: 'inline-flex', alignItems: 'center', gap: 3, padding: '2px 8px', borderRadius: 4, fontSize: 11, background: '#ef444415', color: '#ef4444', border: '1px solid #ef444430' }}>
                  {source}: {(count as number).toLocaleString()}
                </span>
              ))}
            </div>
          )}
          {audienceEstimate.isp_breakdown && Object.keys(audienceEstimate.isp_breakdown).length > 0 && (
            <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
              {Object.entries(audienceEstimate.isp_breakdown).map(([isp, count]) => {
                const meta = ISP_META[isp];
                return (
                  <span key={isp} style={{ display: 'inline-flex', alignItems: 'center', gap: 4, padding: '3px 8px', borderRadius: 6, fontSize: 11, background: (meta?.color || '#64748b') + '15', color: meta?.color || 'rgba(180,210,240,0.65)', border: `1px solid ${(meta?.color || '#64748b')}33` }}>
                    {meta?.emoji || '🌐'} {meta?.label || isp}: {(count as number).toLocaleString()}
                  </span>
                );
              })}
            </div>
          )}
        </div>
      )}
    </div>
  );

  const renderStep5 = () => (
    <div className="wiz-step-content ig-fade-in">
      <h3 style={{ margin: '0 0 4px' }}>Infrastructure Intelligence</h3>
      <p style={{ margin: '0 0 16px', color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>
        Live state of the targeted ecosystem — throughput, warmup, conviction insights, and active warnings.
      </p>
      {loading && <div style={{ textAlign: 'center', padding: 40, color: 'rgba(180,210,240,0.65)' }}><FontAwesomeIcon icon={faSpinner} spin /> Querying governance engine...</div>}
      <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
        {ispIntel.map(intel => {
          const meta = ISP_META[intel.isp] || { label: intel.display_name, color: '#64748b', emoji: '🌐' };
          return (
            <div key={intel.isp} style={{ background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 10, padding: 16 }}>
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 }}>
                <span style={{ fontSize: 15, fontWeight: 600, color: meta.color }}>{meta.emoji} {meta.label}</span>
                <div style={{ display: 'flex', gap: 6 }}>
                  {statusBadge(intel.throughput.status)}
                  {statusBadge(intel.warmup_summary.status)}
                </div>
              </div>

              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: 12, marginBottom: 12 }}>
                {/* Throughput */}
                <div style={{ background: '#0a0f1a', borderRadius: 8, padding: 10 }}>
                  <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', marginBottom: 6 }}>Throughput</div>
                  <div style={{ fontSize: 12, color: '#e0e6f0', lineHeight: 1.8 }}>
                    <div>Active IPs: <strong>{intel.throughput.active_ips}</strong></div>
                    <div>Max/day: <strong>{(intel.throughput.max_daily_capacity / 1000).toFixed(0)}k</strong></div>
                    <div>Max/hour: <strong>{(intel.throughput.max_hourly_rate).toLocaleString()}</strong></div>
                    <div>Audience: <strong>{(intel.throughput.audience_size).toLocaleString()}</strong></div>
                    <div>
                      {intel.throughput.can_send_in_one_pass
                        ? <span style={{ color: '#10b981' }}>Can send in 1 pass</span>
                        : <span style={{ color: '#ef4444' }}>~{intel.throughput.estimated_hours}h needed</span>}
                    </div>
                  </div>
                </div>

                {/* Warmup */}
                <div style={{ background: '#0a0f1a', borderRadius: 8, padding: 10 }}>
                  <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', marginBottom: 6 }}>Warmup</div>
                  <div style={{ fontSize: 12, color: '#e0e6f0', lineHeight: 1.8 }}>
                    <div>Total: <strong>{intel.warmup_summary.total_ips}</strong></div>
                    <div>Warmed: <strong style={{ color: '#10b981' }}>{intel.warmup_summary.warmed_ips}</strong></div>
                    <div>Warming: <strong style={{ color: '#f59e0b' }}>{intel.warmup_summary.warming_ips}</strong></div>
                    <div>Paused: <strong>{intel.warmup_summary.paused_ips}</strong></div>
                    <div>Daily limit: <strong>{(intel.warmup_summary.daily_limit / 1000).toFixed(0)}k</strong></div>
                  </div>
                </div>

                {/* Conviction */}
                <div style={{ background: '#0a0f1a', borderRadius: 8, padding: 10 }}>
                  <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)', marginBottom: 6 }}>
                    <FontAwesomeIcon icon={faBrain} /> Conviction Memory
                  </div>
                  <div style={{ fontSize: 12, color: '#e0e6f0', lineHeight: 1.8 }}>
                    <div>Verdict: <strong style={{ color: intel.conviction_summary.dominant_verdict === 'will' ? '#10b981' : '#ef4444' }}>
                      {intel.conviction_summary.dominant_verdict.toUpperCase()}
                    </strong></div>
                    <div>Confidence: <strong>{(intel.conviction_summary.confidence * 100).toFixed(0)}%</strong></div>
                    <div style={{ color: '#10b981' }}>WILL: {intel.conviction_summary.will_count}</div>
                    <div style={{ color: '#ef4444' }}>WONT: {intel.conviction_summary.wont_count}</div>
                  </div>
                </div>
              </div>

              {/* Risk factors */}
              {intel.conviction_summary.risk_factors && intel.conviction_summary.risk_factors.length > 0 && (
                <div style={{ marginBottom: 8 }}>
                  {intel.conviction_summary.risk_factors.map((rf, i) => (
                    <div key={i} style={{ fontSize: 11, color: '#f59e0b', padding: '2px 0' }}>
                      <FontAwesomeIcon icon={faExclamationTriangle} /> {rf}
                    </div>
                  ))}
                </div>
              )}

              {/* Active warnings */}
              {intel.active_warnings && intel.active_warnings.length > 0 && (
                <div style={{ padding: '8px 10px', background: '#ef444415', borderRadius: 6, marginBottom: 8 }}>
                  {intel.active_warnings.map((w, i) => (
                    <div key={i} style={{ fontSize: 11, color: '#ef4444' }}>
                      <FontAwesomeIcon icon={faExclamationTriangle} /> {w}
                    </div>
                  ))}
                </div>
              )}

              {/* Strategy */}
              <div style={{ padding: '8px 12px', background: 'rgba(0,200,255,0.06)', borderRadius: 8, fontSize: 12, color: '#00b0ff', borderLeft: '3px solid #00b0ff' }}>
                <FontAwesomeIcon icon={faShieldAlt} /> <strong>Strategy:</strong> {intel.strategy}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );

  const renderStep6 = () => (
    <div className="wiz-step-content ig-fade-in">
      <h3 style={{ margin: '0 0 16px' }}>Review + Deploy</h3>

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
            <label style={{ fontSize: 12, color: 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>Campaign Name</label>
            <input
              value={campaignName} placeholder="e.g. Q1 Gmail Warmup Blast"
              onChange={e => setCampaignName(e.target.value)}
              style={{ width: '100%', background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 8, color: '#e0e6f0', padding: '10px 12px', fontSize: 14, boxSizing: 'border-box' }}
            />
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
                    {recommendations.map((rec: any) => {
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
                            {(rec.windows || []).slice(0, 3).map((w: any, i: number) => (
                              <button
                                key={i}
                                onClick={() => {
                                  const now = new Date();
                                  const days = ['Sunday','Monday','Tuesday','Wednesday','Thursday','Friday','Saturday'];
                                  const targetDay = days.indexOf(w.day_of_week);
                                  const currentDay = now.getDay();
                                  let daysUntil = (targetDay - currentDay + 7) % 7;
                                  if (daysUntil === 0 && now.getHours() >= w.start_hour) daysUntil = 7;
                                  const target = new Date(now);
                                  target.setDate(target.getDate() + daysUntil);
                                  target.setHours(w.start_hour, 0, 0, 0);
                                  const pad = (n: number) => n.toString().padStart(2, '0');
                                  setScheduledAt(`${target.getFullYear()}-${pad(target.getMonth()+1)}-${pad(target.getDate())}T${pad(target.getHours())}:${pad(target.getMinutes())}`);
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
              <div>
                <label style={{ fontSize: 12, color: 'rgba(180,210,240,0.65)', display: 'block', marginBottom: 4 }}>Send Date & Time</label>
                <input
                  type="datetime-local"
                  value={scheduledAt}
                  onChange={e => setScheduledAt(e.target.value)}
                  min={new Date(Date.now() + 5 * 60 * 1000).toISOString().slice(0, 16)}
                  style={{ width: '100%', background: '#0a0f1a', border: '1px solid rgba(0,200,255,0.08)', borderRadius: 8, color: '#e0e6f0', padding: '10px 12px', fontSize: 14, boxSizing: 'border-box' }}
                />
              </div>
            </div>
          )}

          {/* Summary cards */}
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12, marginBottom: 16 }}>
            <SummaryCard title="Target ISPs" value={selectedISPs.map(i => ISP_META[i]?.label || i).join(', ')} />
            <SummaryCard title="Sending Domain" value={selectedDomain} />
            <SummaryCard title="Variants" value={`${variants.length} variant${variants.length > 1 ? 's' : ''} (${variants.map(v => `${v.variant_name}: ${v.split_percent}%`).join(', ')})`} />
            <SummaryCard title="Audience" value={audienceEstimate ? `${audienceEstimate.after_suppressions.toLocaleString()} recipients` : 'Not estimated'} />
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

          <button
            className="ig-btn-glow ig-ripple"
            onClick={handleDeploy}
            disabled={deploying || !campaignName.trim() || (sendMode === 'scheduled' && !scheduledAt)}
            style={{
              display: 'flex', alignItems: 'center', justifyContent: 'center', gap: 8,
              width: '100%', padding: '14px 0',
              background: deploying ? '#4b5563' : (sendMode === 'scheduled' ? '#f59e0b' : '#00b0ff'),
              color: '#fff', border: 'none', borderRadius: 10, fontSize: 15, fontWeight: 600,
              cursor: (deploying || (sendMode === 'scheduled' && !scheduledAt)) ? 'not-allowed' : 'pointer',
            }}
          >
            {deploying
              ? <><FontAwesomeIcon icon={faSpinner} spin /> Deploying...</>
              : sendMode === 'scheduled'
                ? <><FontAwesomeIcon icon={faRocket} /> Schedule Campaign</>
                : <><FontAwesomeIcon icon={faRocket} /> Deploy Now</>
            }
          </button>
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
        <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.65)' }}>Step {step} of {STEPS.length}</div>
      </div>

      {/* Step indicator */}
      <div className="ig-stagger" style={{ display: 'flex', padding: '12px 20px', gap: 4, borderBottom: '1px solid rgba(0,200,255,0.08)', background: '#0a1628', overflowX: 'auto' }}>
        {STEPS.map((s) => {
          const isActive = s.id === step;
          const isComplete = s.id < step;
          return (
            <button
              key={s.id}
              className={isActive ? 'ig-pulse-cyan' : undefined}
              onClick={() => { if (s.id < step) setStep(s.id); }}
              style={{
                display: 'flex', alignItems: 'center', gap: 6,
                padding: '6px 12px', borderRadius: 6, border: 'none',
                background: isActive ? 'rgba(0,200,255,0.12)' : 'transparent',
                color: isActive ? '#00b0ff' : isComplete ? '#10b981' : '#64748b',
                fontSize: 12, cursor: s.id < step ? 'pointer' : 'default',
                whiteSpace: 'nowrap', fontWeight: isActive ? 600 : 400,
              }}
            >
              <FontAwesomeIcon icon={isComplete ? faCheck : s.icon} />
              {s.label}
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
              onClick={() => setStep(Math.min(6, step + 1))}
              disabled={!canProceed()}
              style={{
                display: 'flex', alignItems: 'center', gap: 6,
                padding: '8px 18px', borderRadius: 8, border: 'none',
                background: canProceed() ? '#00b0ff' : '#4b5563',
                color: '#fff', fontSize: 13,
                cursor: canProceed() ? 'pointer' : 'not-allowed',
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
