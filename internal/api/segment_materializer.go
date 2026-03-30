package api

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// SegmentMaterializer periodically pre-computes segment membership into
// mailing_segment_members so that audience planning can read cached results
// instead of running expensive live queries with correlated EXISTS subqueries.
type SegmentMaterializer struct {
	db       *sql.DB
	interval time.Duration
}

func NewSegmentMaterializer(db *sql.DB, interval time.Duration) *SegmentMaterializer {
	return &SegmentMaterializer{db: db, interval: interval}
}

func (m *SegmentMaterializer) Start(ctx context.Context) {
	log.Printf("[SegmentMaterializer] started (interval=%s)", m.interval)
	go func() {
		time.Sleep(45 * time.Second)
		m.materializeAll(ctx)

		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
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
			log.Printf("[SegmentMaterializer] %s (%s) failed: %v", seg.name, seg.id[:12], err)
			continue
		}
		log.Printf("[SegmentMaterializer] %s — %d members", seg.name, count)
		materialized++
	}

	log.Printf("[SegmentMaterializer] materialized %d/%d segments in %s",
		materialized, len(segments), time.Since(start).Round(time.Millisecond))
}

func (m *SegmentMaterializer) materializeOne(ctx context.Context, segmentID, listID, conditionsRaw string) (int, error) {
	segCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	// Validate segmentID is a proper UUID before embedding as a literal.
	if len(segmentID) != 36 {
		return 0, fmt.Errorf("invalid segment ID length: %d", len(segmentID))
	}
	for _, c := range segmentID {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || c == '-') {
			return 0, fmt.Errorf("invalid segment ID character: %c", c)
		}
	}

	conn, err := m.db.Conn(segCtx)
	if err != nil {
		return 0, err
	}
	defer func() {
		conn.ExecContext(context.Background(), "RESET ALL")
		conn.Close()
	}()

	if _, err := conn.ExecContext(segCtx, "SET statement_timeout = '600000'"); err != nil {
		log.Printf("[SegmentMaterializer] SET statement_timeout failed: %v", err)
	}

	var listIDVal interface{}
	if listID != "" {
		listIDVal = listID
	}
	segQuery, segArgs := buildSegmentQuery(conditionsRaw, listIDVal)

	// Server-side INSERT...SELECT: all data stays in PostgreSQL, zero Go memory.
	// The segment_id is embedded as a validated UUID literal so the inner query's
	// $N placeholders pass through to segArgs without renumbering.
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
		 SELECT '%s'::uuid, q.id::uuid, q.email, NOW()
		 FROM (%s) q`, segmentID, segQuery)

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
