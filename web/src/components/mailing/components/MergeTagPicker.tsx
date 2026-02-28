import React, { useState, useEffect, useRef } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faSearch, faChevronDown, faCopy, faCheck, faTimes, faMagic, faCode, faFilter, faEye } from '@fortawesome/free-solid-svg-icons';
import './MergeTagPicker.css';

// ============================================
// TYPES
// ============================================

interface MergeTag {
  key: string;
  label: string;
  category: string;
  data_type: string;
  sample?: string;
  syntax: string;
}

interface FilterInfo {
  name: string;
  description: string;
  example: string;
  category: string;
}

interface PreviewResponse {
  rendered_subject: string;
  rendered_html: string;
  rendered_text?: string;
  context: Record<string, unknown>;
  warnings: Array<{ variable: string; message: string }>;
  success: boolean;
}

interface MergeTagPickerProps {
  onInsert: (syntax: string) => void;
  position?: 'dropdown' | 'modal' | 'inline';
  showPreview?: boolean;
  currentContent?: string;
  currentSubject?: string;
}

// ============================================
// MERGE TAG PICKER COMPONENT
// ============================================

export const MergeTagPicker: React.FC<MergeTagPickerProps> = ({
  onInsert,
  position = 'dropdown',
  showPreview = false,
  currentContent = '',
  currentSubject = '',
}) => {
  const [isOpen, setIsOpen] = useState(false);
  const [activeTab, setActiveTab] = useState<'tags' | 'filters' | 'logic' | 'preview'>('tags');
  const [searchQuery, setSearchQuery] = useState('');
  const [selectedCategory, setSelectedCategory] = useState<string>('all');
  const [copiedTag, setCopiedTag] = useState<string | null>(null);

  const [tags, setTags] = useState<MergeTag[]>([]);
  const [customTags, setCustomTags] = useState<MergeTag[]>([]);
  const [filters, setFilters] = useState<FilterInfo[]>([]);
  const [preview, setPreview] = useState<PreviewResponse | null>(null);
  const [isLoadingPreview, setIsLoadingPreview] = useState(false);

  const dropdownRef = useRef<HTMLDivElement>(null);

  // Close dropdown when clicking outside
  useEffect(() => {
    const handleClickOutside = (event: MouseEvent) => {
      if (dropdownRef.current && !dropdownRef.current.contains(event.target as Node)) {
        setIsOpen(false);
      }
    };

    document.addEventListener('mousedown', handleClickOutside);
    return () => document.removeEventListener('mousedown', handleClickOutside);
  }, []);

  // Load merge tags
  useEffect(() => {
    if (isOpen) {
      loadMergeTags();
      loadFilters();
    }
  }, [isOpen]);

  const loadMergeTags = async () => {
    try {
      const [tagsRes, customRes] = await Promise.all([
        fetch('/api/mailing/personalization/merge-tags'),
        fetch('/api/mailing/personalization/merge-tags/custom'),
      ]);

      if (tagsRes.ok) {
        const data = await tagsRes.json();
        setTags(data.tags || []);
      }

      if (customRes.ok) {
        const data = await customRes.json();
        setCustomTags(data.tags || []);
      }
    } catch (error) {
      console.error('Failed to load merge tags:', error);
    }
  };

  const loadFilters = async () => {
    try {
      const res = await fetch('/api/mailing/personalization/filters');
      if (res.ok) {
        const data = await res.json();
        setFilters(data.filters || []);
      }
    } catch (error) {
      console.error('Failed to load filters:', error);
    }
  };

  const loadPreview = async () => {
    if (!currentContent && !currentSubject) return;

    setIsLoadingPreview(true);
    try {
      const res = await fetch('/api/mailing/personalization/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          subject: currentSubject,
          html_content: currentContent,
          use_sample: true,
        }),
      });

      if (res.ok) {
        const data = await res.json();
        setPreview(data);
      }
    } catch (error) {
      console.error('Failed to load preview:', error);
    } finally {
      setIsLoadingPreview(false);
    }
  };

  const handleInsertTag = (tag: MergeTag) => {
    onInsert(tag.syntax);
    setCopiedTag(tag.key);
    setTimeout(() => setCopiedTag(null), 2000);
  };

  const handleCopyToClipboard = (text: string, key: string) => {
    navigator.clipboard.writeText(text);
    setCopiedTag(key);
    setTimeout(() => setCopiedTag(null), 2000);
  };

  // Filter tags based on search and category
  const allTags = [...tags, ...customTags];
  const filteredTags = allTags.filter((tag) => {
    const matchesSearch =
      searchQuery === '' ||
      tag.label.toLowerCase().includes(searchQuery.toLowerCase()) ||
      tag.key.toLowerCase().includes(searchQuery.toLowerCase());
    const matchesCategory = selectedCategory === 'all' || tag.category === selectedCategory;
    return matchesSearch && matchesCategory;
  });

  // Group filtered tags by category
  const groupedTags: Record<string, MergeTag[]> = {};
  filteredTags.forEach((tag) => {
    if (!groupedTags[tag.category]) {
      groupedTags[tag.category] = [];
    }
    groupedTags[tag.category].push(tag);
  });

  const categories = ['all', ...new Set(allTags.map((t) => t.category))];

  const categoryLabels: Record<string, string> = {
    all: 'All Tags',
    profile: 'Profile',
    custom: 'Custom Fields',
    engagement: 'Engagement',
    computed: 'Computed',
    system: 'System',
    logic: 'Logic Blocks',
  };

  const categoryIcons: Record<string, string> = {
    profile: '👤',
    custom: '📝',
    engagement: '📊',
    computed: '🧮',
    system: '⚙️',
    logic: '🔀',
  };

  return (
    <div className={`merge-tag-picker ${position}`} ref={dropdownRef}>
      <button
        className="merge-tag-trigger"
        onClick={() => setIsOpen(!isOpen)}
        type="button"
      >
        <FontAwesomeIcon icon={faCode} />
        <span>Personalize</span>
        <FontAwesomeIcon icon={faChevronDown} className={isOpen ? 'rotated' : ''} />
      </button>

      {isOpen && (
        <div className="merge-tag-dropdown">
          <div className="merge-tag-header">
            <div className="merge-tag-tabs">
              <button
                className={activeTab === 'tags' ? 'active' : ''}
                onClick={() => setActiveTab('tags')}
              >
                <FontAwesomeIcon icon={faMagic} />
                Variables
              </button>
              <button
                className={activeTab === 'filters' ? 'active' : ''}
                onClick={() => setActiveTab('filters')}
              >
                <FontAwesomeIcon icon={faFilter} />
                Filters
              </button>
              <button
                className={activeTab === 'logic' ? 'active' : ''}
                onClick={() => setActiveTab('logic')}
              >
                <FontAwesomeIcon icon={faCode} />
                Logic
              </button>
              {showPreview && (
                <button
                  className={activeTab === 'preview' ? 'active' : ''}
                  onClick={() => {
                    setActiveTab('preview');
                    loadPreview();
                  }}
                >
                  <FontAwesomeIcon icon={faEye} />
                  Preview
                </button>
              )}
            </div>
            <button className="close-btn" onClick={() => setIsOpen(false)}>
              <FontAwesomeIcon icon={faTimes} />
            </button>
          </div>

          {activeTab === 'tags' && (
            <div className="merge-tag-content">
              <div className="search-bar">
                <FontAwesomeIcon icon={faSearch} />
                <input
                  type="text"
                  placeholder="Search variables..."
                  value={searchQuery}
                  onChange={(e) => setSearchQuery(e.target.value)}
                  autoFocus
                />
              </div>

              <div className="category-filter">
                {categories.map((cat) => (
                  <button
                    key={cat}
                    className={selectedCategory === cat ? 'active' : ''}
                    onClick={() => setSelectedCategory(cat)}
                  >
                    {categoryIcons[cat] && <span>{categoryIcons[cat]}</span>}
                    {categoryLabels[cat] || cat}
                  </button>
                ))}
              </div>

              <div className="tags-list">
                {Object.entries(groupedTags).map(([category, categoryTags]) => (
                  <div key={category} className="tag-group">
                    <div className="tag-group-header">
                      {categoryIcons[category]} {categoryLabels[category] || category}
                    </div>
                    {categoryTags.map((tag) => (
                      <div key={tag.key} className="tag-item">
                        <div className="tag-info">
                          <span className="tag-label">{tag.label}</span>
                          <code className="tag-syntax">{tag.syntax}</code>
                          {tag.sample && (
                            <span className="tag-sample">e.g., {tag.sample}</span>
                          )}
                        </div>
                        <div className="tag-actions">
                          <button
                            className="insert-btn"
                            onClick={() => handleInsertTag(tag)}
                            title="Insert into editor"
                          >
                            {copiedTag === tag.key ? (
                              <FontAwesomeIcon icon={faCheck} />
                            ) : (
                              'Insert'
                            )}
                          </button>
                          <button
                            className="copy-btn"
                            onClick={() => handleCopyToClipboard(tag.syntax, tag.key)}
                            title="Copy to clipboard"
                          >
                            <FontAwesomeIcon icon={faCopy} />
                          </button>
                        </div>
                      </div>
                    ))}
                  </div>
                ))}

                {Object.keys(groupedTags).length === 0 && (
                  <div className="no-results">
                    No variables match your search
                  </div>
                )}
              </div>
            </div>
          )}

          {activeTab === 'filters' && (
            <div className="merge-tag-content">
              <div className="filters-intro">
                <p>Filters transform variable values. Use them with the pipe symbol:</p>
                <code>{'{{ variable | filter }}'}</code>
              </div>

              <div className="filters-list">
                {filters.map((filter) => (
                  <div key={filter.name} className="filter-item">
                    <div className="filter-info">
                      <span className="filter-name">{filter.name}</span>
                      <span className="filter-description">{filter.description}</span>
                      <code className="filter-example">{filter.example}</code>
                    </div>
                    <button
                      className="copy-btn"
                      onClick={() => handleCopyToClipboard(filter.example, filter.name)}
                      title="Copy example"
                    >
                      {copiedTag === filter.name ? (
                        <FontAwesomeIcon icon={faCheck} />
                      ) : (
                        <FontAwesomeIcon icon={faCopy} />
                      )}
                    </button>
                  </div>
                ))}
              </div>
            </div>
          )}

          {activeTab === 'logic' && (
            <div className="merge-tag-content">
              <div className="logic-intro">
                <p>Use logic blocks for conditional content and loops.</p>
              </div>

              <div className="logic-blocks">
                <div className="logic-block">
                  <div className="logic-header">Conditional (If/Else)</div>
                  <pre className="logic-code">
{`{% if first_name %}
  Hello {{ first_name }}!
{% else %}
  Hello there!
{% endif %}`}
                  </pre>
                  <button
                    className="copy-btn"
                    onClick={() =>
                      handleCopyToClipboard(
                        '{% if first_name %}Hello {{ first_name }}!{% else %}Hello there!{% endif %}',
                        'if-else'
                      )
                    }
                  >
                    {copiedTag === 'if-else' ? <FontAwesomeIcon icon={faCheck} /> : <FontAwesomeIcon icon={faCopy} />}
                    Copy
                  </button>
                </div>

                <div className="logic-block">
                  <div className="logic-header">Check Custom Field</div>
                  <pre className="logic-code">
{`{% if custom.is_vip %}
  <div class="vip-badge">VIP Member</div>
{% endif %}`}
                  </pre>
                  <button
                    className="copy-btn"
                    onClick={() =>
                      handleCopyToClipboard(
                        '{% if custom.is_vip %}<div class="vip-badge">VIP</div>{% endif %}',
                        'if-vip'
                      )
                    }
                  >
                    {copiedTag === 'if-vip' ? <FontAwesomeIcon icon={faCheck} /> : <FontAwesomeIcon icon={faCopy} />}
                    Copy
                  </button>
                </div>

                <div className="logic-block">
                  <div className="logic-header">Loop Through Array</div>
                  <pre className="logic-code">
{`{% for item in custom.interests %}
  <span class="tag">{{ item }}</span>
{% endfor %}`}
                  </pre>
                  <button
                    className="copy-btn"
                    onClick={() =>
                      handleCopyToClipboard(
                        '{% for item in custom.interests %}<span>{{ item }}</span>{% endfor %}',
                        'for-loop'
                      )
                    }
                  >
                    {copiedTag === 'for-loop' ? <FontAwesomeIcon icon={faCheck} /> : <FontAwesomeIcon icon={faCopy} />}
                    Copy
                  </button>
                </div>

                <div className="logic-block">
                  <div className="logic-header">Unless (Negative Conditional)</div>
                  <pre className="logic-code">
{`{% unless custom.opted_out %}
  <p>Special offer just for you!</p>
{% endunless %}`}
                  </pre>
                  <button
                    className="copy-btn"
                    onClick={() =>
                      handleCopyToClipboard(
                        '{% unless custom.opted_out %}<p>Special offer!</p>{% endunless %}',
                        'unless'
                      )
                    }
                  >
                    {copiedTag === 'unless' ? <FontAwesomeIcon icon={faCheck} /> : <FontAwesomeIcon icon={faCopy} />}
                    Copy
                  </button>
                </div>

                <div className="logic-block">
                  <div className="logic-header">Case/When (Switch)</div>
                  <pre className="logic-code">
{`{% case custom.membership_level %}
  {% when "gold" %}
    Gold Member Benefits
  {% when "silver" %}
    Silver Member Benefits
  {% else %}
    Standard Benefits
{% endcase %}`}
                  </pre>
                  <button
                    className="copy-btn"
                    onClick={() =>
                      handleCopyToClipboard(
                        '{% case custom.level %}{% when "gold" %}Gold{% when "silver" %}Silver{% else %}Standard{% endcase %}',
                        'case'
                      )
                    }
                  >
                    {copiedTag === 'case' ? <FontAwesomeIcon icon={faCheck} /> : <FontAwesomeIcon icon={faCopy} />}
                    Copy
                  </button>
                </div>
              </div>
            </div>
          )}

          {activeTab === 'preview' && (
            <div className="merge-tag-content preview-content">
              {isLoadingPreview ? (
                <div className="loading">Loading preview...</div>
              ) : preview ? (
                <>
                  {preview.warnings.length > 0 && (
                    <div className="preview-warnings">
                      <strong>Warnings:</strong>
                      <ul>
                        {preview.warnings.map((w, i) => (
                          <li key={i}>
                            <code>{w.variable}</code>: {w.message}
                          </li>
                        ))}
                      </ul>
                    </div>
                  )}

                  {preview.rendered_subject && (
                    <div className="preview-section">
                      <h4>Subject Line</h4>
                      <div className="preview-subject">{preview.rendered_subject}</div>
                    </div>
                  )}

                  {preview.rendered_html && (
                    <div className="preview-section">
                      <h4>HTML Content</h4>
                      <iframe
                        srcDoc={preview.rendered_html}
                        className="preview-iframe"
                        title="Email Preview"
                      />
                    </div>
                  )}
                </>
              ) : (
                <div className="no-preview">
                  <p>No content to preview.</p>
                  <p>Add content with merge tags to see the rendered result.</p>
                </div>
              )}
            </div>
          )}

          <div className="merge-tag-footer">
            <span className="footer-hint">
              Use <code>{'{{ variable }}'}</code> for simple values,{' '}
              <code>{'{% if %}'}</code> for logic
            </span>
          </div>
        </div>
      )}
    </div>
  );
};

