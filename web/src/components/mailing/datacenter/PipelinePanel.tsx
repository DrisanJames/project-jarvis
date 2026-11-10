// PipelinePanel — Data Center → Pipeline. The operator's view of the New Data
// Pipeline on the SES lane (tasks/eng-team/requirements/2026-11-10-new-data-pipeline.md):
// per mailbox provider, how much raw data waits to be cleaned, how much clean
// data is ready, how many days it lasts at the contract, how many never-mailed
// people went out today against the 20% target, how many became engagers, and
// which provider is running low.
//
// Every number comes from GET /api/mailing/data-pipeline/summary
// (data_pipeline_daily_handlers.go). Verdicts (days of supply, percent of
// target, "low") are computed server-side and displayed, never recomputed
// (PORTAL_DESIGN_SYSTEM §6.4). A missing row renders "not yet measured".

import React, { useMemo, useState } from 'react';
import { faFilter, faUsers, faTable, faGlobe, faDatabase } from '@fortawesome/free-solid-svg-icons';
import { usePolling } from '../shared/usePolling';
import { Panel, SectionHeader, SectionError, EmptyState, Pill, ProgressBar, Stat, LivePill } from '../shared/ui';
import { colors, tableStyle, thStyle, tdStyle, numTd, numTh, btnStyle } from '../shared/theme';
import { Measured } from '../dataingest/Measured';
import { NoteBanner, FreshnessLine } from '../dataingest/Envelope';
import type { Count, FieldFlags } from '../dataingest/api';
import {
  fetchPipelineSummary, ispLabel, sourceLabel,
  type PipelineSummary, type PipelineStage, type PipelineISP,
} from './pipelineApi';

const LANE = 'ses';
const DAYS = 14;
const POLL_MS = 60_000;

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

const fmtInt = (n: number) => Math.round(n).toLocaleString();
const fmtDays = (n: number) => `${n.toFixed(1)} days`;
const fmtPct = (n: number) => `${n.toFixed(1)}%`;

const scroll: React.CSSProperties = { overflowX: 'auto' };

/** A plain number cell that renders null as "not yet measured". */
const N: React.FC<{ fields: FieldFlags; name: string; value: Count | undefined; format?: (n: number) => string; color?: string }> = (p) => (
  <Measured fields={p.fields} name={p.name} value={p.value} format={p.format ?? fmtInt} color={p.color} />
);

const LowBadge: React.FC = () => <Pill color={colors.danger}>Low — get data</Pill>;

