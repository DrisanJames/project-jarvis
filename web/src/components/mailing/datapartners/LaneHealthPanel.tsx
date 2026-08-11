import React, { useCallback, useEffect, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faSpinner, faTriangleExclamation, faCircleCheck, faSync } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import { labelForVertical } from './verticalLabels';

// LaneHealthPanel — the delivery + integrity + attribution view for one partner
// lane. Deliberately NOT another engagement panel: FeedFunnelBoard already
// covers records -> mailed -> engaged, and DripPerformancePanel covers pacing.
// This shows the three things that were invisible on 2026-08-11 while the lane
// looked healthy everywhere else:
//
//   DELIVERY   an ISP can accept 0% while the lane "mails" perfectly
//              (Comcast: 82 sent, 0 delivered, 66 blocked).
//   INTEGRITY  'mailed' is a terminal CLAIM stamp, not a send. A claimed-vs-sent
//              gap means records were consumed and will never be retried.
//   ATTRIBUTION whether a conversion could be measured at all. "0 conversions"
//              and "conversions are unmeasurable" look identical everywhere else.

interface ISPRow {
  isp: string;
  received: number; claimed: number; ready: number;
  suppressed: number; dead_letter: number;
  sent: number; delivered: number; bounced: number;
}

interface LaneHealth {
  vertical: string;
  ingest: { records: number; last_hour: number; last_record_at: string | null };
  funnel: Record<string, number>;
  integrity: { claimed: number; recipients: number; sent: number; burned: number };
  isps: ISPRow[];
  attribution: {
    claimed: number; with_tid: number; with_first_name: number;
    offer_name: string; everflow_offer_id: string; conversions: number;
  };
}

const pct = (n: number, d: number) => (d > 0 ? `${((100 * n) / d).toFixed(1)}%` : '—');

// Delivery verdict per ISP. Deliberately three-state and glyph-labelled (never
// colour alone): a lane with nothing sent yet is UNKNOWN, not healthy.
const verdict = (r: ISPRow): { label: string; tone: string } => {
  if (r.sent === 0) return { label: 'no sends', tone: 'text-gray-500' };
  if (r.bounced > r.delivered && r.bounced >= 10) return { label: '● blocked', tone: 'text-red-400' };
  const settled = r.delivered + r.bounced;
  if (settled < r.sent * 0.4) return { label: '◐ confirming', tone: 'text-amber-400' };
  if (r.delivered >= settled * 0.8) return { label: '● ok', tone: 'text-emerald-400' };
  return { label: '◐ degraded', tone: 'text-amber-400' };
};

