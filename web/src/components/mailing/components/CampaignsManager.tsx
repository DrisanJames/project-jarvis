import React, { useEffect, useState, useRef } from 'react';
import { useCampaigns, useLists } from '../hooks/useMailingApi';
import type { Campaign, List } from '../types';
import './CampaignsManager.css';
import { ABTestBuilder, ABTestList, ABTestResults } from './ABTestBuilder';
import { PersonalizedInput } from './PersonalizedInput';

// Types for new features
interface SendingProfile {
  id: string;
  name: string;
  vendor_type: string;
  from_name: string;
  from_email: string;
  is_default: boolean;
}

interface Segment {
  id: string;
  name: string;
  list_id?: string;
  list_name?: string;
  subscriber_count?: number;
  description?: string;
  tags?: string[];
}

interface SuppressionList {
  id: string;
  name: string;
  type: string;
  count?: number;
  entry_count?: number;
  description?: string;
}

// Searchable Multi-Select Component for handling 1000+ items
interface SearchableMultiSelectProps {
  items: Array<{ id: string; name: string; count?: number; description?: string; tags?: string[]; list_name?: string }>;
  selectedIds: string[];
  onChange: (ids: string[]) => void;
  placeholder: string;
  searchPlaceholder?: string;
  emptyMessage?: string;
  maxDisplay?: number;
  variant?: 'default' | 'suppression';
}

const SearchableMultiSelect: React.FC<SearchableMultiSelectProps> = ({
  items,
  selectedIds,
  onChange,
  placeholder,
  searchPlaceholder = 'Search...',
  emptyMessage = 'No items found',
  maxDisplay = 50,
  variant = 'default',
}) => {
  const [isOpen, setIsOpen] = useState(false);
  const [search, setSearch] = useState('');
  const containerRef = useRef<HTMLDivElement>(null);

  // Close dropdown when clicking outside
  useEffect(() => {
    const handleClickOutside = (e: MouseEvent) => {
      if (containerRef.current && !containerRef.current.contains(e.target as Node)) {
        setIsOpen(false);
      }
    };
    document.addEventListener('mousedown', handleClickOutside);
    return () => document.removeEventListener('mousedown', handleClickOutside);
  }, []);

  // Filter items based on search
  const filteredItems = items.filter(item => {
    const searchLower = search.toLowerCase();
    return (
      item.name.toLowerCase().includes(searchLower) ||
      item.description?.toLowerCase().includes(searchLower) ||
      item.tags?.some(t => t.toLowerCase().includes(searchLower)) ||
      item.list_name?.toLowerCase().includes(searchLower)
    );
  }).slice(0, maxDisplay);

  const selectedItems = items.filter(item => selectedIds.includes(item.id));

  const toggleItem = (id: string) => {
    if (selectedIds.includes(id)) {
      onChange(selectedIds.filter(i => i !== id));
    } else {
      onChange([...selectedIds, id]);
    }
  };

  const removeItem = (id: string, e: React.MouseEvent) => {
    e.stopPropagation();
    onChange(selectedIds.filter(i => i !== id));
  };

  return (
    <div className={`searchable-multi-select ${variant}`} ref={containerRef}>
      <div className="select-trigger" onClick={() => setIsOpen(!isOpen)}>
        {selectedItems.length === 0 ? (
          <span className="placeholder">{placeholder}</span>
        ) : (
          <div className="selected-tags">
            {selectedItems.slice(0, 3).map(item => (
              <span key={item.id} className="selected-tag">
                {item.name}
                <button onClick={(e) => removeItem(item.id, e)}>×</button>
              </span>
            ))}
            {selectedItems.length > 3 && (
              <span className="more-tag">+{selectedItems.length - 3} more</span>
            )}
          </div>
        )}
        <span className="dropdown-arrow">{isOpen ? '▲' : '▼'}</span>
      </div>

      {isOpen && (
        <div className="select-dropdown">
          <div className="search-box">
            <input
              type="text"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder={searchPlaceholder}
              autoFocus
            />
            {search && (
              <button className="clear-search" onClick={() => setSearch('')}>×</button>
            )}
          </div>

          <div className="items-list">
            {filteredItems.length === 0 ? (
              <div className="empty-message">{emptyMessage}</div>
            ) : (
              <>
                {filteredItems.map(item => (
                  <div
                    key={item.id}
                    className={`select-item ${selectedIds.includes(item.id) ? 'selected' : ''}`}
                    onClick={() => toggleItem(item.id)}
                  >
                    <div className="item-checkbox">
                      {selectedIds.includes(item.id) ? '✓' : ''}
                    </div>
                    <div className="item-content">
                      <div className="item-name">{item.name}</div>
                      <div className="item-meta">
                        {item.list_name && <span className="meta-tag list">{item.list_name}</span>}
                        {item.count !== undefined && (
                          <span className="meta-count">{(item.count || 0).toLocaleString()} contacts</span>
                        )}
                        {item.description && (
                          <span className="meta-desc">{item.description}</span>
                        )}
                      </div>
                    </div>
                  </div>
                ))}
                {items.length > maxDisplay && filteredItems.length === maxDisplay && (
                  <div className="more-items">
                    Showing {maxDisplay} of {items.length} items. Type to search for more.
                  </div>
                )}
              </>
            )}
          </div>

          {selectedIds.length > 0 && (
            <div className="select-footer">
              <span>{selectedIds.length} selected</span>
              <button onClick={() => onChange([])}>Clear all</button>
            </div>
          )}
        </div>
      )}
    </div>
  );
};

// Rich Text Editor Component (Enterprise-grade WYSIWYG)
interface RichTextEditorProps {
  value: string;
  onChange: (html: string) => void;
  placeholder?: string;
}

