// DripOverviewPanel — the GLOBAL dashboard at the top of the Drip Journey
// screen: estate-wide clean/dirty totals, cold-data pipeline stats, per-ISP
// composition, and a by-lane table that spotlights a lane into the existing
// vertical selector on click.
//
// Data: GET /api/mailing/pmta-campaign/property-ledger/overview (NEW endpoint,
// may 404 until the backend deploys — this panel degrades to its error state
// with Retry and never blocks the rest of the screen).
//
// Friendly names: PUT /api/mailing/pmta-campaign/property-ledger/lane-label
// {vertical, display_name} — empty string deletes the label. The write is
// gated server-side like roster writes; a rejected write surfaces the server's
// error and reverts the optimistic value.

import React, { useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faGlobe, faPencilAlt, faUpload, faPlus } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import {
  colors, alpha, thStyle, tdStyle, numTd, numTh, tableStyle,
} from '../shared/theme';
import { SectionHeader, Stat, SectionError, EmptyState } from '../shared/ui';

// ── API shapes (contract for the parallel backend build) ────────────────────

export interface OverviewTotals {
  ready: number;
  cleaning: number;
  pending_eo: number;
  eo_in_flight: number;
  held: number;
  suppressed: number;
  dead_letter: number;
  mailed_today: number;
  mailed_lifetime: number;
  clean: number;
  dirty: number;
}
export interface OverviewISP {
  isp: string;
  ready: number;
  mailed_today: number;
}
export interface OverviewLane {
  vertical: string;
  display_name: string;
  feeds: number;
  ready: number;
  cleaning: number;
  pending_eo: number;
  suppressed: number;
  dead_letter: number;
  mailed_today: number;
  tranche_total: number;
  clean: number;
  dirty: number;
}
export interface OverviewResponse {
  computed_at: string;
  totals: OverviewTotals;
  by_isp: OverviewISP[];
  by_lane: OverviewLane[];
}

// ── Shared lane-label writer (also used by the selector-side rename) ────────

export async function saveLaneLabel(
  vertical: string,
  displayName: string,
): Promise<{ ok: boolean; error?: string }> {
  try {
    const r = await apiFetch('/api/mailing/pmta-campaign/property-ledger/lane-label', {
      method: 'PUT',
      body: JSON.stringify({ vertical, display_name: displayName }),
    });
    if (!r.ok) {
      let msg = `HTTP ${r.status}`;
      try {
        const j = (await r.json()) as { error?: unknown };
        if (j && typeof j.error === 'string') msg = j.error;
      } catch { /* non-JSON error body */ }
      return { ok: false, error: msg };
    }
    return { ok: true };
  } catch (e) {
    return { ok: false, error: e instanceof Error ? e.message : 'network error' };
  }
}

// ── Local formatting — an unknown is never 0 ────────────────────────────────

const UNKNOWN = '—';
const num = (n: number | null | undefined): string =>
  typeof n === 'number' && Number.isFinite(n) ? Math.round(n).toLocaleString() : UNKNOWN;
const pctOf = (n: number | null | undefined, d: number | null | undefined): string =>
  typeof n === 'number' && typeof d === 'number' && d > 0 ? `${((n / d) * 100).toFixed(2)}%` : UNKNOWN;
const shortTime = (iso: string | null | undefined): string => {
  if (!iso) return UNKNOWN;
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return UNKNOWN;
  return d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
};

const smallBtn: React.CSSProperties = {
  background: alpha(colors.indigo500, '22'),
  color: colors.indigo200,
  border: `1px solid ${alpha(colors.indigo500, '66')}`,
  borderRadius: 5,
  padding: '4px 10px',
  fontSize: 11,
  fontWeight: 700,
  cursor: 'pointer',
};
const inputStyle: React.CSSProperties = {
  background: 'rgba(255,255,255,0.06)',
  color: colors.text,
  border: `1px solid ${colors.panelBorderStrong}`,
  borderRadius: 5,
  padding: '3px 6px',
  fontSize: 11,
};

// ── Inline lane rename (pencil → input → save) ──────────────────────────────

