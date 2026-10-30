import React, { useState, useEffect, useCallback, useRef } from 'react';
import { useAuth } from '../../../contexts/AuthContext';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faLink,
  faSpinner,
  faCheck,
  faClock,
  faExclamationTriangle,
  faPlus,
  faSync,
  faShieldAlt,
  faGlobe,
  faCheckCircle,
} from '@fortawesome/free-solid-svg-icons';

const API_BASE = '/api/mailing';

// ---- Interfaces ----

interface TrackingDomainSuggestion {
  sending_domain: string;
  suggested_tracking_domain: string;
  profile_name: string;
  status: 'not_provisioned' | 'pending' | 'provisioning' | 'active' | 'failed';
  domain_id: string | null;
  ssl_status: string;
  cloudfront_domain: string;
  verified: boolean;
  ssl_provisioned: boolean;
}

interface TrackingDomain {
  id: string;
  org_id: string;
  domain: string;
  verified: boolean;
  ssl_provisioned: boolean;
  ssl_status: string;
  cloudfront_id: string;
  cloudfront_domain: string;
  acm_cert_arn: string;
  origin_server: string;
  dns_records: DNSRecord[];
  created_at: string;
  updated_at: string;
}

interface DNSRecord {
  type: string;
  name: string;
  value: string;
  status: string;
}

interface AWSStatus {
  domain: string;
  ssl_status: string;
  ssl_provisioned: boolean;
  cloudfront_id: string;
  cloudfront_domain: string;
  acm_cert_arn: string;
  verified: boolean;
  acm_status?: string;
  cloudfront_status?: string;
}

// ---- Helpers ----

async function orgFetch(url: string, orgId: string, opts?: RequestInit) {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    'X-Organization-ID': orgId,
    ...(opts?.headers as Record<string, string> || {}),
  };
  return fetch(url, { ...opts, headers });
}

const statusConfig: Record<string, { label: string; color: string; bg: string; icon: any }> = {
  not_provisioned: { label: 'Not Provisioned', color: 'var(--j-text-muted)', bg: 'rgba(136,136,136,0.15)', icon: faClock },
  pending: { label: 'Pending', color: 'var(--j-warn)', bg: 'color-mix(in srgb, var(--j-warn) 15%, transparent)', icon: faClock },
  provisioning: { label: 'Provisioning', color: 'var(--j-blue)', bg: 'color-mix(in srgb, var(--j-blue) 15%, transparent)', icon: faSpinner },
  active: { label: 'Active', color: 'var(--j-success)', bg: 'color-mix(in srgb, var(--j-success) 15%, transparent)', icon: faCheck },
  failed: { label: 'Failed', color: 'var(--j-danger)', bg: 'color-mix(in srgb, var(--j-danger) 15%, transparent)', icon: faExclamationTriangle },
};

// ---- Component ----

