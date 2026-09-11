// ContentDeskQueue.tsx — Pane 1 of the Content Desk: the review queue.
//
// The actionable surface. Oldest-waiting first by default, because the oldest
// item is the one about to go stale. Filtering composes the ONE shared FilterBar
// (status / site / consequential ride in its extraFields slot, their chips in
// its chip row). The bar hides its date controls here: a state-based queue with
// a date window would silently drop the oldest waiting article.
//
// Binding: `site` is also sent to the API, but EVERY filter is re-applied
// client-side on the rows returned — a filter the backend ignores still binds
// (PORTAL_DESIGN_SYSTEM §3.5). Status options come from the statuses actually
// present; this screen never invents the backend's vocabulary.

import React from 'react'
import { faInbox, faListCheck, faRotate } from '@fortawesome/free-solid-svg-icons'
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome'
import { colors, alpha, btnStyle, stateColor } from '../shared/theme'
import { Panel, SectionHeader, SectionError, EmptyState, Pill, Stat, LivePill, PortalKeyframes } from '../shared/ui'
import { usePolling } from '../shared/usePolling'
import {
  FilterBar, filterFieldLabelStyle, filterInputStyle, denverToday,
  type LakeFilterDraft, type AppliedLakeFilters,
} from '../shared/filters'
import { ScrollX, LoadingRow, Unknown, fmtTime, tableStyle, thStyle, tdStyle, numTd, numTh } from './supplyShared'
import {
  cdGet, FetchNote, FindingChips, Hash, SafeText, asText, hoursSince, fmtHours,
  type ArticleRow, type Site, type Loadable, type FetchStamp,
} from './contentDeskShared'

interface QueueFilters {
  status: string
  siteId: string
  consequential: '' | 'yes' | 'no'
}

const EMPTY_Q: QueueFilters = { status: '', siteId: '', consequential: '' }

// The shared bar's own draft. Only the Run button + chip row are live here
// (show={{}} + hideDates) — it carries no fields of its own on this screen.
const lakeDraft = (): LakeFilterDraft => ({
  from: denverToday(), to: denverToday(), ispGroup: '', brand: '', routeType: '', transport: 'combined',
})

type SortKey = 'age' | 'title' | 'domain' | 'status' | 'S1'

const POLL_MS = 60_000

