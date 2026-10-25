import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faRotateRight, faSpinner } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import './InboxScore.css';

/**
 * Inbox Score.
 *
 * CATALOG (default — operator 2026-10-22): one card per OFFER (associated by name);
 * inside, every creative in Creative Studio and every subject line + preheader it
 * carries, each scored per mailbox provider on its content alone. The sending
 * domain is never part of the score. Rows come from the content gate
 * (agents/inboxscore/gate.py) — the exact bytes our defender received.
 *   GET /api/mailing/inbox-score/catalog  (internal/api/inbox_score_catalog.go)
 *
 * LIVE SENDS: the day's campaigns, one cell per provider — the real seed verdict
 * on the live send when it has landed, the pre-send scan until then.
 *   GET /api/mailing/inbox-score/summary?from&to  (internal/api/inbox_score.go)
 */

// ───────────────────────────── shared

const ISP_LABEL: Record<string, string> = {
  gmail: 'Gmail',
  yahoo_family: 'Yahoo / AOL / AT&T',
  microsoft: 'Outlook / Hotmail',
  apple: 'Apple iCloud',
  cables: 'Cable (Charter, Comcast, Cox)',
};
const ISP_SHORT: Record<string, string> = { gmail: 'Gmail', yahoo_family: 'Yahoo', microsoft: 'Outlook', apple: 'Apple', cables: 'Cable' };

const FOLDER_LABEL: Record<string, string> = {
  inbox: 'Inbox',
  promotions: 'Promotions',
  spam: 'Junk',
  missing: 'Not delivered',
  other: 'Other folder',
};

type Band = 'ready' | 'review' | 'act' | 'junk' | 'none';
const BAND_WORDS: Record<Band, string> = { ready: 'ready', review: 'review', act: 'fix before send', junk: 'junked', none: '' };

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

const BAND_RANK: Record<Band, number> = { none: -1, ready: 0, review: 1, act: 2, junk: 3 };
function worseBand(a: Band, b: Band): Band {
  return BAND_RANK[a] >= BAND_RANK[b] ? a : b;
}

function denverToday(): string {
  const parts = new Intl.DateTimeFormat('en-CA', { timeZone: 'America/Denver', year: 'numeric', month: '2-digit', day: '2-digit' }).formatToParts(new Date());
  const get = (t: string) => parts.find((p) => p.type === t)?.value ?? '';
  return `${get('year')}-${get('month')}-${get('day')}`;
}

type Symbol = { symbol: string; score: number; detail?: string };
type Rule = { rule: string; score: number };

function symbolsList(s: unknown): Symbol[] {
  if (!Array.isArray(s)) return [];
  return s.filter((x): x is Symbol => !!x && typeof x === 'object' && typeof (x as Symbol).symbol === 'string');
}
function rulesList(s: unknown): Rule[] {
  if (!Array.isArray(s)) return [];
  return s.filter((x): x is Rule => !!x && typeof x === 'object' && typeof (x as Rule).rule === 'string');
}
function stringList(r: unknown): string[] {
  if (Array.isArray(r)) return r.filter((x): x is string => typeof x === 'string');
  return [];
}

// ───────────────────────────── catalog types

type CatalogCell = {
  isp: string;
  source: string;
  sa_score: number | null;
  sa_rules: unknown;
  defender_score: number | null;
  defender_symbols: unknown;
  folder: string;
  auth: Record<string, string> | null;
  reasons: unknown;
  verdict: 'PASS' | 'FAIL' | string;
  sending_domain_used: string;
  scored_at: string;
  stale: boolean;
  links: { listed_hosts?: string[]; reasons?: string[]; rules?: string[] } | null;
  link_check: 'ok' | 'degraded' | 'skipped' | 'unknown' | string;
  prev_defender_score: number | null;
  prev_sa_score: number | null;
  prev_scored_at?: string;
};

/** One filter symbol/rule and its reach across the scored subject lines; scope says what to change. */
type Driver = {
  name: string;
  kind: 'defender' | 'presend';
  score: number;
  variants: number;
  of: number;
  scope: 'body' | 'copy' | 'transport' | 'content';
  detail?: string;
};
const SCOPE_WORDS: Record<Driver['scope'], string> = { body: 'BODY', copy: 'COPY', transport: 'TRANSPORT', content: 'CONTENT' };
const SCOPE_HELP: Record<Driver['scope'], string> = {
  body: 'fires on every scored subject line of the creative — it is the HTML',
  copy: 'fires on some subject lines only — it is the subject or preheader',
  transport: 'a sending-path symbol (SPF/DKIM/Received) — not content',
  content: 'one scored line so far — body and copy cannot be told apart yet',
};

