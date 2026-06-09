import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faBoxes,
  faCheck,
  faClock,
  faExclamationTriangle,
  faRotate,
  faSkullCrossbones,
  faSpinner,
  faSyncAlt,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';

// OutboxDashboard — live observability for the durable injection outbox.
//
// Mirrors the shape of GET /api/outbox/summary and /api/outbox/dead-letter
// (defined in upside-down/internal/api/outbox_admin.go). Polls every 5s so
// operators can watch a live wave dispatch. Zero new infra — the outbox
// table is its own source of truth.
//
// Page is intentionally compact: a strip of state-depth cards, a stuck-age
// bar with a red threshold, a rolling-5-minute throughput strip, and a
// dead-letter table at the bottom. No charts, no graphs, no filters that
// the operator does not immediately need during an incident.

const PAGE_VERSION = '1.0';
const POLL_INTERVAL_MS = 5000;
const STUCK_SUBMITTING_RED_THRESHOLD_SEC = 600;
const STUCK_SUBMITTING_AMBER_THRESHOLD_SEC = 300;

interface OutboxSummary {
  generated_at: string;
  api_version: string;
  depth_by_status: Record<string, number>;
  oldest_queued_seconds: number;
  oldest_sending_seconds: number;
  oldest_submitting_seconds: number;
  idempotency_keyed_rows: number;
  idempotency_null_rows: number;
  last_5min_inserted: number;
  last_5min_sent: number;
  last_5min_failed: number;
}

interface DeadLetterRow {
  id: string;
  idempotency_key?: string;
  campaign_id: string;
  subscriber_id: string;
  email: string;
  status: string;
  attempts: number;
  last_error?: string;
  last_attempt_at: string;
  created_at: string;
}

interface DeadLetterResponse {
  api_version: string;
  rows: DeadLetterRow[];
  count: number;
}

const fmt = (n: number | null | undefined): string => {
  if (n == null || Number.isNaN(n)) return '0';
  return n.toLocaleString();
};

