// Package segmentation provides enterprise-grade email segmentation capabilities
// with complex boolean logic, event-based queries, and computed fields.
package segmentation

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// ==========================================
// OPERATORS
// ==========================================

// Operator represents a comparison operator
type Operator string

const (
	// String operators
	OpEquals      Operator = "equals"
	OpNotEquals   Operator = "not_equals"
	OpContains    Operator = "contains"
	OpNotContains Operator = "not_contains"
	OpStartsWith  Operator = "starts_with"
	OpEndsWith    Operator = "ends_with"
	OpIsEmpty     Operator = "is_empty"
	OpIsNotEmpty  Operator = "is_not_empty"
	OpMatchesRegex Operator = "matches_regex"

	// Numeric operators
	OpGt         Operator = "gt"
	OpGte        Operator = "gte"
	OpLt         Operator = "lt"
	OpLte        Operator = "lte"
	OpBetween    Operator = "between"
	OpNotBetween Operator = "not_between"

	// Date operators
	OpDateEquals      Operator = "date_equals"
	OpDateBefore      Operator = "date_before"
	OpDateAfter       Operator = "date_after"
	OpDateBetween     Operator = "date_between"
	OpInLastDays      Operator = "in_last_days"
	OpInNextDays      Operator = "in_next_days"
	OpMoreThanDaysAgo Operator = "more_than_days_ago"
	OpAnniversaryMonth Operator = "anniversary_month"
	OpAnniversaryDay   Operator = "anniversary_day"

	// Array/List operators
	OpContainsAny      Operator = "contains_any"
	OpContainsAll      Operator = "contains_all"
	OpNotContainsAny   Operator = "not_contains_any"
	OpArrayIsEmpty     Operator = "array_is_empty"
	OpArrayIsNotEmpty  Operator = "array_is_not_empty"

	// Boolean operators
	OpIsTrue  Operator = "is_true"
	OpIsFalse Operator = "is_false"

	// NULL checks
	OpIsNull    Operator = "is_null"
	OpIsNotNull Operator = "is_not_null"

	// Event-specific operators
	OpEventCountGte         Operator = "event_count_gte"
	OpEventCountLte         Operator = "event_count_lte"
	OpEventCountBetween     Operator = "event_count_between"
	OpEventInLastDays       Operator = "event_in_last_days"
	OpEventNotInLastDays    Operator = "event_not_in_last_days"
	OpEventPropertyEquals   Operator = "event_property_equals"
	OpEventPropertyContains Operator = "event_property_contains"
)

// OperatorMetadata contains info about an operator
type OperatorMetadata struct {
	Operator       Operator `json:"operator"`
	Label          string   `json:"label"`
	Description    string   `json:"description"`
	ApplicableTypes []FieldType `json:"applicable_types"`
	RequiresValue  bool     `json:"requires_value"`
	RequiresSecondary bool  `json:"requires_secondary"` // For "between" operators
	RequiresArray  bool     `json:"requires_array"`     // For "contains_any" etc
}

