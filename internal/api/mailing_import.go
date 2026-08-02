package api

import (
	"bytes"
	"crypto/md5"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// MaxImportFileBytes is the hard ceiling on a single CSV posted to
// POST /api/mailing/lists/{listId}/import. A file above it is REJECTED
// (REQ-069, 2026-08-01).
//
// Before this constant existed the handler read the upload through
// io.LimitReader(file, 32*1024*1024). io.LimitReader returns EOF, not an
// error, so every byte past 32 MB was discarded silently and the job still
// terminated status='completed' — a ~400k-row list imported partially and read
// as a successful import. Rejecting is the honest failure.
//
// Lifting the ceiling means streaming/COPY instead of a full in-memory read;
// that is REQ-075 and is deliberately NOT done here.
const MaxImportFileBytes = 32 * 1024 * 1024

// MaxImportFileMB is the human-readable form of MaxImportFileBytes used in the
// rejection message and mirrored by the UI next to the file picker.
const MaxImportFileMB = MaxImportFileBytes / (1024 * 1024)

func (s *AdvancedMailingService) HandleImportSubscribers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	listID := chi.URLParam(r, "listId")
	listUUID, _ := uuid.Parse(listID)

	// Parse multipart form. NOTE: this argument is the in-MEMORY budget, not a
	// size limit — bigger parts spill to a temp file — so the real ceiling is
	// enforced explicitly below.
	r.ParseMultipartForm(32 << 20)

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"file required"}`, http.StatusBadRequest)
		return
	}
	defer file.Close()

	// REQ-069: reject oversize BEFORE the mailing_import_jobs INSERT, so an
	// over-ceiling upload leaves no job row at all rather than a job row that
	// reports 'completed' over a truncated file.
	if header.Size > MaxImportFileBytes {
		respondError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"file is %.1f MB — over the %d MB import limit; split the file and import the parts",
			float64(header.Size)/(1024*1024), MaxImportFileMB))
		return
	}

	// REQ-068: field mapping from the form. There is deliberately NO positional
	// fallback here. The deleted default mapped email to column 0, first_name
	// to column 1 and last_name to column 2; being non-empty it shadowed the
	// header auto-mapper for EVERY upload, so any CSV that was not exactly
	// email,first,last in that order either imported nothing or wrote ZIP and
	// state values into first_name/last_name — which the send path then
	// personalizes mail with.
	fieldMapping := r.FormValue("field_mapping")
	// An absent mapping falls through to the header auto-mapper: mapping stays
	// nil here and processCSVImportEnhanced resolves it from the header row via
	// resolveImportMapping. Never positionally.
	var mapping map[string]int
	if fieldMapping != "" {
		if err := json.Unmarshal([]byte(fieldMapping), &mapping); err != nil {
			respondError(w, http.StatusBadRequest,
				"field_mapping must be a JSON object of {field: column_index}")
			return
		}
	} else {
		// field_mapping is a JSONB column; '' is not valid JSON.
		fieldMapping = "{}"
	}

	// Get update_existing flag (defaults to true)
	updateExisting := r.FormValue("update_existing") != "false"

	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
		return
	}

	// Read the file. The +1 catches an upload whose declared Size was wrong or
	// absent (chunked/streamed clients), so nothing is truncated silently.
	fileContent, err := io.ReadAll(io.LimitReader(file, MaxImportFileBytes+1))
	if err != nil {
		http.Error(w, `{"error":"failed to read file"}`, http.StatusInternalServerError)
		return
	}
	if len(fileContent) > MaxImportFileBytes {
		respondError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"file exceeds the %d MB import limit; split the file and import the parts",
			MaxImportFileMB))
		return
	}

	// Create import job
	jobID := uuid.New()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO mailing_import_jobs (id, organization_id, list_id, filename, field_mapping, status, started_at)
		VALUES ($1, $2, $3, $4, $5, 'processing', NOW())
	`, jobID, orgID, listUUID, header.Filename, fieldMapping); err != nil {
		// Log-and-continue, NOT fatal: mailing_import_jobs has no CREATE TABLE
		// anywhere in this repo and its existence in prod is still unconfirmed
		// (REQ-074). Failing the request here would break importing outright if
		// the table is absent. The import itself does not depend on the row —
		// only progress reporting does.
		log.Printf("[ERROR] import job %s: could not record job row (progress will not be visible): %v", jobID, err)
	}

	// Process CSV in background
	go s.processCSVImportEnhanced(jobID, listUUID, orgID, bytes.NewReader(fileContent), mapping, updateExisting)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"job_id": jobID.String(), "status": "processing",
	})
}

