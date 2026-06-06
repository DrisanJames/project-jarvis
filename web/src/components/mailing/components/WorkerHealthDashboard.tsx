import React, { useCallback, useEffect, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faHeartPulse,
  faCircleCheck,
  faTriangleExclamation,
  faSpinner,
  faSyncAlt,
  faSkull,
} from '@fortawesome/free-solid-svg-icons';

// WorkerHealthDashboard — live view of background-worker heartbeats and stall
// status. Mirrors GET /api/worker-health (handlers_worker_health.go), which
// reads mailing_worker_heartbeats. A worker is "stalled" once it misses 3× its
// declared cycle interval; the WorkerHealthMonitor also pushes a Slack alert.
// Built 2026-06-04 after the EngineSignalsArchiver silently wedged for ~2
// months with no visibility.

const PAGE_VERSION = '1.0';
const POLL_INTERVAL_MS = 15000;

interface WorkerRow {
  worker_name: string;
  last_beat_at: string;
  seconds_since_beat: number;
  last_status: string;
  last_error?: string;
  cycle_count: number;
  expected_interval_seconds: number;
  stalled: boolean;
  stalled_since?: string;
  last_alerted_at?: string;
}

interface HealthResponse {
  api_version: string;
  generated_at: string;
  stalled_count: number;
  workers: WorkerRow[];
}

