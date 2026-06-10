package api

// Bulk-tag canonical loader — server-side admin endpoint that mirrors
// the local Python loader (scripts/import/load_eo_harvest_keepers.py and
// .scratch/apr29_load_trugreen_attribits.py) so vendor batches can be
// loaded into mailing_subscribers from any laptop that can reach
// projectjarvis.io over HTTPS, without needing direct RDS connectivity.
//
// Why this exists
// ---------------
// The existing /api/mailing/lists/{listId}/subscribers/import endpoint
// writes tags to the legacy mailing_subscriber_tags junction table. The
// V2 segmentation engine queries the mailing_subscribers.tags TEXT[]
// array column (see internal/segmentation/query_builder.go
// buildTagCondition). The two stores are not synchronized, so a segment
// using "tag contains_any" on imports that went through the old path
// resolves to zero rows.
//
// load_eo_harvest_keepers.py and friends bypass that mismatch by
// COPY'ing into a TEMP staging table and MERGE'ing into mailing_subscribers
// with the canonical tags column populated. This handler runs the
// equivalent SQL inside the production process, so an operator can POST
// the canonical CSV produced by the Python preparation step over
// HTTPS and have the production server (which already holds an RDS
// connection in its private subnet) do the heavy lifting.
//
// Wire format
// -----------
// POST /api/admin/bulk-tag-canonical
// Headers:
//   X-Admin-Key:    required (must equal $ADMIN_API_KEY)
//   Content-Type:   text/csv
//   X-Organization-ID:  optional, defaults to "00000000-0000-0000-0000-000000000001"
// Query params (URL-encoded):
//   list_name         (default: "Verified External Imports - Master")
//   segment_name      required (e.g. "TruGreen-Attribits-Apr2026")
//   segment_tag       required (offer/segment-defining tag, e.g. "offer:trugreen:apr2026")
//   segment_description optional
//   data_set          optional, <=10 chars (data_set column constraint)
//   vendor            optional (audit row)
//   batch_key         optional (audit row)
//   datatype          optional (audit row)
//   batch_size        optional, default 1000
// Body: canonical CSV with header row and columns:
//   email, email_hash, first_name, last_name, isp, eo_domain_group,
//   tags, source_detail, source_metadata, custom_fields
//
// Response: chunked NDJSON. One JSON object per line. The final line
// has phase="done" and contains the audit summary. Intermediate lines
// have phase in {start, merge, segment, materialize, audit} and exist
// to keep the ALB idle timer alive on long batches.

import (
	"bufio"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/mailing"
	"github.com/lib/pq"
)

const (
	bulkTagDefaultListName = "Verified External Imports - Master"
	bulkTagDefaultBatch    = 1000
	bulkTagMaxBatch        = 5000
	bulkTagDefaultOrgID    = "00000000-0000-0000-0000-000000000001"

	// VersionBulkTagCanonical is the handler version. Bump on any
	// behavior change so log greps and clients can confirm the running
	// build matches what they expect.
	//
	// 1.4 — 2026-05-06: rejects rows whose email fails
	//        mailing.ClassifyEmailForIngest (typo-trap, disposable,
	//        litigator, role-based). Skipped rows are reported per
	//        reason in the audit summary.
	VersionBulkTagCanonical = "1.4"
)

