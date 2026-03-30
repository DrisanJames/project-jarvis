package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/engine"
)

// BlogCampaignInput is the minimal payload blog sites POST.
// The system expands it into a full PMTACampaignInput using per-brand config.
type BlogCampaignInput struct {
	SendingDomain string     `json:"sending_domain"`
	Subject       string     `json:"subject"`
	PreviewText   string     `json:"preview_text"`
	HTMLContent   string     `json:"html_content"`
	ScheduledAt   *time.Time `json:"scheduled_at,omitempty"`
}

type blogBrandConfig struct {
	BrandName    string
	FromName     string
	SeedListIDs  []string
	SegmentIDs   []string // 7D Openers, 14D Clickers
	TargetISPs   []engine.ISP
}

var blogBrandConfigs = map[string]blogBrandConfig{
	"em.discountblog.com": {
		BrandName: "Discount Blog",
		FromName:  "Jamie @ Discount Blog",
		SeedListIDs: []string{
			"4d2d35aa-0959-4798-8ea5-47dcae7ad77b", // Gmail Seeds
			"69490436-02cf-434e-aa11-005cac118d48", // Microsoft Seeds
			"9f02f764-833e-4467-b5f9-0a01d648c84b", // Yahoo Seeds
		},
		SegmentIDs: []string{
			"fee53e1a-c017-4545-92be-e31780103fa6", // 7D Openers
			"0fb158d9-6cf3-4778-84a2-b8384d0845b6", // 14D Clickers
		},
		TargetISPs: defaultBlogTargetISPs,
	},
	"em.quizfiesta.com": {
		BrandName: "Quiz Fiesta",
		FromName:  "Quiz Master",
		SeedListIDs: []string{
			"fd2d3a87-cc81-4266-ae09-1e67bdba8e94",
			"c50c018a-bcf1-4299-a9e6-1f32b3a018a0",
			"227a2cb4-18d3-4fb9-aa2a-c0fed2be300b",
		},
		SegmentIDs: []string{
			"016da7c1-a933-4666-b09f-b602879cfe40",
			"89585f01-a1ea-494e-8eee-754e5a7a0224",
		},
		TargetISPs: defaultBlogTargetISPs,
	},
	"em.historythinking.com": {
		BrandName: "History Thinking",
		FromName:  "History Thinking",
		SeedListIDs: []string{
			"8594ce85-a69f-4d4a-bf28-b631897e8d79",
			"0c5bda44-f732-4c9c-9ade-3a4884580c20",
			"b939d864-5f5d-42b4-966f-5347424660f6",
		},
		SegmentIDs: []string{
			"637fdd67-c16d-4ad2-baa3-33dc9e291b40",
			"eefe5963-22f8-4dc7-9210-6c099aaf4037",
		},
		TargetISPs: defaultBlogTargetISPs,
	},
	"em.myownhealth.net": {
		BrandName: "My Own Health",
		FromName:  "Arnold @ My Own Health",
		SeedListIDs: []string{
			"e3a1ba88-d561-43fe-bf3f-7afa88cc5661",
			"9cf97bd2-120a-482a-bd82-d7f5cb7452b0",
			"d442463a-4c0f-4a5c-beec-e201e2e92e53",
		},
		SegmentIDs: []string{
			"1aee0c64-64e5-437d-84ab-f5d061dbe7fe",
			"570941b4-1373-4c2c-9c59-f5f7bc901ff9",
		},
		TargetISPs: defaultBlogTargetISPs,
	},
}

var defaultBlogTargetISPs = []engine.ISP{
	"gmail", "yahoo", "microsoft", "apple",
	"comcast", "att", "cox", "charter",
}

