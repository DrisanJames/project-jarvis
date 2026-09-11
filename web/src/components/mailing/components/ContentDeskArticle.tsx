// ContentDeskArticle.tsx — Pane 2 of the Content Desk: one article, reviewed.
//
// The article renders as reviewers will see it, block by block — every one of
// the backend's block types (contentdesk.AllowedBlockTypes) with every field it
// carries (heading · text · items · rows · calc · caption). Every sentence the
// judgment pass covered is marked in place; clicking it opens the evidence
// panel: the sentence next to its source passage, context, dates, scope,
// conditions, claim status and derivation.
//
// Rules:
//   · NO RAW HTML. Every field goes through SafeText / plain text nodes; there is
//     no dangerouslySetInnerHTML anywhere on this screen. Markup shows escaped
//     with a visible marker.
//   · The review is bound to revision.revision_hash — the revision on screen. A
//     409 means it moved: toast, reload, keep the reviewer's findings.
//   · An approve carries accepted_ids: every non-supported judgment item and
//     every failed S2 code check must be explicitly accepted (the server
//     refuses otherwise, 422 with the blockers shown).
//   · Blind second review hides the primary review AND the judgment verdicts
//     (including lost qualifiers and notes, which imply the verdict) until the
//     second reviewer has submitted.
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
  decisionColor, sevColor, SEVERITIES, CAUGHT_BY, BLOCK_TYPES, SafeText, EscapedMarker, VerdictChip, CheckPill, Hash,
  FetchNote, toneBtn, mono, blockParts,
  type ArticleDetail, type ArticlePackage, type Block, type BlockPart, type Claim, type CodeCheck, type SentenceGroup,
  type SentencePair, type FindingDraft, type ReviewRole, type ReviewDecision, type ReviewPayload, type ReviewRecord,
  type Severity, type Judgment, type CaughtBy,
} from './contentDeskShared'

type Mode = ReviewRole

const MODES = [
  { key: 'primary', label: 'Primary reviewer' },
  { key: 'second', label: 'Second reviewer (blind)' },
]

/** An item the reviewer must explicitly accept before an approve is recorded. */
interface Acceptable {
  id: string
  label: string
  revealLabel: string
}

const pairKey = (blockId: string, idx: number, claimId: string, version: number | string) =>
  `${blockId}#${idx}@${claimKey(claimId, version)}`

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
  const codeChecks: CodeCheck[] | null = rev?.checks?.code ?? null
  const groups = React.useMemo(() => groupJudgments(judgments), [judgments])
  const groupsByBlock = React.useMemo(() => {
    const m = new Map<string, SentenceGroup[]>()
    groups.forEach(g => m.set(g.blockId, [...(m.get(g.blockId) ?? []), g]))
    return m
  }, [groups])
  const groupByKey = React.useMemo(() => new Map(groups.map(g => [g.key, g])), [groups])
  const claims = React.useMemo(() => new Map((d?.claims ?? []).map(c => [claimKey(c.claim_id, c.version), c])), [d])
  const pairs = React.useMemo(
    () => new Map((d?.pairs ?? []).map(p => [pairKey(p.block_id, p.sentence_idx, p.claim_id, p.version), p])),
    [d],
  )
  const acceptables: Acceptable[] = React.useMemo(() => {
    const out: Acceptable[] = []
    judgments.forEach(j => {
      if (j.kind === 'claim' && j.verdict === 'supported') return
      const where = `${j.block_id}#${j.sentence_idx}`
      out.push({
        id: j.id,
        label: `${j.id} · ${where}`,
        revealLabel: `${j.id} · ${j.kind === 'claim' ? j.verdict : `flag ${j.kind}`} · ${where}`,
      })
    })
    ;(codeChecks ?? []).forEach(c => {
      if (!c.passed && c.severity !== 'S1') {
        out.push({ id: `code:${c.name}`, label: `code:${c.name}`, revealLabel: `code:${c.name} (${c.severity} failed)` })
      }
    })
    return out
  }, [judgments, codeChecks])
  const blockIds = React.useMemo(() => new Set(blocks.map(b => b.id)), [blocks])
  const outsideGroups = groups.filter(g => !blockIds.has(g.blockId))
  const s1Failed = (codeChecks ?? []).filter(c => !c.passed && c.severity === 'S1').map(c => c.name)

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
              <span>revision <Hash value={rev?.revision_hash || a.revision_hash || null} /></span>
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
              This article is consequential — a primary AND a different second reviewer must approve the same revision.
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
              {outsideGroups.length > 0 && (
                <div data-testid="judged-outside-body" style={{ marginTop: 10, fontSize: 11, color: colors.textMuted }}>
                  Judged sentences outside the body (package fields — title, excerpt, meta, subjects, preheaders):
                  {outsideGroups.map(g => (
                    <div key={g.key} style={{ marginTop: 4 }}>
                      <span style={mono}>{g.blockId}</span>{' '}
                      <SentenceMark group={g} text={g.sentence || '(sentence not resolved)'} reveal={reveal} selected={selected} onSelect={setSelected} />
                    </div>
                  ))}
                </div>
              )}
            </Panel>

            <CodeChecksPanel checks={codeChecks} />

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
            <EvidencePanel group={selectedGroup} claims={claims} pairs={pairs} reveal={reveal} />
            <ReviewForm
              articleId={articleId}
              revisionHash={rev.revision_hash}
              role={mode}
              blockIds={blocks.map(b => b.id)}
              flag={flag}
              acceptables={acceptables}
              s1Failed={s1Failed}
              reveal={reveal}
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
              <span style={{ color: colors.textFaint, fontStyle: 'italic' }}>none — the pipeline never invents an image</span>
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
// BLOCKS — one renderer per backend block type; every field is shown
// ═══════════════════════════════════════════════════════════════════════════

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

