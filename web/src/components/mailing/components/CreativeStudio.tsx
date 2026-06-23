import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faWandMagicSparkles,
  faRotate,
  faCircleCheck,
  faTriangleExclamation,
  faPaperPlane,
  faFloppyDisk,
  faBookOpen,
  faHammer,
  faRobot,
  faStamp,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import { useToast } from '../shared/ToastSystem';
import { OfferProofs } from './OfferProofs';

// Creative Studio v2 — the ReviewForge email builder, centralized in the
// portal. Builder renders through the engine sidecar (byte-identical to the
// operator's local ReviewForge); saves land in the Content Library
// ("Creative Studio/<SITE>") + the mailing_creatives registry; the docked
// agent generates conversationally (replaces the Cursor loop).
// Multi-brand: pick several sites, generate once, tab across the results.

const SITE_KEYS = [
  'db', 'mh', 'qf', 'ht', 'bwp', 'fc', 'cp', 'hws',
  'rru', 'tot', 'yih', 'mrd', 'ci', 'lpl', 'rb', 'wfy',
  'mpf', 'pmd', 'trb',
];

interface StudioBrand {
  brandKey: string;
  name: string;
  category?: string;
}

interface GenerateResult {
  site_key: string;
  html: string;
  subject: string;
  preheader: string;
  filename?: string;
  template_id?: string;
  creative_id?: string;
  money_urls: number;
  saved: boolean;
  error?: string;
}

interface CreativeMeta {
  id: string;
  offer_key: string;
  brand_code: string;
  filename: string;
  subject: string;
  preheader: string;
  money_urls: number;
  tagged: boolean;
  source: string;
  generated_at: string;
  html_bytes: number;
  // Approval + money-link spot-check (joined onto the registry row by the API).
  approval_status?: 'pending' | 'approved' | 'rejected';
  approved_by?: string;
  approved_at?: string | null;
  money_link_status?: 'untested' | 'pass' | 'warn' | 'dead';
  money_link_checked_at?: string | null;
  money_link_detail?: string;
}

type CreativeAction = 'check' | 'proof' | 'approve' | 'reject';

interface MoneyLinkFinding {
  level: string;
  code: string;
  detail: string;
}

interface MoneyLinkCheckResponse {
  money_link_status: CreativeMeta['money_link_status'];
  detail?: string;
  findings?: MoneyLinkFinding[];
}

interface SendProofResponse {
  status: 'sent' | 'error';
  message_id?: string;
  error?: string;
}

interface ChatMsg {
  role: 'user' | 'assistant';
  content: string;
  actions?: string[];
}

const inputStyle: React.CSSProperties = {
  background: '#1e293b', border: '1px solid #334155', borderRadius: 6,
  color: '#e5e7eb', padding: '7px 10px', fontSize: 13, width: '100%',
  boxSizing: 'border-box',
};

const labelStyle: React.CSSProperties = {
  fontSize: 11, color: '#94a3b8', textTransform: 'uppercase',
  letterSpacing: 0.5, marginBottom: 4, display: 'block',
};

const btnStyle = (bg: string, border: string): React.CSSProperties => ({
  background: bg, border: `1px solid ${border}`, borderRadius: 6,
  color: '#e5e7eb', padding: '8px 14px', fontSize: 13, cursor: 'pointer',
  display: 'inline-flex', alignItems: 'center', gap: 6, fontWeight: 600,
});

const chipStyle = (active: boolean): React.CSSProperties => ({
  background: active ? '#312e81' : '#1e293b',
  border: `1px solid ${active ? '#6366f1' : '#334155'}`,
  borderRadius: 999, color: active ? '#e0e7ff' : '#94a3b8',
  padding: '3px 10px', fontSize: 12, cursor: 'pointer', fontWeight: 600,
});

const badgeStyle = (fg: string): React.CSSProperties => ({
  display: 'inline-block', padding: '2px 8px', borderRadius: 999,
  fontSize: 11, fontWeight: 600, lineHeight: 1.4,
  color: fg, background: `${fg}1f`, border: `1px solid ${fg}55`,
  whiteSpace: 'nowrap',
});

function approvalColor(status: CreativeMeta['approval_status']): string {
  if (status === 'approved') return '#00b894';
  if (status === 'rejected') return '#e94560';
  return '#fdcb6e'; // pending / undefined
}

function moneyLinkColor(status: CreativeMeta['money_link_status']): string {
  if (status === 'pass') return '#00b894';
  if (status === 'warn') return '#f59e0b';
  if (status === 'dead') return '#ef4444';
  return '#94a3b8'; // untested / undefined
}

const smallBtnStyle = (bg: string, border: string): React.CSSProperties => ({
  background: bg, border: `1px solid ${border}`, borderRadius: 5,
  color: '#e5e7eb', padding: '3px 8px', fontSize: 11, cursor: 'pointer',
  fontWeight: 600, whiteSpace: 'nowrap',
});

