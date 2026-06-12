import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { PersonalizedInput } from './PersonalizedInput';
import {
  faEnvelope,
  faChartBar,
  faChartPie,
  faPaperPlane,
  faCalendarAlt,
  faClock,
  faPlay,
  faPause,
  faStop,
  faEye,
  faEdit,
  faCopy,
  faSync,
  faCheckCircle,
  faTimesCircle,
  faExclamationTriangle,
  faSpinner,
  faEnvelopeOpen,
  faMousePointer,
  faExclamationCircle,
  faUserMinus,
  faBan,
  faArrowUp,
  faArrowDown,
  faArrowRight,
  faTrophy,
  faUsers,
  faPercentage,
  faFileAlt,
  faTachometerAlt,
  faList,
  faCalendarCheck,
  faInfoCircle,
  faTimes,
  faSave,
  faBullseye,
  faCode,
  faChevronDown,
  faChevronUp,
} from '@fortawesome/free-solid-svg-icons';
import { useAuth } from '../../../contexts/AuthContext';
import { AnimatedCounter } from '../shared/AnimatedCounter';
import CampaignCenterList, { ListStatusFilter } from './campaign-center/CampaignCenterList';
import './CampaignPortal.css';

// ============================================================================
// TYPES
// ============================================================================

interface Campaign {
  id: string;
  name: string;
  subject: string;
  status: 'draft' | 'scheduled' | 'preparing' | 'sending' | 'paused' | 'sent' | 'completed' | 'completed_with_errors' | 'cancelled' | 'failed';
  total_recipients: number;
  sent_count: number;
  open_count: number;
  click_count: number;
  bounce_count: number;
  hard_bounce_count: number;
  soft_bounce_count: number;
  complaint_count: number;
  unsubscribe_count: number;
  queued_count?: number;
  scheduled_at?: string;
  started_at?: string;
  completed_at?: string;
  created_at: string;
  updated_at: string;
  list_name?: string;
  list_names?: string[];
  segment_name?: string;
  segment_names?: string[];
  profile_name?: string;
  profile_vendor?: string;
  revenue?: number;
  from_name?: string;
  from_email?: string;
  throttle_speed?: string;
  preview_text?: string;
  html_preview?: string;
}

interface CampaignVariant {
  id: string;
  variant_name: string;
  subject: string;
  from_name: string;
  html_content: string;
  split_percent: number;
  sent_count: number;
  open_count: number;
  click_count: number;
  is_winner: boolean;
  open_rate: number;
  click_rate: number;
}

// Row shape returned by GET /api/mailing/analytics/campaign-summary.
// This is the fast, accurate list data source — it carries pre-aggregated
// delivery/engagement counts and explicit denominators, and crucially does
// NOT include html_content / creative bodies (the old /campaigns list pulled
// those per-row, which timed out and rendered zeros).
interface CampaignSummaryRow {
  id: string;
  name: string;
  status: Campaign['status'];
  scheduled_at?: string;
  updated_at?: string;
  route_type: string; // "ses" | "pmta" (delivery basis)
  targeted: number;   // planned audience
  sent: number;       // attempted/injected
  delivered: number;  // SES DELIVERY (ses) / PMTA direct (pmta); relay-to-SES excluded
  hard_bounce: number;
  soft_bounce: number;
  complaints: number;
  opens: number;
  clicks: number;
  unsubscribes: number;
  delivery_rate: number;
  hard_bounce_rate: number;
  soft_bounce_rate: number;
  complaint_rate: number;
  open_rate: number;
  click_rate: number;
  ctor: number;
  // Rollup markers. When is_rollup is true this row aggregates wave_count
  // partner-drip mini-campaigns sharing partner_tag; id is the synthetic
  // "drip-rollup:<tag>" so the card drills (partner_tag) instead of opening
  // a per-campaign detail.
  is_rollup?: boolean;
  wave_count?: number;
  partner_tag?: string;
}

interface CampaignSummaryResponse {
  api_version?: string;
  generated_at?: string;
  data_as_of?: string;
  count?: number;
  campaigns?: CampaignSummaryRow[];
  denominators?: Record<string, string>;
  notes?: string;
  rolled_up?: boolean;
  partner_tag?: string;
  error?: string;
}

// ----------------------------------------------------------------------------
// DETAIL analytics: GET /api/mailing/analytics/campaign-summary/{id}
//
// This is the per-campaign companion to the list summary. It carries the same
// accurate, tracking-derived delivery/engagement counts PLUS an audience funnel
// (how the planned audience was whittled down by suppression/dedup/quota) and a
// terminal delivery breakdown. It's additive: the legacy /stats panels stay.
// ----------------------------------------------------------------------------

// Per-reason suppression counts emitted by the audience finalizer. Keys mirror
// the finalizer's filter chain (see send-day-process.mdc §4).
interface CampaignFunnelReasonBreakdown {
  global_or_brand_suppression: number;
  dedup_or_empty: number;
  isp_quota_cap: number;
  offer_suppression: number;
  exclusion_list: number;
  excluded_segment: number;
  recently_mailed: number;
  sds_cold_suppressed: number;
  sds_recent_24h: number;
  isp_not_allowed: number;
}

interface CampaignAudienceFunnel {
  total_seen: number;
  targeted: number;
  reserve: number;
  suppressed_total: number;
  reason_breakdown: CampaignFunnelReasonBreakdown;
  computed_at: string;
}

// The accurate, tracking-derived campaign metrics block. `route_type` decides
// whether `relayed_to_ses` is meaningful (SES route only — it's a PMTA→SES
// handoff, NOT a delivery).
interface CampaignSummaryDetailCampaign {
  id: string;
  name: string;
  status: Campaign['status'];
  route_type: 'ses' | 'pmta';
  scheduled_at?: string;
  created_at?: string;
  // Content + sender metadata (v1.4).
  subject?: string;
  preheader?: string;
  from_name?: string;
  from_email?: string;
  campaign_type?: string;
  sending_profile?: string;
  sending_domain?: string;
  ip_pool?: string;
  vendor?: string;
  targeted: number;
  sent: number;
  relayed_to_ses: number;
  recipients: number;
  delivered: number;
  hard_bounce: number;
  // Reputation/system block of a VALID recipient (DSN 5.3/5.4/5.5/5.7 +
  // policy/routing/connection). NOT list hygiene. Added v1.4.
  reputation_block: number;
  soft_bounce: number;
  deferred_open: number;
  sent_only: number;
  unique_opens: number;
  total_opens: number;
  unique_clicks: number;
  total_clicks: number;
  delivery_rate: number;
  hard_bounce_rate: number;
  reputation_block_rate: number;
  soft_bounce_rate: number;
  open_rate: number;
  click_rate: number;
  ctor: number;
  metrics_source: string;
  data_as_of: string;
  generated_at: string;
}

interface CampaignSummaryDetail {
  api_version?: string;
  campaign: CampaignSummaryDetailCampaign;
  // null when the campaign was finalized before funnel tracking existed.
  audience_funnel: CampaignAudienceFunnel | null;
  denominators?: Record<string, string>;
  error?: string;
}

// Human-readable labels for the funnel reason keys, used when rendering the
// suppression breakdown rows.
const FUNNEL_REASON_LABELS: Record<keyof CampaignFunnelReasonBreakdown, string> = {
  global_or_brand_suppression: 'Global/Brand Suppression',
  recently_mailed: 'Recently Mailed',
  isp_quota_cap: 'ISP Quota Cap',
  dedup_or_empty: 'Dedup/Empty',
  offer_suppression: 'Offer Suppression',
  exclusion_list: 'Exclusion List',
  excluded_segment: 'Excluded Segment',
  sds_cold_suppressed: 'SDS Cold/Suppressed',
  sds_recent_24h: 'SDS Recently Mailed (24h)',
  isp_not_allowed: 'ISP Not Allowed',
};

// Minimum preparation time in minutes (must match backend)
const MIN_PREPARATION_MINUTES = 5;

// Helper function to check if a campaign can be edited.
// Accepts the minimal shape shared by Campaign and CampaignSummaryRow so the
// repointed list cards can reuse the exact same edit-lock logic.
const canEditCampaign = (campaign: Pick<Campaign, 'status' | 'scheduled_at'>): boolean => {
  // Can always edit drafts
  if (campaign.status === 'draft') return true;
  
  // Cannot edit preparing, sending, completed, cancelled, failed
  if (['preparing', 'sending', 'sent', 'completed', 'completed_with_errors', 'cancelled', 'failed', 'paused'].includes(campaign.status)) {
    return false;
  }
  
  // For scheduled campaigns, check edit lock window
  if (campaign.status === 'scheduled' && campaign.scheduled_at) {
    const scheduledTime = new Date(campaign.scheduled_at);
    const editLockTime = new Date(scheduledTime.getTime() - MIN_PREPARATION_MINUTES * 60 * 1000);
    return new Date() < editLockTime;
  }
  
  return false;
};

// Helper function to get edit lock info
const getEditLockInfo = (campaign: Campaign): { isLocked: boolean; lockTime?: Date; message?: string } => {
  if (campaign.status !== 'scheduled' || !campaign.scheduled_at) {
    return { isLocked: false };
  }
  
  const scheduledTime = new Date(campaign.scheduled_at);
  const editLockTime = new Date(scheduledTime.getTime() - MIN_PREPARATION_MINUTES * 60 * 1000);
  const isLocked = new Date() >= editLockTime;
  
  if (isLocked) {
    return {
      isLocked: true,
      lockTime: editLockTime,
      message: `Campaign locked for preparation. Scheduled to send at ${scheduledTime.toLocaleString()}`
    };
  }
  
  return {
    isLocked: false,
    lockTime: editLockTime,
    message: `Can edit until ${editLockTime.toLocaleString()}`
  };
};

interface ISPBreakdownEntry {
  isp: string;
  quota?: number;
  sent: number;
  delivered: number;
  opens: number;
  clicks: number;
  hard_bounces: number;
  soft_bounces: number;
  complaints: number;
  open_rate: number;
  click_rate: number;
  planned_recipients?: number;
  enqueued_recipients?: number;
}

interface CampaignStats {
  sent: number;
  delivered?: number;
  deferred?: number;
  opens: number;
  clicks: number;
  hard_bounces: number;
  soft_bounces: number;
  complaints: number;
  unsubscribes: number;
  open_rate: number;
  click_rate: number;
  hard_bounce_rate: number;
  soft_bounce_rate: number;
  complaint_rate: number;
  unsubscribe_rate: number;
  deferral_rate?: number;
  isp_breakdown?: ISPBreakdownEntry[];
  domain_breakdown?: Array<{ domain: string; sent: number; delivered: number; opens: number; clicks: number }>;
  // Phase B: queue_status maps mailing_campaign_queue.status -> count.
  // Surfaces "what's actually in flight right now" without bouncing to Outbox.
  queue_status?: Record<string, number>;
  api_version?: string;
}

type ViewType = 'campaigns' | 'details' | 'create' | 'edit';

const API_BASE = '/api/mailing';

// PAGE_VERSION is bumped on every Campaign Center behavior/UX change per
// workspace rule testing.mdc.
//
// History:
//   1.0 (2026-05-08) — Phase B remediation:
//     * Total Campaigns now reads pagination.total (was page length, max 200)
//     * Top Performers now includes 'sent' / 'completed' / 'completed_with_errors'
//     * Open / click rate denominator switched to `delivered` for parity
//       with the Deliverability screen and the backend stats endpoint
//     * Stats refetch on org change (was only on mount)
//     * Hard/Soft bounce shown as separate tiles (was already true; reaffirmed)
//     * Per-campaign queue-depth surfaced in detail (added to /stats payload)
//   1.1 (2026-06-04) — Phase 0 analytics rebuild:
//     * LIST view repointed from /campaigns?limit=200 (pulled full html_content
//       per row → slow/timeout/zeros) to /analytics/campaign-summary?limit=200
//     * List cards now render pre-aggregated counts with explicit denominators
//       (delivery of sent, opens/clicks of delivered, hard/soft bounce of sent)
//     * Added freshness indicator (data_as_of / X-Cache-Age-Seconds),
//       SES/PMTA route badge, and an inline error state with retry
//     * List no longer depends on per-campaign creative bodies; dashboard
//       aggregates + detail modal are unchanged
//   1.2 (2026-06-04) — Phase 1 detail analytics:
//     * Detail modal now ALSO fetches /analytics/campaign-summary/{id} in
//       parallel with the legacy /campaigns/{id} + /stats + /variants loads
//     * Added an "Audience Funnel" panel (Total Seen → Targeted → Suppressed
//       with a descending reason breakdown; Global/Brand Suppression emphasized)
//     * Added a "Delivery Terminal State" panel from the accurate tracking-
//       derived counts (delivered / hard / soft / deferred / sent-only /
//       recipients, SES relay-to-SES with tooltip, rates w/ explicit denoms,
//       route badge + data_as_of freshness)
//     * Purely additive — existing /stats-based panels are untouched; new
//       endpoint failures degrade to an inline notice, never a crash
//   1.3 (2026-06-04) — Partner-drip rollup:
//     * The list now renders the backend's partner-drip rollup rows
//       (is_rollup=true): one card per partner feed (ROLLUP badge + wave
//       count) instead of thousands of 15-min mini-campaign waves that
//       otherwise bury every real campaign past the limit
//     * Rollup cards hide per-campaign mutations (edit/duplicate/pause/cancel)
//       and instead drill: opening one re-fetches the list scoped to that
//       partner_tag (individual waves, real uuids) with a "Back to rollups"
//       banner; the background poller preserves the drill via a ref
//   1.4 (2026-06-12) — Dashboard subtab facelift:
//     * Dashboard now reads /analytics/campaign-summary rows (drip waves
//       rolled up) instead of the /campaigns page aggregate — one coherent
//       hero (campaigns / sent / delivered% / opens / clicks / hard / soft)
//       labeled with its true 90-day scope; the garbled perf-intel bar,
//       duplicated rate tiles, and "from last 200" sample rates are removed
//     * Top Performers exclude rollups + '[partner-drip]' waves and require
//       sent >= 500 (tiny drip waves at 100% opens can no longer rank);
//       sent count shown alongside rates
//     * Recent Campaigns excludes rollups + '[partner-drip]' waves — the
//       drip program has its own screen
//   2.0 (2026-06-12) — Campaign Center rebuild (Round-3 §1):
//     * The card/hero Dashboard + card-grid All Campaigns are REPLACED by one
//       dense sortable/filterable LIST (campaign-center/CampaignCenterList)
//       with inline accordion analytics per row (campaign-center/
//       CampaignExpand): recharts send-pace graph with wave markers, KPI
//       strip, per-ISP mini-table, top links, content/segment/throttle facts
//     * Metric columns now come from /campaigns/list-metrics — ONE grouped
//       mailing_tracking_events scan per visible page (≤50 ids); the
//       campaign-summary endpoint is consumed for IDENTITY/STATUS ONLY
//       (its counter-derived metrics are no longer rendered anywhere here)
//     * Filters: date range (default today, Denver), status, sending domain,
//       offer text, drip toggle (default OFF; ON = one rollup row per
//       partner_drip vertical tag), group-by-offer mode with top subject
//       lines ranked by human CTR
//     * The legacy detail modal is KEPT (reachable via "Full record" in the
//       inline expand) because it still owns creative variants, the audience
//       funnel, queue depth and pause/cancel/edit actions — not yet at parity
const PAGE_VERSION_CAMPAIGN_PORTAL = '2.0';

