import React, { useState, useEffect, useCallback } from 'react';
import './OfferManagement.css';

const PAGE_VERSION = '1.0';

// ═══════════════════════════════════════════════════════════════════════════
// TYPES
// ═══════════════════════════════════════════════════════════════════════════

interface TreeOffer {
  id: string;
  name: string;
  status: string;
  last_sent_at: string | null;
  total_conversions: number;
}

interface TreeBrand {
  brand_name: string;
  offers: TreeOffer[];
}

interface TreeVertical {
  id: string;
  name: string;
  sort_order: number;
  brands: TreeBrand[];
}

interface Offer {
  id: string;
  organization_id: string;
  vertical_id: string | null;
  brand_name: string;
  name: string;
  description: string | null;
  everflow_offer_id: string | null;
  everflow_creative_id: string | null;
  tracking_link_template: string | null;
  optizmo_link: string | null;
  web_property: string | null;
  landing_page_slug: string | null;
  landing_page_url: string | null;
  landing_page_html: string | null;
  original_html_creative: string | null;
  payout: number | null;
  payout_type: string | null;
  optizmo_status: string | null;
  optizmo_last_scrubbed_at: string | null;
  status: string;
  created_at: string;
  updated_at: string;
}

interface SubjectLine {
  id: string;
  offer_id: string;
  subject_line: string;
  status: string;
  performance_score: number | null;
  total_sends: number;
  total_opens: number;
  open_rate: number;
  created_at: string;
  updated_at: string;
}

interface FromName {
  id: string;
  offer_id: string;
  from_name: string;
  status: string;
  performance_score: number | null;
  total_sends: number;
  total_opens: number;
  complaint_rate: number;
  created_at: string;
  updated_at: string;
}

interface OfferCreative {
  id: string;
  offer_id: string;
  version: number;
  html_content: string | null;
  subject_line_id: string | null;
  from_name_id: string | null;
  status: string;
  approval_notes: string | null;
  total_sends: number;
  total_clicks: number;
  total_opens: number;
  total_conversions: number;
  click_rate: number;
  open_rate: number;
  created_at: string;
  updated_at: string;
}

interface Deployment {
  id: string;
  offer_id: string;
  campaign_id: string | null;
  creative_id: string | null;
  subject_line_id: string | null;
  from_name_id: string | null;
  audience_list_ids: string[];
  deployed_at: string | null;
  total_sent: number;
  total_conversions: number;
  revenue: number;
}

interface OfferPerformance {
  offer_id: string;
  total_sent: number;
  total_opens: number;
  total_clicks: number;
  total_conversions: number;
  revenue: number;
  epc: number;
  open_rate: number;
  click_rate: number;
  conversion_rate: number;
}

interface OptizmoStatus {
  optizmo_status: string | null;
  optizmo_last_scrubbed_at: string | null;
  jobs: OptizmoJob[];
}

interface OptizmoJob {
  id: string;
  status: string;
  error_message?: string;
  requested_at: string;
  completed_at: string | null;
  audience_count: number;
  suppressed_count: number;
}

type DetailTab = 'overview' | 'subjects' | 'from-names' | 'creatives' | 'landing-page' | 'compliance' | 'performance' | 'deploy';

const API = '/api/mailing/offer-center';

const WEB_PROPERTIES = [
  { key: 'discountblog', label: 'DiscountBlog (discountblog.com)' },
  { key: 'quizfiesta', label: 'QuizFiesta (quizfiesta.com)' },
  { key: 'historythinking', label: 'History Thinking (historythinking.com)' },
  { key: 'myownhealth', label: 'My Own Health (myownhealth.net)' },
];

const detailTabs: { id: DetailTab; label: string }[] = [
  { id: 'overview', label: 'Overview' },
  { id: 'subjects', label: 'Subjects' },
  { id: 'from-names', label: 'From Names' },
  { id: 'creatives', label: 'Creatives' },
  { id: 'landing-page', label: 'Landing Page' },
  { id: 'compliance', label: 'Compliance' },
  { id: 'performance', label: 'Performance' },
  { id: 'deploy', label: 'Deploy' },
];

// ═══════════════════════════════════════════════════════════════════════════
// STYLES
// ═══════════════════════════════════════════════════════════════════════════

const inputStyle: React.CSSProperties = {
  background: '#0a0f1a', border: '1px solid rgba(255,255,255,0.08)', borderRadius: 6,
  color: '#e0e6f0', padding: '8px 10px', fontSize: 13, width: '100%', boxSizing: 'border-box',
  fontFamily: 'inherit',
};

const btnPrimary: React.CSSProperties = {
  background: '#818cf8', color: '#fff', border: 'none', borderRadius: 6,
  padding: '7px 16px', fontSize: 12, fontWeight: 600, cursor: 'pointer', fontFamily: 'inherit',
};

const btnGhost: React.CSSProperties = {
  background: 'transparent', border: '1px solid rgba(255,255,255,0.1)', borderRadius: 6,
  padding: '5px 12px', fontSize: 11, cursor: 'pointer', color: 'rgba(255,255,255,0.6)',
  fontFamily: 'inherit',
};

const btnDanger: React.CSSProperties = {
  ...btnGhost, color: '#ef4444', borderColor: 'rgba(239,68,68,0.2)',
};

const sectionTitle: React.CSSProperties = {
  fontSize: 15, fontWeight: 700, color: '#e0e6f0', margin: '0 0 12px',
};

function statusColor(s: string): string {
  switch (s) {
    case 'active': case 'approved': case 'scrubbed': return '#22c55e';
    case 'draft': case 'pending': case 'scrub_pending': case 'processing': return '#f59e0b';
    case 'paused': case 'rejected': case 'failed': case 'scrub_failed': case 'cancelled': return '#ef4444';
    default: return '#94a3b8';
  }
}

function statusBadgeClass(s: string): string {
  return `offer-mgmt-badge ${s}`;
}

// ═══════════════════════════════════════════════════════════════════════════
// MAIN COMPONENT
// ═══════════════════════════════════════════════════════════════════════════