export const LaneRename: React.FC<{
  vertical: string;
  currentName: string;   // display_name if set, else '' (falls back to vertical)
  onSaved: () => void;   // reload the overview after a successful write
  onNotice: (s: string) => void;
}> = ({ vertical, currentName, onSaved, onNotice }) => {
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState(currentName);
  const [busy, setBusy] = useState(false);

  const commit = async () => {
    if (busy) return;
    const next = value.trim();
    if (next === currentName.trim()) { setEditing(false); return; }
    setBusy(true);
    const res = await saveLaneLabel(vertical, next);
    setBusy(false);
    if (!res.ok) {
      onNotice(`Lane rename failed for ${vertical}: ${res.error ?? 'unknown error'}`);
      setValue(currentName);
      setEditing(false);
      return;
    }
    onNotice(next === ''
      ? `Friendly name removed from ${vertical} — it shows its vertical id again.`
      : `Lane ${vertical} renamed to “${next}”.`);
    setEditing(false);
    onSaved();
  };

  if (!editing) {
    return (
      <button
        type="button"
        title={`Rename this lane (friendly display name). Current: ${currentName || '(none — shows the vertical id)'}. An empty name deletes the label.`}
        onClick={(e) => { e.stopPropagation(); setValue(currentName); setEditing(true); }}
        style={{
          background: 'none', border: 'none', cursor: 'pointer', padding: '0 4px',
          color: colors.textFaint, fontSize: 11,
        }}
      >
        <FontAwesomeIcon icon={faPencilAlt} />
      </button>
    );
  }
  return (
    <span style={{ display: 'inline-flex', gap: 4, alignItems: 'center' }} onClick={(e) => e.stopPropagation()}>
      <input
        style={{ ...inputStyle, width: 160 }}
        value={value}
        autoFocus
        disabled={busy}
        placeholder="(empty deletes the label)"
        onChange={(e) => setValue(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') void commit();
          if (e.key === 'Escape') { setValue(currentName); setEditing(false); }
        }}
      />
      <button type="button" style={{ ...smallBtn, padding: '2px 8px', fontSize: 10 }} disabled={busy} onClick={() => void commit()}>
        {busy ? '…' : 'Save'}
      </button>
      <button
        type="button"
        style={{ ...smallBtn, padding: '2px 8px', fontSize: 10, background: 'transparent', color: colors.textMuted, border: `1px solid ${colors.hairline}` }}
        disabled={busy}
        onClick={() => { setValue(currentName); setEditing(false); }}
      >
        Cancel
      </button>
    </span>
  );
};

// ── The panel ───────────────────────────────────────────────────────────────

