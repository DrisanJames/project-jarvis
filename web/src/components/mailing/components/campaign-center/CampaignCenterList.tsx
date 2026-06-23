import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faSync, faSearch, faSpinner, faExclamationTriangle, faLayerGroup,
  faChevronDown, faChevronRight, faSort, faSortUp, faSortDown, faClock,
  faEnvelope, faTags,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../../shared/apiFetch';
import {
  CampaignIdentityRow, ListEntry, TagEntry, ListMetricsResponse, ListMetrics,
  EMPTY_METRICS, DatePreset, DATE_PRESET_LABELS, datePresetRange,
  parseOfferFromName, isDripIdentityRow, fmtDenverTime, fmtWindow, numFmt, pctStr,
} from './types';
import CampaignExpand from './CampaignExpand';

// CampaignCenterList — the Round-3 §1 list rebuild. One sortable/filterable
// row per campaign; rows expand INLINE (accordion, multiple open) into the
// CampaignExpand analytics panel. Identity/status come from the pre-existing
// /analytics/campaign-summary rows (identity ONLY — its counter metrics are
// never rendered); per-row metrics come from /campaigns/list-metrics, ONE
// grouped tracking-event scan per visible page (≤50 ids). Drip campaigns are
// hidden by default; the toggle shows them ROLLED UP per partner_drip tag
// (never individual micro-campaigns). Group-by-offer mode rolls rows up per
// parsed offer with top subject lines ranked by human CTR.

const PAGE_SIZE = 50;

type SortKey =
  | 'name' | 'offer' | 'domain' | 'status' | 'first_wave' | 'planned'
  | 'attempted' | 'delivered' | 'opens' | 'clicks' | 'unsubs' | 'hard' | 'soft' | 'complaints';

type SortDir = 'asc' | 'desc';

export type ListStatusFilter = 'all' | 'draft' | 'scheduled' | 'sending' | 'completed' | 'paused' | 'failed';

const COMPLETED = ['sent', 'completed', 'completed_with_errors'];

const matchesStatus = (status: string, f: ListStatusFilter): boolean => {
  if (f === 'all') return true;
  if (f === 'completed') return COMPLETED.includes(status);
  return status === f;
};

const STATUS_COLORS: Record<string, string> = {
  draft: '#94a3b8', scheduled: '#60a5fa', preparing: '#22d3ee', sending: '#22d3ee',
  paused: '#f59e0b', sent: '#22c55e', completed: '#22c55e',
  completed_with_errors: '#f59e0b', cancelled: '#64748b', failed: '#ef4444',
};

const inputStyle: React.CSSProperties = {
  background: 'rgba(15,22,41,0.9)', border: '1px solid rgba(99,102,241,0.2)',
  borderRadius: 7, color: '#e0e6f0', padding: '6px 10px', fontSize: 12.5,
};

const thStyle: React.CSSProperties = {
  padding: '7px 8px', fontSize: 10.5, textTransform: 'uppercase', letterSpacing: 0.4,
  color: 'rgba(180,210,240,0.5)', fontWeight: 700, whiteSpace: 'nowrap',
  cursor: 'pointer', userSelect: 'none', textAlign: 'right',
};

const tdNum: React.CSSProperties = {
  padding: '6px 8px', fontSize: 12.5, color: '#cbd5e1', textAlign: 'right', whiteSpace: 'nowrap',
};

interface DisplayRow {
  key: string;            // campaign id or "tag:<partner_tag>"
  identity: CampaignIdentityRow;
  isRollup: boolean;
  tag?: string;
  offer: string;
  domain: string;
  firstWaveAt?: string;
  windowStart?: string;
  windowEnd?: string;
  planned: number;
  subject?: string;
  enriched: boolean;
  metrics: ListMetrics;
}

const StatusPill: React.FC<{ status: string }> = ({ status }) => {
  const color = STATUS_COLORS[status] || '#94a3b8';
  return (
    <span style={{
      fontSize: 10.5, fontWeight: 700, letterSpacing: 0.3, padding: '2px 8px',
      borderRadius: 10, border: `1px solid ${color}55`, background: `${color}1a`, color,
      whiteSpace: 'nowrap',
    }}>
      {status.replace(/_/g, ' ')}
    </span>
  );
};

