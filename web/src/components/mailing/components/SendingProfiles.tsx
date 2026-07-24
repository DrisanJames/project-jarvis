import React, { useState, useEffect, useCallback } from 'react';
import { useAuth } from '../../../contexts/AuthContext';
import './SendingProfiles.css';

interface SendingProfile {
  id: string;
  name: string;
  description?: string;
  vendor_type: string;
  from_name: string;
  from_email: string;
  reply_email?: string;
  sending_domain?: string;
  tracking_domain?: string;
  hourly_limit: number;
  daily_limit: number;
  current_hourly_count: number;
  current_daily_count: number;
  status: string;
  is_default: boolean;
  credentials_verified: boolean;
  domain_verified: boolean;
  spf_verified?: boolean;
  dkim_verified?: boolean;
  dmarc_verified?: boolean;
  via_ses?: boolean;
  ses_configuration_set?: string;
  ses_tenant_name?: string;
  routing_mode?: string;
  created_at: string;
}

// Result of POST /{id}/verify. The backend returns per-field DNS/identity
// verdicts (spf/dkim/dmarc/domain) alongside the credential check; fields the
// server did not check come back undefined and render as "not checked" —
// never as a green badge.
interface VerifyResult {
  verified?: boolean;
  spf_verified?: boolean;
  dkim_verified?: boolean;
  dmarc_verified?: boolean;
  domain_verified?: boolean;
  error?: string;
  checkedAt: string; // client-side timestamp of this verify run
}

// Normalize the verify response defensively: per-field verdicts may arrive
// top-level or nested under `checks` depending on server build.
function normalizeVerifyResponse(data: any): Omit<VerifyResult, 'checkedAt'> {
  const src = data && typeof data === 'object' ? { ...(data.checks || {}), ...data } : {};
  const pick = (k: string): boolean | undefined =>
    typeof src[k] === 'boolean' ? src[k] : undefined;
  return {
    verified: pick('verified') ?? pick('credentials_verified'),
    spf_verified: pick('spf_verified'),
    dkim_verified: pick('dkim_verified'),
    dmarc_verified: pick('dmarc_verified'),
    domain_verified: pick('domain_verified'),
    error: typeof src.error === 'string' ? src.error : (typeof src.verification_error === 'string' ? src.verification_error : undefined),
  };
}

const VENDOR_TYPES = [
  { value: 'pmta', label: 'PMTA (SES-routed)', icon: '🛰️' },
  { value: 'sparkpost', label: 'SparkPost', icon: '⚡' },
  { value: 'ses', label: 'AWS SES', icon: '☁️' },
  { value: 'mailgun', label: 'Mailgun', icon: '📧' },
  { value: 'sendgrid', label: 'SendGrid', icon: '📬' },
  { value: 'smtp', label: 'Custom SMTP', icon: '🔌' },
];

// routing_mode values understood by the send path: '' = server default
// (PMTA bridge / SES per via_ses), 'kumo' = KumoMTA transport.
const ROUTING_MODES = [
  { value: '', label: 'Default (PMTA bridge / SES)' },
  { value: 'kumo', label: 'KumoMTA' },
];

