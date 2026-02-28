import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { 
  faLightbulb, 
  faMagic, 
  faArrowUp, 
  faExclamationTriangle, 
  faCheckCircle, 
  faCopy, 
  faCheck,
  faPaperPlane,
  faSpinner,
  faChevronRight,
  faBolt,
  faBullseye,
  faChartBar,
  faTimes
} from '@fortawesome/free-solid-svg-icons';

// =============================================================================
// A/B TEST AI INSIGHTS - Enterprise Grade
// =============================================================================
// Features:
// - Real-time AI-generated recommendations
// - Context-aware suggestions based on test performance
// - One-click prompt generation for QA
// - Statistical insights and warnings
// - Actionable improvement suggestions
// =============================================================================

interface ABVariant {
  id: string;
  variant_name: string;
  variant_label: string;
  subject: string;
  from_name: string;
  sent_count: number;
  open_rate: number;
  click_rate: number;
  confidence_score: number;
  lift_vs_control: number;
  statistical_significance: boolean;
  is_control: boolean;
  is_winner: boolean;
}

interface ABTest {
  id: string;
  name: string;
  test_type: string;
  status: string;
  total_audience_size: number;
  test_sample_size: number;
  test_sample_percent?: number;
  winner_metric: string;
  winner_wait_hours: number;
  variants: ABVariant[];
}

interface Insight {
  id: string;
  type: 'suggestion' | 'warning' | 'success' | 'info';
  category: 'performance' | 'statistical' | 'content' | 'timing' | 'audience';
  title: string;
  description: string;
  action?: string;
  prompt?: string;
  priority: 'high' | 'medium' | 'low';
}

interface ABTestInsightsProps {
  test: ABTest | null;
  onApplySuggestion?: (suggestion: string) => void;
  isOpen: boolean;
  onClose: () => void;
}

