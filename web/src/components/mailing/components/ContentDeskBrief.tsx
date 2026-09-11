// ContentDeskBrief.tsx — Pane 4 of the Content Desk: commission an article.
//
// New brief (site, reader question, format, category, optional angle) → POST
// briefs. When the created brief names its article, "Run pipeline" is offered
// right there (POST articles/{id}/run). Recent briefs list below — each with
// Open / Run when it carries an article id.

import React from 'react'
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome'
import { faPenToSquare, faPlay, faFolderOpen, faRotate, faClockRotateLeft } from '@fortawesome/free-solid-svg-icons'
import { colors, btnStyle, stateColor } from '../shared/theme'
import { Panel, SectionHeader, SectionError, EmptyState, Pill } from '../shared/ui'
import { useToast } from '../shared/ToastSystem'
import { filterFieldLabelStyle, filterInputStyle } from '../shared/filters'
import { ScrollX, LoadingRow, Unknown, fmtTime, tableStyle, thStyle, tdStyle } from './supplyShared'
import {
  useCdGet, cdPost, runPipeline, errMsg, SafeText, asText, FetchNote,
  type Site, type Brief, type BriefInput, type Loadable,
} from './contentDeskShared'

const briefArticleId = (b: Brief | null | undefined): string | null => {
  if (!b) return null
  if (typeof b.article_id === 'string' && b.article_id) return b.article_id
  if (b.article && typeof b.article.id === 'string' && b.article.id) return b.article.id
  return null
}

const EMPTY: BriefInput = { site_id: '', reader_question: '', format: '', category: '', angle: '' }

