import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faCalendarDay, faRotateRight } from '@fortawesome/free-solid-svg-icons';
import apiFetch from '../shared/apiFetch';
import { colors, alpha, pageStyle } from '../shared/theme';
import { Panel, SectionHeader, Stat, SectionError, EmptyState } from '../shared/ui';
import { denverToday } from '../shared/filters';

// Send-Day Schedule — the day-clock. Live twin of schedule_report.py: one
// timeline per lane (board waves + sidecars), each showing its send window,
// route (PMTA solid / SES hatched), offer mix and recipients. Reads
// GET /api/mailing/schedule/day?date=YYYY-MM-DD.
const PAGE_VERSION = '1.0.0';

// offer → color (kept local; these are domain identities, like series colors)
const OFFER_COLOR: Record<string, string> = {
  'metal-roofing': '#e0862f',
  'liberty-mutual': '#4a8fd6',
  'sams-club': '#3fa66a',
  'fidelity': '#b06fd6',
};
const OFFER_NAME: Record<string, string> = {
  'metal-roofing': 'Metal Roofing', 'liberty-mutual': 'Liberty Mutual',
  'sams-club': "Sam's Club", 'fidelity': 'Fidelity',
};
const offerColor = (o: string) => OFFER_COLOR[o] ?? colors.indigo400;

interface Lane {
  name: string; label: string; meta?: string; start: string; window: number;
  n: number; rec: number; offers: Record<string, number>; esp: Record<string, number>;
  route: string; gated?: boolean;
}
interface ScheduleModel {
  date: string; day_label: string; span_start_ms: number; span_end_ms: number;
  total_campaigns: number; total_recipients: number; lanes: number; offers: string[];
  board_waves: Lane[]; sidecars: Lane[];
}

const LANE_W = 190;
const fmtInt = (n: number) => n.toLocaleString();
const pad = (n: number) => String(n).padStart(2, '0');
const hhz = (ms: number) => { const d = new Date(ms); return `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}`; };
const mdt = (ms: number) => { const d = new Date(ms - 6 * 3600e3); return `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}`; };
const dominant = (offers: Record<string, number>) =>
  Object.entries(offers).sort((a, b) => b[1] - a[1])[0]?.[0] ?? 'other';

const OfferBar: React.FC<{ offers: Record<string, number> }> = ({ offers }) => {
  const tot = Object.values(offers).reduce((a, b) => a + b, 0) || 1;
  return (
    <div style={{ display: 'flex', height: 9, borderRadius: 3, overflow: 'hidden', flex: 1,
      minWidth: 40, maxWidth: 230, border: `1px solid ${alpha('#000000', '33')}` }} title="offer mix">
      {Object.entries(offers).sort((a, b) => b[1] - a[1]).map(([k, v]) => (
        <span key={k} title={`${OFFER_NAME[k] ?? k} ×${v}`}
          style={{ width: `${(v / tot) * 100}%`, height: '100%', background: offerColor(k) }} />
      ))}
    </div>
  );
};

const Ruler: React.FC<{ t0: number; t1: number }> = ({ t0, t1 }) => {
  const span = t1 - t0 || 1;
  const ticks: React.ReactNode[] = [];
  for (let h = 0; h <= 21; h += 3) {
    const t = t0 + h * 3600e3;
    const left = ((t - t0) / span) * 100;
    ticks.push(
      <div key={h} style={{ position: 'absolute', top: 0, bottom: -2000, width: 1, left: `${left}%`,
        background: h % 6 === 0 ? colors.hairline : colors.divider, zIndex: 0 }}>
        <span style={{ position: 'absolute', top: 2, left: 5, fontSize: 10.5, whiteSpace: 'nowrap',
          color: colors.textFaint, fontVariantNumeric: 'tabular-nums' }}>
          {hhz(t)}Z<br /><span style={{ color: colors.textMuted }}>{mdt(t)} MDT</span>
        </span>
      </div>,
    );
  }
  return <div style={{ position: 'relative', height: 34, marginLeft: LANE_W,
    borderBottom: `1px solid ${colors.hairline}`, minWidth: 640 }}>{ticks}</div>;
};

