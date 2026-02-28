import React, { useState, useEffect } from 'react';
import { SegmentBuilder, ConditionGroupBuilder } from '../components/segments/SegmentBuilder';
import { SegmentDetail } from '../components/segments/SegmentDetail';

interface Segment {
  id: string;
  name: string;
  description?: string;
  segment_type: string;
  subscriber_count: number;
  last_calculated_at?: string;
  status: string;
  created_at: string;
}

type ViewMode = 'list' | 'builder' | 'detail';

const SegmentsPage: React.FC = () => {
  const [segments, setSegments] = useState<Segment[]>([]);
  const [loading, setLoading] = useState(true);
  const [viewMode, setViewMode] = useState<ViewMode>('list');
  const [selectedSegmentId, setSelectedSegmentId] = useState<string | null>(null);
  // Helper to set view mode to builder
  const setShowBuilder = (show: boolean) => setViewMode(show ? 'builder' : 'list');

  // Load segments
  useEffect(() => {
    const loadSegments = async () => {
      try {
        const response = await fetch('/api/mailing/v2/segments');
        if (response.ok) {
          const data = await response.json();
          setSegments(data || []);
        }
      } catch (error) {
        console.error('Failed to load segments:', error);
      }
      setLoading(false);
    };
    loadSegments();
  }, []);

  const handleCreateSegment = async (name: string, conditions: ConditionGroupBuilder) => {
    try {
      const response = await fetch('/api/mailing/v2/segments', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name,
          root_group: conditions,
        }),
      });

      if (response.ok) {
        const newSegment = await response.json();
        setSegments([...segments, newSegment]);
        setShowBuilder(false);
      } else {
        const error = await response.json();
        alert(`Failed to create segment: ${error.error || 'Unknown error'}`);
      }
    } catch (error) {
      console.error('Failed to create segment:', error);
      alert('Failed to create segment');
    }
  };

  const handleDeleteSegment = async (segmentId: string) => {
    if (!confirm('Are you sure you want to delete this segment?')) return;

    try {
      const response = await fetch(`/api/mailing/v2/segments/${segmentId}`, {
        method: 'DELETE',
      });

      if (response.ok) {
        setSegments(segments.filter(s => s.id !== segmentId));
      }
    } catch (error) {
      console.error('Failed to delete segment:', error);
    }
  };

  const handleRecalculate = async (segmentId: string) => {
    try {
      // Use the count endpoint for fast recalculation
      const response = await fetch(`/api/mailing/v2/segments/${segmentId}/count`);

      if (response.ok) {
        const result = await response.json();
        setSegments(segments.map(s => 
          s.id === segmentId 
            ? { ...s, subscriber_count: result.count, last_calculated_at: new Date().toISOString() }
            : s
        ));
      }
    } catch (error) {
      console.error('Failed to recalculate segment:', error);
    }
  };

  const handleRefreshAllCounts = async () => {
    setLoading(true);
    try {
      const updates = await Promise.all(
        segments.map(async (segment) => {
          try {
            const response = await fetch(`/api/mailing/v2/segments/${segment.id}/count`);
            if (response.ok) {
              const result = await response.json();
              return { id: segment.id, count: result.count };
            }
          } catch (e) {
            // Ignore individual failures
          }
          return null;
        })
      );

      const validUpdates = updates.filter(Boolean) as { id: string; count: number }[];
      setSegments(segments.map(s => {
        const update = validUpdates.find(u => u.id === s.id);
        return update ? { ...s, subscriber_count: update.count, last_calculated_at: new Date().toISOString() } : s;
      }));
    } catch (error) {
      console.error('Failed to refresh counts:', error);
    }
    setLoading(false);
  };

  const handleCreateSnapshot = async (segmentId: string) => {
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

  // Show segment detail view
  if (viewMode === 'detail' && selectedSegmentId) {
    return (
      <SegmentDetail 
        segmentId={selectedSegmentId} 
        onBack={() => {
          setViewMode('list');
          setSelectedSegmentId(null);
        }}
      />
    );
  }

  // Show segment builder
  if (viewMode === 'builder') {
    return (
      <div className="p-6">
        <div className="flex items-center justify-between mb-6">
          <h1 className="text-2xl font-bold text-gray-900">Create Segment</h1>
          <button
            onClick={() => setViewMode('list')}
            className="px-4 py-2 text-gray-600 hover:text-gray-900"
          >
            Cancel
          </button>
        </div>
        <SegmentBuilder onSave={handleCreateSegment} />
      </div>
    );
  }

  // Helper to view segment detail
  const viewSegmentDetail = (segmentId: string) => {
    setSelectedSegmentId(segmentId);
    setViewMode('detail');
  };

  return (
    <div className="p-6">
      {/* Header */}
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">Segments</h1>
          <p className="text-gray-600">Create dynamic audience segments based on contact attributes and behavior</p>
        </div>
        <div className="flex items-center gap-3">
          {segments.length > 0 && (
            <button
              onClick={handleRefreshAllCounts}
              disabled={loading}
              className="flex items-center gap-2 px-4 py-2 bg-gray-100 text-gray-700 font-medium rounded-lg hover:bg-gray-200 transition-colors disabled:opacity-50"
            >
              <svg className={`w-4 h-4 ${loading ? 'animate-spin' : ''}`} fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" />
              </svg>
              Refresh Counts
            </button>
          )}
          <button
            onClick={() => setShowBuilder(true)}
            className="flex items-center gap-2 px-4 py-2 bg-blue-600 text-white font-medium rounded-lg hover:bg-blue-700 transition-colors"
          >
            <svg className="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M12 4v16m8-8H4" />
            </svg>
            Create Segment
          </button>
        </div>
      </div>

      {/* Segments List */}
      {loading ? (
        <div className="flex items-center justify-center py-12">
          <div className="animate-spin w-8 h-8 border-4 border-blue-500 border-t-transparent rounded-full"></div>
        </div>
      ) : segments.length === 0 ? (
        <div className="text-center py-12 bg-gray-50 rounded-lg border-2 border-dashed border-gray-200">
          <svg className="w-12 h-12 text-gray-400 mx-auto mb-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M17 20h5v-2a3 3 0 00-5.356-1.857M17 20H7m10 0v-2c0-.656-.126-1.283-.356-1.857M7 20H2v-2a3 3 0 015.356-1.857M7 20v-2c0-.656.126-1.283.356-1.857m0 0a5.002 5.002 0 019.288 0M15 7a3 3 0 11-6 0 3 3 0 016 0zm6 3a2 2 0 11-4 0 2 2 0 014 0zM7 10a2 2 0 11-4 0 2 2 0 014 0z" />
          </svg>
          <h3 className="text-lg font-medium text-gray-900 mb-2">No segments yet</h3>
          <p className="text-gray-600 mb-4">Create your first segment to target specific audiences</p>
          <button
            onClick={() => setShowBuilder(true)}
            className="px-4 py-2 bg-blue-600 text-white font-medium rounded-lg hover:bg-blue-700 transition-colors"
          >
            Create Segment
          </button>
        </div>
      ) : (
        <div className="bg-white rounded-lg border border-gray-200 overflow-hidden">
          <table className="w-full">
            <thead className="bg-gray-50">
              <tr>
                <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
                  Segment
                </th>
                <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
                  Type
                </th>
                <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
                  Contacts
                </th>
                <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
                  Last Calculated
                </th>
                <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">
                  Status
                </th>
                <th className="px-6 py-3 text-right text-xs font-medium text-gray-500 uppercase tracking-wider">
                  Actions
                </th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-200">
              {segments.map((segment) => (
                <tr 
                  key={segment.id} 
                  className="hover:bg-gray-50 group cursor-pointer"
                  onClick={() => viewSegmentDetail(segment.id)}
                >
                  <td className="px-6 py-4">
                    <div className="text-sm font-medium text-gray-900 hover:text-blue-600">
                      {segment.name}
                    </div>
                    {segment.description && (
                      <div className="text-sm text-gray-500">{segment.description}</div>
                    )}
                  </td>
                  <td className="px-6 py-4">
                    <span className={`inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium ${
                      segment.segment_type === 'dynamic'
                        ? 'bg-blue-100 text-blue-800'
                        : 'bg-gray-100 text-gray-800'
                    }`}>
                      {segment.segment_type}
                    </span>
                  </td>
                  <td className="px-6 py-4">
                    <div className="flex items-center gap-2">
                      <span className="text-lg font-semibold text-gray-900">
                        {segment.subscriber_count.toLocaleString()}
                      </span>
                      <button
                        onClick={(e) => {
                          e.stopPropagation();
                          handleRecalculate(segment.id);
                        }}
                        className="p-1 text-gray-400 hover:text-blue-500 rounded opacity-0 group-hover:opacity-100 transition-opacity"
                        title="Refresh count"
                      >
                        <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" />
                        </svg>
                      </button>
                    </div>
                    <div className="text-xs text-gray-500">contacts</div>
                  </td>
                  <td className="px-6 py-4 text-sm text-gray-500">
                    {segment.last_calculated_at
                      ? new Date(segment.last_calculated_at).toLocaleDateString()
                      : 'Never'}
                  </td>
                  <td className="px-6 py-4">
                    <span className={`inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium ${
                      segment.status === 'active'
                        ? 'bg-green-100 text-green-800'
                        : 'bg-gray-100 text-gray-800'
                    }`}>
                      {segment.status}
                    </span>
                  </td>
                  <td className="px-6 py-4 text-right" onClick={(e) => e.stopPropagation()}>
                    <div className="flex items-center justify-end gap-2">
                      <button
                        onClick={() => handleRecalculate(segment.id)}
                        className="p-1.5 text-gray-400 hover:text-blue-500 hover:bg-blue-50 rounded"
                        title="Recalculate"
                      >
                        <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" />
                        </svg>
                      </button>
                      <button
                        onClick={() => handleCreateSnapshot(segment.id)}
                        className="p-1.5 text-gray-400 hover:text-purple-500 hover:bg-purple-50 rounded"
                        title="Create Snapshot"
                      >
                        <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M3 9a2 2 0 012-2h.93a2 2 0 001.664-.89l.812-1.22A2 2 0 0110.07 4h3.86a2 2 0 011.664.89l.812 1.22A2 2 0 0018.07 7H19a2 2 0 012 2v9a2 2 0 01-2 2H5a2 2 0 01-2-2V9z" />
                          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M15 13a3 3 0 11-6 0 3 3 0 016 0z" />
                        </svg>
                      </button>
                      <button
                        onClick={() => handleDeleteSegment(segment.id)}
                        className="p-1.5 text-gray-400 hover:text-red-500 hover:bg-red-50 rounded"
                        title="Delete"
                      >
                        <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16" />
                        </svg>
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* Feature Highlights */}
      <div className="mt-8 grid grid-cols-1 md:grid-cols-3 gap-4">
        <div className="p-4 bg-blue-50 rounded-lg">
          <div className="flex items-center gap-2 mb-2">
            <svg className="w-5 h-5 text-blue-500" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M9 19v-6a2 2 0 00-2-2H5a2 2 0 00-2 2v6a2 2 0 002 2h2a2 2 0 002-2zm0 0V9a2 2 0 012-2h2a2 2 0 012 2v10m-6 0a2 2 0 002 2h2a2 2 0 002-2m0 0V5a2 2 0 012-2h2a2 2 0 012 2v14a2 2 0 01-2 2h-2a2 2 0 01-2-2z" />
            </svg>
            <h4 className="font-medium text-blue-900">Behavioral Targeting</h4>
          </div>
          <p className="text-sm text-blue-700">
            Target contacts based on email opens, clicks, purchases, and custom events
          </p>
        </div>
        <div className="p-4 bg-purple-50 rounded-lg">
          <div className="flex items-center gap-2 mb-2">
            <svg className="w-5 h-5 text-purple-500" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M8 9l3 3-3 3m5 0h3M5 20h14a2 2 0 002-2V6a2 2 0 00-2-2H5a2 2 0 00-2 2v12a2 2 0 002 2z" />
            </svg>
            <h4 className="font-medium text-purple-900">Complex Logic</h4>
          </div>
          <p className="text-sm text-purple-700">
            Build nested AND/OR conditions for precise audience targeting
          </p>
        </div>
        <div className="p-4 bg-green-50 rounded-lg">
          <div className="flex items-center gap-2 mb-2">
            <svg className="w-5 h-5 text-green-500" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M9.663 17h4.673M12 3v1m6.364 1.636l-.707.707M21 12h-1M4 12H3m3.343-5.657l-.707-.707m2.828 9.9a5 5 0 117.072 0l-.548.547A3.374 3.374 0 0014 18.469V19a2 2 0 11-4 0v-.531c0-.895-.356-1.754-.988-2.386l-.548-.547z" />
            </svg>
            <h4 className="font-medium text-green-900">AI-Powered</h4>
          </div>
          <p className="text-sm text-green-700">
            Use predictive scores like churn risk and LTV for smarter targeting
          </p>
        </div>
      </div>
    </div>
  );
};

export default SegmentsPage;
