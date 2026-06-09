import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faSpinner, faSync, faExclamationTriangle, faClipboardList } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';

interface AuditEvent {
  id: string;
  actor: string;
  action: string;
  target_type: string;
  target_id: string;
  before_state: unknown;
  after_state: unknown;
  created_at: string;
}

const ACTION_COLORS: Record<string, string> = {
  create_partner: '#10b981',
  create_dataset: '#10b981',
  emergency_stop_dataset: '#ef4444',
  resume_dataset: '#10b981',
  update_isp_distribution: '#a78bfa',
  update_creative: '#60a5fa',
};

export const AuditLogPanel: React.FC = () => {
  const [events, setEvents] = useState<AuditEvent[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [actionFilter, setActionFilter] = useState('');
  const [actorFilter, setActorFilter] = useState('');
  const [expanded, setExpanded] = useState<Set<string>>(new Set());

  const fetchEvents = useCallback(() => {
    setLoading(true);
    setError(null);
    const params = new URLSearchParams();
    if (actionFilter) params.set('action', actionFilter);
    if (actorFilter) params.set('actor', actorFilter);
    params.set('limit', '200');
    apiFetch(`/api/mailing/data-partners/audit-log?${params.toString()}`, { credentials: 'include' })
      .then(r => r.json())
      .then(data => setEvents(data?.events ?? []))
      .catch(err => setError(String(err)))
      .finally(() => setLoading(false));
  }, [actionFilter, actorFilter]);

  useEffect(() => { fetchEvents(); }, [fetchEvents]);

  const toggle = (id: string) => {
    setExpanded(prev => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const uniqueActions = Array.from(new Set(events.map(e => e.action))).sort();
  const uniqueActors = Array.from(new Set(events.map(e => e.actor))).sort();

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 }}>
        <div style={{ fontSize: 13, color: 'rgba(180,210,240,0.7)' }}>
          <FontAwesomeIcon icon={faClipboardList} /> Every admin action — partner creation, dataset CRUD, emergency stop / resume, ISP overrides, creative hot-swaps — is recorded here with before/after state.
        </div>
        <button onClick={fetchEvents} style={iconBtn}>
          <FontAwesomeIcon icon={faSync} spin={loading} /> Refresh
        </button>
      </div>

      <div style={{ display: 'flex', gap: 8, marginBottom: 12 }}>
        <select value={actionFilter} onChange={e => setActionFilter(e.target.value)} style={selectStyle}>
          <option value="">All actions</option>
          {uniqueActions.map(a => <option key={a} value={a}>{a}</option>)}
        </select>
        <select value={actorFilter} onChange={e => setActorFilter(e.target.value)} style={selectStyle}>
          <option value="">All actors</option>
          {uniqueActors.map(a => <option key={a} value={a}>{a}</option>)}
        </select>
      </div>

      {error && (
        <div style={errorBox}>
          <FontAwesomeIcon icon={faExclamationTriangle} /> {error}
        </div>
      )}

      {loading && events.length === 0 && (
        <div style={{ textAlign: 'center', padding: 40, color: 'rgba(180,210,240,0.65)' }}>
          <FontAwesomeIcon icon={faSpinner} spin /> Loading…
        </div>
      )}

      <table style={tableStyle}>
        <thead>
          <tr style={{ background: 'rgba(120,150,200,0.06)' }}>
            <th style={th}>When</th>
            <th style={th}>Actor</th>
            <th style={th}>Action</th>
            <th style={th}>Target</th>
            <th style={th}></th>
          </tr>
        </thead>
        <tbody>
          {events.map(ev => (
            <React.Fragment key={ev.id}>
              <tr style={tr}>
                <td style={td}>{new Date(ev.created_at).toLocaleString()}</td>
                <td style={td}>{ev.actor}</td>
                <td style={td}>
                  <span style={{ color: ACTION_COLORS[ev.action] ?? '#cbd5f5', fontWeight: 600 }}>{ev.action}</span>
                </td>
                <td style={tdMono}>
                  {ev.target_type}
                  {ev.target_id ? <span style={{ color: 'rgba(180,210,240,0.55)' }}>: {ev.target_id.slice(0, 8)}…</span> : null}
                </td>
                <td style={td}>
                  {(ev.before_state != null || ev.after_state != null) ? (
                    <button onClick={() => toggle(ev.id)} style={ghostBtn}>
                      {expanded.has(ev.id) ? 'Hide' : 'Diff'}
                    </button>
                  ) : null}
                </td>
              </tr>
              {expanded.has(ev.id) && (
                <tr>
                  <td colSpan={5} style={{ ...td, background: 'rgba(0,0,0,0.25)' }}>
                    <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16 }}>
                      <div>
                        <div style={{ fontSize: 11, color: '#ef4444', textTransform: 'uppercase', marginBottom: 4 }}>Before</div>
                        <pre style={preStyle}>{String(JSON.stringify(ev.before_state ?? null, null, 2))}</pre>
                      </div>
                      <div>
                        <div style={{ fontSize: 11, color: '#10b981', textTransform: 'uppercase', marginBottom: 4 }}>After</div>
                        <pre style={preStyle}>{String(JSON.stringify(ev.after_state ?? null, null, 2))}</pre>
                      </div>
                    </div>
                  </td>
                </tr>
              )}
            </React.Fragment>
          ))}
          {!loading && events.length === 0 && (
            <tr>
              <td colSpan={5} style={{ ...td, textAlign: 'center', color: 'rgba(180,210,240,0.55)', padding: 32 }}>
                No audit events yet.
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  );
};

const tableStyle: React.CSSProperties = {
  width: '100%', borderCollapse: 'collapse', background: 'rgba(15,30,60,0.35)',
  borderRadius: 8, overflow: 'hidden', fontSize: 13,
};
const th: React.CSSProperties = {
  textAlign: 'left', padding: '10px 12px', color: 'rgba(180,210,240,0.65)',
  fontWeight: 500, fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.5,
};
const td: React.CSSProperties = { padding: '10px 12px', borderTop: '1px solid rgba(120,150,200,0.1)' };
const tdMono: React.CSSProperties = { ...td, fontFamily: 'ui-monospace, monospace', fontSize: 12 };
const tr: React.CSSProperties = {};
const iconBtn: React.CSSProperties = {
  background: 'rgba(120,150,200,0.08)', color: 'rgba(220,235,250,0.85)',
  border: '1px solid rgba(120,150,200,0.18)', padding: '6px 12px', borderRadius: 6,
  cursor: 'pointer', fontSize: 12,
};
const ghostBtn: React.CSSProperties = {
  background: 'transparent', color: '#6366f1',
  border: '1px solid rgba(99,102,241,0.4)', padding: '4px 10px', borderRadius: 4,
  cursor: 'pointer', fontSize: 12,
};
const selectStyle: React.CSSProperties = {
  background: 'rgba(0,0,0,0.25)', color: 'white',
  border: '1px solid rgba(120,150,200,0.25)', padding: '6px 10px', borderRadius: 4, fontSize: 13,
};
const preStyle: React.CSSProperties = {
  background: 'rgba(0,0,0,0.4)', padding: 12, borderRadius: 4,
  color: 'rgba(220,235,250,0.85)', fontSize: 11, fontFamily: 'ui-monospace, monospace',
  margin: 0, overflow: 'auto', maxHeight: 240,
};
const errorBox: React.CSSProperties = {
  background: 'rgba(239,68,68,0.18)', border: '1px solid rgba(239,68,68,0.4)',
  padding: 8, borderRadius: 4, marginBottom: 12, fontSize: 12,
};
