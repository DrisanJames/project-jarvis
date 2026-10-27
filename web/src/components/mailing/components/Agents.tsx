// Agents.tsx — one screen for the platform's autonomous agents (operator
// 2026-10-27: "how this system is performing from an agentic standpoint …
// consider the other sessions' agentic work as well").
//
//   Warmup     the AgentCore LLM that warms a new box: rates, caps, lessons, report  (WarmupAgent.tsx)
//   Scheduling the 21:00 MT plan (HOLD/UP, offers, CPM pace) + the 15-min supervisor (AgentsOps.tsx)
//   Data       the 30-min data agent: 12 checks, repairs, pages, verdict flips     (AgentsOps.tsx)
//
// The screen displays each agent's own record (S3 journals, state, ledger); it
// computes nothing the agents did not already write. Date = Denver day.

import React, { useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faRobot } from '@fortawesome/free-solid-svg-icons';
import { colors, pageStyle, btnStyle } from '../shared/theme';
import { SubNav } from '../shared/SubNav';
import WarmupAgent from './WarmupAgent';
import { SchedulingAgentView, DataAgentView } from './AgentsOps';

type AgentKey = 'warmup' | 'scheduling' | 'data';
const AGENTS: { key: AgentKey; label: string }[] = [
  { key: 'warmup', label: 'Warmup · colo2' },
  { key: 'scheduling', label: 'Scheduling' },
  { key: 'data', label: 'Data' },
];

function denverToday(): string {
  const parts = new Intl.DateTimeFormat('en-CA', { timeZone: 'America/Denver', year: 'numeric', month: '2-digit', day: '2-digit' }).formatToParts(new Date());
  const get = (t: string) => parts.find((p) => p.type === t)?.value ?? '';
  return `${get('year')}-${get('month')}-${get('day')}`;
}

const Agents: React.FC = () => {
  const [agent, setAgent] = useState<AgentKey>('warmup');
  const [date, setDate] = useState(denverToday());
  return (
    <div style={pageStyle}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', flexWrap: 'wrap', gap: 12, marginBottom: 12 }}>
        <div>
          <h1 style={{ margin: 0, color: colors.heading, fontSize: 22, display: 'flex', alignItems: 'center', gap: 10 }}>
            <FontAwesomeIcon icon={faRobot} style={{ color: colors.indigo400 }} /> Agents
          </h1>
          <div style={{ color: colors.textMuted, fontSize: 12, marginTop: 4 }}>
            What each autonomous agent decided, why, and what happened — from the agents' own journals. Denver day.
          </div>
        </div>
        <input type="date" value={date} max={denverToday()} onChange={(e) => setDate(e.target.value)}
          style={{ ...btnStyle, colorScheme: 'dark' }} aria-label="Denver date" />
      </div>
      <SubNav items={AGENTS} active={agent} onChange={(k) => setAgent(k as AgentKey)} ariaLabel="Agents" />
      <div style={{ marginTop: 14 }}>
        {agent === 'warmup' && <WarmupAgent date={date} />}
        {agent === 'scheduling' && <SchedulingAgentView date={date} />}
        {agent === 'data' && <DataAgentView date={date} />}
      </div>
    </div>
  );
};

export default Agents;