type LiveCell = { name: string; day: string; status: string; recipients: number; subject: string; creative?: string; subject_catalogued: boolean };
type Live = { cells: number; matching: number; drifted: number; recipients: number; drift: LiveCell[]; uncatalogued_subjects: string[] };
type Extreme = { creative: string; subject: string; index: number; defender: number; presend: number; headroom: number };

type CatalogVariant = {
  index: number;
  subject: string;
  preheader: string;
  verdict: 'PASS' | 'FAIL' | 'PARTIAL' | 'UNSCORED';
  scored_at?: string;
  cells: Record<string, CatalogCell | undefined>;
};

type CatalogCreative = {
  proof_id: string;
  name: string;
  offer_key: string;
  approval_status: string;
  from_names: string[];
  updated_at: string;
  variants: CatalogVariant[];
  drivers: Driver[];
  scored_lines: number;
  live_cells: number;
  live_recipients: number;
};

type CatalogOffer = {
  offer: string;
  creatives: CatalogCreative[];
  variants: number;
  scored: number;
  passed: number;
  failed: number;
  worst: Extreme | null;
  best: Extreme | null;
  drivers: Driver[];
  link_checks: Record<string, number>;
  live: Live;
};

function fmtN(n: number): string {
  return n.toLocaleString('en-US');
}
function listedHosts(cell: CatalogCell | undefined): string[] {
  return cell?.links?.listed_hosts ?? [];
}
function variantListed(v: CatalogVariant): string[] {
  const out = new Set<string>();
  Object.values(v.cells).forEach((c) => listedHosts(c).forEach((h) => out.add(h)));
  return [...out];
}
function variantDegraded(v: CatalogVariant): string[] {
  return Object.values(v.cells)
    .filter((c): c is CatalogCell => !!c && !c.stale && (c.link_check === 'degraded' || c.link_check === 'skipped'))
    .map((c) => c.isp);
}
/** Lanes that saw the same bytes score the same way; group them so the detail reads once, not five times. */
function laneSignature(c: CatalogCell): string {
  const syms = symbolsList(c.defender_symbols).filter((x) => x.score > 0).map((x) => `${x.symbol}:${x.score}`).sort().join(',');
  const rules = rulesList(c.sa_rules).filter((x) => x.score > 0).map((x) => `${x.rule}:${x.score}`).sort().join(',');
  return [c.verdict, c.folder, c.defender_score, c.sa_score, syms, rules, c.link_check, listedHosts(c).join(','), stringList(c.reasons).join('|')].join('#');
}

type Catalog = { as_of: string; isps: string[]; offers: CatalogOffer[]; how_to_read: string[] };

/** One provider cell for one subject line: the defender verdict leads, the pre-send scan follows. */
export function catalogCellBand(cell: CatalogCell | undefined): Band {
  if (!cell || cell.stale) return 'none';
  let b: Band = 'none';
  if (cell.folder === 'spam' || cell.folder === 'missing') b = 'junk';
  b = worseBand(b, defenderBand(cell.defender_score));
  b = worseBand(b, presendBand(cell.sa_score));
  if (cell.verdict === 'FAIL' && b === 'ready') b = 'review';
  return b;
}

export function variantBand(v: CatalogVariant, isps: string[]): Band {
  return isps.reduce<Band>((acc, isp) => worseBand(acc, catalogCellBand(v.cells[isp])), 'none');
}

export function offerBand(o: CatalogOffer, isps: string[]): Band {
  return o.creatives.reduce<Band>((acc, c) => c.variants.reduce<Band>((a2, v) => worseBand(a2, variantBand(v, isps)), acc), 'none');
}

const VERDICT_WORDS: Record<CatalogVariant['verdict'], string> = { PASS: 'PASS', FAIL: 'FAIL', PARTIAL: 'partly scored', UNSCORED: 'not scored' };

