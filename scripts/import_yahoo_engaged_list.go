// +build ignore

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// Configuration - all values from environment with sensible defaults
var (
	orgID            = getEnvOrDefault("ORG_ID", "")
	csvFilePath      = getEnvOrDefault("CSV_FILE_PATH", "")
	batchSize        = getEnvIntOrDefault("BATCH_SIZE", 5000)
	sendingProfileID = getEnvOrDefault("SENDING_PROFILE_ID", "")
)

func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvIntOrDefault(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		var i int
		fmt.Sscanf(val, "%d", &i)
		if i > 0 {
			return i
		}
	}
	return defaultVal
}

// Results structure
type ImportResults struct {
	ListID           uuid.UUID   `json:"list_id"`
	SegmentID        uuid.UUID   `json:"segment_id"`
	CampaignID       uuid.UUID   `json:"campaign_id"`
	TemplateID       uuid.UUID   `json:"template_id"`
	ABTestID         uuid.UUID   `json:"ab_test_id"`
	ABVariantIDs     []uuid.UUID `json:"ab_variant_ids"`
	CustomFieldIDs   []uuid.UUID `json:"custom_field_ids"`
	TotalImported    int         `json:"total_imported"`
	SegmentCount     int         `json:"segment_count"`
}

func main() {
	// Database connection
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://ignite:ignite_dev_password@localhost:5432/ignite?sslmode=disable"
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	// Test connection
	if err := db.Ping(); err != nil {
		log.Fatalf("Failed to ping database: %v", err)
	}

	ctx := context.Background()
	results := &ImportResults{}

	fmt.Println("🚀 Starting Yahoo Highly Engaged List Import...")
	fmt.Printf("📁 CSV File: %s\n", csvFilePath)
	fmt.Printf("🏢 Organization ID: %s\n\n", orgID)

	// Step 1: Create Custom Field Definitions
	fmt.Println("📋 Step 1: Creating Custom Field Definitions...")
	if err := createCustomFieldDefinitions(ctx, db, results); err != nil {
		log.Fatalf("Failed to create custom fields: %v", err)
	}

	// Step 2: Create Mailing List
	fmt.Println("\n📧 Step 2: Creating Mailing List...")
	if err := createMailingList(ctx, db, results); err != nil {
		log.Fatalf("Failed to create mailing list: %v", err)
	}

	// Step 3: Import CSV Data
	fmt.Println("\n📥 Step 3: Importing CSV Data...")
	if err := importCSVData(ctx, db, results); err != nil {
		log.Fatalf("Failed to import CSV: %v", err)
	}

	// Step 4: Create Segment
	fmt.Println("\n🎯 Step 4: Creating Segment...")
	if err := createSegment(ctx, db, results); err != nil {
		log.Fatalf("Failed to create segment: %v", err)
	}

	// Step 5: Get/Create Template and Create Campaign with A/B Variants
	fmt.Println("\n📝 Step 5: Creating Campaign with A/B Test...")
	if err := createCampaignWithABTest(ctx, db, results); err != nil {
		log.Fatalf("Failed to create campaign: %v", err)
	}

	// Step 6: Enable AI Smart Sending
	fmt.Println("\n🤖 Step 6: Enabling AI Smart Sending...")
	if err := enableAISmartSending(ctx, db, results); err != nil {
		log.Fatalf("Failed to enable AI smart sending: %v", err)
	}

	// Print final results
	printResults(results)
}

