import React, { useState } from 'react';
import { useConvictionStream, Conviction } from '../../hooks/useConvictionStream';
import { useApi } from '../../hooks/useApi';
import { Card, CardHeader, CardBody } from '../common/Card';

const ISP_COLORS: Record<string, string> = {
  gmail: '#ea4335', yahoo: '#6001d2', microsoft: '#00a4ef', apple: '#a2aaad',
  comcast: '#ed1c24', att: '#009fdb', cox: '#0077c8', charter: '#0078d4',
};

const AGENT_LABELS: Record<string, string> = {
  reputation: 'Reputation', throttle: 'Throttle', pool: 'Pool',
  warmup: 'Warmup', emergency: 'Emergency', suppression: 'Suppression',
};

const ALL_ISPS = ['gmail', 'yahoo', 'microsoft', 'apple', 'comcast', 'att', 'cox', 'charter'];
const ALL_AGENTS = ['reputation', 'throttle', 'pool', 'warmup', 'emergency', 'suppression'];

interface VelocityData {
  global: { per_minute_1m: number; per_minute_5m: number; total_1m: number; total_5m: number };
  by_isp: Record<string, { will: number; wont: number; total: number }>;
  by_agent: Record<string, { will: number; wont: number; total: number }>;
}

const MemoryView: React.FC = () => {
  const [ispFilter, setIspFilter] = useState('');
  const [agentFilter, setAgentFilter] = useState('');
  const [verdictFilter, setVerdictFilter] = useState<'' | 'will' | 'wont'>('');
  const [expandedId, setExpandedId] = useState<string | null>(null);

  const { convictions, connected, stats } = useConvictionStream({
    isp: ispFilter || undefined,
    agentType: agentFilter || undefined,
  });

  const { data: velocity } = useApi<VelocityData>('/api/mailing/engine/convictions/velocity', {
    pollingInterval: 5000,
  });

  const filtered = verdictFilter
    ? convictions.filter(c => c.verdict === verdictFilter)
    : convictions;

  return (
    <div style={{ display: 'grid', gridTemplateColumns: '1fr 360px', gap: '1rem', height: 'calc(100vh - 340px)', minHeight: '400px' }}>
      {/* Left: Live Feed */}
      <div style={{ display: 'flex', flexDirection: 'column', minHeight: 0 }}>
        {/* Filter Bar */}
        <div style={{
          display: 'flex', gap: '0.5rem', alignItems: 'center', marginBottom: '0.75rem',
          padding: '0.5rem 0.75rem', background: '#111827', borderRadius: '0.5rem',
        }}>
          <div style={{
            width: '0.5rem', height: '0.5rem', borderRadius: '50%',
            background: connected ? '#10b981' : '#ef4444',
          }} />
          <span style={{ fontSize: '0.7rem', color: '#9ca3af', marginRight: '0.5rem' }}>
            {connected ? 'LIVE' : 'RECONNECTING...'}
          </span>

          <select
            value={ispFilter}
            onChange={e => setIspFilter(e.target.value)}
            style={selectStyle}
          >
            <option value="">All ISPs</option>
            {ALL_ISPS.map(i => <option key={i} value={i}>{i}</option>)}
          </select>

          <select
            value={agentFilter}
            onChange={e => setAgentFilter(e.target.value)}
            style={selectStyle}
          >
            <option value="">All Agents</option>
            {ALL_AGENTS.map(a => <option key={a} value={a}>{AGENT_LABELS[a]}</option>)}
          </select>

          <select
            value={verdictFilter}
            onChange={e => setVerdictFilter(e.target.value as '' | 'will' | 'wont')}
            style={selectStyle}
          >
            <option value="">All Verdicts</option>
            <option value="will">WILL</option>
            <option value="wont">WONT</option>
          </select>

          <span style={{ marginLeft: 'auto', fontSize: '0.75rem', color: '#6b7280' }}>
            {stats.perMinute}/min &middot; {stats.total} total
          </span>
        </div>

        {/* Feed */}
        <Card style={{ flex: 1, overflow: 'hidden' }}>
          <CardBody style={{ padding: 0, height: '100%', overflow: 'auto' }}>
            {filtered.length === 0 ? (
              <div style={{ padding: '2rem', textAlign: 'center', color: '#6b7280' }}>
                {connected ? 'Waiting for convictions...' : 'Connecting to conviction stream...'}
              </div>
            ) : (
              filtered.map(c => (
                <ConvictionRow
                  key={c.id}
                  conviction={c}
                  expanded={expandedId === c.id}
                  onToggle={() => setExpandedId(expandedId === c.id ? null : c.id)}
                />
              ))
            )}
          </CardBody>
        </Card>
      </div>

      {/* Right: Velocity + Depth */}
      <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem', minHeight: 0 }}>
        {/* Velocity */}
        <Card>
          <CardHeader title="Conviction Velocity" />
          <CardBody>
            <div style={{ display: 'flex', justifyContent: 'space-around', marginBottom: '1rem' }}>
              <VelocityGauge label="1 min" value={velocity?.global.total_1m ?? stats.perMinute} suffix="/min" />
              <VelocityGauge label="5 min avg" value={velocity?.global.per_minute_5m ?? 0} suffix="/min" />
            </div>
            <div style={{ fontSize: '0.7rem', color: '#6b7280', textTransform: 'uppercase', marginBottom: '0.5rem' }}>
              WILL vs WONT by ISP
            </div>
            {ALL_ISPS.map(isp => {
              const d = velocity?.by_isp?.[isp] ?? stats.byISP[isp] ?? { will: 0, wont: 0, total: 0 };
              const total = d.will + d.wont;
              const willPct = total > 0 ? (d.will / total) * 100 : 50;
              return (
                <div key={isp} style={{ display: 'flex', alignItems: 'center', gap: '0.4rem', marginBottom: '0.3rem' }}>
                  <span style={{ width: '60px', fontSize: '0.7rem', color: ISP_COLORS[isp], fontWeight: 600 }}>{isp}</span>
                  <div style={{ flex: 1, height: '8px', borderRadius: '4px', background: '#374151', overflow: 'hidden', display: 'flex' }}>
                    <div style={{ width: `${willPct}%`, background: '#10b981', transition: 'width 0.5s' }} />
                    <div style={{ flex: 1, background: '#ef4444' }} />
                  </div>
                  <span style={{ width: '35px', fontSize: '0.65rem', color: '#9ca3af', textAlign: 'right' }}>{total}</span>
                </div>
              );
            })}
          </CardBody>
        </Card>

        {/* Agent Memory Depth */}
        <Card style={{ flex: 1, minHeight: 0, overflow: 'auto' }}>
          <CardHeader title="Agent Memory Depth" />
          <CardBody>
            {ALL_AGENTS.map(at => {
              const d = velocity?.by_agent?.[at] ?? stats.byAgent[at] ?? { will: 0, wont: 0, total: 0 };
              const total = d.will + d.wont;
              return (
                <div key={at} style={{ marginBottom: '0.75rem' }}>
                  <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '0.25rem' }}>
                    <span style={{ fontSize: '0.8rem', fontWeight: 600, textTransform: 'capitalize' }}>
                      {AGENT_LABELS[at]}
                    </span>
                    <span style={{ fontSize: '0.75rem', color: '#9ca3af' }}>{total} memories</span>
                  </div>
                  <div style={{ display: 'flex', gap: '0.5rem', fontSize: '0.7rem' }}>
                    <span style={{ color: '#10b981' }}>WILL: {d.will}</span>
                    <span style={{ color: '#ef4444' }}>WONT: {d.wont}</span>
                  </div>
                  <div style={{ height: '4px', borderRadius: '2px', background: '#374151', marginTop: '0.3rem', overflow: 'hidden' }}>
                    <div style={{
                      width: total > 0 ? `${Math.min((total / 500) * 100, 100)}%` : '0%',
                      height: '100%',
                      background: 'linear-gradient(90deg, #6366f1, #a855f7)',
                      transition: 'width 0.5s',
                    }} />
                  </div>
                </div>
              );
            })}
          </CardBody>
        </Card>
      </div>
    </div>
  );
};

