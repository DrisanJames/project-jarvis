import React, { useState, useEffect, useCallback, useRef } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faShieldAlt, faHashtag, faBan, faSearch,
  faExclamationTriangle, faCheckCircle, faTrash,
  faFileExport, faPlus, faSync, faClock,
  faChartBar, faGlobe, faEnvelope,
} from '@fortawesome/free-solid-svg-icons';

interface SuppressionStats {
  total_suppressed: number;
  today_added: number;
  last_24h_added: number;
  last_1h_added: number;
  by_reason: Record<string, number>;
  by_source: Record<string, number>;
  by_isp: Record<string, number>;
  velocity_per_min: number;
}

interface SuppressionEntry {
  id: string;
  email: string;
  md5_hash: string;
  reason: string;
  source: string;
  isp: string;
  dsn_code: string;
  dsn_diag: string;
  source_ip: string;
  campaign_id: string;
  created_at: string;
}

interface ScrubResult {
  total_input: number;
  deliverable_count: number;
  suppressed_count: number;
  suppression_rate: number;
  processing_ms: number;
}

interface SSEEvent {
  email: string;
  md5_hash: string;
  reason: string;
  source: string;
  isp: string;
  action: string;
  timestamp: string;
}

const REASON_COLORS: Record<string, string> = {
  hard_bounce: '#ef4444',
  soft_bounce: '#f97316',
  spam_complaint: '#dc2626',
  unsubscribe: '#8b5cf6',
  inactive: '#6b7280',
  fbl_complaint: '#dc2626',
  manual: '#3b82f6',
  repeated_transient: '#f59e0b',
};

const REASON_LABELS: Record<string, string> = {
  hard_bounce: 'Hard Bounce',
  soft_bounce: 'Soft Bounce',
  spam_complaint: 'Spam Complaint',
  unsubscribe: 'Unsubscribe',
  inactive: 'Inactive (4+ sends)',
  'fbl-complaint': 'FBL Complaint',
  manual: 'Manual',
  'repeated-transient': 'Repeated Transient',
  'bad-mailbox': 'Bad Mailbox',
  'bad-domain': 'Bad Domain',
  'inactive-mailbox': 'Inactive Mailbox',
  'policy-related': 'Policy',
  'spam-related': 'Spam Related',
  'routing-errors': 'Routing Error',
};

