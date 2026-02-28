package api

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// ImportTemplateService handles import template generation and field mapping
type ImportTemplateService struct {
	db *sql.DB
}

// NewImportTemplateService creates a new import template service
func NewImportTemplateService(db *sql.DB) *ImportTemplateService {
	return &ImportTemplateService{db: db}
}

// RegisterRoutes registers import template routes
func (s *ImportTemplateService) RegisterRoutes(r chi.Router) {
	r.Route("/import", func(r chi.Router) {
		// Template downloads
		r.Get("/templates", s.HandleListTemplates)
		r.Get("/templates/basic", s.HandleDownloadBasicTemplate)
		r.Get("/templates/full", s.HandleDownloadFullTemplate)
		r.Get("/templates/custom", s.HandleDownloadCustomTemplate)
		
		// Field mapping helpers
		r.Get("/fields", s.HandleGetAvailableFields)
		r.Post("/validate", s.HandleValidateHeaders)
		r.Post("/preview", s.HandlePreviewImport)
	})
}

// ==========================================
// TEMPLATE DEFINITIONS
// ==========================================

// TemplateField represents a field in the import template
type TemplateField struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Required    bool   `json:"required"`
	Description string `json:"description"`
	Example     string `json:"example"`
	DataType    string `json:"data_type"`
	Category    string `json:"category"`
}

// GetStandardFields returns the standard import fields
func GetStandardFields() []TemplateField {
	return []TemplateField{
		// Required
		{Key: "email", Label: "Email Address", Required: true, Description: "Subscriber's email address (required)", Example: "john@example.com", DataType: "email", Category: "required"},
		
		// Profile (recommended)
		{Key: "first_name", Label: "First Name", Required: false, Description: "Subscriber's first name", Example: "John", DataType: "string", Category: "profile"},
		{Key: "last_name", Label: "Last Name", Required: false, Description: "Subscriber's last name", Example: "Smith", DataType: "string", Category: "profile"},
		{Key: "phone", Label: "Phone Number", Required: false, Description: "Phone number with country code", Example: "+1-555-123-4567", DataType: "phone", Category: "profile"},
		
		// Location
		{Key: "city", Label: "City", Required: false, Description: "City of residence", Example: "New York", DataType: "string", Category: "location"},
		{Key: "state", Label: "State/Province", Required: false, Description: "State or province", Example: "NY", DataType: "string", Category: "location"},
		{Key: "country", Label: "Country", Required: false, Description: "Country (ISO code preferred)", Example: "US", DataType: "string", Category: "location"},
		{Key: "postal_code", Label: "Postal/ZIP Code", Required: false, Description: "Postal or ZIP code", Example: "10001", DataType: "string", Category: "location"},
		{Key: "timezone", Label: "Timezone", Required: false, Description: "IANA timezone identifier", Example: "America/New_York", DataType: "string", Category: "location"},
		
		// Business
		{Key: "company", Label: "Company Name", Required: false, Description: "Company or organization name", Example: "Acme Corp", DataType: "string", Category: "business"},
		{Key: "job_title", Label: "Job Title", Required: false, Description: "Job title or position", Example: "Marketing Manager", DataType: "string", Category: "business"},
		{Key: "industry", Label: "Industry", Required: false, Description: "Industry or sector", Example: "Technology", DataType: "string", Category: "business"},
		
		// Preferences
		{Key: "language", Label: "Language", Required: false, Description: "Preferred language (ISO 639-1)", Example: "en", DataType: "string", Category: "preferences"},
		{Key: "source", Label: "Source", Required: false, Description: "How subscriber was acquired", Example: "website_signup", DataType: "string", Category: "preferences"},
		{Key: "tags", Label: "Tags", Required: false, Description: "Comma-separated tags", Example: "vip,early-adopter", DataType: "tags", Category: "preferences"},
		
		// Dates
		{Key: "birthdate", Label: "Birth Date", Required: false, Description: "Date of birth (YYYY-MM-DD)", Example: "1990-05-15", DataType: "date", Category: "dates"},
		{Key: "subscribed_at", Label: "Subscribe Date", Required: false, Description: "Original subscription date", Example: "2024-01-15", DataType: "date", Category: "dates"},
	}
}

// ==========================================
// HANDLERS
// ==========================================

// HandleListTemplates returns available template types
func (s *ImportTemplateService) HandleListTemplates(w http.ResponseWriter, r *http.Request) {
	templates := []map[string]interface{}{
		{
			"id":          "basic",
			"name":        "Basic Template",
			"description": "Essential fields only: email, first name, last name",
			"fields":      3,
			"download_url": "/api/mailing/import/templates/basic",
		},
		{
			"id":          "full",
			"name":        "Full Template",
			"description": "All standard fields including location, business, and preferences",
			"fields":      17,
			"download_url": "/api/mailing/import/templates/full",
		},
		{
			"id":          "custom",
			"name":        "Custom Template",
			"description": "Includes your organization's custom fields",
			"fields":      "dynamic",
			"download_url": "/api/mailing/import/templates/custom",
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"templates": templates,
		"tips": []string{
			"Use CSV format with UTF-8 encoding",
			"First row must contain column headers",
			"Email is the only required field",
			"Empty cells are allowed for optional fields",
			"Dates should be in YYYY-MM-DD format",
			"Tags can be comma-separated in a single cell",
		},
	})
}

