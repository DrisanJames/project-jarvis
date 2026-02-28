package mailing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// =============================================================================
// CUSTOM FIELDS SERVICE
// =============================================================================
// Manages custom field definitions for organizations.
// Custom fields allow users to store non-standard data from CSV imports
// and use them for segmentation and personalization.

// FieldType represents the data type of a custom field
type FieldType string

const (
	FieldTypeString   FieldType = "string"
	FieldTypeNumber   FieldType = "number"
	FieldTypeBoolean  FieldType = "boolean"
	FieldTypeDate     FieldType = "date"
	FieldTypeDatetime FieldType = "datetime"
	FieldTypeEnum     FieldType = "enum"
)

// StandardFields defines the built-in fields that have special handling
var StandardFields = map[string]bool{
	"email":      true,
	"first_name": true,
	"last_name":  true,
	"status":     true,
}

// StandardFieldAliases maps common CSV column names to standard fields
var StandardFieldAliases = map[string]string{
	"email":            "email",
	"email_address":    "email",
	"e-mail":           "email",
	"emailaddress":     "email",
	"mail":             "email",
	"subscriber_email": "email",
	"address":          "email",
	"first_name":       "first_name",
	"firstname":        "first_name",
	"first":            "first_name",
	"fname":            "first_name",
	"given_name":       "first_name",
	"givenname":        "first_name",
	"last_name":        "last_name",
	"lastname":         "last_name",
	"last":             "last_name",
	"lname":            "last_name",
	"surname":          "last_name",
	"family_name":      "last_name",
	"familyname":       "last_name",
	"status":           "status",
	"subscription_status": "status",
	"sub_status":       "status",
}

// =============================================================================
// TYPES
// =============================================================================

// CustomFieldDefinition represents a custom field schema
type CustomFieldDefinition struct {
	ID             uuid.UUID  `json:"id" db:"id"`
	OrganizationID uuid.UUID  `json:"organization_id" db:"organization_id"`
	Name           string     `json:"name" db:"name"`                 // Internal name (snake_case)
	DisplayName    string     `json:"display_name" db:"display_name"` // Friendly UI name
	FieldType      FieldType  `json:"field_type" db:"field_type"`
	EnumValues     []string   `json:"enum_values,omitempty"`          // For enum types
	DefaultValue   *string    `json:"default_value,omitempty" db:"default_value"`
	IsRequired     bool       `json:"is_required" db:"is_required"`
	Description    *string    `json:"description,omitempty" db:"description"`
	IsActive       bool       `json:"is_active" db:"is_active"`
	SortOrder      int        `json:"sort_order" db:"sort_order"`
	CreatedAt      time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at" db:"updated_at"`
}

// CustomFieldStats tracks usage statistics
type CustomFieldStats struct {
	CustomFieldID        uuid.UUID               `json:"custom_field_id"`
	SubscribersWithValue int                     `json:"subscribers_with_value"`
	TotalSubscribers     int                     `json:"total_subscribers"`
	UniqueValuesCount    int                     `json:"unique_values_count"`
	MostCommonValues     []CustomFieldValueCount `json:"most_common_values,omitempty"`
	LastCalculatedAt     *time.Time              `json:"last_calculated_at,omitempty"`
}

// CustomFieldValueCount represents a value and its frequency
type CustomFieldValueCount struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// NonStandardColumnInfo describes a detected non-standard column
type NonStandardColumnInfo struct {
	ColumnName      string    `json:"column_name"`
	ColumnIndex     int       `json:"column_index"`
	SuggestedName   string    `json:"suggested_name"`   // snake_case version
	SuggestedType   FieldType `json:"suggested_type"`
	SampleValues    []string  `json:"sample_values"`
	UniqueValues    []string  `json:"unique_values,omitempty"` // If few unique values, suggest enum
	ExistingFieldID *uuid.UUID `json:"existing_field_id,omitempty"` // If matches existing custom field
}

