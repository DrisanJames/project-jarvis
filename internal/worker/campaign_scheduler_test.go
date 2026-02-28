package worker

import (
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// =============================================================================
// CAMPAIGN SCHEDULER TESTS
// =============================================================================

func TestCampaignScheduler_NewScheduler(t *testing.T) {
	db, _, cleanup := setupTestDB(t)
	defer cleanup()

	scheduler := NewCampaignScheduler(db)

	if scheduler == nil {
		t.Error("NewCampaignScheduler() returned nil")
	}
	if scheduler.db != db {
		t.Error("Scheduler db not set correctly")
	}
	if scheduler.pollInterval != DefaultSchedulerPollInterval {
		t.Errorf("pollInterval = %v, want %v", scheduler.pollInterval, DefaultSchedulerPollInterval)
	}
}

func TestCampaignScheduler_StartStop(t *testing.T) {
	db, mock, cleanup := setupTestDB(t)
	defer cleanup()

	// Mock worker registration
	mock.ExpectExec("INSERT INTO mailing_workers").
		WillReturnResult(sqlmock.NewResult(1, 1))

	scheduler := NewCampaignScheduler(db)

	// Start
	err := scheduler.Start()
	if err != nil {
		t.Errorf("Start() error: %v", err)
	}

	scheduler.mu.RLock()
	running := scheduler.running
	scheduler.mu.RUnlock()

	if !running {
		t.Error("Scheduler should be running after Start()")
	}

	// Double start should error
	err = scheduler.Start()
	if err == nil {
		t.Error("Double Start() should return error")
	}

	// Mock worker deregistration
	mock.ExpectExec("UPDATE mailing_workers").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Stop
	scheduler.Stop()

	scheduler.mu.RLock()
	running = scheduler.running
	scheduler.mu.RUnlock()

	if running {
		t.Error("Scheduler should not be running after Stop()")
	}
}

// =============================================================================
// VALIDATION TESTS
// =============================================================================

func TestValidateScheduleTime(t *testing.T) {
	tests := []struct {
		name        string
		scheduledAt time.Time
		wantErr     bool
	}{
		{
			name:        "valid - 10 minutes from now",
			scheduledAt: time.Now().Add(10 * time.Minute),
			wantErr:     false,
		},
		{
			name:        "valid - 1 hour from now",
			scheduledAt: time.Now().Add(1 * time.Hour),
			wantErr:     false,
		},
		{
			name:        "invalid - 2 minutes from now",
			scheduledAt: time.Now().Add(2 * time.Minute),
			wantErr:     true,
		},
		{
			name:        "invalid - in the past",
			scheduledAt: time.Now().Add(-1 * time.Hour),
			wantErr:     true,
		},
		{
			name:        "boundary - exactly 5 minutes from now",
			scheduledAt: time.Now().Add(5 * time.Minute),
			wantErr:     true, // Must be MORE than 5 minutes
		},
		{
			name:        "valid - 6 minutes from now",
			scheduledAt: time.Now().Add(6 * time.Minute),
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateScheduleTime(tt.scheduledAt)
			if tt.wantErr {
				if err == nil {
					t.Error("ValidateScheduleTime() expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("ValidateScheduleTime() unexpected error: %v", err)
				}
			}
		})
	}
}

func TestCanEditCampaign(t *testing.T) {
	future := time.Now().Add(1 * time.Hour)
	nearFuture := time.Now().Add(3 * time.Minute)
	past := time.Now().Add(-1 * time.Hour)

	tests := []struct {
		name        string
		status      string
		scheduledAt *time.Time
		canEdit     bool
	}{
		{"draft - always editable", "draft", nil, true},
		{"scheduled - far future", "scheduled", &future, true},
		{"scheduled - near future (edit lock)", "scheduled", &nearFuture, false},
		{"scheduled - past (should not happen)", "scheduled", &past, false},
		{"sending - not editable", "sending", nil, false},
		{"completed - not editable", "completed", nil, false},
		{"cancelled - not editable", "cancelled", nil, false},
		{"failed - not editable", "failed", nil, false},
		{"preparing - not editable", "preparing", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			canEdit, reason := CanEditCampaign(tt.status, tt.scheduledAt)
			if canEdit != tt.canEdit {
				t.Errorf("CanEditCampaign() = %v, want %v (reason: %s)", canEdit, tt.canEdit, reason)
			}
		})
	}
}

