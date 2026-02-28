import React, { useEffect, useState } from 'react';
import { useCampaigns, useLists } from '../hooks/useMailingApi';
import type { Campaign, List } from '../types';
import './CampaignsManager.css';

interface CreateCampaignModalProps {
  isOpen: boolean;
  lists: List[];
  onClose: () => void;
  onSubmit: (campaign: Partial<Campaign>) => Promise<void>;
}

const CreateCampaignModal: React.FC<CreateCampaignModalProps> = ({
  isOpen,
  lists,
  onClose,
  onSubmit,
}) => {
  const [formData, setFormData] = useState({
    name: '',
    list_id: '',
    subject: '',
    from_name: '',
    from_email: '',
    html_content: '',
    plain_content: '',
    preview_text: '',
    ai_send_time_optimization: true,
    ai_content_optimization: false,
    ai_audience_optimization: false,
  });
  const [loading, setLoading] = useState(false);

  if (!isOpen) return null;

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setLoading(true);
    try {
      await onSubmit({
        ...formData,
        list_id: formData.list_id || null,
        campaign_type: 'regular',
      });
      onClose();
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal-content large" onClick={(e) => e.stopPropagation()}>
        <h2>Create Campaign</h2>
        <form onSubmit={handleSubmit}>
          <div className="form-row">
            <div className="form-group">
              <label>Campaign Name *</label>
              <input
                type="text"
                value={formData.name}
                onChange={(e) => setFormData({ ...formData, name: e.target.value })}
                required
                placeholder="e.g., January Newsletter"
              />
            </div>
            <div className="form-group">
              <label>List</label>
              <select
                value={formData.list_id}
                onChange={(e) => setFormData({ ...formData, list_id: e.target.value })}
              >
                <option value="">Select a list</option>
                {lists.map((list) => (
                  <option key={list.id} value={list.id}>
                    {list.name} ({list.subscriber_count.toLocaleString()} subscribers)
                  </option>
                ))}
              </select>
            </div>
          </div>

          <div className="form-group">
            <label>Subject Line *</label>
            <input
              type="text"
              value={formData.subject}
              onChange={(e) => setFormData({ ...formData, subject: e.target.value })}
              required
              placeholder="Your email subject"
            />
          </div>

          <div className="form-row">
            <div className="form-group">
              <label>From Name</label>
              <input
                type="text"
                value={formData.from_name}
                onChange={(e) => setFormData({ ...formData, from_name: e.target.value })}
                placeholder="Sender Name"
              />
            </div>
            <div className="form-group">
              <label>From Email</label>
              <input
                type="email"
                value={formData.from_email}
                onChange={(e) => setFormData({ ...formData, from_email: e.target.value })}
                placeholder="sender@example.com"
              />
            </div>
          </div>

          <div className="form-group">
            <label>Preview Text</label>
            <input
              type="text"
              value={formData.preview_text}
              onChange={(e) => setFormData({ ...formData, preview_text: e.target.value })}
              placeholder="Text shown in email preview"
            />
          </div>

          <div className="form-group">
            <label>HTML Content</label>
            <textarea
              value={formData.html_content}
              onChange={(e) => setFormData({ ...formData, html_content: e.target.value })}
              placeholder="<html>...</html>"
              rows={6}
            />
          </div>

          <div className="form-group">
            <label>Plain Text Content</label>
            <textarea
              value={formData.plain_content}
              onChange={(e) => setFormData({ ...formData, plain_content: e.target.value })}
              placeholder="Plain text version of your email"
              rows={4}
            />
          </div>

          <div className="ai-options">
            <h3>AI Optimization</h3>
            <div className="checkbox-group">
              <label>
                <input
                  type="checkbox"
                  checked={formData.ai_send_time_optimization}
                  onChange={(e) =>
                    setFormData({ ...formData, ai_send_time_optimization: e.target.checked })
                  }
                />
                <span>Send Time Optimization</span>
                <small>AI determines optimal send time for each subscriber</small>
              </label>
              <label>
                <input
                  type="checkbox"
                  checked={formData.ai_content_optimization}
                  onChange={(e) =>
                    setFormData({ ...formData, ai_content_optimization: e.target.checked })
                  }
                />
                <span>Content Optimization</span>
                <small>AI optimizes subject lines and content</small>
              </label>
              <label>
                <input
                  type="checkbox"
                  checked={formData.ai_audience_optimization}
                  onChange={(e) =>
                    setFormData({ ...formData, ai_audience_optimization: e.target.checked })
                  }
                />
                <span>Audience Optimization</span>
                <small>AI selects optimal audience segments</small>
              </label>
            </div>
          </div>

          <div className="modal-actions">
            <button type="button" className="cancel-btn" onClick={onClose}>
              Cancel
            </button>
            <button type="submit" className="submit-btn" disabled={loading}>
              {loading ? 'Creating...' : 'Create Campaign'}
            </button>
          </div>
        </form>
      </div>
    </div>
  );
};