// ============================================================================
// API HELPER WITH ORGANIZATION CONTEXT
// ============================================================================

/**
 * Creates headers with organization context for authenticated API calls
 */
const getOrgHeaders = (organizationId: string | undefined): HeadersInit => {
  const headers: HeadersInit = {
    'Content-Type': 'application/json',
  };
  if (organizationId) {
    headers['X-Organization-ID'] = organizationId;
  }
  return headers;
};

/**
 * Fetch wrapper that includes organization context
 */
const orgFetch = async (
  url: string,
  organizationId: string | undefined,
  options: RequestInit = {}
): Promise<Response> => {
  return fetch(url, {
    ...options,
    headers: {
      ...getOrgHeaders(organizationId),
      ...(options.headers || {}),
    },
    credentials: 'include',
  });
};

// ============================================================================
// HELPER COMPONENTS
// ============================================================================

const StatusBadge: React.FC<{ status: Campaign['status'] }> = ({ status }) => {
  const statusConfig: Record<string, { icon: any; label: string; className: string }> = {
    draft: { icon: faFileAlt, label: 'Draft', className: 'status-draft' },
    scheduled: { icon: faCalendarAlt, label: 'Scheduled', className: 'status-scheduled' },
    preparing: { icon: faClock, label: 'Preparing', className: 'status-preparing' },
    sending: { icon: faSpinner, label: 'Sending', className: 'status-sending' },
    paused: { icon: faPause, label: 'Paused', className: 'status-paused' },
    sent: { icon: faCheckCircle, label: 'Sent', className: 'status-completed' },
    completed: { icon: faCheckCircle, label: 'Completed', className: 'status-completed' },
    completed_with_errors: { icon: faExclamationTriangle, label: 'Completed w/ Errors', className: 'status-warning' },
    cancelled: { icon: faTimesCircle, label: 'Cancelled', className: 'status-cancelled' },
    failed: { icon: faExclamationCircle, label: 'Failed', className: 'status-failed' },
  };

  const config = statusConfig[status] || statusConfig.draft;

  return (
    <span className={`campaign-status-badge ${config.className}`}>
      {status === 'sending' && <span className="ig-pulse-dot" />}
      <FontAwesomeIcon icon={config.icon} spin={status === 'sending' || status === 'preparing'} />
      {config.label}
    </span>
  );
};

// ============================================================================
// CAMPAIGN DETAILS MODAL
// ============================================================================

