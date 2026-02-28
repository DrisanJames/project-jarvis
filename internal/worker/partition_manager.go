package worker

import (
	"context"
	"database/sql"
	"log"
	"time"
)

// PartitionManager runs daily, calling the PostgreSQL function that creates
// subscriber_events partitions 2 months ahead (H2).
type PartitionManager struct {
	db        *sql.DB
	ctx       context.Context
	cancel    context.CancelFunc
	lastRunAt time.Time
	healthy   bool
}

func NewPartitionManager(db *sql.DB) *PartitionManager {
	return &PartitionManager{db: db, healthy: true}
}

func (pm *PartitionManager) Start() {
	pm.ctx, pm.cancel = context.WithCancel(context.Background())
	go func() {
		log.Println("[PartitionManager] Starting partition maintenance worker")
		time.Sleep(1 * time.Minute)
		pm.runOnce()

		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-pm.ctx.Done():
				log.Println("[PartitionManager] Stopped")
				return
			case <-ticker.C:
				pm.runOnce()
			}
		}
	}()
}

func (pm *PartitionManager) Stop() {
	if pm.cancel != nil {
		pm.cancel()
	}
}

func (pm *PartitionManager) IsHealthy() bool  { return pm.healthy }
func (pm *PartitionManager) LastRunAt() time.Time { return pm.lastRunAt }

func (pm *PartitionManager) runOnce() {
	pm.lastRunAt = time.Now()
	pm.healthy = true

	_, err := pm.db.ExecContext(pm.ctx, `SELECT create_subscriber_events_partition()`)
	if err != nil {
		log.Printf("[PartitionManager] Failed to create partition: %v", err)
		pm.healthy = false
		return
	}
	log.Println("[PartitionManager] Partition maintenance completed")
}
