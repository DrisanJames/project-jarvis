import React, { useState, useEffect, useCallback } from 'react';
import './ABTestBuilder.css';
import { ABTestInsights, ABTestInsightsButton } from './ABTestInsights';

// =============================================================================
// A/B SPLIT TESTING UI - Enterprise Grade
// =============================================================================
// Features:
// - Multiple test types (subject, content, from_name, send_time, full variants)
// - Visual variant editor with side-by-side comparison
// - Real-time results dashboard with statistical significance
// - Automatic winner selection with confidence levels
// - Integration with campaign builder workflow
// =============================================================================

// Types
interface ABVariant {
  id: string;
  variant_name: string;
  variant_label: string;
  subject: string;
  from_name: string;
  preheader: string;
  html_content: string;
  split_percent: number;
  is_control: boolean;
  is_winner: boolean;
  sent_count: number;
  open_count: number;
  unique_open_count: number;
  click_count: number;
  unique_click_count: number;
  bounce_count: number;
  unsubscribe_count: number;
  open_rate: number;
  click_rate: number;
  click_to_open_rate: number;
  confidence_score: number;
  lift_vs_control: number;
  statistical_significance: boolean;
}

interface ABTest {
  id: string;
  name: string;
  description: string;
  test_type: string;
  status: string;
  total_audience_size: number;
  test_sample_size: number;
  remaining_audience_size: number;
  winner_metric: string;
  winner_wait_hours: number;
  winner_auto_select: boolean;
  winner_confidence_threshold: number;
  winner_variant_id: string | null;
  variants: ABVariant[];
  created_at: string;
}

interface Segment {
  id: string;
  name: string;
  subscriber_count: number;
  list_name?: string;
}

interface ABTestBuilderProps {
  campaignId?: string;
  campaignData?: {
    name: string;
    subject: string;
    from_name: string;
    html_content: string;
    list_id?: string;
    segment_id?: string;
  };
  onClose: () => void;
  onSuccess?: (test: ABTest) => void;
}

// Test type configurations
const TEST_TYPES = [
  {
    value: 'subject_line',
    label: 'Subject Line',
    icon: '📝',
    description: 'Test different subject lines to maximize open rates',
    metric: 'open_rate',
  },
  {
    value: 'from_name',
    label: 'From Name',
    icon: '👤',
    description: 'Test different sender names to build trust',
    metric: 'open_rate',
  },
  {
    value: 'content',
    label: 'Content',
    icon: '🎨',
    description: 'Test different email designs and copy',
    metric: 'click_rate',
  },
  {
    value: 'send_time',
    label: 'Send Time',
    icon: '⏰',
    description: 'Find the optimal time to send your emails',
    metric: 'open_rate',
  },
  {
    value: 'preheader',
    label: 'Preview Text',
    icon: '👁️',
    description: 'Test different preview/preheader text',
    metric: 'open_rate',
  },
  {
    value: 'full_variant',
    label: 'Full Variant',
    icon: '📧',
    description: 'Test completely different emails',
    metric: 'click_rate',
  },
];

const WINNER_METRICS = [
  { value: 'open_rate', label: 'Open Rate', description: 'Best for subject line & from name tests' },
  { value: 'click_rate', label: 'Click Rate', description: 'Best for content & CTA tests' },
  { value: 'click_to_open_rate', label: 'Click-to-Open Rate', description: 'Engagement among openers' },
  { value: 'conversion_rate', label: 'Conversion Rate', description: 'Requires conversion tracking' },
  { value: 'unsubscribe_rate', label: 'Unsubscribe Rate', description: 'Lower is better' },
];

