package storage

import (
	"testing"
	"time"
)

func TestSuggestionStatus(t *testing.T) {
	tests := []struct {
		name   string
		status SuggestionStatus
		want   string
	}{
		{"pending status", SuggestionStatusPending, "pending"},
		{"resolved status", SuggestionStatusResolved, "resolved"},
		{"denied status", SuggestionStatusDenied, "denied"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.status) != tt.want {
				t.Errorf("SuggestionStatus = %v, want %v", tt.status, tt.want)
			}
		})
	}
}

func TestSuggestionStructure(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	
	suggestion := Suggestion{
		ID:                 "sug_123",
		SubmittedByEmail:   "user@example.com",
		SubmittedByName:    "Test User",
		Area:               "Dashboard",
		AreaContext:        "Main view",
		OriginalSuggestion: "Please add dark mode",
		Requirements:       "## Requirements\n- Add theme toggle",
		Status:             SuggestionStatusPending,
		ResolutionNotes:    "",
		CreatedAt:          now,
		UpdatedAt:          now,
	}

	// Verify all fields are set correctly
	if suggestion.ID != "sug_123" {
		t.Errorf("ID = %v, want %v", suggestion.ID, "sug_123")
	}
	if suggestion.SubmittedByEmail != "user@example.com" {
		t.Errorf("SubmittedByEmail = %v, want %v", suggestion.SubmittedByEmail, "user@example.com")
	}
	if suggestion.SubmittedByName != "Test User" {
		t.Errorf("SubmittedByName = %v, want %v", suggestion.SubmittedByName, "Test User")
	}
	if suggestion.Area != "Dashboard" {
		t.Errorf("Area = %v, want %v", suggestion.Area, "Dashboard")
	}
	if suggestion.Status != SuggestionStatusPending {
		t.Errorf("Status = %v, want %v", suggestion.Status, SuggestionStatusPending)
	}
}

func TestSuggestionDefaultValues(t *testing.T) {
	suggestion := Suggestion{}
	
	// Default status should be empty
	if suggestion.Status != "" {
		t.Errorf("Default Status = %v, want empty string", suggestion.Status)
	}
	
	// ID should be empty until set
	if suggestion.ID != "" {
		t.Errorf("Default ID = %v, want empty string", suggestion.ID)
	}
}

func TestSuggestionPK(t *testing.T) {
	suggestion := Suggestion{
		PK: "SUGGESTION",
		SK: "sug_123",
	}

	if suggestion.PK != "SUGGESTION" {
		t.Errorf("PK = %v, want SUGGESTION", suggestion.PK)
	}
	if suggestion.SK != "sug_123" {
		t.Errorf("SK = %v, want sug_123", suggestion.SK)
	}
}
