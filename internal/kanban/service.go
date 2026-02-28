package kanban

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Service provides business logic for the Kanban board
type Service struct {
	client *Client
	config Config
	mu     sync.RWMutex

	// Cached board for fast reads
	cachedBoard *KanbanBoard
	cacheTime   time.Time
	cacheTTL    time.Duration
}

// NewService creates a new Kanban service
func NewService(client *Client, config Config) *Service {
	if config.MaxActiveTasks == 0 {
		config.MaxActiveTasks = 20
	}
	if config.MaxNewTasksPerRun == 0 {
		config.MaxNewTasksPerRun = 3
	}
	if config.AIRunInterval == 0 {
		config.AIRunInterval = 1 * time.Hour
	}
	if config.DueSoonThreshold == 0 {
		config.DueSoonThreshold = 24 * time.Hour
	}

	return &Service{
		client:   client,
		config:   config,
		cacheTTL: 5 * time.Second, // Short cache for responsiveness
	}
}

// GetBoard retrieves the current Kanban board
func (s *Service) GetBoard(ctx context.Context) (*KanbanBoard, error) {
	s.mu.RLock()
	if s.cachedBoard != nil && time.Since(s.cacheTime) < s.cacheTTL {
		board := s.cachedBoard
		s.mu.RUnlock()
		return board, nil
	}
	s.mu.RUnlock()

	board, err := s.client.GetBoard(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.cachedBoard = board
	s.cacheTime = time.Now()
	s.mu.Unlock()

	return board, nil
}

// UpdateBoard updates the entire board (used for drag-drop operations)
func (s *Service) UpdateBoard(ctx context.Context, board *KanbanBoard) error {
	// Recalculate active task count
	board.ActiveTaskCount = s.countActiveTasks(board)

	if err := s.client.SaveBoard(ctx, board); err != nil {
		return err
	}

	s.mu.Lock()
	s.cachedBoard = board
	s.cacheTime = time.Now()
	s.mu.Unlock()

	return nil
}

// CreateCard creates a new card in the specified column
func (s *Service) CreateCard(ctx context.Context, req CreateCardRequest) (*Card, error) {
	board, err := s.GetBoard(ctx)
	if err != nil {
		return nil, err
	}

	card := Card{
		ID:          uuid.New().String(),
		Title:       req.Title,
		Description: req.Description,
		Priority:    req.Priority,
		DueDate:     req.DueDate,
		CreatedAt:   time.Now(),
		CreatedBy:   CreatedByHuman,
		AIGenerated: false,
		Labels:      req.Labels,
	}

	if card.Priority == "" {
		card.Priority = PriorityNormal
	}

	// Find column and add card
	columnID := req.ColumnID
	if columnID == "" {
		columnID = ColumnBacklog
	}

	found := false
	for i, col := range board.Columns {
		if col.ID == columnID {
			card.Order = len(col.Cards)
			board.Columns[i].Cards = append(board.Columns[i].Cards, card)
			found = true
			break
		}
	}

	if !found {
		return nil, fmt.Errorf("column not found: %s", columnID)
	}

	if err := s.UpdateBoard(ctx, board); err != nil {
		return nil, err
	}

	return &card, nil
}

// UpdateCard updates an existing card
func (s *Service) UpdateCard(ctx context.Context, cardID string, req UpdateCardRequest) (*Card, error) {
	board, err := s.GetBoard(ctx)
	if err != nil {
		return nil, err
	}

	var updatedCard *Card
	for i, col := range board.Columns {
		for j, card := range col.Cards {
			if card.ID == cardID {
				if req.Title != nil {
					board.Columns[i].Cards[j].Title = *req.Title
				}
				if req.Description != nil {
					board.Columns[i].Cards[j].Description = *req.Description
				}
				if req.Priority != nil {
					board.Columns[i].Cards[j].Priority = *req.Priority
				}
				if req.DueDate != nil {
					board.Columns[i].Cards[j].DueDate = req.DueDate
				}
				if req.Labels != nil {
					board.Columns[i].Cards[j].Labels = req.Labels
				}
				updatedCard = &board.Columns[i].Cards[j]
				break
			}
		}
		if updatedCard != nil {
			break
		}
	}

	if updatedCard == nil {
		return nil, fmt.Errorf("card not found: %s", cardID)
	}

	if err := s.UpdateBoard(ctx, board); err != nil {
		return nil, err
	}

	return updatedCard, nil
}

// MoveCard moves a card between columns or reorders within a column
func (s *Service) MoveCard(ctx context.Context, req MoveCardRequest) error {
	board, err := s.GetBoard(ctx)
	if err != nil {
		return err
	}

	// Find and remove card from source column
	var card *Card
	for i, col := range board.Columns {
		if col.ID == req.FromColumn {
			for j, c := range col.Cards {
				if c.ID == req.CardID {
					card = &c
					board.Columns[i].Cards = append(col.Cards[:j], col.Cards[j+1:]...)
					break
				}
			}
			break
		}
	}

	if card == nil {
		return fmt.Errorf("card not found: %s", req.CardID)
	}

	// If moving to "done" column, set completed time
	if req.ToColumn == ColumnDone && card.CompletedAt == nil {
		now := time.Now()
		card.CompletedAt = &now
	}
	// If moving out of "done" column, clear completed time
	if req.FromColumn == ColumnDone && req.ToColumn != ColumnDone {
		card.CompletedAt = nil
	}

	// Add card to destination column at specified position
	for i, col := range board.Columns {
		if col.ID == req.ToColumn {
			card.Order = req.NewOrder

			// Insert at position
			if req.NewOrder >= len(col.Cards) {
				board.Columns[i].Cards = append(col.Cards, *card)
			} else {
				cards := make([]Card, 0, len(col.Cards)+1)
				cards = append(cards, col.Cards[:req.NewOrder]...)
				cards = append(cards, *card)
				cards = append(cards, col.Cards[req.NewOrder:]...)
				board.Columns[i].Cards = cards
			}

			// Reorder all cards in column
			for j := range board.Columns[i].Cards {
				board.Columns[i].Cards[j].Order = j
			}
			break
		}
	}

	return s.UpdateBoard(ctx, board)
}

// CompleteCard marks a card as complete and moves it to Done
func (s *Service) CompleteCard(ctx context.Context, cardID string) error {
	board, err := s.GetBoard(ctx)
	if err != nil {
		return err
	}

	// Find the card and its current column
	var card *Card
	var fromColumn string
	for i, col := range board.Columns {
		for j, c := range col.Cards {
			if c.ID == cardID {
				card = &board.Columns[i].Cards[j]
				fromColumn = col.ID
				break
			}
		}
		if card != nil {
			break
		}
	}

	if card == nil {
		return fmt.Errorf("card not found: %s", cardID)
	}

	// If already in done, just update completed time
	if fromColumn == ColumnDone {
		if card.CompletedAt == nil {
			now := time.Now()
			card.CompletedAt = &now
		}
		return s.UpdateBoard(ctx, board)
	}

	// Move to done column
	return s.MoveCard(ctx, MoveCardRequest{
		CardID:     cardID,
		FromColumn: fromColumn,
		ToColumn:   ColumnDone,
		NewOrder:   0, // Add to top of done column
	})
}

// DeleteCard removes a card from the board
func (s *Service) DeleteCard(ctx context.Context, cardID string) error {
	board, err := s.GetBoard(ctx)
	if err != nil {
		return err
	}

	var deletedCard *Card
	for i, col := range board.Columns {
		for j, card := range col.Cards {
			if card.ID == cardID {
				deletedCard = &card
				board.Columns[i].Cards = append(col.Cards[:j], col.Cards[j+1:]...)
				break
			}
		}
		if deletedCard != nil {
			break
		}
	}

	if deletedCard == nil {
		return fmt.Errorf("card not found: %s", cardID)
	}

	// Remove fingerprint from active issues if AI-generated
	if deletedCard.IssueFingerprint != "" {
		issues, _ := s.client.GetActiveIssues(ctx)
		if issues != nil {
			delete(issues.Fingerprints, deletedCard.IssueFingerprint)
			s.client.SaveActiveIssues(ctx, issues)
		}
	}

	return s.UpdateBoard(ctx, board)
}

// GetDueTasks returns tasks that are due or overdue
func (s *Service) GetDueTasks(ctx context.Context) (*DueTasksResponse, error) {
	board, err := s.GetBoard(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, now.Location())
	soon := now.Add(s.config.DueSoonThreshold)

	response := &DueTasksResponse{
		Overdue:  []Card{},
		DueToday: []Card{},
		DueSoon:  []Card{},
	}

	// Check all non-done columns
	for _, col := range board.Columns {
		if col.ID == ColumnDone {
			continue
		}

		for _, card := range col.Cards {
			if card.DueDate == nil {
				continue
			}

			dueDate := *card.DueDate
			if dueDate.Before(now) {
				response.Overdue = append(response.Overdue, card)
			} else if dueDate.Before(today) || dueDate.Equal(today) {
				response.DueToday = append(response.DueToday, card)
			} else if dueDate.Before(soon) {
				response.DueSoon = append(response.DueSoon, card)
			}
		}
	}

	// Sort by due date
	sortByDueDate := func(cards []Card) {
		sort.Slice(cards, func(i, j int) bool {
			if cards[i].DueDate == nil {
				return false
			}
			if cards[j].DueDate == nil {
				return true
			}
			return cards[i].DueDate.Before(*cards[j].DueDate)
		})
	}

	sortByDueDate(response.Overdue)
	sortByDueDate(response.DueToday)
	sortByDueDate(response.DueSoon)

	return response, nil
}

// AddAIGeneratedCard adds an AI-generated card to the board
func (s *Service) AddAIGeneratedCard(ctx context.Context, card Card) error {
	board, err := s.GetBoard(ctx)
	if err != nil {
		return err
	}

	// Check rate limits
	if board.ActiveTaskCount >= board.MaxActiveTasks {
		return fmt.Errorf("max active tasks reached: %d", board.MaxActiveTasks)
	}

	// Check deduplication
	if card.IssueFingerprint != "" {
		issues, _ := s.client.GetActiveIssues(ctx)
		if issues != nil {
			if _, exists := issues.Fingerprints[card.IssueFingerprint]; exists {
				return fmt.Errorf("duplicate task: fingerprint already exists")
			}
		}
	}

	// Set AI-specific fields
	card.ID = uuid.New().String()
	card.CreatedAt = time.Now()
	card.CreatedBy = CreatedByAI
	card.AIGenerated = true

	// Determine target column based on priority
	targetColumn := ColumnBacklog
	if card.Priority == PriorityCritical {
		targetColumn = ColumnTodo // Critical tasks go straight to To Do
	}

	// Add to column
	for i, col := range board.Columns {
		if col.ID == targetColumn {
			card.Order = 0 // Add to top
			// Shift existing cards down
			for j := range board.Columns[i].Cards {
				board.Columns[i].Cards[j].Order++
			}
			board.Columns[i].Cards = append([]Card{card}, board.Columns[i].Cards...)
			break
		}
	}

	// Update active issues for deduplication
	if card.IssueFingerprint != "" {
		issues, _ := s.client.GetActiveIssues(ctx)
		if issues == nil {
			issues = &ActiveIssues{
				PK:           "KANBAN#issues",
				SK:           "ACTIVE",
				Fingerprints: make(map[string]string),
			}
		}
		issues.Fingerprints[card.IssueFingerprint] = card.ID
		s.client.SaveActiveIssues(ctx, issues)
	}

	return s.UpdateBoard(ctx, board)
}

// UpdateAIRunTime updates the last AI run time
func (s *Service) UpdateAIRunTime(ctx context.Context) error {
	board, err := s.GetBoard(ctx)
	if err != nil {
		return err
	}

	board.LastAIRun = time.Now()
	return s.UpdateBoard(ctx, board)
}

// CanRunAIAnalysis checks if AI analysis can run (rate limiting)
func (s *Service) CanRunAIAnalysis(ctx context.Context) (bool, time.Duration, error) {
	return s.CanRunAIAnalysisWithInterval(ctx, s.config.AIRunInterval)
}

// CanRunAIAnalysisWithInterval checks if AI analysis can run with a custom interval
func (s *Service) CanRunAIAnalysisWithInterval(ctx context.Context, interval time.Duration) (bool, time.Duration, error) {
	board, err := s.GetBoard(ctx)
	if err != nil {
		return false, 0, err
	}

	// Check time since last run (skip if interval is 0 for manual triggers)
	if interval > 0 {
		timeSinceLastRun := time.Since(board.LastAIRun)
		if timeSinceLastRun < interval {
			waitTime := interval - timeSinceLastRun
			return false, waitTime, nil
		}
	}

	// Check active task count
	if board.ActiveTaskCount >= board.MaxActiveTasks {
		return false, 0, nil
	}

	return true, 0, nil
}

// GetActiveIssues returns the active issues fingerprint map
func (s *Service) GetActiveIssues(ctx context.Context) (map[string]string, error) {
	issues, err := s.client.GetActiveIssues(ctx)
	if err != nil {
		return nil, err
	}
	return issues.Fingerprints, nil
}

// RemoveFingerprint removes a fingerprint from active issues (called when task is completed/archived)
func (s *Service) RemoveFingerprint(ctx context.Context, fingerprint string) error {
	issues, err := s.client.GetActiveIssues(ctx)
	if err != nil {
		return err
	}

	delete(issues.Fingerprints, fingerprint)
	return s.client.SaveActiveIssues(ctx, issues)
}

// GetConfig returns the service configuration
func (s *Service) GetConfig() Config {
	return s.config
}

// countActiveTasks counts non-completed tasks across all columns
func (s *Service) countActiveTasks(board *KanbanBoard) int {
	count := 0
	for _, col := range board.Columns {
		if col.ID == ColumnDone {
			continue
		}
		count += len(col.Cards)
	}
	return count
}

// InvalidateCache clears the cached board
func (s *Service) InvalidateCache() {
	s.mu.Lock()
	s.cachedBoard = nil
	s.mu.Unlock()
}

// GetCurrentVelocityStats returns velocity stats for the current month (in-progress)
func (s *Service) GetCurrentVelocityStats(ctx context.Context) (*VelocityReport, error) {
	board, err := s.GetBoard(ctx)
	if err != nil {
		return nil, err
	}

	currentMonth := time.Now().Format("2006-01")
	report := &VelocityReport{
		Month:      currentMonth,
		ByPriority: make(map[string]VelocityStats),
		BySource:   make(map[string]VelocityStats),
	}

	// Count completed tasks in Done column
	for _, col := range board.Columns {
		if col.ID == ColumnDone {
			for _, card := range col.Cards {
				if card.CompletedAt != nil && card.CompletedAt.Format("2006-01") == currentMonth {
					report.TotalCompleted++
					if card.AIGenerated {
						report.TotalAIGenerated++
					} else {
						report.TotalHumanCreated++
					}

					// Calculate velocity
					velocity := card.CompletedAt.Sub(card.CreatedAt).Hours()
					report.AvgCompletionTime = (report.AvgCompletionTime*float64(report.TotalCompleted-1) + velocity) / float64(report.TotalCompleted)
				}
			}
		}
	}

	if report.TotalCompleted > 0 {
		report.AIGeneratedPercent = float64(report.TotalAIGenerated) / float64(report.TotalCompleted) * 100
	}

	return report, nil
}

// Start begins background tasks (called from main)
func (s *Service) Start(ctx context.Context) {
	log.Println("Kanban service started")
	
	// Initial board load to ensure it exists
	_, err := s.GetBoard(ctx)
	if err != nil {
		log.Printf("Kanban: Error loading initial board: %v", err)
	}
}
