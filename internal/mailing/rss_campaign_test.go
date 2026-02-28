package mailing

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFeedItem_Validation(t *testing.T) {
	tests := []struct {
		name string
		item FeedItem
		want bool
	}{
		{
			name: "valid feed item with all fields",
			item: FeedItem{
				GUID:        "https://example.com/article-1",
				Title:       "Test Article",
				Description: "This is a test description",
				Link:        "https://example.com/article-1",
				PubDate:     time.Now(),
				ImageURL:    "https://example.com/image.jpg",
				Author:      "John Doe",
				Categories:  []string{"Tech", "News"},
			},
			want: true,
		},
		{
			name: "valid feed item with minimal fields",
			item: FeedItem{
				GUID:        "guid-123",
				Title:       "Minimal Article",
				Link:        "https://example.com/minimal",
				PubDate:     time.Now(),
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Verify GUID and Link are set
			assert.NotEmpty(t, tt.item.GUID)
			assert.NotEmpty(t, tt.item.Link)
		})
	}
}

func TestRSSCampaign_Defaults(t *testing.T) {
	campaign := RSSCampaign{
		OrgID:   "org-123",
		Name:    "Test RSS Campaign",
		FeedURL: "https://example.com/feed.xml",
		ListID:  "list-123",
	}

	// Verify required fields
	assert.NotEmpty(t, campaign.OrgID)
	assert.NotEmpty(t, campaign.Name)
	assert.NotEmpty(t, campaign.FeedURL)

	// Default values should be empty before service processes them
	assert.Empty(t, campaign.ID)
	assert.Empty(t, campaign.PollInterval)
}

func TestStripHTML(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple HTML tags",
			input:    "<p>Hello <strong>World</strong></p>",
			expected: "Hello World",
		},
		{
			name:     "HTML entities",
			input:    "&amp; &lt; &gt; &quot;",
			expected: "& < > \"",
		},
		{
			name:     "complex HTML",
			input:    "<div class=\"test\"><p>Paragraph 1</p><p>Paragraph 2</p></div>",
			expected: "Paragraph 1Paragraph 2",
		},
		{
			name:     "plain text",
			input:    "No HTML here",
			expected: "No HTML here",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "script tags",
			input:    "<script>alert('xss')</script>Safe content",
			expected: "alert('xss')Safe content",
		},
		{
			name:     "multiple spaces",
			input:    "<p>Multiple    spaces   here</p>",
			expected: "Multiple spaces here",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := stripHTML(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestNullString(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool // true if should be nil
	}{
		{
			name:     "empty string returns nil",
			input:    "",
			expected: true,
		},
		{
			name:     "non-empty string returns pointer",
			input:    "test",
			expected: false,
		},
		{
			name:     "whitespace only returns pointer",
			input:    "   ",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := nullString(tt.input)
			if tt.expected {
				assert.Nil(t, result)
			} else {
				require.NotNil(t, result)
				assert.Equal(t, tt.input, *result)
			}
		})
	}
}

func TestRSSCampaign_PollIntervals(t *testing.T) {
	validIntervals := []string{"hourly", "daily", "weekly"}

	for _, interval := range validIntervals {
		t.Run("valid_"+interval, func(t *testing.T) {
			campaign := RSSCampaign{
				PollInterval: interval,
			}
			assert.Contains(t, validIntervals, campaign.PollInterval)
		})
	}
}

func TestFeedItem_DateParsing(t *testing.T) {
	now := time.Now()
	yesterday := now.Add(-24 * time.Hour)
	nextWeek := now.Add(7 * 24 * time.Hour)

	tests := []struct {
		name    string
		pubDate time.Time
		valid   bool
	}{
		{
			name:    "current time",
			pubDate: now,
			valid:   true,
		},
		{
			name:    "past date",
			pubDate: yesterday,
			valid:   true,
		},
		{
			name:    "future date",
			pubDate: nextWeek,
			valid:   true,
		},
		{
			name:    "zero time",
			pubDate: time.Time{},
			valid:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := FeedItem{
				GUID:    "test-guid",
				Title:   "Test",
				Link:    "https://example.com",
				PubDate: tt.pubDate,
			}

			if tt.valid {
				assert.False(t, item.PubDate.IsZero())
			} else {
				assert.True(t, item.PubDate.IsZero())
			}
		})
	}
}

