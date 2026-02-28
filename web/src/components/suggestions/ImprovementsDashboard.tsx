import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { 
  faLightbulb, 
  faClock, 
  faCheckCircle, 
  faTimesCircle, 
  faCopy, 
  faCheck,
  faSync,
  faUser,
  faCalendar,
  faMapMarkerAlt,
  faSpinner,
  faChevronDown,
  faChevronUp,
  faMagic
} from '@fortawesome/free-solid-svg-icons';
import { Suggestion, SuggestionStatus, UpdateSuggestionStatusRequest } from './types';
import { Card, CardBody } from '../common/Card';

export const ImprovementsDashboard: React.FC = () => {
  const [suggestions, setSuggestions] = useState<Suggestion[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [statusFilter, setStatusFilter] = useState<SuggestionStatus | 'all'>('all');
  const [expandedIds, setExpandedIds] = useState<Set<string>>(new Set());
  const [copiedId, setCopiedId] = useState<string | null>(null);
  const [updatingId, setUpdatingId] = useState<string | null>(null);

  const fetchSuggestions = useCallback(async () => {
    try {
      setLoading(true);
      const url = statusFilter === 'all' 
        ? '/api/suggestions/'
        : `/api/suggestions/?status=${statusFilter}`;
      
      const response = await fetch(url, { credentials: 'include' });
      if (!response.ok) throw new Error('Failed to fetch suggestions');
      
      const data = await response.json();
      setSuggestions(data.suggestions || []);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load suggestions');
    } finally {
      setLoading(false);
    }
  }, [statusFilter]);

  useEffect(() => {
    fetchSuggestions();
  }, [fetchSuggestions]);

  const handleCopyRequirements = async (suggestion: Suggestion) => {
    const textToCopy = `## Improvement Request

**Area:** ${suggestion.area}
**Submitted by:** ${suggestion.submitted_by_name} (${suggestion.submitted_by_email})
**Date:** ${new Date(suggestion.created_at).toLocaleString()}

### Original Suggestion
${suggestion.original_suggestion}

### AI-Generated Requirements
${suggestion.requirements || 'No requirements generated'}

---
Suggestion ID: ${suggestion.id}`;

    try {
      await navigator.clipboard.writeText(textToCopy);
      setCopiedId(suggestion.id);
      setTimeout(() => setCopiedId(null), 2000);
    } catch (err) {
      console.error('Failed to copy:', err);
    }
  };

  const handleStatusUpdate = async (
    suggestionId: string, 
    status: 'resolved' | 'denied',
    notes?: string
  ) => {
    setUpdatingId(suggestionId);
    try {
      const request: UpdateSuggestionStatusRequest = {
        status,
        resolution_notes: notes || (status === 'resolved' ? 'Implemented' : 'Not feasible at this time'),
      };

      const response = await fetch(`/api/suggestions/${suggestionId}/status`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify(request),
      });

      if (!response.ok) throw new Error('Failed to update status');
      
      // Refresh the list
      await fetchSuggestions();
    } catch (err) {
      console.error('Failed to update status:', err);
    } finally {
      setUpdatingId(null);
    }
  };

  const handleRegenerateRequirements = async (suggestionId: string) => {
    setUpdatingId(suggestionId);
    try {
      const response = await fetch(`/api/suggestions/${suggestionId}/regenerate`, {
        method: 'POST',
        credentials: 'include',
      });

      if (!response.ok) throw new Error('Failed to regenerate requirements');
      
      await fetchSuggestions();
    } catch (err) {
      console.error('Failed to regenerate:', err);
    } finally {
      setUpdatingId(null);
    }
  };

  const toggleExpanded = (id: string) => {
    setExpandedIds(prev => {
      const next = new Set(prev);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  };

  const getStatusIcon = (status: SuggestionStatus) => {
    switch (status) {
      case 'pending':
        return <FontAwesomeIcon icon={faClock} style={{ color: 'var(--accent-yellow, #facc15)' }} />;
      case 'resolved':
        return <FontAwesomeIcon icon={faCheckCircle} style={{ color: 'var(--accent-green, #22c55e)' }} />;
      case 'denied':
        return <FontAwesomeIcon icon={faTimesCircle} style={{ color: 'var(--accent-red, #ef4444)' }} />;
    }
  };

  const getStatusColor = (status: SuggestionStatus) => {
    switch (status) {
      case 'pending':
        return 'var(--accent-yellow, #facc15)';
      case 'resolved':
        return 'var(--accent-green, #22c55e)';
      case 'denied':
        return 'var(--accent-red, #ef4444)';
    }
  };

  const stats = {
    total: suggestions.length,
    pending: suggestions.filter(s => s.status === 'pending').length,
    resolved: suggestions.filter(s => s.status === 'resolved').length,
    denied: suggestions.filter(s => s.status === 'denied').length,
  };

  if (loading) {
    return (
      <div style={styles.loadingContainer}>
        <FontAwesomeIcon icon={faSpinner} spin style={{ fontSize: '32px' }} />
        <p>Loading suggestions...</p>
      </div>
    );
  }

  return (
    <div style={styles.container}>
      <div style={styles.header}>
        <div style={styles.titleSection}>
          <FontAwesomeIcon icon={faLightbulb} style={{ color: 'var(--accent-yellow, #facc15)', fontSize: '28px' }} />
          <div>
            <h1 style={styles.title}>Improvements</h1>
            <p style={styles.subtitle}>User suggestions and requirements</p>
          </div>
        </div>
        <button onClick={fetchSuggestions} style={styles.refreshButton}>
          <FontAwesomeIcon icon={faSync} />
          Refresh
        </button>
      </div>

      {/* Stats Cards */}
      <div style={styles.statsGrid}>
        <div style={styles.statCard} onClick={() => setStatusFilter('all')}>
          <div style={styles.statValue}>{stats.total}</div>
          <div style={styles.statLabel}>Total</div>
        </div>
        <div style={styles.statCard} onClick={() => setStatusFilter('pending')}>
          <div style={{ ...styles.statValue, color: 'var(--accent-yellow, #facc15)' }}>{stats.pending}</div>
          <div style={styles.statLabel}>Pending</div>
        </div>
        <div style={styles.statCard} onClick={() => setStatusFilter('resolved')}>
          <div style={{ ...styles.statValue, color: 'var(--accent-green, #22c55e)' }}>{stats.resolved}</div>
          <div style={styles.statLabel}>Resolved</div>
        </div>
        <div style={styles.statCard} onClick={() => setStatusFilter('denied')}>
          <div style={{ ...styles.statValue, color: 'var(--accent-red, #ef4444)' }}>{stats.denied}</div>
          <div style={styles.statLabel}>Denied</div>
        </div>
      </div>

      {/* Filter */}
      <div style={styles.filterSection}>
        <span style={styles.filterLabel}>Filter by status:</span>
        <div style={styles.filterButtons}>
          {(['all', 'pending', 'resolved', 'denied'] as const).map(filter => (
            <button
              key={filter}
              onClick={() => setStatusFilter(filter)}
              style={{
                ...styles.filterButton,
                ...(statusFilter === filter ? styles.filterButtonActive : {}),
              }}
            >
              {filter.charAt(0).toUpperCase() + filter.slice(1)}
            </button>
          ))}
        </div>
      </div>

      {error && <div style={styles.error}>{error}</div>}

      {/* Suggestions List */}
      <div style={styles.suggestionsList}>
        {suggestions.length === 0 ? (
          <div style={styles.emptyState}>
            <FontAwesomeIcon icon={faLightbulb} style={{ opacity: 0.3, fontSize: '48px' }} />
            <p>No suggestions found</p>
          </div>
        ) : (
          suggestions.map(suggestion => (
            <Card key={suggestion.id} style={styles.suggestionCard}>
              <div 
                style={styles.suggestionHeader}
                onClick={() => toggleExpanded(suggestion.id)}
              >
                <div style={styles.suggestionMain}>
                  <div style={styles.statusBadge}>
                    {getStatusIcon(suggestion.status)}
                    <span style={{ color: getStatusColor(suggestion.status) }}>
                      {suggestion.status.toUpperCase()}
                    </span>
                  </div>
                  <div style={styles.suggestionPreview}>
                    {suggestion.original_suggestion.slice(0, 100)}
                    {suggestion.original_suggestion.length > 100 ? '...' : ''}
                  </div>
                </div>
                <div style={styles.suggestionMeta}>
                  <div style={styles.metaItem}>
                    <FontAwesomeIcon icon={faMapMarkerAlt} />
                    <span>{suggestion.area}</span>
                  </div>
                  <div style={styles.metaItem}>
                    <FontAwesomeIcon icon={faUser} />
                    <span>{suggestion.submitted_by_name}</span>
                  </div>
                  <div style={styles.metaItem}>
                    <FontAwesomeIcon icon={faCalendar} />
                    <span>{new Date(suggestion.created_at).toLocaleDateString()}</span>
                  </div>
                </div>
                <button style={styles.expandButton}>
                  {expandedIds.has(suggestion.id) ? <FontAwesomeIcon icon={faChevronUp} /> : <FontAwesomeIcon icon={faChevronDown} />}
                </button>
              </div>

              {expandedIds.has(suggestion.id) && (
                <CardBody>
                  <div style={styles.expandedContent}>
                    <div style={styles.section}>
                      <h4 style={styles.sectionTitle}>Original Suggestion</h4>
                      <p style={styles.sectionContent}>{suggestion.original_suggestion}</p>
                    </div>

                    {suggestion.area_context && (
                      <div style={styles.section}>
                        <h4 style={styles.sectionTitle}>Area Context</h4>
                        <p style={styles.sectionContent}>{suggestion.area_context}</p>
                      </div>
                    )}

                    {suggestion.requirements && (
                      <div style={styles.section}>
                        <h4 style={styles.sectionTitle}>
                          <FontAwesomeIcon icon={faMagic} />
                          AI-Generated Requirements
                        </h4>
                        <div style={styles.requirementsBox}>
                          <pre style={styles.requirements}>{suggestion.requirements}</pre>
                        </div>
                      </div>
                    )}

                    {suggestion.resolution_notes && (
                      <div style={styles.section}>
                        <h4 style={styles.sectionTitle}>Resolution Notes</h4>
                        <p style={styles.sectionContent}>{suggestion.resolution_notes}</p>
                      </div>
                    )}

                    <div style={styles.actionBar}>
                      <button
                        onClick={() => handleCopyRequirements(suggestion)}
                        style={styles.actionButton}
                      >
                        {copiedId === suggestion.id ? (
                          <>
                            <FontAwesomeIcon icon={faCheck} />
                            Copied!
                          </>
                        ) : (
                          <>
                            <FontAwesomeIcon icon={faCopy} />
                            Copy for Terminal
                          </>
                        )}
                      </button>

                      {suggestion.status === 'pending' && (
                        <>
                          <button
                            onClick={() => handleRegenerateRequirements(suggestion.id)}
                            disabled={updatingId === suggestion.id}
                            style={styles.actionButton}
                          >
                            {updatingId === suggestion.id ? (
                              <FontAwesomeIcon icon={faSpinner} spin />
                            ) : (
                              <FontAwesomeIcon icon={faSync} />
                            )}
                            Regenerate
                          </button>

                          <button
                            onClick={() => handleStatusUpdate(suggestion.id, 'resolved')}
                            disabled={updatingId === suggestion.id}
                            style={{ ...styles.actionButton, ...styles.resolveButton }}
                          >
                            <FontAwesomeIcon icon={faCheckCircle} />
                            Mark Resolved
                          </button>

                          <button
                            onClick={() => handleStatusUpdate(suggestion.id, 'denied')}
                            disabled={updatingId === suggestion.id}
                            style={{ ...styles.actionButton, ...styles.denyButton }}
                          >
                            <FontAwesomeIcon icon={faTimesCircle} />
                            Deny
                          </button>
                        </>
                      )}
                    </div>
                  </div>
                </CardBody>
              )}
            </Card>
          ))
        )}
      </div>
    </div>
  );
};

const styles: Record<string, React.CSSProperties> = {
  container: {
    padding: '1.5rem',
    maxWidth: '1200px',
    margin: '0 auto',
  },
  loadingContainer: {
    display: 'flex',
    flexDirection: 'column' as const,
    alignItems: 'center',
    justifyContent: 'center',
    padding: '4rem',
    color: 'var(--text-muted, #888)',
    gap: '1rem',
  },
  header: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    marginBottom: '1.5rem',
  },
  titleSection: {
    display: 'flex',
    alignItems: 'center',
    gap: '1rem',
  },
  title: {
    margin: 0,
    fontSize: '1.5rem',
    fontWeight: 600,
    color: 'var(--text-primary, #fff)',
  },
  subtitle: {
    margin: 0,
    fontSize: '0.875rem',
    color: 'var(--text-muted, #888)',
  },
  refreshButton: {
    display: 'flex',
    alignItems: 'center',
    gap: '0.5rem',
    padding: '0.5rem 1rem',
    fontSize: '0.875rem',
    backgroundColor: 'var(--bg-tertiary, #2a2a3e)',
    color: 'var(--text-muted, #888)',
    border: '1px solid var(--border-color, #333)',
    borderRadius: '6px',
    cursor: 'pointer',
  },
  statsGrid: {
    display: 'grid',
    gridTemplateColumns: 'repeat(4, 1fr)',
    gap: '1rem',
    marginBottom: '1.5rem',
  },
  statCard: {
    backgroundColor: 'var(--bg-secondary, #1e1e2e)',
    border: '1px solid var(--border-color, #333)',
    borderRadius: '12px',
    padding: '1.25rem',
    textAlign: 'center' as const,
    cursor: 'pointer',
    transition: 'border-color 0.2s',
  },
  statValue: {
    fontSize: '2rem',
    fontWeight: 700,
    color: 'var(--text-primary, #fff)',
  },
  statLabel: {
    fontSize: '0.75rem',
    color: 'var(--text-muted, #888)',
    textTransform: 'uppercase' as const,
    letterSpacing: '0.05em',
  },
  filterSection: {
    display: 'flex',
    alignItems: 'center',
    gap: '1rem',
    marginBottom: '1.5rem',
  },
  filterLabel: {
    fontSize: '0.875rem',
    color: 'var(--text-muted, #888)',
  },
  filterButtons: {
    display: 'flex',
    gap: '0.5rem',
  },
  filterButton: {
    padding: '0.5rem 1rem',
    fontSize: '0.8125rem',
    backgroundColor: 'var(--bg-tertiary, #2a2a3e)',
    color: 'var(--text-muted, #888)',
    border: '1px solid var(--border-color, #333)',
    borderRadius: '6px',
    cursor: 'pointer',
  },
  filterButtonActive: {
    backgroundColor: 'var(--accent-blue, #3b82f6)',
    color: '#fff',
    borderColor: 'var(--accent-blue, #3b82f6)',
  },
  error: {
    padding: '1rem',
    backgroundColor: 'rgba(239, 68, 68, 0.1)',
    border: '1px solid var(--accent-red, #ef4444)',
    borderRadius: '8px',
    color: 'var(--accent-red, #ef4444)',
    marginBottom: '1rem',
  },
  suggestionsList: {
    display: 'flex',
    flexDirection: 'column' as const,
    gap: '1rem',
  },
  emptyState: {
    display: 'flex',
    flexDirection: 'column' as const,
    alignItems: 'center',
    justifyContent: 'center',
    padding: '4rem',
    color: 'var(--text-muted, #888)',
    gap: '1rem',
  },
  suggestionCard: {
    overflow: 'hidden',
  },
  suggestionHeader: {
    display: 'flex',
    alignItems: 'center',
    padding: '1rem 1.25rem',
    cursor: 'pointer',
    gap: '1rem',
  },
  suggestionMain: {
    flex: 1,
    minWidth: 0,
  },
  statusBadge: {
    display: 'inline-flex',
    alignItems: 'center',
    gap: '0.375rem',
    fontSize: '0.6875rem',
    fontWeight: 600,
    letterSpacing: '0.05em',
    marginBottom: '0.5rem',
  },
  suggestionPreview: {
    fontSize: '0.9375rem',
    color: 'var(--text-primary, #fff)',
    lineHeight: 1.4,
  },
  suggestionMeta: {
    display: 'flex',
    flexDirection: 'column' as const,
    gap: '0.375rem',
    alignItems: 'flex-end',
  },
  metaItem: {
    display: 'flex',
    alignItems: 'center',
    gap: '0.375rem',
    fontSize: '0.75rem',
    color: 'var(--text-muted, #888)',
  },
  expandButton: {
    background: 'none',
    border: 'none',
    color: 'var(--text-muted, #888)',
    cursor: 'pointer',
    padding: '0.25rem',
  },
  expandedContent: {
    display: 'flex',
    flexDirection: 'column' as const,
    gap: '1.25rem',
  },
  section: {
    display: 'flex',
    flexDirection: 'column' as const,
    gap: '0.5rem',
  },
  sectionTitle: {
    display: 'flex',
    alignItems: 'center',
    gap: '0.5rem',
    margin: 0,
    fontSize: '0.8125rem',
    fontWeight: 600,
    color: 'var(--text-muted, #888)',
    textTransform: 'uppercase' as const,
    letterSpacing: '0.05em',
  },
  sectionContent: {
    margin: 0,
    fontSize: '0.9375rem',
    color: 'var(--text-primary, #fff)',
    lineHeight: 1.6,
  },
  requirementsBox: {
    backgroundColor: 'var(--bg-tertiary, #2a2a3e)',
    border: '1px solid var(--border-color, #333)',
    borderRadius: '8px',
    padding: '1rem',
    overflow: 'auto',
  },
  requirements: {
    margin: 0,
    fontSize: '0.8125rem',
    fontFamily: 'inherit',
    color: 'var(--text-primary, #fff)',
    whiteSpace: 'pre-wrap' as const,
    lineHeight: 1.6,
  },
  actionBar: {
    display: 'flex',
    flexWrap: 'wrap' as const,
    gap: '0.75rem',
    paddingTop: '1rem',
    borderTop: '1px solid var(--border-color, #333)',
  },
  actionButton: {
    display: 'flex',
    alignItems: 'center',
    gap: '0.5rem',
    padding: '0.5rem 1rem',
    fontSize: '0.8125rem',
    backgroundColor: 'var(--bg-tertiary, #2a2a3e)',
    color: 'var(--text-primary, #fff)',
    border: '1px solid var(--border-color, #333)',
    borderRadius: '6px',
    cursor: 'pointer',
  },
  resolveButton: {
    backgroundColor: 'rgba(34, 197, 94, 0.1)',
    borderColor: 'var(--accent-green, #22c55e)',
    color: 'var(--accent-green, #22c55e)',
  },
  denyButton: {
    backgroundColor: 'rgba(239, 68, 68, 0.1)',
    borderColor: 'var(--accent-red, #ef4444)',
    color: 'var(--accent-red, #ef4444)',
  },
};

export default ImprovementsDashboard;
