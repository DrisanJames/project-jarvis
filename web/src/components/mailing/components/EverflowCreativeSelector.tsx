import React, { useState, useEffect, useCallback } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faSearch, faImage, faLink, faExclamationTriangle, faCheckCircle,
  faSpinner, faDollarSign, faTag, faCopy, faEye,
  faPaste, faChevronDown, faChevronUp,
} from '@fortawesome/free-solid-svg-icons';

// ---------- Types ----------

interface EverflowCreative {
  creative_id: number;
  name: string;
  has_tracking_link: boolean;
  html_preview?: string;
}

interface CreativeCategory {
  payment_model: string;
  offer_name: string;
  offer_id: number;
  internal_id: string;
  requires_manual_link: boolean;
  creatives: EverflowCreative[];
}

interface AffiliateEncoding {
  id: string;
  affiliate_id: string;
  encoded_value: string;
  affiliate_name: string;
  is_default: boolean;
}

interface TrackingLinkResponse {
  tracking_link: string;
  merge_tags_used: string[];
  branded_domain?: string;
  branded_domain_used?: boolean;
  original_domain?: string;
}

interface Props {
  onCreativeSelect: (html: string, creativeId: number, offerId: number, trackingLink: string) => void;
  organizationId?: string;
}

const API_BASE = '/api/mailing';

async function orgFetch(url: string, orgId?: string, opts?: RequestInit) {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    ...(opts?.headers as Record<string, string> || {}),
  };
  if (orgId) headers['X-Organization-ID'] = orgId;
  return fetch(url, { ...opts, headers });
}

// ---------- Component ----------

