import React, { useState, useEffect, useCallback, useRef, useMemo } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faShieldAlt, faBan, faSearch,
  faExclamationTriangle, faCheckCircle, faTrash,
  faFileExport, faPlus, faSync, faUpload,
  faChartBar, faGlobe, faSpinner, faPaste,
} from '@fortawesome/free-solid-svg-icons';

const API_BASE = '/api/mailing';

interface DashboardStats {
  total_suppressed: number;
  global_suppression: {
    by_category: Record<string, number>;
    hard_bounces: number;
    invalid: number;
    disposable: number;
    known_litigator: number;
  };
  by_source: { source: string; count: number }[];
  recent_additions: number;
}

interface SuppressionEntry {
  id: string;
  email: string;
  md5_hash: string;
  reason: string;
  source: string;
  created_at: string;
}

interface BulkUploadProgress {
  total: number;
  processed: number;
  succeeded: number;
  failed: number;
  status: 'idle' | 'uploading' | 'done' | 'error';
  errorMessage?: string;
}

const REASON_COLORS: Record<string, string> = {
  hard_bounce: '#e94560',
  soft_bounce: '#f97316',
  spam_complaint: '#e94560',
  unsubscribe: '#00b0ff',
  inactive: 'rgba(180,210,240,0.65)',
  manual: '#00e5ff',
  unverified: '#fdcb6e',
  system: '#6c5ce7',
};

const REASON_LABELS: Record<string, string> = {
  hard_bounce: 'Hard Bounce',
  soft_bounce: 'Soft Bounce',
  spam_complaint: 'Spam Complaint',
  unsubscribe: 'Unsubscribe',
  inactive: 'Inactive',
  manual: 'Manual',
  unverified: 'Unverified',
  system: 'System',
  bot_clicker: 'Bot Clicker',
};

const BATCH_SIZE = 500;

