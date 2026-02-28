import { useState, useEffect } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faTimes, faEnvelope, faUsers, faBullseye, faDollarSign, faArrowUp, faExclamationCircle, faCheckCircle } from '@fortawesome/free-solid-svg-icons';
import { EnrichedCampaignDetails, EnrichedCampaignResponse } from '../../types';

interface CampaignDetailsModalProps {
  mailingId: string;
  onClose: () => void;
}

export const CampaignDetailsModal: React.FC<CampaignDetailsModalProps> = ({
  mailingId,
  onClose,
}) => {
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [campaign, setCampaign] = useState<EnrichedCampaignDetails | null>(null);

  useEffect(() => {
    const fetchDetails = async () => {
      try {
        setLoading(true);
        setError(null);

        const response = await fetch(`/api/everflow/campaigns/${mailingId}`);
        if (!response.ok) {
          const errorData = await response.json().catch(() => ({}));
          throw new Error(errorData.error || `HTTP ${response.status}`);
        }

        const data: EnrichedCampaignResponse = await response.json();
        setCampaign(data.campaign);
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to fetch campaign details');
      } finally {
        setLoading(false);
      }
    };

    fetchDetails();
  }, [mailingId]);

  const formatCurrency = (value: number): string => {
    return new Intl.NumberFormat('en-US', {
      style: 'currency',
      currency: 'USD',
      minimumFractionDigits: 2,
    }).format(value);
  };

  const formatNumber = (value: number): string => {
    return new Intl.NumberFormat('en-US').format(value);
  };

  const formatPercent = (value: number): string => {
    return `${(value * 100).toFixed(2)}%`;
  };

  const formatDate = (dateStr: string): string => {
    if (!dateStr) return 'N/A';
    try {
      const date = new Date(dateStr);
      return date.toLocaleString();
    } catch {
      return dateStr;
    }
  };

  // Handle backdrop click
  const handleBackdropClick = (e: React.MouseEvent) => {
    if (e.target === e.currentTarget) {
      onClose();
    }
  };

  // Handle escape key
  useEffect(() => {
    const handleEscape = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        onClose();
      }
    };
    document.addEventListener('keydown', handleEscape);
    return () => document.removeEventListener('keydown', handleEscape);
  }, [onClose]);

  return (
    <div className="modal-backdrop" onClick={handleBackdropClick}>
      <div className="modal-content campaign-details-modal">
        {/* Header */}
        <div className="modal-header">
          <div className="modal-title">
            <h2>Campaign Details</h2>
            <span className="mailing-id">Mailing ID: {mailingId}</span>
          </div>
          <button className="modal-close" onClick={onClose} aria-label="Close">
            <FontAwesomeIcon icon={faTimes} />
          </button>
        </div>

        {/* Body */}
        <div className="modal-body">
          {loading && (
            <div className="modal-loading">
              <div className="spinner" />
              <p>Loading campaign details...</p>
            </div>
          )}

          {error && (
            <div className="modal-error">
              <FontAwesomeIcon icon={faExclamationCircle} />
              <p>{error}</p>
            </div>
          )}

          {campaign && !loading && (
            <>
              {/* Campaign Header */}
              <div className="campaign-header">
                <h3>{campaign.campaign_name || 'Campaign'}</h3>
                {campaign.subject && (
                  <p className="campaign-subject">
                    <FontAwesomeIcon icon={faEnvelope} />
                    {campaign.subject}
                  </p>
                )}
                <div className="campaign-status">
                  {campaign.ongage_linked ? (
                    <span className="status-linked">
                      <FontAwesomeIcon icon={faCheckCircle} /> Ongage Linked
                    </span>
                  ) : (
                    <span className="status-unlinked">
                      <FontAwesomeIcon icon={faExclamationCircle} /> {campaign.link_error || 'Ongage not linked'}
                    </span>
                  )}
                  {campaign.status_desc && (
                    <span className="status-badge">{campaign.status_desc}</span>
                  )}
                </div>
              </div>

              {/* Revenue & Performance Section */}
              <div className="details-section">
                <h4><FontAwesomeIcon icon={faDollarSign} /> Revenue Performance</h4>
                <div className="metrics-grid">
                  <div className="metric-item highlight">
                    <span className="metric-label">Total Revenue</span>
                    <span className="metric-value revenue">{formatCurrency(campaign.revenue)}</span>
                  </div>
                  <div className="metric-item highlight">
                    <span className="metric-label">eCPM</span>
                    <span className="metric-value">{formatCurrency(campaign.ecpm)}</span>
                    <span className="metric-subtitle">Revenue per 1000 targeted</span>
                  </div>
                  <div className="metric-item">
                    <span className="metric-label">Conversions</span>
                    <span className="metric-value">{formatNumber(campaign.conversions)}</span>
                  </div>
                  <div className="metric-item">
                    <span className="metric-label">Everflow Clicks</span>
                    <span className="metric-value">{formatNumber(campaign.clicks)}</span>
                  </div>
                  <div className="metric-item">
                    <span className="metric-label">Conversion Rate</span>
                    <span className="metric-value">{formatPercent(campaign.conversion_rate)}</span>
                  </div>
                  <div className="metric-item">
                    <span className="metric-label">Revenue Per Click</span>
                    <span className="metric-value">{formatCurrency(campaign.revenue_per_click)}</span>
                  </div>
                </div>
              </div>

              {/* Offer & Property Info */}
              <div className="details-section">
                <h4><FontAwesomeIcon icon={faBullseye} /> Offer & Property</h4>
                <div className="info-grid">
                  <div className="info-item">
                    <span className="info-label">Offer</span>
                    <span className="info-value">{campaign.offer_name || campaign.offer_id}</span>
                  </div>
                  <div className="info-item">
                    <span className="info-label">Property</span>
                    <span className="info-value">{campaign.property_name || campaign.property_code}</span>
                  </div>
                </div>
              </div>

              {/* Email Delivery Stats (if Ongage linked) */}
              {campaign.ongage_linked && (
                <div className="details-section">
                  <h4><FontAwesomeIcon icon={faEnvelope} /> Email Delivery</h4>
                  <div className="metrics-grid">
                    <div className="metric-item">
                      <span className="metric-label">Audience Size</span>
                      <span className="metric-value">{formatNumber(campaign.audience_size)}</span>
                    </div>
                    <div className="metric-item">
                      <span className="metric-label">Sent</span>
                      <span className="metric-value">{formatNumber(campaign.sent)}</span>
                    </div>
                    <div className="metric-item">
                      <span className="metric-label">Delivered</span>
                      <span className="metric-value">{formatNumber(campaign.delivered)}</span>
                      <span className="metric-subtitle">{formatPercent(campaign.delivery_rate)} rate</span>
                    </div>
                    <div className="metric-item">
                      <span className="metric-label">Unique Opens</span>
                      <span className="metric-value">{formatNumber(campaign.unique_opens)}</span>
                      <span className="metric-subtitle">{formatPercent(campaign.open_rate)} rate</span>
                    </div>
                    <div className="metric-item">
                      <span className="metric-label">Unique Clicks</span>
                      <span className="metric-value">{formatNumber(campaign.email_clicks)}</span>
                      <span className="metric-subtitle">{formatPercent(campaign.click_to_open_rate)} CTOR</span>
                    </div>
                    <div className="metric-item">
                      <span className="metric-label">Total Clicks</span>
                      <span className="metric-value">{formatNumber(campaign.total_email_clicks || 0)}</span>
                      <span className="metric-subtitle">incl. repeats</span>
                    </div>
                  </div>
                  
                  {/* Delivery Issues Breakdown */}
                  {(campaign.bounces > 0 || campaign.failed > 0) && (
                    <div className="delivery-issues">
                      <h5>Delivery Issues ({formatNumber(campaign.sent - campaign.delivered)} total)</h5>
                      <div className="issues-grid">
                        {campaign.hard_bounces > 0 && (
                          <div className="issue-item">
                            <span className="issue-label">Hard Bounces</span>
                            <span className="issue-value">{formatNumber(campaign.hard_bounces)}</span>
                          </div>
                        )}
                        {campaign.soft_bounces > 0 && (
                          <div className="issue-item">
                            <span className="issue-label">Soft Bounces</span>
                            <span className="issue-value">{formatNumber(campaign.soft_bounces)}</span>
                          </div>
                        )}
                        {campaign.failed > 0 && (
                          <div className="issue-item">
                            <span className="issue-label">Failed</span>
                            <span className="issue-value">{formatNumber(campaign.failed)}</span>
                          </div>
                        )}
                      </div>
                    </div>
                  )}
                </div>
              )}

              {/* Sending Configuration (if Ongage linked) */}
              {campaign.ongage_linked && (
                <div className="details-section">
                  <h4><FontAwesomeIcon icon={faArrowUp} /> Sending Configuration</h4>
                  <div className="info-grid">
                    <div className="info-item">
                      <span className="info-label">Sending Domain</span>
                      <span className="info-value">{campaign.sending_domain || 'N/A'}</span>
                    </div>
                    <div className="info-item">
                      <span className="info-label">ESP</span>
                      <span className="info-value">{campaign.esp_name || 'N/A'}</span>
                    </div>
                    <div className="info-item">
                      <span className="info-label">Scheduled</span>
                      <span className="info-value">{formatDate(campaign.schedule_date)}</span>
                    </div>
                    <div className="info-item">
                      <span className="info-label">Send Window</span>
                      <span className="info-value">
                        {formatDate(campaign.sending_start_date)} - {formatDate(campaign.sending_end_date)}
                      </span>
                    </div>
                  </div>
                </div>
              )}

              {/* Segments (if Ongage linked) */}
              {campaign.ongage_linked && (campaign.sending_segments?.length > 0 || campaign.suppression_segments?.length > 0) && (
                <div className="details-section">
                  <h4><FontAwesomeIcon icon={faUsers} /> Segments</h4>
                  
                  {campaign.sending_segments?.length > 0 && (
                    <div className="segment-group">
                      <h5>Sending Segments</h5>
                      <div className="segment-list">
                        {campaign.sending_segments.map((seg, idx) => (
                          <div key={idx} className="segment-item sending">
                            <span className="segment-name">{seg.name}</span>
                            {seg.count > 0 && (
                              <span className="segment-count">{formatNumber(seg.count)}</span>
                            )}
                          </div>
                        ))}
                      </div>
                    </div>
                  )}

                  {campaign.suppression_segments?.length > 0 && (
                    <div className="segment-group">
                      <h5>Suppression Segments</h5>
                      <div className="segment-list">
                        {campaign.suppression_segments.map((seg, idx) => (
                          <div key={idx} className="segment-item suppression">
                            <span className="segment-name">{seg.name}</span>
                            {seg.count > 0 && (
                              <span className="segment-count">{formatNumber(seg.count)}</span>
                            )}
                          </div>
                        ))}
                      </div>
                    </div>
                  )}
                </div>
              )}
            </>
          )}
        </div>
      </div>
    </div>
  );
};

export default CampaignDetailsModal;
