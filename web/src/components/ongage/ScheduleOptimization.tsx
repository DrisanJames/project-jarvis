import { useMemo } from 'react';
import { OngageScheduleAnalysisResponse } from '../../types';
import { useApi } from '../../hooks/useApi';
import { Card, CardBody, Heatmap, PerformanceBadge, Loading } from '../common';

const DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const HOURS = Array.from({ length: 24 }, (_, i) => {
  const hour = i % 12 || 12;
  const ampm = i < 12 ? 'AM' : 'PM';
  return `${hour}${ampm}`;
});

export function ScheduleOptimization() {
  const { data, loading, error, refetch } = useApi<OngageScheduleAnalysisResponse>('/api/ongage/schedule');

  // Convert data to heatmap format
  const heatmapData = useMemo(() => {
    if (!data?.analysis) return [];
    
    return data.analysis.map(a => ({
      x: a.hour,
      y: a.day_of_week - 1, // Convert to 0-indexed
      value: a.avg_open_rate,
      label: `${a.day_name} ${HOURS[a.hour]}: ${(a.avg_open_rate * 100).toFixed(1)}% open rate (${a.campaign_count} campaigns)`,
    }));
  }, [data]);

  // Find best times
  const bestTimes = useMemo(() => {
    if (!data?.analysis) return [];
    
    return [...data.analysis]
      .sort((a, b) => b.avg_open_rate - a.avg_open_rate)
      .slice(0, 5);
  }, [data]);

  // Calculate day performance
  const dayPerformance = useMemo(() => {
    if (!data?.analysis) return [];
    
    const dayStats = DAYS.map((day, i) => {
      const dayData = data.analysis.filter(a => a.day_of_week === i + 1);
      const totalSent = dayData.reduce((sum, d) => sum + d.total_sent, 0);
      const avgOpenRate = dayData.length > 0
        ? dayData.reduce((sum, d) => sum + d.avg_open_rate * d.total_sent, 0) / (totalSent || 1)
        : 0;
      const campaigns = dayData.reduce((sum, d) => sum + d.campaign_count, 0);
      
      return {
        day,
        dayIndex: i + 1,
        totalSent,
        avgOpenRate,
        campaigns,
      };
    });
    
    return dayStats.sort((a, b) => b.avgOpenRate - a.avgOpenRate);
  }, [data]);

  const formatPercent = (num: number): string => `${(num * 100).toFixed(2)}%`;

  if (loading) {
    return <Loading message="Loading schedule analysis..." />;
  }

  if (error) {
    return (
      <Card>
        <CardBody>
          <div style={{ textAlign: 'center', padding: '2rem' }}>
            <p>Error loading schedule data: {error}</p>
            <button onClick={refetch} style={{ marginTop: '1rem', padding: '0.5rem 1rem' }}>
              Retry
            </button>
          </div>
        </CardBody>
      </Card>
    );
  }

  return (
    <div className="schedule-optimization">
      <Card>
        <div style={{ 
          display: 'flex', 
          justifyContent: 'space-between', 
          alignItems: 'center', 
          padding: '1rem 1.5rem', 
          borderBottom: '1px solid var(--border-color)',
        }}>
          <h2 style={{ margin: 0 }}>Send Time Optimization</h2>
          <button
            onClick={refetch}
            style={{
              padding: '0.5rem 1rem',
              borderRadius: '4px',
              border: 'none',
              backgroundColor: 'var(--primary-color)',
              color: 'white',
              cursor: 'pointer',
            }}
          >
            Refresh
          </button>
        </div>
        <CardBody>
          <div style={{ 
            display: 'grid', 
            gridTemplateColumns: '1fr 300px', 
            gap: '2rem',
            alignItems: 'start',
          }}>
            {/* Heatmap */}
            <div>
              <h4 style={{ margin: '0 0 1rem 0' }}>Open Rate by Day & Hour</h4>
              <Heatmap
                data={heatmapData}
                xLabels={HOURS}
                yLabels={DAYS}
                colorScale={{ min: '#fed7d7', max: '#48bb78' }}
                cellSize={32}
                showValues={false}
                valueFormatter={(v) => `${(v * 100).toFixed(1)}%`}
              />
            </div>

            {/* Best Times & Day Performance */}
            <div>
              {/* Optimal Times */}
              <div style={{ marginBottom: '2rem' }}>
                <h4 style={{ margin: '0 0 1rem 0' }}>
                  Optimal Send Times
                  <span style={{ 
                    marginLeft: '0.5rem', 
                    fontSize: '0.75rem', 
                    color: 'var(--text-muted)',
                    fontWeight: 'normal',
                  }}>
                    ({data?.optimal_count || 0} optimal slots)
                  </span>
                </h4>
                <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
                  {bestTimes.map((time, i) => (
                    <div 
                      key={`${time.day_of_week}-${time.hour}`}
                      style={{
                        display: 'flex',
                        justifyContent: 'space-between',
                        alignItems: 'center',
                        padding: '0.75rem',
                        borderRadius: '8px',
                        backgroundColor: i === 0 ? 'rgba(72, 187, 120, 0.1)' : 'var(--card-bg)',
                        border: `1px solid ${i === 0 ? '#48bb78' : 'var(--border-color)'}`,
                      }}
                    >
                      <div>
                        <div style={{ fontWeight: 500 }}>
                          {time.day_name} at {HOURS[time.hour]}
                        </div>
                        <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>
                          {time.campaign_count} campaigns
                        </div>
                      </div>
                      <div style={{ textAlign: 'right' }}>
                        <div style={{ fontWeight: 600, color: '#22543d' }}>
                          {formatPercent(time.avg_open_rate)}
                        </div>
                        <PerformanceBadge level={time.performance} size="sm" />
                      </div>
                    </div>
                  ))}
                </div>
              </div>

              {/* Day Rankings */}
              <div>
                <h4 style={{ margin: '0 0 1rem 0' }}>Performance by Day</h4>
                <div style={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
                  {dayPerformance.map((day, i) => (
                    <div 
                      key={day.day}
                      style={{
                        display: 'flex',
                        justifyContent: 'space-between',
                        alignItems: 'center',
                        padding: '0.5rem 0.75rem',
                        borderRadius: '4px',
                        backgroundColor: i < 2 ? 'rgba(72, 187, 120, 0.05)' : 'transparent',
                      }}
                    >
                      <div style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                        <span style={{ 
                          width: 20, 
                          height: 20, 
                          borderRadius: '50%', 
                          backgroundColor: i < 2 ? '#48bb78' : i > 4 ? '#f56565' : '#ecc94b',
                          color: 'white',
                          display: 'flex',
                          alignItems: 'center',
                          justifyContent: 'center',
                          fontSize: '0.625rem',
                          fontWeight: 'bold',
                        }}>
                          {i + 1}
                        </span>
                        <span style={{ fontWeight: 500 }}>{day.day}</span>
                      </div>
                      <div style={{ textAlign: 'right' }}>
                        <span style={{ fontWeight: 600 }}>
                          {formatPercent(day.avgOpenRate)}
                        </span>
                        <span style={{ fontSize: '0.75rem', color: 'var(--text-muted)', marginLeft: '0.5rem' }}>
                          ({day.campaigns} campaigns)
                        </span>
                      </div>
                    </div>
                  ))}
                </div>
              </div>
            </div>
          </div>

          {/* Insights */}
          <div style={{ 
            marginTop: '2rem', 
            padding: '1rem', 
            backgroundColor: 'var(--info-bg, #ebf8ff)', 
            borderRadius: '8px',
            border: '1px solid var(--info-border, #90cdf4)',
          }}>
            <h4 style={{ margin: '0 0 0.5rem 0', color: 'var(--info-text, #2c5282)' }}>
              💡 Scheduling Insights
            </h4>
            <ul style={{ margin: 0, paddingLeft: '1.5rem', color: 'var(--info-text, #2c5282)', fontSize: '0.875rem' }}>
              {bestTimes.length > 0 && (
                <li>
                  Best time to send: <strong>{bestTimes[0].day_name} at {HOURS[bestTimes[0].hour]}</strong> 
                  {' '}with {formatPercent(bestTimes[0].avg_open_rate)} average open rate
                </li>
              )}
              {dayPerformance.length > 0 && (
                <li>
                  <strong>{dayPerformance[0].day}</strong> consistently performs best overall
                </li>
              )}
              {dayPerformance.length > 4 && (
                <li>
                  Consider avoiding <strong>{dayPerformance[dayPerformance.length - 1].day}</strong> which has the lowest engagement
                </li>
              )}
            </ul>
          </div>
        </CardBody>
      </Card>
    </div>
  );
}

export default ScheduleOptimization;