export const GlobalSuppressionDashboard: React.FC = () => {
  const [stats, setStats] = useState<SuppressionStats | null>(null);
  const [entries, setEntries] = useState<SuppressionEntry[]>([]);
  const [totalEntries, setTotalEntries] = useState(0);
  const [searchQuery, setSearchQuery] = useState('');
  const [liveEvents, setLiveEvents] = useState<SSEEvent[]>([]);
  const [activeView, setActiveView] = useState<'overview' | 'search' | 'scrub' | 'live'>('overview');
  const [loading, setLoading] = useState(true);

  const [scrubInput, setScrubInput] = useState('');
  const [scrubResult, setScrubResult] = useState<ScrubResult | null>(null);
  const [scrubbing, setScrubbing] = useState(false);

  const [suppressEmail, setSuppressEmail] = useState('');
  const [suppressReason, setSuppressReason] = useState('manual');

  const eventSourceRef = useRef<EventSource | null>(null);

  const fetchStats = useCallback(async () => {
    try {
      const res = await fetch('/api/mailing/global-suppression/stats');
      if (res.ok) setStats(await res.json());
    } catch { /* ignore */ }
  }, []);

  const fetchEntries = useCallback(async (q?: string) => {
    try {
      setLoading(true);
      const params = new URLSearchParams({ limit: '50', offset: '0' });
      if (q) params.set('q', q);
      const res = await fetch(`/api/mailing/global-suppression/search?${params}`);
      if (res.ok) {
        const data = await res.json();
        setEntries(data.entries || []);
        setTotalEntries(data.total || 0);
      }
    } catch { /* ignore */ } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchStats();
    fetchEntries();
    const interval = setInterval(fetchStats, 15000);
    return () => clearInterval(interval);
  }, [fetchStats, fetchEntries]);

  useEffect(() => {
    if (activeView !== 'live') return;
    const es = new EventSource('/api/mailing/global-suppression/stream');
    eventSourceRef.current = es;
    es.onmessage = (event) => {
      try {
        const data: SSEEvent = JSON.parse(event.data);
        setLiveEvents(prev => [data, ...prev].slice(0, 200));
      } catch { /* ignore */ }
    };
    return () => es.close();
  }, [activeView]);

  const handleSearch = () => {
    fetchEntries(searchQuery);
  };

  const handleScrub = async () => {
    if (!scrubInput.trim()) return;
    setScrubbing(true);
    setScrubResult(null);
    try {
      const emails = scrubInput.split('\n').map(e => e.trim()).filter(Boolean);
      const isHashes = emails.every(e => /^[a-f0-9]{32}$/i.test(e));
      const body = isHashes ? { md5_hashes: emails } : { emails };
      const res = await fetch('/api/mailing/global-suppression/scrub-list', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (res.ok) setScrubResult(await res.json());
    } catch { /* ignore */ } finally {
      setScrubbing(false);
    }
  };

  const handleSuppress = async () => {
    if (!suppressEmail.trim()) return;
    await fetch('/api/mailing/global-suppression/suppress', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email: suppressEmail, reason: suppressReason, source: 'manual' }),
    });
    setSuppressEmail('');
    fetchStats();
    fetchEntries(searchQuery);
  };

  const handleRemove = async (email: string) => {
    if (!confirm(`Remove ${email} from global suppression?`)) return;
    await fetch(`/api/mailing/global-suppression/remove/${encodeURIComponent(email)}`, { method: 'DELETE' });
    fetchStats();
    fetchEntries(searchQuery);
  };

  const handleExportMD5 = () => {
    window.open('/api/mailing/global-suppression/export-md5?format=text', '_blank');
  };

  const reasonLabel = (reason: string) => REASON_LABELS[reason] || reason;
  const reasonColor = (reason: string) => REASON_COLORS[reason] || '#6b7280';

  const s: React.CSSProperties = {
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif',
    color: '#e0e0e0',
    padding: 24,
  };
  const card: React.CSSProperties = {
    background: 'linear-gradient(135deg, rgba(30,30,40,0.9), rgba(20,20,30,0.95))',
    border: '1px solid rgba(255,255,255,0.08)',
    borderRadius: 12,
    padding: 20,
    marginBottom: 16,
  };
  const statCard: React.CSSProperties = {
    ...card,
    textAlign: 'center' as const,
    flex: 1,
    minWidth: 160,
  };

  return (
    <div style={s}>
      {/* Header */}
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 24 }}>
        <div>
          <h2 style={{ margin: 0, fontSize: 22, fontWeight: 700, display: 'flex', alignItems: 'center', gap: 10 }}>
            <FontAwesomeIcon icon={faShieldAlt} style={{ color: '#ef4444' }} />
            Global Suppression Hub
          </h2>
          <p style={{ margin: '4px 0 0', fontSize: 13, color: '#8b8fa3' }}>
            Single source of truth — all negative signals converge here. MD5-hashed for instant comparison.
          </p>
        </div>
        <div style={{ display: 'flex', gap: 8 }}>
          <button onClick={handleExportMD5} style={{ background: 'rgba(59,130,246,0.15)', border: '1px solid rgba(59,130,246,0.3)', color: '#60a5fa', padding: '8px 14px', borderRadius: 8, cursor: 'pointer', fontSize: 13, display: 'flex', alignItems: 'center', gap: 6 }}>
            <FontAwesomeIcon icon={faFileExport} /> Export MD5
          </button>
          <button onClick={() => { fetchStats(); fetchEntries(searchQuery); }} style={{ background: 'rgba(34,197,94,0.15)', border: '1px solid rgba(34,197,94,0.3)', color: '#4ade80', padding: '8px 14px', borderRadius: 8, cursor: 'pointer', fontSize: 13, display: 'flex', alignItems: 'center', gap: 6 }}>
            <FontAwesomeIcon icon={faSync} /> Refresh
          </button>
        </div>
      </div>

      {/* Navigation */}
      <div style={{ display: 'flex', gap: 8, marginBottom: 20 }}>
        {(['overview', 'search', 'scrub', 'live'] as const).map(view => (
          <button
            key={view}
            onClick={() => setActiveView(view)}
            style={{
              background: activeView === view ? 'rgba(99,102,241,0.25)' : 'rgba(255,255,255,0.04)',
              border: `1px solid ${activeView === view ? 'rgba(99,102,241,0.5)' : 'rgba(255,255,255,0.08)'}`,
              color: activeView === view ? '#a5b4fc' : '#8b8fa3',
              padding: '8px 16px',
              borderRadius: 8,
              cursor: 'pointer',
              fontSize: 13,
              fontWeight: activeView === view ? 600 : 400,
              textTransform: 'capitalize' as const,
            }}
          >
            {view === 'overview' && <><FontAwesomeIcon icon={faChartBar} style={{ marginRight: 6 }} />Overview</>}
            {view === 'search' && <><FontAwesomeIcon icon={faSearch} style={{ marginRight: 6 }} />Search & Manage</>}
            {view === 'scrub' && <><FontAwesomeIcon icon={faShieldAlt} style={{ marginRight: 6 }} />Scrub List</>}
            {view === 'live' && <><FontAwesomeIcon icon={faClock} style={{ marginRight: 6 }} />Live Feed</>}
          </button>
        ))}
      </div>

      {/* ===== OVERVIEW ===== */}
      {activeView === 'overview' && stats && (
        <>
          {/* Top Stats */}
          <div style={{ display: 'flex', gap: 12, marginBottom: 16, flexWrap: 'wrap' }}>
            <div style={statCard}>
              <div style={{ fontSize: 28, fontWeight: 700, color: '#ef4444' }}>{(stats.total_suppressed || 0).toLocaleString()}</div>
              <div style={{ fontSize: 12, color: '#8b8fa3', marginTop: 4 }}>Total Suppressed</div>
            </div>
            <div style={statCard}>
              <div style={{ fontSize: 28, fontWeight: 700, color: '#f97316' }}>{(stats.today_added || 0).toLocaleString()}</div>
              <div style={{ fontSize: 12, color: '#8b8fa3', marginTop: 4 }}>Added Today</div>
            </div>
            <div style={statCard}>
              <div style={{ fontSize: 28, fontWeight: 700, color: '#eab308' }}>{(stats.last_24h_added || 0).toLocaleString()}</div>
              <div style={{ fontSize: 12, color: '#8b8fa3', marginTop: 4 }}>Last 24h</div>
            </div>
            <div style={statCard}>
              <div style={{ fontSize: 28, fontWeight: 700, color: '#a78bfa' }}>{(stats.last_1h_added || 0).toLocaleString()}</div>
              <div style={{ fontSize: 12, color: '#8b8fa3', marginTop: 4 }}>Last Hour</div>
            </div>
            <div style={statCard}>
              <div style={{ fontSize: 28, fontWeight: 700, color: '#60a5fa' }}>{(stats.velocity_per_min || 0).toFixed(1)}</div>
              <div style={{ fontSize: 12, color: '#8b8fa3', marginTop: 4 }}>Suppressions/min</div>
            </div>
          </div>

          {/* Breakdowns */}
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: 16 }}>
            {/* By Reason */}
            <div style={card}>
              <h3 style={{ margin: '0 0 12px', fontSize: 14, color: '#a5b4fc' }}>
                <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 6 }} />By Reason
              </h3>
              {Object.entries(stats.by_reason || {}).sort((a, b) => b[1] - a[1]).map(([reason, count]) => (
                <div key={reason} style={{ display: 'flex', justifyContent: 'space-between', padding: '6px 0', borderBottom: '1px solid rgba(255,255,255,0.04)' }}>
                  <span style={{ fontSize: 13, display: 'flex', alignItems: 'center', gap: 6 }}>
                    <span style={{ width: 8, height: 8, borderRadius: '50%', background: reasonColor(reason), display: 'inline-block' }} />
                    {reasonLabel(reason)}
                  </span>
                  <span style={{ fontSize: 13, fontWeight: 600, color: '#e0e0e0' }}>{count.toLocaleString()}</span>
                </div>
              ))}
              {Object.keys(stats.by_reason || {}).length === 0 && <div style={{ color: '#6b7280', fontSize: 13 }}>No data yet</div>}
            </div>

            {/* By Source */}
            <div style={card}>
              <h3 style={{ margin: '0 0 12px', fontSize: 14, color: '#a5b4fc' }}>
                <FontAwesomeIcon icon={faGlobe} style={{ marginRight: 6 }} />By Source
              </h3>
              {Object.entries(stats.by_source || {}).sort((a, b) => b[1] - a[1]).map(([source, count]) => (
                <div key={source} style={{ display: 'flex', justifyContent: 'space-between', padding: '6px 0', borderBottom: '1px solid rgba(255,255,255,0.04)' }}>
                  <span style={{ fontSize: 13, color: '#d1d5db' }}>{source}</span>
                  <span style={{ fontSize: 13, fontWeight: 600, color: '#e0e0e0' }}>{count.toLocaleString()}</span>
                </div>
              ))}
              {Object.keys(stats.by_source || {}).length === 0 && <div style={{ color: '#6b7280', fontSize: 13 }}>No data yet</div>}
            </div>

            {/* By ISP */}
            <div style={card}>
              <h3 style={{ margin: '0 0 12px', fontSize: 14, color: '#a5b4fc' }}>
                <FontAwesomeIcon icon={faEnvelope} style={{ marginRight: 6 }} />By ISP
              </h3>
              {Object.entries(stats.by_isp || {}).sort((a, b) => b[1] - a[1]).map(([isp, count]) => (
                <div key={isp} style={{ display: 'flex', justifyContent: 'space-between', padding: '6px 0', borderBottom: '1px solid rgba(255,255,255,0.04)' }}>
                  <span style={{ fontSize: 13, color: '#d1d5db' }}>{isp}</span>
                  <span style={{ fontSize: 13, fontWeight: 600, color: '#e0e0e0' }}>{count.toLocaleString()}</span>
                </div>
              ))}
              {Object.keys(stats.by_isp || {}).length === 0 && <div style={{ color: '#6b7280', fontSize: 13 }}>No data yet</div>}
            </div>
          </div>

          {/* Add Suppression */}
          <div style={{ ...card, marginTop: 16 }}>
            <h3 style={{ margin: '0 0 12px', fontSize: 14, color: '#a5b4fc' }}>
              <FontAwesomeIcon icon={faPlus} style={{ marginRight: 6 }} />Manual Suppress
            </h3>
            <div style={{ display: 'flex', gap: 8 }}>
              <input
                type="email"
                placeholder="email@example.com"
                value={suppressEmail}
                onChange={e => setSuppressEmail(e.target.value)}
                style={{ flex: 1, background: 'rgba(0,0,0,0.3)', border: '1px solid rgba(255,255,255,0.1)', borderRadius: 8, padding: '8px 12px', color: '#e0e0e0', fontSize: 13 }}
              />
              <select
                value={suppressReason}
                onChange={e => setSuppressReason(e.target.value)}
                style={{ background: 'rgba(0,0,0,0.3)', border: '1px solid rgba(255,255,255,0.1)', borderRadius: 8, padding: '8px 12px', color: '#e0e0e0', fontSize: 13 }}
              >
                <option value="manual">Manual</option>
                <option value="hard_bounce">Hard Bounce</option>
                <option value="spam_complaint">Spam Complaint</option>
                <option value="unsubscribe">Unsubscribe</option>
                <option value="inactive">Inactive</option>
              </select>
              <button onClick={handleSuppress} style={{ background: 'rgba(239,68,68,0.2)', border: '1px solid rgba(239,68,68,0.4)', color: '#f87171', padding: '8px 16px', borderRadius: 8, cursor: 'pointer', fontSize: 13 }}>
                <FontAwesomeIcon icon={faBan} /> Suppress
              </button>
            </div>
          </div>
        </>
      )}

      {/* ===== SEARCH & MANAGE ===== */}
      {activeView === 'search' && (
        <div>
          <div style={{ ...card, display: 'flex', gap: 8, alignItems: 'center' }}>
            <input
              type="text"
              placeholder="Search by email or MD5 hash..."
              value={searchQuery}
              onChange={e => setSearchQuery(e.target.value)}
              onKeyDown={e => e.key === 'Enter' && handleSearch()}
              style={{ flex: 1, background: 'rgba(0,0,0,0.3)', border: '1px solid rgba(255,255,255,0.1)', borderRadius: 8, padding: '10px 14px', color: '#e0e0e0', fontSize: 14 }}
            />
            <button onClick={handleSearch} style={{ background: 'rgba(99,102,241,0.2)', border: '1px solid rgba(99,102,241,0.4)', color: '#a5b4fc', padding: '10px 18px', borderRadius: 8, cursor: 'pointer', fontSize: 13 }}>
              <FontAwesomeIcon icon={faSearch} /> Search
            </button>
          </div>

          <div style={{ fontSize: 13, color: '#8b8fa3', marginBottom: 8 }}>
            {totalEntries.toLocaleString()} results {searchQuery && `for "${searchQuery}"`}
          </div>

          <div style={{ ...card, padding: 0, overflow: 'hidden' }}>
            <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
              <thead>
                <tr style={{ background: 'rgba(255,255,255,0.03)' }}>
                  <th style={{ padding: '10px 14px', textAlign: 'left', color: '#8b8fa3', fontWeight: 500 }}>Email</th>
                  <th style={{ padding: '10px 14px', textAlign: 'left', color: '#8b8fa3', fontWeight: 500 }}>MD5</th>
                  <th style={{ padding: '10px 14px', textAlign: 'left', color: '#8b8fa3', fontWeight: 500 }}>Reason</th>
                  <th style={{ padding: '10px 14px', textAlign: 'left', color: '#8b8fa3', fontWeight: 500 }}>Source</th>
                  <th style={{ padding: '10px 14px', textAlign: 'left', color: '#8b8fa3', fontWeight: 500 }}>ISP</th>
                  <th style={{ padding: '10px 14px', textAlign: 'left', color: '#8b8fa3', fontWeight: 500 }}>Date</th>
                  <th style={{ padding: '10px 14px', textAlign: 'center', color: '#8b8fa3', fontWeight: 500 }}>Action</th>
                </tr>
              </thead>
              <tbody>
                {entries.map(entry => (
                  <tr key={entry.id} style={{ borderTop: '1px solid rgba(255,255,255,0.04)' }}>
                    <td style={{ padding: '8px 14px', color: '#e0e0e0' }}>{entry.email || '(hash-only)'}</td>
                    <td style={{ padding: '8px 14px', color: '#6b7280', fontFamily: 'monospace', fontSize: 11 }}>{entry.md5_hash?.substring(0, 12)}...</td>
                    <td style={{ padding: '8px 14px' }}>
                      <span style={{ background: `${reasonColor(entry.reason)}22`, color: reasonColor(entry.reason), padding: '2px 8px', borderRadius: 4, fontSize: 11, fontWeight: 600 }}>
                        {reasonLabel(entry.reason)}
                      </span>
                    </td>
                    <td style={{ padding: '8px 14px', color: '#9ca3af', fontSize: 12 }}>{entry.source}</td>
                    <td style={{ padding: '8px 14px', color: '#9ca3af', fontSize: 12 }}>{entry.isp || '-'}</td>
                    <td style={{ padding: '8px 14px', color: '#6b7280', fontSize: 12 }}>{new Date(entry.created_at).toLocaleDateString()}</td>
                    <td style={{ padding: '8px 14px', textAlign: 'center' }}>
                      <button onClick={() => handleRemove(entry.email)} style={{ background: 'none', border: 'none', color: '#ef4444', cursor: 'pointer', fontSize: 13 }}>
                        <FontAwesomeIcon icon={faTrash} />
                      </button>
                    </td>
                  </tr>
                ))}
                {entries.length === 0 && !loading && (
                  <tr><td colSpan={7} style={{ padding: 20, textAlign: 'center', color: '#6b7280' }}>No entries found</td></tr>
                )}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {/* ===== SCRUB LIST ===== */}
      {activeView === 'scrub' && (
        <div>
          <div style={card}>
            <h3 style={{ margin: '0 0 8px', fontSize: 14, color: '#a5b4fc' }}>
              <FontAwesomeIcon icon={faShieldAlt} style={{ marginRight: 6 }} />Pre-Send List Scrub
            </h3>
            <p style={{ margin: '0 0 12px', fontSize: 13, color: '#8b8fa3' }}>
              Paste emails or MD5 hashes (one per line) to check against the global suppression hub. This is the same check that runs before every campaign send.
            </p>
            <textarea
              placeholder={"email1@example.com\nemail2@example.com\n... or paste MD5 hashes"}
              value={scrubInput}
              onChange={e => setScrubInput(e.target.value)}
              rows={10}
              style={{ width: '100%', background: 'rgba(0,0,0,0.3)', border: '1px solid rgba(255,255,255,0.1)', borderRadius: 8, padding: '10px 14px', color: '#e0e0e0', fontSize: 13, fontFamily: 'monospace', resize: 'vertical', boxSizing: 'border-box' }}
            />
            <div style={{ marginTop: 10, display: 'flex', gap: 8, alignItems: 'center' }}>
              <button onClick={handleScrub} disabled={scrubbing} style={{ background: 'rgba(34,197,94,0.2)', border: '1px solid rgba(34,197,94,0.4)', color: '#4ade80', padding: '10px 20px', borderRadius: 8, cursor: 'pointer', fontSize: 13, fontWeight: 600 }}>
                {scrubbing ? 'Scrubbing...' : 'Scrub Against Global Suppression'}
              </button>
              <span style={{ fontSize: 12, color: '#6b7280' }}>
                {scrubInput.split('\n').filter(l => l.trim()).length} entries ready
              </span>
            </div>
          </div>

          {scrubResult && (
            <div style={card}>
              <h3 style={{ margin: '0 0 12px', fontSize: 14, color: '#a5b4fc' }}>
                <FontAwesomeIcon icon={faCheckCircle} style={{ marginRight: 6, color: '#4ade80' }} />Scrub Results
              </h3>
              <div style={{ display: 'flex', gap: 16, marginBottom: 16 }}>
                <div style={{ flex: 1, textAlign: 'center', padding: 16, background: 'rgba(34,197,94,0.1)', borderRadius: 8, border: '1px solid rgba(34,197,94,0.2)' }}>
                  <div style={{ fontSize: 28, fontWeight: 700, color: '#4ade80' }}>{scrubResult.deliverable_count.toLocaleString()}</div>
                  <div style={{ fontSize: 12, color: '#4ade80' }}>Deliverable</div>
                </div>
                <div style={{ flex: 1, textAlign: 'center', padding: 16, background: 'rgba(239,68,68,0.1)', borderRadius: 8, border: '1px solid rgba(239,68,68,0.2)' }}>
                  <div style={{ fontSize: 28, fontWeight: 700, color: '#ef4444' }}>{scrubResult.suppressed_count.toLocaleString()}</div>
                  <div style={{ fontSize: 12, color: '#ef4444' }}>Suppressed</div>
                </div>
                <div style={{ flex: 1, textAlign: 'center', padding: 16, background: 'rgba(99,102,241,0.1)', borderRadius: 8, border: '1px solid rgba(99,102,241,0.2)' }}>
                  <div style={{ fontSize: 28, fontWeight: 700, color: '#a5b4fc' }}>{scrubResult.suppression_rate.toFixed(1)}%</div>
                  <div style={{ fontSize: 12, color: '#a5b4fc' }}>Suppression Rate</div>
                </div>
                <div style={{ flex: 1, textAlign: 'center', padding: 16, background: 'rgba(255,255,255,0.04)', borderRadius: 8, border: '1px solid rgba(255,255,255,0.08)' }}>
                  <div style={{ fontSize: 28, fontWeight: 700, color: '#d1d5db' }}>{scrubResult.processing_ms}ms</div>
                  <div style={{ fontSize: 12, color: '#6b7280' }}>Processing Time</div>
                </div>
              </div>
            </div>
          )}
        </div>
      )}

      {/* ===== LIVE FEED ===== */}
      {activeView === 'live' && (
        <div style={card}>
          <h3 style={{ margin: '0 0 12px', fontSize: 14, color: '#a5b4fc', display: 'flex', alignItems: 'center', gap: 8 }}>
            <FontAwesomeIcon icon={faClock} />
            Live Suppression Feed
            <span style={{ width: 8, height: 8, borderRadius: '50%', background: '#4ade80', animation: 'pulse 2s infinite' }} />
          </h3>
          <div style={{ maxHeight: 500, overflowY: 'auto' }}>
            {liveEvents.map((event, i) => (
              <div key={i} style={{ display: 'flex', alignItems: 'center', gap: 12, padding: '8px 0', borderBottom: '1px solid rgba(255,255,255,0.04)' }}>
                <FontAwesomeIcon icon={event.action === 'suppressed' ? faBan : faCheckCircle} style={{ color: event.action === 'suppressed' ? '#ef4444' : '#4ade80' }} />
                <span style={{ color: '#e0e0e0', fontSize: 13, minWidth: 200 }}>{event.email}</span>
                <span style={{ background: `${reasonColor(event.reason)}22`, color: reasonColor(event.reason), padding: '2px 8px', borderRadius: 4, fontSize: 11, fontWeight: 600 }}>
                  {reasonLabel(event.reason)}
                </span>
                <span style={{ color: '#6b7280', fontSize: 12 }}>{event.source}</span>
                <span style={{ color: '#6b7280', fontSize: 11, fontFamily: 'monospace' }}>{event.md5_hash?.substring(0, 12)}</span>
                <span style={{ marginLeft: 'auto', color: '#6b7280', fontSize: 11 }}>{new Date(event.timestamp).toLocaleTimeString()}</span>
              </div>
            ))}
            {liveEvents.length === 0 && (
              <div style={{ textAlign: 'center', padding: 40, color: '#6b7280' }}>
                <FontAwesomeIcon icon={faHashtag} style={{ fontSize: 24, marginBottom: 8 }} /><br />
                Waiting for suppression events...
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  );
};