func createCustomFieldDefinitions(ctx context.Context, db *sql.DB, results *ImportResults) error {
	customFields := []struct {
		Name        string
		DisplayName string
		FieldType   string
		Description string
		EnumValues  *string
	}{
		{"is_role_address", "Is Role Address", "boolean", "Indicates if email is a role address", nil},
		{"is_disposable_address", "Is Disposable Address", "boolean", "Indicates if email is from a disposable domain", nil},
		{"did_you_mean", "Did You Mean", "string", "Suggested email correction", nil},
		{"result", "Validation Result", "string", "Email validation result", nil},
		{"reason", "Validation Reason", "string", "Detailed validation reason", nil},
		{"risk", "Risk Level", "string", "Email risk level", nil},
		{"root_address", "Root Address", "string", "Root email address", nil},
		{"engaging", "Is Engaging", "boolean", "Indicates if subscriber is engaging", nil},
		{"is_bot", "Is Bot", "boolean", "Indicates if email belongs to a bot", nil},
		{"engagement_behavior", "Engagement Behavior", "enum", "Engagement classification", ptr(`["highly_engaged", "engager", "disengaged", "complainer", "no_data"]`)},
	}

	results.CustomFieldIDs = make([]uuid.UUID, 0)

	for _, cf := range customFields {
		var id uuid.UUID
		var enumValues interface{} = nil
		if cf.EnumValues != nil {
			enumValues = *cf.EnumValues
		}

		err := db.QueryRowContext(ctx, `
			INSERT INTO mailing_custom_field_definitions (
				organization_id, name, display_name, field_type, description, enum_values
			) VALUES ($1, $2, $3, $4, $5, $6::jsonb)
			ON CONFLICT (organization_id, name) DO UPDATE SET
				display_name = EXCLUDED.display_name,
				field_type = EXCLUDED.field_type,
				description = EXCLUDED.description,
				enum_values = EXCLUDED.enum_values,
				updated_at = NOW()
			RETURNING id
		`, orgID, cf.Name, cf.DisplayName, cf.FieldType, cf.Description, enumValues).Scan(&id)
		
		if err != nil {
			return fmt.Errorf("failed to create custom field %s: %w", cf.Name, err)
		}
		results.CustomFieldIDs = append(results.CustomFieldIDs, id)
		fmt.Printf("   ✓ Created: %s (%s)\n", cf.DisplayName, id)
	}

	return nil
}

func createMailingList(ctx context.Context, db *sql.DB, results *ImportResults) error {
	listName := "Yahoo Highly Engaged - Mailgun Validations"
	listDesc := "List imported from ignite-test.csv with Mailgun validation data - 182,834 records"
	
	// First try to find existing list
	err := db.QueryRowContext(ctx, `
		SELECT id FROM mailing_lists 
		WHERE organization_id = $1 AND name = $2
	`, orgID, listName).Scan(&results.ListID)
	
	if err == sql.ErrNoRows {
		// Create new list
		listID := uuid.New()
		_, err = db.ExecContext(ctx, `
			INSERT INTO mailing_lists (
				id, organization_id, name, description, subscriber_count, status
			) VALUES ($1, $2, $3, $4, 0, 'active')
		`, listID, orgID, listName, listDesc)
		
		if err != nil {
			return fmt.Errorf("failed to insert list: %w", err)
		}
		results.ListID = listID
		fmt.Printf("   ✓ Created List ID: %s\n", results.ListID)
	} else if err != nil {
		return fmt.Errorf("failed to check existing list: %w", err)
	} else {
		// Update existing list description
		_, _ = db.ExecContext(ctx, `
			UPDATE mailing_lists SET description = $1, updated_at = NOW() WHERE id = $2
		`, listDesc, results.ListID)
		fmt.Printf("   ✓ Using existing List ID: %s\n", results.ListID)
	}

	return nil
}