// buildBlogCampaignInput expands a minimal blog payload into a full
// PMTACampaignInput. Pure function — no DB or HTTP dependency.
func buildBlogCampaignInput(input BlogCampaignInput) (engine.PMTACampaignInput, error) {
	domain := strings.TrimSpace(strings.ToLower(input.SendingDomain))
	cfg, ok := blogBrandConfigs[domain]
	if !ok {
		return engine.PMTACampaignInput{}, fmt.Errorf("unknown sending_domain %q — supported: em.discountblog.com, em.quizfiesta.com, em.historythinking.com, em.myownhealth.net", domain)
	}

	subject := strings.TrimSpace(input.Subject)
	if subject == "" {
		return engine.PMTACampaignInput{}, fmt.Errorf("subject is required")
	}
	html := strings.TrimSpace(input.HTMLContent)
	if html == "" {
		return engine.PMTACampaignInput{}, fmt.Errorf("html_content is required")
	}

	scheduledAt := input.ScheduledAt
	if scheduledAt == nil {
		mountain, err := time.LoadLocation("America/Boise")
		if err != nil {
			mountain = time.FixedZone("MST", -7*3600)
		}
		tomorrow := time.Now().In(mountain).AddDate(0, 0, 1)
		defaultTime := time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 8, 0, 0, 0, mountain).UTC()
		scheduledAt = &defaultTime
	}

	datePart := scheduledAt.Format("01022006")
	name := fmt.Sprintf("%s - %s - Engaged Audience", datePart, cfg.BrandName)

	return engine.PMTACampaignInput{
		Name:          name,
		SendingDomain: domain,
		TargetISPs:    cfg.TargetISPs,
		Variants: []engine.ContentVariant{{
			VariantName:  "A",
			FromName:     cfg.FromName,
			Subject:      subject,
			PreviewText:  strings.TrimSpace(input.PreviewText),
			HTMLContent:  html,
			SplitPercent: 100,
		}},
		InclusionLists:    cfg.SeedListIDs,
		InclusionSegments: cfg.SegmentIDs,
		ExclusionLists:    []string{"global-suppression-list"},
		SendMode:          "scheduled",
		ScheduledAt:       scheduledAt,
		Timezone:          "America/Boise",
		ThrottleStrategy:  "auto",
		RandomizeAudience: false,
		// ISPQuotas intentionally empty → Quota=0 → unlimited (no truncation)
	}, nil
}

// HandleBlogCampaign accepts a minimal JSON payload from blog sites and creates
// a fully configured engaged-audience campaign using per-brand defaults.
// The campaign is created in 'finalizing_audience' status and processed by
// the AudienceFinalizationWorker in the background.
func (s *PMTACampaignService) HandleBlogCampaign(w http.ResponseWriter, r *http.Request) {
	var input BlogCampaignInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	campaignInput, err := buildBlogCampaignInput(input)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	log.Printf("[blog-campaign] creating campaign %q for domain %s", campaignInput.Name, campaignInput.SendingDomain)

	ctx := r.Context()
	orgID := getOrgID(r)

	preflight := s.runPreflight(ctx, orgID, campaignInput.SendingDomain)
	if !preflight.OK {
		msgs := make([]string, len(preflight.Errors))
		for i, e := range preflight.Errors {
			msgs[i] = e.Check + ": " + e.Message
		}
		respondJSON(w, http.StatusBadRequest, map[string]string{
			"error": "preflight failed: " + strings.Join(msgs, "; "),
		})
		return
	}

	normalized, err := normalizePMTACampaignInput(campaignInput)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	campaignID, err := s.reserveCampaignForDeploy(ctx, orgID, campaignInput, normalized)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	log.Printf("[blog-campaign] queued %q campaign_id=%s for audience finalization", campaignInput.Name, campaignID)

	respondJSON(w, http.StatusAccepted, map[string]interface{}{
		"campaign_id":   campaignID,
		"name":          campaignInput.Name,
		"status":        "finalizing_audience",
		"target_isps":   campaignInput.TargetISPs,
		"variant_count": len(campaignInput.Variants),
	})
}
