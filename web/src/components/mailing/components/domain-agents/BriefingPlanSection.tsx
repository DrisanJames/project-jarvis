// Domain Agents — BRIEFING & PLAN section.
// Generate a briefing/plan for the selected domain, review postures/flags,
// and edit slot copy fields (subject / preheader / creative / offer).

import React, { useCallback, useEffect, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faWandMagicSparkles, faFloppyDisk, faSpinner, faFlag } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../../shared/apiFetch';
import { useToast } from '../../shared/ToastSystem';
import { StatusBadge } from '../../../common/StatusBadge';
import {
  DOMAIN_AGENT_BASE, Plan, PlanSlot, PlanStatus, postureToBadge,
} from './types';
import {
  C, panelStyle, sectionTitleStyle, btnStyle, btnDisabledStyle, inputStyle,
  Loading, ErrorState, fmtInt, fmtPct,
} from './ui';

const STATUS_CHIP_COLORS: Record<PlanStatus, { bg: string; border: string; fg: string }> = {
  draft:     { bg: 'rgba(180,210,240,0.10)', border: 'rgba(180,210,240,0.35)', fg: 'rgba(180,210,240,0.85)' },
  compiled:  { bg: 'rgba(0,229,255,0.10)',   border: 'rgba(0,229,255,0.45)',   fg: C.accent },
  approved:  { bg: 'rgba(0,176,255,0.12)',   border: 'rgba(0,176,255,0.5)',    fg: C.accent2 },
  deployed:  { bg: 'rgba(0,184,148,0.12)',   border: 'rgba(0,184,148,0.5)',    fg: C.success },
  failed:    { bg: 'rgba(233,69,96,0.12)',   border: 'rgba(233,69,96,0.5)',    fg: C.danger },
  cancelled: { bg: 'rgba(120,120,120,0.15)', border: 'rgba(120,120,120,0.4)',  fg: C.muted },
};

export const PlanStatusChip: React.FC<{ status: PlanStatus }> = ({ status }) => {
  const c = STATUS_CHIP_COLORS[status];
  return (
    <span
      style={{
        display: 'inline-block',
        padding: '3px 10px',
        borderRadius: 12,
        fontSize: 11,
        fontWeight: 700,
        letterSpacing: 0.8,
        textTransform: 'uppercase',
        background: c.bg,
        border: `1px solid ${c.border}`,
        color: c.fg,
      }}
    >
      {status}
    </span>
  );
};

function renderGuidance(v: unknown): string {
  if (v == null) return '—';
  if (typeof v === 'string') return v;
  if (typeof v === 'number' || typeof v === 'boolean') return String(v);
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}

interface SlotDraft {
  subject: string;
  preheader: string;
  creative: string;
  offer: string;
}

const draftsFromPlan = (plan: Plan): SlotDraft[] =>
  (plan.slots || []).map(s => ({
    subject: s.subject, preheader: s.preheader, creative: s.creative, offer: s.offer,
  }));

interface Props {
  domain: string;
  plan: Plan | null;
  planMissing: boolean;
  loading: boolean;
  error: string | null;
  onRetry: () => void;
  onPlanChange: (plan: Plan) => void;
}