const LaneRow: React.FC<{ lane: Lane; t0: number; t1: number; last: boolean }> = ({ lane, t0, t1, last }) => {
  const span = t1 - t0 || 1;
  const st = new Date(lane.start).getTime();
  const en = st + lane.window * 3600e3;
  const pct = (t: number) => ((t - t0) / span) * 100;
  const dom = dominant(lane.offers);
  const stripe = offerColor(dom);
  const ses = lane.route === 'SES';
  return (
    <div style={{ position: 'relative', display: 'flex', alignItems: 'stretch', minHeight: 56,
      borderBottom: last ? 'none' : `1px solid ${colors.divider}` }}>
      <div style={{ width: LANE_W, flex: 'none', padding: '11px 14px 11px 2px', display: 'flex',
        flexDirection: 'column', justifyContent: 'center', gap: 3 }}>
        <span style={{ fontWeight: 600, fontSize: 13.5, color: colors.text, letterSpacing: '-0.01em' }}>{lane.label}</span>
        <span style={{ fontSize: 10.5, color: colors.textFaint, fontVariantNumeric: 'tabular-nums' }}>
          {lane.meta ? `${lane.meta} · ` : ''}{lane.name}
        </span>
      </div>
      <div style={{ position: 'relative', flex: 1, margin: '8px 0' }}>
        <div title={`${lane.label} — ${lane.n} campaigns · ${fmtInt(lane.rec)} recipients`}
          style={{ position: 'absolute', top: 0, bottom: 0, left: `${pct(st)}%`, width: `${Math.max(pct(en) - pct(st), 6)}%`,
            borderRadius: 6, border: `1px solid ${lane.gated ? colors.warning : colors.panelBorder}`,
            borderStyle: lane.gated ? 'dashed' : 'solid', background: colors.panelBgSolid, display: 'flex',
            alignItems: 'center', overflow: 'hidden', minWidth: 60,
            backgroundImage: ses ? 'repeating-linear-gradient(135deg,transparent,transparent 6px,rgba(255,255,255,0.03) 6px,rgba(255,255,255,0.03) 7px)' : undefined }}>
          <span style={{ position: 'absolute', left: 0, top: 0, bottom: 0, width: 3, background: lane.gated ? colors.warning : stripe }} />
          <div style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '0 10px 0 13px', width: '100%', minWidth: 0 }}>
            <span style={{ fontSize: 11, color: colors.text, whiteSpace: 'nowrap', fontVariantNumeric: 'tabular-nums' }}>
              <b>{lane.n}</b> <span style={{ color: colors.textMuted }}>camp · {hhz(st)}Z ({mdt(st)}) · +{lane.window}h · {lane.route}</span>
            </span>
            <OfferBar offers={lane.offers} />
            {lane.gated && (
              <span style={{ fontSize: 9.5, letterSpacing: '0.1em', textTransform: 'uppercase', color: colors.warning,
                border: `1px solid ${colors.warning}`, borderRadius: 3, padding: '1px 5px', whiteSpace: 'nowrap' }}>gated</span>
            )}
          </div>
        </div>
      </div>
    </div>
  );
};

const Timeline: React.FC<{ lanes: Lane[]; t0: number; t1: number }> = ({ lanes, t0, t1 }) => (
  <div style={{ position: 'relative', background: colors.panelBg, border: `1px solid ${colors.panelBorder}`,
    borderRadius: 10, padding: '16px 18px 10px', overflowX: 'auto' }}>
    <Ruler t0={t0} t1={t1} />
    <div style={{ position: 'relative', minWidth: 640 }}>
      {lanes.map((l, i) => <LaneRow key={l.name} lane={l} t0={t0} t1={t1} last={i === lanes.length - 1} />)}
    </div>
  </div>
);

const BandHeader: React.FC<{ children: React.ReactNode }> = ({ children }) => (
  <div style={{ display: 'flex', alignItems: 'center', gap: 10, margin: '26px 0 8px' }}>
    <h2 style={{ fontSize: 11, letterSpacing: '0.2em', textTransform: 'uppercase', color: colors.textMuted, margin: 0, fontWeight: 500 }}>{children}</h2>
    <div style={{ flex: 1, height: 1, background: colors.hairline }} />
  </div>
);

