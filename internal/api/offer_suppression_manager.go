package api

import (
	"bufio"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// OfferSuppressionManager manages per-offer Bloom filters for O(1)
// suppression checks at send time. Only active offers with
// suppression_sync_enabled=TRUE have their Bloom filters loaded.
type OfferSuppressionManager struct {
	mu      sync.RWMutex
	filters map[string]*BloomFilter // offerID -> Bloom
	db      *sql.DB
	s3      *SuppressionS3Client
}

func NewOfferSuppressionManager(db *sql.DB, s3Client *SuppressionS3Client) *OfferSuppressionManager {
	return &OfferSuppressionManager{
		filters: make(map[string]*BloomFilter),
		db:      db,
		s3:      s3Client,
	}
}

// LoadActiveOffers loads Bloom filters for all active offers that have
// suppression data in S3. Called at server startup.
func (m *OfferSuppressionManager) LoadActiveOffers(ctx context.Context) error {
	rows, err := m.db.QueryContext(ctx,
		`SELECT id FROM mailing_offers
		 WHERE status = 'active' AND suppression_sync_enabled = TRUE
		   AND optizmo_link IS NOT NULL AND optizmo_link != ''`)
	if err != nil {
		return fmt.Errorf("query active offers: %w", err)
	}
	defer rows.Close()

	var offerIDs []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			offerIDs = append(offerIDs, id)
		}
	}

	if len(offerIDs) == 0 {
		log.Printf("[OfferSuppMgr] no active offers with suppression sync — skipping Bloom load")
		return nil
	}

	loaded := 0
	for _, offerID := range offerIDs {
		if err := m.LoadOfferBloom(ctx, offerID); err != nil {
			log.Printf("[OfferSuppMgr] could not load Bloom for offer %s: %v (will use DB fallback)", offerID, err)
			continue
		}
		loaded++
	}

	m.mu.RLock()
	totalMem := uint64(0)
	for _, bf := range m.filters {
		totalMem += bf.MemoryKB()
	}
	m.mu.RUnlock()

	log.Printf("[OfferSuppMgr] loaded %d/%d offer Bloom filters (total memory: %d KB)",
		loaded, len(offerIDs), totalMem)
	return nil
}

// LoadOfferBloom downloads and deserializes a Bloom filter from S3 for a
// single offer. If no S3 Bloom exists, tries to build one from S3 hash file.
func (m *OfferSuppressionManager) LoadOfferBloom(ctx context.Context, offerID string) error {
	if m.s3 == nil {
		return fmt.Errorf("S3 client not configured")
	}

	bloomData, err := m.s3.DownloadBloom(ctx, offerID)
	if err == nil && len(bloomData) > 0 {
		bf, err := UnmarshalBloomFilter(bloomData)
		if err != nil {
			return fmt.Errorf("unmarshal bloom: %w", err)
		}
		m.mu.Lock()
		m.filters[offerID] = bf
		m.mu.Unlock()
		log.Printf("[OfferSuppMgr] loaded Bloom for offer %s from S3 (%d elements, %d KB)",
			offerID, bf.Size(), bf.MemoryKB())
		return nil
	}

	return m.RebuildBloomFromS3Hashes(ctx, offerID)
}

