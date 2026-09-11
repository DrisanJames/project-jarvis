// ContentDeskArticle.tsx — Pane 2 of the Content Desk: one article, reviewed.
//
// The article renders as reviewers will see it, block by block. Every sentence
// the judgment pass covered is marked in place; clicking it opens the evidence
// panel: the sentence next to its source passage, context, dates, scope,
// conditions, claim status and derivation.
//
// Rules:
//   · NO RAW HTML. Every field goes through SafeText / plain text nodes; there is
//     no dangerouslySetInnerHTML anywhere on this screen. Markup shows escaped
//     with a visible marker.
//   · The review is bound to revision.revision_hash — the revision on screen. A
//     409 means it moved: toast, reload, keep the reviewer's findings.
//   · Blind second review hides the primary review AND the judgment verdicts
//     (including lost qualifiers, which imply the verdict) until the second
//     reviewer has submitted.
//   · "Never built" (no revision) and "built empty" (a revision with no blocks)
//     are different displays.

import React from 'react'
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome'
import {
  faArrowLeft, faRotate, faPlay, faBan, faTriangleExclamation, faFileLines, faMagnifyingGlass,
  faClipboardCheck, faFlag, faPlus, faTrash, faEyeSlash, faUserCheck, faListCheck,
} from '@fortawesome/free-solid-svg-icons'
import { colors, alpha, btnStyle, stateColor } from '../shared/theme'
import { Panel, SectionHeader, SectionError, EmptyState, Pill } from '../shared/ui'
import { SubNav } from '../shared/SubNav'
import { useToast } from '../shared/ToastSystem'
import { filterFieldLabelStyle, filterInputStyle } from '../shared/filters'
import { Unknown, LoadingRow, ScrollX, fmtTime, tableStyle, tdStyle } from './supplyShared'
import {
  useCdGet, articlePath, runPipeline, withdrawArticle, errMsg, buildReviewPayload, submitReview,
  groupJudgments, segmentPieces, claimKey, asText, containsHtml, safeUrl, worstVerdict, verdictColor,
  decisionColor, sevColor, SEVERITIES, SafeText, EscapedMarker, VerdictChip, CheckPill, Hash, FetchNote,
  toneBtn, mono,
  type ArticleDetail, type ArticlePackage, type Block, type Claim, type SentenceGroup, type FindingDraft, type ReviewRole,
  type ReviewDecision, type ReviewPayload, type ReviewRecord, type Severity, type Judgment,
} from './contentDeskShared'

type Mode = ReviewRole

const MODES = [
  { key: 'primary', label: 'Primary reviewer' },
  { key: 'second', label: 'Second reviewer (blind)' },
]

const CAUGHT_BY_SUGGESTIONS = ['reviewer', 'code_check', 'judgment']

// ═══════════════════════════════════════════════════════════════════════════
// PANE
// ═══════════════════════════════════════════════════════════════════════════