const fmtSeconds = (s: number): string => {
  if (s <= 0) return '—';
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  const r = s % 60;
  if (m < 60) return `${m}m ${r}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
};

const stateColor = (state: string): string => {
  switch (state) {
    case 'accepted':
    case 'sent':
      return '#22c55e';
    case 'submitting':
    case 'sending':
    case 'claimed':
    case 'processing':
      return '#60a5fa';
    case 'queued':
    case 'pending':
      return '#94a3b8';
    case 'failed_retryable':
    case 'failed':
      return '#f59e0b';
    case 'failed_permanent':
    case 'dead_letter':
    case 'dead_letter_strict':
      return '#ef4444';
    case 'skipped':
      return '#64748b';
    default:
      return '#9ca3af';
  }
};

const stuckSubmittingColor = (ageSec: number): string => {
  if (ageSec >= STUCK_SUBMITTING_RED_THRESHOLD_SEC) return '#ef4444';
  if (ageSec >= STUCK_SUBMITTING_AMBER_THRESHOLD_SEC) return '#f59e0b';
  return '#22c55e';
};

export const OutboxDashboard: React.FC = () => {
  const [summary, setSummary] = useState<OutboxSummary | null>(null);
  const [deadLetter, setDeadLetter] = useState<DeadLetterRow[]>([]);
  const [loading, setLoading] = useState<boolean>(true);
  const [error, setError] = useState<string | null>(null);
  const [lastFetchAt, setLastFetchAt] = useState<Date | null>(null);

  const fetchAll = useCallback(async () => {
    try {
      const [sumRes, dlRes] = await Promise.all([
        apiFetch('/api/outbox/summary', { credentials: 'include' }),
        apiFetch('/api/outbox/dead-letter?limit=200', { credentials: 'include' }),
      ]);
      if (!sumRes.ok) throw new Error(`summary HTTP ${sumRes.status}`);
      if (!dlRes.ok) throw new Error(`dead-letter HTTP ${dlRes.status}`);
      const sumJson = (await sumRes.json()) as OutboxSummary;
      const dlJson = (await dlRes.json()) as DeadLetterResponse;
      setSummary(sumJson);
      setDeadLetter(dlJson.rows || []);
      setError(null);
      setLastFetchAt(new Date());
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      setError(msg);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchAll();
    const timer = setInterval(fetchAll, POLL_INTERVAL_MS);
    return () => clearInterval(timer);
  }, [fetchAll]);

  const stateCards = useMemo(() => {
    if (!summary) return [];
    const preferredOrder = [
      'queued',
      'submitting',
      'sending',
      'accepted',
      'sent',
      'failed_retryable',
      'failed',
      'failed_permanent',
      'dead_letter',
      'dead_letter_strict',
      'skipped',
    ];
    const known = new Set(preferredOrder);
    const extras = Object.keys(summary.depth_by_status).filter((k) => !known.has(k));
    const ordered = [...preferredOrder, ...extras].filter((k) => k in summary.depth_by_status);
    return ordered.map((state) => ({
      state,
      count: summary.depth_by_status[state] || 0,
      color: stateColor(state),
    }));
  }, [summary]);

  const stuckAge = summary?.oldest_submitting_seconds ?? 0;
  const idempotencyCoverage = summary
    ? summary.idempotency_keyed_rows + summary.idempotency_null_rows > 0
      ? (summary.idempotency_keyed_rows /
          (summary.idempotency_keyed_rows + summary.idempotency_null_rows)) *
        100
      : 0
    : 0;

  return (
    <div style={{ padding: '24px', color: '#e5e7eb', minHeight: '100vh' }}>
      <header
        style={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          marginBottom: 18,
        }}
      >
        <div>
          <h1 style={{ margin: 0, fontSize: 22, display: 'flex', alignItems: 'center', gap: 10 }}>
            <FontAwesomeIcon icon={faBoxes} style={{ color: '#60a5fa' }} />
            Durable Injection Outbox
          </h1>
          <div style={{ fontSize: 12, color: '#94a3b8', marginTop: 4 }}>
            Live view of <code>mailing_campaign_queue</code> — polls every 5s.
            {' '}
            Page v{PAGE_VERSION}
            {summary && <> · API v{summary.api_version}</>}
            {lastFetchAt && <> · Last fetch {lastFetchAt.toLocaleTimeString()}</>}
          </div>
        </div>
        <button
          onClick={fetchAll}
          disabled={loading}
          style={{
            background: '#1e293b',
            color: '#e5e7eb',
            border: '1px solid #334155',
            padding: '6px 14px',
            borderRadius: 6,
            cursor: loading ? 'not-allowed' : 'pointer',
            display: 'flex',
            alignItems: 'center',
            gap: 6,
            fontSize: 13,
          }}
        >
          <FontAwesomeIcon icon={loading ? faSpinner : faSyncAlt} spin={loading} />
          Refresh
        </button>
      </header>

      {error && (
        <div
          style={{
            background: '#3f1d1d',
            color: '#fecaca',
            padding: '10px 14px',
            borderRadius: 6,
            marginBottom: 14,
            fontSize: 13,
            display: 'flex',
            alignItems: 'center',
            gap: 8,
          }}
        >
          <FontAwesomeIcon icon={faExclamationTriangle} />
          Error loading outbox data: {error}
        </div>
      )}

      {loading && !summary ? (
        <div style={{ textAlign: 'center', padding: 60, color: '#94a3b8' }}>
          <FontAwesomeIcon icon={faSpinner} spin /> Loading outbox state…
        </div>
      ) : summary ? (
        <>
          <section
            style={{
              display: 'grid',
              gridTemplateColumns: 'repeat(auto-fit, minmax(140px, 1fr))',
              gap: 10,
              marginBottom: 18,
            }}
          >
            {stateCards.map((card) => (
              <div
                key={card.state}
                style={{
                  background: '#0f172a',
                  border: `1px solid ${card.color}33`,
                  borderLeft: `4px solid ${card.color}`,
                  borderRadius: 6,
                  padding: '12px 14px',
                }}
              >
                <div style={{ fontSize: 11, color: '#94a3b8', textTransform: 'uppercase', letterSpacing: 0.4 }}>
                  {card.state}
                </div>
                <div style={{ fontSize: 22, fontWeight: 600, marginTop: 4, color: card.color }}>
                  {fmt(card.count)}
                </div>
              </div>
            ))}
          </section>

          <section
            style={{
              display: 'grid',
              gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))',
              gap: 10,
              marginBottom: 18,
            }}
          >
            <Card
              title="Oldest submitting (stuck)"
              icon={faClock}
              iconColor={stuckSubmittingColor(stuckAge)}
              value={fmtSeconds(stuckAge)}
              note={
                stuckAge >= STUCK_SUBMITTING_RED_THRESHOLD_SEC
                  ? 'Reconciler should be recovering these; check logs.'
                  : stuckAge >= STUCK_SUBMITTING_AMBER_THRESHOLD_SEC
                  ? 'Approaching grace window.'
                  : 'Healthy.'
              }
            />
            <Card
              title="Oldest queued"
              icon={faClock}
              iconColor="#94a3b8"
              value={fmtSeconds(summary.oldest_queued_seconds)}
              note="Time since earliest waiting row was scheduled."
            />
            <Card
              title="Idempotency coverage"
              icon={faCheck}
              iconColor={idempotencyCoverage > 90 ? '#22c55e' : idempotencyCoverage > 50 ? '#f59e0b' : '#ef4444'}
              value={`${idempotencyCoverage.toFixed(1)}%`}
              note={`${fmt(summary.idempotency_keyed_rows)} keyed · ${fmt(summary.idempotency_null_rows)} legacy`}
            />
          </section>

          <section
            style={{
              display: 'grid',
              gridTemplateColumns: 'repeat(auto-fit, minmax(180px, 1fr))',
              gap: 10,
              marginBottom: 18,
            }}
          >
            <Card title="Inserted (last 5 min)" icon={faRotate} iconColor="#60a5fa" value={fmt(summary.last_5min_inserted)} />
            <Card title="Sent (last 5 min)" icon={faCheck} iconColor="#22c55e" value={fmt(summary.last_5min_sent)} />
            <Card
              title="Failed (last 5 min)"
              icon={faExclamationTriangle}
              iconColor={summary.last_5min_failed > 0 ? '#ef4444' : '#64748b'}
              value={fmt(summary.last_5min_failed)}
            />
          </section>

          <section style={{ background: '#0f172a', border: '1px solid #1f2937', borderRadius: 6 }}>
            <header
              style={{
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'space-between',
                padding: '12px 14px',
                borderBottom: '1px solid #1f2937',
              }}
            >
              <h2 style={{ margin: 0, fontSize: 14, display: 'flex', alignItems: 'center', gap: 8 }}>
                <FontAwesomeIcon icon={faSkullCrossbones} style={{ color: '#ef4444' }} />
                Dead-letter queue ({fmt(deadLetter.length)})
              </h2>
              <span style={{ fontSize: 11, color: '#94a3b8' }}>Most recent 200</span>
            </header>
            {deadLetter.length === 0 ? (
              <div style={{ padding: 28, textAlign: 'center', color: '#64748b', fontSize: 13 }}>
                No dead-letter rows. Pipeline healthy.
              </div>
            ) : (
              <div style={{ overflowX: 'auto' }}>
                <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 12 }}>
                  <thead>
                    <tr style={{ background: '#1e293b', color: '#94a3b8' }}>
                      <th style={thStyle}>Email</th>
                      <th style={thStyle}>Status</th>
                      <th style={thStyle}>Attempts</th>
                      <th style={thStyle}>Last error</th>
                      <th style={thStyle}>Last attempt</th>
                      <th style={thStyle}>Campaign</th>
                    </tr>
                  </thead>
                  <tbody>
                    {deadLetter.map((r) => (
                      <tr key={r.id} style={{ borderTop: '1px solid #1f2937' }}>
                        <td style={tdStyle}>{r.email || r.subscriber_id}</td>
                        <td style={{ ...tdStyle, color: stateColor(r.status) }}>{r.status}</td>
                        <td style={tdStyle}>{r.attempts}</td>
                        <td style={{ ...tdStyle, maxWidth: 380, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={r.last_error || ''}>
                          {r.last_error || '—'}
                        </td>
                        <td style={tdStyle}>{r.last_attempt_at ? new Date(r.last_attempt_at).toLocaleString() : '—'}</td>
                        <td style={{ ...tdStyle, fontFamily: 'monospace', fontSize: 11 }}>
                          {r.campaign_id ? r.campaign_id.slice(0, 8) : '—'}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </section>
        </>
      ) : null}
    </div>
  );
};

interface CardProps {
  title: string;
  icon: typeof faClock;
  iconColor: string;
  value: string;
  note?: string;
}

const Card: React.FC<CardProps> = ({ title, icon, iconColor, value, note }) => (
  <div
    style={{
      background: '#0f172a',
      border: '1px solid #1f2937',
      borderRadius: 6,
      padding: '12px 14px',
    }}
  >
    <div
      style={{
        fontSize: 11,
        color: '#94a3b8',
        textTransform: 'uppercase',
        letterSpacing: 0.4,
        display: 'flex',
        alignItems: 'center',
        gap: 6,
      }}
    >
      <FontAwesomeIcon icon={icon} style={{ color: iconColor }} />
      {title}
    </div>
    <div style={{ fontSize: 20, fontWeight: 600, marginTop: 6 }}>{value}</div>
    {note && <div style={{ fontSize: 11, color: '#64748b', marginTop: 4 }}>{note}</div>}
  </div>
);

const thStyle: React.CSSProperties = {
  textAlign: 'left',
  padding: '8px 12px',
  fontWeight: 600,
  fontSize: 11,
  textTransform: 'uppercase',
  letterSpacing: 0.4,
};

const tdStyle: React.CSSProperties = {
  padding: '8px 12px',
  fontSize: 12,
  color: '#e5e7eb',
};

export default OutboxDashboard;
