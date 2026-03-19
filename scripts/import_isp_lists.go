// +build ignore

package main

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// ISP family merge map: filename ISP prefix -> PMTA pool name
var ispMergeMap = map[string]string{
	"AOL":             "yahoo",
	"Verizon":         "yahoo",
	"ATT":             "att",
	"SBC":             "att",
	"Gmail":           "gmail",
	"Microsoft":       "msft",
	"Comcast":         "comcast",
	"Cox":             "cox",
	"Everything Else": "other",
}

// Brand suffix -> brand prefix + sending domain
var brandMap = map[string]struct {
	Prefix string
	Domain string
}{
	"HT":  {Prefix: "HT", Domain: "em.historythinking.com"},
	"OMH": {Prefix: "MH", Domain: "em.myownhealth.net"},
}

// ValidationStatusId -> suppression reason
var suppressionReasonMap = map[string]string{
	"Undeliverable": "hard_bounce",
	"SpamTrap":      "spam_trap",
	"Complainer":    "complaint",
	"Unknown":       "unverified",
}

const batchSize = 5000

type listKey struct {
	Brand string
	ISP   string
}

type importStats struct {
	ListsCreated       int
	VerifiedImported   int
	SuppressionsAdded  int
	SegmentsCreated    int
	DuplicatesSkipped  int
	InvalidSkipped     int
	PerList            map[string]int
	PerSuppReason      map[string]int
}

func main() {
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/Users/mrjames/Desktop/Email Data/Data Uploads/03192026"
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://ignite:ignite_dev_password@127.0.0.1:5432/ignite?sslmode=disable"
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(10)

	if err := db.Ping(); err != nil {
		log.Fatalf("Failed to ping database: %v", err)
	}

	ctx := context.Background()

	orgID, err := getOrgID(ctx, db)
	if err != nil {
		log.Fatalf("Failed to get organization ID: %v", err)
	}
	log.Printf("Organization ID: %s", orgID)

	csvFiles, err := filepath.Glob(filepath.Join(dataDir, "*.csv"))
	if err != nil || len(csvFiles) == 0 {
		log.Fatalf("No CSV files found in %s", dataDir)
	}
	log.Printf("Found %d CSV files in %s", len(csvFiles), dataDir)

	// Ensure ISP column exists on mailing_subscribers
	ensureISPColumn(ctx, db)

	// Phase 1: Create all 14 lists
	listIDs := make(map[listKey]uuid.UUID)
	for _, file := range csvFiles {
		brand, isp := parseFilename(filepath.Base(file))
		if brand == "" || isp == "" {
			log.Printf("SKIP: cannot parse filename: %s", filepath.Base(file))
			continue
		}
		key := listKey{Brand: brand, ISP: isp}
		if _, exists := listIDs[key]; exists {
			continue
		}
		listName := fmt.Sprintf("%s %s", brandMap[brand].Prefix, strings.Title(isp))
		listID, err := ensureList(ctx, db, orgID, listName,
			fmt.Sprintf("ISP-segmented list for %s subscribers (%s)", strings.Title(isp), brandMap[brand].Domain))
		if err != nil {
			log.Fatalf("Failed to create list %s: %v", listName, err)
		}
		listIDs[key] = listID
		log.Printf("List: %s -> %s", listName, listID)
	}

	// Phase 2: Process each CSV file
	stats := &importStats{
		PerList:       make(map[string]int),
		PerSuppReason: make(map[string]int),
	}
	startTime := time.Now()

	for _, file := range csvFiles {
		brand, isp := parseFilename(filepath.Base(file))
		if brand == "" || isp == "" {
			continue
		}
		key := listKey{Brand: brand, ISP: isp}
		listID := listIDs[key]
		listName := fmt.Sprintf("%s %s", brandMap[brand].Prefix, strings.Title(isp))

		log.Printf("Processing: %s -> list %s", filepath.Base(file), listName)
		processCSVFile(ctx, db, orgID, listID, isp, file, stats, listName)
	}

	// Phase 3: Update list counts
	log.Println("Updating list subscriber counts...")
	for key, listID := range listIDs {
		listName := fmt.Sprintf("%s %s", brandMap[key.Brand].Prefix, strings.Title(key.ISP))
		updateListCounts(ctx, db, listID)
		var count int
		db.QueryRowContext(ctx, "SELECT subscriber_count FROM mailing_lists WHERE id = $1", listID).Scan(&count)
		log.Printf("  %s: %d subscribers", listName, count)
	}

	// Phase 4: Create segments
	log.Println("Creating segments...")
	segmentIDs := createSegments(ctx, db, orgID, listIDs)
	stats.SegmentsCreated = len(segmentIDs)

	elapsed := time.Since(startTime)
	printSummary(stats, listIDs, segmentIDs, elapsed)
}

