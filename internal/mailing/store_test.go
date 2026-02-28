package mailing

import (
	"testing"
)

func TestHashEmailExported(t *testing.T) {
	tests := []struct {
		name     string
		email    string
		expected string
	}{
		{
			name:     "lowercase email",
			email:    "test@example.com",
			expected: HashEmail("test@example.com"),
		},
		{
			name:     "uppercase email should match lowercase",
			email:    "TEST@EXAMPLE.COM",
			expected: HashEmail("test@example.com"),
		},
		{
			name:     "email with spaces should be trimmed",
			email:    "  test@example.com  ",
			expected: HashEmail("test@example.com"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := HashEmail(tt.email)
			if result != tt.expected {
				t.Errorf("HashEmail(%q) = %q, want %q", tt.email, result, tt.expected)
			}
		})
	}
}

func TestValidateEmail(t *testing.T) {
	tests := []struct {
		name  string
		email string
		want  bool
	}{
		{"valid email", "test@example.com", true},
		{"valid email with subdomain", "test@mail.example.com", true},
		{"valid email with plus", "test+tag@example.com", true},
		{"empty email", "", false},
		{"no at sign", "testexample.com", false},
		{"no domain", "test@", false},
		{"no local part", "@example.com", false},
		{"no tld", "test@example", false},
		{"multiple at signs", "test@@example.com", false},
		{"too long local part", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa@example.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidateEmail(tt.email); got != tt.want {
				t.Errorf("ValidateEmail(%q) = %v, want %v", tt.email, got, tt.want)
			}
		})
	}
}

func TestCampaignCalculateStats(t *testing.T) {
	tests := []struct {
		name     string
		campaign Campaign
		wantOpen float64
		wantClick float64
	}{
		{
			name: "campaign with metrics",
			campaign: Campaign{
				SentCount:  1000,
				OpenCount:  200,
				ClickCount: 50,
			},
			wantOpen:  20.0,
			wantClick: 5.0,
		},
		{
			name: "campaign with no sends",
			campaign: Campaign{
				SentCount:  0,
				OpenCount:  0,
				ClickCount: 0,
			},
			wantOpen:  0,
			wantClick: 0,
		},
		{
			name: "high performing campaign",
			campaign: Campaign{
				SentCount:  500,
				OpenCount:  150,
				ClickCount: 30,
			},
			wantOpen:  30.0,
			wantClick: 6.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stats := tt.campaign.CalculateStats()
			if stats.OpenRate != tt.wantOpen {
				t.Errorf("OpenRate = %v, want %v", stats.OpenRate, tt.wantOpen)
			}
			if stats.ClickRate != tt.wantClick {
				t.Errorf("ClickRate = %v, want %v", stats.ClickRate, tt.wantClick)
			}
		})
	}
}
