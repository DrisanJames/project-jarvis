# Data Partner Ingestion — Operator Setup Guide

This guide walks an operator through onboarding a new data partner end-to-end, from provisioning through verifying the first batch lands in the drip orchestrator. It maps the same journey the agent walked when validating the system on `2026-05-12` (see `.scratch/may12_partner_ingest_seed_validation.py` and the e2e smoke test below).

> **Status (2026-05-12).** Backend, frontend, migrations, and workers are committed and tested. Code is intentionally **NOT shipped to production yet** — the May 12 evening deploy stashes Partner-Ingest WIP per `.scratch/may12_evening_deploy_handoff.md`. This guide assumes the next deploy after that one will carry Partner-Ingest. The local-dev steps work today against `apex-postgres`.

---

## 1. Prerequisites (one-time, before any partner is onboarded)

### 1a. S3 bucket — `jarvis-partner-ingest`

```bash
aws s3api create-bucket --bucket jarvis-partner-ingest --region us-west-2 \
  --create-bucket-configuration LocationConstraint=us-west-2
aws s3api put-bucket-versioning --bucket jarvis-partner-ingest \
  --versioning-configuration Status=Enabled
aws s3api put-public-access-block --bucket jarvis-partner-ingest \
  --public-access-block-configuration "BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true"
```

Confirm:
```bash
aws s3 ls | grep jarvis-partner-ingest
```

### 1b. ECS task definition env vars

Open `upside-down/deploy/prepare_task_definition.py` and ensure both keys are present (already added 2026-05-12):

```python
upsert_env(env_list, "PARTNER_INGEST_S3_BUCKET", "jarvis-partner-ingest")
upsert_env(env_list, "PARTNER_INGEST_S3_REGION", "us-west-2")
```

The IAM task role must have `s3:PutObject`, `s3:GetObject`, `s3:HeadObject`, and `s3:ListBucket` for `arn:aws:s3:::jarvis-partner-ingest/*`. The same role used by `jarvis-offer-suppressions` is sufficient if extended.

### 1c. Verify production database has the partner schema (post-deploy)

After the deploy that carries Partner-Ingest:

```sql
SELECT tablename FROM pg_tables WHERE schemaname = 'public'
  AND (tablename LIKE 'data_partner%' OR tablename LIKE 'partner_%')
  ORDER BY tablename;
```

Expected: 9 rows — `data_partners`, `partner_admin_audit_log`, `partner_api_keys`, `partner_clean_queue`, `partner_datasets`, `partner_drip_creatives`, `partner_drip_state`, `partner_inbound_batches`, `partner_isp_distribution_overrides`.

```sql
SELECT vertical, brand, creative_filename FROM partner_drip_creatives
 ORDER BY vertical, brand;
```

Expected: 16 rows (4 verticals × 4 brands). If 0, the seed migrations didn't run — check the startup-migration log for `dp_seed_creative_*`.

### 1d. EmailOversight allowlist

EO's API needs the production NAT/elastic IP added to its account allowlist. The current ECS NAT IP is `98.95.199.98` (per `deployment-and-infrastructure.mdc`). Confirm with the EO account manager before the first batch reaches `pending_eo`, otherwise validator calls will 401.

---

## 2. Onboarding a New Partner — UI Flow

The fastest path. All actions audit to `partner_admin_audit_log`.

1. Open **`https://projectjarvis.io` → Mailing → Data Partners**.
2. Click **Onboard Partner** (top-right). The wizard collects:
   - **Partner name** (e.g. *Attribits*)
   - **Slug** (auto-derived; lowercase letters, digits, dashes only)
   - **Contact email**
   - **Notes** (optional)
3. Add at least one **Dataset** before clicking Finish:
   - **Dataset name** (e.g. *Attribits-HELOC*)
   - **Vertical** — must be one of `refi_heloc`, `personal_loans`, `tax_relief`, `remodel`. Anything else is rejected.
   - **Flush window hours** — defaults to 24. The audience finalizer aims to drain a dataset's `ready_queue` over this many hours.
4. After Finish, the wizard surfaces the API key for **each** dataset **once**. Copy and store securely. The key cannot be retrieved later — only the prefix is visible afterward.

