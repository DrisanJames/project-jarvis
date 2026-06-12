import React, { useState, useEffect, useRef, useCallback, useMemo } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faBrain, faLightbulb, faCommentDots, faShieldAlt,
  faChartBar, faExclamationTriangle,
  faSyncAlt, faEye, faStream,
  faBolt, faTowerBroadcast, faGears,
} from '@fortawesome/free-solid-svg-icons';
import {
  ResponsiveContainer, LineChart, Line, AreaChart, Area,
  XAxis, YAxis, Tooltip, CartesianGrid, Legend,
} from 'recharts';
import { AnimatedCounter } from '../shared/AnimatedCounter';
import { apiFetch } from '../shared/apiFetch';

// ----------------------------------------------------------------------------
//  Types — engine consciousness
// ----------------------------------------------------------------------------

interface Philosophy {
  id: string;
  isp: string;
  domain: string;
  belief: string;
  explanation: string;
  confidence: number;
  evidence_count: number;
  category: string;
  sentiment: string;
  created_at: string;
  updated_at: string;
  strength: number;
  challenges: number;
  tags?: string[];
}

interface Thought {
  id: string;
  timestamp: string;
  isp?: string;
  agent_type?: string;
  type: string;
  content: string;
  reasoning?: string;
  confidence?: number;
  severity?: string;
  related_ids?: string[];
}

interface ConsciousnessState {
  philosophies: Philosophy[];
  recent_thoughts: Thought[];
  summary: string;
  health_score: number;
  mood: string;
  active_isps: number;
  total_beliefs: number;
  generated_at: string;
}

interface CampaignMetrics {
  campaign_id: string;
  started_at: string;
  updated_at: string;
  sent: number;
  delivered: number;
  soft_bounce: number;
  hard_bounce: number;
  complaints: number;
  unsubscribes: number;
  opens: number;
  unique_opens: number;
  clicks: number;
  unique_clicks: number;
  inactive: number;
  isp_metrics?: Record<string, any>;
  variant_metrics?: Record<string, any>;
}

// ----------------------------------------------------------------------------
//  Types — ISP operations (deliverability endpoints, pmta_acct_raw-backed)
// ----------------------------------------------------------------------------

interface MatrixCell {
  isp: string;
  sending_domain: string;
  attempted: number;
  delivered: number;
  hard_bounce: number;
  soft_bounce: number;
  deferred: number;
  complaint: number;
  reputation_blocked: number;
  accept_pct: number;
  defer_pct: number;
  hard_pct: number;
  soft_pct: number;
  complaint_pct: number;
}

interface ISPConfigEntry {
  isp: string;
  display_name: string;
  max_msg_rate: number;
  max_connections: number;
  bounce_warn_pct: number;
  bounce_action_pct: number;
  complaint_warn_pct: number;
  complaint_action_pct: number;
  pool_name: string;
  enabled: boolean;
  current_rate: number;
  rate_adjustment: number;
  backoff_count: number;
  in_recovery: boolean;
  ip_count: number;
  sent_1h: number;
  delivered_1h: number;
}

interface TimeseriesBucket {
  bucket: string;
  series: Record<string, number>;
}

interface TimeseriesResp {
  metric: string;
  keys: string[];
  buckets: TimeseriesBucket[];
}

/** Aggregated 24h row, one per recipient ISP. */
interface ISPRow {
  isp: string;
  attempted: number;
  delivered: number;
  hardBounce: number;
  softBounce: number;
  deferred: number;
  complaint: number;
  repBlocked: number;
  deliveredPct: number;
  deferPct: number;
  hardPct: number;
  softPct: number;
  complaintPct: number;
}

interface Suggestion {
  text: string;
  tone: 'good' | 'warn' | 'bad' | 'info';
}

// ----------------------------------------------------------------------------
//  Types — Hourly Assessment (consciousness/hourly-assessment, 60-min window,
//  cached 10 min server-side — poll on the same cadence, never faster)
// ----------------------------------------------------------------------------

interface HourlyPipe {
  isp: string;
  vmta: string;
  attempted: number;
  delivered: number;
  deferred: number;
  bounced: number;
  delivered_pct: number;
  deferral_pct: number;
  bounce_pct: number;
}

interface HourlyRecommendation {
  isp: string;
  type: string; // 'reroute' | 'pace_down'
  severity: string;
  message: string;
  bad_vmta?: string;
  bad_deferral_pct?: number;
  good_vmta?: string;
  good_deferral_pct?: number;
}

interface HourlyAssessmentResp {
  generated_at: string;
  window_minutes: number;
  cached: boolean;
  recommendations: HourlyRecommendation[] | null;
  pipes: HourlyPipe[] | null;
}

const TS_METRICS = ['delivered', 'deferred', 'hard_bounce', 'soft_bounce'] as const;
type TSMetric = typeof TS_METRICS[number];

// Engine agents are keyed gmail/yahoo/aol/microsoft/apple/comcast/att/sbcglobal/cox/charter.
// recipient_isp values occasionally use consumer-facing names — normalize for the join.
const CONFIG_ALIASES: Record<string, string> = {
  outlook: 'microsoft',
  hotmail: 'microsoft',
  live: 'microsoft',
  msn: 'microsoft',
  icloud: 'apple',
  me: 'apple',
  mac: 'apple',
  verizon: 'aol',
};

const ISP_DISPLAY: Record<string, string> = {
  gmail: 'Gmail',
  microsoft: 'Microsoft / Outlook',
  yahoo: 'Yahoo',
  aol: 'AOL',
  comcast: 'Comcast',
  charter: 'Charter',
  apple: 'Apple / iCloud',
  att: 'AT&T',
  sbcglobal: 'SBCGlobal',
  cox: 'Cox',
  other: 'Other',
};

const moodEmoji: Record<string, string> = {
  confident: '🟢',
  cautious: '🟡',
  concerned: '🟠',
  alert: '🔴',
  observing: '⚪',
};

const sentimentColor: Record<string, string> = {
  positive: '#00b894',
  cautious: '#fdcb6e',
  neutral: 'rgba(180,210,240,0.65)',
  negative: '#e94560',
};

const severityColor: Record<string, string> = {
  info: '#00b0ff',
  caution: '#fdcb6e',
  warning: '#f97316',
  critical: '#e94560',
};

const toneColor: Record<Suggestion['tone'], string> = {
  good: '#10b981',
  warn: '#f59e0b',
  bad: '#ef4444',
  info: '#93c5fd',
};

const thoughtTypeIcon: Record<string, any> = {
  observation: faEye,
  decision: faBolt,
  reflection: faLightbulb,
  warning: faExclamationTriangle,
  insight: faBrain,
  philosophy_update: faLightbulb,
};

function normalizeISP(isp: string): string {
  const k = (isp || 'other').toLowerCase();
  return CONFIG_ALIASES[k] || k;
}

function ispLabel(isp: string): string {
  return ISP_DISPLAY[isp] || isp.charAt(0).toUpperCase() + isp.slice(1);
}

function pctOf(num: number, denom: number): number {
  if (denom <= 0) return 0;
  return Math.round((num / denom) * 10000) / 100;
}

// ----------------------------------------------------------------------------
//  Suggestion engine — engine signals first, then the platform's own
//  thresholds (deferral storm >10%, hard-bounce hygiene >2%, headroom rule).
// ----------------------------------------------------------------------------

function buildSuggestions(row: ISPRow, cfg: ISPConfigEntry | undefined, engineThought: Thought | undefined): Suggestion[] {
  const out: Suggestion[] = [];

  if (engineThought) {
    const sev = engineThought.severity || 'info';
    const tone: Suggestion['tone'] = sev === 'critical' ? 'bad' : (sev === 'warning' || sev === 'caution') ? 'warn' : 'info';
    const content = engineThought.content.length > 110
      ? engineThought.content.slice(0, 107) + '…'
      : engineThought.content;
    out.push({ text: `Engine: ${content}`, tone });
  }

  if (row.attempted === 0) {
    out.push({ text: 'No traffic in last 24h', tone: 'info' });
    return out.slice(0, 3);
  }

  if (row.deferPct > 10) {
    out.push({ text: `Pace down — deferral storm watch (${row.deferPct.toFixed(1)}% deferred)`, tone: 'bad' });
  } else if (row.deferPct > 5) {
    out.push({ text: `Deferrals elevated (${row.deferPct.toFixed(1)}%) — watch pacing`, tone: 'warn' });
  }

  if (row.hardPct > 2) {
    out.push({ text: `Cut volume — hard bounces ${row.hardPct.toFixed(2)}%, check list hygiene`, tone: 'bad' });
  } else if (row.hardPct > 1) {
    out.push({ text: `Hard bounces ${row.hardPct.toFixed(2)}% — tighten list hygiene`, tone: 'warn' });
  }

  const repPct = pctOf(row.repBlocked, row.attempted);
  if (repPct > 1) {
    out.push({ text: `Reputation blocks ${repPct.toFixed(1)}% — review content & IP reputation`, tone: 'bad' });
  }

  if (row.complaintPct > 0.3) {
    out.push({ text: `Complaints ${row.complaintPct.toFixed(2)}% — suppress complainers, review targeting`, tone: 'bad' });
  } else if (row.complaintPct > 0.1) {
    out.push({ text: `Complaints ${row.complaintPct.toFixed(2)}% — keep an eye on FBL`, tone: 'warn' });
  }

  if (cfg) {
    if (cfg.in_recovery) {
      out.push({ text: 'Agent in recovery — hold volume steady, let it climb back', tone: 'warn' });
    } else if (cfg.backoff_count > 0) {
      out.push({ text: `Agent backing off (×${cfg.backoff_count}) — let pacing settle`, tone: 'warn' });
    } else if (!cfg.enabled) {
      out.push({ text: 'Agent disabled — sends are not rate-governed for this ISP', tone: 'warn' });
    }
  }

  const onlyEngineNote = out.length === (engineThought ? 1 : 0);
  if (onlyEngineNote) {
    if (row.deliveredPct >= 92 && row.hardPct < 1 && row.deferPct < 5) {
      out.push({ text: 'Healthy & stable — headroom to grow', tone: 'good' });
    } else {
      out.push({ text: 'Stable — monitor', tone: 'info' });
    }
  }

  return out.slice(0, 3);
}

