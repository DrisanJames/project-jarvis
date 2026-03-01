package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

// Consciousness is the system's self-aware layer. It continuously consumes
// signals, convictions, and decisions, synthesizing them into higher-order
// "philosophies" — beliefs the system holds about each ISP and the overall
// sending ecosystem. Philosophies evolve over time as new evidence arrives.
type Consciousness struct {
	convictions *ConvictionStore
	processor   *SignalProcessor
	memory      *MemoryStore
	tracker     *CampaignEventTracker
	s3Client    *s3.Client
	bucket      string

	mu           sync.RWMutex
	philosophies map[string]*Philosophy // keyed by "{isp}/{domain}"
	thoughts     []Thought              // recent thought stream (ring buffer)
	maxThoughts  int

	subMu       sync.RWMutex
	subscribers map[string]chan Thought

	stopCh chan struct{}
}

// Philosophy is a higher-order belief the system holds about an ISP or domain.
// It emerges from accumulated convictions and observed patterns. Philosophies
// are mutable — they strengthen or weaken as evidence accumulates.
type Philosophy struct {
	ID          string    `json:"id"`
	ISP         ISP       `json:"isp"`
	Domain      string    `json:"domain"`
	Belief      string    `json:"belief"`
	Explanation string    `json:"explanation"`
	Confidence  float64   `json:"confidence"`
	Evidence    int       `json:"evidence_count"`
	Category    string    `json:"category"`
	Sentiment   string    `json:"sentiment"` // positive, negative, neutral, cautious
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Strength    float64   `json:"strength"` // 0-1, how strongly held
	Challenges  int       `json:"challenges"`
	Tags        []string  `json:"tags,omitempty"`
}

// Thought is a single conscious observation — the system narrating its own
// reasoning as it processes events and makes decisions. The thought stream
// is the "inner monologue" visible to the operator.
type Thought struct {
	ID         string    `json:"id"`
	Timestamp  time.Time `json:"timestamp"`
	ISP        ISP       `json:"isp,omitempty"`
	AgentType  AgentType `json:"agent_type,omitempty"`
	Type       string    `json:"type"` // observation, decision, reflection, warning, insight, philosophy_update
	Content    string    `json:"content"`
	Reasoning  string    `json:"reasoning,omitempty"`
	Confidence float64   `json:"confidence,omitempty"`
	Severity   string    `json:"severity,omitempty"` // info, caution, warning, critical
	RelatedIDs []string  `json:"related_ids,omitempty"`
}

// ConsciousnessState is the full exportable state of the consciousness.
type ConsciousnessState struct {
	Philosophies []Philosophy `json:"philosophies"`
	Thoughts     []Thought    `json:"recent_thoughts"`
	Summary      string       `json:"summary"`
	HealthScore  float64      `json:"health_score"`
	Mood         string       `json:"mood"` // confident, cautious, concerned, alert
	ActiveISPs   int          `json:"active_isps"`
	TotalBeliefs int          `json:"total_beliefs"`
	GeneratedAt  time.Time    `json:"generated_at"`
}

// NewConsciousness creates the consciousness layer.
func NewConsciousness(
	convictions *ConvictionStore,
	processor *SignalProcessor,
	memory *MemoryStore,
	s3Client *s3.Client,
	bucket string,
) *Consciousness {
	return &Consciousness{
		convictions:  convictions,
		processor:    processor,
		memory:       memory,
		s3Client:     s3Client,
		bucket:       bucket,
		philosophies: make(map[string]*Philosophy),
		maxThoughts:  500,
		subscribers:  make(map[string]chan Thought),
		stopCh:       make(chan struct{}),
	}
}

// SetCampaignTracker links the campaign event tracker for campaign-aware thoughts.
func (c *Consciousness) SetCampaignTracker(t *CampaignEventTracker) {
	c.tracker = t
}

