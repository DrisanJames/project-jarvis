import React, { useState, useRef, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faBolt, faEnvelope, faClock, faCodeBranch, faRandom, faBullseye,
  faSave, faRocket, faCog, faEye, faBookOpen, faFilter,
} from '@fortawesome/free-solid-svg-icons';
import './JourneyBuilder.css';
import { apiFetch } from '../shared/apiFetch';

// JOURNEY_BUILDER_VERSION is surfaced in the canvas header so we can verify
// the deployed UI build at a glance after each release. Bumped whenever the
// shape of NodeConfig, Journey, or any persisted journey field changes.
//
// History:
//   0.1 - original implementation (list trigger only, basic email node)
//   0.2 - Phase 1 of Welcome Series plan: segment trigger, template picker,
//         email preview, sending profile prepop, until-time + timezone,
//         journey settings drawer (exit_on_open / exit_on_click / isp quotas)
export const JOURNEY_BUILDER_VERSION = '0.3';

// Hourly ISP quota buckets. Order matches the ISP grouping used by the PMTA
// planner (internal/isp/group.go) so a quota set defined here maps directly
// onto the buckets the wave scheduler will throttle against.
export const ISP_QUOTA_BUCKETS = [
  { key: 'gmail', label: 'Gmail' },
  { key: 'yahoo', label: 'Yahoo / AOL' },
  { key: 'microsoft', label: 'Microsoft (Outlook / Hotmail)' },
  { key: 'apple', label: 'Apple (icloud / me / mac)' },
  { key: 'comcast', label: 'Comcast' },
  { key: 'cox', label: 'Cox' },
  { key: 'charter', label: 'Charter' },
  { key: 'att', label: 'AT&T / SBCGlobal' },
  { key: 'other', label: 'Other / Long-tail' },
] as const;

// Time zones we offer for the "delay until time" node. Must match Go time
// zone names that Phase 2's executeDelayNode will pass to time.LoadLocation.
export const DELAY_TIMEZONES = [
  { value: 'America/Denver', label: 'Mountain (MST/MDT)' },
  { value: 'America/Los_Angeles', label: 'Pacific (PST/PDT)' },
  { value: 'America/Chicago', label: 'Central (CST/CDT)' },
  { value: 'America/New_York', label: 'Eastern (EST/EDT)' },
  { value: 'UTC', label: 'UTC' },
] as const;

// Magic segment id used by the trigger node to mean "everyone who has been
// cleaned (verified_at IS NOT NULL) but never received an email
// (computed_fields.first_email_at IS NULL)". Phase 2's segment_enroller
// worker will materialize this preset against the segmentation engine.
export const CLEANED_NEVER_MAILED_PRESET_ID = '__preset_cleaned_never_mailed__';

// Types
interface Position {
  x: number;
  y: number;
}

interface JourneyNode {
  id: string;
  type: 'trigger' | 'email' | 'delay' | 'condition' | 'split' | 'goal';
  position: Position;
  config: NodeConfig;
  connections: string[]; // IDs of connected nodes
}

interface NodeConfig {
  name?: string;
  // Trigger config
  triggerType?: 'schedule' | 'performance' | 'event' | 'segment';
  listId?: string;
  segmentId?: string;
  segmentName?: string;
  scheduleType?: 'once' | 'recurring';
  scheduleDate?: string;
  recurringPattern?: string;
  performanceEvent?: 'open' | 'click' | 'no_open' | 'no_click' | 'bounce' | 'complaint';
  performanceWindow?: number; // hours
  // Email config
  subject?: string;
  fromName?: string;
  fromEmail?: string;
  sendingProfileId?: string;
  sendingDomain?: string;
  templateId?: string;
  templateName?: string;
  htmlContent?: string;
  // Delay config
  delayType?: 'fixed' | 'until_time' | 'until_day';
  delayValue?: number;
  delayUnit?: 'minutes' | 'hours' | 'days' | 'weeks';
  untilTime?: string;
  untilDay?: string;
  untilTimezone?: string;
  // Condition config
  conditionType?: 'opened' | 'clicked' | 'not_opened' | 'not_clicked' | 'custom';
  conditionField?: string;
  conditionOperator?: string;
  conditionValue?: string;
  // Split config
  splitType?: 'random' | 'percentage';
  splitPercentage?: number;
  // Goal config
  goalType?: 'conversion' | 'engagement' | 'custom';
  goalValue?: string;
}

interface Connection {
  from: string;
  to: string;
  label?: string;
}

interface Journey {
  id: string;
  name: string;
  status: 'draft' | 'active' | 'paused' | 'completed';
  nodes: JourneyNode[];
  connections: Connection[];
  createdAt: string;
  // Phase 1 (UI) / Phase 2 (backend persistence): journey-level engagement
  // exit policy. When either flag is true and a subscriber opens or clicks
  // ANY email in the system (Phase 4 watcher), every active enrollment in
  // this journey for that subscriber is exited.
  exitOnOpen?: boolean;
  exitOnClick?: boolean;
  // Phase 1 (UI) / Phase 2 (backend persistence): journey-level hourly
  // ISP quotas. Keys match ISP_QUOTA_BUCKETS; values are integers (hourly
  // cap per ISP across all email nodes in the journey).
  ispQuotas?: Record<string, number>;
  // Phase 1 (UI) / Phase 2 (backend persistence): journey-level default
  // sending profile. Email nodes inherit this when their own
  // sendingProfileId is empty.
  sendingProfileId?: string;
  stats?: {
    entered: number;
    completed: number;
    active: number;
  };
}

interface List {
  id: string;
  name: string;
  subscriber_count: number;
}

interface SendingProfile {
  id: string;
  name: string;
  vendor_type: string;
  sending_domain?: string;
  from_name?: string;
  from_email?: string;
  status?: string;
}

interface SegmentOption {
  id: string;
  name: string;
  description?: string;
  subscriber_count?: number;
}

interface TemplateOption {
  id: string;
  name: string;
  description?: string;
  subject?: string;
  thumbnail_url?: string;
}

interface TemplateDetail extends TemplateOption {
  from_name?: string;
  from_email?: string;
  html_content?: string;
  plain_content?: string;
}

// Phase 3 (Welcome Series wave-native): canvas tile lifetime stats.
// Server contract: GET /api/mailing/journeys/:id/node-stats returns
// { api_version, journey_id, nodes: { [nodeId]: JourneyNodeStat } }.
// Hard / soft bounce are split per .cursor/rules/bounce-metrics.mdc;
// never display a combined "bounces" total.
export interface JourneyNodeStat {
  audience_awaiting: number;
  sent: number;
  delivered: number;
  opens: number;
  clicks: number;
  hard_bounce: number;
  soft_bounce: number;
  shadow_campaigns: number;
}

interface JourneyNodeStatsResponse {
  api_version?: string;
  journey_id?: string;
  nodes?: Record<string, JourneyNodeStat>;
  error?: string;
}

// Node type definitions
const NODE_TYPES = {
  trigger: { label: 'Trigger', icon: faBolt, color: '#00b894', description: 'Start the journey' },
  email: { label: 'Send Email', icon: faEnvelope, color: '#00e5ff', description: 'Send an email' },
  delay: { label: 'Wait', icon: faClock, color: '#fdcb6e', description: 'Wait before next step' },
  condition: { label: 'Condition', icon: faCodeBranch, color: '#00b0ff', description: 'Branch based on behavior' },
  split: { label: 'A/B Split', icon: faRandom, color: '#ec4899', description: 'Split traffic for testing' },
  goal: { label: 'Goal', icon: faBullseye, color: '#14b8a6', description: 'Track conversions' },
};

