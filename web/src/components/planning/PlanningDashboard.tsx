import React, { useCallback, useEffect, useMemo, useState } from 'react';
import ReactFlow, {
  Node,
  Edge,
  Connection,
  useNodesState,
  useEdgesState,
  Controls,
  Background,
  BackgroundVariant,
  MarkerType,
  ConnectionLineType,
  ReactFlowProvider,
} from 'reactflow';
import 'reactflow/dist/style.css';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faSync, faBolt, faExclamationTriangle, faInfoCircle, faTrash, faSave, faFolderOpen, faCheck, faTimes } from '@fortawesome/free-solid-svg-icons';
import ISPNode, { ISPNodeData } from './ISPNode';
import ESPNode, { ESPNodeData } from './ESPNode';
import { Card, CardBody, Loading } from '../common';
import { useDateFilter } from '../../context/DateFilterContext';

// Register custom node types
const nodeTypes = {
  ispNode: ISPNode,
  espNode: ESPNode,
};

// API Types
interface PlanningISP {
  id: string;
  name: string;
  volume_today: number;
  total_clicks: number;
  delivery_rate: number;
  open_rate: number;
  click_rate: number;
  status: string;
}

interface PlanningESP {
  id: string;
  name: string;
  monthly_allocation: number;
  daily_allocation: number;
  used_today: number;
  used_mtd: number;
  remaining_today: number;
  remaining_mtd: number;
  daily_average: number;
  monthly_fee: number;
  overage_rate: number;
  is_pay_as_you_go: boolean;
}

interface PlanningDashboardData {
  timestamp: string;
  isps: PlanningISP[];
  esps: PlanningESP[];
}

// Routing Plan Types
interface RoutingRule {
  isp_id: string;
  isp_name: string;
  esp_id: string;
  esp_name: string;
}

interface RoutingPlan {
  id: string;
  name: string;
  description?: string;
  routes: RoutingRule[];
  created_at: string;
  updated_at: string;
  is_active: boolean;
}

const formatVolume = (volume: number): string => {
  if (volume >= 1000000000) {
    return `${(volume / 1000000000).toFixed(1)}B`;
  }
  if (volume >= 1000000) {
    return `${(volume / 1000000).toFixed(1)}M`;
  }
  if (volume >= 1000) {
    return `${(volume / 1000).toFixed(0)}K`;
  }
  return volume.toLocaleString();
};

// Track planned routing: ISP ID -> ESP ID
type PlannedRoutes = Map<string, string>;

