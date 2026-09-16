import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faSync, faSpinner, faExclamationTriangle, faUpload, faDatabase, faXmark,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import { labelForVertical } from './verticalLabels';
import { DripCsvUpload, type CsvFeedOption } from '../components/DripCsvUpload';

// ReservoirPanel — "what do we actually hold, and what can mail tomorrow".
//
// Backed by GET /api/mailing/data-partners/reservoir (partner_clean_queue
// grouped by vertical × status × ISP, 60s server cache). Three buckets, and
// the distinction is the whole point of the screen:
//   MAILABLE  status=ready              — can be composed into a send today
//   RESERVE   held + pending_eo         — owned, but needs release or EO first
//   EXHAUSTED mailed/suppressed/dead    — spent or unusable
// A failed fetch renders as an explicit error; it never renders as zero.

const UNKNOWN = '—';
const num = (n: number | null | undefined): string =>
  typeof n === 'number' && Number.isFinite(n) ? Math.round(n).toLocaleString() : UNKNOWN;

interface VerticalTotal {
  vertical: string;
  total: number;
  by_status: Record<string, number>;
  by_isp: Record<string, number>;
  mailable: number;
  reserve: number;
  exhausted: number;
}
interface ReservoirResponse {
  generated_at: string;
  cached: boolean;
  age_seconds: number;
  query_ms: number;
  total: number;
  mailable_total: number;
  by_status: Array<{ status: string; count: number }>;
  by_vertical: VerticalTotal[];
}

const card: React.CSSProperties = {
  background: 'rgba(15,23,42,0.55)', border: '1px solid rgba(99,102,241,0.25)',
  borderRadius: 8, padding: '14px 16px',
};
const th: React.CSSProperties = {
  textAlign: 'left', padding: '7px 10px', fontSize: 10, letterSpacing: 0.6,
  textTransform: 'uppercase', color: 'rgba(180,210,240,0.65)',
  borderBottom: '1px solid rgba(99,102,241,0.25)', whiteSpace: 'nowrap',
};
const td: React.CSSProperties = {
  padding: '7px 10px', fontSize: 13, color: 'rgba(220,235,250,0.92)',
  borderBottom: '1px solid rgba(99,102,241,0.12)',
};
const tdNum: React.CSSProperties = { ...td, textAlign: 'right', fontVariantNumeric: 'tabular-nums' };
const btn: React.CSSProperties = {
  background: 'rgba(99,102,241,0.13)', color: '#c7d2fe',
  border: '1px solid rgba(99,102,241,0.4)', borderRadius: 5,
  padding: '6px 13px', fontSize: 12, fontWeight: 700, cursor: 'pointer',
};

const BigNumber: React.FC<{ label: string; value: number; tone: string; hint: string }> =
  ({ label, value, tone, hint }) => (
    <div style={{ ...card, flex: '1 1 180px' }}>
      <div style={{ fontSize: 10, letterSpacing: 0.7, textTransform: 'uppercase', color: 'rgba(180,210,240,0.65)' }}>{label}</div>
      <div style={{ fontSize: 28, fontWeight: 700, color: tone, fontVariantNumeric: 'tabular-nums', lineHeight: 1.2 }}>{num(value)}</div>
      <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.6)' }}>{hint}</div>
    </div>
  );

