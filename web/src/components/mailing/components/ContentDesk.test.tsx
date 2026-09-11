import React from 'react'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { describe, it, expect, beforeEach, vi } from 'vitest'

/**
 * Content Desk guards:
 *   1. review-form HASH HANDLING — the review carries the revision_hash of the
 *      revision on screen; a 409 toasts, reloads, and the NEXT submit carries the
 *      new hash (never the stale one, never a refetched-at-submit one);
 *   2. blind second review hides the primary review + verdicts until submit;
 *   3. NO RAW HTML — markup in any article field renders as literal text.
 */

const { addToast } = vi.hoisted(() => ({ addToast: vi.fn() }))
vi.mock('../shared/ToastSystem', () => ({
  useToast: () => ({ addToast, campaignComplete: vi.fn(), campaignAlert: vi.fn() }),
}))

import { ContentDeskArticle } from './ContentDeskArticle'
import {
  buildReviewPayload, submitReview, segmentPieces, groupJudgments, containsHtml,
  type ArticleDetail, type Judgment,
} from './contentDeskShared'

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

const J: Judgment = {
  block_id: 'b1', sentence_idx: 0, sentence: 'Rates fell in May.', verdict: 'overstated',
  lost_qualifier: 'for 30-year fixed', claim_id: 'c1', version: 1,
}

const detail = (hash: string): ArticleDetail => ({
  article: {
    id: 'a1', site_id: 's1', domain: 'discountblog.com', slug: 'rates', status: 'in_review', title: 'Rates',
    consequential: true, current_revision_hash: hash, updated_at: '2026-09-10T12:00:00Z', open_findings: { S1: 0, S2: 1, S3: 0 },
  },
  revision: {
    id: `r-${hash}`,
    revision_hash: hash,
    package: {
      title: 'Rates', excerpt: 'E', meta_title: 'MT', meta_description: 'MD',
      blocks: [{ id: 'b1', type: 'paragraph', text: 'Rates fell in May. Nothing else moved.' }],
      hero_image: null, subjects: ['s1'], preheaders: ['p1'],
    },
    checks: { code: [{ name: 'links', passed: true, detail: '' }], judgment: [J] },
    usage: null,
  },
  claims: [{
    claim_id: 'c1', version: 1, text: 'Rates fell', type: 'stat', source_url: 'https://example.gov/x',
    passage: 'The 30-year fixed rate fell in May.', context: 'ctx', published_at: '2026-06-01T00:00:00Z',
    effective_at: null, retrieved_at: '2026-09-01T00:00:00Z', jurisdiction: 'US', population: 'borrowers',
    conditions: null, status: 'active', derivation: 'direct',
  }],
  reviews: [{
    role: 'primary', decision: 'approve', revision_hash: hash, minutes: 12,
    findings: [{ severity: 'S3', caught_by: 'reviewer', block_id: 'b1', text: 'PRIMARY-REVIEW-NOTE' }],
  }],
})

beforeEach(() => {
  addToast.mockReset()
})

describe('buildReviewPayload', () => {
  it('carries the revision hash, trims, and drops blank findings', () => {
    const p = buildReviewPayload({
      revisionHash: ' h1 ', role: 'primary', decision: 'changes', minutes: '7',
      findings: [
        { severity: 'S1', caught_by: ' reviewer ', block_id: 'b1', text: ' wrong number ' },
        { severity: 'S3', caught_by: '', block_id: '', text: '   ' },
      ],
    })
    expect(p).toEqual({
      revision_hash: 'h1', role: 'primary', decision: 'changes', minutes: 7,
      findings: [{ severity: 'S1', caught_by: 'reviewer', block_id: 'b1', text: 'wrong number' }],
    })
  })

  it('refuses without a revision hash or with bad minutes', () => {
    const base = { role: 'primary' as const, decision: 'approve' as const, findings: [] }
    expect(() => buildReviewPayload({ ...base, revisionHash: null, minutes: 3 })).toThrow(/revision hash/)
    expect(() => buildReviewPayload({ ...base, revisionHash: '', minutes: 3 })).toThrow(/revision hash/)
    expect(() => buildReviewPayload({ ...base, revisionHash: 'h', minutes: '' })).toThrow(/Minutes/)
    expect(() => buildReviewPayload({ ...base, revisionHash: 'h', minutes: -1 })).toThrow(/Minutes/)
  })
})

describe('submitReview', () => {
  it('POSTs the payload (hash included) to the article reviews endpoint', async () => {
    install(() => resp(201, { ok: true }))
    const r = await submitReview('a/1', { revision_hash: 'h1', role: 'second', decision: 'approve', findings: [], minutes: 4 })
    expect(r).toEqual({ kind: 'ok' })
    expect(calls[0].url).toBe('/api/mailing/content-desk/articles/a%2F1/reviews')
    expect(calls[0].method).toBe('POST')
    expect(calls[0].body.revision_hash).toBe('h1')
  })

  it('maps 409 to a conflict and other failures to an error', async () => {
    install(() => resp(409, { error: 'revision changed' }))
    const c = await submitReview('a1', { revision_hash: 'old', role: 'primary', decision: 'approve', findings: [], minutes: 1 })
    expect(c.kind).toBe('conflict')
    install(() => resp(500, { error: 'boom' }))
    const e = await submitReview('a1', { revision_hash: 'h', role: 'primary', decision: 'approve', findings: [], minutes: 1 })
    expect(e).toMatchObject({ kind: 'error', status: 500 })
  })
})