// Main Component
export const JourneyBuilder: React.FC = () => {
  const [journeys, setJourneys] = useState<Journey[]>([]);
  const [activeJourney, setActiveJourney] = useState<Journey | null>(null);
  const [selectedNode, setSelectedNode] = useState<JourneyNode | null>(null);
  const [showNodeConfig, setShowNodeConfig] = useState(false);
  const [showJourneySettings, setShowJourneySettings] = useState(false);
  const [showTemplatePicker, setShowTemplatePicker] = useState(false);
  const [showEmailPreview, setShowEmailPreview] = useState(false);
  const [previewHtml, setPreviewHtml] = useState<string>('');
  const [previewSubject, setPreviewSubject] = useState<string>('');
  const [, setDraggingNode] = useState<string | null>(null);
  const [connecting, setConnecting] = useState<{ from: string; fromPos: Position } | null>(null);
  const [lists, setLists] = useState<List[]>([]);
  const [profiles, setProfiles] = useState<SendingProfile[]>([]);
  const [segments, setSegments] = useState<SegmentOption[]>([]);
  const [templates, setTemplates] = useState<TemplateOption[]>([]);
  const [loading, setLoading] = useState(true);
  const canvasRef = useRef<HTMLDivElement>(null);
  const [canvasOffset] = useState({ x: 0, y: 0 });
  const [zoom, setZoom] = useState(1);

  // Phase 3 (Welcome Series wave-native): per-node lifetime stats from
  // /api/mailing/journeys/:id/node-stats. Keyed by node id; missing keys
  // render as zeros via the `??` fallback in the tile JSX. Polling lives
  // in a dedicated effect below so it tears down cleanly when the active
  // journey changes.
  const [nodeStats, setNodeStats] = useState<Record<string, JourneyNodeStat>>({});

  // Load data
  useEffect(() => {
    Promise.all([
      apiFetch('/api/mailing/journeys').then(r => r.json()).catch(() => ({ journeys: [] })),
      apiFetch('/api/mailing/lists').then(r => r.json()).catch(() => ({ lists: [] })),
      apiFetch('/api/mailing/sending-profiles').then(r => r.json()).catch(() => ({ profiles: [] })),
      apiFetch('/api/mailing/segments').then(r => r.json()).catch(() => ({ segments: [] })),
      apiFetch('/api/mailing/templates').then(r => r.json()).catch(() => ({ templates: [] })),
    ]).then(([journeyData, listData, profileData, segmentData, templateData]) => {
      setJourneys(journeyData.journeys || []);
      setLists(listData.lists || []);
      setProfiles(profileData.profiles || []);
      setSegments(segmentData.segments || []);
      setTemplates(templateData.templates || []);
      setLoading(false);
    });
  }, []);

  // Phase 3 (Welcome Series wave-native): poll per-node lifetime
  // stats every 10s while a journey is open in the builder. Stops
  // automatically when the journey changes via the deps array.
  useEffect(() => {
    if (!activeJourney?.id) {
      setNodeStats({});
      return;
    }
    let cancelled = false;
    const fetchStats = () => {
      apiFetch(`/api/mailing/journeys/${encodeURIComponent(activeJourney.id)}/node-stats`)
        .then(r => r.json())
        .then((data: JourneyNodeStatsResponse) => {
          if (cancelled) return;
          if (data && data.nodes) {
            setNodeStats(data.nodes);
          }
        })
        .catch(() => {
          // Polling failures are non-fatal; tiles fall back to the
          // last known good values, or zeros on first render. This
          // keeps the canvas usable when the backend is briefly
          // unreachable (e.g. during a deploy).
        });
    };
    fetchStats();
    const id = window.setInterval(fetchStats, 10_000);
    return () => {
      cancelled = true;
      window.clearInterval(id);
    };
  }, [activeJourney?.id]);

  // Create new journey
  const createJourney = () => {
    const newJourney: Journey = {
      id: `journey-${Date.now()}`,
      name: 'New Journey',
      status: 'draft',
      nodes: [
        {
          id: `node-${Date.now()}`,
          type: 'trigger',
          position: { x: 400, y: 100 },
          // Default new journeys to the segment trigger so the first thing
          // the user sees is the "cleaned, never mailed" workflow described
          // in the Welcome Series plan.
          config: { name: 'Journey Start', triggerType: 'segment' },
          connections: [],
        },
      ],
      connections: [],
      createdAt: new Date().toISOString(),
      // Sensible defaults for the Welcome Series use case: exit on any
      // engagement, no per-ISP quotas (executor falls back to global).
      exitOnOpen: true,
      exitOnClick: true,
      ispQuotas: {},
    };
    setActiveJourney(newJourney);
    setJourneys([...journeys, newJourney]);
  };

  // Phase 5 (Welcome Series E2E): one-click skeleton for the
  // 4-email + 3-delay welcome waterfall. Operator picks a brand,
  // we lay out trigger -> email1 -> delay -> email2 -> delay ->
  // email3 -> delay -> email4(offer). Sending profile + template
  // are left blank for the operator to fill via the email config
  // panel — Phase 1 makes that a 2-click workflow per node.
  // Phase 6 reuses this same helper to clone for HT, MH, QF.
  const createWelcomeSeriesJourney = (brandLabel = 'Discount Blog') => {
    const ts = Date.now();
    const triggerId = `trigger-${ts}`;
    const e1 = `email-${ts}-1`;
    const d1 = `delay-${ts}-1`;
    const e2 = `email-${ts}-2`;
    const d2 = `delay-${ts}-2`;
    const e3 = `email-${ts}-3`;
    const d3 = `delay-${ts}-3`;
    const e4 = `email-${ts}-4`;

    const nodes: JourneyNode[] = [
      {
        id: triggerId,
        type: 'trigger',
        position: { x: 100, y: 200 },
        config: {
          name: 'Cleaned, Never Mailed',
          triggerType: 'segment',
          segmentId: CLEANED_NEVER_MAILED_PRESET_ID,
        },
        connections: [e1],
      },
      {
        id: e1,
        type: 'email',
        position: { x: 320, y: 200 },
        config: { name: `${brandLabel} Welcome 1` },
        connections: [d1],
      },
      {
        id: d1,
        type: 'delay',
        position: { x: 540, y: 200 },
        config: {
          name: 'Wait until 9 AM',
          delayType: 'until_time',
          untilTime: '09:00',
          untilTimezone: 'America/Denver',
        },
        connections: [e2],
      },
      {
        id: e2,
        type: 'email',
        position: { x: 760, y: 200 },
        config: { name: `${brandLabel} Welcome 2` },
        connections: [d2],
      },
      {
        id: d2,
        type: 'delay',
        position: { x: 980, y: 200 },
        config: {
          name: 'Wait until 9 AM',
          delayType: 'until_time',
          untilTime: '09:00',
          untilTimezone: 'America/Denver',
        },
        connections: [e3],
      },
      {
        id: e3,
        type: 'email',
        position: { x: 1200, y: 200 },
        config: { name: `${brandLabel} Welcome 3` },
        connections: [d3],
      },
      {
        id: d3,
        type: 'delay',
        position: { x: 1420, y: 200 },
        config: {
          name: 'Wait until 9 AM',
          delayType: 'until_time',
          untilTime: '09:00',
          untilTimezone: 'America/Denver',
        },
        connections: [e4],
      },
      {
        id: e4,
        type: 'email',
        position: { x: 1640, y: 200 },
        config: { name: `${brandLabel} Welcome 4 - Offer` },
        connections: [],
      },
    ];

    const connections: Connection[] = [
      { from: triggerId, to: e1 },
      { from: e1, to: d1 },
      { from: d1, to: e2 },
      { from: e2, to: d2 },
      { from: d2, to: e3 },
      { from: e3, to: d3 },
      { from: d3, to: e4 },
    ];

    const newJourney: Journey = {
      id: `journey-${ts}`,
      name: `${brandLabel} Welcome Series`,
      status: 'draft',
      nodes,
      connections,
      createdAt: new Date().toISOString(),
      // Per locked-in user decision: any open or click anywhere
      // exits the subscriber from the journey.
      exitOnOpen: true,
      exitOnClick: true,
      // Reasonable starting hourly per-ISP caps for a brand-new
      // welcome waterfall. Operator should tune these in the
      // settings drawer once warm-up data is in.
      ispQuotas: {
        gmail: 5000,
        yahoo: 2000,
        microsoft: 2000,
        aol: 1000,
        comcast: 1000,
        charter: 1000,
        att: 1000,
        sbcglobal: 1000,
        cox: 500,
        apple: 1000,
        other: 1000,
      },
    };
    setActiveJourney(newJourney);
    setJourneys([...journeys, newJourney]);
  };

  // Update a journey-level setting (exit toggles, ISP quotas, sending profile).
  // Phase 2 backend will persist these; until then they round-trip through
  // the unknown-field-tolerant Go json.Unmarshal at /api/mailing/journeys.
  const updateJourneySetting = <K extends keyof Journey>(key: K, value: Journey[K]) => {
    if (!activeJourney) return;
    setActiveJourney({ ...activeJourney, [key]: value });
  };

  // Update a single ISP bucket's hourly quota. Empty/0 removes the entry so
  // the executor treats it as "unlimited within global caps".
  const updateIspQuota = (bucket: string, value: number) => {
    if (!activeJourney) return;
    const next = { ...(activeJourney.ispQuotas ?? {}) };
    if (Number.isFinite(value) && value > 0) {
      next[bucket] = value;
    } else {
      delete next[bucket];
    }
    setActiveJourney({ ...activeJourney, ispQuotas: next });
  };

  // When an email node picks a sending profile, prepopulate the from-name,
  // from-email and sending domain on the node config so the user can see
  // exactly what envelope will be used. They can still override fromName
  // and fromEmail if they need a per-node alias.
  const applySendingProfileDefaults = (profileId: string) => {
    const profile = profiles.find(p => p.id === profileId);
    if (!profile) return;
    updateNodeConfig({
      sendingProfileId: profileId,
      sendingDomain: profile.sending_domain ?? '',
      fromName: profile.from_name ?? selectedNode?.config.fromName ?? '',
      fromEmail: profile.from_email ?? selectedNode?.config.fromEmail ?? '',
    });
  };

  // Pick a template from the library: store id + display name on the node
  // config so the Journey Builder canvas can show it without an extra fetch.
  const applyTemplateSelection = async (template: TemplateOption) => {
    updateNodeConfig({
      templateId: template.id,
      templateName: template.name,
      subject: selectedNode?.config.subject || template.subject || '',
    });
    setShowTemplatePicker(false);
  };

  // Fetch full template HTML for preview. Falls back to the live htmlContent
  // on the node if no template is attached.
  const openEmailPreview = async () => {
    if (!selectedNode) return;
    const tplId = selectedNode.config.templateId;
    if (tplId) {
      try {
        const resp = await apiFetch(`/api/mailing/templates/${tplId}`);
        if (resp.ok) {
          const detail: TemplateDetail = await resp.json();
          setPreviewHtml(detail.html_content || '<p><em>(empty template)</em></p>');
          setPreviewSubject(detail.subject || selectedNode.config.subject || '(no subject)');
          setShowEmailPreview(true);
          return;
        }
      } catch {
        /* fall through to inline content */
      }
    }
    setPreviewHtml(selectedNode.config.htmlContent || '<p><em>(no preview available — paste HTML or pick a template)</em></p>');
    setPreviewSubject(selectedNode.config.subject || '(no subject)');
    setShowEmailPreview(true);
  };

  // Add node to canvas
  const addNode = (type: JourneyNode['type']) => {
    if (!activeJourney) return;
    
    const newNode: JourneyNode = {
      id: `node-${Date.now()}`,
      type,
      position: { x: 400, y: (activeJourney.nodes.length + 1) * 150 },
      config: { name: NODE_TYPES[type].label },
      connections: [],
    };
    
    setActiveJourney({
      ...activeJourney,
      nodes: [...activeJourney.nodes, newNode],
    });
  };

  // Delete node
  const deleteNode = (nodeId: string) => {
    if (!activeJourney) return;
    
    setActiveJourney({
      ...activeJourney,
      nodes: activeJourney.nodes.filter(n => n.id !== nodeId),
      connections: activeJourney.connections.filter(c => c.from !== nodeId && c.to !== nodeId),
    });
    setSelectedNode(null);
    setShowNodeConfig(false);
  };

  // Handle node drag
  const handleNodeDrag = useCallback((_e: React.MouseEvent, nodeId: string) => {
    if (!activeJourney || !canvasRef.current) return;
    
    const canvas = canvasRef.current;
    const rect = canvas.getBoundingClientRect();
    
    const onMouseMove = (moveEvent: MouseEvent) => {
      const x = (moveEvent.clientX - rect.left - canvasOffset.x) / zoom;
      const y = (moveEvent.clientY - rect.top - canvasOffset.y) / zoom;
      
      setActiveJourney(prev => {
        if (!prev) return prev;
        return {
          ...prev,
          nodes: prev.nodes.map(n => 
            n.id === nodeId ? { ...n, position: { x, y } } : n
          ),
        };
      });
    };
    
    const onMouseUp = () => {
      document.removeEventListener('mousemove', onMouseMove);
      document.removeEventListener('mouseup', onMouseUp);
      setDraggingNode(null);
    };
    
    setDraggingNode(nodeId);
    document.addEventListener('mousemove', onMouseMove);
    document.addEventListener('mouseup', onMouseUp);
  }, [activeJourney, canvasOffset, zoom]);

  // Start connection
  const startConnection = (nodeId: string, position: Position) => {
    setConnecting({ from: nodeId, fromPos: position });
  };

  // Complete connection
  const completeConnection = (toNodeId: string) => {
    if (!connecting || !activeJourney || connecting.from === toNodeId) {
      setConnecting(null);
      return;
    }
    
    // Check if connection already exists
    const exists = activeJourney.connections.some(
      c => c.from === connecting.from && c.to === toNodeId
    );
    
    if (!exists) {
      setActiveJourney({
        ...activeJourney,
        connections: [...activeJourney.connections, { from: connecting.from, to: toNodeId }],
      });
    }
    
    setConnecting(null);
  };

  // Delete connection
  const deleteConnection = (from: string, to: string) => {
    if (!activeJourney) return;
    
    setActiveJourney({
      ...activeJourney,
      connections: activeJourney.connections.filter(c => !(c.from === from && c.to === to)),
    });
  };

  // Save journey
  const saveJourney = async () => {
    if (!activeJourney) return;
    
    try {
      await apiFetch(`/api/mailing/journeys/${activeJourney.id}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(activeJourney),
      });
      
      setJourneys(journeys.map(j => j.id === activeJourney.id ? activeJourney : j));
      alert('Journey saved successfully!');
    } catch (error) {
      console.error('Error saving journey:', error);
    }
  };

  // Activate journey
  const activateJourney = async () => {
    if (!activeJourney) return;
    
    const updated = { ...activeJourney, status: 'active' as const };
    setActiveJourney(updated);
    setJourneys(journeys.map(j => j.id === activeJourney.id ? updated : j));
    
    try {
      await apiFetch(`/api/mailing/journeys/${activeJourney.id}/activate`, { method: 'POST' });
    } catch (error) {
      console.error('Error activating journey:', error);
    }
  };

  // Update node config
  const updateNodeConfig = (config: Partial<NodeConfig>) => {
    if (!selectedNode || !activeJourney) return;
    
    const updatedNode = { ...selectedNode, config: { ...selectedNode.config, ...config } };
    setSelectedNode(updatedNode);
    setActiveJourney({
      ...activeJourney,
      nodes: activeJourney.nodes.map(n => n.id === selectedNode.id ? updatedNode : n),
    });
  };

  // Render connection lines
  const renderConnections = () => {
    if (!activeJourney) return null;
    
    return activeJourney.connections.map((conn, idx) => {
      const fromNode = activeJourney.nodes.find(n => n.id === conn.from);
      const toNode = activeJourney.nodes.find(n => n.id === conn.to);
      
      if (!fromNode || !toNode) return null;
      
      const x1 = fromNode.position.x + 100;
      const y1 = fromNode.position.y + 50;
      const x2 = toNode.position.x + 100;
      const y2 = toNode.position.y;
      
      const midY = (y1 + y2) / 2;
      const path = `M ${x1} ${y1} C ${x1} ${midY}, ${x2} ${midY}, ${x2} ${y2}`;
      
      return (
        <g key={idx} className="connection-line">
          <path d={path} stroke="#6b7280" strokeWidth="2" fill="none" markerEnd="url(#arrowhead)" />
          <circle 
            cx={(x1 + x2) / 2} 
            cy={midY} 
            r="8" 
            fill="#e94560" 
            className="delete-connection"
            onClick={() => deleteConnection(conn.from, conn.to)}
          />
          <text x={(x1 + x2) / 2} y={midY + 4} textAnchor="middle" fill="white" fontSize="10">×</text>
        </g>
      );
    });
  };

  // Render node
  const renderNode = (node: JourneyNode) => {
    const nodeType = NODE_TYPES[node.type];
    const isSelected = selectedNode?.id === node.id;
    
    return (
      <div
        key={node.id}
        className={`journey-node ${node.type} ${isSelected ? 'selected' : ''}`}
        style={{
          left: node.position.x,
          top: node.position.y,
          borderColor: nodeType.color,
        }}
        onMouseDown={(e) => {
          e.stopPropagation();
          handleNodeDrag(e, node.id);
        }}
        onClick={(e) => {
          e.stopPropagation();
          setSelectedNode(node);
          setShowNodeConfig(true);
        }}
      >
        <div className="node-header" style={{ background: nodeType.color }}>
          <span className="node-icon"><FontAwesomeIcon icon={nodeType.icon} /></span>
          <span className="node-type">{nodeType.label}</span>
        </div>
        <div className="node-body">
          <div className="node-name">{node.config.name || nodeType.label}</div>
          {node.type === 'trigger' && node.config.triggerType && (
            <div className="node-detail">
              {node.config.triggerType === 'schedule' ? '📅 Scheduled' : '📈 Performance-based'}
            </div>
          )}
          {node.type === 'email' && (
            <>
              {node.config.subject && (
                <div className="node-detail">{node.config.subject}</div>
              )}
              {node.config.sendingDomain && (
                <div className="node-detail" style={{ opacity: 0.75 }}>
                  via {node.config.sendingDomain}
                </div>
              )}
              {/* Phase 3 (Welcome Series wave-native): lifetime per-node
                 stats sourced from /api/mailing/journeys/:id/node-stats.
                 Hard/soft bounce shown separately per
                 .cursor/rules/bounce-metrics.mdc — never combined.
                 Missing keys render as zero via `?? 0` so a freshly
                 created journey shows a clean baseline. */}
              <div className="node-stats" data-testid={`node-stats-${node.id}`}>
                <div className="node-stat"><span>Awaiting</span><strong>{nodeStats[node.id]?.audience_awaiting ?? 0}</strong></div>
                <div className="node-stat"><span>Delivered</span><strong>{nodeStats[node.id]?.delivered ?? 0}</strong></div>
                <div className="node-stat"><span>Opens</span><strong>{nodeStats[node.id]?.opens ?? 0}</strong></div>
                <div className="node-stat"><span>Clicks</span><strong>{nodeStats[node.id]?.clicks ?? 0}</strong></div>
                <div
                  className="node-stat"
                  style={{ color: (nodeStats[node.id]?.hard_bounce ?? 0) > 0 ? '#ef4444' : undefined }}
                >
                  <span>Hard Bounce</span>
                  <strong>{nodeStats[node.id]?.hard_bounce ?? 0}</strong>
                </div>
                <div
                  className="node-stat"
                  style={{ color: (nodeStats[node.id]?.soft_bounce ?? 0) > 0 ? '#f59e0b' : undefined }}
                >
                  <span>Soft Bounce</span>
                  <strong>{nodeStats[node.id]?.soft_bounce ?? 0}</strong>
                </div>
              </div>
            </>
          )}
          {node.type === 'delay' && node.config.delayValue && (
            <div className="node-detail">
              Wait {node.config.delayValue} {node.config.delayUnit}
            </div>
          )}
          {node.type === 'condition' && node.config.conditionType && (
            <div className="node-detail">If: {node.config.conditionType}</div>
          )}
        </div>
        
        {/* Connection points */}
        {node.type !== 'goal' && (
          <div 
            className="connection-point output"
            onMouseDown={(e) => {
              e.stopPropagation();
              const rect = e.currentTarget.getBoundingClientRect();
              startConnection(node.id, { x: rect.left + rect.width / 2, y: rect.top + rect.height / 2 });
            }}
          />
        )}
        {node.type !== 'trigger' && (
          <div 
            className="connection-point input"
            onMouseUp={(e) => {
              e.stopPropagation();
              completeConnection(node.id);
            }}
          />
        )}
        
        {/* Delete button */}
        {node.type !== 'trigger' && (
          <button 
            className="delete-node"
            onClick={(e) => {
              e.stopPropagation();
              deleteNode(node.id);
            }}
          >
            ×
          </button>
        )}
      </div>
    );
  };

  // Node configuration panel
  const renderNodeConfig = () => {
    if (!selectedNode || !showNodeConfig) return null;
    
    const nodeType = NODE_TYPES[selectedNode.type];
    
    return (
      <div className="node-config-panel">
        <div className="config-header">
          <h3>
            <span style={{ color: nodeType.color }}><FontAwesomeIcon icon={nodeType.icon} /></span>
            {' '}Configure {nodeType.label}
          </h3>
          <button onClick={() => setShowNodeConfig(false)}>×</button>
        </div>
        
        <div className="config-body">
          <div className="config-group">
            <label>Node Name</label>
            <input
              type="text"
              value={selectedNode.config.name || ''}
              onChange={(e) => updateNodeConfig({ name: e.target.value })}
              placeholder="Give this step a name"
            />
          </div>
          
          {/* Trigger Configuration */}
          {selectedNode.type === 'trigger' && (
            <>
              <div className="config-group">
                <label>Trigger Type</label>
                <div className="trigger-type-selector">
                  <label className={`trigger-option ${selectedNode.config.triggerType === 'segment' ? 'selected' : ''}`}>
                    <input
                      type="radio"
                      name="triggerType"
                      value="segment"
                      checked={selectedNode.config.triggerType === 'segment'}
                      onChange={(e) => updateNodeConfig({ triggerType: e.target.value as any })}
                    />
                    <div className="trigger-content">
                      <span className="trigger-icon"><FontAwesomeIcon icon={faFilter} /></span>
                      <strong>Segment-based</strong>
                      <small>Auto-enroll subscribers matching a segment</small>
                    </div>
                  </label>
                  <label className={`trigger-option ${selectedNode.config.triggerType === 'schedule' ? 'selected' : ''}`}>
                    <input
                      type="radio"
                      name="triggerType"
                      value="schedule"
                      checked={selectedNode.config.triggerType === 'schedule'}
                      onChange={(e) => updateNodeConfig({ triggerType: e.target.value as any })}
                    />
                    <div className="trigger-content">
                      <span className="trigger-icon">📅</span>
                      <strong>Schedule-based</strong>
                      <small>Start at a specific date/time</small>
                    </div>
                  </label>
                  <label className={`trigger-option ${selectedNode.config.triggerType === 'performance' ? 'selected' : ''}`}>
                    <input
                      type="radio"
                      name="triggerType"
                      value="performance"
                      checked={selectedNode.config.triggerType === 'performance'}
                      onChange={(e) => updateNodeConfig({ triggerType: e.target.value as any })}
                    />
                    <div className="trigger-content">
                      <span className="trigger-icon">📈</span>
                      <strong>Performance-based</strong>
                      <small>Triggered by subscriber actions</small>
                    </div>
                  </label>
                  <label className={`trigger-option ${selectedNode.config.triggerType === 'event' ? 'selected' : ''}`}>
                    <input
                      type="radio"
                      name="triggerType"
                      value="event"
                      checked={selectedNode.config.triggerType === 'event'}
                      onChange={(e) => updateNodeConfig({ triggerType: e.target.value as any })}
                    />
                    <div className="trigger-content">
                      <span className="trigger-icon">🎯</span>
                      <strong>Event-based</strong>
                      <small>Triggered by custom events</small>
                    </div>
                  </label>
                </div>
              </div>

              {/* Segment trigger: pick a saved segment OR the
                 "cleaned, never mailed" preset described in the Welcome
                 Series plan. The preset is materialized server-side by the
                 Phase 2 segment_enroller worker. */}
              {selectedNode.config.triggerType === 'segment' && (
                <>
                  <div className="config-group">
                    <label>Audience Segment *</label>
                    <select
                      value={selectedNode.config.segmentId || ''}
                      onChange={(e) => {
                        const id = e.target.value;
                        const seg = segments.find(s => s.id === id);
                        updateNodeConfig({
                          segmentId: id,
                          segmentName: id === CLEANED_NEVER_MAILED_PRESET_ID
                            ? 'Cleaned, never mailed'
                            : (seg?.name ?? ''),
                        });
                      }}
                    >
                      <option value="">Select an audience segment</option>
                      <option value={CLEANED_NEVER_MAILED_PRESET_ID}>
                        ★ Preset — Cleaned, never mailed (welcome series)
                      </option>
                      {segments.map((seg) => (
                        <option key={seg.id} value={seg.id}>
                          {seg.name}
                          {seg.subscriber_count !== undefined ? ` (${seg.subscriber_count.toLocaleString()} subs)` : ''}
                        </option>
                      ))}
                    </select>
                    {selectedNode.config.segmentId === CLEANED_NEVER_MAILED_PRESET_ID && (
                      <small>
                        This preset auto-enrolls every subscriber whose <code>verified_at</code> is set
                        and whose <code>computed_fields.first_email_at</code> is NULL — i.e. cleaned
                        but never mailed. Wave-native sends will respect global suppression and the
                        per-journey ISP quotas configured in Settings.
                      </small>
                    )}
                  </div>
                </>
              )}

              {/* Legacy list trigger — kept available so older journeys
                 can still load and edit. Not exposed via radio because the
                 segment trigger covers everything a list trigger could. */}
              {selectedNode.config.triggerType !== 'segment' && (
                <div className="config-group">
                  <label>Target List *</label>
                  <select
                    value={selectedNode.config.listId || ''}
                    onChange={(e) => updateNodeConfig({ listId: e.target.value })}
                  >
                    <option value="">Select a list</option>
                    {lists.map((list) => (
                      <option key={list.id} value={list.id}>
                        {list.name} ({list.subscriber_count?.toLocaleString() || 0} subscribers)
                      </option>
                    ))}
                  </select>
                </div>
              )}
              
              {selectedNode.config.triggerType === 'schedule' && (
                <>
                  <div className="config-group">
                    <label>Schedule Type</label>
                    <select
                      value={selectedNode.config.scheduleType || 'once'}
                      onChange={(e) => updateNodeConfig({ scheduleType: e.target.value as any })}
                    >
                      <option value="once">One-time</option>
                      <option value="recurring">Recurring</option>
                    </select>
                  </div>
                  <div className="config-group">
                    <label>Start Date & Time</label>
                    <input
                      type="datetime-local"
                      value={selectedNode.config.scheduleDate || ''}
                      onChange={(e) => updateNodeConfig({ scheduleDate: e.target.value })}
                    />
                  </div>
                  {selectedNode.config.scheduleType === 'recurring' && (
                    <div className="config-group">
                      <label>Recurring Pattern</label>
                      <select
                        value={selectedNode.config.recurringPattern || 'daily'}
                        onChange={(e) => updateNodeConfig({ recurringPattern: e.target.value })}
                      >
                        <option value="daily">Daily</option>
                        <option value="weekly">Weekly</option>
                        <option value="monthly">Monthly</option>
                      </select>
                    </div>
                  )}
                </>
              )}
              
              {selectedNode.config.triggerType === 'performance' && (
                <>
                  <div className="config-group">
                    <label>Performance Event</label>
                    <select
                      value={selectedNode.config.performanceEvent || 'open'}
                      onChange={(e) => updateNodeConfig({ performanceEvent: e.target.value as any })}
                    >
                      <option value="open">Opened email</option>
                      <option value="click">Clicked link</option>
                      <option value="no_open">Did NOT open (within window)</option>
                      <option value="no_click">Did NOT click (within window)</option>
                      <option value="bounce">Email bounced</option>
                      <option value="complaint">Marked as spam</option>
                    </select>
                  </div>
                  <div className="config-group">
                    <label>Time Window (hours)</label>
                    <input
                      type="number"
                      value={selectedNode.config.performanceWindow || 24}
                      onChange={(e) => updateNodeConfig({ performanceWindow: parseInt(e.target.value) })}
                      min={1}
                      max={168}
                    />
                    <small>Check for this event within this time window</small>
                  </div>
                </>
              )}
            </>
          )}
          
          {/* Email Configuration */}
          {selectedNode.type === 'email' && (
            <>
              <div className="config-group">
                <label>Sending Profile (ESP)</label>
                <select
                  data-testid="email-sending-profile-select"
                  value={selectedNode.config.sendingProfileId || ''}
                  onChange={(e) => applySendingProfileDefaults(e.target.value)}
                >
                  <option value="">Use journey default profile</option>
                  {profiles.map((profile) => {
                    const domain = profile.sending_domain ? ` — ${profile.sending_domain}` : '';
                    return (
                      <option key={profile.id} value={profile.id}>
                        {profile.name}{domain} ({profile.vendor_type})
                      </option>
                    );
                  })}
                </select>
                {selectedNode.config.sendingDomain && (
                  <div className="sending-domain-badge" data-testid="email-sending-domain-badge">
                    <span className="badge-label">Sending domain:</span>
                    <code>{selectedNode.config.sendingDomain}</code>
                  </div>
                )}
                <small>
                  Selecting a profile prepopulates From Name and From Email below. Override
                  them per-node if you need a different alias for this step only.
                </small>
              </div>

              <div className="config-group">
                <label>Email Template</label>
                <div className="template-picker-row">
                  <input
                    type="text"
                    value={selectedNode.config.templateName || ''}
                    placeholder="No template selected"
                    readOnly
                    data-testid="email-template-name"
                  />
                  <button
                    type="button"
                    className="library-btn"
                    onClick={() => setShowTemplatePicker(true)}
                    data-testid="open-template-picker"
                  >
                    <FontAwesomeIcon icon={faBookOpen} style={{ marginRight: 6 }} />
                    Library
                  </button>
                  <button
                    type="button"
                    className="preview-btn"
                    onClick={openEmailPreview}
                    disabled={!selectedNode.config.templateId && !selectedNode.config.htmlContent}
                    data-testid="open-email-preview"
                  >
                    <FontAwesomeIcon icon={faEye} style={{ marginRight: 6 }} />
                    Preview
                  </button>
                </div>
              </div>

              <div className="config-group">
                <label>Subject Line *</label>
                <input
                  type="text"
                  value={selectedNode.config.subject || ''}
                  onChange={(e) => updateNodeConfig({ subject: e.target.value })}
                  placeholder="Email subject"
                />
              </div>
              <div className="config-row">
                <div className="config-group">
                  <label>From Name</label>
                  <input
                    type="text"
                    data-testid="email-from-name"
                    value={selectedNode.config.fromName || ''}
                    onChange={(e) => updateNodeConfig({ fromName: e.target.value })}
                    placeholder="Sender name"
                  />
                </div>
                <div className="config-group">
                  <label>From Email</label>
                  <input
                    type="email"
                    data-testid="email-from-email"
                    value={selectedNode.config.fromEmail || ''}
                    onChange={(e) => updateNodeConfig({ fromEmail: e.target.value })}
                    placeholder="sender@example.com"
                  />
                </div>
              </div>
              <div className="config-group">
                <label>Inline HTML override (optional)</label>
                <textarea
                  value={selectedNode.config.htmlContent || ''}
                  onChange={(e) => updateNodeConfig({ htmlContent: e.target.value })}
                  placeholder="Leave empty to use the template's HTML."
                  rows={6}
                />
                <small>If set, this HTML overrides the template body for this node only.</small>
              </div>
            </>
          )}
          
          {/* Delay Configuration */}
          {selectedNode.type === 'delay' && (
            <>
              <div className="config-group">
                <label>Delay Type</label>
                <select
                  value={selectedNode.config.delayType || 'fixed'}
                  onChange={(e) => updateNodeConfig({ delayType: e.target.value as any })}
                >
                  <option value="fixed">Fixed duration</option>
                  <option value="until_time">Until specific time of day</option>
                  <option value="until_day">Until specific day of week</option>
                </select>
              </div>
              
              {selectedNode.config.delayType === 'fixed' && (
                <div className="config-row">
                  <div className="config-group">
                    <label>Wait for</label>
                    <input
                      type="number"
                      value={selectedNode.config.delayValue || 1}
                      onChange={(e) => updateNodeConfig({ delayValue: parseInt(e.target.value) })}
                      min={1}
                    />
                  </div>
                  <div className="config-group">
                    <label>Unit</label>
                    <select
                      value={selectedNode.config.delayUnit || 'days'}
                      onChange={(e) => updateNodeConfig({ delayUnit: e.target.value as any })}
                    >
                      <option value="minutes">Minutes</option>
                      <option value="hours">Hours</option>
                      <option value="days">Days</option>
                      <option value="weeks">Weeks</option>
                    </select>
                  </div>
                </div>
              )}
              
              {selectedNode.config.delayType === 'until_time' && (
                <div className="config-row">
                  <div className="config-group">
                    <label>Wait until</label>
                    <input
                      type="time"
                      data-testid="delay-until-time"
                      value={selectedNode.config.untilTime || '09:00'}
                      onChange={(e) => updateNodeConfig({ untilTime: e.target.value })}
                    />
                  </div>
                  <div className="config-group">
                    <label>Timezone</label>
                    <select
                      data-testid="delay-until-timezone"
                      value={selectedNode.config.untilTimezone || 'America/Denver'}
                      onChange={(e) => updateNodeConfig({ untilTimezone: e.target.value })}
                    >
                      {DELAY_TIMEZONES.map(tz => (
                        <option key={tz.value} value={tz.value}>{tz.label}</option>
                      ))}
                    </select>
                  </div>
                </div>
              )}
              
              {selectedNode.config.delayType === 'until_day' && (
                <div className="config-group">
                  <label>Wait until</label>
                  <select
                    value={selectedNode.config.untilDay || 'monday'}
                    onChange={(e) => updateNodeConfig({ untilDay: e.target.value })}
                  >
                    <option value="monday">Monday</option>
                    <option value="tuesday">Tuesday</option>
                    <option value="wednesday">Wednesday</option>
                    <option value="thursday">Thursday</option>
                    <option value="friday">Friday</option>
                    <option value="saturday">Saturday</option>
                    <option value="sunday">Sunday</option>
                  </select>
                </div>
              )}
            </>
          )}
          
          {/* Condition Configuration */}
          {selectedNode.type === 'condition' && (
            <>
              <div className="config-group">
                <label>Condition Type</label>
                <select
                  value={selectedNode.config.conditionType || 'opened'}
                  onChange={(e) => updateNodeConfig({ conditionType: e.target.value as any })}
                >
                  <option value="opened">Opened previous email</option>
                  <option value="clicked">Clicked in previous email</option>
                  <option value="not_opened">Did NOT open previous email</option>
                  <option value="not_clicked">Did NOT click in previous email</option>
                  <option value="custom">Custom field condition</option>
                </select>
              </div>
              
              {selectedNode.config.conditionType === 'custom' && (
                <>
                  <div className="config-group">
                    <label>Field</label>
                    <input
                      type="text"
                      value={selectedNode.config.conditionField || ''}
                      onChange={(e) => updateNodeConfig({ conditionField: e.target.value })}
                      placeholder="e.g., country, plan_type"
                    />
                  </div>
                  <div className="config-row">
                    <div className="config-group">
                      <label>Operator</label>
                      <select
                        value={selectedNode.config.conditionOperator || 'equals'}
                        onChange={(e) => updateNodeConfig({ conditionOperator: e.target.value })}
                      >
                        <option value="equals">Equals</option>
                        <option value="not_equals">Does not equal</option>
                        <option value="contains">Contains</option>
                        <option value="starts_with">Starts with</option>
                      </select>
                    </div>
                    <div className="config-group">
                      <label>Value</label>
                      <input
                        type="text"
                        value={selectedNode.config.conditionValue || ''}
                        onChange={(e) => updateNodeConfig({ conditionValue: e.target.value })}
                        placeholder="Value to compare"
                      />
                    </div>
                  </div>
                </>
              )}
              
              <div className="condition-branches">
                <div className="branch yes">
                  <span>✅ YES branch</span>
                  <small>Continues if condition is true</small>
                </div>
                <div className="branch no">
                  <span>❌ NO branch</span>
                  <small>Continues if condition is false</small>
                </div>
              </div>
            </>
          )}
          
          {/* Split Configuration */}
          {selectedNode.type === 'split' && (
            <>
              <div className="config-group">
                <label>Split Type</label>
                <select
                  value={selectedNode.config.splitType || 'percentage'}
                  onChange={(e) => updateNodeConfig({ splitType: e.target.value as any })}
                >
                  <option value="percentage">Percentage split</option>
                  <option value="random">Random 50/50</option>
                </select>
              </div>
              
              {selectedNode.config.splitType === 'percentage' && (
                <div className="config-group">
                  <label>Variant A Percentage: {selectedNode.config.splitPercentage || 50}%</label>
                  <input
                    type="range"
                    value={selectedNode.config.splitPercentage || 50}
                    onChange={(e) => updateNodeConfig({ splitPercentage: parseInt(e.target.value) })}
                    min={10}
                    max={90}
                  />
                  <div className="split-preview">
                    <span>A: {selectedNode.config.splitPercentage || 50}%</span>
                    <span>B: {100 - (selectedNode.config.splitPercentage || 50)}%</span>
                  </div>
                </div>
              )}
            </>
          )}
          
          {/* Goal Configuration */}
          {selectedNode.type === 'goal' && (
            <>
              <div className="config-group">
                <label>Goal Type</label>
                <select
                  value={selectedNode.config.goalType || 'conversion'}
                  onChange={(e) => updateNodeConfig({ goalType: e.target.value as any })}
                >
                  <option value="conversion">Conversion (purchase, signup)</option>
                  <option value="engagement">Engagement (opened all emails)</option>
                  <option value="custom">Custom event</option>
                </select>
              </div>
              
              {selectedNode.config.goalType === 'custom' && (
                <div className="config-group">
                  <label>Goal Event Name</label>
                  <input
                    type="text"
                    value={selectedNode.config.goalValue || ''}
                    onChange={(e) => updateNodeConfig({ goalValue: e.target.value })}
                    placeholder="e.g., purchase_completed"
                  />
                </div>
              )}
            </>
          )}
        </div>
        
        <div className="config-footer">
          <button className="save-config-btn" onClick={() => setShowNodeConfig(false)}>
            ✓ Done
          </button>
        </div>
      </div>
    );
  };

  // Journey list view
  const renderJourneyList = () => (
    <div className="journey-list-view">
      <div className="journey-header">
        <div>
          <h2>📧 Email Journeys</h2>
          <p>Create automated email sequences triggered by schedules or subscriber behavior</p>
        </div>
        <div style={{ display: 'flex', gap: 12, alignItems: 'center' }}>
          {/* Phase 5/6 (Welcome Series): one-click brand-tagged skeletons.
             Operator picks a brand, fills in templates + profile per
             email node, then activates. Same helper drives multi-brand
             cloning for Phase 6 (HT, MH, QF). */}
          <select
            data-testid="welcome-preset-brand"
            defaultValue=""
            onChange={(e) => {
              const v = e.target.value;
              e.target.value = '';
              if (!v) return;
              createWelcomeSeriesJourney(v);
            }}
            style={{
              padding: '8px 12px',
              borderRadius: 6,
              border: '1px solid rgba(99,102,241,0.3)',
              background: 'rgba(15,15,30,0.6)',
              color: '#e0e0ff',
              fontSize: 14,
              cursor: 'pointer',
            }}
          >
            <option value="">+ From Welcome Preset…</option>
            <option value="Discount Blog">Discount Blog</option>
            <option value="History Thinking">History Thinking</option>
            <option value="My Own Health">My Own Health</option>
            <option value="Quiz Fiesta">Quiz Fiesta</option>
          </select>
          <button className="create-btn" onClick={createJourney}>
            + Create Journey
          </button>
        </div>
      </div>
      
      {journeys.length === 0 ? (
        <div className="empty-state">
          <div className="empty-icon">🗺️</div>
          <h3>No journeys yet</h3>
          <p>Create your first automated email journey to engage subscribers</p>
          <button className="create-btn" onClick={createJourney}>
            + Create Your First Journey
          </button>
        </div>
      ) : (
        <div className="journey-grid">
          {journeys.map((journey) => (
            <div 
              key={journey.id} 
              className="journey-card"
              onClick={() => setActiveJourney(journey)}
            >
              <div className="journey-card-header">
                <h3>{journey.name}</h3>
                <span className={`status-badge ${journey.status}`}>
                  {journey.status}
                </span>
              </div>
              <div className="journey-card-stats">
                <div className="stat">
                  <span className="stat-value">{journey.nodes?.length || 0}</span>
                  <span className="stat-label">Steps</span>
                </div>
                <div className="stat">
                  <span className="stat-value">{journey.stats?.entered || 0}</span>
                  <span className="stat-label">Entered</span>
                </div>
                <div className="stat">
                  <span className="stat-value">{journey.stats?.completed || 0}</span>
                  <span className="stat-label">Completed</span>
                </div>
              </div>
              <div className="journey-card-footer">
                <span>Created {new Date(journey.createdAt).toLocaleDateString()}</span>
                <button className="edit-btn">Edit →</button>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );

  // Canvas view
  const renderCanvas = () => (
    <div className="journey-canvas-view">
      <div className="canvas-header">
        <div className="canvas-header-left">
          <button className="back-btn" onClick={() => setActiveJourney(null)}>
            ← Back
          </button>
          <input
            type="text"
            className="journey-name-input"
            value={activeJourney?.name || ''}
            onChange={(e) => setActiveJourney({ ...activeJourney!, name: e.target.value })}
            placeholder="Journey name"
          />
          <span className={`status-badge ${activeJourney?.status}`}>
            {activeJourney?.status}
          </span>
        </div>
        <div className="canvas-header-right">
          <div className="zoom-controls">
            <button onClick={() => setZoom(Math.max(0.5, zoom - 0.1))}>−</button>
            <span>{Math.round(zoom * 100)}%</span>
            <button onClick={() => setZoom(Math.min(2, zoom + 0.1))}>+</button>
          </div>
          <button
            className="settings-btn"
            onClick={() => setShowJourneySettings(true)}
            data-testid="open-journey-settings"
            title="Journey settings (exit policy, ISP quotas)"
          >
            <FontAwesomeIcon icon={faCog} style={{ marginRight: 6 }} /> Settings
          </button>
          <button className="save-btn" onClick={saveJourney}>
            <FontAwesomeIcon icon={faSave} style={{ marginRight: 6 }} /> Save
          </button>
          {activeJourney?.status === 'draft' && (
            <button className="activate-btn" onClick={activateJourney}>
              <FontAwesomeIcon icon={faRocket} style={{ marginRight: 6 }} /> Activate
            </button>
          )}
        </div>
      </div>
      
      <div className="canvas-container">
        <div className="node-palette">
          <h4>Add Step</h4>
          {Object.entries(NODE_TYPES).map(([type, info]) => (
            type !== 'trigger' && (
              <button
                key={type}
                className="palette-item"
                onClick={() => addNode(type as JourneyNode['type'])}
                style={{ borderColor: info.color }}
              >
                <span className="palette-icon"><FontAwesomeIcon icon={info.icon} /></span>
                <div>
                  <strong>{info.label}</strong>
                  <small>{info.description}</small>
                </div>
              </button>
            )
          ))}
        </div>
        
        <div 
          className="canvas" 
          ref={canvasRef}
          onClick={() => {
            setSelectedNode(null);
            setShowNodeConfig(false);
          }}
        >
          <svg className="connections-layer" style={{ transform: `scale(${zoom})` }}>
            <defs>
              <marker
                id="arrowhead"
                markerWidth="10"
                markerHeight="7"
                refX="9"
                refY="3.5"
                orient="auto"
              >
                <polygon points="0 0, 10 3.5, 0 7" fill="#6b7280" />
              </marker>
            </defs>
            {renderConnections()}
          </svg>
          
          <div className="nodes-layer" style={{ transform: `scale(${zoom})` }}>
            {activeJourney?.nodes.map(renderNode)}
          </div>
        </div>
        
        {renderNodeConfig()}
      </div>
    </div>
  );

  if (loading) {
    return (
      <div className="journey-builder loading">
        <div className="loading-spinner">Loading...</div>
      </div>
    );
  }

  // Journey-level settings drawer: exit policy + ISP quotas + default
  // sending profile. Phase 1 stores everything client-side on the active
  // journey object and ships it through the existing PUT /api/mailing/journeys
  // endpoint; Phase 2 adds matching columns + handler hydration.
  const renderJourneySettings = () => {
    if (!showJourneySettings || !activeJourney) return null;
    return (
      <div className="jb-modal-backdrop" onClick={() => setShowJourneySettings(false)}>
        <div className="jb-drawer" onClick={(e) => e.stopPropagation()} role="dialog" aria-label="Journey settings">
          <div className="jb-drawer-header">
            <h3><FontAwesomeIcon icon={faCog} /> Journey Settings</h3>
            <button onClick={() => setShowJourneySettings(false)} aria-label="Close settings">×</button>
          </div>
          <div className="jb-drawer-body">
            <section className="settings-section">
              <h4>Default sending profile</h4>
              <p className="hint">
                Email nodes inherit this when their own profile is left blank.
              </p>
              <select
                data-testid="journey-default-profile"
                value={activeJourney.sendingProfileId || ''}
                onChange={(e) => updateJourneySetting('sendingProfileId', e.target.value)}
              >
                <option value="">(no default — each node must specify one)</option>
                {profiles.map((p) => {
                  const dom = p.sending_domain ? ` — ${p.sending_domain}` : '';
                  return (
                    <option key={p.id} value={p.id}>
                      {p.name}{dom} ({p.vendor_type})
                    </option>
                  );
                })}
              </select>
            </section>

            <section className="settings-section">
              <h4>Engagement exit policy</h4>
              <p className="hint">
                Phase 4 watcher exits any active enrollment in this journey
                when the subscriber opens or clicks <em>any</em> email in the
                system, not just emails from this journey.
              </p>
              <div className="toggle-row">
                <label htmlFor="exitOnOpen">Exit on open (any email, system-wide)</label>
                <input
                  id="exitOnOpen"
                  data-testid="journey-exit-on-open"
                  type="checkbox"
                  checked={!!activeJourney.exitOnOpen}
                  onChange={(e) => updateJourneySetting('exitOnOpen', e.target.checked)}
                />
              </div>
              <div className="toggle-row">
                <label htmlFor="exitOnClick">Exit on click (any email, system-wide)</label>
                <input
                  id="exitOnClick"
                  data-testid="journey-exit-on-click"
                  type="checkbox"
                  checked={!!activeJourney.exitOnClick}
                  onChange={(e) => updateJourneySetting('exitOnClick', e.target.checked)}
                />
              </div>
            </section>

            <section className="settings-section">
              <h4>Hourly ISP quotas</h4>
              <p className="hint">
                Wave-native sends throttle per ISP bucket. Leave a value at 0
                to fall back to global per-IP caps from the PMTA scheduler.
              </p>
              <table className="isp-quota-table" data-testid="journey-isp-quotas">
                <thead>
                  <tr>
                    <th>ISP bucket</th>
                    <th style={{ width: 120 }}>Hourly cap</th>
                  </tr>
                </thead>
                <tbody>
                  {ISP_QUOTA_BUCKETS.map((b) => (
                    <tr key={b.key}>
                      <td>{b.label}</td>
                      <td>
                        <input
                          type="number"
                          min={0}
                          step={100}
                          data-testid={`isp-quota-${b.key}`}
                          value={activeJourney.ispQuotas?.[b.key] ?? ''}
                          placeholder="0"
                          onChange={(e) => updateIspQuota(b.key, parseInt(e.target.value, 10))}
                        />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </section>

            <section className="settings-section">
              <h4>Suppression</h4>
              <p className="hint">
                Global suppression is always applied to every wave injection;
                this cannot be disabled on a per-journey basis. Hard bounces,
                unsubscribes, and complaints from any campaign suppress the
                subscriber for every journey.
              </p>
            </section>
          </div>
          <div className="jb-drawer-footer">
            <button onClick={() => setShowJourneySettings(false)}>Close</button>
            <button
              className="primary"
              onClick={() => {
                setShowJourneySettings(false);
                saveJourney();
              }}
            >
              Save settings
            </button>
          </div>
        </div>
      </div>
    );
  };

  // Library template picker for email nodes.
  const renderTemplatePicker = () => {
    if (!showTemplatePicker) return null;
    return (
      <div className="jb-modal-backdrop" onClick={() => setShowTemplatePicker(false)}>
        <div className="jb-drawer wide" onClick={(e) => e.stopPropagation()} role="dialog" aria-label="Template library">
          <div className="jb-drawer-header">
            <h3><FontAwesomeIcon icon={faBookOpen} /> Template Library</h3>
            <button onClick={() => setShowTemplatePicker(false)} aria-label="Close template picker">×</button>
          </div>
          <div className="jb-drawer-body">
            {templates.length === 0 ? (
              <p style={{ color: 'rgba(180,210,240,0.6)' }}>
                No templates found. Create templates from the Mailing → Templates page first.
              </p>
            ) : (
              <div className="template-grid" data-testid="template-grid">
                {templates.map((t) => (
                  <button
                    key={t.id}
                    type="button"
                    className="template-card"
                    onClick={() => applyTemplateSelection(t)}
                    data-testid={`template-card-${t.id}`}
                  >
                    <strong>{t.name}</strong>
                    {t.subject && <small>Subject: {t.subject}</small>}
                    {t.description && <small>{t.description}</small>}
                  </button>
                ))}
              </div>
            )}
          </div>
        </div>
      </div>
    );
  };

  // Email preview modal, populated by openEmailPreview().
  const renderEmailPreview = () => {
    if (!showEmailPreview) return null;
    return (
      <div className="jb-modal-backdrop" onClick={() => setShowEmailPreview(false)}>
        <div className="jb-drawer wide" onClick={(e) => e.stopPropagation()} role="dialog" aria-label="Email preview">
          <div className="jb-drawer-header">
            <h3><FontAwesomeIcon icon={faEye} /> Email preview</h3>
            <button onClick={() => setShowEmailPreview(false)} aria-label="Close preview">×</button>
          </div>
          <div className="jb-drawer-body">
            <div className="preview-subject" data-testid="preview-subject">
              <strong>Subject</strong>
              {previewSubject}
            </div>
            <iframe
              data-testid="preview-iframe"
              className="email-preview-frame"
              title="email-preview"
              srcDoc={previewHtml}
              sandbox=""
            />
          </div>
        </div>
      </div>
    );
  };

  return (
    <div className="journey-builder">
      {activeJourney ? renderCanvas() : renderJourneyList()}
      {renderJourneySettings()}
      {renderTemplatePicker()}
      {renderEmailPreview()}
      <span className="jb-version-footer" data-testid="journey-builder-version">
        Journey Builder v{JOURNEY_BUILDER_VERSION}
      </span>
    </div>
  );
};

export default JourneyBuilder;
