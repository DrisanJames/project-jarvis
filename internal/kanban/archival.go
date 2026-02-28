package kanban

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"
)

// ArchivalService handles archiving completed tasks and generating velocity reports
type ArchivalService struct {
	client  *Client
	service *Service
}

// NewArchivalService creates a new archival service
func NewArchivalService(client *Client, service *Service) *ArchivalService {
	return &ArchivalService{
		client:  client,
		service: service,
	}
}

// WeeklyCleanup archives completed tasks from the Done column
// Should be run weekly (e.g., Sunday at midnight)
func (a *ArchivalService) WeeklyCleanup(ctx context.Context) error {
	log.Println("Kanban: Starting weekly cleanup...")

	board, err := a.service.GetBoard(ctx)
	if err != nil {
		return fmt.Errorf("getting board: %w", err)
	}

	var toArchive []Card
	now := time.Now()

	// Find completed tasks in "Done" column
	for i, col := range board.Columns {
		if col.ID != ColumnDone {
			continue
		}

		var remaining []Card
		for _, card := range col.Cards {
			// Archive if completed (has CompletedAt set)
			if card.CompletedAt != nil {
				toArchive = append(toArchive, card)
			} else {
				remaining = append(remaining, card)
			}
		}
		board.Columns[i].Cards = remaining
	}

	if len(toArchive) == 0 {
		log.Println("Kanban: No completed tasks to archive")
		return nil
	}

	log.Printf("Kanban: Archiving %d completed tasks", len(toArchive))

	// Group by month and archive
	byMonth := make(map[string][]Card)
	for _, card := range toArchive {
		month := card.CompletedAt.Format("2006-01")
		byMonth[month] = append(byMonth[month], card)
	}

	for month, cards := range byMonth {
		if err := a.archiveCards(ctx, month, cards, now); err != nil {
			log.Printf("Kanban: Error archiving to %s: %v", month, err)
		}
	}

	// Remove fingerprints from active issues
	for _, card := range toArchive {
		if card.IssueFingerprint != "" {
			a.service.RemoveFingerprint(ctx, card.IssueFingerprint)
		}
	}

	// Update board (removes archived cards from Done column)
	if err := a.service.UpdateBoard(ctx, board); err != nil {
		return fmt.Errorf("updating board: %w", err)
	}

	log.Printf("Kanban: Weekly cleanup complete - %d tasks archived", len(toArchive))
	return nil
}

// archiveCards adds cards to the archive for a specific month
func (a *ArchivalService) archiveCards(ctx context.Context, month string, cards []Card, now time.Time) error {
	archive, err := a.client.GetArchive(ctx, month)
	if err != nil {
		return err
	}

	for _, card := range cards {
		velocity := 0.0
		if card.CompletedAt != nil {
			velocity = card.CompletedAt.Sub(card.CreatedAt).Hours()
		}

		archive.Tasks = append(archive.Tasks, ArchivedCard{
			Card:       card,
			ArchivedAt: now,
			Velocity:   velocity,
		})
	}

	archive.TotalCompleted = len(archive.Tasks)
	return a.client.SaveArchive(ctx, archive)
}

// GenerateMonthlyReport generates a velocity report for a specific month
// Should be run on the 1st of each month for the previous month
func (a *ArchivalService) GenerateMonthlyReport(ctx context.Context, month string) (*VelocityReport, error) {
	log.Printf("Kanban: Generating velocity report for %s", month)

	archive, err := a.client.GetArchive(ctx, month)
	if err != nil {
		return nil, fmt.Errorf("getting archive: %w", err)
	}

	if len(archive.Tasks) == 0 {
		log.Printf("Kanban: No archived tasks for %s", month)
		return &VelocityReport{
			Month:       month,
			GeneratedAt: time.Now(),
		}, nil
	}

	report := &VelocityReport{
		Month:       month,
		ByPriority:  make(map[string]VelocityStats),
		BySource:    make(map[string]VelocityStats),
		GeneratedAt: time.Now(),
	}

	// Collect all velocities
	var totalVelocity float64
	priorityVelocities := make(map[string][]float64)
	sourceVelocities := make(map[string][]float64)

	for _, task := range archive.Tasks {
		report.TotalCompleted++
		totalVelocity += task.Velocity

		if task.AIGenerated {
			report.TotalAIGenerated++
		} else {
			report.TotalHumanCreated++
		}

		// Group by priority
		priorityVelocities[task.Priority] = append(priorityVelocities[task.Priority], task.Velocity)

		// Group by AI source
		if task.AIContext != nil {
			sourceVelocities[task.AIContext.Source] = append(sourceVelocities[task.AIContext.Source], task.Velocity)
		} else {
			sourceVelocities["human"] = append(sourceVelocities["human"], task.Velocity)
		}
	}

	// Calculate overall average
	if report.TotalCompleted > 0 {
		report.AvgCompletionTime = totalVelocity / float64(report.TotalCompleted)
		report.AIGeneratedPercent = float64(report.TotalAIGenerated) / float64(report.TotalCompleted) * 100
	}

	// Calculate stats by priority
	for priority, velocities := range priorityVelocities {
		report.ByPriority[priority] = calculateVelocityStats(velocities)
	}

	// Calculate stats by source
	for source, velocities := range sourceVelocities {
		report.BySource[source] = calculateVelocityStats(velocities)
	}

	// Find fastest and slowest categories
	report.FastestCategory, report.SlowestCategory = findFastestSlowest(report.ByPriority)

	// Update archive with calculated metrics
	archive.AvgCompletionTime = report.AvgCompletionTime
	archive.ByPriority = report.ByPriority
	archive.BySource = report.BySource
	archive.GeneratedAt = time.Now()
	a.client.SaveArchive(ctx, archive)

	// Save report
	if err := a.client.SaveVelocityReport(ctx, report); err != nil {
		log.Printf("Kanban: Error saving velocity report: %v", err)
	}

	log.Printf("Kanban: Velocity report generated - %d tasks, avg %.1f hours", report.TotalCompleted, report.AvgCompletionTime)
	return report, nil
}

