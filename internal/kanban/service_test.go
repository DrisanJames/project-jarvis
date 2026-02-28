package kanban

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestNewService_ConfigDefaults tests that NewService sets default config values
func TestNewService_ConfigDefaults(t *testing.T) {
	// Test with empty config - verify defaults are set
	config := Config{} // Empty config
	
	service := NewService(nil, config)

	// Should use default values
	assert.Equal(t, 20, service.config.MaxActiveTasks)
	assert.Equal(t, 3, service.config.MaxNewTasksPerRun)
	assert.Equal(t, time.Hour, service.config.AIRunInterval)
	assert.Equal(t, 24*time.Hour, service.config.DueSoonThreshold)
}

func TestNewService_WithProvidedConfig(t *testing.T) {
	config := Config{
		MaxActiveTasks:    30,
		MaxNewTasksPerRun: 5,
		AIRunInterval:     2 * time.Hour,
		DueSoonThreshold:  48 * time.Hour,
	}
	
	service := NewService(nil, config)

	assert.Equal(t, 30, service.config.MaxActiveTasks)
	assert.Equal(t, 5, service.config.MaxNewTasksPerRun)
	assert.Equal(t, 2*time.Hour, service.config.AIRunInterval)
	assert.Equal(t, 48*time.Hour, service.config.DueSoonThreshold)
}

// TestService_CountActiveTasks tests the active task counting logic
func TestService_CountActiveTasks(t *testing.T) {
	service := &Service{
		config: DefaultConfig(),
	}

	board := &KanbanBoard{
		Columns: []Column{
			{ID: ColumnBacklog, Cards: []Card{{ID: "1"}, {ID: "2"}}},
			{ID: ColumnTodo, Cards: []Card{{ID: "3"}}},
			{ID: ColumnInProgress, Cards: []Card{{ID: "4"}, {ID: "5"}}},
			{ID: ColumnDone, Cards: []Card{{ID: "6"}, {ID: "7"}, {ID: "8"}}},
		},
	}

	count := service.countActiveTasks(board)

	// Should count all except done: 2 + 1 + 2 = 5
	assert.Equal(t, 5, count)
}

func TestService_CountActiveTasks_EmptyBoard(t *testing.T) {
	service := &Service{
		config: DefaultConfig(),
	}

	board := NewDefaultBoard()
	count := service.countActiveTasks(board)

	assert.Equal(t, 0, count)
}

func TestService_GetConfig(t *testing.T) {
	config := Config{
		MaxActiveTasks:    25,
		MaxNewTasksPerRun: 5,
		AIRunInterval:     2 * time.Hour,
		DueSoonThreshold:  48 * time.Hour,
	}
	service := &Service{config: config}

	result := service.GetConfig()

	assert.Equal(t, 25, result.MaxActiveTasks)
	assert.Equal(t, 5, result.MaxNewTasksPerRun)
	assert.Equal(t, 2*time.Hour, result.AIRunInterval)
}

func TestService_InvalidateCache(t *testing.T) {
	service := &Service{
		cachedBoard: NewDefaultBoard(),
	}

	assert.NotNil(t, service.cachedBoard)

	service.InvalidateCache()

	assert.Nil(t, service.cachedBoard)
}
