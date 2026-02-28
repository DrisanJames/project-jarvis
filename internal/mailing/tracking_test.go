package mailing

import (
	"testing"
)

func TestIsOwnedDomain(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://discountblog.com/deals/123", true},
		{"https://www.discountblog.com/", true},
		{"https://quizfiesta.com/quiz/1", true},
		{"https://getmecoupons.net/coupon/abc", true},
		{"https://google.com/search", false},
		{"https://evil-discountblog.com/", false},
		{"not-a-url", false},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got := isOwnedDomain(tt.url)
			if got != tt.want {
				t.Errorf("isOwnedDomain(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestValidateEmailTracking(t *testing.T) {
	tests := []struct {
		email string
		valid bool
	}{
		{"test@example.com", true},
		{"user@domain.co.uk", true},
		{"bad", false},
		{"@domain.com", false},
		{"user@", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			got := ValidateEmail(tt.email)
			if got != tt.valid {
				t.Errorf("ValidateEmail(%q) = %v, want %v", tt.email, got, tt.valid)
			}
		})
	}
}
