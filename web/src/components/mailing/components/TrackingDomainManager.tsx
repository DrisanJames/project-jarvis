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
  not_provisioned: { label: 'Not Provisioned', color: '#888', bg: 'rgba(136,136,136,0.15)', icon: faClock },
  pending: { label: 'Pending', color: '#fdcb6e', bg: 'rgba(253,203,110,0.15)', icon: faClock },
  provisioning: { label: 'Provisioning', color: '#74b9ff', bg: 'rgba(116,185,255,0.15)', icon: faSpinner },
  active: { label: 'Active', color: '#00b894', bg: 'rgba(0,184,148,0.15)', icon: faCheck },
  failed: { label: 'Failed', color: '#e94560', bg: 'rgba(233,69,96,0.15)', icon: faExclamationTriangle },
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
          body: JSON.stringify({ origin_server: 'api.ignitemailing.com' }),
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
    <div style={{ padding: '0', maxWidth: '960px', color: '#e0e0e0' }}>
      {/* Header */}
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '20px' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
          <FontAwesomeIcon icon={faLink} style={{ fontSize: '22px', color: '#a29bfe' }} />
          <h2 style={{ margin: 0, fontSize: '20px', fontWeight: 700, color: '#fff' }}>Tracking Domains</h2>
        </div>
        <button
          onClick={loadAll}
          disabled={loading}
          style={{
            background: 'rgba(255,255,255,0.06)',
            color: '#e0e0e0',
            border: '1px solid rgba(255,255,255,0.1)',
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
            background: activeTrackingURL.includes('si3p4trk') ? 'rgba(253,203,110,0.1)' : 'rgba(0,184,148,0.1)',
            border: `1px solid ${activeTrackingURL.includes('si3p4trk') ? 'rgba(253,203,110,0.3)' : 'rgba(0,184,148,0.3)'}`,
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
            style={{ color: activeTrackingURL.includes('si3p4trk') ? '#fdcb6e' : '#00b894' }}
          />
          <div>
            <strong>Active Tracking Domain:</strong>{' '}
            <code style={{ background: 'rgba(255,255,255,0.06)', padding: '2px 6px', borderRadius: '4px', fontSize: '12px' }}>
              {activeTrackingURL}
            </code>
            {activeTrackingURL.includes('si3p4trk') && (
              <span style={{ color: '#fdcb6e', marginLeft: '8px', fontSize: '12px' }}>
                (Default Everflow domain - provision a branded domain below)
              </span>
            )}
          </div>
        </div>
      )}

      {/* Error Banner */}
      {error && (
        <div
          style={{
            background: 'rgba(233,69,96,0.12)',
            border: '1px solid rgba(233,69,96,0.3)',
            borderRadius: '8px',
            padding: '12px 16px',
            marginBottom: '20px',
            color: '#e94560',
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
            style={{ marginLeft: 'auto', background: 'none', border: 'none', color: '#e94560', cursor: 'pointer', fontSize: '16px' }}
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
        <FontAwesomeIcon icon={faShieldAlt} style={{ color: '#a29bfe', fontSize: '16px', marginTop: '2px' }} />
        <div>
          <strong style={{ color: '#a29bfe' }}>Branded Tracking Domains</strong> (e.g.,{' '}
          <code style={{ background: 'rgba(255,255,255,0.06)', padding: '2px 6px', borderRadius: '4px', fontSize: '12px' }}>
            track.yoursendingdomain.com
          </code>
          ) replace the default Everflow tracking domain in email links, improving brand consistency and inbox deliverability.
          Once active, all new tracking links will automatically use the branded domain.
        </div>
      </div>

      {/* Suggestions Section */}
      <div
        style={{
          background: '#1a1a2e',
          borderRadius: '10px',
          overflow: 'hidden',
          marginBottom: '24px',
          border: '1px solid rgba(255,255,255,0.06)',
        }}
      >
        <div
          style={{
            background: '#16213e',
            padding: '14px 20px',
            borderBottom: '1px solid rgba(255,255,255,0.06)',
            display: 'flex',
            alignItems: 'center',
            gap: '10px',
          }}
        >
          <FontAwesomeIcon icon={faGlobe} style={{ color: '#a29bfe' }} />
          <h3 style={{ margin: 0, fontSize: '15px', fontWeight: 600 }}>
            Domain Suggestions from Sending Profiles
          </h3>
        </div>

        <div style={{ padding: '16px 20px' }}>
          {loading && suggestions.length === 0 ? (
            <div style={{ textAlign: 'center', padding: '30px', color: '#888' }}>
              <FontAwesomeIcon icon={faSpinner} spin style={{ fontSize: '20px', marginBottom: '10px' }} />
              <div>Loading suggestions...</div>
            </div>
          ) : suggestions.length === 0 ? (
            <div style={{ textAlign: 'center', padding: '30px', color: '#888', fontSize: '14px' }}>
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
                      background: 'rgba(255,255,255,0.03)',
                      border: '1px solid rgba(255,255,255,0.06)',
                      borderRadius: '8px',
                      padding: '16px',
                    }}
                  >
                    {/* Row: info + action */}
                    <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: '12px' }}>
                      <div style={{ flex: 1, minWidth: '250px' }}>
                        <div style={{ fontSize: '11px', color: '#888', textTransform: 'uppercase', letterSpacing: '0.5px', marginBottom: '4px' }}>
                          {s.profile_name}
                        </div>
                        <div style={{ display: 'flex', alignItems: 'center', gap: '8px', flexWrap: 'wrap' }}>
                          <span style={{ fontSize: '13px', color: '#aaa' }}>{s.sending_domain}</span>
                          <span style={{ color: '#555' }}>&rarr;</span>
                          <span style={{ fontSize: '14px', fontWeight: 600, color: '#e0e0e0', fontFamily: 'monospace' }}>
                            {s.suggested_tracking_domain}
                          </span>
                        </div>
                        {s.cloudfront_domain && (
                          <div style={{ fontSize: '11px', color: '#a29bfe', marginTop: '4px' }}>
                            CloudFront: {s.cloudfront_domain}
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
                              background: '#0f3460',
                              color: '#e0e0e0',
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
                              background: '#e94560',
                              color: '#fff',
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
                          borderTop: '1px solid rgba(255,255,255,0.06)',
                          display: 'flex',
                          gap: '16px',
                          flexWrap: 'wrap',
                          fontSize: '12px',
                        }}
                      >
                        <span style={{ color: '#00b894' }}>
                          <FontAwesomeIcon icon={faCheck} style={{ marginRight: '4px' }} />
                          SSL: {s.ssl_status || 'Active'}
                        </span>
                        {s.verified && (
                          <span style={{ color: '#00b894' }}>
                            <FontAwesomeIcon icon={faCheck} style={{ marginRight: '4px' }} />
                            DNS Verified
                          </span>
                        )}
                        <span style={{ color: '#a29bfe' }}>
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
          background: '#1a1a2e',
          borderRadius: '10px',
          overflow: 'hidden',
          marginBottom: '24px',
          border: '1px solid rgba(255,255,255,0.06)',
        }}
      >
        <div
          style={{
            background: '#16213e',
            padding: '14px 20px',
            borderBottom: '1px solid rgba(255,255,255,0.06)',
            display: 'flex',
            alignItems: 'center',
            gap: '10px',
          }}
        >
          <FontAwesomeIcon icon={faGlobe} style={{ color: '#00b894' }} />
          <h3 style={{ margin: 0, fontSize: '15px', fontWeight: 600 }}>Registered Tracking Domains</h3>
        </div>

        <div style={{ padding: '16px 20px' }}>
          {existingDomains.length === 0 ? (
            <div style={{ textAlign: 'center', padding: '24px', color: '#888', fontSize: '14px' }}>
              No tracking domains registered yet. Use the suggestions above or add a custom domain below.
            </div>
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column', gap: '12px' }}>
              {existingDomains.map((d) => (
                <div
                  key={d.id}
                  style={{
                    background: 'rgba(255,255,255,0.03)',
                    border: '1px solid rgba(255,255,255,0.06)',
                    borderRadius: '8px',
                    padding: '16px',
                  }}
                >
                  <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: '12px' }}>
                    <div>
                      <div style={{ fontSize: '14px', fontWeight: 600, fontFamily: 'monospace' }}>{d.domain}</div>
                      {d.cloudfront_domain && (
                        <div style={{ fontSize: '11px', color: '#a29bfe', marginTop: '4px' }}>
                          CloudFront: {d.cloudfront_domain}
                        </div>
                      )}
                      <div style={{ fontSize: '11px', color: '#888', marginTop: '4px' }}>
                        Created: {formatDate(d.created_at)}
                      </div>
                    </div>

                    <div style={{ display: 'flex', alignItems: 'center', gap: '10px' }}>
                      {getStatusBadge(d.ssl_status || 'pending')}
                      {d.verified ? (
                        <span style={{ color: '#00b894', fontSize: '12px', display: 'flex', alignItems: 'center', gap: '4px' }}>
                          <FontAwesomeIcon icon={faCheck} /> Verified
                        </span>
                      ) : (
                        <button
                          onClick={() => verifyDomain(d.id)}
                          disabled={verifying[d.id]}
                          style={{
                            background: 'rgba(253,203,110,0.15)',
                            color: '#fdcb6e',
                            border: '1px solid rgba(253,203,110,0.3)',
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
                          background: 'rgba(255,255,255,0.06)',
                          color: '#e0e0e0',
                          border: '1px solid rgba(255,255,255,0.08)',
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
                    <div style={{ marginTop: '14px', paddingTop: '14px', borderTop: '1px solid rgba(255,255,255,0.06)' }}>
                      <div style={{ fontSize: '11px', color: '#888', textTransform: 'uppercase', letterSpacing: '0.5px', marginBottom: '8px' }}>
                        Required DNS Records
                      </div>
                      <div style={{ display: 'flex', flexDirection: 'column', gap: '6px' }}>
                        {d.dns_records.map((rec, idx) => (
                          <div key={idx} style={{ display: 'flex', alignItems: 'center', gap: '10px', fontSize: '12px' }}>
                            <span style={{ width: '16px', textAlign: 'center' }}>
                              {rec.status === 'verified' ? (
                                <FontAwesomeIcon icon={faCheck} style={{ color: '#00b894' }} />
                              ) : (
                                <FontAwesomeIcon icon={faClock} style={{ color: '#888' }} />
                              )}
                            </span>
                            <span style={{
                              background: 'rgba(255,255,255,0.06)',
                              padding: '2px 8px',
                              borderRadius: '4px',
                              fontSize: '11px',
                              fontWeight: 600,
                              color: '#74b9ff',
                              minWidth: '50px',
                              textAlign: 'center',
                            }}>
                              {rec.type}
                            </span>
                            <span style={{ color: '#aaa', fontFamily: 'monospace', fontSize: '11px' }}>{rec.name}</span>
                            <span style={{ color: '#555' }}>&rarr;</span>
                            <span style={{ color: '#e0e0e0', fontFamily: 'monospace', fontSize: '11px', wordBreak: 'break-all' }}>
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
          background: '#1a1a2e',
          borderRadius: '10px',
          overflow: 'hidden',
          border: '1px solid rgba(255,255,255,0.06)',
        }}
      >
        <div
          style={{
            background: '#16213e',
            padding: '14px 20px',
            borderBottom: '1px solid rgba(255,255,255,0.06)',
            display: 'flex',
            alignItems: 'center',
            gap: '10px',
          }}
        >
          <FontAwesomeIcon icon={faPlus} style={{ color: '#fdcb6e' }} />
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
                background: 'rgba(255,255,255,0.05)',
                border: '1px solid rgba(255,255,255,0.12)',
                borderRadius: '6px',
                padding: '10px 14px',
                color: '#e0e0e0',
                fontSize: '14px',
                fontFamily: 'monospace',
                outline: 'none',
              }}
              onKeyDown={(e) => e.key === 'Enter' && handleAddCustomDomain()}
            />
            <button
              onClick={handleAddCustomDomain}
              disabled={addingCustom || !customDomain.trim()}
              style={{
                background: '#0f3460',
                color: '#e0e0e0',
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
          <div style={{ fontSize: '12px', color: '#888', marginTop: '8px' }}>
            Enter a custom tracking domain. The system will provision SSL via ACM, CloudFront distribution, and DNS records.
            Once active, all Everflow tracking links will automatically use this branded domain.
          </div>
        </div>
      </div>
    </div>
  );
};