const RichTextEditor: React.FC<RichTextEditorProps> = ({ value, onChange, placeholder }) => {
  const editorRef = useRef<HTMLDivElement>(null);
  const [showSourceCode, setShowSourceCode] = useState(false);

  const execCommand = (command: string, value?: string) => {
    document.execCommand(command, false, value);
    if (editorRef.current) {
      onChange(editorRef.current.innerHTML);
    }
  };

  const handleInput = () => {
    if (editorRef.current) {
      onChange(editorRef.current.innerHTML);
    }
  };

  const insertLink = () => {
    const url = prompt('Enter URL:');
    if (url) {
      execCommand('createLink', url);
    }
  };

  const insertImage = () => {
    const url = prompt('Enter image URL:');
    if (url) {
      execCommand('insertImage', url);
    }
  };

  const insertButton = () => {
    const text = prompt('Button text:', 'Click Here');
    const url = prompt('Button URL:');
    if (text && url) {
      const buttonHtml = `<a href="${url}" style="display: inline-block; padding: 12px 24px; background: linear-gradient(135deg, #667eea 0%, #764ba2 100%); color: white; text-decoration: none; border-radius: 6px; font-weight: 600;">${text}</a>`;
      document.execCommand('insertHTML', false, buttonHtml);
      handleInput();
    }
  };

  return (
    <div className="rich-text-editor">
      <div className="editor-toolbar">
        <div className="toolbar-group">
          <button type="button" onClick={() => execCommand('bold')} title="Bold">
            <strong>B</strong>
          </button>
          <button type="button" onClick={() => execCommand('italic')} title="Italic">
            <em>I</em>
          </button>
          <button type="button" onClick={() => execCommand('underline')} title="Underline">
            <u>U</u>
          </button>
          <button type="button" onClick={() => execCommand('strikeThrough')} title="Strikethrough">
            <s>S</s>
          </button>
        </div>
        <div className="toolbar-group">
          <select onChange={(e) => execCommand('formatBlock', e.target.value)} defaultValue="">
            <option value="" disabled>Heading</option>
            <option value="h1">Heading 1</option>
            <option value="h2">Heading 2</option>
            <option value="h3">Heading 3</option>
            <option value="p">Paragraph</option>
          </select>
          <select onChange={(e) => execCommand('fontSize', e.target.value)} defaultValue="">
            <option value="" disabled>Size</option>
            <option value="1">Small</option>
            <option value="3">Normal</option>
            <option value="5">Large</option>
            <option value="7">Huge</option>
          </select>
        </div>
        <div className="toolbar-group">
          <button type="button" onClick={() => execCommand('justifyLeft')} title="Align Left">⬛</button>
          <button type="button" onClick={() => execCommand('justifyCenter')} title="Center">⬛</button>
          <button type="button" onClick={() => execCommand('justifyRight')} title="Align Right">⬛</button>
        </div>
        <div className="toolbar-group">
          <button type="button" onClick={() => execCommand('insertUnorderedList')} title="Bullet List">•</button>
          <button type="button" onClick={() => execCommand('insertOrderedList')} title="Numbered List">1.</button>
        </div>
        <div className="toolbar-group">
          <button type="button" onClick={insertLink} title="Insert Link">🔗</button>
          <button type="button" onClick={insertImage} title="Insert Image">🖼️</button>
          <button type="button" onClick={insertButton} title="Insert Button">📦</button>
        </div>
        <div className="toolbar-group">
          <input 
            type="color" 
            onChange={(e) => execCommand('foreColor', e.target.value)} 
            title="Text Color"
            style={{ width: 30, height: 24 }}
          />
          <input 
            type="color" 
            onChange={(e) => execCommand('hiliteColor', e.target.value)} 
            title="Highlight Color"
            defaultValue="#ffff00"
            style={{ width: 30, height: 24 }}
          />
        </div>
        <div className="toolbar-group">
          <button 
            type="button" 
            onClick={() => setShowSourceCode(!showSourceCode)}
            className={showSourceCode ? 'active' : ''}
            title="View Source"
          >
            {'</>'}
          </button>
        </div>
      </div>
      
      {showSourceCode ? (
        <textarea
          className="source-editor"
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder="Enter HTML code..."
        />
      ) : (
        <div
          ref={editorRef}
          className="editor-content"
          contentEditable
          onInput={handleInput}
          dangerouslySetInnerHTML={{ __html: value || '' }}
          data-placeholder={placeholder || 'Start typing your email content...'}
        />
      )}
    </div>
  );
};

// ESP Quota allocation type
interface ESPQuota {
  profile_id: string;
  percentage: number;
}

// Throttle presets
const THROTTLE_PRESETS = [
  { value: 'instant', label: '⚡ Instant', description: 'Send as fast as possible', rate: 1000 },
  { value: 'gentle', label: '🌊 Gentle', description: 'Moderate pace', rate: 100 },
  { value: 'moderate', label: '⏳ Moderate', description: 'Conservative pace', rate: 50 },
  { value: 'careful', label: '🐢 Careful', description: 'Very slow pace', rate: 20 },
  { value: 'custom', label: '🎯 Custom', description: 'Set duration (hours)', rate: 0 },
];

interface CreateCampaignModalProps {
  isOpen: boolean;
  lists: List[];
  onClose: () => void;
  onSubmit: (campaign: any) => Promise<void>;
}

