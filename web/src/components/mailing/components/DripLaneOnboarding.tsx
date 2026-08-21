// Drip Lane Onboarding — take an INGESTED lead file and turn it into a lane
// that actually mails (operator 2026-08-21).
//
// ── THE LAW THIS SCREEN OBEYS ────────────────────────────────────────────────
// cold-data-drip-pipeline-only-LAW (operator 2026-08-17, after the INTRO
// sidecar burn: 88,459 cold sends / 16,018 bounces / 18.1%):
//
//   cold recipients reach production ONLY as
//   dataset -> partner ingest (EO validation) -> partner_clean_queue
//           -> drip orchestrator under its per-wave / daily / allow-list caps
//
// This screen configures ORCHESTRATOR TABLES ONLY. It has no send button, no
// campaign creation, no segment or list write, and it never puts a record into
// partner_clean_queue — that is supplier ingest's job, and the DATA gate below
// simply reports whether ingest has already run.
//
// ── The seven gates ─────────────────────────────────────────────────────────
//   1 DATA      records exist in partner_clean_queue for the vertical
//   2 ROSTER    an ACTIVE (vertical, brand) binding
//   3 PROFILE   the brand resolves to an ACTIVE sending profile — rendered with
//               TRANSPORT and TRACKING DOMAIN so a misroute is visible at
//               SELECTION time, not after the first wave
//   4 WELCOME   an active touch-1 creative
//   5 FOLLOWUP  active follow-up rows in touches 2..MaxTouch (a row at touch 1
//               is INERT — the follow-up pass never asks for it)
//   6 OFFER     the offer carries a SERVING row in all three of
//               mailing_offer_creatives / _subject_lines / _from_names, or the
//               send-time resolve hard-fails
//   7 CAPS      per-ISP budget cells. An EMPTY ledger is UNCONSTRAINED, not
//               zero — for a cold lane that is a warning, not a pass.
//
// Backend contract: internal/api/drip_lane_onboarding.go.
// Networking: shared apiFetch only (org header + credentials). Styling: the
// shared theme/ui kit (PORTAL_DESIGN_SYSTEM §2, §5) — no one-off colors.

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faRoute, faCircleCheck, faTriangleExclamation, faCircleXmark, faQuestion,
  faSpinner, faShieldHalved, faTowerBroadcast, faGaugeHigh, faLayerGroup,
} from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import {
  colors, alpha, pageStyle, panelTitleStyle, btnStyle, cardGrid,
  tableStyle, thStyle, tdStyle, numTd, numTh,
} from '../shared/theme';
import { Panel, SectionHeader, Pill, SectionError, EmptyState } from '../shared/ui';

export const PAGE_VERSION = '1.0.0 (2026-08-21) — seven-gate lane onboarding';

// ── types (mirror internal/api/drip_lane_onboarding.go) ─────────────────────

type GateStatus = 'pass' | 'fail' | 'warn' | 'unknown';

interface Gate { name: string; status: GateStatus; detail: string; fatal: boolean }
interface LaneProfile {
  brand: string; sending_domain: string; transport: string;
  tracking_domain: string; profile_id: string; found: boolean;
}
interface OfferReadiness {
  offer_id: string; name: string; status: string;
  has_creative: boolean; has_subjects: boolean; has_from_names: boolean;
  complete: boolean; missing: string[]; used_by_touches?: number[];
}
interface Touch {
  touch: number; source: string; scope: string;
  creative_filename: string; offer_id?: string; active: boolean;
}
interface BudgetCell {
  isp: string; daily_budget: number; pending_budget?: number;
  pending_effective_day?: string; hold: boolean; lock_version: number;
}
interface VerifyResult {
  vertical: string; brand?: string; brands: string[];
  verdict: 'PASS' | 'WARN' | 'FAIL';
  gates: Gate[]; profiles: LaneProfile[]; touches: Touch[];
  offers: OfferReadiness[]; budgets: BudgetCell[];
  max_touch: number; organization_id: string;
  write_enabled: boolean; write_flag_env: string;
  roster_write_enabled: boolean; roster_write_flag_env: string;
}
interface VerticalOption { vertical: string; rostered: boolean; has_dataset: boolean }
interface Options {
  verticals: VerticalOption[];
  brands: LaneProfile[];
  isps: string[];
  offers: OfferReadiness[];
  max_touch: number;
  write_enabled: boolean; write_flag_env: string;
  roster_write_enabled: boolean; roster_write_flag_env: string;
  budget_effective_note: string;
  cold_data_law: string;
  budget_ceiling_env: string;
}
interface BudgetOutcome {
  isp: string; action: string; staged_budget?: number;
  effective_day?: string; message?: string;
}
interface OnboardResult {
  ok: boolean; vertical: string; brand: string;
  sending_domain: string; transport: string; tracking_domain: string;
  weight_clamped: boolean; followup_touches: number[];
  budgets: BudgetOutcome[]; budget_note: string; verify: VerifyResult;
}