// GetOperatorMetadata returns metadata for all operators
func GetOperatorMetadata() []OperatorMetadata {
	return []OperatorMetadata{
		// String operators
		{OpEquals, "Equals", "Exact match", []FieldType{FieldString, FieldNumber, FieldInteger}, true, false, false},
		{OpNotEquals, "Does not equal", "Not an exact match", []FieldType{FieldString, FieldNumber, FieldInteger}, true, false, false},
		{OpContains, "Contains", "Contains the text", []FieldType{FieldString}, true, false, false},
		{OpNotContains, "Does not contain", "Does not contain the text", []FieldType{FieldString}, true, false, false},
		{OpStartsWith, "Starts with", "Begins with the text", []FieldType{FieldString}, true, false, false},
		{OpEndsWith, "Ends with", "Ends with the text", []FieldType{FieldString}, true, false, false},
		{OpIsEmpty, "Is empty", "Field is empty or null", []FieldType{FieldString}, false, false, false},
		{OpIsNotEmpty, "Is not empty", "Field has a value", []FieldType{FieldString}, false, false, false},

		// Numeric operators
		{OpGt, "Greater than", "Value is greater than", []FieldType{FieldNumber, FieldInteger, FieldDecimal}, true, false, false},
		{OpGte, "Greater than or equal", "Value is greater than or equal to", []FieldType{FieldNumber, FieldInteger, FieldDecimal}, true, false, false},
		{OpLt, "Less than", "Value is less than", []FieldType{FieldNumber, FieldInteger, FieldDecimal}, true, false, false},
		{OpLte, "Less than or equal", "Value is less than or equal to", []FieldType{FieldNumber, FieldInteger, FieldDecimal}, true, false, false},
		{OpBetween, "Between", "Value is between two numbers", []FieldType{FieldNumber, FieldInteger, FieldDecimal}, true, true, false},
		{OpNotBetween, "Not between", "Value is not between two numbers", []FieldType{FieldNumber, FieldInteger, FieldDecimal}, true, true, false},

		// Date operators
		{OpDateEquals, "On date", "Exactly on the date", []FieldType{FieldDate, FieldDatetime}, true, false, false},
		{OpDateBefore, "Before date", "Before the date", []FieldType{FieldDate, FieldDatetime}, true, false, false},
		{OpDateAfter, "After date", "After the date", []FieldType{FieldDate, FieldDatetime}, true, false, false},
		{OpDateBetween, "Between dates", "Between two dates", []FieldType{FieldDate, FieldDatetime}, true, true, false},
		{OpInLastDays, "In the last X days", "Within the last N days", []FieldType{FieldDate, FieldDatetime}, true, false, false},
		{OpInNextDays, "In the next X days", "Within the next N days", []FieldType{FieldDate, FieldDatetime}, true, false, false},
		{OpMoreThanDaysAgo, "More than X days ago", "More than N days in the past", []FieldType{FieldDate, FieldDatetime}, true, false, false},
		{OpAnniversaryMonth, "Anniversary month", "Month matches (ignores year)", []FieldType{FieldDate, FieldDatetime}, true, false, false},
		{OpAnniversaryDay, "Anniversary day", "Day and month match (ignores year)", []FieldType{FieldDate, FieldDatetime}, true, false, false},

		// Array operators
		{OpContainsAny, "Contains any of", "Contains at least one of the values", []FieldType{FieldArray, FieldTags}, false, false, true},
		{OpContainsAll, "Contains all of", "Contains all of the values", []FieldType{FieldArray, FieldTags}, false, false, true},
		{OpNotContainsAny, "Does not contain any of", "Contains none of the values", []FieldType{FieldArray, FieldTags}, false, false, true},
		{OpArrayIsEmpty, "Array is empty", "Array has no items", []FieldType{FieldArray, FieldTags}, false, false, false},
		{OpArrayIsNotEmpty, "Array is not empty", "Array has at least one item", []FieldType{FieldArray, FieldTags}, false, false, false},

		// Boolean operators
		{OpIsTrue, "Is true", "Boolean is true", []FieldType{FieldBoolean}, false, false, false},
		{OpIsFalse, "Is false", "Boolean is false", []FieldType{FieldBoolean}, false, false, false},

		// NULL checks
		{OpIsNull, "Is null", "Value is null/missing", []FieldType{FieldString, FieldNumber, FieldDate, FieldBoolean}, false, false, false},
		{OpIsNotNull, "Is not null", "Value exists", []FieldType{FieldString, FieldNumber, FieldDate, FieldBoolean}, false, false, false},

		// Event operators
		{OpEventCountGte, "Event count >=", "Event occurred at least N times", []FieldType{FieldEvent}, true, false, false},
		{OpEventCountLte, "Event count <=", "Event occurred at most N times", []FieldType{FieldEvent}, true, false, false},
		{OpEventCountBetween, "Event count between", "Event occurred between N and M times", []FieldType{FieldEvent}, true, true, false},
		{OpEventInLastDays, "Event in last X days", "Event occurred in the last N days", []FieldType{FieldEvent}, true, false, false},
		{OpEventNotInLastDays, "Event NOT in last X days", "Event did not occur in the last N days", []FieldType{FieldEvent}, true, false, false},
		{OpEventPropertyEquals, "Event property equals", "Event has a property with specific value", []FieldType{FieldEvent}, true, false, false},
		{OpEventPropertyContains, "Event property contains", "Event property contains value", []FieldType{FieldEvent}, true, false, false},
	}
}

// ==========================================
// FIELD TYPES
// ==========================================

// FieldType represents the data type of a field
type FieldType string