// Start begins the consciousness loops: conviction observation, signal
// reflection, campaign monitoring, and periodic philosophy synthesis.
func (c *Consciousness) Start(ctx context.Context) {
	// Restore persisted state from S3 before starting live loops
	c.restoreState(ctx)

	convCh := c.convictions.Subscribe("consciousness")
	go c.observeConvictions(ctx, convCh)

	signalCh := make(chan SignalSnapshot, 100)
	c.processor.Subscribe(signalCh)
	go c.reflectOnSignals(ctx, signalCh)

	go c.synthesisLoop(ctx)
	go c.persistLoop(ctx)
	go c.campaignMonitorLoop(ctx)

	c.think(Thought{
		Type:     "observation",
		Content:  "Consciousness online. Observing conviction stream, signal snapshots, and campaign events.",
		Severity: "info",
	})

	// Immediately reflect on any existing campaigns
	go func() {
		time.Sleep(5 * time.Second)
		c.reflectOnCampaigns()
	}()

	log.Println("[consciousness] started — observing convictions, signals, and campaigns")
}

func (c *Consciousness) restoreState(ctx context.Context) {
	if c.s3Client == nil {
		return
	}
	key := "consciousness/state.json"
	result, err := c.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		log.Printf("[consciousness] no persisted state to restore: %v", err)
		return
	}
	defer result.Body.Close()

	var saved ConsciousnessState
	if err := json.NewDecoder(result.Body).Decode(&saved); err != nil {
		log.Printf("[consciousness] failed to decode persisted state: %v", err)
		return
	}

	c.mu.Lock()
	for _, p := range saved.Philosophies {
		p := p
		c.philosophies[fmt.Sprintf("%s/%s", p.ISP, p.Domain)] = &p
	}
	c.thoughts = saved.Thoughts
	if len(c.thoughts) > c.maxThoughts {
		c.thoughts = c.thoughts[len(c.thoughts)-c.maxThoughts:]
	}
	c.mu.Unlock()

	log.Printf("[consciousness] restored %d philosophies and %d thoughts from S3", len(saved.Philosophies), len(saved.Thoughts))
}

// Stop terminates the consciousness.
func (c *Consciousness) Stop() {
	close(c.stopCh)
	c.convictions.Unsubscribe("consciousness")
}

// Subscribe registers a listener for the thought stream.
func (c *Consciousness) Subscribe(id string) <-chan Thought {
	ch := make(chan Thought, 100)
	c.subMu.Lock()
	c.subscribers[id] = ch
	c.subMu.Unlock()
	return ch
}

// Unsubscribe removes a thought stream listener.
func (c *Consciousness) Unsubscribe(id string) {
	c.subMu.Lock()
	if ch, ok := c.subscribers[id]; ok {
		close(ch)
		delete(c.subscribers, id)
	}
	c.subMu.Unlock()
}

func (c *Consciousness) fanOutThought(t Thought) {
	c.subMu.RLock()
	defer c.subMu.RUnlock()
	for _, ch := range c.subscribers {
		select {
		case ch <- t:
		default:
		}
	}
}

func (c *Consciousness) think(t Thought) {
	if t.ID == "" {
		t.ID = uuid.New().String()
	}
	if t.Timestamp.IsZero() {
		t.Timestamp = time.Now()
	}

	c.mu.Lock()
	c.thoughts = append(c.thoughts, t)
	if len(c.thoughts) > c.maxThoughts {
		c.thoughts = c.thoughts[len(c.thoughts)-c.maxThoughts:]
	}
	c.mu.Unlock()

	c.fanOutThought(t)
}

func (c *Consciousness) observeConvictions(ctx context.Context, ch <-chan Conviction) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case conv := <-ch:
			c.processConviction(conv)
		}
	}
}

