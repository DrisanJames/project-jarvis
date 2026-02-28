import React, { useState, useEffect } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { 
  faMagic, 
  faWandMagicSparkles, 
  faSync, 
  faCheck, 
  faCopy, 
  faChevronDown, 
  faChevronUp, 
  faBolt, 
  faQuestionCircle, 
  faCrosshairs, 
  faClock, 
  faGift, 
  faArrowRight 
} from '@fortawesome/free-solid-svg-icons';
import './AISubjectSuggestions.css'; // Reuse the same styles

// ============================================
// TYPES
// ============================================

interface PreheaderSuggestion {
  subject: string;
  plain_subject: string;
  personalization_tags: string[];
  predicted_open_rate: number;
  category: string;
  reasoning: string;
  character_count: number;
}

interface AIPreheaderSuggestionsProps {
  onSelect: (preheader: string) => void;
  subjectLine?: string;  // The subject line to complement
  htmlContent?: string;
  tone?: string;
  disabled?: boolean;
}

// ============================================
// CATEGORY ICONS & LABELS
// ============================================

const CATEGORY_CONFIG: Record<string, { icon: React.ReactNode; label: string; color: string }> = {
  urgency: { icon: <FontAwesomeIcon icon={faClock} />, label: 'Urgency', color: '#ef4444' },
  curiosity: { icon: <FontAwesomeIcon icon={faQuestionCircle} />, label: 'Curiosity', color: '#8b5cf6' },
  benefit: { icon: <FontAwesomeIcon icon={faGift} />, label: 'Benefit', color: '#10b981' },
  personalized: { icon: <FontAwesomeIcon icon={faMagic} />, label: 'Personal', color: '#3b82f6' },
  call_to_action: { icon: <FontAwesomeIcon icon={faArrowRight} />, label: 'CTA', color: '#f59e0b' },
  exclusive: { icon: <FontAwesomeIcon icon={faCrosshairs} />, label: 'Exclusive', color: '#6366f1' },
};

// ============================================
// AI PREHEADER SUGGESTIONS COMPONENT
// ============================================

