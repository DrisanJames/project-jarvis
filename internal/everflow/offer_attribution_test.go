package everflow

import (
	"testing"

	"github.com/ignite/sparkpost-monitor/internal/ongage"
	"github.com/stretchr/testify/assert"
)

func TestBuildOfferESPVolumeMap(t *testing.T) {
	campaigns := []ongage.ProcessedCampaign{
		{
			Name: "01292026_FYF_362_FarmersInsuranceCPM_CAB_OPENERS",
			ESP:  "SparkPost",
			Sent: 100000,
		},
		{
			Name: "01292026_FYF_362_FarmersInsuranceCPM_YAH_OPENERS",
			ESP:  "SparkPost",
			Sent: 50000,
		},
		{
			Name: "01292026_HRO_362_FarmersInsuranceCPM_ABS_OPENERS",
			ESP:  "Mailgun",
			Sent: 75000,
		},
		{
			Name: "01282026_DIH_400_HealthcareCPM_CAB_OPENERS",
			ESP:  "Amazon SES",
			Sent: 200000,
		},
	}

	offerMap := BuildOfferESPVolumeMap(campaigns)

	// Check offer 362 (Farmers Insurance CPM)
	assert.Contains(t, offerMap, "362")
	offer362 := offerMap["362"]
	assert.Equal(t, int64(225000), offer362.TotalSent)
	assert.Equal(t, int64(150000), offer362.ESPVolumes["SparkPost"])
	assert.Equal(t, int64(75000), offer362.ESPVolumes["Mailgun"])
	assert.InDelta(t, 66.67, offer362.ESPPercents["SparkPost"], 0.1)
	assert.InDelta(t, 33.33, offer362.ESPPercents["Mailgun"], 0.1)

	// Check offer 400 (Healthcare CPM)
	assert.Contains(t, offerMap, "400")
	offer400 := offerMap["400"]
	assert.Equal(t, int64(200000), offer400.TotalSent)
	assert.Equal(t, int64(200000), offer400.ESPVolumes["Amazon SES"])
	assert.InDelta(t, 100.0, offer400.ESPPercents["Amazon SES"], 0.1)
}

func TestAttributeOfferRevenueToESPs(t *testing.T) {
	offers := []OfferPerformance{
		{OfferID: "362", OfferName: "Farmers Insurance CPM", Revenue: 10000, Payout: 9500},
		{OfferID: "400", OfferName: "Healthcare CPM", Revenue: 5000, Payout: 4800},
	}

	offerVolumes := map[string]*OfferESPVolume{
		"362": {
			OfferID:    "362",
			OfferName:  "FarmersInsuranceCPM",
			TotalSent:  200000,
			ESPVolumes: map[string]int64{"SparkPost": 150000, "Mailgun": 50000},
		},
		"400": {
			OfferID:    "400",
			OfferName:  "HealthcareCPM",
			TotalSent:  100000,
			ESPVolumes: map[string]int64{"Amazon SES": 100000},
		},
	}

	attributions := AttributeOfferRevenueToESPs(offers, offerVolumes)

	// Should have 3 attributions: 2 for offer 362 (SparkPost, Mailgun) + 1 for offer 400 (SES)
	assert.Len(t, attributions, 3)

	// Find SparkPost attribution for offer 362
	var sparkPostAttr *OfferESPAttribution
	var mailgunAttr *OfferESPAttribution
	var sesAttr *OfferESPAttribution
	for i := range attributions {
		if attributions[i].ESPName == "SparkPost" && attributions[i].OfferID == "362" {
			sparkPostAttr = &attributions[i]
		}
		if attributions[i].ESPName == "Mailgun" && attributions[i].OfferID == "362" {
			mailgunAttr = &attributions[i]
		}
		if attributions[i].ESPName == "Amazon SES" && attributions[i].OfferID == "400" {
			sesAttr = &attributions[i]
		}
	}

	// SparkPost gets 75% of offer 362 revenue (150k/200k)
	assert.NotNil(t, sparkPostAttr)
	assert.InDelta(t, 7500.0, sparkPostAttr.Revenue, 0.01) // 10000 * 0.75

	// Mailgun gets 25% of offer 362 revenue (50k/200k)
	assert.NotNil(t, mailgunAttr)
	assert.InDelta(t, 2500.0, mailgunAttr.Revenue, 0.01) // 10000 * 0.25

	// SES gets 100% of offer 400 revenue
	assert.NotNil(t, sesAttr)
	assert.InDelta(t, 5000.0, sesAttr.Revenue, 0.01)
}

func TestAggregateOfferAttributionToESPs(t *testing.T) {
	attributions := []OfferESPAttribution{
		{ESPName: "SparkPost", OfferID: "362", Revenue: 7500, Payout: 7000, SentVolume: 150000},
		{ESPName: "SparkPost", OfferID: "400", Revenue: 2000, Payout: 1900, SentVolume: 50000},
		{ESPName: "Mailgun", OfferID: "362", Revenue: 2500, Payout: 2400, SentVolume: 50000},
	}

	espMap := AggregateOfferAttributionToESPs(attributions)

	assert.Len(t, espMap, 2)

	sparkPost := espMap["SparkPost"]
	assert.NotNil(t, sparkPost)
	assert.InDelta(t, 9500.0, sparkPost.Revenue, 0.01)
	assert.Equal(t, int64(200000), sparkPost.TotalSent)

	mailgun := espMap["Mailgun"]
	assert.NotNil(t, mailgun)
	assert.InDelta(t, 2500.0, mailgun.Revenue, 0.01)
	assert.Equal(t, int64(50000), mailgun.TotalSent)
}

func TestExtractOfferIDFromName(t *testing.T) {
	tests := []struct {
		name     string
		expected string
	}{
		{"Farmers Insurance CPM (362)", "362"},
		{"Healthcare.com U65 ACA (400)", "400"},
		{"Some Offer (2546)", "2546"},
		{"No ID Offer", ""},
		{"Invalid (abc)", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExtractOfferIDFromName(tt.name)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParseOfferIDFromCampaignName(t *testing.T) {
	tests := []struct {
		campaignName string
		expectedID   string
	}{
		{"01292026_FYF_362_FarmersInsuranceCPM_CAB_OPENERS", "362"},
		{"01292026_HRO_400_Healthcare.comU65ACA_YAH_OPENERS", "400"},
		{"12302025_DIH_510_EasyCanvasEmailCPS_ABS_OPENERS", "510"},
		{"invalid_format", ""},
	}

	for _, tt := range tests {
		t.Run(tt.campaignName, func(t *testing.T) {
			_, _, offerID, _, _, _ := ParseCampaignName(tt.campaignName)
			assert.Equal(t, tt.expectedID, offerID)
		})
	}
}
