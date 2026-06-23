// Domain Agents — SCORECARD section.
// Per-ISP aggregates over the fetched window + compact per-day trend.

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { apiFetch } from '../../shared/apiFetch';
import {
  DOMAIN_AGENT_BASE, ScorecardRow,
} from './types';
import {
  C, panelStyle, sectionTitleStyle, thStyle, tdStyle, btnStyle,
  Loading, ErrorState, fmtInt, fmtPct,
} from './ui';

const DAY_CHOICES = [7, 14, 30] as const;

interface IspAgg {
  isp: string;
  sends: number;
  human_opens: number;
  machine_opens: number;
  human_clicks: number;
  hard_bounces: number;
  soft_bounces: number;
  other_bounces: number;
}

interface DayAgg {
  day: string;
  sends: number;
  human_opens: number;
}

export const ScorecardSection: React.FC<{ domain: string }> = ({ domain }) => {
  const [days, setDays] = useState<number>(14);
  const [rows, setRows] = useState<ScorecardRow[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const fetchScorecard = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await apiFetch(
        `${DOMAIN_AGENT_BASE}/scorecard?domain=${encodeURIComponent(domain)}&days=${days}`
      );
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data: ScorecardRow[] = await res.json();
      setRows(Array.isArray(data) ? data : []);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, [domain, days]);

  useEffect(() => { fetchScorecard(); }, [fetchScorecard]);

  const ispAggs = useMemo<IspAgg[]>(() => {
    const map = new Map<string, IspAgg>();
    for (const r of rows) {
      let agg = map.get(r.isp);
      if (!agg) {
        agg = {
          isp: r.isp, sends: 0, human_opens: 0, machine_opens: 0,
          human_clicks: 0, hard_bounces: 0, soft_bounces: 0, other_bounces: 0,
        };
        map.set(r.isp, agg);
      }
      agg.sends += r.sends;
      agg.human_opens += r.human_opens;
      agg.machine_opens += r.machine_opens;
      agg.human_clicks += r.human_clicks;
      agg.hard_bounces += r.hard_bounces;
      agg.soft_bounces += r.soft_bounces;
      agg.other_bounces += r.other_bounces;
    }
    return Array.from(map.values()).sort((a, b) => b.sends - a.sends);
  }, [rows]);

  const dayAggs = useMemo<DayAgg[]>(() => {
    const map = new Map<string, DayAgg>();
    for (const r of rows) {
      let agg = map.get(r.day);
      if (!agg) {
        agg = { day: r.day, sends: 0, human_opens: 0 };
        map.set(r.day, agg);
      }
      agg.sends += r.sends;
      agg.human_opens += r.human_opens;
    }
    return Array.from(map.values()).sort((a, b) => a.day.localeCompare(b.day));
  }, [rows]);

  return (
    <div style={panelStyle}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 12 }}>
        <div style={{ ...sectionTitleStyle, marginBottom: 0 }}>Scorecard</div>
        <div style={{ display: 'flex', gap: 6 }}>
          {DAY_CHOICES.map(d => (
            <button
              key={d}
              onClick={() => setDays(d)}
              style={{
                ...btnStyle,
                padding: '4px 10px',
                fontSize: 12,
                background: days === d ? 'rgba(0,229,255,0.2)' : 'rgba(0,176,255,0.08)',
                border: days === d ? `1px solid ${C.accent}` : '1px solid rgba(0,176,255,0.25)',
                color: days === d ? C.accent : C.muted,
              }}
            >
              {d}d
            </button>
          ))}
        </div>
      </div>

      {loading && <Loading label="Loading scorecard…" />}
      {!loading && error && <ErrorState message={`Failed to load scorecard: ${error}`} onRetry={fetchScorecard} />}

      {!loading && !error && rows.length === 0 && (
        <div style={{ color: C.muted, fontSize: 13, padding: '8px 2px' }}>
          No scorecard rows for {domain} in the last {days} days.
        </div>
      )}

      {!loading && !error && rows.length > 0 && (
        <>
          <div style={{ overflowX: 'auto' }}>
            <table style={{ borderCollapse: 'collapse', width: '100%' }}>
              <thead>
                <tr>
                  <th style={thStyle}>Provider</th>
                  <th style={{ ...thStyle, textAlign: 'right' }}>Sends</th>
                  <th style={{ ...thStyle, textAlign: 'right' }}>Human Open %</th>
                  <th style={{ ...thStyle, textAlign: 'right' }}>Machine Open Share</th>
                  <th style={{ ...thStyle, textAlign: 'right' }}>Human Clicks</th>
                  <th style={{ ...thStyle, textAlign: 'right' }}>Hard</th>
                  <th style={{ ...thStyle, textAlign: 'right' }}>Soft</th>
                  <th style={{ ...thStyle, textAlign: 'right' }}>Other</th>
                </tr>
              </thead>
              <tbody>
                {ispAggs.map(a => {
                  const openPct = a.sends > 0 ? a.human_opens / a.sends : NaN;
                  const totalOpens = a.human_opens + a.machine_opens;
                  const machineShare = totalOpens > 0 ? a.machine_opens / totalOpens : NaN;
                  return (
                    <tr key={a.isp}>
                      <td style={{ ...tdStyle, fontWeight: 600 }}>{a.isp}</td>
                      <td style={{ ...tdStyle, textAlign: 'right' }}>{fmtInt(a.sends)}</td>
                      <td style={{ ...tdStyle, textAlign: 'right' }}>{fmtPct(openPct)}</td>
                      <td style={{ ...tdStyle, textAlign: 'right', color: C.muted }}>{fmtPct(machineShare)}</td>
                      <td style={{ ...tdStyle, textAlign: 'right' }}>{fmtInt(a.human_clicks)}</td>
                      <td style={{ ...tdStyle, textAlign: 'right', color: a.hard_bounces > 0 ? C.danger : C.text }}>{fmtInt(a.hard_bounces)}</td>
                      <td style={{ ...tdStyle, textAlign: 'right', color: a.soft_bounces > 0 ? C.warning : C.text }}>{fmtInt(a.soft_bounces)}</td>
                      <td style={{ ...tdStyle, textAlign: 'right', color: C.muted }}>{fmtInt(a.other_bounces)}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>

          <div style={{ marginTop: 16 }}>
            <div style={{ fontSize: 11, fontWeight: 700, letterSpacing: 0.8, textTransform: 'uppercase', color: C.muted, marginBottom: 6 }}>
              Daily trend
            </div>
            <div style={{ overflowX: 'auto' }}>
              <table style={{ borderCollapse: 'collapse' }}>
                <thead>
                  <tr>
                    <th style={thStyle}>Day</th>
                    <th style={{ ...thStyle, textAlign: 'right' }}>Sends</th>
                    <th style={{ ...thStyle, textAlign: 'right' }}>Human Opens</th>
                  </tr>
                </thead>
                <tbody>
                  {dayAggs.map(d => (
                    <tr key={d.day}>
                      <td style={{ ...tdStyle, color: C.muted, fontSize: 12 }}>{d.day}</td>
                      <td style={{ ...tdStyle, textAlign: 'right', fontSize: 12 }}>{fmtInt(d.sends)}</td>
                      <td style={{ ...tdStyle, textAlign: 'right', fontSize: 12 }}>{fmtInt(d.human_opens)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        </>
      )}
    </div>
  );
};
