package kanban

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfig(t *testing.T) {
	config := DefaultConfig()

	assert.True(t, config.Enabled)
	assert.Equal(t, 20, config.MaxActiveTasks)
	assert.Equal(t, 3, config.MaxNewTasksPerRun)
	assert.Equal(t, time.Hour, config.AIRunInterval)
	assert.Equal(t, 24*time.Hour, config.DueSoonThreshold)
}

func TestGetDefaultColumns(t *testing.T) {
	columns := GetDefaultColumns()

	require.Len(t, columns, 5)
	assert.Equal(t, ColumnBacklog, columns[0].ID)
	assert.Equal(t, ColumnTodo, columns[1].ID)
	assert.Equal(t, ColumnInProgress, columns[2].ID)
	assert.Equal(t, ColumnReview, columns[3].ID)
	assert.Equal(t, ColumnDone, columns[4].ID)

	// Verify order
	for i, col := range columns {
		assert.Equal(t, i, col.Order)
		assert.Empty(t, col.Cards)
	}
}

func TestNewDefaultBoard(t *testing.T) {
	board := NewDefaultBoard()

	require.NotNil(t, board)
	assert.Equal(t, "KANBAN#default", board.PK)
	assert.Equal(t, "BOARD", board.SK)
	assert.Len(t, board.Columns, 5)
	assert.Equal(t, 20, board.MaxActiveTasks)
	assert.Equal(t, 0, board.ActiveTaskCount)
	assert.False(t, board.LastModified.IsZero())
}

func TestGenerateFingerprint(t *testing.T) {
	tests := []struct {
		source     string
		entityType string
		entityID   string
	}{
		{"deliverability", "isp", "gmail"},
		{"revenue", "offer", "12345"},
		{"data_pipeline", "data_set", "GLB_BR"},
	}

	for _, tt := range tests {
		t.Run(tt.source+"-"+tt.entityType, func(t *testing.T) {
			fp := GenerateFingerprint(tt.source, tt.entityType, tt.entityID)

			assert.NotEmpty(t, fp)
			assert.Len(t, fp, 16) // 8 bytes = 16 hex chars

			// Same input should produce same output
			fp2 := GenerateFingerprint(tt.source, tt.entityType, tt.entityID)
			assert.Equal(t, fp, fp2)
		})
	}

	// Different inputs should produce different fingerprints
	fp1 := GenerateFingerprint("deliverability", "isp", "gmail")
	fp2 := GenerateFingerprint("deliverability", "isp", "yahoo")
	assert.NotEqual(t, fp1, fp2)
}

func TestConstants(t *testing.T) {
	// Priority constants
	assert.Equal(t, "normal", PriorityNormal)
	assert.Equal(t, "high", PriorityHigh)
	assert.Equal(t, "critical", PriorityCritical)

	// Source constants
	assert.Equal(t, "deliverability", SourceDeliverability)
	assert.Equal(t, "revenue", SourceRevenue)
	assert.Equal(t, "data_pipeline", SourceDataPipeline)

	// Column constants
	assert.Equal(t, "backlog", ColumnBacklog)
	assert.Equal(t, "todo", ColumnTodo)
	assert.Equal(t, "in-progress", ColumnInProgress)
	assert.Equal(t, "review", ColumnReview)
	assert.Equal(t, "done", ColumnDone)

	// CreatedBy constants
	assert.Equal(t, "ai", CreatedByAI)
	assert.Equal(t, "human", CreatedByHuman)
}

func TestKanbanBoard_Structure(t *testing.T) {
	board := &KanbanBoard{
		PK:              "KANBAN#test",
		SK:              "BOARD",
		LastModified:    time.Now(),
		Columns:         GetDefaultColumns(),
		LastAIRun:       time.Now(),
		ActiveTaskCount: 5,
		MaxActiveTasks:  20,
	}

	assert.Equal(t, "KANBAN#test", board.PK)
	assert.Equal(t, 5, board.ActiveTaskCount)
	assert.Equal(t, 20, board.MaxActiveTasks)
}

