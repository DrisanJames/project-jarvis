// Domain Agents — APPROVE & DEPLOY section.
// Type-the-domain confirm gate → POST approve → poll campaign QA status.

import React, { useCallback, useEffect, useRef, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faRocket, faSpinner, faTriangleExclamation } from '@fortawesome/free-solid-svg-icons';
import { apiFetch } from '../../shared/apiFetch';
import { useToast } from '../../shared/ToastSystem';
import { useAuth } from '../../../../contexts/AuthContext';
import {
  ApproveResponse, ApproveResultRow, CampaignStatusRow, DOMAIN_AGENT_BASE, Plan,
} from './types';
import {
  C, panelStyle, sectionTitleStyle, thStyle, tdStyle, btnStyle, btnDisabledStyle, inputStyle,
  fmtInt,
} from './ui';

// Statuses at which a campaign no longer needs watching.
const SETTLED_STATUSES = new Set(['scheduled', 'sending', 'sent', 'completed', 'failed']);
const POLL_INTERVAL_MS = 10_000;
const MAX_POLLS = 30;

interface Props {
  plan: Plan | null;
  onPlanChange: (plan: Plan) => void;
}

export const ApproveDeploySection: React.FC<Props> = ({ plan, onPlanChange }) => {
  const { addToast } = useToast();
  const { user } = useAuth();

  const [confirmOpen, setConfirmOpen] = useState(false);
  const [typedDomain, setTypedDomain] = useState('');
  const [approving, setApproving] = useState(false);
  const [approveResults, setApproveResults] = useState<ApproveResultRow[] | null>(null);
  const [statusRows, setStatusRows] = useState<CampaignStatusRow[] | null>(null);
  const [statusError, setStatusError] = useState<string | null>(null);
  const [polling, setPolling] = useState(false);

  const pollTimer = useRef<ReturnType<typeof setInterval> | null>(null);
  const pollCount = useRef(0);

  const stopPolling = useCallback(() => {
    if (pollTimer.current) {
      clearInterval(pollTimer.current);
      pollTimer.current = null;
    }
    setPolling(false);
  }, []);

  const fetchStatusOnce = useCallback(async (planId: string): Promise<boolean> => {
    // Returns true when every campaign has settled (or fetch keeps failing softly).
    try {
      const res = await apiFetch(`${DOMAIN_AGENT_BASE}/plans/${encodeURIComponent(planId)}/status`);
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const rows: CampaignStatusRow[] = await res.json();
      setStatusRows(Array.isArray(rows) ? rows : []);
      setStatusError(null);
      return rows.length > 0 && rows.every(r => SETTLED_STATUSES.has(r.status));
    } catch (e) {
      setStatusError(e instanceof Error ? e.message : String(e));
      return false;
    }
  }, []);

  const startPolling = useCallback((planId: string) => {
    stopPolling();
    pollCount.current = 0;
    setPolling(true);
    // Immediate first check, then every 10s up to MAX_POLLS.
    fetchStatusOnce(planId).then(settled => {
      if (settled) { stopPolling(); return; }
      pollTimer.current = setInterval(async () => {
        pollCount.current += 1;
        const done = await fetchStatusOnce(planId);
        if (done || pollCount.current >= MAX_POLLS) stopPolling();
      }, POLL_INTERVAL_MS);
    });
  }, [fetchStatusOnce, stopPolling]);

  // Reset per-plan state; if the plan is already deployed/failed, surface the
  // stored deploy_results and live campaign status immediately.
  useEffect(() => {
    stopPolling();
    setConfirmOpen(false);
    setTypedDomain('');
    setApproveResults(null);
    setStatusRows(null);
    setStatusError(null);
    if (plan && (plan.status === 'deployed' || plan.status === 'failed')) {
      startPolling(plan.id);
    }
    return stopPolling;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [plan?.id, plan?.status]);

  const handleApprove = useCallback(async () => {
    if (!plan) return;
    setApproving(true);
    try {
      const res = await apiFetch(`${DOMAIN_AGENT_BASE}/plans/${encodeURIComponent(plan.id)}/approve`, {
        method: 'POST',
        body: JSON.stringify({ approved_by: user?.email || 'operator' }),
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data: ApproveResponse = await res.json();
      setApproveResults(data.results || []);
      setConfirmOpen(false);
      onPlanChange({ ...plan, status: data.status, deploy_results: (data.results || []) as unknown[] });
      addToast({ type: 'campaign', title: 'Plan approved', message: `Deployment started for ${plan.sending_domain} — monitoring campaign status.` });
      startPolling(plan.id);
    } catch (e) {
      addToast({ type: 'error', title: 'Approve failed', message: e instanceof Error ? e.message : String(e) });
    } finally {
      setApproving(false);
    }
  }, [plan, user, addToast, onPlanChange, startPolling]);

  if (!plan) {
    return (
      <div style={panelStyle}>
        <div style={sectionTitleStyle}>Approve &amp; Deploy</div>
        <div style={{ color: C.muted, fontSize: 13 }}>Generate a plan first — nothing to approve yet.</div>
      </div>
    );
  }

  const payloadCount = plan.payloads?.length ?? 0;
  const canApprove = plan.status === 'compiled' && payloadCount > 0;
  const confirmEnabled = typedDomain.trim() === plan.sending_domain;
  const deployResults = (plan.deploy_results || []) as ApproveResultRow[];
  const showStoredResults = (plan.status === 'deployed' || plan.status === 'failed') && deployResults.length > 0 && !approveResults;

  return (
    <div style={panelStyle}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 12 }}>
        <div style={{ ...sectionTitleStyle, marginBottom: 0 }}>Approve &amp; Deploy</div>
        {plan.approved_by && (
          <span style={{ fontSize: 11.5, color: C.muted }}>
            Approved by {plan.approved_by}{plan.approved_at ? ` at ${plan.approved_at}` : ''}
          </span>
        )}
      </div>

      {/* Approve gate */}
      {plan.status !== 'deployed' && plan.status !== 'failed' && (
        <>
          {!canApprove && (
            <div style={{ color: C.muted, fontSize: 12.5, marginBottom: 10 }}>
              {plan.status !== 'compiled'
                ? `Plan must be compiled before approval (current status: ${plan.status}).`
                : 'Plan is compiled but has no payloads attached — run the compile step first.'}
            </div>
          )}
          {!confirmOpen && (
            <button
              onClick={() => setConfirmOpen(true)}
              disabled={!canApprove}
              style={canApprove ? btnStyle : btnDisabledStyle}
            >
              <FontAwesomeIcon icon={faRocket} style={{ marginRight: 8 }} />
              Approve &amp; deploy {payloadCount > 0 ? `${payloadCount} campaign${payloadCount === 1 ? '' : 's'}` : ''}
            </button>
          )}

          {confirmOpen && canApprove && (
            <div
              style={{
                background: 'rgba(233,69,96,0.08)',
                border: '1px solid rgba(233,69,96,0.4)',
                borderRadius: 8,
                padding: '14px 16px',
              }}
            >
              <div style={{ color: C.danger, fontSize: 13, fontWeight: 600, marginBottom: 8, display: 'flex', alignItems: 'center', gap: 8 }}>
                <FontAwesomeIcon icon={faTriangleExclamation} />
                Deploys {payloadCount} campaign{payloadCount === 1 ? '' : 's'} for {plan.sending_domain} to your mailbox providers
              </div>
              <div style={{ color: C.muted, fontSize: 12, marginBottom: 8 }}>
                Type <span style={{ color: C.text, fontWeight: 700 }}>{plan.sending_domain}</span> to confirm.
              </div>
              <div style={{ display: 'flex', gap: 8 }}>
                <input
                  style={{ ...inputStyle, maxWidth: 280 }}
                  placeholder={plan.sending_domain}
                  value={typedDomain}
                  onChange={e => setTypedDomain(e.target.value)}
                  autoFocus
                />
                <button
                  onClick={handleApprove}
                  disabled={!confirmEnabled || approving}
                  style={
                    !confirmEnabled || approving
                      ? btnDisabledStyle
                      : { ...btnStyle, background: 'rgba(233,69,96,0.25)', border: '1px solid rgba(233,69,96,0.7)', color: '#ffd7de' }
                  }
                >
                  <FontAwesomeIcon icon={approving ? faSpinner : faRocket} spin={approving} style={{ marginRight: 8 }} />
                  {approving ? 'Deploying…' : 'Confirm deploy'}
                </button>
                <button
                  onClick={() => { setConfirmOpen(false); setTypedDomain(''); }}
                  disabled={approving}
                  style={{ ...btnStyle, background: 'rgba(180,180,180,0.10)', border: '1px solid rgba(180,180,180,0.35)' }}
                >
                  Cancel
                </button>
              </div>
            </div>
          )}
        </>
      )}

      {/* Deploy results (from this session's approve, or stored on the plan) */}
      {(approveResults || showStoredResults) && (
        <div style={{ marginTop: 14 }}>
          <div style={{ fontSize: 11, fontWeight: 700, letterSpacing: 0.8, textTransform: 'uppercase', color: C.muted, marginBottom: 6 }}>
            Deploy results
          </div>
          <table style={{ borderCollapse: 'collapse', width: '100%' }}>
            <thead>
              <tr>
                <th style={thStyle}>Name</th>
                <th style={thStyle}>Campaign ID</th>
                <th style={thStyle}>Status</th>
                <th style={thStyle}>Error</th>
              </tr>
            </thead>
            <tbody>
              {(approveResults || deployResults).map((r, i) => (
                <tr key={`${r.name ?? i}-${i}`}>
                  <td style={tdStyle}>{r.name ?? '—'}</td>
                  <td style={{ ...tdStyle, color: C.muted, fontSize: 12 }}>{r.campaign_id ?? '—'}</td>
                  <td style={{ ...tdStyle, color: r.error ? C.danger : C.text }}>{r.status ?? (r.error ? 'error' : '—')}</td>
                  <td style={{ ...tdStyle, color: C.danger, fontSize: 12, whiteSpace: 'normal' }}>{r.error ?? ''}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* Live campaign QA table */}
      {(statusRows || polling || statusError) && (
        <div style={{ marginTop: 14 }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 6 }}>
            <div style={{ fontSize: 11, fontWeight: 700, letterSpacing: 0.8, textTransform: 'uppercase', color: C.muted }}>
              Campaign Status
            </div>
            {polling && (
              <span style={{ fontSize: 11.5, color: C.accent }}>
                <FontAwesomeIcon icon={faSpinner} spin style={{ marginRight: 5 }} />
                watching (every 10s)
              </span>
            )}
          </div>
          {statusError && (
            <div style={{ color: C.danger, fontSize: 12.5, marginBottom: 6 }}>
              Status fetch failed: {statusError}{polling ? ' — retrying on next poll.' : ''}
              {!polling && plan && (
                <button onClick={() => startPolling(plan.id)} style={{ ...btnStyle, marginLeft: 10, padding: '3px 10px', fontSize: 12 }}>
                  Retry
                </button>
              )}
            </div>
          )}
          {statusRows && statusRows.length === 0 && (
            <div style={{ color: C.muted, fontSize: 12.5 }}>No campaigns reported yet.</div>
          )}
          {statusRows && statusRows.length > 0 && (
            <table style={{ borderCollapse: 'collapse', width: '100%' }}>
              <thead>
                <tr>
                  <th style={thStyle}>Name</th>
                  <th style={thStyle}>Status</th>
                  <th style={{ ...thStyle, textAlign: 'right' }}>Recipients</th>
                  <th style={{ ...thStyle, textAlign: 'right' }}>Queued</th>
                </tr>
              </thead>
              <tbody>
                {statusRows.map(r => {
                  const bad = r.status === 'failed' || r.total_recipients === 0;
                  const rowColor = bad ? C.danger : C.text;
                  return (
                    <tr key={r.campaign_id}>
                      <td style={{ ...tdStyle, color: rowColor }}>{r.name}</td>
                      <td style={{ ...tdStyle, color: rowColor, fontWeight: bad ? 700 : 400 }}>{r.status}</td>
                      <td style={{ ...tdStyle, color: rowColor, textAlign: 'right' }}>{fmtInt(r.total_recipients)}</td>
                      <td style={{ ...tdStyle, color: rowColor, textAlign: 'right' }}>{fmtInt(r.queued_count)}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
        </div>
      )}
    </div>
  );
};
