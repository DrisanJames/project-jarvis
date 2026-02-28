import React, { useEffect, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faTimes, faClock, faBolt, faInfoCircle, IconDefinition } from '@fortawesome/free-solid-svg-icons';

interface NotificationToastProps {
  type: 'due' | 'ai' | 'info';
  title: string;
  message: string;
  onDismiss: () => void;
  onClick?: () => void;
  autoHide?: boolean;
  hideAfter?: number; // ms
}

const typeConfig: Record<string, { icon: IconDefinition; color: string; bgColor: string }> = {
  due: {
    icon: faClock,
    color: 'var(--accent-yellow)',
    bgColor: 'rgba(251, 191, 36, 0.1)',
  },
  ai: {
    icon: faBolt,
    color: 'var(--accent-blue)',
    bgColor: 'rgba(59, 130, 246, 0.1)',
  },
  info: {
    icon: faInfoCircle,
    color: 'var(--text-muted)',
    bgColor: 'rgba(148, 163, 184, 0.1)',
  },
};

export const NotificationToast: React.FC<NotificationToastProps> = ({
  type,
  title,
  message,
  onDismiss,
  onClick,
  autoHide = true,
  hideAfter = 10000,
}) => {
  const [isExiting, setIsExiting] = useState(false);
  const config = typeConfig[type];

  useEffect(() => {
    if (autoHide) {
      const timer = setTimeout(() => {
        setIsExiting(true);
        setTimeout(onDismiss, 300);
      }, hideAfter);

      return () => clearTimeout(timer);
    }
  }, [autoHide, hideAfter, onDismiss]);

  const handleClick = () => {
    if (onClick) {
      onClick();
    }
  };

  const handleDismiss = (e: React.MouseEvent) => {
    e.stopPropagation();
    setIsExiting(true);
    setTimeout(onDismiss, 300);
  };

  return (
    <div 
      className={`notification-toast ${isExiting ? 'exiting' : ''}`}
      style={{ 
        backgroundColor: config.bgColor,
        borderColor: config.color,
      }}
      onClick={handleClick}
    >
      <div className="notification-icon" style={{ color: config.color }}>
        <FontAwesomeIcon icon={config.icon} />
      </div>
      <div className="notification-content">
        <div className="notification-title" style={{ color: config.color }}>
          {title}
        </div>
        <div className="notification-message">{message}</div>
      </div>
      <button className="notification-close" onClick={handleDismiss}>
        <FontAwesomeIcon icon={faTimes} />
      </button>
    </div>
  );
};