const CreateCampaignModal: React.FC<CreateCampaignModalProps> = ({
  isOpen,
  lists: _lists, // Kept for backward compatibility, segments are primary now
  onClose,
  onSubmit,
}) => {
  const [step, setStep] = useState(1);
  const [formData, setFormData] = useState({
    name: '',
    segment_ids: [] as string[], // SEGMENTS TO MAIL TO (can span multiple lists)
    subject: '',
    from_name: '',
    from_email: '',
    html_content: '',
    plain_content: '',
    preview_text: '',
    send_type: 'instant', // 'instant' or 'scheduled'
    scheduled_at: '',
    throttle_speed: 'gentle',
    throttle_duration_hours: 8, // Custom hours for smart throttling
    suppression_list_ids: [] as string[], // SUPPRESSION LISTS TO EXCLUDE
    suppression_segment_ids: [] as string[], // SEGMENTS TO EXCLUDE (by conditions)
    esp_quotas: [] as ESPQuota[], // MULTI-ESP WITH QUOTAS
    max_recipients: '',
    ai_send_time_optimization: false,
    ai_content_optimization: false,
  });
  const [loading, setLoading] = useState(false);
  
  // Data for dropdowns - supports 1000+ items
  const [profiles, setProfiles] = useState<SendingProfile[]>([]);
  const [allSegments, setAllSegments] = useState<Segment[]>([]); // All segments across all lists
  const [suppressionLists, setSuppressionLists] = useState<SuppressionList[]>([]);
  const [estimatedAudience, setEstimatedAudience] = useState<number | null>(null);

  // Load ALL segments, suppression lists, and ESP profiles on mount
  useEffect(() => {
    if (isOpen) {
      // Fetch sending profiles (ESPs)
      fetch('/api/mailing/sending-profiles')
        .then(r => r.json())
        .then(data => {
          const active = (data.profiles || []).filter((p: SendingProfile) => p.is_default || true);
          setProfiles(active);
          // Set default ESP with 100% quota
          const defaultProfile = active.find((p: SendingProfile) => p.is_default);
          if (defaultProfile) {
            setFormData(prev => ({
              ...prev,
              esp_quotas: [{ profile_id: defaultProfile.id, percentage: 100 }],
              from_name: defaultProfile.from_name,
              from_email: defaultProfile.from_email,
            }));
          }
        })
        .catch(() => {});

      // Fetch ALL segments (can be 1000+)
      fetch('/api/mailing/segments')
        .then(r => r.json())
        .then(data => {
          const segments = (data.segments || []).map((s: Segment) => ({
            ...s,
            count: s.subscriber_count || 0,
          }));
          setAllSegments(segments);
        })
        .catch(() => setAllSegments([]));

      // Fetch ALL suppression lists (can be 1000+)
      fetch('/api/mailing/suppression-lists')
        .then(r => r.json())
        .then(data => {
          const lists = (data.lists || data || []).map((l: SuppressionList) => ({
            ...l,
            count: l.entry_count || l.count || 0,
          }));
          setSuppressionLists(Array.isArray(lists) ? lists : []);
        })
        .catch(() => {
          setSuppressionLists([
            { id: 'global', name: 'Global Suppressions', type: 'global', count: 0 },
            { id: 'complaints', name: 'Complaint List', type: 'complaint', count: 0 },
            { id: 'bounces', name: 'Hard Bounces', type: 'bounce', count: 0 },
          ]);
        });
    }
  }, [isOpen]);

  // Calculate estimated audience when segments change
  useEffect(() => {
    if (formData.segment_ids.length > 0) {
      // Sum up subscriber counts from selected segments
      const selectedSegments = allSegments.filter(s => formData.segment_ids.includes(s.id));
      const total = selectedSegments.reduce((sum, s) => sum + (s.subscriber_count || 0), 0);
      setEstimatedAudience(total);
    } else {
      setEstimatedAudience(null);
    }
  }, [formData.segment_ids, allSegments]);

  // Add/update ESP quota
  const updateESPQuota = (profileId: string, percentage: number) => {
    setFormData(prev => {
      const existing = prev.esp_quotas.find(q => q.profile_id === profileId);
      if (existing) {
        return {
          ...prev,
          esp_quotas: prev.esp_quotas.map(q =>
            q.profile_id === profileId ? { ...q, percentage } : q
          ),
        };
      }
      return {
        ...prev,
        esp_quotas: [...prev.esp_quotas, { profile_id: profileId, percentage }],
      };
    });
  };

  // Remove ESP from quotas
  const removeESP = (profileId: string) => {
    setFormData(prev => ({
      ...prev,
      esp_quotas: prev.esp_quotas.filter(q => q.profile_id !== profileId),
    }));
  };

  // Calculate throttle rate
  const calculateThrottleRate = () => {
    if (!estimatedAudience || estimatedAudience === 0) return 0;
    if (formData.throttle_speed === 'custom') {
      const hours = formData.throttle_duration_hours || 1;
      return Math.ceil(estimatedAudience / (hours * 60)); // Per minute
    }
    const preset = THROTTLE_PRESETS.find(p => p.value === formData.throttle_speed);
    return preset?.rate || 100;
  };

  // Calculate estimated completion time
  const getEstimatedCompletionTime = () => {
    if (!estimatedAudience || estimatedAudience === 0) return 'N/A';
    const rate = calculateThrottleRate();
    if (rate === 0) return 'N/A';
    const minutes = estimatedAudience / rate;
    if (minutes < 60) return `~${Math.ceil(minutes)} minutes`;
    const hours = minutes / 60;
    if (hours < 24) return `~${hours.toFixed(1)} hours`;
    const days = hours / 24;
    return `~${days.toFixed(1)} days`;
  };

  if (!isOpen) return null;

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setLoading(true);
    try {
      // Calculate per-minute rate for custom throttling
      const throttleRate = formData.throttle_speed === 'custom'
        ? calculateThrottleRate()
        : THROTTLE_PRESETS.find(p => p.value === formData.throttle_speed)?.rate || 100;

      await onSubmit({
        ...formData,
        segment_ids: formData.segment_ids, // Segments to mail to
        suppression_list_ids: formData.suppression_list_ids, // Lists to suppress
        suppression_segment_ids: formData.suppression_segment_ids, // Segments to exclude
        esp_quotas: formData.esp_quotas,
        sending_profile_id: formData.esp_quotas[0]?.profile_id || null, // Primary ESP
        throttle_rate_per_minute: throttleRate,
        throttle_duration_hours: formData.throttle_speed === 'custom' ? formData.throttle_duration_hours : null,
        scheduled_at: formData.send_type === 'scheduled' && formData.scheduled_at 
          ? new Date(formData.scheduled_at).toISOString() 
          : null,
        max_recipients: formData.max_recipients ? parseInt(formData.max_recipients) : null,
        campaign_type: 'regular',
      });
      onClose();
      // Reset form
      setStep(1);
      setFormData({
        name: '',
        segment_ids: [],
        subject: '',
        from_name: '',
        from_email: '',
        html_content: '',
        plain_content: '',
        preview_text: '',
        send_type: 'instant',
        scheduled_at: '',
        throttle_speed: 'gentle',
        throttle_duration_hours: 8,
        suppression_list_ids: [],
        suppression_segment_ids: [],
        esp_quotas: [],
        max_recipients: '',
        ai_send_time_optimization: false,
        ai_content_optimization: false,
      });
    } finally {
      setLoading(false);
    }
  };

  const renderStep1 = () => (
    <>
      <div className="step-header">
        <h3>📝 Step 1: Campaign Basics & Audience</h3>
        <p>Define your campaign, select segments to mail, and suppression lists to exclude</p>
      </div>

      <div className="form-group">
        <label>Campaign Name *</label>
        <input
          type="text"
          value={formData.name}
          onChange={(e) => setFormData({ ...formData, name: e.target.value })}
          required
          placeholder="e.g., February Newsletter"
        />
      </div>

      <div className="form-section">
        <h4>🎯 Segments to Mail (Select Multiple)</h4>
        <p className="section-description">
          Search and select segments to send this campaign to. Segments can span multiple lists.
          {allSegments.length > 0 && <span className="count-badge">{allSegments.length} available</span>}
        </p>
        <SearchableMultiSelect
          items={allSegments.map(s => ({
            id: s.id,
            name: s.name,
            count: s.subscriber_count,
            description: s.description,
            list_name: s.list_name,
            tags: s.tags,
          }))}
          selectedIds={formData.segment_ids}
          onChange={(ids) => setFormData({ ...formData, segment_ids: ids })}
          placeholder="Click to search and select segments..."
          searchPlaceholder="Search by segment name, list, or tag..."
          emptyMessage="No segments found. Create segments from your lists first."
          maxDisplay={100}
        />
        {formData.segment_ids.length > 0 && (
          <div className="selection-summary">
            ✅ {formData.segment_ids.length} segment(s) selected
          </div>
        )}
      </div>

      <div className="form-section">
        <h4>🚫 Suppression Lists (Select Multiple)</h4>
        <p className="section-description">
          Search and select suppression lists - these addresses will NOT receive the email.
          {suppressionLists.length > 0 && <span className="count-badge">{suppressionLists.length} available</span>}
        </p>
        <SearchableMultiSelect
          items={suppressionLists.map(l => ({
            id: l.id,
            name: l.name,
            count: l.count || l.entry_count || 0,
            description: l.description,
          }))}
          selectedIds={formData.suppression_list_ids}
          onChange={(ids) => setFormData({ ...formData, suppression_list_ids: ids })}
          placeholder="Click to search and select suppression lists..."
          searchPlaceholder="Search suppression lists by name..."
          emptyMessage="No suppression lists found."
          maxDisplay={100}
          variant="suppression"
        />
        {formData.suppression_list_ids.length > 0 && (
          <div className="selection-summary warning">
            🚫 {formData.suppression_list_ids.length} suppression list(s) will be applied
          </div>
        )}
      </div>

      <div className="form-section">
        <h4>🚫 Suppression Segments (Exclude by Conditions)</h4>
        <p className="section-description">
          Contacts matching these segment conditions will be excluded from this send. 
          Use this to exclude audiences previously targeted or with specific data attributes.
          {allSegments.length > 0 && <span className="count-badge">{allSegments.length} available</span>}
        </p>
        <SearchableMultiSelect
          items={allSegments.map(s => ({
            id: s.id,
            name: s.name,
            count: s.subscriber_count || 0,
            description: s.description,
          }))}
          selectedIds={formData.suppression_segment_ids}
          onChange={(ids) => setFormData({ ...formData, suppression_segment_ids: ids })}
          placeholder="Click to search and select segments to exclude..."
          searchPlaceholder="Search segments by name..."
          emptyMessage="No segments found."
          maxDisplay={100}
          variant="suppression"
        />
        {formData.suppression_segment_ids.length > 0 && (
          <div className="selection-summary warning">
            🚫 {formData.suppression_segment_ids.length} segment(s) will be excluded
          </div>
        )}
      </div>

      {estimatedAudience !== null && estimatedAudience > 0 && (
        <div className="audience-estimate large">
          <div className="estimate-row">
            <span>📊 Total Recipients from Segments:</span>
            <strong>{estimatedAudience.toLocaleString()}</strong>
          </div>
          {(formData.suppression_list_ids.length > 0 || formData.suppression_segment_ids.length > 0) && (
            <div className="estimate-row warning">
              <span>🚫 After Suppression:</span>
              <strong>Calculated at send time</strong>
              <small>({formData.suppression_list_ids.length} list(s), {formData.suppression_segment_ids.length} segment(s))</small>
            </div>
          )}
        </div>
      )}

      <div className="form-section">
        <h4>✉️ Sender Details</h4>
        <div className="form-row">
          <div className="form-group">
            <label>From Name</label>
            <input
              type="text"
              value={formData.from_name}
              onChange={(e) => setFormData({ ...formData, from_name: e.target.value })}
              placeholder="Sender Name"
            />
          </div>
          <div className="form-group">
            <label>From Email</label>
            <input
              type="email"
              value={formData.from_email}
              onChange={(e) => setFormData({ ...formData, from_email: e.target.value })}
              placeholder="sender@example.com"
            />
          </div>
        </div>
      </div>

      <div className="form-group">
        <PersonalizedInput
          label="Subject Line"
          required
          value={formData.subject}
          onChange={(value) => setFormData({ ...formData, subject: value })}
          placeholder="e.g., Hey {{ first_name }}, check this out!"
          maxLength={60}
          showAISuggestions={true}
          aiSuggestionType="subject"
          aiContext={{
            htmlContent: formData.html_content,
            campaignType: 'newsletter',
            tone: 'professional',
          }}
        />
      </div>

      <div className="form-group">
        <PersonalizedInput
          label="Preview Text"
          value={formData.preview_text}
          onChange={(value) => setFormData({ ...formData, preview_text: value })}
          placeholder="e.g., {{ first_name }}, see what's new this week..."
          maxLength={150}
          hint="Text shown after subject in inbox preview"
          showAISuggestions={true}
          aiSuggestionType="preheader"
          aiContext={{
            subjectLine: formData.subject,
            htmlContent: formData.html_content,
            tone: 'professional',
          }}
        />
      </div>
    </>
  );

  const renderStep2 = () => (
    <>
      <div className="step-header">
        <h3>🎨 Step 2: Email Content</h3>
        <p>Design your email using the visual editor</p>
      </div>

      <div className="form-group">
        <label>Email Content *</label>
        <RichTextEditor
          value={formData.html_content}
          onChange={(html) => setFormData({ ...formData, html_content: html })}
          placeholder="Design your beautiful email..."
        />
      </div>

      <div className="form-group">
        <label>Plain Text Version (Auto-generated if empty)</label>
        <textarea
          value={formData.plain_content}
          onChange={(e) => setFormData({ ...formData, plain_content: e.target.value })}
          placeholder="Plain text fallback for email clients that don't support HTML"
          rows={4}
        />
      </div>
    </>
  );

  const renderStep3 = () => {
    const totalQuota = formData.esp_quotas.reduce((sum, q) => sum + q.percentage, 0);
    
    return (
    <>
      <div className="step-header">
        <h3>⚙️ Step 3: ESPs, Scheduling & Throttling</h3>
        <p>Select ESPs with quotas, schedule delivery, and configure smart throttling</p>
      </div>

      {/* ESP Selection with Quotas */}
      <div className="form-section">
        <h4>🚀 ESP Distribution (Select Multiple with Quotas)</h4>
        <p className="section-description">
          Distribute your campaign across multiple ESPs. Assign percentage quotas that total 100%.
        </p>
        
        <div className="esp-quota-grid">
          {profiles.map((profile) => {
            const quota = formData.esp_quotas.find(q => q.profile_id === profile.id);
            const isSelected = !!quota;
            
            return (
              <div key={profile.id} className={`esp-quota-card ${isSelected ? 'selected' : ''}`}>
                <div className="esp-header">
                  <label className="esp-checkbox">
                    <input
                      type="checkbox"
                      checked={isSelected}
                      onChange={(e) => {
                        if (e.target.checked) {
                          updateESPQuota(profile.id, 0);
                        } else {
                          removeESP(profile.id);
                        }
                      }}
                    />
                    <span className="esp-name">{profile.name}</span>
                    <span className="esp-vendor">{profile.vendor_type.toUpperCase()}</span>
                  </label>
                  {profile.is_default && <span className="default-badge">⭐ Default</span>}
                </div>
                
                {isSelected && (
                  <div className="esp-quota-input">
                    <label>Quota %</label>
                    <input
                      type="number"
                      min={0}
                      max={100}
                      value={quota?.percentage || 0}
                      onChange={(e) => updateESPQuota(profile.id, parseInt(e.target.value) || 0)}
                    />
                    <span className="quota-preview">
                      ~{estimatedAudience ? Math.round(estimatedAudience * (quota?.percentage || 0) / 100).toLocaleString() : 0} emails
                    </span>
                  </div>
                )}
              </div>
            );
          })}
        </div>
        
        <div className={`quota-total ${totalQuota === 100 ? 'valid' : 'invalid'}`}>
          <span>Total: {totalQuota}%</span>
          {totalQuota !== 100 && <span className="warning">⚠️ Must equal 100%</span>}
          {totalQuota === 100 && <span className="valid-check">✓ Valid</span>}
        </div>
      </div>

      {/* Schedule */}
      <div className="form-section">
        <h4>📅 Send Schedule</h4>
        <div className="radio-group">
          <label className={`radio-card ${formData.send_type === 'instant' ? 'selected' : ''}`}>
            <input
              type="radio"
              name="send_type"
              value="instant"
              checked={formData.send_type === 'instant'}
              onChange={(e) => setFormData({ ...formData, send_type: e.target.value })}
            />
            <div className="radio-content">
              <span className="radio-icon">⚡</span>
              <div>
                <strong>Send Immediately</strong>
                <small>Start sending as soon as you click Create</small>
              </div>
            </div>
          </label>
          <label className={`radio-card ${formData.send_type === 'scheduled' ? 'selected' : ''}`}>
            <input
              type="radio"
              name="send_type"
              value="scheduled"
              checked={formData.send_type === 'scheduled'}
              onChange={(e) => setFormData({ ...formData, send_type: e.target.value })}
            />
            <div className="radio-content">
              <span className="radio-icon">📅</span>
              <div>
                <strong>Schedule for Later</strong>
                <small>Pick a specific date and time</small>
              </div>
            </div>
          </label>
        </div>

        {formData.send_type === 'scheduled' && (
          <div className="form-group schedule-datetime">
            <label>📆 Schedule Date & Time *</label>
            <input
              type="datetime-local"
              value={formData.scheduled_at}
              onChange={(e) => setFormData({ ...formData, scheduled_at: e.target.value })}
              min={new Date().toISOString().slice(0, 16)}
              required={formData.send_type === 'scheduled'}
            />
            {formData.scheduled_at && (
              <div className="schedule-preview">
                Will send on: <strong>{new Date(formData.scheduled_at).toLocaleString()}</strong>
              </div>
            )}
          </div>
        )}
      </div>

      {/* Smart Throttling */}
      <div className="form-section">
        <h4>🚦 Smart Throttling</h4>
        <p className="section-description">
          Control delivery speed. For large lists, use custom duration to spread sends over hours.
        </p>
        
        <div className="throttle-options-grid">
          {THROTTLE_PRESETS.map((preset) => (
            <label 
              key={preset.value}
              className={`throttle-card ${formData.throttle_speed === preset.value ? 'selected' : ''}`}
            >
              <input
                type="radio"
                name="throttle_speed"
                value={preset.value}
                checked={formData.throttle_speed === preset.value}
                onChange={(e) => setFormData({ ...formData, throttle_speed: e.target.value })}
              />
              <div className="throttle-content">
                <strong>{preset.label}</strong>
                <small>{preset.description}</small>
                {preset.rate > 0 && <span className="rate-badge">{preset.rate}/min</span>}
              </div>
            </label>
          ))}
        </div>

        {formData.throttle_speed === 'custom' && (
          <div className="custom-throttle-section">
            <div className="form-group">
              <label>⏱️ Spread delivery over how many hours?</label>
              <div className="duration-input">
                <input
                  type="number"
                  min={1}
                  max={168}
                  value={formData.throttle_duration_hours}
                  onChange={(e) => setFormData({ ...formData, throttle_duration_hours: parseInt(e.target.value) || 1 })}
                />
                <span>hours</span>
              </div>
            </div>
            
            {estimatedAudience && estimatedAudience > 0 && (
              <div className="throttle-calculation">
                <div className="calc-row">
                  <span>📊 Total Recipients:</span>
                  <strong>{estimatedAudience.toLocaleString()}</strong>
                </div>
                <div className="calc-row">
                  <span>⏱️ Duration:</span>
                  <strong>{formData.throttle_duration_hours} hours</strong>
                </div>
                <div className="calc-row highlight">
                  <span>📈 Send Rate:</span>
                  <strong>{calculateThrottleRate().toLocaleString()}/min</strong>
                </div>
                <div className="calc-row highlight">
                  <span>⏰ Per Hour:</span>
                  <strong>~{Math.round(estimatedAudience / formData.throttle_duration_hours).toLocaleString()}</strong>
                </div>
              </div>
            )}
          </div>
        )}

        {estimatedAudience && estimatedAudience > 0 && (
          <div className="delivery-estimate">
            <span>🏁 Estimated Completion:</span>
            <strong>{getEstimatedCompletionTime()}</strong>
          </div>
        )}
      </div>

      {/* Advanced Options */}
      <div className="form-section">
        <h4>📊 Advanced Options</h4>
        <div className="form-row">
          <div className="form-group">
            <label>Max Recipients (Optional)</label>
            <input
              type="number"
              value={formData.max_recipients}
              onChange={(e) => setFormData({ ...formData, max_recipients: e.target.value })}
              placeholder="Leave empty for no limit"
              min={1}
            />
            <small>Limit total emails sent (useful for testing)</small>
          </div>
        </div>

        <div className="checkbox-group ai-options-inline">
          <label>
            <input
              type="checkbox"
              checked={formData.ai_send_time_optimization}
              onChange={(e) => setFormData({ ...formData, ai_send_time_optimization: e.target.checked })}
            />
            <span>🤖 AI Send Time Optimization</span>
          </label>
          <label>
            <input
              type="checkbox"
              checked={formData.ai_content_optimization}
              onChange={(e) => setFormData({ ...formData, ai_content_optimization: e.target.checked })}
            />
            <span>🤖 AI Content Optimization</span>
          </label>
        </div>
      </div>
    </>
  );
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal-content extra-large" onClick={(e) => e.stopPropagation()}>
        <div className="modal-header">
          <h2>✨ Create Campaign</h2>
          <div className="step-indicator">
            <span className={step >= 1 ? 'active' : ''}>1. Basics</span>
            <span className={step >= 2 ? 'active' : ''}>2. Content</span>
            <span className={step >= 3 ? 'active' : ''}>3. Schedule</span>
          </div>
        </div>

        <form onSubmit={handleSubmit}>
          <div className="modal-body">
            {step === 1 && renderStep1()}
            {step === 2 && renderStep2()}
            {step === 3 && renderStep3()}
          </div>

          <div className="modal-actions">
            <button type="button" className="cancel-btn" onClick={onClose}>
              Cancel
            </button>
            
            {step > 1 && (
              <button type="button" className="back-btn" onClick={() => setStep(step - 1)}>
                ← Back
              </button>
            )}
            
            {step < 3 ? (
              <button 
                type="button" 
                className="next-btn" 
                onClick={() => setStep(step + 1)}
                disabled={step === 1 && (!formData.name || !formData.subject || formData.segment_ids.length === 0)}
              >
                Next →
              </button>
            ) : (
              <button type="submit" className="submit-btn" disabled={loading}>
                {loading ? 'Creating...' : formData.send_type === 'scheduled' ? '📅 Schedule Campaign' : '🚀 Create & Send'}
              </button>
            )}
          </div>
        </form>
      </div>
    </div>
  );
};