func importCSVData(ctx context.Context, db *sql.DB, results *ImportResults) error {
	file, err := os.Open(csvFilePath)
	if err != nil {
		return fmt.Errorf("failed to open CSV: %w", err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	
	// Read header
	header, err := reader.Read()
	if err != nil {
		return fmt.Errorf("failed to read CSV header: %w", err)
	}

	// Create header index map
	headerIndex := make(map[string]int)
	for i, h := range header {
		headerIndex[strings.TrimSpace(strings.ToLower(h))] = i
	}

	// Verify required columns exist
	addressIdx, ok := headerIndex["address"]
	if !ok {
		return fmt.Errorf("CSV missing required 'address' column")
	}

	totalImported := 0
	batch := make([][]string, 0, batchSize)
	batchNum := 0

	startTime := time.Now()

	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("Warning: error reading row %d: %v", totalImported+1, err)
			continue
		}

		batch = append(batch, record)

		if len(batch) >= batchSize {
			batchNum++
			imported, err := insertBatch(ctx, db, results.ListID, batch, headerIndex, addressIdx)
			if err != nil {
				log.Printf("Warning: batch %d failed: %v", batchNum, err)
			} else {
				totalImported += imported
				elapsed := time.Since(startTime)
				rate := float64(totalImported) / elapsed.Seconds()
				fmt.Printf("   📦 Batch %d: %d records (Total: %d, Rate: %.0f/sec)\n", 
					batchNum, imported, totalImported, rate)
			}
			batch = batch[:0]
		}
	}

	// Insert remaining records
	if len(batch) > 0 {
		batchNum++
		imported, err := insertBatch(ctx, db, results.ListID, batch, headerIndex, addressIdx)
		if err != nil {
			log.Printf("Warning: final batch failed: %v", err)
		} else {
			totalImported += imported
		}
		fmt.Printf("   📦 Batch %d: %d records (Total: %d)\n", batchNum, imported, totalImported)
	}

	results.TotalImported = totalImported
	
	// Update list subscriber count
	_, err = db.ExecContext(ctx, `
		UPDATE mailing_lists 
		SET subscriber_count = $1, 
		    active_count = $1,
		    updated_at = NOW()
		WHERE id = $2
	`, totalImported, results.ListID)
	
	if err != nil {
		log.Printf("Warning: failed to update list count: %v", err)
	}

	fmt.Printf("\n   ✓ Total imported: %d records in %v\n", totalImported, time.Since(startTime).Round(time.Second))
	return nil
}

