package worker

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

const (
	// staticSnapshotRetentionDays is the hard-delete horizon for single-use
	// static (per-campaign) segment snapshots, measured from last_used_at
	// (which equals creation time for these — they're used exactly once at
	// deploy). Operator directive 2026-06-04: static snapshots have no value
	// once their campaign has sent, so they are hard-deleted (segment + member
	// rows) after this window rather than going through the warn/grace/archive
	// flow used for reusable dynamic segments. Pinned (keep_active) segments
	// are always exempt.
	staticSnapshotRetentionDays = 7

	// staticPurgeSegmentBatch caps how many static segments we delete per
	// inner batch so member-row deletes stay bounded and lock-friendly.
	staticPurgeSegmentBatch = 200

	// staticPurgeMaxPerCycle caps total segments purged per org per cycle so a
	// large backlog cannot monopolize a single run under send-day IO.
	staticPurgeMaxPerCycle = 5000
)

// SegmentCleanupWorker handles automatic cleanup of unused segments
type SegmentCleanupWorker struct {
	db              *sql.DB
	checkInterval   time.Duration
	emailSender     EmailSender
	stopChan        chan struct{}
	running         bool
}

// EmailSender interface for sending notification emails
type EmailSender interface {
	SendEmail(to []string, subject, htmlBody, textBody string) error
}

// CleanupSettings represents the cleanup configuration for an organization
type CleanupSettings struct {
	ID                    uuid.UUID
	OrganizationID        uuid.UUID
	Enabled               bool
	InactiveDaysThreshold int
	GracePeriodDays       int
	AutoArchive           bool
	AutoDelete            bool
	ArchiveRetentionDays  int
	NotifyAdmins          bool
	AdminEmails           []string
	MinSegmentAgeDays     int
	ExcludePatterns       []string
}

// StaleSegment represents a segment that hasn't been used
type StaleSegment struct {
	ID              uuid.UUID
	Name            string
	SubscriberCount int
	LastUsedAt      *time.Time
	DaysInactive    int
	CreatedAt       time.Time
}

// CleanupNotification represents a pending cleanup action
type CleanupNotification struct {
	ID                uuid.UUID
	SegmentID         uuid.UUID
	SegmentName       string
	SubscriberCount   int
	WarnedAt          time.Time
	GracePeriodEndsAt time.Time
}

// NewSegmentCleanupWorker creates a new cleanup worker
func NewSegmentCleanupWorker(db *sql.DB, emailSender EmailSender) *SegmentCleanupWorker {
	return &SegmentCleanupWorker{
		db:            db,
		checkInterval: 1 * time.Hour, // Check every hour
		emailSender:   emailSender,
		stopChan:      make(chan struct{}),
	}
}

// Start begins the cleanup worker
func (w *SegmentCleanupWorker) Start() {
	if w.running {
		return
	}
	w.running = true
	log.Println("SegmentCleanupWorker: Starting segment cleanup service...")

	go w.run()
}

// Stop stops the cleanup worker
func (w *SegmentCleanupWorker) Stop() {
	if !w.running {
		return
	}
	close(w.stopChan)
	w.running = false
	log.Println("SegmentCleanupWorker: Stopped")
}

func (w *SegmentCleanupWorker) run() {
	// Run immediately on start
	w.processAllOrganizations()

	ticker := time.NewTicker(w.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			w.processAllOrganizations()
		case <-w.stopChan:
			return
		}
	}
}

// cleanupCycleTotals accumulates the countable outcomes of one full cleanup
// cycle across all organizations, for the run-level summary row.
type cleanupCycleTotals struct {
	warned        int64 // stale segments warned (marked + notification row)
	archived      int64 // grace-expired segments archived or deactivated
	purgedStatic  int64 // aged static snapshots hard-deleted
	deletedArchiv int64 // archived segments hard-deleted past retention
	failedOrgs    int64 // orgs whose settings row could not be processed
}

func (t cleanupCycleTotals) processed() int64 {
	return t.warned + t.archived + t.purgedStatic + t.deletedArchiv
}