// HandleBulkTagCanonical streams a canonical CSV into mailing_subscribers
// and creates/refreshes a dynamic tag-based segment.
func HandleBulkTagCanonical(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()

		// Auth — same pattern as /api/admin/* routes registered nearby.
		adminKey := os.Getenv("ADMIN_API_KEY")
		if adminKey == "" || r.Header.Get("X-Admin-Key") != adminKey {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		q := r.URL.Query()
		segmentName := strings.TrimSpace(q.Get("segment_name"))
		segmentTag := strings.TrimSpace(q.Get("segment_tag"))
		if segmentName == "" || segmentTag == "" {
			http.Error(w, `{"error":"segment_name and segment_tag are required query params"}`, http.StatusBadRequest)
			return
		}
		segmentDesc := q.Get("segment_description")
		if segmentDesc == "" {
			segmentDesc = fmt.Sprintf("Auto-created by bulk-tag-canonical for tag %q.", segmentTag)
		}
		listName := q.Get("list_name")
		if listName == "" {
			listName = bulkTagDefaultListName
		}
		dataSet := q.Get("data_set")
		if len(dataSet) > 10 {
			dataSet = dataSet[:10]
		}
		vendor := q.Get("vendor")
		batchKey := q.Get("batch_key")
		datatype := q.Get("datatype")
		orgID := r.Header.Get("X-Organization-ID")
		if orgID == "" {
			orgID = bulkTagDefaultOrgID
		}
		batchSize := bulkTagDefaultBatch
		if v := q.Get("batch_size"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				if n > bulkTagMaxBatch {
					n = bulkTagMaxBatch
				}
				batchSize = n
			}
		}
		// Source URL — when set, the server fetches the gzipped
		// canonical CSV from this URL instead of reading the request
		// body. Designed for operators on slow/flaky uplinks who
		// cannot keep a streaming POST alive for ~5 min.
		// Workflow: upload to S3 with `aws s3 cp` (multipart,
		// resumable), generate a presigned URL with `aws s3 presign`,
		// then POST a near-empty body with this query param set. The
		// server pulls the CSV from S3 (us-west-2 → us-west-2 ECS is
		// fast and reliable) and processes normally.
		csvURL := strings.TrimSpace(q.Get("csv_url"))

		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("X-Handler-Version", VersionBulkTagCanonical)
		// Critical: write headers immediately so the client knows the
		// server accepted the request even before the first batch
		// completes. Without this the client sees nothing for minutes.
		flusher, _ := w.(http.Flusher)

		emit := func(payload map[string]interface{}) {
			payload["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
			b, _ := json.Marshal(payload)
			b = append(b, '\n')
			_, _ = w.Write(b)
			if flusher != nil {
				flusher.Flush()
			}
		}

		emit(map[string]interface{}{
			"phase":          "received",
			"version":        VersionBulkTagCanonical,
			"segment_name":   segmentName,
			"segment_tag":    segmentTag,
			"list_name":      listName,
			"organization":   orgID,
			"batch_size":     batchSize,
			"data_set":       dataSet,
			"vendor":         vendor,
			"batch_key":      batchKey,
			"datatype":       datatype,
		})

		// Detach context: ALB might cancel the long-running request,
		// but the work should still complete. We only watch r.Context()
		// for explicit client disconnects in the streaming loop.
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
		defer cancel()

		// Sticky connection so the TEMP table survives across COPY,
		// MERGE batches, segment ensure, and materialize.
		conn, err := db.Conn(ctx)
		if err != nil {
			emit(map[string]interface{}{"phase": "error", "where": "acquire_conn", "error": err.Error()})
			return
		}
		defer conn.Close()

		// Resolve list id.
		var listID string
		if err := conn.QueryRowContext(ctx,
			"SELECT id FROM mailing_lists WHERE organization_id = $1 AND name = $2",
			orgID, listName,
		).Scan(&listID); err != nil {
			emit(map[string]interface{}{"phase": "error", "where": "resolve_list", "list_name": listName, "error": err.Error()})
			return
		}
		emit(map[string]interface{}{"phase": "list_resolved", "list_id": listID})

		// Stage TEMP table. ON COMMIT PRESERVE ROWS so we can commit
		// after COPY and continue on subsequent statements with the
		// staged rows still present.
		if _, err := conn.ExecContext(ctx, `
			CREATE TEMP TABLE _bulk_stage (
				batch_row       BIGSERIAL NOT NULL,
				email           TEXT    PRIMARY KEY,
				email_hash      TEXT    NOT NULL,
				first_name      TEXT,
				last_name       TEXT,
				isp             TEXT,
				eo_domain_group TEXT,
				tags            TEXT[]  NOT NULL,
				source_detail   TEXT,
				source_metadata JSONB   NOT NULL DEFAULT '{}'::jsonb,
				custom_fields   JSONB   NOT NULL DEFAULT '{}'::jsonb
			) ON COMMIT PRESERVE ROWS
		`); err != nil {
			emit(map[string]interface{}{"phase": "error", "where": "create_temp", "error": err.Error()})
			return
		}
		if _, err := conn.ExecContext(ctx, `CREATE INDEX _bulk_stage_batch_row_idx ON _bulk_stage (batch_row)`); err != nil {
			emit(map[string]interface{}{"phase": "error", "where": "create_temp_index", "error": err.Error()})
			return
		}

		// Stream-copy the request body via lib/pq's CopyIn protocol. We
		// can't pass it through the database/sql interface because lib/pq
		// requires a tx + prepared stmt + per-row Exec, and the TEMP
		// table must stay alive after the COPY tx commits.
		emit(map[string]interface{}{"phase": "copy_started"})
		copyStart := time.Now()

		copyTx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			emit(map[string]interface{}{"phase": "error", "where": "begin_copy_tx", "error": err.Error()})
			return
		}
		copyStmt, err := copyTx.Prepare(pq.CopyIn("_bulk_stage",
			"email", "email_hash", "first_name", "last_name", "isp",
			"eo_domain_group", "tags", "source_detail", "source_metadata", "custom_fields",
		))
		if err != nil {
			_ = copyTx.Rollback()
			emit(map[string]interface{}{"phase": "error", "where": "prepare_copy", "error": err.Error()})
			return
		}

		// Body decoding — three modes:
		//   1. csv_url query param set → server GETs that URL (intended
		//      for S3 presigned URLs). Always treated as gzip.
		//   2. Content-Encoding: gzip header set → request body is
		//      decoded through gzip.NewReader.
		//   3. Plain text/csv body.
		// Mode 1 exists because operators on slow uplinks cannot reliably
		// keep a streaming POST alive long enough to upload 5+ MB of
		// gzipped CSV. `aws s3 cp` handles their hotspot's flakiness via
		// multipart resumable upload, then this handler does the
		// us-west-2 → us-west-2 fetch in seconds.
		var rawBody io.Reader = r.Body
		var bodyCloser io.Closer
		if csvURL != "" {
			emit(map[string]interface{}{"phase": "csv_url_fetching", "csv_url": csvURL})
			fetchStart := time.Now()
			fetchCtx, fetchCancel := context.WithTimeout(ctx, 5*time.Minute)
			defer fetchCancel()
			req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, csvURL, nil)
			if err != nil {
				_ = copyTx.Rollback()
				emit(map[string]interface{}{"phase": "error", "where": "csv_url_request_build", "error": err.Error()})
				return
			}
			httpClient := &http.Client{Timeout: 5 * time.Minute}
			resp, err := httpClient.Do(req)
			if err != nil {
				_ = copyTx.Rollback()
				emit(map[string]interface{}{"phase": "error", "where": "csv_url_fetch", "error": err.Error()})
				return
			}
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				_ = copyTx.Rollback()
				emit(map[string]interface{}{"phase": "error", "where": "csv_url_status", "status": resp.StatusCode})
				return
			}
			bodyCloser = resp.Body
			gz, err := gzip.NewReader(resp.Body)
			if err != nil {
				resp.Body.Close()
				_ = copyTx.Rollback()
				emit(map[string]interface{}{"phase": "error", "where": "csv_url_gzip_decode", "error": err.Error()})
				return
			}
			defer gz.Close()
			rawBody = gz
			emit(map[string]interface{}{
				"phase":          "csv_url_fetched",
				"fetch_seconds":  time.Since(fetchStart).Seconds(),
				"content_length": resp.ContentLength,
			})
		} else if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				_ = copyTx.Rollback()
				emit(map[string]interface{}{"phase": "error", "where": "gzip_decode", "error": err.Error()})
				return
			}
			defer gz.Close()
			rawBody = gz
		}
		if bodyCloser != nil {
			defer bodyCloser.Close()
		}
		// Robust line reader — supports very long lines (custom_fields
		// can be multi-KB JSON per row).
		bodyReader := bufio.NewReaderSize(rawBody, 1<<20) // 1 MiB read buffer
		// CSV with quotes, commas inside JSON tags & metadata, etc.
		// We need a CSV decoder that handles RFC 4180 quoting.
		// Use encoding/csv with field_size_limit equivalent (Go has no
		// hard limit; just need a Reader with adequate ReadBufferSize).
		csvReader := newCanonicalCSVReader(bodyReader)
		header, err := csvReader.Read()
		if err != nil {
			_ = copyTx.Rollback()
			emit(map[string]interface{}{"phase": "error", "where": "read_header", "error": err.Error()})
			return
		}
		colIdx, err := canonicalColIndex(header)
		if err != nil {
			_ = copyTx.Rollback()
			emit(map[string]interface{}{"phase": "error", "where": "validate_header", "error": err.Error(), "header": header})
			return
		}

		copied := 0
		ingestSkipped := 0
		ingestSkippedByReason := map[string]int{}
		lastEmit := time.Now()
		for {
			rec, err := csvReader.Read()
			if err != nil {
				if isEOF(err) {
					break
				}
				_ = copyTx.Rollback()
				emit(map[string]interface{}{"phase": "error", "where": "read_csv", "row": copied, "error": err.Error()})
				return
			}
			if len(rec) < len(header) {
				continue
			}
			email := strings.TrimSpace(rec[colIdx["email"]])
			if email == "" {
				continue
			}

			// Layer-1 ingest guard. Same set the legacy
			// /api/mailing/lists/{id}/subscribers/import path uses, so
			// vendor batches POSTed via canonical CSV cannot smuggle
			// litigator / honeypot / disposable / role-based addresses
			// into mailing_subscribers. Pre-existing rows are
			// untouched — this only filters new MERGE candidates.
			if decision := mailing.ClassifyEmailForIngest(email); !decision.Accept {
				ingestSkipped++
				ingestSkippedByReason[decision.Reason]++
				continue
			}

			tagsLit := strings.TrimSpace(rec[colIdx["tags"]])
			if tagsLit == "" {
				tagsLit = "{}"
			}
			sourceMeta := strings.TrimSpace(rec[colIdx["source_metadata"]])
			if sourceMeta == "" {
				sourceMeta = "{}"
			}
			customFields := strings.TrimSpace(rec[colIdx["custom_fields"]])
			if customFields == "" {
				customFields = "{}"
			}

			if _, err := copyStmt.ExecContext(ctx,
				email,
				rec[colIdx["email_hash"]],
				nullIfEmpty(rec[colIdx["first_name"]]),
				nullIfEmpty(rec[colIdx["last_name"]]),
				nullIfEmpty(rec[colIdx["isp"]]),
				nullIfEmpty(rec[colIdx["eo_domain_group"]]),
				tagsLit,
				nullIfEmpty(rec[colIdx["source_detail"]]),
				sourceMeta,
				customFields,
			); err != nil {
				_ = copyTx.Rollback()
				emit(map[string]interface{}{"phase": "error", "where": "copy_exec", "row": copied, "email": email, "error": err.Error()})
				return
			}
			copied++

			// Heartbeat every 10s so ALB & client see the connection
			// is alive even when the body is huge and the COPY phase
			// is the long part.
			if time.Since(lastEmit) > 10*time.Second {
				emit(map[string]interface{}{"phase": "copy_progress", "rows": copied})
				lastEmit = time.Now()
			}
		}
		// Flush COPY buffer.
		if _, err := copyStmt.ExecContext(ctx); err != nil {
			_ = copyTx.Rollback()
			emit(map[string]interface{}{"phase": "error", "where": "copy_flush", "rows": copied, "error": err.Error()})
			return
		}
		if err := copyStmt.Close(); err != nil {
			_ = copyTx.Rollback()
			emit(map[string]interface{}{"phase": "error", "where": "copy_close", "rows": copied, "error": err.Error()})
			return
		}
		if err := copyTx.Commit(); err != nil {
			emit(map[string]interface{}{"phase": "error", "where": "copy_commit", "rows": copied, "error": err.Error()})
			return
		}
		copyDur := time.Since(copyStart)

		var staged int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM _bulk_stage").Scan(&staged); err != nil {
			emit(map[string]interface{}{"phase": "error", "where": "stage_count", "error": err.Error()})
			return
		}
		emit(map[string]interface{}{
			"phase":                    "staged",
			"rows":                     staged,
			"copy_seconds":             copyDur.Seconds(),
			"ingest_skipped":           ingestSkipped,
			"ingest_skipped_by_reason": ingestSkippedByReason,
		})
		if staged == 0 {
			emit(map[string]interface{}{
				"phase":                    "error",
				"where":                    "stage_empty",
				"ingest_skipped":           ingestSkipped,
				"ingest_skipped_by_reason": ingestSkippedByReason,
			})
			return
		}

		// MERGE into mailing_subscribers — same canonical SQL the Python
		// loader runs. Per-batch tx so each commit is short and
		// concurrent writes can interleave with our work.
		mergeSQL := `
			WITH upsert AS (
				INSERT INTO mailing_subscribers (
					organization_id, list_id, email, email_hash,
					first_name, last_name, status, source,
					data_source, data_set, verification_status, verified_at,
					isp, custom_fields, tags, created_at, updated_at
				)
				SELECT $1::uuid, $2::uuid, s.email, s.email_hash,
					NULLIF(s.first_name, ''), NULLIF(s.last_name, ''),
					'confirmed', 'vendor_import',
					s.source_detail, NULLIF($3, ''), 'verified', NOW(),
					NULLIF(s.isp, ''),
					s.custom_fields || jsonb_build_object(
						'source', jsonb_build_object(
							'detail', s.source_detail,
							'metadata', s.source_metadata
						)
					),
					s.tags, NOW(), NOW()
				FROM _bulk_stage s
				WHERE s.batch_row > $4 AND s.batch_row <= $5
				ON CONFLICT (list_id, email) DO UPDATE SET
					tags = (
						SELECT COALESCE(array_agg(DISTINCT t ORDER BY t), ARRAY[]::text[])
						FROM unnest(mailing_subscribers.tags || EXCLUDED.tags) AS t
					),
					custom_fields = jsonb_set(
						COALESCE(mailing_subscribers.custom_fields, '{}'::jsonb) || EXCLUDED.custom_fields,
						'{provenance,batches}',
						COALESCE(mailing_subscribers.custom_fields->'provenance'->'batches', '[]'::jsonb)
							|| COALESCE(EXCLUDED.custom_fields->'provenance'->'batches', '[]'::jsonb),
						true
					),
					data_source = COALESCE(EXCLUDED.data_source, mailing_subscribers.data_source),
					data_set = COALESCE(mailing_subscribers.data_set, EXCLUDED.data_set),
					verification_status = CASE
						WHEN mailing_subscribers.verification_status IN ('verified') THEN mailing_subscribers.verification_status
						ELSE EXCLUDED.verification_status
					END,
					verified_at = COALESCE(mailing_subscribers.verified_at, EXCLUDED.verified_at),
					isp = COALESCE(NULLIF(mailing_subscribers.isp, ''), EXCLUDED.isp),
					first_name = COALESCE(NULLIF(mailing_subscribers.first_name, ''), EXCLUDED.first_name),
					last_name  = COALESCE(NULLIF(mailing_subscribers.last_name, ''),  EXCLUDED.last_name),
					updated_at = NOW()
				RETURNING (xmax = 0) AS inserted
			)
			SELECT
				COUNT(*) FILTER (WHERE inserted) AS inserted,
				COUNT(*) FILTER (WHERE NOT inserted) AS merged
			FROM upsert
		`
		emit(map[string]interface{}{"phase": "merge_started", "batch_size": batchSize})
		merged := 0
		inserted := 0
		mergeStart := time.Now()
		offset := 0
		for offset < staged {
			end := offset + batchSize
			tBatch := time.Now()
			var ins, mer int
			err := func() error {
				batchTx, err := conn.BeginTx(ctx, nil)
				if err != nil {
					return fmt.Errorf("begin batch tx: %w", err)
				}
				if _, err := batchTx.ExecContext(ctx, "SET LOCAL statement_timeout = '180s'"); err != nil {
					_ = batchTx.Rollback()
					return err
				}
				if _, err := batchTx.ExecContext(ctx, "SET LOCAL lock_timeout = '15s'"); err != nil {
					_ = batchTx.Rollback()
					return err
				}
				if err := batchTx.QueryRowContext(ctx, mergeSQL, orgID, listID, dataSet, offset, end).Scan(&ins, &mer); err != nil {
					_ = batchTx.Rollback()
					return err
				}
				return batchTx.Commit()
			}()
			if err != nil {
				emit(map[string]interface{}{"phase": "error", "where": "merge_batch", "offset": offset, "error": err.Error()})
				return
			}
			inserted += ins
			merged += mer
			emit(map[string]interface{}{
				"phase":            "merge_batch",
				"offset":           offset,
				"end":              minInt(end, staged),
				"inserted":         ins,
				"merged":           mer,
				"batch_seconds":    time.Since(tBatch).Seconds(),
				"running_inserted": inserted,
				"running_merged":   merged,
			})
			offset = end
		}
		emit(map[string]interface{}{
			"phase":           "merged",
			"inserted":        inserted,
			"merged":          merged,
			"merge_seconds":   time.Since(mergeStart).Seconds(),
		})

		// Segment upsert — idempotent on (org_id, name).
		segmentConditions := map[string]interface{}{
			"logic_operator": "AND",
			"conditions": []map[string]interface{}{
				{
					"condition_type": "tag",
					"field":          "tags",
					"operator":       "contains_any",
					"values_array":   []string{segmentTag},
				},
			},
		}
		segCondJSON, _ := json.Marshal(segmentConditions)

		var segmentID string
		if err := conn.QueryRowContext(ctx,
			"SELECT id FROM mailing_segments WHERE organization_id = $1 AND name = $2",
			orgID, segmentName,
		).Scan(&segmentID); err != nil && err != sql.ErrNoRows {
			emit(map[string]interface{}{"phase": "error", "where": "segment_lookup", "error": err.Error()})
			return
		}
		if segmentID == "" {
			if err := conn.QueryRowContext(ctx, `
				INSERT INTO mailing_segments (
					id, organization_id, list_id, name, description,
					segment_type, conditions, status, subscriber_count,
					created_at, updated_at
				)
				VALUES (
					gen_random_uuid(), $1, $2, $3, $4,
					'dynamic', $5, 'active', 0, NOW(), NOW()
				)
				RETURNING id
			`, orgID, listID, segmentName, segmentDesc, string(segCondJSON)).Scan(&segmentID); err != nil {
				emit(map[string]interface{}{"phase": "error", "where": "segment_insert", "error": err.Error()})
				return
			}
			emit(map[string]interface{}{"phase": "segment_created", "segment_id": segmentID})
		} else {
			if _, err := conn.ExecContext(ctx, `
				UPDATE mailing_segments
				   SET description = $1,
				       segment_type = 'dynamic',
				       conditions = $2,
				       list_id = $3,
				       status = 'active',
				       updated_at = NOW()
				 WHERE id = $4
			`, segmentDesc, string(segCondJSON), listID, segmentID); err != nil {
				emit(map[string]interface{}{"phase": "error", "where": "segment_update", "error": err.Error()})
				return
			}
			emit(map[string]interface{}{"phase": "segment_updated", "segment_id": segmentID})
		}

		// Materialize members — match production's segment_materializer
		// behavior for a tag condition.
		emit(map[string]interface{}{"phase": "materialize_started"})
		matStart := time.Now()
		if _, err := conn.ExecContext(ctx,
			"DELETE FROM mailing_segment_members WHERE segment_id = $1::uuid", segmentID,
		); err != nil {
			emit(map[string]interface{}{"phase": "error", "where": "materialize_delete", "error": err.Error()})
			return
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO mailing_segment_members (segment_id, subscriber_id, email, materialized_at)
			SELECT $1::uuid, s.id, s.email, NOW()
			  FROM mailing_subscribers s
			 WHERE s.organization_id = $2::uuid
			   AND s.status = 'confirmed'
			   AND s.tags && ARRAY[$3]::text[]
			ON CONFLICT (segment_id, subscriber_id) DO NOTHING
		`, segmentID, orgID, segmentTag); err != nil {
			emit(map[string]interface{}{"phase": "error", "where": "materialize_insert", "error": err.Error()})
			return
		}
		var members int
		if err := conn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM mailing_segment_members WHERE segment_id = $1::uuid", segmentID,
		).Scan(&members); err != nil {
			emit(map[string]interface{}{"phase": "error", "where": "materialize_count", "error": err.Error()})
			return
		}
		if _, err := conn.ExecContext(ctx,
			"UPDATE mailing_segments SET subscriber_count = $1 WHERE id = $2::uuid",
			members, segmentID,
		); err != nil {
			emit(map[string]interface{}{"phase": "error", "where": "segment_count_update", "error": err.Error()})
			return
		}
		// Best-effort build-ledger upsert so the v2 segments list shows the
		// authoritative member count for this segment without re-counting the
		// rollup (segment_ledger.go). Observability only — never fails the run.
		if err := UpsertSegmentLedger(ctx, db, segmentID, int64(members), "bulk-tag", "ok", 0, 0, ""); err != nil {
			log.Printf("[BulkTag] segment ledger upsert failed for %s (continuing): %v", segmentID, err)
		}
		emit(map[string]interface{}{
			"phase":               "materialized",
			"members":             members,
			"materialize_seconds": time.Since(matStart).Seconds(),
		})

		// Audit row — best-effort. UNIQUE on (org_id, vendor, batch_key)
		// so a re-run safely skips this step.
		auditID := ""
		if vendor != "" && batchKey != "" {
			cfg, _ := json.Marshal(map[string]interface{}{
				"segment_name":  segmentName,
				"segment_tag":   segmentTag,
				"data_set":      dataSet,
				"datatype":      datatype,
				"version":       VersionBulkTagCanonical,
				"copy_seconds":  copyDur.Seconds(),
				"merge_seconds": time.Since(mergeStart).Seconds(),
			})
			err := conn.QueryRowContext(ctx, `
				INSERT INTO mailing_vendor_batch_audit (
					organization_id, vendor, batch_key, datatype,
					config_snapshot, verified_count, merged_count,
					inserted_count, suppressed_count, segment_id, list_id,
					notes
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 0, $9::uuid, $10::uuid, $11)
				RETURNING id
			`,
				orgID, vendor, batchKey, datatype,
				string(cfg), staged, merged, inserted,
				segmentID, listID,
				fmt.Sprintf("bulk-tag-canonical v%s; segment members materialized: %d",
					VersionBulkTagCanonical, members),
			).Scan(&auditID)
			if err != nil {
				log.Printf("[bulk-tag-canonical] audit insert non-fatal error: %v", err)
				emit(map[string]interface{}{"phase": "audit_skipped", "error": err.Error()})
			} else {
				emit(map[string]interface{}{"phase": "audit_inserted", "audit_id": auditID})
			}
		}

		emit(map[string]interface{}{
			"phase":                    "done",
			"version":                  VersionBulkTagCanonical,
			"list_id":                  listID,
			"segment_id":               segmentID,
			"segment_name":             segmentName,
			"segment_tag":              segmentTag,
			"staged":                   staged,
			"inserted":                 inserted,
			"merged":                   merged,
			"materialized":             members,
			"ingest_skipped":           ingestSkipped,
			"ingest_skipped_by_reason": ingestSkippedByReason,
			"audit_id":                 auditID,
			"copy_seconds":             copyDur.Seconds(),
			"merge_seconds":            time.Since(mergeStart).Seconds(),
			"materialize_seconds":      time.Since(matStart).Seconds(),
			"total_seconds":            time.Since(started).Seconds(),
		})
	}
}

// isEOF tolerates encoding/csv's EOF as well as our wrapper's EOF
// sentinel. We don't import io here because the real encoding/csv path
// goes through canonicalCSVReader, which converts io.EOF into a
// stringly "EOF" error.
func isEOF(err error) bool {
	return err != nil && err.Error() == "EOF"
}
