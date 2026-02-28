import React from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faArrowUp, faArrowDown, faMinus } from '@fortawesome/free-solid-svg-icons';

interface MetricCardProps {
  label: string;
  value: string | number;
  subtitle?: string;
  change?: number;
  changeLabel?: string;
  status?: 'healthy' | 'warning' | 'critical';
  format?: 'number' | 'percentage' | 'currency';
}

export const MetricCard: React.FC<MetricCardProps> = ({
  label,
  value,
  subtitle,
  change,
  changeLabel,
  status,
  format = 'number',
}) => {
  const formatValue = (val: string | number): string => {
    if (typeof val === 'string') return val;
    
    switch (format) {
      case 'percentage':
        return `${(val * 100).toFixed(2)}%`;
      case 'number':
        if (val >= 1000000) {
          return `${(val / 1000000).toFixed(1)}M`;
        }
        if (val >= 1000) {
          return `${(val / 1000).toFixed(1)}K`;
        }
        return val.toLocaleString();
      default:
        return String(val);
    }
  };

  const getChangeClass = (): string => {
    if (!change) return 'neutral';
    return change > 0 ? 'positive' : 'negative';
  };

  const getStatusColor = (): string => {
    switch (status) {
      case 'healthy':
        return 'var(--accent-green)';
      case 'warning':
        return 'var(--accent-yellow)';
      case 'critical':
        return 'var(--accent-red)';
      default:
        return 'var(--text-primary)';
    }
  };

  return (
    <div className="card metric-card">
      <div 
        className="metric-value"
        style={{ color: status ? getStatusColor() : undefined }}
      >
        {formatValue(value)}
      </div>
      <div className="metric-label">{label}</div>
      {subtitle && (
        <div className="metric-subtitle" style={{ fontSize: '0.7rem', color: 'var(--text-muted)', marginTop: '0.25rem' }}>
          {subtitle}
        </div>
      )}
      {change !== undefined && (
        <div className={`metric-change ${getChangeClass()}`}>
          {change > 0 ? (
            <FontAwesomeIcon icon={faArrowUp} />
          ) : change < 0 ? (
            <FontAwesomeIcon icon={faArrowDown} />
          ) : (
            <FontAwesomeIcon icon={faMinus} />
          )}
          <span>
            {change > 0 ? '+' : ''}
            {(change * 100).toFixed(1)}%
          </span>
          {changeLabel && <span> {changeLabel}</span>}
        </div>
      )}
    </div>
  );
};

export default MetricCard;