export const ReservoirPanel: React.FC = () => {
  const [data, setData] = useState<ReservoirResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<string | null>(null);
  const [uploadOpen, setUploadOpen] = useState(false);
  const [feeds, setFeeds] = useState<CsvFeedOption[]>([]);
  const [feedsErr, setFeedsErr] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const load = useCallback(async (refresh = false) => {
    setLoading(true);
    setErr(null);
    try {
      const r = await apiFetch(`/api/mailing/data-partners/reservoir${refresh ? '?refresh=1' : ''}`);
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      setData((await r.json()) as ReservoirResponse);
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'reservoir fetch failed');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void load(false); }, [load]);

  // Dataset roster for the upload target picker. A failure here is NOT fatal
  // to the totals above, but it must be visible: /data-partners/datasets was
  // answering 500 in prod on 2026-09-16 (its per-dataset correlated counts
  // exceed the pool's 30s statement_timeout), which leaves the picker empty.
  // An empty picker with no reason reads as "there are no datasets".
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        // counts=0: the picker needs id/name/vertical only, and the counted
        // variant exceeds the DB's 30s statement timeout at this table size.
        const r = await apiFetch('/api/mailing/data-partners/datasets?counts=0');
        if (!r.ok) {
          let detail = `HTTP ${r.status}`;
          try {
            const j = (await r.json()) as { error?: string };
            if (j.error) detail = j.error;
          } catch { /* non-JSON error body */ }
          throw new Error(detail);
        }
        const j = (await r.json()) as { datasets?: Array<{ id: string; name: string; partner_name?: string; vertical: string }> };
        if (cancelled) return;
        setFeedsErr(null);
        setFeeds((j.datasets ?? []).map((d) => ({
          dataset_id: d.id,
          name: d.partner_name ? `${d.partner_name} / ${d.name}` : d.name,
          vertical: d.vertical,
        })));
      } catch (e) {
        if (!cancelled) setFeedsErr(e instanceof Error ? e.message : 'dataset roster fetch failed');
      }
    })();
    return () => { cancelled = true; };
  }, []);

  const reserveTotal = useMemo(
    () => (data?.by_vertical ?? []).reduce((s, v) => s + v.reserve, 0), [data]);

  const ispRows = useMemo(() => {
    const v = (data?.by_vertical ?? []).find((x) => x.vertical === expanded);
    if (!v) return [];
    return Object.entries(v.by_isp).sort((a, b) => b[1] - a[1]);
  }, [data, expanded]);

  return (
    <div>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 10, alignItems: 'center', marginBottom: 12 }}>
        <h3 style={{ margin: 0, color: '#dbeafe', fontSize: 16 }}>
          <FontAwesomeIcon icon={faDatabase} style={{ marginRight: 8, opacity: 0.8 }} />
          Reservoir
        </h3>
        <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.65)' }}>
          {data
            ? `${data.cached ? `cached ${data.age_seconds}s` : 'fresh'} · scan ${num(data.query_ms)}ms · ${new Date(data.generated_at).toLocaleTimeString()}`
            : 'loading…'}
        </div>
        <div style={{ marginLeft: 'auto', display: 'flex', gap: 8 }}>
          <button style={btn} onClick={() => void load(true)} disabled={loading}>
            <FontAwesomeIcon icon={loading ? faSpinner : faSync} spin={loading} style={{ marginRight: 6 }} />
            Recount
          </button>
          <button style={btn} onClick={() => setUploadOpen((o) => !o)}>
            <FontAwesomeIcon icon={uploadOpen ? faXmark : faUpload} style={{ marginRight: 6 }} />
            {uploadOpen ? 'Close upload' : 'Upload a file'}
          </button>
        </div>
      </div>

      {notice && (
        <div style={{ ...card, borderColor: 'rgba(16,185,129,0.4)', marginBottom: 12, fontSize: 13 }}>{notice}</div>
      )}

      {uploadOpen && feedsErr && (
        <div style={{ ...card, borderColor: 'rgba(239,68,68,0.45)', color: '#fecaca', marginBottom: 12 }}>
          <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 8 }} />
          The dataset list could not be loaded, so there is nothing to upload into: {feedsErr}
        </div>
      )}

      {uploadOpen && (
        <div style={{ ...card, marginBottom: 14 }}>
          <DripCsvUpload
            feeds={feeds}
            onNotice={(s) => setNotice(s)}
            onCommitted={() => { void load(true); }}
            onClose={() => setUploadOpen(false)}
          />
        </div>
      )}

      {err && (
        <div style={{ ...card, borderColor: 'rgba(239,68,68,0.45)', color: '#fecaca', marginBottom: 12 }}>
          <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 8 }} />
          Reservoir totals unavailable: {err}. Nothing below is a real zero.
        </div>
      )}

      {data && (
        <>
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 10, marginBottom: 14 }}>
            <BigNumber label="Mailable now" value={data.mailable_total} tone="#6ee7b7" hint="status ready" />
            <BigNumber label="Reserve" value={reserveTotal} tone="#fcd34d" hint="held or awaiting verification" />
            <BigNumber label="Total records" value={data.total} tone="#c7d2fe" hint="every row in the queue" />
          </div>

          <div style={{ ...card, padding: 0, overflowX: 'auto', marginBottom: 14 }}>
            <table style={{ width: '100%', borderCollapse: 'collapse', minWidth: 660 }}>
              <thead>
                <tr>
                  <th style={th}>Vertical</th>
                  <th style={{ ...th, textAlign: 'right' }}>Mailable</th>
                  <th style={{ ...th, textAlign: 'right' }}>Reserve</th>
                  <th style={{ ...th, textAlign: 'right' }}>Spent</th>
                  <th style={{ ...th, textAlign: 'right' }}>Total</th>
                  <th style={th}>By ISP</th>
                </tr>
              </thead>
              <tbody>
                {data.by_vertical.map((v) => (
                  <tr key={v.vertical}>
                    <td style={td}>{labelForVertical(v.vertical)}</td>
                    <td style={{ ...tdNum, color: v.mailable > 0 ? '#6ee7b7' : 'rgba(220,235,250,0.5)' }}>{num(v.mailable)}</td>
                    <td style={tdNum}>{num(v.reserve)}</td>
                    <td style={{ ...tdNum, color: 'rgba(220,235,250,0.55)' }}>{num(v.exhausted)}</td>
                    <td style={tdNum}>{num(v.total)}</td>
                    <td style={td}>
                      <button
                        style={{ ...btn, padding: '3px 9px', fontSize: 11 }}
                        onClick={() => setExpanded(expanded === v.vertical ? null : v.vertical)}
                      >
                        {expanded === v.vertical ? 'Hide' : 'Show'}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          {expanded && (
            <div style={{ ...card, marginBottom: 14 }}>
              <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.75)', marginBottom: 8 }}>
                {labelForVertical(expanded)} — every record by ISP (all statuses)
              </div>
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
                {ispRows.map(([isp, n]) => (
                  <div key={isp} style={{ ...card, padding: '8px 12px', minWidth: 120 }}>
                    <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.7)' }}>{isp}</div>
                    <div style={{ fontSize: 17, fontWeight: 700, fontVariantNumeric: 'tabular-nums' }}>{num(n)}</div>
                  </div>
                ))}
              </div>
            </div>
          )}

          <div style={{ ...card }}>
            <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.75)', marginBottom: 8 }}>Every status</div>
            <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
              {data.by_status.map((s) => (
                <div key={s.status} style={{ ...card, padding: '8px 12px', minWidth: 130 }}>
                  <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.7)' }}>{s.status}</div>
                  <div style={{ fontSize: 17, fontWeight: 700, fontVariantNumeric: 'tabular-nums' }}>{num(s.count)}</div>
                </div>
              ))}
            </div>
          </div>
        </>
      )}
    </div>
  );
};

export default ReservoirPanel;
