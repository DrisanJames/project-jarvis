import React, { useState, useCallback } from 'react';
import { useApi } from '../../hooks/useApi';
import { Card, CardHeader, CardBody } from '../common/Card';
import { Loading } from '../common/Loading';
import { StatusBadge } from '../common/StatusBadge';

interface WarmupIP {
  id: string;
  ip_address: string;
  hostname: string;
  warmup_day: number;
  warmup_daily_limit: number;
  warmup_started_at: string;
  total_sent: number;
  reputation_score: number;
  status: string;
  today_sent: number;
  today_bounce_rate: number;
  today_complaint_rate: number;
  progress_pct: number;
}

interface WarmupDay {
  date: string;
  planned_volume: number;
  actual_sent: number;
  actual_delivered: number;
  actual_bounced: number;
  actual_complained: number;
  bounce_rate: number;
  complaint_rate: number;
  warmup_day: number;
  status: string;
  notes: string;
}

const IPWarmupDashboard: React.FC = () => {
  const [selectedIP, setSelectedIP] = useState<string | null>(null);
  const { data, loading, refetch } = useApi<{ warming_ips: WarmupIP[]; total: number }>('/api/mailing/warmup/dashboard', {
    pollingInterval: 30000,
  });
  const { data: detailData } = useApi<{ ip_id: string; days: WarmupDay[] }>(
    `/api/mailing/ips/${selectedIP}/warmup/status`,
    { enabled: !!selectedIP }
  );

  const handlePause = useCallback(async (ipId: string) => {
    await fetch(`/api/mailing/ips/${ipId}/warmup/pause`, { method: 'POST', credentials: 'include' });
    refetch();
  }, [refetch]);

  if (loading && !data) return <Loading />;

  const ips = data?.warming_ips ?? [];

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '1.5rem' }}>
      {/* Warmup progress cards */}
      {ips.length > 0 ? (
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(350px, 1fr))', gap: '1rem' }}>
          {ips.map(ip => (
            <Card key={ip.id}>
              <CardBody>
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: '0.75rem' }}>
                  <div>
                    <div style={{ fontFamily: 'monospace', fontSize: '1rem', fontWeight: 600 }}>{ip.ip_address}</div>
                    <div style={{ fontSize: '0.8rem', color: '#9ca3af' }}>{ip.hostname}</div>
                  </div>
                  <div style={{ display: 'flex', gap: '0.5rem', alignItems: 'center' }}>
                    <StatusBadge
                      status={ip.today_bounce_rate > 3 || ip.today_complaint_rate > 0.1 ? 'critical' :
                              ip.today_bounce_rate > 2 || ip.today_complaint_rate > 0.05 ? 'warning' : 'healthy'}
                      label={`Day ${ip.warmup_day}`}
                    />
                    <button
                      onClick={() => handlePause(ip.id)}
                      style={{ padding: '0.2rem 0.4rem', background: '#7f1d1d', color: '#fca5a5', border: 'none', borderRadius: '0.25rem', cursor: 'pointer', fontSize: '0.7rem' }}
                    >
                      Pause
                    </button>
                  </div>
                </div>

                {/* Progress bar */}
                <div style={{ background: '#1f2937', borderRadius: '9999px', height: '0.5rem', marginBottom: '0.75rem' }}>
                  <div
                    style={{
                      width: `${Math.min(ip.progress_pct, 100)}%`,
                      height: '100%',
                      borderRadius: '9999px',
                      background: ip.progress_pct < 33 ? '#6366f1' : ip.progress_pct < 66 ? '#8b5cf6' : '#10b981',
                      transition: 'width 0.3s ease',
                    }}
                  />
                </div>

                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: '0.5rem', fontSize: '0.8rem' }}>
                  <div>
                    <div style={{ color: '#9ca3af' }}>Today Sent</div>
                    <div style={{ fontWeight: 600 }}>{ip.today_sent.toLocaleString()} / {ip.warmup_daily_limit.toLocaleString()}</div>
                  </div>
                  <div>
                    <div style={{ color: '#9ca3af' }}>Bounce Rate</div>
                    <div style={{ fontWeight: 600, color: ip.today_bounce_rate > 3 ? '#ef4444' : ip.today_bounce_rate > 2 ? '#f59e0b' : '#10b981' }}>
                      {ip.today_bounce_rate.toFixed(2)}%
                    </div>
                  </div>
                  <div>
                    <div style={{ color: '#9ca3af' }}>Complaints</div>
                    <div style={{ fontWeight: 600, color: ip.today_complaint_rate > 0.1 ? '#ef4444' : '#10b981' }}>
                      {ip.today_complaint_rate.toFixed(3)}%
                    </div>
                  </div>
                </div>

                <button
                  onClick={() => setSelectedIP(selectedIP === ip.id ? null : ip.id)}
                  style={{ marginTop: '0.75rem', width: '100%', padding: '0.4rem', background: '#374151', color: '#d1d5db', border: 'none', borderRadius: '0.375rem', cursor: 'pointer', fontSize: '0.8rem' }}
                >
                  {selectedIP === ip.id ? 'Hide Details' : 'View Daily Breakdown'}
                </button>

                {/* Detailed warmup log */}
                {selectedIP === ip.id && detailData?.days && (
                  <div style={{ marginTop: '0.75rem', maxHeight: '250px', overflowY: 'auto' }}>
                    <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: '0.75rem' }}>
                      <thead>
                        <tr style={{ borderBottom: '1px solid #374151' }}>
                          <th style={{ textAlign: 'left', padding: '0.3rem', color: '#9ca3af' }}>Day</th>
                          <th style={{ textAlign: 'right', padding: '0.3rem', color: '#9ca3af' }}>Planned</th>
                          <th style={{ textAlign: 'right', padding: '0.3rem', color: '#9ca3af' }}>Sent</th>
                          <th style={{ textAlign: 'right', padding: '0.3rem', color: '#9ca3af' }}>Bounce</th>
                          <th style={{ textAlign: 'center', padding: '0.3rem', color: '#9ca3af' }}>Status</th>
                        </tr>
                      </thead>
                      <tbody>
                        {detailData.days.map((day, i) => (
                          <tr key={i} style={{ borderBottom: '1px solid #1f2937' }}>
                            <td style={{ padding: '0.3rem' }}>
                              D{day.warmup_day} <span style={{ color: '#6b7280' }}>({day.date})</span>
                            </td>
                            <td style={{ padding: '0.3rem', textAlign: 'right' }}>{day.planned_volume.toLocaleString()}</td>
                            <td style={{ padding: '0.3rem', textAlign: 'right' }}>{day.actual_sent.toLocaleString()}</td>
                            <td style={{ padding: '0.3rem', textAlign: 'right', color: day.bounce_rate > 3 ? '#ef4444' : '#10b981' }}>
                              {day.bounce_rate.toFixed(2)}%
                            </td>
                            <td style={{ padding: '0.3rem', textAlign: 'center' }}>
                              <StatusBadge
                                status={day.status === 'completed' ? 'healthy' : day.status === 'in_progress' ? 'warning' : day.status === 'failed' ? 'critical' : 'neutral'}
                                label={day.status}
                              />
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </CardBody>
            </Card>
          ))}
        </div>
      ) : (
        <Card>
          <CardBody>
            <div style={{ textAlign: 'center', padding: '3rem', color: '#9ca3af' }}>
              <p style={{ fontSize: '1.1rem', marginBottom: '0.5rem' }}>No IPs currently warming up</p>
              <p style={{ fontSize: '0.85rem' }}>Start a warmup from the IP Management tab by clicking "Warm" on a pending IP.</p>
            </div>
          </CardBody>
        </Card>
      )}

      {/* Warmup schedule reference */}
      <Card>
        <CardHeader>Standard 30-Day Warmup Schedule</CardHeader>
        <CardBody>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(120px, 1fr))', gap: '0.5rem', fontSize: '0.8rem' }}>
            {[
              { days: 'Day 1-2', vol: '50' }, { days: 'Day 3-4', vol: '100' },
              { days: 'Day 5-7', vol: '250' }, { days: 'Day 8-10', vol: '500' },
              { days: 'Day 11-14', vol: '1,000' }, { days: 'Day 15-18', vol: '2,500' },
              { days: 'Day 19-22', vol: '5,000' }, { days: 'Day 23-26', vol: '10,000' },
              { days: 'Day 27-30', vol: '25,000' }, { days: 'Day 31+', vol: '50,000+' },
            ].map((stage, i) => (
              <div key={i} style={{ padding: '0.5rem', background: '#1f2937', borderRadius: '0.375rem', textAlign: 'center' }}>
                <div style={{ color: '#9ca3af', fontSize: '0.7rem' }}>{stage.days}</div>
                <div style={{ fontWeight: 600, color: '#e5e7eb' }}>{stage.vol}/day</div>
              </div>
            ))}
          </div>
        </CardBody>
      </Card>
    </div>
  );
};

export default IPWarmupDashboard;
