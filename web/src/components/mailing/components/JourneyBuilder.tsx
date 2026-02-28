import React, { useState, useRef, useEffect, useCallback } from 'react';
import './JourneyBuilder.css';

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
  triggerType?: 'schedule' | 'performance' | 'event';
  listId?: string;
  segmentId?: string;
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
  templateId?: string;
  htmlContent?: string;
  // Delay config
  delayType?: 'fixed' | 'until_time' | 'until_day';
  delayValue?: number;
  delayUnit?: 'minutes' | 'hours' | 'days' | 'weeks';
  untilTime?: string;
  untilDay?: string;
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
}

// Node type definitions
const NODE_TYPES = {
  trigger: { label: 'Trigger', icon: '⚡', color: '#10b981', description: 'Start the journey' },
  email: { label: 'Send Email', icon: '✉️', color: '#3b82f6', description: 'Send an email' },
  delay: { label: 'Wait', icon: '⏱️', color: '#f59e0b', description: 'Wait before next step' },
  condition: { label: 'Condition', icon: '🔀', color: '#8b5cf6', description: 'Branch based on behavior' },
  split: { label: 'A/B Split', icon: '📊', color: '#ec4899', description: 'Split traffic for testing' },
  goal: { label: 'Goal', icon: '🎯', color: '#14b8a6', description: 'Track conversions' },
};

// Main Component
export const JourneyBuilder: React.FC = () => {
  const [journeys, setJourneys] = useState<Journey[]>([]);
  const [activeJourney, setActiveJourney] = useState<Journey | null>(null);
  const [selectedNode, setSelectedNode] = useState<JourneyNode | null>(null);
  const [showNodeConfig, setShowNodeConfig] = useState(false);
  const [, setDraggingNode] = useState<string | null>(null);
  const [connecting, setConnecting] = useState<{ from: string; fromPos: Position } | null>(null);
  const [lists, setLists] = useState<List[]>([]);
  const [profiles, setProfiles] = useState<SendingProfile[]>([]);
  const [loading, setLoading] = useState(true);
  const canvasRef = useRef<HTMLDivElement>(null);
  const [canvasOffset] = useState({ x: 0, y: 0 });
  const [zoom, setZoom] = useState(1);

  // Load data
  useEffect(() => {
    Promise.all([
      fetch('/api/mailing/journeys').then(r => r.json()).catch(() => ({ journeys: [] })),
      fetch('/api/mailing/lists').then(r => r.json()).catch(() => ({ lists: [] })),
      fetch('/api/mailing/sending-profiles').then(r => r.json()).catch(() => ({ profiles: [] })),
    ]).then(([journeyData, listData, profileData]) => {
      setJourneys(journeyData.journeys || []);
      setLists(listData.lists || []);
      setProfiles(profileData.profiles || []);
      setLoading(false);
    });
  }, []);

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
          config: { name: 'Journey Start', triggerType: 'schedule' },
          connections: [],
        },
      ],
      connections: [],
      createdAt: new Date().toISOString(),
    };
    setActiveJourney(newJourney);
    setJourneys([...journeys, newJourney]);
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
      await fetch(`/api/mailing/journeys/${activeJourney.id}`, {
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
      await fetch(`/api/mailing/journeys/${activeJourney.id}/activate`, { method: 'POST' });
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
            fill="#ef4444" 
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
          <span className="node-icon">{nodeType.icon}</span>
          <span className="node-type">{nodeType.label}</span>
        </div>
        <div className="node-body">
          <div className="node-name">{node.config.name || nodeType.label}</div>
          {node.type === 'trigger' && node.config.triggerType && (
            <div className="node-detail">
              {node.config.triggerType === 'schedule' ? '📅 Scheduled' : '📈 Performance-based'}
            </div>
          )}
          {node.type === 'email' && node.config.subject && (
            <div className="node-detail">{node.config.subject}</div>
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
            <span style={{ color: nodeType.color }}>{nodeType.icon}</span>
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
                  value={selectedNode.config.sendingProfileId || ''}
                  onChange={(e) => updateNodeConfig({ sendingProfileId: e.target.value })}
                >
                  <option value="">Use default profile</option>
                  {profiles.map((profile) => (
                    <option key={profile.id} value={profile.id}>
                      {profile.name} ({profile.vendor_type})
                    </option>
                  ))}
                </select>
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
                    value={selectedNode.config.fromName || ''}
                    onChange={(e) => updateNodeConfig({ fromName: e.target.value })}
                    placeholder="Sender name"
                  />
                </div>
                <div className="config-group">
                  <label>From Email</label>
                  <input
                    type="email"
                    value={selectedNode.config.fromEmail || ''}
                    onChange={(e) => updateNodeConfig({ fromEmail: e.target.value })}
                    placeholder="sender@example.com"
                  />
                </div>
              </div>
              <div className="config-group">
                <label>Email Content</label>
                <textarea
                  value={selectedNode.config.htmlContent || ''}
                  onChange={(e) => updateNodeConfig({ htmlContent: e.target.value })}
                  placeholder="<html>...</html>"
                  rows={6}
                />
                <small>Use the visual editor in Campaigns for full design capabilities</small>
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
                <div className="config-group">
                  <label>Wait until</label>
                  <input
                    type="time"
                    value={selectedNode.config.untilTime || '09:00'}
                    onChange={(e) => updateNodeConfig({ untilTime: e.target.value })}
                  />
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
        <button className="create-btn" onClick={createJourney}>
          + Create Journey
        </button>
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
          <button className="save-btn" onClick={saveJourney}>
            💾 Save
          </button>
          {activeJourney?.status === 'draft' && (
            <button className="activate-btn" onClick={activateJourney}>
              ▶️ Activate
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
                <span className="palette-icon">{info.icon}</span>
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

  return (
    <div className="journey-builder">
      {activeJourney ? renderCanvas() : renderJourneyList()}
    </div>
  );
};

export default JourneyBuilder;