// CreateCustomFieldRequest is the request payload for creating a custom field
type CreateCustomFieldRequest struct {
	Name         string    `json:"name"`
	DisplayName  string    `json:"display_name"`
	FieldType    FieldType `json:"field_type"`
	EnumValues   []string  `json:"enum_values,omitempty"`
	DefaultValue *string   `json:"default_value,omitempty"`
	IsRequired   bool      `json:"is_required"`
	Description  *string   `json:"description,omitempty"`
}

// HeaderAnalysisResult contains the analysis of CSV headers
type HeaderAnalysisResult struct {
	StandardMappings    map[string]string       `json:"standard_mappings"`    // header -> standard field
	NonStandardColumns  []NonStandardColumnInfo `json:"non_standard_columns"`
	ExistingCustomFields []CustomFieldDefinition `json:"existing_custom_fields"` // Org's existing custom fields
}

// =============================================================================
// CUSTOM FIELD SERVICE
// =============================================================================

// CustomFieldService manages custom field operations
type CustomFieldService struct {
	db *sql.DB
}

// NewCustomFieldService creates a new custom field service
func NewCustomFieldService(db *sql.DB) *CustomFieldService {
	return &CustomFieldService{db: db}
}

// =============================================================================
// CRUD OPERATIONS
// =============================================================================

// CreateCustomField creates a new custom field definition
func (s *CustomFieldService) CreateCustomField(ctx context.Context, orgID uuid.UUID, req CreateCustomFieldRequest) (*CustomFieldDefinition, error) {
	// Validate field type
	if !isValidFieldType(req.FieldType) {
		return nil, fmt.Errorf("invalid field type: %s", req.FieldType)
	}

	// Validate name format (snake_case, alphanumeric + underscore)
	name := normalizeFieldName(req.Name)
	if !isValidFieldName(name) {
		return nil, fmt.Errorf("invalid field name: must be alphanumeric with underscores, got '%s'", name)
	}

	// Check for conflict with standard fields
	if StandardFields[name] {
		return nil, fmt.Errorf("'%s' is a standard field and cannot be used as a custom field name", name)
	}

	// For enum types, validate enum values
	var enumValuesJSON []byte
	if req.FieldType == FieldTypeEnum {
		if len(req.EnumValues) == 0 {
			return nil, fmt.Errorf("enum fields must have at least one enum value")
		}
		enumValuesJSON, _ = json.Marshal(req.EnumValues)
	}

	// Create the field
	field := &CustomFieldDefinition{
		ID:             uuid.New(),
		OrganizationID: orgID,
		Name:           name,
		DisplayName:    req.DisplayName,
		FieldType:      req.FieldType,
		EnumValues:     req.EnumValues,
		DefaultValue:   req.DefaultValue,
		IsRequired:     req.IsRequired,
		Description:    req.Description,
		IsActive:       true,
		SortOrder:      0,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}

	query := `
		INSERT INTO mailing_custom_field_definitions 
		(id, organization_id, name, display_name, field_type, enum_values, default_value, is_required, description, is_active, sort_order, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`

	_, err := s.db.ExecContext(ctx, query,
		field.ID, field.OrganizationID, field.Name, field.DisplayName, field.FieldType,
		enumValuesJSON, field.DefaultValue, field.IsRequired, field.Description,
		field.IsActive, field.SortOrder, field.CreatedAt, field.UpdatedAt,
	)
	if err != nil {
		if strings.Contains(err.Error(), "unique constraint") || strings.Contains(err.Error(), "duplicate key") {
			return nil, fmt.Errorf("custom field '%s' already exists for this organization", name)
		}
		return nil, fmt.Errorf("failed to create custom field: %w", err)
	}

	return field, nil
}

