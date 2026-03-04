import React, { useState, useEffect, useCallback, createContext, useContext } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faCheckCircle, faExclamationTriangle, faInfoCircle,
  faTimes, faRocket, faBolt,
} from '@fortawesome/free-solid-svg-icons';

type ToastType = 'success' | 'error' | 'warning' | 'info' | 'campaign';

interface Toast {
  id: string;
  type: ToastType;
  title: string;
  message?: string;
  duration?: number;
  exiting?: boolean;
}

interface ToastContextValue {
  addToast: (toast: Omit<Toast, 'id'>) => void;
  campaignComplete: (name: string) => void;
  campaignAlert: (name: string, message: string) => void;
}

const ToastContext = createContext<ToastContextValue>({
  addToast: () => {},
  campaignComplete: () => {},
  campaignAlert: () => {},
});

export const useToast = () => useContext(ToastContext);

const TOAST_COLORS: Record<ToastType, { bg: string; border: string; icon: string; glow: string }> = {
  success:  { bg: 'rgba(0, 184, 148, 0.12)', border: 'rgba(0, 184, 148, 0.4)', icon: '#00b894', glow: 'rgba(0, 184, 148, 0.2)' },
  error:    { bg: 'rgba(233, 69, 96, 0.12)', border: 'rgba(233, 69, 96, 0.4)', icon: '#e94560', glow: 'rgba(233, 69, 96, 0.2)' },
  warning:  { bg: 'rgba(253, 203, 110, 0.12)', border: 'rgba(253, 203, 110, 0.4)', icon: '#fdcb6e', glow: 'rgba(253, 203, 110, 0.2)' },
  info:     { bg: 'rgba(0, 229, 255, 0.08)', border: 'rgba(0, 229, 255, 0.3)', icon: '#00e5ff', glow: 'rgba(0, 229, 255, 0.15)' },
  campaign: { bg: 'rgba(0, 229, 255, 0.1)', border: 'rgba(0, 229, 255, 0.5)', icon: '#00e5ff', glow: 'rgba(0, 229, 255, 0.25)' },
};

const TOAST_ICONS = {
  success: faCheckCircle,
  error: faExclamationTriangle,
  warning: faExclamationTriangle,
  info: faInfoCircle,
  campaign: faRocket,
};

const ToastItem: React.FC<{ toast: Toast; onDismiss: (id: string) => void }> = ({ toast, onDismiss }) => {
  const colors = TOAST_COLORS[toast.type];
  const [progress, setProgress] = useState(100);
  const duration = toast.duration || 5000;

  useEffect(() => {
    const start = performance.now();
    let raf: number;
    const tick = (now: number) => {
      const elapsed = now - start;
      setProgress(Math.max(0, 100 - (elapsed / duration) * 100));
      if (elapsed < duration) raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf);
  }, [duration]);

  return (
    <div
      className={toast.exiting ? 'ig-alert-exit' : 'ig-alert-enter'}
      style={{
        background: colors.bg,
        backdropFilter: 'blur(12px)',
        border: `1px solid ${colors.border}`,
        borderRadius: 12,
        padding: '14px 16px',
        marginBottom: 10,
        display: 'flex',
        alignItems: 'flex-start',
        gap: 12,
        minWidth: 320,
        maxWidth: 420,
        boxShadow: `0 4px 24px rgba(0,0,0,0.3), 0 0 30px ${colors.glow}`,
        position: 'relative',
        overflow: 'hidden',
      }}
    >
      <div style={{
        width: 32, height: 32, borderRadius: 8,
        background: `${colors.icon}15`, display: 'flex',
        alignItems: 'center', justifyContent: 'center', flexShrink: 0,
      }}>
        <FontAwesomeIcon icon={TOAST_ICONS[toast.type]} style={{ color: colors.icon, fontSize: 14 }} />
      </div>

      <div style={{ flex: 1, minWidth: 0 }}>
        <div style={{ color: '#e0e6f0', fontSize: 13, fontWeight: 600, marginBottom: toast.message ? 2 : 0 }}>
          {toast.type === 'campaign' && (
            <FontAwesomeIcon icon={faBolt} style={{ color: '#00e5ff', marginRight: 6, fontSize: 10 }} />
          )}
          {toast.title}
        </div>
        {toast.message && (
          <div style={{ color: 'rgba(180,210,240,0.65)', fontSize: 12, lineHeight: 1.4 }}>
            {toast.message}
          </div>
        )}
      </div>

      <button
        onClick={() => onDismiss(toast.id)}
        style={{
          background: 'none', border: 'none', color: 'rgba(180,210,240,0.4)',
          cursor: 'pointer', padding: 4, fontSize: 12, flexShrink: 0,
        }}
      >
        <FontAwesomeIcon icon={faTimes} />
      </button>

      <div style={{
        position: 'absolute', bottom: 0, left: 0, height: 2,
        width: `${progress}%`, background: colors.icon,
        transition: 'width 0.1s linear', borderRadius: '0 0 0 12px',
        opacity: 0.6,
      }} />
    </div>
  );
};

export const ToastProvider: React.FC<{ children: React.ReactNode }> = ({ children }) => {
  const [toasts, setToasts] = useState<Toast[]>([]);

  const addToast = useCallback((toast: Omit<Toast, 'id'>) => {
    const id = `toast-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`;
    setToasts(prev => [...prev, { ...toast, id }]);

    const dur = toast.duration || 5000;
    setTimeout(() => {
      setToasts(prev => prev.map(t => t.id === id ? { ...t, exiting: true } : t));
      setTimeout(() => setToasts(prev => prev.filter(t => t.id !== id)), 350);
    }, dur);
  }, []);

  const campaignComplete = useCallback((name: string) => {
    addToast({
      type: 'campaign',
      title: 'Campaign Deployed',
      message: `${name} is now live. JARVIS is monitoring delivery.`,
      duration: 8000,
    });
  }, [addToast]);

  const campaignAlert = useCallback((name: string, message: string) => {
    addToast({
      type: 'info',
      title: name,
      message,
      duration: 6000,
    });
  }, [addToast]);

  const dismiss = useCallback((id: string) => {
    setToasts(prev => prev.map(t => t.id === id ? { ...t, exiting: true } : t));
    setTimeout(() => setToasts(prev => prev.filter(t => t.id !== id)), 350);
  }, []);

  return (
    <ToastContext.Provider value={{ addToast, campaignComplete, campaignAlert }}>
      {children}
      <div style={{
        position: 'fixed', top: 20, right: 20, zIndex: 10000,
        display: 'flex', flexDirection: 'column', alignItems: 'flex-end',
        pointerEvents: 'none',
      }}>
        <div style={{ pointerEvents: 'auto' }}>
          {toasts.map(t => <ToastItem key={t.id} toast={t} onDismiss={dismiss} />)}
        </div>
      </div>
    </ToastContext.Provider>
  );
};