// Pre-defined insight generators based on test data
const generateInsights = (test: ABTest): Insight[] => {
  const insights: Insight[] = [];
  
  if (!test || !test.variants || test.variants.length === 0) {
    return insights;
  }
  
  const variants = test.variants;
  const totalSent = variants.reduce((sum, v) => sum + v.sent_count, 0);
  const avgOpenRate = variants.reduce((sum, v) => sum + v.open_rate, 0) / variants.length;
  const maxOpenRate = Math.max(...variants.map(v => v.open_rate));
  const minOpenRate = Math.min(...variants.map(v => v.open_rate));
  const openRateSpread = maxOpenRate - minOpenRate;
  const leadingVariant = variants.reduce((leader, v) => 
    v.open_rate > (leader?.open_rate || 0) ? v : leader
  , variants[0]);
  const controlVariant = variants.find(v => v.is_control);
  
  // Performance insights
  if (totalSent < 100) {
    insights.push({
      id: 'low-sample',
      type: 'warning',
      category: 'statistical',
      title: 'Low sample size',
      description: `Only ${totalSent} emails sent so far. Need at least 100 per variant for reliable results.`,
      action: 'Wait for more data before making decisions',
      prompt: `The A/B test "${test.name}" has only sent ${totalSent} emails total. What are the risks of making decisions with such a small sample size, and what minimum sample size would you recommend for a ${test.test_type} test?`,
      priority: 'high',
    });
  }
  
  // Statistical significance insights
  const significantVariants = variants.filter(v => v.statistical_significance);
  if (significantVariants.length > 0 && test.status === 'waiting') {
    insights.push({
      id: 'significance-reached',
      type: 'success',
      category: 'statistical',
      title: 'Statistical significance reached!',
      description: `Variant ${significantVariants[0].variant_name} shows ${(significantVariants[0].confidence_score * 100).toFixed(0)}% confidence. Ready to declare a winner.`,
      action: 'Select winner and send to remaining audience',
      prompt: `The A/B test "${test.name}" has reached statistical significance with variant ${significantVariants[0].variant_name} showing ${(significantVariants[0].lift_vs_control * 100).toFixed(1)}% lift. What should I verify before sending this to the remaining ${test.total_audience_size - test.test_sample_size} subscribers?`,
      priority: 'high',
    });
  }
  
  // No clear winner yet
  if (test.status === 'waiting' && significantVariants.length === 0 && totalSent >= 100) {
    insights.push({
      id: 'no-winner',
      type: 'info',
      category: 'statistical',
      title: 'No clear winner yet',
      description: `Results are too close to call. ${openRateSpread < 0.02 ? 'Very similar performance across variants.' : 'Need more data to determine significance.'}`,
      action: openRateSpread < 0.01 ? 'Consider if the differences matter for your goals' : 'Continue collecting data',
      prompt: `The A/B test "${test.name}" shows no statistically significant winner after ${totalSent} sends. The open rate spread is ${(openRateSpread * 100).toFixed(2)}%. Should I continue the test, increase the sample size, or conclude that there's no meaningful difference?`,
      priority: 'medium',
    });
  }
  
  // Content-specific insights
  if (test.test_type === 'subject_line') {
    const leadingSubject = leadingVariant?.subject || '';
    
    if (leadingSubject.length < 40 && leadingVariant?.open_rate > avgOpenRate) {
      insights.push({
        id: 'short-subject-winning',
        type: 'suggestion',
        category: 'content',
        title: 'Shorter subject lines performing better',
        description: `Variant ${leadingVariant?.variant_name} with ${leadingSubject.length} characters is leading. Consider testing even shorter subjects.`,
        prompt: `The A/B test shows that the shorter subject line "${leadingSubject}" (${leadingSubject.length} chars) is outperforming longer ones. Generate 3 alternative short subject lines (under 40 chars) that maintain the same message but are even more concise.`,
        priority: 'medium',
      });
    }
    
    if (leadingSubject.includes('?') && leadingVariant?.open_rate > avgOpenRate) {
      insights.push({
        id: 'question-winning',
        type: 'suggestion',
        category: 'content',
        title: 'Questions drive engagement',
        description: 'Subject lines with questions are performing well. This creates curiosity.',
        prompt: `The question-based subject line "${leadingSubject}" is performing well in this test. Generate 3 more question-based subject line variations that could drive even higher open rates.`,
        priority: 'low',
      });
    }
  }
  
  // Timing insights
  if (test.test_type === 'send_time') {
    insights.push({
      id: 'timing-analysis',
      type: 'info',
      category: 'timing',
      title: 'Send time optimization active',
      description: 'Analyzing optimal send times based on subscriber engagement patterns.',
      prompt: `Based on the send time A/B test "${test.name}", what factors should I consider when choosing the optimal send time for this audience? Consider time zones, industry benchmarks, and day-of-week patterns.`,
      priority: 'medium',
    });
  }
  
  // Audience insights
  const testSamplePercent = test.test_sample_percent || (test.test_sample_size / test.total_audience_size * 100);
  if (test.total_audience_size > 10000 && testSamplePercent < 15) {
    insights.push({
      id: 'large-audience',
      type: 'suggestion',
      category: 'audience',
      title: 'Large audience opportunity',
      description: `With ${test.total_audience_size.toLocaleString()} total subscribers, even small improvements compound to significant gains.`,
      prompt: `The A/B test "${test.name}" is targeting ${test.total_audience_size.toLocaleString()} subscribers. If the winning variant shows ${(leadingVariant?.lift_vs_control || 0.1 * 100).toFixed(1)}% improvement, calculate the estimated impact on overall engagement metrics.`,
      priority: 'low',
    });
  }
  
  // Variant performance insights
  if (controlVariant && leadingVariant && leadingVariant.id !== controlVariant.id) {
    const lift = ((leadingVariant.open_rate - controlVariant.open_rate) / controlVariant.open_rate) * 100;
    if (lift > 20) {
      insights.push({
        id: 'high-lift',
        type: 'success',
        category: 'performance',
        title: 'Significant improvement found!',
        description: `Variant ${leadingVariant.variant_name} shows ${lift.toFixed(1)}% lift over control. This could significantly impact your results.`,
        prompt: `Variant ${leadingVariant.variant_name} in test "${test.name}" shows ${lift.toFixed(1)}% lift. Analyze what specific elements might be driving this improvement and suggest how to apply these learnings to future campaigns.`,
        priority: 'high',
      });
    }
  }
  
  // Sort by priority
  const priorityOrder = { high: 0, medium: 1, low: 2 };
  return insights.sort((a, b) => priorityOrder[a.priority] - priorityOrder[b.priority]);
};