const CampaignDetailsModal: React.FC<{
  campaign: Campaign | null;
  stats: CampaignStats | null;
  variants: CampaignVariant[];
  loading: boolean;
  // Phase 1 detail analytics (additive) — accurate tracking-derived metrics +
  // audience funnel from /analytics/campaign-summary/{id}. May be null if the
  // fetch failed or hasn't resolved; `summaryError` carries the failure reason.
  summaryDetail: CampaignSummaryDetail | null;
  summaryError: string | null;
  onClose: () => void;
  onAction: (id: string, action: string) => void;
}> = ({ campaign, stats, variants, loading, summaryDetail, summaryError, onClose, onAction }) => {
  const [activeVariantIdx, setActiveVariantIdx] = useState(0);
  const [variantPreviewOpen, setVariantPreviewOpen] = useState(false);

  if (!campaign) return null;

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="campaign-details-modal" onClick={e => e.stopPropagation()}>
        <div className="modal-header">
          <div className="modal-title">
            <h2>{campaign.name}</h2>
            <StatusBadge status={campaign.status} />
          </div>
          <button className="close-btn" onClick={onClose}>&times;</button>
        </div>

        <div className="modal-body">
          {loading ? (
            <div className="loading-state">
              <FontAwesomeIcon icon={faSpinner} spin size="2x" />
              <p>Loading campaign details...</p>
            </div>
          ) : (
            <>
              {/* Campaign Info */}
              <div className="details-section">
                <h3><FontAwesomeIcon icon={faInfoCircle} /> Campaign Info</h3>
                <div className="info-grid">
                  <div className="info-item">
                    <span className="info-label">Subject</span>
                    <span className="info-value">{campaign.subject}</span>
                  </div>
                  {campaign.preview_text && (
                    <div className="info-item">
                      <span className="info-label">Preheader</span>
                      <span className="info-value" style={{ fontStyle: 'italic', opacity: 0.7 }}>{campaign.preview_text}</span>
                    </div>
                  )}
                  <div className="info-item">
                    <span className="info-label">From</span>
                    <span className="info-value">{campaign.from_name} &lt;{campaign.from_email}&gt;</span>
                  </div>
                  <div className="info-item">
                    <span className="info-label">Sending Profile</span>
                    <span className="info-value">{campaign.profile_name || 'Default'}</span>
                  </div>
                  <div className="info-item">
                    <span className="info-label">Recipients</span>
                    <span className="info-value">{campaign.total_recipients?.toLocaleString() || 0}</span>
                  </div>
                  {campaign.scheduled_at && (
                    <div className="info-item">
                      <span className="info-label">Scheduled</span>
                      <span className="info-value">{new Date(campaign.scheduled_at).toLocaleString()}</span>
                    </div>
                  )}
                  <div className="info-item">
                    <span className="info-label">Created</span>
                    <span className="info-value">{new Date(campaign.created_at).toLocaleString()}</span>
                  </div>
                </div>
                {/* Audience: Lists + Segments */}
                {((campaign.list_names && campaign.list_names.length > 0) || (campaign.segment_names && campaign.segment_names.length > 0)) && (
                  <div style={{ marginTop: '14px' }}>
                    <span className="info-label" style={{ display: 'block', marginBottom: '8px' }}>Audience</span>
                    <div className="campaign-audience-badges">
                      {campaign.list_names?.map((ln, i) => (
                        <span key={`list-${i}`} className="audience-badge list-badge">
                          <FontAwesomeIcon icon={faUsers} /> {ln}
                        </span>
                      ))}
                      {campaign.segment_names?.map((sn, i) => (
                        <span key={`seg-${i}`} className="audience-badge segment-badge">
                          <FontAwesomeIcon icon={faBullseye} /> {sn}
                        </span>
                      ))}
                    </div>
                  </div>
                )}
              </div>

              {/* Content Variants (A/B/C/D creatives from EDITH) */}
              {variants.length >= 2 && (() => {
                const activeVariant = variants[activeVariantIdx] || variants[0];
                const hasSendData = variants.some(v => v.sent_count > 0);
                return (
                  <div className="details-section">
                    <style>{`
                      .cv-header { display: flex; align-items: center; gap: 10px; margin-bottom: 0; }
                      .cv-badge { background: rgba(99,102,241,0.15); color: #818cf8; padding: 2px 10px; border-radius: 12px; font-size: 11px; font-weight: 700; letter-spacing: 0.3px; }
                      .cv-tabs { display: flex; gap: 4px; margin: 14px 0 16px; }
                      .cv-tab { padding: 7px 18px; border-radius: 8px; border: 1px solid rgba(99,102,241,0.15); background: rgba(99,102,241,0.04); color: rgba(180,210,240,0.6); font-size: 13px; font-weight: 600; cursor: pointer; transition: all 0.15s; display: flex; align-items: center; gap: 6px; }
                      .cv-tab:hover { background: rgba(99,102,241,0.08); color: #c7d2fe; border-color: rgba(99,102,241,0.25); }
                      .cv-tab.active { background: rgba(99,102,241,0.18); color: #a5b4fc; border-color: rgba(99,102,241,0.4); }
                      .cv-tab .cv-winner-dot { width: 8px; height: 8px; border-radius: 50%; background: #22c55e; display: inline-block; }
                      .cv-card { background: rgba(15,23,42,0.6); border: 1px solid rgba(99,102,241,0.1); border-radius: 10px; padding: 16px; }
                      .cv-field { margin-bottom: 12px; }
                      .cv-field-label { font-size: 11px; color: rgba(180,210,240,0.4); text-transform: uppercase; letter-spacing: 0.5px; font-weight: 600; margin-bottom: 4px; }
                      .cv-field-value { font-size: 14px; color: #e0e6f0; line-height: 1.4; }
                      .cv-split-bar { height: 6px; border-radius: 3px; background: rgba(99,102,241,0.1); margin-top: 6px; overflow: hidden; }
                      .cv-split-fill { height: 100%; border-radius: 3px; background: linear-gradient(90deg, #818cf8, #6366f1); transition: width 0.3s; }
                      .cv-metrics-row { display: grid; grid-template-columns: repeat(3, 1fr); gap: 10px; margin-top: 14px; padding-top: 14px; border-top: 1px solid rgba(99,102,241,0.08); }
                      .cv-metric { text-align: center; }
                      .cv-metric-val { font-size: 18px; font-weight: 700; color: #e0e6f0; }
                      .cv-metric-lbl { font-size: 11px; color: rgba(180,210,240,0.45); margin-top: 2px; }
                      .cv-preview-toggle { margin-top: 14px; padding-top: 14px; border-top: 1px solid rgba(99,102,241,0.08); }
                      .cv-preview-btn { background: rgba(99,102,241,0.08); border: 1px solid rgba(99,102,241,0.15); color: #a5b4fc; padding: 7px 16px; border-radius: 7px; cursor: pointer; font-size: 12px; font-weight: 600; display: flex; align-items: center; gap: 6px; transition: all 0.15s; }
                      .cv-preview-btn:hover { background: rgba(99,102,241,0.14); border-color: rgba(99,102,241,0.3); }
                      .cv-preview-frame { margin-top: 12px; border-radius: 8px; overflow: hidden; border: 1px solid rgba(99,102,241,0.1); background: #fff; }
                      .cv-preview-frame iframe { width: 100%; height: 500px; border: none; }
                      .cv-winner-badge { display: inline-flex; align-items: center; gap: 5px; background: rgba(34,197,94,0.12); color: #22c55e; padding: 3px 10px; border-radius: 6px; font-size: 11px; font-weight: 700; margin-left: 8px; }
                    `}</style>
                    <h3 className="cv-header">
                      <FontAwesomeIcon icon={faCopy} /> Content Variants
                      <span className="cv-badge">{variants.length} variants</span>
                    </h3>
                    <div className="cv-tabs">
                      {variants.map((v, i) => (
                        <button
                          key={v.id}
                          className={`cv-tab ${i === activeVariantIdx ? 'active' : ''}`}
                          onClick={() => { setActiveVariantIdx(i); setVariantPreviewOpen(false); }}
                        >
                          {v.variant_name || `Variant ${i + 1}`}
                          {v.is_winner && <span className="cv-winner-dot" />}
                        </button>
                      ))}
                    </div>
                    <div className="cv-card">
                      <div className="cv-field">
                        <div className="cv-field-label">Subject Line</div>
                        <div className="cv-field-value">{activeVariant.subject}</div>
                      </div>
                      <div className="cv-field">
                        <div className="cv-field-label">From Name</div>
                        <div className="cv-field-value">{activeVariant.from_name}</div>
                      </div>
                      <div className="cv-field">
                        <div className="cv-field-label">Traffic Split</div>
                        <div className="cv-field-value">{activeVariant.split_percent.toFixed(0)}%</div>
                        <div className="cv-split-bar">
                          <div className="cv-split-fill" style={{ width: `${activeVariant.split_percent}%` }} />
                        </div>
                      </div>
                      {activeVariant.is_winner && (
                        <span className="cv-winner-badge">
                          <FontAwesomeIcon icon={faTrophy} /> Winner
                        </span>
                      )}
                      {hasSendData && (
                        <div className="cv-metrics-row">
                          <div className="cv-metric">
                            <div className="cv-metric-val">{activeVariant.sent_count.toLocaleString()}</div>
                            <div className="cv-metric-lbl">Sent</div>
                          </div>
                          <div className="cv-metric">
                            <div className="cv-metric-val">{activeVariant.open_count.toLocaleString()}</div>
                            <div className="cv-metric-lbl">Opens ({activeVariant.open_rate.toFixed(1)}%)</div>
                          </div>
                          <div className="cv-metric">
                            <div className="cv-metric-val">{activeVariant.click_count.toLocaleString()}</div>
                            <div className="cv-metric-lbl">Clicks ({activeVariant.click_rate.toFixed(1)}%)</div>
                          </div>
                        </div>
                      )}
                      <div className="cv-preview-toggle">
                        <button
                          className="cv-preview-btn"
                          onClick={() => setVariantPreviewOpen(!variantPreviewOpen)}
                        >
                          <FontAwesomeIcon icon={variantPreviewOpen ? faChevronUp : faChevronDown} />
                          {variantPreviewOpen ? 'Hide Preview' : 'Preview Email'}
                        </button>
                        {variantPreviewOpen && activeVariant.html_content && (
                          <div className="cv-preview-frame">
                            <iframe
                              srcDoc={activeVariant.html_content}
                              title={`Preview ${activeVariant.variant_name}`}
                              sandbox=""
                            />
                          </div>
                        )}
                      </div>
                    </div>
                  </div>
                );
              })()}

              {/* Performance Metrics */}
              {stats && (
                <div className="details-section">
                  <h3><FontAwesomeIcon icon={faChartBar} /> Performance Metrics <span style={{ fontSize: 11, fontWeight: 600, color: 'rgba(180,210,240,0.45)' }}>legacy /stats</span></h3>
                  <p style={{ fontSize: 11.5, color: 'rgba(180,210,240,0.55)', margin: '-4px 0 10px' }}>
                    Engagement (opens/clicks) from the legacy counters. For the authoritative delivery &amp; bounce breakdown —
                    including hard bounce vs reputation/system block — use <strong>Delivery Terminal State</strong> below
                    (derived from tracking_events). The two can differ while a campaign is still settling.
                  </p>
                  <div className="metrics-grid ig-stagger">
                    <div className="metric-box">
                      <FontAwesomeIcon icon={faPaperPlane} className="metric-icon" />
                      <div className="metric-value"><AnimatedCounter value={stats.sent} formatFn={(n) => Math.round(n).toLocaleString()} /></div>
                      <div className="metric-label">Sent</div>
                    </div>
                    <div className="metric-box">
                      <FontAwesomeIcon icon={faEnvelopeOpen} className="metric-icon" />
                      <div className="metric-value"><AnimatedCounter value={stats.opens} formatFn={(n) => Math.round(n).toLocaleString()} /></div>
                      <div className="metric-label">Opens ({stats.open_rate.toFixed(1)}%)</div>
                    </div>
                    <div className="metric-box">
                      <FontAwesomeIcon icon={faMousePointer} className="metric-icon" />
                      <div className="metric-value"><AnimatedCounter value={stats.clicks} formatFn={(n) => Math.round(n).toLocaleString()} /></div>
                      <div className="metric-label">Clicks ({stats.click_rate.toFixed(1)}%)</div>
                    </div>
                    <div className="metric-box">
                      <FontAwesomeIcon icon={faBan} className="metric-icon" style={{ color: '#ef4444' }} />
                      <div className="metric-value"><AnimatedCounter value={stats.hard_bounces} formatFn={(n) => Math.round(n).toLocaleString()} /></div>
                      <div className="metric-label">Hard Bounces ({(stats.hard_bounce_rate ?? 0).toFixed(1)}%)</div>
                    </div>
                    <div className="metric-box">
                      <FontAwesomeIcon icon={faExclamationTriangle} className="metric-icon" style={{ color: '#f59e0b' }} />
                      <div className="metric-value"><AnimatedCounter value={stats.soft_bounces} formatFn={(n) => Math.round(n).toLocaleString()} /></div>
                      <div className="metric-label">Soft Bounces ({(stats.soft_bounce_rate ?? 0).toFixed(1)}%)</div>
                    </div>
                    <div className="metric-box">
                      <FontAwesomeIcon icon={faExclamationTriangle} className="metric-icon" />
                      <div className="metric-value"><AnimatedCounter value={stats.complaints} formatFn={(n) => Math.round(n).toLocaleString()} /></div>
                      <div className="metric-label">Complaints ({stats.complaint_rate.toFixed(3)}%)</div>
                    </div>
                    <div className="metric-box">
                      <FontAwesomeIcon icon={faUserMinus} className="metric-icon" />
                      <div className="metric-value"><AnimatedCounter value={stats.unsubscribes} formatFn={(n) => Math.round(n).toLocaleString()} /></div>
                      <div className="metric-label">Unsubscribes ({stats.unsubscribe_rate.toFixed(2)}%)</div>
                    </div>
                  </div>
                </div>
              )}

              {/* ============================================================
                  Phase 1 detail analytics (additive). These two panels are fed
                  by /analytics/campaign-summary/{id} — the accurate, tracking-
                  derived source. They sit alongside the legacy /stats panels
                  above, which are intentionally left untouched.
                  ============================================================ */}

              {/* Inline notice if the summary fetch failed — never crash. */}
              {summaryError && !summaryDetail && (
                <div
                  className="details-section"
                  style={{
                    display: 'flex', alignItems: 'center', gap: 10,
                    color: '#f59e0b', fontSize: 13,
                  }}
                >
                  <FontAwesomeIcon icon={faExclamationTriangle} />
                  <span>Audience funnel &amp; terminal-state analytics unavailable ({summaryError}). Legacy metrics above are unaffected.</span>
                </div>
              )}

              {/* Delivery Terminal State — accurate counts from tracking events. */}
              {summaryDetail && (() => {
                const c = summaryDetail.campaign;
                const isSes = c.route_type === 'ses';
                const tile = (
                  label: string,
                  value: number,
                  color: string,
                  sub?: string,
                  tip?: string,
                ) => (
                  <div className="metric-box" title={tip} key={label}>
                    <FontAwesomeIcon icon={faPaperPlane} className="metric-icon" style={{ color }} />
                    <div className="metric-value" style={{ color }}>
                      <AnimatedCounter value={value} formatFn={(n) => Math.round(n).toLocaleString()} />
                    </div>
                    <div className="metric-label">{label}{sub ? ` ${sub}` : ''}</div>
                  </div>
                );
                return (
                  <div className="details-section">
                    <h3 style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
                      <FontAwesomeIcon icon={faChartPie} /> Delivery Terminal State
                      <span
                        style={{
                          fontSize: 11, fontWeight: 700, letterSpacing: 0.4,
                          textTransform: 'uppercase', padding: '2px 9px', borderRadius: 12,
                          background: isSes ? 'rgba(168,85,247,0.15)' : 'rgba(99,102,241,0.15)',
                          color: isSes ? '#c084fc' : '#818cf8',
                          border: `1px solid ${isSes ? 'rgba(168,85,247,0.35)' : 'rgba(99,102,241,0.35)'}`,
                        }}
                      >
                        {c.route_type}
                      </span>
                      <span style={{ fontSize: 11, fontWeight: 600, color: 'rgba(180,210,240,0.45)' }}>
                        accurate
                      </span>
                    </h3>

                    {/* Campaign metadata strip — everything needed to interpret
                        the numbers and decide next steps, in one place. */}
                    {(() => {
                      const meta: Array<[string, React.ReactNode]> = [
                        ['Campaign ID', <code style={{ fontSize: 11 }}>{c.id}</code>],
                        ['Status', c.status],
                        ['Type', c.campaign_type || '—'],
                        ['Route', c.route_type?.toUpperCase()],
                        ['Subject', c.subject || '—'],
                        ['Preheader', c.preheader || '—'],
                        ['From', c.from_name ? `${c.from_name}${c.from_email ? ` <${c.from_email}>` : ''}` : '—'],
                        ['Sending Profile', c.sending_profile || '—'],
                        ['Sending Domain', c.sending_domain || '—'],
                        ['IP Pool', c.ip_pool || '—'],
                        ['Vendor', c.vendor || '—'],
                        ['Targeted (planned)', (c.targeted ?? 0).toLocaleString()],
                        ['Sent (injected)', (c.sent ?? 0).toLocaleString()],
                        ['Scheduled', c.scheduled_at ? new Date(c.scheduled_at).toLocaleString() : '—'],
                        ['Created', c.created_at ? new Date(c.created_at).toLocaleString() : '—'],
                      ];
                      return (
                        <div
                          style={{
                            display: 'grid',
                            gridTemplateColumns: 'repeat(auto-fill, minmax(230px, 1fr))',
                            gap: '6px 18px', marginBottom: 14,
                            fontSize: 12.5,
                          }}
                        >
                          {meta.map(([label, value]) => (
                            <div key={label} style={{ display: 'flex', flexDirection: 'column', minWidth: 0 }}>
                              <span style={{ color: 'rgba(180,210,240,0.45)', fontSize: 10.5, textTransform: 'uppercase', letterSpacing: 0.4 }}>{label}</span>
                              <span style={{ color: '#cbd5e1', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={typeof value === 'string' ? value : undefined}>{value}</span>
                            </div>
                          ))}
                        </div>
                      );
                    })()}

                    {/* Diagnostic: when reputation blocks dominate the failures,
                        tell the operator this is a sender-reputation problem
                        (valid recipients), not list hygiene — and what to do. */}
                    {(() => {
                      const processed = c.delivered + c.hard_bounce + c.reputation_block + c.soft_bounce;
                      const blockPct = processed > 0 ? (c.reputation_block / processed) * 100 : 0;
                      if (!(c.reputation_block > 0 && c.reputation_block >= c.hard_bounce && blockPct >= 2)) return null;
                      return (
                        <div
                          style={{
                            display: 'flex', gap: 10, alignItems: 'flex-start',
                            background: 'rgba(251,146,60,0.10)', border: '1px solid rgba(251,146,60,0.35)',
                            borderRadius: 8, padding: '10px 12px', marginBottom: 14, fontSize: 12.5, color: '#fed7aa',
                          }}
                        >
                          <FontAwesomeIcon icon={faExclamationTriangle} style={{ color: '#fb923c', marginTop: 2 }} />
                          <span>
                            <strong>{blockPct.toFixed(1)}% reputation/system blocks</strong> — the dominant failure here is the receiving ISP
                            refusing mail from this sending IP/domain (e.g. Charter/Spectrum “5.3.2 system not accepting network messages”),
                            <strong> not invalid addresses</strong>. These recipients are valid and were <strong>not suppressed</strong>.
                            Action: slow the send / warm this IP pool, and verify SPF·DKIM·DMARC and blocklist status for <code>{c.sending_domain || 'the sending domain'}</code>.
                          </span>
                        </div>
                      );
                    })()}

                    <div className="metrics-grid ig-stagger" style={{ gap: 8 }}>
                      {tile('Recipients', c.recipients, '#94a3b8', undefined, 'Distinct recipients after audience finalization')}
                      {isSes && tile('Relayed to SES', c.relayed_to_ses, '#c084fc', undefined, 'PMTA handoff to SES — not a delivery')}
                      {tile('Delivered', c.delivered, '#22c55e')}
                      {tile('Hard Bounce', c.hard_bounce, '#ef4444', undefined, 'TRUE list hygiene only: invalid address, dead domain, or disabled mailbox (DSN 5.1.x / 5.2.1). These ARE suppressed.')}
                      {tile('Reputation Block', c.reputation_block, '#fb923c', undefined, 'Sender-side ISP block of a VALID recipient — DSN 5.3/5.4/5.5/5.7 (e.g. "5.3.2 system not accepting network messages") or policy/routing/connection rejections. Recoverable via warmup/pacing/reputation work. NOT a bad address and NOT suppressed.')}
                      {tile('Soft Bounce', c.soft_bounce, '#f59e0b', undefined, 'Transient failure (mailbox full, rate-limit, timeout). Clears on retry.')}
                      {tile('Deferred / Open', c.deferred_open, '#60a5fa', undefined, 'Deferred but later observed open')}
                      {tile('Sent Only', c.sent_only, '#64748b', undefined, 'Injected/sent with no terminal event yet')}
                    </div>

                    {/* Rates with explicit denominator labels */}
                    <div className="metrics-grid ig-stagger" style={{ gap: 8, marginTop: 8 }}>
                      <div className="metric-box">
                        <FontAwesomeIcon icon={faCheckCircle} className="metric-icon" style={{ color: '#22c55e' }} />
                        <div className="metric-value" style={{ color: '#22c55e' }}>{c.delivery_rate.toFixed(2)}%</div>
                        <div className="metric-label">Delivery (of sent)</div>
                      </div>
                      <div className="metric-box" title="TRUE list-hygiene hard bounces (invalid address / dead domain / disabled mailbox) over processed (delivered + hard + reputation block + soft).">
                        <FontAwesomeIcon icon={faBan} className="metric-icon" style={{ color: '#ef4444' }} />
                        <div className="metric-value" style={{ color: '#ef4444' }}>{c.hard_bounce_rate.toFixed(2)}%</div>
                        <div className="metric-label">Hard Bounce (of processed)</div>
                      </div>
                      <div className="metric-box" title="Reputation/system blocks (DSN 5.3/5.4/5.5/5.7, policy/routing/connection) over processed. High here = a sender-reputation / warmup problem, not a dirty list.">
                        <FontAwesomeIcon icon={faExclamationTriangle} className="metric-icon" style={{ color: '#fb923c' }} />
                        <div className="metric-value" style={{ color: '#fb923c' }}>{(c.reputation_block_rate ?? 0).toFixed(2)}%</div>
                        <div className="metric-label">Reputation Block (of processed)</div>
                      </div>
                      <div className="metric-box" title="Transient failures over processed. Clears on retry.">
                        <FontAwesomeIcon icon={faExclamationTriangle} className="metric-icon" style={{ color: '#f59e0b' }} />
                        <div className="metric-value" style={{ color: '#f59e0b' }}>{c.soft_bounce_rate.toFixed(2)}%</div>
                        <div className="metric-label">Soft Bounce (of processed)</div>
                      </div>
                      <div className="metric-box">
                        <FontAwesomeIcon icon={faEnvelopeOpen} className="metric-icon" />
                        <div className="metric-value">{c.open_rate.toFixed(2)}%</div>
                        <div className="metric-label">Open (of delivered)</div>
                      </div>
                      <div className="metric-box">
                        <FontAwesomeIcon icon={faMousePointer} className="metric-icon" />
                        <div className="metric-value">{c.click_rate.toFixed(2)}%</div>
                        <div className="metric-label">Click (of delivered)</div>
                      </div>
                      <div className="metric-box">
                        <FontAwesomeIcon icon={faPercentage} className="metric-icon" />
                        <div className="metric-value">{c.ctor.toFixed(2)}%</div>
                        <div className="metric-label">CTOR (clicks of opens)</div>
                      </div>
                    </div>

                    {c.data_as_of && (
                      <div style={{ marginTop: 10, fontSize: 11, color: 'rgba(180,210,240,0.45)', display: 'flex', alignItems: 'center', gap: 6 }}>
                        <FontAwesomeIcon icon={faClock} />
                        <span>Source: {c.metrics_source || 'tracking_events'} · as of {new Date(c.data_as_of).toLocaleString()}</span>
                      </div>
                    )}
                  </div>
                );
              })()}

              {/* Audience Funnel — how the planned audience was whittled down. */}
              {summaryDetail && (() => {
                const funnel = summaryDetail.audience_funnel;
                if (!funnel) {
                  return (
                    <div className="details-section">
                      <h3><FontAwesomeIcon icon={faUsers} /> Audience Funnel</h3>
                      <div style={{ fontSize: 13, color: 'rgba(180,210,240,0.45)', fontStyle: 'italic' }}>
                        Funnel not recorded for this campaign (finalized before funnel tracking).
                      </div>
                    </div>
                  );
                }
                const reasons = (Object.keys(funnel.reason_breakdown) as Array<keyof CampaignFunnelReasonBreakdown>)
                  .map((key) => ({ key, count: funnel.reason_breakdown[key] }))
                  .filter((r) => r.count > 0)
                  .sort((a, b) => b.count - a.count);
                const maxReason = reasons.length > 0 ? reasons[0].count : 0;
                const stage = (label: string, value: number, color: string, tip?: string) => (
                  <div
                    title={tip}
                    style={{
                      display: 'flex', alignItems: 'center', justifyContent: 'space-between',
                      padding: '12px 16px', borderRadius: 10,
                      background: 'rgba(15,23,42,0.6)', border: '1px solid rgba(99,102,241,0.12)',
                    }}
                  >
                    <span style={{ fontSize: 13, fontWeight: 600, color: 'rgba(180,210,240,0.7)' }}>{label}</span>
                    <span style={{ fontSize: 18, fontWeight: 700, color }}>{value.toLocaleString()}</span>
                  </div>
                );
                return (
                  <div className="details-section">
                    <h3><FontAwesomeIcon icon={faUsers} /> Audience Funnel</h3>
                    <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
                      {stage('Total Seen', funnel.total_seen, '#e0e6f0', 'Subscribers streamed through the finalizer')}
                      <div style={{ textAlign: 'center', color: 'rgba(180,210,240,0.3)', lineHeight: 1 }}>
                        <FontAwesomeIcon icon={faArrowDown} />
                      </div>
                      {stage('Targeted (selected)', funnel.targeted, '#22c55e', 'Recipients that passed every filter')}
                      <div style={{ textAlign: 'center', color: 'rgba(180,210,240,0.3)', lineHeight: 1 }}>
                        <FontAwesomeIcon icon={faArrowDown} />
                      </div>
                      {stage('Suppressed (total)', funnel.suppressed_total, '#f59e0b', 'Sum of all suppression reasons below')}
                      {funnel.reserve > 0 && (
                        <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.4)', textAlign: 'right' }}>
                          + {funnel.reserve.toLocaleString()} held in reserve
                        </div>
                      )}
                    </div>

                    {reasons.length > 0 && (
                      <div style={{ marginTop: 16 }}>
                        <div style={{ fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.5, fontWeight: 600, color: 'rgba(180,210,240,0.4)', marginBottom: 8 }}>
                          Suppression Reasons
                        </div>
                        <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
                          {reasons.map((r) => {
                            const isPrimary = r.key === 'global_or_brand_suppression';
                            const pct = maxReason > 0 ? (r.count / maxReason) * 100 : 0;
                            return (
                              <div key={r.key} style={{ position: 'relative' }}>
                                <div
                                  style={{
                                    display: 'flex', alignItems: 'center', justifyContent: 'space-between',
                                    padding: isPrimary ? '10px 14px' : '8px 14px',
                                    borderRadius: 8, position: 'relative', overflow: 'hidden',
                                    background: isPrimary ? 'rgba(239,68,68,0.08)' : 'rgba(99,102,241,0.05)',
                                    border: `1px solid ${isPrimary ? 'rgba(239,68,68,0.3)' : 'rgba(99,102,241,0.12)'}`,
                                  }}
                                >
                                  <div
                                    className="ig-progress-fill"
                                    style={{
                                      position: 'absolute', left: 0, top: 0, bottom: 0,
                                      width: `${pct}%`,
                                      background: isPrimary ? 'rgba(239,68,68,0.12)' : 'rgba(99,102,241,0.08)',
                                      zIndex: 0,
                                    }}
                                  />
                                  <span
                                    style={{
                                      position: 'relative', zIndex: 1,
                                      fontSize: isPrimary ? 13 : 12.5,
                                      fontWeight: isPrimary ? 700 : 600,
                                      color: isPrimary ? '#fca5a5' : 'rgba(199,210,254,0.85)',
                                    }}
                                  >
                                    {FUNNEL_REASON_LABELS[r.key] || r.key}
                                  </span>
                                  <span
                                    style={{
                                      position: 'relative', zIndex: 1,
                                      fontSize: isPrimary ? 15 : 14,
                                      fontWeight: 700,
                                      color: isPrimary ? '#ef4444' : '#c7d2fe',
                                    }}
                                  >
                                    {r.count.toLocaleString()}
                                  </span>
                                </div>
                              </div>
                            );
                          })}
                        </div>
                      </div>
                    )}

                    {funnel.computed_at && (
                      <div style={{ marginTop: 10, fontSize: 11, color: 'rgba(180,210,240,0.45)', display: 'flex', alignItems: 'center', gap: 6 }}>
                        <FontAwesomeIcon icon={faClock} />
                        <span>Funnel computed {new Date(funnel.computed_at).toLocaleString()}</span>
                      </div>
                    )}
                  </div>
                );
              })()}

              {/* Queue Depth — what is actually in flight in mailing_campaign_queue.
                  Phase B addition (VersionCampaignBuilder=1.0). The campaign
                  detail header tile previously had no insight into the worker
                  queue; you had to bounce to Outbox to see if 50k recipients
                  were stuck or progressing. Now it's one glance. */}
              {stats && stats.queue_status && Object.keys(stats.queue_status).length > 0 && (
                <div className="details-section">
                  <h3><FontAwesomeIcon icon={faClock} /> Queue Depth</h3>
                  <div className="metrics-grid ig-stagger" style={{ gap: 8 }}>
                    {Object.entries(stats.queue_status).map(([status, count]) => {
                      const statusColor: Record<string, string> = {
                        queued:        '#60a5fa',
                        sending:       '#22d3ee',
                        submitting:    '#22d3ee',
                        sent:          '#22c55e',
                        delivered:     '#22c55e',
                        failed:        '#ef4444',
                        dead_lettered: '#b91c1c',
                        bounced:       '#f59e0b',
                      };
                      const color = statusColor[status] || '#94a3b8';
                      return (
                        <div className="metric-box" key={status}>
                          <FontAwesomeIcon icon={faClock} className="metric-icon" style={{ color }} />
                          <div className="metric-value" style={{ color }}>
                            <AnimatedCounter value={count} formatFn={(n) => Math.round(n).toLocaleString()} />
                          </div>
                          <div className="metric-label" style={{ textTransform: 'capitalize' }}>{status.replace(/_/g, ' ')}</div>
                        </div>
                      );
                    })}
                  </div>
                </div>
              )}

              {/* ISP Breakdown — Quotas & Actual Mailed */}
              {stats && stats.isp_breakdown && stats.isp_breakdown.length > 0 && (
                <div className="details-section">
                  <h3><FontAwesomeIcon icon={faBullseye} /> ISP Breakdown — Quotas &amp; Delivery</h3>
                  <div style={{ overflowX: 'auto' }}>
                    <table className="isp-breakdown-table">
                      <thead>
                        <tr>
                          <th>ISP</th>
                          <th>Quota</th>
                          <th>Sent</th>
                          <th>Delivered</th>
                          <th>Opens</th>
                          <th>Clicks</th>
                          <th>Hard B.</th>
                          <th>Soft B.</th>
                          <th>Complaints</th>
                        </tr>
                      </thead>
                      <tbody>
                        {stats.isp_breakdown.map(row => {
                          const ispLabels: Record<string, string> = {
                            gmail: 'Gmail', yahoo: 'Yahoo', microsoft: 'Microsoft', apple: 'Apple iCloud',
                            comcast: 'Comcast', att: 'AT&T', cox: 'Cox', charter: 'Charter/Spectrum', other: 'Other',
                          };
                          const pct = (n: number, d: number) => d > 0 ? (n / d * 100).toFixed(1) + '%' : '—';
                          const quotaUsed = row.quota && row.quota > 0 ? Math.round(row.sent / row.quota * 100) : null;
                          return (
                            <tr key={row.isp}>
                              <td style={{ fontWeight: 600, color: '#e0e6f0' }}>{ispLabels[row.isp] || row.isp}</td>
                              <td>
                                {row.quota ? (
                                  <span style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 2 }}>
                                    <span>{row.quota.toLocaleString()}</span>
                                    {quotaUsed !== null && (
                                      <span style={{
                                        fontSize: '0.7rem',
                                        color: quotaUsed >= 90 ? '#ef4444' : quotaUsed >= 70 ? '#f59e0b' : '#22c55e',
                                        fontWeight: 700,
                                      }}>{quotaUsed}% used</span>
                                    )}
                                  </span>
                                ) : <span style={{ color: 'rgba(180,210,240,0.35)' }}>—</span>}
                              </td>
                              <td>{row.sent.toLocaleString()}</td>
                              <td>{row.delivered.toLocaleString()}</td>
                              <td>
                                <span>{row.opens.toLocaleString()}</span>
                                <span style={{ fontSize: '0.75rem', color: 'rgba(180,210,240,0.55)', marginLeft: 4 }}>
                                  ({pct(row.opens, row.delivered)})
                                </span>
                              </td>
                              <td>
                                <span>{row.clicks.toLocaleString()}</span>
                                <span style={{ fontSize: '0.75rem', color: 'rgba(180,210,240,0.55)', marginLeft: 4 }}>
                                  ({pct(row.clicks, row.delivered)})
                                </span>
                              </td>
                              <td style={{ color: row.hard_bounces > 0 ? '#ef4444' : 'inherit' }}>{row.hard_bounces.toLocaleString()}</td>
                              <td style={{ color: row.soft_bounces > 0 ? '#f59e0b' : 'inherit' }}>{row.soft_bounces.toLocaleString()}</td>
                              <td style={{ color: row.complaints > 0 ? '#ef4444' : 'inherit' }}>{row.complaints.toLocaleString()}</td>
                            </tr>
                          );
                        })}
                      </tbody>
                    </table>
                  </div>
                </div>
              )}

              {/* Edit Lock Info */}
              {campaign.status === 'scheduled' && campaign.scheduled_at && (
                <div className={`edit-lock-info ${getEditLockInfo(campaign).isLocked ? 'locked' : ''}`}>
                  <FontAwesomeIcon icon={getEditLockInfo(campaign).isLocked ? faExclamationTriangle : faInfoCircle} />
                  <span>{getEditLockInfo(campaign).message}</span>
                </div>
              )}

              {/* Actions */}
              <div className="details-actions">
                {canEditCampaign(campaign) && (
                  <button className="action-btn secondary" onClick={() => onAction(campaign.id, 'edit')}>
                    <FontAwesomeIcon icon={faEdit} /> Edit Campaign
                  </button>
                )}
                {campaign.status === 'draft' && (
                  <>
                    <button className="action-btn primary ig-btn-glow ig-ripple" onClick={() => onAction(campaign.id, 'send')}>
                      <FontAwesomeIcon icon={faPaperPlane} /> Send Now
                    </button>
                    <button className="action-btn secondary" onClick={() => onAction(campaign.id, 'schedule')}>
                      <FontAwesomeIcon icon={faCalendarAlt} /> Schedule
                    </button>
                  </>
                )}
                {['scheduled', 'preparing', 'sending'].includes(campaign.status) && (
                  <button className="action-btn warning" onClick={() => onAction(campaign.id, 'pause')}>
                    <FontAwesomeIcon icon={faPause} /> Pause
                  </button>
                )}
                {campaign.status === 'paused' && (
                  <>
                    <button className="action-btn primary ig-btn-glow ig-ripple" onClick={() => onAction(campaign.id, 'resume')}>
                      <FontAwesomeIcon icon={faPlay} /> Resume
                    </button>
                  </>
                )}
                {['scheduled', 'preparing', 'sending', 'paused'].includes(campaign.status) && (
                  <button className="action-btn danger" onClick={() => onAction(campaign.id, 'cancel')}>
                    <FontAwesomeIcon icon={faStop} /> Cancel
                  </button>
                )}
                <button className="action-btn secondary" onClick={() => onAction(campaign.id, 'duplicate')}>
                  <FontAwesomeIcon icon={faCopy} /> Duplicate
                </button>
              </div>
            </>
          )}
        </div>
      </div>
    </div>
  );
};