export const DripOverviewPanel: React.FC<{
  data: OverviewResponse | null;
  loading: boolean;
  error: string | null;
  reload: () => void;
  selectedVertical: string | null;
  onSelectLane: (vertical: string) => void;
  onOpenIngest: () => void;
  onOnboardLane?: () => void;   // absent = no navigation mechanism wired; button hidden
  onNotice: (s: string) => void;
}> = ({ data, loading, error, reload, selectedVertical, onSelectLane, onOpenIngest, onOnboardLane, onNotice }) => {
  const t = data?.totals;
  const cleanDirtyDen = t ? t.clean + t.dirty : 0;
  const lanes = (data?.by_lane ?? []).slice().sort((a, b) => b.ready - a.ready);
  const isps = (data?.by_isp ?? []).slice().sort((a, b) => b.ready - a.ready);
  const ispReadyTotal = isps.reduce((a, r) => a + (Number.isFinite(r.ready) ? r.ready : 0), 0);

  return (
    <>
      <SectionHeader
        title="All lanes — global drip dashboard"
        icon={faGlobe}
        right={
          <span style={{ display: 'inline-flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
            {data?.computed_at && (
              <span style={{ fontSize: 11, color: colors.textFaint }} title="When the server computed this rollup.">
                as of {shortTime(data.computed_at)}
              </span>
            )}
            <button type="button" style={smallBtn} onClick={onOpenIngest}
              title="Upload a CSV of records into a feed (preview → mapping → commit).">
              <FontAwesomeIcon icon={faUpload} style={{ marginRight: 5 }} />Ingest data
            </button>
            {onOnboardLane && (
              <button type="button" style={smallBtn} onClick={onOnboardLane}
                title="Opens the Lane Onboarding sub-tab.">
                <FontAwesomeIcon icon={faPlus} style={{ marginRight: 5 }} />Onboard a new lane
              </button>
            )}
          </span>
        }
      />

      {loading && (
        <div style={{ fontSize: 12, color: colors.textMuted, padding: '14px 4px' }}>
          Loading the global overview…
        </div>
      )}
      {!loading && error && (
        <SectionError
          label="Global overview (the /overview endpoint may not be deployed yet)"
          error={error}
          onRetry={reload}
        />
      )}
      {!loading && !error && (!t || lanes.length === 0) && (
        <EmptyState
          title="No overview data"
          hint="The overview endpoint answered without totals or lanes — nothing is hidden; there is nothing rolled up yet."
        />
      )}

      {!loading && !error && t && lanes.length > 0 && (
        <>
          {/* Clean vs dirty + cold-data pipeline */}
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(150px, 1fr))', gap: 14, marginBottom: 14 }}>
            <Stat
              label="Clean"
              value={num(t.clean)}
              sub={`${pctOf(t.clean, cleanDirtyDen)} of clean+dirty (${num(cleanDirtyDen)})`}
              color={colors.successText}
              title="Records the cleaning pipeline has validated, estate-wide across every drip lane."
            />
            <Stat
              label="Dirty"
              value={num(t.dirty)}
              sub={`${pctOf(t.dirty, cleanDirtyDen)} of clean+dirty (${num(cleanDirtyDen)})`}
              color={t.dirty > 0 ? colors.warningText : colors.text}
              title="Records not yet validated (or failed validation), estate-wide."
            />
            <Stat
              label="Ready now"
              value={num(t.ready)}
              sub="claimable across all lanes"
              color={t.ready > 0 ? colors.successText : colors.warningText}
              title="EO-validated records sitting claimable in partner_clean_queue, all lanes."
            />
            <Stat
              label="In cleaning"
              value={num(t.cleaning)}
              sub={`pending EO ${num(t.pending_eo)} · in flight ${num(t.eo_in_flight)}`}
              title="Records inside the EmailOversight validation pipeline — not claimable yet."
            />
            <Stat
              label="Held"
              value={num(t.held)}
              sub={`suppressed ${num(t.suppressed)} · dead ${num(t.dead_letter)}`}
              color={colors.warningText}
              title="Held = parked by release caps (become ready later). Suppressed and dead-letter never will."
            />
            <Stat
              label="Mailed today"
              value={num(t.mailed_today)}
              sub={`lifetime ${num(t.mailed_lifetime)}`}
              title="First touches stamped today across all drip lanes; lifetime is all-time."
            />
          </div>

          {/* ISP composition of the ready supply */}
          {isps.length > 0 && (
            <div style={{ marginBottom: 14 }}>
              <div style={{ fontSize: 10, letterSpacing: 0.6, textTransform: 'uppercase', color: colors.textMuted, marginBottom: 6 }}>
                Ready supply by ISP — % of {num(ispReadyTotal)} ready
              </div>
              <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(230px, 1fr))', gap: '4px 18px' }}>
                {isps.map((r) => {
                  const share = ispReadyTotal > 0 ? r.ready / ispReadyTotal : 0;
                  return (
                    <div key={r.isp} style={{ display: 'flex', alignItems: 'center', gap: 8 }}
                      title={`${r.isp}: ${num(r.ready)} ready (${pctOf(r.ready, ispReadyTotal)} of ready) · ${num(r.mailed_today)} mailed today`}>
                      <span style={{ fontSize: 11, color: colors.text, width: 76, flex: '0 0 auto' }}>{r.isp}</span>
                      <div style={{ flex: 1, height: 7, background: 'rgba(255,255,255,0.06)', borderRadius: 4 }}>
                        <div style={{
                          width: `${Math.max(share > 0 ? 2 : 0, Math.round(share * 100))}%`,
                          height: 7, background: alpha(colors.indigo400, '66'), borderRadius: 4,
                        }} />
                      </div>
                      <span style={{ fontSize: 10, color: colors.textMuted, minWidth: 88, textAlign: 'right', fontVariantNumeric: 'tabular-nums' }}>
                        {num(r.ready)} · {pctOf(r.ready, ispReadyTotal)}
                      </span>
                    </div>
                  );
                })}
              </div>
            </div>
          )}

          {/* By-lane table — click a row to spotlight the lane */}
          <div style={{ overflowX: 'auto' }}>
            <table style={{ ...tableStyle, minWidth: 980 }}>
              <thead>
                <tr>
                  <th style={thStyle}>Lane</th>
                  <th style={numTh}>Feeds</th>
                  <th style={numTh}>Ready</th>
                  <th style={numTh}>Cleaning</th>
                  <th style={numTh} title="Pending EmailOversight validation.">Pending EO</th>
                  <th style={numTh}>Mailed today</th>
                  <th style={numTh}>Clean</th>
                  <th style={numTh}>Dirty</th>
                  <th style={numTh} title="clean ÷ (clean + dirty) for this lane.">Clean %</th>
                  <th style={numTh} title="suppressed + dead_letter — records that will never mail.">Never-mail</th>
                  <th style={numTh}>Tranche</th>
                  <th style={thStyle} />
                </tr>
              </thead>
              <tbody>
                {lanes.map((l) => {
                  const sel = l.vertical === selectedVertical;
                  const laneDen = l.clean + l.dirty;
                  return (
                    <tr
                      key={l.vertical}
                      onClick={() => onSelectLane(l.vertical)}
                      title={`Spotlight ${l.display_name || l.vertical} — sets the drip selector to this lane.`}
                      style={{ cursor: 'pointer', background: sel ? alpha(colors.indigo400, '14') : undefined }}
                    >
                      <td style={{ ...tdStyle, fontWeight: sel ? 700 : 400 }}>
                        {l.display_name || l.vertical}
                        {l.display_name && (
                          <span style={{ fontSize: 10, color: colors.textFaint, marginLeft: 6, fontFamily: 'monospace' }}>
                            {l.vertical}
                          </span>
                        )}
                        <LaneRename
                          vertical={l.vertical}
                          currentName={l.display_name || ''}
                          onSaved={reload}
                          onNotice={onNotice}
                        />
                      </td>
                      <td style={numTd}>{num(l.feeds)}</td>
                      <td style={{ ...numTd, color: l.ready > 0 ? colors.successText : colors.textMuted }}>{num(l.ready)}</td>
                      <td style={numTd}>{num(l.cleaning)}</td>
                      <td style={numTd}>{num(l.pending_eo)}</td>
                      <td style={numTd}>{num(l.mailed_today)}</td>
                      <td style={numTd}>{num(l.clean)}</td>
                      <td style={{ ...numTd, color: l.dirty > 0 ? colors.warningText : colors.textMuted }}>{num(l.dirty)}</td>
                      <td style={numTd} title={`clean ${num(l.clean)} / (clean+dirty) ${num(laneDen)}`}>{pctOf(l.clean, laneDen)}</td>
                      <td style={{ ...numTd, color: colors.textMuted }} title={`suppressed ${num(l.suppressed)} · dead_letter ${num(l.dead_letter)}`}>
                        {num(l.suppressed + l.dead_letter)}
                      </td>
                      <td style={numTd}>{num(l.tranche_total)}</td>
                      <td style={{ ...tdStyle, fontSize: 10, color: colors.indigo300 }}>{sel ? 'spotlighted' : 'spotlight ▸'}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          <p style={{ fontSize: 11, color: colors.textMuted, lineHeight: 1.6, marginTop: 8 }}>
            Clicking a lane spotlights it in the drip selector above the journey canvas. The pencil
            sets a friendly display name (server-stored; empty deletes it). Clean % divides by that
            lane&apos;s clean+dirty — denominators differ per lane and are shown in each tooltip.
          </p>
        </>
      )}
    </>
  );
};

export default DripOverviewPanel;