// Quick prompts for QA testing
const QA_PROMPTS = [
  {
    id: 'validate-setup',
    icon: faCheckCircle,
    label: 'Validate Test Setup',
    prompt: (test: ABTest) => `Review the A/B test setup for "${test.name}":
- Test type: ${test.test_type}
- Variants: ${test.variants.length}
- Sample size: ${test.test_sample_size} (${((test.test_sample_size / test.total_audience_size) * 100).toFixed(0)}% of audience)
- Winner metric: ${test.winner_metric}

Is this configuration optimal? What potential issues should I check?`,
  },
  {
    id: 'analyze-results',
    icon: faChartBar,
    label: 'Analyze Current Results',
    prompt: (test: ABTest) => {
      const variants = test.variants;
      const resultsSummary = variants.map(v => 
        `- ${v.variant_name}: ${v.sent_count} sent, ${(v.open_rate * 100).toFixed(2)}% open rate, ${(v.click_rate * 100).toFixed(2)}% click rate`
      ).join('\n');
      return `Analyze these A/B test results for "${test.name}":

${resultsSummary}

What insights can you draw? Are there any concerning patterns?`;
    },
  },
  {
    id: 'predict-winner',
    icon: faArrowUp,
    label: 'Predict Winner',
    prompt: (test: ABTest) => {
      const leader = test.variants.reduce((l, v) => v.open_rate > (l?.open_rate || 0) ? v : l, test.variants[0]);
      return `Based on current data for "${test.name}":

Leading variant: ${leader.variant_name} (${(leader.open_rate * 100).toFixed(2)}% open rate)
Confidence: ${(leader.confidence_score * 100).toFixed(0)}%
Statistical significance: ${leader.statistical_significance ? 'Yes' : 'No'}

What's the likelihood this variant will remain the winner? What could change the outcome?`;
    },
  },
  {
    id: 'suggest-next',
    icon: faBolt,
    label: 'Suggest Next Test',
    prompt: (test: ABTest) => `Based on the ${test.test_type} test "${test.name}", what should I test next? Consider:
- What we learned from this test
- Areas that weren't tested
- Industry best practices
- Potential quick wins`,
  },
  {
    id: 'calculate-impact',
    icon: faBullseye,
    label: 'Calculate Impact',
    prompt: (test: ABTest) => {
      const winner = test.variants.find(v => v.is_winner) || test.variants.reduce((l, v) => v.open_rate > (l?.open_rate || 0) ? v : l, test.variants[0]);
      const control = test.variants.find(v => v.is_control) || test.variants[0];
      return `Calculate the projected impact if we roll out the winner:

Winner: ${winner.variant_name} (${(winner.open_rate * 100).toFixed(2)}% open rate)
Control: ${control.variant_name} (${(control.open_rate * 100).toFixed(2)}% open rate)
Remaining audience: ${test.total_audience_size - test.test_sample_size}

What's the expected improvement in opens, clicks, and downstream conversions?`;
    },
  },
];

