import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faSpinner,
  faPlus,
  faTrash,
  faPause,
  faPlay,
  faExternalLinkAlt,
  faExclamationTriangle,
  faCheckCircle,
  faLink,
} from '@fortawesome/free-solid-svg-icons';
import apiFetch from '../shared/apiFetch';
import { colors, thStyle, tdStyle, tableStyle } from '../shared/theme';
import { SectionError, EmptyState, Pill } from '../shared/ui';
import { useToast } from '../shared/ToastSystem';

// Smart Link Gateway admin panel (Phase 1, 2026-05-27; restored 2026-07-22).
//
// Owners: operator. This panel is the canonical surface for managing
// mailing_smart_links rows. It talks to /api/mailing/smartlinks (admin
// CRUD) and surfaces both active and paused/deleted history so the
// operator can audit changes.
//
// Backed by upside-down/internal/api/smartlink_handlers.go. Any field
// added on the server-side struct MUST be added to SmartLink below or
// it will be silently dropped from the UI.

const API_BASE = '/api/mailing/smartlinks';

// 1.1: restored from may-29 stash + apiFetch/design-system modernization + risk_profile.
const PAGE_VERSION = '1.1';

// Keep in sync with internal/pkg/brand/brand.go OwnedDomains. The drop-
// down lists the apex domains the operator can route through the gateway.
const BRAND_OPTIONS: ReadonlyArray<string> = [
  'discountblog.com',
  'quizfiesta.com',
  'historythinking.com',
  'myownhealth.net',
  'getmecoupons.net',
  'businessweeklypro.com',
  'financialcalculate.com',
  'consumerpro.net',
  'homewarrantyservices.org',
  'refinanceratesusa.com',
  'thingoftheday.org',
  'yourinsurancehub.com',
  'myrepairdiy.com',
  'casainsure.com',
  'learnpersonalloans.com',
  'ratesbazar.com',
  'warrantyforyou.com',
];

interface SmartLink {
  id: string;
  brand_root: string;
  slug: string;
  review_slug: string;
  offer_url_template: string;
  status: 'active' | 'paused' | 'deleted';
  risk_profile: 'low' | 'high';
  notes?: string;
  created_at: string;
  updated_at: string;
}

interface ApiListResponse {
  api_version: string;
  smart_links: SmartLink[];
  total: number;
  brand_filter: string;
}

// statusColor maps onto the shared semantic tokens (active = success,
// paused = warning, deleted = faint/neutral).
function statusColor(s: SmartLink['status']): string {
  if (s === 'active') return colors.success;
  if (s === 'paused') return colors.warning;
  return colors.textFaint;
}

// risk_profile → color: high = bridge page on brand site (warning-tinted so
// it stands out); low = direct redirect handoff (neutral/muted).
function riskColor(r: SmartLink['risk_profile']): string {
  return r === 'high' ? colors.warning : colors.textMuted;
}

