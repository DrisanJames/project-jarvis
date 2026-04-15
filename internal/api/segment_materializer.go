package api

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// SegmentMaterializer pre-computes segment membership into
// mailing_segment_members once per day at a fixed UTC time so that audience
// planning can read cached results instead of running expensive live queries.
type SegmentMaterializer struct {
	db         *sql.DB
	targetHour int
	targetMin  int
}

// NewSegmentMaterializer creates a materializer that runs daily at the given
// UTC time (e.g. "04:00" for 9 PM MST).
func NewSegmentMaterializer(db *sql.DB, targetUTC string) *SegmentMaterializer {
	hour, min := 4, 0
	if n, _ := fmt.Sscanf(targetUTC, "%d:%d", &hour, &min); n < 2 {
		log.Printf("[SegmentMaterializer] invalid targetUTC %q, defaulting to 04:00", targetUTC)
		hour, min = 4, 0
	}
	return &SegmentMaterializer{db: db, targetHour: hour, targetMin: min}
}

// DurationUntilNext calculates the sleep duration from `now` until the next
// occurrence of the target time. Exported for unit testing.
func DurationUntilNext(now time.Time, targetHour, targetMin int) time.Duration {
	next := time.Date(now.Year(), now.Month(), now.Day(), targetHour, targetMin, 0, 0, time.UTC)
	if !now.Before(next) {
		next = next.Add(24 * time.Hour)
	}
	return next.Sub(now)
}

func (m *SegmentMaterializer) Start(ctx context.Context) {
	wait := DurationUntilNext(time.Now().UTC(), m.targetHour, m.targetMin)
	nextRun := time.Now().UTC().Add(wait)
	log.Printf("[SegmentMaterializer] started — next run at %s (sleeping %s)",
		nextRun.Format("2006-01-02 15:04 UTC"), wait.Round(time.Second))

	go func() {
		for {
			wait = DurationUntilNext(time.Now().UTC(), m.targetHour, m.targetMin)
			select {
			case <-time.After(wait):
				log.Printf("[SegmentMaterializer] nightly cycle starting")
				m.materializeAll(ctx)
			case <-ctx.Done():
				log.Println("[SegmentMaterializer] context cancelled, stopping")
				return
			}
		}
	}()
}

func (m *SegmentMaterializer) materializeAll(ctx context.Context) {
	start := time.Now()

	rows, err := m.db.QueryContext(ctx, `
		SELECT s.id::text, s.list_id::text, s.name, COALESCE(s.conditions::text, '[]')
		FROM mailing_segments s
		WHERE s.status = 'active' AND s.segment_type = 'dynamic'
		ORDER BY s.updated_at ASC
		LIMIT 200
	`)
	if err != nil {
		log.Printf("[SegmentMaterializer] query segments error: %v", err)
		return
	}
	defer rows.Close()

	type segInfo struct {
		id, listID, name, conditions string
	}
	var segments []segInfo
	for rows.Next() {
		var s segInfo
		var listIDNull sql.NullString
		if err := rows.Scan(&s.id, &listIDNull, &s.name, &s.conditions); err != nil {
			log.Printf("[SegmentMaterializer] scan error: %v", err)
			continue
		}
		if listIDNull.Valid {
			s.listID = listIDNull.String
		}
		segments = append(segments, s)
	}

	if len(segments) == 0 {
		return
	}

	materialized := 0
	for _, seg := range segments {
		if ctx.Err() != nil {
			break
		}
		count, err := m.materializeOne(ctx, seg.id, seg.listID, seg.conditions)
		if err != nil {
			log.Printf("[SegmentMaterializer] %s (%s) failed: %v", seg.name, safePrefix(seg.id, 12), err)
			continue
		}
		log.Printf("[SegmentMaterializer] %s — %d members", seg.name, count)
		materialized++
	}

	log.Printf("[SegmentMaterializer] materialized %d/%d segments in %s",
		materialized, len(segments), time.Since(start).Round(time.Millisecond))
}

func (m *SegmentMaterializer) materializeOne(ctx context.Context, segmentID, listID, conditionsRaw string) (int, error) {
	return MaterializeSegment(ctx, m.db, segmentID, listID, conditionsRaw)
}

// MaterializeSegment populates mailing_segment_members for a single segment.
// Called by the nightly materializer and by segment-creation handlers.
func MaterializeSegment(ctx context.Context, db *sql.DB, segmentID, listID, conditionsRaw string) (int, error) {
	segCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	if len(segmentID) != 36 {
		return 0, fmt.Errorf("invalid segment ID length: %d", len(segmentID))
	}
	for _, c := range segmentID {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || c == '-') {
			return 0, fmt.Errorf("invalid segment ID character: %c", c)
		}
	}

	conn, err := db.Conn(segCtx)
	if err != nil {
		return 0, err
	}
	defer func() {
		conn.ExecContext(context.Background(), "RESET ALL")
		conn.Close()
	}()

	if _, err := conn.ExecContext(segCtx, "SET statement_timeout = '600000'"); err != nil {
		log.Printf("[MaterializeSegment] SET statement_timeout failed: %v", err)
	}

	var listIDVal interface{}
	if listID != "" {
		listIDVal = listID
	}
	segQuery, segArgs := buildSegmentQuery(conditionsRaw, listIDVal)

	tx, err := conn.BeginTx(segCtx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(segCtx, "DELETE FROM mailing_segment_members WHERE segment_id = $1", segmentID); err != nil {
		return 0, fmt.Errorf("delete old members: %w", err)
	}

	insertSQL := fmt.Sprintf(
		`INSERT INTO mailing_segment_members (segment_id, subscriber_id, email, materialized_at)
		 SELECT DISTINCT '%s'::uuid, q.id::uuid, q.email, NOW()
		 FROM (%s) q
		 ON CONFLICT (segment_id, subscriber_id) DO NOTHING`, segmentID, segQuery)

	result, err := tx.ExecContext(segCtx, insertSQL, segArgs...)
	if err != nil {
		return 0, fmt.Errorf("insert-select: %w", err)
	}

	count, _ := result.RowsAffected()

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	return int(count), nil
}