// ListCustomFields returns all custom fields for an organization
func (s *CustomFieldService) ListCustomFields(ctx context.Context, orgID uuid.UUID) ([]CustomFieldDefinition, error) {
	query := `
		SELECT id, organization_id, name, display_name, field_type, enum_values, 
		       default_value, is_required, description, is_active, sort_order, created_at, updated_at
		FROM mailing_custom_field_definitions
		WHERE organization_id = $1 AND is_active = TRUE
		ORDER BY sort_order, display_name
	`

	rows, err := s.db.QueryContext(ctx, query, orgID)
	if err != nil {
		return nil, fmt.Errorf("failed to list custom fields: %w", err)
	}
	defer rows.Close()

	var fields []CustomFieldDefinition
	for rows.Next() {
		var field CustomFieldDefinition
		var enumValuesJSON []byte

		err := rows.Scan(
			&field.ID, &field.OrganizationID, &field.Name, &field.DisplayName, &field.FieldType,
			&enumValuesJSON, &field.DefaultValue, &field.IsRequired, &field.Description,
			&field.IsActive, &field.SortOrder, &field.CreatedAt, &field.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan custom field: %w", err)
		}

		if len(enumValuesJSON) > 0 {
			json.Unmarshal(enumValuesJSON, &field.EnumValues)
		}

		fields = append(fields, field)
	}

	return fields, nil
}

// GetCustomField retrieves a specific custom field by ID
func (s *CustomFieldService) GetCustomField(ctx context.Context, orgID, fieldID uuid.UUID) (*CustomFieldDefinition, error) {
	query := `
		SELECT id, organization_id, name, display_name, field_type, enum_values, 
		       default_value, is_required, description, is_active, sort_order, created_at, updated_at
		FROM mailing_custom_field_definitions
		WHERE id = $1 AND organization_id = $2
	`

	var field CustomFieldDefinition
	var enumValuesJSON []byte

	err := s.db.QueryRowContext(ctx, query, fieldID, orgID).Scan(
		&field.ID, &field.OrganizationID, &field.Name, &field.DisplayName, &field.FieldType,
		&enumValuesJSON, &field.DefaultValue, &field.IsRequired, &field.Description,
		&field.IsActive, &field.SortOrder, &field.CreatedAt, &field.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get custom field: %w", err)
	}

	if len(enumValuesJSON) > 0 {
		json.Unmarshal(enumValuesJSON, &field.EnumValues)
	}

	return &field, nil
}

// GetCustomFieldByName retrieves a custom field by name
func (s *CustomFieldService) GetCustomFieldByName(ctx context.Context, orgID uuid.UUID, name string) (*CustomFieldDefinition, error) {
	normalizedName := normalizeFieldName(name)

	query := `
		SELECT id, organization_id, name, display_name, field_type, enum_values, 
		       default_value, is_required, description, is_active, sort_order, created_at, updated_at
		FROM mailing_custom_field_definitions
		WHERE organization_id = $1 AND name = $2 AND is_active = TRUE
	`

	var field CustomFieldDefinition
	var enumValuesJSON []byte

	err := s.db.QueryRowContext(ctx, query, orgID, normalizedName).Scan(
		&field.ID, &field.OrganizationID, &field.Name, &field.DisplayName, &field.FieldType,
		&enumValuesJSON, &field.DefaultValue, &field.IsRequired, &field.Description,
		&field.IsActive, &field.SortOrder, &field.CreatedAt, &field.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get custom field: %w", err)
	}

	if len(enumValuesJSON) > 0 {
		json.Unmarshal(enumValuesJSON, &field.EnumValues)
	}

	return &field, nil
}

// UpdateCustomField updates a custom field
func (s *CustomFieldService) UpdateCustomField(ctx context.Context, orgID, fieldID uuid.UUID, req CreateCustomFieldRequest) (*CustomFieldDefinition, error) {
	// Validate field type
	if !isValidFieldType(req.FieldType) {
		return nil, fmt.Errorf("invalid field type: %s", req.FieldType)
	}

	var enumValuesJSON []byte
	if req.FieldType == FieldTypeEnum {
		if len(req.EnumValues) == 0 {
			return nil, fmt.Errorf("enum fields must have at least one enum value")
		}
		enumValuesJSON, _ = json.Marshal(req.EnumValues)
	}

	query := `
		UPDATE mailing_custom_field_definitions
		SET display_name = $1, field_type = $2, enum_values = $3, default_value = $4, 
		    is_required = $5, description = $6, updated_at = NOW()
		WHERE id = $7 AND organization_id = $8
		RETURNING id, organization_id, name, display_name, field_type, enum_values, 
		          default_value, is_required, description, is_active, sort_order, created_at, updated_at
	`

	var field CustomFieldDefinition
	var returnedEnumJSON []byte

	err := s.db.QueryRowContext(ctx, query,
		req.DisplayName, req.FieldType, enumValuesJSON, req.DefaultValue,
		req.IsRequired, req.Description, fieldID, orgID,
	).Scan(
		&field.ID, &field.OrganizationID, &field.Name, &field.DisplayName, &field.FieldType,
		&returnedEnumJSON, &field.DefaultValue, &field.IsRequired, &field.Description,
		&field.IsActive, &field.SortOrder, &field.CreatedAt, &field.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("custom field not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to update custom field: %w", err)
	}

	if len(returnedEnumJSON) > 0 {
		json.Unmarshal(returnedEnumJSON, &field.EnumValues)
	}

	return &field, nil
}

