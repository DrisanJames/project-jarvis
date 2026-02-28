package api

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ignite/sparkpost-monitor/internal/mailing"
)

// =============================================================================
// CUSTOM FIELDS HANDLERS
// =============================================================================
// HTTP handlers for the custom fields API. Enables:
// - CRUD operations for custom field definitions
// - Detection of non-standard columns from CSV headers
// - Field type suggestions based on sample data
// - Mapping CSV columns to existing custom fields

// CustomFieldsAPI provides HTTP handlers for custom fields
type CustomFieldsAPI struct {
	db      *sql.DB
	service *mailing.CustomFieldService
}

// NewCustomFieldsAPI creates a new custom fields API handler
func NewCustomFieldsAPI(db *sql.DB) *CustomFieldsAPI {
	return &CustomFieldsAPI{
		db:      db,
		service: mailing.NewCustomFieldService(db),
	}
}

// RegisterRoutes registers custom field routes
func (api *CustomFieldsAPI) RegisterRoutes(r chi.Router) {
	r.Route("/custom-fields", func(r chi.Router) {
		// CRUD operations
		r.Get("/", api.HandleListCustomFields)
		r.Post("/", api.HandleCreateCustomField)
		r.Get("/{fieldId}", api.HandleGetCustomField)
		r.Put("/{fieldId}", api.HandleUpdateCustomField)
		r.Delete("/{fieldId}", api.HandleDeleteCustomField)

		// Detection and analysis
		r.Post("/detect", api.HandleDetectNonStandardColumns)
		r.Post("/bulk", api.HandleBulkCreateCustomFields)
		
		// Standard fields reference
		r.Get("/standard-fields", api.HandleGetStandardFields)
	})
}

// =============================================================================
// CRUD OPERATIONS
// =============================================================================

// HandleListCustomFields returns all custom fields for the organization
// GET /api/mailing/custom-fields
func (api *CustomFieldsAPI) HandleListCustomFields(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgIDStr := getOrgIDFromRequest(r)
	orgID, err := uuid.Parse(orgIDStr)
	if err != nil {
		writeJSONError(w, "invalid organization ID", http.StatusBadRequest)
		return
	}

	fields, err := api.service.ListCustomFields(ctx, orgID)
	if err != nil {
		writeJSONError(w, "failed to list custom fields", http.StatusInternalServerError)
		return
	}

	if fields == nil {
		fields = []mailing.CustomFieldDefinition{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"custom_fields": fields,
		"total":         len(fields),
	})
}

// HandleCreateCustomField creates a new custom field definition
// POST /api/mailing/custom-fields
func (api *CustomFieldsAPI) HandleCreateCustomField(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgIDStr := getOrgIDFromRequest(r)
	orgID, err := uuid.Parse(orgIDStr)
	if err != nil {
		writeJSONError(w, "invalid organization ID", http.StatusBadRequest)
		return
	}

	var req mailing.CreateCustomFieldRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Validate required fields
	if req.Name == "" {
		writeJSONError(w, "name is required", http.StatusBadRequest)
		return
	}
	if req.DisplayName == "" {
		req.DisplayName = req.Name // Default to name if no display name provided
	}
	if req.FieldType == "" {
		req.FieldType = mailing.FieldTypeString // Default to string type
	}

	field, err := api.service.CreateCustomField(ctx, orgID, req)
	if err != nil {
		// Check for duplicate/conflict errors
		if isConflictError(err) {
			writeJSONError(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, http.StatusCreated, field)
}

// HandleGetCustomField retrieves a specific custom field
// GET /api/mailing/custom-fields/{fieldId}
func (api *CustomFieldsAPI) HandleGetCustomField(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgIDStr := getOrgIDFromRequest(r)
	orgID, err := uuid.Parse(orgIDStr)
	if err != nil {
		writeJSONError(w, "invalid organization ID", http.StatusBadRequest)
		return
	}
	fieldIDStr := chi.URLParam(r, "fieldId")

	fieldID, err := uuid.Parse(fieldIDStr)
	if err != nil {
		writeJSONError(w, "invalid field ID", http.StatusBadRequest)
		return
	}

	field, err := api.service.GetCustomField(ctx, orgID, fieldID)
	if err != nil {
		writeJSONError(w, "failed to get custom field", http.StatusInternalServerError)
		return
	}

	if field == nil {
		writeJSONError(w, "custom field not found", http.StatusNotFound)
		return
	}

	writeJSON(w, http.StatusOK, field)
}

// HandleUpdateCustomField updates a custom field definition
// PUT /api/mailing/custom-fields/{fieldId}
func (api *CustomFieldsAPI) HandleUpdateCustomField(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgIDStr := getOrgIDFromRequest(r)
	orgID, err := uuid.Parse(orgIDStr)
	if err != nil {
		writeJSONError(w, "invalid organization ID", http.StatusBadRequest)
		return
	}
	fieldIDStr := chi.URLParam(r, "fieldId")

	fieldID, err := uuid.Parse(fieldIDStr)
	if err != nil {
		writeJSONError(w, "invalid field ID", http.StatusBadRequest)
		return
	}

	var req mailing.CreateCustomFieldRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	field, err := api.service.UpdateCustomField(ctx, orgID, fieldID, req)
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, http.StatusOK, field)
}

// HandleDeleteCustomField soft-deletes a custom field
// DELETE /api/mailing/custom-fields/{fieldId}
func (api *CustomFieldsAPI) HandleDeleteCustomField(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgIDStr := getOrgIDFromRequest(r)
	orgID, err := uuid.Parse(orgIDStr)
	if err != nil {
		writeJSONError(w, "invalid organization ID", http.StatusBadRequest)
		return
	}
	fieldIDStr := chi.URLParam(r, "fieldId")

	fieldID, err := uuid.Parse(fieldIDStr)
	if err != nil {
		writeJSONError(w, "invalid field ID", http.StatusBadRequest)
		return
	}

	err = api.service.DeleteCustomField(ctx, orgID, fieldID)
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "custom field deleted",
	})
}

