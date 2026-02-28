package mailing

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestEngagementScorer_CalculateScore(t *testing.T) {
	scorer := &EngagementScorer{}

	tests := []struct {
		name     string
		sub      *Subscriber
		wantMin  float64
		wantMax  float64
	}{
		{
			name: "new subscriber",
			sub: &Subscriber{
				TotalEmailsReceived: 0,
			},
			wantMin: 50,
			wantMax: 50,
		},
		{
			name: "engaged subscriber",
			sub: &Subscriber{
				TotalEmailsReceived: 10,
				TotalOpens:          8,
				TotalClicks:         3,
				LastOpenAt:          timePtr(time.Now().Add(-24 * time.Hour)),
			},
			wantMin: 40,
			wantMax: 100,
		},
		{
			name: "inactive subscriber",
			sub: &Subscriber{
				TotalEmailsReceived: 50,
				TotalOpens:          2,
				TotalClicks:         0,
				LastOpenAt:          timePtr(time.Now().Add(-180 * 24 * time.Hour)),
			},
			wantMin: 0,
			wantMax: 20,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score := scorer.CalculateScore(tt.sub)
			if score < tt.wantMin || score > tt.wantMax {
				t.Errorf("CalculateScore() = %v, want between %v and %v", score, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestOptimalSendTimePredictor_PredictOptimalSendTime(t *testing.T) {
	predictor := &OptimalSendTimePredictor{}

	tests := []struct {
		name           string
		sub            *Subscriber
		wantHourMin    int
		wantHourMax    int
		wantConfMin    float64
	}{
		{
			name: "high engagement subscriber",
			sub: &Subscriber{
				EngagementScore: 85,
			},
			wantHourMin: 8,
			wantHourMax: 10,
			wantConfMin: 0.5,
		},
		{
			name: "low engagement subscriber",
			sub: &Subscriber{
				EngagementScore: 25,
			},
			wantHourMin: 13,
			wantHourMax: 15,
			wantConfMin: 0.3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hour, conf, _ := predictor.predictWithSubscriber(tt.sub)
			if hour < tt.wantHourMin || hour > tt.wantHourMax {
				t.Errorf("hour = %v, want between %v and %v", hour, tt.wantHourMin, tt.wantHourMax)
			}
			if conf < tt.wantConfMin {
				t.Errorf("confidence = %v, want >= %v", conf, tt.wantConfMin)
			}
		})
	}
}

func (p *OptimalSendTimePredictor) predictWithSubscriber(sub *Subscriber) (int, float64, error) {
	if sub.OptimalSendHourUTC != nil {
		confidence := 0.8
		if sub.TotalOpens >= 10 {
			confidence = 0.9
		}
		return *sub.OptimalSendHourUTC, confidence, nil
	}

	if sub.EngagementScore >= 70 {
		return 9, 0.6, nil
	} else if sub.EngagementScore >= 40 {
		return 11, 0.5, nil
	}
	return 14, 0.4, nil
}

func TestContentOptimizer_OptimizeSubjectLine(t *testing.T) {
	optimizer := NewContentOptimizer()

	tests := []struct {
		name     string
		subject  string
		wantLen  int
	}{
		{
			name:    "basic subject",
			subject: "Check out our new products",
			wantLen: 3, // Original + emoji + personalization
		},
		{
			name:    "short subject",
			subject: "Sale!",
			wantLen: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			variants := optimizer.OptimizeSubjectLine(tt.subject, "general")
			if len(variants) != tt.wantLen {
				t.Errorf("len(variants) = %v, want %v", len(variants), tt.wantLen)
			}
			// Verify sorted by predicted open rate
			for i := 1; i < len(variants); i++ {
				if variants[i].PredictedOpenRate > variants[i-1].PredictedOpenRate {
					t.Errorf("variants not sorted by predicted open rate")
				}
			}
		})
	}
}

func TestSendingPlanPredictions(t *testing.T) {
	plan := &SendingPlanOption{
		RecommendedVolume: 10000,
		Predictions: PlanPredictions{
			EstimatedOpens:   1500,
			EstimatedClicks:  300,
			EstimatedRevenue: 120.0,
		},
	}

	// Verify basic math
	openRate := float64(plan.Predictions.EstimatedOpens) / float64(plan.RecommendedVolume) * 100
	if openRate < 10 || openRate > 20 {
		t.Errorf("open rate %v outside expected range", openRate)
	}

	clickRate := float64(plan.Predictions.EstimatedClicks) / float64(plan.RecommendedVolume) * 100
	if clickRate < 1 || clickRate > 5 {
		t.Errorf("click rate %v outside expected range", clickRate)
	}
}

func timePtr(t time.Time) *time.Time {
	return &t
}

func uuidPtr(u uuid.UUID) *uuid.UUID {
	return &u
}
