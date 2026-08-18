import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faCheckCircle, faExclamationTriangle, faSpinner, faEye } from '@fortawesome/free-solid-svg-icons';

/**
 * Offer + creative selection, bound to the Creative Studio offers library.
 *
 * Standing operator ruling: creative, subject, preheader and from-name ship ONLY
 * from Creative Studio — `mailing_offer_proofs` rows that are approved and
 * active. This panel is the whole content step: pick the approved proof, pick a
 * subject/preheader pair from its `variants[]`, pick a from-name from its
 * `from_names[]`. There is no free-form path, so an unapproved creative cannot
 * reach the queue through this wizard.
 *
 * The offer (`mailing_offers` UUID) is a separate control because there is no DB
 * link between a proof and an offer row — the offer id drives attribution and
 * offer/converted suppression at send time.
 *
 * TWO LIBRARIES, chosen by TRANSPORT — not operator preference:
 *   PMTA/SES  → mailing_offer_proofs. Offers. The 16 legacy brands.
 *   KumoMTA   → mailing_creatives. NEWSLETTERS, rebuilt fresh daily by
 *               agents/jobs/kumo_newsletter_stage.py. Warm-up content is
 *               editorial by design and OFFERS ARE BANNED IN IT
 *               (CLAUDE.md §13.1), so pointing a kumo lane at the offers
 *               library is a doctrine violation, not a preference. The offer
 *               control is therefore hidden on a kumo route.
 *
 * Newsletter mode reuses the existing creatives registry
 * (GET /creatives?brand=<apex>, GET /creatives/{id}/preview) rather than adding
 * a second reader over the same table.
 *
 * from-name on a kumo route comes from the SENDING PROFILE, never the creative
 * row — the same guard kumo_warm._clone_copy carries, because a creative-borne
 * from_email breaks DKIM alignment across domains.
 */

interface ProofVariant { subject: string; preheader?: string; preview_text?: string }

export interface OfferProof {
  id: string;
  name: string;
  offer_key: string;
  approval_status: string;
  is_active: boolean;
  variants: ProofVariant[] | null;
  from_names: string[] | null;
  approved_domains: string[] | null;
  approved_isps: string[] | null;
  html_content?: string;
  updated_at?: string;
}

export interface OfferOption { id: string; key?: string; name: string; status?: string }

export interface RegistryCreative {
  id: string;
  brand_code: string;
  subject: string;
  preheader: string;
  source: string;
  approval_status: string;
  html_bytes: number;
  updated_at?: string;
}

interface Props {
  apiBase: string;
  orgFetch: (url: string, opts?: RequestInit) => Promise<Response>;
  sendingDomain: string;
  brandRoot: string;
  offers: OfferOption[];
  offersError: string;
  selectedOfferId: string;
  onOfferChange: (id: string) => void;
  proofId: string;
  subject: string;
  preheader: string;
  fromName: string;
  hasHtml: boolean;
  /** Applies a whole content selection to the wizard's single variant. */
  onApply: (v: { proofId: string; proofName: string; subject: string; preheader: string; fromName: string; html: string }) => void;
  onFieldChange: (v: { subject?: string; preheader?: string; fromName?: string }) => void;
  profileFromName?: string;
  /** True when the pinned sending profile routes through KumoMTA. */
  isKumoRoute?: boolean;
}

const preheaderOf = (v: ProofVariant) => (v.preheader ?? v.preview_text ?? '');

/** A proof is offerable on this property when it names no domains at all, or
 *  names one that resolves to the same brand root (proofs record `em.<apex>`
 *  while the board mails `m.<apex>`). */
export function proofMatchesBrand(proof: OfferProof, brandRoot: string): boolean {
  const doms = proof.approved_domains || [];
  if (doms.length === 0) return true;
  if (!brandRoot) return true;
  const root = brandRoot.toLowerCase();
  return doms.some(d => {
    const dd = (d || '').toLowerCase().trim();
    return dd === root || dd.endsWith('.' + root);
  });
}

