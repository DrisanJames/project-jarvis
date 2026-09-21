// YesterdayPanel — board 3 ("What arrived on <day>").
//
// Every record that landed on one Denver day, across all feeds and our own
// repository, and whether we have mailed it since. Defaults to D-1 of the
// selected day; prev/next walk the days. Reads GET /loads?date=.
//
// Shapes are the Go loadsResponse (api.ts): totals {landed, mailed, not_mailed,
// removed, duplicates} with the same FLAT flag names; per-load cells ride the
// `loads` flag, composition rides `composition`, sources rides `sources`.
// When the server sets `note` (the day range timed out) the list is NOT an
// empty day: the note prints verbatim and the "stamping gap" hint never shows.

import React, { useEffect, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faChevronLeft, faChevronRight, faBoxOpen } from '@fortawesome/free-solid-svg-icons';
import { usePolling } from '../shared/usePolling';
import { Panel, SectionHeader, SectionError, EmptyState, ProgressBar, Pill } from '../shared/ui';
import { colors, tableStyle, thStyle, tdStyle, numTd, numTh, btnStyle } from '../shared/theme';
import { denverToday } from '../shared/filters';
import { Measured } from './Measured';
import { NoteBanner, FreshnessLine } from './Envelope';
import { dataIngestApi, measured, presentString, type LoadsResponse } from './api';

const POLL_MS = 30_000;

/** Shift a YYYY-MM-DD Denver day by n days (noon-UTC anchor keeps DST safe). */
export const shiftDay = (day: string, n: number): string => {
  const t = Date.parse(`${day}T12:00:00Z`);
  if (Number.isNaN(t)) return day;
  return new Date(t + n * 86_400_000).toISOString().slice(0, 10);
};

interface Props {
  /** the portal's selected day; this panel opens on the day BEFORE it */
  date: string;
}