// Inner component that uses React Flow hooks
const PlanningFlowInner: React.FC = () => {
  const [data, setData] = useState<PlanningDashboardData | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [nodes, setNodes, onNodesChange] = useNodesState([]);
  const [edges, setEdges, onEdgesChange] = useEdgesState([]);
  
  // Track planned routes (ISP -> ESP mapping)
  const [plannedRoutes, setPlannedRoutes] = useState<PlannedRoutes>(new Map());
  
  // Routing plans state
  const [savedPlans, setSavedPlans] = useState<RoutingPlan[]>([]);
  const [currentPlanId, setCurrentPlanId] = useState<string | null>(null);
  const [showSaveModal, setShowSaveModal] = useState(false);
  const [showLoadModal, setShowLoadModal] = useState(false);
  const [savePlanName, setSavePlanName] = useState('');
  const [savePlanDescription, setSavePlanDescription] = useState('');
  const [savingPlan, setSavingPlan] = useState(false);

  // Use global date filter
  const { dateRange } = useDateFilter();

  // Fetch planning data
  const fetchData = useCallback(async () => {
    try {
      setLoading(true);
      setError(null);
      const response = await fetch(`/api/planning/dashboard?start_date=${dateRange.startDate}&end_date=${dateRange.endDate}&range_type=${dateRange.type}`);
      if (!response.ok) {
        throw new Error(`HTTP ${response.status}`);
      }
      const result = await response.json();
      setData(result);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to fetch planning data');
    } finally {
      setLoading(false);
    }
  }, [dateRange.startDate, dateRange.endDate, dateRange.type]);

  // Fetch saved routing plans
  const fetchPlans = useCallback(async () => {
    try {
      const response = await fetch('/api/planning/plans');
      if (response.ok) {
        const result = await response.json();
        setSavedPlans(result.plans || []);
      }
    } catch (err) {
      console.error('Failed to fetch routing plans:', err);
    }
  }, []);

  // Load active plan on mount
  const loadActivePlan = useCallback(async () => {
    try {
      const response = await fetch('/api/planning/plans/active');
      if (response.ok) {
        const result = await response.json();
        if (result.plan) {
          // Convert plan routes to Map
          const routes = new Map<string, string>();
          result.plan.routes?.forEach((route: RoutingRule) => {
            routes.set(route.isp_id, route.esp_id);
          });
          setPlannedRoutes(routes);
          setCurrentPlanId(result.plan.id);
        }
      }
    } catch (err) {
      console.error('Failed to load active plan:', err);
    }
  }, []);

  // Save current routes as a plan
  const savePlan = useCallback(async (name: string, description: string, makeActive: boolean = true) => {
    if (!data || plannedRoutes.size === 0) return;
    
    setSavingPlan(true);
    try {
      const routes: RoutingRule[] = [];
      plannedRoutes.forEach((espId, ispId) => {
        const isp = data.isps.find(i => i.id === ispId);
        const esp = data.esps.find(e => e.id === espId);
        if (isp && esp) {
          routes.push({
            isp_id: ispId,
            isp_name: isp.name,
            esp_id: espId,
            esp_name: esp.name,
          });
        }
      });

      const planData = {
        id: currentPlanId || undefined,
        name,
        description,
        routes,
        is_active: makeActive,
      };

      const response = await fetch('/api/planning/plans', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(planData),
      });

      if (response.ok) {
        const result = await response.json();
        setCurrentPlanId(result.plan?.id || null);
        await fetchPlans();
        setShowSaveModal(false);
        setSavePlanName('');
        setSavePlanDescription('');
      }
    } catch (err) {
      console.error('Failed to save plan:', err);
    } finally {
      setSavingPlan(false);
    }
  }, [data, plannedRoutes, currentPlanId, fetchPlans]);

  // Load a specific plan
  const loadPlan = useCallback(async (plan: RoutingPlan) => {
    const routes = new Map<string, string>();
    plan.routes?.forEach(route => {
      routes.set(route.isp_id, route.esp_id);
    });
    setPlannedRoutes(routes);
    setCurrentPlanId(plan.id);
    setShowLoadModal(false);

    // Set as active
    try {
      await fetch(`/api/planning/plans/${plan.id}/activate`, { method: 'POST' });
    } catch (err) {
      console.error('Failed to set active plan:', err);
    }
  }, []);

  // Delete a plan
  const deletePlan = useCallback(async (planId: string) => {
    try {
      const response = await fetch(`/api/planning/plans/${planId}`, { method: 'DELETE' });
      if (response.ok) {
        await fetchPlans();
        if (currentPlanId === planId) {
          setCurrentPlanId(null);
        }
      }
    } catch (err) {
      console.error('Failed to delete plan:', err);
    }
  }, [currentPlanId, fetchPlans]);

  useEffect(() => {
    fetchData();
    fetchPlans();
    loadActivePlan();
  }, [fetchData, fetchPlans, loadActivePlan]);

  // Calculate ESP usage based on planned routes
  const espUsage = useMemo(() => {
    const usage = new Map<string, number>();
    
    if (!data) return usage;
    
    // Initialize all ESPs with 0
    data.esps.forEach(esp => {
      usage.set(esp.id, 0);
    });
    
    // Sum ISP volumes for each ESP based on planned routes
    plannedRoutes.forEach((espId, ispId) => {
      const isp = data.isps.find(i => i.id === ispId);
      if (isp) {
        const currentUsage = usage.get(espId) || 0;
        usage.set(espId, currentUsage + isp.volume_today);
      }
    });
    
    return usage;
  }, [data, plannedRoutes]);

  // Build nodes when data changes
  useEffect(() => {
    if (!data) return;

    const ispNodes: Node<ISPNodeData>[] = data.isps.map((isp, index) => ({
      id: `isp-${isp.id}`,
      type: 'ispNode',
      position: { x: 50, y: 50 + index * 140 },
      data: {
        id: isp.id,
        name: isp.name,
        volumeToday: isp.volume_today,
        totalClicks: isp.total_clicks || 0,
        deliveryRate: isp.delivery_rate,
        openRate: isp.open_rate,
        clickRate: isp.click_rate,
        status: isp.status as 'healthy' | 'warning' | 'critical',
      },
    }));

    // Calculate ESP data with dynamic usage from planned routes
    const espNodes: Node<ESPNodeData>[] = data.esps.map((esp, index) => {
      const usedFromPlanning = espUsage.get(esp.id) || 0;
      const remainingFromPlanning = esp.is_pay_as_you_go ? -1 : (esp.daily_allocation - usedFromPlanning);
      
      return {
        id: `esp-${esp.id}`,
        type: 'espNode',
        position: { x: 550, y: 80 + index * 220 },
        data: {
          id: esp.id,
          name: esp.name,
          monthlyAllocation: esp.monthly_allocation,
          dailyAllocation: esp.daily_allocation,
          usedToday: usedFromPlanning,  // Dynamic based on planning
          usedMTD: esp.used_mtd,
          remainingToday: remainingFromPlanning,  // Dynamic based on planning
          remainingMTD: esp.remaining_mtd,
          dailyAverage: esp.daily_average,
          monthlyFee: esp.monthly_fee,
          overageRate: esp.overage_rate,
          isPayAsYouGo: esp.is_pay_as_you_go,
        },
      };
    });

    setNodes([...ispNodes, ...espNodes]);
  }, [data, espUsage, setNodes]);

  // Build edges from planned routes
  useEffect(() => {
    if (!data) return;
    
    const routeEdges: Edge[] = [];
    
    plannedRoutes.forEach((espId, ispId) => {
      const isp = data.isps.find(i => i.id === ispId);
      if (!isp) return;
      
      routeEdges.push({
        id: `edge-${ispId}-${espId}`,
        source: `isp-${ispId}`,
        target: `esp-${espId}`,
        sourceHandle: 'source',
        targetHandle: 'target',
        type: 'smoothstep',
        animated: true,
        style: { 
          stroke: '#3b82f6',
          strokeWidth: 2,
        },
        markerEnd: {
          type: MarkerType.ArrowClosed,
          color: '#3b82f6',
        },
        label: formatVolume(isp.volume_today),
        labelStyle: { 
          fontSize: '11px', 
          fontWeight: 600,
          fill: '#ffffff',
        },
        labelBgStyle: { 
          fill: '#3b82f6', 
          fillOpacity: 1,
        },
        labelBgPadding: [6, 4] as [number, number],
        labelBgBorderRadius: 4,
      });
    });
    
    setEdges(routeEdges);
  }, [data, plannedRoutes, setEdges]);

  // Handle new connections
  const onConnect = useCallback(
    (connection: Connection) => {
      if (!connection.source || !connection.target) return;
      
      // Extract ISP and ESP IDs
      const ispId = connection.source.replace('isp-', '');
      const espId = connection.target.replace('esp-', '');
      
      // Update planned routes
      setPlannedRoutes(prev => {
        const newRoutes = new Map(prev);
        newRoutes.set(ispId, espId);
        return newRoutes;
      });
    },
    []
  );

  // Handle edge deletion
  const onEdgesDelete = useCallback(
    (deletedEdges: Edge[]) => {
      setPlannedRoutes(prev => {
        const newRoutes = new Map(prev);
        deletedEdges.forEach(edge => {
          const ispId = edge.source?.replace('isp-', '');
          if (ispId) {
            newRoutes.delete(ispId);
          }
        });
        return newRoutes;
      });
    },
    []
  );

  // Clear all routes
  const clearAllRoutes = useCallback(() => {
    setPlannedRoutes(new Map());
  }, []);

  // Calculate summary stats
  const summaryStats = useMemo(() => {
    if (!data) return null;

    const totalISPVolume = data.isps.reduce((sum, isp) => sum + isp.volume_today, 0);
    const routedVolume = Array.from(plannedRoutes.keys()).reduce((sum, ispId) => {
      const isp = data.isps.find(i => i.id === ispId);
      return sum + (isp?.volume_today || 0);
    }, 0);
    const unroutedVolume = totalISPVolume - routedVolume;
    
    const totalDailyAllocation = data.esps
      .filter(esp => !esp.is_pay_as_you_go)
      .reduce((sum, esp) => sum + esp.daily_allocation, 0);
    
    const totalPlannedUsage = Array.from(espUsage.values()).reduce((sum, usage) => sum + usage, 0);

    return {
      totalISPVolume,
      routedVolume,
      unroutedVolume,
      totalDailyAllocation,
      totalPlannedUsage,
      routedCount: plannedRoutes.size,
      ispCount: data.isps.length,
      espCount: data.esps.length,
    };
  }, [data, plannedRoutes, espUsage]);

  if (loading && !data) {
    return <Loading message="Loading volume planning data..." />;
  }

  if (error) {
    return (
      <Card>
        <CardBody>
          <div style={{ textAlign: 'center', padding: '2rem' }}>
            <FontAwesomeIcon icon={faExclamationTriangle} style={{ color: '#ef4444', marginBottom: '1rem', fontSize: '48px' }} />
            <h3>Error loading planning data</h3>
            <p style={{ color: '#9ca3af' }}>{error}</p>
            <button
              onClick={fetchData}
              style={{
                marginTop: '1rem',
                padding: '0.5rem 1rem',
                background: 'var(--primary-color)',
                color: 'white',
                border: 'none',
                borderRadius: '4px',
                cursor: 'pointer',
              }}
            >
              Retry
            </button>
          </div>
        </CardBody>
      </Card>
    );
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', gap: '1rem' }}>
      {/* Header */}
      <div style={{
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'space-between',
        padding: '0.5rem 0',
      }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
          <FontAwesomeIcon icon={faBolt} style={{ color: '#a855f7', fontSize: '24px' }} />
          <h2 style={{ margin: 0 }}>Volume Planning</h2>
          <span style={{
            padding: '0.25rem 0.75rem',
            background: 'var(--bg-secondary)',
            borderRadius: '1rem',
            fontSize: '0.75rem',
            color: 'var(--text-muted)',
          }}>
            {summaryStats?.routedCount ?? 0} of {summaryStats?.ispCount ?? 0} ISPs routed
          </span>
          <span style={{
            padding: '0.25rem 0.75rem',
            background: '#3b82f6',
            borderRadius: '1rem',
            fontSize: '0.75rem',
            color: 'white',
            fontWeight: 500,
          }}>
            {dateRange.label}
          </span>
        </div>
        <div style={{ display: 'flex', gap: '0.5rem' }}>
          <button
            onClick={() => setShowLoadModal(true)}
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: '0.5rem',
              padding: '0.5rem 1rem',
              background: '#4f46e5',
              border: 'none',
              borderRadius: '6px',
              color: 'white',
              cursor: 'pointer',
            }}
          >
            <FontAwesomeIcon icon={faFolderOpen} />
            Load Plan
          </button>
          <button
            onClick={() => {
              const currentPlan = savedPlans.find(p => p.id === currentPlanId);
              setSavePlanName(currentPlan?.name || '');
              setSavePlanDescription(currentPlan?.description || '');
              setShowSaveModal(true);
            }}
            disabled={plannedRoutes.size === 0}
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: '0.5rem',
              padding: '0.5rem 1rem',
              background: plannedRoutes.size === 0 ? '#374151' : '#22c55e',
              border: 'none',
              borderRadius: '6px',
              color: 'white',
              cursor: plannedRoutes.size === 0 ? 'not-allowed' : 'pointer',
              opacity: plannedRoutes.size === 0 ? 0.5 : 1,
            }}
          >
            <FontAwesomeIcon icon={faSave} />
            Save Plan
          </button>
          <button
            onClick={clearAllRoutes}
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: '0.5rem',
              padding: '0.5rem 1rem',
              background: '#ef4444',
              border: 'none',
              borderRadius: '6px',
              color: 'white',
              cursor: 'pointer',
            }}
          >
            <FontAwesomeIcon icon={faTrash} />
            Clear Routes
          </button>
          <button
            onClick={fetchData}
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: '0.5rem',
              padding: '0.5rem 1rem',
              background: 'var(--bg-secondary)',
              border: '1px solid var(--border-color)',
              borderRadius: '6px',
              color: 'var(--text-primary)',
              cursor: 'pointer',
            }}
          >
            <FontAwesomeIcon icon={faSync} />
            Refresh Data
          </button>
        </div>
      </div>

      {/* Summary Stats */}
      {summaryStats && (
        <div style={{
          display: 'grid',
          gridTemplateColumns: 'repeat(5, 1fr)',
          gap: '1rem',
        }}>
          <div style={{
            background: 'var(--card-bg)',
            padding: '1rem',
            borderRadius: '8px',
            border: '1px solid var(--border-color)',
          }}>
            <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>Total ISP Volume</div>
            <div style={{ fontSize: '1.25rem', fontWeight: 700 }}>{formatVolume(summaryStats.totalISPVolume)}</div>
          </div>
          <div style={{
            background: 'var(--card-bg)',
            padding: '1rem',
            borderRadius: '8px',
            border: '1px solid var(--border-color)',
          }}>
            <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>Routed Volume</div>
            <div style={{ fontSize: '1.25rem', fontWeight: 700, color: '#22c55e' }}>{formatVolume(summaryStats.routedVolume)}</div>
          </div>
          <div style={{
            background: 'var(--card-bg)',
            padding: '1rem',
            borderRadius: '8px',
            border: summaryStats.unroutedVolume > 0 ? '1px solid #eab308' : '1px solid var(--border-color)',
          }}>
            <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>Unrouted Volume</div>
            <div style={{ fontSize: '1.25rem', fontWeight: 700, color: summaryStats.unroutedVolume > 0 ? '#eab308' : '#22c55e' }}>
              {formatVolume(summaryStats.unroutedVolume)}
            </div>
          </div>
          <div style={{
            background: 'var(--card-bg)',
            padding: '1rem',
            borderRadius: '8px',
            border: '1px solid var(--border-color)',
          }}>
            <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>Daily ESP Capacity</div>
            <div style={{ fontSize: '1.25rem', fontWeight: 700 }}>{formatVolume(summaryStats.totalDailyAllocation)}</div>
          </div>
          <div style={{
            background: 'var(--card-bg)',
            padding: '1rem',
            borderRadius: '8px',
            border: '1px solid var(--border-color)',
          }}>
            <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>Planned Usage</div>
            <div style={{ 
              fontSize: '1.25rem', 
              fontWeight: 700,
              color: summaryStats.totalPlannedUsage > summaryStats.totalDailyAllocation ? '#ef4444' : '#22c55e'
            }}>
              {formatVolume(summaryStats.totalPlannedUsage)}
            </div>
          </div>
        </div>
      )}

      {/* Instructions */}
      <div style={{
        display: 'flex',
        alignItems: 'center',
        gap: '0.5rem',
        padding: '0.75rem 1rem',
        background: '#1e1b4b',
        borderRadius: '8px',
        fontSize: '0.8rem',
        color: '#c4b5fd',
        border: '1px solid #4c1d95',
      }}>
        <FontAwesomeIcon icon={faInfoCircle} />
        <span>
          <strong>Drag from an ISP</strong> (right handle) <strong>to an ESP</strong> (left handle) to route volume. 
          ESP capacity updates dynamically. Click an edge and press <strong>Delete</strong> to remove a route.
        </span>
      </div>

      {/* React Flow Canvas */}
      <div style={{
        width: '100%',
        height: '600px',
        background: '#0f0f1a',
        borderRadius: '12px',
        border: '1px solid var(--border-color)',
        overflow: 'hidden',
      }}>
        <ReactFlow
          nodes={nodes}
          edges={edges}
          onNodesChange={onNodesChange}
          onEdgesChange={onEdgesChange}
          onConnect={onConnect}
          onEdgesDelete={onEdgesDelete}
          nodeTypes={nodeTypes}
          connectionLineType={ConnectionLineType.SmoothStep}
          fitView
          fitViewOptions={{ padding: 0.2 }}
          deleteKeyCode={['Backspace', 'Delete']}
          defaultEdgeOptions={{
            type: 'smoothstep',
            animated: true,
          }}
          style={{ width: '100%', height: '100%' }}
        >
          <Background variant={BackgroundVariant.Dots} gap={20} size={1} color="#2a2a4a" />
          <Controls 
            style={{ 
              background: '#1e1e2e', 
              borderRadius: '8px',
              border: '1px solid #374151',
            }}
          />
        </ReactFlow>
      </div>

      {/* Legend */}
      <div style={{
        display: 'flex',
        gap: '2rem',
        padding: '0.75rem 1rem',
        background: 'var(--bg-secondary)',
        borderRadius: '8px',
        fontSize: '0.75rem',
      }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
          <div style={{ width: '12px', height: '12px', borderRadius: '50%', background: '#22c55e' }} />
          <span>Under 70% capacity</span>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
          <div style={{ width: '12px', height: '12px', borderRadius: '50%', background: '#eab308' }} />
          <span>70-90% capacity</span>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
          <div style={{ width: '12px', height: '12px', borderRadius: '50%', background: '#ef4444' }} />
          <span>Over 90% / Over limit</span>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
          <div style={{ width: '12px', height: '12px', borderRadius: '50%', background: '#3b82f6' }} />
          <span>Pay-as-you-go</span>
        </div>
        {currentPlanId && (
          <div style={{ marginLeft: 'auto', display: 'flex', alignItems: 'center', gap: '0.5rem', color: '#a855f7' }}>
            <FontAwesomeIcon icon={faCheck} />
            <span>Active: {savedPlans.find(p => p.id === currentPlanId)?.name || 'Loaded Plan'}</span>
          </div>
        )}
      </div>

      {/* Save Plan Modal */}
      {showSaveModal && (
        <div style={{
          position: 'fixed',
          top: 0,
          left: 0,
          right: 0,
          bottom: 0,
          background: 'rgba(0,0,0,0.7)',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          zIndex: 1000,
        }}>
          <div style={{
            background: '#1e1e2e',
            borderRadius: '12px',
            padding: '1.5rem',
            width: '400px',
            maxWidth: '90vw',
            border: '1px solid #374151',
          }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1rem' }}>
              <h3 style={{ margin: 0, display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                <FontAwesomeIcon icon={faSave} style={{ color: '#22c55e' }} />
                Save Routing Plan
              </h3>
              <button
                onClick={() => setShowSaveModal(false)}
                style={{ background: 'none', border: 'none', cursor: 'pointer', color: '#9ca3af' }}
              >
                <FontAwesomeIcon icon={faTimes} />
              </button>
            </div>
            
            <div style={{ marginBottom: '1rem' }}>
              <label style={{ display: 'block', marginBottom: '0.5rem', fontSize: '0.875rem', color: '#9ca3af' }}>
                Plan Name *
              </label>
              <input
                type="text"
                value={savePlanName}
                onChange={(e) => setSavePlanName(e.target.value)}
                placeholder="e.g., Yahoo Focus Strategy"
                style={{
                  width: '100%',
                  padding: '0.75rem',
                  borderRadius: '6px',
                  border: '1px solid #374151',
                  background: '#0f0f1a',
                  color: 'white',
                  fontSize: '0.875rem',
                }}
              />
            </div>

            <div style={{ marginBottom: '1.5rem' }}>
              <label style={{ display: 'block', marginBottom: '0.5rem', fontSize: '0.875rem', color: '#9ca3af' }}>
                Description (optional)
              </label>
              <textarea
                value={savePlanDescription}
                onChange={(e) => setSavePlanDescription(e.target.value)}
                placeholder="Describe the routing strategy..."
                rows={3}
                style={{
                  width: '100%',
                  padding: '0.75rem',
                  borderRadius: '6px',
                  border: '1px solid #374151',
                  background: '#0f0f1a',
                  color: 'white',
                  fontSize: '0.875rem',
                  resize: 'vertical',
                }}
              />
            </div>

            <div style={{ display: 'flex', gap: '0.5rem', justifyContent: 'flex-end' }}>
              <button
                onClick={() => setShowSaveModal(false)}
                style={{
                  padding: '0.75rem 1.5rem',
                  borderRadius: '6px',
                  border: '1px solid #374151',
                  background: 'transparent',
                  color: '#9ca3af',
                  cursor: 'pointer',
                }}
              >
                Cancel
              </button>
              <button
                onClick={() => savePlan(savePlanName, savePlanDescription, true)}
                disabled={!savePlanName.trim() || savingPlan}
                style={{
                  padding: '0.75rem 1.5rem',
                  borderRadius: '6px',
                  border: 'none',
                  background: !savePlanName.trim() ? '#374151' : '#22c55e',
                  color: 'white',
                  cursor: !savePlanName.trim() ? 'not-allowed' : 'pointer',
                  opacity: savingPlan ? 0.7 : 1,
                }}
              >
                {savingPlan ? 'Saving...' : (currentPlanId ? 'Update Plan' : 'Save Plan')}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Load Plan Modal */}
      {showLoadModal && (
        <div style={{
          position: 'fixed',
          top: 0,
          left: 0,
          right: 0,
          bottom: 0,
          background: 'rgba(0,0,0,0.7)',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          zIndex: 1000,
        }}>
          <div style={{
            background: '#1e1e2e',
            borderRadius: '12px',
            padding: '1.5rem',
            width: '500px',
            maxWidth: '90vw',
            maxHeight: '80vh',
            overflow: 'auto',
            border: '1px solid #374151',
          }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1rem' }}>
              <h3 style={{ margin: 0, display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                <FontAwesomeIcon icon={faFolderOpen} style={{ color: '#4f46e5' }} />
                Load Routing Plan
              </h3>
              <button
                onClick={() => setShowLoadModal(false)}
                style={{ background: 'none', border: 'none', cursor: 'pointer', color: '#9ca3af' }}
              >
                <FontAwesomeIcon icon={faTimes} />
              </button>
            </div>

            {savedPlans.length === 0 ? (
              <div style={{ textAlign: 'center', padding: '2rem', color: '#9ca3af' }}>
                <FontAwesomeIcon icon={faFolderOpen} style={{ opacity: 0.5, marginBottom: '1rem', fontSize: '48px' }} />
                <p>No saved plans yet.</p>
                <p style={{ fontSize: '0.875rem' }}>Create routes and save them to access later.</p>
              </div>
            ) : (
              <div style={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
                {savedPlans.map(plan => (
                  <div
                    key={plan.id}
                    style={{
                      display: 'flex',
                      alignItems: 'center',
                      gap: '1rem',
                      padding: '1rem',
                      borderRadius: '8px',
                      background: plan.id === currentPlanId ? 'rgba(168, 85, 247, 0.1)' : '#0f0f1a',
                      border: plan.id === currentPlanId ? '1px solid #a855f7' : '1px solid #374151',
                    }}
                  >
                    <div style={{ flex: 1 }}>
                      <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                        <span style={{ fontWeight: 600 }}>{plan.name}</span>
                        {plan.is_active && (
                          <span style={{
                            padding: '0.125rem 0.5rem',
                            background: '#22c55e',
                            borderRadius: '4px',
                            fontSize: '0.625rem',
                            fontWeight: 600,
                          }}>
                            ACTIVE
                          </span>
                        )}
                      </div>
                      {plan.description && (
                        <p style={{ margin: '0.25rem 0 0', fontSize: '0.75rem', color: '#9ca3af' }}>
                          {plan.description}
                        </p>
                      )}
                      <p style={{ margin: '0.25rem 0 0', fontSize: '0.7rem', color: '#6b7280' }}>
                        {plan.routes?.length || 0} routes • Updated {new Date(plan.updated_at).toLocaleDateString()}
                      </p>
                    </div>
                    <div style={{ display: 'flex', gap: '0.5rem' }}>
                      <button
                        onClick={() => loadPlan(plan)}
                        style={{
                          padding: '0.5rem 1rem',
                          borderRadius: '6px',
                          border: 'none',
                          background: '#4f46e5',
                          color: 'white',
                          cursor: 'pointer',
                          fontSize: '0.75rem',
                        }}
                      >
                        Load
                      </button>
                      <button
                        onClick={() => deletePlan(plan.id)}
                        style={{
                          padding: '0.5rem',
                          borderRadius: '6px',
                          border: '1px solid #ef4444',
                          background: 'transparent',
                          color: '#ef4444',
                          cursor: 'pointer',
                        }}
                      >
                        <FontAwesomeIcon icon={faTrash} />
                      </button>
                    </div>
                  </div>
                ))}
              </div>
            )}

            <div style={{ marginTop: '1.5rem', display: 'flex', justifyContent: 'flex-end' }}>
              <button
                onClick={() => setShowLoadModal(false)}
                style={{
                  padding: '0.75rem 1.5rem',
                  borderRadius: '6px',
                  border: '1px solid #374151',
                  background: 'transparent',
                  color: '#9ca3af',
                  cursor: 'pointer',
                }}
              >
                Close
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
};

// Wrapper component with ReactFlowProvider
export const PlanningDashboard: React.FC = () => {
  return (
    <ReactFlowProvider>
      <PlanningFlowInner />
    </ReactFlowProvider>
  );
};

export default PlanningDashboard;
