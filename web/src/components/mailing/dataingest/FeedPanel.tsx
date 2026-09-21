// FeedPanel — board 2 ("Feed detail").
//
// One feed = partner × dataset → lane. Shows the four REAL switches as one
// status, drives them through the endpoints that already exist
// (/api/mailing/data-partners/datasets/{id}/emergency-stop | /resume | /express),
// then the funnel, 30 days of landings, raw-vs-staged composition and the loads.
//
// A failed control is STICKY and never looks like success (the REQ-004 rule the
// Data Partners screen learned the hard way): the 30s poll clears the read
// error, never the action error.

import React, { useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faArrowLeft, faExclamationTriangle, faBolt, faPause, faPlay, faRoute, faLayerGroup } from '@fortawesome/free-solid-svg-icons';
import { ResponsiveContainer, BarChart, Bar, XAxis, YAxis, Tooltip, CartesianGrid } from 'recharts';
import { usePolling } from '../shared/usePolling';
import { Panel, SectionHeader, SectionError, EmptyState, Pill, ProgressBar } from '../shared/ui';
import { colors, tableStyle, thStyle, tdStyle, numTd, numTh, btnStyle } from '../shared/theme';
import { Measured } from './Measured';
import { dataIngestApi, datasetAction, measured, type FeedDetailResponse } from './api';

const POLL_MS = 30_000;

interface Props {
  datasetId: string;
  date: string;
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

export const FeedPanel: React.FC<Props> = ({ datasetId, date, onBack }) => {
  const [actionError, setActionError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const feed = usePolling<FeedDetailResponse>(
    (signal) => dataIngestApi.feed(datasetId, date, signal),
    POLL_MS,
    [datasetId, date],
  );
  const d = feed.data;
  const f = d?.fields;

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

  const series = (d?.series ?? []).map((s) => ({ day: s.day.slice(5), Landed: s.n }));
  const comp = d?.composition ?? [];
  const compMax = comp.reduce((m, c) => Math.max(m, typeof c.raw === 'number' ? c.raw : 0, typeof c.staged === 'number' ? c.staged : 0), 0);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
        <button type="button" style={btnStyle} onClick={onBack}>
          <FontAwesomeIcon icon={faArrowLeft} style={{ marginRight: 6 }} /> Data Ingest
        </button>
        <div>
          <div style={{ fontSize: 18, fontWeight: 700, color: colors.heading }}>
            {d ? `${d.partner_name ? `${d.partner_name} → ` : ''}${d.name}` : 'Feed'}
          </div>
          <div style={{ fontSize: 11, color: colors.textMuted, fontFamily: 'monospace' }}>
            dataset {datasetId} {d ? `· lane ${d.lane || '—'} · ${d.supply_class} · ${d.source_channel || 'source channel unknown'}` : ''}
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

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(340px, 1fr))', gap: 14 }}>
        <Panel>
          <SectionHeader title="Status and controls" icon={faRoute} />
          <div style={{ fontSize: 11, color: colors.textMuted, marginBottom: 8 }}>
            The four real switches, shown as one status. Every change writes the audit log.
          </div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
            <SwitchRow label="Ingestion (paused_emergency)" state={d ? d.status.ingest_open : null} onLabel="open" offLabel="paused" />
            <SwitchRow label="Send (partner_drip_state row)" state={d ? d.status.send_row : null} onLabel="present" offLabel="absent" />
            <SwitchRow label="Express dispatch" state={d ? d.status.express : null} onLabel="on" offLabel="off" />
            <SwitchRow label="Supply contract (mediator)" state={d ? d.status.contract : null} onLabel="active" offLabel="none" />
          </div>
          {d?.status_note && <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 6 }}>{d.status_note}</div>}
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
            <button type="button" disabled={busy} onClick={() => toggleExpress(!(d?.status.express === true))} style={btnStyle}>
              <FontAwesomeIcon icon={faBolt} style={{ marginRight: 6 }} />
              {d?.status.express === true ? 'Turn express off' : 'Turn express on'}
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
              ['landed', 'Landed', d?.funnel.landed, colors.text],
              ['raw', 'Raw', d?.funnel.raw, colors.text],
              ['cleaned', 'Cleaned', d?.funnel.cleaned, colors.text],
              ['staged', 'Staged', d?.funnel.staged, colors.indigo200],
              ['mailed', 'Mailed', d?.funnel.mailed, colors.successText],
              ['engaged', 'Engaged', d?.funnel.engaged, colors.warningText],
            ] as const).map(([key, label, value, color]) => (
              <div key={key} style={{ background: 'rgba(15,23,42,0.55)', border: `1px solid ${colors.panelBorder}`, borderRadius: 10, padding: '10px 12px' }}>
                <div style={{ fontSize: 10, color: colors.textMuted, textTransform: 'uppercase', letterSpacing: 0.6 }}>{label}</div>
                <div style={{ fontSize: 18, fontWeight: 700 }}>
                  <Measured fields={f} name={`funnel.${key}`} value={value ?? null} color={color} />
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
                  <Bar dataKey="Landed" fill={colors.indigo500} radius={[3, 3, 0, 0]} />
                </BarChart>
              </ResponsiveContainer>
            )}
          </div>
        </Panel>
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(380px, 1fr))', gap: 14 }}>
        <Panel>
          <SectionHeader title="Composition · raw vs staged" />
          {comp.length === 0 ? (
            <EmptyState title="not yet measured" hint="Per-feed × ISP class split is fed by the counters' per-ISP hashes." />
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
                  {comp.map((c) => (
                    <tr key={c.isp}>
                      <td style={tdStyle}>{c.isp}</td>
                      <td style={{ ...tdStyle, width: '45%' }}>
                        <ProgressBar pct={compMax > 0 && typeof c.raw === 'number' ? c.raw / compMax : 0} height={8} />
                      </td>
                      <td style={numTd}><Measured fields={f} name="composition.raw" value={c.raw} /></td>
                      <td style={numTd}><Measured fields={f} name="composition.staged" value={c.staged} color={colors.indigo200} /></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>

        <Panel>
          <SectionHeader
            title="Loads"
            right={<span style={{ fontSize: 11, color: colors.textMuted }}>each file, batch or insert run · where those rows are now</span>}
          />
          {(d?.loads ?? []).length === 0 ? (
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
                      <td style={{ ...tdStyle, color: colors.textMuted, fontSize: 11 }}>{l.received_at ?? '—'}</td>
                      <td style={tdStyle}>
                        <div>{l.source_path}</div>
                        <div style={{ fontSize: 10, color: colors.textFaint, fontFamily: 'monospace' }} title={l.s3_key ?? undefined}>
                          {l.object && l.s3_bucket ? `${l.s3_bucket}/${(l.s3_key ?? '').slice(0, 42)}` : 'no object'}
                        </div>
                      </td>
                      <td style={numTd}><Measured fields={f} name="loads.records" value={l.records} /></td>
                      <td style={numTd}><Measured fields={f} name="loads.raw" value={l.raw} /></td>
                      <td style={numTd}><Measured fields={f} name="loads.staged" value={l.staged} color={colors.indigo200} /></td>
                      <td style={numTd}><Measured fields={f} name="loads.mailed" value={l.mailed} color={colors.successText} /></td>
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