// ============================================================================
// CAMPAIGN EDITOR - Modern Step-Based Builder
// ============================================================================

import { SearchableMultiSelect, SelectOption } from './SearchableMultiSelect';
import { EmailEditor } from './EmailEditor';
import { EverflowCreativeSelector } from './EverflowCreativeSelector';
import { CampaignPurposeTab, CampaignObjective, DEFAULT_OBJECTIVE } from './CampaignPurposeTab';
import { AgentConfigWizard } from './AgentConfigWizard';
import './SearchableMultiSelect.css';
import './EmailEditor.css';
import './CampaignPurposeTab.css';
import './AgentConfigWizard.css';

type EditorStep = 'purpose' | 'details' | 'audience' | 'content' | 'schedule' | 'review';

interface CampaignEditorProps {
  campaign: Campaign | null;
  onSave: () => void;
  onCancel: () => void;
  initialOffer?: { offerId: string; offerName: string } | null;
  onOfferConsumed?: () => void;
}

const SUBJECT_MAX_LENGTH = 60;
const PREHEADER_MAX_LENGTH = 100;

// Test/Proof Email Result interface
interface TestEmailResult {
  success: boolean;
  message_id?: string;
  sent_at?: string;
  to?: string;
  error?: string;
  vendor?: string;
}