### What the wizard creates server-side

- 1 row in `data_partners`
- N rows in `partner_datasets` (one per dataset added)
- N rows in `partner_api_keys` (one per dataset, key hashed via SHA-256)
- 2N audit rows (`create_partner` + `create_dataset` per dataset)

---

## 3. Onboarding via API (alternative)

For automated provisioning. Requires `X-Admin-Key` (the operator's admin token).

```bash
ADMIN="local-admin-test-key"   # set to $ADMIN_API_KEY in production

# 1. Create partner
PARTNER_ID=$(curl -sS -H "X-Admin-Key: $ADMIN" -H "Content-Type: application/json" \
  -X POST https://projectjarvis.io/api/mailing/data-partners \
  -d '{"name":"Attribits","slug":"attribits","contact_email":"ops@attribits.com"}' \
  | jq -r '.id')

# 2. Create datasets — repeat per vertical
curl -sS -H "X-Admin-Key: $ADMIN" -H "Content-Type: application/json" \
  -X POST https://projectjarvis.io/api/mailing/data-partners/$PARTNER_ID/datasets \
  -d '{"name":"Attribits-HELOC","slug":"attribits-heloc","vertical":"refi_heloc","flush_window_hours":24}'
# Response includes: { dataset_id, api_key, api_key_prefix, ... } — show api_key once.
```

The cutover script `.scratch/may12_attribits_cutover.py` does this for the standing 4-vertical partner template.

---

## 4. Inbound API Contract (hand to partner)

Send the partner this snippet:

```
Endpoint:   POST https://projectjarvis.io/api/partner-ingest/v1/records
Auth:       X-Partner-Key: dpk_<32 chars>     (obtain from Ignite ops; cannot be regenerated)
Content:    application/json | application/x-ndjson | text/plain (NDJSON)
Limits:     max 25 MiB body, max 50,000 records per batch
Schema:     GET https://projectjarvis.io/api/partner-ingest/v1/schema  (no auth required)
Status:     GET https://projectjarvis.io/api/partner-ingest/v1/batches/{id}  (X-Partner-Key)

Required fields per record:
  - email                (lowercased + trimmed server-side)

Optional fields:
  - first_name, last_name, zip, state, ip_address
  - opt_in_date          (ISO 8601 RFC3339)
  - source               (free-text; helpful for compliance audits)
  - metadata             (object; preserved verbatim, never used for routing)
```

Response on success: `HTTP 202` with `{batch_id, record_count, accepted, s3_key, received_at}`. The slicer worker picks the batch up within 30 s.

### Worked POST example (curl)

```bash
KEY="dpk_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
curl -sS -X POST https://projectjarvis.io/api/partner-ingest/v1/records \
  -H "X-Partner-Key: $KEY" \
  -H "Content-Type: application/json" \
  -d '{"records":[
        {"email":"alice@example.com","first_name":"Alice","zip":"83854","opt_in_date":"2026-05-12T20:00:00Z","source":"form-12-homepage"},
        {"email":"bob@example.com","first_name":"Bob"}
      ]}'
```

### NDJSON example (best for >1k records)

```bash
gzip -k payload.ndjson
curl -sS -X POST https://projectjarvis.io/api/partner-ingest/v1/records \
  -H "X-Partner-Key: $KEY" \
  -H "Content-Type: application/x-ndjson" \
  -H "Content-Encoding: gzip" \
  --data-binary @payload.ndjson.gz
```

---

## 5. Verifying the First Batch — Step by Step

After the partner POSTs their first batch, walk this exactly. The full path is `received → slicing → slicing_complete → (per-record) ready/suppressed_global/suppressed_eo → mailed`.

### 5a. Confirm S3 storage

```bash
aws s3 ls s3://jarvis-partner-ingest/partners/<partner_slug>/<dataset_slug>/$(date -u +%Y/%m/%d)/
```

You should see `<batch_id>.ndjson.gz` and `<batch_id>.meta.json`. Inspect either with `aws s3 cp ... -` and pipe through `gunzip` / `jq`.

### 5b. Confirm DB row

```sql
SELECT id, status, record_count, next_record_offset, received_at, completed_at, last_error
FROM partner_inbound_batches
WHERE id = '<batch_id>';
```

Status progression (each row should move within ~30 s of the previous):
- `received` — POST handler wrote to S3 and DB
- `slicing` — slicer worker has the row (advisory lock + claim)
- `slicing_complete` — every record evaluated, written to `partner_clean_queue`
- `(no further status on the batch row)` — counters move on the queue rows

### 5c. Inspect the clean queue

```sql
SELECT status, COUNT(*) FROM partner_clean_queue WHERE batch_id = '<batch_id>' GROUP BY status;
```

Expected statuses:
- `pending_eo` — survived global suppression, waiting on EmailOversight
- `suppressed_global` — matched a global suppression list
- `ready` — EO marked Verified
- `suppressed_eo` — EO marked Risky/Invalid/Unknown
- `mailed` — drip orchestrator has consumed and dispatched

A healthy first batch typically lands ~85-92 % at `ready`, ~5-10 % at `suppressed_global`, ~3-5 % at `suppressed_eo`.

### 5d. Verify via partner-facing status endpoint

```bash
curl -sS -H "X-Partner-Key: $KEY" \
  https://projectjarvis.io/api/partner-ingest/v1/batches/<batch_id> | jq
```

Returns the same counters in JSON. Use this for partner SDK testing — the partner CAN poll their own batches but only their own.

### 5e. Watch the dashboard

Open **Mailing → Data Partners → Overview**. The vertical card for the dataset's vertical should show:
- `ready_queue` ticking up as EO validates
- `pending_eo` going down
- `last_wave_at` updating roughly every 15 min once `ready_queue ≥ wave_size`
- `next_brand_index` rotating 0→1→2→3→0 as DB → HT → MH → QF take wave turns

The Recent Batches table at the bottom should show your partner/dataset, status `slicing_complete`, with an Inspect button that opens the batch inspector modal.

---

## 6. Operator Controls

### 6a. Emergency stop a dataset

UI: **Partners & Datasets → Stop** button. API:

```bash
curl -sS -H "X-Admin-Key: $ADMIN" -H "Content-Type: application/json" \
  -X POST https://projectjarvis.io/api/mailing/data-partners/datasets/<dataset_id>/emergency-stop \
  -d '{"reason":"compliance escalation 12345"}'
```

Effects:
- `partner_datasets.paused_emergency = true`
- Inbound API returns `503 dataset_paused` for any X-Partner-Key tied to this dataset
- Slicer halts at the next slice boundary
- Drip orchestrator skips this dataset's `ready_queue`
- Audit row written

Resume with `POST .../resume` — same shape, no body required.

### 6b. ISP distribution overrides

Default behaviour: drip orchestrator picks records uniformly across ISP families. To bias / cap a specific ISP per dataset:

UI: **Partners & Datasets → ISP** → modal lets you set per-ISP `pct_override` (0.0-1.0) and `max_per_wave`. API:

```bash
curl -sS -H "X-Admin-Key: $ADMIN" -H "Content-Type: application/json" \
  -X PUT https://projectjarvis.io/api/mailing/data-partners/datasets/<dataset_id>/isp-distribution \
  -d '{"overrides":[
        {"isp":"gmail","pct_override":0.5,"max_per_wave":1000},
        {"isp":"yahoo","pct_override":0.3}
      ]}'
```

Override semantics: `pct_override` reweights selection probability; `max_per_wave` is a hard cap regardless of percentage. Setting `pct_override=0` cleanly excludes an ISP family without deleting the row.

### 6c. Hot-swap a drip creative

Creative changes take effect on the **next** wave (no campaigns in flight are mutated). Deploys a new HTML file via Content Library first, then:

```bash
curl -sS -H "X-Admin-Key: $ADMIN" -H "Content-Type: application/json" \
  -X PUT https://projectjarvis.io/api/mailing/data-partners/creatives/refi_heloc/db \
  -d '{"creative_filename":"amerisave-db-newsletter-05142026.html","subject_line":"...","preheader":"...","from_name":"Jamie @ Discount Blog"}'
```

Path-traversal is rejected (`creative_filename` must be a bare basename).

### 6d. Audit log

UI: **Audit Log** sub-tab. API:

```bash
curl -sS -H "X-Admin-Key: $ADMIN" \
  'https://projectjarvis.io/api/mailing/data-partners/audit-log?limit=200&action=update_creative'
```

Filters: `action`, `actor`, `target_type`, `target_id`, `limit` (default 200, max 500).

---

## 7. Local Development & Testing Loop

The system runs entirely against `apex-postgres` for local work.

### 7a. Required local env

```bash
export DATABASE_URL="postgres://apex_user:apex_password@localhost:5432/ignite?sslmode=disable"
export PARTNER_INGEST_S3_BUCKET="jarvis-partner-ingest"
export PARTNER_INGEST_S3_REGION="us-west-2"
export ADMIN_API_KEY="local-admin-test-key"
export DEV_MODE=true   # bypasses session auth on /api routes for the Vite proxy
```

### 7b. Boot

```bash
cd upside-down && go build -o /tmp/ignite-server ./cmd/server && /tmp/ignite-server
# then in another shell:
cd upside-down/web && npx vite --port 5174
```

Port collisions: if `5173` is already in use by an unrelated project, override Vite's port (`--port 5174` works; the proxy still targets `localhost:8080`).

### 7c. Smoke test the full pipeline

```bash
# 1. Provision
PARTNER_ID=$(curl -sS -H "X-Admin-Key: local-admin-test-key" -H "Content-Type: application/json" \
  -X POST http://localhost:8080/api/mailing/data-partners \
  -d '{"name":"Local QA","slug":"local-qa"}' | jq -r '.id')

KEY=$(curl -sS -H "X-Admin-Key: local-admin-test-key" -H "Content-Type: application/json" \
  -X POST http://localhost:8080/api/mailing/data-partners/$PARTNER_ID/datasets \
  -d '{"name":"Local HELOC","slug":"local-heloc","vertical":"refi_heloc"}' | jq -r '.api_key')

# 2. Ingest
curl -sS -X POST http://localhost:8080/api/partner-ingest/v1/records \
  -H "X-Partner-Key: $KEY" -H "Content-Type: application/json" \
  -d '{"records":[{"email":"a@gmail.com"},{"email":"b@yahoo.com"}]}' | jq

# 3. Verify
docker exec apex-postgres psql -U apex_user -d ignite -c \
  "SELECT status, isp_family, vertical FROM partner_clean_queue ORDER BY ingested_at DESC LIMIT 5;"
```

Expected after step 3 (within ~10 s): rows with `status=pending_eo`, `isp_family=gmail|yahoo`, `vertical=refi_heloc`.

### 7d. Run the test suite

```bash
cd upside-down
go vet ./internal/api/... ./internal/worker/...
go test ./internal/api/... ./internal/worker/... -count=1
cd web && npx tsc --noEmit
```

The 34 partner-specific tests in `internal/api/partner_test.go` cover the admin handlers, ingest handlers, key middleware, S3 key builders, and pure helpers (`sanitizeSlug`, `parseIngestPayload`, `normalizeRecord`, etc.).

---

## 8. Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `dataset_paused` 503 from inbound POST | `paused_emergency=true` on the dataset | `POST .../resume` or check the audit log for `emergency_stop` |
| `s3_upload_failed` 502 from inbound POST | Bucket missing, IAM denied, or bucket region mismatch | Check the server log line `[partner-ingest] s3 upload failed bucket=… err=…` for the underlying SDK error |
| Records sit at `pending_eo` indefinitely | EO validator worker not consuming OR EO 401 on the NAT IP | Check `[partner-validator]` logs; verify `98.95.199.98` is in EO's allowlist |
| `last_wave_at` never updates | `ready_queue < wave_size` for that vertical OR all 4 brands are paused | Pull `partner_clean_queue` count where `status=ready` and check the brand's ISP pool status |
| `invalid_api_key` 401 from inbound POST | Wrong key OR the key was revoked OR partner status flipped to `inactive` | Verify the `key_prefix` on the partner's row matches the first 8 chars of the key they're sending |
| `bad_vertical` 400 from create-dataset | Vertical isn't in `{refi_heloc,personal_loans,tax_relief,remodel}` | These four are seeded in `partner_drip_creatives`. Adding a fifth requires a new migration to seed creatives for it across all 4 brands. |

For deeper diagnostics, the server log lines you want are:

```
[partner-slicer] processed batch=… records_in=… ready=… suppressed=…
[partner-validator] verified batch=… verified=… risky=… invalid=…
[partner-drip] wave vertical=… brand=… size=… ready_remaining=…
```

---

## 9. Reference: API Surface

### Partner-facing (X-Partner-Key)

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/partner-ingest/v1/schema` | Documentation; no auth |
| POST | `/api/partner-ingest/v1/records` | Ingest a batch |
| GET | `/api/partner-ingest/v1/batches/{id}` | Status of one batch (partner-scoped) |

### Operator-facing (X-Admin-Key or session)

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/mailing/data-partners/dashboard` | Verticals + recent batches |
| GET | `/api/mailing/data-partners` | List partners |
| POST | `/api/mailing/data-partners` | Create partner |
| GET | `/api/mailing/data-partners/datasets` | List datasets |
| POST | `/api/mailing/data-partners/{id}/datasets` | Create dataset (returns API key once) |
| GET | `/api/mailing/data-partners/datasets/{id}/throughput` | ISP breakdown + overrides + recommended wave size |
| PUT | `/api/mailing/data-partners/datasets/{id}/isp-distribution` | Set ISP overrides |
| POST | `/api/mailing/data-partners/datasets/{id}/emergency-stop` | Pause dataset |
| POST | `/api/mailing/data-partners/datasets/{id}/resume` | Unpause dataset |
| GET | `/api/mailing/data-partners/creatives` | List drip creatives (16 rows) |
| PUT | `/api/mailing/data-partners/creatives/{vertical}/{brand}` | Hot-swap creative |
| GET | `/api/mailing/data-partners/audit-log` | Recent admin events with filters |

---

## 10. What Lives Where in the Repo

| File | Purpose |
| --- | --- |
| `upside-down/internal/api/partner_admin_handlers.go` | All operator HTTP handlers + audit log |
| `upside-down/internal/api/partner_ingest_handlers.go` | Inbound API + payload parsing |
| `upside-down/internal/api/partner_api_key_middleware.go` | `X-Partner-Key` resolution |
| `upside-down/internal/api/partner_s3.go` | Bucketed gzip NDJSON storage |
| `upside-down/internal/api/partner_test.go` | 34-test sqlmock + httptest coverage |
| `upside-down/internal/worker/partner_slicer.go` | S3 NDJSON → clean_queue (FIFO + suppression) |
| `upside-down/internal/worker/partner_validator.go` | Clean queue → EmailOversight |
| `upside-down/internal/worker/partner_drip_orchestrator.go` | 15-min wave dispatch through PMTA |
| `upside-down/web/src/components/mailing/datapartners/` | Portal UI (Overview, Partners & Datasets, Inbound Batches, Drip Creatives, Audit Log) |
| `upside-down/cmd/server/main.go` | Migrations (search `dp_create_*`, `dp_seed_*`) |

---

## 11. Known Limits & Future Work

- **Per-record schema validation** is conservative: only `email` is required, only character-class validity is enforced. Vertical-specific business rules (e.g. ZIP must match state) are intentionally NOT enforced server-side — the partner is responsible for that. Add downstream if needed.
- **No re-key flow yet**: revoking a leaked key requires a manual SQL update (`UPDATE partner_api_keys SET status='revoked' WHERE key_prefix='…'`) plus issuing a new key via the create-dataset endpoint. A self-service rotation endpoint is a v2 add.
- **Single-creative-per-vertical-per-brand** is by design (no A/B yet). To run a creative test, hot-swap rapidly across waves and analyse via the existing campaign analytics; a `partner_drip_creative_variants` table is the v2 path.
- **No outbound webhook** to the partner when batches complete. They poll. A webhook subscription model is a v2 add.
