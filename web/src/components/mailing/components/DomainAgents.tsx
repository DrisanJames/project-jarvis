// Domain Agents — per-domain agentic send planning & approval.
//
// Left rail: sending domains with worst-ISP posture, volume verdict chip
// (INCREASE/MAINTAIN/DECREASE from send-baselines), 7d sends/open%, and a
// plan-today indicator. Main pane (selected domain): VOLUME VERDICT banner,
// TODAY-VS-BASELINE pace meter, SCORECARD (per-ISP window aggregates + daily
// trend), OFFER × ISP historical scorecards, BRIEFING & PLAN (generate /
// review / edit slot copy), APPROVE & DEPLOY (typed-domain confirm gate →
// live PMTA deploy → 10s campaign QA polling). With NO domain selected the
// pane shows the cross-domain baselines comparison (absorbed from the old
// standalone Score Cards tab).
//
// Backend contracts: /api/mailing/domain-agent (domain-agents/types.ts) and
// /api/mailing/send-baselines* (domain-agents/BaselinesSection.tsx).

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faRotateRight, faSpinner, faRobot } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import { useToast } from '../shared/ToastSystem';
import { StatusBadge } from '../../common/StatusBadge';
import { DOMAIN_AGENT_BASE, DomainSummary, Plan, postureToBadge } from './domain-agents/types';
import {
  C, panelStyle, btnStyle, btnDisabledStyle, Loading, ErrorState, fmtInt, fmtPct100,
} from './domain-agents/ui';
import { AgentChatSection } from './domain-agents/AgentChatSection';
import { ScorecardSection } from './domain-agents/ScorecardSection';
import { UpcomingSendsSection } from './domain-agents/UpcomingSendsSection';
import { BriefingPlanSection } from './domain-agents/BriefingPlanSection';
import { ApproveDeploySection } from './domain-agents/ApproveDeploySection';
import {
  BaselinesResponse, BaselineVerdictBanner, TodayPaceSection, DomainScorecardHistory,
  AllDomainsBaselines, VerdictChip, verdictByCanonicalDomain, canonicalDomain,
} from './domain-agents/BaselinesSection';

// Local YYYY-MM-DD (operator timezone) — plan_date is a calendar day, not UTC.
const todayLocalISO = (): string => new Date().toLocaleDateString('en-CA');

