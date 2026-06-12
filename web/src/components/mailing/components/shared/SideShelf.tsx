import React, { useEffect } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faTimes } from '@fortawesome/free-solid-svg-icons';

// ============================================================================
// SideShelf — reusable right-side slide-out panel
//
// Replicates the agent-panel pattern from EmailMarketingAgent.tsx (the
// fixed right-side detail shelf): full-height, dark slab background, indigo
// hairline border, deep left shadow. Closes on ESC or on clicking the light
// translucent overlay behind it; the page behind remains scrollable.
// ============================================================================

interface SideShelfProps {
  title: React.ReactNode;
  onClose: () => void;
  /** Panel width in px (default 560, matching the marketing-agent detail shelf). */
  width?: number;
  children: React.ReactNode;
}

export const SideShelf: React.FC<SideShelfProps> = ({ title, onClose, width = 560, children }) => {
  // ESC closes the shelf.
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [onClose]);

  return (
    <>
      {/* Click-capturing overlay BEHIND the shelf. Intentionally light — it
          should not heavily dim the screen — and it does not lock body scroll. */}
      <div
        onClick={onClose}
        style={{
          position: 'fixed',
          inset: 0,
          background: 'rgba(0,0,0,0.25)',
          zIndex: 199,
        }}
      />
      <div
        style={{
          position: 'fixed',
          top: 0,
          right: 0,
          width,
          height: '100vh',
          background: '#0f1629',
          borderLeft: '1px solid rgba(99,102,241,0.15)',
          boxShadow: '-10px 0 40px rgba(0,0,0,0.5)',
          zIndex: 200,
          display: 'flex',
          flexDirection: 'column',
        }}
      >
        <div
          style={{
            padding: '16px 20px',
            borderBottom: '1px solid rgba(99,102,241,0.1)',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
            gap: 12,
            flexShrink: 0,
          }}
        >
          <div style={{ minWidth: 0, flex: 1, fontSize: 15, fontWeight: 700, color: '#e2e8f0' }}>
            {title}
          </div>
          <button
            onClick={onClose}
            aria-label="Close"
            style={{
              background: 'none',
              border: 'none',
              color: '#64748b',
              cursor: 'pointer',
              fontSize: 16,
              flexShrink: 0,
              padding: 0,
            }}
          >
            <FontAwesomeIcon icon={faTimes} />
          </button>
        </div>
        <div style={{ flex: 1, overflowY: 'auto' }}>{children}</div>
      </div>
    </>
  );
};
