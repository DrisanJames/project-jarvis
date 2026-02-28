import React, { useState, useRef, useEffect } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faCode, faMagic, faChevronDown, faTimes, faCopy, faCheck, faSearch } from '@fortawesome/free-solid-svg-icons';
import './PersonalizedInput.css';
import { AISubjectSuggestions } from './AISubjectSuggestions';
import { AIPreheaderSuggestions } from './AIPreheaderSuggestions';

// ============================================
// TYPES
// ============================================

interface MergeTag {
  key: string;
  label: string;
  category: string;
  syntax: string;
  sample?: string;
}

interface PersonalizedInputProps {
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
  maxLength?: number;
  label?: string;
  required?: boolean;
  hint?: string;
  className?: string;
  inputType?: 'input' | 'textarea';
  rows?: number;
  // AI Suggestions
  showAISuggestions?: boolean;
  aiSuggestionType?: 'subject' | 'preheader';
  aiContext?: {
    htmlContent?: string;
    campaignType?: string;
    tone?: string;
    subjectLine?: string; // For preheader suggestions
  };
}

// ============================================
// DEFAULT QUICK TAGS (Most commonly used)
// ============================================

const QUICK_TAGS: MergeTag[] = [
  { key: 'first_name', label: 'First Name', category: 'profile', syntax: '{{ first_name | default: "there" }}', sample: 'John' },
  { key: 'last_name', label: 'Last Name', category: 'profile', syntax: '{{ last_name }}', sample: 'Doe' },
  { key: 'email', label: 'Email', category: 'profile', syntax: '{{ email }}', sample: 'john@example.com' },
  { key: 'full_name', label: 'Full Name', category: 'profile', syntax: '{{ full_name }}', sample: 'John Doe' },
  { key: 'custom.company', label: 'Company', category: 'custom', syntax: '{{ custom.company }}', sample: 'Acme Inc' },
];

const ALL_TAGS: MergeTag[] = [
  // Profile
  { key: 'first_name', label: 'First Name', category: 'profile', syntax: '{{ first_name }}', sample: 'John' },
  { key: 'first_name_default', label: 'First Name (with fallback)', category: 'profile', syntax: '{{ first_name | default: "Friend" }}', sample: 'John' },
  { key: 'last_name', label: 'Last Name', category: 'profile', syntax: '{{ last_name }}', sample: 'Doe' },
  { key: 'email', label: 'Email', category: 'profile', syntax: '{{ email }}', sample: 'john@example.com' },
  { key: 'full_name', label: 'Full Name', category: 'profile', syntax: '{{ full_name }}', sample: 'John Doe' },
  
  // Custom Fields
  { key: 'custom.company', label: 'Company', category: 'custom', syntax: '{{ custom.company }}', sample: 'Acme Inc' },
  { key: 'custom.job_title', label: 'Job Title', category: 'custom', syntax: '{{ custom.job_title }}', sample: 'CEO' },
  { key: 'custom.city', label: 'City', category: 'custom', syntax: '{{ custom.city }}', sample: 'San Francisco' },
  { key: 'custom.phone', label: 'Phone', category: 'custom', syntax: '{{ custom.phone }}', sample: '+1 555-1234' },
  
  // Engagement
  { key: 'engagement.score', label: 'Engagement Score', category: 'engagement', syntax: '{{ engagement.score }}', sample: '85' },
  { key: 'engagement.total_opens', label: 'Total Opens', category: 'engagement', syntax: '{{ engagement.total_opens }}', sample: '42' },
  
  // System
  { key: 'system.current_date', label: 'Current Date', category: 'system', syntax: '{{ now | date: "%B %d, %Y" }}', sample: 'February 1, 2026' },
  { key: 'system.current_year', label: 'Current Year', category: 'system', syntax: '{{ now | date: "%Y" }}', sample: '2026' },
];

// ============================================
// PERSONALIZED INPUT COMPONENT
// ============================================

