import React, { useState, useRef, useEffect, useCallback } from 'react';
import { apiFetch } from '../shared/apiFetch';

interface CopilotMessage {
  role: 'user' | 'assistant';
  content: string;
  timestamp: Date;
  suggestions?: string[];
  actionsTaken?: string[];
}

interface ChatHistoryEntry {
  role: 'user' | 'assistant';
  content: string;
}

type CopilotModel = 'claude-fable-5' | 'claude-opus-5';

const MODEL_OPTIONS: { value: CopilotModel; label: string }[] = [
  { value: 'claude-fable-5', label: 'Fable 5' },
  { value: 'claude-opus-5', label: 'Opus 5' },
];

interface SchedulerCommand {
  id: string;
  command: string;
  status: string;
  requested_by: string;
  output_tail: string;
  exit_code?: number;
  created_at: string;
  updated_at: string;
}

const COMMAND_STATUS_COLORS: Record<string, { bg: string; fg: string }> = {
  queued: { bg: '#3b2f14', fg: '#f59e0b' },
  running: { bg: '#14243b', fg: '#60a5fa' },
  completed: { bg: '#12301f', fg: '#22c55e' },
  done: { bg: '#12301f', fg: '#22c55e' },
  failed: { bg: '#3a1518', fg: '#ef4444' },
  error: { bg: '#3a1518', fg: '#ef4444' },
};

const INITIAL_MESSAGE: CopilotMessage = {
  role: 'assistant',
  content: `**Campaign Copilot** ready. I can help you build, clone, schedule, and manage campaigns using natural language.\n\n**Try asking:**\n- "Show me my scheduled campaigns"\n- "Clone the last Discount Blog campaign for tomorrow 6am MST"\n- "What were our Gmail open rates this week?"\n- "Create a segment for everyone mailed via quizfiesta"`,
  timestamp: new Date(),
  suggestions: [
    'Show me scheduled campaigns',
    'List all mailing lists',
    'Show mailbox provider performance',
    'What templates do we have?',
  ],
};

interface CampaignCopilotProps {
  isOpen: boolean;
  onClose: () => void;
  /** Render in page flow (a portal tab) instead of the slide-over drawer. */
  inline?: boolean;
}