const getStatusInfo = (status: string) => {
  const statusMap: Record<string, { color: string; label: string }> = {
    draft: { color: '#6b7280', label: 'Draft' },
    queued: { color: '#f59e0b', label: 'Queued' },
    sending: { color: '#3b82f6', label: 'Sending' },
    sent: { color: '#22c55e', label: 'Sent' },
    completed: { color: '#22c55e', label: 'Completed' },
    paused: { color: '#f59e0b', label: 'Paused' },
    failed: { color: '#ef4444', label: 'Failed' },
  };
  return statusMap[status] || { color: '#6b7280', label: status };
};

// Campaign Details Modal
interface CampaignDetailsModalProps {
  campaign: Campaign | null;
  onClose: () => void;
}

const CampaignDetailsModal: React.FC<CampaignDetailsModalProps> = ({ campaign, onClose }) => {
  const [stats, setStats] = useState<any>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (campaign) {
      setLoading(true);
      fetch(`/api/mailing/campaigns/${campaign.id}/stats`)
        .then(r => r.json())
        .then(data => setStats(data))
        .catch(() => {})
        .finally(() => setLoading(false));
    }
  }, [campaign]);

  if (!campaign) return null;

  const statusInfo = getStatusInfo(campaign.status);

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal-content large" onClick={e => e.stopPropagation()}>
        <div className="details-header">
          <div>
            <h2>{campaign.name}</h2>
            <p className="details-subject">{campaign.subject}</p>
          </div>
          <span 
            className="status-badge large"
            style={{ backgroundColor: statusInfo.color + '20', color: statusInfo.color }}
          >
            {statusInfo.label}
          </span>
        </div>

        <div className="details-grid">
          <div className="details-section">
            <h4>📧 Email Details</h4>
            <div className="details-row">
              <span className="label">From:</span>
              <span>{campaign.from_name} &lt;{campaign.from_email}&gt;</span>
            </div>
            <div className="details-row">
              <span className="label">Subject:</span>
              <span>{campaign.subject}</span>
            </div>
            {campaign.preview_text && (
              <div className="details-row">
                <span className="label">Preview:</span>
                <span>{campaign.preview_text}</span>
              </div>
            )}
            <div className="details-row">
              <span className="label">Created:</span>
              <span>{new Date(campaign.created_at).toLocaleString()}</span>
            </div>
            {campaign.started_at && (
              <div className="details-row">
                <span className="label">Started:</span>
                <span>{new Date(campaign.started_at).toLocaleString()}</span>
              </div>
            )}
            {campaign.completed_at && (
              <div className="details-row">
                <span className="label">Completed:</span>
                <span>{new Date(campaign.completed_at).toLocaleString()}</span>
              </div>
            )}
          </div>

          <div className="details-section">
            <h4>📊 Performance Metrics</h4>
            {loading ? (
              <div className="loading-small">Loading stats...</div>
            ) : (
              <div className="stats-grid">
                <div className="stat-box">
                  <span className="stat-number">{(campaign.sent_count || 0).toLocaleString()}</span>
                  <span className="stat-label">Sent</span>
                </div>
                <div className="stat-box">
                  <span className="stat-number">{(campaign.open_count || 0).toLocaleString()}</span>
                  <span className="stat-label">Opens</span>
                  <span className="stat-rate">{stats?.open_rate?.toFixed(1) || 0}%</span>
                </div>
                <div className="stat-box">
                  <span className="stat-number">{(campaign.click_count || 0).toLocaleString()}</span>
                  <span className="stat-label">Clicks</span>
                  <span className="stat-rate">{stats?.click_rate?.toFixed(1) || 0}%</span>
                </div>
                <div className="stat-box">
                  <span className="stat-number">{(campaign.bounce_count || 0).toLocaleString()}</span>
                  <span className="stat-label">Bounces</span>
                  <span className="stat-rate">{stats?.bounce_rate?.toFixed(1) || 0}%</span>
                </div>
                <div className="stat-box">
                  <span className="stat-number">{(campaign.complaint_count || 0).toLocaleString()}</span>
                  <span className="stat-label">Complaints</span>
                  <span className="stat-rate">{stats?.complaint_rate?.toFixed(1) || 0}%</span>
                </div>
                <div className="stat-box">
                  <span className="stat-number">${(campaign.revenue || 0).toFixed(2)}</span>
                  <span className="stat-label">Revenue</span>
                </div>
              </div>
            )}
          </div>
        </div>

        {campaign.html_content && (
          <div className="details-section">
            <h4>👁️ Email Preview</h4>
            <div className="email-preview">
              <iframe 
                srcDoc={campaign.html_content}
                title="Email Preview"
                sandbox="allow-same-origin"
              />
            </div>
          </div>
        )}

        <div className="modal-actions">
          <button type="button" className="cancel-btn" onClick={onClose}>
            Close
          </button>
        </div>
      </div>
    </div>
  );
};