const getStatusInfo = (status: string) => {
  const statusMap: Record<string, { color: string; label: string }> = {
    draft: { color: '#6b7280', label: 'Draft' },
    queued: { color: '#f59e0b', label: 'Queued' },
    sending: { color: '#3b82f6', label: 'Sending' },
    sent: { color: '#22c55e', label: 'Sent' },
    paused: { color: '#f59e0b', label: 'Paused' },
    failed: { color: '#ef4444', label: 'Failed' },
  };
  return statusMap[status] || { color: '#6b7280', label: status };
};

export const CampaignsManager: React.FC = () => {
  const { campaigns, loading, error, fetchCampaigns, createCampaign, sendCampaign } = useCampaigns();
  const { lists, fetchLists } = useLists();
  const [showCreateModal, setShowCreateModal] = useState(false);
  const [sending, setSending] = useState<string | null>(null);

  useEffect(() => {
    fetchCampaigns();
    fetchLists();
  }, [fetchCampaigns, fetchLists]);

  const handleCreateCampaign = async (campaign: Partial<Campaign>) => {
    await createCampaign(campaign);
  };

  const handleSend = async (campaignId: string) => {
    if (!confirm('Are you sure you want to send this campaign?')) return;
    
    setSending(campaignId);
    try {
      const result = await sendCampaign(campaignId);
      alert(`Campaign sending initiated! ${result.total_recipients} recipients queued.`);
      fetchCampaigns();
    } catch (error) {
      alert('Failed to send campaign');
    } finally {
      setSending(null);
    }
  };

  const calculateRate = (count: number, total: number) => {
    if (total === 0) return 0;
    return ((count / total) * 100).toFixed(1);
  };

  return (
    <div className="campaigns-manager">
      <header className="campaigns-header">
        <div>
          <h1>Campaigns</h1>
          <p>Create and manage your email campaigns</p>
        </div>
        <button className="create-btn" onClick={() => setShowCreateModal(true)}>
          + Create Campaign
        </button>
      </header>

      {error && <div className="error-message">{error}</div>}

      {loading && campaigns.length === 0 && (
        <div className="loading-state">Loading campaigns...</div>
      )}

      <div className="campaigns-list">
        {campaigns.map((campaign) => {
          const statusInfo = getStatusInfo(campaign.status);
          return (
            <div key={campaign.id} className="campaign-card">
              <div className="campaign-main">
                <div className="campaign-info">
                  <h3>{campaign.name}</h3>
                  <p className="campaign-subject">{campaign.subject}</p>
                  <div className="campaign-meta">
                    <span
                      className="status-badge"
                      style={{ backgroundColor: statusInfo.color + '20', color: statusInfo.color }}
                    >
                      {statusInfo.label}
                    </span>
                    <span className="campaign-date">
                      Created {new Date(campaign.created_at).toLocaleDateString()}
                    </span>
                  </div>
                </div>

                <div className="campaign-stats">
                  <div className="stat-item">
                    <span className="stat-label">Sent</span>
                    <span className="stat-value">{campaign.sent_count.toLocaleString()}</span>
                  </div>
                  <div className="stat-item">
                    <span className="stat-label">Opens</span>
                    <span className="stat-value">
                      {campaign.open_count.toLocaleString()}
                      <span className="stat-rate">
                        ({calculateRate(campaign.open_count, campaign.sent_count)}%)
                      </span>
                    </span>
                  </div>
                  <div className="stat-item">
                    <span className="stat-label">Clicks</span>
                    <span className="stat-value">
                      {campaign.click_count.toLocaleString()}
                      <span className="stat-rate">
                        ({calculateRate(campaign.click_count, campaign.sent_count)}%)
                      </span>
                    </span>
                  </div>
                  <div className="stat-item">
                    <span className="stat-label">Revenue</span>
                    <span className="stat-value revenue">${campaign.revenue.toFixed(2)}</span>
                  </div>
                </div>

                <div className="campaign-actions">
                  {campaign.status === 'draft' && (
                    <button
                      className="send-btn"
                      onClick={() => handleSend(campaign.id)}
                      disabled={sending === campaign.id}
                    >
                      {sending === campaign.id ? 'Sending...' : 'Send'}
                    </button>
                  )}
                  {campaign.status === 'sending' && (
                    <div className="sending-indicator">
                      <div className="pulse" />
                      Sending...
                    </div>
                  )}
                  <button className="view-btn">View Details</button>
                </div>
              </div>

              {campaign.sent_count > 0 && (
                <div className="campaign-progress">
                  <div className="progress-bar">
                    <div
                      className="progress-fill open"
                      style={{ width: `${calculateRate(campaign.open_count, campaign.sent_count)}%` }}
                    />
                  </div>
                  <div className="progress-labels">
                    <span>Open Rate: {calculateRate(campaign.open_count, campaign.sent_count)}%</span>
                    <span>Click Rate: {calculateRate(campaign.click_count, campaign.sent_count)}%</span>
                    <span>Bounce Rate: {calculateRate(campaign.bounce_count, campaign.sent_count)}%</span>
                  </div>
                </div>
              )}
            </div>
          );
        })}
      </div>

      <CreateCampaignModal
        isOpen={showCreateModal}
        lists={lists}
        onClose={() => setShowCreateModal(false)}
        onSubmit={handleCreateCampaign}
      />
    </div>
  );
};

export default CampaignsManager;
