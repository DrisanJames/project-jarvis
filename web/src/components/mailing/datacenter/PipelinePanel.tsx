// PipelinePanel — Data Center → Pipeline. The supply chain of never-mailed
// records on the SES lane (tasks/eng-team/requirements/2026-11-10-new-data-pipeline.md,
// screen v2): the agent that watches it, the funnel, supply at rest per
// mailbox provider family, the five-step introduction, activation, contracts
// per sending domain, throttling, and Email Oversight spend.
//
// Every number comes from GET /api/mailing/data-pipeline/summary and
// /agent (data_pipeline_daily_handlers.go, data_pipeline_agent.go). Verdicts
// (days of supply, low, over contract, above the ramp, stale agent) are
// computed server-side and displayed, never recomputed (PORTAL_DESIGN_SYSTEM
// §6.4). A missing row renders "not reported yet", never 0.

import React, { useMemo, useState } from 'react';
import {
  faFilter, faRobot, faWarehouse, faShoePrints, faBolt, faFileContract, faGaugeHigh, faBroom, faDatabase,
} from '@fortawesome/free-solid-svg-icons';
import { usePolling } from '../shared/usePolling';
import { Panel, SectionHeader, SectionError, EmptyState, Pill, ProgressBar, Stat, LivePill } from '../shared/ui';
import { colors, tableStyle, thStyle, tdStyle, numTd, numTh, btnStyle } from '../shared/theme';
import { measured, type Count, type FieldFlags } from '../dataingest/api';
import { NoteBanner, FreshnessLine } from '../dataingest/Envelope';
import {
  fetchPipelineSummary, fetchPipelineAgent, ispLabel, sourceLabel,
  type PipelineSummary, type PipelineStage, type PipelineISP, type PipelineAgentLog, type PipelineAgentEntry,
} from './pipelineApi';

const LANE = 'ses';
const DAYS = 14;
const POLL_MS = 60_000;
const NOT_REPORTED = 'not reported yet';

const FUNNEL_LABEL: Partial<Record<PipelineStage, string>> = {
  received: 'Received',
  cleaned_ok: 'Cleaned',
  clean: 'Clean reservoir',
  first_mailed: 'First mailed',
  promoted: 'Promoted to engagers',
};

const FUNNEL_HINT: Partial<Record<PipelineStage, string>> = {
  received: 'New records that arrived from partners and verticals',
  cleaned_ok: 'Records that passed cleaning',
  clean: 'Never-mailed records ready to send',
  first_mailed: 'Never-mailed people sent their first email',
  promoted: 'Opened or clicked, moved to the engaged mailing',
};

const KIND_LABEL: Record<PipelineAgentEntry['kind'], string> = {
  check: 'Check',
  decision: 'Decision',
  repair: 'Repair',
  learned: 'Learned',
  alert: 'Alert',
  report: 'Report',
};

const STATUS_LABEL: Record<string, string> = { ok: 'All good', warn: 'Needs attention', fail: 'Failing' };
const statusColor = (s: string | null | undefined) =>
  s === 'ok' ? colors.success : s === 'warn' ? colors.warning : s === 'fail' ? colors.danger : colors.idle;

