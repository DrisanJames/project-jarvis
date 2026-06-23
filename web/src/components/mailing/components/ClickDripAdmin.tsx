import React, { useState, useEffect, useCallback, useMemo, useRef } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faHandPointer, faToggleOn, faToggleOff, faPen, faRotateLeft,
  faCheck, faTimes, faClock, faSync, faSpinner, faExclamationTriangle,
  faPlus, faFloppyDisk,
} from '@fortawesome/free-solid-svg-icons';
import { useAuth } from '../../../contexts/AuthContext';
import { useToast } from '../shared/ToastSystem';

// ============================================================================
// CLICK-DRIP ADMIN
//
// Admin surface for the click-triggered drip campaign feature. Two panels:
//   1. Top: Offer → Journey map (which offer wires to which click journey,
//      and the master enable/disable safety switch per offer).
//   2. Bottom: Reminder subjects for the selected offer at sequence_index
//      0..3 (corresponding to +1h, +6h, +24h, +72h reminders).
//
// Backend contract (built by sister task — design assumes these exist):
//   GET    /api/mailing/offer-journey-map
//   PUT    /api/mailing/offer-journey-map/{everflow_offer_id}
//   GET    /api/mailing/offer-reminder-subjects?everflow_offer_id={id}
//   PUT    /api/mailing/offer-reminder-subjects/{everflow_offer_id}/{sequence_index}
//   DELETE /api/mailing/offer-reminder-subjects/{everflow_offer_id}/{sequence_index}
// ============================================================================

export const CLICK_DRIP_ADMIN_VERSION = '0.1';

const API_BASE = '/api/mailing';

// Sequence indices map to fixed reminder offsets in the click-drip cadence.
// Higher indices (>3) fall back to a generic "Step N" label so an operator
// can still see — and reset — drift rows if the backend ever ships them.
const SEQUENCE_LABELS: Record<number, string> = {
  0: '+1h',
  1: '+6h',
  2: '+24h',
  3: '+72h',
};

const REQUIRED_SEQUENCE_INDICES = [0, 1, 2, 3] as const;
const SUBJECT_MAX_LEN = 200;
const PREHEADER_MAX_LEN = 200;
const FROM_NAME_MAX_LEN = 120;
const NOTES_MAX_LEN = 500;

// Per project rule: payout-type badges have a fixed palette so the operator
// can scan a long list and pick out the high-stakes (CPA/CPL) offers fast.
type PayoutType = 'CPM' | 'eCPM' | 'CPA' | 'CPL' | 'CPC' | 'IO' | 'PRV' | 'UNKNOWN';
const PAYOUT_TYPES: PayoutType[] = ['CPM', 'eCPM', 'CPA', 'CPL', 'CPC', 'IO', 'PRV', 'UNKNOWN'];

const PAYOUT_COLORS: Record<PayoutType, { bg: string; border: string; text: string }> = {
  CPM:     { bg: 'rgba(99, 102, 241, 0.15)',  border: 'rgba(99, 102, 241, 0.5)',  text: '#a5b4fc' },
  eCPM:    { bg: 'rgba(168, 85, 247, 0.15)',  border: 'rgba(168, 85, 247, 0.5)',  text: '#d8b4fe' },
  CPA:     { bg: 'rgba(34, 197, 94, 0.15)',   border: 'rgba(34, 197, 94, 0.5)',   text: '#86efac' },
  CPL:     { bg: 'rgba(6, 182, 212, 0.15)',   border: 'rgba(6, 182, 212, 0.5)',   text: '#67e8f9' },
  CPC:     { bg: 'rgba(239, 68, 68, 0.15)',   border: 'rgba(239, 68, 68, 0.5)',   text: '#fca5a5' },
  IO:      { bg: 'rgba(245, 158, 11, 0.15)',  border: 'rgba(245, 158, 11, 0.5)',  text: '#fcd34d' },
  PRV:     { bg: 'rgba(100, 116, 139, 0.15)', border: 'rgba(100, 116, 139, 0.5)', text: '#cbd5e1' },
  UNKNOWN: { bg: 'rgba(75, 85, 99, 0.15)',    border: 'rgba(75, 85, 99, 0.5)',    text: '#9ca3af' },
};

// ============================================================================
// TYPES
// ============================================================================

interface OfferJourneyRow {
  everflow_offer_id: string;
  click_journey_id: string;
  payout_type: PayoutType | string;
  enabled: boolean;
  notes?: string;
  created_at?: string;
  updated_at?: string;
}

interface ReminderSubjectRow {
  everflow_offer_id: string;
  sequence_index: number;
  subject: string;
  preheader?: string;
  from_name_override?: string;
  enabled: boolean;
  notes?: string;
  updated_at?: string;
}

interface OfferJourneyListResponse {
  data?: OfferJourneyRow[];
  error?: string;
}

interface OfferJourneyItemResponse {
  data?: OfferJourneyRow;
  error?: string;
}

interface ReminderSubjectListResponse {
  data?: ReminderSubjectRow[];
  error?: string;
}

interface ReminderSubjectItemResponse {
  data?: ReminderSubjectRow;
  error?: string;
}

// ============================================================================
// HELPERS
// ============================================================================

const getOrgHeaders = (orgId: string | undefined): HeadersInit => {
  const headers: HeadersInit = { 'Content-Type': 'application/json' };
  if (orgId) headers['X-Organization-ID'] = orgId;
  return headers;
};

const orgFetch = (url: string, orgId: string | undefined, init: RequestInit = {}) =>
  fetch(url, {
    ...init,
    headers: { ...getOrgHeaders(orgId), ...(init.headers || {}) },
    credentials: 'include',
  });

