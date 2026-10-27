// Agents.tsx — the platform's autonomous agents, by sending environment
// (operator 2026-10-27: "separate by environment as I have. Kumo, PMTA A and
// PMTA B, SES, Gmail, Colo 1, Colo 2").
//
//   Kumo · PMTA A · PMTA B · SES · Gmail   what the scheduling agent decided for that environment (lane gates, programs
//                                          built + verified, supervisor repairs, offers/CPM on the SES board) + its ledger
//   Colo 1 · Colo 2                        the warmup agent on that box (speed, targets, lessons, daily report)
//   Data                                   the data agent (cross-environment: supply, EO, ingest, growth, segments, suppression)
//
// Every view displays the agents' own records (S3 journals, state, change ledger); nothing is computed server-side.

import React, { useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faRobot } from '@fortawesome/free-solid-svg-icons';
import { colors, pageStyle, btnStyle } from '../shared/theme';
import { SubNav } from '../shared/SubNav';
import { EmptyState, Panel } from '../shared/ui';
import WarmupAgent from './WarmupAgent';
import { DataAgentView, EnvironmentView, type EnvKey } from './AgentsOps';

type Key = EnvKey | 'data';
const ENVS: { key: Key; label: string }[] = [
  { key: 'kumo', label: 'Kumo' }, { key: 'pmta_a', label: 'PMTA A' }, { key: 'pmta_b', label: 'PMTA B' }, { key: 'ses', label: 'SES' },
  { key: 'gmail', label: 'Gmail' }, { key: 'colo1', label: 'Colo 1' }, { key: 'colo2', label: 'Colo 2' }, { key: 'data', label: 'Data' },
];

function denverToday(): string {
  const parts = new Intl.DateTimeFormat('en-CA', { timeZone: 'America/Denver', year: 'numeric', month: '2-digit', day: '2-digit' }).formatToParts(new Date());
  const get = (t: string) => parts.find((p) => p.type === t)?.value ?? '';
  return `${get('year')}-${get('month')}-${get('day')}`;
}

const Agents: React.FC = () => {
  const [env, setEnv] = useState<Key>('colo2');
  const [date, setDate] = useState(denverToday());
  return (
    <div style={pageStyle}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', flexWrap: 'wrap', gap: 12, marginBottom: 12 }}>
        <div>
          <h1 style={{ margin: 0, color: colors.heading, fontSize: 22, display: 'flex', alignItems: 'center', gap: 10 }}>
            <FontAwesomeIcon icon={faRobot} style={{ color: colors.indigo400 }} /> Agents
          </h1>
          <div style={{ color: colors.textMuted, fontSize: 12, marginTop: 4 }}>
            What the agents decided for each sending environment, why, and what happened — from their own journals. Denver day.
          </div>
        </div>
        <input type="date" value={date} max={denverToday()} onChange={(e) => setDate(e.target.value)}
          style={{ ...btnStyle, colorScheme: 'dark' }} aria-label="Denver date" />
      </div>
      <SubNav items={ENVS} active={env} onChange={(k) => setEnv(k as Key)} ariaLabel="Environments" />
      <div style={{ marginTop: 14 }}>
        {env === 'colo2' && <WarmupAgent date={date} />}
        {env === 'colo1' && <Panel><EmptyState title="Colo 1 · no agent assigned yet" hint="When a warmup instance is created for this box it appears here with the same views as Colo 2." /></Panel>}
        {env === 'data' && <DataAgentView date={date} />}
        {(env === 'kumo' || env === 'pmta_a' || env === 'pmta_b' || env === 'ses' || env === 'gmail') && <EnvironmentView env={env} date={date} />}
      </div>
    </div>
  );
};

export default Agents;
