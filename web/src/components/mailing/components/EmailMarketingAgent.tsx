import React, { useState, useRef, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faRobot, faCalendarAlt, faCogs, faComments, faPaperPlane,
  faSpinner, faPlus, faTrash, faCheck, faTimes,
  faChartLine, faArrowUp, faSyncAlt, faHistory,
  faEdit, faSave, faArrowLeft,
} from '@fortawesome/free-solid-svg-icons';

const PAGE_VERSION = '1.3';

// ── Markdown renderer (same pattern as CampaignCopilot) ─────────────────────
function simpleMarkdown(text: string): string {
  let out = text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  out = out.replace(/```([\s\S]*?)```/g, '<pre style="background:#0d1117;padding:8px 10px;border-radius:6px;overflow-x:auto;font-size:12px;margin:6px 0"><code>$1</code></pre>');
  out = out.replace(/`([^`]+)`/g, '<code style="background:#1a2233;padding:1px 5px;border-radius:3px;font-size:12px">$1</code>');
  out = out.replace(/\*\*(.+?)\*\*/g, '<strong>$1</strong>');
  out = out.replace(/\*(.+?)\*/g, '<em>$1</em>');
  out = out.replace(/^### (.+)$/gm, '<h4 style="margin:8px 0 4px;font-size:14px;color:#a5b4fc">$1</h4>');
  out = out.replace(/^## (.+)$/gm, '<h3 style="margin:10px 0 4px;font-size:15px;color:#a5b4fc">$1</h3>');
  out = out.replace(/^\|(.+)\|$/gm, (match) => {
    const cells = match.split('|').filter(c => c.trim());
    if (cells.every(c => /^[\s-:]+$/.test(c))) return '';
    const tds = cells.map(c => `<td style="padding:3px 8px;border:1px solid #2a3550">${c.trim()}</td>`).join('');
    return `<tr>${tds}</tr>`;
  });
  out = out.replace(/(<tr>[\s\S]*?<\/tr>)/g, '<table style="border-collapse:collapse;margin:6px 0;font-size:12px;width:100%">$1</table>');
  out = out.replace(/^- (.+)$/gm, '<li style="margin:2px 0;padding-left:4px">$1</li>');
  out = out.replace(/(<li[\s\S]*?<\/li>)/g, '<ul style="margin:4px 0;padding-left:16px">$1</ul>');
  out = out.replace(/<\/ul>\s*<ul[^>]*>/g, '');
  out = out.replace(/\n\n/g, '<br/><br/>');
  return out;
}

// ── Types ────────────────────────────────────────────────────────────────────
interface AgentMessage { role: 'user' | 'assistant'; content: string; timestamp: Date; actionsTaken?: string[]; }
interface Conversation { id: string; title: string; summary: string; message_count: number; updated_at: string; }
interface DomainStrategy { id: string; sending_domain: string; strategy: string; params: Record<string, any>; }
interface Recommendation { id: string; sending_domain: string; scheduled_date: string; scheduled_time?: string; campaign_name: string; reasoning: string; strategy: string; projected_volume: number; status: string; campaign_config?: Record<string, any>; }
interface CalendarDay { date: string; projected_volume: number; recommendations: Recommendation[]; }
interface ForecastData { month: string; sending_domain: string; strategy: string; days: CalendarDay[]; summary: { total_projected_volume: number; days_with_recommendations: number; }; }

type SubTab = 'chat' | 'calendar' | 'strategy';

// ── Styles ──────────────────────────────────────────────────────────────────
const S = {
  container: { display: 'flex', flexDirection: 'column' as const, height: '100%', background: '#0a0e1a', color: '#e2e8f0', fontFamily: '-apple-system,BlinkMacSystemFont,sans-serif' },
  header: { display: 'flex', alignItems: 'center', gap: 12, padding: '16px 24px', borderBottom: '1px solid rgba(99,102,241,0.15)', background: 'linear-gradient(135deg, #0f1629 0%, #131b33 100%)' },
  headerIcon: { width: 36, height: 36, borderRadius: 10, background: 'linear-gradient(135deg,#6366f1,#8b5cf6)', display: 'flex', alignItems: 'center', justifyContent: 'center', fontSize: 16, color: '#fff', flexShrink: 0 },
  headerTitle: { fontSize: 18, fontWeight: 700, color: '#e2e8f0', margin: 0 },
  headerSubtitle: { fontSize: 12, color: 'rgba(148,163,184,0.7)', marginTop: 2 },
  tabs: { display: 'flex', gap: 2, padding: '0 24px', background: '#0d1220', borderBottom: '1px solid rgba(99,102,241,0.1)' },
  tab: (active: boolean) => ({ padding: '10px 20px', fontSize: 13, fontWeight: active ? 600 : 400, color: active ? '#a5b4fc' : '#94a3b8', background: active ? 'rgba(99,102,241,0.08)' : 'transparent', border: 'none', borderBottom: active ? '2px solid #6366f1' : '2px solid transparent', cursor: 'pointer', display: 'flex', alignItems: 'center', gap: 6, transition: 'all 0.15s' }),
  body: { flex: 1, overflow: 'hidden', display: 'flex' },
};

// ═══════════════════════════════════════════════════════════════════════════
// CHAT SUB-TAB
// ═══════════════════════════════════════════════════════════════════════════
const AgentChat: React.FC = () => {
  const [messages, setMessages] = useState<AgentMessage[]>([{
    role: 'assistant', content: '**Maven** here — your email marketing strategist. I can analyze ISP health, plan campaigns, manage warmup strategies, create templates, and forecast your entire month.\n\n**Try asking:**\n- "How is our ISP health looking?"\n- "Plan a week of campaigns for quizfiesta.com"\n- "What templates do we have?"\n- "Set up a warmup strategy for our new domain"',
    timestamp: new Date(),
  }]);
  const [input, setInput] = useState('');
  const [loading, setLoading] = useState(false);
  const [conversationId, setConversationId] = useState<string | null>(null);
  const [conversations, setConversations] = useState<Conversation[]>([]);
  const [sidebarOpen, setSidebarOpen] = useState(true);
  const messagesEndRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => { messagesEndRef.current?.scrollIntoView({ behavior: 'smooth' }); }, [messages]);
  useEffect(() => { loadConversations(); }, []);

  const loadConversations = async () => {
    try {
      const resp = await fetch('/api/mailing/agent/conversations');
      if (resp.ok) setConversations(await resp.json());
    } catch {}
  };

  const loadConversation = async (id: string) => {
    try {
      const resp = await fetch(`/api/mailing/agent/conversations/${id}`);
      if (!resp.ok) return;
      const data = await resp.json();
      setConversationId(id);
      const msgs: AgentMessage[] = data.messages
        .filter((m: any) => m.role === 'user' || (m.role === 'assistant' && m.content))
        .map((m: any) => ({ role: m.role, content: m.content, timestamp: new Date(m.created_at) }));
      setMessages(msgs.length ? msgs : [{ role: 'assistant', content: 'Conversation loaded. How can I help?', timestamp: new Date() }]);
    } catch {}
  };

  const startNewConversation = () => {
    setConversationId(null);
    setMessages([{ role: 'assistant', content: '**Maven** here — starting a fresh conversation. What would you like to work on?', timestamp: new Date() }]);
  };

  const deleteConversation = async (id: string) => {
    await fetch(`/api/mailing/agent/conversations/${id}`, { method: 'DELETE' });
    if (conversationId === id) startNewConversation();
    loadConversations();
  };

  const sendMessage = useCallback(async (text: string) => {
    if (!text.trim() || loading) return;
    setInput('');
    setLoading(true);
    setMessages(prev => [...prev, { role: 'user', content: text.trim(), timestamp: new Date() }]);

    try {
      const resp = await fetch('/api/mailing/agent/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ message: text.trim(), conversation_id: conversationId || '' }),
      });
      const data = await resp.json();
      if (!resp.ok) throw new Error(data.error || 'Request failed');

      if (!conversationId && data.conversation_id) setConversationId(data.conversation_id);
      setMessages(prev => [...prev, { role: 'assistant', content: data.response, timestamp: new Date(), actionsTaken: data.actions_taken }]);
      loadConversations();
    } catch (err: unknown) {
      setMessages(prev => [...prev, { role: 'assistant', content: `**Error:** ${err instanceof Error ? err.message : 'Unknown'}`, timestamp: new Date() }]);
    } finally {
      setLoading(false);
    }
  }, [loading, conversationId]);

  return (
    <div style={{ display: 'flex', flex: 1, overflow: 'hidden' }}>
      {/* Conversation sidebar */}
      <div style={{ width: sidebarOpen ? 240 : 0, transition: 'width 0.2s', overflow: 'hidden', borderRight: '1px solid rgba(99,102,241,0.1)', background: '#0b1020', flexShrink: 0 }}>
        <div style={{ padding: '12px 10px', borderBottom: '1px solid rgba(99,102,241,0.08)' }}>
          <button onClick={startNewConversation} style={{ width: '100%', padding: '8px 12px', fontSize: 12, fontWeight: 600, color: '#a5b4fc', background: 'rgba(99,102,241,0.08)', border: '1px solid rgba(99,102,241,0.2)', borderRadius: 8, cursor: 'pointer', display: 'flex', alignItems: 'center', gap: 6 }}>
            <FontAwesomeIcon icon={faPlus} /> New Conversation
          </button>
        </div>
        <div style={{ overflowY: 'auto', maxHeight: 'calc(100vh - 200px)' }}>
          {conversations.map(c => (
            <div key={c.id} onClick={() => loadConversation(c.id)} style={{ padding: '10px 12px', cursor: 'pointer', background: conversationId === c.id ? 'rgba(99,102,241,0.1)' : 'transparent', borderBottom: '1px solid rgba(255,255,255,0.03)', display: 'flex', alignItems: 'center', gap: 8, transition: 'background 0.1s' }}>
              <FontAwesomeIcon icon={faComments} style={{ fontSize: 11, color: '#64748b', flexShrink: 0 }} />
              <div style={{ flex: 1, minWidth: 0 }}>
                <div style={{ fontSize: 12, fontWeight: 500, color: '#cbd5e1', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{c.title || 'Untitled'}</div>
                <div style={{ fontSize: 10, color: '#475569', marginTop: 2 }}>{c.message_count} msgs</div>
              </div>
              <button onClick={(e) => { e.stopPropagation(); deleteConversation(c.id); }} style={{ background: 'none', border: 'none', color: '#475569', cursor: 'pointer', fontSize: 11, padding: 4, opacity: 0.6 }} title="Delete">
                <FontAwesomeIcon icon={faTrash} />
              </button>
            </div>
          ))}
        </div>
      </div>

      {/* Chat main area */}
      <div style={{ flex: 1, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
        <div style={{ display: 'flex', alignItems: 'center', padding: '8px 16px', gap: 8, borderBottom: '1px solid rgba(99,102,241,0.05)' }}>
          <button onClick={() => setSidebarOpen(!sidebarOpen)} style={{ background: 'none', border: 'none', color: '#64748b', cursor: 'pointer', fontSize: 12 }}>
            <FontAwesomeIcon icon={faHistory} /> {sidebarOpen ? 'Hide' : 'Show'} History
          </button>
        </div>

        <div style={{ flex: 1, overflowY: 'auto', padding: '16px 20px', display: 'flex', flexDirection: 'column', gap: 12 }}>
          {messages.map((msg, i) => (
            <div key={i} style={{ display: 'flex', flexDirection: 'column', alignItems: msg.role === 'user' ? 'flex-end' : 'flex-start', maxWidth: '85%', alignSelf: msg.role === 'user' ? 'flex-end' : 'flex-start' }}>
              <div style={{
                padding: '10px 14px', borderRadius: msg.role === 'user' ? '14px 14px 4px 14px' : '14px 14px 14px 4px',
                background: msg.role === 'user' ? 'linear-gradient(135deg,#4f46e5,#6366f1)' : 'rgba(30,41,59,0.6)',
                border: msg.role === 'user' ? 'none' : '1px solid rgba(99,102,241,0.1)', fontSize: 13, lineHeight: 1.5, color: '#e2e8f0',
              }}>
                <div dangerouslySetInnerHTML={{ __html: simpleMarkdown(msg.content) }} />
              </div>
              {msg.actionsTaken && msg.actionsTaken.length > 0 && (
                <div style={{ marginTop: 4, display: 'flex', flexWrap: 'wrap', gap: 4 }}>
                  {msg.actionsTaken.map((a, j) => (
                    <span key={j} style={{ fontSize: 10, padding: '2px 8px', borderRadius: 4, background: 'rgba(34,197,94,0.1)', color: '#4ade80', border: '1px solid rgba(34,197,94,0.15)' }}>
                      <FontAwesomeIcon icon={faCheck} style={{ marginRight: 4 }} />{a}
                    </span>
                  ))}
                </div>
              )}
              <div style={{ fontSize: 10, color: '#475569', marginTop: 4 }}>{msg.timestamp.toLocaleTimeString()}</div>
            </div>
          ))}
          {loading && (
            <div style={{ display: 'flex', alignItems: 'center', gap: 8, color: '#64748b', fontSize: 13 }}>
              <FontAwesomeIcon icon={faSpinner} spin /> Maven is thinking...
            </div>
          )}
          <div ref={messagesEndRef} />
        </div>

        <form onSubmit={(e) => { e.preventDefault(); sendMessage(input); }} style={{ padding: '12px 16px', borderTop: '1px solid rgba(99,102,241,0.1)', display: 'flex', gap: 8 }}>
          <input ref={inputRef} value={input} onChange={e => setInput(e.target.value)} placeholder="Ask Maven anything about your email program..."
            disabled={loading} style={{ flex: 1, padding: '10px 14px', background: '#111827', border: '1px solid rgba(99,102,241,0.15)', borderRadius: 10, color: '#e2e8f0', fontSize: 13, outline: 'none' }} />
          <button type="submit" disabled={loading || !input.trim()} style={{ padding: '10px 16px', background: 'linear-gradient(135deg,#6366f1,#8b5cf6)', border: 'none', borderRadius: 10, color: '#fff', cursor: 'pointer', fontSize: 13, fontWeight: 600, opacity: loading || !input.trim() ? 0.5 : 1 }}>
            <FontAwesomeIcon icon={faPaperPlane} />
          </button>
        </form>
      </div>
    </div>
  );
};

// ═══════════════════════════════════════════════════════════════════════════
// CALENDAR SUB-TAB
// ═══════════════════════════════════════════════════════════════════════════
const ISP_BUCKETS = [
  { key: 'gmail', label: 'Gmail' },
  { key: 'yahoo', label: 'Yahoo/AOL' },
  { key: 'microsoft', label: 'Microsoft' },
  { key: 'apple', label: 'Apple' },
  { key: 'comcast', label: 'Comcast' },
  { key: 'att', label: 'AT&T' },
  { key: 'cox', label: 'Cox' },
  { key: 'charter', label: 'Charter' },
];

const AgentCalendar: React.FC = () => {
  const [forecast, setForecast] = useState<ForecastData | null>(null);
  const [loading, setLoading] = useState(false);
  const [selectedDay, setSelectedDay] = useState<CalendarDay | null>(null);
  const [selectedRec, setSelectedRec] = useState<Recommendation | null>(null);
  const [domains, setDomains] = useState<string[]>([]);
  const [selectedDomain, setSelectedDomain] = useState('');
  const [month, setMonth] = useState(() => new Date().toISOString().slice(0, 7));
  const [generating, setGenerating] = useState(false);
  const [editConfig, setEditConfig] = useState<Record<string, any>>({});
  const [saving, setSaving] = useState(false);
  const [miniChatInput, setMiniChatInput] = useState('');
  const [miniChatMessages, setMiniChatMessages] = useState<{role: string; content: string}[]>([]);
  const [miniChatLoading, setMiniChatLoading] = useState(false);
  const [cameFromDay, setCameFromDay] = useState<CalendarDay | null>(null);
  const [templateHtml, setTemplateHtml] = useState<string | null>(null);
  const [showTemplatePreview, setShowTemplatePreview] = useState(false);
  const [templateLoading, setTemplateLoading] = useState(false);
  const [approving, setApproving] = useState(false);
  const [approvalResult, setApprovalResult] = useState<Record<string, any> | null>(null);

  useEffect(() => { loadDomains(); }, []);
  useEffect(() => { if (selectedDomain) loadForecast(); }, [selectedDomain, month]);

  useEffect(() => {
    if (selectedRec) {
      const cfg = selectedRec.campaign_config || {};
      setEditConfig({
        campaign_name: selectedRec.campaign_name || '',
        scheduled_date: selectedRec.scheduled_date || '',
        scheduled_time: (selectedRec.scheduled_time || '13:00').replace(/:\d{2}$/, ''),
        from_name: cfg.from_name || '',
        from_email: cfg.from_email || '',
        subject: cfg.subject || '',
        preview_text: cfg.preview_text || '',
        template_id: cfg.template_id || '',
        template_name: cfg.template_name || '',
        wave_interval_minutes: cfg.wave_interval_minutes || 15,
        throttle_per_wave: cfg.throttle_per_wave || 0,
        audience_priority: cfg.audience_priority || ['openers_7d','clickers_14d','engagers_30d','recent_subscribers','cold'],
        inclusion_lists: cfg.inclusion_lists || [],
        exclusion_lists: cfg.exclusion_lists || [],
        isp_quotas: cfg.isp_quotas || {},
      });
      setMiniChatMessages([]);
      setMiniChatInput('');
      setTemplateHtml(null);
      setShowTemplatePreview(false);
      setApprovalResult(null);
      if (cfg.template_id) {
        setTemplateLoading(true);
        fetch(`/api/mailing/templates/${cfg.template_id}`)
          .then(r => r.ok ? r.json() : null)
          .then(data => { if (data?.html_content) setTemplateHtml(data.html_content); })
          .catch(() => {})
          .finally(() => setTemplateLoading(false));
      }
    }
  }, [selectedRec]);

  const loadDomains = async () => {
    try {
      const resp = await fetch('/api/mailing/agent/strategies');
      if (resp.ok) {
        const data = await resp.json();
        const d = data.map((s: DomainStrategy) => s.sending_domain);
        setDomains(d);
        if (d.length > 0 && !selectedDomain) setSelectedDomain(d[0]);
      }
    } catch {}
  };

  const loadForecast = async () => {
    setLoading(true);
    try {
      const resp = await fetch(`/api/mailing/agent/calendar/forecast?month=${month}&sending_domain=${encodeURIComponent(selectedDomain)}`);
      if (resp.ok) setForecast(await resp.json());
    } catch {} finally { setLoading(false); }
  };

  const generateForecast = async () => {
    if (!selectedDomain) return;
    setGenerating(true);
    try {
      await fetch('/api/mailing/agent/calendar/generate', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ sending_domain: selectedDomain, month, force_regenerate: true }),
      });
      await loadForecast();
    } catch {} finally { setGenerating(false); }
  };

  const approveRec = async (id: string) => {
    setApproving(true);
    setApprovalResult(null);
    try {
      const resp = await fetch(`/api/mailing/agent/calendar/recommendations/${id}/approve`, { method: 'POST' });
      const data = await resp.json();
      if (!resp.ok) {
        setApprovalResult({ error: data.error || 'Approval failed' });
        return;
      }
      setApprovalResult(data);
      loadForecast();
    } catch (err: unknown) {
      setApprovalResult({ error: err instanceof Error ? err.message : 'Unknown error' });
    } finally {
      setApproving(false);
    }
  };
  const rejectRec = async (id: string) => {
    await fetch(`/api/mailing/agent/calendar/recommendations/${id}/reject`, { method: 'POST' });
    loadForecast();
    setSelectedRec(null);
  };

  const saveRecommendation = async () => {
    if (!selectedRec) return;
    setSaving(true);
    try {
      const resp = await fetch(`/api/mailing/agent/calendar/recommendations/${selectedRec.id}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(editConfig),
      });
      if (resp.ok) {
        await loadForecast();
        setSelectedRec(null);
      }
    } catch {} finally { setSaving(false); }
  };

  const sendMiniChat = async () => {
    if (!miniChatInput.trim() || miniChatLoading || !selectedRec) return;
    const msg = miniChatInput.trim();
    setMiniChatInput('');
    setMiniChatMessages(prev => [...prev, { role: 'user', content: msg }]);
    setMiniChatLoading(true);
    try {
      const resp = await fetch('/api/mailing/agent/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          message: `[Re: campaign "${selectedRec.campaign_name}" for ${selectedRec.scheduled_date}] ${msg}`,
          conversation_id: '',
        }),
      });
      const data = await resp.json();
      if (resp.ok) {
        setMiniChatMessages(prev => [...prev, { role: 'assistant', content: data.response }]);
      }
    } catch {} finally { setMiniChatLoading(false); }
  };

  const weekdays = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
  const today = new Date().toISOString().slice(0, 10);

  const buildCalendarGrid = () => {
    if (!forecast) return [];
    const firstDay = new Date(forecast.days[0]?.date || month + '-01');
    const startDow = firstDay.getDay();
    const grid: (CalendarDay | null)[] = [];
    for (let i = 0; i < startDow; i++) grid.push(null);
    for (const d of forecast.days) grid.push(d);
    while (grid.length % 7 !== 0) grid.push(null);
    return grid;
  };

  const statusColor = (s: string) => {
    switch (s) { case 'approved': return '#22c55e'; case 'rejected': return '#ef4444'; case 'executed': return '#3b82f6'; case 'failed': return '#f97316'; default: return '#eab308'; }
  };

  return (
    <div style={{ flex: 1, overflow: 'auto', padding: 20 }}>
      {/* Controls */}
      <div style={{ display: 'flex', gap: 12, marginBottom: 16, alignItems: 'center', flexWrap: 'wrap' }}>
        <select value={selectedDomain} onChange={e => setSelectedDomain(e.target.value)} style={{ padding: '8px 12px', background: '#111827', border: '1px solid rgba(99,102,241,0.2)', borderRadius: 8, color: '#e2e8f0', fontSize: 13 }}>
          <option value="">Select Domain</option>
          {domains.map(d => <option key={d} value={d}>{d}</option>)}
        </select>
        <input type="month" value={month} onChange={e => setMonth(e.target.value)} style={{ padding: '8px 12px', background: '#111827', border: '1px solid rgba(99,102,241,0.2)', borderRadius: 8, color: '#e2e8f0', fontSize: 13 }} />
        <button onClick={generateForecast} disabled={generating || !selectedDomain}
          style={{ padding: '8px 16px', background: 'linear-gradient(135deg,#6366f1,#8b5cf6)', border: 'none', borderRadius: 8, color: '#fff', cursor: 'pointer', fontSize: 13, fontWeight: 600, opacity: generating || !selectedDomain ? 0.5 : 1, display: 'flex', alignItems: 'center', gap: 6 }}>
          <FontAwesomeIcon icon={generating ? faSpinner : faSyncAlt} spin={generating} /> {generating ? 'Generating...' : 'Generate Forecast'}
        </button>
        {forecast?.summary && (
          <div style={{ marginLeft: 'auto', fontSize: 12, color: '#94a3b8', display: 'flex', gap: 16 }}>
            <span><strong style={{ color: '#a5b4fc' }}>{forecast.summary.total_projected_volume.toLocaleString()}</strong> total volume</span>
            <span><strong style={{ color: '#a5b4fc' }}>{forecast.summary.days_with_recommendations}</strong> send days</span>
            {forecast.strategy && <span>Strategy: <strong style={{ color: forecast.strategy === 'warmup' ? '#fbbf24' : '#34d399' }}>{forecast.strategy}</strong></span>}
          </div>
        )}
      </div>

      {!selectedDomain && (
        <div style={{ textAlign: 'center', padding: 60, color: '#475569' }}>
          <FontAwesomeIcon icon={faCogs} style={{ fontSize: 32, marginBottom: 12 }} />
          <div style={{ fontSize: 14 }}>Configure a domain strategy first to see the marketing calendar.</div>
          <div style={{ fontSize: 12, marginTop: 4, color: '#334155' }}>Go to the Strategy tab to set up warmup or performance settings.</div>
        </div>
      )}

      {loading && <div style={{ textAlign: 'center', padding: 40, color: '#64748b' }}><FontAwesomeIcon icon={faSpinner} spin /> Loading forecast...</div>}

      {/* Calendar grid */}
      {forecast && !loading && (
        <div style={{ background: '#0d1220', borderRadius: 12, border: '1px solid rgba(99,102,241,0.1)', overflow: 'hidden' }}>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(7, 1fr)' }}>
            {weekdays.map(d => <div key={d} style={{ padding: '8px 4px', textAlign: 'center', fontSize: 11, fontWeight: 600, color: '#64748b', borderBottom: '1px solid rgba(99,102,241,0.08)' }}>{d}</div>)}
            {buildCalendarGrid().map((day, i) => (
              <div key={i} onClick={() => day && day.recommendations.length > 0 && setSelectedDay(day)}
                style={{
                  minHeight: 80, padding: 6, borderBottom: '1px solid rgba(99,102,241,0.05)', borderRight: '1px solid rgba(99,102,241,0.05)',
                  background: day?.date === today ? 'rgba(99,102,241,0.06)' : 'transparent',
                  cursor: day?.recommendations.length ? 'pointer' : 'default', transition: 'background 0.1s',
                }}>
                {day && (
                  <>
                    <div style={{ fontSize: 11, color: day.date === today ? '#a5b4fc' : '#64748b', fontWeight: day.date === today ? 700 : 400 }}>{parseInt(day.date.split('-')[2])}</div>
                    {day.projected_volume > 0 && <div style={{ fontSize: 10, color: '#6366f1', fontWeight: 600, marginTop: 2 }}>{(day.projected_volume / 1000).toFixed(0)}K</div>}
                    {day.recommendations.map((r, j) => (
                      <div key={j} onClick={(e) => { e.stopPropagation(); setCameFromDay(day); setSelectedDay(null); setSelectedRec(r); }}
                        style={{ marginTop: 2, fontSize: 9, padding: '2px 4px', borderRadius: 4, background: `${statusColor(r.status)}15`, color: statusColor(r.status), overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', cursor: 'pointer', border: `1px solid ${statusColor(r.status)}30` }}>
                        {r.campaign_name?.split('—')[0]?.trim() || r.sending_domain}
                      </div>
                    ))}
                  </>
                )}
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Day detail panel */}
      {selectedDay && (
        <div style={{ position: 'fixed', top: 0, right: 0, width: 440, height: '100vh', background: '#0f1629', borderLeft: '1px solid rgba(99,102,241,0.15)', zIndex: 100, display: 'flex', flexDirection: 'column', boxShadow: '-10px 0 40px rgba(0,0,0,0.5)' }}>
          <div style={{ padding: '16px 20px', borderBottom: '1px solid rgba(99,102,241,0.1)', display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
            <h3 style={{ margin: 0, fontSize: 15, color: '#e2e8f0' }}>{new Date(selectedDay.date + 'T12:00:00').toLocaleDateString('en-US', { weekday: 'long', month: 'short', day: 'numeric' })}</h3>
            <button onClick={() => setSelectedDay(null)} style={{ background: 'none', border: 'none', color: '#64748b', cursor: 'pointer', fontSize: 16 }}><FontAwesomeIcon icon={faTimes} /></button>
          </div>
          <div style={{ flex: 1, overflowY: 'auto', padding: 16 }}>
            <div style={{ fontSize: 12, color: '#94a3b8', marginBottom: 12 }}>Projected volume: <strong style={{ color: '#a5b4fc' }}>{selectedDay.projected_volume.toLocaleString()}</strong></div>
            {selectedDay.recommendations.map((rec, i) => (
              <div key={i} style={{ background: '#111827', borderRadius: 10, padding: 14, marginBottom: 10, border: '1px solid rgba(99,102,241,0.1)' }}>
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8 }}>
                  <span style={{ fontWeight: 600, fontSize: 13, color: '#e2e8f0' }}>{rec.campaign_name}</span>
                  <span style={{ fontSize: 10, padding: '2px 8px', borderRadius: 4, background: `${statusColor(rec.status)}15`, color: statusColor(rec.status), fontWeight: 600 }}>{rec.status}</span>
                </div>
                <div style={{ fontSize: 12, color: '#94a3b8', marginBottom: 6 }}>{rec.reasoning}</div>
                <div style={{ fontSize: 11, color: '#64748b', marginBottom: 8 }}>Volume: {rec.projected_volume.toLocaleString()} | Time: {rec.scheduled_time || '13:00'} UTC</div>
                {rec.campaign_config?.isp_quotas && (
                  <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4, marginBottom: 8 }}>
                    {Object.entries(rec.campaign_config.isp_quotas).map(([isp, q]) => (
                      <span key={isp} style={{ fontSize: 10, padding: '2px 6px', borderRadius: 4, background: 'rgba(99,102,241,0.08)', color: '#818cf8' }}>
                        {isp}: {typeof q === 'number' ? (q as number).toLocaleString() : String(q)}
                      </span>
                    ))}
                  </div>
                )}
                {rec.status === 'pending' && (
                  <div style={{ display: 'flex', gap: 8, marginTop: 8 }}>
                    <button onClick={() => approveRec(rec.id)} style={{ flex: 1, padding: '6px 12px', background: 'rgba(34,197,94,0.1)', border: '1px solid rgba(34,197,94,0.3)', borderRadius: 6, color: '#22c55e', cursor: 'pointer', fontSize: 12, fontWeight: 600 }}>
                      <FontAwesomeIcon icon={faCheck} /> Approve
                    </button>
                    <button onClick={() => rejectRec(rec.id)} style={{ flex: 1, padding: '6px 12px', background: 'rgba(239,68,68,0.1)', border: '1px solid rgba(239,68,68,0.3)', borderRadius: 6, color: '#ef4444', cursor: 'pointer', fontSize: 12, fontWeight: 600 }}>
                      <FontAwesomeIcon icon={faTimes} /> Reject
                    </button>
                  </div>
                )}
                <button onClick={() => { setCameFromDay(selectedDay); setSelectedDay(null); setSelectedRec(rec); }} style={{ width: '100%', padding: '6px 12px', background: 'rgba(99,102,241,0.06)', border: '1px solid rgba(99,102,241,0.12)', borderRadius: 6, color: '#818cf8', cursor: 'pointer', fontSize: 11, fontWeight: 600, marginTop: 8 }}>
                  View Details <FontAwesomeIcon icon={faEdit} style={{ marginLeft: 4 }} />
                </button>
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Recommendation detail slide-out */}
      {selectedRec && (
        <div style={{ position: 'fixed', top: 0, right: 0, width: 560, height: '100vh', background: '#0f1629', borderLeft: '1px solid rgba(99,102,241,0.15)', zIndex: 200, display: 'flex', flexDirection: 'column', boxShadow: '-10px 0 40px rgba(0,0,0,0.5)' }}>
          {/* Section 1: Header */}
          <div style={{ padding: '16px 20px', borderBottom: '1px solid rgba(99,102,241,0.1)', display: 'flex', flexDirection: 'column', gap: 8 }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
              {cameFromDay && (
                <button onClick={() => { setSelectedRec(null); setSelectedDay(cameFromDay); setCameFromDay(null); }} style={{ background: 'none', border: 'none', color: '#818cf8', cursor: 'pointer', fontSize: 12, display: 'flex', alignItems: 'center', gap: 4, padding: 0 }}>
                  <FontAwesomeIcon icon={faArrowLeft} /> Back to day
                </button>
              )}
              <div style={{ flex: 1 }} />
              <span style={{ fontSize: 10, padding: '2px 8px', borderRadius: 4, background: `${statusColor(selectedRec.status)}15`, color: statusColor(selectedRec.status), fontWeight: 600 }}>{selectedRec.status}</span>
              <button onClick={() => { setSelectedRec(null); setCameFromDay(null); }} style={{ background: 'none', border: 'none', color: '#64748b', cursor: 'pointer', fontSize: 16 }}><FontAwesomeIcon icon={faTimes} /></button>
            </div>
            <input value={editConfig.campaign_name || ''} onChange={e => setEditConfig(c => ({ ...c, campaign_name: e.target.value }))} style={{ background: '#111827', border: '1px solid rgba(99,102,241,0.2)', borderRadius: 8, color: '#e2e8f0', fontSize: 16, fontWeight: 700, padding: '8px 12px', width: '100%', outline: 'none', boxSizing: 'border-box' as const }} />
            <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
              <span style={{ fontSize: 11, padding: '2px 8px', borderRadius: 6, fontWeight: 600, background: selectedRec.strategy === 'warmup' ? 'rgba(251,191,36,0.1)' : 'rgba(52,211,153,0.1)', color: selectedRec.strategy === 'warmup' ? '#fbbf24' : '#34d399', border: `1px solid ${selectedRec.strategy === 'warmup' ? 'rgba(251,191,36,0.2)' : 'rgba(52,211,153,0.2)'}` }}>
                <FontAwesomeIcon icon={selectedRec.strategy === 'warmup' ? faArrowUp : faChartLine} style={{ marginRight: 4 }} />{selectedRec.strategy || 'N/A'}
              </span>
              <span style={{ fontSize: 12, color: '#94a3b8' }}>{selectedRec.sending_domain}</span>
            </div>
          </div>

          {/* Scrollable body */}
          <div style={{ flex: 1, overflowY: 'auto', padding: '16px 20px', display: 'flex', flexDirection: 'column', gap: 20 }}>
            {/* Section 2: Schedule & Delivery */}
            <div>
              <div style={{ fontSize: 13, fontWeight: 600, color: '#a5b4fc', marginBottom: 8, paddingBottom: 6, borderBottom: '1px solid rgba(99,102,241,0.1)' }}>Schedule &amp; Delivery</div>
              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 10 }}>
                <div>
                  <label style={{ display: 'block', fontSize: 11, color: '#94a3b8', marginBottom: 4, fontWeight: 600 }}>Date</label>
                  <input type="date" value={editConfig.scheduled_date || ''} onChange={e => setEditConfig(c => ({ ...c, scheduled_date: e.target.value }))} style={{ width: '100%', padding: '8px 12px', background: '#111827', border: '1px solid rgba(99,102,241,0.2)', borderRadius: 8, color: '#e2e8f0', fontSize: 13, boxSizing: 'border-box' as const }} />
                </div>
                <div>
                  <label style={{ display: 'block', fontSize: 11, color: '#94a3b8', marginBottom: 4, fontWeight: 600 }}>Time (UTC)</label>
                  <input type="time" value={editConfig.scheduled_time || ''} onChange={e => setEditConfig(c => ({ ...c, scheduled_time: e.target.value }))} style={{ width: '100%', padding: '8px 12px', background: '#111827', border: '1px solid rgba(99,102,241,0.2)', borderRadius: 8, color: '#e2e8f0', fontSize: 13, boxSizing: 'border-box' as const }} />
                </div>
                <div>
                  <label style={{ display: 'block', fontSize: 11, color: '#94a3b8', marginBottom: 4, fontWeight: 600 }}>Wave Interval (min)</label>
                  <input type="number" value={editConfig.wave_interval_minutes ?? 15} onChange={e => setEditConfig(c => ({ ...c, wave_interval_minutes: +e.target.value }))} style={{ width: '100%', padding: '8px 12px', background: '#111827', border: '1px solid rgba(99,102,241,0.2)', borderRadius: 8, color: '#e2e8f0', fontSize: 13, boxSizing: 'border-box' as const }} />
                </div>
                <div>
                  <label style={{ display: 'block', fontSize: 11, color: '#94a3b8', marginBottom: 4, fontWeight: 600 }}>Throttle / Wave</label>
                  <input type="number" value={editConfig.throttle_per_wave ?? 0} onChange={e => setEditConfig(c => ({ ...c, throttle_per_wave: +e.target.value }))} placeholder="0 = unlimited" style={{ width: '100%', padding: '8px 12px', background: '#111827', border: '1px solid rgba(99,102,241,0.2)', borderRadius: 8, color: '#e2e8f0', fontSize: 13, boxSizing: 'border-box' as const }} />
                </div>
              </div>
            </div>

            {/* Section 3: Content */}
            <div>
              <div style={{ fontSize: 13, fontWeight: 600, color: '#a5b4fc', marginBottom: 8, paddingBottom: 6, borderBottom: '1px solid rgba(99,102,241,0.1)' }}>Content</div>
              <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 10 }}>
                  <div>
                    <label style={{ display: 'block', fontSize: 11, color: '#94a3b8', marginBottom: 4, fontWeight: 600 }}>From Name</label>
                    <input value={editConfig.from_name || ''} onChange={e => setEditConfig(c => ({ ...c, from_name: e.target.value }))} style={{ width: '100%', padding: '8px 12px', background: '#111827', border: '1px solid rgba(99,102,241,0.2)', borderRadius: 8, color: '#e2e8f0', fontSize: 13, boxSizing: 'border-box' as const }} />
                  </div>
                  <div>
                    <label style={{ display: 'block', fontSize: 11, color: '#94a3b8', marginBottom: 4, fontWeight: 600 }}>From Email</label>
                    <input value={editConfig.from_email || ''} readOnly style={{ width: '100%', padding: '8px 12px', background: '#111827', border: '1px solid rgba(99,102,241,0.2)', borderRadius: 8, color: '#94a3b8', fontSize: 13, cursor: 'not-allowed', boxSizing: 'border-box' as const }} />
                  </div>
                </div>
                <div>
                  <label style={{ display: 'block', fontSize: 11, color: '#94a3b8', marginBottom: 4, fontWeight: 600 }}>Subject Line</label>
                  <input value={editConfig.subject || ''} onChange={e => setEditConfig(c => ({ ...c, subject: e.target.value }))} style={{ width: '100%', padding: '8px 12px', background: '#111827', border: '1px solid rgba(99,102,241,0.2)', borderRadius: 8, color: '#e2e8f0', fontSize: 13, boxSizing: 'border-box' as const }} />
                </div>
                <div>
                  <label style={{ display: 'block', fontSize: 11, color: '#94a3b8', marginBottom: 4, fontWeight: 600 }}>Preview / Pre-header Text</label>
                  <input value={editConfig.preview_text || ''} onChange={e => setEditConfig(c => ({ ...c, preview_text: e.target.value }))} style={{ width: '100%', padding: '8px 12px', background: '#111827', border: '1px solid rgba(99,102,241,0.2)', borderRadius: 8, color: '#e2e8f0', fontSize: 13, boxSizing: 'border-box' as const }} />
                </div>
                {editConfig.template_id ? (
                  <div>
                    <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '8px 10px', background: 'rgba(99,102,241,0.06)', borderRadius: 8, border: '1px solid rgba(99,102,241,0.1)' }}>
                      <div>
                        <div style={{ fontSize: 12, color: '#a5b4fc', fontWeight: 600 }}>{editConfig.template_name || 'Template'}</div>
                        <div style={{ fontSize: 10, color: '#64748b', marginTop: 2 }}>ID: {editConfig.template_id}</div>
                      </div>
                      <button onClick={() => setShowTemplatePreview(!showTemplatePreview)}
                        style={{ padding: '5px 12px', background: showTemplatePreview ? 'rgba(239,68,68,0.1)' : 'rgba(99,102,241,0.1)', border: `1px solid ${showTemplatePreview ? 'rgba(239,68,68,0.2)' : 'rgba(99,102,241,0.2)'}`, borderRadius: 6, color: showTemplatePreview ? '#ef4444' : '#a5b4fc', cursor: 'pointer', fontSize: 11, fontWeight: 600 }}>
                        {showTemplatePreview ? 'Hide Preview' : 'Preview Template'}
                      </button>
                    </div>
                    {showTemplatePreview && (
                      <div style={{ marginTop: 8, borderRadius: 8, overflow: 'hidden', border: '1px solid rgba(99,102,241,0.15)' }}>
                        {templateLoading ? (
                          <div style={{ padding: 24, textAlign: 'center', color: '#64748b', fontSize: 12 }}><FontAwesomeIcon icon={faSpinner} spin /> Loading template...</div>
                        ) : templateHtml ? (
                          <iframe srcDoc={templateHtml} sandbox="" style={{ width: '100%', height: 400, border: 'none', background: '#fff', borderRadius: 8 }} title="Template Preview" />
                        ) : (
                          <div style={{ padding: 16, textAlign: 'center', color: '#475569', fontSize: 12 }}>Template HTML not available</div>
                        )}
                      </div>
                    )}
                  </div>
                ) : (
                  <div style={{ padding: '10px', background: 'rgba(251,191,36,0.06)', borderRadius: 8, border: '1px solid rgba(251,191,36,0.15)', fontSize: 12, color: '#fbbf24' }}>
                    No template assigned. Generate a forecast to auto-create templates, or ask Maven to generate one.
                  </div>
                )}
              </div>
            </div>

            {/* Section 4: ISP Quotas */}
            <div>
              <div style={{ fontSize: 13, fontWeight: 600, color: '#a5b4fc', marginBottom: 8, paddingBottom: 6, borderBottom: '1px solid rgba(99,102,241,0.1)' }}>ISP Quotas</div>
              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 8 }}>
                {ISP_BUCKETS.map(({ key, label }) => (
                  <div key={key} style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '6px 10px', background: '#111827', borderRadius: 8, border: '1px solid rgba(99,102,241,0.08)' }}>
                    <span style={{ fontSize: 12, color: '#e2e8f0' }}>{label}</span>
                    <input type="number" value={(editConfig.isp_quotas || {})[key] ?? ''} onChange={e => setEditConfig(c => ({ ...c, isp_quotas: { ...c.isp_quotas, [key]: e.target.value === '' ? 0 : +e.target.value } }))} style={{ width: 80, padding: '4px 8px', background: '#0d1220', border: '1px solid rgba(99,102,241,0.15)', borderRadius: 6, color: '#e2e8f0', fontSize: 12, textAlign: 'right' as const }} />
                  </div>
                ))}
              </div>
              <div style={{ marginTop: 8, padding: '8px 10px', background: 'rgba(99,102,241,0.06)', borderRadius: 6, display: 'flex', justifyContent: 'space-between', alignItems: 'center', fontSize: 12 }}>
                <span style={{ color: '#94a3b8' }}>Total Volume</span>
                <strong style={{ color: '#a5b4fc' }}>{Object.values(editConfig.isp_quotas || {}).reduce((a: number, b: any) => a + (Number(b) || 0), 0).toLocaleString()}</strong>
              </div>
            </div>

            {/* Section 5: Audience & Lists */}
            <div>
              <div style={{ fontSize: 13, fontWeight: 600, color: '#a5b4fc', marginBottom: 8, paddingBottom: 6, borderBottom: '1px solid rgba(99,102,241,0.1)' }}>Audience &amp; Lists</div>
              <div style={{ marginBottom: 10 }}>
                <div style={{ fontSize: 11, color: '#94a3b8', marginBottom: 4, fontWeight: 600 }}>Audience Priority</div>
                <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4 }}>
                  {(editConfig.audience_priority || []).map((tier: string, i: number) => (
                    <span key={i} style={{ fontSize: 11, padding: '3px 8px', borderRadius: 6, background: 'rgba(99,102,241,0.08)', color: '#818cf8', border: '1px solid rgba(99,102,241,0.12)' }}>{i + 1}. {tier}</span>
                  ))}
                </div>
              </div>
              <div style={{ marginBottom: 10 }}>
                <div style={{ fontSize: 11, color: '#94a3b8', marginBottom: 6, fontWeight: 600 }}>Inclusion Lists ({(editConfig.inclusion_lists || []).length})</div>
                {(editConfig.inclusion_lists || []).length > 0 ? (
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
                    {(editConfig.inclusion_lists as any[]).map((item: any, i: number) => (
                      <div key={i} style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '6px 10px', background: '#111827', borderRadius: 6, border: '1px solid rgba(99,102,241,0.08)' }}>
                        <span style={{ fontSize: 10, color: '#64748b', fontWeight: 700, width: 18, textAlign: 'center' as const }}>{i + 1}</span>
                        <div style={{ flex: 1 }}>
                          <div style={{ fontSize: 12, color: '#e2e8f0', fontWeight: 500 }}>{typeof item === 'object' ? item.name : item}</div>
                          {typeof item === 'object' && item.id && <div style={{ fontSize: 10, color: '#475569', marginTop: 1 }}>{item.id}</div>}
                        </div>
                      </div>
                    ))}
                  </div>
                ) : (
                  <div style={{ fontSize: 12, color: '#fbbf24', padding: '8px 10px', background: 'rgba(251,191,36,0.06)', borderRadius: 6, border: '1px solid rgba(251,191,36,0.1)' }}>No inclusion lists — required for deployment</div>
                )}
              </div>
              <div>
                <div style={{ fontSize: 11, color: '#94a3b8', marginBottom: 6, fontWeight: 600 }}>Suppression Lists ({(editConfig.exclusion_lists || []).length})</div>
                {(editConfig.exclusion_lists || []).length > 0 ? (
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
                    {(editConfig.exclusion_lists as any[]).map((item: any, i: number) => (
                      <div key={i} style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '6px 10px', background: '#111827', borderRadius: 6, border: '1px solid rgba(239,68,68,0.08)' }}>
                        <span style={{ fontSize: 12, color: '#e2e8f0' }}>{typeof item === 'object' ? item.name : item}</span>
                      </div>
                    ))}
                  </div>
                ) : (
                  <div style={{ fontSize: 12, color: '#64748b', fontStyle: 'italic' }}>No suppression lists configured</div>
                )}
              </div>
            </div>

            {/* Section 6: AI Reasoning */}
            <div>
              <div style={{ fontSize: 13, fontWeight: 600, color: '#a5b4fc', marginBottom: 8, paddingBottom: 6, borderBottom: '1px solid rgba(99,102,241,0.1)' }}>AI Reasoning</div>
              <div style={{ fontSize: 12, color: '#94a3b8', lineHeight: 1.6, padding: '10px 12px', background: 'rgba(99,102,241,0.04)', borderRadius: 8, border: '1px solid rgba(99,102,241,0.08)' }}>{selectedRec.reasoning}</div>
              <div style={{ marginTop: 10 }}>
                {miniChatMessages.map((m, i) => (
                  <div key={i} style={{ marginBottom: 8, padding: '8px 10px', borderRadius: 8, background: m.role === 'user' ? 'rgba(99,102,241,0.08)' : 'rgba(30,41,59,0.6)', border: `1px solid ${m.role === 'user' ? 'rgba(99,102,241,0.15)' : 'rgba(99,102,241,0.08)'}`, fontSize: 12, color: '#e2e8f0' }}>
                    <div style={{ fontSize: 10, color: '#64748b', marginBottom: 3, fontWeight: 600 }}>{m.role === 'user' ? 'You' : 'Maven'}</div>
                    {m.role === 'assistant' ? <div dangerouslySetInnerHTML={{ __html: simpleMarkdown(m.content) }} /> : m.content}
                  </div>
                ))}
                <div style={{ display: 'flex', gap: 6, marginTop: 4 }}>
                  <input value={miniChatInput} onChange={e => setMiniChatInput(e.target.value)} onKeyDown={e => e.key === 'Enter' && sendMiniChat()} placeholder="Ask Maven about this campaign..." style={{ flex: 1, padding: '8px 12px', background: '#111827', border: '1px solid rgba(99,102,241,0.15)', borderRadius: 8, color: '#e2e8f0', fontSize: 12, outline: 'none' }} />
                  <button onClick={sendMiniChat} disabled={!miniChatInput.trim() || miniChatLoading} style={{ padding: '8px 12px', background: 'linear-gradient(135deg,#6366f1,#8b5cf6)', border: 'none', borderRadius: 8, color: '#fff', cursor: 'pointer', fontSize: 12, opacity: !miniChatInput.trim() || miniChatLoading ? 0.5 : 1 }}>
                    {miniChatLoading ? <FontAwesomeIcon icon={faSpinner} spin /> : <FontAwesomeIcon icon={faPaperPlane} />}
                  </button>
                </div>
              </div>
            </div>

            {/* Section 7: Actions */}
            <div>
              <div style={{ fontSize: 13, fontWeight: 600, color: '#a5b4fc', marginBottom: 8, paddingBottom: 6, borderBottom: '1px solid rgba(99,102,241,0.1)' }}>Deploy</div>
              {approvalResult && (
                <div style={{ marginBottom: 10, padding: '10px 12px', borderRadius: 8, background: approvalResult.error ? 'rgba(239,68,68,0.08)' : 'rgba(34,197,94,0.08)', border: `1px solid ${approvalResult.error ? 'rgba(239,68,68,0.2)' : 'rgba(34,197,94,0.2)'}` }}>
                  {approvalResult.error ? (
                    <div style={{ fontSize: 12, color: '#ef4444' }}>{approvalResult.error}</div>
                  ) : (
                    <div style={{ fontSize: 12 }}>
                      <div style={{ color: '#22c55e', fontWeight: 600, marginBottom: 4 }}>Campaign Deployed Successfully</div>
                      <div style={{ color: '#94a3b8' }}>Campaign ID: <strong style={{ color: '#a5b4fc' }}>{approvalResult.campaign_id}</strong></div>
                      <div style={{ color: '#94a3b8' }}>Scheduled: <strong style={{ color: '#e2e8f0' }}>{approvalResult.scheduled_at ? new Date(approvalResult.scheduled_at).toLocaleString() : 'N/A'}</strong></div>
                      <div style={{ color: '#94a3b8' }}>Audience: <strong style={{ color: '#e2e8f0' }}>{(approvalResult.total_audience || 0).toLocaleString()}</strong></div>
                    </div>
                  )}
                </div>
              )}
              {selectedRec.status === 'pending' ? (
                <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
                  <button onClick={saveRecommendation} disabled={saving} style={{ padding: '10px', background: 'rgba(99,102,241,0.1)', border: '1px solid rgba(99,102,241,0.3)', borderRadius: 8, color: '#a5b4fc', cursor: 'pointer', fontSize: 13, fontWeight: 600, display: 'flex', alignItems: 'center', justifyContent: 'center', gap: 6, opacity: saving ? 0.6 : 1 }}>
                    <FontAwesomeIcon icon={saving ? faSpinner : faSave} spin={saving} /> {saving ? 'Saving...' : 'Save Changes'}
                  </button>
                  <div style={{ fontSize: 11, color: '#64748b', padding: '0 4px', lineHeight: 1.4 }}>
                    Approving will deploy this as a real scheduled campaign. It will be sent at the configured date/time through the PMTA pipeline.
                  </div>
                  <div style={{ display: 'flex', gap: 8 }}>
                    <button onClick={() => approveRec(selectedRec.id)} disabled={approving}
                      style={{ flex: 1, padding: '10px', background: 'linear-gradient(135deg, rgba(34,197,94,0.2), rgba(34,197,94,0.1))', border: '1px solid rgba(34,197,94,0.3)', borderRadius: 8, color: '#22c55e', cursor: 'pointer', fontSize: 13, fontWeight: 600, opacity: approving ? 0.6 : 1 }}>
                      {approving ? <><FontAwesomeIcon icon={faSpinner} spin /> Deploying...</> : <><FontAwesomeIcon icon={faCheck} /> Approve &amp; Deploy</>}
                    </button>
                    <button onClick={() => rejectRec(selectedRec.id)} disabled={approving}
                      style={{ flex: 1, padding: '10px', background: 'rgba(239,68,68,0.1)', border: '1px solid rgba(239,68,68,0.3)', borderRadius: 8, color: '#ef4444', cursor: 'pointer', fontSize: 13, fontWeight: 600 }}>
                      <FontAwesomeIcon icon={faTimes} /> Reject
                    </button>
                  </div>
                </div>
              ) : (
                <div style={{ padding: '12px', background: `${statusColor(selectedRec.status)}10`, borderRadius: 8, border: `1px solid ${statusColor(selectedRec.status)}25` }}>
                  <div style={{ textAlign: 'center' as const }}>
                    <span style={{ fontSize: 13, color: statusColor(selectedRec.status), fontWeight: 600 }}>
                      {selectedRec.status === 'approved' ? 'Deployed — Campaign Scheduled' : selectedRec.status === 'rejected' ? 'Rejected' : selectedRec.status === 'executed' ? 'Executed' : selectedRec.status}
                    </span>
                  </div>
                </div>
              )}
            </div>
          </div>
        </div>
      )}
    </div>
  );
};