func getOrgID(ctx context.Context, db *sql.DB) (string, error) {
	var orgID string
	envOrg := os.Getenv("ORG_ID")
	if envOrg != "" {
		return envOrg, nil
	}
	err := db.QueryRowContext(ctx, "SELECT id::text FROM organizations LIMIT 1").Scan(&orgID)
	return orgID, err
}

func ensureISPColumn(ctx context.Context, db *sql.DB) {
	db.ExecContext(ctx, "ALTER TABLE mailing_subscribers ADD COLUMN IF NOT EXISTS isp VARCHAR(20) DEFAULT ''")
	db.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_subscribers_isp ON mailing_subscribers(isp) WHERE isp != ''")
}

func parseFilename(name string) (brand string, isp string) {
	name = strings.TrimSuffix(name, ".csv")

	// Detect brand from suffix: "- HT" or "- OMH"
	if strings.HasSuffix(name, " - HT") {
		brand = "HT"
		name = strings.TrimSuffix(name, " - HT")
	} else if strings.HasSuffix(name, " - OMH") {
		brand = "OMH"
		name = strings.TrimSuffix(name, " - OMH")
	} else {
		return "", ""
	}

	// Strip " - Active" middle part
	name = strings.TrimSuffix(name, " - Active")

	// Map ISP prefix to PMTA pool name
	poolName, ok := ispMergeMap[name]
	if !ok {
		return "", ""
	}
	return brand, poolName
}

func ensureList(ctx context.Context, db *sql.DB, orgID, name, description string) (uuid.UUID, error) {
	var listID uuid.UUID
	err := db.QueryRowContext(ctx,
		"SELECT id FROM mailing_lists WHERE organization_id = $1 AND name = $2",
		orgID, name).Scan(&listID)
	if err == sql.ErrNoRows {
		listID = uuid.New()
		_, err = db.ExecContext(ctx,
			`INSERT INTO mailing_lists (id, organization_id, name, description, subscriber_count, status, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, 0, 'active', NOW(), NOW())`,
			listID, orgID, name, description)
		if err != nil {
			return uuid.Nil, err
		}
	} else if err != nil {
		return uuid.Nil, err
	}
	return listID, nil
}

func processCSVFile(ctx context.Context, db *sql.DB, orgID string, listID uuid.UUID, isp, filePath string, stats *importStats, listName string) {
	f, err := os.Open(filePath)
	if err != nil {
		log.Printf("ERROR opening %s: %v", filePath, err)
		return
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.LazyQuotes = true
	reader.FieldsPerRecord = -1

	header, err := reader.Read()
	if err != nil {
		log.Printf("ERROR reading header from %s: %v", filePath, err)
		return
	}

	colIdx := make(map[string]int)
	for i, h := range header {
		colIdx[strings.TrimSpace(h)] = i
	}

	emailIdx, ok := colIdx["Email"]
	if !ok {
		log.Printf("ERROR: no Email column in %s", filePath)
		return
	}
	validIdx, ok2 := colIdx["ValidationStatusId"]
	if !ok2 {
		log.Printf("ERROR: no ValidationStatusId column in %s", filePath)
		return
	}
	fnIdx := colIdx["FirstName"]
	lnIdx := colIdx["LastName"]

	var subBatch []subscriberRow
	var suppBatch []suppressionRow
	rowNum := 0

	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		rowNum++

		if len(record) <= emailIdx || len(record) <= validIdx {
			stats.InvalidSkipped++
			continue
		}

		email := strings.TrimSpace(strings.ToLower(record[emailIdx]))
		if email == "" || !strings.Contains(email, "@") {
			stats.InvalidSkipped++
			continue
		}

		validStatus := strings.TrimSpace(record[validIdx])

		if validStatus == "Verified" {
			firstName := safeCol(record, fnIdx)
			lastName := safeCol(record, lnIdx)
			subBatch = append(subBatch, subscriberRow{
				Email:     email,
				FirstName: firstName,
				LastName:  lastName,
				ISP:       isp,
			})
		} else {
			reason, ok := suppressionReasonMap[validStatus]
			if !ok {
				reason = "unverified"
			}
			suppBatch = append(suppBatch, suppressionRow{
				Email:  email,
				Reason: reason,
			})
		}

		if len(subBatch) >= batchSize {
			n := flushSubscribers(ctx, db, orgID, listID, subBatch)
			stats.VerifiedImported += n
			stats.PerList[listName] += n
			subBatch = subBatch[:0]
		}
		if len(suppBatch) >= batchSize {
			n := flushSuppressions(ctx, db, orgID, suppBatch)
			stats.SuppressionsAdded += n
			for _, s := range suppBatch[:n] {
				stats.PerSuppReason[s.Reason]++
			}
			suppBatch = suppBatch[:0]
		}
	}

	// Flush remaining
	if len(subBatch) > 0 {
		n := flushSubscribers(ctx, db, orgID, listID, subBatch)
		stats.VerifiedImported += n
		stats.PerList[listName] += n
	}
	if len(suppBatch) > 0 {
		n := flushSuppressions(ctx, db, orgID, suppBatch)
		stats.SuppressionsAdded += n
	}

	log.Printf("  Done: %d rows processed", rowNum)
}

