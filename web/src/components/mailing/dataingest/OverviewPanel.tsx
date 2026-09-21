// OverviewPanel — board 1 ("Data Ingest overview").
//
// Two supply classes, never mixed: what is LOADED and at rest (S3 objects +
// rows parked in the database) vs what ARRIVES on its own clock (API feeds,
// site events, pulls); internal transfers are shown but are NOT ingest.
//
// Envelope field names this panel reads (the Go handler must flag these):
//   /day    at_rest.{landed,raw,staged,inflight,mailed,removed,static_objects,
//           loads_without_object,parked_in_db} · dynamic.{arrived,yesterday,
//           arrival_rate,feeds_live} · internal_transfer.n · series · composition
//   /hours  hours
//   /feeds  feeds.{today,yesterday,raw,staged,inflight,mailed,records,consumed,remaining}

import React, { useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faDatabase, faSatelliteDish, faUpload, faChartColumn, faClock } from '@fortawesome/free-solid-svg-icons';
import {
  ResponsiveContainer, BarChart, ComposedChart, Bar, Line, XAxis, YAxis, Tooltip, Legend, CartesianGrid,
} from 'recharts';
import { usePolling } from '../shared/usePolling';
import { Panel, SectionHeader, SectionError, EmptyState, Pill, ProgressBar } from '../shared/ui';
import { colors, panelStyle, tableStyle, thStyle, tdStyle, numTd, numTh, btnStyle } from '../shared/theme';
import { denverToday } from '../shared/filters';
import { Measured } from './Measured';
import {
  dataIngestApi, measured,
  type Count, type DayResponse, type FeedRow, type FeedsResponse, type HoursResponse, type IngestDelta,
} from './api';

const POLL_MS = 30_000;

const AT_REST = colors.warning;
const DYNAMIC = colors.success;
const TRANSFER = '#a78bfa';

interface Props {
  date: string;
  deltas: IngestDelta[];
  /** the roster is owned by the portal so the upload door shares one read */
  feeds: FeedsResponse | null;
  feedsError: string | null;
  refreshFeeds: () => void;
  onOpenFeed: (datasetId: string) => void;
  onGoUpload: () => void;
}

// Fold the live stream into the polled snapshot: only for the day being shown,
// and only as an ADDITION to what the poll already counted.
function liveAdd(deltas: IngestDelta[], date: string, cls: IngestDelta['supply_class'], transitions: string[]): number {
  let n = 0;
  for (const d of deltas) {
    if (d.day === date && d.supply_class === cls && transitions.includes(d.transition)) n += d.n;
  }
  return n;
}

const withLive = (base: Count, add: number): Count =>
  typeof base === 'number' && Number.isFinite(base) ? base + add : base;

const Tile: React.FC<{
  label: string; hint: string; children: React.ReactNode; accent?: string; borderAccent?: boolean;
}> = ({ label, hint, children, accent, borderAccent }) => (
  <div
    style={{
      background: 'rgba(15,23,42,0.55)',
      border: `1px solid ${borderAccent && accent ? accent + '66' : colors.panelBorder}`,
      borderRadius: 10,
      padding: '12px 14px',
      display: 'flex',
      flexDirection: 'column',
      gap: 4,
      minWidth: 0,
    }}
  >
    <div style={{ fontSize: 10, color: accent ?? colors.textMuted, textTransform: 'uppercase', letterSpacing: 0.6 }}>{label}</div>
    <div style={{ fontSize: 22, fontWeight: 700, lineHeight: 1.2 }}>{children}</div>
    <div style={{ fontSize: 11, color: colors.textMuted }}>{hint}</div>
  </div>
);

const chartTooltip = {
  contentStyle: { background: '#0f1c33', border: '1px solid rgba(120,150,200,0.3)', borderRadius: 6, fontSize: 12 },
  labelStyle: { color: colors.heading },
};