describe('sentence placement', () => {
  it('places judged sentences verbatim, in order, and reports the ones it cannot find', () => {
    const groups = groupJudgments([
      { ...J, sentence_idx: 0, sentence: 'A b.' },
      { ...J, sentence_idx: 1, sentence: 'Not here.' },
      { ...J, sentence_idx: 2, sentence: 'C d.' },
    ])
    const { pieces, unplaced } = segmentPieces(['A b. C d. E.'], groups)
    expect(pieces[0].map(s => [s.text, s.group?.sentenceIdx ?? null])).toEqual([
      ['A b.', 0], [' ', null], ['C d.', 2], [' E.', null],
    ])
    expect(unplaced.map(g => g.sentenceIdx)).toEqual([1])
  })

  it('detects markup', () => {
    expect(containsHtml('<b>x</b>')).toBe(true)
    expect(containsHtml('a &amp; b')).toBe(true)
    expect(containsHtml('3 < 4 and 5 > 2')).toBe(false)
  })
})

describe('ContentDeskArticle — review form hash handling', () => {
  it('sends the displayed revision_hash; on 409 toasts + reloads, and the next submit carries the NEW hash', async () => {
    let gets = 0
    let posts = 0
    install(c => {
      if (c.method === 'GET') { gets += 1; return resp(200, detail(gets === 1 ? 'hash-one' : 'hash-two')) }
      posts += 1
      return posts === 1 ? resp(409, { error: 'revision changed' }) : resp(201, { ok: true })
    })
    render(<ContentDeskArticle articleId="a1" onBack={() => {}} />)
    await screen.findByText(/Mandatory human review/i)

    fireEvent.change(screen.getByLabelText('Minutes spent'), { target: { value: '7' } })
    fireEvent.click(screen.getByRole('button', { name: 'Approve' }))

    await waitFor(() => expect(posts).toBe(1))
    const first = calls.filter(c => c.method === 'POST')[0]
    expect(first.url).toBe('/api/mailing/content-desk/articles/a1/reviews')
    expect(first.body).toMatchObject({ revision_hash: 'hash-one', role: 'primary', decision: 'approve', minutes: 7 })

    await waitFor(() => expect(addToast).toHaveBeenCalledWith(expect.objectContaining({ type: 'warning', title: expect.stringMatching(/Revision changed/) })))
    await waitFor(() => expect(gets).toBe(2))
    await waitFor(() => expect(screen.getAllByTitle('hash-two').length).toBeGreaterThan(0))

    // Minutes were kept across the conflict; resubmit binds to the new revision.
    fireEvent.click(screen.getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(posts).toBe(2))
    expect(calls.filter(c => c.method === 'POST')[1].body.revision_hash).toBe('hash-two')
    await waitFor(() => expect(addToast).toHaveBeenCalledWith(expect.objectContaining({ type: 'success' })))
  })

  it('blind second review hides the primary review and verdicts until it submits', async () => {
    install(c => (c.method === 'GET' ? resp(200, detail('h1')) : resp(201, {})))
    render(<ContentDeskArticle articleId="a1" onBack={() => {}} />)
    await screen.findByText('PRIMARY-REVIEW-NOTE')
    expect(screen.getAllByTestId('verdict-chip').length).toBeGreaterThan(0)

    fireEvent.click(screen.getByRole('tab', { name: 'Second reviewer (blind)' }))
    expect(screen.queryByText('PRIMARY-REVIEW-NOTE')).toBeNull()
    expect(screen.queryAllByTestId('verdict-chip')).toHaveLength(0)

    fireEvent.click(screen.getByTestId('judged-sentence'))
    expect(screen.getByText('The 30-year fixed rate fell in May.')).toBeTruthy() // evidence stays visible
    expect(screen.queryByText(/Lost qualifier/)).toBeNull() // …but the qualifier (implies verdict) does not
    expect(screen.queryAllByTestId('verdict-chip')).toHaveLength(0)

    fireEvent.change(screen.getByLabelText('Minutes spent'), { target: { value: '5' } })
    fireEvent.click(screen.getByRole('button', { name: 'Reject' }))
    await screen.findByText('PRIMARY-REVIEW-NOTE')
    const post = calls.find(c => c.method === 'POST')!
    expect(post.body).toMatchObject({ role: 'second', decision: 'reject', revision_hash: 'h1' })
    expect(screen.getAllByTestId('verdict-chip').length).toBeGreaterThan(0)
  })
})

describe('ContentDeskArticle — renders no raw HTML', () => {
  it('shows markup in blocks and package fields as escaped literal text', async () => {
    const d = detail('h1')
    d.revision!.package!.title = '<b>Bold title</b>'
    d.revision!.package!.excerpt = 'Tom &amp; Jerry'
    d.revision!.package!.blocks = [
      { id: 'b1', type: 'paragraph', text: 'Hi <script>window.__pwned=1</script><img src=x onerror="window.__pwned=1"><em>there</em>' },
      { id: 'b2', type: 'list', items: ['<a href="javascript:alert(1)">click</a>'] },
      { id: 'b3', type: 'table', rows: [['<i>h</i>', 'v'], ['<u>x</u>', 'y']] },
    ]
    d.revision!.checks!.judgment = []
    d.article.consequential = false
    install(() => resp(200, d))
    const { container } = render(<ContentDeskArticle articleId="a1" onBack={() => {}} />)
    await screen.findAllByText('<b>Bold title</b>')

    for (const tag of ['script', 'b', 'em', 'i', 'u', 'img', 'a[href^="javascript"]']) {
      expect(container.querySelector(tag), tag).toBeNull()
    }
    expect(container.textContent).toContain('<script>window.__pwned=1</script>')
    expect(container.textContent).toContain('<a href="javascript:alert(1)">click</a>')
    expect(container.textContent).toContain('Tom &amp; Jerry')
    expect(screen.getAllByTestId('html-escaped-marker').length).toBeGreaterThanOrEqual(4)
    expect((window as unknown as { __pwned?: number }).__pwned).toBeUndefined()
  })
})
