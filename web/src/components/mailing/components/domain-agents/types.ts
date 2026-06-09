// Domain Agents — contract types for /api/mailing/domain-agent.
// Mirrors the Go backend's snake_case JSON exactly; do not rename fields.

export type Posture = 'healthy' | 'watch' | 'repair' | 'blocked' | 'unknown';

export type PlanStatus =
  | 'draft'
  | 'compiled'
  | 'approved'
  | 'deployed'
  | 'failed'
  | 'cancelled';

// GET /domains
export interface DomainSummary {
  domain: string;
  sends_7d: number;
  human_open_pct_7d: number;
  posture_worst: Posture;
  has_plan_today: boolean;
}

// GET /scorecard?domain=X&days=N
export interface ScorecardRow {
  day: string;
  sending_domain: string;
  isp: string;
  sends: number;
  delivered: number;
  human_opens: number;
  machine_opens: number;
  human_clicks: number;
  machine_clicks: number;
  hard_bounces: number;
  soft_bounces: number;
  other_bounces: number;
  complaints: number;
  unsubscribes: number;
}

export interface BriefingPosture {
  isp: string;
  posture: Posture;
  open_pct: number;
  human_clicks: number;
  machine_open_share: number;
  sends: number;
  reasons: string[];
}

export interface Briefing {
  domain: string;
  window_days: number;
  generated_at: string;
  postures: BriefingPosture[];
  flags: string[];
  volume_guidance: unknown;
  rationale: string;
}

// Editable plan slot (subject/preheader/creative/offer are operator-editable;
// scheduling & audience fields are read-only in the UI).
export interface PlanSlot {
  name: string;
  offer: string;
  creative: string;
  subject: string;
  preheader: string;
  scheduled_at: string;
  window_hours: number;
  inclusion_segments: string[];
  exclusion_lists: string[];
  target_isps?: string[];
  campaign_name?: string;
}

export interface Plan {
  id: string;
  sending_domain: string;
  plan_date: string;
  status: PlanStatus;
  briefing: Briefing;
  slots: PlanSlot[];
  payloads: unknown[];
  deploy_results: unknown[];
  approved_by?: string;
  approved_at?: string;
}

// POST /plans/{id}/approve response
export interface ApproveResultRow {
  name: string;
  campaign_id?: string;
  status?: string;
  error?: string;
}

export interface ApproveResponse {
  results: ApproveResultRow[];
  status: PlanStatus;
}

// GET /plans/{id}/status
export interface CampaignStatusRow {
  campaign_id: string;
  name: string;
  status: string;
  total_recipients: number;
  queued_count: number;
}

// Posture → StatusBadge status mapping (healthy→healthy, watch→warning,
// repair/blocked→critical, unknown→neutral).
export type BadgeStatus = 'healthy' | 'warning' | 'critical' | 'neutral';

export function postureToBadge(p: Posture): BadgeStatus {
  switch (p) {
    case 'healthy':
      return 'healthy';
    case 'watch':
      return 'warning';
    case 'repair':
    case 'blocked':
      return 'critical';
    default:
      return 'neutral';
  }
}

export const DOMAIN_AGENT_BASE = '/api/mailing/domain-agent';