export const OverviewPanel: React.FC<Props> = ({ date, deltas, feeds, feedsError, refreshFeeds, onOpenFeed, onGoUpload }) => {
  const [feedFilter, setFeedFilter] = useState('');
  const isToday = date === denverToday();

  const day = usePolling<DayResponse>((signal) => dataIngestApi.day(date, signal), POLL_MS, [date]);
  const hours = usePolling<HoursResponse>((signal) => dataIngestApi.hours(date, 'dynamic', signal), POLL_MS, [date]);

  const d = day.data;
  const f = d?.fields;

  // Live-folded numbers (stream deltas for the displayed day only).
  const arrivedLive = isToday ? liveAdd(deltas, date, 'dynamic', ['landed', 'site_event', 'hydrated']) : 0;
  const landedAtRestLive = isToday ? liveAdd(deltas, date, 'at_rest', ['landed', 'load_registered']) : 0;
  const transferLive = isToday ? liveAdd(deltas, date, 'internal_transfer', ['transfer']) : 0;

  const series = useMemo(() => (d?.series ?? []).map((s) => ({
    day: s.day.slice(5),
    'At rest': s.at_rest,
    Dynamic: s.dynamic,
    Transfer: s.transfer,
  })), [d]);

  const hourRows = useMemo(() => (hours.data?.hours ?? []).map((h) => ({
    hour: String(h.hour).padStart(2, '0'),
    Arrived: h.n,
    'Median 7d': h.median_7d ?? null,
  })), [hours.data]);
  const hoursMeasured = measured(hours.data?.fields, 'hours') !== 'not_measured'
    && hourRows.some((h) => typeof h.Arrived === 'number');

  const allFeeds = feeds?.feeds ?? [];
  const ff = feeds?.fields;
  const atRestFeeds = allFeeds.filter((x) => x.supply_class === 'at_rest' || x.supply_class === 'internal_transfer');
  const liveFeeds = allFeeds.filter((x) => x.supply_class === 'dynamic')
    .filter((x) => !feedFilter || `${x.name} ${x.lane}`.toLowerCase().includes(feedFilter.toLowerCase()));

  const composition = d?.composition ?? [];
  const compMax = composition.reduce((m, c) => Math.max(m, typeof c.n === 'number' ? c.n : 0), 0);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      {/* ── the two supply classes ─────────────────────────────────────── */}
      {day.error && !d && <SectionError label="Day totals" error={day.error} onRetry={day.refresh} />}
      {day.error && d && (
        <div style={{ fontSize: 12, color: colors.warningText }}>
          Showing the last good read — refresh failed: {day.error}
        </div>
      )}

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(460px, 1fr))', gap: 14 }}>
        <Panel accent={AT_REST}>
          <SectionHeader
            title="At rest · loaded inventory"
            icon={faDatabase}
            right={<span style={{ fontSize: 11, color: colors.textMuted }}>nothing here moves until someone loads, releases or claims it</span>}
          />
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(150px, 1fr))', gap: 10 }}>
            <Tile label="Static files in S3" hint="objects in s3://jarvis-partner-ingest — a file that is not in the bucket is not inventory">
              <Measured fields={f} name="at_rest.static_objects" value={d?.classes.at_rest.static_objects} />
            </Tile>
            <Tile label="Loaded without an object" hint="desktop · downloads · operator-upload · gdrive — no S3 object behind them; not reproducible" accent={colors.danger} borderAccent>
              <Measured fields={f} name="at_rest.loads_without_object" value={d?.classes.at_rest.loads_without_object} color={colors.dangerText} />
            </Tile>
            <Tile label="In the database, parked" hint="held rows awaiting a release">
              <Measured fields={f} name="at_rest.parked_in_db" value={d?.classes.at_rest.parked_in_db} />
            </Tile>
            <Tile label="Staged for mailing" hint="ready · EO-verdicted" accent={colors.indigo300} borderAccent>
              <Measured fields={f} name="at_rest.staged" value={d?.classes.at_rest.staged} color={colors.indigo200} />
            </Tile>
          </div>
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 16, fontSize: 11, color: colors.textMuted, marginTop: 10 }}>
            <span>
              Loaded on {date}{' '}
              <Measured fields={f} name="at_rest.landed" value={withLive(d?.classes.at_rest.landed ?? null, landedAtRestLive)} color={AT_REST} />
            </span>
            <span>In flight (claimed) <Measured fields={f} name="at_rest.inflight" value={d?.classes.at_rest.inflight} color={colors.warningText} /></span>
            <span>Mailed lifetime <Measured fields={f} name="at_rest.mailed" value={d?.classes.at_rest.mailed} /></span>
            <span>Removed by cleaning <Measured fields={f} name="at_rest.removed" value={d?.classes.at_rest.removed} color={colors.dangerText} /></span>
          </div>
        </Panel>

        <Panel accent={DYNAMIC}>
          <SectionHeader
            title="Dynamic · live feeds"
            icon={faSatelliteDish}
            right={<span style={{ fontSize: 11, color: colors.textMuted }}>arrives on the sender&apos;s clock; counted at the edge, never re-scanned</span>}
          />
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(150px, 1fr))', gap: 10 }}>
            <Tile label={isToday ? 'Arrived today' : `Arrived ${date}`} hint="records landed from API feeds, site events and pulls">
              <Measured fields={f} name="dynamic.arrived" value={withLive(d?.classes.dynamic.arrived ?? null, arrivedLive)} color={DYNAMIC} />
            </Tile>
            <Tile label="Yesterday" hint="the same count for the previous Denver day">
              <Measured fields={f} name="dynamic.yesterday" value={d?.classes.dynamic.yesterday} />
            </Tile>
            <Tile label="Arrival rate" hint="records / hour across live feeds, from the counters">
              <Measured fields={f} name="dynamic.arrival_rate" value={d?.classes.dynamic.arrival_rate} unit="/hr" />
            </Tile>
            <Tile label="Feeds live" hint="datasets with an open ingest path">
              <Measured fields={f} name="dynamic.feeds_live" value={d?.classes.dynamic.feeds_live} />
            </Tile>
          </div>
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 16, fontSize: 11, color: colors.textMuted, marginTop: 10 }}>
            <span>
              Internal transfers on {date} (NOT ingest){' '}
              <Measured fields={f} name="internal_transfer.n" value={withLive(d?.classes.internal_transfer.n ?? null, transferLive)} color={TRANSFER} />
              {d?.classes.internal_transfer.note ? ` · ${d.classes.internal_transfer.note}` : ' · nightly lane inject, reservoir → lane'}
            </span>
            <span>Last API batch <span style={{ color: colors.text }}>{d?.classes.dynamic.last_api_batch ?? 'not yet measured'}</span></span>
          </div>
        </Panel>
      </div>

      {/* ── day over day + time of day ─────────────────────────────────── */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(420px, 1fr))', gap: 14 }}>
        <Panel>
          <SectionHeader
            title="Day over day"
            icon={faChartColumn}
            right={<span style={{ fontSize: 11, color: colors.textMuted }}>records per Denver day · loaded vs arrived vs internal transfer</span>}
          />
          {measured(f, 'series') === 'not_measured' || series.length === 0 ? (
            <EmptyState title="not yet measured" hint="the day-over-day series comes from data_ingest_rollup; it fills once the nightly rollup has run." />
          ) : (
            <ResponsiveContainer width="100%" height={230}>
              <BarChart data={series} margin={{ top: 4, right: 12, left: -14, bottom: 0 }}>
                <CartesianGrid stroke="rgba(255,255,255,0.06)" vertical={false} />
                <XAxis dataKey="day" tick={{ fill: colors.textMuted, fontSize: 11 }} stroke="rgba(120,150,200,0.25)" />
                <YAxis tick={{ fill: colors.textMuted, fontSize: 11 }} stroke="rgba(120,150,200,0.25)" />
                <Tooltip {...chartTooltip} />
                <Legend wrapperStyle={{ fontSize: 12 }} />
                <Bar dataKey="At rest" stackId="a" fill={AT_REST} radius={[0, 0, 0, 0]} />
                <Bar dataKey="Dynamic" stackId="a" fill={DYNAMIC} />
                <Bar dataKey="Transfer" stackId="a" fill={TRANSFER} radius={[3, 3, 0, 0]} />
              </BarChart>
            </ResponsiveContainer>
          )}
          <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 6 }}>
            A day with no bar for a class has no counted records for it — a gap is not a zero.
          </div>
        </Panel>

        <Panel>
          <SectionHeader
            title="Time of day · dynamic only"
            icon={faClock}
            right={<span style={{ fontSize: 11, color: colors.textMuted }}>arrivals per Denver hour</span>}
          />
          {hours.error && <SectionError label="Hourly arrivals" error={hours.error} onRetry={hours.refresh} />}
          {!hours.error && !hoursMeasured && (
            <div style={{ ...panelStyle, borderColor: 'rgba(245,158,11,0.35)', color: colors.warningText, fontSize: 12 }}>
              Not yet measured. This is fed by the hourly counters of data.ingest.v1. Loads at rest are point
              events and would show as markers on this axis, not bars.
            </div>
          )}
          {!hours.error && hoursMeasured && (
            <ResponsiveContainer width="100%" height={230}>
              <ComposedChart data={hourRows} margin={{ top: 4, right: 12, left: -14, bottom: 0 }}>
                <CartesianGrid stroke="rgba(255,255,255,0.06)" vertical={false} />
                <XAxis dataKey="hour" tick={{ fill: colors.textMuted, fontSize: 10 }} stroke="rgba(120,150,200,0.25)" interval={3} />
                <YAxis tick={{ fill: colors.textMuted, fontSize: 11 }} stroke="rgba(120,150,200,0.25)" />
                <Tooltip {...chartTooltip} />
                <Legend wrapperStyle={{ fontSize: 12 }} />
                <Bar dataKey="Arrived" fill={DYNAMIC} radius={[2, 2, 0, 0]} />
                <Line type="monotone" dataKey="Median 7d" stroke={colors.indigo400} strokeWidth={2} dot={false} />
              </ComposedChart>
            </ResponsiveContainer>
          )}
        </Panel>
      </div>

      {/* ── loaded inventory ───────────────────────────────────────────── */}
      <Panel accent={AT_REST}>
        <SectionHeader
          title="Loaded inventory · at rest"
          icon={faDatabase}
          right={
            <button type="button" style={btnStyle} onClick={onGoUpload}>
              <FontAwesomeIcon icon={faUpload} style={{ marginRight: 6 }} />
              Upload static file → s3://jarvis-partner-ingest/static/
            </button>
          }
        />
        {feedsError && !feeds && <SectionError label="Feeds" error={feedsError} onRetry={refreshFeeds} />}
        {feeds && atRestFeeds.length === 0 && (
          <EmptyState title="No at-rest loads" hint="Nothing is registered as loaded inventory for this organization yet." />
        )}
        {atRestFeeds.length > 0 && (
          <div style={{ overflowX: 'auto' }}>
            <table style={{ ...tableStyle, minWidth: 900 }}>
              <thead>
                <tr>
                  <th style={thStyle}>Source</th>
                  <th style={thStyle}>Where</th>
                  <th style={thStyle}>Loaded / last</th>
                  <th style={numTh}>Records</th>
                  <th style={numTh}>Consumed</th>
                  <th style={numTh}>Remaining</th>
                  <th style={numTh}>Staged</th>
                  <th style={thStyle}>Controls</th>
                </tr>
              </thead>
              <tbody>
                {atRestFeeds.map((r) => (
                  <tr key={r.dataset_id}>
                    <td style={tdStyle}>
                      <div>{r.name}</div>
                      <div style={{ fontSize: 10, color: colors.textFaint, fontFamily: 'monospace' }} title={r.dataset_id}>
                        {r.lane || r.source_channel} · {r.dataset_id.slice(0, 8)}
                      </div>
                    </td>
                    <td style={tdStyle}>
                      <Pill color={r.supply_class === 'internal_transfer' ? TRANSFER : AT_REST}>{r.location ?? r.supply_class}</Pill>
                    </td>
                    <td style={{ ...tdStyle, color: colors.textMuted, fontSize: 11 }}>{r.last_loaded ?? 'never'}</td>
                    <td style={numTd}><Measured fields={ff} name="feeds.records" value={r.records ?? null} /></td>
                    <td style={numTd}><Measured fields={ff} name="feeds.consumed" value={r.consumed ?? null} /></td>
                    <td style={numTd}><Measured fields={ff} name="feeds.remaining" value={r.remaining ?? null} color={AT_REST} /></td>
                    <td style={numTd}><Measured fields={ff} name="feeds.staged" value={r.staged} color={colors.indigo200} /></td>
                    <td style={tdStyle}>
                      <button type="button" style={{ ...btnStyle, padding: '3px 10px', fontSize: 11 }} onClick={() => onOpenFeed(r.dataset_id)}>
                        Open
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 8 }}>
          A static file is inventory only once it is an object in the bucket: upload → object → loader → batch row → rows.
        </div>
      </Panel>

      {/* ── live feeds ─────────────────────────────────────────────────── */}
      <Panel accent={DYNAMIC}>
        <SectionHeader
          title="Live feeds · dynamic"
          icon={faSatelliteDish}
          right={
            <label style={{ fontSize: 11, color: colors.textMuted, display: 'flex', alignItems: 'center', gap: 6 }}>
              Filter
              <input
                type="search"
                value={feedFilter}
                onChange={(e) => setFeedFilter(e.target.value)}
                placeholder="feed or lane"
                style={{
                  background: 'rgba(15,23,42,0.6)', color: colors.text, border: `1px solid ${colors.panelBorder}`,
                  borderRadius: 8, padding: '5px 9px', fontSize: 12, width: 180,
                }}
              />
            </label>
          }
        />
        {feeds && liveFeeds.length === 0 && (
          <EmptyState title="No live feeds match" hint={feedFilter ? 'Clear the filter to see every dynamic feed.' : 'No dataset is currently classed dynamic.'} />
        )}
        {liveFeeds.length > 0 && (
          <div style={{ overflowX: 'auto' }}>
            <table style={{ ...tableStyle, minWidth: 980 }}>
              <thead>
                <tr>
                  <th style={thStyle}>Feed → lane</th>
                  <th style={thStyle}>Status</th>
                  <th style={numTh}>Today</th>
                  <th style={numTh}>Yesterday</th>
                  <th style={numTh}>Raw</th>
                  <th style={numTh}>Staged</th>
                  <th style={numTh}>Mailed</th>
                  <th style={thStyle}>Last event</th>
                  <th style={thStyle}>Controls</th>
                </tr>
              </thead>
              <tbody>
                {liveFeeds.map((r) => (
                  <tr key={r.dataset_id}>
                    <td style={tdStyle}>
                      <div>{r.name}</div>
                      <div style={{ fontSize: 10, color: colors.textFaint, fontFamily: 'monospace' }}>{r.lane || r.source_channel}</div>
                    </td>
                    <td style={tdStyle}><FeedStatusPill row={r} /></td>
                    <td style={numTd}><Measured fields={ff} name="feeds.today" value={r.today} color={DYNAMIC} /></td>
                    <td style={numTd}><Measured fields={ff} name="feeds.yesterday" value={r.yesterday} /></td>
                    <td style={numTd}><Measured fields={ff} name="feeds.raw" value={r.raw} /></td>
                    <td style={numTd}><Measured fields={ff} name="feeds.staged" value={r.staged} color={colors.indigo200} /></td>
                    <td style={numTd}><Measured fields={ff} name="feeds.mailed" value={r.mailed} /></td>
                    <td style={{ ...tdStyle, color: colors.textMuted, fontSize: 11 }}>{r.last_event ?? '—'}</td>
                    <td style={tdStyle}>
                      <button type="button" style={{ ...btnStyle, padding: '3px 10px', fontSize: 11 }} onClick={() => onOpenFeed(r.dataset_id)}>
                        Open
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 8 }}>
          Status is the composite of the four real switches: ingest open · send row present · express dispatch · supply contract.
          Open a feed to change any of them.
        </div>
      </Panel>

      {/* ── composition ────────────────────────────────────────────────── */}
      <Panel>
        <SectionHeader title="Composition of staged data" icon={faChartColumn} />
        {measured(f, 'composition') === 'not_measured' || composition.length === 0 ? (
          <EmptyState title="not yet measured" hint="Ready rows by canonical ISP class — fed by the counters' per-ISP hashes." />
        ) : (
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(300px, 1fr))', gap: '6px 28px' }}>
            {composition.map((c) => (
              <div key={c.isp} style={{ display: 'grid', gridTemplateColumns: '90px 1fr 92px', alignItems: 'center', gap: 10, fontSize: 12 }}>
                <div style={{ color: colors.text }}>{c.isp}</div>
                <ProgressBar pct={compMax > 0 && typeof c.n === 'number' ? c.n / compMax : 0} height={9} />
                <div style={{ textAlign: 'right' }}>
                  <Measured fields={f} name="composition" value={c.n} />
                </div>
              </div>
            ))}
          </div>
        )}
        <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 8 }}>
          Distinct addresses in status <code>ready</code>. Totals here are the estate, not one feed — open a feed for its own split.
        </div>
      </Panel>

      <div style={{ fontSize: 11, color: colors.textFaint }}>
        Sources: s3://jarvis-partner-ingest (objects) · partner_inbound_batches · partner_clean_queue ·
        partner_datasets.source_channel · mailing_subscribers · the data.ingest.v1 counters.
        {d?.source ? ` · this view: ${d.source}` : ''}
      </div>
    </div>
  );
};

// The four switches, collapsed to one word. Unknown switches never read as "off".
export const FeedStatusPill: React.FC<{ row: FeedRow }> = ({ row }) => {
  const s = row.status ?? { ingest_open: null, send_row: null, express: null, contract: null };
  if (s.ingest_open === false) return <Pill color={colors.danger}>paused</Pill>;
  if (s.express === true) return <Pill color={colors.indigo300}>express</Pill>;
  if (s.ingest_open === true && s.send_row === true) return <Pill color={colors.success}>active</Pill>;
  if (s.ingest_open === true) return <Pill color={colors.warning}>staged</Pill>;
  return <Pill color={colors.idle}>unknown</Pill>;
};

export default OverviewPanel;