const fmtInt = (n: number) => Math.round(n).toLocaleString();
const fmtDays = (n: number) => `${n.toFixed(1)} days`;
const fmtPct = (n: number) => `${n.toFixed(1)}%`;
const fmtMoney = (cents: number) =>
  `$${(cents / 100).toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
const fmtTimeMT = (iso: string) => {
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return iso;
  return `${t.toLocaleString('en-US', { timeZone: 'America/Denver', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' })} MT`;
};

const scroll: React.CSSProperties = { overflowX: 'auto' };
const subtle: React.CSSProperties = { fontSize: 11, color: colors.textMuted, marginBottom: 8 };

/** One number. null (or a stage the API says it has no rows for) reads "not reported yet". */
const N: React.FC<{ fields: FieldFlags; name: string; value: Count | undefined; format?: (n: number) => string; color?: string }> = ({
  fields, name, value, format, color,
}) => {
  if (measured(fields, name) === 'not_measured' || typeof value !== 'number' || !Number.isFinite(value)) {
    return <span style={{ color: colors.textFaint, fontStyle: 'italic' }} title={`${name}: no row posted`}>{NOT_REPORTED}</span>;
  }
  return <span style={{ color: color ?? colors.text, fontVariantNumeric: 'tabular-nums' }}>{(format ?? fmtInt)(value)}</span>;
};

/** Indented label for a sub-row (Windstream inside Long tail, AOL inside Yahoo family). */
const ISPCell: React.FC<{ isp: string; parent?: string; bold?: boolean }> = ({ isp, parent, bold }) => (
  <td style={{ ...tdStyle, paddingLeft: parent ? 26 : 10, color: parent ? colors.textMuted : colors.text, fontWeight: bold ? 700 : undefined }}>
    {ispLabel(isp)}
  </td>
);

/** A JSON object rendered as labelled lines, not a dump. */
const KeyValues: React.FC<{ obj: Record<string, unknown> | null | undefined }> = ({ obj }) => {
  if (!obj || Object.keys(obj).length === 0) return null;
  return (
    <div style={{ display: 'grid', gridTemplateColumns: 'max-content 1fr', gap: '2px 12px', fontSize: 12, marginTop: 4 }}>
      {Object.entries(obj).map(([k, v]) => (
        <React.Fragment key={k}>
          <div style={{ color: colors.textMuted }}>{k.replace(/_/g, ' ')}</div>
          <div>
            {Array.isArray(v) ? v.map((x, i) => <div key={i}>{typeof x === 'object' ? JSON.stringify(x) : String(x)}</div>)
              : typeof v === 'object' && v !== null ? JSON.stringify(v)
                : typeof v === 'number' ? v.toLocaleString() : String(v)}
          </div>
        </React.Fragment>
      ))}
    </div>
  );
};

// ── (1) agent strip ─────────────────────────────────────────────────────────

const fmtMinutes = (m: number) => (m < 120 ? `${Math.round(m)} minutes` : `${(m / 60).toFixed(1)} hours`);

const AgentStrip: React.FC = () => {
  const poll = usePolling<PipelineAgentLog>((signal) => fetchPipelineAgent(50, signal), POLL_MS, []);
  const [open, setOpen] = useState(false);
  const [showMandate, setShowMandate] = useState(false);
  const [openEntry, setOpenEntry] = useState<number | null>(null);
  const a = poll.data;
  if (poll.loading && !a) return <Panel><div style={{ fontSize: 12, color: colors.textMuted }}>Loading the pipeline agent…</div></Panel>;
  if (!a) return <SectionError label="Pipeline agent" error={poll.error ?? undefined} onRetry={poll.refresh} />;
  const h = a.header;
  if (a.entries.length === 0 && !h.last_check_at) {
    return (
      <Panel accent={colors.idle}>
        <SectionHeader title="Pipeline agent" icon={faRobot} />
        <EmptyState icon={faRobot} title="The agent has not reported yet" hint={`It counts as stopped until it posts a check; it goes stale after ${h.stale_after_minutes} minutes without one.`} />
        <FreshnessLine asOf={a.as_of} source={a.source} />
      </Panel>
    );
  }
  const running = !h.stale;
  const sc = h.scorecard;
  const day = sc?.last_24h;
  const week = sc?.last_7d;
  const recent = a.entries.filter((e) => e.kind === 'decision' || e.kind === 'alert').slice(0, 5);
  const shown = open ? a.entries : recent;
  const tile = (label: string, value: React.ReactNode, sub?: React.ReactNode, color?: string, title?: string) => (
    <Panel style={{ padding: '10px 12px' }}><Stat label={label} value={value} sub={sub} color={color} title={title} /></Panel>
  );
  const none = <i style={{ color: colors.textFaint, fontWeight: 400 }}>{NOT_REPORTED}</i>;
  return (
    <Panel accent={running ? statusColor(h.status) : colors.danger}>
      <SectionHeader
        title="Pipeline agent"
        icon={faRobot}
        right={<button type="button" style={btnStyle} onClick={() => setOpen(!open)}>{open ? 'Show last 5 decisions and alerts' : `Show all ${a.entries.length} entries`}</button>}
      />
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 24, marginBottom: 10 }}>
        <Stat label="Running" value={running ? 'Yes' : 'No'} color={running ? colors.successText : colors.dangerText}
          sub={h.last_check_at ? `stale after ${h.stale_after_minutes} minutes without a check` : 'has never posted a check'} />
        <Stat label="Last check" value={h.last_check_at ? fmtTimeMT(h.last_check_at) : NOT_REPORTED} />
        <Stat label="Overall state" value={h.status ? STATUS_LABEL[h.status] ?? h.status : NOT_REPORTED} color={statusColor(h.status)} />
      </div>
      {poll.error && <div style={{ fontSize: 12, color: colors.warningText }}>Showing the last good read — refresh failed: {poll.error}</div>}

      {/* scorecard */}
      {sc && day && week && (
        <>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(150px, 1fr))', gap: 10, marginBottom: 6 }}>
            {tile('Checks in the last 24 hours', day.checks.toLocaleString(), `${week.checks.toLocaleString()} in 7 days · ${day.cycles} cycles`)}
            {tile('Passed', day.ok.toLocaleString(), day.pass_rate != null ? `${fmtPct(day.pass_rate)} of checks (7 days: ${week.pass_rate != null ? fmtPct(week.pass_rate) : 'none'})` : 'no checks', colors.successText, 'Checks that returned ok ÷ all checks')}
            {tile('Warnings', day.warn.toLocaleString(), `${week.warn.toLocaleString()} in 7 days`, day.warn > 0 ? colors.warningText : undefined)}
            {tile('Failures', day.fail.toLocaleString(), `${week.fail.toLocaleString()} in 7 days`, day.fail > 0 ? colors.dangerText : undefined)}
            {tile('Open problems', sc.open_failures.length.toLocaleString(), 'failed checks with no later pass', sc.open_failures.length > 0 ? colors.dangerText : undefined)}
            {tile('Alerts sent', day.alerts.toLocaleString(), `${day.decisions} decisions sent to you · 7 days: ${week.alerts} alerts`)}
            {tile('Repairs made', day.repairs.toLocaleString(), `${day.learned} facts learned · 7 days: ${week.repairs} repairs`)}
            {tile('Minutes since last check', h.minutes_since_last_check != null ? Math.round(h.minutes_since_last_check).toLocaleString() : none,
              day.avg_minutes_between_cycles != null ? `cycles every ${fmtMinutes(day.avg_minutes_between_cycles)} on average · longest gap ${day.longest_gap_minutes != null ? fmtMinutes(day.longest_gap_minutes) : 'unknown'}` : (day.longest_gap_minutes != null ? `longest gap ${fmtMinutes(day.longest_gap_minutes)}` : undefined),
              h.stale ? colors.dangerText : undefined)}
            {tile('Forecast accuracy', week.forecast_mape != null ? `${fmtPct(week.forecast_mape)} average miss` : none,
              week.forecast_samples > 0 ? `over ${week.forecast_samples} forecasts in 7 days` : 'no forecasts with a measured result yet', undefined,
              'Mean absolute percent error: how far each forecast was from what actually happened')}
          </div>
          {sc.truncated && <NoteBanner label="Incomplete" note="More than 20,000 log entries in 7 days; the scorecard covers the newest 20,000." />}
          {sc.open_failures.length > 0 && (
            <div style={{ ...scroll, marginBottom: 10 }}>
              <table style={tableStyle}>
                <thead><tr><th style={thStyle}>Open problem</th><th style={thStyle}>Failing since</th><th style={numTh}>Open for</th><th style={thStyle}>Reported by</th></tr></thead>
                <tbody>
                  {sc.open_failures.map((o) => (
                    <tr key={o.title}>
                      <td style={{ ...tdStyle, color: colors.dangerText }}>{o.title}</td>
                      <td style={tdStyle}>{fmtTimeMT(o.since)}</td>
                      <td style={numTd}>{fmtMinutes(o.age_minutes)}</td>
                      <td style={tdStyle}>{o.agent}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          <div style={{ fontSize: 12, fontWeight: 700, margin: '6px 0' }}>Each check, latest result</div>
          {sc.checks_latest.length === 0 ? (
            <div style={{ fontSize: 12, color: colors.textFaint }}>No checks in the last 7 days.</div>
          ) : (
            <div style={{ ...scroll, marginBottom: 10 }}>
              <table style={tableStyle}>
                <thead><tr><th style={thStyle}>Check</th><th style={thStyle}>Latest result</th><th style={thStyle}>When</th><th style={thStyle}>Reported by</th></tr></thead>
                <tbody>
                  {sc.checks_latest.map((c) => (
                    <tr key={c.title}>
                      <td style={tdStyle}>{c.title}</td>
                      <td style={tdStyle}><Pill color={statusColor(c.status)}>{STATUS_LABEL[c.status] ?? c.status}</Pill></td>
                      <td style={tdStyle}>{fmtTimeMT(c.at)}</td>
                      <td style={tdStyle}>{c.agent}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}

      <div style={{ fontSize: 12, fontWeight: 700, margin: '6px 0' }}>{open ? 'Everything the agent posted' : 'Last 5 decisions and alerts'}</div>
      {shown.length === 0 ? (
        <div style={{ fontSize: 12, color: colors.textFaint }}>{open ? 'The agent has posted nothing yet.' : 'No decisions or alerts posted yet.'}</div>
      ) : (
        <div style={scroll}>
          <table style={tableStyle}>
            <thead>
              <tr>
                <th style={thStyle}>When</th>
                <th style={thStyle}>Type</th>
                <th style={thStyle}>State</th>
                <th style={thStyle}>Mailbox provider</th>
                <th style={thStyle}>What happened</th>
                <th style={thStyle}>Reported by</th>
              </tr>
            </thead>
            <tbody>
              {shown.map((e, i) => {
                const key = e.id ?? i;
                const { agent: _agent, ...details } = e.body ?? {};
                return (
                  <React.Fragment key={key}>
                    <tr style={{ cursor: 'pointer' }} onClick={() => setOpenEntry(openEntry === key ? null : key)}>
                      <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>{fmtTimeMT(e.at)}</td>
                      <td style={tdStyle}>{KIND_LABEL[e.kind] ?? e.kind}</td>
                      <td style={tdStyle}><Pill color={statusColor(e.status)}>{STATUS_LABEL[e.status] ?? e.status}</Pill></td>
                      <td style={tdStyle}>{e.isp ? ispLabel(e.isp) : 'All'}</td>
                      <td style={tdStyle}>{openEntry === key ? '▾' : '▸'} {e.title}</td>
                      <td style={tdStyle}>{e.agent}</td>
                    </tr>
                    {openEntry === key && (
                      <tr><td colSpan={6} style={{ ...tdStyle, paddingLeft: 30 }}><KeyValues obj={details} /></td></tr>
                    )}
                  </React.Fragment>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      <div style={{ marginTop: 10 }}>
        <button type="button" style={{ ...btnStyle, padding: '4px 10px' }} onClick={() => setShowMandate(!showMandate)}>
          {showMandate ? '▾' : '▸'} What the agent is allowed to do
        </button>
        {showMandate && (h.mandate ? (
          <>
            <KeyValues obj={h.mandate} />
            {h.mandate_at && <div style={{ fontSize: 11, color: colors.textFaint, marginTop: 4 }}>posted {fmtTimeMT(h.mandate_at)}</div>}
          </>
        ) : <div style={{ fontSize: 12, color: colors.textFaint, fontStyle: 'italic', marginTop: 4 }}>{NOT_REPORTED}</div>)}
      </div>
      <FreshnessLine asOf={a.as_of} source={a.source} />
    </Panel>
  );
};

// ── screen ──────────────────────────────────────────────────────────────────

export const PipelinePanel: React.FC = () => {
  const poll = usePolling<PipelineSummary>((signal) => fetchPipelineSummary(LANE, DAYS, signal), POLL_MS, []);
  const d = poll.data;
  const f: FieldFlags = d?.fields ?? {};
  const [openSource, setOpenSource] = useState<string | null>(null);

  const families = useMemo(() => (d?.isps ?? []), [d]);
  const lowISPs = useMemo(() => families.filter((x) => x.low), [families]);
  const parentOf = useMemo(() => new Map(families.map((x) => [x.isp as string, x.parent ?? ''])), [families]);
  const domains = useMemo(
    () => [...(d?.domains ?? [])].sort((a, b) => (b.contract ?? -1) - (a.contract ?? -1)),
    [d],
  );
  const cohorts = useMemo(
    () => [...(d?.activation ?? [])].filter((c) => !parentOf.get(c.isp)).sort((a, b) => (a.day < b.day ? 1 : a.day > b.day ? -1 : 0)),
    [d, parentOf],
  );
  const nights = useMemo(
    () => [...(d?.cleaning ?? [])].sort((a, b) => (a.day < b.day ? 1 : a.day > b.day ? -1 : 0)),
    [d],
  );

  const header = (
    <SectionHeader
      title="New data pipeline — SES lane"
      icon={faFilter}
      right={
        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <LivePill live={poll.live} agoSeconds={poll.secondsSinceUpdate} />
          <button type="button" style={btnStyle} onClick={poll.refresh}>Refresh</button>
        </div>
      }
    />
  );

  let body: React.ReactNode;
  if (poll.loading && !d) {
    body = <Panel>{header}<div style={{ fontSize: 12, color: colors.textMuted, padding: '12px 0' }}>Loading the pipeline…</div></Panel>;
  } else if (!d) {
    body = <Panel>{header}<SectionError label="Pipeline" error={poll.error ?? undefined} onRetry={poll.refresh} /></Panel>;
  } else if (d.row_count === 0 || !d.latest_day) {
    body = (
      <Panel>
        {header}
        {poll.error && <SectionError label="Refresh" error={poll.error} onRetry={poll.refresh} />}
        <EmptyState icon={faDatabase} title="No pipeline rollup has been posted yet" hint="The nightly pipeline job has not sent its first daily count for the SES lane." />
        <FreshnessLine asOf={d.as_of} source={d.source} />
      </Panel>
    );
  } else {
    body = <Summary d={d} f={f} header={header} error={poll.error} lowISPs={lowISPs} families={families}
      domains={domains} cohorts={cohorts} nights={nights} openSource={openSource} setOpenSource={setOpenSource} />;
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <AgentStrip />
      {body}
    </div>
  );
};

const Summary: React.FC<{
  d: PipelineSummary;
  f: FieldFlags;
  header: React.ReactNode;
  error: string | null;
  lowISPs: PipelineISP[];
  families: PipelineISP[];
  domains: PipelineSummary['domains'];
  cohorts: PipelineSummary['activation'];
  nights: PipelineSummary['cleaning'];
  openSource: string | null;
  setOpenSource: (s: string | null) => void;
}> = ({ d, f, header, error, lowISPs, families, domains, cohorts, nights, openSource, setOpenSource }) => {
  const latest = d.latest_day ?? '';
  const stale = latest !== d.denver_day;
  const t = d.totals;
  return (
    <>
      <Panel>
        {header}
        {error && <div style={{ fontSize: 12, color: colors.warningText, marginBottom: 8 }}>Showing the last good read — refresh failed: {error}</div>}
        <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
          {stale && <NoteBanner label="Not today" note={`The newest posted day is ${latest}; today in Denver is ${d.denver_day}. Every number below is for ${latest}.`} />}
          {d.degraded.map((line) => <NoteBanner key={line} label="Incomplete" note={line} />)}
        </div>

        <div style={{ marginTop: 10 }}>
          {lowISPs.length > 0 ? (
            <Panel accent={colors.danger} style={{ padding: '10px 14px' }}>
              {lowISPs.map((x) => (
                <div key={x.isp} style={{ fontSize: 13, fontWeight: 700, color: colors.dangerText }}>
                  Ask a data partner for {ispLabel(x.isp)} records — {x.days_of_supply != null ? fmtDays(x.days_of_supply) : 'unknown'} of clean supply left
                </div>
              ))}
              <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 2 }}>
                Low means the clean reservoir lasts fewer than {d.low_days_threshold} days at the current contract.
              </div>
            </Panel>
          ) : (
            <Panel accent={colors.success} style={{ padding: '10px 14px' }}>
              <div style={{ fontSize: 13, fontWeight: 600 }}>No mailbox provider is below {d.low_days_threshold} days of clean supply.</div>
            </Panel>
          )}
        </div>

        {/* funnel strip */}
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(170px, 1fr))', gap: 10, marginTop: 12 }}>
          {d.funnel.map((step, i) => {
            const prior = i > 0 ? d.funnel[i - 1] : null;
            return (
              <Panel key={step.stage} style={{ padding: '10px 12px' }}>
                <Stat
                  label={FUNNEL_LABEL[step.stage] ?? step.stage}
                  title={FUNNEL_HINT[step.stage]}
                  value={<N fields={f} name={step.stage} value={step.today} />}
                  sub={
                    <>
                      <div>7-day average: {step.avg_7d != null ? fmtInt(step.avg_7d) : <i>{NOT_REPORTED}</i>}</div>
                      {prior && (
                        <div title={`Compared with ${FUNNEL_LABEL[prior.stage]} on the same day`}>
                          Drop from {FUNNEL_LABEL[prior.stage]?.toLowerCase()}:{' '}
                          {step.drop_from_prior != null ? fmtInt(step.drop_from_prior) : <i>unknown</i>}
                          {step.pct_of_prior != null ? ` (${fmtPct(step.pct_of_prior)} carried through)` : ''}
                        </div>
                      )}
                    </>
                  }
                />
              </Panel>
            );
          })}
        </div>
        <div style={{ fontSize: 11, color: colors.textFaint, marginTop: 6 }}>
          Lane totals add Microsoft, Apple, Comcast, Yahoo family and Long tail. Indented rows (Yahoo, AOL, AT&T, SBCGlobal, Cox, Verizon, Frontier, Windstream) are already inside their family and are never added again.
        </div>
        <FreshnessLine asOf={d.as_of} source={d.source}
          extra={<>latest day posted {latest} · today in Denver {d.denver_day} · {d.row_count.toLocaleString()} rows over {d.days} days</>} />
      </Panel>

      {/* (2) supply at rest */}
      <Panel>
        <SectionHeader title="Supply at rest" icon={faWarehouse} />
        <div style={subtle}>For {latest}. Counts unique records. Days of supply = clean ready to mail ÷ used per day (contract).</div>
        <div style={scroll}>
          <table style={tableStyle}>
            <thead>
              <tr>
                <th style={thStyle}>Mailbox provider</th>
                <th style={numTh} title="Records we hold that are not loaded yet (storage, SFTP, local files, subscriber base)">Dirty outside the queue</th>
                <th style={numTh} title="Loaded, waiting to be cleaned">Dirty in the queue</th>
                <th style={numTh}>Clean ready to mail</th>
                <th style={numTh}>Arriving today</th>
                <th style={numTh}>Arriving per day (7-day average)</th>
                <th style={numTh}>Used per day (contract)</th>
                <th style={numTh}>Days of supply</th>
                <th style={thStyle}>Action</th>
              </tr>
            </thead>
            <tbody>
              {families.map((x) => (
                <tr key={x.isp}>
                  <ISPCell isp={x.isp} parent={x.parent} />
                  <td style={numTd}><N fields={f} name="dirty_outside_queue" value={x.dirty_outside_queue} /></td>
                  <td style={numTd}><N fields={f} name="raw" value={x.raw} /></td>
                  <td style={numTd}><N fields={f} name="clean" value={x.clean} /></td>
                  <td style={numTd}><N fields={f} name="arrivals" value={x.arrivals} /></td>
                  <td style={numTd}><N fields={f} name="arrivals" value={x.avg_7d.arrivals ?? null} /></td>
                  <td style={numTd}><N fields={f} name="contract" value={x.contract} /></td>
                  <td style={numTd} title="Clean ready to mail ÷ used per day">
                    <N fields={f} name="days_of_supply" value={x.days_of_supply} format={fmtDays} color={x.low ? colors.dangerText : undefined} />
                  </td>
                  <td style={tdStyle}>
                    {x.low ? <Pill color={colors.danger}>Ask a data partner for {ispLabel(x.isp)} records</Pill>
                      : x.contract === 0 ? <span style={{ color: colors.textFaint, fontStyle: 'italic' }} title="Contract is 0, so days of supply does not apply">No contract</span> : null}
                  </td>
                </tr>
              ))}
              <tr style={{ fontWeight: 700 }}>
                <td style={tdStyle}>SES lane total</td>
                <td style={numTd}><N fields={f} name="dirty_outside_queue" value={t.dirty_outside_queue} /></td>
                <td style={numTd}><N fields={f} name="raw" value={t.raw} /></td>
                <td style={numTd}><N fields={f} name="clean" value={t.clean} /></td>
                <td style={numTd}><N fields={f} name="arrivals" value={t.arrivals} /></td>
                <td style={numTd}><N fields={f} name="arrivals" value={d.totals_avg_7d.arrivals ?? null} /></td>
                <td style={numTd}><N fields={f} name="contract" value={t.contract} /></td>
                <td style={numTd}><N fields={f} name="days_of_supply" value={t.days_of_supply} format={fmtDays} /></td>
                <td style={tdStyle} />
              </tr>
            </tbody>
          </table>
        </div>

        <div style={{ fontSize: 12, fontWeight: 700, margin: '14px 0 6px' }}>Where the supply is, by source</div>
        {d.sources.length === 0 ? (
          <EmptyState title="No per-source rows posted for this day" />
        ) : (
          <div style={scroll}>
            <table style={tableStyle}>
              <thead>
                <tr>
                  <th style={thStyle}>Source</th>
                  <th style={numTh}>Dirty outside the queue</th>
                  <th style={numTh}>Dirty in the queue</th>
                  <th style={numTh}>Clean ready to mail</th>
                </tr>
              </thead>
              <tbody>
                {d.sources.map((s) => (
                  <React.Fragment key={s.source}>
                    <tr style={{ cursor: 'pointer' }} onClick={() => setOpenSource(openSource === s.source ? null : s.source)}>
                      <td style={{ ...tdStyle, fontWeight: 600 }}>{openSource === s.source ? '▾' : '▸'} {sourceLabel(s.source)}</td>
                      <td style={numTd}><N fields={f} name="dirty_outside_queue" value={s.dirty_outside_queue} /></td>
                      <td style={numTd}><N fields={f} name="raw" value={s.raw} /></td>
                      <td style={numTd}><N fields={f} name="clean" value={s.clean} /></td>
                    </tr>
                    {openSource === s.source && s.by_isp.map((b) => (
                      <tr key={b.isp}>
                        <td style={{ ...tdStyle, paddingLeft: 30, color: colors.textMuted }}>{ispLabel(b.isp)}</td>
                        <td style={numTd}><N fields={f} name="dirty_outside_queue" value={b.dirty_outside_queue} /></td>
                        <td style={numTd}><N fields={f} name="raw" value={b.raw} /></td>
                        <td style={numTd}><N fields={f} name="clean" value={b.clean} /></td>
                      </tr>
                    ))}
                  </React.Fragment>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>

      {/* (3) introduction in progress */}
      <Panel>
        <SectionHeader title="Introduction in progress" icon={faShoePrints} />
        <div style={subtle}>Records mailed on {latest} at each of the five introduction steps. Promoted = opened or clicked and moved to the engaged mailing. Retired = no response after step 5.</div>
        <div style={scroll}>
          <table style={tableStyle}>
            <thead>
              <tr>
                <th style={thStyle}>Mailbox provider</th>
                <th style={numTh}>Step 1</th>
                <th style={numTh}>Step 2</th>
                <th style={numTh}>Step 3</th>
                <th style={numTh}>Step 4</th>
                <th style={numTh}>Step 5</th>
                <th style={numTh}>Promoted</th>
                <th style={numTh}>Retired</th>
              </tr>
            </thead>
            <tbody>
              {[...families, { ...t, isp: 'total' } as unknown as PipelineISP].map((x) => (
                <tr key={x.isp} style={x.isp === ('total' as string) ? { fontWeight: 700 } : undefined}>
                  {x.isp === ('total' as string) ? <td style={tdStyle}>SES lane total</td> : <ISPCell isp={x.isp} parent={x.parent} />}
                  <td style={numTd}><N fields={f} name="touch_1" value={x.touch_1} /></td>
                  <td style={numTd}><N fields={f} name="touch_2" value={x.touch_2} /></td>
                  <td style={numTd}><N fields={f} name="touch_3" value={x.touch_3} /></td>
                  <td style={numTd}><N fields={f} name="touch_4" value={x.touch_4} /></td>
                  <td style={numTd}><N fields={f} name="touch_5" value={x.touch_5} /></td>
                  <td style={numTd}><N fields={f} name="promoted" value={x.promoted} /></td>
                  <td style={numTd}><N fields={f} name="retired" value={x.retired} /></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>

        <div style={{ fontSize: 12, fontWeight: 700, margin: '14px 0 6px' }}>New people introduced vs the 20% target</div>
        <div style={subtle}>Daily target = 20% of the unique people the SES lane mailed (7 days), per mailbox provider.</div>
        <div style={scroll}>
          <table style={tableStyle}>
            <thead>
              <tr>
                <th style={thStyle}>Mailbox provider</th>
                <th style={numTh}>Introduced today</th>
                <th style={numTh}>Daily target</th>
                <th style={numTh}>Percent of target</th>
                <th style={{ ...thStyle, width: 160 }}>Progress</th>
                <th style={numTh} title="Introduced today ÷ unique people mailed">New people as share of mailed</th>
              </tr>
            </thead>
            <tbody>
              {families.map((x) => (
                <tr key={x.isp}>
                  <ISPCell isp={x.isp} parent={x.parent} />
                  <td style={numTd}><N fields={f} name="first_mailed" value={x.first_mailed} /></td>
                  <td style={numTd}><N fields={f} name="target" value={x.target} /></td>
                  <td style={numTd}><N fields={f} name="pct_of_target" value={x.pct_of_target} format={fmtPct} /></td>
                  <td style={tdStyle}>{x.pct_of_target != null ? <ProgressBar pct={Math.min(100, x.pct_of_target)} /> : null}</td>
                  <td style={numTd} title={`denominator: unique people mailed = ${x.unique_mailed != null ? fmtInt(x.unique_mailed) : 'unknown'}`}>
                    <N fields={f} name="new_share" value={x.new_share} format={fmtPct} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Panel>

      {/* (4) activation */}
      <Panel>
        <SectionHeader title="Activation by family" icon={faBolt} />
        <div style={subtle}>
          A cohort is the people introduced on one day. Activated = opened or clicked since. Activation rate = activated ÷ mailed.
          The newest cohorts have had less time to respond, so their rate is still rising for about three days.
        </div>
        <div style={scroll}>
          <table style={tableStyle}>
            <thead>
              <tr>
                <th style={thStyle}>Mailbox provider</th>
                <th style={numTh} title="All activated ÷ all mailed over the cohorts of the last 7 days">Activation rate, last 7 days</th>
              </tr>
            </thead>
            <tbody>
              {families.map((x) => (
                <tr key={x.isp}>
                  <ISPCell isp={x.isp} parent={x.parent} />
                  <td style={numTd}><N fields={f} name="activation_rate_7d" value={x.activation_rate_7d} format={fmtPct} /></td>
                </tr>
              ))}
              <tr style={{ fontWeight: 700 }}>
                <td style={tdStyle}>SES lane total</td>
                <td style={numTd}><N fields={f} name="activation_rate_7d" value={t.activation_rate_7d} format={fmtPct} /></td>
              </tr>
            </tbody>
          </table>
        </div>
        <div style={{ fontSize: 12, fontWeight: 700, margin: '14px 0 6px' }}>By cohort day</div>
        {cohorts.length === 0 ? (
          <EmptyState title="No cohort counts posted yet" />
        ) : (
          <div style={{ ...scroll, maxHeight: 360, overflowY: 'auto' }}>
            <table style={tableStyle}>
              <thead>
                <tr>
                  <th style={thStyle}>Cohort day</th>
                  <th style={thStyle}>Mailbox provider</th>
                  <th style={numTh}>Mailed</th>
                  <th style={numTh}>Activated</th>
                  <th style={numTh}>Activation rate</th>
                </tr>
              </thead>
              <tbody>
                {cohorts.map((c) => (
                  <tr key={`${c.day}-${c.isp}`}>
                    <td style={tdStyle}>{c.day}</td>
                    <td style={tdStyle}>{ispLabel(c.isp)}</td>
                    <td style={numTd}><N fields={f} name="cohort_mailed" value={c.mailed} /></td>
                    <td style={numTd}><N fields={f} name="cohort_activated" value={c.activated} /></td>
                    <td style={numTd}><N fields={f} name="cohort_activated" value={c.rate} format={fmtPct} /></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>

      {/* (5) contracts */}
      <Panel>
        <SectionHeader title="Contracts by sending domain" icon={faFileContract} />
        <div style={subtle}>
          For {latest}. Over contract = introduced more than 5% above the contract. Above the ramp = the contract is higher than today's ramp level.
          Domain rows add their mailbox providers; indented sub-rows are not added again.
        </div>
        {domains.length === 0 ? (
          <EmptyState title="No sending-domain rows posted for this day" />
        ) : (
          <div style={scroll}>
            <table style={tableStyle}>
              <thead>
                <tr>
                  <th style={thStyle}>Sending domain / mailbox provider</th>
                  <th style={numTh}>Today's contract</th>
                  <th style={numTh}>Ramp level today</th>
                  <th style={numTh}>Introduced today</th>
                  <th style={numTh}>Percent of contract</th>
                  <th style={thStyle}>Flag</th>
                </tr>
              </thead>
              <tbody>
                {domains.map((dom) => (
                  <React.Fragment key={dom.sending_domain}>
                    <tr>
                      <td style={{ ...tdStyle, fontWeight: 700 }}>{dom.sending_domain}</td>
                      <td style={numTd}><N fields={f} name="contract" value={dom.contract} /></td>
                      <td style={numTd}><N fields={f} name="ramp_target" value={dom.ramp_target} /></td>
                      <td style={numTd}><N fields={f} name="first_mailed" value={dom.first_mailed} /></td>
                      <td style={numTd}><N fields={f} name="contract" value={dom.pct_of_contract} format={fmtPct} /></td>
                      <td style={tdStyle}><ContractFlags over={dom.over_contract} off={dom.off_ramp} /></td>
                    </tr>
                    {dom.by_isp.map((b) => (
                      <tr key={b.isp}>
                        <td style={{ ...tdStyle, paddingLeft: parentOfISP(families, b.isp) ? 44 : 26, color: colors.textMuted }}>{ispLabel(b.isp)}</td>
                        <td style={numTd}><N fields={f} name="contract" value={b.contract} /></td>
                        <td style={numTd}><N fields={f} name="ramp_target" value={b.ramp_target} /></td>
                        <td style={numTd}><N fields={f} name="first_mailed" value={b.first_mailed} /></td>
                        <td style={numTd}><N fields={f} name="contract" value={b.pct_of_contract} format={fmtPct} /></td>
                        <td style={tdStyle}><ContractFlags over={b.over_contract} off={b.off_ramp} /></td>
                      </tr>
                    ))}
                  </React.Fragment>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>

      {/* (6) throttling */}
      <Panel>
        <SectionHeader title="Throttling" icon={faGaugeHigh} />
        <div style={subtle}>Pipeline cells only, {latest}. Deferred = the mailbox provider asked us to slow down and retry later.</div>
        <div style={scroll}>
          <table style={tableStyle}>
            <thead>
              <tr>
                <th style={thStyle}>Mailbox provider</th>
                <th style={numTh}>Sent</th>
                <th style={numTh}>Delivered</th>
                <th style={numTh}>Deferred (retrying)</th>
                <th style={numTh} title="Delivered ÷ sent">Delivered percent</th>
                <th style={numTh} title="Deferred ÷ sent">Deferred percent</th>
              </tr>
            </thead>
            <tbody>
              {[...families, { ...t, isp: 'total' } as unknown as PipelineISP].map((x) => (
                <tr key={x.isp} style={x.isp === ('total' as string) ? { fontWeight: 700 } : undefined}>
                  {x.isp === ('total' as string) ? <td style={tdStyle}>SES lane total</td> : <ISPCell isp={x.isp} parent={x.parent} />}
                  <td style={numTd}><N fields={f} name="sent" value={x.sent} /></td>
                  <td style={numTd}><N fields={f} name="delivered" value={x.delivered} /></td>
                  <td style={numTd}><N fields={f} name="deferred" value={x.deferred} /></td>
                  <td style={numTd}><N fields={f} name="delivered_rate" value={x.delivered_rate} format={fmtPct} /></td>
                  <td style={numTd}><N fields={f} name="deferred_rate" value={x.deferred_rate} format={fmtPct} /></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Panel>

      {/* (7) Email Oversight */}
      <Panel>
        <SectionHeader title="Email Oversight cleaning" icon={faBroom} />
        <div style={subtle}>Records sent to Email Oversight each night, how many passed, and what it cost. Month to date runs from {d.cleaning_mtd.from || 'the 1st'}.</div>
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 24, marginBottom: 10 }}>
          <Stat label={`Ordered, ${d.cleaning_mtd.month || 'this month'}`} value={<N fields={f} name="cleaning_ordered" value={d.cleaning_mtd.ordered} />} sub="records sent for cleaning" />
          <Stat label="Passed, month to date" value={<N fields={f} name="cleaning_passed" value={d.cleaning_mtd.passed} />} sub="records clean to mail" />
          <Stat label="Cost, month to date" value={<N fields={f} name="cleaning_cost_cents" value={d.cleaning_mtd.cost_cents} format={fmtMoney} />} />
        </div>
        {d.cleaning_mtd.by_isp.length > 0 && (
          <div style={{ ...scroll, marginBottom: 12 }}>
            <table style={tableStyle}>
              <thead>
                <tr>
                  <th style={thStyle}>Mailbox provider (month to date)</th>
                  <th style={numTh}>Ordered</th>
                  <th style={numTh}>Passed</th>
                  <th style={numTh}>Cost</th>
                </tr>
              </thead>
              <tbody>
                {d.cleaning_mtd.by_isp.map((m) => (
                  <tr key={m.isp}>
                    <ISPCell isp={m.isp} parent={parentOfISP(families, m.isp)} />
                    <td style={numTd}><N fields={f} name="cleaning_ordered" value={m.ordered} /></td>
                    <td style={numTd}><N fields={f} name="cleaning_passed" value={m.passed} /></td>
                    <td style={numTd}><N fields={f} name="cleaning_cost_cents" value={m.cost_cents} format={fmtMoney} /></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {nights.length === 0 ? (
          <EmptyState title="No cleaning orders posted in the last 14 days" />
        ) : (
          <div style={{ ...scroll, maxHeight: 360, overflowY: 'auto' }}>
            <table style={tableStyle}>
              <thead>
                <tr>
                  <th style={thStyle}>Night</th>
                  <th style={thStyle}>Mailbox provider</th>
                  <th style={numTh}>Ordered</th>
                  <th style={numTh}>Passed</th>
                  <th style={numTh} title="Passed ÷ ordered">Pass rate</th>
                  <th style={numTh}>Cost</th>
                </tr>
              </thead>
              <tbody>
                {nights.map((n) => (
                  <tr key={`${n.day}-${n.isp}`}>
                    <td style={tdStyle}>{n.day}</td>
                    <ISPCell isp={n.isp} parent={parentOfISP(families, n.isp)} />
                    <td style={numTd}><N fields={f} name="cleaning_ordered" value={n.ordered} /></td>
                    <td style={numTd}><N fields={f} name="cleaning_passed" value={n.passed} /></td>
                    <td style={numTd}><N fields={f} name="cleaning_pass_rate" value={n.pass_rate} format={fmtPct} /></td>
                    <td style={numTd}><N fields={f} name="cleaning_cost_cents" value={n.cost_cents} format={fmtMoney} /></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>
    </>
  );
};

const parentOfISP = (families: PipelineISP[], isp: string): string | undefined =>
  families.find((x) => x.isp === isp)?.parent || undefined;

const ContractFlags: React.FC<{ over: boolean; off: boolean }> = ({ over, off }) => (
  <span style={{ display: 'inline-flex', gap: 6 }}>
    {over && <Pill color={colors.danger}>Over contract</Pill>}
    {off && <Pill color={colors.warning}>Above the ramp</Pill>}
  </span>
);

export default PipelinePanel;
