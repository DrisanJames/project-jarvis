package datanorm

import (
	"strings"
	"testing"
)

func TestClassifier(t *testing.T) {
	c := NewClassifier()

	tests := []struct {
		name     string
		filename string
		headers  []string
		want     Classification
	}{
		{"mailable keyword", "2024-01-JVC-Mailable.csv", []string{"email", "first_name"}, ClassMailable},
		{"suppression keyword", "suppression-list-january.csv", []string{"email"}, ClassSuppression},
		{"warmup keyword", "warmup-data-high.csv", []string{"email", "score"}, ClassWarmup},
		{"header with suppress_reason indicates suppression", "unknown.csv", []string{"email", "suppress_reason"}, ClassSuppression},
		{"header with first_name indicates mailable", "data.csv", []string{"email", "first_name", "last_name"}, ClassMailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.Classify(tt.filename, tt.headers)
			if got != tt.want {
				t.Errorf("Classify(%q, %v) = %s, want %s", tt.filename, tt.headers, got, tt.want)
			}
		})
	}
}

func TestClassifications(t *testing.T) {
	if ClassMailable != "mailable" {
		t.Errorf("ClassMailable = %q", ClassMailable)
	}
	if ClassSuppression != "suppression" {
		t.Errorf("ClassSuppression = %q", ClassSuppression)
	}
	if !strings.Contains(string(ClassWarmup), "warmup") {
		t.Errorf("ClassWarmup = %q", ClassWarmup)
	}
}
