import React, { useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faSpinner, faCopy, faExclamationTriangle, faCheckCircle } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';

interface Props {
  onClose: () => void;
}

const VERTICALS = [
  { value: 'refi_heloc', label: 'Refi / HELOC (Amerisave)' },
  { value: 'personal_loans', label: 'Personal Loans' },
  { value: 'tax_relief', label: 'Tax Relief (Optima)' },
  { value: 'remodel', label: 'Remodel (Renewal by Andersen)' },
];

type Step = 1 | 2 | 3 | 4;

export const PartnerOnboardingWizard: React.FC<Props> = ({ onClose }) => {
  const [step, setStep] = useState<Step>(1);

  // Step 1: partner info
  const [partnerName, setPartnerName] = useState('');
  const [contactEmail, setContactEmail] = useState('');
  const [partnerNotes, setPartnerNotes] = useState('');
  const [partnerId, setPartnerId] = useState<string | null>(null);

  // Step 2: dataset
  const [datasetName, setDatasetName] = useState('');
  const [vertical, setVertical] = useState(VERTICALS[0].value);
  const [flushHours, setFlushHours] = useState(24);

  // Step 3: api key
  const [apiKey, setApiKey] = useState<string | null>(null);
  const [apiKeyPrefix, setApiKeyPrefix] = useState<string | null>(null);

  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleCreatePartner = async () => {
    if (!partnerName.trim()) {
      setError('Partner name is required');
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      const res = await apiFetch('/api/mailing/data-partners', {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: partnerName, contact_email: contactEmail, notes: partnerNotes }),
      });
      const body = await res.json();
      if (!res.ok) {
        setError(body.error ?? `HTTP ${res.status}`);
        return;
      }
      setPartnerId(body.id);
      setStep(2);
    } catch (err) {
      setError(String(err));
    } finally {
      setSubmitting(false);
    }
  };

  const handleCreateDataset = async () => {
    if (!partnerId) return;
    if (!datasetName.trim()) {
      setError('Dataset name is required');
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      const res = await apiFetch(`/api/mailing/data-partners/${partnerId}/datasets`, {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: datasetName, vertical, flush_window_hours: flushHours }),
      });
      const body = await res.json();
      if (!res.ok) {
        setError(body.error ?? `HTTP ${res.status}`);
        return;
      }
      setApiKey(body.api_key);
      setApiKeyPrefix(body.api_key_prefix);
      setStep(3);
    } catch (err) {
      setError(String(err));
    } finally {
      setSubmitting(false);
    }
  };

  const copyKey = async () => {
    if (apiKey) {
      try { await navigator.clipboard.writeText(apiKey); } catch (_e) { /* ignore */ }
    }
  };

  const exampleNDJSON = `{"email":"first.last@example.com","first_name":"First","zip":"83854","opt_in_date":"2026-05-01T14:22:00Z"}`;
  const exampleCurl = apiKey ? `curl -X POST https://projectjarvis.io/api/partner-ingest/v1/records \\
  -H "X-Partner-Key: ${apiKey}" \\
  -H "Content-Type: application/x-ndjson" \\
  --data-binary '${exampleNDJSON}'` : '';

  return (
    <div style={modalBackdrop}>
      <div style={modalBody}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', borderBottom: '1px solid rgba(120,150,200,0.18)', paddingBottom: 12, marginBottom: 20 }}>
          <h3 style={{ margin: 0, color: '#dbeafe' }}>Onboard Data Partner — Step {step} of 4</h3>
          <Stepper step={step} />
        </div>

        {step === 1 && (
          <>
            <h4 style={sectionTitle}>Partner Info</h4>
            <label style={fieldLabel}>Partner name</label>
            <input value={partnerName} onChange={e => setPartnerName(e.target.value)} placeholder="Attribits" style={input} />
            <label style={fieldLabel}>Contact email (optional)</label>
            <input value={contactEmail} onChange={e => setContactEmail(e.target.value)} placeholder="datapartner@attribits.com" style={input} />
            <label style={fieldLabel}>Notes (optional)</label>
            <textarea value={partnerNotes} onChange={e => setPartnerNotes(e.target.value)} rows={3} style={{ ...input, fontFamily: 'inherit' }} />
            {error && <div style={errorBox}><FontAwesomeIcon icon={faExclamationTriangle} /> {error}</div>}
            <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 16 }}>
              <button onClick={onClose} style={ghostBtn}>Cancel</button>
              <button onClick={handleCreatePartner} disabled={submitting} style={primaryBtn}>
                {submitting && <FontAwesomeIcon icon={faSpinner} spin />} Create partner →
              </button>
            </div>
          </>
        )}

        {step === 2 && (
          <>
            <h4 style={sectionTitle}>Add First Dataset</h4>
            <div style={{ marginBottom: 12, fontSize: 12, color: 'rgba(180,210,240,0.7)' }}>
              Each dataset is bound to one vertical. A partner can have multiple datasets (e.g. Attribits HOME → remodel, Attribits PERSONAL LOAN → personal_loans).
            </div>
            <label style={fieldLabel}>Dataset name</label>
            <input value={datasetName} onChange={e => setDatasetName(e.target.value)} placeholder="Attribits HOME" style={input} />
            <label style={fieldLabel}>Vertical</label>
            <select value={vertical} onChange={e => setVertical(e.target.value)} style={input}>
              {VERTICALS.map(v => <option key={v.value} value={v.value}>{v.label}</option>)}
            </select>
            <label style={fieldLabel}>Flush window (hours) — how long before all records have shipped</label>
            <input type="number" min={1} max={168} value={flushHours} onChange={e => setFlushHours(parseInt(e.target.value || '24', 10))} style={input} />
            {error && <div style={errorBox}><FontAwesomeIcon icon={faExclamationTriangle} /> {error}</div>}
            <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 16 }}>
              <button onClick={() => setStep(1)} style={ghostBtn}>← Back</button>
              <button onClick={handleCreateDataset} disabled={submitting} style={primaryBtn}>
                {submitting && <FontAwesomeIcon icon={faSpinner} spin />} Create dataset & generate key →
              </button>
            </div>
          </>
        )}

        {step === 3 && apiKey && (
          <>
            <h4 style={sectionTitle}>API Key Generated</h4>
            <div style={{ background: 'rgba(245,158,11,0.12)', border: '1px solid rgba(245,158,11,0.35)', padding: 12, borderRadius: 6, marginBottom: 16 }}>
              <FontAwesomeIcon icon={faExclamationTriangle} style={{ color: '#f59e0b', marginRight: 8 }} />
              <b>Show this key to the partner ONCE.</b> It cannot be retrieved later. Only the prefix ({apiKeyPrefix}…) is stored.
            </div>
            <label style={fieldLabel}>API key</label>
            <div style={{ display: 'flex', gap: 8 }}>
              <code style={{ ...input, fontFamily: 'ui-monospace, monospace', fontSize: 13, padding: 12 }}>{apiKey}</code>
              <button onClick={copyKey} style={iconBtn}>
                <FontAwesomeIcon icon={faCopy} /> Copy
              </button>
            </div>
            <h4 style={{ ...sectionTitle, marginTop: 24 }}>Example curl</h4>
            <pre style={codeBox}>{exampleCurl}</pre>
            <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 16 }}>
              <button onClick={() => setStep(4)} style={primaryBtn}>I have shared the key →</button>
            </div>
          </>
        )}

        {step === 4 && (
          <>
            <h4 style={sectionTitle}>Done</h4>
            <div style={{ textAlign: 'center', padding: 24, color: '#10b981' }}>
              <FontAwesomeIcon icon={faCheckCircle} size="3x" />
              <div style={{ marginTop: 12, fontSize: 16, color: '#dbeafe' }}>{partnerName} is live.</div>
              <div style={{ marginTop: 8, color: 'rgba(180,210,240,0.7)', fontSize: 13 }}>
                Dataset <b>{datasetName}</b> ({vertical}) is ready to receive records.
                Records will start hitting the cleaning pipeline within 30s of POST.
              </div>
            </div>
            <div style={{ background: 'rgba(15,30,60,0.5)', padding: 16, borderRadius: 6, fontSize: 12, color: 'rgba(180,210,240,0.7)' }}>
              <b>What happens next:</b><br />
              1. Partner POSTs records to <code>/api/partner-ingest/v1/records</code> with their X-Partner-Key.<br />
              2. Records are stored in S3 and queued for slicing.<br />
              3. Slicer drops globally-suppressed records, queues survivors for EmailOversight validation.<br />
              4. EO-Verified + Complainer records become &lsquo;ready&rsquo; in the cleaning queue.<br />
              5. The 15-minute drip orchestrator round-robins them across DB / HT / MH / QF brands.<br />
            </div>
            <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 16 }}>
              <button onClick={onClose} style={primaryBtn}>Close</button>
            </div>
          </>
        )}
      </div>
    </div>
  );
};

