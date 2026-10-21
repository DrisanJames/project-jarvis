import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faRotateRight, faSpinner } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import './InboxScore.css';

/**
 * Inbox Score — "Will this email land in the inbox?"
 *
 * One row per campaign, one column per mailbox provider. Each cell shows two
 * separate facts and never blends them:
 *   1. Chance of inbox — a spam-filter profile trained on OUR past sends to
 *      that provider scored the exact bytes this campaign sends. Shown as
 *      "8 in 10" with the profile's own test score ("right 9 in 10 times").
 *   2. Test inbox — where the real send landed in a real mailbox we own at
 *      that provider (inbox / promotions / spam / not seen yet).
 *
 * Data: GET /api/mailing/inbox-score/summary?from&to (internal/api/inbox_score.go).
 */

type Cell = {
  isp: string;
  p_inbox: number | null;
  sa_score: number | null;
  profile_version: number;
  profile_auc: number | null;
  reasons: string[] | unknown;
  scored_at: string | null;
  seed_folder: string;
  seed_email?: string;
  seed_observed_at?: string;
};

type CampaignRow = {
  campaign_id: string;
  name: string;
  sending_domain: string;
  status: string;
  scheduled_at: string | null;
  cells: Cell[];
};

type Summary = {
  as_of: string;
  from: string;
  to: string;
  timezone: string;
  isps: string[];
  campaigns: CampaignRow[];
  how_to_read: string[];
};

const ISP_LABEL: Record<string, string> = {
  gmail: 'Gmail',
  yahoo_family: 'Yahoo / AOL / AT&T',
  microsoft: 'Outlook / Hotmail',
  apple: 'Apple iCloud',
  cables: 'Cable (Charter, Comcast, Cox)',
};

const FOLDER_LABEL: Record<string, string> = {
  inbox: 'Inbox',
  promotions: 'Promotions tab',
  spam: 'Spam',
  missing: 'Not delivered',
  other: 'Other folder',
};

function denverToday(): string {
  const d = new Date();
  const parts = new Intl.DateTimeFormat('en-CA', { timeZone: 'America/Denver', year: 'numeric', month: '2-digit', day: '2-digit' }).formatToParts(d);
  const get = (t: string) => parts.find((p) => p.type === t)?.value ?? '';
  return `${get('year')}-${get('month')}-${get('day')}`;
}

/** "8 in 10" from a 0..1 probability; null when there is no score yet. */
export function tenScale(p: number | null | undefined): string | null {
  if (p === null || p === undefined || Number.isNaN(p)) return null;
  const n = Math.round(Math.min(1, Math.max(0, p)) * 10);
  return `${n} in 10`;
}

/** Band for the chance column — plain words, no colors without words. */
export function chanceBand(p: number | null | undefined): 'good' | 'mixed' | 'poor' | 'none' {
  if (p === null || p === undefined) return 'none';
  if (p >= 0.7) return 'good';
  if (p >= 0.4) return 'mixed';
  return 'poor';
}

/** The profile's own test score drives whether the guess is shown as trustworthy. */
export function trustLabel(auc: number | null | undefined): string {
  if (auc === null || auc === undefined) return 'not tested yet';
  const n = Math.round(auc * 10);
  return `right ${n} in 10 times on past sends`;
}

function reasonsList(r: Cell['reasons']): string[] {
  if (Array.isArray(r)) return r.filter((x): x is string => typeof x === 'string');
  return [];
}