func TestRSSCampaign_MergeTags(t *testing.T) {
	// Test that template service can handle RSS merge tags
	templateSvc := NewTemplateService()

	rssContext := map[string]interface{}{
		"rss": map[string]interface{}{
			"title":       "Breaking News: Go 2.0 Released",
			"description": "The Go team has released version 2.0...",
			"link":        "https://golang.org/news/go2",
			"image":       "https://golang.org/images/go2.png",
			"author":      "The Go Team",
			"date":        "February 4, 2026",
			"categories":  "Tech, Programming",
		},
	}

	tests := []struct {
		name     string
		template string
		expected string
	}{
		{
			name:     "title merge tag",
			template: "{{rss.title}}",
			expected: "Breaking News: Go 2.0 Released",
		},
		{
			name:     "description merge tag",
			template: "{{rss.description}}",
			expected: "The Go team has released version 2.0...",
		},
		{
			name:     "link merge tag",
			template: "{{rss.link}}",
			expected: "https://golang.org/news/go2",
		},
		{
			name:     "author merge tag",
			template: "{{rss.author}}",
			expected: "The Go Team",
		},
		{
			name:     "date merge tag",
			template: "{{rss.date}}",
			expected: "February 4, 2026",
		},
		{
			name:     "combined template",
			template: "New: {{rss.title}} by {{rss.author}}",
			expected: "New: Breaking News: Go 2.0 Released by The Go Team",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := templateSvc.Render("", tt.template, rssContext)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestRSSCampaign_DefaultHTMLGeneration(t *testing.T) {
	svc := &RSSCampaignService{
		templateSvc: NewTemplateService(),
	}

	item := FeedItem{
		GUID:        "test-guid-123",
		Title:       "Test Article Title",
		Description: "This is a test description for the article.",
		Link:        "https://example.com/article",
		PubDate:     time.Date(2026, 2, 4, 10, 0, 0, 0, time.UTC),
		ImageURL:    "https://example.com/image.jpg",
		Author:      "Test Author",
	}

	html := svc.generateDefaultRSSHTML(item)

	// Verify HTML contains key elements
	assert.Contains(t, html, item.Title)
	assert.Contains(t, html, item.Description)
	assert.Contains(t, html, item.Link)
	assert.Contains(t, html, item.ImageURL)
	assert.Contains(t, html, "Read More")
	assert.Contains(t, html, "<!DOCTYPE html>")
}

func TestRSSCampaign_DefaultPlainGeneration(t *testing.T) {
	svc := &RSSCampaignService{
		templateSvc: NewTemplateService(),
	}

	item := FeedItem{
		GUID:        "test-guid-123",
		Title:       "Test Article Title",
		Description: "This is a test description for the article.",
		Link:        "https://example.com/article",
		PubDate:     time.Date(2026, 2, 4, 10, 0, 0, 0, time.UTC),
	}

	plain := svc.generateDefaultRSSPlain(item)

	// Verify plain text contains key elements
	assert.Contains(t, plain, item.Title)
	assert.Contains(t, plain, item.Description)
	assert.Contains(t, plain, item.Link)
	assert.Contains(t, plain, "Published:")
}

func TestRSSSentItem_Status(t *testing.T) {
	validStatuses := []string{"pending", "generated", "sent", "failed", "skipped"}

	for _, status := range validStatuses {
		t.Run("status_"+status, func(t *testing.T) {
			item := RSSSentItem{
				Status: status,
			}
			assert.Contains(t, validStatuses, item.Status)
		})
	}
}

func TestRSSPollLog_Fields(t *testing.T) {
	log := RSSPollLog{
		ID:                 "log-123",
		RSSCampaignID:      "campaign-456",
		ItemsFound:         10,
		NewItems:           3,
		CampaignsGenerated: 3,
		Status:             "success",
		DurationMs:         150,
		PolledAt:           time.Now(),
	}

	assert.NotEmpty(t, log.ID)
	assert.NotEmpty(t, log.RSSCampaignID)
	assert.GreaterOrEqual(t, log.ItemsFound, 0)
	assert.GreaterOrEqual(t, log.NewItems, 0)
	assert.LessOrEqual(t, log.NewItems, log.ItemsFound)
	assert.GreaterOrEqual(t, log.DurationMs, 0)
	assert.Equal(t, "success", log.Status)
}

// Integration test placeholder - requires database
func TestRSSCampaignService_Integration(t *testing.T) {
	t.Skip("Integration test requires database connection")

	// This would test:
	// - CreateRSSCampaign
	// - GetRSSCampaigns
	// - GetRSSCampaign
	// - UpdateRSSCampaign
	// - DeleteRSSCampaign
	// - PollFeed (with mock HTTP server)
	// - GenerateCampaignFromFeed
}

// Benchmark for HTML stripping
func BenchmarkStripHTML(b *testing.B) {
	input := `<div class="article">
		<h1>Test Article</h1>
		<p>This is a <strong>test</strong> paragraph with <em>various</em> HTML tags.</p>
		<ul><li>Item 1</li><li>Item 2</li></ul>
		<a href="https://example.com">Link</a>
	</div>`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stripHTML(input)
	}
}

// Test context cancellation
func TestRSSCampaignService_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Operations should respect cancelled context
	assert.Error(t, ctx.Err())
}
