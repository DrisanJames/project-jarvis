// api.ts — typed clients for GET/POST /api/mailing/data-ingest/*.
//
// THE GO HANDLER STRUCTS ARE THE CONTRACT. Every type below mirrors the
// `json:"…"` tags in internal/api/data_ingest_handlers.go and
// data_ingest_static.go verbatim (struct + line cited on each type). The
// fixtures under __fixtures__/ are REAL prod bodies (2026-09-21) and the
// panel tests render them, so a shape drift on either side fails a test
// instead of rendering a confident blank on the operator's screen.
//
// Envelope (REQ 2026-09-20 "Read API"): EVERY response carries
//   { as_of, source, fields: {<FLAT go name>: 'measured'|'derived'|'not_measured'} }
// and the screen renders a field the API flags `not_measured` as the words
// "not yet measured" — never 0, never 0%, never "—" where a count belongs
// (PORTAL_DESIGN_SYSTEM §6.2). `measured()` below is deliberately CONSERVATIVE:
// a field the envelope does not flag is treated as not measured, so a backend
// that forgets to flag something can never make this screen invent a number.
// The flag keys are the FLAT names Go marks (`landed`, `arrived`, `raw` …),
// shared across sections of one response — never prefixed.

import { apiFetch } from '../shared/apiFetch';

export const DATA_INGEST_BASE = '/api/mailing/data-ingest';

export type Measurement = 'measured' | 'derived' | 'not_measured';
export type FieldFlags = Record<string, Measurement>;

/** diMeta (data_ingest_handlers.go:177-182) — on every response body. */
export interface Envelope {
  as_of: string | null;
  /** counters | rollup | table | mixed (what actually produced these numbers). */
  source: string;
  fields: FieldFlags;
  organization_id?: string;
}

/** A count the backend may not have measured (Go *int64 → null). Never coerce null to 0. */
export type Count = number | null;

export type SupplyClass = 'at_rest' | 'dynamic' | 'internal_transfer';

// ── measurement helpers ────────────────────────────────────────────────────

export function measured(fields: FieldFlags | undefined, name: string): Measurement {
  const f = fields ? fields[name] : undefined;
  return f === 'measured' || f === 'derived' || f === 'not_measured' ? f : 'not_measured';
}

/** True when the field is measured (or derived) AND a real number arrived. */
export function hasValue(fields: FieldFlags | undefined, name: string, value: Count | undefined): boolean {
  return measured(fields, name) !== 'not_measured' && typeof value === 'number' && Number.isFinite(value);
}

export const fmtCount = (n: number): string => Math.round(n).toLocaleString();

/** Go sends "" for an absent timestamp/string (non-pointer string) — treat it as absent. */
export function presentString(s: string | null | undefined): string | null {
  if (typeof s !== 'string') return null;
  const t = s.trim();
  return t === '' ? null : t;
}

/** ispCount (data_ingest_handlers.go:261-265). `mailed` only on /loads composition. */
export interface IspCount {
  isp: string;
  n: number;
  mailed?: number;
}

/** daySeriesPoint (:554-558) — used by /day AND /feeds/{id}. Never null: a day absent from the rollup is absent from the array. */
export interface DaySeriesPoint {
  day: string;
  at_rest: number;
  dynamic: number;
  transfer: number;
}

// ── GET /day ───────────────────────────────────────────────────────────────

/** dayAtRest (:530-540). Flags: landed raw staged inflight mailed removed static_objects loads_without_object parked_in_db */
export interface AtRestClass {
  /** static objects registered that day (counter fact) */
  landed: Count;
  raw: Count;
  staged: Count;
  inflight: Count;
  mailed: Count;
  removed: Count;
  static_objects: Count;
  loads_without_object: Count;
  parked_in_db: Count;
}

/** dayDynamic (:542-547). Flags: arrived yesterday arrival_rate feeds_live */
export interface DynamicClass {
  arrived: Count;
  yesterday: Count;
  /** records / elapsed Denver hour (derived) */
  arrival_rate: Count;
  feeds_live: Count;
}

/** dayTransfer (:549-551). Flag: n */
export interface TransferClass {
  n: Count;
}

