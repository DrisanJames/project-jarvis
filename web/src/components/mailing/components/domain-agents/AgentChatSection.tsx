// Domain Agents — AGENT CHAT section.
// Conversational scheduling copilot: full Domain Agent lifecycle (scorecard →
// briefing → plan → approve-to-deploy) + the Campaign Copilot action set
// (clone / deploy / emergency-stop) behind a chat box. Backed by
// POST /api/mailing/domain-agent/chat (domain_agent_chat.go). Irreversible
// actions are confirmation-gated server-side: the agent will ask you to type
// the sending domain (approve) or explicitly confirm (deploy/stop).

import React, { useCallback, useEffect, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faPaperPlane, faSpinner, faRobot, faPlus, faBolt } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../../shared/apiFetch';
import { C, panelStyle, sectionTitleStyle } from './ui';

interface ChatMsg {
  role: 'user' | 'assistant';
  content: string;
  actions?: string[];
}

// Minimal markdown: bold, inline code, headings, bullet lines. Same approach
// as EmailMarketingAgent's renderer — keeps the bundle free of a md library.
function renderMarkdown(text: string): string {
  const esc = text
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  return esc
    .replace(/^### (.*)$/gm, '<div style="font-weight:700;margin:6px 0 2px">$1</div>')
    .replace(/^## (.*)$/gm, '<div style="font-weight:700;font-size:14px;margin:8px 0 2px">$1</div>')
    .replace(/\*\*(.+?)\*\*/g, '<b>$1</b>')
    .replace(/`([^`]+)`/g, '<code style="background:rgba(0,0,0,0.35);padding:1px 5px;border-radius:3px;font-size:12px">$1</code>')
    .replace(/^[-•] (.*)$/gm, '<div style="padding-left:14px;text-indent:-10px">• $1</div>')
    .replace(/\n/g, '<br/>');
}

export const AgentChatSection: React.FC<{ domain: string | null }> = ({ domain }) => {
  const [messages, setMessages] = useState<ChatMsg[]>([]);
  const [input, setInput] = useState('');
  const [busy, setBusy] = useState(false);
  const [convoID, setConvoID] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLTextAreaElement>(null);

  useEffect(() => {
    scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight, behavior: 'smooth' });
  }, [messages, busy]);

  const send = useCallback(async (text: string) => {
    const message = text.trim();
    if (!message || busy) return;
    setInput('');
    setError(null);
    setMessages(m => [...m, { role: 'user', content: message }]);
    setBusy(true);
    try {
      const res = await apiFetch('/api/mailing/domain-agent/chat/', {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ message, conversation_id: convoID ?? undefined }),
      });
      const data = await res.json();
      if (!res.ok || data.error) {
        setError(data.error ?? `HTTP ${res.status}`);
      } else {
        if (data.conversation_id) setConvoID(data.conversation_id);
        setMessages(m => [...m, { role: 'assistant', content: data.response ?? '', actions: data.actions_taken ?? [] }]);
      }
    } catch (err) {
      setError(String(err));
    } finally {
      setBusy(false);
      inputRef.current?.focus();
    }
  }, [busy, convoID]);

  const newChat = () => { setMessages([]); setConvoID(null); setError(null); };

  const suggestions = domain ? [
    `How is ${domain} performing this week?`,
    `Draft today's plan for ${domain}`,
    `What's scheduled on ${domain} in the next 48h?`,
    `Clone yesterday's best campaign on ${domain} for 9am tomorrow`,
  ] : [
    'Which domain has the worst hard-bounce rate this week?',
    "What's scheduled across all domains in the next 48h?",
    'Rank domains by human opens over 7 days',
  ];

  return (
    <div style={{ ...panelStyle, display: 'flex', flexDirection: 'column' }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 8 }}>
        <div style={sectionTitleStyle}>
          <FontAwesomeIcon icon={faRobot} style={{ marginRight: 6, color: C.accent }} />
          AGENT CHAT
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <span style={{ fontSize: 11.5, color: C.muted }}>
            plans · slots · clone · deploy · stop — deploys require your explicit confirmation
          </span>
          {messages.length > 0 && (
            <button onClick={newChat} style={ghostBtn} title="Start a new conversation">
              <FontAwesomeIcon icon={faPlus} /> New
            </button>
          )}
        </div>
      </div>

      {messages.length === 0 && (
        <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', marginBottom: 10 }}>
          {suggestions.map(s => (
            <button key={s} onClick={() => send(s)} style={chipStyle} disabled={busy}>
              <FontAwesomeIcon icon={faBolt} style={{ fontSize: 10, marginRight: 5, color: C.accent2 }} />{s}
            </button>
          ))}
        </div>
      )}

      {messages.length > 0 && (
        <div ref={scrollRef} style={{ maxHeight: 420, overflowY: 'auto', display: 'flex', flexDirection: 'column', gap: 10, padding: '4px 2px', marginBottom: 10 }}>
          {messages.map((m, i) => (
            <div key={i} style={{ display: 'flex', justifyContent: m.role === 'user' ? 'flex-end' : 'flex-start' }}>
              <div style={m.role === 'user' ? userBubble : agentBubble}>
                {m.role === 'assistant'
                  ? <div dangerouslySetInnerHTML={{ __html: renderMarkdown(m.content) }} />
                  : m.content}
                {m.actions && m.actions.length > 0 && (
                  <div style={{ marginTop: 8, display: 'flex', flexWrap: 'wrap', gap: 4 }}>
                    {m.actions.map((a, j) => (
                      <span key={j} style={actionBadge}>⚡ {a}</span>
                    ))}
                  </div>
                )}
              </div>
            </div>
          ))}
          {busy && (
            <div style={{ ...agentBubble, color: C.muted, display: 'inline-flex', alignItems: 'center', gap: 8, alignSelf: 'flex-start' }}>
              <FontAwesomeIcon icon={faSpinner} spin /> working — gathering data and planning…
            </div>
          )}
        </div>
      )}

      {error && (
        <div style={{ color: C.danger, fontSize: 12.5, marginBottom: 8 }}>{error}</div>
      )}

      <div style={{ display: 'flex', gap: 8, alignItems: 'flex-end' }}>
        <textarea
          ref={inputRef}
          value={input}
          onChange={e => setInput(e.target.value)}
          onKeyDown={e => {
            if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send(input); }
          }}
          placeholder={domain ? `Ask about ${domain}, draft a plan, clone & schedule a campaign…` : 'Ask about any domain, schedule, or campaign…'}
          rows={input.split('\n').length > 2 ? 3 : 1}
          style={inputStyle}
          disabled={busy}
        />
        <button onClick={() => send(input)} disabled={busy || !input.trim()} style={sendBtn}>
          <FontAwesomeIcon icon={busy ? faSpinner : faPaperPlane} spin={busy} />
        </button>
      </div>
    </div>
  );
};