export const ContentDeskArticle: React.FC<{ articleId: string | null; onBack: () => void }> = ({ articleId, onBack }) => {
  const toast = useToast()
  const detail = useCdGet<ArticleDetail>(articleId ? articlePath(articleId) : null)
  const [mode, setMode] = React.useState<Mode>('primary')
  const [blindSubmitted, setBlindSubmitted] = React.useState(false)
  const [selected, setSelected] = React.useState<string | null>(null)
  const [flag, setFlag] = React.useState<{ blockId: string; nonce: number } | null>(null)
  const [busy, setBusy] = React.useState<'' | 'run' | 'withdraw'>('')

  React.useEffect(() => {
    setBlindSubmitted(false)
    setSelected(null)
  }, [articleId])

  const reveal = mode === 'primary' || blindSubmitted

  const d = detail.data
  const rev = d?.revision ?? null
  const pkg = rev?.package ?? null
  const blocks: Block[] = React.useMemo(() => (Array.isArray(pkg?.blocks) ? (pkg?.blocks as Block[]) : []), [pkg])
  const judgments: Judgment[] = React.useMemo(() => rev?.checks?.judgment ?? [], [rev])
  const groups = React.useMemo(() => groupJudgments(judgments), [judgments])
  const groupsByBlock = React.useMemo(() => {
    const m = new Map<string, SentenceGroup[]>()
    groups.forEach(g => m.set(g.blockId, [...(m.get(g.blockId) ?? []), g]))
    return m
  }, [groups])
  const groupByKey = React.useMemo(() => new Map(groups.map(g => [g.key, g])), [groups])
  const claims = React.useMemo(() => new Map((d?.claims ?? []).map(c => [claimKey(c.claim_id, c.version), c])), [d])
  const blockIds = React.useMemo(() => new Set(blocks.map(b => b.id)), [blocks])
  const orphanGroups = groups.filter(g => !blockIds.has(g.blockId))

  if (!articleId) {
    return (
      <Panel>
        <EmptyState icon={faFileLines} title="No article selected" hint="Open one from the Review queue." />
      </Panel>
    )
  }
  if (detail.loading && !d) return <LoadingRow what="the article" />
  if (!d) return <SectionError label="Article" error={detail.error ?? 'no data'} onRetry={detail.reload} />

  const a = d.article
  const act = async (kind: 'run' | 'withdraw') => {
    if (kind === 'withdraw' && !window.confirm('Withdraw this article? It is pulled from review and release.')) return
    setBusy(kind)
    try {
      if (kind === 'run') await runPipeline(articleId)
      else await withdrawArticle(articleId)
      toast.addToast({
        type: 'success',
        title: kind === 'run' ? 'Pipeline run requested' : 'Article withdrawn',
        message: asText(a.title) || articleId,
      })
      detail.reload()
    } catch (e) {
      toast.addToast({ type: 'error', title: kind === 'run' ? 'Run pipeline failed' : 'Withdraw failed', message: errMsg(e) })
    } finally {
      setBusy('')
    }
  }

  const verdictCounts = judgments.reduce<Record<string, number>>((m, j) => {
    m[j.verdict] = (m[j.verdict] ?? 0) + 1
    return m
  }, {})
  const selectedGroup = selected ? groupByKey.get(selected) ?? null : null

  return (
    <div>
      {/* ── Header ───────────────────────────────────────────────────── */}
      <Panel style={{ marginBottom: 12 }}>
        <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 12, flexWrap: 'wrap' }}>
          <div style={{ minWidth: 0 }}>
            <button type="button" onClick={onBack} style={{ ...btnStyle, padding: '3px 9px', fontSize: 11, marginBottom: 8 }}>
              <FontAwesomeIcon icon={faArrowLeft} /> Review queue
            </button>
            <div style={{ fontSize: 18, fontWeight: 700, color: colors.heading }}>
              <SafeText value={pkg?.title ?? a.title} />
            </div>
            <div style={{ display: 'flex', gap: 12, alignItems: 'center', flexWrap: 'wrap', marginTop: 6, fontSize: 12, color: colors.textMuted }}>
              <span>{a.domain || <Unknown />}</span>
              <span style={mono} title={a.slug}>/{a.slug}</span>
              {a.status ? <Pill color={stateColor(a.status)} style={{ fontSize: 10, padding: '1px 8px' }}>{a.status}</Pill> : <Unknown hint="no status" />}
              <span>revision <Hash value={rev?.revision_hash ?? a.current_revision_hash} /></span>
              <span>updated {a.updated_at ? `${fmtTime(a.updated_at)} MT` : <Unknown />}</span>
              <FetchNote stamp={detail.stamp} />
            </div>
          </div>
          <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
            <button type="button" style={btnStyle} onClick={detail.reload} disabled={detail.loading}>
              <FontAwesomeIcon icon={faRotate} spin={detail.loading} /> Reload
            </button>
            <button type="button" style={btnStyle} onClick={() => act('run')} disabled={busy !== ''}>
              <FontAwesomeIcon icon={faPlay} /> {busy === 'run' ? 'Requesting…' : 'Run pipeline'}
            </button>
            <button type="button" style={toneBtn(colors.danger)} onClick={() => act('withdraw')} disabled={busy !== ''}>
              <FontAwesomeIcon icon={faBan} /> {busy === 'withdraw' ? 'Withdrawing…' : 'Withdraw'}
            </button>
          </div>
        </div>
        {detail.error && (
          <div style={{ marginTop: 10 }}>
            <SectionError label="Article refresh (showing the last good copy)" error={detail.error} onRetry={detail.reload} />
          </div>
        )}
      </Panel>

      {a.consequential && (
        <Panel accent={colors.danger} style={{ marginBottom: 12, background: alpha(colors.danger, '14') }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 10, color: colors.dangerText }}>
            <FontAwesomeIcon icon={faTriangleExclamation} />
            <strong style={{ letterSpacing: 0.5, textTransform: 'uppercase', fontSize: 13 }}>Mandatory human review</strong>
            <span style={{ fontSize: 12, color: colors.dangerFaint }}>
              This article is consequential — it cannot ship on machine checks alone.
            </span>
          </div>
        </Panel>
      )}

      <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
        <SubNav
          items={MODES}
          active={mode}
          onChange={k => {
            setMode(k as Mode)
            if (k === 'second') setBlindSubmitted(false)
          }}
          ariaLabel="Reviewer mode"
        />
        {mode === 'second' && (
          <span style={{ fontSize: 12, color: blindSubmitted ? colors.successText : colors.warningText, marginBottom: 20 }}>
            <FontAwesomeIcon icon={blindSubmitted ? faUserCheck : faEyeSlash} />{' '}
            {blindSubmitted
              ? 'Your blind review is recorded — the primary review and verdicts are now shown.'
              : 'Blind: the primary review and the judgment verdicts are hidden until you submit.'}
          </span>
        )}
      </div>

      {!rev ? (
        <Panel>
          <EmptyState icon={faFileLines} title="Never built — this article has no revision yet" hint="Run the pipeline to produce a first revision." />
        </Panel>
      ) : (
        <div style={{ display: 'flex', gap: 14, alignItems: 'flex-start', flexWrap: 'wrap' }}>
          {/* ── Left: the article ─────────────────────────────────────── */}
          <div style={{ flex: '1 1 560px', minWidth: 0, display: 'flex', flexDirection: 'column', gap: 12 }}>
            <PackagePanel pkg={pkg} />

            <Panel>
              <SectionHeader
                title="Article body"
                icon={faFileLines}
                right={
                  reveal ? (
                    <span style={{ display: 'inline-flex', gap: 6, alignItems: 'center', fontSize: 11, color: colors.textMuted }}>
                      {groups.length} judged sentence{groups.length === 1 ? '' : 's'}
                      {Object.entries(verdictCounts).map(([v, n]) => (
                        <span key={v} style={{ display: 'inline-flex', gap: 4, alignItems: 'center' }}>
                          <VerdictChip verdict={v} /> {n}
                        </span>
                      ))}
                    </span>
                  ) : (
                    <span style={{ fontSize: 11, color: colors.textMuted }}>
                      {groups.length} judged sentence{groups.length === 1 ? '' : 's'} · verdicts hidden (blind)
                    </span>
                  )
                }
              />
              {blocks.length === 0 ? (
                <EmptyState icon={faFileLines} title="Built empty — this revision has no blocks" />
              ) : (
                blocks.map(b => (
                  <BlockView
                    key={b.id}
                    block={b}
                    groups={groupsByBlock.get(b.id) ?? []}
                    reveal={reveal}
                    selected={selected}
                    onSelect={setSelected}
                    onFlag={id => setFlag(f => ({ blockId: id, nonce: (f?.nonce ?? 0) + 1 }))}
                  />
                ))
              )}
              {orphanGroups.length > 0 && (
                <div style={{ marginTop: 10, fontSize: 11, color: colors.warningText }}>
                  Judged sentences whose block_id is not in this revision:
                  {orphanGroups.map(g => (
                    <div key={g.key} style={{ marginTop: 4 }}>
                      <span style={mono}>{g.blockId}</span>{' '}
                      <SentenceMark group={g} text={g.sentence} reveal={reveal} selected={selected} onSelect={setSelected} />
                    </div>
                  ))}
                </div>
              )}
            </Panel>

            <CodeChecksPanel checks={rev.checks?.code} />

            {reveal ? (
              <ReviewsPanel reviews={d.reviews} currentHash={rev.revision_hash} />
            ) : (
              <Panel>
                <SectionHeader title="Prior reviews" icon={faEyeSlash} />
                <div style={{ fontSize: 12, color: colors.textMuted }}>
                  Hidden in blind second-review mode — the primary review appears after you submit yours.
                </div>
              </Panel>
            )}
          </div>

          {/* ── Right: evidence + review form ────────────────────────── */}
          <div style={{ flex: '1 1 340px', minWidth: 0, display: 'flex', flexDirection: 'column', gap: 12, position: 'sticky', top: 12 }}>
            <EvidencePanel group={selectedGroup} claims={claims} reveal={reveal} />
            <ReviewForm
              articleId={articleId}
              revisionHash={rev.revision_hash}
              role={mode}
              blockIds={blocks.map(b => b.id)}
              flag={flag}
              onSubmitted={() => {
                if (mode === 'second') setBlindSubmitted(true)
                detail.reload()
              }}
              onConflict={detail.reload}
            />
          </div>
        </div>
      )}
    </div>
  )
}