export const BriefingPlanSection: React.FC<Props> = ({
  domain, plan, planMissing, loading, error, onRetry, onPlanChange,
}) => {
  const { addToast } = useToast();
  const [generating, setGenerating] = useState(false);
  const [saving, setSaving] = useState(false);
  const [drafts, setDrafts] = useState<SlotDraft[]>([]);
  const [dirty, setDirty] = useState(false);

  useEffect(() => {
    if (plan) {
      setDrafts(draftsFromPlan(plan));
      setDirty(false);
    } else {
      setDrafts([]);
      setDirty(false);
    }
  }, [plan]);

  const editable = plan != null && (plan.status === 'draft' || plan.status === 'compiled');

  const handleGenerate = useCallback(async () => {
    setGenerating(true);
    try {
      const res = await apiFetch(`${DOMAIN_AGENT_BASE}/plans/generate`, {
        method: 'POST',
        body: JSON.stringify({ domain }),
      });
      if (res.status === 409) {
        addToast({ type: 'info', title: 'Plan already exists', message: `A plan beyond draft already exists for ${domain} today.` });
        return;
      }
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const created: Plan = await res.json();
      onPlanChange(created);
      addToast({ type: 'success', title: 'Briefing generated', message: `Plan ${created.id} (${created.status}) for ${domain}.` });
    } catch (e) {
      addToast({ type: 'error', title: 'Generate failed', message: e instanceof Error ? e.message : String(e) });
    } finally {
      setGenerating(false);
    }
  }, [domain, addToast, onPlanChange]);

  const updateDraft = (idx: number, patch: Partial<SlotDraft>) => {
    setDrafts(prev => prev.map((d, i) => (i === idx ? { ...d, ...patch } : d)));
    setDirty(true);
  };

  const handleSave = useCallback(async () => {
    if (!plan || !editable) return;
    setSaving(true);
    try {
      const slots: PlanSlot[] = plan.slots.map((s, i) => ({
        ...s,
        subject: drafts[i]?.subject ?? s.subject,
        preheader: drafts[i]?.preheader ?? s.preheader,
        creative: drafts[i]?.creative ?? s.creative,
        offer: drafts[i]?.offer ?? s.offer,
      }));
      const res = await apiFetch(`${DOMAIN_AGENT_BASE}/plans/${encodeURIComponent(plan.id)}`, {
        method: 'PATCH',
        body: JSON.stringify({ slots }),
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const updated: Plan = await res.json();
      onPlanChange(updated);
      addToast({ type: 'success', title: 'Slots saved', message: `Plan ${updated.id} updated.` });
    } catch (e) {
      addToast({ type: 'error', title: 'Save failed', message: e instanceof Error ? e.message : String(e) });
    } finally {
      setSaving(false);
    }
  }, [plan, editable, drafts, addToast, onPlanChange]);

  return (
    <div style={panelStyle}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 12 }}>
        <div style={{ ...sectionTitleStyle, marginBottom: 0 }}>Briefing &amp; Plan</div>
        {plan && <PlanStatusChip status={plan.status} />}
      </div>

      {loading && <Loading label="Loading plan…" />}
      {!loading && error && <ErrorState message={`Failed to load plan: ${error}`} onRetry={onRetry} />}

      {!loading && !error && planMissing && (
        <div style={{ textAlign: 'center', padding: '20px 0' }}>
          <div style={{ color: C.muted, fontSize: 13, marginBottom: 12 }}>
            No plan exists for <span style={{ color: C.text, fontWeight: 600 }}>{domain}</span> today.
          </div>
          <button
            onClick={handleGenerate}
            disabled={generating}
            style={generating ? btnDisabledStyle : btnStyle}
          >
            <FontAwesomeIcon icon={generating ? faSpinner : faWandMagicSparkles} spin={generating} style={{ marginRight: 8 }} />
            {generating ? 'Generating…' : 'Generate briefing'}
          </button>
        </div>
      )}

      {!loading && !error && plan && (
        <>
          {/* Rationale */}
          <p style={{ color: C.text, fontSize: 13, lineHeight: 1.6, margin: '0 0 12px 0' }}>
            {plan.briefing?.rationale || 'No rationale provided.'}
          </p>

          {/* Flags */}
          {plan.briefing?.flags && plan.briefing.flags.length > 0 && (
            <div
              style={{
                background: 'rgba(253,203,110,0.08)',
                border: '1px solid rgba(253,203,110,0.35)',
                borderRadius: 8,
                padding: '10px 14px',
                marginBottom: 12,
              }}
            >
              {plan.briefing.flags.map((f, i) => (
                <div key={i} style={{ color: C.warning, fontSize: 12.5, display: 'flex', gap: 8, alignItems: 'baseline', padding: '2px 0' }}>
                  <FontAwesomeIcon icon={faFlag} style={{ fontSize: 10 }} />
                  <span>{f}</span>
                </div>
              ))}
            </div>
          )}

          {/* Posture grid */}
          {plan.briefing?.postures && plan.briefing.postures.length > 0 && (
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(220px, 1fr))', gap: 10, marginBottom: 12 }}>
              {plan.briefing.postures.map(p => (
                <div
                  key={p.isp}
                  style={{
                    background: 'rgba(10,15,26,0.7)',
                    border: `1px solid ${C.panelBorder}`,
                    borderRadius: 8,
                    padding: '10px 12px',
                  }}
                >
                  <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 6 }}>
                    <span style={{ fontWeight: 700, fontSize: 13, color: C.text }}>{p.isp}</span>
                    <StatusBadge status={postureToBadge(p.posture)} label={p.posture} />
                  </div>
                  <div style={{ fontSize: 12, color: C.muted, lineHeight: 1.5 }}>
                    {fmtInt(p.sends)} sends · open {fmtPct(p.open_pct)} · machine {fmtPct(p.machine_open_share)} · {fmtInt(p.human_clicks)} human clicks
                  </div>
                  {p.reasons && p.reasons.length > 0 && (
                    <ul style={{ margin: '6px 0 0', paddingLeft: 16, color: C.muted, fontSize: 11.5 }}>
                      {p.reasons.map((r, i) => <li key={i}>{r}</li>)}
                    </ul>
                  )}
                </div>
              ))}
            </div>
          )}

          {/* Volume guidance */}
          <div style={{ marginBottom: 14 }}>
            <div style={{ fontSize: 11, fontWeight: 700, letterSpacing: 0.8, textTransform: 'uppercase', color: C.muted, marginBottom: 4 }}>
              Volume guidance
            </div>
            <pre
              style={{
                margin: 0,
                whiteSpace: 'pre-wrap',
                fontFamily: typeof plan.briefing?.volume_guidance === 'string' ? 'inherit' : undefined,
                fontSize: 12.5,
                color: C.text,
                background: 'rgba(10,15,26,0.7)',
                border: `1px solid ${C.panelBorder}`,
                borderRadius: 8,
                padding: '8px 12px',
              }}
            >
              {renderGuidance(plan.briefing?.volume_guidance)}
            </pre>
          </div>

          {/* Draft compile hint */}
          {plan.status === 'draft' && (
            <div
              style={{
                background: 'rgba(0,229,255,0.06)',
                border: '1px solid rgba(0,229,255,0.25)',
                borderRadius: 8,
                padding: '8px 12px',
                marginBottom: 14,
                fontSize: 12,
                color: C.muted,
              }}
            >
              Attach compiled payloads:{' '}
              <code style={{ color: C.accent, fontSize: 12 }}>
                python3 -m agents.domainagent compile --domain {domain} --brief &lt;brief.yaml&gt; --date {plan.plan_date}
              </code>
            </div>
          )}

          {/* Slots */}
          <div style={{ fontSize: 11, fontWeight: 700, letterSpacing: 0.8, textTransform: 'uppercase', color: C.muted, marginBottom: 8 }}>
            Slots ({plan.slots?.length ?? 0})
          </div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
            {(plan.slots || []).map((s, i) => (
              <div
                key={`${s.name}-${i}`}
                style={{
                  background: 'rgba(10,15,26,0.7)',
                  border: `1px solid ${C.panelBorder}`,
                  borderRadius: 8,
                  padding: '12px 14px',
                }}
              >
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', marginBottom: 8, gap: 12 }}>
                  <span style={{ fontWeight: 700, fontSize: 13, color: C.text }}>{s.name}</span>
                  <span style={{ fontSize: 11.5, color: C.muted, textAlign: 'right' }}>
                    {s.scheduled_at} · {s.window_hours}h window
                    {s.target_isps && s.target_isps.length > 0 ? ` · ISPs: ${s.target_isps.join(', ')}` : ''}
                  </span>
                </div>
                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 8 }}>
                  <label style={{ fontSize: 11, color: C.muted }}>
                    Subject
                    <input
                      style={{ ...inputStyle, marginTop: 3 }}
                      value={drafts[i]?.subject ?? ''}
                      disabled={!editable}
                      onChange={e => updateDraft(i, { subject: e.target.value })}
                    />
                  </label>
                  <label style={{ fontSize: 11, color: C.muted }}>
                    Preheader
                    <input
                      style={{ ...inputStyle, marginTop: 3 }}
                      value={drafts[i]?.preheader ?? ''}
                      disabled={!editable}
                      onChange={e => updateDraft(i, { preheader: e.target.value })}
                    />
                  </label>
                  <label style={{ fontSize: 11, color: C.muted }}>
                    Creative
                    <input
                      style={{ ...inputStyle, marginTop: 3 }}
                      value={drafts[i]?.creative ?? ''}
                      disabled={!editable}
                      onChange={e => updateDraft(i, { creative: e.target.value })}
                    />
                  </label>
                  <label style={{ fontSize: 11, color: C.muted }}>
                    Offer
                    <input
                      style={{ ...inputStyle, marginTop: 3 }}
                      value={drafts[i]?.offer ?? ''}
                      disabled={!editable}
                      onChange={e => updateDraft(i, { offer: e.target.value })}
                    />
                  </label>
                </div>
                <div style={{ marginTop: 8, fontSize: 11.5, color: C.muted, lineHeight: 1.5 }}>
                  <div>Inclusion: {s.inclusion_segments?.length ? s.inclusion_segments.join(', ') : '—'}</div>
                  <div>Exclusion: {s.exclusion_lists?.length ? s.exclusion_lists.join(', ') : '—'}</div>
                  {s.campaign_name && <div>Campaign: {s.campaign_name}</div>}
                </div>
              </div>
            ))}
          </div>

          {/* Save */}
          <div style={{ marginTop: 12, display: 'flex', justifyContent: 'flex-end' }}>
            <button
              onClick={handleSave}
              disabled={!editable || !dirty || saving}
              style={!editable || !dirty || saving ? btnDisabledStyle : btnStyle}
              title={editable ? undefined : 'Slots are editable only while the plan is draft or compiled'}
            >
              <FontAwesomeIcon icon={saving ? faSpinner : faFloppyDisk} spin={saving} style={{ marginRight: 8 }} />
              {saving ? 'Saving…' : 'Save slots'}
            </button>
          </div>
        </>
      )}
    </div>
  );
};
