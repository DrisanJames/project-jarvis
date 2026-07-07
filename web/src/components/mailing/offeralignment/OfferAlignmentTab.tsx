// OfferAlignmentTab.tsx — the "Offer Alignment" tab (replaces the old
// Creatives tab in the Event Explorer).
//
// The primary object is the DECISION: offer × ISP × data-source × creative ×
// subject. Two independent visual channels — PERFORMANCE (RPM / click rates,
// sequential indigo ramp) and DELIVERY HEALTH (badges) — never blended.
//
// View routing is component state (no React Router), three levels:
//   matrix   (Level 0) — offers × ISPs snapshot, window 7d/30d
//   profile  (Level 1) — one offer: KPIs, per-ISP table, creatives, sources
//   evidence (Level 2) — grouped SMTP evidence behind a BLOCKING/THROTTLED badge
//
// The matrix stays mounted (display:none) while drilled in so its snapshot
// and metric selection survive the round trip. Each level owns its fetch and
// fails soft — one failing panel never blanks the tab.
//
// Default window: last 7 days, America/Denver (lifetime is never a default).

import React, { useCallback, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faCrosshairs } from '@fortawesome/free-solid-svg-icons';
import { daysAgoDenver, denverToday } from '../shared/filters';
import AlignmentMatrix from './AlignmentMatrix';
import OfferProfile from './OfferProfile';
import EvidencePanel from './EvidencePanel';
import './OfferAlignmentTab.css';

type View =
  | { level: 'matrix' }
  | { level: 'profile'; offer: string; offerName: string; focusIsp?: string }
  | { level: 'evidence'; offer: string; offerName: string; isp: string; badge?: string };

export const OfferAlignmentTab: React.FC = () => {
  const [view, setView] = useState<View>({ level: 'matrix' });
  const [windowDays, setWindowDays] = useState<7 | 30>(7);
  // Profile/evidence date range — seeded from the matrix window on drill-in,
  // then freely editable inside the profile.
  const [range, setRange] = useState<{ from: string; to: string }>(() => ({
    from: daysAgoDenver(6),
    to: denverToday(),
  }));

  const openProfile = useCallback(
    (offer: string, offerName: string, isp?: string) => {
      setRange({ from: daysAgoDenver(windowDays === 7 ? 6 : 29), to: denverToday() });
      setView({ level: 'profile', offer, offerName, focusIsp: isp });
    },
    [windowDays]
  );

  const openEvidenceFromMatrix = useCallback(
    (offer: string, offerName: string, isp: string, badge: string) => {
      setRange({ from: daysAgoDenver(windowDays === 7 ? 6 : 29), to: denverToday() });
      setView({ level: 'evidence', offer, offerName, isp, badge });
    },
    [windowDays]
  );

  const backToMatrix = useCallback(() => setView({ level: 'matrix' }), []);

  return (
    <div className="oa-tab-root">
      {/* ── Tab header ── */}
      <div className="oa-tab-header">
        <div>
          <h2 className="oa-tab-title">
            <FontAwesomeIcon icon={faCrosshairs} className="oa-tab-title-icon" />
            Offer Alignment
          </h2>
          <p className="oa-tab-subtitle">
            The decision surface: offer × ISP × data-source × creative × subject. Color carries{' '}
            <b>performance</b> (RPM / click rates); badges carry <b>delivery health</b> — two independent
            channels, never blended. Click a cell for the offer profile; click a red/amber badge for the SMTP
            evidence. Every rate discloses its denominator. Days are America/Denver.
          </p>
        </div>
        {view.level !== 'matrix' && (
          <div className="oa-tab-crumbs">
            <button type="button" className="oa-tab-crumb" onClick={backToMatrix}>Matrix</button>
            <span className="oa-tab-crumb-sep">›</span>
            {view.level === 'profile' ? (
              <span className="oa-tab-crumb-here">{view.offerName || view.offer}</span>
            ) : (
              <>
                <button
                  type="button"
                  className="oa-tab-crumb"
                  onClick={() => setView({ level: 'profile', offer: view.offer, offerName: view.offerName, focusIsp: view.isp })}
                >
                  {view.offerName || view.offer}
                </button>
                <span className="oa-tab-crumb-sep">›</span>
                <span className="oa-tab-crumb-here">evidence · {view.isp}</span>
              </>
            )}
          </div>
        )}
      </div>

      {/* ── Level 0: matrix (stays mounted while drilled in) ── */}
      <div style={{ display: view.level === 'matrix' ? 'block' : 'none' }}>
        <AlignmentMatrix
          windowDays={windowDays}
          onWindowChange={setWindowDays}
          onOpenProfile={openProfile}
          onOpenEvidence={openEvidenceFromMatrix}
        />
      </div>

      {/* ── Level 1: offer profile ── */}
      {view.level === 'profile' && (
        <OfferProfile
          key={view.offer}
          offer={view.offer}
          offerName={view.offerName}
          focusIsp={view.focusIsp}
          from={range.from}
          to={range.to}
          onRangeChange={(from, to) => setRange({ from, to })}
          onBack={backToMatrix}
          onOpenEvidence={(isp, badge) =>
            setView({ level: 'evidence', offer: view.offer, offerName: view.offerName, isp, badge })
          }
        />
      )}

      {/* ── Level 2: SMTP evidence ── */}
      {view.level === 'evidence' && (
        <EvidencePanel
          key={`${view.offer}|${view.isp}`}
          offer={view.offer}
          offerName={view.offerName}
          isp={view.isp}
          badge={view.badge}
          from={range.from}
          to={range.to}
          onBack={() => setView({ level: 'profile', offer: view.offer, offerName: view.offerName, focusIsp: view.isp })}
        />
      )}
    </div>
  );
};

export default OfferAlignmentTab;