type subscriberRow struct {
	Email, FirstName, LastName, ISP string
}

type suppressionRow struct {
	Email, Reason string
}

func safeCol(record []string, idx int) string {
	if idx < len(record) {
		return strings.TrimSpace(record[idx])
	}
	return ""
}

func sha256Hash(email string) string {
	h := sha256.Sum256([]byte(email))
	return hex.EncodeToString(h[:])
}

func md5Hash(email string) string {
	h := md5.Sum([]byte(email))
	return hex.EncodeToString(h[:])
}

func flushSubscribers(ctx context.Context, db *sql.DB, orgID string, listID uuid.UUID, rows []subscriberRow) int {
	if len(rows) == 0 {
		return 0
	}

	var b strings.Builder
	args := make([]interface{}, 0, len(rows)*7)
	b.WriteString(`INSERT INTO mailing_subscribers
		(id, organization_id, list_id, email, email_hash, first_name, last_name, status, source, engagement_score, isp, created_at, updated_at)
		VALUES `)

	for i, r := range rows {
		if i > 0 {
			b.WriteString(",")
		}
		base := i * 7
		b.WriteString(fmt.Sprintf("(gen_random_uuid(), $%d, $%d, $%d, $%d, $%d, $%d, 'confirmed', 'csv_import', 50.0, $%d, NOW(), NOW())",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7))
		args = append(args, orgID, listID, r.Email, sha256Hash(r.Email), r.FirstName, r.LastName, r.ISP)
	}

	b.WriteString(" ON CONFLICT (list_id, email) DO UPDATE SET first_name = EXCLUDED.first_name, last_name = EXCLUDED.last_name, isp = EXCLUDED.isp, updated_at = NOW()")

	_, err := db.ExecContext(ctx, b.String(), args...)
	if err != nil {
		log.Printf("ERROR inserting subscriber batch (%d rows): %v", len(rows), err)
		return insertSubscribersOneByOne(ctx, db, orgID, listID, rows)
	}
	return len(rows)
}

func insertSubscribersOneByOne(ctx context.Context, db *sql.DB, orgID string, listID uuid.UUID, rows []subscriberRow) int {
	count := 0
	for _, r := range rows {
		_, err := db.ExecContext(ctx,
			`INSERT INTO mailing_subscribers
			 (id, organization_id, list_id, email, email_hash, first_name, last_name, status, source, engagement_score, isp, created_at, updated_at)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, 'confirmed', 'csv_import', 50.0, $7, NOW(), NOW())
			 ON CONFLICT (list_id, email) DO UPDATE SET first_name = EXCLUDED.first_name, last_name = EXCLUDED.last_name, isp = EXCLUDED.isp, updated_at = NOW()`,
			orgID, listID, r.Email, sha256Hash(r.Email), r.FirstName, r.LastName, r.ISP)
		if err == nil {
			count++
		}
	}
	return count
}

