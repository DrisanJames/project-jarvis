import React from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faCheckCircle, faExclamationTriangle, faTimesCircle, faCircle } from '@fortawesome/free-solid-svg-icons';

type Status = 'healthy' | 'warning' | 'critical' | 'neutral';

interface StatusBadgeProps {
  status: Status;
  label?: string;
  showIcon?: boolean;
}

export const StatusBadge: React.FC<StatusBadgeProps> = ({
  status,
  label,
  showIcon = true,
}) => {
  const getIcon = () => {
    switch (status) {
      case 'healthy':
        return <FontAwesomeIcon icon={faCheckCircle} />;
      case 'warning':
        return <FontAwesomeIcon icon={faExclamationTriangle} />;
      case 'critical':
        return <FontAwesomeIcon icon={faTimesCircle} />;
      case 'neutral':
        return <FontAwesomeIcon icon={faCircle} />;
    }
  };

  const getLabel = () => {
    if (label) return label;
    switch (status) {
      case 'healthy':
        return 'Healthy';
      case 'warning':
        return 'Warning';
      case 'critical':
        return 'Critical';
      case 'neutral':
        return 'Inactive';
    }
  };

  return (
    <span className={`status-badge ${status}`}>
      {showIcon && getIcon()}
      {getLabel()}
    </span>
  );
};

export default StatusBadge;
