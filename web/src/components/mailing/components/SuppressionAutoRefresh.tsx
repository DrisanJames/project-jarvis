import React, { useState, useEffect, useCallback, useMemo } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import {
  faSync, faPlay, faStop, faPause, faUpload, faSearch,
  faChevronDown, faChevronRight,
  faCheckCircle, faTimesCircle, faExclamationTriangle,
  faSpinner, faCopy, faEye, faTrash, faPlus,
  faTags, faHistory, faShieldAlt,
  faFileImport, faTimes, faToggleOn, faToggleOff,
  faList
} from '@fortawesome/free-solid-svg-icons';
import './SuppressionAutoRefresh.css';

// ============================================================================
// TYPES
// ============================================================================

interface RefreshSource {
  id: string;
  offer_id: string;
  campaign_name: string;
  suppression_url: string;
  source_provider: string;
  ga_suppression_id: string;
  internal_list_id: string | null;
  is_active: boolean;
  refresh_group: string | null;
  priority: number;
  last_refreshed_at: string | null;
  last_refresh_status: string | null;
  last_entry_count: number;
  last_refresh_ms: number | null;
  last_error: string | null;
  notes: string | null;
  created_at: string;
  updated_at: string;
}

interface RefreshCycle {
  id: string;
  started_at: string;
  completed_at: string | null;
  status: string;
  total_sources: number;
  completed_sources: number;
  failed_sources: number;
  skipped_sources: number;
  total_entries_downloaded: number;
  total_new_entries: number;
  avg_download_ms: number;
  triggered_by: string;
  error_message: string | null;
}

interface RefreshLog {
  id: string;
  cycle_id: string;
  source_id: string;
  status: string;
  entries_downloaded: number;
  entries_new: number;
  download_ms: number;
  processing_ms: number;
  error_message: string | null;
  started_at: string;
  completed_at: string | null;
  campaign_name?: string;
  offer_id?: string;
}

interface RefreshGroup {
  name: string;
  description: string;
  is_active: boolean;
  source_count: number;
  active_count?: number;
  created_at: string;
}

interface EngineStatus {
  engine_running: boolean;
  in_refresh_window: boolean;
  current_cycle: RefreshCycle | null;
  last_completed_cycle: {
    id: string;
    completed_at: string;
    total_sources: number;
    completed_sources: number;
    failed_sources: number;
  } | null;
  total_active_sources: number;
  next_window_start: string;
}

export interface SuppressionAutoRefreshProps {
  onBack: () => void;
  animateIn: boolean;
}

// ============================================================================
// HELPERS
// ============================================================================

function formatRelative(dateStr: string | null | undefined): string {
  if (!dateStr) return 'Never';
  const date = new Date(dateStr);
  const now = new Date();
  const diffMs = now.getTime() - date.getTime();
  const diffSec = Math.floor(diffMs / 1000);
  const diffMin = Math.floor(diffSec / 60);
  const diffHr = Math.floor(diffMin / 60);
  const diffDay = Math.floor(diffHr / 24);
  if (diffSec < 60) return 'Just now';
  if (diffMin < 60) return `${diffMin}m ago`;
  if (diffHr < 24) return `${diffHr}h ago`;
  if (diffDay === 1) return 'Yesterday';
  if (diffDay < 7) return `${diffDay}d ago`;
  return date.toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' });
}

function formatDuration(ms: number | null | undefined): string {
  if (ms == null || ms <= 0) return '\u2014';
  if (ms < 1000) return `${ms}ms`;
  const sec = ms / 1000;
  if (sec < 60) return `${sec.toFixed(1)}s`;
  const min = Math.floor(sec / 60);
  const remainSec = Math.floor(sec % 60);
  if (min < 60) return `${min}m ${remainSec}s`;
  const hr = Math.floor(min / 60);
  const remainMin = min % 60;
  return `${hr}h ${remainMin}m`;
}

function formatNumber(n: number | null | undefined): string {
  if (n == null) return '0';
  return n.toLocaleString('en-US');
}

function formatDateTime(dateStr: string | null | undefined): string {
  if (!dateStr) return '\u2014';
  const d = new Date(dateStr);
  return d.toLocaleString('en-US', {
    month: 'short', day: 'numeric', year: 'numeric',
    hour: '2-digit', minute: '2-digit',
  });
}

function getProviderClass(provider: string): string {
  const p = provider.toLowerCase();
  if (p === 'optizmo') return 'optizmo';
  if (p === 'unsubcentral') return 'unsubcentral';
  if (p === 'manual') return 'manual';
  if (p === 'api') return 'api';
  return '';
}

function getStatusBadgeClass(status: string | null): string {
  if (!status) return '';
  const s = status.toLowerCase();
  if (s === 'success' || s === 'completed') return 'success';
  if (s === 'failed' || s === 'error') return 'failed';
  if (s === 'skipped') return 'skipped';
  if (s === 'running' || s === 'in_progress') return 'running';
  return '';
}

function getSuccessRateClass(rate: number): string {
  if (rate >= 95) return 'excellent';
  if (rate >= 80) return 'good';
  if (rate >= 60) return 'warning';
  return 'poor';
}

function truncateUrl(url: string, maxLen: number = 45): string {
  if (!url) return '\u2014';
  if (url.length <= maxLen) return url;
  return url.slice(0, maxLen) + '\u2026';
}

// ============================================================================
// INTERNAL TYPES
// ============================================================================

interface SourcesFilter {
  active?: boolean;
  provider?: string;
  group?: string;
  status?: string;
}

type TabId = 'sources' | 'cycles' | 'groups';

interface ImportResult {
  imported: number;
  updated: number;
  skipped: number;
  errors: number;
  details?: string[];
}

interface TestResult {
  success: boolean;
  entries_found: number;
  sample_entries?: string[];
  download_ms: number;
  error?: string;
}

// ============================================================================
// COMPONENT
// ============================================================================