func flushSuppressions(ctx context.Context, db *sql.DB, orgID string, rows []suppressionRow) int {
	if len(rows) == 0 {
		return 0
	}

	var b strings.Builder
	args := make([]interface{}, 0, len(rows)*4)
	b.WriteString(`INSERT INTO mailing_global_suppressions
		(id, organization_id, email, md5_hash, reason, source, created_at)
		VALUES `)

	for i, r := range rows {
		if i > 0 {
			b.WriteString(",")
		}
		base := i * 4
		b.WriteString(fmt.Sprintf("(gen_random_uuid(), $%d, $%d, $%d, $%d, 'csv_import_03192026', NOW())",
			base+1, base+2, base+3, base+4))
		args = append(args, orgID, r.Email, md5Hash(r.Email), r.Reason)
	}

	b.WriteString(" ON CONFLICT (organization_id, md5_hash) DO NOTHING")

	_, err := db.ExecContext(ctx, b.String(), args...)
	if err != nil {
		log.Printf("ERROR inserting suppression batch (%d rows): %v", len(rows), err)
		return 0
	}
	return len(rows)
}

func updateListCounts(ctx context.Context, db *sql.DB, listID uuid.UUID) {
	db.ExecContext(ctx, `
		UPDATE mailing_lists l
		SET subscriber_count = sub.cnt,
		    active_count = sub.active_cnt,
		    updated_at = NOW()
		FROM (
			SELECT list_id,
			       COUNT(*) AS cnt,
			       COUNT(*) FILTER (WHERE status IN ('active','confirmed')) AS active_cnt
			FROM mailing_subscribers WHERE list_id = $1
			GROUP BY list_id
		) sub
		WHERE l.id = sub.list_id`, listID)
}

// --- Segment Creation ---

type segmentDef struct {
	Name       string
	Purpose    string
	Conditions interface{}
}

func createSegments(ctx context.Context, db *sql.DB, orgID string, listIDs map[listKey]uuid.UUID) map[string]uuid.UUID {
	segmentIDs := make(map[string]uuid.UUID)

	for _, brandInfo := range []struct {
		FileSuffix string
		Prefix     string
		Domain     string
	}{
		{"HT", "HT", "em.historythinking.com"},
		{"OMH", "MH", "em.myownhealth.net"},
	} {
		prefix := brandInfo.Prefix
		domain := brandInfo.Domain

		inclusionSegments := []segmentDef{
			{
				Name:    fmt.Sprintf("%s Never Mailed", prefix),
				Purpose: "Warmup: fresh audience with no prior sends",
				Conditions: conditionGroup("AND", false, []condition{
					{Type: "profile", Field: "total_emails_received", Op: "equals", Value: "0"},
				}, nil),
			},
			{
				Name:    fmt.Sprintf("%s 7D Openers", prefix),
				Purpose: "Warmup: subscribers who opened in last 7 days (domain-scoped)",
				Conditions: conditionGroup("AND", false, []condition{
					{Type: "event", Field: "email_opened", Op: "event_in_last_days", Value: "7", SendingDomain: domain},
				}, nil),
			},
			{
				Name:    fmt.Sprintf("%s 14D Clickers", prefix),
				Purpose: "High engagement: clicked in last 14 days (domain-scoped)",
				Conditions: conditionGroup("AND", false, []condition{
					{Type: "event", Field: "email_clicked", Op: "event_in_last_days", Value: "14", SendingDomain: domain},
				}, nil),
			},
			{
				Name:    fmt.Sprintf("%s 30D Engaged", prefix),
				Purpose: "Monetization: opened in last 30 days (domain-scoped)",
				Conditions: conditionGroup("AND", false, []condition{
					{Type: "event", Field: "email_opened", Op: "event_in_last_days", Value: "30", SendingDomain: domain},
				}, nil),
			},
		}

		exclusionSegments := []segmentDef{
			{
				Name:    fmt.Sprintf("%s 60D Inactive", prefix),
				Purpose: "Reputation protection: no opens in 60+ days (domain-scoped)",
				Conditions: conditionGroup("OR", false, []condition{
					{Type: "event", Field: "email_opened", Op: "event_not_in_last_days", Value: "60", SendingDomain: domain},
					{Type: "profile", Field: "last_open_at", Op: "is_null", Value: ""},
				}, nil),
			},
			{
				Name:    fmt.Sprintf("%s Hard Bounced", prefix),
				Purpose: "Exclusion: permanently failed delivery",
				Conditions: conditionGroup("AND", false, []condition{
					{Type: "profile", Field: "status", Op: "equals", Value: "bounced"},
				}, nil),
			},
			{
				Name:    fmt.Sprintf("%s Complained", prefix),
				Purpose: "Exclusion: subscribers who filed complaints",
				Conditions: conditionGroup("AND", false, []condition{
					{Type: "profile", Field: "status", Op: "equals", Value: "complained"},
				}, nil),
			},
		}

		allSegments := append(inclusionSegments, exclusionSegments...)
		for _, seg := range allSegments {
			condJSON, _ := json.Marshal(seg.Conditions)
			segID, err := ensureSegment(ctx, db, orgID, seg.Name, seg.Purpose, string(condJSON))
			if err != nil {
				log.Printf("ERROR creating segment %s: %v", seg.Name, err)
				continue
			}
			segmentIDs[seg.Name] = segID
			log.Printf("  Segment: %s -> %s", seg.Name, segID)
		}
	}

	return segmentIDs
}