export const CampaignCenterList: React.FC<{
  rows: CampaignIdentityRow[];
  loading: boolean;
  error?: string | null;
  dataAsOf?: string | null;
  statusFilter: ListStatusFilter;
  onStatusFilterChange: (f: ListStatusFilter) => void;
  onOpenModal: (id: string) => void;
  onRefresh: () => void;
}> = ({ rows, loading, error, dataAsOf, statusFilter, onStatusFilterChange, onOpenModal, onRefresh }) => {
  const [datePreset, setDatePreset] = useState<DatePreset>('today');
  const [domainFilter, setDomainFilter] = useState<string>('all');
  const [offerText, setOfferText] = useState('');
  const [showDrips, setShowDrips] = useState(false); // default OFF per spec
  const [groupByOffer, setGroupByOffer] = useState(false);
  const [sortKey, setSortKey] = useState<SortKey>('first_wave');
  const [sortDir, setSortDir] = useState<SortDir>('desc');
  const [page, setPage] = useState(0);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [expandedGroups, setExpandedGroups] = useState<Set<string>>(new Set());

  // Enrichment caches: per-campaign entries + per-drip-tag entries (tag
  // metrics are date-range-scoped, so the cache key includes the preset).
  const [entries, setEntries] = useState<Record<string, ListEntry>>({});
  const [tagEntries, setTagEntries] = useState<Record<string, TagEntry>>({});
  const [enriching, setEnriching] = useState(false);
  const [enrichNote, setEnrichNote] = useState<string | null>(null);
  const inflightIds = useRef<Set<string>>(new Set());
  const inflightTags = useRef<Set<string>>(new Set());

  const range = useMemo(() => datePresetRange(datePreset), [datePreset]);

  // ── filtering (identity-level) ─────────────────────────────────────────────
  const baseFiltered = useMemo(() => {
    const fromMs = range.from.getTime();
    const toMs = range.to.getTime();
    const q = offerText.trim().toLowerCase();
    return rows.filter(r => {
      const isDrip = isDripIdentityRow(r);
      if (!showDrips && isDrip) return false;             // drips hidden by default
      if (showDrips && isDrip && !r.is_rollup) return false; // never individual micro-campaigns
      if (!matchesStatus(r.status, statusFilter)) return false;
      const ts = r.scheduled_at ? new Date(r.scheduled_at).getTime()
        : r.updated_at ? new Date(r.updated_at).getTime() : NaN;
      if (!Number.isNaN(ts) && (ts < fromMs || ts >= toMs)) return false;
      if (q) {
        const offer = parseOfferFromName(r.name).toLowerCase();
        if (!offer.includes(q) && !r.name.toLowerCase().includes(q)) return false;
      }
      return true;
    });
  }, [rows, statusFilter, range, offerText, showDrips]);

  // ── enrichment: ONE batched call per page of visible ids ─────────────────
  const tagCacheKey = useCallback((tag: string) => `${tag}|${datePreset}`, [datePreset]);

  const fetchEnrichment = useCallback(async (ids: string[], tags: string[]) => {
    if (ids.length === 0 && tags.length === 0) return;
    ids.forEach(i => inflightIds.current.add(i));
    tags.forEach(t => inflightTags.current.add(t));
    setEnriching(true);
    try {
      const params = new URLSearchParams();
      if (ids.length > 0) params.set('ids', ids.join(','));
      if (tags.length > 0) {
        params.set('tags', tags.join(','));
        params.set('from', range.from.toISOString());
        params.set('to', range.to.toISOString());
      }
      const res = await apiFetch(`/api/mailing/campaigns/list-metrics?${params.toString()}`);
      const data: ListMetricsResponse = await res.json();
      if (data.error) throw new Error(data.error);
      // Cache EVERY requested key — ids/tags missing from the response get an
      // empty sentinel so the effect never refetches them in a loop.
      setEntries(prev => {
        const next = { ...prev };
        ids.forEach(id => {
          next[id] = (data.campaigns && data.campaigns[id]) || {
            subject: '', sending_domain: '', route_type: '', planned: 0,
            wave_count: 0, metrics: { ...EMPTY_METRICS },
          };
        });
        return next;
      });
      if (tags.length > 0) {
        setTagEntries(prev => {
          const next = { ...prev };
          tags.forEach(tag => {
            next[tagCacheKey(tag)] = (data.tags && data.tags[tag]) || {
              wave_count: 0, planned: 0, route_type: '', metrics: { ...EMPTY_METRICS },
            };
          });
          return next;
        });
      }
      setEnrichNote(null);
    } catch (e) {
      setEnrichNote(e instanceof Error ? e.message : 'metrics fetch failed');
    } finally {
      ids.forEach(i => inflightIds.current.delete(i));
      tags.forEach(t => inflightTags.current.delete(t));
      setEnriching(false);
    }
  }, [range, tagCacheKey]);

  // Which ids need metrics: the visible page normally; the whole filtered set
  // when a domain filter or offer grouping needs every row (chunked ≤50/call).
  useEffect(() => {
    const needAll = domainFilter !== 'all' || groupByOffer;
    const candidates = needAll ? baseFiltered : baseFiltered.slice(page * PAGE_SIZE, (page + 1) * PAGE_SIZE);
    const missingIds = candidates
      .filter(r => !r.is_rollup && !entries[r.id] && !inflightIds.current.has(r.id))
      .map(r => r.id);
    const missingTags = candidates
      .filter(r => !!r.is_rollup && !!r.partner_tag && !tagEntries[tagCacheKey(r.partner_tag)] && !inflightTags.current.has(r.partner_tag))
      .map(r => r.partner_tag as string);
    if (missingIds.length === 0 && missingTags.length === 0) return;
    // chunk ids ≤50 per request (page-size aligned)
    const chunks: Array<{ ids: string[]; tags: string[] }> = [];
    for (let i = 0; i < Math.max(1, Math.ceil(missingIds.length / PAGE_SIZE)); i++) {
      chunks.push({ ids: missingIds.slice(i * PAGE_SIZE, (i + 1) * PAGE_SIZE), tags: i === 0 ? missingTags.slice(0, 25) : [] });
    }
    chunks.forEach(ch => { void fetchEnrichment(ch.ids, ch.tags); });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [baseFiltered, page, domainFilter, groupByOffer, entries, tagEntries, fetchEnrichment, tagCacheKey]);

  // ── build display rows (identity + enrichment merged) ─────────────────────
  const displayRows = useMemo<DisplayRow[]>(() => baseFiltered.map(r => {
    if (r.is_rollup && r.partner_tag) {
      const te: TagEntry | undefined = tagEntries[tagCacheKey(r.partner_tag)];
      return {
        key: `tag:${r.partner_tag}`,
        identity: r,
        isRollup: true,
        tag: r.partner_tag,
        offer: parseOfferFromName(r.name),
        domain: '—',
        firstWaveAt: te?.first_wave_at || r.scheduled_at,
        windowStart: te?.first_wave_at,
        windowEnd: te?.last_wave_at,
        planned: te?.planned ?? 0,
        enriched: !!te,
        metrics: te?.metrics || EMPTY_METRICS,
      };
    }
    const e: ListEntry | undefined = entries[r.id];
    return {
      key: r.id,
      identity: r,
      isRollup: false,
      offer: parseOfferFromName(r.name),
      domain: e?.sending_domain || '',
      firstWaveAt: e?.first_wave_at || r.scheduled_at,
      windowStart: e?.window_start,
      windowEnd: e?.window_end,
      planned: e?.planned ?? r.targeted ?? 0,
      subject: e?.subject,
      enriched: !!e,
      metrics: e?.metrics || EMPTY_METRICS,
    };
  }), [baseFiltered, entries, tagEntries, tagCacheKey]);

  const domainOptions = useMemo(() => {
    const set = new Set<string>();
    displayRows.forEach(r => { if (r.domain && r.domain !== '—') set.add(r.domain); });
    return Array.from(set).sort();
  }, [displayRows]);

  const domainFiltered = useMemo(() => (
    domainFilter === 'all' ? displayRows : displayRows.filter(r => r.domain === domainFilter)
  ), [displayRows, domainFilter]);

  // ── sorting ────────────────────────────────────────────────────────────────
  const sorted = useMemo(() => {
    const val = (r: DisplayRow): string | number => {
      switch (sortKey) {
        case 'name': return r.identity.name.toLowerCase();
        case 'offer': return r.offer.toLowerCase();
        case 'domain': return r.domain;
        case 'status': return r.identity.status;
        case 'first_wave': return r.firstWaveAt ? new Date(r.firstWaveAt).getTime() : 0;
        case 'planned': return r.planned;
        case 'attempted': return r.metrics.attempted;
        case 'delivered': return r.metrics.delivered;
        case 'opens': return r.metrics.human_opens;
        case 'clicks': return r.metrics.human_clicks;
        case 'unsubs': return r.metrics.unsubscribes;
        case 'hard': return r.metrics.hard_bounces;
        case 'soft': return r.metrics.soft_bounces;
        case 'complaints': return r.metrics.complaints;
        default: return 0;
      }
    };
    const dir = sortDir === 'asc' ? 1 : -1;
    return [...domainFiltered].sort((a, b) => {
      const va = val(a); const vb = val(b);
      if (va < vb) return -1 * dir;
      if (va > vb) return 1 * dir;
      return 0;
    });
  }, [domainFiltered, sortKey, sortDir]);

  const pageCount = Math.max(1, Math.ceil(sorted.length / PAGE_SIZE));
  const safePage = Math.min(page, pageCount - 1);
  const pageRows = sorted.slice(safePage * PAGE_SIZE, (safePage + 1) * PAGE_SIZE);

  useEffect(() => { setPage(0); }, [statusFilter, datePreset, domainFilter, offerText, showDrips, groupByOffer]);

  const toggleSort = (k: SortKey) => {
    if (sortKey === k) setSortDir(d => (d === 'asc' ? 'desc' : 'asc'));
    else { setSortKey(k); setSortDir(k === 'name' || k === 'offer' || k === 'domain' || k === 'status' ? 'asc' : 'desc'); }
  };

  const toggleExpand = (key: string, expandable: boolean) => {
    if (!expandable) return;
    setExpanded(prev => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key); else next.add(key);
      return next;
    });
  };

  // ── group-by-offer rollups ────────────────────────────────────────────────
  interface OfferGroup {
    offer: string;
    members: DisplayRow[];
    metrics: ListMetrics;
    planned: number;
    subjects: Array<{ subject: string; delivered: number; clicks: number; ctr: number }>;
  }
  const offerGroups = useMemo<OfferGroup[]>(() => {
    if (!groupByOffer) return [];
    const map = new Map<string, OfferGroup>();
    sorted.forEach(r => {
      let g = map.get(r.offer);
      if (!g) {
        g = { offer: r.offer, members: [], metrics: { ...EMPTY_METRICS }, planned: 0, subjects: [] };
        map.set(r.offer, g);
      }
      g.members.push(r);
      g.planned += r.planned;
      (Object.keys(EMPTY_METRICS) as Array<keyof ListMetrics>).forEach(k => { g!.metrics[k] += r.metrics[k]; });
    });
    map.forEach(g => {
      const bySubject = new Map<string, { delivered: number; clicks: number }>();
      g.members.forEach(m => {
        const s = (m.subject || '').trim();
        if (!s) return;
        const agg = bySubject.get(s) || { delivered: 0, clicks: 0 };
        agg.delivered += m.metrics.delivered;
        agg.clicks += m.metrics.human_clicks;
        bySubject.set(s, agg);
      });
      g.subjects = Array.from(bySubject.entries())
        .map(([subject, a]) => ({ subject, delivered: a.delivered, clicks: a.clicks, ctr: a.delivered > 0 ? (a.clicks / a.delivered) * 100 : 0 }))
        .filter(s => s.delivered > 0)
        .sort((a, b) => b.ctr - a.ctr)
        .slice(0, 5);
    });
    return Array.from(map.values()).sort((a, b) => b.metrics.delivered - a.metrics.delivered);
  }, [groupByOffer, sorted]);

  const SortIcon: React.FC<{ k: SortKey }> = ({ k }) => (
    <FontAwesomeIcon
      icon={sortKey !== k ? faSort : sortDir === 'asc' ? faSortUp : faSortDown}
      style={{ marginLeft: 4, opacity: sortKey === k ? 0.9 : 0.3, fontSize: 9 }}
    />
  );

  const metricCells = (r: { metrics: ListMetrics; enriched: boolean }) => {
    const m = r.metrics;
    const dim = !r.enriched;
    const cell = (v: number, color?: string, tip?: string): React.ReactNode => (
      <span title={tip} style={{ color: dim ? 'rgba(148,163,184,0.35)' : color, fontVariantNumeric: 'tabular-nums' }}>
        {dim ? '…' : numFmt(v)}
      </span>
    );
    return (
      <>
        <td style={tdNum}>{cell(m.attempted, undefined, 'Attempted (sent events)')}</td>
        <td style={tdNum}>{cell(m.delivered, '#22c55e', `Delivered (${pctStr(m.delivered, Math.max(m.attempted, m.delivered + m.hard_bounces + m.soft_bounces))} of attempted)`)}</td>
        <td style={tdNum}>{cell(m.human_opens, '#38bdf8', `Human opens, unique (${pctStr(m.human_opens, m.delivered)} of delivered)`)}</td>
        <td style={tdNum}>{cell(m.human_clicks, '#c084fc', `Human clicks, unique (${pctStr(m.human_clicks, m.delivered)} of delivered)`)}</td>
        <td style={tdNum}>{cell(m.unsubscribes)}</td>
        <td style={tdNum}>{cell(m.hard_bounces, m.hard_bounces > 0 ? '#ef4444' : undefined, 'Hard bounces (list hygiene) — always split from soft')}</td>
        <td style={tdNum}>{cell(m.soft_bounces, m.soft_bounces > 0 ? '#f59e0b' : undefined, 'Soft bounces (transient)')}</td>
        <td style={tdNum}>{cell(m.complaints, m.complaints > 0 ? '#ef4444' : undefined)}</td>
      </>
    );
  };

  const COL_COUNT = 15;

  const renderCampaignRow = (r: DisplayRow, indent = false) => {
    const isOpen = expanded.has(r.key);
    const expandable = !r.isRollup;
    return (
      <React.Fragment key={r.key}>
        <tr
          onClick={() => toggleExpand(r.key, expandable)}
          style={{
            borderTop: '1px solid rgba(99,102,241,0.08)',
            cursor: expandable ? 'pointer' : 'default',
            background: isOpen ? 'rgba(99,102,241,0.07)' : undefined,
          }}
        >
          <td style={{ padding: '6px 4px 6px 8px', width: 18 }}>
            {expandable ? (
              <FontAwesomeIcon icon={isOpen ? faChevronDown : faChevronRight} style={{ fontSize: 10, color: '#6366f1' }} />
            ) : (
              <FontAwesomeIcon icon={faLayerGroup} title="Automated follow-ups grouped — individual sends aggregated; no inline expand" style={{ fontSize: 10, color: '#f59e0b' }} />
            )}
          </td>
          <td style={{ padding: '6px 8px', maxWidth: 320, paddingLeft: indent ? 24 : 8 }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
              <span
                title={r.identity.name}
                style={{ fontSize: 12.5, fontWeight: 600, color: '#e0e6f0', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
              >
                {r.identity.name}
              </span>
              {r.isRollup && (
                <span style={{
                  fontSize: 9.5, fontWeight: 700, padding: '1px 6px', borderRadius: 6,
                  border: '1px solid #f59e0b55', background: '#f59e0b1a', color: '#f59e0b', whiteSpace: 'nowrap',
                }}>
                  ROLLUP · {numFmt(r.identity.wave_count || 0)}
                </span>
              )}
            </div>
          </td>
          <td style={{ padding: '6px 8px', fontSize: 12, color: '#a5b4fc', whiteSpace: 'nowrap', maxWidth: 160, overflow: 'hidden', textOverflow: 'ellipsis' }} title={r.offer}>
            {r.offer}
          </td>
          <td style={{ padding: '6px 8px', fontSize: 12, color: '#94a3b8', whiteSpace: 'nowrap' }}>
            {r.enriched || r.isRollup ? (r.domain || '—') : '…'}
          </td>
          <td style={{ padding: '6px 8px' }}><StatusPill status={r.identity.status} /></td>
          <td style={{ ...tdNum, textAlign: 'left' }} title="First send (Denver)">{fmtDenverTime(r.firstWaveAt)}</td>
          <td style={{ ...tdNum, textAlign: 'left' }} title="Send window (Denver)">{fmtWindow(r.windowStart, r.windowEnd)}</td>
          <td style={tdNum} title="Planned recipients (operator plan)">{numFmt(r.planned)}</td>
          {metricCells(r)}
        </tr>
        {isOpen && (
          <tr style={{ background: 'rgba(10,15,30,0.6)' }}>
            <td colSpan={COL_COUNT} style={{ padding: '0 10px' }}>
              <CampaignExpand campaignId={r.key} onOpenModal={onOpenModal} />
            </td>
          </tr>
        )}
      </React.Fragment>
    );
  };

  // ── render ────────────────────────────────────────────────────────────────
  return (
    <div>
      {/* toolbar */}
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', marginBottom: 10 }}>
        <select value={datePreset} onChange={e => setDatePreset(e.target.value as DatePreset)} style={inputStyle} title="Date range (campaign activity, Denver)">
          {(Object.keys(DATE_PRESET_LABELS) as DatePreset[]).map(p => (
            <option key={p} value={p}>{DATE_PRESET_LABELS[p]}</option>
          ))}
        </select>
        <select value={statusFilter} onChange={e => onStatusFilterChange(e.target.value as ListStatusFilter)} style={inputStyle} title="Status">
          {(['all', 'draft', 'scheduled', 'sending', 'completed', 'paused', 'failed'] as ListStatusFilter[]).map(f => (
            <option key={f} value={f}>{f === 'all' ? 'All statuses' : f.charAt(0).toUpperCase() + f.slice(1)}</option>
          ))}
        </select>
        <select value={domainFilter} onChange={e => setDomainFilter(e.target.value)} style={inputStyle} title="Sending domain">
          <option value="all">All domains</option>
          {domainOptions.map(d => <option key={d} value={d}>{d}</option>)}
        </select>
        <div style={{ position: 'relative' }}>
          <FontAwesomeIcon icon={faSearch} style={{ position: 'absolute', left: 9, top: 9, fontSize: 11, color: '#475569' }} />
          <input
            value={offerText}
            onChange={e => setOfferText(e.target.value)}
            placeholder="Offer / name…"
            style={{ ...inputStyle, paddingLeft: 26, width: 170 }}
          />
        </div>
        <label style={{ display: 'inline-flex', alignItems: 'center', gap: 6, fontSize: 12, color: showDrips ? '#fbbf24' : '#94a3b8', cursor: 'pointer' }} title="Show automated follow-up programs grouped by category (never individual sends)">
          <input type="checkbox" checked={showDrips} onChange={e => setShowDrips(e.target.checked)} />
          <FontAwesomeIcon icon={faLayerGroup} /> Follow-ups
        </label>
        <label style={{ display: 'inline-flex', alignItems: 'center', gap: 6, fontSize: 12, color: groupByOffer ? '#a5b4fc' : '#94a3b8', cursor: 'pointer' }} title="Group rows by offer with rollup totals + top subjects by click-through rate">
          <input type="checkbox" checked={groupByOffer} onChange={e => setGroupByOffer(e.target.checked)} />
          <FontAwesomeIcon icon={faTags} /> Group by offer
        </label>
        <span style={{ marginLeft: 'auto', display: 'inline-flex', alignItems: 'center', gap: 10, fontSize: 11.5, color: '#64748b' }}>
          {enriching && <span><FontAwesomeIcon icon={faSpinner} spin /> metrics…</span>}
          {dataAsOf && (
            <span title={`Campaign data as of ${new Date(dataAsOf).toLocaleString()}`}>
              <FontAwesomeIcon icon={faClock} style={{ marginRight: 4 }} />
              {new Date(dataAsOf).toLocaleTimeString()}
            </span>
          )}
          <button
            onClick={onRefresh}
            disabled={loading}
            style={{ ...inputStyle, cursor: 'pointer', display: 'inline-flex', alignItems: 'center', gap: 6 }}
          >
            <FontAwesomeIcon icon={faSync} spin={loading} /> Refresh
          </button>
        </span>
      </div>

      {/* error banners */}
      {error && (
        <div style={{
          display: 'flex', alignItems: 'center', gap: 10, padding: '10px 12px', marginBottom: 8,
          borderRadius: 8, border: '1px solid #ef444455', background: '#ef44441a', color: '#fca5a5', fontSize: 12.5,
        }}>
          <FontAwesomeIcon icon={faExclamationTriangle} style={{ color: '#ef4444' }} />
          Failed to load campaigns: {error}
        </div>
      )}
      {enrichNote && (
        <div style={{
          padding: '8px 12px', marginBottom: 8, borderRadius: 8, fontSize: 12,
          border: '1px solid #f59e0b55', background: '#f59e0b14', color: '#fbbf24',
        }}>
          <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 6 }} />
          Row metrics unavailable: {enrichNote} (campaign details still shown)
        </div>
      )}

      {/* table */}
      {loading && rows.length === 0 ? (
        <div style={{ textAlign: 'center', padding: '40px 0', color: '#64748b' }}>
          <FontAwesomeIcon icon={faSpinner} spin size="2x" />
          <p style={{ marginTop: 10 }}>Loading campaigns…</p>
        </div>
      ) : sorted.length === 0 && !groupByOffer ? (
        <div style={{ textAlign: 'center', padding: '40px 0', color: '#64748b' }}>
          <FontAwesomeIcon icon={faEnvelope} size="2x" />
          <p style={{ marginTop: 10 }}>No campaigns match the current filters.</p>
        </div>
      ) : (
        <div style={{ overflowX: 'auto', border: '1px solid rgba(99,102,241,0.12)', borderRadius: 10 }}>
          <table style={{ width: '100%', borderCollapse: 'collapse', minWidth: 1240 }}>
            <thead>
              <tr style={{ background: 'rgba(15,22,41,0.9)' }}>
                <th style={{ ...thStyle, cursor: 'default', width: 18 }} />
                <th style={{ ...thStyle, textAlign: 'left' }} onClick={() => toggleSort('name')}>Campaign<SortIcon k="name" /></th>
                <th style={{ ...thStyle, textAlign: 'left' }} onClick={() => toggleSort('offer')}>Offer<SortIcon k="offer" /></th>
                <th style={{ ...thStyle, textAlign: 'left' }} onClick={() => toggleSort('domain')}>Domain<SortIcon k="domain" /></th>
                <th style={{ ...thStyle, textAlign: 'left' }} onClick={() => toggleSort('status')}>Status<SortIcon k="status" /></th>
                <th style={{ ...thStyle, textAlign: 'left' }} onClick={() => toggleSort('first_wave')}>1st Send<SortIcon k="first_wave" /></th>
                <th style={{ ...thStyle, textAlign: 'left', cursor: 'default' }}>Window</th>
                <th style={thStyle} onClick={() => toggleSort('planned')}>Planned<SortIcon k="planned" /></th>
                <th style={thStyle} onClick={() => toggleSort('attempted')}>Attempted<SortIcon k="attempted" /></th>
                <th style={thStyle} onClick={() => toggleSort('delivered')}>Delivered<SortIcon k="delivered" /></th>
                <th style={thStyle} onClick={() => toggleSort('opens')} title="Unique human opens (Apple Mail privacy & automated opens excluded)">Opens (h)<SortIcon k="opens" /></th>
                <th style={thStyle} onClick={() => toggleSort('clicks')} title="Unique clicks">Clicks (h)<SortIcon k="clicks" /></th>
                <th style={thStyle} onClick={() => toggleSort('unsubs')}>Unsubs<SortIcon k="unsubs" /></th>
                <th style={{ ...thStyle, color: '#ef4444' }} onClick={() => toggleSort('hard')}>Hard<SortIcon k="hard" /></th>
                <th style={{ ...thStyle, color: '#f59e0b' }} onClick={() => toggleSort('soft')}>Soft<SortIcon k="soft" /></th>
                <th style={thStyle} onClick={() => toggleSort('complaints')}>Compl<SortIcon k="complaints" /></th>
              </tr>
            </thead>
            <tbody>
              {groupByOffer ? (
                offerGroups.map(g => {
                  const gOpen = expandedGroups.has(g.offer);
                  return (
                    <React.Fragment key={g.offer}>
                      <tr
                        onClick={() => setExpandedGroups(prev => {
                          const next = new Set(prev);
                          if (next.has(g.offer)) next.delete(g.offer); else next.add(g.offer);
                          return next;
                        })}
                        style={{ borderTop: '1px solid rgba(99,102,241,0.15)', cursor: 'pointer', background: 'rgba(99,102,241,0.05)' }}
                      >
                        <td style={{ padding: '7px 4px 7px 8px' }}>
                          <FontAwesomeIcon icon={gOpen ? faChevronDown : faChevronRight} style={{ fontSize: 10, color: '#6366f1' }} />
                        </td>
                        <td style={{ padding: '7px 8px' }} colSpan={4}>
                          <span style={{ fontSize: 13, fontWeight: 700, color: '#c7d2fe' }}>{g.offer}</span>
                          <span style={{ fontSize: 11, color: '#64748b', marginLeft: 8 }}>{g.members.length} campaign{g.members.length === 1 ? '' : 's'}</span>
                        </td>
                        <td style={{ ...tdNum, textAlign: 'left' }} colSpan={2} />
                        <td style={tdNum}>{numFmt(g.planned)}</td>
                        {metricCells({ metrics: g.metrics, enriched: g.members.every(m => m.enriched) })}
                      </tr>
                      {gOpen && (
                        <>
                          {g.subjects.length > 0 && (
                            <tr style={{ background: 'rgba(10,15,30,0.6)' }}>
                              <td colSpan={COL_COUNT} style={{ padding: '8px 16px' }}>
                                <div style={{ fontSize: 10.5, textTransform: 'uppercase', letterSpacing: 0.5, color: 'rgba(180,210,240,0.45)', fontWeight: 700, marginBottom: 4 }}>
                                  Top subject lines by click-through rate (clicks / delivered)
                                </div>
                                {g.subjects.map(s => (
                                  <div key={s.subject} style={{ display: 'flex', gap: 10, fontSize: 12, color: '#cbd5e1', padding: '2px 0' }}>
                                    <span style={{ flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={s.subject}>{s.subject}</span>
                                    <span style={{ color: '#c084fc', fontWeight: 700 }}>{s.ctr.toFixed(2)}%</span>
                                    <span style={{ color: '#64748b' }}>{numFmt(s.clicks)} clicks / {numFmt(s.delivered)} delivered</span>
                                  </div>
                                ))}
                              </td>
                            </tr>
                          )}
                          {g.members.map(m => renderCampaignRow(m, true))}
                        </>
                      )}
                    </React.Fragment>
                  );
                })
              ) : (
                pageRows.map(r => renderCampaignRow(r))
              )}
            </tbody>
          </table>
        </div>
      )}

      {/* pagination */}
      {!groupByOffer && sorted.length > PAGE_SIZE && (
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginTop: 8, fontSize: 12, color: '#94a3b8' }}>
          <button
            disabled={safePage === 0}
            onClick={() => setPage(p => Math.max(0, p - 1))}
            style={{ ...inputStyle, cursor: safePage === 0 ? 'default' : 'pointer', opacity: safePage === 0 ? 0.4 : 1 }}
          >
            ‹ Prev
          </button>
          <span>
            {safePage * PAGE_SIZE + 1}–{Math.min((safePage + 1) * PAGE_SIZE, sorted.length)} of {sorted.length}
          </span>
          <button
            disabled={safePage >= pageCount - 1}
            onClick={() => setPage(p => Math.min(pageCount - 1, p + 1))}
            style={{ ...inputStyle, cursor: safePage >= pageCount - 1 ? 'default' : 'pointer', opacity: safePage >= pageCount - 1 ? 0.4 : 1 }}
          >
            Next ›
          </button>
        </div>
      )}

      <div style={{ marginTop: 8, fontSize: 10.5, color: 'rgba(100,116,139,0.8)' }}>
        Opens shown are human (Apple Mail privacy & automated opens excluded), clicks unique.
        Relayed sends may under-report here (Reporting follow-up). Follow-ups {showDrips ? 'shown grouped by category' : 'hidden'}.
      </div>
    </div>
  );
};

export default CampaignCenterList;