export const TrackingDomainManager: React.FC = () => {
  const { organization } = useAuth();
  const orgId = organization?.id || '';

  const [suggestions, setSuggestions] = useState<TrackingDomainSuggestion[]>([]);
  const [existingDomains, setExistingDomains] = useState<TrackingDomain[]>([]);
  const [loading, setLoading] = useState(false);
  const [provisioning, setProvisioning] = useState<Record<string, boolean>>({});
  const [verifying, setVerifying] = useState<Record<string, boolean>>({});
  const [error, setError] = useState<string | null>(null);
  const [customDomain, setCustomDomain] = useState('');
  const [addingCustom, setAddingCustom] = useState(false);
  const [activeTrackingURL, setActiveTrackingURL] = useState<string | null>(null);

  const pollTimersRef = useRef<Record<string, ReturnType<typeof setInterval>>>({});

  // Cleanup timers on unmount
  useEffect(() => {
    return () => {
      Object.values(pollTimersRef.current).forEach(clearInterval);
    };
  }, []);

  // ---- Data Fetching ----

  const fetchSuggestions = useCallback(async () => {
    if (!orgId) return;
    try {
      const res = await orgFetch(`${API_BASE}/tracking-domains/suggestions`, orgId);
      if (res.ok) {
        const data = await res.json();
        setSuggestions(data.suggestions || []);
      }
    } catch (err: any) {
      console.error('Failed to fetch tracking domain suggestions:', err);
    }
  }, [orgId]);

  const fetchExistingDomains = useCallback(async () => {
    if (!orgId) return;
    try {
      const res = await orgFetch(`${API_BASE}/tracking-domains`, orgId);
      if (res.ok) {
        const data = await res.json();
        setExistingDomains(Array.isArray(data) ? data : data.domains || []);
      }
    } catch (err: any) {
      console.error('Failed to fetch existing tracking domains:', err);
    }
  }, [orgId]);

  const fetchActiveTrackingURL = useCallback(async () => {
    if (!orgId) return;
    try {
      const res = await orgFetch(`${API_BASE}/tracking-domains/active-url`, orgId);
      if (res.ok) {
        const data = await res.json();
        setActiveTrackingURL(data.tracking_url || null);
      }
    } catch (err: any) {
      console.error('Failed to fetch active tracking URL:', err);
    }
  }, [orgId]);

  const loadAll = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      await Promise.all([fetchSuggestions(), fetchExistingDomains(), fetchActiveTrackingURL()]);
    } catch (err: any) {
      setError(err.message || 'Failed to load data');
    } finally {
      setLoading(false);
    }
  }, [fetchSuggestions, fetchExistingDomains, fetchActiveTrackingURL]);

  useEffect(() => {
    if (orgId) loadAll();
  }, [orgId, loadAll]);

  // ---- Provision Domain ----

  const pollAWSStatus = useCallback((domainId: string, suggestedDomain: string) => {
    if (pollTimersRef.current[suggestedDomain]) {
      clearInterval(pollTimersRef.current[suggestedDomain]);
    }

    const timer = setInterval(async () => {
      try {
        const res = await orgFetch(`${API_BASE}/tracking-domains/${domainId}/aws-status`, orgId);
        if (res.ok) {
          const status: AWSStatus = await res.json();

          if (status.ssl_status === 'active' || status.ssl_status === 'failed') {
            clearInterval(pollTimersRef.current[suggestedDomain]);
            delete pollTimersRef.current[suggestedDomain];
            setProvisioning(prev => ({ ...prev, [suggestedDomain]: false }));
            await fetchSuggestions();
            await fetchExistingDomains();
            await fetchActiveTrackingURL();
          }
        }
      } catch (err) {
        console.error('Poll AWS status error:', err);
      }
    }, 5000);

    pollTimersRef.current[suggestedDomain] = timer;
  }, [orgId, fetchSuggestions, fetchExistingDomains, fetchActiveTrackingURL]);

  const provisionDomain = useCallback(async (domain: string) => {
    if (!orgId) return;
    setProvisioning(prev => ({ ...prev, [domain]: true }));
    setError(null);

    try {
      // Step 1: Register the domain
      const regRes = await orgFetch(`${API_BASE}/tracking-domains`, orgId, {
        method: 'POST',
        body: JSON.stringify({ domain }),
      });

      if (!regRes.ok) {
        const errData = await regRes.json().catch(() => ({}));
        throw new Error(errData.error || `Registration failed (${regRes.status})`);
      }

      const regData = await regRes.json();
      const domainId = regData.id || regData.domain?.id;

      if (domainId) {
        // Step 2: Trigger AWS provisioning
        const provRes = await orgFetch(`${API_BASE}/aws/tracking-domains/${domainId}/provision`, orgId, {
          method: 'POST',
          body: JSON.stringify({ origin_server: 'projectjarvis.io' }),
        });

        if (!provRes.ok) {
          console.warn('AWS provisioning request failed, domain registered without SSL');
        }

        // Start polling
        pollAWSStatus(domainId, domain);
      }

      await fetchSuggestions();
      await fetchExistingDomains();
    } catch (err: any) {
      setError(err.message || 'Failed to provision domain');
      setProvisioning(prev => ({ ...prev, [domain]: false }));
    }
  }, [orgId, pollAWSStatus, fetchSuggestions, fetchExistingDomains]);

  // ---- Verify DNS ----

  const verifyDomain = useCallback(async (domainId: string) => {
    if (!orgId) return;
    setVerifying(prev => ({ ...prev, [domainId]: true }));

    try {
      const res = await orgFetch(`${API_BASE}/tracking-domains/${domainId}/verify`, orgId, {
        method: 'POST',
      });

      if (!res.ok) {
        const errData = await res.json().catch(() => ({}));
        throw new Error(errData.error || 'Verification failed');
      }

      await loadAll();
    } catch (err: any) {
      setError(err.message || 'Failed to verify domain');
    } finally {
      setVerifying(prev => ({ ...prev, [domainId]: false }));
    }
  }, [orgId, loadAll]);

  // ---- Refresh AWS Status ----

  const refreshAWSStatus = useCallback(async (domainId: string) => {
    if (!orgId) return;
    try {
      await orgFetch(`${API_BASE}/tracking-domains/${domainId}/refresh-aws-status`, orgId, {
        method: 'POST',
      });
      await loadAll();
    } catch (err: any) {
      console.error('Failed to refresh AWS status:', err);
    }
  }, [orgId, loadAll]);

  // ---- Add Custom Domain ----

  const handleAddCustomDomain = useCallback(async () => {
    if (!orgId || !customDomain.trim()) return;
    const domain = customDomain.trim().toLowerCase();
    if (!domain.includes('.')) {
      setError('Please enter a valid domain (e.g., track.example.com)');
      return;
    }

    setAddingCustom(true);
    setError(null);

    try {
      await provisionDomain(domain);
      setCustomDomain('');
    } catch (err: any) {
      setError(err.message || 'Failed to add custom domain');
    } finally {
      setAddingCustom(false);
    }
  }, [orgId, customDomain, provisionDomain]);

  // ---- Render Helpers ----

  const getStatusBadge = (status: string) => {
    const cfg = statusConfig[status] || statusConfig.pending;
    return (
      <span
        style={{
          display: 'inline-flex',
          alignItems: 'center',
          gap: '6px',
          padding: '4px 12px',
          borderRadius: '20px',
          fontSize: '11px',
          fontWeight: 600,
          color: cfg.color,
          background: cfg.bg,
          textTransform: 'uppercase',
          letterSpacing: '0.5px',
        }}
      >
        <FontAwesomeIcon icon={cfg.icon} spin={status === 'provisioning'} />
        {cfg.label}
      </span>
    );
  };

  const formatDate = (dateStr: string) => {
    if (!dateStr) return '-';
    const d = new Date(dateStr);
    return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' });
  };

  // ---- Main Render ----

  return (
    <div style={{ padding: '0', maxWidth: '960px', color: 'var(--j-text)' }}>
      {/* Header */}
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '20px' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
          <FontAwesomeIcon icon={faLink} style={{ fontSize: '22px', color: 'var(--j-violet)' }} />
          <h2 style={{ margin: 0, fontSize: '20px', fontWeight: 700, color: 'var(--j-text)' }}>Tracking Domains</h2>
        </div>
        <button
          onClick={loadAll}
          disabled={loading}
          style={{
            background: 'color-mix(in srgb, var(--j-text) 6%, transparent)',
            color: 'var(--j-text)',
            border: '1px solid color-mix(in srgb, var(--j-text) 10%, transparent)',
            borderRadius: '6px',
            padding: '8px 14px',
            cursor: 'pointer',
            display: 'flex',
            alignItems: 'center',
            gap: '6px',
            fontSize: '12px',
          }}
        >
          <FontAwesomeIcon icon={faSync} spin={loading} />
          Refresh
        </button>
      </div>

      {/* Active Tracking URL Banner */}
      {activeTrackingURL && (
        <div
          style={{
            background: activeTrackingURL.includes('si3p4trk') ? 'color-mix(in srgb, var(--j-warn) 10%, transparent)' : 'color-mix(in srgb, var(--j-success) 10%, transparent)',
            border: `1px solid ${activeTrackingURL.includes('si3p4trk') ? 'color-mix(in srgb, var(--j-warn) 30%, transparent)' : 'color-mix(in srgb, var(--j-success) 30%, transparent)'}`,
            borderRadius: '8px',
            padding: '12px 16px',
            marginBottom: '20px',
            display: 'flex',
            alignItems: 'center',
            gap: '10px',
            fontSize: '13px',
          }}
        >
          <FontAwesomeIcon
            icon={activeTrackingURL.includes('si3p4trk') ? faExclamationTriangle : faCheckCircle}
            style={{ color: activeTrackingURL.includes('si3p4trk') ? 'var(--j-warn)' : 'var(--j-success)' }}
          />
          <div>
            <strong>Active Tracking Domain:</strong>{' '}
            <code style={{ background: 'color-mix(in srgb, var(--j-text) 6%, transparent)', padding: '2px 6px', borderRadius: '4px', fontSize: '12px' }}>
              {activeTrackingURL}
            </code>
            {activeTrackingURL.includes('si3p4trk') && (
              <span style={{ color: 'var(--j-warn)', marginLeft: '8px', fontSize: '12px' }}>
                (Default tracking domain - set up a branded domain below)
              </span>
            )}
          </div>
        </div>
      )}

      {/* Error Banner */}
      {error && (
        <div
          style={{
            background: 'color-mix(in srgb, var(--j-danger) 12%, transparent)',
            border: '1px solid color-mix(in srgb, var(--j-danger) 30%, transparent)',
            borderRadius: '8px',
            padding: '12px 16px',
            marginBottom: '20px',
            color: 'var(--j-danger)',
            fontSize: '13px',
            display: 'flex',
            alignItems: 'center',
            gap: '10px',
          }}
        >
          <FontAwesomeIcon icon={faExclamationTriangle} />
          {error}
          <button
            onClick={() => setError(null)}
            style={{ marginLeft: 'auto', background: 'none', border: 'none', color: 'var(--j-danger)', cursor: 'pointer', fontSize: '16px' }}
          >
            &times;
          </button>
        </div>
      )}

      {/* Info Banner */}
      <div
        style={{
          background: 'rgba(162,155,254,0.08)',
          border: '1px solid rgba(162,155,254,0.2)',
          borderRadius: '8px',
          padding: '14px 18px',
          marginBottom: '24px',
          display: 'flex',
          alignItems: 'flex-start',
          gap: '12px',
          fontSize: '13px',
          lineHeight: '1.5',
        }}
      >
        <FontAwesomeIcon icon={faShieldAlt} style={{ color: 'var(--j-violet)', fontSize: '16px', marginTop: '2px' }} />
        <div>
          <strong style={{ color: 'var(--j-violet)' }}>Branded Tracking Domains</strong> (e.g.,{' '}
          <code style={{ background: 'color-mix(in srgb, var(--j-text) 6%, transparent)', padding: '2px 6px', borderRadius: '4px', fontSize: '12px' }}>
            track.yoursendingdomain.com
          </code>
          ) replace the default tracking domain in email links, improving brand consistency and inbox deliverability.
          Once active, all new tracking links will automatically use the branded domain.
        </div>
      </div>

      {/* Suggestions Section */}
      <div
        style={{
          background: 'var(--j-surface-2)',
          borderRadius: '10px',
          overflow: 'hidden',
          marginBottom: '24px',
          border: '1px solid color-mix(in srgb, var(--j-text) 6%, transparent)',
        }}
      >
        <div
          style={{
            background: 'var(--j-surface-2)',
            padding: '14px 20px',
            borderBottom: '1px solid color-mix(in srgb, var(--j-text) 6%, transparent)',
            display: 'flex',
            alignItems: 'center',
            gap: '10px',
          }}
        >
          <FontAwesomeIcon icon={faGlobe} style={{ color: 'var(--j-violet)' }} />
          <h3 style={{ margin: 0, fontSize: '15px', fontWeight: 600 }}>
            Domain Suggestions from Sending Profiles
          </h3>
        </div>

        <div style={{ padding: '16px 20px' }}>
          {loading && suggestions.length === 0 ? (
            <div style={{ textAlign: 'center', padding: '30px', color: 'var(--j-text-muted)' }}>
              <FontAwesomeIcon icon={faSpinner} spin style={{ fontSize: '20px', marginBottom: '10px' }} />
              <div>Loading suggestions...</div>
            </div>
          ) : suggestions.length === 0 ? (
            <div style={{ textAlign: 'center', padding: '30px', color: 'var(--j-text-muted)', fontSize: '14px' }}>
              No sending profiles found with configured domains. Add sending profiles to see tracking domain suggestions.
            </div>
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column', gap: '12px' }}>
              {suggestions.map((s, idx) => {
                const isProvisioning = provisioning[s.suggested_tracking_domain];

                return (
                  <div
                    key={idx}
                    style={{
                      background: 'color-mix(in srgb, var(--j-text) 3%, transparent)',
                      border: '1px solid color-mix(in srgb, var(--j-text) 6%, transparent)',
                      borderRadius: '8px',
                      padding: '16px',
                    }}
                  >
                    {/* Row: info + action */}
                    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: '12px' }}>
                      <div style={{ flex: 1, minWidth: '250px' }}>
                        <div style={{ fontSize: '11px', color: 'var(--j-text-muted)', textTransform: 'uppercase', letterSpacing: '0.5px', marginBottom: '4px' }}>
                          {s.profile_name}
                        </div>
                        <div style={{ display: 'flex', alignItems: 'center', gap: '8px', flexWrap: 'wrap' }}>
                          <span style={{ fontSize: '13px', color: 'var(--j-text-2)' }}>{s.sending_domain}</span>
                          <span style={{ color: 'var(--j-text-faint)' }}>&rarr;</span>
                          <span style={{ fontSize: '14px', fontWeight: 600, color: 'var(--j-text)', fontFamily: 'monospace' }}>
                            {s.suggested_tracking_domain}
                          </span>
                        </div>
                        {s.cloudfront_domain && (
                          <div style={{ fontSize: '11px', color: 'var(--j-violet)', marginTop: '4px' }}>
                            CDN domain: {s.cloudfront_domain}
                          </div>
                        )}
                      </div>

                      <div style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
                        {getStatusBadge(s.status)}
                        {s.status === 'not_provisioned' && (
                          <button
                            onClick={() => provisionDomain(s.suggested_tracking_domain)}
                            disabled={isProvisioning}
                            style={{
                              background: 'var(--j-surface-3)',
                              color: 'var(--j-text)',
                              border: 'none',
                              borderRadius: '6px',
                              padding: '8px 16px',
                              cursor: isProvisioning ? 'not-allowed' : 'pointer',
                              display: 'flex',
                              alignItems: 'center',
                              gap: '6px',
                              fontSize: '12px',
                              fontWeight: 600,
                              opacity: isProvisioning ? 0.6 : 1,
                              transition: 'opacity 0.2s',
                            }}
                          >
                            {isProvisioning ? (
                              <>
                                <FontAwesomeIcon icon={faSpinner} spin /> Provisioning...
                              </>
                            ) : (
                              <>
                                <FontAwesomeIcon icon={faShieldAlt} /> Provision
                              </>
                            )}
                          </button>
                        )}
                        {s.status === 'failed' && (
                          <button
                            onClick={() => provisionDomain(s.suggested_tracking_domain)}
                            disabled={isProvisioning}
                            style={{
                              background: 'var(--j-danger)',
                              color: 'var(--j-text)',
                              border: 'none',
                              borderRadius: '6px',
                              padding: '8px 16px',
                              cursor: 'pointer',
                              display: 'flex',
                              alignItems: 'center',
                              gap: '6px',
                              fontSize: '12px',
                              fontWeight: 600,
                            }}
                          >
                            <FontAwesomeIcon icon={faSync} /> Retry
                          </button>
                        )}
                      </div>
                    </div>

                    {/* Active summary */}
                    {s.status === 'active' && (
                      <div
                        style={{
                          marginTop: '10px',
                          paddingTop: '10px',
                          borderTop: '1px solid color-mix(in srgb, var(--j-text) 6%, transparent)',
                          display: 'flex',
                          gap: '16px',
                          flexWrap: 'wrap',
                          fontSize: '12px',
                        }}
                      >
                        <span style={{ color: 'var(--j-success)' }}>
                          <FontAwesomeIcon icon={faCheck} style={{ marginRight: '4px' }} />
                          SSL: {s.ssl_status || 'Active'}
                        </span>
                        {s.verified && (
                          <span style={{ color: 'var(--j-success)' }}>
                            <FontAwesomeIcon icon={faCheck} style={{ marginRight: '4px' }} />
                            DNS Verified
                          </span>
                        )}
                        <span style={{ color: 'var(--j-violet)' }}>
                          <FontAwesomeIcon icon={faLink} style={{ marginRight: '4px' }} />
                          Auto-applied to new tracking links
                        </span>
                      </div>
                    )}
                  </div>
                );
              })}
            </div>
          )}
        </div>
      </div>

      {/* Existing Domains Section */}
      <div
        style={{
          background: 'var(--j-surface-2)',
          borderRadius: '10px',
          overflow: 'hidden',
          marginBottom: '24px',
          border: '1px solid color-mix(in srgb, var(--j-text) 6%, transparent)',
        }}
      >
        <div
          style={{
            background: 'var(--j-surface-2)',
            padding: '14px 20px',
            borderBottom: '1px solid color-mix(in srgb, var(--j-text) 6%, transparent)',
            display: 'flex',
            alignItems: 'center',
            gap: '10px',
          }}
        >
          <FontAwesomeIcon icon={faGlobe} style={{ color: 'var(--j-success)' }} />
          <h3 style={{ margin: 0, fontSize: '15px', fontWeight: 600 }}>Registered Tracking Domains</h3>
        </div>

        <div style={{ padding: '16px 20px' }}>
          {existingDomains.length === 0 ? (
            <div style={{ textAlign: 'center', padding: '24px', color: 'var(--j-text-muted)', fontSize: '14px' }}>
              No tracking domains registered yet. Use the suggestions above or add a custom domain below.
            </div>
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column', gap: '12px' }}>
              {existingDomains.map((d) => (
                <div
                  key={d.id}
                  style={{
                    background: 'color-mix(in srgb, var(--j-text) 3%, transparent)',
                    border: '1px solid color-mix(in srgb, var(--j-text) 6%, transparent)',
                    borderRadius: '8px',
                    padding: '16px',
                  }}
                >
                  <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: '12px' }}>
                    <div>
                      <div style={{ fontSize: '14px', fontWeight: 600, fontFamily: 'monospace' }}>{d.domain}</div>
                      {d.cloudfront_domain && (
                        <div style={{ fontSize: '11px', color: 'var(--j-violet)', marginTop: '4px' }}>
                          CDN domain: {d.cloudfront_domain}
                        </div>
                      )}
                      <div style={{ fontSize: '11px', color: 'var(--j-text-muted)', marginTop: '4px' }}>
                        Created: {formatDate(d.created_at)}
                      </div>
                    </div>

                    <div style={{ display: 'flex', alignItems: 'center', gap: '10px' }}>
                      {getStatusBadge(d.ssl_status || 'pending')}
                      {d.verified ? (
                        <span style={{ color: 'var(--j-success)', fontSize: '12px', display: 'flex', alignItems: 'center', gap: '4px' }}>
                          <FontAwesomeIcon icon={faCheck} /> Verified
                        </span>
                      ) : (
                        <button
                          onClick={() => verifyDomain(d.id)}
                          disabled={verifying[d.id]}
                          style={{
                            background: 'color-mix(in srgb, var(--j-warn) 15%, transparent)',
                            color: 'var(--j-warn)',
                            border: '1px solid color-mix(in srgb, var(--j-warn) 30%, transparent)',
                            borderRadius: '6px',
                            padding: '6px 12px',
                            cursor: verifying[d.id] ? 'not-allowed' : 'pointer',
                            fontSize: '11px',
                            fontWeight: 600,
                            display: 'flex',
                            alignItems: 'center',
                            gap: '4px',
                          }}
                        >
                          {verifying[d.id] ? (
                            <><FontAwesomeIcon icon={faSpinner} spin /> Verifying...</>
                          ) : (
                            <><FontAwesomeIcon icon={faCheckCircle} /> Verify DNS</>
                          )}
                        </button>
                      )}
                      <button
                        onClick={() => refreshAWSStatus(d.id)}
                        style={{
                          background: 'color-mix(in srgb, var(--j-text) 6%, transparent)',
                          color: 'var(--j-text)',
                          border: '1px solid color-mix(in srgb, var(--j-text) 8%, transparent)',
                          borderRadius: '6px',
                          padding: '6px 10px',
                          cursor: 'pointer',
                          fontSize: '11px',
                        }}
                        title="Refresh AWS Status"
                      >
                        <FontAwesomeIcon icon={faSync} />
                      </button>
                    </div>
                  </div>

                  {/* DNS Records */}
                  {d.dns_records && d.dns_records.length > 0 && !d.verified && (
                    <div style={{ marginTop: '14px', paddingTop: '14px', borderTop: '1px solid color-mix(in srgb, var(--j-text) 6%, transparent)' }}>
                      <div style={{ fontSize: '11px', color: 'var(--j-text-muted)', textTransform: 'uppercase', letterSpacing: '0.5px', marginBottom: '8px' }}>
                        Required DNS Records
                      </div>
                      <div style={{ display: 'flex', flexDirection: 'column', gap: '6px' }}>
                        {d.dns_records.map((rec, idx) => (
                          <div key={idx} style={{ display: 'flex', alignItems: 'center', gap: '10px', fontSize: '12px' }}>
                            <span style={{ width: '16px', textAlign: 'center' }}>
                              {rec.status === 'verified' ? (
                                <FontAwesomeIcon icon={faCheck} style={{ color: 'var(--j-success)' }} />
                              ) : (
                                <FontAwesomeIcon icon={faClock} style={{ color: 'var(--j-text-muted)' }} />
                              )}
                            </span>
                            <span style={{
                              background: 'color-mix(in srgb, var(--j-text) 6%, transparent)',
                              padding: '2px 8px',
                              borderRadius: '4px',
                              fontSize: '11px',
                              fontWeight: 600,
                              color: 'var(--j-blue)',
                              minWidth: '50px',
                              textAlign: 'center',
                            }}>
                              {rec.type}
                            </span>
                            <span style={{ color: 'var(--j-text-2)', fontFamily: 'monospace', fontSize: '11px' }}>{rec.name}</span>
                            <span style={{ color: 'var(--j-text-faint)' }}>&rarr;</span>
                            <span style={{ color: 'var(--j-text)', fontFamily: 'monospace', fontSize: '11px', wordBreak: 'break-all' }}>
                              {rec.value}
                            </span>
                          </div>
                        ))}
                      </div>
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}
        </div>
      </div>

      {/* Manual Add Domain */}
      <div
        style={{
          background: 'var(--j-surface-2)',
          borderRadius: '10px',
          overflow: 'hidden',
          border: '1px solid color-mix(in srgb, var(--j-text) 6%, transparent)',
        }}
      >
        <div
          style={{
            background: 'var(--j-surface-2)',
            padding: '14px 20px',
            borderBottom: '1px solid color-mix(in srgb, var(--j-text) 6%, transparent)',
            display: 'flex',
            alignItems: 'center',
            gap: '10px',
          }}
        >
          <FontAwesomeIcon icon={faPlus} style={{ color: 'var(--j-warn)' }} />
          <h3 style={{ margin: 0, fontSize: '15px', fontWeight: 600 }}>Add Custom Tracking Domain</h3>
        </div>

        <div style={{ padding: '16px 20px' }}>
          <div style={{ display: 'flex', gap: '12px', alignItems: 'center' }}>
            <input
              type="text"
              value={customDomain}
              onChange={(e) => setCustomDomain(e.target.value)}
              placeholder="track.yourdomain.com"
              style={{
                flex: 1,
                background: 'color-mix(in srgb, var(--j-text) 5%, transparent)',
                border: '1px solid color-mix(in srgb, var(--j-text) 12%, transparent)',
                borderRadius: '6px',
                padding: '10px 14px',
                color: 'var(--j-text)',
                fontSize: '14px',
                fontFamily: 'monospace',
                outline: 'none',
              }}
              onKeyDown={(e) => e.key === 'Enter' && handleAddCustomDomain()}
            />
            <button aria-label="Add"
              onClick={handleAddCustomDomain}
              disabled={addingCustom || !customDomain.trim()}
              style={{
                background: 'var(--j-surface-3)',
                color: 'var(--j-text)',
                border: 'none',
                borderRadius: '6px',
                padding: '10px 20px',
                cursor: addingCustom || !customDomain.trim() ? 'not-allowed' : 'pointer',
                display: 'flex',
                alignItems: 'center',
                gap: '8px',
                fontSize: '13px',
                fontWeight: 600,
                opacity: addingCustom || !customDomain.trim() ? 0.5 : 1,
                whiteSpace: 'nowrap',
              }}
            >
              {addingCustom ? (
                <>
                  <FontAwesomeIcon icon={faSpinner} spin /> Adding...
                </>
              ) : (
                <>
                  <FontAwesomeIcon icon={faPlus} /> Add Domain
                </>
              )}
            </button>
          </div>
          <div style={{ fontSize: '12px', color: 'var(--j-text-muted)', marginTop: '8px' }}>
            Enter a custom tracking domain. The system will set up SSL, content delivery, and DNS records.
            Once active, all tracking links will automatically use this branded domain.
          </div>
        </div>
      </div>
    </div>
  );
};
