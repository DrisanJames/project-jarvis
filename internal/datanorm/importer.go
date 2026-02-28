package datanorm

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/google/uuid"
)

type Importer struct {
	db     *sql.DB
	orgID  string
	listID string
}

func NewImporter(db *sql.DB, orgID, listID string) *Importer {
	return &Importer{db: db, orgID: orgID, listID: listID}
}

const importBatchSize = 500

// ImportFromReader reads a CSV stream, maps columns to canonical fields,
// normalizes values, and imports records. Returns (imported, errors, err).
func (imp *Importer) ImportFromReader(ctx context.Context, r io.Reader, classification Classification, sourceFile string) (int, int, error) {
	reader := csv.NewReader(stripBOM(r))
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true

	header, err := reader.Read()
	if err != nil {
		if err == io.EOF {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("read header: %w", err)
	}

	mapping := MapColumns(header)
	if mapping == nil {
		return 0, 0, fmt.Errorf("no email column detected in header: %v", header)
	}

	var batch []*NormalizedRecord
	imported, errCount := 0, 0

	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			errCount++
			continue
		}

		rec := NormalizeRecord(row, mapping, sourceFile)
		if !isValidEmail(rec.Email) {
			errCount++
			continue
		}

		// Fill domain group from email if not set by a dedicated column
		if rec.DomainGroup == "" {
			rec.DomainGroup = InferDomainGroupFromEmail(rec.Email)
		}

		batch = append(batch, rec)

		if len(batch) >= importBatchSize {
			n, e := imp.flushBatch(ctx, classification, batch)
			imported += n
			errCount += e
			batch = batch[:0]
		}
	}

	if len(batch) > 0 {
		n, e := imp.flushBatch(ctx, classification, batch)
		imported += n
		errCount += e
	}

	return imported, errCount, nil
}

func (imp *Importer) flushBatch(ctx context.Context, classification Classification, records []*NormalizedRecord) (int, int) {
	switch classification {
	case ClassMailable:
		return imp.importMailable(ctx, records, "jvc-import")
	case ClassSuppression:
		return imp.importSuppression(ctx, records)
	case ClassWarmup:
		return imp.importMailable(ctx, records, "jvc-warmup")
	default:
		return 0, len(records)
	}
}

func (imp *Importer) importMailable(ctx context.Context, records []*NormalizedRecord, dataSource string) (int, int) {
	tx, err := imp.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, len(records)
	}

	imported, errCount := 0, 0
	for _, rec := range records {
		_, err := tx.ExecContext(ctx, "SAVEPOINT batch_sp")
		if err != nil {
			errCount++
			continue
		}

		emailHash := sha256Hex(rec.Email)
		subID := uuid.New()

		customFields := buildCustomFields(rec)
		customJSON, _ := json.Marshal(customFields)

		_, err = tx.ExecContext(ctx,
			`INSERT INTO mailing_subscribers
				(id, organization_id, list_id, email, email_hash, status,
				 first_name, last_name, source,
				 data_source, data_quality_score, verification_status,
				 custom_fields, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'confirmed',
				$6, $7, $8,
				$9, $10, $11,
				$12, NOW(), NOW())
			ON CONFLICT (list_id, email) DO UPDATE SET
				first_name = COALESCE(NULLIF(EXCLUDED.first_name, ''), mailing_subscribers.first_name),
				last_name = COALESCE(NULLIF(EXCLUDED.last_name, ''), mailing_subscribers.last_name),
				data_quality_score = GREATEST(EXCLUDED.data_quality_score, mailing_subscribers.data_quality_score),
				verification_status = CASE
					WHEN EXCLUDED.verification_status IN ('verified','invalid') THEN EXCLUDED.verification_status
					ELSE COALESCE(mailing_subscribers.verification_status, EXCLUDED.verification_status)
				END,
				custom_fields = mailing_subscribers.custom_fields || EXCLUDED.custom_fields,
				data_source = COALESCE(EXCLUDED.data_source, mailing_subscribers.data_source),
				updated_at = NOW()`,
			subID, imp.orgID, imp.listID, rec.Email, emailHash,
			rec.FirstName, rec.LastName, rec.SourceFile,
			dataSource, rec.QualityScore, rec.VerificationStatus,
			string(customJSON),
		)
		if err != nil {
			tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT batch_sp")
			errCount++
			continue
		}

		tx.ExecContext(ctx, "RELEASE SAVEPOINT batch_sp")
		imported++
	}

	if err := tx.Commit(); err != nil {
		log.Printf("[datanorm] commit error: %v", err)
		return 0, len(records)
	}
	return imported, errCount
}

