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
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';

// Creative Studio v2 — the ReviewForge email builder, centralized in the
// portal. Builder renders through the engine sidecar (byte-identical to the
// operator's local ReviewForge); saves land in the Content Library
// ("Creative Studio/<SITE>") + the mailing_creatives registry; the docked
// agent generates conversationally (replaces the Cursor loop).

const SITE_KEYS = [
  'db', 'mh', 'qf', 'ht', 'bwp', 'fc', 'cp', 'hws',
  'rru', 'tot', 'yih', 'mrd', 'ci', 'lpl', 'rb', 'wfy',
];

interface StudioBrand {
  brandKey: string;
  name: string;
  category?: string;
}

interface GenerateResult {
  html: string;
  subject: string;
  preheader: string;
  filename?: string;
  template_id?: string;
  creative_id?: string;
  money_urls: number;
  saved: boolean;
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
  const [engineUp, setEngineUp] = useState<boolean | null>(null);
  const [brands, setBrands] = useState<StudioBrand[]>([]);
  const [view, setView] = useState<'builder' | 'library'>('builder');

  // Builder state
  const [mode, setMode] = useState<'newsletter' | 'solo'>('newsletter');
  const [siteKey, setSiteKey] = useState('db');
  const [primaryBrandKey, setPrimaryBrandKey] = useState('');
  const [secondaryBrandKey, setSecondaryBrandKey] = useState('');
  const [subjectLine, setSubjectLine] = useState('');
  const [preheader, setPreheader] = useState('');
  const [refreshContent, setRefreshContent] = useState(true);
  const [soloCreativeUrl, setSoloCreativeUrl] = useState('');
  const [soloHeadline, setSoloHeadline] = useState('');
  const [soloCtaLabel, setSoloCtaLabel] = useState('');
  const [soloBelowMode, setSoloBelowMode] = useState<'review_card' | 'full_review' | 'none'>('review_card');
  const [generating, setGenerating] = useState(false);
  const [result, setResult] = useState<GenerateResult | null>(null);
  const [error, setError] = useState<string | null>(null);

  // Library state
  const [creatives, setCreatives] = useState<CreativeMeta[]>([]);
  const [libraryLoading, setLibraryLoading] = useState(false);
  const [selected, setSelected] = useState<CreativeMeta | null>(null);
  const [previewHtml, setPreviewHtml] = useState<string | null>(null);

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