// Relative-time formatter — single source of truth for "Updated At" cells.
// Falls back to ISO date when the timestamp is older than ~30 days because
// "27 days ago" is less actionable than the actual date.
const relativeTime = (iso?: string): string => {
  if (!iso) return '—';
  const ts = new Date(iso).getTime();
  if (Number.isNaN(ts)) return '—';
  const diff = Date.now() - ts;
  const sec = Math.floor(diff / 1000);
  if (sec < 5) return 'just now';
  if (sec < 60) return `${sec}s ago`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m ago`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}h ago`;
  const days = Math.floor(hr / 24);
  if (days < 30) return `${days}d ago`;
  return new Date(iso).toLocaleDateString();
};

const sequenceLabel = (idx: number): string => SEQUENCE_LABELS[idx] ?? `Step ${idx}`;

const normalizePayout = (raw: unknown): PayoutType => {
  if (typeof raw !== 'string') return 'UNKNOWN';
  const upper = raw.toUpperCase() as PayoutType;
  return (PAYOUT_TYPES as readonly string[]).includes(upper) ? upper : 'UNKNOWN';
};

const errorMessage = async (resp: Response): Promise<string> => {
  try {
    const body = await resp.json();
    if (body?.error) return String(body.error);
  } catch {
    /* fallthrough */
  }
  return `${resp.status} ${resp.statusText || 'request failed'}`;
};

// ============================================================================
// SUB-COMPONENTS
// ============================================================================

const PayoutBadge: React.FC<{ value: string }> = ({ value }) => {
  const pt = normalizePayout(value);
  const c = PAYOUT_COLORS[pt];
  return (
    <span
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        padding: '3px 10px',
        borderRadius: 6,
        background: c.bg,
        border: `1px solid ${c.border}`,
        color: c.text,
        fontSize: 11,
        fontWeight: 600,
        letterSpacing: 0.3,
        textTransform: 'uppercase',
      }}
    >
      {pt}
    </span>
  );
};

const ToggleSwitch: React.FC<{
  value: boolean;
  pending?: boolean;
  disabled?: boolean;
  onChange: (next: boolean) => void;
  title?: string;
}> = ({ value, pending, disabled, onChange, title }) => (
  <button
    type="button"
    title={title}
    disabled={pending || disabled}
    onClick={() => onChange(!value)}
    style={{
      background: 'none',
      border: 'none',
      cursor: pending || disabled ? 'not-allowed' : 'pointer',
      padding: 0,
      color: value ? '#00b894' : 'rgba(180,210,240,0.4)',
      fontSize: 22,
      lineHeight: 1,
      opacity: pending ? 0.6 : 1,
      transition: 'color 0.15s',
    }}
  >
    {pending ? (
      <FontAwesomeIcon icon={faSpinner} spin style={{ fontSize: 14 }} />
    ) : (
      <FontAwesomeIcon icon={value ? faToggleOn : faToggleOff} />
    )}
  </button>
);

