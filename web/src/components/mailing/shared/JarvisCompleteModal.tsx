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
        background: 'rgba(5, 8, 15, 0.85)',
        display: 'flex', alignItems: 'center', justifyContent: 'center',
        backdropFilter: 'blur(6px)',
      }}
    >
      <div
        className="ig-modal-content"
        onClick={e => e.stopPropagation()}
        style={{
          background: 'linear-gradient(135deg, #0d1526 0%, #0a1628 50%, #0d1526 100%)',
          border: '1px solid rgba(0, 229, 255, 0.2)',
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
            background: 'linear-gradient(90deg, transparent 0%, rgba(0,229,255,0.4) 50%, transparent 100%)',
            animation: 'ig-scan 3s linear infinite',
          }} />
        </div>

        {/* Close button */}
        <button
          onClick={onClose}
          style={{
            position: 'absolute', top: 16, right: 16,
            background: 'none', border: 'none', color: 'rgba(180,210,240,0.4)',
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
            borderTopColor: 'rgba(0, 229, 255, 0.6)',
            borderRightColor: 'rgba(0, 229, 255, 0.2)',
          }} />
          {/* Inner ring */}
          <div className="ig-orbit-ring-reverse" style={{
            position: 'absolute', inset: 12, borderRadius: '50%',
            border: '1px solid transparent',
            borderBottomColor: 'rgba(0, 176, 255, 0.5)',
            borderLeftColor: 'rgba(0, 176, 255, 0.15)',
          }} />
          {/* Core glow */}
          <div style={{
            width: 44, height: 44, borderRadius: '50%',
            background: 'radial-gradient(circle, rgba(0,229,255,0.3) 0%, transparent 70%)',
            display: 'flex', alignItems: 'center', justifyContent: 'center',
            animation: 'ig-pulse-glow 2s ease-in-out infinite',
          }}>
            <FontAwesomeIcon
              icon={phase >= 2 ? faCheckCircle : faRocket}
              style={{
                fontSize: 22,
                color: phase >= 2 ? '#00b894' : '#00e5ff',
                transition: 'all 0.5s cubic-bezier(0.34, 1.56, 0.64, 1)',
                transform: phase >= 2 ? 'scale(1.2)' : 'scale(1)',
              }}
            />
          </div>
        </div>

        {/* Title */}
        <h2 style={{
          color: '#e0e6f0', fontSize: 22, fontWeight: 700, margin: '0 0 6px',
          opacity: phase >= 1 ? 1 : 0,
          transform: phase >= 1 ? 'translateY(0)' : 'translateY(10px)',
          transition: 'all 0.5s cubic-bezier(0.4, 0, 0.2, 1)',
        }}>
          Campaign Deployed
        </h2>

        <div style={{
          color: 'rgba(0, 229, 255, 0.8)', fontSize: 14, fontWeight: 500,
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
                background: 'rgba(0, 229, 255, 0.06)', borderRadius: 10, padding: '12px 20px',
                border: '1px solid rgba(0, 229, 255, 0.1)',
              }}>
                <div style={{ color: '#00e5ff', fontSize: 20, fontWeight: 700 }}>
                  {stats.recipients.toLocaleString()}
                </div>
                <div style={{ color: 'rgba(180,210,240,0.5)', fontSize: 11, marginTop: 2 }}>Recipients</div>
              </div>
            )}
            {stats.variants != null && (
              <div style={{
                background: 'rgba(0, 176, 255, 0.06)', borderRadius: 10, padding: '12px 20px',
                border: '1px solid rgba(0, 176, 255, 0.1)',
              }}>
                <div style={{ color: '#00b0ff', fontSize: 20, fontWeight: 700 }}>{stats.variants}</div>
                <div style={{ color: 'rgba(180,210,240,0.5)', fontSize: 11, marginTop: 2 }}>Variants</div>
              </div>
            )}
          </div>
        )}

        {/* JARVIS monitoring message */}
        <div style={{
          color: 'rgba(180,210,240,0.5)', fontSize: 12, lineHeight: 1.6,
          opacity: phase >= 3 ? 1 : 0,
          transition: 'opacity 0.5s ease',
        }}>
          <FontAwesomeIcon icon={faChartLine} style={{ color: '#00b894', marginRight: 6 }} />
          JARVIS is actively monitoring delivery performance
        </div>

        {/* Action button */}
        <button
          onClick={onClose}
          className="ig-btn-glow ig-ripple"
          style={{
            marginTop: 24, padding: '10px 32px', borderRadius: 10,
            background: 'linear-gradient(135deg, #00e5ff 0%, #00b0ff 100%)',
            color: '#0a0f1a', border: 'none', fontSize: 13, fontWeight: 600,
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
