import React, { useState } from 'react';
import { DRY_RUN_ENDPOINT } from './constants';
import { buildPayload, validateInvariants } from './payloadBuilder';
import { datePrefixFromSendDate } from './schedule';
import type { CellDraft } from './types';

interface AuditDrawerProps {
  open: boolean;
  onClose: () => void;
  cells: CellDraft[];
  sendDate: string;
  organizationId: string;
}

interface DryRunResult {
  cell: CellDraft;
  payload: Record<string, unknown>;
  invariantError?: string;
  dryRunResponse?: unknown;
  dryRunStatus?: number;
}

export const AuditDrawer: React.FC<AuditDrawerProps> = ({ open, onClose, cells, sendDate, organizationId }) => {
  const [results, setResults] = useState<DryRunResult[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const runAudit = async () => {
    setLoading(true);
    setError(null);
    try {
      const datePrefix = datePrefixFromSendDate(sendDate);
      const out: DryRunResult[] = [];
      // Bounded concurrency — match the server's audience worker pool (4).
      const queue = cells.slice();
      const workers = Array.from({ length: 4 }, async () => {
        while (queue.length) {
          const cell = queue.shift();
          if (!cell) return;
          const payload = buildPayload(cell, { datePrefix });
          const invariantError = validateInvariants(payload) ?? undefined;
          let dryRunResponse: unknown;
          let dryRunStatus = 0;
          try {
            const r = await fetch(DRY_RUN_ENDPOINT, {
              method: 'POST',
              headers: {
                'Content-Type': 'application/json',
                'X-Organization-ID': organizationId,
              },
              body: JSON.stringify(payload),
            });
            dryRunStatus = r.status;
            dryRunResponse = await r.json().catch(() => null);
          } catch (e) {
            dryRunResponse = { error: String(e) };
            dryRunStatus = 0;
          }
          out.push({
            cell,
            payload: payload as unknown as Record<string, unknown>,
            invariantError,
            dryRunResponse,
            dryRunStatus,
          });
        }
      });
      await Promise.all(workers);
      // Sort by slot then brand for predictable display.
      out.sort((a, b) => {
        const sa = `${a.cell.slot}|${a.cell.brand}`;
        const sb = `${b.cell.slot}|${b.cell.brand}`;
        return sa < sb ? -1 : sa > sb ? 1 : 0;
      });
      setResults(out);
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  };

  const exportAuditJSON = () => {
    if (!results) return;
    const audit = {
      generated_at: new Date().toISOString(),
      send_date: sendDate,
      cell_count: results.length,
      deploys: results.map(r => ({
        slot: r.cell.slot,
        brand: r.cell.brand,
        family: r.cell.family,
        scheduled_at: r.cell.startAtUTC,
        end_at: r.cell.endAtUTC,
        window_hours: r.cell.windowHours,
        invariant_error: r.invariantError ?? null,
        dry_run_status: r.dryRunStatus,
        payload_summary: {
          name: r.payload.name,
          target_isps: r.payload.target_isps,
          isp_quotas: r.payload.isp_quotas,
          inclusion_segments: r.payload.inclusion_segments,
          exclusion_segments: r.payload.exclusion_segments,
          time_span_source: ((r.payload.isp_plans as Array<{ time_spans: Array<{ source: string }> }>)?.[0]?.time_spans?.[0]?.source) ?? 'unknown',
        },
        dry_run_response: r.dryRunResponse,
      })),
    };
    const blob = new Blob([JSON.stringify(audit, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `${sendDate}_mature_send_day_audit.json`;
    a.click();
    URL.revokeObjectURL(url);
  };

  if (!open) return null;
  return (
    <div style={drawerOverlay} onClick={onClose}>
      <div style={drawerPanel} onClick={e => e.stopPropagation()}>
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '14px 18px', borderBottom: '1px solid rgba(0,200,255,0.15)' }}>
          <h2 style={{ margin: 0, fontSize: 15, color: 'rgba(220,235,250,0.95)' }}>
            Audit JSON · {cells.length} cells · {sendDate}
          </h2>
          <div style={{ display: 'flex', gap: 8 }}>
            <button onClick={runAudit} disabled={loading} style={btn('rgba(0,176,255,0.15)', '#00b0ff')}>
              {loading ? 'Running…' : 'Run dry-run for all'}
            </button>
            <button onClick={exportAuditJSON} disabled={!results} style={btn('rgba(0,184,148,0.12)', '#00b894')}>Export JSON</button>
            <button onClick={onClose} style={btn('rgba(180,180,180,0.12)', '#cbd5f5')}>Close</button>
          </div>
        </div>
        <div style={{ flex: 1, overflow: 'auto', padding: 18 }}>
          {error && <div style={{ color: '#e94560', marginBottom: 12 }}>{error}</div>}
          {!results && !loading && (
            <div style={{ color: 'rgba(180,210,240,0.7)' }}>
              Click <strong>Run dry-run for all</strong> to POST every cell payload to <code>{DRY_RUN_ENDPOINT}</code> and accumulate responses below.
            </div>
          )}
          {results?.map((r, i) => (
            <div key={i} style={{ marginBottom: 14, padding: 12, background: 'rgba(13,21,38,0.6)', border: '1px solid rgba(0,200,255,0.1)', borderRadius: 8 }}>
              <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 6 }}>
                <strong style={{ color: '#00e5ff' }}>{r.cell.slot} · {r.cell.brand} · {r.cell.family}</strong>
                <span style={{ fontSize: 11, color: r.dryRunStatus === 200 ? '#00b894' : r.dryRunStatus === 0 ? '#e94560' : '#fdcb6e' }}>
                  HTTP {r.dryRunStatus || 'NETWORK_FAIL'}
                </span>
              </div>
              {r.invariantError && (
                <div style={{ color: '#e94560', fontSize: 12, marginBottom: 6 }}>INVARIANT FAIL: {r.invariantError}</div>
              )}
              <details>
                <summary style={{ cursor: 'pointer', fontSize: 11, color: 'rgba(180,210,240,0.7)' }}>payload</summary>
                <pre style={{ fontSize: 10, color: 'rgba(220,235,250,0.85)', overflow: 'auto', maxHeight: 220 }}>
                  {JSON.stringify(r.payload, null, 2)}
                </pre>
              </details>
              <details>
                <summary style={{ cursor: 'pointer', fontSize: 11, color: 'rgba(180,210,240,0.7)' }}>dry-run response</summary>
                <pre style={{ fontSize: 10, color: 'rgba(220,235,250,0.85)', overflow: 'auto', maxHeight: 220 }}>
                  {JSON.stringify(r.dryRunResponse, null, 2)}
                </pre>
              </details>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
};

function btn(bg: string, fg: string): React.CSSProperties {
  return {
    background: bg, border: `1px solid ${fg}`, color: fg,
    padding: '6px 12px', borderRadius: 6, fontSize: 12, fontWeight: 600, cursor: 'pointer',
  };
}

const drawerOverlay: React.CSSProperties = {
  position: 'fixed', top: 0, left: 0, right: 0, bottom: 0,
  background: 'rgba(0,0,0,0.55)', zIndex: 9999,
  display: 'flex', justifyContent: 'flex-end',
};

const drawerPanel: React.CSSProperties = {
  width: 'min(900px, 95vw)', height: '100%',
  background: '#0d1526', borderLeft: '1px solid rgba(0,200,255,0.2)',
  display: 'flex', flexDirection: 'column',
};