export const ABTestBuilder: React.FC<ABTestBuilderProps> = ({
  campaignId,
  campaignData,
  onClose,
  onSuccess,
}) => {
  const [step, setStep] = useState(1);
  const [loading, setLoading] = useState(false);
  const [segments, setSegments] = useState<Segment[]>([]);
  
  // Form state
  const [formData, setFormData] = useState({
    name: campaignData?.name ? `${campaignData.name} - A/B Test` : '',
    description: '',
    test_type: 'subject_line',
    segment_id: campaignData?.segment_id || '',
    test_sample_percent: 20,
    winner_metric: 'open_rate',
    winner_wait_hours: 4,
    winner_auto_select: true,
    winner_confidence_threshold: 0.95,
    winner_min_sample_size: 100,
  });
  
  // Variants state
  const [variants, setVariants] = useState<Partial<ABVariant>[]>([
    {
      variant_name: 'A',
      variant_label: 'Control',
      subject: campaignData?.subject || '',
      from_name: campaignData?.from_name || '',
      preheader: '',
      html_content: campaignData?.html_content || '',
      split_percent: 50,
      is_control: true,
    },
    {
      variant_name: 'B',
      variant_label: 'Variant B',
      subject: campaignData?.subject || '',
      from_name: campaignData?.from_name || '',
      preheader: '',
      html_content: campaignData?.html_content || '',
      split_percent: 50,
      is_control: false,
    },
  ]);

  // Load segments
  useEffect(() => {
    fetch('/api/mailing/segments')
      .then(r => r.json())
      .then(data => setSegments(data.segments || []))
      .catch(() => {});
  }, []);

  // Get selected segment details
  const selectedSegment = segments.find(s => s.id === formData.segment_id);
  const testSampleSize = selectedSegment
    ? Math.floor(selectedSegment.subscriber_count * formData.test_sample_percent / 100)
    : 0;
  const remainingAudience = selectedSegment
    ? selectedSegment.subscriber_count - testSampleSize
    : 0;

  // Update variant split percentages when variants change
  const updateSplitPercentages = useCallback(() => {
    const equalSplit = Math.floor(100 / variants.length);
    const remainder = 100 - (equalSplit * variants.length);
    
    setVariants(prev => prev.map((v, i) => ({
      ...v,
      split_percent: equalSplit + (i === 0 ? remainder : 0),
    })));
  }, [variants.length]);

  // Add variant
  const addVariant = () => {
    const nextLetter = String.fromCharCode(65 + variants.length); // A, B, C, D...
    setVariants([...variants, {
      variant_name: nextLetter,
      variant_label: `Variant ${nextLetter}`,
      subject: campaignData?.subject || '',
      from_name: campaignData?.from_name || '',
      preheader: '',
      html_content: campaignData?.html_content || '',
      split_percent: 0,
      is_control: false,
    }]);
  };

  // Remove variant
  const removeVariant = (index: number) => {
    if (variants.length <= 2) return; // Minimum 2 variants
    setVariants(variants.filter((_, i) => i !== index));
  };

  // Update variant
  const updateVariant = (index: number, field: string, value: any) => {
    setVariants(prev => prev.map((v, i) => 
      i === index ? { ...v, [field]: value } : v
    ));
  };

  // Create test
  const handleSubmit = async () => {
    setLoading(true);
    try {
      const response = await fetch('/api/mailing/ab-tests', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          ...formData,
          variants: variants.map(v => ({
            variant_name: v.variant_name,
            variant_label: v.variant_label,
            subject: v.subject,
            from_name: v.from_name,
            preheader: v.preheader,
            html_content: v.html_content,
            split_percent: v.split_percent,
            is_control: v.is_control,
          })),
        }),
      });
      
      if (!response.ok) throw new Error('Failed to create test');
      
      const data = await response.json();
      
      // If created from a campaign, link them
      if (campaignId) {
        await fetch(`/api/mailing/campaigns/${campaignId}/create-ab-test`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ test_id: data.id }),
        });
      }
      
      onSuccess?.(data);
      onClose();
    } catch (error) {
      console.error('Error creating A/B test:', error);
      alert('Failed to create A/B test');
    } finally {
      setLoading(false);
    }
  };

  // Render functions for each step
  const renderStep1 = () => (
    <div className="ab-step-content">
      <div className="step-header">
        <h3>Step 1: Test Configuration</h3>
        <p>Choose what you want to test and configure the test settings</p>
      </div>

      <div className="form-group">
        <label>Test Name *</label>
        <input
          type="text"
          value={formData.name}
          onChange={e => setFormData({ ...formData, name: e.target.value })}
          placeholder="e.g., February Newsletter Subject Test"
          required
        />
      </div>

      <div className="form-group">
        <label>Description</label>
        <textarea
          value={formData.description}
          onChange={e => setFormData({ ...formData, description: e.target.value })}
          placeholder="Describe what you're testing and why..."
          rows={2}
        />
      </div>

      <div className="form-section">
        <h4>What do you want to test?</h4>
        <div className="test-type-grid">
          {TEST_TYPES.map(type => (
            <label
              key={type.value}
              className={`test-type-card ${formData.test_type === type.value ? 'selected' : ''}`}
            >
              <input
                type="radio"
                name="test_type"
                value={type.value}
                checked={formData.test_type === type.value}
                onChange={e => setFormData({ ...formData, test_type: e.target.value })}
              />
              <span className="type-icon">{type.icon}</span>
              <div className="type-info">
                <strong>{type.label}</strong>
                <small>{type.description}</small>
              </div>
            </label>
          ))}
        </div>
      </div>

      <div className="form-section">
        <h4>Select Audience</h4>
        <div className="form-group">
          <label>Segment to Test</label>
          <select
            value={formData.segment_id}
            onChange={e => setFormData({ ...formData, segment_id: e.target.value })}
            required
          >
            <option value="">Select a segment...</option>
            {segments.map(seg => (
              <option key={seg.id} value={seg.id}>
                {seg.name} ({seg.subscriber_count?.toLocaleString()} contacts)
              </option>
            ))}
          </select>
        </div>

        {selectedSegment && (
          <div className="audience-breakdown">
            <div className="breakdown-item">
              <span className="breakdown-label">Total Audience</span>
              <span className="breakdown-value">{selectedSegment.subscriber_count.toLocaleString()}</span>
            </div>
            <div className="breakdown-item">
              <span className="breakdown-label">Test Sample ({formData.test_sample_percent}%)</span>
              <span className="breakdown-value highlight">{testSampleSize.toLocaleString()}</span>
            </div>
            <div className="breakdown-item">
              <span className="breakdown-label">Winner Audience</span>
              <span className="breakdown-value">{remainingAudience.toLocaleString()}</span>
            </div>
          </div>
        )}

        <div className="form-group">
          <label>Test Sample Size: {formData.test_sample_percent}%</label>
          <input
            type="range"
            min={10}
            max={50}
            value={formData.test_sample_percent}
            onChange={e => setFormData({ ...formData, test_sample_percent: parseInt(e.target.value) })}
          />
          <div className="range-labels">
            <span>10%</span>
            <span>30%</span>
            <span>50%</span>
          </div>
        </div>
      </div>
    </div>
  );

  const renderStep2 = () => {
    const testType = TEST_TYPES.find(t => t.value === formData.test_type);
    
    return (
      <div className="ab-step-content">
        <div className="step-header">
          <h3>Step 2: Create Variants</h3>
          <p>Create {variants.length} versions to test - each will be sent to a portion of your test sample</p>
        </div>

        <div className="variants-header">
          <div className="variant-count">
            {variants.length} Variants
          </div>
          {variants.length < 5 && (
            <button type="button" className="add-variant-btn" onClick={addVariant}>
              + Add Variant
            </button>
          )}
          <button type="button" className="equalize-btn" onClick={updateSplitPercentages}>
            Equalize Splits
          </button>
        </div>

        <div className="variants-container">
          {variants.map((variant, index) => (
            <div key={index} className={`variant-card ${variant.is_control ? 'control' : ''}`}>
              <div className="variant-header">
                <div className="variant-badge">
                  {variant.variant_name}
                  {variant.is_control && <span className="control-badge">Control</span>}
                </div>
                <input
                  type="text"
                  className="variant-label-input"
                  value={variant.variant_label}
                  onChange={e => updateVariant(index, 'variant_label', e.target.value)}
                  placeholder="Variant name..."
                />
                {variants.length > 2 && !variant.is_control && (
                  <button
                    type="button"
                    className="remove-variant-btn"
                    onClick={() => removeVariant(index)}
                  >
                    ×
                  </button>
                )}
              </div>

              <div className="variant-content">
                {/* Subject Line Test */}
                {(formData.test_type === 'subject_line' || formData.test_type === 'full_variant') && (
                  <div className="form-group">
                    <label>Subject Line</label>
                    <input
                      type="text"
                      value={variant.subject}
                      onChange={e => updateVariant(index, 'subject', e.target.value)}
                      placeholder="Enter subject line..."
                    />
                    <small>{(variant.subject || '').length}/50 characters</small>
                  </div>
                )}

                {/* From Name Test */}
                {(formData.test_type === 'from_name' || formData.test_type === 'full_variant') && (
                  <div className="form-group">
                    <label>From Name</label>
                    <input
                      type="text"
                      value={variant.from_name}
                      onChange={e => updateVariant(index, 'from_name', e.target.value)}
                      placeholder="Sender name..."
                    />
                  </div>
                )}

                {/* Preheader Test */}
                {(formData.test_type === 'preheader' || formData.test_type === 'full_variant') && (
                  <div className="form-group">
                    <label>Preview Text (Preheader)</label>
                    <input
                      type="text"
                      value={variant.preheader}
                      onChange={e => updateVariant(index, 'preheader', e.target.value)}
                      placeholder="Preview text shown in inbox..."
                      maxLength={150}
                    />
                  </div>
                )}

                {/* Content Test */}
                {(formData.test_type === 'content' || formData.test_type === 'full_variant') && (
                  <div className="form-group">
                    <label>Email Content</label>
                    <textarea
                      value={variant.html_content}
                      onChange={e => updateVariant(index, 'html_content', e.target.value)}
                      placeholder="HTML content..."
                      rows={6}
                      className="content-textarea"
                    />
                    <div className="content-preview-btn">
                      <button 
                        type="button" 
                        onClick={() => window.open('', '_blank')?.document.write(variant.html_content || '')}
                      >
                        Preview
                      </button>
                    </div>
                  </div>
                )}

                {/* Send Time Test */}
                {formData.test_type === 'send_time' && (
                  <div className="form-row">
                    <div className="form-group">
                      <label>Send Hour (24h)</label>
                      <select
                        value={(variant as any).send_hour || 9}
                        onChange={e => updateVariant(index, 'send_hour', parseInt(e.target.value))}
                      >
                        {Array.from({ length: 24 }, (_, i) => (
                          <option key={i} value={i}>
                            {i.toString().padStart(2, '0')}:00
                          </option>
                        ))}
                      </select>
                    </div>
                    <div className="form-group">
                      <label>Day of Week</label>
                      <select
                        value={(variant as any).send_day_of_week || 1}
                        onChange={e => updateVariant(index, 'send_day_of_week', parseInt(e.target.value))}
                      >
                        <option value={0}>Sunday</option>
                        <option value={1}>Monday</option>
                        <option value={2}>Tuesday</option>
                        <option value={3}>Wednesday</option>
                        <option value={4}>Thursday</option>
                        <option value={5}>Friday</option>
                        <option value={6}>Saturday</option>
                      </select>
                    </div>
                  </div>
                )}
              </div>

              <div className="variant-split">
                <label>Traffic Split</label>
                <div className="split-input">
                  <input
                    type="number"
                    min={0}
                    max={100}
                    value={variant.split_percent}
                    onChange={e => updateVariant(index, 'split_percent', parseInt(e.target.value) || 0)}
                  />
                  <span>%</span>
                </div>
                {selectedSegment && (
                  <small>~{Math.floor(testSampleSize * (variant.split_percent || 0) / 100).toLocaleString()} recipients</small>
                )}
              </div>
            </div>
          ))}
        </div>

        {/* Split validation */}
        {(() => {
          const totalSplit = variants.reduce((sum, v) => sum + (v.split_percent || 0), 0);
          return totalSplit !== 100 && (
            <div className={`split-warning ${totalSplit > 100 ? 'error' : 'warning'}`}>
              Total split is {totalSplit}% - must equal 100%
            </div>
          );
        })()}

        {/* Primary metric info */}
        {testType && (
          <div className="metric-info">
            Primary metric for this test: <strong>{testType.metric.replace('_', ' ')}</strong>
          </div>
        )}
      </div>
    );
  };

  const renderStep3 = () => (
    <div className="ab-step-content">
      <div className="step-header">
        <h3>Step 3: Winner Selection</h3>
        <p>Configure how and when the winning variant will be determined</p>
      </div>

      <div className="form-section">
        <h4>Winner Metric</h4>
        <div className="metric-grid">
          {WINNER_METRICS.map(metric => (
            <label
              key={metric.value}
              className={`metric-card ${formData.winner_metric === metric.value ? 'selected' : ''}`}
            >
              <input
                type="radio"
                name="winner_metric"
                value={metric.value}
                checked={formData.winner_metric === metric.value}
                onChange={e => setFormData({ ...formData, winner_metric: e.target.value })}
              />
              <div className="metric-info">
                <strong>{metric.label}</strong>
                <small>{metric.description}</small>
              </div>
            </label>
          ))}
        </div>
      </div>

      <div className="form-section">
        <h4>Timing & Automation</h4>
        
        <div className="form-group">
          <label>Wait time before selecting winner</label>
          <select
            value={formData.winner_wait_hours}
            onChange={e => setFormData({ ...formData, winner_wait_hours: parseInt(e.target.value) })}
          >
            <option value={1}>1 hour</option>
            <option value={2}>2 hours</option>
            <option value={4}>4 hours</option>
            <option value={8}>8 hours</option>
            <option value={12}>12 hours</option>
            <option value={24}>24 hours</option>
            <option value={48}>48 hours</option>
          </select>
          <small>We'll collect results for this duration before determining the winner</small>
        </div>

        <div className="checkbox-group">
          <label className="checkbox-card">
            <input
              type="checkbox"
              checked={formData.winner_auto_select}
              onChange={e => setFormData({ ...formData, winner_auto_select: e.target.checked })}
            />
            <div className="checkbox-content">
              <strong>Auto-select winner</strong>
              <small>Automatically send the winning variant to the remaining audience</small>
            </div>
          </label>
        </div>

        <div className="form-group">
          <label>Confidence Threshold: {Math.round(formData.winner_confidence_threshold * 100)}%</label>
          <input
            type="range"
            min={80}
            max={99}
            value={formData.winner_confidence_threshold * 100}
            onChange={e => setFormData({ ...formData, winner_confidence_threshold: parseInt(e.target.value) / 100 })}
          />
          <div className="range-labels">
            <span>80% (faster)</span>
            <span>95% (standard)</span>
            <span>99% (stricter)</span>
          </div>
        </div>

        <div className="form-group">
          <label>Minimum Sample Size</label>
          <input
            type="number"
            min={50}
            value={formData.winner_min_sample_size}
            onChange={e => setFormData({ ...formData, winner_min_sample_size: parseInt(e.target.value) || 100 })}
          />
          <small>Minimum responses per variant before declaring significance</small>
        </div>
      </div>

      {/* Summary */}
      <div className="test-summary">
        <h4>Test Summary</h4>
        <div className="summary-grid">
          <div className="summary-item">
            <span className="summary-label">Test Type</span>
            <span className="summary-value">{TEST_TYPES.find(t => t.value === formData.test_type)?.label}</span>
          </div>
          <div className="summary-item">
            <span className="summary-label">Variants</span>
            <span className="summary-value">{variants.length}</span>
          </div>
          <div className="summary-item">
            <span className="summary-label">Test Sample</span>
            <span className="summary-value">{testSampleSize.toLocaleString()} contacts</span>
          </div>
          <div className="summary-item">
            <span className="summary-label">Winner Audience</span>
            <span className="summary-value">{remainingAudience.toLocaleString()} contacts</span>
          </div>
          <div className="summary-item">
            <span className="summary-label">Winner Metric</span>
            <span className="summary-value">{WINNER_METRICS.find(m => m.value === formData.winner_metric)?.label}</span>
          </div>
          <div className="summary-item">
            <span className="summary-label">Wait Time</span>
            <span className="summary-value">{formData.winner_wait_hours} hours</span>
          </div>
        </div>
      </div>
    </div>
  );

  // Validate each step
  const canProceed = () => {
    if (step === 1) {
      return formData.name && formData.test_type && formData.segment_id;
    }
    if (step === 2) {
      const totalSplit = variants.reduce((sum, v) => sum + (v.split_percent || 0), 0);
      return totalSplit === 100 && variants.length >= 2;
    }
    return true;
  };

  return (
    <div className="ab-test-builder-overlay" onClick={onClose}>
      <div className="ab-test-builder" onClick={e => e.stopPropagation()}>
        <div className="builder-header">
          <h2>Create A/B Test</h2>
          <div className="step-indicator">
            <div className={`step ${step >= 1 ? 'active' : ''} ${step > 1 ? 'completed' : ''}`}>
              <span className="step-number">1</span>
              <span className="step-label">Configure</span>
            </div>
            <div className="step-line" />
            <div className={`step ${step >= 2 ? 'active' : ''} ${step > 2 ? 'completed' : ''}`}>
              <span className="step-number">2</span>
              <span className="step-label">Variants</span>
            </div>
            <div className="step-line" />
            <div className={`step ${step >= 3 ? 'active' : ''}`}>
              <span className="step-number">3</span>
              <span className="step-label">Winner</span>
            </div>
          </div>
          <button className="close-btn" onClick={onClose}>×</button>
        </div>

        <div className="builder-body">
          {step === 1 && renderStep1()}
          {step === 2 && renderStep2()}
          {step === 3 && renderStep3()}
        </div>

        <div className="builder-footer">
          <button type="button" className="cancel-btn" onClick={onClose}>
            Cancel
          </button>
          
          <div className="nav-buttons">
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
                disabled={!canProceed()}
              >
                Next →
              </button>
            ) : (
              <button 
                type="button" 
                className="submit-btn"
                onClick={handleSubmit}
                disabled={loading || !canProceed()}
              >
                {loading ? 'Creating...' : 'Create A/B Test'}
              </button>
            )}
          </div>
        </div>
      </div>
    </div>
  );
};

