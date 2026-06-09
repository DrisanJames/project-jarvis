// Event Lake Explorer
//
// PAGE_VERSION 1.0 — read-only window into the S3/Athena analytics event lake
// (internal/analytics/reader.go + internal/api/handlers_analytics_lake.go).
//
// Backend (all READ ONLY, all under /api/mailing/analytics/lake):
//   GET /status   → { enabled_write, enabled_read, sent, failed, dropped }
//   GET /summary?from=YYYY-MM-DD&to=YYYY-MM-DD
//                 → { rows: [{ event_type, count }] }  OR  { disabled: true, rows: [] }
//   GET /events?dt=&campaign_id=&isp_group=&event_type=&limit=
//                 → { events: [Event…] }  OR  { disabled: true, events: [] }
//
// The lake is DISABLED BY DEFAULT (ships dark): the write side fans events to
// Firehose only when ANALYTICS_FIREHOSE_STREAM is set, and the read side only
// answers when ANALYTICS_ATHENA_OUTPUT is set. When read is not enabled we render
// a friendly empty state and DO NOT issue summary/events calls.

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faSpinner, faSyncAlt, faExclamationTriangle, faDatabase,
  faCircle, faSearch, faMoon, faInfoCircle, faTable, faLayerGroup,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import { useToast } from '../shared/ToastSystem';

const PAGE_VERSION = '1.0';

// ─── Types (match backend JSON keys exactly) ─────────────────────────────

interface LakeStatus {
  enabled_write: boolean;
  enabled_read: boolean;
  sent: number;
  failed: number;
  dropped: number;
}

interface SummaryRow {
  event_type: string;
  count: number;
}

interface SummaryResponse {
  disabled?: boolean;
  rows: SummaryRow[];
}

// Mirrors internal/analytics/lake_emitter.go Event struct json tags.
interface LakeEvent {
  event_uid: string;
  recipient_send_id: string;
  campaign_id: string;
  subscriber_id: string;
  email: string;
  email_domain: string;
  brand: string;
  isp_group: string;
  route_type: string;
  event_type: string;
  suppression_reason: string;
  vmta: string;
  pool: string;
  bounce_cat: string;
  dsn_code: string;
  dsn_diag: string;
  link_url: string;
  source_ip: string;
  variant: string;
  event_at: string;
  event_epoch_ms: number;
  ingested_at: string;
  source: string;
  dt: string;
}

interface EventsResponse {
  disabled?: boolean;
  events: LakeEvent[];
}

// ─── Style tokens (match Welcome Audience Health / Analytics dark theme) ──

const COLORS = {
  bgDeep:        '#0a0e1a',
  bgPanel:       '#0f1424',
  bgPanelAlt:    '#131a2e',
  border:        'rgba(255,255,255,0.06)',
  borderStrong:  'rgba(255,255,255,0.12)',
  textPrimary:   '#e2e8f0',
  textSecondary: '#94a3b8',
  textMuted:     '#64748b',
  accent:        '#818cf8',
  accentAlt:     '#a78bfa',
  accentPink:    '#f472b6',
  good:          '#34d399',
  warn:          '#fbbf24',
  danger:        '#f87171',
};

const fmt = (n: number) => (n ?? 0).toLocaleString('en-US');

const todayUTC = () => new Date().toISOString().slice(0, 10);
const daysAgoUTC = (n: number) => new Date(Date.now() - n * 86400000).toISOString().slice(0, 10);

// Color hint for an event_type chip.
const eventTypeColor = (t: string): string => {
  const k = (t || '').toLowerCase();
  if (k.includes('hard') || k.includes('bounce') || k.includes('fail') || k.includes('drop')) return COLORS.danger;
  if (k.includes('soft') || k.includes('defer') || k.includes('suppress')) return COLORS.warn;
  if (k.includes('open') || k.includes('click') || k.includes('deliver') || k.includes('sent')) return COLORS.good;
  return COLORS.accent;
};

const truncate = (s: string, max: number): string => {
  if (!s) return '';
  return s.length > max ? s.slice(0, max - 1) + '…' : s;
};

const fmtEventAt = (e: LakeEvent): string => {
  if (e.event_epoch_ms && e.event_epoch_ms > 0) {
    return new Date(e.event_epoch_ms).toLocaleString('en-US', { hour12: false, dateStyle: 'short', timeStyle: 'medium' });
  }
  if (e.event_at) return e.event_at;
  return '—';
};

