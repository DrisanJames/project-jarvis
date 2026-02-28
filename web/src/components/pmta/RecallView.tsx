import React, { useState } from 'react';
import { Card, CardHeader, CardBody } from '../common/Card';

const ISP_COLORS: Record<string, string> = {
  gmail: '#ea4335', yahoo: '#6001d2', microsoft: '#00a4ef', apple: '#a2aaad',
  comcast: '#ed1c24', att: '#009fdb', cox: '#0077c8', charter: '#0078d4',
};

const ALL_ISPS = ['gmail', 'yahoo', 'microsoft', 'apple', 'comcast', 'att', 'cox', 'charter'];
const ALL_AGENTS = ['reputation', 'throttle', 'pool', 'warmup', 'emergency', 'suppression'];
const DAYS = ['Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday', 'Sunday'];

interface RecallSynthesis {
  total_relevant: number;
  will_count: number;
  wont_count: number;
  dominant_verdict: 'will' | 'wont';
  confidence: number;
  summary: string;
  key_observations: string[];
  rate_range: [number, number];
  avg_recovery_min: number;
  common_dsn_codes: string[];
  time_patterns: string[];
}

interface ScoredConviction {
  conviction: {
    id: string;
    agent_type: string;
    isp: string;
    verdict: 'will' | 'wont';
    statement: string;
    context: Record<string, unknown>;
    created_at: string;
  };
  similarity: number;
}

interface RecallResponse {
  synthesis: RecallSynthesis;
  convictions: ScoredConviction[];
  isp: string;
  agent_type: string;
}