const CampaignEditor: React.FC<CampaignEditorProps> = ({ campaign, onSave, onCancel, initialOffer, onOfferConsumed }) => {
  const { organization } = useAuth();
  const isEditing = campaign !== null;
  const [currentStep, setCurrentStep] = useState<EditorStep>('purpose');
  const [objective, setObjective] = useState<CampaignObjective>(DEFAULT_OBJECTIVE);
  const [formData, setFormData] = useState({
    name: campaign?.name || '',
    subject: campaign?.subject || '',
    preheader: '',
    from_name: campaign?.from_name || '',
    from_email: campaign?.from_email || '',
    reply_to: '',
    scheduled_at: campaign?.scheduled_at ? new Date(campaign.scheduled_at).toISOString().slice(0, 16) : '',
    html_content: '',
    throttle_speed: campaign?.throttle_speed || 'instant',
  });
  const [saving, setSaving] = useState(false);
  const [sendingTest, setSendingTest] = useState(false);
  const [profiles, setProfiles] = useState<Array<{ id: string; name: string; from_name: string; from_email: string; vendor_type: string; is_default?: boolean }>>([]);
  const [selectedProfile, setSelectedProfile] = useState('');
  const [segments, setSegments] = useState<SelectOption[]>([]);
  const [selectedSegments, setSelectedSegments] = useState<string[]>([]);
  const [suppressionLists, setSuppressionLists] = useState<SelectOption[]>([]);
  const [selectedSuppressions, setSelectedSuppressions] = useState<string[]>([]);
  const [selectedSuppressionSegments, setSelectedSuppressionSegments] = useState<string[]>([]);
  const [estimatedReach, setEstimatedReach] = useState(0);
  
  // AI Agent Wizard Mode
  const [useAgentWizard, setUseAgentWizard] = useState(true);
  
  // Everflow Creative Mode — when a creative is applied, bypass TipTap and use raw HTML
  const [creativeMode, setCreativeMode] = useState(false);
  const [creativeCodeView, setCreativeCodeView] = useState(false);

  // Test/Proof Email Modal State
  const [showTestModal, setShowTestModal] = useState(false);
  const [testEmails, setTestEmails] = useState('');
  const [testSubjectPrefix, setTestSubjectPrefix] = useState('[TEST] ');
  const [testResult, setTestResult] = useState<TestEmailResult | null>(null);
  const [testHistory, setTestHistory] = useState<TestEmailResult[]>([]);

  // Performance & Quota stats (edit mode only)
  const [editStats, setEditStats] = useState<CampaignStats | null>(null);
  const [editStatsExpanded, setEditStatsExpanded] = useState(false);
  const [editStatsLoading, setEditStatsLoading] = useState(false);

  const steps: { id: EditorStep; label: string; icon: any }[] = [
    { id: 'purpose', label: 'Purpose', icon: faBullseye },
    { id: 'details', label: 'Details', icon: faFileAlt },
    { id: 'audience', label: 'Audience', icon: faUsers },
    { id: 'content', label: 'Content', icon: faEnvelope },
    { id: 'schedule', label: 'Schedule', icon: faCalendarAlt },
    { id: 'review', label: 'Review', icon: faCheckCircle },
  ];

  // Load data on mount
  useEffect(() => {
    const loadData = async () => {
      try {
        // First, load all reference data
        const [profilesRes, segmentsRes, suppressionsRes] = await Promise.all([
          orgFetch(`${API_BASE}/sending-profiles`, organization?.id),
          orgFetch(`${API_BASE}/segments`, organization?.id),
          orgFetch(`${API_BASE}/suppression-lists`, organization?.id),
        ]);
        
        const [profilesData, segmentsData, suppressionsData] = await Promise.all([
          profilesRes.json(),
          segmentsRes.json(),
          suppressionsRes.json(),
        ]);
        
        setProfiles(profilesData.profiles || []);
        
        // Transform segments for SearchableMultiSelect
        const segmentOptions: SelectOption[] = (segmentsData.segments || []).map((s: any, i: number) => ({
          id: s.id,
          name: s.name,
          count: s.subscriber_count || 0,
          category: s.category || 'All Segments',
          isFavorite: i < 3,
          isRecent: i < 5,
          type: 'segment' as const,
        }));
        setSegments(segmentOptions);
        
        // Transform suppression lists
        const suppressionOptions: SelectOption[] = (suppressionsData.lists || suppressionsData || []).map((s: any) => ({
          id: s.id,
          name: s.name,
          count: s.entry_count || 0,
          category: s.is_global ? 'Global' : 'Campaign',
          isLocked: s.is_global,
          isRequired: s.is_global,
          type: 'suppression' as const,
        }));
        setSuppressionLists(suppressionOptions);
        
        // Now load campaign data if editing
        if (isEditing && campaign) {
          const campaignRes = await orgFetch(`${API_BASE}/campaigns/${campaign.id}`, organization?.id);
          const data = await campaignRes.json();
          
          // Set form data with all campaign fields
          // Convert UTC scheduled_at to local time for datetime-local input
          let localScheduledAt = '';
          if (data.scheduled_at) {
            const utcDate = new Date(data.scheduled_at);
            // Format as local datetime-local value (YYYY-MM-DDTHH:mm)
            const year = utcDate.getFullYear();
            const month = String(utcDate.getMonth() + 1).padStart(2, '0');
            const day = String(utcDate.getDate()).padStart(2, '0');
            const hours = String(utcDate.getHours()).padStart(2, '0');
            const minutes = String(utcDate.getMinutes()).padStart(2, '0');
            localScheduledAt = `${year}-${month}-${day}T${hours}:${minutes}`;
          }
          
          setFormData(prev => ({
            ...prev,
            name: data.name || prev.name,
            subject: data.subject || prev.subject,
            preheader: data.preview_text || data.preheader || '',
            from_name: data.from_name || prev.from_name,
            from_email: data.from_email || prev.from_email,
            reply_to: data.reply_email || '',
            html_content: data.html_content || '',
            scheduled_at: localScheduledAt,
          }));
          
          // Set sending profile
          if (data.sending_profile_id) {
            setSelectedProfile(data.sending_profile_id);
          }
          
          // Set selected segments (from segment_ids array or fallback to segment_id)
          const campaignSegmentIds = data.segment_ids || (data.segment_id ? [data.segment_id] : []);
          if (campaignSegmentIds.length > 0) {
            setSelectedSegments(campaignSegmentIds);
          }
          
          // Set selected suppression lists
          const globalSuppressions = suppressionOptions.filter(s => s.isLocked).map(s => s.id);
          const campaignSuppressionListIds = data.suppression_list_ids || [];
          // Merge global suppressions with campaign-specific ones
          const allSuppressions = [...new Set([...globalSuppressions, ...campaignSuppressionListIds])];
          setSelectedSuppressions(allSuppressions);
          
          // Set suppression segments
          if (data.suppression_segment_ids && data.suppression_segment_ids.length > 0) {
            setSelectedSuppressionSegments(data.suppression_segment_ids);
          }
          
          // Load campaign objective
          try {
            const objectiveRes = await orgFetch(`${API_BASE}/campaigns/${campaign.id}/objective`, organization?.id);
            if (objectiveRes.ok) {
              const objectiveData = await objectiveRes.json();
              if (objectiveData && objectiveData.purpose) {
                setObjective(objectiveData);
              }
            }
          } catch (objErr) {
            console.log('No objective found for campaign, using defaults');
          }
        } else {
          // New campaign - set defaults
          const globalSuppressions = suppressionOptions.filter(s => s.isLocked).map(s => s.id);
          setSelectedSuppressions(globalSuppressions);
          
          if (profilesData.profiles?.length > 0) {
            const defaultProfile = profilesData.profiles.find((p: any) => p.is_default) || profilesData.profiles[0];
            setSelectedProfile(defaultProfile.id);
            setFormData(prev => ({
              ...prev,
              from_name: defaultProfile.from_name,
              from_email: defaultProfile.from_email,
            }));
          }
        }
      } catch (err) {
        console.error('Failed to load editor data:', err);
      }
    };
    
    loadData();
  }, [isEditing, campaign, organization]);

  // Fetch performance stats when editing an existing campaign
  useEffect(() => {
    if (!isEditing || !campaign?.id) return;
    const fetchEditStats = async () => {
      setEditStatsLoading(true);
      try {
        const res = await orgFetch(`${API_BASE}/campaigns/${campaign.id}/stats`, organization?.id);
        if (res.ok) {
          const data = await res.json();
          setEditStats(data);
        }
      } catch (err) {
        console.error('Failed to load campaign stats for editor:', err);
      } finally {
        setEditStatsLoading(false);
      }
    };
    fetchEditStats();
  }, [isEditing, campaign?.id, organization?.id]);

  // Calculate estimated reach when segments change
  useEffect(() => {
    const total = segments
      .filter(s => selectedSegments.includes(s.id))
      .reduce((sum, s) => sum + (s.count || 0), 0);
    setEstimatedReach(total);
  }, [selectedSegments, segments]);

  const handleProfileChange = (profileId: string) => {
    setSelectedProfile(profileId);
    const profile = profiles.find(p => p.id === profileId);
    if (profile) {
      setFormData(prev => ({
        ...prev,
        from_name: profile.from_name,
        from_email: profile.from_email,
      }));
    }
  };

  const handleSubmit = async () => {
    if (!formData.name || !formData.subject) {
      alert('Please fill in campaign name and subject');
      return;
    }

    setSaving(true);
    try {
      // Capture Everflow creative metadata if present
      const efCreative = (window as any).__everflowCreative;
      const payload: Record<string, any> = {
        name: formData.name,
        subject: formData.subject,
        preview_text: formData.preheader, // Backend expects preview_text
        html_content: formData.html_content,
        from_name: formData.from_name,
        from_email: formData.from_email,
        reply_to: formData.reply_to,
        sending_profile_id: selectedProfile,
        segment_ids: selectedSegments,
        suppression_list_ids: selectedSuppressions,
        suppression_segment_ids: selectedSuppressionSegments,
        scheduled_at: formData.scheduled_at ? new Date(formData.scheduled_at).toISOString() : null,
      };
      // Include Everflow creative info in campaign payload
      if (efCreative) {
        payload.everflow_creative_id = efCreative.creativeId;
        payload.everflow_offer_id = efCreative.offerId;
        payload.tracking_link_template = efCreative.trackingLink;
      }

      let res;
      let campaignId = campaign?.id;
      
      if (isEditing && campaign) {
        res = await orgFetch(`${API_BASE}/campaigns/${campaign.id}`, organization?.id, {
          method: 'PUT',
          body: JSON.stringify(payload),
        });
      } else {
        res = await orgFetch(`${API_BASE}/campaigns`, organization?.id, {
          method: 'POST',
          body: JSON.stringify(payload),
        });
      }

      if (res.ok) {
        // Get the campaign ID from response for new campaigns
        if (!isEditing) {
          const newCampaign = await res.json();
          campaignId = newCampaign.id;
        }
        
        // Save the campaign objective
        if (campaignId && objective.purpose) {
          try {
            const objectiveMethod = isEditing ? 'PUT' : 'POST';
            await orgFetch(`${API_BASE}/campaigns/${campaignId}/objective`, organization?.id, {
              method: objectiveMethod,
              body: JSON.stringify(objective),
            });
          } catch (objErr) {
            console.error('Failed to save campaign objective:', objErr);
            // Don't fail the whole save for objective errors
          }
        }
        
        onSave();
      } else {
        const error = await res.json();
        alert(`Failed to save campaign: ${error.error || 'Unknown error'}`);
      }
    } catch (err) {
      console.error('Failed to save campaign:', err);
      alert('Failed to save campaign');
    } finally {
      setSaving(false);
    }
  };

  const handleSendTest = async () => {
    if (!testEmails.trim()) return;
    
    setSendingTest(true);
    setTestResult(null);
    
    // Parse email addresses (comma or newline separated)
    const emailList = testEmails
      .split(/[,\n]/)
      .map(e => e.trim())
      .filter(e => e && e.includes('@'));
    
    if (emailList.length === 0) {
      setTestResult({ success: false, error: 'Please enter valid email addresses' });
      setSendingTest(false);
      return;
    }

    try {
      // Send test emails one by one
      const results: TestEmailResult[] = [];
      for (const email of emailList) {
        const response = await orgFetch('/api/mailing/send-test', organization?.id, {
          method: 'POST',
          body: JSON.stringify({
            to: email,
            subject: `${testSubjectPrefix}${formData.subject}`,
            from_name: formData.from_name,
            from_email: formData.from_email,
            html_content: formData.html_content,
            sending_profile_id: selectedProfile || undefined,
            preheader: formData.preheader,
          }),
        });

        const data = await response.json();
        results.push({ ...data, to: email });
      }
      
      // Show result for last email sent
      const lastResult = results[results.length - 1];
      setTestResult(lastResult);
      
      // Add successful sends to history
      const successful = results.filter(r => r.success);
      if (successful.length > 0) {
        setTestHistory(prev => [...successful, ...prev].slice(0, 10));
      }
    } catch (err) {
      setTestResult({ success: false, error: 'Network error - please try again' });
    } finally {
      setSendingTest(false);
    }
  };

  const openTestModal = () => {
    setTestResult(null);
    setShowTestModal(true);
  };

  const goToStep = (step: EditorStep) => setCurrentStep(step);
  const nextStep = () => {
    const currentIndex = steps.findIndex(s => s.id === currentStep);
    if (currentIndex < steps.length - 1) {
      setCurrentStep(steps[currentIndex + 1].id);
    }
  };
  const prevStep = () => {
    const currentIndex = steps.findIndex(s => s.id === currentStep);
    if (currentIndex > 0) {
      setCurrentStep(steps[currentIndex - 1].id);
    }
  };

  const isStepComplete = (step: EditorStep): boolean => {
    switch (step) {
      case 'purpose':
        return !!objective.purpose; // Always has a default value
      case 'details':
        return !!(formData.name && formData.subject && selectedProfile);
      case 'audience':
        return selectedSegments.length > 0;
      case 'content':
        return !!formData.html_content;
      case 'schedule':
        return true; // Schedule is optional
      case 'review':
        return true;
      default:
        return false;
    }
  };

  return (
    <div className="campaign-builder">
      {/* Header */}
      <div className="cb-header">
        <div className="cb-header-left">
          <button className="cb-back-btn" onClick={onCancel} aria-label="Close campaign editor">
            <FontAwesomeIcon icon={faTimes} />
          </button>
          <div className="cb-title-area">
            <h1>{isEditing ? 'Edit Campaign' : 'Create Campaign'}</h1>
            {formData.name && <span className="cb-campaign-name">{formData.name}</span>}
          </div>
        </div>
        <div className="cb-header-actions">
          <button className="cb-btn cb-btn-secondary" onClick={handleSendTest} disabled={sendingTest || !formData.html_content}>
            <FontAwesomeIcon icon={sendingTest ? faSpinner : faPaperPlane} spin={sendingTest} />
            Send Test
          </button>
          <button className="cb-btn cb-btn-primary ig-btn-glow ig-ripple" onClick={handleSubmit} disabled={saving}>
            <FontAwesomeIcon icon={saving ? faSpinner : faSave} spin={saving} />
            {saving ? 'Saving...' : 'Save Campaign'}
          </button>
        </div>
      </div>

      {/* Performance & Quotas Banner (edit mode only) */}
      {isEditing && (editStats || editStatsLoading) && (
        <div className="cb-perf-banner">
          <button
            className="cb-perf-banner-toggle"
            onClick={() => setEditStatsExpanded(prev => !prev)}
            aria-expanded={editStatsExpanded}
          >
            <FontAwesomeIcon icon={faBullseye} />
            <span className="cb-perf-banner-title">Performance &amp; Quotas</span>
            {!editStatsExpanded && editStats && (
              <span className="cb-perf-banner-summary">
                {editStats.sent.toLocaleString()} sent · {editStats.open_rate.toFixed(1)}% opens · {editStats.click_rate.toFixed(1)}% clicks
                {editStats.isp_breakdown && editStats.isp_breakdown.length > 0 && (
                  <> · {editStats.isp_breakdown.length} ISPs</>
                )}
              </span>
            )}
            <FontAwesomeIcon icon={editStatsExpanded ? faChevronUp : faChevronDown} className="cb-perf-banner-chevron" />
          </button>

          {editStatsExpanded && (
            <div className="cb-perf-banner-content">
              {editStatsLoading ? (
                <div style={{ textAlign: 'center', padding: '16px 0', color: 'rgba(180,210,240,0.55)' }}>
                  <FontAwesomeIcon icon={faSpinner} spin /> Loading stats...
                </div>
              ) : editStats ? (
                <>
                  {/* Summary metrics row */}
                  <div className="cb-perf-metrics-row">
                    <div className="cb-perf-metric">
                      <span className="cb-perf-metric-value">{editStats.sent.toLocaleString()}</span>
                      <span className="cb-perf-metric-label">Sent</span>
                    </div>
                    <div className="cb-perf-metric">
                      <span className="cb-perf-metric-value">{editStats.opens.toLocaleString()}</span>
                      <span className="cb-perf-metric-label">Opens ({editStats.open_rate.toFixed(1)}%)</span>
                    </div>
                    <div className="cb-perf-metric">
                      <span className="cb-perf-metric-value">{editStats.clicks.toLocaleString()}</span>
                      <span className="cb-perf-metric-label">Clicks ({editStats.click_rate.toFixed(1)}%)</span>
                    </div>
                    <div className="cb-perf-metric">
                      <span className="cb-perf-metric-value" style={{ color: '#ef4444' }}>{editStats.hard_bounces.toLocaleString()}</span>
                      <span className="cb-perf-metric-label">Hard Bounces ({(editStats.hard_bounce_rate ?? 0).toFixed(1)}%)</span>
                    </div>
                    <div className="cb-perf-metric">
                      <span className="cb-perf-metric-value" style={{ color: '#f59e0b' }}>{editStats.soft_bounces.toLocaleString()}</span>
                      <span className="cb-perf-metric-label">Soft Bounces ({(editStats.soft_bounce_rate ?? 0).toFixed(1)}%)</span>
                    </div>
                    <div className="cb-perf-metric">
                      <span className="cb-perf-metric-value">{editStats.complaints.toLocaleString()}</span>
                      <span className="cb-perf-metric-label">Complaints ({editStats.complaint_rate.toFixed(3)}%)</span>
                    </div>
                    <div className="cb-perf-metric">
                      <span className="cb-perf-metric-value">{editStats.unsubscribes.toLocaleString()}</span>
                      <span className="cb-perf-metric-label">Unsubs ({editStats.unsubscribe_rate.toFixed(2)}%)</span>
                    </div>
                  </div>

                  {/* ISP Breakdown table */}
                  {editStats.isp_breakdown && editStats.isp_breakdown.length > 0 && (
                    <div style={{ overflowX: 'auto', marginTop: 12 }}>
                      <table className="isp-breakdown-table">
                        <thead>
                          <tr>
                            <th>ISP</th>
                            <th>Quota</th>
                            <th>Sent</th>
                            <th>Delivered</th>
                            <th>Opens</th>
                            <th>Clicks</th>
                            <th>Hard B.</th>
                            <th>Soft B.</th>
                            <th>Complaints</th>
                          </tr>
                        </thead>
                        <tbody>
                          {editStats.isp_breakdown.map(row => {
                            const ispLabels: Record<string, string> = {
                              gmail: 'Gmail', yahoo: 'Yahoo', microsoft: 'Microsoft',
                              apple: 'Apple iCloud', comcast: 'Comcast', charter: 'Charter/Spectrum',
                              cox: 'Cox', att: 'AT&T', verizon: 'Verizon', aol: 'AOL',
                            };
                            const pct = (n: number, d: number) => d > 0 ? (n / d * 100).toFixed(1) + '%' : '—';
                            const quotaUsed = row.quota && row.quota > 0 ? Math.round(row.sent / row.quota * 100) : null;
                            return (
                              <tr key={row.isp}>
                                <td style={{ fontWeight: 600, color: '#e0e6f0' }}>{ispLabels[row.isp] || row.isp}</td>
                                <td>
                                  {row.quota ? (
                                    <span style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 2 }}>
                                      <span>{row.quota.toLocaleString()}</span>
                                      {quotaUsed !== null && (
                                        <span style={{
                                          fontSize: '0.7rem',
                                          color: quotaUsed >= 90 ? '#ef4444' : quotaUsed >= 70 ? '#f59e0b' : '#22c55e',
                                          fontWeight: 700,
                                        }}>{quotaUsed}% used</span>
                                      )}
                                    </span>
                                  ) : <span style={{ color: 'rgba(180,210,240,0.35)' }}>—</span>}
                                </td>
                                <td>{row.sent.toLocaleString()}</td>
                                <td>{row.delivered.toLocaleString()}</td>
                                <td>
                                  <span>{row.opens.toLocaleString()}</span>
                                  <span style={{ fontSize: '0.75rem', color: 'rgba(180,210,240,0.55)', marginLeft: 4 }}>
                                    ({pct(row.opens, row.delivered)})
                                  </span>
                                </td>
                                <td>
                                  <span>{row.clicks.toLocaleString()}</span>
                                  <span style={{ fontSize: '0.75rem', color: 'rgba(180,210,240,0.55)', marginLeft: 4 }}>
                                    ({pct(row.clicks, row.delivered)})
                                  </span>
                                </td>
                                <td style={{ color: row.hard_bounces > 0 ? '#ef4444' : 'inherit' }}>{row.hard_bounces.toLocaleString()}</td>
                                <td style={{ color: row.soft_bounces > 0 ? '#f59e0b' : 'inherit' }}>{row.soft_bounces.toLocaleString()}</td>
                                <td style={{ color: row.complaints > 0 ? '#ef4444' : 'inherit' }}>{row.complaints.toLocaleString()}</td>
                              </tr>
                            );
                          })}
                        </tbody>
                      </table>
                    </div>
                  )}
                </>
              ) : null}
            </div>
          )}
        </div>
      )}

      {/* Progress Steps (ARIA-accessible) */}
      <nav aria-label="Campaign wizard steps" className="cb-steps-nav">
        <ol role="tablist" className="cb-steps">
          {steps.map((step, index) => {
            const isActive = currentStep === step.id;
            const isComplete = isStepComplete(step.id);
            return (
              <li key={step.id} role="presentation">
                <button
                  role="tab"
                  id={`tab-${step.id}`}
                  aria-selected={isActive}
                  aria-current={isActive ? 'step' : undefined}
                  aria-controls={`panel-${step.id}`}
                  aria-label={`Step ${index + 1}: ${step.label}${isComplete ? ' (complete)' : ''}`}
                  className={`cb-step ${isActive ? 'active' : ''} ${isComplete ? 'complete' : ''}`}
                  onClick={() => goToStep(step.id)}
                >
                  <span className="cb-step-number">
                    {isComplete && !isActive ? (
                      <FontAwesomeIcon icon={faCheckCircle} />
                    ) : (
                      index + 1
                    )}
                  </span>
                  <span className="cb-step-label">{step.label}</span>
                </button>
              </li>
            );
          })}
        </ol>
      </nav>

      {/* Content Area */}
      <div className="cb-content" role="tabpanel" id={`panel-${currentStep}`} aria-labelledby={`tab-${currentStep}`}>
        {/* Step 1: Purpose — AI Agent Wizard or Classic Mode */}
        {currentStep === 'purpose' && (
          <div className="cb-step-content">
            <div style={{ display: 'flex', justifyContent: 'flex-end', marginBottom: 12, gap: 8 }}>
              <button
                onClick={() => setUseAgentWizard(true)}
                className={`cb-mode-btn ${useAgentWizard ? 'active' : ''}`}
                style={{
                  padding: '6px 14px', borderRadius: 8, border: useAgentWizard ? '1px solid #00e5ff' : '1px solid #333',
                  background: useAgentWizard ? 'rgba(0,229,255,0.15)' : 'transparent', color: useAgentWizard ? '#00e5ff' : '#888',
                  cursor: 'pointer', fontSize: 12, fontWeight: 600
                }}
              >
                🤖 AI Agent Wizard
              </button>
              <button
                onClick={() => setUseAgentWizard(false)}
                className={`cb-mode-btn ${!useAgentWizard ? 'active' : ''}`}
                style={{
                  padding: '6px 14px', borderRadius: 8, border: !useAgentWizard ? '1px solid #00b0ff' : '1px solid #333',
                  background: !useAgentWizard ? 'rgba(0,176,255,0.15)' : 'transparent', color: !useAgentWizard ? '#33ccff' : '#888',
                  cursor: 'pointer', fontSize: 12, fontWeight: 600
                }}
              >
                📋 Classic Mode
              </button>
            </div>
            {useAgentWizard ? (
              <AgentConfigWizard
                initialOffer={initialOffer}
                onOfferConsumed={onOfferConsumed}
                onComplete={(config) => {
                  // When agent wizard completes, pre-fill form data and advance
                  if (config?.offer_name) {
                    setFormData(prev => ({ ...prev, name: `AI Campaign: ${config.offer_name}` }));
                  }
                  setCurrentStep('content');
                }}
                onCancel={() => setUseAgentWizard(false)}
              />
            ) : (
              <CampaignPurposeTab
                objective={objective}
                onChange={setObjective}
              />
            )}
          </div>
        )}

        {/* Step 2: Details */}
        {currentStep === 'details' && (
          <div className="cb-step-content">
            <div className="cb-section">
              <h2><FontAwesomeIcon icon={faFileAlt} /> Campaign Details</h2>
              <div className="cb-form-grid">
                <div className="cb-form-group">
                  <label htmlFor="cb-campaign-name">Campaign Name <span className="required" aria-hidden="true">*</span></label>
                  <input
                    id="cb-campaign-name"
                    type="text"
                    value={formData.name}
                    onChange={e => setFormData(prev => ({ ...prev, name: e.target.value }))}
                    placeholder="e.g., February Newsletter"
                    className="cb-input"
                    aria-required="true"
                  />
                  {!formData.name.trim() && <small className="cb-field-error" role="alert">Campaign name is required</small>}
                </div>
                <div className="cb-form-group">
                  <PersonalizedInput
                    label="Subject Line"
                    required
                    value={formData.subject}
                    onChange={value => setFormData(prev => ({ ...prev, subject: value }))}
                    placeholder="e.g., Hey {{ first_name }}, Your Weekly Update is Here!"
                    maxLength={SUBJECT_MAX_LENGTH}
                    showAISuggestions={true}
                    aiContext={{
                      htmlContent: formData.html_content,
                      campaignType: 'newsletter',
                      tone: 'professional',
                    }}
                  />
                </div>
                <div className="cb-form-group full-width">
                  <PersonalizedInput
                    label="Preview Text (Preheader)"
                    value={formData.preheader}
                    onChange={value => setFormData(prev => ({ ...prev, preheader: value }))}
                    placeholder="e.g., {{ first_name }}, see what's new this week..."
                    maxLength={PREHEADER_MAX_LENGTH}
                    hint="This text appears after the subject line in most email clients"
                    showAISuggestions={true}
                    aiSuggestionType="preheader"
                    aiContext={{
                      subjectLine: formData.subject,
                      htmlContent: formData.html_content,
                      tone: 'professional',
                    }}
                  />
                </div>
              </div>
            </div>

            <div className="cb-section">
              <h2><FontAwesomeIcon icon={faPaperPlane} /> Sending Profile</h2>
              <div className="cb-form-grid">
                <div className="cb-form-group">
                  <label>ESP Profile <span className="required">*</span></label>
                  <select value={selectedProfile} onChange={e => handleProfileChange(e.target.value)} className="cb-select">
                    <option value="">Select a sending profile</option>
                    {profiles.map(p => (
                      <option key={p.id} value={p.id}>
                        {p.name} ({p.vendor_type})
                      </option>
                    ))}
                  </select>
                </div>
                <div className="cb-form-group">
                  <label>From Name</label>
                  <input
                    type="text"
                    value={formData.from_name}
                    onChange={e => setFormData(prev => ({ ...prev, from_name: e.target.value }))}
                    placeholder="Sender name"
                    className="cb-input"
                  />
                </div>
                <div className="cb-form-group">
                  <label>From Email</label>
                  <input
                    type="email"
                    value={formData.from_email}
                    onChange={e => setFormData(prev => ({ ...prev, from_email: e.target.value }))}
                    placeholder="sender@example.com"
                    className="cb-input"
                  />
                </div>
                <div className="cb-form-group">
                  <label>Reply-To Email (optional)</label>
                  <input
                    type="email"
                    value={formData.reply_to}
                    onChange={e => setFormData(prev => ({ ...prev, reply_to: e.target.value }))}
                    placeholder="reply@example.com"
                    className="cb-input"
                  />
                </div>
              </div>
            </div>
          </div>
        )}

        {/* Step 2: Audience */}
        {currentStep === 'audience' && (
          <div className="cb-step-content">
            <div className="cb-section">
              <h2><FontAwesomeIcon icon={faUsers} /> Target Audience</h2>
              <p className="cb-section-desc">Select one or more segments to send this campaign to. Segments can span multiple lists.</p>
              
              <SearchableMultiSelect
                options={segments}
                selected={selectedSegments}
                onChange={setSelectedSegments}
                label="Target Segments"
                type="segment"
                placeholder="Search segments by name..."
                emptyMessage="No segments available. Create segments in the Segments section."
                showEstimate={true}
              />
            </div>

            <div className="cb-section">
              <h2><FontAwesomeIcon icon={faBan} /> Suppression Lists</h2>
              <p className="cb-section-desc">Contacts on these lists will be excluded from this send. Global suppressions are always applied.</p>
              
              <SearchableMultiSelect
                options={suppressionLists}
                selected={selectedSuppressions}
                onChange={setSelectedSuppressions}
                label="Suppression Lists"
                type="suppression"
                placeholder="Search suppression lists..."
                emptyMessage="No suppression lists available."
                showEstimate={true}
              />
            </div>

            <div className="cb-section">
              <h2><FontAwesomeIcon icon={faBan} /> Suppression Segments</h2>
              <p className="cb-section-desc">Contacts matching these segment conditions will be excluded. Use this to exclude audiences previously targeted or with specific attributes.</p>
              
              <SearchableMultiSelect
                options={segments.map(s => ({ ...s, type: 'suppression' as const }))}
                selected={selectedSuppressionSegments}
                onChange={setSelectedSuppressionSegments}
                label="Suppression Segments"
                type="suppression"
                placeholder="Search segments to exclude..."
                emptyMessage="No segments available."
                showEstimate={true}
              />
              {selectedSuppressionSegments.length > 0 && (
                <div className="cb-suppression-warning">
                  <FontAwesomeIcon icon={faBan} />
                  <span>{selectedSuppressionSegments.length} segment(s) will be excluded from this send</span>
                </div>
              )}
            </div>

            {estimatedReach > 0 && (
              <div className="cb-audience-summary">
                <FontAwesomeIcon icon={faUsers} />
                <div>
                  <strong>Estimated Reach</strong>
                  <span>{estimatedReach.toLocaleString()} subscribers</span>
                </div>
              </div>
            )}
          </div>
        )}

        {/* Step 3: Content */}
        {currentStep === 'content' && (
          <div className="cb-step-content cb-content-step">
            <div className="cb-section full-height">
              <h2><FontAwesomeIcon icon={faEnvelope} /> Email Content</h2>
              
              {/* Everflow Creative Selector - Pull creatives from network */}
              <EverflowCreativeSelector
                organizationId={organization?.id}
                onCreativeSelect={(html, creativeId, offerId, trackingLink) => {
                  setFormData(prev => ({ ...prev, html_content: html }));
                  setCreativeMode(true);
                  setCreativeCodeView(false);
                  // Store the everflow creative metadata for campaign save
                  (window as any).__everflowCreative = { creativeId, offerId, trackingLink };
                }}
              />

              {/* Creative Mode: Raw HTML preview + code editor (bypasses TipTap) */}
              {creativeMode ? (
                <div className="cb-creative-content">
                  <style>{`
                    .cb-creative-content { border: 1px solid rgba(0,200,255,0.08); border-radius: 10px; overflow: hidden; background: #0d1526; }
                    .cb-creative-toolbar { display: flex; align-items: center; gap: 10px; padding: 10px 14px; border-bottom: 1px solid rgba(0,200,255,0.08); background: rgba(0,200,255,0.03); }
                    .cb-creative-toolbar .cb-ct-badge { background: #00b894; color: white; padding: 3px 10px; border-radius: 6px; font-size: 11px; font-weight: 600; display: flex; align-items: center; gap: 5px; }
                    .cb-creative-toolbar .cb-ct-label { color: rgba(180,210,240,0.5); font-size: 12px; flex: 1; }
                    .cb-creative-toolbar button { background: rgba(0,200,255,0.08); border: 1px solid rgba(0,200,255,0.15); color: #e0e6f0; padding: 5px 12px; border-radius: 6px; cursor: pointer; font-size: 12px; }
                    .cb-creative-toolbar button:hover { background: rgba(0,200,255,0.12); }
                    .cb-creative-toolbar button.active { background: #00b0ff; border-color: #00b0ff; color: white; }
                    .cb-creative-toolbar .cb-ct-switch { background: rgba(233,69,96,0.15); border-color: rgba(233,69,96,0.3); color: #e94560; }
                    .cb-creative-toolbar .cb-ct-switch:hover { background: rgba(233,69,96,0.25); }
                    .cb-creative-preview iframe { width: 100%; height: 600px; border: none; background: white; }
                    .cb-creative-code textarea { width: 100%; height: 500px; background: #0d1117; color: #c9d1d9; border: none; padding: 14px; font-family: 'Fira Code', 'Consolas', monospace; font-size: 13px; line-height: 1.5; resize: vertical; outline: none; }
                  `}</style>
                  <div className="cb-creative-toolbar">
                    <span className="cb-ct-badge">
                      <FontAwesomeIcon icon={faCheckCircle} /> Everflow Creative Applied
                    </span>
                    <span className="cb-ct-label">
                      Raw email HTML preserved with tracking links intact
                    </span>
                    <button
                      className={!creativeCodeView ? 'active' : ''}
                      onClick={() => setCreativeCodeView(false)}
                    >
                      <FontAwesomeIcon icon={faEye} /> Preview
                    </button>
                    <button
                      className={creativeCodeView ? 'active' : ''}
                      onClick={() => setCreativeCodeView(true)}
                    >
                      <FontAwesomeIcon icon={faCode} /> HTML Code
                    </button>
                    <button
                      className="cb-ct-switch"
                      onClick={() => setCreativeMode(false)}
                    >
                      Switch to WYSIWYG
                    </button>
                  </div>
                  {!creativeCodeView ? (
                    <div className="cb-creative-preview">
                      <iframe
                        srcDoc={formData.html_content}
                        title="Creative Preview"
                        sandbox="allow-same-origin"
                      />
                    </div>
                  ) : (
                    <div className="cb-creative-code">
                      <textarea
                        value={formData.html_content}
                        onChange={(e) => setFormData(prev => ({ ...prev, html_content: e.target.value }))}
                        spellCheck={false}
                      />
                    </div>
                  )}
                </div>
              ) : (
                <EmailEditor
                  content={formData.html_content}
                  onChange={(content) => setFormData(prev => ({ ...prev, html_content: content }))}
                  placeholder="Start designing your email..."
                  minHeight={500}
                />
              )}
            </div>
          </div>
        )}

        {/* Step 4: Schedule */}
        {currentStep === 'schedule' && (
          <div className="cb-step-content">
            <div className="cb-section">
              <h2><FontAwesomeIcon icon={faCalendarAlt} /> Schedule Send</h2>
              <p className="cb-section-desc">Choose when to send this campaign. Leave empty to save as draft.</p>
              
              <div className="cb-schedule-options">
                <label className="cb-schedule-option">
                  <input
                    type="radio"
                    name="schedule"
                    checked={!formData.scheduled_at}
                    onChange={() => setFormData(prev => ({ ...prev, scheduled_at: '' }))}
                  />
                  <div className="cb-schedule-option-content">
                    <FontAwesomeIcon icon={faFileAlt} />
                    <div>
                      <strong>Save as Draft</strong>
                      <span>Save and send later manually</span>
                    </div>
                  </div>
                </label>
                
                <label className="cb-schedule-option">
                  <input
                    type="radio"
                    name="schedule"
                    checked={!!formData.scheduled_at}
                    onChange={() => {
                      const defaultTime = new Date();
                      defaultTime.setHours(defaultTime.getHours() + 1);
                      defaultTime.setMinutes(0);
                      setFormData(prev => ({
                        ...prev,
                        scheduled_at: defaultTime.toISOString().slice(0, 16)
                      }));
                    }}
                  />
                  <div className="cb-schedule-option-content">
                    <FontAwesomeIcon icon={faCalendarAlt} />
                    <div>
                      <strong>Schedule for Later</strong>
                      <span>Set a specific date and time</span>
                    </div>
                  </div>
                </label>
              </div>

              {formData.scheduled_at && (
                <div className="cb-schedule-picker">
                  <label htmlFor="cb-send-datetime">Send Date & Time <span className="required" aria-hidden="true">*</span></label>
                  <input
                    id="cb-send-datetime"
                    type="datetime-local"
                    value={formData.scheduled_at}
                    onChange={e => setFormData(prev => ({ ...prev, scheduled_at: e.target.value }))}
                    className="cb-input cb-datetime"
                    min={new Date(Date.now() + 6 * 60000).toISOString().slice(0, 16)}
                    aria-required="true"
                  />
                  {formData.scheduled_at && new Date(formData.scheduled_at) < new Date() && (
                    <small className="cb-field-error" role="alert">
                      <FontAwesomeIcon icon={faExclamationCircle} /> Scheduled time must be in the future
                    </small>
                  )}
                  <small className="cb-hint">
                    <FontAwesomeIcon icon={faInfoCircle} /> Campaigns must be scheduled at least 5 minutes in advance
                  </small>
                </div>
              )}
            </div>

            <div className="cb-section">
              <h2><FontAwesomeIcon icon={faTachometerAlt} /> Send Speed</h2>
              <p className="cb-section-desc">Control how quickly your campaign is delivered. Slower speeds are better for ISP reputation.</p>
              <div className="cb-throttle-grid">
                {[
                  { value: 'instant', label: 'Instant', rate: 'Full speed' },
                  { value: 'gentle', label: 'Gentle', rate: '~500/min' },
                  { value: 'moderate', label: 'Moderate', rate: '~250/min' },
                  { value: 'careful', label: 'Careful', rate: '~125/min' },
                ].map(opt => (
                  <button
                    key={opt.value}
                    type="button"
                    className={`cb-throttle-option ${formData.throttle_speed === opt.value ? 'active' : ''}`}
                    onClick={() => setFormData(prev => ({ ...prev, throttle_speed: opt.value }))}
                    aria-pressed={formData.throttle_speed === opt.value}
                  >
                    <div className="throttle-label">{opt.label}</div>
                    <div className="throttle-rate">{opt.rate}</div>
                  </button>
                ))}
              </div>
              {estimatedReach > 0 && (
                <div className="cb-send-estimate">
                  <FontAwesomeIcon icon={faInfoCircle} />
                  Estimated send duration: <strong>
                    {(() => {
                      const rate = formData.throttle_speed === 'gentle' ? 500 :
                        formData.throttle_speed === 'moderate' ? 250 :
                        formData.throttle_speed === 'careful' ? 125 : 50000;
                      const minutes = Math.ceil(estimatedReach / rate);
                      if (minutes < 60) return `~${minutes} min`;
                      const h = Math.floor(minutes / 60);
                      const m = minutes % 60;
                      return `~${h}h ${m}m`;
                    })()}
                  </strong> for {estimatedReach.toLocaleString()} recipients
                </div>
              )}
            </div>
          </div>
        )}

        {/* Step 5: Review */}
        {currentStep === 'review' && (
          <div className="cb-step-content">
            <div className="cb-section">
              <h2><FontAwesomeIcon icon={faCheckCircle} /> Review Campaign</h2>
              <p className="cb-section-desc">Review your campaign settings before saving.</p>
              
              <div className="cb-review-grid">
                <div className="cb-review-card cb-review-purpose">
                  <h3><FontAwesomeIcon icon={faBullseye} /> Campaign Purpose</h3>
                  <dl>
                    <dt>Purpose</dt>
                    <dd>
                      {objective.purpose === 'data_activation' 
                        ? 'Data Activation'
                        : 'Offer Revenue'
                      }
                    </dd>
                    {objective.purpose === 'data_activation' && (
                      <>
                        <dt>Goal</dt>
                        <dd>
                          {objective.activation_goal === 'warm_new_data' && 'Warm New Data'}
                          {objective.activation_goal === 'reactivate_cold' && 'Reactivate Cold'}
                          {objective.activation_goal === 'domain_warmup' && 'Domain Warmup'}
                          {!objective.activation_goal && <em>Not set</em>}
                        </dd>
                      </>
                    )}
                    {objective.purpose === 'offer_revenue' && (
                      <>
                        <dt>Model</dt>
                        <dd>{objective.offer_model?.toUpperCase() || <em>Not set</em>}</dd>
                        {objective.budget_limit && (
                          <>
                            <dt>Budget</dt>
                            <dd>${objective.budget_limit.toLocaleString()}</dd>
                          </>
                        )}
                        {objective.ecpm_target && (
                          <>
                            <dt>ECM Target</dt>
                            <dd>${objective.ecpm_target.toFixed(2)}</dd>
                          </>
                        )}
                      </>
                    )}
                    <dt>AI Optimization</dt>
                    <dd>
                      {objective.ai_optimization_enabled || objective.ai_throughput_optimization 
                        ? <span className="cb-status-good"><FontAwesomeIcon icon={faCheckCircle} /> Enabled</span>
                        : <span className="cb-status-warn">Disabled</span>
                      }
                    </dd>
                  </dl>
                  <button className="cb-review-edit" onClick={() => goToStep('purpose')}>
                    <FontAwesomeIcon icon={faEdit} /> Edit
                  </button>
                </div>

                <div className="cb-review-card">
                  <h3>Campaign Details</h3>
                  <dl>
                    <dt>Name</dt>
                    <dd>{formData.name || <em>Not set</em>}</dd>
                    <dt>Subject</dt>
                    <dd>{formData.subject || <em>Not set</em>}</dd>
                    <dt>Preview Text</dt>
                    <dd>{formData.preheader || <em>Not set</em>}</dd>
                    <dt>From</dt>
                    <dd>{formData.from_name} &lt;{formData.from_email}&gt;</dd>
                  </dl>
                  <button className="cb-review-edit" onClick={() => goToStep('details')}>
                    <FontAwesomeIcon icon={faEdit} /> Edit
                  </button>
                </div>

                <div className="cb-review-card">
                  <h3>Audience</h3>
                  <dl>
                    <dt>Segments</dt>
                    <dd>
                      {selectedSegments.length > 0 
                        ? segments.filter(s => selectedSegments.includes(s.id)).map(s => s.name).join(', ')
                        : <em>No segments selected</em>
                      }
                    </dd>
                    <dt>Estimated Reach</dt>
                    <dd>{estimatedReach.toLocaleString()} subscribers</dd>
                    <dt>Suppression Lists</dt>
                    <dd>{selectedSuppressions.length} list(s) applied</dd>
                    <dt>Suppression Segments</dt>
                    <dd>{selectedSuppressionSegments.length} segment(s) excluded</dd>
                  </dl>
                  <button className="cb-review-edit" onClick={() => goToStep('audience')}>
                    <FontAwesomeIcon icon={faEdit} /> Edit
                  </button>
                </div>

                <div className="cb-review-card">
                  <h3>Content</h3>
                  <dl>
                    <dt>Status</dt>
                    <dd>
                      {formData.html_content ? (
                        <span className="cb-status-good"><FontAwesomeIcon icon={faCheckCircle} /> Content ready</span>
                      ) : (
                        <span className="cb-status-warn"><FontAwesomeIcon icon={faExclamationTriangle} /> No content</span>
                      )}
                    </dd>
                  </dl>
                  <button className="cb-review-edit" onClick={() => goToStep('content')}>
                    <FontAwesomeIcon icon={faEdit} /> Edit
                  </button>
                </div>

                <div className="cb-review-card">
                  <h3>Schedule & Delivery</h3>
                  <dl>
                    <dt>Send Time</dt>
                    <dd>
                      {formData.scheduled_at 
                        ? new Date(formData.scheduled_at).toLocaleString()
                        : 'Save as draft'
                      }
                    </dd>
                    <dt>Send Speed</dt>
                    <dd style={{ textTransform: 'capitalize' }}>{formData.throttle_speed || 'Instant'}</dd>
                    {estimatedReach > 0 && (
                      <>
                        <dt>Estimated Duration</dt>
                        <dd>
                          {(() => {
                            const rate = formData.throttle_speed === 'gentle' ? 500 :
                              formData.throttle_speed === 'moderate' ? 250 :
                              formData.throttle_speed === 'careful' ? 125 : 50000;
                            const minutes = Math.ceil(estimatedReach / rate);
                            if (minutes < 60) return `~${minutes} min`;
                            const h = Math.floor(minutes / 60);
                            const m = minutes % 60;
                            return `~${h}h ${m}m`;
                          })()}
                        </dd>
                      </>
                    )}
                  </dl>
                  <button className="cb-review-edit" onClick={() => goToStep('schedule')} aria-label="Edit schedule">
                    <FontAwesomeIcon icon={faEdit} /> Edit
                  </button>
                </div>
              </div>
            </div>
          </div>
        )}
      </div>

      {/* Footer Navigation */}
      <div className="cb-footer" role="navigation" aria-label="Campaign step navigation">
        <button 
          className="cb-btn cb-btn-secondary" 
          onClick={prevStep}
          disabled={currentStep === 'details'}
          aria-label="Go to previous step"
        >
          <FontAwesomeIcon icon={faArrowUp} style={{ transform: 'rotate(-90deg)' }} /> Previous
        </button>
        <div className="cb-footer-center">
          <button 
            className="cb-btn cb-btn-outline" 
            onClick={openTestModal}
            disabled={!formData.html_content || !formData.subject}
            title={!formData.html_content ? 'Add email content first' : 'Send a test/proof email'}
          >
            <FontAwesomeIcon icon={faPaperPlane} /> Send Test/Proof
          </button>
          <span className="cb-step-indicator">Step {steps.findIndex(s => s.id === currentStep) + 1} of {steps.length}</span>
        </div>
        {currentStep !== 'review' ? (
          <button className="cb-btn cb-btn-primary" onClick={nextStep} aria-label="Go to next step">
            Next <FontAwesomeIcon icon={faArrowRight} />
          </button>
        ) : (
          <button className="cb-btn cb-btn-success ig-btn-glow ig-ripple" onClick={handleSubmit} disabled={saving} aria-label={formData.scheduled_at ? 'Schedule campaign' : 'Save draft'}>
            <FontAwesomeIcon icon={saving ? faSpinner : faCheckCircle} spin={saving} />
            {saving ? 'Saving...' : (formData.scheduled_at ? 'Schedule Campaign' : 'Save Draft')}
          </button>
        )}
      </div>

      {/* Test/Proof Email Modal */}
      {showTestModal && (
        <div className="cb-modal-overlay" onClick={() => setShowTestModal(false)} role="dialog" aria-modal="true" aria-label="Send test email">
          <div className="cb-modal cb-test-modal" onClick={e => e.stopPropagation()}>
            <div className="cb-modal-header">
              <h2><FontAwesomeIcon icon={faPaperPlane} /> Send Test/Proof Email</h2>
              <button className="cb-modal-close" onClick={() => setShowTestModal(false)} aria-label="Close test email dialog">
                <FontAwesomeIcon icon={faTimes} />
              </button>
            </div>
            
            <div className="cb-modal-body">
              <p className="cb-modal-desc">
                Send a test email to preview how your campaign will look in real inboxes.
              </p>

              <div className="cb-test-form">
                <div className="cb-form-group">
                  <label>
                    <FontAwesomeIcon icon={faEnvelope} /> Recipient Email(s)
                    <span className="cb-label-hint">Separate multiple emails with commas</span>
                  </label>
                  <textarea
                    className="cb-input cb-textarea"
                    value={testEmails}
                    onChange={e => setTestEmails(e.target.value)}
                    placeholder="Enter email addresses...&#10;example@gmail.com, colleague@company.com"
                    rows={3}
                  />
                </div>

                <div className="cb-form-group">
                  <label>
                    <FontAwesomeIcon icon={faFileAlt} /> Subject Line Prefix
                    <span className="cb-label-hint">Added before your subject to identify test emails</span>
                  </label>
                  <input
                    type="text"
                    className="cb-input"
                    value={testSubjectPrefix}
                    onChange={e => setTestSubjectPrefix(e.target.value)}
                    placeholder="[TEST] "
                  />
                </div>

                <div className="cb-test-preview">
                  <h4>Preview</h4>
                  <div className="cb-preview-item">
                    <span className="cb-preview-label">Subject:</span>
                    <span className="cb-preview-value">{testSubjectPrefix}{formData.subject}</span>
                  </div>
                  <div className="cb-preview-item">
                    <span className="cb-preview-label">From:</span>
                    <span className="cb-preview-value">{formData.from_name} &lt;{formData.from_email}&gt;</span>
                  </div>
                  <div className="cb-preview-item">
                    <span className="cb-preview-label">Via:</span>
                    <span className="cb-preview-value">
                      {profiles.find(p => p.id === selectedProfile)?.name || 'Default Profile'} 
                      ({profiles.find(p => p.id === selectedProfile)?.vendor_type?.toUpperCase() || 'ESP'})
                    </span>
                  </div>
                </div>

                {testResult && (
                  <div className={`cb-test-result ${testResult.success ? 'success' : 'error'}`}>
                    {testResult.success ? (
                      <>
                        <FontAwesomeIcon icon={faCheckCircle} />
                        <div>
                          <strong>Test email sent successfully!</strong>
                          <p>Sent to: {testResult.to}</p>
                          {testResult.message_id && <p className="cb-message-id">ID: {testResult.message_id}</p>}
                        </div>
                      </>
                    ) : (
                      <>
                        <FontAwesomeIcon icon={faTimesCircle} />
                        <div>
                          <strong>Failed to send test email</strong>
                          <p>{testResult.error}</p>
                        </div>
                      </>
                    )}
                  </div>
                )}

                {testHistory.length > 0 && (
                  <div className="cb-test-history">
                    <h4>Recent Test Sends</h4>
                    <ul>
                      {testHistory.map((item, idx) => (
                        <li key={idx}>
                          <FontAwesomeIcon icon={faCheckCircle} className="cb-history-icon" />
                          <span className="cb-history-email">{item.to}</span>
                          <span className="cb-history-time">
                            {item.sent_at ? new Date(item.sent_at).toLocaleTimeString() : 'Just now'}
                          </span>
                        </li>
                      ))}
                    </ul>
                  </div>
                )}
              </div>
            </div>

            <div className="cb-modal-footer">
              <button 
                className="cb-btn cb-btn-secondary" 
                onClick={() => setShowTestModal(false)}
              >
                Close
              </button>
              <button 
                className="cb-btn cb-btn-primary" 
                onClick={handleSendTest}
                disabled={sendingTest || !testEmails.trim()}
              >
                <FontAwesomeIcon icon={sendingTest ? faSpinner : faPaperPlane} spin={sendingTest} />
                {sendingTest ? 'Sending...' : 'Send Test Email'}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
};

// ============================================================================
// MAIN CAMPAIGN PORTAL
// ============================================================================

export const CampaignPortal: React.FC<{
  initialOffer?: { offerId: string; offerName: string } | null;
  onOfferConsumed?: () => void;
  onEditInWizard?: (campaignId: string) => void;
}> = ({ initialOffer, onOfferConsumed, onEditInWizard }) => {
  const { organization } = useAuth();
  const [view, setView] = useState<ViewType>(initialOffer ? 'create' : 'campaigns');

  // When initialOffer changes externally, switch to create view
  useEffect(() => {
    if (initialOffer) {
      setView('create');
    }
  }, [initialOffer]);
  const [campaigns, setCampaigns] = useState<Campaign[]>([]);
  const [selectedCampaign, setSelectedCampaign] = useState<Campaign | null>(null);
  const [selectedCampaignStats, setSelectedCampaignStats] = useState<CampaignStats | null>(null);
  const [campaignVariants, setCampaignVariants] = useState<CampaignVariant[]>([]);
  // Legacy full-record modal (kept until the inline expand reaches full parity
  // — it still owns creative variants, the audience funnel, queue depth and
  // pause/cancel/edit actions). Opened via "Full record" in the inline expand.
  const [modalOpen, setModalOpen] = useState(false);
  const [summaryDetail, setSummaryDetail] = useState<CampaignSummaryDetail | null>(null);
  const [summaryDetailError, setSummaryDetailError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [detailsLoading, setDetailsLoading] = useState(false);
  const [filter, setFilter] = useState<ListStatusFilter>('all');

  // v2.0: /analytics/campaign-summary is consumed for IDENTITY/STATUS ONLY —
  // its counter-derived metric fields are never rendered. Per-row metrics come
  // from /campaigns/list-metrics inside CampaignCenterList (tracking events).
  const [summaryRows, setSummaryRows] = useState<CampaignSummaryRow[]>([]);
  const [summaryLoading, setSummaryLoading] = useState(true);
  const [summaryError, setSummaryError] = useState<string | null>(null);
  const [summaryDataAsOf, setSummaryDataAsOf] = useState<string | null>(null);

  // Fetch the raw campaign page — still needed for the nav-tab counts and the
  // sending/scheduled auto-refresh trigger (identity only, no metrics use).
  const fetchDashboardStats = useCallback(async (background = false) => {
    if (!background) setLoading(true);
    try {
      const campaignsRes = await orgFetch(`${API_BASE}/campaigns?limit=200`, organization?.id);
      const campaignsData = await campaignsRes.json();
      const allCampaigns: Campaign[] = campaignsData.data || campaignsData.campaigns || [];
      setCampaigns(allCampaigns);
    } catch (err) {
      console.error('Failed to fetch campaigns:', err);
    } finally {
      if (!background) setLoading(false);
    }
  }, [organization?.id]);

  // Fetch the campaign LIST identity rows (rollup mode keeps partner-drip
  // programs as one row per vertical tag — exactly what the drip toggle needs).
  const fetchCampaignSummary = useCallback(async (background = false) => {
    if (!background) setSummaryLoading(true);
    try {
      const res = await orgFetch(`${API_BASE}/analytics/campaign-summary?limit=200`, organization?.id);
      let data: CampaignSummaryResponse;
      try {
        data = await res.json();
      } catch {
        throw new Error(`HTTP ${res.status}`);
      }
      if (!res.ok || data.error) {
        throw new Error(data.error || `HTTP ${res.status}`);
      }
      setSummaryRows(Array.isArray(data.campaigns) ? data.campaigns : []);
      setSummaryDataAsOf(data.data_as_of || data.generated_at || null);
      setSummaryError(null);
    } catch (err) {
      console.error('Failed to fetch campaign summary:', err);
      setSummaryError(err instanceof Error ? err.message : 'Unknown error');
    } finally {
      if (!background) setSummaryLoading(false);
    }
  }, [organization?.id]);

  // Fetch campaign details (legacy full-record modal)
  const fetchCampaignDetails = useCallback(async (id: string) => {
    setDetailsLoading(true);
    // Reset the additive summary panel state so a stale campaign's funnel never
    // bleeds into the next one while the new fetch is in flight.
    setSummaryDetail(null);
    setSummaryDetailError(null);
    try {
      const [campaignRes, statsRes, variantsRes] = await Promise.all([
        orgFetch(`${API_BASE}/campaigns/${id}`, organization?.id),
        orgFetch(`${API_BASE}/campaigns/${id}/stats`, organization?.id),
        orgFetch(`${API_BASE}/campaigns/${id}/variants`, organization?.id)
          .catch(() => ({ json: async () => [] } as Response)),
      ]);
      const campaign = await campaignRes.json();
      const stats = await statsRes.json();
      const variants = await variantsRes.json();
      setSelectedCampaign(campaign);
      setSelectedCampaignStats(stats);
      setCampaignVariants(Array.isArray(variants) ? variants : []);
    } catch (err) {
      console.error('Failed to fetch campaign details:', err);
    } finally {
      setDetailsLoading(false);
    }

    // Accurate per-campaign summary + funnel, fetched independently so a
    // failure here never blocks the legacy /stats-based modal rendering.
    try {
      const res = await orgFetch(`${API_BASE}/analytics/campaign-summary/${id}`, organization?.id);
      let data: CampaignSummaryDetail;
      try {
        data = await res.json();
      } catch {
        throw new Error(`HTTP ${res.status}`);
      }
      if (!res.ok || data.error || !data.campaign) {
        throw new Error(data.error || `HTTP ${res.status}`);
      }
      setSummaryDetail(data);
      setSummaryDetailError(null);
    } catch (err) {
      console.error('Failed to fetch campaign summary detail:', err);
      setSummaryDetail(null);
      setSummaryDetailError(err instanceof Error ? err.message : 'Unknown error');
    }
  }, [organization]);

  // Handle campaign action
  const handleAction = useCallback(async (id: string, action: string) => {
    try {
      if (action === 'edit') {
        if (onEditInWizard) {
          onEditInWizard(id);
          return;
        }
        const res = await orgFetch(`${API_BASE}/campaigns/${id}`, organization?.id);
        const campaign = await res.json();
        setSelectedCampaign(campaign);
        setModalOpen(false);
        setView('edit');
        return;
      } else if (action === 'duplicate') {
        await orgFetch(`${API_BASE}/campaigns/${id}/duplicate`, organization?.id, { method: 'POST' });
      } else if (action === 'pause') {
        await orgFetch(`${API_BASE}/campaigns/${id}/pause`, organization?.id, { method: 'POST' });
      } else if (action === 'resume') {
        await orgFetch(`${API_BASE}/campaigns/${id}/resume`, organization?.id, { method: 'POST' });
      } else if (action === 'cancel') {
        await orgFetch(`${API_BASE}/campaigns/${id}/cancel`, organization?.id, { method: 'POST' });
      } else if (action === 'send') {
        await orgFetch(`${API_BASE}/campaigns/${id}/send`, organization?.id, { method: 'POST' });
      }
      // Refresh data
      fetchDashboardStats();
      fetchCampaignSummary();
      if (selectedCampaign?.id === id) {
        fetchCampaignDetails(id);
      }
    } catch (err) {
      console.error(`Failed to ${action} campaign:`, err);
    }
  }, [fetchDashboardStats, fetchCampaignSummary, fetchCampaignDetails, selectedCampaign, organization]);

  // Initial load
  useEffect(() => {
    fetchDashboardStats();
    fetchCampaignSummary();
  }, [fetchDashboardStats, fetchCampaignSummary]);

  // Auto-refresh for sending/scheduled campaigns — background mode preserves UI state
  useEffect(() => {
    const hasSendingCampaigns = campaigns.some(c => c.status === 'sending' || c.status === 'scheduled');
    if (!hasSendingCampaigns) return;

    const interval = setInterval(() => {
      fetchDashboardStats(true);
      fetchCampaignSummary(true);
    }, 15000);

    return () => clearInterval(interval);
  }, [campaigns.length, fetchDashboardStats, fetchCampaignSummary]);

  // Open the legacy full-record modal (from the inline expand's "Full record").
  // The list stays mounted underneath so expanded rows are preserved.
  const openCampaignModal = (id: string) => {
    fetchCampaignDetails(id);
    setModalOpen(true);
  };

  return (
    <div className="campaign-portal">
      {/* Header */}
      <div className="portal-header">
        <div className="header-title">
          <FontAwesomeIcon icon={faEnvelope} className="header-icon" />
          <div>
            <h1>Campaign Center</h1>
            <p>One list, inline analytics — expand any row for its send pace &amp; KPIs</p>
          </div>
        </div>
        <div className="header-actions">
          <span style={{ fontSize: 10, color: '#374151', alignSelf: 'center', marginRight: 8 }}>
            v{PAGE_VERSION_CAMPAIGN_PORTAL}
          </span>
          <button
            className="refresh-btn"
            onClick={() => { fetchDashboardStats(false); fetchCampaignSummary(false); }}
            disabled={loading || summaryLoading}
          >
            <FontAwesomeIcon icon={faSync} spin={loading || summaryLoading} /> Refresh
          </button>
        </div>
      </div>

      {/* Navigation Tabs */}
      <div className="portal-nav">
        <button
          className={`nav-tab ${view === 'campaigns' && filter !== 'scheduled' ? 'active' : ''}`}
          onClick={() => { setView('campaigns'); setFilter('all'); }}
        >
          <FontAwesomeIcon icon={faList} /> All Campaigns
          <span className="nav-count">{campaigns.length}</span>
        </button>
        <button
          className={`nav-tab ${view === 'campaigns' && filter === 'scheduled' ? 'active' : ''}`}
          onClick={() => { setView('campaigns'); setFilter('scheduled'); }}
        >
          <FontAwesomeIcon icon={faCalendarCheck} /> Scheduled
          <span className="nav-count">{campaigns.filter(c => c.status === 'scheduled').length}</span>
        </button>
      </div>

      {/* Main Content */}
      <div className="portal-content ig-fade-in">
        {view === 'campaigns' && (
          <CampaignCenterList
            rows={summaryRows}
            loading={summaryLoading}
            error={summaryError}
            dataAsOf={summaryDataAsOf}
            statusFilter={filter}
            onStatusFilterChange={setFilter}
            onOpenModal={openCampaignModal}
            onRefresh={() => fetchCampaignSummary(false)}
          />
        )}

        {(view === 'create' || view === 'edit') && (
          <CampaignEditor
            campaign={view === 'edit' ? selectedCampaign : null}
            initialOffer={view === 'create' ? initialOffer : null}
            onOfferConsumed={onOfferConsumed}
            onSave={() => {
              fetchDashboardStats();
              setView('campaigns');
            }}
            onCancel={() => setView('campaigns')}
          />
        )}
      </div>

      {/* Legacy full-record modal — variants / audience funnel / queue depth /
          actions. Retired only once the inline expand reaches full parity. */}
      {modalOpen && selectedCampaign && (
        <CampaignDetailsModal
          campaign={selectedCampaign}
          stats={selectedCampaignStats}
          variants={campaignVariants}
          loading={detailsLoading}
          summaryDetail={summaryDetail}
          summaryError={summaryDetailError}
          onClose={() => setModalOpen(false)}
          onAction={handleAction}
        />
      )}
    </div>
  );
};

export default CampaignPortal;