func (c *Consciousness) processConviction(conv Conviction) {
	severity := "info"
	if conv.Verdict == VerdictWont {
		severity = "caution"
	}

	c.think(Thought{
		Type:       "observation",
		ISP:        conv.ISP,
		AgentType:  conv.AgentType,
		Content:    fmt.Sprintf("[%s/%s] %s: %s", conv.ISP, conv.AgentType, strings.ToUpper(string(conv.Verdict)), conv.Statement),
		Confidence: conv.Confidence,
		Severity:   severity,
		RelatedIDs: []string{conv.ID},
	})

	c.updatePhilosophyFromConviction(conv)
}

func (c *Consciousness) updatePhilosophyFromConviction(conv Conviction) {
	key := fmt.Sprintf("%s/%s", conv.ISP, conv.AgentType)

	c.mu.Lock()
	defer c.mu.Unlock()

	p, exists := c.philosophies[key]
	if !exists {
		p = &Philosophy{
			ID:        uuid.New().String(),
			ISP:       conv.ISP,
			Domain:    string(conv.AgentType),
			Category:  categoryForAgent(conv.AgentType),
			CreatedAt: time.Now(),
			Tags:      []string{string(conv.AgentType), string(conv.ISP)},
		}
		c.philosophies[key] = p
	}

	p.Evidence++
	p.UpdatedAt = time.Now()

	// Compute belief direction based on conviction history
	all := c.convictions.RecallAll(conv.ISP, conv.AgentType)
	willCount, wontCount := 0, 0
	var totalConf float64
	for _, cc := range all {
		if cc.Verdict == VerdictWill {
			willCount++
		} else {
			wontCount++
		}
		totalConf += cc.Confidence
	}
	total := willCount + wontCount
	if total == 0 {
		return
	}
	avgConf := totalConf / float64(total)

	// Synthesize belief
	willRatio := float64(willCount) / float64(total)
	p.Confidence = avgConf
	p.Strength = math.Abs(willRatio - 0.5) * 2

	switch {
	case willRatio >= 0.8:
		p.Sentiment = "positive"
		p.Belief = synthesizeBelief(conv.ISP, conv.AgentType, "positive", willCount, wontCount, all)
	case willRatio >= 0.6:
		p.Sentiment = "cautious"
		p.Belief = synthesizeBelief(conv.ISP, conv.AgentType, "cautious", willCount, wontCount, all)
	case willRatio >= 0.4:
		p.Sentiment = "neutral"
		p.Belief = synthesizeBelief(conv.ISP, conv.AgentType, "neutral", willCount, wontCount, all)
	case willRatio >= 0.2:
		p.Sentiment = "cautious"
		p.Belief = synthesizeBelief(conv.ISP, conv.AgentType, "negative-cautious", willCount, wontCount, all)
	default:
		p.Sentiment = "negative"
		p.Belief = synthesizeBelief(conv.ISP, conv.AgentType, "negative", willCount, wontCount, all)
	}

	p.Explanation = synthesizeExplanation(conv.ISP, conv.AgentType, willCount, wontCount, all)
}

func (c *Consciousness) reflectOnSignals(ctx context.Context, ch <-chan SignalSnapshot) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case snap := <-ch:
			c.reflectOnSnapshot(snap)
		}
	}
}