// ============================================
// FLOATING PERSONALIZE BUTTON (for editors)
// ============================================

interface PersonalizeButtonProps {
  onInsert: (syntax: string) => void;
  className?: string;
}

export const PersonalizeButton: React.FC<PersonalizeButtonProps> = ({
  onInsert,
  className = '',
}) => {
  return (
    <div className={`personalize-button-wrapper ${className}`}>
      <MergeTagPicker onInsert={onInsert} position="dropdown" />
    </div>
  );
};

// ============================================
// QUICK INSERT TOOLBAR (commonly used tags)
// ============================================

interface QuickInsertToolbarProps {
  onInsert: (syntax: string) => void;
}

export const QuickInsertToolbar: React.FC<QuickInsertToolbarProps> = ({ onInsert }) => {
  const quickTags = [
    { label: 'First Name', syntax: '{{ first_name | default: "Friend" }}' },
    { label: 'Email', syntax: '{{ email }}' },
    { label: 'Company', syntax: '{{ custom.company }}' },
    { label: 'Unsubscribe', syntax: '{{ system.unsubscribe_url }}' },
  ];

  return (
    <div className="quick-insert-toolbar">
      <span className="toolbar-label">Quick Insert:</span>
      {quickTags.map((tag) => (
        <button
          key={tag.label}
          className="quick-tag-btn"
          onClick={() => onInsert(tag.syntax)}
          title={tag.syntax}
        >
          {tag.label}
        </button>
      ))}
      <MergeTagPicker onInsert={onInsert} position="dropdown" />
    </div>
  );
};

export default MergeTagPicker;
