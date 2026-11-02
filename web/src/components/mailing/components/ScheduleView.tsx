import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faCalendarDay, faRotateRight } from '@fortawesome/free-solid-svg-icons';
import apiFetch from '../shared/apiFetch';
import { colors, alpha, pageStyle } from '../shared/theme';
import { Panel, SectionHeader, Stat, SectionError, EmptyState } from '../shared/ui';
import { denverToday } from '../shared/filters';

// Send-Day Schedule — the day-clock. One band per transport (SES, PMTA, Kumo)
// with every lane mailing on it: send window, offer mix, campaigns, recipients.
// Reads GET /api/mailing/schedule/day?date=YYYY-MM-DD.
// PAGE_VERSION 2.0.0 — all lanes (operator 2026-11-02): the old board/sidecar
// split matched retired W1/W2 names and guessed transport from the domain prefix.
const PAGE_VERSION = '2.0.0';

// Offers are not a fixed list, so each one takes a stable color from its name.
const OFFER_PALETTE = ['--j-blue', '--j-violet', '--j-success', '--j-soft', '--j-cyan', '--j-pink'];
const offerColor = (o: string) => {
  let h = 0;
  for (let i = 0; i < o.length; i++) h = (h * 31 + o.charCodeAt(i)) >>> 0;
  return `var(${OFFER_PALETTE[h % OFFER_PALETTE.length]})`;
};
const offerName = (o: string) => o.replace(/-/g, ' ');

interface Lane {
  name: string; label: string; start: string; window: number;
  n: number; rec: number; offers: Record<string, number>;
}
interface Band { key: string; label: string; n: number; rec: number; lanes: Lane[] }
interface ScheduleModel {
  date: string; day_label: string; span_start_ms: number; span_end_ms: number;
  total_campaigns: number; total_recipients: number; lanes: number; offers: string[];
  bands: Band[];
}

const LANE_W = 190;
const fmtInt = (n: number) => n.toLocaleString();
const pad = (n: number) => String(n).padStart(2, '0');
const hhz = (ms: number) => { const d = new Date(ms); return `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}`; };
const mdt = (ms: number) => new Date(ms).toLocaleTimeString('en-GB', { timeZone: 'America/Denver', hour: '2-digit', minute: '2-digit' });
const dominant = (offers: Record<string, number>) =>
  Object.entries(offers).sort((a, b) => b[1] - a[1])[0]?.[0] ?? 'other';