func (c *Consciousness) reflectOnSnapshot(snap SignalSnapshot) {
	if snap.BounceRate1h > 10 {
		c.think(Thought{
			Type:    "warning",
			ISP:     snap.ISP,
			Content: fmt.Sprintf("Elevated bounce rate for %s: %.1f%% (1h). Reputation impact likely.", snap.ISP, snap.BounceRate1h),
			Severity: "warning",
		})
	}
	if snap.ComplaintRate1h > 0.5 {
		c.think(Thought{
			Type:    "warning",
			ISP:     snap.ISP,
			Content: fmt.Sprintf("Complaint rate for %s at %.2f%%. This exceeds safe thresholds — throttling recommended.", snap.ISP, snap.ComplaintRate1h),
			Severity: "critical",
		})
	}
	if snap.DeferralRate5m > 30 {
		c.think(Thought{
			Type:    "observation",
			ISP:     snap.ISP,
			Content: fmt.Sprintf("%s is deferring %.1f%% of messages (5m). The ISP may be rate-limiting us.", snap.ISP, snap.DeferralRate5m),
			Severity: "caution",
		})
	}

	// Positive observations
	if snap.Sent1h > 100 && snap.BounceRate1h < 1 && snap.ComplaintRate1h < 0.05 && snap.DeferralRate5m < 5 {
		c.think(Thought{
			Type:       "insight",
			ISP:        snap.ISP,
			Content:    fmt.Sprintf("%s ecosystem healthy. %d sent, %.1f%% bounce, %.2f%% complaint. Conditions favorable for volume increase.", snap.ISP, snap.Sent1h, snap.BounceRate1h, snap.ComplaintRate1h),
			Confidence: 0.9,
			Severity:   "info",
		})
	}
}

func (c *Consciousness) synthesisLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.synthesizePhilosophies()
		}
	}
}

func (c *Consciousness) synthesizePhilosophies() {
	for _, isp := range AllISPs() {
		for _, at := range AllAgentTypes() {
			key := fmt.Sprintf("%s/%s", isp, at)
			c.mu.RLock()
			p, exists := c.philosophies[key]
			c.mu.RUnlock()
			if !exists || p.Evidence < 3 {
				continue
			}

			// Check for contradictions in recent convictions
			recent := c.convictions.RecallRecent(isp, at, 20)
			if len(recent) < 5 {
				continue
			}

			recentWill := 0
			for _, cc := range recent {
				if cc.Verdict == VerdictWill {
					recentWill++
				}
			}
			recentRatio := float64(recentWill) / float64(len(recent))

			c.mu.Lock()
			// Detect shifting beliefs
			var overallRatio float64
			all := c.convictions.RecallAll(isp, at)
			wc, _ := 0, 0
			for _, cc := range all {
				if cc.Verdict == VerdictWill {
					wc++
				}
			}
			if len(all) > 0 {
				overallRatio = float64(wc) / float64(len(all))
			}

			shift := math.Abs(recentRatio - overallRatio)
			if shift > 0.3 {
				p.Challenges++
				direction := "positive"
				if recentRatio < overallRatio {
					direction = "negative"
				}
				c.mu.Unlock()

				c.think(Thought{
					Type:      "reflection",
					ISP:       isp,
					AgentType: at,
					Content: fmt.Sprintf("Belief shift detected for %s/%s. Recent trend is %s (%.0f%% will) vs historical (%.0f%% will). Philosophy may need revision.",
						isp, at, direction, recentRatio*100, overallRatio*100),
					Reasoning: fmt.Sprintf("Analyzed %d recent and %d total convictions. Shift magnitude: %.0f%%", len(recent), len(all), shift*100),
					Severity:  "caution",
				})
			} else {
				c.mu.Unlock()
			}
		}
	}
}

func (c *Consciousness) campaignMonitorLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.reflectOnCampaigns()
		}
	}
}

