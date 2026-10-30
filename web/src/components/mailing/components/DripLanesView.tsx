// Drip Lanes — operator control surface for partner-drip lanes
// (operator 2026-08-05: "I should be able to manage this from the UI").
//
// One row per lane: offer binding, daily new-record budget, pause state, and
// live queue depth (ready / held / mailed today). The daily budget is enforced
// supply-side by agents/jobs/drip_lane_release.py — it keeps at most
// `daily_cap` rows 'ready' and parks the remainder 'held' (0 = uncapped).

import React, { useCallback, useEffect, useState } from 'react';
import { useAuth } from '../../../contexts/AuthContext';

const DEFAULT_ORG = '00000000-0000-0000-0000-000000000001';

interface Lane {
  dataset_id: string;
  slug: string;
  vertical: string;
  offer_id: string;
  offer_name: string;
  daily_cap: number;
  paused: boolean;
  ready: number;
  held: number;
  mailed_today: number;
}
interface Offer { id: string; name: string; }

const num = (n: number) => (n || 0).toLocaleString();

export const DripLanesView: React.FC = () => {
  const { organization } = useAuth();
  const orgId = organization?.id ?? DEFAULT_ORG;
  const headers = React.useMemo(
    () => ({ 'X-Organization-ID': orgId, 'Content-Type': 'application/json' }), [orgId]);

  const [lanes, setLanes] = useState<Lane[]>([]);
  const [offers, setOffers] = useState<Offer[]>([]);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [offersErr, setOffersErr] = useState<string | null>(null);
  const [edit, setEdit] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState<Record<string, boolean>>({});

  const load = useCallback(async () => {
    setLoading(true); setErr(null);
    try {
      const r = await fetch('/api/mailing/pmta-campaign/drip-lanes', { headers });
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      const j = await r.json();
      setLanes(j.lanes ?? []);
      // /api/mailing/offers 404s in prod — the real catalog endpoint is
      // /offers/list → {offers:[{id,key,name,everflow_id,status}]}. A failure
      // is surfaced (offersErr), never a silently empty dropdown.
      try {
        const o = await fetch('/api/mailing/offers/list', { headers });
        if (!o.ok) {
          setOffers([]);
          setOffersErr(`offer catalog unavailable: HTTP ${o.status}`);
        } else {
          const oj: { offers?: Array<{ id: string; name: string; status?: string }> } = await o.json();
          setOffers((oj.offers ?? [])
            .filter(x => x.status === 'active')
            .map(x => ({ id: x.id, name: x.name }))
            .sort((a, b) => a.name.localeCompare(b.name)));
          setOffersErr(null);
        }
      } catch (oe) {
        setOffers([]);
        setOffersErr(`offer catalog unavailable: ${oe instanceof Error ? oe.message : 'network error'}`);
      }
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'load failed');
    } finally { setLoading(false); }
  }, [headers]);

  useEffect(() => { void load(); }, [load]);

  const save = async (l: Lane, patch: Record<string, unknown>) => {
    setBusy(b => ({ ...b, [l.dataset_id]: true }));
    try {
      const r = await fetch('/api/mailing/pmta-campaign/drip-lanes/update', {
        method: 'POST', headers,
        body: JSON.stringify({ dataset_id: l.dataset_id, ...patch }),
      });
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      await load();
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'save failed');
    } finally { setBusy(b => ({ ...b, [l.dataset_id]: false })); }
  };

  const th: React.CSSProperties = {
    textAlign: 'left', padding: '8px 10px', fontSize: 11, letterSpacing: 0.6,
    color: 'color-mix(in srgb, var(--j-text) 65%, transparent)', textTransform: 'uppercase', whiteSpace: 'nowrap',
  };
  const td: React.CSSProperties = {
    padding: '8px 10px', fontSize: 13, color: 'var(--j-text)',
    borderTop: '1px solid color-mix(in srgb, var(--j-text) 6%, transparent)', whiteSpace: 'nowrap',
  };
  const totalCapped = lanes.filter(l => l.daily_cap > 0).reduce((a, l) => a + l.daily_cap, 0);

  return (
    <div style={{ padding: 20 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 14, marginBottom: 14 }}>
        <h2 style={{ margin: 0, fontSize: 18, color: 'var(--j-text)' }}>Drip Lanes</h2>
        <span style={{ fontSize: 12, color: 'color-mix(in srgb, var(--j-text) 70%, transparent)' }}>
          {lanes.length} lanes · {num(totalCapped)}/day capped
          {lanes.some(l => l.daily_cap === 0) ? ' (+ uncapped lanes)' : ''}
        </span>
        <button onClick={() => void load()} style={{
          marginLeft: 'auto', background: 'transparent', color: '#9fd3ff',
          border: '1px solid rgba(120,180,255,0.35)', borderRadius: 6,
          padding: '6px 12px', fontSize: 12, cursor: 'pointer',
        }}>{loading ? 'Loading…' : 'Refresh'}</button>
      </div>
      {err && <div style={{ color: 'var(--j-danger)', fontSize: 12, marginBottom: 10 }}>{err}</div>}
      {offersErr && (
        <div style={{
          display: 'inline-block', color: 'var(--j-danger)', fontSize: 12, fontWeight: 700,
          background: 'color-mix(in srgb, var(--j-danger) 10%, transparent)', border: '1px solid color-mix(in srgb, var(--j-danger) 40%, transparent)',
          borderRadius: 6, padding: '5px 10px', marginBottom: 10,
        }}>
          {offersErr} — offer dropdowns are disabled until it loads.
        </div>
      )}
      <div style={{ overflowX: 'auto' }}>
        <table style={{ borderCollapse: 'collapse', width: '100%', minWidth: 900 }}>
          <thead><tr>
            <th style={th}>Lane</th><th style={th}>Vertical</th><th style={th}>Offer</th>
            <th style={{ ...th, textAlign: 'right' }}>Daily cap</th>
            <th style={{ ...th, textAlign: 'right' }}>Ready</th>
            <th style={{ ...th, textAlign: 'right' }}>Held</th>
            <th style={{ ...th, textAlign: 'right' }}>Mailed today</th>
            <th style={th}>State</th>
          </tr></thead>
          <tbody>
            {lanes.map(l => (
              <tr key={l.dataset_id} style={{ opacity: l.paused ? 0.55 : 1 }}>
                <td style={{ ...td, fontWeight: 600 }}>{l.slug}</td>
                <td style={{ ...td, color: 'color-mix(in srgb, var(--j-text) 75%, transparent)' }}>{l.vertical}</td>
                <td style={td}>
                  <select
                    value={l.offer_id || 'none'}
                    disabled={busy[l.dataset_id] || !!offersErr}
                    title={offersErr ?? undefined}
                    onChange={e => void save(l, { offer_id: e.target.value })}
                    style={{ background: 'color-mix(in srgb, var(--j-text) 6%, transparent)', color: 'var(--j-text)',
                             border: '1px solid color-mix(in srgb, var(--j-text) 15%, transparent)', borderRadius: 5,
                             padding: '4px 6px', fontSize: 12, maxWidth: 240 }}>
                    <option value="none">(none — vertical default)</option>
                    {offers.map(o => <option key={o.id} value={o.id}>{o.name}</option>)}
                  </select>
                </td>
                <td style={{ ...td, textAlign: 'right' }}>
                  <input
                    type="number" min={0} step={1000}
                    value={edit[l.dataset_id] ?? String(l.daily_cap)}
                    disabled={busy[l.dataset_id]}
                    onChange={e => setEdit(s => ({ ...s, [l.dataset_id]: e.target.value }))}
                    onBlur={e => {
                      const v = parseInt(e.target.value, 10);
                      if (!Number.isNaN(v) && v !== l.daily_cap) void save(l, { daily_cap: v });
                    }}
                    style={{ width: 90, textAlign: 'right', background: 'color-mix(in srgb, var(--j-text) 6%, transparent)',
                             color: 'var(--j-text)', border: '1px solid color-mix(in srgb, var(--j-text) 15%, transparent)',
                             borderRadius: 5, padding: '4px 6px', fontSize: 12 }} />
                  {l.daily_cap === 0 && <span style={{ fontSize: 10, color: 'var(--j-warn)', marginLeft: 6 }}>UNCAPPED</span>}
                </td>
                <td style={{ ...td, textAlign: 'right' }}>{num(l.ready)}</td>
                <td style={{ ...td, textAlign: 'right', color: 'color-mix(in srgb, var(--j-text) 60%, transparent)' }}>{num(l.held)}</td>
                <td style={{ ...td, textAlign: 'right', color: l.mailed_today ? 'var(--j-success)' : 'color-mix(in srgb, var(--j-text) 50%, transparent)' }}>
                  {num(l.mailed_today)}
                </td>
                <td style={td}>
                  <button
                    onClick={() => void save(l, { paused: !l.paused })}
                    disabled={busy[l.dataset_id]}
                    style={{ background: l.paused ? 'color-mix(in srgb, var(--j-danger) 15%, transparent)' : 'color-mix(in srgb, var(--j-success) 12%, transparent)',
                             color: l.paused ? 'var(--j-danger)' : 'var(--j-success)',
                             border: `1px solid ${l.paused ? 'color-mix(in srgb, var(--j-danger) 40%, transparent)' : 'color-mix(in srgb, var(--j-success) 40%, transparent)'}`,
                             borderRadius: 999, padding: '3px 10px', fontSize: 11,
                             fontWeight: 700, cursor: 'pointer' }}>
                    {l.paused ? 'PAUSED' : 'ACTIVE'}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <p style={{ fontSize: 11, color: 'color-mix(in srgb, var(--j-text) 55%, transparent)', marginTop: 14, lineHeight: 1.6 }}>
        Daily cap = new records released to the drip per day (0 = uncapped). Enforced supply-side:
        the release job keeps at most this many rows claimable and parks the rest as held reserve.
        Held records are never lost and never mailed until released.
      </p>
    </div>
  );
};

export default DripLanesView;
