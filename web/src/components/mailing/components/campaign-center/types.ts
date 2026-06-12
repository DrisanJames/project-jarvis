// Campaign Center list rebuild (Round-3 §1) — shared types + helpers for the
// LIST view and the inline row expansion. Metrics here are TRACKING-EVENT
// derived (backend campaign_timeseries.go); campaign counters are never used
// for metric columns.

// ── identity rows (from /analytics/campaign-summary — identity/status ONLY) ──

export type CampaignStatus =
  | 'draft' | 'scheduled' | 'preparing' | 'sending' | 'paused' | 'sent'
  | 'completed' | 'completed_with_errors' | 'cancelled' | 'failed';

// Structural subset of CampaignPortal's CampaignSummaryRow — only the
// identity/status fields the list is allowed to consume from that endpoint
// (its counter-derived metric fields are deliberately NOT in this type).
export interface CampaignIdentityRow {
  id: string;
  name: string;
  status: CampaignStatus;
  scheduled_at?: string;
  updated_at?: string;
  route_type: string;
  is_rollup?: boolean;
  wave_count?: number;
  partner_tag?: string;
  targeted: number; // planned audience (operator intent, not a tracking counter)
}

// ── /api/mailing/campaigns/list-metrics ──────────────────────────────────────

export interface ListMetrics {
  attempted: number;
  delivered: number;
  human_opens: number;
  human_clicks: number;
  unsubscribes: number;
  hard_bounces: number;
  soft_bounces: number;
  complaints: number;
}

export const EMPTY_METRICS: ListMetrics = {
  attempted: 0, delivered: 0, human_opens: 0, human_clicks: 0,
  unsubscribes: 0, hard_bounces: 0, soft_bounces: 0, complaints: 0,
};

export interface ListEntry {
  subject: string;
  sending_domain: string;
  route_type: string;
  planned: number;
  offer_id?: string;
  scheduled_at?: string;
  first_wave_at?: string;
  window_start?: string;
  window_end?: string;
  wave_count: number;
  metrics: ListMetrics;
}

export interface TagEntry {
  wave_count: number;
  planned: number;
  first_wave_at?: string;
  last_wave_at?: string;
  route_type: string;
  metrics: ListMetrics;
}

export interface ListMetricsResponse {
  api_version?: string;
  source?: string;
  generated_at?: string;
  campaigns?: Record<string, ListEntry>;
  tags?: Record<string, TagEntry>;
  notes?: string;
  error?: string;
}

// ── /api/mailing/campaigns/{id}/timeseries ───────────────────────────────────

export interface TimeseriesBucket {
  t: string;
  attempted: number;
  delivered: number;
  opens: number; // HUMAN opens
  clicks: number;
  unsubscribes: number;
  bounces: number;
}

export interface WaveMarker {
  wave_number: number;
  scheduled_at: string;
  status: string;
  planned_recipients: number;
}

export interface TimeseriesResponse {
  api_version?: string;
  campaign_id?: string;
  source?: string;
  route_type?: string;
  bucket?: string;
  window_start?: string;
  window_end?: string;
  series?: TimeseriesBucket[];
  waves?: WaveMarker[];
  notes?: string;
  error?: string;
}

// ── /api/mailing/campaigns/{id}/detail ───────────────────────────────────────

export interface DetailKPIs {
  planned: number;
  attempted: number;
  delivered: number;
  human_opens: number;
  human_opens_total: number;
  human_clicks: number;
  clicks_total: number;
  unsubscribes: number;
  hard_bounces: number;
  soft_bounces: number;
  complaints: number;
  deferrals: number;
  conversions: number | null; // null = offer not resolvable → omit from UI
  delivered_rate: number;
  human_open_rate: number;
  human_click_rate: number;
  ctor: number;
  hard_bounce_rate: number;
  soft_bounce_rate: number;
  data_as_of?: string;
}

export interface DetailISPRow {
  isp: string;
  delivered: number;
  human_opens: number;
  human_clicks: number;
  hard: number;
  soft: number;
  deferrals: number;
}

export interface DetailLink {
  url: string;
  clicks: number;
  unique_clicks: number;
}