func (c *Consciousness) reflectOnCampaigns() {
	if c.tracker == nil {
		return
	}

	campaigns := c.tracker.GetAllCampaigns()
	for _, cm := range campaigns {
		if cm.Sent < 10 {
			continue
		}

		bounceRate := float64(cm.SoftBounce+cm.HardBounce) / float64(cm.Sent) * 100
		complaintRate := float64(cm.Complaints) / float64(cm.Sent) * 100

		if cm.HardBounce > 0 && bounceRate > 5 {
			c.think(Thought{
				Type:    "warning",
				Content: fmt.Sprintf("Campaign %s has %.1f%% bounce rate (%d hard, %d soft out of %d sent). List quality may need review.", cm.CampaignID, bounceRate, cm.HardBounce, cm.SoftBounce, cm.Sent),
				Severity: "warning",
			})
		}

		if complaintRate > 0.1 {
			c.think(Thought{
				Type:    "warning",
				Content: fmt.Sprintf("Campaign %s complaint rate at %.2f%% (%d complaints). This exceeds the 0.1%% safe threshold.", cm.CampaignID, complaintRate, cm.Complaints),
				Severity: "critical",
			})
		}

		if cm.Inactive > 0 {
			inactiveRate := float64(cm.Inactive) / float64(cm.Sent) * 100
			if inactiveRate > 5 {
				c.think(Thought{
					Type:    "insight",
					Content: fmt.Sprintf("Campaign %s has %d inactive recipients (%.1f%%) — sent 4+ times with zero engagement. Recommend suppression.", cm.CampaignID, cm.Inactive, inactiveRate),
					Severity: "caution",
				})
			}
		}

		if cm.Delivered > 100 && cm.UniqueOpens > 0 {
			openRate := float64(cm.UniqueOpens) / float64(cm.Delivered) * 100
			clickRate := float64(cm.UniqueClicks) / float64(cm.Delivered) * 100
			if openRate > 20 && bounceRate < 2 && complaintRate < 0.05 {
				c.think(Thought{
					Type:       "insight",
					Content:    fmt.Sprintf("Campaign %s performing well: %.1f%% open rate, %.1f%% click rate, %.1f%% bounce. Strong engagement signals.", cm.CampaignID, openRate, clickRate, bounceRate),
					Confidence: 0.85,
					Severity:   "info",
				})
			}
		}
	}
}

func (c *Consciousness) persistLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.persistState(ctx)
		}
	}
}

func (c *Consciousness) persistState(ctx context.Context) {
	if c.s3Client == nil {
		return
	}

	state := c.GetState()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		log.Printf("[consciousness] marshal error: %v", err)
		return
	}

	key := "consciousness/state.json"
	_, err = c.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		log.Printf("[consciousness] persist error: %v", err)
		return
	}

	// Also persist thought stream
	thoughtData, _ := json.MarshalIndent(state.Thoughts, "", "  ")
	thoughtKey := fmt.Sprintf("consciousness/thoughts/%s.json", time.Now().Format("2006-01-02"))
	c.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(thoughtKey),
		Body:        bytes.NewReader(thoughtData),
		ContentType: aws.String("application/json"),
	})
}

// GetState returns the full consciousness state.
func (c *Consciousness) GetState() ConsciousnessState {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var philosophies []Philosophy
	for _, p := range c.philosophies {
		philosophies = append(philosophies, *p)
	}
	sort.Slice(philosophies, func(i, j int) bool {
		return philosophies[i].Strength > philosophies[j].Strength
	})

	thoughts := make([]Thought, len(c.thoughts))
	copy(thoughts, c.thoughts)

	mood, health := c.computeMood(philosophies)

	return ConsciousnessState{
		Philosophies: philosophies,
		Thoughts:     thoughts,
		Summary:      c.generateSummary(philosophies),
		HealthScore:  health,
		Mood:         mood,
		ActiveISPs:   c.countActiveISPs(philosophies),
		TotalBeliefs: len(philosophies),
		GeneratedAt:  time.Now(),
	}
}

// GetPhilosophies returns all current philosophies.
func (c *Consciousness) GetPhilosophies() []Philosophy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var result []Philosophy
	for _, p := range c.philosophies {
		result = append(result, *p)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Strength > result[j].Strength
	})
	return result
}

// GetPhilosophiesByISP returns philosophies for a specific ISP.
func (c *Consciousness) GetPhilosophiesByISP(isp ISP) []Philosophy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var result []Philosophy
	for _, p := range c.philosophies {
		if p.ISP == isp {
			result = append(result, *p)
		}
	}
	return result
}

