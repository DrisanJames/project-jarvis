import React, { useEffect, useState } from 'react';
import { useSendingPlans } from '../hooks/useMailingApi';
import type { SendingPlan } from '../types';
import './SendingPlans.css';

interface PlanCardProps {
  plan: SendingPlan;
  isSelected: boolean;
  onSelect: () => void;
}

const PlanCard: React.FC<PlanCardProps> = ({ plan, isSelected, onSelect }) => {
  const formatCurrency = (value: number) => `$${value.toFixed(2)}`;
  const formatPercent = (value: number) => `${value.toFixed(1)}%`;

  return (
    <div
      className={`plan-card ${isSelected ? 'selected' : ''}`}
      onClick={onSelect}
    >
      <div className="plan-header">
        <h3>{plan.name || 'Unnamed Plan'}</h3>
        <div className="confidence-badge">
          {formatPercent((plan.confidence_score || 0) * 100)} confidence
        </div>
      </div>

      <p className="plan-description">{plan.description}</p>

      <div className="plan-metrics">
        <div className="metric">
          <span className="metric-label">Volume</span>
          <span className="metric-value">{(plan.recommended_volume || 0).toLocaleString()}</span>
        </div>
        <div className="metric">
          <span className="metric-label">Est. Opens</span>
          <span className="metric-value">{(plan.predictions?.estimated_opens || 0).toLocaleString()}</span>
        </div>
        <div className="metric">
          <span className="metric-label">Est. Clicks</span>
          <span className="metric-value">{(plan.predictions?.estimated_clicks || 0).toLocaleString()}</span>
        </div>
        <div className="metric revenue">
          <span className="metric-label">Est. Revenue</span>
          <span className="metric-value">{formatCurrency(plan.predictions?.estimated_revenue || 0)}</span>
          {plan.predictions?.revenue_range && (
            <span className="metric-range">
              ({formatCurrency(plan.predictions.revenue_range[0] || 0)} - {formatCurrency(plan.predictions.revenue_range[1] || 0)})
            </span>
          )}
        </div>
      </div>

      {plan.warnings && plan.warnings.length > 0 && (
        <div className="plan-warnings">
          {plan.warnings.map((warning, i) => (
            <div key={i} className="warning">⚠️ {warning}</div>
          ))}
        </div>
      )}

      {plan.recommendations && plan.recommendations.length > 0 && (
        <div className="plan-recommendations">
          <h4>Recommendations</h4>
          <ul>
            {plan.recommendations.map((rec, i) => (
              <li key={i}>{rec}</li>
            ))}
          </ul>
        </div>
      )}

      {plan.ai_explanation && (
        <div className="ai-explanation">
          <h4>AI Analysis</h4>
          <p>{plan.ai_explanation}</p>
        </div>
      )}

      {isSelected && (
        <div className="plan-details">
          {plan.time_slots && plan.time_slots.length > 0 && (
            <>
              <h4>Time Slots</h4>
              <div className="time-slots">
                {plan.time_slots.map((slot, i) => (
                  <div key={i} className="time-slot">
                    <span className="slot-time">
                      {new Date(slot.start_time).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })} -
                      {new Date(slot.end_time).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}
                    </span>
                    <span className="slot-volume">{(slot.volume || 0).toLocaleString()} emails</span>
                    <span className={`slot-priority priority-${slot.priority || 'normal'}`}>{slot.priority || 'normal'}</span>
                  </div>
                ))}
              </div>
            </>
          )}

          {plan.audience_breakdown && plan.audience_breakdown.length > 0 && (
            <>
              <h4>Audience Breakdown</h4>
              <div className="audience-breakdown">
                {plan.audience_breakdown.map((segment, i) => (
                  <div key={i} className="audience-segment">
                    <div className="segment-header">
                      <span className="segment-name">{segment.name || 'Unknown'}</span>
                      <span className="segment-count">{(segment.count || 0).toLocaleString()}</span>
                    </div>
                    <div className="segment-metrics">
                      <span>Open Rate: {formatPercent(segment.predicted_open_rate || 0)}</span>
                      <span>Click Rate: {formatPercent(segment.predicted_click_rate || 0)}</span>
                    </div>
                    <div className="segment-action">{segment.recommended_action || 'No action'}</div>
                  </div>
                ))}
              </div>
            </>
          )}
        </div>
      )}
    </div>
  );
};

export const SendingPlans: React.FC = () => {
  const { plans, loading, error, fetchPlans } = useSendingPlans();
  const [selectedDate, setSelectedDate] = useState(
    new Date().toISOString().split('T')[0]
  );
  const [selectedPlan, setSelectedPlan] = useState<string | null>(null);

  useEffect(() => {
    fetchPlans(selectedDate);
  }, [selectedDate, fetchPlans]);

  const handleApprove = (plan: SendingPlan) => {
    // In production, this would call the API to approve the plan
    console.log('Approving plan:', plan.time_period);
    alert(`Plan "${plan.name}" approved for execution!`);
  };

  return (
    <div className="sending-plans">
      <header className="plans-header">
        <div>
          <h1>AI Sending Plans</h1>
          <p>Review AI-generated sending strategies for maximum performance</p>
        </div>
        <div className="date-selector">
          <label>Target Date:</label>
          <input
            type="date"
            value={selectedDate}
            onChange={(e) => setSelectedDate(e.target.value)}
            min={new Date().toISOString().split('T')[0]}
          />
          <button onClick={() => fetchPlans(selectedDate)} disabled={loading}>
            {loading ? 'Generating...' : 'Regenerate Plans'}
          </button>
        </div>
      </header>

      {error && (
        <div className="error-message">
          <p>{error}</p>
          <button onClick={() => fetchPlans(selectedDate)}>Retry</button>
        </div>
      )}

      {loading && plans.length === 0 && (
        <div className="loading-state">
          <div className="loading-spinner" />
          <p>Analyzing your data and generating optimal sending plans...</p>
        </div>
      )}

      <div className="plans-grid">
        {plans.map((plan) => (
          <PlanCard
            key={plan.time_period}
            plan={plan}
            isSelected={selectedPlan === plan.time_period}
            onSelect={() =>
              setSelectedPlan(
                selectedPlan === plan.time_period ? null : plan.time_period
              )
            }
          />
        ))}
      </div>

      {selectedPlan && (
        <div className="plan-actions">
          <button
            className="approve-btn"
            onClick={() => {
              const plan = plans.find((p) => p.time_period === selectedPlan);
              if (plan) handleApprove(plan);
            }}
          >
            Approve & Execute Plan
          </button>
          <button className="modify-btn" onClick={() => alert('Modification modal coming soon')}>
            Modify Plan
          </button>
        </div>
      )}
    </div>
  );
};

export default SendingPlans;
