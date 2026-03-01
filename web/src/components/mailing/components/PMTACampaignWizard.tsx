import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faArrowLeft, faArrowRight, faCheck, faServer, faGlobe,
  faPenFancy, faUsers, faBrain, faRocket, faSpinner,
  faExclamationTriangle, faCheckCircle, faTimesCircle,
  faPlus, faTimes, faChartBar, faShieldAlt,
} from '@fortawesome/free-solid-svg-icons';
import { useAuth } from '../../../contexts/AuthContext';

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

  const [step, setStep] = useState(1);
  const [loading, setLoading] = useState(false);

  // Step 1 state
  const [ispReadiness, setISPReadiness] = useState<ISPReadiness[]>([]);
  const [selectedISPs, setSelectedISPs] = useState<string[]>([]);

  // Step 2 state
  const [sendingDomains, setSendingDomains] = useState<SendingDomain[]>([]);
  const [selectedDomain, setSelectedDomain] = useState('');

  // Step 3 state
  const [variants, setVariants] = useState<ContentVariant[]>([
    { variant_name: 'A', from_name: '', subject: '', html_content: '', split_percent: 100 },
  ]);
  const [templates, setTemplates] = useState<any[]>([]);
  const [showTemplatePicker, setShowTemplatePicker] = useState(false);

  // Step 4 state
  const [lists, setLists] = useState<{ id: string; name: string; subscriber_count: number }[]>([]);
  const [segments, setSegments] = useState<{ id: string; name: string; cached_count: number }[]>([]);
  const [suppressionLists, setSuppressionLists] = useState<{ id: string; name: string; entry_count: number }[]>([]);
  const [selectedLists, setSelectedLists] = useState<string[]>([]);
  const [selectedSegments, setSelectedSegments] = useState<string[]>([]);
  const [selectedSuppLists, setSelectedSuppLists] = useState<string[]>([]);
  const [audienceEstimate, setAudienceEstimate] = useState<AudienceEstimate | null>(null);

  // Step 5 state
  const [ispIntel, setISPIntel] = useState<ISPIntel[]>([]);

  // Step 6 state
  const [campaignName, setCampaignName] = useState('');
  const [deploying, setDeploying] = useState(false);
  const [deployResult, setDeployResult] = useState<any>(null);

  // ── Data fetching ────────────────────────────────────────────────────────

  const fetchReadiness = useCallback(async () => {
    setLoading(true);
    try {
      const res = await orgFetch(`${API_BASE}/pmta-campaign/readiness`, orgId);
      const data = await res.json();
      setISPReadiness(data.isps || []);
    } catch { /* noop */ }
    setLoading(false);
  }, [orgId]);

  const fetchDomains = useCallback(async () => {
    try {
      const res = await orgFetch(`${API_BASE}/pmta-campaign/sending-domains`, orgId);
      const data = await res.json();
      setSendingDomains(data.domains || []);
    } catch { /* noop */ }
  }, [orgId]);

  const fetchAudienceData = useCallback(async () => {
    try {
      const [listRes, segRes, suppRes] = await Promise.all([
        orgFetch(`${API_BASE}/lists`, orgId),
        orgFetch(`${API_BASE}/segments`, orgId),
        orgFetch(`${API_BASE}/suppression-lists`, orgId),
      ]);
      const listData = await listRes.json();
      const segData = await segRes.json();
      const suppData = await suppRes.json();
      setLists(Array.isArray(listData) ? listData : listData.lists || []);
      setSegments(Array.isArray(segData) ? segData : segData.segments || []);
      setSuppressionLists(Array.isArray(suppData) ? suppData : suppData.lists || []);
    } catch { /* noop */ }
  }, [orgId]);

  const fetchAudienceEstimate = useCallback(async () => {
    if (selectedLists.length === 0 && selectedSegments.length === 0) {
      setAudienceEstimate(null);
      return;
    }
    try {
      const res = await orgFetch(`${API_BASE}/pmta-campaign/estimate-audience`, orgId, {
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
    } catch { /* noop */ }
  }, [orgId, selectedLists, selectedSegments, selectedSuppLists, selectedISPs]);

  const fetchIntel = useCallback(async () => {
    setLoading(true);
    try {
      const audiencePerISP: Record<string, number> = {};
      if (audienceEstimate?.isp_breakdown) {
        for (const [k, v] of Object.entries(audienceEstimate.isp_breakdown)) {
          audiencePerISP[k] = v;
        }
      }
      const res = await orgFetch(`${API_BASE}/pmta-campaign/intel`, orgId, {
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
    } catch { /* noop */ }
    setLoading(false);
  }, [orgId, selectedISPs, audienceEstimate]);

  const fetchTemplates = useCallback(async () => {
    try {
      const res = await fetch('/api/mailing/templates');
      const data = await res.json();
      setTemplates(data.templates || []);
    } catch { /* noop */ }
  }, []);

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

  // ── Step validation ──────────────────────────────────────────────────────

  const canProceed = (): boolean => {
    switch (step) {
      case 1: return selectedISPs.length > 0;
      case 2: return selectedDomain !== '';
      case 3: return variants.every(v => v.from_name && v.subject) &&
                     Math.abs(variants.reduce((s, v) => s + v.split_percent, 0) - 100) < 1;
      case 4: return selectedLists.length > 0 || selectedSegments.length > 0;
      case 5: return true;
      case 6: return campaignName.trim() !== '';
      default: return false;
    }
  };

  // ── Deploy ───────────────────────────────────────────────────────────────

  const handleDeploy = async () => {
    setDeploying(true);
    try {
      const res = await orgFetch(`${API_BASE}/pmta-campaign/deploy`, orgId, {
        method: 'POST',
        body: JSON.stringify({
          name: campaignName,
          target_isps: selectedISPs,
          sending_domain: selectedDomain,
          variants,
          inclusion_segments: selectedSegments,
          inclusion_lists: selectedLists,
          exclusion_lists: selectedSuppLists,
          send_days: [],
          send_hour: new Date().getUTCHours(),
          timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
          throttle_strategy: 'auto',
        }),
      });
      const data = await res.json();
      setDeployResult(data);
    } catch (err) {
      setDeployResult({ error: 'Deploy failed' });
    }
    setDeploying(false);
  };

  // ── Toggle helpers ───────────────────────────────────────────────────────

  const toggleISP = (isp: string) => {
    setSelectedISPs(prev => prev.includes(isp) ? prev.filter(i => i !== isp) : [...prev, isp]);
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
      from_name: '', subject: '', html_content: '',
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

  // ── Render helpers ───────────────────────────────────────────────────────

  const statusBadge = (status: string) => {
    const colors: Record<string, string> = { ready: '#10b981', caution: '#f59e0b', blocked: '#ef4444', green: '#10b981', yellow: '#f59e0b', red: '#ef4444', established: '#10b981', ramping: '#f59e0b', early: '#f97316' };
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
    <div className="wiz-step-content">
      <h3 style={{ margin: '0 0 4px' }}>Select Target ISPs</h3>
      <p style={{ margin: '0 0 16px', color: '#8b8fa3', fontSize: 13 }}>
        Choose which ISP ecosystems to target. Cards show live health from the governance engine.
      </p>
      {loading && <div style={{ textAlign: 'center', padding: 40, color: '#8b8fa3' }}><FontAwesomeIcon icon={faSpinner} spin /> Loading readiness data...</div>}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(280px, 1fr))', gap: 12 }}>
        {(ispReadiness.length > 0 ? ispReadiness : ALL_ISPS.map(isp => ({ isp, display_name: ISP_META[isp]?.label || isp, health_score: 0, status: 'unknown', active_agents: 0, total_agents: 6, bounce_rate: 0, deferral_rate: 0, complaint_rate: 0, warmup_ips: 0, active_ips: 0, quarantined_ips: 0, max_daily_capacity: 0, max_hourly_rate: 0, pool_name: '', has_emergency: false, warnings: [] }))).map((r: any) => {
          const meta = ISP_META[r.isp] || { label: r.display_name, color: '#64748b', emoji: '🌐' };
          const selected = selectedISPs.includes(r.isp);
          return (
            <div
              key={r.isp}
              onClick={() => toggleISP(r.isp)}
              style={{
                background: selected ? `${meta.color}15` : '#1e1f2e',
                border: `2px solid ${selected ? meta.color : '#2d2e3e'}`,
                borderRadius: 10, padding: 14, cursor: 'pointer',
                transition: 'all 0.2s',
              }}
            >
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8 }}>
                <span style={{ fontSize: 18 }}>{meta.emoji} <strong style={{ color: meta.color }}>{meta.label}</strong></span>
                {statusBadge(r.status)}
              </div>
              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '6px 16px', fontSize: 12, color: '#8b8fa3' }}>
                <span>Health: <strong style={{ color: '#e2e4ed' }}>{r.health_score.toFixed(0)}%</strong></span>
                <span>Agents: <strong style={{ color: '#e2e4ed' }}>{r.active_agents}/{r.total_agents}</strong></span>
                <span>Active IPs: <strong style={{ color: '#e2e4ed' }}>{r.active_ips}</strong></span>
                <span>Warmup IPs: <strong style={{ color: '#e2e4ed' }}>{r.warmup_ips}</strong></span>
                <span>Capacity: <strong style={{ color: '#e2e4ed' }}>{(r.max_daily_capacity / 1000).toFixed(0)}k/day</strong></span>
                <span>Bounce: <strong style={{ color: r.bounce_rate > 5 ? '#ef4444' : '#e2e4ed' }}>{r.bounce_rate.toFixed(1)}%</strong></span>
              </div>
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
    </div>
  );

  const renderStep2 = () => (
    <div className="wiz-step-content">
      <h3 style={{ margin: '0 0 4px' }}>Select Sending Domain</h3>
      <p style={{ margin: '0 0 16px', color: '#8b8fa3', fontSize: 13 }}>
        Choose the domain that will appear in the "From" address. Each domain shows DNS and IP pool info.
      </p>
      {sendingDomains.length === 0 && (
        <div style={{ textAlign: 'center', padding: 40, color: '#8b8fa3' }}>
          No sending domains configured. Add domains in Domain Center first.
        </div>
      )}
      <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
        {sendingDomains.map(d => (
          <div
            key={d.domain}
            onClick={() => setSelectedDomain(d.domain)}
            style={{
              background: selectedDomain === d.domain ? '#6366f115' : '#1e1f2e',
              border: `2px solid ${selectedDomain === d.domain ? '#6366f1' : '#2d2e3e'}`,
              borderRadius: 10, padding: 14, cursor: 'pointer',
              transition: 'all 0.2s',
            }}
          >
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8 }}>
              <span style={{ fontSize: 15, fontWeight: 600, color: '#e2e4ed' }}>{d.domain}</span>
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
              <span style={{ color: '#8b8fa3' }}>Pool: {d.pool_name}</span>
              <span style={{ color: '#8b8fa3' }}>IPs: {d.active_ips} active / {d.warmup_ips} warmup</span>
              <span style={{ color: '#8b8fa3' }}>Rep: {d.reputation_score.toFixed(0)}%</span>
            </div>
          </div>
        ))}
      </div>
    </div>
  );

  const loadTemplate = (tpl: any, variantIdx: number) => {
    updateVariant(variantIdx, 'subject', tpl.subject || '');
    updateVariant(variantIdx, 'html_content', tpl.html_content || '');
    if (tpl.from_name) updateVariant(variantIdx, 'from_name', tpl.from_name);
    setShowTemplatePicker(false);
  };

  const renderStep3 = () => (
    <div className="wiz-step-content">
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
        <div>
          <h3 style={{ margin: 0 }}>Content + A/B Split Testing</h3>
          <p style={{ margin: '4px 0 0', color: '#8b8fa3', fontSize: 13 }}>Configure from-names, subject lines, and content. Add variants for A/B testing.</p>
        </div>
        <div style={{ display: 'flex', gap: 8 }}>
          <button onClick={() => setShowTemplatePicker(!showTemplatePicker)} style={{ display: 'flex', alignItems: 'center', gap: 6, background: '#1e1f2e', color: '#a78bfa', border: '1px solid #2d2e3e', borderRadius: 8, padding: '8px 14px', fontSize: 13, cursor: 'pointer' }}>
            <FontAwesomeIcon icon={faPenFancy} /> Load Template
          </button>
          {variants.length < 4 && (
            <button onClick={addVariant} style={{ display: 'flex', alignItems: 'center', gap: 6, background: '#6366f1', color: '#fff', border: 'none', borderRadius: 8, padding: '8px 14px', fontSize: 13, cursor: 'pointer' }}>
              <FontAwesomeIcon icon={faPlus} /> Add Variant
            </button>
          )}
        </div>
      </div>

      {showTemplatePicker && (
        <div style={{ background: '#1e1f2e', border: '1px solid #6366f1', borderRadius: 10, padding: 16, marginBottom: 16 }}>
          <h4 style={{ margin: '0 0 12px', color: '#a78bfa', fontSize: 14 }}>Content Library — Select a Template</h4>
          {templates.length === 0 ? (
            <p style={{ color: '#8b8fa3', fontSize: 13 }}>No templates saved yet. Create templates in the Content Library tab.</p>
          ) : (
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(220px, 1fr))', gap: 10 }}>
              {templates.map((tpl: any) => (
                <div key={tpl.id} style={{ background: '#14151f', border: '1px solid #2d2e3e', borderRadius: 8, padding: 12, cursor: 'pointer', transition: 'border-color 0.2s' }}
                  onMouseEnter={e => (e.currentTarget.style.borderColor = '#6366f1')}
                  onMouseLeave={e => (e.currentTarget.style.borderColor = '#2d2e3e')}>
                  <strong style={{ color: '#e2e4ed', fontSize: 13, display: 'block', marginBottom: 4 }}>{tpl.name}</strong>
                  <span style={{ color: '#8b8fa3', fontSize: 12 }}>{tpl.subject || tpl.description || 'No subject'}</span>
                  <div style={{ marginTop: 8, display: 'flex', gap: 6 }}>
                    {variants.map((v, idx) => (
                      <button key={idx} onClick={() => loadTemplate(tpl, idx)}
                        style={{ fontSize: 11, background: '#6366f1', color: '#fff', border: 'none', borderRadius: 4, padding: '4px 8px', cursor: 'pointer' }}>
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

      {variants.map((v, idx) => (
        <div key={idx} style={{ background: '#1e1f2e', border: '1px solid #2d2e3e', borderRadius: 10, padding: 16, marginBottom: 12, position: 'relative' }}>
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 }}>
            <span style={{ fontSize: 14, fontWeight: 600, color: '#a78bfa' }}>Variant {v.variant_name}</span>
            <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
              <label style={{ fontSize: 12, color: '#8b8fa3', display: 'flex', alignItems: 'center', gap: 4 }}>
                Split:
                <input
                  type="number" min={1} max={100} value={v.split_percent}
                  onChange={e => updateVariant(idx, 'split_percent', Number(e.target.value))}
                  style={{ width: 50, background: '#14151f', border: '1px solid #2d2e3e', borderRadius: 6, color: '#e2e4ed', padding: '4px 6px', fontSize: 12, textAlign: 'center' }}
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
              <label style={{ fontSize: 11, color: '#8b8fa3', display: 'block', marginBottom: 4 }}>From Name</label>
              <input
                value={v.from_name} placeholder="e.g. Jarvis Team"
                onChange={e => updateVariant(idx, 'from_name', e.target.value)}
                style={{ width: '100%', background: '#14151f', border: '1px solid #2d2e3e', borderRadius: 6, color: '#e2e4ed', padding: '8px 10px', fontSize: 13, boxSizing: 'border-box' }}
              />
            </div>
            <div>
              <label style={{ fontSize: 11, color: '#8b8fa3', display: 'block', marginBottom: 4 }}>Subject Line <span style={{ color: '#64748b' }}>({v.subject.length} chars)</span></label>
              <input
                value={v.subject} placeholder="e.g. Don't miss this deal"
                onChange={e => updateVariant(idx, 'subject', e.target.value)}
                style={{ width: '100%', background: '#14151f', border: '1px solid #2d2e3e', borderRadius: 6, color: '#e2e4ed', padding: '8px 10px', fontSize: 13, boxSizing: 'border-box' }}
              />
            </div>
          </div>
          <div>
            <label style={{ fontSize: 11, color: '#8b8fa3', display: 'block', marginBottom: 4 }}>HTML Content</label>
            <textarea
              value={v.html_content} rows={5} placeholder="<html>..."
              onChange={e => updateVariant(idx, 'html_content', e.target.value)}
              style={{ width: '100%', background: '#14151f', border: '1px solid #2d2e3e', borderRadius: 6, color: '#e2e4ed', padding: '8px 10px', fontSize: 12, fontFamily: 'monospace', resize: 'vertical', boxSizing: 'border-box' }}
            />
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
    <div className="wiz-step-content">
      <h3 style={{ margin: '0 0 4px' }}>Audience + Suppression</h3>
      <p style={{ margin: '0 0 16px', color: '#8b8fa3', fontSize: 13 }}>
        Select inclusion lists/segments and exclusion suppression lists.
      </p>

      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16, marginBottom: 16 }}>
        {/* Inclusion */}
        <div>
          <h4 style={{ margin: '0 0 8px', fontSize: 13, color: '#10b981' }}>
            <FontAwesomeIcon icon={faCheckCircle} /> Inclusion
          </h4>
          <div style={{ background: '#1e1f2e', border: '1px solid #2d2e3e', borderRadius: 8, padding: 10, maxHeight: 200, overflowY: 'auto' }}>
            {lists.length === 0 && segments.length === 0 && (
              <div style={{ color: '#64748b', fontSize: 12, padding: 10 }}>No lists or segments available.</div>
            )}
            {lists.map(l => (
              <label key={`list-${l.id}`} style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '6px 4px', cursor: 'pointer', fontSize: 12, color: '#e2e4ed', borderBottom: '1px solid #1a1b2e' }}>
                <input type="checkbox" checked={selectedLists.includes(l.id)} onChange={() => toggleList(l.id)} />
                <span style={{ flex: 1 }}>{l.name}</span>
                <span style={{ color: '#8b8fa3' }}>{(l.subscriber_count || 0).toLocaleString()}</span>
              </label>
            ))}
            {segments.map(s => (
              <label key={`seg-${s.id}`} style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '6px 4px', cursor: 'pointer', fontSize: 12, color: '#a78bfa', borderBottom: '1px solid #1a1b2e' }}>
                <input type="checkbox" checked={selectedSegments.includes(s.id)} onChange={() => toggleSegment(s.id)} />
                <span style={{ flex: 1 }}>{s.name}</span>
                <span style={{ color: '#8b8fa3' }}>{(s.cached_count || 0).toLocaleString()}</span>
              </label>
            ))}
          </div>
        </div>

        {/* Exclusion */}
        <div>
          <h4 style={{ margin: '0 0 8px', fontSize: 13, color: '#ef4444' }}>
            <FontAwesomeIcon icon={faTimesCircle} /> Suppression
          </h4>
          <div style={{ background: '#1e1f2e', border: '1px solid #2d2e3e', borderRadius: 8, padding: 10, maxHeight: 200, overflowY: 'auto' }}>
            {suppressionLists.length === 0 && (
              <div style={{ color: '#64748b', fontSize: 12, padding: 10 }}>No suppression lists available.</div>
            )}
            {suppressionLists.map(sl => (
              <label key={`supp-${sl.id}`} style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '6px 4px', cursor: 'pointer', fontSize: 12, color: '#e2e4ed', borderBottom: '1px solid #1a1b2e' }}>
                <input type="checkbox" checked={selectedSuppLists.includes(sl.id)} onChange={() => toggleSuppList(sl.id)} />
                <span style={{ flex: 1 }}>{sl.name}</span>
                <span style={{ color: '#8b8fa3' }}>{(sl.entry_count || 0).toLocaleString()}</span>
              </label>
            ))}
          </div>
        </div>
      </div>

      {/* Audience estimate */}
      {audienceEstimate && (
        <div style={{ background: '#1e1f2e', border: '1px solid #2d2e3e', borderRadius: 10, padding: 14 }}>
          <h4 style={{ margin: '0 0 10px', fontSize: 13, color: '#e2e4ed' }}>
            <FontAwesomeIcon icon={faChartBar} /> Audience Estimate
          </h4>
          <div style={{ display: 'flex', gap: 20, fontSize: 13, marginBottom: 12, flexWrap: 'wrap' }}>
            <span style={{ color: '#8b8fa3' }}>Total: <strong style={{ color: '#e2e4ed' }}>{audienceEstimate.total_recipients.toLocaleString()}</strong></span>
            <span style={{ color: '#8b8fa3' }}>Suppressed: <strong style={{ color: '#ef4444' }}>-{audienceEstimate.suppressed_count.toLocaleString()}</strong></span>
            <span style={{ color: '#8b8fa3' }}>Net: <strong style={{ color: '#10b981' }}>{audienceEstimate.after_suppressions.toLocaleString()}</strong></span>
          </div>
          {audienceEstimate.isp_breakdown && Object.keys(audienceEstimate.isp_breakdown).length > 0 && (
            <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
              {Object.entries(audienceEstimate.isp_breakdown).map(([isp, count]) => {
                const meta = ISP_META[isp];
                return (
                  <span key={isp} style={{ display: 'inline-flex', alignItems: 'center', gap: 4, padding: '3px 8px', borderRadius: 6, fontSize: 11, background: (meta?.color || '#64748b') + '15', color: meta?.color || '#8b8fa3', border: `1px solid ${(meta?.color || '#64748b')}33` }}>
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
    <div className="wiz-step-content">
      <h3 style={{ margin: '0 0 4px' }}>Infrastructure Intelligence</h3>
      <p style={{ margin: '0 0 16px', color: '#8b8fa3', fontSize: 13 }}>
        Live state of the targeted ecosystem — throughput, warmup, conviction insights, and active warnings.
      </p>
      {loading && <div style={{ textAlign: 'center', padding: 40, color: '#8b8fa3' }}><FontAwesomeIcon icon={faSpinner} spin /> Querying governance engine...</div>}
      <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
        {ispIntel.map(intel => {
          const meta = ISP_META[intel.isp] || { label: intel.display_name, color: '#64748b', emoji: '🌐' };
          return (
            <div key={intel.isp} style={{ background: '#1e1f2e', border: '1px solid #2d2e3e', borderRadius: 10, padding: 16 }}>
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 }}>
                <span style={{ fontSize: 15, fontWeight: 600, color: meta.color }}>{meta.emoji} {meta.label}</span>
                <div style={{ display: 'flex', gap: 6 }}>
                  {statusBadge(intel.throughput.status)}
                  {statusBadge(intel.warmup_summary.status)}
                </div>
              </div>

              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: 12, marginBottom: 12 }}>
                {/* Throughput */}
                <div style={{ background: '#14151f', borderRadius: 8, padding: 10 }}>
                  <div style={{ fontSize: 11, color: '#8b8fa3', marginBottom: 6 }}>Throughput</div>
                  <div style={{ fontSize: 12, color: '#e2e4ed', lineHeight: 1.8 }}>
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
                <div style={{ background: '#14151f', borderRadius: 8, padding: 10 }}>
                  <div style={{ fontSize: 11, color: '#8b8fa3', marginBottom: 6 }}>Warmup</div>
                  <div style={{ fontSize: 12, color: '#e2e4ed', lineHeight: 1.8 }}>
                    <div>Total: <strong>{intel.warmup_summary.total_ips}</strong></div>
                    <div>Warmed: <strong style={{ color: '#10b981' }}>{intel.warmup_summary.warmed_ips}</strong></div>
                    <div>Warming: <strong style={{ color: '#f59e0b' }}>{intel.warmup_summary.warming_ips}</strong></div>
                    <div>Paused: <strong>{intel.warmup_summary.paused_ips}</strong></div>
                    <div>Daily limit: <strong>{(intel.warmup_summary.daily_limit / 1000).toFixed(0)}k</strong></div>
                  </div>
                </div>

                {/* Conviction */}
                <div style={{ background: '#14151f', borderRadius: 8, padding: 10 }}>
                  <div style={{ fontSize: 11, color: '#8b8fa3', marginBottom: 6 }}>
                    <FontAwesomeIcon icon={faBrain} /> Conviction Memory
                  </div>
                  <div style={{ fontSize: 12, color: '#e2e4ed', lineHeight: 1.8 }}>
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
              <div style={{ padding: '8px 12px', background: '#6366f110', borderRadius: 8, fontSize: 12, color: '#a78bfa', borderLeft: '3px solid #6366f1' }}>
                <FontAwesomeIcon icon={faShieldAlt} /> <strong>Strategy:</strong> {intel.strategy}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );

  const renderStep6 = () => (
    <div className="wiz-step-content">
      <h3 style={{ margin: '0 0 16px' }}>Review + Deploy</h3>

      {deployResult ? (
        <div style={{ textAlign: 'center', padding: 40 }}>
          {deployResult.error ? (
            <div style={{ color: '#ef4444' }}>
              <FontAwesomeIcon icon={faTimesCircle} size="3x" style={{ marginBottom: 12 }} />
              <h3>Deploy Failed</h3>
              <p>{deployResult.error}</p>
            </div>
          ) : (
            <div style={{ color: '#10b981' }}>
              <FontAwesomeIcon icon={faCheckCircle} size="3x" style={{ marginBottom: 12 }} />
              <h3>Campaign Created</h3>
              <p>ID: {deployResult.campaign_id}</p>
              <p>{deployResult.variant_count} variant{deployResult.variant_count > 1 ? 's' : ''} targeting {deployResult.target_isps?.length} ISP{deployResult.target_isps?.length > 1 ? 's' : ''}</p>
              <button onClick={onClose} style={{ marginTop: 16, background: '#6366f1', color: '#fff', border: 'none', borderRadius: 8, padding: '10px 24px', fontSize: 14, cursor: 'pointer' }}>
                Done
              </button>
            </div>
          )}
        </div>
      ) : (
        <>
          {/* Campaign name */}
          <div style={{ marginBottom: 16 }}>
            <label style={{ fontSize: 12, color: '#8b8fa3', display: 'block', marginBottom: 4 }}>Campaign Name</label>
            <input
              value={campaignName} placeholder="e.g. Q1 Gmail Warmup Blast"
              onChange={e => setCampaignName(e.target.value)}
              style={{ width: '100%', background: '#14151f', border: '1px solid #2d2e3e', borderRadius: 8, color: '#e2e4ed', padding: '10px 12px', fontSize: 14, boxSizing: 'border-box' }}
            />
          </div>

          {/* Summary cards */}
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12, marginBottom: 16 }}>
            <SummaryCard title="Target ISPs" value={selectedISPs.map(i => ISP_META[i]?.label || i).join(', ')} />
            <SummaryCard title="Sending Domain" value={selectedDomain} />
            <SummaryCard title="Variants" value={`${variants.length} variant${variants.length > 1 ? 's' : ''} (${variants.map(v => `${v.variant_name}: ${v.split_percent}%`).join(', ')})`} />
            <SummaryCard title="Audience" value={audienceEstimate ? `${audienceEstimate.after_suppressions.toLocaleString()} recipients` : 'Not estimated'} />
            <SummaryCard title="From Names" value={variants.map(v => v.from_name).filter(Boolean).join(' / ') || '—'} />
            <SummaryCard title="Subject Lines" value={variants.map(v => v.subject).filter(Boolean).join(' / ') || '—'} />
          </div>

          <button
            onClick={handleDeploy}
            disabled={deploying || !campaignName.trim()}
            style={{
              display: 'flex', alignItems: 'center', justifyContent: 'center', gap: 8,
              width: '100%', padding: '14px 0', background: deploying ? '#4b5563' : '#6366f1',
              color: '#fff', border: 'none', borderRadius: 10, fontSize: 15, fontWeight: 600,
              cursor: deploying ? 'not-allowed' : 'pointer',
            }}
          >
            {deploying ? <><FontAwesomeIcon icon={faSpinner} spin /> Deploying...</> : <><FontAwesomeIcon icon={faRocket} /> Deploy Campaign</>}
          </button>
        </>
      )}
    </div>
  );

  const SummaryCard: React.FC<{ title: string; value: string }> = ({ title, value }) => (
    <div style={{ background: '#1e1f2e', border: '1px solid #2d2e3e', borderRadius: 8, padding: 12 }}>
      <div style={{ fontSize: 11, color: '#8b8fa3', marginBottom: 4 }}>{title}</div>
      <div style={{ fontSize: 13, color: '#e2e4ed', wordBreak: 'break-word' }}>{value || '—'}</div>
    </div>
  );

  // ── Render ─────────────────────────────────────────────────────────────────

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', background: '#14151f', color: '#e2e4ed' }}>
      {/* Header */}
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '14px 20px', borderBottom: '1px solid #2d2e3e', background: '#1a1b2e' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          {onClose && (
            <button onClick={onClose} style={{ background: 'none', border: 'none', color: '#8b8fa3', cursor: 'pointer', fontSize: 14 }}>
              <FontAwesomeIcon icon={faArrowLeft} />
            </button>
          )}
          <h2 style={{ margin: 0, fontSize: 16, fontWeight: 700, letterSpacing: 1 }}>PMTA Campaign Wizard</h2>
        </div>
        <div style={{ fontSize: 12, color: '#8b8fa3' }}>Step {step} of {STEPS.length}</div>
      </div>

      {/* Step indicator */}
      <div style={{ display: 'flex', padding: '12px 20px', gap: 4, borderBottom: '1px solid #2d2e3e', background: '#1a1b2e', overflowX: 'auto' }}>
        {STEPS.map((s) => {
          const isActive = s.id === step;
          const isComplete = s.id < step;
          return (
            <button
              key={s.id}
              onClick={() => { if (s.id < step) setStep(s.id); }}
              style={{
                display: 'flex', alignItems: 'center', gap: 6,
                padding: '6px 12px', borderRadius: 6, border: 'none',
                background: isActive ? '#6366f120' : 'transparent',
                color: isActive ? '#a78bfa' : isComplete ? '#10b981' : '#64748b',
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
        <div style={{ display: 'flex', justifyContent: 'space-between', padding: '12px 20px', borderTop: '1px solid #2d2e3e', background: '#1a1b2e' }}>
          <button
            onClick={() => setStep(Math.max(1, step - 1))}
            disabled={step === 1}
            style={{
              display: 'flex', alignItems: 'center', gap: 6,
              padding: '8px 18px', borderRadius: 8, border: '1px solid #2d2e3e',
              background: 'transparent', color: step === 1 ? '#4b5563' : '#e2e4ed',
              fontSize: 13, cursor: step === 1 ? 'default' : 'pointer',
            }}
          >
            <FontAwesomeIcon icon={faArrowLeft} /> Back
          </button>
          {step < 6 && (
            <button
              onClick={() => setStep(Math.min(6, step + 1))}
              disabled={!canProceed()}
              style={{
                display: 'flex', alignItems: 'center', gap: 6,
                padding: '8px 18px', borderRadius: 8, border: 'none',
                background: canProceed() ? '#6366f1' : '#4b5563',
                color: '#fff', fontSize: 13,
                cursor: canProceed() ? 'pointer' : 'not-allowed',
              }}
            >
              Next <FontAwesomeIcon icon={faArrowRight} />
            </button>
          )}
        </div>
      )}
    </div>
  );
};
