import React, { useState, useEffect, useCallback, useRef } from 'react';
import { useAuth } from '../../../contexts/AuthContext';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faGlobe,
  faSpinner,
  faCheck,
  faClock,
  faExclamationTriangle,
  faPlus,
  faSync,
  faCloudUploadAlt,
  faImage,
} from '@fortawesome/free-solid-svg-icons';

const API_BASE = '/api/mailing';

// ---- Interfaces ----

interface ImageDomainSuggestion {
  sending_domain: string;
  suggested_image_domain: string;
  profile_name: string;
  status: 'not_provisioned' | 'pending' | 'provisioning' | 'active' | 'failed';
  domain_id: string | null;
  ssl_status: string;
  cloudfront_domain: string;
  verified: boolean;
}

interface ImageDomain {
  id: string;
  domain: string;
  org_id: string;
  s3_bucket: string;
  cloudfront_domain: string;
  ssl_status: string;
  verified: boolean;
  created_at: string;
  updated_at: string;
}

interface AWSStatus {
  domain_id: string;
  status: string;
  ssl_status: string;
  cloudfront_domain: string;
  verified: boolean;
  steps?: Record<string, string>;
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

const stepLabels: Record<string, string> = {
  s3_bucket: 'S3 Bucket Configuration',
  acm_certificate: 'ACM SSL Certificate',
  dns_validation: 'DNS Validation',
  cloudfront_distribution: 'CloudFront Distribution',
  route53_alias: 'Route53 ALIAS Record',
};

const statusConfig: Record<string, { label: string; color: string; bg: string; icon: any }> = {
  not_provisioned: { label: 'Not Provisioned', color: '#888', bg: 'rgba(136,136,136,0.15)', icon: faClock },
  pending: { label: 'Pending', color: '#fdcb6e', bg: 'rgba(253,203,110,0.15)', icon: faClock },
  provisioning: { label: 'Provisioning', color: '#74b9ff', bg: 'rgba(116,185,255,0.15)', icon: faSpinner },
  active: { label: 'Active', color: '#00b894', bg: 'rgba(0,184,148,0.15)', icon: faCheck },
  failed: { label: 'Failed', color: '#e94560', bg: 'rgba(233,69,96,0.15)', icon: faExclamationTriangle },
};

// ---- Component ----

export const ImageDomainManager: React.FC = () => {
  const { organization } = useAuth();
  const orgId = organization?.id || '';

  const [suggestions, setSuggestions] = useState<ImageDomainSuggestion[]>([]);
  const [existingDomains, setExistingDomains] = useState<ImageDomain[]>([]);
  const [loading, setLoading] = useState(false);
  const [provisioning, setProvisioning] = useState<Record<string, boolean>>({});
  const [awsStatuses, setAwsStatuses] = useState<Record<string, AWSStatus>>({});
  const [error, setError] = useState<string | null>(null);
  const [customDomain, setCustomDomain] = useState('');
  const [addingCustom, setAddingCustom] = useState(false);

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
      const res = await orgFetch(`${API_BASE}/image-domains/suggestions`, orgId);
      if (res.ok) {
        const data = await res.json();
        setSuggestions(data.suggestions || []);
      }
    } catch (err: any) {
      console.error('Failed to fetch suggestions:', err);
    }
  }, [orgId]);

  const fetchExistingDomains = useCallback(async () => {
    if (!orgId) return;
    try {
      const res = await orgFetch(`${API_BASE}/image-domains`, orgId);
      if (res.ok) {
        const data = await res.json();
        setExistingDomains(Array.isArray(data) ? data : data.domains || []);
      }
    } catch (err: any) {
      console.error('Failed to fetch existing domains:', err);
    }
  }, [orgId]);

  const loadAll = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      await Promise.all([fetchSuggestions(), fetchExistingDomains()]);
    } catch (err: any) {
      setError(err.message || 'Failed to load data');
    } finally {
      setLoading(false);
    }
  }, [fetchSuggestions, fetchExistingDomains]);

  useEffect(() => {
    if (orgId) loadAll();
  }, [orgId, loadAll]);

  // ---- Provision Domain ----

  const pollStatus = useCallback((domainId: string, suggestedDomain: string) => {
    // Clear existing timer for this domain
    if (pollTimersRef.current[suggestedDomain]) {
      clearInterval(pollTimersRef.current[suggestedDomain]);
    }

    const timer = setInterval(async () => {
      try {
        const res = await orgFetch(`${API_BASE}/aws/image-domains/${domainId}/status`, orgId);
        if (res.ok) {
          const status: AWSStatus = await res.json();
          setAwsStatuses(prev => ({ ...prev, [domainId]: status }));

          // If active or failed, stop polling and refresh
          if (status.status === 'active' || status.status === 'failed' || status.ssl_status === 'active' || status.ssl_status === 'failed') {
            clearInterval(pollTimersRef.current[suggestedDomain]);
            delete pollTimersRef.current[suggestedDomain];
            setProvisioning(prev => ({ ...prev, [suggestedDomain]: false }));
            // Refresh everything
            await fetchSuggestions();
            await fetchExistingDomains();
          }
        }
      } catch (err) {
        console.error('Poll status error:', err);
      }
    }, 5000);

    pollTimersRef.current[suggestedDomain] = timer;
  }, [orgId, fetchSuggestions, fetchExistingDomains]);

  const provisionDomain = useCallback(async (domain: string) => {
    if (!orgId) return;
    setProvisioning(prev => ({ ...prev, [domain]: true }));
    setError(null);

    try {
      const res = await orgFetch(`${API_BASE}/aws/image-domains/provision`, orgId, {
        method: 'POST',
        body: JSON.stringify({ domain, bucket_name: 'ignite-email-images-prod' }),
      });

      if (!res.ok) {
        const errData = await res.json().catch(() => ({}));
        throw new Error(errData.error || `Provision failed (${res.status})`);
      }

      const data = await res.json();
      const domainInfo = data.domain;

      // Start polling for status
      if (domainInfo?.id) {
        pollStatus(domainInfo.id, domain);
      }

      // Refresh suggestions to pick up the new domain_id
      await fetchSuggestions();
      await fetchExistingDomains();
    } catch (err: any) {
      setError(err.message || 'Failed to provision domain');
      setProvisioning(prev => ({ ...prev, [domain]: false }));
    }
  }, [orgId, pollStatus, fetchSuggestions, fetchExistingDomains]);

  // ---- Add Custom Domain ----

  const handleAddCustomDomain = useCallback(async () => {
    if (!orgId || !customDomain.trim()) return;

    const domain = customDomain.trim().toLowerCase();
    if (!domain.includes('.')) {
      setError('Please enter a valid domain (e.g., img.example.com)');
      return;
    }

    setAddingCustom(true);
    setError(null);

    try {
      const res = await orgFetch(`${API_BASE}/aws/image-domains/provision`, orgId, {
        method: 'POST',
        body: JSON.stringify({ domain, bucket_name: 'ignite-email-images-prod' }),
      });

      if (!res.ok) {
        const errData = await res.json().catch(() => ({}));
        throw new Error(errData.error || `Failed to add domain (${res.status})`);
      }

      const data = await res.json();
      const domainInfo = data.domain;
      if (domainInfo?.id) {
        pollStatus(domainInfo.id, domain);
      }

      setCustomDomain('');
      await loadAll();
    } catch (err: any) {
      setError(err.message || 'Failed to add custom domain');
    } finally {
      setAddingCustom(false);
    }
  }, [orgId, customDomain, pollStatus, loadAll]);

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

  const getStepIcon = (stepStatus: string) => {
    if (stepStatus === 'complete') return <FontAwesomeIcon icon={faCheck} style={{ color: '#00b894', fontSize: '12px' }} />;
    if (stepStatus === 'failed') return <FontAwesomeIcon icon={faExclamationTriangle} style={{ color: '#e94560', fontSize: '12px' }} />;
    return <FontAwesomeIcon icon={faClock} style={{ color: '#888', fontSize: '12px' }} />;
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
          <FontAwesomeIcon icon={faImage} style={{ fontSize: '22px', color: '#74b9ff' }} />
          <h2 style={{ margin: 0, fontSize: '20px', fontWeight: 700, color: '#fff' }}>Image CDN Domains</h2>
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
          background: 'rgba(116,185,255,0.08)',
          border: '1px solid rgba(116,185,255,0.2)',
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
        <FontAwesomeIcon icon={faCloudUploadAlt} style={{ color: '#74b9ff', fontSize: '16px', marginTop: '2px' }} />
        <div>
          <strong style={{ color: '#74b9ff' }}>Custom Image Domains</strong> (e.g.,{' '}
          <code style={{ background: 'rgba(255,255,255,0.06)', padding: '2px 6px', borderRadius: '4px', fontSize: '12px' }}>
            img.yoursendingdomain.com
          </code>
          ) allow emails to serve images from your brand domain, improving deliverability and inbox reputation.
          The system automatically provisions S3 storage, SSL certificates, CloudFront CDN, and DNS records using your AWS profile.
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
          <FontAwesomeIcon icon={faGlobe} style={{ color: '#74b9ff' }} />
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
              No sending profiles found with configured domains. Add sending profiles to see image domain suggestions.
            </div>
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column', gap: '12px' }}>
              {suggestions.map((s, idx) => {
                const isProvisioning = provisioning[s.suggested_image_domain];
                const awsStatus = s.domain_id ? awsStatuses[s.domain_id] : null;

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
                            {s.suggested_image_domain}
                          </span>
                        </div>
                        {s.cloudfront_domain && (
                          <div style={{ fontSize: '11px', color: '#74b9ff', marginTop: '4px' }}>
                            CloudFront: {s.cloudfront_domain}
                          </div>
                        )}
                      </div>

                      <div style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
                        {getStatusBadge(s.status)}
                        {s.status === 'not_provisioned' && (
                          <button
                            onClick={() => provisionDomain(s.suggested_image_domain)}
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
                                <FontAwesomeIcon icon={faCloudUploadAlt} /> Provision
                              </>
                            )}
                          </button>
                        )}
                        {s.status === 'failed' && (
                          <button
                            onClick={() => provisionDomain(s.suggested_image_domain)}
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

                    {/* Provisioning Steps */}
                    {(s.status === 'provisioning' || s.status === 'pending' || isProvisioning) && awsStatus?.steps && (
                      <div style={{ marginTop: '14px', paddingTop: '14px', borderTop: '1px solid rgba(255,255,255,0.06)' }}>
                        <div style={{ fontSize: '11px', color: '#888', textTransform: 'uppercase', letterSpacing: '0.5px', marginBottom: '10px' }}>
                          Provisioning Progress
                        </div>
                        <div style={{ display: 'flex', flexDirection: 'column', gap: '8px' }}>
                          {Object.entries(awsStatus.steps).map(([key, value]) => (
                            <div key={key} style={{ display: 'flex', alignItems: 'center', gap: '10px', fontSize: '13px' }}>
                              <span style={{ width: '16px', textAlign: 'center' }}>{getStepIcon(value)}</span>
                              <span
                                style={{
                                  color: value === 'complete' ? '#00b894' : value === 'failed' ? '#e94560' : '#b0b0c0',
                                }}
                              >
                                {stepLabels[key] || key}
                              </span>
                            </div>
                          ))}
                        </div>
                      </div>
                    )}

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
                            Verified
                          </span>
                        )}
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
          <h3 style={{ margin: 0, fontSize: '15px', fontWeight: 600 }}>Existing Image Domains</h3>
        </div>

        <div style={{ padding: '16px 20px' }}>
          {existingDomains.length === 0 ? (
            <div style={{ textAlign: 'center', padding: '24px', color: '#888', fontSize: '14px' }}>
              No image domains registered yet. Use the suggestions above or add a custom domain below.
            </div>
          ) : (
            <div style={{ overflowX: 'auto' }}>
              <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: '13px' }}>
                <thead>
                  <tr style={{ borderBottom: '1px solid rgba(255,255,255,0.1)' }}>
                    <th style={{ padding: '10px 12px', textAlign: 'left', color: '#888', fontWeight: 600, fontSize: '11px', textTransform: 'uppercase' }}>Domain</th>
                    <th style={{ padding: '10px 12px', textAlign: 'left', color: '#888', fontWeight: 600, fontSize: '11px', textTransform: 'uppercase' }}>CloudFront</th>
                    <th style={{ padding: '10px 12px', textAlign: 'center', color: '#888', fontWeight: 600, fontSize: '11px', textTransform: 'uppercase' }}>SSL</th>
                    <th style={{ padding: '10px 12px', textAlign: 'center', color: '#888', fontWeight: 600, fontSize: '11px', textTransform: 'uppercase' }}>Verified</th>
                    <th style={{ padding: '10px 12px', textAlign: 'left', color: '#888', fontWeight: 600, fontSize: '11px', textTransform: 'uppercase' }}>Created</th>
                  </tr>
                </thead>
                <tbody>
                  {existingDomains.map((d) => (
                    <tr key={d.id} style={{ borderBottom: '1px solid rgba(255,255,255,0.04)' }}>
                      <td style={{ padding: '12px', fontFamily: 'monospace', fontWeight: 600 }}>{d.domain}</td>
                      <td style={{ padding: '12px', color: '#74b9ff', fontSize: '12px' }}>{d.cloudfront_domain || '-'}</td>
                      <td style={{ padding: '12px', textAlign: 'center' }}>{getStatusBadge(d.ssl_status || 'pending')}</td>
                      <td style={{ padding: '12px', textAlign: 'center' }}>
                        {d.verified ? (
                          <FontAwesomeIcon icon={faCheck} style={{ color: '#00b894' }} />
                        ) : (
                          <FontAwesomeIcon icon={faClock} style={{ color: '#fdcb6e' }} />
                        )}
                      </td>
                      <td style={{ padding: '12px', color: '#888', fontSize: '12px' }}>{formatDate(d.created_at)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
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
          <h3 style={{ margin: 0, fontSize: '15px', fontWeight: 600 }}>Add Custom Image Domain</h3>
        </div>

        <div style={{ padding: '16px 20px' }}>
          <div style={{ display: 'flex', gap: '12px', alignItems: 'center' }}>
            <input
              type="text"
              value={customDomain}
              onChange={(e) => setCustomDomain(e.target.value)}
              placeholder="img.yourdomain.com"
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
            Enter a custom domain for image hosting. The system will automatically provision S3, SSL, CloudFront, and DNS records.
          </div>
        </div>
      </div>
    </div>
  );
};