func insertBatch(ctx context.Context, db *sql.DB, listID uuid.UUID, batch [][]string, headerIndex map[string]int, addressIdx int) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO mailing_subscribers (
			organization_id, list_id, email, email_hash, status, custom_fields, source
		) VALUES ($1, $2, $3, $4, 'confirmed', $5::jsonb, 'csv_import')
		ON CONFLICT (list_id, email) DO UPDATE SET
			custom_fields = EXCLUDED.custom_fields,
			updated_at = NOW()
	`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	inserted := 0
	for _, row := range batch {
		if len(row) <= addressIdx {
			continue
		}

		email := strings.TrimSpace(strings.ToLower(row[addressIdx]))
		if email == "" || !strings.Contains(email, "@") {
			continue
		}

		emailHash := hashEmail(email)
		customFields := buildCustomFields(row, headerIndex)

		_, err := stmt.ExecContext(ctx, orgID, listID, email, emailHash, customFields)
		if err != nil {
			log.Printf("Warning: failed to insert %s: %v", email, err)
			continue
		}
		inserted++
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	return inserted, nil
}

func buildCustomFields(row []string, headerIndex map[string]int) string {
	fields := make(map[string]interface{})

	// Boolean fields
	boolFields := []string{"is_role_address", "is_disposable_address", "engaging", "is_bot"}
	for _, f := range boolFields {
		if idx, ok := headerIndex[f]; ok && idx < len(row) {
			val := strings.TrimSpace(strings.ToUpper(row[idx]))
			fields[f] = val == "TRUE" || val == "1" || val == "YES"
		}
	}

	// String fields
	stringFields := []string{"did_you_mean", "result", "reason", "risk", "root_address", "engagement_behavior"}
	for _, f := range stringFields {
		if idx, ok := headerIndex[f]; ok && idx < len(row) {
			val := strings.TrimSpace(row[idx])
			if val != "" {
				fields[f] = val
			}
		}
	}

	jsonBytes, _ := json.Marshal(fields)
	return string(jsonBytes)
}

func hashEmail(email string) string {
	hash := sha256.Sum256([]byte(strings.ToLower(email)))
	return hex.EncodeToString(hash[:])
}

func createSegment(ctx context.Context, db *sql.DB, results *ImportResults) error {
	segmentID := uuid.New()

	conditions := `[{"field": "engagement_behavior", "operator": "equals", "value": "highly_engaged", "source": "custom_field"}]`

	err := db.QueryRowContext(ctx, `
		INSERT INTO mailing_segments (
			id, organization_id, list_id, name, description, segment_type, conditions, subscriber_count, status
		) VALUES ($1, $2, $3, $4, $5, 'dynamic', $6::jsonb, 0, 'active')
		ON CONFLICT DO NOTHING
		RETURNING id
	`, segmentID, orgID, results.ListID,
		"YAHOO HIGHLY ENGAGED MAILGUN VALIDATIONS",
		"Highly engaged Yahoo users from Mailgun validation",
		conditions,
	).Scan(&results.SegmentID)

	if err != nil {
		// Try to get existing
		err = db.QueryRowContext(ctx, `
			SELECT id FROM mailing_segments 
			WHERE organization_id = $1 AND list_id = $2 AND name = $3
		`, orgID, results.ListID, "YAHOO HIGHLY ENGAGED MAILGUN VALIDATIONS").Scan(&results.SegmentID)
		if err != nil {
			return fmt.Errorf("failed to create/get segment: %w", err)
		}
	}

	// Calculate segment count
	var segmentCount int
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM mailing_subscribers 
		WHERE list_id = $1 
		AND custom_fields->>'engagement_behavior' = 'highly_engaged'
	`, results.ListID).Scan(&segmentCount)
	
	if err != nil {
		log.Printf("Warning: failed to count segment: %v", err)
	} else {
		results.SegmentCount = segmentCount
		_, _ = db.ExecContext(ctx, `
			UPDATE mailing_segments SET subscriber_count = $1, last_calculated_at = NOW() WHERE id = $2
		`, segmentCount, results.SegmentID)
	}

	fmt.Printf("   ✓ Segment ID: %s\n", results.SegmentID)
	fmt.Printf("   ✓ Segment Count: %d highly engaged subscribers\n", segmentCount)
	return nil
}

