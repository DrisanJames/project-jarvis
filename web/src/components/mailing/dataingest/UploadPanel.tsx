// UploadPanel — board 5 ("Upload static file"): the S3-first upload door.
//
// A file from a laptop becomes inventory the moment it is an object in
// s3://jarvis-partner-ingest/static/<source>/<YYYY-MM-DD>/<file>. The browser
// PUTs the parts straight to the presigned URLs — nothing passes through the
// API server. The four steps:
//   1 file + source   (the source is part of the object key, so it is required
//                      before a presign can be issued)
//   2 upload          presign → PUT each part (XHR progress) → complete
//   3 declare         dataset → lane, cleaned upstream, landing status, dedupe, note
//   4 register        creates the batch row from the REAL object and runs the loader
//
// sha256: SubtleCrypto has NO streaming digest, so the file is read in 8 MB
// chunks into one buffer and digested once. Above SHA256_MAX_BYTES that would
// cost more memory/time than it is worth, so the hash is SKIPPED and the panel
// says so — it never reports a hash it did not compute.

import React, { useMemo, useState } from 'react';
import { FontAwesomeIcon } from '@fortawesome/react-fontawesome';
import { faUpload, faExclamationTriangle, faCheck, faSpinner, faFileArrowUp } from '@fortawesome/free-solid-svg-icons';
import { Panel, SectionHeader, SectionError, ProgressBar, Pill } from '../shared/ui';
import { colors, btnStyle, tableStyle, thStyle, tdStyle } from '../shared/theme';
import { dataIngestApi, type DeclaredLoad, type FeedRow, type CompletePart } from './api';

const SHA256_MAX_BYTES = 2 * 1024 * 1024 * 1024; // 2 GB
const HASH_CHUNK = 8 * 1024 * 1024;

type FileStatus = 'pending' | 'hashing' | 'uploading' | 'object' | 'registering' | 'registered' | 'error';

interface FileState {
  id: string;
  file: File;
  status: FileStatus;
  progress: number;      // 0..1 upload
  hashProgress: number;  // 0..1 hash
  sha256: string | null;
  sha256Skipped: boolean;
  objectId: string | null;
  uploadId: string | null;
  bucket: string | null;
  key: string | null;
  batchIds: string[];
  records: number | null;
  error: string | null;
}

const fmtBytes = (n: number): string => {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`;
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`;
};

async function sha256Hex(file: File, onProgress: (p: number) => void): Promise<string | null> {
  if (file.size > SHA256_MAX_BYTES) return null;
  const subtle = typeof crypto !== 'undefined' ? crypto.subtle : undefined;
  if (!subtle) return null; // non-secure context: no WebCrypto
  const buf = new Uint8Array(file.size);
  let offset = 0;
  while (offset < file.size) {
    const slice = file.slice(offset, Math.min(offset + HASH_CHUNK, file.size));
    const ab = await slice.arrayBuffer();
    buf.set(new Uint8Array(ab), offset);
    offset += ab.byteLength;
    onProgress(file.size > 0 ? offset / file.size : 1);
  }
  const digest = await subtle.digest('SHA-256', buf);
  return Array.from(new Uint8Array(digest)).map((b) => b.toString(16).padStart(2, '0')).join('');
}

