// FeedPanel — board 2 ("Feed detail").
//
// One feed = partner × dataset → lane. Shows the four REAL switches as one
// status, drives them through the endpoints that already exist
// (/api/mailing/data-partners/datasets/{id}/emergency-stop | /resume | /express),
// then the funnel, 30 days of landings, raw-vs-staged composition and the loads.
//
// Shapes are the Go feedDetailResponse (api.ts): funnel flags are the FLAT
// names landed raw cleaned staged mailed engaged; composition is
// {raw:[], staged:[]} (flags composition_raw / composition_staged); series
// points are per-class {day, at_rest, dynamic, transfer}; loads are feedLoad
// rows (flag loads). The header (name / partner / lane / class / channel /
// status) comes from the detail body when the server sends it and otherwise
// from the /feeds list row for this dataset_id, which the portal already holds.
//
// A failed control is STICKY and never looks like success (the REQ-004 rule the
// Data Partners screen learned the hard way): the 30s poll clears the read
// error, never the action error.

import React, { useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faArrowLeft, faExclamationTriangle, faBolt, faPause, faPlay, faRoute, faLayerGroup } from '@fortawesome/free-solid-svg-icons';
import { ResponsiveContainer, BarChart, Bar, XAxis, YAxis, Tooltip, CartesianGrid, Legend } from 'recharts';
import { usePolling } from '../shared/usePolling';
import { Panel, SectionHeader, SectionError, EmptyState, Pill, ProgressBar } from '../shared/ui';
import { colors, tableStyle, thStyle, tdStyle, numTd, numTh, btnStyle } from '../shared/theme';
import { Measured } from './Measured';
import { FreshnessLine } from './Envelope';
import { AT_REST, DYNAMIC, TRANSFER, FeedStatusPill } from './OverviewPanel';
import {
  dataIngestApi, datasetAction, measured, presentString,
  type FeedDetailResponse, type FeedRow, type FeedStatus,
} from './api';

const POLL_MS = 30_000;

interface Props {
  datasetId: string;
  date: string;
  /** the /feeds row for this dataset (header fallback + status fallback) */
  listRow: FeedRow | null;
  onBack: () => void;
}

const SwitchRow: React.FC<{ label: string; state: boolean | null; onLabel: string; offLabel: string }> = ({
  label, state, onLabel, offLabel,
}) => (
  <div
    style={{
      display: 'flex', alignItems: 'center', justifyContent: 'space-between',
      padding: '8px 10px', background: 'rgba(15,23,42,0.55)',
      border: `1px solid ${colors.panelBorder}`, borderRadius: 8, fontSize: 12,
    }}
  >
    <span style={{ color: colors.text }}>{label}</span>
    {state === null ? (
      <span style={{ color: colors.textFaint, fontStyle: 'italic' }}>not yet measured</span>
    ) : (
      <span style={{ color: state ? colors.success : colors.textMuted, fontWeight: 600 }}>{state ? onLabel : offLabel}</span>
    )}
  </div>
);

const classColor = (cls: string | undefined): string =>
  cls === 'at_rest' ? AT_REST : cls === 'dynamic' ? DYNAMIC : cls === 'internal_transfer' ? TRANSFER : colors.idle;

