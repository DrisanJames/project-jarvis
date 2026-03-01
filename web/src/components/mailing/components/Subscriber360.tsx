import { useState, useCallback, useEffect, useRef } from 'react';

interface Subscriber360Data {
  profile: {
    id: string;
    email: string;
    first_name: string;
    last_name: string;
    status: string;
    data_quality_score: number;
    data_source: string;
    verification_status: string;
    created_at: string;
  };
  events: Array<{
    id: number;
    event_type: string;
    source: string;
    metadata: Record<string, unknown>;
    event_at: string;
  }>;
  engagement_chart: Array<{
    date: string;
    opens: number;
    clicks: number;
    page_views: number;
  }>;
  quality: {
    score: number;
    verification_status: string;
    data_source: string;
    total_events: number;
  };
}

const qualityColor = (score: number) => {
  if (score >= 0.75) return 'text-emerald-400';
  if (score >= 0.50) return 'text-yellow-400';
  if (score >= 0.25) return 'text-orange-400';
  return 'text-red-400';
};

const qualityLabel = (score: number) => {
  if (score >= 0.75) return 'High';
  if (score >= 0.50) return 'Good';
  if (score >= 0.25) return 'Fair';
  return 'Low';
};

const eventIcon = (type: string) => {
  switch (type) {
    case 'open': return '📧';
    case 'click': return '🖱️';
    case 'page_view': return '👁️';
    case 'session_end': return '⏱️';
    case 'send': return '📤';
    case 'bounce': return '⚠️';
    case 'unsubscribe': return '🚫';
    default: return '📌';
  }
};