func TestColumn_Structure(t *testing.T) {
	col := Column{
		ID:    "test-col",
		Title: "Test Column",
		Order: 0,
		Cards: []Card{
			{ID: "card-1", Title: "Card 1"},
			{ID: "card-2", Title: "Card 2"},
		},
	}

	assert.Equal(t, "test-col", col.ID)
	assert.Equal(t, "Test Column", col.Title)
	assert.Len(t, col.Cards, 2)
}

func TestCard_Structure(t *testing.T) {
	now := time.Now()
	dueDate := now.Add(24 * time.Hour)
	completedAt := now.Add(48 * time.Hour)

	card := Card{
		ID:               "card-123",
		Title:            "Test Card",
		Description:      "Test Description",
		Priority:         PriorityHigh,
		DueDate:          &dueDate,
		CreatedAt:        now,
		CompletedAt:      &completedAt,
		CreatedBy:        CreatedByAI,
		AIGenerated:      true,
		IssueFingerprint: "fp-123",
		Labels:           []string{"urgent", "deliverability"},
		Order:            0,
		AIContext: &AIContext{
			Source:     SourceDeliverability,
			Reasoning:  "High complaint rate detected",
			Severity:   PriorityCritical,
			EntityType: "isp",
			EntityID:   "gmail",
		},
	}

	assert.Equal(t, "card-123", card.ID)
	assert.Equal(t, "Test Card", card.Title)
	assert.True(t, card.AIGenerated)
	assert.NotNil(t, card.AIContext)
	assert.Equal(t, SourceDeliverability, card.AIContext.Source)
}

func TestAIContext_Structure(t *testing.T) {
	ctx := &AIContext{
		Source:      SourceRevenue,
		Reasoning:   "Revenue dropped significantly",
		DataPoints:  map[string]string{"revenue_drop": "25%", "period": "7d"},
		Severity:    PriorityHigh,
		EntityType:  "offer",
		EntityID:    "offer-123",
		GeneratedAt: time.Now(),
	}

	assert.Equal(t, SourceRevenue, ctx.Source)
	assert.Contains(t, ctx.DataPoints, "revenue_drop")
	assert.Equal(t, "25%", ctx.DataPoints["revenue_drop"])
}

func TestMoveCardRequest_Structure(t *testing.T) {
	req := MoveCardRequest{
		CardID:     "card-123",
		FromColumn: ColumnTodo,
		ToColumn:   ColumnInProgress,
		NewOrder:   2,
	}

	assert.Equal(t, "card-123", req.CardID)
	assert.Equal(t, ColumnTodo, req.FromColumn)
	assert.Equal(t, ColumnInProgress, req.ToColumn)
	assert.Equal(t, 2, req.NewOrder)
}

func TestCreateCardRequest_Structure(t *testing.T) {
	dueDate := time.Now().Add(24 * time.Hour)
	req := CreateCardRequest{
		Title:       "New Task",
		Description: "Task Description",
		Priority:    PriorityHigh,
		DueDate:     &dueDate,
		ColumnID:    ColumnTodo,
		Labels:      []string{"bug", "urgent"},
	}

	assert.Equal(t, "New Task", req.Title)
	assert.Equal(t, ColumnTodo, req.ColumnID)
	assert.Len(t, req.Labels, 2)
}

func TestUpdateCardRequest_Structure(t *testing.T) {
	newTitle := "Updated Title"
	newDesc := "Updated Description"
	newPriority := PriorityCritical

	req := UpdateCardRequest{
		Title:       &newTitle,
		Description: &newDesc,
		Priority:    &newPriority,
		Labels:      []string{"updated"},
	}

	assert.Equal(t, "Updated Title", *req.Title)
	assert.Equal(t, "Updated Description", *req.Description)
	assert.Equal(t, PriorityCritical, *req.Priority)
}