const (
	FieldString   FieldType = "string"
	FieldNumber   FieldType = "number"
	FieldInteger  FieldType = "integer"
	FieldDecimal  FieldType = "decimal"
	FieldBoolean  FieldType = "boolean"
	FieldDate     FieldType = "date"
	FieldDatetime FieldType = "datetime"
	FieldArray    FieldType = "array"
	FieldTags     FieldType = "tags"
	FieldEvent    FieldType = "event"
)

// ==========================================
// CONDITION TYPES
// ==========================================

// ConditionType represents the category of condition
type ConditionType string

const (
	ConditionProfile     ConditionType = "profile"      // Contact attributes
	ConditionCustomField ConditionType = "custom_field" // Custom fields in JSONB
	ConditionEvent       ConditionType = "event"        // Behavioral events
	ConditionComputed    ConditionType = "computed"     // Computed fields
	ConditionTag         ConditionType = "tag"          // Array/tag matching
)

// ==========================================
// LOGIC OPERATORS
// ==========================================

// LogicOperator for combining conditions
type LogicOperator string

const (
	LogicAnd LogicOperator = "AND"
	LogicOr  LogicOperator = "OR"
)

// ==========================================
// SEGMENT STRUCTURES
// ==========================================

// Segment represents an email segment with conditions
type Segment struct {
	ID                   uuid.UUID       `json:"id" db:"id"`
	OrganizationID       uuid.UUID       `json:"organization_id" db:"organization_id"`
	ListID               *uuid.UUID      `json:"list_id,omitempty" db:"list_id"`
	Name                 string          `json:"name" db:"name"`
	Description          string          `json:"description,omitempty" db:"description"`
	SegmentType          string          `json:"segment_type" db:"segment_type"` // dynamic, static
	CalculationMode      string          `json:"calculation_mode" db:"calculation_mode"` // realtime, batch, hybrid
	RefreshIntervalMin   int             `json:"refresh_interval_minutes" db:"refresh_interval_minutes"`
	IncludeSuppressed    bool            `json:"include_suppressed" db:"include_suppressed"`
	GlobalExclusionRules json.RawMessage `json:"global_exclusion_rules" db:"global_exclusion_rules"`
	SubscriberCount      int             `json:"subscriber_count" db:"subscriber_count"`
	LastCalculatedAt     *time.Time      `json:"last_calculated_at,omitempty" db:"last_calculated_at"`
	Status               string          `json:"status" db:"status"`
	CreatedBy            *uuid.UUID      `json:"created_by,omitempty" db:"created_by"`
	LastEditedBy         *uuid.UUID      `json:"last_edited_by,omitempty" db:"last_edited_by"`
	LastEditedAt         *time.Time      `json:"last_edited_at,omitempty" db:"last_edited_at"`
	CreatedAt            time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at" db:"updated_at"`
}

// ConditionGroup represents a group of conditions with AND/OR logic
type ConditionGroup struct {
	ID            uuid.UUID        `json:"id" db:"id"`
	SegmentID     uuid.UUID        `json:"segment_id" db:"segment_id"`
	ParentGroupID *uuid.UUID       `json:"parent_group_id,omitempty" db:"parent_group_id"`
	LogicOperator LogicOperator    `json:"logic_operator" db:"logic_operator"`
	IsNegated     bool             `json:"is_negated" db:"is_negated"`
	SortOrder     int              `json:"sort_order" db:"sort_order"`
	Conditions    []Condition      `json:"conditions,omitempty"`
	ChildGroups   []ConditionGroup `json:"child_groups,omitempty"`
	CreatedAt     time.Time        `json:"created_at" db:"created_at"`
}