export const OfferManagement: React.FC = () => {
  const [tree, setTree] = useState<TreeVertical[]>([]);
  const [selectedOfferId, setSelectedOfferId] = useState<string | null>(null);
  const [offer, setOffer] = useState<Offer | null>(null);
  const [activeTab, setActiveTab] = useState<DetailTab>('overview');
  const [expandedVerticals, setExpandedVerticals] = useState<Set<string>>(new Set());
  const [expandedBrands, setExpandedBrands] = useState<Set<string>>(new Set());
  const [searchTerm, setSearchTerm] = useState('');
  const [showNewOfferModal, setShowNewOfferModal] = useState(false);
  const [showNewVerticalInput, setShowNewVerticalInput] = useState(false);
  const [newVerticalName, setNewVerticalName] = useState('');
  const [treeLoading, setTreeLoading] = useState(true);
  const [detailLoading, setDetailLoading] = useState(false);

  // Sub-tab data
  const [subjects, setSubjects] = useState<SubjectLine[]>([]);
  const [fromNames, setFromNames] = useState<FromName[]>([]);
  const [creatives, setCreatives] = useState<OfferCreative[]>([]);
  const [deployments, setDeployments] = useState<Deployment[]>([]);
  const [performance, setPerformance] = useState<OfferPerformance | null>(null);
  const [optizmoStatus, setOptizmoStatus] = useState<OptizmoStatus | null>(null);

  // ─── Data Fetching ─────────────────────────────────────────────────────

  const fetchTree = useCallback(async () => {
    try {
      const res = await fetch(`${API}/offers/tree`, { credentials: 'include' });
      if (res.ok) {
        const data = await res.json();
        const verts: TreeVertical[] = data.verticals || [];
        setTree(verts);
        if (expandedVerticals.size === 0) {
          const all = new Set(verts.map(v => v.id));
          setExpandedVerticals(all);
          const allBrands = new Set<string>();
          verts.forEach(v => v.brands.forEach(b => allBrands.add(`${v.id}::${b.brand_name}`)));
          setExpandedBrands(allBrands);
        }
      }
    } catch { /* swallow */ }
    setTreeLoading(false);
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const fetchOffer = useCallback(async (id: string) => {
    setDetailLoading(true);
    try {
      const res = await fetch(`${API}/offers/${id}`, { credentials: 'include' });
      if (res.ok) {
        const data = await res.json();
        setOffer(data.offer || data);
      }
    } catch { /* swallow */ }
    setDetailLoading(false);
  }, []);

  const fetchSubjects = useCallback(async (id: string) => {
    try {
      const res = await fetch(`${API}/offers/${id}/subjects`, { credentials: 'include' });
      if (res.ok) setSubjects(await res.json());
    } catch { /* swallow */ }
  }, []);

  const fetchFromNames = useCallback(async (id: string) => {
    try {
      const res = await fetch(`${API}/offers/${id}/from-names`, { credentials: 'include' });
      if (res.ok) setFromNames(await res.json());
    } catch { /* swallow */ }
  }, []);

  const fetchCreatives = useCallback(async (id: string) => {
    try {
      const res = await fetch(`${API}/offers/${id}/creatives`, { credentials: 'include' });
      if (res.ok) setCreatives(await res.json());
    } catch { /* swallow */ }
  }, []);

  const fetchDeployments = useCallback(async (id: string) => {
    try {
      const res = await fetch(`${API}/offers/${id}/deployments`, { credentials: 'include' });
      if (res.ok) setDeployments(await res.json());
    } catch { /* swallow */ }
  }, []);

  const fetchPerformance = useCallback(async (id: string) => {
    try {
      const res = await fetch(`${API}/offers/${id}/performance`, { credentials: 'include' });
      if (res.ok) setPerformance(await res.json());
    } catch { /* swallow */ }
  }, []);

  const fetchOptizmoStatus = useCallback(async (id: string) => {
    try {
      const res = await fetch(`${API}/offers/${id}/optizmo/status`, { credentials: 'include' });
      if (res.ok) setOptizmoStatus(await res.json());
    } catch { /* swallow */ }
  }, []);

  useEffect(() => { fetchTree(); }, [fetchTree]);

  useEffect(() => {
    if (!selectedOfferId) return;
    fetchOffer(selectedOfferId);
    setActiveTab('overview');
  }, [selectedOfferId, fetchOffer]);

  useEffect(() => {
    if (!selectedOfferId) return;
    switch (activeTab) {
      case 'subjects': fetchSubjects(selectedOfferId); break;
      case 'from-names': fetchFromNames(selectedOfferId); break;
      case 'creatives': fetchCreatives(selectedOfferId); break;
      case 'performance': fetchPerformance(selectedOfferId); fetchDeployments(selectedOfferId); break;
      case 'compliance': fetchOptizmoStatus(selectedOfferId); break;
      case 'deploy': fetchCreatives(selectedOfferId); fetchSubjects(selectedOfferId); fetchFromNames(selectedOfferId); break;
    }
  }, [activeTab, selectedOfferId, fetchSubjects, fetchFromNames, fetchCreatives, fetchDeployments, fetchPerformance, fetchOptizmoStatus]);

  // ─── Tree Actions ──────────────────────────────────────────────────────

  const toggleVertical = (id: string) => {
    setExpandedVerticals(prev => {
      const next = new Set(prev);
      next.has(id) ? next.delete(id) : next.add(id);
      return next;
    });
  };

  const toggleBrand = (key: string) => {
    setExpandedBrands(prev => {
      const next = new Set(prev);
      next.has(key) ? next.delete(key) : next.add(key);
      return next;
    });
  };

  const createVertical = async () => {
    if (!newVerticalName.trim()) return;
    try {
      const res = await fetch(`${API}/verticals`, {
        method: 'POST', credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: newVerticalName.trim(), sort_order: tree.length }),
      });
      if (res.ok) {
        setNewVerticalName('');
        setShowNewVerticalInput(false);
        fetchTree();
      }
    } catch { /* swallow */ }
  };

  // ─── Filtered Tree ─────────────────────────────────────────────────────

  const filteredTree = searchTerm.trim()
    ? tree.map(v => ({
        ...v,
        brands: v.brands.map(b => ({
          ...b,
          offers: b.offers.filter(o => o.name.toLowerCase().includes(searchTerm.toLowerCase())),
        })).filter(b => b.offers.length > 0),
      })).filter(v => v.brands.length > 0)
    : tree;

  // ─── Render: Folder Tree ───────────────────────────────────────────────

  const renderFolderTree = () => (
    <div className="offer-mgmt-tree">
      <div className="offer-mgmt-tree-header">
        <div style={{ fontSize: 14, fontWeight: 700, color: '#e0e6f0', marginBottom: 8 }}>Offers</div>
        <div style={{ display: 'flex', gap: 6 }}>
          <button style={btnPrimary} onClick={() => setShowNewOfferModal(true)}>+ Offer</button>
          <button style={btnGhost} onClick={() => setShowNewVerticalInput(true)}>+ Vertical</button>
        </div>
      </div>

      <div className="offer-mgmt-tree-body">
        {showNewVerticalInput && (
          <div style={{ padding: '6px 12px', display: 'flex', gap: 4 }}>
            <input
              autoFocus
              value={newVerticalName}
              onChange={e => setNewVerticalName(e.target.value)}
              onKeyDown={e => e.key === 'Enter' && createVertical()}
              placeholder="Vertical name"
              style={{ ...inputStyle, padding: '4px 8px', fontSize: 12 }}
            />
            <button style={{ ...btnPrimary, padding: '4px 8px', fontSize: 11 }} onClick={createVertical}>Add</button>
            <button style={{ ...btnGhost, padding: '4px 6px', fontSize: 11 }} onClick={() => setShowNewVerticalInput(false)}>×</button>
          </div>
        )}

        {treeLoading && (
          <div style={{ padding: 20, textAlign: 'center', color: 'rgba(255,255,255,0.4)', fontSize: 12 }}>Loading…</div>
        )}

        {filteredTree.map(v => {
          const isExpanded = expandedVerticals.has(v.id);
          return (
            <div key={v.id}>
              <div
                onClick={() => toggleVertical(v.id)}
                style={{
                  padding: '6px 12px', cursor: 'pointer', fontSize: 13, fontWeight: 600,
                  color: '#e0e6f0', display: 'flex', alignItems: 'center', gap: 6,
                  userSelect: 'none',
                }}
              >
                <span style={{ fontSize: 10, width: 12, color: 'rgba(255,255,255,0.4)' }}>
                  {isExpanded ? '▼' : '▶'}
                </span>
                {v.name}
              </div>

              {isExpanded && v.brands.map(b => {
                const brandKey = `${v.id}::${b.brand_name}`;
                const brandExpanded = expandedBrands.has(brandKey);
                return (
                  <div key={brandKey}>
                    <div
                      onClick={() => toggleBrand(brandKey)}
                      style={{
                        padding: '4px 12px 4px 28px', cursor: 'pointer', fontSize: 12,
                        color: 'rgba(255,255,255,0.65)', display: 'flex', alignItems: 'center', gap: 6,
                        userSelect: 'none',
                      }}
                    >
                      <span style={{ fontSize: 9, width: 10, color: 'rgba(255,255,255,0.3)' }}>
                        {brandExpanded ? '▼' : '▶'}
                      </span>
                      {b.brand_name}
                      <span style={{ marginLeft: 'auto', fontSize: 10, color: 'rgba(255,255,255,0.25)' }}>{b.offers.length}</span>
                    </div>

                    {brandExpanded && b.offers.map(o => (
                      <div
                        key={o.id}
                        onClick={() => setSelectedOfferId(o.id)}
                        style={{
                          padding: '4px 12px 4px 48px', cursor: 'pointer', fontSize: 12,
                          color: selectedOfferId === o.id ? '#818cf8' : 'rgba(255,255,255,0.55)',
                          background: selectedOfferId === o.id ? 'rgba(129,140,248,0.08)' : 'transparent',
                          display: 'flex', alignItems: 'center', gap: 6,
                          borderLeft: selectedOfferId === o.id ? '2px solid #818cf8' : '2px solid transparent',
                          transition: 'all 0.1s',
                        }}
                      >
                        <span style={{
                          width: 6, height: 6, borderRadius: '50%', flexShrink: 0,
                          background: statusColor(o.status),
                        }} />
                        <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{o.name}</span>
                      </div>
                    ))}
                  </div>
                );
              })}
            </div>
          );
        })}

        {!treeLoading && filteredTree.length === 0 && (
          <div style={{ padding: 20, textAlign: 'center', color: 'rgba(255,255,255,0.35)', fontSize: 12 }}>
            {searchTerm ? 'No matching offers' : 'No offers yet'}
          </div>
        )}
      </div>

      <div className="offer-mgmt-tree-footer">
        <input
          value={searchTerm}
          onChange={e => setSearchTerm(e.target.value)}
          placeholder="Search offers…"
          style={{ ...inputStyle, padding: '6px 8px', fontSize: 12 }}
        />
      </div>
    </div>
  );

  // ─── Render: Empty State ───────────────────────────────────────────────

  const renderEmptyState = () => (
    <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center', height: '100%', color: 'rgba(255,255,255,0.35)' }}>
      <div style={{ fontSize: 48, marginBottom: 16 }}>📦</div>
      <div style={{ fontSize: 16, fontWeight: 600, marginBottom: 6 }}>Select an offer</div>
      <div style={{ fontSize: 13 }}>Choose an offer from the left panel to view details</div>
    </div>
  );

  // ─── Render: Detail Panel ──────────────────────────────────────────────

  const renderDetail = () => {
    if (detailLoading || !offer) {
      return <div style={{ padding: 40, textAlign: 'center', color: 'rgba(255,255,255,0.4)' }}>Loading…</div>;
    }

    return (
      <div>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 }}>
          <div>
            <h2 style={{ margin: 0, fontSize: 20, fontWeight: 700, color: '#e0e6f0' }}>{offer.name}</h2>
            <div style={{ fontSize: 12, color: 'rgba(255,255,255,0.45)', marginTop: 2 }}>
              {offer.brand_name} · <span className={statusBadgeClass(offer.status)}>{offer.status}</span>
            </div>
          </div>
        </div>

        <div className="offer-mgmt-tabs">
          {detailTabs.map(t => (
            <button
              key={t.id}
              className={`offer-mgmt-tab ${activeTab === t.id ? 'active' : ''}`}
              onClick={() => setActiveTab(t.id)}
            >
              {t.label}
            </button>
          ))}
        </div>

        {activeTab === 'overview' && <OverviewTab offer={offer} onSave={(updated) => { setOffer(updated); fetchTree(); }} />}
        {activeTab === 'subjects' && <SubjectsTab offerId={offer.id} subjects={subjects} onRefresh={() => fetchSubjects(offer.id)} />}
        {activeTab === 'from-names' && <FromNamesTab offerId={offer.id} fromNames={fromNames} onRefresh={() => fetchFromNames(offer.id)} />}
        {activeTab === 'creatives' && <CreativesTab offerId={offer.id} creatives={creatives} onRefresh={() => fetchCreatives(offer.id)} />}
        {activeTab === 'landing-page' && <LandingPageTab offer={offer} onRefresh={() => selectedOfferId && fetchOffer(selectedOfferId)} />}
        {activeTab === 'compliance' && <ComplianceTab offerId={offer.id} optizmoStatus={optizmoStatus} onRefresh={() => fetchOptizmoStatus(offer.id)} />}
        {activeTab === 'performance' && <PerformanceTab performance={performance} deployments={deployments} />}
        {activeTab === 'deploy' && <DeployTab subjects={subjects} fromNames={fromNames} creatives={creatives} optizmoStatus={optizmoStatus} />}
      </div>
    );
  };

  // ─── Render: New Offer Modal ───────────────────────────────────────────

  const renderNewOfferModal = () => <NewOfferModal verticals={tree} onClose={() => setShowNewOfferModal(false)} onCreated={(id) => { setShowNewOfferModal(false); fetchTree(); setSelectedOfferId(id); }} />;

  // ─── Main Render ───────────────────────────────────────────────────────

  return (
    <div className="offer-mgmt-container">
      {renderFolderTree()}
      <div className="offer-mgmt-detail">
        {selectedOfferId && offer ? renderDetail() : renderEmptyState()}
      </div>
      {showNewOfferModal && renderNewOfferModal()}
      <div style={{ position: 'fixed', bottom: 8, right: 12, fontSize: 10, color: 'rgba(255,255,255,0.15)' }}>
        Offer Management v{PAGE_VERSION}
      </div>
    </div>
  );
};