export const DomainAgents: React.FC = () => {
  const { addToast } = useToast();

  // ── Domain rail state ──────────────────────────────────────────────────
  const [domains, setDomains] = useState<DomainSummary[]>([]);
  const [domainsLoading, setDomainsLoading] = useState(true);
  const [domainsError, setDomainsError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [refreshing, setRefreshing] = useState(false);

  // ── Plan state (today's plan for the selected domain) ──────────────────
  const [plan, setPlan] = useState<Plan | null>(null);
  const [planMissing, setPlanMissing] = useState(false);
  const [planLoading, setPlanLoading] = useState(false);
  const [planError, setPlanError] = useState<string | null>(null);

  // ── Send baselines (one fetch for all domains: rail chips, verdict
  //    banner on the selected domain, and the all-domains comparison view) ─
  const [baselines, setBaselines] = useState<BaselinesResponse | null>(null);
  const [baselinesLoading, setBaselinesLoading] = useState(true);
  const [baselinesError, setBaselinesError] = useState<string | null>(null);

  const fetchDomains = useCallback(async () => {
    setDomainsLoading(true);
    setDomainsError(null);
    try {
      const res = await apiFetch(`${DOMAIN_AGENT_BASE}/domains`);
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data: DomainSummary[] = await res.json();
      const list = Array.isArray(data) ? data : [];
      setDomains(list);
      // No auto-select: with nothing selected the main pane shows the
      // all-domains baselines comparison (old Score Cards tab main view).
      setSelected(prev => prev && list.some(d => d.domain === prev) ? prev : null);
    } catch (e) {
      setDomainsError(e instanceof Error ? e.message : String(e));
    } finally {
      setDomainsLoading(false);
    }
  }, []);

  useEffect(() => { fetchDomains(); }, [fetchDomains]);

  const fetchBaselines = useCallback(async () => {
    setBaselinesLoading(true);
    setBaselinesError(null);
    try {
      const res = await apiFetch('/api/mailing/send-baselines?days=28');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      setBaselines(await res.json() as BaselinesResponse);
    } catch (e) {
      setBaselinesError(e instanceof Error ? e.message : String(e));
    } finally {
      setBaselinesLoading(false);
    }
  }, []);

  useEffect(() => { fetchBaselines(); }, [fetchBaselines]);

  // canonical domain (em.<apex>) → worst verdict across its em.+m. × ISP rows.
  const railVerdicts = useMemo(
    () => verdictByCanonicalDomain(baselines?.rows ?? []),
    [baselines],
  );

  const fetchPlan = useCallback(async (domain: string) => {
    setPlanLoading(true);
    setPlanError(null);
    setPlanMissing(false);
    setPlan(null);
    try {
      const res = await apiFetch(
        `${DOMAIN_AGENT_BASE}/plans?domain=${encodeURIComponent(domain)}&date=${todayLocalISO()}`
      );
      if (res.status === 404) {
        setPlanMissing(true);
        return;
      }
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data: Plan = await res.json();
      setPlan(data);
    } catch (e) {
      setPlanError(e instanceof Error ? e.message : String(e));
    } finally {
      setPlanLoading(false);
    }
  }, []);

  useEffect(() => {
    if (selected) fetchPlan(selected);
  }, [selected, fetchPlan]);

  const handleRefreshScorecard = useCallback(async () => {
    setRefreshing(true);
    try {
      const res = await apiFetch(`${DOMAIN_AGENT_BASE}/scorecard/refresh`, {
        method: 'POST',
        body: JSON.stringify({ days: 3 }),
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data: { status: 'ok' | 'started' } = await res.json();
      addToast({
        type: data.status === 'ok' ? 'success' : 'info',
        title: 'Scorecard refresh',
        message: data.status === 'ok' ? 'Refresh complete (3 days).' : 'Refresh started (3 days) — data will update shortly.',
      });
    } catch (e) {
      addToast({ type: 'error', title: 'Refresh failed', message: e instanceof Error ? e.message : String(e) });
    } finally {
      setRefreshing(false);
    }
  }, [addToast]);

  const handlePlanChange = useCallback((p: Plan) => {
    setPlan(p);
    setPlanMissing(false);
  }, []);

  const selectedSummary = domains.find(d => d.domain === selected);

  return (
    <div style={{ display: 'flex', gap: 16, alignItems: 'flex-start', color: C.text }}>
      {/* ── Left rail: domain list ─────────────────────────────────────── */}
      <div style={{ ...panelStyle, width: 300, flexShrink: 0, padding: 12 }}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 10 }}>
          <span style={{ fontSize: 12, fontWeight: 700, letterSpacing: 1, textTransform: 'uppercase', color: C.accent }}>
            <FontAwesomeIcon icon={faRobot} style={{ marginRight: 6 }} />
            Domains
          </span>
          <button
            onClick={handleRefreshScorecard}
            disabled={refreshing}
            style={refreshing ? { ...btnDisabledStyle, padding: '4px 10px', fontSize: 11.5 } : { ...btnStyle, padding: '4px 10px', fontSize: 11.5 }}
            title="Update the per-domain performance metrics for the last 3 days"
          >
            <FontAwesomeIcon icon={refreshing ? faSpinner : faRotateRight} spin={refreshing} style={{ marginRight: 5 }} />
            Refresh scorecard
          </button>
        </div>

        {domainsLoading && <Loading label="Loading domains…" />}
        {!domainsLoading && domainsError && (
          <ErrorState message={`Failed to load domains: ${domainsError}`} onRetry={fetchDomains} />
        )}
        {!domainsLoading && !domainsError && domains.length === 0 && (
          <div style={{ color: C.muted, fontSize: 13, padding: '8px 2px' }}>No sending domains found.</div>
        )}

        {!domainsLoading && !domainsError && domains.length > 0 && (
          <div
            onClick={() => setSelected(null)}
            style={{
              padding: '10px 10px',
              borderRadius: 8,
              cursor: 'pointer',
              marginBottom: 6,
              background: selected === null ? 'rgba(0,229,255,0.08)' : 'transparent',
              border: selected === null ? '1px solid rgba(0,229,255,0.35)' : '1px solid transparent',
            }}
          >
            <div style={{ fontSize: 13, fontWeight: 600, color: selected === null ? C.accent : C.text }}>
              All domains
            </div>
            <div style={{ fontSize: 11.5, color: C.muted }}>Performance comparison across domains</div>
          </div>
        )}

        {!domainsLoading && !domainsError && domains.map(d => {
          const active = d.domain === selected;
          return (
            <div
              key={d.domain}
              onClick={() => setSelected(d.domain)}
              style={{
                padding: '10px 10px',
                borderRadius: 8,
                cursor: 'pointer',
                marginBottom: 6,
                background: active ? 'rgba(0,229,255,0.08)' : 'transparent',
                border: active ? '1px solid rgba(0,229,255,0.35)' : '1px solid transparent',
              }}
            >
              <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 4 }}>
                <span style={{ fontSize: 13, fontWeight: 600, color: active ? C.accent : C.text }}>
                  {(d.upcoming_campaigns_48h ?? 0) > 0 && (
                    <span
                      title={`${fmtInt(d.upcoming_campaigns_48h)} campaign${d.upcoming_campaigns_48h === 1 ? '' : 's'} scheduled (next 48h) · ${fmtInt(d.upcoming_recipients_48h)} recipients`}
                      style={{
                        display: 'inline-block', width: 7, height: 7, borderRadius: '50%',
                        background: C.success, marginRight: 5, verticalAlign: 'middle',
                      }}
                    />
                  )}
                  {d.has_plan_today && (
                    <span
                      title="Domain-agent plan exists for today"
                      style={{
                        display: 'inline-block', width: 7, height: 7, borderRadius: '50%',
                        background: C.accent, marginRight: 7, verticalAlign: 'middle',
                      }}
                    />
                  )}
                  {d.domain}
                </span>
                <span style={{ display: 'flex', alignItems: 'center', gap: 5, flexShrink: 0 }}>
                  {(() => {
                    const v = railVerdicts.get(canonicalDomain(d.domain));
                    return v ? <VerdictChip verdict={v} compact /> : null;
                  })()}
                  <StatusBadge status={postureToBadge(d.posture_worst)} label={d.posture_worst} showIcon={false} />
                </span>
              </div>
              <div style={{ fontSize: 11.5, color: C.muted }}>
                {fmtInt(d.sends_7d)} sends 7d · open {fmtPct100(d.human_open_pct_7d)}
                {(d.upcoming_campaigns_48h ?? 0) > 0 && (
                  <> · {fmtInt(d.upcoming_campaigns_48h)} sched 48h</>
                )}
              </div>
            </div>
          );
        })}
      </div>

      {/* ── Main pane ──────────────────────────────────────────────────────
           Layout (2026-06-12 restructure): data first. Verdict banner stays
           full-width at the top; below it a responsive 2-column grid puts the
           metrics stack (pace / scorecard / history) on the left and the
           action stack (upcoming sends / briefing-plan / approve-deploy) on
           the right. auto-fit + minmax(480px,1fr) collapses to a single
           column when the pane is too narrow for two 480px tracks (roughly
           <1200px viewports with the 300px rail). AgentChat moved to the
           bottom, full-width — it used to sit on top and push every data
           section below the fold. ─────────────────────────────────────── */}
      <div style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column', gap: 16 }}>
        {!selected && !domainsLoading && (
          <AllDomainsBaselines
            data={baselines}
            loading={baselinesLoading}
            error={baselinesError}
            onRetry={fetchBaselines}
            onSelectDomain={canonical => {
              const match = domains.find(d => canonicalDomain(d.domain) === canonical);
              if (match) setSelected(match.domain);
            }}
          />
        )}

        {selected && (
          <>
            <div style={{ display: 'flex', alignItems: 'baseline', gap: 12 }}>
              <h2 style={{ margin: 0, fontSize: 20, color: C.text }}>{selected}</h2>
              {selectedSummary && (
                <span style={{ fontSize: 12.5, color: C.muted }}>
                  worst posture: {selectedSummary.posture_worst} · {fmtInt(selectedSummary.sends_7d)} sends / 7d
                </span>
              )}
            </div>

            <BaselineVerdictBanner
              domain={selected}
              data={baselines}
              loading={baselinesLoading}
              error={baselinesError}
              onRetry={fetchBaselines}
            />

            <div
              style={{
                display: 'grid',
                gridTemplateColumns: 'repeat(auto-fit, minmax(480px, 1fr))',
                gap: 16,
                alignItems: 'start',
              }}
            >
              {/* Left column — metrics stack */}
              <div style={{ display: 'flex', flexDirection: 'column', gap: 16, minWidth: 0 }}>
                <TodayPaceSection domain={selected} />

                <ScorecardSection domain={selected} />

                <DomainScorecardHistory domain={selected} />
              </div>

              {/* Right column — sends & plan actions */}
              <div style={{ display: 'flex', flexDirection: 'column', gap: 16, minWidth: 0 }}>
                <UpcomingSendsSection domain={selected} />

                <BriefingPlanSection
                  domain={selected}
                  plan={plan}
                  planMissing={planMissing}
                  loading={planLoading}
                  error={planError}
                  onRetry={() => fetchPlan(selected)}
                  onPlanChange={handlePlanChange}
                />

                {!planLoading && !planError && (
                  <ApproveDeploySection plan={plan} onPlanChange={handlePlanChange} />
                )}
              </div>
            </div>
          </>
        )}

        {/* Agent chat — full-width at the bottom so the data sections above
            stay above the fold. */}
        <AgentChatSection domain={selected} />
      </div>
    </div>
  );
};

export default DomainAgents;