export const EverflowCreativeSelector: React.FC<Props> = ({ onCreativeSelect, organizationId }) => {
  const [search, setSearch] = useState('Sams');
  const [categories, setCategories] = useState<CreativeCategory[]>([]);
  const [totalCreatives, setTotalCreatives] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');

  // Selection state
  const [selectedCategory, setSelectedCategory] = useState<CreativeCategory | null>(null);
  const [selectedCreative, setSelectedCreative] = useState<EverflowCreative | null>(null);
  const [expandedCategories, setExpandedCategories] = useState<Set<string>>(new Set());

  // Tracking link state
  const [affiliates, setAffiliates] = useState<AffiliateEncoding[]>([]);
  const [selectedAffiliate, setSelectedAffiliate] = useState('');
  const [trackingLink, setTrackingLink] = useState('');
  const [buildingLink, setBuildingLink] = useState(false);
  const [manualLink, setManualLink] = useState('');

  // Preview state
  const [previewHtml, setPreviewHtml] = useState('');
  const [showPreview, setShowPreview] = useState(false);

  // Raw creative HTML (fetched from Everflow via search endpoint)
  const [rawCreatives, setRawCreatives] = useState<Record<number, string>>({});

  // ---------- Fetch categories ----------

  const fetchCategories = useCallback(async () => {
    setLoading(true);
    setError('');
    try {
      const res = await orgFetch(
        `${API_BASE}/everflow-creatives/categories?search=${encodeURIComponent(search)}`,
        organizationId
      );
      if (!res.ok) throw new Error('Failed to fetch creatives');
      const data = await res.json();
      setCategories(data.categories || []);
      setTotalCreatives(data.total_creatives || 0);
    } catch (err: any) {
      setError(err.message);
    } finally {
      setLoading(false);
    }
  }, [search, organizationId]);

  // Fetch raw creative HTML for all results
  const fetchRawCreatives = useCallback(async () => {
    try {
      const res = await orgFetch(
        `${API_BASE}/everflow-creatives/?search=${encodeURIComponent(search)}`,
        organizationId
      );
      if (!res.ok) return;
      const data = await res.json();
      const map: Record<number, string> = {};
      for (const c of data.creatives || []) {
        map[c.network_offer_creative_id] = c.html_code || '';
      }
      setRawCreatives(map);
    } catch { /* ignore */ }
  }, [search, organizationId]);

  // Fetch affiliate encodings
  const fetchAffiliates = useCallback(async () => {
    try {
      const res = await orgFetch(`${API_BASE}/everflow-creatives/affiliate-encodings`, organizationId);
      if (!res.ok) return;
      const data = await res.json();
      setAffiliates(data || []);
      const def = (data || []).find((a: AffiliateEncoding) => a.is_default);
      if (def) setSelectedAffiliate(def.affiliate_id);
      else if (data?.length) setSelectedAffiliate(data[0].affiliate_id);
    } catch { /* ignore */ }
  }, [organizationId]);

  useEffect(() => {
    fetchAffiliates();
  }, [fetchAffiliates]);

  useEffect(() => {
    if (search.length >= 2) {
      fetchCategories();
      fetchRawCreatives();
    }
  }, [search, fetchCategories, fetchRawCreatives]);

  // ---------- Build tracking link ----------

  const [brandedDomainInfo, setBrandedDomainInfo] = useState<{ used: boolean; domain: string; original: string }>({ used: false, domain: '', original: '' });

  const buildTrackingLink = async () => {
    if (!selectedCategory || !selectedCreative || !selectedAffiliate) return;

    setBuildingLink(true);
    try {
      const res = await orgFetch(`${API_BASE}/everflow-creatives/build-tracking-link`, organizationId, {
        method: 'POST',
        body: JSON.stringify({
          affiliate_id: selectedAffiliate,
          offer_id: String(selectedCategory.offer_id),
          creative_id: selectedCreative.creative_id,
          data_set: 'IGN',
        }),
      });
      if (!res.ok) throw new Error('Failed to build tracking link');
      const data: TrackingLinkResponse = await res.json();
      setTrackingLink(data.tracking_link);
      setBrandedDomainInfo({
        used: data.branded_domain_used || false,
        domain: data.branded_domain || '',
        original: data.original_domain || '',
      });
    } catch (err: any) {
      setError(err.message);
    } finally {
      setBuildingLink(false);
    }
  };

  // ---------- Image rehosting state ----------
  const [rehostingImages, setRehostingImages] = useState(false);
  const [rehostStatus, setRehostStatus] = useState('');

  // ---------- Apply creative ----------

  const handleApplyCreative = async () => {
    if (!selectedCreative || !selectedCategory) return;

    const html = rawCreatives[selectedCreative.creative_id] || '';
    if (!html) {
      setError('No HTML content found for this creative');
      return;
    }

    // Determine the tracking link to use
    let finalLink = trackingLink;
    if (selectedCategory.requires_manual_link) {
      if (!manualLink) {
        setError('This offer requires a manual tracking link. Please paste it above.');
        return;
      }
      finalLink = manualLink;
    }

    if (!finalLink) {
      setError('Please build or paste a tracking link first');
      return;
    }

    // Replace {tracking_link} in HTML
    let processedHtml = html.replace(/\{tracking_link\}/g, finalLink);

    // Rehost external images through our CDN
    setRehostingImages(true);
    setRehostStatus('Downloading and rehosting images to CDN...');
    setError('');

    try {
      const rehostRes = await orgFetch(
        `${API_BASE}/images/rehost-html`,
        organizationId,
        {
          method: 'POST',
          body: JSON.stringify({
            html: processedHtml,
            org_id: organizationId || '',
          }),
        }
      );

      if (rehostRes.ok) {
        const rehostData = await rehostRes.json();
        processedHtml = rehostData.html;
        const parts: string[] = [];
        if (rehostData.images_rehosted > 0) parts.push(`${rehostData.images_rehosted} rehosted`);
        if (rehostData.images_cached > 0) parts.push(`${rehostData.images_cached} cached`);
        if (rehostData.images_skipped > 0) parts.push(`${rehostData.images_skipped} skipped`);
        if (rehostData.images_failed > 0) parts.push(`${rehostData.images_failed} failed`);
        setRehostStatus(`Images processed: ${parts.join(', ') || 'none found'}`);
      } else {
        const errData = await rehostRes.json().catch(() => ({ error: 'Unknown error' }));
        console.warn('Image rehosting failed, using original URLs:', errData.error);
        setRehostStatus('Image rehosting unavailable - using original image URLs');
      }
    } catch (err: any) {
      console.warn('Image rehosting request failed, using original URLs:', err);
      setRehostStatus('Image rehosting unavailable - using original image URLs');
    } finally {
      setRehostingImages(false);
    }

    onCreativeSelect(
      processedHtml,
      selectedCreative.creative_id,
      selectedCategory.offer_id,
      finalLink,
    );
  };

  // ---------- Toggle category expand ----------

  const toggleCategory = (key: string) => {
    setExpandedCategories(prev => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  const getCategoryKey = (cat: CreativeCategory) => `${cat.payment_model}-${cat.offer_id}`;

  // ---------- Render ----------

  return (
    <div className="ef-creative-selector">
      <style>{`
        .ef-creative-selector {
          background: #1a1a2e;
          border-radius: 12px;
          padding: 20px;
          margin-bottom: 20px;
          border: 1px solid rgba(255,255,255,0.1);
        }
        .ef-header {
          display: flex;
          align-items: center;
          gap: 12px;
          margin-bottom: 16px;
        }
        .ef-header h3 {
          margin: 0;
          color: #e0e0e0;
          font-size: 16px;
        }
        .ef-header .ef-badge {
          background: #6c5ce7;
          color: white;
          padding: 2px 8px;
          border-radius: 12px;
          font-size: 11px;
        }
        .ef-search-row {
          display: flex;
          gap: 10px;
          margin-bottom: 16px;
        }
        .ef-search-input {
          flex: 1;
          background: rgba(255,255,255,0.06);
          border: 1px solid rgba(255,255,255,0.15);
          color: #e0e0e0;
          padding: 8px 12px;
          border-radius: 8px;
          font-size: 14px;
        }
        .ef-search-input::placeholder { color: rgba(255,255,255,0.35); }
        .ef-search-btn {
          background: #6c5ce7;
          color: white;
          border: none;
          padding: 8px 16px;
          border-radius: 8px;
          cursor: pointer;
          font-size: 14px;
          display: flex;
          align-items: center;
          gap: 6px;
        }
        .ef-search-btn:hover { background: #5a4bd1; }
        .ef-search-btn:disabled { opacity: 0.5; cursor: not-allowed; }
        .ef-error {
          background: rgba(231,76,60,0.15);
          color: #e74c3c;
          padding: 8px 14px;
          border-radius: 8px;
          margin-bottom: 12px;
          font-size: 13px;
        }
        .ef-categories {
          display: flex;
          flex-direction: column;
          gap: 8px;
        }
        .ef-category {
          background: rgba(255,255,255,0.04);
          border: 1px solid rgba(255,255,255,0.08);
          border-radius: 10px;
          overflow: hidden;
        }
        .ef-category-header {
          display: flex;
          align-items: center;
          padding: 12px 16px;
          cursor: pointer;
          gap: 10px;
        }
        .ef-category-header:hover { background: rgba(255,255,255,0.03); }
        .ef-model-badge {
          font-size: 11px;
          font-weight: 700;
          padding: 3px 10px;
          border-radius: 6px;
          letter-spacing: 0.5px;
        }
        .ef-model-badge.cps { background: #27ae60; color: white; }
        .ef-model-badge.cpm { background: #e67e22; color: white; }
        .ef-model-badge.cpl { background: #3498db; color: white; }
        .ef-model-badge.cpa { background: #9b59b6; color: white; }
        .ef-category-name {
          flex: 1;
          color: #e0e0e0;
          font-size: 14px;
          font-weight: 500;
        }
        .ef-category-count {
          color: rgba(255,255,255,0.4);
          font-size: 12px;
        }
        .ef-manual-badge {
          background: rgba(231,76,60,0.2);
          color: #e74c3c;
          font-size: 10px;
          padding: 2px 8px;
          border-radius: 6px;
        }
        .ef-creatives-list {
          padding: 0 16px 12px;
          display: flex;
          flex-direction: column;
          gap: 6px;
        }
        .ef-creative-item {
          display: flex;
          align-items: center;
          gap: 10px;
          padding: 8px 12px;
          background: rgba(255,255,255,0.03);
          border: 1px solid rgba(255,255,255,0.06);
          border-radius: 8px;
          cursor: pointer;
          transition: all 0.15s;
        }
        .ef-creative-item:hover {
          background: rgba(108,92,231,0.15);
          border-color: rgba(108,92,231,0.3);
        }
        .ef-creative-item.selected {
          background: rgba(108,92,231,0.2);
          border-color: #6c5ce7;
        }
        .ef-creative-name {
          flex: 1;
          color: #e0e0e0;
          font-size: 13px;
        }
        .ef-creative-id {
          color: rgba(255,255,255,0.3);
          font-size: 11px;
        }
        .ef-link-icon {
          color: #27ae60;
          font-size: 12px;
        }
        .ef-link-icon.missing { color: rgba(255,255,255,0.2); }
        
        /* Tracking Link Builder */
        .ef-link-builder {
          margin-top: 16px;
          padding: 16px;
          background: rgba(255,255,255,0.04);
          border: 1px solid rgba(255,255,255,0.1);
          border-radius: 10px;
        }
        .ef-link-builder h4 {
          margin: 0 0 12px;
          color: #e0e0e0;
          font-size: 14px;
        }
        .ef-link-row {
          display: flex;
          gap: 10px;
          margin-bottom: 10px;
          align-items: center;
        }
        .ef-link-row label {
          min-width: 80px;
          color: rgba(255,255,255,0.5);
          font-size: 12px;
        }
        .ef-link-row select,
        .ef-link-row input {
          flex: 1;
          background: rgba(255,255,255,0.06);
          border: 1px solid rgba(255,255,255,0.15);
          color: #e0e0e0;
          padding: 6px 10px;
          border-radius: 6px;
          font-size: 13px;
        }
        .ef-tracking-link-display {
          background: rgba(39,174,96,0.1);
          border: 1px solid rgba(39,174,96,0.3);
          color: #27ae60;
          padding: 10px 14px;
          border-radius: 8px;
          font-family: monospace;
          font-size: 12px;
          word-break: break-all;
          margin: 10px 0;
        }
        .ef-manual-warning {
          background: rgba(231,76,60,0.1);
          border: 1px solid rgba(231,76,60,0.3);
          color: #e74c3c;
          padding: 12px;
          border-radius: 8px;
          margin: 10px 0;
          font-size: 13px;
        }
        .ef-manual-warning .ef-warn-title {
          font-weight: 600;
          margin-bottom: 6px;
          display: flex;
          align-items: center;
          gap: 6px;
        }
        .ef-actions {
          display: flex;
          gap: 10px;
          margin-top: 14px;
        }
        .ef-btn {
          padding: 8px 16px;
          border: none;
          border-radius: 8px;
          cursor: pointer;
          font-size: 13px;
          display: flex;
          align-items: center;
          gap: 6px;
        }
        .ef-btn-primary {
          background: #6c5ce7;
          color: white;
        }
        .ef-btn-primary:hover { background: #5a4bd1; }
        .ef-btn-primary:disabled { opacity: 0.5; cursor: not-allowed; }
        .ef-btn-success {
          background: #27ae60;
          color: white;
        }
        .ef-btn-success:hover { background: #219a52; }
        .ef-btn-success:disabled { opacity: 0.5; cursor: not-allowed; }
        .ef-btn-outline {
          background: transparent;
          color: #e0e0e0;
          border: 1px solid rgba(255,255,255,0.2);
        }
        .ef-btn-outline:hover { border-color: rgba(255,255,255,0.4); }
        
        /* Preview */
        .ef-preview-overlay {
          position: fixed;
          top: 0; left: 0; right: 0; bottom: 0;
          background: rgba(0,0,0,0.8);
          z-index: 9999;
          display: flex;
          align-items: center;
          justify-content: center;
        }
        .ef-preview-modal {
          background: white;
          border-radius: 12px;
          width: 650px;
          max-height: 90vh;
          overflow-y: auto;
        }
        .ef-preview-header {
          display: flex;
          justify-content: space-between;
          align-items: center;
          padding: 12px 16px;
          border-bottom: 1px solid #eee;
        }
        .ef-preview-header h4 { margin: 0; color: #333; }
        .ef-preview-close {
          background: none;
          border: none;
          font-size: 20px;
          cursor: pointer;
          color: #999;
        }
        .ef-preview-body {
          padding: 0;
        }
        .ef-preview-body iframe {
          width: 100%;
          height: 700px;
          border: none;
        }
        .ef-empty {
          text-align: center;
          padding: 30px;
          color: rgba(255,255,255,0.3);
          font-size: 14px;
        }
        .ef-selection-summary {
          background: rgba(108,92,231,0.1);
          border: 1px solid rgba(108,92,231,0.3);
          border-radius: 8px;
          padding: 10px 14px;
          margin-top: 10px;
          font-size: 13px;
          color: #b8b0f0;
        }
      `}</style>

      {/* Header */}
      <div className="ef-header">
        <FontAwesomeIcon icon={faImage} style={{ color: '#6c5ce7', fontSize: 18 }} />
        <h3>Everflow Creative Library</h3>
        {totalCreatives > 0 && <span className="ef-badge">{totalCreatives} creatives</span>}
      </div>

      {/* Search */}
      <div className="ef-search-row">
        <input
          className="ef-search-input"
          type="text"
          value={search}
          onChange={e => setSearch(e.target.value)}
          placeholder="Search creatives (e.g., Sams, Norton, Lifelock)..."
        />
        <button className="ef-search-btn" onClick={() => { fetchCategories(); fetchRawCreatives(); }} disabled={loading}>
          <FontAwesomeIcon icon={loading ? faSpinner : faSearch} spin={loading} />
          {loading ? 'Loading...' : 'Search'}
        </button>
      </div>

      {error && <div className="ef-error"><FontAwesomeIcon icon={faExclamationTriangle} /> {error}</div>}

      {/* Categories */}
      {categories.length > 0 ? (
        <div className="ef-categories">
          {categories.map(cat => {
            const key = getCategoryKey(cat);
            const isExpanded = expandedCategories.has(key);
            return (
              <div key={key} className="ef-category">
                <div className="ef-category-header" onClick={() => toggleCategory(key)}>
                  <span className={`ef-model-badge ${cat.payment_model.toLowerCase()}`}>
                    <FontAwesomeIcon icon={faDollarSign} /> {cat.payment_model}
                  </span>
                  <span className="ef-category-name">
                    {cat.offer_name}
                    {cat.internal_id && <span style={{ color: 'rgba(255,255,255,0.3)', marginLeft: 6, fontSize: 11 }}>({cat.internal_id})</span>}
                  </span>
                  <span className="ef-category-count">{cat.creatives.length} creative{cat.creatives.length !== 1 ? 's' : ''}</span>
                  {cat.requires_manual_link && (
                    <span className="ef-manual-badge">
                      <FontAwesomeIcon icon={faExclamationTriangle} /> Manual Link
                    </span>
                  )}
                  <FontAwesomeIcon icon={isExpanded ? faChevronUp : faChevronDown} style={{ color: 'rgba(255,255,255,0.3)' }} />
                </div>
                {isExpanded && (
                  <div className="ef-creatives-list">
                    {cat.creatives.map(creative => {
                      const isSelected = selectedCreative?.creative_id === creative.creative_id;
                      return (
                        <div
                          key={creative.creative_id}
                          className={`ef-creative-item ${isSelected ? 'selected' : ''}`}
                          onClick={() => {
                            setSelectedCreative(creative);
                            setSelectedCategory(cat);
                            setTrackingLink('');
                            setManualLink('');
                            setError('');
                          }}
                        >
                          <FontAwesomeIcon icon={faTag} style={{ color: 'rgba(255,255,255,0.3)', fontSize: 12 }} />
                          <span className="ef-creative-name">{creative.name}</span>
                          <span className="ef-creative-id">ID: {creative.creative_id}</span>
                          <FontAwesomeIcon
                            icon={faLink}
                            className={`ef-link-icon ${creative.has_tracking_link ? '' : 'missing'}`}
                            title={creative.has_tracking_link ? 'Has tracking link placeholder' : 'No tracking link placeholder'}
                          />
                          {rawCreatives[creative.creative_id] && (
                            <button
                              className="ef-btn ef-btn-outline"
                              style={{ padding: '2px 8px', fontSize: 11 }}
                              onClick={e => {
                                e.stopPropagation();
                                setPreviewHtml(rawCreatives[creative.creative_id]);
                                setShowPreview(true);
                              }}
                            >
                              <FontAwesomeIcon icon={faEye} /> Preview
                            </button>
                          )}
                        </div>
                      );
                    })}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      ) : !loading ? (
        <div className="ef-empty">
          <FontAwesomeIcon icon={faSearch} style={{ fontSize: 24, marginBottom: 8 }} /><br />
          Search for creatives to get started
        </div>
      ) : null}

      {/* Selection Summary */}
      {selectedCreative && selectedCategory && (
        <div className="ef-selection-summary">
          <strong>Selected:</strong> {selectedCreative.name} (ID: {selectedCreative.creative_id})
          &nbsp;|&nbsp;
          <strong>Offer:</strong> {selectedCategory.offer_name} ({selectedCategory.payment_model})
          &nbsp;|&nbsp;
          <strong>Offer ID:</strong> {selectedCategory.offer_id}
        </div>
      )}

      {/* Tracking Link Builder */}
      {selectedCreative && selectedCategory && (
        <div className="ef-link-builder">
          {selectedCategory.requires_manual_link ? (
            <>
              <div className="ef-manual-warning">
                <div className="ef-warn-title">
                  <FontAwesomeIcon icon={faExclamationTriangle} />
                  Manual Tracking Link Required
                </div>
                <p style={{ margin: 0 }}>
                  <strong>{selectedCategory.offer_name}</strong> uses creative-specific tracking links.
                  These cannot be auto-generated. Please paste the tracking link provided by the network below.
                </p>
              </div>
              <div className="ef-link-row">
                <label><FontAwesomeIcon icon={faPaste} /> Tracking Link</label>
                <input
                  type="text"
                  value={manualLink}
                  onChange={e => setManualLink(e.target.value)}
                  placeholder="Paste the creative-specific tracking link here..."
                />
              </div>
            </>
          ) : (
            <>
              <h4><FontAwesomeIcon icon={faLink} /> Build Tracking Link</h4>
              <div className="ef-link-row">
                <label>Affiliate</label>
                <select value={selectedAffiliate} onChange={e => { setSelectedAffiliate(e.target.value); setTrackingLink(''); }}>
                  {affiliates.map(a => (
                    <option key={a.affiliate_id} value={a.affiliate_id}>
                      {a.affiliate_name || `Affiliate ${a.affiliate_id}`} ({a.affiliate_id} → {a.encoded_value})
                    </option>
                  ))}
                </select>
              </div>
              <div className="ef-link-row">
                <label>Offer ID</label>
                <input type="text" value={String(selectedCategory.offer_id)} disabled />
              </div>
              <div className="ef-link-row">
                <label>Creative ID</label>
                <input type="text" value={String(selectedCreative.creative_id)} disabled />
              </div>
              <div className="ef-actions">
                <button className="ef-btn ef-btn-primary" onClick={buildTrackingLink} disabled={buildingLink || !selectedAffiliate}>
                  <FontAwesomeIcon icon={buildingLink ? faSpinner : faLink} spin={buildingLink} />
                  {buildingLink ? 'Building...' : 'Build Tracking Link'}
                </button>
              </div>
            </>
          )}

          {trackingLink && (
            <div className="ef-tracking-link-display">
              <FontAwesomeIcon icon={faCheckCircle} /> {trackingLink}
              {brandedDomainInfo.used && (
                <div style={{
                  marginTop: 6,
                  fontSize: 11,
                  color: '#00b894',
                  display: 'flex',
                  alignItems: 'center',
                  gap: 6,
                }}>
                  <FontAwesomeIcon icon={faCheckCircle} />
                  Branded domain active: <strong>{brandedDomainInfo.domain}</strong>
                </div>
              )}
              {!brandedDomainInfo.used && brandedDomainInfo.original && (
                <div style={{
                  marginTop: 6,
                  fontSize: 11,
                  color: '#fdcb6e',
                }}>
                  Using default domain. Set up a branded tracking domain in Tracking Domains tab for better deliverability.
                </div>
              )}
            </div>
          )}

          {(trackingLink || (selectedCategory.requires_manual_link && manualLink)) && (
            <div className="ef-actions">
              <button
                className="ef-btn ef-btn-success"
                onClick={handleApplyCreative}
                disabled={rehostingImages}
              >
                <FontAwesomeIcon icon={faCheckCircle} spin={rehostingImages} />
                {rehostingImages ? 'Rehosting Images to CDN...' : 'Apply Creative & Tracking Link to Campaign'}
              </button>
              <button
                className="ef-btn ef-btn-outline"
                onClick={() => {
                  const link = selectedCategory.requires_manual_link ? manualLink : trackingLink;
                  navigator.clipboard.writeText(link);
                }}
              >
                <FontAwesomeIcon icon={faCopy} /> Copy Link
              </button>
              {rehostStatus && (
                <div style={{
                  marginTop: 8,
                  padding: '6px 12px',
                  borderRadius: 6,
                  fontSize: 12,
                  background: rehostStatus.includes('failed') ? '#fff3cd' : '#d4edda',
                  color: rehostStatus.includes('failed') ? '#856404' : '#155724',
                  border: `1px solid ${rehostStatus.includes('failed') ? '#ffc107' : '#28a745'}`,
                }}>
                  {rehostStatus}
                </div>
              )}
            </div>
          )}
        </div>
      )}

      {/* Preview Modal */}
      {showPreview && (
        <div className="ef-preview-overlay" onClick={() => setShowPreview(false)}>
          <div className="ef-preview-modal" onClick={e => e.stopPropagation()}>
            <div className="ef-preview-header">
              <h4>Creative Preview</h4>
              <button className="ef-preview-close" onClick={() => setShowPreview(false)}>&times;</button>
            </div>
            <div className="ef-preview-body">
              <iframe
                srcDoc={previewHtml}
                title="Creative Preview"
                sandbox="allow-same-origin"
              />
            </div>
          </div>
        </div>
      )}
    </div>
  );
};

export default EverflowCreativeSelector;