// HandleDownloadBasicTemplate generates and downloads a basic CSV template
func (s *ImportTemplateService) HandleDownloadBasicTemplate(w http.ResponseWriter, r *http.Request) {
	// Basic fields
	headers := []string{"email", "first_name", "last_name"}
	
	// Sample data rows
	samples := [][]string{
		{"john@example.com", "John", "Smith"},
		{"jane@example.com", "Jane", "Doe"},
		{"bob@example.com", "Bob", "Wilson"},
	}

	s.writeCSVTemplate(w, "subscriber_import_basic.csv", headers, samples)
}

// HandleDownloadFullTemplate generates and downloads a full CSV template
func (s *ImportTemplateService) HandleDownloadFullTemplate(w http.ResponseWriter, r *http.Request) {
	fields := GetStandardFields()
	
	headers := make([]string, len(fields))
	for i, f := range fields {
		headers[i] = f.Key
	}

	// Sample data rows
	samples := [][]string{
		{
			"john@example.com", "John", "Smith", "+1-555-123-4567",
			"New York", "NY", "US", "10001", "America/New_York",
			"Acme Corp", "Marketing Manager", "Technology",
			"en", "website_signup", "vip,newsletter",
			"1990-05-15", "2024-01-15",
		},
		{
			"jane@example.com", "Jane", "Doe", "+1-555-987-6543",
			"Los Angeles", "CA", "US", "90001", "America/Los_Angeles",
			"Tech Inc", "CEO", "SaaS",
			"en", "referral", "vip,enterprise",
			"1985-08-20", "2024-02-01",
		},
		{
			"bob@example.com", "Bob", "Wilson", "",
			"Chicago", "IL", "US", "60601", "America/Chicago",
			"", "", "",
			"en", "facebook_ad", "new-subscriber",
			"", "2024-03-10",
		},
	}

	s.writeCSVTemplate(w, "subscriber_import_full.csv", headers, samples)
}

// HandleDownloadCustomTemplate generates a template with org-specific custom fields
func (s *ImportTemplateService) HandleDownloadCustomTemplate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgIDFromContext(ctx)

	// Start with standard fields
	standardFields := GetStandardFields()
	headers := make([]string, 0, len(standardFields)+10)
	for _, f := range standardFields {
		headers = append(headers, f.Key)
	}

	// Add organization's custom fields
	rows, err := s.db.QueryContext(ctx, `
		SELECT field_key, field_label 
		FROM mailing_contact_fields 
		WHERE organization_id = $1 AND is_system = false
		ORDER BY display_order, field_label
	`, orgID)
	
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var key, label string
			if rows.Scan(&key, &label) == nil {
				headers = append(headers, "custom_"+key)
			}
		}
	}

	// Generate sample row with all fields empty except basics
	sampleRow := make([]string, len(headers))
	sampleRow[0] = "subscriber@example.com"
	sampleRow[1] = "FirstName"
	sampleRow[2] = "LastName"

	samples := [][]string{sampleRow}

	s.writeCSVTemplate(w, "subscriber_import_custom.csv", headers, samples)
}

// HandleGetAvailableFields returns all available import fields with metadata
func (s *ImportTemplateService) HandleGetAvailableFields(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgID := getOrgIDFromContext(ctx)

	// Standard fields
	standardFields := GetStandardFields()

	// Custom fields from database
	customFields := make([]TemplateField, 0)
	rows, err := s.db.QueryContext(ctx, `
		SELECT field_key, field_label, field_type, description, category
		FROM mailing_contact_fields 
		WHERE organization_id = $1 AND is_system = false
		ORDER BY display_order, field_label
	`, orgID)

	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var f TemplateField
			var desc, cat sql.NullString
			if rows.Scan(&f.Key, &f.Label, &f.DataType, &desc, &cat) == nil {
				f.Key = "custom_" + f.Key
				f.Category = "custom"
				f.Required = false
				if desc.Valid {
					f.Description = desc.String
				}
				customFields = append(customFields, f)
			}
		}
	}

	// Group by category
	categorized := map[string][]TemplateField{
		"required":    {},
		"profile":     {},
		"location":    {},
		"business":    {},
		"preferences": {},
		"dates":       {},
		"custom":      customFields,
	}

	for _, f := range standardFields {
		categorized[f.Category] = append(categorized[f.Category], f)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"fields":     standardFields,
		"custom":     customFields,
		"categories": categorized,
		"field_mapping_help": map[string]string{
			"email":        "Required. Must be a valid email address.",
			"first_name":   "Subscriber's first/given name.",
			"last_name":    "Subscriber's last/family name.",
			"phone":        "Include country code (e.g., +1-555-123-4567).",
			"tags":         "Comma-separated values (e.g., 'vip,newsletter').",
			"birthdate":    "Format: YYYY-MM-DD (e.g., 1990-05-15).",
			"subscribed_at": "Original subscribe date in YYYY-MM-DD format.",
			"timezone":     "IANA timezone (e.g., America/New_York).",
		},
	})
}