// DeleteCustomField soft-deletes a custom field
func (s *CustomFieldService) DeleteCustomField(ctx context.Context, orgID, fieldID uuid.UUID) error {
	query := `
		UPDATE mailing_custom_field_definitions
		SET is_active = FALSE, updated_at = NOW()
		WHERE id = $1 AND organization_id = $2
	`

	result, err := s.db.ExecContext(ctx, query, fieldID, orgID)
	if err != nil {
		return fmt.Errorf("failed to delete custom field: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("custom field not found")
	}

	return nil
}

// =============================================================================
// HEADER ANALYSIS & TYPE DETECTION
// =============================================================================

// DetectNonStandardColumns analyzes CSV headers and returns non-standard columns
func (s *CustomFieldService) DetectNonStandardColumns(ctx context.Context, orgID uuid.UUID, headers []string, sampleData [][]string) (*HeaderAnalysisResult, error) {
	result := &HeaderAnalysisResult{
		StandardMappings:   make(map[string]string),
		NonStandardColumns: []NonStandardColumnInfo{},
	}

	// Get existing custom fields for this org
	existingFields, err := s.ListCustomFields(ctx, orgID)
	if err != nil {
		return nil, err
	}
	result.ExistingCustomFields = existingFields

	// Build lookup map for existing custom fields
	existingFieldMap := make(map[string]*CustomFieldDefinition)
	for i := range existingFields {
		existingFieldMap[existingFields[i].Name] = &existingFields[i]
	}

	// Analyze each header
	for colIdx, header := range headers {
		normalizedHeader := normalizeFieldName(header)

		// Check if it maps to a standard field
		if stdField, ok := StandardFieldAliases[normalizedHeader]; ok {
			result.StandardMappings[header] = stdField
			continue
		}

		// Also check direct standard field match
		if StandardFields[normalizedHeader] {
			result.StandardMappings[header] = normalizedHeader
			continue
		}

		// This is a non-standard column
		info := NonStandardColumnInfo{
			ColumnName:    header,
			ColumnIndex:   colIdx,
			SuggestedName: normalizedHeader,
		}

		// Collect sample values for this column
		var sampleValues []string
		uniqueValues := make(map[string]bool)
		for _, row := range sampleData {
			if colIdx < len(row) && row[colIdx] != "" {
				val := strings.TrimSpace(row[colIdx])
				sampleValues = append(sampleValues, val)
				uniqueValues[val] = true
			}
		}
		if len(sampleValues) > 5 {
			info.SampleValues = sampleValues[:5]
		} else {
			info.SampleValues = sampleValues
		}

		// If few unique values, store them for potential enum
		if len(uniqueValues) <= 10 && len(uniqueValues) > 0 {
			for v := range uniqueValues {
				info.UniqueValues = append(info.UniqueValues, v)
			}
		}

		// Suggest field type based on column name and sample values
		info.SuggestedType = SuggestFieldType(normalizedHeader, sampleValues)

		// Check if this matches an existing custom field
		if existingField, ok := existingFieldMap[normalizedHeader]; ok {
			info.ExistingFieldID = &existingField.ID
		}

		result.NonStandardColumns = append(result.NonStandardColumns, info)
	}

	return result, nil
}

// SuggestFieldType analyzes sample values to suggest the best field type
func SuggestFieldType(fieldName string, sampleValues []string) FieldType {
	// First, check name patterns
	nameLower := strings.ToLower(fieldName)

	// Boolean patterns in name
	if strings.HasPrefix(nameLower, "is_") ||
		strings.HasPrefix(nameLower, "has_") ||
		strings.HasPrefix(nameLower, "can_") ||
		strings.HasPrefix(nameLower, "should_") ||
		strings.Contains(nameLower, "enabled") ||
		strings.Contains(nameLower, "active") ||
		strings.Contains(nameLower, "verified") ||
		strings.Contains(nameLower, "valid") {
		return FieldTypeBoolean
	}

	// Date patterns in name
	if strings.HasSuffix(nameLower, "_at") ||
		strings.HasSuffix(nameLower, "_on") ||
		strings.HasSuffix(nameLower, "_date") ||
		strings.Contains(nameLower, "created") ||
		strings.Contains(nameLower, "updated") ||
		strings.Contains(nameLower, "timestamp") {
		return FieldTypeDatetime
	}

	// Number patterns in name
	if strings.Contains(nameLower, "count") ||
		strings.Contains(nameLower, "amount") ||
		strings.Contains(nameLower, "score") ||
		strings.Contains(nameLower, "number") ||
		strings.Contains(nameLower, "qty") ||
		strings.Contains(nameLower, "quantity") ||
		strings.Contains(nameLower, "total") ||
		strings.Contains(nameLower, "sum") ||
		strings.Contains(nameLower, "age") {
		return FieldTypeNumber
	}

	// Analyze sample values
	if len(sampleValues) == 0 {
		return FieldTypeString
	}

	booleanCount := 0
	numberCount := 0
	dateCount := 0
	uniqueValues := make(map[string]bool)

	for _, val := range sampleValues {
		normalizedVal := strings.ToLower(strings.TrimSpace(val))
		uniqueValues[normalizedVal] = true

		// Check for boolean values
		if normalizedVal == "true" || normalizedVal == "false" ||
			normalizedVal == "yes" || normalizedVal == "no" ||
			normalizedVal == "1" || normalizedVal == "0" {
			booleanCount++
			continue
		}

		// Check for numeric values
		if isNumeric(val) {
			numberCount++
			continue
		}

		// Check for date values
		if isDateLike(val) {
			dateCount++
		}
	}

	total := len(sampleValues)

	// If >80% are boolean-like, suggest boolean
	if float64(booleanCount)/float64(total) > 0.8 {
		return FieldTypeBoolean
	}

	// If >80% are numeric, suggest number
	if float64(numberCount)/float64(total) > 0.8 {
		return FieldTypeNumber
	}

	// If >80% look like dates, suggest datetime
	if float64(dateCount)/float64(total) > 0.8 {
		return FieldTypeDatetime
	}

	// If few unique values, suggest enum
	if len(uniqueValues) <= 10 && len(uniqueValues) >= 2 && len(sampleValues) >= 5 {
		return FieldTypeEnum
	}

	return FieldTypeString
}

// =============================================================================
// CUSTOM FIELD VALUE OPERATIONS
// =============================================================================

// ValidateCustomFieldValue validates a value against a field definition
func (s *CustomFieldService) ValidateCustomFieldValue(field *CustomFieldDefinition, value string) error {
	if value == "" {
		if field.IsRequired {
			return fmt.Errorf("field '%s' is required", field.DisplayName)
		}
		return nil
	}

	switch field.FieldType {
	case FieldTypeBoolean:
		lower := strings.ToLower(value)
		if lower != "true" && lower != "false" && lower != "1" && lower != "0" && lower != "yes" && lower != "no" {
			return fmt.Errorf("field '%s' must be a boolean value", field.DisplayName)
		}

	case FieldTypeNumber:
		if !isNumeric(value) {
			return fmt.Errorf("field '%s' must be a number", field.DisplayName)
		}

	case FieldTypeDate, FieldTypeDatetime:
		if !isDateLike(value) {
			return fmt.Errorf("field '%s' must be a valid date", field.DisplayName)
		}

	case FieldTypeEnum:
		found := false
		for _, enumVal := range field.EnumValues {
			if strings.EqualFold(value, enumVal) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("field '%s' must be one of: %v", field.DisplayName, field.EnumValues)
		}
	}

	return nil
}

// NormalizeCustomFieldValue normalizes a value based on field type
func NormalizeCustomFieldValue(fieldType FieldType, value string) interface{} {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}

	switch fieldType {
	case FieldTypeBoolean:
		lower := strings.ToLower(value)
		return lower == "true" || lower == "1" || lower == "yes"

	case FieldTypeNumber:
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			return f
		}
		return value

	default:
		return value
	}
}