export const CampaignCopilot: React.FC<CampaignCopilotProps> = ({ isOpen, onClose, inline }) => {
  const [messages, setMessages] = useState<CopilotMessage[]>([INITIAL_MESSAGE]);
  const [input, setInput] = useState('');
  const [loading, setLoading] = useState(false);
  const [history, setHistory] = useState<ChatHistoryEntry[]>([]);
  const [model, setModel] = useState<CopilotModel>('claude-fable-5');
  const [commands, setCommands] = useState<SchedulerCommand[]>([]);
  const messagesEndRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  const fetchCommands = useCallback(async () => {
    try {
      const resp = await apiFetch('/api/mailing/copilot/commands?limit=10');
      if (!resp.ok) return;
      const data = await resp.json();
      setCommands(Array.isArray(data.commands) ? data.commands : []);
    } catch {
      // status strip is best-effort; ignore fetch errors
    }
  }, []);

  useEffect(() => {
    if (isOpen) fetchCommands();
  }, [isOpen, fetchCommands]);

  const hasActiveCommand = commands.some(c => c.status === 'queued' || c.status === 'running');
  useEffect(() => {
    if (!isOpen || !hasActiveCommand) return;
    const timer = setInterval(fetchCommands, 10_000);
    return () => clearInterval(timer);
  }, [isOpen, hasActiveCommand, fetchCommands]);

  const scrollToBottom = useCallback(() => {
    messagesEndRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, []);

  useEffect(() => {
    scrollToBottom();
  }, [messages, scrollToBottom]);

  useEffect(() => {
    if (isOpen) {
      setTimeout(() => inputRef.current?.focus(), 200);
    }
  }, [isOpen]);

  const sendMessage = useCallback(async (text: string) => {
    if (!text.trim() || loading) return;

    const userMsg = text.trim();
    setInput('');
    setLoading(true);

    setMessages(prev => [...prev, { role: 'user', content: userMsg, timestamp: new Date() }]);
    const updatedHistory: ChatHistoryEntry[] = [...history, { role: 'user', content: userMsg }];

    try {
      const resp = await apiFetch('/api/mailing/copilot/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ message: userMsg, history: updatedHistory, model }),
      });
      const data = await resp.json();

      if (!resp.ok) {
        throw new Error(data.error || 'Request failed');
      }

      const assistantMsg: CopilotMessage = {
        role: 'assistant',
        content: data.response || 'No response received.',
        timestamp: new Date(),
        suggestions: data.suggestions,
        actionsTaken: data.actions_taken,
      };
      setMessages(prev => [...prev, assistantMsg]);
      setHistory([...updatedHistory, { role: 'assistant', content: data.response }]);
      // The agent may have queued scheduler-bridge commands this turn.
      fetchCommands();
    } catch (err: unknown) {
      const errorMsg = err instanceof Error ? err.message : 'Unknown error';
      setMessages(prev => [...prev, {
        role: 'assistant',
        content: `**Error:** ${errorMsg}`,
        timestamp: new Date(),
      }]);
    } finally {
      setLoading(false);
    }
  }, [loading, history, model, fetchCommands]);

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    sendMessage(input);
  };

  const handleSuggestionClick = (suggestion: string) => {
    sendMessage(suggestion);
  };

  if (!isOpen) return null;

  const containerStyle: React.CSSProperties = inline
    ? { display: 'flex', justifyContent: 'stretch' }
    : styles.overlay;
  const panelStyle: React.CSSProperties = inline
    ? { ...styles.panel, width: '100%', maxWidth: 900, height: 'calc(100vh - 210px)', minHeight: 480,
        border: '1px solid #1e293b', borderRadius: 12, boxShadow: 'none', margin: '0 auto' }
    : styles.panel;

  return (
    <div style={containerStyle} onClick={inline ? undefined : onClose}>
      <div style={panelStyle} onClick={e => e.stopPropagation()}>
        <div style={styles.header}>
          <div style={styles.headerLeft}>
            <div style={styles.botIcon}>AI</div>
            <div>
              <div style={styles.headerTitle}>Campaign Copilot</div>
              <div style={styles.headerSub}>AI-powered campaign management</div>
            </div>
          </div>
          <div style={styles.headerRight}>
            <select
              value={model}
              onChange={e => setModel(e.target.value as CopilotModel)}
              style={styles.modelSelect}
              title="Model used for each chat turn"
            >
              {MODEL_OPTIONS.map(opt => (
                <option key={opt.value} value={opt.value}>{opt.label}</option>
              ))}
            </select>
            <button style={styles.closeBtn} onClick={onClose} title="Close">&times;</button>
          </div>
        </div>

        {commands.length > 0 && (
          <div style={styles.commandStrip}>
            {commands.slice(0, 5).map(cmd => {
              const chip = COMMAND_STATUS_COLORS[cmd.status] || { bg: '#1e293b', fg: '#94a3b8' };
              return (
                <details key={cmd.id} style={styles.commandRow}>
                  <summary style={styles.commandSummary}>
                    <span style={styles.commandName}>{cmd.command}</span>
                    <span style={{ ...styles.commandChip, background: chip.bg, color: chip.fg }}>
                      {cmd.status}{typeof cmd.exit_code === 'number' ? ` (${cmd.exit_code})` : ''}
                    </span>
                    <span style={styles.commandTime}>{new Date(cmd.updated_at).toLocaleTimeString()}</span>
                  </summary>
                  <pre style={styles.commandOutput}>{cmd.output_tail || '(no output yet)'}</pre>
                </details>
              );
            })}
          </div>
        )}

        <div style={styles.messages}>
          {messages.map((msg, i) => (
            <div key={i} style={msg.role === 'user' ? styles.userRow : styles.assistantRow}>
              <div style={msg.role === 'user' ? styles.userBubble : styles.assistantBubble}>
                <MarkdownContent content={msg.content} />
                {msg.actionsTaken && msg.actionsTaken.length > 0 && (
                  <div style={styles.actionsBar}>
                    {msg.actionsTaken.map((a, j) => (
                      <span key={j} style={styles.actionChip}>{a}</span>
                    ))}
                  </div>
                )}
              </div>
              {msg.suggestions && msg.suggestions.length > 0 && (
                <div style={styles.suggestions}>
                  {msg.suggestions.map((s, j) => (
                    <button key={j} style={styles.suggestionBtn} onClick={() => handleSuggestionClick(s)}>{s}</button>
                  ))}
                </div>
              )}
            </div>
          ))}
          {loading && (
            <div style={styles.assistantRow}>
              <div style={styles.assistantBubble}>
                <div style={styles.typingDots}>
                  <span style={styles.dot} />
                  <span style={{ ...styles.dot, animationDelay: '0.2s' }} />
                  <span style={{ ...styles.dot, animationDelay: '0.4s' }} />
                </div>
              </div>
            </div>
          )}
          <div ref={messagesEndRef} />
        </div>

        <form onSubmit={handleSubmit} style={styles.inputBar}>
          <input
            ref={inputRef}
            type="text"
            value={input}
            onChange={e => setInput(e.target.value)}
            placeholder="Ask about campaigns, lists, segments, templates..."
            style={styles.input}
            disabled={loading}
          />
          <button type="submit" style={styles.sendBtn} disabled={loading || !input.trim()}>
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
              <line x1="22" y1="2" x2="11" y2="13" />
              <polygon points="22 2 15 22 11 13 2 9 22 2" />
            </svg>
          </button>
        </form>
      </div>
    </div>
  );
};

