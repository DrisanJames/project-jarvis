import React, { memo } from 'react';
import { Handle, Position, NodeProps } from 'reactflow';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faGlobe, faArrowUp, faArrowDown, faMinus } from '@fortawesome/free-solid-svg-icons';

export interface ISPNodeData {
  id: string;
  name: string;
  volumeToday: number;
  totalClicks: number;
  deliveryRate: number;
  openRate: number;
  clickRate: number;
  status: 'healthy' | 'warning' | 'critical';
}

const formatVolume = (volume: number): string => {
  if (volume >= 1000000) {
    return `${(volume / 1000000).toFixed(1)}M`;
  }
  if (volume >= 1000) {
    return `${(volume / 1000).toFixed(0)}K`;
  }
  return volume.toLocaleString();
};

const formatPercent = (rate: number): string => {
  return `${(rate * 100).toFixed(1)}%`;
};

const formatCTR = (rate: number): string => {
  return `${(rate * 100).toFixed(2)}%`;
};

const getStatusColor = (status: string): string => {
  switch (status) {
    case 'healthy':
      return '#22c55e'; // green
    case 'warning':
      return '#eab308'; // yellow
    case 'critical':
      return '#ef4444'; // red
    default:
      return '#6b7280'; // gray
  }
};

const getStatusIcon = (status: string) => {
  switch (status) {
    case 'healthy':
      return <FontAwesomeIcon icon={faArrowUp} />;
    case 'warning':
      return <FontAwesomeIcon icon={faMinus} />;
    case 'critical':
      return <FontAwesomeIcon icon={faArrowDown} />;
    default:
      return null;
  }
};

const ISPNode: React.FC<NodeProps<ISPNodeData>> = ({ data }) => {
  const statusColor = getStatusColor(data.status);

  return (
    <div
      style={{
        background: '#1e1e2e',
        border: `2px solid ${statusColor}`,
        borderRadius: '12px',
        padding: '12px 16px',
        minWidth: '160px',
        boxShadow: '0 4px 12px rgba(0, 0, 0, 0.3)',
        color: '#ffffff',
      }}
    >
      {/* ISP Name and Icon */}
      <div style={{ display: 'flex', alignItems: 'center', gap: '8px', marginBottom: '8px' }}>
        <FontAwesomeIcon icon={faGlobe} style={{ color: statusColor }} />
        <span style={{ fontWeight: 600, fontSize: '0.9rem' }}>{data.name}</span>
        <span style={{ marginLeft: 'auto', color: statusColor }}>
          {getStatusIcon(data.status)}
        </span>
      </div>

      {/* Volume - Main Metric */}
      <div style={{ 
        fontSize: '1.5rem', 
        fontWeight: 700, 
        marginBottom: '8px',
      }}>
        {formatVolume(data.volumeToday)}
      </div>

      {/* Sub-metrics */}
      <div style={{ display: 'flex', gap: '10px', fontSize: '0.7rem', color: '#9ca3af' }}>
        <span title="Delivery Rate">Del: {formatPercent(data.deliveryRate)}</span>
        <span title="Open Rate">Open: {formatPercent(data.openRate)}</span>
        <span title="Click-Through Rate" style={{ color: '#60a5fa' }}>CTR: {formatCTR(data.clickRate)}</span>
      </div>
      
      {/* Total Clicks */}
      <div style={{ marginTop: '6px', fontSize: '0.7rem', color: '#60a5fa' }}>
        <span title="Total Clicks">Clicks: {data.totalClicks.toLocaleString()}</span>
      </div>

      {/* Source Handle (right side) */}
      <Handle
        type="source"
        position={Position.Right}
        id="source"
        style={{
          width: '12px',
          height: '12px',
          background: statusColor,
          border: '2px solid #1e1e2e',
        }}
      />
    </div>
  );
};

export default memo(ISPNode);
