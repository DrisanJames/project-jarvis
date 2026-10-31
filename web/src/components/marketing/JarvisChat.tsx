import React, { useEffect, useRef, useState } from 'react';
import { JarvisMark } from './JarvisLogo';
import { LeadForm } from './LeadForm';

// Bottom-right Jarvis launcher. For now the window holds a contact form; there
// is no assistant behind it, so it never pretends to answer.
export const JarvisChat: React.FC = () => {
  const [open, setOpen] = useState(false);
  const launcher = useRef<HTMLButtonElement>(null);
  const panel = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    panel.current?.querySelector('input')?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return;
      setOpen(false);
      launcher.current?.focus();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [open]);

  return (
    <aside className="jv-chatw" aria-label="Contact">
      {open && (
        <div className="jv-chatw-panel" role="dialog" aria-labelledby="jv-chatw-title" ref={panel}>
          <div className="jv-chatw-head">
            <JarvisMark size={38} title="Jarvis" mood="wink" />
            <div>
              <h2 id="jv-chatw-title">Contact us</h2>
              <p>Leave a message and the team will reply by email.</p>
            </div>
            <button
              type="button"
              className="jv-chatw-close"
              aria-label="Close"
              onClick={() => {
                setOpen(false);
                launcher.current?.focus();
              }}
            >
              ×
            </button>
          </div>
          <div className="jv-chatw-body">
            <LeadForm
              kind="contact"
              compact
              fixedUseCase="Chat window"
              submitLabel="Send message"
              success="Thanks. We’ve received your message."
              fields={[
                { name: 'name', label: 'Name', kind: 'text', required: true, autoComplete: 'name', wide: true },
                { name: 'email', label: 'Email', kind: 'email', required: true, autoComplete: 'email', wide: true },
                { name: 'notes', label: 'Message', kind: 'textarea', required: true },
              ]}
            />
          </div>
        </div>
      )}
      <button
        ref={launcher}
        type="button"
        className="jv-chatw-launcher"
        aria-label={open ? 'Close contact window' : 'Contact us'}
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
      >
        <JarvisMark size={44} title="" animated look />
      </button>
    </aside>
  );
};