// =============================================================================
// DETECTION & ANALYSIS
// =============================================================================

// DetectColumnsRequest is the request body for column detection
type DetectColumnsRequest struct {
	Headers    []string   `json:"headers"`     // CSV column headers
	SampleData [][]string `json:"sample_data"` // Sample rows (max 10)
}

// HandleDetectNonStandardColumns analyzes CSV headers and detects non-standard columns
// POST /api/mailing/custom-fields/detect
func (api *CustomFieldsAPI) HandleDetectNonStandardColumns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgIDStr := getOrgIDFromRequest(r)
	orgID, err := uuid.Parse(orgIDStr)
	if err != nil {
		writeJSONError(w, "invalid organization ID", http.StatusBadRequest)
		return
	}

	var req DetectColumnsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if len(req.Headers) == 0 {
		writeJSONError(w, "headers are required", http.StatusBadRequest)
		return
	}

	// Limit sample data to 10 rows
	sampleData := req.SampleData
	if len(sampleData) > 10 {
		sampleData = sampleData[:10]
	}

	result, err := api.service.DetectNonStandardColumns(ctx, orgID, req.Headers, sampleData)
	if err != nil {
		writeJSONError(w, "failed to detect columns: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Build response with suggestions for each non-standard column
	response := map[string]interface{}{
		"standard_mappings":     result.StandardMappings,
		"non_standard_columns":  result.NonStandardColumns,
		"existing_custom_fields": result.ExistingCustomFields,
		"total_columns":         len(req.Headers),
		"standard_count":        len(result.StandardMappings),
		"non_standard_count":    len(result.NonStandardColumns),
	}

	// Add field creation suggestions for non-standard columns
	var suggestions []map[string]interface{}
	for _, col := range result.NonStandardColumns {
		suggestion := map[string]interface{}{
			"column_name":     col.ColumnName,
			"column_index":    col.ColumnIndex,
			"suggested_name":  col.SuggestedName,
			"suggested_type":  col.SuggestedType,
			"sample_values":   col.SampleValues,
			"action":          "create_new", // Default action
		}

		// If matches existing field, suggest mapping
		if col.ExistingFieldID != nil {
			suggestion["action"] = "map_existing"
			suggestion["existing_field_id"] = col.ExistingFieldID
		}

		// If has limited unique values, suggest enum
		if len(col.UniqueValues) > 0 && len(col.UniqueValues) <= 10 {
			suggestion["suggested_type"] = "enum"
			suggestion["suggested_enum_values"] = col.UniqueValues
		}

		suggestions = append(suggestions, suggestion)
	}
	response["suggestions"] = suggestions

	writeJSON(w, http.StatusOK, response)
}

// BulkCreateRequest is the request for bulk creating custom fields
type BulkCreateRequest struct {
	Fields []mailing.CreateCustomFieldRequest `json:"fields"`
}

// HandleBulkCreateCustomFields creates multiple custom fields at once
// POST /api/mailing/custom-fields/bulk
func (api *CustomFieldsAPI) HandleBulkCreateCustomFields(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orgIDStr := getOrgIDFromRequest(r)
	orgID, err := uuid.Parse(orgIDStr)
	if err != nil {
		writeJSONError(w, "invalid organization ID", http.StatusBadRequest)
		return
	}

	var req BulkCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if len(req.Fields) == 0 {
		writeJSONError(w, "fields array is required", http.StatusBadRequest)
		return
	}

	// Limit bulk creation to 50 fields at a time
	if len(req.Fields) > 50 {
		writeJSONError(w, "maximum 50 fields can be created at once", http.StatusBadRequest)
		return
	}

	created, err := api.service.CreateBulkCustomFields(ctx, orgID, req.Fields)
	if err != nil {
		writeJSONError(w, "failed to create custom fields: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"created_fields": created,
		"created_count":  len(created),
		"requested":      len(req.Fields),
	})
}

// =============================================================================
// STANDARD FIELDS REFERENCE
// =============================================================================

// HandleGetStandardFields returns the list of standard fields and their aliases
// GET /api/mailing/custom-fields/standard-fields
func (api *CustomFieldsAPI) HandleGetStandardFields(w http.ResponseWriter, r *http.Request) {
	// Return the standard fields with their aliases for frontend reference
	standardFields := []map[string]interface{}{
		{
			"name":         "email",
			"display_name": "Email Address",
			"type":         "email",
			"required":     true,
			"aliases":      []string{"email", "email_address", "e-mail", "emailaddress", "mail", "subscriber_email", "address"},
		},
		{
			"name":         "first_name",
			"display_name": "First Name",
			"type":         "text",
			"required":     false,
			"aliases":      []string{"first_name", "firstname", "first", "fname", "given_name", "givenname"},
		},
		{
			"name":         "last_name",
			"display_name": "Last Name",
			"type":         "text",
			"required":     false,
			"aliases":      []string{"last_name", "lastname", "last", "lname", "surname", "family_name", "familyname"},
		},
		{
			"name":         "phone",
			"display_name": "Phone Number",
			"type":         "phone",
			"required":     false,
			"aliases":      []string{"phone", "phone_number", "phonenumber", "mobile", "cell", "telephone"},
		},
		{
			"name":         "company",
			"display_name": "Company",
			"type":         "text",
			"required":     false,
			"aliases":      []string{"company", "company_name", "organization", "org", "business"},
		},
		{
			"name":         "city",
			"display_name": "City",
			"type":         "text",
			"required":     false,
			"aliases":      []string{"city", "town", "locality"},
		},
		{
			"name":         "state",
			"display_name": "State/Province",
			"type":         "text",
			"required":     false,
			"aliases":      []string{"state", "state_province", "province", "region"},
		},
		{
			"name":         "country",
			"display_name": "Country",
			"type":         "text",
			"required":     false,
			"aliases":      []string{"country", "nation", "country_code"},
		},
		{
			"name":         "postal_code",
			"display_name": "Postal Code",
			"type":         "text",
			"required":     false,
			"aliases":      []string{"postal_code", "postalcode", "zip", "zipcode", "zip_code", "postcode"},
		},
		{
			"name":         "timezone",
			"display_name": "Timezone",
			"type":         "timezone",
			"required":     false,
			"aliases":      []string{"timezone", "time_zone", "tz"},
		},
		{
			"name":         "source",
			"display_name": "Source",
			"type":         "text",
			"required":     false,
			"aliases":      []string{"source", "lead_source", "signup_source", "origin"},
		},
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"standard_fields": standardFields,
		"total":           len(standardFields),
	})
}

// =============================================================================
// HELPER FUNCTIONS
// =============================================================================

func writeJSON(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(data)
}

func writeJSONError(w http.ResponseWriter, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func isConflictError(err error) bool {
	if err == nil {
		return false
	}
	errMsg := err.Error()
	return contains(errMsg, "already exists") || contains(errMsg, "duplicate") || contains(errMsg, "unique constraint")
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