const RecallView: React.FC = () => {
  const [isp, setIsp] = useState('yahoo');
  const [agentType, setAgentType] = useState('throttle');
  const [dsnCodes, setDsnCodes] = useState('TSS04');
  const [deferralRate, setDeferralRate] = useState(30);
  const [bounceRate, setBounceRate] = useState(0);
  const [dayOfWeek, setDayOfWeek] = useState('');
  const [hourUTC, setHourUTC] = useState(14);
  const [attemptedRate, setAttemptedRate] = useState(500);
  const [ip, setIp] = useState('');

  const [loading, setLoading] = useState(false);
  const [result, setResult] = useState<RecallResponse | null>(null);
  const [error, setError] = useState('');
  const [expandedId, setExpandedId] = useState<string | null>(null);

  const handleRecall = async () => {
    setLoading(true);
    setError('');
    setResult(null);

    try {
      const resp = await fetch('/api/mailing/engine/recall', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify({
          isp,
          agent_type: agentType,
          scenario: {
            dsn_codes: dsnCodes ? dsnCodes.split(',').map(s => s.trim()).filter(Boolean) : [],
            deferral_rate: deferralRate,
            bounce_rate: bounceRate,
            day_of_week: dayOfWeek,
            hour_utc: hourUTC,
            attempted_rate: attemptedRate,
            ip,
          },
          limit: 20,
        }),
      });

      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
      const data: RecallResponse = await resp.json();
      setResult(data);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Unknown error');
    } finally {
      setLoading(false);
    }
  };

  return (
    <div style={{ display: 'grid', gridTemplateColumns: '340px 1fr', gap: '1rem', minHeight: '400px' }}>
      {/* Left: Scenario Builder */}
      <Card style={{ overflow: 'visible' }}>
        <CardHeader title="Scenario Builder" />
        <CardBody>
          <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '0.5rem' }}>
              <FormField label="ISP">
                <select value={isp} onChange={e => setIsp(e.target.value)} style={inputStyle}>
                  {ALL_ISPS.map(i => <option key={i} value={i}>{i}</option>)}
                </select>
              </FormField>
              <FormField label="Agent">
                <select value={agentType} onChange={e => setAgentType(e.target.value)} style={inputStyle}>
                  {ALL_AGENTS.map(a => (
                    <option key={a} value={a}>{a.charAt(0).toUpperCase() + a.slice(1)}</option>
                  ))}
                </select>
              </FormField>
            </div>

            <FormField label="DSN Codes">
              <input
                type="text"
                value={dsnCodes}
                onChange={e => setDsnCodes(e.target.value)}
                placeholder="TSS04, 421-4.7.28"
                style={inputStyle}
              />
            </FormField>

            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '0.5rem' }}>
              <FormField label={`Deferral: ${deferralRate}%`}>
                <input
                  type="range"
                  min={0} max={100} step={1}
                  value={deferralRate}
                  onChange={e => setDeferralRate(Number(e.target.value))}
                  style={{ width: '100%', accentColor: '#6366f1' }}
                />
              </FormField>
              <FormField label={`Bounce: ${bounceRate}%`}>
                <input
                  type="range"
                  min={0} max={50} step={0.5}
                  value={bounceRate}
                  onChange={e => setBounceRate(Number(e.target.value))}
                  style={{ width: '100%', accentColor: '#6366f1' }}
                />
              </FormField>
            </div>

            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: '0.5rem' }}>
              <FormField label="Day">
                <select value={dayOfWeek} onChange={e => setDayOfWeek(e.target.value)} style={inputStyle}>
                  <option value="">Any</option>
                  {DAYS.map(d => <option key={d} value={d}>{d}</option>)}
                </select>
              </FormField>
              <FormField label="Hour (UTC)">
                <input
                  type="number" min={0} max={23}
                  value={hourUTC}
                  onChange={e => setHourUTC(Number(e.target.value))}
                  style={inputStyle}
                />
              </FormField>
              <FormField label="Rate/hr">
                <input
                  type="number" min={0}
                  value={attemptedRate}
                  onChange={e => setAttemptedRate(Number(e.target.value))}
                  style={inputStyle}
                />
              </FormField>
            </div>

            <FormField label="Source IP (optional)">
              <input
                type="text"
                value={ip}
                onChange={e => setIp(e.target.value)}
                placeholder="144.225.178.5"
                style={inputStyle}
              />
            </FormField>

            <button
              onClick={handleRecall}
              disabled={loading}
              style={{
                padding: '0.6rem', background: loading ? '#374151' : '#4f46e5',
                color: '#fff', border: 'none', borderRadius: '0.375rem',
                cursor: loading ? 'wait' : 'pointer', fontSize: '0.85rem',
                fontWeight: 600,
              }}
            >
              {loading ? 'Recalling...' : 'Ask the Agent'}
            </button>
          </div>
        </CardBody>
      </Card>

      {/* Right: Results */}
      <div style={{ display: 'flex', flexDirection: 'column', gap: '1rem', minHeight: 0, overflow: 'auto' }}>
        {error && (
          <div style={{ padding: '0.75rem', background: '#7f1d1d', color: '#fca5a5', borderRadius: '0.375rem', fontSize: '0.85rem' }}>
            Error: {error}
          </div>
        )}

        {!result && !loading && !error && (
          <div style={{
            display: 'flex', alignItems: 'center', justifyContent: 'center',
            height: '100%', color: '#6b7280', fontSize: '0.9rem',
          }}>
            Configure a scenario and click &quot;Ask the Agent&quot; to query its memory.
          </div>
        )}

        {result && (
          <>
            {/* Synthesis Card */}
            <Card>
              <CardBody>
                <div style={{ display: 'flex', alignItems: 'center', gap: '1rem', marginBottom: '1rem' }}>
                  {/* Verdict */}
                  <div style={{
                    width: '64px', height: '64px', borderRadius: '50%',
                    display: 'flex', alignItems: 'center', justifyContent: 'center',
                    background: result.synthesis.dominant_verdict === 'will' ? '#10b98120' : '#ef444420',
                    color: result.synthesis.dominant_verdict === 'will' ? '#10b981' : '#ef4444',
                    fontSize: '0.75rem', fontWeight: 800, flexShrink: 0,
                  }}>
                    {result.synthesis.dominant_verdict.toUpperCase()}
                  </div>

                  <div style={{ flex: 1 }}>
                    <div style={{ display: 'flex', gap: '0.75rem', alignItems: 'center', marginBottom: '0.3rem' }}>
                      <span style={{ fontSize: '0.8rem', color: '#9ca3af' }}>
                        {result.synthesis.total_relevant} memories matched
                      </span>
                      <span style={{ fontSize: '0.75rem', color: '#6b7280' }}>
                        WILL: {result.synthesis.will_count} &middot; WONT: {result.synthesis.wont_count}
                      </span>
                    </div>
                    {/* Confidence bar */}
                    <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                      <div style={{ flex: 1, height: '6px', borderRadius: '3px', background: '#374151', overflow: 'hidden' }}>
                        <div style={{
                          width: `${result.synthesis.confidence * 100}%`, height: '100%',
                          background: result.synthesis.confidence > 0.7 ? '#10b981' : result.synthesis.confidence > 0.4 ? '#f59e0b' : '#ef4444',
                          transition: 'width 0.5s',
                        }} />
                      </div>
                      <span style={{ fontSize: '0.75rem', color: '#d1d5db', fontWeight: 600 }}>
                        {(result.synthesis.confidence * 100).toFixed(0)}%
                      </span>
                    </div>
                  </div>
                </div>

                {/* Summary */}
                <div style={{ fontSize: '0.85rem', color: '#d1d5db', lineHeight: 1.6, marginBottom: '1rem' }}>
                  {result.synthesis.summary}
                </div>

                {/* Metric pills */}
                <div style={{ display: 'flex', flexWrap: 'wrap', gap: '0.5rem', marginBottom: '1rem' }}>
                  {result.synthesis.rate_range[0] > 0 && (
                    <Pill label="Rate Range" value={`${result.synthesis.rate_range[0]}-${result.synthesis.rate_range[1]}/hr`} />
                  )}
                  {result.synthesis.avg_recovery_min > 0 && (
                    <Pill label="Avg Recovery" value={`${result.synthesis.avg_recovery_min.toFixed(0)} min`} />
                  )}
                  {result.synthesis.common_dsn_codes?.length > 0 && (
                    <Pill label="DSN Codes" value={result.synthesis.common_dsn_codes.join(', ')} />
                  )}
                </div>

                {/* Key Observations */}
                {result.synthesis.key_observations?.length > 0 && (
                  <div style={{ marginBottom: '0.75rem' }}>
                    <div style={{ fontSize: '0.7rem', color: '#6b7280', textTransform: 'uppercase', marginBottom: '0.4rem' }}>
                      Key Observations
                    </div>
                    <ul style={{ margin: 0, paddingLeft: '1.25rem' }}>
                      {result.synthesis.key_observations.map((obs, i) => (
                        <li key={i} style={{ fontSize: '0.8rem', color: '#d1d5db', marginBottom: '0.25rem', lineHeight: 1.4 }}>
                          {obs}
                        </li>
                      ))}
                    </ul>
                  </div>
                )}

                {/* Time Patterns */}
                {result.synthesis.time_patterns?.length > 0 && (
                  <div>
                    <div style={{ fontSize: '0.7rem', color: '#6b7280', textTransform: 'uppercase', marginBottom: '0.4rem' }}>
                      Time Patterns
                    </div>
                    {result.synthesis.time_patterns.map((pat, i) => (
                      <div key={i} style={{ fontSize: '0.78rem', color: '#9ca3af', marginBottom: '0.2rem' }}>
                        {pat}
                      </div>
                    ))}
                  </div>
                )}
              </CardBody>
            </Card>

            {/* Matched Convictions */}
            {result.convictions?.length > 0 && (
              <Card>
                <CardHeader title={`Matched Convictions (${result.convictions.length})`} />
                <CardBody style={{ padding: 0 }}>
                  {result.convictions.map(sc => (
                    <div
                      key={sc.conviction.id}
                      onClick={() => setExpandedId(expandedId === sc.conviction.id ? null : sc.conviction.id)}
                      style={{
                        padding: '0.6rem 0.75rem', borderBottom: '1px solid #1f2937',
                        cursor: 'pointer',
                      }}
                    >
                      <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', marginBottom: '0.25rem' }}>
                        {/* Similarity bar */}
                        <div style={{
                          width: '40px', height: '4px', borderRadius: '2px',
                          background: '#374151', overflow: 'hidden', flexShrink: 0,
                        }}>
                          <div style={{
                            width: `${sc.similarity * 100}%`, height: '100%',
                            background: sc.similarity > 0.7 ? '#10b981' : sc.similarity > 0.4 ? '#f59e0b' : '#6b7280',
                          }} />
                        </div>
                        <span style={{ fontSize: '0.65rem', color: '#9ca3af', minWidth: '30px' }}>
                          {(sc.similarity * 100).toFixed(0)}%
                        </span>
                        <span style={{
                          fontSize: '0.6rem', fontWeight: 800, padding: '0.1rem 0.3rem', borderRadius: '0.2rem',
                          background: sc.conviction.verdict === 'will' ? '#10b98120' : '#ef444420',
                          color: sc.conviction.verdict === 'will' ? '#10b981' : '#ef4444',
                        }}>
                          {sc.conviction.verdict.toUpperCase()}
                        </span>
                        <span style={{ fontSize: '0.65rem', color: '#6b7280' }}>
                          {new Date(sc.conviction.created_at).toLocaleDateString()}
                        </span>
                        <span style={{
                          fontSize: '0.6rem', padding: '0.1rem 0.3rem', borderRadius: '0.2rem',
                          background: `${ISP_COLORS[sc.conviction.isp] || '#6b7280'}20`,
                          color: ISP_COLORS[sc.conviction.isp] || '#9ca3af',
                        }}>
                          {sc.conviction.isp}
                        </span>
                      </div>
                      <div style={{ fontSize: '0.78rem', color: '#d1d5db', lineHeight: 1.4 }}>
                        {expandedId === sc.conviction.id ? sc.conviction.statement : truncate(sc.conviction.statement, 200)}
                      </div>

                      {expandedId === sc.conviction.id && (
                        <div style={{
                          marginTop: '0.5rem', padding: '0.5rem', background: '#0f172a',
                          borderRadius: '0.375rem', fontSize: '0.7rem', color: '#9ca3af',
                        }}>
                          <pre style={{ margin: 0, whiteSpace: 'pre-wrap', fontFamily: 'monospace', fontSize: '0.65rem' }}>
                            {JSON.stringify(sc.conviction.context, null, 2)}
                          </pre>
                        </div>
                      )}
                    </div>
                  ))}
                </CardBody>
              </Card>
            )}
          </>
        )}
      </div>
    </div>
  );
};

const FormField: React.FC<{ label: string; children: React.ReactNode }> = ({ label, children }) => (
  <div>
    <label style={{ display: 'block', fontSize: '0.7rem', color: '#9ca3af', marginBottom: '0.25rem', textTransform: 'uppercase' }}>
      {label}
    </label>
    {children}
  </div>
);

const Pill: React.FC<{ label: string; value: string }> = ({ label, value }) => (
  <div style={{
    padding: '0.3rem 0.6rem', background: '#1e293b', borderRadius: '0.375rem',
    fontSize: '0.75rem', display: 'flex', gap: '0.3rem', alignItems: 'center',
  }}>
    <span style={{ color: '#6b7280' }}>{label}:</span>
    <span style={{ color: '#d1d5db', fontWeight: 600, fontFamily: 'monospace' }}>{value}</span>
  </div>
);

const inputStyle: React.CSSProperties = {
  width: '100%', padding: '0.4rem 0.5rem', background: '#111827',
  color: '#d1d5db', border: '1px solid #374151', borderRadius: '0.25rem',
  fontSize: '0.8rem', boxSizing: 'border-box',
};

function truncate(s: string, max: number): string {
  return s.length <= max ? s : s.slice(0, max) + '...';
}

export default RecallView;
