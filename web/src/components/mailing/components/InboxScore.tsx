import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faRotateRight, faSpinner } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import './InboxScore.css';

/**
 * Inbox Score — offers only, grouped by offer, one column per mailbox provider.
 *
 * Each cell shows ONE verdict, in this order of truth:
 *   1. Test inbox (the defender): our own mailbox on that provider's real lane
 *      received the live send; mailcow's rspamd scored it. Inbox / Junk + score of 15.
 *   2. Pre-send: the scan before sending (SpamAssassin scale). Same copy for every
 *      provider until a provider's own model is trusted.
 * Newsletters are hidden unless the toggle is flipped (operator 2026-10-22).
 *
 * Data: GET /api/mailing/inbox-score/summary?from&to (internal/api/inbox_score.go).
 */

type ProfileBrief = { version: number; status: string; auc: number | null; finding: string };

type Symbol = { symbol: string; score: number; detail?: string };

type Cell = {
  isp: string;
  p_inbox: number | null;
  sa_score: number | null;
  profile_version: number;
  profile_auc: number | null;
  reasons: string[] | unknown;
  scored_at: string | null;
  seed_folder: string;
  seed_score?: number | null;
  seed_action?: string;
  seed_symbols?: Symbol[] | unknown;
  seed_email?: string;
  seed_observed_at?: string;
};

type CampaignRow = {
  campaign_id: string;
  name: string;
  sending_domain: string;
  status: string;
  scheduled_at: string | null;
  kind: string;
  offer: string;
  subject: string;
  cells: Cell[];
};

type Summary = {
  as_of: string;
  from: string;
  to: string;
  timezone: string;
  isps: string[];
  campaigns: CampaignRow[];
  profiles?: Record<string, ProfileBrief>;
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
  promotions: 'Promotions',
  spam: 'Junk',
  missing: 'Not delivered',
  other: 'Other folder',
};

type Band = 'ready' | 'review' | 'act' | 'junk' | 'none';
const BAND_WORDS: Record<Band, string> = { ready: 'ready', review: 'review', act: 'fix before send', junk: 'junked', none: '' };

function denverToday(): string {
  const parts = new Intl.DateTimeFormat('en-CA', { timeZone: 'America/Denver', year: 'numeric', month: '2-digit', day: '2-digit' }).formatToParts(new Date());
  const get = (t: string) => parts.find((p) => p.type === t)?.value ?? '';
  return `${get('year')}-${get('month')}-${get('day')}`;
}

/** The defender (mailcow rspamd, 0..15, junk at 8) in the operator's bands:
 *  ready < 2.4 · review < 4.6 · fix before send < 8 · junked ≥ 8. */
export function defenderBand(score: number | null | undefined): Band {
  if (score === null || score === undefined) return 'none';
  if (score < 2.4) return 'ready';
  if (score < 4.6) return 'review';
  if (score < 8) return 'act';
  return 'junk';
}

/** The pre-send scan (SpamAssassin scale, 5 = fail): ready < 1.5 · review < 3 · fix < 5 · fail ≥ 5. */
export function presendBand(score: number | null | undefined): Band {
  if (score === null || score === undefined) return 'none';
  if (score < 1.5) return 'ready';
  if (score < 3) return 'review';
  if (score < 5) return 'act';
  return 'junk';
}

export function tenScale(p: number | null | undefined): string | null {
  if (p === null || p === undefined || Number.isNaN(p)) return null;
  return `${Math.round(p * 10)} in 10`;
}

export function chanceBand(p: number | null | undefined): 'good' | 'mixed' | 'poor' | 'none' {
  if (p === null || p === undefined) return 'none';
  if (p >= 0.8) return 'good';
  if (p >= 0.5) return 'mixed';
  return 'poor';
}

export function trustLabel(auc: number | null | undefined): string {
  if (auc === null || auc === undefined) return 'untested';
  const n = Math.round(auc * 10);
  return `right ${n} in 10 times on past sends`;
}

function reasonsList(r: Cell['reasons']): string[] {
  if (Array.isArray(r)) return r.filter((x): x is string => typeof x === 'string');
  return [];
}