export const PersonalizedInput: React.FC<PersonalizedInputProps> = ({
  value,
  onChange,
  placeholder = '',
  maxLength,
  label,
  required = false,
  hint,
  className = '',
  inputType = 'input',
  rows = 3,
  showAISuggestions = false,
  aiSuggestionType = 'subject',
  aiContext = {},
}) => {
  const [showPicker, setShowPicker] = useState(false);
  const [showAllTags, setShowAllTags] = useState(false);
  const [searchQuery, setSearchQuery] = useState('');
  const [copiedTag, setCopiedTag] = useState<string | null>(null);
  const [cursorPosition, setCursorPosition] = useState<number | null>(null);
  
  const inputRef = useRef<HTMLInputElement | HTMLTextAreaElement>(null);
  const pickerRef = useRef<HTMLDivElement>(null);

  // Close picker when clicking outside
  useEffect(() => {
    const handleClickOutside = (event: MouseEvent) => {
      if (pickerRef.current && !pickerRef.current.contains(event.target as Node)) {
        setShowPicker(false);
        setShowAllTags(false);
      }
    };

    document.addEventListener('mousedown', handleClickOutside);
    return () => document.removeEventListener('mousedown', handleClickOutside);
  }, []);

  // Track cursor position
  const handleInputChange = (e: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
    onChange(e.target.value);
    setCursorPosition(e.target.selectionStart);
  };

  const handleInputClick = () => {
    if (inputRef.current) {
      setCursorPosition(inputRef.current.selectionStart);
    }
  };

  // Insert tag at cursor position
  const insertTag = (tag: MergeTag) => {
    const pos = cursorPosition ?? value.length;
    const newValue = value.slice(0, pos) + tag.syntax + value.slice(pos);
    onChange(newValue);
    
    // Show feedback
    setCopiedTag(tag.key);
    setTimeout(() => setCopiedTag(null), 1500);
    
    // Focus back on input and set cursor after inserted tag
    setTimeout(() => {
      if (inputRef.current) {
        inputRef.current.focus();
        const newPos = pos + tag.syntax.length;
        inputRef.current.setSelectionRange(newPos, newPos);
        setCursorPosition(newPos);
      }
    }, 50);
  };

  // Check if value contains merge tags
  const hasMergeTags = /\{\{.*?\}\}/.test(value);

  // Filter tags for search
  const filteredTags = ALL_TAGS.filter(tag =>
    tag.label.toLowerCase().includes(searchQuery.toLowerCase()) ||
    tag.key.toLowerCase().includes(searchQuery.toLowerCase())
  );

  // Group tags by category
  const groupedTags: Record<string, MergeTag[]> = {};
  filteredTags.forEach(tag => {
    if (!groupedTags[tag.category]) {
      groupedTags[tag.category] = [];
    }
    groupedTags[tag.category].push(tag);
  });

  const categoryLabels: Record<string, string> = {
    profile: '👤 Profile',
    custom: '📝 Custom Fields',
    engagement: '📊 Engagement',
    system: '⚙️ System',
  };

  const charCount = value.length;
  const isOverLimit = maxLength ? charCount > maxLength : false;

  return (
    <div className={`personalized-input-wrapper ${className}`} ref={pickerRef}>
      {label && (
        <label className="pi-label">
          {label}
          {required && <span className="pi-required">*</span>}
          {maxLength && (
            <span className={`pi-char-count ${isOverLimit ? 'over' : ''}`}>
              {charCount}/{maxLength}
            </span>
          )}
        </label>
      )}
      
      <div className={`pi-input-container ${hasMergeTags ? 'has-tags' : ''}`}>
        {inputType === 'textarea' ? (
          <textarea
            ref={inputRef as React.RefObject<HTMLTextAreaElement>}
            value={value}
            onChange={handleInputChange}
            onClick={handleInputClick}
            placeholder={placeholder}
            className="pi-input pi-textarea"
            rows={rows}
          />
        ) : (
          <input
            ref={inputRef as React.RefObject<HTMLInputElement>}
            type="text"
            value={value}
            onChange={handleInputChange}
            onClick={handleInputClick}
            placeholder={placeholder}
            className="pi-input"
          />
        )}
        
        <div className="pi-buttons">
          {showAISuggestions && aiSuggestionType === 'subject' && (
            <AISubjectSuggestions
              onSelect={onChange}
              currentSubject={value}
              htmlContent={aiContext.htmlContent}
              campaignType={aiContext.campaignType}
              tone={aiContext.tone}
            />
          )}
          {showAISuggestions && aiSuggestionType === 'preheader' && (
            <AIPreheaderSuggestions
              onSelect={onChange}
              subjectLine={aiContext.subjectLine}
              htmlContent={aiContext.htmlContent}
              tone={aiContext.tone}
            />
          )}
          <button
            type="button"
            className={`pi-personalize-btn ${showPicker ? 'active' : ''}`}
            onClick={() => setShowPicker(!showPicker)}
            title="Insert personalization variable"
          >
            <FontAwesomeIcon icon={faMagic} />
            <span className="pi-btn-text">Personalize</span>
            <FontAwesomeIcon icon={faChevronDown} className={showPicker ? 'rotated' : ''} />
          </button>
        </div>
      </div>

      {hint && <small className="pi-hint">{hint}</small>}
      
      {hasMergeTags && (
        <div className="pi-tags-preview">
          <span className="pi-preview-label">Variables detected:</span>
          {value.match(/\{\{[^}]+\}\}/g)?.map((tag, i) => (
            <code key={i} className="pi-preview-tag">{tag}</code>
          ))}
        </div>
      )}

      {/* Quick Tags Dropdown */}
      {showPicker && !showAllTags && (
        <div className="pi-picker-dropdown">
          <div className="pi-picker-header">
            <span className="pi-picker-title">Quick Insert</span>
            <button
              type="button"
              className="pi-close-btn"
              onClick={() => setShowPicker(false)}
            >
              <FontAwesomeIcon icon={faTimes} />
            </button>
          </div>
          
          <div className="pi-quick-tags">
            {QUICK_TAGS.map(tag => (
              <button
                key={tag.key}
                type="button"
                className="pi-quick-tag"
                onClick={() => insertTag(tag)}
              >
                {copiedTag === tag.key ? (
                  <FontAwesomeIcon icon={faCheck} className="pi-check" />
                ) : null}
                <span className="pi-tag-label">{tag.label}</span>
                {tag.sample && <span className="pi-tag-sample">{tag.sample}</span>}
              </button>
            ))}
          </div>
          
          <button
            type="button"
            className="pi-show-all-btn"
            onClick={() => setShowAllTags(true)}
          >
            <FontAwesomeIcon icon={faCode} />
            Browse all variables
          </button>
        </div>
      )}

      {/* Full Tags Browser */}
      {showPicker && showAllTags && (
        <div className="pi-picker-dropdown pi-full-picker">
          <div className="pi-picker-header">
            <button
              type="button"
              className="pi-back-btn"
              onClick={() => setShowAllTags(false)}
            >
              ← Back
            </button>
            <span className="pi-picker-title">All Variables</span>
            <button
              type="button"
              className="pi-close-btn"
              onClick={() => { setShowPicker(false); setShowAllTags(false); }}
            >
              <FontAwesomeIcon icon={faTimes} />
            </button>
          </div>
          
          <div className="pi-search">
            <FontAwesomeIcon icon={faSearch} />
            <input
              type="text"
              placeholder="Search variables..."
              value={searchQuery}
              onChange={e => setSearchQuery(e.target.value)}
              autoFocus
            />
          </div>
          
          <div className="pi-tags-list">
            {Object.entries(groupedTags).map(([category, tags]) => (
              <div key={category} className="pi-tag-group">
                <div className="pi-group-header">{categoryLabels[category] || category}</div>
                {tags.map(tag => (
                  <button
                    key={tag.key}
                    type="button"
                    className="pi-tag-item"
                    onClick={() => insertTag(tag)}
                  >
                    <div className="pi-tag-info">
                      <span className="pi-tag-label">{tag.label}</span>
                      <code className="pi-tag-syntax">{tag.syntax}</code>
                    </div>
                    {copiedTag === tag.key ? (
                      <FontAwesomeIcon icon={faCheck} className="pi-check" />
                    ) : (
                      <FontAwesomeIcon icon={faCopy} className="pi-copy-icon" />
                    )}
                  </button>
                ))}
              </div>
            ))}
            
            {Object.keys(groupedTags).length === 0 && (
              <div className="pi-no-results">No variables match "{searchQuery}"</div>
            )}
          </div>
        </div>
      )}
    </div>
  );
};