// ═══════════════════════════════════════════════════════════════════════════
// TAB: OVERVIEW
// ═══════════════════════════════════════════════════════════════════════════

const OverviewTab: React.FC<{ offer: Offer; onSave: (o: Offer) => void }> = ({ offer, onSave }) => {
  const [form, setForm] = useState({ ...offer });
  const [saving, setSaving] = useState(false);

  useEffect(() => { setForm({ ...offer }); }, [offer]);

  const handleSave = async () => {
    setSaving(true);
    try {
      const res = await fetch(`${API}/offers/${offer.id}`, {
        method: 'PUT', credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: form.name,
          brand_name: form.brand_name,
          description: form.description,
          vertical_id: form.vertical_id,
          everflow_offer_id: form.everflow_offer_id,
          everflow_creative_id: form.everflow_creative_id,
          tracking_link_template: form.tracking_link_template,
          optizmo_link: form.optizmo_link,
          web_property: form.web_property,
          landing_page_slug: form.landing_page_slug,
          payout: form.payout,
          payout_type: form.payout_type,
          status: form.status,
        }),
      });
      if (res.ok) {
        const data = await res.json();
        onSave(data.offer || data);
      }
    } catch { /* swallow */ }
    setSaving(false);
  };

  const field = (label: string, key: keyof Offer, type: string = 'text') => (
    <div style={{ marginBottom: 10 }}>
      <label style={{ display: 'block', fontSize: 11, color: 'rgba(255,255,255,0.45)', marginBottom: 3 }}>{label}</label>
      <input
        type={type}
        value={(form[key] as string | number) ?? ''}
        onChange={e => setForm(prev => ({ ...prev, [key]: type === 'number' ? (e.target.value ? parseFloat(e.target.value) : null) : e.target.value }))}
        style={inputStyle}
      />
    </div>
  );

  return (
    <div>
      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 }}>
        {field('Offer Name', 'name')}
        {field('Brand Name', 'brand_name')}
        {field('Description', 'description')}
        <div style={{ marginBottom: 10 }}>
          <label style={{ display: 'block', fontSize: 11, color: 'rgba(255,255,255,0.45)', marginBottom: 3 }}>Web Property</label>
          <select
            value={form.web_property ?? ''}
            onChange={e => setForm(prev => ({ ...prev, web_property: e.target.value }))}
            style={inputStyle}
          >
            <option value="">Select web property…</option>
            {WEB_PROPERTIES.map(wp => <option key={wp.key} value={wp.key}>{wp.label}</option>)}
          </select>
        </div>
        {field('Everflow Offer ID', 'everflow_offer_id')}
        {field('Everflow Creative ID', 'everflow_creative_id')}
        {field('Tracking Link Template', 'tracking_link_template')}
        {field('Optizmo Link', 'optizmo_link')}
        {field('Landing Page Slug', 'landing_page_slug')}
        {field('Payout', 'payout', 'number')}
        <div style={{ marginBottom: 10 }}>
          <label style={{ display: 'block', fontSize: 11, color: 'rgba(255,255,255,0.45)', marginBottom: 3 }}>Payout Type</label>
          <select
            value={form.payout_type ?? ''}
            onChange={e => setForm(prev => ({ ...prev, payout_type: e.target.value }))}
            style={inputStyle}
          >
            <option value="">Select…</option>
            <option value="cpa">CPA</option>
            <option value="cpl">CPL</option>
            <option value="revshare">RevShare</option>
          </select>
        </div>
        <div style={{ marginBottom: 10 }}>
          <label style={{ display: 'block', fontSize: 11, color: 'rgba(255,255,255,0.45)', marginBottom: 3 }}>Status</label>
          <select
            value={form.status ?? 'draft'}
            onChange={e => setForm(prev => ({ ...prev, status: e.target.value }))}
            style={inputStyle}
          >
            <option value="draft">Draft</option>
            <option value="active">Active</option>
            <option value="paused">Paused</option>
          </select>
        </div>
      </div>
      <div style={{ marginTop: 16, textAlign: 'right' }}>
        <button style={{ ...btnPrimary, opacity: saving ? 0.6 : 1 }} onClick={handleSave} disabled={saving}>
          {saving ? 'Saving…' : 'Save Changes'}
        </button>
      </div>
    </div>
  );
};

