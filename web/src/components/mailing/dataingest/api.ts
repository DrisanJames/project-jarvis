// api.ts — typed clients for GET/POST /api/mailing/data-ingest/*.
//
// Contract (REQ 2026-09-20 "Read API"): EVERY response carries
//   { as_of, source: 'counters'|'rollup'|'table', fields: {<name>: 'measured'|'derived'|'not_measured'} }
// and the screen renders a field the API flags `not_measured` as the words
// "not yet measured" — never 0, never 0%, never "—" where a count belongs
// (PORTAL_DESIGN_SYSTEM §6.2). `measured()` below is deliberately CONSERVATIVE:
// a field the envelope does not flag is treated as not measured, so a backend
// that forgets to flag something can never make this screen invent a number.

import { apiFetch } from '../shared/apiFetch';

export const DATA_INGEST_BASE = '/api/mailing/data-ingest';

export type Measurement = 'measured' | 'derived' | 'not_measured';
export type FieldFlags = Record<string, Measurement>;

/** Every data-ingest response body carries this envelope. */
export interface Envelope {
  as_of: string | null;
  /** counters | rollup | table (what actually produced these numbers). */
  source: string;
  fields: FieldFlags;
}

/** A count the backend may not have measured. Never coerce null to 0. */
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

// ── GET /day ───────────────────────────────────────────────────────────────

export interface AtRestClass {
  /** static objects registered that day */
  landed: Count;
  raw: Count;
  staged: Count;
  inflight: Count;
  mailed: Count;
  removed: Count;
  /** inventory tiles (board 1, "At rest · loaded inventory") */
  static_objects: Count;
  loads_without_object: Count;
  parked_in_db: Count;
}

export interface DynamicClass {
  arrived: Count;
  yesterday: Count;
  /** records / hour across live feeds */
  arrival_rate: Count;
  feeds_live: Count;
  last_api_batch: string | null;
  raw: Count;
  staged: Count;
  mailed: Count;
}

export interface DayResponse extends Envelope {
  date: string;
  classes: {
    at_rest: AtRestClass;
    dynamic: DynamicClass;
    internal_transfer: { n: Count; note?: string | null };
  };
  /** last 30 Denver days from the rollup; nulls where a class has no history. */
  series: Array<{ day: string; at_rest: Count; dynamic: Count; transfer: Count }>;
  /** composition of STAGED (ready) rows by canonical ISP class. */
  composition: Array<{ isp: string; n: Count }>;
}

// ── GET /hours ─────────────────────────────────────────────────────────────

export interface HoursResponse extends Envelope {
  date: string;
  class: SupplyClass;
  /** 24 rows, Denver hour 0..23. `n` null = that hour was never counted. */
  hours: Array<{ hour: number; n: Count; median_7d?: Count }>;
}

// ── GET /feeds, GET /feeds/{id} ────────────────────────────────────────────

export interface FeedStatus {
  ingest_open: boolean | null;
  send_row: boolean | null;
  express: boolean | null;
  contract: boolean | null;
}

export interface FeedRow {
  dataset_id: string;
  partner_id: string;
  name: string;
  lane: string;
  supply_class: SupplyClass;
  source_channel: string;
  status: FeedStatus;
  today: Count;
  yesterday: Count;
  raw: Count;
  staged: Count;
  inflight: Count;
  mailed: Count;
  last_event: string | null;
  last_loaded: string | null;
  /** at-rest inventory columns (board 1, "Loaded inventory") */
  location?: string | null;
  records?: Count;
  consumed?: Count;
  remaining?: Count;
}

export interface FeedsResponse extends Envelope {
  feeds: FeedRow[];
}

export interface LoadRow {
  batch_id: string;
  dataset_id: string;
  dataset: string;
  source_path: string;
  supply_class: SupplyClass;
  s3_bucket: string | null;
  s3_key: string | null;
  object: boolean;
  received_at: string | null;
  records: Count;
  mailed: Count;
  staged: Count;
  raw: Count;
  removed: Count;
}

export interface FeedDetailResponse extends Envelope {
  dataset_id: string;
  name: string;
  partner_name: string;
  lane: string;
  supply_class: SupplyClass;
  source_channel: string;
  status: FeedStatus;
  status_note?: string | null;
  funnel: {
    landed: Count;
    raw: Count;
    cleaned: Count;
    staged: Count;
    mailed: Count;
    engaged: Count;
  };
  series: Array<{ day: string; n: Count }>;
  composition: Array<{ isp: string; raw: Count; staged: Count }>;
  loads: LoadRow[];
}

// ── GET /loads ─────────────────────────────────────────────────────────────

export interface LoadsResponse extends Envelope {
  date: string;
  totals: {
    landed: Count;
    mailed_since: Count;
    not_yet_mailed: Count;
    removed: Count;
    duplicates: Count;
  };
  loads: LoadRow[];
  composition: Array<{ isp: string; n: Count; mailed: Count }>;
  sources: Array<{ name: string; detail: string | null; n: Count }>;
}

// ── GET /state ─────────────────────────────────────────────────────────────

export interface StateResponse extends Envelope {
  tiles: {
    static_objects: Count;
    loads_without_object: Count;
    parked_in_db: Count;
    staged_for_mailing: Count;
    inflight: Count;
    mailed_lifetime: Count;
    removed_by_cleaning: Count;
  };
  counters: {
    live: boolean;
    last_event_at: string | null;
    consumer: string | null;
    lag_seconds: Count;
  };
}

// ── POST /static/* (the upload door) ───────────────────────────────────────

export interface PresignRequest {
  filename: string;
  content_type: string;
  bytes: number;
  source: string;
  /** recorded on the object row; the key is static/<source>/<date>/<file> */
  dataset_id?: string;
}

/** Shapes below mirror internal/api/data_ingest_static.go verbatim. */
export interface PresignResponse {
  object_id: string;
  s3_bucket: string;
  s3_key: string;
  upload_id: string;
  part_size: number;
  part_count: number;
  parts: Array<{ part_number: number; url: string }>;
  expires_in: number;
  method: string;
}

export interface CompletePart { part_number: number; etag: string }

export interface CompleteRequest {
  object_id: string;
  upload_id: string;
  sha256: string;
  parts: CompletePart[];
}

export interface CompleteResponse {
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

export interface RegisterRequest {
  object_id?: string;
  key?: string;
  dataset_id: string;
  /** {target: column_index}; omitted = derived from the object header */
  mapping?: Record<string, number>;
  declared: DeclaredLoad;
}

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

export interface IngestDelta {
  day: string;
  supply_class: SupplyClass;
  transition: string;
  by_isp: Record<string, number>;
  n: number;
  dataset_id: string;
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

// ── the four dataset switches (existing data-partners endpoints) ───────────
// FeedPanel drives the REAL controls, not new ones: emergency-stop / resume /
// express already exist under /api/mailing/data-partners/datasets/{id}/…

export async function datasetAction(
  datasetId: string,
  action: 'emergency-stop' | 'resume' | 'express',
  payload?: Record<string, unknown>,
): Promise<void> {
  const r = await apiFetch(`/api/mailing/data-partners/datasets/${encodeURIComponent(datasetId)}/${action}`, {
    method: 'POST',
    body: payload ? JSON.stringify(payload) : undefined,
  });
  if (!r.ok) throw new Error(await readError(r));
}