export interface DetailResponse {
  api_version?: string;
  source?: string;
  campaign?: {
    id: string;
    name: string;
    status: string;
    route_type: string;
    campaign_type?: string;
    partner_drip_tag?: string;
    subject?: string;
    preheader?: string;
    from_name?: string;
    from_email?: string;
    sending_domain?: string;
    sending_profile?: string;
    offer_name?: string;
    scheduled_at?: string;
    created_at?: string;
  };
  window?: {
    first_wave_at?: string;
    window_start?: string;
    window_end?: string;
    wave_count: number;
    throttle_speed?: string;
    throttle_rate_per_minute?: number;
    throttle_duration_hours?: number;
  };
  segments?: { inclusion: string[]; exclusion: string[] };
  kpis?: DetailKPIs;
  isp_breakdown?: DetailISPRow[];
  top_links?: DetailLink[];
  waves?: WaveMarker[];
  notes?: string;
  error?: string;
}

// ── helpers ──────────────────────────────────────────────────────────────────

// Offer parsed from the campaign-name suffix after the last ' - ' (operator
// naming convention, e.g. "DB Engager AM - Warby Parker" → "Warby Parker").
export const parseOfferFromName = (name: string): string => {
  const i = (name || '').lastIndexOf(' - ');
  if (i <= 0) return '—';
  const suffix = name.slice(i + 3).trim();
  return suffix || '—';
};

export const isDripIdentityRow = (r: CampaignIdentityRow): boolean =>
  !!r.is_rollup || (r.name || '').startsWith('[partner-drip]');

const DENVER_TZ = 'America/Denver';

// Start-of-day instant in America/Denver, offset by `dayOffset` days.
export const denverDayStart = (dayOffset = 0): Date => {
  const now = new Date();
  // Interpret "now" in Denver wall-clock, zero the time, then convert the
  // wall-clock back to a real instant using the rendered-offset trick.
  const denverWall = new Date(now.toLocaleString('en-US', { timeZone: DENVER_TZ }));
  const drift = now.getTime() - denverWall.getTime();
  denverWall.setHours(0, 0, 0, 0);
  denverWall.setDate(denverWall.getDate() + dayOffset);
  return new Date(denverWall.getTime() + drift);
};

export type DatePreset = 'today' | 'yesterday' | '7d' | '30d' | 'all';

export const DATE_PRESET_LABELS: Record<DatePreset, string> = {
  today: 'Today (Denver)',
  yesterday: 'Yesterday',
  '7d': 'Last 7 days',
  '30d': 'Last 30 days',
  all: 'All (90d)',
};

export const datePresetRange = (p: DatePreset): { from: Date; to: Date } => {
  const now = new Date();
  switch (p) {
    case 'today':     return { from: denverDayStart(0), to: now };
    case 'yesterday': return { from: denverDayStart(-1), to: denverDayStart(0) };
    case '7d':        return { from: denverDayStart(-6), to: now };
    case '30d':       return { from: denverDayStart(-29), to: now };
    default:          return { from: new Date(now.getTime() - 90 * 86400_000), to: now };
  }
};

export const fmtDenverTime = (iso?: string | null): string => {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleString('en-US', {
    timeZone: DENVER_TZ, month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit',
  });
};

export const fmtDenverClock = (iso?: string | null): string => {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleString('en-US', { timeZone: DENVER_TZ, hour: 'numeric', minute: '2-digit' });
};

// "8.0h (06:30–14:30 MT)" window summary.
export const fmtWindow = (startIso?: string, endIso?: string): string => {
  if (!startIso || !endIso) return '—';
  const s = new Date(startIso).getTime();
  const e = new Date(endIso).getTime();
  if (Number.isNaN(s) || Number.isNaN(e) || e <= s) return '—';
  const hours = (e - s) / 3600_000;
  const h = hours >= 10 ? Math.round(hours).toString() : hours.toFixed(1).replace(/\.0$/, '');
  return `${h}h (${fmtDenverClock(startIso)}–${fmtDenverClock(endIso)} MT)`;
};

export const pctStr = (num: number, den: number): string =>
  den > 0 ? ((num / den) * 100).toFixed(1) + '%' : '—';

export const numFmt = (n?: number): string => (n ?? 0).toLocaleString();