/** dayResponse (:560-568) — the three classes are TOP-LEVEL keys, not nested. */
export interface DayResponse extends Envelope {
  date: string;
  at_rest: AtRestClass;
  dynamic: DynamicClass;
  internal_transfer: TransferClass;
  /** last 30 Denver days from the rollup (flag: series) */
  series: DaySeriesPoint[];
  /** the day's arrivals by canonical ISP class, all classes (flag: composition) */
  composition: IspCount[];
}

// ── GET /hours ─────────────────────────────────────────────────────────────

/** hourPoint (:876-879) + hoursResponse (:881-886). Flag: hours */
export interface HoursResponse extends Envelope {
  date: string;
  class: SupplyClass;
  /** Denver hours that HAVE a counter; an empty array = not measured. */
  hours: Array<{ hour: number; n: number }>;
}

// ── GET /feeds, GET /feeds/{id} ────────────────────────────────────────────

/** feedStatus (:927-932) — plain bools, never null on the wire. */
export interface FeedStatus {
  ingest_open: boolean;
  send_row: boolean;
  express: boolean;
  contract: boolean;
  /** partner_datasets.paused_emergency — the SENDING pause (drip/broadcast claims stop). */
  sending_paused: boolean;
  /** partner_datasets.intake_paused — the INTAKE door. Independent of sending_paused (brain #3823). */
  intake_paused: boolean;
}

/** feedRow (:934-956). Flags: today yesterday raw staged inflight mailed records consumed remaining last_event last_loaded supply_class source_channel */
export interface FeedRow {
  dataset_id: string;
  partner_id: string;
  name: string;
  partner: string;
  lane: string;
  /** '' until the batch stamp backfill runs */
  supply_class: SupplyClass | '';
  source_channel: string;
  status: FeedStatus;
  today: Count;
  yesterday: Count;
  raw: Count;
  staged: Count;
  inflight: Count;
  mailed: Count;
  records: Count;
  consumed: Count;
  remaining: Count;
  /** RFC3339 or '' (absent) — read through presentString() */
  last_event: string;
  last_loaded: string;
}

/** feedsResponse (:958-965) */
export interface FeedsResponse extends Envelope {
  date: string;
  feeds: FeedRow[];
  cache_age_seconds: number;
  query_ms: number;
  /** set when the per-dataset last-batch lookup was skipped (degraded) */
  note?: string;
}

/** feedLoad (:1164-1173) — the feed page's load rows. Flag: loads */
export interface FeedLoad {
  batch_id: string;
  /** RFC3339 or '' */
  received_at: string;
  name: string;
  /** s3 bucket/key, source_path, or the batch id — whatever identifies the load */
  ref: string;
  records: number;
  raw: number;
  staged: number;
  mailed: number;
}

/** feedFunnel (:1175-1182). Flags: landed raw cleaned staged mailed engaged */
export interface FeedFunnel {
  landed: Count;
  raw: Count;
  cleaned: Count;
  staged: Count;
  mailed: Count;
  engaged: Count;
}

/** feedComposition (:1184-1187). Flags: composition_raw composition_staged */
export interface FeedComposition {
  raw: IspCount[];
  staged: IspCount[];
}

/**
 * feedDetailResponse (:1189-1197). The header fields (name … last_loaded) are
 * being added server-side; until they land the panel takes them from the
 * /feeds list row for the same dataset_id.
 */
export interface FeedDetailResponse extends Envelope {
  date: string;
  dataset_id: string;
  funnel: FeedFunnel;
  /** flag: series — per-class landings for THIS feed, 30 days */
  series: DaySeriesPoint[];
  composition: FeedComposition;
  loads: FeedLoad[];
  name?: string;
  partner?: string;
  lane?: string;
  supply_class?: SupplyClass | '';
  source_channel?: string;
  status?: FeedStatus;
  last_loaded?: string;
}

// ── GET /loads ─────────────────────────────────────────────────────────────