// Condition represents a single condition in a segment
type Condition struct {
	ID               uuid.UUID     `json:"id" db:"id"`
	SegmentID        uuid.UUID     `json:"segment_id" db:"segment_id"`
	GroupID          uuid.UUID     `json:"group_id" db:"group_id"`
	ConditionType    ConditionType `json:"condition_type" db:"condition_type"`
	Field            string        `json:"field" db:"field"`
	FieldType        FieldType     `json:"field_type,omitempty" db:"field_type"`
	Operator         Operator      `json:"operator" db:"operator"`
	Value            string        `json:"value,omitempty" db:"value"`
	ValueSecondary   string        `json:"value_secondary,omitempty" db:"value_secondary"`
	ValuesArray      []string      `json:"values_array,omitempty"`
	
	// Event-specific fields
	EventName           string `json:"event_name,omitempty" db:"event_name"`
	EventTimeWindowDays int    `json:"event_time_window_days,omitempty" db:"event_time_window_days"`
	EventMinCount       int    `json:"event_min_count,omitempty" db:"event_min_count"`
	EventMaxCount       int    `json:"event_max_count,omitempty" db:"event_max_count"`
	EventPropertyPath   string `json:"event_property_path,omitempty" db:"event_property_path"`
	
	SortOrder int       `json:"sort_order" db:"sort_order"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
}

// ==========================================
// SEGMENT BUILDER INPUT STRUCTURES
// ==========================================

// SegmentBuilder is used to build segments from API requests
type SegmentBuilder struct {
	Name              string                 `json:"name"`
	Description       string                 `json:"description,omitempty"`
	ListID            *uuid.UUID             `json:"list_id,omitempty"`
	CalculationMode   string                 `json:"calculation_mode,omitempty"`
	IncludeSuppressed bool                   `json:"include_suppressed"`
	RootGroup         ConditionGroupBuilder  `json:"root_group"`
	GlobalExclusions  []ConditionBuilder     `json:"global_exclusions,omitempty"`
}

// ConditionGroupBuilder is used to build condition groups
type ConditionGroupBuilder struct {
	LogicOperator LogicOperator           `json:"logic_operator"`
	IsNegated     bool                    `json:"is_negated"`
	Conditions    []ConditionBuilder      `json:"conditions,omitempty"`
	Groups        []ConditionGroupBuilder `json:"groups,omitempty"`
}

// ConditionBuilder is used to build individual conditions
type ConditionBuilder struct {
	ConditionType       ConditionType `json:"condition_type"`
	Field               string        `json:"field"`
	FieldType           FieldType     `json:"field_type,omitempty"`
	Operator            Operator      `json:"operator"`
	Value               string        `json:"value,omitempty"`
	ValueSecondary      string        `json:"value_secondary,omitempty"`
	ValuesArray         []string      `json:"values_array,omitempty"`
	EventName           string        `json:"event_name,omitempty"`
	EventTimeWindowDays int           `json:"event_time_window_days,omitempty"`
	EventMinCount       int           `json:"event_min_count,omitempty"`
	EventMaxCount       int           `json:"event_max_count,omitempty"`
	EventPropertyPath   string        `json:"event_property_path,omitempty"`
}

// ==========================================
// SEGMENT RESULTS
// ==========================================

// SegmentResult represents the result of a segment calculation
type SegmentResult struct {
	SegmentID       uuid.UUID   `json:"segment_id"`
	SubscriberCount int         `json:"subscriber_count"`
	SubscriberIDs   []uuid.UUID `json:"subscriber_ids,omitempty"` // Only for small segments or previews
	QueryHash       string      `json:"query_hash"`
	CalculatedAt    time.Time   `json:"calculated_at"`
	DurationMs      int64       `json:"duration_ms"`
}

// SegmentPreview represents a preview of segment results
type SegmentPreview struct {
	EstimatedCount   int               `json:"estimated_count"`
	SampleSubscribers []SubscriberPreview `json:"sample_subscribers"`
	QuerySQL         string            `json:"query_sql,omitempty"` // Only in debug mode
	CalculatedAt     time.Time         `json:"calculated_at"`
}

// SubscriberPreview is a minimal subscriber representation for previews
type SubscriberPreview struct {
	ID              uuid.UUID `json:"id"`
	Email           string    `json:"email"`
	FirstName       string    `json:"first_name,omitempty"`
	LastName        string    `json:"last_name,omitempty"`
	EngagementScore float64   `json:"engagement_score"`
}

// ==========================================
// SEGMENT SNAPSHOTS
// ==========================================

// SegmentSnapshot represents a frozen segment at a point in time
type SegmentSnapshot struct {
	ID                 uuid.UUID       `json:"id" db:"id"`
	SegmentID          uuid.UUID       `json:"segment_id" db:"segment_id"`
	OrganizationID     uuid.UUID       `json:"organization_id" db:"organization_id"`
	Name               string          `json:"name,omitempty" db:"name"`
	Description        string          `json:"description,omitempty" db:"description"`
	ConditionsSnapshot json.RawMessage `json:"conditions_snapshot" db:"conditions_snapshot"`
	SubscriberCount    int             `json:"subscriber_count" db:"subscriber_count"`
	SubscriberIDs      []uuid.UUID     `json:"subscriber_ids,omitempty"`
	QueryHash          string          `json:"query_hash,omitempty" db:"query_hash"`
	Purpose            string          `json:"purpose" db:"purpose"` // manual, ab_test, campaign, audit
	CampaignID         *uuid.UUID      `json:"campaign_id,omitempty" db:"campaign_id"`
	CreatedBy          *uuid.UUID      `json:"created_by,omitempty" db:"created_by"`
	SnapshotAt         time.Time       `json:"snapshot_at" db:"snapshot_at"`
	ExpiresAt          *time.Time      `json:"expires_at,omitempty" db:"expires_at"`
	CreatedAt          time.Time       `json:"created_at" db:"created_at"`
}

// ==========================================
// CUSTOM EVENTS
// ==========================================

// CustomEvent represents a behavioral event
type CustomEvent struct {
	ID             uuid.UUID       `json:"id" db:"id"`
	OrganizationID uuid.UUID       `json:"organization_id" db:"organization_id"`
	SubscriberID   uuid.UUID       `json:"subscriber_id" db:"subscriber_id"`
	EventName      string          `json:"event_name" db:"event_name"`
	EventCategory  string          `json:"event_category,omitempty" db:"event_category"`
	Properties     json.RawMessage `json:"properties,omitempty" db:"properties"`
	Source         string          `json:"source,omitempty" db:"source"`
	IPAddress      string          `json:"ip_address,omitempty" db:"ip_address"`
	UserAgent      string          `json:"user_agent,omitempty" db:"user_agent"`
	DeviceType     string          `json:"device_type,omitempty" db:"device_type"`
	GeoCountry     string          `json:"geo_country,omitempty" db:"geo_country"`
	GeoRegion      string          `json:"geo_region,omitempty" db:"geo_region"`
	GeoCity        string          `json:"geo_city,omitempty" db:"geo_city"`
	SessionID      string          `json:"session_id,omitempty" db:"session_id"`
	EventAt        time.Time       `json:"event_at" db:"event_at"`
	CreatedAt      time.Time       `json:"created_at" db:"created_at"`
}

// ==========================================
// CONTACT FIELDS (Schema Registry)
// ==========================================

// ContactField represents a custom field definition
type ContactField struct {
	ID              uuid.UUID       `json:"id" db:"id"`
	OrganizationID  uuid.UUID       `json:"organization_id" db:"organization_id"`
	FieldKey        string          `json:"field_key" db:"field_key"`
	FieldLabel      string          `json:"field_label" db:"field_label"`
	FieldType       FieldType       `json:"field_type" db:"field_type"`
	Description     string          `json:"description,omitempty" db:"description"`
	Category        string          `json:"category,omitempty" db:"category"`
	IsSystem        bool            `json:"is_system" db:"is_system"`
	IsPII           bool            `json:"is_pii" db:"is_pii"`
	IsRequired      bool            `json:"is_required" db:"is_required"`
	ValidationRules json.RawMessage `json:"validation_rules,omitempty" db:"validation_rules"`
	DefaultValue    string          `json:"default_value,omitempty" db:"default_value"`
	AllowedValues   []string        `json:"allowed_values,omitempty"`
	DisplayOrder    int             `json:"display_order" db:"display_order"`
	IsVisible       bool            `json:"is_visible" db:"is_visible"`
	CreatedAt       time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at" db:"updated_at"`
}