// ═══════════════════════════════════════════════════════════════════════════
// PACKAGE — the fields that ship alongside the body
// ═══════════════════════════════════════════════════════════════════════════

const Field: React.FC<{ label: string; children: React.ReactNode }> = ({ label, children }) => (
  <div style={{ display: 'grid', gridTemplateColumns: '130px minmax(0,1fr)', gap: 10, padding: '5px 0', borderTop: `1px solid ${colors.divider}`, fontSize: 12 }}>
    <div style={{ color: colors.textMuted, fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.4 }}>{label}</div>
    <div style={{ color: colors.text, overflowWrap: 'anywhere' }}>{children}</div>
  </div>
)

const TextList: React.FC<{ items: unknown }> = ({ items }) => {
  if (items == null) return <Unknown hint="not present in the package" />
  if (!Array.isArray(items)) return <SafeText value={items} />
  if (items.length === 0) return <span style={{ color: colors.textFaint, fontStyle: 'italic' }}>none</span>
  return (
    <ol style={{ margin: 0, paddingLeft: 18 }}>
      {items.map((s, i) => <li key={i}><SafeText value={s} /></li>)}
    </ol>
  )
}

const heroParts = (v: unknown): { url: string | null; alt: string; raw: unknown } => {
  if (typeof v === 'string') return { url: safeUrl(v), alt: '', raw: v }
  if (v && typeof v === 'object') {
    const o = v as Record<string, unknown>
    return { url: safeUrl(o.url ?? o.src), alt: asText(o.alt ?? ''), raw: v }
  }
  return { url: null, alt: '', raw: v }
}