export const PipelinePanel: React.FC = () => {
  const poll = usePolling<PipelineSummary>((signal) => fetchPipelineSummary(LANE, DAYS, signal), POLL_MS, []);
  const d = poll.data;
  const f: FieldFlags = d?.fields ?? {};
  const [openDomain, setOpenDomain] = useState<string | null>(null);
  const [openSource, setOpenSource] = useState<string | null>(null);

  const lowISPs = useMemo(() => (d?.isps ?? []).filter((x) => x.low), [d]);
  const domains = useMemo(
    () => [...(d?.domains ?? [])].sort((a, b) => (b.contract ?? -1) - (a.contract ?? -1)),
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

  if (poll.loading && !d) {
    return (
      <Panel>
        {header}
        <div style={{ fontSize: 12, color: colors.textMuted, padding: '12px 0' }}>Loading the pipeline…</div>
      </Panel>
    );
  }
  if (!d) {
    return (
      <Panel>
        {header}
        <SectionError label="Pipeline" error={poll.error ?? undefined} onRetry={poll.refresh} />
      </Panel>
    );
  }

  const freshness = (
    <FreshnessLine
      asOf={d.as_of}
      source={d.source}
      extra={<>latest day posted {d.latest_day ?? 'none'} · today in Denver {d.denver_day} · {d.row_count.toLocaleString()} rows over {d.days} days</>}
    />
  );

  if (d.row_count === 0 || !d.latest_day) {
    return (
      <Panel>
        {header}
        {poll.error && <SectionError label="Refresh" error={poll.error} onRetry={poll.refresh} />}
        <EmptyState icon={faDatabase} title="No pipeline rollup has been posted yet" hint="The nightly pipeline job has not sent its first daily count for the SES lane." />
        {freshness}
      </Panel>
    );
  }

  const stale = d.latest_day !== d.denver_day;

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <Panel>
        {header}
        {poll.error && (
          <div style={{ fontSize: 12, color: colors.warningText, marginBottom: 8 }}>
            Showing the last good read — refresh failed: {poll.error}
          </div>
        )}
        <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
          {stale && <NoteBanner label="Not today" note={`The newest posted day is ${d.latest_day}; today in Denver is ${d.denver_day}. Every number below is for ${d.latest_day}.`} />}
          {d.degraded.map((line) => <NoteBanner key={line} label="Incomplete" note={line} />)}
        </div>

        {/* which provider is running low — the first thing to read */}
        <div style={{ marginTop: 10 }}>
          {lowISPs.length > 0 ? (
            <Panel accent={colors.danger} style={{ padding: '10px 14px' }}>
              <div style={{ fontSize: 13, fontWeight: 700, color: colors.dangerText }}>
                Running low — get data for: {lowISPs.map((x) => `${ispLabel(x.isp)} (${x.days_of_supply != null ? fmtDays(x.days_of_supply) : 'unknown'} left)`).join(' · ')}
              </div>
              <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 2 }}>
                Low means the clean reservoir lasts fewer than {d.low_days_threshold} days at the current contract.
              </div>
            </Panel>
          ) : (
            <Panel accent={colors.success} style={{ padding: '10px 14px' }}>
              <div style={{ fontSize: 13, fontWeight: 600 }}>
                No mailbox provider is below {d.low_days_threshold} days of clean supply.
              </div>
            </Panel>
          )}
        </div>

        {/* (a) funnel strip */}
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
                      <div>7-day average: {step.avg_7d != null ? fmtInt(step.avg_7d) : <i>not yet measured</i>}</div>
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
          Lane totals add Microsoft, Apple, Comcast, Yahoo family and Long tail. Windstream is already inside Long tail and is not added again.
        </div>
        {freshness}
      </Panel>

      {/* (b) new people vs the 20% target */}
      <Panel>
        <SectionHeader title="New people introduced vs target" icon={faUsers} />
        <div style={{ fontSize: 11, color: colors.textMuted, marginBottom: 8 }}>
          Target = 20% of the unique people the SES lane mailed (7 days), per mailbox provider. Counts unique people.
        </div>
        <div style={scroll}>
          <table style={tableStyle}>
            <thead>
              <tr>
                <th style={thStyle}>Mailbox provider</th>
                <th style={numTh}>Introduced today</th>
                <th style={numTh}>Daily target</th>
                <th style={numTh}>Percent of target</th>
                <th style={{ ...thStyle, width: 180 }}>Progress</th>
                <th style={numTh} title="Introduced today ÷ unique people mailed">New people as share of mailed</th>
              </tr>
            </thead>
            <tbody>
              {d.isps.map((x) => (
                <tr key={x.isp}>
                  <td style={{ ...tdStyle, paddingLeft: x.parent ? 26 : 10 }}>{ispLabel(x.isp)}</td>
                  <td style={numTd}><N fields={f} name="first_mailed" value={x.first_mailed} /></td>
                  <td style={numTd}><N fields={f} name="target" value={x.target} /></td>
                  <td style={numTd} title="Introduced today ÷ daily target"><N fields={f} name="pct_of_target" value={x.pct_of_target} format={fmtPct} /></td>
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

      {/* (c) per-ISP table */}
      <Panel>
        <SectionHeader title="Supply by mailbox provider" icon={faTable} />
        <div style={{ fontSize: 11, color: colors.textMuted, marginBottom: 8 }}>
          For {d.latest_day}. Counts unique records. Days of supply = ready to send ÷ contract.
        </div>
        <div style={scroll}>
          <table style={tableStyle}>
            <thead>
              <tr>
                <th style={thStyle}>Mailbox provider</th>
                <th style={numTh}>Waiting to be cleaned</th>
                <th style={numTh}>Ready to send</th>
                <th style={numTh}>Days of supply</th>
                <th style={numTh}>Daily target (20% of unique people mailed)</th>
                <th style={numTh}>Contract</th>
                <th style={numTh}>Introduced today</th>
                <th style={numTh}>Percent of target</th>
                <th style={numTh}>Promoted</th>
                <th style={numTh}>Retired</th>
                <th style={thStyle}>Status</th>
              </tr>
            </thead>
            <tbody>
              {d.isps.map((x) => <ISPRow key={x.isp} x={x} f={f} />)}
              <tr style={{ fontWeight: 700 }}>
                <td style={tdStyle}>SES lane total</td>
                <td style={numTd}><N fields={f} name="raw" value={d.totals.raw} /></td>
                <td style={numTd}><N fields={f} name="clean" value={d.totals.clean} /></td>
                <td style={numTd}><N fields={f} name="days_of_supply" value={d.totals.days_of_supply} format={fmtDays} /></td>
                <td style={numTd}><N fields={f} name="target" value={d.totals.target} /></td>
                <td style={numTd}><N fields={f} name="contract" value={d.totals.contract} /></td>
                <td style={numTd}><N fields={f} name="first_mailed" value={d.totals.first_mailed} /></td>
                <td style={numTd}><N fields={f} name="pct_of_target" value={d.totals.pct_of_target} format={fmtPct} /></td>
                <td style={numTd}><N fields={f} name="promoted" value={d.totals.promoted} /></td>
                <td style={numTd}><N fields={f} name="retired" value={d.totals.retired} /></td>
                <td style={tdStyle} />
              </tr>
            </tbody>
          </table>
        </div>
      </Panel>

      {/* (d) per sending domain */}
      <Panel>
        <SectionHeader title="By sending domain" icon={faGlobe} />
        <div style={{ fontSize: 11, color: colors.textMuted, marginBottom: 8 }}>
          For {d.latest_day}. Click a domain to see its mailbox providers. Domain totals do not add Windstream again.
        </div>
        {domains.length === 0 ? (
          <EmptyState title="No sending-domain rows posted for this day" />
        ) : (
          <div style={scroll}>
            <table style={tableStyle}>
              <thead>
                <tr>
                  <th style={thStyle}>Sending domain</th>
                  <th style={numTh}>Contract</th>
                  <th style={numTh}>Daily target</th>
                  <th style={numTh}>Introduced today</th>
                  <th style={numTh}>Percent of contract</th>
                </tr>
              </thead>
              <tbody>
                {domains.map((dom) => (
                  <React.Fragment key={dom.sending_domain}>
                    <tr style={{ cursor: 'pointer' }} onClick={() => setOpenDomain(openDomain === dom.sending_domain ? null : dom.sending_domain)}>
                      <td style={{ ...tdStyle, fontWeight: 600 }}>{openDomain === dom.sending_domain ? '▾' : '▸'} {dom.sending_domain}</td>
                      <td style={numTd}><N fields={f} name="contract" value={dom.contract} /></td>
                      <td style={numTd}><N fields={f} name="target" value={dom.target} /></td>
                      <td style={numTd}><N fields={f} name="first_mailed" value={dom.first_mailed} /></td>
                      <td style={numTd} title="Introduced today ÷ contract"><N fields={f} name="pct_of_target" value={dom.pct_of_contract} format={fmtPct} /></td>
                    </tr>
                    {openDomain === dom.sending_domain && dom.by_isp.map((b) => (
                      <tr key={b.isp}>
                        <td style={{ ...tdStyle, paddingLeft: 30, color: colors.textMuted }}>{ispLabel(b.isp)}</td>
                        <td style={numTd}><N fields={f} name="contract" value={b.contract} /></td>
                        <td style={numTd}><N fields={f} name="target" value={b.target} /></td>
                        <td style={numTd}><N fields={f} name="first_mailed" value={b.first_mailed} /></td>
                        <td style={numTd}><N fields={f} name="pct_of_target" value={b.pct_of_contract} format={fmtPct} /></td>
                      </tr>
                    ))}
                  </React.Fragment>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>

      {/* (e) reservoir by source */}
      <Panel>
        <SectionHeader title="Reservoir by source" icon={faDatabase} />
        <div style={{ fontSize: 11, color: colors.textMuted, marginBottom: 8 }}>
          For {d.latest_day}. Click a source to see its mailbox providers.
        </div>
        {d.sources.length === 0 ? (
          <EmptyState title="No per-source rows posted for this day" />
        ) : (
          <div style={scroll}>
            <table style={tableStyle}>
              <thead>
                <tr>
                  <th style={thStyle}>Source</th>
                  <th style={numTh}>Waiting to be cleaned</th>
                  <th style={numTh}>Ready to send</th>
                </tr>
              </thead>
              <tbody>
                {d.sources.map((s) => (
                  <React.Fragment key={s.source}>
                    <tr style={{ cursor: 'pointer' }} onClick={() => setOpenSource(openSource === s.source ? null : s.source)}>
                      <td style={{ ...tdStyle, fontWeight: 600 }}>{openSource === s.source ? '▾' : '▸'} {sourceLabel(s.source)}</td>
                      <td style={numTd}><N fields={f} name="raw" value={s.raw} /></td>
                      <td style={numTd}><N fields={f} name="clean" value={s.clean} /></td>
                    </tr>
                    {openSource === s.source && s.by_isp.map((b) => (
                      <tr key={b.isp}>
                        <td style={{ ...tdStyle, paddingLeft: 30, color: colors.textMuted }}>{ispLabel(b.isp)}</td>
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
    </div>
  );
};

const ISPRow: React.FC<{ x: PipelineISP; f: FieldFlags }> = ({ x, f }) => (
  <tr>
    <td style={{ ...tdStyle, paddingLeft: x.parent ? 26 : 10 }}>{ispLabel(x.isp)}</td>
    <td style={numTd}><N fields={f} name="raw" value={x.raw} /></td>
    <td style={numTd}><N fields={f} name="clean" value={x.clean} /></td>
    <td style={numTd} title="Ready to send ÷ contract">
      <N fields={f} name="days_of_supply" value={x.days_of_supply} format={fmtDays} color={x.low ? colors.dangerText : undefined} />
    </td>
    <td style={numTd}><N fields={f} name="target" value={x.target} /></td>
    <td style={numTd}><N fields={f} name="contract" value={x.contract} /></td>
    <td style={numTd}><N fields={f} name="first_mailed" value={x.first_mailed} /></td>
    <td style={numTd}><N fields={f} name="pct_of_target" value={x.pct_of_target} format={fmtPct} /></td>
    <td style={numTd}><N fields={f} name="promoted" value={x.promoted} /></td>
    <td style={numTd}><N fields={f} name="retired" value={x.retired} /></td>
    <td style={tdStyle}>{x.low ? <LowBadge /> : x.contract === 0 ? <span title="Contract is 0, so days of supply does not apply" style={{ color: colors.textFaint, fontStyle: 'italic' }}>No contract</span> : null}</td>
  </tr>
);

export default PipelinePanel;
