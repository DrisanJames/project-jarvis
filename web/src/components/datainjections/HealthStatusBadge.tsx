import React from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faCheckCircle, faExclamationTriangle, faStopCircle, faQuestionCircle } from '@fortawesome/free-solid-svg-icons';
import type { HealthStatus } from './types';

interface HealthStatusBadgeProps {
  status: HealthStatus;
  label?: string;
  showIcon?: boolean;
  size?: 'small' | 'medium' | 'large';
}

const statusConfig: Record<HealthStatus, { color: string; bgColor: string; icon: React.ReactNode; label: string }> = {
  healthy: {
    color: 'var(--accent-green)',
    bgColor: 'rgba(52, 211, 153, 0.1)',
    icon: <FontAwesomeIcon icon={faCheckCircle} />,
    label: 'Healthy',
  },
  warning: {
    color: 'var(--accent-yellow)',
    bgColor: 'rgba(251, 191, 36, 0.1)',
    icon: <FontAwesomeIcon icon={faExclamationTriangle} />,
    label: 'Warning',
  },
  critical: {
    color: 'var(--accent-red)',
    bgColor: 'rgba(239, 68, 68, 0.1)',
    icon: <FontAwesomeIcon icon={faStopCircle} />,
    label: 'Critical',
  },
  unknown: {
    color: 'var(--text-muted)',
    bgColor: 'rgba(148, 163, 184, 0.1)',
    icon: <FontAwesomeIcon icon={faQuestionCircle} />,
    label: 'Unknown',
  },
};

export const HealthStatusBadge: React.FC<HealthStatusBadgeProps> = ({
  status,
  label,
  showIcon = true,
  size = 'medium',
}) => {
  const config = statusConfig[status] || statusConfig.unknown;
  const displayLabel = label || config.label;

  const sizeStyles: Record<string, React.CSSProperties> = {
    small: { padding: '2px 8px', fontSize: '0.7rem' },
    medium: { padding: '4px 12px', fontSize: '0.8rem' },
    large: { padding: '6px 16px', fontSize: '0.9rem' },
  };

  return (
    <span
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        gap: '6px',
        color: config.color,
        backgroundColor: config.bgColor,
        borderRadius: '4px',
        fontWeight: 500,
        ...sizeStyles[size],
      }}
    >
      {showIcon && config.icon}
      {displayLabel}
    </span>
  );
};

export default HealthStatusBadge;
