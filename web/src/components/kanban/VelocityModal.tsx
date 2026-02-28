import React, { useState, useEffect } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faTimes, faChartLine, faClock, faBolt, faUser } from '@fortawesome/free-solid-svg-icons';
import type { VelocityReport } from './types';

interface VelocityModalProps {
  onClose: () => void;
}

export const VelocityModal: React.FC<VelocityModalProps> = ({ onClose }) => {
  const [report, setReport] = useState<VelocityReport | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const fetchReport = async () => {
      try {
        const response = await fetch('/api/kanban/reports/current');
        if (!response.ok) throw new Error('Failed to fetch report');
        const data = await response.json();
        setReport(data);
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to load report');
      } finally {
        setLoading(false);
      }
    };

    fetchReport();
  }, []);

  const formatHours = (hours: number): string => {
    if (hours < 1) return `${Math.round(hours * 60)}m`;
    if (hours < 24) return `${hours.toFixed(1)}h`;
    return `${(hours / 24).toFixed(1)}d`;
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal-content kanban-modal velocity-modal" onClick={e => e.stopPropagation()}>
        {/* Header */}
        <div className="modal-header">
          <h3><FontAwesomeIcon icon={faChartLine} /> Velocity Report</h3>
          <span className="modal-subtitle">Current Month Performance</span>
          <button className="modal-close" onClick={onClose}>
            <FontAwesomeIcon icon={faTimes} />
          </button>
        </div>

        {/* Body */}
        <div className="modal-body">
          {loading ? (
            <div className="velocity-loading">Loading velocity data...</div>
          ) : error ? (
            <div className="velocity-error">{error}</div>
          ) : !report || report.total_completed === 0 ? (
            <div className="velocity-empty">
              <FontAwesomeIcon icon={faClock} style={{ fontSize: '48px' }} />
              <p>No completed tasks this month yet.</p>
              <p className="velocity-hint">Complete some tasks to see velocity metrics!</p>
            </div>
          ) : (
            <>
              {/* Summary Stats */}
              <div className="velocity-summary">
                <div className="velocity-stat">
                  <span className="velocity-stat-value">{report.total_completed}</span>
                  <span className="velocity-stat-label">Tasks Completed</span>
                </div>
                <div className="velocity-stat">
                  <span className="velocity-stat-value">{formatHours(report.avg_completion_hours)}</span>
                  <span className="velocity-stat-label">Avg Completion</span>
                </div>
                <div className="velocity-stat">
                  <span className="velocity-stat-value">{report.ai_generated_percent.toFixed(0)}%</span>
                  <span className="velocity-stat-label">AI Generated</span>
                </div>
              </div>

              {/* Breakdown by Source */}
              <div className="velocity-section">
                <h4>By Source</h4>
                <div className="velocity-breakdown">
                  <div className="velocity-item">
                    <FontAwesomeIcon icon={faBolt} />
                    <span>AI Generated</span>
                    <span className="velocity-count">{report.total_ai_generated}</span>
                  </div>
                  <div className="velocity-item">
                    <FontAwesomeIcon icon={faUser} />
                    <span>Human Created</span>
                    <span className="velocity-count">{report.total_human_created}</span>
                  </div>
                </div>
              </div>

              {/* Breakdown by Priority */}
              {report.by_priority && Object.keys(report.by_priority).length > 0 && (
                <div className="velocity-section">
                  <h4>By Priority</h4>
                  <div className="velocity-table">
                    <div className="velocity-table-header">
                      <span>Priority</span>
                      <span>Count</span>
                      <span>Avg Time</span>
                    </div>
                    {Object.entries(report.by_priority).map(([priority, stats]) => (
                      <div key={priority} className="velocity-table-row">
                        <span className={`priority-badge ${priority}`}>
                          {priority.charAt(0).toUpperCase() + priority.slice(1)}
                        </span>
                        <span>{stats.count}</span>
                        <span>{formatHours(stats.avg_completion_hours)}</span>
                      </div>
                    ))}
                  </div>
                </div>
              )}

              {/* Insights */}
              <div className="velocity-insights">
                {report.fastest_category && (
                  <div className="velocity-insight">
                    <span className="insight-icon">🚀</span>
                    <span>Fastest: <strong>{report.fastest_category}</strong> priority tasks</span>
                  </div>
                )}
                {report.slowest_category && (
                  <div className="velocity-insight">
                    <span className="insight-icon">🐢</span>
                    <span>Slowest: <strong>{report.slowest_category}</strong> priority tasks</span>
                  </div>
                )}
              </div>
            </>
          )}
        </div>

        {/* Footer */}
        <div className="modal-footer">
          <button className="btn-secondary" onClick={onClose}>
            Close
          </button>
        </div>
      </div>
    </div>
  );
};