// ─── Main component ───────────────────────────────────────────────────────

export const EventLakeExplorer: React.FC = () => {
  const { addToast } = useToast();

  const [status, setStatus] = useState<LakeStatus | null>(null);
  const [statusLoading, setStatusLoading] = useState(true);
  const [statusError, setStatusError] = useState('');

  // Summary
  const [from, setFrom] = useState(daysAgoUTC(6));
  const [to, setTo] = useState(todayUTC());
  const [summaryRows, setSummaryRows] = useState<SummaryRow[]>([]);
  const [summaryLoading, setSummaryLoading] = useState(false);

  // Events filters
  const [dt, setDt] = useState('');
  const [campaignId, setCampaignId] = useState('');
  const [ispGroup, setIspGroup] = useState('');
  const [eventType, setEventType] = useState('');
  const [limit, setLimit] = useState(100);
  const [events, setEvents] = useState<LakeEvent[]>([]);
  const [eventsLoading, setEventsLoading] = useState(false);
  const [eventsLoaded, setEventsLoaded] = useState(false);

  const readEnabled = !!status?.enabled_read;

  // ── status ──
  const fetchStatus = useCallback(async () => {
    setStatusLoading(true);
    try {
      const res = await apiFetch('/api/mailing/analytics/lake/status');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const json: LakeStatus = await res.json();
      setStatus(json);
      setStatusError('');
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      setStatusError(msg);
      addToast({ type: 'error', title: 'Event lake status failed', message: msg });
    } finally {
      setStatusLoading(false);
    }
  }, [addToast]);

  // ── summary ──
  const fetchSummary = useCallback(async () => {
    setSummaryLoading(true);
    try {
      const qs = new URLSearchParams({ from, to }).toString();
      const res = await apiFetch(`/api/mailing/analytics/lake/summary?${qs}`);
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const json: SummaryResponse = await res.json();
      setSummaryRows(Array.isArray(json.rows) ? json.rows : []);
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      addToast({ type: 'error', title: 'Event lake summary failed', message: msg });
    } finally {
      setSummaryLoading(false);
    }
  }, [from, to, addToast]);

  // ── events ──
  const fetchEvents = useCallback(async () => {
    setEventsLoading(true);
    try {
      const params = new URLSearchParams();
      if (dt) params.set('dt', dt);
      if (campaignId.trim()) params.set('campaign_id', campaignId.trim());
      if (ispGroup.trim()) params.set('isp_group', ispGroup.trim());
      if (eventType.trim()) params.set('event_type', eventType.trim());
      params.set('limit', String(limit));
      const res = await apiFetch(`/api/mailing/analytics/lake/events?${params.toString()}`);
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const json: EventsResponse = await res.json();
      setEvents(Array.isArray(json.events) ? json.events : []);
      setEventsLoaded(true);
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      addToast({ type: 'error', title: 'Event lake query failed', message: msg });
    } finally {
      setEventsLoading(false);
    }
  }, [dt, campaignId, ispGroup, eventType, limit, addToast]);

  // On mount: status only. Summary/events are gated on read being enabled.
  useEffect(() => { fetchStatus(); }, [fetchStatus]);

  // When read becomes enabled, prime the summary once (do not spam when dark).
  useEffect(() => {
    if (readEnabled) fetchSummary();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [readEnabled]);

  const summaryTotal = useMemo(
    () => summaryRows.reduce((acc, r) => acc + (r.count || 0), 0),
    [summaryRows]
  );

  // ── Loading shell (initial status fetch) ──
  if (statusLoading && !status) {
    return (
      <div style={styles.loadingShell}>
        <FontAwesomeIcon icon={faSpinner} spin /> Loading event lake status…
      </div>
    );
  }

  return (
    <div style={styles.page}>
      {/* ─── Header ──────────────────────────────────────────────── */}
      <div style={styles.header}>
        <div>
          <h1 style={styles.title}>
            <FontAwesomeIcon icon={faDatabase} style={{ color: COLORS.accent, marginRight: 10 }} />
            Event Lake Explorer
          </h1>
          <p style={styles.subtitle}>
            Read-only window into the S3 / Athena email-event lake — per-recipient sends, bounces,
            suppressions, opens and clicks fanned out via Firehose to <code style={styles.code}>ignite_analytics.email_events</code>.
          </p>
        </div>
        <button style={styles.refreshBtn} onClick={fetchStatus} disabled={statusLoading}>
          <FontAwesomeIcon icon={statusLoading ? faSpinner : faSyncAlt} spin={statusLoading} /> Refresh status
        </button>
      </div>

      {statusError && (
        <div style={styles.errorShell}>
          <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 8 }} />
          Failed to load status: {statusError}
          <button style={styles.inlineRetry} onClick={fetchStatus}>Retry</button>
        </div>
      )}

      {/* ─── Status strip ───────────────────────────────────────── */}
      {status && (
        <div style={styles.statusStrip}>
          <EnableBadge label="Write" enabled={status.enabled_write} />
          <EnableBadge label="Read" enabled={status.enabled_read} />
          <div style={styles.statusDivider} />
          <Counter label="Sent" value={status.sent} color={COLORS.good} />
          <Counter label="Failed" value={status.failed} color={COLORS.danger} />
          <Counter label="Dropped" value={status.dropped} color={COLORS.warn} />
        </div>
      )}

      {/* ─── DARK empty state ──────────────────────────────────── */}
      {status && !readEnabled && (
        <div style={styles.darkCard}>
          <FontAwesomeIcon icon={faMoon} style={{ fontSize: 28, color: COLORS.accentAlt }} />
          <div style={{ flex: 1 }}>
            <div style={styles.darkTitle}>Event lake read layer is dark</div>
            <div style={styles.darkBody}>
              The Athena-backed read layer is not configured. Set <code style={styles.code}>ANALYTICS_ATHENA_OUTPUT</code> (the
              S3 Athena results location) on the server to enable summary &amp; event queries. Until then this screen shows
              the write-side emitter counters above; query controls stay disabled.
            </div>
            <div style={styles.darkHint}>
              <FontAwesomeIcon icon={faInfoCircle} style={{ color: COLORS.textMuted, marginRight: 6 }} />
              Write side {status.enabled_write ? 'is emitting events to Firehose' : 'is also dark (ANALYTICS_FIREHOSE_STREAM unset)'}.
            </div>
          </div>
        </div>
      )}

      {/* ─── Summary + Events (only when read enabled) ─────────── */}
      {status && readEnabled && (
        <>
          {/* Summary */}
          <div style={styles.panel}>
            <div style={styles.panelHeader}>
              <div>
                <h2 style={styles.panelTitle}>
                  <FontAwesomeIcon icon={faLayerGroup} style={{ marginRight: 8, color: COLORS.accentAlt }} />
                  Event Counts by Type
                </h2>
                <p style={styles.panelSubtitle}>
                  Aggregated over <strong style={{ color: COLORS.textSecondary }}>dt</strong> partitions in the selected range (inclusive).
                </p>
              </div>
              <div style={styles.filterRow}>
                <label style={styles.fieldLabel}>From
                  <input type="date" value={from} max={to} onChange={e => setFrom(e.target.value)} style={styles.input} />
                </label>
                <label style={styles.fieldLabel}>To
                  <input type="date" value={to} min={from} max={todayUTC()} onChange={e => setTo(e.target.value)} style={styles.input} />
                </label>
                <button style={styles.primaryBtn} onClick={fetchSummary} disabled={summaryLoading}>
                  <FontAwesomeIcon icon={summaryLoading ? faSpinner : faSearch} spin={summaryLoading} /> Run
                </button>
              </div>
            </div>

            {summaryLoading ? (
              <div style={styles.sectionLoading}><FontAwesomeIcon icon={faSpinner} spin /> Querying Athena…</div>
            ) : summaryRows.length === 0 ? (
              <div style={styles.sectionEmpty}>No events in this range.</div>
            ) : (
              <div style={styles.summaryGrid}>
                {summaryRows.map((r) => {
                  const c = eventTypeColor(r.event_type);
                  const pct = summaryTotal > 0 ? (r.count / summaryTotal) * 100 : 0;
                  return (
                    <div key={r.event_type || 'unknown'} style={styles.summaryCard}>
                      <div style={styles.summaryType}>
                        <FontAwesomeIcon icon={faCircle} style={{ color: c, fontSize: 8, marginRight: 6 }} />
                        {r.event_type || '(none)'}
                      </div>
                      <div style={{ ...styles.summaryCount, color: c }}>{fmt(r.count)}</div>
                      <div style={styles.summaryPct}>{pct.toFixed(1)}% of {fmt(summaryTotal)}</div>
                    </div>
                  );
                })}
              </div>
            )}
          </div>

          {/* Events */}
          <div style={styles.panel}>
            <div style={styles.panelHeader}>
              <div>
                <h2 style={styles.panelTitle}>
                  <FontAwesomeIcon icon={faTable} style={{ marginRight: 8, color: COLORS.accentPink }} />
                  Recent Events
                </h2>
                <p style={styles.panelSubtitle}>
                  Newest first. Filters are validated server-side (dt = YYYY-MM-DD, campaign_id = UUID,
                  isp_group / event_type = tokens). Limit clamps to 1–1000.
                </p>
              </div>
            </div>

            <div style={styles.eventFilterBar}>
              <label style={styles.fieldLabel}>dt (day)
                <input type="date" value={dt} max={todayUTC()} onChange={e => setDt(e.target.value)} style={styles.input} />
              </label>
              <label style={styles.fieldLabel}>campaign_id
                <input type="text" value={campaignId} placeholder="UUID" onChange={e => setCampaignId(e.target.value)} style={{ ...styles.input, width: 280 }} />
              </label>
              <label style={styles.fieldLabel}>isp_group
                <input type="text" value={ispGroup} placeholder="gmail" onChange={e => setIspGroup(e.target.value)} style={{ ...styles.input, width: 120 }} />
              </label>
              <label style={styles.fieldLabel}>event_type
                <input type="text" value={eventType} placeholder="delivered" onChange={e => setEventType(e.target.value)} style={{ ...styles.input, width: 140 }} />
              </label>
              <label style={styles.fieldLabel}>limit
                <input
                  type="number" min={1} max={1000} value={limit}
                  onChange={e => setLimit(Math.max(1, Math.min(1000, Number(e.target.value) || 1)))}
                  style={{ ...styles.input, width: 90 }}
                />
              </label>
              <button style={styles.primaryBtn} onClick={fetchEvents} disabled={eventsLoading}>
                <FontAwesomeIcon icon={eventsLoading ? faSpinner : faSearch} spin={eventsLoading} /> Query
              </button>
            </div>

            {eventsLoading ? (
              <div style={styles.sectionLoading}><FontAwesomeIcon icon={faSpinner} spin /> Querying Athena…</div>
            ) : !eventsLoaded ? (
              <div style={styles.sectionEmpty}>Set filters (optional) and run a query to load recent events.</div>
            ) : events.length === 0 ? (
              <div style={styles.sectionEmpty}>No events matched these filters.</div>
            ) : (
              <div style={styles.tableWrap}>
                <table style={styles.table}>
                  <thead>
                    <tr>
                      <th style={{ ...styles.th, textAlign: 'left' }}>event_at</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>event_type</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>brand</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>isp_group</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>email_domain</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>campaign_id</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>bounce_cat</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>dsn_code</th>
                      <th style={{ ...styles.th, textAlign: 'left' }}>link_url</th>
                    </tr>
                  </thead>
                  <tbody>
                    {events.map((e, i) => (
                      <tr key={e.event_uid || `${i}`} style={styles.tr}>
                        <td style={{ ...styles.td, whiteSpace: 'nowrap' }}>{fmtEventAt(e)}</td>
                        <td style={styles.td}>
                          <span style={{ ...styles.typeChip, color: eventTypeColor(e.event_type), borderColor: eventTypeColor(e.event_type) + '55' }}>
                            {e.event_type || '—'}
                          </span>
                        </td>
                        <td style={styles.td}>{e.brand || '—'}</td>
                        <td style={styles.td}>{e.isp_group || '—'}</td>
                        <td style={styles.td}>{e.email_domain || '—'}</td>
                        <td style={{ ...styles.td, fontFamily: 'monospace', fontSize: 11 }} title={e.campaign_id}>
                          {e.campaign_id ? truncate(e.campaign_id, 14) : '—'}
                        </td>
                        <td style={styles.td}>{e.bounce_cat || '—'}</td>
                        <td style={styles.td}>{e.dsn_code || '—'}</td>
                        <td style={{ ...styles.td, maxWidth: 240 }} title={e.link_url}>
                          {e.link_url ? truncate(e.link_url, 40) : '—'}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
                <div style={styles.tableFooterNote}>
                  Showing {events.length} event{events.length === 1 ? '' : 's'} (limit {limit}).
                </div>
              </div>
            )}
          </div>
        </>
      )}

      {/* ─── Footer / version stripe ───────────────────────────── */}
      <div style={styles.footer}>
        <span>Page: Event Lake Explorer v{PAGE_VERSION}</span>
        <span>Source: s3://ignite-analytics-lake → ignite_analytics.email_events</span>
        <span>Read {readEnabled ? 'enabled' : 'dark'}</span>
      </div>
    </div>
  );
};

// ─── Sub-components ──────────────────────────────────────────────────────

const EnableBadge: React.FC<{ label: string; enabled: boolean }> = ({ label, enabled }) => (
  <div style={{
    ...styles.badge,
    color: enabled ? COLORS.good : COLORS.textMuted,
    borderColor: enabled ? COLORS.good + '55' : COLORS.borderStrong,
    background: enabled ? COLORS.good + '12' : 'transparent',
  }}>
    <FontAwesomeIcon icon={faCircle} style={{ fontSize: 8 }} />
    {label}: {enabled ? 'on' : 'off'}
  </div>
);

const Counter: React.FC<{ label: string; value: number; color: string }> = ({ label, value, color }) => (
  <div style={styles.counter}>
    <div style={{ ...styles.counterValue, color }}>{fmt(value)}</div>
    <div style={styles.counterLabel}>{label}</div>
  </div>
);

// ─── Styles ─────────────────────────────────────────────────────────────

const styles: Record<string, React.CSSProperties> = {
  page: {
    padding: '24px 28px 64px',
    background: COLORS.bgDeep,
    minHeight: '100vh',
    color: COLORS.textPrimary,
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif',
  },
  loadingShell: {
    display: 'flex', alignItems: 'center', justifyContent: 'center',
    height: '50vh', color: COLORS.textSecondary, gap: 10, fontSize: 14,
    background: COLORS.bgDeep,
  },
  errorShell: {
    margin: '0 0 20px', padding: 16, background: COLORS.bgPanel, borderRadius: 8,
    color: COLORS.danger, display: 'flex', alignItems: 'center', gap: 8,
    border: `1px solid ${COLORS.danger}33`,
  },
  inlineRetry: {
    marginLeft: 'auto', background: 'transparent', color: COLORS.danger,
    border: `1px solid ${COLORS.danger}55`, padding: '4px 12px',
    borderRadius: 6, fontSize: 12, cursor: 'pointer',
  },
  header: {
    display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start',
    marginBottom: 20, gap: 16,
  },
  title: {
    margin: 0, fontSize: 24, fontWeight: 700, color: COLORS.textPrimary,
    letterSpacing: -0.3, display: 'flex', alignItems: 'center',
  },
  subtitle: {
    margin: '6px 0 0', fontSize: 13, color: COLORS.textSecondary, maxWidth: 760, lineHeight: 1.5,
  },
  code: {
    fontFamily: 'monospace', fontSize: 12, color: COLORS.accentAlt,
    background: 'rgba(167,139,250,0.1)', padding: '1px 6px', borderRadius: 4,
  },
  refreshBtn: {
    background: COLORS.bgPanel, color: COLORS.textPrimary,
    border: `1px solid ${COLORS.borderStrong}`, padding: '8px 16px',
    borderRadius: 6, fontSize: 13, cursor: 'pointer',
    display: 'flex', alignItems: 'center', gap: 8, height: 36, whiteSpace: 'nowrap',
  },
  statusStrip: {
    display: 'flex', alignItems: 'center', gap: 16, flexWrap: 'wrap',
    padding: '14px 18px', background: COLORS.bgPanel,
    border: `1px solid ${COLORS.border}`, borderRadius: 10, marginBottom: 20,
  },
  statusDivider: {
    width: 1, height: 32, background: COLORS.borderStrong, margin: '0 4px',
  },
  badge: {
    display: 'inline-flex', alignItems: 'center', gap: 6,
    padding: '5px 12px', borderRadius: 999, fontSize: 12, fontWeight: 600,
    border: '1px solid', textTransform: 'uppercase', letterSpacing: 0.4,
  },
  counter: { display: 'flex', flexDirection: 'column', alignItems: 'flex-start' },
  counterValue: { fontSize: 22, fontWeight: 700, lineHeight: 1, fontVariantNumeric: 'tabular-nums' },
  counterLabel: {
    fontSize: 10, color: COLORS.textMuted, textTransform: 'uppercase',
    letterSpacing: 0.6, marginTop: 4,
  },
  darkCard: {
    display: 'flex', gap: 18, alignItems: 'flex-start',
    padding: 24, background: COLORS.bgPanel,
    border: `1px solid ${COLORS.borderStrong}`, borderRadius: 12,
    marginBottom: 24,
  },
  darkTitle: { fontSize: 16, fontWeight: 700, color: COLORS.textPrimary, marginBottom: 6 },
  darkBody: { fontSize: 13, color: COLORS.textSecondary, lineHeight: 1.6, maxWidth: 760 },
  darkHint: { fontSize: 12, color: COLORS.textMuted, marginTop: 12 },
  panel: {
    background: COLORS.bgPanel,
    border: `1px solid ${COLORS.border}`, borderRadius: 10,
    padding: 20, marginBottom: 20,
  },
  panelHeader: {
    display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start',
    marginBottom: 16, gap: 16, flexWrap: 'wrap',
  },
  panelTitle: { margin: 0, fontSize: 16, fontWeight: 600, color: COLORS.textPrimary },
  panelSubtitle: { margin: '4px 0 0', fontSize: 12, color: COLORS.textSecondary, maxWidth: 720, lineHeight: 1.5 },
  filterRow: { display: 'flex', alignItems: 'flex-end', gap: 10, flexWrap: 'wrap' },
  eventFilterBar: {
    display: 'flex', alignItems: 'flex-end', gap: 12, flexWrap: 'wrap',
    padding: '14px 16px', background: COLORS.bgPanelAlt,
    border: `1px solid ${COLORS.border}`, borderRadius: 8, marginBottom: 16,
  },
  fieldLabel: {
    display: 'flex', flexDirection: 'column', gap: 4,
    fontSize: 10, color: COLORS.textMuted, textTransform: 'uppercase', letterSpacing: 0.5,
  },
  input: {
    background: COLORS.bgDeep, color: COLORS.textPrimary,
    border: `1px solid ${COLORS.borderStrong}`, borderRadius: 6,
    padding: '7px 10px', fontSize: 13, outline: 'none',
  },
  primaryBtn: {
    background: COLORS.accent, color: '#0a0e1a',
    border: 'none', padding: '8px 16px', borderRadius: 6,
    fontSize: 13, fontWeight: 600, cursor: 'pointer',
    display: 'flex', alignItems: 'center', gap: 8, height: 34,
  },
  sectionLoading: {
    display: 'flex', alignItems: 'center', justifyContent: 'center',
    gap: 8, padding: '32px 0', color: COLORS.textSecondary, fontSize: 13,
  },
  sectionEmpty: {
    padding: '32px 0', textAlign: 'center', color: COLORS.textMuted, fontSize: 13,
  },
  summaryGrid: {
    display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(180px, 1fr))', gap: 12,
  },
  summaryCard: {
    padding: 14, background: COLORS.bgPanelAlt,
    border: `1px solid ${COLORS.border}`, borderRadius: 8,
  },
  summaryType: {
    fontSize: 12, color: COLORS.textSecondary, display: 'flex', alignItems: 'center',
    fontFamily: 'monospace',
  },
  summaryCount: { fontSize: 26, fontWeight: 700, marginTop: 6, fontVariantNumeric: 'tabular-nums' },
  summaryPct: { fontSize: 11, color: COLORS.textMuted, marginTop: 2 },
  tableWrap: { overflowX: 'auto' },
  table: { width: '100%', borderCollapse: 'collapse', fontSize: 13 },
  th: {
    padding: '8px 12px', color: COLORS.textMuted,
    fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.5,
    borderBottom: `1px solid ${COLORS.borderStrong}`, fontWeight: 600, whiteSpace: 'nowrap',
  },
  tr: { borderBottom: `1px solid ${COLORS.border}` },
  td: {
    padding: '9px 12px', color: COLORS.textPrimary, textAlign: 'left',
    overflow: 'hidden', textOverflow: 'ellipsis',
  },
  typeChip: {
    display: 'inline-block', padding: '2px 8px', borderRadius: 999,
    border: '1px solid', fontSize: 11, fontWeight: 600, whiteSpace: 'nowrap',
  },
  tableFooterNote: { padding: '10px 4px 0', fontSize: 11, color: COLORS.textMuted },
  footer: {
    marginTop: 32, padding: '8px 4px',
    fontSize: 11, color: COLORS.textMuted,
    borderTop: `1px solid ${COLORS.border}`,
    display: 'flex', gap: 16, flexWrap: 'wrap',
  },
};

export default EventLakeExplorer;