export const AIPreheaderSuggestions: React.FC<AIPreheaderSuggestionsProps> = ({
  onSelect,
  subjectLine = '',
  htmlContent = '',
  tone = 'professional',
  disabled = false,
}) => {
  const [isOpen, setIsOpen] = useState(false);
  const [isLoading, setIsLoading] = useState(false);
  const [suggestions, setSuggestions] = useState<PreheaderSuggestion[]>([]);
  const [copiedIndex, setCopiedIndex] = useState<number | null>(null);
  const [showSettings, setShowSettings] = useState(false);
  
  // Settings state
  const [settings, setSettings] = useState({
    tone: tone,
    includeEmoji: false,
    count: 5,
  });

  // Regenerate when subject line changes significantly
  const [lastSubject, setLastSubject] = useState(subjectLine);
  
  useEffect(() => {
    // If panel is open and subject changed significantly, auto-regenerate
    if (isOpen && subjectLine !== lastSubject && subjectLine.length > 5) {
      setLastSubject(subjectLine);
      // Small debounce
      const timer = setTimeout(() => {
        generateSuggestions();
      }, 500);
      return () => clearTimeout(timer);
    }
  }, [subjectLine, isOpen]);

  const generateSuggestions = async () => {
    setIsLoading(true);

    try {
      const response = await fetch('/api/mailing/ai/preheader-suggestions', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          subject: subjectLine,
          html_content: htmlContent,
          tone: settings.tone,
          include_emoji: settings.includeEmoji,
          count: settings.count,
        }),
      });

      if (!response.ok) {
        console.warn('AI API returned error, using fallback suggestions');
        setSuggestions(getFallbackSuggestions(subjectLine));
        return;
      }

      const data = await response.json();
      const suggestions = data.suggestions || [];
      
      if (suggestions.length === 0) {
        setSuggestions(getFallbackSuggestions(subjectLine));
      } else {
        setSuggestions(suggestions);
      }
    } catch (err) {
      console.warn('AI suggestions unavailable, using smart defaults:', err);
      setSuggestions(getFallbackSuggestions(subjectLine));
    } finally {
      setIsLoading(false);
    }
  };

  const handleSelect = (suggestion: PreheaderSuggestion) => {
    onSelect(suggestion.subject);
    setCopiedIndex(suggestions.indexOf(suggestion));
    setTimeout(() => setCopiedIndex(null), 2000);
  };

  const handleCopy = (text: string, index: number) => {
    navigator.clipboard.writeText(text);
    setCopiedIndex(index);
    setTimeout(() => setCopiedIndex(null), 2000);
  };

  const getCategoryConfig = (category: string) => {
    return CATEGORY_CONFIG[category] || CATEGORY_CONFIG.personalized;
  };

  return (
    <div className="ai-subject-suggestions">
      <button
        type="button"
        className={`ai-trigger-btn ${isOpen ? 'active' : ''}`}
        onClick={() => {
          setIsOpen(!isOpen);
          if (!isOpen && suggestions.length === 0) {
            generateSuggestions();
          }
        }}
        disabled={disabled}
      >
        <FontAwesomeIcon icon={faWandMagicSparkles} />
        <span>AI Suggestions</span>
        {isOpen ? <FontAwesomeIcon icon={faChevronUp} /> : <FontAwesomeIcon icon={faChevronDown} />}
      </button>

      {isOpen && (
        <div className="ai-suggestions-panel">
          <div className="ai-panel-header">
            <div className="ai-header-left">
              <FontAwesomeIcon icon={faMagic} className="ai-icon" />
              <span>AI Preheader Suggestions</span>
            </div>
            <div className="ai-header-actions">
              <button
                type="button"
                className="ai-settings-btn"
                onClick={() => setShowSettings(!showSettings)}
                title="Settings"
              >
                <FontAwesomeIcon icon={faBolt} />
              </button>
              <button
                type="button"
                className="ai-refresh-btn"
                onClick={generateSuggestions}
                disabled={isLoading}
                title="Regenerate"
              >
                <FontAwesomeIcon icon={faSync} className={isLoading ? 'spinning' : ''} />
              </button>
            </div>
          </div>

          {subjectLine && (
            <div className="ai-context-info">
              <span className="context-label">Complementing subject:</span>
              <span className="context-value">{subjectLine.length > 50 ? subjectLine.slice(0, 50) + '...' : subjectLine}</span>
            </div>
          )}

          {showSettings && (
            <div className="ai-settings">
              <div className="ai-setting-row">
                <label>Tone</label>
                <select
                  value={settings.tone}
                  onChange={(e) => setSettings({ ...settings, tone: e.target.value })}
                >
                  <option value="professional">Professional</option>
                  <option value="casual">Casual</option>
                  <option value="friendly">Friendly</option>
                  <option value="urgent">Urgent</option>
                  <option value="playful">Playful</option>
                </select>
              </div>
              <div className="ai-setting-row">
                <label>Include Emojis</label>
                <input
                  type="checkbox"
                  checked={settings.includeEmoji}
                  onChange={(e) => setSettings({ ...settings, includeEmoji: e.target.checked })}
                />
              </div>
              <button
                type="button"
                className="ai-apply-settings"
                onClick={() => {
                  setShowSettings(false);
                  generateSuggestions();
                }}
              >
                Apply & Regenerate
              </button>
            </div>
          )}

          <div className="ai-suggestions-list">
            {isLoading ? (
              <div className="ai-loading">
                <FontAwesomeIcon icon={faSync} className="spinning" size="lg" />
                <span>Generating complementary preheaders...</span>
              </div>
            ) : suggestions.length === 0 ? (
              <div className="ai-empty">
                <FontAwesomeIcon icon={faMagic} size="lg" />
                <p>Click "AI Suggestions" to generate preheaders</p>
              </div>
            ) : (
              suggestions.map((suggestion, index) => {
                const config = getCategoryConfig(suggestion.category);
                return (
                  <div key={index} className="ai-suggestion-card">
                    <div className="suggestion-header">
                      <span
                        className="suggestion-category"
                        style={{ backgroundColor: config.color + '20', color: config.color }}
                      >
                        {config.icon}
                        {config.label}
                      </span>
                      <span className="suggestion-rate">
                        ~{suggestion.predicted_open_rate.toFixed(1)}% open rate
                      </span>
                    </div>
                    
                    <div className="suggestion-content">
                      <div className="suggestion-subject">{suggestion.subject}</div>
                      <div className="suggestion-preview">
                        Preview: <em>{suggestion.plain_subject}</em>
                      </div>
                    </div>
                    
                    {suggestion.personalization_tags && suggestion.personalization_tags.length > 0 && (
                      <div className="suggestion-tags">
                        {suggestion.personalization_tags.map((tag, i) => (
                          <span key={i} className="personalization-tag">
                            {`{{ ${tag} }}`}
                          </span>
                        ))}
                      </div>
                    )}
                    
                    <div className="suggestion-reasoning">
                      <strong>Why this works:</strong> {suggestion.reasoning}
                    </div>
                    
                    <div className="suggestion-actions">
                      <button
                        type="button"
                        className="use-btn"
                        onClick={() => handleSelect(suggestion)}
                      >
                        {copiedIndex === index ? (
                          <>
                            <FontAwesomeIcon icon={faCheck} />
                            Applied!
                          </>
                        ) : (
                          <>
                            <FontAwesomeIcon icon={faBolt} />
                            Use This
                          </>
                        )}
                      </button>
                      <button
                        type="button"
                        className="copy-btn"
                        onClick={() => handleCopy(suggestion.subject, index)}
                        title="Copy to clipboard"
                      >
                        <FontAwesomeIcon icon={faCopy} />
                      </button>
                      <span className="char-count">
                        {suggestion.character_count} chars
                      </span>
                    </div>
                  </div>
                );
              })
            )}
          </div>

          <div className="ai-panel-footer">
            <span className="ai-tip">
              💡 Tip: Great preheaders extend the subject line's promise without repeating it
            </span>
          </div>
        </div>
      )}
    </div>
  );
};