// ═══════════════════════════════════════════════════════════════════════════
// TAB: SUBJECTS
// ═══════════════════════════════════════════════════════════════════════════

const SubjectsTab: React.FC<{ offerId: string; subjects: SubjectLine[]; onRefresh: () => void }> = ({ offerId, subjects, onRefresh }) => {
  const [newSubject, setNewSubject] = useState('');
  const [editId, setEditId] = useState<string | null>(null);
  const [editValue, setEditValue] = useState('');
  const [bulkAdding, setBulkAdding] = useState(false);

  const addSubject = async () => {
    if (!newSubject.trim()) return;
    const lines = newSubject.split('\n').map(l => l.trim()).filter(Boolean);
    setBulkAdding(true);
    try {
      for (const line of lines) {
        await fetch(`${API}/offers/${offerId}/subjects`, {
          method: 'POST', credentials: 'include',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ subject_line: line, status: 'draft' }),
        });
      }
      setNewSubject('');
      onRefresh();
    } catch { /* swallow */ }
    setBulkAdding(false);
  };

  const updateSubject = async (sid: string, payload: { subject_line?: string; status?: string }) => {
    try {
      const res = await fetch(`${API}/offers/${offerId}/subjects/${sid}`, {
        method: 'PUT', credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      if (res.ok) { setEditId(null); onRefresh(); }
    } catch { /* swallow */ }
  };

  const deleteSubject = async (sid: string) => {
    try {
      await fetch(`${API}/offers/${offerId}/subjects/${sid}`, { method: 'DELETE', credentials: 'include' });
      onRefresh();
    } catch { /* swallow */ }
  };

  return (
    <div>
      <div style={{ display: 'flex', gap: 8, marginBottom: 14, alignItems: 'flex-start' }}>
        <textarea
          value={newSubject}
          onChange={e => setNewSubject(e.target.value)}
          placeholder="Paste subject lines (one per line) or type a single one…"
          rows={newSubject.includes('\n') ? 4 : 1}
          style={{ ...inputStyle, flex: 1, resize: 'vertical', fontFamily: 'inherit' }}
        />
        <button style={btnPrimary} onClick={addSubject} disabled={bulkAdding}>
          {bulkAdding ? 'Adding…' : newSubject.includes('\n') ? `Add ${newSubject.split('\n').filter(l => l.trim()).length} Subjects` : 'Add Subject'}
        </button>
      </div>

      <table className="offer-mgmt-table">
        <thead>
          <tr>
            <th>Subject Line</th>
            <th>Status</th>
            <th>Sends</th>
            <th>Opens</th>
            <th>Open Rate</th>
            <th>Actions</th>
          </tr>
        </thead>
        <tbody>
          {subjects.map(s => (
            <tr key={s.id}>
              <td>
                {editId === s.id ? (
                  <div style={{ display: 'flex', gap: 4 }}>
                    <input value={editValue} onChange={e => setEditValue(e.target.value)} style={{ ...inputStyle, padding: '3px 6px', fontSize: 12 }} />
                    <button style={{ ...btnPrimary, padding: '3px 8px', fontSize: 11 }} onClick={() => updateSubject(s.id, { subject_line: editValue })}>Save</button>
                    <button style={{ ...btnGhost, padding: '3px 6px', fontSize: 11 }} onClick={() => setEditId(null)}>×</button>
                  </div>
                ) : (
                  <span onClick={() => { setEditId(s.id); setEditValue(s.subject_line); }} style={{ cursor: 'pointer' }}>
                    {s.subject_line}
                  </span>
                )}
              </td>
              <td><span className={statusBadgeClass(s.status)}>{s.status}</span></td>
              <td>{s.total_sends.toLocaleString()}</td>
              <td>{s.total_opens.toLocaleString()}</td>
              <td>{(s.open_rate * 100).toFixed(1)}%</td>
              <td>
                <div style={{ display: 'flex', gap: 4 }}>
                  {s.status !== 'approved' && (
                    <button style={{ ...btnGhost, color: '#22c55e', borderColor: 'rgba(34,197,94,0.2)' }} onClick={() => updateSubject(s.id, { status: 'approved' })}>Approve</button>
                  )}
                  {s.status !== 'rejected' && (
                    <button style={{ ...btnGhost, color: '#ef4444', borderColor: 'rgba(239,68,68,0.2)' }} onClick={() => updateSubject(s.id, { status: 'rejected' })}>Reject</button>
                  )}
                  <button style={btnDanger} onClick={() => deleteSubject(s.id)}>Delete</button>
                </div>
              </td>
            </tr>
          ))}
          {subjects.length === 0 && (
            <tr><td colSpan={6} style={{ textAlign: 'center', color: 'rgba(255,255,255,0.35)', padding: 24 }}>No subject lines yet</td></tr>
          )}
        </tbody>
      </table>
    </div>
  );
};

// ═══════════════════════════════════════════════════════════════════════════
// TAB: FROM NAMES
// ═══════════════════════════════════════════════════════════════════════════

const FromNamesTab: React.FC<{ offerId: string; fromNames: FromName[]; onRefresh: () => void }> = ({ offerId, fromNames, onRefresh }) => {
  const [newFromName, setNewFromName] = useState('');
  const [editId, setEditId] = useState<string | null>(null);
  const [editValue, setEditValue] = useState('');
  const [bulkAdding, setBulkAdding] = useState(false);

  const addFromName = async () => {
    if (!newFromName.trim()) return;
    const lines = newFromName.split('\n').map(l => l.trim()).filter(Boolean);
    setBulkAdding(true);
    try {
      for (const line of lines) {
        await fetch(`${API}/offers/${offerId}/from-names`, {
          method: 'POST', credentials: 'include',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ from_name: line, status: 'draft' }),
        });
      }
      setNewFromName('');
      onRefresh();
    } catch { /* swallow */ }
    setBulkAdding(false);
  };

  const updateFromName = async (fid: string, payload: { from_name?: string; status?: string }) => {
    try {
      const res = await fetch(`${API}/offers/${offerId}/from-names/${fid}`, {
        method: 'PUT', credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      if (res.ok) { setEditId(null); onRefresh(); }
    } catch { /* swallow */ }
  };

  const deleteFromName = async (fid: string) => {
    try {
      await fetch(`${API}/offers/${offerId}/from-names/${fid}`, { method: 'DELETE', credentials: 'include' });
      onRefresh();
    } catch { /* swallow */ }
  };

  return (
    <div>
      <div style={{ display: 'flex', gap: 8, marginBottom: 14, alignItems: 'flex-start' }}>
        <textarea
          value={newFromName}
          onChange={e => setNewFromName(e.target.value)}
          placeholder="Paste from names (one per line) or type a single one…"
          rows={newFromName.includes('\n') ? 4 : 1}
          style={{ ...inputStyle, flex: 1, resize: 'vertical', fontFamily: 'inherit' }}
        />
        <button style={btnPrimary} onClick={addFromName} disabled={bulkAdding}>
          {bulkAdding ? 'Adding…' : newFromName.includes('\n') ? `Add ${newFromName.split('\n').filter(l => l.trim()).length} Names` : 'Add From Name'}
        </button>
      </div>

      <table className="offer-mgmt-table">
        <thead>
          <tr>
            <th>From Name</th>
            <th>Status</th>
            <th>Sends</th>
            <th>Opens</th>
            <th>Complaint Rate</th>
            <th>Actions</th>
          </tr>
        </thead>
        <tbody>
          {fromNames.map(f => (
            <tr key={f.id}>
              <td>
                {editId === f.id ? (
                  <div style={{ display: 'flex', gap: 4 }}>
                    <input value={editValue} onChange={e => setEditValue(e.target.value)} style={{ ...inputStyle, padding: '3px 6px', fontSize: 12 }} />
                    <button style={{ ...btnPrimary, padding: '3px 8px', fontSize: 11 }} onClick={() => updateFromName(f.id, { from_name: editValue })}>Save</button>
                    <button style={{ ...btnGhost, padding: '3px 6px', fontSize: 11 }} onClick={() => setEditId(null)}>×</button>
                  </div>
                ) : (
                  <span onClick={() => { setEditId(f.id); setEditValue(f.from_name); }} style={{ cursor: 'pointer' }}>
                    {f.from_name}
                  </span>
                )}
              </td>
              <td><span className={statusBadgeClass(f.status)}>{f.status}</span></td>
              <td>{f.total_sends.toLocaleString()}</td>
              <td>{f.total_opens.toLocaleString()}</td>
              <td style={{ color: f.complaint_rate > 0.001 ? '#ef4444' : undefined }}>{(f.complaint_rate * 100).toFixed(3)}%</td>
              <td>
                <div style={{ display: 'flex', gap: 4 }}>
                  {f.status !== 'approved' && (
                    <button style={{ ...btnGhost, color: '#22c55e', borderColor: 'rgba(34,197,94,0.2)' }} onClick={() => updateFromName(f.id, { status: 'approved' })}>Approve</button>
                  )}
                  {f.status !== 'rejected' && (
                    <button style={{ ...btnGhost, color: '#ef4444', borderColor: 'rgba(239,68,68,0.2)' }} onClick={() => updateFromName(f.id, { status: 'rejected' })}>Reject</button>
                  )}
                  <button style={btnDanger} onClick={() => deleteFromName(f.id)}>Delete</button>
                </div>
              </td>
            </tr>
          ))}
          {fromNames.length === 0 && (
            <tr><td colSpan={6} style={{ textAlign: 'center', color: 'rgba(255,255,255,0.35)', padding: 24 }}>No from names yet</td></tr>
          )}
        </tbody>
      </table>
    </div>
  );
};

// ═══════════════════════════════════════════════════════════════════════════
// TAB: CREATIVES
// ═══════════════════════════════════════════════════════════════════════════

const CreativesTab: React.FC<{ offerId: string; creatives: OfferCreative[]; onRefresh: () => void }> = ({ offerId, creatives, onRefresh }) => {
  const [showUpload, setShowUpload] = useState(false);
  const [htmlContent, setHtmlContent] = useState('');
  const [previewId, setPreviewId] = useState<string | null>(null);
  const [generating, setGenerating] = useState(false);
  const [genError, setGenError] = useState('');
  const [genResult, setGenResult] = useState('');

  const isGenerating = generating || creatives.some(c => c.status === 'generating');

  useEffect(() => {
    if (!isGenerating) return;
    const interval = setInterval(() => onRefresh(), 8000);
    return () => clearInterval(interval);
  }, [isGenerating, onRefresh]);

  useEffect(() => {
    if (generating && !creatives.some(c => c.status === 'generating') && creatives.some(c => c.status === 'generated')) {
      const genCount = creatives.filter(c => c.status === 'generated').length;
      setGenResult(`Generated ${genCount} creatives`);
      setGenerating(false);
    }
    if (generating && creatives.some(c => c.status === 'failed' && c.approval_notes?.includes('Generation failed'))) {
      const failedNote = creatives.find(c => c.status === 'failed')?.approval_notes || 'Unknown error';
      setGenError(failedNote);
      setGenerating(false);
    }
  }, [creatives, generating]);

  const generateCreatives = async () => {
    setGenerating(true);
    setGenError('');
    setGenResult('');
    try {
      const res = await fetch(`${API}/offers/${offerId}/creatives/generate`, {
        method: 'POST', credentials: 'include',
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        setGenError(data.error || `Generation failed (${res.status})`);
        setGenerating(false);
      }
    } catch {
      setGenError('Network error during generation');
      setGenerating(false);
    }
  };

  const uploadCreative = async () => {
    if (!htmlContent.trim()) return;
    try {
      const res = await fetch(`${API}/offers/${offerId}/creatives`, {
        method: 'POST', credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ html_content: htmlContent, status: 'draft' }),
      });
      if (res.ok) { setHtmlContent(''); setShowUpload(false); onRefresh(); }
    } catch { /* swallow */ }
  };

  const updateCreativeStatus = async (cid: string, status: string) => {
    try {
      const res = await fetch(`${API}/offers/${offerId}/creatives/${cid}`, {
        method: 'PUT', credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ status }),
      });
      if (res.ok) onRefresh();
    } catch { /* swallow */ }
  };

  return (
    <div>
      <div style={{ display: 'flex', gap: 8, marginBottom: 14, alignItems: 'center' }}>
        <button
          style={{ ...btnPrimary, background: isGenerating ? '#6366f1' : '#818cf8', opacity: isGenerating ? 0.7 : 1, display: 'flex', alignItems: 'center', gap: 6 }}
          onClick={generateCreatives}
          disabled={isGenerating}
        >
          {isGenerating ? 'Generating ~10 Creatives…' : 'Generate Creatives'}
        </button>
        <button style={btnGhost} onClick={() => setShowUpload(!showUpload)}>
          {showUpload ? 'Cancel' : 'Upload Manual'}
        </button>
        {isGenerating && (
          <span style={{ fontSize: 11, color: '#f59e0b' }}>AI is crafting your emails — this takes 1-2 minutes</span>
        )}
        {genResult && !isGenerating && (
          <span style={{ fontSize: 11, color: '#22c55e' }}>{genResult}</span>
        )}
      </div>

      {genError && (
        <div style={{ padding: 10, background: 'rgba(239,68,68,0.08)', border: '1px solid rgba(239,68,68,0.2)', borderRadius: 8, fontSize: 12, color: '#ef4444', marginBottom: 12 }}>
          {genError}
        </div>
      )}

      {showUpload && (
        <div style={{ background: '#0d1526', border: '1px solid rgba(255,255,255,0.06)', borderRadius: 10, padding: 14, marginBottom: 14 }}>
          <label style={{ fontSize: 11, color: 'rgba(255,255,255,0.45)', display: 'block', marginBottom: 4 }}>HTML Content</label>
          <textarea
            value={htmlContent}
            onChange={e => setHtmlContent(e.target.value)}
            rows={8}
            style={{ ...inputStyle, fontFamily: 'monospace', fontSize: 12, resize: 'vertical' }}
          />
          <div style={{ marginTop: 8, textAlign: 'right' }}>
            <button style={btnPrimary} onClick={uploadCreative}>Upload</button>
          </div>
        </div>
      )}

      <div className="offer-mgmt-cards">
        {creatives.filter(c => c.status !== 'generating').map(c => (
          <div key={c.id} className="offer-mgmt-card">
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 4 }}>
              <span style={{ fontSize: 12, fontWeight: 600, color: '#e0e6f0' }}>v{c.version}</span>
              <span className={statusBadgeClass(c.status)}>{c.status}</span>
            </div>
            {c.approval_notes && (
              <div style={{ fontSize: 11, color: 'rgba(255,255,255,0.5)', marginBottom: 8, lineHeight: 1.3 }}>{c.approval_notes}</div>
            )}

            {c.html_content && (
              <div style={{ background: '#0a0f1a', borderRadius: 6, overflow: 'hidden', marginBottom: 8 }}>
                {previewId === c.id ? (
                  <iframe
                    srcDoc={c.html_content}
                    title={`Creative v${c.version}`}
                    style={{ width: '100%', height: 300, border: 'none' }}
                    sandbox="allow-same-origin"
                  />
                ) : (
                  <iframe
                    srcDoc={c.html_content}
                    title={`Creative v${c.version}`}
                    style={{ width: '100%', height: 150, border: 'none', pointerEvents: 'none' }}
                    sandbox="allow-same-origin"
                  />
                )}
              </div>
            )}

            <div style={{ display: 'flex', gap: 12, fontSize: 11, color: 'rgba(255,255,255,0.45)', marginBottom: 8 }}>
              <span>Sends: {c.total_sends.toLocaleString()}</span>
              <span>Opens: {(c.open_rate * 100).toFixed(1)}%</span>
              <span>Clicks: {(c.click_rate * 100).toFixed(1)}%</span>
              <span>Conv: {c.total_conversions}</span>
            </div>

            <div style={{ display: 'flex', gap: 4 }}>
              <button style={btnGhost} onClick={() => setPreviewId(previewId === c.id ? null : c.id)}>
                {previewId === c.id ? 'Collapse' : 'Preview'}
              </button>
              {c.status !== 'approved' && (
                <button style={{ ...btnGhost, color: '#22c55e', borderColor: 'rgba(34,197,94,0.2)' }} onClick={() => updateCreativeStatus(c.id, 'approved')}>Approve</button>
              )}
              {c.status !== 'rejected' && (
                <button style={{ ...btnGhost, color: '#ef4444', borderColor: 'rgba(239,68,68,0.2)' }} onClick={() => updateCreativeStatus(c.id, 'rejected')}>Reject</button>
              )}
            </div>
          </div>
        ))}
        {creatives.length === 0 && (
          <div style={{ gridColumn: '1 / -1', textAlign: 'center', color: 'rgba(255,255,255,0.35)', padding: 40 }}>No creatives yet</div>
        )}
      </div>
    </div>
  );
};