export const ContentDeskBrief: React.FC<{
  sites: Loadable<Site[]>
  onOpenArticle: (id: string) => void
}> = ({ sites, onOpenArticle }) => {
  const toast = useToast()
  const briefs = useCdGet<Brief[]>('/briefs')
  const [form, setForm] = React.useState<BriefInput>(EMPTY)
  const [submitting, setSubmitting] = React.useState(false)
  const [error, setError] = React.useState<string | null>(null)
  const [created, setCreated] = React.useState<Brief | null>(null)
  const [running, setRunning] = React.useState<string | null>(null)

  const siteList = sites.data ?? []
  const siteName = (id: string | null | undefined) => (id ? siteList.find(s => s.id === id)?.domain ?? id : '')

  const missing = [
    !form.site_id && 'site',
    !form.reader_question.trim() && 'reader question',
    !form.format.trim() && 'format',
    !form.category.trim() && 'category',
  ].filter(Boolean) as string[]

  const submit = async () => {
    if (missing.length) {
      setError(`Required: ${missing.join(', ')}.`)
      return
    }
    setError(null)
    setSubmitting(true)
    const angle = (form.angle ?? '').trim()
    const body: BriefInput = {
      site_id: form.site_id,
      reader_question: form.reader_question.trim(),
      format: form.format.trim(),
      category: form.category.trim(),
      ...(angle ? { angle } : {}),
    }
    try {
      const res = await cdPost<Brief>('/briefs', body)
      setCreated(res ?? { ...body })
      toast.addToast({ type: 'success', title: 'Brief created', message: body.reader_question })
      setForm(f => ({ ...f, reader_question: '', angle: '' }))
      briefs.reload()
    } catch (e) {
      setError(errMsg(e))
      toast.addToast({ type: 'error', title: 'Brief not created', message: errMsg(e) })
    } finally {
      setSubmitting(false)
    }
  }

  const run = async (articleId: string) => {
    setRunning(articleId)
    try {
      await runPipeline(articleId)
      toast.addToast({ type: 'success', title: 'Pipeline run requested', message: articleId })
    } catch (e) {
      toast.addToast({ type: 'error', title: 'Run pipeline failed', message: errMsg(e) })
    } finally {
      setRunning(null)
    }
  }

  const createdArticle = briefArticleId(created)
  const list = Array.isArray(briefs.data)
    ? [...briefs.data].sort((a, b) => String(b.created_at ?? '').localeCompare(String(a.created_at ?? '')))
    : null

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <Panel>
        <SectionHeader title="New brief" icon={faPenToSquare} />
        {sites.loading && !sites.data ? (
          <LoadingRow what="sites" />
        ) : sites.error && !sites.data ? (
          <SectionError label="Site list (needed to create a brief)" error={sites.error} onRetry={sites.reload} />
        ) : siteList.length === 0 ? (
          <EmptyState icon={faPenToSquare} title="No Content Desk sites configured" hint="A brief needs a site; none are returned by GET sites." />
        ) : (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 10, maxWidth: 820 }}>
            <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap' }}>
              <label style={filterFieldLabelStyle}>site *
                <select aria-label="Site" value={form.site_id} onChange={e => setForm(f => ({ ...f, site_id: e.target.value }))} style={{ ...filterInputStyle, width: 240 }}>
                  <option value="">— choose —</option>
                  {siteList.map(s => (
                    <option key={s.id} value={s.id} disabled={!s.enabled}>
                      {s.domain} · {s.brand_code} · {s.surface}{s.enabled ? '' : ' (disabled)'}
                    </option>
                  ))}
                </select>
              </label>
              <label style={filterFieldLabelStyle}>format *
                <input aria-label="Format" value={form.format} onChange={e => setForm(f => ({ ...f, format: e.target.value }))} style={{ ...filterInputStyle, width: 160 }} />
              </label>
              <label style={filterFieldLabelStyle}>category *
                <input aria-label="Category" value={form.category} onChange={e => setForm(f => ({ ...f, category: e.target.value }))} style={{ ...filterInputStyle, width: 180 }} />
              </label>
            </div>
            <label style={filterFieldLabelStyle}>reader question *
              <textarea
                aria-label="Reader question"
                rows={2}
                value={form.reader_question}
                onChange={e => setForm(f => ({ ...f, reader_question: e.target.value }))}
                placeholder="The question a reader has that this article answers"
                style={{ ...filterInputStyle, width: '100%', boxSizing: 'border-box', resize: 'vertical', fontFamily: 'inherit' }}
              />
            </label>
            <label style={filterFieldLabelStyle}>angle (optional)
              <input aria-label="Angle" value={form.angle ?? ''} onChange={e => setForm(f => ({ ...f, angle: e.target.value }))} style={{ ...filterInputStyle, width: '100%', boxSizing: 'border-box' }} />
            </label>
            <div style={{ display: 'flex', gap: 10, alignItems: 'center' }}>
              <button type="button" style={btnStyle} disabled={submitting} onClick={submit}>
                {submitting ? 'Creating…' : 'Create brief'}
              </button>
              {missing.length > 0 && <span style={{ fontSize: 11, color: colors.textFaint }}>required: {missing.join(', ')}</span>}
            </div>
            {error && <SectionError label="Brief" error={error} />}
          </div>
        )}

        {created && (
          <div style={{ marginTop: 12, padding: '8px 10px', borderRadius: 8, border: `1px solid ${colors.hairline}`, fontSize: 12 }}>
            <div style={{ color: colors.successText, fontWeight: 600, marginBottom: 4 }}>Brief created</div>
            <div style={{ color: colors.textMuted }}>
              {siteName(created.site_id)} · <SafeText value={created.reader_question} />
            </div>
            <div style={{ marginTop: 6, display: 'flex', gap: 8, alignItems: 'center' }}>
              {createdArticle ? (
                <>
                  <button type="button" style={btnStyle} disabled={running === createdArticle} onClick={() => run(createdArticle)}>
                    <FontAwesomeIcon icon={faPlay} /> {running === createdArticle ? 'Requesting…' : 'Run pipeline'}
                  </button>
                  <button type="button" style={btnStyle} onClick={() => onOpenArticle(createdArticle)}>
                    <FontAwesomeIcon icon={faFolderOpen} /> Open article
                  </button>
                </>
              ) : (
                <span style={{ color: colors.textFaint }}>
                  The response names no article id — run the pipeline from the article once it appears in the Review queue.
                </span>
              )}
            </div>
          </div>
        )}
      </Panel>

      <Panel>
        <SectionHeader
          title="Recent briefs"
          icon={faClockRotateLeft}
          right={
            <span style={{ display: 'inline-flex', gap: 10, alignItems: 'center' }}>
              <FetchNote stamp={briefs.stamp} />
              <button type="button" style={{ ...btnStyle, padding: '3px 9px', fontSize: 11 }} onClick={briefs.reload}>
                <FontAwesomeIcon icon={faRotate} /> Refresh
              </button>
            </span>
          }
        />
        {briefs.loading && !briefs.data ? (
          <LoadingRow what="briefs" />
        ) : list == null ? (
          <SectionError label="Briefs" error={briefs.error ?? 'the API returned no list'} onRetry={briefs.reload} />
        ) : list.length === 0 ? (
          <EmptyState icon={faClockRotateLeft} title="No briefs yet" hint="Create the first one above." />
        ) : (
          <ScrollX>
            <table style={tableStyle}>
              <thead>
                <tr>
                  <th style={thStyle}>Created (MT)</th>
                  <th style={thStyle}>Site</th>
                  <th style={thStyle}>Reader question</th>
                  <th style={thStyle}>Format</th>
                  <th style={thStyle}>Category</th>
                  <th style={thStyle}>Status</th>
                  <th style={thStyle}>Article</th>
                </tr>
              </thead>
              <tbody>
                {list.map((b, i) => {
                  const aid = briefArticleId(b)
                  return (
                    <tr key={b.id ?? i}>
                      <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>{b.created_at ? fmtTime(b.created_at) : <Unknown />}</td>
                      <td style={tdStyle}>{b.site_id ? siteName(b.site_id) : <Unknown />}</td>
                      <td style={{ ...tdStyle, maxWidth: 380, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={asText(b.reader_question)}>
                        <SafeText value={b.reader_question} />
                      </td>
                      <td style={tdStyle}><SafeText value={b.format} /></td>
                      <td style={tdStyle}><SafeText value={b.category} /></td>
                      <td style={tdStyle}>{b.status ? <Pill color={stateColor(b.status)} style={{ fontSize: 10, padding: '1px 8px' }}>{b.status}</Pill> : <Unknown />}</td>
                      <td style={{ ...tdStyle, whiteSpace: 'nowrap' }}>
                        {aid ? (
                          <span style={{ display: 'inline-flex', gap: 6 }}>
                            <button type="button" style={{ ...btnStyle, padding: '2px 8px', fontSize: 11 }} onClick={() => onOpenArticle(aid)}>Open</button>
                            <button type="button" style={{ ...btnStyle, padding: '2px 8px', fontSize: 11 }} disabled={running === aid} onClick={() => run(aid)}>
                              {running === aid ? '…' : 'Run'}
                            </button>
                          </span>
                        ) : (
                          <span style={{ fontSize: 11, color: colors.textFaint }}>no article yet</span>
                        )}
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </ScrollX>
        )}
      </Panel>
    </div>
  )
}

export default ContentDeskBrief