// Inline-editable text input that persists on blur. Tracks a draft string
// alongside a `dirty` flag so it can show a save-in-flight indicator and a
// red validation hint without surrendering focus while the operator types.
const BlurInput: React.FC<{
  value: string;
  placeholder?: string;
  maxLength?: number;
  required?: boolean;
  disabled?: boolean;
  onCommit: (next: string) => Promise<void> | void;
  'data-testid'?: string;
}> = ({ value, placeholder, maxLength, required, disabled, onCommit, ...rest }) => {
  const [draft, setDraft] = useState(value);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Track latest committed value so an external refresh (e.g. another tab
  // updating the row) replaces the draft, but our own typing doesn't get
  // clobbered while the input is focused.
  const focusedRef = useRef(false);

  useEffect(() => {
    if (!focusedRef.current) {
      setDraft(value);
    }
  }, [value]);

  const validate = (s: string): string | null => {
    if (required && s.trim().length === 0) return 'Required';
    if (maxLength && s.length > maxLength) return `Max ${maxLength} characters`;
    return null;
  };

  const handleBlur = async () => {
    focusedRef.current = false;
    const err = validate(draft);
    if (err) {
      setError(err);
      return;
    }
    if (draft === value) {
      setError(null);
      return;
    }
    setSaving(true);
    setError(null);
    try {
      await onCommit(draft);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Save failed');
      setDraft(value); // revert
    } finally {
      setSaving(false);
    }
  };

  return (
    <div style={{ position: 'relative', width: '100%' }}>
      <input
        type="text"
        value={draft}
        placeholder={placeholder}
        disabled={disabled || saving}
        onFocus={() => { focusedRef.current = true; }}
        onChange={(e) => {
          setDraft(e.target.value);
          if (error) setError(validate(e.target.value));
        }}
        onBlur={handleBlur}
        onKeyDown={(e) => {
          if (e.key === 'Enter') (e.target as HTMLInputElement).blur();
          if (e.key === 'Escape') {
            setDraft(value);
            setError(null);
            (e.target as HTMLInputElement).blur();
          }
        }}
        data-testid={rest['data-testid']}
        style={{
          width: '100%',
          padding: '6px 28px 6px 10px',
          background: 'rgba(15,15,30,0.5)',
          border: `1px solid ${error ? 'rgba(239,68,68,0.6)' : 'rgba(99,102,241,0.25)'}`,
          borderRadius: 6,
          color: '#e0e6f0',
          fontSize: 13,
          outline: 'none',
        }}
      />
      {saving && (
        <FontAwesomeIcon
          icon={faSpinner}
          spin
          style={{
            position: 'absolute',
            right: 10,
            top: '50%',
            transform: 'translateY(-50%)',
            color: 'rgba(180,210,240,0.5)',
            fontSize: 11,
          }}
        />
      )}
      {error && (
        <div
          style={{
            position: 'absolute',
            top: '100%',
            left: 0,
            marginTop: 2,
            fontSize: 11,
            color: '#fca5a5',
            display: 'flex',
            alignItems: 'center',
            gap: 4,
          }}
        >
          <FontAwesomeIcon icon={faExclamationTriangle} style={{ fontSize: 9 }} />
          {error}
        </div>
      )}
    </div>
  );
};

// Loading skeleton row used by both tables before initial fetch resolves.
const SkeletonRow: React.FC<{ cols: number }> = ({ cols }) => (
  <tr>
    {Array.from({ length: cols }).map((_, i) => (
      <td key={i} style={{ padding: '12px 10px' }}>
        <div
          style={{
            height: 14,
            background: 'linear-gradient(90deg, rgba(99,102,241,0.06), rgba(99,102,241,0.18), rgba(99,102,241,0.06))',
            backgroundSize: '200% 100%',
            borderRadius: 4,
            animation: 'cd-skeleton 1.4s ease-in-out infinite',
          }}
        />
      </td>
    ))}
  </tr>
);

// ============================================================================
// EDIT MODAL: OFFER-JOURNEY MAP ROW
// ============================================================================

const OfferJourneyEditModal: React.FC<{
  initial: OfferJourneyRow;
  onClose: () => void;
  onSave: (next: Pick<OfferJourneyRow, 'click_journey_id' | 'payout_type' | 'enabled' | 'notes'>) => Promise<void>;
}> = ({ initial, onClose, onSave }) => {
  const [clickJourneyId, setClickJourneyId] = useState(initial.click_journey_id || '');
  const [payoutType, setPayoutType] = useState<PayoutType>(normalizePayout(initial.payout_type));
  const [enabled, setEnabled] = useState(initial.enabled);
  const [notes, setNotes] = useState(initial.notes || '');
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleSave = async () => {
    if (!clickJourneyId.trim()) {
      setError('Follow-Up Sequence ID is required');
      return;
    }
    setSaving(true);
    setError(null);
    try {
      await onSave({
        click_journey_id: clickJourneyId.trim(),
        payout_type: payoutType,
        enabled,
        notes: notes.trim() || undefined,
      });
      onClose();
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Save failed');
    } finally {
      setSaving(false);
    }
  };

  return (
    <div
      onClick={onClose}
      style={{
        position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.6)', zIndex: 9999,
        display: 'flex', alignItems: 'center', justifyContent: 'center',
      }}
    >
      <div
        onClick={(e) => e.stopPropagation()}
        style={{
          background: 'linear-gradient(180deg, rgba(20,20,40,0.98), rgba(15,15,30,0.98))',
          border: '1px solid rgba(99,102,241,0.4)',
          borderRadius: 12,
          padding: 24,
          width: 520,
          maxWidth: '90vw',
          boxShadow: '0 20px 60px rgba(0,0,0,0.5)',
        }}
      >
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 20 }}>
          <h3 style={{ margin: 0, color: '#e0e6f0', fontSize: 16, fontWeight: 600 }}>
            Edit offer follow-up mapping
          </h3>
          <button
            onClick={onClose}
            style={{ background: 'none', border: 'none', color: 'rgba(180,210,240,0.6)', cursor: 'pointer', fontSize: 14 }}
          >
            <FontAwesomeIcon icon={faTimes} />
          </button>
        </div>

        <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
          <div>
            <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.7)', fontWeight: 600, textTransform: 'uppercase', letterSpacing: 0.5 }}>
              Offer ID
            </label>
            <div style={{ marginTop: 4, padding: '8px 12px', background: 'rgba(15,15,30,0.5)', border: '1px solid rgba(99,102,241,0.2)', borderRadius: 6, color: 'rgba(180,210,240,0.6)', fontSize: 13, fontFamily: 'monospace' }}>
              {initial.everflow_offer_id}
            </div>
          </div>

          <div>
            <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.7)', fontWeight: 600, textTransform: 'uppercase', letterSpacing: 0.5 }}>
              Follow-Up Sequence ID *
            </label>
            <input
              type="text"
              value={clickJourneyId}
              onChange={(e) => setClickJourneyId(e.target.value)}
              placeholder="sequence-xxx"
              style={{
                marginTop: 4, width: '100%', padding: '8px 12px',
                background: 'rgba(15,15,30,0.5)', border: '1px solid rgba(99,102,241,0.25)',
                borderRadius: 6, color: '#e0e6f0', fontSize: 13, outline: 'none',
              }}
            />
          </div>

          <div>
            <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.7)', fontWeight: 600, textTransform: 'uppercase', letterSpacing: 0.5 }}>
              Payout Type
            </label>
            <select
              value={payoutType}
              onChange={(e) => setPayoutType(e.target.value as PayoutType)}
              style={{
                marginTop: 4, width: '100%', padding: '8px 12px',
                background: 'rgba(15,15,30,0.5)', border: '1px solid rgba(99,102,241,0.25)',
                borderRadius: 6, color: '#e0e6f0', fontSize: 13, outline: 'none',
              }}
            >
              {PAYOUT_TYPES.map((p) => (
                <option key={p} value={p}>{p}</option>
              ))}
            </select>
          </div>

          <div>
            <label style={{ display: 'flex', alignItems: 'center', gap: 8, cursor: 'pointer', color: '#e0e6f0', fontSize: 13 }}>
              <input
                type="checkbox"
                checked={enabled}
                onChange={(e) => setEnabled(e.target.checked)}
                style={{ width: 16, height: 16, cursor: 'pointer' }}
              />
              Enabled — follow-up enrollments active for this offer
            </label>
          </div>

          <div>
            <label style={{ fontSize: 11, color: 'rgba(180,210,240,0.7)', fontWeight: 600, textTransform: 'uppercase', letterSpacing: 0.5 }}>
              Notes
            </label>
            <textarea
              value={notes}
              onChange={(e) => setNotes(e.target.value.slice(0, NOTES_MAX_LEN))}
              rows={3}
              placeholder="Optional internal notes"
              style={{
                marginTop: 4, width: '100%', padding: '8px 12px',
                background: 'rgba(15,15,30,0.5)', border: '1px solid rgba(99,102,241,0.25)',
                borderRadius: 6, color: '#e0e6f0', fontSize: 13, outline: 'none',
                resize: 'vertical', fontFamily: 'inherit',
              }}
            />
          </div>

          {error && (
            <div style={{ color: '#fca5a5', fontSize: 12, display: 'flex', alignItems: 'center', gap: 6 }}>
              <FontAwesomeIcon icon={faExclamationTriangle} />
              {error}
            </div>
          )}
        </div>

        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 20 }}>
          <button
            onClick={onClose}
            disabled={saving}
            style={{
              padding: '8px 16px', background: 'transparent',
              border: '1px solid rgba(180,210,240,0.2)', borderRadius: 6,
              color: 'rgba(180,210,240,0.8)', fontSize: 13, cursor: saving ? 'not-allowed' : 'pointer',
            }}
          >
            Cancel
          </button>
          <button
            onClick={handleSave}
            disabled={saving}
            style={{
              padding: '8px 16px',
              background: 'linear-gradient(135deg, #6366f1, #a855f7)',
              border: 'none', borderRadius: 6, color: '#fff',
              fontSize: 13, fontWeight: 600, cursor: saving ? 'not-allowed' : 'pointer',
              display: 'flex', alignItems: 'center', gap: 6,
            }}
          >
            {saving ? <FontAwesomeIcon icon={faSpinner} spin /> : <FontAwesomeIcon icon={faFloppyDisk} />}
            Save
          </button>
        </div>
      </div>
    </div>
  );
};

