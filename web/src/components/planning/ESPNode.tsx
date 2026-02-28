import React, { memo } from 'react';
import { Handle, Position, NodeProps } from 'reactflow';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faServer, faInfinity, faExclamationTriangle } from '@fortawesome/free-solid-svg-icons';

export interface ESPNodeData {
  id: string;
  name: string;
  monthlyAllocation: number;
  dailyAllocation: number;
  usedToday: number;
  usedMTD: number;
  remainingToday: number;
  remainingMTD: number;
  dailyAverage: number;
  monthlyFee: number;
  overageRate: number;
  isPayAsYouGo: boolean;
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

const formatCurrency = (amount: number): string => {
  return new Intl.NumberFormat('en-US', {
    style: 'currency',
    currency: 'USD',
    minimumFractionDigits: 0,
    maximumFractionDigits: 0,
  }).format(amount);
};

const getUsagePercent = (used: number, allocation: number): number => {
  if (allocation <= 0) return 0;
  return Math.min((used / allocation) * 100, 100);
};

const getCapacityColor = (percent: number, isPayAsYouGo: boolean): string => {
  if (isPayAsYouGo) return '#3b82f6'; // blue
  if (percent >= 90) return '#ef4444'; // red
  if (percent >= 70) return '#eab308'; // yellow
  return '#22c55e'; // green
};

const ESPNode: React.FC<NodeProps<ESPNodeData>> = ({ data }) => {
  // Use daily metrics for the progress bar since ISPs show daily volume
  const dailyUsagePercent = getUsagePercent(data.usedToday, data.dailyAllocation);
  const capacityColor = getCapacityColor(dailyUsagePercent, data.isPayAsYouGo);
  const isOverDailyLimit = data.remainingToday < 0 && !data.isPayAsYouGo;

  return (
    <div
      style={{
        background: '#1e1e2e',
        border: `2px solid ${capacityColor}`,
        borderRadius: '12px',
        padding: '12px 16px',
        minWidth: '180px',
        boxShadow: '0 4px 12px rgba(0, 0, 0, 0.3)',
        color: '#ffffff',
        position: 'relative',
      }}
    >
      {/* Target Handle (left side) */}
      <Handle
        type="target"
        position={Position.Left}
        id="target"
        style={{
          width: '12px',
          height: '12px',
          background: capacityColor,
          border: '2px solid #1e1e2e',
        }}
      />

      {/* ESP Name and Icon */}
      <div style={{ display: 'flex', alignItems: 'center', gap: '8px', marginBottom: '8px' }}>
        <FontAwesomeIcon icon={faServer} style={{ color: capacityColor }} />
        <span style={{ fontWeight: 600, fontSize: '0.9rem' }}>{data.name}</span>
        {isOverDailyLimit && (
          <FontAwesomeIcon icon={faExclamationTriangle} style={{ color: '#ef4444', marginLeft: 'auto', fontSize: '14px' }} />
        )}
      </div>

      {/* Daily Allocation */}
      <div style={{ fontSize: '0.7rem', color: '#9ca3af', marginBottom: '4px' }}>
        {data.isPayAsYouGo ? (
          <span style={{ display: 'inline-flex', alignItems: 'center', gap: '2px' }}>
            <FontAwesomeIcon icon={faInfinity} /> Pay-as-you-go
          </span>
        ) : (
          `Daily limit: ${formatVolume(data.dailyAllocation)}`
        )}
      </div>

      {/* Usage Bar (Daily) */}
      {!data.isPayAsYouGo && (
        <div style={{
          width: '100%',
          height: '8px',
          background: '#374151',
          borderRadius: '4px',
          overflow: 'hidden',
          marginBottom: '8px',
        }}>
          <div style={{
            width: `${dailyUsagePercent}%`,
            height: '100%',
            background: capacityColor,
            transition: 'width 0.3s ease',
          }} />
        </div>
      )}

      {/* Used Today / Remaining Today */}
      <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: '0.75rem', marginBottom: '8px' }}>
        <div>
          <div style={{ color: '#9ca3af' }}>Used Today</div>
          <div style={{ fontWeight: 600 }}>{formatVolume(data.usedToday)}</div>
        </div>
        <div style={{ textAlign: 'right' }}>
          <div style={{ color: '#9ca3af' }}>
            {data.isPayAsYouGo ? 'Daily Avg' : 'Remaining'}
          </div>
          <div style={{ 
            fontWeight: 600, 
            color: data.isPayAsYouGo 
              ? '#ffffff' 
              : (data.remainingToday < 0 ? '#ef4444' : '#22c55e')
          }}>
            {data.isPayAsYouGo 
              ? formatVolume(data.dailyAverage)
              : (data.remainingToday < 0 
                  ? `${formatVolume(Math.abs(data.remainingToday))} over` 
                  : formatVolume(data.remainingToday))}
          </div>
        </div>
      </div>

      {/* Cost Info */}
      <div style={{ 
        display: 'flex', 
        justifyContent: 'space-between', 
        fontSize: '0.65rem', 
        color: '#9ca3af',
        borderTop: '1px solid #374151',
        paddingTop: '8px',
      }}>
        <span>Fee: {formatCurrency(data.monthlyFee)}</span>
        <span>Overage: ${data.overageRate.toFixed(4)}/1K</span>
      </div>

      {/* Percentage Badge (Daily) */}
      {!data.isPayAsYouGo && (
        <div style={{
          position: 'absolute',
          top: '-8px',
          right: '-8px',
          background: capacityColor,
          color: 'white',
          fontSize: '0.65rem',
          fontWeight: 600,
          padding: '2px 6px',
          borderRadius: '8px',
        }}>
          {dailyUsagePercent.toFixed(0)}%
        </div>
      )}
    </div>
  );
};

export default memo(ESPNode);