export default function Subscriber360() {
  const [email, setEmail] = useState('');
  const [data, setData] = useState<Subscriber360Data | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const eventStreamRef = useRef<EventSource | null>(null);
  const [liveEvents, setLiveEvents] = useState<Array<{ type: string; time: string }>>([]);

  const lookup = useCallback(async () => {
    if (!email.trim()) return;
    setLoading(true);
    setError('');
    try {
      const res = await fetch(`/api/v1/subscribers/${encodeURIComponent(email)}/360`);
      if (!res.ok) throw new Error('Subscriber not found');
      const json = await res.json();
      setData(json);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Unknown error');
      setData(null);
    } finally {
      setLoading(false);
    }
  }, [email]);

  useEffect(() => {
    if (!data) return;
    const es = new EventSource('/ws/events');
    eventStreamRef.current = es;
    es.onmessage = (e) => {
      try {
        const event = JSON.parse(e.data);
        if (event.email_hash || event.subscriber_id) {
          setLiveEvents(prev => [
            { type: event.event_type, time: new Date().toISOString() },
            ...prev.slice(0, 19),
          ]);
        }
      } catch { /* ignore parse errors */ }
    };
    return () => es.close();
  }, [data]);

  return (
    <div className="space-y-6">
      <div className="flex items-center gap-4">
        <input
          type="email"
          value={email}
          onChange={e => setEmail(e.target.value)}
          onKeyDown={e => e.key === 'Enter' && lookup()}
          placeholder="Enter subscriber email..."
          className="flex-1 bg-gray-800 border border-gray-700 rounded-lg px-4 py-2.5 text-white placeholder-gray-500 focus:border-indigo-500 focus:ring-1 focus:ring-indigo-500 outline-none"
        />
        <button
          onClick={lookup}
          disabled={loading}
          className="px-6 py-2.5 bg-gradient-to-r from-indigo-600 to-purple-600 hover:from-indigo-500 hover:to-purple-500 text-white rounded-lg font-medium transition-all disabled:opacity-50"
        >
          {loading ? 'Loading...' : 'Lookup'}
        </button>
      </div>

      {error && <p className="text-red-400 text-sm">{error}</p>}

      {data && (
        <>
          {/* Profile Card */}
          <div className="grid grid-cols-1 md:grid-cols-4 gap-4">
            <div className="bg-gray-800/50 rounded-xl p-5 border border-gray-700/50">
              <p className="text-gray-400 text-xs uppercase tracking-wider mb-1">Email</p>
              <p className="text-white font-medium truncate">{data.profile.email}</p>
              <p className="text-gray-500 text-xs mt-1">
                {data.profile.first_name} {data.profile.last_name}
              </p>
            </div>
            <div className="bg-gray-800/50 rounded-xl p-5 border border-gray-700/50">
              <p className="text-gray-400 text-xs uppercase tracking-wider mb-1">Quality Score</p>
              <p className={`text-2xl font-bold ${qualityColor(data.quality.score)}`}>
                {(data.quality.score * 100).toFixed(0)}%
              </p>
              <p className="text-gray-500 text-xs mt-1">{qualityLabel(data.quality.score)}</p>
            </div>
            <div className="bg-gray-800/50 rounded-xl p-5 border border-gray-700/50">
              <p className="text-gray-400 text-xs uppercase tracking-wider mb-1">Status</p>
              <span className={`inline-block px-2 py-0.5 rounded-full text-xs font-medium ${
                data.profile.status === 'confirmed' ? 'bg-emerald-500/20 text-emerald-400' :
                data.profile.status === 'bounced' ? 'bg-red-500/20 text-red-400' :
                'bg-gray-500/20 text-gray-400'
              }`}>
                {data.profile.status}
              </span>
              <p className="text-gray-500 text-xs mt-2">{data.quality.verification_status || 'unverified'}</p>
            </div>
            <div className="bg-gray-800/50 rounded-xl p-5 border border-gray-700/50">
              <p className="text-gray-400 text-xs uppercase tracking-wider mb-1">Total Events</p>
              <p className="text-2xl font-bold text-white">{data.quality.total_events}</p>
              <p className="text-gray-500 text-xs mt-1">Source: {data.quality.data_source || 'unknown'}</p>
            </div>
          </div>

          {/* Engagement Chart (simple bar representation) */}
          {data.engagement_chart && data.engagement_chart.length > 0 && (
            <div className="bg-gray-800/50 rounded-xl p-5 border border-gray-700/50">
              <h3 className="text-white font-semibold mb-4">7-Day Engagement</h3>
              <div className="flex items-end gap-2 h-32">
                {data.engagement_chart.map(day => {
                  const max = Math.max(...data.engagement_chart.map(d => d.opens + d.clicks + d.page_views), 1);
                  const total = day.opens + day.clicks + day.page_views;
                  const height = (total / max) * 100;
                  return (
                    <div key={day.date} className="flex-1 flex flex-col items-center gap-1">
                      <div className="w-full flex flex-col justify-end" style={{ height: '100px' }}>
                        <div
                          className="w-full bg-gradient-to-t from-indigo-600 to-purple-500 rounded-t-sm transition-all"
                          style={{ height: `${height}%`, minHeight: total > 0 ? '4px' : '0px' }}
                          title={`Opens: ${day.opens}, Clicks: ${day.clicks}, Page Views: ${day.page_views}`}
                        />
                      </div>
                      <span className="text-[10px] text-gray-500">{day.date.slice(5)}</span>
                    </div>
                  );
                })}
              </div>
              <div className="flex gap-4 mt-3 text-xs text-gray-500">
                <span className="flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-indigo-500" /> Opens</span>
                <span className="flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-purple-500" /> Clicks</span>
                <span className="flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-pink-500" /> Page Views</span>
              </div>
            </div>
          )}

          {/* Live Events */}
          {liveEvents.length > 0 && (
            <div className="bg-gray-800/50 rounded-xl p-5 border border-gray-700/50">
              <h3 className="text-white font-semibold mb-3 flex items-center gap-2">
                <span className="w-2 h-2 rounded-full bg-green-500 animate-pulse" />
                Live Events
              </h3>
              <div className="space-y-1 max-h-32 overflow-y-auto">
                {liveEvents.map((ev, i) => (
                  <div key={i} className="flex justify-between text-sm">
                    <span className="text-gray-300">{ev.type}</span>
                    <span className="text-gray-500 text-xs">{new Date(ev.time).toLocaleTimeString()}</span>
                  </div>
                ))}
              </div>
            </div>
          )}

          {/* Event Timeline */}
          <div className="bg-gray-800/50 rounded-xl p-5 border border-gray-700/50">
            <h3 className="text-white font-semibold mb-4">Event Timeline</h3>
            <div className="space-y-2 max-h-96 overflow-y-auto">
              {data.events && data.events.length > 0 ? (
                data.events.map(event => (
                  <div key={event.id} className="flex items-start gap-3 py-2 border-b border-gray-700/30 last:border-0">
                    <span className="text-lg">{eventIcon(event.event_type)}</span>
                    <div className="flex-1 min-w-0">
                      <div className="flex justify-between items-center">
                        <span className="text-white text-sm font-medium">{event.event_type}</span>
                        <span className="text-gray-500 text-xs">{new Date(event.event_at).toLocaleString()}</span>
                      </div>
                      <p className="text-gray-400 text-xs">
                        Source: {event.source}
                        {event.metadata?.page_path ? ` · ${String(event.metadata.page_path)}` : null}
                        {event.metadata?.dwell_ms ? ` · ${Math.round(Number(event.metadata.dwell_ms) / 1000)}s dwell` : null}
                      </p>
                    </div>
                  </div>
                ))
              ) : (
                <p className="text-gray-500 text-sm">No events recorded yet</p>
              )}
            </div>
          </div>
        </>
      )}
    </div>
  );
}