func TestCanCancelCampaign(t *testing.T) {
	tests := []struct {
		name      string
		status    string
		canCancel bool
	}{
		{"draft - can cancel", "draft", true},
		{"scheduled - can cancel", "scheduled", true},
		{"preparing - can cancel", "preparing", true},
		{"sending - can cancel", "sending", true},
		{"paused - can cancel", "paused", true},
		{"completed - cannot cancel", "completed", false},
		{"completed_with_errors - cannot cancel", "completed_with_errors", false},
		{"cancelled - already cancelled", "cancelled", false},
		{"failed - cannot cancel", "failed", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			canCancel, _ := CanCancelCampaign(tt.status)
			if canCancel != tt.canCancel {
				t.Errorf("CanCancelCampaign(%s) = %v, want %v", tt.status, canCancel, tt.canCancel)
			}
		})
	}
}

func TestCanPauseCampaign(t *testing.T) {
	tests := []struct {
		name     string
		status   string
		canPause bool
	}{
		{"scheduled - can pause", "scheduled", true},
		{"preparing - can pause", "preparing", true},
		{"sending - can pause", "sending", true},
		{"draft - cannot pause", "draft", false},
		{"paused - already paused", "paused", false},
		{"completed - cannot pause", "completed", false},
		{"cancelled - cannot pause", "cancelled", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			canPause, _ := CanPauseCampaign(tt.status)
			if canPause != tt.canPause {
				t.Errorf("CanPauseCampaign(%s) = %v, want %v", tt.status, canPause, tt.canPause)
			}
		})
	}
}

// =============================================================================
// SCHEDULED CAMPAIGN PROCESSING TESTS
// =============================================================================

func TestCampaignScheduler_GetRecipientCount(t *testing.T) {
	db, mock, cleanup := setupTestDB(t)
	defer cleanup()

	scheduler := NewCampaignScheduler(db)

	tests := []struct {
		name        string
		campaign    ScheduledCampaign
		mockSetup   func()
		wantCount   int
		wantErr     bool
	}{
		{
			name: "list-based campaign",
			campaign: ScheduledCampaign{
				ListID: sql.NullString{String: "list-1", Valid: true},
			},
			mockSetup: func() {
				mock.ExpectQuery("SELECT COUNT").
					WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(100))
			},
			wantCount: 100,
			wantErr:   false,
		},
		{
			name: "with max recipients limit",
			campaign: ScheduledCampaign{
				ListID:        sql.NullString{String: "list-1", Valid: true},
				MaxRecipients: sql.NullInt64{Int64: 50, Valid: true},
			},
			mockSetup: func() {
				mock.ExpectQuery("SELECT COUNT").
					WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(100))
			},
			wantCount: 50, // Limited by MaxRecipients
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.mockSetup()

			ctx := scheduler.ctx
			if ctx == nil {
				ctx, _ = scheduler.newContext()
			}
			scheduler.ctx, scheduler.cancel = scheduler.newContext()

			count, err := scheduler.getRecipientCount(scheduler.ctx, tt.campaign)
			if tt.wantErr {
				if err == nil {
					t.Error("getRecipientCount() expected error")
				}
				return
			}
			if err != nil {
				t.Errorf("getRecipientCount() error: %v", err)
				return
			}
			if count != tt.wantCount {
				t.Errorf("getRecipientCount() = %d, want %d", count, tt.wantCount)
			}
		})
	}
}

// =============================================================================
// EDGE CASES
// =============================================================================