// =============================================================================
// A/B TEST RESULTS DASHBOARD
// =============================================================================

interface ABTestResultsProps {
  testId: string;
  onClose: () => void;
}

export const ABTestResults: React.FC<ABTestResultsProps> = ({ testId, onClose }) => {
  const [test, setTest] = useState<ABTest | null>(null);
  const [loading, setLoading] = useState(true);
  const [selectingWinner, setSelectingWinner] = useState(false);
  const [showInsights, setShowInsights] = useState(false);

  // Load test data
  useEffect(() => {
    const fetchTest = async () => {
      try {
        const response = await fetch(`/api/mailing/ab-tests/${testId}`);
        const data = await response.json();
        setTest(data);
      } catch (error) {
        console.error('Error fetching test:', error);
      } finally {
        setLoading(false);
      }
    };

    fetchTest();
    // Refresh every 30 seconds for live updates
    const interval = setInterval(fetchTest, 30000);
    return () => clearInterval(interval);
  }, [testId]);

  // Actions
  const startTest = async () => {
    try {
      await fetch(`/api/mailing/ab-tests/${testId}/start`, { method: 'POST' });
      const response = await fetch(`/api/mailing/ab-tests/${testId}`);
      setTest(await response.json());
    } catch (error) {
      alert('Failed to start test');
    }
  };

  const selectWinner = async (variantId: string) => {
    setSelectingWinner(true);
    try {
      await fetch(`/api/mailing/ab-tests/${testId}/select-winner`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ variant_id: variantId }),
      });
      const response = await fetch(`/api/mailing/ab-tests/${testId}`);
      setTest(await response.json());
    } catch (error) {
      alert('Failed to select winner');
    } finally {
      setSelectingWinner(false);
    }
  };

  const sendWinner = async () => {
    try {
      await fetch(`/api/mailing/ab-tests/${testId}/send-winner`, { method: 'POST' });
      const response = await fetch(`/api/mailing/ab-tests/${testId}`);
      setTest(await response.json());
    } catch (error) {
      alert('Failed to send winner');
    }
  };

  if (loading) {
    return (
      <div className="ab-results-overlay">
        <div className="ab-results-loading">Loading test results...</div>
      </div>
    );
  }

  if (!test) {
    return (
      <div className="ab-results-overlay">
        <div className="ab-results-error">Test not found</div>
      </div>
    );
  }

  // Find leading variant
  const leadingVariant = test.variants.reduce((leader, v) => 
    v.open_rate > (leader?.open_rate || 0) ? v : leader
  , test.variants[0]);

  // Calculate if we have enough data
  const hasEnoughData = test.variants.every(v => v.sent_count >= 100);

  return (
    <div className="ab-results-overlay" onClick={onClose}>
      <div className="ab-results-modal" onClick={e => e.stopPropagation()}>
        <div className="results-header">
          <div className="header-info">
            <h2>{test.name}</h2>
            <span className={`status-badge status-${test.status}`}>{test.status}</span>
          </div>
          <button className="close-btn" onClick={onClose}>×</button>
        </div>

        <div className="results-overview">
          <div className="overview-item">
            <span className="overview-value">{test.total_audience_size.toLocaleString()}</span>
            <span className="overview-label">Total Audience</span>
          </div>
          <div className="overview-item">
            <span className="overview-value">{test.test_sample_size.toLocaleString()}</span>
            <span className="overview-label">Test Sample</span>
          </div>
          <div className="overview-item">
            <span className="overview-value">{test.variants.length}</span>
            <span className="overview-label">Variants</span>
          </div>
          <div className="overview-item">
            <span className="overview-value">{test.winner_wait_hours}h</span>
            <span className="overview-label">Wait Time</span>
          </div>
        </div>

        <div className="results-variants">
          {test.variants
            .sort((a, b) => b.open_rate - a.open_rate)
            .map((variant, index) => (
            <div 
              key={variant.id} 
              className={`result-variant-card ${variant.is_winner ? 'winner' : ''} ${variant.is_control ? 'control' : ''}`}
            >
              <div className="variant-rank">
                {index === 0 && <span className="leading-badge">Leading</span>}
                {variant.is_winner && <span className="winner-badge">Winner</span>}
                {variant.is_control && <span className="control-badge">Control</span>}
              </div>

              <div className="variant-identity">
                <span className="variant-letter">{variant.variant_name}</span>
                <span className="variant-label">{variant.variant_label}</span>
              </div>

              <div className="variant-preview">
                {variant.subject && (
                  <div className="preview-subject">
                    <span className="preview-label">Subject:</span>
                    {variant.subject}
                  </div>
                )}
                {variant.from_name && (
                  <div className="preview-from">
                    <span className="preview-label">From:</span>
                    {variant.from_name}
                  </div>
                )}
              </div>

              <div className="variant-metrics">
                <div className="metric">
                  <span className="metric-value">{variant.sent_count.toLocaleString()}</span>
                  <span className="metric-label">Sent</span>
                </div>
                <div className="metric highlight">
                  <span className="metric-value">{(variant.open_rate * 100).toFixed(2)}%</span>
                  <span className="metric-label">Open Rate</span>
                </div>
                <div className="metric">
                  <span className="metric-value">{(variant.click_rate * 100).toFixed(2)}%</span>
                  <span className="metric-label">Click Rate</span>
                </div>
                <div className="metric">
                  <span className="metric-value">{(variant.click_to_open_rate * 100).toFixed(2)}%</span>
                  <span className="metric-label">CTOR</span>
                </div>
              </div>

              {!variant.is_control && (
                <div className="variant-comparison">
                  <div className={`lift ${variant.lift_vs_control > 0 ? 'positive' : 'negative'}`}>
                    {variant.lift_vs_control > 0 ? '+' : ''}{variant.lift_vs_control.toFixed(1)}%
                    <span>vs Control</span>
                  </div>
                  <div className="confidence">
                    {(variant.confidence_score * 100).toFixed(0)}% confidence
                    {variant.statistical_significance && <span className="sig-badge">Significant</span>}
                  </div>
                </div>
              )}

              {test.status === 'waiting' && !test.winner_variant_id && (
                <button
                  className="select-winner-btn"
                  onClick={() => selectWinner(variant.id)}
                  disabled={selectingWinner}
                >
                  Select as Winner
                </button>
              )}
            </div>
          ))}
        </div>

        {/* Significance indicator */}
        {test.status === 'testing' || test.status === 'waiting' ? (
          <div className="significance-indicator">
            {hasEnoughData ? (
              leadingVariant?.statistical_significance ? (
                <div className="sig-status significant">
                  Statistical significance reached - ready to select winner
                </div>
              ) : (
                <div className="sig-status waiting">
                  Collecting more data - not yet statistically significant
                </div>
              )
            ) : (
              <div className="sig-status gathering">
                Gathering data - need at least 100 sends per variant
              </div>
            )}
          </div>
        ) : null}

        <div className="results-actions">
          {test.status === 'draft' && (
            <button className="action-btn primary" onClick={startTest}>
              Start Test
            </button>
          )}
          {test.status === 'winner_selected' && (
            <button className="action-btn primary" onClick={sendWinner}>
              Send Winner to Remaining {test.remaining_audience_size.toLocaleString()} Contacts
            </button>
          )}
          <button className="action-btn secondary" onClick={onClose}>
            Close
          </button>
        </div>
      </div>
      
      {/* AI Insights Button */}
      <ABTestInsightsButton 
        onClick={() => setShowInsights(true)} 
        insightCount={test.variants.length > 0 ? Math.min(5, test.variants.length + 2) : 0}
      />
      
      {/* AI Insights Panel */}
      <ABTestInsights
        test={test}
        isOpen={showInsights}
        onClose={() => setShowInsights(false)}
        onApplySuggestion={(prompt) => {
          console.log('Applied prompt:', prompt);
          // Could integrate with a chat interface or AI service
        }}
      />
    </div>
  );
};