// ═══════════════════════════════════════════════════════════════════════════
// TAB: LANDING PAGE
// ═══════════════════════════════════════════════════════════════════════════

const LandingPageTab: React.FC<{ offer: Offer; onRefresh: () => void }> = ({ offer, onRefresh }) => {
  const [generating, setGenerating] = useState(false);
  const [republishing, setRepublishing] = useState(false);
  const [error, setError] = useState('');

  const generateLandingPage = async () => {
    setGenerating(true);
    setError('');
    try {
      const res = await fetch(`${API}/offers/${offer.id}/landing-page/generate`, {
        method: 'POST', credentials: 'include',
      });
      if (res.ok) { onRefresh(); }
      else { const data = await res.json().catch(() => ({})); setError(data.error || `Generation failed (${res.status})`); }
    } catch { setError('Network error during generation'); }
    setGenerating(false);
  };

  const republishLandingPage = async () => {
    setRepublishing(true);
    setError('');
    try {
      const res = await fetch(`${API}/offers/${offer.id}/landing-page/republish`, {
        method: 'POST', credentials: 'include',
      });
      if (res.ok) { onRefresh(); }
      else { const data = await res.json().catch(() => ({})); setError(data.error || `Republish failed (${res.status})`); }
    } catch { setError('Network error during republish'); }
    setRepublishing(false);
  };

  return (
    <div>
      <h3 style={sectionTitle}>Landing Page</h3>

      {error && (
        <div style={{ padding: 12, background: 'rgba(239,68,68,0.08)', border: '1px solid rgba(239,68,68,0.2)', borderRadius: 8, fontSize: 13, color: '#ef4444', marginBottom: 16 }}>
          {error}
        </div>
      )}

      {offer.landing_page_url ? (
        <div>
          <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 12 }}>
            <div>
              <span style={{ fontSize: 12, color: 'rgba(255,255,255,0.45)' }}>Live URL: </span>
              <a href={offer.landing_page_url} target="_blank" rel="noopener noreferrer" style={{ color: '#818cf8', fontSize: 13 }}>
                {offer.landing_page_url}
              </a>
            </div>
            <div style={{ marginLeft: 'auto', display: 'flex', gap: 8 }}>
              <button style={{ ...btnGhost, fontSize: 11 }} onClick={republishLandingPage} disabled={republishing}>
                {republishing ? 'Publishing…' : 'Republish'}
              </button>
              <button style={{ ...btnPrimary, fontSize: 11 }} onClick={generateLandingPage} disabled={generating}>
                {generating ? 'Generating…' : 'Regenerate'}
              </button>
            </div>
          </div>
          <div style={{ background: '#0a0f1a', borderRadius: 8, overflow: 'hidden', border: '1px solid rgba(255,255,255,0.06)' }}>
            <iframe
              src={offer.landing_page_url}
              title="Landing Page Preview"
              style={{ width: '100%', height: 500, border: 'none' }}
              sandbox="allow-same-origin allow-scripts"
            />
          </div>
        </div>
      ) : (
        <div style={{ textAlign: 'center', padding: 40, color: 'rgba(255,255,255,0.35)' }}>
          <div style={{ fontSize: 36, marginBottom: 12 }}>🌐</div>
          <div style={{ marginBottom: 16 }}>No landing page generated yet</div>
          <div style={{ display: 'flex', gap: 8, justifyContent: 'center' }}>
            {offer.landing_page_html && (
              <button style={{ ...btnGhost, opacity: republishing ? 0.6 : 1 }} onClick={republishLandingPage} disabled={republishing}>
                {republishing ? 'Publishing…' : 'Publish Stored Content'}
              </button>
            )}
            <button style={{ ...btnPrimary, opacity: generating ? 0.6 : 1 }} onClick={generateLandingPage} disabled={generating}>
              {generating ? 'Generating…' : 'Generate Landing Page'}
            </button>
          </div>
        </div>
      )}
    </div>
  );
};