// ==========================================
// COMPUTED FIELDS
// ==========================================

// ComputedFields represents pre-calculated subscriber metrics
type ComputedFields struct {
	SubscriberID       uuid.UUID  `json:"subscriber_id" db:"subscriber_id"`
	OrganizationID     uuid.UUID  `json:"organization_id" db:"organization_id"`
	TotalPurchases     int        `json:"total_purchases" db:"total_purchases"`
	TotalRevenue       float64    `json:"total_revenue" db:"total_revenue"`
	AverageOrderValue  float64    `json:"average_order_value" db:"average_order_value"`
	FirstEmailAt       *time.Time `json:"first_email_at,omitempty" db:"first_email_at"`
	LastActiveAt       *time.Time `json:"last_active_at,omitempty" db:"last_active_at"`
	LastPurchaseAt     *time.Time `json:"last_purchase_at,omitempty" db:"last_purchase_at"`
	LastLoginAt        *time.Time `json:"last_login_at,omitempty" db:"last_login_at"`
	Opens7d            int        `json:"opens_7d" db:"opens_7d"`
	Opens30d           int        `json:"opens_30d" db:"opens_30d"`
	Opens90d           int        `json:"opens_90d" db:"opens_90d"`
	Clicks7d           int        `json:"clicks_7d" db:"clicks_7d"`
	Clicks30d          int        `json:"clicks_30d" db:"clicks_30d"`
	Clicks90d          int        `json:"clicks_90d" db:"clicks_90d"`
	EngagementVelocity float64    `json:"engagement_velocity" db:"engagement_velocity"`
	PropensityToBuy    *float64   `json:"propensity_to_buy,omitempty" db:"propensity_to_buy"`
	NextPurchaseDays   *int       `json:"next_purchase_days,omitempty" db:"next_purchase_days"`
	CalculatedAt       time.Time  `json:"calculated_at" db:"calculated_at"`
}

