import React from 'react'
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react'
import { describe, it, expect, beforeEach, vi } from 'vitest'

/**
 * CONTRACT tests: every pane renders the BACKEND's own responses. The fixtures
 * in __fixtures__/content-desk/ are byte-identical copies of
 * internal/contentdesk/testdata/contract/*.json, written from the real handlers
 * on real Postgres (cmd/server/content_desk_contract_integration_test.go) and
 * kept in sync by scripts/check-content-desk-contract.sh (+ a Go test). A
 * backend shape change regenerates them; these tests then fail wherever the
 * portal reads a field the backend no longer sends.
 */

const { addToast } = vi.hoisted(() => ({ addToast: vi.fn() }))
vi.mock('../shared/ToastSystem', () => ({
  useToast: () => ({ addToast, campaignComplete: vi.fn(), campaignAlert: vi.fn() }),
}))

import { ContentDeskArticle } from './ContentDeskArticle'
import { ContentDeskQueue } from './ContentDeskQueue'
import { ContentDeskOps } from './ContentDeskOps'
import { ContentDeskBrief } from './ContentDeskBrief'
import {
  BLOCK_TYPES, CATEGORIES,
  type ArticleDetail, type ArticleRow, type Brief, type Loadable, type OpsStatus, type Site,
} from './contentDeskShared'
import listFx from './__fixtures__/content-desk/list.json'
import detailFx from './__fixtures__/content-desk/detail.json'
import withdrawnFx from './__fixtures__/content-desk/detail_withdrawn.json'
import opsFx from './__fixtures__/content-desk/ops_status.json'
import sitesFx from './__fixtures__/content-desk/sites.json'
import briefsFx from './__fixtures__/content-desk/briefs.json'

const clone = <T,>(x: T): T => JSON.parse(JSON.stringify(x)) as T
const LIST = (listFx as unknown as { articles: ArticleRow[] }).articles
const D = detailFx as unknown as ArticleDetail
const W = withdrawnFx as unknown as ArticleDetail
const OPS = opsFx as unknown as OpsStatus
const SITES = (sitesFx as unknown as { sites: Site[] }).sites
const BRIEFS = (briefsFx as unknown as { briefs: Brief[] }).briefs

const resp = (status: number, body?: unknown) => ({
  ok: status >= 200 && status < 300,
  status,
  json: async () => body,
  text: async () => (body === undefined ? '' : JSON.stringify(body)),
})

interface Call { url: string; method: string; body: any }
let calls: Call[] = []
function install(route: (c: Call) => ReturnType<typeof resp>) {
  calls = []
  global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const c: Call = { url: String(url), method: init?.method ?? 'GET', body: init?.body ? JSON.parse(String(init.body)) : undefined }
    calls.push(c)
    return route(c) as unknown as Response
  }) as unknown as typeof fetch
}

const loaded = <T,>(data: T): Loadable<T> => ({ data, loading: false, error: null, stamp: null, reload: () => {} })
const CD = '/api/mailing/content-desk'

beforeEach(() => {
  addToast.mockReset()
  window.localStorage.clear()
})

describe('queue — GET articles', () => {
  it('renders every row with domain, status, consequential flag, open findings and revision', async () => {
    install(c => (c.url.startsWith(`${CD}/articles`) ? resp(200, clone(listFx)) : resp(404, { error: 'not in this test' })))
    render(<ContentDeskQueue sites={loaded(SITES)} onOpen={() => {}} />)
    for (const a of LIST) {
      const row = (await screen.findByText(a.slug)).closest('tr')!
      expect(within(row).getByText(a.domain)).toBeTruthy()
      expect(within(row).getByText(a.status)).toBeTruthy()
      expect(within(row).queryByText('mandatory human') != null).toBe(a.consequential)
      for (const s of ['S1', 'S2', 'S3'] as const) {
        expect(within(row).getByText(`${s} ${a.open_findings![s]}`)).toBeTruthy()
      }
      if (a.revision_hash) expect(within(row).getByTitle(a.revision_hash)).toBeTruthy()
    }
    expect(calls[0].url).toBe(`${CD}/articles`)
    expect(screen.queryByText('No articles yet')).toBeNull()
  })
})

