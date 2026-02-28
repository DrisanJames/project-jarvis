package everflow

import (
	"testing"
	"time"
)

func TestParseSub1(t *testing.T) {
	tests := []struct {
		name          string
		sub1          string
		wantProperty  string
		wantOfferID   string
		wantMailingID string
		wantErr       bool
	}{
		{
			name:          "Full format with property code",
			sub1:          "FYF_556_1091_01262026_3219015617",
			wantProperty:  "FYF",
			wantOfferID:   "556",
			wantMailingID: "3219015617",
			wantErr:       false,
		},
		{
			name:          "TDIH property",
			sub1:          "TDIH_407_3926_01262026_3219537162",
			wantProperty:  "TDIH",
			wantOfferID:   "407",
			wantMailingID: "3219537162",
			wantErr:       false,
		},
		{
			name:          "HRO property",
			sub1:          "HRO_1944_TXT3_01262026_3219752092",
			wantProperty:  "HRO",
			wantOfferID:   "1944",
			wantMailingID: "3219752092",
			wantErr:       false,
		},
		{
			name:          "Without property code",
			sub1:          "400_3875_01262026_3219015617",
			wantProperty:  "",
			wantOfferID:   "400",
			wantMailingID: "3219015617",
			wantErr:       false,
		},
		{
			name:          "Unknown property code",
			sub1:          "XYZ_123_456_01262026_9999999",
			wantProperty:  "XYZ",
			wantOfferID:   "123",
			wantMailingID: "9999999",
			wantErr:       false,
		},
		{
			name:    "Empty string",
			sub1:    "",
			wantErr: true,
		},
		{
			name:    "Invalid format",
			sub1:    "invalid",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := ParseSub1(tt.sub1)
			
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseSub1() expected error, got nil")
				}
				return
			}
			
			if err != nil {
				t.Errorf("ParseSub1() unexpected error: %v", err)
				return
			}

			if parsed.PropertyCode != tt.wantProperty {
				t.Errorf("PropertyCode = %s, want %s", parsed.PropertyCode, tt.wantProperty)
			}
			if parsed.OfferID != tt.wantOfferID {
				t.Errorf("OfferID = %s, want %s", parsed.OfferID, tt.wantOfferID)
			}
			if parsed.MailingID != tt.wantMailingID {
				t.Errorf("MailingID = %s, want %s", parsed.MailingID, tt.wantMailingID)
			}
		})
	}
}

func TestParseCampaignName(t *testing.T) {
	tests := []struct {
		name         string
		campaignName string
		wantDate     string
		wantProperty string
		wantOfferID  string
		wantOffer    string
		wantSegment  string
		wantErr      bool
	}{
		{
			name:         "Full campaign name",
			campaignName: "02052025_HRO_1944_FidelityLife_OPENERS",
			wantDate:     "02052025",
			wantProperty: "HRO",
			wantOfferID:  "1944",
			wantOffer:    "FidelityLife",
			wantSegment:  "OPENERS",
			wantErr:      false,
		},
		{
			name:         "Campaign with multi-word offer",
			campaignName: "01262026_TDIH_407_Empire_Flooring_CPL_ACTIVE",
			wantDate:     "01262026",
			wantProperty: "TDIH",
			wantOfferID:  "407",
			wantOffer:    "Empire_Flooring_CPL",
			wantSegment:  "ACTIVE",
			wantErr:      false,
		},
		{
			name:    "Empty string",
			campaignName: "",
			wantErr: true,
		},
		{
			name:    "Invalid format",
			campaignName: "invalid",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			date, property, offerID, offer, segment, err := ParseCampaignName(tt.campaignName)
			
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseCampaignName() expected error, got nil")
				}
				return
			}
			
			if err != nil {
				t.Errorf("ParseCampaignName() unexpected error: %v", err)
				return
			}

			if date != tt.wantDate {
				t.Errorf("date = %s, want %s", date, tt.wantDate)
			}
			if property != tt.wantProperty {
				t.Errorf("property = %s, want %s", property, tt.wantProperty)
			}
			if offerID != tt.wantOfferID {
				t.Errorf("offerID = %s, want %s", offerID, tt.wantOfferID)
			}
			if offer != tt.wantOffer {
				t.Errorf("offer = %s, want %s", offer, tt.wantOffer)
			}
			if segment != tt.wantSegment {
				t.Errorf("segment = %s, want %s", segment, tt.wantSegment)
			}
		})
	}
}

func TestParseTimestamp(t *testing.T) {
	tests := []struct {
		name    string
		ts      string
		wantErr bool
	}{
		{
			name:    "PST format",
			ts:      "01/27/2026 00:06:13 PST",
			wantErr: false,
		},
		{
			name:    "ISO format",
			ts:      "2026-01-27 00:00:00",
			wantErr: false,
		},
		{
			name:    "Date only",
			ts:      "2026-01-27",
			wantErr: false,
		},
		{
			name:    "Empty string",
			ts:      "",
			wantErr: false, // Returns zero time, no error
		},
		{
			name:    "Invalid format",
			ts:      "invalid",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseTimestamp(tt.ts)
			
			if tt.wantErr && err == nil {
				t.Errorf("ParseTimestamp() expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("ParseTimestamp() unexpected error: %v", err)
			}
		})
	}
}