const Stepper: React.FC<{ step: Step }> = ({ step }) => (
  <div style={{ display: 'flex', gap: 4 }}>
    {[1, 2, 3, 4].map(n => (
      <div key={n} style={{
        width: 24, height: 4, borderRadius: 2,
        background: n <= step ? '#6366f1' : 'rgba(120,150,200,0.2)',
      }} />
    ))}
  </div>
);

const modalBackdrop: React.CSSProperties = {
  position: 'fixed', top: 0, left: 0, right: 0, bottom: 0,
  background: 'rgba(0,0,0,0.65)', display: 'flex', alignItems: 'center',
  justifyContent: 'center', zIndex: 1000, padding: 24,
};
const modalBody: React.CSSProperties = {
  background: '#0f1c33', border: '1px solid rgba(120,150,200,0.25)',
  borderRadius: 10, padding: 24, width: '100%', maxWidth: 640,
  maxHeight: '90vh', overflowY: 'auto',
  boxShadow: '0 20px 60px rgba(0,0,0,0.6)', color: 'rgba(220,235,250,0.92)',
};
const sectionTitle: React.CSSProperties = { margin: '0 0 12px 0', color: '#cbd5f5', fontSize: 14 };
const fieldLabel: React.CSSProperties = {
  display: 'block', fontSize: 12, color: 'rgba(180,210,240,0.7)', margin: '12px 0 4px 0',
};
const input: React.CSSProperties = {
  width: '100%', padding: '8px 10px', background: 'rgba(0,0,0,0.25)',
  border: '1px solid rgba(120,150,200,0.25)', borderRadius: 4, color: 'white', fontSize: 13,
  boxSizing: 'border-box',
};
const codeBox: React.CSSProperties = {
  background: 'rgba(0,0,0,0.45)', padding: 12, borderRadius: 4, fontSize: 12,
  fontFamily: 'ui-monospace, monospace', whiteSpace: 'pre-wrap', wordBreak: 'break-all',
  color: '#a5f3fc',
};
const errorBox: React.CSSProperties = {
  background: 'rgba(239,68,68,0.18)', border: '1px solid rgba(239,68,68,0.4)',
  padding: 8, borderRadius: 4, marginTop: 12, fontSize: 12, color: '#fca5a5',
};
const primaryBtn: React.CSSProperties = {
  background: 'linear-gradient(135deg, #6366f1 0%, #8b5cf6 100%)',
  color: 'white', border: 'none', padding: '8px 14px', borderRadius: 6, cursor: 'pointer',
  fontWeight: 600, display: 'inline-flex', alignItems: 'center', gap: 8,
};
const ghostBtn: React.CSSProperties = {
  background: 'transparent', color: 'rgba(220,235,250,0.85)',
  border: '1px solid rgba(120,150,200,0.25)', padding: '8px 14px', borderRadius: 6,
  cursor: 'pointer',
};
const iconBtn: React.CSSProperties = {
  background: 'rgba(120,150,200,0.08)', color: 'rgba(220,235,250,0.85)',
  border: '1px solid rgba(120,150,200,0.18)', padding: '8px 14px', borderRadius: 6,
  cursor: 'pointer',
};