const ConvictionRow: React.FC<{
  conviction: Conviction;
  expanded: boolean;
  onToggle: () => void;
}> = ({ conviction: c, expanded, onToggle }) => {
  const ts = new Date(c.created_at);
  const ctx = c.context as Record<string, unknown>;

  return (
    <div
      onClick={onToggle}
      style={{
        padding: '0.6rem 0.75rem',
        borderBottom: '1px solid #1f2937',
        cursor: 'pointer',
        transition: 'background 0.15s',
      }}
      onMouseEnter={e => (e.currentTarget.style.background = '#111827')}
      onMouseLeave={e => (e.currentTarget.style.background = 'transparent')}
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.25rem' }}>
        <span style={{ fontSize: '0.7rem', color: '#6b7280', minWidth: '55px' }}>
          {ts.toLocaleTimeString()}
        </span>
        <span style={{
          fontSize: '0.65rem', fontWeight: 700, padding: '0.1rem 0.3rem', borderRadius: '0.2rem',
          background: `${ISP_COLORS[c.isp] || '#6b7280'}25`, color: ISP_COLORS[c.isp] || '#9ca3af',
        }}>
          {c.isp}
        </span>
        <span style={{ fontSize: '0.65rem', color: '#9ca3af', textTransform: 'capitalize' }}>
          {AGENT_LABELS[c.agent_type] || c.agent_type}
        </span>
        <span style={{
          fontSize: '0.6rem', fontWeight: 800, padding: '0.1rem 0.35rem', borderRadius: '0.2rem',
          background: c.verdict === 'will' ? '#10b98120' : '#ef444420',
          color: c.verdict === 'will' ? '#10b981' : '#ef4444',
          letterSpacing: '0.05em',
        }}>
          {c.verdict.toUpperCase()}
        </span>
      </div>
      <div style={{ fontSize: '0.78rem', color: '#d1d5db', lineHeight: 1.4 }}>
        {expanded ? c.statement : truncate(c.statement, 160)}
      </div>

      {expanded && (
        <div style={{
          marginTop: '0.5rem', padding: '0.5rem', background: '#0f172a',
          borderRadius: '0.375rem', fontSize: '0.7rem', color: '#9ca3af',
        }}>
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: '0.4rem' }}>
            {has(ctx, 'date') && <CtxItem label="Date" value={String(ctx.date)} />}
            {has(ctx, 'day_of_week') && <CtxItem label="Day" value={String(ctx.day_of_week)} />}
            {ctx.hour_utc !== undefined && <CtxItem label="Hour (UTC)" value={String(ctx.hour_utc)} />}
            {has(ctx, 'ip') && <CtxItem label="IP" value={String(ctx.ip)} />}
            {has(ctx, 'pool') && <CtxItem label="Pool" value={String(ctx.pool)} />}
            {has(ctx, 'effective_rate') && <CtxItem label="Rate" value={`${ctx.effective_rate}/hr`} />}
            {has(ctx, 'attempted_volume') && <CtxItem label="Volume" value={String(ctx.attempted_volume)} />}
            {has(ctx, 'bounce_rate') && <CtxItem label="Bounce %" value={`${Number(ctx.bounce_rate).toFixed(2)}%`} />}
            {has(ctx, 'deferral_rate') && <CtxItem label="Deferral %" value={`${Number(ctx.deferral_rate).toFixed(1)}%`} />}
            {has(ctx, 'complaint_rate') && <CtxItem label="Complaint %" value={`${Number(ctx.complaint_rate).toFixed(3)}%`} />}
            {has(ctx, 'acceptance_rate') && <CtxItem label="Acceptance %" value={`${Number(ctx.acceptance_rate).toFixed(1)}%`} />}
            {has(ctx, 'accepted_count') && <CtxItem label="Accepted" value={String(ctx.accepted_count)} />}
            {has(ctx, 'deferral_count') && <CtxItem label="Deferred" value={String(ctx.deferral_count)} />}
            {has(ctx, 'bounce_count') && <CtxItem label="Bounced" value={String(ctx.bounce_count)} />}
            {has(ctx, 'ip_score') && <CtxItem label="IP Score" value={String(Number(ctx.ip_score).toFixed(1))} />}
            {has(ctx, 'recovery_time_min') && <CtxItem label="Recovery" value={`${Number(ctx.recovery_time_min).toFixed(0)}min`} />}
            {Boolean(ctx.is_holiday) && <CtxItem label="Holiday" value={String(ctx.holiday_name || 'Yes')} />}
            {has(ctx, 'email') && <CtxItem label="Email" value={String(ctx.email)} />}
            {has(ctx, 'campaign_id') && <CtxItem label="Campaign" value={String(ctx.campaign_id)} />}
            {has(ctx, 'reason') && <CtxItem label="Reason" value={String(ctx.reason)} />}
          </div>
          {Array.isArray(ctx.dsn_codes) && ctx.dsn_codes.length > 0 && (
            <div style={{ marginTop: '0.4rem' }}>
              <span style={{ color: '#6b7280' }}>DSN: </span>
              {(ctx.dsn_codes as string[]).map((code, i) => (
                <span key={i} style={{
                  display: 'inline-block', padding: '0.1rem 0.3rem', margin: '0.1rem',
                  background: '#1e293b', borderRadius: '0.2rem', fontFamily: 'monospace',
                }}>{code}</span>
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  );
};

const CtxItem: React.FC<{ label: string; value: string }> = ({ label, value }) => (
  <div>
    <div style={{ fontSize: '0.6rem', color: '#6b7280', textTransform: 'uppercase' }}>{label}</div>
    <div style={{ fontFamily: 'monospace', color: '#d1d5db' }}>{value}</div>
  </div>
);

const VelocityGauge: React.FC<{ label: string; value: number; suffix: string }> = ({ label, value, suffix }) => (
  <div style={{ textAlign: 'center' }}>
    <div style={{ fontSize: '1.5rem', fontWeight: 700, color: value > 0 ? '#e0e7ff' : '#4b5563' }}>
      {value.toFixed(1)}
    </div>
    <div style={{ fontSize: '0.65rem', color: '#9ca3af' }}>{suffix}</div>
    <div style={{ fontSize: '0.6rem', color: '#6b7280', marginTop: '0.1rem' }}>{label}</div>
  </div>
);

const selectStyle: React.CSSProperties = {
  padding: '0.25rem 0.5rem', background: '#1f2937', color: '#d1d5db',
  border: '1px solid #374151', borderRadius: '0.25rem', fontSize: '0.75rem',
};

function has(obj: Record<string, unknown>, key: string): boolean {
  const v = obj[key];
  return v !== undefined && v !== null && v !== '' && v !== 0;
}

function truncate(s: string, max: number): string {
  return s.length <= max ? s : s.slice(0, max) + '...';
}

export default MemoryView;
