// Domain Agents — UPCOMING SENDS section.
// Forward-looking counterpart to the scorecard: every scheduled campaign whose
// planned waves fire from now through the next N days for this domain (covers
// both the em.<apex> PMTA route and the m.<apex> SES tenant), grouped by day —
// regardless of which tool deployed it.

import React, { useCallback, useEffect, useState } from 'react';
import { apiFetch } from '../../shared/apiFetch';
import { DOMAIN_AGENT_BASE } from './types';
import {
  C, panelStyle, sectionTitleStyle, thStyle, tdStyle, btnStyle,
  Loading, ErrorState, fmtInt,
} from './ui';

const DAY_CHOICES = [1, 3, 7, 14] as const;

interface UpcomingCampaign {
  id: string;
  name: string;
  status: string;
  subject: string;
  recipients: number;
  isps: string[] | null;
  sending_domains: string[] | null;
  first_wave_at: string | null; // null while async planning hasn't built waves yet
  window_end_at: string | null;
  waves: number;
  waves_pending: number;
}

interface UpcomingDay {
  date: string;
  campaigns: UpcomingCampaign[];
  recipients: number;
  drip_campaigns: number;
  drip_recipients: number;
}

interface UpcomingResponse {
  domain: string;
  days: number;
  dates: UpcomingDay[];
}

const fmtTime = (iso: string | null): string => {
  if (!iso) return 'planning…';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleTimeString('en-US', { hour: 'numeric', minute: '2-digit' });
};

const dayLabel = (date: string): string =>
  date === 'planning' ? 'Audience planning in progress' : date;

const statusColor = (status: string): string => {
  if (status === 'sending') return '#34d399';
  if (status === 'scheduled') return '#a5b4fc';
  return '#fbbf24'; // preparing / finalizing_audience — still planning
};

export const UpcomingSendsSection: React.FC<{ domain: string }> = ({ domain }) => {
  const [days, setDays] = useState<number>(3);
  const [data, setData] = useState<UpcomingResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const fetchUpcoming = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await apiFetch(
        `${DOMAIN_AGENT_BASE}/upcoming?domain=${encodeURIComponent(domain)}&days=${days}`
      );
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const body: UpcomingResponse = await res.json();
      setData(body);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, [domain, days]);

  useEffect(() => { fetchUpcoming(); }, [fetchUpcoming]);

  return (
    <div style={panelStyle}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <h3 style={sectionTitleStyle}>Upcoming sends</h3>
        <div style={{ display: 'flex', gap: 6 }}>
          {DAY_CHOICES.map((d) => (
            <button
              key={d}
              style={{
                ...btnStyle,
                opacity: d === days ? 1 : 0.55,
                borderColor: d === days ? C.accent : undefined,
              }}
              onClick={() => setDays(d)}
            >
              {d}d
            </button>
          ))}
        </div>
      </div>

      {loading && <Loading label="Loading upcoming sends…" />}
      {!loading && error && <ErrorState message={error} onRetry={fetchUpcoming} />}

      {!loading && !error && data && data.dates.length === 0 && (
        <div style={{ color: C.muted, fontSize: 13, padding: '10px 2px' }}>
          Nothing scheduled for this domain in the next {days} day{days === 1 ? '' : 's'}.
        </div>
      )}

      {!loading && !error && data && data.dates.map((day) => (
        <div key={day.date} style={{ marginTop: 10 }}>
          <div style={{ fontSize: 13, color: C.text, fontWeight: 600, margin: '6px 0' }}>
            {dayLabel(day.date)}
            <span style={{ color: C.muted, fontWeight: 400 }}>
              {' '}· {day.campaigns.length} campaign{day.campaigns.length === 1 ? '' : 's'} · {fmtInt(day.recipients)} planned recipients
              {day.drip_campaigns > 0 && (
                <> · +{day.drip_campaigns} drip touch{day.drip_campaigns === 1 ? '' : 'es'} ({fmtInt(day.drip_recipients)} recipients)</>
              )}
            </span>
          </div>
          <table style={{ width: '100%', borderCollapse: 'collapse' }}>
            <thead>
              <tr>
                <th style={thStyle}>First wave (local)</th>
                <th style={thStyle}>Campaign</th>
                <th style={thStyle}>Subject</th>
                <th style={thStyle}>ISPs</th>
                <th style={{ ...thStyle, textAlign: 'right' }}>Recipients</th>
                <th style={{ ...thStyle, textAlign: 'right' }}>Waves left</th>
                <th style={thStyle}>Status</th>
              </tr>
            </thead>
            <tbody>
              {day.campaigns.map((c) => (
                <tr key={c.id}>
                  <td style={tdStyle}>{fmtTime(c.first_wave_at)}</td>
                  <td style={{ ...tdStyle, maxWidth: 280, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={c.name}>
                    {c.name}
                  </td>
                  <td style={{ ...tdStyle, maxWidth: 240, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', color: C.muted }} title={c.subject}>
                    {c.subject || '—'}
                  </td>
                  <td style={{ ...tdStyle, color: C.muted }}>{(c.isps || []).join(', ') || 'all'}</td>
                  <td style={{ ...tdStyle, textAlign: 'right' }}>{fmtInt(c.recipients)}</td>
                  <td style={{ ...tdStyle, textAlign: 'right' }}>{c.waves_pending}/{c.waves}</td>
                  <td style={{ ...tdStyle, color: statusColor(c.status) }}>{c.status}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ))}
    </div>
  );
};

export default UpcomingSendsSection;