// GetRecentThoughts returns the N most recent thoughts.
func (c *Consciousness) GetRecentThoughts(n int) []Thought {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if n >= len(c.thoughts) {
		result := make([]Thought, len(c.thoughts))
		copy(result, c.thoughts)
		return result
	}
	result := make([]Thought, n)
	copy(result, c.thoughts[len(c.thoughts)-n:])
	return result
}

func (c *Consciousness) computeMood(philosophies []Philosophy) (string, float64) {
	if len(philosophies) == 0 {
		return "observing", 50.0
	}

	positiveCount := 0
	negativeCount := 0
	totalStrength := 0.0
	for _, p := range philosophies {
		totalStrength += p.Strength
		switch p.Sentiment {
		case "positive":
			positiveCount++
		case "negative":
			negativeCount++
		}
	}

	ratio := float64(positiveCount) / float64(len(philosophies))
	health := ratio * 100

	switch {
	case negativeCount > len(philosophies)/2:
		return "concerned", health
	case ratio < 0.3:
		return "alert", health
	case ratio > 0.7:
		return "confident", health
	default:
		return "cautious", health
	}
}

func (c *Consciousness) generateSummary(philosophies []Philosophy) string {
	if len(philosophies) == 0 {
		return "No beliefs formed yet. Collecting observations."
	}

	positive, negative, cautious := 0, 0, 0
	for _, p := range philosophies {
		switch p.Sentiment {
		case "positive":
			positive++
		case "negative":
			negative++
		case "cautious":
			cautious++
		}
	}

	parts := []string{fmt.Sprintf("Holding %d beliefs across %d ISPs.", len(philosophies), c.countActiveISPs(philosophies))}
	if positive > 0 {
		parts = append(parts, fmt.Sprintf("%d positive,", positive))
	}
	if cautious > 0 {
		parts = append(parts, fmt.Sprintf("%d cautious,", cautious))
	}
	if negative > 0 {
		parts = append(parts, fmt.Sprintf("%d concerning.", negative))
	}

	return strings.Join(parts, " ")
}

func (c *Consciousness) countActiveISPs(philosophies []Philosophy) int {
	isps := make(map[ISP]bool)
	for _, p := range philosophies {
		isps[p.ISP] = true
	}
	return len(isps)
}

// --- Philosophy synthesis helpers ---

func categoryForAgent(at AgentType) string {
	switch at {
	case AgentReputation:
		return "reputation_management"
	case AgentThrottle:
		return "throughput_optimization"
	case AgentPool:
		return "ip_health"
	case AgentWarmup:
		return "ip_warming"
	case AgentEmergency:
		return "emergency_response"
	case AgentSuppression:
		return "list_hygiene"
	default:
		return "general"
	}
}

