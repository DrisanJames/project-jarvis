import React, { useEffect, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faRocket, faCheckCircle, faChartLine, faTimes } from '@fortawesome/free-solid-svg-icons';

interface JarvisCompleteModalProps {
  visible: boolean;
  onClose: () => void;
  campaignName: string;
  stats?: {
    recipients?: number;
    variants?: number;
    scheduledTime?: string;
  };
}

export const JarvisCompleteModal: React.FC<JarvisCompleteModalProps> = ({
  visible, onClose, campaignName, stats,
}) => {
  const [phase, setPhase] = useState(0);

  useEffect(() => {
    if (!visible) { setPhase(0); return; }
    const t1 = setTimeout(() => setPhase(1), 300);
    const t2 = setTimeout(() => setPhase(2), 900);
    const t3 = setTimeout(() => setPhase(3), 1500);
    return () => { clearTimeout(t1); clearTimeout(t2); clearTimeout(t3); };
  }, [visible]);

  if (!visible) return null;

  return (
    <div
      className="ig-modal-overlay"
      onClick={onClose}
      style={{
        position: 'fixed', inset: 0, zIndex: 9999,
        background: 'color-mix(in srgb, var(--j-shade) 85%, transparent)',
        display: 'flex', alignItems: 'center', justifyContent: 'center',
        backdropFilter: 'blur(6px)',
      }}
    >
      <div
        className="ig-modal-content"
        onClick={e => e.stopPropagation()}
        style={{
          background: 'linear-gradient(135deg, var(--j-surface) 0%, var(--j-surface) 50%, var(--j-surface) 100%)',
          border: '1px solid color-mix(in srgb, var(--j-cyan) 20%, transparent)',
          borderRadius: 20,
          padding: '40px 48px',
          maxWidth: 480,
          width: '90%',
          textAlign: 'center',
          position: 'relative',
          overflow: 'hidden',
        }}
      >
        {/* Animated scan line */}
        <div style={{
          position: 'absolute', top: 0, left: 0, right: 0, bottom: 0,
          pointerEvents: 'none', overflow: 'hidden', borderRadius: 20,
        }}>
          <div style={{
            position: 'absolute', top: '-100%', left: 0, right: 0, height: 1,
            background: 'linear-gradient(90deg, transparent 0%, color-mix(in srgb, var(--j-cyan) 40%, transparent) 50%, transparent 100%)',
            animation: 'ig-scan 3s linear infinite',
          }} />
        </div>

        {/* Close button */}
        <button aria-label="Close"
          onClick={onClose}
          style={{
            position: 'absolute', top: 16, right: 16,
            background: 'none', border: 'none', color: 'var(--j-text-muted)',
            cursor: 'pointer', fontSize: 16, padding: 4,
          }}
        >
          <FontAwesomeIcon icon={faTimes} />
        </button>

        {/* Jarvis orb */}
        <div style={{
          width: 100, height: 100, margin: '0 auto 24px',
          position: 'relative', display: 'flex', alignItems: 'center', justifyContent: 'center',
        }}>
          {/* Outer ring */}
          <div className="ig-orbit-ring" style={{
            position: 'absolute', inset: 0, borderRadius: '50%',
            border: '2px solid transparent',
            borderTopColor: 'color-mix(in srgb, var(--j-cyan) 60%, transparent)',
            borderRightColor: 'color-mix(in srgb, var(--j-cyan) 20%, transparent)',
          }} />
          {/* Inner ring */}
          <div className="ig-orbit-ring-reverse" style={{
            position: 'absolute', inset: 12, borderRadius: '50%',
            border: '1px solid transparent',
            borderBottomColor: 'color-mix(in srgb, var(--j-blue) 50%, transparent)',
            borderLeftColor: 'color-mix(in srgb, var(--j-blue) 15%, transparent)',
          }} />
          {/* Core glow */}
          <div style={{
            width: 44, height: 44, borderRadius: '50%',
            background: 'radial-gradient(circle, color-mix(in srgb, var(--j-cyan) 30%, transparent) 0%, transparent 70%)',
            display: 'flex', alignItems: 'center', justifyContent: 'center',
            animation: 'ig-pulse-glow 2s ease-in-out infinite',
          }}>
            <FontAwesomeIcon
              icon={phase >= 2 ? faCheckCircle : faRocket}
              style={{
                fontSize: 22,
                color: phase >= 2 ? 'var(--j-success)' : 'var(--j-cyan)',
                transition: 'all 0.5s cubic-bezier(0.34, 1.56, 0.64, 1)',
                transform: phase >= 2 ? 'scale(1.2)' : 'scale(1)',
              }}
            />
          </div>
        </div>

        {/* Title */}
        <h2 style={{
          color: 'var(--j-text)', fontSize: 22, fontWeight: 700, margin: '0 0 6px',
          opacity: phase >= 1 ? 1 : 0,
          transform: phase >= 1 ? 'translateY(0)' : 'translateY(10px)',
          transition: 'all 0.5s cubic-bezier(0.4, 0, 0.2, 1)',
        }}>
          Campaign Deployed
        </h2>

        <div style={{
          color: 'var(--j-text-2)', fontSize: 14, fontWeight: 500,
          marginBottom: 20,
          opacity: phase >= 1 ? 1 : 0,
          transition: 'opacity 0.5s ease 0.15s',
        }}>
          {campaignName}
        </div>

        {/* Stats row */}
        {stats && (
          <div style={{
            display: 'flex', gap: 16, justifyContent: 'center', marginBottom: 24,
            opacity: phase >= 2 ? 1 : 0,
            transform: phase >= 2 ? 'translateY(0)' : 'translateY(10px)',
            transition: 'all 0.5s ease',
          }}>
            {stats.recipients != null && (
              <div style={{
                background: 'color-mix(in srgb, var(--j-cyan) 6%, transparent)', borderRadius: 10, padding: '12px 20px',
                border: '1px solid color-mix(in srgb, var(--j-cyan) 10%, transparent)',
              }}>
                <div style={{ color: 'var(--j-cyan)', fontSize: 20, fontWeight: 700 }}>
                  {stats.recipients.toLocaleString()}
                </div>
                <div style={{ color: 'var(--j-text-muted)', fontSize: 11, marginTop: 2 }}>Recipients</div>
              </div>
            )}
            {stats.variants != null && (
              <div style={{
                background: 'color-mix(in srgb, var(--j-blue) 6%, transparent)', borderRadius: 10, padding: '12px 20px',
                border: '1px solid color-mix(in srgb, var(--j-blue) 10%, transparent)',
              }}>
                <div style={{ color: 'var(--j-blue)', fontSize: 20, fontWeight: 700 }}>{stats.variants}</div>
                <div style={{ color: 'var(--j-text-muted)', fontSize: 11, marginTop: 2 }}>Variants</div>
              </div>
            )}
          </div>
        )}

        {/* JARVIS monitoring message */}
        <div style={{
          color: 'var(--j-text-muted)', fontSize: 12, lineHeight: 1.6,
          opacity: phase >= 3 ? 1 : 0,
          transition: 'opacity 0.5s ease',
        }}>
          <FontAwesomeIcon icon={faChartLine} style={{ color: 'var(--j-success)', marginRight: 6 }} />
          Jarvis is actively monitoring your delivery performance
        </div>

        {/* Action button */}
        <button
          onClick={onClose}
          className="ig-btn-glow ig-ripple"
          style={{
            marginTop: 24, padding: '10px 32px', borderRadius: 10,
            background: 'linear-gradient(135deg, var(--j-cyan) 0%, var(--j-blue) 100%)',
            color: 'var(--j-bg)', border: 'none', fontSize: 13, fontWeight: 600,
            cursor: 'pointer',
            opacity: phase >= 3 ? 1 : 0,
            transform: phase >= 3 ? 'translateY(0)' : 'translateY(8px)',
            transition: 'all 0.4s ease',
          }}
        >
          View Campaign
        </button>
      </div>
    </div>
  );
};

export default JarvisCompleteModal;