export const ScheduleView: React.FC = () => {
  const [date, setDate] = useState<string>(() => denverToday());
  const [model, setModel] = useState<ScheduleModel | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [fetchedAt, setFetchedAt] = useState<string>('');

  const load = useCallback(async () => {
    setLoading(true); setError(null);
    try {
      const r = await apiFetch(`/api/mailing/schedule/day?date=${encodeURIComponent(date)}`);
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      const j = (await r.json()) as ScheduleModel;
      setModel(j);
      setFetchedAt(new Date().toLocaleTimeString());
    } catch (e) {
      setError(e instanceof Error ? e.message : 'failed to load schedule');
    } finally {
      setLoading(false);
    }
  }, [date]);

  useEffect(() => { void load(); }, [load]);

  const allLanes = useMemo(() => (model ? [...model.board_waves, ...model.sidecars] : []), [model]);

  return (
    <div style={pageStyle}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-end', gap: 24,
        paddingBottom: 18, marginBottom: 22, borderBottom: `1px solid ${colors.hairline}`, flexWrap: 'wrap' }}>
        <div>
          <div style={{ fontSize: 11, letterSpacing: '0.22em', textTransform: 'uppercase', color: colors.textFaint, marginBottom: 8 }}>
            Project Jarvis · Operating Schedule
          </div>
          <h1 style={{ fontSize: 32, lineHeight: 1.02, margin: 0, fontWeight: 600, color: colors.heading, letterSpacing: '-0.015em' }}>
            Send-Day <span style={{ color: OFFER_COLOR['metal-roofing'] }}>{model?.day_label ?? '—'}</span>
          </h1>
          <p style={{ color: colors.textMuted, fontSize: 13, margin: '8px 0 0', maxWidth: '54ch' }}>
            Every wave, its send window, and the route it rides — one day-clock so cadence stays consistent brand-to-brand.
          </p>
        </div>
        <div style={{ display: 'flex', gap: 22, alignItems: 'flex-end' }}>
          <label style={{ display: 'flex', flexDirection: 'column', gap: 4, fontSize: 10.5,
            textTransform: 'uppercase', letterSpacing: '0.12em', color: colors.textFaint }}>
            <FontAwesomeIcon icon={faCalendarDay} /> Send day
            <input type="date" value={date} onChange={(e) => setDate(e.target.value)}
              style={{ background: colors.panelBgSolid, color: colors.text, border: `1px solid ${colors.panelBorder}`,
                borderRadius: 6, padding: '6px 8px', fontSize: 13 }} />
          </label>
          {model && (
            <>
              <Stat label="Campaigns" value={model.total_campaigns} />
              <Stat label="Recipients" value={fmtInt(model.total_recipients)} />
              <Stat label="Lanes" value={model.lanes} />
              <Stat label="Offers" value={model.offers.length} />
            </>
          )}
          <button onClick={() => void load()} title="Refresh"
            style={{ background: alpha(colors.indigo500, '22'), border: `1px solid ${alpha(colors.indigo500, '66')}`,
              color: colors.indigo200, borderRadius: 6, padding: '8px 10px', cursor: 'pointer' }}>
            <FontAwesomeIcon icon={faRotateRight} spin={loading} />
          </button>
        </div>
      </div>

      {/* legend */}
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: '14px 24px', alignItems: 'center', marginBottom: 20,
        fontSize: 12, color: colors.textMuted }}>
        {Object.keys(OFFER_COLOR).map((o) => (
          <span key={o} style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
            <span style={{ width: 12, height: 12, borderRadius: 3, background: offerColor(o) }} />{OFFER_NAME[o]}
          </span>
        ))}
        <span style={{ marginLeft: 'auto', display: 'flex', gap: 10, fontSize: 10.5 }}>
          <span style={{ border: `1px solid ${colors.panelBorder}`, borderRadius: 4, padding: '2px 7px' }}>PMTA solid</span>
          <span style={{ border: `1px solid ${colors.panelBorder}`, borderRadius: 4, padding: '2px 7px',
            backgroundImage: 'repeating-linear-gradient(135deg,transparent,transparent 3px,rgba(255,255,255,0.08) 3px,rgba(255,255,255,0.08) 4px)' }}>SES hatched</span>
        </span>
      </div>

      {error && <SectionError label="Schedule" error={error} onRetry={() => void load()} />}
      {!error && model && allLanes.length === 0 && (
        <EmptyState icon={faCalendarDay} title="No campaigns scheduled for this day"
          hint="Pick another send day, or deploy a board — this reflects what's live in mailing_campaigns." />
      )}

      {!error && model && model.board_waves.length > 0 && (
        <>
          <BandHeader>The Board — engaged tier</BandHeader>
          <Timeline lanes={model.board_waves} t0={model.span_start_ms} t1={model.span_end_ms} />
        </>
      )}
      {!error && model && model.sidecars.length > 0 && (
        <>
          <BandHeader>Sidecars &amp; peppering lanes</BandHeader>
          <Timeline lanes={model.sidecars} t0={model.span_start_ms} t1={model.span_end_ms} />
        </>
      )}

      {/* lane detail table */}
      {!error && model && allLanes.length > 0 && (
        <>
          <BandHeader>Lane detail</BandHeader>
          <Panel>
            <SectionHeader title="Lanes" />
            <div style={{ overflowX: 'auto' }}>
              <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 12.5 }}>
                <thead>
                  <tr style={{ textAlign: 'left', color: colors.textMuted, borderBottom: `1px solid ${colors.hairline}` }}>
                    <th style={{ padding: '6px 8px' }}>Lane</th>
                    <th style={{ padding: '6px 8px' }}>Start</th>
                    <th style={{ padding: '6px 8px' }}>Window</th>
                    <th style={{ padding: '6px 8px' }}>Route</th>
                    <th style={{ padding: '6px 8px', textAlign: 'right' }}>Campaigns</th>
                    <th style={{ padding: '6px 8px', textAlign: 'right' }}>Recipients</th>
                    <th style={{ padding: '6px 8px' }}>Offers</th>
                  </tr>
                </thead>
                <tbody>
                  {allLanes.map((l) => {
                    const st = new Date(l.start).getTime();
                    return (
                      <tr key={l.name} style={{ borderBottom: `1px solid ${colors.divider}` }}>
                        <td style={{ padding: '6px 8px', color: colors.text }}>
                          {l.label}{l.gated && <span style={{ color: colors.warning, marginLeft: 6, fontSize: 10 }}>GATED</span>}
                        </td>
                        <td style={{ padding: '6px 8px', color: colors.textMuted, fontVariantNumeric: 'tabular-nums' }}>{hhz(st)}Z ({mdt(st)} MDT)</td>
                        <td style={{ padding: '6px 8px', color: colors.textMuted }}>+{l.window}h</td>
                        <td style={{ padding: '6px 8px', color: colors.textMuted }}>{l.route}</td>
                        <td style={{ padding: '6px 8px', textAlign: 'right', color: colors.text, fontVariantNumeric: 'tabular-nums' }}>{l.n}</td>
                        <td style={{ padding: '6px 8px', textAlign: 'right', color: colors.text, fontVariantNumeric: 'tabular-nums' }}>{fmtInt(l.rec)}</td>
                        <td style={{ padding: '6px 8px', color: colors.textMuted }}>
                          {Object.entries(l.offers).sort((a, b) => b[1] - a[1]).map(([o, c]) => `${OFFER_NAME[o] ?? o} ×${c}`).join(' · ')}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
                <tfoot>
                  <tr style={{ borderTop: `1px solid ${colors.hairline}`, fontWeight: 600 }}>
                    <td style={{ padding: '6px 8px', color: colors.text }} colSpan={4}>Total</td>
                    <td style={{ padding: '6px 8px', textAlign: 'right', color: colors.text, fontVariantNumeric: 'tabular-nums' }}>{model.total_campaigns}</td>
                    <td style={{ padding: '6px 8px', textAlign: 'right', color: colors.text, fontVariantNumeric: 'tabular-nums' }}>{fmtInt(model.total_recipients)}</td>
                    <td />
                  </tr>
                </tfoot>
              </table>
            </div>
          </Panel>
        </>
      )}

      <div style={{ marginTop: 22, paddingTop: 14, borderTop: `1px solid ${colors.hairline}`,
        fontSize: 11, color: colors.textFaint, display: 'flex', justifyContent: 'space-between', flexWrap: 'wrap', gap: 10 }}>
        <span>All times UTC · Denver (MDT = UTC−6) in parentheses · window = hours · v{PAGE_VERSION}</span>
        <span>{fetchedAt ? `fetched ${fetchedAt}` : ''}</span>
      </div>
    </div>
  );
};

export default ScheduleView;