func synthesizeBelief(isp ISP, at AgentType, sentiment string, will, wont int, all []Conviction) string {
	total := will + wont
	ispName := string(isp)

	switch at {
	case AgentReputation:
		switch sentiment {
		case "positive":
			return fmt.Sprintf("%s accepts our traffic reliably. Reputation is strong with %d/%d favorable observations.", ispName, will, total)
		case "cautious":
			return fmt.Sprintf("%s is generally receptive but shows intermittent sensitivity. Monitor bounce patterns.", ispName)
		case "neutral":
			return fmt.Sprintf("%s has mixed signals on reputation. Need more data to establish clear pattern.", ispName)
		case "negative-cautious":
			return fmt.Sprintf("%s is showing resistance. Bounce rates have triggered multiple interventions.", ispName)
		case "negative":
			return fmt.Sprintf("%s is actively rejecting significant traffic. Reputation repair needed before scaling.", ispName)
		}

	case AgentThrottle:
		switch sentiment {
		case "positive":
			return fmt.Sprintf("%s handles our sending rates well. Current throughput is sustainable.", ispName)
		case "cautious":
			return fmt.Sprintf("%s occasionally pushes back on rate. Conservative pacing recommended.", ispName)
		case "negative":
			return fmt.Sprintf("%s is rate-limiting aggressively. Significant backoff required.", ispName)
		default:
			return fmt.Sprintf("%s throughput behavior is variable. Adapting to patterns.", ispName)
		}

	case AgentWarmup:
		switch sentiment {
		case "positive":
			return fmt.Sprintf("IP warmup for %s is progressing well. ISP accepting volume increases.", ispName)
		case "negative":
			return fmt.Sprintf("IP warmup for %s is stalled. ISP not accepting volume ramps.", ispName)
		default:
			return fmt.Sprintf("IP warmup for %s progressing with caution.", ispName)
		}

	case AgentPool:
		switch sentiment {
		case "positive":
			return fmt.Sprintf("IP health in %s pool is strong. All IPs scoring well.", ispName)
		case "negative":
			return fmt.Sprintf("Multiple IPs degraded in %s pool. Quarantine activity elevated.", ispName)
		default:
			return fmt.Sprintf("IP pool for %s shows mixed health signals.", ispName)
		}

	case AgentEmergency:
		if wont > 0 {
			return fmt.Sprintf("Emergency conditions have occurred %d times for %s. Vigilance required.", wont, ispName)
		}
		return fmt.Sprintf("No emergency conditions observed for %s. Infrastructure stable.", ispName)

	case AgentSuppression:
		switch sentiment {
		case "positive":
			return fmt.Sprintf("List hygiene for %s is excellent. Low suppression rate indicates clean data.", ispName)
		case "negative":
			return fmt.Sprintf("High suppression rate for %s. Data quality issues detected.", ispName)
		default:
			return fmt.Sprintf("Suppression activity for %s is within expected bounds.", ispName)
		}
	}

	return fmt.Sprintf("Observing %s behavior for %s. %d/%d favorable outcomes.", ispName, at, will, total)
}

func synthesizeExplanation(isp ISP, at AgentType, will, wont int, all []Conviction) string {
	total := will + wont
	if total == 0 {
		return "Insufficient data."
	}

	willPct := float64(will) / float64(total) * 100
	var parts []string
	parts = append(parts, fmt.Sprintf("Based on %d observations: %.0f%% favorable, %.0f%% unfavorable.", total, willPct, 100-willPct))

	// Analyze recent trend
	recentCount := 10
	if recentCount > len(all) {
		recentCount = len(all)
	}
	recent := all[len(all)-recentCount:]
	recentWill := 0
	for _, cc := range recent {
		if cc.Verdict == VerdictWill {
			recentWill++
		}
	}
	recentPct := float64(recentWill) / float64(len(recent)) * 100
	if recentPct > willPct+10 {
		parts = append(parts, "Recent trend is improving.")
	} else if recentPct < willPct-10 {
		parts = append(parts, "Recent trend shows deterioration.")
	} else {
		parts = append(parts, "Recent trend is stable.")
	}

	// Look for common DSN codes in WONT convictions
	dsnFreq := make(map[string]int)
	for _, cc := range all {
		if cc.Verdict == VerdictWont {
			for _, code := range cc.Context.DSNCodes {
				dsnFreq[code]++
			}
		}
	}
	if len(dsnFreq) > 0 {
		type kv struct {
			Code  string
			Count int
		}
		var pairs []kv
		for c, n := range dsnFreq {
			pairs = append(pairs, kv{c, n})
		}
		sort.Slice(pairs, func(i, j int) bool { return pairs[i].Count > pairs[j].Count })
		if len(pairs) > 3 {
			pairs = pairs[:3]
		}
		codes := make([]string, len(pairs))
		for i, p := range pairs {
			codes[i] = p.Code
		}
		parts = append(parts, fmt.Sprintf("Common rejection codes: %s.", strings.Join(codes, ", ")))
	}

	return strings.Join(parts, " ")
}