const CatalogView: React.FC = () => {
  const [data, setData] = useState<Catalog | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [openVariant, setOpenVariant] = useState<string | null>(null);
  const [filter, setFilter] = useState('');
  const [onlyScored, setOnlyScored] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await apiFetch('/api/mailing/inbox-score/catalog');
      if (!res.ok) {
        const body = await res.json().catch(() => ({}));
        throw new Error(body?.error || `HTTP ${res.status}`);
      }
      setData((await res.json()) as Catalog);
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not load the catalog');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const isps = useMemo(() => data?.isps ?? ['gmail', 'yahoo_family', 'microsoft', 'apple', 'cables'], [data]);
  const offers = useMemo(() => {
    const q = filter.trim().toLowerCase();
    return (data?.offers ?? []).filter((o) => (!onlyScored || o.scored > 0) && (!q || o.offer.toLowerCase().includes(q) || o.creatives.some((c) => c.name.toLowerCase().includes(q))));
  }, [data, filter, onlyScored]);
  const current = offers.find((o) => o.offer === selected) ?? null;
  const totals = useMemo(() => {
    const all = data?.offers ?? [];
    return {
      offers: all.length,
      creatives: all.reduce((n, o) => n + o.creatives.length, 0),
      variants: all.reduce((n, o) => n + o.variants, 0),
      scored: all.reduce((n, o) => n + o.scored, 0),
      failed: all.reduce((n, o) => n + o.failed, 0),
    };
  }, [data]);

  return (
    <>
      <section className="inbox-score__howto">
        <ul>
          {(data?.how_to_read ?? []).map((line, i) => (
            <li key={i}>{line}</li>
          ))}
        </ul>
        {data && (
          <p className="muted">
            {totals.offers} offers · {totals.creatives} creatives · {totals.variants} subject lines · {totals.scored} scored
            {totals.failed ? ` · ${totals.failed} failing` : ''}
          </p>
        )}
      </section>

      <div className="isc-toolbar">
        <input id="inbox-score-filter" type="search" placeholder="Find an offer or creative" value={filter} onChange={(e) => setFilter(e.target.value)} />
        <label className="inbox-score__toggle">
          <input id="inbox-score-only-scored" type="checkbox" checked={onlyScored} onChange={(e) => setOnlyScored(e.target.checked)} />
          scored only
        </label>
        <button id="inbox-score-catalog-refresh" type="button" onClick={() => void load()} disabled={loading}>
          <FontAwesomeIcon icon={loading ? faSpinner : faRotateRight} spin={loading} /> Refresh
        </button>
      </div>

      {error && <div className="inbox-score__error">{error}</div>}
      {!loading && data && offers.length === 0 && <div className="inbox-score__empty">No offers match.</div>}

      <div className="isc-cards">
        {offers.map((o) => {
          const band = offerBand(o, isps);
          const unscored = o.variants - o.scored;
          return (
            <button
              key={o.offer}
              type="button"
              className={`isc-card band-${band}${selected === o.offer ? ' isc-card--selected' : ''}`}
              onClick={() => {
                setSelected(selected === o.offer ? null : o.offer);
                setOpenVariant(null);
              }}
            >
              <div className="isc-card__name">{o.offer}</div>
              <div className="isc-card__meta">
                {o.creatives.length} creative{o.creatives.length === 1 ? '' : 's'} · {o.variants} subject line{o.variants === 1 ? '' : 's'}
              </div>
              <div className="isc-card__chips">
                {o.passed > 0 && <span className="isc-chip isc-chip--pass">{o.passed} pass</span>}
                {o.failed > 0 && <span className="isc-chip isc-chip--fail">{o.failed} fail</span>}
                {unscored > 0 && <span className="isc-chip isc-chip--none">{unscored} not scored</span>}
                {o.live.drifted > 0 && <span className="isc-chip isc-chip--fail">{o.live.drifted} board cells drifted</span>}
                {(o.link_checks?.degraded ?? 0) + (o.link_checks?.skipped ?? 0) > 0 && <span className="isc-chip isc-chip--warn">link check degraded</span>}
              </div>
              {o.worst && (
                <div className="isc-card__worst">
                  worst {o.worst.defender.toFixed(1)} of 15 · {o.worst.headroom >= 0 ? `${o.worst.headroom.toFixed(1)} below review` : `${(-o.worst.headroom).toFixed(1)} over review`}
                  {o.drivers[0] ? ` · top: ${o.drivers[0].name} +${o.drivers[0].score.toFixed(1)}` : ''}
                </div>
              )}
              {o.live.cells > 0 && (
                <div className="isc-card__live">
                  board: {o.live.cells} cells · {fmtN(o.live.recipients)} recipients · {o.live.matching} match the catalogue
                </div>
              )}
            </button>
          );
        })}
      </div>

      {current && (
        <section className="isc-detail">
          <header className="isc-detail__head">
            <h3>{current.offer}</h3>
            <p className="muted">
              Each row is one subject line + preheader on one creative. Scores are the content&apos;s alone — the domain a run went out from is shown only in the detail.
            </p>
          </header>
          <div className="isc-insight">
            <section className="isc-panel">
              <h4>What moves the score</h4>
              {current.drivers.length === 0 ? (
                <p className="muted">Nothing scored yet.</p>
              ) : (
                <table className="isc-drivers">
                  <tbody>
                    {current.drivers.slice(0, 8).map((d) => (
                      <tr key={`${d.kind}:${d.name}`}>
                        <td>
                          <code>{d.name}</code>
                          <span className="muted"> {d.kind === 'presend' ? 'pre-send' : 'defender'}</span>
                        </td>
                        <td className="isc-num">+{d.score.toFixed(1)}</td>
                        <td className="isc-num">
                          {d.variants} of {d.of} lines
                        </td>
                        <td>
                          <span className={`isc-tag isc-tag--${d.scope}`} title={SCOPE_HELP[d.scope]}>
                            {SCOPE_WORDS[d.scope]}
                          </span>
                        </td>
                        <td className="muted isc-detailcol">{d.detail || ''}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
              <p className="muted isc-legend">BODY = fix the HTML · COPY = fix those subject lines · TRANSPORT = the route, not content.</p>
            </section>
            <section className="isc-panel">
              <h4>Range</h4>
              {current.worst && current.best ? (
                <ul className="isc-range">
                  <li>
                    <strong>Worst</strong> {current.worst.defender.toFixed(1)} of 15 · pre-send {current.worst.presend.toFixed(1)} ·{' '}
                    {current.worst.headroom >= 0 ? `${current.worst.headroom.toFixed(1)} below the review line` : `${(-current.worst.headroom).toFixed(1)} over the review line`}
                    <div className="muted">
                      {current.worst.creative} — “{current.worst.subject}”
                    </div>
                  </li>
                  <li>
                    <strong>Best</strong> {current.best.defender.toFixed(1)} of 15 · pre-send {current.best.presend.toFixed(1)}
                    <div className="muted">
                      {current.best.creative} — “{current.best.subject}”
                    </div>
                  </li>
                  {Object.keys(current.link_checks ?? {}).length > 0 && (
                    <li>
                      <strong>Link check</strong>{' '}
                      {Object.entries(current.link_checks)
                        .map(([k, n]) => `${n} lanes ${k}`)
                        .join(' · ')}
                      {(current.link_checks.degraded ?? 0) + (current.link_checks.skipped ?? 0) > 0 && (
                        <div className="muted">A degraded lane could not prove “no listed link host” — a block list refused or did not answer. Re-run the link pass: INBOXSCORE_DNS=127.0.0.1:7853 python3 -m agents.inboxscore.gate --relink .scratch/inboxscore/gate/&lt;artifact&gt;.json</div>
                      )}
                    </li>
                  )}
                </ul>
              ) : (
                <p className="muted">Nothing scored yet.</p>
              )}
            </section>
            <section className={`isc-panel${current.live.drifted > 0 ? ' isc-panel--alert' : ''}`}>
              <h4>Board parity (yesterday → +8 days)</h4>
              {current.live.cells === 0 ? (
                <p className="muted">No board cell mails this offer in the window.</p>
              ) : (
                <>
                  <p>
                    {current.live.cells} cells · {fmtN(current.live.recipients)} recipients · <strong>{current.live.matching}</strong> carry catalogued bytes
                    {current.live.drifted > 0 ? (
                      <>
                        {' · '}
                        <strong className="isc-bad">{current.live.drifted} drifted</strong> — bytes no catalogued creative has; the scores above do not describe them
                      </>
                    ) : (
                      ' · every cell matches a catalogued creative'
                    )}
                  </p>
                  {current.live.drift.length > 0 && (
                    <table className="isc-drift">
                      <tbody>
                        {current.live.drift.slice(0, 40).map((cell) => (
                          <tr key={cell.name}>
                            <td>{cell.day}</td>
                            <td>{cell.name}</td>
                            <td className="isc-num">{fmtN(cell.recipients)}</td>
                            <td className="muted">{cell.status}</td>
                            <td className="muted">{cell.subject_catalogued ? '' : 'subject not in the catalogue'}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  )}
                  {current.live.uncatalogued_subjects.length > 0 && (
                    <p className="muted">
                      Mailing with subject lines the catalogue never scored: {current.live.uncatalogued_subjects.slice(0, 5).map((x) => `“${x}”`).join(', ')}
                      {current.live.uncatalogued_subjects.length > 5 ? ` +${current.live.uncatalogued_subjects.length - 5}` : ''}
                    </p>
                  )}
                </>
              )}
            </section>
          </div>
          <div className="inbox-score__tablewrap">
            <table className="inbox-score__table isc-table">
              <thead>
                <tr>
                  <th>Creative / subject line</th>
                  {isps.map((isp) => (
                    <th key={isp}>{ISP_LABEL[isp] ?? isp}</th>
                  ))}
                  <th>Verdict</th>
                </tr>
              </thead>
              <tbody>
                {current.creatives.map((c) => (
                  <React.Fragment key={c.proof_id}>
                    <tr className="isc-creative">
                      <td colSpan={isps.length + 2}>
                        <span className="isc-creative__name">{c.name}</span>
                        <span className="isc-creative__meta">
                          {c.approval_status}
                          {c.from_names.length ? ` · from “${c.from_names[0]}”${c.from_names.length > 1 ? ` +${c.from_names.length - 1}` : ''}` : ''}
                          {' · '}
                          <code>{c.offer_key}</code>
                          {` · ${c.scored_lines} of ${c.variants.length} lines scored`}
                          {c.live_cells > 0 ? ` · on the board: ${c.live_cells} cells, ${fmtN(c.live_recipients)} recipients` : ' · not on the board'}
                          {c.drivers.length > 0 && (
                            <span className="isc-creative__drivers">
                              {' · '}
                              {c.drivers.slice(0, 3).map((d) => (
                                <span key={`${d.kind}:${d.name}`} className={`isc-tag isc-tag--${d.scope}`} title={`${d.name} +${d.score.toFixed(1)} on ${d.variants} of ${d.of} lines — ${SCOPE_HELP[d.scope]}`}>
                                  {d.name} +{d.score.toFixed(1)}
                                </span>
                              ))}
                            </span>
                          )}
                        </span>
                      </td>
                    </tr>
                    {c.variants.map((v) => {
                      const key = `${c.proof_id}:${v.index}`;
                      const vb = variantBand(v, isps);
                      const isOpen = openVariant === key;
                      return (
                        <React.Fragment key={key}>
                          <tr className={`inbox-score__row isc-variant verdict-${v.verdict.toLowerCase()}`} onClick={() => setOpenVariant(isOpen ? null : key)}>
                            <td className="inbox-score__camp inbox-score__camp--inner">
                              <div className="inbox-score__name">
                                {isOpen ? '▾' : '▸'} {v.index < 0 ? <em>no subject on file (proof name used)</em> : v.subject || <em>empty subject</em>}
                              </div>
                              <div className="inbox-score__meta">
                                {v.preheader ? `preheader: ${v.preheader}` : 'no preheader'}
                                {variantListed(v).map((h) => (
                                  <span key={h} className="isc-chip isc-chip--fail">listed link: {h}</span>
                                ))}
                                {variantDegraded(v).length > 0 && <span className="isc-chip isc-chip--warn">link check degraded on {variantDegraded(v).map((x) => ISP_SHORT[x] ?? x).join(', ')}</span>}
                              </div>
                            </td>
                            {isps.map((isp) => {
                              const cell = v.cells[isp];
                              const b = catalogCellBand(cell);
                              let text = 'not scored';
                              if (cell && cell.stale) text = 'stale — copy changed';
                              else if (cell) {
                                const folder = FOLDER_LABEL[cell.folder] ?? cell.folder;
                                const d = cell.defender_score !== null && cell.defender_score !== undefined ? `${cell.defender_score.toFixed(1)} of 15` : '';
                                const sa = cell.sa_score !== null && cell.sa_score !== undefined ? ` · pre-send ${cell.sa_score.toFixed(1)}` : '';
                                text = `${folder}${d ? ` · ${d}` : ''}${sa}`;
                              }
                              return (
                                <td key={isp} className={`inbox-score__cell band-${b}`}>
                                  <div className="inbox-score__verdict">{text}</div>
                                </td>
                              );
                            })}
                            <td className={`isc-verdict verdict-${v.verdict.toLowerCase()} band-${vb}`}>{VERDICT_WORDS[v.verdict]}</td>
                          </tr>
                          {isOpen && (
                            <tr className="inbox-score__detail">
                              <td colSpan={isps.length + 2}>
                                <div className="inbox-score__reasons">
                                  {(() => {
                                    const missing = isps.filter((isp) => !v.cells[isp]);
                                    const groups = new Map<string, { lanes: string[]; cell: CatalogCell }>();
                                    isps.forEach((isp) => {
                                      const cell = v.cells[isp];
                                      if (!cell) return;
                                      const sig = laneSignature(cell);
                                      const g = groups.get(sig);
                                      if (g) g.lanes.push(isp);
                                      else groups.set(sig, { lanes: [isp], cell });
                                    });
                                    const scopeOf = (name: string): Driver['scope'] | null => c.drivers.find((d) => d.name === name)?.scope ?? null;
                                    const blocks = [...groups.values()].map(({ lanes, cell }) => {
                                      const syms = symbolsList(cell.defender_symbols).filter((x) => x.score > 0).sort((a, b2) => b2.score - a.score).slice(0, 8);
                                      const rules = rulesList(cell.sa_rules).filter((x) => x.score > 0).sort((a, b2) => b2.score - a.score).slice(0, 8);
                                      const why = stringList(cell.reasons);
                                      const au = cell.auth ?? {};
                                      const linkReasons = cell.links?.reasons ?? [];
                                      const listed = listedHosts(cell);
                                      return (
                                        <div key={lanes.join('+')} className={`inbox-score__reason-block${lanes.length > 1 ? ' inbox-score__reason-block--shared' : ''}`}>
                                          <h4>
                                            {lanes.map((x) => ISP_SHORT[x] ?? x).join(' · ')} — {cell.verdict}
                                            {lanes.length > 1 ? ` (${lanes.length} providers saw the same bytes and scored them the same)` : ''}
                                            {cell.stale ? ' (stale: the copy changed after this score)' : ''}
                                          </h4>
                                          <p>
                                            Landed in <strong>{FOLDER_LABEL[cell.folder] ?? cell.folder}</strong>
                                            {cell.defender_score !== null ? ` · defender ${cell.defender_score.toFixed(1)} of 15 (${BAND_WORDS[defenderBand(cell.defender_score)]})` : ''}
                                            {cell.sa_score !== null ? ` · pre-send ${cell.sa_score.toFixed(1)} (${BAND_WORDS[presendBand(cell.sa_score)]})` : ''}
                                            {` · spf ${au.spf ?? '-'} · dkim ${au.dkim ?? '-'} · dmarc ${au.dmarc ?? '-'}`}
                                          </p>
                                          {cell.prev_defender_score !== null && cell.prev_defender_score !== undefined && (
                                            <p className="muted">
                                              Previous score {cell.prev_defender_score.toFixed(1)} of 15
                                              {cell.prev_sa_score !== null && cell.prev_sa_score !== undefined ? ` · pre-send ${cell.prev_sa_score.toFixed(1)}` : ''}
                                              {cell.prev_scored_at ? ` on ${cell.prev_scored_at.slice(0, 10)}` : ''}
                                              {cell.defender_score !== null
                                                ? ` → ${cell.defender_score - cell.prev_defender_score <= 0 ? 'better' : 'worse'} by ${Math.abs(cell.defender_score - cell.prev_defender_score).toFixed(1)}`
                                                : ''}
                                            </p>
                                          )}
                                          {why.length > 0 && <ul>{why.map((r, i) => <li key={i}>{r}</li>)}</ul>}
                                          {(syms.length > 0 || rules.length > 0) && (
                                            <ul className="inbox-score__symbols">
                                              {syms.map((sy) => {
                                                const sc = scopeOf(sy.symbol);
                                                return (
                                                  <li key={`d-${sy.symbol}`}>
                                                    defender <code>{sy.symbol}</code> +{sy.score.toFixed(1)}
                                                    {sc ? <span className={`isc-tag isc-tag--${sc}`} title={SCOPE_HELP[sc]}>{SCOPE_WORDS[sc]}</span> : null}
                                                    {sy.detail ? ` — ${sy.detail}` : ''}
                                                  </li>
                                                );
                                              })}
                                              {rules.map((r) => {
                                                const sc = scopeOf(r.rule);
                                                return (
                                                  <li key={`s-${r.rule}`}>
                                                    pre-send <code>{r.rule}</code> +{r.score.toFixed(1)}
                                                    {sc ? <span className={`isc-tag isc-tag--${sc}`} title={SCOPE_HELP[sc]}>{SCOPE_WORDS[sc]}</span> : null}
                                                  </li>
                                                );
                                              })}
                                            </ul>
                                          )}
                                          <p className={cell.link_check === 'ok' ? 'muted' : 'isc-bad'}>
                                            Link check {cell.link_check}
                                            {listed.length > 0 ? ` · listed: ${listed.join(', ')}` : cell.link_check === 'ok' ? ' · no listed link host' : ' · “no listed link host” unproven'}
                                          </p>
                                          {linkReasons.length > 0 && <ul className="inbox-score__symbols">{linkReasons.slice(0, 4).map((r, i) => <li key={i}>{r}</li>)}</ul>}
                                          <p className="muted">
                                            Sent {cell.scored_at.slice(0, 16).replace('T', ' ')} UTC from {cell.sending_domain_used || 'unknown domain'} (not part of the score).
                                          </p>
                                        </div>
                                      );
                                    });
                                    if (missing.length > 0) {
                                      blocks.push(
                                        <div key="missing" className="inbox-score__reason-block">
                                          <h4>{missing.map((x) => ISP_SHORT[x] ?? x).join(' · ')}</h4>
                                          <p className="muted">Not scored yet.</p>
                                        </div>,
                                      );
                                    }
                                    return blocks;
                                  })()}
                                </div>
                                <p className="muted isc-run">
                                  Re-score: <code>INBOXSCORE_DNS=127.0.0.1:7853 python3 -m agents.inboxscore.gate --proof &quot;{c.name}&quot; {v.index >= 0 ? `--variant ${v.index}` : ''}</code>
                                  {v.index < 0 ? ' — this creative has no subject on file; the gate mailed it under the creative name, which is not what a board cell would carry.' : ''}
                                </p>
                              </td>
                            </tr>
                          )}
                        </React.Fragment>
                      );
                    })}
                  </React.Fragment>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}
    </>
  );
};

// ───────────────────────────── live sends (the day's campaigns)

type ProfileBrief = { version: number; status: string; auc: number | null; finding: string };

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
  const lists = isps.map((isp) => stringList(cells.find((c) => c.isp === isp)?.reasons));
  const shared = lists.length ? lists[0].filter((r) => lists.every((l) => l.includes(r))) : [];
  const perIsp: Record<string, string[]> = {};
  isps.forEach((isp, i) => {
    perIsp[isp] = (lists[i] ?? []).filter((r) => !shared.includes(r));
  });
  return { shared, perIsp };
}

const LiveSendsView: React.FC = () => {
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
    <>
      <div className="inbox-score__controls isc-toolbar">
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
    </>
  );
};

// ───────────────────────────── screen

const InboxScore: React.FC = () => {
  const [view, setView] = useState<'catalog' | 'live'>('catalog');
  return (
    <div className="inbox-score">
      <header className="inbox-score__head">
        <div>
          <h2>Inbox Score</h2>
          <p className="inbox-score__sub">
            {view === 'catalog'
              ? 'Every offer, every creative, every subject line + preheader — scored per mailbox provider on the content alone.'
              : "The day's live sends: the real test-inbox verdict when the send has landed, the pre-send scan until then."}
          </p>
        </div>
        <div className="isc-views" role="tablist">
          <button id="inbox-score-view-catalog" type="button" role="tab" aria-selected={view === 'catalog'} className={view === 'catalog' ? 'active' : ''} onClick={() => setView('catalog')}>
            Catalog
          </button>
          <button id="inbox-score-view-live" type="button" role="tab" aria-selected={view === 'live'} className={view === 'live' ? 'active' : ''} onClick={() => setView('live')}>
            Live sends
          </button>
        </div>
      </header>
      {view === 'catalog' ? <CatalogView /> : <LiveSendsView />}
    </div>
  );
};

export default InboxScore;