// ═══════════════════════════════════════════════════════════════════════════
// TAB: COMPLIANCE (OPTIZMO)
// ═══════════════════════════════════════════════════════════════════════════

const ComplianceTab: React.FC<{ offerId: string; optizmoStatus: OptizmoStatus | null; onRefresh: () => void }> = ({ offerId, optizmoStatus, onRefresh }) => {
  const [requesting, setRequesting] = useState(false);
  const [cancelling, setCancelling] = useState(false);
  const [uploading, setUploading] = useState(false);
  const [uploadMsg, setUploadMsg] = useState('');

  useEffect(() => {
    const scrubStatus = optizmoStatus?.optizmo_status || 'not_scrubbed';
    if (scrubStatus !== 'scrub_pending') return;
    const interval = setInterval(onRefresh, 5000);
    return () => clearInterval(interval);
  }, [optizmoStatus?.optizmo_status, onRefresh]);

  const requestScrub = async () => {
    setRequesting(true);
    try {
      const res = await fetch(`${API}/offers/${offerId}/optizmo/request-scrub`, {
        method: 'POST', credentials: 'include',
      });
      if (res.ok) onRefresh();
    } catch { /* swallow */ }
    setRequesting(false);
  };

  const cancelScrub = async () => {
    setCancelling(true);
    try {
      const res = await fetch(`${API}/offers/${offerId}/optizmo/cancel-scrub`, {
        method: 'POST', credentials: 'include',
      });
      if (res.ok) onRefresh();
    } catch { /* swallow */ }
    setCancelling(false);
  };

  const handleFileUpload = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    setUploading(true);
    setUploadMsg('');
    try {
      const form = new FormData();
      form.append('file', file);
      const res = await fetch(`${API}/offers/${offerId}/optizmo/import-result`, {
        method: 'POST', credentials: 'include', body: form,
      });
      const data = await res.json();
      if (res.ok) {
        setUploadMsg(`Imported: ${(data.suppressed_count ?? 0).toLocaleString()} suppressed out of ${(data.hashes_uploaded ?? 0).toLocaleString()} hashes`);
        onRefresh();
      } else {
        setUploadMsg(`Error: ${data.error || 'upload failed'}`);
      }
    } catch {
      setUploadMsg('Network error during upload');
    }
    setUploading(false);
    e.target.value = '';
  };

  const scrubStatus = optizmoStatus?.optizmo_status || 'not_scrubbed';
  const isPending = scrubStatus === 'scrub_pending';
  const isFailed = scrubStatus === 'scrub_failed';
  const canCancel = isPending || isFailed;
  const canRequest = !isPending && scrubStatus !== 'scrubbed';

  return (
    <div>
      <h3 style={sectionTitle}>Optizmo Compliance</h3>

      <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 20, padding: 16, background: '#0d1526', borderRadius: 10, border: '1px solid rgba(255,255,255,0.06)' }}>
        <div>
          <div style={{ fontSize: 11, color: 'rgba(255,255,255,0.45)', marginBottom: 4 }}>Scrub Status</div>
          <span className={statusBadgeClass(scrubStatus)} style={{ fontSize: 13 }}>{scrubStatus.replace(/_/g, ' ')}</span>
        </div>
        {optizmoStatus?.optizmo_last_scrubbed_at && (
          <div style={{ marginLeft: 24 }}>
            <div style={{ fontSize: 11, color: 'rgba(255,255,255,0.45)', marginBottom: 4 }}>Last Scrubbed</div>
            <div style={{ fontSize: 13, color: '#e0e6f0' }}>{new Date(optizmoStatus.optizmo_last_scrubbed_at).toLocaleDateString()}</div>
          </div>
        )}
        <div style={{ marginLeft: 'auto', display: 'flex', gap: 8 }}>
          {canCancel && (
            <button
              style={{ ...btnPrimary, background: '#dc2626', opacity: cancelling ? 0.6 : 1 }}
              onClick={cancelScrub}
              disabled={cancelling}
            >
              {cancelling ? 'Cancelling…' : 'Cancel Scrub'}
            </button>
          )}
          {canRequest && (
            <button
              style={{ ...btnPrimary, opacity: requesting ? 0.6 : 1 }}
              onClick={requestScrub}
              disabled={requesting}
            >
              {requesting ? 'Requesting…' : isFailed ? 'Retry Scrub' : 'Request Scrub'}
            </button>
          )}
          {isPending && (
            <span style={{ fontSize: 12, color: '#f59e0b', alignSelf: 'center' }}>Processing…</span>
          )}
        </div>
      </div>

      {scrubStatus === 'scrubbed' && (
        <div style={{ padding: 12, background: 'rgba(34,197,94,0.08)', border: '1px solid rgba(34,197,94,0.2)', borderRadius: 8, fontSize: 13, color: '#22c55e', marginBottom: 16 }}>
          This offer has been scrubbed and is compliant for deployment
        </div>
      )}

      {scrubStatus === 'not_scrubbed' && (
        <div style={{ padding: 12, background: 'rgba(245,158,11,0.08)', border: '1px solid rgba(245,158,11,0.2)', borderRadius: 8, fontSize: 13, color: '#f59e0b', marginBottom: 16 }}>
          This offer has not been scrubbed — request a scrub before deploying
        </div>
      )}

      {isFailed && (
        <div style={{ padding: 12, background: 'rgba(239,68,68,0.08)', border: '1px solid rgba(239,68,68,0.2)', borderRadius: 8, fontSize: 13, color: '#ef4444', marginBottom: 16 }}>
          Scrub failed — {optizmoStatus?.jobs?.[0]?.error_message || 'check the Optizmo link and try again'}
        </div>
      )}

      <div style={{ marginBottom: 20, padding: 16, background: '#0d1526', borderRadius: 10, border: '1px solid rgba(255,255,255,0.06)' }}>
        <div style={{ fontSize: 12, color: 'rgba(255,255,255,0.55)', marginBottom: 8 }}>Manual Import (upload Optizmo suppression hash file)</div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          <input
            type="file"
            accept=".csv,.txt"
            onChange={handleFileUpload}
            disabled={uploading}
            style={{ fontSize: 12, color: '#e0e6f0' }}
          />
          {uploading && <span style={{ fontSize: 12, color: '#f59e0b' }}>Uploading…</span>}
        </div>
        {uploadMsg && (
          <div style={{ marginTop: 8, fontSize: 12, color: uploadMsg.startsWith('Error') ? '#ef4444' : '#22c55e' }}>{uploadMsg}</div>
        )}
      </div>

      {optizmoStatus?.jobs && optizmoStatus.jobs.length > 0 && (
        <div>
          <h4 style={{ fontSize: 13, fontWeight: 600, color: '#e0e6f0', marginBottom: 8 }}>Scrub History</h4>
          <table className="offer-mgmt-table">
            <thead>
              <tr>
                <th>Status</th>
                <th>Requested</th>
                <th>Completed</th>
                <th>Records</th>
                <th>Suppressed</th>
                <th>Error</th>
              </tr>
            </thead>
            <tbody>
              {optizmoStatus.jobs.map(j => (
                <tr key={j.id}>
                  <td><span className={statusBadgeClass(j.status)}>{j.status}</span></td>
                  <td>{new Date(j.requested_at).toLocaleDateString()}</td>
                  <td>{j.completed_at ? new Date(j.completed_at).toLocaleDateString() : '—'}</td>
                  <td>{(j.audience_count ?? 0).toLocaleString()}</td>
                  <td>{(j.suppressed_count ?? 0).toLocaleString()}</td>
                  <td style={{ fontSize: 11, color: '#ef4444', maxWidth: 200, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{j.error_message || '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
};

// ═══════════════════════════════════════════════════════════════════════════
// TAB: PERFORMANCE
// ═══════════════════════════════════════════════════════════════════════════

const PerformanceTab: React.FC<{ performance: OfferPerformance | null; deployments: Deployment[] }> = ({ performance, deployments }) => {
  if (!performance) {
    return <div style={{ padding: 40, textAlign: 'center', color: 'rgba(255,255,255,0.35)' }}>Loading performance data…</div>;
  }

  const metrics = [
    { label: 'Sent', value: performance.total_sent.toLocaleString(), color: '#818cf8' },
    { label: 'Opens', value: performance.total_opens.toLocaleString(), color: '#22c55e' },
    { label: 'Clicks', value: performance.total_clicks.toLocaleString(), color: '#f59e0b' },
    { label: 'Conversions', value: performance.total_conversions.toLocaleString(), color: '#3b82f6' },
    { label: 'Revenue', value: `$${performance.revenue.toFixed(2)}`, color: '#22c55e' },
    { label: 'EPC', value: `$${performance.epc.toFixed(2)}`, color: '#818cf8' },
  ];

  return (
    <div>
      <div className="offer-mgmt-metrics">
        {metrics.map(m => (
          <div key={m.label} className="offer-mgmt-metric">
            <div className="offer-mgmt-metric-value" style={{ color: m.color }}>{m.value}</div>
            <div className="offer-mgmt-metric-label">{m.label}</div>
          </div>
        ))}
      </div>

      <h4 style={{ fontSize: 13, fontWeight: 600, color: '#e0e6f0', marginBottom: 8 }}>Deployment History</h4>
      <table className="offer-mgmt-table">
        <thead>
          <tr>
            <th>Deployed</th>
            <th>Sent</th>
            <th>Conversions</th>
            <th>Revenue</th>
          </tr>
        </thead>
        <tbody>
          {deployments.map(d => (
            <tr key={d.id}>
              <td>{d.deployed_at ? new Date(d.deployed_at).toLocaleDateString() : '—'}</td>
              <td>{d.total_sent.toLocaleString()}</td>
              <td>{d.total_conversions.toLocaleString()}</td>
              <td>${d.revenue.toFixed(2)}</td>
            </tr>
          ))}
          {deployments.length === 0 && (
            <tr><td colSpan={4} style={{ textAlign: 'center', color: 'rgba(255,255,255,0.35)', padding: 24 }}>No deployments yet</td></tr>
          )}
        </tbody>
      </table>
    </div>
  );
};

// ═══════════════════════════════════════════════════════════════════════════
// TAB: DEPLOY
// ═══════════════════════════════════════════════════════════════════════════

const DeployTab: React.FC<{
  subjects: SubjectLine[];
  fromNames: FromName[];
  creatives: OfferCreative[];
  optizmoStatus: OptizmoStatus | null;
}> = ({ subjects, fromNames, creatives, optizmoStatus }) => {
  const [selectedCreative, setSelectedCreative] = useState('');
  const [selectedSubject, setSelectedSubject] = useState('');
  const [selectedFromName, setSelectedFromName] = useState('');

  const approvedSubjects = subjects.filter(s => s.status === 'approved');
  const approvedFromNames = fromNames.filter(f => f.status === 'approved');
  const approvedCreatives = creatives.filter(c => c.status === 'approved');
  const isCompliant = optizmoStatus?.optizmo_status === 'scrubbed';

  const canDeploy = selectedCreative && selectedSubject && selectedFromName && isCompliant;

  return (
    <div>
      <h3 style={sectionTitle}>Deploy Campaign</h3>

      <div style={{ display: 'grid', gap: 14, maxWidth: 500 }}>
        <div>
          <label style={{ display: 'block', fontSize: 11, color: 'rgba(255,255,255,0.45)', marginBottom: 4 }}>Creative</label>
          <select value={selectedCreative} onChange={e => setSelectedCreative(e.target.value)} style={inputStyle}>
            <option value="">Select approved creative…</option>
            {approvedCreatives.map(c => (
              <option key={c.id} value={c.id}>v{c.version} — {c.total_sends} sends, {(c.open_rate * 100).toFixed(1)}% open rate</option>
            ))}
          </select>
          {approvedCreatives.length === 0 && <div style={{ fontSize: 11, color: '#f59e0b', marginTop: 4 }}>No approved creatives — approve one in the Creatives tab</div>}
        </div>

        <div>
          <label style={{ display: 'block', fontSize: 11, color: 'rgba(255,255,255,0.45)', marginBottom: 4 }}>Subject Line</label>
          <select value={selectedSubject} onChange={e => setSelectedSubject(e.target.value)} style={inputStyle}>
            <option value="">Select approved subject…</option>
            {approvedSubjects.map(s => (
              <option key={s.id} value={s.id}>{s.subject_line} — {(s.open_rate * 100).toFixed(1)}% open rate</option>
            ))}
          </select>
          {approvedSubjects.length === 0 && <div style={{ fontSize: 11, color: '#f59e0b', marginTop: 4 }}>No approved subjects — approve one in the Subjects tab</div>}
        </div>

        <div>
          <label style={{ display: 'block', fontSize: 11, color: 'rgba(255,255,255,0.45)', marginBottom: 4 }}>From Name</label>
          <select value={selectedFromName} onChange={e => setSelectedFromName(e.target.value)} style={inputStyle}>
            <option value="">Select approved from name…</option>
            {approvedFromNames.map(f => (
              <option key={f.id} value={f.id}>{f.from_name} — {(f.complaint_rate * 100).toFixed(3)}% complaint rate</option>
            ))}
          </select>
          {approvedFromNames.length === 0 && <div style={{ fontSize: 11, color: '#f59e0b', marginTop: 4 }}>No approved from names — approve one in the From Names tab</div>}
        </div>

        <div style={{ padding: 12, background: isCompliant ? 'rgba(34,197,94,0.08)' : 'rgba(239,68,68,0.08)', border: `1px solid ${isCompliant ? 'rgba(34,197,94,0.2)' : 'rgba(239,68,68,0.2)'}`, borderRadius: 8 }}>
          <div style={{ fontSize: 13, color: isCompliant ? '#22c55e' : '#ef4444', fontWeight: 600 }}>
            {isCompliant ? '✓ Compliance gate passed' : '✗ Compliance gate failed — scrub required'}
          </div>
          <div style={{ fontSize: 11, color: 'rgba(255,255,255,0.45)', marginTop: 2 }}>
            Optizmo status: {optizmoStatus?.optizmo_status?.replace(/_/g, ' ') || 'unknown'}
          </div>
        </div>

        <button
          style={{ ...btnPrimary, padding: '12px 24px', fontSize: 14, opacity: canDeploy ? 1 : 0.4 }}
          disabled={!canDeploy}
          onClick={() => alert('Deployment wiring coming in a future phase. Selected creative, subject, and from name are ready.')}
        >
          Deploy Campaign
        </button>
      </div>
    </div>
  );
};

// ═══════════════════════════════════════════════════════════════════════════
// NEW OFFER MODAL
// ═══════════════════════════════════════════════════════════════════════════

const NewOfferModal: React.FC<{
  verticals: TreeVertical[];
  onClose: () => void;
  onCreated: (id: string) => void;
}> = ({ verticals, onClose, onCreated }) => {
  const [form, setForm] = useState({
    name: '', vertical_id: '', brand_name: '', web_property: '',
    everflow_offer_id: '', everflow_creative_id: '', tracking_link_template: '',
    optizmo_link: '', payout: '' as string, payout_type: 'cpa',
    original_html_creative: '', status: 'draft',
  });
  const [submitting, setSubmitting] = useState(false);

  const handleSubmit = async () => {
    if (!form.name.trim() || !form.brand_name.trim()) return;
    setSubmitting(true);
    try {
      const payload: Record<string, unknown> = { ...form };
      payload.payout = form.payout ? parseFloat(form.payout) : null;
      const res = await fetch(`${API}/offers`, {
        method: 'POST', credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      if (res.ok) {
        const data = await res.json();
        const newId = data.offer?.id || data.id;
        onCreated(newId);
      }
    } catch { /* swallow */ }
    setSubmitting(false);
  };

  const field = (label: string, key: string, opts?: { type?: string; placeholder?: string }) => (
    <div>
      <label style={{ display: 'block', fontSize: 11, color: 'rgba(255,255,255,0.45)', marginBottom: 3 }}>{label}</label>
      <input
        type={opts?.type || 'text'}
        value={(form as Record<string, string>)[key] ?? ''}
        onChange={e => setForm(prev => ({ ...prev, [key]: e.target.value }))}
        placeholder={opts?.placeholder || label}
        style={inputStyle}
      />
    </div>
  );

  return (
    <div className="offer-mgmt-modal-overlay" onClick={onClose}>
      <div className="offer-mgmt-modal" onClick={e => e.stopPropagation()}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
          <h3 style={{ margin: 0, fontSize: 18, fontWeight: 700, color: '#e0e6f0' }}>New Offer</h3>
          <button style={{ ...btnGhost, fontSize: 16, padding: '2px 8px' }} onClick={onClose}>×</button>
        </div>

        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 10, marginBottom: 12 }}>
          {field('Offer Name *', 'name')}
          <div>
            <label style={{ display: 'block', fontSize: 11, color: 'rgba(255,255,255,0.45)', marginBottom: 3 }}>Vertical</label>
            <select
              value={form.vertical_id}
              onChange={e => setForm(prev => ({ ...prev, vertical_id: e.target.value }))}
              style={inputStyle}
            >
              <option value="">Select vertical…</option>
              {verticals.map(v => <option key={v.id} value={v.id}>{v.name}</option>)}
            </select>
          </div>
          {field('Brand Name *', 'brand_name')}
          <div>
            <label style={{ display: 'block', fontSize: 11, color: 'rgba(255,255,255,0.45)', marginBottom: 3 }}>Web Property</label>
            <select
              value={form.web_property}
              onChange={e => setForm(prev => ({ ...prev, web_property: e.target.value }))}
              style={inputStyle}
            >
              <option value="">Select web property…</option>
              {WEB_PROPERTIES.map(wp => <option key={wp.key} value={wp.key}>{wp.label}</option>)}
            </select>
          </div>
          {field('Everflow Offer ID', 'everflow_offer_id')}
          {field('Everflow Creative ID', 'everflow_creative_id')}
          {field('Tracking Link Template', 'tracking_link_template')}
          {field('Optizmo Link', 'optizmo_link')}
          {field('Payout', 'payout', { type: 'number' })}
          <div>
            <label style={{ display: 'block', fontSize: 11, color: 'rgba(255,255,255,0.45)', marginBottom: 3 }}>Payout Type</label>
            <select value={form.payout_type} onChange={e => setForm(prev => ({ ...prev, payout_type: e.target.value }))} style={inputStyle}>
              <option value="cpa">CPA</option>
              <option value="cpl">CPL</option>
              <option value="revshare">RevShare</option>
            </select>
          </div>
        </div>

        <div style={{ marginBottom: 12 }}>
          <label style={{ display: 'block', fontSize: 11, color: 'rgba(255,255,255,0.45)', marginBottom: 3 }}>Raw HTML Creative</label>
          <textarea
            value={form.original_html_creative}
            onChange={e => setForm(prev => ({ ...prev, original_html_creative: e.target.value }))}
            rows={6}
            placeholder="Paste HTML creative here…"
            style={{ ...inputStyle, fontFamily: 'monospace', fontSize: 12, resize: 'vertical' }}
          />
        </div>

        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8 }}>
          <button style={btnGhost} onClick={onClose}>Cancel</button>
          <button
            style={{ ...btnPrimary, opacity: submitting || !form.name.trim() || !form.brand_name.trim() ? 0.5 : 1 }}
            onClick={handleSubmit}
            disabled={submitting || !form.name.trim() || !form.brand_name.trim()}
          >
            {submitting ? 'Creating…' : 'Create Offer'}
          </button>
        </div>
      </div>
    </div>
  );
};

export default OfferManagement;