const OfferBar: React.FC<{ offers: Record<string, number> }> = ({ offers }) => {
  const tot = Object.values(offers).reduce((a, b) => a + b, 0) || 1;
  return (
    <div style={{ display: 'flex', height: 9, borderRadius: 3, overflow: 'hidden', flex: 1,
      minWidth: 40, maxWidth: 230, border: `1px solid ${alpha('#000000', '33')}` }} title="offer mix">
      {Object.entries(offers).sort((a, b) => b[1] - a[1]).map(([k, v]) => (
        <span key={k} title={`${offerName(k)} ×${v}`}
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
          {mdt(t)} MT<br /><span style={{ color: colors.textMuted }}>{hhz(t)}Z</span>
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
  const stripe = offerColor(dominant(lane.offers));
  return (
    <div style={{ position: 'relative', display: 'flex', alignItems: 'stretch', minHeight: 56,
      borderBottom: last ? 'none' : `1px solid ${colors.divider}` }}>
      <div style={{ width: LANE_W, flex: 'none', padding: '11px 14px 11px 2px', display: 'flex',
        flexDirection: 'column', justifyContent: 'center', gap: 3 }}>
        <span style={{ fontWeight: 600, fontSize: 13.5, color: colors.text, letterSpacing: '-0.01em' }}>{lane.label}</span>
        <span style={{ fontSize: 10.5, color: colors.textFaint, fontVariantNumeric: 'tabular-nums' }}>
          {fmtInt(lane.rec)} recipients
        </span>
      </div>
      <div style={{ position: 'relative', flex: 1, margin: '8px 0' }}>
        <div title={`${lane.label} — ${lane.n} campaigns · ${fmtInt(lane.rec)} recipients`}
          style={{ position: 'absolute', top: 0, bottom: 0, left: `${pct(st)}%`, width: `${Math.max(pct(en) - pct(st), 6)}%`,
            borderRadius: 6, border: `1px solid ${colors.panelBorder}`, background: colors.panelBgSolid, display: 'flex',
            alignItems: 'center', overflow: 'hidden', minWidth: 60 }}>
          <span style={{ position: 'absolute', left: 0, top: 0, bottom: 0, width: 3, background: stripe }} />
          <div style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '0 10px 0 13px', width: '100%', minWidth: 0 }}>
            <span style={{ fontSize: 11, color: colors.text, whiteSpace: 'nowrap', fontVariantNumeric: 'tabular-nums' }}>
              <b>{lane.n}</b> <span style={{ color: colors.textMuted }}>camp · {mdt(st)} MT · +{lane.window}h</span>
            </span>
            <OfferBar offers={lane.offers} />
          </div>
        </div>
      </div>
    </div>
  );
};

const Timeline: React.FC<{ lanes: Lane[]; t0: number; t1: number }> = ({ lanes, t0, t1 }) => (
  <div style={{ position: 'relative', background: colors.panelBg, border: `1px solid ${colors.panelBorder}`,
    borderRadius: 10, padding: '16px 18px 10px', overflowX: 'auto' }} tabIndex={0}>
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
      setModel({ ...j, bands: j.bands ?? [], offers: j.offers ?? [] });
      setFetchedAt(new Date().toLocaleTimeString());
    } catch (e) {
      setError(e instanceof Error ? e.message : 'failed to load schedule');
    } finally {
      setLoading(false);
    }
  }, [date]);

  useEffect(() => { void load(); }, [load]);

  const bands = useMemo(() => model?.bands ?? [], [model]);

  return (
    <div style={pageStyle}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-end', gap: 24,
        paddingBottom: 18, marginBottom: 22, borderBottom: `1px solid ${colors.hairline}`, flexWrap: 'wrap' }}>
        <div>
          <div style={{ fontSize: 11, letterSpacing: '0.22em', textTransform: 'uppercase', color: colors.textFaint, marginBottom: 8 }}>
            Project Jarvis · Operating Schedule
          </div>
          <h1 style={{ fontSize: 32, lineHeight: 1.02, margin: 0, fontWeight: 600, color: colors.heading, letterSpacing: '-0.015em' }}>
            Send-Day <span style={{ color: colors.indigo400 }}>{model?.day_label ?? '—'}</span>
          </h1>
          <p style={{ color: colors.textMuted, fontSize: 13, margin: '8px 0 0', maxWidth: '54ch' }}>
            Every lane on SES, PMTA and Kumo for the day, with its send window, volume and offers.
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
              <Stat label="Campaigns" value={fmtInt(model.total_campaigns)} />
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

      {error && <SectionError label="Schedule" error={error} onRetry={() => void load()} />}
      {!error && model && model.lanes === 0 && (
        <EmptyState icon={faCalendarDay} title="No campaigns scheduled for this day"
          hint="Pick another send day, or schedule a campaign. This shows what is in mailing_campaigns." />
      )}

      {!error && model && model.lanes > 0 && bands.map((b) => (
        <React.Fragment key={b.key}>
          <BandHeader>{b.label} · {b.lanes.length} {b.lanes.length === 1 ? 'lane' : 'lanes'} · {fmtInt(b.n)} campaigns · {fmtInt(b.rec)} recipients</BandHeader>
          {b.lanes.length > 0
            ? <Timeline lanes={b.lanes} t0={model.span_start_ms} t1={model.span_end_ms} />
            : <div style={{ fontSize: 12.5, color: colors.textMuted, padding: '4px 2px' }}>Nothing scheduled on {b.label} for this day.</div>}
        </React.Fragment>
      ))}

      {/* lane detail table */}
      {!error && model && model.lanes > 0 && (
        <>
          <BandHeader>Lane detail</BandHeader>
          <Panel>
            <SectionHeader title="Lanes" />
            <div style={{ overflowX: 'auto' }}>
              <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 12.5 }}>
                <thead>
                  <tr style={{ textAlign: 'left', color: colors.textMuted, borderBottom: `1px solid ${colors.hairline}` }}>
                    <th style={{ padding: '6px 8px' }}>Sent through</th>
                    <th style={{ padding: '6px 8px' }}>Lane</th>
                    <th style={{ padding: '6px 8px' }}>Start</th>
                    <th style={{ padding: '6px 8px' }}>Window</th>
                    <th style={{ padding: '6px 8px', textAlign: 'right' }}>Campaigns</th>
                    <th style={{ padding: '6px 8px', textAlign: 'right' }}>Recipients</th>
                    <th style={{ padding: '6px 8px' }}>Offers</th>
                  </tr>
                </thead>
                <tbody>
                  {bands.flatMap((b) => b.lanes.map((l) => {
                    const st = new Date(l.start).getTime();
                    return (
                      <tr key={l.name} style={{ borderBottom: `1px solid ${colors.divider}` }}>
                        <td style={{ padding: '6px 8px', color: colors.textMuted }}>{b.label}</td>
                        <td style={{ padding: '6px 8px', color: colors.text }}>{l.label}</td>
                        <td style={{ padding: '6px 8px', color: colors.textMuted, fontVariantNumeric: 'tabular-nums' }}>{mdt(st)} MT</td>
                        <td style={{ padding: '6px 8px', color: colors.textMuted }}>+{l.window}h</td>
                        <td style={{ padding: '6px 8px', textAlign: 'right', color: colors.text, fontVariantNumeric: 'tabular-nums' }}>{fmtInt(l.n)}</td>
                        <td style={{ padding: '6px 8px', textAlign: 'right', color: colors.text, fontVariantNumeric: 'tabular-nums' }}>{fmtInt(l.rec)}</td>
                        <td style={{ padding: '6px 8px', color: colors.textMuted }}>
                          {Object.entries(l.offers).sort((x, y) => y[1] - x[1]).slice(0, 6).map(([o, c]) => `${offerName(o)} ×${c}`).join(' · ')}
                          {Object.keys(l.offers).length > 6 ? ` · +${Object.keys(l.offers).length - 6} more` : ''}
                        </td>
                      </tr>
                    );
                  }))}
                </tbody>
                <tfoot>
                  <tr style={{ borderTop: `1px solid ${colors.hairline}`, fontWeight: 600 }}>
                    <td style={{ padding: '6px 8px', color: colors.text }} colSpan={4}>Total</td>
                    <td style={{ padding: '6px 8px', textAlign: 'right', color: colors.text, fontVariantNumeric: 'tabular-nums' }}>{fmtInt(model.total_campaigns)}</td>
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
        <span>Times are Denver · window = hours · v{PAGE_VERSION}</span>
        <span>{fetchedAt ? `fetched ${fetchedAt}` : ''}</span>
      </div>
    </div>
  );
};

export default ScheduleView;
