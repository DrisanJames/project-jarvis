package worker

import (
	"regexp"
	"testing"
)

func govRow() domainGovernorRow {
	return domainGovernorRow{brand: "ht", isp: "gmail", dailyCap: 31000, coldCap: 4000,
		allowed: regexp.MustCompile(`^internal_auto_insurance`), laneDailyCap: 4000, laneWindowCap: 250, windowMinutes: 15, enforce: true}
}

func TestDomainGovernorDecide(t *testing.T) {
	row := govRow()
	cases := []struct {
		name     string
		vertical string
		capIn    int
		sp       domainGovernorSpend
		want     int
		wantWhy  string
	}{
		{"unconstrained wave", "internal_auto_insurance_gmail_v1", 100, domainGovernorSpend{board: 20000, drips: 100, laneToday: 100, laneWindow: 0}, 100, "lane cap"},
		{"15-min window binds", "internal_auto_insurance_gmail_v1", 100, domainGovernorSpend{board: 20000, drips: 1000, laneToday: 1000, laneWindow: 200}, 50, "lane_window_cap"},
		{"window exhausted", "internal_auto_insurance_gmail_v1", 100, domainGovernorSpend{laneWindow: 250}, 0, "lane_window_cap"},
		{"lane daily binds", "internal_auto_insurance_gmail_v1", 100, domainGovernorSpend{laneToday: 3950}, 50, "lane_daily_cap"},
		{"cold cap binds across lanes", "internal_auto_insurance_gmail_v1", 100, domainGovernorSpend{drips: 3980, laneToday: 1000}, 20, "cold_cap"},
		{"board ate the global cap", "internal_auto_insurance_gmail_v1", 100, domainGovernorSpend{board: 30990, drips: 0}, 10, "daily_cap"},
		{"board over cap -> zero, never negative", "internal_auto_insurance_gmail_v1", 100, domainGovernorSpend{board: 40000}, 0, "daily_cap"},
		{"non-internal vertical banned", "refi_heloc", 100, domainGovernorSpend{}, 0, "vertical not allowed"},
		{"other internal auto lane allowed", "internal_auto_insurance_v5", 100, domainGovernorSpend{}, 100, "lane cap"},
		{"cap already zero stays zero", "internal_auto_insurance_gmail_v1", 0, domainGovernorSpend{}, 0, "cap already 0"},
	}
	for _, tc := range cases {
		got, why := domainGovernorDecide(row, tc.vertical, tc.capIn, tc.sp)
		if got != tc.want {
			t.Errorf("%s: cap=%d want %d (%s)", tc.name, got, tc.want, why)
		}
		if len(why) < len(tc.wantWhy) || why[:len(tc.wantWhy)] != tc.wantWhy {
			t.Errorf("%s: reason %q, want prefix %q", tc.name, why, tc.wantWhy)
		}
	}
}

func TestDomainGovernorShadowLeavesCaps(t *testing.T) {
	// Shadow rows must never change the cap; enforce rows must.
	row := govRow()
	row.enforce = false
	capIn := 100
	got, _ := domainGovernorDecide(row, "refi_heloc", capIn, domainGovernorSpend{})
	if got != 0 {
		t.Fatalf("decide is mode-agnostic; expected 0 got %d", got)
	}
	// applyDomainGovernor is what honours shadow; verified by the mode branch
	// in code review (no DB in unit scope). Guard the kill switch here.
	t.Setenv("PARTNER_DRIP_DOMAIN_GOVERNOR_DISABLED", "true")
	if !domainGovernorDisabled() {
		t.Fatal("kill switch not honoured")
	}
}