export const LaneHealthPanel: React.FC<{ vertical: string }> = ({ vertical }) => {
  const [data, setData] = useState<LaneHealth | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const res = await apiFetch(`/api/mailing/data-partners/lanes/${encodeURIComponent(vertical)}/health`);
      if (!res.ok) throw new Error(`${res.status}`);
      setData(await res.json());
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'failed to load lane health');
    } finally {
      setLoading(false);
    }
  }, [vertical]);

  useEffect(() => {
    setLoading(true);
    load();
    const t = setInterval(load, 60000);
    return () => clearInterval(t);
  }, [load]);

  if (loading) {
    return <div className="p-8 text-center text-gray-400"><FontAwesomeIcon icon={faSpinner} spin /> Loading lane health…</div>;
  }
  if (error) {
    return <div className="p-4 rounded bg-red-900/30 border border-red-700 text-red-200">Lane health unavailable: {error}</div>;
  }
  if (!data) return null;

  const { integrity: integ, attribution: attr } = data;
  const tidPct = attr.claimed > 0 ? (100 * attr.with_tid) / attr.claimed : 0;
  const attributionBroken = attr.claimed > 0 && (tidPct < 50 || attr.everflow_offer_id === '');

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <h3 className="text-lg font-semibold text-gray-100">
          Lane Health — {labelForVertical(data.vertical)}
        </h3>
        <button onClick={load} className="text-sm text-gray-400 hover:text-gray-200">
          <FontAwesomeIcon icon={faSync} /> Refresh
        </button>
      </div>

      {/* Headline: ingest + the send chain */}
      <div className="grid grid-cols-2 md:grid-cols-5 gap-3">
        {[
          { k: 'Received', v: data.ingest.records, s: `${data.ingest.last_hour}/hr` },
          { k: 'Ready', v: data.funnel['ready'] ?? 0, s: 'awaiting a wave' },
          { k: 'Claimed', v: integ.claimed, s: 'stamped mailed' },
          { k: 'Sent', v: integ.sent, s: 'left the building' },
          { k: 'Burned', v: integ.burned, s: 'claimed, never sent' },
        ].map((c) => (
          <div key={c.k} className={`rounded-lg border p-3 ${
            c.k === 'Burned' && c.v > 0 ? 'bg-red-900/20 border-red-700' : 'bg-gray-800/60 border-gray-700'}`}>
            <div className="text-xs uppercase tracking-wide text-gray-400">{c.k}</div>
            <div className={`text-2xl font-semibold ${c.k === 'Burned' && c.v > 0 ? 'text-red-300' : 'text-gray-100'}`}>
              {c.v.toLocaleString()}
            </div>
            <div className="text-xs text-gray-500">{c.s}</div>
          </div>
        ))}
      </div>

      {integ.burned > 0 && (
        <div className="rounded-lg border border-red-700 bg-red-900/20 p-3 text-sm text-red-200">
          <FontAwesomeIcon icon={faTriangleExclamation} className="mr-2" />
          <strong>{integ.burned.toLocaleString()} records were claimed but never sent</strong> and sit on
          campaigns that already finished — they will not be retried. Recover with{' '}
          <code className="text-red-100">python3 -m agents.jobs.partner_burned_record_salvage --confirm-live</code>.
        </div>
      )}

      {/* Delivery per ISP */}
      <div>
        <h4 className="text-sm font-semibold text-gray-300 mb-2">Delivery by ISP</h4>
        <div className="overflow-x-auto rounded-lg border border-gray-700">
          <table className="min-w-full text-sm">
            <thead className="bg-gray-800/80 text-gray-400">
              <tr>
                {['ISP', 'Received', 'Ready', 'Sent', 'Delivered', 'Deliv %', 'Bounced', 'Status'].map((h) => (
                  <th key={h} className="px-3 py-2 text-left font-medium whitespace-nowrap">{h}</th>
                ))}
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-800">
              {data.isps.map((r) => {
                const v = verdict(r);
                return (
                  <tr key={r.isp} className="hover:bg-gray-800/40">
                    <td className="px-3 py-2 text-gray-200">{r.isp}</td>
                    <td className="px-3 py-2 text-gray-400">{r.received.toLocaleString()}</td>
                    <td className="px-3 py-2 text-gray-400">{r.ready.toLocaleString()}</td>
                    <td className="px-3 py-2 text-gray-200">{r.sent.toLocaleString()}</td>
                    <td className="px-3 py-2 text-gray-200">{r.delivered.toLocaleString()}</td>
                    <td className="px-3 py-2 text-gray-300">{pct(r.delivered, r.sent)}</td>
                    <td className={`px-3 py-2 ${r.bounced > 0 ? 'text-amber-300' : 'text-gray-500'}`}>
                      {r.bounced.toLocaleString()}
                    </td>
                    <td className={`px-3 py-2 whitespace-nowrap ${v.tone}`}>{v.label}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
        <p className="mt-2 text-xs text-gray-500">
          “confirming” means most sends have no delivery confirmation yet — normal for Microsoft,
          which confirms on a ~3h lag while other ISPs take 4–8 minutes. Judge it after that window.
        </p>
      </div>

      {/* Attribution readiness */}
      <div>
        <h4 className="text-sm font-semibold text-gray-300 mb-2">Conversion attribution</h4>
        <div className={`rounded-lg border p-4 ${
          attributionBroken ? 'bg-amber-900/20 border-amber-700' : 'bg-gray-800/60 border-gray-700'}`}>
          <div className="flex items-start gap-3">
            <FontAwesomeIcon
              icon={attributionBroken ? faTriangleExclamation : faCircleCheck}
              className={attributionBroken ? 'text-amber-400 mt-1' : 'text-emerald-400 mt-1'} />
            <div className="text-sm">
              {attributionBroken ? (
                <>
                  <div className="font-semibold text-amber-200">
                    Conversions cannot be attributed on this lane.
                  </div>
                  <ul className="mt-1 space-y-0.5 text-amber-100/80">
                    {tidPct < 50 && (
                      <li>
                        • the money link’s <code>tokenid</code> comes from <code>custom.tid</code>, populated on{' '}
                        <strong>{attr.with_tid.toLocaleString()} of {attr.claimed.toLocaleString()} ({tidPct.toFixed(1)}%)</strong>{' '}
                        — the partner feed must supply it
                      </li>
                    )}
                    {attr.everflow_offer_id === '' && (
                      <li>• offer <strong>{attr.offer_name || '(unnamed)'}</strong> has no <code>everflow_offer_id</code>, so postbacks have nowhere to land</li>
                    )}
                  </ul>
                  <div className="mt-2 text-amber-100/70">
                    Recorded conversions: <strong>{attr.conversions.toLocaleString()}</strong> — treat as
                    “unmeasured”, not “none”.
                  </div>
                </>
              ) : (
                <div className="text-emerald-200">
                  Attribution wired — {attr.conversions.toLocaleString()} conversions recorded.
                </div>
              )}
            </div>
          </div>
        </div>
      </div>
    </div>
  );
};
