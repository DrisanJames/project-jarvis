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
)

func (s *AdvancedMailingService) HandleImportSubscribers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	listID := chi.URLParam(r, "listId")
	listUUID, _ := uuid.Parse(listID)
	
	// Parse multipart form
	r.ParseMultipartForm(32 << 20) // 32MB max
	
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"file required"}`, http.StatusBadRequest)
		return
	}
	defer file.Close()
	
	// Get field mapping from form
	fieldMapping := r.FormValue("field_mapping")
	if fieldMapping == "" {
		fieldMapping = `{"email":0,"first_name":1,"last_name":2}`
	}
	
	// Get update_existing flag (defaults to true)
	updateExisting := r.FormValue("update_existing") != "false"
	
	var mapping map[string]int
	json.Unmarshal([]byte(fieldMapping), &mapping)
	
	// Create import job
	jobID := uuid.New()
	orgID, err := GetOrgIDFromRequest(r)
	if err != nil {
		http.Error(w, `{"error":"organization context required"}`, http.StatusUnauthorized)
		return
	}
	
	s.db.ExecContext(ctx, `
		INSERT INTO mailing_import_jobs (id, organization_id, list_id, filename, field_mapping, status, started_at)
		VALUES ($1, $2, $3, $4, $5, 'processing', NOW())
	`, jobID, orgID, listUUID, header.Filename, fieldMapping)
	
	// Read file into memory for processing (limit to 32MB matching multipart form limit)
	fileContent, err := io.ReadAll(io.LimitReader(file, 32*1024*1024))
	if err != nil {
		http.Error(w, `{"error":"failed to read file"}`, http.StatusInternalServerError)
		return
	}
	
	// Process CSV in background
	go s.processCSVImportEnhanced(jobID, listUUID, orgID, bytes.NewReader(fileContent), mapping, updateExisting)
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"job_id": jobID.String(), "status": "processing",
	})
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
	
	// Standard field mappings
	standardFields := map[string]bool{
		"email": true, "first_name": true, "last_name": true, "phone": true,
		"city": true, "state": true, "country": true, "postal_code": true,
		"timezone": true, "company": true, "job_title": true, "industry": true,
		"language": true, "source": true, "tags": true, "birthdate": true,
		"subscribed_at": true,
	}
	
	// Read header
	headers, err := reader.Read()
	if err != nil {
		s.db.Exec(`UPDATE mailing_import_jobs SET status = 'failed', error_count = 1 WHERE id = $1`, jobID)
		return
	}
	
	// Build mapping from headers if not provided
	if len(mapping) == 0 {
		mapping = make(map[string]int)
		for i, h := range headers {
			key := strings.ToLower(strings.TrimSpace(h))
			key = strings.ReplaceAll(key, " ", "_")
			key = strings.ReplaceAll(key, "-", "_")
			
			// Common aliases
			switch key {
			case "e_mail", "emailaddress", "email_address":
				key = "email"
			case "firstname", "first", "given_name":
				key = "first_name"
			case "lastname", "last", "family_name", "surname":
				key = "last_name"
			case "mobile", "phone_number", "tel":
				key = "phone"
			case "zip", "zipcode", "zip_code", "postcode":
				key = "postal_code"
			case "region", "province":
				key = "state"
			case "organisation", "organization", "company_name":
				key = "company"
			case "title", "position", "role":
				key = "job_title"
			}
			mapping[key] = i
		}
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