export const ABTestInsights: React.FC<ABTestInsightsProps> = ({
  test,
  onApplySuggestion,
  isOpen,
  onClose,
}) => {
  const [insights, setInsights] = useState<Insight[]>([]);
  const [copiedId, setCopiedId] = useState<string | null>(null);
  const [customPrompt, setCustomPrompt] = useState('');
  const [isGenerating, setIsGenerating] = useState(false);
  const [activeTab, setActiveTab] = useState<'insights' | 'prompts'>('insights');

  // Generate insights when test data changes
  useEffect(() => {
    if (test) {
      const newInsights = generateInsights(test);
      setInsights(newInsights);
    }
  }, [test]);

  const copyToClipboard = useCallback(async (text: string, id: string) => {
    try {
      await navigator.clipboard.writeText(text);
      setCopiedId(id);
      setTimeout(() => setCopiedId(null), 2000);
    } catch (err) {
      console.error('Failed to copy:', err);
    }
  }, []);

  const handlePromptSubmit = useCallback(async () => {
    if (!customPrompt.trim()) return;
    
    setIsGenerating(true);
    try {
      // Copy the prompt to clipboard for now
      await navigator.clipboard.writeText(customPrompt);
      onApplySuggestion?.(customPrompt);
      setCustomPrompt('');
    } finally {
      setIsGenerating(false);
    }
  }, [customPrompt, onApplySuggestion]);

  const getInsightIcon = (type: Insight['type']) => {
    switch (type) {
      case 'suggestion':
        return <FontAwesomeIcon icon={faLightbulb} />;
      case 'warning':
        return <FontAwesomeIcon icon={faExclamationTriangle} />;
      case 'success':
        return <FontAwesomeIcon icon={faCheckCircle} />;
      case 'info':
        return <FontAwesomeIcon icon={faMagic} />;
    }
  };

  const getInsightColor = (type: Insight['type']) => {
    switch (type) {
      case 'suggestion':
        return '#facc15';
      case 'warning':
        return '#f97316';
      case 'success':
        return '#22c55e';
      case 'info':
        return '#3b82f6';
    }
  };

  if (!isOpen) return null;

  return (
    <div className="ab-insights-panel">
      <div className="insights-header">
        <div className="header-title">
          <FontAwesomeIcon icon={faMagic} />
          <span>AI Insights</span>
        </div>
        <button className="close-btn" onClick={onClose}>
          <FontAwesomeIcon icon={faTimes} />
        </button>
      </div>

      <div className="insights-tabs">
        <button 
          className={`tab ${activeTab === 'insights' ? 'active' : ''}`}
          onClick={() => setActiveTab('insights')}
        >
          <FontAwesomeIcon icon={faLightbulb} />
          Insights ({insights.length})
        </button>
        <button 
          className={`tab ${activeTab === 'prompts' ? 'active' : ''}`}
          onClick={() => setActiveTab('prompts')}
        >
          <FontAwesomeIcon icon={faBolt} />
          Quick Prompts
        </button>
      </div>

      <div className="insights-content">
        {activeTab === 'insights' && (
          <>
            {insights.length === 0 ? (
              <div className="empty-insights">
                <FontAwesomeIcon icon={faMagic} style={{ fontSize: '32px' }} />
                <p>No insights yet</p>
                <small>Insights will appear as your test collects data</small>
              </div>
            ) : (
              <div className="insights-list">
                {insights.map(insight => (
                  <div 
                    key={insight.id} 
                    className={`insight-card priority-${insight.priority}`}
                    style={{ borderLeftColor: getInsightColor(insight.type) }}
                  >
                    <div className="insight-header">
                      <span className="insight-icon" style={{ color: getInsightColor(insight.type) }}>
                        {getInsightIcon(insight.type)}
                      </span>
                      <span className="insight-title">{insight.title}</span>
                      <span className="insight-category">{insight.category}</span>
                    </div>
                    <p className="insight-description">{insight.description}</p>
                    {insight.action && (
                      <div className="insight-action">
                        <FontAwesomeIcon icon={faChevronRight} />
                        <span>{insight.action}</span>
                      </div>
                    )}
                    {insight.prompt && (
                      <button 
                        className="copy-prompt-btn"
                        onClick={() => copyToClipboard(insight.prompt!, insight.id)}
                      >
                        {copiedId === insight.id ? (
                          <>
                            <FontAwesomeIcon icon={faCheck} />
                            Copied!
                          </>
                        ) : (
                          <>
                            <FontAwesomeIcon icon={faCopy} />
                            Copy Prompt
                          </>
                        )}
                      </button>
                    )}
                  </div>
                ))}
              </div>
            )}
          </>
        )}

        {activeTab === 'prompts' && test && (
          <div className="prompts-section">
            <div className="quick-prompts">
              {QA_PROMPTS.map(qp => (
                <button
                  key={qp.id}
                  className="quick-prompt-btn"
                  onClick={() => copyToClipboard(qp.prompt(test), qp.id)}
                >
                  <FontAwesomeIcon icon={qp.icon} />
                  <span>{qp.label}</span>
                  {copiedId === qp.id ? (
                    <FontAwesomeIcon icon={faCheck} className="copied-icon" />
                  ) : (
                    <FontAwesomeIcon icon={faCopy} className="copy-icon" />
                  )}
                </button>
              ))}
            </div>

            <div className="custom-prompt-section">
              <label>Custom Prompt</label>
              <textarea
                value={customPrompt}
                onChange={(e) => setCustomPrompt(e.target.value)}
                placeholder="Type a custom question about this A/B test..."
                rows={4}
              />
              <button 
                className="generate-btn"
                onClick={handlePromptSubmit}
                disabled={!customPrompt.trim() || isGenerating}
              >
                {isGenerating ? (
                  <>
                    <FontAwesomeIcon icon={faSpinner} spin className="spinner" />
                    Generating...
                  </>
                ) : (
                  <>
                    <FontAwesomeIcon icon={faPaperPlane} />
                    Copy & Apply Prompt
                  </>
                )}
              </button>
            </div>

            <div className="context-info">
              <h4>Test Context</h4>
              <div className="context-item">
                <span className="label">Name:</span>
                <span className="value">{test.name}</span>
              </div>
              <div className="context-item">
                <span className="label">Type:</span>
                <span className="value">{test.test_type.replace('_', ' ')}</span>
              </div>
              <div className="context-item">
                <span className="label">Status:</span>
                <span className="value">{test.status}</span>
              </div>
              <div className="context-item">
                <span className="label">Variants:</span>
                <span className="value">{test.variants.length}</span>
              </div>
              <div className="context-item">
                <span className="label">Sample Size:</span>
                <span className="value">{test.test_sample_size.toLocaleString()}</span>
              </div>
            </div>
          </div>
        )}
      </div>

      <style>{`
        .ab-insights-panel {
          position: fixed;
          right: 0;
          top: 0;
          bottom: 0;
          width: 380px;
          background: #1a1a2e;
          border-left: 1px solid #333;
          display: flex;
          flex-direction: column;
          z-index: 1001;
          box-shadow: -4px 0 20px rgba(0, 0, 0, 0.3);
        }
        
        .insights-header {
          display: flex;
          align-items: center;
          justify-content: space-between;
          padding: 16px 20px;
          border-bottom: 1px solid #333;
          background: linear-gradient(135deg, #1e1e2e 0%, #2a2a3e 100%);
        }
        
        .header-title {
          display: flex;
          align-items: center;
          gap: 10px;
          font-weight: 600;
          font-size: 1rem;
          color: #fff;
        }
        
        .header-title svg {
          color: #facc15;
        }
        
        .close-btn {
          background: none;
          border: none;
          color: #888;
          cursor: pointer;
          padding: 4px;
          display: flex;
          align-items: center;
          justify-content: center;
          border-radius: 4px;
        }
        
        .close-btn:hover {
          background: #333;
          color: #fff;
        }
        
        .insights-tabs {
          display: flex;
          border-bottom: 1px solid #333;
        }
        
        .insights-tabs .tab {
          flex: 1;
          display: flex;
          align-items: center;
          justify-content: center;
          gap: 6px;
          padding: 12px;
          background: none;
          border: none;
          color: #888;
          font-size: 0.8125rem;
          cursor: pointer;
          transition: all 0.2s;
        }
        
        .insights-tabs .tab:hover {
          color: #fff;
          background: rgba(255, 255, 255, 0.05);
        }
        
        .insights-tabs .tab.active {
          color: #3b82f6;
          background: rgba(59, 130, 246, 0.1);
          border-bottom: 2px solid #3b82f6;
        }
        
        .insights-content {
          flex: 1;
          overflow-y: auto;
          padding: 16px;
        }
        
        .empty-insights {
          display: flex;
          flex-direction: column;
          align-items: center;
          justify-content: center;
          padding: 40px 20px;
          color: #666;
          text-align: center;
        }
        
        .empty-insights svg {
          margin-bottom: 12px;
          opacity: 0.5;
        }
        
        .empty-insights p {
          margin: 0 0 4px;
          color: #888;
        }
        
        .empty-insights small {
          font-size: 0.75rem;
        }
        
        .insights-list {
          display: flex;
          flex-direction: column;
          gap: 12px;
        }
        
        .insight-card {
          background: #1e1e2e;
          border: 1px solid #333;
          border-left: 3px solid;
          border-radius: 8px;
          padding: 14px;
        }
        
        .insight-card.priority-high {
          background: rgba(250, 204, 21, 0.05);
        }
        
        .insight-header {
          display: flex;
          align-items: center;
          gap: 8px;
          margin-bottom: 8px;
        }
        
        .insight-icon {
          display: flex;
          align-items: center;
        }
        
        .insight-title {
          font-weight: 600;
          font-size: 0.875rem;
          color: #fff;
          flex: 1;
        }
        
        .insight-category {
          font-size: 0.6875rem;
          color: #666;
          text-transform: uppercase;
          background: #2a2a3e;
          padding: 2px 6px;
          border-radius: 4px;
        }
        
        .insight-description {
          margin: 0 0 10px;
          font-size: 0.8125rem;
          color: #aaa;
          line-height: 1.5;
        }
        
        .insight-action {
          display: flex;
          align-items: center;
          gap: 6px;
          font-size: 0.75rem;
          color: #3b82f6;
          margin-bottom: 10px;
        }
        
        .copy-prompt-btn {
          display: flex;
          align-items: center;
          gap: 6px;
          padding: 6px 10px;
          background: #2a2a3e;
          border: 1px solid #333;
          border-radius: 6px;
          color: #888;
          font-size: 0.75rem;
          cursor: pointer;
          transition: all 0.2s;
        }
        
        .copy-prompt-btn:hover {
          background: #333;
          color: #fff;
        }
        
        .prompts-section {
          display: flex;
          flex-direction: column;
          gap: 20px;
        }
        
        .quick-prompts {
          display: flex;
          flex-direction: column;
          gap: 8px;
        }
        
        .quick-prompt-btn {
          display: flex;
          align-items: center;
          gap: 10px;
          padding: 12px;
          background: #1e1e2e;
          border: 1px solid #333;
          border-radius: 8px;
          color: #fff;
          font-size: 0.875rem;
          cursor: pointer;
          transition: all 0.2s;
          text-align: left;
        }
        
        .quick-prompt-btn:hover {
          background: #2a2a3e;
          border-color: #3b82f6;
        }
        
        .quick-prompt-btn span {
          flex: 1;
        }
        
        .quick-prompt-btn .copy-icon {
          color: #666;
        }
        
        .quick-prompt-btn .copied-icon {
          color: #22c55e;
        }
        
        .custom-prompt-section {
          background: #1e1e2e;
          border: 1px solid #333;
          border-radius: 8px;
          padding: 14px;
        }
        
        .custom-prompt-section label {
          display: block;
          font-size: 0.75rem;
          font-weight: 500;
          color: #888;
          margin-bottom: 8px;
          text-transform: uppercase;
        }
        
        .custom-prompt-section textarea {
          width: 100%;
          padding: 10px;
          background: #2a2a3e;
          border: 1px solid #333;
          border-radius: 6px;
          color: #fff;
          font-size: 0.875rem;
          resize: vertical;
          font-family: inherit;
          margin-bottom: 10px;
        }
        
        .custom-prompt-section textarea:focus {
          outline: none;
          border-color: #3b82f6;
        }
        
        .generate-btn {
          display: flex;
          align-items: center;
          justify-content: center;
          gap: 8px;
          width: 100%;
          padding: 10px;
          background: linear-gradient(135deg, #3b82f6 0%, #2563eb 100%);
          border: none;
          border-radius: 6px;
          color: #fff;
          font-size: 0.875rem;
          font-weight: 500;
          cursor: pointer;
          transition: all 0.2s;
        }
        
        .generate-btn:hover:not(:disabled) {
          background: linear-gradient(135deg, #2563eb 0%, #1d4ed8 100%);
        }
        
        .generate-btn:disabled {
          opacity: 0.5;
          cursor: not-allowed;
        }
        
        .generate-btn .spinner {
          animation: spin 1s linear infinite;
        }
        
        @keyframes spin {
          from { transform: rotate(0deg); }
          to { transform: rotate(360deg); }
        }
        
        .context-info {
          background: #1e1e2e;
          border: 1px solid #333;
          border-radius: 8px;
          padding: 14px;
        }
        
        .context-info h4 {
          margin: 0 0 12px;
          font-size: 0.75rem;
          font-weight: 500;
          color: #888;
          text-transform: uppercase;
        }
        
        .context-item {
          display: flex;
          justify-content: space-between;
          padding: 6px 0;
          border-bottom: 1px solid #2a2a3e;
        }
        
        .context-item:last-child {
          border-bottom: none;
        }
        
        .context-item .label {
          font-size: 0.8125rem;
          color: #666;
        }
        
        .context-item .value {
          font-size: 0.8125rem;
          color: #fff;
          font-weight: 500;
          text-transform: capitalize;
        }
      `}</style>
    </div>
  );
};