func TestParseDate(t *testing.T) {
	tests := []struct {
		name     string
		dateStr  string
		wantYear int
		wantMonth time.Month
		wantDay  int
		wantErr  bool
	}{
		{
			name:      "Valid date",
			dateStr:   "01262026",
			wantYear:  2026,
			wantMonth: time.January,
			wantDay:   26,
			wantErr:   false,
		},
		{
			name:      "Another valid date",
			dateStr:   "12252025",
			wantYear:  2025,
			wantMonth: time.December,
			wantDay:   25,
			wantErr:   false,
		},
		{
			name:    "Invalid length",
			dateStr: "1262026",
			wantErr: true,
		},
		{
			name:    "Empty string",
			dateStr: "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			date, err := ParseDate(tt.dateStr)
			
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseDate() expected error, got nil")
				}
				return
			}
			
			if err != nil {
				t.Errorf("ParseDate() unexpected error: %v", err)
				return
			}

			if date.Year() != tt.wantYear {
				t.Errorf("Year = %d, want %d", date.Year(), tt.wantYear)
			}
			if date.Month() != tt.wantMonth {
				t.Errorf("Month = %v, want %v", date.Month(), tt.wantMonth)
			}
			if date.Day() != tt.wantDay {
				t.Errorf("Day = %d, want %d", date.Day(), tt.wantDay)
			}
		})
	}
}

func TestIsNumeric(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"123", true},
		{"0", true},
		{"12345678901234567890", true},
		{"abc", false},
		{"12a3", false},
		{"", false},
		{" 123", false},
		{"123 ", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := isNumeric(tt.input); got != tt.expected {
				t.Errorf("isNumeric(%q) = %v, want %v", tt.input, got, tt.expected)
			}
		})
	}
}

func TestExtractMailingIDFromSub1(t *testing.T) {
	tests := []struct {
		sub1     string
		expected string
	}{
		{"FYF_556_1091_01262026_3219015617", "3219015617"},
		{"TDIH_407_3926_01262026_3219537162", "3219537162"},
		{"invalid", ""},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.sub1, func(t *testing.T) {
			if got := ExtractMailingIDFromSub1(tt.sub1); got != tt.expected {
				t.Errorf("ExtractMailingIDFromSub1(%q) = %q, want %q", tt.sub1, got, tt.expected)
			}
		})
	}
}

func TestExtractPropertyFromSub1(t *testing.T) {
	tests := []struct {
		sub1     string
		expected string
	}{
		{"FYF_556_1091_01262026_3219015617", "FYF"},
		{"TDIH_407_3926_01262026_3219537162", "TDIH"},
		{"HRO_1944_TXT3_01262026_3219752092", "HRO"},
		{"invalid", ""},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.sub1, func(t *testing.T) {
			if got := ExtractPropertyFromSub1(tt.sub1); got != tt.expected {
				t.Errorf("ExtractPropertyFromSub1(%q) = %q, want %q", tt.sub1, got, tt.expected)
			}
		})
	}
}

func TestValidatePropertyCode(t *testing.T) {
	tests := []struct {
		code     string
		expected bool
	}{
		{"FYF", true},
		{"TDIH", true},
		{"HRO", true},
		{"ftt", true}, // Should be case-insensitive
		{"INVALID", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			if got := ValidatePropertyCode(tt.code); got != tt.expected {
				t.Errorf("ValidatePropertyCode(%q) = %v, want %v", tt.code, got, tt.expected)
			}
		})
	}
}

func TestGetPropertyName(t *testing.T) {
	tests := []struct {
		code     string
		expected string
	}{
		{"FYF", "findyourfit.net"},
		{"TDIH", "thisdayinhistory.co"},
		{"HRO", "horoscopeinfo.com"},
		{"NPY", "newproductsforyou.com"},
		{"UNKNOWN", "UNKNOWN"}, // Should return code if not found
	}

	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			if got := GetPropertyName(tt.code); got != tt.expected {
				t.Errorf("GetPropertyName(%q) = %q, want %q", tt.code, got, tt.expected)
			}
		})
	}
}

func TestBuildCampaignInfoFromName(t *testing.T) {
	info := BuildCampaignInfoFromName("123456", "02052025_HRO_1944_FidelityLife_OPENERS")
	
	if info.ID != "123456" {
		t.Errorf("ID = %s, want 123456", info.ID)
	}
	if info.PropertyCode != "HRO" {
		t.Errorf("PropertyCode = %s, want HRO", info.PropertyCode)
	}
	if info.OfferID != "1944" {
		t.Errorf("OfferID = %s, want 1944", info.OfferID)
	}
	if info.ScheduleDate != "02052025" {
		t.Errorf("ScheduleDate = %s, want 02052025", info.ScheduleDate)
	}
}