export const FeedPanel: React.FC<Props> = ({ datasetId, date, listRow, onBack }) => {
  const [actionError, setActionError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const feed = usePolling<FeedDetailResponse>(
    (signal) => dataIngestApi.feed(datasetId, date, signal),
    POLL_MS,
    [datasetId, date],
  );
  const d = feed.data;
  const f = d?.fields;

  // Header: detail body first (once the server sends it), then the list row.
  const name = presentString(d?.name) ?? listRow?.name ?? null;
  const partner = presentString(d?.partner) ?? presentString(listRow?.partner) ?? null;
  const lane = presentString(d?.lane) ?? presentString(listRow?.lane) ?? null;
  const supplyClass = presentString(d?.supply_class) ?? presentString(listRow?.supply_class) ?? null;
  const sourceChannel = presentString(d?.source_channel) ?? presentString(listRow?.source_channel) ?? null;
  const status: FeedStatus | null = d?.status ?? listRow?.status ?? null;
  const lastLoaded = presentString(d?.last_loaded) ?? presentString(listRow?.last_loaded) ?? null;

  const run = async (
    action: 'emergency-stop' | 'resume' | 'express',
    confirmText: string,
    payload?: Record<string, unknown>,
    failMsg?: string,
  ) => {
    if (!window.confirm(confirmText)) return;
    setBusy(true);
    try {
      await datasetAction(datasetId, action, payload);
      setActionError(null);
      feed.refresh();
    } catch (e) {
      setActionError(`${failMsg ?? action} FAILED (${e instanceof Error ? e.message : String(e)}) — nothing changed on the dataset. Retry or change it server-side.`);
    } finally {
      setBusy(false);
    }
  };

  const pause = () => {
    const reason = window.prompt('Reason for pausing ingestion:', 'operator emergency stop');
    if (reason === null) return;
    void run(
      'emergency-stop',
      'Pause ingestion for this dataset? Inbound records stop being processed at the next safe point.',
      { reason },
      'EMERGENCY STOP',
    );
  };
  const resume = () => void run('resume', 'Resume ingestion for this dataset?', undefined, 'RESUME');
  const toggleExpress = (next: boolean) =>
    void run(
      'express',
      next
        ? 'Turn express dispatch ON? Records will mail on arrival and the dataset is exempt from the multi-day drain horizon.'
        : 'Turn express dispatch OFF?',
      { enabled: next },
      'EXPRESS TOGGLE',
    );

  const series = (d?.series ?? []).map((s) => ({
    day: s.day.slice(5),
    'At rest': s.at_rest,
    Dynamic: s.dynamic,
    Transfer: s.transfer,
  }));

  // Merge the two per-ISP arrays into one row per ISP; a class absent for an
  // ISP is absent (null), not zero.
  const compRows = (() => {
    const byIsp = new Map<string, { raw: number | null; staged: number | null }>();
    for (const c of d?.composition.raw ?? []) byIsp.set(c.isp, { raw: c.n, staged: byIsp.get(c.isp)?.staged ?? null });
    for (const c of d?.composition.staged ?? []) byIsp.set(c.isp, { raw: byIsp.get(c.isp)?.raw ?? null, staged: c.n });
    return Array.from(byIsp.entries())
      .map(([isp, v]) => ({ isp, ...v }))
      .sort((a, b) => ((b.raw ?? 0) + (b.staged ?? 0)) - ((a.raw ?? 0) + (a.staged ?? 0)) || a.isp.localeCompare(b.isp));
  })();
  const compMax = compRows.reduce((m, c) => Math.max(m, c.raw ?? 0, c.staged ?? 0), 0);
  const compMeasured = measured(f, 'composition_raw') !== 'not_measured' || measured(f, 'composition_staged') !== 'not_measured';

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
        <button type="button" style={btnStyle} onClick={onBack}>
          <FontAwesomeIcon icon={faArrowLeft} style={{ marginRight: 6 }} /> Data Ingest
        </button>
        <div>
          <div style={{ fontSize: 18, fontWeight: 700, color: colors.heading, display: 'flex', alignItems: 'center', gap: 10 }}>
            {name ? `${partner ? `${partner} → ` : ''}${name}` : 'Feed'}
            {status && <FeedStatusPill row={{ status }} />}
            <Pill color={classColor(supplyClass ?? undefined)}>{supplyClass ?? 'unclassed'}</Pill>
          </div>
          <div style={{ fontSize: 11, color: colors.textMuted, fontFamily: 'monospace' }}>
            dataset {datasetId} · lane {lane ?? '—'} · {sourceChannel ?? 'source channel unknown'} · last loaded {lastLoaded ?? 'never'}
          </div>
        </div>
      </div>

      {actionError && (
        <div
          style={{
            background: 'rgba(239,68,68,0.28)', border: '1px solid rgba(239,68,68,0.6)',
            padding: 12, borderRadius: 6, fontWeight: 600, color: colors.dangerFaint, fontSize: 13,
          }}
        >
          <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 8 }} />
          {actionError}
          <button type="button" style={{ ...btnStyle, marginLeft: 10 }} onClick={() => setActionError(null)}>Dismiss</button>
        </div>
      )}

      {feed.error && !d && <SectionError label="Feed detail" error={feed.error} onRetry={feed.refresh} />}
      {feed.error && d && (
        <div style={{ fontSize: 12, color: colors.warningText }}>Showing the last good read — refresh failed: {feed.error}</div>
      )}

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(340px, 1fr))', gap: 14 }}>
        <Panel>
          <SectionHeader title="Status and controls" icon={faRoute} />
          <div style={{ fontSize: 11, color: colors.textMuted, marginBottom: 8 }}>
            The four real switches, shown as one status. Every change writes the audit log.
          </div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
            <SwitchRow label="Ingestion (paused_emergency)" state={status ? status.ingest_open : null} onLabel="open" offLabel="paused" />
            <SwitchRow label="Send (partner_drip_state row)" state={status ? status.send_row : null} onLabel="present" offLabel="absent" />
            <SwitchRow label="Express dispatch" state={status ? status.express : null} onLabel="on" offLabel="off" />
            <SwitchRow label="Supply contract (mediator)" state={status ? status.contract : null} onLabel="active" offLabel="none" />
          </div>
          <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginTop: 12 }}>
            <button
              type="button"
              disabled={busy}
              onClick={pause}
              style={{ ...btnStyle, color: colors.dangerText, borderColor: 'rgba(239,68,68,0.45)', background: 'rgba(239,68,68,0.10)' }}
            >
              <FontAwesomeIcon icon={faPause} style={{ marginRight: 6 }} /> Pause ingestion
            </button>
            <button type="button" disabled={busy} onClick={resume} style={{ ...btnStyle, color: colors.successText, borderColor: 'rgba(34,197,94,0.45)', background: 'rgba(34,197,94,0.10)' }}>
              <FontAwesomeIcon icon={faPlay} style={{ marginRight: 6 }} /> Resume ingestion
            </button>
            <button type="button" disabled={busy} onClick={() => toggleExpress(!(status?.express === true))} style={btnStyle}>
              <FontAwesomeIcon icon={faBolt} style={{ marginRight: 6 }} />
              {status?.express === true ? 'Turn express off' : 'Turn express on'}
            </button>
          </div>
          <div style={{ fontSize: 11, color: colors.textFaint, marginTop: 8 }}>
            Releasing held rows into the lane is a supply decision and stays on the Data Partners screen —
            this panel only opens and closes the feed.
          </div>
        </Panel>

        <Panel>
          <SectionHeader
            title="Funnel"
            icon={faLayerGroup}
            right={<span style={{ fontSize: 11, color: colors.textMuted }}>every row this feed landed, by state</span>}
          />
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(120px, 1fr))', gap: 10 }}>
            {([
              ['landed', `Landed ${date}`, d?.funnel.landed, colors.text],
              ['raw', 'Raw', d?.funnel.raw, colors.text],
              ['cleaned', 'Cleaned', d?.funnel.cleaned, colors.text],
              ['staged', 'Staged', d?.funnel.staged, colors.indigo200],
              ['mailed', 'Mailed', d?.funnel.mailed, colors.successText],
              ['engaged', 'Engaged', d?.funnel.engaged, colors.warningText],
            ] as const).map(([key, label, value, color]) => (
              <div key={key} style={{ background: 'rgba(15,23,42,0.55)', border: `1px solid ${colors.panelBorder}`, borderRadius: 10, padding: '10px 12px' }}>
                <div style={{ fontSize: 10, color: colors.textMuted, textTransform: 'uppercase', letterSpacing: 0.6 }}>{label}</div>
                <div style={{ fontSize: 18, fontWeight: 700 }}>
                  <Measured fields={f} name={key} value={value ?? null} color={color} />
                </div>
              </div>
            ))}
          </div>
          <div style={{ marginTop: 12 }}>
            {measured(f, 'series') === 'not_measured' || series.length === 0 ? (
              <EmptyState title="not yet measured" hint="Day-over-day landings for this feed come from the rollup." />
            ) : (
              <ResponsiveContainer width="100%" height={160}>
                <BarChart data={series} margin={{ top: 4, right: 12, left: -18, bottom: 0 }}>
                  <CartesianGrid stroke="rgba(255,255,255,0.06)" vertical={false} />
                  <XAxis dataKey="day" tick={{ fill: colors.textMuted, fontSize: 10 }} stroke="rgba(120,150,200,0.25)" interval="preserveStartEnd" />
                  <YAxis tick={{ fill: colors.textMuted, fontSize: 10 }} stroke="rgba(120,150,200,0.25)" />
                  <Tooltip contentStyle={{ background: '#0f1c33', border: '1px solid rgba(120,150,200,0.3)', borderRadius: 6, fontSize: 12 }} labelStyle={{ color: colors.heading }} />
                  <Legend wrapperStyle={{ fontSize: 11 }} />
                  <Bar dataKey="At rest" stackId="a" fill={AT_REST} />
                  <Bar dataKey="Dynamic" stackId="a" fill={DYNAMIC} />
                  <Bar dataKey="Transfer" stackId="a" fill={TRANSFER} radius={[3, 3, 0, 0]} />
                </BarChart>
              </ResponsiveContainer>
            )}
          </div>
          <FreshnessLine asOf={d?.as_of} source={d?.source} />
        </Panel>
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(380px, 1fr))', gap: 14 }}>
        <Panel>
          <SectionHeader title="Composition · raw vs staged" />
          {!compMeasured || compRows.length === 0 ? (
            <EmptyState title={compMeasured ? 'No rows in raw or staged' : 'not yet measured'} hint="Per-feed × ISP class split of partner_clean_queue (raw = held + pending_eo · staged = ready)." />
          ) : (
            <div style={{ overflowX: 'auto' }}>
              <table style={{ ...tableStyle, minWidth: 380 }}>
                <thead>
                  <tr>
                    <th style={thStyle}>Class</th>
                    <th style={thStyle} />
                    <th style={numTh}>Raw</th>
                    <th style={numTh}>Staged</th>
                  </tr>
                </thead>
                <tbody>
                  {compRows.map((c) => (
                    <tr key={c.isp}>
                      <td style={tdStyle}>{c.isp}</td>
                      <td style={{ ...tdStyle, width: '45%' }}>
                        <ProgressBar pct={compMax > 0 ? ((c.raw ?? 0) + (c.staged ?? 0)) / (compMax * 2) : 0} height={8} />
                      </td>
                      <td style={numTd}><Measured fields={f} name="composition_raw" value={c.raw} /></td>
                      <td style={numTd}><Measured fields={f} name="composition_staged" value={c.staged} color={colors.indigo200} /></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>

        <Panel>
          <SectionHeader
            title={`Loads received ${date}`}
            right={<span style={{ fontSize: 11, color: colors.textMuted }}>each file, batch or insert run · where those rows are now</span>}
          />
          {measured(f, 'loads') === 'not_measured' ? (
            <EmptyState title="not yet measured" hint="The per-batch lookup for this day did not complete." />
          ) : (d?.loads ?? []).length === 0 ? (
            <EmptyState title="No loads recorded" hint="A load appears here once a batch row exists for it — a direct insert with no batch id is invisible by design." />
          ) : (
            <div style={{ overflowX: 'auto' }}>
              <table style={{ ...tableStyle, minWidth: 720 }}>
                <thead>
                  <tr>
                    <th style={thStyle}>Received</th>
                    <th style={thStyle}>Load</th>
                    <th style={numTh}>Records</th>
                    <th style={numTh}>Raw</th>
                    <th style={numTh}>Staged</th>
                    <th style={numTh}>Mailed</th>
                  </tr>
                </thead>
                <tbody>
                  {(d?.loads ?? []).map((l) => (
                    <tr key={l.batch_id}>
                      <td style={{ ...tdStyle, color: colors.textMuted, fontSize: 11 }}>{presentString(l.received_at) ?? '—'}</td>
                      <td style={tdStyle}>
                        <div>{l.name || l.batch_id.slice(0, 8)}</div>
                        <div style={{ fontSize: 10, color: colors.textFaint, fontFamily: 'monospace' }} title={l.batch_id}>
                          {l.ref}
                        </div>
                      </td>
                      <td style={numTd}><Measured fields={f} name="loads" value={l.records} /></td>
                      <td style={numTd}><Measured fields={f} name="loads" value={l.raw} /></td>
                      <td style={numTd}><Measured fields={f} name="loads" value={l.staged} color={colors.indigo200} /></td>
                      <td style={numTd}><Measured fields={f} name="loads" value={l.mailed} color={colors.successText} /></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>
      </div>

      <div style={{ fontSize: 11, color: colors.textFaint }}>
        Grain = one partner_inbound_batches row per file. {d?.as_of ? `As of ${d.as_of} · ${d.source}.` : ''}
        {' '}<Pill color={colors.idle}>{date}</Pill>
      </div>
    </div>
  );
};

export default FeedPanel;