export const YesterdayPanel: React.FC<Props> = ({ date }) => {
  const [day, setDay] = useState(() => shiftDay(date, -1));
  useEffect(() => { setDay(shiftDay(date, -1)); }, [date]);

  const loads = usePolling<LoadsResponse>((signal) => dataIngestApi.loads(day, signal), POLL_MS, [day]);
  const d = loads.data;
  const f = d?.fields;

  const comp = d?.composition ?? [];
  const compMax = comp.reduce((m, c) => Math.max(m, typeof c.n === 'number' ? c.n : 0), 0);
  const atToday = day >= denverToday();
  const loadsMeasured = measured(f, 'loads') !== 'not_measured';
  const degraded = presentString(d?.note);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
        <div>
          <div style={{ fontSize: 18, fontWeight: 700, color: colors.heading }}>What arrived on {day}</div>
          <div style={{ fontSize: 11, color: colors.textMuted }}>
            every record that landed that Denver day, across all feeds and our own repository, and whether we have mailed it since
          </div>
        </div>
        <div style={{ marginLeft: 'auto', display: 'flex', gap: 6 }}>
          <button type="button" style={btnStyle} onClick={() => setDay(shiftDay(day, -1))}>
            <FontAwesomeIcon icon={faChevronLeft} style={{ marginRight: 6 }} />{shiftDay(day, -1).slice(5)}
          </button>
          <button type="button" style={btnStyle} disabled={atToday} onClick={() => setDay(shiftDay(day, 1))}>
            {shiftDay(day, 1).slice(5)}<FontAwesomeIcon icon={faChevronRight} style={{ marginLeft: 6 }} />
          </button>
        </div>
      </div>

      {loads.error && !d && <SectionError label="That day's loads" error={loads.error} onRetry={loads.refresh} />}
      {loads.error && d && (
        <div style={{ fontSize: 12, color: colors.warningText }}>Showing the last good read — refresh failed: {loads.error}</div>
      )}
      <NoteBanner note={degraded} label="Loads" />

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(190px, 1fr))', gap: 12 }}>
        {([
          ['landed', 'Landed', 'records persisted from the batches received that day', colors.text],
          ['mailed', 'Mailed since', 'of those records, rows with a mailed_at', colors.successText],
          ['not_mailed', 'Not yet mailed', 'landed − mailed − removed: staged (ready) plus raw (held / pending EO)', colors.warningText],
          ['removed', 'Removed by cleaning', 'EO undeliverable / trap / suppressed, from that day’s rows', colors.dangerText],
          ['duplicates', 'Duplicates of known', 'declared by the loads minus persisted (the dedup drop)', colors.textMuted],
        ] as const).map(([key, label, hint, color]) => (
          <div key={key} style={{ background: 'rgba(15,23,42,0.55)', border: `1px solid ${colors.panelBorder}`, borderRadius: 10, padding: '12px 14px' }}>
            <div style={{ fontSize: 10, color: colors.textMuted, textTransform: 'uppercase', letterSpacing: 0.6 }}>{label}</div>
            <div style={{ fontSize: 22, fontWeight: 700, lineHeight: 1.3 }}>
              <Measured fields={f} name={key} value={d?.totals[key] ?? null} color={color} />
            </div>
            <div style={{ fontSize: 11, color: colors.textMuted }}>{hint}</div>
          </div>
        ))}
      </div>

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(420px, 1fr))', gap: 14 }}>
        <Panel>
          <SectionHeader
            title="By load"
            icon={faBoxOpen}
            right={<span style={{ fontSize: 11, color: colors.textMuted }}>each file / batch / insert run that day</span>}
          />
          {!loadsMeasured ? (
            <EmptyState title="not yet measured" hint={degraded ?? 'The per-batch lookup for this day did not complete.'} />
          ) : (d?.loads ?? []).length === 0 ? (
            <EmptyState title="No loads recorded for this day" hint="A landing path with no batch id cannot appear here — that is the stamping gap, not an empty day." />
          ) : (
            <div style={{ overflowX: 'auto' }}>
              <table style={{ ...tableStyle, minWidth: 760 }}>
                <thead>
                  <tr>
                    <th style={thStyle}>Load</th>
                    <th style={numTh}>Records</th>
                    <th style={numTh}>Mailed</th>
                    <th style={numTh}>Staged</th>
                    <th style={numTh}>Raw</th>
                    <th style={numTh}>Removed</th>
                  </tr>
                </thead>
                <tbody>
                  {(d?.loads ?? []).map((l) => (
                    <tr key={l.batch_id}>
                      <td style={tdStyle}>
                        <div>{l.dataset || l.source_path || l.batch_id.slice(0, 8)}</div>
                        <div style={{ fontSize: 10, color: colors.textFaint, fontFamily: 'monospace' }} title={`${l.batch_id}${l.s3_key ? ` · ${l.s3_key}` : ''}`}>
                          {presentString(l.received_at) ?? 'received —'} · {l.source_path || 'no source_path'} · {l.object && l.s3_bucket ? `s3://${l.s3_bucket}` : 'no object'} · {l.supply_class || 'unclassed'}
                        </div>
                      </td>
                      <td style={numTd}><Measured fields={f} name="loads" value={l.records} /></td>
                      <td style={numTd}><Measured fields={f} name="loads" value={l.mailed} color={colors.successText} /></td>
                      <td style={numTd}><Measured fields={f} name="loads" value={l.staged} color={colors.indigo200} /></td>
                      <td style={numTd}><Measured fields={f} name="loads" value={l.raw} /></td>
                      <td style={numTd}><Measured fields={f} name="loads" value={l.removed} color={colors.dangerText} /></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>

        <Panel>
          <SectionHeader title="Composition of that day" right={<span style={{ fontSize: 11, color: colors.textMuted }}>landed rows by canonical ISP class</span>} />
          {measured(f, 'composition') === 'not_measured' ? (
            <EmptyState title="not yet measured" hint="The day's own composition comes from the batches' rows in partner_clean_queue." />
          ) : comp.length === 0 ? (
            <EmptyState title="No rows landed" hint="No batch received that day has rows in partner_clean_queue." />
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
              {comp.map((c) => (
                <div key={c.isp} style={{ display: 'grid', gridTemplateColumns: '92px 1fr 92px 92px', alignItems: 'center', gap: 10, fontSize: 12 }}>
                  <div>{c.isp}</div>
                  <ProgressBar pct={compMax > 0 && typeof c.n === 'number' ? c.n / compMax : 0} height={9} />
                  <div style={{ textAlign: 'right' }}><Measured fields={f} name="composition" value={c.n} /></div>
                  <div style={{ textAlign: 'right' }}><Measured fields={f} name="composition" value={c.mailed ?? null} color={colors.successText} /></div>
                </div>
              ))}
              <div style={{ fontSize: 10, color: colors.textFaint, textAlign: 'right' }}>landed · mailed</div>
            </div>
          )}

          <div style={{ marginTop: 14 }}>
            <SectionHeader title="Sources that day" />
            {measured(f, 'sources') === 'not_measured' ? (
              <EmptyState title="not yet measured" hint="One row per landing path (API → slicer, nightly inject, hydration, site events, pulls)." />
            ) : (d?.sources ?? []).length === 0 ? (
              <EmptyState title="No sources" hint="No batch was received that day." />
            ) : (
              <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
                {(d?.sources ?? []).map((s) => (
                  <div
                    key={s.name}
                    style={{
                      display: 'flex', justifyContent: 'space-between', gap: 10, padding: '6px 10px',
                      background: 'rgba(15,23,42,0.55)', border: `1px solid ${colors.panelBorder}`, borderRadius: 8, fontSize: 12,
                    }}
                  >
                    <span>{s.name === 'unknown' ? <span style={{ color: colors.textFaint }}>source_path not stamped</span> : s.name}</span>
                    <Measured fields={f} name="sources" value={s.n} />
                  </div>
                ))}
              </div>
            )}
          </div>
        </Panel>
      </div>

      <div style={{ fontSize: 11, color: colors.textFaint }}>
        <Pill color={colors.idle}>{day}</Pill>{' '}
        America/Denver operating day
        <FreshnessLine asOf={d?.as_of} source={d?.source} />
      </div>
    </div>
  );
};

export default YesterdayPanel;