const MarkdownContent: React.FC<{ content: string }> = ({ content }) => {
  const html = simpleMarkdown(content);
  return <div dangerouslySetInnerHTML={{ __html: html }} style={{ lineHeight: 1.55, fontSize: 13 }} />;
};

function simpleMarkdown(text: string): string {
  let out = text
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;');

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
  out = out.replace(/\n/g, '<br/>');

  return out;
}

const styles: Record<string, React.CSSProperties> = {
  overlay: {
    position: 'fixed',
    inset: 0,
    background: 'rgba(0,0,0,0.4)',
    zIndex: 9999,
    display: 'flex',
    justifyContent: 'flex-end',
  },
  panel: {
    width: 480,
    maxWidth: '100vw',
    height: '100vh',
    background: '#0f1729',
    display: 'flex',
    flexDirection: 'column',
    borderLeft: '1px solid #1e293b',
    boxShadow: '-8px 0 32px rgba(0,0,0,0.5)',
  },
  header: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    padding: '14px 16px',
    borderBottom: '1px solid #1e293b',
    background: '#0a1020',
  },
  headerLeft: { display: 'flex', alignItems: 'center', gap: 10 },
  headerRight: { display: 'flex', alignItems: 'center', gap: 8 },
  modelSelect: {
    background: '#1a2236',
    border: '1px solid #2a3550',
    borderRadius: 6,
    color: '#a5b4fc',
    fontSize: 12,
    padding: '5px 8px',
    outline: 'none',
    cursor: 'pointer',
  },
  commandStrip: {
    borderBottom: '1px solid #1e293b',
    background: '#0b1322',
    padding: '6px 14px',
    display: 'flex',
    flexDirection: 'column',
    gap: 4,
    maxHeight: 180,
    overflowY: 'auto',
  },
  commandRow: {
    fontSize: 12,
    color: '#cbd5e1',
  },
  commandSummary: {
    display: 'flex',
    alignItems: 'center',
    gap: 8,
    cursor: 'pointer',
    listStyle: 'none',
  },
  commandName: {
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
    fontSize: 12,
    color: '#e2e8f0',
  },
  commandChip: {
    fontSize: 10,
    fontWeight: 600,
    padding: '1px 8px',
    borderRadius: 10,
    textTransform: 'uppercase' as const,
    letterSpacing: 0.4,
  },
  commandTime: {
    marginLeft: 'auto',
    fontSize: 10,
    color: '#64748b',
  },
  commandOutput: {
    margin: '4px 0 6px',
    padding: '6px 8px',
    background: '#0d1117',
    border: '1px solid #1e293b',
    borderRadius: 6,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
    fontSize: 11,
    lineHeight: 1.45,
    color: '#94a3b8',
    whiteSpace: 'pre-wrap' as const,
    overflowX: 'auto',
    maxHeight: 120,
    overflowY: 'auto',
  },
  botIcon: {
    width: 34,
    height: 34,
    borderRadius: 8,
    background: 'linear-gradient(135deg, #6366f1, #8b5cf6)',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    fontSize: 12,
    fontWeight: 700,
    color: '#fff',
  },
  headerTitle: { fontSize: 15, fontWeight: 600, color: '#e2e8f0' },
  headerSub: { fontSize: 11, color: '#64748b' },
  closeBtn: {
    background: 'none',
    border: 'none',
    color: '#94a3b8',
    fontSize: 22,
    cursor: 'pointer',
    padding: '4px 8px',
    borderRadius: 4,
    lineHeight: 1,
  },
  messages: {
    flex: 1,
    overflowY: 'auto',
    padding: '12px 14px',
    display: 'flex',
    flexDirection: 'column',
    gap: 12,
  },
  userRow: { display: 'flex', justifyContent: 'flex-end' },
  assistantRow: { display: 'flex', flexDirection: 'column', alignItems: 'flex-start' },
  userBubble: {
    maxWidth: '85%',
    background: '#312e81',
    color: '#e0e7ff',
    padding: '8px 12px',
    borderRadius: '12px 12px 2px 12px',
    fontSize: 13,
    lineHeight: 1.5,
  },
  assistantBubble: {
    maxWidth: '92%',
    background: '#1a2236',
    color: '#cbd5e1',
    padding: '10px 14px',
    borderRadius: '12px 12px 12px 2px',
    border: '1px solid #1e293b',
  },
  actionsBar: {
    marginTop: 8,
    display: 'flex',
    flexWrap: 'wrap' as const,
    gap: 4,
  },
  actionChip: {
    fontSize: 10,
    padding: '2px 8px',
    borderRadius: 10,
    background: '#1e3a5f',
    color: '#7dd3fc',
    fontWeight: 500,
  },
  suggestions: {
    display: 'flex',
    flexWrap: 'wrap' as const,
    gap: 6,
    marginTop: 8,
  },
  suggestionBtn: {
    fontSize: 11,
    padding: '5px 10px',
    borderRadius: 14,
    border: '1px solid #334155',
    background: '#0f172a',
    color: '#a5b4fc',
    cursor: 'pointer',
    transition: 'all 0.15s',
  },
  inputBar: {
    display: 'flex',
    alignItems: 'center',
    padding: '10px 14px',
    borderTop: '1px solid #1e293b',
    background: '#0a1020',
    gap: 8,
  },
  input: {
    flex: 1,
    background: '#1a2236',
    border: '1px solid #2a3550',
    borderRadius: 8,
    padding: '10px 12px',
    color: '#e2e8f0',
    fontSize: 13,
    outline: 'none',
  },
  sendBtn: {
    width: 38,
    height: 38,
    borderRadius: 8,
    background: 'linear-gradient(135deg, #6366f1, #8b5cf6)',
    border: 'none',
    color: '#fff',
    cursor: 'pointer',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    opacity: 1,
  },
  typingDots: {
    display: 'flex',
    gap: 4,
    padding: '4px 0',
  },
  dot: {
    width: 6,
    height: 6,
    borderRadius: '50%',
    background: '#6366f1',
    animation: 'copilotPulse 1.2s ease-in-out infinite',
  },
};

const styleSheet = document.createElement('style');
styleSheet.textContent = `
@keyframes copilotPulse {
  0%, 100% { opacity: 0.3; transform: scale(0.8); }
  50% { opacity: 1; transform: scale(1.2); }
}
.copilot-suggestion:hover {
  background: #1e293b !important;
  border-color: #6366f1 !important;
}
`;
if (!document.getElementById('copilot-styles')) {
  styleSheet.id = 'copilot-styles';
  document.head.appendChild(styleSheet);
}