// =============================================================================
// FLOATING INSIGHTS BUTTON
// =============================================================================

interface InsightsButtonProps {
  onClick: () => void;
  insightCount?: number;
}

export const ABTestInsightsButton: React.FC<InsightsButtonProps> = ({ onClick, insightCount }) => {
  return (
    <button className="ab-insights-btn" onClick={onClick}>
      <FontAwesomeIcon icon={faLightbulb} />
      {insightCount && insightCount > 0 && (
        <span className="insight-badge">{insightCount}</span>
      )}
      <style>{`
        .ab-insights-btn {
          position: fixed;
          right: 20px;
          top: 50%;
          transform: translateY(-50%);
          width: 52px;
          height: 52px;
          border-radius: 50%;
          background: linear-gradient(135deg, #facc15 0%, #eab308 100%);
          color: #000;
          border: none;
          cursor: pointer;
          display: flex;
          align-items: center;
          justify-content: center;
          box-shadow: 0 4px 15px rgba(250, 204, 21, 0.4);
          z-index: 999;
          transition: all 0.2s;
        }
        
        .ab-insights-btn:hover {
          transform: translateY(-50%) scale(1.1);
          box-shadow: 0 6px 20px rgba(250, 204, 21, 0.5);
        }
        
        .insight-badge {
          position: absolute;
          top: -4px;
          right: -4px;
          min-width: 20px;
          height: 20px;
          background: #ef4444;
          color: #fff;
          border-radius: 10px;
          font-size: 0.6875rem;
          font-weight: 600;
          display: flex;
          align-items: center;
          justify-content: center;
          padding: 0 4px;
        }
      `}</style>
    </button>
  );
};

export default ABTestInsights;