const InboxScore: React.FC = () => {
  const today = denverToday();
  const [from, setFrom] = useState(today);
  const [to, setTo] = useState(today);
  const [data, setData] = useState<Summary | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [open, setOpen] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await apiFetch(`/api/mailing/inbox-score/summary?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}`);
      if (!res.ok) {
        const body = await res.json().catch(() => ({}));
        throw new Error(body?.error || `HTTP ${res.status}`);
      }
      setData((await res.json()) as Summary);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not load inbox scores');
    } finally {
      setLoading(false);
    }
  }, [from, to]);

  useEffect(() => {
    void load();
  }, [load]);

  const isps = useMemo(() => data?.isps ?? ['gmail', 'yahoo_family', 'microsoft', 'apple', 'cables'], [data]);

  return (
    <div className="inbox-score">
      <header className="inbox-score__head">
        <div>
          <h2>Inbox Score</h2>
          <p className="inbox-score__sub">Will this email land in the inbox? One guess per mailbox provider, and what our real test inbox saw.</p>
        </div>
        <div className="inbox-score__controls">
          <label>
            From
            <input id="inbox-score-from" type="date" value={from} onChange={(e) => setFrom(e.target.value)} />
          </label>
          <label>
            To
            <input id="inbox-score-to" type="date" value={to} onChange={(e) => setTo(e.target.value)} />
          </label>
          <button id="inbox-score-refresh" type="button" onClick={() => void load()} disabled={loading}>
            <FontAwesomeIcon icon={loading ? faSpinner : faRotateRight} spin={loading} /> Refresh
          </button>
        </div>
      </header>

      {error && <div className="inbox-score__error">{error}</div>}

      <section className="inbox-score__howto">
        <h3>How to read this</h3>
        <ul>
          {(data?.how_to_read ?? []).map((line, i) => (
            <li key={i}>{line}</li>
          ))}
        </ul>
      </section>

      {!loading && data && data.campaigns.length === 0 && (
        <div className="inbox-score__empty">No scored campaigns between {data.from} and {data.to}. Scores appear after the nightly scan runs on tomorrow&apos;s campaigns.</div>
      )}

      {data && data.campaigns.length > 0 && (
        <div className="inbox-score__tablewrap">
          <table className="inbox-score__table">
            <thead>
              <tr>
                <th>Campaign</th>
                {isps.map((isp) => (
                  <th key={isp}>{ISP_LABEL[isp] ?? isp}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {data.campaigns.map((c) => (
                <React.Fragment key={c.campaign_id}>
                  <tr className={open === c.campaign_id ? 'is-open' : ''} onClick={() => setOpen(open === c.campaign_id ? null : c.campaign_id)}>
                    <td className="inbox-score__camp">
                      <div className="inbox-score__name">{c.name}</div>
                      <div className="inbox-score__meta">
                        {c.sending_domain}
                        {c.scheduled_at ? ` · ${c.scheduled_at.slice(0, 16).replace('T', ' ')} MT` : ''} · {c.status}
                      </div>
                    </td>
                    {isps.map((isp) => {
                      const cell = c.cells.find((x) => x.isp === isp);
                      const chance = tenScale(cell?.p_inbox);
                      const band = chanceBand(cell?.p_inbox);
                      const folder = cell?.seed_folder ? FOLDER_LABEL[cell.seed_folder] ?? cell.seed_folder : '';
                      return (
                        <td key={isp} className={`inbox-score__cell band-${band}`}>
                          <div className="inbox-score__chance">{chance ? `${chance} chance of inbox` : 'no guess yet'}</div>
                          <div className="inbox-score__trust">{cell && chance ? `guess is ${trustLabel(cell.profile_auc)}` : ''}</div>
                          <div className={`inbox-score__seed seed-${cell?.seed_folder || 'none'}`}>
                            {folder ? `Test inbox: ${folder}` : 'Test inbox: not seen yet'}
                          </div>
                        </td>
                      );
                    })}
                  </tr>
                  {open === c.campaign_id && (
                    <tr className="inbox-score__detail">
                      <td colSpan={isps.length + 1}>
                        <div className="inbox-score__reasons">
                          {isps.map((isp) => {
                            const cell = c.cells.find((x) => x.isp === isp);
                            const reasons = cell ? reasonsList(cell.reasons) : [];
                            return (
                              <div key={isp} className="inbox-score__reason-block">
                                <h4>{ISP_LABEL[isp] ?? isp}</h4>
                                {reasons.length === 0 ? (
                                  <p className="muted">No reasons recorded yet.</p>
                                ) : (
                                  <ul>
                                    {reasons.map((r, i) => (
                                      <li key={i}>{r}</li>
                                    ))}
                                  </ul>
                                )}
                                {cell?.sa_score !== null && cell?.sa_score !== undefined && (
                                  <p className="muted">Filter score {cell.sa_score.toFixed(1)} (lower is better) · profile v{cell.profile_version}</p>
                                )}
                                {cell?.seed_email && (
                                  <p className="muted">
                                    Test inbox {cell.seed_email}
                                    {cell.seed_observed_at ? ` checked ${cell.seed_observed_at.slice(0, 16).replace('T', ' ')} UTC` : ''}
                                  </p>
                                )}
                              </div>
                            );
                          })}
                        </div>
                      </td>
                    </tr>
                  )}
                </React.Fragment>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
};

export default InboxScore;