export const SmartLinkManager: React.FC = () => {
  const { addToast } = useToast();
  const [links, setLinks] = useState<SmartLink[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [brandFilter, setBrandFilter] = useState<string>('');
  const [includeDeleted, setIncludeDeleted] = useState(false);
  const [showCreate, setShowCreate] = useState(false);
  // Per design-system §1.6: every data panel shows its fetch timing.
  const [fetchedAt, setFetchedAt] = useState<string>('');

  // Form state for the create dialog. Kept simple — no react-hook-form,
  // we just submit on click.
  const [form, setForm] = useState({
    brand_root: 'discountblog.com',
    slug: '',
    review_slug: '',
    offer_url_template: '',
    risk_profile: 'low' as 'low' | 'high',
    notes: '',
  });
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);

  const fetchLinks = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const qs = new URLSearchParams();
      if (brandFilter) qs.set('brand_root', brandFilter);
      if (includeDeleted) qs.set('include_deleted', '1');
      // Trailing slash on the collection path is intentional — the chi mount
      // for /api/mailing/smartlinks expects it on list/create.
      const res = await apiFetch(`${API_BASE}/?${qs.toString()}`);
      if (!res.ok) {
        throw new Error(`HTTP ${res.status} ${res.statusText}`);
      }
      const body = (await res.json()) as ApiListResponse;
      setLinks(body.smart_links || []);
      setFetchedAt(new Date().toLocaleTimeString());
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, [brandFilter, includeDeleted]);

  useEffect(() => {
    fetchLinks();
  }, [fetchLinks]);

  const handleCreate = useCallback(async () => {
    setSubmitting(true);
    setSubmitError(null);
    try {
      const res = await apiFetch(`${API_BASE}/`, {
        method: 'POST',
        body: JSON.stringify(form),
      });
      if (!res.ok) {
        const txt = await res.text();
        throw new Error(`HTTP ${res.status}: ${txt}`);
      }
      setShowCreate(false);
      setForm({
        brand_root: 'discountblog.com',
        slug: '',
        review_slug: '',
        offer_url_template: '',
        risk_profile: 'low',
        notes: '',
      });
      await fetchLinks();
    } catch (e) {
      setSubmitError(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmitting(false);
    }
  }, [form, fetchLinks]);

  // Generalized row PATCH — status pause/resume and risk_profile changes both
  // flow through here so every mutation reconciles via fetchLinks().
  const handlePatch = useCallback(
    async (link: SmartLink, patch: Partial<Pick<SmartLink, 'status' | 'risk_profile'>>) => {
      try {
        const res = await apiFetch(`${API_BASE}/${link.id}`, {
          method: 'PATCH',
          body: JSON.stringify(patch),
        });
        if (!res.ok) {
          throw new Error(`HTTP ${res.status}`);
        }
        await fetchLinks();
      } catch (e) {
        addToast({
          type: 'error',
          title: 'Smart link update failed',
          message: `${link.brand_root}/${link.slug}: ${e instanceof Error ? e.message : String(e)}`,
        });
      }
    },
    [fetchLinks, addToast],
  );

  const handleDelete = useCallback(
    async (link: SmartLink) => {
      if (!confirm(`Delete smart link ${link.brand_root}/${link.slug}? It will be soft-deleted (status=deleted).`)) {
        return;
      }
      try {
        const res = await apiFetch(`${API_BASE}/${link.id}`, {
          method: 'DELETE',
        });
        if (!res.ok) {
          throw new Error(`HTTP ${res.status}`);
        }
        await fetchLinks();
      } catch (e) {
        addToast({
          type: 'error',
          title: 'Smart link delete failed',
          message: `${link.brand_root}/${link.slug}: ${e instanceof Error ? e.message : String(e)}`,
        });
      }
    },
    [fetchLinks, addToast],
  );

  const grouped = useMemo(() => {
    const map = new Map<string, SmartLink[]>();
    for (const l of links) {
      const arr = map.get(l.brand_root) ?? [];
      arr.push(l);
      map.set(l.brand_root, arr);
    }
    return Array.from(map.entries()).sort(([a], [b]) => a.localeCompare(b));
  }, [links]);

  return (
    <div style={{ padding: '1.5rem', color: colors.text }}>
      <header
        style={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          marginBottom: '1rem',
        }}
      >
        <div>
          <h2 style={{ margin: 0, fontSize: '1.5rem', fontWeight: 600 }}>
            Smart Link Gateway
          </h2>
          <p style={{ margin: '0.25rem 0 0', color: colors.textMuted, fontSize: '0.875rem' }}>
            Brand-domain bot/human routing layer · v{PAGE_VERSION}
            {fetchedAt && (
              <span style={{ color: colors.textFaint, marginLeft: '0.75rem', fontSize: '0.6875rem' }}>
                fetched {fetchedAt}
              </span>
            )}
          </p>
        </div>
        <button
          onClick={() => setShowCreate(true)}
          style={{
            background: 'linear-gradient(135deg, #6366f1 0%, #8b5cf6 100%)',
            border: 'none',
            color: 'white',
            padding: '0.5rem 1rem',
            borderRadius: '0.5rem',
            cursor: 'pointer',
            fontSize: '0.875rem',
            fontWeight: 500,
          }}
        >
          <FontAwesomeIcon icon={faPlus} style={{ marginRight: '0.5rem' }} />
          New Smart Link
        </button>
      </header>

      <div style={{ display: 'flex', gap: '1rem', marginBottom: '1rem', alignItems: 'center' }}>
        <label style={{ fontSize: '0.875rem', color: '#cbd5e1' }}>
          Brand:
          <select
            value={brandFilter}
            onChange={(e) => setBrandFilter(e.target.value)}
            style={{
              marginLeft: '0.5rem',
              background: '#1e293b',
              color: colors.text,
              border: '1px solid #334155',
              borderRadius: '0.375rem',
              padding: '0.375rem 0.5rem',
            }}
          >
            <option value="">All</option>
            {BRAND_OPTIONS.map((b) => (
              <option key={b} value={b}>
                {b}
              </option>
            ))}
          </select>
        </label>
        <label style={{ fontSize: '0.875rem', color: '#cbd5e1', display: 'flex', alignItems: 'center', gap: '0.375rem' }}>
          <input
            type="checkbox"
            checked={includeDeleted}
            onChange={(e) => setIncludeDeleted(e.target.checked)}
          />
          Include deleted
        </label>
        <button
          onClick={fetchLinks}
          style={{
            background: '#1e293b',
            color: colors.text,
            border: '1px solid #334155',
            borderRadius: '0.375rem',
            padding: '0.375rem 0.75rem',
            cursor: 'pointer',
            fontSize: '0.875rem',
          }}
        >
          Refresh
        </button>
      </div>

      {error && (
        <div style={{ marginBottom: '1rem' }}>
          <SectionError label="Smart links" error={error} onRetry={fetchLinks} />
        </div>
      )}

      {loading ? (
        <div style={{ padding: '2rem', textAlign: 'center', color: colors.textMuted }}>
          <FontAwesomeIcon icon={faSpinner} spin style={{ marginRight: '0.5rem' }} />
          Loading smart links…
        </div>
      ) : error ? null : grouped.length === 0 ? (
        <div style={{ background: '#1e293b', borderRadius: '0.5rem', paddingBottom: '1rem' }}>
          <EmptyState
            icon={faLink}
            title="No smart links configured."
            hint="Create one to start piloting the gateway."
          />
          <div style={{ textAlign: 'center' }}>
            <button
              onClick={() => setShowCreate(true)}
              style={{ background: 'none', border: 'none', color: '#8b5cf6', cursor: 'pointer', textDecoration: 'underline', fontSize: '0.875rem' }}
            >
              Create one
            </button>
          </div>
        </div>
      ) : (
        grouped.map(([brand, rows]) => (
          <section key={brand} style={{ marginBottom: '1.5rem' }}>
            <h3
              style={{
                color: '#cbd5e1',
                fontSize: '1rem',
                marginBottom: '0.5rem',
                fontWeight: 600,
              }}
            >
              {brand}{' '}
              <span style={{ color: colors.textFaint, fontWeight: 400, fontSize: '0.875rem' }}>
                ({rows.length})
              </span>
            </h3>
            <div
              style={{
                background: '#0f172a',
                border: '1px solid #1e293b',
                borderRadius: '0.5rem',
                overflow: 'hidden',
              }}
            >
              <table style={{ ...tableStyle, fontSize: '0.875rem' }}>
                <thead style={{ background: '#1e293b' }}>
                  <tr>
                    <th style={thStyle}>Slug</th>
                    <th style={thStyle}>Review</th>
                    <th style={thStyle}>Offer URL</th>
                    <th style={thStyle}>Status</th>
                    <th style={thStyle}>Risk</th>
                    <th style={thStyle}>Updated</th>
                    <th style={thStyle}>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((link) => (
                    <tr key={link.id} style={{ borderTop: '1px solid #1e293b' }}>
                      <td style={tdStyle}>
                        <a
                          href={`https://${link.brand_root}/${link.slug}`}
                          target="_blank"
                          rel="noopener noreferrer"
                          style={{ color: colors.indigo300, textDecoration: 'none' }}
                        >
                          /{link.slug}
                          <FontAwesomeIcon icon={faExternalLinkAlt} style={{ marginLeft: '0.375rem', fontSize: '0.625rem' }} />
                        </a>
                      </td>
                      <td style={tdStyle}>
                        <a
                          href={`https://${link.brand_root}/reviews/${link.review_slug}`}
                          target="_blank"
                          rel="noopener noreferrer"
                          style={{ color: colors.textMuted, textDecoration: 'none' }}
                        >
                          {link.review_slug}
                        </a>
                      </td>
                      <td style={{ ...tdStyle, maxWidth: '320px', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                        <code style={{ fontSize: '0.75rem', color: '#cbd5e1' }} title={link.offer_url_template}>
                          {link.offer_url_template}
                        </code>
                      </td>
                      <td style={tdStyle}>
                        <Pill color={statusColor(link.status)}>{link.status}</Pill>
                      </td>
                      <td style={tdStyle}>
                        <select
                          value={link.risk_profile}
                          onChange={(e) => handlePatch(link, { risk_profile: e.target.value as 'low' | 'high' })}
                          disabled={link.status === 'deleted'}
                          title="low = direct redirect handoff · high = bridge page on brand site"
                          style={{
                            background: '#1e293b',
                            color: riskColor(link.risk_profile),
                            border: '1px solid #334155',
                            borderRadius: '0.25rem',
                            padding: '0.125rem 0.25rem',
                            fontSize: '0.75rem',
                          }}
                        >
                          <option value="low">low</option>
                          <option value="high">high</option>
                        </select>
                      </td>
                      <td style={{ ...tdStyle, color: colors.textMuted, fontSize: '0.75rem' }}>
                        {new Date(link.updated_at).toLocaleString()}
                      </td>
                      <td style={tdStyle}>
                        {link.status === 'active' ? (
                          <button
                            onClick={() => handlePatch(link, { status: 'paused' })}
                            title="Pause"
                            style={actionBtn}
                          >
                            <FontAwesomeIcon icon={faPause} />
                          </button>
                        ) : link.status === 'paused' ? (
                          <button
                            onClick={() => handlePatch(link, { status: 'active' })}
                            title="Reactivate"
                            style={actionBtn}
                          >
                            <FontAwesomeIcon icon={faPlay} />
                          </button>
                        ) : null}
                        {link.status !== 'deleted' && (
                          <button
                            onClick={() => handleDelete(link)}
                            title="Delete (soft)"
                            style={{ ...actionBtn, color: colors.dangerText }}
                          >
                            <FontAwesomeIcon icon={faTrash} />
                          </button>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </section>
        ))
      )}

      {showCreate && (
        <CreateDialog
          form={form}
          setForm={setForm}
          submitting={submitting}
          submitError={submitError}
          onClose={() => {
            setShowCreate(false);
            setSubmitError(null);
          }}
          onSubmit={handleCreate}
        />
      )}
    </div>
  );
};

const actionBtn: React.CSSProperties = {
  background: 'none',
  border: '1px solid #334155',
  color: '#cbd5e1',
  padding: '0.25rem 0.5rem',
  borderRadius: '0.25rem',
  cursor: 'pointer',
  marginRight: '0.25rem',
};

interface CreateDialogProps {
  form: {
    brand_root: string;
    slug: string;
    review_slug: string;
    offer_url_template: string;
    risk_profile: 'low' | 'high';
    notes: string;
  };
  setForm: React.Dispatch<React.SetStateAction<CreateDialogProps['form']>>;
  submitting: boolean;
  submitError: string | null;
  onClose: () => void;
  onSubmit: () => void;
}

const CreateDialog: React.FC<CreateDialogProps> = ({
  form,
  setForm,
  submitting,
  submitError,
  onClose,
  onSubmit,
}) => {
  const canSubmit =
    !submitting &&
    form.brand_root.trim() !== '' &&
    form.slug.trim() !== '' &&
    form.review_slug.trim() !== '' &&
    form.offer_url_template.trim() !== '';

  return (
    <div
      style={{
        position: 'fixed',
        inset: 0,
        background: 'rgba(0,0,0,0.7)',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        zIndex: 1000,
      }}
      onClick={onClose}
    >
      <div
        onClick={(e) => e.stopPropagation()}
        style={{
          background: '#0f172a',
          border: '1px solid #1e293b',
          borderRadius: '0.75rem',
          padding: '1.5rem',
          width: '100%',
          maxWidth: '640px',
          color: colors.text,
        }}
      >
        <h3 style={{ margin: '0 0 1rem', fontSize: '1.25rem' }}>New Smart Link</h3>

        <div style={{ display: 'grid', gap: '0.75rem' }}>
          <label style={labelStyle}>
            Brand
            <select
              value={form.brand_root}
              onChange={(e) => setForm({ ...form, brand_root: e.target.value })}
              style={inputStyle}
            >
              {BRAND_OPTIONS.map((b) => (
                <option key={b} value={b}>
                  {b}
                </option>
              ))}
            </select>
          </label>

          <label style={labelStyle}>
            Slug (lowercase, hyphens only)
            <input
              type="text"
              value={form.slug}
              onChange={(e) => setForm({ ...form, slug: e.target.value })}
              placeholder="capital-wallet"
              style={inputStyle}
            />
          </label>

          <label style={labelStyle}>
            Review slug (existing review article on the brand site)
            <input
              type="text"
              value={form.review_slug}
              onChange={(e) => setForm({ ...form, review_slug: e.target.value })}
              placeholder="capital-wallet"
              style={inputStyle}
            />
          </label>

          <label style={labelStyle}>
            Offer URL template (cratoolpro money URL with {'{{subscriber.id}}'} placeholder)
            <input
              type="text"
              value={form.offer_url_template}
              onChange={(e) => setForm({ ...form, offer_url_template: e.target.value })}
              placeholder="https://www.cratoolpro.com/BJB4Q5BF/KFSPRLK/?source_id=email&sub1={{subscriber.id}}&sub2={{brand.domain}}"
              style={inputStyle}
            />
          </label>

          <label style={labelStyle}>
            Risk profile
            <select
              value={form.risk_profile}
              onChange={(e) => setForm({ ...form, risk_profile: e.target.value as 'low' | 'high' })}
              style={inputStyle}
            >
              <option value="low">low — direct redirect handoff</option>
              <option value="high">high — bridge page on brand site</option>
            </select>
          </label>

          <label style={labelStyle}>
            Notes (optional)
            <textarea
              value={form.notes}
              onChange={(e) => setForm({ ...form, notes: e.target.value })}
              rows={2}
              style={{ ...inputStyle, fontFamily: 'inherit', resize: 'vertical' }}
            />
          </label>
        </div>

        {submitError && (
          <div
            style={{
              marginTop: '0.75rem',
              padding: '0.5rem 0.75rem',
              background: 'rgba(239, 68, 68, 0.1)',
              border: '1px solid rgba(239, 68, 68, 0.3)',
              color: colors.dangerText,
              borderRadius: '0.375rem',
              fontSize: '0.8125rem',
            }}
          >
            <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: '0.375rem' }} />
            {submitError}
          </div>
        )}

        <div style={{ display: 'flex', gap: '0.5rem', justifyContent: 'flex-end', marginTop: '1.25rem' }}>
          <button onClick={onClose} disabled={submitting} style={secondaryBtn}>
            Cancel
          </button>
          <button onClick={onSubmit} disabled={!canSubmit} style={primaryBtn}>
            {submitting ? (
              <>
                <FontAwesomeIcon icon={faSpinner} spin style={{ marginRight: '0.5rem' }} />
                Saving…
              </>
            ) : (
              <>
                <FontAwesomeIcon icon={faCheckCircle} style={{ marginRight: '0.5rem' }} />
                Create
              </>
            )}
          </button>
        </div>
      </div>
    </div>
  );
};

const labelStyle: React.CSSProperties = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.25rem',
  fontSize: '0.8125rem',
  color: '#cbd5e1',
};

const inputStyle: React.CSSProperties = {
  background: '#1e293b',
  border: '1px solid #334155',
  borderRadius: '0.375rem',
  padding: '0.5rem 0.75rem',
  color: colors.text,
  fontSize: '0.875rem',
};

const primaryBtn: React.CSSProperties = {
  background: 'linear-gradient(135deg, #6366f1 0%, #8b5cf6 100%)',
  border: 'none',
  color: 'white',
  padding: '0.5rem 1rem',
  borderRadius: '0.375rem',
  cursor: 'pointer',
  fontSize: '0.875rem',
  fontWeight: 500,
  opacity: 1,
};

const secondaryBtn: React.CSSProperties = {
  background: '#1e293b',
  border: '1px solid #334155',
  color: '#cbd5e1',
  padding: '0.5rem 1rem',
  borderRadius: '0.375rem',
  cursor: 'pointer',
  fontSize: '0.875rem',
};

export default SmartLinkManager;