// ── presentation helpers ────────────────────────────────────────────────────

const gateColor = (s: GateStatus) =>
  s === 'pass' ? colors.success
    : s === 'fail' ? colors.danger
      : s === 'warn' ? colors.warning
        : colors.textMuted;

const gateIcon = (s: GateStatus) =>
  s === 'pass' ? faCircleCheck
    : s === 'fail' ? faCircleXmark
      : s === 'warn' ? faTriangleExclamation
        : faQuestion;

const verdictColor = (v: string) =>
  v === 'PASS' ? colors.success : v === 'WARN' ? colors.warning : colors.danger;

// Transport is a routing fact, not a status — but a lane pointed at the wrong
// transport ships to the wrong estate, so it gets its own color per family.
const transportColor = (t: string) =>
  t === 'SES' ? colors.indigo400 : t === 'KUMO' ? '#22d3ee' : colors.indigo200;

const num = (n: number) => (n ?? 0).toLocaleString();

const inputStyle: React.CSSProperties = {
  background: 'rgba(2,6,23,0.6)',
  border: `1px solid ${colors.panelBorder}`,
  borderRadius: 8,
  color: colors.text,
  padding: '7px 10px',
  fontSize: 13,
  width: '100%',
};

const labelStyle: React.CSSProperties = {
  fontSize: 10,
  color: colors.textMuted,
  textTransform: 'uppercase',
  letterSpacing: 0.5,
  marginBottom: 4,
  display: 'block',
};

const noteStyle: React.CSSProperties = {
  fontSize: 11,
  color: colors.textMuted,
  marginTop: 4,
  lineHeight: 1.5,
};

// ── component ───────────────────────────────────────────────────────────────