// importStandardFields is the field vocabulary the importer honors. Per ruling
// R2 (2026-08-01) it is DERIVED from GetStandardFields() (import_templates.go)
// rather than re-declared, so the /import/fields, /import/templates and
// /import/validate endpoints and the importer itself can never drift apart.
func importStandardFields() map[string]bool {
	fields := GetStandardFields()
	out := make(map[string]bool, len(fields))
	for _, f := range fields {
		out[f.Key] = true
	}
	return out
}

// importHeaderAliases maps a normalized CSV header to a standard field key.
//
// TODO(REQ-077): import_templates.go:312-345 carries a near-duplicate alias
// table. It is NOT a drop-in replacement: HandleValidateHeaders normalizes
// '-' to '_' but keys that table with "e-mail", so a header of "E-Mail" or
// "e_mail" resolves to email HERE and resolves to nothing THERE. Consolidating
// onto it therefore requires fixing that table first, which ruling R2 puts out
// of scope for this change. Do not delete one without the other.
var importHeaderAliases = map[string]string{
	"e_mail":        "email",
	"emailaddress":  "email",
	"email_address": "email",
	"firstname":     "first_name",
	"first":         "first_name",
	"given_name":    "first_name",
	"lastname":      "last_name",
	"last":          "last_name",
	"family_name":   "last_name",
	"surname":       "last_name",
	"mobile":        "phone",
	"phone_number":  "phone",
	"tel":           "phone",
	"zip":           "postal_code",
	"zipcode":       "postal_code",
	"zip_code":      "postal_code",
	"postcode":      "postal_code",
	"region":        "state",
	"province":      "state",
	"organisation":  "company",
	"organization":  "company",
	"company_name":  "company",
	"title":         "job_title",
	"position":      "job_title",
	"role":          "job_title",
}

// normalizeImportHeader lowercases a raw CSV header and folds spaces/hyphens to
// underscores, matching HandleValidateHeaders (import_templates.go:352-354).
func normalizeImportHeader(h string) string {
	key := strings.ToLower(strings.TrimSpace(h))
	key = strings.ReplaceAll(key, " ", "_")
	key = strings.ReplaceAll(key, "-", "_")
	return key
}

// resolveImportMapping returns the {field: column_index} map the importer will
// use for a file.
//
// An explicit, operator-reviewed mapping ALWAYS wins. An empty one is derived
// from the header row by name + alias — never positionally (REQ-068). A header
// that resolves to no known field keeps its own normalized name and lands in
// custom_fields, exactly as before. When two headers fold to the same key the
// later column wins, which is the pre-existing behavior of the inline block
// this replaces.
func resolveImportMapping(provided map[string]int, headers []string) map[string]int {
	if len(provided) > 0 {
		return provided
	}
	mapping := make(map[string]int, len(headers))
	for i, h := range headers {
		key := normalizeImportHeader(h)
		if target, ok := importHeaderAliases[key]; ok {
			key = target
		}
		mapping[key] = i
	}
	return mapping
}

