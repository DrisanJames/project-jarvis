import { useState, useMemo, useEffect, useCallback } from 'react';
import { OngageSubjectAnalysisResponse, OngageSubjectAnalysis } from '../../types';
import { Card, CardBody, SortableTable, PerformanceBadge, Loading } from '../common';
import type { SortableColumn } from '../common';

interface SubjectFeature {
  name: string;
  key: keyof OngageSubjectAnalysis;
  description: string;
}

const SUBJECT_FEATURES: SubjectFeature[] = [
  { name: 'Emoji', key: 'has_emoji', description: 'Contains emoji characters' },
  { name: 'Number', key: 'has_number', description: 'Contains numbers (e.g., "50% Off")' },
  { name: 'Question', key: 'has_question', description: 'Contains a question mark' },
  { name: 'Urgency', key: 'has_urgency', description: 'Contains urgency words (e.g., "Limited time")' },
];

interface SubjectLineAnalysisProps {
  days?: number;
}

export function SubjectLineAnalysis({ days = 1 }: SubjectLineAnalysisProps) {
  const [data, setData] = useState<OngageSubjectAnalysisResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const fetchData = useCallback(async () => {
    try {
      setLoading(true);
      setError(null);
      const response = await fetch(`/api/ongage/subjects?min_audience=10000`);
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const result = await response.json();
      setData(result);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to fetch subject analysis');
    } finally {
      setLoading(false);
    }
  }, [days]);

  useEffect(() => {
    fetchData();
  }, [fetchData]);

  const refetch = () => fetchData();
  
  const [performanceFilter, setPerformanceFilter] = useState<string>('all');
  const [featureFilter, setFeatureFilter] = useState<string>('all');
  const [searchFilter, setSearchFilter] = useState<string>('');

  // Filter data
  const filteredData = useMemo(() => {
    if (!data?.subject_analysis) return [];
    
    return data.subject_analysis.filter(s => {
      if (performanceFilter !== 'all' && s.performance !== performanceFilter) return false;
      if (featureFilter !== 'all') {
        const feature = SUBJECT_FEATURES.find(f => f.key === featureFilter);
        if (feature && !s[feature.key]) return false;
      }
      if (searchFilter && !s.subject.toLowerCase().includes(searchFilter.toLowerCase())) {
        return false;
      }
      return true;
    });
  }, [data, performanceFilter, featureFilter, searchFilter]);

  // Calculate feature performance stats
  const featureStats = useMemo(() => {
    if (!data?.subject_analysis) return [];
    
    return SUBJECT_FEATURES.map(feature => {
      const withFeature = data.subject_analysis.filter(s => s[feature.key]);
      const withoutFeature = data.subject_analysis.filter(s => !s[feature.key]);
      
      const avgWithFeature = withFeature.length > 0 
        ? withFeature.reduce((sum, s) => sum + s.avg_open_rate, 0) / withFeature.length 
        : 0;
      const avgWithoutFeature = withoutFeature.length > 0
        ? withoutFeature.reduce((sum, s) => sum + s.avg_open_rate, 0) / withoutFeature.length
        : 0;
      
      return {
        ...feature,
        count: withFeature.length,
        total: data.subject_analysis.length,
        avgOpenRate: avgWithFeature,
        comparison: avgWithFeature - avgWithoutFeature,
      };
    });
  }, [data]);

  const formatPercent = (num: number): string => `${(num * 100).toFixed(2)}%`;
  const formatComparison = (num: number): string => {
    const pct = (num * 100).toFixed(2);
    return num >= 0 ? `+${pct}%` : `${pct}%`;
  };

  const columns: SortableColumn<OngageSubjectAnalysis>[] = [
    {
      key: 'subject',
      header: 'Subject Line',
      render: (s) => (
        <div style={{ maxWidth: 400 }}>
          <div style={{ fontWeight: 500, wordBreak: 'break-word' }}>{s.subject}</div>
          <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)', marginTop: '0.25rem' }}>
            {s.length} chars • {s.campaign_count} campaign{s.campaign_count !== 1 ? 's' : ''}
            {s.esps.length > 0 && ` • ${s.esps.join(', ')}`}
          </div>
        </div>
      ),
      width: '400px',
    },
    {
      key: 'features',
      header: 'Features',
      sortable: false,
      render: (s) => (
        <div style={{ display: 'flex', gap: '0.25rem', flexWrap: 'wrap' }}>
          {s.has_emoji && <span className="feature-tag">😊 Emoji</span>}
          {s.has_number && <span className="feature-tag"># Number</span>}
          {s.has_question && <span className="feature-tag">? Question</span>}
          {s.has_urgency && <span className="feature-tag">⚡ Urgency</span>}
        </div>
      ),
      width: '200px',
    },
    {
      key: 'total_sent',
      header: 'Sent',
      align: 'right',
      render: (s) => s.total_sent.toLocaleString(),
    },
    {
      key: 'avg_open_rate',
      header: 'Open Rate',
      align: 'right',
      render: (s) => (
        <span style={{ 
          color: s.avg_open_rate >= 0.20 ? '#22543d' : 
                 s.avg_open_rate >= 0.12 ? '#744210' : '#822727',
          fontWeight: 500,
        }}>
          {formatPercent(s.avg_open_rate)}
        </span>
      ),
    },
    {
      key: 'avg_click_rate',
      header: 'Click Rate',
      align: 'right',
      render: (s) => formatPercent(s.avg_click_rate),
    },
    {
      key: 'avg_ctr',
      header: 'CTR',
      align: 'right',
      render: (s) => formatPercent(s.avg_ctr),
    },
    {
      key: 'performance',
      header: 'Performance',
      align: 'center',
      render: (s) => <PerformanceBadge level={s.performance} />,
    },
  ];

  if (loading) {
    return <Loading message="Loading subject line analysis..." />;
  }

  if (error) {
    return (
      <Card>
        <CardBody>
          <div style={{ textAlign: 'center', padding: '2rem' }}>
            <p>Error loading analysis: {error}</p>
            <button onClick={refetch} style={{ marginTop: '1rem', padding: '0.5rem 1rem' }}>
              Retry
            </button>
          </div>
        </CardBody>
      </Card>
    );
  }

  const performanceCounts = filteredData.reduce((acc, s) => {
    acc[s.performance] = (acc[s.performance] || 0) + 1;
    return acc;
  }, {} as Record<string, number>);

  return (
    <div className="subject-analysis">
      <Card>
        <div style={{ 
          padding: '1rem 1.5rem', 
          borderBottom: '1px solid var(--border-color)',
        }}>
          <h2 style={{ margin: '0 0 1rem 0' }}>Subject Line Analysis</h2>
          
          {/* Feature Performance Cards */}
          <div style={{ 
            display: 'grid', 
            gridTemplateColumns: 'repeat(auto-fit, minmax(180px, 1fr))', 
            gap: '1rem',
            marginBottom: '1rem',
          }}>
            {featureStats.map(feature => (
              <div 
                key={feature.key} 
                className="feature-card"
                style={{
                  borderLeft: `3px solid ${feature.comparison >= 0 ? '#48bb78' : '#f56565'}`,
                }}
              >
                <div style={{ fontWeight: 600, marginBottom: '0.25rem' }}>{feature.name}</div>
                <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)', marginBottom: '0.5rem' }}>
                  {feature.count} of {feature.total} subjects
                </div>
                <div style={{ 
                  fontSize: '1.25rem', 
                  fontWeight: 600,
                  color: feature.comparison >= 0 ? '#22543d' : '#822727',
                }}>
                  {formatComparison(feature.comparison)}
                </div>
                <div style={{ fontSize: '0.625rem', color: 'var(--text-muted)' }}>
                  vs. without {feature.name.toLowerCase()}
                </div>
              </div>
            ))}
          </div>

          {/* Filters */}
          <div style={{ display: 'flex', gap: '0.75rem', flexWrap: 'wrap' }}>
            <select
              value={performanceFilter}
              onChange={(e) => setPerformanceFilter(e.target.value)}
              style={{
                padding: '0.5rem',
                borderRadius: '4px',
                border: '1px solid var(--border-color)',
                backgroundColor: 'var(--card-bg)',
                color: 'var(--text-color)',
              }}
            >
              <option value="all">All Performance</option>
              <option value="high">High ({performanceCounts.high || 0})</option>
              <option value="medium">Medium ({performanceCounts.medium || 0})</option>
              <option value="low">Low ({performanceCounts.low || 0})</option>
            </select>
            <select
              value={featureFilter}
              onChange={(e) => setFeatureFilter(e.target.value)}
              style={{
                padding: '0.5rem',
                borderRadius: '4px',
                border: '1px solid var(--border-color)',
                backgroundColor: 'var(--card-bg)',
                color: 'var(--text-color)',
              }}
            >
              <option value="all">All Features</option>
              {SUBJECT_FEATURES.map(f => (
                <option key={f.key} value={f.key}>Has {f.name}</option>
              ))}
            </select>
            <input
              type="text"
              placeholder="Search subjects..."
              value={searchFilter}
              onChange={(e) => setSearchFilter(e.target.value)}
              style={{
                padding: '0.5rem',
                borderRadius: '4px',
                border: '1px solid var(--border-color)',
                backgroundColor: 'var(--card-bg)',
                color: 'var(--text-color)',
                width: '200px',
              }}
            />
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
        </div>
        <CardBody>
          <SortableTable
            columns={columns}
            data={filteredData}
            keyExtractor={(s, i) => `${s.subject.slice(0, 20)}-${i}`}
            defaultSortKey="avg_open_rate"
            defaultSortDirection="desc"
            emptyMessage="No subject lines match your filters"
            stickyHeader
            maxHeight="500px"
          />

          <div style={{ marginTop: '1rem', fontSize: '0.875rem', color: 'var(--text-muted)' }}>
            Showing {filteredData.length} of {data?.count || 0} subject lines
          </div>
        </CardBody>
      </Card>

      <style>{`
        .feature-card {
          background: var(--card-bg);
          padding: 1rem;
          border-radius: 8px;
          border: 1px solid var(--border-color);
        }
        .feature-tag {
          display: inline-block;
          padding: 0.125rem 0.375rem;
          border-radius: 4px;
          background: var(--tag-bg, #e2e8f0);
          font-size: 0.625rem;
          font-weight: 500;
        }
      `}</style>
    </div>
  );
}

export default SubjectLineAnalysis;
