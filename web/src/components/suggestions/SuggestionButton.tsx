import React, { useState, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faLightbulb, faTimes, faPaperPlane, faSpinner, faMousePointer } from '@fortawesome/free-solid-svg-icons';
import { CreateSuggestionRequest } from './types';

interface SuggestionButtonProps {
  onSuggestionSubmitted?: () => void;
}

type ModalState = 'closed' | 'selecting' | 'form';

export const SuggestionButton: React.FC<SuggestionButtonProps> = ({ onSuggestionSubmitted }) => {
  const [modalState, setModalState] = useState<ModalState>('closed');
  const [selectedArea, setSelectedArea] = useState<string>('');
  const [areaContext, setAreaContext] = useState<string>('');
  const [suggestion, setSuggestion] = useState<string>('');
  const [isSubmitting, setIsSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [submitSuccess, setSubmitSuccess] = useState(false);

  const handleOpen = () => {
    setModalState('selecting');
    setSubmitError(null);
    setSubmitSuccess(false);
  };

  const handleClose = () => {
    setModalState('closed');
    setSelectedArea('');
    setAreaContext('');
    setSuggestion('');
    setSubmitError(null);
    setSubmitSuccess(false);
  };

  const handleAreaClick = useCallback((event: React.MouseEvent) => {
    if (modalState !== 'selecting') return;
    
    event.stopPropagation();
    
    // Find the closest named element or section
    const target = event.target as HTMLElement;
    let areaName = 'General';
    let context = '';

    // Try to find meaningful parent elements
    const navItem = target.closest('.nav-item');
    const card = target.closest('.card');
    const section = target.closest('section');
    const main = target.closest('.main-content');
    
    if (navItem) {
      areaName = navItem.textContent?.trim() || 'Navigation';
    } else if (card) {
      const cardHeader = card.querySelector('.card-header, h2, h3');
      areaName = cardHeader?.textContent?.trim() || 'Card Component';
      context = `Element type: ${target.tagName.toLowerCase()}`;
    } else if (section) {
      const sectionHeader = section.querySelector('h1, h2, h3');
      areaName = sectionHeader?.textContent?.trim() || 'Section';
    } else if (main) {
      areaName = 'Main Dashboard';
    }

    // Add element context
    if (target.className) {
      context += ` | Classes: ${target.className.split(' ').slice(0, 3).join(', ')}`;
    }

    setSelectedArea(areaName);
    setAreaContext(context);
    setModalState('form');
  }, [modalState]);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!suggestion.trim()) return;

    setIsSubmitting(true);
    setSubmitError(null);

    try {
      const request: CreateSuggestionRequest = {
        area: selectedArea,
        area_context: areaContext,
        original_suggestion: suggestion,
      };

      const response = await fetch('/api/suggestions/', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify(request),
      });

      if (!response.ok) {
        throw new Error('Failed to submit suggestion');
      }

      setSubmitSuccess(true);
      onSuggestionSubmitted?.();
      
      // Close after showing success
      setTimeout(() => {
        handleClose();
      }, 2000);
    } catch (err) {
      setSubmitError(err instanceof Error ? err.message : 'Failed to submit suggestion');
    } finally {
      setIsSubmitting(false);
    }
  };

  return (
    <>
      {/* Floating Button */}
      <button
        onClick={handleOpen}
        style={styles.floatingButton}
        title="Suggest an improvement"
      >
        <FontAwesomeIcon icon={faLightbulb} style={{ fontSize: '24px' }} />
      </button>

      {/* Overlay for area selection */}
      {modalState === 'selecting' && (
        <div style={styles.overlay} onClick={handleAreaClick}>
          <div style={styles.instructionBanner}>
            <FontAwesomeIcon icon={faMousePointer} style={{ fontSize: '20px' }} />
            <span>Tap the area you would like to improve</span>
            <button onClick={handleClose} style={styles.closeButton}>
              <FontAwesomeIcon icon={faTimes} />
            </button>
          </div>
        </div>
      )}

      {/* Form Modal */}
      {modalState === 'form' && (
        <div style={styles.modalOverlay}>
          <div style={styles.modal}>
            <div style={styles.modalHeader}>
              <h2 style={styles.modalTitle}>
                <FontAwesomeIcon icon={faLightbulb} />
                Suggest an Improvement
              </h2>
              <button onClick={handleClose} style={styles.closeButton}>
                <FontAwesomeIcon icon={faTimes} />
              </button>
            </div>

            <form onSubmit={handleSubmit} style={styles.form}>
              <div style={styles.areaInfo}>
                <label style={styles.label}>Selected Area</label>
                <div style={styles.areaValue}>{selectedArea}</div>
                {areaContext && (
                  <div style={styles.areaContext}>{areaContext}</div>
                )}
              </div>

              <div style={styles.inputGroup}>
                <label style={styles.label}>Your Suggestion</label>
                <textarea
                  value={suggestion}
                  onChange={(e) => setSuggestion(e.target.value)}
                  placeholder="Describe what you'd like to improve or change..."
                  style={styles.textarea}
                  rows={5}
                  autoFocus
                />
              </div>

              {submitError && (
                <div style={styles.error}>{submitError}</div>
              )}

              {submitSuccess && (
                <div style={styles.success}>
                  Suggestion submitted successfully! Our AI is generating requirements...
                </div>
              )}

              <div style={styles.actions}>
                <button
                  type="button"
                  onClick={() => setModalState('selecting')}
                  style={styles.secondaryButton}
                >
                  Change Area
                </button>
                <button
                  type="submit"
                  disabled={!suggestion.trim() || isSubmitting}
                  style={{
                    ...styles.primaryButton,
                    opacity: !suggestion.trim() || isSubmitting ? 0.5 : 1,
                  }}
                >
                  {isSubmitting ? (
                    <>
                      <FontAwesomeIcon icon={faSpinner} spin />
                      Submitting...
                    </>
                  ) : (
                    <>
                      <FontAwesomeIcon icon={faPaperPlane} />
                      Submit Suggestion
                    </>
                  )}
                </button>
              </div>
            </form>
          </div>
        </div>
      )}
    </>
  );
};