func (imp *Importer) importSuppression(ctx context.Context, records []*NormalizedRecord) (int, int) {
	tx, err := imp.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, len(records)
	}

	imported, errCount := 0, 0
	for _, rec := range records {
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(rec.Email)))
		md5hash := hash[:32]

		reason := "imported"
		if rec.BounceCategory != "" {
			reason = "bounce:" + rec.BounceCategory
		}
		source := "jvc-import"
		if rec.SourceFile != "" {
			source = rec.SourceFile
		}

		_, err := tx.ExecContext(ctx,
			`INSERT INTO mailing_global_suppressions
				(organization_id, email, md5_hash, reason, source, isp, dsn_code, dsn_diag, source_ip, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW())
			ON CONFLICT (organization_id, md5_hash) DO NOTHING`,
			imp.orgID, rec.Email, md5hash, reason, source,
			rec.DomainGroup, rec.DSNStatus, rec.DSNDiag, rec.SourceIP,
		)
		if err != nil {
			errCount++
			continue
		}
		imported++
	}

	if err := tx.Commit(); err != nil {
		log.Printf("[datanorm] suppression commit error: %v", err)
		return 0, len(records)
	}
	return imported, errCount
}

// buildCustomFields constructs the JSONB custom_fields from the normalized record,
// including only non-empty enrichment data not stored in dedicated columns.
func buildCustomFields(rec *NormalizedRecord) map[string]interface{} {
	cf := make(map[string]interface{})

	if rec.City != "" {
		cf["city"] = rec.City
	}
	if rec.State != "" {
		cf["state"] = rec.State
	}
	if rec.Country != "" {
		cf["country"] = rec.Country
	}
	if rec.Zip != "" {
		cf["zip"] = rec.Zip
	}
	if rec.Phone != "" {
		cf["phone"] = rec.Phone
	}
	if rec.DomainGroup != "" {
		cf["domain_group"] = rec.DomainGroup
	}
	if rec.EngagementBehavior != "" {
		cf["engagement_behavior"] = rec.EngagementBehavior
	}
	if rec.IsRole {
		cf["is_role"] = true
	}
	if rec.IsDisposable {
		cf["is_disposable"] = true
	}
	if rec.IsBot {
		cf["is_bot"] = true
	}
	if rec.SourceSignal != "" {
		cf["source_signal"] = rec.SourceSignal
	}
	if rec.ExternalID != "" {
		cf["external_id"] = rec.ExternalID
	}

	// Remaining unmapped columns
	for k, v := range rec.Extra {
		cf[k] = v
	}

	return cf
}

func isValidEmail(email string) bool {
	if len(email) < 5 || len(email) > 254 {
		return false
	}
	at := strings.LastIndex(email, "@")
	if at < 1 || at >= len(email)-1 {
		return false
	}
	domain := email[at+1:]
	if !strings.Contains(domain, ".") || len(domain) < 3 {
		return false
	}
	return true
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}

// stripBOM wraps a reader to strip a UTF-8 BOM if present.
func stripBOM(r io.Reader) io.Reader {
	buf := make([]byte, 3)
	n, err := r.Read(buf)
	if err != nil || n < 3 {
		return io.MultiReader(strings.NewReader(string(buf[:n])), r)
	}
	if buf[0] == 0xEF && buf[1] == 0xBB && buf[2] == 0xBF {
		return r
	}
	return io.MultiReader(strings.NewReader(string(buf[:n])), r)
}
