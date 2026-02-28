import React, { useState, useRef, useEffect } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faPaperPlane, faRobot, faUser, faMagic } from '@fortawesome/free-solid-svg-icons';
import { Card, CardHeader, CardBody, Loading } from '../common';

interface Message {
  role: 'user' | 'agent';
  content: string;
  timestamp: Date;
  suggestions?: string[];
}

interface ChatHistoryMessage {
  role: string;
  content: string;
}

export const AgentChat: React.FC = () => {
  const [messages, setMessages] = useState<Message[]>([
    {
      role: 'agent',
      content: "👋 Hello! I'm your AI-powered email analytics assistant. I have access to **all your data** across Everflow, Ongage, SparkPost, Mailgun, and SES.\n\n**Ask me anything about:**\n• 💰 Revenue, conversions, and offer performance\n• 📧 Campaign performance and send times\n• 📊 Deliverability across ISPs and providers\n• 🎯 Subject line effectiveness\n• ⚠️ Issues and recommendations\n\nJust type naturally - I'll fetch the data and give you insights!",
      timestamp: new Date(),
      suggestions: [
        "What's our revenue today?",
        'How are campaigns performing?',
        'Any concerns I should know about?',
        'Compare ESP provider performance',
      ],
    },
  ]);
  const [input, setInput] = useState('');
  const [loading, setLoading] = useState(false);
  const [isAIPowered, setIsAIPowered] = useState<boolean | null>(null);
  const [chatHistory, setChatHistory] = useState<ChatHistoryMessage[]>([]);
  const messagesEndRef = useRef<HTMLDivElement>(null);

  const scrollToBottom = () => {
    messagesEndRef.current?.scrollIntoView({ behavior: 'smooth' });
  };

  useEffect(() => {
    scrollToBottom();
  }, [messages]);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!input.trim() || loading) return;

    const userMessage = input.trim();
    setInput('');
    setLoading(true);

    // Add user message to display
    setMessages((prev) => [
      ...prev,
      {
        role: 'user',
        content: userMessage,
        timestamp: new Date(),
      },
    ]);

    try {
      // Send to API with conversation history for context
      const response = await fetch('/api/agent/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          message: userMessage,
          history: chatHistory,
        }),
      });

      const result = await response.json();

      if (response.ok && result.response) {
        // Update AI powered status
        if (result.ai_powered !== undefined) {
          setIsAIPowered(result.ai_powered);
        }

        // Add agent response
        setMessages((prev) => [
          ...prev,
          {
            role: 'agent',
            content: result.response.message,
            timestamp: new Date(),
            suggestions: result.response.suggestions,
          },
        ]);

        // Update conversation history for future context
        setChatHistory((prev) => [
          ...prev,
          { role: 'user', content: userMessage },
          { role: 'assistant', content: result.response.message },
        ]);
      } else {
        throw new Error(result.error || 'Unknown error');
      }
    } catch (err) {
      const errorMessage = err instanceof Error ? err.message : 'Unknown error';
      let displayMessage = "I'm sorry, I encountered an error processing your request.";
      
      if (errorMessage.includes('AI agent not configured')) {
        displayMessage = "**AI Agent Not Configured**\n\nTo enable the conversational AI, please add your OpenAI API key to `config/config.yaml`:\n\n```yaml\nopenai:\n  api_key: \"sk-your-api-key-here\"\n  enabled: true\n```\n\nThen restart the server.";
      } else if (errorMessage.includes('API error')) {
        displayMessage = `**AI Error**\n\n${errorMessage}\n\nPlease check your OpenAI API key and try again.`;
      }
      
      setMessages((prev) => [
        ...prev,
        {
          role: 'agent',
          content: displayMessage,
          timestamp: new Date(),
        },
      ]);
    } finally {
      setLoading(false);
    }
  };

  const handleSuggestionClick = (suggestion: string) => {
    setInput(suggestion);
  };
  
  const clearHistory = () => {
    setChatHistory([]);
    setMessages([{
      role: 'agent',
      content: "Conversation cleared! How can I help you?",
      timestamp: new Date(),
      suggestions: [
        "What's our revenue today?",
        'Show me campaign performance',
        'Any delivery issues?',
      ],
    }]);
  };

  const formatMessage = (content: string) => {
    // Convert markdown-like formatting to HTML
    return content
      .split('\n')
      .map((line, idx) => {
        // Bold text
        line = line.replace(/\*\*(.*?)\*\*/g, '<strong>$1</strong>');
        // Bullet points
        if (line.startsWith('•') || line.startsWith('-')) {
          return `<li key="${idx}" style="margin-left: 1rem">${line.substring(1).trim()}</li>`;
        }
        if (line.startsWith('━')) {
          return `<hr key="${idx}" style="border: none; border-top: 1px solid var(--border-color); margin: 0.5rem 0" />`;
        }
        return line;
      })
      .join('<br />');
  };

  return (
    <Card style={{ height: 'calc(100vh - 200px)', display: 'flex', flexDirection: 'column' }}>
      <CardHeader 
        title={
          <span style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
            Jarvis AI
            {isAIPowered && (
              <span style={{ 
                display: 'inline-flex',
                alignItems: 'center',
                gap: '0.25rem',
                fontSize: '0.65rem',
                padding: '0.15rem 0.4rem',
                backgroundColor: 'var(--primary-color)',
                color: 'white',
                borderRadius: '4px',
              }}>
                <FontAwesomeIcon icon={faMagic} />
                GPT-4
              </span>
            )}
          </span>
        }
        action={
          <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
            {chatHistory.length > 0 && (
              <button
                onClick={clearHistory}
                style={{
                  fontSize: '0.7rem',
                  padding: '0.25rem 0.5rem',
                  backgroundColor: 'transparent',
                  border: '1px solid var(--border-color)',
                  borderRadius: '4px',
                  cursor: 'pointer',
                  color: 'var(--text-muted)',
                }}
              >
                Clear History
              </button>
            )}
            <span style={{ 
              display: 'flex', 
              alignItems: 'center', 
              gap: '0.5rem',
              fontSize: '0.75rem',
              color: 'var(--accent-green)',
            }}>
              <span style={{
                width: '8px',
                height: '8px',
                backgroundColor: 'var(--accent-green)',
                borderRadius: '50%',
              }} />
              Online
            </span>
          </div>
        }
      />
      <CardBody style={{ 
        flex: 1, 
        display: 'flex', 
        flexDirection: 'column',
        padding: 0,
        overflow: 'hidden',
      }}>
        {/* Messages */}
        <div className="chat-messages" style={{ flex: 1, overflowY: 'auto' }}>
          {messages.map((message, idx) => (
            <div key={idx}>
              <div className={`chat-message ${message.role}`}>
                <div style={{ 
                  display: 'flex', 
                  alignItems: 'flex-start', 
                  gap: '0.75rem',
                  marginBottom: '0.5rem',
                }}>
                  {message.role === 'agent' ? (
                    <FontAwesomeIcon icon={faRobot} style={{ color: 'var(--accent-blue)', flexShrink: 0, marginTop: '2px' }} />
                  ) : (
                    <FontAwesomeIcon icon={faUser} style={{ flexShrink: 0, marginTop: '2px' }} />
                  )}
                  <div 
                    dangerouslySetInnerHTML={{ __html: formatMessage(message.content) }}
                    style={{ flex: 1 }}
                  />
                </div>
                <div style={{ 
                  fontSize: '0.625rem', 
                  color: message.role === 'user' ? 'rgba(255,255,255,0.6)' : 'var(--text-muted)',
                  textAlign: 'right',
                }}>
                  {message.timestamp.toLocaleTimeString()}
                </div>
              </div>
              
              {/* Suggestions */}
              {message.role === 'agent' && message.suggestions && message.suggestions.length > 0 && (
                <div className="chat-suggestions" style={{ marginTop: '0.5rem' }}>
                  {message.suggestions.map((suggestion, sIdx) => (
                    <button
                      key={sIdx}
                      className="chat-suggestion"
                      onClick={() => handleSuggestionClick(suggestion)}
                    >
                      {suggestion}
                    </button>
                  ))}
                </div>
              )}
            </div>
          ))}
          
          {loading && (
            <div className="chat-message agent">
              <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                <FontAwesomeIcon icon={faRobot} style={{ color: 'var(--accent-blue)' }} />
                <Loading message="Thinking..." />
              </div>
            </div>
          )}
          
          <div ref={messagesEndRef} />
        </div>

        {/* Input */}
        <form onSubmit={handleSubmit} className="chat-input-container">
          <input
            type="text"
            className="chat-input"
            placeholder="Ask me about your email performance..."
            value={input}
            onChange={(e) => setInput(e.target.value)}
            disabled={loading}
          />
          <button 
            type="submit" 
            className="chat-submit"
            disabled={loading || !input.trim()}
          >
            <FontAwesomeIcon icon={faPaperPlane} />
          </button>
        </form>
      </CardBody>
    </Card>
  );
};

export default AgentChat;