const KNOWN_TYPES = new Set<string>(BLOCK_TYPES)

const BlockView: React.FC<{
  block: Block
  groups: SentenceGroup[]
  reveal: boolean
  selected: string | null
  onSelect: (k: string) => void
  onFlag: (blockId: string) => void
}> = ({ block, groups, reveal, selected, onSelect, onFlag }) => {
  const type = String(block.type ?? '').toLowerCase()
  const parts = blockParts(block)
  const { pieces: segs, unplaced } = segmentPieces(parts.map(p => p.text), groups)
  const html = parts.some(p => containsHtml(p.text))
  const at = (kind: BlockPart['kind']) => parts.map((p, i) => ({ p, i })).filter(x => x.p.kind === kind)
  const heading = at('heading')[0]
  const text = at('text')[0]
  const items = at('item')
  const rows = at('row')
  const caption = at('caption')[0]

  const seg = (i: number) =>
    (segs[i] ?? []).map((s, j) =>
      s.group ? (
        <SentenceMark key={j} group={s.group} text={s.text} reveal={reveal} selected={selected} onSelect={onSelect} />
      ) : (
        <React.Fragment key={j}>{s.text}</React.Fragment>
      ),
    )
  // A table row is ONE unit (cells joined " | "); a judged row is marked whole.
  const rowGroup = (i: number) => (segs[i] ?? []).find(s => s.group != null && s.text === parts[i].text)?.group ?? null

  const headingEl = heading && (
    type === 'stat' ? (
      <div data-testid="stat-figure" style={{ fontSize: 26, fontWeight: 800, color: colors.heading, fontVariantNumeric: 'tabular-nums' }}>{seg(heading.i)}</div>
    ) : (
      <div style={{ fontSize: 16, fontWeight: 700, color: colors.heading, marginBottom: 4 }}>{seg(heading.i)}</div>
    )
  )
  const textEl = text && (
    type === 'pull_quote' ? (
      <blockquote style={{ margin: 0, paddingLeft: 12, borderLeft: `3px solid ${colors.panelBorderStrong}`, color: colors.heading, fontStyle: 'italic', fontSize: 16 }}>
        {seg(text.i)}
      </blockquote>
    ) : (
      <p style={{ margin: '0 0 4px', lineHeight: 1.65, fontSize: type === 'lede' ? 15 : 14 }}>{seg(text.i)}</p>
    )
  )
  let itemsEl: React.ReactNode = null
  if (items.length > 0) {
    if (type === 'faq') {
      itemsEl = (
        <dl data-testid="faq" style={{ margin: 0 }}>
          {items.map((x, k) =>
            k % 2 === 0 ? (
              <dt key={x.i} style={{ fontWeight: 700, marginTop: k ? 8 : 0 }}>{seg(x.i)}</dt>
            ) : (
              <dd key={x.i} style={{ margin: '2px 0 0 14px' }}>{seg(x.i)}</dd>
            ),
          )}
          {items.length % 2 === 1 && (
            <dd style={{ margin: '2px 0 0 14px', color: colors.warningText, fontSize: 11 }}>no answer — faq items must alternate question, answer</dd>
          )}
        </dl>
      )
    } else {
      const List = type === 'steps' ? 'ol' : 'ul'
      itemsEl = <List style={{ margin: 0, paddingLeft: 20, lineHeight: 1.6 }}>{items.map(x => <li key={x.i}>{seg(x.i)}</li>)}</List>
    }
  }
  const rowsEl = rows.length > 0 && (
    <ScrollX>
      <table style={tableStyle}>
        <tbody>
          {rows.map(({ p, i }, ri) => {
            const g = rowGroup(i)
            const c = g ? (reveal ? verdictColor(worstVerdict(g.judgments) ?? '') : colors.indigo400) : null
            return (
              <tr
                key={ri}
                data-testid={g ? 'judged-sentence' : undefined}
                onClick={g ? () => onSelect(g.key) : undefined}
                style={g && c ? { background: alpha(c, selected === g.key ? '33' : '14'), cursor: 'pointer' } : undefined}
              >
                {(p.cells ?? []).map((cell, ci) => (
                  <td key={ci} style={{ ...tdStyle, fontWeight: ri === 0 ? 700 : 400 }}>{cell}</td>
                ))}
              </tr>
            )
          })}
        </tbody>
      </table>
    </ScrollX>
  )
  const calc = block.calc
  const calcEl = calc && (
    <div data-testid="calc" style={{ ...mono, fontSize: 12, margin: '6px 0', padding: '6px 8px', border: `1px solid ${colors.hairline}`, borderRadius: 6 }}>
      {(calc.inputs ?? []).map((inp, k) => <div key={k}>{asText(inp.name)} = {asText(inp.value)}</div>)}
      <div>formula: {asText(calc.formula)}</div>
      <div>result: {asText(calc.result)}</div>
    </div>
  )
  const captionEl = caption && (
    <div style={{ fontSize: 12, color: colors.textMuted, fontStyle: 'italic', marginTop: 4 }}>{seg(caption.i)}</div>
  )
  let body = (
    <>
      {headingEl}
      {textEl}
      {itemsEl}
      {rowsEl}
      {calcEl}
      {captionEl}
    </>
  )
  if (type === 'callout') {
    body = <div style={{ border: `1px solid ${alpha(colors.indigo400, '44')}`, borderLeft: `3px solid ${colors.indigo400}`, borderRadius: 6, padding: '8px 10px' }}>{body}</div>
  }

  return (
    <div data-testid={`block-${block.id}`} style={{ display: 'flex', gap: 10, padding: '10px 0', borderTop: `1px solid ${colors.divider}` }}>
      <div style={{ width: 110, flexShrink: 0, fontSize: 10, color: colors.textFaint }}>
        <div title={`block id ${block.id}`} style={{ ...mono, fontSize: 10, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{block.id}</div>
        <div data-testid="block-type">{type || 'untyped'}</div>
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
        {!KNOWN_TYPES.has(type) && (
          <div style={{ fontSize: 11, color: colors.warningText, marginBottom: 4 }}>
            block type “{type || 'untyped'}” is not in the backend's allowed set — shown generically
          </div>
        )}
        {parts.length === 0 && !calc ? <span style={{ color: colors.textFaint, fontStyle: 'italic' }}>empty block</span> : body}
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
  v ? <span title={v}>{/T/.test(v) ? `${fmtTime(v)} MT` : v}</span> : <Unknown hint="not recorded on the claim" />

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

const EvidencePanel: React.FC<{
  group: SentenceGroup | null
  claims: Map<string, Claim>
  pairs: Map<string, SentencePair>
  reveal: boolean
}> = ({ group, claims, pairs, reveal }) => (
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
          if (j.kind !== 'claim') {
            return (
              <div key={i} style={{ border: `1px solid ${colors.hairline}`, borderRadius: 8, padding: '8px 10px', marginBottom: 8, fontSize: 12 }}>
                {reveal ? (
                  <>
                    <div style={{ display: 'flex', gap: 8, alignItems: 'center', marginBottom: 4 }}>
                      <VerdictChip verdict={j.verdict} /> <span style={mono}>{j.kind}</span> <span style={{ ...mono, color: colors.textFaint }}>{j.id}</span>
                    </div>
                    {j.note && <SafeText value={j.note} />}
                  </>
                ) : (
                  <Pill color={colors.idle} style={{ fontSize: 10, padding: '1px 8px' }}>judge item {j.id} — hidden (blind)</Pill>
                )}
              </div>
            )
          }
          const ck = claimKey(j.claim_id ?? '', j.version ?? 0)
          const c = claims.get(ck)
          const pair = pairs.get(pairKey(j.block_id, j.sentence_idx, j.claim_id ?? '', j.version ?? 0))
          return (
            <div key={i} style={{ border: `1px solid ${colors.hairline}`, borderRadius: 8, padding: '8px 10px', marginBottom: 8 }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', marginBottom: 4 }}>
                {reveal ? <VerdictChip verdict={j.verdict} /> : <Pill color={colors.idle} style={{ fontSize: 10, padding: '1px 8px' }}>verdict hidden</Pill>}
                <span style={{ ...mono, color: colors.textMuted }} title="claim_id @ version">{j.claim_id} v{String(j.version)}</span>
              </div>
              {pair?.stale && (
                <div style={{ fontSize: 12, color: colors.warningText, marginBottom: 4 }}>
                  Stale: this sentence cites v{String(j.version)}; the claim is now at v{pair.latest_version}.
                </div>
              )}
              {reveal && j.lost_qualifier && (
                <div style={{ fontSize: 12, color: colors.warningText, marginBottom: 4 }}>
                  Lost qualifier: <SafeText value={j.lost_qualifier} />
                </div>
              )}
              {reveal && j.note && (
                <div style={{ fontSize: 12, color: colors.textMuted, marginBottom: 4 }}>
                  Judge note: <SafeText value={j.note} />
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

const CodeChecksPanel: React.FC<{ checks: CodeCheck[] | null | undefined }> = ({ checks }) => {
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
          <div key={i} data-testid="code-check" style={{ padding: '4px 0', borderTop: `1px solid ${colors.divider}`, fontSize: 12 }}>
            <div style={{ display: 'flex', gap: 10, alignItems: 'baseline', flexWrap: 'wrap' }}>
              <CheckPill status={c.passed ? 'ok' : c.severity === 'S1' ? 'fail' : 'warn'} label={c.passed ? 'pass' : `fail ${c.severity}`} />
              <span style={{ color: colors.text, minWidth: 140 }}><SafeText value={c.name} /></span>
              <span style={{ color: colors.textFaint, fontSize: 11 }}>{c.severity}{c.heuristic ? ' · heuristic' : ''}</span>
            </div>
            {(c.details ?? []).length > 0 && (
              <ul style={{ margin: '4px 0 0', paddingLeft: 20, color: colors.textMuted, overflowWrap: 'anywhere' }}>
                {(c.details ?? []).map((d, k) => <li key={k}><SafeText value={d} /></li>)}
              </ul>
            )}
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
              <Pill color={colors.indigo400} style={{ fontSize: 10, padding: '1px 8px' }}>{r.role || 'role?'}</Pill>
              <Pill color={decisionColor(r.decision)} style={{ fontSize: 10, padding: '1px 8px' }}>{r.decision || 'decision?'}</Pill>
              {r.reviewer && <span style={{ color: colors.textMuted }}><SafeText value={r.reviewer} /></span>}
              <span style={{ color: colors.textMuted }}>{r.minutes == null ? <Unknown hint="minutes not recorded" /> : `${r.minutes} min`}</span>
              {r.created_at && <span style={{ color: colors.textFaint }}>{fmtTime(r.created_at)} MT</span>}
              <span>rev <Hash value={r.revision_hash} /></span>
              {stale && <span style={{ color: colors.warningText, fontSize: 11 }}>on an older revision</span>}
            </div>
            {(r.accepted_ids ?? []).length > 0 && (
              <div style={{ marginTop: 4, paddingLeft: 6, fontSize: 11, color: colors.textFaint }}>
                accepted: <span style={mono}>{(r.accepted_ids ?? []).join(', ')}</span>
              </div>
            )}
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

const REVIEWER_KEY = 'contentDesk.reviewer'
const readReviewer = (): string => {
  try {
    return window.localStorage.getItem(REVIEWER_KEY) ?? ''
  } catch {
    return ''
  }
}
const saveReviewer = (v: string) => {
  try {
    window.localStorage.setItem(REVIEWER_KEY, v)
  } catch {
    /* storage blocked — the field just is not remembered */
  }
}

const newFinding = (blockId = ''): FindingDraft => ({ severity: 'S2', caught_by: 'human', block_id: blockId, text: '' })

const ReviewForm: React.FC<{
  articleId: string
  revisionHash: string | null
  role: ReviewRole
  blockIds: string[]
  flag: { blockId: string; nonce: number } | null
  acceptables: Acceptable[]
  s1Failed: string[]
  reveal: boolean
  onSubmitted: () => void
  onConflict: () => void
}> = ({ articleId, revisionHash, role, blockIds, flag, acceptables, s1Failed, reveal, onSubmitted, onConflict }) => {
  const toast = useToast()
  const [findings, setFindings] = React.useState<FindingDraft[]>([])
  const [minutes, setMinutes] = React.useState('')
  const [reviewer, setReviewer] = React.useState<string>(readReviewer)
  const [accepted, setAccepted] = React.useState<Set<string>>(() => new Set())
  const [submitting, setSubmitting] = React.useState<ReviewDecision | null>(null)
  const [error, setError] = React.useState<string | null>(null)

  React.useEffect(() => {
    if (flag) setFindings(f => [...f, newFinding(flag.blockId)])
  }, [flag])
  // Acceptances are per revision — a new revision has new items.
  React.useEffect(() => setAccepted(new Set()), [revisionHash])

  const patch = (i: number, p: Partial<FindingDraft>) => setFindings(fs => fs.map((f, k) => (k === i ? { ...f, ...p } : f)))

  const submit = async (decision: ReviewDecision) => {
    setError(null)
    let payload: ReviewPayload
    try {
      payload = buildReviewPayload({ revisionHash, reviewer, role, decision, findings, minutes, acceptedIds: Array.from(accepted) })
    } catch (e) {
      setError(errMsg(e))
      return
    }
    setSubmitting(decision)
    const res = await submitReview(articleId, payload)
    setSubmitting(null)
    if (res.kind === 'ok') {
      saveReviewer(payload.reviewer)
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
              <select aria-label={`Finding ${i + 1} caught by`} value={f.caught_by} onChange={e => patch(i, { caught_by: e.target.value as CaughtBy })} style={{ ...filterInputStyle, width: 110 }}>
                {CAUGHT_BY.map(c => <option key={c} value={c}>{c}</option>)}
              </select>
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
      <button type="button" style={{ ...btnStyle, marginBottom: 12 }} onClick={() => setFindings(fs => [...fs, newFinding()])}>
        <FontAwesomeIcon icon={faPlus} /> Add finding
      </button>

      {acceptables.length > 0 && (
        <div data-testid="accept-list" style={{ marginBottom: 12 }}>
          <div style={{ fontSize: 11, color: colors.textMuted, textTransform: 'uppercase', letterSpacing: 0.4, marginBottom: 2 }}>
            Accept to approve ({accepted.size}/{acceptables.length})
          </div>
          <div style={{ fontSize: 11, color: colors.textFaint, marginBottom: 4 }}>
            An approve is refused while any of these is unaccepted.{reveal ? '' : ' Verdicts stay hidden until your blind review is in.'}
          </div>
          {acceptables.map(x => (
            <label key={x.id} style={{ display: 'flex', gap: 6, alignItems: 'center', fontSize: 12 }}>
              <input
                type="checkbox"
                aria-label={`Accept ${x.id}`}
                checked={accepted.has(x.id)}
                onChange={e => setAccepted(s => {
                  const n = new Set(s)
                  if (e.target.checked) n.add(x.id)
                  else n.delete(x.id)
                  return n
                })}
              />
              <span style={mono}>{reveal ? x.revealLabel : x.label}</span>
            </label>
          ))}
        </div>
      )}
      {s1Failed.length > 0 && (
        <div style={{ fontSize: 12, color: colors.dangerText, marginBottom: 10 }}>
          S1 code check failed ({s1Failed.join(', ')}) — it cannot be accepted; approval needs a new revision.
        </div>
      )}

      <div style={{ display: 'flex', gap: 10, alignItems: 'flex-end', flexWrap: 'wrap' }}>
        <label style={filterFieldLabelStyle}>reviewer *
          <input aria-label="Reviewer" value={reviewer} onChange={e => setReviewer(e.target.value)} placeholder="your name" style={{ ...filterInputStyle, width: 130 }} />
        </label>
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