// failImportJob terminalizes an import job as 'failed' and records the reason.
//
// Only columns the running schema is known to carry are written:
// mailing_import_jobs has no CREATE TABLE anywhere in this repo (REQ-074), so
// the human-readable reason goes to the log at error level rather than to an
// error column whose existence cannot be verified — writing a column that does
// not exist would make this UPDATE a no-op and strand the job in 'processing'
// forever, which is strictly worse than today. The Exec error is checked, not
// discarded, for the same reason.
func (s *AdvancedMailingService) failImportJob(jobID uuid.UUID, reason string) {
	log.Printf("[ERROR] import job %s FAILED: %s", jobID, reason)
	if _, err := s.db.Exec(`
		UPDATE mailing_import_jobs SET
			status = 'failed',
			total_rows = 0,
			processed_rows = 0,
			imported_count = 0,
			error_count = GREATEST(COALESCE(error_count, 0), 1),
			completed_at = NOW()
		WHERE id = $1
	`, jobID); err != nil {
		log.Printf("[ERROR] import job %s: could not record 'failed' status: %v", jobID, err)
	}
}

func (s *AdvancedMailingService) processCSVImport(jobID, listID, orgID uuid.UUID, file io.Reader, mapping map[string]int) {
	reader := csv.NewReader(file)
	
	var totalRows, imported, skipped, errorCount int
	
	// Standard field mappings
	standardFields := map[string]bool{
		"email": true, "first_name": true, "last_name": true, "phone": true,
		"city": true, "state": true, "country": true, "postal_code": true,
		"timezone": true, "company": true, "job_title": true, "industry": true,
		"language": true, "source": true, "tags": true, "birthdate": true,
		"subscribed_at": true,
	}
	
	// Skip header
	reader.Read()
	
	for {
		record, err := reader.Read()
		if err == io.EOF { break }
		if err != nil {
			errorCount++
			continue
		}
		totalRows++
		
		emailIdx, ok := mapping["email"]
		if !ok || emailIdx >= len(record) {
			errorCount++
			continue
		}
		
		email := strings.ToLower(strings.TrimSpace(record[emailIdx]))
		if email == "" || !strings.Contains(email, "@") {
			skipped++
			continue
		}

		// Layer-1 ingest guard (typo-traps, disposable, role-based).
		if decision := mailing.ClassifyEmailForIngest(email); !decision.Accept {
			skipped++
			continue
		}
		
		// Extract standard fields
		firstName, lastName := "", ""
		if idx, ok := mapping["first_name"]; ok && idx < len(record) {
			firstName = strings.TrimSpace(record[idx])
		}
		if idx, ok := mapping["last_name"]; ok && idx < len(record) {
			lastName = strings.TrimSpace(record[idx])
		}
		
		// Extract optional fields
		phone, city, state, country, postalCode := "", "", "", "", ""
		timezone, company, jobTitle, industry := "", "", "", ""
		language, source, tags := "", "", ""
		
		if idx, ok := mapping["phone"]; ok && idx < len(record) {
			phone = strings.TrimSpace(record[idx])
		}
		if idx, ok := mapping["city"]; ok && idx < len(record) {
			city = strings.TrimSpace(record[idx])
		}
		if idx, ok := mapping["state"]; ok && idx < len(record) {
			state = strings.TrimSpace(record[idx])
		}
		if idx, ok := mapping["country"]; ok && idx < len(record) {
			country = strings.TrimSpace(record[idx])
		}
		if idx, ok := mapping["postal_code"]; ok && idx < len(record) {
			postalCode = strings.TrimSpace(record[idx])
		}
		if idx, ok := mapping["timezone"]; ok && idx < len(record) {
			timezone = strings.TrimSpace(record[idx])
		}
		if idx, ok := mapping["company"]; ok && idx < len(record) {
			company = strings.TrimSpace(record[idx])
		}
		if idx, ok := mapping["job_title"]; ok && idx < len(record) {
			jobTitle = strings.TrimSpace(record[idx])
		}
		if idx, ok := mapping["industry"]; ok && idx < len(record) {
			industry = strings.TrimSpace(record[idx])
		}
		if idx, ok := mapping["language"]; ok && idx < len(record) {
			language = strings.TrimSpace(record[idx])
		}
		if idx, ok := mapping["source"]; ok && idx < len(record) {
			source = strings.TrimSpace(record[idx])
		}
		if idx, ok := mapping["tags"]; ok && idx < len(record) {
			tags = strings.TrimSpace(record[idx])
		}
		
		// Build custom fields JSON from any custom_ prefixed mappings
		customFields := make(map[string]interface{})
		for field, idx := range mapping {
			if strings.HasPrefix(field, "custom_") && idx < len(record) {
				fieldKey := strings.TrimPrefix(field, "custom_")
				value := strings.TrimSpace(record[idx])
				if value != "" {
					customFields[fieldKey] = value
				}
			} else if !standardFields[field] && idx < len(record) {
				// Any unmapped field goes to custom_fields
				value := strings.TrimSpace(record[idx])
				if value != "" {
					customFields[field] = value
				}
			}
		}
		
		// Add location fields to custom if present
		if city != "" { customFields["city"] = city }
		if state != "" { customFields["state"] = state }
		if country != "" { customFields["country"] = country }
		if postalCode != "" { customFields["postal_code"] = postalCode }
		if company != "" { customFields["company"] = company }
		if jobTitle != "" { customFields["job_title"] = jobTitle }
		if industry != "" { customFields["industry"] = industry }
		if language != "" { customFields["language"] = language }
		if phone != "" { customFields["phone"] = phone }
		
		customFieldsJSON, _ := json.Marshal(customFields)
		
		// Check suppression
		var suppressed bool
		s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM mailing_suppressions WHERE email = $1 AND active = true)", email).Scan(&suppressed)
		if suppressed {
			skipped++
			continue
		}
		
		// Insert subscriber with all fields
		subID := uuid.New()
		emailHash := fmt.Sprintf("%x", email) // Simple hash for demo
		
		if source == "" {
			source = "import"
		}
		
		_, err = s.db.Exec(`
			INSERT INTO mailing_subscribers (
				id, organization_id, list_id, email, email_hash, 
				first_name, last_name, status, source, timezone,
				custom_fields, engagement_score, created_at, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'confirmed', $8, $9, $10, 50.0, NOW(), NOW())
			ON CONFLICT (list_id, email) DO UPDATE SET 
				first_name = COALESCE(NULLIF($6, ''), mailing_subscribers.first_name),
				last_name = COALESCE(NULLIF($7, ''), mailing_subscribers.last_name),
				timezone = COALESCE(NULLIF($9, ''), mailing_subscribers.timezone),
				custom_fields = mailing_subscribers.custom_fields || $10::jsonb,
				updated_at = NOW()
		`, subID, orgID, listID, email, emailHash, firstName, lastName, source, timezone, string(customFieldsJSON))
		
		if err != nil {
			errorCount++
		} else {
			imported++
		}
		
		// Handle tags separately if present
		if tags != "" {
			tagList := strings.Split(tags, ",")
			for _, tag := range tagList {
				tag = strings.TrimSpace(tag)
				if tag != "" {
					s.db.Exec(`
						INSERT INTO mailing_subscriber_tags (subscriber_id, tag)
						VALUES ((SELECT id FROM mailing_subscribers WHERE list_id = $1 AND email = $2), $3)
						ON CONFLICT DO NOTHING
					`, listID, email, tag)
				}
			}
		}
		
		// Update progress every 100 rows
		if totalRows % 100 == 0 {
			s.db.Exec(`UPDATE mailing_import_jobs SET processed_rows = $2 WHERE id = $1`, jobID, totalRows)
		}
	}
	
	// Update list count
	s.db.Exec(`UPDATE mailing_lists SET subscriber_count = (SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = $1) WHERE id = $1`, listID)
	
	// Complete job
	s.db.Exec(`
		UPDATE mailing_import_jobs SET 
			status = 'completed', total_rows = $2, processed_rows = $2, 
			imported_count = $3, skipped_count = $4, error_count = $5, completed_at = NOW()
		WHERE id = $1
	`, jobID, totalRows, imported, skipped, errorCount)
}