// HandleValidateHeaders validates uploaded CSV headers and suggests mapping
func (s *ImportTemplateService) HandleValidateHeaders(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Headers []string `json:"headers"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	standardFields := GetStandardFields()
	fieldMap := make(map[string]TemplateField)
	for _, f := range standardFields {
		fieldMap[strings.ToLower(f.Key)] = f
		fieldMap[strings.ToLower(f.Label)] = f
	}

	// Add common aliases
	aliases := map[string]string{
		"e-mail":       "email",
		"emailaddress": "email",
		"email_address": "email",
		"firstname":    "first_name",
		"first":        "first_name",
		"given_name":   "first_name",
		"lastname":     "last_name",
		"last":         "last_name",
		"family_name":  "last_name",
		"surname":      "last_name",
		"mobile":       "phone",
		"phone_number": "phone",
		"tel":          "phone",
		"zip":          "postal_code",
		"zipcode":      "postal_code",
		"zip_code":     "postal_code",
		"postcode":     "postal_code",
		"region":       "state",
		"province":     "state",
		"organisation": "company",
		"organization": "company",
		"company_name": "company",
		"title":        "job_title",
		"position":     "job_title",
		"role":         "job_title",
		"dob":          "birthdate",
		"date_of_birth": "birthdate",
		"birthday":     "birthdate",
		"lang":         "language",
		"locale":       "language",
		"tz":           "timezone",
		"time_zone":    "timezone",
	}

	// Build suggested mapping
	mapping := make([]map[string]interface{}, len(req.Headers))
	hasEmail := false

	for i, header := range req.Headers {
		normalized := strings.ToLower(strings.TrimSpace(header))
		normalized = strings.ReplaceAll(normalized, " ", "_")
		normalized = strings.ReplaceAll(normalized, "-", "_")

		result := map[string]interface{}{
			"column_index":   i,
			"original_header": header,
			"suggested_field": nil,
			"confidence":     "none",
			"is_custom":      false,
		}

		// Check direct match
		if field, ok := fieldMap[normalized]; ok {
			result["suggested_field"] = field.Key
			result["confidence"] = "high"
			if field.Key == "email" {
				hasEmail = true
			}
		} else if aliasTarget, ok := aliases[normalized]; ok {
			// Check alias match
			result["suggested_field"] = aliasTarget
			result["confidence"] = "medium"
			if aliasTarget == "email" {
				hasEmail = true
			}
		} else if strings.HasPrefix(normalized, "custom_") {
			// Custom field
			result["suggested_field"] = normalized
			result["confidence"] = "high"
			result["is_custom"] = true
		}

		mapping[i] = result
	}

	// Validation result
	validation := map[string]interface{}{
		"valid":         hasEmail,
		"has_email":     hasEmail,
		"total_columns": len(req.Headers),
		"mapped_columns": 0,
		"warnings":      []string{},
		"errors":        []string{},
	}

	warnings := []string{}
	if !hasEmail {
		validation["errors"] = []string{"Email column is required but not found. Please ensure your file has an 'email' column."}
	}

	mappedCount := 0
	for _, m := range mapping {
		if m["suggested_field"] != nil {
			mappedCount++
		}
	}
	validation["mapped_columns"] = mappedCount

	if mappedCount < len(req.Headers)/2 {
		warnings = append(warnings, fmt.Sprintf("Only %d of %d columns were automatically mapped. Please review the mapping.", mappedCount, len(req.Headers)))
	}

	validation["warnings"] = warnings

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"mapping":    mapping,
		"validation": validation,
	})
}

// HandlePreviewImport previews the first few rows of an import
func (s *ImportTemplateService) HandlePreviewImport(w http.ResponseWriter, r *http.Request) {
	r.ParseMultipartForm(10 << 20) // 10MB max for preview

	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"file required"}`, http.StatusBadRequest)
		return
	}
	defer file.Close()

	reader := csv.NewReader(file)
	
	// Read headers
	headers, err := reader.Read()
	if err != nil {
		http.Error(w, `{"error":"failed to read CSV headers"}`, http.StatusBadRequest)
		return
	}

	// Read up to 5 preview rows
	previewRows := make([][]string, 0, 5)
	for i := 0; i < 5; i++ {
		row, err := reader.Read()
		if err != nil {
			break
		}
		previewRows = append(previewRows, row)
	}

	// Count total rows (estimate)
	totalEstimate := len(previewRows)
	for {
		_, err := reader.Read()
		if err != nil {
			break
		}
		totalEstimate++
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"headers":        headers,
		"preview_rows":   previewRows,
		"total_estimate": totalEstimate,
		"column_count":   len(headers),
	})
}

// ==========================================
// HELPERS
// ==========================================

func (s *ImportTemplateService) writeCSVTemplate(w http.ResponseWriter, filename string, headers []string, sampleRows [][]string) {
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))

	writer := csv.NewWriter(w)
	defer writer.Flush()

	// Write headers
	writer.Write(headers)

	// Write sample rows
	for _, row := range sampleRows {
		writer.Write(row)
	}
}
