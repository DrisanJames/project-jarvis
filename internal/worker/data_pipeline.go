package worker

import (
	"bufio"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	"github.com/ignite/sparkpost-monitor/internal/config"
	"github.com/ignite/sparkpost-monitor/internal/emailoversight"
	"github.com/ignite/sparkpost-monitor/internal/pkg/isp"
)

// s3FolderToISP maps S3 folder names (case-insensitive) to canonical ISP group names.
//
// When the S3 data pipeline ingests a folder named "aol", it now maps to isp.Aol
// (not isp.Yahoo). This ensures subscribers imported from AOL-specific data files
// get the correct ISP tag and route through AOL's dedicated sending pool.
//
// Verizon maps to isp.Verizon (not Yahoo). The legacy mapping to Yahoo was
// incorrect; Verizon is a distinct ISP with its own email infrastructure.
var s3FolderToISP = map[string]string{
	"google":    isp.Gmail,
	"gmail":     isp.Gmail,
	"yahoo":     isp.Yahoo,
	"microsoft": isp.Microsoft,
	"outlook":   isp.Microsoft,
	"apple":     isp.Apple,
	"comcast":   isp.Comcast,
	"charter":   isp.Charter,
	"spectrum":  isp.Charter,
	"att":       isp.ATT,
	"sbcglobal": isp.Sbcglobal,
	"bellsouth": isp.Sbcglobal,
	"cox":       isp.Cox,
	"aol":       isp.Aol,
	"verizon":   isp.Verizon,
}

type DataPipeline struct {
	db       *sql.DB
	s3Client *s3.Client
	eoClient *emailoversight.Client
	cfg      config.DataPipelineConfig
	orgID    string

	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	wg     sync.WaitGroup

	running int32

	// PipelineNotifier is set after construction to avoid circular dependency.
	Notifier PipelineNotifier
}

// PipelineNotifier sends email reports after a pipeline run.
type PipelineNotifier interface {
	SendPipelineReport(report PipelineReport) error
}

// PipelineReport is the summary sent to the admin after each run.
type PipelineReport struct {
	RunID           string
	StartedAt       time.Time
	CompletedAt     time.Time
	FilesProcessed  int
	EmailsTotal     int
	EmailsVerified  int
	EmailsSuppressed int
	EmailsDeduped   int
	DomainBreakdown []DomainStat
	Errors          []string
}

type DomainStat struct {
	SendingDomain string
	ISP           string
	ListName      string
	Added         int
	Suppressed    int
	Deduped       int
}

// DomainDeficit represents how many subscribers a sending domain+ISP list needs.
type DomainDeficit struct {
	SendingDomain string
	ISP           string
	ListID        string
	ListName      string
	CurrentCount  int
	DailyQuota    int
	DaysRemaining int
	Deficit       int
}

func NewDataPipeline(db *sql.DB, cfg config.DataPipelineConfig, orgID string) (*DataPipeline, error) {
	ctx := context.Background()

	var awsCfg aws.Config
	var err error
	if cfg.AWSProfile != "" {
		awsCfg, err = awsconfig.LoadDefaultConfig(ctx,
			awsconfig.WithRegion(cfg.S3Region),
			awsconfig.WithSharedConfigProfile(cfg.AWSProfile),
		)
	} else {
		awsCfg, err = awsconfig.LoadDefaultConfig(ctx,
			awsconfig.WithRegion(cfg.S3Region),
		)
	}
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}

	concurrency := cfg.EOConcurrency
	if concurrency < 1 {
		concurrency = 20
	}

	return &DataPipeline{
		db:       db,
		s3Client: s3.NewFromConfig(awsCfg),
		eoClient: emailoversight.New(cfg.EOAPIToken, cfg.EOListID, concurrency),
		cfg:      cfg,
		orgID:    orgID,
	}, nil
}