export const OfferCreativePicker: React.FC<Props> = ({
  apiBase, orgFetch, sendingDomain, brandRoot, offers, offersError,
  selectedOfferId, onOfferChange, proofId, subject, preheader, fromName, hasHtml,
  onApply, onFieldChange, profileFromName, isKumoRoute = false,
}) => {
  const [proofs, setProofs] = useState<OfferProof[]>([]);
  const [, setNewsletters] = useState<RegistryCreative[]>([]);  // list render pending (peer WIP); setter keeps fetch path alive
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [loadingProofId, setLoadingProofId] = useState('');
  const [showAll, setShowAll] = useState(false);
  const [previewOpen, setPreviewOpen] = useState(false);
  const [previewHtml, setPreviewHtml] = useState('');

  const load = useCallback(async () => {
    setLoading(true);
    setError('');
    try {
      if (isKumoRoute) {
        // Newsletter library. brand_code on a kumo creative is the APEX
        // ("bestcreditcare.com"), which is exactly kumo_estate.json's
        // creative_brand_code — not a short code.
        if (!brandRoot) { setNewsletters([]); return; }
        const res = await orgFetch(`${apiBase}/creatives?brand=${encodeURIComponent(brandRoot)}&limit=50`);
        const data = await res.json();
        if (!res.ok) { setError(data.error || `HTTP ${res.status}`); return; }
        const rows: RegistryCreative[] = Array.isArray(data.creatives) ? data.creatives : [];
        setNewsletters(rows.filter(c => c.approval_status === 'approved' && c.html_bytes > 0));
        return;
      }
      const res = await orgFetch(`${apiBase}/offer-proofs?status=approved&active=true`);
      const data = await res.json();
      if (!res.ok) { setError(data.error || `HTTP ${res.status}`); return; }
      setProofs(Array.isArray(data.proofs) ? data.proofs : []);
    } catch (e: any) {
      setError(e?.message || 'network error');
    } finally {
      setLoading(false);
    }
  }, [apiBase, orgFetch, isKumoRoute, brandRoot]);

  useEffect(() => { load(); }, [load]);

  const matching = useMemo(() => proofs.filter(p => proofMatchesBrand(p, brandRoot)), [proofs, brandRoot]);
  const visible = showAll ? proofs : matching;
  const selectedProof = proofs.find(p => p.id === proofId) || null;

  const selectProof = async (p: OfferProof) => {
    setLoadingProofId(p.id);
    setError('');
    try {
      const res = await orgFetch(`${apiBase}/offer-proofs/${p.id}`);
      const full: OfferProof = await res.json();
      if (!res.ok) { setError((full as any)?.error || `HTTP ${res.status}`); return; }
      const vs = full.variants || [];
      const fns = full.from_names || [];
      onApply({
        proofId: p.id,
        proofName: p.name,
        subject: vs.length > 0 ? vs[0].subject : '',
        preheader: vs.length > 0 ? preheaderOf(vs[0]) : '',
        fromName: fns.length > 0 ? fns[0] : (profileFromName || ''),
        html: full.html_content || '',
      });
      setProofs(prev => prev.map(x => (x.id === p.id ? { ...x, ...full } : x)));
    } catch (e: any) {
      setError(e?.message || 'network error');
    } finally {
      setLoadingProofId('');
    }
  };

  const selectNewsletter = async (c: RegistryCreative) => {
    setLoadingProofId(c.id);
    setError('');
    try {
      // The registry preview endpoint returns the raw HTML body.
      const res = await orgFetch(`${apiBase}/creatives/${c.id}/preview`);
      const html = await res.text();
      if (!res.ok) { setError(`HTTP ${res.status}`); return; }
      onApply({
        proofId: c.id,
        proofName: c.subject || c.brand_code,
        subject: c.subject || '',
        preheader: c.preheader || '',
        // DOMAIN persona, never the creative row — DKIM alignment.
        fromName: profileFromName || '',
        html,
      });
    } catch (e: any) {
      setError(e?.message || 'network error');
    } finally {
      setLoadingProofId('');
    }
  };

  const openPreview = async () => {
    if (!selectedProof) return;
    setPreviewHtml(selectedProof.html_content || '');
    setPreviewOpen(true);
  };

  const label: React.CSSProperties = { display: 'block', fontSize: 11, color: 'rgba(180,210,240,0.6)', marginBottom: 4 };
  const field: React.CSSProperties = {
    width: '100%', background: '#0a0f1a', color: '#e0e6f0',
    border: '1px solid rgba(0,200,255,0.08)', borderRadius: 8, padding: '8px 10px', fontSize: 13,
  };
  const box: React.CSSProperties = {
    background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)',
    borderRadius: 10, padding: 16, marginBottom: 16,
  };

  const proofVariants = selectedProof?.variants || [];
  const proofFromNames = selectedProof?.from_names || [];

  void selectNewsletter;  // wired by peer's pending newsletter picker UI
  return (
    <div>
      {/* Offer (attribution + suppression). Hidden on a kumo route: warm-up
          content is editorial and offers are banned in it, so there is no
          offer to attribute. */}
      {isKumoRoute ? (
        <div style={{ ...box, borderColor: 'rgba(56,189,248,0.35)', background: 'rgba(56,189,248,0.06)' }}>
          <h4 style={{ margin: '0 0 4px', fontSize: 14, color: '#38bdf8' }}>
            KumoMTA warm-up route &mdash; newsletter content
          </h4>
          <p style={{ margin: 0, fontSize: 12, color: 'rgba(180,210,240,0.7)' }}>
            This property warms on its own brand newsletters, rebuilt fresh each morning.
            <strong> Offers are banned in warm-up content</strong>, so the offer selector and the
            offers library are not available here. The creative comes from the newsletter library
            below, and the from-name from this domain&apos;s sending profile.
          </p>
        </div>
      ) : (
      <div style={box}>
        <h4 style={{ margin: '0 0 4px', fontSize: 14, color: '#e0e6f0' }}>Offer</h4>
        <p style={{ margin: '0 0 10px', fontSize: 12, color: 'rgba(180,210,240,0.55)' }}>
          Stamps the campaign for attribution and fires offer / converted suppression at send time.
        </p>
        {offersError && <div style={{ fontSize: 12, color: '#ef4444', marginBottom: 6 }}>{offersError}</div>}
        <select value={selectedOfferId} onChange={e => onOfferChange(e.target.value)} style={field}>
          <option value="">— no offer (attribution falls back to name inference) —</option>
          {offers.map(o => <option key={o.id} value={o.id}>{o.name}</option>)}
        </select>
      </div>
      )}

      {/* ── Approved creative ─────────────────────────────────────────── */}
      <div style={box}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', marginBottom: 4 }}>
          <h4 style={{ margin: 0, fontSize: 14, color: '#e0e6f0' }}>Creative — Creative Studio offers library</h4>
          <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)' }}>
            {matching.length} approved for {brandRoot || sendingDomain}
          </span>
        </div>
        <p style={{ margin: '0 0 10px', fontSize: 12, color: 'rgba(180,210,240,0.55)' }}>
          Approved, active proofs only. Subject, preheader and from-name come from the proof's
          approved pools — this wizard has no free-form content path.
        </p>

        {loading && <div style={{ fontSize: 13, color: 'rgba(180,210,240,0.7)' }}><FontAwesomeIcon icon={faSpinner} spin /> Loading approved creatives…</div>}
        {error && (
          <div style={{ fontSize: 12, color: '#ef4444', marginBottom: 8 }}>
            <FontAwesomeIcon icon={faExclamationTriangle} /> {error}{' '}
            <button onClick={load} style={{ marginLeft: 6, background: 'transparent', color: '#00b0ff', border: 'none', cursor: 'pointer', fontSize: 12 }}>retry</button>
          </div>
        )}

        {!loading && visible.length === 0 && (
          <div style={{ fontSize: 13, color: '#f59e0b', padding: '8px 0' }}>
            <FontAwesomeIcon icon={faExclamationTriangle} /> No approved creative is cleared for{' '}
            <strong>{brandRoot || sendingDomain}</strong>. Approve one in Creative Studio → Offers, or
            show all approved creatives below and confirm the domain is cleared.
          </div>
        )}

        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(240px, 1fr))', gap: 8 }}>
          {visible.map(p => {
            const selected = p.id === proofId;
            const cleared = proofMatchesBrand(p, brandRoot);
            return (
              <div key={p.id} role="button" tabIndex={0}
                   onClick={() => selectProof(p)}
                   onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); selectProof(p); } }}
                   style={{
                     padding: '10px 12px', cursor: 'pointer', borderRadius: 8,
                     background: selected ? 'rgba(139,92,246,0.12)' : '#0a0f1a',
                     border: `1.5px solid ${selected ? '#8b5cf6' : 'rgba(0,200,255,0.06)'}`,
                     opacity: cleared ? 1 : 0.6,
                   }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                  {selected && <FontAwesomeIcon icon={faCheckCircle} style={{ color: '#8b5cf6', fontSize: 12 }} />}
                  {loadingProofId === p.id && <FontAwesomeIcon icon={faSpinner} spin style={{ fontSize: 11, color: '#00b0ff' }} />}
                  <span style={{ fontSize: 13, fontWeight: 600, color: '#e0e6f0' }}>{p.name}</span>
                </div>
                <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)', marginTop: 3 }}>
                  {(p.variants || []).length} subject{(p.variants || []).length === 1 ? '' : 's'} ·{' '}
                  {(p.from_names || []).length} from-name{(p.from_names || []).length === 1 ? '' : 's'}
                  {!cleared && <span style={{ color: '#f59e0b' }}> · not cleared for this property</span>}
                </div>
              </div>
            );
          })}
        </div>

        {proofs.length > matching.length && (
          <label style={{ display: 'flex', alignItems: 'center', gap: 6, marginTop: 10, cursor: 'pointer' }}>
            <input type="checkbox" checked={showAll} onChange={e => setShowAll(e.target.checked)} />
            <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.55)' }}>
              Show all {proofs.length} approved creatives, including those not cleared for this property
            </span>
          </label>
        )}
      </div>

      {/* ── Approved copy pools ───────────────────────────────────────── */}
      {selectedProof && (
        <div style={box}>
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', marginBottom: 10 }}>
            <h4 style={{ margin: 0, fontSize: 14, color: '#e0e6f0' }}>Approved copy — {selectedProof.name}</h4>
            <button onClick={openPreview} disabled={!hasHtml}
                    style={{ background: '#0a0f1a', color: hasHtml ? '#00b0ff' : 'rgba(180,210,240,0.3)', border: '1px solid rgba(0,200,255,0.15)', borderRadius: 6, padding: '4px 10px', fontSize: 12, cursor: hasHtml ? 'pointer' : 'default' }}>
              <FontAwesomeIcon icon={faEye} /> Preview
            </button>
          </div>

          <div style={{ marginBottom: 10 }}>
            <label style={label}>Subject + preheader (from the proof's approved variants)</label>
            {proofVariants.length === 0 ? (
              <div style={{ fontSize: 12, color: '#f59e0b' }}>
                <FontAwesomeIcon icon={faExclamationTriangle} /> This proof carries no approved subject
                variants — approve subjects on it in Creative Studio before scheduling.
              </div>
            ) : (
              <select
                value={subject}
                onChange={e => {
                  const v = proofVariants.find(x => x.subject === e.target.value);
                  onFieldChange({ subject: e.target.value, preheader: v ? preheaderOf(v) : '' });
                }}
                style={field}
              >
                <option value="">— select an approved subject —</option>
                {proofVariants.map((v, i) => <option key={i} value={v.subject}>{v.subject}</option>)}
              </select>
            )}
            {preheader && (
              <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)', marginTop: 5 }}>
                Preheader: {preheader}
              </div>
            )}
          </div>

          <div>
            <label style={label}>From name (from the proof's approved pool)</label>
            {proofFromNames.length === 0 ? (
              <>
                <input value={fromName} readOnly style={{ ...field, opacity: 0.75 }} />
                <div style={{ fontSize: 11, color: '#f59e0b', marginTop: 5 }}>
                  <FontAwesomeIcon icon={faExclamationTriangle} /> This proof carries no approved
                  from-names; falling back to the sending profile's persona
                  {profileFromName ? ` ("${profileFromName}")` : ''}.
                </div>
              </>
            ) : (
              <select value={fromName} onChange={e => onFieldChange({ fromName: e.target.value })} style={field}>
                <option value="">— select an approved from-name —</option>
                {proofFromNames.map((f, i) => <option key={i} value={f}>{f}</option>)}
              </select>
            )}
          </div>
        </div>
      )}

      {previewOpen && (
        <div onClick={() => setPreviewOpen(false)}
             style={{ position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.75)', zIndex: 9999, display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 24 }}>
          <div onClick={e => e.stopPropagation()}
               style={{ background: '#fff', borderRadius: 10, width: 'min(760px, 100%)', height: '85vh', overflow: 'hidden', display: 'flex', flexDirection: 'column' }}>
            <div style={{ padding: '8px 12px', background: '#0d1526', color: '#e0e6f0', fontSize: 12, display: 'flex', justifyContent: 'space-between' }}>
              <span>{selectedProof?.name}</span>
              <button onClick={() => setPreviewOpen(false)} style={{ background: 'transparent', border: 'none', color: '#00b0ff', cursor: 'pointer' }}>close</button>
            </div>
            <iframe title="creative preview" srcDoc={previewHtml} sandbox="" style={{ flex: 1, border: 'none', background: '#fff' }} />
          </div>
        </div>
      )}
    </div>
  );
};

export default OfferCreativePicker;