func createCampaignWithABTest(ctx context.Context, db *sql.DB, results *ImportResults) error {
	// First, find or create template
	var templateID uuid.UUID
	err := db.QueryRowContext(ctx, `
		SELECT id FROM mailing_templates 
		WHERE organization_id = $1 AND name = 'Welcome Quiz'
		LIMIT 1
	`, orgID).Scan(&templateID)
	
	if err == sql.ErrNoRows {
		// Template doesn't exist, create a basic one
		templateID = uuid.New()
		_, err = db.ExecContext(ctx, `
			INSERT INTO mailing_templates (
				id, organization_id, name, subject, html_content, status
			) VALUES ($1, $2, 'Welcome Quiz', 
				'🧠 Can You Go 3 for 3? Today''s Triple Challenge',
				'<html><body><h1>Welcome Quiz</h1></body></html>',
				'active'
			)
		`, templateID, orgID)
		if err != nil {
			return fmt.Errorf("failed to create template: %w", err)
		}
		fmt.Printf("   ✓ Created Template: %s\n", templateID)
	} else if err != nil {
		return fmt.Errorf("failed to query template: %w", err)
	} else {
		fmt.Printf("   ✓ Using existing Template: %s\n", templateID)
	}
	results.TemplateID = templateID

	// Create campaign
	campaignID := uuid.New()
	
	// Schedule for 8:03 AM MST = 15:03 UTC on today's date
	scheduledAt := time.Now().UTC().Truncate(24*time.Hour).Add(15*time.Hour + 3*time.Minute)
	if scheduledAt.Before(time.Now().UTC()) {
		scheduledAt = scheduledAt.Add(24 * time.Hour)
	}

	preheader := "From Titanic to the Wright Brothers - How much do you really know?"
	htmlContent := "<html><body><h1>This Day In History - Welcome Quiz</h1></body></html>"
	
	// NOTE: We write to BOTH send_at AND scheduled_at for compatibility with scheduler
	_, err = db.ExecContext(ctx, `
		INSERT INTO mailing_campaigns (
			id, organization_id, name, subject, preview_text, html_content, plain_content,
			list_id, segment_id, template_id,
			status, send_at, scheduled_at, campaign_type,
			from_name, from_email
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			$8, $9, $10,
			'scheduled', $11, $11, 'ab_test',
			'This Day In History', 'quiz@thisdayinhistory.com'
		)
	`, campaignID, orgID,
		"This Day In History - Welcome Quiz (A/B Test)",
		"🧠 Can You Go 3 for 3? Today's Triple Challenge",
		preheader,
		htmlContent,
		"This Day In History - Welcome Quiz",
		results.ListID, results.SegmentID, templateID,
		scheduledAt,
	)
	if err != nil {
		return fmt.Errorf("failed to create campaign: %w", err)
	}
	results.CampaignID = campaignID
	fmt.Printf("   ✓ Campaign ID: %s\n", campaignID)
	fmt.Printf("   ✓ Scheduled for: %s\n", scheduledAt.Format(time.RFC3339))

	// Create A/B Test
	abTestID := uuid.New()
	_, err = db.ExecContext(ctx, `
		INSERT INTO mailing_ab_tests (
			id, organization_id, campaign_id, name, description, test_type,
			split_type, test_sample_percent, winner_metric, winner_wait_hours,
			winner_auto_select, winner_confidence_threshold, winner_min_sample_size,
			list_id, segment_id, status
		) VALUES (
			$1, $2, $3, $4, $5, 'subject_line',
			'percentage', 100, 'open_rate', 4,
			TRUE, 0.95, 1000,
			$6, $7, 'scheduled'
		)
	`, abTestID, orgID, campaignID,
		"Welcome Quiz A/B Subject Test",
		"Testing 4 different subject lines for maximum open rates",
		results.ListID, results.SegmentID,
	)
	if err != nil {
		return fmt.Errorf("failed to create A/B test: %w", err)
	}
	results.ABTestID = abTestID
	fmt.Printf("   ✓ A/B Test ID: %s\n", abTestID)

	// Create A/B Variants
	variants := []struct {
		Name      string
		Subject   string
		Preheader string
		IsControl bool
	}{
		{"A", "🧠 Can You Go 3 for 3? Today's Triple Challenge", "From Titanic to the Wright Brothers - How much do you really know?", true},
		{"B", "Your Daily History Quiz is Ready! 🎉", "Quick! The answers might surprise you...", false},
		{"C", "Did You Know? Test Your History Knowledge Today", "Today's challenge: Entertainment, Aviation, and Sports", false},
		{"D", "3 Questions. 3 Chances. How Many Can You Get Right?", "Join thousands testing their knowledge daily", false},
	}

	results.ABVariantIDs = make([]uuid.UUID, 0)
	for _, v := range variants {
		variantID := uuid.New()
		_, err = db.ExecContext(ctx, `
			INSERT INTO mailing_ab_variants (
				id, test_id, variant_name, subject, preheader, split_percent, is_control
			) VALUES ($1, $2, $3, $4, $5, 25, $6)
		`, variantID, abTestID, v.Name, v.Subject, v.Preheader, v.IsControl)
		
		if err != nil {
			log.Printf("Warning: failed to create variant %s: %v", v.Name, err)
			continue
		}
		results.ABVariantIDs = append(results.ABVariantIDs, variantID)
		controlStr := ""
		if v.IsControl {
			controlStr = " (CONTROL)"
		}
		fmt.Printf("   ✓ Variant %s%s: %s\n", v.Name, controlStr, variantID)
	}

	// Also create variants in the campaign_ab_variants table (for AI smart sender)
	for _, v := range variants {
		_, _ = db.ExecContext(ctx, `
			INSERT INTO mailing_campaign_ab_variants (
				campaign_id, variant_name, variant_type, variant_value, traffic_percentage, is_control
			) VALUES ($1, $2, 'subject', $3, 25, $4)
			ON CONFLICT (campaign_id, variant_name) DO NOTHING
		`, campaignID, v.Name, v.Subject, v.IsControl)
	}

	// Link campaign to A/B test
	_, _ = db.ExecContext(ctx, `
		UPDATE mailing_campaigns 
		SET is_ab_test = TRUE, ab_test_id = $1 
		WHERE id = $2
	`, abTestID, campaignID)

	return nil
}