function posture(cfg: ISPConfigEntry | undefined): { label: string; color: string } {
  if (!cfg) return { label: 'no agent', color: 'rgba(180,210,240,0.45)' };
  if (!cfg.enabled) return { label: 'disabled', color: 'rgba(180,210,240,0.45)' };
  if (cfg.in_recovery) return { label: 'recovering', color: '#f59e0b' };
  if (cfg.backoff_count > 0) return { label: `backoff ×${cfg.backoff_count}`, color: '#f59e0b' };
  if (cfg.rate_adjustment < 0.95) return { label: `throttled ${Math.round(cfg.rate_adjustment * 100)}%`, color: '#f59e0b' };
  return { label: 'full rate', color: '#10b981' };
}

// ----------------------------------------------------------------------------
//  Component
// ----------------------------------------------------------------------------

const REFRESH_MS = 20000;

export const ConsciousnessDashboard: React.FC = () => {
  const [mainTab, setMainTab] = useState<'operations' | 'engine'>('operations');

  // Engine consciousness state
  const [state, setState] = useState<ConsciousnessState | null>(null);
  const [campaigns, setCampaigns] = useState<CampaignMetrics[]>([]);
  const [liveThoughts, setLiveThoughts] = useState<Thought[]>([]);
  // Operator finds the thought firehose noisy — default to paused/snapshot;
  // the SSE stream only opens when explicitly toggled on.
  const [thoughtsLive, setThoughtsLive] = useState(false);
  const [activeSection, setActiveSection] = useState<'campaigns' | 'overview' | 'philosophies' | 'thoughts'>('overview');
  const [selectedISP, setSelectedISP] = useState<string>('all');
  const [loading, setLoading] = useState(true);
  const thoughtsEndRef = useRef<HTMLDivElement>(null);
  const eventSourceRef = useRef<EventSource | null>(null);

  // ISP operations state
  const [matrixCells, setMatrixCells] = useState<MatrixCell[]>([]);
  const [configs, setConfigs] = useState<ISPConfigEntry[]>([]);
  const [series, setSeries] = useState<Partial<Record<TSMetric, TimeseriesResp>>>({});
  const [lastUpdated, setLastUpdated] = useState<Date | null>(null);
  const [opsISP, setOpsISP] = useState<string | null>(null);

  const fetchState = useCallback(async () => {
    try {
      const [stateRes, campaignsRes, dbCampaignsRes] = await Promise.all([
        apiFetch('/api/mailing/consciousness/state'),
        apiFetch('/api/mailing/campaign-events/campaigns'),
        apiFetch('/api/mailing/campaigns'),
      ]);
      if (stateRes.ok) setState(await stateRes.json());
      const inMemory = campaignsRes.ok ? await campaignsRes.json() : [];
      let merged = Array.isArray(inMemory) ? inMemory : [];
      // If in-memory tracker is empty, build metrics from DB campaigns
      if (merged.length === 0 && dbCampaignsRes.ok) {
        const dbData = await dbCampaignsRes.json();
        const dbCamps = dbData.campaigns || [];
        merged = dbCamps
          .filter((c: any) => c.status === 'completed' || c.status === 'sending' || c.status === 'completed_with_errors')
          .map((c: any) => ({
            campaign_id: c.id,
            started_at: c.started_at || c.created_at,
            updated_at: c.updated_at,
            sent: c.sent_count || 0,
            delivered: c.sent_count || 0,
            soft_bounce: 0, hard_bounce: 0,
            complaints: 0,
            unsubscribes: c.unsubscribe_count || 0,
            opens: c.open_count || 0,
            unique_opens: c.open_count || 0,
            clicks: c.click_count || 0,
            unique_clicks: c.click_count || 0,
            inactive: 0,
          }));
      }
      setCampaigns(merged);
    } catch {
      // silent
    } finally {
      setLoading(false);
    }
  }, []);

  const fetchOps = useCallback(async () => {
    try {
      const [matrixRes, configRes, ...tsResults] = await Promise.all([
        apiFetch('/api/mailing/deliverability/matrix?window=24h'),
        apiFetch('/api/mailing/deliverability/config'),
        ...TS_METRICS.map(m =>
          apiFetch(`/api/mailing/deliverability/timeseries?metric=${m}&groupBy=isp&window=24h&bucket=1h`)
        ),
      ]);
      if (matrixRes.ok) {
        const data = await matrixRes.json();
        setMatrixCells(Array.isArray(data.cells) ? data.cells : []);
      }
      if (configRes.ok) {
        const data = await configRes.json();
        setConfigs(Array.isArray(data.configs) ? data.configs : []);
      }
      const nextSeries: Partial<Record<TSMetric, TimeseriesResp>> = {};
      for (let i = 0; i < TS_METRICS.length; i++) {
        const res = tsResults[i];
        if (res && res.ok) {
          nextSeries[TS_METRICS[i]] = await res.json();
        }
      }
      setSeries(prev => ({ ...prev, ...nextSeries }));
      setLastUpdated(new Date());
    } catch {
      // silent — keep last good data on screen
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchState();
    fetchOps();
    const interval = setInterval(() => {
      fetchState();
      fetchOps();
    }, REFRESH_MS);
    return () => clearInterval(interval);
  }, [fetchState, fetchOps]);

  useEffect(() => {
    if (!thoughtsLive) {
      eventSourceRef.current?.close();
      eventSourceRef.current = null;
      return;
    }
    const es = new EventSource('/api/mailing/consciousness/thoughts/stream');
    es.addEventListener('thought', (e) => {
      const thought: Thought = JSON.parse(e.data);
      setLiveThoughts(prev => {
        const next = [...prev, thought];
        return next.length > 200 ? next.slice(-200) : next;
      });
    });
    eventSourceRef.current = es;
    return () => {
      es.close();
      eventSourceRef.current = null;
    };
  }, [thoughtsLive]);

  useEffect(() => {
    if (activeSection === 'thoughts' && thoughtsLive) {
      thoughtsEndRef.current?.scrollIntoView({ behavior: 'smooth' });
    }
  }, [liveThoughts, activeSection, thoughtsLive]);

  const allThoughts = useMemo(() => [
    ...(state?.recent_thoughts || []),
    ...liveThoughts,
  ].sort((a, b) => new Date(b.timestamp).getTime() - new Date(a.timestamp).getTime()), [state, liveThoughts]);

  // ---- Aggregate the ISP × domain matrix into one row per ISP -------------
  const ispRows = useMemo<ISPRow[]>(() => {
    const byISP = new Map<string, ISPRow>();
    for (const c of matrixCells) {
      const key = (c.isp || 'other').toLowerCase();
      let row = byISP.get(key);
      if (!row) {
        row = {
          isp: key, attempted: 0, delivered: 0, hardBounce: 0, softBounce: 0,
          deferred: 0, complaint: 0, repBlocked: 0,
          deliveredPct: 0, deferPct: 0, hardPct: 0, softPct: 0, complaintPct: 0,
        };
        byISP.set(key, row);
      }
      row.attempted += c.attempted;
      row.delivered += c.delivered;
      row.hardBounce += c.hard_bounce;
      row.softBounce += c.soft_bounce;
      row.deferred += c.deferred;
      row.complaint += c.complaint;
      row.repBlocked += c.reputation_blocked;
    }
    // Include engine-governed ISPs that had no traffic in the window
    for (const cfg of configs) {
      const key = cfg.isp.toLowerCase();
      if (!byISP.has(key) && cfg.enabled) {
        byISP.set(key, {
          isp: key, attempted: 0, delivered: 0, hardBounce: 0, softBounce: 0,
          deferred: 0, complaint: 0, repBlocked: 0,
          deliveredPct: 0, deferPct: 0, hardPct: 0, softPct: 0, complaintPct: 0,
        });
      }
    }
    const rows = Array.from(byISP.values());
    for (const r of rows) {
      r.deliveredPct = pctOf(r.delivered, r.attempted);
      r.deferPct = pctOf(r.deferred, r.attempted);
      r.hardPct = pctOf(r.hardBounce, r.attempted);
      r.softPct = pctOf(r.softBounce, r.attempted);
      r.complaintPct = pctOf(r.complaint, r.attempted);
    }
    rows.sort((a, b) => b.attempted - a.attempted);
    return rows;
  }, [matrixCells, configs]);

  const configByISP = useMemo(() => {
    const m = new Map<string, ISPConfigEntry>();
    for (const c of configs) m.set(c.isp.toLowerCase(), c);
    return m;
  }, [configs]);

  // Latest actionable engine thought per ISP (decision/warning, or any warn+ severity)
  const engineNoteByISP = useMemo(() => {
    const m = new Map<string, Thought>();
    for (const t of allThoughts) {
      if (!t.isp) continue;
      const key = normalizeISP(t.isp);
      if (m.has(key)) continue; // allThoughts is newest-first
      const actionable = t.type === 'decision' || t.type === 'warning'
        || t.severity === 'warning' || t.severity === 'critical' || t.severity === 'caution';
      if (actionable) m.set(key, t);
    }
    return m;
  }, [allThoughts]);

  const selectedOpsISP = opsISP || (ispRows.length > 0 ? ispRows[0].isp : null);

  if (loading) {
    return (
      <div style={styles.loadingContainer}>
        <FontAwesomeIcon icon={faBrain} spin style={{ fontSize: 48, color: '#00b0ff' }} />
        <p style={{ color: 'rgba(180,210,240,0.65)', marginTop: 16 }}>Initializing consciousness...</p>
      </div>
    );
  }

  const filteredPhilosophies = state?.philosophies?.filter(
    p => selectedISP === 'all' || p.isp === selectedISP
  ) || [];

  return (
    <div className="ig-scan-line" style={styles.container}>
      {/* Header */}
      <div style={styles.header}>
        <div style={styles.headerLeft}>
          <div style={styles.consciousnessIcon}>
            <FontAwesomeIcon icon={faBrain} style={{ fontSize: 28, color: '#00b0ff' }} />
            <div style={styles.iconPulse} />
          </div>
          <div>
            <h1 style={styles.title}>Consciousness</h1>
            <p style={styles.subtitle}>
              {state?.summary || 'All-ISP delivery operations, live'}
            </p>
          </div>
        </div>
        <div style={styles.headerRight}>
          <div style={styles.liveBadge}>
            <span style={styles.liveDot} />
            <span style={{ color: '#10b981', fontSize: 12, fontWeight: 700, letterSpacing: 1 }}>LIVE</span>
            <span style={{ color: 'rgba(180,210,240,0.5)', fontSize: 11 }}>
              {lastUpdated ? `updated ${lastUpdated.toLocaleTimeString()}` : 'loading…'}
            </span>
          </div>
          <div className="ig-pulse-cyan" style={styles.moodBadge}>
            <span style={{ fontSize: 20 }}>{moodEmoji[state?.mood || 'observing']}</span>
            <span style={styles.moodLabel}>{state?.mood || 'observing'}</span>
          </div>
          <div className="ig-pulse-cyan" style={styles.healthScore}>
            <AnimatedCounter value={Math.round(state?.health_score || 0)} style={styles.healthValue} />
            <span style={styles.healthLabel}>Health</span>
          </div>
          <button onClick={() => { fetchState(); fetchOps(); }} style={styles.refreshBtn}>
            <FontAwesomeIcon icon={faSyncAlt} />
          </button>
        </div>
      </div>

      {/* Main tabs: Operations dashboard (primary) vs Engine Internals */}
      <div style={styles.sectionTabs}>
        <button
          onClick={() => setMainTab('operations')}
          style={{ ...styles.sectionTab, ...(mainTab === 'operations' ? styles.sectionTabActive : {}) }}
        >
          <FontAwesomeIcon icon={faTowerBroadcast} style={{ marginRight: 6 }} />
          ISP Operations
          <span style={styles.liveDot} />
        </button>
        <button
          onClick={() => setMainTab('engine')}
          style={{ ...styles.sectionTab, ...(mainTab === 'engine' ? styles.sectionTabActive : {}) }}
        >
          <FontAwesomeIcon icon={faGears} style={{ marginRight: 6 }} />
          Engine Internals
        </button>
      </div>

      {mainTab === 'operations' && (
        <>
          <HourlyAssessmentPanel />
          <OperationsBoard
            rows={ispRows}
            configByISP={configByISP}
            engineNoteByISP={engineNoteByISP}
            series={series}
            selected={selectedOpsISP}
            onSelect={setOpsISP}
            thoughts={allThoughts}
            philosophies={state?.philosophies || []}
          />
        </>
      )}

      {mainTab === 'engine' && (
        <>
          <div style={styles.sectionTabs}>
            {(['overview', 'campaigns', 'philosophies', 'thoughts'] as const).map(section => (
              <button
                key={section}
                onClick={() => setActiveSection(section)}
                style={{
                  ...styles.sectionTab,
                  ...(activeSection === section ? styles.sectionTabActive : {}),
                }}
              >
                <FontAwesomeIcon icon={
                  section === 'campaigns' ? faChartBar :
                  section === 'overview' ? faEye :
                  section === 'philosophies' ? faLightbulb :
                  faCommentDots
                } style={{ marginRight: 6 }} />
                {section === 'campaigns' ? 'Mail Flow' : section.charAt(0).toUpperCase() + section.slice(1)}
                {section === 'thoughts' && liveThoughts.length > 0 && (
                  <span style={styles.liveDot} />
                )}
              </button>
            ))}
          </div>
          <div style={styles.content}>
            {activeSection === 'campaigns' && renderCampaigns(campaigns)}
            {activeSection === 'overview' && renderOverview(state, campaigns, allThoughts)}
            {activeSection === 'philosophies' && renderPhilosophies(filteredPhilosophies, selectedISP, setSelectedISP)}
            {activeSection === 'thoughts' && renderThoughts(allThoughts, thoughtsEndRef, thoughtsLive, setThoughtsLive)}
          </div>
        </>
      )}
    </div>
  );
};

// ----------------------------------------------------------------------------
//  Hourly Assessment — last-60-min per-(ISP, VMTA) pipe health + routing
//  recommendations. Server caches for 10 minutes; we poll on that cadence.
// ----------------------------------------------------------------------------

const HOURLY_ASSESS_POLL_MS = 10 * 60 * 1000;

function deferralColor(pct: number): string {
  if (pct > 50) return '#ef4444';
  if (pct > 10) return '#f59e0b';
  return 'rgba(180,210,240,0.8)';
}

const HourlyAssessmentPanel: React.FC = () => {
  const [assessment, setAssessment] = useState<HourlyAssessmentResp | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    const load = async () => {
      try {
        const res = await apiFetch('/api/mailing/consciousness/hourly-assessment');
        if (cancelled) return;
        if (res.ok) {
          setAssessment(await res.json());
          setError(null);
        } else {
          const body = await res.json().catch(() => null);
          setError(body?.error || `hourly assessment unavailable (${res.status})`);
        }
      } catch {
        if (!cancelled) setError('hourly assessment unreachable');
      }
    };
    load();
    // Cached 10 min server-side — do NOT fast-poll.
    const interval = setInterval(load, HOURLY_ASSESS_POLL_MS);
    return () => { cancelled = true; clearInterval(interval); };
  }, []);

  const recommendations = assessment?.recommendations || [];
  const pipes = assessment?.pipes || [];

  return (
    <div className="ig-card-hover" style={{ ...styles.card, marginBottom: 20 }}>
      <h3 style={styles.cardTitle}>
        <FontAwesomeIcon icon={faBolt} style={{ marginRight: 8, color: '#fdcb6e' }} />
        Hourly Assessment
        <span style={{ color: 'rgba(180,210,240,0.45)', fontSize: 11, fontWeight: 500, marginLeft: 10 }}>
          last 60 min · per-pipe routing read
          {assessment && ` · generated ${new Date(assessment.generated_at).toLocaleTimeString()}`}
        </span>
      </h3>

      {error && (
        <p style={{ color: 'rgba(180,210,240,0.5)', fontSize: 12, margin: 0 }}>{error}</p>
      )}

      {!error && !assessment && (
        <p style={{ color: 'rgba(180,210,240,0.5)', fontSize: 12, margin: 0 }}>Assessing the last hour…</p>
      )}

      {assessment && (
        <>
          {/* Headline routing recommendations */}
          {recommendations.length === 0 ? (
            <p style={{ color: '#10b981', fontSize: 12, margin: '0 0 12px' }}>
              No routing imbalances detected — all pipes within band for the last hour.
            </p>
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 6, marginBottom: 14 }}>
              {recommendations.map((rec, i) => {
                const color = rec.severity === 'critical' ? '#ef4444' : '#f59e0b';
                return (
                  <div key={i} style={{
                    display: 'flex', alignItems: 'flex-start', gap: 8,
                    padding: '8px 12px', borderRadius: 8,
                    background: color + '14', borderLeft: `3px solid ${color}`,
                  }}>
                    <FontAwesomeIcon icon={faExclamationTriangle} style={{ color, fontSize: 12, marginTop: 2, flexShrink: 0 }} />
                    <div style={{ minWidth: 0 }}>
                      <span style={{ color: '#e0e6f0', fontSize: 13, lineHeight: 1.4 }}>{rec.message}</span>
                      {rec.type === 'reroute' && (
                        <span style={{ color: 'rgba(180,210,240,0.5)', fontSize: 11, display: 'block', fontFamily: 'monospace' }}>
                          {rec.bad_vmta}: {rec.bad_deferral_pct?.toFixed(1)}% deferral vs {rec.good_vmta}: {rec.good_deferral_pct?.toFixed(1)}%
                        </span>
                      )}
                    </div>
                  </div>
                );
              })}
            </div>
          )}

          {/* Per-ISP → VMTA pipes table */}
          {pipes.length === 0 ? (
            <p style={{ color: 'rgba(180,210,240,0.5)', fontSize: 12, margin: 0 }}>
              No PMTA traffic in the last 60 minutes.
            </p>
          ) : (
            <div style={{ ...styles.opsTableWrap, maxHeight: 320 }}>
              <table style={styles.opsTable}>
                <thead>
                  <tr>
                    {['ISP', 'VMTA', 'Attempted', 'Delivered %', 'Deferral %', 'Bounce %'].map(h => (
                      <th key={h} style={styles.opsTh}>{h}</th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {pipes.map((p, i) => {
                    const firstOfISP = i === 0 || pipes[i - 1].isp !== p.isp;
                    return (
                      <tr key={`${p.isp}-${p.vmta}`} style={styles.opsTr}>
                        <td style={{ ...styles.opsTd, fontWeight: 700, color: '#dbeafe', whiteSpace: 'nowrap' }}>
                          {firstOfISP ? ispLabel(p.isp) : ''}
                        </td>
                        <td style={{ ...styles.opsTd, fontFamily: 'monospace', color: '#e0e6f0', whiteSpace: 'nowrap' }}>{p.vmta}</td>
                        <td style={{ ...styles.opsTd, fontFamily: 'monospace', color: '#e0e6f0' }}>{p.attempted.toLocaleString()}</td>
                        <td style={{ ...styles.opsTd, fontFamily: 'monospace', color: p.delivered_pct >= 92 ? '#10b981' : p.delivered_pct >= 80 ? '#f59e0b' : '#ef4444' }}>
                          {p.delivered_pct.toFixed(1)}%
                        </td>
                        <td style={{ ...styles.opsTd, fontFamily: 'monospace', color: deferralColor(p.deferral_pct) }}>
                          {p.deferral_pct.toFixed(1)}%
                        </td>
                        <td style={{ ...styles.opsTd, fontFamily: 'monospace', color: p.bounce_pct > 2 ? '#ef4444' : 'rgba(180,210,240,0.8)' }}>
                          {p.bounce_pct.toFixed(2)}%
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </div>
  );
};

// ----------------------------------------------------------------------------
//  ISP Operations board — the running all-ISP dashboard
// ----------------------------------------------------------------------------

const OperationsBoard: React.FC<{
  rows: ISPRow[];
  configByISP: Map<string, ISPConfigEntry>;
  engineNoteByISP: Map<string, Thought>;
  series: Partial<Record<TSMetric, TimeseriesResp>>;
  selected: string | null;
  onSelect: (isp: string) => void;
  thoughts: Thought[];
  philosophies: Philosophy[];
}> = ({ rows, configByISP, engineNoteByISP, series, selected, onSelect, thoughts, philosophies }) => {
  const totals = rows.reduce((acc, r) => ({
    attempted: acc.attempted + r.attempted,
    delivered: acc.delivered + r.delivered,
    deferred: acc.deferred + r.deferred,
    hard: acc.hard + r.hardBounce,
    soft: acc.soft + r.softBounce,
    complaint: acc.complaint + r.complaint,
    repBlocked: acc.repBlocked + r.repBlocked,
  }), { attempted: 0, delivered: 0, deferred: 0, hard: 0, soft: 0, complaint: 0, repBlocked: 0 });

  const sparkDataFor = (isp: string): { v: number }[] => {
    const ts = series.delivered;
    if (!ts) return [];
    const key = normalizeISP(isp);
    return ts.buckets.map(b => ({ v: b.series[isp] ?? b.series[key] ?? 0 }));
  };

  // Merge the 4 metric timeseries into one chart-friendly array for selected ISP
  const detailData = useMemo(() => {
    if (!selected) return [];
    const key = normalizeISP(selected);
    const byBucket = new Map<string, { time: string; ts: number; delivered: number; deferred: number; hard_bounce: number; soft_bounce: number }>();
    for (const metric of TS_METRICS) {
      const ts = series[metric];
      if (!ts) continue;
      for (const b of ts.buckets) {
        let row = byBucket.get(b.bucket);
        if (!row) {
          const d = new Date(b.bucket);
          row = {
            time: d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }),
            ts: d.getTime(),
            delivered: 0, deferred: 0, hard_bounce: 0, soft_bounce: 0,
          };
          byBucket.set(b.bucket, row);
        }
        row[metric] = b.series[selected] ?? b.series[key] ?? 0;
      }
    }
    return Array.from(byBucket.values()).sort((a, b) => a.ts - b.ts);
  }, [series, selected]);

  const selectedThoughts = useMemo(() => {
    if (!selected) return [];
    const key = normalizeISP(selected);
    return thoughts.filter(t => t.isp && normalizeISP(t.isp) === key).slice(0, 5);
  }, [thoughts, selected]);

  const selectedBeliefs = useMemo(() => {
    if (!selected) return [];
    const key = normalizeISP(selected);
    return philosophies.filter(p => normalizeISP(p.isp) === key).slice(0, 3);
  }, [philosophies, selected]);

  const selectedCfg = selected ? configByISP.get(normalizeISP(selected)) : undefined;

  return (
    <div>
      {/* 24h KPI strip */}
      <div className="ig-stagger" style={{ display: 'grid', gridTemplateColumns: 'repeat(7, 1fr)', gap: 12, marginBottom: 20 }}>
        {[
          { label: 'Sent 24h', value: totals.attempted, color: '#818cf8', fmt: (v: number) => v.toLocaleString() },
          { label: 'Delivered %', value: pctOf(totals.delivered, totals.attempted), color: '#10b981', fmt: (v: number) => `${v.toFixed(1)}%` },
          { label: 'Deferred %', value: pctOf(totals.deferred, totals.attempted), color: '#f59e0b', fmt: (v: number) => `${v.toFixed(1)}%` },
          { label: 'Hard %', value: pctOf(totals.hard, totals.attempted), color: '#ef4444', fmt: (v: number) => `${v.toFixed(2)}%` },
          { label: 'Soft %', value: pctOf(totals.soft, totals.attempted), color: '#f59e0b', fmt: (v: number) => `${v.toFixed(2)}%` },
          { label: 'Rep. Blocked', value: totals.repBlocked, color: '#e94560', fmt: (v: number) => v.toLocaleString() },
          { label: 'Complaints', value: totals.complaint, color: '#e94560', fmt: (v: number) => v.toLocaleString() },
        ].map(m => (
          <div key={m.label} className="ig-card-hover" style={styles.kpiCard}>
            <div style={{ color: m.color, fontSize: 24, fontWeight: 700, fontFamily: 'monospace' }}>{m.fmt(m.value)}</div>
            <div style={{ color: 'rgba(180,210,240,0.65)', fontSize: 11, marginTop: 4 }}>{m.label}</div>
          </div>
        ))}
      </div>

      {/* All-ISP table */}
      <div style={styles.opsTableWrap}>
        <table style={styles.opsTable}>
          <thead>
            <tr>
              {['ISP', '24h Trend', 'Sent', 'Delivered %', 'Deferral %', 'Hard %', 'Soft %', 'Complaints', 'Engine Posture', 'Suggestions'].map(h => (
                <th key={h} style={styles.opsTh}>{h}</th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.length === 0 && (
              <tr>
                <td colSpan={10} style={{ ...styles.opsTd, textAlign: 'center', padding: 40, color: 'rgba(180,210,240,0.5)' }}>
                  No delivery events in the last 24h.
                </td>
              </tr>
            )}
            {rows.map(r => {
              const cfg = configByISP.get(normalizeISP(r.isp));
              const note = engineNoteByISP.get(normalizeISP(r.isp));
              const suggestions = buildSuggestions(r, cfg, note);
              const post = posture(cfg);
              const isSelected = selected === r.isp;
              return (
                <tr
                  key={r.isp}
                  onClick={() => onSelect(r.isp)}
                  style={{
                    ...styles.opsTr,
                    background: isSelected ? 'rgba(99,102,241,0.12)' : 'transparent',
                    cursor: 'pointer',
                  }}
                >
                  <td style={{ ...styles.opsTd, fontWeight: 700, color: '#dbeafe', whiteSpace: 'nowrap' }}>
                    {ispLabel(r.isp)}
                  </td>
                  <td style={styles.opsTd}>
                    <AreaChart width={110} height={28} data={sparkDataFor(r.isp)} margin={{ top: 2, right: 0, bottom: 0, left: 0 }}>
                      <Area type="monotone" dataKey="v" stroke="#818cf8" fill="rgba(129,140,248,0.18)" strokeWidth={1.5} isAnimationActive={false} dot={false} />
                    </AreaChart>
                  </td>
                  <td style={{ ...styles.opsTd, fontFamily: 'monospace', color: '#e0e6f0' }}>{r.attempted.toLocaleString()}</td>
                  <td style={{ ...styles.opsTd, fontFamily: 'monospace', color: r.deliveredPct >= 92 ? '#10b981' : r.deliveredPct >= 80 ? '#f59e0b' : '#ef4444' }}>
                    {r.attempted > 0 ? `${r.deliveredPct.toFixed(1)}%` : '—'}
                  </td>
                  <td style={{ ...styles.opsTd, fontFamily: 'monospace', color: r.deferPct > 10 ? '#ef4444' : r.deferPct > 5 ? '#f59e0b' : 'rgba(180,210,240,0.8)' }}>
                    {r.attempted > 0 ? `${r.deferPct.toFixed(1)}%` : '—'}
                  </td>
                  <td style={{ ...styles.opsTd, fontFamily: 'monospace', color: r.hardPct > 2 ? '#ef4444' : r.hardPct > 1 ? '#f87171' : 'rgba(180,210,240,0.8)' }}>
                    {r.attempted > 0 ? `${r.hardPct.toFixed(2)}%` : '—'}
                  </td>
                  <td style={{ ...styles.opsTd, fontFamily: 'monospace', color: r.softPct > 5 ? '#f59e0b' : 'rgba(180,210,240,0.8)' }}>
                    {r.attempted > 0 ? `${r.softPct.toFixed(2)}%` : '—'}
                  </td>
                  <td style={{ ...styles.opsTd, fontFamily: 'monospace', color: r.complaint > 0 ? '#e94560' : 'rgba(180,210,240,0.5)' }}>
                    {r.complaint.toLocaleString()}
                  </td>
                  <td style={styles.opsTd}>
                    <span style={{
                      padding: '2px 8px', borderRadius: 4, fontSize: 11, fontWeight: 700,
                      textTransform: 'uppercase', letterSpacing: 0.5,
                      background: post.color + '22', color: post.color, whiteSpace: 'nowrap',
                    }}>{post.label}</span>
                    {cfg && (
                      <div style={{ color: 'rgba(180,210,240,0.45)', fontSize: 10, marginTop: 4, whiteSpace: 'nowrap' }}>
                        {Math.round(cfg.current_rate)}/{cfg.max_msg_rate} msg/hr · {cfg.ip_count} IPs
                      </div>
                    )}
                  </td>
                  <td style={{ ...styles.opsTd, maxWidth: 320 }}>
                    {suggestions.map((s, i) => (
                      <div key={i} style={{ display: 'flex', alignItems: 'flex-start', gap: 6, marginBottom: i < suggestions.length - 1 ? 4 : 0 }}>
                        <span style={{ width: 6, height: 6, borderRadius: '50%', background: toneColor[s.tone], flexShrink: 0, marginTop: 5 }} />
                        <span style={{ color: toneColor[s.tone], fontSize: 12, lineHeight: 1.35 }}>{s.text}</span>
                      </div>
                    ))}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>

      {/* Selected ISP — 24h trend + engine context */}
      {selected && (
        <div style={{ marginTop: 20 }}>
          <h3 style={{ color: '#dbeafe', fontSize: 15, fontWeight: 700, margin: '0 0 12px' }}>
            {ispLabel(selected)} — last 24h
            {selectedCfg && (
              <span style={{ color: 'rgba(180,210,240,0.5)', fontSize: 12, fontWeight: 500, marginLeft: 10 }}>
                pool {selectedCfg.pool_name} · {selectedCfg.ip_count} IPs · rate {Math.round(selectedCfg.current_rate)}/{selectedCfg.max_msg_rate}
              </span>
            )}
          </h3>
          <div style={styles.detailGrid}>
            <div className="ig-card-hover" style={{ ...styles.card, gridColumn: 'span 2' }}>
              {detailData.length === 0 ? (
                <p style={{ color: 'rgba(180,210,240,0.5)', fontSize: 13 }}>No timeseries data for this ISP in the last 24h.</p>
              ) : (
                <ResponsiveContainer width="100%" height={260}>
                  <LineChart data={detailData} margin={{ top: 8, right: 16, bottom: 0, left: 0 }}>
                    <CartesianGrid stroke="rgba(148,163,184,0.08)" />
                    <XAxis dataKey="time" stroke="#64748b" fontSize={11} tickLine={false} />
                    <YAxis stroke="#64748b" fontSize={11} tickLine={false} width={50} />
                    <Tooltip
                      contentStyle={{ background: '#0b1220', border: '1px solid rgba(99,102,241,0.35)', borderRadius: 8, fontSize: 12 }}
                      labelStyle={{ color: '#dbeafe' }}
                    />
                    <Legend wrapperStyle={{ fontSize: 12 }} />
                    <Line type="monotone" dataKey="delivered" name="Delivered" stroke="#10b981" strokeWidth={2} dot={false} isAnimationActive={false} />
                    <Line type="monotone" dataKey="deferred" name="Deferred" stroke="#f59e0b" strokeWidth={2} dot={false} isAnimationActive={false} />
                    <Line type="monotone" dataKey="hard_bounce" name="Hard Bounce" stroke="#ef4444" strokeWidth={2} dot={false} isAnimationActive={false} />
                    <Line type="monotone" dataKey="soft_bounce" name="Soft Bounce" stroke="#fbbf24" strokeWidth={1.5} dot={false} isAnimationActive={false} strokeDasharray="4 3" />
                  </LineChart>
                </ResponsiveContainer>
              )}
            </div>

            <div className="ig-card-hover" style={styles.card}>
              <h4 style={styles.cardTitle}>
                <FontAwesomeIcon icon={faStream} style={{ marginRight: 8, color: '#00b0ff' }} />
                Engine Signals
              </h4>
              {selectedThoughts.length === 0 ? (
                <p style={{ color: 'rgba(180,210,240,0.5)', fontSize: 12 }}>No recent engine signals for this ISP.</p>
              ) : (
                selectedThoughts.map(t => (
                  <div key={t.id} style={{ display: 'flex', gap: 8, marginBottom: 10 }}>
                    <FontAwesomeIcon
                      icon={thoughtTypeIcon[t.type] || faCommentDots}
                      style={{ color: severityColor[t.severity || 'info'] || '#00b0ff', flexShrink: 0, marginTop: 2, fontSize: 12 }}
                    />
                    <div style={{ minWidth: 0 }}>
                      <p style={{ color: '#e0e6f0', fontSize: 12, margin: 0, lineHeight: 1.4 }}>{t.content}</p>
                      <span style={{ color: 'rgba(180,210,240,0.4)', fontSize: 10 }}>
                        {new Date(t.timestamp).toLocaleTimeString()} · {t.type}
                      </span>
                    </div>
                  </div>
                ))
              )}
              {selectedBeliefs.length > 0 && (
                <>
                  <h4 style={{ ...styles.cardTitle, marginTop: 16 }}>
                    <FontAwesomeIcon icon={faLightbulb} style={{ marginRight: 8, color: '#fdcb6e' }} />
                    Beliefs
                  </h4>
                  {selectedBeliefs.map(p => (
                    <div key={p.id} style={{ display: 'flex', gap: 8, marginBottom: 8 }}>
                      <span style={{ width: 7, height: 7, borderRadius: '50%', background: sentimentColor[p.sentiment] || 'rgba(180,210,240,0.65)', flexShrink: 0, marginTop: 5 }} />
                      <p style={{ color: 'rgba(180,210,240,0.8)', fontSize: 12, margin: 0, lineHeight: 1.4 }}>
                        {p.belief}
                        <span style={{ color: 'rgba(180,210,240,0.4)', fontSize: 10 }}> · {Math.round(p.confidence * 100)}% conf</span>
                      </p>
                    </div>
                  ))}
                </>
              )}
            </div>
          </div>
        </div>
      )}
    </div>
  );
};

// ----------------------------------------------------------------------------
//  Engine Internals — preserved views
// ----------------------------------------------------------------------------

function renderOverview(
  state: ConsciousnessState | null,
  campaigns: CampaignMetrics[],
  thoughts: Thought[],
) {
  const recentThoughts = thoughts.slice(0, 15);
  const positivePhils = state?.philosophies?.filter(p => p.sentiment === 'positive').length || 0;
  const cautiousPhils = state?.philosophies?.filter(p => p.sentiment === 'cautious').length || 0;
  const negativePhils = state?.philosophies?.filter(p => p.sentiment === 'negative').length || 0;

  return (
    <div className="ig-stagger" style={styles.overviewGrid}>
      {/* Belief Distribution */}
      <div className="ig-card-hover" style={styles.card}>
        <h3 style={styles.cardTitle}>
          <FontAwesomeIcon icon={faShieldAlt} style={{ marginRight: 8, color: '#00b0ff' }} />
          Belief Distribution
        </h3>
        <div style={styles.beliefBars}>
          <div style={styles.beliefRow}>
            <span style={{ color: '#00b894' }}>Positive</span>
            <div style={styles.beliefBarTrack}>
              <div style={{ ...styles.beliefBarFill, width: `${positivePhils / Math.max(1, (state?.total_beliefs || 1)) * 100}%`, background: '#00b894' }} />
            </div>
            <span style={styles.beliefCount}>{positivePhils}</span>
          </div>
          <div style={styles.beliefRow}>
            <span style={{ color: '#fdcb6e' }}>Cautious</span>
            <div style={styles.beliefBarTrack}>
              <div style={{ ...styles.beliefBarFill, width: `${cautiousPhils / Math.max(1, (state?.total_beliefs || 1)) * 100}%`, background: '#fdcb6e' }} />
            </div>
            <span style={styles.beliefCount}>{cautiousPhils}</span>
          </div>
          <div style={styles.beliefRow}>
            <span style={{ color: '#e94560' }}>Negative</span>
            <div style={styles.beliefBarTrack}>
              <div style={{ ...styles.beliefBarFill, width: `${negativePhils / Math.max(1, (state?.total_beliefs || 1)) * 100}%`, background: '#e94560' }} />
            </div>
            <span style={styles.beliefCount}>{negativePhils}</span>
          </div>
        </div>
      </div>

      {/* Strongest Beliefs */}
      <div className="ig-card-hover" style={styles.card}>
        <h3 style={styles.cardTitle}>
          <FontAwesomeIcon icon={faLightbulb} style={{ marginRight: 8, color: '#fdcb6e' }} />
          Strongest Beliefs
        </h3>
        <div style={styles.beliefsList}>
          {(state?.philosophies || []).slice(0, 5).map(p => (
            <div key={p.id} style={styles.miniPhilosophy}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 4 }}>
                <span style={{
                  width: 8, height: 8, borderRadius: '50%',
                  background: sentimentColor[p.sentiment] || 'rgba(180,210,240,0.65)',
                }} />
                <span style={{ color: '#e0e6f0', fontWeight: 600, fontSize: 12, textTransform: 'uppercase' }}>
                  {p.isp} / {p.domain}
                </span>
                <span style={{ marginLeft: 'auto', color: 'rgba(180,210,240,0.65)', fontSize: 11 }}>
                  {Math.round(p.strength * 100)}% strength
                </span>
              </div>
              <p style={{ color: 'rgba(180,210,240,0.65)', fontSize: 13, margin: 0, lineHeight: 1.4 }}>{p.belief}</p>
            </div>
          ))}
        </div>
      </div>

      {/* Active Campaigns */}
      <div className="ig-card-hover" style={styles.card}>
        <h3 style={styles.cardTitle}>
          <FontAwesomeIcon icon={faChartBar} style={{ marginRight: 8, color: '#00e5ff' }} />
          Active Campaigns
        </h3>
        {campaigns.length === 0 ? (
          <p style={{ color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>No active campaigns</p>
        ) : (
          <div style={styles.campaignMiniList}>
            {campaigns.slice(0, 4).map(c => (
              <div key={c.campaign_id} style={styles.campaignMiniItem}>
                <span style={{ color: '#e0e6f0', fontWeight: 600, fontSize: 13 }}>{c.campaign_id}</span>
                <div style={styles.miniStats}>
                  <span style={{ color: '#00b894' }}>{c.delivered} dlvd</span>
                  <span style={{ color: '#00e5ff' }}>{c.unique_opens} opens</span>
                  <span style={{ color: '#fdcb6e' }}>{c.unique_clicks} clicks</span>
                  {c.complaints > 0 && <span style={{ color: '#e94560' }}>{c.complaints} complaints</span>}
                </div>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Live Thought Feed */}
      <div className="ig-data-stream ig-card-hover" style={{ ...styles.card, gridColumn: '1 / -1' }}>
        <h3 style={styles.cardTitle}>
          <FontAwesomeIcon icon={faStream} style={{ marginRight: 8, color: '#00b0ff' }} />
          Live Thought Stream
          <span style={styles.liveDot} />
        </h3>
        <div style={styles.thoughtFeed}>
          {recentThoughts.length === 0 ? (
            <p style={{ color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>Awaiting observations...</p>
          ) : (
            recentThoughts.map(t => (
              <div key={t.id} style={styles.thoughtItem}>
                <FontAwesomeIcon
                  icon={thoughtTypeIcon[t.type] || faCommentDots}
                  style={{ color: severityColor[t.severity || 'info'] || '#00b0ff', flexShrink: 0, marginTop: 2 }}
                />
                <div style={{ flex: 1, minWidth: 0 }}>
                  <p style={{ color: '#e0e6f0', fontSize: 13, margin: 0, lineHeight: 1.4 }}>{t.content}</p>
                  <span style={{ color: 'rgba(180,210,240,0.45)', fontSize: 11 }}>
                    {new Date(t.timestamp).toLocaleTimeString()}
                    {t.isp && ` · ${t.isp}`}
                    {t.agent_type && ` · ${t.agent_type}`}
                  </span>
                </div>
              </div>
            ))
          )}
        </div>
      </div>
    </div>
  );
}

function renderPhilosophies(
  philosophies: Philosophy[],
  selectedISP: string,
  setSelectedISP: (isp: string) => void,
) {
  const isps = ['all', 'gmail', 'yahoo', 'microsoft', 'apple', 'comcast', 'att', 'cox', 'charter'];

  return (
    <div>
      {/* ISP Filter */}
      <div style={styles.ispFilter}>
        {isps.map(isp => (
          <button
            key={isp}
            onClick={() => setSelectedISP(isp)}
            style={{
              ...styles.ispFilterBtn,
              ...(selectedISP === isp ? styles.ispFilterBtnActive : {}),
            }}
          >
            {isp === 'all' ? 'All ISPs' : isp.charAt(0).toUpperCase() + isp.slice(1)}
          </button>
        ))}
      </div>

      {/* Philosophy Cards */}
      <div style={styles.philosophyGrid}>
        {philosophies.length === 0 ? (
          <div style={styles.emptyState}>
            <FontAwesomeIcon icon={faLightbulb} style={{ fontSize: 48, color: 'rgba(0,229,255,0.15)', marginBottom: 16 }} />
            <p style={{ color: 'rgba(180,210,240,0.65)' }}>No philosophies formed yet. The system needs more observations.</p>
          </div>
        ) : (
          philosophies.map(p => (
            <div key={p.id} style={styles.philosophyCard}>
              <div style={styles.philosophyHeader}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                  <span style={{
                    padding: '2px 8px', borderRadius: 4, fontSize: 11, fontWeight: 700,
                    textTransform: 'uppercase', letterSpacing: 0.5,
                    background: sentimentColor[p.sentiment] + '22',
                    color: sentimentColor[p.sentiment],
                  }}>{p.sentiment}</span>
                  <span style={{ color: 'rgba(180,210,240,0.65)', fontSize: 12 }}>{p.isp} / {p.domain}</span>
                </div>
                <span style={{ color: 'rgba(180,210,240,0.65)', fontSize: 11 }}>{p.evidence_count} observations</span>
              </div>

              <p style={styles.philosophyBelief}>{p.belief}</p>
              <p style={styles.philosophyExplanation}>{p.explanation}</p>

              <div style={styles.philosophyFooter}>
                <div style={styles.strengthBar}>
                  <div style={{ ...styles.strengthFill, width: `${p.strength * 100}%` }} />
                </div>
                <span style={{ color: 'rgba(180,210,240,0.65)', fontSize: 11 }}>
                  {Math.round(p.confidence * 100)}% confidence · {Math.round(p.strength * 100)}% strength
                  {p.challenges > 0 && ` · ${p.challenges} challenges`}
                </span>
              </div>

              {p.tags && p.tags.length > 0 && (
                <div style={styles.tagRow}>
                  {p.tags.map(tag => (
                    <span key={tag} style={styles.tag}>{tag}</span>
                  ))}
                </div>
              )}
            </div>
          ))
        )}
      </div>
    </div>
  );
}

function renderThoughts(
  thoughts: Thought[],
  endRef: React.RefObject<HTMLDivElement>,
  live: boolean,
  setLive: (v: boolean) => void,
) {
  return (
    <div>
      {/* Default is paused/snapshot — the firehose only runs when toggled on */}
      <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 12 }}>
        <span style={{ color: 'rgba(180,210,240,0.65)', fontSize: 12 }}>
          {live ? 'Streaming thoughts live' : 'Snapshot view (stream paused)'}
        </span>
        <button
          onClick={() => setLive(!live)}
          style={{
            padding: '5px 12px', borderRadius: 8, fontSize: 12, fontWeight: 600, cursor: 'pointer',
            background: live ? 'rgba(16,185,129,0.12)' : 'rgba(0,200,255,0.07)',
            border: live ? '1px solid rgba(16,185,129,0.4)' : '1px solid rgba(0,200,255,0.15)',
            color: live ? '#10b981' : 'rgba(180,210,240,0.8)',
            display: 'flex', alignItems: 'center',
          }}
        >
          <FontAwesomeIcon icon={faStream} style={{ marginRight: 6, fontSize: 11 }} />
          Live stream {live ? 'ON' : 'OFF'}
          {live && <span style={styles.liveDot} />}
        </button>
      </div>
      <div className="ig-data-stream" style={styles.thoughtsContainer}>
      {thoughts.length === 0 ? (
        <div style={styles.emptyState}>
          <FontAwesomeIcon icon={faCommentDots} style={{ fontSize: 48, color: 'rgba(0,229,255,0.15)', marginBottom: 16 }} />
          <p style={{ color: 'rgba(180,210,240,0.65)' }}>No thoughts yet. The system is still forming its initial observations.</p>
        </div>
      ) : (
        <>
          {thoughts.map(t => (
            <div key={t.id} style={{
              ...styles.thoughtCard,
              borderLeft: `3px solid ${severityColor[t.severity || 'info'] || '#00b0ff'}`,
            }}>
              <div style={styles.thoughtCardHeader}>
                <FontAwesomeIcon
                  icon={thoughtTypeIcon[t.type] || faCommentDots}
                  style={{ color: severityColor[t.severity || 'info'], marginRight: 8 }}
                />
                <span style={{ color: 'rgba(180,210,240,0.65)', fontSize: 12, textTransform: 'uppercase', fontWeight: 600 }}>
                  {t.type}
                </span>
                {t.isp && <span style={styles.thoughtTag}>{t.isp}</span>}
                {t.agent_type && <span style={styles.thoughtTag}>{t.agent_type}</span>}
                <span style={{ marginLeft: 'auto', color: 'rgba(180,210,240,0.45)', fontSize: 11 }}>
                  {new Date(t.timestamp).toLocaleString()}
                </span>
              </div>
              <p style={{ color: '#e0e6f0', fontSize: 14, margin: '8px 0', lineHeight: 1.5 }}>{t.content}</p>
              {t.reasoning && (
                <p style={{ color: 'rgba(180,210,240,0.65)', fontSize: 12, margin: '4px 0 0', fontStyle: 'italic' }}>
                  Reasoning: {t.reasoning}
                </p>
              )}
              {t.confidence != null && t.confidence > 0 && (
                <span style={{ color: 'rgba(180,210,240,0.45)', fontSize: 11 }}>Confidence: {Math.round(t.confidence * 100)}%</span>
              )}
            </div>
          ))}
          <div ref={endRef as React.RefObject<HTMLDivElement>} />
        </>
      )}
      </div>
    </div>
  );
}

function renderCampaigns(campaigns: CampaignMetrics[]) {
  const totals = campaigns.reduce((acc, c) => ({
    sent: acc.sent + c.sent,
    delivered: acc.delivered + c.delivered,
    opens: acc.opens + c.opens,
    clicks: acc.clicks + c.clicks,
    hard_bounces: acc.hard_bounces + (c.hard_bounce || 0),
    soft_bounces: acc.soft_bounces + (c.soft_bounce || 0),
    complaints: acc.complaints + c.complaints,
  }), { sent: 0, delivered: 0, opens: 0, clicks: 0, hard_bounces: 0, soft_bounces: 0, complaints: 0 });

  return (
    <div>
      {/* PMTA Throughput Summary */}
      <div className="ig-stagger" style={{ display: 'grid', gridTemplateColumns: 'repeat(7, 1fr)', gap: 12, marginBottom: 20 }}>
        {[
          { label: 'Total Sent', value: totals.sent, color: '#00b0ff' },
          { label: 'Delivered', value: totals.delivered, color: '#00b894' },
          { label: 'Opens', value: totals.opens, color: '#00e5ff' },
          { label: 'Clicks', value: totals.clicks, color: '#00e5ff' },
          { label: 'Hard Bounces', value: totals.hard_bounces, color: '#ef4444' },
          { label: 'Soft Bounces', value: totals.soft_bounces, color: '#f59e0b' },
          { label: 'Complaints', value: totals.complaints, color: '#e94560' },
        ].map(m => (
          <div key={m.label} className="ig-card-hover" style={{ background: '#0d1526', borderRadius: 10, padding: '16px 12px', textAlign: 'center', border: '1px solid rgba(0,200,255,0.08)' }}>
            <AnimatedCounter value={m.value} style={{ color: m.color, fontSize: 28, fontWeight: 700, fontFamily: 'monospace' }} />
            <div style={{ color: 'rgba(180,210,240,0.65)', fontSize: 11, marginTop: 4 }}>{m.label}</div>
          </div>
        ))}
      </div>

      {campaigns.length === 0 ? (
        <div style={styles.emptyState}>
          <FontAwesomeIcon icon={faChartBar} style={{ fontSize: 48, color: 'rgba(0,229,255,0.15)', marginBottom: 16 }} />
          <p style={{ color: 'rgba(180,210,240,0.65)' }}>No campaigns tracked yet. Send a campaign via PMTA to see live metrics here.</p>
        </div>
      ) : (
        <div className="ig-stagger" style={styles.campaignGrid}>
          {campaigns.map(c => {
            const deliveryRate = c.sent > 0 ? (c.delivered / c.sent * 100) : 0;
            const openRate = c.delivered > 0 ? (c.unique_opens / c.delivered * 100) : 0;
            const clickRate = c.delivered > 0 ? (c.unique_clicks / c.delivered * 100) : 0;

            return (
              <div key={c.campaign_id} className="ig-card-hover" style={styles.campaignCard}>
                <div style={styles.campaignCardHeader}>
                  <h3 style={{ color: '#e0e6f0', fontSize: 16, margin: 0, fontWeight: 700 }}>
                    {c.campaign_id}
                  </h3>
                  <span style={{ color: 'rgba(180,210,240,0.45)', fontSize: 11 }}>
                    Started {new Date(c.started_at).toLocaleDateString()}
                  </span>
                </div>

                <div style={styles.metricsGrid}>
                  <MetricBox label="Sent" value={c.sent} color="#00b0ff" />
                  <MetricBox label="Delivered" value={c.delivered} color="#00b894" sub={`${deliveryRate.toFixed(1)}%`} />
                  <MetricBox label="Opens" value={c.opens} color="#00e5ff" sub={`${c.unique_opens} unique`} />
                  <MetricBox label="Clicks" value={c.clicks} color="#00e5ff" sub={`${c.unique_clicks} unique`} />
                  <MetricBox label="Soft Bounce" value={c.soft_bounce} color="#fdcb6e" />
                  <MetricBox label="Hard Bounce" value={c.hard_bounce} color="#e94560" />
                  <MetricBox label="Complaints" value={c.complaints} color="#e94560" />
                  <MetricBox label="Unsubscribes" value={c.unsubscribes} color="#f97316" />
                  <MetricBox label="Inactive" value={c.inactive} color="rgba(180,210,240,0.65)" sub="4+ sends, 0 engagement" />
                </div>

                <div style={styles.rateRow}>
                  <RateBar label="Delivery" value={deliveryRate} color="#00b894" />
                  <RateBar label="Open Rate" value={openRate} color="#00e5ff" />
                  <RateBar label="Click Rate" value={clickRate} color="#00e5ff" />
                </div>

                {c.isp_metrics && Object.keys(c.isp_metrics).length > 0 && (
                  <div style={styles.ispBreakdown}>
                    <h4 style={{ color: 'rgba(180,210,240,0.65)', fontSize: 12, margin: '12px 0 8px', textTransform: 'uppercase' }}>ISP Breakdown</h4>
                    {Object.entries(c.isp_metrics).map(([isp, m]: [string, any]) => (
                      <div key={isp} style={styles.ispRow}>
                        <span style={{ color: '#e0e6f0', fontWeight: 600, fontSize: 12, width: 80 }}>{isp}</span>
                        <span style={{ color: '#00b894', fontSize: 11 }}>{m.delivered} dlvd</span>
                        <span style={{ color: '#00e5ff', fontSize: 11 }}>{m.unique_opens} opens</span>
                        <span style={{ color: '#00e5ff', fontSize: 11 }}>{m.unique_clicks} clicks</span>
                        {(m.hard_bounce > 0 || m.soft_bounce > 0) && (
                          <span style={{ fontSize: 11 }}>
                            <span style={{ color: '#ef4444' }}>{m.hard_bounce} hard</span>
                            <span style={{ color: 'rgba(180,210,240,0.5)' }}> / </span>
                            <span style={{ color: '#f59e0b' }}>{m.soft_bounce} soft</span>
                          </span>
                        )}
                      </div>
                    ))}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

const MetricBox: React.FC<{ label: string; value: number; color: string; sub?: string }> = ({ label, value, color, sub }) => (
  <div style={styles.metricBox}>
    <span style={{ color, fontSize: 24, fontWeight: 700, fontFamily: 'monospace' }}>
      {value.toLocaleString()}
    </span>
    <span style={{ color: 'rgba(180,210,240,0.65)', fontSize: 11 }}>{label}</span>
    {sub && <span style={{ color: 'rgba(180,210,240,0.45)', fontSize: 10 }}>{sub}</span>}
  </div>
);

const RateBar: React.FC<{ label: string; value: number; color: string }> = ({ label, value, color }) => (
  <div style={{ flex: 1 }}>
    <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 4 }}>
      <span style={{ color: 'rgba(180,210,240,0.65)', fontSize: 11 }}>{label}</span>
      <span style={{ color, fontSize: 11, fontWeight: 600 }}>{value.toFixed(1)}%</span>
    </div>
    <div style={{ height: 4, background: '#0d1526', borderRadius: 2 }}>
      <div style={{ height: 4, borderRadius: 2, background: color, width: `${Math.min(value, 100)}%`, transition: 'width 0.5s ease' }} />
    </div>
  </div>
);

const styles: Record<string, React.CSSProperties> = {
  container: {
    padding: 24,
    maxWidth: 1400,
    margin: '0 auto',
  },
  loadingContainer: {
    display: 'flex',
    flexDirection: 'column',
    alignItems: 'center',
    justifyContent: 'center',
    height: '60vh',
  },
  header: {
    display: 'flex',
    justifyContent: 'space-between',
    alignItems: 'center',
    marginBottom: 24,
    flexWrap: 'wrap',
    gap: 16,
  },
  headerLeft: {
    display: 'flex',
    alignItems: 'center',
    gap: 16,
  },
  consciousnessIcon: {
    position: 'relative',
    width: 56,
    height: 56,
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    background: 'linear-gradient(135deg, rgba(0,229,255,0.15), rgba(0,229,255,0.15))',
    borderRadius: 16,
    border: '1px solid rgba(0,229,255,0.3)',
  },
  iconPulse: {
    position: 'absolute',
    inset: -4,
    borderRadius: 20,
    border: '1px solid rgba(0,229,255,0.2)',
    animation: 'pulse 2s ease-in-out infinite',
  },
  title: {
    color: '#e0e6f0',
    fontSize: 28,
    fontWeight: 800,
    margin: 0,
    letterSpacing: -0.5,
  },
  subtitle: {
    color: 'rgba(180,210,240,0.65)',
    fontSize: 14,
    margin: '4px 0 0',
  },
  headerRight: {
    display: 'flex',
    alignItems: 'center',
    gap: 12,
  },
  liveBadge: {
    display: 'flex',
    alignItems: 'center',
    gap: 8,
    padding: '8px 14px',
    background: 'rgba(16,185,129,0.08)',
    borderRadius: 12,
    border: '1px solid rgba(16,185,129,0.25)',
  },
  moodBadge: {
    display: 'flex',
    alignItems: 'center',
    gap: 8,
    padding: '8px 16px',
    background: 'rgba(0,200,255,0.08)',
    borderRadius: 12,
    border: '1px solid rgba(0,200,255,0.08)',
  },
  moodLabel: {
    color: '#e0e6f0',
    fontSize: 13,
    fontWeight: 600,
    textTransform: 'capitalize' as const,
  },
  healthScore: {
    display: 'flex',
    flexDirection: 'column' as const,
    alignItems: 'center',
    padding: '6px 14px',
    background: 'rgba(0,200,255,0.08)',
    borderRadius: 12,
    border: '1px solid rgba(0,200,255,0.08)',
  },
  healthValue: {
    color: '#00b894',
    fontSize: 20,
    fontWeight: 800,
    fontFamily: 'monospace',
  },
  healthLabel: {
    color: 'rgba(180,210,240,0.45)',
    fontSize: 10,
    textTransform: 'uppercase' as const,
  },
  refreshBtn: {
    background: 'none',
    border: '1px solid rgba(0,200,255,0.08)',
    color: 'rgba(180,210,240,0.65)',
    padding: '8px 12px',
    borderRadius: 8,
    cursor: 'pointer',
    fontSize: 14,
  },
  sectionTabs: {
    display: 'flex',
    gap: 4,
    marginBottom: 24,
    background: 'rgba(13,21,38,0.5)',
    padding: 4,
    borderRadius: 12,
    border: '1px solid rgba(0,200,255,0.06)',
  },
  sectionTab: {
    flex: 1,
    padding: '10px 16px',
    background: 'transparent',
    border: 'none',
    color: 'rgba(180,210,240,0.65)',
    fontSize: 13,
    fontWeight: 600,
    cursor: 'pointer',
    borderRadius: 8,
    transition: 'all 0.2s',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    position: 'relative' as const,
  },
  sectionTabActive: {
    background: 'rgba(0,229,255,0.15)',
    color: '#00b0ff',
    border: '1px solid rgba(0,229,255,0.3)',
  },
  liveDot: {
    width: 6,
    height: 6,
    borderRadius: '50%',
    background: '#00b894',
    marginLeft: 6,
    animation: 'pulse 1.5s ease-in-out infinite',
  },
  content: {},
  // ---- ISP operations styles ----
  kpiCard: {
    background: 'rgba(15,30,60,0.35)',
    borderRadius: 10,
    padding: '14px 12px',
    textAlign: 'center' as const,
    border: '1px solid rgba(99,102,241,0.12)',
  },
  opsTableWrap: {
    background: 'rgba(15,30,60,0.35)',
    border: '1px solid rgba(99,102,241,0.15)',
    borderRadius: 12,
    overflow: 'auto' as const,
  },
  opsTable: {
    width: '100%',
    borderCollapse: 'collapse' as const,
    fontSize: 13,
  },
  opsTh: {
    textAlign: 'left' as const,
    padding: '10px 12px',
    color: '#dbeafe',
    fontSize: 11,
    fontWeight: 700,
    textTransform: 'uppercase' as const,
    letterSpacing: 0.5,
    borderBottom: '1px solid rgba(99,102,241,0.2)',
    whiteSpace: 'nowrap' as const,
    background: 'rgba(15,30,60,0.5)',
  },
  opsTr: {
    borderBottom: '1px solid rgba(99,102,241,0.08)',
    transition: 'background 0.15s',
  },
  opsTd: {
    padding: '10px 12px',
    verticalAlign: 'top' as const,
  },
  detailGrid: {
    display: 'grid',
    gridTemplateColumns: '2fr 1fr',
    gap: 16,
  },
  // ---- shared cards ----
  overviewGrid: {
    display: 'grid',
    gridTemplateColumns: 'repeat(3, 1fr)',
    gap: 16,
  },
  card: {
    background: 'rgba(13,21,38,0.6)',
    border: '1px solid rgba(0,200,255,0.08)',
    borderRadius: 12,
    padding: 20,
  },
  cardTitle: {
    color: '#dbeafe',
    fontSize: 14,
    fontWeight: 700,
    margin: '0 0 16px',
    display: 'flex',
    alignItems: 'center',
  },
  beliefBars: {
    display: 'flex',
    flexDirection: 'column' as const,
    gap: 12,
  },
  beliefRow: {
    display: 'flex',
    alignItems: 'center',
    gap: 10,
    fontSize: 13,
  },
  beliefBarTrack: {
    flex: 1,
    height: 6,
    background: '#0d1526',
    borderRadius: 3,
    overflow: 'hidden' as const,
  },
  beliefBarFill: {
    height: '100%',
    borderRadius: 3,
    transition: 'width 0.5s ease',
  },
  beliefCount: {
    color: 'rgba(180,210,240,0.65)',
    fontSize: 13,
    fontWeight: 600,
    width: 20,
    textAlign: 'right' as const,
  },
  beliefsList: {
    display: 'flex',
    flexDirection: 'column' as const,
    gap: 12,
  },
  miniPhilosophy: {
    padding: '10px 12px',
    background: 'rgba(0,200,255,0.05)',
    borderRadius: 8,
  },
  campaignMiniList: {
    display: 'flex',
    flexDirection: 'column' as const,
    gap: 10,
  },
  campaignMiniItem: {
    padding: '8px 12px',
    background: 'rgba(0,200,255,0.05)',
    borderRadius: 8,
  },
  miniStats: {
    display: 'flex',
    gap: 12,
    marginTop: 4,
    fontSize: 11,
  },
  thoughtFeed: {
    maxHeight: 400,
    overflow: 'auto',
    display: 'flex',
    flexDirection: 'column' as const,
    gap: 8,
  },
  thoughtItem: {
    display: 'flex',
    gap: 10,
    padding: '8px 12px',
    background: 'rgba(0,200,255,0.04)',
    borderRadius: 8,
  },
  ispFilter: {
    display: 'flex',
    gap: 6,
    marginBottom: 20,
    flexWrap: 'wrap' as const,
  },
  ispFilterBtn: {
    padding: '6px 14px',
    background: 'rgba(0,200,255,0.07)',
    border: '1px solid rgba(0,200,255,0.08)',
    color: 'rgba(180,210,240,0.65)',
    borderRadius: 8,
    fontSize: 12,
    fontWeight: 600,
    cursor: 'pointer',
    transition: 'all 0.2s',
  },
  ispFilterBtnActive: {
    background: 'rgba(0,229,255,0.2)',
    borderColor: 'rgba(0,229,255,0.5)',
    color: '#00b0ff',
  },
  philosophyGrid: {
    display: 'grid',
    gridTemplateColumns: 'repeat(2, 1fr)',
    gap: 16,
  },
  emptyState: {
    gridColumn: '1 / -1',
    textAlign: 'center' as const,
    padding: 60,
  },
  philosophyCard: {
    background: 'rgba(13,21,38,0.6)',
    border: '1px solid rgba(0,200,255,0.08)',
    borderRadius: 12,
    padding: 20,
  },
  philosophyHeader: {
    display: 'flex',
    justifyContent: 'space-between',
    alignItems: 'center',
    marginBottom: 12,
  },
  philosophyBelief: {
    color: '#e0e6f0',
    fontSize: 15,
    lineHeight: 1.5,
    margin: '0 0 8px',
    fontWeight: 500,
  },
  philosophyExplanation: {
    color: 'rgba(180,210,240,0.65)',
    fontSize: 13,
    lineHeight: 1.5,
    margin: '0 0 12px',
  },
  philosophyFooter: {
    display: 'flex',
    flexDirection: 'column' as const,
    gap: 6,
  },
  strengthBar: {
    height: 4,
    background: '#0d1526',
    borderRadius: 2,
    overflow: 'hidden' as const,
  },
  strengthFill: {
    height: '100%',
    background: 'linear-gradient(90deg, #00b0ff, #00e5ff)',
    borderRadius: 2,
    transition: 'width 0.5s ease',
  },
  tagRow: {
    display: 'flex',
    gap: 6,
    marginTop: 10,
  },
  tag: {
    padding: '2px 8px',
    background: 'rgba(0,200,255,0.07)',
    border: '1px solid rgba(0,200,255,0.08)',
    borderRadius: 4,
    color: 'rgba(180,210,240,0.65)',
    fontSize: 11,
  },
  thoughtsContainer: {
    maxHeight: 'calc(100vh - 250px)',
    overflow: 'auto',
    display: 'flex',
    flexDirection: 'column' as const,
    gap: 8,
  },
  thoughtCard: {
    background: 'rgba(13,21,38,0.6)',
    border: '1px solid rgba(0,200,255,0.08)',
    borderRadius: 8,
    padding: 16,
  },
  thoughtCardHeader: {
    display: 'flex',
    alignItems: 'center',
    gap: 8,
    marginBottom: 4,
    flexWrap: 'wrap' as const,
  },
  thoughtTag: {
    padding: '1px 6px',
    background: 'rgba(0,229,255,0.15)',
    borderRadius: 4,
    color: '#00b0ff',
    fontSize: 10,
    fontWeight: 600,
  },
  campaignGrid: {
    display: 'grid',
    gridTemplateColumns: '1fr',
    gap: 16,
  },
  campaignCard: {
    background: 'rgba(13,21,38,0.6)',
    border: '1px solid rgba(0,200,255,0.08)',
    borderRadius: 12,
    padding: 20,
  },
  campaignCardHeader: {
    display: 'flex',
    justifyContent: 'space-between',
    alignItems: 'center',
    marginBottom: 16,
  },
  metricsGrid: {
    display: 'grid',
    gridTemplateColumns: 'repeat(auto-fill, minmax(120px, 1fr))',
    gap: 12,
    marginBottom: 16,
  },
  metricBox: {
    display: 'flex',
    flexDirection: 'column' as const,
    alignItems: 'center',
    padding: '12px 8px',
    background: 'rgba(0,200,255,0.05)',
    borderRadius: 8,
    gap: 2,
  },
  rateRow: {
    display: 'flex',
    gap: 20,
    padding: '12px 0',
    borderTop: '1px solid rgba(0,200,255,0.08)',
  },
  ispBreakdown: {},
  ispRow: {
    display: 'flex',
    gap: 16,
    padding: '6px 0',
    borderBottom: '1px solid rgba(0,200,255,0.05)',
    alignItems: 'center',
  },
};