// RebuildBloomFromS3Hashes builds a new Bloom filter by streaming the S3 hash
// file, then uploads the serialized Bloom back to S3.
func (m *OfferSuppressionManager) RebuildBloomFromS3Hashes(ctx context.Context, offerID string) error {
	if m.s3 == nil {
		return fmt.Errorf("S3 client not configured")
	}

	reader, err := m.s3.DownloadHashFile(ctx, offerID)
	if err != nil {
		return fmt.Errorf("download hash file: %w", err)
	}
	defer reader.Close()

	// First pass: count lines to size the Bloom optimally.
	// For large files this is fast since hashes are 33 bytes/line.
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	count := uint64(0)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			count++
		}
	}
	reader.Close()

	if count == 0 {
		log.Printf("[OfferSuppMgr] empty hash file for offer %s — no Bloom to build", offerID)
		return nil
	}

	// Second pass: populate the Bloom.
	reader2, err := m.s3.DownloadHashFile(ctx, offerID)
	if err != nil {
		return fmt.Errorf("re-download hash file: %w", err)
	}
	defer reader2.Close()

	bf := NewBloomFilter(count, 0.001)
	scanner2 := bufio.NewScanner(reader2)
	scanner2.Buffer(make([]byte, 256*1024), 256*1024)
	for scanner2.Scan() {
		h := strings.TrimSpace(scanner2.Text())
		if h != "" {
			bf.AddMD5(h)
		}
	}

	m.mu.Lock()
	m.filters[offerID] = bf
	m.mu.Unlock()

	bloomBytes, err := bf.MarshalBinary()
	if err != nil {
		return fmt.Errorf("marshal bloom: %w", err)
	}
	if _, err := m.s3.UploadBloom(ctx, offerID, bloomBytes); err != nil {
		log.Printf("[OfferSuppMgr] warning: could not persist Bloom to S3 for offer %s: %v", offerID, err)
	}

	m.s3.SaveMeta(ctx, SuppressionMeta{
		OfferID:     offerID,
		HashCount:   int(count),
		BloomSizeKB: int(bf.MemoryKB()),
		BloomFPRate: 0.001,
		LastSyncAt:  time.Now(),
		S3HashKey:   m.s3.hashKey(offerID),
		S3BloomKey:  m.s3.bloomKey(offerID),
	})

	log.Printf("[OfferSuppMgr] rebuilt Bloom for offer %s: %d elements, %d KB", offerID, bf.Size(), bf.MemoryKB())
	return nil
}

// IsSuppressed checks the in-memory Bloom for an offer+email pair.
// Returns (suppressed, bloomAvailable). If bloomAvailable=false, the
// caller should fall back to a DB query.
func (m *OfferSuppressionManager) IsSuppressed(offerID, email string) (bool, bool) {
	m.mu.RLock()
	bf, ok := m.filters[offerID]
	m.mu.RUnlock()
	if !ok {
		return false, false
	}

	hash := md5.Sum([]byte(strings.ToLower(strings.TrimSpace(email))))
	md5Hex := hex.EncodeToString(hash[:])
	return bf.ContainsMD5(md5Hex), true
}

// IsSuppressedBySubID checks the Bloom using the subscriber's email MD5 hash
// directly (already computed by the planner).
func (m *OfferSuppressionManager) IsSuppressedMD5(offerID, md5Hash string) (bool, bool) {
	m.mu.RLock()
	bf, ok := m.filters[offerID]
	m.mu.RUnlock()
	if !ok {
		return false, false
	}
	return bf.ContainsMD5(md5Hash), true
}

// EvictOffer removes an offer's Bloom filter from memory (e.g., when deactivated).
func (m *OfferSuppressionManager) EvictOffer(offerID string) {
	m.mu.Lock()
	delete(m.filters, offerID)
	m.mu.Unlock()
	log.Printf("[OfferSuppMgr] evicted Bloom for offer %s", offerID)
}

// Stats returns memory usage statistics.
func (m *OfferSuppressionManager) Stats() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	totalMem := uint64(0)
	totalElements := uint64(0)
	offers := make([]map[string]interface{}, 0, len(m.filters))

	for offerID, bf := range m.filters {
		mem := bf.MemoryKB()
		totalMem += mem
		totalElements += bf.Size()
		offers = append(offers, map[string]interface{}{
			"offer_id":      offerID,
			"element_count": bf.Size(),
			"memory_kb":     mem,
		})
	}

	return map[string]interface{}{
		"loaded_offers":  len(m.filters),
		"total_elements": totalElements,
		"total_memory_kb": totalMem,
		"offers":         offers,
	}
}
