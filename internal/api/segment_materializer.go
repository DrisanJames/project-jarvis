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

	conn, err := m.db.Conn(segCtx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(segCtx, "SET statement_timeout = '600s'"); err != nil {
		log.Printf("[SegmentMaterializer] SET statement_timeout failed: %v", err)
	}
	defer conn.ExecContext(context.Background(), "RESET statement_timeout")

	var listIDVal interface{}
	if listID != "" {
		listIDVal = listID
	}
	query, args := buildSegmentQuery(conditionsRaw, listIDVal)

	rows, err := conn.QueryContext(segCtx, query, args...)
	if err != nil {
		return 0, err
	}

	type member struct {
		subID, email string
	}
	var members []member
	for rows.Next() {
		var subID, email string
		if rows.Scan(&subID, &email) == nil {
			members = append(members, member{subID, email})
		}
	}
	rows.Close()

	tx, err := conn.BeginTx(segCtx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(segCtx, "DELETE FROM mailing_segment_members WHERE segment_id = $1", segmentID); err != nil {
		return 0, err
	}

	const batchSize = 500
	for i := 0; i < len(members); i += batchSize {
		end := i + batchSize
		if end > len(members) {
			end = len(members)
		}
		batch := members[i:end]

		q := "INSERT INTO mailing_segment_members (segment_id, subscriber_id, email, materialized_at) VALUES "
		vals := make([]interface{}, 0, len(batch)*3)
		for j, m := range batch {
			if j > 0 {
				q += ", "
			}
			base := j*3 + 1
			q += fmt.Sprintf("($%d, $%d, $%d, NOW())", base, base+1, base+2)
			vals = append(vals, segmentID, m.subID, m.email)
		}
		if _, err := tx.ExecContext(segCtx, q, vals...); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	return len(members), nil
}
