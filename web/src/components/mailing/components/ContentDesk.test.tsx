import React from 'react'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { describe, it, expect, beforeEach, vi } from 'vitest'

/**
 * Content Desk guards (on the BACKEND's golden detail response,
 * __fixtures__/content-desk/detail.json — see ContentDeskContract.test.tsx):
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
import detailFx from './__fixtures__/content-desk/detail.json'

const clone = <T,>(x: T): T => JSON.parse(JSON.stringify(x)) as T
const D = detailFx as unknown as ArticleDetail
const ART = D.article.id

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
  id: 'j1', kind: 'claim', block_id: 'b1', sentence_idx: 0, sentence: 'Rates fell in May.', verdict: 'overstated',
  lost_qualifier: 'for 30-year fixed', claim_id: 'c1', version: 1,
}

/** The golden detail with its revision hash swapped (hash-move scenarios). */
const detail = (hash: string): ArticleDetail => {
  const d = clone(D)
  d.revision!.revision_hash = hash
  d.article.revision_hash = hash
  d.reviews = (d.reviews ?? []).map(r => ({ ...r, revision_hash: hash }))
  return d
}

const overstated = () => D.revision!.checks!.judgment!.find(j => j.verdict === 'overstated')!
const primaryNote = () => D.reviews![0].findings![0].text

beforeEach(() => {
  addToast.mockReset()
  window.localStorage.clear()
})

describe('buildReviewPayload', () => {
  it('carries the revision hash, reviewer and accepted ids; trims; drops blank findings', () => {
    const p = buildReviewPayload({
      revisionHash: ' h1 ', reviewer: ' ann ', role: 'primary', decision: 'changes', minutes: '7',
      acceptedIds: ['j2', 'code:near_duplicate', 'j2'],
      findings: [
        { severity: 'S1', caught_by: 'human', block_id: 'b1', text: ' wrong number ' },
        { severity: 'S3', caught_by: 'judgment', block_id: '', text: '   ' },
      ],
    })
    expect(p).toEqual({
      revision_hash: 'h1', reviewer: 'ann', role: 'primary', decision: 'changes', minutes: 7,
      accepted_ids: ['j2', 'code:near_duplicate'],
      findings: [{ severity: 'S1', caught_by: 'human', block_id: 'b1', text: 'wrong number' }],
    })
  })

  it('refuses without a revision hash, a reviewer, or with bad minutes', () => {
    const base = { role: 'primary' as const, decision: 'approve' as const, findings: [], reviewer: 'ann' }
    expect(() => buildReviewPayload({ ...base, revisionHash: null, minutes: 3 })).toThrow(/revision hash/)
    expect(() => buildReviewPayload({ ...base, revisionHash: '', minutes: 3 })).toThrow(/revision hash/)
    expect(() => buildReviewPayload({ ...base, revisionHash: 'h', reviewer: ' ', minutes: 3 })).toThrow(/Reviewer/)
    expect(() => buildReviewPayload({ ...base, revisionHash: 'h', minutes: '' })).toThrow(/Minutes/)
    expect(() => buildReviewPayload({ ...base, revisionHash: 'h', minutes: -1 })).toThrow(/Minutes/)
  })
})