// =============================================================================
// A/B TEST LIST COMPONENT
// =============================================================================

interface ABTestListProps {
  onCreateNew: () => void;
  onViewResults: (testId: string) => void;
}

export const ABTestList: React.FC<ABTestListProps> = ({ onCreateNew, onViewResults }) => {
  const [tests, setTests] = useState<ABTest[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    fetch('/api/mailing/ab-tests')
      .then(r => r.json())
      .then(data => setTests(data.tests || []))
      .catch(() => {})
      .finally(() => setLoading(false));
  }, []);

  const getStatusColor = (status: string) => {
    const colors: Record<string, string> = {
      draft: '#6b7280',
      scheduled: '#f59e0b',
      testing: '#3b82f6',
      waiting: '#8b5cf6',
      analyzing: '#ec4899',
      winner_selected: '#10b981',
      sending_winner: '#06b6d4',
      completed: '#22c55e',
      cancelled: '#ef4444',
      failed: '#dc2626',
    };
    return colors[status] || '#6b7280';
  };

  if (loading) {
    return <div className="ab-tests-loading">Loading A/B tests...</div>;
  }

  return (
    <div className="ab-tests-list">
      <div className="ab-tests-header">
        <h2>A/B Split Tests</h2>
        <button className="create-test-btn" onClick={onCreateNew}>
          + Create A/B Test
        </button>
      </div>

      {tests.length === 0 ? (
        <div className="ab-tests-empty">
          <div className="empty-icon">🧪</div>
          <h3>No A/B tests yet</h3>
          <p>Create your first A/B test to optimize your email campaigns</p>
          <button className="create-test-btn large" onClick={onCreateNew}>
            Create Your First A/B Test
          </button>
        </div>
      ) : (
        <div className="ab-tests-grid">
          {tests.map(test => (
            <div key={test.id} className="ab-test-card" onClick={() => onViewResults(test.id)}>
              <div className="test-card-header">
                <h3>{test.name}</h3>
                <span 
                  className="status-badge"
                  style={{ backgroundColor: getStatusColor(test.status) + '20', color: getStatusColor(test.status) }}
                >
                  {test.status.replace('_', ' ')}
                </span>
              </div>
              
              <div className="test-card-info">
                <div className="info-item">
                  <span className="info-label">Type</span>
                  <span className="info-value">{test.test_type.replace('_', ' ')}</span>
                </div>
                <div className="info-item">
                  <span className="info-label">Variants</span>
                  <span className="info-value">{test.variants?.length || 0}</span>
                </div>
                <div className="info-item">
                  <span className="info-label">Audience</span>
                  <span className="info-value">{test.total_audience_size.toLocaleString()}</span>
                </div>
              </div>

              {test.variants && test.variants.length > 0 && (
                <div className="test-card-variants">
                  {test.variants.slice(0, 3).map(v => (
                    <div key={v.id} className={`mini-variant ${v.is_winner ? 'winner' : ''}`}>
                      <span className="mini-letter">{v.variant_name}</span>
                      <span className="mini-rate">{(v.open_rate * 100).toFixed(1)}%</span>
                    </div>
                  ))}
                </div>
              )}

              <div className="test-card-footer">
                <span className="test-date">
                  Created {new Date(test.created_at).toLocaleDateString()}
                </span>
                <button className="view-btn">View Results →</button>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
};

export default ABTestBuilder;