func enableAISmartSending(ctx context.Context, db *sql.DB, results *ImportResults) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO mailing_campaign_ai_settings (
			campaign_id, enable_smart_sending, enable_throttle_optimization,
			enable_send_time_optimization, target_metric,
			min_throttle_rate, max_throttle_rate, current_throttle_rate,
			learning_period_minutes
		) VALUES (
			$1, TRUE, TRUE, TRUE, 'opens',
			5000, 15000, 10000, 60
		)
		ON CONFLICT (campaign_id) DO UPDATE SET
			enable_smart_sending = TRUE,
			enable_throttle_optimization = TRUE,
			enable_send_time_optimization = TRUE,
			target_metric = 'opens',
			min_throttle_rate = 5000,
			max_throttle_rate = 15000,
			current_throttle_rate = 10000,
			learning_period_minutes = 60,
			updated_at = NOW()
	`, results.CampaignID)

	if err != nil {
		return fmt.Errorf("failed to enable AI settings: %w", err)
	}

	fmt.Printf("   ✓ AI Smart Sending enabled for campaign %s\n", results.CampaignID)
	fmt.Println("      - Smart Sending: ON")
	fmt.Println("      - Throttle Optimization: ON")
	fmt.Println("      - Send Time Optimization: ON")
	fmt.Println("      - Target Metric: Opens")
	fmt.Println("      - Throttle: 5,000-15,000/hour")
	fmt.Println("      - Learning Period: 60 minutes")

	return nil
}

func printResults(results *ImportResults) {
	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Println("✅ IMPORT COMPLETED SUCCESSFULLY")
	fmt.Println(strings.Repeat("=", 60))
	
	fmt.Println("\n📋 CREATED RESOURCES:")
	fmt.Printf("   List ID:        %s\n", results.ListID)
	fmt.Printf("   Segment ID:     %s (Count: %d)\n", results.SegmentID, results.SegmentCount)
	fmt.Printf("   Campaign ID:    %s\n", results.CampaignID)
	fmt.Printf("   Template ID:    %s\n", results.TemplateID)
	fmt.Printf("   A/B Test ID:    %s\n", results.ABTestID)
	
	fmt.Println("\n🧪 A/B VARIANT IDS:")
	for i, id := range results.ABVariantIDs {
		variant := string(rune('A' + i))
		fmt.Printf("   Variant %s:      %s\n", variant, id)
	}

	fmt.Println("\n📊 IMPORT STATISTICS:")
	fmt.Printf("   Total Imported: %d subscribers\n", results.TotalImported)
	fmt.Printf("   Highly Engaged: %d subscribers\n", results.SegmentCount)

	fmt.Println("\n📌 CUSTOM FIELD IDS:")
	for i, id := range results.CustomFieldIDs {
		fmt.Printf("   Field %d:        %s\n", i+1, id)
	}

	// JSON output
	jsonBytes, _ := json.MarshalIndent(results, "", "  ")
	fmt.Println("\n📄 JSON OUTPUT:")
	fmt.Println(string(jsonBytes))
}

func ptr(s string) *string {
	return &s
}
