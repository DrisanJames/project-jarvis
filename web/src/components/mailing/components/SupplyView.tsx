// SupplyView.tsx — the portal's Supply tab (REQ-118 WP10).
//
// PAGE_VERSION 1 (2026-09-03) — first cut: the three panes of DRIP_SUPPLY_CHAIN_
// DESIGN.md §6 over the WP9 API (/api/mailing/supply/*).
//
// SHAPE: lane-driven, one section, progressive drill-down —
//
//     Ecosystem queue  →  Lane (record flow / contracts)  →  Domains
//        (polls 60s)         (study surface, no poll)      (capacity + contract)
//
// Order is deliberate and follows the operator's standing display doctrine:
// the IN-MOTION surface first (what the mediators are doing right now), then
// PLAN-AHEAD (contracts, scheduled for the next Denver midnight), then the
// ledgers as history. Lifetime aggregates do not appear at all — this whole tab
// is scoped to ONE Denver day.
//
// The day comes from the shared FilterBar (Denver presets + date inputs). The
// supply chain is a single-day surface, so the bar's `To` day IS the day; a
// wider range is reported as such rather than silently averaged. The lane is
// chosen by DRILL-DOWN (clicking a lane row), not by a second, forked filter —
// PORTAL_DESIGN_SYSTEM §3 forbids a one-off filter control.

import React from 'react'
import { colors, pageStyle } from '../shared/theme'
import { SubNav } from '../shared/SubNav'
import { FilterBar, FilterChip, denverToday, type LakeFilterDraft, type AppliedLakeFilters } from '../shared/filters'
import { SupplyEcosystem } from './SupplyEcosystem'
import { SupplyLane } from './SupplyLane'
import { SupplyDomains } from './SupplyDomains'
import { supplyGet, type EcosystemResponse, type EcosystemLaneRow } from './supplyShared'

type Pane = 'ecosystem' | 'lane' | 'domains'

const PANES = [
  { key: 'ecosystem', label: 'Ecosystem queue' },
  { key: 'lane', label: 'Lane' },
  { key: 'domains', label: 'Domains' },
]

const emptyDraft = (): LakeFilterDraft => ({
  from: denverToday(),
  to: denverToday(),
  ispGroup: '',
  brand: '',
  routeType: '',
  transport: 'combined',
})

export const SupplyView: React.FC = () => {
  const [pane, setPane] = React.useState<Pane>('ecosystem')
  const [draft, setDraft] = React.useState<LakeFilterDraft>(emptyDraft)
  const [applied, setApplied] = React.useState<AppliedLakeFilters>(() => ({ ...emptyDraft(), nonce: 0 }))
  const [lane, setLane] = React.useState<string | null>(null)
  const [domain, setDomain] = React.useState<string | null>(null)

  // The Denver operating day this whole tab is scoped to.
  const day = applied.to

  // The lane roster, kept here so the Lane pane can offer a picker without
  // refetching the queue. A failure is silent ONLY for the picker (the
  // Ecosystem pane surfaces the same fetch's error properly).
  const [lanes, setLanes] = React.useState<EcosystemLaneRow[]>([])
  React.useEffect(() => {
    const ctrl = new AbortController()
    supplyGet<EcosystemResponse>('/ecosystem', { day }, ctrl.signal)
      .then(d => setLanes(d.lanes ?? []))
      .catch(() => { if (!ctrl.signal.aborted) setLanes([]) })
    return () => ctrl.abort()
  }, [day, applied.nonce])

  const openLane = (l: string) => { setLane(l); setPane('lane') }

  return (
    <div style={pageStyle}>
      <div style={{ marginBottom: 14 }}>
        <h1 style={{ margin: 0, fontSize: 22, color: colors.heading }}>Supply</h1>
        <div style={{ fontSize: 12, color: colors.textMuted, marginTop: 4, maxWidth: 900 }}>
          The drip supply chain: contracts are policy, mediators decide, ledgers record, this screen projects. Every number
          below is labelled <em>contracted</em>, <em>effective</em>, <em>planned</em>, <em>reserved</em>, <em>actual</em> or{' '}
          <em>forecast</em> — hover any figure to see which, and what its denominator is. A missing measurement renders as{' '}
          <em>unknown</em>, never as 0.
        </div>
      </div>

      <FilterBar
        draft={draft}
        setDraft={setDraft}
        applied={applied}
        onRun={next => setApplied(a => ({ ...next, nonce: a.nonce + 1 }))}
        show={{}}
        activeLabel="Active (the supply chain is a single-day surface — the To day is the day):"
      />

      <div style={{ display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap', marginBottom: 12 }}>
        <SubNav
          items={PANES}
          active={pane}
          onChange={k => setPane(k as Pane)}
          ariaLabel="Supply panes"
        />
        {applied.from !== applied.to && (
          <span style={{ fontSize: 11, color: colors.warningText }}>
            A range was applied ({applied.from} → {applied.to}) but this tab reads ONE Denver day — showing {day}.
          </span>
        )}
        {lane && pane !== 'lane' && (
          <FilterChip label={`lane=${lane}`} onRemove={() => setLane(null)} />
        )}
        {domain && pane !== 'domains' && (
          <FilterChip label={`domain=${domain}`} onRemove={() => setDomain(null)} />
        )}
      </div>

      {/* Panes stay mounted once visited so their state and scroll survive a
          flip (PORTAL_DESIGN_SYSTEM §4). The Ecosystem pane is the only one
          that polls, so hiding it does not start a second poller elsewhere. */}
      <div style={{ display: pane === 'ecosystem' ? 'block' : 'none' }}>
        <SupplyEcosystem day={day} onSelectLane={openLane} />
      </div>
      <div style={{ display: pane === 'lane' ? 'block' : 'none' }}>
        <SupplyLane
          day={day}
          lane={lane}
          lanes={lanes}
          onSelectLane={setLane}
          onBack={() => setPane('ecosystem')}
        />
      </div>
      <div style={{ display: pane === 'domains' ? 'block' : 'none' }}>
        <SupplyDomains day={day} domain={domain} onSelectDomain={setDomain} />
      </div>
    </div>
  )
}

export default SupplyView