function symbolsList(s: Cell['seed_symbols']): Symbol[] {
  if (!Array.isArray(s)) return [];
  return s.filter((x): x is Symbol => !!x && typeof x === 'object' && typeof (x as Symbol).symbol === 'string');
}

/** What one cell says, in one line. The defender's real verdict wins over the pre-send scan. */
export function cellVerdict(cell: Cell | undefined): { text: string; band: Band; source: 'seed' | 'presend' | 'none' } {
  if (!cell) return { text: '—', band: 'none', source: 'none' };
  if (cell.seed_folder) {
    const folder = FOLDER_LABEL[cell.seed_folder] ?? cell.seed_folder;
    if (cell.seed_score !== null && cell.seed_score !== undefined) {
      const band = defenderBand(cell.seed_score);
      return { text: `${folder} · ${cell.seed_score.toFixed(1)} of 15 · ${BAND_WORDS[band]}`, band, source: 'seed' };
    }
    const band: Band = cell.seed_folder === 'inbox' ? 'ready' : cell.seed_folder === 'spam' || cell.seed_folder === 'missing' ? 'junk' : 'review';
    return { text: folder, band, source: 'seed' };
  }
  if (cell.sa_score !== null && cell.sa_score !== undefined) {
    const band = presendBand(cell.sa_score);
    return { text: `pre-send ${cell.sa_score.toFixed(1)} · ${BAND_WORDS[band]}`, band, source: 'presend' };
  }
  return { text: 'not scored', band: 'none', source: 'none' };
}

/** Roll one offer's campaigns up per provider: seed verdict counts when any seed read exists, else the median pre-send band. */
function offerRollup(rows: CampaignRow[], isp: string): { text: string; band: Band } {
  const cells = rows.map((r) => r.cells.find((c) => c.isp === isp)).filter((c): c is Cell => !!c);
  const seeds = cells.filter((c) => c.seed_folder);
  if (seeds.length > 0) {
    const inbox = seeds.filter((c) => c.seed_folder === 'inbox' || c.seed_folder === 'promotions').length;
    const junk = seeds.length - inbox;
    const scores = seeds.map((c) => c.seed_score).filter((s): s is number => typeof s === 'number').sort((a, b) => a - b);
    const med = scores.length ? scores[Math.floor(scores.length / 2)] : null;
    const band: Band = junk === 0 ? (med === null ? 'ready' : defenderBand(med)) : junk >= inbox ? 'junk' : 'act';
    return { text: `Inbox ${inbox} · Junk ${junk}${med !== null ? ` · median ${med.toFixed(1)}` : ''}`, band };
  }
  const scores = cells.map((c) => c.sa_score).filter((s): s is number => typeof s === 'number').sort((a, b) => a - b);
  if (scores.length === 0) return { text: '—', band: 'none' };
  const med = scores[Math.floor(scores.length / 2)];
  const band = presendBand(med);
  return { text: `pre-send ${med.toFixed(1)} · ${BAND_WORDS[band]} · no seed read yet`, band };
}

/** Reasons shared by every provider are shown once; the rest per provider. */
function splitReasons(cells: Cell[], isps: string[]): { shared: string[]; perIsp: Record<string, string[]> } {
  const lists = isps.map((isp) => reasonsList(cells.find((c) => c.isp === isp)?.reasons));
  const shared = lists.length ? lists[0].filter((r) => lists.every((l) => l.includes(r))) : [];
  const perIsp: Record<string, string[]> = {};
  isps.forEach((isp, i) => {
    perIsp[isp] = (lists[i] ?? []).filter((r) => !shared.includes(r));
  });
  return { shared, perIsp };
}