export const GlobalSuppressionDashboard: React.FC = () => {
  const [stats, setStats] = useState<DashboardStats | null>(null);
  const [entries, setEntries] = useState<SuppressionEntry[]>([]);
  const [totalEntries, setTotalEntries] = useState(0);
  const [searchQuery, setSearchQuery] = useState('');
  const [activeView, setActiveView] = useState<'overview' | 'search' | 'add'>('overview');
  const [loading, setLoading] = useState(true);

  const [suppressEmail, setSuppressEmail] = useState('');
  const [suppressReason, setSuppressReason] = useState('manual');
  const [suppressing, setSuppressing] = useState(false);
  const [suppressResult, setSuppressResult] = useState<{ ok: boolean; msg: string } | null>(null);

  const [pasteInput, setPasteInput] = useState('');
  const [pasteReason, setPasteReason] = useState('manual');

  const [bulkProgress, setBulkProgress] = useState<BulkUploadProgress>({ total: 0, processed: 0, succeeded: 0, failed: 0, status: 'idle' });
  const fileInputRef = useRef<HTMLInputElement | null>(null);
  const abortRef = useRef(false);

  const fetchStats = useCallback(async () => {
    try {
      const res = await fetch(`${API_BASE}/suppressions/dashboard`);
      if (res.ok) setStats(await res.json());
    } catch { /* ignore */ }
  }, []);

  const fetchEntries = useCallback(async (q?: string) => {
    try {
      setLoading(true);
      const params = new URLSearchParams({ limit: '50' });
      if (q) params.set('q', q);
      const res = await fetch(`${API_BASE}/suppressions/global/entries?${params}`);
      if (res.ok) {
        const data = await res.json();
        setEntries(data.entries || []);
        setTotalEntries(data.total || 0);
      }
    } catch { /* ignore */ } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    fetchStats();
    fetchEntries();
    const interval = setInterval(fetchStats, 30000);
    return () => clearInterval(interval);
  }, [fetchStats, fetchEntries]);

  const handleSearch = () => fetchEntries(searchQuery);

  const handleSuppress = async () => {
    const email = suppressEmail.trim().toLowerCase();
    if (!email) return;
    setSuppressing(true);
    setSuppressResult(null);
    try {
      const res = await fetch(`${API_BASE}/suppressions/global`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email, category: suppressReason, source: 'manual_ui' }),
      });
      if (res.ok) {
        setSuppressResult({ ok: true, msg: `${email} suppressed` });
        setSuppressEmail('');
        fetchStats();
        fetchEntries(searchQuery);
      } else {
        const err = await res.json().catch(() => ({}));
        setSuppressResult({ ok: false, msg: (err as any).error || 'Failed' });
      }
    } catch { setSuppressResult({ ok: false, msg: 'Network error' }); } finally {
      setSuppressing(false);
    }
  };

  const handleRemove = async (email: string) => {
    if (!confirm(`Remove ${email} from global suppression?`)) return;
    await fetch(`${API_BASE}/suppressions/global/${encodeURIComponent(email)}`, { method: 'DELETE' });
    fetchStats();
    fetchEntries(searchQuery);
  };

  const handleExportMD5 = () => {
    window.open(`${API_BASE}/v2/suppressions/export?format=text`, '_blank');
  };

  const parseCSVEmails = (text: string): string[] => {
    const lines = text.split(/\r?\n/);
    if (lines.length === 0) return [];
    const header = lines[0].toLowerCase();
    let emailCol = 0;
    if (header.includes(',') || header.includes('\t')) {
      const sep = header.includes('\t') ? '\t' : ',';
      const cols = header.split(sep).map(c => c.trim().replace(/"/g, ''));
      emailCol = cols.findIndex(c => c === 'email' || c === 'email_address' || c === 'emailaddress' || c === 'e-mail');
      if (emailCol === -1) emailCol = 0;
    }
    const emailRegex = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
    const emails: string[] = [];
    const seen = new Set<string>();
    const start = header.includes('@') ? 0 : 1;
    for (let i = start; i < lines.length; i++) {
      const line = lines[i].trim();
      if (!line) continue;
      const sep = line.includes('\t') ? '\t' : ',';
      const parts = line.split(sep);
      const raw = (parts[emailCol] || '').trim().replace(/"/g, '').toLowerCase();
      if (emailRegex.test(raw) && !seen.has(raw)) {
        seen.add(raw);
        emails.push(raw);
      }
    }
    return emails;
  };

  const sendBatchesToSuppression = async (emails: string[], source: string, reason: string) => {
    abortRef.current = false;
    setBulkProgress({ total: emails.length, processed: 0, succeeded: 0, failed: 0, status: 'uploading' });

    let succeeded = 0, failed = 0;
    for (let i = 0; i < emails.length; i += BATCH_SIZE) {
      if (abortRef.current) break;
      const batch = emails.slice(i, i + BATCH_SIZE);
      try {
        const res = await fetch(`${API_BASE}/suppressions/global/bulk`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ emails: batch, category: reason, source }),
        });
        if (res.ok) {
          const data = await res.json();
          succeeded += data.added || batch.length;
          failed += data.failed || 0;
        } else {
          failed += batch.length;
        }
      } catch {
        failed += batch.length;
      }
      setBulkProgress(p => ({ ...p, processed: Math.min(i + BATCH_SIZE, emails.length), succeeded, failed }));
    }
    setBulkProgress(p => ({ ...p, processed: emails.length, succeeded, failed, status: 'done' }));
    fetchStats();
    fetchEntries(searchQuery);
  };

  const handleFileUpload = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    setBulkProgress({ total: 0, processed: 0, succeeded: 0, failed: 0, status: 'uploading' });
    try {
      const text = await file.text();
      const emails = parseCSVEmails(text);
      if (emails.length === 0) {
        setBulkProgress(p => ({ ...p, status: 'error', errorMessage: 'No valid emails found in file' }));
        return;
      }
      await sendBatchesToSuppression(emails, 'csv_upload', 'manual');
    } catch {
      setBulkProgress(p => ({ ...p, status: 'error', errorMessage: 'Failed to read file' }));
    }
    if (fileInputRef.current) fileInputRef.current.value = '';
  };

  const parseTextEmails = (text: string): string[] => {
    const emailRegex = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
    const seen = new Set<string>();
    const emails: string[] = [];
    for (const token of text.split(/[\n\r,;]+/)) {
      const raw = token.trim().replace(/"/g, '').toLowerCase();
      if (emailRegex.test(raw) && !seen.has(raw)) {
        seen.add(raw);
        emails.push(raw);
      }
    }
    return emails;
  };

  const pasteEmailCount = useMemo(() => parseTextEmails(pasteInput).length, [pasteInput]);

  const handlePasteSuppress = async () => {
    const emails = parseTextEmails(pasteInput);
    if (emails.length === 0) return;
    await sendBatchesToSuppression(emails, 'paste_upload', pasteReason);
    setPasteInput('');
  };

  const reasonLabel = (reason: string) => REASON_LABELS[reason] || reason;
  const reasonColor = (reason: string) => REASON_COLORS[reason] || 'rgba(180,210,240,0.65)';

  const s: React.CSSProperties = {
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif',
    color: '#e0e6f0',
    padding: 24,
  };
  const card: React.CSSProperties = {
    background: '#0d1526',
    border: '1px solid rgba(0,200,255,0.08)',
    borderRadius: 12,
    padding: 20,
    marginBottom: 16,
  };
  const statCard: React.CSSProperties = {
    ...card,
    textAlign: 'center' as const,
    flex: 1,
    minWidth: 160,
  };
  const navBtn = (active: boolean): React.CSSProperties => ({
    background: active ? 'rgba(0,229,255,0.25)' : 'rgba(0,200,255,0.04)',
    border: `1px solid ${active ? 'rgba(0,229,255,0.5)' : 'rgba(0,200,255,0.08)'}`,
    color: active ? '#00e5ff' : 'rgba(180,210,240,0.65)',
    padding: '8px 16px',
    borderRadius: 8,
    cursor: 'pointer',
    fontSize: 13,
    fontWeight: active ? 600 : 400,
  });

  const gs = stats?.global_suppression;

  return (
    <div style={s}>
      {/* Header */}
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 24 }}>
        <div>
          <h2 style={{ margin: 0, fontSize: 22, fontWeight: 700, display: 'flex', alignItems: 'center', gap: 10 }}>
            <FontAwesomeIcon icon={faShieldAlt} style={{ color: '#e94560' }} />
            Global Suppression Hub
          </h2>
          <p style={{ margin: '4px 0 0', fontSize: 13, color: 'rgba(180,210,240,0.65)' }}>
            Single source of truth — all negative signals converge here. MD5-hashed for instant comparison.
          </p>
        </div>
        <div style={{ display: 'flex', gap: 8 }}>
          <button onClick={handleExportMD5} style={{ background: 'rgba(0,229,255,0.15)', border: '1px solid rgba(0,229,255,0.3)', color: '#00e5ff', padding: '8px 14px', borderRadius: 8, cursor: 'pointer', fontSize: 13, display: 'flex', alignItems: 'center', gap: 6 }}>
            <FontAwesomeIcon icon={faFileExport} /> Export
          </button>
          <button onClick={() => { fetchStats(); fetchEntries(searchQuery); }} style={{ background: 'rgba(0,184,148,0.15)', border: '1px solid rgba(0,184,148,0.3)', color: '#00b894', padding: '8px 14px', borderRadius: 8, cursor: 'pointer', fontSize: 13, display: 'flex', alignItems: 'center', gap: 6 }}>
            <FontAwesomeIcon icon={faSync} /> Refresh
          </button>
        </div>
      </div>

      {/* Navigation */}
      <div style={{ display: 'flex', gap: 8, marginBottom: 20 }}>
        <button onClick={() => setActiveView('overview')} style={navBtn(activeView === 'overview')}>
          <FontAwesomeIcon icon={faChartBar} style={{ marginRight: 6 }} />Overview
        </button>
        <button onClick={() => setActiveView('search')} style={navBtn(activeView === 'search')}>
          <FontAwesomeIcon icon={faSearch} style={{ marginRight: 6 }} />Search & Manage
        </button>
        <button onClick={() => setActiveView('add')} style={navBtn(activeView === 'add')}>
          <FontAwesomeIcon icon={faPlus} style={{ marginRight: 6 }} />Add to Suppression
        </button>
      </div>

      {/* ===== OVERVIEW ===== */}
      {activeView === 'overview' && !stats && (
        <div>
          <div style={{ display: 'flex', gap: 12, marginBottom: 16, flexWrap: 'wrap' }}>
            {[1, 2, 3, 4].map(i => (
              <div key={i} style={{ ...statCard, flex: 1, minWidth: 160 }}>
                <div style={{ height: 28, width: '40%', margin: '0 auto', background: 'rgba(0,200,255,0.06)', borderRadius: 6, marginBottom: 8, animation: 'igShimmer 1.5s ease infinite', animationDelay: `${i * 0.12}s` }} />
                <div style={{ height: 12, width: '60%', margin: '0 auto', background: 'rgba(0,200,255,0.04)', borderRadius: 4, animation: 'igShimmer 1.5s ease infinite', animationDelay: `${i * 0.15}s` }} />
              </div>
            ))}
          </div>
          <style>{`@keyframes igShimmer { 0% { opacity: 0.4; } 50% { opacity: 0.7; } 100% { opacity: 0.4; } }`}</style>
        </div>
      )}
      {activeView === 'overview' && stats && (
        <>
          {/* Top Stats */}
          <div style={{ display: 'flex', gap: 12, marginBottom: 16, flexWrap: 'wrap' }}>
            <div style={statCard}>
              <div style={{ fontSize: 28, fontWeight: 700, color: '#e94560' }}>{(stats.total_suppressed || 0).toLocaleString()}</div>
              <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.65)', marginTop: 4 }}>Total Suppressed</div>
            </div>
            <div style={statCard}>
              <div style={{ fontSize: 28, fontWeight: 700, color: '#f97316' }}>{(stats.recent_additions || 0).toLocaleString()}</div>
              <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.65)', marginTop: 4 }}>Recent Additions</div>
            </div>
            <div style={statCard}>
              <div style={{ fontSize: 28, fontWeight: 700, color: '#fdcb6e' }}>{(gs?.hard_bounces || 0).toLocaleString()}</div>
              <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.65)', marginTop: 4 }}>Hard Bounces</div>
            </div>
            <div style={statCard}>
              <div style={{ fontSize: 28, fontWeight: 700, color: '#00b0ff' }}>{(gs?.invalid || 0).toLocaleString()}</div>
              <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.65)', marginTop: 4 }}>Invalid</div>
            </div>
          </div>

          {/* Breakdowns */}
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16 }}>
            {/* By Category */}
            <div style={card}>
              <h3 style={{ margin: '0 0 12px', fontSize: 14, color: '#00e5ff' }}>
                <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 6 }} />By Category
              </h3>
              {Object.entries(gs?.by_category || {}).sort((a, b) => b[1] - a[1]).map(([cat, count]) => (
                <div key={cat} style={{ display: 'flex', justifyContent: 'space-between', padding: '6px 0', borderBottom: '1px solid rgba(0,200,255,0.04)' }}>
                  <span style={{ fontSize: 13, display: 'flex', alignItems: 'center', gap: 6 }}>
                    <span style={{ width: 8, height: 8, borderRadius: '50%', background: reasonColor(cat), display: 'inline-block' }} />
                    {reasonLabel(cat)}
                  </span>
                  <span style={{ fontSize: 13, fontWeight: 600, color: '#e0e6f0' }}>{count.toLocaleString()}</span>
                </div>
              ))}
              {Object.keys(gs?.by_category || {}).length === 0 && <div style={{ color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>No data yet</div>}
            </div>

            {/* By Source */}
            <div style={card}>
              <h3 style={{ margin: '0 0 12px', fontSize: 14, color: '#00e5ff' }}>
                <FontAwesomeIcon icon={faGlobe} style={{ marginRight: 6 }} />By Source
              </h3>
              {(Array.isArray(stats.by_source) ? stats.by_source : Object.entries(stats.by_source || {}).map(([source, count]) => ({ source, count: count as number }))).sort((a: any, b: any) => b.count - a.count).map((item: any) => (
                <div key={item.source} style={{ display: 'flex', justifyContent: 'space-between', padding: '6px 0', borderBottom: '1px solid rgba(0,200,255,0.04)' }}>
                  <span style={{ fontSize: 13, color: '#e0e6f0' }}>{item.source}</span>
                  <span style={{ fontSize: 13, fontWeight: 600, color: '#e0e6f0' }}>{(item.count || 0).toLocaleString()}</span>
                </div>
              ))}
              {(!stats.by_source || (Array.isArray(stats.by_source) && stats.by_source.length === 0)) && <div style={{ color: 'rgba(180,210,240,0.65)', fontSize: 13 }}>No data yet</div>}
            </div>
          </div>

          {/* Add Single Suppression */}
          <div style={{ ...card, marginTop: 16 }}>
            <h3 style={{ margin: '0 0 12px', fontSize: 14, color: '#00e5ff' }}>
              <FontAwesomeIcon icon={faPlus} style={{ marginRight: 6 }} />Suppress Email Address
            </h3>
            <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
              <input
                type="email"
                placeholder="email@example.com"
                value={suppressEmail}
                onChange={e => { setSuppressEmail(e.target.value); setSuppressResult(null); }}
                onKeyDown={e => e.key === 'Enter' && handleSuppress()}
                style={{ flex: 1, background: '#0a1020', border: '1px solid rgba(0,200,255,0.1)', borderRadius: 8, padding: '10px 14px', color: '#e0e6f0', fontSize: 13 }}
              />
              <select
                value={suppressReason}
                onChange={e => setSuppressReason(e.target.value)}
                style={{ background: '#0a1020', border: '1px solid rgba(0,200,255,0.1)', borderRadius: 8, padding: '10px 14px', color: '#e0e6f0', fontSize: 13 }}
              >
                <option value="manual">Manual</option>
                <option value="hard_bounce">Hard Bounce</option>
                <option value="spam_complaint">Spam Complaint</option>
                <option value="unsubscribe">Unsubscribe</option>
                <option value="inactive">Inactive</option>
              </select>
              <button onClick={handleSuppress} disabled={suppressing || !suppressEmail.trim()} style={{ background: 'rgba(233,69,96,0.2)', border: '1px solid rgba(233,69,96,0.4)', color: '#e94560', padding: '10px 18px', borderRadius: 8, cursor: suppressing ? 'wait' : 'pointer', fontSize: 13, fontWeight: 600, opacity: suppressing ? 0.6 : 1, display: 'flex', alignItems: 'center', gap: 6 }}>
                {suppressing ? <FontAwesomeIcon icon={faSpinner} spin /> : <FontAwesomeIcon icon={faBan} />} Suppress
              </button>
            </div>
            {suppressResult && (
              <div style={{ marginTop: 8, fontSize: 13, color: suppressResult.ok ? '#00b894' : '#e94560', display: 'flex', alignItems: 'center', gap: 6 }}>
                <FontAwesomeIcon icon={suppressResult.ok ? faCheckCircle : faExclamationTriangle} />
                {suppressResult.msg}
              </div>
            )}
          </div>
        </>
      )}

      {/* ===== SEARCH & MANAGE ===== */}
      {activeView === 'search' && (
        <div>
          <div style={{ ...card, display: 'flex', gap: 8, alignItems: 'center' }}>
            <input
              type="text"
              placeholder="Search by email or MD5 hash..."
              value={searchQuery}
              onChange={e => setSearchQuery(e.target.value)}
              onKeyDown={e => e.key === 'Enter' && handleSearch()}
              style={{ flex: 1, background: '#0a1020', border: '1px solid rgba(0,200,255,0.1)', borderRadius: 8, padding: '10px 14px', color: '#e0e6f0', fontSize: 14 }}
            />
            <button onClick={handleSearch} style={{ background: 'rgba(0,229,255,0.2)', border: '1px solid rgba(0,229,255,0.4)', color: '#00e5ff', padding: '10px 18px', borderRadius: 8, cursor: 'pointer', fontSize: 13 }}>
              <FontAwesomeIcon icon={faSearch} /> Search
            </button>
          </div>

          <div style={{ fontSize: 13, color: 'rgba(180,210,240,0.65)', marginBottom: 8 }}>
            {totalEntries.toLocaleString()} results {searchQuery && `for "${searchQuery}"`}
          </div>

          <div style={{ ...card, padding: 0, overflow: 'hidden' }}>
            <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
              <thead>
                <tr style={{ background: 'rgba(0,200,255,0.03)' }}>
                  <th style={{ padding: '10px 14px', textAlign: 'left', color: 'rgba(180,210,240,0.65)', fontWeight: 500 }}>Email</th>
                  <th style={{ padding: '10px 14px', textAlign: 'left', color: 'rgba(180,210,240,0.65)', fontWeight: 500 }}>MD5</th>
                  <th style={{ padding: '10px 14px', textAlign: 'left', color: 'rgba(180,210,240,0.65)', fontWeight: 500 }}>Reason</th>
                  <th style={{ padding: '10px 14px', textAlign: 'left', color: 'rgba(180,210,240,0.65)', fontWeight: 500 }}>Source</th>
                  <th style={{ padding: '10px 14px', textAlign: 'left', color: 'rgba(180,210,240,0.65)', fontWeight: 500 }}>Date</th>
                  <th style={{ padding: '10px 14px', textAlign: 'center', color: 'rgba(180,210,240,0.65)', fontWeight: 500 }}>Action</th>
                </tr>
              </thead>
              <tbody>
                {loading && entries.length === 0 && (
                  <>
                    {[1, 2, 3, 4, 5].map(i => (
                      <tr key={`skel-${i}`} style={{ borderTop: '1px solid rgba(0,200,255,0.04)' }}>
                        {[1, 2, 3, 4, 5, 6].map(j => (
                          <td key={j} style={{ padding: '10px 14px' }}>
                            <div style={{ height: 12, width: `${60 + Math.random() * 30}%`, background: 'rgba(0,200,255,0.06)', borderRadius: 3, animation: 'igShimmer 1.5s ease infinite', animationDelay: `${(i + j) * 0.08}s` }} />
                          </td>
                        ))}
                      </tr>
                    ))}
                    <style>{`@keyframes igShimmer { 0% { opacity: 0.4; } 50% { opacity: 0.7; } 100% { opacity: 0.4; } }`}</style>
                  </>
                )}
                {entries.map(entry => (
                  <tr key={entry.id} style={{ borderTop: '1px solid rgba(0,200,255,0.04)' }}>
                    <td style={{ padding: '8px 14px', color: '#e0e6f0' }}>{entry.email || '(hash-only)'}</td>
                    <td style={{ padding: '8px 14px', color: 'rgba(180,210,240,0.65)', fontFamily: 'monospace', fontSize: 11 }}>{entry.md5_hash?.substring(0, 12)}...</td>
                    <td style={{ padding: '8px 14px' }}>
                      <span style={{ background: `${reasonColor(entry.reason)}22`, color: reasonColor(entry.reason), padding: '2px 8px', borderRadius: 4, fontSize: 11, fontWeight: 600 }}>
                        {reasonLabel(entry.reason)}
                      </span>
                    </td>
                    <td style={{ padding: '8px 14px', color: 'rgba(180,210,240,0.65)', fontSize: 12 }}>{entry.source}</td>
                    <td style={{ padding: '8px 14px', color: 'rgba(180,210,240,0.65)', fontSize: 12 }}>{new Date(entry.created_at).toLocaleDateString()}</td>
                    <td style={{ padding: '8px 14px', textAlign: 'center' }}>
                      <button onClick={() => handleRemove(entry.email)} style={{ background: 'none', border: 'none', color: '#e94560', cursor: 'pointer', fontSize: 13 }}>
                        <FontAwesomeIcon icon={faTrash} />
                      </button>
                    </td>
                  </tr>
                ))}
                {entries.length === 0 && !loading && (
                  <tr><td colSpan={6} style={{ padding: 20, textAlign: 'center', color: 'rgba(180,210,240,0.65)' }}>No entries found</td></tr>
                )}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {/* ===== ADD TO SUPPRESSION ===== */}
      {activeView === 'add' && (
        <div>
          {/* Section 1: Single Email */}
          <div style={card}>
            <h3 style={{ margin: '0 0 12px', fontSize: 14, color: '#00e5ff' }}>
              <FontAwesomeIcon icon={faBan} style={{ marginRight: 6 }} />Single Email
            </h3>
            <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
              <input
                type="email"
                placeholder="email@example.com"
                value={suppressEmail}
                onChange={e => { setSuppressEmail(e.target.value); setSuppressResult(null); }}
                onKeyDown={e => e.key === 'Enter' && handleSuppress()}
                style={{ flex: 1, background: '#0a1020', border: '1px solid rgba(0,200,255,0.1)', borderRadius: 8, padding: '10px 14px', color: '#e0e6f0', fontSize: 13 }}
              />
              <select
                value={suppressReason}
                onChange={e => setSuppressReason(e.target.value)}
                style={{ background: '#0a1020', border: '1px solid rgba(0,200,255,0.1)', borderRadius: 8, padding: '10px 14px', color: '#e0e6f0', fontSize: 13 }}
              >
                <option value="manual">Manual</option>
                <option value="hard_bounce">Hard Bounce</option>
                <option value="spam_complaint">Spam Complaint</option>
                <option value="unsubscribe">Unsubscribe</option>
                <option value="inactive">Inactive</option>
              </select>
              <button onClick={handleSuppress} disabled={suppressing || !suppressEmail.trim()} style={{ background: 'rgba(233,69,96,0.2)', border: '1px solid rgba(233,69,96,0.4)', color: '#e94560', padding: '10px 18px', borderRadius: 8, cursor: suppressing ? 'wait' : 'pointer', fontSize: 13, fontWeight: 600, opacity: suppressing ? 0.6 : 1, display: 'flex', alignItems: 'center', gap: 6 }}>
                {suppressing ? <FontAwesomeIcon icon={faSpinner} spin /> : <FontAwesomeIcon icon={faBan} />} Suppress
              </button>
            </div>
            {suppressResult && (
              <div style={{ marginTop: 8, fontSize: 13, color: suppressResult.ok ? '#00b894' : '#e94560', display: 'flex', alignItems: 'center', gap: 6 }}>
                <FontAwesomeIcon icon={suppressResult.ok ? faCheckCircle : faExclamationTriangle} />
                {suppressResult.msg}
              </div>
            )}
          </div>

          {/* Section 2: Paste Emails */}
          <div style={card}>
            <h3 style={{ margin: '0 0 8px', fontSize: 14, color: '#00e5ff' }}>
              <FontAwesomeIcon icon={faPaste} style={{ marginRight: 6 }} />Paste Emails
            </h3>
            <p style={{ margin: '0 0 12px', fontSize: 12, color: 'rgba(180,210,240,0.5)' }}>
              Paste one email per line, or separated by commas. Duplicates are removed automatically.
            </p>
            <textarea
              placeholder={"user1@example.com\nuser2@example.com\nuser3@example.com"}
              value={pasteInput}
              onChange={e => setPasteInput(e.target.value)}
              rows={8}
              style={{ width: '100%', background: '#0a1020', border: '1px solid rgba(0,200,255,0.1)', borderRadius: 8, padding: '10px 14px', color: '#e0e6f0', fontSize: 13, fontFamily: 'monospace', resize: 'vertical', boxSizing: 'border-box' }}
            />
            <div style={{ marginTop: 10, display: 'flex', gap: 8, alignItems: 'center' }}>
              <select
                value={pasteReason}
                onChange={e => setPasteReason(e.target.value)}
                style={{ background: '#0a1020', border: '1px solid rgba(0,200,255,0.1)', borderRadius: 8, padding: '10px 14px', color: '#e0e6f0', fontSize: 13 }}
              >
                <option value="manual">Manual</option>
                <option value="hard_bounce">Hard Bounce</option>
                <option value="spam_complaint">Spam Complaint</option>
                <option value="unsubscribe">Unsubscribe</option>
                <option value="inactive">Inactive</option>
              </select>
              <button
                onClick={handlePasteSuppress}
                disabled={pasteEmailCount === 0 || bulkProgress.status === 'uploading'}
                style={{ background: 'rgba(233,69,96,0.2)', border: '1px solid rgba(233,69,96,0.4)', color: '#e94560', padding: '10px 18px', borderRadius: 8, cursor: pasteEmailCount === 0 ? 'not-allowed' : 'pointer', fontSize: 13, fontWeight: 600, opacity: pasteEmailCount === 0 ? 0.5 : 1, display: 'flex', alignItems: 'center', gap: 6 }}
              >
                <FontAwesomeIcon icon={faBan} /> Suppress All
              </button>
              <span style={{ fontSize: 12, color: pasteEmailCount > 0 ? '#00e5ff' : 'rgba(180,210,240,0.5)', fontWeight: pasteEmailCount > 0 ? 600 : 400 }}>
                {pasteEmailCount > 0 ? `${pasteEmailCount.toLocaleString()} valid email${pasteEmailCount !== 1 ? 's' : ''} detected` : 'No valid emails yet'}
              </span>
            </div>
          </div>

          {/* Section 3: File Upload */}
          <div style={card}>
            <h3 style={{ margin: '0 0 8px', fontSize: 14, color: '#00e5ff' }}>
              <FontAwesomeIcon icon={faUpload} style={{ marginRight: 6 }} />Upload File
            </h3>
            <p style={{ margin: '0 0 12px', fontSize: 12, color: 'rgba(180,210,240,0.5)' }}>
              CSV or TXT file with email addresses. Supports files with an "email" column header, or one email per line.
            </p>
            <input
              ref={fileInputRef}
              type="file"
              accept=".csv,.txt,.tsv"
              onChange={handleFileUpload}
              style={{ display: 'none' }}
            />
            <div style={{ display: 'flex', gap: 12, alignItems: 'center' }}>
              <button
                onClick={() => fileInputRef.current?.click()}
                disabled={bulkProgress.status === 'uploading'}
                style={{
                  background: 'rgba(233,69,96,0.15)',
                  border: '2px dashed rgba(233,69,96,0.4)',
                  color: '#e94560',
                  padding: '16px 28px',
                  borderRadius: 12,
                  cursor: bulkProgress.status === 'uploading' ? 'wait' : 'pointer',
                  fontSize: 13,
                  fontWeight: 600,
                  display: 'flex',
                  alignItems: 'center',
                  gap: 8,
                }}
              >
                {bulkProgress.status === 'uploading'
                  ? <><FontAwesomeIcon icon={faSpinner} spin /> Processing...</>
                  : <><FontAwesomeIcon icon={faUpload} /> Choose CSV / TXT File</>
                }
              </button>
              {bulkProgress.status === 'uploading' && (
                <button onClick={() => { abortRef.current = true; }} style={{ background: 'rgba(233,69,96,0.1)', border: '1px solid rgba(233,69,96,0.3)', color: '#e94560', padding: '8px 16px', borderRadius: 8, cursor: 'pointer', fontSize: 12 }}>
                  Cancel
                </button>
              )}
            </div>
          </div>

          {/* Shared Progress Display */}
          {bulkProgress.total > 0 && (
            <div style={card}>
              <h3 style={{ margin: '0 0 12px', fontSize: 14, color: '#00e5ff' }}>
                <FontAwesomeIcon icon={bulkProgress.status === 'done' ? faCheckCircle : faSpinner} spin={bulkProgress.status === 'uploading'} style={{ marginRight: 6 }} />
                {bulkProgress.status === 'uploading' ? 'Processing...' : bulkProgress.status === 'done' ? 'Complete' : 'Error'}
              </h3>

              <div style={{ background: 'rgba(0,200,255,0.06)', borderRadius: 8, height: 12, marginBottom: 16, overflow: 'hidden' }}>
                <div style={{
                  background: bulkProgress.status === 'error' ? '#e94560' : 'linear-gradient(90deg, #00e5ff, #6c5ce7)',
                  height: '100%',
                  width: `${Math.min((bulkProgress.processed / bulkProgress.total) * 100, 100)}%`,
                  borderRadius: 8,
                  transition: 'width 0.3s ease',
                }} />
              </div>

              <div style={{ display: 'flex', gap: 16 }}>
                <div style={{ flex: 1, textAlign: 'center', padding: 14, background: 'rgba(0,200,255,0.04)', borderRadius: 8, border: '1px solid rgba(0,200,255,0.08)' }}>
                  <div style={{ fontSize: 24, fontWeight: 700, color: '#e0e6f0' }}>{bulkProgress.total.toLocaleString()}</div>
                  <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)' }}>Total Emails</div>
                </div>
                <div style={{ flex: 1, textAlign: 'center', padding: 14, background: 'rgba(0,200,255,0.04)', borderRadius: 8, border: '1px solid rgba(0,200,255,0.08)' }}>
                  <div style={{ fontSize: 24, fontWeight: 700, color: '#00e5ff' }}>{bulkProgress.processed.toLocaleString()}</div>
                  <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)' }}>Processed</div>
                </div>
                <div style={{ flex: 1, textAlign: 'center', padding: 14, background: 'rgba(0,184,148,0.08)', borderRadius: 8, border: '1px solid rgba(0,184,148,0.15)' }}>
                  <div style={{ fontSize: 24, fontWeight: 700, color: '#00b894' }}>{bulkProgress.succeeded.toLocaleString()}</div>
                  <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)' }}>Suppressed</div>
                </div>
                {bulkProgress.failed > 0 && (
                  <div style={{ flex: 1, textAlign: 'center', padding: 14, background: 'rgba(233,69,96,0.08)', borderRadius: 8, border: '1px solid rgba(233,69,96,0.15)' }}>
                    <div style={{ fontSize: 24, fontWeight: 700, color: '#e94560' }}>{bulkProgress.failed.toLocaleString()}</div>
                    <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.65)' }}>Failed</div>
                  </div>
                )}
              </div>

              {bulkProgress.errorMessage && (
                <div style={{ marginTop: 12, padding: '10px 14px', background: 'rgba(233,69,96,0.1)', border: '1px solid rgba(233,69,96,0.2)', borderRadius: 8, color: '#e94560', fontSize: 13 }}>
                  {bulkProgress.errorMessage}
                </div>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  );
};
