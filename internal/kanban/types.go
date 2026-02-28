package kanban

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// KanbanBoard represents the entire Kanban board stored as a single DynamoDB document
type KanbanBoard struct {
	PK           string    `dynamodbav:"PK" json:"pk"`              // "KANBAN#default"
	SK           string    `dynamodbav:"SK" json:"sk"`              // "BOARD"
	LastModified time.Time `dynamodbav:"LastModified" json:"last_modified"`
	Columns      []Column  `dynamodbav:"Columns" json:"columns"`

	// AI Rate Limiting
	LastAIRun       time.Time `dynamodbav:"LastAIRun" json:"last_ai_run"`
	ActiveTaskCount int       `dynamodbav:"ActiveTaskCount" json:"active_task_count"`
	MaxActiveTasks  int       `dynamodbav:"MaxActiveTasks" json:"max_active_tasks"`
}

// Column represents a Kanban column (e.g., "To Do", "In Progress")
type Column struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Order int    `json:"order"`
	Cards []Card `json:"cards"`
}

// Card represents a task card on the Kanban board
type Card struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Priority    string     `json:"priority"` // "normal", "high", "critical"
	DueDate     *time.Time `json:"due_date,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	CreatedBy   string     `json:"created_by"` // "ai" or "human"
	AIGenerated bool       `json:"ai_generated"`
	AIContext   *AIContext `json:"ai_context,omitempty"`

	// Deduplication - hash of issue type + entity for AI-generated tasks
	IssueFingerprint string `json:"issue_fingerprint,omitempty"`

	Labels []string `json:"labels"`
	Order  int      `json:"order"`
}

// AIContext provides context for AI-generated tasks
type AIContext struct {
	Source      string            `json:"source"`      // "deliverability", "revenue", "data_pipeline", "campaign"
	Reasoning   string            `json:"reasoning"`   // Why AI created this task
	DataPoints  map[string]string `json:"data_points"` // Supporting metrics
	Severity    string            `json:"severity"`    // Maps to priority
	EntityType  string            `json:"entity_type"` // "isp", "offer", "property", "data_set"
	EntityID    string            `json:"entity_id"`   // Specific entity identifier
	GeneratedAt time.Time         `json:"generated_at"`
}

// ArchivedTasks stores completed tasks grouped by month for velocity tracking
type ArchivedTasks struct {
	PK    string `dynamodbav:"PK" json:"pk"` // "KANBAN#archive"
	SK    string `dynamodbav:"SK" json:"sk"` // "2026-01" (year-month)
	Month string `dynamodbav:"Month" json:"month"`
	Tasks []ArchivedCard `dynamodbav:"Tasks" json:"tasks"`

	// Velocity Metrics (calculated when report is generated)
	TotalCompleted    int                      `dynamodbav:"TotalCompleted" json:"total_completed"`
	AvgCompletionTime float64                  `dynamodbav:"AvgCompletionTime" json:"avg_completion_hours"`
	ByPriority        map[string]VelocityStats `dynamodbav:"ByPriority" json:"by_priority"`
	BySource          map[string]VelocityStats `dynamodbav:"BySource" json:"by_source"`
	GeneratedAt       time.Time                `dynamodbav:"GeneratedAt" json:"generated_at"`
}

// ArchivedCard extends Card with archival metadata
type ArchivedCard struct {
	Card
	ArchivedAt time.Time `json:"archived_at"`
	Velocity   float64   `json:"velocity_hours"` // Hours from created to completed
}

// VelocityStats holds velocity statistics for a category
type VelocityStats struct {
	Count             int     `json:"count"`
	AvgCompletionTime float64 `json:"avg_completion_hours"`
	MinTime           float64 `json:"min_hours"`
	MaxTime           float64 `json:"max_hours"`
}

// ActiveIssues tracks active issue fingerprints for deduplication
type ActiveIssues struct {
	PK           string            `dynamodbav:"PK" json:"pk"`           // "KANBAN#issues"
	SK           string            `dynamodbav:"SK" json:"sk"`           // "ACTIVE"
	Fingerprints map[string]string `dynamodbav:"Fingerprints" json:"fingerprints"` // fingerprint -> cardID
	LastUpdated  time.Time         `dynamodbav:"LastUpdated" json:"last_updated"`
}

// VelocityReport is the monthly velocity report
type VelocityReport struct {
	Month             string                   `json:"month"`
	TotalCompleted    int                      `json:"total_completed"`
	TotalAIGenerated  int                      `json:"total_ai_generated"`
	TotalHumanCreated int                      `json:"total_human_created"`
	AvgCompletionTime float64                  `json:"avg_completion_hours"`
	ByPriority        map[string]VelocityStats `json:"by_priority"`
	BySource          map[string]VelocityStats `json:"by_source"`

	// Insights
	FastestCategory    string  `json:"fastest_category"`
	SlowestCategory    string  `json:"slowest_category"`
	AIGeneratedPercent float64 `json:"ai_generated_percent"`
	GeneratedAt        time.Time `json:"generated_at"`
}

// MoveCardRequest represents a request to move a card
type MoveCardRequest struct {
	CardID     string `json:"card_id"`
	FromColumn string `json:"from_column"`
	ToColumn   string `json:"to_column"`
	NewOrder   int    `json:"new_order"`
}

// CreateCardRequest represents a request to create a new card
type CreateCardRequest struct {
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Priority    string     `json:"priority"`
	DueDate     *time.Time `json:"due_date,omitempty"`
	ColumnID    string     `json:"column_id"`
	Labels      []string   `json:"labels,omitempty"`
}

// UpdateCardRequest represents a request to update a card
type UpdateCardRequest struct {
	Title       *string    `json:"title,omitempty"`
	Description *string    `json:"description,omitempty"`
	Priority    *string    `json:"priority,omitempty"`
	DueDate     *time.Time `json:"due_date,omitempty"`
	Labels      []string   `json:"labels,omitempty"`
}

// DueTasksResponse returns tasks that are due or overdue
type DueTasksResponse struct {
	Overdue  []Card `json:"overdue"`
	DueToday []Card `json:"due_today"`
	DueSoon  []Card `json:"due_soon"` // Within 24 hours
}

// AIAnalysisResult represents the result of AI ecosystem analysis
type AIAnalysisResult struct {
	NewTasks      []Card    `json:"new_tasks"`
	SkippedCount  int       `json:"skipped_count"`  // Duplicates skipped
	RateLimited   bool      `json:"rate_limited"`   // True if hit rate limit
	AnalyzedAt    time.Time `json:"analyzed_at"`
	NextRunAfter  time.Time `json:"next_run_after"`
}

// Config holds Kanban service configuration
type Config struct {
	Enabled          bool          `yaml:"enabled"`
	MaxActiveTasks   int           `yaml:"max_active_tasks"`   // Max tasks before AI stops creating (default: 20)
	MaxNewTasksPerRun int          `yaml:"max_new_tasks_per_run"` // Max AI tasks per hourly run (default: 3)
	AIRunInterval    time.Duration `yaml:"ai_run_interval"`    // How often AI runs (default: 1h)
	DueSoonThreshold time.Duration `yaml:"due_soon_threshold"` // What counts as "due soon" (default: 24h)
}

// DefaultConfig returns the default Kanban configuration
func DefaultConfig() Config {
	return Config{
		Enabled:           true,
		MaxActiveTasks:    20,
		MaxNewTasksPerRun: 3,
		AIRunInterval:     1 * time.Hour,
		DueSoonThreshold:  24 * time.Hour,
	}
}

// GetDefaultColumns returns the default Kanban columns
func GetDefaultColumns() []Column {
	return []Column{
		{ID: "backlog", Title: "Backlog", Order: 0, Cards: []Card{}},
		{ID: "todo", Title: "To Do", Order: 1, Cards: []Card{}},
		{ID: "in-progress", Title: "In Progress", Order: 2, Cards: []Card{}},
		{ID: "review", Title: "Review", Order: 3, Cards: []Card{}},
		{ID: "done", Title: "Done", Order: 4, Cards: []Card{}},
	}
}

// NewDefaultBoard creates a new board with default settings
func NewDefaultBoard() *KanbanBoard {
	return &KanbanBoard{
		PK:              "KANBAN#default",
		SK:              "BOARD",
		Columns:         GetDefaultColumns(),
		MaxActiveTasks:  20,
		ActiveTaskCount: 0,
		LastModified:    time.Now(),
	}
}

// GenerateFingerprint creates a unique fingerprint for deduplication
func GenerateFingerprint(source, entityType, entityID string) string {
	input := fmt.Sprintf("%s:%s:%s", source, entityType, entityID)
	hash := sha256.Sum256([]byte(input))
	return hex.EncodeToString(hash[:8]) // First 8 bytes = 16 hex chars
}

// Priority constants
const (
	PriorityNormal   = "normal"
	PriorityHigh     = "high"
	PriorityCritical = "critical"
)

// AI Source constants
const (
	SourceDeliverability = "deliverability"
	SourceRevenue        = "revenue"
	SourceDataPipeline   = "data_pipeline"
)

// Column ID constants
const (
	ColumnBacklog    = "backlog"
	ColumnTodo       = "todo"
	ColumnInProgress = "in-progress"
	ColumnReview     = "review"
	ColumnDone       = "done"
)

// CreatedBy constants
const (
	CreatedByAI    = "ai"
	CreatedByHuman = "human"
)
