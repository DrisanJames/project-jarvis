package worker

import "testing"

func TestDeriveBrandKey(t *testing.T) {
	tests := []struct {
		fromEmail string
		want      string
	}{
		{"hello@em.discountblog.com", "discountblog"},
		{"hello@em.quizfiesta.com", "quizfiesta"},
		{"hello@m.discountblog.com", "discountblog"},
		{"noreply@discountblog.com", "discountblog"},
		{"hello@em.DISCOUNTBLOG.COM", "discountblog"},
		{"", ""},
		{"noemail", ""},
		{"user@sub.em.example.com", "sub"},
	}
	for _, tt := range tests {
		t.Run(tt.fromEmail, func(t *testing.T) {
			got := deriveBrandKey(tt.fromEmail)
			if got != tt.want {
				t.Errorf("deriveBrandKey(%q) = %q, want %q", tt.fromEmail, got, tt.want)
			}
		})
	}
}

func TestCoalesceWaveValue(t *testing.T) {
	tests := []struct {
		value, fallback, want string
	}{
		{"cached subject", "default subject", "cached subject"},
		{"", "default subject", "default subject"},
		{"cached", "", "cached"},
		{"", "", ""},
	}
	for _, tt := range tests {
		got := coalesceWaveValue(tt.value, tt.fallback)
		if got != tt.want {
			t.Errorf("coalesceWaveValue(%q, %q) = %q, want %q", tt.value, tt.fallback, got, tt.want)
		}
	}
}
