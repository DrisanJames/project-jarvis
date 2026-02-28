import React, { useState, useEffect, useCallback } from 'react';
import './SegmentCleanup.css';

// ==========================================
// TYPES
// ==========================================

interface CleanupSettings {
  enabled: boolean;
  inactive_days_threshold: number;
  grace_period_days: number;
  auto_archive: boolean;
  auto_delete: boolean;
  archive_retention_days: number;
  notify_admins: boolean;
  admin_emails: string[];
  min_segment_age_days: number;
  exclude_patterns: string[];
}

interface PendingCleanup {
  id: string;
  segment_id: string;
  segment_name: string;
  subscriber_count: number;
  last_used_at: string | null;
  warning_sent_at: string;
  grace_period_ends_at: string;
  days_remaining: number;
}

interface StaleSegment {
  id: string;
  name: string;
  subscriber_count: number;
  last_used_at: string | null;
  days_inactive: number;
  created_at: string;
  keep_active: boolean;
  cleanup_warning_sent: boolean;
}

interface CleanupHistory {
  segment_name: string;
  subscriber_count: number;
  warning_sent_at: string;
  action_taken: string;
  action_taken_at: string;
}

// ==========================================
// MAIN COMPONENT
// ==========================================

export const SegmentCleanup: React.FC = () => {
  const [activeTab, setActiveTab] = useState<'settings' | 'pending' | 'stale' | 'history'>('pending');
  const [settings, setSettings] = useState<CleanupSettings | null>(null);
  const [pending, setPending] = useState<PendingCleanup[]>([]);
  const [staleSegments, setStaleSegments] = useState<StaleSegment[]>([]);
  const [history, setHistory] = useState<CleanupHistory[]>([]);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [newEmail, setNewEmail] = useState('');
  const [newPattern, setNewPattern] = useState('');

  // Load all data
  const loadData = useCallback(async () => {
    setLoading(true);
    try {
      const [settingsRes, pendingRes, staleRes, historyRes] = await Promise.all([
        fetch('/api/mailing/segment-cleanup/settings'),
        fetch('/api/mailing/segment-cleanup/pending'),
        fetch('/api/mailing/segment-cleanup/stale-segments'),
        fetch('/api/mailing/segment-cleanup/history'),
      ]);

      if (settingsRes.ok) {
        const data = await settingsRes.json();
        setSettings(data);
      }
      if (pendingRes.ok) {
        const data = await pendingRes.json();
        setPending(data.pending || []);
      }
      if (staleRes.ok) {
        const data = await staleRes.json();
        setStaleSegments(data.stale_segments || []);
      }
      if (historyRes.ok) {
        const data = await historyRes.json();
        setHistory(data.history || []);
      }
    } catch (error) {
      console.error('Failed to load cleanup data:', error);
    }
    setLoading(false);
  }, []);

  useEffect(() => {
    loadData();
  }, [loadData]);

  // Save settings
  const saveSettings = async () => {
    if (!settings) return;
    setSaving(true);
    try {
      const response = await fetch('/api/mailing/segment-cleanup/settings', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(settings),
      });
      if (response.ok) {
        alert('Settings saved successfully');
      }
    } catch (error) {
      console.error('Failed to save settings:', error);
      alert('Failed to save settings');
    }
    setSaving(false);
  };

  // Keep segment active
  const keepActive = async (segmentId: string) => {
    try {
      const response = await fetch(`/api/mailing/segment-cleanup/segments/${segmentId}/keep-active`, {
        method: 'POST',
      });
      if (response.ok) {
        loadData(); // Refresh
      }
    } catch (error) {
      console.error('Failed to mark segment as keep active:', error);
    }
  };

  // Count segment (resets active state)
  const countSegment = async (segmentId: string) => {
    try {
      const response = await fetch(`/api/mailing/segment-cleanup/segments/${segmentId}/count`, {
        method: 'POST',
      });
      if (response.ok) {
        const data = await response.json();
        alert(`Segment has ${data.subscriber_count} subscribers. Cleanup warning cleared.`);
        loadData();
      }
    } catch (error) {
      console.error('Failed to count segment:', error);
    }
  };

  // Add email to notify list
  const addEmail = () => {
    if (!settings || !newEmail || !newEmail.includes('@')) return;
    if (!settings.admin_emails.includes(newEmail)) {
      setSettings({
        ...settings,
        admin_emails: [...settings.admin_emails, newEmail],
      });
    }
    setNewEmail('');
  };

  // Remove email from notify list
  const removeEmail = (email: string) => {
    if (!settings) return;
    setSettings({
      ...settings,
      admin_emails: settings.admin_emails.filter(e => e !== email),
    });
  };

  // Add exclude pattern
  const addPattern = () => {
    if (!settings || !newPattern) return;
    if (!settings.exclude_patterns.includes(newPattern)) {
      setSettings({
        ...settings,
        exclude_patterns: [...settings.exclude_patterns, newPattern],
      });
    }
    setNewPattern('');
  };

  // Remove exclude pattern
  const removePattern = (pattern: string) => {
    if (!settings) return;
    setSettings({
      ...settings,
      exclude_patterns: settings.exclude_patterns.filter(p => p !== pattern),
    });
  };

  if (loading) {
    return (
      <div className="sc-loading">
        <div className="sc-spinner"></div>
        <p>Loading cleanup data...</p>
      </div>
    );
  }

  return (
    <div className="segment-cleanup">
      <div className="sc-header">
        <div>
          <h1>🧹 Segment Cleanup</h1>
          <p>Automatically clean up unused segments to keep your system organized</p>
        </div>
        <div className="sc-stats">
          <div className="sc-stat">
            <span className="sc-stat-value">{pending.length}</span>
            <span className="sc-stat-label">Pending Cleanup</span>
          </div>
          <div className="sc-stat">
            <span className="sc-stat-value">{staleSegments.length}</span>
            <span className="sc-stat-label">Stale Segments</span>
          </div>
        </div>
      </div>

      {/* Tabs */}
      <div className="sc-tabs">
        <button 
          className={activeTab === 'pending' ? 'active' : ''} 
          onClick={() => setActiveTab('pending')}
        >
          ⏳ Pending Cleanup ({pending.length})
        </button>
        <button 
          className={activeTab === 'stale' ? 'active' : ''} 
          onClick={() => setActiveTab('stale')}
        >
          ⚠️ Stale Segments ({staleSegments.length})
        </button>
        <button 
          className={activeTab === 'settings' ? 'active' : ''} 
          onClick={() => setActiveTab('settings')}
        >
          ⚙️ Settings
        </button>
        <button 
          className={activeTab === 'history' ? 'active' : ''} 
          onClick={() => setActiveTab('history')}
        >
          📜 History
        </button>
      </div>

      <div className="sc-content">
        {/* Pending Cleanup Tab */}
        {activeTab === 'pending' && (
          <div className="sc-pending">
            <div className="sc-section-header">
              <h2>Segments Pending Cleanup</h2>
              <p>These segments have been warned and will be cleaned up unless action is taken</p>
            </div>
            
            {pending.length === 0 ? (
              <div className="sc-empty">
                <span className="sc-empty-icon">✅</span>
                <p>No segments pending cleanup</p>
              </div>
            ) : (
              <table className="sc-table">
                <thead>
                  <tr>
                    <th>Segment Name</th>
                    <th>Subscribers</th>
                    <th>Warning Sent</th>
                    <th>Days Remaining</th>
                    <th>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {pending.map((item) => (
                    <tr key={item.id}>
                      <td>
                        <strong>{item.segment_name}</strong>
                      </td>
                      <td>{item.subscriber_count.toLocaleString()}</td>
                      <td>{new Date(item.warning_sent_at).toLocaleDateString()}</td>
                      <td>
                        <span className={`sc-days ${item.days_remaining <= 2 ? 'critical' : item.days_remaining <= 4 ? 'warning' : ''}`}>
                          {Math.max(0, Math.ceil(item.days_remaining))} days
                        </span>
                      </td>
                      <td>
                        <div className="sc-actions">
                          <button className="sc-btn sc-btn-primary" onClick={() => keepActive(item.segment_id)}>
                            Keep Active
                          </button>
                          <button className="sc-btn" onClick={() => countSegment(item.segment_id)}>
                            Count & Reset
                          </button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        )}

        {/* Stale Segments Tab */}
        {activeTab === 'stale' && (
          <div className="sc-stale">
            <div className="sc-section-header">
              <h2>Stale Segments</h2>
              <p>Segments that haven't been used and may receive cleanup warnings soon</p>
            </div>
            
            {staleSegments.length === 0 ? (
              <div className="sc-empty">
                <span className="sc-empty-icon">🎉</span>
                <p>No stale segments found</p>
              </div>
            ) : (
              <table className="sc-table">
                <thead>
                  <tr>
                    <th>Segment Name</th>
                    <th>Subscribers</th>
                    <th>Last Used</th>
                    <th>Days Inactive</th>
                    <th>Status</th>
                    <th>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {staleSegments.map((seg) => (
                    <tr key={seg.id} className={seg.keep_active ? 'protected' : ''}>
                      <td>
                        <strong>{seg.name}</strong>
                        {seg.keep_active && <span className="sc-badge sc-badge-green">Protected</span>}
                      </td>
                      <td>{seg.subscriber_count.toLocaleString()}</td>
                      <td>
                        {seg.last_used_at 
                          ? new Date(seg.last_used_at).toLocaleDateString() 
                          : <em>Never</em>}
                      </td>
                      <td>
                        <span className={`sc-days ${seg.days_inactive > 60 ? 'critical' : seg.days_inactive > 30 ? 'warning' : ''}`}>
                          {seg.days_inactive} days
                        </span>
                      </td>
                      <td>
                        {seg.cleanup_warning_sent 
                          ? <span className="sc-badge sc-badge-yellow">Warning Sent</span>
                          : seg.keep_active 
                            ? <span className="sc-badge sc-badge-green">Protected</span>
                            : <span className="sc-badge sc-badge-gray">At Risk</span>}
                      </td>
                      <td>
                        <div className="sc-actions">
                          {!seg.keep_active && (
                            <button className="sc-btn sc-btn-primary" onClick={() => keepActive(seg.id)}>
                              Keep Active
                            </button>
                          )}
                          <button className="sc-btn" onClick={() => countSegment(seg.id)}>
                            Count & Reset
                          </button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        )}

        {/* Settings Tab */}
        {activeTab === 'settings' && settings && (
          <div className="sc-settings">
            <div className="sc-section-header">
              <h2>Cleanup Settings</h2>
              <p>Configure automatic segment cleanup behavior</p>
            </div>

            <div className="sc-settings-grid">
              {/* Enable/Disable */}
              <div className="sc-setting-card">
                <h3>Automatic Cleanup</h3>
                <label className="sc-toggle">
                  <input
                    type="checkbox"
                    checked={settings.enabled}
                    onChange={(e) => setSettings({ ...settings, enabled: e.target.checked })}
                  />
                  <span className="sc-toggle-slider"></span>
                  <span>{settings.enabled ? 'Enabled' : 'Disabled'}</span>
                </label>
              </div>

              {/* Inactive Threshold */}
              <div className="sc-setting-card">
                <h3>Inactive Threshold</h3>
                <p>Days of inactivity before warning</p>
                <input
                  type="number"
                  min={1}
                  max={365}
                  value={settings.inactive_days_threshold}
                  onChange={(e) => setSettings({ ...settings, inactive_days_threshold: parseInt(e.target.value) || 30 })}
                />
                <span className="sc-unit">days</span>
              </div>

              {/* Grace Period */}
              <div className="sc-setting-card">
                <h3>Grace Period</h3>
                <p>Days after warning before cleanup</p>
                <input
                  type="number"
                  min={1}
                  max={30}
                  value={settings.grace_period_days}
                  onChange={(e) => setSettings({ ...settings, grace_period_days: parseInt(e.target.value) || 7 })}
                />
                <span className="sc-unit">days</span>
              </div>

              {/* Min Age */}
              <div className="sc-setting-card">
                <h3>Minimum Segment Age</h3>
                <p>Don't warn about new segments</p>
                <input
                  type="number"
                  min={1}
                  max={90}
                  value={settings.min_segment_age_days}
                  onChange={(e) => setSettings({ ...settings, min_segment_age_days: parseInt(e.target.value) || 14 })}
                />
                <span className="sc-unit">days</span>
              </div>

              {/* Archive Options */}
              <div className="sc-setting-card full-width">
                <h3>Cleanup Actions</h3>
                <div className="sc-checkbox-group">
                  <label className="sc-checkbox">
                    <input
                      type="checkbox"
                      checked={settings.auto_archive}
                      onChange={(e) => setSettings({ ...settings, auto_archive: e.target.checked })}
                    />
                    <span>Auto-archive unused segments (can be restored)</span>
                  </label>
                  <label className="sc-checkbox">
                    <input
                      type="checkbox"
                      checked={settings.auto_delete}
                      onChange={(e) => setSettings({ ...settings, auto_delete: e.target.checked })}
                    />
                    <span>Permanently delete archived segments after {settings.archive_retention_days} days</span>
                  </label>
                </div>
                {settings.auto_delete && (
                  <div className="sc-inline-input">
                    <label>Archive retention:</label>
                    <input
                      type="number"
                      min={30}
                      max={365}
                      value={settings.archive_retention_days}
                      onChange={(e) => setSettings({ ...settings, archive_retention_days: parseInt(e.target.value) || 90 })}
                    />
                    <span>days</span>
                  </div>
                )}
              </div>

              {/* Notifications */}
              <div className="sc-setting-card full-width">
                <h3>Notifications</h3>
                <label className="sc-checkbox">
                  <input
                    type="checkbox"
                    checked={settings.notify_admins}
                    onChange={(e) => setSettings({ ...settings, notify_admins: e.target.checked })}
                  />
                  <span>Send email notifications to admins</span>
                </label>
                
                {settings.notify_admins && (
                  <div className="sc-email-list">
                    <label>Additional email addresses:</label>
                    <div className="sc-tags">
                      {settings.admin_emails.map((email) => (
                        <span key={email} className="sc-tag">
                          {email}
                          <button onClick={() => removeEmail(email)}>×</button>
                        </span>
                      ))}
                    </div>
                    <div className="sc-add-input">
                      <input
                        type="email"
                        placeholder="email@example.com"
                        value={newEmail}
                        onChange={(e) => setNewEmail(e.target.value)}
                        onKeyPress={(e) => e.key === 'Enter' && addEmail()}
                      />
                      <button onClick={addEmail}>Add</button>
                    </div>
                  </div>
                )}
              </div>

              {/* Exclude Patterns */}
              <div className="sc-setting-card full-width">
                <h3>Exclude Patterns</h3>
                <p>Segment names matching these patterns will never be cleaned up</p>
                <div className="sc-tags">
                  {settings.exclude_patterns.map((pattern) => (
                    <span key={pattern} className="sc-tag">
                      {pattern}
                      <button onClick={() => removePattern(pattern)}>×</button>
                    </span>
                  ))}
                </div>
                <div className="sc-add-input">
                  <input
                    type="text"
                    placeholder="e.g., system_%, test_%"
                    value={newPattern}
                    onChange={(e) => setNewPattern(e.target.value)}
                    onKeyPress={(e) => e.key === 'Enter' && addPattern()}
                  />
                  <button onClick={addPattern}>Add Pattern</button>
                </div>
              </div>
            </div>

            <div className="sc-save-bar">
              <button className="sc-btn sc-btn-primary sc-btn-large" onClick={saveSettings} disabled={saving}>
                {saving ? 'Saving...' : 'Save Settings'}
              </button>
            </div>
          </div>
        )}

        {/* History Tab */}
        {activeTab === 'history' && (
          <div className="sc-history">
            <div className="sc-section-header">
              <h2>Cleanup History</h2>
              <p>Record of cleanup actions taken</p>
            </div>
            
            {history.length === 0 ? (
              <div className="sc-empty">
                <span className="sc-empty-icon">📝</span>
                <p>No cleanup history yet</p>
              </div>
            ) : (
              <table className="sc-table">
                <thead>
                  <tr>
                    <th>Segment Name</th>
                    <th>Subscribers</th>
                    <th>Warning Sent</th>
                    <th>Action</th>
                    <th>Action Date</th>
                  </tr>
                </thead>
                <tbody>
                  {history.map((item, i) => (
                    <tr key={i}>
                      <td>{item.segment_name}</td>
                      <td>{item.subscriber_count.toLocaleString()}</td>
                      <td>{new Date(item.warning_sent_at).toLocaleDateString()}</td>
                      <td>
                        <span className={`sc-badge ${
                          item.action_taken === 'kept' ? 'sc-badge-green' : 
                          item.action_taken === 'archived' ? 'sc-badge-yellow' : 
                          'sc-badge-red'
                        }`}>
                          {item.action_taken}
                        </span>
                      </td>
                      <td>{new Date(item.action_taken_at).toLocaleString()}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        )}
      </div>
    </div>
  );
};

export default SegmentCleanup;