const PackagePanel: React.FC<{ pkg: ArticlePackage | null }> = ({ pkg }) => {
  const hero = heroParts(pkg?.hero_image)
  return (
    <Panel>
      <SectionHeader title="Package — what ships" icon={faClipboardCheck} />
      {!pkg ? (
        <Unknown hint="the revision carries no package" />
      ) : (
        <>
          <Field label="Title"><SafeText value={pkg.title} /></Field>
          <Field label="Excerpt"><SafeText value={pkg.excerpt} /></Field>
          <Field label="Meta title"><SafeText value={pkg.meta_title} /></Field>
          <Field label="Meta description"><SafeText value={pkg.meta_description} /></Field>
          <Field label="Hero image">
            {pkg.hero_image == null ? (
              <Unknown hint="no hero_image in the package" />
            ) : hero.url ? (
              <div>
                <img src={hero.url} alt={hero.alt} style={{ maxWidth: '100%', maxHeight: 220, borderRadius: 6, display: 'block' }} />
                <div style={{ fontSize: 11, color: colors.textFaint, marginTop: 4 }}>
                  alt: <SafeText value={hero.alt} />
                </div>
              </div>
            ) : (
              <span>
                <SafeText value={hero.raw} />{' '}
                <span style={{ fontSize: 11, color: colors.warningText }}>(not an http(s) URL — not rendered)</span>
              </span>
            )}
          </Field>
          <Field label="Subjects"><TextList items={pkg.subjects} /></Field>
          <Field label="Preheaders"><TextList items={pkg.preheaders} /></Field>
        </>
      )}
    </Panel>
  )
}

// ═══════════════════════════════════════════════════════════════════════════
// BLOCKS
// ═══════════════════════════════════════════════════════════════════════════

const HEADING_TYPES = new Set(['heading', 'h1', 'h2', 'h3', 'h4', 'title', 'subheading'])
const QUOTE_TYPES = new Set(['quote', 'blockquote', 'pullquote'])
const ORDERED_TYPES = new Set(['ol', 'ordered_list', 'numbered_list', 'steps'])

const SentenceMark: React.FC<{
  group: SentenceGroup
  text: string
  reveal: boolean
  selected: string | null
  onSelect: (k: string) => void
}> = ({ group, text, reveal, selected, onSelect }) => {
  const worst = reveal ? worstVerdict(group.judgments) : null
  const c = worst ? verdictColor(worst) : colors.indigo400
  const on = selected === group.key
  return (
    <span
      role="button"
      tabIndex={0}
      data-testid="judged-sentence"
      onClick={() => onSelect(group.key)}
      onKeyDown={e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onSelect(group.key) } }}
      title={reveal ? `${worst ?? 'judged'} — click for the source passage` : 'Judged sentence (verdict hidden — blind) — click for the source passage'}
      style={{
        background: alpha(c, on ? '33' : '14'),
        borderBottom: `2px solid ${alpha(c, '66')}`,
        outline: on ? `1px solid ${c}` : 'none',
        borderRadius: 3,
        padding: '0 1px',
        cursor: 'pointer',
      }}
    >
      {text}
    </span>
  )
}