  useEffect(() => {
    chatBottomRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, [messages, chatBusy]);

  const generate = useCallback(async (save: boolean) => {
    setGenerating(true);
    setError(null);
    try {
      const body: Record<string, unknown> = {
        site_key: siteKey,
        primary_brand_key: primaryBrandKey,
        secondary_brand_key: secondaryBrandKey || undefined,
        subject_line: subjectLine || undefined,
        preheader: preheader || undefined,
        refresh_content: refreshContent,
        mode,
        save,
      };
      if (mode === 'solo') {
        body.solo = {
          creativeUrl: soloCreativeUrl,
          headline: soloHeadline || undefined,
          ctaLabel: soloCtaLabel || undefined,
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
      setResult(json as GenerateResult);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setGenerating(false);
    }
  }, [siteKey, primaryBrandKey, secondaryBrandKey, subjectLine, preheader,
      refreshContent, mode, soloCreativeUrl, soloHeadline, soloCtaLabel, soloBelowMode]);

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
        Renders with the production ReviewForge template engine. Saves land in Content Library →
        “Creative Studio/&lt;SITE&gt;” and the creative registry (pull to send-day with <code>forge-pull</code>).
      </div>

      <div style={{ display: 'flex', gap: 16, marginTop: 16, flex: 1, minHeight: 0 }}>
        {/* ───────────────────────── main area ───────────────────────── */}
        <div style={{ flex: 1, minWidth: 0, display: 'flex', gap: 16 }}>
          {view === 'builder' ? (
            <>
              <div style={{ flex: '0 0 320px', overflowY: 'auto', paddingRight: 4 }}>
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

                <label style={labelStyle}>Site (sending brand)</label>
                <select value={siteKey} onChange={(e) => setSiteKey(e.target.value)} style={{ ...inputStyle, marginBottom: 12 }}>
                  {SITE_KEYS.map((s) => <option key={s} value={s}>{s.toUpperCase()}</option>)}
                </select>

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
                    <label style={labelStyle}>Headline (optional)</label>
                    <input value={soloHeadline} onChange={(e) => setSoloHeadline(e.target.value)} style={{ ...inputStyle, marginBottom: 12 }} />
                    <label style={labelStyle}>CTA label (optional)</label>
                    <input value={soloCtaLabel} onChange={(e) => setSoloCtaLabel(e.target.value)} style={{ ...inputStyle, marginBottom: 12 }} />
                    <label style={labelStyle}>Below the ad</label>
                    <select value={soloBelowMode} onChange={(e) => setSoloBelowMode(e.target.value as typeof soloBelowMode)} style={{ ...inputStyle, marginBottom: 12 }}>
                      <option value="review_card">Review card</option>
                      <option value="full_review">Full review + articles</option>
                      <option value="none">Nothing (pure ad)</option>
                    </select>
                  </>
                )}

                <label style={labelStyle}>Subject (blank = pool pick)</label>
                <input value={subjectLine} onChange={(e) => setSubjectLine(e.target.value)} style={{ ...inputStyle, marginBottom: 12 }} />
                <label style={labelStyle}>Preheader (blank = pool pick)</label>
                <input value={preheader} onChange={(e) => setPreheader(e.target.value)} style={{ ...inputStyle, marginBottom: 12 }} />

                <label style={{ fontSize: 13, color: '#cbd5e1', display: 'flex', alignItems: 'center', gap: 8, marginBottom: 16 }}>
                  <input type="checkbox" checked={refreshContent} onChange={(e) => setRefreshContent(e.target.checked)} />
                  Refresh editorial content (live site feeds)
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
                {result?.saved && (
                  <div style={{ marginTop: 12, color: '#22c55e', fontSize: 13 }}>
                    <FontAwesomeIcon icon={faCircleCheck} /> Saved <code>{result.filename}</code> to the
                    Content Library + registry ({result.money_urls} money URL{result.money_urls === 1 ? '' : 's'})
                  </div>
                )}
              </div>

              <div style={{ flex: 1, minWidth: 0 }}>
                {result ? (
                  <>
                    <div style={{ fontSize: 13, marginBottom: 8 }}>
                      <div style={{ fontWeight: 600 }}>{result.subject}</div>
                      <div style={{ color: '#94a3b8', marginTop: 2 }}>{result.preheader}</div>
                    </div>
                    <iframe title="builder-preview" sandbox="" srcDoc={result.html}
                      style={{ width: '100%', height: 'calc(100% - 48px)', background: '#fff', border: '1px solid #334155', borderRadius: 8 }} />
                  </>
                ) : (
                  <div style={{ color: '#64748b', fontSize: 13, padding: 24 }}>
                    Configure and hit Preview — the engine renders the exact email a recipient would see
                    (live editorial blocks included).
                  </div>
                )}
              </div>
            </>
          ) : (
            <>
              <div style={{ flex: '0 0 480px', overflowY: 'auto' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 8 }}>
                  <button onClick={fetchLibrary} style={btnStyle('#1e293b', '#334155')}>
                    <FontAwesomeIcon icon={faRotate} spin={libraryLoading} /> Refresh
                  </button>
                  <span style={{ fontSize: 12, color: '#94a3b8' }}>
                    {creatives.length} creatives · manage in Content Library → “Creative Studio”
                  </span>
                </div>
                <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
                  <thead>
                    <tr style={{ color: '#94a3b8', textAlign: 'left' }}>
                      <th style={{ padding: '6px 8px' }}>Offer</th>
                      <th style={{ padding: '6px 8px' }}>Brand</th>
                      <th style={{ padding: '6px 8px' }}>Date</th>
                      <th style={{ padding: '6px 8px' }}>Source</th>
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
                      </tr>
                    ))}
                    {!libraryLoading && creatives.length === 0 && (
                      <tr><td colSpan={4} style={{ padding: 16, color: '#94a3b8' }}>No creatives yet.</td></tr>
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
                    <iframe title="library-preview" sandbox="" srcDoc={previewHtml ?? '<p style="font-family:sans-serif;color:#64748b">loading…</p>'}
                      style={{ width: '100%', height: 'calc(100% - 44px)', background: '#fff', border: '1px solid #334155', borderRadius: 8 }} />
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
              <span style={{ fontWeight: 400, color: '#64748b', fontSize: 11 }}>generates via the real engine</span>
            </div>
            <div style={{ flex: 1, overflowY: 'auto', padding: 12, display: 'flex', flexDirection: 'column', gap: 10 }}>
              {messages.length === 0 && (
                <div style={{ color: '#64748b', fontSize: 12, lineHeight: 1.6 }}>
                  Try: “Make a Warby Parker newsletter for DB and RRU”, “Give me 5 subject ideas for
                  the BWP pool”, “Generate a solo for the Jacuzzi hero image on MH”.
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
    </div>
  );
};