describe('submitReview', () => {
  const payload = { revision_hash: 'h1', reviewer: 'ann', role: 'second' as const, decision: 'approve' as const, findings: [], minutes: 4, accepted_ids: [] }

  it('POSTs the payload (hash included) to the article reviews endpoint', async () => {
    install(() => resp(201, { review: {}, outcome: { status: 'approved' } }))
    const r = await submitReview('a/1', payload)
    expect(r).toEqual({ kind: 'ok' })
    expect(calls[0].url).toBe('/api/mailing/content-desk/articles/a%2F1/reviews')
    expect(calls[0].method).toBe('POST')
    expect(calls[0].body.revision_hash).toBe('h1')
  })

  it('maps 409 to a conflict, and a 422 error carries the server blockers', async () => {
    install(() => resp(409, { error: 'revision_hash does not match' }))
    expect((await submitReview('a1', payload)).kind).toBe('conflict')
    install(() => resp(422, { error: 'approval blocked', blockers: ['judgment j2 (claim overstated at b10#0) is unresolved'] }))
    const e = await submitReview('a1', payload)
    expect(e).toMatchObject({ kind: 'error', status: 422 })
    expect(e.kind === 'error' && e.message).toMatch(/judgment j2 .* is unresolved/)
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
    const h1 = 'a'.repeat(64)
    const h2 = 'b'.repeat(64)
    let gets = 0
    let posts = 0
    install(c => {
      if (c.method === 'GET') { gets += 1; return resp(200, detail(gets === 1 ? h1 : h2)) }
      posts += 1
      return posts === 1 ? resp(409, { error: 'revision changed' }) : resp(201, { review: {}, outcome: { status: 'in_review' } })
    })
    render(<ContentDeskArticle articleId={ART} onBack={() => {}} />)
    await screen.findByText(/Mandatory human review/i)

    fireEvent.change(screen.getByLabelText('Reviewer'), { target: { value: 'ann' } })
    fireEvent.change(screen.getByLabelText('Minutes spent'), { target: { value: '7' } })
    fireEvent.click(screen.getByRole('button', { name: 'Approve' }))

    await waitFor(() => expect(posts).toBe(1))
    const first = calls.filter(c => c.method === 'POST')[0]
    expect(first.url).toBe(`/api/mailing/content-desk/articles/${ART}/reviews`)
    expect(first.body).toMatchObject({ revision_hash: h1, reviewer: 'ann', role: 'primary', decision: 'approve', minutes: 7 })

    await waitFor(() => expect(addToast).toHaveBeenCalledWith(expect.objectContaining({ type: 'warning', title: expect.stringMatching(/Revision changed/) })))
    await waitFor(() => expect(gets).toBe(2))
    await waitFor(() => expect(screen.getAllByTitle(h2).length).toBeGreaterThan(0))

    // Minutes were kept across the conflict; resubmit binds to the new revision.
    fireEvent.click(screen.getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(posts).toBe(2))
    expect(calls.filter(c => c.method === 'POST')[1].body.revision_hash).toBe(h2)
    await waitFor(() => expect(addToast).toHaveBeenCalledWith(expect.objectContaining({ type: 'success' })))
  })

  it('blind second review hides the primary review and verdicts until it submits', async () => {
    install(c => (c.method === 'GET' ? resp(200, clone(D)) : resp(201, { review: {}, outcome: { status: 'in_review' } })))
    render(<ContentDeskArticle articleId={ART} onBack={() => {}} />)
    await screen.findByText(primaryNote())
    expect(screen.getAllByTestId('verdict-chip').length).toBeGreaterThan(0)

    fireEvent.click(screen.getByRole('tab', { name: 'Second reviewer (blind)' }))
    expect(screen.queryByText(primaryNote())).toBeNull()
    expect(screen.queryAllByTestId('verdict-chip')).toHaveLength(0)

    const over = overstated()
    fireEvent.click(screen.getAllByTestId('judged-sentence').find(el => el.textContent === over.sentence)!)
    const passage = D.claims!.find(c => c.claim_id === over.claim_id)!.passage!
    expect(screen.getByText(passage)).toBeTruthy() // evidence stays visible
    expect(screen.queryByText(/Lost qualifier/)).toBeNull() // …but the qualifier (implies verdict) does not
    expect(screen.queryAllByTestId('verdict-chip')).toHaveLength(0)
    expect(screen.getByTestId('accept-list').textContent).not.toMatch(/overstated|flag low_usefulness/)

    fireEvent.change(screen.getByLabelText('Reviewer'), { target: { value: 'bo' } })
    fireEvent.change(screen.getByLabelText('Minutes spent'), { target: { value: '5' } })
    fireEvent.click(screen.getByRole('button', { name: 'Reject' }))
    await screen.findByText(primaryNote())
    const post = calls.find(c => c.method === 'POST')!
    expect(post.body).toMatchObject({ role: 'second', decision: 'reject', reviewer: 'bo', revision_hash: D.revision!.revision_hash })
    expect(screen.getAllByTestId('verdict-chip').length).toBeGreaterThan(0)
  })
})

describe('ContentDeskArticle — renders no raw HTML', () => {
  it('shows markup in blocks and package fields as escaped literal text', async () => {
    const d = clone(D)
    d.revision!.package!.title = '<b>Bold title</b>'
    d.revision!.package!.excerpt = 'Tom &amp; Jerry'
    d.revision!.package!.blocks = [
      { id: 'b1', type: 'lede', text: 'Hi <script>window.__pwned=1</script><img src=x onerror="window.__pwned=1"><em>there</em>' },
      { id: 'b2', type: 'key_takeaways', items: ['<a href="javascript:alert(1)">click</a>'] },
      { id: 'b3', type: 'comparison_table', rows: [['<i>h</i>', 'v'], ['<u>x</u>', 'y']], caption: '<strong>cap</strong>' },
    ]
    d.revision!.checks!.judgment = []
    d.pairs = []
    d.article.consequential = false
    install(() => resp(200, d))
    const { container } = render(<ContentDeskArticle articleId={ART} onBack={() => {}} />)
    await screen.findAllByText('<b>Bold title</b>')

    for (const tag of ['script', 'b', 'em', 'i', 'u', 'strong', 'img', 'a[href^="javascript"]']) {
      expect(container.querySelector(tag), tag).toBeNull()
    }
    expect(container.textContent).toContain('<script>window.__pwned=1</script>')
    expect(container.textContent).toContain('<a href="javascript:alert(1)">click</a>')
    expect(container.textContent).toContain('Tom &amp; Jerry')
    expect(screen.getAllByTestId('html-escaped-marker').length).toBeGreaterThanOrEqual(4)
    expect((window as unknown as { __pwned?: number }).__pwned).toBeUndefined()
  })
})