export const CampaignsManager: React.FC = () => {
  const { campaigns, loading, error, fetchCampaigns, createCampaign, sendCampaign } = useCampaigns();
  const { lists, fetchLists } = useLists();
  const [showCreateModal, setShowCreateModal] = useState(false);
  const [selectedCampaign, setSelectedCampaign] = useState<Campaign | null>(null);
  const [sending, setSending] = useState<string | null>(null);
  
  // A/B Testing State
  const [activeTab, setActiveTab] = useState<'campaigns' | 'ab-tests'>('campaigns');
  const [showABTestBuilder, setShowABTestBuilder] = useState(false);
  const [selectedTestId, setSelectedTestId] = useState<string | null>(null);
  const [abTestFromCampaign, setAbTestFromCampaign] = useState<{
    campaignId: string;
    name: string;
    subject: string;
    from_name: string;
    html_content: string;
    list_id?: string;
    segment_id?: string;
  } | null>(null);

  useEffect(() => {
    fetchCampaigns();
    fetchLists();
  }, [fetchCampaigns, fetchLists]);
  
  // Create A/B test from an existing campaign
  const handleCreateABTestFromCampaign = (campaign: Campaign) => {
    setAbTestFromCampaign({
      campaignId: campaign.id,
      name: campaign.name,
      subject: campaign.subject,
      from_name: campaign.from_name,
      html_content: campaign.html_content || '',
      list_id: campaign.list_id || undefined,
      segment_id: campaign.segment_id || undefined,
    });
    setShowABTestBuilder(true);
  };

  const handleCreateCampaign = async (campaign: Partial<Campaign>) => {
    await createCampaign(campaign);
  };

  const handleSend = async (campaignId: string) => {
    if (!confirm('Are you sure you want to send this campaign?')) return;
    
    setSending(campaignId);
    try {
      const result = await sendCampaign(campaignId);
      alert(`Campaign sending initiated! ${result.total_recipients} recipients queued.`);
      fetchCampaigns();
    } catch (error) {
      alert('Failed to send campaign');
    } finally {
      setSending(null);
    }
  };

  const calculateRate = (count: number, total: number) => {
    if (total === 0) return 0;
    return ((count / total) * 100).toFixed(1);
  };

  return (
    <div className="campaigns-manager">
      <header className="campaigns-header">
        <div>
          <h1>Campaigns</h1>
          <p>Create and manage your email campaigns</p>
        </div>
        <div className="header-actions">
          {activeTab === 'campaigns' && (
            <button className="create-btn" onClick={() => setShowCreateModal(true)}>
              + Create Campaign
            </button>
          )}
          {activeTab === 'ab-tests' && (
            <button className="create-btn ab-test" onClick={() => setShowABTestBuilder(true)}>
              + Create A/B Test
            </button>
          )}
        </div>
      </header>

      {/* Tab Navigation */}
      <div className="tab-navigation">
        <button 
          className={`tab-btn ${activeTab === 'campaigns' ? 'active' : ''}`}
          onClick={() => setActiveTab('campaigns')}
        >
          <span className="tab-icon">📧</span>
          Campaigns
        </button>
        <button 
          className={`tab-btn ${activeTab === 'ab-tests' ? 'active' : ''}`}
          onClick={() => setActiveTab('ab-tests')}
        >
          <span className="tab-icon">🧪</span>
          A/B Tests
        </button>
      </div>

      {/* Campaigns Tab */}
      {activeTab === 'campaigns' && (
        <>
          {error && <div className="error-message">{error}</div>}

          {loading && campaigns.length === 0 && (
            <div className="loading-state">Loading campaigns...</div>
          )}

          <div className="campaigns-list">
            {campaigns.map((campaign) => {
              const statusInfo = getStatusInfo(campaign.status);
              return (
                <div key={campaign.id} className="campaign-card">
                  <div className="campaign-main">
                    <div className="campaign-info">
                      <h3>{campaign.name}</h3>
                      <p className="campaign-subject">{campaign.subject}</p>
                      <div className="campaign-meta">
                        <span
                          className="status-badge"
                          style={{ backgroundColor: statusInfo.color + '20', color: statusInfo.color }}
                        >
                          {statusInfo.label}
                        </span>
                        <span className="campaign-date">
                          Created {new Date(campaign.created_at).toLocaleDateString()}
                        </span>
                      </div>
                    </div>

                    <div className="campaign-stats">
                      <div className="stat-item">
                        <span className="stat-label">Sent</span>
                        <span className="stat-value">{(campaign.sent_count || 0).toLocaleString()}</span>
                      </div>
                      <div className="stat-item">
                        <span className="stat-label">Opens</span>
                        <span className="stat-value">
                          {(campaign.open_count || 0).toLocaleString()}
                          <span className="stat-rate">
                            ({calculateRate(campaign.open_count || 0, campaign.sent_count || 0)}%)
                          </span>
                        </span>
                      </div>
                      <div className="stat-item">
                        <span className="stat-label">Clicks</span>
                        <span className="stat-value">
                          {(campaign.click_count || 0).toLocaleString()}
                          <span className="stat-rate">
                            ({calculateRate(campaign.click_count || 0, campaign.sent_count || 0)}%)
                          </span>
                        </span>
                      </div>
                      <div className="stat-item">
                        <span className="stat-label">Revenue</span>
                        <span className="stat-value revenue">${(campaign.revenue || 0).toFixed(2)}</span>
                      </div>
                    </div>

                    <div className="campaign-actions">
                      {campaign.status === 'draft' && (
                        <>
                          <button
                            className="send-btn"
                            onClick={() => handleSend(campaign.id)}
                            disabled={sending === campaign.id}
                          >
                            {sending === campaign.id ? 'Sending...' : 'Send'}
                          </button>
                          <button 
                            className="ab-test-btn"
                            onClick={() => handleCreateABTestFromCampaign(campaign)}
                            title="Create A/B Test from this campaign"
                          >
                            🧪 A/B Test
                          </button>
                        </>
                      )}
                      {campaign.status === 'sending' && (
                        <div className="sending-indicator">
                          <div className="pulse" />
                          Sending...
                        </div>
                      )}
                      <button className="view-btn" onClick={() => setSelectedCampaign(campaign)}>View Details</button>
                    </div>
                  </div>

                  {campaign.sent_count > 0 && (
                    <div className="campaign-progress">
                      <div className="progress-bar">
                        <div
                          className="progress-fill open"
                          style={{ width: `${calculateRate(campaign.open_count, campaign.sent_count)}%` }}
                        />
                      </div>
                      <div className="progress-labels">
                        <span>Open Rate: {calculateRate(campaign.open_count, campaign.sent_count)}%</span>
                        <span>Click Rate: {calculateRate(campaign.click_count, campaign.sent_count)}%</span>
                        <span>Bounce Rate: {calculateRate(campaign.bounce_count, campaign.sent_count)}%</span>
                      </div>
                    </div>
                  )}
                </div>
              );
            })}
          </div>
        </>
      )}

      {/* A/B Tests Tab */}
      {activeTab === 'ab-tests' && (
        <ABTestList 
          onCreateNew={() => setShowABTestBuilder(true)}
          onViewResults={(testId) => setSelectedTestId(testId)}
        />
      )}

      {/* Modals */}
      <CreateCampaignModal
        isOpen={showCreateModal}
        lists={lists}
        onClose={() => setShowCreateModal(false)}
        onSubmit={handleCreateCampaign}
      />

      <CampaignDetailsModal
        campaign={selectedCampaign}
        onClose={() => setSelectedCampaign(null)}
      />

      {/* A/B Test Builder Modal */}
      {showABTestBuilder && (
        <ABTestBuilder
          campaignId={abTestFromCampaign?.campaignId}
          campaignData={abTestFromCampaign ? {
            name: abTestFromCampaign.name,
            subject: abTestFromCampaign.subject,
            from_name: abTestFromCampaign.from_name,
            html_content: abTestFromCampaign.html_content,
            list_id: abTestFromCampaign.list_id,
            segment_id: abTestFromCampaign.segment_id,
          } : undefined}
          onClose={() => {
            setShowABTestBuilder(false);
            setAbTestFromCampaign(null);
          }}
          onSuccess={() => {
            setShowABTestBuilder(false);
            setAbTestFromCampaign(null);
            setActiveTab('ab-tests'); // Switch to A/B tests tab
          }}
        />
      )}

      {/* A/B Test Results Modal */}
      {selectedTestId && (
        <ABTestResults
          testId={selectedTestId}
          onClose={() => setSelectedTestId(null)}
        />
      )}
    </div>
  );
};

export default CampaignsManager;
