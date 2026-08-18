import React from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faExclamationTriangle, faMousePointer, faEnvelopeOpen, faSpinner } from '@fortawesome/free-solid-svg-icons';

/**
 * Engagement-range picker — the send-day board's audience primitive, in the UI.
 *
 * The board builds its engaged tier from ENGAGEMENT RANGES (clickers and
 * openers over a recency window), not from segment UUIDs. This panel renders
 * the live grid returned by
 * `GET /api/mailing/pmta-campaign/engagement-tiers?sending_domain=…` — one chip
 * per (kind, window) that actually exists for the property, with its real
 * member count — so the operator picks a range instead of hunting a name.
 *
 * Data only, no fetching: the wizard owns the request so one call serves both
 * this panel and the offer/creative step (which needs the same brand root).
 */

export interface EngagementTier {
  segment_id: string;
  name: string;
  window_days: number;
  /** Authoritative membership from mailing_segment_build_ledger. */
  count: number;
  /** The cached mailing_segments.subscriber_count. */
  counter_count: number;
  /** True when the cached counter disagrees with live membership. */
  counter_mismatch: boolean;
  /** False when the ledger was unreadable or has no row; `count` fell back to the counter. */
  count_is_live: boolean;
  /** Last build status from the ledger — 'running' means the count is the PREVIOUS build. */
  build_status?: string;
  built_at?: string;
  last_calculated_at?: string;
  stale: boolean;
}

export interface EngagementTiers {
  sending_domain: string;
  brand_root: string;
  clickers: EngagementTier[];
  openers: EngagementTier[];
  other: EngagementTier[];
}

interface Props {
  tiers: EngagementTiers | null;
  loading: boolean;
  error: string;
  selectedClickerIds: string[];
  selectedOpenerIds: string[];
  onToggle: (kind: 'clickers' | 'openers', segmentId: string) => void;
  excludeClickers: boolean;
  onExcludeClickersChange: (v: boolean) => void;
  onRetry: () => void;
}

const CLICK_COLOR = '#f59e0b';   // clicks = GOLD (signal-grading doctrine)
const OPEN_COLOR = '#94a3b8';    // opens  = silver

const fmt = (n: number) => n.toLocaleString();

const Chip: React.FC<{
  tier: EngagementTier;
  selected: boolean;
  color: string;
  onClick: () => void;
}> = ({ tier, selected, color, onClick }) => (
  <button
    type="button"
    onClick={onClick}
    title={[
      tier.name,
      `${tier.count.toLocaleString()} members (live)`,
      tier.counter_mismatch
        ? `⚠ the cached segment counter says ${tier.counter_count.toLocaleString()} — the per-segment refresh is failing to write its tally. The ledger number above is what mails.`
        : '',
      tier.count_is_live ? '' : '⚠ build ledger unreadable; showing the cached counter',
      tier.build_status && tier.build_status !== 'ok'
        ? `⚠ last build status "${tier.build_status}" — this count describes the previous build`
        : '',
      tier.built_at ? `built ${tier.built_at}` : '',
      tier.stale ? `⚠ last built ${tier.last_calculated_at || 'never'} — the daily refresh has not run` : `last built ${tier.last_calculated_at || 'unknown'}`,
    ].filter(Boolean).join('\n')}
    style={{
      display: 'flex', flexDirection: 'column', alignItems: 'flex-start', gap: 2,
      minWidth: 108, padding: '8px 12px', cursor: 'pointer', textAlign: 'left',
      background: selected ? `${color}18` : '#0a0f1a',
      border: `1.5px solid ${selected ? color : 'rgba(0,200,255,0.08)'}`,
      borderRadius: 8, transition: 'all 0.15s ease',
    }}
  >
    <span style={{ display: 'flex', alignItems: 'center', gap: 5, fontSize: 13, fontWeight: 700, color: selected ? color : '#e0e6f0' }}>
      {tier.window_days}D
      {(tier.stale || tier.counter_mismatch || !tier.count_is_live ||
        (!!tier.build_status && tier.build_status !== 'ok')) && (
        <FontAwesomeIcon icon={faExclamationTriangle} style={{ fontSize: 9, color: '#f59e0b' }} />
      )}
    </span>
    <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.6)' }}>{fmt(tier.count)}</span>
  </button>
);