describe('article view — GET articles/{id}', () => {
  const blocks = D.revision!.package!.blocks!
  const judgments = D.revision!.checks!.judgment!

  it('the golden exercises every backend block type', () => {
    expect(new Set(blocks.map(b => b.type))).toEqual(new Set(BLOCK_TYPES))
  })

  it('renders every block type with every field it carries', async () => {
    install(() => resp(200, clone(D)))
    render(<ContentDeskArticle articleId={D.article.id} onBack={() => {}} />)
    await screen.findByText(/Mandatory human review/i)
    for (const b of blocks) {
      const el = screen.getByTestId(`block-${b.id}`)
      expect(within(el).getByTestId('block-type').textContent).toBe(b.type)
      expect(el.textContent).not.toMatch(/not in the backend's allowed set|empty block/)
      for (const s of [b.heading, b.text, b.caption, ...(b.items ?? []), ...(b.rows ?? []).flat()]) {
        if (s) expect(el.textContent, `${b.id} ${b.type}`).toContain(s)
      }
      if (b.calc) {
        const calc = within(el).getByTestId('calc').textContent ?? ''
        expect(calc).toContain(`formula: ${b.calc.formula}`)
        expect(calc).toContain(`result: ${b.calc.result}`)
        b.calc.inputs.forEach(i => expect(calc).toContain(`${i.name} = ${i.value}`))
      }
    }
    const faq = blocks.find(b => b.type === 'faq')!
    const dl = screen.getByTestId('faq')
    expect(Array.from(dl.querySelectorAll('dt')).map(x => x.textContent)).toEqual(faq.items!.filter((_, i) => i % 2 === 0))
    expect(Array.from(dl.querySelectorAll('dd')).map(x => x.textContent)).toEqual(faq.items!.filter((_, i) => i % 2 === 1))
    const steps = blocks.find(b => b.type === 'steps')!
    expect(screen.getByTestId(`block-${steps.id}`).querySelectorAll('ol li')).toHaveLength(steps.items!.length)
    const table = blocks.find(b => b.type === 'comparison_table')!
    expect(screen.getByTestId(`block-${table.id}`).querySelectorAll('tr')).toHaveLength(table.rows!.length)
    expect(screen.getByTestId('stat-figure').textContent).toBe(blocks.find(b => b.type === 'stat')!.heading)
    // code checks: severity + details (not the old single `detail`)
    const failed = D.revision!.checks!.code!.find(c => !c.passed)!
    expect(screen.getByText(`fail ${failed.severity}`)).toBeTruthy()
    expect(screen.getByText(failed.details![0])).toBeTruthy()
  })

  it('places every judged sentence and pairs it with its source passage', async () => {
    install(() => resp(200, clone(D)))
    render(<ContentDeskArticle articleId={D.article.id} onBack={() => {}} />)
    await screen.findByText(/Mandatory human review/i)
    const marks = screen.getAllByTestId('judged-sentence')
    expect(marks).toHaveLength(new Set(judgments.map(j => `${j.block_id}#${j.sentence_idx}`)).size)
    expect(screen.queryByText(/not found verbatim/)).toBeNull()
    const bodyIds = new Set(blocks.map(b => b.id))
    for (const p of D.pairs!) {
      if (bodyIds.has(p.block_id)) expect(screen.getByTestId(`block-${p.block_id}`).textContent).toContain(p.sentence)
      else expect(screen.getByTestId('judged-outside-body').textContent).toContain(p.sentence)
    }

    const over = judgments.find(j => j.verdict === 'overstated')!
    fireEvent.click(marks.find(el => el.textContent === over.sentence)!)
    const claim = D.claims!.find(c => c.claim_id === over.claim_id)!
    expect(screen.getByText(claim.passage!)).toBeTruthy()
    expect(screen.getByText(new RegExp(`Lost qualifier: ${over.lost_qualifier}`))).toBeTruthy()
    expect(screen.getByText(claim.population!)).toBeTruthy()

    const flag = judgments.find(j => j.kind !== 'claim')!
    fireEvent.click(marks.find(el => el.textContent === flag.sentence)!)
    expect(screen.getByText(flag.kind)).toBeTruthy()
    expect(screen.getByText(flag.note!)).toBeTruthy()
  })

  it('approve sends reviewer, accepted_ids for every blocker and backend caught_by values', async () => {
    install(c => (c.method === 'GET' ? resp(200, clone(D)) : resp(201, { review: {}, outcome: { status: 'in_review', awaiting: 'second' } })))
    render(<ContentDeskArticle articleId={D.article.id} onBack={() => {}} />)
    await screen.findByTestId('accept-list')
    const want = [
      ...judgments.filter(j => !(j.kind === 'claim' && j.verdict === 'supported')).map(j => j.id),
      ...D.revision!.checks!.code!.filter(c => !c.passed && c.severity !== 'S1').map(c => `code:${c.name}`),
    ]
    expect(want.length).toBeGreaterThan(0)
    want.forEach(id => fireEvent.click(screen.getByLabelText(`Accept ${id}`)))

    fireEvent.click(screen.getByRole('button', { name: /Add finding/ }))
    const caught = screen.getByLabelText('Finding 1 caught by') as HTMLSelectElement
    expect(Array.from(caught.options).map(o => o.value).sort()).toEqual(['code', 'human', 'judgment'])
    fireEvent.change(caught, { target: { value: 'judgment' } })
    fireEvent.change(screen.getByLabelText('Finding 1 text'), { target: { value: 'qualifier dropped' } })
    fireEvent.change(screen.getByLabelText('Reviewer'), { target: { value: 'bo' } })
    fireEvent.change(screen.getByLabelText('Minutes spent'), { target: { value: '9' } })
    fireEvent.click(screen.getByRole('button', { name: 'Approve' }))

    await waitFor(() => expect(calls.some(c => c.method === 'POST')).toBe(true))
    const body = calls.find(c => c.method === 'POST')!.body
    expect(body).toMatchObject({ revision_hash: D.revision!.revision_hash, reviewer: 'bo', role: 'primary', decision: 'approve', minutes: 9 })
    expect([...body.accepted_ids].sort()).toEqual([...want].sort())
    expect(body.findings).toEqual([{ severity: 'S2', caught_by: 'judgment', block_id: '', text: 'qualifier dropped' }])
  })

  it('a 422 blocked approve shows the server blockers', async () => {
    install(c => (c.method === 'GET' ? resp(200, clone(D)) : resp(422, { error: 'approval blocked', blockers: ['judgment j2 (claim overstated at b10#0) is unresolved'] })))
    render(<ContentDeskArticle articleId={D.article.id} onBack={() => {}} />)
    await screen.findByTestId('accept-list')
    fireEvent.change(screen.getByLabelText('Reviewer'), { target: { value: 'bo' } })
    fireEvent.change(screen.getByLabelText('Minutes spent'), { target: { value: '3' } })
    fireEvent.click(screen.getByRole('button', { name: 'Approve' }))
    expect(await screen.findByText(/judgment j2 \(claim overstated at b10#0\) is unresolved/)).toBeTruthy()
  })

  it('a withdrawn, never-built article says so', async () => {
    install(() => resp(200, clone(W)))
    render(<ContentDeskArticle articleId={W.article.id} onBack={() => {}} />)
    expect(await screen.findByText(/Never built/)).toBeTruthy()
    expect(screen.getByText('withdrawn')).toBeTruthy()
  })
})

describe('ops dashboard — GET ops-status', () => {
  it('renders the backend snapshot: switch, spend, queue, pipeline pivot, releases, checks, supply', async () => {
    install(() => resp(200, clone(opsFx)))
    render(<ContentDeskOps />)
    await screen.findByText('Is everything working?')
    expect(screen.getByText('enabled')).toBeTruthy()
    expect(screen.getByText(/pipeline may run/)).toBeTruthy()
    expect(screen.getByText(/oldest waiting 1\.5h/)).toBeTruthy() // oldest_age_seconds 5400
    expect(screen.getByText('model-write')).toBeTruthy()

    const stages = screen.getAllByTestId('pipeline-stage')
    expect(stages.map(r => r.querySelector('td')!.textContent)).toEqual([...new Set(OPS.pipeline!.map(r => r.stage))])
    for (const r of OPS.pipeline!) {
      const row = stages.find(s => s.querySelector('td')!.textContent === r.stage)!
      expect(row.textContent).toContain(String(r.count))
    }

    const rels = screen.getAllByTestId('release-row')
    expect(rels).toHaveLength(OPS.releases!.length)
    const live = OPS.releases!.find(r => r.status)!
    expect(rels[0].textContent).toContain(live.domain)
    expect(rels[0].textContent).toContain(live.status)

    for (const c of OPS.checks!) expect(screen.getByText(c.name)).toBeTruthy()
    const consumers = screen.getAllByTestId('supply-consumer')
    expect(consumers).toHaveLength(OPS.supply.consumers!.length)
    expect(consumers[0].textContent).toContain('yahoo_family') // least runway first
  })
})

describe('ops dashboard — absent and null fields render "unknown", never 0 or a crash', () => {
  it('a snapshot carrying only checks', async () => {
    install(() => resp(200, { checks: [{ name: 'server: budget', status: 'ok', detail: 'fine', checked_at: null }] }))
    render(<ContentDeskOps />)
    await screen.findByText('Is everything working?')
    expect(screen.getByTitle('kill_switch not reported')).toBeTruthy()
    expect(screen.getByTitle('today_usd not reported')).toBeTruthy()
    expect(screen.getByTitle('nothing in review')).toBeTruthy()
    expect(screen.getByTitle('pipeline not reported')).toBeTruthy()
    expect(screen.getAllByTitle('releases not reported').length).toBeGreaterThan(0)
    expect(screen.getByText(/no supply_runway report received/)).toBeTruthy()
    expect(screen.queryByText('enabled')).toBeNull()
    expect(screen.queryByText('killed')).toBeNull()
    expect(screen.getByText('server: budget')).toBeTruthy()
  })

  it('a snapshot whose objects are null', async () => {
    const o = clone(opsFx) as unknown as Record<string, unknown>
    Object.assign(o, {
      kill_switch: null, spend: null, models: null, pipeline: null, releases: null, checks: null,
      last_pipeline_error: null, review_queue: { size: 0, oldest_age_seconds: null }, supply: { consumers: null },
    })
    install(() => resp(200, o))
    render(<ContentDeskOps />)
    await screen.findByText('Is everything working?')
    expect(screen.getByText('No checks reported — state unknown.')).toBeTruthy()
    expect(screen.getByTitle('kill_switch not reported')).toBeTruthy()
    expect(screen.getByTitle('nothing in review')).toBeTruthy()
    expect(screen.getAllByTitle('releases not reported').length).toBeGreaterThan(0)
    expect(screen.queryByTestId('last-pipeline-error')).toBeNull()
    expect(screen.getByText(/no supply_runway report received/)).toBeTruthy()
  })
})

describe('briefs — GET briefs / sites', () => {
  it('lists the backend briefs and offers only the backend categories', async () => {
    install(c => (c.url.startsWith(`${CD}/briefs`) ? resp(200, clone(briefsFx)) : resp(404, {})))
    render(<ContentDeskBrief sites={loaded(SITES)} onOpenArticle={() => {}} />)
    for (const b of BRIEFS) expect(await screen.findByText(b.reader_question)).toBeTruthy()
    expect(screen.getAllByRole('button', { name: 'Open' })).toHaveLength(BRIEFS.filter(b => b.article_id).length)
    const cat = screen.getByLabelText('Category') as HTMLSelectElement
    expect(Array.from(cat.options).map(o => o.value).filter(Boolean)).toEqual([...CATEGORIES])
  })
})
