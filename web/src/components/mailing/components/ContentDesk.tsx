// ContentDesk.tsx — the portal's Content Desk tab (tab id 'content-desk').
//
// PAGE_VERSION 1 (2026-09-11) — first cut over /api/mailing/content-desk/*,
// built against the API contract while the backend lands in parallel: every
// call may 404 until it is deployed, and each pane says so rather than blanking.
//
// SHAPE — in-motion → plan-ahead → history:
//   Review queue    the actionable surface: what waits on a human, oldest first
//   Article review  one article block by block, each judged sentence beside its
//                   source evidence; the review form binds to the revision read
//   Operations      "Is everything working?" — polls ops-status (30s)
//   New brief       plan-ahead: commission an article, then run the pipeline
//
// Panes mount on first visit and then stay mounted (PORTAL_DESIGN_SYSTEM §4),
// so a half-written review survives a flip to the queue and back.

import React from 'react'
import { colors, pageStyle } from '../shared/theme'
import { SubNav } from '../shared/SubNav'
import { useCdGet, type Site, type Loadable } from './contentDeskShared'
import { ContentDeskQueue } from './ContentDeskQueue'
import { ContentDeskArticle } from './ContentDeskArticle'
import { ContentDeskOps } from './ContentDeskOps'
import { ContentDeskBrief } from './ContentDeskBrief'

type Pane = 'queue' | 'article' | 'ops' | 'brief'

const PANES = [
  { key: 'queue', label: 'Review queue' },
  { key: 'article', label: 'Article review' },
  { key: 'ops', label: 'Operations' },
  { key: 'brief', label: 'New brief' },
]

export const ContentDesk: React.FC = () => {
  const [pane, setPane] = React.useState<Pane>('queue')
  const [visited, setVisited] = React.useState<Set<Pane>>(() => new Set<Pane>(['queue']))
  const [articleId, setArticleId] = React.useState<string | null>(null)
  const sitesRes = useCdGet<{ sites: Site[] | null }>('/sites')
  const sites: Loadable<Site[]> = { ...sitesRes, data: sitesRes.data?.sites ?? null }

  const go = (p: Pane) => {
    setPane(p)
    setVisited(v => (v.has(p) ? v : new Set(v).add(p)))
  }
  const openArticle = (id: string) => {
    setArticleId(id)
    go('article')
  }

  const show = (p: Pane) => ({ display: pane === p ? 'block' : 'none' })

  return (
    <div style={pageStyle}>
      <div style={{ marginBottom: 14 }}>
        <h1 style={{ margin: 0, fontSize: 22, color: colors.heading }}>Content Desk</h1>
        <div style={{ fontSize: 12, color: colors.textMuted, marginTop: 4, maxWidth: 900 }}>
          Articles move brief → pipeline → human review → release. The queue shows what is waiting on a person; an article
          opens block by block with every judged sentence next to its source passage. Nothing here renders raw HTML — any
          markup in a field is shown escaped and flagged.
        </div>
      </div>

      <SubNav items={PANES} active={pane} onChange={k => go(k as Pane)} ariaLabel="Content Desk panes" />

      <div style={show('queue')}>
        <ContentDeskQueue sites={sites} onOpen={openArticle} />
      </div>
      {visited.has('article') && (
        <div style={show('article')}>
          <ContentDeskArticle articleId={articleId} onBack={() => go('queue')} />
        </div>
      )}
      {visited.has('ops') && (
        <div style={show('ops')}>
          <ContentDeskOps />
        </div>
      )}
      {visited.has('brief') && (
        <div style={show('brief')}>
          <ContentDeskBrief sites={sites} onOpenArticle={openArticle} />
        </div>
      )}
    </div>
  )
}

export default ContentDesk