const BlockView: React.FC<{
  block: Block
  groups: SentenceGroup[]
  reveal: boolean
  selected: string | null
  onSelect: (k: string) => void
  onFlag: (blockId: string) => void
}> = ({ block, groups, reveal, selected, onSelect, onFlag }) => {
  const type = String(block.type ?? '').toLowerCase()
  const text = block.text != null ? asText(block.text) : null
  const items = Array.isArray(block.items) ? (block.items as unknown[]).map(asText) : null
  const rows = Array.isArray(block.rows)
    ? (block.rows as unknown[]).map(r => (Array.isArray(r) ? (r as unknown[]).map(asText) : [asText(r)]))
    : null
  const pieces: string[] = text != null ? [text] : items ?? (rows ? rows.flat() : [])
  const { pieces: segs, unplaced } = segmentPieces(pieces, groups)
  const html = pieces.some(containsHtml)

  const renderPiece = (i: number) =>
    (segs[i] ?? []).map((s, j) =>
      s.group ? (
        <SentenceMark key={j} group={s.group} text={s.text} reveal={reveal} selected={selected} onSelect={onSelect} />
      ) : (
        <React.Fragment key={j}>{s.text}</React.Fragment>
      ),
    )

  let body: React.ReactNode
  if (text != null) {
    if (HEADING_TYPES.has(type)) body = <div style={{ fontSize: 17, fontWeight: 700, color: colors.heading }}>{renderPiece(0)}</div>
    else if (QUOTE_TYPES.has(type)) {
      body = (
        <blockquote style={{ margin: 0, paddingLeft: 12, borderLeft: `3px solid ${colors.panelBorderStrong}`, color: colors.textMuted, fontStyle: 'italic' }}>
          {renderPiece(0)}
        </blockquote>
      )
    } else body = <p style={{ margin: 0, lineHeight: 1.65 }}>{renderPiece(0)}</p>
  } else if (items) {
    const List = ORDERED_TYPES.has(type) ? 'ol' : 'ul'
    body = <List style={{ margin: 0, paddingLeft: 20, lineHeight: 1.6 }}>{items.map((_, i) => <li key={i}>{renderPiece(i)}</li>)}</List>
  } else if (rows) {
    const offsets: number[] = []
    rows.reduce((acc, r) => { offsets.push(acc); return acc + r.length }, 0)
    body = (
      <ScrollX>
        <table style={tableStyle}>
          <tbody>
            {rows.map((r, ri) => (
              <tr key={ri}>
                {r.map((_, ci) => (
                  <td key={ci} style={{ ...tdStyle, fontWeight: ri === 0 ? 700 : 400 }}>{renderPiece(offsets[ri] + ci)}</td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </ScrollX>
    )
  } else {
    const { id: _id, type: _type, ...rest } = block
    void _id
    void _type
    const img = safeUrl(rest.url ?? rest.src)
    body = (
      <div>
        {img && <img src={img} alt={asText(rest.alt ?? '')} style={{ maxWidth: '100%', maxHeight: 260, borderRadius: 6, display: 'block', marginBottom: 6 }} />}
        <div style={{ fontSize: 11, color: colors.textFaint }}>{type || 'untyped'} block — no text / items / rows; fields shown as escaped JSON:</div>
        <pre style={{ ...mono, margin: '4px 0 0', whiteSpace: 'pre-wrap', color: colors.textMuted }}>{JSON.stringify(rest, null, 2)}</pre>
      </div>
    )
  }

  return (
    <div style={{ display: 'flex', gap: 10, padding: '10px 0', borderTop: `1px solid ${colors.divider}` }}>
      <div style={{ width: 92, flexShrink: 0, fontSize: 10, color: colors.textFaint }}>
        <div title={`block id ${block.id}`} style={{ ...mono, fontSize: 10, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{block.id}</div>
        <div>{type || 'untyped'}</div>
        <button
          type="button"
          onClick={() => onFlag(block.id)}
          title="Add a finding against this block"
          style={{ ...btnStyle, padding: '1px 6px', fontSize: 10, marginTop: 4 }}
        >
          <FontAwesomeIcon icon={faFlag} /> Flag
        </button>
      </div>
      <div style={{ flex: 1, minWidth: 0, fontSize: 14, color: colors.text }}>
        {body}
        {html && <div style={{ marginTop: 4 }}><EscapedMarker /></div>}
        {unplaced.length > 0 && (
          <div style={{ marginTop: 6, fontSize: 11, color: colors.warningText }}>
            Judged sentence{unplaced.length === 1 ? '' : 's'} not found verbatim in this block:
            {unplaced.map(g => (
              <div key={g.key} style={{ marginTop: 3 }}>
                #{g.sentenceIdx} <SentenceMark group={g} text={g.sentence || '(empty sentence)'} reveal={reveal} selected={selected} onSelect={onSelect} />
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  )
}

// ═══════════════════════════════════════════════════════════════════════════
// EVIDENCE — the sentence beside its source
// ═══════════════════════════════════════════════════════════════════════════

const DateVal: React.FC<{ v: string | null | undefined }> = ({ v }) =>
  v ? <span title={v}>{fmtTime(v)} MT</span> : <Unknown hint="not recorded on the claim" />

const ClaimFields: React.FC<{ claim: Claim }> = ({ claim }) => {
  const href = safeUrl(claim.source_url)
  return (
    <div>
      <Field label="Claim"><SafeText value={claim.text} /></Field>
      <Field label="Type"><SafeText value={claim.type} /></Field>
      <Field label="Source passage">
        {claim.passage == null ? (
          <Unknown hint="no passage on the claim" />
        ) : (
          <blockquote style={{ margin: 0, paddingLeft: 10, borderLeft: `3px solid ${colors.indigo400}`, color: colors.heading }}>
            <SafeText value={claim.passage} />
          </blockquote>
        )}
      </Field>
      <Field label="Context"><SafeText value={claim.context} /></Field>
      <Field label="Source">
        {href ? (
          <a href={href} target="_blank" rel="noopener noreferrer" style={{ color: colors.indigo300 }}>{href}</a>
        ) : (
          <SafeText value={claim.source_url} />
        )}
      </Field>
      <Field label="Published"><DateVal v={claim.published_at} /></Field>
      <Field label="Effective"><DateVal v={claim.effective_at} /></Field>
      <Field label="Retrieved"><DateVal v={claim.retrieved_at} /></Field>
      <Field label="Jurisdiction"><SafeText value={claim.jurisdiction} /></Field>
      <Field label="Population (scope)"><SafeText value={claim.population} /></Field>
      <Field label="Conditions"><SafeText value={claim.conditions} /></Field>
      <Field label="Claim status">
        {claim.status ? <Pill color={stateColor(claim.status)} style={{ fontSize: 10, padding: '1px 8px' }}>{claim.status}</Pill> : <Unknown />}
      </Field>
      <Field label="Derivation"><SafeText value={claim.derivation} /></Field>
    </div>
  )
}

const EvidencePanel: React.FC<{ group: SentenceGroup | null; claims: Map<string, Claim>; reveal: boolean }> = ({ group, claims, reveal }) => (
  <Panel>
    <SectionHeader title="Evidence" icon={faMagnifyingGlass} />
    {!group ? (
      <EmptyState
        icon={faMagnifyingGlass}
        title="Select a highlighted sentence"
        hint="It opens here beside its source passage, context, dates, scope, conditions, claim status and derivation."
      />
    ) : (
      <div>
        <div style={{ fontSize: 10, color: colors.textFaint, textTransform: 'uppercase', letterSpacing: 0.5 }}>
          Sentence · block <span style={mono}>{group.blockId}</span> · #{group.sentenceIdx}
        </div>
        <div style={{ fontSize: 14, color: colors.heading, margin: '4px 0 10px', lineHeight: 1.5 }}>
          <SafeText value={group.sentence} />
        </div>
        {group.judgments.map((j, i) => {
          const c = claims.get(claimKey(j.claim_id, j.version))
          return (
            <div key={i} style={{ border: `1px solid ${colors.hairline}`, borderRadius: 8, padding: '8px 10px', marginBottom: 8 }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', marginBottom: 4 }}>
                {reveal ? <VerdictChip verdict={j.verdict} /> : <Pill color={colors.idle} style={{ fontSize: 10, padding: '1px 8px' }}>verdict hidden</Pill>}
                <span style={{ ...mono, color: colors.textMuted }} title="claim_id @ version">{j.claim_id} v{String(j.version)}</span>
              </div>
              {reveal && j.lost_qualifier && (
                <div style={{ fontSize: 12, color: colors.warningText, marginBottom: 4 }}>
                  Lost qualifier: <SafeText value={j.lost_qualifier} />
                </div>
              )}
              {c ? (
                <ClaimFields claim={c} />
              ) : (
                <div style={{ fontSize: 12, color: colors.warningText }}>
                  Claim {j.claim_id} v{String(j.version)} is not in this article's claim bundle — the evidence cannot be shown.
                </div>
              )}
            </div>
          )
        })}
      </div>
    )}
  </Panel>
)

// ═══════════════════════════════════════════════════════════════════════════
// CODE CHECKS + PRIOR REVIEWS
// ═══════════════════════════════════════════════════════════════════════════

const CodeChecksPanel: React.FC<{ checks: { name: string; passed: boolean; detail?: string | null }[] | null | undefined }> = ({ checks }) => {
  const passed = (checks ?? []).filter(c => c.passed).length
  return (
    <Panel>
      <SectionHeader
        title="Code checks"
        icon={faListCheck}
        right={checks && checks.length > 0 ? <span style={{ fontSize: 11, color: colors.textMuted }}>{passed} of {checks.length} passed</span> : undefined}
      />
      {checks == null ? (
        <Unknown hint="the revision reports no code checks" />
      ) : checks.length === 0 ? (
        <div style={{ fontSize: 12, color: colors.textFaint }}>No code checks ran on this revision.</div>
      ) : (
        [...checks].sort((x, y) => Number(x.passed) - Number(y.passed)).map((c, i) => (
          <div key={i} style={{ display: 'flex', gap: 10, alignItems: 'baseline', padding: '4px 0', borderTop: `1px solid ${colors.divider}`, fontSize: 12 }}>
            <CheckPill status={c.passed ? 'ok' : 'fail'} label={c.passed ? 'pass' : 'fail'} />
            <span style={{ color: colors.text, minWidth: 140 }}><SafeText value={c.name} /></span>
            <span style={{ color: colors.textMuted, overflowWrap: 'anywhere' }}>{c.detail ? <SafeText value={c.detail} /> : ''}</span>
          </div>
        ))
      )}
    </Panel>
  )
}

const ReviewsPanel: React.FC<{ reviews: ReviewRecord[] | null; currentHash: string }> = ({ reviews, currentHash }) => (
  <Panel>
    <SectionHeader title="Prior reviews" icon={faUserCheck} />
    {reviews == null ? (
      <Unknown hint="the API sent no reviews array" />
    ) : reviews.length === 0 ? (
      <div style={{ fontSize: 12, color: colors.textFaint }}>No reviews yet.</div>
    ) : (
      reviews.map((r, i) => {
        const stale = r.revision_hash && r.revision_hash !== currentHash
        return (
          <div key={r.id ?? i} style={{ borderTop: `1px solid ${colors.divider}`, padding: '8px 0', fontSize: 12 }}>
            <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
              <Pill color={colors.indigo400} style={{ fontSize: 10, padding: '1px 8px' }}>{r.role ?? 'role?'}</Pill>
              <Pill color={decisionColor(r.decision)} style={{ fontSize: 10, padding: '1px 8px' }}>{r.decision ?? 'decision?'}</Pill>
              {r.reviewer && <span style={{ color: colors.textMuted }}><SafeText value={r.reviewer} /></span>}
              <span style={{ color: colors.textMuted }}>{r.minutes == null ? <Unknown hint="minutes not recorded" /> : `${r.minutes} min`}</span>
              {r.created_at && <span style={{ color: colors.textFaint }}>{fmtTime(r.created_at)} MT</span>}
              <span>rev <Hash value={r.revision_hash} /></span>
              {stale && <span style={{ color: colors.warningText, fontSize: 11 }}>on an older revision</span>}
            </div>
            {(r.findings ?? []).map((f, k) => (
              <div key={k} style={{ display: 'flex', gap: 8, marginTop: 4, paddingLeft: 6 }}>
                <span style={{ color: sevColor(f.severity), fontWeight: 700, fontSize: 11 }}>{f.severity}</span>
                <span style={{ color: colors.textFaint, fontSize: 11 }}>{f.caught_by || '—'} · <span style={mono}>{f.block_id || 'no block'}</span></span>
                <span style={{ color: colors.text }}><SafeText value={f.text} /></span>
              </div>
            ))}
          </div>
        )
      })
    )}
  </Panel>
)

// ═══════════════════════════════════════════════════════════════════════════
// REVIEW FORM
// ═══════════════════════════════════════════════════════════════════════════

const DECISIONS: Array<{ d: ReviewDecision; label: string; color: string }> = [
  { d: 'approve', label: 'Approve', color: colors.success },
  { d: 'changes', label: 'Request changes', color: colors.warning },
  { d: 'reject', label: 'Reject', color: colors.danger },
]

const ReviewForm: React.FC<{
  articleId: string
  revisionHash: string | null
  role: ReviewRole
  blockIds: string[]
  flag: { blockId: string; nonce: number } | null
  onSubmitted: () => void
  onConflict: () => void
}> = ({ articleId, revisionHash, role, blockIds, flag, onSubmitted, onConflict }) => {
  const toast = useToast()
  const [findings, setFindings] = React.useState<FindingDraft[]>([])
  const [minutes, setMinutes] = React.useState('')
  const [submitting, setSubmitting] = React.useState<ReviewDecision | null>(null)
  const [error, setError] = React.useState<string | null>(null)
  const uid = React.useId()

  React.useEffect(() => {
    if (flag) setFindings(f => [...f, { severity: 'S2', caught_by: '', block_id: flag.blockId, text: '' }])
  }, [flag])

  const patch = (i: number, p: Partial<FindingDraft>) => setFindings(fs => fs.map((f, k) => (k === i ? { ...f, ...p } : f)))

  const submit = async (decision: ReviewDecision) => {
    setError(null)
    let payload: ReviewPayload
    try {
      payload = buildReviewPayload({ revisionHash, role, decision, findings, minutes })
    } catch (e) {
      setError(errMsg(e))
      return
    }
    setSubmitting(decision)
    const res = await submitReview(articleId, payload)
    setSubmitting(null)
    if (res.kind === 'ok') {
      toast.addToast({ type: 'success', title: 'Review recorded', message: `${decision} · ${role} · revision ${payload.revision_hash.slice(0, 10)}` })
      setFindings([])
      setMinutes('')
      onSubmitted()
    } else if (res.kind === 'conflict') {
      toast.addToast({
        type: 'warning',
        title: 'Revision changed — reloading',
        message: 'The article moved to a new revision while you reviewed it. Your findings are kept; re-read the new revision and submit again.',
        duration: 8000,
      })
      onConflict()
    } else {
      setError(res.message)
      toast.addToast({ type: 'error', title: 'Review not recorded', message: res.message })
    }
  }

  return (
    <Panel>
      <SectionHeader title={role === 'second' ? 'Second review (blind)' : 'Primary review'} icon={faClipboardCheck} />
      <div style={{ fontSize: 12, color: colors.textMuted, marginBottom: 10 }}>
        Reviewing revision <Hash value={revisionHash} n={12} /> — this hash is sent with the review; if the article moves on, the
        server refuses (409) and the page reloads.
      </div>

      {findings.length === 0 && <div style={{ fontSize: 12, color: colors.textFaint, marginBottom: 8 }}>No findings. Add one, or use Flag on a block.</div>}
      {findings.map((f, i) => (
        <div key={i} style={{ border: `1px solid ${alpha(sevColor(f.severity), '44')}`, borderLeft: `3px solid ${sevColor(f.severity)}`, borderRadius: 8, padding: 8, marginBottom: 8 }}>
          <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'flex-end' }}>
            <label style={filterFieldLabelStyle}>severity
              <select aria-label={`Finding ${i + 1} severity`} value={f.severity} onChange={e => patch(i, { severity: e.target.value as Severity })} style={{ ...filterInputStyle, width: 70 }}>
                {SEVERITIES.map(s => <option key={s} value={s}>{s}</option>)}
              </select>
            </label>
            <label style={filterFieldLabelStyle}>caught by
              <input
                aria-label={`Finding ${i + 1} caught by`}
                list={`${uid}-caught`}
                value={f.caught_by}
                placeholder="who/what caught it"
                onChange={e => patch(i, { caught_by: e.target.value })}
                style={{ ...filterInputStyle, width: 130 }}
              />
            </label>
            <label style={filterFieldLabelStyle}>block
              <select aria-label={`Finding ${i + 1} block`} value={f.block_id} onChange={e => patch(i, { block_id: e.target.value })} style={{ ...filterInputStyle, width: 130 }}>
                <option value="">— none —</option>
                {blockIds.map(b => <option key={b} value={b}>{b}</option>)}
                {f.block_id && !blockIds.includes(f.block_id) && <option value={f.block_id}>{f.block_id} (not in this revision)</option>}
              </select>
            </label>
            <button type="button" onClick={() => setFindings(fs => fs.filter((_, k) => k !== i))} style={{ ...toneBtn(colors.danger), padding: '6px 9px' }} title="Remove finding">
              <FontAwesomeIcon icon={faTrash} />
            </button>
          </div>
          <textarea
            aria-label={`Finding ${i + 1} text`}
            value={f.text}
            onChange={e => patch(i, { text: e.target.value })}
            placeholder="What is wrong, and where"
            rows={2}
            style={{ ...filterInputStyle, width: '100%', marginTop: 6, boxSizing: 'border-box', resize: 'vertical', fontFamily: 'inherit' }}
          />
        </div>
      ))}
      <datalist id={`${uid}-caught`}>
        {CAUGHT_BY_SUGGESTIONS.map(s => <option key={s} value={s} />)}
      </datalist>
      <button type="button" style={{ ...btnStyle, marginBottom: 12 }} onClick={() => setFindings(fs => [...fs, { severity: 'S2', caught_by: '', block_id: '', text: '' }])}>
        <FontAwesomeIcon icon={faPlus} /> Add finding
      </button>

      <div style={{ display: 'flex', gap: 10, alignItems: 'flex-end', flexWrap: 'wrap' }}>
        <label style={filterFieldLabelStyle}>minutes spent
          <input
            aria-label="Minutes spent"
            type="number"
            min={0}
            step={1}
            value={minutes}
            onChange={e => setMinutes(e.target.value)}
            style={{ ...filterInputStyle, width: 90 }}
          />
        </label>
        {DECISIONS.map(x => (
          <button
            key={x.d}
            type="button"
            disabled={submitting != null || !revisionHash}
            onClick={() => submit(x.d)}
            style={{ ...toneBtn(x.color), opacity: submitting != null && submitting !== x.d ? 0.5 : 1 }}
          >
            {submitting === x.d ? 'Submitting…' : x.label}
          </button>
        ))}
      </div>
      {error && <div style={{ marginTop: 8 }}><SectionError label="Review" error={error} /></div>}
    </Panel>
  )
}

export default ContentDeskArticle