// ==========================================
// CONSENT & SUPPRESSION
// ==========================================

// ConsentType represents types of consent
type ConsentType string

const (
	ConsentMarketingEmail    ConsentType = "marketing_email"
	ConsentTransactionalEmail ConsentType = "transactional_email"
	ConsentTracking          ConsentType = "tracking"
	ConsentProfiling         ConsentType = "profiling"
	ConsentDataProcessing    ConsentType = "data_processing"
	ConsentThirdPartySharing ConsentType = "third_party_sharing"
)

// ConsentRecord represents a consent record
type ConsentRecord struct {
	ID             uuid.UUID   `json:"id" db:"id"`
	OrganizationID uuid.UUID   `json:"organization_id" db:"organization_id"`
	SubscriberID   *uuid.UUID  `json:"subscriber_id,omitempty" db:"subscriber_id"`
	Email          string      `json:"email" db:"email"`
	EmailHash      string      `json:"email_hash" db:"email_hash"`
	ConsentType    ConsentType `json:"consent_type" db:"consent_type"`
	Status         string      `json:"status" db:"status"` // granted, denied, withdrawn
	LegalBasis     string      `json:"legal_basis,omitempty" db:"legal_basis"`
	ConsentText    string      `json:"consent_text,omitempty" db:"consent_text"`
	ConsentVersion string      `json:"consent_version,omitempty" db:"consent_version"`
	Source         string      `json:"source,omitempty" db:"source"`
	IPAddress      string      `json:"ip_address,omitempty" db:"ip_address"`
	UserAgent      string      `json:"user_agent,omitempty" db:"user_agent"`
	ConsentedAt    time.Time   `json:"consented_at" db:"consented_at"`
	ExpiresAt      *time.Time  `json:"expires_at,omitempty" db:"expires_at"`
	WithdrawnAt    *time.Time  `json:"withdrawn_at,omitempty" db:"withdrawn_at"`
	CreatedAt      time.Time   `json:"created_at" db:"created_at"`
}

// SuppressionReason represents reasons for suppression
type SuppressionReason string

const (
	SuppressionUnsubscribe SuppressionReason = "unsubscribe"
	SuppressionComplaint   SuppressionReason = "complaint"
	SuppressionHardBounce  SuppressionReason = "hard_bounce"
	SuppressionManual      SuppressionReason = "manual"
	SuppressionGDPR        SuppressionReason = "gdpr_request"
	SuppressionCCPA        SuppressionReason = "ccpa_request"
	SuppressionLegal       SuppressionReason = "legal"
	SuppressionCompetitor  SuppressionReason = "competitor"
)

// SuppressionRecord represents a suppressed email
type SuppressionRecord struct {
	ID             uuid.UUID         `json:"id" db:"id"`
	OrganizationID uuid.UUID         `json:"organization_id" db:"organization_id"`
	Email          string            `json:"email" db:"email"`
	EmailHash      string            `json:"email_hash" db:"email_hash"`
	Reason         SuppressionReason `json:"reason" db:"reason"`
	Scope          string            `json:"scope" db:"scope"` // all, marketing, transactional
	Notes          string            `json:"notes,omitempty" db:"notes"`
	Source         string            `json:"source,omitempty" db:"source"`
	OriginalListID *uuid.UUID        `json:"original_list_id,omitempty" db:"original_list_id"`
	SuppressedAt   time.Time         `json:"suppressed_at" db:"suppressed_at"`
	ExpiresAt      *time.Time        `json:"expires_at,omitempty" db:"expires_at"`
	CreatedAt      time.Time         `json:"created_at" db:"created_at"`
}