const styles: Record<string, React.CSSProperties> = {
  floatingButton: {
    position: 'fixed',
    right: '20px',
    top: '50%',
    transform: 'translateY(-50%)',
    width: '56px',
    height: '56px',
    borderRadius: '50%',
    backgroundColor: 'var(--accent-yellow, #facc15)',
    color: '#000',
    border: 'none',
    cursor: 'pointer',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    boxShadow: '0 4px 12px rgba(250, 204, 21, 0.4)',
    zIndex: 1000,
    transition: 'all 0.2s ease',
  },
  overlay: {
    position: 'fixed',
    top: 0,
    left: 0,
    right: 0,
    bottom: 0,
    backgroundColor: 'rgba(0, 0, 0, 0.6)',
    zIndex: 9998,
    cursor: 'crosshair',
  },
  instructionBanner: {
    position: 'fixed',
    top: '20px',
    left: '50%',
    transform: 'translateX(-50%)',
    backgroundColor: 'var(--accent-yellow, #facc15)',
    color: '#000',
    padding: '1rem 2rem',
    borderRadius: '12px',
    display: 'flex',
    alignItems: 'center',
    gap: '0.75rem',
    fontSize: '1rem',
    fontWeight: 600,
    boxShadow: '0 4px 20px rgba(0, 0, 0, 0.3)',
    zIndex: 9999,
  },
  modalOverlay: {
    position: 'fixed',
    top: 0,
    left: 0,
    right: 0,
    bottom: 0,
    backgroundColor: 'rgba(0, 0, 0, 0.7)',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    zIndex: 9999,
    padding: '1rem',
  },
  modal: {
    backgroundColor: 'var(--bg-secondary, #1e1e2e)',
    borderRadius: '16px',
    border: '1px solid var(--border-color, #333)',
    width: '100%',
    maxWidth: '500px',
    maxHeight: '90vh',
    overflow: 'auto',
  },
  modalHeader: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    padding: '1.25rem 1.5rem',
    borderBottom: '1px solid var(--border-color, #333)',
  },
  modalTitle: {
    display: 'flex',
    alignItems: 'center',
    gap: '0.5rem',
    margin: 0,
    fontSize: '1.25rem',
    fontWeight: 600,
    color: 'var(--text-primary, #fff)',
  },
  closeButton: {
    background: 'none',
    border: 'none',
    color: 'var(--text-muted, #888)',
    cursor: 'pointer',
    padding: '0.25rem',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
  },
  form: {
    padding: '1.5rem',
    display: 'flex',
    flexDirection: 'column' as const,
    gap: '1.25rem',
  },
  areaInfo: {
    padding: '1rem',
    backgroundColor: 'var(--bg-tertiary, #2a2a3e)',
    borderRadius: '8px',
  },
  label: {
    display: 'block',
    fontSize: '0.75rem',
    fontWeight: 500,
    color: 'var(--text-muted, #888)',
    marginBottom: '0.375rem',
    textTransform: 'uppercase' as const,
    letterSpacing: '0.05em',
  },
  areaValue: {
    fontSize: '1rem',
    fontWeight: 600,
    color: 'var(--text-primary, #fff)',
  },
  areaContext: {
    fontSize: '0.75rem',
    color: 'var(--text-muted, #888)',
    marginTop: '0.25rem',
  },
  inputGroup: {
    display: 'flex',
    flexDirection: 'column' as const,
  },
  textarea: {
    width: '100%',
    padding: '0.875rem',
    fontSize: '0.9375rem',
    backgroundColor: 'var(--bg-tertiary, #2a2a3e)',
    border: '1px solid var(--border-color, #333)',
    borderRadius: '8px',
    color: 'var(--text-primary, #fff)',
    resize: 'vertical' as const,
    outline: 'none',
    fontFamily: 'inherit',
    lineHeight: 1.5,
  },
  error: {
    padding: '0.75rem',
    backgroundColor: 'rgba(239, 68, 68, 0.1)',
    border: '1px solid var(--accent-red, #ef4444)',
    borderRadius: '8px',
    color: 'var(--accent-red, #ef4444)',
    fontSize: '0.875rem',
  },
  success: {
    padding: '0.75rem',
    backgroundColor: 'rgba(34, 197, 94, 0.1)',
    border: '1px solid var(--accent-green, #22c55e)',
    borderRadius: '8px',
    color: 'var(--accent-green, #22c55e)',
    fontSize: '0.875rem',
  },
  actions: {
    display: 'flex',
    gap: '0.75rem',
    justifyContent: 'flex-end',
  },
  secondaryButton: {
    padding: '0.75rem 1.25rem',
    fontSize: '0.875rem',
    fontWeight: 500,
    backgroundColor: 'transparent',
    color: 'var(--text-muted, #888)',
    border: '1px solid var(--border-color, #333)',
    borderRadius: '8px',
    cursor: 'pointer',
  },
  primaryButton: {
    display: 'flex',
    alignItems: 'center',
    gap: '0.5rem',
    padding: '0.75rem 1.25rem',
    fontSize: '0.875rem',
    fontWeight: 500,
    backgroundColor: 'var(--accent-blue, #3b82f6)',
    color: '#fff',
    border: 'none',
    borderRadius: '8px',
    cursor: 'pointer',
  },
};

export default SuggestionButton;