// GetVelocityReport retrieves a monthly velocity report
func (a *ArchivalService) GetVelocityReport(ctx context.Context, month string) (*VelocityReport, error) {
	return a.client.GetVelocityReport(ctx, month)
}

// GetAllReports retrieves all available velocity reports
func (a *ArchivalService) GetAllReports(ctx context.Context) ([]*VelocityReport, error) {
	months, err := a.client.ListArchiveMonths(ctx)
	if err != nil {
		return nil, err
	}

	var reports []*VelocityReport
	for _, month := range months {
		report, err := a.client.GetVelocityReport(ctx, month)
		if err != nil {
			continue
		}
		if report != nil {
			reports = append(reports, report)
		}
	}

	return reports, nil
}

// calculateVelocityStats calculates min, max, and average for a set of velocities
func calculateVelocityStats(velocities []float64) VelocityStats {
	if len(velocities) == 0 {
		return VelocityStats{}
	}

	var sum, min, max float64
	min = math.MaxFloat64
	max = 0

	for _, v := range velocities {
		sum += v
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}

	return VelocityStats{
		Count:             len(velocities),
		AvgCompletionTime: sum / float64(len(velocities)),
		MinTime:           min,
		MaxTime:           max,
	}
}

// findFastestSlowest finds the fastest and slowest priority categories
func findFastestSlowest(byPriority map[string]VelocityStats) (fastest, slowest string) {
	var fastestTime, slowestTime float64
	fastestTime = math.MaxFloat64
	slowestTime = 0

	for priority, stats := range byPriority {
		if stats.Count == 0 {
			continue
		}
		if stats.AvgCompletionTime < fastestTime {
			fastestTime = stats.AvgCompletionTime
			fastest = priority
		}
		if stats.AvgCompletionTime > slowestTime {
			slowestTime = stats.AvgCompletionTime
			slowest = priority
		}
	}

	return fastest, slowest
}

// Scheduler runs archival and reporting on schedule
type Scheduler struct {
	analyzer *AIAnalyzer
	archival *ArchivalService
	config   Config
	stopCh   chan struct{}
}

// NewScheduler creates a new scheduler
func NewScheduler(analyzer *AIAnalyzer, archival *ArchivalService, config Config) *Scheduler {
	return &Scheduler{
		analyzer: analyzer,
		archival: archival,
		config:   config,
		stopCh:   make(chan struct{}),
	}
}

// Start begins the scheduler background tasks
func (s *Scheduler) Start(ctx context.Context) {
	log.Println("Kanban: Scheduler started")

	// Hourly AI analysis
	go func() {
		// Initial delay to let collectors populate
		time.Sleep(5 * time.Minute)

		ticker := time.NewTicker(s.config.AIRunInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if s.analyzer != nil {
					result, err := s.analyzer.AnalyzeAndGenerateTasks(ctx)
					if err != nil {
						log.Printf("Kanban: AI analysis error: %v", err)
					} else if result != nil && len(result.NewTasks) > 0 {
						log.Printf("Kanban: AI created %d new tasks", len(result.NewTasks))
					}
				}
			case <-s.stopCh:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	// Daily check for weekly cleanup and monthly reports
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				now := time.Now()

				// Weekly cleanup on Sunday
				if now.Weekday() == time.Sunday {
					if err := s.archival.WeeklyCleanup(ctx); err != nil {
						log.Printf("Kanban: Weekly cleanup error: %v", err)
					}
				}

				// Monthly report on 1st of month
				if now.Day() == 1 {
					lastMonth := now.AddDate(0, -1, 0).Format("2006-01")
					if _, err := s.archival.GenerateMonthlyReport(ctx, lastMonth); err != nil {
						log.Printf("Kanban: Monthly report error: %v", err)
					}
				}
			case <-s.stopCh:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Stop stops the scheduler
func (s *Scheduler) Stop() {
	close(s.stopCh)
}