const InboxScore: React.FC = () => {
  const today = denverToday();
  const [from, setFrom] = useState(today);
  const [to, setTo] = useState(today);
  const [data, setData] = useState<Summary | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [showNewsletters, setShowNewsletters] = useState(false);
  const [openOffers, setOpenOffers] = useState<Record<string, boolean>>({});
  const [openCampaign, setOpenCampaign] = useState<string | null>(null);
  const [showLogs, setShowLogs] = useState(false);

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

  const groups = useMemo(() => {
    const rows = (data?.campaigns ?? []).filter((c) => showNewsletters || (c.kind ?? 'offer') !== 'newsletter');
    const byOffer = new Map<string, CampaignRow[]>();
    for (const r of rows) {
      const key = r.offer || 'Unknown offer';
      byOffer.set(key, [...(byOffer.get(key) ?? []), r]);
    }
    return [...byOffer.entries()]
      .map(([offer, list]) => ({ offer, rows: list.sort((a, b) => (a.scheduled_at ?? '').localeCompare(b.scheduled_at ?? '') || a.name.localeCompare(b.name)) }))
      .sort((a, b) => b.rows.length - a.rows.length || a.offer.localeCompare(b.offer));
  }, [data, showNewsletters]);

  const hidden = (data?.campaigns ?? []).filter((c) => (c.kind ?? 'offer') === 'newsletter').length;
  const seedReads = (data?.campaigns ?? []).reduce((n, c) => n + c.cells.filter((x) => x.seed_folder).length, 0);

  return (
    <div className="inbox-score">
      <header className="inbox-score__head">
        <div>
          <h2>Inbox Score</h2>
          <p className="inbox-score__sub">Every offer we send, scored by each mailbox provider&apos;s lane: the real test-inbox verdict when the send has landed, the pre-send scan until then.</p>
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
          <label className="inbox-score__toggle">
            <input id="inbox-score-newsletters" type="checkbox" checked={showNewsletters} onChange={(e) => setShowNewsletters(e.target.checked)} />
            show newsletters{hidden ? ` (${hidden} hidden)` : ''}
          </label>
          <button id="inbox-score-refresh" type="button" onClick={() => void load()} disabled={loading}>
            <FontAwesomeIcon icon={loading ? faSpinner : faRotateRight} spin={loading} /> Refresh
          </button>
        </div>
      </header>

      {error && <div className="inbox-score__error">{error}</div>}

      <section className="inbox-score__howto">
        <ul>
          {(data?.how_to_read ?? []).map((line, i) => (
            <li key={i}>{line}</li>
          ))}
        </ul>
        {data && (
          <p className="muted">
            {groups.reduce((n, g) => n + g.rows.length, 0)} offer campaigns · {seedReads} test-inbox reads
            {seedReads === 0 ? ' (seeds join every lane a campaign mails; reads appear as those campaigns send)' : ''}
          </p>
        )}
      </section>

      {data && data.profiles && Object.keys(data.profiles).length > 0 && (
        <section className="inbox-score__findings">
          <button type="button" className="inbox-score__linkbtn" onClick={() => setShowLogs((v) => !v)}>
            {showLogs ? 'Hide' : 'Show'} what the server logs say per provider
          </button>
          {showLogs && (
            <ul>
              {isps.map((isp) => {
                const b = data.profiles?.[isp];
                if (!b) return null;
                return (
                  <li key={isp}>
                    <strong>{ISP_LABEL[isp] ?? isp}</strong>
                    {b.status === 'active' ? ` — guess is ${trustLabel(b.auc)}. ` : ' — '}
                    {b.finding}
                  </li>
                );
              })}
            </ul>
          )}
        </section>
      )}

      {!loading && data && groups.length === 0 && (
        <div className="inbox-score__empty">No scored offer campaigns between {data.from} and {data.to}.{hidden ? ` ${hidden} newsletters are hidden.` : ''}</div>
      )}

      {groups.length > 0 && (
        <div className="inbox-score__tablewrap">
          <table className="inbox-score__table">
            <thead>
              <tr>
                <th>Offer / campaign</th>
                {isps.map((isp) => (
                  <th key={isp}>{ISP_LABEL[isp] ?? isp}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {groups.map((g) => {
                const isOpen = openOffers[g.offer] ?? false;
                return (
                  <React.Fragment key={g.offer}>
                    <tr className="inbox-score__offer" onClick={() => setOpenOffers((o) => ({ ...o, [g.offer]: !isOpen }))}>
                      <td className="inbox-score__camp">
                        <div className="inbox-score__name">
                          {isOpen ? '▾' : '▸'} {g.offer}
                        </div>
                        <div className="inbox-score__meta">{g.rows.length} campaign{g.rows.length === 1 ? '' : 's'} · {new Set(g.rows.map((r) => r.sending_domain)).size} sending domains</div>
                      </td>
                      {isps.map((isp) => {
                        const r = offerRollup(g.rows, isp);
                        return (
                          <td key={isp} className={`inbox-score__cell band-${r.band}`}>
                            <div className="inbox-score__verdict">{r.text}</div>
                          </td>
                        );
                      })}
                    </tr>
                    {isOpen &&
                      g.rows.map((c) => (
                        <React.Fragment key={c.campaign_id}>
                          <tr className="inbox-score__row" onClick={() => setOpenCampaign(openCampaign === c.campaign_id ? null : c.campaign_id)}>
                            <td className="inbox-score__camp inbox-score__camp--inner">
                              <div className="inbox-score__name">{c.name}</div>
                              <div className="inbox-score__meta">
                                {c.sending_domain} · {c.scheduled_at ? c.scheduled_at.slice(0, 16).replace('T', ' ') : ''} MT · {c.status}
                                {c.subject ? ` · “${c.subject}”` : ''}
                              </div>
                            </td>
                            {isps.map((isp) => {
                              const v = cellVerdict(c.cells.find((x) => x.isp === isp));
                              return (
                                <td key={isp} className={`inbox-score__cell band-${v.band} source-${v.source}`}>
                                  <div className="inbox-score__verdict">{v.text}</div>
                                </td>
                              );
                            })}
                          </tr>
                          {openCampaign === c.campaign_id && (
                            <tr className="inbox-score__detail">
                              <td colSpan={isps.length + 1}>
                                {(() => {
                                  const { shared, perIsp } = splitReasons(c.cells, isps);
                                  return (
                                    <div className="inbox-score__reasons">
                                      <div className="inbox-score__reason-block inbox-score__reason-block--shared">
                                        <h4>Same for every provider (pre-send scan)</h4>
                                        {shared.length === 0 ? <p className="muted">Nothing shared.</p> : (
                                          <ul>{shared.map((r, i) => <li key={i}>{r}</li>)}</ul>
                                        )}
                                      </div>
                                      {isps.map((isp) => {
                                        const cell = c.cells.find((x) => x.isp === isp);
                                        const own = perIsp[isp] ?? [];
                                        const syms = symbolsList(cell?.seed_symbols).filter((s) => s.score !== 0 || s.symbol.startsWith('JARVIS_')).sort((a, b) => Math.abs(b.score) - Math.abs(a.score)).slice(0, 8);
                                        if (own.length === 0 && !cell?.seed_folder) return null;
                                        return (
                                          <div key={isp} className="inbox-score__reason-block">
                                            <h4>{ISP_LABEL[isp] ?? isp}</h4>
                                            {cell?.seed_folder && (
                                              <p>
                                                Test inbox {cell.seed_email ?? ''}: <strong>{FOLDER_LABEL[cell.seed_folder] ?? cell.seed_folder}</strong>
                                                {cell.seed_score !== null && cell.seed_score !== undefined ? ` · defender ${cell.seed_score.toFixed(1)} of 15 (${BAND_WORDS[defenderBand(cell.seed_score)]})` : ''}
                                                {cell.seed_action ? ` · action: ${cell.seed_action}` : ''}
                                                {cell.seed_observed_at ? ` · checked ${cell.seed_observed_at.slice(0, 16).replace('T', ' ')} UTC` : ''}
                                              </p>
                                            )}
                                            {syms.length > 0 && (
                                              <ul className="inbox-score__symbols">
                                                {syms.map((s) => (
                                                  <li key={s.symbol}>
                                                    <code>{s.symbol}</code> {s.score > 0 ? '+' : ''}{s.score.toFixed(1)}{s.detail ? ` — ${s.detail}` : ''}
                                                  </li>
                                                ))}
                                              </ul>
                                            )}
                                            {own.length > 0 && <ul>{own.map((r, i) => <li key={i}>{r}</li>)}</ul>}
                                          </div>
                                        );
                                      })}
                                    </div>
                                  );
                                })()}
                              </td>
                            </tr>
                          )}
                        </React.Fragment>
                      ))}
                  </React.Fragment>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
};

export default InboxScore;