// ═══════════════════════════════════════════════════════════════════════════
// STRATEGY SUB-TAB
// ═══════════════════════════════════════════════════════════════════════════
const AgentStrategy: React.FC = () => {
  const [strategies, setStrategies] = useState<DomainStrategy[]>([]);
  const [loading, setLoading] = useState(false);
  const [editing, setEditing] = useState<DomainStrategy | null>(null);
  const [creating, setCreating] = useState(false);
  const [form, setForm] = useState({ sending_domain: '', strategy: 'warmup', daily_volume_increase_pct: 10, max_daily_volume: 500000, audience_priority: 'openers_7d,clickers_14d,engagers_30d,recent_subscribers,cold' });

  useEffect(() => { loadStrategies(); }, []);

  const loadStrategies = async () => {
    setLoading(true);
    try {
      const resp = await fetch('/api/mailing/agent/strategies');
      if (resp.ok) setStrategies(await resp.json());
    } catch {} finally { setLoading(false); }
  };

  const saveStrategy = async () => {
    const params: Record<string, any> = { daily_volume_increase_pct: form.daily_volume_increase_pct, max_daily_volume: form.max_daily_volume, audience_priority: form.audience_priority.split(',').map(s => s.trim()).filter(Boolean) };
    const body = { sending_domain: form.sending_domain, strategy: form.strategy, params };

    if (editing) {
      await fetch(`/api/mailing/agent/strategies/${editing.id}`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ strategy: form.strategy, params }) });
    } else {
      await fetch('/api/mailing/agent/strategies', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
    }
    setEditing(null);
    setCreating(false);
    loadStrategies();
  };

  const deleteStrategy = async (id: string) => {
    await fetch(`/api/mailing/agent/strategies/${id}`, { method: 'DELETE' });
    loadStrategies();
  };

  const openEdit = (s: DomainStrategy) => {
    setEditing(s);
    setCreating(false);
    setForm({
      sending_domain: s.sending_domain, strategy: s.strategy,
      daily_volume_increase_pct: s.params?.daily_volume_increase_pct || 10,
      max_daily_volume: s.params?.max_daily_volume || 500000,
      audience_priority: (s.params?.audience_priority || ['openers_7d', 'clickers_14d', 'engagers_30d', 'recent_subscribers', 'cold']).join(','),
    });
  };

  const openCreate = () => {
    setCreating(true);
    setEditing(null);
    setForm({ sending_domain: '', strategy: 'warmup', daily_volume_increase_pct: 10, max_daily_volume: 500000, audience_priority: 'openers_7d,clickers_14d,engagers_30d,recent_subscribers,cold' });
  };

  const inputStyle = { padding: '8px 12px', background: '#111827', border: '1px solid rgba(99,102,241,0.2)', borderRadius: 8, color: '#e2e8f0', fontSize: 13, width: '100%' };
  const labelStyle = { display: 'block', fontSize: 11, color: '#94a3b8', marginBottom: 4, fontWeight: 600 as const };

  return (
    <div style={{ flex: 1, overflow: 'auto', padding: 20 }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
        <div>
          <h3 style={{ margin: 0, fontSize: 16, color: '#e2e8f0' }}>Domain Strategies</h3>
          <p style={{ margin: '4px 0 0', fontSize: 12, color: '#64748b' }}>Configure warmup or performance strategies for each sending domain.</p>
        </div>
        <button onClick={openCreate} style={{ padding: '8px 16px', background: 'linear-gradient(135deg,#6366f1,#8b5cf6)', border: 'none', borderRadius: 8, color: '#fff', cursor: 'pointer', fontSize: 13, fontWeight: 600, display: 'flex', alignItems: 'center', gap: 6 }}>
          <FontAwesomeIcon icon={faPlus} /> Add Strategy
        </button>
      </div>

      {loading && <div style={{ textAlign: 'center', padding: 40, color: '#64748b' }}><FontAwesomeIcon icon={faSpinner} spin /></div>}

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(300px, 1fr))', gap: 12 }}>
        {strategies.map(s => (
          <div key={s.id} style={{ background: '#0d1220', borderRadius: 12, padding: 16, border: '1px solid rgba(99,102,241,0.1)' }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 10 }}>
              <span style={{ fontWeight: 600, fontSize: 14, color: '#e2e8f0' }}>{s.sending_domain}</span>
              <span style={{ fontSize: 11, padding: '3px 10px', borderRadius: 6, fontWeight: 600, background: s.strategy === 'warmup' ? 'rgba(251,191,36,0.1)' : 'rgba(52,211,153,0.1)', color: s.strategy === 'warmup' ? '#fbbf24' : '#34d399', border: `1px solid ${s.strategy === 'warmup' ? 'rgba(251,191,36,0.2)' : 'rgba(52,211,153,0.2)'}` }}>
                <FontAwesomeIcon icon={s.strategy === 'warmup' ? faArrowUp : faChartLine} style={{ marginRight: 4 }} />{s.strategy}
              </span>
            </div>
            {s.params?.daily_volume_increase_pct && <div style={{ fontSize: 12, color: '#94a3b8', marginBottom: 4 }}>Volume increase: <strong>{s.params.daily_volume_increase_pct}%/day</strong></div>}
            {s.params?.max_daily_volume && <div style={{ fontSize: 12, color: '#94a3b8', marginBottom: 4 }}>Max volume: <strong>{s.params.max_daily_volume.toLocaleString()}</strong></div>}
            {s.params?.audience_priority && (
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: 3, marginTop: 6 }}>
                {(s.params.audience_priority as string[]).map((tier, i) => (
                  <span key={i} style={{ fontSize: 10, padding: '2px 6px', borderRadius: 4, background: 'rgba(99,102,241,0.08)', color: '#818cf8' }}>{i + 1}. {tier}</span>
                ))}
              </div>
            )}
            <div style={{ display: 'flex', gap: 6, marginTop: 10 }}>
              <button onClick={() => openEdit(s)} style={{ flex: 1, padding: '6px', background: 'rgba(99,102,241,0.08)', border: '1px solid rgba(99,102,241,0.2)', borderRadius: 6, color: '#a5b4fc', cursor: 'pointer', fontSize: 11, fontWeight: 600 }}>Edit</button>
              <button onClick={() => deleteStrategy(s.id)} style={{ padding: '6px 10px', background: 'rgba(239,68,68,0.08)', border: '1px solid rgba(239,68,68,0.2)', borderRadius: 6, color: '#ef4444', cursor: 'pointer', fontSize: 11 }}>
                <FontAwesomeIcon icon={faTrash} />
              </button>
            </div>
          </div>
        ))}
      </div>

      {strategies.length === 0 && !loading && (
        <div style={{ textAlign: 'center', padding: 60, color: '#475569' }}>
          <FontAwesomeIcon icon={faCogs} style={{ fontSize: 32, marginBottom: 12 }} />
          <div style={{ fontSize: 14 }}>No domain strategies configured yet.</div>
          <div style={{ fontSize: 12, marginTop: 4, color: '#334155' }}>Click "Add Strategy" to set up warmup or performance goals for a sending domain.</div>
        </div>
      )}

      {/* Create/Edit modal */}
      {(creating || editing) && (
        <div style={{ position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.6)', zIndex: 200, display: 'flex', alignItems: 'center', justifyContent: 'center' }} onClick={() => { setCreating(false); setEditing(null); }}>
          <div onClick={e => e.stopPropagation()} style={{ background: '#0f1629', borderRadius: 14, padding: 24, width: 440, border: '1px solid rgba(99,102,241,0.2)' }}>
            <h3 style={{ margin: '0 0 16px', fontSize: 16, color: '#e2e8f0' }}>{editing ? 'Edit' : 'New'} Domain Strategy</h3>
            <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
              <div>
                <label style={labelStyle}>Sending Domain</label>
                <input style={inputStyle} value={form.sending_domain} onChange={e => setForm(f => ({ ...f, sending_domain: e.target.value }))} disabled={!!editing} placeholder="em.example.com" />
              </div>
              <div>
                <label style={labelStyle}>Strategy</label>
                <select style={inputStyle} value={form.strategy} onChange={e => setForm(f => ({ ...f, strategy: e.target.value }))}>
                  <option value="warmup">Warmup — Increase volume, prioritize engaged</option>
                  <option value="performance">Performance — Monetize engaged audience</option>
                </select>
              </div>
              <div>
                <label style={labelStyle}>Daily Volume Increase %</label>
                <input style={inputStyle} type="number" value={form.daily_volume_increase_pct} onChange={e => setForm(f => ({ ...f, daily_volume_increase_pct: +e.target.value }))} />
              </div>
              <div>
                <label style={labelStyle}>Max Daily Volume</label>
                <input style={inputStyle} type="number" value={form.max_daily_volume} onChange={e => setForm(f => ({ ...f, max_daily_volume: +e.target.value }))} />
              </div>
              <div>
                <label style={labelStyle}>Audience Priority (comma-separated)</label>
                <input style={inputStyle} value={form.audience_priority} onChange={e => setForm(f => ({ ...f, audience_priority: e.target.value }))} placeholder="openers_7d,clickers_14d,engagers_30d,recent_subscribers,cold" />
              </div>
            </div>
            <div style={{ display: 'flex', gap: 10, marginTop: 20, justifyContent: 'flex-end' }}>
              <button onClick={() => { setCreating(false); setEditing(null); }} style={{ padding: '8px 16px', background: 'transparent', border: '1px solid rgba(99,102,241,0.2)', borderRadius: 8, color: '#94a3b8', cursor: 'pointer', fontSize: 13 }}>Cancel</button>
              <button onClick={saveStrategy} style={{ padding: '8px 20px', background: 'linear-gradient(135deg,#6366f1,#8b5cf6)', border: 'none', borderRadius: 8, color: '#fff', cursor: 'pointer', fontSize: 13, fontWeight: 600 }}>Save</button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
};

// ═══════════════════════════════════════════════════════════════════════════
// MAIN CONTAINER
// ═══════════════════════════════════════════════════════════════════════════
export const EmailMarketingAgent: React.FC = () => {
  const [activeSubTab, setActiveSubTab] = useState<SubTab>('chat');

  const renderContent = () => {
    switch (activeSubTab) {
      case 'chat': return <AgentChat />;
      case 'calendar': return <AgentCalendar />;
      case 'strategy': return <AgentStrategy />;
    }
  };

  return (
    <div style={S.container}>
      <div style={S.header}>
        <div style={S.headerIcon}><FontAwesomeIcon icon={faRobot} /></div>
        <div>
          <h2 style={S.headerTitle}>Maven — Email Marketing Agent</h2>
          <div style={S.headerSubtitle}>AI strategist for ISP warmup, campaign planning & audience monetization</div>
        </div>
        <span style={{ marginLeft: 'auto', fontSize: 10, color: '#475569' }}>v{PAGE_VERSION}</span>
      </div>
      <div style={S.tabs}>
        <button style={S.tab(activeSubTab === 'chat')} onClick={() => setActiveSubTab('chat')}>
          <FontAwesomeIcon icon={faComments} /> Chat
        </button>
        <button style={S.tab(activeSubTab === 'calendar')} onClick={() => setActiveSubTab('calendar')}>
          <FontAwesomeIcon icon={faCalendarAlt} /> Marketing Calendar
        </button>
        <button style={S.tab(activeSubTab === 'strategy')} onClick={() => setActiveSubTab('strategy')}>
          <FontAwesomeIcon icon={faCogs} /> Domain Strategy
        </button>
      </div>
      <div style={S.body}>
        {renderContent()}
      </div>
    </div>
  );
};