export const SendingProfiles: React.FC = () => {
  const { organization } = useAuth();
  const [profiles, setProfiles] = useState<SendingProfile[]>([]);
  const [loading, setLoading] = useState(true);
  const [listError, setListError] = useState<string | null>(null);
  const [showForm, setShowForm] = useState(false);
  const [editingProfile, setEditingProfile] = useState<SendingProfile | null>(null);
  const [verifying, setVerifying] = useState<string | null>(null);
  const [verifyResults, setVerifyResults] = useState<Record<string, VerifyResult>>({});
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});

  const EMPTY_FORM = {
    name: '',
    description: '',
    vendor_type: 'pmta',
    from_name: '',
    from_email: '',
    reply_email: '',
    api_key: '',
    sending_domain: '',
    tracking_domain: '',
    hourly_limit: 10000,
    daily_limit: 100000,
    via_ses: false,
    ses_configuration_set: '',
    ses_tenant_name: '',
    routing_mode: '',
  };

  // Form state
  const [form, setForm] = useState({ ...EMPTY_FORM });

  // Helper for API calls with organization context
  const orgFetch = useCallback((url: string, options: RequestInit = {}) => {
    const headers: Record<string, string> = {
      'Content-Type': 'application/json',
      ...(options.headers as Record<string, string> || {}),
    };
    if (organization?.id) {
      headers['X-Organization-ID'] = organization.id;
    }
    return fetch(url, { ...options, headers, credentials: 'include' });
  }, [organization]);

  const fetchProfiles = useCallback(async () => {
    try {
      const response = await orgFetch('/api/mailing/sending-profiles');
      const data = await response.json().catch(() => null);
      if (!response.ok) {
        throw new Error(data?.error || `HTTP ${response.status}`);
      }
      setProfiles(data?.profiles || []);
      setListError(null);
    } catch (err) {
      setListError(err instanceof Error ? err.message : 'Failed to fetch profiles');
    } finally {
      setLoading(false);
    }
  }, [orgFetch]);

  // Fetch profiles on mount and when organization changes
  useEffect(() => {
    fetchProfiles();
  }, [fetchProfiles]);

  // Mirrors the backend rule (planner preflight ses_routing_fields): a profile
  // routed via SES MUST carry both a configuration set and a tenant name, or
  // planning hard-fails at deploy time. Reject client-side with inline errors.
  const validateForm = (): Record<string, string> => {
    const errs: Record<string, string> = {};
    if (form.via_ses) {
      if (!form.ses_configuration_set.trim()) {
        errs.ses_configuration_set = 'Required when SES routing is on — the send worker stamps X-SES-CONFIGURATION-SET from this.';
      }
      if (!form.ses_tenant_name.trim()) {
        errs.ses_tenant_name = 'Required when SES routing is on — the send worker stamps X-SES-TENANT from this.';
      }
    }
    return errs;
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();

    const errs = validateForm();
    setFieldErrors(errs);
    if (Object.keys(errs).length > 0) return;

    const url = editingProfile
      ? `/api/mailing/sending-profiles/${editingProfile.id}`
      : '/api/mailing/sending-profiles';

    const method = editingProfile ? 'PUT' : 'POST';

    setSaving(true);
    setSaveError(null);
    try {
      const response = await orgFetch(url, {
        method,
        body: JSON.stringify({
          ...form,
          organization_id: organization?.id,
        }),
      });

      if (response.ok) {
        fetchProfiles();
        resetForm();
      } else {
        const data = await response.json().catch(() => null);
        setSaveError(data?.error || `Save failed (HTTP ${response.status})`);
      }
    } catch (err) {
      setSaveError(err instanceof Error ? err.message : 'Failed to save profile');
    } finally {
      setSaving(false);
    }
  };

  const handleVerify = async (profileId: string) => {
    setVerifying(profileId);
    try {
      const response = await orgFetch(`/api/mailing/sending-profiles/${profileId}/verify`, {
        method: 'POST',
      });
      const data = await response.json().catch(() => null);
      if (!response.ok) {
        setVerifyResults(prev => ({
          ...prev,
          [profileId]: {
            checkedAt: new Date().toISOString(),
            error: data?.error || `Verify failed (HTTP ${response.status})`,
          },
        }));
        return;
      }
      setVerifyResults(prev => ({
        ...prev,
        [profileId]: { ...normalizeVerifyResponse(data), checkedAt: new Date().toISOString() },
      }));
      // Refresh regardless of outcome — the server persists the *_verified
      // columns, and the list is the durable projection of the same rows.
      fetchProfiles();
    } catch (err) {
      setVerifyResults(prev => ({
        ...prev,
        [profileId]: {
          checkedAt: new Date().toISOString(),
          error: err instanceof Error ? err.message : 'Verification failed',
        },
      }));
    } finally {
      setVerifying(null);
    }
  };

  const handleSetDefault = async (profileId: string) => {
    try {
      await orgFetch(`/api/mailing/sending-profiles/${profileId}/set-default`, {
        method: 'POST',
      });
      fetchProfiles();
    } catch (err) {
      console.error('Failed to set default');
    }
  };

  const handleDelete = async (profileId: string) => {
    if (!confirm('Are you sure you want to delete this profile?')) return;
    
    try {
      await orgFetch(`/api/mailing/sending-profiles/${profileId}`, {
        method: 'DELETE',
      });
      fetchProfiles();
    } catch (err) {
      console.error('Failed to delete profile');
    }
  };

  const handleEdit = (profile: SendingProfile) => {
    setEditingProfile(profile);
    setForm({
      name: profile.name,
      description: profile.description || '',
      vendor_type: profile.vendor_type,
      from_name: profile.from_name,
      from_email: profile.from_email,
      reply_email: profile.reply_email || '',
      api_key: '', // Don't populate API key for security
      sending_domain: profile.sending_domain || '',
      tracking_domain: profile.tracking_domain || '',
      hourly_limit: profile.hourly_limit,
      daily_limit: profile.daily_limit,
      via_ses: profile.via_ses || false,
      ses_configuration_set: profile.ses_configuration_set || '',
      ses_tenant_name: profile.ses_tenant_name || '',
      routing_mode: profile.routing_mode || '',
    });
    setFieldErrors({});
    setSaveError(null);
    setShowForm(true);
  };

  const resetForm = () => {
    setShowForm(false);
    setEditingProfile(null);
    setFieldErrors({});
    setSaveError(null);
    setForm({ ...EMPTY_FORM });
  };

  const getVendorIcon = (type: string) => {
    const vendor = VENDOR_TYPES.find(v => v.value === type);
    return vendor?.icon || '📧';
  };

  const getVendorLabel = (type: string) => {
    const vendor = VENDOR_TYPES.find(v => v.value === type);
    return vendor?.label || type;
  };

  if (loading) {
    return <div className="loading-state">Loading sending profiles...</div>;
  }

  if (listError && profiles.length === 0) {
    return (
      <div className="sending-profiles">
        <div className="list-error-state">
          <p>⚠️ Could not load sending profiles: {listError}</p>
          <button
            className="add-button"
            onClick={() => {
              setLoading(true);
              fetchProfiles();
            }}
          >
            Retry
          </button>
        </div>
      </div>
    );
  }

  // Tri-state per-field verification chip: true = verified (green), false =
  // failed (red), undefined = not checked (muted). Never renders green for an
  // unchecked field.
  const VerifyField: React.FC<{ label: string; value: boolean | undefined }> = ({ label, value }) => (
    <span
      className={`verify-badge ${value === true ? 'verified' : value === false ? 'failed' : 'unchecked'}`}
      title={value === undefined ? `${label}: not checked by this server yet` : `${label}: ${value ? 'verified' : 'FAILED'}`}
    >
      {value === true ? '✅' : value === false ? '❌' : '·'} {label}
    </span>
  );

  return (
    <div className="sending-profiles">
      <div className="profiles-header">
        <div>
          <h1>🚀 Sending Profiles</h1>
          <p className="subtitle">Configure email service connections (multiple providers supported)</p>
        </div>
        <button className="add-button" onClick={() => setShowForm(true)}>
          + Add Profile
        </button>
      </div>

      {/* Profile Form Modal */}
      {showForm && (
        <div className="modal-overlay" onClick={() => resetForm()}>
          <div className="modal-content" onClick={e => e.stopPropagation()}>
            <h2>{editingProfile ? 'Edit Profile' : 'Create Sending Profile'}</h2>
            <form onSubmit={handleSubmit}>
              <div className="form-row">
                <div className="form-group">
                  <label>Profile Name *</label>
                  <input
                    type="text"
                    value={form.name}
                    onChange={e => setForm({...form, name: e.target.value})}
                    placeholder="e.g., SparkPost - Marketing"
                    required
                  />
                </div>
                <div className="form-group">
                  <label>Vendor Type *</label>
                  <select
                    value={form.vendor_type}
                    onChange={e => setForm({...form, vendor_type: e.target.value})}
                  >
                    {VENDOR_TYPES.map(v => (
                      <option key={v.value} value={v.value}>
                        {v.icon} {v.label}
                      </option>
                    ))}
                  </select>
                </div>
              </div>

              <div className="form-group">
                <label>Description</label>
                <input
                  type="text"
                  value={form.description}
                  onChange={e => setForm({...form, description: e.target.value})}
                  placeholder="Optional description"
                />
              </div>

              <div className="form-row">
                <div className="form-group">
                  <label>From Name *</label>
                  <input
                    type="text"
                    value={form.from_name}
                    onChange={e => setForm({...form, from_name: e.target.value})}
                    placeholder="Your Company"
                    required
                  />
                </div>
                <div className="form-group">
                  <label>From Email *</label>
                  <input
                    type="email"
                    value={form.from_email}
                    onChange={e => setForm({...form, from_email: e.target.value})}
                    placeholder="hello@yourdomain.com"
                    required
                  />
                </div>
              </div>

              <div className="form-row">
                <div className="form-group">
                  <label>Reply Email</label>
                  <input
                    type="email"
                    value={form.reply_email}
                    onChange={e => setForm({...form, reply_email: e.target.value})}
                    placeholder="reply@yourdomain.com"
                  />
                </div>
                <div className="form-group">
                  <label>Sending Domain</label>
                  <input
                    type="text"
                    value={form.sending_domain}
                    onChange={e => setForm({...form, sending_domain: e.target.value})}
                    placeholder="yourdomain.com"
                  />
                </div>
              </div>

              <div className="form-row">
                <div className="form-group">
                  <label>Tracking Domain</label>
                  <input
                    type="text"
                    value={form.tracking_domain}
                    onChange={e => setForm({...form, tracking_domain: e.target.value})}
                    placeholder="t.em.yourdomain.com"
                  />
                  <span className="field-hint">Open pixel + click-wrap base for this profile's sends.</span>
                </div>
                <div className="form-group">
                  <label>Routing Mode</label>
                  <select
                    value={form.routing_mode}
                    onChange={e => setForm({...form, routing_mode: e.target.value})}
                  >
                    {ROUTING_MODES.map(m => (
                      <option key={m.value} value={m.value}>{m.label}</option>
                    ))}
                  </select>
                </div>
              </div>

              {/* SES tenant routing — the production convention for first-party
                  domains is vendor_type='pmta' + via_ses=true (the PMTA HTTP
                  bridge relays to SES using the headers derived from these). */}
              <div className={`ses-section ${form.via_ses ? 'open' : ''}`}>
                <label className="ses-toggle">
                  <input
                    type="checkbox"
                    checked={form.via_ses}
                    onChange={e => setForm({...form, via_ses: e.target.checked})}
                  />
                  <span className="ses-toggle-label">Route via SES (tenant relay)</span>
                  <span className="ses-toggle-hint">
                    Sends relay to AWS SES through the PMTA bridge; requires a configuration set and tenant name.
                  </span>
                </label>

                {form.via_ses && (
                  <div className="form-row">
                    <div className="form-group">
                      <label>SES Configuration Set *</label>
                      <input
                        type="text"
                        value={form.ses_configuration_set}
                        onChange={e => setForm({...form, ses_configuration_set: e.target.value})}
                        placeholder="e.g. discountblog"
                        className={fieldErrors.ses_configuration_set ? 'input-error' : ''}
                      />
                      {fieldErrors.ses_configuration_set && (
                        <span className="field-error">{fieldErrors.ses_configuration_set}</span>
                      )}
                    </div>
                    <div className="form-group">
                      <label>SES Tenant Name *</label>
                      <input
                        type="text"
                        value={form.ses_tenant_name}
                        onChange={e => setForm({...form, ses_tenant_name: e.target.value})}
                        placeholder="e.g. discountblog"
                        className={fieldErrors.ses_tenant_name ? 'input-error' : ''}
                      />
                      {fieldErrors.ses_tenant_name && (
                        <span className="field-error">{fieldErrors.ses_tenant_name}</span>
                      )}
                    </div>
                  </div>
                )}
              </div>

              <div className="form-group">
                <label>API Key {editingProfile && '(leave blank to keep current)'}</label>
                <input
                  type="password"
                  value={form.api_key}
                  onChange={e => setForm({...form, api_key: e.target.value})}
                  placeholder="Enter API key"
                />
              </div>

              <div className="form-row">
                <div className="form-group">
                  <label>Hourly Limit</label>
                  <input
                    type="number"
                    value={form.hourly_limit}
                    onChange={e => setForm({...form, hourly_limit: parseInt(e.target.value)})}
                  />
                </div>
                <div className="form-group">
                  <label>Daily Limit</label>
                  <input
                    type="number"
                    value={form.daily_limit}
                    onChange={e => setForm({...form, daily_limit: parseInt(e.target.value)})}
                  />
                </div>
              </div>

              {saveError && (
                <div className="form-error-banner">
                  ⚠️ {saveError}
                </div>
              )}

              <div className="form-actions">
                <button type="button" className="cancel-button" onClick={resetForm}>
                  Cancel
                </button>
                <button type="submit" className="submit-button" disabled={saving}>
                  {saving ? 'Saving…' : editingProfile ? 'Update Profile' : 'Create Profile'}
                </button>
              </div>
            </form>
          </div>
        </div>
      )}

      {/* Stale-data banner: the grid below still shows the last good fetch */}
      {listError && profiles.length > 0 && (
        <div className="form-error-banner" style={{ marginBottom: 14 }}>
          ⚠️ Refresh failed ({listError}) — showing last loaded profiles.
          <button
            className="action-btn verify"
            style={{ marginLeft: 10 }}
            onClick={() => fetchProfiles()}
          >
            Retry
          </button>
        </div>
      )}

      {/* Profiles List */}
      <div className="profiles-grid">
        {profiles.length === 0 ? (
          <div className="empty-state">
            <p>No sending profiles configured yet.</p>
            <p>Add a profile to route emails through different email services.</p>
          </div>
        ) : (
          profiles.map(profile => (
            <div key={profile.id} className={`profile-card ${profile.is_default ? 'default' : ''}`}>
              <div className="profile-header">
                <div className="profile-icon">{getVendorIcon(profile.vendor_type)}</div>
                <div className="profile-title">
                  <h3>{profile.name}</h3>
                  <span className="vendor-badge">{getVendorLabel(profile.vendor_type)}</span>
                  {profile.via_ses && <span className="ses-badge">SES-ROUTED</span>}
                  {profile.is_default && <span className="default-badge">⭐ DEFAULT</span>}
                </div>
                <span className={`status-badge ${profile.status}`}>
                  {profile.status}
                </span>
              </div>

              <div className="profile-details">
                <div className="detail-row">
                  <span className="label">From:</span>
                  <span>{profile.from_name} &lt;{profile.from_email}&gt;</span>
                </div>
                {profile.sending_domain && (
                  <div className="detail-row">
                    <span className="label">Domain:</span>
                    <span>{profile.sending_domain}</span>
                  </div>
                )}
                {profile.tracking_domain && (
                  <div className="detail-row">
                    <span className="label">Tracking:</span>
                    <span>{profile.tracking_domain}</span>
                  </div>
                )}
                {profile.via_ses && (
                  <div className="detail-row">
                    <span className="label">SES:</span>
                    <span>
                      config-set <code>{profile.ses_configuration_set || '∅'}</code>
                      {' · '}tenant <code>{profile.ses_tenant_name || '∅'}</code>
                    </span>
                  </div>
                )}
                {profile.routing_mode && (
                  <div className="detail-row">
                    <span className="label">Routing:</span>
                    <span>{profile.routing_mode}</span>
                  </div>
                )}
                <div className="detail-row">
                  <span className="label">Limits:</span>
                  <span>{profile.hourly_limit.toLocaleString()}/hr, {profile.daily_limit.toLocaleString()}/day</span>
                </div>
                <div className="detail-row">
                  <span className="label">Usage:</span>
                  <span>{profile.current_hourly_count}/{profile.hourly_limit} this hour</span>
                </div>
              </div>

              {(() => {
                // Fresh verify result (this session) wins; the persisted list
                // row is the fallback projection of the same columns.
                const vr = verifyResults[profile.id];
                const spf = vr ? vr.spf_verified : profile.spf_verified;
                const dkim = vr ? vr.dkim_verified : profile.dkim_verified;
                const dmarc = vr ? vr.dmarc_verified : profile.dmarc_verified;
                const dom = vr ? (vr.domain_verified ?? profile.domain_verified) : profile.domain_verified;
                return (
                  <div className="verification-status">
                    <span className={`verify-badge ${profile.credentials_verified ? 'verified' : 'unverified'}`}>
                      {profile.credentials_verified ? '✅ Credentials' : '⚠️ Credentials unverified'}
                    </span>
                    <VerifyField label="SPF" value={spf} />
                    <VerifyField label="DKIM" value={dkim} />
                    <VerifyField label="DMARC" value={dmarc} />
                    <VerifyField label="Domain" value={dom} />
                    {vr?.error && (
                      <span className="verify-badge failed" title={vr.error}>❌ {vr.error}</span>
                    )}
                    {vr && !vr.error && (
                      <span className="verify-checked-at">
                        checked {new Date(vr.checkedAt).toLocaleTimeString()}
                      </span>
                    )}
                  </div>
                );
              })()}

              <div className="profile-actions">
                <button 
                  className="action-btn verify" 
                  onClick={() => handleVerify(profile.id)}
                  disabled={verifying === profile.id}
                >
                  {verifying === profile.id ? '⏳' : '🔍'} Verify
                </button>
                {!profile.is_default && (
                  <button 
                    className="action-btn default" 
                    onClick={() => handleSetDefault(profile.id)}
                  >
                    ⭐ Set Default
                  </button>
                )}
                <button className="action-btn edit" onClick={() => handleEdit(profile)}>
                  ✏️ Edit
                </button>
                <button className="action-btn delete" onClick={() => handleDelete(profile.id)}>
                  🗑️ Delete
                </button>
              </div>
            </div>
          ))
        )}
      </div>
    </div>
  );
};

export default SendingProfiles;