const fmtSeconds = (s: number): string => {
  if (s <= 0) return '0s';
  if (s < 60) return `${Math.round(s)}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${Math.round(s % 60)}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
};

const fmtInterval = (s: number): string => {
  if (s % 3600 === 0) return `${s / 3600}h`;
  if (s % 60 === 0) return `${s / 60}m`;
  return `${s}s`;
};

// A worker is healthy when last beat is within its expected interval; "late"
// once it exceeds 1× but the monitor hasn't yet flagged a hard stall (3×).
const rowState = (r: WorkerRow): 'stalled' | 'error' | 'late' | 'ok' => {
  if (r.stalled) return 'stalled';
  if (r.last_status === 'error') return 'error';
  if (r.seconds_since_beat > r.expected_interval_seconds) return 'late';
  return 'ok';
};

const stateMeta: Record<string, { color: string; label: string; icon: typeof faCircleCheck }> = {
  ok: { color: '#22c55e', label: 'Healthy', icon: faCircleCheck },
  late: { color: '#f59e0b', label: 'Late', icon: faTriangleExclamation },
  error: { color: '#f59e0b', label: 'Error', icon: faTriangleExclamation },
  stalled: { color: '#ef4444', label: 'Stalled', icon: faSkull },
};

export const WorkerHealthDashboard: React.FC = () => {
  const [data, setData] = useState<HealthResponse | null>(null);
  const [loading, setLoading] = useState<boolean>(true);
  const [error, setError] = useState<string | null>(null);
  const [lastFetchAt, setLastFetchAt] = useState<Date | null>(null);

  const fetchHealth = useCallback(async () => {
    try {
      const res = await fetch('/api/worker-health', { credentials: 'include' });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const json = (await res.json()) as HealthResponse;
      setData(json);
      setError(null);
      setLastFetchAt(new Date());
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchHealth();
    const timer = setInterval(fetchHealth, POLL_INTERVAL_MS);
    return () => clearInterval(timer);
  }, [fetchHealth]);

  const workers = data?.workers ?? [];
  const stalledCount = data?.stalled_count ?? 0;

  return (
    <div style={{ padding: '24px', color: '#e5e7eb', minHeight: '100vh' }}>
      <header style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 18 }}>
        <div>
          <h1 style={{ margin: 0, fontSize: 22, display: 'flex', alignItems: 'center', gap: 10 }}>
            <FontAwesomeIcon icon={faHeartPulse} style={{ color: stalledCount > 0 ? '#ef4444' : '#22c55e' }} />
            Worker Health
          </h1>
          <div style={{ fontSize: 12, color: '#94a3b8', marginTop: 4 }}>
            Background-worker heartbeats from <code>mailing_worker_heartbeats</code> — polls every 15s.
            {' '}Page v{PAGE_VERSION}
            {data && <> · API v{data.api_version}</>}
            {lastFetchAt && <> · Last fetch {lastFetchAt.toLocaleTimeString()}</>}
          </div>
        </div>
        <button
          onClick={fetchHealth}
          disabled={loading}
          style={{
            background: '#1e293b', color: '#e5e7eb', border: '1px solid #334155',
            padding: '6px 14px', borderRadius: 6, cursor: loading ? 'not-allowed' : 'pointer',
            display: 'flex', alignItems: 'center', gap: 6, fontSize: 13,
          }}
        >
          <FontAwesomeIcon icon={loading ? faSpinner : faSyncAlt} spin={loading} />
          Refresh
        </button>
      </header>

      {error && (
        <div style={{ background: '#3f1d1d', color: '#fecaca', padding: '10px 14px', borderRadius: 6, marginBottom: 14, fontSize: 13, display: 'flex', alignItems: 'center', gap: 8 }}>
          <FontAwesomeIcon icon={faTriangleExclamation} />
          Error loading worker health: {error}
        </div>
      )}

      {stalledCount > 0 && (
        <div style={{ background: '#3f1d1d', color: '#fecaca', padding: '12px 16px', borderRadius: 6, marginBottom: 16, fontSize: 14, display: 'flex', alignItems: 'center', gap: 10, border: '1px solid #ef444455' }}>
          <FontAwesomeIcon icon={faSkull} style={{ color: '#ef4444' }} />
          <strong>{stalledCount}</strong> worker{stalledCount === 1 ? '' : 's'} stalled — a Slack alert has been sent (if configured).
        </div>
      )}

      {loading && !data ? (
        <div style={{ textAlign: 'center', padding: 60, color: '#94a3b8' }}>
          <FontAwesomeIcon icon={faSpinner} spin /> Loading worker health…
        </div>
      ) : workers.length === 0 ? (
        <div style={{ padding: 40, textAlign: 'center', color: '#64748b', fontSize: 13, background: '#0f172a', border: '1px solid #1f2937', borderRadius: 6 }}>
          No worker heartbeats recorded yet. Workers emit their first beat at the end of their first cycle after boot.
        </div>
      ) : (
        <section style={{ background: '#0f172a', border: '1px solid #1f2937', borderRadius: 6, overflowX: 'auto' }}>
          <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
            <thead>
              <tr style={{ background: '#1e293b', color: '#94a3b8' }}>
                <th style={thStyle}>Worker</th>
                <th style={thStyle}>State</th>
                <th style={thStyle}>Last beat</th>
                <th style={thStyle}>Cycle interval</th>
                <th style={thStyle}>Cycles</th>
                <th style={thStyle}>Detail</th>
              </tr>
            </thead>
            <tbody>
              {workers.map((r) => {
                const st = rowState(r);
                const meta = stateMeta[st];
                return (
                  <tr key={r.worker_name} style={{ borderTop: '1px solid #1f2937' }}>
                    <td style={{ ...tdStyle, fontFamily: 'monospace' }}>{r.worker_name}</td>
                    <td style={tdStyle}>
                      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6, color: meta.color, fontWeight: 600 }}>
                        <FontAwesomeIcon icon={meta.icon} />
                        {meta.label}
                      </span>
                    </td>
                    <td style={{ ...tdStyle, color: st === 'stalled' ? '#ef4444' : '#e5e7eb' }}>
                      {fmtSeconds(r.seconds_since_beat)} ago
                    </td>
                    <td style={tdStyle}>{fmtInterval(r.expected_interval_seconds)}</td>
                    <td style={tdStyle}>{r.cycle_count.toLocaleString()}</td>
                    <td style={{ ...tdStyle, maxWidth: 420, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={r.last_error || ''}>
                      {r.stalled && r.stalled_since
                        ? `Stalled since ${new Date(r.stalled_since).toLocaleString()}`
                        : r.last_error || '—'}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </section>
      )}
    </div>
  );
};

const thStyle: React.CSSProperties = {
  textAlign: 'left', padding: '10px 14px', fontWeight: 600, fontSize: 11,
  textTransform: 'uppercase', letterSpacing: 0.4,
};

const tdStyle: React.CSSProperties = {
  padding: '10px 14px', fontSize: 13, color: '#e5e7eb',
};

export default WorkerHealthDashboard;
