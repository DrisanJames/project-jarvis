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
  hourly_limit: number;
  daily_limit: number;
  current_hourly_count: number;
  current_daily_count: number;
  status: string;
  is_default: boolean;
  credentials_verified: boolean;
  domain_verified: boolean;
  created_at: string;
}

const VENDOR_TYPES = [
  { value: 'sparkpost', label: 'SparkPost', icon: '⚡' },
  { value: 'ses', label: 'AWS SES', icon: '☁️' },
  { value: 'mailgun', label: 'Mailgun', icon: '📧' },
  { value: 'sendgrid', label: 'SendGrid', icon: '📬' },
  { value: 'smtp', label: 'Custom SMTP', icon: '🔌' },
];

export const SendingProfiles: React.FC = () => {
  const { organization } = useAuth();
  const [profiles, setProfiles] = useState<SendingProfile[]>([]);
  const [loading, setLoading] = useState(true);
  const [showForm, setShowForm] = useState(false);
  const [editingProfile, setEditingProfile] = useState<SendingProfile | null>(null);
  const [verifying, setVerifying] = useState<string | null>(null);

  // Form state
  const [form, setForm] = useState({
    name: '',
    description: '',
    vendor_type: 'sparkpost',
    from_name: '',
    from_email: '',
    reply_email: '',
    api_key: '',
    sending_domain: '',
    hourly_limit: 10000,
    daily_limit: 100000,
  });

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
      const data = await response.json();
      setProfiles(data.profiles || []);
    } catch (err) {
      console.error('Failed to fetch profiles');
    } finally {
      setLoading(false);
    }
  }, [orgFetch]);

  // Fetch profiles on mount and when organization changes
  useEffect(() => {
    fetchProfiles();
  }, [fetchProfiles]);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    
    const url = editingProfile 
      ? `/api/mailing/sending-profiles/${editingProfile.id}`
      : '/api/mailing/sending-profiles';
    
    const method = editingProfile ? 'PUT' : 'POST';

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
      }
    } catch (err) {
      console.error('Failed to save profile');
    }
  };

  const handleVerify = async (profileId: string) => {
    setVerifying(profileId);
    try {
      const response = await orgFetch(`/api/mailing/sending-profiles/${profileId}/verify`, {
        method: 'POST',
      });
      const data = await response.json();
      if (data.verified) {
        fetchProfiles();
      }
    } catch (err) {
      console.error('Verification failed');
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
      hourly_limit: profile.hourly_limit,
      daily_limit: profile.daily_limit,
    });
    setShowForm(true);
  };

  const resetForm = () => {
    setShowForm(false);
    setEditingProfile(null);
    setForm({
      name: '',
      description: '',
      vendor_type: 'sparkpost',
      from_name: '',
      from_email: '',
      reply_email: '',
      api_key: '',
      sending_domain: '',
      hourly_limit: 10000,
      daily_limit: 100000,
    });
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

  return (
    <div className="sending-profiles">
      <div className="profiles-header">
        <div>
          <h1>🚀 Sending Profiles</h1>
          <p className="subtitle">Configure ESP connections (SparkPost, SES, Mailgun, etc.)</p>
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

              <div className="form-actions">
                <button type="button" className="cancel-button" onClick={resetForm}>
                  Cancel
                </button>
                <button type="submit" className="submit-button">
                  {editingProfile ? 'Update Profile' : 'Create Profile'}
                </button>
              </div>
            </form>
          </div>
        </div>
      )}

      {/* Profiles List */}
      <div className="profiles-grid">
        {profiles.length === 0 ? (
          <div className="empty-state">
            <p>No sending profiles configured yet.</p>
            <p>Add a profile to start routing emails through different ESPs.</p>
          </div>
        ) : (
          profiles.map(profile => (
            <div key={profile.id} className={`profile-card ${profile.is_default ? 'default' : ''}`}>
              <div className="profile-header">
                <div className="profile-icon">{getVendorIcon(profile.vendor_type)}</div>
                <div className="profile-title">
                  <h3>{profile.name}</h3>
                  <span className="vendor-badge">{getVendorLabel(profile.vendor_type)}</span>
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
                <div className="detail-row">
                  <span className="label">Limits:</span>
                  <span>{profile.hourly_limit.toLocaleString()}/hr, {profile.daily_limit.toLocaleString()}/day</span>
                </div>
                <div className="detail-row">
                  <span className="label">Usage:</span>
                  <span>{profile.current_hourly_count}/{profile.hourly_limit} this hour</span>
                </div>
              </div>

              <div className="verification-status">
                <span className={`verify-badge ${profile.credentials_verified ? 'verified' : 'unverified'}`}>
                  {profile.credentials_verified ? '✅ Credentials Verified' : '⚠️ Unverified'}
                </span>
                {profile.domain_verified && (
                  <span className="verify-badge verified">✅ Domain Verified</span>
                )}
              </div>

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