func (t cleanupCycleTotals) detail() string {
	return fmt.Sprintf("warned %d, archived/deactivated %d, purged %d static snapshots, deleted %d expired archives",
		t.warned, t.archived, t.purgedStatic, t.deletedArchiv)
}

func (w *SegmentCleanupWorker) processAllOrganizations() {
	ctx := context.Background()
	start := time.Now()

	hbStatus, hbErr := "ok", ""
	var totals cleanupCycleTotals
	defer func() {
		EmitHeartbeat(ctx, w.db, "segment_cleanup", int(w.checkInterval.Seconds()), hbStatus, hbErr)

		// Run-level summary, next to the heartbeat: what this cycle did.
		runStatus := "ok"
		detail := totals.detail()
		switch {
		case hbStatus == "error":
			runStatus = "failed"
			detail = truncateRunDetail("cycle error: " + hbErr)
		case totals.failedOrgs > 0:
			runStatus = "partial"
		}
		RecordWorkerRun(ctx, w.db, "segment_cleanup", start, runStatus, int(totals.processed()), int(totals.failedOrgs), detail)
	}()

	// Get all organizations with cleanup enabled
	rows, err := w.db.QueryContext(ctx, `
		SELECT organization_id, enabled, inactive_days_threshold, grace_period_days,
			   auto_archive, auto_delete, archive_retention_days, notify_admins,
			   admin_emails, min_segment_age_days, exclude_patterns
		FROM mailing_segment_cleanup_settings
		WHERE enabled = TRUE
	`)
	if err != nil {
		hbStatus, hbErr = "error", err.Error()
		log.Printf("SegmentCleanupWorker: Error fetching settings: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var settings CleanupSettings
		var adminEmails, excludePatterns sql.NullString

		err := rows.Scan(
			&settings.OrganizationID,
			&settings.Enabled,
			&settings.InactiveDaysThreshold,
			&settings.GracePeriodDays,
			&settings.AutoArchive,
			&settings.AutoDelete,
			&settings.ArchiveRetentionDays,
			&settings.NotifyAdmins,
			&adminEmails,
			&settings.MinSegmentAgeDays,
			&excludePatterns,
		)
		if err != nil {
			log.Printf("SegmentCleanupWorker: Error scanning settings: %v", err)
			totals.failedOrgs++
			continue
		}

		// Parse arrays
		if adminEmails.Valid {
			settings.AdminEmails = parsePostgresArray(adminEmails.String)
		}
		if excludePatterns.Valid {
			settings.ExcludePatterns = parsePostgresArray(excludePatterns.String)
		}

		// Process this organization
		w.processOrganization(ctx, settings, &totals)
	}
}

func (w *SegmentCleanupWorker) processOrganization(ctx context.Context, settings CleanupSettings, totals *cleanupCycleTotals) {
	log.Printf("SegmentCleanupWorker: Processing organization %s", settings.OrganizationID)
	if totals == nil {
		totals = &cleanupCycleTotals{}
	}

	// 0. Hard-delete aged single-use static snapshots (segment + members).
	//    Runs before the warn/grace flow because static snapshots are dead
	//    weight the moment their campaign has sent — there is no value in a
	//    7-day warning email for an audience snapshot nobody will reuse.
	totals.purgedStatic += w.purgeStaticSnapshots(ctx, settings)

	// 1. Find segments that need warning
	staleSegments := w.findStaleSegments(ctx, settings)
	if len(staleSegments) > 0 {
		log.Printf("SegmentCleanupWorker: Found %d stale segments for org %s", len(staleSegments), settings.OrganizationID)
		totals.warned += int64(w.sendWarningNotifications(ctx, settings, staleSegments))
	}

	// 2. Process segments where grace period has expired
	expiredNotifications := w.findExpiredNotifications(ctx, settings.OrganizationID)
	if len(expiredNotifications) > 0 {
		log.Printf("SegmentCleanupWorker: Found %d segments with expired grace period for org %s", len(expiredNotifications), settings.OrganizationID)
		totals.archived += int64(w.processExpiredSegments(ctx, settings, expiredNotifications))
	}

	// 3. Clean up old archived segments (if auto-delete enabled)
	if settings.AutoDelete && settings.ArchiveRetentionDays > 0 {
		totals.deletedArchiv += w.cleanupArchivedSegments(ctx, settings)
	}
}

func (w *SegmentCleanupWorker) findStaleSegments(ctx context.Context, settings CleanupSettings) []StaleSegment {
	var segments []StaleSegment

	query := `
		SELECT 
			s.id,
			s.name,
			COALESCE(s.subscriber_count, 0),
			s.last_used_at,
			EXTRACT(DAY FROM NOW() - COALESCE(s.last_used_at, s.created_at))::INTEGER,
			s.created_at
		FROM mailing_segments s
		WHERE s.organization_id = $1
			AND s.segment_type != 'static'
			AND s.archived_at IS NULL
			AND s.keep_active = FALSE
			AND s.cleanup_warning_sent = FALSE
			AND s.created_at < NOW() - ($2 || ' days')::INTERVAL
			AND COALESCE(s.last_used_at, s.created_at) < NOW() - ($3 || ' days')::INTERVAL
		ORDER BY s.last_used_at ASC NULLS FIRST
		LIMIT 50
	`

	rows, err := w.db.QueryContext(ctx, query,
		settings.OrganizationID,
		settings.MinSegmentAgeDays,
		settings.InactiveDaysThreshold,
	)
	if err != nil {
		log.Printf("SegmentCleanupWorker: Error finding stale segments: %v", err)
		return segments
	}
	defer rows.Close()

	for rows.Next() {
		var seg StaleSegment
		var lastUsed sql.NullTime

		if err := rows.Scan(&seg.ID, &seg.Name, &seg.SubscriberCount, &lastUsed, &seg.DaysInactive, &seg.CreatedAt); err != nil {
			continue
		}

		if lastUsed.Valid {
			seg.LastUsedAt = &lastUsed.Time
		}

		// Check if name matches exclude patterns
		excluded := false
		for _, pattern := range settings.ExcludePatterns {
			if matchesPattern(seg.Name, pattern) {
				excluded = true
				break
			}
		}
		if !excluded {
			segments = append(segments, seg)
		}
	}

	return segments
}

// sendWarningNotifications returns the number of segments marked as warned.
func (w *SegmentCleanupWorker) sendWarningNotifications(ctx context.Context, settings CleanupSettings, segments []StaleSegment) int {
	if !settings.NotifyAdmins || w.emailSender == nil {
		// Just mark as warned without sending email
		for _, seg := range segments {
			w.markSegmentWarned(ctx, settings, seg)
		}
		return len(segments)
	}

	// Get admin emails for this organization
	adminEmails := w.getAdminEmails(ctx, settings)
	if len(adminEmails) == 0 {
		log.Printf("SegmentCleanupWorker: No admin emails found for org %s", settings.OrganizationID)
		return 0
	}

	// Build email content
	subject := fmt.Sprintf("Action Required: %d unused segments will be cleaned up", len(segments))
	htmlBody := w.buildWarningEmailHTML(segments, settings.GracePeriodDays)
	textBody := w.buildWarningEmailText(segments, settings.GracePeriodDays)

	// Send email
	if err := w.emailSender.SendEmail(adminEmails, subject, htmlBody, textBody); err != nil {
		log.Printf("SegmentCleanupWorker: Error sending notification email: %v", err)
	} else {
		log.Printf("SegmentCleanupWorker: Sent cleanup warning email to %d admins", len(adminEmails))
	}

	// Mark segments as warned and create notifications
	for _, seg := range segments {
		w.markSegmentWarned(ctx, settings, seg)
	}
	return len(segments)
}

func (w *SegmentCleanupWorker) markSegmentWarned(ctx context.Context, settings CleanupSettings, seg StaleSegment) {
	graceEnds := time.Now().AddDate(0, 0, settings.GracePeriodDays)

	// Update segment
	_, err := w.db.ExecContext(ctx, `
		UPDATE mailing_segments 
		SET cleanup_warning_sent = TRUE, cleanup_warned_at = NOW()
		WHERE id = $1
	`, seg.ID)
	if err != nil {
		log.Printf("SegmentCleanupWorker: Error marking segment warned: %v", err)
	}

	// Create notification record
	_, err = w.db.ExecContext(ctx, `
		INSERT INTO mailing_segment_cleanup_notifications 
			(organization_id, segment_id, segment_name, subscriber_count, last_used_at, grace_period_ends_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, settings.OrganizationID, seg.ID, seg.Name, seg.SubscriberCount, seg.LastUsedAt, graceEnds)
	if err != nil {
		log.Printf("SegmentCleanupWorker: Error creating notification record: %v", err)
	}
}

func (w *SegmentCleanupWorker) findExpiredNotifications(ctx context.Context, orgID uuid.UUID) []CleanupNotification {
	var notifications []CleanupNotification

	rows, err := w.db.QueryContext(ctx, `
		SELECT n.id, n.segment_id, n.segment_name, n.subscriber_count, n.warning_sent_at, n.grace_period_ends_at
		FROM mailing_segment_cleanup_notifications n
		JOIN mailing_segments s ON s.id = n.segment_id
		WHERE n.organization_id = $1
			AND n.action_taken IS NULL
			AND n.grace_period_ends_at < NOW()
			AND s.archived_at IS NULL
			AND s.keep_active = FALSE
	`, orgID)
	if err != nil {
		log.Printf("SegmentCleanupWorker: Error finding expired notifications: %v", err)
		return notifications
	}
	defer rows.Close()

	for rows.Next() {
		var n CleanupNotification
		if err := rows.Scan(&n.ID, &n.SegmentID, &n.SegmentName, &n.SubscriberCount, &n.WarnedAt, &n.GracePeriodEndsAt); err != nil {
			continue
		}
		notifications = append(notifications, n)
	}

	return notifications
}

// processExpiredSegments returns the number of segments archived/deactivated.
func (w *SegmentCleanupWorker) processExpiredSegments(ctx context.Context, settings CleanupSettings, notifications []CleanupNotification) int {
	acted := 0
	for _, n := range notifications {
		var action string

		if settings.AutoArchive {
			// Archive the segment (soft delete)
			_, err := w.db.ExecContext(ctx, `
				UPDATE mailing_segments 
				SET archived_at = NOW(), status = 'archived'
				WHERE id = $1
			`, n.SegmentID)
			if err != nil {
				log.Printf("SegmentCleanupWorker: Error archiving segment %s: %v", n.SegmentID, err)
				continue
			}
			action = "archived"
			log.Printf("SegmentCleanupWorker: Archived segment '%s' (%s)", n.SegmentName, n.SegmentID)
		} else {
			// Just mark as inactive
			_, err := w.db.ExecContext(ctx, `
				UPDATE mailing_segments 
				SET status = 'inactive'
				WHERE id = $1
			`, n.SegmentID)
			if err != nil {
				log.Printf("SegmentCleanupWorker: Error deactivating segment %s: %v", n.SegmentID, err)
				continue
			}
			action = "deactivated"
		}

		// Update notification record
		_, _ = w.db.ExecContext(ctx, `
			UPDATE mailing_segment_cleanup_notifications
			SET action_taken = $1, action_taken_at = NOW()
			WHERE id = $2
		`, action, n.ID)
		acted++
	}
	return acted
}

// cleanupArchivedSegments returns the number of archived segments deleted.
func (w *SegmentCleanupWorker) cleanupArchivedSegments(ctx context.Context, settings CleanupSettings) int64 {
	// Find archived segments past retention. There is NO FK cascade from
	// mailing_segment_members -> mailing_segments, so we MUST delete member
	// rows explicitly first or they orphan and the space is never reclaimed.
	rows, err := w.db.QueryContext(ctx, `
		SELECT id FROM mailing_segments
		WHERE organization_id = $1
			AND archived_at IS NOT NULL
			AND archived_at < NOW() - ($2 || ' days')::INTERVAL
		LIMIT $3
	`, settings.OrganizationID, settings.ArchiveRetentionDays, staticPurgeMaxPerCycle)
	if err != nil {
		log.Printf("SegmentCleanupWorker: Error listing old archived segments: %v", err)
		return 0
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return 0
	}

	segs, members := w.deleteSegmentsWithMembers(ctx, ids)
	if segs > 0 {
		log.Printf("SegmentCleanupWorker: Permanently deleted %d archived segments (+%d member rows) older than %d days",
			segs, members, settings.ArchiveRetentionDays)
	}
	return segs
}

// purgeStaticSnapshots hard-deletes single-use static segment snapshots (and
// their member rows) once they age past staticSnapshotRetentionDays from
// last_used_at. Pinned (keep_active) and already-archived segments are exempt.
// Work is chunked so member-row deletes stay lock- and IO-friendly under
// send-day load. Returns the number of snapshot segments deleted.
func (w *SegmentCleanupWorker) purgeStaticSnapshots(ctx context.Context, settings CleanupSettings) int64 {
	var totalSegs, totalMembers int64
	for totalSegs < staticPurgeMaxPerCycle {
		if ctx.Err() != nil {
			return totalSegs
		}

		lctx, lcancel := context.WithTimeout(ctx, 30*time.Second)
		rows, err := w.db.QueryContext(lctx, `
			SELECT id FROM mailing_segments
			WHERE organization_id = $1
				AND segment_type = 'static'
				AND keep_active = FALSE
				AND archived_at IS NULL
				AND COALESCE(last_used_at, created_at) < NOW() - ($2 || ' days')::INTERVAL
			LIMIT $3
		`, settings.OrganizationID, staticSnapshotRetentionDays, staticPurgeSegmentBatch)
		if err != nil {
			lcancel()
			log.Printf("SegmentCleanupWorker: static purge list error: %v", err)
			return totalSegs
		}
		var ids []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		lcancel()
		if len(ids) == 0 {
			break
		}

		segs, members := w.deleteSegmentsWithMembers(ctx, ids)
		totalSegs += segs
		totalMembers += members
		if segs == 0 {
			break // nothing deleted (e.g. transient error) — avoid hot loop
		}
		time.Sleep(100 * time.Millisecond)
	}
	if totalSegs > 0 {
		log.Printf("SegmentCleanupWorker: hard-deleted %d static snapshot(s) (+%d member rows) older than %dd for org %s",
			totalSegs, totalMembers, staticSnapshotRetentionDays, settings.OrganizationID)
	}
	return totalSegs
}

// deleteSegmentsWithMembers removes member rows (indexed by segment_id) and
// then the segment rows for the given ids. Returns (segmentsDeleted,
// membersDeleted). Members are deleted first so a mid-operation failure leaves
// at worst an empty segment that the next cycle re-collects — never orphaned
// member rows.
func (w *SegmentCleanupWorker) deleteSegmentsWithMembers(ctx context.Context, ids []uuid.UUID) (int64, int64) {
	if len(ids) == 0 {
		return 0, 0
	}

	mctx, mcancel := context.WithTimeout(ctx, 60*time.Second)
	mres, err := w.db.ExecContext(mctx,
		`DELETE FROM mailing_segment_members WHERE segment_id = ANY($1::uuid[])`,
		pq.Array(ids))
	mcancel()
	if err != nil {
		log.Printf("SegmentCleanupWorker: member purge error (%d segs): %v", len(ids), err)
		return 0, 0
	}
	members, _ := mres.RowsAffected()

	sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
	sres, err := w.db.ExecContext(sctx,
		`DELETE FROM mailing_segments WHERE id = ANY($1::uuid[])`,
		pq.Array(ids))
	scancel()
	if err != nil {
		log.Printf("SegmentCleanupWorker: segment delete error (%d segs, members already purged): %v", len(ids), err)
		return 0, members
	}
	segs, _ := sres.RowsAffected()
	return segs, members
}

func (w *SegmentCleanupWorker) getAdminEmails(ctx context.Context, settings CleanupSettings) []string {
	emails := make([]string, 0)

	// Add configured admin emails
	emails = append(emails, settings.AdminEmails...)

	// Get organization admins from users table
	rows, err := w.db.QueryContext(ctx, `
		SELECT email FROM users 
		WHERE organization_id = $1 AND role IN ('admin', 'owner')
		AND email IS NOT NULL AND email != ''
	`, settings.OrganizationID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var email string
			if rows.Scan(&email) == nil {
				emails = append(emails, email)
			}
		}
	}

	// Deduplicate
	seen := make(map[string]bool)
	result := make([]string, 0)
	for _, e := range emails {
		if !seen[e] {
			seen[e] = true
			result = append(result, e)
		}
	}

	return result
}

func (w *SegmentCleanupWorker) buildWarningEmailHTML(segments []StaleSegment, graceDays int) string {
	var sb strings.Builder

	sb.WriteString(`<!DOCTYPE html>
<html>
<head>
<style>
body { font-family: Arial, sans-serif; line-height: 1.6; color: #333; }
.container { max-width: 600px; margin: 0 auto; padding: 20px; }
.header { background: #f59e0b; color: white; padding: 20px; border-radius: 8px 8px 0 0; }
.content { background: #f9fafb; padding: 20px; border-radius: 0 0 8px 8px; }
.segment-table { width: 100%; border-collapse: collapse; margin: 20px 0; }
.segment-table th, .segment-table td { padding: 12px; text-align: left; border-bottom: 1px solid #e5e7eb; }
.segment-table th { background: #f3f4f6; }
.btn { display: inline-block; padding: 12px 24px; background: #3b82f6; color: white; text-decoration: none; border-radius: 6px; margin: 10px 5px 10px 0; }
.btn-secondary { background: #6b7280; }
.warning { color: #dc2626; font-weight: bold; }
</style>
</head>
<body>
<div class="container">
<div class="header">
<h1>⚠️ Segment Cleanup Notice</h1>
</div>
<div class="content">
<p>The following segments haven't been used and will be automatically cleaned up in <strong class="warning">`)
	sb.WriteString(fmt.Sprintf("%d days", graceDays))
	sb.WriteString(`</strong> unless you take action:</p>

<table class="segment-table">
<tr>
<th>Segment Name</th>
<th>Contacts</th>
<th>Days Inactive</th>
</tr>
`)

	for _, seg := range segments {
		sb.WriteString(fmt.Sprintf(`<tr>
<td><strong>%s</strong></td>
<td>%d</td>
<td>%d days</td>
</tr>
`, seg.Name, seg.SubscriberCount, seg.DaysInactive))
	}

	sb.WriteString(`</table>

<h3>What you can do:</h3>
<ul>
<li><strong>Keep a segment:</strong> Go to Segments → Click the segment → Click "Keep Active" to prevent cleanup</li>
<li><strong>Use a segment:</strong> Using a segment in a campaign or running a count query will reset its inactive status</li>
<li><strong>Do nothing:</strong> Unused segments will be archived after the grace period</li>
</ul>

<p>
<a href="/mailing/segments" class="btn">Review Segments</a>
<a href="/settings/cleanup" class="btn btn-secondary">Cleanup Settings</a>
</p>

<p style="color: #6b7280; font-size: 14px; margin-top: 20px;">
This is an automated message from your mailing system. To change these notifications, update your cleanup settings.
</p>
</div>
</div>
</body>
</html>`)

	return sb.String()
}

func (w *SegmentCleanupWorker) buildWarningEmailText(segments []StaleSegment, graceDays int) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("SEGMENT CLEANUP NOTICE\n\nThe following segments haven't been used and will be cleaned up in %d days:\n\n", graceDays))

	for _, seg := range segments {
		sb.WriteString(fmt.Sprintf("- %s (%d contacts, %d days inactive)\n", seg.Name, seg.SubscriberCount, seg.DaysInactive))
	}

	sb.WriteString("\nTo keep a segment, go to Segments and click 'Keep Active' on the segment.\nUsing a segment in a campaign will also reset its inactive status.\n")

	return sb.String()
}

// Helper functions

func parsePostgresArray(s string) []string {
	// Simple postgres array parser for {val1,val2} format
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	if s == "" {
		return []string{}
	}
	return strings.Split(s, ",")
}

func matchesPattern(name, pattern string) bool {
	// Simple wildcard matching (% as wildcard)
	pattern = strings.ReplaceAll(pattern, "%", ".*")
	// Case insensitive check
	return strings.Contains(strings.ToLower(name), strings.ToLower(strings.ReplaceAll(pattern, ".*", "")))
}