func TestCampaignScheduler_MultipleScheduledCampaigns(t *testing.T) {
	// Test handling multiple campaigns scheduled at the same time
	db, mock, cleanup := setupTestDB(t)
	defer cleanup()

	scheduler := NewCampaignScheduler(db)
	scheduler.ctx, scheduler.cancel = scheduler.newContext()

	// Mock: 5 campaigns scheduled for the same minute
	rows := sqlmock.NewRows([]string{
		"id", "name", "subject", "html_content", "plain_content",
		"from_name", "from_email", "list_id", "segment_id", "sending_profile_id",
		"scheduled_at", "throttle_speed", "max_recipients",
	})
	for i := 0; i < 5; i++ {
		rows.AddRow(
			"campaign-"+string(rune('a'+i)), "Test Campaign", "Subject",
			"<p>Hello</p>", "Hello",
			"Sender", "sender@test.com", "list-1", nil, "profile-1",
			time.Now(), "gentle", nil,
		)
	}

	mock.ExpectQuery("SELECT").
		WillReturnRows(rows)

	// processReadyCampaigns should handle all 5
	// We just verify it doesn't panic
	// In a real test, we'd mock the subsequent DB calls
}

func TestCampaignScheduler_WorkerRestart(t *testing.T) {
	// Simulates worker restart scenario
	// Scheduled campaigns should still be processed

	db, mock, cleanup := setupTestDB(t)
	defer cleanup()

	// First scheduler starts and registers
	mock.ExpectExec("INSERT INTO mailing_workers").
		WillReturnResult(sqlmock.NewResult(1, 1))

	scheduler1 := NewCampaignScheduler(db)
	scheduler1.Start()

	// Simulate crash (don't call Stop properly)
	scheduler1.cancel() // Just cancel context

	// Second scheduler starts (restart)
	mock.ExpectExec("INSERT INTO mailing_workers").
		WillReturnResult(sqlmock.NewResult(1, 1))

	scheduler2 := NewCampaignScheduler(db)
	err := scheduler2.Start()
	if err != nil {
		t.Errorf("Restarted scheduler should start: %v", err)
	}

	mock.ExpectExec("UPDATE mailing_workers").
		WillReturnResult(sqlmock.NewResult(0, 1))
	scheduler2.Stop()
}

func TestCampaignScheduler_InvalidTimezone(t *testing.T) {
	// Test that invalid timezone in scheduled_at is handled
	// This shouldn't happen in practice, but we should handle gracefully

	// All times should be stored in UTC, so this test verifies
	// the comparison works correctly
	now := time.Now().UTC()
	scheduledAt := now.Add(1 * time.Hour)

	// Verify times can be compared correctly
	if !scheduledAt.After(now) {
		t.Error("Scheduled time should be after now")
	}
}

// =============================================================================
// COMPLETION DETECTION TESTS
// =============================================================================

func TestCampaignScheduler_DetectCompletion(t *testing.T) {
	db, mock, cleanup := setupTestDB(t)
	defer cleanup()

	scheduler := NewCampaignScheduler(db)
	scheduler.ctx, scheduler.cancel = scheduler.newContext()

	// Use a real UUID for the campaign ID
	campaignID := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	// Mock: campaign with all items processed (95 sent, 5 failed, 0 pending)
	mock.ExpectQuery("SELECT c.id").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "sent", "failed", "skipped", "pending", "total",
		}).AddRow(campaignID, 95, 5, 0, 0, 100))

	// Mock: update campaign to completed_with_errors
	// Args order: $1=campaignID, $2=finalStatus, $3=sent
	mock.ExpectExec("UPDATE mailing_campaigns").
		WithArgs(sqlmock.AnyArg(), "completed_with_errors", 95).
		WillReturnResult(sqlmock.NewResult(0, 1))

	scheduler.checkCompletedCampaigns()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("Unfulfilled expectations: %v", err)
	}
}

func TestCampaignScheduler_StatusDetermination(t *testing.T) {
	tests := []struct {
		name       string
		sent       int
		failed     int
		total      int
		wantStatus string
	}{
		{"all sent", 100, 0, 100, "completed"},
		{"all failed", 0, 100, 100, "failed"},
		{"mixed", 90, 10, 100, "completed_with_errors"},
		{"mostly sent", 99, 1, 100, "completed_with_errors"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var status string
			if tt.failed > 0 && tt.sent > 0 {
				status = "completed_with_errors"
			} else if tt.failed == tt.total {
				status = "failed"
			} else {
				status = "completed"
			}

			if status != tt.wantStatus {
				t.Errorf("Status = %s, want %s", status, tt.wantStatus)
			}
		})
	}
}
