import React, { useState, useEffect, useCallback } from 'react';

interface SegmentDetailProps {
  segmentId: string;
  onBack?: () => void;
}

interface Segment {
  id: string;
  name: string;
  description?: string;
  segment_type: string;
  subscriber_count: number;
  last_calculated_at?: string;
  status: string;
  calculation_mode: string;
  created_at: string;
}

interface SegmentConditions {
  logic_operator: 'AND' | 'OR';
  is_negated: boolean;
  conditions: Array<{
    condition_type: string;
    field: string;
    operator: string;
    value?: string;
    event_name?: string;
  }>;
  groups: Array<any>;
}

interface SubscriberSample {
  id: string;
  email: string;
  first_name?: string;
  last_name?: string;
  engagement_score: number;
}

export const SegmentDetail: React.FC<SegmentDetailProps> = ({ segmentId, onBack }) => {
  const [segment, setSegment] = useState<Segment | null>(null);
  const [conditions, setConditions] = useState<SegmentConditions | null>(null);
  const [subscribers, setSubscribers] = useState<SubscriberSample[]>([]);
  const [loading, setLoading] = useState(true);
  const [countLoading, setCountLoading] = useState(false);
  const [subscribersLoading, setSubscribersLoading] = useState(false);
  const [, setHasMore] = useState(false);
  const [, setOffset] = useState(0);

  // Load segment details
  useEffect(() => {
    const loadSegment = async () => {
      try {
        const response = await fetch(`/api/mailing/v2/segments/${segmentId}`);
        if (response.ok) {
          const data = await response.json();
          setSegment(data.segment);
          setConditions(data.conditions);
        }
      } catch (error) {
        console.error('Failed to load segment:', error);
      }
      setLoading(false);
    };
    loadSegment();
  }, [segmentId]);

  // Load subscribers
  const loadSubscribers = useCallback(async (newOffset = 0) => {
    setSubscribersLoading(true);
    try {
      const response = await fetch(
        `/api/mailing/v2/segments/${segmentId}/subscribers?limit=20&offset=${newOffset}`
      );
      if (response.ok) {
        const data = await response.json();
        if (newOffset === 0) {
          setSubscribers(data.subscriber_ids || []);
        } else {
          setSubscribers(prev => [...prev, ...(data.subscriber_ids || [])]);
        }
        setHasMore(data.has_more);
        setOffset(newOffset + 20);
      }
    } catch (error) {
      console.error('Failed to load subscribers:', error);
    }
    setSubscribersLoading(false);
  }, [segmentId]);

  useEffect(() => {
    loadSubscribers(0);
  }, [loadSubscribers]);

  // Refresh count
  const refreshCount = async () => {
    setCountLoading(true);
    try {
      const response = await fetch(`/api/mailing/v2/segments/${segmentId}/count`);
      if (response.ok) {
        const data = await response.json();
        setSegment(prev => prev ? { ...prev, subscriber_count: data.count } : null);
      }
    } catch (error) {
      console.error('Failed to refresh count:', error);
    }
    setCountLoading(false);
  };

  // Create snapshot
  const createSnapshot = async () => {
    try {
      const response = await fetch(`/api/mailing/v2/segments/${segmentId}/snapshot`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ purpose: 'manual' }),
      });
      if (response.ok) {
        alert('Snapshot created successfully');
      }
    } catch (error) {
      console.error('Failed to create snapshot:', error);
    }
  };

  const renderCondition = (cond: any, index: number) => {
    const operatorLabels: Record<string, string> = {
      equals: 'is',
      not_equals: 'is not',
      contains: 'contains',
      not_contains: 'does not contain',
      gt: '>',
      gte: '>=',
      lt: '<',
      lte: '<=',
      in_last_days: 'in last',
      more_than_days_ago: 'more than',
      event_count_gte: 'at least',
      event_in_last_days: 'in last',
    };

    return (
      <div key={index} className="flex items-center gap-2 p-2 bg-gray-50 rounded text-sm">
        <span className="font-medium text-gray-700">
          {cond.condition_type === 'event' ? cond.event_name : cond.field}
        </span>
        <span className="text-gray-500">{operatorLabels[cond.operator] || cond.operator}</span>
        {cond.value && (
          <span className="font-medium text-blue-600">
            {cond.value}
            {['in_last_days', 'more_than_days_ago', 'event_in_last_days'].includes(cond.operator) && ' days'}
            {cond.operator === 'event_count_gte' && ' times'}
          </span>
        )}
      </div>
    );
  };

  if (loading) {
    return (
      <div className="flex items-center justify-center py-12">
        <div className="animate-spin w-8 h-8 border-4 border-blue-500 border-t-transparent rounded-full"></div>
      </div>
    );
  }

  if (!segment) {
    return (
      <div className="p-6 text-center">
        <p className="text-gray-500">Segment not found</p>
      </div>
    );
  }

  return (
    <div className="p-6">
      {/* Header */}
      <div className="flex items-center gap-4 mb-6">
        {onBack && (
          <button
            onClick={onBack}
            className="p-2 hover:bg-gray-100 rounded-lg transition-colors"
          >
            <svg className="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M15 19l-7-7 7-7" />
            </svg>
          </button>
        )}
        <div className="flex-1">
          <h1 className="text-2xl font-bold text-gray-900">{segment.name}</h1>
          {segment.description && (
            <p className="text-gray-600">{segment.description}</p>
          )}
        </div>
        <div className="flex items-center gap-2">
          <button
            onClick={createSnapshot}
            className="px-4 py-2 bg-purple-100 text-purple-700 font-medium rounded-lg hover:bg-purple-200 transition-colors"
          >
            Create Snapshot
          </button>
        </div>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6">
        {/* Left Column - Stats & Info */}
        <div className="space-y-6">
          {/* Count Card */}
          <div className="bg-gradient-to-br from-blue-500 to-purple-600 rounded-xl p-6 text-white">
            <div className="flex items-center justify-between mb-2">
              <span className="text-blue-100">Audience Size</span>
              <button
                onClick={refreshCount}
                disabled={countLoading}
                className="p-1.5 hover:bg-white/10 rounded-lg transition-colors"
                title="Refresh count"
              >
                <svg className={`w-4 h-4 ${countLoading ? 'animate-spin' : ''}`} fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" />
                </svg>
              </button>
            </div>
            <div className="text-4xl font-bold">
              {segment.subscriber_count.toLocaleString()}
            </div>
            <div className="text-sm text-blue-100 mt-1">
              contacts match this segment
            </div>
            {segment.last_calculated_at && (
              <div className="text-xs text-blue-200 mt-3">
                Last calculated: {new Date(segment.last_calculated_at).toLocaleString()}
              </div>
            )}
          </div>

          {/* Segment Info */}
          <div className="bg-white rounded-xl border border-gray-200 p-4">
            <h3 className="font-medium text-gray-900 mb-3">Segment Details</h3>
            <dl className="space-y-2 text-sm">
              <div className="flex justify-between">
                <dt className="text-gray-500">Type</dt>
                <dd className="font-medium text-gray-900">{segment.segment_type}</dd>
              </div>
              <div className="flex justify-between">
                <dt className="text-gray-500">Mode</dt>
                <dd className="font-medium text-gray-900">{segment.calculation_mode || 'batch'}</dd>
              </div>
              <div className="flex justify-between">
                <dt className="text-gray-500">Status</dt>
                <dd>
                  <span className={`px-2 py-0.5 rounded-full text-xs font-medium ${
                    segment.status === 'active' ? 'bg-green-100 text-green-800' : 'bg-gray-100 text-gray-800'
                  }`}>
                    {segment.status}
                  </span>
                </dd>
              </div>
              <div className="flex justify-between">
                <dt className="text-gray-500">Created</dt>
                <dd className="font-medium text-gray-900">
                  {new Date(segment.created_at).toLocaleDateString()}
                </dd>
              </div>
            </dl>
          </div>
        </div>

        {/* Middle Column - Conditions */}
        <div className="bg-white rounded-xl border border-gray-200 p-4">
          <h3 className="font-medium text-gray-900 mb-3">Conditions</h3>
          {conditions ? (
            <div className="space-y-2">
              <div className="flex items-center gap-2 mb-3">
                <span className={`px-2 py-0.5 text-xs font-semibold rounded ${
                  conditions.logic_operator === 'AND' 
                    ? 'bg-blue-100 text-blue-700' 
                    : 'bg-orange-100 text-orange-700'
                }`}>
                  {conditions.logic_operator}
                </span>
                <span className="text-sm text-gray-500">
                  Match {conditions.logic_operator === 'AND' ? 'all' : 'any'} conditions
                </span>
              </div>
              {conditions.conditions.map((cond, i) => renderCondition(cond, i))}
              {conditions.groups?.map((group, gi) => (
                <div key={gi} className="ml-4 p-2 border-l-2 border-gray-200">
                  <div className="text-xs text-gray-500 mb-1">
                    {group.logic_operator} Group
                  </div>
                  {group.conditions?.map((cond: any, ci: number) => renderCondition(cond, ci))}
                </div>
              ))}
            </div>
          ) : (
            <p className="text-sm text-gray-500">No conditions defined</p>
          )}
        </div>

        {/* Right Column - Sample Subscribers */}
        <div className="bg-white rounded-xl border border-gray-200 p-4">
          <h3 className="font-medium text-gray-900 mb-3">Sample Contacts</h3>
          {subscribersLoading && subscribers.length === 0 ? (
            <div className="flex items-center justify-center py-8">
              <div className="animate-spin w-6 h-6 border-2 border-blue-500 border-t-transparent rounded-full"></div>
            </div>
          ) : subscribers.length === 0 ? (
            <p className="text-sm text-gray-500">No contacts in this segment</p>
          ) : (
            <div className="space-y-2">
              {subscribers.slice(0, 10).map((sub: any) => (
                <div key={sub} className="flex items-center justify-between p-2 bg-gray-50 rounded">
                  <div className="text-sm text-gray-700 truncate">
                    {typeof sub === 'string' ? sub.slice(0, 8) + '...' : sub.email || sub.id}
                  </div>
                </div>
              ))}
              {segment.subscriber_count > 10 && (
                <div className="text-center pt-2">
                  <span className="text-sm text-gray-500">
                    +{(segment.subscriber_count - 10).toLocaleString()} more contacts
                  </span>
                </div>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  );
};

export default SegmentDetail;