const Row: React.FC<{
  label: string; icon: any; color: string; hint: string;
  tiers: EngagementTier[]; selected: string[];
  onToggle: (id: string) => void;
}> = ({ label, icon, color, hint, tiers, selected, onToggle }) => (
  <div style={{ display: 'flex', alignItems: 'flex-start', gap: 12, padding: '8px 0' }}>
    <div style={{ width: 118, flexShrink: 0, paddingTop: 8 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 12, fontWeight: 600, color }}>
        <FontAwesomeIcon icon={icon} style={{ fontSize: 11 }} /> {label}
      </div>
      <div style={{ fontSize: 10, color: 'rgba(180,210,240,0.45)', marginTop: 2 }}>{hint}</div>
    </div>
    <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, flex: 1 }}>
      {tiers.length === 0
        ? <div style={{ fontSize: 12, color: 'rgba(180,210,240,0.4)', padding: '10px 0' }}>
            No {label.toLowerCase()} segments exist for this property.
          </div>
        : tiers.map(t => (
            <Chip key={t.segment_id} tier={t} color={color}
                  selected={selected.includes(t.segment_id)}
                  onClick={() => onToggle(t.segment_id)} />
          ))}
    </div>
  </div>
);

export const EngagementTierPicker: React.FC<Props> = ({
  tiers, loading, error, selectedClickerIds, selectedOpenerIds, onToggle,
  excludeClickers, onExcludeClickersChange, onRetry,
}) => {
  const box: React.CSSProperties = {
    background: '#0d1526', border: '1px solid rgba(0,200,255,0.08)',
    borderRadius: 10, padding: 16, marginBottom: 16,
  };

  if (loading) {
    return (
      <div style={box}>
        <FontAwesomeIcon icon={faSpinner} spin style={{ marginRight: 8, color: '#00b0ff' }} />
        <span style={{ fontSize: 13, color: 'rgba(180,210,240,0.7)' }}>Loading engagement ranges…</span>
      </div>
    );
  }
  if (error) {
    return (
      <div style={{ ...box, borderColor: 'rgba(239,68,68,0.35)' }}>
        <div style={{ fontSize: 13, color: '#ef4444', marginBottom: 8 }}>
          <FontAwesomeIcon icon={faExclamationTriangle} /> Engagement ranges unavailable — {error}
        </div>
        <button onClick={onRetry} style={{ background: '#0a0f1a', color: '#00b0ff', border: '1px solid rgba(0,200,255,0.2)', borderRadius: 6, padding: '5px 12px', fontSize: 12, cursor: 'pointer' }}>Retry</button>
      </div>
    );
  }
  if (!tiers) {
    return (
      <div style={box}>
        <div style={{ fontSize: 13, color: 'rgba(180,210,240,0.55)' }}>Select a sending domain to load its engagement ranges.</div>
      </div>
    );
  }

  const all = [...tiers.clickers, ...tiers.openers];
  const selectedAll = [...selectedClickerIds, ...selectedOpenerIds];
  const selectedTiers = all.filter(t => selectedAll.includes(t.segment_id));
  const rawTotal = selectedTiers.reduce((s, t) => s + t.count, 0);
  const empty = tiers.clickers.length === 0 && tiers.openers.length === 0;
  const canDisjoin = selectedClickerIds.length > 0 && selectedOpenerIds.length > 0;

  return (
    <div style={box}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', marginBottom: 4 }}>
        <h4 style={{ margin: 0, fontSize: 14, color: '#e0e6f0' }}>Engagement range</h4>
        <span style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)' }}>{tiers.brand_root}</span>
      </div>
      <p style={{ margin: '0 0 8px', fontSize: 12, color: 'rgba(180,210,240,0.55)' }}>
        Pick the recency windows that make up this send's engaged audience. Counts are the live
        segment membership, rebuilt daily.
      </p>

      {empty ? (
        <div style={{ padding: '14px 0', fontSize: 13, color: '#f59e0b' }}>
          <FontAwesomeIcon icon={faExclamationTriangle} /> No engagement segments exist for{' '}
          <strong>{tiers.brand_root}</strong>. Use the advanced picker below, or have the segment
          family built for this property first.
        </div>
      ) : (
        <>
          <Row label="Clickers" icon={faMousePointer} color={CLICK_COLOR}
               hint="clicked in window" tiers={tiers.clickers}
               selected={selectedClickerIds} onToggle={id => onToggle('clickers', id)} />
          <Row label="Openers" icon={faEnvelopeOpen} color={OPEN_COLOR}
               hint="opened in window" tiers={tiers.openers}
               selected={selectedOpenerIds} onToggle={id => onToggle('openers', id)} />

          {[...tiers.clickers, ...tiers.openers].some(t => t.counter_mismatch) && (
            <div style={{
              marginTop: 10, padding: '8px 10px', borderRadius: 8,
              background: 'rgba(245,158,11,0.08)', border: '1px solid rgba(245,158,11,0.35)',
              fontSize: 11, color: '#f59e0b',
            }}>
              <FontAwesomeIcon icon={faExclamationTriangle} /> Some cached segment counters disagree
              with the segment build ledger. The numbers above come from the LEDGER — what the builder
              actually materialized, and what the planner will mail. A disagreement means the
              per-segment refresh failed to write its tally (usually a query timeout under DB load),
              not that the audience is missing.
            </div>
          )}

          <label style={{
            display: 'flex', alignItems: 'flex-start', gap: 8, marginTop: 10,
            cursor: canDisjoin ? 'pointer' : 'default', opacity: canDisjoin ? 1 : 0.45,
          }}>
            <input type="checkbox" checked={excludeClickers && canDisjoin} disabled={!canDisjoin}
                   onChange={e => onExcludeClickersChange(e.target.checked)}
                   style={{ marginTop: 2, width: 15, height: 15, cursor: canDisjoin ? 'pointer' : 'default' }} />
            <span style={{ fontSize: 12, color: 'rgba(180,210,240,0.75)' }}>
              Exclude the selected clickers from this send (engager disjointness — the board's
              OFR-ENG campaigns exclude their own clicker segments so the two tiers never
              double-mail the same person on the same day).
            </span>
          </label>

          <div style={{
            marginTop: 12, paddingTop: 10, borderTop: '1px solid rgba(0,200,255,0.06)',
            fontSize: 12, color: 'rgba(180,210,240,0.7)',
          }}>
            {selectedTiers.length === 0
              ? <span style={{ color: 'rgba(180,210,240,0.45)' }}>No engagement range selected.</span>
              : <>
                  <strong style={{ color: '#e0e6f0' }}>{selectedTiers.map(t => t.name).join(' + ')}</strong>
                  <span> — up to {fmt(rawTotal)} before dedupe and suppression</span>
                </>}
          </div>
        </>
      )}

      {tiers.other.length > 0 && (
        <details style={{ marginTop: 10 }}>
          <summary style={{ fontSize: 11, color: 'rgba(180,210,240,0.5)', cursor: 'pointer' }}>
            {tiers.other.length} other engagement segment(s) for this property (no recency window)
          </summary>
          <div style={{ fontSize: 11, color: 'rgba(180,210,240,0.45)', marginTop: 6 }}>
            {tiers.other.map(t => `${t.name} (${fmt(t.count)})`).join(' · ')} — select these from the
            advanced picker below if you need them.
          </div>
        </details>
      )}
    </div>
  );
};

export default EngagementTierPicker;