// ============================================
// INLINE PERSONALIZE BUTTON (for existing inputs)
// ============================================

interface InlinePersonalizeButtonProps {
  onInsert: (syntax: string) => void;
}

export const InlinePersonalizeButton: React.FC<InlinePersonalizeButtonProps> = ({ onInsert }) => {
  const [showPicker, setShowPicker] = useState(false);
  const pickerRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const handleClickOutside = (event: MouseEvent) => {
      if (pickerRef.current && !pickerRef.current.contains(event.target as Node)) {
        setShowPicker(false);
      }
    };
    document.addEventListener('mousedown', handleClickOutside);
    return () => document.removeEventListener('mousedown', handleClickOutside);
  }, []);

  return (
    <div className="inline-personalize-wrapper" ref={pickerRef}>
      <button
        type="button"
        className="inline-personalize-btn"
        onClick={() => setShowPicker(!showPicker)}
        title="Add personalization"
      >
        <FontAwesomeIcon icon={faMagic} />
      </button>
      
      {showPicker && (
        <div className="inline-personalize-dropdown">
          <div className="ipd-header">Insert Variable</div>
          {QUICK_TAGS.map(tag => (
            <button
              key={tag.key}
              type="button"
              className="ipd-tag"
              onClick={() => {
                onInsert(tag.syntax);
                setShowPicker(false);
              }}
            >
              <span>{tag.label}</span>
              <code>{tag.syntax}</code>
            </button>
          ))}
        </div>
      )}
    </div>
  );
};

export default PersonalizedInput;