const SuppressionAutoRefresh: React.FC<SuppressionAutoRefreshProps> = ({
  onBack: _onBack,
  animateIn,
}) => {
  // onBack available via _onBack if needed for future navigation
  void _onBack;
  // -- Tabs --
  const [activeTab, setActiveTab] = useState<TabId>('sources');

  // -- Engine status --
  const [status, setStatus] = useState<EngineStatus | null>(null);

  // -- Sources --
  const [sources, setSources] = useState<RefreshSource[]>([]);
  const [totalSources, setTotalSources] = useState(0);
  const [sourcesPage, setSourcesPage] = useState(1);
  const [sourcesSearch, setSourcesSearch] = useState('');
  const [sourcesFilter, setSourcesFilter] = useState<SourcesFilter>({});
  const [sourcesSort, setSourcesSort] = useState('priority');
  const [selectedSources, setSelectedSources] = useState<Set<string>>(new Set());

  // -- Cycles --
  const [cycles, setCycles] = useState<RefreshCycle[]>([]);
  const [totalCycles, setTotalCycles] = useState(0);
  const [cyclesPage, setCyclesPage] = useState(1);
  const [expandedCycle, setExpandedCycle] = useState<string | null>(null);
  const [cycleLogs, setCycleLogs] = useState<Record<string, RefreshLog[]>>({});

  // -- Groups --
  const [groups, setGroups] = useState<RefreshGroup[]>([]);

  // -- UI --
  const [loading, setLoading] = useState(true);
  const [showImport, setShowImport] = useState(false);
  const [importFile, setImportFile] = useState<File | null>(null);
  const [importing, setImporting] = useState(false);
  const [importResult, setImportResult] = useState<ImportResult | null>(null);
  const [showDetail, setShowDetail] = useState<string | null>(null);
  const [detailSource, setDetailSource] = useState<RefreshSource | null>(null);
  const [detailLogs, setDetailLogs] = useState<RefreshLog[]>([]);
  const [testingSource, setTestingSource] = useState<string | null>(null);
  const [testResult, setTestResult] = useState<TestResult | null>(null);
  // editingSource reserved for future inline-edit mode
  const [, /* editingSource */ setEditingSource] = useState<string | null>(null);
  void setEditingSource;
  const [showCreateGroup, setShowCreateGroup] = useState(false);
  const [newGroupName, setNewGroupName] = useState('');
  const [newGroupDesc, setNewGroupDesc] = useState('');
  const [showBulkDropdown, setShowBulkDropdown] = useState(false);
  const [confirmDeleteSource, setConfirmDeleteSource] = useState<string | null>(null);
  const [confirmDeleteGroup, setConfirmDeleteGroup] = useState<string | null>(null);
  const [dragOver, setDragOver] = useState(false);

  // -- Debounced search --
  const [debouncedSearch, setDebouncedSearch] = useState('');
  useEffect(() => {
    const timer = setTimeout(() => setDebouncedSearch(sourcesSearch), 300);
    return () => clearTimeout(timer);
  }, [sourcesSearch]);

  // ==========================================================================
  // DATA FETCHING
  // ==========================================================================

  const fetchStatus = useCallback(async () => {
    try {
      const res = await fetch('/api/mailing/suppression-refresh/status');
      if (!res.ok) throw new Error(`Status fetch failed: ${res.status}`);
      const data: EngineStatus = await res.json();
      setStatus(data);
    } catch (err) {
      console.error('Failed to fetch engine status:', err);
    }
  }, []);

  const fetchSources = useCallback(async () => {
    try {
      const params = new URLSearchParams();
      params.set('page', String(sourcesPage));
      params.set('limit', '50');
      params.set('sort', sourcesSort);
      if (debouncedSearch) params.set('search', debouncedSearch);
      if (sourcesFilter.active !== undefined) params.set('active', String(sourcesFilter.active));
      if (sourcesFilter.provider) params.set('provider', sourcesFilter.provider);
      if (sourcesFilter.group) params.set('group', sourcesFilter.group);
      if (sourcesFilter.status) params.set('status', sourcesFilter.status);
      const res = await fetch(`/api/mailing/suppression-refresh/sources?${params.toString()}`);
      if (!res.ok) throw new Error(`Sources fetch failed: ${res.status}`);
      const data = await res.json();
      setSources(data.sources || data.data || []);
      setTotalSources(data.total ?? data.count ?? 0);
    } catch (err) {
      console.error('Failed to fetch sources:', err);
    }
  }, [sourcesPage, sourcesSort, debouncedSearch, sourcesFilter]);

  const fetchCycles = useCallback(async () => {
    try {
      const params = new URLSearchParams();
      params.set('page', String(cyclesPage));
      params.set('limit', '20');
      const res = await fetch(`/api/mailing/suppression-refresh/cycles?${params.toString()}`);
      if (!res.ok) throw new Error(`Cycles fetch failed: ${res.status}`);
      const data = await res.json();
      setCycles(data.cycles || data.data || []);
      setTotalCycles(data.total ?? data.count ?? 0);
    } catch (err) {
      console.error('Failed to fetch cycles:', err);
    }
  }, [cyclesPage]);

  const fetchCycleLogs = useCallback(async (cycleId: string) => {
    try {
      const res = await fetch(`/api/mailing/suppression-refresh/cycles/${cycleId}/logs`);
      if (!res.ok) throw new Error(`Cycle logs fetch failed: ${res.status}`);
      const data: RefreshLog[] = await res.json();
      setCycleLogs((prev) => ({ ...prev, [cycleId]: data }));
    } catch (err) {
      console.error('Failed to fetch cycle logs:', err);
    }
  }, []);

  const fetchGroups = useCallback(async () => {
    try {
      const res = await fetch('/api/mailing/suppression-refresh/groups');
      if (!res.ok) throw new Error(`Groups fetch failed: ${res.status}`);
      const data: RefreshGroup[] = await res.json();
      setGroups(data);
    } catch (err) {
      console.error('Failed to fetch groups:', err);
    }
  }, []);

  const fetchSourceDetail = useCallback(async (id: string) => {
    try {
      const res = await fetch(`/api/mailing/suppression-refresh/sources/${id}`);
      if (!res.ok) throw new Error(`Source detail fetch failed: ${res.status}`);
      const data = await res.json();
      setDetailSource(data.source || data);
      setDetailLogs(data.recent_logs || data.logs || []);
    } catch (err) {
      console.error('Failed to fetch source detail:', err);
    }
  }, []);

  // -- Initial load --
  useEffect(() => {
    const init = async () => {
      setLoading(true);
      await Promise.all([fetchStatus(), fetchSources(), fetchGroups()]);
      setLoading(false);
    };
    init();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // -- Poll status when engine is running --
  useEffect(() => {
    if (!status?.engine_running) return;
    const interval = setInterval(fetchStatus, 10_000);
    return () => clearInterval(interval);
  }, [status?.engine_running, fetchStatus]);

  // -- Re-fetch on tab change --
  useEffect(() => {
    if (activeTab === 'sources') fetchSources();
    else if (activeTab === 'cycles') fetchCycles();
    else if (activeTab === 'groups') fetchGroups();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeTab]);

  // -- Re-fetch sources when filters change --
  useEffect(() => {
    if (activeTab === 'sources') fetchSources();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sourcesPage, sourcesSort, debouncedSearch, sourcesFilter]);

  // ==========================================================================
  // ACTIONS
  // ==========================================================================

  const triggerCycle = useCallback(async () => {
    try {
      const res = await fetch('/api/mailing/suppression-refresh/trigger', { method: 'POST' });
      if (!res.ok) throw new Error(`Trigger failed: ${res.status}`);
      await fetchStatus();
    } catch (err) {
      console.error('Failed to trigger cycle:', err);
    }
  }, [fetchStatus]);

  const stopCycle = useCallback(async () => {
    try {
      const res = await fetch('/api/mailing/suppression-refresh/stop', { method: 'POST' });
      if (!res.ok) throw new Error(`Stop failed: ${res.status}`);
      await fetchStatus();
    } catch (err) {
      console.error('Failed to stop cycle:', err);
    }
  }, [fetchStatus]);

  const toggleSource = useCallback(
    async (id: string, currentActive: boolean) => {
      try {
        const res = await fetch(`/api/mailing/suppression-refresh/sources/${id}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ is_active: !currentActive }),
        });
        if (!res.ok) throw new Error(`Toggle failed: ${res.status}`);
        await fetchSources();
      } catch (err) {
        console.error('Failed to toggle source:', err);
      }
    },
    [fetchSources],
  );

  const bulkAction = useCallback(
    async (action: string, group?: string, priority?: number) => {
      try {
        const ids = Array.from(selectedSources);
        if (ids.length === 0) return;
        const res = await fetch('/api/mailing/suppression-refresh/sources/bulk-update', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ ids, action, group, priority }),
        });
        if (!res.ok) throw new Error(`Bulk action failed: ${res.status}`);
        setSelectedSources(new Set());
        setShowBulkDropdown(false);
        await fetchSources();
      } catch (err) {
        console.error('Failed to perform bulk action:', err);
      }
    },
    [selectedSources, fetchSources],
  );

  const testSource = useCallback(async (id: string) => {
    try {
      setTestingSource(id);
      setTestResult(null);
      const res = await fetch(`/api/mailing/suppression-refresh/sources/${id}/test`, {
        method: 'POST',
      });
      if (!res.ok) throw new Error(`Test failed: ${res.status}`);
      const data: TestResult = await res.json();
      setTestResult(data);
    } catch (err) {
      console.error('Failed to test source:', err);
      setTestResult({
        success: false,
        entries_found: 0,
        download_ms: 0,
        error: err instanceof Error ? err.message : 'Unknown error',
      });
    } finally {
      setTestingSource(null);
    }
  }, []);

  const importCSV = useCallback(async () => {
    if (!importFile) return;
    try {
      setImporting(true);
      setImportResult(null);
      const formData = new FormData();
      formData.append('file', importFile);
      const res = await fetch('/api/mailing/suppression-refresh/sources/bulk-import', {
        method: 'POST',
        body: formData,
      });
      if (!res.ok) throw new Error(`Import failed: ${res.status}`);
      const data: ImportResult = await res.json();
      setImportResult(data);
      await fetchSources();
    } catch (err) {
      console.error('Failed to import CSV:', err);
      setImportResult({
        imported: 0,
        updated: 0,
        skipped: 0,
        errors: 1,
        details: [err instanceof Error ? err.message : 'Unknown error'],
      });
    } finally {
      setImporting(false);
    }
  }, [importFile, fetchSources]);

  const deleteSource = useCallback(
    async (id: string) => {
      try {
        const res = await fetch(`/api/mailing/suppression-refresh/sources/${id}`, {
          method: 'DELETE',
        });
        if (!res.ok) throw new Error(`Delete failed: ${res.status}`);
        setConfirmDeleteSource(null);
        if (showDetail === id) {
          setShowDetail(null);
          setDetailSource(null);
        }
        await fetchSources();
      } catch (err) {
        console.error('Failed to delete source:', err);
      }
    },
    [fetchSources, showDetail],
  );

  const createGroup = useCallback(async () => {
    if (!newGroupName.trim()) return;
    try {
      const res = await fetch('/api/mailing/suppression-refresh/groups', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: newGroupName.trim(),
          description: newGroupDesc.trim(),
        }),
      });
      if (!res.ok) throw new Error(`Create group failed: ${res.status}`);
      setNewGroupName('');
      setNewGroupDesc('');
      setShowCreateGroup(false);
      await fetchGroups();
    } catch (err) {
      console.error('Failed to create group:', err);
    }
  }, [newGroupName, newGroupDesc, fetchGroups]);

  const deleteGroup = useCallback(
    async (name: string) => {
      try {
        const res = await fetch(
          `/api/mailing/suppression-refresh/groups/${encodeURIComponent(name)}`,
          { method: 'DELETE' },
        );
        if (!res.ok) throw new Error(`Delete group failed: ${res.status}`);
        setConfirmDeleteGroup(null);
        await fetchGroups();
      } catch (err) {
        console.error('Failed to delete group:', err);
      }
    },
    [fetchGroups],
  );

  const bulkGroupAction = useCallback(
    async (groupName: string, action: 'activate' | 'deactivate') => {
      try {
        const res = await fetch('/api/mailing/suppression-refresh/sources/bulk-update', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ group: groupName, action }),
        });
        if (!res.ok) throw new Error(`Bulk group action failed: ${res.status}`);
        await Promise.all([fetchSources(), fetchGroups()]);
      } catch (err) {
        console.error('Failed to perform group action:', err);
      }
    },
    [fetchSources, fetchGroups],
  );

  const copyToClipboard = useCallback(async (text: string) => {
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      // silent
    }
  }, []);

  // ==========================================================================
  // COMPUTED
  // ==========================================================================

  const statusClass = useMemo((): string => {
    if (!status) return '';
    if (status.engine_running) return 'running';
    if (status.in_refresh_window) return 'idle';
    return 'outside-window';
  }, [status]);

  const statusMessage = useMemo((): string => {
    if (!status) return 'Loading\u2026';
    if (status.engine_running && status.current_cycle) {
      const c = status.current_cycle;
      const done = c.completed_sources + c.failed_sources + c.skipped_sources;
      const pct = c.total_sources > 0 ? Math.round((done / c.total_sources) * 100) : 0;
      return `Refresh in progress \u2014 ${pct}% (${c.completed_sources}/${c.total_sources})`;
    }
    if (status.engine_running) return 'Engine running \u2014 waiting for cycle start';
    if (status.in_refresh_window) return 'Idle \u2014 within refresh window';
    return 'Outside refresh window';
  }, [status]);

  const cycleProgress = useMemo((): number => {
    if (!status?.current_cycle) return 0;
    const c = status.current_cycle;
    if (c.total_sources === 0) return 0;
    return Math.round(
      ((c.completed_sources + c.failed_sources + c.skipped_sources) / c.total_sources) * 100,
    );
  }, [status]);

  const lastCycleSuccessRate = useMemo((): number => {
    const lc = status?.last_completed_cycle;
    if (!lc || lc.total_sources === 0) return 0;
    return Math.round((lc.completed_sources / lc.total_sources) * 100);
  }, [status]);

  const totalSourcesPages = useMemo(
    () => Math.max(1, Math.ceil(totalSources / 50)),
    [totalSources],
  );

  const totalCyclesPages = useMemo(
    () => Math.max(1, Math.ceil(totalCycles / 20)),
    [totalCycles],
  );

  const uniqueProviders = useMemo((): string[] => {
    const s = new Set(sources.map((src) => src.source_provider));
    return Array.from(s).sort();
  }, [sources]);

  const uniqueGroups = useMemo((): string[] => groups.map((g) => g.name).sort(), [groups]);

  const allVisibleSelected = useMemo(
    () => sources.length > 0 && sources.every((s) => selectedSources.has(s.id)),
    [sources, selectedSources],
  );

  const toggleSelectAll = useCallback(() => {
    if (allVisibleSelected) {
      setSelectedSources(new Set());
    } else {
      setSelectedSources(new Set(sources.map((s) => s.id)));
    }
  }, [allVisibleSelected, sources]);

  const toggleSelectSource = useCallback((id: string) => {
    setSelectedSources((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  // ==========================================================================
  // RENDER: STATUS BANNER
  // ==========================================================================

  const renderStatusBanner = (): React.ReactElement => (
    <div className={`sr-status-banner ${statusClass}`}>
      <div className="sr-status-indicator" />
      <div className="sr-status-text">{statusMessage}</div>
      <div className="sr-status-meta">
        {status?.last_completed_cycle && (
          <span>Last completed: {formatRelative(status.last_completed_cycle.completed_at)}</span>
        )}
        {!status?.engine_running && status?.next_window_start && (
          <span> &middot; Next window: {formatRelative(status.next_window_start)}</span>
        )}
      </div>
      {status?.current_cycle && (
        <div className="sr-progress-bar">
          <div className="sr-progress-fill" style={{ width: `${cycleProgress}%` }} />
        </div>
      )}
      <div className="sr-quick-actions">
        <button
          className="sr-btn-primary"
          onClick={triggerCycle}
          disabled={status?.engine_running === true}
        >
          <FontAwesomeIcon icon={faPlay} /> Start Refresh
        </button>
        <button
          className="sr-btn-danger"
          onClick={stopCycle}
          disabled={!status?.engine_running}
        >
          <FontAwesomeIcon icon={faStop} /> Stop
        </button>
      </div>
    </div>
  );

  // ==========================================================================
  // RENDER: STATS ROW
  // ==========================================================================

  const renderStatsRow = (): React.ReactElement => (
    <div className="sr-stats-row">
      <div className="sr-stat-card">
        <span className="sr-stat-value">{formatNumber(status?.total_active_sources ?? 0)}</span>
        <span className="sr-stat-label">Active Sources</span>
      </div>
      <div className="sr-stat-card">
        <span className="sr-stat-value">{formatNumber(totalSources)}</span>
        <span className="sr-stat-label">Total Sources</span>
      </div>
      <div className="sr-stat-card">
        <span className="sr-stat-value">
          <span className={`sr-success-rate ${getSuccessRateClass(lastCycleSuccessRate)}`}>
            {lastCycleSuccessRate}%
          </span>
        </span>
        <span className="sr-stat-label">Last Cycle Success</span>
      </div>
      <div className="sr-stat-card">
        <span className="sr-stat-value">
          {formatRelative(status?.last_completed_cycle?.completed_at)}
        </span>
        <span className="sr-stat-label">Last Completed</span>
      </div>
      <div className="sr-stat-card">
        <span className="sr-stat-value">{groups.length}</span>
        <span className="sr-stat-label">Groups</span>
      </div>
    </div>
  );

  // ==========================================================================
  // RENDER: TABS
  // ==========================================================================

  const renderTabs = (): React.ReactElement => (
    <div className="sr-tabs">
      <button
        className={`sr-tab ${activeTab === 'sources' ? 'active' : ''}`}
        onClick={() => setActiveTab('sources')}
      >
        <FontAwesomeIcon icon={faList} /> Sources
        <span className="sr-tab-badge">{totalSources}</span>
      </button>
      <button
        className={`sr-tab ${activeTab === 'cycles' ? 'active' : ''}`}
        onClick={() => setActiveTab('cycles')}
      >
        <FontAwesomeIcon icon={faHistory} /> Cycle History
        <span className="sr-tab-badge">{totalCycles}</span>
      </button>
      <button
        className={`sr-tab ${activeTab === 'groups' ? 'active' : ''}`}
        onClick={() => setActiveTab('groups')}
      >
        <FontAwesomeIcon icon={faTags} /> Groups
        <span className="sr-tab-badge">{groups.length}</span>
      </button>
    </div>
  );

  // ==========================================================================
  // RENDER: SOURCES TOOLBAR
  // ==========================================================================

  const renderSourcesToolbar = (): React.ReactElement => (
    <div className="sr-toolbar">
      <div className="sr-search-input">
        <FontAwesomeIcon icon={faSearch} className="search-icon" />
        <input
          type="text"
          placeholder="Search by campaign, offer, URL\u2026"
          value={sourcesSearch}
          onChange={(e) => { setSourcesSearch(e.target.value); setSourcesPage(1); }}
        />
        {sourcesSearch && (
          <button
            style={{ background: 'none', border: 'none', color: '#5a6070', cursor: 'pointer', padding: '2px 4px' }}
            onClick={() => { setSourcesSearch(''); setSourcesPage(1); }}
          >
            <FontAwesomeIcon icon={faTimes} />
          </button>
        )}
      </div>

      <div className="sr-filter-group">
        <select
          className="sr-filter-btn"
          value={sourcesFilter.active === undefined ? '' : String(sourcesFilter.active)}
          onChange={(e) => {
            const v = e.target.value;
            setSourcesFilter((prev) => ({ ...prev, active: v === '' ? undefined : v === 'true' }));
            setSourcesPage(1);
          }}
        >
          <option value="">All Status</option>
          <option value="true">Active</option>
          <option value="false">Inactive</option>
        </select>

        <select
          className="sr-filter-btn"
          value={sourcesFilter.provider || ''}
          onChange={(e) => {
            setSourcesFilter((prev) => ({ ...prev, provider: e.target.value || undefined }));
            setSourcesPage(1);
          }}
        >
          <option value="">All Providers</option>
          {uniqueProviders.map((p) => <option key={p} value={p}>{p}</option>)}
        </select>

        <select
          className="sr-filter-btn"
          value={sourcesFilter.group || ''}
          onChange={(e) => {
            setSourcesFilter((prev) => ({ ...prev, group: e.target.value || undefined }));
            setSourcesPage(1);
          }}
        >
          <option value="">All Groups</option>
          {uniqueGroups.map((g) => <option key={g} value={g}>{g}</option>)}
        </select>

        <select
          className="sr-filter-btn"
          value={sourcesFilter.status || ''}
          onChange={(e) => {
            setSourcesFilter((prev) => ({ ...prev, status: e.target.value || undefined }));
            setSourcesPage(1);
          }}
        >
          <option value="">All Refresh Status</option>
          <option value="success">Success</option>
          <option value="failed">Failed</option>
          <option value="skipped">Skipped</option>
          <option value="running">Running</option>
        </select>

        <select
          className="sr-filter-btn"
          value={sourcesSort}
          onChange={(e) => { setSourcesSort(e.target.value); setSourcesPage(1); }}
        >
          <option value="priority">Sort: Priority</option>
          <option value="campaign_name">Sort: Campaign</option>
          <option value="last_refreshed_at">Sort: Last Refreshed</option>
          <option value="last_entry_count">Sort: Entry Count</option>
          <option value="created_at">Sort: Created</option>
        </select>
      </div>

      <div className="sr-toolbar-actions">
        <button
          className="sr-btn-secondary"
          onClick={() => { setShowImport(true); setImportFile(null); setImportResult(null); }}
        >
          <FontAwesomeIcon icon={faFileImport} /> Import CSV
        </button>
        <button className="sr-btn-secondary" onClick={fetchSources}>
          <FontAwesomeIcon icon={faSync} />
        </button>
      </div>
    </div>
  );

  // ==========================================================================
  // RENDER: BULK ACTION BAR
  // ==========================================================================

  const renderBulkActionBar = (): React.ReactElement | null => {
    if (selectedSources.size === 0) return null;
    return (
      <div
        className="sr-toolbar"
        style={{ background: 'rgba(79, 195, 247, 0.06)', borderColor: 'rgba(79, 195, 247, 0.15)' }}
      >
        <span style={{ color: '#4fc3f7', fontWeight: 600, fontSize: 13 }}>
          {selectedSources.size} selected
        </span>
        <div className="sr-filter-group">
          <button
            className="sr-btn-primary"
            onClick={() => bulkAction('activate')}
            style={{ fontSize: 12, padding: '6px 14px' }}
          >
            <FontAwesomeIcon icon={faToggleOn} /> Activate All
          </button>
          <button
            className="sr-btn-secondary"
            onClick={() => bulkAction('deactivate')}
            style={{ fontSize: 12, padding: '6px 14px' }}
          >
            <FontAwesomeIcon icon={faToggleOff} /> Deactivate All
          </button>
          <div style={{ position: 'relative' }}>
            <button
              className="sr-btn-secondary"
              onClick={() => setShowBulkDropdown(!showBulkDropdown)}
              style={{ fontSize: 12, padding: '6px 14px' }}
            >
              <FontAwesomeIcon icon={faTags} /> Assign Group <FontAwesomeIcon icon={faChevronDown} />
            </button>
            {showBulkDropdown && (
              <div style={{
                position: 'absolute', top: '100%', left: 0, marginTop: 4,
                background: '#22262e', border: '1px solid rgba(255,255,255,0.1)',
                borderRadius: 8, padding: 8, zIndex: 50, minWidth: 180,
                boxShadow: '0 8px 24px rgba(0,0,0,0.4)',
              }}>
                {uniqueGroups.length === 0 && (
                  <span style={{ color: '#5a6070', fontSize: 12, padding: 8, display: 'block' }}>
                    No groups yet
                  </span>
                )}
                {uniqueGroups.map((g) => (
                  <button
                    key={g}
                    style={{
                      display: 'block', width: '100%', textAlign: 'left',
                      padding: '8px 12px', background: 'transparent', border: 'none',
                      color: '#c0c4cc', fontSize: 13, cursor: 'pointer', borderRadius: 4,
                    }}
                    onMouseEnter={(e) => { (e.currentTarget as HTMLButtonElement).style.background = '#2a2e38'; }}
                    onMouseLeave={(e) => { (e.currentTarget as HTMLButtonElement).style.background = 'transparent'; }}
                    onClick={() => bulkAction('assign_group', g)}
                  >
                    {g}
                  </button>
                ))}
              </div>
            )}
          </div>
          <button
            className="sr-btn-danger"
            onClick={() => bulkAction('delete')}
            style={{ fontSize: 12, padding: '6px 14px' }}
          >
            <FontAwesomeIcon icon={faTrash} /> Delete
          </button>
        </div>
        <button
          className="sr-btn-secondary"
          onClick={() => setSelectedSources(new Set())}
          style={{ marginLeft: 'auto', fontSize: 12, padding: '6px 14px' }}
        >
          Clear Selection
        </button>
      </div>
    );
  };

  // ==========================================================================
  // RENDER: PAGINATION HELPER
  // ==========================================================================

  const renderPagination = (
    currentPage: number,
    totalPages: number,
    totalItems: number,
    perPage: number,
    setPage: React.Dispatch<React.SetStateAction<number>>,
  ): React.ReactElement => {
    const start = Math.min((currentPage - 1) * perPage + 1, totalItems);
    const end = Math.min(currentPage * perPage, totalItems);
    const pageNumbers: number[] = [];
    const maxVisible = 5;
    if (totalPages <= maxVisible) {
      for (let i = 1; i <= totalPages; i++) pageNumbers.push(i);
    } else if (currentPage <= 3) {
      for (let i = 1; i <= maxVisible; i++) pageNumbers.push(i);
    } else if (currentPage >= totalPages - 2) {
      for (let i = totalPages - maxVisible + 1; i <= totalPages; i++) pageNumbers.push(i);
    } else {
      for (let i = currentPage - 2; i <= currentPage + 2; i++) pageNumbers.push(i);
    }
    return (
      <div style={{
        display: 'flex', justifyContent: 'space-between', alignItems: 'center',
        padding: '14px 0', fontSize: 13, color: '#8b919c',
      }}>
        <span>Showing {start}&ndash;{end} of {formatNumber(totalItems)}</span>
        <div style={{ display: 'flex', gap: 6 }}>
          <button
            className="sr-btn-secondary"
            disabled={currentPage <= 1}
            onClick={() => setPage((p) => Math.max(1, p - 1))}
            style={{ padding: '6px 12px', fontSize: 12 }}
          >
            Previous
          </button>
          {pageNumbers.map((pg) => (
            <button
              key={pg}
              className={`sr-filter-btn ${pg === currentPage ? 'active' : ''}`}
              onClick={() => setPage(pg)}
              style={{ padding: '6px 10px', fontSize: 12, minWidth: 32 }}
            >
              {pg}
            </button>
          ))}
          <button
            className="sr-btn-secondary"
            disabled={currentPage >= totalPages}
            onClick={() => setPage((p) => Math.min(totalPages, p + 1))}
            style={{ padding: '6px 12px', fontSize: 12 }}
          >
            Next
          </button>
        </div>
      </div>
    );
  };

  // ==========================================================================
  // RENDER: SOURCES TABLE
  // ==========================================================================

  const renderSourcesTable = (): React.ReactElement => {
    if (sources.length === 0 && !loading) {
      const hasFilters =
        debouncedSearch ||
        sourcesFilter.active !== undefined ||
        sourcesFilter.provider ||
        sourcesFilter.group ||
        sourcesFilter.status;
      return (
        <div className="sr-empty-state">
          <FontAwesomeIcon icon={faShieldAlt} className="empty-icon" />
          <h4>No Sources Found</h4>
          <p>
            {hasFilters
              ? 'No sources match your current filters. Try adjusting your search or filters.'
              : 'Import suppression sources via CSV or add them through the API to get started.'}
          </p>
          {!hasFilters && (
            <button
              className="sr-btn-primary"
              onClick={() => { setShowImport(true); setImportFile(null); setImportResult(null); }}
            >
              <FontAwesomeIcon icon={faFileImport} /> Import Sources
            </button>
          )}
        </div>
      );
    }

    return (
      <>
        <div style={{ overflowX: 'auto' }}>
          <table className="sr-source-table">
            <thead>
              <tr>
                <th style={{ width: 40 }}>
                  <input
                    type="checkbox"
                    className="sr-checkbox"
                    checked={allVisibleSelected}
                    onChange={toggleSelectAll}
                  />
                </th>
                <th style={{ width: 60 }}>Active</th>
                <th>Offer ID</th>
                <th>Campaign Name</th>
                <th>Provider</th>
                <th>URL</th>
                <th>Entry Count</th>
                <th>Last Refreshed</th>
                <th>Status</th>
                <th>Group</th>
                <th style={{ width: 120 }}>Actions</th>
              </tr>
            </thead>
            <tbody>
              {sources.map((source) => (
                <tr key={source.id} className={selectedSources.has(source.id) ? 'selected' : ''}>
                  <td>
                    <input
                      type="checkbox"
                      className="sr-checkbox"
                      checked={selectedSources.has(source.id)}
                      onChange={() => toggleSelectSource(source.id)}
                    />
                  </td>
                  <td>
                    <label className="sr-toggle">
                      <input
                        type="checkbox"
                        checked={source.is_active}
                        onChange={() => toggleSource(source.id, source.is_active)}
                      />
                      <span className="slider" />
                    </label>
                  </td>
                  <td style={{ fontWeight: 600, color: '#e4e6ea' }}>{source.offer_id}</td>
                  <td style={{ color: '#e4e6ea' }}>{source.campaign_name}</td>
                  <td>
                    <span className={`sr-provider-badge ${getProviderClass(source.source_provider)}`}>
                      {source.source_provider}
                    </span>
                  </td>
                  <td>
                    <div className="sr-url-cell">
                      <span title={source.suppression_url}>{truncateUrl(source.suppression_url)}</span>
                      <button title="Copy URL" onClick={() => copyToClipboard(source.suppression_url)}>
                        <FontAwesomeIcon icon={faCopy} />
                      </button>
                    </div>
                  </td>
                  <td style={{ fontWeight: 600 }}>{formatNumber(source.last_entry_count)}</td>
                  <td style={{ color: '#8b919c' }}>
                    {formatRelative(source.last_refreshed_at)}
                    {source.last_refresh_ms != null && (
                      <span style={{ display: 'block', fontSize: 11, color: '#5a6070' }}>
                        {formatDuration(source.last_refresh_ms)}
                      </span>
                    )}
                  </td>
                  <td>
                    <span className={`sr-status-badge ${getStatusBadgeClass(source.last_refresh_status)}`}>
                      {source.last_refresh_status === 'success' && <FontAwesomeIcon icon={faCheckCircle} />}
                      {source.last_refresh_status === 'failed' && <FontAwesomeIcon icon={faTimesCircle} />}
                      {source.last_refresh_status === 'running' && <FontAwesomeIcon icon={faSpinner} spin />}
                      {source.last_refresh_status === 'skipped' && <FontAwesomeIcon icon={faPause} />}
                      {' '}{source.last_refresh_status || 'Never'}
                    </span>
                  </td>
                  <td>
                    {source.refresh_group ? (
                      <span className="sr-group-chip">
                        <FontAwesomeIcon icon={faTags} /> {source.refresh_group}
                      </span>
                    ) : (
                      <span style={{ color: '#3a3f4a' }}>{'\u2014'}</span>
                    )}
                  </td>
                  <td>
                    <div className="sr-actions-cell">
                      <button
                        title="Test download"
                        onClick={() => testSource(source.id)}
                        disabled={testingSource === source.id}
                      >
                        {testingSource === source.id
                          ? <FontAwesomeIcon icon={faSpinner} spin />
                          : <FontAwesomeIcon icon={faSync} />}
                      </button>
                      <button
                        title="View details"
                        onClick={() => { setShowDetail(source.id); setTestResult(null); fetchSourceDetail(source.id); }}
                      >
                        <FontAwesomeIcon icon={faEye} />
                      </button>
                      {confirmDeleteSource === source.id ? (
                        <>
                          <button className="danger" title="Confirm delete" onClick={() => deleteSource(source.id)} style={{ color: '#ef5350' }}>
                            <FontAwesomeIcon icon={faCheckCircle} />
                          </button>
                          <button title="Cancel" onClick={() => setConfirmDeleteSource(null)}>
                            <FontAwesomeIcon icon={faTimes} />
                          </button>
                        </>
                      ) : (
                        <button className="danger" title="Delete source" onClick={() => setConfirmDeleteSource(source.id)}>
                          <FontAwesomeIcon icon={faTrash} />
                        </button>
                      )}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {totalSourcesPages > 1 && renderPagination(sourcesPage, totalSourcesPages, totalSources, 50, setSourcesPage)}
      </>
    );
  };

  const renderSourcesTab = (): React.ReactElement => (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
      {renderSourcesToolbar()}
      {renderBulkActionBar()}
      {renderSourcesTable()}
    </div>
  );

  // ==========================================================================
  // RENDER: CYCLE LOG TABLE
  // ==========================================================================

  const renderCycleLogTable = (cycleId: string): React.ReactElement => {
    const logs = cycleLogs[cycleId];
    if (!logs) {
      return (
        <div className="sr-cycle-expand">
          <div className="sr-loading"><span>Loading logs&hellip;</span></div>
        </div>
      );
    }
    if (logs.length === 0) {
      return (
        <div className="sr-cycle-expand">
          <div style={{ color: '#5a6070', fontSize: 13, padding: '12px 0', textAlign: 'center' }}>
            No log entries for this cycle
          </div>
        </div>
      );
    }
    const thStyle: React.CSSProperties = {
      padding: '8px 10px', textAlign: 'left', color: '#8b919c',
      fontSize: 11, fontWeight: 600, textTransform: 'uppercase',
      letterSpacing: 0.4, borderBottom: '1px solid rgba(255,255,255,0.06)',
      background: '#1e2128', position: 'sticky', top: 0,
    };
    const tdStyle: React.CSSProperties = {
      padding: '8px 10px', borderBottom: '1px solid rgba(255,255,255,0.03)',
    };
    return (
      <div className="sr-cycle-expand">
        <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 12 }}>
          <thead>
            <tr>
              <th style={thStyle}>Source</th>
              <th style={thStyle}>Status</th>
              <th style={{ ...thStyle, textAlign: 'right' }}>Entries</th>
              <th style={{ ...thStyle, textAlign: 'right' }}>New</th>
              <th style={{ ...thStyle, textAlign: 'right' }}>Duration</th>
              <th style={thStyle}>Error</th>
            </tr>
          </thead>
          <tbody>
            {logs.map((log) => (
              <tr key={log.id}>
                <td style={{ ...tdStyle, color: '#c0c4cc' }}>
                  <div style={{ fontWeight: 500 }}>{log.campaign_name || log.source_id}</div>
                  {log.offer_id && <div style={{ fontSize: 11, color: '#5a6070' }}>{log.offer_id}</div>}
                </td>
                <td style={tdStyle}>
                  <span className={`sr-status-badge ${getStatusBadgeClass(log.status)}`}>{log.status}</span>
                </td>
                <td style={{ ...tdStyle, textAlign: 'right', color: '#c0c4cc', fontWeight: 600 }}>
                  {formatNumber(log.entries_downloaded)}
                </td>
                <td style={{ ...tdStyle, textAlign: 'right', color: log.entries_new > 0 ? '#81c784' : '#5a6070', fontWeight: 600 }}>
                  {log.entries_new > 0 ? `+${formatNumber(log.entries_new)}` : '0'}
                </td>
                <td style={{ ...tdStyle, textAlign: 'right', color: '#8b919c' }}>
                  {formatDuration(log.download_ms + log.processing_ms)}
                </td>
                <td style={{ ...tdStyle, color: '#ef9a9a', fontSize: 11, maxWidth: 200, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={log.error_message || ''}>
                  {log.error_message || '\u2014'}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    );
  };

  // ==========================================================================
  // RENDER: CYCLES TAB
  // ==========================================================================

  const renderCyclesTab = (): React.ReactElement => {
    if (cycles.length === 0 && !loading) {
      return (
        <div className="sr-empty-state">
          <FontAwesomeIcon icon={faHistory} className="empty-icon" />
          <h4>No Refresh Cycles Yet</h4>
          <p>Start your first suppression refresh cycle to begin tracking history. Cycles run automatically during configured refresh windows.</p>
          <button className="sr-btn-primary" onClick={triggerCycle} disabled={status?.engine_running === true}>
            <FontAwesomeIcon icon={faPlay} /> Start First Cycle
          </button>
        </div>
      );
    }
    return (
      <div className="sr-cycle-history">
        {cycles.map((cycle) => {
          const isExpanded = expandedCycle === cycle.id;
          const successPct = cycle.total_sources > 0 ? Math.round((cycle.completed_sources / cycle.total_sources) * 100) : 0;
          const failPct = cycle.total_sources > 0 ? Math.round((cycle.failed_sources / cycle.total_sources) * 100) : 0;
          const skipPct = cycle.total_sources > 0 ? Math.round((cycle.skipped_sources / cycle.total_sources) * 100) : 0;
          const durationMs = cycle.completed_at
            ? new Date(cycle.completed_at).getTime() - new Date(cycle.started_at).getTime()
            : null;
          return (
            <div key={cycle.id} className="sr-cycle-card">
              <div
                className="sr-cycle-header"
                onClick={() => {
                  if (isExpanded) { setExpandedCycle(null); }
                  else { setExpandedCycle(cycle.id); if (!cycleLogs[cycle.id]) fetchCycleLogs(cycle.id); }
                }}
              >
                <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
                  <FontAwesomeIcon icon={isExpanded ? faChevronDown : faChevronRight} style={{ color: '#5a6070', width: 12 }} />
                  <span className="cycle-date">{formatDateTime(cycle.started_at)}</span>
                  <span className="cycle-duration">{durationMs != null ? formatDuration(durationMs) : 'In progress\u2026'}</span>
                </div>
                <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
                  <span className={`sr-status-badge ${getStatusBadgeClass(cycle.status)}`}>
                    {cycle.status === 'running' && <FontAwesomeIcon icon={faSpinner} spin />}
                    {cycle.status === 'completed' && <FontAwesomeIcon icon={faCheckCircle} />}
                    {cycle.status === 'failed' && <FontAwesomeIcon icon={faTimesCircle} />}
                    {' '}{cycle.status}
                  </span>
                  <span className={`sr-success-rate ${getSuccessRateClass(successPct)}`}>{successPct}%</span>
                </div>
              </div>
              <div className="sr-cycle-stats">
                <span className="cycle-stat"><strong>{cycle.completed_sources}</strong>/{cycle.total_sources} completed</span>
                <span className="cycle-stat"><strong style={{ color: cycle.failed_sources > 0 ? '#ef9a9a' : undefined }}>{cycle.failed_sources}</strong> failed</span>
                <span className="cycle-stat"><strong>{cycle.skipped_sources}</strong> skipped</span>
                <span className="cycle-stat"><strong>{formatNumber(cycle.total_entries_downloaded)}</strong> entries</span>
                <span className="cycle-stat"><strong>{formatNumber(cycle.total_new_entries)}</strong> new</span>
                <span className="cycle-stat">Avg: <strong>{formatDuration(cycle.avg_download_ms)}</strong></span>
                <span className="cycle-stat" style={{ color: '#5a6070' }}>By: {cycle.triggered_by}</span>
              </div>
              {/* Multi-segment progress bar */}
              <div style={{ padding: '0 20px 14px' }}>
                <div className="sr-progress-bar" style={{ height: 5 }}>
                  <div style={{ display: 'flex', height: '100%', borderRadius: 3, overflow: 'hidden' }}>
                    <div style={{ width: `${successPct}%`, background: '#66bb6a', transition: 'width 0.4s' }} />
                    <div style={{ width: `${failPct}%`, background: '#ef5350', transition: 'width 0.4s' }} />
                    <div style={{ width: `${skipPct}%`, background: '#5a6070', transition: 'width 0.4s' }} />
                  </div>
                </div>
              </div>
              {cycle.error_message && (
                <div style={{ padding: '10px 20px', background: 'rgba(239,83,80,0.06)', borderTop: '1px solid rgba(239,83,80,0.1)', fontSize: 12, color: '#ef9a9a' }}>
                  <FontAwesomeIcon icon={faExclamationTriangle} /> {cycle.error_message}
                </div>
              )}
              {isExpanded && renderCycleLogTable(cycle.id)}
            </div>
          );
        })}
        {totalCyclesPages > 1 && renderPagination(cyclesPage, totalCyclesPages, totalCycles, 20, setCyclesPage)}
      </div>
    );
  };

  // ==========================================================================
  // RENDER: GROUPS TAB
  // ==========================================================================

  const renderGroupsTab = (): React.ReactElement => (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
      <div className="sr-groups-panel">
        <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
          <h3>Refresh Groups</h3>
          <button className="sr-btn-primary" onClick={() => setShowCreateGroup(!showCreateGroup)} style={{ fontSize: 12, padding: '7px 14px' }}>
            <FontAwesomeIcon icon={faPlus} /> New Group
          </button>
        </div>
        {showCreateGroup && (
          <div className="sr-group-create" style={{ flexDirection: 'column', gap: 10, paddingTop: 14 }}>
            <input type="text" placeholder="Group name" value={newGroupName} onChange={(e) => setNewGroupName(e.target.value)} />
            <input type="text" placeholder="Description (optional)" value={newGroupDesc} onChange={(e) => setNewGroupDesc(e.target.value)} />
            <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
              <button className="sr-btn-secondary" onClick={() => { setShowCreateGroup(false); setNewGroupName(''); setNewGroupDesc(''); }} style={{ fontSize: 12, padding: '6px 14px' }}>Cancel</button>
              <button className="sr-btn-primary" onClick={createGroup} disabled={!newGroupName.trim()} style={{ fontSize: 12, padding: '6px 14px' }}>Create Group</button>
            </div>
          </div>
        )}
      </div>

      {groups.length === 0 && !loading ? (
        <div className="sr-empty-state">
          <FontAwesomeIcon icon={faTags} className="empty-icon" />
          <h4>No Groups Created</h4>
          <p>Groups let you organize suppression sources and manage them in bulk. Create your first group to get started.</p>
          <button className="sr-btn-primary" onClick={() => setShowCreateGroup(true)}>
            <FontAwesomeIcon icon={faPlus} /> Create First Group
          </button>
        </div>
      ) : (
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(340px, 1fr))', gap: 14 }}>
          {groups.map((group) => (
            <div key={group.name} className="sr-group-card" style={{ flexDirection: 'column', alignItems: 'stretch', cursor: 'default' }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
                <FontAwesomeIcon icon={faTags} style={{ color: '#ce93d8', fontSize: 14 }} />
                <span className="group-name">{group.name}</span>
                <div className="sr-group-stats" style={{ marginLeft: 'auto' }}>
                  <span><span className="count">{group.source_count}</span> sources</span>
                  {group.active_count !== undefined && (
                    <span><span className="count" style={{ color: '#66bb6a' }}>{group.active_count}</span> active</span>
                  )}
                </div>
              </div>
              {group.description && (
                <div style={{ fontSize: 12, color: '#8b919c', marginTop: 6, lineHeight: 1.5 }}>{group.description}</div>
              )}
              <div style={{ display: 'flex', gap: 8, marginTop: 10, paddingTop: 10, borderTop: '1px solid rgba(255,255,255,0.04)' }}>
                <button className="sr-btn-secondary" onClick={() => bulkGroupAction(group.name, 'activate')} style={{ fontSize: 11, padding: '5px 10px' }}>
                  <FontAwesomeIcon icon={faToggleOn} /> Activate All
                </button>
                <button className="sr-btn-secondary" onClick={() => bulkGroupAction(group.name, 'deactivate')} style={{ fontSize: 11, padding: '5px 10px' }}>
                  <FontAwesomeIcon icon={faToggleOff} /> Deactivate All
                </button>
                <div style={{ marginLeft: 'auto' }}>
                  {confirmDeleteGroup === group.name ? (
                    <div style={{ display: 'flex', gap: 6 }}>
                      <button className="sr-btn-danger" onClick={() => deleteGroup(group.name)} style={{ fontSize: 11, padding: '5px 10px' }}>Confirm</button>
                      <button className="sr-btn-secondary" onClick={() => setConfirmDeleteGroup(null)} style={{ fontSize: 11, padding: '5px 10px' }}>Cancel</button>
                    </div>
                  ) : (
                    <button className="sr-btn-secondary" onClick={() => setConfirmDeleteGroup(group.name)} style={{ fontSize: 11, padding: '5px 10px', color: '#ef5350' }}>
                      <FontAwesomeIcon icon={faTrash} />
                    </button>
                  )}
                </div>
              </div>
              <div style={{ fontSize: 11, color: '#5a6070', marginTop: 6 }}>Created {formatRelative(group.created_at)}</div>
            </div>
          ))}
        </div>
      )}
    </div>
  );

  // ==========================================================================
  // RENDER: IMPORT MODAL
  // ==========================================================================

  const handleFileDrop = useCallback((e: React.DragEvent) => {
    e.preventDefault();
    e.stopPropagation();
    setDragOver(false);
    const files = e.dataTransfer.files;
    if (files.length > 0) {
      const file = files[0];
      if (file.name.endsWith('.csv') || file.type === 'text/csv') {
        setImportFile(file);
      }
    }
  }, []);

  const openFilePicker = useCallback(() => {
    const input = document.createElement('input');
    input.type = 'file';
    input.accept = '.csv';
    input.onchange = (ev: Event) => {
      const target = ev.target as HTMLInputElement;
      if (target.files && target.files.length > 0) {
        setImportFile(target.files[0]);
      }
    };
    input.click();
  }, []);

  const renderImportModal = (): React.ReactElement => (
    <>
      <div className="sr-overlay" onClick={() => setShowImport(false)} />
      <div className="sr-import-panel">
        <header>
          <h3><FontAwesomeIcon icon={faFileImport} style={{ marginRight: 10 }} /> Import Suppression Sources</h3>
          <button
            style={{ padding: '6px 10px', background: 'transparent', border: '1px solid rgba(255,255,255,0.08)', borderRadius: 6, color: '#8b919c', cursor: 'pointer', fontSize: 16 }}
            onClick={() => setShowImport(false)}
          >
            <FontAwesomeIcon icon={faTimes} />
          </button>
        </header>
        <section>
          {!importResult ? (
            <>
              <div
                className={`sr-import-dropzone ${dragOver ? 'dragover' : ''}`}
                onDragEnter={(e) => { e.preventDefault(); e.stopPropagation(); setDragOver(true); }}
                onDragLeave={(e) => { e.preventDefault(); e.stopPropagation(); setDragOver(false); }}
                onDragOver={(e) => { e.preventDefault(); e.stopPropagation(); }}
                onDrop={handleFileDrop}
                onClick={openFilePicker}
              >
                <FontAwesomeIcon icon={faUpload} className="drop-icon" />
                {importFile ? (
                  <>
                    <p style={{ color: '#e4e6ea', fontWeight: 600 }}>{importFile.name}</p>
                    <small>{(importFile.size / 1024).toFixed(1)} KB</small>
                  </>
                ) : (
                  <>
                    <p>Drop a CSV file here or click to browse</p>
                    <small>Expected columns: offer_id, campaign_name, suppression_url, source_provider, ga_suppression_id, refresh_group, priority</small>
                  </>
                )}
              </div>
              {importFile && (
                <div style={{ marginTop: 20, display: 'flex', gap: 10, justifyContent: 'flex-end' }}>
                  <button className="sr-btn-secondary" onClick={() => setImportFile(null)}>Clear</button>
                  <button className="sr-btn-primary" onClick={importCSV} disabled={importing}>
                    {importing
                      ? <><FontAwesomeIcon icon={faSpinner} spin /> Importing&hellip;</>
                      : <><FontAwesomeIcon icon={faUpload} /> Import</>}
                  </button>
                </div>
              )}
              {importing && (
                <div className="sr-import-progress">
                  <div className="fill" style={{ width: '60%' }} />
                </div>
              )}
            </>
          ) : (
            <>
              <div style={{ marginBottom: 20, textAlign: 'center' }}>
                <FontAwesomeIcon
                  icon={importResult.errors > 0 ? faExclamationTriangle : faCheckCircle}
                  style={{ fontSize: 48, color: importResult.errors > 0 ? '#ffa726' : '#66bb6a', marginBottom: 12 }}
                />
                <h4 style={{ margin: 0, color: '#e4e6ea', fontSize: 18, fontWeight: 600 }}>Import Complete</h4>
              </div>
              <div className="sr-import-summary">
                <div className="summary-item imported"><span className="value">{importResult.imported}</span><span className="label">Imported</span></div>
                <div className="summary-item"><span className="value">{importResult.updated}</span><span className="label">Updated</span></div>
                <div className="summary-item skipped"><span className="value">{importResult.skipped}</span><span className="label">Skipped</span></div>
                <div className="summary-item errors"><span className="value">{importResult.errors}</span><span className="label">Errors</span></div>
              </div>
              {importResult.details && importResult.details.length > 0 && (
                <div style={{ marginTop: 16, padding: 14, background: '#1a1d23', borderRadius: 8, border: '1px solid rgba(255,255,255,0.06)', maxHeight: 180, overflowY: 'auto' }}>
                  {importResult.details.map((detail, i) => (
                    <div key={i} style={{ fontSize: 12, color: '#8b919c', padding: '4px 0', borderBottom: i < (importResult.details?.length ?? 0) - 1 ? '1px solid rgba(255,255,255,0.03)' : 'none' }}>
                      {detail}
                    </div>
                  ))}
                </div>
              )}
              <div style={{ marginTop: 20, display: 'flex', justifyContent: 'flex-end', gap: 10 }}>
                <button className="sr-btn-secondary" onClick={() => { setImportResult(null); setImportFile(null); }}>Import Another</button>
                <button className="sr-btn-primary" onClick={() => setShowImport(false)}>Done</button>
              </div>
            </>
          )}
        </section>
      </div>
    </>
  );

  // ==========================================================================
  // RENDER: DETAIL DRAWER
  // ==========================================================================

  const closeDetailDrawer = useCallback(() => {
    setShowDetail(null);
    setDetailSource(null);
    setDetailLogs([]);
    setTestResult(null);
  }, []);

  const renderDetailDrawer = (): React.ReactElement | null => {
    if (!showDetail) return null;
    return (
      <>
        <div className="sr-overlay" onClick={closeDetailDrawer} />
        <div className="sr-source-detail">
          <div className="sr-detail-header">
            <h3><FontAwesomeIcon icon={faEye} style={{ marginRight: 10, color: '#4fc3f7' }} /> Source Details</h3>
            <button className="close-btn" onClick={closeDetailDrawer}><FontAwesomeIcon icon={faTimes} /></button>
          </div>

          {!detailSource ? (
            <div className="sr-loading"><span>Loading source details&hellip;</span></div>
          ) : (
            <>
              {/* Source Info */}
              <div className="sr-detail-section">
                <h4>Source Information</h4>
                <div className="sr-detail-grid">
                  <div className="detail-item">
                    <span className="detail-label">Offer ID</span>
                    <span className="detail-value">{detailSource.offer_id}</span>
                  </div>
                  <div className="detail-item">
                    <span className="detail-label">Campaign Name</span>
                    <span className="detail-value">{detailSource.campaign_name}</span>
                  </div>
                  <div className="detail-item">
                    <span className="detail-label">Provider</span>
                    <span className="detail-value">
                      <span className={`sr-provider-badge ${getProviderClass(detailSource.source_provider)}`}>{detailSource.source_provider}</span>
                    </span>
                  </div>
                  <div className="detail-item">
                    <span className="detail-label">Status</span>
                    <span className="detail-value">
                      <span className={`sr-status-badge ${detailSource.is_active ? 'success' : 'skipped'}`}>{detailSource.is_active ? 'Active' : 'Inactive'}</span>
                    </span>
                  </div>
                  <div className="detail-item">
                    <span className="detail-label">GA Suppression ID</span>
                    <span className="detail-value" style={{ fontFamily: "'SF Mono', monospace", fontSize: 12 }}>{detailSource.ga_suppression_id}</span>
                  </div>
                  <div className="detail-item">
                    <span className="detail-label">Internal List ID</span>
                    <span className="detail-value" style={{ fontFamily: "'SF Mono', monospace", fontSize: 12 }}>{detailSource.internal_list_id || '\u2014'}</span>
                  </div>
                  <div className="detail-item">
                    <span className="detail-label">Priority</span>
                    <span className="detail-value">{detailSource.priority}</span>
                  </div>
                  <div className="detail-item">
                    <span className="detail-label">Group</span>
                    <span className="detail-value">
                      {detailSource.refresh_group
                        ? <span className="sr-group-chip"><FontAwesomeIcon icon={faTags} /> {detailSource.refresh_group}</span>
                        : '\u2014'}
                    </span>
                  </div>
                </div>
                {/* URL */}
                <div style={{ marginTop: 14 }}>
                  <span style={{ fontSize: 11, color: '#5a6070', textTransform: 'uppercase', letterSpacing: 0.3, display: 'block', marginBottom: 6 }}>Suppression URL</span>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 10, padding: '10px 14px', background: '#1a1d23', borderRadius: 6, border: '1px solid rgba(255,255,255,0.06)' }}>
                    <span style={{ flex: 1, fontFamily: "'SF Mono', monospace", fontSize: 12, color: '#8b919c', wordBreak: 'break-all', lineHeight: 1.5 }}>{detailSource.suppression_url}</span>
                    <button className="sr-btn-secondary" onClick={() => copyToClipboard(detailSource.suppression_url)} style={{ fontSize: 11, padding: '5px 10px', flexShrink: 0 }}>
                      <FontAwesomeIcon icon={faCopy} /> Copy
                    </button>
                  </div>
                </div>
              </div>

              {/* Last Refresh */}
              <div className="sr-detail-section">
                <h4>Last Refresh</h4>
                <div className="sr-detail-grid">
                  <div className="detail-item">
                    <span className="detail-label">Refreshed At</span>
                    <span className="detail-value">{formatDateTime(detailSource.last_refreshed_at)}</span>
                  </div>
                  <div className="detail-item">
                    <span className="detail-label">Status</span>
                    <span className="detail-value">
                      <span className={`sr-status-badge ${getStatusBadgeClass(detailSource.last_refresh_status)}`}>{detailSource.last_refresh_status || 'Never'}</span>
                    </span>
                  </div>
                  <div className="detail-item">
                    <span className="detail-label">Entry Count</span>
                    <span className="detail-value">{formatNumber(detailSource.last_entry_count)}</span>
                  </div>
                  <div className="detail-item">
                    <span className="detail-label">Duration</span>
                    <span className="detail-value">{formatDuration(detailSource.last_refresh_ms)}</span>
                  </div>
                </div>
                {detailSource.last_error && (
                  <div style={{ marginTop: 12, padding: '10px 14px', background: 'rgba(239,83,80,0.06)', border: '1px solid rgba(239,83,80,0.15)', borderRadius: 6, fontSize: 12, color: '#ef9a9a', lineHeight: 1.5 }}>
                    <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 8 }} />{detailSource.last_error}
                  </div>
                )}
              </div>

              {/* Test Download */}
              <div className="sr-detail-section">
                <h4>Test Download</h4>
                <button className="sr-btn-primary" onClick={() => testSource(detailSource.id)} disabled={testingSource === detailSource.id} style={{ fontSize: 12, padding: '7px 16px' }}>
                  {testingSource === detailSource.id
                    ? <><FontAwesomeIcon icon={faSpinner} spin /> Testing&hellip;</>
                    : <><FontAwesomeIcon icon={faSync} /> Test Download</>}
                </button>
                {testResult && (
                  <div className={`sr-test-result ${testResult.success ? 'success' : 'error'}`}>
                    <h5>
                      <FontAwesomeIcon icon={testResult.success ? faCheckCircle : faTimesCircle} style={{ marginRight: 8 }} />
                      {testResult.success ? 'Test Successful' : 'Test Failed'}
                    </h5>
                    {testResult.success ? (
                      <p>
                        Found <strong>{formatNumber(testResult.entries_found)}</strong> entries
                        in <strong>{formatDuration(testResult.download_ms)}</strong>.
                        {testResult.sample_entries && testResult.sample_entries.length > 0 && (
                          <><br />Sample: {testResult.sample_entries.slice(0, 3).join(', ')}{testResult.sample_entries.length > 3 && '\u2026'}</>
                        )}
                      </p>
                    ) : (
                      <p>{testResult.error || 'Unknown error occurred'}</p>
                    )}
                  </div>
                )}
              </div>

              {/* Notes */}
              <div className="sr-detail-section">
                <h4>Notes</h4>
                <div style={{ padding: '10px 14px', background: '#1a1d23', borderRadius: 6, border: '1px solid rgba(255,255,255,0.06)', fontSize: 13, color: detailSource.notes ? '#c0c4cc' : '#5a6070', lineHeight: 1.6, minHeight: 60 }}>
                  {detailSource.notes || 'No notes added'}
                </div>
              </div>

              {/* Recent Refresh History */}
              <div className="sr-detail-section" style={{ borderBottom: 'none' }}>
                <h4>Recent Refresh History</h4>
                {detailLogs.length === 0 ? (
                  <div style={{ color: '#5a6070', fontSize: 13, padding: '12px 0' }}>No refresh history available for this source</div>
                ) : (
                  <div className="sr-detail-history">
                    {detailLogs.slice(0, 10).map((log) => (
                      <div key={log.id} className="history-entry">
                        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
                          <span className={`sr-status-badge ${getStatusBadgeClass(log.status)}`} style={{ fontSize: 10 }}>{log.status}</span>
                          <span className="entry-date">{formatDateTime(log.started_at)}</span>
                        </div>
                        <div style={{ display: 'flex', alignItems: 'center', gap: 14 }}>
                          <span className="entry-count">{formatNumber(log.entries_downloaded)} entries</span>
                          {log.entries_new > 0 && (
                            <span style={{ color: '#81c784', fontSize: 11, fontWeight: 600 }}>+{formatNumber(log.entries_new)} new</span>
                          )}
                          <span style={{ color: '#5a6070', fontSize: 11 }}>{formatDuration(log.download_ms + log.processing_ms)}</span>
                        </div>
                      </div>
                    ))}
                  </div>
                )}
              </div>

              {/* Metadata footer */}
              <div className="sr-detail-section" style={{ background: 'rgba(26,29,35,0.5)', borderBottom: 'none' }}>
                <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 11, color: '#5a6070' }}>
                  <span>Created: {formatDateTime(detailSource.created_at)}</span>
                  <span>Updated: {formatDateTime(detailSource.updated_at)}</span>
                  <span style={{ fontFamily: "'SF Mono', monospace", fontSize: 10, color: '#3a3f4a' }}>ID: {detailSource.id}</span>
                </div>
              </div>
            </>
          )}
        </div>
      </>
    );
  };

  // ==========================================================================
  // LOADING STATE
  // ==========================================================================

  if (loading) {
    return (
      <div className={`sr-auto-refresh ${animateIn ? 'sr-animate-in' : ''}`}>
        <div className="sr-loading">
          <span>Loading suppression refresh engine&hellip;</span>
        </div>
      </div>
    );
  }

  // ==========================================================================
  // MAIN RENDER
  // ==========================================================================

  return (
    <div
      className={`sr-auto-refresh ${animateIn ? 'sr-animate-in' : ''}`}
      style={{ display: 'flex', flexDirection: 'column', gap: 20 }}
    >
      {renderStatusBanner()}
      {renderStatsRow()}
      {renderTabs()}

      <div style={{ minHeight: 300 }}>
        {activeTab === 'sources' && renderSourcesTab()}
        {activeTab === 'cycles' && renderCyclesTab()}
        {activeTab === 'groups' && renderGroupsTab()}
      </div>

      {showImport && renderImportModal()}
      {showDetail && renderDetailDrawer()}
    </div>
  );
};

export default SuppressionAutoRefresh;