export const ContentDeskQueue: React.FC<{
  sites: Loadable<Site[]>
  onOpen: (id: string) => void
}> = ({ sites, onOpen }) => {
  const [lake, setLake] = React.useState<LakeFilterDraft>(lakeDraft)
  const [lakeApplied, setLakeApplied] = React.useState<AppliedLakeFilters>(() => ({ ...lakeDraft(), nonce: 0 }))
  const [draft, setDraft] = React.useState<QueueFilters>(EMPTY_Q)
  const [applied, setApplied] = React.useState<QueueFilters>(EMPTY_Q)
  const [sort, setSort] = React.useState<{ key: SortKey; dir: 1 | -1 }>({ key: 'age', dir: -1 })

  const apply = (nextLake: LakeFilterDraft, nextQ: QueueFilters) => {
    setLakeApplied(a => ({ ...nextLake, nonce: a.nonce + 1 }))
    setDraft(nextQ)
    setApplied(nextQ)
  }

  const state = usePolling<{ rows: ArticleRow[]; stamp: FetchStamp }>(
    React.useCallback(
      async (signal: AbortSignal) => {
        const t0 = performance.now()
        const rows = await cdGet<ArticleRow[] | null>('/articles', { site: applied.siteId || undefined }, signal)
        return { rows: Array.isArray(rows) ? rows : [], stamp: { at: Date.now(), ms: Math.round(performance.now() - t0) } }
      },
      // eslint-disable-next-line react-hooks/exhaustive-deps
      [applied.siteId, lakeApplied.nonce],
    ),
    POLL_MS,
    [applied.siteId, lakeApplied.nonce],
  )

  const all = state.data?.rows ?? []

  // Site options: the configured sites; if /sites failed, the sites seen in rows.
  const siteOptions: Array<{ id: string; domain: string; enabled: boolean }> = React.useMemo(() => {
    if (sites.data && sites.data.length) return sites.data.map(s => ({ id: s.id, domain: s.domain, enabled: s.enabled }))
    const seen = new Map<string, string>()
    all.forEach(r => { if (r.site_id && !seen.has(r.site_id)) seen.set(r.site_id, r.domain) })
    return Array.from(seen.entries()).map(([id, domain]) => ({ id, domain, enabled: true }))
  }, [sites.data, all])
  const siteName = (id: string) => siteOptions.find(s => s.id === id)?.domain ?? id

  const statuses = React.useMemo(() => {
    const set = new Set(all.map(r => r.status).filter(Boolean))
    if (draft.status) set.add(draft.status)
    return Array.from(set).sort()
  }, [all, draft.status])

  const rows = React.useMemo(() => {
    const now = Date.now()
    const filtered = all.filter(r =>
      (!applied.siteId || r.site_id === applied.siteId) &&
      (!applied.status || r.status === applied.status) &&
      (applied.consequential === '' || (applied.consequential === 'yes') === Boolean(r.consequential)),
    )
    const val = (r: ArticleRow): string | number | null => {
      switch (sort.key) {
        case 'age': return hoursSince(r.updated_at, now)
        case 'title': return asText(r.title).toLowerCase()
        case 'domain': return (r.domain || '').toLowerCase()
        case 'status': return r.status || ''
        case 'S1': return r.open_findings?.S1 ?? null
      }
    }
    return [...filtered].sort((a, b) => {
      const x = val(a)
      const y = val(b)
      if (x == null && y == null) return 0
      if (x == null) return 1 // unknowns sort last in either direction
      if (y == null) return -1
      return (x < y ? -1 : x > y ? 1 : 0) * sort.dir
    })
  }, [all, applied, sort])

  const sum = (s: 'S1' | 'S2' | 'S3') => {
    let t = 0
    let n = 0
    rows.forEach(r => { const v = r.open_findings?.[s]; if (v != null) { t += v; n += 1 } })
    return n > 0 ? t : null
  }
  const oldest = rows.reduce<number | null>((m, r) => {
    const h = hoursSince(r.updated_at)
    return h == null ? m : m == null || h > m ? h : m
  }, null)
  const consequentialCount = rows.filter(r => r.consequential).length

  const chips: Array<{ label: string; tone?: string; onRemove?: () => void }> = []
  if (applied.status) chips.push({ label: `status=${applied.status}`, tone: colors.indigo300, onRemove: () => apply(lake, { ...applied, status: '' }) })
  if (applied.siteId) chips.push({ label: `site=${siteName(applied.siteId)}`, tone: colors.indigo200, onRemove: () => apply(lake, { ...applied, siteId: '' }) })
  if (applied.consequential) chips.push({
    label: `consequential=${applied.consequential}`, tone: colors.warning,
    onRemove: () => apply(lake, { ...applied, consequential: '' }),
  })

  const extraFields = (
    <>
      <label style={filterFieldLabelStyle}>status
        <select value={draft.status} onChange={e => setDraft(d => ({ ...d, status: e.target.value }))} style={{ ...filterInputStyle, width: 160 }}>
          <option value="">all</option>
          {statuses.map(s => <option key={s} value={s}>{s}</option>)}
        </select>
      </label>
      <label style={filterFieldLabelStyle}>site
        <select value={draft.siteId} onChange={e => setDraft(d => ({ ...d, siteId: e.target.value }))} style={{ ...filterInputStyle, width: 200 }}>
          <option value="">all</option>
          {siteOptions.map(s => <option key={s.id} value={s.id}>{s.domain}{s.enabled ? '' : ' (disabled)'}</option>)}
        </select>
      </label>
      <label style={filterFieldLabelStyle}>consequential
        <select
          value={draft.consequential}
          onChange={e => setDraft(d => ({ ...d, consequential: e.target.value as QueueFilters['consequential'] }))}
          style={{ ...filterInputStyle, width: 190 }}
        >
          <option value="">all</option>
          <option value="yes">yes — mandatory human review</option>
          <option value="no">no</option>
        </select>
      </label>
    </>
  )

  const SortTh: React.FC<{ k: SortKey; label: string; num?: boolean; title?: string }> = ({ k, label, num, title }) => (
    <th
      style={{ ...(num ? numTh : thStyle), cursor: 'pointer', whiteSpace: 'nowrap' }}
      title={title}
      onClick={() => setSort(s => ({ key: k, dir: s.key === k ? (s.dir === 1 ? -1 : 1) : k === 'age' || k === 'S1' ? -1 : 1 }))}
    >
      {label}{sort.key === k ? (sort.dir === 1 ? ' ▲' : ' ▼') : ''}
    </th>
  )

  return (
    <div>
      <PortalKeyframes />
      <FilterBar
        draft={lake}
        setDraft={setLake}
        applied={lakeApplied}
        onRun={next => apply(next, draft)}
        show={{}}
        hideDates
        extraFields={extraFields}
        extraChips={chips}
        activeLabel="Active (state-based queue — no date window, so the oldest waiting article is never filtered out):"
      />
      {sites.error && (
        <div style={{ marginBottom: 10 }}>
          <SectionError label="Site list" error={`${sites.error} — site filter falls back to sites seen in the queue`} onRetry={sites.reload} />
        </div>
      )}

      <Panel>
        <SectionHeader
          title="Review queue"
          icon={faListCheck}
          right={
            <span style={{ display: 'inline-flex', alignItems: 'center', gap: 10 }}>
              <LivePill live={state.live} agoSeconds={state.secondsSinceUpdate} />
              <FetchNote stamp={state.data?.stamp} />
              <button type="button" style={{ ...btnStyle, padding: '3px 9px', fontSize: 11 }} onClick={state.refresh}>
                <FontAwesomeIcon icon={faRotate} /> Refresh
              </button>
            </span>
          }
        />

        {state.loading && !state.data ? (
          <LoadingRow what="the review queue" />
        ) : !state.data ? (
          <SectionError label="Review queue" error={state.error ?? 'no data'} onRetry={state.refresh} />
        ) : (
          <>
            {state.error && (
              <div style={{ marginBottom: 10 }}>
                <SectionError label="Queue refresh (showing the last good list)" error={state.error} onRetry={state.refresh} />
              </div>
            )}
            <div style={{ display: 'flex', gap: 28, flexWrap: 'wrap', marginBottom: 12 }}>
              <Stat label="Articles shown" value={rows.length} sub={`of ${all.length} loaded`} />
              <Stat
                label="Consequential (shown)"
                value={consequentialCount}
                sub="mandatory human review"
                color={consequentialCount > 0 ? colors.warningText : undefined}
              />
              <Stat
                label="Open S1 findings (shown)"
                value={sum('S1') ?? <Unknown hint="no shown article reported S1 counts" />}
                color={(sum('S1') ?? 0) > 0 ? colors.dangerText : undefined}
                title="Sum of open S1 findings over the articles shown; articles with no count reported are excluded."
              />
              <Stat
                label="Oldest since last update"
                value={oldest == null ? <Unknown hint="no shown article carries updated_at" /> : fmtHours(oldest)}
                title="Age = now − updated_at of the article (the API reports no queue-entry time)."
              />
            </div>

            {all.length === 0 ? (
              <EmptyState icon={faInbox} title="No articles yet" hint="Nothing has been commissioned — create one in New brief." />
            ) : rows.length === 0 ? (
              <EmptyState icon={faInbox} title="No articles match the active filters" hint={`${all.length} loaded — remove a chip to widen.`} />
            ) : (
              <ScrollX>
                <table style={tableStyle}>
                  <thead>
                    <tr>
                      <SortTh k="title" label="Article" />
                      <SortTh k="domain" label="Site" />
                      <SortTh k="status" label="Status" />
                      <th style={thStyle}>Review</th>
                      <SortTh k="S1" label="Open findings" title="Open S1 / S2 / S3 findings; sorts by S1" />
                      <SortTh k="age" label="Age (since updated_at)" num title="now − updated_at; the API reports no queue-entry time" />
                      <th style={thStyle}>Revision</th>
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map(r => {
                      const h = hoursSince(r.updated_at)
                      return (
                        <tr
                          key={r.id}
                          onClick={() => onOpen(r.id)}
                          style={{ cursor: 'pointer' }}
                          onMouseEnter={e => { e.currentTarget.style.background = colors.hover }}
                          onMouseLeave={e => { e.currentTarget.style.background = 'transparent' }}
                        >
                          <td style={{ ...tdStyle, maxWidth: 420 }}>
                            <div style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', color: colors.heading }} title={asText(r.title)}>
                              <SafeText value={r.title} />
                            </div>
                            <div style={{ fontSize: 11, color: colors.textFaint }} title={r.slug}>{r.slug}</div>
                          </td>
                          <td style={tdStyle}>{r.domain || <Unknown />}</td>
                          <td style={tdStyle}>
                            {r.status ? <Pill color={stateColor(r.status)} style={{ fontSize: 10, padding: '1px 8px' }}>{r.status}</Pill> : <Unknown />}
                          </td>
                          <td style={tdStyle}>
                            {r.consequential
                              ? <Pill color={colors.warning} style={{ fontSize: 10, padding: '1px 8px' }}>mandatory human</Pill>
                              : <span style={{ fontSize: 11, color: colors.textFaint }}>standard</span>}
                          </td>
                          <td style={tdStyle}><FindingChips counts={r.open_findings} /></td>
                          <td style={numTd} title={r.updated_at ? `updated ${fmtTime(r.updated_at)} MT` : undefined}>
                            {h == null ? <Unknown hint="no updated_at" /> : fmtHours(h)}
                          </td>
                          <td style={tdStyle}><Hash value={r.current_revision_hash} /></td>
                        </tr>
                      )
                    })}
                  </tbody>
                  <tfoot>
                    <tr style={{ background: alpha(colors.indigo500, '0d') }}>
                      <td style={{ ...tdStyle, fontWeight: 700 }} colSpan={4}>Total · {rows.length} article{rows.length === 1 ? '' : 's'}</td>
                      <td style={tdStyle}><FindingChips counts={{ S1: sum('S1'), S2: sum('S2'), S3: sum('S3') }} /></td>
                      <td style={numTd}>{oldest == null ? '' : `oldest ${fmtHours(oldest)}`}</td>
                      <td style={tdStyle} />
                    </tr>
                  </tfoot>
                </table>
              </ScrollX>
            )}
          </>
        )}
      </Panel>
    </div>
  )
}

export default ContentDeskQueue