const userBubble: React.CSSProperties = {
  background: 'linear-gradient(135deg, #6366f1 0%, #4f46e5 100%)',
  color: 'white', padding: '9px 13px', borderRadius: '12px 12px 2px 12px',
  maxWidth: '78%', fontSize: 13.5, lineHeight: 1.5, whiteSpace: 'pre-wrap',
};
const agentBubble: React.CSSProperties = {
  background: 'rgba(0,0,0,0.3)', border: `1px solid ${C.panelBorder}`,
  color: C.text, padding: '9px 13px', borderRadius: '12px 12px 12px 2px',
  maxWidth: '88%', fontSize: 13.5, lineHeight: 1.55,
};
const actionBadge: React.CSSProperties = {
  fontSize: 11, background: 'rgba(0,229,255,0.10)', color: C.accent,
  border: '1px solid rgba(0,229,255,0.25)', borderRadius: 10, padding: '2px 8px',
};
const chipStyle: React.CSSProperties = {
  background: 'rgba(0,0,0,0.25)', color: C.text, border: `1px solid ${C.panelBorder}`,
  borderRadius: 14, padding: '6px 12px', fontSize: 12.5, cursor: 'pointer',
};
const inputStyle: React.CSSProperties = {
  flex: 1, background: 'rgba(0,0,0,0.3)', color: C.text,
  border: `1px solid ${C.panelBorder}`, borderRadius: 8, padding: '10px 12px',
  fontSize: 13.5, resize: 'none', outline: 'none', fontFamily: 'inherit', lineHeight: 1.4,
};
const sendBtn: React.CSSProperties = {
  background: 'linear-gradient(135deg, #6366f1 0%, #8b5cf6 100%)', color: 'white',
  border: 'none', borderRadius: 8, padding: '10px 16px', cursor: 'pointer', fontSize: 14,
};
const ghostBtn: React.CSSProperties = {
  background: 'transparent', color: C.accent, border: `1px solid rgba(0,229,255,0.3)`,
  borderRadius: 6, padding: '3px 10px', cursor: 'pointer', fontSize: 12,
};