// =============================================================================
// BATCH OPERATIONS FOR IMPORTS
// =============================================================================

// CreateBulkCustomFields creates multiple custom fields in a transaction
func (s *CustomFieldService) CreateBulkCustomFields(ctx context.Context, orgID uuid.UUID, requests []CreateCustomFieldRequest) ([]CustomFieldDefinition, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to start transaction: %w", err)
	}
	defer tx.Rollback()

	var created []CustomFieldDefinition

	for _, req := range requests {
		name := normalizeFieldName(req.Name)
		if !isValidFieldName(name) || StandardFields[name] {
			continue // Skip invalid fields
		}

		var enumValuesJSON []byte
		if req.FieldType == FieldTypeEnum && len(req.EnumValues) > 0 {
			enumValuesJSON, _ = json.Marshal(req.EnumValues)
		}

		field := CustomFieldDefinition{
			ID:             uuid.New(),
			OrganizationID: orgID,
			Name:           name,
			DisplayName:    req.DisplayName,
			FieldType:      req.FieldType,
			EnumValues:     req.EnumValues,
			DefaultValue:   req.DefaultValue,
			IsRequired:     req.IsRequired,
			Description:    req.Description,
			IsActive:       true,
			CreatedAt:      time.Now(),
			UpdatedAt:      time.Now(),
		}

		_, err := tx.ExecContext(ctx, `
			INSERT INTO mailing_custom_field_definitions 
			(id, organization_id, name, display_name, field_type, enum_values, default_value, is_required, description, is_active, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT (organization_id, name) DO NOTHING
		`,
			field.ID, field.OrganizationID, field.Name, field.DisplayName, field.FieldType,
			enumValuesJSON, field.DefaultValue, field.IsRequired, field.Description,
			field.IsActive, field.CreatedAt, field.UpdatedAt,
		)
		if err == nil {
			created = append(created, field)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return created, nil
}

// =============================================================================
// HELPER FUNCTIONS
// =============================================================================

func normalizeFieldName(name string) string {
	// Convert to lowercase
	normalized := strings.ToLower(strings.TrimSpace(name))
	// Replace spaces and hyphens with underscores
	normalized = strings.ReplaceAll(normalized, " ", "_")
	normalized = strings.ReplaceAll(normalized, "-", "_")
	// Remove any characters that aren't alphanumeric or underscore
	reg := regexp.MustCompile(`[^a-z0-9_]`)
	normalized = reg.ReplaceAllString(normalized, "")
	// Remove multiple consecutive underscores
	reg = regexp.MustCompile(`_+`)
	normalized = reg.ReplaceAllString(normalized, "_")
	// Remove leading/trailing underscores
	normalized = strings.Trim(normalized, "_")
	return normalized
}

func isValidFieldName(name string) bool {
	if name == "" || len(name) > 100 {
		return false
	}
	// Must be alphanumeric with underscores, start with letter
	matched, _ := regexp.MatchString(`^[a-z][a-z0-9_]*$`, name)
	return matched
}

func isValidFieldType(ft FieldType) bool {
	switch ft {
	case FieldTypeString, FieldTypeNumber, FieldTypeBoolean, FieldTypeDate, FieldTypeDatetime, FieldTypeEnum:
		return true
	}
	return false
}

func isNumeric(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

func isDateLike(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}

	// Common date formats
	formats := []string{
		"2006-01-02",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"01/02/2006",
		"02/01/2006",
		"Jan 2, 2006",
		"January 2, 2006",
	}

	for _, format := range formats {
		if _, err := time.Parse(format, s); err == nil {
			return true
		}
	}
	return false
}