/** loadRow (:1361-1376) */
export interface LoadRow {
  batch_id: string;
  dataset: string;
  dataset_id: string;
  source_path: string;
  supply_class: SupplyClass | '';
  s3_bucket: string;
  s3_key: string;
  object: boolean;
  records: number;
  mailed: number;
  staged: number;
  raw: number;
  removed: number;
  /** RFC3339 or '' */
  received_at: string;
}

/** loadTotals (:1392-1398). Flags: landed mailed not_mailed removed duplicates */
export interface LoadTotals {
  landed: Count;
  mailed: Count;
  not_mailed: Count;
  removed: Count;
  duplicates: Count;
}

/** loadsResponse (:1405-1413). Flags also: loads sources composition */
export interface LoadsResponse extends Envelope {
  date: string;
  totals: LoadTotals;
  loads: LoadRow[];
  composition: IspCount[];
  sources: Array<{ name: string; n: number }>;
  /** "loads_query_failed: …" when the day range timed out — the list is then NOT an empty day */
  note?: string;
}

// ── GET /state ─────────────────────────────────────────────────────────────

/** stateCounters (:1636-1647) — read from the same /health snapshot. */
export interface StateCounters {
  running: boolean;
  /** RFC3339 or '' */
  last_handled_at: string;
  applied: number;
  duplicates: number;
  lag_max: number;
  redis_available: boolean;
  measured_today: boolean;
  stream_clients: number;
}

/** stateTiles (:1649-1657). Flags: static_objects loads_without_object parked_in_db staged inflight mailed removed */
export interface StateTiles {
  static_objects: Count;
  loads_without_object: Count;
  parked_in_db: Count;
  staged: Count;
  inflight: Count;
  mailed: Count;
  removed: Count;
}

/** stateResponse (:1659-1666) */
export interface StateResponse extends Envelope {
  tiles: StateTiles;
  by_status: Record<string, number>;
  counters: StateCounters;
  cache_age_seconds: number;
  query_ms: number;
}

// ── POST /static/* (the upload door) ───────────────────────────────────────

/** staticPresignRequest (data_ingest_static.go:268-274) */
export interface PresignRequest {
  filename: string;
  content_type: string;
  bytes: number;
  source: string;
  /** recorded on the object row; the key is static/<source>/<date>/<file> */
  dataset_id?: string;
}

/** HandlePresign response map (data_ingest_static.go:405-416) */
export interface PresignResponse {
  organization_id?: string;
  object_id: string;
  s3_bucket: string;
  s3_key: string;
  upload_id: string;
  part_size: number;
  part_count: number;
  parts: Array<{ part_number: number; url: string }>;
  expires_in: number;
  method: string;
  message?: string;
}

/** staticCompleteRequest.Parts (data_ingest_static.go:425-429) */
export interface CompletePart { part_number: number; etag: string }

/** staticCompleteRequest (:421-430) */
export interface CompleteRequest {
  object_id: string;
  upload_id: string;
  sha256: string;
  parts: CompletePart[];
}

/** HandleComplete response map (:536-543) */
export interface CompleteResponse {
  organization_id?: string;
  object_id: string;
  s3_bucket: string;
  s3_key: string;
  bytes: Count;
  etag: string;
  sha256: string;
  status: string;
}

export interface DeclaredLoad {
  cleaned_upstream: boolean;
  landing_status: 'held' | 'pending_eo' | 'ready';
  dedupe: 'skip_known' | 'skip_lane' | 'load_all';
  note: string;
}

/** staticRegisterRequest (:548-559) */
export interface RegisterRequest {
  object_id?: string;
  key?: string;
  dataset_id: string;
  /** {target: column_index}; omitted = derived from the object header */
  mapping?: Record<string, number>;
  declared: DeclaredLoad;
}

/** HandleRegister response map (:770-784) */
export interface RegisterResponse {
  object_id: string;
  s3_bucket: string;
  s3_key: string;
  object_sha256: string;
  dataset_id: string;
  dataset_name: string;
  supply_class: string;
  source_path: string;
  landing_status: string;
  batch_ids: string[];
  batches: number;
  records: Count;
  skipped_invalid: Count;
  status: string;
  message?: string;
}

// ── the stream delta (SSE `event: delta`) ──────────────────────────────────

