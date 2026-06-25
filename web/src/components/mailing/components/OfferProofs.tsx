import React, { useCallback, useEffect, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faUpload, faPaperPlane, faCircleCheck, faBan, faTrash, faUserPlus,
  faRotate, faToggleOn, faToggleOff, faXmark,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import { useToast } from '../shared/ToastSystem';

// Offer Proofs — Creative Studio "Offers" sub-view. Upload a network-approved
// HTML creative; the server rehosts its images + injects our footer/unsub. Email
// the proof to account managers via a chosen sending domain, then manually
// approve it — recording approved sending domains, approved ISPs, and the
// subject/preheader + from-name variants it may mail with. v1 is a registry.

interface ProofVariant { subject: string; preheader: string }

interface OfferProof {
  id: string;
  name: string;
  offer_key: string;
  html_content?: string;
  images_rehosted: number;
  approval_status: 'pending' | 'approved' | 'rejected';
  is_active: boolean;
  approved_by: string;
  approved_at?: string;
  variants: ProofVariant[];
  from_names: string[];
  approved_domains: string[];
  approved_isps: string[];
  created_at: string;
  updated_at: string;
}

interface ProofRecipient {
  id: string; name: string; email: string; is_active: boolean;
}

interface SendingDomain { domain: string; from_name?: string }

interface RehostDetail { url: string; outcome: string; reason?: string; cdn_url?: string }
interface RehostSummary { rehosted: number; cached: number; skipped: number; failed: number; details?: RehostDetail[] }

// rehostToast turns a rehost summary into a toast — and surfaces the first
// failure reason so a "0 rehosted" is never silent (download 404, S3 error, etc).
function rehostToast(rh: RehostSummary): { type: 'success' | 'warning'; title: string; message?: string } {
  const parts = [`${rh.rehosted} rehosted`];
  if (rh.cached) parts.push(`${rh.cached} cached`);
  if (rh.failed) parts.push(`${rh.failed} failed`);
  if (rh.skipped) parts.push(`${rh.skipped} skipped`);
  const firstFail = (rh.details ?? []).find((d) => d.outcome === 'failed');
  return {
    type: rh.failed > 0 ? 'warning' : 'success',
    title: `Images — ${parts.join(', ')}`,
    message: firstFail ? `${firstFail.url}: ${firstFail.reason}` : undefined,
  };
}

const badge = (fg: string): React.CSSProperties => ({
  display: 'inline-block', padding: '2px 8px', borderRadius: 999, fontSize: 11,
  fontWeight: 600, color: fg, background: `${fg}1f`, border: `1px solid ${fg}55`,
  whiteSpace: 'nowrap',
});
const statusColor = (s: OfferProof['approval_status']) =>
  s === 'approved' ? '#22c55e' : s === 'rejected' ? '#ef4444' : '#f59e0b';

const btn = (bg: string, border: string): React.CSSProperties => ({
  background: bg, border: `1px solid ${border}`, borderRadius: 6, color: '#e5e7eb',
  padding: '7px 12px', fontSize: 13, cursor: 'pointer', display: 'inline-flex',
  alignItems: 'center', gap: 6, fontWeight: 600,
});
const input: React.CSSProperties = {
  background: '#0f172a', border: '1px solid #334155', borderRadius: 6,
  color: '#e5e7eb', padding: '7px 10px', fontSize: 13, width: '100%', boxSizing: 'border-box',
};
const label: React.CSSProperties = {
  fontSize: 11, color: '#94a3b8', textTransform: 'uppercase', letterSpacing: 0.5,
  marginBottom: 4, display: 'block',
};
const card: React.CSSProperties = {
  background: '#0f172a', border: '1px solid #1f2937', borderRadius: 10, padding: 14,
};
const chip = (active: boolean): React.CSSProperties => ({
  background: active ? '#312e81' : '#1e293b',
  border: `1px solid ${active ? '#6366f1' : '#334155'}`,
  borderRadius: 999, color: active ? '#e0e7ff' : '#94a3b8',
  padding: '4px 11px', fontSize: 12, cursor: 'pointer', fontWeight: 600,
});

export const OfferProofs: React.FC = () => {
  const { addToast } = useToast();
  const [proofs, setProofs] = useState<OfferProof[]>([]);
  const [recipients, setRecipients] = useState<ProofRecipient[]>([]);
  const [domains, setDomains] = useState<SendingDomain[]>([]);
  const [isps, setIsps] = useState<string[]>([]);
  const [loading, setLoading] = useState(false);
  const [selected, setSelected] = useState<OfferProof | null>(null);
  const [previewHtml, setPreviewHtml] = useState<string>('');
  const [checked, setChecked] = useState<Record<string, boolean>>({});
  const [busy, setBusy] = useState(false);

  // upload form
  const [upName, setUpName] = useState('');
  const [upOffer, setUpOffer] = useState('');
  const [upHtml, setUpHtml] = useState('');

  // account-manager form
  const [amName, setAmName] = useState('');
  const [amEmail, setAmEmail] = useState('');

  // send form
  const [sendDomain, setSendDomain] = useState('');
  const [sendSubject, setSendSubject] = useState('');
  const [sendFromName, setSendFromName] = useState('');
  const [sendRcpts, setSendRcpts] = useState<Record<string, boolean>>({});

  const fetchProofs = useCallback(async () => {
    setLoading(true);
    try {
      const res = await apiFetch('/api/mailing/offer-proofs/');
      if (res.ok) setProofs((await res.json()).proofs ?? []);
    } finally { setLoading(false); }
  }, []);

  const fetchRecipients = useCallback(async () => {
    const res = await apiFetch('/api/mailing/proof-recipients/');
    if (res.ok) setRecipients((await res.json()).recipients ?? []);
  }, []);

  useEffect(() => {
    fetchProofs();
    fetchRecipients();
    (async () => {
      try {
        const d = await apiFetch('/api/mailing/pmta-campaign/sending-domains');
        if (d.ok) setDomains((await d.json()).domains ?? []);
      } catch { /* dropdown just stays empty */ }
      try {
        const i = await apiFetch('/api/mailing/offer-proofs/isps');
        if (i.ok) setIsps((await i.json()).isps ?? []);
      } catch { /* */ }
    })();
  }, [fetchProofs, fetchRecipients]);

  // Load full proof (with html_content) when a row is opened.
  const openProof = useCallback(async (id: string) => {
    const res = await apiFetch(`/api/mailing/offer-proofs/${id}`);
    if (res.ok) {
      const p: OfferProof = await res.json();
      setSelected(p);
      setSendSubject(p.variants[0]?.subject ?? '');
      setSendFromName(p.from_names[0] ?? '');
      setSendRcpts({});
      // Preview via the /preview endpoint so it shows the footer exactly as it
      // renders at send (bottom of the email, branded). apiFetch carries the org
      // header; an iframe src cannot.
      setPreviewHtml('');
      try {
        const pv = await apiFetch(`/api/mailing/offer-proofs/${id}/preview`);
        if (pv.ok) setPreviewHtml(await pv.text());
      } catch { /* fall back to html_content below */ }
    }
  }, []);

  const refreshSelected = useCallback(async () => {
    if (selected) await openProof(selected.id);
    await fetchProofs();
  }, [selected, openProof, fetchProofs]);

  // ── create ────────────────────────────────────────────────────────────────
  const createProof = useCallback(async () => {
    if (!upName.trim() || !upHtml.trim()) {
      addToast({ type: 'warning', title: 'Name and HTML required' });
      return;
    }
    setBusy(true);
    try {
      const res = await apiFetch('/api/mailing/offer-proofs/', {
        method: 'POST',
        body: JSON.stringify({ name: upName.trim(), offer_key: upOffer.trim(), html: upHtml }),
      });
      const json = await res.json();
      if (!res.ok) { addToast({ type: 'error', title: 'Upload failed', message: json.error }); return; }
      if (json.rehost) addToast(rehostToast(json.rehost as RehostSummary));
      setUpName(''); setUpOffer(''); setUpHtml('');
      await fetchProofs();
      if (json.proof) setSelected(json.proof);
    } catch (e) {
      addToast({ type: 'error', title: 'Upload failed', message: e instanceof Error ? e.message : String(e) });
    } finally { setBusy(false); }
  }, [upName, upOffer, upHtml, addToast, fetchProofs]);

  const onFile = useCallback((f: File | undefined) => {
    if (!f) return;
    const reader = new FileReader();
    reader.onload = () => {
      setUpHtml(String(reader.result ?? ''));
      if (!upName.trim()) setUpName(f.name.replace(/\.html?$/i, ''));
    };
    reader.readAsText(f);
  }, [upName]);

  // ── account managers ────────────────────────────────────────────────────
  const addRecipient = useCallback(async () => {
    if (!amName.trim() || !amEmail.trim()) { addToast({ type: 'warning', title: 'Name and email required' }); return; }
    const res = await apiFetch('/api/mailing/proof-recipients/', {
      method: 'POST', body: JSON.stringify({ name: amName.trim(), email: amEmail.trim() }),
    });
    const json = await res.json();
    if (!res.ok) { addToast({ type: 'error', title: 'Add failed', message: json.error }); return; }
    setAmName(''); setAmEmail('');
    fetchRecipients();
  }, [amName, amEmail, addToast, fetchRecipients]);

  const toggleRecipient = useCallback(async (rc: ProofRecipient) => {
    await apiFetch(`/api/mailing/proof-recipients/${rc.id}`, {
      method: 'PATCH', body: JSON.stringify({ is_active: !rc.is_active }),
    });
    fetchRecipients();
  }, [fetchRecipients]);

  const deleteRecipient = useCallback(async (rc: ProofRecipient) => {
    await apiFetch(`/api/mailing/proof-recipients/${rc.id}`, { method: 'DELETE' });
    fetchRecipients();
  }, [fetchRecipients]);

  // ── send ──────────────────────────────────────────────────────────────────
  const sendProof = useCallback(async () => {
    if (!selected) return;
    const ids = Object.keys(sendRcpts).filter((k) => sendRcpts[k]);
    if (!sendDomain) { addToast({ type: 'warning', title: 'Pick a sending domain' }); return; }
    if (ids.length === 0) { addToast({ type: 'warning', title: 'Select at least one account manager' }); return; }
    setBusy(true);
    try {
      const res = await apiFetch(`/api/mailing/offer-proofs/${selected.id}/send`, {
        method: 'POST',
        body: JSON.stringify({
          recipient_ids: ids, sending_domain: sendDomain,
          subject: sendSubject, from_name: sendFromName,
        }),
      });
      const json = await res.json();
      if (!res.ok) { addToast({ type: 'error', title: 'Send failed', message: json.error }); return; }
      addToast({
        type: json.failed > 0 ? 'warning' : 'success',
        title: `Proof sent — ${json.sent} ok, ${json.failed} failed`,
      });
    } catch (e) {
      addToast({ type: 'error', title: 'Send failed', message: e instanceof Error ? e.message : String(e) });
    } finally { setBusy(false); }
  }, [selected, sendRcpts, sendDomain, sendSubject, sendFromName, addToast]);

  // ── variants / from-names editing (saved on approve or via PATCH) ──────────
  const updateSelected = useCallback((patch: Partial<OfferProof>) => {
    setSelected((cur) => (cur ? { ...cur, ...patch } : cur));
  }, []);

  const saveVariants = useCallback(async () => {
    if (!selected) return;
    const res = await apiFetch(`/api/mailing/offer-proofs/${selected.id}`, {
      method: 'PATCH',
      body: JSON.stringify({ variants: selected.variants, from_names: selected.from_names }),
    });
    if (res.ok) { addToast({ type: 'success', title: 'Saved' }); refreshSelected(); }
    else { addToast({ type: 'error', title: 'Save failed', message: (await res.json()).error }); }
  }, [selected, addToast, refreshSelected]);

  // ── approve ────────────────────────────────────────────────────────────────
  const approveProof = useCallback(async () => {
    if (!selected) return;
    if (selected.approved_domains.length === 0 || selected.approved_isps.length === 0) {
      addToast({ type: 'warning', title: 'Select at least one domain and one ISP to approve' });
      return;
    }
    setBusy(true);
    try {
      const res = await apiFetch(`/api/mailing/offer-proofs/${selected.id}/approve`, {
        method: 'POST',
        body: JSON.stringify({
          approved_domains: selected.approved_domains,
          approved_isps: selected.approved_isps,
          variants: selected.variants,
          from_names: selected.from_names,
        }),
      });
      const json = await res.json();
      if (!res.ok) { addToast({ type: 'error', title: 'Approve failed', message: json.error }); return; }
      addToast({ type: 'success', title: `Approved — ${selected.name}` });
      refreshSelected();
    } finally { setBusy(false); }
  }, [selected, addToast, refreshSelected]);

  const rejectProof = useCallback(async () => {
    if (!selected) return;
    await apiFetch(`/api/mailing/offer-proofs/${selected.id}/reject`, { method: 'POST' });
    addToast({ type: 'info', title: 'Rejected' });
    refreshSelected();
  }, [selected, addToast, refreshSelected]);

  const rehostProof = useCallback(async () => {
    if (!selected) return;
    setBusy(true);
    try {
      const res = await apiFetch(`/api/mailing/offer-proofs/${selected.id}/rehost`, { method: 'POST' });
      const json = await res.json();
      if (!res.ok) { addToast({ type: 'error', title: 'Re-rehost failed', message: json.error }); return; }
      if (json.rehost) addToast(rehostToast(json.rehost as RehostSummary));
      if (json.proof) setSelected(json.proof);
      fetchProofs();
    } finally { setBusy(false); }
  }, [selected, addToast, fetchProofs]);

  const toggleActive = useCallback(async (p: OfferProof) => {
    await apiFetch(`/api/mailing/offer-proofs/${p.id}`, {
      method: 'PATCH', body: JSON.stringify({ is_active: !p.is_active }),
    });
    refreshSelected();
  }, [refreshSelected]);

  // ── bulk ────────────────────────────────────────────────────────────────
  const bulk = useCallback(async (action: 'delete' | 'activate' | 'deactivate') => {
    const ids = Object.keys(checked).filter((k) => checked[k]);
    if (ids.length === 0) return;
    if (action === 'delete' && !window.confirm(`Delete ${ids.length} proof(s)? This cannot be undone.`)) return;
    const res = await apiFetch('/api/mailing/offer-proofs/bulk', {
      method: 'POST', body: JSON.stringify({ ids, action }),
    });
    const json = await res.json();
    if (!res.ok) { addToast({ type: 'error', title: 'Bulk action failed', message: json.error }); return; }
    addToast({ type: 'success', title: `${action} — ${json.affected} affected` });
    setChecked({});
    if (action === 'delete' && selected && ids.includes(selected.id)) setSelected(null);
    fetchProofs();
  }, [checked, selected, addToast, fetchProofs]);

  const checkedCount = Object.values(checked).filter(Boolean).length;

  // ── variant row helpers ───────────────────────────────────────────────────
  const setVariant = (i: number, patch: Partial<ProofVariant>) => {
    if (!selected) return;
    const v = selected.variants.map((x, idx) => (idx === i ? { ...x, ...patch } : x));
    updateSelected({ variants: v });
  };
  const addVariant = () => selected && updateSelected({ variants: [...selected.variants, { subject: '', preheader: '' }] });
  const delVariant = (i: number) => selected && updateSelected({ variants: selected.variants.filter((_, idx) => idx !== i) });
  const setFromName = (i: number, val: string) => {
    if (!selected) return;
    updateSelected({ from_names: selected.from_names.map((x, idx) => (idx === i ? val : x)) });
  };
  const addFromName = () => selected && updateSelected({ from_names: [...selected.from_names, ''] });
  const delFromName = (i: number) => selected && updateSelected({ from_names: selected.from_names.filter((_, idx) => idx !== i) });

  const toggleApprovedDomain = (d: string) => {
    if (!selected) return;
    const has = selected.approved_domains.includes(d);
    updateSelected({ approved_domains: has ? selected.approved_domains.filter((x) => x !== d) : [...selected.approved_domains, d] });
  };
  const toggleApprovedISP = (isp: string) => {
    if (!selected) return;
    const has = selected.approved_isps.includes(isp);
    updateSelected({ approved_isps: has ? selected.approved_isps.filter((x) => x !== isp) : [...selected.approved_isps, isp] });
  };

  return (
    <div style={{ display: 'flex', gap: 16, flex: 1, minHeight: 0, width: '100%' }}>
      {/* ───────── left column: upload + AMs + list ───────── */}
      <div style={{ flex: '0 0 420px', overflowY: 'auto', display: 'flex', flexDirection: 'column', gap: 14, paddingRight: 4 }}>
        {/* upload */}
        <div style={card}>
          <div style={{ fontSize: 13, fontWeight: 600, marginBottom: 10 }}>
            <FontAwesomeIcon icon={faUpload} style={{ color: '#a78bfa' }} /> Upload network creative
          </div>
          <label style={label}>Proof name</label>
          <input style={input} value={upName} onChange={(e) => setUpName(e.target.value)} placeholder="e.g. Empire Loans — Jun spot" />
          <label style={{ ...label, marginTop: 8 }}>Offer key (optional)</label>
          <input style={input} value={upOffer} onChange={(e) => setUpOffer(e.target.value)} placeholder="e.g. empire-loans" />
          <label style={{ ...label, marginTop: 8 }}>HTML (paste or upload)</label>
          <textarea style={{ ...input, minHeight: 90, fontFamily: 'monospace', fontSize: 11 }}
            value={upHtml} onChange={(e) => setUpHtml(e.target.value)} placeholder="<html>…network approved creative…</html>" />
          <div style={{ display: 'flex', gap: 8, marginTop: 8, alignItems: 'center' }}>
            <label style={{ ...btn('#1e293b', '#334155'), cursor: 'pointer' }}>
              <FontAwesomeIcon icon={faUpload} /> Choose .html
              <input type="file" accept=".html,.htm,text/html" style={{ display: 'none' }}
                onChange={(e) => onFile(e.target.files?.[0])} />
            </label>
            <button style={btn('#312e81', '#4338ca')} disabled={busy} onClick={createProof}>
              <FontAwesomeIcon icon={faCircleCheck} /> Create proof
            </button>
          </div>
          <div style={{ fontSize: 11, color: '#64748b', marginTop: 6 }}>
            Images are rehosted to our CDN and our footer/unsubscribe is injected automatically.
          </div>
        </div>

        {/* account managers */}
        <div style={card}>
          <div style={{ fontSize: 13, fontWeight: 600, marginBottom: 10 }}>
            <FontAwesomeIcon icon={faUserPlus} style={{ color: '#a78bfa' }} /> Account managers
          </div>
          <div style={{ display: 'flex', gap: 6, marginBottom: 8 }}>
            <input style={{ ...input, flex: 1 }} value={amName} onChange={(e) => setAmName(e.target.value)} placeholder="Name" />
            <input style={{ ...input, flex: 1.4 }} value={amEmail} onChange={(e) => setAmEmail(e.target.value)} placeholder="email@network.com" />
            <button style={btn('#1e293b', '#334155')} onClick={addRecipient}><FontAwesomeIcon icon={faUserPlus} /></button>
          </div>
          {recipients.length === 0 && <div style={{ fontSize: 12, color: '#64748b' }}>No account managers yet.</div>}
          {recipients.map((rc) => (
            <div key={rc.id} style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '5px 0', borderTop: '1px solid #1f2937' }}>
              <span style={{ flex: 1, fontSize: 13, color: rc.is_active ? '#e5e7eb' : '#64748b' }}>
                {rc.name} <span style={{ color: '#64748b', fontSize: 11 }}>· {rc.email}</span>
              </span>
              <button title={rc.is_active ? 'active' : 'inactive'} style={{ ...btn('transparent', 'transparent'), padding: 4 }} onClick={() => toggleRecipient(rc)}>
                <FontAwesomeIcon icon={rc.is_active ? faToggleOn : faToggleOff} style={{ color: rc.is_active ? '#22c55e' : '#64748b' }} />
              </button>
              <button title="delete" style={{ ...btn('transparent', 'transparent'), padding: 4 }} onClick={() => deleteRecipient(rc)}>
                <FontAwesomeIcon icon={faTrash} style={{ color: '#ef4444' }} />
              </button>
            </div>
          ))}
        </div>

        {/* proofs list */}
        <div style={{ ...card, flex: 1 }}>
          <div style={{ display: 'flex', alignItems: 'center', marginBottom: 10 }}>
            <div style={{ fontSize: 13, fontWeight: 600, flex: 1 }}>Proofs ({proofs.length})</div>
            <button style={{ ...btn('#1e293b', '#334155'), padding: 4 }} onClick={fetchProofs} title="refresh">
              <FontAwesomeIcon icon={faRotate} spin={loading} />
            </button>
          </div>
          {checkedCount > 0 && (
            <div style={{ display: 'flex', gap: 6, marginBottom: 8 }}>
              <span style={{ fontSize: 12, color: '#94a3b8', alignSelf: 'center' }}>{checkedCount} selected:</span>
              <button style={btn('#1e293b', '#334155')} onClick={() => bulk('activate')}>Activate</button>
              <button style={btn('#1e293b', '#334155')} onClick={() => bulk('deactivate')}>Deactivate</button>
              <button style={btn('#3f1d1d', '#7f1d1d')} onClick={() => bulk('delete')}>Delete</button>
            </div>
          )}
          {proofs.length === 0 && <div style={{ fontSize: 12, color: '#64748b' }}>No proofs uploaded yet.</div>}
          {proofs.map((p) => (
            <div key={p.id} onClick={() => openProof(p.id)}
              style={{
                display: 'flex', alignItems: 'center', gap: 8, padding: '7px 6px', cursor: 'pointer',
                borderTop: '1px solid #1f2937',
                background: selected?.id === p.id ? '#1e1b4b' : 'transparent', borderRadius: 6,
              }}>
              <input type="checkbox" checked={!!checked[p.id]} onClick={(e) => e.stopPropagation()}
                onChange={(e) => setChecked((c) => ({ ...c, [p.id]: e.target.checked }))} />
              <div style={{ flex: 1, minWidth: 0 }}>
                <div style={{ fontSize: 13, fontWeight: 600, color: p.is_active ? '#e5e7eb' : '#64748b', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{p.name}</div>
                {p.offer_key && <div style={{ fontSize: 11, color: '#64748b' }}>{p.offer_key}</div>}
              </div>
              {!p.is_active && <span style={badge('#64748b')}>inactive</span>}
              <span style={badge(statusColor(p.approval_status))}>{p.approval_status}</span>
            </div>
          ))}
        </div>
      </div>

      {/* ───────── right column: detail ───────── */}
      <div style={{ flex: 1, minWidth: 0, overflowY: 'auto' }}>
        {!selected ? (
          <div style={{ color: '#64748b', fontSize: 13, padding: 24 }}>Select or upload a proof to manage it.</div>
        ) : (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
            {/* header */}
            <div style={{ ...card, display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
              <div style={{ flex: 1, minWidth: 0 }}>
                <div style={{ fontSize: 16, fontWeight: 700 }}>{selected.name}</div>
                <div style={{ fontSize: 12, color: '#64748b' }}>
                  {selected.offer_key || 'no offer key'} · {selected.images_rehosted} image(s) rehosted
                  {selected.approved_at ? ` · approved ${new Date(selected.approved_at).toLocaleString()}` : ''}
                </div>
              </div>
              <span style={badge(statusColor(selected.approval_status))}>{selected.approval_status}</span>
              <button style={btn('#1e293b', '#334155')} disabled={busy} onClick={rehostProof}
                title="Re-run image rehosting + footer on the stored creative">
                <FontAwesomeIcon icon={faRotate} /> Re-rehost images
              </button>
              <button style={btn('#1e293b', '#334155')} onClick={() => toggleActive(selected)}>
                <FontAwesomeIcon icon={selected.is_active ? faToggleOn : faToggleOff}
                  style={{ color: selected.is_active ? '#22c55e' : '#64748b' }} /> {selected.is_active ? 'Active' : 'Inactive'}
              </button>
            </div>

            <div style={{ display: 'flex', gap: 14, flexWrap: 'wrap' }}>
              {/* preview */}
              <div style={{ ...card, flex: '1 1 340px', minWidth: 0 }}>
                <div style={{ fontSize: 13, fontWeight: 600, marginBottom: 8 }}>Preview</div>
                <iframe title="proof-preview" srcDoc={previewHtml || selected.html_content || ''}
                  style={{ width: '100%', height: 360, border: '1px solid #1f2937', borderRadius: 6, background: '#fff' }} />
              </div>

              {/* variants + from names */}
              <div style={{ ...card, flex: '1 1 340px', minWidth: 0 }}>
                <div style={{ fontSize: 13, fontWeight: 600, marginBottom: 8 }}>Subject / preheader variants</div>
                {selected.variants.map((v, i) => (
                  <div key={i} style={{ display: 'flex', gap: 6, marginBottom: 6 }}>
                    <div style={{ flex: 1 }}>
                      <input style={input} value={v.subject} placeholder="Subject line"
                        onChange={(e) => setVariant(i, { subject: e.target.value })} />
                      <input style={{ ...input, marginTop: 4, fontSize: 12, color: '#94a3b8' }} value={v.preheader} placeholder="Preheader"
                        onChange={(e) => setVariant(i, { preheader: e.target.value })} />
                    </div>
                    <button style={{ ...btn('transparent', 'transparent'), padding: 4 }} onClick={() => delVariant(i)}>
                      <FontAwesomeIcon icon={faXmark} style={{ color: '#ef4444' }} />
                    </button>
                  </div>
                ))}
                <button style={{ ...btn('#1e293b', '#334155'), marginTop: 2 }} onClick={addVariant}>+ Add variant</button>

                <div style={{ fontSize: 13, fontWeight: 600, margin: '14px 0 8px' }}>From names</div>
                {selected.from_names.map((fn, i) => (
                  <div key={i} style={{ display: 'flex', gap: 6, marginBottom: 6 }}>
                    <input style={input} value={fn} placeholder="From name"
                      onChange={(e) => setFromName(i, e.target.value)} />
                    <button style={{ ...btn('transparent', 'transparent'), padding: 4 }} onClick={() => delFromName(i)}>
                      <FontAwesomeIcon icon={faXmark} style={{ color: '#ef4444' }} />
                    </button>
                  </div>
                ))}
                <button style={{ ...btn('#1e293b', '#334155'), marginTop: 2 }} onClick={addFromName}>+ Add from name</button>
                <div style={{ marginTop: 10 }}>
                  <button style={btn('#1e293b', '#334155')} onClick={saveVariants}>Save variants</button>
                </div>
              </div>
            </div>

            {/* send panel */}
            <div style={card}>
              <div style={{ fontSize: 13, fontWeight: 600, marginBottom: 10 }}>
                <FontAwesomeIcon icon={faPaperPlane} style={{ color: '#a78bfa' }} /> Send proof to account managers
              </div>
              <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap', marginBottom: 10 }}>
                <div style={{ flex: '1 1 200px' }}>
                  <label style={label}>Sending domain</label>
                  <select style={input as React.CSSProperties} value={sendDomain} onChange={(e) => setSendDomain(e.target.value)}>
                    <option value="">— choose domain —</option>
                    {domains.map((d) => <option key={d.domain} value={d.domain}>{d.domain}</option>)}
                  </select>
                </div>
                <div style={{ flex: '1 1 200px' }}>
                  <label style={label}>Subject</label>
                  <input style={input} value={sendSubject} onChange={(e) => setSendSubject(e.target.value)} placeholder="subject for the proof email" />
                </div>
                <div style={{ flex: '1 1 160px' }}>
                  <label style={label}>From name</label>
                  <input style={input} value={sendFromName} onChange={(e) => setSendFromName(e.target.value)} placeholder="optional" />
                </div>
              </div>
              <label style={label}>Recipients</label>
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, marginBottom: 10 }}>
                {recipients.filter((r) => r.is_active).length === 0 && <span style={{ fontSize: 12, color: '#64748b' }}>Add active account managers above.</span>}
                {recipients.filter((r) => r.is_active).map((r) => (
                  <button key={r.id} style={chip(!!sendRcpts[r.id])}
                    onClick={() => setSendRcpts((c) => ({ ...c, [r.id]: !c[r.id] }))}>
                    {r.name}
                  </button>
                ))}
              </div>
              <button style={btn('#312e81', '#4338ca')} disabled={busy} onClick={sendProof}>
                <FontAwesomeIcon icon={faPaperPlane} /> Send proof
              </button>
            </div>

            {/* approval panel */}
            <div style={card}>
              <div style={{ fontSize: 13, fontWeight: 600, marginBottom: 10 }}>
                <FontAwesomeIcon icon={faCircleCheck} style={{ color: '#22c55e' }} /> Approve — where this proof may mail
              </div>
              <label style={label}>Approved sending domains</label>
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, marginBottom: 10 }}>
                {domains.map((d) => (
                  <button key={d.domain} style={chip(selected.approved_domains.includes(d.domain))}
                    onClick={() => toggleApprovedDomain(d.domain)}>{d.domain}</button>
                ))}
              </div>
              <label style={label}>Approved ISPs</label>
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, marginBottom: 12 }}>
                {isps.map((isp) => (
                  <button key={isp} style={chip(selected.approved_isps.includes(isp))}
                    onClick={() => toggleApprovedISP(isp)}>{isp}</button>
                ))}
              </div>
              <div style={{ display: 'flex', gap: 8 }}>
                <button style={btn('#14532d', '#16a34a')} disabled={busy} onClick={approveProof}>
                  <FontAwesomeIcon icon={faCircleCheck} /> Approve
                </button>
                <button style={btn('#3f1d1d', '#7f1d1d')} disabled={busy} onClick={rejectProof}>
                  <FontAwesomeIcon icon={faBan} /> Reject
                </button>
              </div>
            </div>
          </div>
        )}
      </div>
    </div>
  );
};

export default OfferProofs;