// ============================================================================
// MAIN COMPONENT
// ============================================================================

export const ClickDripAdmin: React.FC = () => {
  const { organization } = useAuth();
  const { addToast } = useToast();
  const orgId = organization?.id;

  // ── Top panel state ────────────────────────────────────────────────────────
  const [journeyMap, setJourneyMap] = useState<OfferJourneyRow[]>([]);
  const [journeyMapLoading, setJourneyMapLoading] = useState(true);
  const [journeyMapError, setJourneyMapError] = useState<string | null>(null);
  const [togglesPending, setTogglesPending] = useState<Set<string>>(new Set());
  const [editingRow, setEditingRow] = useState<OfferJourneyRow | null>(null);
  const [selectedOfferId, setSelectedOfferId] = useState<string | null>(null);

  // ── Bottom panel state ─────────────────────────────────────────────────────
  const [subjects, setSubjects] = useState<ReminderSubjectRow[]>([]);
  const [subjectsLoading, setSubjectsLoading] = useState(false);
  const [subjectsError, setSubjectsError] = useState<string | null>(null);
  const [subjectTogglesPending, setSubjectTogglesPending] = useState<Set<number>>(new Set());

  // ── Fetch helpers ──────────────────────────────────────────────────────────
  const fetchJourneyMap = useCallback(async () => {
    setJourneyMapLoading(true);
    setJourneyMapError(null);
    try {
      const resp = await orgFetch(`${API_BASE}/offer-journey-map`, orgId);
      if (!resp.ok) throw new Error(await errorMessage(resp));
      const body: OfferJourneyListResponse = await resp.json();
      const rows = body.data || [];
      setJourneyMap(rows);
      setSelectedOfferId((prev) => {
        if (prev && rows.some((r) => r.everflow_offer_id === prev)) return prev;
        return rows[0]?.everflow_offer_id ?? null;
      });
    } catch (e) {
      setJourneyMapError(e instanceof Error ? e.message : 'Failed to load offer-journey map');
    } finally {
      setJourneyMapLoading(false);
    }
  }, [orgId]);

  const fetchSubjects = useCallback(async (offerId: string) => {
    setSubjectsLoading(true);
    setSubjectsError(null);
    try {
      const resp = await orgFetch(
        `${API_BASE}/offer-reminder-subjects?everflow_offer_id=${encodeURIComponent(offerId)}`,
        orgId,
      );
      if (!resp.ok) throw new Error(await errorMessage(resp));
      const body: ReminderSubjectListResponse = await resp.json();
      setSubjects(body.data || []);
    } catch (e) {
      setSubjectsError(e instanceof Error ? e.message : 'Failed to load reminder subjects');
      setSubjects([]);
    } finally {
      setSubjectsLoading(false);
    }
  }, [orgId]);

  useEffect(() => { void fetchJourneyMap(); }, [fetchJourneyMap]);

  useEffect(() => {
    if (selectedOfferId) {
      void fetchSubjects(selectedOfferId);
    } else {
      setSubjects([]);
      setSubjectsError(null);
    }
  }, [selectedOfferId, fetchSubjects]);

  // ── Offer-journey-map mutations ────────────────────────────────────────────
  const putJourneyMapRow = useCallback(async (
    offerId: string,
    patch: Partial<Pick<OfferJourneyRow, 'click_journey_id' | 'payout_type' | 'enabled' | 'notes'>>,
  ): Promise<OfferJourneyRow> => {
    const current = journeyMap.find((r) => r.everflow_offer_id === offerId);
    const body = {
      click_journey_id: patch.click_journey_id ?? current?.click_journey_id ?? '',
      payout_type:      patch.payout_type      ?? current?.payout_type      ?? 'UNKNOWN',
      enabled:          patch.enabled          ?? current?.enabled          ?? false,
      notes:            patch.notes            ?? current?.notes            ?? '',
    };
    const resp = await orgFetch(
      `${API_BASE}/offer-journey-map/${encodeURIComponent(offerId)}`,
      orgId,
      { method: 'PUT', body: JSON.stringify(body) },
    );
    if (!resp.ok) throw new Error(await errorMessage(resp));
    const parsed: OfferJourneyItemResponse = await resp.json();
    if (!parsed.data) throw new Error('Server returned no data');
    return parsed.data;
  }, [journeyMap, orgId]);

  const handleToggleJourneyMap = useCallback(async (row: OfferJourneyRow) => {
    if (togglesPending.has(row.everflow_offer_id)) return;
    // Optimistic flip — revert on error so the operator can see the switch
    // bouncing back if the safety toggle didn't actually take effect.
    const previous = row.enabled;
    setJourneyMap((rows) =>
      rows.map((r) => (r.everflow_offer_id === row.everflow_offer_id ? { ...r, enabled: !previous } : r)),
    );
    setTogglesPending((s) => new Set(s).add(row.everflow_offer_id));
    try {
      const updated = await putJourneyMapRow(row.everflow_offer_id, { enabled: !previous });
      setJourneyMap((rows) =>
        rows.map((r) => (r.everflow_offer_id === row.everflow_offer_id ? updated : r)),
      );
      addToast({
        type: 'success',
        title: !previous ? 'Follow-ups enabled' : 'Follow-ups disabled',
        message: `Offer ${row.everflow_offer_id}`,
      });
    } catch (e) {
      setJourneyMap((rows) =>
        rows.map((r) => (r.everflow_offer_id === row.everflow_offer_id ? { ...r, enabled: previous } : r)),
      );
      addToast({
        type: 'error',
        title: 'Failed to save',
        message: e instanceof Error ? e.message : 'Unknown error',
      });
    } finally {
      setTogglesPending((s) => {
        const next = new Set(s);
        next.delete(row.everflow_offer_id);
        return next;
      });
    }
  }, [togglesPending, putJourneyMapRow, addToast]);

  const handleSaveJourneyMapEdit = useCallback(async (
    offerId: string,
    patch: Pick<OfferJourneyRow, 'click_journey_id' | 'payout_type' | 'enabled' | 'notes'>,
  ) => {
    const updated = await putJourneyMapRow(offerId, patch);
    setJourneyMap((rows) =>
      rows.map((r) => (r.everflow_offer_id === offerId ? updated : r)),
    );
    addToast({
      type: 'success',
      title: 'Mapping saved',
      message: `Offer ${offerId} → ${updated.click_journey_id}`,
    });
  }, [putJourneyMapRow, addToast]);

  // ── Reminder-subject mutations ─────────────────────────────────────────────
  const putReminder = useCallback(async (
    offerId: string,
    seq: number,
    body: Pick<ReminderSubjectRow, 'subject' | 'preheader' | 'from_name_override' | 'enabled' | 'notes'>,
  ): Promise<ReminderSubjectRow> => {
    const resp = await orgFetch(
      `${API_BASE}/offer-reminder-subjects/${encodeURIComponent(offerId)}/${seq}`,
      orgId,
      { method: 'PUT', body: JSON.stringify(body) },
    );
    if (!resp.ok) throw new Error(await errorMessage(resp));
    const parsed: ReminderSubjectItemResponse = await resp.json();
    if (!parsed.data) throw new Error('Server returned no data');
    return parsed.data;
  }, [orgId]);

  const upsertReminderField = useCallback(async (
    seq: number,
    field: 'subject' | 'preheader' | 'from_name_override' | 'notes',
    value: string,
  ) => {
    if (!selectedOfferId) return;
    const existing = subjects.find((s) => s.sequence_index === seq);
    const body = {
      subject:            field === 'subject'            ? value : (existing?.subject ?? ''),
      preheader:          field === 'preheader'          ? value : (existing?.preheader ?? ''),
      from_name_override: field === 'from_name_override' ? value : (existing?.from_name_override ?? ''),
      enabled:            existing?.enabled ?? true,
      notes:              field === 'notes'              ? value : (existing?.notes ?? ''),
    };
    if (!body.subject.trim()) {
      throw new Error('Subject is required');
    }
    const updated = await putReminder(selectedOfferId, seq, body);
    setSubjects((rows) => {
      const found = rows.some((r) => r.sequence_index === seq);
      return found
        ? rows.map((r) => (r.sequence_index === seq ? updated : r))
        : [...rows, updated].sort((a, b) => a.sequence_index - b.sequence_index);
    });
    addToast({
      type: 'success',
      title: `${field === 'from_name_override' ? 'From name' : field.charAt(0).toUpperCase() + field.slice(1)} updated`,
      message: `${sequenceLabel(seq)} reminder`,
    });
  }, [selectedOfferId, subjects, putReminder, addToast]);

  const handleToggleReminder = useCallback(async (seq: number) => {
    if (!selectedOfferId) return;
    if (subjectTogglesPending.has(seq)) return;
    const existing = subjects.find((s) => s.sequence_index === seq);
    if (!existing) return;
    const previous = existing.enabled;
    setSubjects((rows) =>
      rows.map((r) => (r.sequence_index === seq ? { ...r, enabled: !previous } : r)),
    );
    setSubjectTogglesPending((s) => new Set(s).add(seq));
    try {
      const updated = await putReminder(selectedOfferId, seq, {
        subject:            existing.subject,
        preheader:          existing.preheader ?? '',
        from_name_override: existing.from_name_override ?? '',
        enabled:            !previous,
        notes:              existing.notes ?? '',
      });
      setSubjects((rows) =>
        rows.map((r) => (r.sequence_index === seq ? updated : r)),
      );
      addToast({
        type: 'success',
        title: !previous ? 'Reminder enabled' : 'Reminder disabled',
        message: `${sequenceLabel(seq)} for offer ${selectedOfferId}`,
      });
    } catch (e) {
      setSubjects((rows) =>
        rows.map((r) => (r.sequence_index === seq ? { ...r, enabled: previous } : r)),
      );
      addToast({
        type: 'error',
        title: 'Failed to toggle',
        message: e instanceof Error ? e.message : 'Unknown error',
      });
    } finally {
      setSubjectTogglesPending((s) => {
        const next = new Set(s);
        next.delete(seq);
        return next;
      });
    }
  }, [selectedOfferId, subjects, subjectTogglesPending, putReminder, addToast]);

  const handleAddReminder = useCallback(async (seq: number) => {
    if (!selectedOfferId) return;
    try {
      const updated = await putReminder(selectedOfferId, seq, {
        subject: `Reminder ${sequenceLabel(seq)} — edit me`,
        preheader: '',
        from_name_override: '',
        enabled: false,
        notes: '',
      });
      setSubjects((rows) =>
        [...rows, updated].sort((a, b) => a.sequence_index - b.sequence_index),
      );
      addToast({
        type: 'success',
        title: 'Reminder added',
        message: `${sequenceLabel(seq)} — disabled until subject is edited`,
      });
    } catch (e) {
      addToast({
        type: 'error',
        title: 'Failed to add reminder',
        message: e instanceof Error ? e.message : 'Unknown error',
      });
    }
  }, [selectedOfferId, putReminder, addToast]);

  const handleResetReminder = useCallback(async (seq: number) => {
    if (!selectedOfferId) return;
    if (!window.confirm(`Reset ${sequenceLabel(seq)} reminder for offer ${selectedOfferId}? This deletes the row.`)) {
      return;
    }
    try {
      const resp = await orgFetch(
        `${API_BASE}/offer-reminder-subjects/${encodeURIComponent(selectedOfferId)}/${seq}`,
        orgId,
        { method: 'DELETE' },
      );
      if (!resp.ok) throw new Error(await errorMessage(resp));
      setSubjects((rows) => rows.filter((r) => r.sequence_index !== seq));
      addToast({
        type: 'success',
        title: 'Reminder reset',
        message: `${sequenceLabel(seq)} deleted — system default will be used`,
      });
    } catch (e) {
      addToast({
        type: 'error',
        title: 'Failed to reset',
        message: e instanceof Error ? e.message : 'Unknown error',
      });
    }
  }, [selectedOfferId, orgId, addToast]);

  // Sequence indices we'll surface in the bottom table. Union of the four
  // required indices + any extras the backend returned (so drift rows from
  // future expansions are visible and resettable).
  const renderedSequenceIndices = useMemo(() => {
    const set = new Set<number>(REQUIRED_SEQUENCE_INDICES);
    for (const s of subjects) set.add(s.sequence_index);
    return [...set].sort((a, b) => a - b);
  }, [subjects]);

  // ────────────────────────────────────────────────────────────────────────────
  return (
    <div style={{ padding: 24, maxWidth: 1400, margin: '0 auto', color: '#e0e6f0' }}>
      <style>{`
        @keyframes cd-skeleton {
          0% { background-position: 200% 0; }
          100% { background-position: -200% 0; }
        }
      `}</style>

      <div style={{ marginBottom: 20 }}>
        <h2 style={{
          margin: 0, fontSize: 20, fontWeight: 700,
          display: 'flex', alignItems: 'center', gap: 10,
          background: 'linear-gradient(135deg, #a5b4fc, #d8b4fe, #f9a8d4)',
          WebkitBackgroundClip: 'text', WebkitTextFillColor: 'transparent',
        }}>
          <FontAwesomeIcon icon={faHandPointer} style={{ color: '#a5b4fc' }} />
          Automated Follow-Ups Admin
        </h2>
        <p style={{ margin: '6px 0 0', color: 'rgba(180,210,240,0.6)', fontSize: 13 }}>
          Wire offers to click-triggered follow-up sequences. Toggling{' '}
          <strong style={{ color: '#86efac' }}>Enabled</strong> stops new enrollments immediately.{' '}
          <span style={{ color: 'rgba(180,210,240,0.45)' }}>v{CLICK_DRIP_ADMIN_VERSION}</span>
        </p>
      </div>

      {/* ── TOP: OFFER-JOURNEY MAP ─────────────────────────────────────────── */}
      <section
        style={{
          background: 'rgba(15,15,30,0.4)',
          border: '1px solid rgba(99,102,241,0.2)',
          borderRadius: 12, padding: 18, marginBottom: 24,
        }}
      >
        <header style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 }}>
          <div>
            <h3 style={{ margin: 0, fontSize: 14, fontWeight: 600, color: '#e0e6f0', display: 'flex', alignItems: 'center', gap: 8 }}>
              Offer Follow-Up Map
              <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)', fontWeight: 400 }}>
                {journeyMap.length} {journeyMap.length === 1 ? 'mapping' : 'mappings'}
              </span>
            </h3>
            <p style={{ margin: '4px 0 0', fontSize: 12, color: 'rgba(180,210,240,0.55)' }}>
              Click an offer row to load its reminder subjects below.
            </p>
          </div>
          <button
            onClick={() => fetchJourneyMap()}
            disabled={journeyMapLoading}
            style={{
              padding: '6px 12px', background: 'rgba(99,102,241,0.15)',
              border: '1px solid rgba(99,102,241,0.35)', borderRadius: 6,
              color: '#a5b4fc', fontSize: 12, cursor: journeyMapLoading ? 'not-allowed' : 'pointer',
              display: 'flex', alignItems: 'center', gap: 6,
            }}
          >
            <FontAwesomeIcon icon={faSync} spin={journeyMapLoading} /> Refresh
          </button>
        </header>

        {journeyMapError && (
          <div style={{
            padding: 10, marginBottom: 12, background: 'rgba(239,68,68,0.1)',
            border: '1px solid rgba(239,68,68,0.4)', borderRadius: 6,
            color: '#fca5a5', fontSize: 12, display: 'flex', alignItems: 'center', gap: 8,
          }}>
            <FontAwesomeIcon icon={faExclamationTriangle} />
            {journeyMapError}
          </div>
        )}

        <div style={{ overflowX: 'auto' }}>
          <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
            <thead>
              <tr style={{ borderBottom: '1px solid rgba(99,102,241,0.2)' }}>
                <th style={thStyle}>Offer ID</th>
                <th style={thStyle}>Follow-Up Sequence ID</th>
                <th style={thStyle}>Payout</th>
                <th style={{ ...thStyle, width: 80 }}>Enabled</th>
                <th style={thStyle}>Notes</th>
                <th style={{ ...thStyle, width: 110 }}>Updated</th>
                <th style={{ ...thStyle, width: 80 }}>Actions</th>
              </tr>
            </thead>
            <tbody>
              {journeyMapLoading && journeyMap.length === 0 ? (
                <>
                  <SkeletonRow cols={7} />
                  <SkeletonRow cols={7} />
                  <SkeletonRow cols={7} />
                </>
              ) : journeyMap.length === 0 ? (
                <tr>
                  <td colSpan={7} style={{ textAlign: 'center', padding: 30, color: 'rgba(180,210,240,0.45)' }}>
                    No offer follow-up mappings yet.
                  </td>
                </tr>
              ) : (
                journeyMap.map((row) => {
                  const isSelected = row.everflow_offer_id === selectedOfferId;
                  return (
                    <tr
                      key={row.everflow_offer_id}
                      onClick={() => setSelectedOfferId(row.everflow_offer_id)}
                      style={{
                        borderBottom: '1px solid rgba(99,102,241,0.08)',
                        background: isSelected ? 'rgba(99,102,241,0.10)' : 'transparent',
                        cursor: 'pointer',
                        transition: 'background 0.15s',
                      }}
                    >
                      <td style={tdStyle}>
                        <code style={{ color: isSelected ? '#a5b4fc' : '#e0e6f0', fontSize: 12 }}>
                          {row.everflow_offer_id}
                        </code>
                      </td>
                      <td style={tdStyle}>
                        <code style={{
                          padding: '2px 8px',
                          background: 'rgba(99,102,241,0.1)',
                          border: '1px solid rgba(99,102,241,0.3)',
                          borderRadius: 4, fontSize: 11, color: '#c7d2fe',
                        }}>
                          {row.click_journey_id || '—'}
                        </code>
                      </td>
                      <td style={tdStyle}>
                        <PayoutBadge value={String(row.payout_type)} />
                      </td>
                      <td style={tdStyle} onClick={(e) => e.stopPropagation()}>
                        <ToggleSwitch
                          value={row.enabled}
                          pending={togglesPending.has(row.everflow_offer_id)}
                          onChange={() => handleToggleJourneyMap(row)}
                          title={row.enabled ? 'Disable follow-ups for this offer' : 'Enable follow-ups for this offer'}
                        />
                      </td>
                      <td style={{ ...tdStyle, maxWidth: 220 }}>
                        <span
                          title={row.notes || ''}
                          style={{
                            display: 'block', whiteSpace: 'nowrap', overflow: 'hidden',
                            textOverflow: 'ellipsis', color: 'rgba(180,210,240,0.7)', fontSize: 12,
                          }}
                        >
                          {row.notes || '—'}
                        </span>
                      </td>
                      <td style={{ ...tdStyle, color: 'rgba(180,210,240,0.6)', fontSize: 11 }}>
                        <FontAwesomeIcon icon={faClock} style={{ marginRight: 4, fontSize: 9 }} />
                        {relativeTime(row.updated_at)}
                      </td>
                      <td style={tdStyle} onClick={(e) => e.stopPropagation()}>
                        <button
                          onClick={() => setEditingRow(row)}
                          style={{
                            padding: '4px 10px',
                            background: 'rgba(99,102,241,0.15)',
                            border: '1px solid rgba(99,102,241,0.3)',
                            borderRadius: 4, color: '#a5b4fc', fontSize: 11,
                            cursor: 'pointer', display: 'inline-flex', alignItems: 'center', gap: 4,
                          }}
                        >
                          <FontAwesomeIcon icon={faPen} /> Edit
                        </button>
                      </td>
                    </tr>
                  );
                })
              )}
            </tbody>
          </table>
        </div>
      </section>

      {/* ── BOTTOM: REMINDER SUBJECTS ───────────────────────────────────────── */}
      <section
        style={{
          background: 'rgba(15,15,30,0.4)',
          border: '1px solid rgba(168,85,247,0.2)',
          borderRadius: 12, padding: 18,
          opacity: selectedOfferId ? 1 : 0.45,
          pointerEvents: selectedOfferId ? 'auto' : 'none',
          transition: 'opacity 0.15s',
        }}
      >
        <header style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 }}>
          <div>
            <h3 style={{ margin: 0, fontSize: 14, fontWeight: 600, color: '#e0e6f0' }}>
              Reminder Subjects
              {selectedOfferId && (
                <span style={{ marginLeft: 8, fontSize: 11, color: 'rgba(180,210,240,0.5)', fontWeight: 400 }}>
                  for offer <code style={{ color: '#a5b4fc' }}>{selectedOfferId}</code>
                </span>
              )}
            </h3>
            <p style={{ margin: '4px 0 0', fontSize: 12, color: 'rgba(180,210,240,0.55)' }}>
              {selectedOfferId
                ? 'Edits persist on blur. Subject is required. Reset deletes the row so the system default takes over.'
                : 'Select an offer above to manage its reminder subjects.'}
            </p>
          </div>
          {selectedOfferId && (
            <button
              onClick={() => fetchSubjects(selectedOfferId)}
              disabled={subjectsLoading}
              style={{
                padding: '6px 12px', background: 'rgba(168,85,247,0.15)',
                border: '1px solid rgba(168,85,247,0.35)', borderRadius: 6,
                color: '#d8b4fe', fontSize: 12, cursor: subjectsLoading ? 'not-allowed' : 'pointer',
                display: 'flex', alignItems: 'center', gap: 6,
              }}
            >
              <FontAwesomeIcon icon={faSync} spin={subjectsLoading} /> Refresh
            </button>
          )}
        </header>

        {subjectsError && (
          <div style={{
            padding: 10, marginBottom: 12, background: 'rgba(239,68,68,0.1)',
            border: '1px solid rgba(239,68,68,0.4)', borderRadius: 6,
            color: '#fca5a5', fontSize: 12, display: 'flex', alignItems: 'center', gap: 8,
          }}>
            <FontAwesomeIcon icon={faExclamationTriangle} />
            {subjectsError}
          </div>
        )}

        <div style={{ overflowX: 'auto' }}>
          <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
            <thead>
              <tr style={{ borderBottom: '1px solid rgba(168,85,247,0.2)' }}>
                <th style={{ ...thStyle, width: 70 }}>Step</th>
                <th style={thStyle}>Subject *</th>
                <th style={thStyle}>Preheader</th>
                <th style={thStyle}>From Name Override</th>
                <th style={{ ...thStyle, width: 80 }}>Enabled</th>
                <th style={{ ...thStyle, width: 110 }}>Updated</th>
                <th style={{ ...thStyle, width: 100 }}>Action</th>
              </tr>
            </thead>
            <tbody>
              {subjectsLoading && subjects.length === 0 ? (
                <>
                  <SkeletonRow cols={7} />
                  <SkeletonRow cols={7} />
                  <SkeletonRow cols={7} />
                  <SkeletonRow cols={7} />
                </>
              ) : (
                renderedSequenceIndices.map((seq) => {
                  const row = subjects.find((s) => s.sequence_index === seq);
                  const togglePending = subjectTogglesPending.has(seq);
                  return (
                    <tr
                      key={seq}
                      style={{ borderBottom: '1px solid rgba(168,85,247,0.08)' }}
                    >
                      <td style={tdStyle}>
                        <span
                          style={{
                            display: 'inline-block', padding: '3px 10px',
                            background: 'rgba(168,85,247,0.15)',
                            border: '1px solid rgba(168,85,247,0.4)',
                            borderRadius: 4, fontSize: 11, color: '#d8b4fe', fontWeight: 600,
                          }}
                        >
                          {sequenceLabel(seq)}
                        </span>
                      </td>

                      {row ? (
                        <>
                          <td style={{ ...tdStyle, minWidth: 220 }}>
                            <BlurInput
                              value={row.subject || ''}
                              placeholder="Subject line (required)"
                              maxLength={SUBJECT_MAX_LEN}
                              required
                              disabled={!selectedOfferId}
                              onCommit={(v) => upsertReminderField(seq, 'subject', v)}
                            />
                          </td>
                          <td style={{ ...tdStyle, minWidth: 180 }}>
                            <BlurInput
                              value={row.preheader || ''}
                              placeholder="Preheader (optional)"
                              maxLength={PREHEADER_MAX_LEN}
                              disabled={!selectedOfferId}
                              onCommit={(v) => upsertReminderField(seq, 'preheader', v)}
                            />
                          </td>
                          <td style={{ ...tdStyle, minWidth: 160 }}>
                            <BlurInput
                              value={row.from_name_override || ''}
                              placeholder="(use journey default)"
                              maxLength={FROM_NAME_MAX_LEN}
                              disabled={!selectedOfferId}
                              onCommit={(v) => upsertReminderField(seq, 'from_name_override', v)}
                            />
                          </td>
                          <td style={tdStyle}>
                            <ToggleSwitch
                              value={row.enabled}
                              pending={togglePending}
                              onChange={() => handleToggleReminder(seq)}
                              title={row.enabled ? 'Disable this reminder' : 'Enable this reminder'}
                            />
                          </td>
                          <td style={{ ...tdStyle, color: 'rgba(180,210,240,0.6)', fontSize: 11 }}>
                            <FontAwesomeIcon icon={faClock} style={{ marginRight: 4, fontSize: 9 }} />
                            {relativeTime(row.updated_at)}
                          </td>
                          <td style={tdStyle}>
                            <button
                              onClick={() => handleResetReminder(seq)}
                              style={{
                                padding: '4px 10px',
                                background: 'rgba(239,68,68,0.1)',
                                border: '1px solid rgba(239,68,68,0.3)',
                                borderRadius: 4, color: '#fca5a5', fontSize: 11,
                                cursor: 'pointer', display: 'inline-flex', alignItems: 'center', gap: 4,
                              }}
                            >
                              <FontAwesomeIcon icon={faRotateLeft} /> Reset
                            </button>
                          </td>
                        </>
                      ) : (
                        <td colSpan={6} style={{ ...tdStyle, color: 'rgba(180,210,240,0.5)' }}>
                          <span style={{ marginRight: 12 }}>No custom subject — system default in use.</span>
                          <button
                            onClick={() => handleAddReminder(seq)}
                            disabled={!selectedOfferId}
                            style={{
                              padding: '4px 10px',
                              background: 'rgba(34,197,94,0.1)',
                              border: '1px solid rgba(34,197,94,0.35)',
                              borderRadius: 4, color: '#86efac', fontSize: 11,
                              cursor: selectedOfferId ? 'pointer' : 'not-allowed',
                              display: 'inline-flex', alignItems: 'center', gap: 4,
                            }}
                          >
                            <FontAwesomeIcon icon={faPlus} /> Add reminder
                          </button>
                        </td>
                      )}
                    </tr>
                  );
                })
              )}
            </tbody>
          </table>
        </div>

        {selectedOfferId && !subjectsLoading && subjects.length > 0 && (
          <div
            style={{
              marginTop: 14, padding: '8px 12px',
              background: 'rgba(34,197,94,0.08)',
              border: '1px solid rgba(34,197,94,0.2)',
              borderRadius: 6, fontSize: 11, color: 'rgba(180,210,240,0.7)',
              display: 'flex', alignItems: 'center', gap: 8,
            }}
          >
            <FontAwesomeIcon icon={faCheck} style={{ color: '#86efac' }} />
            {subjects.filter((s) => s.enabled).length} of {subjects.length}{' '}
            reminder{subjects.length === 1 ? '' : 's'} enabled.
          </div>
        )}
      </section>

      {editingRow && (
        <OfferJourneyEditModal
          initial={editingRow}
          onClose={() => setEditingRow(null)}
          onSave={(patch) => handleSaveJourneyMapEdit(editingRow.everflow_offer_id, patch)}
        />
      )}
    </div>
  );
};

const thStyle: React.CSSProperties = {
  textAlign: 'left',
  padding: '10px 10px',
  fontSize: 11,
  fontWeight: 600,
  textTransform: 'uppercase',
  letterSpacing: 0.5,
  color: 'rgba(180,210,240,0.6)',
};

const tdStyle: React.CSSProperties = {
  padding: '10px 10px',
  verticalAlign: 'middle',
};

export default ClickDripAdmin;
