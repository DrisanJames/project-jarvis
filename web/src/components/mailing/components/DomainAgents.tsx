// Domain Agents — per-domain agentic send planning & approval.
//
// Left rail: sending domains with worst-ISP posture, 7d sends/open%, and a
// plan-today indicator. Main pane (selected domain): SCORECARD (per-ISP
// window aggregates + daily trend), BRIEFING & PLAN (generate / review /
// edit slot copy), APPROVE & DEPLOY (typed-domain confirm gate → live
// PMTA deploy → 10s campaign QA polling).
//
// Backend contract: /api/mailing/domain-agent (see domain-agents/types.ts).

import React, { useCallback, useEffect, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faRotateRight, faSpinner, faRobot } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../shared/apiFetch';
import { useToast } from '../shared/ToastSystem';
import { StatusBadge } from '../../common/StatusBadge';
import { DOMAIN_AGENT_BASE, DomainSummary, Plan, postureToBadge } from './domain-agents/types';
import {
  C, panelStyle, btnStyle, btnDisabledStyle, Loading, ErrorState, fmtInt, fmtPct100,
} from './domain-agents/ui';
import { ScorecardSection } from './domain-agents/ScorecardSection';
import { BriefingPlanSection } from './domain-agents/BriefingPlanSection';
import { ApproveDeploySection } from './domain-agents/ApproveDeploySection';

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

  const fetchDomains = useCallback(async () => {
    setDomainsLoading(true);
    setDomainsError(null);
    try {
      const res = await apiFetch(`${DOMAIN_AGENT_BASE}/domains`);
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data: DomainSummary[] = await res.json();
      const list = Array.isArray(data) ? data : [];
      setDomains(list);
      setSelected(prev => prev && list.some(d => d.domain === prev) ? prev : (list[0]?.domain ?? null));
    } catch (e) {
      setDomainsError(e instanceof Error ? e.message : String(e));
    } finally {
      setDomainsLoading(false);
    }
  }, []);

  useEffect(() => { fetchDomains(); }, [fetchDomains]);

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
            title="Recompute the per-domain scorecard for the last 3 days"
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
                  {d.has_plan_today && (
                    <span
                      title="Plan exists for today"
                      style={{
                        display: 'inline-block', width: 7, height: 7, borderRadius: '50%',
                        background: C.success, marginRight: 7, verticalAlign: 'middle',
                      }}
                    />
                  )}
                  {d.domain}
                </span>
                <StatusBadge status={postureToBadge(d.posture_worst)} label={d.posture_worst} showIcon={false} />
              </div>
              <div style={{ fontSize: 11.5, color: C.muted }}>
                {fmtInt(d.sends_7d)} sends 7d · open {fmtPct100(d.human_open_pct_7d)}
              </div>
            </div>
          );
        })}
      </div>

      {/* ── Main pane ──────────────────────────────────────────────────── */}
      <div style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column', gap: 16 }}>
        {!selected && !domainsLoading && (
          <div style={{ ...panelStyle, color: C.muted, fontSize: 13 }}>
            Select a domain on the left to view its scorecard and plan.
          </div>
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

            <ScorecardSection domain={selected} />

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
          </>
        )}
      </div>
    </div>
  );
};

export default DomainAgents;