// Email validation regex
var emailValidationRegex = regexp.MustCompile(`^[a-zA-Z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)

// validateEmailFormat checks if an email has valid format
func validateEmailFormat(email string) bool {
	if len(email) < 5 || len(email) > 254 {
		return false
	}
	return emailValidationRegex.MatchString(email)
}

// processCSVImportEnhanced is the enhanced version with better tracking
func (s *AdvancedMailingService) processCSVImportEnhanced(jobID, listID, orgID uuid.UUID, file io.Reader, mapping map[string]int, updateExisting bool) {
	reader := csv.NewReader(file)
	
	var totalRows, newCount, updatedCount, skippedCount, errorCount, duplicateCount int

	// Standard field vocabulary — derived from GetStandardFields() per ruling R2.
	standardFields := importStandardFields()

	// Read header
	headers, err := reader.Read()
	if err != nil {
		s.failImportJob(jobID, fmt.Sprintf("could not read the CSV header row: %v", err))
		return
	}

	// Explicit mapping wins; otherwise derive it from the header row by
	// name/alias. Never positionally — see resolveImportMapping (REQ-068).
	mapping = resolveImportMapping(mapping, headers)

	// REQ-068: a file with no email-resolvable column fails LOUDLY. Previously
	// this fell straight through: every row missed the mapping["email"] lookup
	// at the top of the loop, incremented errorCount, and the job still
	// terminated status='completed' with 0 imported — a total no-op that read
	// as a success.
	if _, ok := mapping["email"]; !ok {
		s.failImportJob(jobID, fmt.Sprintf(
			"no email column found in header row [%s] — map a column to 'email' and re-upload",
			strings.Join(headers, ", ")))
		return
	}

	// Count total rows first
	allRecords := make([][]string, 0)
	for {
		record, err := reader.Read()
		if err == io.EOF { break }
		if err != nil { continue }
		allRecords = append(allRecords, record)
	}
	
	totalToProcess := len(allRecords)
	
	// Update job with total
	s.db.Exec(`UPDATE mailing_import_jobs SET total_rows = $2 WHERE id = $1`, jobID, totalToProcess)
	
	// Process records
	seenEmails := make(map[string]bool)
	
	for _, record := range allRecords {
		totalRows++
		
		emailIdx, ok := mapping["email"]
		if !ok || emailIdx >= len(record) {
			errorCount++
			continue
		}
		
		email := strings.ToLower(strings.TrimSpace(record[emailIdx]))
		
		// Validate email format
		if email == "" {
			skippedCount++
			continue
		}
		
		if !validateEmailFormat(email) {
			skippedCount++
			continue
		}

		// Layer-1 ingest guard (typo-traps, disposable, role-based).
		// Runs before dedupe so the first occurrence of a bad address
		// is rejected and doesn't poison seenEmails with useless work.
		if decision := mailing.ClassifyEmailForIngest(email); !decision.Accept {
			skippedCount++
			continue
		}
		
		// Check for duplicates within file
		if seenEmails[email] {
			duplicateCount++
			skippedCount++
			continue
		}
		seenEmails[email] = true
		
		// Extract fields
		firstName, lastName := "", ""
		if idx, ok := mapping["first_name"]; ok && idx < len(record) {
			firstName = strings.TrimSpace(record[idx])
		}
		if idx, ok := mapping["last_name"]; ok && idx < len(record) {
			lastName = strings.TrimSpace(record[idx])
		}
		
		// Extract optional fields
		phone, city, state, country, postalCode := "", "", "", "", ""
		timezone, company, jobTitle, industry := "", "", "", ""
		language, source, tags := "", "", ""
		
		if idx, ok := mapping["phone"]; ok && idx < len(record) { phone = strings.TrimSpace(record[idx]) }
		if idx, ok := mapping["city"]; ok && idx < len(record) { city = strings.TrimSpace(record[idx]) }
		if idx, ok := mapping["state"]; ok && idx < len(record) { state = strings.TrimSpace(record[idx]) }
		if idx, ok := mapping["country"]; ok && idx < len(record) { country = strings.TrimSpace(record[idx]) }
		if idx, ok := mapping["postal_code"]; ok && idx < len(record) { postalCode = strings.TrimSpace(record[idx]) }
		if idx, ok := mapping["timezone"]; ok && idx < len(record) { timezone = strings.TrimSpace(record[idx]) }
		if idx, ok := mapping["company"]; ok && idx < len(record) { company = strings.TrimSpace(record[idx]) }
		if idx, ok := mapping["job_title"]; ok && idx < len(record) { jobTitle = strings.TrimSpace(record[idx]) }
		if idx, ok := mapping["industry"]; ok && idx < len(record) { industry = strings.TrimSpace(record[idx]) }
		if idx, ok := mapping["language"]; ok && idx < len(record) { language = strings.TrimSpace(record[idx]) }
		if idx, ok := mapping["source"]; ok && idx < len(record) { source = strings.TrimSpace(record[idx]) }
		if idx, ok := mapping["tags"]; ok && idx < len(record) { tags = strings.TrimSpace(record[idx]) }
		
		// Build custom fields
		customFields := make(map[string]interface{})
		for field, idx := range mapping {
			if strings.HasPrefix(field, "custom_") && idx < len(record) {
				fieldKey := strings.TrimPrefix(field, "custom_")
				value := strings.TrimSpace(record[idx])
				if value != "" { customFields[fieldKey] = value }
			} else if !standardFields[field] && idx < len(record) {
				value := strings.TrimSpace(record[idx])
				if value != "" { customFields[field] = value }
			}
		}
		
		// Add to custom fields
		if city != "" { customFields["city"] = city }
		if state != "" { customFields["state"] = state }
		if country != "" { customFields["country"] = country }
		if postalCode != "" { customFields["postal_code"] = postalCode }
		if company != "" { customFields["company"] = company }
		if jobTitle != "" { customFields["job_title"] = jobTitle }
		if industry != "" { customFields["industry"] = industry }
		if language != "" { customFields["language"] = language }
		if phone != "" { customFields["phone"] = phone }
		
		customFieldsJSON, _ := json.Marshal(customFields)
		
		// Check suppression
		var suppressed bool
		s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM mailing_suppressions WHERE email = $1 AND active = true)", email).Scan(&suppressed)
		if suppressed {
			skippedCount++
			continue
		}
		
		// Check if email already exists
		var existingID uuid.UUID
		err := s.db.QueryRow("SELECT id FROM mailing_subscribers WHERE list_id = $1 AND email = $2", listID, email).Scan(&existingID)
		emailExists := err == nil
		
		if emailExists && !updateExisting {
			// Skip existing emails if update is disabled
			skippedCount++
			duplicateCount++
			continue
		}
		
		subID := uuid.New()
		h := md5.New()
		h.Write([]byte(email))
		emailHash := hex.EncodeToString(h.Sum(nil))
		
		if source == "" { source = "import" }
		
		if emailExists {
			// Update existing record
			_, err = s.db.Exec(`
				UPDATE mailing_subscribers SET
					first_name = COALESCE(NULLIF($1, ''), first_name),
					last_name = COALESCE(NULLIF($2, ''), last_name),
					timezone = COALESCE(NULLIF($3, ''), timezone),
					custom_fields = custom_fields || $4::jsonb,
					updated_at = NOW()
				WHERE id = $5
			`, firstName, lastName, timezone, string(customFieldsJSON), existingID)
			
			if err != nil {
				errorCount++
			} else {
				updatedCount++
			}
		} else {
			// Insert new record
			_, err = s.db.Exec(`
				INSERT INTO mailing_subscribers (
					id, organization_id, list_id, email, email_hash, 
					first_name, last_name, status, source, timezone,
					custom_fields, engagement_score, created_at, updated_at
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, 'confirmed', $8, $9, $10, 50.0, NOW(), NOW())
			`, subID, orgID, listID, email, emailHash, firstName, lastName, source, timezone, string(customFieldsJSON))
			
			if err != nil {
				errorCount++
			} else {
				newCount++
			}
		}
		
		// Handle tags
		if tags != "" {
			targetID := existingID
			if targetID == uuid.Nil { targetID = subID }
			
			tagList := strings.Split(tags, ",")
			for _, tag := range tagList {
				tag = strings.TrimSpace(tag)
				if tag != "" {
					s.db.Exec(`
						INSERT INTO mailing_subscriber_tags (subscriber_id, tag)
						VALUES ($1, $2) ON CONFLICT DO NOTHING
					`, targetID, tag)
				}
			}
		}
		
		// Update progress every 50 rows
		if totalRows % 50 == 0 {
			s.db.Exec(`UPDATE mailing_import_jobs SET processed_rows = $2 WHERE id = $1`, jobID, totalRows)
		}
	}
	
	// Update list count
	s.db.Exec(`UPDATE mailing_lists SET subscriber_count = (SELECT COUNT(*) FROM mailing_subscribers WHERE list_id = $1) WHERE id = $1`, listID)
	
	// Complete job with detailed stats
	s.db.Exec(`
		UPDATE mailing_import_jobs SET 
			status = 'completed', 
			total_rows = $2, 
			processed_rows = $2, 
			imported_count = $3, 
			skipped_count = $4, 
			error_count = $5,
			completed_at = NOW()
		WHERE id = $1
	`, jobID, totalRows, newCount+updatedCount, skippedCount, errorCount)
	
	log.Printf("Import complete: %d total, %d new, %d updated, %d skipped, %d errors, %d duplicates",
		totalRows, newCount, updatedCount, skippedCount, errorCount, duplicateCount)
}

func (s *AdvancedMailingService) HandleGetImportJobs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, _ := s.db.QueryContext(ctx, `
		SELECT id, list_id, filename, total_rows, imported_count, skipped_count, error_count, status, created_at, completed_at
		FROM mailing_import_jobs ORDER BY created_at DESC LIMIT 20
	`)
	defer rows.Close()
	
	var jobs []map[string]interface{}
	for rows.Next() {
		var id, listID uuid.UUID
		var filename, status string
		var total, imported, skipped, errors int
		var createdAt time.Time
		var completedAt *time.Time
		rows.Scan(&id, &listID, &filename, &total, &imported, &skipped, &errors, &status, &createdAt, &completedAt)
		jobs = append(jobs, map[string]interface{}{
			"id": id.String(), "list_id": listID.String(), "filename": filename,
			"total_rows": total, "imported_count": imported, "skipped_count": skipped,
			"error_count": errors, "status": status, "created_at": createdAt, "completed_at": completedAt,
		})
	}
	if jobs == nil { jobs = []map[string]interface{}{} }
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"jobs": jobs})
}

func (s *AdvancedMailingService) HandleGetImportJob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	jobID := chi.URLParam(r, "jobId")
	
	var id, listID uuid.UUID
	var filename, status string
	var total, processed, imported, skipped, errors int
	var createdAt time.Time
	var completedAt *time.Time
	
	err := s.db.QueryRowContext(ctx, `
		SELECT id, list_id, filename, total_rows, processed_rows, imported_count, skipped_count, error_count, status, created_at, completed_at
		FROM mailing_import_jobs WHERE id = $1
	`, jobID).Scan(&id, &listID, &filename, &total, &processed, &imported, &skipped, &errors, &status, &createdAt, &completedAt)
	
	if err != nil {
		http.Error(w, `{"error":"job not found"}`, http.StatusNotFound)
		return
	}
	
	progress := 0
	if total > 0 { progress = processed * 100 / total }
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id.String(), "list_id": listID.String(), "filename": filename,
		"total_rows": total, "processed_rows": processed, "progress_percent": progress,
		"imported_count": imported, "skipped_count": skipped, "error_count": errors,
		"status": status, "created_at": createdAt, "completed_at": completedAt,
	})
}