/** dataingest.Delta (internal/dataingest/counters.go:539-547); dataset_id/lane/by_isp are omitempty. */
export interface IngestDelta {
  day: string;
  supply_class: SupplyClass;
  transition: string;
  by_isp: Record<string, number>;
  n: number;
  dataset_id: string;
  lane?: string;
}

// ── transport ──────────────────────────────────────────────────────────────

/** Normalize so `fields` is always an object — measured() must never throw. */
function withEnvelope<T>(body: T): T {
  const b = body as unknown as { fields?: unknown; source?: unknown; as_of?: unknown };
  if (!b || typeof b !== 'object') return body;
  if (typeof b.fields !== 'object' || b.fields === null) b.fields = {};
  if (typeof b.source !== 'string') b.source = 'unknown';
  if (typeof b.as_of !== 'string') b.as_of = null;
  return body;
}

async function readError(r: Response): Promise<string> {
  let detail = `HTTP ${r.status}`;
  try {
    const j = (await r.json()) as { error?: string };
    if (j && j.error) detail = j.error;
  } catch {
    /* non-JSON error body */
  }
  return detail;
}

async function get<T>(path: string, signal?: AbortSignal): Promise<T> {
  const r = await apiFetch(`${DATA_INGEST_BASE}${path}`, { signal });
  if (!r.ok) throw new Error(await readError(r));
  const body = (await r.json()) as T & { error?: string };
  if (body && typeof body === 'object' && body.error) throw new Error(body.error);
  return withEnvelope(body);
}

async function post<Req, Res>(path: string, payload: Req, signal?: AbortSignal): Promise<Res> {
  const r = await apiFetch(`${DATA_INGEST_BASE}${path}`, {
    method: 'POST',
    body: JSON.stringify(payload),
    signal,
  });
  if (!r.ok) throw new Error(await readError(r));
  const body = (await r.json()) as Res & { error?: string };
  if (body && typeof body === 'object' && body.error) throw new Error(body.error);
  return body;
}

export const dataIngestApi = {
  day: (date: string, signal?: AbortSignal) =>
    get<DayResponse>(`/day?date=${encodeURIComponent(date)}`, signal),

  hours: (date: string, cls: SupplyClass, signal?: AbortSignal) =>
    get<HoursResponse>(`/hours?date=${encodeURIComponent(date)}&class=${encodeURIComponent(cls)}`, signal),

  feeds: (signal?: AbortSignal) => get<FeedsResponse>('/feeds', signal),

  feed: (datasetId: string, date: string, signal?: AbortSignal) =>
    get<FeedDetailResponse>(`/feeds/${encodeURIComponent(datasetId)}?date=${encodeURIComponent(date)}`, signal),

  loads: (date: string, signal?: AbortSignal) =>
    get<LoadsResponse>(`/loads?date=${encodeURIComponent(date)}`, signal),

  state: (signal?: AbortSignal) => get<StateResponse>('/state', signal),

  presign: (req: PresignRequest, signal?: AbortSignal) =>
    post<PresignRequest, PresignResponse>('/static/presign', req, signal),

  complete: (req: CompleteRequest, signal?: AbortSignal) =>
    post<CompleteRequest, CompleteResponse>('/static/complete', req, signal),

  register: (req: RegisterRequest, signal?: AbortSignal) =>
    post<RegisterRequest, RegisterResponse>('/static/register', req, signal),
};

// ── the dataset switches (existing data-partners endpoints) ────────────────
// FeedPanel drives the REAL controls, not new ones: emergency-stop / resume
// (SENDING), intake-pause / intake-resume (INTAKE) and express all live under
// /api/mailing/data-partners/datasets/{id}/…

export type DatasetAction = 'emergency-stop' | 'resume' | 'intake-pause' | 'intake-resume' | 'express';

export async function datasetAction(
  datasetId: string,
  action: DatasetAction,
  payload?: Record<string, unknown>,
): Promise<void> {
  const r = await apiFetch(`/api/mailing/data-partners/datasets/${encodeURIComponent(datasetId)}/${action}`, {
    method: 'POST',
    body: payload ? JSON.stringify(payload) : undefined,
  });
  if (!r.ok) throw new Error(await readError(r));
}