func (dp *DataPipeline) Start() {
	dp.mu.Lock()
	defer dp.mu.Unlock()

	dp.ctx, dp.cancel = context.WithCancel(context.Background())
	dp.wg.Add(1)
	go func() {
		defer dp.wg.Done()
		dp.schedulerLoop()
	}()
	log.Printf("[DataPipeline] started — run_hour_utc=%d", dp.cfg.RunHourUTC)
}

func (dp *DataPipeline) Stop() {
	dp.mu.Lock()
	defer dp.mu.Unlock()
	if dp.cancel != nil {
		dp.cancel()
	}
	dp.wg.Wait()
	log.Println("[DataPipeline] stopped")
}

func (dp *DataPipeline) schedulerLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	dp.checkAndRun()

	for {
		select {
		case <-dp.ctx.Done():
			return
		case <-ticker.C:
			dp.checkAndRun()
		}
	}
}

func (dp *DataPipeline) checkAndRun() {
	now := time.Now().UTC()
	if now.Hour() != dp.cfg.RunHourUTC {
		return
	}

	var ranToday bool
	err := dp.db.QueryRowContext(dp.ctx,
		`SELECT EXISTS(SELECT 1 FROM data_pipeline_runs WHERE started_at >= CURRENT_DATE AND status != 'failed')`,
	).Scan(&ranToday)
	if err != nil {
		log.Printf("[DataPipeline] check today's run: %v", err)
		return
	}
	if ranToday {
		return
	}

	dp.RunOnce(dp.ctx)
}

// RunOnce triggers a single pipeline run. Safe to call externally (e.g., from an API trigger).
func (dp *DataPipeline) RunOnce(ctx context.Context) {
	if !atomic.CompareAndSwapInt32(&dp.running, 0, 1) {
		log.Println("[DataPipeline] run already in progress, skipping")
		return
	}
	defer atomic.StoreInt32(&dp.running, 0)

	log.Println("[DataPipeline] === run starting ===")
	startedAt := time.Now()

	runID := uuid.New().String()
	_, err := dp.db.ExecContext(ctx,
		`INSERT INTO data_pipeline_runs (id, organization_id, started_at, status)
		 VALUES ($1, $2, $3, 'running')`,
		runID, dp.orgID, startedAt,
	)
	if err != nil {
		log.Printf("[DataPipeline] create run record: %v", err)
		return
	}

	report := PipelineReport{RunID: runID, StartedAt: startedAt}
	var reportErrors []string

	deficits, err := dp.assessDemand(ctx)
	if err != nil {
		dp.failRun(ctx, runID, fmt.Sprintf("assess demand: %v", err))
		return
	}

	if len(deficits) == 0 {
		log.Println("[DataPipeline] no deficits detected — all lists healthy")
		dp.completeRun(ctx, runID, report)
		return
	}

	log.Printf("[DataPipeline] %d domain/ISP combos need replenishment", len(deficits))

	if err := dp.discoverS3Files(ctx); err != nil {
		reportErrors = append(reportErrors, fmt.Sprintf("S3 discovery: %v", err))
	}

	for _, deficit := range deficits {
		if ctx.Err() != nil {
			break
		}

		stats, errs := dp.processDeficit(ctx, runID, deficit)
		report.FilesProcessed += stats.files
		report.EmailsTotal += stats.total
		report.EmailsVerified += stats.verified
		report.EmailsSuppressed += stats.suppressed
		report.EmailsDeduped += stats.deduped
		if stats.files > 0 {
			report.DomainBreakdown = append(report.DomainBreakdown, DomainStat{
				SendingDomain: deficit.SendingDomain,
				ISP:           deficit.ISP,
				ListName:      deficit.ListName,
				Added:         stats.verified,
				Suppressed:    stats.suppressed,
				Deduped:       stats.deduped,
			})
		}
		reportErrors = append(reportErrors, errs...)
	}

	report.CompletedAt = time.Now()
	report.Errors = reportErrors
	dp.completeRun(ctx, runID, report)

	if dp.Notifier != nil {
		if err := dp.Notifier.SendPipelineReport(report); err != nil {
			log.Printf("[DataPipeline] notification error: %v", err)
		}
	}

	log.Printf("[DataPipeline] === run complete: files=%d verified=%d suppressed=%d deduped=%d ===",
		report.FilesProcessed, report.EmailsVerified, report.EmailsSuppressed, report.EmailsDeduped)
}