export const DripLaneOnboarding: React.FC = () => {
  const [opts, setOpts] = useState<Options | null>(null);
  const [optsErr, setOptsErr] = useState<string | null>(null);

  const [vertical, setVertical] = useState('');
  const [brand, setBrand] = useState('');
  const [offerId, setOfferId] = useState('');
  const [touches, setTouches] = useState(4);
  const [weight, setWeight] = useState(1);
  const [budgets, setBudgets] = useState<Record<string, string>>({});

  const [verify, setVerify] = useState<VerifyResult | null>(null);
  const [verifyErr, setVerifyErr] = useState<string | null>(null);
  const [verifying, setVerifying] = useState(false);
  const [verifiedAt, setVerifiedAt] = useState<string | null>(null);

  const [committing, setCommitting] = useState(false);
  const [commitErr, setCommitErr] = useState<string | null>(null);
  const [commitRes, setCommitRes] = useState<OnboardResult | null>(null);

  // ── options ───────────────────────────────────────────────────────────────
  const loadOptions = useCallback(async (signal?: AbortSignal) => {
    setOptsErr(null);
    try {
      const r = await apiFetch('/api/mailing/drip-lane/options', { signal });
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      setOpts(await r.json());
    } catch (e) {
      if ((e as Error).name === 'AbortError') return;
      setOptsErr(e instanceof Error ? e.message : 'options load failed');
    }
  }, []);

  useEffect(() => {
    const ac = new AbortController();
    void loadOptions(ac.signal);
    return () => ac.abort();
  }, [loadOptions]);

  // ── verify (re-runs whenever the lane selection changes) ──────────────────
  const runVerify = useCallback(async (v: string, b: string, signal?: AbortSignal) => {
    if (!v) { setVerify(null); setVerifyErr(null); return; }
    setVerifying(true); setVerifyErr(null);
    const t0 = performance.now();
    try {
      const qs = new URLSearchParams({ vertical: v });
      if (b) qs.set('brand', b);
      const r = await apiFetch(`/api/mailing/drip-lane/verify?${qs.toString()}`, { signal });
      const body = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(body.error || `HTTP ${r.status}`);
      setVerify(body as VerifyResult);
      setVerifiedAt(`${new Date().toLocaleTimeString()} · ${Math.round(performance.now() - t0)}ms`);
    } catch (e) {
      if ((e as Error).name === 'AbortError') return;
      setVerify(null);
      setVerifyErr(e instanceof Error ? e.message : 'verify failed');
    } finally {
      setVerifying(false);
    }
  }, []);

  useEffect(() => {
    const ac = new AbortController();
    void runVerify(vertical, brand, ac.signal);
    return () => ac.abort();
  }, [vertical, brand, runVerify]);

  // ── derived ───────────────────────────────────────────────────────────────
  const selectedBrandProfile = useMemo(
    () => opts?.brands.find(b => b.brand === brand) ?? null, [opts, brand]);
  const selectedOffer = useMemo(
    () => opts?.offers.find(o => o.offer_id === offerId) ?? null, [opts, offerId]);

  const writeEnabled = opts?.write_enabled ?? false;
  const rosterWriteEnabled = opts?.roster_write_enabled ?? false;

  // Commit is blocked on exactly the things the SERVER will refuse, so the
  // operator never gets a surprise 4xx. The server re-asserts all of it — this
  // is disclosure, not the gate.
  const blockers = useMemo(() => {
    const out: string[] = [];
    if (!vertical) out.push('pick a vertical');
    if (!brand) out.push('pick a sending domain');
    if (!offerId) out.push('pick an offer');
    if (selectedBrandProfile && !selectedBrandProfile.found) {
      out.push(`${brand} has no active sending profile — onboard the domain first`);
    }
    if (selectedOffer && !selectedOffer.complete) {
      out.push(`offer is missing ${selectedOffer.missing.join(' + ')} — wire it in Creative Studio first`);
    }
    if (!writeEnabled) out.push(`writes disabled (${opts?.write_flag_env ?? 'DRIP_LANE_ONBOARD_ENABLED'} is not set to 1)`);
    if (!rosterWriteEnabled) out.push(`roster writes disabled (${opts?.roster_write_flag_env ?? 'PROPERTY_LEDGER_ROSTER_WRITE_ENABLED'} is not set to 1)`);
    return out;
  }, [vertical, brand, offerId, selectedBrandProfile, selectedOffer, writeEnabled, rosterWriteEnabled, opts]);

  // ── commit ────────────────────────────────────────────────────────────────
  const commit = useCallback(async () => {
    setCommitting(true); setCommitErr(null); setCommitRes(null);
    try {
      const body = {
        vertical, brand, offer_id: offerId,
        touches, weight, sort_order: 0,
        budgets: Object.entries(budgets)
          .map(([isp, v]) => ({ isp, daily_budget: parseInt(v, 10) }))
          .filter(b => Number.isFinite(b.daily_budget) && b.daily_budget >= 0),
        confirm: true,
      };
      const r = await apiFetch('/api/mailing/drip-lane/onboard', {
        method: 'POST', body: JSON.stringify(body),
      });
      const j = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(j.error || `HTTP ${r.status}`);
      setCommitRes(j as OnboardResult);
      if (j.verify) setVerify(j.verify as VerifyResult);
    } catch (e) {
      setCommitErr(e instanceof Error ? e.message : 'commit failed');
    } finally {
      setCommitting(false);
    }
  }, [vertical, brand, offerId, touches, weight, budgets]);

  // ── render ────────────────────────────────────────────────────────────────
  return (
    <div style={pageStyle}>
      <div style={{ marginBottom: 16 }}>
        <h2 style={{ ...panelTitleStyle, fontSize: 16, marginBottom: 6 }}>
          <FontAwesomeIcon icon={faRoute} style={{ color: colors.indigo400 }} />
          Drip Lane Onboarding
        </h2>
        <div style={{ fontSize: 12, color: colors.textMuted, maxWidth: 900, lineHeight: 1.6 }}>
          Turn an ingested lead file into a lane that mails. This screen configures the drip
          orchestrator&rsquo;s own tables — roster, welcome creative, follow-up ladder and per-ISP
          budget cells. It never creates a campaign, never writes <code>partner_clean_queue</code>,
          and never touches a segment or list.
        </div>
      </div>

      {/* The law, stated on the screen that could most easily violate it. */}
      <Panel accent={colors.indigo500} style={{ marginBottom: 14 }}>
        <SectionHeader title="Cold-data law" icon={faShieldHalved} />
        <div style={{ fontSize: 12, color: colors.text, lineHeight: 1.7 }}>
          Cold recipients reach production <strong>only</strong> as{' '}
          <span style={{ color: colors.indigo200 }}>
            dataset → partner ingest (EO validation) → partner_clean_queue → drip orchestrator
          </span>{' '}
          under its per-wave / daily / allow-list caps. Never a campaign-sidecar sender.
          <div style={noteStyle}>
            If the DATA gate below is red, the answer is to <em>ingest the file</em>, not to
            mail it another way. There is no send button on this screen by design.
          </div>
        </div>
      </Panel>

      {optsErr && (
        <div style={{ marginBottom: 14 }}>
          <SectionError label="Options" error={optsErr} onRetry={() => void loadOptions()} />
        </div>
      )}

      {!writeEnabled && opts && (
        <Panel accent={colors.warning} style={{ marginBottom: 14 }}>
          <div style={{ fontSize: 12, color: colors.warningText }}>
            <FontAwesomeIcon icon={faTriangleExclamation} /> READ-ONLY — the onboard endpoint is
            inert. Set the server env <code>{opts.write_flag_env}=1</code> to enable it
            (unsetting it is the one-move rollback).
            {!rosterWriteEnabled && (
              <> The roster write additionally requires <code>{opts.roster_write_flag_env}=1</code>;
                this endpoint will not bypass that gate.</>
            )}
          </div>
        </Panel>
      )}

      {/* ── Step 1 · pick the lane ───────────────────────────────────────── */}
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader title="1 · Lane" icon={faLayerGroup} />
        <div style={cardGrid(240)}>
          <div>
            <label style={labelStyle}>Vertical (the ingested feed)</label>
            <select style={inputStyle} value={vertical} onChange={e => setVertical(e.target.value)}>
              <option value="">— select —</option>
              {(opts?.verticals ?? []).map(v => (
                <option key={v.vertical} value={v.vertical}>
                  {v.vertical}{v.has_dataset ? '' : ' (no active dataset)'}
                </option>
              ))}
            </select>
            <div style={noteStyle}>
              Only verticals the write path accepts. A vertical with no dataset and no roster row
              would be a phantom lane — the server refuses it.
            </div>
          </div>

          <div>
            <label style={labelStyle}>Sending domain (brand)</label>
            <select style={inputStyle} value={brand} onChange={e => setBrand(e.target.value)}>
              <option value="">— select —</option>
              {(opts?.brands ?? []).map(b => (
                <option key={b.brand} value={b.brand}>
                  {b.brand} · {b.sending_domain || 'NO DOMAIN'}
                  {b.found ? ` [${b.transport}]` : ' — NO ACTIVE PROFILE'}
                </option>
              ))}
            </select>
            {selectedBrandProfile && (
              <div style={{ marginTop: 8, display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'center' }}>
                {selectedBrandProfile.found ? (
                  <>
                    <Pill color={transportColor(selectedBrandProfile.transport)}>
                      <FontAwesomeIcon icon={faTowerBroadcast} /> {selectedBrandProfile.transport}
                    </Pill>
                    <span style={{ fontSize: 11, color: colors.textMuted }}>
                      track={selectedBrandProfile.tracking_domain || '(global fallback)'}
                    </span>
                  </>
                ) : (
                  <Pill color={colors.danger}>no active sending profile</Pill>
                )}
              </div>
            )}
            <div style={noteStyle}>
              Transport and tracking domain are shown here so a misroute is visible at selection.
              A lane pointed at a tracking host that does not serve TLS ships dead links.
            </div>
          </div>

          <div>
            <label style={labelStyle}>Offer</label>
            <select style={inputStyle} value={offerId} onChange={e => setOfferId(e.target.value)}>
              <option value="">— select —</option>
              {(opts?.offers ?? []).map(o => (
                <option key={o.offer_id} value={o.offer_id}>
                  {o.name || o.offer_id}{o.complete ? '' : ` — MISSING ${o.missing.join('+')}`}
                </option>
              ))}
            </select>
            {selectedOffer && (
              <div style={{ marginTop: 8, display: 'flex', gap: 6, flexWrap: 'wrap' }}>
                <Pill color={selectedOffer.has_creative ? colors.success : colors.danger}>creatives</Pill>
                <Pill color={selectedOffer.has_subjects ? colors.success : colors.danger}>subjects</Pill>
                <Pill color={selectedOffer.has_from_names ? colors.success : colors.danger}>from-names</Pill>
              </div>
            )}
            <div style={noteStyle}>
              All three pools must hold a serving (non-archived, non-empty) row, or the send-time
              resolve hard-fails. Wire a missing pool in Creative Studio.
            </div>
          </div>

          <div>
            <label style={labelStyle}>Ladder length (incl. welcome)</label>
            <input
              style={inputStyle} type="number" min={1} max={opts?.max_touch ?? 5}
              value={touches}
              onChange={e => setTouches(Math.max(1, Math.min(opts?.max_touch ?? 5, parseInt(e.target.value, 10) || 1)))}
            />
            <div style={noteStyle}>
              Touch 1 is the welcome. Follow-up rows are written for touches 2…{touches}. The
              orchestrator only ever resolves follow-up touches 2…{opts?.max_touch ?? 5}, so a row
              outside that band is dead configuration.
            </div>
          </div>

          <div>
            <label style={labelStyle}>Roster weight (1–20)</label>
            <input
              style={inputStyle} type="number" min={1} max={20} value={weight}
              onChange={e => setWeight(Math.max(1, Math.min(20, parseInt(e.target.value, 10) || 1)))}
            />
            <div style={noteStyle}>
              How many slots this brand occupies in the vertical&rsquo;s rotation. Clamped
              server-side to the orchestrator&rsquo;s own 1–20 range.
            </div>
          </div>
        </div>
      </Panel>

      {/* ── Step 2 · per-ISP budgets ─────────────────────────────────────── */}
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader
          title="2 · Per-ISP budgets"
          icon={faGaugeHigh}
          right={<span style={{ fontSize: 11, color: colors.textMuted }}>seed-only</span>}
        />
        <div style={{ fontSize: 12, color: colors.textMuted, marginBottom: 10, lineHeight: 1.6 }}>
          {opts?.budget_effective_note ??
            'Budgets seeded here take effect tomorrow (Denver); the cell is created at 0/day today.'}
          {' '}An increase also has to fit the global ceiling
          (<code>{opts?.budget_ceiling_env ?? 'PROPERTY_LEDGER_TOTAL_MAX'}</code>); with that env
          unset, every increase is refused by design.
        </div>
        <div style={cardGrid(150)}>
          {(opts?.isps ?? []).map(isp => (
            <div key={isp}>
              <label style={labelStyle}>{isp}</label>
              <input
                style={inputStyle} type="number" min={0} placeholder="—"
                value={budgets[isp] ?? ''}
                onChange={e => setBudgets(b => ({ ...b, [isp]: e.target.value }))}
              />
            </div>
          ))}
        </div>
        {(!opts || opts.isps.length === 0) && (
          <EmptyState title="No ISP authority loaded" hint="The options call has not returned yet." />
        )}
      </Panel>

      {/* ── Step 3 · verify ──────────────────────────────────────────────── */}
      <Panel style={{ marginBottom: 14 }}>
        <SectionHeader
          title="3 · Verify — the seven gates"
          icon={faShieldHalved}
          right={
            <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
              {verifiedAt && <span style={{ fontSize: 11, color: colors.textFaint }}>fetched {verifiedAt}</span>}
              <button
                style={{ ...btnStyle, opacity: vertical ? 1 : 0.5, cursor: vertical ? 'pointer' : 'not-allowed' }}
                disabled={!vertical || verifying}
                onClick={() => void runVerify(vertical, brand)}
              >
                {verifying ? <><FontAwesomeIcon icon={faSpinner} spin /> Verifying…</> : 'Re-verify'}
              </button>
            </div>
          }
        />

        {!vertical && <EmptyState icon={faRoute} title="Pick a vertical" hint="Gates evaluate live as you select." />}

        {vertical && verifyErr && (
          <SectionError label="Verify" error={verifyErr} onRetry={() => void runVerify(vertical, brand)} />
        )}

        {vertical && verifying && !verify && (
          <div style={{ padding: 20, color: colors.textMuted, fontSize: 13 }}>
            <FontAwesomeIcon icon={faSpinner} spin /> Evaluating gates…
          </div>
        )}

        {verify && (
          <>
            <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 12 }}>
              <Pill color={verdictColor(verify.verdict)}>{verify.verdict}</Pill>
              <span style={{ fontSize: 12, color: colors.textMuted }}>
                {verify.vertical}{verify.brand ? ` / ${verify.brand}` : ` · ${verify.brands.length} active brand(s)`}
              </span>
            </div>

            <div style={{ display: 'grid', gap: 8 }}>
              {verify.gates.map(g => (
                <div
                  key={g.name}
                  style={{
                    display: 'flex', gap: 10, alignItems: 'flex-start',
                    padding: '9px 12px', borderRadius: 8,
                    background: alpha(gateColor(g.status), '0d'),
                    border: `1px solid ${alpha(gateColor(g.status), '33')}`,
                  }}
                >
                  <FontAwesomeIcon
                    icon={gateIcon(g.status)}
                    style={{ color: gateColor(g.status), marginTop: 2, width: 16 }}
                  />
                  <div style={{ minWidth: 92 }}>
                    <div style={{ fontSize: 12, fontWeight: 700, color: colors.heading, letterSpacing: 0.4 }}>
                      {g.name}
                    </div>
                    <div style={{ fontSize: 10, color: colors.textFaint, textTransform: 'uppercase' }}>
                      {g.fatal ? 'blocking' : 'advisory'}
                    </div>
                  </div>
                  <div style={{ fontSize: 12, color: colors.text, lineHeight: 1.6, flex: 1 }}>
                    {g.detail}
                  </div>
                </div>
              ))}
            </div>

            {verify.touches.length > 0 && (
              <div style={{ marginTop: 16, overflowX: 'auto' }}>
                <div style={{ ...panelTitleStyle, fontSize: 11, marginBottom: 6 }}>Ladder as configured</div>
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={numTh}>Touch</th>
                      <th style={thStyle}>Source</th>
                      <th style={thStyle}>Scope</th>
                      <th style={thStyle}>Creative</th>
                      <th style={thStyle}>Offer</th>
                      <th style={thStyle}>Active</th>
                    </tr>
                  </thead>
                  <tbody>
                    {verify.touches.map((t, i) => {
                      const inert = t.source.includes('followup') && (t.touch < 2 || t.touch > verify.max_touch);
                      return (
                        <tr key={`${t.source}-${t.touch}-${t.scope}-${i}`}>
                          <td style={numTd}>{t.touch}</td>
                          <td style={tdStyle}>{t.source.replace('partner_drip_', '')}</td>
                          <td style={tdStyle}>{t.scope}</td>
                          <td style={tdStyle} title={t.creative_filename || 'offer-center (resolved from the offer pools)'}>
                            {t.creative_filename || <span style={{ color: colors.indigo300 }}>offer-center</span>}
                          </td>
                          <td style={tdStyle} title={t.offer_id || ''}>
                            <span style={{ fontFamily: 'monospace', fontSize: 11 }}>
                              {t.offer_id ? `${t.offer_id.slice(0, 8)}…` : '—'}
                            </span>
                          </td>
                          <td style={tdStyle}>
                            {inert
                              ? <Pill color={colors.warning}>inert</Pill>
                              : <Pill color={t.active ? colors.success : colors.idle}>{t.active ? 'active' : 'off'}</Pill>}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}

            {verify.budgets.length > 0 && (
              <div style={{ marginTop: 16, overflowX: 'auto' }}>
                <div style={{ ...panelTitleStyle, fontSize: 11, marginBottom: 6 }}>Existing budget cells</div>
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={thStyle}>ISP</th>
                      <th style={numTh}>Daily</th>
                      <th style={numTh}>Pending</th>
                      <th style={thStyle}>Effective</th>
                      <th style={thStyle}>Hold</th>
                      <th style={numTh}>Lock</th>
                    </tr>
                  </thead>
                  <tbody>
                    {verify.budgets.map(b => (
                      <tr key={b.isp}>
                        <td style={tdStyle}>{b.isp}</td>
                        <td style={numTd}>{num(b.daily_budget)}</td>
                        <td style={numTd}>{b.pending_budget != null ? num(b.pending_budget) : '—'}</td>
                        <td style={tdStyle}>{b.pending_effective_day || '—'}</td>
                        <td style={tdStyle}>
                          {b.hold ? <Pill color={colors.danger}>held</Pill> : <span style={{ color: colors.textFaint }}>—</span>}
                        </td>
                        <td style={numTd}>{b.lock_version}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
                <div style={noteStyle}>
                  Existing cells are never modified by this screen — the Property Ledger owns
                  budget edits (it holds the lock-version CAS, the min/max clamp and the hold
                  bookkeeping).
                </div>
              </div>
            )}
          </>
        )}
      </Panel>

      {/* ── Step 4 · commit ──────────────────────────────────────────────── */}
      <Panel accent={blockers.length === 0 ? colors.success : colors.warning}>
        <SectionHeader title="4 · Commit" icon={faCircleCheck} />
        {blockers.length > 0 && (
          <ul style={{ margin: '0 0 12px 18px', padding: 0, fontSize: 12, color: colors.warningText, lineHeight: 1.8 }}>
            {blockers.map(b => <li key={b}>{b}</li>)}
          </ul>
        )}
        <div style={{ display: 'flex', gap: 12, alignItems: 'center', flexWrap: 'wrap' }}>
          <button
            style={{
              ...btnStyle,
              background: blockers.length === 0 ? alpha(colors.success, '22') : alpha(colors.idle, '22'),
              border: `1px solid ${alpha(blockers.length === 0 ? colors.success : colors.idle, '66')}`,
              color: blockers.length === 0 ? colors.successText : colors.textMuted,
              cursor: blockers.length === 0 && !committing ? 'pointer' : 'not-allowed',
              padding: '9px 18px',
            }}
            disabled={blockers.length > 0 || committing}
            onClick={() => void commit()}
          >
            {committing
              ? <><FontAwesomeIcon icon={faSpinner} spin /> Wiring lane…</>
              : 'Wire this lane'}
          </button>
          <span style={{ fontSize: 11, color: colors.textMuted, maxWidth: 620, lineHeight: 1.6 }}>
            Writes the roster binding, the welcome creative, the follow-up ladder and any
            not-yet-existing budget cells. No campaign is created and no recipient is queued.
          </span>
        </div>

        {commitErr && (
          <div style={{ marginTop: 12 }}>
            <SectionError label="Commit refused" error={commitErr} />
          </div>
        )}

        {commitRes && (
          <div style={{ marginTop: 14 }}>
            <div style={{ fontSize: 13, color: colors.successText, marginBottom: 8 }}>
              <FontAwesomeIcon icon={faCircleCheck} /> Lane wired: {commitRes.vertical} / {commitRes.brand}
              {' '}→ {commitRes.sending_domain} [{commitRes.transport}]
              {commitRes.weight_clamped && ' · weight clamped to the orchestrator range'}
            </div>
            <div style={{ fontSize: 12, color: colors.textMuted, marginBottom: 8 }}>
              Follow-up touches written: {commitRes.followup_touches.length
                ? commitRes.followup_touches.join(', ')
                : 'none (welcome-only ladder)'}
            </div>
            {commitRes.budgets.length > 0 && (
              <div style={{ overflowX: 'auto' }}>
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <th style={thStyle}>ISP</th>
                      <th style={thStyle}>Action</th>
                      <th style={numTh}>Staged</th>
                      <th style={thStyle}>Effective</th>
                      <th style={thStyle}>Note</th>
                    </tr>
                  </thead>
                  <tbody>
                    {commitRes.budgets.map(b => (
                      <tr key={b.isp}>
                        <td style={tdStyle}>{b.isp}</td>
                        <td style={tdStyle}>
                          <Pill color={b.action === 'seeded' ? colors.success : colors.warning}>{b.action}</Pill>
                        </td>
                        <td style={numTd}>{b.staged_budget != null ? num(b.staged_budget) : '—'}</td>
                        <td style={tdStyle}>{b.effective_day || '—'}</td>
                        <td style={{ ...tdStyle, color: colors.textMuted }}>{b.message}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
            <div style={noteStyle}>{commitRes.budget_note}</div>
          </div>
        )}
      </Panel>

      <div style={{ marginTop: 14, fontSize: 10, color: colors.textFaint }}>{PAGE_VERSION}</div>
    </div>
  );
};

export default DripLaneOnboarding;