// Minimal markdown for agent replies (bold, inline code, line breaks).
function renderAgentText(text: string): React.ReactNode {
  return text.split('\n').map((line, i) => {
    const parts = line.split(/(\*\*[^*]+\*\*|`[^`]+`)/g).map((seg, j) => {
      if (seg.startsWith('**') && seg.endsWith('**')) {
        return <strong key={j}>{seg.slice(2, -2)}</strong>;
      }
      if (seg.startsWith('`') && seg.endsWith('`')) {
        return (
          <code key={j} style={{ background: '#0f172a', padding: '1px 5px', borderRadius: 4, fontSize: 12 }}>
            {seg.slice(1, -1)}
          </code>
        );
      }
      return seg;
    });
    return <div key={i} style={{ minHeight: line ? undefined : 8 }}>{parts}</div>;
  });
}

export const CreativeStudio: React.FC = () => {
  const { addToast } = useToast();
  const [engineUp, setEngineUp] = useState<boolean | null>(null);
  const [brands, setBrands] = useState<StudioBrand[]>([]);
  const [view, setView] = useState<'builder' | 'library' | 'offers'>('builder');

  // Builder state
  const [mode, setMode] = useState<'newsletter' | 'solo'>('newsletter');
  const [selectedSites, setSelectedSites] = useState<string[]>(['db']);
  const [primaryBrandKey, setPrimaryBrandKey] = useState('');
  const [secondaryBrandKey, setSecondaryBrandKey] = useState('');
  const [subjectLine, setSubjectLine] = useState('');
  const [preheader, setPreheader] = useState('');
  const [refreshContent, setRefreshContent] = useState(true);
  // Imagery / copy overrides — shared by newsletter and solo (operator swaps
  // imagery per send often).
  const [bannerUrl, setBannerUrl] = useState('');
  const [logoUrl, setLogoUrl] = useState('');
  const [imageOrientation, setImageOrientation] = useState<'' | 'horizontal' | 'vertical' | 'logo'>('');
  const [titleOverride, setTitleOverride] = useState('');
  const [subtitleOverride, setSubtitleOverride] = useState('');
  const [ctaLabel, setCtaLabel] = useState('');
  const [ctaUrl, setCtaUrl] = useState('');
  const [soloCreativeUrl, setSoloCreativeUrl] = useState('');
  const [soloBelowMode, setSoloBelowMode] = useState<'review_card' | 'full_review' | 'none'>('review_card');
  const [generating, setGenerating] = useState(false);
  const [results, setResults] = useState<GenerateResult[]>([]);
  const [activeResultSite, setActiveResultSite] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  // Library state
  const [creatives, setCreatives] = useState<CreativeMeta[]>([]);
  const [libraryLoading, setLibraryLoading] = useState(false);
  const [selected, setSelected] = useState<CreativeMeta | null>(null);
  const [previewHtml, setPreviewHtml] = useState<string | null>(null);
  // Approval / money-link spot-check controls.
  const [proofEmail, setProofEmail] = useState('');
  // Per-creative in-flight state, keyed "<id>:<action>".
  const [inFlight, setInFlight] = useState<Record<string, boolean>>({});
  // Which creative has its inline approve-confirm armed.
  const [approveArmed, setApproveArmed] = useState<string | null>(null);

  // Agent state
  const [chatOpen, setChatOpen] = useState(true);
  const [messages, setMessages] = useState<ChatMsg[]>([]);
  const [chatInput, setChatInput] = useState('');
  const [chatBusy, setChatBusy] = useState(false);
  const [conversationId, setConversationId] = useState<string | null>(null);
  const chatBottomRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    (async () => {
      try {
        const st = await apiFetch('/api/mailing/creative-studio/status', { credentials: 'include' });
        const stJson = await st.json();
        setEngineUp(Boolean(stJson.engine_up));
      } catch {
        setEngineUp(false);
      }
      try {
        const br = await apiFetch('/api/mailing/creative-studio/brands', { credentials: 'include' });
        if (br.ok) {
          const json = await br.json();
          const list: StudioBrand[] = (json.brands ?? []).filter(
            (b: StudioBrand) => !b.brandKey.startsWith('custom-test') && b.name !== '(deleted)',
          );
          setBrands(list);
          if (list.length && !primaryBrandKey) setPrimaryBrandKey(list[0].brandKey);
        }
      } catch {
        /* engine down — banner covers it */
      }
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const fetchLibrary = useCallback(async () => {
    setLibraryLoading(true);
    try {
      const res = await apiFetch('/api/mailing/creatives/?limit=100', { credentials: 'include' });
      if (res.ok) {
        const json = await res.json();
        setCreatives(json.creatives ?? []);
      }
    } finally {
      setLibraryLoading(false);
    }
  }, []);

  useEffect(() => {
    if (view === 'library') fetchLibrary();
  }, [view, fetchLibrary]);

  // Keep the right-hand preview's selected row in sync with refreshed metadata
  // (approval / money-link status) without dropping the loaded previewHtml.
  useEffect(() => {
    if (!selected) return;
    const fresh = creatives.find((c) => c.id === selected.id);
    if (fresh && fresh !== selected) setSelected(fresh);
  }, [creatives, selected]);

  useEffect(() => {
    chatBottomRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, [messages, chatBusy]);

  const toggleSite = useCallback((s: string) => {
    setSelectedSites((cur) => (cur.includes(s) ? cur.filter((x) => x !== s) : [...cur, s]));
  }, []);

  const generate = useCallback(async (save: boolean) => {
    if (selectedSites.length === 0) {
      setError('pick at least one site');
      return;
    }
    setGenerating(true);
    setError(null);
    try {
      const overrides: Record<string, unknown> = {};
      if (bannerUrl) overrides.bannerUrl = bannerUrl;
      if (logoUrl) overrides.logoUrl = logoUrl;
      if (imageOrientation) overrides.imageOrientation = imageOrientation;
      if (titleOverride) overrides.title = titleOverride;
      if (subtitleOverride) overrides.subtitle = subtitleOverride;
      if (ctaLabel) overrides.ctaLabel = ctaLabel;
      if (ctaUrl) overrides.ctaUrl = ctaUrl;

      const body: Record<string, unknown> = {
        site_keys: selectedSites,
        primary_brand_key: primaryBrandKey,
        secondary_brand_key: secondaryBrandKey || undefined,
        subject_line: subjectLine || undefined,
        preheader: preheader || undefined,
        refresh_content: refreshContent,
        mode,
        save,
      };
      if (Object.keys(overrides).length > 0) body.primary_overrides = overrides;
      if (mode === 'solo') {
        body.solo = {
          creativeUrl: soloCreativeUrl,
          headline: titleOverride || undefined,
          subheadline: subtitleOverride || undefined,
          ctaLabel: ctaLabel || undefined,
          ctaUrl: ctaUrl || undefined,
          logoUrl: logoUrl || undefined,
          belowMode: soloBelowMode,
        };
      }
      const res = await apiFetch('/api/mailing/creative-studio/generate', {
        method: 'POST',
        body: JSON.stringify(body),
        credentials: 'include',
      });
      const json = await res.json();
      if (!res.ok) throw new Error(json.error || `HTTP ${res.status}`);
      const out: GenerateResult[] = json.results ?? [];
      setResults(out);
      const firstOk = out.find((r) => !r.error);
      setActiveResultSite((firstOk ?? out[0])?.site_key ?? null);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setGenerating(false);
    }
  }, [selectedSites, primaryBrandKey, secondaryBrandKey, subjectLine, preheader,
      refreshContent, mode, bannerUrl, logoUrl, imageOrientation, titleOverride,
      subtitleOverride, ctaLabel, ctaUrl, soloCreativeUrl, soloBelowMode]);

  const openPreview = useCallback(async (c: CreativeMeta) => {
    setSelected(c);
    setPreviewHtml(null);
    try {
      const res = await apiFetch(`/api/mailing/creatives/${c.id}/preview`, { credentials: 'include' });
      setPreviewHtml(res.ok ? await res.text() : `<p>preview failed: HTTP ${res.status}</p>`);
    } catch (err) {
      setPreviewHtml(`<p>preview failed: ${err instanceof Error ? err.message : String(err)}</p>`);
    }
  }, []);

  const flightKey = (id: string, action: CreativeAction) => `${id}:${action}`;
  const isInFlight = useCallback(
    (id: string, action: CreativeAction) => Boolean(inFlight[flightKey(id, action)]),
    [inFlight],
  );
  const setFlight = useCallback((id: string, action: CreativeAction, on: boolean) => {
    setInFlight((cur) => ({ ...cur, [flightKey(id, action)]: on }));
  }, []);

  // Run the money-link spot-check. Toasts the verdict; a `dead` result raises an
  // error toast carrying the detail (the in-app "spot-check" alert).
  const checkMoneyLink = useCallback(async (c: CreativeMeta) => {
    if (isInFlight(c.id, 'check')) return;
    setFlight(c.id, 'check', true);
    try {
      const res = await apiFetch(`/api/mailing/creatives/${c.id}/money-link-check`, {
        method: 'POST',
        body: JSON.stringify({ http: true }),
        credentials: 'include',
      });
      const json: MoneyLinkCheckResponse & { error?: string } = await res.json();
      if (!res.ok) throw new Error(json.error || `HTTP ${res.status}`);
      const status = json.money_link_status;
      const detail = json.detail || '';
      if (status === 'dead') {
        addToast({ type: 'error', title: `Tracking link DEAD — ${c.offer_key} (${c.brand_code})`, message: detail });
      } else if (status === 'warn') {
        addToast({ type: 'warning', title: `Tracking link WARN — ${c.offer_key} (${c.brand_code})`, message: detail });
      } else if (status === 'pass') {
        addToast({ type: 'success', title: `Tracking link OK — ${c.offer_key} (${c.brand_code})`, message: detail || undefined });
      } else {
        addToast({ type: 'info', title: `Tracking link ${status ?? 'untested'} — ${c.offer_key}`, message: detail || undefined });
      }
    } catch (err) {
      addToast({ type: 'error', title: 'Tracking link check failed', message: err instanceof Error ? err.message : String(err) });
    } finally {
      setFlight(c.id, 'check', false);
      fetchLibrary();
    }
  }, [isInFlight, setFlight, addToast, fetchLibrary]);

  const sendProof = useCallback(async (c: CreativeMeta) => {
    if (isInFlight(c.id, 'proof')) return;
    const to = proofEmail.trim();
    if (!to) {
      addToast({ type: 'warning', title: 'Proof recipient needed', message: 'Enter a test email in the library header first.' });
      return;
    }
    setFlight(c.id, 'proof', true);
    try {
      const res = await apiFetch(`/api/mailing/creatives/${c.id}/send-proof`, {
        method: 'POST',
        body: JSON.stringify({ to_email: to }),
        credentials: 'include',
      });
      const json: SendProofResponse & { error?: string } = await res.json();
      if (!res.ok || json.status === 'error') {
        throw new Error(json.error || json.message_id || `HTTP ${res.status}`);
      }
      addToast({ type: 'success', title: `Proof sent → ${to}`, message: json.message_id ? `message ${json.message_id}` : undefined });
    } catch (err) {
      addToast({ type: 'error', title: 'Proof send failed', message: err instanceof Error ? err.message : String(err) });
    } finally {
      setFlight(c.id, 'proof', false);
    }
  }, [isInFlight, setFlight, proofEmail, addToast]);

  const approveCreative = useCallback(async (c: CreativeMeta) => {
    if (isInFlight(c.id, 'approve')) return;
    setFlight(c.id, 'approve', true);
    try {
      const res = await apiFetch(`/api/mailing/creatives/${c.id}/approve`, {
        method: 'POST',
        body: JSON.stringify({}),
        credentials: 'include',
      });
      const json: { error?: string } = await res.json();
      if (!res.ok) throw new Error(json.error || `HTTP ${res.status}`);
      addToast({ type: 'success', title: `Approved — ${c.offer_key} (${c.brand_code})` });
    } catch (err) {
      addToast({ type: 'error', title: 'Approve failed', message: err instanceof Error ? err.message : String(err) });
    } finally {
      setApproveArmed(null);
      setFlight(c.id, 'approve', false);
      fetchLibrary();
    }
  }, [isInFlight, setFlight, addToast, fetchLibrary]);

  const rejectCreative = useCallback(async (c: CreativeMeta) => {
    if (isInFlight(c.id, 'reject')) return;
    setFlight(c.id, 'reject', true);
    try {
      const res = await apiFetch(`/api/mailing/creatives/${c.id}/reject`, {
        method: 'POST',
        body: JSON.stringify({}),
        credentials: 'include',
      });
      const json: { error?: string } = await res.json();
      if (!res.ok) throw new Error(json.error || `HTTP ${res.status}`);
      addToast({ type: 'warning', title: `Rejected — ${c.offer_key} (${c.brand_code})` });
    } catch (err) {
      addToast({ type: 'error', title: 'Reject failed', message: err instanceof Error ? err.message : String(err) });
    } finally {
      setFlight(c.id, 'reject', false);
      fetchLibrary();
    }
  }, [isInFlight, setFlight, addToast, fetchLibrary]);

  const sendChat = useCallback(async () => {
    const text = chatInput.trim();
    if (!text || chatBusy) return;
    setChatInput('');
    setMessages((m) => [...m, { role: 'user', content: text }]);
    setChatBusy(true);
    try {
      const res = await apiFetch('/api/mailing/creative-studio/agent/chat', {
        method: 'POST',
        body: JSON.stringify({ message: text, conversation_id: conversationId || undefined }),
        credentials: 'include',
      });
      const json = await res.json();
      if (!res.ok) throw new Error(json.error || `HTTP ${res.status}`);
      setConversationId(json.conversation_id);
      setMessages((m) => [...m, {
        role: 'assistant',
        content: json.response,
        actions: json.actions_taken ?? [],
      }]);
      if ((json.creatives_created ?? []).length > 0 && view === 'library') {
        fetchLibrary();
      }
    } catch (err) {
      setMessages((m) => [...m, {
        role: 'assistant',
        content: `⚠ ${err instanceof Error ? err.message : String(err)}`,
      }]);
    } finally {
      setChatBusy(false);
    }
  }, [chatInput, chatBusy, conversationId, view, fetchLibrary]);

  const brandOptions = useMemo(
    () => brands.map((b) => (
      <option key={b.brandKey} value={b.brandKey}>{b.name} ({b.brandKey})</option>
    )),
    [brands],
  );

  const activeResult = useMemo(
    () => results.find((r) => r.site_key === activeResultSite) ?? null,
    [results, activeResultSite],
  );

  return (
    <div style={{ padding: '20px 24px', color: '#e5e7eb', display: 'flex', flexDirection: 'column', height: 'calc(100vh - 60px)' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 14, flexWrap: 'wrap' }}>
        <h1 style={{ margin: 0, fontSize: 22, display: 'flex', alignItems: 'center', gap: 10 }}>
          <FontAwesomeIcon icon={faWandMagicSparkles} style={{ color: '#a78bfa' }} />
          Creative Studio
        </h1>
        <div style={{ display: 'flex', gap: 6 }}>
          <button onClick={() => setView('builder')}
            style={btnStyle(view === 'builder' ? '#312e81' : '#1e293b', view === 'builder' ? '#4338ca' : '#334155')}>
            <FontAwesomeIcon icon={faHammer} /> Builder
          </button>
          <button onClick={() => setView('library')}
            style={btnStyle(view === 'library' ? '#312e81' : '#1e293b', view === 'library' ? '#4338ca' : '#334155')}>
            <FontAwesomeIcon icon={faBookOpen} /> Library
          </button>
          <button onClick={() => setView('offers')}
            style={btnStyle(view === 'offers' ? '#312e81' : '#1e293b', view === 'offers' ? '#4338ca' : '#334155')}>
            <FontAwesomeIcon icon={faStamp} /> Offers
          </button>
          <button onClick={() => setChatOpen((v) => !v)}
            style={btnStyle(chatOpen ? '#14532d' : '#1e293b', chatOpen ? '#16a34a' : '#334155')}>
            <FontAwesomeIcon icon={faRobot} /> Agent
          </button>
        </div>
        {engineUp === false && (
          <span style={{ color: '#f59e0b', fontSize: 12 }}>
            <FontAwesomeIcon icon={faTriangleExclamation} /> creative engine offline — generation unavailable
          </span>
        )}
        {engineUp === true && (
          <span style={{ color: '#22c55e', fontSize: 12 }}>
            <FontAwesomeIcon icon={faCircleCheck} /> engine online
          </span>
        )}
      </div>
      <div style={{ fontSize: 12, color: '#94a3b8', marginTop: 4 }}>
        Renders with the production creative engine. Saves land in Content Library →
        “Creative Studio/&lt;SITE&gt;” and the creative library.
      </div>

      {view === 'offers' ? (
        <div style={{ display: 'flex', marginTop: 16, flex: 1, minHeight: 0 }}>
          <OfferProofs />
        </div>
      ) : (
      <div style={{ display: 'flex', gap: 16, marginTop: 16, flex: 1, minHeight: 0 }}>
        {/* ───────────────────────── main area ───────────────────────── */}
        <div style={{ flex: 1, minWidth: 0, display: 'flex', gap: 16 }}>
          {view === 'builder' ? (
            <>
              <div style={{ flex: '0 0 330px', overflowY: 'auto', paddingRight: 4 }}>
                <div style={{ display: 'flex', gap: 6, marginBottom: 14 }}>
                  <button onClick={() => setMode('newsletter')}
                    style={btnStyle(mode === 'newsletter' ? '#312e81' : '#1e293b', mode === 'newsletter' ? '#4338ca' : '#334155')}>
                    Newsletter
                  </button>
                  <button onClick={() => setMode('solo')}
                    style={btnStyle(mode === 'solo' ? '#312e81' : '#1e293b', mode === 'solo' ? '#4338ca' : '#334155')}>
                    Solo Offer
                  </button>
                </div>

                <label style={labelStyle}>
                  Sites ({selectedSites.length} selected)
                  <span style={{ float: 'right', display: 'flex', gap: 8 }}>
                    <span style={{ cursor: 'pointer', color: '#a78bfa' }}
                      onClick={() => setSelectedSites([...SITE_KEYS])}>all</span>
                    <span style={{ cursor: 'pointer', color: '#64748b' }}
                      onClick={() => setSelectedSites([])}>none</span>
                  </span>
                </label>
                <div style={{ display: 'flex', flexWrap: 'wrap', gap: 5, marginBottom: 12 }}>
                  {SITE_KEYS.map((s) => (
                    <span key={s} style={chipStyle(selectedSites.includes(s))} onClick={() => toggleSite(s)}>
                      {s.toUpperCase()}
                    </span>
                  ))}
                </div>

                <label style={labelStyle}>Primary offer brand</label>
                <select value={primaryBrandKey} onChange={(e) => setPrimaryBrandKey(e.target.value)} style={{ ...inputStyle, marginBottom: 12 }}>
                  {brandOptions}
                </select>

                {mode === 'newsletter' && (
                  <>
                    <label style={labelStyle}>Secondary offer (optional)</label>
                    <select value={secondaryBrandKey} onChange={(e) => setSecondaryBrandKey(e.target.value)} style={{ ...inputStyle, marginBottom: 12 }}>
                      <option value="">— none —</option>
                      {brandOptions}
                    </select>
                  </>
                )}

                {mode === 'solo' && (
                  <>
                    <label style={labelStyle}>Creative image URL *</label>
                    <input value={soloCreativeUrl} onChange={(e) => setSoloCreativeUrl(e.target.value)}
                      placeholder="https://img.projectjarvis.io/…" style={{ ...inputStyle, marginBottom: 12 }} />
                    <label style={labelStyle}>Below the ad</label>
                    <select value={soloBelowMode} onChange={(e) => setSoloBelowMode(e.target.value as typeof soloBelowMode)} style={{ ...inputStyle, marginBottom: 12 }}>
                      <option value="review_card">Review card</option>
                      <option value="full_review">Full review + articles</option>
                      <option value="none">Nothing (pure ad)</option>
                    </select>
                  </>
                )}

                <div style={{ borderTop: '1px solid #1f2937', margin: '4px 0 12px', paddingTop: 10 }}>
                  <div style={{ ...labelStyle, color: '#a78bfa' }}>
                    {mode === 'solo' ? 'Copy & imagery' : 'Overrides — swap imagery & copy (optional)'}
                  </div>
                  {mode === 'newsletter' && (
                    <>
                      <label style={labelStyle}>Banner / hero image URL</label>
                      <input value={bannerUrl} onChange={(e) => setBannerUrl(e.target.value)}
                        placeholder="blank = brand default" style={{ ...inputStyle, marginBottom: 10 }} />
                      <label style={labelStyle}>Image orientation</label>
                      <select value={imageOrientation} onChange={(e) => setImageOrientation(e.target.value as typeof imageOrientation)} style={{ ...inputStyle, marginBottom: 10 }}>
                        <option value="">auto</option>
                        <option value="horizontal">horizontal (full-bleed)</option>
                        <option value="vertical">vertical (portrait)</option>
                        <option value="logo">logo (centered, capped)</option>
                      </select>
                    </>
                  )}
                  <label style={labelStyle}>Logo URL</label>
                  <input value={logoUrl} onChange={(e) => setLogoUrl(e.target.value)}
                    placeholder="blank = brand default" style={{ ...inputStyle, marginBottom: 10 }} />
                  <label style={labelStyle}>{mode === 'solo' ? 'Headline' : 'Title'}</label>
                  <input value={titleOverride} onChange={(e) => setTitleOverride(e.target.value)} style={{ ...inputStyle, marginBottom: 10 }} />
                  <label style={labelStyle}>{mode === 'solo' ? 'Subheadline' : 'Subtitle'}</label>
                  <input value={subtitleOverride} onChange={(e) => setSubtitleOverride(e.target.value)} style={{ ...inputStyle, marginBottom: 10 }} />
                  <label style={labelStyle}>CTA label</label>
                  <input value={ctaLabel} onChange={(e) => setCtaLabel(e.target.value)} style={{ ...inputStyle, marginBottom: 10 }} />
                  <label style={labelStyle}>CTA URL (leave blank — tracking links are added automatically)</label>
                  <input value={ctaUrl} onChange={(e) => setCtaUrl(e.target.value)} style={{ ...inputStyle, marginBottom: 10 }} />
                </div>

                <label style={labelStyle}>Subject (leave blank to auto-fill)</label>
                <input value={subjectLine} onChange={(e) => setSubjectLine(e.target.value)} style={{ ...inputStyle, marginBottom: 12 }} />
                <label style={labelStyle}>Preview text (leave blank to auto-fill)</label>
                <input value={preheader} onChange={(e) => setPreheader(e.target.value)} style={{ ...inputStyle, marginBottom: 12 }} />

                <label style={{ fontSize: 13, color: '#cbd5e1', display: 'flex', alignItems: 'center', gap: 8, marginBottom: 16 }}>
                  <input type="checkbox" checked={refreshContent} onChange={(e) => setRefreshContent(e.target.checked)} />
                  Refresh editorial content (latest site articles)
                </label>

                <div style={{ display: 'flex', gap: 8 }}>
                  <button disabled={generating || engineUp === false} onClick={() => generate(false)}
                    style={{ ...btnStyle('#1e293b', '#334155'), opacity: generating ? 0.6 : 1 }}>
                    <FontAwesomeIcon icon={faRotate} spin={generating} /> Preview
                  </button>
                  <button disabled={generating || engineUp === false} onClick={() => generate(true)}
                    style={{ ...btnStyle('#312e81', '#4338ca'), opacity: generating ? 0.6 : 1 }}>
                    <FontAwesomeIcon icon={faFloppyDisk} /> Generate &amp; Save
                  </button>
                </div>

                {error && (
                  <div style={{ marginTop: 12, color: '#ef4444', fontSize: 13 }}>
                    <FontAwesomeIcon icon={faTriangleExclamation} /> {error}
                  </div>
                )}
                {results.some((r) => r.saved) && (
                  <div style={{ marginTop: 12, color: '#22c55e', fontSize: 13 }}>
                    <FontAwesomeIcon icon={faCircleCheck} /> Saved {results.filter((r) => r.saved).length}/{results.length} to
                    the Content Library
                  </div>
                )}
              </div>

              <div style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column' }}>
                {results.length > 0 ? (
                  <>
                    <div style={{ display: 'flex', gap: 5, flexWrap: 'wrap', marginBottom: 8 }}>
                      {results.map((r) => (
                        <span key={r.site_key}
                          style={{
                            ...chipStyle(activeResultSite === r.site_key),
                            ...(r.error ? { borderColor: '#ef4444', color: '#ef4444' } : {}),
                          }}
                          onClick={() => setActiveResultSite(r.site_key)}>
                          {r.site_key.toUpperCase()}{r.error ? ' ✕' : r.saved ? ' ✓' : ''}
                        </span>
                      ))}
                    </div>
                    {activeResult?.error ? (
                      <div style={{ color: '#ef4444', fontSize: 13, padding: 16 }}>
                        <FontAwesomeIcon icon={faTriangleExclamation} /> {activeResult.site_key.toUpperCase()}: {activeResult.error}
                      </div>
                    ) : activeResult ? (
                      <>
                        <div style={{ fontSize: 13, marginBottom: 8 }}>
                          <div style={{ fontWeight: 600 }}>{activeResult.subject}</div>
                          <div style={{ color: '#94a3b8', marginTop: 2 }}>{activeResult.preheader}</div>
                          {activeResult.filename && (
                            <div style={{ color: '#64748b', fontFamily: 'monospace', fontSize: 11, marginTop: 2 }}>
                              {activeResult.filename} · {activeResult.money_urls} tracking link{activeResult.money_urls === 1 ? '' : 's'}
                            </div>
                          )}
                        </div>
                        <iframe title={`preview-${activeResult.site_key}`} sandbox="" srcDoc={activeResult.html}
                          style={{ width: '100%', flex: 1, background: '#fff', border: '1px solid #334155', borderRadius: 8 }} />
                      </>
                    ) : null}
                  </>
                ) : (
                  <div style={{ color: '#64748b', fontSize: 13, padding: 24 }}>
                    Pick sites and hit Preview — one render per brand, tab across the results. The
                    creative engine produces the exact email a recipient would see (live editorial included).
                  </div>
                )}
              </div>
            </>
          ) : (
            <>
              <div style={{ flex: '0 0 760px', overflowY: 'auto' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 8, flexWrap: 'wrap' }}>
                  <button onClick={fetchLibrary} style={btnStyle('#1e293b', '#334155')}>
                    <FontAwesomeIcon icon={faRotate} spin={libraryLoading} /> Refresh
                  </button>
                  <span style={{ fontSize: 12, color: '#94a3b8' }}>
                    {creatives.length} creatives · manage in Content Library → “Creative Studio”
                  </span>
                  <span style={{ display: 'flex', alignItems: 'center', gap: 6, marginLeft: 'auto' }}>
                    <label style={{ ...labelStyle, marginBottom: 0 }}>Proof to</label>
                    <input
                      value={proofEmail}
                      onChange={(e) => setProofEmail(e.target.value)}
                      placeholder="test@example.com"
                      style={{ ...inputStyle, width: 180, padding: '5px 8px' }}
                    />
                  </span>
                </div>
                <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
                  <thead>
                    <tr style={{ color: '#94a3b8', textAlign: 'left' }}>
                      <th style={{ padding: '6px 8px' }}>Offer</th>
                      <th style={{ padding: '6px 8px' }}>Brand</th>
                      <th style={{ padding: '6px 8px' }}>Date</th>
                      <th style={{ padding: '6px 8px' }}>Source</th>
                      <th style={{ padding: '6px 8px' }}>Approval</th>
                      <th style={{ padding: '6px 8px' }}>Tracking Link</th>
                      <th style={{ padding: '6px 8px' }}>Actions</th>
                    </tr>
                  </thead>
                  <tbody>
                    {creatives.map((c) => (
                      <tr key={c.id} onClick={() => openPreview(c)}
                        style={{ cursor: 'pointer', background: selected?.id === c.id ? '#1e293b' : 'transparent', borderTop: '1px solid #1f2937' }}>
                        <td style={{ padding: '6px 8px' }} title={c.filename}>{c.offer_key}</td>
                        <td style={{ padding: '6px 8px' }}>{c.brand_code}</td>
                        <td style={{ padding: '6px 8px', color: '#94a3b8' }}>{new Date(c.generated_at).toLocaleDateString()}</td>
                        <td style={{ padding: '6px 8px', color: c.source === 'studio' ? '#a78bfa' : '#94a3b8' }}>{c.source}</td>
                        <td style={{ padding: '6px 8px' }}>
                          <span style={badgeStyle(approvalColor(c.approval_status))} title={c.approved_by ? `by ${c.approved_by}` : undefined}>
                            {c.approval_status ?? 'pending'}
                          </span>
                        </td>
                        <td style={{ padding: '6px 8px' }}>
                          <span style={badgeStyle(moneyLinkColor(c.money_link_status))} title={c.money_link_detail || undefined}>
                            {c.money_link_status ?? 'untested'}
                          </span>
                        </td>
                        <td style={{ padding: '6px 8px' }} onClick={(e) => e.stopPropagation()}>
                          <div style={{ display: 'flex', gap: 4, flexWrap: 'wrap' }}>
                            <button
                              disabled={isInFlight(c.id, 'check')}
                              onClick={() => checkMoneyLink(c)}
                              style={{ ...smallBtnStyle('#1e293b', '#334155'), opacity: isInFlight(c.id, 'check') ? 0.5 : 1 }}>
                              Check
                            </button>
                            <button
                              disabled={isInFlight(c.id, 'proof')}
                              onClick={() => sendProof(c)}
                              style={{ ...smallBtnStyle('#1e293b', '#334155'), opacity: isInFlight(c.id, 'proof') ? 0.5 : 1 }}>
                              Proof
                            </button>
                            {approveArmed === c.id ? (
                              <>
                                <button
                                  disabled={isInFlight(c.id, 'approve')}
                                  onClick={() => approveCreative(c)}
                                  style={{ ...smallBtnStyle('rgba(0,184,148,0.18)', '#00b894'), color: '#00b894', opacity: isInFlight(c.id, 'approve') ? 0.5 : 1 }}>
                                  Confirm
                                </button>
                                <button
                                  onClick={() => setApproveArmed(null)}
                                  style={smallBtnStyle('#1e293b', '#334155')}>
                                  Cancel
                                </button>
                              </>
                            ) : (
                              <button
                                disabled={isInFlight(c.id, 'approve')}
                                onClick={() => setApproveArmed(c.id)}
                                style={{ ...smallBtnStyle('rgba(0,184,148,0.12)', '#00b894'), color: '#00b894', opacity: isInFlight(c.id, 'approve') ? 0.5 : 1 }}>
                                Approve
                              </button>
                            )}
                            <button
                              disabled={isInFlight(c.id, 'reject')}
                              onClick={() => rejectCreative(c)}
                              style={{ ...smallBtnStyle('rgba(233,69,96,0.12)', '#e94560'), color: '#e94560', opacity: isInFlight(c.id, 'reject') ? 0.5 : 1 }}>
                              Reject
                            </button>
                          </div>
                        </td>
                      </tr>
                    ))}
                    {!libraryLoading && creatives.length === 0 && (
                      <tr><td colSpan={7} style={{ padding: 16, color: '#94a3b8' }}>No creatives yet.</td></tr>
                    )}
                  </tbody>
                </table>
              </div>
              <div style={{ flex: 1, minWidth: 0 }}>
                {selected ? (
                  <>
                    <div style={{ fontSize: 13, marginBottom: 8 }}>
                      <div style={{ fontWeight: 600 }}>{selected.subject}</div>
                      <div style={{ color: '#64748b', fontFamily: 'monospace', fontSize: 11, marginTop: 2 }}>{selected.filename}</div>
                    </div>
                    <div style={{
                      display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', marginBottom: 8,
                      padding: '8px 10px', background: '#0f172a', border: '1px solid #1f2937', borderRadius: 8,
                    }}>
                      <span style={badgeStyle(approvalColor(selected.approval_status))} title={selected.approved_by ? `by ${selected.approved_by}` : undefined}>
                        {selected.approval_status ?? 'pending'}
                      </span>
                      <span style={badgeStyle(moneyLinkColor(selected.money_link_status))}>
                        tracking link: {selected.money_link_status ?? 'untested'}
                      </span>
                      {selected.money_link_detail && (
                        <span style={{ fontSize: 11, color: '#94a3b8' }}>{selected.money_link_detail}</span>
                      )}
                      <button
                        disabled={isInFlight(selected.id, 'check')}
                        onClick={() => checkMoneyLink(selected)}
                        style={{ ...smallBtnStyle('#1e293b', '#334155'), marginLeft: 'auto', opacity: isInFlight(selected.id, 'check') ? 0.5 : 1 }}>
                        <FontAwesomeIcon icon={faRotate} spin={isInFlight(selected.id, 'check')} /> Run tracking-link check
                      </button>
                    </div>
                    <iframe title="library-preview" sandbox="" srcDoc={previewHtml ?? '<p style="font-family:sans-serif;color:#64748b">loading…</p>'}
                      style={{ width: '100%', height: 'calc(100% - 108px)', background: '#fff', border: '1px solid #334155', borderRadius: 8 }} />
                  </>
                ) : (
                  <div style={{ color: '#64748b', fontSize: 13, padding: 24 }}>Select a creative to preview.</div>
                )}
              </div>
            </>
          )}
        </div>

        {/* ───────────────────────── agent panel ───────────────────────── */}
        {chatOpen && (
          <div style={{
            flex: '0 0 360px', display: 'flex', flexDirection: 'column',
            background: '#0f172a', border: '1px solid #1f2937', borderRadius: 10, minHeight: 0,
          }}>
            <div style={{ padding: '10px 14px', borderBottom: '1px solid #1f2937', fontSize: 13, fontWeight: 600, display: 'flex', alignItems: 'center', gap: 8 }}>
              <FontAwesomeIcon icon={faRobot} style={{ color: '#a78bfa' }} /> Creative Agent
              <span style={{ fontWeight: 400, color: '#64748b', fontSize: 11 }}>generates via the creative engine</span>
            </div>
            <div style={{ flex: 1, overflowY: 'auto', padding: 12, display: 'flex', flexDirection: 'column', gap: 10 }}>
              {messages.length === 0 && (
                <div style={{ color: '#64748b', fontSize: 12, lineHeight: 1.6 }}>
                  Try: “Make a Warby Parker newsletter for all brands”, “Regenerate DB with this hero:
                  &lt;url&gt;”, “Give me 5 subject ideas from the BWP pool”.
                </div>
              )}
              {messages.map((m, i) => (
                <div key={i} style={{
                  alignSelf: m.role === 'user' ? 'flex-end' : 'flex-start',
                  maxWidth: '92%',
                  background: m.role === 'user' ? '#312e81' : '#1e293b',
                  border: `1px solid ${m.role === 'user' ? '#4338ca' : '#334155'}`,
                  borderRadius: 10, padding: '8px 12px', fontSize: 13, lineHeight: 1.5,
                }}>
                  {renderAgentText(m.content)}
                  {(m.actions ?? []).map((a, j) => (
                    <div key={j} style={{ marginTop: 6, fontSize: 11, color: '#22c55e' }}>
                      <FontAwesomeIcon icon={faCircleCheck} /> {a}
                    </div>
                  ))}
                </div>
              ))}
              {chatBusy && <div style={{ color: '#64748b', fontSize: 12 }}>thinking…</div>}
              <div ref={chatBottomRef} />
            </div>
            <div style={{ padding: 10, borderTop: '1px solid #1f2937', display: 'flex', gap: 8 }}>
              <input
                value={chatInput}
                onChange={(e) => setChatInput(e.target.value)}
                onKeyDown={(e) => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); sendChat(); } }}
                placeholder="Ask for creatives, copy, variants…"
                style={{ ...inputStyle, flex: 1 }}
                disabled={chatBusy}
              />
              <button onClick={sendChat} disabled={chatBusy || !chatInput.trim()} style={btnStyle('#312e81', '#4338ca')}>
                <FontAwesomeIcon icon={faPaperPlane} />
              </button>
            </div>
          </div>
        )}
      </div>
      )}
    </div>
  );
};