type processStats struct {
	files, total, verified, suppressed, deduped int
}

// assessDemand identifies sending domain + ISP combos that can't fulfill the next day's quotas.
func (dp *DataPipeline) assessDemand(ctx context.Context) ([]DomainDeficit, error) {
	rows, err := dp.db.QueryContext(ctx, `
		WITH domain_lists AS (
			SELECT dl.sending_domain, dl.isp, dl.list_id, l.name,
			       COALESCE((SELECT COUNT(*) FROM mailing_subscribers s WHERE s.list_id = dl.list_id AND s.status = 'confirmed'), 0) AS subscriber_count
			FROM data_pipeline_domain_lists dl
			JOIN mailing_lists l ON l.id = dl.list_id
		),
		daily_quotas AS (
			SELECT c.sending_domain, sq.isp_group AS isp,
			       SUM(sq.daily_limit) AS daily_quota
			FROM mailing_campaigns c
			JOIN mailing_isp_quotas sq ON sq.campaign_id = c.id
			WHERE c.status IN ('active', 'scheduled', 'approved')
			GROUP BY c.sending_domain, sq.isp_group
		)
		SELECT dl.sending_domain, dl.isp, dl.list_id::text, dl.name,
		       dl.subscriber_count, COALESCE(dq.daily_quota, 0) AS daily_quota
		FROM domain_lists dl
		LEFT JOIN daily_quotas dq ON dq.sending_domain = dl.sending_domain AND dq.isp = dl.isp
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deficits []DomainDeficit
	for rows.Next() {
		var d DomainDeficit
		if err := rows.Scan(&d.SendingDomain, &d.ISP, &d.ListID, &d.ListName, &d.CurrentCount, &d.DailyQuota); err != nil {
			return nil, err
		}
		if d.DailyQuota == 0 {
			continue
		}
		d.DaysRemaining = 0
		if d.DailyQuota > 0 {
			d.DaysRemaining = d.CurrentCount / d.DailyQuota
		}

		unsent := d.CurrentCount
		row := dp.db.QueryRowContext(ctx,
			`SELECT COUNT(DISTINCT subscriber_id) FROM mailing_message_log
			 WHERE sending_domain = $1 AND sent_at >= NOW() - INTERVAL '90 days'
			 AND subscriber_id IN (SELECT id FROM mailing_subscribers WHERE list_id = $2)`,
			d.SendingDomain, d.ListID,
		)
		var sentCount int
		if row.Scan(&sentCount) == nil {
			unsent = d.CurrentCount - sentCount
		}
		if unsent < 0 {
			unsent = 0
		}

		d.Deficit = d.DailyQuota*3 - unsent
		if d.Deficit > dp.cfg.MinDeficitThreshold {
			deficits = append(deficits, d)
			log.Printf("[DataPipeline] deficit: %s/%s list=%s unsent=%d quota/day=%d deficit=%d",
				d.SendingDomain, d.ISP, d.ListName, unsent, d.DailyQuota, d.Deficit)
		}
	}
	return deficits, rows.Err()
}

// discoverS3Files scans the S3 prefix and registers any new files.
func (dp *DataPipeline) discoverS3Files(ctx context.Context) error {
	paginator := s3.NewListObjectsV2Paginator(dp.s3Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(dp.cfg.S3Bucket),
		Prefix: aws.String(dp.cfg.S3Prefix),
	})

	var discovered int
	for paginator.HasMorePages() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list objects: %w", err)
		}

		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			if !strings.HasSuffix(strings.ToLower(key), ".csv") {
				continue
			}
			ispFolder, ispNorm := dp.parseS3Key(key)
			if ispNorm == "" {
				continue
			}

			_, err := dp.db.ExecContext(ctx,
				`INSERT INTO data_pipeline_files (id, s3_key, isp_folder, isp_normalized, status, created_at)
				 VALUES (gen_random_uuid(), $1, $2, $3, 'available', NOW())
				 ON CONFLICT (s3_key) DO NOTHING`,
				key, ispFolder, ispNorm,
			)
			if err != nil {
				log.Printf("[DataPipeline] register file %s: %v", key, err)
				continue
			}
			discovered++
		}
	}

	if discovered > 0 {
		log.Printf("[DataPipeline] discovered %d new S3 files", discovered)
	}
	return nil
}

// parseS3Key extracts ISP folder name and normalized ISP from an S3 key like "warmup-data/2026/Google/Google_001.csv".
func (dp *DataPipeline) parseS3Key(key string) (folder, normalized string) {
	trimmed := strings.TrimPrefix(key, dp.cfg.S3Prefix)
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) < 2 {
		return "", ""
	}
	folder = parts[0]
	lower := strings.ToLower(folder)
	if norm, ok := s3FolderToISP[lower]; ok {
		return folder, norm
	}
	return folder, ""
}

// processDeficit handles one domain/ISP deficit: picks files, downloads, validates, routes.
func (dp *DataPipeline) processDeficit(ctx context.Context, runID string, deficit DomainDeficit) (processStats, []string) {
	var stats processStats
	var errs []string

	rows, err := dp.db.QueryContext(ctx,
		`SELECT id::text, s3_key FROM data_pipeline_files
		 WHERE isp_normalized = $1 AND status = 'available'
		 ORDER BY created_at ASC
		 LIMIT 5`,
		deficit.ISP,
	)
	if err != nil {
		return stats, append(errs, fmt.Sprintf("query files for %s/%s: %v", deficit.SendingDomain, deficit.ISP, err))
	}

	type fileRef struct {
		id, key string
	}
	var files []fileRef
	for rows.Next() {
		var f fileRef
		rows.Scan(&f.id, &f.key)
		files = append(files, f)
	}
	rows.Close()

	if len(files) == 0 {
		log.Printf("[DataPipeline] no available files for ISP %s", deficit.ISP)
		return stats, errs
	}

	for _, f := range files {
		if ctx.Err() != nil {
			break
		}

		dp.db.ExecContext(ctx,
			`UPDATE data_pipeline_files SET status = 'processing', run_id = $1, sending_domain = $2, target_list_id = $3
			 WHERE id = $4`,
			runID, deficit.SendingDomain, deficit.ListID, f.id,
		)

		fStats, fErr := dp.processFile(ctx, f.id, f.key, deficit)
		stats.files++
		stats.total += fStats.total
		stats.verified += fStats.verified
		stats.suppressed += fStats.suppressed
		stats.deduped += fStats.deduped

		status := "completed"
		if fErr != nil {
			status = "error"
			errs = append(errs, fmt.Sprintf("file %s: %v", f.key, fErr))
		}

		dp.db.ExecContext(ctx,
			`UPDATE data_pipeline_files SET status = $1, verified_count = $2, suppressed_count = $3, deduped_count = $4,
			 row_count = $5, processed_at = NOW()
			 WHERE id = $6`,
			status, fStats.verified, fStats.suppressed, fStats.deduped, fStats.total, f.id,
		)

		if stats.verified >= deficit.Deficit {
			log.Printf("[DataPipeline] deficit for %s/%s satisfied (%d added)", deficit.SendingDomain, deficit.ISP, stats.verified)
			break
		}
	}

	return stats, errs
}

// processFile downloads a CSV from S3, dedupes, validates via EO, and routes emails.
func (dp *DataPipeline) processFile(ctx context.Context, fileID, s3Key string, deficit DomainDeficit) (processStats, error) {
	var stats processStats

	getOut, err := dp.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(dp.cfg.S3Bucket),
		Key:    aws.String(s3Key),
	})
	if err != nil {
		return stats, fmt.Errorf("get S3 object: %w", err)
	}
	defer getOut.Body.Close()

	reader := csv.NewReader(bufio.NewReaderSize(getOut.Body, 256*1024))
	reader.FieldsPerRecord = -1

	header, err := reader.Read()
	if err != nil {
		return stats, fmt.Errorf("read CSV header: %w", err)
	}
	emailCol := findEmailColumn(header)
	if emailCol < 0 {
		return stats, fmt.Errorf("no email column found in header: %v", header)
	}

	var emails []string
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		if emailCol >= len(record) {
			continue
		}
		email := strings.TrimSpace(strings.ToLower(record[emailCol]))
		if email != "" && strings.Contains(email, "@") {
			emails = append(emails, email)
		}
	}

	stats.total = len(emails)
	if stats.total == 0 {
		return stats, nil
	}

	// Dedupe against existing subscribers in this list
	deduped, dedupCount := dp.dedupeAgainstList(ctx, emails, deficit.ListID)
	stats.deduped = dedupCount
	emails = deduped

	if len(emails) == 0 {
		log.Printf("[DataPipeline] file %s: all %d emails already in list", s3Key, stats.total)
		return stats, nil
	}

	// Validate via EmailOversight in batches of 500
	batchSize := 500
	for i := 0; i < len(emails); i += batchSize {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		end := i + batchSize
		if end > len(emails) {
			end = len(emails)
		}
		batch := emails[i:end]

		results := dp.eoClient.ValidateBatch(ctx, batch)

		var validEmails []string
		var invalidEmails []string
		for _, r := range results {
			if r.Err != nil {
				stats.suppressed++
				invalidEmails = append(invalidEmails, r.Email)
				continue
			}
			if r.IsValid {
				stats.verified++
				validEmails = append(validEmails, r.Email)
			} else {
				stats.suppressed++
				invalidEmails = append(invalidEmails, r.Email)
			}
		}

		if len(validEmails) > 0 {
			dp.insertSubscribers(ctx, validEmails, deficit.ListID)
		}
		if len(invalidEmails) > 0 {
			dp.addToGlobalSuppression(ctx, invalidEmails)
		}
	}

	log.Printf("[DataPipeline] file %s: total=%d verified=%d suppressed=%d deduped=%d",
		s3Key, stats.total, stats.verified, stats.suppressed, stats.deduped)
	return stats, nil
}

func findEmailColumn(header []string) int {
	for i, h := range header {
		h = strings.TrimSpace(strings.ToLower(h))
		if h == "email" || h == "email_address" || h == "emailaddress" || h == "e-mail" {
			return i
		}
	}
	if len(header) > 0 {
		return 0
	}
	return -1
}

func (dp *DataPipeline) dedupeAgainstList(ctx context.Context, emails []string, listID string) ([]string, int) {
	if len(emails) == 0 {
		return emails, 0
	}

	existing := make(map[string]bool)
	batchSize := 1000
	for i := 0; i < len(emails); i += batchSize {
		end := i + batchSize
		if end > len(emails) {
			end = len(emails)
		}
		batch := emails[i:end]

		placeholders := make([]string, len(batch))
		args := []interface{}{listID}
		for j, e := range batch {
			placeholders[j] = fmt.Sprintf("$%d", j+2)
			args = append(args, e)
		}

		rows, err := dp.db.QueryContext(ctx,
			fmt.Sprintf(`SELECT LOWER(email) FROM mailing_subscribers WHERE list_id = $1 AND LOWER(email) IN (%s)`,
				strings.Join(placeholders, ",")),
			args...,
		)
		if err != nil {
			log.Printf("[DataPipeline] dedup query: %v", err)
			continue
		}
		for rows.Next() {
			var e string
			rows.Scan(&e)
			existing[e] = true
		}
		rows.Close()
	}

	// Also check global suppression
	for i := 0; i < len(emails); i += batchSize {
		end := i + batchSize
		if end > len(emails) {
			end = len(emails)
		}
		batch := emails[i:end]

		hashes := make([]string, len(batch))
		hashToEmail := make(map[string]string, len(batch))
		for j, e := range batch {
			h := md5Hash(e)
			hashes[j] = fmt.Sprintf("$%d", j+1)
			hashToEmail[h] = e
		}

		args := make([]interface{}, len(batch))
		for j, e := range batch {
			args[j] = md5Hash(e)
		}

		rows, err := dp.db.QueryContext(ctx,
			fmt.Sprintf(`SELECT email_hash FROM mailing_global_suppressions WHERE email_hash IN (%s)`,
				strings.Join(hashes, ",")),
			args...,
		)
		if err != nil {
			log.Printf("[DataPipeline] suppression dedup: %v", err)
			continue
		}
		for rows.Next() {
			var h string
			rows.Scan(&h)
			if e, ok := hashToEmail[h]; ok {
				existing[e] = true
			}
		}
		rows.Close()
	}

	var deduped []string
	dupCount := 0
	for _, e := range emails {
		if existing[e] {
			dupCount++
			continue
		}
		deduped = append(deduped, e)
	}
	return deduped, dupCount
}

func (dp *DataPipeline) insertSubscribers(ctx context.Context, emails []string, listID string) {
	for _, email := range emails {
		id := uuid.New().String()
		h := md5Hash(email)
		_, err := dp.db.ExecContext(ctx,
			`INSERT INTO mailing_subscribers (id, organization_id, list_id, email, email_hash,
			 first_name, last_name, status, source, engagement_score, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, '', '', 'confirmed', 'data_pipeline', 50.0, NOW(), NOW())
			 ON CONFLICT (list_id, email) DO NOTHING`,
			id, dp.orgID, listID, email, h,
		)
		if err != nil {
			log.Printf("[DataPipeline] insert subscriber %s: %v", email, err)
		}
	}
}

func (dp *DataPipeline) addToGlobalSuppression(ctx context.Context, emails []string) {
	for _, email := range emails {
		h := md5Hash(email)
		_, err := dp.db.ExecContext(ctx,
			`INSERT INTO mailing_global_suppressions (email_hash, email, reason, source, created_at)
			 VALUES ($1, $2, 'eo_invalid', 'data_pipeline', NOW())
			 ON CONFLICT (email_hash) DO NOTHING`,
			h, email,
		)
		if err != nil {
			log.Printf("[DataPipeline] suppress %s: %v", email, err)
		}
	}
}

func (dp *DataPipeline) completeRun(ctx context.Context, runID string, report PipelineReport) {
	_, err := dp.db.ExecContext(ctx,
		`UPDATE data_pipeline_runs SET
		 status = 'completed', completed_at = NOW(),
		 files_processed = $1, emails_total = $2, emails_verified = $3,
		 emails_suppressed = $4, emails_deduped = $5,
		 notification_sent = $6
		 WHERE id = $7`,
		report.FilesProcessed, report.EmailsTotal, report.EmailsVerified,
		report.EmailsSuppressed, report.EmailsDeduped,
		dp.Notifier != nil, runID,
	)
	if err != nil {
		log.Printf("[DataPipeline] complete run %s: %v", runID, err)
	}
}

func (dp *DataPipeline) failRun(ctx context.Context, runID string, errMsg string) {
	log.Printf("[DataPipeline] run %s failed: %s", runID, errMsg)
	dp.db.ExecContext(ctx,
		`UPDATE data_pipeline_runs SET status = 'failed', completed_at = NOW(), error_message = $1 WHERE id = $2`,
		errMsg, runID,
	)
}

func md5Hash(s string) string {
	h := md5.Sum([]byte(strings.ToLower(s)))
	return hex.EncodeToString(h[:])
}