func TestDueTasksResponse_Structure(t *testing.T) {
	resp := DueTasksResponse{
		Overdue:  []Card{{ID: "overdue-1"}},
		DueToday: []Card{{ID: "today-1"}, {ID: "today-2"}},
		DueSoon:  []Card{{ID: "soon-1"}},
	}

	assert.Len(t, resp.Overdue, 1)
	assert.Len(t, resp.DueToday, 2)
	assert.Len(t, resp.DueSoon, 1)
}

func TestAIAnalysisResult_Structure(t *testing.T) {
	result := AIAnalysisResult{
		NewTasks: []Card{
			{ID: "task-1", Title: "New AI Task"},
		},
		SkippedCount: 2,
		RateLimited:  false,
		AnalyzedAt:   time.Now(),
		NextRunAfter: time.Now().Add(time.Hour),
	}

	assert.Len(t, result.NewTasks, 1)
	assert.Equal(t, 2, result.SkippedCount)
	assert.False(t, result.RateLimited)
}

func TestVelocityReport_Structure(t *testing.T) {
	report := VelocityReport{
		Month:             "2026-01",
		TotalCompleted:    50,
		TotalAIGenerated:  30,
		TotalHumanCreated: 20,
		AvgCompletionTime: 24.5,
		ByPriority: map[string]VelocityStats{
			PriorityCritical: {Count: 10, AvgCompletionTime: 8.0},
			PriorityHigh:     {Count: 20, AvgCompletionTime: 16.0},
			PriorityNormal:   {Count: 20, AvgCompletionTime: 48.0},
		},
		BySource: map[string]VelocityStats{
			SourceDeliverability: {Count: 25, AvgCompletionTime: 12.0},
			SourceRevenue:        {Count: 15, AvgCompletionTime: 24.0},
		},
		FastestCategory:    PriorityCritical,
		SlowestCategory:    PriorityNormal,
		AIGeneratedPercent: 60.0,
		GeneratedAt:        time.Now(),
	}

	assert.Equal(t, "2026-01", report.Month)
	assert.Equal(t, 50, report.TotalCompleted)
	assert.Equal(t, 60.0, report.AIGeneratedPercent)
	assert.Contains(t, report.ByPriority, PriorityCritical)
}

func TestArchivedTasks_Structure(t *testing.T) {
	archive := ArchivedTasks{
		PK:    "KANBAN#archive",
		SK:    "2026-01",
		Month: "2026-01",
		Tasks: []ArchivedCard{
			{
				Card: Card{ID: "card-1", Title: "Archived Task"},
				ArchivedAt: time.Now(),
				Velocity:   48.5,
			},
		},
		TotalCompleted:    1,
		AvgCompletionTime: 48.5,
	}

	assert.Equal(t, "KANBAN#archive", archive.PK)
	assert.Len(t, archive.Tasks, 1)
	assert.Equal(t, 48.5, archive.Tasks[0].Velocity)
}

func TestActiveIssues_Structure(t *testing.T) {
	issues := ActiveIssues{
		PK:           "KANBAN#issues",
		SK:           "ACTIVE",
		Fingerprints: map[string]string{
			"fp-1": "card-1",
			"fp-2": "card-2",
		},
		LastUpdated: time.Now(),
	}

	assert.Equal(t, "KANBAN#issues", issues.PK)
	assert.Len(t, issues.Fingerprints, 2)
	assert.Equal(t, "card-1", issues.Fingerprints["fp-1"])
}

func TestVelocityStats_Structure(t *testing.T) {
	stats := VelocityStats{
		Count:             10,
		AvgCompletionTime: 24.5,
		MinTime:           4.0,
		MaxTime:           72.0,
	}

	assert.Equal(t, 10, stats.Count)
	assert.Equal(t, 24.5, stats.AvgCompletionTime)
	assert.Equal(t, 4.0, stats.MinTime)
	assert.Equal(t, 72.0, stats.MaxTime)
}