type condition struct {
	Type          string
	Field         string
	Op            string
	Value         string
	SendingDomain string
}

func conditionGroup(logic string, negated bool, conds []condition, subGroups []interface{}) map[string]interface{} {
	condList := make([]map[string]interface{}, 0, len(conds))
	for _, c := range conds {
		m := map[string]interface{}{
			"id":             uuid.New().String(),
			"condition_type": c.Type,
			"field":          c.Field,
			"operator":       c.Op,
		}
		if c.Value != "" {
			m["value"] = c.Value
		}
		if c.SendingDomain != "" {
			m["event_sending_domain"] = c.SendingDomain
		}
		condList = append(condList, m)
	}

	groups := make([]interface{}, 0)
	if subGroups != nil {
		groups = subGroups
	}

	return map[string]interface{}{
		"id":             uuid.New().String(),
		"logic_operator": logic,
		"is_negated":     negated,
		"conditions":     condList,
		"groups":         groups,
	}
}

func ensureSegment(ctx context.Context, db *sql.DB, orgID, name, description, conditionsJSON string) (uuid.UUID, error) {
	var segID uuid.UUID
	err := db.QueryRowContext(ctx,
		"SELECT id FROM mailing_segments WHERE organization_id = $1 AND name = $2",
		orgID, name).Scan(&segID)
	if err == sql.ErrNoRows {
		segID = uuid.New()
		_, err = db.ExecContext(ctx,
			`INSERT INTO mailing_segments
			 (id, organization_id, name, description, segment_type, conditions, subscriber_count, status, calculation_mode, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, 'dynamic', $5::jsonb, 0, 'active', 'batch', NOW(), NOW())`,
			segID, orgID, name, description, conditionsJSON)
		if err != nil {
			return uuid.Nil, err
		}
	} else if err != nil {
		return uuid.Nil, err
	}
	return segID, nil
}

func printSummary(stats *importStats, listIDs map[listKey]uuid.UUID, segmentIDs map[string]uuid.UUID, elapsed time.Duration) {
	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Println("  ISP LIST IMPORT COMPLETE")
	fmt.Println(strings.Repeat("=", 70))

	fmt.Printf("\n  Duration:          %v\n", elapsed.Round(time.Second))
	fmt.Printf("  Verified imported: %d\n", stats.VerifiedImported)
	fmt.Printf("  Suppressions:      %d\n", stats.SuppressionsAdded)
	fmt.Printf("  Lists created:     %d\n", len(listIDs))
	fmt.Printf("  Segments created:  %d\n", stats.SegmentsCreated)

	fmt.Println("\n  --- Lists ---")
	for key, id := range listIDs {
		name := fmt.Sprintf("%s %s", brandMap[key.Brand].Prefix, strings.Title(key.ISP))
		count := stats.PerList[name]
		fmt.Printf("  %-20s %s  (%d verified)\n", name, id, count)
	}

	fmt.Println("\n  --- Segments ---")
	for name, id := range segmentIDs {
		fmt.Printf("  %-30s %s\n", name, id)
	}

	fmt.Println("\n  --- Suppression Breakdown ---")
	for reason, count := range stats.PerSuppReason {
		fmt.Printf("  %-20s %d\n", reason, count)
	}

	// Output JSON manifest for use by campaign configuration
	manifest := map[string]interface{}{
		"lists":    listIDs,
		"segments": segmentIDs,
		"stats": map[string]interface{}{
			"verified_imported":  stats.VerifiedImported,
			"suppressions_added": stats.SuppressionsAdded,
			"per_list":           stats.PerList,
		},
	}
	jsonBytes, _ := json.MarshalIndent(manifest, "", "  ")
	fmt.Println("\n  --- JSON Manifest ---")
	fmt.Println(string(jsonBytes))
}