/** PUT one part straight to its presigned URL; resolves with the part's ETag. */
function putPart(url: string, body: Blob, onProgress: (loaded: number) => void): Promise<string> {
  return new Promise<string>((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('PUT', url);
    xhr.upload.onprogress = (e) => { if (e.lengthComputable) onProgress(e.loaded); };
    xhr.onload = () => {
      if (xhr.status < 200 || xhr.status >= 300) {
        reject(new Error(`S3 part upload failed (HTTP ${xhr.status})`));
        return;
      }
      const etag = xhr.getResponseHeader('ETag');
      if (!etag) {
        reject(new Error('S3 did not return an ETag — the bucket CORS rule must expose ETag, otherwise the multipart upload cannot be completed.'));
        return;
      }
      resolve(etag.replace(/"/g, ''));
    };
    xhr.onerror = () => reject(new Error('S3 part upload failed (network/CORS)'));
    xhr.send(body);
  });
}

interface Props {
  feeds: FeedRow[];
  feedsError: string | null;
  onOpenFeed: (datasetId: string) => void;
}

export const UploadPanel: React.FC<Props> = ({ feeds, feedsError, onOpenFeed }) => {
  const [files, setFiles] = useState<FileState[]>([]);
  const [source, setSource] = useState('');
  const [datasetId, setDatasetId] = useState('');
  const [declared, setDeclared] = useState<DeclaredLoad>({
    cleaned_upstream: false,
    landing_status: 'held',
    dedupe: 'skip_known',
    note: '',
  });
  const [stepError, setStepError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const patch = (id: string, next: Partial<FileState>) =>
    setFiles((prev) => prev.map((f) => (f.id === id ? { ...f, ...next } : f)));

  const pick = (list: FileList | null) => {
    if (!list) return;
    const added: FileState[] = Array.from(list).map((file, i) => ({
      id: `${Date.now()}-${i}-${file.name}`,
      file,
      status: 'pending',
      progress: 0,
      hashProgress: 0,
      sha256: null,
      sha256Skipped: false,
      objectId: null,
      uploadId: null,
      bucket: null,
      key: null,
      batchIds: [],
      records: null,
      error: null,
    }));
    setFiles((prev) => [...prev, ...added]);
  };

  const uploadOne = async (fs: FileState) => {
    patch(fs.id, { status: 'hashing', error: null, hashProgress: 0 });
    let sha: string | null = null;
    try {
      sha = await sha256Hex(fs.file, (p) => patch(fs.id, { hashProgress: p }));
    } catch (e) {
      patch(fs.id, { status: 'error', error: `sha256 failed: ${e instanceof Error ? e.message : String(e)}` });
      return;
    }
    patch(fs.id, { sha256: sha, sha256Skipped: sha === null, status: 'uploading', progress: 0 });

    try {
      const pre = await dataIngestApi.presign({
        filename: fs.file.name,
        content_type: fs.file.type || 'application/octet-stream',
        bytes: fs.file.size,
        source,
        dataset_id: datasetId || undefined,
      });
      if (!pre.parts || pre.parts.length === 0) throw new Error('presign returned no part URLs');
      patch(fs.id, { objectId: pre.object_id, uploadId: pre.upload_id, bucket: pre.s3_bucket, key: pre.s3_key });

      const partSize = pre.part_size > 0 ? pre.part_size : fs.file.size;
      const loadedPerPart: number[] = new Array(pre.parts.length).fill(0);
      const parts: CompletePart[] = [];

      for (let i = 0; i < pre.parts.length; i += 1) {
        const p = pre.parts[i];
        const start = (p.part_number - 1) * partSize;
        const end = Math.min(start + partSize, fs.file.size);
        // eslint-disable-next-line no-await-in-loop
        const etag = await putPart(p.url, fs.file.slice(start, end), (loaded) => {
          loadedPerPart[i] = loaded;
          const total = loadedPerPart.reduce((a, b) => a + b, 0);
          patch(fs.id, { progress: fs.file.size > 0 ? Math.min(1, total / fs.file.size) : 1 });
        });
        parts.push({ part_number: p.part_number, etag });
      }

      const done = await dataIngestApi.complete({
        object_id: pre.object_id,
        upload_id: pre.upload_id,
        sha256: sha ?? '',
        parts,
      });
      patch(fs.id, {
        status: 'object',
        progress: 1,
        objectId: done.object_id || pre.object_id,
        bucket: done.s3_bucket || pre.s3_bucket,
        key: done.s3_key || pre.s3_key,
      });
    } catch (e) {
      patch(fs.id, { status: 'error', error: e instanceof Error ? e.message : String(e) });
    }
  };

  const uploadAll = async () => {
    if (!source.trim()) { setStepError('A source is required before uploading — it is part of the object key.'); return; }
    setStepError(null);
    setBusy(true);
    try {
      for (const fs of files) {
        if (fs.status === 'pending' || fs.status === 'error') {
          // eslint-disable-next-line no-await-in-loop
          await uploadOne(fs);
        }
      }
    } finally {
      setBusy(false);
    }
  };

  const registerAll = async () => {
    if (!datasetId) { setStepError('Pick the dataset → lane this load belongs to.'); return; }
    const ready = files.filter((f) => f.status === 'object' && f.objectId);
    if (ready.length === 0) { setStepError('No uploaded object to register yet.'); return; }
    setStepError(null);
    setBusy(true);
    try {
      for (const fs of ready) {
        patch(fs.id, { status: 'registering' });
        try {
          // eslint-disable-next-line no-await-in-loop
          const res = await dataIngestApi.register({
            object_id: fs.objectId ?? undefined,
            key: fs.key ?? undefined,
            dataset_id: datasetId,
            declared,
          });
          patch(fs.id, {
            status: 'registered',
            batchIds: Array.isArray(res.batch_ids) ? res.batch_ids : [],
            records: typeof res.records === 'number' ? res.records : null,
            error: null,
          });
        } catch (e) {
          patch(fs.id, { status: 'error', error: `register failed: ${e instanceof Error ? e.message : String(e)}` });
        }
      }
    } finally {
      setBusy(false);
    }
  };

  const datasetOptions = useMemo(
    () => feeds.slice().sort((a, b) => a.name.localeCompare(b.name)),
    [feeds],
  );
  const anyObject = files.some((f) => f.status === 'object');
  const registered = files.filter((f) => f.status === 'registered');
  const today = new Date().toISOString().slice(0, 10);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: 12, flexWrap: 'wrap' }}>
        <div>
          <div style={{ fontSize: 18, fontWeight: 700, color: colors.heading }}>Upload static file</div>
          <div style={{ fontSize: 11, color: colors.textMuted }}>
            the browser uploads straight to s3://jarvis-partner-ingest/static/ — nothing passes through the server
          </div>
        </div>
        <div style={{ marginLeft: 'auto', display: 'flex', gap: 6 }}>
          <Pill color={files.length ? colors.indigo300 : colors.idle}>1 file</Pill>
          <Pill color={files.some((f) => f.status !== 'pending') ? colors.indigo300 : colors.idle}>2 upload</Pill>
          <Pill color={anyObject ? colors.indigo300 : colors.idle}>3 declare</Pill>
          <Pill color={registered.length ? colors.success : colors.idle}>4 register</Pill>
        </div>
      </div>

      {stepError && (
        <div style={{ background: 'rgba(239,68,68,0.22)', border: '1px solid rgba(239,68,68,0.55)', borderRadius: 6, padding: 10, color: colors.dangerFaint, fontSize: 13, fontWeight: 600 }}>
          <FontAwesomeIcon icon={faExclamationTriangle} style={{ marginRight: 8 }} />
          {stepError}
          <button type="button" style={{ ...btnStyle, marginLeft: 10 }} onClick={() => setStepError(null)}>Dismiss</button>
        </div>
      )}

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(420px, 1fr))', gap: 14 }}>
        <Panel>
          <SectionHeader title="1 · File and source" icon={faFileArrowUp} />
          <label htmlFor="di-source" style={{ fontSize: 11, color: colors.textMuted, display: 'block', marginBottom: 4 }}>
            Source (partner / origin) — becomes part of the object key
          </label>
          <input
            id="di-source"
            type="text"
            value={source}
            onChange={(e) => setSource(e.target.value)}
            placeholder="globusa-att"
            style={{ width: '100%', background: 'rgba(15,23,42,0.6)', color: colors.text, border: `1px solid ${colors.panelBorder}`, borderRadius: 8, padding: '8px 10px', fontSize: 12, marginBottom: 10 }}
          />
          <label htmlFor="di-picker" style={{ fontSize: 11, color: colors.textMuted, display: 'block', marginBottom: 4 }}>
            Files (csv · csv.gz · ndjson · zip — multipart, no size ceiling)
          </label>
          <input
            id="di-picker"
            type="file"
            multiple
            onChange={(e) => pick(e.target.files)}
            style={{ width: '100%', fontSize: 12, color: colors.text, marginBottom: 10 }}
          />
          <button type="button" style={btnStyle} disabled={busy || files.length === 0} onClick={() => void uploadAll()}>
            <FontAwesomeIcon icon={busy ? faSpinner : faUpload} spin={busy} style={{ marginRight: 6 }} />
            Upload to S3
          </button>
          <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 8 }}>
            Object key on completion: <code>static/{source || '<source>'}/{today}/&lt;file&gt;</code> · sha256 recorded for
            files up to {fmtBytes(SHA256_MAX_BYTES)}; above that the hash is skipped and labelled, never faked.
          </div>

          {files.length > 0 && (
            <div style={{ marginTop: 12, overflowX: 'auto' }}>
              <table style={{ ...tableStyle, minWidth: 420 }}>
                <thead>
                  <tr>
                    <th style={thStyle}>File</th>
                    <th style={thStyle}>Size</th>
                    <th style={thStyle}>State</th>
                    <th style={thStyle}>Progress</th>
                  </tr>
                </thead>
                <tbody>
                  {files.map((fs) => (
                    <tr key={fs.id}>
                      <td style={{ ...tdStyle, fontFamily: 'monospace', maxWidth: 200, overflow: 'hidden', textOverflow: 'ellipsis' }} title={fs.key ?? fs.file.name}>
                        {fs.file.name}
                        {fs.sha256 && <div style={{ fontSize: 10, color: colors.textFaint }}>sha256 {fs.sha256.slice(0, 16)}…</div>}
                        {fs.sha256Skipped && <div style={{ fontSize: 10, color: colors.warningText }}>sha256 skipped (file over {fmtBytes(SHA256_MAX_BYTES)} or no WebCrypto)</div>}
                        {fs.error && <div style={{ fontSize: 11, color: colors.dangerText }}>{fs.error}</div>}
                        {fs.batchIds.length > 0 && (
                          <div style={{ fontSize: 10, color: colors.successText }}>
                            {fs.batchIds.length} batch{fs.batchIds.length === 1 ? '' : 'es'}: {fs.batchIds.join(', ')}
                          </div>
                        )}
                      </td>
                      <td style={tdStyle}>{fmtBytes(fs.file.size)}</td>
                      <td style={tdStyle}>
                        <Pill color={fs.status === 'error' ? colors.danger : fs.status === 'registered' ? colors.success : fs.status === 'object' ? colors.indigo300 : colors.warning}>
                          {fs.status}
                        </Pill>
                      </td>
                      <td style={{ ...tdStyle, minWidth: 130 }}>
                        <ProgressBar pct={fs.status === 'hashing' ? fs.hashProgress : fs.progress} height={8} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>

        <Panel>
          <SectionHeader title="3 · Declare the load" />
          {feedsError && <SectionError label="Dataset roster" error={feedsError} />}
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(210px, 1fr))', gap: 12 }}>
            <div>
              <label htmlFor="di-dataset" style={{ fontSize: 11, color: colors.textMuted, display: 'block', marginBottom: 4 }}>Dataset → lane</label>
              <select
                id="di-dataset"
                value={datasetId}
                onChange={(e) => setDatasetId(e.target.value)}
                style={{ width: '100%', background: 'rgba(15,23,42,0.6)', color: colors.text, border: `1px solid ${colors.panelBorder}`, borderRadius: 8, padding: '8px 10px', fontSize: 12 }}
              >
                <option value="">Choose a dataset…</option>
                {datasetOptions.map((o) => (
                  <option key={o.dataset_id} value={o.dataset_id}>{o.name}{o.lane ? ` → ${o.lane}` : ''}</option>
                ))}
              </select>
            </div>
            <div>
              <label htmlFor="di-cleaned" style={{ fontSize: 11, color: colors.textMuted, display: 'block', marginBottom: 4 }}>Already cleaned upstream?</label>
              <select
                id="di-cleaned"
                value={declared.cleaned_upstream ? 'yes' : 'no'}
                onChange={(e) => setDeclared({ ...declared, cleaned_upstream: e.target.value === 'yes' })}
                style={{ width: '100%', background: 'rgba(15,23,42,0.6)', color: colors.text, border: `1px solid ${colors.panelBorder}`, borderRadius: 8, padding: '8px 10px', fontSize: 12 }}
              >
                <option value="no">No — validate here (lands pending_eo when released)</option>
                <option value="yes">Yes — carries an EO verdict column</option>
              </select>
            </div>
            <div>
              <label htmlFor="di-landing" style={{ fontSize: 11, color: colors.textMuted, display: 'block', marginBottom: 4 }}>Landing status</label>
              <select
                id="di-landing"
                value={declared.landing_status}
                onChange={(e) => setDeclared({ ...declared, landing_status: e.target.value as DeclaredLoad['landing_status'] })}
                style={{ width: '100%', background: 'rgba(15,23,42,0.6)', color: colors.text, border: `1px solid ${colors.panelBorder}`, borderRadius: 8, padding: '8px 10px', fontSize: 12 }}
              >
                <option value="held">held — inert until you release a tranche (default)</option>
                <option value="pending_eo">pending_eo — clean now, mail when ready</option>
                <option value="ready">ready — mailable immediately (pre-cleaned only)</option>
              </select>
            </div>
            <div>
              <label htmlFor="di-dedupe" style={{ fontSize: 11, color: colors.textMuted, display: 'block', marginBottom: 4 }}>Duplicates against what we hold</label>
              <select
                id="di-dedupe"
                value={declared.dedupe}
                onChange={(e) => setDeclared({ ...declared, dedupe: e.target.value as DeclaredLoad['dedupe'] })}
                style={{ width: '100%', background: 'rgba(15,23,42,0.6)', color: colors.text, border: `1px solid ${colors.panelBorder}`, borderRadius: 8, padding: '8px 10px', fontSize: 12 }}
              >
                <option value="skip_known">Skip known addresses (queue + in-base)</option>
                <option value="skip_lane">Skip only this lane&apos;s addresses</option>
                <option value="load_all">Load all (counts dupes, never re-mails)</option>
              </select>
            </div>
            <div style={{ gridColumn: '1 / -1' }}>
              <label htmlFor="di-note" style={{ fontSize: 11, color: colors.textMuted, display: 'block', marginBottom: 4 }}>Note for the ledger</label>
              <input
                id="di-note"
                type="text"
                value={declared.note}
                onChange={(e) => setDeclared({ ...declared, note: e.target.value })}
                placeholder="why this file, from where"
                style={{ width: '100%', background: 'rgba(15,23,42,0.6)', color: colors.text, border: `1px solid ${colors.panelBorder}`, borderRadius: 8, padding: '8px 10px', fontSize: 12 }}
              />
            </div>
          </div>
          <div style={{ fontSize: 11, color: colors.textMuted, marginTop: 8 }}>
            Supply class is fixed to <span style={{ color: colors.warningText }}>at_rest</span> for this door.
          </div>

          <div style={{ marginTop: 14, borderTop: `1px solid ${colors.divider}`, paddingTop: 12 }}>
            <SectionHeader title="4 · Register the load" />
            <div style={{ fontSize: 12, color: colors.text, lineHeight: 1.5, marginBottom: 10 }}>
              Register refuses a key that is not in the bucket, creates the batch row with the real bucket and key,
              stamps supply_class = at_rest, and runs the loader from the object. Nothing mails until you release
              or the lane claims it.
            </div>
            <button type="button" style={btnStyle} disabled={busy || !anyObject} onClick={() => void registerAll()}>
              <FontAwesomeIcon icon={busy ? faSpinner : faCheck} spin={busy} style={{ marginRight: 6 }} />
              Register {files.filter((f) => f.status === 'object').length || ''} object{files.filter((f) => f.status === 'object').length === 1 ? '' : 's'}
            </button>
            {registered.length > 0 && (
              <div style={{ marginTop: 10, fontSize: 12, color: colors.successText }}>
                {registered.map((fs) => (
                  <div key={fs.id} style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                    <FontAwesomeIcon icon={faCheck} />
                    <span style={{ fontFamily: 'monospace' }}>
                      {fs.file.name} → {fs.batchIds.length} batch{fs.batchIds.length === 1 ? '' : 'es'}
                      {fs.batchIds.length > 0 ? ` (${fs.batchIds.join(', ')})` : ''}
                      {typeof fs.records === 'number' ? ` · ${fs.records.toLocaleString()} records` : ''}
                    </span>
                    {datasetId && (
                      <button type="button" style={{ ...btnStyle, padding: '2px 8px', fontSize: 11 }} onClick={() => onOpenFeed(datasetId)}>
                        Open the feed
                      </button>
                    )}
                  </div>
                ))}
              </div>
            )}
          </div>
        </Panel>
      </div>

      <Panel>
        <SectionHeader title="Large files from a terminal" />
        <div style={{ fontSize: 12, color: colors.text, marginBottom: 8 }}>
          The same door, without a browser. The register step is what makes the object a load.
        </div>
        <pre
          style={{
            margin: 0, fontFamily: 'monospace', fontSize: 11, lineHeight: 1.6, padding: '10px 12px',
            background: 'rgba(15,23,42,0.6)', border: `1px solid ${colors.panelBorder}`, borderRadius: 8,
            color: colors.text, whiteSpace: 'pre-wrap',
          }}
        >
{`aws s3 cp <file> s3://jarvis-partner-ingest/static/${source || '<source>'}/${today}/
python3 -m agents.jobs.static_register --key static/${source || '<source>'}/${today}/<file> --source ${source || '<source>'} --lane <lane> --status ${declared.landing_status}`}
        </pre>
      </Panel>
    </div>
  );
};

export default UploadPanel;