// Fallback suggestions based on subject line context
function getFallbackSuggestions(subjectLine: string): PreheaderSuggestion[] {
  const subjectLower = subjectLine.toLowerCase();
  const hasQuestion = subjectLower.includes('?');
  const hasUrgency = subjectLower.includes('miss') || subjectLower.includes('hurry') || subjectLower.includes('last') || subjectLower.includes('now');
  
  const suggestions: PreheaderSuggestion[] = [];
  
  if (hasQuestion) {
    suggestions.push({
      subject: "{{ first_name }}, the answer might surprise you...",
      plain_subject: "The answer might surprise you...",
      personalization_tags: ["first_name"],
      predicted_open_rate: 21.5,
      category: "curiosity",
      reasoning: "Builds on the question in your subject line",
      character_count: 47,
    });
  }
  
  if (hasUrgency) {
    suggestions.push({
      subject: "{{ first_name }}, this won't last long. Open now →",
      plain_subject: "This won't last long. Open now →",
      personalization_tags: ["first_name"],
      predicted_open_rate: 23.0,
      category: "urgency",
      reasoning: "Reinforces the urgency from your subject line",
      character_count: 50,
    });
  }
  
  // Default suggestions
  suggestions.push(
    {
      subject: "{{ first_name }}, we put this together just for you...",
      plain_subject: "We put this together just for you...",
      personalization_tags: ["first_name"],
      predicted_open_rate: 20.5,
      category: "personalized",
      reasoning: "Personal touch that complements any subject line",
      character_count: 52,
    },
    {
      subject: "Open to see what's waiting inside, {{ first_name }}",
      plain_subject: "Open to see what's waiting inside",
      personalization_tags: ["first_name"],
      predicted_open_rate: 19.5,
      category: "curiosity",
      reasoning: "Creates curiosity without spoiling content",
      character_count: 51,
    },
    {
      subject: "{{ first_name }}, you'll want to see this →",
      plain_subject: "You'll want to see this →",
      personalization_tags: ["first_name"],
      predicted_open_rate: 20.0,
      category: "call_to_action",
      reasoning: "Direct call-to-action with curiosity",
      character_count: 43,
    },
    {
      subject: "Here's what we've been working on, {{ first_name }}",
      plain_subject: "Here's what we've been working on",
      personalization_tags: ["first_name"],
      predicted_open_rate: 18.5,
      category: "personalized",
      reasoning: "Behind-the-scenes feel builds connection",
      character_count: 52,
    },
    {
      subject: "{{ first_name }}, this is worth your time. Trust us.",
      plain_subject: "This is worth your time. Trust us.",
      personalization_tags: ["first_name"],
      predicted_open_rate: 19.0,
      category: "benefit",
      reasoning: "Direct value promise builds trust",
      character_count: 51,
    }
  );
  
  return suggestions.slice(0, 5);
}

export default AIPreheaderSuggestions;
